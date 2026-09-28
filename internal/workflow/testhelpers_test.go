package workflow

import (
	"context"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// fakeAgent はパッケージ内テスト共通の AgentCaller スタブ。
// CallAgent 呼び出しの prompts / metadata / options を蓄積し、固定応答 resp を返す。
// recorder を non-nil にすると AkashicRecorder() がそれを返し、Definition.Recorder の
// 経路をテスト可能にする。
type fakeAgent struct {
	prompts  []string
	metadata []workflowctx.RunMetadata
	options  []*workflowctx.CallAgentOptions
	resp     provider.AgentResponse
	recorder akashic.Recorder
}

func (f *fakeAgent) CallAgent(ctx context.Context, prompt string, cwd string, onEvent provider.AgentEventCallback, resumeSessionID string, options *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
	f.prompts = append(f.prompts, prompt)
	if metadata, ok := workflowctx.RunMetadataFromContext(ctx); ok {
		f.metadata = append(f.metadata, metadata)
	}
	f.options = append(f.options, options)
	if onEvent != nil {
		onEvent(provider.AgentEvent{Type: provider.EventText, Text: "stream"})
	}
	return f.resp, nil
}

func (f *fakeAgent) AkashicRecorder() akashic.Recorder {
	return f.recorder
}

// workflowRecorder はパッケージ内テスト共通の akashic.Recorder スタブ。
// 受信した WorkflowRun / WorkflowRunFinish / RunArtifact を時系列に蓄積する。
type workflowRecorder struct {
	starts       []akashic.WorkflowRun
	finishes     []akashic.WorkflowRunFinish
	childRuns    []akashic.WorkflowRunChild
	runArtifacts []akashic.RunArtifact
}

func (r *workflowRecorder) RecordWorkflowRunStart(_ context.Context, run akashic.WorkflowRun) error {
	r.starts = append(r.starts, run)
	return nil
}

func (r *workflowRecorder) RecordWorkflowRunFinish(_ context.Context, finish akashic.WorkflowRunFinish) error {
	r.finishes = append(r.finishes, finish)
	return nil
}

func (r *workflowRecorder) RecordWorkflowRunChild(_ context.Context, child akashic.WorkflowRunChild) error {
	r.childRuns = append(r.childRuns, child)
	return nil
}

func (r *workflowRecorder) RecordAgentCallStart(context.Context, akashic.AgentCall) error {
	return nil
}

func (r *workflowRecorder) RecordAgentCallFinish(context.Context, akashic.AgentCallResult) error {
	return nil
}

func (r *workflowRecorder) RecordRawEvent(context.Context, akashic.RawEvent) error {
	return nil
}

func (r *workflowRecorder) RecordRunArtifact(_ context.Context, artifact akashic.RunArtifact) error {
	r.runArtifacts = append(r.runArtifacts, artifact)
	return nil
}

// RecordUserCorrection は memory-distiller-trigger-policy spec Task 3.3 で追加した
// Recorder interface 拡張への追従 (no-op)。本パッケージ test は workflow run/artifact
// 経路の検証が責務で user-correction 経路は対象外のため履歴は保持しない。
func (r *workflowRecorder) RecordUserCorrection(context.Context, string) error {
	return nil
}

// RecordLearning は agent-learnings spec (task 3.2) で Recorder interface に追加された
// メソッドへの no-op 追従。本パッケージ test は workflow run/artifact 経路の検証が責務で
// learnings 経路は対象外のため履歴は保持しない。
func (r *workflowRecorder) RecordLearning(context.Context, akashic.LearningRow) error {
	return nil
}

func (r *workflowRecorder) Close() error { return nil }
