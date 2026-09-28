package akashic

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDefaultDBPathUsesEnvironmentOverride(t *testing.T) {
	// 旧 env のみ設定された fallback ケース。新 env は明示的に空に揃える。
	t.Setenv("RILLL_AKASHIC_DB", "")
	t.Setenv("RILLL_OBSERVABILITY_DB", "/tmp/custom-rilll.sqlite")
	resetDeprecationWarningOnce()

	got := DefaultDBPath()
	if got != "/tmp/custom-rilll.sqlite" {
		t.Fatalf("unexpected path: %q", got)
	}
}

func TestDefaultDBPathUsesXDGDataHome(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("RILLL_AKASHIC_DB", "")
	t.Setenv("RILLL_OBSERVABILITY_DB", "")
	t.Setenv("XDG_DATA_HOME", dataHome)
	resetDeprecationWarningOnce()

	got := DefaultDBPath()
	want := filepath.Join(dataHome, "rilll", "akashic.sqlite")
	if got != want {
		t.Fatalf("unexpected path: got %q want %q", got, want)
	}
}

func TestSQLiteRecorderCreatesPrivateDatabaseAndStoresRecords(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data", "observability.sqlite")

	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer recorder.Close()

	if info, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("database directory was not created: %v", err)
	} else if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("database directory permissions = %o, want 700", got)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("database file was not created: %v", err)
	} else if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("database file permissions = %o, want 600", got)
	}

	startedAt := time.Date(2026, 4, 28, 3, 0, 0, 0, time.UTC)
	if err := recorder.RecordWorkflowRunStart(ctx, WorkflowRun{
		ID:           "workflow-run-1",
		WorkflowID:   "update-readme",
		WorkflowName: "Update README",
		CWD:          "/repo",
		StartedAt:    startedAt,
	}); err != nil {
		t.Fatalf("RecordWorkflowRunStart returned error: %v", err)
	}
	if err := recorder.RecordAgentCallStart(ctx, AgentCall{
		ID:               "agent-call-1",
		WorkflowRunID:    "workflow-run-1",
		WorkflowID:       "update-readme",
		WorkflowName:     "Update README",
		StepID:           "plan",
		StepName:         "Plan README update",
		ProviderID:       "claude",
		Prompt:           "hello",
		CWD:              "/repo",
		ResumeSessionID:  "session-old",
		OutputFormatType: "json_schema",
		StartedAt:        startedAt.Add(time.Second),
	}); err != nil {
		t.Fatalf("RecordAgentCallStart returned error: %v", err)
	}
	if err := recorder.RecordRawEvent(ctx, RawEvent{
		AgentCallID:      "agent-call-1",
		WorkflowRunID:    "workflow-run-1",
		Sequence:         1,
		Source:           "claude-sdk",
		Type:             "result",
		Subtype:          "success",
		ObservedAt:       startedAt.Add(2 * time.Second),
		PayloadJSON:      []byte(`{"type":"result","total_cost_usd":0.12}`),
		PayloadTruncated: false,
	}); err != nil {
		t.Fatalf("RecordRawEvent returned error: %v", err)
	}
	if err := recorder.RecordAgentCallFinish(ctx, AgentCallResult{
		ID:                "agent-call-1",
		SessionID:         "session-new",
		Success:           true,
		DurationMS:        1234,
		TotalCostUSD:      sql.NullFloat64{Float64: 0.12, Valid: true},
		InputTokens:       sql.NullInt64{Int64: 10, Valid: true},
		OutputTokens:      sql.NullInt64{Int64: 4, Valid: true},
		ResultSummary:     "done",
		FinishedAt:        startedAt.Add(3 * time.Second),
		ResultPayloadJSON: []byte(`{"subtype":"success"}`),
	}); err != nil {
		t.Fatalf("RecordAgentCallFinish returned error: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open database for assertions: %v", err)
	}
	defer db.Close()

	var workflowCount, callCount, rawCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_runs`).Scan(&workflowCount); err != nil {
		t.Fatalf("query workflow_runs: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_calls WHERE id = 'agent-call-1' AND success = 1 AND total_cost_usd = 0.12`).Scan(&callCount); err != nil {
		t.Fatalf("query agent_calls: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM raw_events WHERE agent_call_id = 'agent-call-1' AND source = 'claude-sdk'`).Scan(&rawCount); err != nil {
		t.Fatalf("query raw_events: %v", err)
	}
	if workflowCount != 1 || callCount != 1 || rawCount != 1 {
		t.Fatalf("unexpected counts: workflow=%d call=%d raw=%d", workflowCount, callCount, rawCount)
	}
}

func TestOpenSQLiteDoesNotChmodExistingParentDirectory(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatalf("failed to set parent permissions: %v", err)
	}

	recorder, err := OpenSQLite(filepath.Join(parent, "observability.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer recorder.Close()

	info, err := os.Stat(parent)
	if err != nil {
		t.Fatalf("stat parent: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("existing parent permissions = %o, want 755", got)
	}
}

func TestRecordAgentCallFinishPersistsFailureCategory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer recorder.Close()

	if err := recorder.RecordAgentCallFinish(ctx, AgentCallResult{
		ID:              "agent-call-idle-1",
		Success:         false,
		Error:           "[stream_idle_timeout] stream idle timeout",
		FailureCategory: "stream_idle_timeout",
		FinishedAt:      time.Now().UTC(),
		DurationMS:      1000,
	}); err != nil {
		t.Fatalf("RecordAgentCallFinish returned error: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	var category string
	if err := db.QueryRowContext(ctx, `SELECT failure_category FROM agent_calls WHERE id = 'agent-call-idle-1'`).Scan(&category); err != nil {
		t.Fatalf("query failure_category: %v", err)
	}
	if category != "stream_idle_timeout" {
		t.Fatalf("failure_category not persisted, got %q", category)
	}
}

func TestRecordAgentCallFinishPersistsModelAndEffort(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer recorder.Close()

	if err := recorder.RecordAgentCallFinish(ctx, AgentCallResult{
		ID:                   "agent-call-model-1",
		Success:              true,
		Model:                "gpt-5.3-codex-spark",
		ModelReasoningEffort: "high",
		FinishedAt:           time.Now().UTC(),
		DurationMS:           10,
	}); err != nil {
		t.Fatalf("RecordAgentCallFinish returned error: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	var model, effort string
	if err := db.QueryRowContext(ctx,
		`SELECT model, model_reasoning_effort FROM agent_calls WHERE id = 'agent-call-model-1'`,
	).Scan(&model, &effort); err != nil {
		t.Fatalf("query model/effort: %v", err)
	}
	if model != "gpt-5.3-codex-spark" {
		t.Fatalf("model not persisted, got %q", model)
	}
	if effort != "high" {
		t.Fatalf("model_reasoning_effort not persisted, got %q", effort)
	}
}

func TestOpenSQLiteAddsFailureCategoryColumnToLegacyDB(t *testing.T) {
	// 既存 DB (failure_category カラムがない) を模擬し、OpenSQLite が ALTER TABLE で
	// カラムを追加することを確認する。
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacy.ExecContext(ctx, `
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
  started_at TEXT,
  finished_at TEXT,
  result_payload_json TEXT
);`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer recorder.Close()

	if err := recorder.RecordAgentCallFinish(ctx, AgentCallResult{
		ID:              "agent-call-after-migration",
		Success:         false,
		FailureCategory: "stream_idle_timeout",
		FinishedAt:      time.Now().UTC(),
		DurationMS:      10,
	}); err != nil {
		t.Fatalf("RecordAgentCallFinish after migration: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db.Close()

	var category string
	if err := db.QueryRowContext(ctx, `SELECT failure_category FROM agent_calls WHERE id = 'agent-call-after-migration'`).Scan(&category); err != nil {
		t.Fatalf("query migrated column: %v", err)
	}
	if category != "stream_idle_timeout" {
		t.Fatalf("failure_category not stored after migration: %q", category)
	}
}

func TestRecordAgentCallFinishCreatesRowWhenStartWasMissing(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "observability.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer recorder.Close()

	if err := recorder.RecordAgentCallFinish(ctx, AgentCallResult{
		ID:         "agent-call-missing-start",
		SessionID:  "session-1",
		Success:    false,
		Error:      "cancelled",
		FinishedAt: time.Now().UTC(),
		DurationMS: 10,
	}); err != nil {
		t.Fatalf("RecordAgentCallFinish returned error: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open database for assertions: %v", err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_calls WHERE id = 'agent-call-missing-start' AND error = 'cancelled'`).Scan(&count); err != nil {
		t.Fatalf("query agent_calls: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected finish-only row to be created, count=%d", count)
	}
}

func TestRecordRawEventKeepsLatestContextUsagePerAgentCall(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "observability.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer recorder.Close()

	observedAt := time.Date(2026, 4, 28, 5, 0, 0, 0, time.UTC)
	if err := recorder.RecordRawEvent(ctx, RawEvent{
		AgentCallID:   "agent-call-1",
		WorkflowRunID: "workflow-run-1",
		Sequence:      3,
		Source:        "claude-control",
		Type:          "context_usage",
		ObservedAt:    observedAt,
		PayloadJSON:   contextUsagePayloadJSON(17055, 9),
	}); err != nil {
		t.Fatalf("RecordRawEvent returned error: %v", err)
	}
	if err := recorder.RecordRawEvent(ctx, RawEvent{
		AgentCallID:   "agent-call-1",
		WorkflowRunID: "workflow-run-1",
		Sequence:      8,
		Source:        "claude-control",
		Type:          "context_usage",
		ObservedAt:    observedAt.Add(time.Second),
		PayloadJSON:   contextUsagePayloadJSON(22930, 11),
	}); err != nil {
		t.Fatalf("RecordRawEvent returned error: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open database for assertions: %v", err)
	}
	defer db.Close()

	var rawCount, contextUsageCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM raw_events WHERE agent_call_id = 'agent-call-1'`).Scan(&rawCount); err != nil {
		t.Fatalf("query raw_events: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_context_usage WHERE agent_call_id = 'agent-call-1'`).Scan(&contextUsageCount); err != nil {
		t.Fatalf("query agent_context_usage count: %v", err)
	}
	if rawCount != 2 || contextUsageCount != 1 {
		t.Fatalf("unexpected counts: raw=%d context_usage=%d", rawCount, contextUsageCount)
	}

	var sequence int
	var rawEventID int64
	var totalTokens, maxTokens, rawMaxTokens, percentage, autoCompactEnabled, autoCompactThreshold int
	var agentCallID, workflowRunID, model, observedAtText, categoriesJSON, memoryFilesJSON, mcpToolsJSON, agentsJSON, messageBreakdownJSON, apiUsageJSON string
	err = db.QueryRowContext(ctx, `
SELECT agent_call_id, workflow_run_id, raw_event_id, sequence, observed_at, model, total_tokens, max_tokens, raw_max_tokens,
  percentage, is_auto_compact_enabled, auto_compact_threshold, categories_json, memory_files_json,
  mcp_tools_json, agents_json, message_breakdown_json, api_usage_json
FROM agent_context_usage
WHERE agent_call_id = 'agent-call-1'
`).Scan(&agentCallID, &workflowRunID, &rawEventID, &sequence, &observedAtText, &model, &totalTokens, &maxTokens, &rawMaxTokens,
		&percentage, &autoCompactEnabled, &autoCompactThreshold, &categoriesJSON, &memoryFilesJSON,
		&mcpToolsJSON, &agentsJSON, &messageBreakdownJSON, &apiUsageJSON)
	if err != nil {
		t.Fatalf("query agent_context_usage: %v", err)
	}
	if rawEventID == 0 {
		t.Fatal("raw_event_id was not recorded")
	}
	if agentCallID != "agent-call-1" || workflowRunID != "workflow-run-1" || observedAtText != observedAt.Add(time.Second).Format(time.RFC3339Nano) {
		t.Fatalf("context usage metadata mismatch: call=%q workflow=%q observed=%q", agentCallID, workflowRunID, observedAtText)
	}
	if sequence != 8 || model != "global.anthropic.claude-opus-4-6-v1" || totalTokens != 22930 || maxTokens != 200000 || rawMaxTokens != 200000 || percentage != 11 {
		t.Fatalf("context usage metrics mismatch: sequence=%d model=%q total=%d max=%d rawMax=%d percentage=%d", sequence, model, totalTokens, maxTokens, rawMaxTokens, percentage)
	}
	if autoCompactEnabled != 1 || autoCompactThreshold != 95000 {
		t.Fatalf("autocompact fields mismatch: enabled=%d threshold=%d", autoCompactEnabled, autoCompactThreshold)
	}
	for name, value := range map[string]string{
		"categories":        categoriesJSON,
		"memory_files":      memoryFilesJSON,
		"mcp_tools":         mcpToolsJSON,
		"agents":            agentsJSON,
		"message_breakdown": messageBreakdownJSON,
		"api_usage":         apiUsageJSON,
	} {
		if value == "" || value == "null" {
			t.Fatalf("%s json was not preserved: %q", name, value)
		}
	}
}

func TestRecordRawEventDoesNotOverwriteContextUsageWithOlderSequence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "observability.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer recorder.Close()

	observedAt := time.Date(2026, 4, 28, 5, 0, 0, 0, time.UTC)
	if err := recorder.RecordRawEvent(ctx, RawEvent{
		AgentCallID:   "agent-call-1",
		WorkflowRunID: "workflow-run-1",
		Sequence:      10,
		Source:        "claude-control",
		Type:          "context_usage",
		ObservedAt:    observedAt,
		PayloadJSON:   contextUsagePayloadJSON(30000, 15),
	}); err != nil {
		t.Fatalf("RecordRawEvent returned error: %v", err)
	}
	if err := recorder.RecordRawEvent(ctx, RawEvent{
		AgentCallID:   "agent-call-1",
		WorkflowRunID: "workflow-run-1",
		Sequence:      4,
		Source:        "claude-control",
		Type:          "context_usage",
		ObservedAt:    observedAt.Add(time.Second),
		PayloadJSON:   contextUsagePayloadJSON(10000, 5),
	}); err != nil {
		t.Fatalf("RecordRawEvent returned error: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open database for assertions: %v", err)
	}
	defer db.Close()

	var rawCount, contextUsageCount, sequence, totalTokens int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM raw_events WHERE agent_call_id = 'agent-call-1'`).Scan(&rawCount); err != nil {
		t.Fatalf("query raw_events: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*), sequence, total_tokens FROM agent_context_usage WHERE agent_call_id = 'agent-call-1'`).Scan(&contextUsageCount, &sequence, &totalTokens); err != nil {
		t.Fatalf("query agent_context_usage: %v", err)
	}
	if rawCount != 2 {
		t.Fatalf("raw event was not preserved, count=%d", rawCount)
	}
	if contextUsageCount != 1 || sequence != 10 || totalTokens != 30000 {
		t.Fatalf("older sequence overwrote latest context usage: count=%d sequence=%d total=%d", contextUsageCount, sequence, totalTokens)
	}
}

func TestRecordRawEventSkipsContextUsageProjectionWhenPayloadIsInvalid(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "observability.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer recorder.Close()

	if err := recorder.RecordRawEvent(ctx, RawEvent{
		AgentCallID:   "agent-call-1",
		WorkflowRunID: "workflow-run-1",
		Sequence:      1,
		Source:        "claude-control",
		Type:          "context_usage",
		ObservedAt:    time.Now().UTC(),
		PayloadJSON:   []byte(`{not-json`),
	}); err != nil {
		t.Fatalf("RecordRawEvent returned error: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open database for assertions: %v", err)
	}
	defer db.Close()

	var rawCount, snapshotCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM raw_events`).Scan(&rawCount); err != nil {
		t.Fatalf("query raw_events: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_context_usage`).Scan(&snapshotCount); err != nil {
		t.Fatalf("query agent_context_usage: %v", err)
	}
	if rawCount != 1 || snapshotCount != 0 {
		t.Fatalf("unexpected counts: raw=%d snapshots=%d", rawCount, snapshotCount)
	}
}

func TestRecordRawEventDoesNotProjectUnsupportedEvents(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "observability.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer recorder.Close()

	if err := recorder.RecordRawEvent(ctx, RawEvent{
		AgentCallID:   "agent-call-1",
		WorkflowRunID: "workflow-run-1",
		Sequence:      1,
		Source:        "claude-hook",
		Type:          "hook",
		Subtype:       "PreToolUse",
		ObservedAt:    time.Now().UTC(),
		PayloadJSON:   []byte(`{"input":{"tool_name":"Bash"}}`),
	}); err != nil {
		t.Fatalf("RecordRawEvent returned error: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open database for assertions: %v", err)
	}
	defer db.Close()

	var rawCount, snapshotCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM raw_events`).Scan(&rawCount); err != nil {
		t.Fatalf("query raw_events: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_context_usage`).Scan(&snapshotCount); err != nil {
		t.Fatalf("query agent_context_usage: %v", err)
	}
	if rawCount != 1 || snapshotCount != 0 {
		t.Fatalf("unexpected counts: raw=%d snapshots=%d", rawCount, snapshotCount)
	}
}

// TestSQLiteRecorderRecordRunArtifactUpsertIsIdempotent は同じ
// (workflow_run_id, path) で 2 回 RecordRunArtifact を呼んだとき、
// 1 行のみが残り、kind / size_bytes / mtime / mime が最新値で上書きされることを検証する
// (akashic-artifacts spec Req 1.6 の冪等 upsert)。
func TestSQLiteRecorderRecordRunArtifactUpsertIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer recorder.Close()

	// .rilll 配下の絶対 clean path を想定 (Recorder は再検証しない前提)
	artifactPath := "/tmp/example/.rilll/wf/run-1/01-proposal.md"
	firstMtime := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)
	secondMtime := firstMtime.Add(15 * time.Minute)

	// 1 回目: proposal kind / 100 bytes / 空 mime
	if err := recorder.RecordRunArtifact(ctx, RunArtifact{
		WorkflowRunID: "run-1",
		Kind:          ArtifactKindProposal,
		Path:          artifactPath,
		SizeBytes:     100,
		Mtime:         firstMtime,
		MIME:          "",
	}); err != nil {
		t.Fatalf("first RecordRunArtifact returned error: %v", err)
	}

	// 2 回目: 同 (run_id, path) で kind / size / mtime / mime を更新
	if err := recorder.RecordRunArtifact(ctx, RunArtifact{
		WorkflowRunID: "run-1",
		Kind:          ArtifactKindResult,
		Path:          artifactPath,
		SizeBytes:     250,
		Mtime:         secondMtime,
		MIME:          "text/markdown",
	}); err != nil {
		t.Fatalf("second RecordRunArtifact returned error: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// 行数: 同 (run_id, path) で 2 回呼んでも 1 行
	var rowCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run_artifacts WHERE workflow_run_id = ? AND path = ?`,
		"run-1", artifactPath).Scan(&rowCount); err != nil {
		t.Fatalf("count run_artifacts: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("upsert did not collapse to 1 row: got %d", rowCount)
	}

	// 列値: 最新値で上書きされている
	var (
		kind      string
		sizeBytes int64
		mtimeText string
		mime      sql.NullString
	)
	if err := db.QueryRowContext(ctx,
		`SELECT kind, size_bytes, mtime, mime FROM run_artifacts WHERE workflow_run_id = ? AND path = ?`,
		"run-1", artifactPath).Scan(&kind, &sizeBytes, &mtimeText, &mime); err != nil {
		t.Fatalf("scan run_artifacts: %v", err)
	}
	if kind != ArtifactKindResult {
		t.Fatalf("kind not overwritten: got %q, want %q", kind, ArtifactKindResult)
	}
	if sizeBytes != 250 {
		t.Fatalf("size_bytes not overwritten: got %d, want 250", sizeBytes)
	}
	if mtimeText != secondMtime.Format(time.RFC3339Nano) {
		t.Fatalf("mtime not overwritten with RFC3339Nano UTC: got %q, want %q",
			mtimeText, secondMtime.Format(time.RFC3339Nano))
	}
	if !mime.Valid || mime.String != "text/markdown" {
		t.Fatalf("mime not overwritten: got %#v", mime)
	}
}

// TestSQLiteRecorderRecordRunArtifactPersistsRowMetadata は RecordRunArtifact が
// SQLite テーブルに RFC3339Nano UTC mtime と nullable MIME (空文字列 → NULL) を
// 期待通り記録することを検証する (Req 1.1)。
func TestSQLiteRecorderRecordRunArtifactPersistsRowMetadata(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer recorder.Close()

	mtime := time.Date(2026, 5, 2, 12, 30, 45, 123456789, time.UTC)
	if err := recorder.RecordRunArtifact(ctx, RunArtifact{
		WorkflowRunID: "run-2",
		Kind:          ArtifactKindContext,
		Path:          "/tmp/example/.rilll/wf/run-2/00-context.json",
		SizeBytes:     42,
		Mtime:         mtime,
		MIME:          "", // 空文字列 → DB では NULL に変換される (nullString)
	}); err != nil {
		t.Fatalf("RecordRunArtifact returned error: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	var (
		runID, kind, mtimeText string
		sizeBytes              int64
		mime                   sql.NullString
	)
	if err := db.QueryRowContext(ctx,
		`SELECT workflow_run_id, kind, size_bytes, mtime, mime FROM run_artifacts WHERE workflow_run_id = ?`,
		"run-2").Scan(&runID, &kind, &sizeBytes, &mtimeText, &mime); err != nil {
		t.Fatalf("scan run_artifacts: %v", err)
	}
	if runID != "run-2" || kind != ArtifactKindContext || sizeBytes != 42 {
		t.Fatalf("scalar columns mismatch: run_id=%q kind=%q size=%d", runID, kind, sizeBytes)
	}
	if mtimeText != mtime.Format(time.RFC3339Nano) {
		t.Fatalf("mtime not RFC3339Nano UTC: got %q, want %q", mtimeText, mtime.Format(time.RFC3339Nano))
	}
	if mime.Valid {
		t.Fatalf("空 MIME は NULL として保存されるべき: got %#v", mime)
	}
}

// TestSQLiteRecorderRecordRunArtifactRejectsEmptyKindAndPath は SQL 層で
// 必須カラム (kind / path) の空文字列を拒否することを検証する。
//
// 設計上 Recorder は呼び出し側からの入力を信頼し Go レイヤでは再検証しない
// (validation 責務は workflow/artifacts.go ヘルパに委譲)。ただし防御層として
// SQLite の CHECK 制約で空文字列を弾き、誤って空文字列が紛れ込んだ場合に
// SQL 層でエラーが表面化する不変式を保つ (akashic-artifacts spec Req 1.7 / 完了条件 b)。
func TestSQLiteRecorderRecordRunArtifactRejectsEmptyKindAndPath(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer recorder.Close()

	mtime := time.Now().UTC()

	cases := []struct {
		name string
		art  RunArtifact
	}{
		{
			name: "kind empty",
			art: RunArtifact{
				WorkflowRunID: "run-x",
				Kind:          "", // 空文字列 → SQL 層で CHECK 違反
				Path:          "/tmp/example/.rilll/wf/run-x/result.json",
				SizeBytes:     1,
				Mtime:         mtime,
			},
		},
		{
			name: "path empty",
			art: RunArtifact{
				WorkflowRunID: "run-x",
				Kind:          ArtifactKindResult,
				Path:          "", // 空文字列 → SQL 層で CHECK 違反
				SizeBytes:     1,
				Mtime:         mtime,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := recorder.RecordRunArtifact(ctx, tc.art)
			if err == nil {
				t.Fatalf("expected SQL layer error for empty %s, got nil", tc.name)
			}
			// modernc.org/sqlite はメッセージで CHECK constraint を返す。
			// テストはメッセージ依存を最小化するため、空でないことだけを確認する。
			if strings.TrimSpace(err.Error()) == "" {
				t.Fatalf("expected non-empty error message, got %q", err.Error())
			}
		})
	}
}

// TestSQLiteRecorderRecordUserCorrectionMarksRow は RecordUserCorrection が
// 対象 agent_call の user_corrected 列を 1 に立てることを検証する
// (memory-distiller-trigger-policy task 3.2, Req 3.1)。
func TestSQLiteRecorderRecordUserCorrectionMarksRow(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer recorder.Close()

	// 事前に agent_call row を作る (start のみで finish 不要 / user-correction は finish と独立)
	if err := recorder.RecordAgentCallStart(ctx, AgentCall{
		ID:            "agent-call-uc-1",
		WorkflowRunID: "run-uc-1",
		WorkflowID:    "wf",
		WorkflowName:  "wf",
		StepID:        "step",
		StepName:      "step",
		ProviderID:    "claude",
		Prompt:        "hi",
		CWD:           "/repo",
		StartedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RecordAgentCallStart returned error: %v", err)
	}

	if err := recorder.RecordUserCorrection(ctx, "agent-call-uc-1"); err != nil {
		t.Fatalf("RecordUserCorrection returned error: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	var marker int
	if err := db.QueryRowContext(ctx,
		`SELECT user_corrected FROM agent_calls WHERE id = ?`, "agent-call-uc-1").Scan(&marker); err != nil {
		t.Fatalf("query user_corrected: %v", err)
	}
	if marker != 1 {
		t.Fatalf("user_corrected was not flipped: got %d, want 1", marker)
	}
}

// TestSQLiteRecorderRecordUserCorrectionIsIdempotent は同 agent_call_id への
// 重複呼び出しが冪等 (常に user_corrected = 1) であることを検証する
// (memory-distiller-trigger-policy task 3.2, Req 3.5, design.md L839)。
func TestSQLiteRecorderRecordUserCorrectionIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer recorder.Close()

	if err := recorder.RecordAgentCallStart(ctx, AgentCall{
		ID:            "agent-call-uc-idem",
		WorkflowRunID: "run-uc-idem",
		WorkflowID:    "wf",
		WorkflowName:  "wf",
		StepID:        "step",
		StepName:      "step",
		ProviderID:    "claude",
		Prompt:        "hi",
		CWD:           "/repo",
		StartedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RecordAgentCallStart returned error: %v", err)
	}

	// 2 回呼び出しても結果は user_corrected = 1 のまま (no-op)
	for i := range 2 {
		if err := recorder.RecordUserCorrection(ctx, "agent-call-uc-idem"); err != nil {
			t.Fatalf("RecordUserCorrection (call #%d) returned error: %v", i+1, err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	var marker, rowCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MAX(user_corrected), -1) FROM agent_calls WHERE id = ?`,
		"agent-call-uc-idem").Scan(&rowCount, &marker); err != nil {
		t.Fatalf("query user_corrected: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("idempotent UPDATE must not duplicate rows: got %d", rowCount)
	}
	if marker != 1 {
		t.Fatalf("user_corrected after duplicate calls: got %d, want 1", marker)
	}
}

// TestSQLiteRecorderRecordUserCorrectionMissingIDIsNoOp は存在しない
// agent_call_id への呼び出しが UPDATE 0 行で error なし (cancel と finish の race safety net)
// であることを検証する (memory-distiller-trigger-policy task 3.2, design.md L840)。
func TestSQLiteRecorderRecordUserCorrectionMissingIDIsNoOp(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer recorder.Close()

	// 何も insert せずに直接 RecordUserCorrection を呼ぶ (存在しない id)
	if err := recorder.RecordUserCorrection(ctx, "agent-call-does-not-exist"); err != nil {
		t.Fatalf("RecordUserCorrection on missing id should be no-op: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// row が新規作成されていないこと (UPDATE は INSERT しないため当然だが念のため)
	var rowCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agent_calls WHERE id = ?`,
		"agent-call-does-not-exist").Scan(&rowCount); err != nil {
		t.Fatalf("query agent_calls: %v", err)
	}
	if rowCount != 0 {
		t.Fatalf("RecordUserCorrection must not insert a row on missing id: got %d", rowCount)
	}
}

// TestNoopRecorderRecordUserCorrectionReturnsNil は NoopRecorder が
// RecordUserCorrection を呼んでも常に nil を返すことを検証する
// (memory-distiller-trigger-policy task 3.2)。
func TestNoopRecorderRecordUserCorrectionReturnsNil(t *testing.T) {
	var rec Recorder = NoopRecorder{}
	if err := rec.RecordUserCorrection(context.Background(), "any-id"); err != nil {
		t.Fatalf("NoopRecorder.RecordUserCorrection returned error: %v", err)
	}
	if err := rec.RecordUserCorrection(context.Background(), ""); err != nil {
		t.Fatalf("NoopRecorder.RecordUserCorrection (empty id) returned error: %v", err)
	}
}

// TestNoopRecorderRecordRunArtifactReturnsNil は NoopRecorder が
// RecordRunArtifact を呼んでも常に nil を返すことを検証する (akashic-artifacts spec
// 完了条件 c)。NoopRecorder は他の Record* メソッドと同じく副作用なし契約で、
// Recorder interface の変数経由 / 空 RunArtifact のいずれの呼び出しでも nil を返す。
func TestNoopRecorderRecordRunArtifactReturnsNil(t *testing.T) {
	var rec Recorder = NoopRecorder{}
	if err := rec.RecordRunArtifact(context.Background(), RunArtifact{
		WorkflowRunID: "run-noop",
		Kind:          ArtifactKindProposal,
		Path:          "/anywhere",
		SizeBytes:     0,
		Mtime:         time.Time{},
	}); err != nil {
		t.Fatalf("NoopRecorder.RecordRunArtifact returned error: %v", err)
	}
	if err := rec.RecordRunArtifact(context.Background(), RunArtifact{}); err != nil {
		t.Fatalf("NoopRecorder.RecordRunArtifact (empty input) returned error: %v", err)
	}
}

func contextUsagePayloadJSON(totalTokens int, percentage int) []byte {
	return []byte(`{
		"categories":[
			{"name":"Messages","tokens":123,"color":"purple_FOR_SUBAGENTS_ONLY"},
			{"name":"MCP tools (deferred)","tokens":45,"color":"inactive","isDeferred":true}
		],
		"totalTokens":` + strconv.Itoa(totalTokens) + `,
		"maxTokens":200000,
		"rawMaxTokens":200000,
		"percentage":` + strconv.Itoa(percentage) + `,
		"model":"global.anthropic.claude-opus-4-6-v1",
		"memoryFiles":[{"path":"CLAUDE.md","type":"project","tokens":1409}],
		"mcpTools":[{"name":"query-docs","serverName":"context7","tokens":1259,"isLoaded":false}],
		"agents":[{"agentType":"general","source":"builtin","tokens":139}],
		"isAutoCompactEnabled":true,
		"autoCompactThreshold":95000,
		"messageBreakdown":{"toolCallTokens":10,"toolResultTokens":20},
		"apiUsage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":3,"cache_read_input_tokens":4}
	}`)
}
