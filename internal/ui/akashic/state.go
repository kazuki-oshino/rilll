// Package akashic のアカシックモード状態定義。
//
// design.md の Service Interface / ListState / SummaryState /
// DetailState / ArtifactState 各セクションの構造体定義に準拠する。
package akashic

import (
	tea "github.com/charmbracelet/bubbletea"

	record "github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflow"
)

// Subview は 3 サブビューの識別子 (design.md AkashicSubview 参照)。
type Subview string

const (
	// SubviewList は Run 一覧サブビュー。
	SubviewList Subview = "list"
	// SubviewSummary は集計サマリサブビュー。
	SubviewSummary Subview = "summary"
	// SubviewDetail は Run 詳細サブビュー。
	SubviewDetail Subview = "detail"
)

// ListSort は Run 一覧の TUI ラベルとして提供するソート軸 (Req 3.6)。
//
// 各値は akashic-query spec が `ListOptions.OrderBy` / `Direction` で
// 提供する列挙値に 1:1 でマッピングされ、Reader.List の SQL `ORDER BY`
// 句で全期間にわたり安定ソートされる。本 spec はクライアントサイド
// ソートを行わない (design.md Preconditions 参照)。
type ListSort string

const (
	// SortNewest は新しい順 (OrderBy=StartedAt, Direction=Desc)。akashic-query 既定。
	SortNewest ListSort = "newest"
	// SortCost はコスト順 (OrderBy=Cost, Direction=Desc)。
	SortCost ListSort = "cost"
	// SortDuration は duration 順 (OrderBy=Duration, Direction=Desc)。
	SortDuration ListSort = "duration"
)

// emptyRunListMessage は Run 一覧が空集合のときに UI と Update / View / テストで
// 共通参照する文言 (Req 3.9)。文言を 1 箇所に集約することで、表示と判定の
// ドリフトを防ぐ。
const emptyRunListMessage = "該当 run がありません"

// ListFilter は Run 一覧サブビューのフィルタ条件 (Req 3.3〜3.5)。
//
//   - WorkflowID: 空文字 → 全 workflow
//   - Range: TimeRange (Since/Until のいずれかまたは両方)
//   - Success: nil → 両方 / true → 成功のみ / false → 失敗のみ
type ListFilter struct {
	WorkflowID string
	Range      record.TimeRange
	Success    *bool
}

// ListState は Run 一覧サブビューの状態 (Req 3)。
//
// Loading / Err / EmptyMessage は排他的にセットされる
// (一度に有効になるのは最大 1 つ)。
type ListState struct {
	Filter       ListFilter
	Sort         ListSort
	Limit        int                          // 既定 record.DefaultListLimit
	Offset       int                          // 現在ページの先頭オフセット
	Items        []record.WorkflowRunListItem // 表示順 = Reader 戻り順 (クライアント側ソートはしない)
	NextOffset   *int                         // ListResult.NextOffset
	Cursor       int                          // 0..len(Items)-1
	Loading      bool
	Err          error
	EmptyMessage string
}

// SummaryState は集計サマリサブビューの状態 (Req 4)。
//
// 集計対象は全期間 / 全 run で、Run 一覧サブビューのフィルタとは独立 (Req 4.4)。
type SummaryState struct {
	WorkflowBuckets []record.WorkflowAggregateBucket
	ProviderBuckets []record.ProviderAggregateBucket
	Loading         bool
	Err             error
	EmptyMessage    string
}

// ArtifactState は Run 詳細内の成果物プレビュー状態 (Req 6)。
//
// PreviewBytes / PreviewWarning / Err は排他的にセットされる
// (上限超過警告 / 削除済み警告 / 通常本体 のいずれか 1 つ)。
//
// Unavailable は ArtifactReader が未注入 (= State.ArtifactReader == nil)
// のときに enterDetail で立てられる graceful フラグ (Req 11.3)。View 側は本フラグ
// で「成果物データが利用できません」と表示し、HandleKey / Update は本フラグが
// true のとき GetArtifacts / ReadArtifact 系 Cmd を一切発行しない。
//
// ArtifactState は子構造体として DetailState 内に存在し、親の
// ArtifactReader 参照に直接到達できないため、Detail 入口で確定したフラグを
// 本フィールドに保持して View / HandleKey / テスト観測の三者が同じ判定を共有する。
type ArtifactState struct {
	Rows           []record.ArtifactRow
	SelectedIndex  int
	PreviewPath    string // 表示中の成果物パス
	PreviewBytes   []byte
	PreviewWarning string // 上限超過 / 成果物が見つかりません 等
	EmptyMessage   string // 「成果物なし」(Req 6.9)
	Loading        bool
	Err            error
	Unavailable    bool // ArtifactReader 未配線 graceful フラグ (Req 11.3)
}

// DetailFocus は Run 詳細サブビュー内のフォーカス位置 (Task 6.1)。
//
// design.md の「Detail 内のサブセクション切替 (workflow_run ヘッダ /
// agent_calls / raw_events / 成果物リスト)」に対応する 4 値の閉集合。
// HandleKey がカーソル移動 / ページ送りキーを「現在 Focus がどこか」で
// 切り替えるためのディスパッチ識別子として使う。
//
// 不変条件: Subview=SubviewDetail のとき必ず 4 値のいずれか。Subview が
// 切り替わったタイミングで `DetailFocusHeader` に初期化する (enterDetail 参照)。
type DetailFocus string

const (
	// DetailFocusHeader は workflow_run ヘッダ領域 (run/cwd/期間/トータル) にフォーカス。
	DetailFocusHeader DetailFocus = "header"
	// DetailFocusAgentCalls は agent_calls 一覧にフォーカス。j/k/↑/↓ で AgentCallsCursor を動かす。
	DetailFocusAgentCalls DetailFocus = "agent_calls"
	// DetailFocusRawEvents は raw_events 一覧にフォーカス。j/k/↑/↓ で RawEventsCursor を動かし、
	// n/p で raw_events ページを進退する (Req 5.3)。
	DetailFocusRawEvents DetailFocus = "raw_events"
	// DetailFocusArtifacts は成果物リストにフォーカス (Task 6.2 で実装する Artifact 操作の入口)。
	DetailFocusArtifacts DetailFocus = "artifacts"
)

// DetailState は Run 詳細サブビューの状態 (Req 5)。
//
//   - RunID: 詳細表示対象の workflow_run_id (Subview=SubviewDetail のときのみ非空)
//   - Detail: Reader.Detail の戻り値。未取得時 / Reader=nil 時は nil
//   - RawEventsOffset: 現在ページの先頭オフセット (前ページ算出の根拠、Task 6.1 で導入)
//   - NextRawEventsOffset: raw_events 次ページオフセット (nil なら次ページなし、Req 5.3)
//   - Artifact: Run 詳細内の成果物プレビュー状態 (Req 6、本タスクでは初期値のみ)
//   - HandoffErr: 改善ワークフロー hand-off 失敗時の inline 通知 (Req 7.7、Task 7.1 で使用)
//   - Focus / AgentCallsCursor / RawEventsCursor: Detail フォーカス・カーソル位置 (Task 6.1)
//   - Loading / Err: 排他的状態 (Loading 中 / Err 設定時 / 通常表示)
//
// 不変条件: Loading=false のとき Err と Detail のいずれか一方のみが意味を持つ
// (両方 nil なら未取得のままのケース)。Err が ErrWorkflowRunNotFound のときは
// View 側で「該当 run が存在しない」を表示する (Req 5.4)。
type DetailState struct {
	RunID               string
	Detail              *record.RunDetail
	RawEventsOffset     int
	NextRawEventsOffset *int
	Artifact            ArtifactState
	HandoffErr          error
	Loading             bool
	Err                 error

	// Focus / カーソルは Subview=SubviewDetail のときのみ意味を持つ (Task 6.1)。
	Focus            DetailFocus
	AgentCallsCursor int
	RawEventsCursor  int

	// AgentCallPaneOpen / RawEventPaneOpen は agent_calls / raw_events の選択行
	// 詳細 pane (Prompt / ResultSummary / Error / FailureCategory / ResultPayload /
	// Payload (JSON) など 1 行に収まらない情報) の inline トグル状態。
	//
	// 不変条件:
	//   - Cursor 移動キー (j/k/↑/↓) で自動 false (別行で開いたまま残らない)
	//   - Focus 切替 (Tab/Shift+Tab) で両 pane=false (別セクションに移ったらクリア)
	//   - Detail 突入直後 (enterDetail) は zero value=false
	//
	// 動機: Req 5 (失敗原因や挙動を外部 SQL クライアントなしで確認できる) を
	// 実装でも満たすため、選択行で Enter を押したら inline pane が agent_calls /
	// raw_events 一覧の直下にトグル表示される (state 値の bool 1 つで表現)。
	AgentCallPaneOpen bool
	RawEventPaneOpen  bool
}

// Deps は NewState の構築引数。TUI シェル側 (internal/ui) から
// akashic mode に必要な依存と、Bubble Tea msg / ask の橋渡しを集約する。
//
// Reader / ArtifactReader は record パッケージ (internal/akashic) のもので、
// nil 許容 (Req 11.1〜11.3 の graceful 経路)。
//
// ForwardEvent は LaunchHandoffCmd 内で onEvent から受け取った WorkflowEvent を
// TUI シェル側 events チャネルへ流すコールバック。ui パッケージ側で
// `func(ev) { events <- workflowEventMsg{event: ev} }` のクロージャを渡す。
// nil 許容 (テスト経路で no-op にする場合)。
//
// Ask は改善ワークフローが SelectParams を要求した場合に使う workflow.WorkflowAsk
// 実装。ui 側の workflowAsk (events チャネル経由で promptMsg を流す実装) を
// そのまま渡す。nil 許容 (テスト経路では noopAsk を渡す)。
type Deps struct {
	Reader         record.Reader
	ArtifactReader record.ArtifactReader
	Workflows      []workflow.Definition
	CWD            string
	ForwardEvent   EventForwarder
	Ask            workflow.WorkflowAsk
}

// State はアカシックモードの全状態 (design.md AkashicState 参照)。
//
// 値型として Model に埋め込み、HandleKey / Update は値で受けて値で返す
// 純関数として動作する (Req 9.2 / 10.1 / 10.2)。
//
// 不変条件: Subview ∈ {SubviewList, SubviewSummary, SubviewDetail} のいずれか 1 つ。
// Detail.RunID は Subview=SubviewDetail のときのみ非空。
type State struct {
	Subview Subview
	List    ListState
	Summary SummaryState
	Detail  DetailState

	// LastHandoff は最後に発行された hand-off パラメータ (Req 10.4 観測点)。
	LastHandoff *workflow.HandoffParams

	// 依存性 (nil 許容、Req 11 graceful)。
	Reader         record.Reader
	ArtifactReader record.ArtifactReader
	Workflows      []workflow.Definition
	CWD            string

	// ForwardEvent / Ask は改善ワークフロー hand-off 起動 (Task 7.1) で
	// LaunchHandoffCmd に渡す。ui シェル側の events / workflowAsk と接続して
	// hand-off 後の WorkflowEvent が既存の waitForEvent 経路に流れ込む不変式を
	// 維持する。
	ForwardEvent EventForwarder
	Ask          workflow.WorkflowAsk
}

// NewState は PhaseAkashic 突入時の初期状態を返す。
// Reader / ArtifactReader が nil のときも graceful に初期化される (Req 11.1)。
//
// deps は ui パッケージ側からの依存性注入。ForwardEvent / Ask には
// `func(ev) { events <- workflowEventMsg{event: ev} }` / `workflowAsk{events}` を
// 渡し、hand-off 後の WorkflowEvent が既存の `waitForEvent` 経路で
// 受信される不変式を維持する。
//
// 初回 List 取得 Cmd (Task 8.1, Req 1.1 / 9.1):
//   - Reader != nil のとき: `ListRunsCmd(reader, record.ListOptions{})` を返し、
//     List.Loading=true を立てる。`ListRunsCmd` 内部で Limit が
//     `record.DefaultListLimit` (=50) に強制される (Req 9.5)。並び順は
//     Reader 既定 (started_at 降順) に従う (Task 1.1 ゲートで akashic-query 側に
//     OrderBy/Direction が拡張されたら、本関数の呼び出し側で Sort を渡せるよう
//     拡張する。現状の本 spec ではゲート未充足のためデフォルト並びのみ提供)。
//   - Reader == nil のとき: Cmd=nil を返し、Loading は立てない。View 側で
//     「履歴データが利用できません」を即座に表示する graceful 経路 (Req 11.2)。
func NewState(deps Deps) (State, tea.Cmd) {
	state := State{
		Subview:        SubviewList,
		Reader:         deps.Reader,
		ArtifactReader: deps.ArtifactReader,
		Workflows:      deps.Workflows,
		CWD:            deps.CWD,
		ForwardEvent:   deps.ForwardEvent,
		Ask:            deps.Ask,
	}
	if deps.Reader == nil {
		// Req 11.2: Reader 未配線時は Cmd を発行しない (graceful 表示は View 側)。
		return state, nil
	}
	// Req 1.1 / 9.1: 初回 List 取得を発行し、応答中表示のために Loading=true を立てる。
	state.List.Loading = true
	return state, ListRunsCmd(deps.Reader, record.ListOptions{})
}

// HandleKey はキー入力を消費し、必要なら tea.Cmd を返す純関数。
//
// 現在 Subview の値に応じて専用ハンドラへディスパッチする:
//   - SubviewList    → handleListKey (Task 8.2: tab で Summary、enter で Detail に遷移)
//   - SubviewSummary → handleSummaryKey (Task 8.2: tab で List に戻る)
//   - SubviewDetail  → handleDetailKey (Task 6.1: Focus 循環 / カーソル / ページ送り、
//     Task 8.2: backspace で List に戻る)
//
// 親 (Model.handleKey) が PhaseAkashic ガード越しにのみ本メソッドを呼ぶ前提と
// 整合する。Esc / q はモード退出として Model 側で処理されるため、本メソッドは
// それらを消費しない (Req 2.7)。
func (s State) HandleKey(msg tea.KeyMsg) (State, tea.Cmd) {
	switch s.Subview {
	case SubviewList:
		return s.handleListKey(msg)
	case SubviewSummary:
		return s.handleSummaryKey(msg)
	case SubviewDetail:
		return s.handleDetailKey(msg)
	}
	return s, nil
}

// Update / View の実装は update.go / view.go に分離されている
// (design.md File Structure Plan に準拠)。本ファイルは型と初期化のみを保持する。
