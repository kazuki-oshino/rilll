package learnings

import (
	"bytes"
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentpkg "github.com/kazuki-oshino/rilll/internal/agent"
	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/cli"
	"github.com/kazuki-oshino/rilll/internal/config"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/ui"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

func TestEarlyReflectionIntegrationMatrixOptInWrapperEnqueuesJobsThroughAgentHook(t *testing.T) {
	t.Setenv(envRillLearningsProviders, "codex")

	ctx := context.Background()
	cwd := t.TempDir()
	recorder := openEarlyIntegrationRecorder(t)
	codex := &earlyIntegrationProvider{
		id: provider.ProviderCodex,
		responses: map[string]provider.AgentResponse{
			"eligible-parent": {
				Content:   "parent done",
				SessionID: "session-parent",
				Success:   true,
			},
			"empty-session": {
				Content: "empty done",
				Success: true,
			},
			"eligible-child": {
				Content:   "child done",
				SessionID: "session-child",
				Success:   true,
			},
		},
	}
	cursor := &earlyIntegrationProvider{
		id: provider.ProviderCursor,
		responses: map[string]provider.AgentResponse{
			"denied-cursor": {
				Content:   "cursor done",
				SessionID: "session-denied",
				Success:   true,
			},
		},
	}
	agentClient := agentpkg.New(provider.ProviderCodex, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderCodex:  codex,
		provider.ProviderCursor: cursor,
	}, recorder)

	var (
		startCount        atomic.Int32
		frontMarkerSeen   bool
		childMarkerSeen   bool
		tailMarkerSeen    bool
		tailEarlyEnabled  bool
		tailParentRunID   string
		frontRunCompleted bool
	)
	runtime := &earlyIntegrationRuntimeSpy{}
	frontDef := workflow.Definition{
		ID:       "early-front",
		Label:    "Early Front",
		Recorder: recorder,
		Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			policy, ok := workflowctx.EarlyReflectionPolicyFromContext(ctx)
			frontMarkerSeen = ok && policy.AllowsProvider(provider.ProviderCodex) && !policy.AllowsProvider(provider.ProviderClaude)
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "front started"})

			if _, err := agentClient.CallAgent(ctx, "eligible-parent", cwd, nil, "", &workflowctx.CallAgentOptions{
				StepID:   "parent-eligible",
				StepName: "Parent Eligible",
			}); err != nil {
				return err
			}
			if _, err := agentClient.CallAgent(ctx, "denied-cursor", cwd, nil, "", &workflowctx.CallAgentOptions{
				StepID:           "parent-denied",
				StepName:         "Parent Denied",
				ProviderOverride: provider.ProviderCursor,
			}); err != nil {
				return err
			}
			if _, err := agentClient.CallAgent(ctx, "empty-session", cwd, nil, "", &workflowctx.CallAgentOptions{
				StepID:   "parent-empty",
				StepName: "Parent Empty",
			}); err != nil {
				return err
			}

			childDef := workflow.Definition{
				ID:       "early-child",
				Label:    "Early Child",
				Recorder: recorder,
				Fn: func(childCtx context.Context, cwd string, _ func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
					policy, ok := workflowctx.EarlyReflectionPolicyFromContext(childCtx)
					childMarkerSeen = ok && policy.AllowsProvider(provider.ProviderCodex)
					_, err := agentClient.CallAgent(childCtx, "eligible-child", cwd, nil, "", &workflowctx.CallAgentOptions{
						StepID:   "child-eligible",
						StepName: "Child Eligible",
					})
					return err
				},
			}
			if _, err := workflow.RunChild(ctx, cwd, childDef, onEvent, ask, workflow.ChildRunOptions{}); err != nil {
				return err
			}
			frontRunCompleted = true
			return nil
		},
	}
	tailDef := workflow.Definition{
		ID:       "agent-learnings",
		Label:    "Agent Learnings",
		Recorder: recorder,
		Fn: func(ctx context.Context, _ string, _ func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			_, tailMarkerSeen = workflowctx.EarlyReflectionPolicyFromContext(ctx)
			tailEarlyEnabled = EarlyAggregationEnabledFromContext(ctx)
			tailParentRunID = ParentRunIDFromContext(ctx)
			return nil
		},
	}
	deps := sequentialWithLearningsTailDeps{
		agent:    agentClient,
		frontDef: frontDef,
		tailDef:  tailDef,
		startEarlyRuntime: func(context.Context, string, workflowctx.AgentCaller, EarlyReflectorRuntimeConfig) (workflowScopedEarlyReflectorRuntime, bool, error) {
			startCount.Add(1)
			return runtime, true, nil
		},
	}

	if err := runSequentialWithLearningsTailOrchestration(ctx, cwd, nil, nil, deps); err != nil {
		t.Fatalf("runSequentialWithLearningsTailOrchestration: %v", err)
	}
	if startCount.Load() != 1 {
		t.Fatalf("runtime start count = %d, want 1", startCount.Load())
	}
	if runtime.shutdownCount.Load() != 1 {
		t.Fatalf("runtime shutdown count = %d, want 1", runtime.shutdownCount.Load())
	}
	waitUntilRuntimeTest(t, time.Second, func() bool { return runtime.notifyCount.Load() == 4 })
	if !frontMarkerSeen {
		t.Fatalf("front run did not receive codex-only early reflection marker")
	}
	if !childMarkerSeen {
		t.Fatalf("child run did not inherit early reflection marker")
	}
	if tailMarkerSeen {
		t.Fatalf("tail run received enqueue marker")
	}
	if !tailEarlyEnabled {
		t.Fatalf("tail run did not receive early aggregation marker")
	}
	if tailParentRunID == "" {
		t.Fatalf("tail parent run ID is empty")
	}
	if !frontRunCompleted {
		t.Fatalf("front workflow did not continue through skipped early reflection rows")
	}

	jobs, err := recorder.LearningReflectionJobsByWorkflowRunTree(context.Background(), tailParentRunID)
	if err != nil {
		t.Fatalf("LearningReflectionJobsByWorkflowRunTree: %v", err)
	}
	jobsByStep := earlyIntegrationJobsByStep(jobs)
	if len(jobsByStep) != 4 {
		t.Fatalf("jobs by step len = %d, want 4: %+v", len(jobsByStep), jobs)
	}
	assertEarlyIntegrationJob(t, jobsByStep["parent-eligible"], akashic.LearningReflectionStatusPending, "", "session-parent", "codex", tailParentRunID)
	assertEarlyIntegrationJob(t, jobsByStep["parent-denied"], akashic.LearningReflectionStatusSkipped, string(akashic.LearningReflectionSkipProviderNotInAllowlist), "session-denied", "cursor", tailParentRunID)
	assertEarlyIntegrationJob(t, jobsByStep["parent-empty"], akashic.LearningReflectionStatusSkipped, string(akashic.LearningReflectionSkipEmptySessionID), "", "codex", tailParentRunID)

	childJob := jobsByStep["child-eligible"]
	assertEarlyIntegrationJob(t, childJob, akashic.LearningReflectionStatusPending, "", "session-child", "codex", childJob.WorkflowRunID)
	if childJob.WorkflowID != "early-child" {
		t.Fatalf("child job WorkflowID = %q, want early-child", childJob.WorkflowID)
	}
	if childJob.WorkflowRunID == "" || childJob.WorkflowRunID == tailParentRunID {
		t.Fatalf("child job WorkflowRunID = %q, want non-empty child run distinct from parent %q", childJob.WorkflowRunID, tailParentRunID)
	}
}

func TestEarlyReflectionIntegrationMatrixNonOptInAndTailContextsDoNotEnqueue(t *testing.T) {
	t.Run("non opt-in workflow", func(t *testing.T) {
		recorder := openEarlyIntegrationRecorder(t)
		agentClient := newEarlyIntegrationAgent(recorder, map[string]provider.AgentResponse{
			"plain": {Content: "plain done", SessionID: "session-plain", Success: true},
		})
		var runID string
		def := workflow.Definition{
			ID:       "plain-workflow",
			Label:    "Plain Workflow",
			Recorder: recorder,
			Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
				onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "plain started"})
				_, err := agentClient.CallAgent(ctx, "plain", cwd, nil, "", &workflowctx.CallAgentOptions{StepID: "plain-step"})
				return err
			},
		}
		if err := def.Run(context.Background(), t.TempDir(), func(event workflow.WorkflowEvent) {
			if runID == "" {
				runID = event.RunID
			}
		}, nil); err != nil {
			t.Fatalf("plain workflow Run: %v", err)
		}
		jobs, err := recorder.LearningReflectionJobsByWorkflowRunTree(context.Background(), runID)
		if err != nil {
			t.Fatalf("LearningReflectionJobsByWorkflowRunTree: %v", err)
		}
		if len(jobs) != 0 {
			t.Fatalf("non opt-in workflow recorded early reflection jobs: %+v", jobs)
		}
	})

	t.Run("tail aggregation marker is not enqueue marker", func(t *testing.T) {
		recorder := openEarlyIntegrationRecorder(t)
		agentClient := newEarlyIntegrationAgent(recorder, map[string]provider.AgentResponse{
			"tail-call": {Content: "tail done", SessionID: "session-tail", Success: true},
		})
		var (
			runID          string
			tailMarkerSeen bool
		)
		def := workflow.Definition{
			ID:       "agent-learnings",
			Label:    "Agent Learnings",
			Recorder: recorder,
			Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
				_, tailMarkerSeen = workflowctx.EarlyReflectionPolicyFromContext(ctx)
				onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "tail started"})
				_, err := agentClient.CallAgent(ctx, "tail-call", cwd, nil, "", &workflowctx.CallAgentOptions{StepID: "tail-step"})
				return err
			},
		}
		ctx := ContextWithEarlyAggregationEnabled(ContextWithParentRunID(context.Background(), "parent-run"))
		if err := def.Run(ctx, t.TempDir(), func(event workflow.WorkflowEvent) {
			if runID == "" {
				runID = event.RunID
			}
		}, nil); err != nil {
			t.Fatalf("tail Run: %v", err)
		}
		if tailMarkerSeen {
			t.Fatalf("tail context unexpectedly contained early reflection enqueue marker")
		}
		jobs, err := recorder.LearningReflectionJobsByWorkflowRunTree(context.Background(), runID)
		if err != nil {
			t.Fatalf("LearningReflectionJobsByWorkflowRunTree: %v", err)
		}
		if len(jobs) != 0 {
			t.Fatalf("tail context recorded early reflection jobs: %+v", jobs)
		}
	})
}

func TestEarlyReflectionIntegrationMatrixWorkerCallDropsFrontMarker(t *testing.T) {
	store := newRuntimeTestStore()
	store.enqueueClaim(akashic.LearningReflectionJob{
		AgentCallID:   "call-worker",
		WorkflowRunID: "run-worker",
		ProviderID:    "codex",
		ProjectID:     "project-rilll",
		SessionID:     "session-worker",
		Status:        akashic.LearningReflectionStatusRunning,
	})
	agent := &runtimeTestAgent{
		recorder: store,
		response: provider.AgentResponse{
			Content: `{"learnings":[]}`,
		},
		checkContext: func(ctx context.Context) {
			if _, ok := workflowctx.EarlyReflectionPolicyFromContext(ctx); ok {
				store.recordMarkerSeen()
			}
		},
	}
	ctx := workflowctx.ContextWithEarlyReflectionEnabled(context.Background(), workflowctx.EarlyReflectionPolicy{
		AllowedProviders: []provider.ProviderID{provider.ProviderCodex},
	})
	rt, err := StartEarlyReflectorRuntime(ctx, t.TempDir(), store, agent, EarlyReflectorRuntimeConfig{
		ProjectID:    "project-rilll",
		WorkerID:     "worker-rilll",
		MaxParallel:  1,
		PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("StartEarlyReflectorRuntime: %v", err)
	}
	defer rt.Shutdown()

	waitUntilRuntimeTest(t, time.Second, func() bool { return store.succeededCount() == 1 })
	if store.markerSeen() {
		t.Fatalf("worker CallAgent received early reflection enqueue marker")
	}
}

func TestEarlyReflectionIntegrationMatrixReadOnlySurfacesDoNotRunWorkflowRuntime(t *testing.T) {
	var runtimeClaimCount atomic.Int32
	def := workflow.Definition{
		ID:    "wrapped-workflow",
		Label: "Wrapped Workflow" + learningsTailLabelMarker,
		Fn: func(context.Context, string, func(workflow.WorkflowEvent), workflow.WorkflowAsk) error {
			runtimeClaimCount.Add(1)
			return nil
		},
	}

	var stdout, stderr bytes.Buffer
	code := cli.Dispatch(context.Background(), []string{"rilll", "list"}, cli.Deps{
		Stdout:    &stdout,
		Stderr:    &stderr,
		Workflows: []workflow.Definition{def},
		CWD:       t.TempDir(),
	})
	if code != cli.ExitOK {
		t.Fatalf("cli list exit code = %d, want %d; stderr=%s", code, cli.ExitOK, stderr.String())
	}
	if runtimeClaimCount.Load() != 0 {
		t.Fatalf("runtime claim count after cli list = %d, want 0", runtimeClaimCount.Load())
	}

	model := ui.NewModel(ui.Options{
		CWD:       t.TempDir(),
		Config:    config.Default(),
		Workflows: []workflow.Definition{def},
	})
	_ = model.View()
	if runtimeClaimCount.Load() != 0 {
		t.Fatalf("runtime claim count after TUI display = %d, want 0", runtimeClaimCount.Load())
	}
}

type earlyIntegrationRuntimeSpy struct {
	notifyCount   atomic.Int32
	shutdownCount atomic.Int32
}

func (s *earlyIntegrationRuntimeSpy) Notify() {
	s.notifyCount.Add(1)
}

func (s *earlyIntegrationRuntimeSpy) Shutdown() {
	s.shutdownCount.Add(1)
}

type earlyIntegrationProvider struct {
	id        provider.ProviderID
	responses map[string]provider.AgentResponse

	mu       sync.Mutex
	requests []provider.AgentRequest
}

func (p *earlyIntegrationProvider) ID() provider.ProviderID {
	return p.id
}

func (p *earlyIntegrationProvider) Call(_ context.Context, req provider.AgentRequest, _ provider.AgentEventCallback) (provider.AgentResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if resp, ok := p.responses[req.Prompt]; ok {
		return resp, nil
	}
	return provider.AgentResponse{
		Content:   "ok",
		SessionID: "session-" + req.Metadata.StepID,
		Success:   true,
	}, nil
}

func openEarlyIntegrationRecorder(t *testing.T) *akashic.SQLiteRecorder {
	t.Helper()
	recorder, err := akashic.OpenSQLite(filepath.Join(t.TempDir(), "akashic.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	return recorder
}

func newEarlyIntegrationAgent(recorder akashic.Recorder, codexResponses map[string]provider.AgentResponse) *agentpkg.Agent {
	codex := &earlyIntegrationProvider{
		id:        provider.ProviderCodex,
		responses: codexResponses,
	}
	return agentpkg.New(provider.ProviderCodex, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderCodex: codex,
	}, recorder)
}

func earlyIntegrationJobsByStep(jobs []akashic.LearningReflectionJob) map[string]akashic.LearningReflectionJob {
	out := make(map[string]akashic.LearningReflectionJob, len(jobs))
	for _, job := range jobs {
		out[job.StepID] = job
	}
	return out
}

func assertEarlyIntegrationJob(
	t *testing.T,
	job akashic.LearningReflectionJob,
	status akashic.LearningReflectionStatus,
	reason string,
	sessionID string,
	providerID string,
	workflowRunID string,
) {
	t.Helper()
	if job.AgentCallID == "" {
		t.Fatalf("job for step %q is missing", job.StepID)
	}
	if job.Status != status {
		t.Fatalf("job %s status = %q, want %q: %+v", job.StepID, job.Status, status, job)
	}
	if string(job.SkipReason) != reason {
		t.Fatalf("job %s skip reason = %q, want %q: %+v", job.StepID, job.SkipReason, reason, job)
	}
	if job.SessionID != sessionID {
		t.Fatalf("job %s session = %q, want %q: %+v", job.StepID, job.SessionID, sessionID, job)
	}
	if job.ProviderID != providerID {
		t.Fatalf("job %s provider = %q, want %q: %+v", job.StepID, job.ProviderID, providerID, job)
	}
	if job.WorkflowRunID != workflowRunID {
		t.Fatalf("job %s workflow run = %q, want %q: %+v", job.StepID, job.WorkflowRunID, workflowRunID, job)
	}
}
