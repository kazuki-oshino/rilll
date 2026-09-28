package akashic

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestRecordLearningReflectionJobEnqueueAndDuplicateAreIdempotent(t *testing.T) {
	ctx := context.Background()
	recorder := newTestRecorder(t)
	now := time.Date(2026, 5, 24, 9, 0, 0, 0, time.UTC)

	first, err := recorder.RecordLearningReflectionJob(ctx, LearningReflectionJob{
		AgentCallID:   "call-1",
		WorkflowRunID: "run-1",
		WorkflowID:    "workflow-a",
		WorkflowName:  "Workflow A",
		StepID:        "step-a",
		StepName:      "Step A",
		ProviderID:    "claude",
		CWD:           "/repo/rilll",
		ProjectID:     "rilll",
		SessionID:     "session-1",
		QueuedAt:      now,
	})
	if err != nil {
		t.Fatalf("RecordLearningReflectionJob first: %v", err)
	}
	if !first.Inserted {
		t.Fatal("first enqueue Inserted=false, want true")
	}
	if first.Job.Status != LearningReflectionStatusPending {
		t.Fatalf("first status = %q, want %q", first.Job.Status, LearningReflectionStatusPending)
	}

	duplicate, err := recorder.RecordLearningReflectionJob(ctx, LearningReflectionJob{
		AgentCallID:   "call-1",
		WorkflowRunID: "run-overwrite",
		WorkflowID:    "workflow-overwrite",
		WorkflowName:  "Workflow Overwrite",
		StepID:        "step-overwrite",
		StepName:      "Step Overwrite",
		ProviderID:    "codex",
		CWD:           "/repo/other",
		ProjectID:     "other",
		SessionID:     "session-overwrite",
		QueuedAt:      now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("RecordLearningReflectionJob duplicate: %v", err)
	}
	if duplicate.Inserted {
		t.Fatal("duplicate enqueue Inserted=true, want false")
	}

	rows, err := recorder.JobsByAgentCallIDs(ctx, []string{"call-1"})
	if err != nil {
		t.Fatalf("JobsByAgentCallIDs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("JobsByAgentCallIDs len=%d, want 1", len(rows))
	}
	got := rows[0]
	if got.WorkflowRunID != "run-1" || got.ProviderID != "claude" || got.ProjectID != "rilll" || got.SessionID != "session-1" {
		t.Fatalf("duplicate changed immutable enqueue fields: %+v", got)
	}
	if got.AttemptCount != 0 || got.MaxAttempts != 2 {
		t.Fatalf("attempt defaults = (%d,%d), want (0,2)", got.AttemptCount, got.MaxAttempts)
	}
	if !got.QueuedAt.Equal(now) || !got.UpdatedAt.Equal(now) {
		t.Fatalf("enqueue timestamps = queued %s updated %s, want %s", got.QueuedAt, got.UpdatedAt, now)
	}
}

func TestRecordLearningReflectionJobRecordsSkipReasons(t *testing.T) {
	ctx := context.Background()
	recorder := newTestRecorder(t)
	now := time.Date(2026, 5, 24, 9, 10, 0, 0, time.UTC)

	cases := []struct {
		name       string
		job        LearningReflectionJob
		wantReason LearningReflectionSkipReason
	}{
		{
			name: "empty session",
			job: LearningReflectionJob{
				AgentCallID:   "call-empty-session",
				WorkflowRunID: "run-1",
				WorkflowID:    "workflow-a",
				WorkflowName:  "Workflow A",
				StepID:        "step-a",
				StepName:      "Step A",
				ProviderID:    "claude",
				CWD:           "/repo/rilll",
				ProjectID:     "rilll",
				QueuedAt:      now,
			},
			wantReason: LearningReflectionSkipEmptySessionID,
		},
		{
			name: "provider denied",
			job: LearningReflectionJob{
				AgentCallID:   "call-provider-denied",
				WorkflowRunID: "run-1",
				WorkflowID:    "workflow-a",
				WorkflowName:  "Workflow A",
				StepID:        "step-a",
				StepName:      "Step A",
				ProviderID:    "cursor",
				CWD:           "/repo/rilll",
				ProjectID:     "rilll",
				SessionID:     "session-1",
				SkipReason:    LearningReflectionSkipProviderNotInAllowlist,
				QueuedAt:      now.Add(time.Second),
			},
			wantReason: LearningReflectionSkipProviderNotInAllowlist,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := recorder.RecordLearningReflectionJob(ctx, tc.job)
			if err != nil {
				t.Fatalf("RecordLearningReflectionJob: %v", err)
			}
			if !result.Inserted {
				t.Fatal("Inserted=false, want true")
			}
			if result.Job.Status != LearningReflectionStatusSkipped {
				t.Fatalf("status=%q, want skipped", result.Job.Status)
			}
			if result.Job.SkipReason != tc.wantReason {
				t.Fatalf("skip_reason=%q, want %q", result.Job.SkipReason, tc.wantReason)
			}
		})
	}
}

func TestClaimLearningReflectionJobScopesByProjectAndRetriesExpiredLease(t *testing.T) {
	ctx := context.Background()
	recorder := newTestRecorder(t)
	base := time.Date(2026, 5, 24, 9, 20, 0, 0, time.UTC)

	for _, job := range []LearningReflectionJob{
		testReflectionJob("call-other-project", "run-other", "other", "session-other", base),
		testReflectionJob("call-rilll", "run-rilll", "rilll", "session-rilll", base.Add(time.Second)),
	} {
		if _, err := recorder.RecordLearningReflectionJob(ctx, job); err != nil {
			t.Fatalf("RecordLearningReflectionJob(%s): %v", job.AgentCallID, err)
		}
	}

	claimed, ok, err := recorder.ClaimLearningReflectionJob(ctx, "rilll", "worker-a", base.Add(time.Minute))
	if err != nil {
		t.Fatalf("ClaimLearningReflectionJob: %v", err)
	}
	if !ok {
		t.Fatal("ClaimLearningReflectionJob ok=false, want true")
	}
	if claimed.AgentCallID != "call-rilll" || claimed.ProjectID != "rilll" {
		t.Fatalf("claimed job = %+v, want call-rilll in project rilll", claimed)
	}
	if claimed.Status != LearningReflectionStatusRunning || claimed.WorkerID != "worker-a" || claimed.AttemptCount != 1 {
		t.Fatalf("claimed transition = %+v, want running worker-a attempt 1", claimed)
	}
	if claimed.LeaseUntil == nil || !claimed.LeaseUntil.After(base.Add(time.Minute)) {
		t.Fatalf("lease_until=%v, want after claim time", claimed.LeaseUntil)
	}

	otherRows, err := recorder.JobsByAgentCallIDs(ctx, []string{"call-other-project"})
	if err != nil {
		t.Fatalf("JobsByAgentCallIDs other: %v", err)
	}
	if otherRows[0].Status != LearningReflectionStatusPending {
		t.Fatalf("cross-project job status=%q, want pending", otherRows[0].Status)
	}

	reclaimAt := base.Add(10 * time.Minute)
	reclaimed, ok, err := recorder.ClaimLearningReflectionJob(ctx, "rilll", "worker-b", reclaimAt)
	if err != nil {
		t.Fatalf("ClaimLearningReflectionJob expired lease: %v", err)
	}
	if !ok {
		t.Fatal("ClaimLearningReflectionJob expired lease ok=false, want true")
	}
	if reclaimed.AgentCallID != "call-rilll" || reclaimed.WorkerID != "worker-b" || reclaimed.AttemptCount != 2 {
		t.Fatalf("reclaimed job = %+v, want same job worker-b attempt 2", reclaimed)
	}
}

func TestLearningReflectionJobTerminalTransitionsAndStaleCAS(t *testing.T) {
	ctx := context.Background()
	recorder := newTestRecorder(t)
	base := time.Date(2026, 5, 24, 9, 30, 0, 0, time.UTC)

	if _, err := recorder.RecordLearningReflectionJob(ctx, testReflectionJob("call-success", "run-1", "rilll", "session-1", base)); err != nil {
		t.Fatalf("RecordLearningReflectionJob success: %v", err)
	}
	if _, ok, err := recorder.ClaimLearningReflectionJob(ctx, "rilll", "worker-a", base.Add(time.Second)); err != nil || !ok {
		t.Fatalf("Claim success job ok=%v err=%v", ok, err)
	}
	resultJSON := json.RawMessage(`{"accepted":[{"id":"learning-1"}],"dropped":[],"failures":[]}`)
	if err := recorder.MarkLearningReflectionSucceeded(ctx, "call-success", LearningReflectionResult{JSON: resultJSON}, base.Add(2*time.Second)); err != nil {
		t.Fatalf("MarkLearningReflectionSucceeded: %v", err)
	}
	if err := recorder.MarkLearningReflectionAggregated(ctx, []string{"call-success"}, "tail-run-1", base.Add(3*time.Second)); err != nil {
		t.Fatalf("MarkLearningReflectionAggregated: %v", err)
	}
	successRows, err := recorder.JobsByAgentCallIDs(ctx, []string{"call-success"})
	if err != nil {
		t.Fatalf("JobsByAgentCallIDs success: %v", err)
	}
	success := successRows[0]
	if success.Status != LearningReflectionStatusAggregated || success.AggregatedTailRunID != "tail-run-1" {
		t.Fatalf("success aggregate = %+v, want aggregated tail-run-1", success)
	}
	if string(success.ResultJSON) != string(resultJSON) {
		t.Fatalf("result_json=%s, want %s", success.ResultJSON, resultJSON)
	}

	if _, err := recorder.RecordLearningReflectionJob(ctx, testReflectionJob("call-stale", "run-1", "rilll", "session-stale", base)); err != nil {
		t.Fatalf("RecordLearningReflectionJob stale: %v", err)
	}
	if _, ok, err := recorder.ClaimLearningReflectionJob(ctx, "rilll", "worker-a", base.Add(time.Second)); err != nil || !ok {
		t.Fatalf("Claim stale job ok=%v err=%v", ok, err)
	}
	if err := recorder.MarkLearningReflectionStale(ctx, []string{"call-stale"}, "tail-timeout", base.Add(4*time.Second)); err != nil {
		t.Fatalf("MarkLearningReflectionStale: %v", err)
	}
	if err := recorder.MarkLearningReflectionSucceeded(ctx, "call-stale", LearningReflectionResult{JSON: json.RawMessage(`{"accepted":["late"]}`)}, base.Add(5*time.Second)); err != nil {
		t.Fatalf("late MarkLearningReflectionSucceeded: %v", err)
	}
	staleRows, err := recorder.JobsByAgentCallIDs(ctx, []string{"call-stale"})
	if err != nil {
		t.Fatalf("JobsByAgentCallIDs stale: %v", err)
	}
	stale := staleRows[0]
	if stale.Status != LearningReflectionStatusStale {
		t.Fatalf("late success overwrote stale status: %+v", stale)
	}
	if len(stale.ResultJSON) != 0 {
		t.Fatalf("late success wrote result_json=%s, want empty", stale.ResultJSON)
	}
	if stale.FailureReason != "tail-timeout" {
		t.Fatalf("stale reason=%q, want tail-timeout", stale.FailureReason)
	}
}

func TestMarkLearningReflectionFailedRetriesUntilAttemptsExhausted(t *testing.T) {
	ctx := context.Background()
	recorder := newTestRecorder(t)
	base := time.Date(2026, 5, 24, 9, 35, 0, 0, time.UTC)

	if _, err := recorder.RecordLearningReflectionJob(ctx, testReflectionJob("call-retry", "run-1", "rilll", "session-retry", base)); err != nil {
		t.Fatalf("RecordLearningReflectionJob: %v", err)
	}
	if _, ok, err := recorder.ClaimLearningReflectionJob(ctx, "rilll", "worker-a", base.Add(time.Second)); err != nil || !ok {
		t.Fatalf("first claim ok=%v err=%v", ok, err)
	}
	if err := recorder.MarkLearningReflectionFailed(ctx, "call-retry", LearningReflectionFailure{
		Reason:     "provider-temporary-error",
		Detail:     "temporary outage",
		Retryable:  true,
		ResultJSON: json.RawMessage(`{"failures":[{"reason":"provider-temporary-error","detail":"temporary outage"}]}`),
	}, base.Add(2*time.Second)); err != nil {
		t.Fatalf("first MarkLearningReflectionFailed: %v", err)
	}
	rows, err := recorder.JobsByAgentCallIDs(ctx, []string{"call-retry"})
	if err != nil {
		t.Fatalf("JobsByAgentCallIDs after retryable fail: %v", err)
	}
	if rows[0].Status != LearningReflectionStatusPending || rows[0].AttemptCount != 1 || rows[0].FinishedAt != nil {
		t.Fatalf("retryable failure row=%+v, want pending attempt 1 without finished_at", rows[0])
	}
	if string(rows[0].ResultJSON) != `{"failures":[{"reason":"provider-temporary-error","detail":"temporary outage"}]}` {
		t.Fatalf("retryable failure result_json=%s, want diagnostic payload", rows[0].ResultJSON)
	}

	if _, ok, err := recorder.ClaimLearningReflectionJob(ctx, "rilll", "worker-b", base.Add(3*time.Second)); err != nil || !ok {
		t.Fatalf("second claim ok=%v err=%v", ok, err)
	}
	if err := recorder.MarkLearningReflectionFailed(ctx, "call-retry", LearningReflectionFailure{
		Reason:     "provider-temporary-error",
		Detail:     "still down",
		Retryable:  true,
		ResultJSON: json.RawMessage(`{"failures":[{"reason":"provider-temporary-error","detail":"still down"}]}`),
	}, base.Add(4*time.Second)); err != nil {
		t.Fatalf("second MarkLearningReflectionFailed: %v", err)
	}
	rows, err = recorder.JobsByAgentCallIDs(ctx, []string{"call-retry"})
	if err != nil {
		t.Fatalf("JobsByAgentCallIDs after exhausted fail: %v", err)
	}
	exhausted := rows[0]
	if exhausted.Status != LearningReflectionStatusFailed || exhausted.AttemptCount != 2 || exhausted.FinishedAt == nil {
		t.Fatalf("exhausted failure row=%+v, want failed attempt 2 with finished_at", exhausted)
	}
	if exhausted.FailureReason != "provider-temporary-error" || exhausted.FailureDetail != "still down" {
		t.Fatalf("failure fields=(%q,%q), want final error detail", exhausted.FailureReason, exhausted.FailureDetail)
	}
	if string(exhausted.ResultJSON) != `{"failures":[{"reason":"provider-temporary-error","detail":"still down"}]}` {
		t.Fatalf("exhausted failure result_json=%s, want final diagnostic payload", exhausted.ResultJSON)
	}
	if _, ok, err := recorder.ClaimLearningReflectionJob(ctx, "rilll", "worker-c", base.Add(5*time.Second)); err != nil || ok {
		t.Fatalf("claim after exhausted ok=%v err=%v, want ok=false nil", ok, err)
	}
}

func TestJobsByAgentCallIDsInputOrderAndWorkflowRunTreeReader(t *testing.T) {
	ctx := context.Background()
	recorder := newTestRecorder(t)
	base := time.Date(2026, 5, 24, 9, 40, 0, 0, time.UTC)

	for _, run := range []WorkflowRun{
		{ID: "parent-run", WorkflowID: "parent", WorkflowName: "Parent", CWD: "/repo/rilll", ProjectID: "rilll", StartedAt: base},
		{ID: "child-run", WorkflowID: "child", WorkflowName: "Child", CWD: "/repo/rilll", ProjectID: "rilll", StartedAt: base.Add(time.Minute)},
		{ID: "grandchild-run", WorkflowID: "grandchild", WorkflowName: "Grandchild", CWD: "/repo/rilll", ProjectID: "rilll", StartedAt: base.Add(2 * time.Minute)},
		{ID: "unrelated-run", WorkflowID: "other", WorkflowName: "Other", CWD: "/repo/rilll", ProjectID: "rilll", StartedAt: base.Add(3 * time.Minute)},
	} {
		if err := recorder.RecordWorkflowRunStart(ctx, run); err != nil {
			t.Fatalf("RecordWorkflowRunStart(%s): %v", run.ID, err)
		}
	}
	for _, edge := range []WorkflowRunChild{
		{ParentRunID: "parent-run", ChildRunID: "child-run", ChildWorkflowID: "child", ChildWorkflowName: "Child", RecordedAt: base.Add(time.Second)},
		{ParentRunID: "child-run", ChildRunID: "grandchild-run", ChildWorkflowID: "grandchild", ChildWorkflowName: "Grandchild", RecordedAt: base.Add(2 * time.Second)},
	} {
		if err := recorder.RecordWorkflowRunChild(ctx, edge); err != nil {
			t.Fatalf("RecordWorkflowRunChild(%s -> %s): %v", edge.ParentRunID, edge.ChildRunID, err)
		}
	}

	for i, job := range []LearningReflectionJob{
		testReflectionJob("call-parent", "parent-run", "rilll", "session-parent", base.Add(time.Second)),
		testReflectionJob("call-child", "child-run", "rilll", "session-child", base.Add(2*time.Second)),
		testReflectionJob("call-grandchild", "grandchild-run", "rilll", "session-grandchild", base.Add(3*time.Second)),
		testReflectionJob("call-unrelated", "unrelated-run", "rilll", "session-unrelated", base.Add(4*time.Second)),
	} {
		job.StepID = "step-" + string(rune('a'+i))
		if _, err := recorder.RecordLearningReflectionJob(ctx, job); err != nil {
			t.Fatalf("RecordLearningReflectionJob(%s): %v", job.AgentCallID, err)
		}
	}

	ordered, err := recorder.JobsByAgentCallIDs(ctx, []string{"call-grandchild", "call-parent", "missing", "call-child"})
	if err != nil {
		t.Fatalf("JobsByAgentCallIDs ordered: %v", err)
	}
	gotOrdered := agentCallIDsFromReflectionJobs(ordered)
	wantOrdered := []string{"call-grandchild", "call-parent", "call-child"}
	if !reflect.DeepEqual(gotOrdered, wantOrdered) {
		t.Fatalf("JobsByAgentCallIDs order=%v, want %v", gotOrdered, wantOrdered)
	}

	treeRows, err := recorder.LearningReflectionJobsByWorkflowRunTree(ctx, "parent-run")
	if err != nil {
		t.Fatalf("LearningReflectionJobsByWorkflowRunTree: %v", err)
	}
	gotTree := agentCallIDsFromReflectionJobs(treeRows)
	wantTree := []string{"call-parent", "call-child", "call-grandchild"}
	if !reflect.DeepEqual(gotTree, wantTree) {
		t.Fatalf("LearningReflectionJobsByWorkflowRunTree=%v, want %v", gotTree, wantTree)
	}
}

func testReflectionJob(agentCallID, workflowRunID, projectID, sessionID string, queuedAt time.Time) LearningReflectionJob {
	return LearningReflectionJob{
		AgentCallID:   agentCallID,
		WorkflowRunID: workflowRunID,
		WorkflowID:    "workflow-a",
		WorkflowName:  "Workflow A",
		StepID:        "step-a",
		StepName:      "Step A",
		ProviderID:    "claude",
		CWD:           "/repo/" + projectID,
		ProjectID:     projectID,
		SessionID:     sessionID,
		QueuedAt:      queuedAt,
	}
}

func agentCallIDsFromReflectionJobs(rows []LearningReflectionJob) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.AgentCallID)
	}
	return out
}
