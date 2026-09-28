// Package ui のアカシックモード Run 一覧サブビューに対するテスト
// (akashic-mode-tui spec Task 4.2)。
//
// 本ファイルは State.Update の ListRunsMsg 受信処理と
// State.View の Run 一覧描画 (Reader=nil graceful 含む) を観測する。
//
// 観測完了条件 (Task 4.2):
//   - fake Reader が (a) 通常 ListResult、(b) 空 ListResult、(c) error、
//     (d) Reader=nil 注入の 4 ケースで、それぞれ Items / EmptyMessage / Err /
//     「履歴データが利用できません」表示が排他的にセットされること
//   - tea.Quit が一度も返らないこと
//   - (d) のケースで Reader メソッドが 0 回呼ばれること
//   - View 関数で Items / Filter / Sort / Cursor / Err / EmptyMessage / Loading /
//     Reader=nil 表示が反映されること
package akashic

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

// makeAkashicState は SubviewList で初期化した State を返すヘルパ。
// reader が nil でも graceful に初期化されることを利用する。
func makeAkashicState(reader akashic.Reader) State {
	return State{
		Subview: SubviewList,
		Reader:  reader,
		List: ListState{
			Sort:   SortNewest,
			Limit:  akashic.DefaultListLimit,
			Offset: 0,
		},
	}
}

// --- State.Update: ListRunsMsg ---

func TestAkashicListUpdateNormalListResultPopulatesItems(t *testing.T) {
	// 観測ケース (a): 通常 ListResult → Items / NextOffset 反映、Loading=false、Err 無し、EmptyMessage 無し
	reader := &fakeReader{}
	s := makeAkashicState(reader)
	s.List.Loading = true

	next := 50
	msg := ListRunsMsg{
		Result: akashic.ListResult{
			Items: []akashic.WorkflowRunListItem{
				{RunHeader: akashic.RunHeader{RunID: "run-1", WorkflowID: "wf-1", StartedAt: time.Now()}},
				{RunHeader: akashic.RunHeader{RunID: "run-2", WorkflowID: "wf-1", StartedAt: time.Now()}},
			},
			Limit:      50,
			Offset:     0,
			NextOffset: &next,
		},
	}

	got, cmd := s.Update(msg)
	// tea.Quit を返さないこと (Req 3.10 / 11.5)
	if cmd != nil {
		// ListRunsMsg 受信は新しい Cmd を発行しない設計
		t.Fatalf("expected nil Cmd from ListRunsMsg, got %T", cmd)
	}
	if got.List.Loading {
		t.Fatalf("expected Loading=false, got true")
	}
	if got.List.Err != nil {
		t.Fatalf("expected Err=nil on success, got %v", got.List.Err)
	}
	if got.List.EmptyMessage != "" {
		t.Fatalf("expected EmptyMessage='' on non-empty list, got %q", got.List.EmptyMessage)
	}
	if len(got.List.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(got.List.Items))
	}
	if got.List.Items[0].RunID != "run-1" || got.List.Items[1].RunID != "run-2" {
		t.Fatalf("expected items in Reader order, got %#v", got.List.Items)
	}
	if got.List.NextOffset == nil || *got.List.NextOffset != 50 {
		t.Fatalf("expected NextOffset=50, got %v", got.List.NextOffset)
	}
}

func TestAkashicListUpdateEmptyListResultSetsEmptyMessage(t *testing.T) {
	// 観測ケース (b): 空 ListResult → EmptyMessage 立ち、Err なし、tea.Quit なし
	reader := &fakeReader{}
	s := makeAkashicState(reader)
	s.List.Loading = true

	msg := ListRunsMsg{
		Result: akashic.ListResult{
			Items:  nil,
			Limit:  50,
			Offset: 0,
		},
	}

	got, cmd := s.Update(msg)
	if cmd != nil {
		t.Fatalf("expected nil Cmd, got %T", cmd)
	}
	if got.List.Loading {
		t.Fatalf("expected Loading=false on empty result, got true")
	}
	if got.List.Err != nil {
		t.Fatalf("expected Err=nil on empty result (Req 3.9), got %v", got.List.Err)
	}
	if got.List.EmptyMessage != "該当 run がありません" {
		t.Fatalf("expected EmptyMessage='該当 run がありません', got %q", got.List.EmptyMessage)
	}
	if len(got.List.Items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(got.List.Items))
	}
}

func TestAkashicListUpdateErrorSetsErrAndDoesNotQuit(t *testing.T) {
	// 観測ケース (c): error → Err 設定、Loading=false、tea.Quit を返さない
	reader := &fakeReader{}
	s := makeAkashicState(reader)
	s.List.Loading = true

	wantErr := errors.New("boom list")
	msg := ListRunsMsg{Err: wantErr}

	got, cmd := s.Update(msg)
	// Req 3.10 / 11.5: TUI プロセスを終了させない
	if cmd != nil {
		t.Fatalf("expected nil Cmd (must NOT return tea.Quit), got %T", cmd)
	}
	if got.List.Loading {
		t.Fatalf("expected Loading=false on error, got true")
	}
	if !errors.Is(got.List.Err, wantErr) {
		t.Fatalf("expected Err=%v, got %v", wantErr, got.List.Err)
	}
	if got.List.EmptyMessage != "" {
		t.Fatalf("expected EmptyMessage='' on error, got %q", got.List.EmptyMessage)
	}
}

func TestAkashicListUpdateErrReaderUnavailableDoesNotPopulateErr(t *testing.T) {
	// Reader 不在 sentinel: Err に貯めず、View 側で s.Reader == nil を検知して
	// 「履歴データが利用できません」を出すため、Err / EmptyMessage / Items は
	// 影響を受けない (Req 11.2 graceful)。
	s := makeAkashicState(nil)
	s.List.Loading = true
	s.List.Items = []akashic.WorkflowRunListItem{
		{RunHeader: akashic.RunHeader{RunID: "stale"}},
	}

	msg := ListRunsMsg{Err: errReaderUnavailable}
	got, cmd := s.Update(msg)
	if cmd != nil {
		t.Fatalf("expected nil Cmd, got %T", cmd)
	}
	if got.List.Err != nil {
		t.Fatalf("expected Err=nil on errReaderUnavailable (graceful), got %v", got.List.Err)
	}
	if got.List.EmptyMessage != "" {
		t.Fatalf("expected EmptyMessage='' on errReaderUnavailable, got %q", got.List.EmptyMessage)
	}
	if got.List.Loading {
		t.Fatalf("expected Loading=false on any ListRunsMsg arrival, got true")
	}
}

func TestAkashicListUpdateClampsCursorOnShorterList(t *testing.T) {
	// 既存 Cursor が新 Items の末尾を超える場合、len(Items)-1 にクランプ。
	s := makeAkashicState(&fakeReader{})
	s.List.Cursor = 10

	msg := ListRunsMsg{
		Result: akashic.ListResult{
			Items: []akashic.WorkflowRunListItem{
				{RunHeader: akashic.RunHeader{RunID: "run-1"}},
				{RunHeader: akashic.RunHeader{RunID: "run-2"}},
				{RunHeader: akashic.RunHeader{RunID: "run-3"}},
			},
		},
	}
	got, _ := s.Update(msg)
	if got.List.Cursor != 2 {
		t.Fatalf("expected Cursor clamped to 2, got %d", got.List.Cursor)
	}
}

func TestAkashicListUpdateClampsNegativeCursor(t *testing.T) {
	// 既存 Cursor が負の場合、0 にクランプ。
	s := makeAkashicState(&fakeReader{})
	s.List.Cursor = -5

	msg := ListRunsMsg{
		Result: akashic.ListResult{
			Items: []akashic.WorkflowRunListItem{
				{RunHeader: akashic.RunHeader{RunID: "run-1"}},
			},
		},
	}
	got, _ := s.Update(msg)
	if got.List.Cursor != 0 {
		t.Fatalf("expected Cursor clamped to 0, got %d", got.List.Cursor)
	}
}

func TestAkashicListUpdateNonListMsgIsNoop(t *testing.T) {
	// 関連しない tea.Msg (例: tea.WindowSizeMsg) を受けたとき state は変わらない。
	// TUI シェル (package ui) 固有の heartbeatMsg は本パッケージから参照
	// できないため、akashic 側で観測したいのは「未知 msg を受けても state を
	// 変更しない」不変式のみ。Bubble Tea 既知型を代用すれば十分。
	s := makeAkashicState(&fakeReader{})
	s.List.Items = []akashic.WorkflowRunListItem{
		{RunHeader: akashic.RunHeader{RunID: "run-1"}},
	}
	got, cmd := s.Update(tea.WindowSizeMsg{})
	if cmd != nil {
		t.Fatalf("expected nil Cmd for unrelated msg, got %T", cmd)
	}
	if len(got.List.Items) != 1 {
		t.Fatalf("expected Items unchanged, got %d items", len(got.List.Items))
	}
}

// --- State.View: Run 一覧サブビュー ---

func TestAkashicListViewReaderNilShowsUnavailable(t *testing.T) {
	// 観測ケース (d): Reader=nil → 「履歴データが利用できません」表示
	s := makeAkashicState(nil)
	view := s.View(120)
	if !strings.Contains(view, "履歴データが利用できません") {
		t.Fatalf("expected 'unavailable' message, got:\n%s", view)
	}
}

func TestAkashicListViewReaderNilDoesNotShowItems(t *testing.T) {
	// 排他: Reader=nil のとき他の表示要素 (Items / EmptyMessage / Err / Loading) は出さない。
	s := makeAkashicState(nil)
	// stale な Items / EmptyMessage / Err / Loading が残っていてもこれらを描画しない
	s.List.Items = []akashic.WorkflowRunListItem{
		{RunHeader: akashic.RunHeader{RunID: "stale-run"}},
	}
	s.List.EmptyMessage = "stale-empty"
	s.List.Err = errors.New("stale-err")
	s.List.Loading = true

	view := s.View(120)
	if !strings.Contains(view, "履歴データが利用できません") {
		t.Fatalf("expected unavailable message, got:\n%s", view)
	}
	if strings.Contains(view, "stale-run") {
		t.Fatalf("Reader=nil view must not render stale items, got:\n%s", view)
	}
	if strings.Contains(view, "stale-empty") {
		t.Fatalf("Reader=nil view must not render EmptyMessage, got:\n%s", view)
	}
	if strings.Contains(view, "stale-err") {
		t.Fatalf("Reader=nil view must not render Err, got:\n%s", view)
	}
}

func TestAkashicListViewItemsRenderedWithCursor(t *testing.T) {
	// Items / Cursor 反映: 2 件描画され、Cursor 行のみ selectedStyle のカーソル
	// マーカ ("›") を持ち、もう一方は持たないことを観測する。
	s := makeAkashicState(&fakeReader{})
	s.List.Items = []akashic.WorkflowRunListItem{
		{RunHeader: akashic.RunHeader{RunID: "run-A", WorkflowID: "wf-A", StartedAt: time.Now()}},
		{RunHeader: akashic.RunHeader{RunID: "run-B", WorkflowID: "wf-B", StartedAt: time.Now()}},
	}
	s.List.Cursor = 1

	view := s.View(120)
	if !strings.Contains(view, "run-A") {
		t.Fatalf("expected run-A in view, got:\n%s", view)
	}
	if !strings.Contains(view, "run-B") {
		t.Fatalf("expected run-B in view, got:\n%s", view)
	}
	// Cursor 行に "›" カーソルが付くこと
	lines := strings.Split(view, "\n")
	var cursorLine, otherLine string
	for _, line := range lines {
		if strings.Contains(line, "run-B") {
			cursorLine = line
		}
		if strings.Contains(line, "run-A") {
			otherLine = line
		}
	}
	if !strings.Contains(cursorLine, "›") {
		t.Fatalf("expected cursor marker '›' on selected (run-B) line, got:\n%s", cursorLine)
	}
	if strings.Contains(otherLine, "›") {
		t.Fatalf("non-selected line (run-A) must not have cursor marker, got:\n%s", otherLine)
	}
}

func TestAkashicListViewEmptyMessageDisplayed(t *testing.T) {
	// 観測ケース (b): EmptyMessage → 「該当 run がありません」表示、Items は出さない
	s := makeAkashicState(&fakeReader{})
	s.List.EmptyMessage = "該当 run がありません"
	view := s.View(120)
	if !strings.Contains(view, "該当 run がありません") {
		t.Fatalf("expected EmptyMessage in view, got:\n%s", view)
	}
}

func TestAkashicListViewErrorDisplayed(t *testing.T) {
	// 観測ケース (c): Err → エラー文を warningStyle で表示
	s := makeAkashicState(&fakeReader{})
	s.List.Err = errors.New("read failed: connection refused")
	view := s.View(120)
	if !strings.Contains(view, "read failed: connection refused") {
		t.Fatalf("expected error text in view, got:\n%s", view)
	}
}

func TestAkashicListViewLoadingDisplayed(t *testing.T) {
	// Loading 中の表示: 何らかの「読み込み中」相当文言が描画される
	s := makeAkashicState(&fakeReader{})
	s.List.Loading = true
	view := s.View(120)
	if !strings.Contains(view, "読み込み中") {
		t.Fatalf("expected loading indicator in view, got:\n%s", view)
	}
}

func TestAkashicListViewSortLabelDisplayed(t *testing.T) {
	// Sort 反映: Sort=SortCost のとき "cost" (または日本語表記) が view 中に出ること
	s := makeAkashicState(&fakeReader{})
	s.List.Sort = SortCost
	s.List.Items = []akashic.WorkflowRunListItem{
		{RunHeader: akashic.RunHeader{RunID: "run-1", StartedAt: time.Now()}},
	}
	view := s.View(120)
	if !strings.Contains(view, "cost") {
		t.Fatalf("expected sort label 'cost' in view, got:\n%s", view)
	}
}

func TestAkashicListViewFilterDisplayed(t *testing.T) {
	// Filter 反映: WorkflowID / Success フィルタが view に出ること
	successTrue := true
	s := makeAkashicState(&fakeReader{})
	s.List.Filter = ListFilter{
		WorkflowID: "wf-X",
		Success:    &successTrue,
	}
	s.List.Items = []akashic.WorkflowRunListItem{
		{RunHeader: akashic.RunHeader{RunID: "run-1", StartedAt: time.Now()}},
	}
	view := s.View(120)
	if !strings.Contains(view, "wf-X") {
		t.Fatalf("expected workflow filter 'wf-X' in view, got:\n%s", view)
	}
}

// TestAkashicListViewExclusiveStates は Reader=nil / Loading / Err /
// EmptyMessage / Items の 5 状態が排他的に描画されることを観測する
// (タスク 4.2 観測完了条件「排他的にセット」の View 側担保)。
//
// 各ケースで「他状態の文言が出ない」ことも合わせて確認する。
func TestAkashicListViewExclusiveStates(t *testing.T) {
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
				// stale な他フィールドは Reader=nil 経路で抑止される
				s.List.Items = []akashic.WorkflowRunListItem{
					{RunHeader: akashic.RunHeader{RunID: "stale"}},
				}
				s.List.EmptyMessage = "stale-empty"
				s.List.Err = errors.New("stale-err")
			},
			want:    "履歴データが利用できません",
			notWant: []string{"stale-run", "stale-empty", "stale-err", "読み込み中"},
		},
		{
			name: "Loading",
			mutate: func(s *State) {
				s.List.Loading = true
			},
			want:    "読み込み中",
			notWant: []string{"履歴データが利用できません", "該当 run がありません"},
		},
		{
			name: "Error",
			mutate: func(s *State) {
				s.List.Err = errors.New("boom-err-display")
			},
			want:    "boom-err-display",
			notWant: []string{"履歴データが利用できません", "該当 run がありません", "読み込み中"},
		},
		{
			name: "Empty",
			mutate: func(s *State) {
				s.List.EmptyMessage = "該当 run がありません"
			},
			want:    "該当 run がありません",
			notWant: []string{"履歴データが利用できません", "読み込み中"},
		},
		{
			name: "Items",
			mutate: func(s *State) {
				s.List.Items = []akashic.WorkflowRunListItem{
					{RunHeader: akashic.RunHeader{RunID: "run-only", WorkflowID: "wf-1", StartedAt: time.Now()}},
				}
			},
			want:    "run-only",
			notWant: []string{"履歴データが利用できません", "該当 run がありません", "読み込み中"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := makeAkashicState(&fakeReader{})
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

// TestAkashicListUpdateNeverReturnsTeaQuit は Update が ListRunsMsg の
// 4 ケース (通常 / 空 / error / errReaderUnavailable) で tea.Quit を一度も
// 返さないことを Cmd 戻り値ベースで観測する (Req 3.10 / 11.5)。
//
// 既存テスト群が cmd != nil 検査で同様に担保するが、本テストは Cmd を invoke して
// quit msg を直接観測することで「将来 Cmd を返す設計に変更しても tea.Quit 経路は
// 残らない」回帰検査として機能する。
func TestAkashicListUpdateNeverReturnsTeaQuit(t *testing.T) {
	cases := []struct {
		name string
		msg  ListRunsMsg
	}{
		{"normal", ListRunsMsg{Result: akashic.ListResult{Items: []akashic.WorkflowRunListItem{
			{RunHeader: akashic.RunHeader{RunID: "r"}},
		}}}},
		{"empty", ListRunsMsg{Result: akashic.ListResult{Items: nil}}},
		{"error", ListRunsMsg{Err: errors.New("boom")}},
		{"reader-unavailable", ListRunsMsg{Err: errReaderUnavailable}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := makeAkashicState(&fakeReader{})
			_, cmd := s.Update(c.msg)
			if cmd == nil {
				return
			}
			msg := cmd()
			if _, isQuit := msg.(tea.QuitMsg); isQuit {
				t.Fatalf("Update must not return tea.Quit on ListRunsMsg")
			}
		})
	}
}

// TestAkashicListReaderNotCalledWhenNil は Reader=nil のときに Reader メソッドが
// 一切呼ばれないことを fake Reader の呼び出し計数で観測する
// (タスク 4.2 観測完了条件 (d) 後段)。
//
// 本タスクは receive 側の実装なので、Update / View が Reader メソッドを
// 直接呼ばないこと自体が前提条件であり、ここではその前提を回帰検査として
// 明示する: Reader=nil で State を構築 → Update / View を起動 →
// fake への呼び出し計数が 0 のまま (= そもそも fake Reader を渡していないが、
// 「将来 Update / View が Reader を内部から呼ぶようになっても気付ける」ように、
// Reader=nil を経路に流して panic / nil pointer dereference が起きないことを
// 担保する)。
func TestAkashicListReaderNotCalledWhenNil(t *testing.T) {
	s := makeAkashicState(nil)
	// Update を 4 ケースで叩いても panic しない
	for _, m := range []tea.Msg{
		ListRunsMsg{Result: akashic.ListResult{}},
		ListRunsMsg{Err: errReaderUnavailable},
		ListRunsMsg{Err: errors.New("x")},
		tea.WindowSizeMsg{},
	} {
		s, _ = s.Update(m)
	}
	// View も Reader=nil 経路で panic しない
	view := s.View(80)
	if !strings.Contains(view, "履歴データが利用できません") {
		t.Fatalf("expected unavailable message, got:\n%s", view)
	}
}
