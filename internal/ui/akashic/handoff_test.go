// Package ui のアカシックモード Run 詳細サブビューにおける改善ワークフロー
// hand-off 起動 (Task 7.1) のテスト。
//
// 本ファイルは Run 詳細上の hand-off キー (`i`) によって、
// (a) `m.Workflows` から `Definition.ID == "improve-workflow"` を
//
//	線形検索し、見つかった場合に
//	`params := workflow.HandoffParams{TargetWorkflowID: detail.Run.WorkflowID,
//	 RunIDs: []string{detail.Run.RunID}}` を構築し
//	`LaunchHandoffCmd(events, def, params, cwd)` を発行すること、
//
// (b) `State.LastHandoff = &params` を設定すること (Req 10.4 観測点)、
// (c) `HandoffStartedMsg` 受信時 (Model.Update 経由) に
//
//	`Model.Phase = PhaseWorkflow` に遷移すること (Req 7.8)、
//
// (d) improve-workflow が未登録 / `HandoffFailedMsg` のときは
//
//	モードを抜けず `Detail.HandoffErr` にエラー文を格納し
//	`tea.Quit` を発行しないこと (Req 7.7 / 11.5)、
//
// (e) MVP 不変式として `RunIDs` の長さが必ず 1 であること (Req 7.6)、
// を観測する。
package akashic

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/config"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
)

// makeAkashicHandoffState は Run 詳細サブビューの上で hand-off を起動できる状態の
// State を返すヘルパ。
//
//   - Subview = SubviewDetail / Focus = Header (i キーは Focus に依存しない単一エントリ)
//   - Detail.Detail に最低限の RunHeader (RunID / WorkflowID) を載せる
//   - Workflows / CWD は呼び出し側が任意に設定できるよう引数で受ける
//
// `i` キーの hand-off 起動は Detail.Detail (= 取得済みの RunDetail) が
// 必要なため、本ヘルパで Detail を非 nil にして渡す。
//
// EventForwarder と Ask は本ファイル内 State 単体テストでは観測対象にしないため
// noop callback / noopAsk を注入する (LaunchHandoffCmd 自体の挙動は
// handoff_launch_test.go の専用テストで担保される)。
func makeAkashicHandoffState(workflows []workflow.Definition) State {
	now := time.Now()
	detail := akashic.RunDetail{
		Run: akashic.RunHeader{
			RunID:      "run-7",
			WorkflowID: "wf-target",
			StartedAt:  now,
		},
	}
	return State{
		Subview:      SubviewDetail,
		Workflows:    workflows,
		CWD:          "/work/cwd",
		ForwardEvent: func(_ workflow.WorkflowEvent) {},
		Ask:          noopAsk{},
		Detail: DetailState{
			RunID:  "run-7",
			Focus:  DetailFocusHeader,
			Detail: &detail,
		},
	}
}

// improvedDef は improve-workflow として `m.Workflows` に並べる fake Definition を返す。
// fn は受領した ctx / cwd を観測できる closure 形式 (capture pattern と同じ手法)。
func improvedDef(fn workflow.Func) workflow.Definition {
	return workflow.Definition{
		ID:    "improve-workflow",
		Label: "Self Improve",
		Fn:    fn,
	}
}

// ---- HandleKey: 'i' キー成功経路 (improve-workflow 登録あり) ----

// TestAkashicDetailHandoffKeyWithRegisteredWorkflowDispatchesLaunchCmd は
// 観測完了条件 (a) (b) (c の前段) を満たす:
//   - i キー押下で hand-off Cmd が返ること
//   - LastHandoff に {TargetWorkflowID = Detail.Run.WorkflowID,
//     RunIDs = [Detail.Run.RunID]} が設定されていること
//   - RunIDs の長さが必ず 1 であること (Req 7.6)
//   - Cmd を invoke すると improve-workflow の Fn が呼ばれ、ctx 上に
//     HandoffParams が乗っていることが確認できる
func TestAkashicDetailHandoffKeyWithRegisteredWorkflowDispatchesLaunchCmd(t *testing.T) {
	fn := &fakeWorkflowFn{}
	def := improvedDef(fn.capture())

	// improve-workflow 以外も並べることで「線形検索で正しい ID を選ぶ」
	// 不変式 (Req 7.1) を観測する。
	other := workflow.Definition{
		ID:    "other",
		Label: "Other",
		Fn:    (&fakeWorkflowFn{}).capture(),
	}
	s := makeAkashicHandoffState([]workflow.Definition{other, def})

	got, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}})
	if cmd == nil {
		t.Fatal("expected non-nil Cmd from 'i' when improve-workflow is registered")
	}

	// LastHandoff の構造観測 (Req 10.4)
	if got.LastHandoff == nil {
		t.Fatal("expected LastHandoff to be set after hand-off start")
	}
	wantParams := workflow.HandoffParams{
		TargetWorkflowID: "wf-target",
		RunIDs:           []string{"run-7"},
	}
	if !reflect.DeepEqual(*got.LastHandoff, wantParams) {
		t.Fatalf("expected LastHandoff=%+v, got %+v", wantParams, *got.LastHandoff)
	}

	// (e) Req 7.6: RunIDs の長さが必ず 1
	if len(got.LastHandoff.RunIDs) != 1 {
		t.Fatalf("expected RunIDs length=1 (Req 7.6 MVP fixed length), got %d", len(got.LastHandoff.RunIDs))
	}

	// hand-off 失敗のエラーは無いはず
	if got.Detail.HandoffErr != nil {
		t.Fatalf("expected Detail.HandoffErr=nil on success path, got %v", got.Detail.HandoffErr)
	}

	// Cmd を invoke して fake Fn が呼ばれていること、ctx 上に HandoffParams が
	// 乗っていることを観測する。
	msg := cmd()
	if _, ok := msg.(HandoffStartedMsg); !ok {
		t.Fatalf("expected HandoffStartedMsg from cmd(), got %T", msg)
	}
	if fn.callCount != 1 {
		t.Fatalf("expected improve-workflow Fn to be called once, got %d", fn.callCount)
	}
	if fn.receivedCwd != "/work/cwd" {
		t.Fatalf("expected cwd=/work/cwd, got %q", fn.receivedCwd)
	}
	if fn.receivedCtx == nil {
		t.Fatal("expected non-nil ctx delivered to def.Run")
	}
	gotParams, ok := workflow.HandoffFromContext(fn.receivedCtx)
	if !ok {
		t.Fatal("expected HandoffFromContext to return ok=true on injected ctx")
	}
	if !reflect.DeepEqual(gotParams, wantParams) {
		t.Fatalf("expected ctx params=%+v, got %+v", wantParams, gotParams)
	}
}

// ---- HandleKey: 'i' キー失敗経路 (improve-workflow 未登録) ----

// TestAkashicDetailHandoffKeyWithoutRegisteredWorkflowSetsHandoffErr は
// 観測完了条件 (b 失敗側) を満たす:
//   - improve-workflow が `m.Workflows` に存在しないとき、
//     Cmd は発行されない (nil) こと
//   - Detail.HandoffErr に「improve-workflow が登録されていません」
//     相当の非 nil エラーが格納されること (Req 7.7)
//   - LastHandoff は変更されない (起動経路に乗っていないため)
//   - tea.Quit は返さない (Req 7.7 / 11.5)
func TestAkashicDetailHandoffKeyWithoutRegisteredWorkflowSetsHandoffErr(t *testing.T) {
	// improve-workflow は含まれない
	other := workflow.Definition{
		ID:    "other",
		Label: "Other",
		Fn:    (&fakeWorkflowFn{}).capture(),
	}
	s := makeAkashicHandoffState([]workflow.Definition{other})

	got, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}})
	if cmd != nil {
		t.Fatalf("expected nil Cmd when improve-workflow is not registered (Req 7.7), got %T", cmd)
	}

	if got.Detail.HandoffErr == nil {
		t.Fatal("expected Detail.HandoffErr to be set when improve-workflow is missing (Req 7.7)")
	}
	// 文言の SSoT 観測: 「improve-workflow が登録されていません」を含むこと
	// (UI 表示時に View 側が err.Error() を表示する経路と整合)。
	if msg := got.Detail.HandoffErr.Error(); !strings.Contains(msg, "improve-workflow") {
		t.Fatalf("expected HandoffErr message to mention improve-workflow, got %q", msg)
	}

	if got.LastHandoff != nil {
		t.Fatalf("expected LastHandoff=nil on failure (no Cmd issued), got %+v", *got.LastHandoff)
	}
}

// TestAkashicDetailHandoffKeyDoesNotReturnTeaQuit は両経路 (登録あり / 登録なし) で
// tea.Quit が一度も返らないことを観測する (Req 7.7 / 11.5)。
func TestAkashicDetailHandoffKeyDoesNotReturnTeaQuit(t *testing.T) {
	fn := &fakeWorkflowFn{}
	def := improvedDef(fn.capture())

	cases := []struct {
		name      string
		workflows []workflow.Definition
	}{
		{name: "registered", workflows: []workflow.Definition{def}},
		{name: "not-registered", workflows: nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := makeAkashicHandoffState(c.workflows)
			_, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}})
			if cmd == nil {
				return
			}
			gotMsg := cmd()
			if _, isQuit := gotMsg.(tea.QuitMsg); isQuit {
				t.Fatalf("HandleKey('i') must not return tea.Quit on case=%s", c.name)
			}
		})
	}
}

// TestAkashicDetailHandoffKeyNoOpWhenDetailNotLoaded は Detail.Detail が未取得 (nil)
// のとき、i キーが no-op (Cmd 発行なし、LastHandoff / HandoffErr 変更なし) で
// あることを観測する。
//
// 動機: hand-off は run の workflow_id / run_id を必要とするため、Detail が未取得の
// うちは起動を抑止する (= nil pointer dereference 防止 + UX 上「読み込み中の操作」
// として扱う)。
func TestAkashicDetailHandoffKeyNoOpWhenDetailNotLoaded(t *testing.T) {
	fn := &fakeWorkflowFn{}
	def := improvedDef(fn.capture())

	s := makeAkashicHandoffState([]workflow.Definition{def})
	s.Detail.Detail = nil // 未取得状態

	got, cmd := s.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}})
	if cmd != nil {
		t.Fatalf("expected nil Cmd when Detail.Detail is nil, got %T", cmd)
	}
	if got.LastHandoff != nil {
		t.Fatalf("expected LastHandoff=nil when Detail not loaded, got %+v", *got.LastHandoff)
	}
	if got.Detail.HandoffErr != nil {
		t.Fatalf("expected Detail.HandoffErr=nil (silent no-op) when Detail not loaded, got %v", got.Detail.HandoffErr)
	}
	if fn.callCount != 0 {
		t.Fatalf("expected workflow Fn never to be called when Detail not loaded, got %d", fn.callCount)
	}
}

// ---- State.Update: HandoffFailedMsg ----

// TestAkashicHandoffFailedMsgPopulatesHandoffErr は HandoffFailedMsg を受信した
// ときに Detail.HandoffErr が設定され、Subview / Phase はそのまま (= Detail に
// 留まる) ことを観測する (Req 7.7)。
func TestAkashicHandoffFailedMsgPopulatesHandoffErr(t *testing.T) {
	fn := &fakeWorkflowFn{}
	def := improvedDef(fn.capture())
	s := makeAkashicHandoffState([]workflow.Definition{def})

	wantErr := errors.New("boom hand-off")
	params := workflow.HandoffParams{
		TargetWorkflowID: "wf-target",
		RunIDs:           []string{"run-7"},
	}

	got, cmd := s.Update(HandoffFailedMsg{Params: params, Err: wantErr})

	// Req 7.7 / 11.5: tea.Quit を返さない
	if cmd != nil {
		t.Fatalf("expected nil Cmd from HandoffFailedMsg, got %T", cmd)
	}
	if got.Subview != SubviewDetail {
		t.Fatalf("expected Subview=SubviewDetail (留まる), got %q", got.Subview)
	}
	if !errors.Is(got.Detail.HandoffErr, wantErr) {
		t.Fatalf("expected HandoffErr=%v, got %v", wantErr, got.Detail.HandoffErr)
	}
}

// ---- 安全網: 仕様外の dependency 観測 ----

// TestNewStateAcceptsForwardEventCallback は NewState が ForwardEvent / Ask 依存を
// 受け取り State に保持することを観測する (Task 7.1 で追加した依存)。
//
// 動機: Run 詳細から improve-workflow への hand-off は LaunchHandoffCmd 経由で
// EventForwarder callback へ WorkflowEvent を流す必要がある。State は HandleKey
// から LaunchHandoffCmd を呼び出すため、forward / ask を依存性として保持する。
//
// 旧 API (Events chan<- tea.Msg) は TUI シェル側の tea.Msg 型に直接結合していたが、
// サブパッケージ化に伴い callback 注入 (EventForwarder) に置換した (issue #17)。
func TestNewStateAcceptsForwardEventCallback(t *testing.T) {
	received := make(chan workflow.WorkflowEvent, 4)
	forward := func(ev workflow.WorkflowEvent) { received <- ev }
	s, cmd := NewState(Deps{
		CWD:          "/cwd",
		ForwardEvent: forward,
		Ask:          noopAsk{},
	})
	if cmd != nil {
		t.Fatalf("expected nil Cmd from NewState (Reader=nil), got %T", cmd)
	}
	if s.ForwardEvent == nil {
		t.Fatal("expected State.ForwardEvent to be set from NewState Deps")
	}
	if s.Ask == nil {
		t.Fatal("expected State.Ask to be set from NewState Deps")
	}

	// State 経由で forward callback が呼べることを観測する
	go s.ForwardEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "hi"})
	select {
	case got := <-received:
		if got.Text != "hi" {
			t.Fatalf("expected forwarded text=hi, got %q", got.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("expected forward callback delivery via State.ForwardEvent")
	}
}

// 未使用 import に対するコンパイル安全弁 (context は workflow.HandoffFromContext
// 経由で間接的に使うが、本ファイル内で context 値を直接握る箇所がないため、
// 念のため _ 使用で Go の未使用エラーを抑止する)。
var _ = context.Background
var _ = config.Config{}
var _ = provider.ProviderClaude
