// Package ui のアカシックモード Detail サブビュー window クリップ表示のテスト。
//
// 背景: agent_calls / agent_context_usage / raw_events / artifacts は件数が多いと
// 縦方向に画面を圧迫し、Cursor 行が画面外に押し出されて視認できなくなっていた。
// 本テストは各セクションの renderer が clipListWindow を介して
//   - 表示行数を detailListWindow 件以下に抑える
//   - focused セクションでは Cursor が必ず表示範囲に含まれる
//   - 窓外の存在は末尾に "showing X-Y / total" で告知される
//
// ことを観測する。
package akashic

import (
	"strings"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

// makeManyAgentCalls は n 件の AgentCallRow を組み立てる (window クリップ観測用)。
func makeManyAgentCalls(n int) []akashic.AgentCallRow {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	calls := make([]akashic.AgentCallRow, 0, n)
	for i := 0; i < n; i++ {
		calls = append(calls, akashic.AgentCallRow{
			ID:         "ac-" + idxToken(i),
			StepID:     "step-" + idxToken(i),
			ProviderID: "p",
			StartedAt:  now.Add(time.Duration(i) * time.Second),
		})
	}
	return calls
}

// makeManyRawEvents は n 件の RawEventRow を組み立てる。
func makeManyRawEvents(n int) []akashic.RawEventRow {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	events := make([]akashic.RawEventRow, 0, n)
	for i := 0; i < n; i++ {
		events = append(events, akashic.RawEventRow{
			Sequence:   int64(i + 1),
			Type:       "type-" + idxToken(i),
			Subtype:    "sub",
			Source:     "src",
			ObservedAt: now.Add(time.Duration(i) * time.Second),
		})
	}
	return events
}

// makeManyArtifacts は n 件の ArtifactRow を組み立てる。
func makeManyArtifacts(n int) []akashic.ArtifactRow {
	rows := make([]akashic.ArtifactRow, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, akashic.ArtifactRow{
			Path:      "path-" + idxToken(i),
			Kind:      "file",
			SizeBytes: int64(i),
			MIME:      "text/plain",
		})
	}
	return rows
}

// idxToken は連番を 3 桁 0 埋め文字列に変換する (テストで一意な ID を作るための小ヘルパ)。
func idxToken(i int) string {
	return string(rune('A'+i%26)) +
		string(rune('A'+(i/26)%26)) +
		string(rune('0'+(i/676)%10))
}

// countLinesContaining は v に含まれる sub の行数 (= 描画されたアイテム件数の近似) を返す。
func countLinesContaining(v, sub string) int {
	c := 0
	for _, line := range strings.Split(v, "\n") {
		if strings.Contains(line, sub) {
			c++
		}
	}
	return c
}

// TestDetailAgentCallsClipsToWindow は agent_calls が window 件を超えるとき
// 表示行数が detailListWindow 件以下に抑えられることを観測する。
func TestDetailAgentCallsClipsToWindow(t *testing.T) {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	detail := akashic.RunDetail{
		Run:        akashic.RunHeader{RunID: "run-1", WorkflowID: "wf-1", StartedAt: now},
		AgentCalls: makeManyAgentCalls(50),
	}
	s := State{
		Subview: SubviewDetail,
		Reader:  &fakeReader{},
		Detail: DetailState{
			RunID:            "run-1",
			Detail:           &detail,
			Focus:            DetailFocusAgentCalls,
			AgentCallsCursor: 25,
		},
	}

	view := s.View(120)
	rows := countLinesContaining(view, "ac-")
	if rows > detailListWindow {
		t.Fatalf("expected at most %d agent_call rows, got %d\n%s", detailListWindow, rows, view)
	}
	if rows == 0 {
		t.Fatalf("expected at least 1 agent_call row, got 0\n%s", view)
	}
}

// TestDetailAgentCallsKeepsCursorInWindow は Focus=AgentCalls + Cursor=25 のとき
// Cursor が指す行 (ac-Z 番目) が表示窓内に必ず描画されることを観測する。
func TestDetailAgentCallsKeepsCursorInWindow(t *testing.T) {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	calls := makeManyAgentCalls(50)
	cursor := 25
	cursorID := calls[cursor].ID

	detail := akashic.RunDetail{
		Run:        akashic.RunHeader{RunID: "run-1", WorkflowID: "wf-1", StartedAt: now},
		AgentCalls: calls,
	}
	s := State{
		Subview: SubviewDetail,
		Reader:  &fakeReader{},
		Detail: DetailState{
			RunID:            "run-1",
			Detail:           &detail,
			Focus:            DetailFocusAgentCalls,
			AgentCallsCursor: cursor,
		},
	}

	view := s.View(120)
	if !strings.Contains(view, cursorID) {
		t.Fatalf("expected cursor row %q to be visible in clipped window\n%s", cursorID, view)
	}
}

// TestDetailAgentCallsShowsWindowFooter は窓外がある場合「showing X-Y / total」が
// 末尾に出ることを観測する。
func TestDetailAgentCallsShowsWindowFooter(t *testing.T) {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	detail := akashic.RunDetail{
		Run:        akashic.RunHeader{RunID: "run-1", WorkflowID: "wf-1", StartedAt: now},
		AgentCalls: makeManyAgentCalls(50),
	}
	s := State{
		Subview: SubviewDetail,
		Reader:  &fakeReader{},
		Detail: DetailState{
			RunID:            "run-1",
			Detail:           &detail,
			Focus:            DetailFocusAgentCalls,
			AgentCallsCursor: 25,
		},
	}

	view := s.View(120)
	if !strings.Contains(view, "/ 50") {
		t.Fatalf("expected window footer with `/ 50` for total=50, got:\n%s", view)
	}
}

// TestDetailRawEventsClipsToWindow は raw_events が window 件を超えるとき
// 表示行数が detailListWindow 件以下に抑えられることを観測する。
func TestDetailRawEventsClipsToWindow(t *testing.T) {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	detail := akashic.RunDetail{
		Run:       akashic.RunHeader{RunID: "run-1", WorkflowID: "wf-1", StartedAt: now},
		RawEvents: makeManyRawEvents(50),
	}
	s := State{
		Subview: SubviewDetail,
		Reader:  &fakeReader{},
		Detail: DetailState{
			RunID:           "run-1",
			Detail:          &detail,
			Focus:           DetailFocusRawEvents,
			RawEventsCursor: 30,
		},
	}

	view := s.View(120)
	rows := countLinesContaining(view, "[seq=")
	if rows > detailListWindow {
		t.Fatalf("expected at most %d raw_event rows, got %d\n%s", detailListWindow, rows, view)
	}
}

// TestDetailRawEventsKeepsCursorInWindow は Focus=RawEvents + Cursor=30 のとき
// Cursor が指す行が表示窓内に必ず描画されることを観測する。
func TestDetailRawEventsKeepsCursorInWindow(t *testing.T) {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	events := makeManyRawEvents(50)
	cursor := 30
	cursorSeq := events[cursor].Sequence

	detail := akashic.RunDetail{
		Run:       akashic.RunHeader{RunID: "run-1", WorkflowID: "wf-1", StartedAt: now},
		RawEvents: events,
	}
	s := State{
		Subview: SubviewDetail,
		Reader:  &fakeReader{},
		Detail: DetailState{
			RunID:           "run-1",
			Detail:          &detail,
			Focus:           DetailFocusRawEvents,
			RawEventsCursor: cursor,
		},
	}

	view := s.View(120)
	wantToken := "[seq=31]"
	_ = cursorSeq // 使うのは表示文字列上の seq=31
	if !strings.Contains(view, wantToken) {
		t.Fatalf("expected cursor seq token %q to be visible, got:\n%s", wantToken, view)
	}
}

// TestDetailArtifactsClipsToWindow は artifacts rows が window 件を超えるとき
// 表示行数が detailListWindow 件以下に抑えられることを観測する。
func TestDetailArtifactsClipsToWindow(t *testing.T) {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	detail := akashic.RunDetail{
		Run: akashic.RunHeader{RunID: "run-1", WorkflowID: "wf-1", StartedAt: now},
	}
	s := State{
		Subview: SubviewDetail,
		Reader:  &fakeReader{},
		Detail: DetailState{
			RunID:  "run-1",
			Detail: &detail,
			Focus:  DetailFocusArtifacts,
			Artifact: ArtifactState{
				Rows:          makeManyArtifacts(40),
				SelectedIndex: 20,
			},
		},
	}

	view := s.View(120)
	rows := countLinesContaining(view, "[file] path-")
	if rows > detailListWindow {
		t.Fatalf("expected at most %d artifact rows, got %d\n%s", detailListWindow, rows, view)
	}
}

// TestDetailArtifactsKeepsSelectedInWindow は Focus=Artifacts + SelectedIndex=20
// のとき選択行が必ず描画されることを観測する。
func TestDetailArtifactsKeepsSelectedInWindow(t *testing.T) {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	rows := makeManyArtifacts(40)
	idx := 20
	wantPath := rows[idx].Path

	detail := akashic.RunDetail{
		Run: akashic.RunHeader{RunID: "run-1", WorkflowID: "wf-1", StartedAt: now},
	}
	s := State{
		Subview: SubviewDetail,
		Reader:  &fakeReader{},
		Detail: DetailState{
			RunID:  "run-1",
			Detail: &detail,
			Focus:  DetailFocusArtifacts,
			Artifact: ArtifactState{
				Rows:          rows,
				SelectedIndex: idx,
			},
		},
	}

	view := s.View(120)
	if !strings.Contains(view, wantPath) {
		t.Fatalf("expected selected artifact path %q to be visible, got:\n%s", wantPath, view)
	}
}

// TestDetailContextUsageClipsToWindow は agent_context_usage 件数が window 超のとき
// 表示行数が detailListWindow 件以下に抑えられることを観測する (Focus 対象でなくても
// 画面圧迫を防ぐため window クリップを適用する)。
func TestDetailContextUsageClipsToWindow(t *testing.T) {
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	usages := make([]akashic.AgentContextUsageRow, 0, 30)
	for i := 0; i < 30; i++ {
		usages = append(usages, akashic.AgentContextUsageRow{
			AgentCallID: "ac-" + idxToken(i),
			Sequence:    int64(i + 1),
			Model:       "model-x",
			TotalTokens: int64(i * 10),
			MaxTokens:   1000,
			Percentage:  int64(i),
			ObservedAt:  now,
		})
	}
	detail := akashic.RunDetail{
		Run:          akashic.RunHeader{RunID: "run-1", WorkflowID: "wf-1", StartedAt: now},
		ContextUsage: usages,
	}
	s := State{
		Subview: SubviewDetail,
		Reader:  &fakeReader{},
		Detail: DetailState{
			RunID:  "run-1",
			Detail: &detail,
			Focus:  DetailFocusAgentCalls,
		},
	}

	view := s.View(120)
	rows := countLinesContaining(view, "model-x")
	if rows > detailListWindow {
		t.Fatalf("expected at most %d context_usage rows, got %d\n%s", detailListWindow, rows, view)
	}
}
