package workflow

import (
	"context"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// recorderKey は ctx 上で akashic.Recorder を運ぶための private 型。
// 別パッケージから ctx を直接覗けないようにする。
type recorderKey struct{}

type WorkflowEventType string

const (
	WorkflowText      WorkflowEventType = "text"
	WorkflowReasoning WorkflowEventType = "reasoning"
	WorkflowToolUse   WorkflowEventType = "tool_use"
	WorkflowStep      WorkflowEventType = "step"
	WorkflowDone      WorkflowEventType = "done"
	WorkflowError     WorkflowEventType = "error"
	// WorkflowActive は現在アクティブなワークフロー名（Label）を伝える。
	// Sequential などの複合ワークフローが子の実行に切り替わるタイミングで発する。
	WorkflowActive WorkflowEventType = "active"
)

type WorkflowEvent struct {
	Type       WorkflowEventType
	Text       string
	ItemID     string
	Tool       string
	Step       int
	TotalSteps int
	Label      string
	Result     string
	Message    string
	// RunID は本イベントを emit した Definition.Run の workflow_run_id を伝搬する。
	// shared run core の wrappedOnEvent が Fn から流れてきた WorkflowEvent に対し、
	// 既に non-empty なら上書きせず、空なら自身の runID を埋める契約 (改名 / 廃止時は
	// `apply.ReviewAndApply` の Sequential ラッパが本フィールドを傍受する経路と同時改訂)。
	//
	// 用途: Sequential 合成 (例: `apply.ReviewAndApply`) で前段 child の WorkflowRunID を
	// 後段 ctx に hand-off するための seam (review-apply-workflow tasks.md 6.1 / Req 1.2, 8.5)。
	// ネスト Run でも内側の runID が優先される: 子 Run の wrappedOnEvent が先に runID を
	// 埋めるため、親 Run は既存値を保持する (空のときのみ自身の runID を埋める)。
	RunID string
}

// ArgSpec はワークフローが必要とする引数のスキーマを宣言する。
// CLI 層は Definition.Args を見て --<Key> フラグを動的生成し、
// `rilll list` / `rilll run --help` のヘルプに反映する。
type ArgSpec struct {
	Key         string
	Description string
	Required    bool
}

type Func func(ctx context.Context, cwd string, onEvent func(WorkflowEvent), ask WorkflowAsk) error

type Definition struct {
	ID       string
	Label    string
	Args     []ArgSpec
	Fn       Func
	Recorder akashic.Recorder
	// Hidden は true のとき TUI トップメニュー / `rilll list` / `rilll help` の
	// 一覧表示から本ワークフローを除外する (hand-off 専用ワークフロー用)。
	// Registry には残るため、akashic Run 詳細からの hand-off (`i` キー) など
	// 既知の起動経路からは引き続き発見・起動できる。
	// `findDefinition` (CLI の `rilll run <id>` 直叩き) は弾かないため、
	// 直叩き時は本来の precondition で fail する従来動作を維持する。
	Hidden bool
}

func (d Definition) Run(ctx context.Context, cwd string, onEvent func(WorkflowEvent), ask WorkflowAsk) error {
	return runDefinitionWithResult(ctx, d, cwd, onEvent, ask, definitionRunOptions{}).ReturnedError
}

// ContextWithRecorder は akashic.Recorder を ctx に乗せる。
// recorder == nil でも呼び出し可能で、その場合は「nil 注入」として扱われる。
// recorderFromContext からは未注入と nil 注入が区別不要な (nil, false) として観測される。
func ContextWithRecorder(ctx context.Context, recorder akashic.Recorder) context.Context {
	return context.WithValue(ctx, recorderKey{}, recorder)
}

// recorderFromContext は ctx に注入された akashic.Recorder を返す。
// 未注入 / nil 注入のいずれの場合も (nil, false) を返す。
// 呼び出し側はこの bool を「Recorder 利用可否」の唯一のシグナルとして扱える。
func recorderFromContext(ctx context.Context) (akashic.Recorder, bool) {
	raw := ctx.Value(recorderKey{})
	if raw == nil {
		return nil, false
	}
	recorder, ok := raw.(akashic.Recorder)
	if !ok || recorder == nil {
		return nil, false
	}
	return recorder, true
}

// WithStep は CallAgentOptions に StepID / StepName を上書きしたコピーを返す。
// options == nil でも空 *CallAgentOptions を新規生成して返すため呼び出し側は nil 受け取り可。
func WithStep(options *workflowctx.CallAgentOptions, stepID string, stepName string) *workflowctx.CallAgentOptions {
	if options == nil {
		return &workflowctx.CallAgentOptions{StepID: stepID, StepName: stepName}
	}
	cloned := *options
	cloned.StepID = stepID
	cloned.StepName = stepName
	return &cloned
}

// RecorderFromAgent は agent が AkashicRecorderProvider を実装している場合に
// akashic.Recorder を取り出す。未実装、または AkashicRecorder() が nil を返す場合は
// nil を返す (Definition.Recorder への注入経路で利用される)。
func RecorderFromAgent(agent workflowctx.AgentCaller) akashic.Recorder {
	if provider, ok := agent.(workflowctx.AkashicRecorderProvider); ok {
		return provider.AkashicRecorder()
	}
	return nil
}

// ReaderFromAgent は agent が AkashicReaderProvider を実装している場合に
// akashic.Reader を取り出す。未実装、または AkashicReader() が nil を返す場合は
// nil を返す (design.md §workflow/optional iface の Postcondition)。
// RecorderFromAgent と完全に同パターン。
func ReaderFromAgent(agent workflowctx.AgentCaller) akashic.Reader {
	if provider, ok := agent.(workflowctx.AkashicReaderProvider); ok {
		return provider.AkashicReader()
	}
	return nil
}

// ArtifactReaderFromAgent は agent が AkashicArtifactReaderProvider を実装している場合に
// akashic.ArtifactReader を取り出す。未実装、または AkashicArtifactReader() が nil を返す場合は
// nil を返す (design.md §workflow/optional iface の Postcondition)。
// RecorderFromAgent と完全に同パターン。
func ArtifactReaderFromAgent(agent workflowctx.AgentCaller) akashic.ArtifactReader {
	if provider, ok := agent.(workflowctx.AkashicArtifactReaderProvider); ok {
		return provider.AkashicArtifactReader()
	}
	return nil
}

// RunIDFromContext は ctx 上の RunMetadata.WorkflowRunID を返す。
// RunMetadata 不在 / WorkflowRunID 空文字列のときは新規 ID を生成して返す
// (Definition.Run 経路外から呼ばれた防御的経路)。
func RunIDFromContext(ctx context.Context) string {
	if metadata, ok := workflowctx.RunMetadataFromContext(ctx); ok && metadata.WorkflowRunID != "" {
		return metadata.WorkflowRunID
	}
	return generateRunID()
}

// ForwardEvents は provider.AgentEvent を WorkflowEvent に変換するアダプタ。
// 子パッケージの workflow 実装が agent.CallAgent に渡す callback として利用される。
func ForwardEvents(onEvent func(WorkflowEvent)) provider.AgentEventCallback {
	return func(event provider.AgentEvent) {
		switch event.Type {
		case provider.EventText:
			onEvent(WorkflowEvent{Type: WorkflowText, Text: event.Text})
		case provider.EventReasoning:
			onEvent(WorkflowEvent{Type: WorkflowReasoning, Text: event.Text, ItemID: event.ItemID})
		case provider.EventToolUse:
			onEvent(WorkflowEvent{Type: WorkflowToolUse, Tool: event.Tool})
		}
	}
}
