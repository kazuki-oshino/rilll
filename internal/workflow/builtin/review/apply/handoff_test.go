// Package apply のテスト本ファイルは Sequential 経路で前段 review-workflow の runID を
// 後段 apply.Workflow に伝達する ctx key 注入/抽出 helper の契約を検証する
// (tasks.md 1.2 / design.md §Handoff / Req 1.2, 8.5)。
//
// 検証観点:
//   - 未注入 ctx に対する ReviewRunIDFromContext は ("", false) を返す
//     (副作用ゼロでの fallback 判定に必要な契約)。
//   - ContextWithReviewRunID で注入した値が ReviewRunIDFromContext で
//     同値かつ第二戻り値 true で抽出できる (round-trip 保証)。
package apply

import (
	"context"
	"testing"
)

// TestReviewRunIDFromContextMissing は ctx に reviewRunID が注入されていない場合に
// ReviewRunIDFromContext が ("", false) を返すことを検証する。
//
// この契約は Workflow Fn 入口で「ctx 注入があるか」を見極める判定 (5.1) に必要で、
// 未注入時に false が返らないと「空文字列の reviewRunID で先に進む」誤動作を招く。
func TestReviewRunIDFromContextMissing(t *testing.T) {
	ctx := context.Background()

	got, ok := ReviewRunIDFromContext(ctx)
	if ok {
		t.Errorf("ReviewRunIDFromContext: ok = true, want false (no injection)")
	}
	if got != "" {
		t.Errorf("ReviewRunIDFromContext: got = %q, want empty string", got)
	}
}

// TestReviewRunIDContextRoundTrip は ContextWithReviewRunID で注入した reviewRunID が
// ReviewRunIDFromContext で同値かつ第二戻り値 true で抽出できる round-trip 保証を検証する。
//
// design.md §Handoff の契約:
//   - ContextWithReviewRunID は新しい ctx を返す (元 ctx は不変)。
//   - 抽出値は注入値と完全に一致する (string の identity を保つ)。
func TestReviewRunIDContextRoundTrip(t *testing.T) {
	const reviewRunID = "20260505T010203Z-abcd1234"

	ctx := ContextWithReviewRunID(context.Background(), reviewRunID)

	got, ok := ReviewRunIDFromContext(ctx)
	if !ok {
		t.Fatalf("ReviewRunIDFromContext: ok = false, want true (after injection)")
	}
	if got != reviewRunID {
		t.Errorf("ReviewRunIDFromContext: got = %q, want %q", got, reviewRunID)
	}
}
