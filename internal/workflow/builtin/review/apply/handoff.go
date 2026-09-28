// Package apply の本ファイルは Sequential 経路 (`review-and-apply`) で前段
// `review-workflow` の runID を後段 `apply.Workflow` に伝達するための ctx key
// 注入/抽出 helper を提供する (design.md §Handoff / Req 1.2, 8.5)。
//
// 設計契約:
//   - `reviewRunIDKey` は **パッケージ private 型** とし、外部パッケージから
//     同等 key を構築できないようにすることで ctx key の一意性を構造的に保証する
//     (design.md §Handoff §Concurrency strategy)。
//   - `internal/workflow/handoff.go` の `HandoffParams` は akashic-mode-tui からの
//     hand-off 用 SSoT のため流用しない (boundary 違反回避: design.md §Architecture
//     Decisions §3 第二段)。本パッケージ内に専用 ctx key を閉じ込める。
//   - apply.Workflow Fn 入口では `ReviewRunIDFromContext` を **第一優先**、
//     `--review-run-id` arg を **第二優先** として reviewRunID を解決する
//     (design.md §Sequential 経路: ctx 注入 → arg fallback)。
package apply

import "context"

// reviewRunIDKey はパッケージ private な ctx key 型。
// 空構造体型を ctx key として用いる Go 標準の慣習に従い、
// 外部パッケージは同型を構築できないため key 衝突を構造的に排除する
// (design.md §Handoff §State Management)。
type reviewRunIDKey struct{}

// ContextWithReviewRunID は ctx に reviewRunID を注入した新しい ctx を返す。
// 元 ctx は不変 (context.Context の不変契約に従う)。
//
// Sequential ラッパ (`apply.ReviewAndApply`) が前段 `review.Workflow` の
// `WorkflowEvent.RunID` から抽出した reviewRunID を後段 `apply.Workflow` の Fn
// 呼び出し前に注入するために使用する (design.md §Sequential)。
func ContextWithReviewRunID(ctx context.Context, reviewRunID string) context.Context {
	return context.WithValue(ctx, reviewRunIDKey{}, reviewRunID)
}

// ReviewRunIDFromContext は ctx に注入された reviewRunID を返す。
// 第二戻り値は注入有無を表し、未注入の場合は ("", false) を返す。
//
// apply.Workflow Fn 入口で reviewRunID 二段階解決 (ctx 第一優先 / arg 第二優先) の
// 第一段として呼び出される (design.md §Workflow §reviewRunID resolution)。
// 未注入時に第二戻り値 false を返すことが arg fallback 判定の必須契約となる
// (空文字列で先に進めると `precondition_failed` の誤検知を招く)。
func ReviewRunIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(reviewRunIDKey{}).(string)
	if !ok {
		return "", false
	}
	return v, true
}
