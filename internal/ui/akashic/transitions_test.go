// Package ui のアカシックモード サブビュー間遷移キー写像と Phase ガード越し
// msg ディスパッチに対するテスト (akashic-mode-tui spec Task 8.2)。
//
// 観測完了条件 (Task 8.2):
//
//	(a) List ↔ Summary、List → Detail → List の往復遷移が決定的に行われること
//	    (Req 2.3〜2.6)
//	(b) PhaseAkashic 中に発行した ListRunsCmd の戻り ListRunsMsg を
//	    PhaseMenu に戻ってから Model.Update に投げて
//	    Phase / 既存表示 / State.List.Items のいずれも変化しないこと
//	    (Req 9.4 stale msg 排除)
//	(c) 再突入で State が初期状態 (Subview=List,
//	    Items=nil または Loading=true 相当) に戻ること
//	    (= 再突入後の State は退出時に届いた stale msg の影響を受けない)
//
// 設計メモ:
//   - List → Summary / Summary → List は `tab` キーで循環する
//     (`tab` は State 側で消費される)
//   - List → Detail は Run 一覧での Enter で選択 run を渡す
//   - Detail → List は `backspace` キーで戻る (Esc / q はモード退出として
//     Model 側で消費されるため、State は Detail 内で backspace のみ受ける)
package akashic

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

// --- (a) サブビュー間遷移キー写像 ---

// TestAkashicHandleKeyListToSummaryOnTab は List → Summary 遷移を観測する
// (Req 2.3 / 2.4)。
//
// 観測点:
//   - Subview=SubviewList で `tab` キー → Subview=SubviewSummary
//   - Reader != nil なら enterSummary 経由で Loading=true / aggregate Cmd 発行
func TestAkashicHandleKeyListToSummaryOnTab(t *testing.T) {
	reader := &fakeReader{}
	s := State{
		Subview: SubviewList,
		Reader:  reader,
		List: ListState{
			Items: []akashic.WorkflowRunListItem{
				{RunHeader: akashic.RunHeader{RunID: "run-1"}},
			},
		},
	}

	got, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyTab})
	if got.Subview != SubviewSummary {
		t.Fatalf("expected Subview=SubviewSummary after tab on List, got %q", got.Subview)
	}
	if !got.Summary.Loading {
		t.Fatalf("expected Summary.Loading=true on enter (Reader != nil)")
	}
	if cmd == nil {
		t.Fatal("expected non-nil Cmd from enterSummary when Reader != nil")
	}
}

// TestAkashicHandleKeyListToSummaryOnTabWithReaderNilNoCmd は Reader=nil でも
// List → Summary 遷移自体は決定的に発生し、Cmd は nil (graceful) であること。
func TestAkashicHandleKeyListToSummaryOnTabWithReaderNilNoCmd(t *testing.T) {
	s := State{Subview: SubviewList, Reader: nil}

	got, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyTab})
	if got.Subview != SubviewSummary {
		t.Fatalf("expected Subview=SubviewSummary after tab on List even with Reader=nil, got %q", got.Subview)
	}
	if cmd != nil {
		t.Fatalf("expected nil Cmd from enterSummary when Reader=nil (graceful), got %T", cmd)
	}
	if got.Summary.Loading {
		t.Fatalf("expected Summary.Loading=false when Reader=nil, got true")
	}
}

// TestAkashicHandleKeySummaryToListOnTab は Summary → List 遷移を観測する
// (Req 2.4)。
//
// 観測点:
//   - Subview=SubviewSummary で `tab` キー → Subview=SubviewList
//   - Cmd は不要 (List は Reader 既存呼出経路を変更しない)
func TestAkashicHandleKeySummaryToListOnTab(t *testing.T) {
	s := State{
		Subview: SubviewSummary,
		Reader:  &fakeReader{},
		Summary: SummaryState{
			WorkflowBuckets: []akashic.WorkflowAggregateBucket{
				{WorkflowID: "wf-1"},
			},
		},
	}

	got, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyTab})
	if got.Subview != SubviewList {
		t.Fatalf("expected Subview=SubviewList after tab on Summary, got %q", got.Subview)
	}
	if cmd != nil {
		// List への戻りは値型再代入のみで Cmd を発行しない
		t.Fatalf("expected nil Cmd from Summary → List transition, got %T", cmd)
	}
}

// TestAkashicHandleKeyListToDetailOnEnter は List → Detail 遷移を観測する
// (Req 2.5)。
//
// 観測点:
//   - Subview=SubviewList で Cursor が指す run を選択した状態で `enter` キー
//   - 遷移後 Subview=SubviewDetail / Detail.RunID = 選択行の RunID
//   - Reader != nil なら RunDetailCmd が tea.Batch (or 単発) で返る
func TestAkashicHandleKeyListToDetailOnEnter(t *testing.T) {
	reader := &fakeReader{}
	s := State{
		Subview: SubviewList,
		Reader:  reader,
		List: ListState{
			Items: []akashic.WorkflowRunListItem{
				{RunHeader: akashic.RunHeader{RunID: "run-A", WorkflowID: "wf-A"}},
				{RunHeader: akashic.RunHeader{RunID: "run-B", WorkflowID: "wf-B"}},
			},
			Cursor: 1, // run-B を選択中
		},
	}

	got, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if got.Subview != SubviewDetail {
		t.Fatalf("expected Subview=SubviewDetail after enter on List, got %q", got.Subview)
	}
	if got.Detail.RunID != "run-B" {
		t.Fatalf("expected Detail.RunID=run-B (cursor selected), got %q", got.Detail.RunID)
	}
	if got.Detail.Focus != DetailFocusAgentCalls {
		t.Fatalf("expected Detail.Focus=DetailFocusAgentCalls on Detail entry, got %q", got.Detail.Focus)
	}
	if !got.Detail.Loading {
		t.Fatalf("expected Detail.Loading=true on enterDetail (Reader != nil)")
	}
	if cmd == nil {
		t.Fatal("expected non-nil Cmd (RunDetailCmd) on List → Detail transition")
	}
}

// TestAkashicHandleKeyListEnterIsNoOpWhenItemsEmpty は Items が空のとき
// `enter` で Detail 遷移しないこと (= Subview 不変、Cmd=nil) を観測する。
//
// 動機: 選択対象の run が無い状態で誤って Detail へ遷移すると Detail.RunID="" になり、
// Reader.Detail を「空 RunID で叩く」異常パスに入る。enterDetail を呼ばない不変式を担保する。
func TestAkashicHandleKeyListEnterIsNoOpWhenItemsEmpty(t *testing.T) {
	s := State{
		Subview: SubviewList,
		Reader:  &fakeReader{},
		List:    ListState{Items: nil},
	}

	got, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if got.Subview != SubviewList {
		t.Fatalf("expected Subview=SubviewList unchanged when Items empty, got %q", got.Subview)
	}
	if cmd != nil {
		t.Fatalf("expected nil Cmd when Items empty, got %T", cmd)
	}
	if got.Detail.RunID != "" {
		t.Fatalf("expected Detail.RunID='' when Items empty, got %q", got.Detail.RunID)
	}
}

// TestAkashicHandleKeyDetailToListOnBackspace は Detail → List 遷移を観測する
// (Req 2.6)。
//
// 観測点:
//   - Subview=SubviewDetail で `backspace` キー → Subview=SubviewList
//   - Cmd は不要 (List 状態の値型を保持して戻る)
//   - 既存 Detail.HandoffErr / Detail.Err はクリアしない (Detail 復帰時に再評価される設計)
func TestAkashicHandleKeyDetailToListOnBackspace(t *testing.T) {
	s := State{
		Subview: SubviewDetail,
		Reader:  &fakeReader{},
		Detail: DetailState{
			RunID: "run-7",
			Focus: DetailFocusHeader,
		},
	}

	got, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyBackspace})
	if got.Subview != SubviewList {
		t.Fatalf("expected Subview=SubviewList after backspace on Detail, got %q", got.Subview)
	}
	if cmd != nil {
		t.Fatalf("expected nil Cmd from Detail → List transition, got %T", cmd)
	}
}

// TestAkashicHandleKeyTabRoundTripListSummaryListIsDeterministic は List ↔ Summary
// 往復遷移が決定的に行われることを 2 回 tab で観測する (Req 2.3 / 2.4)。
func TestAkashicHandleKeyTabRoundTripListSummaryListIsDeterministic(t *testing.T) {
	s := State{Subview: SubviewList, Reader: nil}

	// List → Summary
	s, _ = s.HandleKey(tea.KeyMsg{Type: tea.KeyTab})
	if s.Subview != SubviewSummary {
		t.Fatalf("step1: expected Subview=SubviewSummary, got %q", s.Subview)
	}

	// Summary → List
	s, _ = s.HandleKey(tea.KeyMsg{Type: tea.KeyTab})
	if s.Subview != SubviewList {
		t.Fatalf("step2: expected Subview=SubviewList, got %q", s.Subview)
	}
}

// TestAkashicHandleKeyListEnterDetailBackspaceListRoundTrip は
// List → Detail → List 往復遷移が決定的であることを観測する (Req 2.5 / 2.6)。
func TestAkashicHandleKeyListEnterDetailBackspaceListRoundTrip(t *testing.T) {
	s := State{
		Subview: SubviewList,
		Reader:  &fakeReader{},
		List: ListState{
			Items: []akashic.WorkflowRunListItem{
				{RunHeader: akashic.RunHeader{RunID: "run-A", WorkflowID: "wf-A"}},
			},
			Cursor: 0,
		},
	}

	// List → Detail
	s, _ = s.HandleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if s.Subview != SubviewDetail || s.Detail.RunID != "run-A" {
		t.Fatalf("step1: expected Subview=SubviewDetail RunID=run-A, got Subview=%q RunID=%q", s.Subview, s.Detail.RunID)
	}

	// Detail → List
	s, _ = s.HandleKey(tea.KeyMsg{Type: tea.KeyBackspace})
	if s.Subview != SubviewList {
		t.Fatalf("step2: expected Subview=SubviewList, got %q", s.Subview)
	}
}

// TestAkashicHandleKeyEscIsNotConsumedBySubviews は Esc キーが State 側
// で消費されない (= Subview 不変 / Cmd=nil) ことを観測する。Esc はモード退出
// として Model 側で処理される (Req 2.7)。
//
// 観測対象は List / Summary / Detail の 3 サブビュー。
func TestAkashicHandleKeyEscIsNotConsumedBySubviews(t *testing.T) {
	cases := []struct {
		name    string
		subview Subview
	}{
		{"List", SubviewList},
		{"Summary", SubviewSummary},
		{"Detail", SubviewDetail},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := State{
				Subview: c.subview,
				Reader:  &fakeReader{},
				Detail: DetailState{
					RunID: "run-1",
					Focus: DetailFocusHeader,
				},
			}
			got, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyEsc})
			if got.Subview != c.subview {
				t.Fatalf("expected Subview unchanged on Esc (Model 側で退出処理), got %q (was %q)", got.Subview, c.subview)
			}
			if cmd != nil {
				t.Fatalf("expected nil Cmd on Esc, got %T", cmd)
			}
		})
	}
}

// --- (b) Phase ガード越し msg ディスパッチ ---
//
// Model 層 (NewModel/Options/Phase*) を握るテストはサブパッケージ化に伴い
// `internal/ui/akashic_model_test.go` (package ui) に移動した。本ファイルでは
// State 単体で観測可能な Subview 遷移キー写像のみを担保する。

// --- 補助: Detail key route との非干渉 ---

// TestAkashicHandleKeyDetailTabStaysWithinDetailFocus は Detail サブビューでの
// `tab` が Detail.Focus 循環 (Header→AgentCalls→…) であって Subview 切替には
// ならないこと (= List/Summary 経路の `tab` と排他であること) を観測する。
//
// 動機: List/Summary では `tab` が Subview 遷移キーだが、Detail では既存
// Task 6.1 で Focus 循環の役割を持つ。Subview ベースのディスパッチで
// 干渉が起きないことを担保する。
func TestAkashicHandleKeyDetailTabStaysWithinDetailFocus(t *testing.T) {
	s := State{
		Subview: SubviewDetail,
		Reader:  &fakeReader{},
		Detail: DetailState{
			RunID: "run-1",
			Focus: DetailFocusHeader,
		},
	}

	got, _ := s.HandleKey(tea.KeyMsg{Type: tea.KeyTab})
	if got.Subview != SubviewDetail {
		t.Fatalf("expected Subview=SubviewDetail unchanged on Detail tab, got %q", got.Subview)
	}
	if got.Detail.Focus != DetailFocusAgentCalls {
		t.Fatalf("expected Focus=DetailFocusAgentCalls (next from Header), got %q", got.Detail.Focus)
	}
}

// TestAkashicHandleKeyTransitionsDoNotReturnTeaQuit は本タスクで追加する遷移
// キー (List/Summary tab, List enter, Detail backspace) が一切 tea.Quit を
// 返さないことを観測する (Req 11.5)。
func TestAkashicHandleKeyTransitionsDoNotReturnTeaQuit(t *testing.T) {
	cases := []struct {
		name  string
		state State
		key   tea.KeyMsg
	}{
		{
			name: "List_tab",
			state: State{
				Subview: SubviewList,
				Reader:  &fakeReader{},
			},
			key: tea.KeyMsg{Type: tea.KeyTab},
		},
		{
			name: "Summary_tab",
			state: State{
				Subview: SubviewSummary,
				Reader:  &fakeReader{},
			},
			key: tea.KeyMsg{Type: tea.KeyTab},
		},
		{
			name: "List_enter",
			state: State{
				Subview: SubviewList,
				Reader:  &fakeReader{},
				List: ListState{
					Items: []akashic.WorkflowRunListItem{
						{RunHeader: akashic.RunHeader{RunID: "run-1"}},
					},
				},
			},
			key: tea.KeyMsg{Type: tea.KeyEnter},
		},
		{
			name: "Detail_backspace",
			state: State{
				Subview: SubviewDetail,
				Reader:  &fakeReader{},
				Detail: DetailState{
					RunID: "run-1",
					Focus: DetailFocusHeader,
				},
			},
			key: tea.KeyMsg{Type: tea.KeyBackspace},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, cmd := c.state.HandleKey(c.key)
			if cmd == nil {
				return
			}
			msg := cmd()
			if _, isQuit := msg.(tea.QuitMsg); isQuit {
				t.Fatalf("HandleKey on %s must not return tea.Quit", c.name)
			}
		})
	}
}

// 安全網: 未使用 import に対するコンパイル安全弁
var _ = errors.Is
