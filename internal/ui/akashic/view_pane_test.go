// Package ui のアカシックモード Detail サブビュー詳細 pane (View) のテスト。
//
// pane state (AgentCallPaneOpen / RawEventPaneOpen) が true のとき、agent_calls
// 一覧の直下 / raw_events 一覧の直下にそれぞれ選択行の inline 詳細が表示される
// ことを観測する。pane が閉じているとき (= state false) は表示されない。
package akashic

import (
	"strings"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

// makePaneViewState は agent_calls / raw_events を持つ Detail state を組み立てる。
// pane 表示テストの基底 state として使う。
func makePaneViewState() State {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	successFalse := false
	cost := 0.0123
	dur := int64(1234)
	inTok := int64(100)
	outTok := int64(50)
	detail := akashic.RunDetail{
		Run: akashic.RunHeader{RunID: "run-1", WorkflowID: "wf-1", StartedAt: now},
		AgentCalls: []akashic.AgentCallRow{
			{
				ID:              "ac-1",
				StepID:          "plan",
				ProviderID:      "claude",
				Prompt:          "AC1 PROMPT TEXT",
				ResultSummary:   "AC1 SUMMARY",
				Error:           "AC1 ERROR DETAIL",
				FailureCategory: "timeout",
				DurationMS:      &dur,
				TotalCostUSD:    &cost,
				InputTokens:     &inTok,
				OutputTokens:    &outTok,
				Success:         &successFalse,
				StartedAt:       now,
				ResultPayload:   []byte(`{"ac1":"payload"}`),
			},
			{
				ID:         "ac-2",
				StepID:     "exec",
				ProviderID: "claude",
				Prompt:     "AC2 PROMPT",
				StartedAt:  now,
			},
		},
		RawEvents: []akashic.RawEventRow{
			{
				Sequence:   1,
				Type:       "tool_use",
				Subtype:    "shell",
				Source:     "agent",
				ObservedAt: now,
				Payload:    []byte(`{"event":"EV1 PAYLOAD"}`),
			},
			{
				Sequence:   2,
				Type:       "tool_result",
				Subtype:    "shell",
				Source:     "agent",
				ObservedAt: now,
				Payload:    []byte(`{"event":"EV2 PAYLOAD"}`),
			},
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

// TestAgentCallPaneNotShownWhenClosed は AgentCallPaneOpen=false のとき詳細 pane が
// 描画されないことを観測する (View の排他描画)。
func TestAgentCallPaneNotShownWhenClosed(t *testing.T) {
	s := makePaneViewState()
	s.Detail.Focus = DetailFocusAgentCalls
	s.Detail.AgentCallsCursor = 0
	s.Detail.AgentCallPaneOpen = false

	view := s.View(120)
	if strings.Contains(view, "AC1 PROMPT TEXT") {
		t.Fatalf("expected pane closed (no Prompt body), but Prompt visible:\n%s", view)
	}
	if strings.Contains(view, "AC1 SUMMARY") {
		t.Fatalf("expected pane closed (no ResultSummary), but visible:\n%s", view)
	}
}

// TestAgentCallPaneShowsSelectedRowDetails は AgentCallPaneOpen=true のとき選択行
// (= AgentCallsCursor) の詳細フィールドが描画されることを観測する。
func TestAgentCallPaneShowsSelectedRowDetails(t *testing.T) {
	s := makePaneViewState()
	s.Detail.Focus = DetailFocusAgentCalls
	s.Detail.AgentCallsCursor = 0
	s.Detail.AgentCallPaneOpen = true

	view := s.View(120)
	wants := []string{
		"AC1 PROMPT TEXT",
		"AC1 SUMMARY",
		"AC1 ERROR DETAIL",
		"timeout",         // FailureCategory
		`"ac1":"payload"`, // ResultPayload (JSON 表示の一部)
	}
	for _, w := range wants {
		if !strings.Contains(view, w) {
			t.Fatalf("expected pane to contain %q, got:\n%s", w, view)
		}
	}
}

// TestAgentCallPaneShowsDifferentRowWhenCursorMoves は Cursor を 1 にした state を
// 直接組み立てて開いたとき、ac-2 の Prompt が出ることを観測する。
//
// (HandleKey で Cursor 移動すると pane は閉じる仕様だが、View の責務として
// 「現在の Cursor 行の詳細を出す」が正しく分岐していることを確認する。)
func TestAgentCallPaneShowsDifferentRowWhenCursorMoves(t *testing.T) {
	s := makePaneViewState()
	s.Detail.Focus = DetailFocusAgentCalls
	s.Detail.AgentCallsCursor = 1
	s.Detail.AgentCallPaneOpen = true

	view := s.View(120)
	if !strings.Contains(view, "AC2 PROMPT") {
		t.Fatalf("expected pane to contain `AC2 PROMPT`, got:\n%s", view)
	}
	if strings.Contains(view, "AC1 PROMPT TEXT") {
		t.Fatalf("expected pane NOT to contain ac-1 prompt when cursor=1, got:\n%s", view)
	}
}

// TestRawEventPaneNotShownWhenClosed は RawEventPaneOpen=false のとき Payload pane が
// 出ないことを観測する。
func TestRawEventPaneNotShownWhenClosed(t *testing.T) {
	s := makePaneViewState()
	s.Detail.Focus = DetailFocusRawEvents
	s.Detail.RawEventsCursor = 0
	s.Detail.RawEventPaneOpen = false

	view := s.View(120)
	if strings.Contains(view, "EV1 PAYLOAD") {
		t.Fatalf("expected raw_event pane closed, but payload visible:\n%s", view)
	}
}

// TestRawEventPaneShowsSelectedPayload は RawEventPaneOpen=true のとき選択行
// (= RawEventsCursor) の Payload が描画されることを観測する。
func TestRawEventPaneShowsSelectedPayload(t *testing.T) {
	s := makePaneViewState()
	s.Detail.Focus = DetailFocusRawEvents
	s.Detail.RawEventsCursor = 1 // EV2
	s.Detail.RawEventPaneOpen = true

	view := s.View(120)
	if !strings.Contains(view, "EV2 PAYLOAD") {
		t.Fatalf("expected pane to contain EV2 payload, got:\n%s", view)
	}
	if strings.Contains(view, "EV1 PAYLOAD") {
		t.Fatalf("expected pane NOT to contain EV1 payload when cursor=1, got:\n%s", view)
	}
}

// TestRawEventPaneShowsTruncatedNotice は PayloadTruncated=true のとき切り詰め通知が
// 出ることを観測する。
func TestRawEventPaneShowsTruncatedNotice(t *testing.T) {
	s := makePaneViewState()
	s.Detail.Focus = DetailFocusRawEvents
	s.Detail.RawEventsCursor = 0
	s.Detail.RawEventPaneOpen = true
	// EV1 を truncated にする
	s.Detail.Detail.RawEvents[0].PayloadTruncated = true

	view := s.View(120)
	if !strings.Contains(view, "truncated") {
		t.Fatalf("expected truncated notice in pane, got:\n%s", view)
	}
}

// TestPanesIndependentBetweenSections は両 pane state を同時に true にしたとき、
// agent_calls 一覧の直下に AgentCall 詳細、raw_events 一覧の直下に RawEvent
// 詳細が並ぶことを観測する (排他ではなく独立)。
func TestPanesIndependentBetweenSections(t *testing.T) {
	s := makePaneViewState()
	s.Detail.AgentCallPaneOpen = true
	s.Detail.RawEventPaneOpen = true
	s.Detail.AgentCallsCursor = 0
	s.Detail.RawEventsCursor = 0

	view := s.View(120)
	if !strings.Contains(view, "AC1 PROMPT TEXT") {
		t.Fatalf("expected agent_call pane open, got:\n%s", view)
	}
	if !strings.Contains(view, "EV1 PAYLOAD") {
		t.Fatalf("expected raw_event pane open, got:\n%s", view)
	}
}
