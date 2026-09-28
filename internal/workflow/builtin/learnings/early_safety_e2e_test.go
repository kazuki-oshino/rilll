package learnings

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

func TestEarlyReflectionE2ECancelBeforeApplyKeepsPlaybookUnchangedAndJobsDiagnosable(t *testing.T) {
	t.Setenv(envRillLearningsEarlyWaitMS, "1")

	ctx := context.Background()
	cwd := makeFixtureProjectCWD(t, "project-rilll")
	projectID := akashic.ProjectIDFromCWD(cwd)
	recorder := openEarlyIntegrationRecorder(t)
	base := time.Date(2026, 5, 24, 15, 0, 0, 0, time.UTC)
	parentRunID := "cancel-before-apply-parent"
	tailRunID := "cancel-before-apply-tail"

	playbookDir := filepath.Join(cwd, playbookRootDirRel)
	if err := os.MkdirAll(playbookDir, 0o755); err != nil {
		t.Fatalf("mkdir playbook dir: %v", err)
	}
	playbookPath := filepath.Join(playbookDir, CategoryFailureRecovery+".md")
	playbookBefore := "# failure-recovery\n\n失敗 → 復旧の手順。\n\n## existing\n**適用条件:** existing safety fixture\n**summary:** existing safety summary\n\n- existing safety bullet\n"
	if err := os.WriteFile(playbookPath, []byte(playbookBefore), 0o644); err != nil {
		t.Fatalf("write baseline playbook: %v", err)
	}

	seedTailMatrixWorkflowRun(t, recorder, parentRunID, "safety-front", "Safety Front", cwd, projectID, base)
	seedTailMatrixWorkflowRun(t, recorder, tailRunID, learningsWorkflowID, learningsWorkflowLabel, cwd, projectID, base.Add(time.Second))
	seedTailMatrixAgentCall(t, recorder, "call-succeeded", parentRunID, "session-succeeded", "codex", "succeeded", cwd, projectID, base.Add(10*time.Millisecond))
	seedTailMatrixAgentCall(t, recorder, "call-pending", parentRunID, "session-pending", "codex", "pending", cwd, projectID, base.Add(20*time.Millisecond))

	succeeded := tailMatrixJob("call-succeeded", parentRunID, "session-succeeded", "codex", "succeeded", akashic.LearningReflectionStatusSucceeded, cwd, projectID, base.Add(30*time.Millisecond))
	succeeded.ResultJSON = earlyAggregationResultJSON(t, "call-succeeded", "session-succeeded", "codex", "succeeded", []LearningReflectionAcceptedLearning{
		{
			ID:        "learning-cancel-before-apply",
			Category:  akashic.CategoryFailureRecovery,
			Context:   "ctx cancel before apply",
			Insight:   "ins cancel before apply",
			PathsGlob: []string{"internal/workflow/builtin/learnings/**"},
		},
	})
	seedTailMatrixJob(t, recorder, succeeded)
	seedTailMatrixJob(t, recorder, tailMatrixJob("call-pending", parentRunID, "session-pending", "codex", "pending", akashic.LearningReflectionStatusPending, cwd, projectID, base.Add(40*time.Millisecond)))

	runCtx := makeRunCtx(t, cwd, tailRunID, parentRunID, recorder)
	runCtx = ContextWithEarlyAggregationEnabled(runCtx)
	runCtx, cancel := context.WithCancel(runCtx)
	agent := &cancelBeforeApplyAgent{
		reader:   recorder,
		recorder: recorder,
		cancel:   cancel,
		curatorResp: provider.AgentResponse{
			Content: `{"operations":[{"op":"add","category":"failure-recovery","topic":"cancel-before-apply","paths_glob":["internal/workflow/builtin/learnings/**"],"entry":"cancel before apply bullet","summary":"cancel before apply summary","applies_to":"cancel before apply"}]}`,
		},
	}

	err := runLearningsOrchestration(runCtx, cwd, nil, nil, agent)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runLearningsOrchestration err=%v, want context.Canceled before Apply", err)
	}
	if got, want := agent.stepCalls(), []string{stepIDCurator}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CallAgent steps=%v, want %v", got, want)
	}

	playbookAfter, err := os.ReadFile(playbookPath)
	if err != nil {
		t.Fatalf("read playbook after cancel: %v", err)
	}
	if string(playbookAfter) != playbookBefore {
		t.Fatalf("playbook changed despite cancel before Apply\nbefore:\n%s\nafter:\n%s", playbookBefore, playbookAfter)
	}
	if _, err := os.Stat(filepath.Join(playbookDir, "index.md")); !os.IsNotExist(err) {
		t.Fatalf("playbook index should not be rebuilt when Apply is skipped; stat err=%v", err)
	}

	jobs := readEarlyWorkerJobsByID(t, recorder, []string{"call-succeeded", "call-pending"})
	if succeededJob := jobs["call-succeeded"]; succeededJob.Status != akashic.LearningReflectionStatusAggregated || len(succeededJob.ResultJSON) == 0 {
		t.Fatalf("succeeded job readback=%+v, want aggregated with retained result_json", succeededJob)
	}
	if staleJob := jobs["call-pending"]; staleJob.Status != akashic.LearningReflectionStatusStale || staleJob.FailureReason != earlyAggregationWaitTimeoutReason {
		t.Fatalf("pending job readback=%+v, want stale diagnostic state", staleJob)
	}

	earlyArtifact := readEarlyReflectionArtifact(t, filepath.Join(cwd, ".rilll", learningsWorkflowID, tailRunID, earlyReflectionArtifactFilename))
	if earlyArtifact.Summary.Included != 1 || earlyArtifact.Summary.Stale != 1 || earlyArtifact.Summary.FallbackReason != earlyAggregationFallbackTimeout {
		t.Fatalf("early artifact summary=%+v, want included=1 stale=1 timeout fallback", earlyArtifact.Summary)
	}
	applyResult := readApplyArtifact(t, cwd, tailRunID)
	if applyResult.Verdict != VerdictAppliedNoOp || len(applyResult.Appended) != 0 {
		t.Fatalf("apply result=%+v, want applied-no-op with no appended entries", applyResult)
	}
	rows, err := recorder.LearningsByWorkflowRun(ctx, parentRunID)
	if err != nil {
		t.Fatalf("LearningsByWorkflowRun: %v", err)
	}
	if got, want := learningRowIDs(rows), []string{"learning-cancel-before-apply"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("learning rows IDs=%v, want retained succeeded job learning %v", got, want)
	}
}

func TestEarlyReflectionE2ERuntimeShutdownOnFrontErrorAndTailCompletion(t *testing.T) {
	t.Run("front error cancels in-flight worker", func(t *testing.T) {
		cwd := makeFixtureProjectCWD(t, "project-rilll")
		projectID := akashic.ProjectIDFromCWD(cwd)
		recorder := openEarlyIntegrationRecorder(t)
		base := time.Date(2026, 5, 24, 15, 30, 0, 0, time.UTC)
		seedEarlyWorkerJob(t, recorder, akashic.LearningReflectionJob{
			AgentCallID:   "call-in-flight",
			WorkflowRunID: "front-error-run",
			ProjectID:     projectID,
			ProviderID:    "codex",
			SessionID:     "session-in-flight",
			CWD:           cwd,
			QueuedAt:      base,
		})

		workerAgent := &runtimeTestAgent{blockUntilCanceled: true}
		var runtime *countingWorkflowRuntime
		frontErr := errors.New("front failed after worker started")
		frontDef := workflow.Definition{
			ID:       "front-error",
			Label:    "Front Error",
			Recorder: recorder,
			Fn: func(_ context.Context, _ string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
				onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "front started"})
				waitUntilRuntimeTest(t, time.Second, func() bool { return workerAgent.callCount() == 1 })
				return frontErr
			},
		}
		tailCalls := 0
		tailDef := workflow.Definition{
			ID:       learningsWorkflowID,
			Label:    learningsWorkflowLabel,
			Recorder: recorder,
			Fn: func(context.Context, string, func(workflow.WorkflowEvent), workflow.WorkflowAsk) error {
				tailCalls++
				return nil
			},
		}
		deps := sequentialWithLearningsTailDeps{
			agent:    &fakeWorkflowAgent{reader: recorder, recorder: recorder},
			frontDef: frontDef,
			tailDef:  tailDef,
			startEarlyRuntime: func(ctx context.Context, cwd string, _ workflowctx.AgentCaller, _ EarlyReflectorRuntimeConfig) (workflowScopedEarlyReflectorRuntime, bool, error) {
				rt, err := StartEarlyReflectorRuntime(ctx, cwd, recorder, workerAgent, EarlyReflectorRuntimeConfig{
					ProjectID:    projectID,
					WorkerID:     "safety-worker",
					MaxParallel:  1,
					PollInterval: time.Hour,
				})
				if err != nil {
					return nil, false, err
				}
				runtime = &countingWorkflowRuntime{inner: rt}
				return runtime, true, nil
			},
		}

		err := runSequentialWithLearningsTailOrchestration(context.Background(), cwd, nil, nil, deps)
		if !errors.Is(err, frontErr) {
			t.Fatalf("orchestration err=%v, want %v", err, frontErr)
		}
		if tailCalls != 0 {
			t.Fatalf("tail calls=%d, want 0 after front error", tailCalls)
		}
		if runtime == nil || runtime.shutdownCount.Load() != 1 {
			t.Fatalf("runtime shutdown count=%v, want 1", runtimeShutdownCount(runtime))
		}
		waitUntilRuntimeTest(t, time.Second, workerAgent.sawCanceled)
		job := onlyReflectionJob(t, recorder, "call-in-flight")
		if job.Status != akashic.LearningReflectionStatusPending || job.AttemptCount != 1 || job.FailureReason != failureReasonCallAgent {
			t.Fatalf("in-flight job readback=%+v, want pending retry diagnostic after shutdown cancel", job)
		}
		assertNoPlaybookWrites(t, cwd)
	})

	t.Run("tail completion shuts down runtime", func(t *testing.T) {
		cwd := makeFixtureProjectCWD(t, "project-rilll")
		projectID := akashic.ProjectIDFromCWD(cwd)
		recorder := openEarlyIntegrationRecorder(t)
		agent := &fakeWorkflowAgent{reader: recorder, recorder: recorder}
		var runtime *countingWorkflowRuntime
		frontDef := workflow.Definition{
			ID:       "tail-completion-front",
			Label:    "Tail Completion Front",
			Recorder: recorder,
			Fn: func(_ context.Context, _ string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
				onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "front done"})
				return nil
			},
		}
		tailCalls := 0
		tailDef := LearningsTailDef(agent)
		originalTailFn := tailDef.Fn
		tailDef.Fn = func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			tailCalls++
			return originalTailFn(ctx, cwd, onEvent, ask)
		}
		deps := sequentialWithLearningsTailDeps{
			agent:    agent,
			frontDef: frontDef,
			tailDef:  tailDef,
			startEarlyRuntime: func(ctx context.Context, cwd string, agent workflowctx.AgentCaller, _ EarlyReflectorRuntimeConfig) (workflowScopedEarlyReflectorRuntime, bool, error) {
				rt, err := StartEarlyReflectorRuntime(ctx, cwd, recorder, agent, EarlyReflectorRuntimeConfig{
					ProjectID:    projectID,
					WorkerID:     "safety-tail-worker",
					MaxParallel:  1,
					PollInterval: time.Hour,
				})
				if err != nil {
					return nil, false, err
				}
				runtime = &countingWorkflowRuntime{inner: rt}
				return runtime, true, nil
			},
		}

		if err := runSequentialWithLearningsTailOrchestration(context.Background(), cwd, nil, nil, deps); err != nil {
			t.Fatalf("orchestration: %v", err)
		}
		if tailCalls != 1 {
			t.Fatalf("tail calls=%d, want 1", tailCalls)
		}
		if runtime == nil || runtime.shutdownCount.Load() != 1 {
			t.Fatalf("runtime shutdown count=%v, want 1", runtimeShutdownCount(runtime))
		}
		assertNoPlaybookWrites(t, cwd)
	})
}

type cancelBeforeApplyAgent struct {
	mu          sync.Mutex
	reader      akashic.Reader
	recorder    akashic.Recorder
	cancel      context.CancelFunc
	curatorResp provider.AgentResponse
	calls       []string
}

func (a *cancelBeforeApplyAgent) AkashicReader() akashic.Reader     { return a.reader }
func (a *cancelBeforeApplyAgent) AkashicRecorder() akashic.Recorder { return a.recorder }

func (a *cancelBeforeApplyAgent) CallAgent(_ context.Context, _ string, _ string, _ provider.AgentEventCallback, _ string, opts *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
	step := ""
	if opts != nil {
		step = opts.StepID
	}
	a.mu.Lock()
	a.calls = append(a.calls, step)
	a.mu.Unlock()
	if step != stepIDCurator {
		return provider.AgentResponse{}, fmt.Errorf("unexpected CallAgent step %q", step)
	}
	if a.cancel != nil {
		a.cancel()
	}
	return a.curatorResp, nil
}

func (a *cancelBeforeApplyAgent) stepCalls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

type countingWorkflowRuntime struct {
	inner         workflowScopedEarlyReflectorRuntime
	notifyCount   atomic.Int32
	shutdownCount atomic.Int32
}

func (r *countingWorkflowRuntime) Notify() {
	r.notifyCount.Add(1)
	if r.inner != nil {
		r.inner.Notify()
	}
}

func (r *countingWorkflowRuntime) Shutdown() {
	r.shutdownCount.Add(1)
	if r.inner != nil {
		r.inner.Shutdown()
	}
}

func runtimeShutdownCount(r *countingWorkflowRuntime) int32 {
	if r == nil {
		return 0
	}
	return r.shutdownCount.Load()
}

func assertNoPlaybookWrites(t *testing.T, cwd string) {
	t.Helper()
	playbookRoot := filepath.Join(cwd, playbookRootDirRel)
	if _, err := os.Stat(playbookRoot); !os.IsNotExist(err) {
		t.Fatalf("playbook root should not exist without Apply writes; stat err=%v", err)
	}
}
