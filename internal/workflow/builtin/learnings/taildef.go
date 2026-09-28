// Package learnings の本 file は agent-learnings tail mode 専用 (`Hidden=true`)
// の `workflow.Definition` factory `LearningsTailDef` を提供する
// (tasks.md 8.2 / design.md §Tail Mode Activation / Req 1.1, 1.3)。
//
// 役割:
//
//   - manual mode 不在 (Req 1.1) のため `Workflow(agent)` の Definition を tail 専用 alias
//     として export する。両者は ID / Hidden / Args / Fn のいずれも同一であり、本 factory は
//     呼び出し側 (registry seam / SequentialWithLearningsTail) が「tail 専用 Definition を
//     要求している」という意図を表す API 区別の seam。
//   - `Definition.Hidden=true` で `rilll list` / TUI トップから除外し、
//     `SequentialWithLearningsTail` 経由でのみ起動される hand-off 専用 Definition とする。
//   - Args は nil (`Workflow(agent)` 同様 / manual mode 不在 / Req 1.1)。
//
// 設計契約:
//
//   - 内部実装は `Workflow(agent)` の Definition をそのまま返す (DRY)。`runLearningsOrchestration`
//     の ctx 注入経路 (`ParentRunIDFromContext`) は本 factory 経由でも `Workflow` 経由でも
//     同じ resolver で動作する。
//   - 外部から見て「TailDef を返した」という意味的差別化のみを目的とし、内部 Definition は
//     完全等価。
//   - 将来的に tail 専用の挙動 (例: 別 ID や Args 制限) を導入する場合は本 file 内で
//     差分を持たせる seam として残す。
//
// 関連 Requirement: 1.1 (manual mode 不在 = Hidden=true) / 1.3 (`SequentialWithLearningsTail`
// 経由でのみ起動される)。
package learnings

import (
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// LearningsTailDef は tail mode 専用 (`Hidden=true`) の `workflow.Definition` を返す
// (tasks.md 8.2 / design.md §Tail Mode Activation)。
//
// Definition のフィールド契約:
//   - ID: "agent-learnings" (Workflow と同一 / Req 1.1 で manual mode 不在のため別 ID 不要)
//   - Label: agent-learnings tail (post-run reflection)
//   - Args: nil (manual mode 不在 / Req 1.1)
//   - Hidden: true (rilll list / TUI から除外)
//   - Recorder: `workflow.RecorderFromAgent(agent)`
//
// Fn 本体は `Workflow(agent).Fn` を再利用する。
// `runLearningsOrchestration` 側で `ParentRunIDFromContext(ctx)` から parent_run_id を
// 解決する契約 (workflow.go) を満たすため、本 factory は orchestration の実装を再生せず
// `Workflow` を再利用する。
//
// agent==nil でも Definition 構築は成功する (`Workflow(agent)` が nil-safe に Definition を
// 返すため)。
func LearningsTailDef(agent workflowctx.AgentCaller) workflow.Definition {
	// manual mode が存在しないため、Workflow(agent) の Definition をそのまま返す。
	// 将来 tail 専用の挙動を入れる場合は本 factory 内で field を上書きする (現在は no-op alias)。
	return Workflow(agent)
}
