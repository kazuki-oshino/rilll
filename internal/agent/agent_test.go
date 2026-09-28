package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/bridge"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/provider/middleware/streamidle"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type stubProvider struct {
	id      provider.ProviderID
	called  bool
	request provider.AgentRequest
	event   *provider.AgentEvent
	resp    provider.AgentResponse
}

func (s *stubProvider) ID() provider.ProviderID { return s.id }

func (s *stubProvider) Call(_ context.Context, request provider.AgentRequest, onEvent provider.AgentEventCallback) (provider.AgentResponse, error) {
	s.called = true
	s.request = request
	if s.event != nil && onEvent != nil {
		onEvent(*s.event)
	}
	if s.resp.Success || s.resp.Content != "" {
		return s.resp, nil
	}
	return provider.AgentResponse{Content: "ok", Success: true}, nil
}

func TestCallAgentUsesSelectedProvider(t *testing.T) {
	claude := &stubProvider{id: provider.ProviderClaude}
	cursor := &stubProvider{id: provider.ProviderCursor}
	agent := New(provider.ProviderCursor, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
		provider.ProviderCursor: cursor,
	})

	resp, err := agent.CallAgent(context.Background(), "prompt", "/repo", nil, "", nil)
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if !resp.Success || !cursor.called || claude.called {
		t.Fatalf("selected provider was not used: resp=%#v cursor=%v claude=%v", resp, cursor.called, claude.called)
	}
}

func TestSetProviderChangesSelectedProvider(t *testing.T) {
	claude := &stubProvider{id: provider.ProviderClaude}
	cursor := &stubProvider{id: provider.ProviderCursor}
	agent := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
		provider.ProviderCursor: cursor,
	})

	agent.SetProvider(provider.ProviderCursor)
	_, err := agent.CallAgent(context.Background(), "prompt", "/repo", nil, "", nil)
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if !cursor.called || claude.called {
		t.Fatalf("provider switch was not applied: cursor=%v claude=%v", cursor.called, claude.called)
	}
}

func TestCallAgentMapsWorkflowMetadataToProviderRequest(t *testing.T) {
	claude := &stubProvider{id: provider.ProviderClaude}
	agent := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	})
	ctx := workflowctx.ContextWithRunMetadata(context.Background(), workflowctx.RunMetadata{
		WorkflowRunID: "workflow-run-1",
		WorkflowID:    "update-readme",
		WorkflowName:  "README更新",
	})

	_, err := agent.CallAgent(ctx, "prompt", "/repo", nil, "", &workflowctx.CallAgentOptions{
		StepID:   "plan",
		StepName: "提案作成",
	})
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}

	got := claude.request.Metadata
	if got.WorkflowRunID != "workflow-run-1" || got.WorkflowID != "update-readme" || got.WorkflowName != "README更新" {
		t.Fatalf("workflow metadata was not mapped: %#v", got)
	}
	if got.StepID != "plan" || got.StepName != "提案作成" || got.AgentCallID == "" {
		t.Fatalf("call metadata was not mapped: %#v", got)
	}
}

func TestCallAgentMapsAkashicLevelToProviderRequest(t *testing.T) {
	claude := &stubProvider{id: provider.ProviderClaude}
	agent := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	})

	_, err := agent.CallAgent(context.Background(), "prompt", "/repo", nil, "", nil)
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if claude.request.AkashicLevel != "summary" {
		t.Fatalf("unexpected default akashic level: %#v", claude.request)
	}

	agent.SetAkashicLevel("debug")
	_, err = agent.CallAgent(context.Background(), "prompt", "/repo", nil, "", nil)
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if claude.request.AkashicLevel != "debug" {
		t.Fatalf("akashic level was not mapped: %#v", claude.request)
	}
}

func TestCallAgentInjectsTraceContextToProviderRequest(t *testing.T) {
	oldProvider := otel.GetTracerProvider()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otel.SetTracerProvider(tracerProvider)
	defer func() {
		_ = tracerProvider.Shutdown(context.Background())
		otel.SetTracerProvider(oldProvider)
	}()

	claude := &stubProvider{id: provider.ProviderClaude}
	agent := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	})

	_, err := agent.CallAgent(context.Background(), "prompt", "/repo", nil, "", nil)
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}

	if claude.request.TraceContext.TraceParent == "" {
		t.Fatalf("trace context was not injected into provider request: %#v", claude.request.TraceContext)
	}
}

type recordingRecorder struct {
	fail      bool
	starts    []akashic.AgentCall
	finishes  []akashic.AgentCallResult
	rawEvents []akashic.RawEvent
	// runArtifacts は RecordRunArtifact 経由で受信した RunArtifact のカウント観測用。
	// akashic-artifacts spec Task 3.1 では値そのものではなく interface 拡張の追従を担保する。
	runArtifacts []akashic.RunArtifact
	// userCorrections は RecordUserCorrection 経由で受信した agent_call_id を観測する。
	// memory-distiller-trigger-policy spec Task 4.3 の origin 4 ケース test (a/b/c/d) で
	// 「呼ばれたか / どの id で呼ばれたか」を assert するために履歴を保持する。
	userCorrections []string

	learningReflectionJobs []akashic.LearningReflectionJob
	learningReflectionErr  error
	recordOrder            []string
}

func (r *recordingRecorder) RecordWorkflowRunStart(context.Context, akashic.WorkflowRun) error {
	return r.err()
}

func (r *recordingRecorder) RecordWorkflowRunFinish(context.Context, akashic.WorkflowRunFinish) error {
	return r.err()
}

func (r *recordingRecorder) RecordAgentCallStart(_ context.Context, call akashic.AgentCall) error {
	r.starts = append(r.starts, call)
	r.recordOrder = append(r.recordOrder, "start:"+call.ID)
	return r.err()
}

func (r *recordingRecorder) RecordAgentCallFinish(_ context.Context, result akashic.AgentCallResult) error {
	r.finishes = append(r.finishes, result)
	r.recordOrder = append(r.recordOrder, "finish:"+result.ID)
	return r.err()
}

func (r *recordingRecorder) RecordRawEvent(_ context.Context, event akashic.RawEvent) error {
	r.rawEvents = append(r.rawEvents, event)
	return r.err()
}

// RecordRunArtifact は akashic.Recorder interface 拡張 (akashic-artifacts spec Task 3.1) への
// 追従。受信した RunArtifact を蓄積するだけのテスト用 no-op 相当実装で、
// fail フラグに従って既存挙動と整合した error 返却を行う。
func (r *recordingRecorder) RecordRunArtifact(_ context.Context, artifact akashic.RunArtifact) error {
	r.runArtifacts = append(r.runArtifacts, artifact)
	return r.err()
}

// RecordUserCorrection は memory-distiller-trigger-policy spec Task 3.3 で追加した
// Recorder interface 拡張への追従。Task 4.3 の origin 4 ケース test では
// userCorrections の append 履歴で「呼ばれた / 呼ばれない」を直接 assert する。
// fail フラグに従って既存挙動と整合した error 返却を行う。
func (r *recordingRecorder) RecordUserCorrection(_ context.Context, agentCallID string) error {
	r.userCorrections = append(r.userCorrections, agentCallID)
	return r.err()
}

// RecordLearning は agent-learnings spec (task 3.2) で Recorder interface に追加された
// メソッドへの no-op 追従。本 test は learnings 経路を観測対象としないため履歴を保持しない
// (失敗注入だけは err() 経路で揃える)。
func (r *recordingRecorder) RecordLearning(_ context.Context, _ akashic.LearningRow) error {
	return r.err()
}

func (r *recordingRecorder) RecordLearningReflectionJob(_ context.Context, job akashic.LearningReflectionJob) (akashic.LearningReflectionEnqueueResult, error) {
	r.learningReflectionJobs = append(r.learningReflectionJobs, job)
	r.recordOrder = append(r.recordOrder, "learning-reflection:"+job.AgentCallID)
	if r.learningReflectionErr != nil {
		return akashic.LearningReflectionEnqueueResult{}, r.learningReflectionErr
	}
	if err := r.err(); err != nil {
		return akashic.LearningReflectionEnqueueResult{}, err
	}
	return akashic.LearningReflectionEnqueueResult{Job: job, Inserted: true}, nil
}

func (r *recordingRecorder) Close() error { return nil }

func (r *recordingRecorder) err() error {
	if r.fail {
		return errors.New("recording failed")
	}
	return nil
}

func TestCallAgentRecordsStartFinishAndRawObservation(t *testing.T) {
	recorder := &recordingRecorder{}
	claude := &stubProvider{
		id: provider.ProviderClaude,
		event: &provider.AgentEvent{
			Type:               provider.EventObservation,
			Source:             "claude-sdk",
			ObservationType:    "result",
			ObservationSubtype: "success",
			Sequence:           7,
			Payload:            []byte(`{"type":"result"}`),
			PayloadTruncated:   false,
		},
		resp: provider.AgentResponse{
			Content:   "done",
			SessionID: "session-1",
			Success:   true,
			Metrics: provider.AgentMetrics{
				TotalCostUSD:  floatPtr(0.25),
				InputTokens:   int64Ptr(12),
				OutputTokens:  int64Ptr(5),
				ResultPayload: map[string]any{"subtype": "success"},
			},
		},
	}
	agent := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	}, recorder)
	ctx := workflowctx.ContextWithRunMetadata(context.Background(), workflowctx.RunMetadata{
		WorkflowRunID: "workflow-run-1",
		WorkflowID:    "update-readme",
		WorkflowName:  "README更新",
	})

	var forwarded []provider.AgentEvent
	resp, err := agent.CallAgent(ctx, "prompt", "/repo", func(event provider.AgentEvent) {
		forwarded = append(forwarded, event)
	}, "", &workflowctx.CallAgentOptions{StepID: "plan", StepName: "提案作成"})
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if !resp.Success {
		t.Fatalf("unexpected response: %#v", resp)
	}
	if len(forwarded) != 0 {
		t.Fatalf("observation events should not be forwarded to UI callbacks: %#v", forwarded)
	}
	if len(recorder.starts) != 1 || recorder.starts[0].WorkflowRunID != "workflow-run-1" || recorder.starts[0].ProviderID != "claude" {
		t.Fatalf("start was not recorded: %#v", recorder.starts)
	}
	if len(recorder.rawEvents) != 1 || recorder.rawEvents[0].AgentCallID != recorder.starts[0].ID || recorder.rawEvents[0].Sequence != 7 {
		t.Fatalf("raw event was not recorded: %#v", recorder.rawEvents)
	}
	if len(recorder.finishes) != 1 || !recorder.finishes[0].Success || recorder.finishes[0].SessionID != "session-1" {
		t.Fatalf("finish was not recorded: %#v", recorder.finishes)
	}
	if !recorder.finishes[0].TotalCostUSD.Valid || recorder.finishes[0].TotalCostUSD.Float64 != 0.25 {
		t.Fatalf("metrics were not recorded: %#v", recorder.finishes[0])
	}
}

func TestCallAgentEarlyReflectionMarkerAbsenceDoesNotRecordJob(t *testing.T) {
	recorder := &recordingRecorder{}
	claude := &stubProvider{
		id: provider.ProviderClaude,
		resp: provider.AgentResponse{
			Content:   "done",
			SessionID: "session-1",
			Success:   true,
		},
	}
	agent := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	}, recorder)
	ctx := workflowctx.ContextWithRunMetadata(context.Background(), workflowctx.RunMetadata{
		WorkflowRunID: "workflow-run-early",
		WorkflowID:    "wf",
		WorkflowName:  "Workflow",
	})

	if _, err := agent.CallAgent(ctx, "prompt", t.TempDir(), nil, "", &workflowctx.CallAgentOptions{StepID: "plan", StepName: "Plan"}); err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}

	if len(recorder.learningReflectionJobs) != 0 {
		t.Fatalf("early reflection job must not be recorded without marker: %#v", recorder.learningReflectionJobs)
	}
}

func TestCallAgentEarlyReflectionEnqueuesAfterFinishAndWakes(t *testing.T) {
	recorder := &recordingRecorder{}
	cwd := t.TempDir()
	claude := &stubProvider{
		id: provider.ProviderClaude,
		resp: provider.AgentResponse{
			Content:   "done",
			SessionID: "session-1",
			Success:   true,
		},
	}
	agent := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	}, recorder)
	notifyEntered := make(chan struct{}, 1)
	releaseNotify := make(chan struct{})
	t.Cleanup(func() { close(releaseNotify) })
	ctx := workflowctx.ContextWithRunMetadata(context.Background(), workflowctx.RunMetadata{
		WorkflowRunID: "workflow-run-early",
		WorkflowID:    "wf",
		WorkflowName:  "Workflow",
	})
	ctx = workflowctx.ContextWithEarlyReflectionEnabled(ctx, workflowctx.EarlyReflectionPolicy{
		AllowedProviders: []provider.ProviderID{provider.ProviderClaude},
		Notify: func() {
			notifyEntered <- struct{}{}
			<-releaseNotify
		},
	})

	done := make(chan error, 1)
	go func() {
		_, err := agent.CallAgent(ctx, "prompt", cwd, nil, "", &workflowctx.CallAgentOptions{StepID: "plan", StepName: "Plan"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("CallAgent returned error: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("CallAgent blocked on early reflection Notify")
	}
	select {
	case <-notifyEntered:
	case <-time.After(time.Second):
		t.Fatal("early reflection Notify was not called")
	}

	if len(recorder.finishes) != 1 {
		t.Fatalf("finish was not recorded: %#v", recorder.finishes)
	}
	if len(recorder.learningReflectionJobs) != 1 {
		t.Fatalf("early reflection job was not recorded once: %#v", recorder.learningReflectionJobs)
	}
	job := recorder.learningReflectionJobs[0]
	if job.AgentCallID != recorder.finishes[0].ID || job.AgentCallID != recorder.starts[0].ID {
		t.Fatalf("job agent_call_id mismatch: job=%q start=%q finish=%q", job.AgentCallID, recorder.starts[0].ID, recorder.finishes[0].ID)
	}
	if job.WorkflowRunID != "workflow-run-early" || job.WorkflowID != "wf" || job.WorkflowName != "Workflow" {
		t.Fatalf("job workflow metadata mismatch: %#v", job)
	}
	if job.StepID != "plan" || job.StepName != "Plan" {
		t.Fatalf("job step metadata mismatch: %#v", job)
	}
	if job.ProviderID != string(provider.ProviderClaude) || job.CWD != cwd || job.ProjectID != akashic.ProjectIDFromCWD(cwd) {
		t.Fatalf("job provider/cwd/project mismatch: %#v", job)
	}
	if job.SessionID != "session-1" || job.Status != akashic.LearningReflectionStatusPending || job.SkipReason != "" {
		t.Fatalf("eligible job should be pending with session: %#v", job)
	}
	if !orderBefore(recorder.recordOrder, "finish:"+job.AgentCallID, "learning-reflection:"+job.AgentCallID) {
		t.Fatalf("finish must be recorded before early reflection job: order=%v", recorder.recordOrder)
	}
}

func TestCallAgentEarlyReflectionRecordsSkippedJobs(t *testing.T) {
	tests := []struct {
		name             string
		resp             provider.AgentResponse
		allowedProviders []provider.ProviderID
		wantSkipReason   akashic.LearningReflectionSkipReason
	}{
		{
			name: "empty session",
			resp: provider.AgentResponse{
				Content: "done",
				Success: true,
			},
			allowedProviders: []provider.ProviderID{provider.ProviderClaude},
			wantSkipReason:   akashic.LearningReflectionSkipEmptySessionID,
		},
		{
			name: "provider denied",
			resp: provider.AgentResponse{
				Content:   "done",
				SessionID: "session-1",
				Success:   true,
			},
			allowedProviders: []provider.ProviderID{provider.ProviderCursor},
			wantSkipReason:   akashic.LearningReflectionSkipProviderNotInAllowlist,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &recordingRecorder{}
			claude := &stubProvider{id: provider.ProviderClaude, resp: tt.resp}
			agent := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
				provider.ProviderClaude: claude,
			}, recorder)
			ctx := workflowctx.ContextWithRunMetadata(context.Background(), workflowctx.RunMetadata{
				WorkflowRunID: "workflow-run-skip",
				WorkflowID:    "wf",
				WorkflowName:  "Workflow",
			})
			ctx = workflowctx.ContextWithEarlyReflectionEnabled(ctx, workflowctx.EarlyReflectionPolicy{
				AllowedProviders: tt.allowedProviders,
			})

			if _, err := agent.CallAgent(ctx, "prompt", t.TempDir(), nil, "", &workflowctx.CallAgentOptions{StepID: "plan", StepName: "Plan"}); err != nil {
				t.Fatalf("CallAgent returned error: %v", err)
			}

			if len(recorder.learningReflectionJobs) != 1 {
				t.Fatalf("early reflection skipped job was not recorded once: %#v", recorder.learningReflectionJobs)
			}
			job := recorder.learningReflectionJobs[0]
			if job.Status != akashic.LearningReflectionStatusSkipped || job.SkipReason != tt.wantSkipReason {
				t.Fatalf("unexpected skipped job: got status=%q reason=%q want status=%q reason=%q job=%#v",
					job.Status, job.SkipReason, akashic.LearningReflectionStatusSkipped, tt.wantSkipReason, job)
			}
			if !orderBefore(recorder.recordOrder, "finish:"+job.AgentCallID, "learning-reflection:"+job.AgentCallID) {
				t.Fatalf("finish must be recorded before skipped job: order=%v", recorder.recordOrder)
			}
		})
	}
}

func TestCallAgentEarlyReflectionRecorderErrorDoesNotFailParentCall(t *testing.T) {
	recorder := &recordingRecorder{learningReflectionErr: errors.New("enqueue failed")}
	claude := &stubProvider{
		id: provider.ProviderClaude,
		resp: provider.AgentResponse{
			Content:   "done",
			SessionID: "session-1",
			Success:   true,
		},
	}
	agent := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	}, recorder)
	ctx := workflowctx.ContextWithRunMetadata(context.Background(), workflowctx.RunMetadata{
		WorkflowRunID: "workflow-run-early",
		WorkflowID:    "wf",
		WorkflowName:  "Workflow",
	})
	ctx = workflowctx.ContextWithEarlyReflectionEnabled(ctx, workflowctx.EarlyReflectionPolicy{
		AllowedProviders: []provider.ProviderID{provider.ProviderClaude},
	})

	resp, err := agent.CallAgent(ctx, "prompt", t.TempDir(), nil, "", nil)
	if err != nil {
		t.Fatalf("CallAgent should ignore early reflection enqueue error, got: %v", err)
	}
	if !resp.Success {
		t.Fatalf("parent response should remain successful: %#v", resp)
	}
	if len(recorder.learningReflectionJobs) != 1 {
		t.Fatalf("early reflection enqueue should still be attempted once: %#v", recorder.learningReflectionJobs)
	}
}

func orderBefore(order []string, before string, after string) bool {
	beforeIndex := -1
	afterIndex := -1
	for i, entry := range order {
		switch entry {
		case before:
			beforeIndex = i
		case after:
			afterIndex = i
		}
	}
	return beforeIndex >= 0 && afterIndex >= 0 && beforeIndex < afterIndex
}

func TestCallAgentRecordsResolvedModelAndEffort(t *testing.T) {
	recorder := &recordingRecorder{}
	codex := &stubProvider{
		id: provider.ProviderCodex,
		resp: provider.AgentResponse{
			Content:   "done",
			SessionID: "session-1",
			Success:   true,
			SystemInfo: &provider.AgentSystemInfo{
				Model: "gpt-5.3-codex-spark",
			},
		},
	}
	agentClient := New(provider.ProviderCodex, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderCodex: codex,
	}, recorder)

	_, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", &workflowctx.CallAgentOptions{
		EffortOverride: "high",
	})
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if len(recorder.finishes) != 1 {
		t.Fatalf("finish was not recorded: %#v", recorder.finishes)
	}
	got := recorder.finishes[0]
	if got.Model != "gpt-5.3-codex-spark" {
		t.Fatalf("finish model = %q, want gpt-5.3-codex-spark", got.Model)
	}
	if got.ModelReasoningEffort != "high" {
		t.Fatalf("finish effort = %q, want high", got.ModelReasoningEffort)
	}
}

func TestCallAgentIgnoresRecorderFailures(t *testing.T) {
	recorder := &recordingRecorder{fail: true}
	claude := &stubProvider{id: provider.ProviderClaude}
	agent := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	}, recorder)

	resp, err := agent.CallAgent(context.Background(), "prompt", "/repo", nil, "", nil)
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if !resp.Success {
		t.Fatalf("recorder failure should not fail agent call: %#v", resp)
	}
}

func TestCallAgentRecordsStreamIdleFailureCategory(t *testing.T) {
	recorder := &recordingRecorder{}
	claude := &erroringProvider{id: provider.ProviderClaude, err: streamidle.ErrStreamIdleTimeout}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	}, recorder)

	_, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", nil)
	if !errors.Is(err, streamidle.ErrStreamIdleTimeout) {
		t.Fatalf("expected idle error to propagate, got %v", err)
	}
	if got := CategoryFromError(err); got != FailureCategoryStreamIdleTimeout {
		t.Fatalf("expected category to be %q, got %q", FailureCategoryStreamIdleTimeout, got)
	}
	if got := err.Error(); !strings.Contains(got, "[stream_idle_timeout]") {
		t.Fatalf("error message should include category prefix, got %q", got)
	}
	if len(recorder.finishes) != 1 || recorder.finishes[0].FailureCategory != FailureCategoryStreamIdleTimeout {
		t.Fatalf("failure category was not recorded: %#v", recorder.finishes)
	}
}

func TestCallAgentRecordsExternalAbortOnContextCanceled(t *testing.T) {
	recorder := &recordingRecorder{}
	claude := &erroringProvider{id: provider.ProviderClaude, err: context.Canceled}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	}, recorder)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := agentClient.CallAgent(ctx, "prompt", "/repo", nil, "", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if got := CategoryFromError(err); got != FailureCategoryExternalAbort {
		t.Fatalf("expected category to be %q, got %q", FailureCategoryExternalAbort, got)
	}
	if len(recorder.finishes) != 1 || recorder.finishes[0].FailureCategory != FailureCategoryExternalAbort {
		t.Fatalf("failure category was not recorded: %#v", recorder.finishes)
	}
}

// --- review-workflow Task 1.3: per-call Provider / Model 上書き seam ---
//
// 設計規約 (review-workflow design.md §agent / provider 拡張):
//   - CallAgentOptions.ProviderOverride が zero-value のときは既存挙動 (a.providerID) を維持する。
//   - ProviderOverride 指定時は a.providers[override] を使い、akashic agent_calls.provider_id
//     にも override 後の値を記録する。
//   - ModelOverride は provider.AgentRequest.Model に素通しし、zero-value のときは provider 既定モデル
//     (空文字列) を維持する。
//   - 解決できない ProviderOverride は agent.ErrUnknownProvider sentinel をラップした
//     *CallError{Category: FailureCategoryProviderUnknown} を返し、akashic finish には
//     failure_category="provider_unknown" として記録する。

// TestCallAgentZeroValueOptionsBehaveAsBefore は ProviderOverride/ModelOverride を zero-value にしたとき
// 既存挙動と等価 (a.providerID の provider が呼ばれ、request.Model は空文字列のまま) であることを検証する。
func TestCallAgentZeroValueOptionsBehaveAsBefore(t *testing.T) {
	claude := &stubProvider{id: provider.ProviderClaude}
	cursor := &stubProvider{id: provider.ProviderCursor}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
		provider.ProviderCursor: cursor,
	})

	resp, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", &workflowctx.CallAgentOptions{StepID: "plan"})
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if !resp.Success || !claude.called || cursor.called {
		t.Fatalf("zero-value override should keep selected provider: claude=%v cursor=%v", claude.called, cursor.called)
	}
	if claude.request.Model != "" {
		t.Fatalf("zero-value ModelOverride should keep request.Model empty, got %q", claude.request.Model)
	}
}

// TestCallAgentProviderOverrideSwitchesProvider は ProviderOverride で a.providerID 以外の provider が
// 選択されることを検証する。
func TestCallAgentProviderOverrideSwitchesProvider(t *testing.T) {
	claude := &stubProvider{id: provider.ProviderClaude}
	cursor := &stubProvider{id: provider.ProviderCursor}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
		provider.ProviderCursor: cursor,
	})

	_, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", &workflowctx.CallAgentOptions{
		ProviderOverride: provider.ProviderCursor,
	})
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if !cursor.called || claude.called {
		t.Fatalf("ProviderOverride was not honored: claude=%v cursor=%v", claude.called, cursor.called)
	}
}

// TestCallAgentModelOverrideForwardsToProviderRequest は ModelOverride が provider.AgentRequest.Model
// に素通しされることを検証する。
func TestCallAgentModelOverrideForwardsToProviderRequest(t *testing.T) {
	claude := &stubProvider{id: provider.ProviderClaude}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	})

	_, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", &workflowctx.CallAgentOptions{
		ModelOverride: "claude-opus-4-1-20250805",
	})
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if claude.request.Model != "claude-opus-4-1-20250805" {
		t.Fatalf("ModelOverride was not forwarded to AgentRequest.Model: %#v", claude.request)
	}
}

func TestCallAgentProviderModelDefaultForwardsModelAndEffort(t *testing.T) {
	codex := &stubProvider{id: provider.ProviderCodex}
	agentClient := New(provider.ProviderCodex, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderCodex: codex,
	})
	agentClient.SetProviderModelDefaults(map[provider.ProviderID]provider.ModelSettings{
		provider.ProviderCodex: {Model: "gpt-5.5", Effort: "xhigh"},
	})

	_, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", nil)
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if codex.request.Model != "gpt-5.5" || codex.request.Effort != "xhigh" {
		t.Fatalf("provider model default was not forwarded: %#v", codex.request)
	}
}

func TestCallAgentProviderModelDefaultForwardsModelParams(t *testing.T) {
	cursorSDK := &stubProvider{id: provider.ProviderCursorSDK}
	agentClient := New(provider.ProviderCursorSDK, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderCursorSDK: cursorSDK,
	})
	agentClient.SetProviderModelDefaults(map[provider.ProviderID]provider.ModelSettings{
		provider.ProviderCursorSDK: {
			Model: "gpt-5.6-luna",
			ModelParams: []provider.ModelParam{
				{ID: "reasoning", Value: "max"},
			},
		},
	})

	_, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", nil)
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if len(cursorSDK.request.ModelParams) != 1 || cursorSDK.request.ModelParams[0] != (provider.ModelParam{ID: "reasoning", Value: "max"}) {
		t.Fatalf("provider model params were not forwarded: %#v", cursorSDK.request)
	}
}

func TestCallAgentModelOverrideClearsProviderModelParams(t *testing.T) {
	cursorSDK := &stubProvider{id: provider.ProviderCursorSDK}
	agentClient := New(provider.ProviderCursorSDK, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderCursorSDK: cursorSDK,
	})
	agentClient.SetProviderModelDefaults(map[provider.ProviderID]provider.ModelSettings{
		provider.ProviderCursorSDK: {
			Model: "gpt-5.6-luna",
			ModelParams: []provider.ModelParam{
				{ID: "reasoning", Value: "max"},
			},
		},
	})

	_, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", &workflowctx.CallAgentOptions{
		ModelOverride: "composer-2.5",
	})
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if len(cursorSDK.request.ModelParams) != 0 {
		t.Fatalf("model override must clear params for the previous model: %#v", cursorSDK.request)
	}
}

func TestCallAgentProviderDefaultsForwardReasoningSummary(t *testing.T) {
	codex := &stubProvider{id: provider.ProviderCodex}
	agentClient := New(provider.ProviderCodex, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderCodex: codex,
	})
	agentClient.SetProviderModelDefaults(map[provider.ProviderID]provider.ModelSettings{
		provider.ProviderCodex: {ReasoningSummary: "concise"},
	})

	_, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", nil)
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if codex.request.ReasoningSummary != "concise" {
		t.Fatalf("reasoning summary default was not forwarded: %#v", codex.request)
	}
}

func TestCallAgentOptionOverridesProviderModelDefault(t *testing.T) {
	codex := &stubProvider{id: provider.ProviderCodex}
	agentClient := New(provider.ProviderCodex, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderCodex: codex,
	})
	agentClient.SetProviderModelDefaults(map[provider.ProviderID]provider.ModelSettings{
		provider.ProviderCodex: {Model: "gpt-5.5", Effort: "xhigh"},
	})

	_, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", &workflowctx.CallAgentOptions{
		ModelOverride:  "gpt-5.3-codex-spark",
		EffortOverride: "high",
	})
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if codex.request.Model != "gpt-5.3-codex-spark" || codex.request.Effort != "high" {
		t.Fatalf("CallAgentOptions should override provider defaults: %#v", codex.request)
	}
}

func TestCallAgentProviderModelOverrideUsesResolvedProvider(t *testing.T) {
	claude := &stubProvider{id: provider.ProviderClaude}
	cursorSDK := &stubProvider{id: provider.ProviderCursorSDK}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude:    claude,
		provider.ProviderCursorSDK: cursorSDK,
	})

	_, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", &workflowctx.CallAgentOptions{
		ProviderOverride: provider.ProviderCursorSDK,
		ModelOverride:    "fallback-model",
		ProviderModelOverrides: map[provider.ProviderID]string{
			provider.ProviderCursorSDK: "composer-2.5",
		},
	})
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if !cursorSDK.called || claude.called {
		t.Fatalf("expected cursor-sdk provider only: cursorSDK=%v claude=%v", cursorSDK.called, claude.called)
	}
	if cursorSDK.request.Model != "composer-2.5" {
		t.Fatalf("provider-specific override should win for resolved provider, got %q", cursorSDK.request.Model)
	}
}

// /git-commit シナリオの直接形: provider default で Effort="xhigh" が入っていても、
// ProviderEffortOverrides に空文字 entry を入れれば確実に req.Effort="" に倒される。
// agent.go の ok 判定 semantics に依存する design contract のリグレッション防止。
func TestCallAgentProviderEffortOverrideEmptyValueClearsProviderDefault(t *testing.T) {
	cursorSDK := &stubProvider{id: provider.ProviderCursorSDK}
	agentClient := New(provider.ProviderCursorSDK, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderCursorSDK: cursorSDK,
	})
	agentClient.SetProviderModelDefaults(map[provider.ProviderID]provider.ModelSettings{
		provider.ProviderCursorSDK: {Model: "gpt-5.5", Effort: "xhigh"},
	})

	_, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", &workflowctx.CallAgentOptions{
		ProviderModelOverrides: map[provider.ProviderID]string{
			provider.ProviderCursorSDK: "composer-2.5",
		},
		ProviderEffortOverrides: map[provider.ProviderID]string{
			provider.ProviderCursorSDK: "",
		},
	})
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if cursorSDK.request.Model != "composer-2.5" {
		t.Fatalf("cursor-sdk model override should pin composer-2.5, got %q", cursorSDK.request.Model)
	}
	if cursorSDK.request.Effort != "" {
		t.Fatalf("cursor-sdk effort override with empty value should clear xhigh default, got %q", cursorSDK.request.Effort)
	}
}

// effort override と model params override を同時指定したとき、params が最終的に残ること。
func TestCallAgentProviderModelParamsOverrideSurvivesEffortOverride(t *testing.T) {
	cursorSDK := &stubProvider{id: provider.ProviderCursorSDK}
	agentClient := New(provider.ProviderCursorSDK, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderCursorSDK: cursorSDK,
	})
	agentClient.SetProviderModelDefaults(map[provider.ProviderID]provider.ModelSettings{
		provider.ProviderCursorSDK: {Model: "gpt-5.5", Effort: "xhigh"},
	})

	_, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", &workflowctx.CallAgentOptions{
		ProviderModelOverrides: map[provider.ProviderID]string{
			provider.ProviderCursorSDK: "composer-2.5",
		},
		ProviderModelParamsOverrides: map[provider.ProviderID][]provider.ModelParam{
			provider.ProviderCursorSDK: {{ID: "fast", Value: "false"}},
		},
		ProviderEffortOverrides: map[provider.ProviderID]string{
			provider.ProviderCursorSDK: "",
		},
	})
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if len(cursorSDK.request.ModelParams) != 1 || cursorSDK.request.ModelParams[0].ID != "fast" || cursorSDK.request.ModelParams[0].Value != "false" {
		t.Fatalf("model params should survive effort override, got %#v", cursorSDK.request.ModelParams)
	}
}

func TestCallAgentProviderEffortOverrideUsesResolvedProvider(t *testing.T) {
	codex := &stubProvider{id: provider.ProviderCodex}
	cursorSDK := &stubProvider{id: provider.ProviderCursorSDK}
	agentClient := New(provider.ProviderCodex, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderCodex:     codex,
		provider.ProviderCursorSDK: cursorSDK,
	})

	_, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", &workflowctx.CallAgentOptions{
		ProviderOverride: provider.ProviderCursorSDK,
		EffortOverride:   "xhigh",
		ProviderEffortOverrides: map[provider.ProviderID]string{
			provider.ProviderCursorSDK: "high",
		},
	})
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if !cursorSDK.called || codex.called {
		t.Fatalf("expected cursor-sdk provider only: cursorSDK=%v codex=%v", cursorSDK.called, codex.called)
	}
	if cursorSDK.request.Effort != "high" {
		t.Fatalf("provider-specific effort override should win for resolved provider, got %q", cursorSDK.request.Effort)
	}
}

func TestCallAgentProviderModelOverrideFallsBackToGenericModelOverride(t *testing.T) {
	claude := &stubProvider{id: provider.ProviderClaude}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	})

	_, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", &workflowctx.CallAgentOptions{
		ModelOverride: "claude-sonnet",
		ProviderModelOverrides: map[provider.ProviderID]string{
			provider.ProviderCursorSDK: "composer-2.5",
		},
	})
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if claude.request.Model != "claude-sonnet" {
		t.Fatalf("non-matching provider-specific override should fall back to generic ModelOverride, got %q", claude.request.Model)
	}
}

// ProviderModelOverrides に空文字の entry を入れると、provider default (modelDefaults)
// を明示的にクリアする (= req.Model="" を bridge に流す) semantics へ。
// 汎用 ModelOverride より provider-specific override が優先する性質も同時に検証する
// ため、fallback-model が選択されないことも assert する。
func TestCallAgentProviderModelOverrideEmptyValueClearsProviderDefault(t *testing.T) {
	cursorSDK := &stubProvider{id: provider.ProviderCursorSDK}
	agentClient := New(provider.ProviderCursorSDK, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderCursorSDK: cursorSDK,
	})
	agentClient.SetProviderModelDefaults(map[provider.ProviderID]provider.ModelSettings{
		provider.ProviderCursorSDK: {Model: "gpt-5.5", Effort: "xhigh"},
	})

	_, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", &workflowctx.CallAgentOptions{
		ModelOverride: "fallback-model",
		ProviderModelOverrides: map[provider.ProviderID]string{
			provider.ProviderCursorSDK: "",
		},
	})
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if cursorSDK.request.Model != "" {
		t.Fatalf("empty provider-specific override should clear provider default, got %q", cursorSDK.request.Model)
	}
}

// TestCallAgentAllowedToolsForwardsToProviderRequest は CallAgentOptions.AllowedTools が
// provider.AgentRequest.AllowedTools に素通しされることを検証する。
//
// Phase C: kiro-spec-loop fix-judge ターン等で Edit/Write を禁じる allowedTools 制限を
// 上位 → bridge まで縦に通す seam。zero-value (nil) のときは provider 既定 (= 全ツール許可) を維持する。
func TestCallAgentAllowedToolsForwardsToProviderRequest(t *testing.T) {
	claude := &stubProvider{id: provider.ProviderClaude}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	})

	tools := []string{"Read", "Glob", "Grep"}
	_, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", &workflowctx.CallAgentOptions{
		AllowedTools: tools,
	})
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if len(claude.request.AllowedTools) != 3 {
		t.Fatalf("AllowedTools length: got=%d, want=3 (request=%#v)", len(claude.request.AllowedTools), claude.request)
	}
	for i, want := range tools {
		if claude.request.AllowedTools[i] != want {
			t.Fatalf("AllowedTools[%d]=%q, want %q", i, claude.request.AllowedTools[i], want)
		}
	}
}

// TestCallAgentNilAllowedToolsKeepsProviderDefault は AllowedTools=nil (= 既定) のときに
// provider.AgentRequest.AllowedTools も nil で渡され、SDK 側が既定 (全ツール許可) を維持できることを
// 検証する後方互換ガード。
func TestCallAgentNilAllowedToolsKeepsProviderDefault(t *testing.T) {
	claude := &stubProvider{id: provider.ProviderClaude}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	})

	_, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", &workflowctx.CallAgentOptions{})
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if claude.request.AllowedTools != nil {
		t.Fatalf("AllowedTools should be nil when not specified, got %#v", claude.request.AllowedTools)
	}
}

// TestCallAgentRecordsOverriddenProviderID は akashic agent_calls.provider_id に override 後の値が
// 記録されることを検証する (Req 5.5)。
func TestCallAgentRecordsOverriddenProviderID(t *testing.T) {
	recorder := &recordingRecorder{}
	claude := &stubProvider{id: provider.ProviderClaude}
	cursor := &stubProvider{id: provider.ProviderCursor}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
		provider.ProviderCursor: cursor,
	}, recorder)

	_, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", &workflowctx.CallAgentOptions{
		ProviderOverride: provider.ProviderCursor,
		StepID:           "rule-a",
	})
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if len(recorder.starts) != 1 {
		t.Fatalf("expected exactly one agent_calls start, got %d", len(recorder.starts))
	}
	if got := recorder.starts[0].ProviderID; got != string(provider.ProviderCursor) {
		t.Fatalf("agent_calls.provider_id should reflect override, got %q", got)
	}
}

// TestCallAgentReturnsErrUnknownProviderOnOverride は ProviderOverride が registry に存在しないとき、
// errors.Is(err, agent.ErrUnknownProvider) が成立し、CallError.Category が provider_unknown となり、
// akashic finish にも failure_category="provider_unknown" が記録されることを検証する (Req 5.6)。
func TestCallAgentReturnsErrUnknownProviderOnOverride(t *testing.T) {
	recorder := &recordingRecorder{}
	claude := &stubProvider{id: provider.ProviderClaude}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	}, recorder)

	_, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", &workflowctx.CallAgentOptions{
		ProviderOverride: provider.ProviderID("ghost-provider"),
	})
	if err == nil {
		t.Fatal("CallAgent should return error for unknown ProviderOverride")
	}
	if !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("error should wrap ErrUnknownProvider, got %v", err)
	}
	if got := CategoryFromError(err); got != FailureCategoryProviderUnknown {
		t.Fatalf("CallError.Category should be %q, got %q", FailureCategoryProviderUnknown, got)
	}
	if !strings.Contains(err.Error(), "[provider_unknown]") {
		t.Fatalf("error message should include category prefix, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "unknown agent provider") {
		t.Fatalf("error message should preserve human-readable substring, got %q", err.Error())
	}
	if claude.called {
		t.Fatal("provider.Call should not be invoked when override is unknown")
	}
	if len(recorder.finishes) != 1 || recorder.finishes[0].FailureCategory != FailureCategoryProviderUnknown {
		t.Fatalf("failure_category=provider_unknown was not recorded: %#v", recorder.finishes)
	}
	if len(recorder.starts) != 1 || recorder.starts[0].ProviderID != "ghost-provider" {
		t.Fatalf("agent_calls.start should record override-after provider id, got %#v", recorder.starts)
	}
}

// TestCallAgentReturnsErrUnknownProviderOnSingleProviderMissing は ProviderOverride 未指定でも
// a.providerID が registry に無いときに同じ ErrUnknownProvider 経路を辿ることを検証する。
// (override 経路と single-provider 経路の両方で同一カテゴリが akashic に記録される。)
func TestCallAgentReturnsErrUnknownProviderOnSingleProviderMissing(t *testing.T) {
	recorder := &recordingRecorder{}
	agentClient := New(provider.ProviderID("missing"), map[provider.ProviderID]provider.AgentProvider{}, recorder)

	_, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", nil)
	if err == nil {
		t.Fatal("CallAgent should return error for unknown a.providerID")
	}
	if !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("error should wrap ErrUnknownProvider, got %v", err)
	}
	if got := CategoryFromError(err); got != FailureCategoryProviderUnknown {
		t.Fatalf("CallError.Category should be %q, got %q", FailureCategoryProviderUnknown, got)
	}
	if len(recorder.finishes) != 1 || recorder.finishes[0].FailureCategory != FailureCategoryProviderUnknown {
		t.Fatalf("failure_category=provider_unknown was not recorded: %#v", recorder.finishes)
	}
}

func TestCallAgentRecordsProviderErrorOnGenericFailure(t *testing.T) {
	recorder := &recordingRecorder{}
	wantErr := errors.New("boom")
	claude := &erroringProvider{id: provider.ProviderClaude, err: wantErr}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	}, recorder)

	_, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected provider error, got %v", err)
	}
	if got := CategoryFromError(err); got != FailureCategoryProviderError {
		t.Fatalf("expected category to be %q, got %q", FailureCategoryProviderError, got)
	}
	if len(recorder.finishes) != 1 || recorder.finishes[0].FailureCategory != FailureCategoryProviderError {
		t.Fatalf("failure category was not recorded: %#v", recorder.finishes)
	}
}

type erroringProvider struct {
	id  provider.ProviderID
	err error
}

func (e *erroringProvider) ID() provider.ProviderID { return e.id }

func (e *erroringProvider) Call(_ context.Context, _ provider.AgentRequest, _ provider.AgentEventCallback) (provider.AgentResponse, error) {
	return provider.AgentResponse{}, e.err
}

func floatPtr(value float64) *float64 { return &value }

func int64Ptr(value int64) *int64 { return &value }

// --- Task 10.2: Agent への AkashicReader / AkashicArtifactReader アクセサ追加 ---
//
// 設計規約 (workflow-self-improve design.md §workflow/optional iface):
//   - Agent は recorder が *SQLiteRecorder のとき、その recorder を Reader / ArtifactReader として
//     型アサーションで取り出して保持する (recorder と同 instance)。
//   - recorder が NoopRecorder のときは reader / artifactReader はいずれも nil。
//   - 後方互換のため `New` のシグネチャは変更しない (引数追加なし)。
//
// 観察可能完了条件:
//   - SQLite recorder のとき AkashicReader() / AkashicArtifactReader() が非 nil を返す
//   - NoopRecorder (または recorder 引数なし = 既定 NoopRecorder) のとき両者が nil を返す

// TestAkashicReaderReturnsNonNilWithSQLiteRecorder は recorder が *akashic.SQLiteRecorder のとき
// AkashicReader() / AkashicArtifactReader() がともに非 nil を返し、recorder と同 instance である
// ことを確認する (SQLiteRecorder が Reader / ArtifactReader を兼用している事実を利用)。
func TestAkashicReaderReturnsNonNilWithSQLiteRecorder(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "observability.sqlite")
	recorder, err := akashic.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	t.Cleanup(func() { _ = recorder.Close() })

	claude := &stubProvider{id: provider.ProviderClaude}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	}, recorder)

	if got := agentClient.AkashicReader(); got == nil {
		t.Fatalf("AkashicReader() = nil, want non-nil for *SQLiteRecorder")
	} else if got != akashic.Reader(recorder) {
		t.Fatalf("AkashicReader() identity mismatch: got %p, want %p", got, recorder)
	}

	if got := agentClient.AkashicArtifactReader(); got == nil {
		t.Fatalf("AkashicArtifactReader() = nil, want non-nil for *SQLiteRecorder")
	} else if got != akashic.ArtifactReader(recorder) {
		t.Fatalf("AkashicArtifactReader() identity mismatch: got %p, want %p", got, recorder)
	}
}

// TestAkashicReaderReturnsNilWithNoopRecorder は recorder が NoopRecorder の場合
// (Reader / ArtifactReader interface を満たさない) に AkashicReader() / AkashicArtifactReader() が
// ともに nil を返すことを確認する。
func TestAkashicReaderReturnsNilWithNoopRecorder(t *testing.T) {
	claude := &stubProvider{id: provider.ProviderClaude}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	}, akashic.NoopRecorder{})

	if got := agentClient.AkashicReader(); got != nil {
		t.Fatalf("AkashicReader() = %v, want nil for NoopRecorder", got)
	}
	if got := agentClient.AkashicArtifactReader(); got != nil {
		t.Fatalf("AkashicArtifactReader() = %v, want nil for NoopRecorder", got)
	}
}

// TestAkashicReaderReturnsNilWhenRecorderOmitted は recorder 引数を渡さない (= 既定の NoopRecorder)
// 場合にも AkashicReader() / AkashicArtifactReader() が nil を返すことを確認する。
// これにより既存の `agent.New(provider, providers)` 呼び出し (cmd/rilll 以外の callsite) で
// 後方互換が維持されることを保証する。
func TestAkashicReaderReturnsNilWhenRecorderOmitted(t *testing.T) {
	claude := &stubProvider{id: provider.ProviderClaude}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	})

	if got := agentClient.AkashicReader(); got != nil {
		t.Fatalf("AkashicReader() = %v, want nil when recorder omitted", got)
	}
	if got := agentClient.AkashicArtifactReader(); got != nil {
		t.Fatalf("AkashicArtifactReader() = %v, want nil when recorder omitted", got)
	}
}

// --- memory-distiller-trigger-policy Task 4.2: agent.CallAgent 入口で
// call スコープ atomic を生成し、UI に foreground register する seam ---
//
// 設計規約 (design.md §UI seam closing / §recordAgentCallFinish extension Implementation Notes,
// Req 3.1 / 3.2):
//   - CallAgent 入口で call ごとに新しい *atomic.Value (callRef) を生成する。
//   - bridge.WithCancelOrigin(ctx, callRef) で ctx に attach し、bridge.CancelOriginFromContext
//     から読み取れるようにする (ctx attach は activeCancelOrigin Pointer の有無に依らず
//     常に行う / launcher が result event の origin を Store する経路の前提)。
//   - SetActiveCancelOrigin で UI の Pointer が注入されている場合のみ、
//     activeCancelOrigin.Store(callRef) で foreground register し、
//     defer activeCancelOrigin.CompareAndSwap(callRef, nil) で unregister する。
//     (LIFO 安全のため、CAS で「自分の ref」のみクリアする。)
//   - SetActiveCancelOrigin 未呼び出し / nil 注入 / 旧 callsite (CLI 経路) は
//     register をスキップして既存挙動を保全する (= subagent dispatch / 並列 evaluator
//     経路で背景 call が UI register をスキップする契約)。

// observingProvider は CallAgent の provider.Call が走っている瞬間の ctx と
// 外側 state (activeCancelOrigin Pointer など) を観測するためのテスト用 stub。
// stubProvider が ctx を受け取らないため、Task 4.2 の「中間時点」観測のために導入する。
type observingProvider struct {
	id     provider.ProviderID
	onCall func(ctx context.Context)
}

func (o *observingProvider) ID() provider.ProviderID { return o.id }

func (o *observingProvider) Call(ctx context.Context, _ provider.AgentRequest, _ provider.AgentEventCallback) (provider.AgentResponse, error) {
	if o.onCall != nil {
		o.onCall(ctx)
	}
	return provider.AgentResponse{Content: "ok", Success: true}, nil
}

// TestCallAgentRegistersActiveCancelOriginDuringCallAndUnregistersOnReturn は Task 4.2 の
// 観測可能な完了条件を verify する:
//   - CallAgent 中は activeCancelOrigin.Load() が当該 call の *atomic.Value を指す
//   - ctx には bridge.WithCancelOrigin で同 ref が attach されている (= bridge.CancelOriginFromContext
//     で同値を読み取れる)
//   - return 後は activeCancelOrigin.Load() が nil (= CAS で nil 化)
func TestCallAgentRegistersActiveCancelOriginDuringCallAndUnregistersOnReturn(t *testing.T) {
	activeCancelOrigin := &atomic.Pointer[atomic.Value]{}

	var refDuringCall *atomic.Value
	var ctxOriginAfterUserStore string

	obs := &observingProvider{
		id: provider.ProviderClaude,
		onCall: func(ctx context.Context) {
			refDuringCall = activeCancelOrigin.Load()
			// ctx attach の単方向経路を verify: provider 内で ref に Store した値が
			// CancelOriginFromContext で同値として読み取れることを確認する。
			if refDuringCall != nil {
				refDuringCall.Store(bridge.CancelOriginUser)
				ctxOriginAfterUserStore = bridge.CancelOriginFromContext(ctx)
			}
		},
	}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: obs,
	})
	agentClient.SetActiveCancelOrigin(activeCancelOrigin)

	_, err := agentClient.CallAgent(context.Background(), "p", "/repo", nil, "", nil)
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}

	if refDuringCall == nil {
		t.Fatal("activeCancelOrigin.Load() should be non-nil during CallAgent")
	}
	if ctxOriginAfterUserStore != bridge.CancelOriginUser {
		t.Fatalf("bridge.CancelOriginFromContext should observe Store(%q), got %q",
			bridge.CancelOriginUser, ctxOriginAfterUserStore)
	}
	if loaded := activeCancelOrigin.Load(); loaded != nil {
		t.Fatalf("activeCancelOrigin should be unregistered (nil) after return, got %v", loaded)
	}
}

// TestCallAgentWithoutActiveCancelOriginPointerDoesNotRegister は SetActiveCancelOrigin
// 未呼び出しの agent (= CLI 経路 / 旧テスト互換 / 既存 callsite) で CallAgent が
// panic / nil deref せず、既存挙動を保全することを verify する。これは
// 「subagent dispatch / 並列 evaluator 経路で背景 call が UI register をスキップする契約」
// の最小ガード (Pointer 未注入 = register skip)。
func TestCallAgentWithoutActiveCancelOriginPointerDoesNotRegister(t *testing.T) {
	claude := &stubProvider{id: provider.ProviderClaude}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	})
	// SetActiveCancelOrigin は呼ばない (Pointer は nil のまま)。

	resp, err := agentClient.CallAgent(context.Background(), "p", "/repo", nil, "", nil)
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success, got %#v", resp)
	}
}

// TestCallAgentNilActiveCancelOriginPointerSetExplicitlyDoesNotPanic は
// SetActiveCancelOrigin(nil) が明示的に呼ばれた場合でも CallAgent が panic しないことを
// verify する (defense-in-depth: 上位呼び出し側のミス耐性)。
func TestCallAgentNilActiveCancelOriginPointerSetExplicitlyDoesNotPanic(t *testing.T) {
	claude := &stubProvider{id: provider.ProviderClaude}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	})
	agentClient.SetActiveCancelOrigin(nil)

	if _, err := agentClient.CallAgent(context.Background(), "p", "/repo", nil, "", nil); err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
}

// TestCallAgentUnregistersActiveCancelOriginOnEarlyReturn は ProviderOverride 解決失敗
// (= ErrUnknownProvider) で provider.Call に到達しない経路でも defer によって
// activeCancelOrigin が nil 化することを verify する (Task 4.2 の defer 確実性)。
func TestCallAgentUnregistersActiveCancelOriginOnEarlyReturn(t *testing.T) {
	activeCancelOrigin := &atomic.Pointer[atomic.Value]{}

	agentClient := New(provider.ProviderID("missing"), map[provider.ProviderID]provider.AgentProvider{})
	agentClient.SetActiveCancelOrigin(activeCancelOrigin)

	_, err := agentClient.CallAgent(context.Background(), "p", "/repo", nil, "", nil)
	if err == nil {
		t.Fatal("CallAgent should return error for missing provider")
	}
	if !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("error should wrap ErrUnknownProvider, got %v", err)
	}
	if loaded := activeCancelOrigin.Load(); loaded != nil {
		t.Fatalf("activeCancelOrigin should be unregistered after early return, got %v", loaded)
	}
}

// TestCallAgentAttachesCancelOriginRefToContextEvenWithoutUIPointer は
// activeCancelOrigin Pointer が未注入の場合でも、CallAgent が ctx に call スコープ
// *atomic.Value を attach することを verify する。これにより launcher (task 2.6) が
// result event の origin を ctx atomic に Store する経路は UI 不在環境
// (CLI / test) でも動作する不変式を担保する。
func TestCallAgentAttachesCancelOriginRefToContextEvenWithoutUIPointer(t *testing.T) {
	var observedOrigin string
	obs := &observingProvider{
		id: provider.ProviderClaude,
		onCall: func(ctx context.Context) {
			// 別 goroutine が ctx 経由で origin を Store する経路を擬似再現する。
			bridge.StoreCancelOrigin(ctx, bridge.CancelOriginEngine)
			observedOrigin = bridge.CancelOriginFromContext(ctx)
		},
	}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: obs,
	})
	// SetActiveCancelOrigin 未注入 = UI 不在 (CLI 経路)。

	if _, err := agentClient.CallAgent(context.Background(), "p", "/repo", nil, "", nil); err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if observedOrigin != bridge.CancelOriginEngine {
		t.Fatalf("ctx atomic should be attached even without UI Pointer; observed origin=%q want %q",
			observedOrigin, bridge.CancelOriginEngine)
	}
}

// --- memory-distiller-trigger-policy Task 4.3: recordAgentCallFinish が
// ctx atomic origin を読んで RecordUserCorrection を呼ぶ seam ---
//
// 設計規約 (design.md §recordAgentCallFinish extension, Req 3.1 / 3.3 / 3.4):
//   - recordAgentCallFinish の **既存 critical section 内** で
//     bridge.CancelOriginFromContext(ctx) を取り、`== "user"` のとき
//     recorder.RecordUserCorrection(ctx, agentCallID) を呼ぶ。
//   - origin と failure_category は直交 (classifyFailure は不変)。
//   - 重複呼び出しは Recorder 側の冪等 contract に委譲 (UPDATE SET 1 のみ)。
//   - error は best-effort に握りつぶす (既存 recorder error 取り扱いと同形)。
//
// 4 ケース (a/b/c/d) を origin の closed enum に対して網羅する:
//   (a) ctx atomic に "user" Store + 通常 finish     → 呼ばれる
//   (b) ctx atomic に "engine" Store + 通常 finish   → 呼ばれない
//   (c) ctx atomic 未 attach (= 空文字 / WithCancelOrigin スキップ) → 呼ばれない
//   (d) "user" Store + 重複 finish                   → 冪等 (毎回呼ばれてよいが no-error)

// userCorrectingProvider は provider.Call が走っている瞬間の ctx に対して
// 任意の origin を Store するためのテスト用 stub。recordAgentCallFinish が
// p.Call の return 後に呼ばれる順序に従い、ここで Store した origin が
// recordAgentCallFinish 側の bridge.CancelOriginFromContext で観測されることを期待する。
type userCorrectingProvider struct {
	id     provider.ProviderID
	origin string
}

func (u *userCorrectingProvider) ID() provider.ProviderID { return u.id }

func (u *userCorrectingProvider) Call(ctx context.Context, _ provider.AgentRequest, _ provider.AgentEventCallback) (provider.AgentResponse, error) {
	// nodebridge launcher (task 2.6) が result event の origin を ctx atomic に
	// Store する経路を擬似再現する。空文字なら Store しない (= "ctx 未 attach に近い"
	// 振る舞いを Store しないことで表現)。
	if u.origin != "" {
		bridge.StoreCancelOrigin(ctx, u.origin)
	}
	return provider.AgentResponse{Content: "ok", Success: true}, nil
}

// TestRecordAgentCallFinishCallsRecordUserCorrectionOnUserOrigin: ケース (a)
// ctx atomic に "user" Store された finish 経路で RecordUserCorrection が
// 当該 agent_call_id 引数で呼ばれることを verify する。
func TestRecordAgentCallFinishCallsRecordUserCorrectionOnUserOrigin(t *testing.T) {
	recorder := &recordingRecorder{}
	prov := &userCorrectingProvider{id: provider.ProviderClaude, origin: bridge.CancelOriginUser}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: prov,
	}, recorder)

	if _, err := agentClient.CallAgent(context.Background(), "p", "/repo", nil, "", nil); err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}

	if len(recorder.userCorrections) != 1 {
		t.Fatalf("RecordUserCorrection should be called once for origin=user, got history=%v", recorder.userCorrections)
	}
	// agent_call_id は RecordAgentCallStart で記録された ID と同一であることを検証する。
	if len(recorder.starts) != 1 {
		t.Fatalf("expected exactly one agent_calls start, got %d", len(recorder.starts))
	}
	if got, want := recorder.userCorrections[0], recorder.starts[0].ID; got != want {
		t.Fatalf("RecordUserCorrection agentCallID mismatch: got=%q want=%q", got, want)
	}
}

// TestRecordAgentCallFinishSkipsRecordUserCorrectionOnEngineOrigin: ケース (b)
// ctx atomic に "engine" Store された finish 経路で RecordUserCorrection が
// 呼ばれないことを verify する (engine 起源 cancel / 正常終了は user-correction に該当しない)。
func TestRecordAgentCallFinishSkipsRecordUserCorrectionOnEngineOrigin(t *testing.T) {
	recorder := &recordingRecorder{}
	prov := &userCorrectingProvider{id: provider.ProviderClaude, origin: bridge.CancelOriginEngine}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: prov,
	}, recorder)

	if _, err := agentClient.CallAgent(context.Background(), "p", "/repo", nil, "", nil); err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}

	if len(recorder.userCorrections) != 0 {
		t.Fatalf("RecordUserCorrection must not be called for origin=engine, got history=%v", recorder.userCorrections)
	}
}

// TestRecordAgentCallFinishSkipsRecordUserCorrectionWhenOriginUnset: ケース (c)
// ctx atomic に何も Store されない (= 空文字 / 通常終了) finish 経路で
// RecordUserCorrection が呼ばれないことを verify する。
// CallAgent 入口の WithCancelOrigin で ctx attach は行われるが、
// atomic.Value への Store がないため CancelOriginFromContext は空文字を返す。
func TestRecordAgentCallFinishSkipsRecordUserCorrectionWhenOriginUnset(t *testing.T) {
	recorder := &recordingRecorder{}
	prov := &userCorrectingProvider{id: provider.ProviderClaude, origin: ""} // Store しない
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: prov,
	}, recorder)

	if _, err := agentClient.CallAgent(context.Background(), "p", "/repo", nil, "", nil); err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}

	if len(recorder.userCorrections) != 0 {
		t.Fatalf("RecordUserCorrection must not be called when origin is unset, got history=%v", recorder.userCorrections)
	}
}

// TestRecordAgentCallFinishIsIdempotentOnDuplicateUserOrigin: ケース (d)
// 同 agent_call_id に対して "user" origin で recordAgentCallFinish が
// 重複呼び出しされたとき、(冪等の責務は Recorder 側の UPDATE SET 1 だが)
// agent 層が panic / error 返却せず連続呼出しを許容することを verify する。
//
// recordAgentCallFinish は private method のため、CallAgent を経由した
// 経路で同 ID の finish 記録 + RecordUserCorrection が 2 回観測できれば、
// 重複時の挙動が安定していると確認できる (履歴 length=2、両方が同 ID)。
func TestRecordAgentCallFinishIsIdempotentOnDuplicateUserOrigin(t *testing.T) {
	recorder := &recordingRecorder{}
	prov := &userCorrectingProvider{id: provider.ProviderClaude, origin: bridge.CancelOriginUser}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: prov,
	}, recorder)

	// 重複 finish の擬似再現として CallAgent を 2 回呼ぶ。各 call は別 agent_call_id を
	// 持つが、テストで観測したいのは「同 origin=user で続けて呼ばれても error / panic
	// しない」ことなので、用途として整合する。
	if _, err := agentClient.CallAgent(context.Background(), "p", "/repo", nil, "", nil); err != nil {
		t.Fatalf("CallAgent #1 returned error: %v", err)
	}
	if _, err := agentClient.CallAgent(context.Background(), "p", "/repo", nil, "", nil); err != nil {
		t.Fatalf("CallAgent #2 returned error: %v", err)
	}

	if len(recorder.userCorrections) != 2 {
		t.Fatalf("RecordUserCorrection should be called once per user-origin call (idempotent at recorder layer), got history=%v", recorder.userCorrections)
	}
	if len(recorder.starts) != 2 {
		t.Fatalf("expected 2 agent_calls starts, got %d", len(recorder.starts))
	}
	// 各 RecordUserCorrection 呼び出しが対応する start の ID と一致すること。
	for i := range recorder.userCorrections {
		if recorder.userCorrections[i] != recorder.starts[i].ID {
			t.Fatalf("RecordUserCorrection[%d] agentCallID mismatch: got=%q want=%q",
				i, recorder.userCorrections[i], recorder.starts[i].ID)
		}
	}
}

// TestRecordAgentCallFinishSwallowsRecorderErrorOnUserCorrection は
// RecordUserCorrection が error を返した場合でも CallAgent が成功 path を
// 維持し、上位呼び出しを阻害しないことを verify する (best-effort 規約)。
func TestRecordAgentCallFinishSwallowsRecorderErrorOnUserCorrection(t *testing.T) {
	recorder := &recordingRecorder{fail: true}
	prov := &userCorrectingProvider{id: provider.ProviderClaude, origin: bridge.CancelOriginUser}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: prov,
	}, recorder)

	resp, err := agentClient.CallAgent(context.Background(), "p", "/repo", nil, "", nil)
	if err != nil {
		t.Fatalf("CallAgent should swallow recorder errors, got %v", err)
	}
	if !resp.Success {
		t.Fatalf("response should remain success even if RecordUserCorrection errors: %#v", resp)
	}
	// fail=true でも履歴は append されるため (recordingRecorder の実装上)、
	// 「呼ばれた」事実は同様に観測できる。
	if len(recorder.userCorrections) != 1 {
		t.Fatalf("RecordUserCorrection should still be invoked (and its error swallowed), got history=%v", recorder.userCorrections)
	}
}

// --- memory-distiller-trigger-policy Task 6.1: 並列 in-flight 不変条件 (Issue 2 closing gate) ---
//
// 設計規約 (design.md §Concurrency Tests / §UI seam closing, Req 3.1 / 3.4):
//   - call スコープ atomic は「1 cancel request → 1 agent_call」の物理 1:1 対応で
//     並列 in-flight 時の origin 混線を構造的に防ぐ。
//   - foreground call (= 最も最近 register された ref) のみが ctrl+c で "user" 化される。
//   - background call (UI register をスキップする経路 / Pointer 未注入) は
//     ctrl+c で touch されない (Req 3.4 「subjective inference を使わない」原則と整合)。
//   - CompareAndSwap unregister は LIFO 違反シナリオでも壊れない (自分の ref のみクリア)。
//
// これら 4 シナリオは「UI 全域 atomic 採用 (棄却案) への退行」を機械的に検出する gate。
// 全シナリオで ctrl+c の simulation は UI Model.handleKey の ctrl+c branch
// (internal/ui/model.go) と同一の 2 ステップ動作 (Load → Store) を再現する:
//
//	if ref := activeCancelOrigin.Load(); ref != nil {
//	    ref.Store(bridge.CancelOriginUser)
//	}

// TestParallelInflightCallsIsolateCancelOrigin: **並列 in-flight 隔離 test**
//
// 2 つの CallAgent を同時 in-flight にし、Call A → register、Call B → register
// (= Call A は LIFO スタック上に退避、Pointer は refB を指す) の状態で ctrl+c 相当を
// 発火 → foreground 側 ref (refB) のみ "user" になり、background 退避中の refA は
// 空のまま (= mix していない) ことを assert する。
//
// 退行検出シナリオ:
//   - 仮に SetActiveCancelOrigin が UI 全域シングルトンに置き換えられた場合、
//     refA/refB が同 ref を共有して両方 "user" 化する → このテストが破綻して検出する
func TestParallelInflightCallsIsolateCancelOrigin(t *testing.T) {
	activeCancelOrigin := &atomic.Pointer[atomic.Value]{}

	aEntered := make(chan struct{})
	bEntered := make(chan struct{})
	aRelease := make(chan struct{})
	bRelease := make(chan struct{})
	var refA, refB *atomic.Value

	provA := &observingProvider{
		id: provider.ProviderClaude,
		onCall: func(_ context.Context) {
			refA = activeCancelOrigin.Load()
			close(aEntered)
			<-aRelease
		},
	}
	provB := &observingProvider{
		id: provider.ProviderCodex,
		onCall: func(_ context.Context) {
			refB = activeCancelOrigin.Load()
			close(bEntered)
			<-bRelease
		},
	}
	agentA := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: provA,
	})
	agentA.SetActiveCancelOrigin(activeCancelOrigin)
	agentB := New(provider.ProviderCodex, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderCodex: provB,
	})
	agentB.SetActiveCancelOrigin(activeCancelOrigin)

	doneA := make(chan struct{})
	doneB := make(chan struct{})

	go func() {
		defer close(doneA)
		_, _ = agentA.CallAgent(context.Background(), "p", "/repo", nil, "", nil)
	}()
	<-aEntered // A は provider.Call 内で blocking、Pointer = refA

	go func() {
		defer close(doneB)
		_, _ = agentB.CallAgent(context.Background(), "p", "/repo", nil, "", nil)
	}()
	<-bEntered // B は provider.Call 内で blocking、Pointer = refB (refA は退避)

	if refA == nil || refB == nil {
		t.Fatalf("refA/refB should be captured during in-flight: refA=%v refB=%v", refA, refB)
	}
	if refA == refB {
		t.Fatal("refA and refB must be distinct *atomic.Value instances per call (call スコープ atomic 不変条件)")
	}
	if loaded := activeCancelOrigin.Load(); loaded != refB {
		t.Fatalf("activeCancelOrigin should point to refB (most-recently registered), got %v", loaded)
	}

	// ctrl+c 相当の発火 (UI Model handleKey ctrl+c branch と同一)
	if ref := activeCancelOrigin.Load(); ref != nil {
		ref.Store(bridge.CancelOriginUser)
	}

	// Assert: refB のみ "user"、refA は空 (Load() == nil)
	if v, _ := refB.Load().(string); v != bridge.CancelOriginUser {
		t.Fatalf("refB.Load() = %v, want %q (foreground のみ user 化)", refB.Load(), bridge.CancelOriginUser)
	}
	if v := refA.Load(); v != nil {
		t.Fatalf("refA should remain untouched (Load=nil), got %v (背景退避中の call は ctrl+c で touch されない)", v)
	}

	// 順次 release (B → A)
	close(bRelease)
	<-doneB
	close(aRelease)
	<-doneA

	// 終了後 Pointer は nil (B の defer CAS(refB, nil) 成功 → A の defer CAS(refA, nil) は false で no-op)
	if loaded := activeCancelOrigin.Load(); loaded != nil {
		t.Fatalf("activeCancelOrigin should be nil after both returns, got %v", loaded)
	}
}

// TestBackgroundCallSkipsUIRegisterAndDoesNotPolluteOrigin: **background call 非汚染 test**
//
// foreground Call A が register 済みで、subagent dispatch / 並列 evaluator 経路の
// background Call B (= SetActiveCancelOrigin 未注入の Agent) が並列起動した状態で
// ctrl+c を発火 → A の ref のみ "user"、B の ctx atomic は空のまま (B は Pointer に
// 触れず、ctrl+c は B 側 ref に届かない) ことを assert する。
//
// 退行検出シナリオ:
//   - 仮に subagent dispatch path が UI register をスキップしなくなった場合、
//     ctrl+c が background call まで波及して B の ctx atomic が "user" 化する
//     → このテストが破綻して検出する (Req 3.4 整合 / Issue 2 closing 防御)
func TestBackgroundCallSkipsUIRegisterAndDoesNotPolluteOrigin(t *testing.T) {
	activeCancelOrigin := &atomic.Pointer[atomic.Value]{}

	aEntered := make(chan struct{})
	bEntered := make(chan struct{})
	aRelease := make(chan struct{})
	bRelease := make(chan struct{})
	var refA *atomic.Value
	var bCtxOriginAfterCtrlC string

	provA := &observingProvider{
		id: provider.ProviderClaude,
		onCall: func(_ context.Context) {
			refA = activeCancelOrigin.Load()
			close(aEntered)
			<-aRelease
		},
	}
	// background call の onCall は ctrl+c 後に発火する読み取りで、ctx atomic が
	// 空文字のままであることを観測する (Pointer に触れていない不変条件)。
	provB := &observingProvider{
		id: provider.ProviderCodex,
		onCall: func(ctx context.Context) {
			close(bEntered)
			<-bRelease
			bCtxOriginAfterCtrlC = bridge.CancelOriginFromContext(ctx)
		},
	}

	// Agent A: foreground (SetActiveCancelOrigin 注入)
	agentA := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: provA,
	})
	agentA.SetActiveCancelOrigin(activeCancelOrigin)

	// Agent B: background (SetActiveCancelOrigin 未注入 = 「UI register skip 契約」)
	agentB := New(provider.ProviderCodex, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderCodex: provB,
	})

	doneA := make(chan struct{})
	doneB := make(chan struct{})
	go func() {
		defer close(doneA)
		_, _ = agentA.CallAgent(context.Background(), "p", "/repo", nil, "", nil)
	}()
	<-aEntered

	go func() {
		defer close(doneB)
		_, _ = agentB.CallAgent(context.Background(), "p", "/repo", nil, "", nil)
	}()
	<-bEntered

	if refA == nil {
		t.Fatal("refA should be captured during in-flight")
	}
	// background B が register をスキップしているため、Pointer は依然 refA のまま
	if loaded := activeCancelOrigin.Load(); loaded != refA {
		t.Fatalf("activeCancelOrigin should remain refA after background B starts (B must skip register), got %v", loaded)
	}

	// ctrl+c 相当の発火
	if ref := activeCancelOrigin.Load(); ref != nil {
		ref.Store(bridge.CancelOriginUser)
	}

	// foreground A の ref が "user"
	if v, _ := refA.Load().(string); v != bridge.CancelOriginUser {
		t.Fatalf("refA should be %q, got %v", bridge.CancelOriginUser, refA.Load())
	}

	// B を release し、ctx atomic origin を観測 → 空文字 (= Pointer に触れていない証跡)
	close(bRelease)
	<-doneB
	if bCtxOriginAfterCtrlC != "" {
		t.Fatalf("background call B's ctx origin must remain empty (UI register skip 契約), got %q", bCtxOriginAfterCtrlC)
	}

	close(aRelease)
	<-doneA
}

// TestCtrlCAfterCallReturnIsNoOp: **call return 後の no-op test**
//
// register → return 完了後の ctrl+c で `ActiveCancelOrigin.Load() == nil` のため
// no-op (panic / nil deref しない) ことを verify する。これは UI Model handleKey の
// `if m.activeCancelOrigin != nil { if ref := m.activeCancelOrigin.Load(); ref != nil { ... } }`
// 二重 nil ガードが call return 後の状態 (Pointer は非 nil、Load() は nil) で
// 想定どおり既存挙動 (tea.Quit のみ) を維持する不変条件のガード。
//
// 退行検出シナリオ:
//   - 仮に CallAgent の defer CAS が壊れて Pointer に古い ref が残ると、
//     ctrl+c で stale ref に "user" が書かれる → 後段の RecordUserCorrection 経路で
//     誤検出が発生する。このテストは Pointer = nil (= Load() == nil) の不変式を assert することで
//     stale ref 残留を機械的に検出する
func TestCtrlCAfterCallReturnIsNoOp(t *testing.T) {
	activeCancelOrigin := &atomic.Pointer[atomic.Value]{}

	prov := &stubProvider{id: provider.ProviderClaude}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: prov,
	})
	agentClient.SetActiveCancelOrigin(activeCancelOrigin)

	// 1 回 CallAgent を完走させる (return 後 Pointer は CAS で nil に戻る)
	if _, err := agentClient.CallAgent(context.Background(), "p", "/repo", nil, "", nil); err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if loaded := activeCancelOrigin.Load(); loaded != nil {
		t.Fatalf("activeCancelOrigin should be nil after call return, got %v", loaded)
	}

	// ctrl+c 相当の発火 — panic / nil deref しないことを recover で検出する。
	// UI Model handleKey の二重 nil ガード (Pointer != nil + Load() != nil) を再現する。
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ctrl+c after call return must not panic: %v", r)
			}
		}()
		if ref := activeCancelOrigin.Load(); ref != nil {
			ref.Store(bridge.CancelOriginUser)
		}
	}()

	// Pointer は nil のまま不変
	if loaded := activeCancelOrigin.Load(); loaded != nil {
		t.Fatalf("activeCancelOrigin should remain nil after no-op ctrl+c, got %v", loaded)
	}
}

// TestCASUnregisterIsLIFOViolationSafe: **CAS unregister race safety test**
//
// LIFO 違反シナリオ (Call A register → Call B register で Call B が「上に積まれた」
// 状態で、本来 LIFO なら Call B が先に return すべきところ、Call A が先に return する)
// で `CompareAndSwap(refA, nil)` が現 active=refB に対し false で no-op になり、
// Call B の register 状態 (Pointer = refB) が壊れないことを assert する。
//
// 退行検出シナリオ:
//   - 仮に defer が CompareAndSwap でなく無条件 Store(nil) になると、
//     A が先に return した瞬間 Pointer が nil になり、B の register 状態が破壊される。
//     ctrl+c が B に届かなくなる → このテストが破綻して検出する
func TestCASUnregisterIsLIFOViolationSafe(t *testing.T) {
	activeCancelOrigin := &atomic.Pointer[atomic.Value]{}

	aEntered := make(chan struct{})
	bEntered := make(chan struct{})
	aRelease := make(chan struct{})
	bRelease := make(chan struct{})
	var refA, refB *atomic.Value

	provA := &observingProvider{
		id: provider.ProviderClaude,
		onCall: func(_ context.Context) {
			refA = activeCancelOrigin.Load()
			close(aEntered)
			<-aRelease
		},
	}
	provB := &observingProvider{
		id: provider.ProviderCodex,
		onCall: func(_ context.Context) {
			refB = activeCancelOrigin.Load()
			close(bEntered)
			<-bRelease
		},
	}
	agentA := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: provA,
	})
	agentA.SetActiveCancelOrigin(activeCancelOrigin)
	agentB := New(provider.ProviderCodex, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderCodex: provB,
	})
	agentB.SetActiveCancelOrigin(activeCancelOrigin)

	doneA := make(chan struct{})
	doneB := make(chan struct{})

	go func() {
		defer close(doneA)
		_, _ = agentA.CallAgent(context.Background(), "p", "/repo", nil, "", nil)
	}()
	<-aEntered

	go func() {
		defer close(doneB)
		_, _ = agentB.CallAgent(context.Background(), "p", "/repo", nil, "", nil)
	}()
	<-bEntered

	if refA == nil || refB == nil {
		t.Fatalf("refA/refB should be captured: refA=%v refB=%v", refA, refB)
	}
	if loaded := activeCancelOrigin.Load(); loaded != refB {
		t.Fatalf("Pointer should be refB (B on top of stack) before A returns, got %v", loaded)
	}

	// LIFO 違反: A を先に release
	close(aRelease)
	<-doneA

	// A の defer CAS(refA, nil) は現 active=refB に対し false で no-op
	// → Pointer は依然 refB のまま (B の register 状態は壊れない)
	if loaded := activeCancelOrigin.Load(); loaded != refB {
		t.Fatalf("Pointer should remain refB after A's premature return (CAS no-op expected), got %v", loaded)
	}

	// ctrl+c 相当を発火 → B の ref が "user" になる (B は依然 in-flight = foreground)
	if ref := activeCancelOrigin.Load(); ref != nil {
		ref.Store(bridge.CancelOriginUser)
	}
	if v, _ := refB.Load().(string); v != bridge.CancelOriginUser {
		t.Fatalf("refB should be %q after ctrl+c (B is still foreground), got %v", bridge.CancelOriginUser, refB.Load())
	}
	if v := refA.Load(); v != nil {
		t.Fatalf("refA should remain untouched after own return, got %v", v)
	}

	// B を release → defer CAS(refB, nil) が成功し active=nil に戻る
	close(bRelease)
	<-doneB
	if loaded := activeCancelOrigin.Load(); loaded != nil {
		t.Fatalf("Pointer should be nil after B returns (CAS(refB,nil) success), got %v", loaded)
	}
}

// --- previous-response-passing Task 3.1: agent.CallAgent 前置 hook ---
//
// 設計規約 (previous-response-passing design.md §agent / prepend hook):
//   - options.IncludePreviousResponse=true かつ options.PreviousResponseContent!="" のときに限り、
//     CallAgent 入口で prompt を prependPreviousResponse(prompt, content) に置き換える。
//   - hook は recordAgentCallStart 呼び出しより前で実行され、akashic に記録される prompt と
//     provider に送られる prompt が文字単位で同一 (Req 5.1 / 6.1 の構造的不変式)。
//   - opt-out (zero-value) と opt-in + 空 content (no-op) は raw prompt と完全一致 (Req 1.3 / 1.4 / 7.4)。
//   - ResumeSessionID / ProviderOverride 等の既存 seam と併用可 (Req 4.1 / 6.3)。

// TestCallAgentPreviousResponseOptOutIsByteIdentical は
// IncludePreviousResponse=false (zero-value) のとき、provider に届く prompt と
// akashic 記録の prompt が呼び出し側 raw prompt と完全一致することを検証する (Req 1.3, 7.4)。
func TestCallAgentPreviousResponseOptOutIsByteIdentical(t *testing.T) {
	recorder := &recordingRecorder{}
	claude := &stubProvider{id: provider.ProviderClaude}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	}, recorder)

	rawPrompt := "raw plan prompt"
	// zero-value options (IncludePreviousResponse=false, PreviousResponseContent="") を渡す。
	// 既存挙動の保存を確認する経路。
	_, err := agentClient.CallAgent(context.Background(), rawPrompt, "/repo", nil, "", &workflowctx.CallAgentOptions{
		StepID:                  "plan",
		IncludePreviousResponse: false,
		PreviousResponseContent: "should-be-ignored-because-opt-out",
	})
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if claude.request.Prompt != rawPrompt {
		t.Fatalf("opt-out: provider should see raw prompt unchanged, got %q want %q", claude.request.Prompt, rawPrompt)
	}
	if len(recorder.starts) != 1 {
		t.Fatalf("expected exactly one agent_calls start, got %d", len(recorder.starts))
	}
	if got := recorder.starts[0].Prompt; got != rawPrompt {
		t.Fatalf("opt-out: akashic should record raw prompt unchanged, got %q want %q", got, rawPrompt)
	}
}

// TestCallAgentPreviousResponseEmptyContentIsNoOp は IncludePreviousResponse=true でも
// PreviousResponseContent が空文字列のときは no-op に倒れ、provider / akashic の
// prompt が raw prompt と完全一致することを検証する (Req 1.4)。
func TestCallAgentPreviousResponseEmptyContentIsNoOp(t *testing.T) {
	recorder := &recordingRecorder{}
	claude := &stubProvider{id: provider.ProviderClaude}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	}, recorder)

	rawPrompt := "raw plan prompt"
	_, err := agentClient.CallAgent(context.Background(), rawPrompt, "/repo", nil, "", &workflowctx.CallAgentOptions{
		IncludePreviousResponse: true,
		PreviousResponseContent: "",
	})
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if claude.request.Prompt != rawPrompt {
		t.Fatalf("empty content: provider should see raw prompt unchanged, got %q want %q", claude.request.Prompt, rawPrompt)
	}
	if len(recorder.starts) != 1 || recorder.starts[0].Prompt != rawPrompt {
		t.Fatalf("empty content: akashic should record raw prompt unchanged, got %#v", recorder.starts)
	}
}

// TestCallAgentPreviousResponsePrependsCombinedPrompt は opt-in + 非空 content のとき、
// provider.AgentRequest.Prompt と akashic.AgentCall.Prompt が前置済み combined prompt で
// 文字単位に同一になることを検証する (Req 1.2, 5.1, 6.1)。
// akashic auditability の構造的不変式 (recordAgentCallStart 前に prompt 加工が完了している)
// を 1 本のテストで担保する。
func TestCallAgentPreviousResponsePrependsCombinedPrompt(t *testing.T) {
	recorder := &recordingRecorder{}
	claude := &stubProvider{id: provider.ProviderClaude}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	}, recorder)

	rawPrompt := "plan body"
	content := "summary-of-review"
	want := "# Previous Response\n\n" + content + "\n\n---\n\n" + rawPrompt

	_, err := agentClient.CallAgent(context.Background(), rawPrompt, "/repo", nil, "", &workflowctx.CallAgentOptions{
		IncludePreviousResponse: true,
		PreviousResponseContent: content,
	})
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if claude.request.Prompt != want {
		t.Fatalf("provider should receive combined prompt:\n got=%q\nwant=%q", claude.request.Prompt, want)
	}
	if len(recorder.starts) != 1 {
		t.Fatalf("expected exactly one agent_calls start, got %d", len(recorder.starts))
	}
	// akashic 不変式: 記録 prompt == provider 受信 prompt
	if got := recorder.starts[0].Prompt; got != want {
		t.Fatalf("akashic should record combined prompt identical to provider request:\n got=%q\nwant=%q", got, want)
	}
	if recorder.starts[0].Prompt != claude.request.Prompt {
		t.Fatalf("audit invariant violated: akashic prompt != provider request prompt\n akashic=%q\n provider=%q",
			recorder.starts[0].Prompt, claude.request.Prompt)
	}
}

// TestCallAgentPreviousResponseCoexistsWithResumeSessionID は ResumeSessionID と
// IncludePreviousResponse=true を同じ呼び出しで併用したとき、session resume を行った上で
// 前置を行う (二者択一にならない) ことを検証する (Req 4.1)。
func TestCallAgentPreviousResponseCoexistsWithResumeSessionID(t *testing.T) {
	claude := &stubProvider{id: provider.ProviderClaude}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	})

	rawPrompt := "next turn prompt"
	content := "earlier-summary"
	want := "# Previous Response\n\n" + content + "\n\n---\n\n" + rawPrompt

	_, err := agentClient.CallAgent(context.Background(), rawPrompt, "/repo", nil, "sess-1", &workflowctx.CallAgentOptions{
		IncludePreviousResponse: true,
		PreviousResponseContent: content,
	})
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if claude.request.ResumeSessionID != "sess-1" {
		t.Fatalf("ResumeSessionID should be forwarded, got %q", claude.request.ResumeSessionID)
	}
	if claude.request.Prompt != want {
		t.Fatalf("combined prompt should be applied alongside session resume:\n got=%q\nwant=%q", claude.request.Prompt, want)
	}
}

// TestCallAgentPreviousResponseCoexistsWithProviderOverride は ProviderOverride で
// 切り替えた provider 側でも同一の combined prompt が観測されることを検証する (Req 6.3)。
// override 前後で combined prompt が文字単位に同一であることが provider 横断一貫性の核。
func TestCallAgentPreviousResponseCoexistsWithProviderOverride(t *testing.T) {
	claude := &stubProvider{id: provider.ProviderClaude}
	cursor := &stubProvider{id: provider.ProviderCursor}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
		provider.ProviderCursor: cursor,
	})

	rawPrompt := "plan body"
	content := "review summary"
	want := "# Previous Response\n\n" + content + "\n\n---\n\n" + rawPrompt

	_, err := agentClient.CallAgent(context.Background(), rawPrompt, "/repo", nil, "", &workflowctx.CallAgentOptions{
		ProviderOverride:        provider.ProviderCursor,
		IncludePreviousResponse: true,
		PreviousResponseContent: content,
	})
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if claude.called {
		t.Fatalf("ProviderOverride should redirect to cursor, but claude was called")
	}
	if !cursor.called {
		t.Fatalf("ProviderOverride=cursor should be selected, but cursor was not called")
	}
	if cursor.request.Prompt != want {
		t.Fatalf("override-after provider should also receive combined prompt:\n got=%q\nwant=%q", cursor.request.Prompt, want)
	}
}

// --- output-contract-2phase Task 6.1: CallAgent 入口 precheck (validateContracts) ---
//
// 設計規約 (output-contract-2phase design.md §CallAgent entry guard, R1.4 / R1.5):
//   - CallAgentOptions.OutputContracts が 1 件以上指定されているときに限り、
//     CallAgent 入口で validateContracts(opts.OutputContracts) を呼ぶ。
//   - precheck error 時は provider.Call も recordAgentCallStart も呼ばず、
//     *CallError{Category: FailureCategoryInvalidOptions, Err: <returned err>} を即 return する
//     (Phase 1 / Phase 2 のいずれも実行しない構造的ガード)。
//   - OutputContracts 未指定 (nil / 空 slice) の経路では validateContracts を呼ばず、
//     legacy path と完全に同一挙動を維持する。

// TestCallAgentOutputContractsDuplicateNamePrecheckRejects は同一呼び出し内で OutputContract.Name
// が重複しているとき、CallAgent が CallError{Category: FailureCategoryInvalidOptions} を返し、
// provider.Call が 0 回 / akashic agent_calls.Start が 0 件であることを検証する (R1.4)。
func TestCallAgentOutputContractsDuplicateNamePrecheckRejects(t *testing.T) {
	recorder := &recordingRecorder{}
	claude := &stubProvider{id: provider.ProviderClaude}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	}, recorder)

	opts := &workflowctx.CallAgentOptions{
		StepID: "plan",
		OutputContracts: []workflowctx.OutputContract{
			{Name: "report", FormatFacet: "plan"},
			{Name: "report", FormatFacet: "review-gather"}, // Name 重複
		},
	}

	resp, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", opts)
	if err == nil {
		t.Fatalf("expected CallError, got nil (resp=%#v)", resp)
	}
	if !errors.Is(err, ErrDuplicateContractName) {
		t.Fatalf("expected error to wrap ErrDuplicateContractName, got %v", err)
	}
	if got := CategoryFromError(err); got != FailureCategoryInvalidOptions {
		t.Fatalf("expected category %q, got %q", FailureCategoryInvalidOptions, got)
	}
	if claude.called {
		t.Fatalf("provider.Call must not be invoked on precheck failure")
	}
	if len(recorder.starts) != 0 {
		t.Fatalf("recordAgentCallStart must not be invoked on precheck failure, got %d", len(recorder.starts))
	}
	if len(recorder.finishes) != 0 {
		t.Fatalf("recordAgentCallFinish must not be invoked on precheck failure, got %d", len(recorder.finishes))
	}
}

// TestCallAgentOutputContractsEmptyFormatFacetPrecheckRejects は FormatFacet が空文字列のとき
// CallAgent が CallError{Category: FailureCategoryInvalidOptions} を返し、副作用ゼロで終わる
// ことを検証する (R1.5)。
func TestCallAgentOutputContractsEmptyFormatFacetPrecheckRejects(t *testing.T) {
	recorder := &recordingRecorder{}
	claude := &stubProvider{id: provider.ProviderClaude}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	}, recorder)

	opts := &workflowctx.CallAgentOptions{
		StepID: "plan",
		OutputContracts: []workflowctx.OutputContract{
			{Name: "report", FormatFacet: ""}, // FormatFacet 空文字
		},
	}

	resp, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", opts)
	if err == nil {
		t.Fatalf("expected CallError, got nil (resp=%#v)", resp)
	}
	if !errors.Is(err, ErrEmptyFormatFacet) {
		t.Fatalf("expected error to wrap ErrEmptyFormatFacet, got %v", err)
	}
	if got := CategoryFromError(err); got != FailureCategoryInvalidOptions {
		t.Fatalf("expected category %q, got %q", FailureCategoryInvalidOptions, got)
	}
	if claude.called {
		t.Fatalf("provider.Call must not be invoked on precheck failure")
	}
	if len(recorder.starts) != 0 {
		t.Fatalf("recordAgentCallStart must not be invoked on precheck failure, got %d", len(recorder.starts))
	}
	if len(recorder.finishes) != 0 {
		t.Fatalf("recordAgentCallFinish must not be invoked on precheck failure, got %d", len(recorder.finishes))
	}
}

// TestCallAgentOutputContractsZeroValueSkipsPrecheck は OutputContracts 未指定 (nil) のときに
// precheck を skip して legacy path (従来挙動) と完全に一致することを検証する (R1.3)。
// 「precheck をスキップ」の観察は、空文字 FormatFacet を slice ではなく nil で渡しても error が
// 発生しないこと + provider.Call と recordAgentCallStart が通常通り 1 回呼ばれることで担保する。
func TestCallAgentOutputContractsZeroValueSkipsPrecheck(t *testing.T) {
	recorder := &recordingRecorder{}
	claude := &stubProvider{id: provider.ProviderClaude}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	}, recorder)

	// OutputContracts を明示的に nil にした options (= zero-value)
	opts := &workflowctx.CallAgentOptions{StepID: "plan", OutputContracts: nil}
	resp, err := agentClient.CallAgent(context.Background(), "prompt", "/repo", nil, "", opts)
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}
	if !resp.Success {
		t.Fatalf("legacy path should succeed: %#v", resp)
	}
	if !claude.called {
		t.Fatalf("provider.Call must be invoked when OutputContracts is empty (legacy path)")
	}
	if len(recorder.starts) != 1 {
		t.Fatalf("recordAgentCallStart should be invoked exactly once on legacy path, got %d", len(recorder.starts))
	}
	if len(recorder.finishes) != 1 {
		t.Fatalf("recordAgentCallFinish should be invoked exactly once on legacy path, got %d", len(recorder.finishes))
	}

	// 空 slice (len==0) でも同様に precheck skip → legacy path を辿ることを確認する。
	recorder2 := &recordingRecorder{}
	claude2 := &stubProvider{id: provider.ProviderClaude}
	agentClient2 := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude2,
	}, recorder2)
	opts2 := &workflowctx.CallAgentOptions{StepID: "plan", OutputContracts: []workflowctx.OutputContract{}}
	resp2, err2 := agentClient2.CallAgent(context.Background(), "prompt", "/repo", nil, "", opts2)
	if err2 != nil {
		t.Fatalf("CallAgent returned error (empty slice): %v", err2)
	}
	if !resp2.Success {
		t.Fatalf("legacy path should succeed with empty slice: %#v", resp2)
	}
	if !claude2.called {
		t.Fatalf("provider.Call must be invoked when OutputContracts is empty slice (legacy path)")
	}
	if len(recorder2.starts) != 1 {
		t.Fatalf("recordAgentCallStart should be invoked exactly once on legacy path (empty slice), got %d", len(recorder2.starts))
	}
}

// --- output-contract-2phase Task 6.2: CallAgent に Phase 2 hook (runReportPhase) を統合 ---
//
// 設計規約 (output-contract-2phase design.md §CallAgent entry guard + Phase 1→Phase 2 シーケンス,
// R1.3 / R2.1 / R2.2 / R2.3 / R2.5 / R5.2 / R5.3):
//   - Phase 1 success 後 (recordAgentCallFinish の後) に len(opts.OutputContracts) > 0 のガード下で
//     runReportPhase を呼び、戻り値 []OutputContractResult を resp.OutputContractResults に詰める。
//   - Phase 1 失敗時 (CallError) は runReportPhase を呼ばず Phase 1 のエラーをそのまま return する
//     (R2.3)。このとき resp.OutputContractResults は nil のまま。
//   - OutputContracts 未指定時は Phase 2 hook を skip し、resp.OutputContractResults を nil のまま保ち
//     legacy path と完全に同一挙動を維持する (R1.3 後方互換ガード)。
//   - 一部 contract のみ Phase 2 で失敗するときは、成功分は ReportPath あり、失敗分は Warning ありで
//     同 slice に並ぶ (R5.3 per-contract isolation)。
//   - 既存 field (resp.Content / resp.SessionID / resp.Metrics / resp.SystemInfo) は Phase 2 hook に
//     よって一切変更されない (R2.5 既存応答テキスト不変)。

// TestCallAgentOutputContractsPhase1FailureSkipsPhase2 は Phase 1 が provider エラーで失敗したとき、
// runReportPhase が呼ばれず resp.OutputContractResults が nil のまま帰ることを検証する (R2.3)。
// erroringProvider は Call() で必ず error を返すため、scriptedProvider が必要な複数 Call 経路に
// 到達しないこと自体が「Phase 2 未実行」の構造的証拠となる。
func TestCallAgentOutputContractsPhase1FailureSkipsPhase2(t *testing.T) {
	recorder := &recordingRecorder{}
	wantErr := errors.New("phase1 provider failure")
	claude := &erroringProvider{id: provider.ProviderClaude, err: wantErr}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: claude,
	}, recorder)

	// RunMetadata を ctx に注入しないと runReportPhase 内部の reportDir 算出は失敗するが、
	// Phase 1 が失敗する以上 runReportPhase 自体が呼ばれない (= ctx 注入の有無は影響しない)。
	ctx := workflowctx.ContextWithRunMetadata(context.Background(), workflowctx.RunMetadata{
		WorkflowRunID: "run-phase1-fail",
		WorkflowID:    "review-workflow",
		WorkflowName:  "review",
	})

	opts := &workflowctx.CallAgentOptions{
		StepID: "review-step",
		OutputContracts: []workflowctx.OutputContract{
			{Name: "plan", FormatFacet: "plan"},
		},
	}

	resp, err := agentClient.CallAgent(ctx, "prompt", t.TempDir(), nil, "", opts)
	if err == nil {
		t.Fatalf("Phase 1 failure 経路で err=nil が返った (resp=%#v)", resp)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected error to wrap wantErr, got %v", err)
	}
	// Phase 2 hook は走らないので OutputContractResults は nil のまま (R2.3)
	if resp.OutputContractResults != nil {
		t.Fatalf("Phase 1 失敗時に resp.OutputContractResults が nil でない: %#v", resp.OutputContractResults)
	}
	// akashic agent_calls は Phase 1 の 1 行 (失敗) のみ
	if len(recorder.starts) != 1 || len(recorder.finishes) != 1 {
		t.Fatalf("akashic 行数 mismatch: starts=%d finishes=%d (期待 1/1 = Phase 1 のみ)", len(recorder.starts), len(recorder.finishes))
	}
	if recorder.finishes[0].Success {
		t.Fatalf("Phase 1 finish Success = true (期待 false / provider エラー)")
	}
}

// TestCallAgentOutputContractsPhase1SuccessRunsPhase2OneContract は Phase 1 成功 + 1 contract Phase 2
// 成功のとき、resp.OutputContractResults に 1 件 (Status=success / ReportPath が絶対パス) が並び、
// Phase 1 の Content / SessionID / Metrics が一切変更されないことを検証する (R2.2 / R2.5)。
//
// scriptedProvider に 2 つの scripts を渡し、index 0=Phase 1, index 1=Phase 2 として振る舞わせる。
// Phase 2 で書き出された report ファイルの本文も assert する。
func TestCallAgentOutputContractsPhase1SuccessRunsPhase2OneContract(t *testing.T) {
	recorder := &recordingRecorder{}
	phase1Content := "phase 1 response text"
	phase1SessionID := "phase1-session-001"
	phase1CostUSD := 0.0123
	phase1InputTokens := int64(1234)
	sp := &scriptedProvider{
		id: provider.ProviderClaude,
		scripts: []scriptedResponse{
			// index 0: Phase 1 success
			{resp: provider.AgentResponse{
				Content:   phase1Content,
				SessionID: phase1SessionID,
				Success:   true,
				Metrics: provider.AgentMetrics{
					TotalCostUSD: &phase1CostUSD,
					InputTokens:  &phase1InputTokens,
				},
			}},
			// index 1: Phase 2 success (report body)
			{resp: provider.AgentResponse{
				Content:   "phase 2 plan report body\n",
				SessionID: "phase2-session-001",
				Success:   true,
			}},
		},
	}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: sp,
	}, recorder)

	cwd := t.TempDir()
	workflowID := "review-workflow"
	runID := "run-phase2-success-1"
	ctx := workflowctx.ContextWithRunMetadata(context.Background(), workflowctx.RunMetadata{
		WorkflowRunID: runID,
		WorkflowID:    workflowID,
		WorkflowName:  "review",
	})

	opts := &workflowctx.CallAgentOptions{
		StepID: "review-step",
		OutputContracts: []workflowctx.OutputContract{
			{Name: "plan", FormatFacet: "plan"},
		},
	}

	resp, err := agentClient.CallAgent(ctx, "prompt", cwd, nil, "", opts)
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}

	// Phase 1 応答テキスト / SessionID / Metrics は不変 (R2.5)
	if resp.Content != phase1Content {
		t.Fatalf("resp.Content = %q, want %q (Phase 2 hook は Content を変更してはならない)", resp.Content, phase1Content)
	}
	if resp.SessionID != phase1SessionID {
		t.Fatalf("resp.SessionID = %q, want %q (Phase 2 hook は SessionID を変更してはならない)", resp.SessionID, phase1SessionID)
	}
	if resp.Metrics.TotalCostUSD == nil || *resp.Metrics.TotalCostUSD != phase1CostUSD {
		t.Fatalf("resp.Metrics.TotalCostUSD mismatch: got=%v want=%v", resp.Metrics.TotalCostUSD, phase1CostUSD)
	}
	if resp.Metrics.InputTokens == nil || *resp.Metrics.InputTokens != phase1InputTokens {
		t.Fatalf("resp.Metrics.InputTokens mismatch: got=%v want=%d", resp.Metrics.InputTokens, phase1InputTokens)
	}

	// OutputContractResults は 1 件、Status=success、Mode=native (claude)、ReportPath は絶対パス
	if len(resp.OutputContractResults) != 1 {
		t.Fatalf("resp.OutputContractResults 件数 = %d (期待 1)", len(resp.OutputContractResults))
	}
	r := resp.OutputContractResults[0]
	if r.Name != "plan" {
		t.Fatalf("OutputContractResults[0].Name = %q, want %q", r.Name, "plan")
	}
	if r.Status != "success" {
		t.Fatalf("OutputContractResults[0].Status = %q, want success (warning=%q)", r.Status, r.Warning)
	}
	if r.Mode != "native" {
		t.Fatalf("OutputContractResults[0].Mode = %q, want native (claude=native)", r.Mode)
	}
	if r.ReportPath == "" || !filepath.IsAbs(r.ReportPath) {
		t.Fatalf("OutputContractResults[0].ReportPath は絶対パスである必要がある (got %q)", r.ReportPath)
	}
	if r.RetryUsed {
		t.Fatalf("OutputContractResults[0].RetryUsed = true (期待 false)")
	}

	// 書き出された report ファイルが Phase 2 response.Content と一致
	body, err := os.ReadFile(r.ReportPath)
	if err != nil {
		t.Fatalf("report file ReadFile failed: %v", err)
	}
	if string(body) != "phase 2 plan report body\n" {
		t.Fatalf("report body mismatch: got=%q want=%q", string(body), "phase 2 plan report body\n")
	}

	// scriptedProvider は 2 回呼ばれる (Phase 1 + Phase 2)
	if len(sp.calls) != 2 {
		t.Fatalf("provider.Call 呼び出し回数 = %d (期待 2 = Phase 1 + Phase 2)", len(sp.calls))
	}
	// Phase 2 SDK 呼び出しの metadata 検証 (Phase 1 の継承 + step_id suffix / ResumeSessionID)
	phase2Req := sp.calls[1]
	if phase2Req.ResumeSessionID != phase1SessionID {
		t.Fatalf("Phase 2 ResumeSessionID = %q, want %q (Phase 1 SessionID を継続)", phase2Req.ResumeSessionID, phase1SessionID)
	}
	if len(phase2Req.AllowedTools) != 1 || phase2Req.AllowedTools[0] != "Write" {
		t.Fatalf("Phase 2 AllowedTools = %#v, want []string{\"Write\"}", phase2Req.AllowedTools)
	}
	wantStepID := "review-step:phase2:plan"
	if phase2Req.Metadata.StepID != wantStepID {
		t.Fatalf("Phase 2 Metadata.StepID = %q, want %q", phase2Req.Metadata.StepID, wantStepID)
	}

	// akashic agent_calls は 2 行 (Phase 1 + Phase 2 main)
	if len(recorder.starts) != 2 || len(recorder.finishes) != 2 {
		t.Fatalf("akashic 行数 mismatch: starts=%d finishes=%d (期待 2/2 = Phase 1 + Phase 2)", len(recorder.starts), len(recorder.finishes))
	}
	// Phase 1 / Phase 2 のいずれも success
	for i, fin := range recorder.finishes {
		if !fin.Success {
			t.Fatalf("akashic finishes[%d].Success = false (期待 true)", i)
		}
	}
}

// TestCallAgentOutputContractsPhase1SuccessMixedContracts は Phase 1 成功 + 複数 contract で
// 一部成功 / 一部失敗 (facet 不在で fail) のミックス経路を検証する (R5.3 per-contract isolation)。
// scriptedProvider は Phase 1 (1 回) + Phase 2 success (ok contract / 1 回) = 計 2 回呼ばれる。
// 失敗 contract (missing facet) は SDK 起動前に弾かれるので Phase 2 SDK 呼び出しは発生しない。
// Phase 1 の応答 (Content / SessionID) は維持されること、resp.OutputContractResults に 2 件
// 並ぶこと、成功側は ReportPath あり / 失敗側は Warning ありで分離されることを assert する。
func TestCallAgentOutputContractsPhase1SuccessMixedContracts(t *testing.T) {
	recorder := &recordingRecorder{}
	phase1Content := "phase 1 mixed response"
	phase1SessionID := "phase1-session-mixed"
	sp := &scriptedProvider{
		id: provider.ProviderClaude,
		scripts: []scriptedResponse{
			// index 0: Phase 1 success
			{resp: provider.AgentResponse{
				Content:   phase1Content,
				SessionID: phase1SessionID,
				Success:   true,
			}},
			// index 1: Phase 2 success for ok-doc (missing-doc は facet 解決で fail し SDK 未到達)
			{resp: provider.AgentResponse{
				Content:   "ok-doc body\n",
				SessionID: "phase2-ok",
				Success:   true,
			}},
		},
	}
	agentClient := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: sp,
	}, recorder)

	cwd := t.TempDir()
	workflowID := "review-workflow"
	runID := "run-phase2-mixed"
	ctx := workflowctx.ContextWithRunMetadata(context.Background(), workflowctx.RunMetadata{
		WorkflowRunID: runID,
		WorkflowID:    workflowID,
		WorkflowName:  "review",
	})

	opts := &workflowctx.CallAgentOptions{
		StepID: "review-step",
		OutputContracts: []workflowctx.OutputContract{
			{Name: "ok-doc", FormatFacet: "plan"},
			{Name: "missing-doc", FormatFacet: "this-facet-does-not-exist-xyz-9999"},
		},
	}

	resp, err := agentClient.CallAgent(ctx, "prompt", cwd, nil, "", opts)
	if err != nil {
		t.Fatalf("CallAgent returned error: %v", err)
	}

	// Phase 1 応答は維持される (R2.5)
	if resp.Content != phase1Content {
		t.Fatalf("resp.Content = %q, want %q (mixed 経路でも Phase 1 不変)", resp.Content, phase1Content)
	}
	if resp.SessionID != phase1SessionID {
		t.Fatalf("resp.SessionID = %q, want %q (mixed 経路でも Phase 1 不変)", resp.SessionID, phase1SessionID)
	}

	// OutputContractResults は 2 件、宣言順
	if len(resp.OutputContractResults) != 2 {
		t.Fatalf("resp.OutputContractResults 件数 = %d (期待 2)", len(resp.OutputContractResults))
	}
	// [0] = ok-doc success
	ok := resp.OutputContractResults[0]
	if ok.Name != "ok-doc" {
		t.Fatalf("OutputContractResults[0].Name = %q, want ok-doc", ok.Name)
	}
	if ok.Status != "success" {
		t.Fatalf("OutputContractResults[0].Status = %q, want success (warning=%q)", ok.Status, ok.Warning)
	}
	if ok.ReportPath == "" {
		t.Fatalf("OutputContractResults[0].ReportPath が空 (success では非空)")
	}
	if ok.Warning != "" {
		t.Fatalf("OutputContractResults[0].Warning = %q (success では空)", ok.Warning)
	}
	// [1] = missing-doc failed (facet 不在)
	missing := resp.OutputContractResults[1]
	if missing.Name != "missing-doc" {
		t.Fatalf("OutputContractResults[1].Name = %q, want missing-doc", missing.Name)
	}
	if missing.Status != "failed" {
		t.Fatalf("OutputContractResults[1].Status = %q, want failed", missing.Status)
	}
	if missing.Warning == "" {
		t.Fatalf("OutputContractResults[1].Warning が空 (failed では非空 / facet 不在の理由)")
	}
	if missing.ReportPath != "" {
		t.Fatalf("OutputContractResults[1].ReportPath = %q (failed では空)", missing.ReportPath)
	}

	// scriptedProvider は 2 回呼ばれる (Phase 1 + Phase 2 for ok-doc のみ)
	if len(sp.calls) != 2 {
		t.Fatalf("provider.Call 呼び出し回数 = %d (期待 2 = Phase 1 + Phase 2 for ok-doc)", len(sp.calls))
	}
	// akashic 行数 = 2 (Phase 1 + Phase 2 for ok-doc)、missing-doc は SDK 未起動なので行を積まない
	if len(recorder.starts) != 2 || len(recorder.finishes) != 2 {
		t.Fatalf("akashic 行数 mismatch: starts=%d finishes=%d (期待 2/2)", len(recorder.starts), len(recorder.finishes))
	}
}
