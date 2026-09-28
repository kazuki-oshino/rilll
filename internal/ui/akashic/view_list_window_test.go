// Package ui のアカシックモード Run 一覧サブビュー (renderList) における
// list window クリップ表示のテスト (#11 修正対応)。
//
// 背景: WithAltScreen の固定キャンバスで model.View が末尾優先で clipFromTop
// するため、newest-first で並ぶ Items 全件をそのまま描画すると先頭 (= 新しい側)
// の行が画面外に押し出されて視認できなくなる UX 問題があった。本テストは
// renderList が Detail 側と同じ clipListWindow パターンで
//   - 表示行数を listWindow 件以下に抑える
//   - Cursor が必ず表示範囲に含まれる
//   - 窓外の存在は末尾に "showing X-Y / total" で告知される
//   - 既存の "page offset:" footer は窓フッタの後にそのまま維持される
//
// ことを観測する。
package akashic

import (
	"strings"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

// makeManyRunListItems は n 件の WorkflowRunListItem を newest-first で組み立てる
// (List window クリップ観測用)。RunID は "run-AAA0" 形式で連番付与する。
func makeManyRunListItems(n int) []akashic.WorkflowRunListItem {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	items := make([]akashic.WorkflowRunListItem, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, akashic.WorkflowRunListItem{
			RunHeader: akashic.RunHeader{
				RunID:      "run-" + idxToken(i),
				WorkflowID: "wf-1",
				StartedAt:  now.Add(-time.Duration(i) * time.Second),
			},
		})
	}
	return items
}

// TestListClipsToWindow は Items が listWindow 件を超えるとき renderList の
// 出力行数が listWindow 件以下に抑えられることを観測する (#11)。
func TestListClipsToWindow(t *testing.T) {
	items := makeManyRunListItems(50)
	s := makeAkashicState(&fakeReader{})
	s.List.Items = items
	s.List.Cursor = 0

	view := s.View(120)
	rows := countLinesContaining(view, "run-")
	if rows > listWindow {
		t.Fatalf("expected at most %d run rows, got %d\n%s", listWindow, rows, view)
	}
	if rows == 0 {
		t.Fatalf("expected at least 1 run row, got 0\n%s", view)
	}
}

// TestListKeepsCursorAtTopVisible は既定 Cursor=0 (= newest-first の先頭 = 新しい側)
// のとき、その行が表示窓に必ず含まれることを観測する (#11 の本丸)。
//
// 修正前は全件描画 → clipFromTop で先頭が捨てられ「新しい側が見えない」状態だった。
// 修正後は窓表示で先頭から listWindow 件が描画されるため、Cursor=0 行は確実に映る。
func TestListKeepsCursorAtTopVisible(t *testing.T) {
	items := makeManyRunListItems(50)
	cursorID := items[0].RunID
	s := makeAkashicState(&fakeReader{})
	s.List.Items = items
	s.List.Cursor = 0

	view := s.View(120)
	if !strings.Contains(view, cursorID) {
		t.Fatalf("expected cursor (top/newest) row %q to be visible, got:\n%s", cursorID, view)
	}
}

// TestListKeepsCursorAtMiddleVisible は Cursor を中央付近 (25) に置いたとき、
// 該当行が表示窓に必ず含まれることを観測する (clipListWindow の中央配置)。
func TestListKeepsCursorAtMiddleVisible(t *testing.T) {
	items := makeManyRunListItems(50)
	cursor := 25
	cursorID := items[cursor].RunID
	s := makeAkashicState(&fakeReader{})
	s.List.Items = items
	s.List.Cursor = cursor

	view := s.View(120)
	if !strings.Contains(view, cursorID) {
		t.Fatalf("expected cursor (middle) row %q to be visible, got:\n%s", cursorID, view)
	}
}

// TestListKeepsCursorAtBottomVisible は Cursor を末尾 (49) に置いたとき、
// 該当行が表示窓に必ず含まれることを観測する (clipListWindow の右端基準)。
func TestListKeepsCursorAtBottomVisible(t *testing.T) {
	items := makeManyRunListItems(50)
	cursor := len(items) - 1
	cursorID := items[cursor].RunID
	s := makeAkashicState(&fakeReader{})
	s.List.Items = items
	s.List.Cursor = cursor

	view := s.View(120)
	if !strings.Contains(view, cursorID) {
		t.Fatalf("expected cursor (bottom/oldest) row %q to be visible, got:\n%s", cursorID, view)
	}
}

// TestListShowsWindowFooter は窓外がある場合 "showing X-Y / total" が末尾に
// 出ることを観測する (Detail 側と同じパターン)。
func TestListShowsWindowFooter(t *testing.T) {
	items := makeManyRunListItems(50)
	s := makeAkashicState(&fakeReader{})
	s.List.Items = items
	s.List.Cursor = 0

	view := s.View(120)
	if !strings.Contains(view, "/ 50") {
		t.Fatalf("expected window footer with `/ 50` for total=50, got:\n%s", view)
	}
}

// TestListNoWindowFooterWhenAllVisible は Items 件数 <= listWindow のときは
// 窓フッタを出さないことを観測する (全件表示時はノイズになるため)。
func TestListNoWindowFooterWhenAllVisible(t *testing.T) {
	items := makeManyRunListItems(3)
	s := makeAkashicState(&fakeReader{})
	s.List.Items = items
	s.List.Cursor = 0

	view := s.View(120)
	if strings.Contains(view, "showing ") {
		t.Fatalf("did not expect window footer when all items fit, got:\n%s", view)
	}
	// 既存の page offset footer は維持される (renderListPagingFooter)
	if !strings.Contains(view, "page offset:") {
		t.Fatalf("expected paging footer 'page offset:' to remain, got:\n%s", view)
	}
}

// TestListWindowFooterPrecedesPagingFooter は窓フッタと page offset footer が
// 共存し、窓フッタが先 / page offset footer が後 の順で出ることを観測する。
//
// 順序を観測するのは、ユーザの視線移動として「現在の窓位置 (showing) → 全体ページ
// (page offset)」と段階的に範囲が広がる読み順を維持するため。
func TestListWindowFooterPrecedesPagingFooter(t *testing.T) {
	items := makeManyRunListItems(50)
	s := makeAkashicState(&fakeReader{})
	s.List.Items = items
	s.List.Cursor = 0

	view := s.View(120)
	showingIdx := strings.Index(view, "showing ")
	pagingIdx := strings.Index(view, "page offset:")
	if showingIdx < 0 {
		t.Fatalf("expected window footer 'showing ' in view, got:\n%s", view)
	}
	if pagingIdx < 0 {
		t.Fatalf("expected paging footer 'page offset:' in view, got:\n%s", view)
	}
	if showingIdx > pagingIdx {
		t.Fatalf("expected 'showing' to come before 'page offset:', got showing=%d paging=%d\n%s",
			showingIdx, pagingIdx, view)
	}
}
