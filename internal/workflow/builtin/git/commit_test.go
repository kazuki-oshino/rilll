package git

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	agentpkg "github.com/kazuki-oshino/rilll/internal/agent"
	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

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

type scriptedCommitAgent struct {
	responses []provider.AgentResponse
	errs      []error
	prompts   []string
	options   []*workflowctx.CallAgentOptions
}

func (s *scriptedCommitAgent) CallAgent(ctx context.Context, prompt string, cwd string, onEvent provider.AgentEventCallback, resumeSessionID string, options *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
	idx := len(s.prompts)
	s.prompts = append(s.prompts, prompt)
	s.options = append(s.options, options)
	if onEvent != nil {
		onEvent(provider.AgentEvent{Type: provider.EventText, Text: "stream"})
	}
	if idx >= len(s.responses) {
		return provider.AgentResponse{}, errors.New("unexpected CallAgent")
	}
	var err error
	if idx < len(s.errs) {
		err = s.errs[idx]
	}
	return s.responses[idx], err
}

type commitPolicyStubProvider struct {
	id      provider.ProviderID
	request provider.AgentRequest
}

func (s *commitPolicyStubProvider) ID() provider.ProviderID { return s.id }

func (s *commitPolicyStubProvider) Call(_ context.Context, request provider.AgentRequest, _ provider.AgentEventCallback) (provider.AgentResponse, error) {
	s.request = request
	return provider.AgentResponse{Content: "ok", Success: true}, nil
}

// CommitCallOptions 経由で fast=false が agent 層の effort クリア後も wire に残ること。
func TestCommitCallOptionsForwardsFastFalseThroughAgent(t *testing.T) {
	cursorSDK := &commitPolicyStubProvider{id: provider.ProviderCursorSDK}
	agentClient := agentpkg.New(provider.ProviderCursorSDK, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderCursorSDK: cursorSDK,
	})
	agentClient.SetProviderModelDefaults(map[provider.ProviderID]provider.ModelSettings{
		provider.ProviderCursorSDK: {Model: "gpt-5.5", Effort: "xhigh"},
	})

	_, err := agentClient.CallAgent(context.Background(), "/git-commit", "/repo", nil, "", CommitCallOptions("git-commit", "git commit"))
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if cursorSDK.request.Model != CursorSDKGitCommitModel {
		t.Fatalf("cursor-sdk model = %q, want %q", cursorSDK.request.Model, CursorSDKGitCommitModel)
	}
	if cursorSDK.request.Effort != CursorSDKGitCommitEffort {
		t.Fatalf("cursor-sdk effort = %q, want empty", cursorSDK.request.Effort)
	}
	if len(cursorSDK.request.ModelParams) != 1 || cursorSDK.request.ModelParams[0].ID != "fast" || cursorSDK.request.ModelParams[0].Value != "false" {
		t.Fatalf("cursor-sdk model params = %#v, want fast=false", cursorSDK.request.ModelParams)
	}
}

func TestCommitCallOptionsForwardsLunaMediumThroughAgent(t *testing.T) {
	codex := &commitPolicyStubProvider{id: provider.ProviderCodex}
	agentClient := agentpkg.New(provider.ProviderCodex, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderCodex: codex,
	})
	agentClient.SetProviderModelDefaults(map[provider.ProviderID]provider.ModelSettings{
		provider.ProviderCodex: {Model: "gpt-5.6-sol", Effort: "max"},
	})
	_, err := CallCommit(context.Background(), agentClient, "/repo", nil, "commit", "commit")
	if err != nil {
		t.Fatal(err)
	}
	if codex.request.Model != "gpt-6-luna" || codex.request.Effort != "medium" {
		t.Fatalf("commit request = %#v; want GPT-6 Luna / medium", codex.request)
	}
}

func TestCommitCallOptionsRequestsProviderSpecificModelAndEffortOverrides(t *testing.T) {
	options := CommitCallOptions("commit-custom", "custom commit")

	if options == nil {
		t.Fatal("CommitCallOptions returned nil")
	}
	if options.StepID != "commit-custom" || options.StepName != "custom commit" {
		t.Fatalf("step metadata mismatch: %#v", options)
	}
	models := options.ProviderModelOverrides
	if models[provider.ProviderCursorSDK] != CursorSDKGitCommitModel {
		t.Fatalf("cursor-sdk model override = %q, want %q (all overrides=%#v)", models[provider.ProviderCursorSDK], CursorSDKGitCommitModel, models)
	}
	if models[provider.ProviderCodex] != CodexGitCommitModel {
		t.Fatalf("codex model override = %q, want %q (all overrides=%#v)", models[provider.ProviderCodex], CodexGitCommitModel, models)
	}
	if models[provider.ProviderClaude] != "" || models[provider.ProviderCursor] != "" {
		t.Fatalf("git-commit should only override cursor-sdk and codex models, got %#v", models)
	}
	efforts := options.ProviderEffortOverrides
	if efforts[provider.ProviderCodex] != CodexGitCommitEffort {
		t.Fatalf("codex effort override = %q, want %q (all overrides=%#v)", efforts[provider.ProviderCodex], CodexGitCommitEffort, efforts)
	}
	// Cursor SDK は明示的に空文字 entry を持つ (provider default Effort をクリアし、
	// composer-2.5 で reasoning を確実に omit させるため。agent.go の ok 判定 semantics に依存)。
	if cs, ok := efforts[provider.ProviderCursorSDK]; !ok || cs != CursorSDKGitCommitEffort {
		t.Fatalf("cursor-sdk effort override should be present and empty to clear provider default, got value=%q ok=%v (all=%#v)", cs, ok, efforts)
	}
	params := options.ProviderModelParamsOverrides
	if len(params[provider.ProviderCursorSDK]) != 1 || params[provider.ProviderCursorSDK][0].ID != "fast" || params[provider.ProviderCursorSDK][0].Value != "false" {
		t.Fatalf("cursor-sdk model params override = %#v, want fast=false", params[provider.ProviderCursorSDK])
	}
	if efforts[provider.ProviderClaude] != "" || efforts[provider.ProviderCursor] != "" {
		t.Fatalf("git-commit should only override codex/cursor-sdk effort, got %#v", efforts)
	}
}

func TestCallCommitFallbacksToCursorSDKOnProviderError(t *testing.T) {
	agent := &scriptedCommitAgent{
		responses: []provider.AgentResponse{
			{
				Content:    "Selected model is at capacity",
				Success:    false,
				SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderCodex},
			},
			{
				Content:    "committed with cursor sdk",
				Success:    true,
				SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderCursorSDK},
			},
		},
		errs: []error{
			&agentpkg.CallError{Category: agentpkg.FailureCategoryProviderError, Err: errors.New("capacity")},
			nil,
		},
	}

	resp, err := CallCommit(context.Background(), agent, t.TempDir(), func(workflow.WorkflowEvent) {}, "git-commit", "git commit")
	if err != nil {
		t.Fatalf("CallCommit returned fallback error: %v", err)
	}
	if !resp.Success || resp.Content != "committed with cursor sdk" {
		t.Fatalf("CallCommit should return cursor-sdk fallback response, got %#v", resp)
	}
	if len(agent.prompts) != 2 {
		t.Fatalf("expected initial call + fallback call, got prompts=%#v", agent.prompts)
	}
	if agent.options[0].ProviderOverride != "" {
		t.Fatalf("initial call should not force provider override, got %#v", agent.options[0])
	}
	fallbackOptions := agent.options[1]
	if fallbackOptions.ProviderOverride != provider.ProviderCursorSDK {
		t.Fatalf("fallback ProviderOverride = %q, want %q", fallbackOptions.ProviderOverride, provider.ProviderCursorSDK)
	}
	if fallbackOptions.ProviderModelOverrides[provider.ProviderCursorSDK] != CursorSDKGitCommitModel {
		t.Fatalf("fallback cursor-sdk model = %q, want %q", fallbackOptions.ProviderModelOverrides[provider.ProviderCursorSDK], CursorSDKGitCommitModel)
	}
}

func TestCallCommitFallbacksToCursorSDKOnProviderErrorResponse(t *testing.T) {
	agent := &scriptedCommitAgent{
		responses: []provider.AgentResponse{
			{
				Content:    "Selected model is at capacity. Please try a different model.",
				Success:    false,
				SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderCodex},
			},
			{
				Content:    "committed with cursor sdk",
				Success:    true,
				SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderCursorSDK},
			},
		},
	}

	resp, err := CallCommit(context.Background(), agent, t.TempDir(), func(workflow.WorkflowEvent) {}, "git-commit", "git commit")
	if err != nil {
		t.Fatalf("CallCommit returned fallback error: %v", err)
	}
	if !resp.Success || resp.Content != "committed with cursor sdk" {
		t.Fatalf("CallCommit should return cursor-sdk fallback response, got %#v", resp)
	}
	if len(agent.prompts) != 2 {
		t.Fatalf("expected initial call + fallback call, got prompts=%#v", agent.prompts)
	}
	if agent.options[1].ProviderOverride != provider.ProviderCursorSDK {
		t.Fatalf("fallback ProviderOverride = %q, want %q", agent.options[1].ProviderOverride, provider.ProviderCursorSDK)
	}
}

func TestCallCommitDoesNotFallbackWhenInitialProviderIsCursorSDK(t *testing.T) {
	wantErr := &agentpkg.CallError{Category: agentpkg.FailureCategoryProviderError, Err: errors.New("cursor sdk failed")}
	agent := &scriptedCommitAgent{
		responses: []provider.AgentResponse{{
			Content:    "cursor sdk failed",
			Success:    false,
			SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderCursorSDK},
		}},
		errs: []error{wantErr},
	}

	resp, err := CallCommit(context.Background(), agent, t.TempDir(), func(workflow.WorkflowEvent) {}, "git-commit", "git commit")
	if !errors.Is(err, wantErr) {
		t.Fatalf("CallCommit err = %v, want %v", err, wantErr)
	}
	if resp.SystemInfo == nil || resp.SystemInfo.ProviderID != provider.ProviderCursorSDK {
		t.Fatalf("CallCommit should return original cursor-sdk response, got %#v", resp)
	}
	if len(agent.prompts) != 1 {
		t.Fatalf("cursor-sdk initial failure should not fallback, got prompts=%#v", agent.prompts)
	}
}

func TestCallCommitDoesNotFallbackOnUnsuccessfulAgentResultWithoutProviderError(t *testing.T) {
	agent := &scriptedCommitAgent{
		responses: []provider.AgentResponse{{
			Content:    "nothing to commit",
			Success:    false,
			SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderCodex},
		}},
	}

	resp, err := CallCommit(context.Background(), agent, t.TempDir(), func(workflow.WorkflowEvent) {}, "git-commit", "git commit")
	if err != nil {
		t.Fatalf("CallCommit should preserve nil error for application-level failure, got %v", err)
	}
	if resp.Success {
		t.Fatalf("CallCommit should return original unsuccessful response, got %#v", resp)
	}
	if len(agent.prompts) != 1 {
		t.Fatalf("application-level failure should not fallback, got prompts=%#v", agent.prompts)
	}
}

func TestGitCommitWorkflowCallsSlashCommandAndForwardsEvents(t *testing.T) {
	agent := &fakeAgent{resp: provider.AgentResponse{Content: "committed", Success: true}}
	var events []workflow.WorkflowEvent

	err := Commit(agent).Run(context.Background(), t.TempDir(), func(event workflow.WorkflowEvent) {
		events = append(events, event)
	}, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(agent.prompts) != 1 || agent.prompts[0] != "/git-commit" {
		t.Fatalf("unexpected prompts: %#v", agent.prompts)
	}
	if events[len(events)-1].Type != workflow.WorkflowDone || events[len(events)-1].Result != "committed" {
		t.Fatalf("workflow did not finish with agent content: %#v", events)
	}
}

func TestWorkflowRunAddsMetadataToAgentContext(t *testing.T) {
	agent := &fakeAgent{resp: provider.AgentResponse{Content: "committed", Success: true}}

	if err := Commit(agent).Run(context.Background(), t.TempDir(), func(workflow.WorkflowEvent) {}, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(agent.metadata) != 1 {
		t.Fatalf("expected one metadata record, got %#v", agent.metadata)
	}
	got := agent.metadata[0]
	if got.WorkflowID != "git-commit" || got.WorkflowName != "git commit" || got.WorkflowRunID == "" {
		t.Fatalf("unexpected workflow metadata: %#v", got)
	}
	if len(agent.options) != 1 || agent.options[0] == nil || agent.options[0].StepID != "git-commit" || agent.options[0].StepName != "git commit" {
		t.Fatalf("step metadata was not attached: %#v", agent.options)
	}
}

func TestGitCommitWorkflowRequestsProviderSpecificModelAndEffortOverrides(t *testing.T) {
	agent := &fakeAgent{resp: provider.AgentResponse{Content: "committed", Success: true}}

	if err := Commit(agent).Run(context.Background(), t.TempDir(), func(workflow.WorkflowEvent) {}, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(agent.options) != 1 || agent.options[0] == nil {
		t.Fatalf("expected one CallAgentOptions, got %#v", agent.options)
	}
	got := agent.options[0].ProviderModelOverrides
	if got[provider.ProviderCursorSDK] != CursorSDKGitCommitModel {
		t.Fatalf("cursor-sdk model override = %q, want %q (all overrides=%#v)", got[provider.ProviderCursorSDK], CursorSDKGitCommitModel, got)
	}
	if got[provider.ProviderCodex] != CodexGitCommitModel {
		t.Fatalf("codex model override = %q, want %q (all overrides=%#v)", got[provider.ProviderCodex], CodexGitCommitModel, got)
	}
	if got[provider.ProviderClaude] != "" || got[provider.ProviderCursor] != "" {
		t.Fatalf("git-commit should only override cursor-sdk and codex models, got %#v", got)
	}
	efforts := agent.options[0].ProviderEffortOverrides
	if efforts[provider.ProviderCodex] != CodexGitCommitEffort {
		t.Fatalf("codex effort override = %q, want %q (all overrides=%#v)", efforts[provider.ProviderCodex], CodexGitCommitEffort, efforts)
	}
	if cs, ok := efforts[provider.ProviderCursorSDK]; !ok || cs != CursorSDKGitCommitEffort {
		t.Fatalf("cursor-sdk effort override should be present and empty to clear provider default, got value=%q ok=%v (all=%#v)", cs, ok, efforts)
	}
}

type workflowRecorder struct {
	starts   []akashic.WorkflowRun
	finishes []akashic.WorkflowRunFinish
	// runArtifacts は RecordRunArtifact 経由で受信した RunArtifact を時系列順に蓄積する。
	// akashic-artifacts spec Task 5.1 で git_commit ワークフローの result 登録を assert する
	// ターゲット (本 Task 3.1 ではフィールド導入と interface 追従のみ)。
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

// RecordRunArtifact は akashic.Recorder interface 拡張 (akashic-artifacts spec Task 3.1) への
// 追従。Task 5.1 で result 登録を assert するため受信した RunArtifact を蓄積する。
func (r *workflowRecorder) RecordRunArtifact(_ context.Context, artifact akashic.RunArtifact) error {
	r.runArtifacts = append(r.runArtifacts, artifact)
	return nil
}

// RecordUserCorrection は memory-distiller-trigger-policy spec Task 3.3 で追加した
// Recorder interface 拡張への追従 (no-op)。git-commit workflow test は workflow run /
// artifact 経路の検証が責務で user-correction marker 経路は対象外のため履歴は保持しない。
func (r *workflowRecorder) RecordUserCorrection(context.Context, string) error {
	return nil
}

// RecordLearning は agent-learnings spec (task 3.2) で Recorder interface に追加された
// メソッドへの no-op 追従。git-commit workflow test は workflow run / artifact 経路の
// 検証が責務で learnings 経路は対象外のため履歴は保持しない。
func (r *workflowRecorder) RecordLearning(context.Context, akashic.LearningRow) error {
	return nil
}

func (r *workflowRecorder) Close() error { return nil }

func TestWorkflowRunRecordsStartAndFinish(t *testing.T) {
	recorder := &workflowRecorder{}
	agent := &fakeAgent{resp: provider.AgentResponse{Content: "committed", Success: true}, recorder: recorder}

	if err := Commit(agent).Run(context.Background(), t.TempDir(), func(workflow.WorkflowEvent) {}, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(recorder.starts) != 1 || recorder.starts[0].WorkflowID != "git-commit" || recorder.starts[0].ID == "" {
		t.Fatalf("workflow start was not recorded: %#v", recorder.starts)
	}
	if len(recorder.finishes) != 1 || recorder.finishes[0].ID != recorder.starts[0].ID || recorder.finishes[0].Error != "" {
		t.Fatalf("workflow finish was not recorded: %#v", recorder.finishes)
	}
}

// TestGitCommitWorkflowRecordsResultArtifact は akashic-artifacts spec Task 5.1 の
// 観測可能な完了条件を assert する: 正常系の git_commit ワークフローが result.json を
// 書き出した直後に workflow.RecordRunArtifact を呼び、recorder が Kind=result + 絶対
// clean path の RunArtifact を 1 件受信する (Req 2.1)。
//
// ワークフロー本体の戻り値・WorkflowDone イベントは既存テストで確認済みのため、
// 本テストは "登録呼び出しの side effect" のみに焦点を絞る。
func TestGitCommitWorkflowRecordsResultArtifact(t *testing.T) {
	recorder := &workflowRecorder{}
	agent := &fakeAgent{resp: provider.AgentResponse{Content: "committed", Success: true}, recorder: recorder}
	cwd := t.TempDir()

	if err := Commit(agent).Run(context.Background(), cwd, func(workflow.WorkflowEvent) {}, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(recorder.runArtifacts) != 1 {
		t.Fatalf("expected 1 RunArtifact recorded, got %d: %#v", len(recorder.runArtifacts), recorder.runArtifacts)
	}
	got := recorder.runArtifacts[0]
	if got.Kind != akashic.ArtifactKindResult {
		t.Errorf("RunArtifact.Kind = %q, want %q", got.Kind, akashic.ArtifactKindResult)
	}
	if len(recorder.starts) != 1 {
		t.Fatalf("expected workflow start recorded, got %#v", recorder.starts)
	}
	runID := recorder.starts[0].ID
	expectedPath := filepath.Join(cwd, ".rilll", "git-commit", runID, "result.json")
	// NormalizeArtifactPath 経由で EvalSymlinks 済み絶対 clean path に揃えるため、
	// 比較側も同じ正規化を適用する。
	expectedCanonical, err := filepath.EvalSymlinks(expectedPath)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", expectedPath, err)
	}
	if got.Path != filepath.Clean(expectedCanonical) {
		t.Errorf("RunArtifact.Path = %q, want %q", got.Path, filepath.Clean(expectedCanonical))
	}
	if got.WorkflowRunID != runID {
		t.Errorf("RunArtifact.WorkflowRunID = %q, want %q", got.WorkflowRunID, runID)
	}
}

func TestWorkflowRunRecordsFinishError(t *testing.T) {
	recorder := &workflowRecorder{}
	def := workflow.Definition{
		ID:       "failing",
		Label:    "Failing",
		Recorder: recorder,
		Fn: func(context.Context, string, func(workflow.WorkflowEvent), workflow.WorkflowAsk) error {
			return errors.New("boom")
		},
	}

	err := def.Run(context.Background(), t.TempDir(), func(workflow.WorkflowEvent) {}, nil)
	if err == nil {
		t.Fatal("expected workflow error")
	}
	if len(recorder.finishes) != 1 || recorder.finishes[0].Error != "boom" {
		t.Fatalf("workflow error was not recorded: %#v", recorder.finishes)
	}
}

func TestWorkflowRunRecordsWorkflowErrorEvent(t *testing.T) {
	recorder := &workflowRecorder{}
	def := workflow.Definition{
		ID:       "logical-failure",
		Label:    "Logical Failure",
		Recorder: recorder,
		Fn: func(_ context.Context, _ string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: "agent failed"})
			return nil
		},
	}

	if err := def.Run(context.Background(), t.TempDir(), func(workflow.WorkflowEvent) {}, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(recorder.finishes) != 1 || recorder.finishes[0].Error != "agent failed" {
		t.Fatalf("workflow error event was not recorded: %#v", recorder.finishes)
	}
}
