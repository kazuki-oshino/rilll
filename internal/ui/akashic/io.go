// Package akashic はアカシックモード TUI (Run 一覧 / 集計サマリ / Run 詳細) の
// 状態・I/O・描画ロジックを提供するサブパッケージ (akashic-mode-tui spec)。
//
// internal/ui ルート (TUI シェル本体) からは公開 API
// (`State`, `NewState`, `Update`, `View`, `HandleKey`, message 型) のみで
// 疎結合に扱える。
//
// 本ファイルは Reader / ArtifactReader / Definition.Run の呼び出しを
// goroutine + tea.Cmd / tea.Msg で表現し、Bubble Tea Update を
// 非ブロッキングに保つための薄いヘルパを提供する (Req 9.2 / 10.2)。
//
// 設計上の注意:
//   - Cmd 関数は State.Update から発行され、戻り msg は同じ Update で消費される。
//     I/O は goroutine 内で実行し、戻り値を直接 tea.Msg として返す純関数構造。
//   - Reader == nil / ArtifactReader == nil の場合、Cmd は呼び出し側に
//     エラー msg を即座に返し (errReaderUnavailable / errArtifactReaderUnavailable)、
//     対応するメソッドを呼ばない。これは Req 11.2 / 11.3 の graceful 表示を
//     支える防衛的ガードであり、本来は呼び出し側 (State) が nil のとき
//     Cmd を発行しない設計になっている。
//   - ListRunsCmd は ListOptions.Limit を必ず record.DefaultListLimit (=50)
//     で固定する (Req 9.5: ページサイズ 50 件固定)。呼び出し側は Offset と
//     フィルタのみを管理する。
//   - ReadArtifactCmd は呼び出し前に必ず record.ContextWithRunCWD で cwd を
//     ctx に注入してから ReadArtifact を呼ぶ (Req 6.3 / 8.3)。
//   - 書き込み (Recorder) や `.rilll/` 直読は一切行わない (Req 8.1〜8.5)。
//   - ArtifactReader.ReadArtifactUnlimited は本ファイルから一切参照しない
//     (Req 6.6 / 8.4 の不変式)。回帰テスト
//     TestAkashicIODoesNotReferenceReadArtifactUnlimited で文字列レベルでも
//     検査される。
//   - LaunchHandoffCmd は workflow.ContextWithHandoff で HandoffParams を
//     ctx に乗せた上で Definition.Run を呼び、その戻り err の有無で
//     HandoffStartedMsg / HandoffFailedMsg を Cmd 戻り値として返す (Task 3.3,
//     Req 7.2 / 7.7 / 7.8)。WorkflowEvent は呼び出し側から渡された
//     EventForwarder に転送し、TUI シェル側で workflowEventMsg に包んで
//     events チャネルへ送る経路と接続する。
package akashic

import (
	"context"
	"errors"

	tea "github.com/charmbracelet/bubbletea"

	record "github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflow"
)

// errReaderUnavailable は Reader が未注入 (nil) のときに Cmd ヘルパが返す sentinel error。
// 呼び出し側は errors.Is(err, errReaderUnavailable) で判別し、graceful 表示
// (「履歴データが利用できません」) に切り替える (Req 11.2)。
//
// 本 sentinel は非公開: パッケージ外から判別する経路を持たないことで、
// データ源未配線の取り扱いを akashic パッケージ内に閉じ込める設計。
var errReaderUnavailable = errors.New("ui/akashic: reader is unavailable")

// errArtifactReaderUnavailable は ArtifactReader が未注入 (nil) のときに
// Cmd ヘルパが返す sentinel error。呼び出し側は errors.Is で判別し、
// graceful 表示 (「成果物データが利用できません」) に切り替える (Req 11.3)。
//
// Reader 用の errReaderUnavailable と分離する理由:
//   - 表示文言が異なる (履歴データ vs 成果物データ)
//   - Reader と ArtifactReader は独立に nil/non-nil となり得る
//     (例: Recorder が両 interface を兼用しないテスト構成)
var errArtifactReaderUnavailable = errors.New("ui/akashic: artifact reader is unavailable")

// EventForwarder は LaunchHandoffCmd が def.Run の onEvent から受け取った
// WorkflowEvent を TUI シェル側 Update で消費可能な tea.Msg 経路へ流すための
// コールバック。ui パッケージ側で
// `func(ev) { events <- workflowEventMsg{event: ev} }` というクロージャを
// 渡して接続する。
//
// akashic パッケージは TUI シェル側の workflowEventMsg 型に依存しないことで、
// 関心の分離 (akashic mode の I/O / 状態) と (TUI shell の Bubble Tea msg 型)
// を保つ。
type EventForwarder func(workflow.WorkflowEvent)

// ListRunsMsg は ListRunsCmd の戻り tea.Msg。
// 成功時は Result に Reader.List の戻り値、失敗時は Err に error が入る (排他)。
type ListRunsMsg struct {
	Result record.ListResult
	Err    error
}

// AggregateByWorkflowMsg は AggregateByWorkflowCmd の戻り tea.Msg。
type AggregateByWorkflowMsg struct {
	Result record.WorkflowAggregateResult
	Err    error
}

// AggregateByProviderMsg は AggregateByProviderCmd の戻り tea.Msg。
type AggregateByProviderMsg struct {
	Result record.ProviderAggregateResult
	Err    error
}

// RunDetailMsg は RunDetailCmd の戻り tea.Msg。
// Detail は Reader.Detail の戻り値 (値型)。
type RunDetailMsg struct {
	Detail record.RunDetail
	Err    error
}

// GetArtifactsMsg は GetArtifactsCmd の戻り tea.Msg (Req 6.1)。
// Rows は ArtifactReader.GetArtifacts の戻り値、Err は呼び出しエラー。
// ArtifactReader 未注入時は Err=errArtifactReaderUnavailable / Rows=nil。
type GetArtifactsMsg struct {
	Rows []record.ArtifactRow
	Err  error
}

// ReadArtifactMsg は ReadArtifactCmd の戻り tea.Msg (Req 6.3)。
// Path は呼び出し時の入力 path をそのまま保持し、Err 時もそのまま返す
// (呼び出し側が「どの成果物の読み出しが失敗したか」を Path で照合できるようにする
// = Req 6.5 / 6.7 の上限超過 / 削除済み warning でメタを継続表示する経路と整合)。
// Data は成功時のみ非 nil。
type ReadArtifactMsg struct {
	Path string
	Data []byte
	Err  error
}

// HandoffStartedMsg は LaunchHandoffCmd が def.Run を err=nil で完了したときに
// Cmd 戻り値として発行する tea.Msg (Task 3.3, Req 7.2 / 7.8)。
//
// 呼び出し側 (Task 7.1) は本 msg 受信で Model.Phase = PhaseWorkflow に遷移し、
// 以降は既存ワークフロー実行画面の挙動 (events チャネル経由の WorkflowEvent
// 流入) に委ねる。
//
// Params は LaunchHandoffCmd 呼び出し時に渡された HandoffParams をそのまま
// 保持する (テスト観測点: Req 10.4 の State.LastHandoff と一致確認)。
type HandoffStartedMsg struct {
	Params workflow.HandoffParams
}

// HandoffFailedMsg は LaunchHandoffCmd が def.Run から非 nil err を受領した
// ときに Cmd 戻り値として発行する tea.Msg (Task 3.3, Req 7.7)。
//
// 呼び出し側 (Task 7.1) は本 msg 受信で State.Detail.HandoffErr に
// エラー文を格納し、PhaseAkashic / Detail サブビューに留まる (tea.Quit を発行
// しない、Phase 遷移もしない)。
//
// Params は LaunchHandoffCmd 呼び出し時に渡された HandoffParams をそのまま
// 保持し、どの hand-off が失敗したかを呼び出し側で照合できるようにする。
// Err は def.Run が返したエラーを errors.Is で判別可能なまま保持する。
type HandoffFailedMsg struct {
	Params workflow.HandoffParams
	Err    error
}

// ListRunsCmd は Reader.List を非同期に呼び、結果を ListRunsMsg として返す tea.Cmd を生成する。
//
// 仕様:
//   - Reader == nil のとき: Reader.List を呼ばず、Err=errReaderUnavailable の
//     ListRunsMsg (zero result) を返す Cmd を即座に返す (Req 11.2)。
//   - Reader != nil のとき: opts.Limit を record.DefaultListLimit (=50) に
//     必ず上書きしてから Reader.List(ctx, opts) を呼ぶ (Req 9.5)。
//   - ctx は context.Background() を用いる。Bubble Tea から Cmd 単位で
//     キャンセルする経路は現状未配備のため、ここでは透過する。
func ListRunsCmd(reader record.Reader, opts record.ListOptions) tea.Cmd {
	if reader == nil {
		return func() tea.Msg {
			return ListRunsMsg{Err: errReaderUnavailable}
		}
	}
	// Req 9.5: ページサイズ 50 件で固定。呼び出し側が異なる Limit を渡しても上書き。
	opts.Limit = record.DefaultListLimit
	return func() tea.Msg {
		result, err := reader.List(context.Background(), opts)
		return ListRunsMsg{Result: result, Err: err}
	}
}

// AggregateByWorkflowCmd は Reader.AggregateByWorkflow を非同期に呼び、
// 結果を AggregateByWorkflowMsg として返す tea.Cmd を生成する。
//
// 仕様:
//   - Reader == nil のとき: Reader メソッドを呼ばず、Err=errReaderUnavailable の
//     AggregateByWorkflowMsg (zero result) を返す。
//   - Reader != nil のとき: opts をそのまま渡して Reader.AggregateByWorkflow を呼ぶ。
//     Run 一覧サブビューのフィルタとは独立であることは呼び出し側の責務 (Req 4.4)。
func AggregateByWorkflowCmd(reader record.Reader, opts record.WorkflowAggregateOptions) tea.Cmd {
	if reader == nil {
		return func() tea.Msg {
			return AggregateByWorkflowMsg{Err: errReaderUnavailable}
		}
	}
	return func() tea.Msg {
		result, err := reader.AggregateByWorkflow(context.Background(), opts)
		return AggregateByWorkflowMsg{Result: result, Err: err}
	}
}

// AggregateByProviderCmd は Reader.AggregateByProvider を非同期に呼び、
// 結果を AggregateByProviderMsg として返す tea.Cmd を生成する。
//
// 仕様:
//   - Reader == nil のとき: Reader メソッドを呼ばず、Err=errReaderUnavailable の
//     AggregateByProviderMsg (zero result) を返す。
//   - Reader != nil のとき: opts をそのまま渡して Reader.AggregateByProvider を呼ぶ。
func AggregateByProviderCmd(reader record.Reader, opts record.ProviderAggregateOptions) tea.Cmd {
	if reader == nil {
		return func() tea.Msg {
			return AggregateByProviderMsg{Err: errReaderUnavailable}
		}
	}
	return func() tea.Msg {
		result, err := reader.AggregateByProvider(context.Background(), opts)
		return AggregateByProviderMsg{Result: result, Err: err}
	}
}

// RunDetailCmd は Reader.Detail を非同期に呼び、結果を RunDetailMsg として返す tea.Cmd を生成する。
//
// 仕様:
//   - Reader == nil のとき: Reader メソッドを呼ばず、Err=errReaderUnavailable の
//     RunDetailMsg (zero detail) を返す。
//   - Reader != nil のとき: opts をそのまま渡して Reader.Detail を呼ぶ。
//     RawEvents の Limit / Offset は呼び出し側 (DetailState) が管理する (Req 5.3)。
//     ErrWorkflowRunNotFound は Err にそのまま流し、呼び出し側で
//     errors.Is で判別する (Req 5.4)。
func RunDetailCmd(reader record.Reader, opts record.DetailOptions) tea.Cmd {
	if reader == nil {
		return func() tea.Msg {
			return RunDetailMsg{Err: errReaderUnavailable}
		}
	}
	return func() tea.Msg {
		detail, err := reader.Detail(context.Background(), opts)
		return RunDetailMsg{Detail: detail, Err: err}
	}
}

// GetArtifactsCmd は ArtifactReader.GetArtifacts を非同期に呼び、
// 結果を GetArtifactsMsg として返す tea.Cmd を生成する (Req 6.1)。
//
// 仕様:
//   - ArtifactReader == nil のとき: メソッドを呼ばず、Err=errArtifactReaderUnavailable
//     の GetArtifactsMsg (Rows=nil) を返す Cmd を即座に返す (Req 11.3)。
//   - ArtifactReader != nil のとき: GetArtifacts(ctx, runID) を 1 回だけ呼び、
//     戻り値の rows / err を msg にそのまま格納する。本 Cmd ではファイル本体を
//     読まない (lazy 読み出し: Req 6.2)。本体の読み出しは呼び出し側からの
//     明示的な ReadArtifactCmd 発行に限る。
//   - ctx は context.Background() を用いる (本ファイルの他ヘルパと同じ方針。
//     Bubble Tea からの Cmd 単位キャンセルは現状未配備)。
func GetArtifactsCmd(reader record.ArtifactReader, runID string) tea.Cmd {
	if reader == nil {
		return func() tea.Msg {
			return GetArtifactsMsg{Err: errArtifactReaderUnavailable}
		}
	}
	return func() tea.Msg {
		rows, err := reader.GetArtifacts(context.Background(), runID)
		return GetArtifactsMsg{Rows: rows, Err: err}
	}
}

// ReadArtifactCmd は ArtifactReader.ReadArtifact を非同期に呼び、
// 結果を ReadArtifactMsg として返す tea.Cmd を生成する (Req 6.3)。
//
// 仕様:
//   - ArtifactReader == nil のとき: メソッドを呼ばず、Err=errArtifactReaderUnavailable
//     の ReadArtifactMsg (Path 保持 / Data=nil) を返す Cmd を即座に返す (Req 11.3)。
//     Path は入力をそのまま msg.Path に格納し、呼び出し側がメタ表示と紐付け
//     できるようにする。
//   - ArtifactReader != nil のとき: 必ず record.ContextWithRunCWD で cwd を
//     ctx に注入してから ReadArtifact(ctx, path) を呼ぶ (Req 6.3 / 8.3)。
//     これは ArtifactReader 実装が allowlist 検証で cwd を必要とするためで、
//     Cmd ヘルパ側で確実に注入することで呼び出し側 (State) の責務を軽くする。
//   - 上限超過 (record.ErrArtifactTooLarge) や削除済み (record.ErrArtifactFileMissing)
//     などの sentinel error は Err フィールドにそのまま流し、呼び出し側で
//     errors.Is で判別する (Req 6.5 / 6.7)。
//   - ReadArtifactUnlimited は絶対に呼ばない (Req 6.6 / 8.4)。本ファイル内に
//     ReadArtifactUnlimited という識別子を出現させない不変式は
//     TestAkashicIODoesNotReferenceReadArtifactUnlimited で検査する。
func ReadArtifactCmd(reader record.ArtifactReader, cwd, path string) tea.Cmd {
	if reader == nil {
		return func() tea.Msg {
			return ReadArtifactMsg{Path: path, Err: errArtifactReaderUnavailable}
		}
	}
	return func() tea.Msg {
		// Req 6.3 / 8.3: ArtifactReader.ReadArtifact は ctx 上の cwd を allowlist
		// 検証で要求する。Cmd 側で必ず ContextWithRunCWD を通すことで、
		// 呼び出し側 (State) は cwd を Cmd 引数で渡すだけで済む。
		ctx := record.ContextWithRunCWD(context.Background(), cwd)
		data, err := reader.ReadArtifact(ctx, path)
		return ReadArtifactMsg{Path: path, Data: data, Err: err}
	}
}

// LaunchHandoffCmd は改善ワークフロー (improve-workflow) hand-off を起動する
// tea.Cmd を生成する (Task 3.3, Req 7.2 / 7.7 / 7.8 / 9.2 / 10.2 / 10.4)。
//
// 仕様:
//   - 内部で workflow.ContextWithHandoff(ctx, params) を呼び、HandoffParams を
//     ctx に乗せた上で def.Run(ctx, cwd, onEvent, ask) を起動する (Req 7.2 / 7.5)
//   - WorkflowEvent (テキスト / ツール / ステップ / 完了 / エラー) は、
//     呼び出し側から渡された EventForwarder に転送する。TUI シェル側は
//     `func(ev) { events <- workflowEventMsg{event: ev} }` を渡しており、
//     これにより hand-off 後 (Phase=PhaseWorkflow) の画面更新は既存経路
//     (waitForEvent → applyWorkflowEvent) にそのまま委ねられる
//   - 起動成功時 (def.Run が err=nil で戻る) は HandoffStartedMsg{Params} を
//     Cmd 戻り値として返す。呼び出し側 (Task 7.1) はこの msg 受信で
//     Model.Phase = PhaseWorkflow に遷移する (Req 7.8)
//   - 即時エラー時 (def.Run が err != nil で戻る) は HandoffFailedMsg{Params, Err}
//     を Cmd 戻り値として返す。呼び出し側はモードを抜けず、Detail.HandoffErr に
//     エラー文を格納し PhaseAkashic に留まる (Req 7.7)。tea.Quit は発行しない
//   - ask は workflow.WorkflowAsk (TUI シェル側の workflowAsk 実装) を渡し、
//     改善ワークフローが SelectParams を要求した場合の経路を既存挙動と一致
//     させる
//
// 設計選択 (Approach: 同期呼出):
//
//	本 Cmd は def.Run を直接同期で呼ぶ。Bubble Tea は tea.Cmd を独立した
//	goroutine で実行するため、cmd() のブロッキングは Update / 他の Cmd の
//	進行を妨げない。同期呼出により「起動成功 / 即時エラー」を Cmd 戻り値の
//	型で直接判別でき、テスト観測 (HandoffStartedMsg / HandoffFailedMsg の
//	Cmd 戻り値検査) と Req 7.7 の「失敗時はモードを抜けず PhaseAkashic に
//	留まる」整合性が両立する。WorkflowEvent はその間 forward へ
//	転送され続ける。
//
// 不変式:
//   - 戻り msg は HandoffStartedMsg と HandoffFailedMsg のいずれか 1 つで、
//     Params は呼び出し時に渡された値と完全一致する (Req 10.4 の観測点)
//   - 失敗時でも HandoffParams は必ず ctx に注入された後で def.Run が呼ばれる
//     (起動経路自体は実行されたことを呼び出し側で確認できるようにする)
func LaunchHandoffCmd(forward EventForwarder, ask workflow.WorkflowAsk, def workflow.Definition, params workflow.HandoffParams, cwd string) tea.Cmd {
	return func() tea.Msg {
		// Req 7.2 / 7.5: HandoffParams を ctx に注入してから Definition.Run を呼ぶ。
		// Definition.Run は内部で ctx をさらに ContextWithRunMetadata /
		// ContextWithRecorder で wrap するが、ctx の親方向探索により Fn 側からも
		// HandoffFromContext で取り出せる。
		ctx := workflow.ContextWithHandoff(context.Background(), params)
		err := def.Run(ctx, cwd, func(event workflow.WorkflowEvent) {
			if forward != nil {
				forward(event)
			}
		}, ask)
		if err != nil {
			// Req 7.7: 即時エラー時は呼び出し側 (Task 7.1) で
			// Detail.HandoffErr に格納し PhaseAkashic に留まる経路の入口
			return HandoffFailedMsg{Params: params, Err: err}
		}
		// Req 7.8: 起動成功時は呼び出し側 (Task 7.1) で Phase=PhaseWorkflow に
		// 遷移させ、以降は既存ワークフロー実行画面の挙動に委ねる
		return HandoffStartedMsg{Params: params}
	}
}
