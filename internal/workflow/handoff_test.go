package workflow

import (
	"context"
	"reflect"
	"testing"
)

// HandoffFromContext は ctx に key 未注入のとき (HandoffParams{}, false) を返す
// (Task 1.2 観測条件: 未注入時 (zero, false))。
func TestHandoffFromContextReturnsZeroWhenMissing(t *testing.T) {
	ctx := context.Background()

	params, ok := HandoffFromContext(ctx)

	if ok {
		t.Fatalf("expected ok=false for missing handoff key, got params=%+v ok=true", params)
	}
	zero := HandoffParams{}
	if !reflect.DeepEqual(params, zero) {
		t.Fatalf("expected zero HandoffParams for missing key, got %+v", params)
	}
}

// ContextWithHandoff で注入した HandoffParams を HandoffFromContext で取り出せる
// (Task 1.2 観測条件: 注入 → 取り出し)。
func TestHandoffFromContextRoundTripAfterInjection(t *testing.T) {
	want := HandoffParams{
		TargetWorkflowID: "workflow-self-improve",
		RunIDs:           []string{"run-1"},
	}
	ctx := ContextWithHandoff(context.Background(), want)

	got, ok := HandoffFromContext(ctx)

	if !ok {
		t.Fatalf("expected ok=true after injection, got params=%+v ok=false", got)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %+v after round-trip, got %+v", want, got)
	}
}

// 子 ctx (context.WithValue で追加 wrap した後) でも HandoffFromContext が同値を返す
// (Task 1.2 観測条件: 子 ctx 経由のラウンドトリップ)。
func TestHandoffFromContextSurvivesChildContext(t *testing.T) {
	want := HandoffParams{
		TargetWorkflowID: "workflow-self-improve",
		RunIDs:           []string{"run-a", "run-b"},
	}
	parent := ContextWithHandoff(context.Background(), want)

	// 別の private key を持つ context.WithValue で wrap した「子 ctx」を作り、
	// それでも HandoffParams が取り出せることを確認する。
	type unrelatedKey struct{}
	child := context.WithValue(parent, unrelatedKey{}, "noise")

	got, ok := HandoffFromContext(child)

	if !ok {
		t.Fatalf("expected ok=true through child ctx, got params=%+v ok=false", got)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %+v through child ctx, got %+v", want, got)
	}
}
