package akashic

import "strings"

const learningReflectionJobSelectColumns = `
agent_call_id,
workflow_run_id,
workflow_id,
workflow_name,
step_id,
step_name,
provider_id,
cwd,
project_id,
session_id,
status,
skip_reason,
failure_reason,
failure_detail,
attempt_count,
max_attempts,
worker_id,
lease_until,
result_json,
queued_at,
started_at,
finished_at,
updated_at,
aggregated_tail_run_id,
aggregated_at
`

const queryInsertLearningReflectionJob = `
INSERT INTO learning_reflection_jobs (
  agent_call_id, workflow_run_id, workflow_id, workflow_name, step_id, step_name,
  provider_id, cwd, project_id, session_id, status, skip_reason, failure_reason,
  failure_detail, attempt_count, max_attempts, worker_id, lease_until, result_json,
  queued_at, started_at, finished_at, updated_at, aggregated_tail_run_id, aggregated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(agent_call_id) DO NOTHING
`

const querySelectClaimableLearningReflectionJob = `
SELECT agent_call_id
FROM learning_reflection_jobs
WHERE project_id = ?
  AND attempt_count < max_attempts
  AND (
    status = 'pending'
    OR (status = 'running' AND lease_until IS NOT NULL AND lease_until <= ?)
  )
ORDER BY queued_at ASC, agent_call_id ASC
LIMIT 1
`

const queryUpdateClaimedLearningReflectionJob = `
UPDATE learning_reflection_jobs
SET status = 'running',
    worker_id = ?,
    lease_until = ?,
    attempt_count = attempt_count + 1,
    started_at = ?,
    updated_at = ?
WHERE agent_call_id = ?
  AND project_id = ?
  AND attempt_count < max_attempts
  AND (
    status = 'pending'
    OR (status = 'running' AND lease_until IS NOT NULL AND lease_until <= ?)
  )
`

const queryMarkLearningReflectionSucceeded = `
UPDATE learning_reflection_jobs
SET status = 'succeeded',
    result_json = ?,
    failure_reason = NULL,
    failure_detail = NULL,
    lease_until = NULL,
    finished_at = ?,
    updated_at = ?
WHERE agent_call_id = ? AND status = 'running'
`

const queryMarkLearningReflectionFailed = `
UPDATE learning_reflection_jobs
SET status = CASE
      WHEN ? AND attempt_count < max_attempts THEN 'pending'
      ELSE 'failed'
    END,
    failure_reason = ?,
    failure_detail = ?,
    result_json = ?,
    lease_until = NULL,
    finished_at = CASE
      WHEN ? AND attempt_count < max_attempts THEN NULL
      ELSE ?
    END,
    updated_at = ?
WHERE agent_call_id = ? AND status = 'running'
`

const querySelectLearningReflectionJobsByWorkflowRunTree = `
WITH RECURSIVE run_ids(id) AS (
  SELECT ?
  UNION
  SELECT edge.child_run_id
  FROM workflow_run_edges AS edge
  JOIN run_ids ON edge.parent_run_id = run_ids.id
)
SELECT ` + learningReflectionJobSelectColumns + `
FROM learning_reflection_jobs
WHERE workflow_run_id IN (SELECT id FROM run_ids)
ORDER BY queued_at ASC, agent_call_id ASC
`

const querySelectLearningReflectionJobByAgentCallID = "SELECT " + learningReflectionJobSelectColumns + " FROM learning_reflection_jobs WHERE agent_call_id = ?"

// queryInsertLearningOnce is the insert-once path for early reflection tail aggregation.
// Deterministic learning ID conflicts mean the row was already recorded and are not fatal.
const queryInsertLearningOnce = `
INSERT INTO learnings (
  id, agent_call_id, workflow_run_id, workflow_id, step_id,
  category, context, insight, paths_glob_json, source, recorded_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO NOTHING
`

func querySelectLearningReflectionJobsByAgentCallIDs(placeholders string) string {
	return "SELECT " + learningReflectionJobSelectColumns + " FROM learning_reflection_jobs WHERE agent_call_id IN (" + placeholders + ")"
}

func queryMarkLearningReflectionAggregated(placeholders string) string {
	return `
UPDATE learning_reflection_jobs
SET status = 'aggregated',
    aggregated_tail_run_id = ?,
    aggregated_at = ?,
    updated_at = ?
WHERE agent_call_id IN (` + placeholders + `)
  AND status IN ('succeeded', 'failed', 'skipped')
`
}

func queryMarkLearningReflectionStale(placeholders string) string {
	return `
UPDATE learning_reflection_jobs
SET status = 'stale',
    failure_reason = ?,
    lease_until = NULL,
    finished_at = ?,
    updated_at = ?
WHERE agent_call_id IN (` + placeholders + `)
  AND status IN ('pending', 'running')
`
}

func placeholdersForStrings(values []string) (string, []any) {
	placeholders := make([]string, len(values))
	args := make([]any, len(values))
	for i, value := range values {
		placeholders[i] = "?"
		args[i] = value
	}
	return strings.Join(placeholders, ","), args
}
