// Package learnings の本 file は parent_run_id を後段 TailDef Fn に伝達する ctx
// 注入/抽出 helper (`ContextWithParentRunID` / `ParentRunIDFromContext`) の契約を
// 検証する単体テスト群を保持する (tasks.md 6.4 / Requirement 1.2, 3.1)。
//
// 検証観点:
//
//   - 未注入 ctx に対する `ParentRunIDFromContext` は空文字列 `""` を返す
//     (PreFetch phase が「parent_run_id 未設定」を検知する観測点)。
//   - `ContextWithParentRunID` で注入した値が `ParentRunIDFromContext` で
//     **同値** で抽出できる (round-trip 保証)。
//   - 空文字列を `ContextWithParentRunID` に渡したときも `ParentRunIDFromContext` は
//     空文字列を返す (注入 skip 契約: 未注入と区別不能であること)。
//   - 注入後にさらに無関係な `context.WithValue` で wrap した子 ctx でも
//     注入値を取り出せる (ctx チェーン透過性)。
//
// 本 spec は manual mode 起動経路を持たない (Hidden=true tail Definition 専用、
// Requirement 1.1) ため、v1 distiller のような `(string, bool)` シグネチャは採用せず、
// 単一 `string` の戻り値で「未設定 = 空文字列」を表現する単純化を行う。
package learnings

import (
	"context"
	"testing"
)

// TestParentRunIDFromContextMissing は ctx に parentRunID が注入されていない場合に
// `ParentRunIDFromContext` が空文字列を返すことを検証する。
//
// この契約は PreFetch phase が「親 run ID が ctx に注入されていない」状態を
// 検知して early return / `verdict=applied-no-op` 経路に進むための前提となる。
func TestParentRunIDFromContextMissing(t *testing.T) {
	ctx := context.Background()

	got := ParentRunIDFromContext(ctx)
	if got != "" {
		t.Errorf("ParentRunIDFromContext(empty ctx) = %q, want %q", got, "")
	}
}

// TestParentRunIDContextRoundTrip は `ContextWithParentRunID` で注入した parentRunID が
// `ParentRunIDFromContext` で同値で抽出できる round-trip 保証を検証する。
//
// design.md §Tail Mode Activation の不変式:
//   - `ContextWithParentRunID` は新しい ctx を返す (元 ctx は不変)。
//   - 抽出値は注入値と完全一致する (string identity)。
func TestParentRunIDContextRoundTrip(t *testing.T) {
	const parentRunID = "20260508T010203Z-parent01"

	ctx := ContextWithParentRunID(context.Background(), parentRunID)

	got := ParentRunIDFromContext(ctx)
	if got != parentRunID {
		t.Errorf("ParentRunIDFromContext(injected ctx) = %q, want %q", got, parentRunID)
	}
}

// TestParentRunIDContextEmptyInjection は空文字列を `ContextWithParentRunID` に渡した
// 場合、`ParentRunIDFromContext` が空文字列を返すことを検証する。
//
// 設計契約:
//   - parentRunID == "" のときは ctx 注入を skip し、未注入と観測上同等にする。
//   - これにより「空文字列で注入された」状態と「未注入」状態は区別できなくなり、
//     PreFetch phase は「ctx 注入なし == precondition 失敗」と一貫した判定ができる。
func TestParentRunIDContextEmptyInjection(t *testing.T) {
	ctx := ContextWithParentRunID(context.Background(), "")

	got := ParentRunIDFromContext(ctx)
	if got != "" {
		t.Errorf("ParentRunIDFromContext after empty injection = %q, want %q", got, "")
	}
}

// TestParentRunIDContextChildLookup は `ContextWithParentRunID` で注入した後に
// 無関係な `context.WithValue` で更に wrap した子 ctx からも注入値を取り出せる
// (ctx チェーン透過性) を検証する。
//
// この契約は SequentialWithLearningsTail で後段 TailDef Fn を呼び出す経路において、
// `ctx` が他の middleware (timeout / logging 等) で再 wrap されても親 RunID 受領が
// 機能することを保証する。
func TestParentRunIDContextChildLookup(t *testing.T) {
	const parentRunID = "20260508T010203Z-parent02"

	type unrelatedKey struct{}

	parent := ContextWithParentRunID(context.Background(), parentRunID)
	child := context.WithValue(parent, unrelatedKey{}, "noise")

	got := ParentRunIDFromContext(child)
	if got != parentRunID {
		t.Errorf("ParentRunIDFromContext(child ctx) = %q, want %q", got, parentRunID)
	}
}

func TestEarlyAggregationEnabledFromContextMissing(t *testing.T) {
	ctx := context.Background()

	if EarlyAggregationEnabledFromContext(ctx) {
		t.Fatalf("expected missing early aggregation marker to be false")
	}
}

func TestEarlyAggregationContextRoundTripThroughChildContext(t *testing.T) {
	type unrelatedKey struct{}

	parent := ContextWithEarlyAggregationEnabled(context.Background())
	child := context.WithValue(parent, unrelatedKey{}, "noise")

	if !EarlyAggregationEnabledFromContext(child) {
		t.Fatalf("expected child context to inherit early aggregation marker")
	}
}

func TestEarlyAggregationContextCoexistsWithParentRunID(t *testing.T) {
	const parentRunID = "20260524T120000Z-parent-early"

	ctx := ContextWithParentRunID(context.Background(), parentRunID)
	ctx = ContextWithEarlyAggregationEnabled(ctx)

	if got := ParentRunIDFromContext(ctx); got != parentRunID {
		t.Fatalf("ParentRunIDFromContext(ctx) = %q, want %q", got, parentRunID)
	}
	if !EarlyAggregationEnabledFromContext(ctx) {
		t.Fatalf("expected early aggregation marker to coexist with parent run handoff")
	}
}

func TestEarlyAggregationContextDoesNotCollideWithParentRunIDKey(t *testing.T) {
	const parentRunID = "20260524T120000Z-parent-only"

	parentOnly := ContextWithParentRunID(context.Background(), parentRunID)
	earlyOnly := ContextWithEarlyAggregationEnabled(context.Background())

	if EarlyAggregationEnabledFromContext(parentOnly) {
		t.Fatalf("parent run handoff must not imply early aggregation marker")
	}
	if got := ParentRunIDFromContext(earlyOnly); got != "" {
		t.Fatalf("early aggregation marker must not imply parent run ID, got %q", got)
	}
}
