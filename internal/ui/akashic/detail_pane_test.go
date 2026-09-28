// Package ui のアカシックモード Detail サブビュー詳細 pane (AgentCall / RawEvent
// inline pane) のテスト。
//
// 背景: Detail 画面は agent_calls / raw_events を 1 行のメタ表示しか提供していない
// が、各 row には 1 行に収まらない情報 (AgentCallRow.Prompt / ResultSummary /
// Error / FailureCategory / ResultPayload、RawEventRow.Payload) が含まれる。
// Req 5 (失敗原因や挙動を外部 SQL クライアントなしで確認できる) を実装でも
// 満たすため、選択行で Enter を押したら inline pane が agent_calls / raw_events
// 一覧の直下にトグル表示される。
//
// 不変条件:
//   - pane は Focus と Cursor によって「現在の選択行」に対する開閉状態を表す
//   - Cursor 移動で自動クローズ (別行で開いたまま残らない)
//   - Focus 切替で自動クローズ (別セクションに移ったらクリア)
//   - Detail 突入直後 (enterDetail) は両 pane=false
package akashic

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

// makePaneTestState は agent_calls / raw_events をいくつか持つ Detail state を組み立てる。
// pane 開閉観測の基底状態として使う。
func makePaneTestState() State {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	detail := akashic.RunDetail{
		Run: akashic.RunHeader{RunID: "run-1", WorkflowID: "wf-1", StartedAt: now},
		AgentCalls: []akashic.AgentCallRow{
			{ID: "ac-1", StepID: "s1", ProviderID: "p", StartedAt: now, Prompt: "prompt 1"},
			{ID: "ac-2", StepID: "s2", ProviderID: "p", StartedAt: now, Prompt: "prompt 2"},
		},
		RawEvents: []akashic.RawEventRow{
			{Sequence: 1, Type: "t", Subtype: "u", Source: "s", ObservedAt: now, Payload: []byte(`{"a":1}`)},
			{Sequence: 2, Type: "t", Subtype: "u", Source: "s", ObservedAt: now, Payload: []byte(`{"a":2}`)},
		},
	}
	return State{
		Subview: SubviewDetail,
		Reader:  &fakeReader{},
		Detail: DetailState{
			RunID:  "run-1",
			Detail: &detail,
			Focus:  DetailFocusAgentCalls,
		},
	}
}

// TestEnterDetailInitializesPanesClosed は enterDetail 直後に両 pane=false を観測する。
func TestEnterDetailInitializesPanesClosed(t *testing.T) {
	s := State{Subview: SubviewList, Reader: &fakeReader{}}
	got, _ := s.enterDetail("run-1")
	if got.Detail.AgentCallPaneOpen {
		t.Fatalf("expected AgentCallPaneOpen=false on enter, got true")
	}
	if got.Detail.RawEventPaneOpen {
		t.Fatalf("expected RawEventPaneOpen=false on enter, got true")
	}
}

// TestEnterTogglesAgentCallPaneWhenFocusAgentCalls は Focus=AgentCalls 中に Enter で
// AgentCallPaneOpen がトグルし Cmd は nil (副作用なし) を観測する。
func TestEnterTogglesAgentCallPaneWhenFocusAgentCalls(t *testing.T) {
	s := makePaneTestState()
	s.Detail.Focus = DetailFocusAgentCalls

	got, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !got.Detail.AgentCallPaneOpen {
		t.Fatalf("expected AgentCallPaneOpen=true after first Enter, got false")
	}
	if cmd != nil {
		t.Fatalf("expected nil Cmd on agent_call pane toggle, got %T", cmd)
	}

	got2, _ := got.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if got2.Detail.AgentCallPaneOpen {
		t.Fatalf("expected AgentCallPaneOpen=false after second Enter (toggle), got true")
	}
}

// TestEnterTogglesRawEventPaneWhenFocusRawEvents は Focus=RawEvents 中に Enter で
// RawEventPaneOpen がトグルすることを観測する。
func TestEnterTogglesRawEventPaneWhenFocusRawEvents(t *testing.T) {
	s := makePaneTestState()
	s.Detail.Focus = DetailFocusRawEvents

	got, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !got.Detail.RawEventPaneOpen {
		t.Fatalf("expected RawEventPaneOpen=true after Enter, got false")
	}
	if cmd != nil {
		t.Fatalf("expected nil Cmd on raw_event pane toggle, got %T", cmd)
	}
}

// TestEnterOnArtifactsStillTriggersPreview は Focus=Artifacts での Enter が既存の
// requestArtifactPreview 経路 (= ReadArtifactCmd 発行 / Artifact.Loading=true) を
// そのまま使うことを観測する (pane 機能で artifacts 経路を壊さないガード)。
func TestEnterOnArtifactsStillTriggersPreview(t *testing.T) {
	s := makePaneTestState()
	s.Detail.Focus = DetailFocusArtifacts
	s.ArtifactReader = &fakeArtifactReader{}
	s.Detail.Artifact.Rows = []akashic.ArtifactRow{
		{Path: "a.txt", Kind: "file", SizeBytes: 1, MIME: "text/plain"},
	}
	s.Detail.Artifact.SelectedIndex = 0

	got, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !got.Detail.Artifact.Loading {
		t.Fatalf("expected Artifact.Loading=true after Enter on Focus=Artifacts, got false")
	}
	if cmd == nil {
		t.Fatalf("expected non-nil Cmd (ReadArtifactCmd) on Focus=Artifacts Enter")
	}
}

// TestCursorMoveClosesAgentCallPane は AgentCallPaneOpen=true のとき j (cursor 移動)
// で pane が自動的に閉じることを観測する。
func TestCursorMoveClosesAgentCallPane(t *testing.T) {
	s := makePaneTestState()
	s.Detail.Focus = DetailFocusAgentCalls
	s.Detail.AgentCallPaneOpen = true

	got, _ := s.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if got.Detail.AgentCallPaneOpen {
		t.Fatalf("expected AgentCallPaneOpen=false after cursor move, got true")
	}
}

// TestCursorMoveClosesRawEventPane は RawEventPaneOpen=true のとき k で pane が
// 自動的に閉じることを観測する。
func TestCursorMoveClosesRawEventPane(t *testing.T) {
	s := makePaneTestState()
	s.Detail.Focus = DetailFocusRawEvents
	s.Detail.RawEventsCursor = 1
	s.Detail.RawEventPaneOpen = true

	got, _ := s.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if got.Detail.RawEventPaneOpen {
		t.Fatalf("expected RawEventPaneOpen=false after cursor move, got true")
	}
}

// TestFocusSwitchClosesPanes は Tab で Focus を切替えると両 pane が閉じることを観測する。
func TestFocusSwitchClosesPanes(t *testing.T) {
	s := makePaneTestState()
	s.Detail.Focus = DetailFocusAgentCalls
	s.Detail.AgentCallPaneOpen = true
	s.Detail.RawEventPaneOpen = true

	got, _ := s.HandleKey(tea.KeyMsg{Type: tea.KeyTab})
	if got.Detail.AgentCallPaneOpen {
		t.Fatalf("expected AgentCallPaneOpen=false after Tab, got true")
	}
	if got.Detail.RawEventPaneOpen {
		t.Fatalf("expected RawEventPaneOpen=false after Tab, got true")
	}
}

// TestShiftTabClosesPanes は Shift+Tab でも両 pane が閉じることを観測する。
func TestShiftTabClosesPanes(t *testing.T) {
	s := makePaneTestState()
	s.Detail.Focus = DetailFocusAgentCalls
	s.Detail.AgentCallPaneOpen = true

	got, _ := s.HandleKey(tea.KeyMsg{Type: tea.KeyShiftTab})
	if got.Detail.AgentCallPaneOpen {
		t.Fatalf("expected AgentCallPaneOpen=false after Shift+Tab, got true")
	}
}

// TestEnterReturnsNoTeaQuit は pane トグル経路で tea.Quit を返さないことを観測する
// (Req 11.5 ガード継続)。
func TestEnterReturnsNoTeaQuit(t *testing.T) {
	s := makePaneTestState()
	s.Detail.Focus = DetailFocusAgentCalls

	_, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		return
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); ok {
		t.Fatalf("HandleKey(Enter) on Focus=AgentCalls must not return tea.Quit")
	}
}
