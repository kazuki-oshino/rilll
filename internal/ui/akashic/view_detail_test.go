// Package ui のアカシックモード Run 詳細サブビュー描画 (View) に対するテスト
// (akashic-mode-tui spec Task 6.3)。
//
// 本ファイルは State.View(width) の SubviewDetail 経路を観測する。
// state 側 (Update / HandleKey) の挙動は Task 6.1 / 6.2 の test (akashic_detail_test.go /
// akashic_artifact_test.go) で別途観測される。
//
// 観測完了条件 (Task 6.3):
//   - 通常 RunDetail + Artifact 一覧が表示される View(120) 出力に
//     「workflow_run」「agent_calls」「raw_events」「成果物」見出しと該当行が含まれる
//   - フォーカス位置 (Header / AgentCalls / RawEvents / Artifacts) の
//     ハイライトが該当行に出現し、他行には出現しない
//   - graceful 文言 (「履歴データが利用できません」「成果物データが利用できません」
//     「該当 run が存在しない」「上限超過のため本体を読み込みませんでした」
//     「成果物が見つかりません」「成果物なし」) が想定ケースのみで出現する
//   - Loading / Err / Reader=nil / ArtifactReader 未配線 のケースで stale データが
//     描画されない (排他的状態セット)
//   - Hand-off 失敗 (Detail.HandoffErr) の inline 通知が表示される
package akashic

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

// makeAkashicDetailViewState は SubviewDetail で初期化した State を返すヘルパ。
// 本ファイル内の View 描画テストでのみ利用する。reader / artifactReader が nil でも
// graceful に初期化される (Reader=nil の graceful テストで使う)。
func makeAkashicDetailViewState(reader akashic.Reader, artifactReader akashic.ArtifactReader) State {
	return State{
		Subview:        SubviewDetail,
		Reader:         reader,
		ArtifactReader: artifactReader,
		Detail: DetailState{
			RunID: "run-1",
			Focus: DetailFocusHeader,
		},
	}
}

// makePopulatedRunDetail は通常表示テスト用の RunDetail (run / agent_calls /
// agent_context_usage / raw_events) を組み立てて返す。timestamps はテスト固定値で
// 再現性を確保する。
func makePopulatedRunDetail() akashic.RunDetail {
	startedAt := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	finishedAt := startedAt.Add(2 * time.Minute)
	successTrue := true
	cost := 0.0123
	dur := int64(1234)
	inTok := int64(100)
	outTok := int64(50)
	next := 100
	return akashic.RunDetail{
		Run: akashic.RunHeader{
			RunID:        "run-1",
			WorkflowID:   "wf-alpha",
			WorkflowName: "Alpha",
			CWD:          "/repo/alpha",
			StartedAt:    startedAt,
			FinishedAt:   &finishedAt,
			Success:      &successTrue,
			Error:        "",
		},
		AgentCalls: []akashic.AgentCallRow{
			{
				ID:            "ac-1",
				WorkflowRunID: "run-1",
				StepID:        "plan",
				StepName:      "Plan step",
				ProviderID:    "claude",
				Success:       &successTrue,
				DurationMS:    &dur,
				TotalCostUSD:  &cost,
				InputTokens:   &inTok,
				OutputTokens:  &outTok,
				StartedAt:     startedAt,
			},
			{
				ID:            "ac-2",
				WorkflowRunID: "run-1",
				StepID:        "exec",
				StepName:      "Execute step",
				ProviderID:    "claude",
				Success:       &successTrue,
				DurationMS:    &dur,
				TotalCostUSD:  &cost,
				StartedAt:     startedAt.Add(time.Second),
			},
		},
		ContextUsage: []akashic.AgentContextUsageRow{
			{
				AgentCallID:   "ac-1",
				WorkflowRunID: "run-1",
				RawEventID:    1,
				Sequence:      1,
				ObservedAt:    startedAt,
				Model:         "claude-sonnet-4",
				TotalTokens:   12000,
				MaxTokens:     200000,
				Percentage:    6,
			},
			{
				AgentCallID:   "ac-2",
				WorkflowRunID: "run-1",
				RawEventID:    2,
				Sequence:      2,
				ObservedAt:    startedAt.Add(time.Second),
				Model:         "claude-sonnet-4",
				TotalTokens:   24000,
				MaxTokens:     200000,
				Percentage:    12,
			},
		},
		RawEvents: []akashic.RawEventRow{
			{ID: 1, AgentCallID: "ac-1", WorkflowRunID: "run-1", Sequence: 1, Source: "agent", Type: "tool_use", Subtype: "edit", ObservedAt: startedAt},
			{ID: 2, AgentCallID: "ac-1", WorkflowRunID: "run-1", Sequence: 2, Source: "agent", Type: "tool_result", Subtype: "ok", ObservedAt: startedAt.Add(time.Second)},
		},
		RawEventsLimit:      akashic.DefaultRawEventsLimit,
		RawEventsOffset:     0,
		NextRawEventsOffset: &next,
	}
}

// makePopulatedArtifactRows は通常表示テスト用の ArtifactRow 群を返す。
func makePopulatedArtifactRows() []akashic.ArtifactRow {
	mtime := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	return []akashic.ArtifactRow{
		{WorkflowRunID: "run-1", Kind: akashic.ArtifactKindProposal, Path: "/repo/alpha/.rilll/proposal.md", SizeBytes: 1024, Mtime: mtime, MIME: "text/markdown"},
		{WorkflowRunID: "run-1", Kind: akashic.ArtifactKindResult, Path: "/repo/alpha/.rilll/result.json", SizeBytes: 2048, Mtime: mtime, MIME: "application/json"},
	}
}

// --- View (SubviewDetail): 通常表示 (a) 見出しと行 ---

// TestAkashicDetailViewRendersAllSectionsAndRows は通常 RunDetail + Artifact が
// 揃った state を View(120) で描画したとき、4 つのセクション見出し
// 「workflow_run」「agent_calls」「raw_events」「成果物」と各行 (run_id / step /
// raw_event sequence / artifact kind&path) が含まれることを観測する。
func TestAkashicDetailViewRendersAllSectionsAndRows(t *testing.T) {
	detail := makePopulatedRunDetail()
	rows := makePopulatedArtifactRows()
	s := makeAkashicDetailViewState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Detail = &detail
	s.Detail.RawEventsOffset = detail.RawEventsOffset
	s.Detail.NextRawEventsOffset = detail.NextRawEventsOffset
	s.Detail.Artifact.Rows = rows
	s.Detail.Artifact.SelectedIndex = 0

	view := s.View(120)

	for _, want := range []string{
		"workflow_run",
		"agent_calls",
		"agent_context_usage",
		"raw_events",
		"成果物",
		"run-1",
		"wf-alpha",
		"ac-1",
		"ac-2",
		"tool_use",
		"tool_result",
		"claude-sonnet-4", // ContextUsage.Model 由来 (agent_context_usage 行が描画されている証跡)
		"proposal.md",
		"result.json",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected View to contain %q, got:\n%s", want, view)
		}
	}
}

// --- View (SubviewDetail): (b) フォーカス位置のハイライト ---

// TestAkashicDetailViewFocusHighlightsAgentCallsRow は Focus=AgentCalls + 特定
// AgentCallsCursor のときに、その行に「›」カーソル記号 (renderChoiceWithWidth /
// selectedStyle 経路の互換シンボル) が付き、他の Focus には付かないことを観測する。
func TestAkashicDetailViewFocusHighlightsAgentCallsRow(t *testing.T) {
	detail := makePopulatedRunDetail()
	s := makeAkashicDetailViewState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Detail = &detail
	s.Detail.Focus = DetailFocusAgentCalls
	s.Detail.AgentCallsCursor = 1 // 2 番目の agent_call (ac-2) を選択

	view := s.View(120)

	// 選択行 (ac-2) は "›" カーソル記号を含む行に乗る (renderChoiceWithWidth と同形式)
	lines := strings.Split(view, "\n")
	var found bool
	for _, line := range lines {
		if strings.Contains(line, "ac-2") && strings.Contains(line, "›") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected ac-2 row to have cursor mark '›' under Focus=AgentCalls, got view:\n%s", view)
	}

	// 非選択行 (ac-1) には "›" カーソル記号は付かない
	for _, line := range lines {
		if strings.Contains(line, "ac-1") && strings.Contains(line, "›") {
			t.Fatalf("ac-1 row must NOT have cursor mark '›' (selected is ac-2), got line: %q", line)
		}
	}
}

// TestAkashicDetailViewFocusHighlightsRawEventsRow は Focus=RawEvents + 特定
// RawEventsCursor のときに、その行に「›」カーソル記号が付くことを観測する。
func TestAkashicDetailViewFocusHighlightsRawEventsRow(t *testing.T) {
	detail := makePopulatedRunDetail()
	s := makeAkashicDetailViewState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Detail = &detail
	s.Detail.Focus = DetailFocusRawEvents
	s.Detail.RawEventsCursor = 1 // 2 番目の raw_event (sequence=2) を選択

	view := s.View(120)
	lines := strings.Split(view, "\n")
	var found bool
	for _, line := range lines {
		if strings.Contains(line, "tool_result") && strings.Contains(line, "›") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected raw_event tool_result row to have cursor mark '›' under Focus=RawEvents, got view:\n%s", view)
	}
}

// TestAkashicDetailViewFocusHighlightsArtifactRow は Focus=Artifacts + 特定
// SelectedIndex のときに、その行に「›」カーソル記号が付くことを観測する。
func TestAkashicDetailViewFocusHighlightsArtifactRow(t *testing.T) {
	detail := makePopulatedRunDetail()
	rows := makePopulatedArtifactRows()
	s := makeAkashicDetailViewState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Detail = &detail
	s.Detail.Artifact.Rows = rows
	s.Detail.Focus = DetailFocusArtifacts
	s.Detail.Artifact.SelectedIndex = 1 // result.json 行を選択

	view := s.View(120)
	lines := strings.Split(view, "\n")
	var found bool
	for _, line := range lines {
		if strings.Contains(line, "result.json") && strings.Contains(line, "›") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected result.json artifact row to have cursor mark '›' under Focus=Artifacts, got view:\n%s", view)
	}
}

// --- View (SubviewDetail): (c) graceful 文言 (排他) ---

// TestAkashicDetailViewReaderNilShowsUnavailable は Reader=nil のとき
// 「履歴データが利用できません」を表示し、stale な Detail / Artifact 表示が出ない
// ことを観測する (Req 11.2)。
func TestAkashicDetailViewReaderNilShowsUnavailable(t *testing.T) {
	detail := makePopulatedRunDetail()
	rows := makePopulatedArtifactRows()
	s := makeAkashicDetailViewState(nil, nil)
	// stale データが残っていても Reader=nil 経路では表示されない
	s.Detail.Detail = &detail
	s.Detail.Artifact.Rows = rows
	s.Detail.Loading = true
	s.Detail.Err = errors.New("stale-err")

	view := s.View(120)
	if !strings.Contains(view, "履歴データが利用できません") {
		t.Fatalf("expected '履歴データが利用できません' for Reader=nil, got:\n%s", view)
	}
	for _, nw := range []string{"agent_calls", "agent_context_usage", "raw_events", "成果物", "stale-err", "読み込み中", "ac-1", "ac-2", "claude-sonnet-4", "proposal.md"} {
		if strings.Contains(view, nw) {
			t.Fatalf("Reader=nil view must not render %q, got:\n%s", nw, view)
		}
	}
}

// TestAkashicDetailViewArtifactReaderUnavailableShowsArtifactMessage は
// ArtifactReader 未配線 (Detail.Artifact.Unavailable=true) のとき、Artifact
// 領域に「成果物データが利用できません」が表示されることを観測する (Req 11.3)。
// この graceful は Reader が居る限り Detail 本体描画は通常通り行われ、Artifact
// 領域だけが unavailable 表示に差し替わる (排他は領域内)。
func TestAkashicDetailViewArtifactReaderUnavailableShowsArtifactMessage(t *testing.T) {
	detail := makePopulatedRunDetail()
	s := makeAkashicDetailViewState(&fakeReader{}, nil)
	s.Detail.Detail = &detail
	s.Detail.Artifact.Unavailable = true

	view := s.View(120)
	if !strings.Contains(view, "成果物データが利用できません") {
		t.Fatalf("expected '成果物データが利用できません' for ArtifactReader unavailable, got:\n%s", view)
	}
	// Detail 本体は描画され続ける (Reader は配線されているため)
	for _, want := range []string{"workflow_run", "agent_calls", "ac-1"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected Detail body to be rendered when only ArtifactReader unavailable, missing %q, got:\n%s", want, view)
		}
	}
	// 「履歴データが利用できません」は出ない (Reader は配線されているため)
	if strings.Contains(view, "履歴データが利用できません") {
		t.Fatalf("must NOT show Reader-unavailable message when Reader is wired, got:\n%s", view)
	}
}

// TestAkashicDetailViewLoadingShowsLoading は Detail.Loading=true のとき
// 「読み込み中...」が表示されることを観測する。
func TestAkashicDetailViewLoadingShowsLoading(t *testing.T) {
	s := makeAkashicDetailViewState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Loading = true

	view := s.View(120)
	if !strings.Contains(view, "読み込み中") {
		t.Fatalf("expected '読み込み中' for Detail.Loading=true, got:\n%s", view)
	}
}

// TestAkashicDetailViewErrWorkflowRunNotFoundShowsMessage は
// Detail.Err == ErrWorkflowRunNotFound のとき「該当 run が存在しない」が表示され、
// Detail 本体 / 一般エラー文は表示されないことを観測する (Req 5.4)。
func TestAkashicDetailViewErrWorkflowRunNotFoundShowsMessage(t *testing.T) {
	s := makeAkashicDetailViewState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Err = akashic.ErrWorkflowRunNotFound

	view := s.View(120)
	if !strings.Contains(view, runDetailNotFoundMessage) {
		t.Fatalf("expected %q for ErrWorkflowRunNotFound, got:\n%s", runDetailNotFoundMessage, view)
	}
	// 一般 error 経路 (sentinel error 文字列がそのまま出る) の混入も防ぐ
	if strings.Contains(view, "agent_calls") {
		t.Fatalf("must NOT render Detail body sections on ErrWorkflowRunNotFound, got:\n%s", view)
	}
}

// TestAkashicDetailViewGenericErrorShownAsWarning は
// Detail.Err が一般エラー (not-found 以外) のとき、エラーメッセージが
// (warningStyle 経由で) 表示され、Detail 本体は描画されないことを観測する (Req 5.5)。
func TestAkashicDetailViewGenericErrorShownAsWarning(t *testing.T) {
	s := makeAkashicDetailViewState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Err = errors.New("boom-detail-display")

	view := s.View(120)
	if !strings.Contains(view, "boom-detail-display") {
		t.Fatalf("expected error text in view, got:\n%s", view)
	}
	if strings.Contains(view, "agent_calls") {
		t.Fatalf("must NOT render Detail body sections on generic error, got:\n%s", view)
	}
}

// TestAkashicDetailViewHandoffErrShownInline は Detail.HandoffErr が non-nil のとき、
// hand-off 失敗の inline 通知としてメッセージが表示されることを観測する (Req 7.7)。
// Detail 本体描画は壊さない (排他的に上書きしない)。
func TestAkashicDetailViewHandoffErrShownInline(t *testing.T) {
	detail := makePopulatedRunDetail()
	s := makeAkashicDetailViewState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Detail = &detail
	s.Detail.HandoffErr = errors.New("improve-workflow not registered")

	view := s.View(120)
	if !strings.Contains(view, "improve-workflow not registered") {
		t.Fatalf("expected hand-off err inline notification, got:\n%s", view)
	}
	// Detail 本体描画は引き続き行う
	for _, want := range []string{"workflow_run", "agent_calls", "ac-1"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected Detail body to be rendered alongside HandoffErr, missing %q, got:\n%s", want, view)
		}
	}
}

// --- View (SubviewDetail): Artifact preview pane の graceful ---

// TestAkashicDetailViewPreviewWarningTooLargeShowsMessage は
// PreviewWarning=previewWarningTooLarge のとき、Artifact 領域に
// 「上限超過のため本体を読み込みませんでした」が表示されることを観測する (Req 6.5)。
// メタ一覧は引き続き表示される (Req 6.5 後段「メタは継続表示」)。
func TestAkashicDetailViewPreviewWarningTooLargeShowsMessage(t *testing.T) {
	detail := makePopulatedRunDetail()
	rows := makePopulatedArtifactRows()
	s := makeAkashicDetailViewState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Detail = &detail
	s.Detail.Artifact.Rows = rows
	s.Detail.Artifact.PreviewPath = rows[0].Path
	s.Detail.Artifact.PreviewWarning = previewWarningTooLarge

	view := s.View(120)
	if !strings.Contains(view, previewWarningTooLarge) {
		t.Fatalf("expected %q for ErrArtifactTooLarge, got:\n%s", previewWarningTooLarge, view)
	}
	if !strings.Contains(view, "proposal.md") {
		t.Fatalf("expected meta list to remain visible (Req 6.5), got:\n%s", view)
	}
}

// TestAkashicDetailViewPreviewWarningMissingShowsMessage は
// PreviewWarning=previewWarningMissing のとき「成果物が見つかりません」が
// 表示されることを観測する (Req 6.7)。
func TestAkashicDetailViewPreviewWarningMissingShowsMessage(t *testing.T) {
	detail := makePopulatedRunDetail()
	rows := makePopulatedArtifactRows()
	s := makeAkashicDetailViewState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Detail = &detail
	s.Detail.Artifact.Rows = rows
	s.Detail.Artifact.PreviewPath = rows[0].Path
	s.Detail.Artifact.PreviewWarning = previewWarningMissing

	view := s.View(120)
	if !strings.Contains(view, previewWarningMissing) {
		t.Fatalf("expected %q for ErrArtifactFileMissing, got:\n%s", previewWarningMissing, view)
	}
}

// TestAkashicDetailViewArtifactEmptyMessageShown は Detail.Artifact.EmptyMessage が
// 「成果物なし」のとき、Artifact 領域に「成果物なし」が表示されることを観測する (Req 6.9)。
func TestAkashicDetailViewArtifactEmptyMessageShown(t *testing.T) {
	detail := makePopulatedRunDetail()
	s := makeAkashicDetailViewState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Detail = &detail
	s.Detail.Artifact.EmptyMessage = emptyArtifactsMessage

	view := s.View(120)
	if !strings.Contains(view, emptyArtifactsMessage) {
		t.Fatalf("expected %q for empty artifacts, got:\n%s", emptyArtifactsMessage, view)
	}
}

// TestAkashicDetailViewPreviewBytesRenderedRaw は PreviewBytes 非空のとき
// 本体プレビュー領域に raw bytes が文字列としてそのまま含まれることを観測する (Req 6.4)。
// Markdown シンタックスハイライトは行わない。
func TestAkashicDetailViewPreviewBytesRenderedRaw(t *testing.T) {
	detail := makePopulatedRunDetail()
	rows := makePopulatedArtifactRows()
	s := makeAkashicDetailViewState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Detail = &detail
	s.Detail.Artifact.Rows = rows
	s.Detail.Artifact.PreviewPath = rows[0].Path
	s.Detail.Artifact.PreviewBytes = []byte("# Title\n本体プレビュー文字列です")

	view := s.View(120)
	if !strings.Contains(view, "本体プレビュー文字列です") {
		t.Fatalf("expected raw preview text in view, got:\n%s", view)
	}
	// 「# Title」がそのまま raw で表示される (Markdown syntax highlighting なし)
	if !strings.Contains(view, "# Title") {
		t.Fatalf("expected raw '# Title' preserved (no markdown rendering), got:\n%s", view)
	}
}

// --- View (SubviewDetail): 排他状態の網羅 ---

// TestAkashicDetailViewExclusiveTopLevelStates は Detail サブビューの最上位
// 排他状態 (Reader=nil / Loading / NotFound / 一般 Err / 通常表示) で、
// 想定文言のみが出現し他の排他文言が混入しないことを観測する。
func TestAkashicDetailViewExclusiveTopLevelStates(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*State)
		want    string
		notWant []string
	}{
		{
			name: "ReaderNil",
			mutate: func(s *State) {
				s.Reader = nil
			},
			want:    "履歴データが利用できません",
			notWant: []string{"読み込み中", runDetailNotFoundMessage, "agent_calls", "agent_context_usage", "raw_events", "成果物"},
		},
		{
			name: "Loading",
			mutate: func(s *State) {
				s.Detail.Loading = true
			},
			want:    "読み込み中",
			notWant: []string{"履歴データが利用できません", runDetailNotFoundMessage, "agent_calls", "agent_context_usage", "raw_events"},
		},
		{
			name: "ErrWorkflowRunNotFound",
			mutate: func(s *State) {
				s.Detail.Err = akashic.ErrWorkflowRunNotFound
			},
			want:    runDetailNotFoundMessage,
			notWant: []string{"履歴データが利用できません", "読み込み中", "agent_calls", "agent_context_usage", "raw_events"},
		},
		{
			name: "GenericError",
			mutate: func(s *State) {
				s.Detail.Err = errors.New("boom-exclusive-detail")
			},
			want:    "boom-exclusive-detail",
			notWant: []string{"履歴データが利用できません", "読み込み中", runDetailNotFoundMessage, "agent_calls", "agent_context_usage", "raw_events"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := makeAkashicDetailViewState(&fakeReader{}, &fakeArtifactReader{})
			c.mutate(&s)
			view := s.View(120)
			if !strings.Contains(view, c.want) {
				t.Fatalf("[%s] expected %q in view, got:\n%s", c.name, c.want, view)
			}
			for _, nw := range c.notWant {
				if strings.Contains(view, nw) {
					t.Fatalf("[%s] view must NOT contain %q, got:\n%s", c.name, nw, view)
				}
			}
		})
	}
}
