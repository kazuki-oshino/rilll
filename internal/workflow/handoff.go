// Package workflow の hand-off 契約。
//
// 本ファイルは TUI (akashic モード Run 詳細サブビュー) から workflow-self-improve
// への hand-off 契約を定義する SSoT (akashic-mode-tui spec の Requirement 7)。
// フィールド名・配列形状は SSoT であり、`workflow-self-improve` 側の brief.md
// "Hand-off SSoT" から逆参照される。両者の編集時は双方向の整合を維持すること。
//
// 設計上の注意:
//   - HandoffParams は process 内 in-memory のみで、永続化対象外 (Definition.Run
//     の ctx を介して run スコープでのみ存在する)
//   - ctx-key は private 型で、外部パッケージからは ContextWithHandoff /
//     HandoffFromContext 経由でしか操作できない
//   - 既存 Definition / WorkflowAsk / Func 契約は変更しない (handoff は ctx
//     経由で受け渡し、Run の ctx パススルー特性に乗る)
package workflow

import "context"

// handoffKey は ctx 上で HandoffParams を運ぶための private 型。
// 別パッケージから ctx を直接覗けないようにする (ContextWithRecorder と同パターン)。
type handoffKey struct{}

// HandoffParams は TUI から workflow-self-improve への hand-off 契約
// (akashic-mode-tui spec Requirement 7 の SSoT)。
//
//   - TargetWorkflowID: 改善対象の workflow ID (kebab-case)
//   - RunIDs: 参考 run の workflow_run_id (順序保持の配列、MVP は length=1)
//
// 配列型・フィールド名・対象 workflow ID の引き渡し方は破壊的に変更しない (Req 7.10)。
// 将来 length > 1 への拡張があっても hand-off 契約を破壊しないよう、配列型は
// MVP 段階から固定する。
type HandoffParams struct {
	TargetWorkflowID string
	RunIDs           []string
}

// ContextWithHandoff は HandoffParams を ctx に注入する。
// Definition.Run 実行前に呼ぶことで、Run の ctx パススルー特性により
// Func 内で HandoffFromContext から取り出せる。
func ContextWithHandoff(ctx context.Context, params HandoffParams) context.Context {
	return context.WithValue(ctx, handoffKey{}, params)
}

// HandoffFromContext は ctx に注入された HandoffParams を返す。
// 未注入のときは (HandoffParams{}, false) を返す。
// 子 ctx (追加の context.WithValue で wrap した後) でも、context の親方向
// チェーン探索により注入された値を取り出せる。
func HandoffFromContext(ctx context.Context) (HandoffParams, bool) {
	params, ok := ctx.Value(handoffKey{}).(HandoffParams)
	return params, ok
}
