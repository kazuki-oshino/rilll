// Package ui のアカシックモード Run 一覧サブビューにおけるカーソル移動キー写像
// (`up` / `down` / `j` / `k`) のテスト。
//
// 既存の handleListKey は tab / enter のみを消費しており、上下選択ができない
// 状態だった (akashic_update.go 旧コメント「カーソル写像は本タスクのスコープ外、
// 未実装」)。本テストはその欠落を明示的に観測し、handleListKey に
// カーソル移動が実装されていることを担保する。
package akashic

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

// listStateWithItems は与えた件数の Items を持つ List サブビュー state を返すヘルパ。
// Cursor 初期値はテスト側で必要に応じて上書きする。
func listStateWithItems(n int) State {
	now := time.Now()
	items := make([]akashic.WorkflowRunListItem, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, akashic.WorkflowRunListItem{
			RunHeader: akashic.RunHeader{
				RunID:      "run-" + string(rune('A'+i)),
				WorkflowID: "wf-1",
				StartedAt:  now,
			},
		})
	}
	return State{
		Subview: SubviewList,
		Reader:  &fakeReader{},
		List: ListState{
			Sort:  SortNewest,
			Items: items,
		},
	}
}

// TestAkashicListCursorMovesDownOnArrowDown は ↓ で Cursor が +1 進むことを観測する。
func TestAkashicListCursorMovesDownOnArrowDown(t *testing.T) {
	s := listStateWithItems(3)
	s.List.Cursor = 0

	got, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyDown})
	if got.List.Cursor != 1 {
		t.Fatalf("expected Cursor=1 after Down, got %d", got.List.Cursor)
	}
	if cmd != nil {
		t.Fatalf("expected nil cmd, got %v", cmd)
	}
}

// TestAkashicListCursorMovesDownOnJ は j で Cursor が +1 進むことを観測する
// (vim 流移動キーの一貫性: Detail サブビューと同様)。
func TestAkashicListCursorMovesDownOnJ(t *testing.T) {
	s := listStateWithItems(3)
	s.List.Cursor = 0

	got, _ := s.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if got.List.Cursor != 1 {
		t.Fatalf("expected Cursor=1 after j, got %d", got.List.Cursor)
	}
}

// TestAkashicListCursorMovesUpOnArrowUp は ↑ で Cursor が -1 進むことを観測する。
func TestAkashicListCursorMovesUpOnArrowUp(t *testing.T) {
	s := listStateWithItems(3)
	s.List.Cursor = 2

	got, _ := s.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	if got.List.Cursor != 1 {
		t.Fatalf("expected Cursor=1 after Up, got %d", got.List.Cursor)
	}
}

// TestAkashicListCursorMovesUpOnK は k で Cursor が -1 進むことを観測する。
func TestAkashicListCursorMovesUpOnK(t *testing.T) {
	s := listStateWithItems(3)
	s.List.Cursor = 2

	got, _ := s.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if got.List.Cursor != 1 {
		t.Fatalf("expected Cursor=1 after k, got %d", got.List.Cursor)
	}
}

// TestAkashicListCursorClampsAtEnd は末尾で ↓ を押しても Cursor が範囲外に出ないことを観測する。
func TestAkashicListCursorClampsAtEnd(t *testing.T) {
	s := listStateWithItems(3)
	s.List.Cursor = 2 // 末尾

	got, _ := s.HandleKey(tea.KeyMsg{Type: tea.KeyDown})
	if got.List.Cursor != 2 {
		t.Fatalf("expected Cursor clamped to 2 at end, got %d", got.List.Cursor)
	}
}

// TestAkashicListCursorClampsAtStart は先頭で ↑ を押しても Cursor が負値にならないことを観測する。
func TestAkashicListCursorClampsAtStart(t *testing.T) {
	s := listStateWithItems(3)
	s.List.Cursor = 0 // 先頭

	got, _ := s.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	if got.List.Cursor != 0 {
		t.Fatalf("expected Cursor clamped to 0 at start, got %d", got.List.Cursor)
	}
}

// TestAkashicListCursorEmptyItemsIsNoop は Items 空のときカーソル移動キーが
// silent no-op (Cursor=0 のまま、Cmd nil) になることを観測する。
func TestAkashicListCursorEmptyItemsIsNoop(t *testing.T) {
	s := listStateWithItems(0)
	s.List.Cursor = 0

	got, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyDown})
	if got.List.Cursor != 0 {
		t.Fatalf("expected Cursor=0 on empty list after Down, got %d", got.List.Cursor)
	}
	if cmd != nil {
		t.Fatalf("expected nil cmd on empty list, got %v", cmd)
	}
}

// TestAkashicListCursorNeverReturnsTeaQuit はカーソル移動キーで tea.Quit が
// 一度も返らないことを観測する (Req 11.5 ガード: モードレベルキー以外は退出しない)。
func TestAkashicListCursorNeverReturnsTeaQuit(t *testing.T) {
	s := listStateWithItems(3)
	keys := []tea.KeyMsg{
		{Type: tea.KeyDown},
		{Type: tea.KeyUp},
		{Type: tea.KeyRunes, Runes: []rune{'j'}},
		{Type: tea.KeyRunes, Runes: []rune{'k'}},
	}
	for _, k := range keys {
		_, cmd := s.HandleKey(k)
		if cmd == nil {
			continue
		}
		msg := cmd()
		if _, ok := msg.(tea.QuitMsg); ok {
			t.Fatalf("HandleKey(%v) must not return tea.Quit", k)
		}
	}
}
