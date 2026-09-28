// Package ui のアカシックモード Detail サブビュー用 list window クリップ関数
// (clipListWindow) のテスト。
//
// 背景: Detail 画面は agent_calls / agent_context_usage / raw_events / artifacts を
// 縦に並べて全件表示しているため、件数が多いと WithAltScreen の固定キャンバスから
// Cursor 行が押し出されて見えなくなる UX 問題があった。本ヘルパは Cursor を可能な
// 限り window 中央に配置し、両端で端基準にクランプすることで「Cursor が画面に映る」
// ことを構造的に保証する純関数として導入される。
package akashic

import "testing"

// TestClipListWindowEmpty は total=0 のとき (0, 0) を返すことを観測する。
func TestClipListWindowEmpty(t *testing.T) {
	start, end := clipListWindow(0, 0, 11)
	if start != 0 || end != 0 {
		t.Fatalf("expected (0, 0) for empty list, got (%d, %d)", start, end)
	}
}

// TestClipListWindowSmallerThanWindow は total<=window のとき [0, total) を
// 返す (全件そのまま表示) ことを観測する。
func TestClipListWindowSmallerThanWindow(t *testing.T) {
	start, end := clipListWindow(2, 5, 11)
	if start != 0 || end != 5 {
		t.Fatalf("expected (0, 5) for total<=window, got (%d, %d)", start, end)
	}
}

// TestClipListWindowZeroOrNegativeWindow は window<=0 のとき [0, total) を
// 返す (graceful: 異常値で表示が消えない) ことを観測する。
func TestClipListWindowZeroOrNegativeWindow(t *testing.T) {
	start, end := clipListWindow(2, 5, 0)
	if start != 0 || end != 5 {
		t.Fatalf("expected (0, 5) for window=0, got (%d, %d)", start, end)
	}
	start, end = clipListWindow(2, 5, -3)
	if start != 0 || end != 5 {
		t.Fatalf("expected (0, 5) for negative window, got (%d, %d)", start, end)
	}
}

// TestClipListWindowCursorAtLeftEdge は cursor が左端寄り (cursor < window/2) の
// とき [0, window) を返す (端基準) ことを観測する。
func TestClipListWindowCursorAtLeftEdge(t *testing.T) {
	// total=20, window=11, cursor=0 → start=0, end=11
	start, end := clipListWindow(0, 20, 11)
	if start != 0 || end != 11 {
		t.Fatalf("expected (0, 11) for cursor at left edge, got (%d, %d)", start, end)
	}
	// total=20, window=11, cursor=4 (まだ左端、4 < 11/2=5) → start=0, end=11
	start, end = clipListWindow(4, 20, 11)
	if start != 0 || end != 11 {
		t.Fatalf("expected (0, 11) for cursor=4, got (%d, %d)", start, end)
	}
}

// TestClipListWindowCursorCentered は cursor が中央付近のとき cursor を中心と
// する window を返すことを観測する。
func TestClipListWindowCursorCentered(t *testing.T) {
	// total=20, window=11, cursor=10 → half=5, start=5, end=16 (cursor が中央)
	start, end := clipListWindow(10, 20, 11)
	if start != 5 || end != 16 {
		t.Fatalf("expected (5, 16) for cursor centered, got (%d, %d)", start, end)
	}
}

// TestClipListWindowCursorAtRightEdge は cursor が右端寄りのとき [total-window,
// total) を返す (端基準) ことを観測する。
func TestClipListWindowCursorAtRightEdge(t *testing.T) {
	// total=20, window=11, cursor=15 → start=15-5=10 だが end=21 が total 超過
	// → end=20, start=20-11=9
	start, end := clipListWindow(15, 20, 11)
	if start != 9 || end != 20 {
		t.Fatalf("expected (9, 20) for cursor near right edge, got (%d, %d)", start, end)
	}
	// total=20, window=11, cursor=19 (末尾) → 同上
	start, end = clipListWindow(19, 20, 11)
	if start != 9 || end != 20 {
		t.Fatalf("expected (9, 20) for cursor at right end, got (%d, %d)", start, end)
	}
}

// TestClipListWindowCursorAlwaysVisible はどんな cursor 値でも返り値の範囲に
// cursor が含まれることを観測する (本ヘルパの「Cursor が必ず映る」不変条件)。
func TestClipListWindowCursorAlwaysVisible(t *testing.T) {
	cases := []struct {
		cursor, total, window int
	}{
		{0, 100, 11}, {5, 100, 11}, {50, 100, 11}, {99, 100, 11},
		{0, 12, 11}, {6, 12, 11}, {11, 12, 11},
	}
	for _, c := range cases {
		start, end := clipListWindow(c.cursor, c.total, c.window)
		if c.cursor < start || c.cursor >= end {
			t.Fatalf("cursor=%d not in [%d,%d) for total=%d window=%d",
				c.cursor, start, end, c.total, c.window)
		}
	}
}

// TestClipListWindowReturnsValidRange は 0<=start<=end<=total という不変条件を
// 観測する (View 側で items[start:end] が安全に slice できる)。
func TestClipListWindowReturnsValidRange(t *testing.T) {
	cases := []struct {
		cursor, total, window int
	}{
		{0, 0, 11}, {0, 1, 11}, {5, 100, 11}, {99, 100, 11}, {0, 100, 0},
	}
	for _, c := range cases {
		start, end := clipListWindow(c.cursor, c.total, c.window)
		if start < 0 || start > end || end > c.total {
			t.Fatalf("invalid range (%d, %d) for total=%d", start, end, c.total)
		}
	}
}
