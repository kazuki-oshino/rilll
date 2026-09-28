package bridge

import (
	"context"
	"sync/atomic"
)

// cancelOriginKey は ctx 上で *atomic.Value (cancel origin の保持先) を運ぶための private 型。
// 外部パッケージから ctx を直接覗けないようにする (workflowctx.runMetadataKey と同形)。
type cancelOriginKey struct{}

// WithCancelOrigin は cancel origin を保持する *atomic.Value を ctx に attach する。
//
// design.md (R-1 closing + recordAgentCallFinish extension Implementation Notes) で固定された
// 「ctx atomic 経由で cancel origin を運搬する」契約のヘルパ。
//
// 寿命: 1 atomic.Value = 1 agent_call の per-call スコープ。`agent.CallAgent` 入口で
// 新規生成し、call return 時に UI から unregister する経路を想定する (Issue 1/2 closing)。
//
// nil ref を受け取った場合でも ctx には key を attach するが、後続の
// CancelOriginFromContext は空文字を返す (defense-in-depth)。
func WithCancelOrigin(ctx context.Context, ref *atomic.Value) context.Context {
	return context.WithValue(ctx, cancelOriginKey{}, ref)
}

// CancelOriginFromContext は ctx に attach された *atomic.Value から cancel origin を読み出す。
//
// 返り値の closed enum 解釈:
//   - "" (空文字): ctx に未 attach、または atomic.Value に未 Store / non-string Store
//   - "user":   CancelOriginUser (UI ctrl+c など user 起源 cancel)
//   - "engine": CancelOriginEngine (engine 起源 cancel / 正常終了)
//
// 別 goroutine からの呼び出しでも sync/atomic 規約により同値を読み取れる
// (ConsumeEvents が別 goroutine で result event の origin を Store する経路の前提)。
func CancelOriginFromContext(ctx context.Context) string {
	ref, ok := ctx.Value(cancelOriginKey{}).(*atomic.Value)
	if !ok || ref == nil {
		return ""
	}
	v := ref.Load()
	if v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

// StoreCancelOrigin は ctx に attach された *atomic.Value に origin 文字列を Store する。
//
// CancelOriginFromContext と対称な writer 経路で、cancelOriginKey を private に
// 保ったまま外部パッケージ (nodebridge launcher) から書き込み可能にするためのヘルパ。
//
// no-op となる条件 (defense-in-depth):
//   - origin が空文字 (cancel が来ていない正常終了の result event はここに該当)
//   - ctx に *atomic.Value が未 attach
//   - attach された ref が nil
//
// 想定呼び出し元: nodebridge/launcher.go の ConsumeEvents が `result` event の
// `Origin` field を読み、agent 層 (recordAgentCallFinish) へ ctx atomic 経由で
// 透過する単方向経路 (design.md task 2.6 / Req 3.2)。
func StoreCancelOrigin(ctx context.Context, origin string) {
	if origin == "" {
		return
	}
	ref, ok := ctx.Value(cancelOriginKey{}).(*atomic.Value)
	if !ok || ref == nil {
		return
	}
	ref.Store(origin)
}
