// Package ui のアカシックモード Run 詳細サブビュー / 成果物プレビュー (state) に
// 対するテスト (akashic-mode-tui spec Task 6.2)。
//
// 本ファイルは以下を観測する:
//   - State.enterDetail が ArtifactReader != nil のとき
//     GetArtifactsCmd を tea.Batch で同時発行し、ArtifactReader == nil のときは
//     GetArtifactsCmd を発行せず Artifact.Unavailable フラグを立てること
//     (Req 6.1 / 6.2 / 11.3)
//   - State.Update の GetArtifactsMsg / ReadArtifactMsg 受信処理で、
//     (a) 通常 Rows + 通常 ReadArtifact、(b) 空 Rows、(c) ErrArtifactTooLarge、
//     (d) ErrArtifactFileMissing / fs.ErrNotExist、(e) ArtifactReader=nil の
//     5 ケースで ArtifactState の各フィールドが要件通りに排他的に
//     セットされること (Req 6.1〜6.9)
//   - Detail 突入直後 (Enter 押下前) に ReadArtifact が 0 回しか呼ばれないこと
//     (lazy 読み出し、Req 6.2)
//   - Focus=Artifacts のとき j / k / ↑ / ↓ で SelectedIndex が遷移し、範囲外で
//     クランプされること、他の Focus では SelectedIndex が動かないこと
//     (Task 6.2 観測条件)
//   - Focus=Artifacts のとき Enter キーで ReadArtifactCmd が発行され、
//     Rows[SelectedIndex].Path が ArtifactReader に渡ること (Req 6.3)
//   - ReadArtifactCmd 経路でも ArtifactReader.ReadArtifactUnlimited が
//     一切呼ばれないこと (Req 6.6 / 8.4)
//   - ArtifactReader=nil の状態では ReadArtifactCmd / GetArtifactsCmd が
//     一切発行されないこと (Req 11.3)
//   - tea.Quit を一切返さないこと (Req 11.5)
//
// 描画 (View) は Task 6.3 で実装するため、本ファイルでは検証しない。
package akashic

import (
	"errors"
	"io/fs"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

// makeAkashicArtifactDetailState は SubviewDetail / Focus=Artifacts で初期化した
// State を返すヘルパ。Rows は呼び出し側で詰めて使う。
//
// reader / artifactReader が nil でも graceful に初期化される。
func makeAkashicArtifactDetailState(reader akashic.Reader, artifactReader akashic.ArtifactReader) State {
	return State{
		Subview:        SubviewDetail,
		Reader:         reader,
		ArtifactReader: artifactReader,
		CWD:            "/work/cwd",
		Detail: DetailState{
			RunID: "run-1",
			Focus: DetailFocusArtifacts,
		},
	}
}

// --- enterDetail: GetArtifactsCmd の発行 / ArtifactReader=nil graceful ---

// TestAkashicEnterDetailIssuesGetArtifactsCmdWhenArtifactReaderProvided は
// SubviewDetail に切替時、ArtifactReader != nil のとき
//   - RunDetailCmd と GetArtifactsCmd の両方が tea.Batch で発行されること
//   - GetArtifactsCmd が runID を ArtifactReader.GetArtifacts に渡すこと
//   - Detail 突入時点で ReadArtifact は 0 回しか呼ばれないこと (lazy 読み出し)
//
// を観測する (Task 6.2, Req 6.1 / 6.2)。
func TestAkashicEnterDetailIssuesGetArtifactsCmdWhenArtifactReaderProvided(t *testing.T) {
	reader := &fakeReader{}
	artifactReader := &fakeArtifactReader{
		getArtifactsRows: []akashic.ArtifactRow{
			{WorkflowRunID: "run-7", Kind: akashic.ArtifactKindResult, Path: "/abs/.rilll/run-7/result.md", SizeBytes: 42, MIME: "text/markdown"},
		},
	}
	s := State{
		Subview:        SubviewList,
		Reader:         reader,
		ArtifactReader: artifactReader,
		CWD:            "/work/cwd",
	}

	got, cmd := s.enterDetail("run-7")
	if got.Subview != SubviewDetail {
		t.Fatalf("expected Subview=SubviewDetail, got %q", got.Subview)
	}
	if !got.Detail.Loading {
		t.Fatalf("expected Detail.Loading=true on enter (Reader != nil)")
	}
	if !got.Detail.Artifact.Loading {
		t.Fatalf("expected Detail.Artifact.Loading=true on enter (ArtifactReader != nil)")
	}
	if got.Detail.Artifact.Unavailable {
		t.Fatalf("expected Detail.Artifact.Unavailable=false when ArtifactReader != nil")
	}
	if cmd == nil {
		t.Fatal("expected non-nil Cmd on enter when both readers are provided")
	}

	// tea.Batch 経由で 2 つの Cmd が走ることを観測
	msgs := runBatchCmd(t, cmd)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 inner cmds (runDetail + getArtifacts), got %d: %#v", len(msgs), msgs)
	}
	var sawRunDetail, sawGetArtifacts bool
	for _, m := range msgs {
		switch m.(type) {
		case RunDetailMsg:
			sawRunDetail = true
		case GetArtifactsMsg:
			sawGetArtifacts = true
		}
	}
	if !sawRunDetail {
		t.Fatalf("expected RunDetailMsg in batch, got %#v", msgs)
	}
	if !sawGetArtifacts {
		t.Fatalf("expected GetArtifactsMsg in batch, got %#v", msgs)
	}

	// fake への入力検証
	if reader.detailCalls != 1 {
		t.Fatalf("expected Reader.Detail to be called once, got %d", reader.detailCalls)
	}
	if artifactReader.getArtifactsCalls != 1 {
		t.Fatalf("expected ArtifactReader.GetArtifacts to be called once, got %d", artifactReader.getArtifactsCalls)
	}
	if artifactReader.getArtifactsRunID != "run-7" {
		t.Fatalf("expected GetArtifacts runID=run-7, got %q", artifactReader.getArtifactsRunID)
	}

	// Req 6.2: Detail 突入直後 (Enter 押下前) に ReadArtifact は 0 回
	if artifactReader.readArtifactCalls != 0 {
		t.Fatalf("expected ReadArtifact NOT to be called on Detail entry (lazy load), got %d", artifactReader.readArtifactCalls)
	}
	// Req 6.6 / 8.4: ReadArtifactUnlimited は絶対に呼ばない
	if artifactReader.readArtifactUnlimitedCalls != 0 {
		t.Fatalf("expected ReadArtifactUnlimited NEVER to be called, got %d", artifactReader.readArtifactUnlimitedCalls)
	}
}

// TestAkashicEnterDetailDoesNotIssueGetArtifactsCmdWhenArtifactReaderNil は
// (e) ArtifactReader=nil 時に
//   - GetArtifactsCmd が一切発行されないこと
//   - Artifact.Unavailable=true がセットされること (View 側で graceful 表示)
//   - Artifact.Loading=false のままであること
//
// を観測する (Task 6.2, Req 11.3)。
func TestAkashicEnterDetailDoesNotIssueGetArtifactsCmdWhenArtifactReaderNil(t *testing.T) {
	reader := &fakeReader{}
	s := State{
		Subview:        SubviewList,
		Reader:         reader,
		ArtifactReader: nil,
		CWD:            "/work/cwd",
	}

	got, cmd := s.enterDetail("run-7")
	if got.Subview != SubviewDetail {
		t.Fatalf("expected Subview=SubviewDetail, got %q", got.Subview)
	}
	if !got.Detail.Artifact.Unavailable {
		t.Fatalf("expected Artifact.Unavailable=true when ArtifactReader=nil")
	}
	if got.Detail.Artifact.Loading {
		t.Fatalf("expected Artifact.Loading=false when ArtifactReader=nil (graceful)")
	}

	// Cmd は RunDetailCmd 単発 (Reader != nil なので)。
	// tea.Batch ではなく直接 RunDetailMsg を返すか、または 1 件 Batch のいずれか。
	if cmd == nil {
		t.Fatal("expected non-nil Cmd when Reader != nil even if ArtifactReader is nil")
	}
	msgs := runBatchCmd(t, cmd)
	for _, m := range msgs {
		if _, ok := m.(GetArtifactsMsg); ok {
			t.Fatalf("expected NO GetArtifactsMsg when ArtifactReader=nil, got %#v", msgs)
		}
	}
}

// TestAkashicEnterDetailBothReadersNilIssuesNoCmdAndSetsUnavailable は
// Reader=nil かつ ArtifactReader=nil のときに Cmd を一切発行せず、
// Artifact.Unavailable=true を立てることを観測する (Req 11.2 / 11.3 graceful)。
func TestAkashicEnterDetailBothReadersNilIssuesNoCmdAndSetsUnavailable(t *testing.T) {
	s := State{Subview: SubviewList, Reader: nil, ArtifactReader: nil}
	got, cmd := s.enterDetail("run-1")
	if cmd != nil {
		t.Fatalf("expected nil Cmd when both readers nil, got %T", cmd)
	}
	if got.Detail.Loading {
		t.Fatalf("expected Detail.Loading=false on graceful path")
	}
	if !got.Detail.Artifact.Unavailable {
		t.Fatalf("expected Artifact.Unavailable=true when ArtifactReader=nil")
	}
	if got.Detail.Artifact.Loading {
		t.Fatalf("expected Artifact.Loading=false when ArtifactReader=nil")
	}
}

// --- State.Update: GetArtifactsMsg ---

// TestAkashicArtifactUpdateNormalRowsPopulatesState は (a) 通常 Rows を受信した
// ときに Rows / SelectedIndex / Loading=false / Err=nil / EmptyMessage="" が
// 反映されることを観測する (Req 6.1)。
func TestAkashicArtifactUpdateNormalRowsPopulatesState(t *testing.T) {
	s := makeAkashicArtifactDetailState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Artifact.Loading = true
	// 既存 SelectedIndex を非ゼロにしておき、新ページ受信で 0 リセットされることを確認
	s.Detail.Artifact.SelectedIndex = 5

	rows := []akashic.ArtifactRow{
		{WorkflowRunID: "run-1", Kind: akashic.ArtifactKindResult, Path: "/abs/.rilll/run-1/result.md", SizeBytes: 42},
		{WorkflowRunID: "run-1", Kind: akashic.ArtifactKindProposal, Path: "/abs/.rilll/run-1/proposal.md", SizeBytes: 12},
	}
	got, cmd := s.Update(GetArtifactsMsg{Rows: rows})
	if cmd != nil {
		t.Fatalf("expected nil Cmd from GetArtifactsMsg, got %T", cmd)
	}
	if got.Detail.Artifact.Loading {
		t.Fatalf("expected Loading=false on success, got true")
	}
	if got.Detail.Artifact.Err != nil {
		t.Fatalf("expected Err=nil, got %v", got.Detail.Artifact.Err)
	}
	if got.Detail.Artifact.EmptyMessage != "" {
		t.Fatalf("expected EmptyMessage=\"\" on non-empty Rows, got %q", got.Detail.Artifact.EmptyMessage)
	}
	if len(got.Detail.Artifact.Rows) != 2 {
		t.Fatalf("expected 2 Rows, got %d", len(got.Detail.Artifact.Rows))
	}
	if got.Detail.Artifact.SelectedIndex != 0 {
		t.Fatalf("expected SelectedIndex reset to 0 on GetArtifactsMsg, got %d", got.Detail.Artifact.SelectedIndex)
	}
}

// TestAkashicArtifactUpdateEmptyRowsSetsEmptyMessage は (b) 空 Rows を受信した
// ときに EmptyMessage="成果物なし" がセットされ、Err は立たないことを観測する
// (Req 6.9)。
func TestAkashicArtifactUpdateEmptyRowsSetsEmptyMessage(t *testing.T) {
	s := makeAkashicArtifactDetailState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Artifact.Loading = true

	got, _ := s.Update(GetArtifactsMsg{Rows: nil})
	if got.Detail.Artifact.Loading {
		t.Fatalf("expected Loading=false, got true")
	}
	if got.Detail.Artifact.Err != nil {
		t.Fatalf("expected Err=nil on empty rows (must NOT be treated as error), got %v", got.Detail.Artifact.Err)
	}
	if got.Detail.Artifact.EmptyMessage != "成果物なし" {
		t.Fatalf("expected EmptyMessage=\"成果物なし\" (Req 6.9), got %q", got.Detail.Artifact.EmptyMessage)
	}
	if len(got.Detail.Artifact.Rows) != 0 {
		t.Fatalf("expected len(Rows)==0, got %d", len(got.Detail.Artifact.Rows))
	}
}

// TestAkashicArtifactUpdateGenericErrSetsErr はジェネリック error を受信した
// ときに Err が格納され、tea.Quit を返さないことを観測する (Req 11.5)。
func TestAkashicArtifactUpdateGenericErrSetsErr(t *testing.T) {
	s := makeAkashicArtifactDetailState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Artifact.Loading = true

	wantErr := errors.New("boom getartifacts")
	got, cmd := s.Update(GetArtifactsMsg{Err: wantErr})
	if cmd != nil {
		t.Fatalf("expected nil Cmd, got %T", cmd)
	}
	if got.Detail.Artifact.Loading {
		t.Fatalf("expected Loading=false on error, got true")
	}
	if !errors.Is(got.Detail.Artifact.Err, wantErr) {
		t.Fatalf("expected Err=%v, got %v", wantErr, got.Detail.Artifact.Err)
	}
}

// TestAkashicArtifactUpdateErrArtifactReaderUnavailableNoop は
// errArtifactReaderUnavailable sentinel が Err に貯まらず graceful 経路で
// 扱われることを観測する (Req 11.3)。View 側が Unavailable=true を見て
// 「成果物データが利用できません」を表示するため、Err には貯めない。
func TestAkashicArtifactUpdateErrArtifactReaderUnavailableNoop(t *testing.T) {
	s := makeAkashicArtifactDetailState(&fakeReader{}, nil)
	s.Detail.Artifact.Unavailable = true
	s.Detail.Artifact.Loading = true

	got, cmd := s.Update(GetArtifactsMsg{Err: errArtifactReaderUnavailable})
	if cmd != nil {
		t.Fatalf("expected nil Cmd, got %T", cmd)
	}
	if got.Detail.Artifact.Err != nil {
		t.Fatalf("expected Err=nil on errArtifactReaderUnavailable (graceful), got %v", got.Detail.Artifact.Err)
	}
	if !got.Detail.Artifact.Unavailable {
		t.Fatalf("expected Unavailable=true preserved on graceful path")
	}
	if got.Detail.Artifact.Loading {
		t.Fatalf("expected Loading=false on any GetArtifactsMsg arrival, got true")
	}
}

// --- Artifacts フォーカス時のカーソル移動 ---

// TestAkashicArtifactCursorMoveOnArtifactsFocus は Focus=Artifacts のとき
// j / k / ↑ / ↓ で SelectedIndex のみが動き、AgentCallsCursor / RawEventsCursor は
// 不変なことを観測する。範囲外でクランプされることも検証する。
func TestAkashicArtifactCursorMoveOnArtifactsFocus(t *testing.T) {
	s := makeAkashicArtifactDetailState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Artifact.Rows = []akashic.ArtifactRow{
		{Path: "/p/0"},
		{Path: "/p/1"},
		{Path: "/p/2"},
	}
	s.Detail.Artifact.SelectedIndex = 0
	s.Detail.AgentCallsCursor = 7
	s.Detail.RawEventsCursor = 9

	// j で 0 → 1
	s, _ = s.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if s.Detail.Artifact.SelectedIndex != 1 {
		t.Fatalf("expected SelectedIndex=1 after j, got %d", s.Detail.Artifact.SelectedIndex)
	}
	if s.Detail.AgentCallsCursor != 7 {
		t.Fatalf("expected AgentCallsCursor unchanged at 7 (Focus=Artifacts), got %d", s.Detail.AgentCallsCursor)
	}
	if s.Detail.RawEventsCursor != 9 {
		t.Fatalf("expected RawEventsCursor unchanged at 9 (Focus=Artifacts), got %d", s.Detail.RawEventsCursor)
	}

	// ↓ で 1 → 2
	s, _ = s.HandleKey(tea.KeyMsg{Type: tea.KeyDown})
	if s.Detail.Artifact.SelectedIndex != 2 {
		t.Fatalf("expected SelectedIndex=2 after Down, got %d", s.Detail.Artifact.SelectedIndex)
	}
	// ↓ で末尾クランプ (2 → 2)
	s, _ = s.HandleKey(tea.KeyMsg{Type: tea.KeyDown})
	if s.Detail.Artifact.SelectedIndex != 2 {
		t.Fatalf("expected SelectedIndex clamped to 2 at end, got %d", s.Detail.Artifact.SelectedIndex)
	}
	// k で 2 → 1
	s, _ = s.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if s.Detail.Artifact.SelectedIndex != 1 {
		t.Fatalf("expected SelectedIndex=1 after k, got %d", s.Detail.Artifact.SelectedIndex)
	}
	// ↑↑↑ で先頭クランプ (1 → 0 → 0)
	for i := 0; i < 3; i++ {
		s, _ = s.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	}
	if s.Detail.Artifact.SelectedIndex != 0 {
		t.Fatalf("expected SelectedIndex clamped to 0 at start, got %d", s.Detail.Artifact.SelectedIndex)
	}
}

// TestAkashicArtifactCursorMoveOnEmptyRowsIsNoop は Rows が空のときに
// j / k / ↑ / ↓ が no-op (SelectedIndex=0 のまま) であることを観測する
// (空一覧でカーソルキー連打しても範囲外参照が起きない不変式)。
func TestAkashicArtifactCursorMoveOnEmptyRowsIsNoop(t *testing.T) {
	s := makeAkashicArtifactDetailState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Artifact.Rows = nil
	s.Detail.Artifact.SelectedIndex = 0

	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'j'}},
		{Type: tea.KeyRunes, Runes: []rune{'k'}},
		{Type: tea.KeyDown},
		{Type: tea.KeyUp},
	} {
		s, _ = s.HandleKey(key)
	}
	if s.Detail.Artifact.SelectedIndex != 0 {
		t.Fatalf("expected SelectedIndex=0 on empty rows, got %d", s.Detail.Artifact.SelectedIndex)
	}
}

// --- State.HandleKey: Enter で ReadArtifactCmd 発行 (lazy load) ---

// TestAkashicArtifactEnterIssuesReadArtifactCmd は Focus=Artifacts のとき
// Enter キーで ReadArtifactCmd が発行され、選択中 Path が ArtifactReader に
// 渡ることを観測する (Req 6.3)。
//
// 観測点:
//   - cmd() invoke 後 ArtifactReader.ReadArtifact が 1 回だけ呼ばれる
//   - 渡された path が Rows[SelectedIndex].Path と一致する
//   - ReadArtifactMsg.Path が同値で返る (照合用)
//   - Loading=true がセットされる
//   - ReadArtifactUnlimited は呼ばれない (Req 6.6 / 8.4)
func TestAkashicArtifactEnterIssuesReadArtifactCmd(t *testing.T) {
	artifactReader := &fakeArtifactReader{
		readArtifactData: []byte("body"),
	}
	s := makeAkashicArtifactDetailState(&fakeReader{}, artifactReader)
	s.Detail.Artifact.Rows = []akashic.ArtifactRow{
		{Path: "/abs/.rilll/run-1/result.md"},
		{Path: "/abs/.rilll/run-1/proposal.md"},
	}
	s.Detail.Artifact.SelectedIndex = 1

	got, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected non-nil Cmd from Enter on Focus=Artifacts")
	}
	if !got.Detail.Artifact.Loading {
		t.Fatalf("expected Artifact.Loading=true while reading body")
	}

	msg := cmd()
	got2, ok := msg.(ReadArtifactMsg)
	if !ok {
		t.Fatalf("expected ReadArtifactMsg, got %T", msg)
	}
	if got2.Path != "/abs/.rilll/run-1/proposal.md" {
		t.Fatalf("expected msg.Path=/abs/.rilll/run-1/proposal.md, got %q", got2.Path)
	}
	if artifactReader.readArtifactCalls != 1 {
		t.Fatalf("expected ReadArtifact called once, got %d", artifactReader.readArtifactCalls)
	}
	if artifactReader.readArtifactPath != "/abs/.rilll/run-1/proposal.md" {
		t.Fatalf("expected ArtifactReader received path=/abs/.rilll/run-1/proposal.md, got %q", artifactReader.readArtifactPath)
	}
	if artifactReader.readArtifactUnlimitedCalls != 0 {
		t.Fatalf("expected ReadArtifactUnlimited never called (Req 6.6 / 8.4), got %d", artifactReader.readArtifactUnlimitedCalls)
	}
}

// TestAkashicArtifactEnterNoopOnNonArtifactsFocus は Focus != Artifacts のとき
// Enter キーが no-op (Cmd を発行しない / ReadArtifact を呼ばない) ことを観測する。
//
// 動機: 他 Focus (Header / AgentCalls / RawEvents) で Enter を押しても成果物
// プレビューが暴発しない不変式 (誤読込みの境界保護)。
func TestAkashicArtifactEnterNoopOnNonArtifactsFocus(t *testing.T) {
	artifactReader := &fakeArtifactReader{}
	s := makeAkashicArtifactDetailState(&fakeReader{}, artifactReader)
	s.Detail.Artifact.Rows = []akashic.ArtifactRow{{Path: "/p/0"}}
	s.Detail.Artifact.SelectedIndex = 0

	for _, focus := range []DetailFocus{DetailFocusHeader, DetailFocusAgentCalls, DetailFocusRawEvents} {
		s.Detail.Focus = focus
		_, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
		if cmd != nil {
			t.Fatalf("expected nil Cmd on Enter when Focus=%q, got %T", focus, cmd)
		}
	}
	if artifactReader.readArtifactCalls != 0 {
		t.Fatalf("expected ReadArtifact NOT to be called on non-Artifacts Focus, got %d", artifactReader.readArtifactCalls)
	}
}

// TestAkashicArtifactEnterNoopOnEmptyRows は Focus=Artifacts でも Rows が空のとき
// Enter が no-op であることを観測する (空一覧で Enter 連打しても暴発しない)。
func TestAkashicArtifactEnterNoopOnEmptyRows(t *testing.T) {
	artifactReader := &fakeArtifactReader{}
	s := makeAkashicArtifactDetailState(&fakeReader{}, artifactReader)
	s.Detail.Artifact.Rows = nil

	got, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatalf("expected nil Cmd on Enter with empty Rows, got %T", cmd)
	}
	if got.Detail.Artifact.Loading {
		t.Fatalf("expected Loading=false on no-op, got true")
	}
	if artifactReader.readArtifactCalls != 0 {
		t.Fatalf("expected ReadArtifact NOT to be called on empty Rows, got %d", artifactReader.readArtifactCalls)
	}
}

// TestAkashicArtifactEnterNoopWhenArtifactReaderNil は (e) ArtifactReader=nil の
// とき Enter が Cmd を発行せず ReadArtifact が呼ばれないことを観測する (Req 11.3)。
func TestAkashicArtifactEnterNoopWhenArtifactReaderNil(t *testing.T) {
	s := makeAkashicArtifactDetailState(&fakeReader{}, nil)
	s.Detail.Artifact.Rows = []akashic.ArtifactRow{{Path: "/p/0"}}
	s.Detail.Artifact.SelectedIndex = 0
	s.Detail.Artifact.Unavailable = true

	got, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatalf("expected nil Cmd on Enter when ArtifactReader=nil (Req 11.3), got %T", cmd)
	}
	if got.Detail.Artifact.Loading {
		t.Fatalf("expected Loading=false on graceful no-op, got true")
	}
}

// --- State.Update: ReadArtifactMsg ---

// TestAkashicArtifactReadArtifactSuccessPopulatesPreview は (a) 成功時に
// PreviewBytes / PreviewPath が反映され、PreviewWarning="" / Err=nil /
// Loading=false がセットされることを観測する。
func TestAkashicArtifactReadArtifactSuccessPopulatesPreview(t *testing.T) {
	s := makeAkashicArtifactDetailState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Artifact.Loading = true
	// 既存 PreviewWarning / Err を clear することを確認するため事前にセット
	s.Detail.Artifact.PreviewWarning = "stale-warn"
	s.Detail.Artifact.Err = errors.New("stale-err")

	want := []byte("hello body")
	got, cmd := s.Update(ReadArtifactMsg{Path: "/abs/.rilll/run-1/result.md", Data: want})
	if cmd != nil {
		t.Fatalf("expected nil Cmd, got %T", cmd)
	}
	if got.Detail.Artifact.Loading {
		t.Fatalf("expected Loading=false, got true")
	}
	if got.Detail.Artifact.Err != nil {
		t.Fatalf("expected Err cleared on success, got %v", got.Detail.Artifact.Err)
	}
	if got.Detail.Artifact.PreviewWarning != "" {
		t.Fatalf("expected PreviewWarning cleared on success, got %q", got.Detail.Artifact.PreviewWarning)
	}
	if string(got.Detail.Artifact.PreviewBytes) != string(want) {
		t.Fatalf("expected PreviewBytes=%q, got %q", want, got.Detail.Artifact.PreviewBytes)
	}
	if got.Detail.Artifact.PreviewPath != "/abs/.rilll/run-1/result.md" {
		t.Fatalf("expected PreviewPath=/abs/.rilll/run-1/result.md, got %q", got.Detail.Artifact.PreviewPath)
	}
}

// TestAkashicArtifactReadArtifactTooLargeSetsWarning は (c) ErrArtifactTooLarge を
// 受信したときに PreviewWarning="上限超過のため本体を読み込みませんでした" が立ち
// PreviewBytes=nil / Err=nil / Loading=false がセットされることを観測する
// (Req 6.5 / 6.6)。メタ表示 (Rows / SelectedIndex) は変更されない。
func TestAkashicArtifactReadArtifactTooLargeSetsWarning(t *testing.T) {
	s := makeAkashicArtifactDetailState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Artifact.Loading = true
	s.Detail.Artifact.Rows = []akashic.ArtifactRow{
		{Path: "/abs/.rilll/run-1/big.bin", SizeBytes: 100 * 1024 * 1024},
	}
	s.Detail.Artifact.SelectedIndex = 0

	got, cmd := s.Update(ReadArtifactMsg{Path: "/abs/.rilll/run-1/big.bin", Err: akashic.ErrArtifactTooLarge})
	if cmd != nil {
		t.Fatalf("expected nil Cmd, got %T", cmd)
	}
	if got.Detail.Artifact.Loading {
		t.Fatalf("expected Loading=false, got true")
	}
	if got.Detail.Artifact.Err != nil {
		t.Fatalf("expected Err=nil on graceful warning (Req 6.5), got %v", got.Detail.Artifact.Err)
	}
	if got.Detail.Artifact.PreviewWarning != "上限超過のため本体を読み込みませんでした" {
		t.Fatalf("expected PreviewWarning=\"上限超過のため本体を読み込みませんでした\", got %q", got.Detail.Artifact.PreviewWarning)
	}
	if got.Detail.Artifact.PreviewBytes != nil {
		t.Fatalf("expected PreviewBytes=nil on too-large, got %d bytes", len(got.Detail.Artifact.PreviewBytes))
	}
	// メタ表示は継続 (Req 6.5 後段: メタは引き続き表示)
	if len(got.Detail.Artifact.Rows) != 1 {
		t.Fatalf("expected Rows preserved on too-large warning, got %d rows", len(got.Detail.Artifact.Rows))
	}
}

// TestAkashicArtifactReadArtifactFileMissingSetsWarning は (d) ErrArtifactFileMissing
// を受信したときに PreviewWarning="成果物が見つかりません" が立ち、Err=nil で
// Loading=false がセットされることを観測する (Req 6.7)。
func TestAkashicArtifactReadArtifactFileMissingSetsWarning(t *testing.T) {
	s := makeAkashicArtifactDetailState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Artifact.Loading = true

	got, _ := s.Update(ReadArtifactMsg{Path: "/abs/.rilll/run-1/missing.md", Err: akashic.ErrArtifactFileMissing})
	if got.Detail.Artifact.Loading {
		t.Fatalf("expected Loading=false, got true")
	}
	if got.Detail.Artifact.Err != nil {
		t.Fatalf("expected Err=nil on graceful missing warning (Req 6.7), got %v", got.Detail.Artifact.Err)
	}
	if got.Detail.Artifact.PreviewWarning != "成果物が見つかりません" {
		t.Fatalf("expected PreviewWarning=\"成果物が見つかりません\", got %q", got.Detail.Artifact.PreviewWarning)
	}
	if got.Detail.Artifact.PreviewBytes != nil {
		t.Fatalf("expected PreviewBytes=nil on file-missing, got %d bytes", len(got.Detail.Artifact.PreviewBytes))
	}
}

// TestAkashicArtifactReadArtifactFsErrNotExistSetsWarning は fs.ErrNotExist
// (raw) を受信したときも「成果物が見つかりません」が立つことを観測する。
// errors.Is は ErrArtifactFileMissing と fs.ErrNotExist の双方で真になる契約のため
// (artifact_reader.go: ErrArtifactFileMissing は fs.ErrNotExist で wrap されている)、
// raw fs.ErrNotExist でも同じ graceful 経路に入ることを担保する (Req 6.7)。
func TestAkashicArtifactReadArtifactFsErrNotExistSetsWarning(t *testing.T) {
	s := makeAkashicArtifactDetailState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Artifact.Loading = true

	got, _ := s.Update(ReadArtifactMsg{Path: "/abs/.rilll/run-1/missing.md", Err: fs.ErrNotExist})
	if got.Detail.Artifact.PreviewWarning != "成果物が見つかりません" {
		t.Fatalf("expected PreviewWarning=\"成果物が見つかりません\" for fs.ErrNotExist, got %q", got.Detail.Artifact.PreviewWarning)
	}
	if got.Detail.Artifact.Err != nil {
		t.Fatalf("expected Err=nil for fs.ErrNotExist (graceful), got %v", got.Detail.Artifact.Err)
	}
}

// TestAkashicArtifactReadArtifactGenericErrorSetsErr はジェネリック error を
// 受信したときに Err が立ち、PreviewWarning="" となることを観測する (Req 6 graceful)。
func TestAkashicArtifactReadArtifactGenericErrorSetsErr(t *testing.T) {
	s := makeAkashicArtifactDetailState(&fakeReader{}, &fakeArtifactReader{})
	s.Detail.Artifact.Loading = true
	s.Detail.Artifact.PreviewWarning = "stale-warn" // 直前の warning が clear されることも観測

	wantErr := errors.New("boom read")
	got, cmd := s.Update(ReadArtifactMsg{Path: "/abs/.rilll/run-1/x.md", Err: wantErr})
	if cmd != nil {
		t.Fatalf("expected nil Cmd, got %T", cmd)
	}
	if got.Detail.Artifact.Loading {
		t.Fatalf("expected Loading=false, got true")
	}
	if !errors.Is(got.Detail.Artifact.Err, wantErr) {
		t.Fatalf("expected Err=%v, got %v", wantErr, got.Detail.Artifact.Err)
	}
	if got.Detail.Artifact.PreviewWarning != "" {
		t.Fatalf("expected PreviewWarning cleared on generic error, got %q", got.Detail.Artifact.PreviewWarning)
	}
}

// TestAkashicArtifactReadArtifactErrArtifactReaderUnavailableNoop は graceful
// sentinel が Err に貯まらないことを観測する (Req 11.3)。
func TestAkashicArtifactReadArtifactErrArtifactReaderUnavailableNoop(t *testing.T) {
	s := makeAkashicArtifactDetailState(&fakeReader{}, nil)
	s.Detail.Artifact.Unavailable = true
	s.Detail.Artifact.Loading = true

	got, cmd := s.Update(ReadArtifactMsg{Path: "/p", Err: errArtifactReaderUnavailable})
	if cmd != nil {
		t.Fatalf("expected nil Cmd, got %T", cmd)
	}
	if got.Detail.Artifact.Err != nil {
		t.Fatalf("expected Err=nil on errArtifactReaderUnavailable (graceful), got %v", got.Detail.Artifact.Err)
	}
	if got.Detail.Artifact.PreviewWarning != "" {
		t.Fatalf("expected PreviewWarning empty on graceful path, got %q", got.Detail.Artifact.PreviewWarning)
	}
	if got.Detail.Artifact.Loading {
		t.Fatalf("expected Loading=false on any ReadArtifactMsg arrival, got true")
	}
}

// --- 包括検査: tea.Quit を返さない / Subview ガード ---

// TestAkashicArtifactUpdateNeverReturnsTeaQuit は Update が GetArtifactsMsg /
// ReadArtifactMsg のすべてのケースで tea.Quit を一度も返さないことを観測する
// (Req 11.5)。
func TestAkashicArtifactUpdateNeverReturnsTeaQuit(t *testing.T) {
	cases := []struct {
		name string
		msg  tea.Msg
	}{
		{"getArtifacts/normal", GetArtifactsMsg{Rows: []akashic.ArtifactRow{{Path: "/p"}}}},
		{"getArtifacts/empty", GetArtifactsMsg{Rows: nil}},
		{"getArtifacts/error", GetArtifactsMsg{Err: errors.New("boom")}},
		{"getArtifacts/unavailable", GetArtifactsMsg{Err: errArtifactReaderUnavailable}},
		{"readArtifact/success", ReadArtifactMsg{Path: "/p", Data: []byte("x")}},
		{"readArtifact/tooLarge", ReadArtifactMsg{Path: "/p", Err: akashic.ErrArtifactTooLarge}},
		{"readArtifact/missing", ReadArtifactMsg{Path: "/p", Err: akashic.ErrArtifactFileMissing}},
		{"readArtifact/fsNotExist", ReadArtifactMsg{Path: "/p", Err: fs.ErrNotExist}},
		{"readArtifact/error", ReadArtifactMsg{Path: "/p", Err: errors.New("boom")}},
		{"readArtifact/unavailable", ReadArtifactMsg{Path: "/p", Err: errArtifactReaderUnavailable}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := makeAkashicArtifactDetailState(&fakeReader{}, &fakeArtifactReader{})
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
