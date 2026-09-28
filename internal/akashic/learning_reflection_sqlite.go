package akashic

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

func (r *SQLiteRecorder) RecordLearningReflectionJob(ctx context.Context, job LearningReflectionJob) (LearningReflectionEnqueueResult, error) {
	normalized, err := normalizeLearningReflectionJobForInsert(job)
	if err != nil {
		return LearningReflectionEnqueueResult{}, err
	}
	result, err := r.db.ExecContext(ctx, queryInsertLearningReflectionJob,
		normalized.AgentCallID,
		normalized.WorkflowRunID,
		normalized.WorkflowID,
		normalized.WorkflowName,
		normalized.StepID,
		normalized.StepName,
		normalized.ProviderID,
		normalized.CWD,
		normalized.ProjectID,
		nullString(normalized.SessionID),
		string(normalized.Status),
		nullString(string(normalized.SkipReason)),
		nullString(normalized.FailureReason),
		nullString(normalized.FailureDetail),
		normalized.AttemptCount,
		normalized.MaxAttempts,
		nullString(normalized.WorkerID),
		nullTime(normalized.LeaseUntil),
		nullRawMessage(normalized.ResultJSON),
		formatTime(normalized.QueuedAt),
		nullTime(normalized.StartedAt),
		nullTime(normalized.FinishedAt),
		formatTime(normalized.UpdatedAt),
		nullString(normalized.AggregatedTailRunID),
		nullTime(normalized.AggregatedAt),
	)
	if err != nil {
		return LearningReflectionEnqueueResult{}, fmt.Errorf("akashic: insert learning reflection job: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return LearningReflectionEnqueueResult{}, fmt.Errorf("akashic: learning reflection rows affected: %w", err)
	}
	stored, err := r.learningReflectionJobByAgentCallID(ctx, normalized.AgentCallID)
	if err != nil {
		return LearningReflectionEnqueueResult{}, err
	}
	return LearningReflectionEnqueueResult{Job: stored, Inserted: rowsAffected > 0}, nil
}

func (r *SQLiteRecorder) ClaimLearningReflectionJob(ctx context.Context, projectID string, workerID string, now time.Time) (LearningReflectionJob, bool, error) {
	if projectID == "" {
		return LearningReflectionJob{}, false, fmt.Errorf("akashic: claim learning reflection job: project_id is required")
	}
	if workerID == "" {
		return LearningReflectionJob{}, false, fmt.Errorf("akashic: claim learning reflection job: worker_id is required")
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return LearningReflectionJob{}, false, fmt.Errorf("akashic: begin claim learning reflection job: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	nowStr := formatTime(now)
	var agentCallID string
	if err := tx.QueryRowContext(ctx, querySelectClaimableLearningReflectionJob, projectID, nowStr).Scan(&agentCallID); err != nil {
		if err == sql.ErrNoRows {
			if commitErr := tx.Commit(); commitErr != nil {
				return LearningReflectionJob{}, false, fmt.Errorf("akashic: commit empty claim learning reflection job: %w", commitErr)
			}
			return LearningReflectionJob{}, false, nil
		}
		return LearningReflectionJob{}, false, fmt.Errorf("akashic: select claimable learning reflection job: %w", err)
	}

	leaseUntil := now.Add(learningReflectionLeaseDuration)
	if _, err := tx.ExecContext(ctx, queryUpdateClaimedLearningReflectionJob,
		workerID, formatTime(leaseUntil), nowStr, nowStr, agentCallID, projectID, nowStr); err != nil {
		return LearningReflectionJob{}, false, fmt.Errorf("akashic: update claimed learning reflection job: %w", err)
	}

	claimed, err := learningReflectionJobByAgentCallIDTx(ctx, tx, agentCallID)
	if err != nil {
		return LearningReflectionJob{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return LearningReflectionJob{}, false, fmt.Errorf("akashic: commit claim learning reflection job: %w", err)
	}
	return claimed, true, nil
}

func (r *SQLiteRecorder) MarkLearningReflectionSucceeded(ctx context.Context, agentCallID string, result LearningReflectionResult, now time.Time) error {
	_, err := r.db.ExecContext(ctx, queryMarkLearningReflectionSucceeded, nullRawMessage(result.JSON), formatTime(now), formatTime(now), agentCallID)
	if err != nil {
		return fmt.Errorf("akashic: mark learning reflection succeeded: %w", err)
	}
	return nil
}

func (r *SQLiteRecorder) MarkLearningReflectionFailed(ctx context.Context, agentCallID string, failure LearningReflectionFailure, now time.Time) error {
	_, err := r.db.ExecContext(ctx, queryMarkLearningReflectionFailed,
		boolInt(failure.Retryable), nullString(failure.Reason), nullString(failure.Detail), nullRawMessage(failure.ResultJSON),
		boolInt(failure.Retryable), formatTime(now), formatTime(now), agentCallID)
	if err != nil {
		return fmt.Errorf("akashic: mark learning reflection failed: %w", err)
	}
	return nil
}

func (r *SQLiteRecorder) JobsByAgentCallIDs(ctx context.Context, agentCallIDs []string) ([]LearningReflectionJob, error) {
	if len(agentCallIDs) == 0 {
		return []LearningReflectionJob{}, nil
	}
	placeholders, args := placeholdersForStrings(agentCallIDs)
	rows, err := r.db.QueryContext(ctx, querySelectLearningReflectionJobsByAgentCallIDs(placeholders), args...)
	if err != nil {
		return nil, fmt.Errorf("akashic: select learning reflection jobs by agent calls: %w", err)
	}
	defer rows.Close()

	byID := make(map[string]LearningReflectionJob, len(agentCallIDs))
	for rows.Next() {
		job, err := scanLearningReflectionJob(rows)
		if err != nil {
			return nil, err
		}
		byID[job.AgentCallID] = job
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("akashic: learning reflection jobs by agent calls iter: %w", err)
	}

	out := make([]LearningReflectionJob, 0, len(byID))
	for _, id := range agentCallIDs {
		if job, ok := byID[id]; ok {
			out = append(out, job)
		}
	}
	return out, nil
}

func (r *SQLiteRecorder) MarkLearningReflectionAggregated(ctx context.Context, agentCallIDs []string, tailRunID string, now time.Time) error {
	if len(agentCallIDs) == 0 {
		return nil
	}
	placeholders, args := placeholdersForStrings(agentCallIDs)
	args = append([]any{tailRunID, formatTime(now), formatTime(now)}, args...)
	_, err := r.db.ExecContext(ctx, queryMarkLearningReflectionAggregated(placeholders), args...)
	if err != nil {
		return fmt.Errorf("akashic: mark learning reflection aggregated: %w", err)
	}
	return nil
}

func (r *SQLiteRecorder) MarkLearningReflectionStale(ctx context.Context, agentCallIDs []string, reason string, now time.Time) error {
	if len(agentCallIDs) == 0 {
		return nil
	}
	placeholders, args := placeholdersForStrings(agentCallIDs)
	args = append([]any{nullString(reason), formatTime(now), formatTime(now)}, args...)
	_, err := r.db.ExecContext(ctx, queryMarkLearningReflectionStale(placeholders), args...)
	if err != nil {
		return fmt.Errorf("akashic: mark learning reflection stale: %w", err)
	}
	return nil
}

func (r *SQLiteRecorder) LearningReflectionJobsByWorkflowRunTree(ctx context.Context, parentRunID string) ([]LearningReflectionJob, error) {
	rows, err := r.db.QueryContext(ctx, querySelectLearningReflectionJobsByWorkflowRunTree, parentRunID)
	if err != nil {
		return nil, fmt.Errorf("akashic: select learning reflection jobs by workflow run tree: %w", err)
	}
	defer rows.Close()

	var out []LearningReflectionJob
	for rows.Next() {
		job, err := scanLearningReflectionJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("akashic: learning reflection jobs by workflow run tree iter: %w", err)
	}
	return out, nil
}

func normalizeLearningReflectionJobForInsert(job LearningReflectionJob) (LearningReflectionJob, error) {
	if job.AgentCallID == "" {
		return LearningReflectionJob{}, fmt.Errorf("akashic: learning reflection job agent_call_id is required")
	}
	if job.ProjectID == "" {
		return LearningReflectionJob{}, fmt.Errorf("akashic: learning reflection job project_id is required")
	}
	if job.QueuedAt.IsZero() {
		job.QueuedAt = time.Now().UTC()
	}
	if job.UpdatedAt.IsZero() {
		job.UpdatedAt = job.QueuedAt
	}
	if job.MaxAttempts == 0 {
		job.MaxAttempts = learningReflectionDefaultMaxAttempts
	}
	if job.SessionID == "" {
		job.Status = LearningReflectionStatusSkipped
		job.SkipReason = LearningReflectionSkipEmptySessionID
	} else if job.SkipReason != "" {
		job.Status = LearningReflectionStatusSkipped
	} else if job.Status == "" {
		job.Status = LearningReflectionStatusPending
	}
	if !validLearningReflectionStatus(job.Status) {
		return LearningReflectionJob{}, fmt.Errorf("akashic: invalid learning reflection status %q", job.Status)
	}
	return job, nil
}

func (r *SQLiteRecorder) learningReflectionJobByAgentCallID(ctx context.Context, agentCallID string) (LearningReflectionJob, error) {
	return learningReflectionJobByAgentCallIDQuery(ctx, r.db, agentCallID)
}

func learningReflectionJobByAgentCallIDTx(ctx context.Context, tx *sql.Tx, agentCallID string) (LearningReflectionJob, error) {
	return learningReflectionJobByAgentCallIDQuery(ctx, tx, agentCallID)
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func learningReflectionJobByAgentCallIDQuery(ctx context.Context, q queryRower, agentCallID string) (LearningReflectionJob, error) {
	row := q.QueryRowContext(ctx, querySelectLearningReflectionJobByAgentCallID, agentCallID)
	job, err := scanLearningReflectionJob(row)
	if err != nil {
		return LearningReflectionJob{}, fmt.Errorf("akashic: select learning reflection job %s: %w", agentCallID, err)
	}
	return job, nil
}

type learningReflectionJobScanner interface {
	Scan(dest ...any) error
}

func scanLearningReflectionJob(scanner learningReflectionJobScanner) (LearningReflectionJob, error) {
	var (
		job                 LearningReflectionJob
		sessionID           sql.NullString
		skipReason          sql.NullString
		failureReason       sql.NullString
		failureDetail       sql.NullString
		workerID            sql.NullString
		leaseUntil          sql.NullString
		resultJSON          sql.NullString
		queuedAt            string
		startedAt           sql.NullString
		finishedAt          sql.NullString
		updatedAt           string
		aggregatedTailRunID sql.NullString
		aggregatedAt        sql.NullString
	)
	if err := scanner.Scan(
		&job.AgentCallID,
		&job.WorkflowRunID,
		&job.WorkflowID,
		&job.WorkflowName,
		&job.StepID,
		&job.StepName,
		&job.ProviderID,
		&job.CWD,
		&job.ProjectID,
		&sessionID,
		&job.Status,
		&skipReason,
		&failureReason,
		&failureDetail,
		&job.AttemptCount,
		&job.MaxAttempts,
		&workerID,
		&leaseUntil,
		&resultJSON,
		&queuedAt,
		&startedAt,
		&finishedAt,
		&updatedAt,
		&aggregatedTailRunID,
		&aggregatedAt,
	); err != nil {
		return LearningReflectionJob{}, fmt.Errorf("akashic: scan learning reflection job: %w", err)
	}
	var err error
	job.SessionID = nullStringValue(sessionID)
	job.SkipReason = LearningReflectionSkipReason(nullStringValue(skipReason))
	job.FailureReason = nullStringValue(failureReason)
	job.FailureDetail = nullStringValue(failureDetail)
	job.WorkerID = nullStringValue(workerID)
	job.LeaseUntil, err = parseOptionalStoredTime(leaseUntil)
	if err != nil {
		return LearningReflectionJob{}, fmt.Errorf("akashic: parse learning reflection lease_until for %s: %w", job.AgentCallID, err)
	}
	if resultJSON.Valid {
		job.ResultJSON = json.RawMessage(resultJSON.String)
	}
	job.QueuedAt, err = parseStoredTime(queuedAt)
	if err != nil {
		return LearningReflectionJob{}, fmt.Errorf("akashic: parse learning reflection queued_at for %s: %w", job.AgentCallID, err)
	}
	job.StartedAt, err = parseOptionalStoredTime(startedAt)
	if err != nil {
		return LearningReflectionJob{}, fmt.Errorf("akashic: parse learning reflection started_at for %s: %w", job.AgentCallID, err)
	}
	job.FinishedAt, err = parseOptionalStoredTime(finishedAt)
	if err != nil {
		return LearningReflectionJob{}, fmt.Errorf("akashic: parse learning reflection finished_at for %s: %w", job.AgentCallID, err)
	}
	job.UpdatedAt, err = parseStoredTime(updatedAt)
	if err != nil {
		return LearningReflectionJob{}, fmt.Errorf("akashic: parse learning reflection updated_at for %s: %w", job.AgentCallID, err)
	}
	job.AggregatedTailRunID = nullStringValue(aggregatedTailRunID)
	job.AggregatedAt, err = parseOptionalStoredTime(aggregatedAt)
	if err != nil {
		return LearningReflectionJob{}, fmt.Errorf("akashic: parse learning reflection aggregated_at for %s: %w", job.AgentCallID, err)
	}
	return job, nil
}

func nullTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return formatTime(*t)
}

func nullRawMessage(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}
