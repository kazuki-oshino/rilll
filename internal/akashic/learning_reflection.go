package akashic

import (
	"context"
	"encoding/json"
	"time"
)

type LearningReflectionStatus string

const (
	LearningReflectionStatusPending    LearningReflectionStatus = "pending"
	LearningReflectionStatusRunning    LearningReflectionStatus = "running"
	LearningReflectionStatusSucceeded  LearningReflectionStatus = "succeeded"
	LearningReflectionStatusFailed     LearningReflectionStatus = "failed"
	LearningReflectionStatusSkipped    LearningReflectionStatus = "skipped"
	LearningReflectionStatusStale      LearningReflectionStatus = "stale"
	LearningReflectionStatusAggregated LearningReflectionStatus = "aggregated"
)

type LearningReflectionSkipReason string

const (
	LearningReflectionSkipEmptySessionID         LearningReflectionSkipReason = "empty-session-id"
	LearningReflectionSkipProviderNotInAllowlist LearningReflectionSkipReason = "provider-not-in-allowlist"
	learningReflectionDefaultMaxAttempts                                      = 2
	learningReflectionLeaseDuration                                           = 5 * time.Minute
)

type LearningReflectionJob struct {
	AgentCallID         string
	WorkflowRunID       string
	WorkflowID          string
	WorkflowName        string
	StepID              string
	StepName            string
	ProviderID          string
	CWD                 string
	ProjectID           string
	SessionID           string
	Status              LearningReflectionStatus
	SkipReason          LearningReflectionSkipReason
	FailureReason       string
	FailureDetail       string
	AttemptCount        int
	MaxAttempts         int
	WorkerID            string
	LeaseUntil          *time.Time
	ResultJSON          json.RawMessage
	QueuedAt            time.Time
	StartedAt           *time.Time
	FinishedAt          *time.Time
	UpdatedAt           time.Time
	AggregatedTailRunID string
	AggregatedAt        *time.Time
}

type LearningReflectionEnqueueResult struct {
	Job      LearningReflectionJob
	Inserted bool
}

type LearningReflectionResult struct {
	JSON json.RawMessage
}

type LearningReflectionFailure struct {
	Reason     string
	Detail     string
	Retryable  bool
	ResultJSON json.RawMessage
}

type LearningReflectionJobRecorder interface {
	RecordLearningReflectionJob(ctx context.Context, job LearningReflectionJob) (LearningReflectionEnqueueResult, error)
}

type LearningReflectionJobStore interface {
	LearningReflectionJobRecorder
	ClaimLearningReflectionJob(ctx context.Context, projectID string, workerID string, now time.Time) (LearningReflectionJob, bool, error)
	MarkLearningReflectionSucceeded(ctx context.Context, agentCallID string, result LearningReflectionResult, now time.Time) error
	MarkLearningReflectionFailed(ctx context.Context, agentCallID string, failure LearningReflectionFailure, now time.Time) error
	JobsByAgentCallIDs(ctx context.Context, agentCallIDs []string) ([]LearningReflectionJob, error)
	MarkLearningReflectionAggregated(ctx context.Context, agentCallIDs []string, tailRunID string, now time.Time) error
	MarkLearningReflectionStale(ctx context.Context, agentCallIDs []string, reason string, now time.Time) error
	RecordLearningOnce(ctx context.Context, row LearningRow) (bool, error)
}

type LearningReflectionJobReader interface {
	LearningReflectionJobsByWorkflowRunTree(ctx context.Context, parentRunID string) ([]LearningReflectionJob, error)
}

func validLearningReflectionStatus(status LearningReflectionStatus) bool {
	switch status {
	case LearningReflectionStatusPending,
		LearningReflectionStatusRunning,
		LearningReflectionStatusSucceeded,
		LearningReflectionStatusFailed,
		LearningReflectionStatusSkipped,
		LearningReflectionStatusStale,
		LearningReflectionStatusAggregated:
		return true
	}
	return false
}
