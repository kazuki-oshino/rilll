// Package ui のアカシックモード Run 詳細サブビュー (state) に対するテスト
// (akashic-mode-tui spec Task 6.1)。
//
// 本ファイルは以下を観測する:
//   - State.enterDetail / Update の RunDetailMsg 受信処理
//     (a) 通常 RunDetail / (b) ErrWorkflowRunNotFound / (c) 一般 error /
//     (d) Reader=nil の 4 ケースで DetailState の各フィールドが
//     要件通りに排他的にセットされること (Req 5.1〜5.5 / 11.2 / 11.5)
//   - raw_events ページ遷移 (n / p キー):
//     Offset=0 → Offset=DefaultRawEventsLimit の 2 回フェッチ、Items 差し替え
//     (累積しない、Req 5.3)
//   - Detail フォーカス遷移 (Tab / Shift+Tab) と
//     Focus に応じたカーソル移動 (j / k / ↑ / ↓):
//     AgentCallsCursor / RawEventsCursor が期待通り遷移すること (Task 6.1)
//   - tea.Quit を一切返さないこと (Req 5.5 / 11.5)
//   - Reader=nil 時に Reader メソッドが呼ばれないこと (Req 11.2)
//
// 描画 (View) は Task 6.3 で実装するため、本ファイルでは検証しない。
package akashic

import (
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

// makeAkashicDetailState は SubviewDetail で初期化した State を返すヘルパ。
// reader が nil でも graceful に初期化される。
func makeAkashicDetailState(reader akashic.Reader) State {
	return State{
		Subview: SubviewDetail,
		Reader:  reader,
		Detail: DetailState{
			RunID: "run-1",
			Focus: DetailFocusHeader,
		},
	}
}

// --- State.enterDetail ---

// TestAkashicEnterDetailIssuesRunDetailCmdWithDefaultRawEventsLimit は
// SubviewDetail に切替時に RunDetailCmd を
// `DetailOptions{WorkflowRunID, RawEvents:{Limit: DefaultRawEventsLimit, Offset: 0}}`
// で発行することを観測する (Task 6.1, Req 5.1)。
func TestAkashicEnterDetailIssuesRunDetailCmdWithDefaultRawEventsLimit(t *testing.T) {
	reader := &fakeReader{}
	s := State{
		Subview: SubviewList,
		Reader:  reader,
	}

	got, cmd := s.enterDetail("run-7")
	if got.Subview != SubviewDetail {
		t.Fatalf("expected Subview=SubviewDetail, got %q", got.Subview)
	}
	if got.Detail.RunID != "run-7" {
		t.Fatalf("expected Detail.RunID=run-7, got %q", got.Detail.RunID)
	}
	if got.Detail.Focus != DetailFocusAgentCalls {
		t.Fatalf("expected Detail.Focus=AgentCalls on entry, got %q", got.Detail.Focus)
	}
	if !got.Detail.Loading {
		t.Fatalf("expected Detail.Loading=true on enter (Reader != nil)")
	}
	if cmd == nil {
		t.Fatal("expected non-nil Cmd on enter when Reader != nil")
	}

	// Cmd を invoke して fakeReader への入力を観測
	msg := cmd()
	if _, ok := msg.(RunDetailMsg); !ok {
		t.Fatalf("expected RunDetailMsg from cmd(), got %T", msg)
	}
	if reader.detailCalls != 1 {
		t.Fatalf("expected Detail to be called once, got %d", reader.detailCalls)
	}
	if reader.detailOpts.WorkflowRunID != "run-7" {
		t.Fatalf("expected WorkflowRunID=run-7, got %q", reader.detailOpts.WorkflowRunID)
	}
	if reader.detailOpts.RawEvents.Limit != akashic.DefaultRawEventsLimit {
		t.Fatalf("expected RawEvents.Limit=%d, got %d", akashic.DefaultRawEventsLimit, reader.detailOpts.RawEvents.Limit)
	}
	if reader.detailOpts.RawEvents.Offset != 0 {
		t.Fatalf("expected RawEvents.Offset=0 on first fetch, got %d", reader.detailOpts.RawEvents.Offset)
	}
}

// TestAkashicEnterDetailReaderNilDoesNotIssueCmd は (d) Reader=nil 時に
// Cmd が一切発行されない (Req 11.2) ことと Loading=true にも遷移しないことを観測する。
func TestAkashicEnterDetailReaderNilDoesNotIssueCmd(t *testing.T) {
	s := State{Subview: SubviewList, Reader: nil}
	got, cmd := s.enterDetail("run-1")
	if got.Subview != SubviewDetail {
		t.Fatalf("expected Subview=SubviewDetail, got %q", got.Subview)
	}
	if cmd != nil {
		t.Fatalf("expected nil Cmd when Reader is nil (Req 11.2), got %T", cmd)
	}
	if got.Detail.Loading {
		t.Fatalf("expected Detail.Loading=false when Reader is nil (graceful path)")
	}
	if got.Detail.RunID != "run-1" {
		t.Fatalf("expected Detail.RunID=run-1 even when Reader=nil, got %q", got.Detail.RunID)
	}
}

// TestAkashicEnterDetailResetsStaleDetailFields は再突入時の stale データ
// 混入 (前回の Detail / Err / カーソル / ページ Offset / HandoffErr が残ること) を
// 防ぐ初期化責務を担保する。
func TestAkashicEnterDetailResetsStaleDetailFields(t *testing.T) {
	stale := akashic.RunDetail{Run: akashic.RunHeader{RunID: "stale-run"}}
	staleNext := 200
	s := State{
		Subview: SubviewList,
		Reader:  &fakeReader{},
		Detail: DetailState{
			RunID:               "stale-run",
			Detail:              &stale,
			RawEventsOffset:     100,
			NextRawEventsOffset: &staleNext,
			Err:                 errors.New("stale-err"),
			HandoffErr:          errors.New("stale-handoff-err"),
			Focus:               DetailFocusRawEvents,
			AgentCallsCursor:    7,
			RawEventsCursor:     9,
		},
	}
	got, _ := s.enterDetail("run-new")
	if got.Detail.RunID != "run-new" {
		t.Fatalf("expected RunID=run-new, got %q", got.Detail.RunID)
	}
	if got.Detail.Detail != nil {
		t.Fatalf("expected Detail.Detail reset to nil, got %#v", got.Detail.Detail)
	}
	if got.Detail.RawEventsOffset != 0 {
		t.Fatalf("expected RawEventsOffset reset to 0, got %d", got.Detail.RawEventsOffset)
	}
	if got.Detail.NextRawEventsOffset != nil {
		t.Fatalf("expected NextRawEventsOffset reset to nil, got %v", got.Detail.NextRawEventsOffset)
	}
	if got.Detail.Err != nil {
		t.Fatalf("expected Err reset, got %v", got.Detail.Err)
	}
	if got.Detail.HandoffErr != nil {
		t.Fatalf("expected HandoffErr reset, got %v", got.Detail.HandoffErr)
	}
	if got.Detail.Focus != DetailFocusAgentCalls {
		t.Fatalf("expected Focus reset to AgentCalls, got %q", got.Detail.Focus)
	}
	if got.Detail.AgentCallsCursor != 0 {
		t.Fatalf("expected AgentCallsCursor reset to 0, got %d", got.Detail.AgentCallsCursor)
	}
	if got.Detail.RawEventsCursor != 0 {
		t.Fatalf("expected RawEventsCursor reset to 0, got %d", got.Detail.RawEventsCursor)
	}
}

// --- State.Update: RunDetailMsg ---

// TestAkashicDetailUpdateNormalRunDetailPopulatesDetail は (a) 通常 RunDetail を
// 受信したときに Detail / RawEventsOffset / NextRawEventsOffset / Loading=false が
// 反映され、Err は無いことを観測する。
func TestAkashicDetailUpdateNormalRunDetailPopulatesDetail(t *testing.T) {
	reader := &fakeReader{}
	s := makeAkashicDetailState(reader)
	s.Detail.Loading = true

	next := 100
	now := time.Now()
	detail := akashic.RunDetail{
		Run: akashic.RunHeader{RunID: "run-1", WorkflowID: "wf-1", StartedAt: now},
		AgentCalls: []akashic.AgentCallRow{
			{ID: "ac-1", WorkflowRunID: "run-1", StartedAt: now},
			{ID: "ac-2", WorkflowRunID: "run-1", StartedAt: now.Add(time.Second)},
		},
		RawEvents: []akashic.RawEventRow{
			{ID: 1, WorkflowRunID: "run-1", Sequence: 1, ObservedAt: now},
			{ID: 2, WorkflowRunID: "run-1", Sequence: 2, ObservedAt: now},
		},
		RawEventsLimit:      akashic.DefaultRawEventsLimit,
		RawEventsOffset:     0,
		NextRawEventsOffset: &next,
	}
	got, cmd := s.Update(RunDetailMsg{Detail: detail})

	// Req 5.5 / 11.5: tea.Quit を返さない
	if cmd != nil {
		t.Fatalf("expected nil Cmd from RunDetailMsg, got %T", cmd)
	}
	if got.Detail.Loading {
		t.Fatalf("expected Loading=false on success, got true")
	}
	if got.Detail.Err != nil {
		t.Fatalf("expected Err=nil on success, got %v", got.Detail.Err)
	}
	if got.Detail.Detail == nil {
		t.Fatalf("expected Detail.Detail to be populated, got nil")
	}
	if got.Detail.Detail.Run.RunID != "run-1" {
		t.Fatalf("expected Detail.Run.RunID=run-1, got %q", got.Detail.Detail.Run.RunID)
	}
	if len(got.Detail.Detail.AgentCalls) != 2 {
		t.Fatalf("expected 2 agent_calls, got %d", len(got.Detail.Detail.AgentCalls))
	}
	if len(got.Detail.Detail.RawEvents) != 2 {
		t.Fatalf("expected 2 raw_events, got %d", len(got.Detail.Detail.RawEvents))
	}
	if got.Detail.RawEventsOffset != 0 {
		t.Fatalf("expected RawEventsOffset=0, got %d", got.Detail.RawEventsOffset)
	}
	if got.Detail.NextRawEventsOffset == nil || *got.Detail.NextRawEventsOffset != 100 {
		t.Fatalf("expected NextRawEventsOffset=100, got %v", got.Detail.NextRawEventsOffset)
	}
}

// TestAkashicDetailUpdateErrWorkflowRunNotFoundSetsErr は (b)
// ErrWorkflowRunNotFound を受信したときに Err に格納され、Detail は更新されない
// ことを観測する (Req 5.4)。errors.Is で判別可能でなければならない。
func TestAkashicDetailUpdateErrWorkflowRunNotFoundSetsErr(t *testing.T) {
	reader := &fakeReader{}
	s := makeAkashicDetailState(reader)
	s.Detail.Loading = true

	got, cmd := s.Update(RunDetailMsg{Err: akashic.ErrWorkflowRunNotFound})

	// Req 5.5 / 11.5: tea.Quit を返さない
	if cmd != nil {
		t.Fatalf("expected nil Cmd (must NOT return tea.Quit), got %T", cmd)
	}
	if got.Detail.Loading {
		t.Fatalf("expected Loading=false on error, got true")
	}
	if !errors.Is(got.Detail.Err, akashic.ErrWorkflowRunNotFound) {
		t.Fatalf("expected Err matches ErrWorkflowRunNotFound (Req 5.4), got %v", got.Detail.Err)
	}
	if got.Detail.Detail != nil {
		t.Fatalf("expected Detail.Detail=nil on not-found, got %#v", got.Detail.Detail)
	}
}

// TestAkashicDetailUpdateGenericErrorSetsErrAndDoesNotQuit は (c) 一般 error を
// 受信したときに Err に格納され、tea.Quit を返さないことを観測する (Req 5.5)。
func TestAkashicDetailUpdateGenericErrorSetsErrAndDoesNotQuit(t *testing.T) {
	reader := &fakeReader{}
	s := makeAkashicDetailState(reader)
	s.Detail.Loading = true

	wantErr := errors.New("boom detail")
	got, cmd := s.Update(RunDetailMsg{Err: wantErr})

	if cmd != nil {
		t.Fatalf("expected nil Cmd (must NOT return tea.Quit), got %T", cmd)
	}
	if got.Detail.Loading {
		t.Fatalf("expected Loading=false on error, got true")
	}
	if !errors.Is(got.Detail.Err, wantErr) {
		t.Fatalf("expected Err=%v, got %v", wantErr, got.Detail.Err)
	}
}

// TestAkashicDetailUpdateErrReaderUnavailableDoesNotPopulateErr は
// errReaderUnavailable sentinel が Err に貯まらず graceful 経路で扱われる
// ことを観測する (Req 11.2)。View 側で s.Reader == nil を検知して
// 「履歴データが利用できません」を表示するため、Err に貯めない。
func TestAkashicDetailUpdateErrReaderUnavailableDoesNotPopulateErr(t *testing.T) {
	s := makeAkashicDetailState(nil)
	s.Detail.Loading = true
	stale := akashic.RunDetail{Run: akashic.RunHeader{RunID: "stale"}}
	s.Detail.Detail = &stale

	got, cmd := s.Update(RunDetailMsg{Err: errReaderUnavailable})
	if cmd != nil {
		t.Fatalf("expected nil Cmd, got %T", cmd)
	}
	if got.Detail.Err != nil {
		t.Fatalf("expected Err=nil on errReaderUnavailable (graceful), got %v", got.Detail.Err)
	}
	if got.Detail.Loading {
		t.Fatalf("expected Loading=false on any RunDetailMsg arrival, got true")
	}
	// 既存 Detail は Reader 不在 graceful では変更しない (View が Reader=nil で
	// 描画分岐するため Detail に手は加えない)
	if got.Detail.Detail != &stale {
		t.Fatalf("expected stale Detail unchanged on errReaderUnavailable, got %#v", got.Detail.Detail)
	}
}

// TestAkashicDetailUpdateClampsCursorOnShorterPage は applyRunDetailMsg が
// 新ページの items 件数に応じて AgentCallsCursor / RawEventsCursor を
// クランプすることを観測する (out-of-range panic 防止)。
func TestAkashicDetailUpdateClampsCursorOnShorterPage(t *testing.T) {
	s := makeAkashicDetailState(&fakeReader{})
	s.Detail.AgentCallsCursor = 10
	s.Detail.RawEventsCursor = 10

	now := time.Now()
	detail := akashic.RunDetail{
		Run: akashic.RunHeader{RunID: "run-1"},
		AgentCalls: []akashic.AgentCallRow{
			{ID: "ac-1", StartedAt: now},
			{ID: "ac-2", StartedAt: now},
			{ID: "ac-3", StartedAt: now},
		},
		RawEvents: []akashic.RawEventRow{
			{ID: 1, Sequence: 1, ObservedAt: now},
		},
	}
	got, _ := s.Update(RunDetailMsg{Detail: detail})
	if got.Detail.AgentCallsCursor != 2 {
		t.Fatalf("expected AgentCallsCursor clamped to 2, got %d", got.Detail.AgentCallsCursor)
	}
	if got.Detail.RawEventsCursor != 0 {
		t.Fatalf("expected RawEventsCursor clamped to 0 (single item), got %d", got.Detail.RawEventsCursor)
	}
}

// TestAkashicDetailUpdateClampsCursorOnEmptyPage は items が空のページを
// 受信したときカーソルが 0 にクランプされることを観測する。
func TestAkashicDetailUpdateClampsCursorOnEmptyPage(t *testing.T) {
	s := makeAkashicDetailState(&fakeReader{})
	s.Detail.AgentCallsCursor = 5
	s.Detail.RawEventsCursor = 5

	detail := akashic.RunDetail{
		Run:        akashic.RunHeader{RunID: "run-1"},
		AgentCalls: nil,
		RawEvents:  nil,
	}
	got, _ := s.Update(RunDetailMsg{Detail: detail})
	if got.Detail.AgentCallsCursor != 0 {
		t.Fatalf("expected AgentCallsCursor=0 on empty page, got %d", got.Detail.AgentCallsCursor)
	}
	if got.Detail.RawEventsCursor != 0 {
		t.Fatalf("expected RawEventsCursor=0 on empty page, got %d", got.Detail.RawEventsCursor)
	}
}

// --- raw_events ページング (n / p キー) ---

// TestAkashicDetailRawEventsPagingNextThenPrev は raw_events ページングが
// (1) n キーで Offset=0 → DefaultRawEventsLimit に進む、
// (2) ページ単位で Items が差し替わる (累積しない、Req 5.3 後段)、
// (3) p キーで前ページに戻る、
// を観測する。
func TestAkashicDetailRawEventsPagingNextThenPrev(t *testing.T) {
	reader := &fakeReader{}
	s := makeAkashicDetailState(reader)

	// 初期状態: Offset=0 のページが取得済み (NextRawEventsOffset で次ページ存在を示す)
	next := akashic.DefaultRawEventsLimit
	now := time.Now()
	page0Events := []akashic.RawEventRow{
		{ID: 1, Sequence: 1, ObservedAt: now},
		{ID: 2, Sequence: 2, ObservedAt: now},
	}
	s.Detail.Detail = &akashic.RunDetail{
		Run:                 akashic.RunHeader{RunID: "run-1"},
		RawEvents:           page0Events,
		RawEventsLimit:      akashic.DefaultRawEventsLimit,
		RawEventsOffset:     0,
		NextRawEventsOffset: &next,
	}
	s.Detail.RawEventsOffset = 0
	s.Detail.NextRawEventsOffset = &next

	// (1) n キーで次ページを取得
	got, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if cmd == nil {
		t.Fatal("expected non-nil Cmd from 'n' when NextRawEventsOffset != nil")
	}
	if !got.Detail.Loading {
		t.Fatalf("expected Loading=true while fetching next page")
	}
	if got.Detail.RawEventsCursor != 0 {
		t.Fatalf("expected RawEventsCursor reset to 0 on next-page, got %d", got.Detail.RawEventsCursor)
	}
	// Cmd を invoke して fakeReader への入力を観測 (Offset=DefaultRawEventsLimit)
	msg := cmd()
	if _, ok := msg.(RunDetailMsg); !ok {
		t.Fatalf("expected RunDetailMsg from cmd(), got %T", msg)
	}
	if reader.detailCalls != 1 {
		t.Fatalf("expected Detail to be called once (next-page fetch), got %d", reader.detailCalls)
	}
	if reader.detailOpts.RawEvents.Offset != akashic.DefaultRawEventsLimit {
		t.Fatalf("expected RawEvents.Offset=%d on next-page fetch, got %d",
			akashic.DefaultRawEventsLimit, reader.detailOpts.RawEvents.Offset)
	}
	if reader.detailOpts.RawEvents.Limit != akashic.DefaultRawEventsLimit {
		t.Fatalf("expected RawEvents.Limit=%d, got %d",
			akashic.DefaultRawEventsLimit, reader.detailOpts.RawEvents.Limit)
	}

	// (2) 次ページ RunDetailMsg を applyRunDetailMsg に投入 → Items 差し替え
	page1Events := []akashic.RawEventRow{
		{ID: 101, Sequence: 101, ObservedAt: now},
		{ID: 102, Sequence: 102, ObservedAt: now},
	}
	page1Detail := akashic.RunDetail{
		Run:                 akashic.RunHeader{RunID: "run-1"},
		RawEvents:           page1Events,
		RawEventsLimit:      akashic.DefaultRawEventsLimit,
		RawEventsOffset:     akashic.DefaultRawEventsLimit,
		NextRawEventsOffset: nil, // 末尾ページ
	}
	got, _ = got.Update(RunDetailMsg{Detail: page1Detail})
	if got.Detail.Detail == nil {
		t.Fatal("expected Detail.Detail populated after next-page msg")
	}
	if len(got.Detail.Detail.RawEvents) != 2 {
		t.Fatalf("expected 2 raw_events on page1 (replaced, not accumulated), got %d", len(got.Detail.Detail.RawEvents))
	}
	if got.Detail.Detail.RawEvents[0].ID != 101 {
		// 累積していたら ID=1 から並ぶ。差し替えなら ID=101 が先頭。
		t.Fatalf("expected raw_events to be replaced (ID=101 first), got ID=%d", got.Detail.Detail.RawEvents[0].ID)
	}
	if got.Detail.RawEventsOffset != akashic.DefaultRawEventsLimit {
		t.Fatalf("expected RawEventsOffset=%d after page1 msg, got %d",
			akashic.DefaultRawEventsLimit, got.Detail.RawEventsOffset)
	}
	if got.Detail.NextRawEventsOffset != nil {
		t.Fatalf("expected NextRawEventsOffset=nil at last page, got %v", got.Detail.NextRawEventsOffset)
	}

	// (3) p キーで前ページに戻る (Offset=DefaultRawEventsLimit → 0)
	got2, cmd2 := got.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if cmd2 == nil {
		t.Fatal("expected non-nil Cmd from 'p' when RawEventsOffset > 0")
	}
	if !got2.Detail.Loading {
		t.Fatalf("expected Loading=true while fetching prev page")
	}
	if got2.Detail.RawEventsCursor != 0 {
		t.Fatalf("expected RawEventsCursor reset to 0 on prev-page, got %d", got2.Detail.RawEventsCursor)
	}
	_ = cmd2() // Cmd を invoke して fakeReader への入力を更新
	if reader.detailCalls != 2 {
		t.Fatalf("expected Detail to be called twice (next + prev), got %d", reader.detailCalls)
	}
	if reader.detailOpts.RawEvents.Offset != 0 {
		t.Fatalf("expected RawEvents.Offset=0 on prev-page fetch, got %d", reader.detailOpts.RawEvents.Offset)
	}
}

// TestAkashicDetailRawEventsNextNoOpWhenNoNextPage は NextRawEventsOffset=nil
// (= 末尾ページ) のとき n キーが no-op で Cmd を発行しないことを観測する。
func TestAkashicDetailRawEventsNextNoOpWhenNoNextPage(t *testing.T) {
	reader := &fakeReader{}
	s := makeAkashicDetailState(reader)
	s.Detail.NextRawEventsOffset = nil
	s.Detail.RawEventsOffset = 0

	got, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if cmd != nil {
		t.Fatalf("expected nil Cmd when no next page, got %T", cmd)
	}
	if got.Detail.Loading {
		t.Fatalf("expected Loading=false on no-op next, got true")
	}
	if reader.detailCalls != 0 {
		t.Fatalf("expected Reader.Detail not called on no-op, got %d", reader.detailCalls)
	}
}

// TestAkashicDetailRawEventsPrevNoOpAtFirstPage は RawEventsOffset==0
// (= 先頭ページ) のとき p キーが no-op で Cmd を発行しないことを観測する。
func TestAkashicDetailRawEventsPrevNoOpAtFirstPage(t *testing.T) {
	reader := &fakeReader{}
	s := makeAkashicDetailState(reader)
	s.Detail.RawEventsOffset = 0

	got, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	if cmd != nil {
		t.Fatalf("expected nil Cmd at first page, got %T", cmd)
	}
	if got.Detail.Loading {
		t.Fatalf("expected Loading=false on no-op prev, got true")
	}
	if reader.detailCalls != 0 {
		t.Fatalf("expected Reader.Detail not called on no-op, got %d", reader.detailCalls)
	}
}

// TestAkashicDetailRawEventsPagingReaderNilNoOp は Reader=nil のときに
// n / p キーが Cmd を発行しないことを観測する (Req 11.2 graceful)。
func TestAkashicDetailRawEventsPagingReaderNilNoOp(t *testing.T) {
	s := makeAkashicDetailState(nil)
	next := 100
	s.Detail.NextRawEventsOffset = &next
	s.Detail.RawEventsOffset = 100

	for _, key := range []rune{'n', 'p'} {
		_, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{key}})
		if cmd != nil {
			t.Fatalf("expected nil Cmd for key=%q when Reader=nil (Req 11.2), got %T", string(key), cmd)
		}
	}
}

// --- フォーカス遷移 (Tab / Shift+Tab) ---

// TestAkashicDetailFocusTabCyclesForward は Tab キーで Focus が
// AgentCalls → RawEvents → Artifacts → AgentCalls と循環することを観測する。
// Header は workflow_run の見出し 1 行のみで navigation 対象がないため
// 循環には含まれない (詳細画面に入った瞬間から j/k/Enter が反応する UX)。
func TestAkashicDetailFocusTabCyclesForward(t *testing.T) {
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
	}
}

// TestAkashicDetailFocusShiftTabCyclesBackward は Shift+Tab キーで Focus が
// AgentCalls → Artifacts → RawEvents → AgentCalls と逆循環することを観測する。
// Header は循環外 (TestAkashicDetailFocusTabCyclesForward と同じ理由)。
func TestAkashicDetailFocusShiftTabCyclesBackward(t *testing.T) {
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
	}
}

// --- カーソル移動 (j / k / ↑ / ↓) ---

// TestAkashicDetailCursorMoveOnAgentCallsFocus は Focus=AgentCalls のとき
// j / k / ↑ / ↓ で AgentCallsCursor のみが動き、RawEventsCursor は不変なことを観測する。
func TestAkashicDetailCursorMoveOnAgentCallsFocus(t *testing.T) {
	s := makeAkashicDetailState(&fakeReader{})
	now := time.Now()
	s.Detail.Detail = &akashic.RunDetail{
		Run: akashic.RunHeader{RunID: "run-1"},
		AgentCalls: []akashic.AgentCallRow{
			{ID: "ac-1", StartedAt: now},
			{ID: "ac-2", StartedAt: now},
			{ID: "ac-3", StartedAt: now},
		},
		RawEvents: []akashic.RawEventRow{
			{ID: 1, Sequence: 1, ObservedAt: now},
			{ID: 2, Sequence: 2, ObservedAt: now},
		},
	}
	s.Detail.Focus = DetailFocusAgentCalls
	s.Detail.AgentCallsCursor = 0
	s.Detail.RawEventsCursor = 1

	// j で 0 → 1
	s, _ = s.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if s.Detail.AgentCallsCursor != 1 {
		t.Fatalf("expected AgentCallsCursor=1 after j, got %d", s.Detail.AgentCallsCursor)
	}
	if s.Detail.RawEventsCursor != 1 {
		t.Fatalf("expected RawEventsCursor unchanged at 1 (Focus=AgentCalls), got %d", s.Detail.RawEventsCursor)
	}
	// ↓ で 1 → 2
	s, _ = s.HandleKey(tea.KeyMsg{Type: tea.KeyDown})
	if s.Detail.AgentCallsCursor != 2 {
		t.Fatalf("expected AgentCallsCursor=2 after Down, got %d", s.Detail.AgentCallsCursor)
	}
	// ↓ で 2 → 2 (末尾でクランプ)
	s, _ = s.HandleKey(tea.KeyMsg{Type: tea.KeyDown})
	if s.Detail.AgentCallsCursor != 2 {
		t.Fatalf("expected AgentCallsCursor clamped to 2 at end, got %d", s.Detail.AgentCallsCursor)
	}
	// k で 2 → 1
	s, _ = s.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if s.Detail.AgentCallsCursor != 1 {
		t.Fatalf("expected AgentCallsCursor=1 after k, got %d", s.Detail.AgentCallsCursor)
	}
	// ↑↑↑ で 1 → 0 (先頭でクランプ)
	for i := 0; i < 3; i++ {
		s, _ = s.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	}
	if s.Detail.AgentCallsCursor != 0 {
		t.Fatalf("expected AgentCallsCursor clamped to 0 at start, got %d", s.Detail.AgentCallsCursor)
	}
}

// TestAkashicDetailCursorMoveOnRawEventsFocus は Focus=RawEvents のとき
// j / k / ↑ / ↓ で RawEventsCursor のみが動き、AgentCallsCursor は不変なことを観測する。
func TestAkashicDetailCursorMoveOnRawEventsFocus(t *testing.T) {
	s := makeAkashicDetailState(&fakeReader{})
	now := time.Now()
	s.Detail.Detail = &akashic.RunDetail{
		Run: akashic.RunHeader{RunID: "run-1"},
		AgentCalls: []akashic.AgentCallRow{
			{ID: "ac-1", StartedAt: now},
			{ID: "ac-2", StartedAt: now},
		},
		RawEvents: []akashic.RawEventRow{
			{ID: 1, Sequence: 1, ObservedAt: now},
			{ID: 2, Sequence: 2, ObservedAt: now},
			{ID: 3, Sequence: 3, ObservedAt: now},
		},
	}
	s.Detail.Focus = DetailFocusRawEvents
	s.Detail.AgentCallsCursor = 1
	s.Detail.RawEventsCursor = 0

	// ↓ で 0 → 1
	s, _ = s.HandleKey(tea.KeyMsg{Type: tea.KeyDown})
	if s.Detail.RawEventsCursor != 1 {
		t.Fatalf("expected RawEventsCursor=1 after Down, got %d", s.Detail.RawEventsCursor)
	}
	if s.Detail.AgentCallsCursor != 1 {
		t.Fatalf("expected AgentCallsCursor unchanged at 1 (Focus=RawEvents), got %d", s.Detail.AgentCallsCursor)
	}
	// j で 1 → 2
	s, _ = s.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if s.Detail.RawEventsCursor != 2 {
		t.Fatalf("expected RawEventsCursor=2 after j, got %d", s.Detail.RawEventsCursor)
	}
	// ↑ で 2 → 1
	s, _ = s.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	if s.Detail.RawEventsCursor != 1 {
		t.Fatalf("expected RawEventsCursor=1 after Up, got %d", s.Detail.RawEventsCursor)
	}
}

// TestAkashicDetailCursorMoveOnHeaderFocusIsNoop は Focus=Header のとき
// カーソルキーが no-op (どのカーソルも動かない) ことを観測する。
// Header は単一行のためカーソル概念がない。
func TestAkashicDetailCursorMoveOnHeaderFocusIsNoop(t *testing.T) {
	s := makeAkashicDetailState(&fakeReader{})
	now := time.Now()
	s.Detail.Detail = &akashic.RunDetail{
		Run: akashic.RunHeader{RunID: "run-1"},
		AgentCalls: []akashic.AgentCallRow{
			{ID: "ac-1", StartedAt: now}, {ID: "ac-2", StartedAt: now},
		},
		RawEvents: []akashic.RawEventRow{
			{ID: 1, Sequence: 1, ObservedAt: now}, {ID: 2, Sequence: 2, ObservedAt: now},
		},
	}
	s.Detail.Focus = DetailFocusHeader
	s.Detail.AgentCallsCursor = 1
	s.Detail.RawEventsCursor = 1

	// j / k / Down / Up すべて no-op
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'j'}},
		{Type: tea.KeyRunes, Runes: []rune{'k'}},
		{Type: tea.KeyDown},
		{Type: tea.KeyUp},
	} {
		s, _ = s.HandleKey(key)
	}
	if s.Detail.AgentCallsCursor != 1 {
		t.Fatalf("expected AgentCallsCursor unchanged on Header focus, got %d", s.Detail.AgentCallsCursor)
	}
	if s.Detail.RawEventsCursor != 1 {
		t.Fatalf("expected RawEventsCursor unchanged on Header focus, got %d", s.Detail.RawEventsCursor)
	}
}

// TestAkashicDetailCursorMoveBeforeDetailLoadedIsNoop は Detail が未取得 (nil)
// のときカーソルキーが no-op であることを観測する (nil pointer dereference 防止)。
func TestAkashicDetailCursorMoveBeforeDetailLoadedIsNoop(t *testing.T) {
	s := makeAkashicDetailState(&fakeReader{})
	s.Detail.Detail = nil
	s.Detail.Focus = DetailFocusAgentCalls

	got, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if cmd != nil {
		t.Fatalf("expected nil Cmd, got %T", cmd)
	}
	if got.Detail.AgentCallsCursor != 0 {
		t.Fatalf("expected AgentCallsCursor=0 (no-op when Detail=nil), got %d", got.Detail.AgentCallsCursor)
	}
}

// --- tea.Quit を返さないことの包括検査 (Req 5.5 / 11.5) ---

// TestAkashicDetailUpdateNeverReturnsTeaQuit は Update が RunDetailMsg の
// すべてのケース (通常 / ErrWorkflowRunNotFound / 一般 error / errReaderUnavailable)
// で tea.Quit を一度も返さないことを観測する。
func TestAkashicDetailUpdateNeverReturnsTeaQuit(t *testing.T) {
	cases := []struct {
		name string
		msg  RunDetailMsg
	}{
		{"normal", RunDetailMsg{Detail: akashic.RunDetail{Run: akashic.RunHeader{RunID: "r"}}}},
		{"not-found", RunDetailMsg{Err: akashic.ErrWorkflowRunNotFound}},
		{"generic-error", RunDetailMsg{Err: errors.New("boom")}},
		{"reader-unavailable", RunDetailMsg{Err: errReaderUnavailable}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := makeAkashicDetailState(&fakeReader{})
			_, cmd := s.Update(c.msg)
			if cmd == nil {
				return
			}
			gotMsg := cmd()
			if _, isQuit := gotMsg.(tea.QuitMsg); isQuit {
				t.Fatalf("Update must not return tea.Quit on %s", c.name)
			}
		})
	}
}

// TestAkashicDetailHandleKeyNeverReturnsTeaQuit は HandleKey が
// 想定キー (Tab / Shift+Tab / j / k / ↑ / ↓ / n / p / その他) で
// tea.Quit を一度も返さないことを観測する。
func TestAkashicDetailHandleKeyNeverReturnsTeaQuit(t *testing.T) {
	next := akashic.DefaultRawEventsLimit
	now := time.Now()

	build := func() State {
		s := makeAkashicDetailState(&fakeReader{})
		s.Detail.Detail = &akashic.RunDetail{
			Run: akashic.RunHeader{RunID: "run-1"},
			AgentCalls: []akashic.AgentCallRow{
				{ID: "ac-1", StartedAt: now},
			},
			RawEvents: []akashic.RawEventRow{
				{ID: 1, Sequence: 1, ObservedAt: now},
			},
		}
		s.Detail.NextRawEventsOffset = &next
		s.Detail.RawEventsOffset = 0
		return s
	}

	cases := []struct {
		name string
		key  tea.KeyMsg
	}{
		{"tab", tea.KeyMsg{Type: tea.KeyTab}},
		{"shift+tab", tea.KeyMsg{Type: tea.KeyShiftTab}},
		{"j", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}}},
		{"k", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}}},
		{"down", tea.KeyMsg{Type: tea.KeyDown}},
		{"up", tea.KeyMsg{Type: tea.KeyUp}},
		{"n", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}}},
		{"p", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}}},
		{"unknown", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := build()
			_, cmd := s.HandleKey(c.key)
			if cmd == nil {
				return
			}
			gotMsg := cmd()
			if _, isQuit := gotMsg.(tea.QuitMsg); isQuit {
				t.Fatalf("HandleKey must not return tea.Quit on key=%s", c.name)
			}
		})
	}
}

// TestAkashicDetailHandleKeyOnlyDispatchedOnSubviewDetail は
// Subview != SubviewDetail のとき HandleKey が Detail 経路を
// 走らせない (Detail.Focus を変えない) ことを観測する (Subview ガードの担保)。
//
// Task 8.2 で SubviewList の Tab は List → Summary 遷移として handleListKey →
// enterSummary に消費されるようになった。本テストの観測点は「Detail 経路 (Focus
// 循環) が走らない」ことなので、Detail.Focus が Header のまま維持されること /
// Subview が Summary に切り替わったことの 2 点で Subview ガードを担保する。
// Cmd の有無は enterSummary の Reader != nil 時の挙動 (aggregate batch 発行) に
// 依存するため、本テストでは観測しない (handleListKey 側の専用テストで担保)。
func TestAkashicDetailHandleKeyOnlyDispatchedOnSubviewDetail(t *testing.T) {
	s := State{
		Subview: SubviewList,
		Reader:  &fakeReader{},
		Detail: DetailState{
			Focus: DetailFocusHeader,
		},
	}
	got, _ := s.HandleKey(tea.KeyMsg{Type: tea.KeyTab})
	// Detail 経路が走らないことの主要観測点: Focus が変化していない
	if got.Detail.Focus != DetailFocusHeader {
		t.Fatalf("expected Focus unchanged on non-Detail Subview (Tab must not run Detail handler), got %q", got.Detail.Focus)
	}
	// 補助観測: Tab は handleListKey 経由で SubviewSummary に切替 (Task 8.2)
	if got.Subview != SubviewSummary {
		t.Fatalf("expected Subview=SubviewSummary after Tab on SubviewList (Task 8.2 dispatch), got %q", got.Subview)
	}
}
