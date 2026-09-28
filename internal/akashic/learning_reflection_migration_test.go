package akashic

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestSchemaCreatesLearningReflectionJobsOnFreshDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fresh-reflection-jobs.sqlite")

	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	var tableName string
	if err := recorder.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'learning_reflection_jobs'`).Scan(&tableName); err != nil {
		t.Fatalf("learning_reflection_jobs table missing: %v", err)
	}

	wantColumns := []columnSnapshot{
		{cid: 0, name: "agent_call_id", ctype: "TEXT", notnull: 0, pk: 1},
		{cid: 1, name: "workflow_run_id", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 2, name: "workflow_id", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 3, name: "workflow_name", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 4, name: "step_id", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 5, name: "step_name", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 6, name: "provider_id", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 7, name: "cwd", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 8, name: "project_id", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 9, name: "session_id", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 10, name: "status", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 11, name: "skip_reason", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 12, name: "failure_reason", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 13, name: "failure_detail", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 14, name: "attempt_count", ctype: "INTEGER", notnull: 1, pk: 0},
		{cid: 15, name: "max_attempts", ctype: "INTEGER", notnull: 1, pk: 0},
		{cid: 16, name: "worker_id", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 17, name: "lease_until", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 18, name: "result_json", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 19, name: "queued_at", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 20, name: "started_at", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 21, name: "finished_at", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 22, name: "updated_at", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 23, name: "aggregated_tail_run_id", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 24, name: "aggregated_at", ctype: "TEXT", notnull: 0, pk: 0},
	}
	assertColumnSnapshots(t, "learning_reflection_jobs", pragmaTableInfo(t, recorder.db, "learning_reflection_jobs"), wantColumns)

	assertIndexColumns(t, recorder.db, "idx_learning_reflection_jobs_workflow_run_id", []string{"workflow_run_id"})
	assertIndexColumns(t, recorder.db, "idx_learning_reflection_jobs_status", []string{"status"})
	assertIndexColumns(t, recorder.db, "idx_learning_reflection_jobs_project_status_lease", []string{"project_id", "status", "lease_until"})
	assertIndexColumns(t, recorder.db, "idx_learning_reflection_jobs_lease_until", []string{"lease_until"})

	for _, status := range []string{"pending", "running", "succeeded", "failed", "skipped", "stale", "aggregated"} {
		insertMinimalLearningReflectionJob(t, recorder.db, "job-"+status, "project-a", status)
	}
	var attemptCount, maxAttempts int
	if err := recorder.db.QueryRowContext(ctx,
		`SELECT attempt_count, max_attempts FROM learning_reflection_jobs WHERE agent_call_id = ?`, "job-pending",
	).Scan(&attemptCount, &maxAttempts); err != nil {
		t.Fatalf("query default attempt counters: %v", err)
	}
	if attemptCount != 0 || maxAttempts != 2 {
		t.Fatalf("default attempt counters mismatch: attempt_count=%d max_attempts=%d", attemptCount, maxAttempts)
	}
	if _, err := recorder.db.ExecContext(ctx, minimalLearningReflectionJobInsertSQL(),
		"job-invalid-status", "run-invalid-status", "wf", "Workflow", "step", "Step", "claude",
		"/repo", "project-a", "session", "unknown-status", "2026-05-24T00:00:00Z", "2026-05-24T00:00:00Z",
	); err == nil {
		t.Fatal("invalid reflection job status was accepted")
	}
	if _, err := recorder.db.ExecContext(ctx, minimalLearningReflectionJobInsertSQL(),
		"job-null-project", "run-null-project", "wf", "Workflow", "step", "Step", "claude",
		"/repo", nil, "session", "pending", "2026-05-24T00:00:00Z", "2026-05-24T00:00:00Z",
	); err == nil {
		t.Fatal("NULL project_id was accepted")
	}
}

func TestOpenSQLiteAddsLearningReflectionJobsToLegacyDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-reflection-jobs.sqlite")

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	legacy.SetMaxOpenConns(1)
	if _, err := legacy.ExecContext(ctx, legacySchemaBeforeLearningReflectionJobs); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := legacy.ExecContext(ctx,
		`INSERT INTO workflow_runs (id, workflow_id, workflow_name, cwd, project_id, started_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"legacy-run-1", "wf-legacy", "Legacy Workflow", "/repo/legacy", "legacy-project", "2026-05-24T00:00:00Z"); err != nil {
		t.Fatalf("insert legacy workflow_run: %v", err)
	}
	if _, err := legacy.ExecContext(ctx,
		`INSERT INTO agent_calls (id, workflow_run_id, workflow_id, workflow_name, step_id, step_name, provider_id, cwd, project_id, session_id, started_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"legacy-call-1", "legacy-run-1", "wf-legacy", "Legacy Workflow", "step-1", "Step 1", "claude", "/repo/legacy", "legacy-project", "session-legacy", "2026-05-24T00:00:01Z"); err != nil {
		t.Fatalf("insert legacy agent_call: %v", err)
	}
	if _, err := legacy.ExecContext(ctx,
		`INSERT INTO run_artifacts (workflow_run_id, path, kind, size_bytes, mtime, mime) VALUES (?, ?, ?, ?, ?, ?)`,
		"legacy-run-1", "/repo/legacy/.rilll/wf/run/context.json", "context", 100, "2026-05-24T00:00:02Z", "application/json"); err != nil {
		t.Fatalf("insert legacy run_artifact: %v", err)
	}
	if _, err := legacy.ExecContext(ctx,
		`INSERT INTO learnings (id, agent_call_id, workflow_run_id, workflow_id, step_id, category, context, insight, paths_glob_json, source, recorded_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"learning-1", "legacy-call-1", "legacy-run-1", "wf-legacy", "step-1", CategoryToolPattern,
		"ctx", "insight", `["internal/akashic/*"]`, SourceAgentSelfReport, "2026-05-24T00:00:03Z"); err != nil {
		t.Fatalf("insert legacy learning: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	var tableName string
	if err := recorder.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'learning_reflection_jobs'`).Scan(&tableName); err != nil {
		t.Fatalf("learning_reflection_jobs table was not added: %v", err)
	}

	var providerID, sessionID string
	if err := recorder.db.QueryRowContext(ctx,
		`SELECT provider_id, session_id FROM agent_calls WHERE id = ?`, "legacy-call-1").Scan(&providerID, &sessionID); err != nil {
		t.Fatalf("legacy agent_calls row missing: %v", err)
	}
	if providerID != "claude" || sessionID != "session-legacy" {
		t.Fatalf("legacy agent_calls row was corrupted: provider=%q session=%q", providerID, sessionID)
	}
	var artifactKind string
	if err := recorder.db.QueryRowContext(ctx,
		`SELECT kind FROM run_artifacts WHERE workflow_run_id = ?`, "legacy-run-1").Scan(&artifactKind); err != nil {
		t.Fatalf("legacy run_artifacts row missing: %v", err)
	}
	if artifactKind != "context" {
		t.Fatalf("legacy run_artifacts row was corrupted: kind=%q", artifactKind)
	}
	var insight string
	if err := recorder.db.QueryRowContext(ctx,
		`SELECT insight FROM learnings WHERE id = ?`, "learning-1").Scan(&insight); err != nil {
		t.Fatalf("legacy learnings row missing: %v", err)
	}
	if insight != "insight" {
		t.Fatalf("legacy learnings row was corrupted: insight=%q", insight)
	}
}

func TestOpenSQLiteLearningReflectionJobsMigrationIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "idempotent-reflection-jobs.sqlite")

	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("first OpenSQLite: %v", err)
	}
	insertMinimalLearningReflectionJob(t, recorder.db, "job-idempotent", "project-a", "pending")
	if err := recorder.Close(); err != nil {
		t.Fatalf("close first recorder: %v", err)
	}

	recorder, err = OpenSQLite(path)
	if err != nil {
		t.Fatalf("second OpenSQLite: %v", err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("close second recorder: %v", err)
	}

	recorder, err = OpenSQLite(path)
	if err != nil {
		t.Fatalf("third OpenSQLite: %v", err)
	}
	defer recorder.Close()

	var jobCount int
	if err := recorder.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM learning_reflection_jobs WHERE agent_call_id = ?`, "job-idempotent").Scan(&jobCount); err != nil {
		t.Fatalf("count reflection job: %v", err)
	}
	if jobCount != 1 {
		t.Fatalf("reflection job row was not preserved idempotently: count=%d", jobCount)
	}
	for _, indexName := range []string{
		"idx_learning_reflection_jobs_workflow_run_id",
		"idx_learning_reflection_jobs_status",
		"idx_learning_reflection_jobs_project_status_lease",
		"idx_learning_reflection_jobs_lease_until",
	} {
		var indexCount int
		if err := recorder.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, indexName).Scan(&indexCount); err != nil {
			t.Fatalf("count index %s: %v", indexName, err)
		}
		if indexCount != 1 {
			t.Fatalf("index %s count=%d, want 1", indexName, indexCount)
		}
	}
}

func insertMinimalLearningReflectionJob(t *testing.T, db *sql.DB, agentCallID, projectID, status string) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, minimalLearningReflectionJobInsertSQL(),
		agentCallID, "run-"+agentCallID, "wf", "Workflow", "step", "Step", "claude",
		"/repo", projectID, "session-"+agentCallID, status, "2026-05-24T00:00:00Z", "2026-05-24T00:00:00Z",
	); err != nil {
		t.Fatalf("insert minimal reflection job %s/%s: %v", agentCallID, status, err)
	}
}

func minimalLearningReflectionJobInsertSQL() string {
	return `
INSERT INTO learning_reflection_jobs (
  agent_call_id, workflow_run_id, workflow_id, workflow_name, step_id, step_name,
  provider_id, cwd, project_id, session_id, status, queued_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`
}

func assertIndexColumns(t *testing.T, db *sql.DB, indexName string, want []string) {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `PRAGMA index_info(`+indexName+`)`)
	if err != nil {
		t.Fatalf("pragma index_info(%s): %v", indexName, err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var seqno, cid int
		var name string
		if err := rows.Scan(&seqno, &cid, &name); err != nil {
			t.Fatalf("scan index_info(%s): %v", indexName, err)
		}
		got = append(got, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("index_info(%s) rows: %v", indexName, err)
	}
	if len(got) != len(want) {
		t.Fatalf("%s columns mismatch: got=%v want=%v", indexName, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s column[%d] got=%q want=%q (all got=%v)", indexName, i, got[i], want[i], got)
		}
	}
}

const legacySchemaBeforeLearningReflectionJobs = `
CREATE TABLE workflow_runs (
  id TEXT PRIMARY KEY,
  workflow_id TEXT NOT NULL,
  workflow_name TEXT NOT NULL,
  cwd TEXT NOT NULL,
  project_id TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  finished_at TEXT,
  error TEXT
);

CREATE TABLE agent_calls (
  id TEXT PRIMARY KEY,
  workflow_run_id TEXT,
  workflow_id TEXT,
  workflow_name TEXT,
  step_id TEXT,
  step_name TEXT,
  provider_id TEXT,
  prompt TEXT,
  cwd TEXT,
  project_id TEXT,
  resume_session_id TEXT,
  output_format_type TEXT,
  session_id TEXT,
  success INTEGER,
  duration_ms INTEGER,
  total_cost_usd REAL,
  input_tokens INTEGER,
  output_tokens INTEGER,
  result_summary TEXT,
  error TEXT,
  failure_category TEXT,
  started_at TEXT,
  finished_at TEXT,
  result_payload_json TEXT,
  user_corrected INTEGER NOT NULL DEFAULT 0,
  model TEXT,
  model_reasoning_effort TEXT
);

CREATE TABLE run_artifacts (
  workflow_run_id TEXT NOT NULL CHECK (length(workflow_run_id) > 0),
  path TEXT NOT NULL CHECK (length(path) > 0),
  kind TEXT NOT NULL CHECK (length(kind) > 0),
  size_bytes INTEGER NOT NULL,
  mtime TEXT NOT NULL,
  mime TEXT,
  PRIMARY KEY (workflow_run_id, path)
);

CREATE TABLE learnings (
  id TEXT PRIMARY KEY,
  agent_call_id TEXT NOT NULL,
  workflow_run_id TEXT NOT NULL,
  workflow_id TEXT NOT NULL,
  step_id TEXT NOT NULL,
  category TEXT NOT NULL,
  context TEXT NOT NULL,
  insight TEXT NOT NULL,
  paths_glob_json TEXT NOT NULL,
  source TEXT NOT NULL,
  recorded_at TEXT NOT NULL
);

CREATE INDEX idx_agent_calls_workflow_run_id ON agent_calls(workflow_run_id);
CREATE INDEX idx_run_artifacts_workflow_run_id ON run_artifacts(workflow_run_id);
CREATE INDEX idx_learnings_workflow_run_id ON learnings(workflow_run_id);
`
