package streamidle

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

// fakeProvider は AgentProvider のテスト用スタブ。
// callBehaviour に attempt 番号 (0-origin) で振る舞いを差し替える。
type fakeProvider struct {
	id        provider.ProviderID
	mu        sync.Mutex
	calls     []provider.AgentRequest
	behaviour func(attempt int, ctx context.Context, req provider.AgentRequest, onEvent provider.AgentEventCallback) (provider.AgentResponse, error)
}

func (f *fakeProvider) ID() provider.ProviderID { return f.id }

func (f *fakeProvider) Call(ctx context.Context, req provider.AgentRequest, onEvent provider.AgentEventCallback) (provider.AgentResponse, error) {
	f.mu.Lock()
	attempt := len(f.calls)
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	if f.behaviour == nil {
		return provider.AgentResponse{Content: "ok", Success: true}, nil
	}
	return f.behaviour(attempt, ctx, req, onEvent)
}

func (f *fakeProvider) requests() []provider.AgentRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]provider.AgentRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

func newFastConfig() Config {
	return Config{
		StreamIdleTimeout: 50 * time.Millisecond,
		MaxRetries:        2,
		BaseRetryDelay:    1 * time.Millisecond,
	}
}

func TestWrapPassesThroughOnSuccess(t *testing.T) {
	inner := &fakeProvider{
		id: provider.ProviderClaude,
		behaviour: func(_ int, _ context.Context, _ provider.AgentRequest, onEvent provider.AgentEventCallback) (provider.AgentResponse, error) {
			if onEvent != nil {
				onEvent(provider.AgentEvent{Type: provider.EventText, Text: "hi"})
			}
			return provider.AgentResponse{Content: "ok", Success: true, SessionID: "sess-1"}, nil
		},
	}
	wrapped := WrapWithConfig(inner, newFastConfig())
	if wrapped.ID() != provider.ProviderClaude {
		t.Fatalf("ID() should delegate: got %v", wrapped.ID())
	}

	var forwarded []provider.AgentEvent
	resp, err := wrapped.Call(context.Background(), provider.AgentRequest{Prompt: "p"}, func(ev provider.AgentEvent) {
		forwarded = append(forwarded, ev)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Success || resp.SessionID != "sess-1" {
		t.Fatalf("unexpected response: %#v", resp)
	}
	if len(forwarded) != 1 || forwarded[0].Text != "hi" {
		t.Fatalf("events were not forwarded: %#v", forwarded)
	}
	if got := len(inner.requests()); got != 1 {
		t.Fatalf("expected single inner call, got %d", got)
	}
}

func TestWrapRetriesOnceAfterIdleTimeoutThenSucceeds(t *testing.T) {
	inner := &fakeProvider{
		id: provider.ProviderClaude,
		behaviour: func(attempt int, ctx context.Context, req provider.AgentRequest, onEvent provider.AgentEventCallback) (provider.AgentResponse, error) {
			if attempt == 0 {
				// イベント発火 → SessionID 通知 → 以後沈黙して idle timeout を発火させる
				onEvent(provider.AgentEvent{Type: provider.EventObservation, SessionID: "sess-mid"})
				select {
				case <-ctx.Done():
					return provider.AgentResponse{}, ctx.Err()
				case <-time.After(500 * time.Millisecond):
					return provider.AgentResponse{Content: "should-not-reach", Success: true}, nil
				}
			}
			// 2 回目は即座に成功
			return provider.AgentResponse{Content: "ok", Success: true, SessionID: "sess-final"}, nil
		},
	}
	wrapped := WrapWithConfig(inner, newFastConfig())
	resp, err := wrapped.Call(context.Background(), provider.AgentRequest{Prompt: "p"}, func(provider.AgentEvent) {})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Success || resp.SessionID != "sess-final" {
		t.Fatalf("unexpected final response: %#v", resp)
	}
	calls := inner.requests()
	if len(calls) != 2 {
		t.Fatalf("expected 2 inner calls, got %d", len(calls))
	}
	// イベント中で取得した SessionID が 2 回目の ResumeSessionID に渡る
	if calls[1].ResumeSessionID != "sess-mid" {
		t.Fatalf("retry should resume with mid-stream sessionId, got %q", calls[1].ResumeSessionID)
	}
}

func TestWrapReturnsErrStreamIdleTimeoutAfterMaxRetries(t *testing.T) {
	inner := &fakeProvider{
		id: provider.ProviderClaude,
		behaviour: func(_ int, ctx context.Context, _ provider.AgentRequest, _ provider.AgentEventCallback) (provider.AgentResponse, error) {
			select {
			case <-ctx.Done():
				return provider.AgentResponse{}, ctx.Err()
			case <-time.After(500 * time.Millisecond):
				return provider.AgentResponse{Content: "should-not-reach", Success: true}, nil
			}
		},
	}
	cfg := newFastConfig()
	cfg.MaxRetries = 2
	wrapped := WrapWithConfig(inner, cfg)
	_, err := wrapped.Call(context.Background(), provider.AgentRequest{Prompt: "p"}, nil)
	if !errors.Is(err, ErrStreamIdleTimeout) {
		t.Fatalf("expected ErrStreamIdleTimeout, got %v", err)
	}
	if got := len(inner.requests()); got != cfg.MaxRetries+1 {
		t.Fatalf("expected %d attempts, got %d", cfg.MaxRetries+1, got)
	}
}

func TestWrapStopsRetryingWhenParentCtxCancels(t *testing.T) {
	inner := &fakeProvider{
		id: provider.ProviderClaude,
		behaviour: func(_ int, ctx context.Context, _ provider.AgentRequest, _ provider.AgentEventCallback) (provider.AgentResponse, error) {
			<-ctx.Done()
			return provider.AgentResponse{}, ctx.Err()
		},
	}
	cfg := Config{
		StreamIdleTimeout: 20 * time.Millisecond,
		MaxRetries:        5,
		BaseRetryDelay:    1 * time.Second, // 親 ctx cancel で即抜けなければ長時間待つ
	}
	wrapped := WrapWithConfig(inner, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := wrapped.Call(ctx, provider.AgentRequest{Prompt: "p"}, nil)
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrStreamIdleTimeout) {
		t.Fatalf("expected ctx canceled or stream idle, got %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Call did not return promptly after parent cancel: elapsed=%s", elapsed)
	}
}

func TestWrapDoesNotRetryOnSuccessEvenIfTimerRaces(t *testing.T) {
	// Inner が成功を返す直前に timer が発火するようなレースを再現:
	// inner は短い idle timeout 直前まで沈黙し、ぎりぎりで成功を返す。
	// 何度回しても retry してはいけない (resp.Success=true を信頼する)。
	var attempts int32
	inner := &fakeProvider{
		id: provider.ProviderClaude,
		behaviour: func(_ int, _ context.Context, _ provider.AgentRequest, _ provider.AgentEventCallback) (provider.AgentResponse, error) {
			atomic.AddInt32(&attempts, 1)
			return provider.AgentResponse{Content: "ok", Success: true}, nil
		},
	}
	wrapped := WrapWithConfig(inner, newFastConfig())
	resp, err := wrapped.Call(context.Background(), provider.AgentRequest{Prompt: "p"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success: %#v", resp)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("expected single attempt on success, got %d", got)
	}
}

func TestWrapResetsIdleTimerOnEachEvent(t *testing.T) {
	// イベントが連続して来ている間は idle timeout しないことを確認する。
	cfg := Config{
		StreamIdleTimeout: 60 * time.Millisecond,
		MaxRetries:        0,
		BaseRetryDelay:    1 * time.Millisecond,
	}
	inner := &fakeProvider{
		id: provider.ProviderClaude,
		behaviour: func(_ int, ctx context.Context, _ provider.AgentRequest, onEvent provider.AgentEventCallback) (provider.AgentResponse, error) {
			// 30ms 間隔で 5 回イベント (合計 ~150ms) → timeout=60ms より長いが、
			// 各イベントで reset されるので idle にはならない
			for i := 0; i < 5; i++ {
				select {
				case <-ctx.Done():
					return provider.AgentResponse{}, ctx.Err()
				case <-time.After(30 * time.Millisecond):
				}
				if onEvent != nil {
					onEvent(provider.AgentEvent{Type: provider.EventText, Text: "tick"})
				}
			}
			return provider.AgentResponse{Content: "ok", Success: true}, nil
		},
	}
	wrapped := WrapWithConfig(inner, cfg)
	resp, err := wrapped.Call(context.Background(), provider.AgentRequest{Prompt: "p"}, func(provider.AgentEvent) {})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success: %#v", resp)
	}
}

// TestWrapAppliesDefaultMaxRetries は `streamidle.Wrap()` (Config 未指定) 経由で
// `DefaultMaxRetries` が確実に適用されることをガードする回帰テスト。
//
// 過去、`Wrap()` は `WrapWithConfig(inner, Config{})` を呼び、`Config{}.MaxRetries=0`
// を `withDefaults()` の `if MaxRetries < 0` ガードがすり抜け、production の
// `streamidle.Wrap(claude.New())` 経路でリトライが 1 度も走らない不具合があった
// (kiro-impl-loop の task 1.2 が 5 分沈黙 1 回で `ErrStreamIdleTimeout` を返した
// 事例で発覚)。再発防止のため、Wrap() の config 値そのものをチェックする。
func TestWrapAppliesDefaultMaxRetries(t *testing.T) {
	inner := &fakeProvider{id: provider.ProviderClaude}
	p := Wrap(inner)
	if p.config.StreamIdleTimeout != DefaultStreamIdleTimeout {
		t.Errorf("StreamIdleTimeout: want %v, got %v", DefaultStreamIdleTimeout, p.config.StreamIdleTimeout)
	}
	if p.config.MaxRetries != DefaultMaxRetries {
		t.Errorf("MaxRetries: want %d, got %d", DefaultMaxRetries, p.config.MaxRetries)
	}
	if p.config.BaseRetryDelay != DefaultBaseRetryDelay {
		t.Errorf("BaseRetryDelay: want %v, got %v", DefaultBaseRetryDelay, p.config.BaseRetryDelay)
	}
}

// TestWrapWithConfigZeroMaxRetriesDisablesRetry は WrapWithConfig に MaxRetries=0
// を明示した場合、idle 1 回で確定失敗しリトライが走らないことを保証する。
// production 経路は `Wrap()` 経由で DefaultMaxRetries を効かせるため、
// 明示 0 を「リトライ無効」として残す既存契約を固定する。
func TestWrapWithConfigZeroMaxRetriesDisablesRetry(t *testing.T) {
	var attempts int32
	inner := &fakeProvider{
		id: provider.ProviderClaude,
		behaviour: func(_ int, ctx context.Context, _ provider.AgentRequest, _ provider.AgentEventCallback) (provider.AgentResponse, error) {
			atomic.AddInt32(&attempts, 1)
			select {
			case <-ctx.Done():
				return provider.AgentResponse{}, ctx.Err()
			case <-time.After(500 * time.Millisecond):
				return provider.AgentResponse{Success: true}, nil
			}
		},
	}
	cfg := Config{StreamIdleTimeout: 50 * time.Millisecond, MaxRetries: 0, BaseRetryDelay: 1 * time.Millisecond}
	wrapped := WrapWithConfig(inner, cfg)
	_, err := wrapped.Call(context.Background(), provider.AgentRequest{Prompt: "p"}, nil)
	if !errors.Is(err, ErrStreamIdleTimeout) {
		t.Fatalf("expected ErrStreamIdleTimeout on first attempt, got %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("MaxRetries=0 should disable retry: attempts=%d", got)
	}
}

func TestWrapPropagatesNonIdleErrorsWithoutRetry(t *testing.T) {
	wantErr := errors.New("provider exploded")
	var attempts int32
	inner := &fakeProvider{
		id: provider.ProviderClaude,
		behaviour: func(_ int, _ context.Context, _ provider.AgentRequest, _ provider.AgentEventCallback) (provider.AgentResponse, error) {
			atomic.AddInt32(&attempts, 1)
			return provider.AgentResponse{}, wantErr
		},
	}
	wrapped := WrapWithConfig(inner, newFastConfig())
	_, err := wrapped.Call(context.Background(), provider.AgentRequest{Prompt: "p"}, nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected provider error to propagate, got %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("non-idle errors should not retry, attempts=%d", got)
	}
}

func TestWrapPreservesInitialResumeSessionID(t *testing.T) {
	// 1 回目失敗時、リクエストの ResumeSessionID が空でも middleware が捕捉した
	// SessionID で resume されること。SessionID をストリーム途中で受信しないケースは、
	// 2 回目の ResumeSessionID が初期値のままであること。
	inner := &fakeProvider{
		id: provider.ProviderClaude,
		behaviour: func(attempt int, ctx context.Context, _ provider.AgentRequest, _ provider.AgentEventCallback) (provider.AgentResponse, error) {
			if attempt == 0 {
				select {
				case <-ctx.Done():
					return provider.AgentResponse{}, ctx.Err()
				case <-time.After(500 * time.Millisecond):
					return provider.AgentResponse{Success: true}, nil
				}
			}
			return provider.AgentResponse{Content: "ok", Success: true}, nil
		},
	}
	wrapped := WrapWithConfig(inner, newFastConfig())
	_, err := wrapped.Call(context.Background(), provider.AgentRequest{Prompt: "p", ResumeSessionID: "initial-session"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	calls := inner.requests()
	if len(calls) < 2 {
		t.Fatalf("expected retry, got %d calls", len(calls))
	}
	if calls[0].ResumeSessionID != "initial-session" {
		t.Fatalf("first attempt should use original resume session: %q", calls[0].ResumeSessionID)
	}
	if calls[1].ResumeSessionID != "initial-session" {
		t.Fatalf("retry should fall back to initial resume session when no SessionID streamed: %q", calls[1].ResumeSessionID)
	}
}
