package learnings

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

func TestEarlyReflectorRuntimeStartRequiresStoreAndProject(t *testing.T) {
	store := newRuntimeTestStore()
	agent := &runtimeTestAgent{recorder: store}
	rt, started, err := StartWorkflowScopedEarlyReflectorRuntime(context.Background(), t.TempDir(), agent, EarlyReflectorRuntimeConfig{
		WorkerID:     "worker-1",
		PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("StartWorkflowScopedEarlyReflectorRuntime valid store: %v", err)
	}
	if !started || rt == nil {
		t.Fatalf("runtime not started for valid job store and project")
	}
	rt.Shutdown()

	noopAgent := &runtimeTestAgent{recorder: akashic.NoopRecorder{}}
	rt, started, err = StartWorkflowScopedEarlyReflectorRuntime(context.Background(), t.TempDir(), noopAgent, EarlyReflectorRuntimeConfig{
		WorkerID:     "worker-1",
		PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("StartWorkflowScopedEarlyReflectorRuntime noop recorder: %v", err)
	}
	if started || rt != nil {
		t.Fatalf("runtime started for NoopRecorder; started=%v rt=%v", started, rt)
	}

	rt, started, err = StartWorkflowScopedEarlyReflectorRuntime(context.Background(), "/", agent, EarlyReflectorRuntimeConfig{
		WorkerID:     "worker-1",
		PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("StartWorkflowScopedEarlyReflectorRuntime unknown project: %v", err)
	}
	if started || rt != nil {
		t.Fatalf("runtime started for unknown project; started=%v rt=%v", started, rt)
	}
}

func TestEarlyReflectorRuntimeConfigUsesMaxParallelEnv(t *testing.T) {
	t.Setenv(envRillLearningsMaxParallel, "2")
	store := newRuntimeTestStore()
	agent := &runtimeTestAgent{recorder: store}

	rt, err := StartEarlyReflectorRuntime(context.Background(), t.TempDir(), store, agent, EarlyReflectorRuntimeConfig{
		ProjectID:    "project-1",
		WorkerID:     "worker-1",
		PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("StartEarlyReflectorRuntime: %v", err)
	}
	defer rt.Shutdown()

	if got := rt.MaxParallel(); got != 2 {
		t.Fatalf("MaxParallel() = %d, want env-derived 2", got)
	}
}

func TestEarlyReflectorRuntimeWakePollsWithoutBlocking(t *testing.T) {
	store := newRuntimeTestStore()
	agent := &runtimeTestAgent{recorder: store}
	rt, err := StartEarlyReflectorRuntime(context.Background(), t.TempDir(), store, agent, EarlyReflectorRuntimeConfig{
		ProjectID:    "project-1",
		WorkerID:     "worker-1",
		MaxParallel:  1,
		PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("StartEarlyReflectorRuntime: %v", err)
	}
	defer rt.Shutdown()
	waitUntilRuntimeTest(t, time.Second, func() bool { return store.claimCount() > 0 })
	before := store.claimCount()

	for i := 0; i < 100; i++ {
		rt.Notify()
	}
	waitUntilRuntimeTest(t, time.Second, func() bool { return store.claimCount() > before })
}

func TestEarlyReflectorRuntimePeriodicPolls(t *testing.T) {
	store := newRuntimeTestStore()
	agent := &runtimeTestAgent{recorder: store}
	rt, err := StartEarlyReflectorRuntime(context.Background(), t.TempDir(), store, agent, EarlyReflectorRuntimeConfig{
		ProjectID:    "project-1",
		WorkerID:     "worker-1",
		MaxParallel:  1,
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("StartEarlyReflectorRuntime: %v", err)
	}
	defer rt.Shutdown()

	waitUntilRuntimeTest(t, time.Second, func() bool { return store.claimCount() >= 2 })
}

func TestEarlyReflectorRuntimeWorkerCallHasNoEarlyReflectionMarker(t *testing.T) {
	store := newRuntimeTestStore()
	store.enqueueClaim(akashic.LearningReflectionJob{
		AgentCallID:   "call-1",
		WorkflowRunID: "run-1",
		ProviderID:    "codex",
		ProjectID:     "project-1",
		SessionID:     "session-1",
		Status:        akashic.LearningReflectionStatusRunning,
	})
	agent := &runtimeTestAgent{
		recorder: store,
		response: provider.AgentResponse{
			Content: `{"learnings":[{"category":"tool-pattern","context":"ctx","insight":"ins","paths_glob":["x/**"]}]}`,
		},
		checkContext: func(ctx context.Context) {
			if _, ok := workflowctx.EarlyReflectionPolicyFromContext(ctx); ok {
				store.recordMarkerSeen()
			}
		},
	}

	ctx := workflowctx.ContextWithEarlyReflectionEnabled(context.Background(), workflowctx.EarlyReflectionPolicy{
		AllowedProviders: []provider.ProviderID{"codex"},
	})
	rt, err := StartEarlyReflectorRuntime(ctx, t.TempDir(), store, agent, EarlyReflectorRuntimeConfig{
		ProjectID:    "project-1",
		WorkerID:     "worker-1",
		MaxParallel:  1,
		PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("StartEarlyReflectorRuntime: %v", err)
	}
	defer rt.Shutdown()

	waitUntilRuntimeTest(t, time.Second, func() bool { return store.succeededCount() == 1 })
	if got := agent.callCount(); got != 1 {
		t.Fatalf("CallAgent count = %d, want 1", got)
	}
	if got := store.failedCount(); got != 0 {
		t.Fatalf("failed count = %d, want 0", got)
	}
	if store.markerSeen() {
		t.Fatalf("worker CallAgent received early reflection marker")
	}
}

func TestEarlyReflectorRuntimeShutdownCancelsWorkers(t *testing.T) {
	store := newRuntimeTestStore()
	store.enqueueClaim(akashic.LearningReflectionJob{
		AgentCallID:   "call-1",
		WorkflowRunID: "run-1",
		ProviderID:    "codex",
		ProjectID:     "project-1",
		SessionID:     "session-1",
		Status:        akashic.LearningReflectionStatusRunning,
	})
	agent := &runtimeTestAgent{
		recorder: store,
		response: provider.AgentResponse{
			Content: `{"learnings":[]}`,
		},
		blockUntilCanceled: true,
	}

	rt, err := StartEarlyReflectorRuntime(context.Background(), t.TempDir(), store, agent, EarlyReflectorRuntimeConfig{
		ProjectID:    "project-1",
		WorkerID:     "worker-1",
		MaxParallel:  1,
		PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("StartEarlyReflectorRuntime: %v", err)
	}
	waitUntilRuntimeTest(t, time.Second, func() bool { return agent.callCount() == 1 })

	rt.Shutdown()

	waitUntilRuntimeTest(t, time.Second, agent.sawCanceled)
}

func TestEarlyReflectorRuntimeParentContextCancelStopsWorkers(t *testing.T) {
	store := newRuntimeTestStore()
	store.enqueueClaim(akashic.LearningReflectionJob{
		AgentCallID:   "call-1",
		WorkflowRunID: "run-1",
		ProviderID:    "codex",
		ProjectID:     "project-1",
		SessionID:     "session-1",
		Status:        akashic.LearningReflectionStatusRunning,
	})
	agent := &runtimeTestAgent{
		recorder:           store,
		blockUntilCanceled: true,
	}

	ctx, cancel := context.WithCancel(context.Background())
	rt, err := StartEarlyReflectorRuntime(ctx, t.TempDir(), store, agent, EarlyReflectorRuntimeConfig{
		ProjectID:    "project-1",
		WorkerID:     "worker-1",
		MaxParallel:  1,
		PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("StartEarlyReflectorRuntime: %v", err)
	}
	defer rt.Shutdown()
	waitUntilRuntimeTest(t, time.Second, func() bool { return agent.callCount() == 1 })

	cancel()

	waitUntilRuntimeTest(t, time.Second, agent.sawCanceled)
}

type runtimeTestAgent struct {
	mu                 sync.Mutex
	recorder           akashic.Recorder
	response           provider.AgentResponse
	err                error
	calls              int
	checkContext       func(context.Context)
	blockUntilCanceled bool
	canceled           bool
}

func (a *runtimeTestAgent) AkashicRecorder() akashic.Recorder {
	return a.recorder
}

func (a *runtimeTestAgent) CallAgent(ctx context.Context, _ string, _ string, _ provider.AgentEventCallback, _ string, _ *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
	if a.checkContext != nil {
		a.checkContext(ctx)
	}
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()

	if a.blockUntilCanceled {
		<-ctx.Done()
		a.mu.Lock()
		a.canceled = true
		a.mu.Unlock()
		return provider.AgentResponse{}, ctx.Err()
	}
	if a.err != nil {
		return provider.AgentResponse{}, a.err
	}
	return a.response, nil
}

func (a *runtimeTestAgent) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func (a *runtimeTestAgent) sawCanceled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.canceled
}

type runtimeTestStore struct {
	akashic.NoopRecorder

	mu        sync.Mutex
	queued    []akashic.LearningReflectionJob
	claims    int
	succeeded []akashic.LearningReflectionResult
	failed    []akashic.LearningReflectionFailure
	marker    bool
}

func newRuntimeTestStore() *runtimeTestStore {
	return &runtimeTestStore{}
}

func (s *runtimeTestStore) enqueueClaim(job akashic.LearningReflectionJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queued = append(s.queued, job)
}

func (s *runtimeTestStore) RecordLearningReflectionJob(_ context.Context, job akashic.LearningReflectionJob) (akashic.LearningReflectionEnqueueResult, error) {
	return akashic.LearningReflectionEnqueueResult{Job: job, Inserted: true}, nil
}

func (s *runtimeTestStore) ClaimLearningReflectionJob(_ context.Context, projectID string, workerID string, now time.Time) (akashic.LearningReflectionJob, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	if projectID == "" {
		return akashic.LearningReflectionJob{}, false, errors.New("project_id required")
	}
	if workerID == "" {
		return akashic.LearningReflectionJob{}, false, errors.New("worker_id required")
	}
	if len(s.queued) == 0 {
		return akashic.LearningReflectionJob{}, false, nil
	}
	job := s.queued[0]
	s.queued = s.queued[1:]
	if job.ProjectID == "" {
		job.ProjectID = projectID
	}
	job.WorkerID = workerID
	job.StartedAt = &now
	return job, true, nil
}

func (s *runtimeTestStore) MarkLearningReflectionSucceeded(_ context.Context, _ string, result akashic.LearningReflectionResult, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.succeeded = append(s.succeeded, result)
	return nil
}

func (s *runtimeTestStore) MarkLearningReflectionFailed(_ context.Context, _ string, failure akashic.LearningReflectionFailure, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = append(s.failed, failure)
	return nil
}

func (s *runtimeTestStore) JobsByAgentCallIDs(context.Context, []string) ([]akashic.LearningReflectionJob, error) {
	return nil, nil
}

func (s *runtimeTestStore) MarkLearningReflectionAggregated(context.Context, []string, string, time.Time) error {
	return nil
}

func (s *runtimeTestStore) MarkLearningReflectionStale(context.Context, []string, string, time.Time) error {
	return nil
}

func (s *runtimeTestStore) RecordLearningOnce(context.Context, akashic.LearningRow) (bool, error) {
	return true, nil
}

func (s *runtimeTestStore) claimCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claims
}

func (s *runtimeTestStore) succeededCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.succeeded)
}

func (s *runtimeTestStore) failedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.failed)
}

func (s *runtimeTestStore) lastFailure() akashic.LearningReflectionFailure {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.failed) == 0 {
		return akashic.LearningReflectionFailure{}
	}
	return s.failed[len(s.failed)-1]
}

func (s *runtimeTestStore) recordMarkerSeen() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marker = true
}

func (s *runtimeTestStore) markerSeen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.marker
}

func waitUntilRuntimeTest(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}
