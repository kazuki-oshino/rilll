// Package akashic のアカシックモード Update ロジック。
//
// State.Update は副作用を持たず、必要なら tea.Cmd を戻り値で返す
// (Bubble Tea の純粋関数構造を維持: Req 9.2 / 10.1 / 10.2)。I/O は一切行わず、
// io.go のヘルパに委譲する。
//
// ListRunsMsg / AggregateBy{Workflow,Provider}Msg の errReaderUnavailable
// sentinel は graceful 経路として Err に貯めない: View 側で s.Reader == nil を
// 検知して「履歴データが利用できません」を表示する (Req 11.2)。
package akashic

import (
	"errors"
	"io/fs"

	tea "github.com/charmbracelet/bubbletea"

	record "github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflow"
)

// handoffImproveWorkflowID は hand-off 先となる改善ワークフローの ID
// (Task 7.1, design.md Hand-off 動作 §)。`State.Workflows` 内を線形検索する際の
// SSoT として 1 箇所に集約することで、後続タスクや brief 側との表記ずれを防ぐ。
//
// 値は workflow-self-improve spec の実装側 (`improve.improveWorkflowID`) と
// 一致させる必要がある。両者がずれると `i` キーで起動ができなくなるため、
// 実装側 (improve-workflow パッケージ) の ID 変更時は本定数も追従する。
const handoffImproveWorkflowID = "improve-workflow"

// errHandoffWorkflowNotRegistered は `State.Workflows` 内に
// `Definition.ID == handoffImproveWorkflowID` が存在しないときに
// `Detail.HandoffErr` に格納される sentinel error (Task 7.1, Req 7.7)。
//
// 文言は実装側 ID `improve-workflow` を主表記として揃え、View / テスト /
// ログの三者で同一文言を共有する。
var errHandoffWorkflowNotRegistered = errors.New("improve-workflow が登録されていません")

// emptySummaryMessage は集計サマリが「対象 run なし」を示すときに UI と Update /
// View / テストで共通参照する文言 (Req 4.5)。文言を 1 箇所に集約することで
// 表示と判定のドリフトを防ぐ。
const emptySummaryMessage = "集計対象 run がありません"

// runDetailNotFoundMessage は Run 詳細で workflow_run_id が DB に存在しない
// ときに UI / Update / View / テストで共通参照する文言 (Req 5.4)。
//
// 文言の SSoT を 1 箇所に集約することで、View (Task 6.3) と判定 / テスト
// (Task 6.1) のドリフトを防ぐ。
const runDetailNotFoundMessage = "該当 run が存在しない"

// emptyArtifactsMessage は Run 詳細サブビューで成果物が 0 件のときに UI と
// Update / View / テストで共通参照する文言 (Req 6.9)。
//
// 0 件はエラーではなく graceful 表示のため、Err には貯めず本文言を
// ArtifactState.EmptyMessage に格納する。
const emptyArtifactsMessage = "成果物なし"

// previewWarningTooLarge は ReadArtifact が ErrArtifactTooLarge を返したときの
// PreviewWarning 文言 (Req 6.5)。本体は読み込まれず、メタ表示は継続する。
const previewWarningTooLarge = "上限超過のため本体を読み込みませんでした"

// previewWarningMissing は ReadArtifact が ErrArtifactFileMissing /
// fs.ErrNotExist を返したときの PreviewWarning 文言 (Req 6.7)。
//
// ErrArtifactFileMissing は内部で fs.ErrNotExist を wrap している
// (artifact_reader.go) ため、errors.Is は両方で真になる。本ハンドラでは
// 双方を同一経路に集約することで、呼び出し元 (io.go) や Reader 実装の
// 詳細によって挙動が分岐しない不変式を維持する。
const previewWarningMissing = "成果物が見つかりません"

// clamp は value を [min, max] の範囲にクランプする小さな数値ヘルパ。
// パッケージ内のみで利用するため unexported のまま保持する
// (ui パッケージの同名 helper と独立)。
func clamp(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

// Update はアカシックモード関連の tea.Msg を処理し、新しい state と Cmd を返す。
//
// 関連しない tea.Msg を受けた場合は state をそのまま返し Cmd は nil とする
// (Model 側で Phase ガード越しに本メソッドが呼ばれる前提と整合)。
func (s State) Update(msg tea.Msg) (State, tea.Cmd) {
	switch m := msg.(type) {
	case ListRunsMsg:
		return s.applyListRunsMsg(m), nil
	case AggregateByWorkflowMsg:
		return s.applyAggregateByWorkflowMsg(m), nil
	case AggregateByProviderMsg:
		return s.applyAggregateByProviderMsg(m), nil
	case RunDetailMsg:
		return s.applyRunDetailMsg(m), nil
	case GetArtifactsMsg:
		return s.applyGetArtifactsMsg(m), nil
	case ReadArtifactMsg:
		return s.applyReadArtifactMsg(m), nil
	case HandoffStartedMsg:
		return s.applyHandoffStartedMsg(m), nil
	case HandoffFailedMsg:
		return s.applyHandoffFailedMsg(m), nil
	}
	return s, nil
}

// applyHandoffStartedMsg は HandoffStartedMsg を State に反映する純関数
// (Task 7.1, Req 7.8)。
//
// 起動成功時の状態整理として `Detail.HandoffErr` を nil にクリアする
// (前回 hand-off の失敗が表示残ることを防ぐ)。Phase 遷移 (PhaseAkashic →
// PhaseWorkflow) は Model.Update 側の責務で、本ヘルパは Model レイヤから委譲
// された場合の State 内の状態整合のみを担う。
//
// tea.Quit は絶対に発行しない (Req 11.5)。
func (s State) applyHandoffStartedMsg(_ HandoffStartedMsg) State {
	s.Detail.HandoffErr = nil
	return s
}

// applyHandoffFailedMsg は HandoffFailedMsg を State に反映する純関数
// (Task 7.1, Req 7.7)。
//
// 起動失敗時は `Detail.HandoffErr` に msg.Err を格納する。Subview / Phase は
// 一切変更しない (Detail サブビューに留まる contract: Req 7.7)。
//
// tea.Quit は絶対に発行しない (Req 7.7 / 11.5)。
func (s State) applyHandoffFailedMsg(msg HandoffFailedMsg) State {
	s.Detail.HandoffErr = msg.Err
	return s
}

// enterSummary は SubviewSummary への切替時に呼ばれる純関数 (Req 4.1〜4.4 / 11.2)。
//
// 責務:
//   - Subview を SubviewSummary に切り替える
//   - 前回 Summary 表示の stale データ (Buckets / Err / EmptyMessage) をリセット
//     する。再突入時に古い表示が混入しないようにするため
//   - Reader != nil なら Loading=true を立て、AggregateByWorkflowCmd と
//     AggregateByProviderCmd を tea.Batch で同時発行する
//   - Reader == nil なら Cmd を発行せず Loading も立てない (View 側で
//     「履歴データが利用できません」を即出す graceful 経路: Req 11.2)
//
// Aggregate Options は必ず WorkflowAggregateOptions{} / ProviderAggregateOptions{}
// (空) で固定する (Req 4.4: Run 一覧サブビューのフィルタとは独立)。Run 一覧側の
// ListFilter は集計には伝播しない。
func (s State) enterSummary() (State, tea.Cmd) {
	s.Subview = SubviewSummary
	// 再突入時の stale データ排除: 値型 Summary をゼロ値で上書きする。
	// 値型再代入によって、突入後に届いた前回フェッチの戻り msg は
	// 新しい Summary には影響しない (排他的状態セットの構造的保証)。
	s.Summary = SummaryState{}
	if s.Reader == nil {
		// Req 11.2: Reader 未配線時は Cmd を発行しない (View が unavailable 表示)。
		return s, nil
	}
	s.Summary.Loading = true
	// Req 4.4: Aggregate Options は空 (Run 一覧フィルタからは独立)。
	return s, tea.Batch(
		AggregateByWorkflowCmd(s.Reader, record.WorkflowAggregateOptions{}),
		AggregateByProviderCmd(s.Reader, record.ProviderAggregateOptions{}),
	)
}

// applyAggregateByWorkflowMsg は AggregateByWorkflowMsg を SummaryState に
// 反映する純関数 (Req 4.1 / 4.3 / 4.5 / 4.6 / 11.2 / 11.5)。
//
// 排他的状態セット (Loading=false 確定 + 以下のいずれか 1 つ):
//   - 成功: WorkflowBuckets を反映、Err をクリア、両 Buckets が空かを再評価して
//     EmptyMessage を立てる / クリア
//   - errReaderUnavailable: graceful (Err / EmptyMessage / Buckets を変更しない)
//     View が s.Reader == nil を検知して「履歴データが利用できません」を出す
//   - その他のエラー: Err に格納し EmptyMessage をクリア (Req 4.6)
//
// tea.Quit は絶対に発行しない (Req 4.6 / 11.5)。
func (s State) applyAggregateByWorkflowMsg(msg AggregateByWorkflowMsg) State {
	s.Summary.Loading = false
	if msg.Err != nil {
		if errors.Is(msg.Err, errReaderUnavailable) {
			return s
		}
		s.Summary.Err = msg.Err
		s.Summary.EmptyMessage = ""
		return s
	}
	s.Summary.Err = nil
	s.Summary.WorkflowBuckets = msg.Result.Buckets
	s.reconcileSummaryEmpty()
	return s
}

// applyAggregateByProviderMsg は AggregateByProviderMsg を SummaryState に
// 反映する純関数。挙動は applyAggregateByWorkflowMsg と対称
// (Req 4.2 / 4.3 / 4.5 / 4.6 / 11.2 / 11.5)。
func (s State) applyAggregateByProviderMsg(msg AggregateByProviderMsg) State {
	s.Summary.Loading = false
	if msg.Err != nil {
		if errors.Is(msg.Err, errReaderUnavailable) {
			return s
		}
		s.Summary.Err = msg.Err
		s.Summary.EmptyMessage = ""
		return s
	}
	s.Summary.Err = nil
	s.Summary.ProviderBuckets = msg.Result.Buckets
	s.reconcileSummaryEmpty()
	return s
}

// reconcileSummaryEmpty は WorkflowBuckets / ProviderBuckets の両方が空のときに
// 限り EmptyMessage を立てる (Req 4.5)。片方でも非空なら EmptyMessage はクリア
// する。
//
// 動機: 2 つの Aggregate Cmd は独立に戻ってくるため、片側が早く返って空、もう
// 片側が遅く返って非空、という遷移が起こりうる。各 msg arrival で「現状」を
// 再評価することで「集計対象 run がない」を立てる条件 (= 全集計が空) を
// 一貫して維持する。
//
// 値型 receiver にしないと s.Summary の更新が呼び出し側に伝わらないため
// pointer receiver を採用する。
func (s *State) reconcileSummaryEmpty() {
	if len(s.Summary.WorkflowBuckets) == 0 && len(s.Summary.ProviderBuckets) == 0 {
		s.Summary.EmptyMessage = emptySummaryMessage
		return
	}
	s.Summary.EmptyMessage = ""
}

// enterList は SubviewList への切替 (戻り) 時に呼ばれる純関数 (Task 8.2,
// Req 2.4 / 2.6 / 10.1)。
//
// 責務:
//   - Subview を SubviewList に切り替える
//   - List 状態 (Items / Cursor / Filter / Sort / Offset / NextOffset 等) は
//     変更しない: Detail / Summary から戻ったときに、直前の List 表示状態
//     (フィルタ・ソート・ページ位置・選択カーソル) をそのまま復元するため
//
// 注意: List への戻りは Cmd を発行しない。新規 fetch を行いたい場合は呼び出し側で
// 別途 ListRunsCmd を発行する責務を負う (本ヘルパは「Subview を List に切り替える」
// 1 アクションに限定する)。
func (s State) enterList() (State, tea.Cmd) {
	s.Subview = SubviewList
	return s, nil
}

// enterDetail は SubviewDetail への切替時に呼ばれる純関数 (Task 6.1 / 6.2,
// Req 5.1 / 6.1 / 6.2 / 11.2 / 11.3)。
//
// 責務:
//   - Subview を SubviewDetail に切り替える
//   - Detail を「指定 RunID + Focus=Header」だけのゼロ値で初期化し、前回 Detail 表示の
//     stale データ (Detail 本体 / NextRawEventsOffset / カーソル / Err / HandoffErr /
//     Artifact プレビュー全フィールド) を全リセットする (再突入時の表示混入排除)
//   - Reader != nil なら Detail.Loading=true を立て、`RunDetailCmd` を
//     `DetailOptions{WorkflowRunID, RawEvents:{Limit: DefaultRawEventsLimit, Offset:0}}` で発行する
//   - ArtifactReader != nil なら Artifact.Loading=true を立て、`GetArtifactsCmd` を
//     発行する。本体は読み込まない (Req 6.2 lazy 読み出し)
//   - Reader == nil なら Cmd を発行せず Detail.Loading も立てない (View 側で
//     「履歴データが利用できません」を即出す graceful 経路: Req 11.2)
//   - ArtifactReader == nil なら Cmd を発行せず、Artifact.Unavailable=true を立て
//     Artifact.Loading も立てない (View 側で「成果物データが利用できません」を
//     即出す graceful 経路: Req 11.3)
//
// 同時発行は tea.Batch で組み合わせる。両 Reader が nil のときは nil Cmd を返す。
// 1 つだけ非 nil のときは tea.Batch を経由せず直接 Cmd を返し、
// テスト側 (`runBatchCmd`) でも単発 / Batch の両形態を扱えるようにする。
//
// Subview 切替経路 (List → Detail) は Task 8.2 で `State.HandleKey` に実装される。
// 本ヘルパは「Subview=SubviewDetail かつ RunID 確定」を 1 トランザクションで行う関数として
// 公開し、呼び出し側が個別フィールドを書き換える経路を作らない (不変条件
// 「RunID は Subview=SubviewDetail のときのみ非空」を構造的に保証する)。
func (s State) enterDetail(runID string) (State, tea.Cmd) {
	s.Subview = SubviewDetail
	// stale データ排除: 値型 Detail をゼロ値で上書き (Focus は AgentCalls に初期化)。
	// Header は workflow_run の見出し 1 行のみで navigation 対象がないため、
	// 初期 Focus を AgentCalls にすることで Detail に入った直後から j/k/Enter が
	// 直ちに反応するようにする (旧実装の「Tab を 1 回押すまでキーが死ぬ」UX 改善)。
	// Artifact もゼロ値になり、SelectedIndex / PreviewBytes / PreviewWarning / Rows /
	// EmptyMessage / Err / Loading / Unavailable がすべてリセットされる。
	s.Detail = DetailState{
		RunID: runID,
		Focus: DetailFocusAgentCalls,
	}

	// ArtifactReader 未配線時の graceful フラグを Detail 入口で確定する (Req 11.3)。
	// View / HandleKey / テストの三者がこのフラグを参照することで「Cmd 発行抑止」と
	// 「unavailable 文言表示」の判定基準を一元化する。
	if s.ArtifactReader == nil {
		s.Detail.Artifact.Unavailable = true
	}

	var cmds []tea.Cmd
	if s.Reader != nil {
		s.Detail.Loading = true
		cmds = append(cmds, RunDetailCmd(s.Reader, record.DetailOptions{
			WorkflowRunID: runID,
			RawEvents: record.RawEventsQuery{
				Limit:  record.DefaultRawEventsLimit,
				Offset: 0,
			},
		}))
	}
	if s.ArtifactReader != nil {
		// Req 6.1 / 6.2: Detail 突入時はメタ列挙のみ (本体は読み込まない)。
		s.Detail.Artifact.Loading = true
		cmds = append(cmds, GetArtifactsCmd(s.ArtifactReader, runID))
	}

	switch len(cmds) {
	case 0:
		return s, nil
	case 1:
		return s, cmds[0]
	default:
		return s, tea.Batch(cmds...)
	}
}

// applyRunDetailMsg は RunDetailMsg を DetailState に反映する純関数
// (Req 5.1 / 5.2 / 5.3 / 5.4 / 5.5 / 11.2 / 11.5)。
//
// 排他的状態セット (Loading=false 確定 + 以下のいずれか 1 つ):
//   - 成功: Detail / RawEventsOffset / NextRawEventsOffset を反映、Err をクリア。
//     カーソルは新ページ範囲にクランプ (items 件数縮小時の out-of-range を防ぐ)
//   - errReaderUnavailable: graceful (Detail / Err を変更しない)
//     View が s.Reader == nil を検知して「履歴データが利用できません」を出す
//   - ErrWorkflowRunNotFound: Err に格納し、View 側で
//     「該当 run が存在しない」(runDetailNotFoundMessage) を表示する (Req 5.4)
//   - その他のエラー: Err に格納 (Req 5.5)
//
// agent_calls / raw_events の並び順は Reader 戻り値順 (started_at 昇順 /
// sequence 昇順) を信頼してそのまま保持する (Req 5.2)。クライアントサイドで
// ソートし直さない。
//
// tea.Quit は絶対に発行しない (Req 5.5 / 11.5)。
func (s State) applyRunDetailMsg(msg RunDetailMsg) State {
	s.Detail.Loading = false
	if msg.Err != nil {
		if errors.Is(msg.Err, errReaderUnavailable) {
			return s
		}
		// ErrWorkflowRunNotFound もそのまま Err に格納する。View / テスト側は
		// errors.Is(err, record.ErrWorkflowRunNotFound) で判別する (Req 5.4)。
		s.Detail.Err = msg.Err
		return s
	}
	s.Detail.Err = nil
	// RunDetailMsg.Detail は値型 RunDetail。ローカル変数に再代入してから
	// アドレスを取ることで、各メッセージごとに独立したインスタンスを
	// DetailState.Detail に格納する (前回の Detail を上書きしない設計)。
	detail := msg.Detail
	s.Detail.Detail = &detail
	s.Detail.RawEventsOffset = detail.RawEventsOffset
	s.Detail.NextRawEventsOffset = detail.NextRawEventsOffset
	s.Detail.AgentCallsCursor = clampCursorToLen(s.Detail.AgentCallsCursor, len(detail.AgentCalls))
	s.Detail.RawEventsCursor = clampCursorToLen(s.Detail.RawEventsCursor, len(detail.RawEvents))
	return s
}

// clampCursorToLen はカーソル値を items 件数に応じて [0, len-1] にクランプする。
// items が空 (len==0) のときは 0 を返す。
//
// 動機: applyRunDetailMsg が新しい Detail を反映した直後、前回 page で
// 末尾近くにあったカーソルが新 page で out-of-range になりうる。本ヘルパで
// 構造的に範囲内へ閉じ込め、View 側のインデックス参照 panic を防ぐ。
func clampCursorToLen(cursor, length int) int {
	if length <= 0 {
		return 0
	}
	return clamp(cursor, 0, length-1)
}

// detailFocusOrder は Detail サブビューのフォーカス循環順序。
//
// 順序: AgentCalls → RawEvents → Artifacts → AgentCalls → ...
// Tab で前進、Shift+Tab で後退する。順序を 1 箇所の slice に集約することで
// next/prev の双方が同じインデックス計算を使い、追加 / 削除時の整合性を
// 構造的に保証する (環状の next/prev は ((i+delta)+n) mod n で表現)。
//
// DetailFocusHeader は Focus 値としては残るが本循環には含めない。Header は
// workflow_run の見出し 1 行のみで navigation 対象がないため、Tab 巡回に含めると
// 「何も動かない」死に Focus が混じり UX を損なう。Header 値が紛れ込んだ場合は
// shiftDetailFocus 内のフォールバックで AgentCalls (= order[0]) に復帰する。
var detailFocusOrder = []DetailFocus{
	DetailFocusAgentCalls,
	DetailFocusRawEvents,
	DetailFocusArtifacts,
}

// nextDetailFocus は detailFocusOrder の循環順序で次の Focus を返す。
// 未知の値が来た場合は Header にフォールバックする (壊れた state からの回復経路)。
func nextDetailFocus(f DetailFocus) DetailFocus {
	return shiftDetailFocus(f, 1)
}

// prevDetailFocus は detailFocusOrder の循環順序で前の Focus を返す。
func prevDetailFocus(f DetailFocus) DetailFocus {
	return shiftDetailFocus(f, -1)
}

// shiftDetailFocus は detailFocusOrder 上で f を delta 分ずらした Focus を返す。
// f が detailFocusOrder にない場合は Header (= order[0]) を返す。
func shiftDetailFocus(f DetailFocus, delta int) DetailFocus {
	n := len(detailFocusOrder)
	for i, x := range detailFocusOrder {
		if x == f {
			// Go の %  は負数で負値を返すため、+n してから mod する
			return detailFocusOrder[((i+delta)%n+n)%n]
		}
	}
	return detailFocusOrder[0]
}

// handleDetailKey は SubviewDetail のキー処理 (Task 6.1 / 6.2,
// Req 5.1〜5.5 / 6.3 / 11.3)。
//
// 責務:
//   - Tab / Shift+Tab で Focus を循環切替 (Header / AgentCalls / RawEvents / Artifacts)
//   - j / k / ↑ / ↓ で現在 Focus が指す list のカーソル (AgentCallsCursor /
//     RawEventsCursor / Artifact.SelectedIndex) を上下移動。Focus=Header では
//     本キーは no-op (Header は単一行)
//   - n / p で raw_events ページを進退 (Focus を問わず受け付ける単純化、
//     Req 5.3)。Reader=nil または NextRawEventsOffset 不在 (next の場合) や
//     RawEventsOffset==0 (prev の場合) では no-op
//   - Enter で Focus=Artifacts のとき選択中成果物の本体読み出し
//     (`ReadArtifactCmd`) を発行する (Req 6.3 lazy load)。他 Focus / Rows 空 /
//     ArtifactReader=nil では no-op (Req 11.3 graceful)
//   - その他のキーは no-op (サブビュー間遷移キー / モード退出キーは Task 8.2 で実装)
//
// 不変条件:
//   - 戻り値の State は受領 state からの差分のみで、List / Summary は触らない
//   - tea.Quit を返さない
//   - 編集 / 削除 / アーカイブ操作キーを受け付けない (Req 6.8。本ハンドラは
//     既知キーのみを消費し、未知キーは no-op で抜けるため誤動作が発生しない)
func (s State) handleDetailKey(msg tea.KeyMsg) (State, tea.Cmd) {
	switch msg.String() {
	case "tab":
		s.Detail.Focus = nextDetailFocus(s.Detail.Focus)
		// Focus 切替で pane は閉じる (別セクションに移ったらクリア)。
		s.Detail.AgentCallPaneOpen = false
		s.Detail.RawEventPaneOpen = false
		return s, nil
	case "shift+tab":
		s.Detail.Focus = prevDetailFocus(s.Detail.Focus)
		s.Detail.AgentCallPaneOpen = false
		s.Detail.RawEventPaneOpen = false
		return s, nil
	case "down", "j":
		return s.moveDetailCursor(1), nil
	case "up", "k":
		return s.moveDetailCursor(-1), nil
	case "n":
		return s.nextRawEventsPage()
	case "p":
		return s.prevRawEventsPage()
	case "enter":
		// Focus 別に分岐:
		//   - AgentCalls: 選択行詳細 pane をトグル
		//   - RawEvents:  選択行 Payload pane をトグル
		//   - Artifacts:  既存の成果物プレビュー (ReadArtifactCmd)
		//   - その他:     no-op
		switch s.Detail.Focus {
		case DetailFocusAgentCalls:
			s.Detail.AgentCallPaneOpen = !s.Detail.AgentCallPaneOpen
			return s, nil
		case DetailFocusRawEvents:
			s.Detail.RawEventPaneOpen = !s.Detail.RawEventPaneOpen
			return s, nil
		case DetailFocusArtifacts:
			return s.requestArtifactPreview()
		}
		return s, nil
	case "i":
		return s.startHandoff()
	case "backspace":
		// Task 8.2 / Req 2.6: Detail → List 戻り遷移。
		// Esc / q はモード退出として Model 側で処理されるため、ここでは backspace のみを
		// Detail 側の back キーとして消費する。enterList は値型再代入なし (Subview のみ
		// 変更) なので、List の Items / Cursor / Filter / Sort などは復元される。
		return s.enterList()
	}
	return s, nil
}

// handleListKey は SubviewList のキー処理 (Task 8.2, Req 2.3 / 2.4 / 2.5 / 10.1)。
//
// 責務:
//   - tab: List → Summary 遷移 (enterSummary 経由で aggregate Cmd を発行)
//   - enter: List → Detail 遷移 (Cursor が指す run を Detail に渡す)。
//     Items が空のとき / Cursor が範囲外のときは silent no-op
//     (= Reader.Detail を空 RunID で呼ぶ異常パスを構造的に閉じる)
//   - down / j: List.Cursor を +1 (末尾でクランプ)
//   - up / k:   List.Cursor を -1 (先頭でクランプ)
//     Items が空のときはカーソル移動キーは silent no-op
//   - その他のキー: no-op (フィルタ・ソート・ページングキー写像は別タスク扱い)
//
// 不変条件: tea.Quit を返さない (Req 11.5)。Esc / q は本ハンドラで消費せず、
// Model 側でモード退出として処理される (Req 2.7)。
func (s State) handleListKey(msg tea.KeyMsg) (State, tea.Cmd) {
	switch msg.String() {
	case "tab":
		return s.enterSummary()
	case "enter":
		// Items 空 / Cursor 範囲外なら silent no-op
		if len(s.List.Items) == 0 {
			return s, nil
		}
		idx := s.List.Cursor
		if idx < 0 || idx >= len(s.List.Items) {
			return s, nil
		}
		runID := s.List.Items[idx].RunID
		if runID == "" {
			// 防御: 不正な空 RunID で Detail 経路に入らない
			return s, nil
		}
		return s.enterDetail(runID)
	case "down", "j":
		return s.moveListCursor(1), nil
	case "up", "k":
		return s.moveListCursor(-1), nil
	}
	return s, nil
}

// moveListCursor は List.Cursor を delta 分動かして範囲内にクランプする。
//
// Items が空 (len==0) のときは何もせず元の state を返す (Cursor=0 のまま)。
// 既存 clampCursorToLen で [0, len-1] に閉じ込めるため、末尾 +1 や先頭 -1 は
// 範囲外に出ない。Detail サブビューの moveDetailCursor と挙動を揃えることで
// ユーザの認知負荷を減らす。
func (s State) moveListCursor(delta int) State {
	if len(s.List.Items) == 0 {
		return s
	}
	s.List.Cursor = clampCursorToLen(s.List.Cursor+delta, len(s.List.Items))
	return s
}

// handleSummaryKey は SubviewSummary のキー処理 (Task 8.2, Req 2.4)。
//
// 責務:
//   - tab: Summary → List 戻り遷移 (enterList で Subview のみ切替)
//   - その他のキー: no-op
//
// 不変条件: tea.Quit を返さない (Req 11.5)。Esc / q は本ハンドラで消費せず、
// Model 側でモード退出として処理される (Req 2.7)。
func (s State) handleSummaryKey(msg tea.KeyMsg) (State, tea.Cmd) {
	switch msg.String() {
	case "tab":
		return s.enterList()
	}
	return s, nil
}

// startHandoff は Run 詳細サブビュー上の hand-off キー (`i`) で発動する
// 改善ワークフロー (improve-workflow) hand-off の起動経路 (Task 7.1)。
//
// 振る舞い (Req 7.1〜7.10 / 10.4 / 11.5):
//
//   - `s.Detail.Detail == nil` (Run 詳細未取得) のとき: silent no-op。
//     run の workflow_id / run_id を取得できないため Cmd を発行せず、
//     LastHandoff / HandoffErr も変更しない (UX 上「読み込み中の操作」)
//
//   - `s.Workflows` を線形検索し `Definition.ID == handoffImproveWorkflowID`
//     が見つからないとき (Req 7.7):
//     `Detail.HandoffErr = errHandoffWorkflowNotRegistered` を立てて Detail
//     サブビューに留まる。Cmd は発行しない。LastHandoff も変更しない
//
//   - 見つかったとき (Req 7.2〜7.6 / 10.4):
//     `params := workflow.HandoffParams{TargetWorkflowID: detail.Run.WorkflowID,
//     RunIDs: []string{detail.Run.RunID}}` を構築し
//     `LastHandoff = &params` を観測点として保持。
//     `LaunchHandoffCmd(s.ForwardEvent, s.Ask, def, params, s.CWD)` を Cmd として返す。
//     起動成功 / 失敗の最終確定は Cmd 戻り msg
//     (`HandoffStartedMsg` / `HandoffFailedMsg`) の Update 側受信で行う
//
// MVP 不変式 (Req 7.6): `RunIDs` の長さは必ず 1 (詳細表示中の単一 run)。
// 成功例 / 失敗例選定や追加 run 自動収集は本 spec 側で行わない。
//
// tea.Quit は絶対に発行しない (Req 7.7 / 11.5)。
func (s State) startHandoff() (State, tea.Cmd) {
	// Detail 未取得ガード: run 情報がないので silent no-op (Cmd / state 変更なし)
	if s.Detail.Detail == nil {
		return s, nil
	}

	// `s.Workflows` を線形検索 (Req 7.1 / 7.7)。
	// 走査範囲を `s.Workflows` 全体に限定し、外部 registry を引かない
	// (本 spec の責務外: improve-workflow 本体の実装は Req 7.9)。
	var def *workflow.Definition
	for i := range s.Workflows {
		if s.Workflows[i].ID == handoffImproveWorkflowID {
			def = &s.Workflows[i]
			break
		}
	}
	if def == nil {
		// Req 7.7: 未登録時はモードを抜けず HandoffErr に通知文を格納。
		// LastHandoff は変更しない (起動経路に乗っていないため)。
		s.Detail.HandoffErr = errHandoffWorkflowNotRegistered
		return s, nil
	}

	// Req 7.4 / 7.5 / 7.6: HandoffParams 構築。RunIDs は MVP では必ず長さ 1。
	run := s.Detail.Detail.Run
	params := workflow.HandoffParams{
		TargetWorkflowID: run.WorkflowID,
		RunIDs:           []string{run.RunID},
	}
	// Req 10.4: LastHandoff にコピーを保持し、テスト / 観測の単一参照を確保する
	// (パラメータの値型コピーを取ることで、後続の状態変更で上書きされない)。
	captured := params
	s.LastHandoff = &captured
	// 直前の hand-off 失敗による HandoffErr 残留をクリア (再試行時の表示整合)。
	s.Detail.HandoffErr = nil
	return s, LaunchHandoffCmd(s.ForwardEvent, s.Ask, *def, params, s.CWD)
}

// moveDetailCursor は Focus に応じて該当カーソルを delta 分動かす
// (Task 6.1 / 6.2)。
//
// Focus=Header / 未知の Focus / Detail が未取得かつ AgentCalls / RawEvents 系の
// 場合は no-op。範囲外への移動は clampCursorToLen で 0..len-1 に閉じ込める。
//
// Focus=Artifacts のときは ArtifactState.SelectedIndex を移動する。
// Detail (RunDetail) が未取得 (= s.Detail.Detail == nil) でも Artifact.Rows は
// 独立に取得されうるため、Detail nil ガードは AgentCalls / RawEvents 経路に限定し、
// Artifacts 経路は Rows の長さのみで判定する。Rows が空のときは clampCursorToLen が
// 0 を返すため、空状態での連打も安全に no-op となる。
func (s State) moveDetailCursor(delta int) State {
	switch s.Detail.Focus {
	case DetailFocusAgentCalls:
		if s.Detail.Detail == nil {
			return s
		}
		s.Detail.AgentCallsCursor = clampCursorToLen(s.Detail.AgentCallsCursor+delta, len(s.Detail.Detail.AgentCalls))
	case DetailFocusRawEvents:
		if s.Detail.Detail == nil {
			return s
		}
		s.Detail.RawEventsCursor = clampCursorToLen(s.Detail.RawEventsCursor+delta, len(s.Detail.Detail.RawEvents))
	case DetailFocusArtifacts:
		s.Detail.Artifact.SelectedIndex = clampCursorToLen(s.Detail.Artifact.SelectedIndex+delta, len(s.Detail.Artifact.Rows))
	}
	// Cursor 移動で詳細 pane は自動的に閉じる (別行で開いたまま残らない)。
	// Focus に依らず両 pane を false にすることで、Tab → 別 Focus → カーソル
	// 移動という経路でも pane が残留しない不変条件を構造的に保証する。
	s.Detail.AgentCallPaneOpen = false
	s.Detail.RawEventPaneOpen = false
	return s
}

// requestArtifactPreview は Focus=Artifacts のとき Enter キーで現在選択中の
// 成果物の本体読み出しを要求する Cmd を返す (Task 6.2, Req 6.3 / 6.6 / 11.3)。
//
// 発行条件 (すべて満たすときのみ Cmd を発行):
//   - Focus == DetailFocusArtifacts (他 Focus からの誤暴発を防ぐ)
//   - ArtifactReader != nil (Req 11.3 graceful no-op)
//   - len(Rows) > 0 (空一覧では選択対象なし)
//   - 0 <= SelectedIndex < len(Rows) (範囲外指定の自衛)
//
// 副作用:
//   - Artifact.Loading=true を立てる
//   - 既存 PreviewBytes / PreviewWarning / Err はクリアしない (msg 受信時に
//     applyReadArtifactMsg が排他的に上書きする)
//
// 戻り値の Cmd は ReadArtifactCmd で、本体は record.ContextWithRunCWD に s.CWD を
// 載せた ctx で `ArtifactReader.ReadArtifact` を呼ぶ (Req 6.3 / 8.3)。
// `ReadArtifactUnlimited` には絶対にフォールバックしない (Req 6.6 / 8.4。
// 不変式は io.go の AST 検査でも担保されている)。
func (s State) requestArtifactPreview() (State, tea.Cmd) {
	if s.Detail.Focus != DetailFocusArtifacts {
		return s, nil
	}
	if s.ArtifactReader == nil {
		return s, nil
	}
	if len(s.Detail.Artifact.Rows) == 0 {
		return s, nil
	}
	idx := s.Detail.Artifact.SelectedIndex
	if idx < 0 || idx >= len(s.Detail.Artifact.Rows) {
		return s, nil
	}
	path := s.Detail.Artifact.Rows[idx].Path
	s.Detail.Artifact.Loading = true
	return s, ReadArtifactCmd(s.ArtifactReader, s.CWD, path)
}

// applyGetArtifactsMsg は GetArtifactsMsg を ArtifactState に反映する
// 純関数 (Task 6.2, Req 6.1 / 6.9 / 11.3 / 11.5)。
//
// 排他的状態セット (Loading=false 確定 + 以下のいずれか 1 つ):
//   - 成功: Rows を反映、SelectedIndex=0 で初期化、Err をクリア。
//     len(Rows)==0 のとき EmptyMessage="成果物なし" を立てる (Req 6.9)
//   - errArtifactReaderUnavailable: graceful (Rows / Err / EmptyMessage を変更しない)。
//     View 側で Unavailable=true を見て「成果物データが利用できません」を出す (Req 11.3)
//   - その他のエラー: Err に格納し、Rows を nil で上書き、EmptyMessage はクリア
//
// SelectedIndex を 0 に必ずリセットする理由: 新ページ取得 (Detail 切替や再取得)
// で行数が変わったときに、前回末尾近くの SelectedIndex が新 Rows で out-of-range
// になりうる。0 リセットによって View / HandleKey での範囲外参照を構造的に防ぐ。
//
// tea.Quit は絶対に発行しない (Req 11.5)。
func (s State) applyGetArtifactsMsg(msg GetArtifactsMsg) State {
	s.Detail.Artifact.Loading = false
	if msg.Err != nil {
		if errors.Is(msg.Err, errArtifactReaderUnavailable) {
			// Req 11.3 graceful: Err を貯めず Unavailable フラグ表示に委ねる。
			return s
		}
		s.Detail.Artifact.Err = msg.Err
		s.Detail.Artifact.Rows = nil
		s.Detail.Artifact.EmptyMessage = ""
		s.Detail.Artifact.SelectedIndex = 0
		return s
	}
	s.Detail.Artifact.Err = nil
	s.Detail.Artifact.Rows = msg.Rows
	s.Detail.Artifact.SelectedIndex = 0
	if len(msg.Rows) == 0 {
		s.Detail.Artifact.EmptyMessage = emptyArtifactsMessage
		return s
	}
	s.Detail.Artifact.EmptyMessage = ""
	return s
}

// applyReadArtifactMsg は ReadArtifactMsg を ArtifactState に反映する
// 純関数 (Task 6.2, Req 6.3 / 6.5 / 6.6 / 6.7 / 11.3 / 11.5)。
//
// 排他的状態セット (Loading=false 確定 + 以下のいずれか 1 つ):
//   - 成功: PreviewBytes=Data / PreviewPath=Path / PreviewWarning="" / Err=nil
//   - ErrArtifactTooLarge: PreviewWarning="上限超過のため本体を読み込みませんでした"
//     PreviewBytes=nil / PreviewPath は Path で更新し Err=nil (Req 6.5)。
//     Rows / SelectedIndex (= メタ表示) は変更しない (Req 6.5 後段「メタは継続表示」)
//   - ErrArtifactFileMissing or fs.ErrNotExist: PreviewWarning="成果物が見つかりません"
//     PreviewBytes=nil / Err=nil (Req 6.7)
//   - errArtifactReaderUnavailable: graceful (Preview 系を変更しない)。
//     View 側で Unavailable=true を見て unavailable 表示する
//   - その他のエラー: Err に格納、PreviewWarning="" / PreviewBytes=nil
//
// `ReadArtifactUnlimited` への自動フォールバックは行わない (Req 6.6 / 8.4)。
// ErrArtifactTooLarge は警告メッセージのみ立てて静的に終端する。
//
// tea.Quit は絶対に発行しない (Req 11.5)。
func (s State) applyReadArtifactMsg(msg ReadArtifactMsg) State {
	s.Detail.Artifact.Loading = false
	if msg.Err != nil {
		if errors.Is(msg.Err, errArtifactReaderUnavailable) {
			// Req 11.3 graceful: Err を貯めず Unavailable フラグ表示に委ねる。
			return s
		}
		if errors.Is(msg.Err, record.ErrArtifactTooLarge) {
			// Req 6.5: 警告のみ立て、本体未取得 / メタ継続表示。
			s.Detail.Artifact.Err = nil
			s.Detail.Artifact.PreviewWarning = previewWarningTooLarge
			s.Detail.Artifact.PreviewBytes = nil
			s.Detail.Artifact.PreviewPath = msg.Path
			return s
		}
		if errors.Is(msg.Err, record.ErrArtifactFileMissing) || errors.Is(msg.Err, fs.ErrNotExist) {
			// Req 6.7: 削除済み / 不在も graceful 警告のみ。
			s.Detail.Artifact.Err = nil
			s.Detail.Artifact.PreviewWarning = previewWarningMissing
			s.Detail.Artifact.PreviewBytes = nil
			s.Detail.Artifact.PreviewPath = msg.Path
			return s
		}
		// その他: Err に格納し、警告 / 本体はクリア
		s.Detail.Artifact.Err = msg.Err
		s.Detail.Artifact.PreviewWarning = ""
		s.Detail.Artifact.PreviewBytes = nil
		return s
	}
	// 成功: 排他的に Preview を上書き (前回 warning / err は確定的にクリア)
	s.Detail.Artifact.Err = nil
	s.Detail.Artifact.PreviewWarning = ""
	s.Detail.Artifact.PreviewBytes = msg.Data
	s.Detail.Artifact.PreviewPath = msg.Path
	return s
}

// nextRawEventsPage は raw_events 次ページを取得する Cmd を返す (Task 6.1, Req 5.3)。
//
// 発行条件:
//   - Reader != nil (Reader=nil なら graceful no-op)
//   - NextRawEventsOffset != nil (次ページなしなら no-op)
//
// 副作用:
//   - Loading=true を立てる
//   - RawEventsCursor を 0 にリセット (新ページの先頭から閲覧開始)
//
// 戻り値の Cmd は `RunDetailCmd` で、戻り msg (`RunDetailMsg`) は applyRunDetailMsg
// で受信されページ単位で Detail.RawEvents を差し替える (累積しない、Req 5.3 後段)。
func (s State) nextRawEventsPage() (State, tea.Cmd) {
	if s.Reader == nil || s.Detail.NextRawEventsOffset == nil {
		return s, nil
	}
	nextOffset := *s.Detail.NextRawEventsOffset
	s.Detail.Loading = true
	s.Detail.RawEventsCursor = 0
	return s, RunDetailCmd(s.Reader, record.DetailOptions{
		WorkflowRunID: s.Detail.RunID,
		RawEvents: record.RawEventsQuery{
			Limit:  record.DefaultRawEventsLimit,
			Offset: nextOffset,
		},
	})
}

// prevRawEventsPage は raw_events 前ページを取得する Cmd を返す (Task 6.1, Req 5.3)。
//
// 発行条件:
//   - Reader != nil (Reader=nil なら graceful no-op)
//   - 現在 RawEventsOffset > 0 (= 前ページが存在する)
//
// 副作用:
//   - Loading=true を立てる
//   - RawEventsCursor を 0 にリセット
//
// 前ページ Offset は (現在 Offset - DefaultRawEventsLimit) で算出する。負値に
// なる場合は 0 にクランプして先頭ページを再取得する (= prev 連打時の境界挙動)。
func (s State) prevRawEventsPage() (State, tea.Cmd) {
	if s.Reader == nil {
		return s, nil
	}
	if s.Detail.RawEventsOffset <= 0 {
		// 既に先頭ページ。前ページなしなので no-op (新規 Cmd は発行しない)
		return s, nil
	}
	prev := s.Detail.RawEventsOffset - record.DefaultRawEventsLimit
	if prev < 0 {
		prev = 0
	}
	s.Detail.Loading = true
	s.Detail.RawEventsCursor = 0
	return s, RunDetailCmd(s.Reader, record.DetailOptions{
		WorkflowRunID: s.Detail.RunID,
		RawEvents: record.RawEventsQuery{
			Limit:  record.DefaultRawEventsLimit,
			Offset: prev,
		},
	})
}

// applyListRunsMsg は ListRunsMsg を ListState に反映する純関数
// (Req 3.1 / 3.9 / 3.10 / 11.2 / 11.5)。
//
// 排他的状態セット (Loading=false 確定 + 以下のいずれか 1 つ):
//   - 成功: Items / NextOffset を反映、Err をクリア
//   - len(Items)==0 のとき EmptyMessage を立てる (Req 3.9)
//   - errReaderUnavailable: graceful (Items / Err / EmptyMessage を変更しない)
//     View が s.Reader == nil を検知して「履歴データが利用できません」を出す
//   - その他のエラー: Err に格納 (Req 3.10)
//
// tea.Quit は絶対に発行しない (Req 3.10 / 11.5)。
func (s State) applyListRunsMsg(msg ListRunsMsg) State {
	s.List.Loading = false
	if msg.Err != nil {
		if errors.Is(msg.Err, errReaderUnavailable) {
			return s
		}
		s.List.Err = msg.Err
		return s
	}
	s.List.Err = nil
	s.List.Items = msg.Result.Items
	s.List.NextOffset = msg.Result.NextOffset
	if len(s.List.Items) == 0 {
		s.List.EmptyMessage = emptyRunListMessage
		s.List.Cursor = 0
		return s
	}
	s.List.EmptyMessage = ""
	s.List.Cursor = clamp(s.List.Cursor, 0, len(s.List.Items)-1)
	return s
}
