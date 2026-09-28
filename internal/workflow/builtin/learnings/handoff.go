package learnings

// 本 file は agent-learnings tail mode 経路 (`SequentialWithLearningsTail`) で前段
// 親 workflow の RunID を後段 `TailDef` の Fn に伝達するための ctx key 注入/抽出
// helper を提供する (design.md §Tail Mode Activation §Handoff / Requirement 1.2, 3.1)。
//
// 設計契約:
//
//   - `parentRunIDKey` は **package private 型** とし、外部 package から同等 key を
//     構築できないようにすることで ctx key の一意性を構造的に保証する
//     (Go 標準慣習: 空構造体型を ctx key に使用)。
//   - 本 spec は manual mode 起動経路 (`rilll run agent-learnings`) を持たず、tail wrap
//     経路で必ず parent_run_id が ctx に注入される (design.md §Tail Mode Activation,
//     §LearningsWorkflow Preconditions L446)。したがって v1 distiller のような
//     `(string, bool)` 二段階 fallback シグネチャは採用せず、単一 `string` の
//     戻り値で「未設定 = 空文字列」を表現する単純化を行う。
//   - 空文字列の `parentRunID` は注入しない契約 — `parentRunID == ""` のときは
//     `ContextWithParentRunID` が元 ctx をそのまま返すことで、未注入 ctx と観測上
//     区別できないようにする (PreFetch phase が「parent_run_id 未設定」を一貫した
//     経路で検知できるようにするため)。
//
// 関連 Requirement: 1.2 (`DistillLearningsAfter=true` で `<base-id>-with-learnings`
// wrap exposure / parent_run_id ctx hand-off) / 3.1 (PreFetch が parent_run_id から
// agent_calls を取得する起点)。

import "context"

// parentRunIDKey は package private な ctx key 型。
// 空構造体型を ctx key として用いる Go 標準慣習に従い、外部 package は同型を
// 構築できないため key 衝突を構造的に排除する。
type parentRunIDKey struct{}

// earlyAggregationEnabledKey は tail 専用 early aggregation marker の ctx key 型。
// parentRunIDKey と別型にすることで handoff marker との key 衝突を構造的に排除する。
type earlyAggregationEnabledKey struct{}

// ContextWithParentRunID は ctx に parentRunID を注入した新しい ctx を返す。
// 元 ctx は不変 (`context.Context` の不変契約に従う)。
//
// `SequentialWithLearningsTail` (task 8.3) が前段 base.Run の `WorkflowEvent.RunID` から
// 抽出した parentRunID を後段 `TailDef` の Fn 呼び出し前に注入するために使用する。
//
// parentRunID が空文字列の場合は注入を skip し元 ctx をそのまま返す。これにより
// `ParentRunIDFromContext` での観測結果は「未注入」と区別できなくなり、PreFetch phase は
// 「ctx に parent_run_id が無い」状態を一貫した経路で扱える。
func ContextWithParentRunID(ctx context.Context, parentRunID string) context.Context {
	if parentRunID == "" {
		return ctx
	}
	return context.WithValue(ctx, parentRunIDKey{}, parentRunID)
}

// ParentRunIDFromContext は ctx に注入された parentRunID を返す。
// 未注入の場合 / `ContextWithParentRunID` に空文字列を渡された場合はいずれも
// 空文字列 `""` を返す (両者を観測上区別しない)。
//
// PreFetch phase (task 7.1) の入口で本関数を呼び、空文字列なら precondition 違反と
// 判定する。tail wrap 経路では `SequentialWithLearningsTail` が必ず注入するため、
// 通常運用では空文字列にはならない (design.md L446 の Precondition)。
func ParentRunIDFromContext(ctx context.Context) string {
	v, ok := ctx.Value(parentRunIDKey{}).(string)
	if !ok {
		return ""
	}
	return v
}

// ContextWithEarlyAggregationEnabled は ctx に tail 専用の early aggregation marker を
// 注入した新しい ctx を返す。
//
// marker は runtime-only の true-only signal であり、project ID、store、target list、
// provider policy は持たない。`SequentialWithLearningsTail` が store-backed runtime 起動に
// 成功した場合だけ tail ctx に注入し、tail workflow は marker の有無だけで early aggregation
// 分岐と legacy fallback 分岐を判定する。
func ContextWithEarlyAggregationEnabled(ctx context.Context) context.Context {
	return context.WithValue(ctx, earlyAggregationEnabledKey{}, true)
}

// EarlyAggregationEnabledFromContext は ctx に tail 専用 early aggregation marker が
// 注入されているかを返す。未注入の場合は false。
func EarlyAggregationEnabledFromContext(ctx context.Context) bool {
	enabled, ok := ctx.Value(earlyAggregationEnabledKey{}).(bool)
	return ok && enabled
}
