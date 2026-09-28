package learnings

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

func TestEarlyReflectionIntegrationMatrixWorkerRuntimeWakeProjectIsolationAndAllowlistSnapshot(t *testing.T) {
	t.Setenv(envRillLearningsProviders, "codex")

	ctx := context.Background()
	cwd := t.TempDir()
	recorder := openEarlyIntegrationRecorder(t)
	store := &earlyIntegrationCountingJobStore{SQLiteRecorder: recorder}
	agent := &earlyWorkerMatrixAgent{outcomes: map[string][]earlyWorkerMatrixOutcome{
		"session-current": {
			{response: provider.AgentResponse{Content: `{"learnings":[{"category":"tool-pattern","context":"ctx","insight":"ins","paths_glob":["internal/**"]}]}`}},
		},
		"session-other": {
			{response: provider.AgentResponse{Content: `{"learnings":[{"category":"antipattern","context":"other","insight":"other","paths_glob":["other/**"]}]}`}},
		},
	}}
	rt, err := StartEarlyReflectorRuntime(ctx, cwd, store, agent, EarlyReflectorRuntimeConfig{
		ProjectID:    "project-rilll",
		WorkerID:     "worker-rilll",
		MaxParallel:  1,
		PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("StartEarlyReflectorRuntime: %v", err)
	}
	defer rt.Shutdown()
	waitUntilRuntimeTest(t, time.Second, func() bool { return store.claimCount() >= 1 })

	base := time.Date(2026, 5, 24, 11, 0, 0, 0, time.UTC)
	seedEarlyWorkerJob(t, recorder, akashic.LearningReflectionJob{
		AgentCallID:   "call-current",
		WorkflowRunID: "run-current",
		ProjectID:     "project-rilll",
		ProviderID:    "codex",
		SessionID:     "session-current",
		CWD:           cwd,
		QueuedAt:      base,
	})
	seedEarlyWorkerJob(t, recorder, akashic.LearningReflectionJob{
		AgentCallID:   "call-other",
		WorkflowRunID: "run-other",
		ProjectID:     "other-project",
		ProviderID:    "codex",
		SessionID:     "session-other",
		CWD:           cwd,
		QueuedAt:      base.Add(time.Second),
	})

	t.Setenv(envRillLearningsProviders, "cursor")
	beforeWakeClaims := store.claimCount()
	rt.Notify()

	var jobs map[string]akashic.LearningReflectionJob
	waitUntilRuntimeTest(t, time.Second, func() bool {
		jobs = readEarlyWorkerJobsByID(t, recorder, []string{"call-current", "call-other"})
		return jobs["call-current"].Status == akashic.LearningReflectionStatusSucceeded
	})
	if store.claimCount() <= beforeWakeClaims {
		t.Fatalf("worker did not claim after wake; before=%d after=%d", beforeWakeClaims, store.claimCount())
	}

	current := jobs["call-current"]
	if current.WorkerID != "worker-rilll" || current.AttemptCount != 1 {
		t.Fatalf("current worker/attempt=(%q,%d), want (worker-rilll,1): %+v", current.WorkerID, current.AttemptCount, current)
	}
	payload := decodeReflectionJobResultT(t, current.ResultJSON)
	if payload.ProviderID != "codex" || payload.AgentCallID != "call-current" {
		t.Fatalf("current result provenance=%+v, want codex call-current", payload)
	}
	if len(payload.Accepted) != 1 || payload.Accepted[0].Category != "tool-pattern" {
		t.Fatalf("current accepted payload=%+v, want one tool-pattern learning", payload.Accepted)
	}
	if got := agent.callCountFor("session-current"); got != 1 {
		t.Fatalf("session-current CallAgent count=%d, want 1", got)
	}

	other := jobs["call-other"]
	if other.Status != akashic.LearningReflectionStatusPending || other.AttemptCount != 0 || other.WorkerID != "" {
		t.Fatalf("cross-project job was touched: %+v", other)
	}
	if got := agent.callCountFor("session-other"); got != 0 {
		t.Fatalf("cross-project session CallAgent count=%d, want 0", got)
	}
}

func TestEarlyReflectionIntegrationMatrixWorkerRuntimePeriodicFailureRetryAndTerminalStates(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	recorder := openEarlyIntegrationRecorder(t)
	store := &earlyIntegrationCountingJobStore{SQLiteRecorder: recorder}
	agent := &earlyWorkerMatrixAgent{outcomes: map[string][]earlyWorkerMatrixOutcome{
		"session-no-credentials": {
			{err: errors.New("provider auth: no credentials configured")},
		},
		"session-retry-success": {
			{response: provider.AgentResponse{Content: "not json"}},
			{response: provider.AgentResponse{Content: `{"learnings":[]}`}},
		},
		"session-exhausted": {
			{err: errors.New("provider temporary outage")},
			{err: errors.New("provider temporary outage")},
		},
	}}
	rt, err := StartEarlyReflectorRuntime(ctx, cwd, store, agent, EarlyReflectorRuntimeConfig{
		ProjectID:    "project-rilll",
		WorkerID:     "worker-rilll",
		MaxParallel:  1,
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("StartEarlyReflectorRuntime: %v", err)
	}
	defer rt.Shutdown()
	waitUntilRuntimeTest(t, time.Second, func() bool { return store.claimCount() >= 1 })

	base := time.Date(2026, 5, 24, 11, 10, 0, 0, time.UTC)
	seedEarlyWorkerJob(t, recorder, akashic.LearningReflectionJob{
		AgentCallID:   "call-no-credentials",
		WorkflowRunID: "run-failure",
		ProjectID:     "project-rilll",
		ProviderID:    "codex",
		SessionID:     "session-no-credentials",
		CWD:           cwd,
		QueuedAt:      base,
	})
	seedEarlyWorkerJob(t, recorder, akashic.LearningReflectionJob{
		AgentCallID:   "call-retry-success",
		WorkflowRunID: "run-failure",
		ProjectID:     "project-rilll",
		ProviderID:    "codex",
		SessionID:     "session-retry-success",
		CWD:           cwd,
		MaxAttempts:   2,
		QueuedAt:      base.Add(time.Second),
	})
	seedEarlyWorkerJob(t, recorder, akashic.LearningReflectionJob{
		AgentCallID:   "call-exhausted",
		WorkflowRunID: "run-failure",
		ProjectID:     "project-rilll",
		ProviderID:    "codex",
		SessionID:     "session-exhausted",
		CWD:           cwd,
		MaxAttempts:   2,
		QueuedAt:      base.Add(2 * time.Second),
	})
	seedEarlyWorkerJob(t, recorder, akashic.LearningReflectionJob{
		AgentCallID:   "call-terminal-provider",
		WorkflowRunID: "run-failure",
		ProjectID:     "project-rilll",
		SessionID:     "session-terminal-provider",
		CWD:           cwd,
		MaxAttempts:   2,
		QueuedAt:      base.Add(3 * time.Second),
	})

	var jobs map[string]akashic.LearningReflectionJob
	waitUntilRuntimeTest(t, 2*time.Second, func() bool {
		jobs = readEarlyWorkerJobsByID(t, recorder, []string{
			"call-no-credentials",
			"call-retry-success",
			"call-exhausted",
			"call-terminal-provider",
		})
		return jobs["call-no-credentials"].Status == akashic.LearningReflectionStatusFailed &&
			jobs["call-retry-success"].Status == akashic.LearningReflectionStatusSucceeded &&
			jobs["call-exhausted"].Status == akashic.LearningReflectionStatusFailed &&
			jobs["call-terminal-provider"].Status == akashic.LearningReflectionStatusFailed
	})

	noCredentials := jobs["call-no-credentials"]
	if noCredentials.AttemptCount != 1 || noCredentials.FailureReason != failureReasonNoCredentials {
		t.Fatalf("no credentials row=%+v, want failed attempt 1 reason %q", noCredentials, failureReasonNoCredentials)
	}
	assertEarlyWorkerFailurePayload(t, noCredentials, failureReasonNoCredentials)
	if got := agent.callCountFor("session-no-credentials"); got != 1 {
		t.Fatalf("session-no-credentials CallAgent count=%d, want 1", got)
	}

	retrySuccess := jobs["call-retry-success"]
	if retrySuccess.AttemptCount != 2 || retrySuccess.FailureReason != "" {
		t.Fatalf("retry-success row=%+v, want succeeded on second attempt with cleared failure", retrySuccess)
	}
	if got := agent.callCountFor("session-retry-success"); got != 2 {
		t.Fatalf("session-retry-success CallAgent count=%d, want 2", got)
	}

	exhausted := jobs["call-exhausted"]
	if exhausted.AttemptCount != 2 || exhausted.FailureReason != failureReasonCallAgent || exhausted.FinishedAt == nil {
		t.Fatalf("exhausted row=%+v, want failed attempt 2 reason %q", exhausted, failureReasonCallAgent)
	}
	assertEarlyWorkerFailurePayload(t, exhausted, failureReasonCallAgent)
	if got := agent.callCountFor("session-exhausted"); got != 2 {
		t.Fatalf("session-exhausted CallAgent count=%d, want 2", got)
	}

	terminalProvider := jobs["call-terminal-provider"]
	if terminalProvider.AttemptCount != 1 || terminalProvider.FailureReason != "provider-id-empty" {
		t.Fatalf("terminal provider row=%+v, want failed attempt 1 reason provider-id-empty", terminalProvider)
	}
	assertEarlyWorkerFailurePayload(t, terminalProvider, "provider-id-empty")
	if got := agent.callCountFor("session-terminal-provider"); got != 0 {
		t.Fatalf("terminal provider validation should not call agent; got %d calls", got)
	}
}

type earlyIntegrationCountingJobStore struct {
	*akashic.SQLiteRecorder
	claims atomic.Int32
}

func (s *earlyIntegrationCountingJobStore) ClaimLearningReflectionJob(ctx context.Context, projectID string, workerID string, now time.Time) (akashic.LearningReflectionJob, bool, error) {
	s.claims.Add(1)
	return s.SQLiteRecorder.ClaimLearningReflectionJob(ctx, projectID, workerID, now)
}

func (s *earlyIntegrationCountingJobStore) claimCount() int32 {
	return s.claims.Load()
}

type earlyWorkerMatrixOutcome struct {
	response provider.AgentResponse
	err      error
}

type earlyWorkerMatrixAgent struct {
	mu       sync.Mutex
	outcomes map[string][]earlyWorkerMatrixOutcome
	calls    []string
}

func (a *earlyWorkerMatrixAgent) CallAgent(ctx context.Context, _ string, _ string, _ provider.AgentEventCallback, resumeSessionID string, _ *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
	select {
	case <-ctx.Done():
		return provider.AgentResponse{}, ctx.Err()
	default:
	}

	a.mu.Lock()
	a.calls = append(a.calls, resumeSessionID)
	outcomes := a.outcomes[resumeSessionID]
	if len(outcomes) == 0 {
		a.mu.Unlock()
		return provider.AgentResponse{}, fmt.Errorf("unexpected CallAgent resume session %q", resumeSessionID)
	}
	outcome := outcomes[0]
	a.outcomes[resumeSessionID] = outcomes[1:]
	a.mu.Unlock()

	if outcome.err != nil {
		return provider.AgentResponse{}, outcome.err
	}
	return outcome.response, nil
}

func (a *earlyWorkerMatrixAgent) callCountFor(sessionID string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	var count int
	for _, got := range a.calls {
		if got == sessionID {
			count++
		}
	}
	return count
}

func readEarlyWorkerJobsByID(t *testing.T, recorder *akashic.SQLiteRecorder, ids []string) map[string]akashic.LearningReflectionJob {
	t.Helper()
	rows, err := recorder.JobsByAgentCallIDs(context.Background(), ids)
	if err != nil {
		t.Fatalf("JobsByAgentCallIDs(%v): %v", ids, err)
	}
	return reflectionJobsByID(rows)
}

func assertEarlyWorkerFailurePayload(t *testing.T, job akashic.LearningReflectionJob, reason string) {
	t.Helper()
	payload := decodeReflectionJobResultT(t, job.ResultJSON)
	if len(payload.Failures) != 1 {
		t.Fatalf("job %s failures len=%d, want 1; payload=%s", job.AgentCallID, len(payload.Failures), job.ResultJSON)
	}
	if payload.Failures[0].Reason != reason {
		t.Fatalf("job %s failure reason=%q, want %q", job.AgentCallID, payload.Failures[0].Reason, reason)
	}
}
