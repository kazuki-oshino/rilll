package learnings

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

func TestEarlyReflectorWorkerClaimsExecutesAndStoresResult(t *testing.T) {
	ctx := context.Background()
	recorder := newEarlyWorkerRecorder(t)
	base := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)
	cwd := t.TempDir()

	seedEarlyWorkerJob(t, recorder, akashic.LearningReflectionJob{
		AgentCallID:   "call-other-project",
		WorkflowRunID: "run-other",
		ProjectID:     "other-project",
		ProviderID:    "codex",
		SessionID:     "session-other",
		CWD:           cwd,
		QueuedAt:      base,
	})
	seedEarlyWorkerJob(t, recorder, akashic.LearningReflectionJob{
		AgentCallID:   "call-rilll",
		WorkflowRunID: "run-rilll",
		WorkflowID:    "workflow-a",
		StepID:        "step-a",
		ProjectID:     "project-rilll",
		ProviderID:    "codex",
		SessionID:     "session-rilll",
		CWD:           cwd,
		QueuedAt:      base.Add(time.Second),
	})

	agent := &fakeReflectorAgent{responses: map[string]provider.AgentResponse{
		"session-rilll": {Content: `{"learnings":[{"category":"tool-pattern","context":"ctx","insight":"ins","paths_glob":["internal/**"]}]}`},
	}}
	rt := newEarlyWorkerRuntime(t, recorder, agent, "project-rilll", cwd, base.Add(time.Minute))

	if !rt.processOne() {
		t.Fatal("processOne returned false, want claimed current-project job")
	}
	if got := len(agentCalls(agent)); got != 1 {
		t.Fatalf("CallAgent count=%d, want 1", got)
	}

	rows, err := recorder.JobsByAgentCallIDs(ctx, []string{"call-rilll", "call-other-project"})
	if err != nil {
		t.Fatalf("JobsByAgentCallIDs: %v", err)
	}
	byID := reflectionJobsByID(rows)
	current := byID["call-rilll"]
	if current.Status != akashic.LearningReflectionStatusSucceeded {
		t.Fatalf("current status=%q, want succeeded: %+v", current.Status, current)
	}
	if current.WorkerID != "worker-rilll" || current.AttemptCount != 1 {
		t.Fatalf("worker/attempt=(%q,%d), want (worker-rilll,1)", current.WorkerID, current.AttemptCount)
	}
	if current.StartedAt == nil || current.FinishedAt == nil {
		t.Fatalf("started_at/finished_at not set: %+v", current)
	}
	payload := decodeReflectionJobResultT(t, current.ResultJSON)
	if payload.AgentCallID != "call-rilll" || payload.SessionID != "session-rilll" || payload.ProviderID != "codex" {
		t.Fatalf("result provenance mismatch: %+v", payload)
	}
	if len(payload.Accepted) != 1 || payload.Accepted[0].Category != "tool-pattern" {
		t.Fatalf("accepted payload mismatch: %+v", payload.Accepted)
	}

	other := byID["call-other-project"]
	if other.Status != akashic.LearningReflectionStatusPending {
		t.Fatalf("cross-project status=%q, want pending", other.Status)
	}
}

func TestEarlyReflectorWorkerRetriesFailuresAndExhaustsAttempts(t *testing.T) {
	recorder := newEarlyWorkerRecorder(t)
	base := time.Date(2026, 5, 24, 10, 10, 0, 0, time.UTC)
	cwd := t.TempDir()

	seedEarlyWorkerJob(t, recorder, akashic.LearningReflectionJob{
		AgentCallID:   "call-retry",
		WorkflowRunID: "run-retry",
		ProjectID:     "project-rilll",
		ProviderID:    "codex",
		SessionID:     "session-retry",
		CWD:           cwd,
		MaxAttempts:   2,
		QueuedAt:      base,
	})
	agent := &fakeReflectorAgent{responses: map[string]provider.AgentResponse{
		"session-retry": {Content: "not json"},
	}}
	rt := newEarlyWorkerRuntime(t, recorder, agent, "project-rilll", cwd, base.Add(time.Minute))

	if !rt.processOne() {
		t.Fatal("first processOne returned false, want retryable failure claim")
	}
	row := onlyReflectionJob(t, recorder, "call-retry")
	if row.Status != akashic.LearningReflectionStatusPending || row.AttemptCount != 1 {
		t.Fatalf("retry row after first failure=%+v, want pending attempt 1", row)
	}
	if row.FailureReason != failureReasonParseJSON {
		t.Fatalf("failure reason=%q, want %q", row.FailureReason, failureReasonParseJSON)
	}
	if len(row.ResultJSON) == 0 {
		t.Fatal("retryable failure did not persist diagnostic result_json")
	}
	failedPayload := decodeReflectionJobResultT(t, row.ResultJSON)
	if len(failedPayload.Failures) != 1 || failedPayload.Failures[0].Reason != failureReasonParseJSON {
		t.Fatalf("failure payload=%+v, want parse-json failure", failedPayload.Failures)
	}

	agent.responses["session-retry"] = provider.AgentResponse{Content: `{"learnings":[]}`}
	if !rt.processOne() {
		t.Fatal("second processOne returned false, want retry claim")
	}
	row = onlyReflectionJob(t, recorder, "call-retry")
	if row.Status != akashic.LearningReflectionStatusSucceeded || row.AttemptCount != 2 {
		t.Fatalf("retry row after success=%+v, want succeeded attempt 2", row)
	}
	successPayload := decodeReflectionJobResultT(t, row.ResultJSON)
	if len(successPayload.Failures) != 0 {
		t.Fatalf("success payload failures=%+v, want none", successPayload.Failures)
	}

	seedEarlyWorkerJob(t, recorder, akashic.LearningReflectionJob{
		AgentCallID:   "call-exhausted",
		WorkflowRunID: "run-exhausted",
		ProjectID:     "project-rilll",
		ProviderID:    "codex",
		SessionID:     "session-exhausted",
		CWD:           cwd,
		MaxAttempts:   1,
		QueuedAt:      base.Add(2 * time.Second),
	})
	agent.responses["session-exhausted"] = provider.AgentResponse{Content: "not json"}
	if !rt.processOne() {
		t.Fatal("exhaustion processOne returned false, want failed claim")
	}
	exhausted := onlyReflectionJob(t, recorder, "call-exhausted")
	if exhausted.Status != akashic.LearningReflectionStatusFailed || exhausted.AttemptCount != 1 || exhausted.FinishedAt == nil {
		t.Fatalf("exhausted row=%+v, want failed attempt 1 with finished_at", exhausted)
	}
	if exhausted.FailureReason != failureReasonParseJSON || len(exhausted.ResultJSON) == 0 {
		t.Fatalf("exhausted failure reason/result_json=(%q,%s), want parse failure payload", exhausted.FailureReason, exhausted.ResultJSON)
	}

	if rt.processOne() {
		t.Fatal("processOne after exhausted returned true, want no claimable job")
	}
}

func TestEarlyReflectorWorkerDiscardsLateSuccessAfterStale(t *testing.T) {
	ctx := context.Background()
	recorder := newEarlyWorkerRecorder(t)
	base := time.Date(2026, 5, 24, 10, 20, 0, 0, time.UTC)
	cwd := t.TempDir()
	seedEarlyWorkerJob(t, recorder, akashic.LearningReflectionJob{
		AgentCallID:   "call-late",
		WorkflowRunID: "run-late",
		ProjectID:     "project-rilll",
		ProviderID:    "codex",
		SessionID:     "session-late",
		CWD:           cwd,
		QueuedAt:      base,
	})

	agent := newBlockingEarlyWorkerAgent(provider.AgentResponse{Content: `{"learnings":[{"category":"success-procedure","context":"late ctx","insight":"late ins","paths_glob":["docs/**"]}]}`})
	rt := newEarlyWorkerRuntime(t, recorder, agent, "project-rilll", cwd, base.Add(time.Minute))

	done := make(chan bool, 1)
	go func() {
		done <- rt.processOne()
	}()
	select {
	case <-agent.called:
	case <-time.After(time.Second):
		t.Fatal("worker did not enter CallAgent")
	}
	running := onlyReflectionJob(t, recorder, "call-late")
	if running.Status != akashic.LearningReflectionStatusRunning {
		t.Fatalf("row before stale=%+v, want running while CallAgent is in-flight", running)
	}
	if err := recorder.MarkLearningReflectionStale(ctx, []string{"call-late"}, "tail-timeout", base.Add(2*time.Minute)); err != nil {
		t.Fatalf("MarkLearningReflectionStale: %v", err)
	}
	close(agent.release)
	select {
	case claimed := <-done:
		if !claimed {
			t.Fatal("processOne returned false, want true for stale-discarded claim")
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not finish after release")
	}

	row := onlyReflectionJob(t, recorder, "call-late")
	if row.Status != akashic.LearningReflectionStatusStale {
		t.Fatalf("late success overwrote status: %+v", row)
	}
	if len(row.ResultJSON) != 0 {
		t.Fatalf("late success wrote result_json=%s, want empty", row.ResultJSON)
	}
	if row.FailureReason != "tail-timeout" {
		t.Fatalf("stale reason=%q, want tail-timeout", row.FailureReason)
	}
	if got := agent.callCount(); got != 1 {
		t.Fatalf("CallAgent count=%d, want 1", got)
	}
}

func TestEarlyReflectorWorkerTreatsClaimedProjectMismatchAsInvariantViolation(t *testing.T) {
	store := newRuntimeTestStore()
	store.enqueueClaim(akashic.LearningReflectionJob{
		AgentCallID:   "call-mismatch",
		WorkflowRunID: "run-1",
		ProviderID:    "codex",
		ProjectID:     "other-project",
		SessionID:     "session-1",
		Status:        akashic.LearningReflectionStatusRunning,
	})
	agent := &runtimeTestAgent{}
	rt := newEarlyWorkerRuntime(t, store, agent, "project-rilll", t.TempDir(), time.Date(2026, 5, 24, 10, 30, 0, 0, time.UTC))

	if !rt.processOne() {
		t.Fatal("processOne returned false, want claimed mismatch row")
	}
	if got := agent.callCount(); got != 0 {
		t.Fatalf("CallAgent count=%d, want 0 for invariant violation", got)
	}
	if got := store.failedCount(); got != 1 {
		t.Fatalf("failed count=%d, want 1", got)
	}
	if failure := store.lastFailure(); failure.Reason != "project-id-mismatch" || failure.Retryable {
		t.Fatalf("failure=%+v, want non-retryable project-id-mismatch", failure)
	}
}

func newEarlyWorkerRecorder(t *testing.T) *akashic.SQLiteRecorder {
	t.Helper()
	recorder, err := akashic.OpenSQLite(filepath.Join(t.TempDir(), "akashic.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	return recorder
}

func newEarlyWorkerRuntime(t *testing.T, store akashic.LearningReflectionJobStore, agent workflowctx.AgentCaller, projectID string, cwd string, now time.Time) *EarlyReflectorRuntime {
	t.Helper()
	return &EarlyReflectorRuntime{
		cwd:     cwd,
		store:   store,
		agent:   agent,
		config:  EarlyReflectorRuntimeConfig{ProjectID: projectID, WorkerID: "worker-rilll", MaxParallel: 1},
		ctx:     context.Background(),
		limiter: NewLimiter("", 1),
		now:     func() time.Time { return now },
	}
}

func seedEarlyWorkerJob(t *testing.T, recorder *akashic.SQLiteRecorder, job akashic.LearningReflectionJob) {
	t.Helper()
	if job.WorkflowID == "" {
		job.WorkflowID = "workflow-a"
	}
	if job.WorkflowName == "" {
		job.WorkflowName = "Workflow A"
	}
	if job.StepID == "" {
		job.StepID = "step-a"
	}
	if job.StepName == "" {
		job.StepName = "Step A"
	}
	if _, err := recorder.RecordLearningReflectionJob(context.Background(), job); err != nil {
		t.Fatalf("RecordLearningReflectionJob(%s): %v", job.AgentCallID, err)
	}
}

func onlyReflectionJob(t *testing.T, recorder *akashic.SQLiteRecorder, agentCallID string) akashic.LearningReflectionJob {
	t.Helper()
	rows, err := recorder.JobsByAgentCallIDs(context.Background(), []string{agentCallID})
	if err != nil {
		t.Fatalf("JobsByAgentCallIDs(%s): %v", agentCallID, err)
	}
	if len(rows) != 1 {
		t.Fatalf("JobsByAgentCallIDs(%s) len=%d, want 1", agentCallID, len(rows))
	}
	return rows[0]
}

func reflectionJobsByID(rows []akashic.LearningReflectionJob) map[string]akashic.LearningReflectionJob {
	out := make(map[string]akashic.LearningReflectionJob, len(rows))
	for _, row := range rows {
		out[row.AgentCallID] = row
	}
	return out
}

func agentCalls(agent *fakeReflectorAgent) []string {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return append([]string(nil), agent.calls...)
}

type blockingEarlyWorkerAgent struct {
	response provider.AgentResponse
	err      error
	called   chan struct{}
	release  chan struct{}

	mu    sync.Mutex
	calls int
}

func newBlockingEarlyWorkerAgent(response provider.AgentResponse) *blockingEarlyWorkerAgent {
	return &blockingEarlyWorkerAgent{
		response: response,
		called:   make(chan struct{}),
		release:  make(chan struct{}),
	}
}

func (a *blockingEarlyWorkerAgent) CallAgent(ctx context.Context, _ string, _ string, _ provider.AgentEventCallback, _ string, _ *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
	a.mu.Lock()
	a.calls++
	if a.calls == 1 {
		close(a.called)
	}
	a.mu.Unlock()

	select {
	case <-a.release:
	case <-ctx.Done():
		return provider.AgentResponse{}, ctx.Err()
	}
	if a.err != nil {
		return provider.AgentResponse{}, a.err
	}
	return a.response, nil
}

func (a *blockingEarlyWorkerAgent) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}
