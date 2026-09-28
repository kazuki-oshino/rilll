// Package ui のアカシックモード Detail サブビュー Focus 状態行のテスト。
//
// 背景: Detail サブビューでは Tab/Shift+Tab で Focus が agent_calls / raw_events /
// artifacts を循環し、j/k/↑↓ で Cursor が動く。だがリスト件数が多いと Cursor
// 行が画面外に押し出されて視認できず、「キーが効いていないように見える」UX
// 問題があった。本テストは renderDetail のヘッダ直下に
// `Focus: <name>  Cursor: <i+1>/<n>` の 1 行が常に表示され、Tab/j/k の効きが
// 状態行の文字列変化として常時可視化されることを観測する。
package akashic

import (
	"strings"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

// makeFocusStatusViewState は agent_calls / raw_events / artifacts の各セクションに
// 件数を持たせた Detail を組み立てる小さなヘルパ。Focus と Cursor は呼び出し側で上書きする。
func makeFocusStatusViewState() State {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	detail := akashic.RunDetail{
		Run: akashic.RunHeader{RunID: "run-1", WorkflowID: "wf-1", StartedAt: now},
		AgentCalls: []akashic.AgentCallRow{
			{ID: "ac-1", StepID: "s1", ProviderID: "p", StartedAt: now},
			{ID: "ac-2", StepID: "s2", ProviderID: "p", StartedAt: now},
		},
		RawEvents: []akashic.RawEventRow{
			{Sequence: 1, Type: "t", Subtype: "u", Source: "s", ObservedAt: now},
			{Sequence: 2, Type: "t", Subtype: "u", Source: "s", ObservedAt: now},
			{Sequence: 3, Type: "t", Subtype: "u", Source: "s", ObservedAt: now},
		},
	}
	s := State{
		Subview: SubviewDetail,
		Reader:  &fakeReader{},
		Detail: DetailState{
			RunID:  "run-1",
			Detail: &detail,
			Artifact: ArtifactState{
				Rows: []akashic.ArtifactRow{
					{Path: "a.txt", Kind: "file", SizeBytes: 1, MIME: "text/plain"},
					{Path: "b.txt", Kind: "file", SizeBytes: 2, MIME: "text/plain"},
				},
			},
		},
	}
	return s
}

// TestDetailFocusStatusLineForAgentCalls は Focus=AgentCalls + Cursor=0 のとき
// `Focus: agent_calls  Cursor: 1/2` が含まれることを観測する。
func TestDetailFocusStatusLineForAgentCalls(t *testing.T) {
	s := makeFocusStatusViewState()
	s.Detail.Focus = DetailFocusAgentCalls
	s.Detail.AgentCallsCursor = 0

	view := s.View(120)
	if !strings.Contains(view, "Focus: agent_calls") {
		t.Fatalf("expected `Focus: agent_calls` in view, got:\n%s", view)
	}
	if !strings.Contains(view, "Cursor: 1/2") {
		t.Fatalf("expected `Cursor: 1/2` in view, got:\n%s", view)
	}
}

// TestDetailFocusStatusLineReflectsCursorMovement は Cursor を動かすと状態行の
// 表示位置が更新されることを観測する (Tab/j/k の効きの可視化)。
func TestDetailFocusStatusLineReflectsCursorMovement(t *testing.T) {
	s := makeFocusStatusViewState()
	s.Detail.Focus = DetailFocusAgentCalls
	s.Detail.AgentCallsCursor = 1

	view := s.View(120)
	if !strings.Contains(view, "Cursor: 2/2") {
		t.Fatalf("expected `Cursor: 2/2` after cursor move, got:\n%s", view)
	}
}

// TestDetailFocusStatusLineForRawEvents は Focus=RawEvents で raw_events 件数と
// 該当 Cursor が状態行に反映されることを観測する。
func TestDetailFocusStatusLineForRawEvents(t *testing.T) {
	s := makeFocusStatusViewState()
	s.Detail.Focus = DetailFocusRawEvents
	s.Detail.RawEventsCursor = 2

	view := s.View(120)
	if !strings.Contains(view, "Focus: raw_events") {
		t.Fatalf("expected `Focus: raw_events`, got:\n%s", view)
	}
	if !strings.Contains(view, "Cursor: 3/3") {
		t.Fatalf("expected `Cursor: 3/3`, got:\n%s", view)
	}
}

// TestDetailFocusStatusLineForArtifacts は Focus=Artifacts で artifact rows 件数と
// SelectedIndex が状態行に反映されることを観測する。
func TestDetailFocusStatusLineForArtifacts(t *testing.T) {
	s := makeFocusStatusViewState()
	s.Detail.Focus = DetailFocusArtifacts
	s.Detail.Artifact.SelectedIndex = 1

	view := s.View(120)
	if !strings.Contains(view, "Focus: artifacts") {
		t.Fatalf("expected `Focus: artifacts`, got:\n%s", view)
	}
	if !strings.Contains(view, "Cursor: 2/2") {
		t.Fatalf("expected `Cursor: 2/2`, got:\n%s", view)
	}
}

// TestDetailFocusStatusLineForEmptyList は当該セクションの件数が 0 のとき
// `Cursor: -/0` のような空表記で graceful に出ることを観測する。
func TestDetailFocusStatusLineForEmptyList(t *testing.T) {
	s := makeFocusStatusViewState()
	// AgentCalls / RawEvents を空にする
	s.Detail.Detail.AgentCalls = nil
	s.Detail.Detail.RawEvents = nil
	s.Detail.Focus = DetailFocusAgentCalls

	view := s.View(120)
	if !strings.Contains(view, "Focus: agent_calls") {
		t.Fatalf("expected `Focus: agent_calls`, got:\n%s", view)
	}
	if !strings.Contains(view, "Cursor: -/0") {
		t.Fatalf("expected `Cursor: -/0` for empty list, got:\n%s", view)
	}
}
