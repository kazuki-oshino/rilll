package learnings

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

func TestAggregateEarlyReflectionBuildsArtifactsMarksStaleAndInsertsOnce(t *testing.T) {
	ctx := context.Background()
	cwd := makeFixtureProjectCWD(t, "project-rilll")
	runDir := filepath.Join(cwd, ".rilll", learningsWorkflowID, "tail-run-1")
	base := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	reader := &fakePrefetchReader{
		descendantRunIDs: []string{"child-run-1"},
		rowsByRunID: map[string][]akashic.AgentCallRow{
			"parent-run-1": {
				earlyAggregationAgentCallRow("call-succeeded", "parent-run-1", "session-succeeded", "codex", "step-a", "project-rilll", base),
				earlyAggregationAgentCallRow("call-skipped", "parent-run-1", "", "cursor", "step-b", "project-rilll", base.Add(time.Second)),
				earlyAggregationAgentCallRow("call-failed", "parent-run-1", "session-failed", "codex", "step-c", "project-rilll", base.Add(2*time.Second)),
				earlyAggregationAgentCallRow("call-missing", "parent-run-1", "session-missing", "codex", "step-d", "project-rilll", base.Add(3*time.Second)),
			},
			"child-run-1": {
				earlyAggregationAgentCallRow("call-wait-completed", "child-run-1", "session-wait-completed", "claude", "step-e", "project-rilll", base.Add(4*time.Second)),
				earlyAggregationAgentCallRow("call-pending", "child-run-1", "session-pending", "codex", "step-f", "project-rilll", base.Add(5*time.Second)),
			},
		},
	}
	store := newEarlyAggregationStore()
	succeeded := earlyAggregationJob("call-succeeded", "parent-run-1", "session-succeeded", "codex", "step-a", akashic.LearningReflectionStatusSucceeded, base)
	succeeded.ResultJSON = earlyAggregationResultJSON(t, "call-succeeded", "session-succeeded", "codex", "step-a", []LearningReflectionAcceptedLearning{
		{
			ID:        "learning-dup",
			Category:  akashic.CategoryToolPattern,
			Context:   "ctx succeeded",
			Insight:   "ins succeeded",
			PathsGlob: []string{"internal/workflow/**"},
		},
	})
	store.jobs["call-succeeded"] = succeeded
	store.jobs["call-skipped"] = earlyAggregationJob("call-skipped", "parent-run-1", "", "cursor", "step-b", akashic.LearningReflectionStatusSkipped, base)
	skipped := store.jobs["call-skipped"]
	skipped.SkipReason = akashic.LearningReflectionSkipProviderNotInAllowlist
	store.jobs["call-skipped"] = skipped
	store.jobs["call-failed"] = earlyAggregationJob("call-failed", "parent-run-1", "session-failed", "codex", "step-c", akashic.LearningReflectionStatusFailed, base)
	failed := store.jobs["call-failed"]
	failed.FailureReason = failureReasonNoCredentials
	failed.FailureDetail = "missing API key"
	store.jobs["call-failed"] = failed
	store.jobs["call-wait-completed"] = earlyAggregationJob("call-wait-completed", "child-run-1", "session-wait-completed", "claude", "step-e", akashic.LearningReflectionStatusPending, base)
	waitCompleted := earlyAggregationJob("call-wait-completed", "child-run-1", "session-wait-completed", "claude", "step-e", akashic.LearningReflectionStatusSucceeded, base)
	waitCompleted.ResultJSON = earlyAggregationResultJSON(t, "call-wait-completed", "session-wait-completed", "claude", "step-e", []LearningReflectionAcceptedLearning{
		{
			ID:        "learning-dup",
			Category:  akashic.CategoryToolPattern,
			Context:   "ctx duplicate",
			Insight:   "ins duplicate",
			PathsGlob: []string{"internal/workflow/**"},
		},
	})
	store.transitions["call-wait-completed"] = earlyAggregationTransition{poll: 2, job: waitCompleted}
	store.jobs["call-pending"] = earlyAggregationJob("call-pending", "child-run-1", "session-pending", "codex", "step-f", akashic.LearningReflectionStatusPending, base)

	result, err := AggregateEarlyReflection(ctx, EarlyAggregationInput{
		CWD:         cwd,
		ParentRunID: "parent-run-1",
		TailRunID:   "tail-run-1",
		RunDir:      runDir,
		WaitBudget:  20 * time.Millisecond,
	}, reader, store)
	if err != nil {
		t.Fatalf("AggregateEarlyReflection: %v", err)
	}

	if got, want := result.IncludedAgentCallIDs, []string{"call-succeeded", "call-wait-completed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("IncludedAgentCallIDs=%v, want %v", got, want)
	}
	if len(result.MissingTargets) != 1 || result.MissingTargets[0].AgentCallID != "call-missing" {
		t.Fatalf("MissingTargets=%+v, want call-missing only", result.MissingTargets)
	}

	earlyArtifact := readEarlyReflectionArtifact(t, result.EarlyReflectionJSONPath)
	wantSummary := EarlyReflectionSummary{
		TotalTargets:   6,
		Included:       2,
		WaitCompleted:  1,
		Skipped:        1,
		Failed:         1,
		Stale:          1,
		MissingJob:     1,
		FallbackReason: "bounded-wait-timeout",
	}
	if earlyArtifact.Summary != wantSummary {
		t.Fatalf("early summary=%+v, want %+v", earlyArtifact.Summary, wantSummary)
	}
	targets := earlyAggregationTargetsByID(earlyArtifact.Targets)
	assertEarlyAggregationTarget(t, targets["call-succeeded"], EarlyReflectionTargetStatusSucceeded, "", true)
	assertEarlyAggregationTarget(t, targets["call-wait-completed"], EarlyReflectionTargetStatusWaitCompleted, "", true)
	assertEarlyAggregationTarget(t, targets["call-skipped"], EarlyReflectionTargetStatusSkipped, string(akashic.LearningReflectionSkipProviderNotInAllowlist), false)
	assertEarlyAggregationTarget(t, targets["call-failed"], EarlyReflectionTargetStatusFailed, failureReasonNoCredentials, false)
	assertEarlyAggregationTarget(t, targets["call-pending"], EarlyReflectionTargetStatusStale, "wait-budget-exhausted", false)
	assertEarlyAggregationTarget(t, targets["call-missing"], EarlyReflectionTargetStatusMissingJob, "job-row-absent", false)

	learningsArtifact := readLearningsArtifact(t, result.LearningsJSONPath)
	if len(learningsArtifact.Learnings) != 1 {
		t.Fatalf("learnings count=%d, want 1 insert-once entry: %+v", len(learningsArtifact.Learnings), learningsArtifact.Learnings)
	}
	if learningsArtifact.Learnings[0].ID != "learning-dup" || learningsArtifact.Learnings[0].SessionID != "session-succeeded" {
		t.Fatalf("learning entry=%+v, want first deterministic entry", learningsArtifact.Learnings[0])
	}
	if got := store.recordLearningCallCount("learning-dup"); got != 2 {
		t.Fatalf("RecordLearningOnce calls for learning-dup=%d, want 2 attempts", got)
	}
	if got := len(store.learningRows); got != 1 {
		t.Fatalf("stored learning rows=%d, want 1", got)
	}

	reflectorArtifact := readReflectorArtifactT(t, result.ReflectorJSONPath)
	if len(reflectorArtifact.PerSession) != 2 {
		t.Fatalf("reflector per_session count=%d, want 2: %+v", len(reflectorArtifact.PerSession), reflectorArtifact.PerSession)
	}
	if len(store.staleCalls) != 1 || !reflect.DeepEqual(store.staleCalls[0].ids, []string{"call-pending"}) {
		t.Fatalf("stale calls=%+v, want call-pending", store.staleCalls)
	}
	if got, want := store.aggregatedIDs, []string{"call-succeeded", "call-skipped", "call-failed", "call-wait-completed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("aggregated IDs=%v, want %v", got, want)
	}
}

func earlyAggregationAgentCallRow(agentCallID, workflowRunID, sessionID, providerID, stepID, projectID string, startedAt time.Time) akashic.AgentCallRow {
	return akashic.AgentCallRow{
		ID:            agentCallID,
		WorkflowRunID: workflowRunID,
		SessionID:     sessionID,
		ProviderID:    providerID,
		StepID:        stepID,
		StepName:      "Step " + stepID,
		ProjectID:     projectID,
		StartedAt:     startedAt,
	}
}

func earlyAggregationJob(agentCallID, workflowRunID, sessionID, providerID, stepID string, status akashic.LearningReflectionStatus, queuedAt time.Time) akashic.LearningReflectionJob {
	return akashic.LearningReflectionJob{
		AgentCallID:   agentCallID,
		WorkflowRunID: workflowRunID,
		WorkflowID:    "workflow-a",
		WorkflowName:  "Workflow A",
		StepID:        stepID,
		StepName:      "Step " + stepID,
		ProviderID:    providerID,
		ProjectID:     "project-rilll",
		SessionID:     sessionID,
		Status:        status,
		QueuedAt:      queuedAt,
		UpdatedAt:     queuedAt,
	}
}

func earlyAggregationResultJSON(t *testing.T, agentCallID, sessionID, providerID, stepID string, accepted []LearningReflectionAcceptedLearning) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(LearningReflectionResultPayload{
		SchemaVersion: learningReflectionResultSchemaVersion,
		AgentCallID:   agentCallID,
		SessionID:     sessionID,
		ProviderID:    providerID,
		StepID:        stepID,
		Accepted:      accepted,
		Dropped:       []DroppedLearning{},
		Failures:      []SessionFailure{},
	})
	if err != nil {
		t.Fatalf("marshal reflection result: %v", err)
	}
	return raw
}

func readEarlyReflectionArtifact(t *testing.T, path string) EarlyReflectionArtifact {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read early-reflection.json: %v", err)
	}
	var artifact EarlyReflectionArtifact
	if err := json.Unmarshal(body, &artifact); err != nil {
		t.Fatalf("unmarshal early-reflection.json: %v\nbody=%s", err, body)
	}
	return artifact
}

func readLearningsArtifact(t *testing.T, path string) LearningsArtifact {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read learnings.json: %v", err)
	}
	var artifact LearningsArtifact
	if err := json.Unmarshal(body, &artifact); err != nil {
		t.Fatalf("unmarshal learnings.json: %v\nbody=%s", err, body)
	}
	return artifact
}

func earlyAggregationTargetsByID(targets []EarlyReflectionTargetSummary) map[string]EarlyReflectionTargetSummary {
	out := make(map[string]EarlyReflectionTargetSummary, len(targets))
	for _, target := range targets {
		out[target.AgentCallID] = target
	}
	return out
}

func assertEarlyAggregationTarget(t *testing.T, target EarlyReflectionTargetSummary, status EarlyReflectionTargetStatus, reason string, included bool) {
	t.Helper()
	if target.Status != status || target.Reason != reason || target.IncludedInCurator != included {
		t.Fatalf("target[%s]=%+v, want status=%q reason=%q included=%v", target.AgentCallID, target, status, reason, included)
	}
}

type earlyAggregationTransition struct {
	poll int
	job  akashic.LearningReflectionJob
}

type earlyAggregationStaleCall struct {
	ids    []string
	reason string
}

type earlyAggregationStore struct {
	mu                  sync.Mutex
	jobs                map[string]akashic.LearningReflectionJob
	transitions         map[string]earlyAggregationTransition
	pollCount           int
	recordLearningCalls []string
	learningRows        map[string]akashic.LearningRow
	staleCalls          []earlyAggregationStaleCall
	aggregatedIDs       []string
}

func newEarlyAggregationStore() *earlyAggregationStore {
	return &earlyAggregationStore{
		jobs:         map[string]akashic.LearningReflectionJob{},
		transitions:  map[string]earlyAggregationTransition{},
		learningRows: map[string]akashic.LearningRow{},
	}
}

func (s *earlyAggregationStore) RecordLearningReflectionJob(_ context.Context, job akashic.LearningReflectionJob) (akashic.LearningReflectionEnqueueResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[job.AgentCallID] = job
	return akashic.LearningReflectionEnqueueResult{Job: job, Inserted: true}, nil
}

func (s *earlyAggregationStore) ClaimLearningReflectionJob(context.Context, string, string, time.Time) (akashic.LearningReflectionJob, bool, error) {
	return akashic.LearningReflectionJob{}, false, nil
}

func (s *earlyAggregationStore) MarkLearningReflectionSucceeded(_ context.Context, agentCallID string, result akashic.LearningReflectionResult, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[agentCallID]
	job.Status = akashic.LearningReflectionStatusSucceeded
	job.ResultJSON = result.JSON
	job.FinishedAt = &now
	job.UpdatedAt = now
	s.jobs[agentCallID] = job
	return nil
}

func (s *earlyAggregationStore) MarkLearningReflectionFailed(_ context.Context, agentCallID string, failure akashic.LearningReflectionFailure, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[agentCallID]
	job.Status = akashic.LearningReflectionStatusFailed
	job.FailureReason = failure.Reason
	job.FailureDetail = failure.Detail
	job.ResultJSON = failure.ResultJSON
	job.FinishedAt = &now
	job.UpdatedAt = now
	s.jobs[agentCallID] = job
	return nil
}

func (s *earlyAggregationStore) JobsByAgentCallIDs(_ context.Context, agentCallIDs []string) ([]akashic.LearningReflectionJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pollCount++
	for id, transition := range s.transitions {
		if s.pollCount >= transition.poll {
			s.jobs[id] = transition.job
			delete(s.transitions, id)
		}
	}
	out := make([]akashic.LearningReflectionJob, 0, len(agentCallIDs))
	for _, id := range agentCallIDs {
		if job, ok := s.jobs[id]; ok {
			out = append(out, job)
		}
	}
	return out, nil
}

func (s *earlyAggregationStore) MarkLearningReflectionAggregated(_ context.Context, agentCallIDs []string, tailRunID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range agentCallIDs {
		job := s.jobs[id]
		switch job.Status {
		case akashic.LearningReflectionStatusSucceeded, akashic.LearningReflectionStatusFailed, akashic.LearningReflectionStatusSkipped:
			job.Status = akashic.LearningReflectionStatusAggregated
			job.AggregatedTailRunID = tailRunID
			job.AggregatedAt = &now
			job.UpdatedAt = now
			s.jobs[id] = job
			s.aggregatedIDs = append(s.aggregatedIDs, id)
		}
	}
	return nil
}

func (s *earlyAggregationStore) MarkLearningReflectionStale(_ context.Context, agentCallIDs []string, reason string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.staleCalls = append(s.staleCalls, earlyAggregationStaleCall{ids: append([]string(nil), agentCallIDs...), reason: reason})
	for _, id := range agentCallIDs {
		job := s.jobs[id]
		switch job.Status {
		case akashic.LearningReflectionStatusPending, akashic.LearningReflectionStatusRunning:
			job.Status = akashic.LearningReflectionStatusStale
			job.FailureReason = reason
			job.FinishedAt = &now
			job.UpdatedAt = now
			s.jobs[id] = job
		}
	}
	return nil
}

func (s *earlyAggregationStore) RecordLearningOnce(_ context.Context, row akashic.LearningRow) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordLearningCalls = append(s.recordLearningCalls, row.ID)
	if _, exists := s.learningRows[row.ID]; exists {
		return false, nil
	}
	s.learningRows[row.ID] = row
	return true, nil
}

func (s *earlyAggregationStore) recordLearningCallCount(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, got := range s.recordLearningCalls {
		if got == id {
			count++
		}
	}
	return count
}
