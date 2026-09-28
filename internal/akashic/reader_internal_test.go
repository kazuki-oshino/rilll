package akashic

import (
	"database/sql"
	"testing"
	"time"
)

// reader_internal_test.go は reader_internal.go の package private ヘルパに対する
// 最小限の TDD smoke test を提供する。網羅的な table-driven test はタスク 4.5 で
// reader_test.go に追加する想定。
//
// 役割分担:
//   - 本ファイル: タスク 1.3 の観測可能な完了条件 (verification example) を
//     実コードで担保する RED → GREEN サイクル。
//   - tasks 4.5 (reader_test.go): 数字マスキング / UUID マスキング / パスマスキング /
//     連続空白圧縮 / 80 文字切詰めの全パターンを table-driven で検証する。

// TestPercentile_EmptyAndSingle は percentile の境界条件を検証する (task 1.3)。
func TestPercentile_EmptyAndSingle(t *testing.T) {
	t.Run("empty slice returns nil", func(t *testing.T) {
		got := percentile(nil, 0.5)
		if got != nil {
			t.Fatalf("expected nil for empty slice, got %v", *got)
		}
	})
	t.Run("single element returns that value", func(t *testing.T) {
		got := percentile([]int64{42}, 0.95)
		if got == nil {
			t.Fatalf("expected non-nil for single element")
		}
		if *got != 42 {
			t.Errorf("expected 42, got %d", *got)
		}
	})
}

// TestPercentile_LinearInterp は複数要素の場合の index 計算を検証する。
// ソート済みを前提とするため、入力は昇順で渡す。
func TestPercentile_LinearInterp(t *testing.T) {
	sorted := []int64{10, 20, 30, 40, 50}

	t.Run("p50 of [10,20,30,40,50] is 30", func(t *testing.T) {
		got := percentile(sorted, 0.5)
		if got == nil || *got != 30 {
			t.Fatalf("expected 30, got %v", got)
		}
	})
	t.Run("p0 returns first element", func(t *testing.T) {
		got := percentile(sorted, 0.0)
		if got == nil || *got != 10 {
			t.Fatalf("expected 10, got %v", got)
		}
	})
	t.Run("p100 returns last element", func(t *testing.T) {
		got := percentile(sorted, 1.0)
		if got == nil || *got != 50 {
			t.Fatalf("expected 50, got %v", got)
		}
	})
}

// TestNormalizeError_VerificationExample はタスク 1.3 の観測可能な完了条件
// (verification example) を担保する。スペック上の正規化ヒューリスティック
// (パス → <PATH> / UUID → <UUID>) が実例で機能していることを確認する。
func TestNormalizeError_VerificationExample(t *testing.T) {
	in := "Connection refused at /tmp/sock-1234 with id 550e8400-e29b-41d4-a716-446655440000"
	want := "Connection refused at <PATH> with id <UUID>"
	got := normalizeError(in)
	if got != want {
		t.Errorf("normalizeError mismatch\n input: %q\n  want: %q\n   got: %q", in, want, got)
	}
}

// TestNormalizeError_Empty は空入力で空出力になることを確認する (空白圧縮や
// マスキングで panic しないことの最低保証)。
func TestNormalizeError_Empty(t *testing.T) {
	if got := normalizeError(""); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

// TestNullPtrHelpers は scan ヘルパが Valid=false で nil / Valid=true で値を
// 返すことを確認する (task 1.3 の scan 共通ヘルパ要件)。
func TestNullPtrHelpers(t *testing.T) {
	t.Run("nullBoolPtr invalid -> nil", func(t *testing.T) {
		if got := nullBoolPtr(sql.NullBool{}); got != nil {
			t.Errorf("expected nil, got %v", *got)
		}
	})
	t.Run("nullBoolPtr valid -> *bool", func(t *testing.T) {
		got := nullBoolPtr(sql.NullBool{Bool: true, Valid: true})
		if got == nil || *got != true {
			t.Errorf("expected *bool=true, got %v", got)
		}
	})

	t.Run("nullInt64Ptr invalid -> nil", func(t *testing.T) {
		if got := nullInt64Ptr(sql.NullInt64{}); got != nil {
			t.Errorf("expected nil, got %v", *got)
		}
	})
	t.Run("nullInt64Ptr valid -> *int64", func(t *testing.T) {
		got := nullInt64Ptr(sql.NullInt64{Int64: 7, Valid: true})
		if got == nil || *got != 7 {
			t.Errorf("expected *int64=7, got %v", got)
		}
	})

	t.Run("nullFloat64Ptr invalid -> nil", func(t *testing.T) {
		if got := nullFloat64Ptr(sql.NullFloat64{}); got != nil {
			t.Errorf("expected nil, got %v", *got)
		}
	})
	t.Run("nullFloat64Ptr valid -> *float64", func(t *testing.T) {
		got := nullFloat64Ptr(sql.NullFloat64{Float64: 1.5, Valid: true})
		if got == nil || *got != 1.5 {
			t.Errorf("expected *float64=1.5, got %v", got)
		}
	})

	t.Run("nullStringValue invalid -> empty", func(t *testing.T) {
		if got := nullStringValue(sql.NullString{}); got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})
	t.Run("nullStringValue valid -> string", func(t *testing.T) {
		got := nullStringValue(sql.NullString{String: "hi", Valid: true})
		if got != "hi" {
			t.Errorf("expected hi, got %q", got)
		}
	})

	t.Run("nullTimePtr invalid -> nil", func(t *testing.T) {
		if got := nullTimePtr(sql.NullTime{}); got != nil {
			t.Errorf("expected nil, got %v", *got)
		}
	})
	t.Run("nullTimePtr valid -> *time.Time", func(t *testing.T) {
		when := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)
		got := nullTimePtr(sql.NullTime{Time: when, Valid: true})
		if got == nil || !got.Equal(when) {
			t.Errorf("expected %v, got %v", when, got)
		}
	})
}
