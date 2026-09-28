// Package ui のアカシックモード集計サマリサブビューに対するテスト
// (akashic-mode-tui spec Task 5.1)。
//
// 本ファイルは State.enterSummary / Update の aggregate 系 msg 受信処理 /
// State.View の SubviewSummary 描画 (Reader=nil graceful 含む) を観測する。
//
// 観測完了条件 (Task 5.1):
//   - fake Reader が (a) 通常 buckets、(b) 空 buckets、(c) error、(d) Reader=nil
//     の 4 ケースで、それぞれ Buckets / EmptyMessage / Err / 「履歴データが
//     利用できません」が排他的にセットされる
//   - Run 一覧側でフィルタを設定した状態でも Aggregate Options が常に空
//     (Req 4.4: Run 一覧サブビューのフィルタとは独立)
//   - tea.Quit が一度も返らない (Req 4.6 / 11.5)
//   - Reader=nil 時は Reader メソッドが 0 回呼ばれる (cmd が nil または
//     errReaderUnavailable msg 即時返し)
//   - ASCII バーが [####------] 形式で表示され、長さが画面幅依存 (10〜20)
//     (Req 4.7)
package akashic

import (
	"errors"
	"strings"
	"testing"
	"testing/quick"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

// makeAkashicSummaryState は SubviewSummary で初期化した State を返すヘルパ。
// reader が nil でも graceful に初期化される。
func makeAkashicSummaryState(reader akashic.Reader) State {
	return State{
		Subview: SubviewSummary,
		Reader:  reader,
		Summary: SummaryState{},
	}
}

// runBatchCmd は tea.Batch から返る Cmd を invoke して内側の Cmd 群を実行し、
// 各内側 Cmd の戻り tea.Msg を順序保持で返す。
//
// 動機: enterSummary は tea.Batch(workflowCmd, providerCmd) を返すが、
// 通常 invoke すると tea.BatchMsg ([]tea.Cmd) しか得られないため、
// 内側 Cmd の戻り msg を観測したいテストではここで一段展開する必要がある。
func runBatchCmd(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		// 単発 Cmd のときは 1 件として返す
		return []tea.Msg{msg}
	}
	out := make([]tea.Msg, 0, len(batch))
	for _, inner := range batch {
		if inner == nil {
			continue
		}
		out = append(out, inner())
	}
	return out
}

// --- State.enterSummary ---

// TestAkashicEnterSummaryIssuesBothCmdsWithEmptyOptions は Task 5.1 の中核要件:
//   - SubviewSummary に切替時に AggregateByWorkflowCmd と AggregateByProviderCmd
//     の両方を発行する (tea.Batch で組み合わせる)
//   - Options は空 (WorkflowAggregateOptions{} / ProviderAggregateOptions{}) で
//     固定する (Req 4.4: Run 一覧サブビューのフィルタとは独立)
//
// を 1 ケースで観測する。Run 一覧側に WorkflowID / Range / Success フィルタが
// 設定されている状態を作っても、Aggregate Options は zero value のまま発行される
// ことを fake Reader への入力で検証する。
func TestAkashicEnterSummaryIssuesBothCmdsWithEmptyOptions(t *testing.T) {
	reader := &fakeReader{}
	successFlag := true
	s := State{
		Subview: SubviewList,
		Reader:  reader,
		List: ListState{
			// 意図的にフィルタを設定: Aggregate Options に伝播してはならない (Req 4.4)
			Filter: ListFilter{
				WorkflowID: "wf-X",
				Success:    &successFlag,
			},
		},
	}

	got, cmd := s.enterSummary()
	if got.Subview != SubviewSummary {
		t.Fatalf("expected Subview=SubviewSummary, got %q", got.Subview)
	}
	if !got.Summary.Loading {
		t.Fatalf("expected Summary.Loading=true on enter (Reader != nil)")
	}
	if cmd == nil {
		t.Fatal("expected non-nil Cmd batch on enter when Reader != nil")
	}

	msgs := runBatchCmd(t, cmd)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 inner cmds (workflow + provider), got %d", len(msgs))
	}

	// 順序を問わず両 msg が含まれていることを確認
	var sawWorkflowMsg, sawProviderMsg bool
	for _, m := range msgs {
		switch m.(type) {
		case AggregateByWorkflowMsg:
			sawWorkflowMsg = true
		case AggregateByProviderMsg:
			sawProviderMsg = true
		}
	}
	if !sawWorkflowMsg {
		t.Fatalf("expected AggregateByWorkflowMsg in batch, got %#v", msgs)
	}
	if !sawProviderMsg {
		t.Fatalf("expected AggregateByProviderMsg in batch, got %#v", msgs)
	}

	// fake Reader への入力検証 (Req 4.4: フィルタ独立性)
	if reader.aggWorkflowCalls != 1 {
		t.Fatalf("expected AggregateByWorkflow to be called once, got %d", reader.aggWorkflowCalls)
	}
	if reader.aggProviderCalls != 1 {
		t.Fatalf("expected AggregateByProvider to be called once, got %d", reader.aggProviderCalls)
	}
	if reader.aggWorkflowOpts != (akashic.WorkflowAggregateOptions{}) {
		t.Fatalf("expected empty WorkflowAggregateOptions (Req 4.4), got %+v", reader.aggWorkflowOpts)
	}
	if reader.aggProviderOpts != (akashic.ProviderAggregateOptions{}) {
		t.Fatalf("expected empty ProviderAggregateOptions (Req 4.4), got %+v", reader.aggProviderOpts)
	}
}

// TestAkashicEnterSummaryReaderNilDoesNotIssueCmds は Reader=nil 時に Cmd が
// 一切発行されない (Req 11.2) ことと、Loading=true にも遷移しない
// (graceful 経路で「履歴データが利用できません」を View が即出すため Loading
// 中の表示にはしない) ことを観測する。
func TestAkashicEnterSummaryReaderNilDoesNotIssueCmds(t *testing.T) {
	s := State{Subview: SubviewList, Reader: nil}
	got, cmd := s.enterSummary()
	if got.Subview != SubviewSummary {
		t.Fatalf("expected Subview=SubviewSummary, got %q", got.Subview)
	}
	if cmd != nil {
		t.Fatalf("expected nil Cmd when Reader is nil (Req 11.2), got %T", cmd)
	}
	if got.Summary.Loading {
		t.Fatalf("expected Summary.Loading=false when Reader is nil (graceful path)")
	}
}

// TestAkashicEnterSummaryResetsStaleSummaryFields は再突入時の stale データ
// 混入 (前回の Buckets / Err / EmptyMessage が残ること) を防ぐ初期化責務を
// 担保する。
func TestAkashicEnterSummaryResetsStaleSummaryFields(t *testing.T) {
	s := State{
		Subview: SubviewList,
		Reader:  &fakeReader{},
		Summary: SummaryState{
			WorkflowBuckets: []akashic.WorkflowAggregateBucket{{WorkflowID: "stale"}},
			ProviderBuckets: []akashic.ProviderAggregateBucket{{ProviderID: "stale"}},
			Err:             errors.New("stale-err"),
			EmptyMessage:    "stale-empty",
		},
	}
	got, _ := s.enterSummary()
	if len(got.Summary.WorkflowBuckets) != 0 {
		t.Fatalf("expected WorkflowBuckets reset, got %#v", got.Summary.WorkflowBuckets)
	}
	if len(got.Summary.ProviderBuckets) != 0 {
		t.Fatalf("expected ProviderBuckets reset, got %#v", got.Summary.ProviderBuckets)
	}
	if got.Summary.Err != nil {
		t.Fatalf("expected Err reset, got %v", got.Summary.Err)
	}
	if got.Summary.EmptyMessage != "" {
		t.Fatalf("expected EmptyMessage reset, got %q", got.Summary.EmptyMessage)
	}
}

// --- State.Update: aggregate msgs (a) 通常 buckets ---

func TestAkashicUpdateAggregateByWorkflowMsgPopulatesBuckets(t *testing.T) {
	s := makeAkashicSummaryState(&fakeReader{})
	s.Summary.Loading = true

	cost := 0.0123
	dur := int64(1234)
	msg := AggregateByWorkflowMsg{
		Result: akashic.WorkflowAggregateResult{
			Buckets: []akashic.WorkflowAggregateBucket{
				{
					WorkflowID:      "wf-1",
					RunCount:        5,
					SuccessRate:     0.8,
					AvgCostUSD:      &cost,
					P95DurationMS:   &dur,
					SumInputTokens:  1234,
					SumOutputTokens: 567,
				},
			},
		},
	}
	got, cmd := s.Update(msg)

	// Req 4.6 / 11.5: tea.Quit を返さない
	if cmd != nil {
		t.Fatalf("expected nil Cmd from AggregateByWorkflowMsg, got %T", cmd)
	}
	if got.Summary.Loading {
		t.Fatalf("expected Loading=false on success, got true")
	}
	if got.Summary.Err != nil {
		t.Fatalf("expected Err=nil on success, got %v", got.Summary.Err)
	}
	if len(got.Summary.WorkflowBuckets) != 1 || got.Summary.WorkflowBuckets[0].WorkflowID != "wf-1" {
		t.Fatalf("expected WorkflowBuckets populated, got %#v", got.Summary.WorkflowBuckets)
	}
	// 1 件以上の workflow buckets が来たので EmptyMessage はクリア (Provider が空でも片方が non-empty なので Empty 扱いにしない)
	if got.Summary.EmptyMessage != "" {
		t.Fatalf("expected EmptyMessage='' when WorkflowBuckets non-empty, got %q", got.Summary.EmptyMessage)
	}
}

func TestAkashicUpdateAggregateByProviderMsgPopulatesBuckets(t *testing.T) {
	s := makeAkashicSummaryState(&fakeReader{})
	s.Summary.Loading = true

	msg := AggregateByProviderMsg{
		Result: akashic.ProviderAggregateResult{
			Buckets: []akashic.ProviderAggregateBucket{
				{ProviderID: "claude", CallCount: 10, SuccessRate: 0.9},
			},
		},
	}
	got, cmd := s.Update(msg)
	if cmd != nil {
		t.Fatalf("expected nil Cmd, got %T", cmd)
	}
	if got.Summary.Loading {
		t.Fatalf("expected Loading=false, got true")
	}
	if got.Summary.Err != nil {
		t.Fatalf("expected Err=nil, got %v", got.Summary.Err)
	}
	if len(got.Summary.ProviderBuckets) != 1 || got.Summary.ProviderBuckets[0].ProviderID != "claude" {
		t.Fatalf("expected ProviderBuckets populated, got %#v", got.Summary.ProviderBuckets)
	}
}

// --- State.Update: aggregate msgs (b) 空 buckets ---

// TestAkashicUpdateAggregateBothEmptySetsEmptyMessage は両 msg が空 Buckets を
// 返したとき初めて「集計対象 run がありません」を立てることを観測する (Req 4.5)。
//
// 片方だけ空のケース (workflow=空 / provider=非空) では EmptyMessage が
// 立たないことも合わせて確認する: 集計対象 run が 0 でないなら
// 「集計対象 run がありません」とは表示しない。
func TestAkashicUpdateAggregateBothEmptySetsEmptyMessage(t *testing.T) {
	s := makeAkashicSummaryState(&fakeReader{})
	s.Summary.Loading = true

	s, _ = s.Update(AggregateByWorkflowMsg{Result: akashic.WorkflowAggregateResult{Buckets: nil}})
	s, _ = s.Update(AggregateByProviderMsg{Result: akashic.ProviderAggregateResult{Buckets: nil}})

	if s.Summary.EmptyMessage != "集計対象 run がありません" {
		t.Fatalf("expected EmptyMessage='集計対象 run がありません', got %q", s.Summary.EmptyMessage)
	}
	if s.Summary.Err != nil {
		t.Fatalf("expected Err=nil on empty result (Req 4.5), got %v", s.Summary.Err)
	}
	if s.Summary.Loading {
		t.Fatalf("expected Loading=false on empty result, got true")
	}
}

func TestAkashicUpdateAggregateOneSideNonEmptyClearsEmptyMessage(t *testing.T) {
	// 順序:
	//  1. workflow 空 → 一旦 EmptyMessage 立つ (provider 未受信時の暫定状態)
	//  2. provider 非空 → 集計対象 run が存在するため EmptyMessage クリア
	//
	// この挙動は applyAggregate*Msg 内で reconcileEmpty を呼ぶ設計の正当性を
	// 担保する: 片側がデータを持つ場合 EmptyMessage は立てない。
	s := makeAkashicSummaryState(&fakeReader{})
	s.Summary.Loading = true

	s, _ = s.Update(AggregateByWorkflowMsg{Result: akashic.WorkflowAggregateResult{Buckets: nil}})
	if s.Summary.EmptyMessage == "" {
		// 1 段階目時点で workflow=空 / provider=空 なので EmptyMessage 立つ
		t.Fatalf("expected interim EmptyMessage after workflow-empty msg, got empty")
	}

	s, _ = s.Update(AggregateByProviderMsg{
		Result: akashic.ProviderAggregateResult{
			Buckets: []akashic.ProviderAggregateBucket{
				{ProviderID: "claude", CallCount: 1},
			},
		},
	})
	if s.Summary.EmptyMessage != "" {
		t.Fatalf("expected EmptyMessage cleared after non-empty provider msg, got %q", s.Summary.EmptyMessage)
	}
}

// --- State.Update: aggregate msgs (c) error ---

func TestAkashicUpdateAggregateByWorkflowMsgErrorSetsErr(t *testing.T) {
	s := makeAkashicSummaryState(&fakeReader{})
	s.Summary.Loading = true

	wantErr := errors.New("boom agg wf")
	got, cmd := s.Update(AggregateByWorkflowMsg{Err: wantErr})

	// Req 4.6 / 11.5: TUI プロセスを終了させない
	if cmd != nil {
		t.Fatalf("expected nil Cmd on error (must NOT return tea.Quit), got %T", cmd)
	}
	if got.Summary.Loading {
		t.Fatalf("expected Loading=false on error, got true")
	}
	if !errors.Is(got.Summary.Err, wantErr) {
		t.Fatalf("expected Err=%v, got %v", wantErr, got.Summary.Err)
	}
	if got.Summary.EmptyMessage != "" {
		t.Fatalf("expected EmptyMessage='' on error, got %q", got.Summary.EmptyMessage)
	}
}

func TestAkashicUpdateAggregateByProviderMsgErrorSetsErr(t *testing.T) {
	s := makeAkashicSummaryState(&fakeReader{})
	s.Summary.Loading = true

	wantErr := errors.New("boom agg provider")
	got, cmd := s.Update(AggregateByProviderMsg{Err: wantErr})
	if cmd != nil {
		t.Fatalf("expected nil Cmd, got %T", cmd)
	}
	if got.Summary.Loading {
		t.Fatalf("expected Loading=false, got true")
	}
	if !errors.Is(got.Summary.Err, wantErr) {
		t.Fatalf("expected Err=%v, got %v", wantErr, got.Summary.Err)
	}
}

// errReaderUnavailable graceful: List 同様、Err に貯めず View 側で Reader=nil
// を検知して unavailable 表示する (Req 11.2)。
func TestAkashicUpdateAggregateByWorkflowMsgErrReaderUnavailableDoesNotPopulateErr(t *testing.T) {
	s := makeAkashicSummaryState(nil)
	s.Summary.Loading = true
	s.Summary.WorkflowBuckets = []akashic.WorkflowAggregateBucket{{WorkflowID: "stale"}}

	got, cmd := s.Update(AggregateByWorkflowMsg{Err: errReaderUnavailable})
	if cmd != nil {
		t.Fatalf("expected nil Cmd, got %T", cmd)
	}
	if got.Summary.Err != nil {
		t.Fatalf("expected Err=nil on errReaderUnavailable (graceful), got %v", got.Summary.Err)
	}
	if got.Summary.Loading {
		t.Fatalf("expected Loading=false on any aggregate msg arrival, got true")
	}
}

func TestAkashicUpdateAggregateByProviderMsgErrReaderUnavailableDoesNotPopulateErr(t *testing.T) {
	s := makeAkashicSummaryState(nil)
	s.Summary.Loading = true

	got, cmd := s.Update(AggregateByProviderMsg{Err: errReaderUnavailable})
	if cmd != nil {
		t.Fatalf("expected nil Cmd, got %T", cmd)
	}
	if got.Summary.Err != nil {
		t.Fatalf("expected Err=nil on errReaderUnavailable (graceful), got %v", got.Summary.Err)
	}
}

// --- State.View: SubviewSummary ---

// (d) Reader=nil → 「履歴データが利用できません」表示
func TestAkashicSummaryViewReaderNilShowsUnavailable(t *testing.T) {
	s := State{Subview: SubviewSummary, Reader: nil}
	view := s.View(120)
	if !strings.Contains(view, "履歴データが利用できません") {
		t.Fatalf("expected 'unavailable' message, got:\n%s", view)
	}
}

func TestAkashicSummaryViewReaderNilDoesNotShowBuckets(t *testing.T) {
	// 排他: Reader=nil のとき stale な Buckets / EmptyMessage / Err / Loading は描画しない
	s := State{Subview: SubviewSummary, Reader: nil}
	s.Summary.WorkflowBuckets = []akashic.WorkflowAggregateBucket{{WorkflowID: "stale-wf"}}
	s.Summary.ProviderBuckets = []akashic.ProviderAggregateBucket{{ProviderID: "stale-pr"}}
	s.Summary.EmptyMessage = "stale-empty"
	s.Summary.Err = errors.New("stale-err")
	s.Summary.Loading = true

	view := s.View(120)
	if !strings.Contains(view, "履歴データが利用できません") {
		t.Fatalf("expected unavailable message, got:\n%s", view)
	}
	for _, nw := range []string{"stale-wf", "stale-pr", "stale-empty", "stale-err", "読み込み中"} {
		if strings.Contains(view, nw) {
			t.Fatalf("Reader=nil view must not render %q, got:\n%s", nw, view)
		}
	}
}

// Loading
func TestAkashicSummaryViewLoadingDisplayed(t *testing.T) {
	s := makeAkashicSummaryState(&fakeReader{})
	s.Summary.Loading = true
	view := s.View(120)
	if !strings.Contains(view, "読み込み中") {
		t.Fatalf("expected loading indicator in view, got:\n%s", view)
	}
}

// Err
func TestAkashicSummaryViewErrorDisplayed(t *testing.T) {
	s := makeAkashicSummaryState(&fakeReader{})
	s.Summary.Err = errors.New("read failed: connection refused")
	view := s.View(120)
	if !strings.Contains(view, "read failed: connection refused") {
		t.Fatalf("expected error text in view, got:\n%s", view)
	}
}

// Empty
func TestAkashicSummaryViewEmptyMessageDisplayed(t *testing.T) {
	s := makeAkashicSummaryState(&fakeReader{})
	s.Summary.EmptyMessage = "集計対象 run がありません"
	view := s.View(120)
	if !strings.Contains(view, "集計対象 run がありません") {
		t.Fatalf("expected EmptyMessage in view, got:\n%s", view)
	}
}

// (a) 通常 Buckets + ASCII バー (Req 4.1, 4.2, 4.7)
func TestAkashicSummaryViewBucketsRenderedWithASCIIBar(t *testing.T) {
	cost := 0.0123
	dur := int64(1234)
	s := makeAkashicSummaryState(&fakeReader{})
	s.Summary.WorkflowBuckets = []akashic.WorkflowAggregateBucket{
		{
			WorkflowID:      "wf-1",
			RunCount:        5,
			SuccessRate:     0.8,
			AvgCostUSD:      &cost,
			P95DurationMS:   &dur,
			SumInputTokens:  1234,
			SumOutputTokens: 567,
		},
	}
	s.Summary.ProviderBuckets = []akashic.ProviderAggregateBucket{
		{
			ProviderID:      "claude",
			CallCount:       10,
			SuccessRate:     0.9,
			AvgCostUSD:      &cost,
			P95DurationMS:   &dur,
			SumInputTokens:  234,
			SumOutputTokens: 123,
		},
	}
	view := s.View(120)

	// workflow / provider 両方の bucket id が含まれる
	if !strings.Contains(view, "wf-1") {
		t.Fatalf("expected wf-1 in view, got:\n%s", view)
	}
	if !strings.Contains(view, "claude") {
		t.Fatalf("expected claude in view, got:\n%s", view)
	}

	// ASCII バー [####------] 形式 (Req 4.7)
	if !strings.Contains(view, "[") || !strings.Contains(view, "#") || !strings.Contains(view, "]") {
		t.Fatalf("expected ASCII bar with [, #, ], got:\n%s", view)
	}

	// success rate 80% / 90% の数値表示が出ること
	if !strings.Contains(view, "80%") {
		t.Fatalf("expected 80%% success rate, got:\n%s", view)
	}
	if !strings.Contains(view, "90%") {
		t.Fatalf("expected 90%% success rate, got:\n%s", view)
	}

	// run / call count
	if !strings.Contains(view, "5") {
		t.Fatalf("expected RunCount=5 in view, got:\n%s", view)
	}
	if !strings.Contains(view, "10") {
		t.Fatalf("expected CallCount=10 in view, got:\n%s", view)
	}

	// section ヘッダ (workflow / provider 集計)
	if !strings.Contains(view, "Workflow") {
		t.Fatalf("expected 'Workflow' section header, got:\n%s", view)
	}
	if !strings.Contains(view, "Provider") {
		t.Fatalf("expected 'Provider' section header, got:\n%s", view)
	}
}

// ASCII バー長さは画面幅依存で 10〜20 文字 (Req 4.7)
func TestAkashicSummaryASCIIBarLengthDependsOnWidth(t *testing.T) {
	s := makeAkashicSummaryState(&fakeReader{})
	s.Summary.WorkflowBuckets = []akashic.WorkflowAggregateBucket{
		{WorkflowID: "wf-1", RunCount: 1, SuccessRate: 0.5},
	}

	// 各 width で View を呼び、表示される `[` 〜 `]` のバー幅を抽出して長さを確認
	cases := []struct {
		width  int
		minBar int
		maxBar int
	}{
		{30, 10, 10},  // 狭い: 最小 10
		{80, 10, 20},  // 中: 10〜20
		{120, 10, 20}, // 広: 10〜20
	}
	for _, c := range cases {
		view := s.View(c.width)
		barLen := extractFirstBarLength(view)
		if barLen < c.minBar || barLen > c.maxBar {
			t.Fatalf("width=%d: expected ASCII bar inner length in [%d, %d], got %d (view=%q)",
				c.width, c.minBar, c.maxBar, barLen, view)
		}
	}
}

// extractFirstBarLength は view 中の最初の "[...]" バーの内側 (# と - の合計)
// の長さを返す。バーが見つからなければ -1 を返す。
//
// view には他にも "[" 文字が含まれる可能性があるため、最初に "#" or "-" の連続が
// 続く "[" を探す。
func extractFirstBarLength(view string) int {
	for i := 0; i < len(view)-1; i++ {
		if view[i] != '[' {
			continue
		}
		// "[####----]" のような形を探す
		j := i + 1
		for j < len(view) && (view[j] == '#' || view[j] == '-') {
			j++
		}
		if j < len(view) && view[j] == ']' && j > i+1 {
			return j - i - 1
		}
	}
	return -1
}

// 排他: Reader=nil / Loading / Err / EmptyMessage / Buckets の 5 状態が排他的
func TestAkashicSummaryViewExclusiveStates(t *testing.T) {
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
				s.Summary.WorkflowBuckets = []akashic.WorkflowAggregateBucket{{WorkflowID: "stale"}}
				s.Summary.EmptyMessage = "stale-empty"
				s.Summary.Err = errors.New("stale-err")
			},
			want:    "履歴データが利用できません",
			notWant: []string{"stale", "stale-empty", "stale-err", "読み込み中", "集計対象"},
		},
		{
			name: "Loading",
			mutate: func(s *State) {
				s.Summary.Loading = true
			},
			want:    "読み込み中",
			notWant: []string{"履歴データが利用できません", "集計対象 run がありません"},
		},
		{
			name: "Error",
			mutate: func(s *State) {
				s.Summary.Err = errors.New("boom-summary-display")
			},
			want:    "boom-summary-display",
			notWant: []string{"履歴データが利用できません", "集計対象 run がありません", "読み込み中"},
		},
		{
			name: "Empty",
			mutate: func(s *State) {
				s.Summary.EmptyMessage = "集計対象 run がありません"
			},
			want:    "集計対象 run がありません",
			notWant: []string{"履歴データが利用できません", "読み込み中"},
		},
		{
			name: "Buckets",
			mutate: func(s *State) {
				s.Summary.WorkflowBuckets = []akashic.WorkflowAggregateBucket{
					{WorkflowID: "wf-only", RunCount: 1, SuccessRate: 1.0},
				}
			},
			want:    "wf-only",
			notWant: []string{"履歴データが利用できません", "集計対象 run がありません", "読み込み中"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := makeAkashicSummaryState(&fakeReader{})
			c.mutate(&s)
			view := s.View(120)
			if !strings.Contains(view, c.want) {
				t.Fatalf("expected %q in view, got:\n%s", c.want, view)
			}
			for _, nw := range c.notWant {
				if strings.Contains(view, nw) {
					t.Fatalf("did not expect %q in view, got:\n%s", nw, view)
				}
			}
		})
	}
}

// TestAkashicSummaryUpdateNeverReturnsTeaQuit は Update が aggregate 系 msg の
// すべてのケース (通常 / 空 / error / errReaderUnavailable) で tea.Quit を
// 一度も返さないことを観測する (Req 4.6 / 11.5)。
func TestAkashicSummaryUpdateNeverReturnsTeaQuit(t *testing.T) {
	cases := []struct {
		name string
		msg  tea.Msg
	}{
		{"workflow-normal", AggregateByWorkflowMsg{Result: akashic.WorkflowAggregateResult{Buckets: []akashic.WorkflowAggregateBucket{{WorkflowID: "wf-1"}}}}},
		{"workflow-empty", AggregateByWorkflowMsg{Result: akashic.WorkflowAggregateResult{Buckets: nil}}},
		{"workflow-error", AggregateByWorkflowMsg{Err: errors.New("boom-w")}},
		{"workflow-reader-unavailable", AggregateByWorkflowMsg{Err: errReaderUnavailable}},
		{"provider-normal", AggregateByProviderMsg{Result: akashic.ProviderAggregateResult{Buckets: []akashic.ProviderAggregateBucket{{ProviderID: "p"}}}}},
		{"provider-empty", AggregateByProviderMsg{Result: akashic.ProviderAggregateResult{Buckets: nil}}},
		{"provider-error", AggregateByProviderMsg{Err: errors.New("boom-p")}},
		{"provider-reader-unavailable", AggregateByProviderMsg{Err: errReaderUnavailable}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := makeAkashicSummaryState(&fakeReader{})
			_, cmd := s.Update(c.msg)
			if cmd == nil {
				return
			}
			msg := cmd()
			if _, isQuit := msg.(tea.QuitMsg); isQuit {
				t.Fatalf("Update must not return tea.Quit on %s", c.name)
			}
		})
	}
}

// --- ASCII バー描画関数の単体テスト ---

func TestRenderASCIIBarSpec(t *testing.T) {
	cases := []struct {
		rate   float64
		length int
		want   string
	}{
		{0.0, 10, "[----------]"},
		{1.0, 10, "[##########]"},
		{0.5, 10, "[#####-----]"},
		{0.8, 10, "[########--]"},
		{0.0, 20, "[--------------------]"},
		{1.0, 20, "[####################]"},
	}
	for _, c := range cases {
		got := renderASCIIBar(c.rate, c.length)
		if got != c.want {
			t.Fatalf("rate=%g length=%d: expected %q, got %q", c.rate, c.length, c.want, got)
		}
	}
}

// TestRenderASCIIBarClampsLengthAndRate は length / rate が範囲外 (length<10,
// length>20, rate<0, rate>1) のときのクランプ挙動を確認する。
func TestRenderASCIIBarClampsLengthAndRate(t *testing.T) {
	// length < 10 → 10 にクランプ
	got := renderASCIIBar(0.5, 5)
	if len(got) != 12 { // [+10+]
		t.Fatalf("expected length clamped to 10 (output 12 chars), got %q (len=%d)", got, len(got))
	}
	// length > 20 → 20 にクランプ
	got = renderASCIIBar(0.5, 30)
	if len(got) != 22 {
		t.Fatalf("expected length clamped to 20 (output 22 chars), got %q (len=%d)", got, len(got))
	}
	// rate < 0 → 0 として描画
	got = renderASCIIBar(-0.5, 10)
	if !strings.Contains(got, "----------") {
		t.Fatalf("rate<0 should render all '-', got %q", got)
	}
	// rate > 1 → 1 として描画
	got = renderASCIIBar(2.0, 10)
	if !strings.Contains(got, "##########") {
		t.Fatalf("rate>1 should render all '#', got %q", got)
	}
}

// TestRenderASCIIBarPropertyFillRangesValid は property-based に
// 任意の rate / length に対して renderASCIIBar が
//   - 「[」+ length 個の "#" or "-" + 「]」 という形式
//   - 「#」の数 ≦ length、「-」の数 ≦ length、「#」+「-」 = length
//
// を満たすことを観測する (回帰テスト: float64 演算の丸めで length 超過しない)。
func TestRenderASCIIBarPropertyFillRangesValid(t *testing.T) {
	prop := func(rate float64, length int) bool {
		got := renderASCIIBar(rate, length)
		// length クランプ後の値で再評価
		clamped := length
		if clamped < 10 {
			clamped = 10
		}
		if clamped > 20 {
			clamped = 20
		}
		if len(got) != clamped+2 { // [...]
			return false
		}
		if got[0] != '[' || got[len(got)-1] != ']' {
			return false
		}
		hashes := strings.Count(got, "#")
		dashes := strings.Count(got, "-")
		return hashes+dashes == clamped
	}
	if err := quick.Check(prop, nil); err != nil {
		t.Fatalf("property check failed: %v", err)
	}
}
