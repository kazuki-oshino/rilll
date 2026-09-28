// Package ui のアカシックモード Detail サブビュー Focus 設計 (Header を循環外にする)
// に対するテスト。
//
// 背景: 旧実装では初期 Focus=DetailFocusHeader、Tab 循環は
// Header → AgentCalls → RawEvents → Artifacts → Header だった。Header は
// workflow_run の見出し 1 行のみで navigation 対象がなく、moveDetailCursor も
// Header に対して no-op のため、詳細画面に入った直後ユーザが j/k/Enter を押しても
// 完全に反応しなかった。Tab を 1 回押すまで利用キーが「死んでいる」状態。
//
// 新仕様:
//   - 初期 Focus = DetailFocusAgentCalls (Detail に入ったらすぐ j/k/Enter が効く)
//   - Tab 循環 = AgentCalls → RawEvents → Artifacts → AgentCalls (3 セクション)
//   - Header は Focus 値としては残るが Tab 循環には含まれない (graceful fallback 用)
package akashic

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestAkashicEnterDetailDefaultFocusIsAgentCalls は enterDetail 時の初期 Focus が
// DetailFocusAgentCalls であることを観測する。
func TestAkashicEnterDetailDefaultFocusIsAgentCalls(t *testing.T) {
	s := State{Subview: SubviewList, Reader: &fakeReader{}}
	got, _ := s.enterDetail("run-1")
	if got.Detail.Focus != DetailFocusAgentCalls {
		t.Fatalf("expected initial Focus=AgentCalls on enterDetail, got %q", got.Detail.Focus)
	}
}

// TestAkashicDetailTabCycleSkipsHeader は Tab 循環が
// AgentCalls → RawEvents → Artifacts → AgentCalls (3 サイクル) であり
// Header を含まないことを観測する。
func TestAkashicDetailTabCycleSkipsHeader(t *testing.T) {
	s := makeAkashicDetailState(&fakeReader{})
	s.Detail.Focus = DetailFocusAgentCalls

	wantOrder := []DetailFocus{
		DetailFocusRawEvents,
		DetailFocusArtifacts,
		DetailFocusAgentCalls, // 1 周
	}
	for i, want := range wantOrder {
		var cmd tea.Cmd
		s, cmd = s.HandleKey(tea.KeyMsg{Type: tea.KeyTab})
		if cmd != nil {
			t.Fatalf("step %d: expected nil Cmd from Tab, got %T", i, cmd)
		}
		if s.Detail.Focus != want {
			t.Fatalf("step %d: expected Focus=%q after Tab, got %q", i, want, s.Detail.Focus)
		}
		if s.Detail.Focus == DetailFocusHeader {
			t.Fatalf("step %d: Focus must never be Header in Tab cycle, got %q", i, s.Detail.Focus)
		}
	}
}

// TestAkashicDetailShiftTabCycleSkipsHeader は Shift+Tab 逆循環でも
// Header を含まないことを観測する。
func TestAkashicDetailShiftTabCycleSkipsHeader(t *testing.T) {
	s := makeAkashicDetailState(&fakeReader{})
	s.Detail.Focus = DetailFocusAgentCalls

	wantOrder := []DetailFocus{
		DetailFocusArtifacts,
		DetailFocusRawEvents,
		DetailFocusAgentCalls, // 1 周
	}
	for i, want := range wantOrder {
		var cmd tea.Cmd
		s, cmd = s.HandleKey(tea.KeyMsg{Type: tea.KeyShiftTab})
		if cmd != nil {
			t.Fatalf("step %d: expected nil Cmd from Shift+Tab, got %T", i, cmd)
		}
		if s.Detail.Focus != want {
			t.Fatalf("step %d: expected Focus=%q after Shift+Tab, got %q", i, want, s.Detail.Focus)
		}
		if s.Detail.Focus == DetailFocusHeader {
			t.Fatalf("step %d: Focus must never be Header in Shift+Tab cycle, got %q", i, s.Detail.Focus)
		}
	}
}
