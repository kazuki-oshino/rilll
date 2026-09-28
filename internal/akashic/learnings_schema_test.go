package akashic

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// columnSnapshot は PRAGMA table_info の 1 行を保持し、
// 既存 table の column 順序 / 名前 / 型 / NOT NULL 制約 / PK の不変性を verify するために使う。
type columnSnapshot struct {
	cid     int
	name    string
	ctype   string
	notnull int
	pk      int
}

// pragmaTableInfo は PRAGMA table_info(<table>) を実行し、column 順序のまま slice で返す。
// agent-learnings task 3.1 / Req 5.5 の「既存 table の column 順序 / 名前 / 型が不変」確認に使う。
func pragmaTableInfo(t *testing.T, db *sql.DB, table string) []columnSnapshot {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `PRAGMA table_info(`+table+`)`)
	if err != nil {
		t.Fatalf("pragma table_info(%s): %v", table, err)
	}
	defer rows.Close()
	var out []columnSnapshot
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
			t.Fatalf("scan pragma: %v", err)
		}
		out = append(out, columnSnapshot{cid: cid, name: name, ctype: ctype, notnull: notnull, pk: pk})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	return out
}

// assertColumnSnapshots は got と want を順序込みで比較する。
// 不一致の場合は cid 単位で詳細な diff を出力し、どの column が動いたかを特定可能にする。
func assertColumnSnapshots(t *testing.T, table string, got, want []columnSnapshot) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: column count mismatch: got=%d want=%d (got=%v)", table, len(got), len(want), got)
	}
	for i := range want {
		w := want[i]
		g := got[i]
		if g.cid != i {
			t.Errorf("%s[%d]: cid mismatch: got=%d (PRAGMA は cid を 0 から順に返すべき)", table, i, g.cid)
		}
		if g.name != w.name {
			t.Errorf("%s[%d]: name mismatch: got=%q want=%q", table, i, g.name, w.name)
		}
		if g.ctype != w.ctype {
			t.Errorf("%s[%d:%s]: type mismatch: got=%q want=%q", table, i, w.name, g.ctype, w.ctype)
		}
		if g.notnull != w.notnull {
			t.Errorf("%s[%d:%s]: notnull mismatch: got=%d want=%d", table, i, w.name, g.notnull, w.notnull)
		}
		if g.pk != w.pk {
			t.Errorf("%s[%d:%s]: pk mismatch: got=%d want=%d", table, i, w.name, g.pk, w.pk)
		}
	}
}

// TestSchemaCreatesLearningsTableOnFreshDB は agent-learnings task 3.1 / Req 5.1, 5.5 の核心:
// 空 DB で OpenSQLite を実行すると learnings table と idx_learnings_workflow_run_id index が
// sqlite_master 上に CREATE され、column 構成が design.md L820-834 の DDL と一致することを確認する。
func TestSchemaCreatesLearningsTableOnFreshDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fresh-learnings.sqlite")

	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	// learnings table が sqlite_master 上に存在する (Req 5.1)
	var tableName string
	if err := recorder.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='learnings'`).Scan(&tableName); err != nil {
		t.Fatalf("learnings テーブルが sqlite_master 上に存在しない: %v", err)
	}

	// idx_learnings_workflow_run_id index が sqlite_master 上に存在する
	var indexName string
	if err := recorder.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_learnings_workflow_run_id'`).Scan(&indexName); err != nil {
		t.Fatalf("idx_learnings_workflow_run_id index が sqlite_master 上に存在しない: %v", err)
	}

	// learnings table の column 順序 / 名前 / 型 / NOT NULL / PK が design.md L820-834 と一致 (Req 5.1)
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
	gotLearnings := pragmaTableInfo(t, recorder.db, "learnings")
	assertColumnSnapshots(t, "learnings", gotLearnings, wantLearnings)
}

// TestSchemaPreservesExistingTablesAfterLearningsAdded は agent-learnings task 3.1 / Req 5.5 の不変式:
// learnings table が schema 定数に追加された後も、既存 workflow_runs / agent_calls / raw_events の
// column 順序 / 名前 / 型 / NOT NULL 制約 / PK が変わっていないことを pin する。
//
// このテストは regression alarm 役で、誰かが既存 table の column を reorder / rename / 型変更すると失敗する。
// 既存 schema を意図的に変更したい場合はこのテストの期待値も同時に更新すること。
func TestSchemaPreservesExistingTablesAfterLearningsAdded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh-existing.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	// workflow_runs (8 columns) — Req 5.5: 既存 column 順序 / 名前 / 型 不変
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

	// agent_calls (27 columns) — Req 5.5: 既存 column 順序 / 名前 / 型 不変
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

	// raw_events (10 columns) — Req 5.5: 既存 column 順序 / 名前 / 型 不変
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
}
