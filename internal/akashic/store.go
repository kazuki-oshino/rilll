package akashic

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kazuki-oshino/rilll/internal/telemetry"
)

type WorkflowRun struct {
	ID           string
	WorkflowID   string
	WorkflowName string
	CWD          string
	// ProjectID は cwd 由来の project 絞り込みキー (ProjectIDFromCWD 由来)。
	// 空文字列で渡された場合は SQL 層で legacyProjectIDBackfillValue 等の既定値が適用されない点に注意し、
	// 呼び出し側は ProjectIDFromCWD(cwd) を渡すこと。
	ProjectID string
	StartedAt time.Time
}

type WorkflowRunFinish struct {
	ID         string
	Error      string
	FinishedAt time.Time
}

// WorkflowRunChild は workflow_run 間の親子関係を記録する。
//
// 親子関係は複合 workflow の child run を agent-learnings が後続で辿るための軽量 lineage で、
// workflow_runs / agent_calls の既存カラムは変更しない。
type WorkflowRunChild struct {
	ParentRunID       string
	ChildRunID        string
	ChildWorkflowID   string
	ChildWorkflowName string
	RecordedAt        time.Time
}

type AgentCall struct {
	ID            string
	WorkflowRunID string
	WorkflowID    string
	WorkflowName  string
	StepID        string
	StepName      string
	ProviderID    string
	Prompt        string
	CWD           string
	// ProjectID は cwd 由来の project 絞り込みキー (ProjectIDFromCWD 由来)。
	ProjectID        string
	ResumeSessionID  string
	OutputFormatType string
	StartedAt        time.Time
}

type AgentCallResult struct {
	ID                   string
	SessionID            string
	Success              bool
	DurationMS           int64
	TotalCostUSD         sql.NullFloat64
	InputTokens          sql.NullInt64
	OutputTokens         sql.NullInt64
	Model                string
	ModelReasoningEffort string
	ResultSummary        string
	Error                string
	// FailureCategory は失敗種別の分類タグ。`stream_idle_timeout` /
	// `external_abort` / `provider_error` 等。成功時は空文字列。
	FailureCategory   string
	FinishedAt        time.Time
	ResultPayloadJSON []byte
}

// RunArtifact は run_artifacts テーブルへの 1 件 upsert 入力。
//
// 設計契約 (akashic-artifacts spec / design.md "Recorder.RecordRunArtifact"):
//   - WorkflowRunID は必須 (空文字列は SQL 層で CHECK 違反として弾かれる)
//   - Kind は closed set 4 値 (proposal / review / result / context) のいずれか。
//     Recorder 自体は kind を再検証しない (検証責務は workflow/artifacts.go ヘルパに委譲)。
//   - Path は呼び出し側で正規化済みの絶対 clean path であることを前提とし、
//     Recorder 側で path 正規化や allowlist 再検証は行わない。
//   - Mtime は os.Stat().ModTime() の UTC 値を想定し、SQL 層では RFC3339Nano に整形して保存する。
//   - MIME は検出失敗時は空文字列。空文字列は DB では NULL として保存される。
type RunArtifact struct {
	WorkflowRunID string
	Kind          string
	Path          string
	SizeBytes     int64
	Mtime         time.Time
	MIME          string
}

type RawEvent struct {
	AgentCallID      string
	WorkflowRunID    string
	Sequence         int64
	Source           string
	Type             string
	Subtype          string
	ObservedAt       time.Time
	PayloadJSON      []byte
	PayloadTruncated bool
}

type Recorder interface {
	RecordWorkflowRunStart(context.Context, WorkflowRun) error
	RecordWorkflowRunFinish(context.Context, WorkflowRunFinish) error
	RecordAgentCallStart(context.Context, AgentCall) error
	RecordAgentCallFinish(context.Context, AgentCallResult) error
	RecordRawEvent(context.Context, RawEvent) error
	// RecordRunArtifact は run_artifacts に 1 件 upsert する (akashic-artifacts spec Req 1.1, 1.6)。
	// `(workflow_run_id, path)` 複合 PK で冪等。Path / Kind の検証は呼び出し側 (workflow/artifacts.go) の責務で、
	// Recorder 層は Go レイヤでの再検証を行わない。SQL 層では空文字列を CHECK で弾くことで防御層を維持する。
	RecordRunArtifact(ctx context.Context, artifact RunArtifact) error
	// RecordUserCorrection は agent_call に user-correction marker を立てる
	// (memory-distiller-trigger-policy spec Req 3.1)。
	//   - 同 agent_call_id への重複呼び出しは冪等 (UPDATE は SET 1 のみ; row 状態は不変)。
	//   - 存在しない agent_call_id への呼び出しは no-op (UPDATE 0 行で nil 返却 / cancel と finish の race safety)。
	//   - 呼び出しは agent.recordAgentCallFinish の同一 critical section 内で行うこと。
	RecordUserCorrection(ctx context.Context, agentCallID string) error
	// RecordLearning は learnings table に 1 行 INSERT する
	// (agent-learnings spec task 3.2 / Req 5.1, 5.2, 5.3)。
	//
	// 設計契約:
	//   - row.Category が ValidLearningCategory で false → ErrInvalidLearningCategory
	//     を sentinel として返し、INSERT を発行しない (Req 5.3)。
	//   - len(row.PathsGlob) > MaxPathsGlobEntries (5) → ErrTooManyPathsGlob を返し
	//     INSERT を発行しない (task 3.2 観測可能完了)。
	//   - learnings は append-only。同 row.ID を 2 度 record すると PK 衝突 error が
	//     上位に伝播する (ON CONFLICT 句なし)。
	RecordLearning(ctx context.Context, row LearningRow) error
	Close() error
}

type SQLiteRecorder struct {
	db *sql.DB
}

func OpenSQLite(path string) (*SQLiteRecorder, error) {
	dir := filepath.Dir(path)
	_, statErr := os.Stat(dir)
	createdDir := os.IsNotExist(statErr)
	if statErr != nil && !createdDir {
		return nil, statErr
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if createdDir {
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, err
		}
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &os.PathError{Op: "mkdir", Path: dir, Err: os.ErrExist}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	recorder := &SQLiteRecorder{db: db}
	if err := recorder.initialize(context.Background(), path); err != nil {
		_ = db.Close()
		return nil, err
	}
	return recorder, nil
}

func (r *SQLiteRecorder) initialize(ctx context.Context, path string) error {
	if err := r.db.PingContext(ctx); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	if _, err := r.db.ExecContext(ctx, `PRAGMA journal_mode = WAL`); err != nil {
		return err
	}
	if _, err := r.db.ExecContext(ctx, `PRAGMA busy_timeout = 5000`); err != nil {
		return err
	}
	if _, err := r.db.ExecContext(ctx, `PRAGMA synchronous = NORMAL`); err != nil {
		return err
	}
	if _, err := r.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	return r.applyColumnMigrations(ctx)
}

// applyColumnMigrations は既存 DB に対して未追加のカラムを足す。
// SQLite の `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` は未サポートのため、
// PRAGMA table_info で存在確認してから ALTER TABLE を発行する。
func (r *SQLiteRecorder) applyColumnMigrations(ctx context.Context) error {
	type columnSpec struct {
		table  string
		column string
		ddl    string
	}
	specs := []columnSpec{
		{table: "agent_calls", column: "failure_category", ddl: "ALTER TABLE agent_calls ADD COLUMN failure_category TEXT"},
		// project_id は新規 DB では schema 定数で `NOT NULL DEFAULT ''` として作られるが、
		// 既存 DB に追加する場合は SQLite の制約上 NOT NULL + DEFAULT '' で ALTER ADD する。
		// agent_calls 側は元々 cwd が NULLABLE のため project_id も NULLABLE のまま揃える。
		{table: "workflow_runs", column: "project_id", ddl: "ALTER TABLE workflow_runs ADD COLUMN project_id TEXT NOT NULL DEFAULT ''"},
		{table: "agent_calls", column: "project_id", ddl: "ALTER TABLE agent_calls ADD COLUMN project_id TEXT"},
		// user_corrected は user-correction marker (memory-distiller-trigger-policy Req 3.1, 3.5, 6.2, 6.3)。
		// DEFAULT 0 で既存 row が "no correction" として読める (backfill 不要 / Req 6.3)。
		// 集計は SUM(user_corrected) のみで filter には使わないため INDEX は作らない。
		{table: "agent_calls", column: "user_corrected", ddl: "ALTER TABLE agent_calls ADD COLUMN user_corrected INTEGER NOT NULL DEFAULT 0"},
		{table: "agent_calls", column: "model", ddl: "ALTER TABLE agent_calls ADD COLUMN model TEXT"},
		{table: "agent_calls", column: "model_reasoning_effort", ddl: "ALTER TABLE agent_calls ADD COLUMN model_reasoning_effort TEXT"},
	}
	for _, spec := range specs {
		exists, err := columnExists(ctx, r.db, spec.table, spec.column)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := r.db.ExecContext(ctx, spec.ddl); err != nil {
			return err
		}
	}
	if err := r.backfillLegacyProjectID(ctx); err != nil {
		return err
	}
	return r.ensureProjectIDIndexes(ctx)
}

// ensureProjectIDIndexes は project_id カラム確保後にインデックスを作成する。
// schema 定数に置くと legacy DB の ALTER TABLE 前に走り
// 「列が存在しない」エラーになるため、column migration の後に分離して実行する。
func (r *SQLiteRecorder) ensureProjectIDIndexes(ctx context.Context) error {
	stmts := []string{
		`CREATE INDEX IF NOT EXISTS idx_workflow_runs_project_id ON workflow_runs(project_id)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_calls_project_id ON agent_calls(project_id)`,
	}
	for _, s := range stmts {
		if _, err := r.db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// backfillLegacyProjectID は project_id が空 (legacy 未設定) のレコードを
// legacyProjectIDBackfillValue で一括 UPDATE する。冪等。
//
// 既存マシン (rilll 専用利用) を前提に "rilll" を直接書き込む。
// 既に project_id が入っている行 (新規記録 / 過去 backfill 済み) は触らない。
func (r *SQLiteRecorder) backfillLegacyProjectID(ctx context.Context) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE workflow_runs SET project_id = ? WHERE project_id = '' OR project_id IS NULL`,
		legacyProjectIDBackfillValue,
	); err != nil {
		return err
	}
	if _, err := r.db.ExecContext(ctx,
		`UPDATE agent_calls SET project_id = ? WHERE project_id = '' OR project_id IS NULL`,
		legacyProjectIDBackfillValue,
	); err != nil {
		return err
	}
	return nil
}

func columnExists(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (r *SQLiteRecorder) RecordWorkflowRunStart(ctx context.Context, run WorkflowRun) (err error) {
	_, _, finishTelemetry := telemetry.StartDBOperation(ctx, "workflow_runs.upsert_start")
	defer func() { finishTelemetry(err) }()
	_, err = r.db.ExecContext(ctx, `
INSERT INTO workflow_runs (id, workflow_id, workflow_name, cwd, project_id, started_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  workflow_id = excluded.workflow_id,
  workflow_name = excluded.workflow_name,
  cwd = excluded.cwd,
  project_id = excluded.project_id,
  started_at = excluded.started_at
`, run.ID, run.WorkflowID, run.WorkflowName, run.CWD, resolveProjectID(run.ProjectID, run.CWD), formatTime(run.StartedAt))
	return err
}

func (r *SQLiteRecorder) RecordWorkflowRunFinish(ctx context.Context, finish WorkflowRunFinish) (err error) {
	_, _, finishTelemetry := telemetry.StartDBOperation(ctx, "workflow_runs.update_finish")
	defer func() { finishTelemetry(err) }()
	_, err = r.db.ExecContext(ctx, `
UPDATE workflow_runs
SET finished_at = ?, error = ?
WHERE id = ?
`, formatTime(finish.FinishedAt), nullString(finish.Error), finish.ID)
	return err
}

func (r *SQLiteRecorder) RecordWorkflowRunChild(ctx context.Context, child WorkflowRunChild) (err error) {
	_, _, finishTelemetry := telemetry.StartDBOperation(ctx, "workflow_run_edges.upsert")
	defer func() { finishTelemetry(err) }()
	_, err = r.db.ExecContext(ctx, queryUpsertWorkflowRunChild,
		child.ParentRunID,
		child.ChildRunID,
		child.ChildWorkflowID,
		child.ChildWorkflowName,
		formatTime(child.RecordedAt),
	)
	return err
}

func (r *SQLiteRecorder) RecordAgentCallStart(ctx context.Context, call AgentCall) (err error) {
	_, _, finishTelemetry := telemetry.StartDBOperation(ctx, "agent_calls.upsert_start")
	defer func() { finishTelemetry(err) }()
	_, err = r.db.ExecContext(ctx, `
INSERT INTO agent_calls (
  id, workflow_run_id, workflow_id, workflow_name, step_id, step_name,
  provider_id, prompt, cwd, project_id, resume_session_id, output_format_type, started_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  workflow_run_id = excluded.workflow_run_id,
  workflow_id = excluded.workflow_id,
  workflow_name = excluded.workflow_name,
  step_id = excluded.step_id,
  step_name = excluded.step_name,
  provider_id = excluded.provider_id,
  prompt = excluded.prompt,
  cwd = excluded.cwd,
  project_id = excluded.project_id,
  resume_session_id = excluded.resume_session_id,
  output_format_type = excluded.output_format_type,
  started_at = excluded.started_at
`, call.ID, call.WorkflowRunID, call.WorkflowID, call.WorkflowName, call.StepID, call.StepName,
		call.ProviderID, call.Prompt, call.CWD, resolveProjectID(call.ProjectID, call.CWD),
		nullString(call.ResumeSessionID), nullString(call.OutputFormatType), formatTime(call.StartedAt))
	return err
}

func (r *SQLiteRecorder) RecordAgentCallFinish(ctx context.Context, result AgentCallResult) (err error) {
	_, _, finishTelemetry := telemetry.StartDBOperation(ctx, "agent_calls.upsert_finish")
	defer func() { finishTelemetry(err) }()
	_, err = r.db.ExecContext(ctx, `
INSERT INTO agent_calls (
  id, session_id, success, duration_ms, total_cost_usd, input_tokens,
  output_tokens, model, model_reasoning_effort, result_summary, error, failure_category,
  finished_at, result_payload_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  session_id = excluded.session_id,
  success = excluded.success,
  duration_ms = excluded.duration_ms,
  total_cost_usd = excluded.total_cost_usd,
  input_tokens = excluded.input_tokens,
  output_tokens = excluded.output_tokens,
  model = excluded.model,
  model_reasoning_effort = excluded.model_reasoning_effort,
  result_summary = excluded.result_summary,
  error = excluded.error,
  failure_category = excluded.failure_category,
  finished_at = excluded.finished_at,
  result_payload_json = excluded.result_payload_json
`, result.ID, nullString(result.SessionID), boolInt(result.Success), result.DurationMS, result.TotalCostUSD,
		result.InputTokens, result.OutputTokens, nullString(result.Model), nullString(result.ModelReasoningEffort),
		nullString(result.ResultSummary), nullString(result.Error), nullString(result.FailureCategory),
		formatTime(result.FinishedAt), nullBytes(result.ResultPayloadJSON))
	return err
}

func (r *SQLiteRecorder) RecordRawEvent(ctx context.Context, event RawEvent) (err error) {
	_, _, finishTelemetry := telemetry.StartDBOperation(ctx, "raw_events.insert")
	defer func() { finishTelemetry(err) }()
	result, err := r.db.ExecContext(ctx, `
INSERT INTO raw_events (
  agent_call_id, workflow_run_id, sequence, source, type, subtype,
  observed_at, payload_json, payload_truncated
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
`, event.AgentCallID, event.WorkflowRunID, event.Sequence, event.Source, event.Type, nullString(event.Subtype),
		formatTime(event.ObservedAt), nullBytes(event.PayloadJSON), boolInt(event.PayloadTruncated))
	if err != nil {
		return err
	}
	rawEventID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	return r.projectRawEvent(ctx, rawEventID, event)
}

func (r *SQLiteRecorder) projectRawEvent(ctx context.Context, rawEventID int64, event RawEvent) error {
	if event.AgentCallID == "" || event.Source != "claude-control" || event.Type != "context_usage" || event.PayloadTruncated || len(event.PayloadJSON) == 0 {
		return nil
	}
	var payload contextUsagePayload
	if err := json.Unmarshal(event.PayloadJSON, &payload); err != nil {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO agent_context_usage (
  agent_call_id, workflow_run_id, raw_event_id, sequence, observed_at, model,
  total_tokens, max_tokens, raw_max_tokens, percentage, is_auto_compact_enabled,
  auto_compact_threshold, categories_json, memory_files_json, mcp_tools_json,
  agents_json, message_breakdown_json, api_usage_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(agent_call_id) DO UPDATE SET
  workflow_run_id = excluded.workflow_run_id,
  raw_event_id = excluded.raw_event_id,
  sequence = excluded.sequence,
  observed_at = excluded.observed_at,
  model = excluded.model,
  total_tokens = excluded.total_tokens,
  max_tokens = excluded.max_tokens,
  raw_max_tokens = excluded.raw_max_tokens,
  percentage = excluded.percentage,
  is_auto_compact_enabled = excluded.is_auto_compact_enabled,
  auto_compact_threshold = excluded.auto_compact_threshold,
  categories_json = excluded.categories_json,
  memory_files_json = excluded.memory_files_json,
  mcp_tools_json = excluded.mcp_tools_json,
  agents_json = excluded.agents_json,
  message_breakdown_json = excluded.message_breakdown_json,
  api_usage_json = excluded.api_usage_json
WHERE excluded.sequence >= agent_context_usage.sequence
`, event.AgentCallID, event.WorkflowRunID, rawEventID, event.Sequence, formatTime(event.ObservedAt), nullString(payload.Model),
		payload.TotalTokens, payload.MaxTokens, payload.RawMaxTokens, payload.Percentage, boolInt(payload.IsAutoCompactEnabled),
		nullInt(payload.AutoCompactThreshold), nullBytes(payload.Categories), nullBytes(payload.MemoryFiles),
		nullBytes(payload.MCPTools), nullBytes(payload.Agents), nullBytes(payload.MessageBreakdown), nullBytes(payload.APIUsage))
	return err
}

// RecordRunArtifact は run_artifacts への 1 件 upsert を発行する
// (akashic-artifacts spec Req 1.1 / 1.6)。
//
// 設計契約:
//   - 入力 RunArtifact.Path は呼び出し側 (workflow/artifacts.go ヘルパ) で
//     EvalSymlinks 済みの絶対 clean path であることを前提とする。Recorder 自体は
//     path 正規化や allowlist 再検証を行わない (Recorder の責務は SQL 整合性のみ)。
//   - mtime は内部で formatTime により RFC3339Nano UTC に整形して保存する。
//   - MIME は空文字列のとき nullString により sql.NullString{Valid: false} となり
//     DB 上では NULL として保存される。
//   - 同 (workflow_run_id, path) で再呼び出しされたときは PK + ON CONFLICT DO UPDATE
//     により kind / size_bytes / mtime / mime が最新値で上書きされ、重複行は作らない。
func (r *SQLiteRecorder) RecordRunArtifact(ctx context.Context, artifact RunArtifact) (err error) {
	_, _, finishTelemetry := telemetry.StartDBOperation(ctx, "run_artifacts.upsert")
	defer func() { finishTelemetry(err) }()
	_, err = r.db.ExecContext(ctx, queryUpsertRunArtifact,
		artifact.WorkflowRunID,
		artifact.Path,
		artifact.Kind,
		artifact.SizeBytes,
		formatTime(artifact.Mtime),
		nullString(artifact.MIME),
	)
	return err
}

// RecordUserCorrection は agent_calls.user_corrected を 1 に立てる
// (memory-distiller-trigger-policy spec Req 3.1, 3.5, 6.3 / design.md L838-841)。
//
// 設計契約:
//   - SQL は `UPDATE agent_calls SET user_corrected = 1 WHERE id = ?` のみ。SET 1 は冪等で
//     重複呼び出しでも row 状態が変わらない。
//   - 存在しない agent_call_id への呼び出しは UPDATE 0 行で nil を返す (cancel と finish の
//     race safety net / design.md L840)。row を新規作成しない (INSERT しない)。
//   - 呼び出しは agent.recordAgentCallFinish の同一 critical section 内で行うこと
//     (design.md L859 の規約)。
func (r *SQLiteRecorder) RecordUserCorrection(ctx context.Context, agentCallID string) (err error) {
	_, _, finishTelemetry := telemetry.StartDBOperation(ctx, "agent_calls.update_user_corrected")
	defer func() { finishTelemetry(err) }()
	_, err = r.db.ExecContext(ctx, `UPDATE agent_calls SET user_corrected = 1 WHERE id = ?`, agentCallID)
	return err
}

func (r *SQLiteRecorder) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db.Close()
}

type NoopRecorder struct{}

func (NoopRecorder) RecordWorkflowRunStart(context.Context, WorkflowRun) error        { return nil }
func (NoopRecorder) RecordWorkflowRunFinish(context.Context, WorkflowRunFinish) error { return nil }
func (NoopRecorder) RecordWorkflowRunChild(context.Context, WorkflowRunChild) error   { return nil }
func (NoopRecorder) RecordAgentCallStart(context.Context, AgentCall) error            { return nil }
func (NoopRecorder) RecordAgentCallFinish(context.Context, AgentCallResult) error     { return nil }
func (NoopRecorder) RecordRawEvent(context.Context, RawEvent) error                   { return nil }
func (NoopRecorder) RecordRunArtifact(context.Context, RunArtifact) error             { return nil }
func (NoopRecorder) RecordUserCorrection(context.Context, string) error               { return nil }

// RecordLearning は agent-learnings spec (task 3.2) で Recorder interface に追加された
// メソッドへの no-op 追従。本 NoopRecorder は test / build 互換用 stub なので
// validation も DB 書き込みもせず、入力 row を黙って捨てる。
func (NoopRecorder) RecordLearning(context.Context, LearningRow) error { return nil }
func (NoopRecorder) Close() error                                      { return nil }

type contextUsagePayload struct {
	Categories           json.RawMessage `json:"categories"`
	TotalTokens          int64           `json:"totalTokens"`
	MaxTokens            int64           `json:"maxTokens"`
	RawMaxTokens         int64           `json:"rawMaxTokens"`
	Percentage           int64           `json:"percentage"`
	Model                string          `json:"model"`
	MemoryFiles          json.RawMessage `json:"memoryFiles"`
	MCPTools             json.RawMessage `json:"mcpTools"`
	Agents               json.RawMessage `json:"agents"`
	IsAutoCompactEnabled bool            `json:"isAutoCompactEnabled"`
	AutoCompactThreshold *int64          `json:"autoCompactThreshold"`
	MessageBreakdown     json.RawMessage `json:"messageBreakdown"`
	APIUsage             json.RawMessage `json:"apiUsage"`
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		t = time.Now().UTC()
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func nullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func nullBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}

func nullInt(value *int64) sql.NullInt64 {
	if value == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *value, Valid: true}
}

// resolveProjectID は呼び出し側が ProjectID を空で渡してきた場合に
// CWD から ProjectIDFromCWD で導出して返す defensive fallback。
// 既に non-empty な ProjectID が渡されていればそれを尊重する (呼び出し側責務優先)。
func resolveProjectID(projectID, cwd string) string {
	if projectID != "" {
		return projectID
	}
	return ProjectIDFromCWD(cwd)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

const schema = `
CREATE TABLE IF NOT EXISTS workflow_runs (
  id TEXT PRIMARY KEY,
  workflow_id TEXT NOT NULL,
  workflow_name TEXT NOT NULL,
  cwd TEXT NOT NULL,
  project_id TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  finished_at TEXT,
  error TEXT
);

CREATE TABLE IF NOT EXISTS agent_calls (
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

CREATE TABLE IF NOT EXISTS raw_events (
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

CREATE TABLE IF NOT EXISTS agent_context_usage (
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

CREATE TABLE IF NOT EXISTS run_artifacts (
  workflow_run_id TEXT NOT NULL CHECK (length(workflow_run_id) > 0),
  path TEXT NOT NULL CHECK (length(path) > 0),
  kind TEXT NOT NULL CHECK (length(kind) > 0),
  size_bytes INTEGER NOT NULL,
  mtime TEXT NOT NULL,
  mime TEXT,
  PRIMARY KEY (workflow_run_id, path)
);

CREATE TABLE IF NOT EXISTS workflow_run_edges (
  parent_run_id TEXT NOT NULL CHECK (length(parent_run_id) > 0),
  child_run_id TEXT NOT NULL CHECK (length(child_run_id) > 0),
  child_workflow_id TEXT NOT NULL DEFAULT '',
  child_workflow_name TEXT NOT NULL DEFAULT '',
  recorded_at TEXT NOT NULL,
  PRIMARY KEY (parent_run_id, child_run_id)
);

-- learnings は agent-learnings spec で additive 追加された table。
-- 1 行 = 1 つの「AI が反省して言語化した学習」。append-only / UPDATE / DELETE なし。
-- column 構成は agent-learnings design.md L820-834 と一致させること (Req 5.1)。
-- paths_glob は []string を JSON encode して paths_glob_json に格納する (closed enum / 上限は
-- task 3.2 の Recorder validation で強制)。
CREATE TABLE IF NOT EXISTS learnings (
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

-- learning_reflection_jobs は agent-learnings early reflector の durable queue。
-- agent_call_id を natural key にし、project scoped claim / poll は project_id 条件で行う。
CREATE TABLE IF NOT EXISTS learning_reflection_jobs (
  agent_call_id TEXT PRIMARY KEY,
  workflow_run_id TEXT NOT NULL,
  workflow_id TEXT NOT NULL,
  workflow_name TEXT NOT NULL,
  step_id TEXT NOT NULL,
  step_name TEXT NOT NULL,
  provider_id TEXT NOT NULL,
  cwd TEXT NOT NULL,
  project_id TEXT NOT NULL,
  session_id TEXT,
  status TEXT NOT NULL CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'skipped', 'stale', 'aggregated')),
  skip_reason TEXT,
  failure_reason TEXT,
  failure_detail TEXT,
  attempt_count INTEGER NOT NULL DEFAULT 0,
  max_attempts INTEGER NOT NULL DEFAULT 2,
  worker_id TEXT,
  lease_until TEXT,
  result_json TEXT,
  queued_at TEXT NOT NULL,
  started_at TEXT,
  finished_at TEXT,
  updated_at TEXT NOT NULL,
  aggregated_tail_run_id TEXT,
  aggregated_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_agent_calls_workflow_run_id ON agent_calls(workflow_run_id);
CREATE INDEX IF NOT EXISTS idx_raw_events_agent_call_id ON raw_events(agent_call_id);
CREATE INDEX IF NOT EXISTS idx_raw_events_type ON raw_events(type, subtype);
CREATE INDEX IF NOT EXISTS idx_agent_context_usage_workflow_run_id ON agent_context_usage(workflow_run_id);
CREATE INDEX IF NOT EXISTS idx_run_artifacts_workflow_run_id ON run_artifacts(workflow_run_id);
CREATE INDEX IF NOT EXISTS idx_workflow_run_edges_parent ON workflow_run_edges(parent_run_id);
CREATE INDEX IF NOT EXISTS idx_workflow_run_edges_child ON workflow_run_edges(child_run_id);
CREATE INDEX IF NOT EXISTS idx_learnings_workflow_run_id ON learnings(workflow_run_id);
CREATE INDEX IF NOT EXISTS idx_learning_reflection_jobs_workflow_run_id ON learning_reflection_jobs(workflow_run_id);
CREATE INDEX IF NOT EXISTS idx_learning_reflection_jobs_status ON learning_reflection_jobs(status);
CREATE INDEX IF NOT EXISTS idx_learning_reflection_jobs_project_status_lease ON learning_reflection_jobs(project_id, status, lease_until);
CREATE INDEX IF NOT EXISTS idx_learning_reflection_jobs_lease_until ON learning_reflection_jobs(lease_until);
`

// legacyProjectIDBackfillValue は既存 DB の project_id 空 (未マイグレーション) 行に
// 一括代入する値。本マシンは現状 rilll 専用利用のためハードコード。後続マシンで
// 同様の DB を再利用する際は別 migration で見直すこと。
const legacyProjectIDBackfillValue = "rilll"
