// Package apply のテスト本ファイルは task 6.1 / design.md §Sequential / Req 1.2,
// 8.3, 8.4, 8.5 で要求される `review-and-apply` Sequential ラッパの不変式を検証する。
//
// 検証観点:
//
//  1. **TestReviewAndApplyDefinition**:
//     `apply.ReviewAndApply(agent)` が返す Definition の基本契約 (ID / Args / Hidden /
//     Recorder) を検証する (design.md §Sequential Service Interface / Req 8.3)。
//     Args は `review.Workflow(agent).Args` を直接コピーして継承する契約。
//
//  2. **parity (a) 前段 nil → 後段 1 回起動**:
//     `workflow.Sequential` の `TestSequentialRunsChildrenInOrder` と同等の挙動 (前段が
//     エラー無く完了したとき後段 Fn が 1 回呼ばれる) を本 orchestration が再現することを
//     確認する (design.md §Sequential §Postconditions)。
//
//  3. **parity (b) 前段 error → 後段未起動 + error 伝播 (戻り値経路)**:
//     `workflow.Sequential` の `TestSequentialStopsOnFirstChildReturningError` と同等。
//     前段 Fn が non-nil error を返したとき後段が未起動かつ同 error が外向きに伝播する。
//
//  4. **parity (b') 前段 WorkflowError event → 後段未起動 + error 伝播 (event 経路)**:
//     `workflow.Sequential` の `TestSequentialStopsOnWorkflowErrorEvent` と同等。
//     前段が WorkflowError イベントを発したが Fn は nil を返した場合でも、後段は起動せず
//     event の Message が error として外向きに伝播する。
//
//  5. **parity (c) 子の onEvent → 親への転送順序**:
//     `workflow.Sequential` の `TestSequentialForwardsChildEventsToParent` と同等。
//     前段の events が後段起動前に親 onEvent に届く (時系列順序保持) ことを確認する。
//
//  6. **TestReviewAndApply_InjectsReviewRunIDIntoCtx**:
//     前段 review の WorkflowRunID を `WorkflowEvent.RunID` 経由で抽出し、
//     `ContextWithReviewRunID(ctx, reviewRunID)` で後段 ctx に注入したことを後段 Fn が
//     `ReviewRunIDFromContext(ctx)` で観測できることを確認する (Req 1.2, 8.5)。
package apply

import (
	"context"
	"errors"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/workflow"
	reviewpkg "github.com/kazuki-oshino/rilll/internal/workflow/builtin/review"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// stubDef は Sequential ラッパテスト用の最小 Definition stub helper。
// Fn 内で fn を実行し、emit イベントを順番に発火する。
//
// emit に渡された WorkflowEvent は順次 onEvent に流される。fn は events 発火前に呼ばれ、
// その戻り値が Fn の戻り値となる。fn 内で record が必要な場合は closure で外部に持ち出す。
func stubDef(id, label string, fn func(ctx context.Context) error, emit []workflow.WorkflowEvent) workflow.Definition {
	return workflow.Definition{
		ID:    id,
		Label: label,
		Fn: func(ctx context.Context, _ string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			for _, e := range emit {
				if onEvent != nil {
					onEvent(e)
				}
			}
			if fn != nil {
				return fn(ctx)
			}
			return nil
		},
	}
}

// TestReviewAndApplyDefinition は `apply.ReviewAndApply(agent)` が返す Definition の
// 基本契約を検証する (design.md §Sequential Service Interface / Req 8.3)。
//
// 期待:
//   - Definition.ID は "review-and-apply" (kebab-case)
//   - Definition.Args は `review.Workflow(agent).Args` を継承 (= --pr 必須)
//   - Definition.Hidden=false (rilll list / TUI トップに通常表示)
//   - Definition.Fn が non-nil
func TestReviewAndApplyDefinition(t *testing.T) {
	def := ReviewAndApply(nil)

	if def.ID != "review-and-apply" {
		t.Errorf("Definition.ID=%q, want %q", def.ID, "review-and-apply")
	}
	if def.Hidden {
		t.Errorf("Definition.Hidden=true, want false (rilll list 通常表示)")
	}
	if def.Fn == nil {
		t.Errorf("Definition.Fn が nil")
	}

	// Args が review.Workflow.Args を継承していること (= --pr 必須)。
	wantArgs := reviewpkg.Workflow(nil).Args
	if len(def.Args) != len(wantArgs) {
		t.Fatalf("len(Args)=%d, want %d (review.Workflow.Args を継承する契約)", len(def.Args), len(wantArgs))
	}
	for i, want := range wantArgs {
		got := def.Args[i]
		if got.Key != want.Key || got.Required != want.Required {
			t.Errorf("Args[%d]={Key:%q, Required:%v}, want {Key:%q, Required:%v}",
				i, got.Key, got.Required, want.Key, want.Required)
		}
	}
}

// TestReviewAndApply_ParityFrontStageNilRunsApply は parity (a):
// 前段 Fn が nil 返却 (WorkflowError event 無し) のとき、後段 Fn が 1 回呼ばれることを検証する。
// `workflow.Sequential` の `TestSequentialRunsChildrenInOrder` と同等の挙動を要求する。
func TestReviewAndApply_ParityFrontStageNilRunsApply(t *testing.T) {
	var reviewCalls, applyCalls int
	reviewDef := stubDef("test-review", "Test Review", func(_ context.Context) error {
		reviewCalls++
		return nil
	}, nil)
	applyDef := stubDef("test-apply", "Test Apply", func(_ context.Context) error {
		applyCalls++
		return nil
	}, nil)

	deps := reviewAndApplyDeps{reviewDef: reviewDef, applyDef: applyDef}
	err := runReviewAndApplyOrchestration(context.Background(), t.TempDir(), nil, nil, deps)
	if err != nil {
		t.Fatalf("orchestration returned error: %v (前段 nil なら後段が起動し成功)", err)
	}
	if reviewCalls != 1 {
		t.Errorf("reviewCalls=%d, want 1", reviewCalls)
	}
	if applyCalls != 1 {
		t.Errorf("applyCalls=%d, want 1 (parity (a): 前段 nil → 後段 1 回起動)", applyCalls)
	}
}

// TestReviewAndApply_ParityFrontStageErrorSkipsApply は parity (b) (戻り値経路):
// 前段 Fn が non-nil error を返したとき、後段 Fn が起動せず同 error が外向きに伝播することを検証する。
// `workflow.Sequential` の `TestSequentialStopsOnFirstChildReturningError` と同等。
func TestReviewAndApply_ParityFrontStageErrorSkipsApply(t *testing.T) {
	wantErr := errors.New("review-boom")
	var applyCalls int
	reviewDef := stubDef("test-review", "Test Review", func(_ context.Context) error {
		return wantErr
	}, nil)
	applyDef := stubDef("test-apply", "Test Apply", func(_ context.Context) error {
		applyCalls++
		return nil
	}, nil)

	deps := reviewAndApplyDeps{reviewDef: reviewDef, applyDef: applyDef}
	err := runReviewAndApplyOrchestration(context.Background(), t.TempDir(), nil, nil, deps)
	if err == nil {
		t.Fatalf("orchestration returned nil (前段 error を期待)")
	}
	if !errors.Is(err, wantErr) && err.Error() != wantErr.Error() {
		t.Errorf("err=%v, want %v (同 error 伝播)", err, wantErr)
	}
	if applyCalls != 0 {
		t.Errorf("applyCalls=%d, want 0 (parity (b): 前段 error → 後段未起動)", applyCalls)
	}
}

// TestReviewAndApply_ParityFrontStageWorkflowErrorEventSkipsApply は parity (b') (event 経路):
// 前段が WorkflowError イベントを発したが Fn は nil を返した場合でも、後段が起動せず
// event の Message が error として外向きに伝播することを検証する。
// `workflow.Sequential` の `TestSequentialStopsOnWorkflowErrorEvent` と同等。
func TestReviewAndApply_ParityFrontStageWorkflowErrorEventSkipsApply(t *testing.T) {
	const errMsg = "[verdict:failed] agent failed"
	var applyCalls int
	reviewDef := stubDef("test-review", "Test Review", nil, []workflow.WorkflowEvent{
		{Type: workflow.WorkflowError, Message: errMsg},
	})
	applyDef := stubDef("test-apply", "Test Apply", func(_ context.Context) error {
		applyCalls++
		return nil
	}, nil)

	deps := reviewAndApplyDeps{reviewDef: reviewDef, applyDef: applyDef}
	err := runReviewAndApplyOrchestration(context.Background(), t.TempDir(), nil, nil, deps)
	if err == nil {
		t.Fatalf("orchestration returned nil (WorkflowError event → error 変換を期待)")
	}
	if err.Error() != errMsg {
		t.Errorf("err=%q, want %q (WorkflowError.Message → error.Error() 同値)", err.Error(), errMsg)
	}
	if applyCalls != 0 {
		t.Errorf("applyCalls=%d, want 0 (parity (b'): WorkflowError → 後段未起動)", applyCalls)
	}
}

// TestReviewAndApply_ParityForwardsChildEventsInOrder は parity (c):
// 子 (前段 review / 後段 apply) の onEvent が親 onEvent に時系列順序で転送されることを検証する。
// `workflow.Sequential` の `TestSequentialForwardsChildEventsToParent` と同等。
//
// 期待: 親が見る WorkflowText イベント順序が
//   - "review-step-1" → "review-step-2" → "apply-step-1"
//
// となり、前段 events が後段 Fn 起動前に親に届くこと (順序保持) を assert する。
func TestReviewAndApply_ParityForwardsChildEventsInOrder(t *testing.T) {
	var firedOrder []string
	reviewDef := workflow.Definition{
		ID:    "test-review",
		Label: "Test Review",
		Fn: func(_ context.Context, _ string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "review-step-1"})
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "review-step-2"})
			return nil
		},
	}
	applyDef := workflow.Definition{
		ID:    "test-apply",
		Label: "Test Apply",
		Fn: func(_ context.Context, _ string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			firedOrder = append(firedOrder, "apply-fn-entered")
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "apply-step-1"})
			return nil
		},
	}

	deps := reviewAndApplyDeps{reviewDef: reviewDef, applyDef: applyDef}
	var parentTexts []string
	parent := func(e workflow.WorkflowEvent) {
		if e.Type == workflow.WorkflowText {
			parentTexts = append(parentTexts, e.Text)
		}
	}

	err := runReviewAndApplyOrchestration(context.Background(), t.TempDir(), parent, nil, deps)
	if err != nil {
		t.Fatalf("orchestration returned error: %v", err)
	}

	wantTexts := []string{"review-step-1", "review-step-2", "apply-step-1"}
	if len(parentTexts) != len(wantTexts) {
		t.Fatalf("parent text events=%v, want %v (parity (c) 順序保持)", parentTexts, wantTexts)
	}
	for i, want := range wantTexts {
		if parentTexts[i] != want {
			t.Errorf("parentTexts[%d]=%q, want %q", i, parentTexts[i], want)
		}
	}
	// apply Fn が起動したことを independent な firedOrder slice でも anchor。
	if len(firedOrder) != 1 || firedOrder[0] != "apply-fn-entered" {
		t.Errorf("firedOrder=%v, want [apply-fn-entered] (apply Fn が 1 回起動)", firedOrder)
	}
}

// TestReviewAndApply_InjectsReviewRunIDIntoCtx は前段 review の WorkflowRunID を
// `WorkflowEvent.RunID` 経由で抽出し、後段 apply の ctx に
// `ContextWithReviewRunID(ctx, reviewRunID)` で注入したことを後段 Fn が観測できることを検証する
// (design.md §Sequential / Req 1.2, 8.5)。
//
// 検証フロー:
//  1. 前段 review stub: ctx の RunMetadata.WorkflowRunID を変数 reviewRunIDFromMetadata に保存し、
//     1 件 WorkflowText イベントを発火する (これにより `Definition.Run` が `event.RunID` に runID を埋める)。
//  2. 後段 apply stub: ctx から `ReviewRunIDFromContext` で reviewRunID を取り出し、
//     reviewRunIDFromCtx に保存する。
//  3. 両者が同値かつ非空であることを assert する。
func TestReviewAndApply_InjectsReviewRunIDIntoCtx(t *testing.T) {
	var reviewRunIDFromMetadata, reviewRunIDFromCtx string
	var reviewRunIDOK bool

	reviewDef := workflow.Definition{
		ID:    "test-review",
		Label: "Test Review",
		Fn: func(ctx context.Context, _ string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			if md, ok := workflowctx.RunMetadataFromContext(ctx); ok {
				reviewRunIDFromMetadata = md.WorkflowRunID
			}
			// `Definition.Run` の wrappedOnEvent が `event.RunID = runID` を埋める契約に従い、
			// 親側 (orchestration) が runID を抽出するための anchor として 1 件発火する。
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "review-tick"})
			return nil
		},
	}
	applyDef := workflow.Definition{
		ID:    "test-apply",
		Label: "Test Apply",
		Fn: func(ctx context.Context, _ string, _ func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			reviewRunIDFromCtx, reviewRunIDOK = ReviewRunIDFromContext(ctx)
			return nil
		},
	}

	deps := reviewAndApplyDeps{reviewDef: reviewDef, applyDef: applyDef}
	err := runReviewAndApplyOrchestration(context.Background(), t.TempDir(), nil, nil, deps)
	if err != nil {
		t.Fatalf("orchestration returned error: %v", err)
	}

	if !reviewRunIDOK {
		t.Fatalf("ReviewRunIDFromContext: ok=false (apply ctx に reviewRunID が未注入)")
	}
	if reviewRunIDFromCtx == "" {
		t.Fatalf("reviewRunIDFromCtx が空 (Req 8.5 同 runID 紐付け違反)")
	}
	if reviewRunIDFromMetadata == "" {
		t.Fatalf("reviewRunIDFromMetadata が空 (前段 review の Run() が runID 未注入: テスト前提崩れ)")
	}
	if reviewRunIDFromMetadata != reviewRunIDFromCtx {
		t.Errorf("reviewRunIDFromCtx=%q, want %q (前段 review.Run の runID と完全一致)",
			reviewRunIDFromCtx, reviewRunIDFromMetadata)
	}
}
