package kiro

import (
	"context"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
	gitworkflow "github.com/kazuki-oshino/rilll/internal/workflow/builtin/git"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// fakeAgent は kiro サブパッケージ共通の AgentCaller スタブ。
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

func (f *fakeAgent) AkashicRecorder() akashic.Recorder { return f.recorder }

func assertGitCommitProviderOverrides(t testing.TB, options *workflowctx.CallAgentOptions) {
	t.Helper()
	if options == nil {
		t.Fatal("git-commit CallAgentOptions is nil")
	}
	models := options.ProviderModelOverrides
	if models[provider.ProviderCursorSDK] != gitworkflow.CursorSDKGitCommitModel {
		t.Fatalf("cursor-sdk model override = %q, want %q (all overrides=%#v)", models[provider.ProviderCursorSDK], gitworkflow.CursorSDKGitCommitModel, models)
	}
	if models[provider.ProviderCodex] != gitworkflow.CodexGitCommitModel {
		t.Fatalf("codex model override = %q, want %q (all overrides=%#v)", models[provider.ProviderCodex], gitworkflow.CodexGitCommitModel, models)
	}
	if models[provider.ProviderClaude] != "" || models[provider.ProviderCursor] != "" {
		t.Fatalf("git-commit should only override cursor-sdk and codex models, got %#v", models)
	}
	efforts := options.ProviderEffortOverrides
	if efforts[provider.ProviderCodex] != gitworkflow.CodexGitCommitEffort {
		t.Fatalf("codex effort override = %q, want %q (all overrides=%#v)", efforts[provider.ProviderCodex], gitworkflow.CodexGitCommitEffort, efforts)
	}
	if efforts[provider.ProviderCursorSDK] != "" || efforts[provider.ProviderClaude] != "" || efforts[provider.ProviderCursor] != "" {
		t.Fatalf("git-commit should only override codex effort, got %#v", efforts)
	}
}

// workflowRecorder は kiro サブパッケージ共通の akashic.Recorder スタブ。
// 受信した WorkflowRun / WorkflowRunFinish / RunArtifact を時系列順に蓄積する。
type workflowRecorder struct {
	starts       []akashic.WorkflowRun
	finishes     []akashic.WorkflowRunFinish
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
// Recorder interface 拡張への追従 (no-op)。kiro パッケージ test は workflow run / artifact
// 経路の検証が責務で user-correction 経路は対象外のため履歴は保持しない。
func (r *workflowRecorder) RecordUserCorrection(context.Context, string) error {
	return nil
}

// RecordLearning は agent-learnings spec (task 3.2) で Recorder interface に追加された
// メソッドへの no-op 追従。kiro パッケージ test は workflow run / artifact 経路の
// 検証が責務で learnings 経路は対象外のため履歴は保持しない。
func (r *workflowRecorder) RecordLearning(context.Context, akashic.LearningRow) error {
	return nil
}

func (r *workflowRecorder) Close() error { return nil }
