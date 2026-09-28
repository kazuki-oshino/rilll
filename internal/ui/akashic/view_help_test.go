// Package ui のアカシックモード キー操作ヒント行 (footer hint) に対するテスト。
//
// 既存のメニュー / provider 設定 / workflow 選択 / prompt 各画面と同様に、
// アカシックモードでも各サブビュー (List / Summary / Detail) の末尾に
// 操作キーのヒント行を表示することで、初見ユーザに操作方法を伝える。
//
// 観測内容:
//   - List / Summary / Detail それぞれの View(120) 末尾にサブビュー固有の
//     ヒント行が含まれること
//   - Reader=nil / Loading / Err 等の graceful 経路でも esc/q による退出を
//     伝えるためにヒント行が表示されること
package akashic

import (
	"strings"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

// TestAkashicListViewShowsKeyHint は List サブビューの正常表示で
// 「↑/↓ 選択 · Enter 詳細 · Tab 集計 · esc/q 戻る」相当のヒント行が
// 含まれることを観測する。
func TestAkashicListViewShowsKeyHint(t *testing.T) {
	s := makeAkashicState(&fakeReader{})
	s.List.Items = []akashic.WorkflowRunListItem{
		{RunHeader: akashic.RunHeader{RunID: "run-1", WorkflowID: "wf-1", StartedAt: time.Now()}},
	}
	view := s.View(120)
	for _, want := range []string{"↑/↓", "Enter 詳細", "Tab 集計", "esc/q 戻る"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected list hint containing %q, got:\n%s", want, view)
		}
	}
}

// TestAkashicListViewHintShownOnReaderNil は Reader=nil graceful 経路でも
// 退出キーを伝えるためにヒント行 ("esc/q 戻る") が表示されることを観測する。
func TestAkashicListViewHintShownOnReaderNil(t *testing.T) {
	s := makeAkashicState(nil)
	view := s.View(120)
	if !strings.Contains(view, "esc/q 戻る") {
		t.Fatalf("expected list hint to remain visible on Reader=nil, got:\n%s", view)
	}
}

// TestAkashicSummaryViewShowsKeyHint は Summary サブビューの正常表示で
// 「Tab 一覧 · esc/q 戻る」相当のヒント行が含まれることを観測する。
func TestAkashicSummaryViewShowsKeyHint(t *testing.T) {
	s := makeAkashicSummaryState(&fakeReader{})
	view := s.View(120)
	for _, want := range []string{"Tab 一覧", "esc/q 戻る"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected summary hint containing %q, got:\n%s", want, view)
		}
	}
}

// TestAkashicSummaryViewHintShownOnReaderNil は Summary サブビューの
// Reader=nil graceful 経路でもヒント行が表示されることを観測する。
func TestAkashicSummaryViewHintShownOnReaderNil(t *testing.T) {
	s := makeAkashicSummaryState(nil)
	view := s.View(120)
	if !strings.Contains(view, "esc/q 戻る") {
		t.Fatalf("expected summary hint to remain visible on Reader=nil, got:\n%s", view)
	}
}

// TestAkashicDetailViewShowsKeyHint は Detail サブビューの正常表示で
// 「Tab/Shift+Tab フォーカス · ↑/↓ 移動 · n/p ページ · Enter プレビュー ·
// i 改善 · Backspace 一覧 · esc/q 戻る」相当のヒント行が含まれることを観測する。
func TestAkashicDetailViewShowsKeyHint(t *testing.T) {
	detail := makePopulatedRunDetail()
	s := makeAkashicDetailViewState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Detail = &detail
	view := s.View(120)
	for _, want := range []string{
		"Tab/Shift+Tab",
		"↑/↓",
		"n/p",
		"Enter",
		"i 改善",
		"Backspace 一覧",
		"esc/q 戻る",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected detail hint containing %q, got:\n%s", want, view)
		}
	}
}

// TestAkashicDetailViewHintShownOnReaderNil は Detail サブビューの
// Reader=nil graceful 経路でもヒント行が表示されることを観測する。
func TestAkashicDetailViewHintShownOnReaderNil(t *testing.T) {
	s := makeAkashicDetailViewState(nil, nil)
	view := s.View(120)
	if !strings.Contains(view, "esc/q 戻る") {
		t.Fatalf("expected detail hint to remain visible on Reader=nil, got:\n%s", view)
	}
}
