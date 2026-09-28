package learnings

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
)

func TestEarlyReflectionIntegrationMatrixTailAggregationStatusesAndBoundaries(t *testing.T) {
	t.Setenv(envRillLearningsEarlyWaitMS, "60")

	ctx := context.Background()
	cwd := makeFixtureProjectCWD(t, "project-rilll")
	projectID := akashic.ProjectIDFromCWD(cwd)
	recorder := openEarlyIntegrationRecorder(t)
	base := time.Date(2026, 5, 24, 13, 0, 0, 0, time.UTC)
	parentRunID := "tail-matrix-parent"
	childRunID := "tail-matrix-child"
	tailRunID := "tail-matrix-tail"
	runDir := filepath.Join(cwd, ".rilll", learningsWorkflowID, tailRunID)

	seedTailMatrixWorkflowRun(t, recorder, parentRunID, "matrix-front", "Matrix Front", cwd, projectID, base)
	seedTailMatrixWorkflowRun(t, recorder, childRunID, "matrix-child", "Matrix Child", cwd, projectID, base.Add(time.Second))
	seedTailMatrixWorkflowRun(t, recorder, tailRunID, learningsWorkflowID, learningsWorkflowLabel, cwd, projectID, base.Add(2*time.Second))
	if err := recorder.RecordWorkflowRunChild(ctx, akashic.WorkflowRunChild{
		ParentRunID:       parentRunID,
		ChildRunID:        childRunID,
		ChildWorkflowID:   "matrix-child",
		ChildWorkflowName: "Matrix Child",
		RecordedAt:        base.Add(time.Second),
	}); err != nil {
		t.Fatalf("RecordWorkflowRunChild: %v", err)
	}

	calls := []struct {
		id            string
		workflowRunID string
		sessionID     string
		providerID    string
		stepID        string
		startedAt     time.Time
	}{
		{"call-succeeded", parentRunID, "session-succeeded", "codex", "succeeded-before-tail", base.Add(10 * time.Millisecond)},
		{"call-failed", parentRunID, "session-failed", "codex", "failed-terminal", base.Add(20 * time.Millisecond)},
		{"call-skipped", parentRunID, "session-skipped", "cursor", "skipped-provider", base.Add(30 * time.Millisecond)},
		{"call-missing", parentRunID, "session-missing", "codex", "missing-job", base.Add(40 * time.Millisecond)},
		{"call-wait-completed", childRunID, "session-wait-completed", "claude", "wait-completed", base.Add(50 * time.Millisecond)},
		{"call-timeout-stale", childRunID, "session-timeout-stale", "codex", "timeout-stale", base.Add(60 * time.Millisecond)},
	}
	for _, call := range calls {
		seedTailMatrixAgentCall(t, recorder, call.id, call.workflowRunID, call.sessionID, call.providerID, call.stepID, cwd, projectID, call.startedAt)
	}

	succeeded := tailMatrixJob("call-succeeded", parentRunID, "session-succeeded", "codex", "succeeded-before-tail", akashic.LearningReflectionStatusSucceeded, cwd, projectID, base)
	succeeded.ResultJSON = earlyAggregationResultJSON(t, "call-succeeded", "session-succeeded", "codex", "succeeded-before-tail", []LearningReflectionAcceptedLearning{
		{
			ID:        "learning-tail-succeeded",
			Category:  akashic.CategoryFailureRecovery,
			Context:   "ctx succeeded",
			Insight:   "ins succeeded",
			PathsGlob: []string{"internal/workflow/builtin/learnings/**"},
		},
	})
	seedTailMatrixJob(t, recorder, succeeded)

	failed := tailMatrixJob("call-failed", parentRunID, "session-failed", "codex", "failed-terminal", akashic.LearningReflectionStatusFailed, cwd, projectID, base.Add(time.Millisecond))
	failed.FailureReason = failureReasonNoCredentials
	failed.FailureDetail = "missing API key"
	seedTailMatrixJob(t, recorder, failed)

	skipped := tailMatrixJob("call-skipped", parentRunID, "session-skipped", "cursor", "skipped-provider", akashic.LearningReflectionStatusSkipped, cwd, projectID, base.Add(2*time.Millisecond))
	skipped.SkipReason = akashic.LearningReflectionSkipProviderNotInAllowlist
	seedTailMatrixJob(t, recorder, skipped)

	waitCompleted := tailMatrixJob("call-wait-completed", childRunID, "session-wait-completed", "claude", "wait-completed", akashic.LearningReflectionStatusRunning, cwd, projectID, base.Add(3*time.Millisecond))
	waitCompleted.AttemptCount = 1
	waitCompleted.WorkerID = "worker-matrix"
	startedAt := base.Add(3 * time.Millisecond)
	leaseUntil := base.Add(time.Minute)
	waitCompleted.StartedAt = &startedAt
	waitCompleted.LeaseUntil = &leaseUntil
	seedTailMatrixJob(t, recorder, waitCompleted)
	go func() {
		time.Sleep(10 * time.Millisecond)
		result := earlyAggregationResultJSON(t, "call-wait-completed", "session-wait-completed", "claude", "wait-completed", []LearningReflectionAcceptedLearning{
			{
				ID:        "learning-tail-wait-completed",
				Category:  akashic.CategoryToolPattern,
				Context:   "ctx wait",
				Insight:   "ins wait",
				PathsGlob: []string{"internal/workflow/**"},
			},
		})
		_ = recorder.MarkLearningReflectionSucceeded(context.Background(), "call-wait-completed", akashic.LearningReflectionResult{JSON: result}, time.Now().UTC())
	}()

	pending := tailMatrixJob("call-timeout-stale", childRunID, "session-timeout-stale", "codex", "timeout-stale", akashic.LearningReflectionStatusPending, cwd, projectID, base.Add(4*time.Millisecond))
	seedTailMatrixJob(t, recorder, pending)

	agent := &fakeWorkflowAgent{
		reader:       recorder,
		recorder:     recorder,
		reflectorErr: errors.New("legacy reflector must not run in store-backed early aggregation"),
		curatorResp: provider.AgentResponse{
			Content: `{"operations":[{"op":"add","category":"failure-recovery","topic":"tail-matrix","paths_glob":["internal/workflow/builtin/learnings/**"],"entry":"tail matrix boundary bullet","summary":"tail matrix summary","applies_to":"tail matrix"}]}`,
		},
	}

	runCtx := makeRunCtx(t, cwd, tailRunID, parentRunID, recorder)
	runCtx = ContextWithEarlyAggregationEnabled(runCtx)
	if err := runLearningsOrchestration(runCtx, cwd, nil, nil, agent); err != nil {
		t.Fatalf("runLearningsOrchestration: %v", err)
	}

	if len(agent.calls) != 1 || agent.calls[0] != stepIDCurator {
		t.Fatalf("CallAgent steps = %v, want [%s] (legacy reflector skipped, curator once)", agent.calls, stepIDCurator)
	}

	earlyArtifact := readEarlyReflectionArtifact(t, filepath.Join(runDir, earlyReflectionArtifactFilename))
	wantSummary := EarlyReflectionSummary{
		TotalTargets:   6,
		Included:       2,
		WaitCompleted:  1,
		Skipped:        1,
		Failed:         1,
		Stale:          1,
		MissingJob:     1,
		FallbackReason: earlyAggregationFallbackTimeout,
	}
	if earlyArtifact.Summary != wantSummary {
		t.Fatalf("early summary=%+v, want %+v", earlyArtifact.Summary, wantSummary)
	}
	targets := earlyAggregationTargetsByID(earlyArtifact.Targets)
	assertEarlyAggregationTarget(t, targets["call-succeeded"], EarlyReflectionTargetStatusSucceeded, "", true)
	assertEarlyAggregationTarget(t, targets["call-wait-completed"], EarlyReflectionTargetStatusWaitCompleted, "", true)
	assertEarlyAggregationTarget(t, targets["call-failed"], EarlyReflectionTargetStatusFailed, failureReasonNoCredentials, false)
	assertEarlyAggregationTarget(t, targets["call-skipped"], EarlyReflectionTargetStatusSkipped, string(akashic.LearningReflectionSkipProviderNotInAllowlist), false)
	assertEarlyAggregationTarget(t, targets["call-missing"], EarlyReflectionTargetStatusMissingJob, earlyAggregationMissingJobReason, false)
	assertEarlyAggregationTarget(t, targets["call-timeout-stale"], EarlyReflectionTargetStatusStale, earlyAggregationWaitTimeoutReason, false)

	learningsArtifact := readLearningsArtifact(t, filepath.Join(runDir, learningsArtifactFilename))
	if got, want := learningIDs(learningsArtifact.Learnings), []string{"learning-tail-succeeded", "learning-tail-wait-completed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("learnings.json IDs=%v, want %v", got, want)
	}
	reflectorArtifact := readReflectorArtifactT(t, filepath.Join(runDir, reflectorFilename))
	if len(reflectorArtifact.PerSession) != 2 {
		t.Fatalf("reflector per_session count=%d, want 2: %+v", len(reflectorArtifact.PerSession), reflectorArtifact.PerSession)
	}

	rows, err := recorder.LearningsByWorkflowRun(ctx, parentRunID)
	if err != nil {
		t.Fatalf("LearningsByWorkflowRun: %v", err)
	}
	if got, want := learningRowIDs(rows), []string{"learning-tail-succeeded", "learning-tail-wait-completed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("learning rows IDs=%v, want %v", got, want)
	}

	jobs := readEarlyWorkerJobsByID(t, recorder, []string{
		"call-succeeded",
		"call-failed",
		"call-skipped",
		"call-wait-completed",
		"call-timeout-stale",
	})
	for _, id := range []string{"call-succeeded", "call-failed", "call-skipped", "call-wait-completed"} {
		job := jobs[id]
		if job.Status != akashic.LearningReflectionStatusAggregated || job.AggregatedTailRunID != tailRunID || job.AggregatedAt == nil {
			t.Fatalf("job %s aggregation readback=%+v, want aggregated by %s", id, job, tailRunID)
		}
	}
	if stale := jobs["call-timeout-stale"]; stale.Status != akashic.LearningReflectionStatusStale || stale.AggregatedTailRunID != "" {
		t.Fatalf("stale job readback=%+v, want stale and not independently aggregated", stale)
	}

	artifacts, err := recorder.GetArtifacts(ctx, tailRunID)
	if err != nil {
		t.Fatalf("GetArtifacts: %v", err)
	}
	for _, name := range []string{sessionsFilename, reflectorFilename, learningsArtifactFilename, earlyReflectionArtifactFilename, curatorFilename, applyResultFilename} {
		if !tailMatrixHasArtifactBase(artifacts, name) {
			t.Fatalf("artifact %s not registered; artifacts=%+v", name, artifacts)
		}
	}

	applyResult := readApplyArtifact(t, cwd, tailRunID)
	if applyResult.Verdict != VerdictApplied || len(applyResult.Appended) != 1 {
		t.Fatalf("apply result=%+v, want one applied operation", applyResult)
	}
	playbookPath := filepath.Join(cwd, playbookRootDirRel, CategoryFailureRecovery+".md")
	body, err := os.ReadFile(playbookPath)
	if err != nil {
		t.Fatalf("read playbook category file: %v", err)
	}
	if strings.Count(string(body), "tail matrix boundary bullet") != 1 {
		t.Fatalf("playbook bullet count = %d, want 1\n%s", strings.Count(string(body), "tail matrix boundary bullet"), body)
	}
	if _, err := os.Stat(filepath.Join(cwd, playbookRootDirRel, "index.md")); err != nil {
		t.Fatalf("playbook index not rebuilt in wiki schema path: %v", err)
	}
}

func TestEarlyReflectionIntegrationMatrixTailFallbacksUseLegacyReflector(t *testing.T) {
	withProviderEnv(t, "codex")
	t.Setenv(envRillLearningsEarlyWaitMS, "20")

	t.Run("store unavailable recorder", func(t *testing.T) {
		cwd := makeFixtureProjectCWD(t, "project-rilll")
		recorder := openEarlyIntegrationRecorder(t)
		parentRunID := "fallback-parent"
		tailRunID := "fallback-tail"
		projectID := akashic.ProjectIDFromCWD(cwd)
		base := time.Date(2026, 5, 24, 14, 0, 0, 0, time.UTC)
		seedTailMatrixWorkflowRun(t, recorder, parentRunID, "fallback-front", "Fallback Front", cwd, projectID, base)
		seedTailMatrixAgentCall(t, recorder, "fallback-call", parentRunID, "fallback-session", "codex", "fallback-step", cwd, projectID, base.Add(time.Millisecond))

		fakeRecorder := &fakeWorkflowRecorder{}
		agent := &fakeWorkflowAgent{
			reader:   recorder,
			recorder: fakeRecorder,
			reflectorResp: provider.AgentResponse{
				Content: `{"learnings":[{"category":"failure-recovery","context":"legacy ctx","insight":"legacy insight","paths_glob":["internal/**"]}]}`,
			},
			curatorResp: provider.AgentResponse{
				Content: `{"operations":[{"op":"add","category":"failure-recovery","topic":"store-unavailable-fallback","paths_glob":["internal/**"],"entry":"store unavailable fallback bullet","summary":"store fallback summary","applies_to":"store fallback"}]}`,
			},
		}
		runCtx := makeRunCtx(t, cwd, tailRunID, parentRunID, fakeRecorder)
		runCtx = ContextWithEarlyAggregationEnabled(runCtx)
		if err := runLearningsOrchestration(runCtx, cwd, nil, nil, agent); err != nil {
			t.Fatalf("runLearningsOrchestration: %v", err)
		}
		if got, want := agent.calls, []string{stepIDReflector, stepIDCurator}; !reflect.DeepEqual(got, want) {
			t.Fatalf("CallAgent steps=%v, want %v", got, want)
		}
		if fakeRecorder.hasArtifactBase(earlyReflectionArtifactFilename) {
			t.Fatalf("early-reflection artifact registered despite store unavailable fallback")
		}
		if !fakeRecorder.hasArtifactBase(reflectorFilename) || !fakeRecorder.hasArtifactBase(learningsArtifactFilename) {
			t.Fatalf("legacy reflector artifacts missing after fallback; artifacts=%+v", fakeRecorder.artifacts)
		}
		if _, err := os.Stat(filepath.Join(cwd, ".rilll", learningsWorkflowID, tailRunID, earlyReflectionArtifactFilename)); !os.IsNotExist(err) {
			t.Fatalf("early-reflection file exists in legacy fallback path: err=%v", err)
		}
	})

	t.Run("NoopRecorder fallback", func(t *testing.T) {
		cwd := makeFixtureProjectCWD(t, "project-rilll")
		reader := openEarlyIntegrationRecorder(t)
		parentRunID := "noop-parent"
		tailRunID := "noop-tail"
		projectID := akashic.ProjectIDFromCWD(cwd)
		base := time.Date(2026, 5, 24, 14, 10, 0, 0, time.UTC)
		seedTailMatrixWorkflowRun(t, reader, parentRunID, "noop-front", "Noop Front", cwd, projectID, base)
		seedTailMatrixAgentCall(t, reader, "noop-call", parentRunID, "noop-session", "codex", "noop-step", cwd, projectID, base.Add(time.Millisecond))

		noopRecorder := akashic.NoopRecorder{}
		agent := &fakeWorkflowAgent{
			reader:   reader,
			recorder: noopRecorder,
			reflectorResp: provider.AgentResponse{
				Content: `{"learnings":[{"category":"failure-recovery","context":"noop ctx","insight":"noop insight","paths_glob":["internal/**"]}]}`,
			},
			curatorResp: provider.AgentResponse{
				Content: `{"operations":[{"op":"add","category":"failure-recovery","topic":"noop-fallback","paths_glob":["internal/**"],"entry":"noop fallback bullet","summary":"noop fallback summary","applies_to":"noop fallback"}]}`,
			},
		}
		runCtx := makeRunCtx(t, cwd, tailRunID, parentRunID, noopRecorder)
		runCtx = ContextWithEarlyAggregationEnabled(runCtx)
		if err := runLearningsOrchestration(runCtx, cwd, nil, nil, agent); err != nil {
			t.Fatalf("runLearningsOrchestration: %v", err)
		}
		if got, want := agent.calls, []string{stepIDReflector, stepIDCurator}; !reflect.DeepEqual(got, want) {
			t.Fatalf("CallAgent steps=%v, want %v", got, want)
		}
		if _, err := os.Stat(filepath.Join(cwd, ".rilll", learningsWorkflowID, tailRunID, earlyReflectionArtifactFilename)); !os.IsNotExist(err) {
			t.Fatalf("early-reflection file exists in NoopRecorder legacy fallback path: err=%v", err)
		}
		applyResult := readApplyArtifact(t, cwd, tailRunID)
		if applyResult.Verdict != VerdictApplied || len(applyResult.Appended) != 1 {
			t.Fatalf("NoopRecorder apply result=%+v, want one applied legacy operation", applyResult)
		}
	})
}

func seedTailMatrixWorkflowRun(t *testing.T, recorder *akashic.SQLiteRecorder, runID, workflowID, workflowName, cwd, projectID string, startedAt time.Time) {
	t.Helper()
	if err := recorder.RecordWorkflowRunStart(context.Background(), akashic.WorkflowRun{
		ID:           runID,
		WorkflowID:   workflowID,
		WorkflowName: workflowName,
		CWD:          cwd,
		ProjectID:    projectID,
		StartedAt:    startedAt,
	}); err != nil {
		t.Fatalf("RecordWorkflowRunStart(%s): %v", runID, err)
	}
}

func seedTailMatrixAgentCall(t *testing.T, recorder *akashic.SQLiteRecorder, callID, workflowRunID, sessionID, providerID, stepID, cwd, projectID string, startedAt time.Time) {
	t.Helper()
	if err := recorder.RecordAgentCallStart(context.Background(), akashic.AgentCall{
		ID:            callID,
		WorkflowRunID: workflowRunID,
		WorkflowID:    "matrix-workflow",
		WorkflowName:  "Matrix Workflow",
		StepID:        stepID,
		StepName:      "Step " + stepID,
		ProviderID:    providerID,
		Prompt:        "prompt " + callID,
		CWD:           cwd,
		ProjectID:     projectID,
		StartedAt:     startedAt,
	}); err != nil {
		t.Fatalf("RecordAgentCallStart(%s): %v", callID, err)
	}
	if err := recorder.RecordAgentCallFinish(context.Background(), akashic.AgentCallResult{
		ID:         callID,
		SessionID:  sessionID,
		Success:    true,
		DurationMS: 100,
		FinishedAt: startedAt.Add(100 * time.Millisecond),
	}); err != nil {
		t.Fatalf("RecordAgentCallFinish(%s): %v", callID, err)
	}
}

func tailMatrixJob(agentCallID, workflowRunID, sessionID, providerID, stepID string, status akashic.LearningReflectionStatus, cwd, projectID string, queuedAt time.Time) akashic.LearningReflectionJob {
	return akashic.LearningReflectionJob{
		AgentCallID:   agentCallID,
		WorkflowRunID: workflowRunID,
		WorkflowID:    "matrix-workflow",
		WorkflowName:  "Matrix Workflow",
		StepID:        stepID,
		StepName:      "Step " + stepID,
		ProviderID:    providerID,
		CWD:           cwd,
		ProjectID:     projectID,
		SessionID:     sessionID,
		Status:        status,
		QueuedAt:      queuedAt,
		UpdatedAt:     queuedAt,
	}
}

func seedTailMatrixJob(t *testing.T, recorder *akashic.SQLiteRecorder, job akashic.LearningReflectionJob) {
	t.Helper()
	if _, err := recorder.RecordLearningReflectionJob(context.Background(), job); err != nil {
		t.Fatalf("RecordLearningReflectionJob(%s): %v", job.AgentCallID, err)
	}
}

func learningIDs(rows []LearningEntry) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func learningRowIDs(rows []akashic.LearningRow) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func tailMatrixHasArtifactBase(rows []akashic.ArtifactRow, base string) bool {
	for _, row := range rows {
		if filepath.Base(row.Path) == base {
			return true
		}
	}
	return false
}
