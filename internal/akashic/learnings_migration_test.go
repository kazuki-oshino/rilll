package akashic

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// legacyV1Schema は agent-learnings spec が schema を変更する**前**の v1 fixture DB schema。
//
// 設計意図 (task 10.1 / Req 5.5, 5.6):
//   - `learnings` table と `idx_learnings_workflow_run_id` index を含まない以外、当時の本番 DB
//     と同じ table / index 構成を再現する。
//   - これにより「v1 環境で動いていた DB を agent-learnings spec の OpenSQLite で opening した」
//     状況を integration レベルで再現でき、additive migration の不変式 (既存 column 順序 /
//     名前 / 型 / NOT NULL / PK 不変、既存 row 不破壊) を pin できる。
//   - 既存 schema (store.go の `schema` 定数) を意図的に変更したい場合は、本 const と
//     learnings_schema_test.go の want* 期待値を同時に update すること。schema regression
//     アラーム役を果たすため、片方だけ修正すると CI で fail する。
const legacyV1Schema = `
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
  user_corrected INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE raw_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  agent_call_id TEXT,
  workflow_run_id TEXT,
  sequence INTEGER NOT NULL,
  source TEXT NOT NULL,
  type TEXT NOT NULL,
  subtype TEXT,
  observed_at TEXT NOT NULL,
  payload_json TEXT,
  payload_truncated INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE agent_context_usage (
  agent_call_id TEXT PRIMARY KEY,
  workflow_run_id TEXT,
  raw_event_id INTEGER NOT NULL,
  sequence INTEGER NOT NULL,
  observed_at TEXT NOT NULL,
  model TEXT,
  total_tokens INTEGER NOT NULL,
  max_tokens INTEGER NOT NULL,
  raw_max_tokens INTEGER NOT NULL,
  percentage INTEGER NOT NULL,
  is_auto_compact_enabled INTEGER NOT NULL DEFAULT 0,
  auto_compact_threshold INTEGER,
  categories_json TEXT,
  memory_files_json TEXT,
  mcp_tools_json TEXT,
  agents_json TEXT,
  message_breakdown_json TEXT,
  api_usage_json TEXT
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

CREATE INDEX idx_agent_calls_workflow_run_id ON agent_calls(workflow_run_id);
CREATE INDEX idx_raw_events_agent_call_id ON raw_events(agent_call_id);
CREATE INDEX idx_raw_events_type ON raw_events(type, subtype);
CREATE INDEX idx_agent_context_usage_workflow_run_id ON agent_context_usage(workflow_run_id);
CREATE INDEX idx_run_artifacts_workflow_run_id ON run_artifacts(workflow_run_id);
CREATE INDEX idx_workflow_runs_project_id ON workflow_runs(project_id);
CREATE INDEX idx_agent_calls_project_id ON agent_calls(project_id);
`

// TestOpenSQLitePreservesV1FixtureAndAddsLearningsAdditively は agent-learnings task 10.1 /
// Req 5.5, 5.6 の integration regression test。v1 fixture DB (learnings table 無し / 既存
// fixture rows あり) を agent-learnings spec の OpenSQLite で opening したときの不変式を pin する:
//
//  1. learnings table と idx_learnings_workflow_run_id index が additive に追加される
//  2. 既存 workflow_runs / agent_calls / raw_events の column 順序 / 名前 / 型 / NOT NULL / PK が
//     PRAGMA table_info で完全不変 (Req 5.5: schema migration is additive only)
//  3. 既存 fixture row が round-trip で読み取れる (既存行が破壊されない)
//  4. 新 table 追加後も v1 既存行への UPDATE が成功する (Req 5.6: v1 は learnings table を
//     必要とせず無視できる)
//
// 観測可能な完了 (tasks.md L211): schema regression テストが pass。
// このテストは既存 schema を意図的に reorder / rename / 型変更すると即 fail する regression
// alarm 役。学んだ schema を変更したい場合は wantWorkflowRuns / wantAgentCalls / wantRawEvents
// と legacyV1Schema を同時に update すること。
func TestOpenSQLitePreservesV1FixtureAndAddsLearningsAdditively(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v1-fixture.sqlite")

	// v1 fixture DB を旧スキーマ (learnings 無し) で作成し、各既存 table に fixture row を入れる。
	// modernc.org/sqlite 経由で raw に DDL を流し込み「v1 当時の本番 DB」状態を再現する。
	legacyDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	legacyDB.SetMaxOpenConns(1)

	if _, err := legacyDB.ExecContext(ctx, legacyV1Schema); err != nil {
		t.Fatalf("create legacy v1 schema: %v", err)
	}

	// fixture rows を v1 既存 table に投入。後段 OpenSQLite の additive migration で
	// 既存行が破壊されないことを確認するための種データ。
	if _, err := legacyDB.ExecContext(ctx,
		`INSERT INTO workflow_runs (id, workflow_id, workflow_name, cwd, project_id, started_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"v1-run-1", "wf-1", "V1 Workflow", "/repo/v1", "rilll", "2026-05-01T00:00:00Z"); err != nil {
		t.Fatalf("insert legacy workflow_runs row: %v", err)
	}
	if _, err := legacyDB.ExecContext(ctx,
		`INSERT INTO agent_calls (id, workflow_run_id, workflow_id, workflow_name, step_id, step_name, provider_id, project_id, started_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"v1-call-1", "v1-run-1", "wf-1", "V1 Workflow", "step-1", "Step 1", "claude", "rilll", "2026-05-01T00:00:01Z"); err != nil {
		t.Fatalf("insert legacy agent_calls row: %v", err)
	}
	if _, err := legacyDB.ExecContext(ctx,
		`INSERT INTO raw_events (agent_call_id, workflow_run_id, sequence, source, type, observed_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"v1-call-1", "v1-run-1", 1, "claude-cli", "stdout", "2026-05-01T00:00:02Z"); err != nil {
		t.Fatalf("insert legacy raw_events row: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	// agent-learnings spec の OpenSQLite で再 open。CREATE TABLE IF NOT EXISTS により
	// 既存 table はスキップされ、learnings table のみ additive に追加される想定。
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite (re-open v1 fixture): %v", err)
	}
	defer recorder.Close()

	// (a) learnings table が additive 追加されている (Req 5.1, task 10.1 観測完了 1)
	var tableName string
	if err := recorder.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='learnings'`).Scan(&tableName); err != nil {
		t.Fatalf("learnings table が v1 fixture DB に additive 追加されていない: %v", err)
	}
	// idx_learnings_workflow_run_id index も additive に追加 (workflow_run_id 索引)
	var indexName string
	if err := recorder.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_learnings_workflow_run_id'`).Scan(&indexName); err != nil {
		t.Fatalf("idx_learnings_workflow_run_id が v1 fixture DB に additive 追加されていない: %v", err)
	}

	// learnings table の column 構成 (Req 5.1) が design.md L820-834 と一致
	wantLearnings := []columnSnapshot{
		{cid: 0, name: "id", ctype: "TEXT", notnull: 0, pk: 1},
		{cid: 1, name: "agent_call_id", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 2, name: "workflow_run_id", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 3, name: "workflow_id", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 4, name: "step_id", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 5, name: "category", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 6, name: "context", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 7, name: "insight", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 8, name: "paths_glob_json", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 9, name: "source", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 10, name: "recorded_at", ctype: "TEXT", notnull: 1, pk: 0},
	}
	assertColumnSnapshots(t, "learnings", pragmaTableInfo(t, recorder.db, "learnings"), wantLearnings)

	// (b) 既存 table の column 順序 / 名前 / 型 / NOT NULL / PK が不変 (Req 5.5, task 10.1 観測完了 2)。
	// learnings_schema_test.go の fresh DB 用 want と同一値を採用 (fresh DB / v1 migration いずれの
	// 経路でも同じ schema になることを担保: schema 定数 1 つから両経路が派生する設計の不変式)。
	wantWorkflowRuns := []columnSnapshot{
		{cid: 0, name: "id", ctype: "TEXT", notnull: 0, pk: 1},
		{cid: 1, name: "workflow_id", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 2, name: "workflow_name", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 3, name: "cwd", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 4, name: "project_id", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 5, name: "started_at", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 6, name: "finished_at", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 7, name: "error", ctype: "TEXT", notnull: 0, pk: 0},
	}
	assertColumnSnapshots(t, "workflow_runs", pragmaTableInfo(t, recorder.db, "workflow_runs"), wantWorkflowRuns)

	wantAgentCalls := []columnSnapshot{
		{cid: 0, name: "id", ctype: "TEXT", notnull: 0, pk: 1},
		{cid: 1, name: "workflow_run_id", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 2, name: "workflow_id", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 3, name: "workflow_name", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 4, name: "step_id", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 5, name: "step_name", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 6, name: "provider_id", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 7, name: "prompt", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 8, name: "cwd", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 9, name: "project_id", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 10, name: "resume_session_id", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 11, name: "output_format_type", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 12, name: "session_id", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 13, name: "success", ctype: "INTEGER", notnull: 0, pk: 0},
		{cid: 14, name: "duration_ms", ctype: "INTEGER", notnull: 0, pk: 0},
		{cid: 15, name: "total_cost_usd", ctype: "REAL", notnull: 0, pk: 0},
		{cid: 16, name: "input_tokens", ctype: "INTEGER", notnull: 0, pk: 0},
		{cid: 17, name: "output_tokens", ctype: "INTEGER", notnull: 0, pk: 0},
		{cid: 18, name: "result_summary", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 19, name: "error", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 20, name: "failure_category", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 21, name: "started_at", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 22, name: "finished_at", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 23, name: "result_payload_json", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 24, name: "user_corrected", ctype: "INTEGER", notnull: 1, pk: 0},
		{cid: 25, name: "model", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 26, name: "model_reasoning_effort", ctype: "TEXT", notnull: 0, pk: 0},
	}
	assertColumnSnapshots(t, "agent_calls", pragmaTableInfo(t, recorder.db, "agent_calls"), wantAgentCalls)

	wantRawEvents := []columnSnapshot{
		{cid: 0, name: "id", ctype: "INTEGER", notnull: 0, pk: 1},
		{cid: 1, name: "agent_call_id", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 2, name: "workflow_run_id", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 3, name: "sequence", ctype: "INTEGER", notnull: 1, pk: 0},
		{cid: 4, name: "source", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 5, name: "type", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 6, name: "subtype", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 7, name: "observed_at", ctype: "TEXT", notnull: 1, pk: 0},
		{cid: 8, name: "payload_json", ctype: "TEXT", notnull: 0, pk: 0},
		{cid: 9, name: "payload_truncated", ctype: "INTEGER", notnull: 1, pk: 0},
	}
	assertColumnSnapshots(t, "raw_events", pragmaTableInfo(t, recorder.db, "raw_events"), wantRawEvents)

	// (c) 既存 fixture rows が round-trip で読み取れる (task 10.1 観測完了 3: 既存行が破壊されない)。
	// workflow_runs / agent_calls / raw_events の各既存行に対して SELECT し、insert 時の値が
	// そのまま読み戻せることを確認。
	var wfID, wfName string
	if err := recorder.db.QueryRowContext(ctx,
		`SELECT workflow_id, workflow_name FROM workflow_runs WHERE id = ?`, "v1-run-1").Scan(&wfID, &wfName); err != nil {
		t.Fatalf("v1 fixture workflow_runs row が読めない: %v", err)
	}
	if wfID != "wf-1" || wfName != "V1 Workflow" {
		t.Fatalf("v1 fixture workflow_runs row が破壊された: id=%q name=%q", wfID, wfName)
	}

	var providerID, stepID string
	if err := recorder.db.QueryRowContext(ctx,
		`SELECT provider_id, step_id FROM agent_calls WHERE id = ?`, "v1-call-1").Scan(&providerID, &stepID); err != nil {
		t.Fatalf("v1 fixture agent_calls row が読めない: %v", err)
	}
	if providerID != "claude" || stepID != "step-1" {
		t.Fatalf("v1 fixture agent_calls row が破壊された: provider_id=%q step_id=%q", providerID, stepID)
	}

	var rawEventSource, rawEventType string
	var rawEventSequence int64
	if err := recorder.db.QueryRowContext(ctx,
		`SELECT source, type, sequence FROM raw_events WHERE agent_call_id = ?`,
		"v1-call-1").Scan(&rawEventSource, &rawEventType, &rawEventSequence); err != nil {
		t.Fatalf("v1 fixture raw_events row が読めない: %v", err)
	}
	if rawEventSource != "claude-cli" || rawEventType != "stdout" || rawEventSequence != 1 {
		t.Fatalf("v1 fixture raw_events row が破壊された: source=%q type=%q sequence=%d",
			rawEventSource, rawEventType, rawEventSequence)
	}

	// (d) Req 5.6: v1 (memory-distiller) は learnings table を必要とせず無視できる。
	// 新 learnings table が追加された後も、v1 既存行に対する UPDATE が成功することを確認。
	// この経路は v1 の `RecordWorkflowRunFinish` 等が最終的に発行する SQL と同等。
	if _, err := recorder.db.ExecContext(ctx,
		`UPDATE workflow_runs SET finished_at = ? WHERE id = ?`,
		"2026-05-01T00:00:10Z", "v1-run-1"); err != nil {
		t.Fatalf("v1 既存行への UPDATE が learnings 追加で破壊された: %v", err)
	}
	var finishedAt sql.NullString
	if err := recorder.db.QueryRowContext(ctx,
		`SELECT finished_at FROM workflow_runs WHERE id = ?`, "v1-run-1").Scan(&finishedAt); err != nil {
		t.Fatalf("UPDATE 後の workflow_runs.finished_at が読めない: %v", err)
	}
	if !finishedAt.Valid || finishedAt.String != "2026-05-01T00:00:10Z" {
		t.Fatalf("UPDATE が反映されていない: finished_at=%v", finishedAt)
	}
}
