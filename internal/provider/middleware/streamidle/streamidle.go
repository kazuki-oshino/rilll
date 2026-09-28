// Package streamidle は AgentProvider をラップして、ストリームの長時間沈黙
// (= Anthropic API の Stream idle timeout) を自前で先回り検知し、同一 SessionID
// での自動リトライを行う middleware を提供する。
//
// 検知の仕組み:
//   - inner.Call の callCtx を context.WithCancelCause で派生させる
//   - StreamIdleTimeout で発火する time.AfterFunc を起動し、callCtx を
//     ErrStreamIdleTimeout で cancel する
//   - inner からのイベント受信のたびに timer を Reset する
//   - inner.Call から戻った後、timer 発火が確認できれば「stream idle」と判定し、
//     resume_session_id を引き継いだまま BaseRetryDelay <<attempt のバックオフで
//     最大 MaxRetries 回まで再試行する
//
// この middleware はワークフロー層・provider 実装の双方から独立して動作するため、
// claude / codex / cursor の各 provider をそれぞれ Wrap するだけで適用できる。
package streamidle

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

// 既定値: Anthropic API の idle timeout (~5 分) と同じ閾値で先回り abort する
const (
	DefaultStreamIdleTimeout = 5 * time.Minute
	DefaultMaxRetries        = 2
	DefaultBaseRetryDelay    = 1 * time.Second
)

// ErrStreamIdleTimeout はストリームが StreamIdleTimeout を超えて沈黙した場合の
// 確定失敗エラー。リトライ全敗時の最終エラーとしても返される。
var ErrStreamIdleTimeout = errors.New("stream idle timeout")

// Config は middleware の動作を上書きする。0 値は既定値にフォールバックする。
type Config struct {
	StreamIdleTimeout time.Duration
	MaxRetries        int
	BaseRetryDelay    time.Duration
}

// withDefaults は StreamIdleTimeout の 0/負値を既定値で埋め、MaxRetries / BaseRetryDelay
// の負値を 0 にクランプする。
//
// MaxRetries=0 は「明示的にリトライ無効」を意味する有効な設定値として保持する
// (テスト容易性 / 意図的な無効化用途)。Wrap() 経由で既定リトライ回数を有効にしたい
// production 経路は Wrap() 側で `DefaultMaxRetries` を明示的にセットする。
func (c Config) withDefaults() Config {
	if c.StreamIdleTimeout <= 0 {
		c.StreamIdleTimeout = DefaultStreamIdleTimeout
	}
	if c.MaxRetries < 0 {
		c.MaxRetries = 0
	}
	if c.BaseRetryDelay < 0 {
		c.BaseRetryDelay = 0
	}
	return c
}

// Provider は inner provider をラップする AgentProvider 実装。
type Provider struct {
	inner  provider.AgentProvider
	config Config
}

// Wrap は既定 Config (StreamIdleTimeout=5min / MaxRetries=2 / BaseRetryDelay=1s) で
// inner provider を decorate する。
//
// 過去 `Config{}` を渡していたため `withDefaults()` の `MaxRetries < 0` ガードを
// `MaxRetries == 0` (zero value) がすり抜け、production 経路 (cmd/rilll/main.go) で
// idle timeout 後のリトライが 1 度も走らなかった (kiro-impl-loop の task 1.2 が
// 5 分沈黙 1 回で `ErrStreamIdleTimeout` を返した事例で発覚)。再発防止のため
// Default* を明示的に渡す。
func Wrap(inner provider.AgentProvider) *Provider {
	return WrapWithConfig(inner, Config{
		StreamIdleTimeout: DefaultStreamIdleTimeout,
		MaxRetries:        DefaultMaxRetries,
		BaseRetryDelay:    DefaultBaseRetryDelay,
	})
}

// WrapWithConfig は任意 Config で inner provider を decorate する。
func WrapWithConfig(inner provider.AgentProvider, config Config) *Provider {
	return &Provider{inner: inner, config: config.withDefaults()}
}

// ID は inner の ID をそのまま返す (decorate 透過)。
func (p *Provider) ID() provider.ProviderID { return p.inner.ID() }

// Call は inner.Call を idle 監視つきで呼び出す。
// idle timeout に到達した場合は同一 SessionID で最大 MaxRetries 回まで再試行し、
// すべて idle で確定失敗した場合のみ ErrStreamIdleTimeout を返す。
func (p *Provider) Call(ctx context.Context, req provider.AgentRequest, onEvent provider.AgentEventCallback) (provider.AgentResponse, error) {
	sessionID := req.ResumeSessionID
	var (
		lastResp provider.AgentResponse
		lastErr  error
	)

	for attempt := 0; attempt <= p.config.MaxRetries; attempt++ {
		callCtx, cancel := context.WithCancelCause(ctx)

		var (
			mu        sync.Mutex
			idleFired bool
		)
		timer := time.AfterFunc(p.config.StreamIdleTimeout, func() {
			mu.Lock()
			idleFired = true
			mu.Unlock()
			cancel(ErrStreamIdleTimeout)
		})

		resetIdle := func() {
			mu.Lock()
			defer mu.Unlock()
			if idleFired {
				return
			}
			timer.Reset(p.config.StreamIdleTimeout)
		}

		wrapped := func(ev provider.AgentEvent) {
			resetIdle()
			if ev.SessionID != "" {
				mu.Lock()
				sessionID = ev.SessionID
				mu.Unlock()
			}
			if onEvent != nil {
				onEvent(ev)
			}
		}

		attemptReq := req
		attemptReq.ResumeSessionID = sessionID

		resp, err := p.inner.Call(callCtx, attemptReq, wrapped)

		mu.Lock()
		timer.Stop()
		fired := idleFired
		mu.Unlock()
		cancel(nil)

		if resp.SessionID != "" {
			sessionID = resp.SessionID
		}

		// 成功は無条件で確定 (timer fired のレース勝ちでも retry しない)
		if err == nil && resp.Success {
			return resp, nil
		}

		// idle 発火確認 + 親 ctx は健在 → リトライ判定へ
		if fired && ctx.Err() == nil {
			lastResp = resp
			lastErr = ErrStreamIdleTimeout
			if attempt >= p.config.MaxRetries {
				break
			}
			delay := p.config.BaseRetryDelay << attempt
			select {
			case <-ctx.Done():
				return lastResp, ctx.Err()
			case <-time.After(delay):
			}
			continue
		}

		// それ以外はそのまま返す (provider error / 親 ctx cancel / !Success)
		return resp, err
	}
	return lastResp, lastErr
}
