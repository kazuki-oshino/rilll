package akashic

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// migrateTestPaths はテスト共通の path セットを返す。
// 各テストで独立した一時ディレクトリを生成し、新名 / 旧名 / dir をまとめて返す。
func migrateTestPaths(t *testing.T) (newPath, oldPath, dir string) {
	t.Helper()
	dir = t.TempDir()
	newPath = filepath.Join(dir, "akashic.sqlite")
	oldPath = filepath.Join(dir, "observability.sqlite")
	return
}

// writeFile は指定 path にバイト列を書き込み、0o600 権限を設定する。
func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("failed to write %s: %v", path, err)
	}
}

// fileSHA256 は path のファイルの SHA256 ハッシュを 16 進文字列で返す。
// データ無改変担保 (Req 2.7) の検証に使用する。
func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("failed to open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("failed to hash %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ケース 1: 旧単独 (旧 observability.sqlite のみ存在)
// → 新 path への rename 成功 + 移行通知 warning + データ無改変 + 権限 0o600 維持
func TestMigrateRenamesOldToNew(t *testing.T) {
	newPath, oldPath, _ := migrateTestPaths(t)
	content := []byte("legacy sqlite binary data")
	writeFile(t, oldPath, content)
	beforeHash := fileSHA256(t, oldPath)

	got := Migrate(newPath)

	if got.Path != newPath {
		t.Fatalf("expected Path=%q, got %q", newPath, got.Path)
	}
	if got.Warning == "" {
		t.Fatal("expected migration notice in Warning")
	}
	// 移行通知は旧 / 新両方の path を含む (Req 2.4: 1 行ログで識別可能)
	if !strings.Contains(got.Warning, oldPath) || !strings.Contains(got.Warning, newPath) {
		t.Fatalf("warning should mention both old and new paths: %q", got.Warning)
	}
	// 新 path にファイルが存在する
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("expected new path to exist: %v", err)
	}
	// 旧 path はもう存在しない
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("expected old path to be removed, got err %v", err)
	}
	// データ無改変: rename 前後の sha256 が一致 (Req 2.7)
	afterHash := fileSHA256(t, newPath)
	if beforeHash != afterHash {
		t.Fatalf("file content changed during rename: before=%s after=%s", beforeHash, afterHash)
	}
	// ファイル権限が 0o600 を維持 (Req 8.6 / 観測完了条件)
	info, err := os.Stat(newPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected file permission 0o600, got %#o", perm)
	}
}

// ケース 2: 新単独 (新 akashic.sqlite のみ存在 = 既に移行済み or fresh install 後の再起動)
// → 冪等 no-op (warning なし)
func TestMigrateNoOpWhenNewExists(t *testing.T) {
	newPath, _, _ := migrateTestPaths(t)
	writeFile(t, newPath, []byte("akashic content"))

	got := Migrate(newPath)

	if got.Path != newPath {
		t.Fatalf("expected Path=%q, got %q", newPath, got.Path)
	}
	if got.Warning != "" {
		t.Fatalf("expected no warning when already migrated, got %q", got.Warning)
	}
}

// ケース 3: 両方存在 → 新優先 + 旧保持 (no-op、旧ファイル破壊禁止)
func TestMigrateNoOpWhenBothExist(t *testing.T) {
	newPath, oldPath, _ := migrateTestPaths(t)
	writeFile(t, newPath, []byte("akashic content"))
	writeFile(t, oldPath, []byte("observability content"))

	got := Migrate(newPath)

	if got.Path != newPath {
		t.Fatalf("expected Path=%q, got %q", newPath, got.Path)
	}
	if got.Warning != "" {
		t.Fatalf("expected no warning when both exist, got %q", got.Warning)
	}
	// 旧ファイルは破壊されず保持 (Req 2.5)
	oldData, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatalf("expected old file to be preserved: %v", err)
	}
	if string(oldData) != "observability content" {
		t.Fatalf("old file content was modified: %q", oldData)
	}
}

// ケース 4: WAL 副ファイル群 (-shm / -wal) も追従 rename
func TestMigrateRenamesWALSiblings(t *testing.T) {
	newPath, oldPath, _ := migrateTestPaths(t)
	mainContent := []byte("main")
	walContent := []byte("wal")
	shmContent := []byte("shm")
	writeFile(t, oldPath, mainContent)
	writeFile(t, oldPath+"-wal", walContent)
	writeFile(t, oldPath+"-shm", shmContent)

	got := Migrate(newPath)

	if got.Path != newPath {
		t.Fatalf("expected Path=%q, got %q", newPath, got.Path)
	}
	if got.Warning == "" {
		t.Fatal("expected migration notice")
	}
	// 新名にすべてが存在
	for _, p := range []string{newPath, newPath + "-wal", newPath + "-shm"} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected %s to exist: %v", p, err)
		}
	}
	// 旧名はすべて消えている
	for _, p := range []string{oldPath, oldPath + "-wal", oldPath + "-shm"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be removed, got %v", p, err)
		}
	}
	// 内容も保たれる (バイナリ無改変; Req 2.3, 2.7)
	if data, _ := os.ReadFile(newPath); string(data) != "main" {
		t.Fatalf("main content mismatch: %q", data)
	}
	if data, _ := os.ReadFile(newPath + "-wal"); string(data) != "wal" {
		t.Fatalf("-wal content mismatch: %q", data)
	}
	if data, _ := os.ReadFile(newPath + "-shm"); string(data) != "shm" {
		t.Fatalf("-shm content mismatch: %q", data)
	}
}

// ケース 5: rename 失敗 (parent ディレクトリ 0o500 で書き込み不可)
// → 旧 path 採用 + warning + 起動継続 (Req 2.6)
func TestMigrateMainRenameFailsKeepsOldPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file permission semantics differ on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission checks")
	}

	newPath, oldPath, dir := migrateTestPaths(t)
	writeFile(t, oldPath, []byte("legacy"))

	// parent dir を read+execute のみ (write 拒否) にして rename を失敗させる
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod failed: %v", err)
	}
	// t.TempDir の自動削除より前に書き込み権限を戻す (LIFO)
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o700)
	})

	got := Migrate(newPath)

	if got.Path != oldPath {
		t.Fatalf("expected fallback to old path on failure, got %q", got.Path)
	}
	if got.Warning == "" {
		t.Fatal("expected failure warning")
	}
	if !strings.Contains(got.Warning, "失敗") {
		t.Fatalf("warning should mention failure: %q", got.Warning)
	}
	// 旧ファイルは健在
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("old path should remain after failure: %v", err)
	}
}

// ケース 6: 部分失敗 -shm 成功 → -wal 失敗のロールバック
// → -shm を旧名へ戻し、main rename はスキップして旧 path 採用
func TestMigratePartialFailureRollsBackSHM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file system semantics differ on Windows")
	}

	newPath, oldPath, _ := migrateTestPaths(t)
	writeFile(t, oldPath, []byte("main"))
	writeFile(t, oldPath+"-wal", []byte("wal"))
	writeFile(t, oldPath+"-shm", []byte("shm"))

	// 新 path の -wal 位置に non-empty なディレクトリを置き、
	// file → existing directory rename を必ず失敗させる (POSIX: EISDIR / ENOTEMPTY)
	walTargetAsDir := newPath + "-wal"
	if err := os.Mkdir(walTargetAsDir, 0o700); err != nil {
		t.Fatalf("mkdir blocker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(walTargetAsDir, "blocker"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	got := Migrate(newPath)

	if got.Path != oldPath {
		t.Fatalf("expected fallback to old path, got %q", got.Path)
	}
	if got.Warning == "" {
		t.Fatal("expected failure warning")
	}
	// -shm は旧名にロールバック
	if _, err := os.Stat(oldPath + "-shm"); err != nil {
		t.Fatalf("expected -shm rolled back to old path, got %v", err)
	}
	if _, err := os.Stat(newPath + "-shm"); !os.IsNotExist(err) {
		t.Fatalf("expected -shm not at new path, got %v", err)
	}
	// main rename は実行されていない
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("main file should still be at old path: %v", err)
	}
	// main は新 path にファイルとして移動していない (新 path の -wal は事前作成のディレクトリ)
	if info, err := os.Stat(newPath); err == nil {
		t.Fatalf("main file should not be at new path, got file mode=%v", info.Mode())
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected stat error for new main path: %v", err)
	}
	// -wal は元のまま (rename 失敗 = atomic、source 残存)
	if _, err := os.Stat(oldPath + "-wal"); err != nil {
		t.Fatalf("-wal should remain at old path: %v", err)
	}
}

// 追加テスト: basename に "akashic" を含まない path → 旧名導出スキップで no-op
// (env override で旧名 path を強制した場合の意図された挙動。
//
//	godoc にも限界として明示している)
func TestMigrateNoOpWhenBasenameLacksAkashic(t *testing.T) {
	dir := t.TempDir()
	customPath := filepath.Join(dir, "observability.sqlite")
	// このファイルが存在しても、basename に "akashic" がないため migration は走らない
	writeFile(t, customPath, []byte("legacy"))

	got := Migrate(customPath)

	if got.Path != customPath {
		t.Fatalf("expected Path=%q, got %q", customPath, got.Path)
	}
	if got.Warning != "" {
		t.Fatalf("expected no warning, got %q", got.Warning)
	}
	// ファイルは無改変
	if _, err := os.Stat(customPath); err != nil {
		t.Fatalf("input file should be untouched: %v", err)
	}
}

// 追加テスト: 旧ファイルが存在しない fresh install → no-op
func TestMigrateNoOpWhenNoOldFile(t *testing.T) {
	newPath, _, _ := migrateTestPaths(t)
	// 何も作成しない (fresh install)

	got := Migrate(newPath)

	if got.Path != newPath {
		t.Fatalf("expected Path=%q, got %q", newPath, got.Path)
	}
	if got.Warning != "" {
		t.Fatalf("expected no warning, got %q", got.Warning)
	}
}

// run_artifacts スキーマ非破壊マイグレーション (Req 5.3) — 新規 DB ケース。
// 新規 DB に対して OpenSQLite を実行すると、run_artifacts テーブルと
// idx_run_artifacts_workflow_run_id index が sqlite_master 上に作成されることを確認する。
func TestOpenSQLiteCreatesRunArtifactsSchemaOnFresh(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")

	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite returned error: %v", err)
	}
	defer recorder.Close()

	// run_artifacts テーブルが sqlite_master 上に存在する
	var name string
	err = recorder.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='run_artifacts'`).Scan(&name)
	if err != nil {
		t.Fatalf("run_artifacts テーブルが sqlite_master 上に存在しない: %v", err)
	}

	// idx_run_artifacts_workflow_run_id index が sqlite_master 上に存在する
	err = recorder.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_run_artifacts_workflow_run_id'`).Scan(&name)
	if err != nil {
		t.Fatalf("idx_run_artifacts_workflow_run_id index が sqlite_master 上に存在しない: %v", err)
	}
}

// run_artifacts スキーマ非破壊マイグレーション (Req 5.3) — 既存 DB ケース。
// 旧スキーマ (run_artifacts 無し) と既存 workflow_runs 行を仕込んだ SQLite ファイルに対し
// 再度 OpenSQLite を実行しても、既存行と既存テーブル定義が破壊されず、
// run_artifacts だけが additive に追加されることを確認する。
func TestOpenSQLitePreservesExistingDataAndAddsRunArtifactsAdditively(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")

	// 旧スキーマで DB を作成 (run_artifacts なし、workflow_runs のみ)。
	// modernc.org/sqlite 経由で raw に DDL を流し込み「旧バージョン状態」を再現する。
	legacyDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open legacy DB: %v", err)
	}
	legacyDB.SetMaxOpenConns(1)
	const legacyDDL = `
CREATE TABLE workflow_runs (
  id TEXT PRIMARY KEY,
  workflow_id TEXT NOT NULL,
  workflow_name TEXT NOT NULL,
  cwd TEXT NOT NULL,
  started_at TEXT NOT NULL,
  finished_at TEXT,
  error TEXT
);
`
	if _, err := legacyDB.ExecContext(ctx, legacyDDL); err != nil {
		t.Fatalf("旧スキーマ作成失敗: %v", err)
	}
	if _, err := legacyDB.ExecContext(ctx,
		`INSERT INTO workflow_runs (id, workflow_id, workflow_name, cwd, started_at) VALUES (?, ?, ?, ?, ?)`,
		"legacy-run-1", "wf-1", "Legacy WF", "/repo", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("既存 workflow_runs 行 insert 失敗: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("legacy DB close: %v", err)
	}

	// 新スキーマ込みで再 OpenSQLite。CREATE TABLE IF NOT EXISTS なので
	// 既存テーブル定義・既存行は保持され、run_artifacts のみ additive に追加される想定。
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite (re-open) returned error: %v", err)
	}
	defer recorder.Close()

	// 既存 workflow_runs 行が保持されている
	var workflowID string
	err = recorder.db.QueryRowContext(ctx,
		`SELECT workflow_id FROM workflow_runs WHERE id = ?`, "legacy-run-1").Scan(&workflowID)
	if err != nil {
		t.Fatalf("既存 workflow_runs 行が保持されていない: %v", err)
	}
	if workflowID != "wf-1" {
		t.Fatalf("既存行の workflow_id が破壊された: got %q, want wf-1", workflowID)
	}

	// 既存 workflow_runs テーブル定義 (PK 句) が破壊されていないことの簡易確認
	var sqlText string
	err = recorder.db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='workflow_runs'`).Scan(&sqlText)
	if err != nil {
		t.Fatalf("workflow_runs テーブル定義の取得失敗: %v", err)
	}
	if !strings.Contains(sqlText, "id TEXT PRIMARY KEY") {
		t.Fatalf("workflow_runs PK 定義が破壊された: %q", sqlText)
	}

	// run_artifacts テーブルが additive に追加されている
	var name string
	err = recorder.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='run_artifacts'`).Scan(&name)
	if err != nil {
		t.Fatalf("run_artifacts テーブルが additive に追加されていない: %v", err)
	}

	// idx_run_artifacts_workflow_run_id index も additive に追加されている
	err = recorder.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_run_artifacts_workflow_run_id'`).Scan(&name)
	if err != nil {
		t.Fatalf("idx_run_artifacts_workflow_run_id index が additive に追加されていない: %v", err)
	}
}

// agent_calls.user_corrected 列の冪等追加テスト (memory-distiller-trigger-policy task 3.1)
// 既存 DB (user_corrected 列なし) に対して OpenSQLite を 2 回実行しても
// PRAGMA table_info で user_corrected 列が 1 個だけ存在することを確認する (Req 3.5, 6.2)。
func TestApplyColumnMigrationsAddsUserCorrectedIdempotently(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.sqlite")

	// 旧スキーマ (user_corrected 列なし) で agent_calls を作成する。
	// failure_category / project_id すら無い「v1 以前」相当の状態を再現し、
	// applyColumnMigrations の 1 行追加が他の column migration と独立して冪等に動くことを担保する。
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

	// 1 回目の OpenSQLite: applyColumnMigrations が user_corrected 列を ALTER TABLE で追加する。
	recorder1, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite first call: %v", err)
	}
	if got := countAgentCallsColumns(t, recorder1.db, "user_corrected"); got != 1 {
		t.Fatalf("after first migration: expected user_corrected column count = 1, got %d", got)
	}
	if err := recorder1.Close(); err != nil {
		t.Fatalf("close recorder1: %v", err)
	}

	// 2 回目の OpenSQLite: 既に user_corrected 列が存在するため columnExists で skip され、
	// PRAGMA table_info の column 数は不変 (= 1) であるべき。
	recorder2, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite second call: %v", err)
	}
	defer recorder2.Close()
	if got := countAgentCallsColumns(t, recorder2.db, "user_corrected"); got != 1 {
		t.Fatalf("after second migration (idempotency): expected user_corrected column count = 1, got %d", got)
	}
}

// agent_calls.user_corrected の default 0 が legacy row に対して適用されることを確認する
// (memory-distiller-trigger-policy task 3.1, Req 3.5, 6.3)。
// 旧スキーマで insert された row は user_corrected 列を持たないが、ALTER TABLE で
// `INTEGER NOT NULL DEFAULT 0` を追加すると既存 row には 0 が埋められる。
func TestUserCorrectedColumnDefaultsToZeroForLegacyRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy_with_row.sqlite")

	// 旧スキーマで legacy row を 1 件 insert する。
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
	if _, err := legacy.ExecContext(ctx,
		`INSERT INTO agent_calls (id, workflow_run_id) VALUES (?, ?)`,
		"legacy-call-1", "legacy-run-1"); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	// applyColumnMigrations 経由で user_corrected 列を追加。
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	// legacy row が default 0 で読めることを確認 (Req 6.3 "no correction" 値)。
	var userCorrected int64
	err = recorder.db.QueryRowContext(ctx,
		`SELECT user_corrected FROM agent_calls WHERE id = ?`, "legacy-call-1").Scan(&userCorrected)
	if err != nil {
		t.Fatalf("query user_corrected on legacy row: %v", err)
	}
	if userCorrected != 0 {
		t.Fatalf("legacy row should default to 0 (no correction), got %d", userCorrected)
	}
}

// 新規 DB の schema 定数で agent_calls.user_corrected が CREATE TABLE 時から
// 含まれていることを確認する (memory-distiller-trigger-policy task 3.1)。
// 既存 DB / 新規 DB の両経路で同じ column 形状であることを保証する。
func TestSchemaCreatesUserCorrectedColumnOnFreshDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fresh.sqlite")

	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	// 新規 DB でも user_corrected 列が PRAGMA table_info に 1 行として存在する。
	if got := countAgentCallsColumns(t, recorder.db, "user_corrected"); got != 1 {
		t.Fatalf("fresh DB: expected user_corrected column count = 1, got %d", got)
	}

	// dflt_value が 0 (NOT NULL DEFAULT 0) である。
	rows, err := recorder.db.QueryContext(ctx, `PRAGMA table_info(agent_calls)`)
	if err != nil {
		t.Fatalf("pragma table_info: %v", err)
	}
	defer rows.Close()
	var (
		foundDefault string
		foundNotNull int
		found        bool
	)
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
		if name == "user_corrected" {
			found = true
			foundNotNull = notnull
			if dflt.Valid {
				foundDefault = dflt.String
			}
		}
	}
	if !found {
		t.Fatalf("user_corrected column not found in PRAGMA table_info")
	}
	if foundNotNull != 1 {
		t.Fatalf("user_corrected should be NOT NULL (notnull=1), got %d", foundNotNull)
	}
	if foundDefault != "0" {
		t.Fatalf("user_corrected default should be 0, got %q", foundDefault)
	}
}

// countAgentCallsColumns は PRAGMA table_info(agent_calls) で `column` 名を持つ列の出現回数を返す。
// 冪等性 (= 同名 column が重複追加されていないこと) の検証に用いる。
func countAgentCallsColumns(t *testing.T, db *sql.DB, column string) int {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `PRAGMA table_info(agent_calls)`)
	if err != nil {
		t.Fatalf("pragma table_info: %v", err)
	}
	defer rows.Close()
	count := 0
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
		if name == column {
			count++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	return count
}
