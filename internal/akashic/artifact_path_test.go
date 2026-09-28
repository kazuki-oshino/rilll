// Package akashic の artifact_path_test.go は、artifact_path.go (タスク 2.1) の
// TDD discipline を担保する companion test。
//
// design.md "Failure mode policy" に列挙された 7 ケース ((a) 通常パス、(b) `..` 含み、
// (c) 絶対 symlink で `.rilll` 外、(d) 相対 symlink、(e) ファイル不在、(f) `.rilll` 自体の symlink、
// (g) `.rilll` ルート不在) を、登録経路 (NormalizeArtifactPath) と読み取り経路
// (ValidateAllowlist) の双方で `errors.Is` 判定可能な sentinel まで観測する。
package akashic

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupRillCWD はテスト用の cwd と `.rilll` ルートを生成する。
// 戻り値: cwd (絶対 clean), rillRoot (cwd/.rilll の絶対 clean)
func setupRillCWD(t *testing.T) (string, string) {
	t.Helper()
	cwd := t.TempDir()
	// macOS の TempDir は /var/folders/... のような symlink を含むことがあるため、
	// 比較用に EvalSymlinks 済みの形を使う。
	resolvedCWD, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatalf("EvalSymlinks(cwd): %v", err)
	}
	rillRoot := filepath.Join(resolvedCWD, ".rilll")
	if err := os.MkdirAll(rillRoot, 0o700); err != nil {
		t.Fatalf("MkdirAll(.rilll): %v", err)
	}
	return resolvedCWD, rillRoot
}

// writeArtifactFile は `.rilll/<rel>` 配下にファイルを書き込み、絶対 clean path を返す。
func writeArtifactFile(t *testing.T, rillRoot, rel string, content []byte) string {
	t.Helper()
	abs := filepath.Join(rillRoot, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(abs), err)
	}
	if err := os.WriteFile(abs, content, 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", abs, err)
	}
	return abs
}

// openArtifactDB はテスト用の SQLite DB を開き、recorder.db を返す。
// テスト終了時に Close を登録する。
func openArtifactDB(t *testing.T) *SQLiteRecorder {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "akashic.sqlite")
	rec, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })
	return rec
}

// insertArtifactRow は run_artifacts に直接 INSERT する (タスク 3.1 未完了のため
// SQLiteRecorder.RecordRunArtifact 経由ではなく素の SQL で投入する)。
func insertArtifactRow(t *testing.T, rec *SQLiteRecorder, runID, path, kind string) {
	t.Helper()
	ctx := context.Background()
	_, err := rec.db.ExecContext(ctx,
		`INSERT INTO run_artifacts (workflow_run_id, path, kind, size_bytes, mtime, mime)
VALUES (?, ?, ?, ?, ?, ?)`,
		runID, path, kind, int64(0), "2026-05-02T00:00:00Z", "text/plain")
	if err != nil {
		t.Fatalf("INSERT run_artifacts: %v", err)
	}
}

// --- (a) 通常パス -------------------------------------------------------------

// TestNormalizeArtifactPath_NormalPath は `.rilll` 配下の実在ファイルを登録経路で
// 正規化すると、絶対 clean path がそのまま返ることを確認する (Req 1.2)。
func TestNormalizeArtifactPath_NormalPath(t *testing.T) {
	t.Parallel()
	cwd, rillRoot := setupRillCWD(t)
	abs := writeArtifactFile(t, rillRoot, "wf/run/result.json", []byte("{}"))

	got, err := NormalizeArtifactPath(cwd, abs)
	if err != nil {
		t.Fatalf("NormalizeArtifactPath: %v", err)
	}
	if got != abs {
		t.Fatalf("got %q, want %q", got, abs)
	}
}

// TestValidateAllowlist_NormalPath_Registered は登録済みパスの ValidateAllowlist が
// 正規化済み絶対 clean path を nil error で返すことを確認する (Req 4.1, 4.2)。
func TestValidateAllowlist_NormalPath_Registered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cwd, rillRoot := setupRillCWD(t)
	abs := writeArtifactFile(t, rillRoot, "wf/run/result.json", []byte("{}"))

	rec := openArtifactDB(t)
	insertArtifactRow(t, rec, "run-001", abs, "result")

	got, err := ValidateAllowlist(ctx, rec.db, cwd, abs)
	if err != nil {
		t.Fatalf("ValidateAllowlist: %v", err)
	}
	if got != abs {
		t.Fatalf("got %q, want %q", got, abs)
	}
}

// TestValidateAllowlist_NormalPath_NotRegistered は `.rilll` 配下にあるが
// run_artifacts に未登録のパスが ErrArtifactNotAllowed で拒否されることを確認する
// (Req 4.1, 4.2)。
func TestValidateAllowlist_NormalPath_NotRegistered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cwd, rillRoot := setupRillCWD(t)
	abs := writeArtifactFile(t, rillRoot, "wf/run/result.json", []byte("{}"))

	rec := openArtifactDB(t)
	// 故意に INSERT しない

	_, err := ValidateAllowlist(ctx, rec.db, cwd, abs)
	if !errors.Is(err, ErrArtifactNotAllowed) {
		t.Fatalf("err = %v, want ErrArtifactNotAllowed", err)
	}
}

// --- (b) `..` 含み ------------------------------------------------------------

// TestNormalizeArtifactPath_DotDotInsideRill は `..` を含むが解消後 `.rilll` 配下に
// 留まるパスが正常に正規化されることを確認する。
func TestNormalizeArtifactPath_DotDotInsideRill(t *testing.T) {
	t.Parallel()
	cwd, rillRoot := setupRillCWD(t)
	abs := writeArtifactFile(t, rillRoot, "wf/run/result.json", []byte("{}"))

	// `.rilll/wf/run/../run/result.json` のように `..` を含む同等パスを構築
	withDotDot := filepath.Join(rillRoot, "wf", "run", "..", "run", "result.json")
	got, err := NormalizeArtifactPath(cwd, withDotDot)
	if err != nil {
		t.Fatalf("NormalizeArtifactPath: %v", err)
	}
	if got != abs {
		t.Fatalf("got %q, want %q (canonical form)", got, abs)
	}
}

// TestNormalizeArtifactPath_DotDotEscapesRill は `..` 解消後に `.rilll` 外を指す
// パスが ErrArtifactOutsideRillRoot で拒否されることを確認する (Req 1.5, 4.5)。
func TestNormalizeArtifactPath_DotDotEscapesRill(t *testing.T) {
	t.Parallel()
	cwd, rillRoot := setupRillCWD(t)
	// cwd 直下に外部ファイルを置く (`.rilll` の外)
	outside := filepath.Join(cwd, "outside.txt")
	if err := os.WriteFile(outside, []byte("nope"), 0o600); err != nil {
		t.Fatalf("WriteFile outside: %v", err)
	}

	// `.rilll/../outside.txt` で .rilll 外へ脱出を試みる
	escape := filepath.Join(rillRoot, "..", "outside.txt")
	_, err := NormalizeArtifactPath(cwd, escape)
	if !errors.Is(err, ErrArtifactOutsideRillRoot) {
		t.Fatalf("err = %v, want ErrArtifactOutsideRillRoot", err)
	}
}

// TestValidateAllowlist_DotDotEscapesRill は読み取り経路で `..` 脱出が
// ErrArtifactNotAllowed (allowlist 拒否優先) になることを確認する (Req 4.3, 4.5)。
func TestValidateAllowlist_DotDotEscapesRill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cwd, rillRoot := setupRillCWD(t)
	outside := filepath.Join(cwd, "outside.txt")
	if err := os.WriteFile(outside, []byte("nope"), 0o600); err != nil {
		t.Fatalf("WriteFile outside: %v", err)
	}

	rec := openArtifactDB(t)
	escape := filepath.Join(rillRoot, "..", "outside.txt")
	_, err := ValidateAllowlist(ctx, rec.db, cwd, escape)
	if !errors.Is(err, ErrArtifactNotAllowed) {
		t.Fatalf("err = %v, want ErrArtifactNotAllowed", err)
	}
}

// --- (c) 絶対 symlink で `.rilll` 外 -------------------------------------------

// TestNormalizeArtifactPath_AbsoluteSymlinkOutsideRill は絶対 symlink で `.rilll` 外を
// 指す経路が ErrArtifactOutsideRillRoot で拒否されることを確認する (Req 4.5)。
func TestNormalizeArtifactPath_AbsoluteSymlinkOutsideRill(t *testing.T) {
	t.Parallel()
	cwd, rillRoot := setupRillCWD(t)
	// .rilll の外にターゲットファイルを実在させる
	externalDir := t.TempDir()
	externalFile := filepath.Join(externalDir, "secret.txt")
	if err := os.WriteFile(externalFile, []byte("secret"), 0o600); err != nil {
		t.Fatalf("WriteFile external: %v", err)
	}
	// `.rilll/wf/run/link.txt` → 絶対 symlink → externalFile
	linkDir := filepath.Join(rillRoot, "wf", "run")
	if err := os.MkdirAll(linkDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	link := filepath.Join(linkDir, "link.txt")
	if err := os.Symlink(externalFile, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	_, err := NormalizeArtifactPath(cwd, link)
	if !errors.Is(err, ErrArtifactOutsideRillRoot) {
		t.Fatalf("err = %v, want ErrArtifactOutsideRillRoot", err)
	}
}

// TestValidateAllowlist_AbsoluteSymlinkOutsideRill は絶対 symlink 脱出を
// 読み取り経路で ErrArtifactNotAllowed として拒否することを確認する (Req 4.5)。
func TestValidateAllowlist_AbsoluteSymlinkOutsideRill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cwd, rillRoot := setupRillCWD(t)
	externalDir := t.TempDir()
	externalFile := filepath.Join(externalDir, "secret.txt")
	if err := os.WriteFile(externalFile, []byte("secret"), 0o600); err != nil {
		t.Fatalf("WriteFile external: %v", err)
	}
	linkDir := filepath.Join(rillRoot, "wf", "run")
	if err := os.MkdirAll(linkDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	link := filepath.Join(linkDir, "link.txt")
	if err := os.Symlink(externalFile, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	rec := openArtifactDB(t)
	// 仮に攻撃者が link path を DB に登録できた場合でも (テスト都合で直接 INSERT)、
	// `.rilll` ルート再確認で拒否されることを観測する。
	insertArtifactRow(t, rec, "run-001", link, "result")

	_, err := ValidateAllowlist(ctx, rec.db, cwd, link)
	if !errors.Is(err, ErrArtifactNotAllowed) {
		t.Fatalf("err = %v, want ErrArtifactNotAllowed", err)
	}
}

// --- (d) 相対 symlink ---------------------------------------------------------

// TestNormalizeArtifactPath_RelativeSymlinkInsideRill は相対 symlink でも `.rilll` 内に
// 留まる場合は正常に正規化されることを確認する。
func TestNormalizeArtifactPath_RelativeSymlinkInsideRill(t *testing.T) {
	t.Parallel()
	cwd, rillRoot := setupRillCWD(t)
	// 実体: .rilll/wf/actual.txt
	actual := writeArtifactFile(t, rillRoot, "wf/actual.txt", []byte("ok"))
	// link: .rilll/wf/run/link.txt → ../actual.txt (相対)
	linkDir := filepath.Join(rillRoot, "wf", "run")
	if err := os.MkdirAll(linkDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	link := filepath.Join(linkDir, "link.txt")
	if err := os.Symlink("../actual.txt", link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	got, err := NormalizeArtifactPath(cwd, link)
	if err != nil {
		t.Fatalf("NormalizeArtifactPath: %v", err)
	}
	if got != actual {
		t.Fatalf("got %q, want %q (canonical actual)", got, actual)
	}
}

// --- (e) ファイル不在 --------------------------------------------------------

// TestNormalizeArtifactPath_TargetMissing は実在しないパスが
// ErrArtifactFileMissing (fs.ErrNotExist で wrap 済) で返ることを確認する。
func TestNormalizeArtifactPath_TargetMissing(t *testing.T) {
	t.Parallel()
	cwd, rillRoot := setupRillCWD(t)
	missing := filepath.Join(rillRoot, "wf", "run", "missing.txt")

	_, err := NormalizeArtifactPath(cwd, missing)
	if !errors.Is(err, ErrArtifactFileMissing) {
		t.Fatalf("err = %v, want ErrArtifactFileMissing", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("errors.Is(err, fs.ErrNotExist) = false; want true")
	}
}

// TestValidateAllowlist_TargetMissingButRegistered は登録済みパスのファイルが
// 後から削除された場合に ErrArtifactFileMissing が返ることを確認する (Req 3.7)。
// run_artifacts レコード自体は変更されない。
func TestValidateAllowlist_TargetMissingButRegistered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cwd, rillRoot := setupRillCWD(t)
	abs := writeArtifactFile(t, rillRoot, "wf/run/result.json", []byte("{}"))

	rec := openArtifactDB(t)
	insertArtifactRow(t, rec, "run-001", abs, "result")

	// 登録後にファイルを消す
	if err := os.Remove(abs); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	_, err := ValidateAllowlist(ctx, rec.db, cwd, abs)
	if !errors.Is(err, ErrArtifactFileMissing) {
		t.Fatalf("err = %v, want ErrArtifactFileMissing", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("errors.Is(err, fs.ErrNotExist) = false; want true")
	}

	// run_artifacts のレコードは保持されている (Req 3.7)
	var count int
	if err := rec.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run_artifacts WHERE workflow_run_id = ? AND path = ?`,
		"run-001", abs).Scan(&count); err != nil {
		t.Fatalf("SELECT COUNT: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1 (DB 行は不変であるべき)", count)
	}
}

// --- (f) `.rilll` 自体の symlink ----------------------------------------------

// TestNormalizeArtifactPath_RillRootIsSymlink は `.rilll` 自体が symlink であっても
// canonical 形で `.rilll` 配下と判定でき、絶対 clean path が返ることを確認する。
func TestNormalizeArtifactPath_RillRootIsSymlink(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	resolvedCWD, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatalf("EvalSymlinks(cwd): %v", err)
	}
	// 実体ディレクトリ: cwd/actual_rill
	actualRillDir := filepath.Join(resolvedCWD, "actual_rill")
	if err := os.MkdirAll(actualRillDir, 0o700); err != nil {
		t.Fatalf("MkdirAll actual_rill: %v", err)
	}
	// .rilll → actual_rill の symlink
	rillSymlink := filepath.Join(resolvedCWD, ".rilll")
	if err := os.Symlink(actualRillDir, rillSymlink); err != nil {
		t.Fatalf("Symlink .rilll: %v", err)
	}
	// ファイル: actual_rill/wf/run/result.json
	abs := filepath.Join(actualRillDir, "wf", "run", "result.json")
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(abs, []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// 入力は .rilll 経由のパス、結果は canonical (actual_rill 経由) で返るべき
	input := filepath.Join(rillSymlink, "wf", "run", "result.json")
	got, err := NormalizeArtifactPath(resolvedCWD, input)
	if err != nil {
		t.Fatalf("NormalizeArtifactPath: %v", err)
	}
	if got != abs {
		t.Fatalf("got %q, want %q (canonical via actual_rill)", got, abs)
	}
}

// --- (g) `.rilll` ルート不在 --------------------------------------------------

// TestRillRootFromCWD_Missing は `.rilll` 不在時に ErrArtifactFileMissing が
// 返り、fs.ErrNotExist でも errors.Is が真になることを確認する。
func TestRillRootFromCWD_Missing(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	// `.rilll` は作らない

	_, err := RillRootFromCWD(cwd)
	if !errors.Is(err, ErrArtifactFileMissing) {
		t.Fatalf("err = %v, want ErrArtifactFileMissing", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("errors.Is(err, fs.ErrNotExist) = false; want true")
	}
}

// TestNormalizeArtifactPath_RillRootMissing は `.rilll` ルート不在時に登録経路で
// ErrArtifactFileMissing が返ることを確認する。
func TestNormalizeArtifactPath_RillRootMissing(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	// `.rilll` を作らないまま、適当な path を渡す
	path := filepath.Join(cwd, ".rilll", "wf", "run", "result.json")

	_, err := NormalizeArtifactPath(cwd, path)
	if !errors.Is(err, ErrArtifactFileMissing) {
		t.Fatalf("err = %v, want ErrArtifactFileMissing", err)
	}
}

// TestValidateAllowlist_RillRootMissing は `.rilll` ルート不在時に読み取り経路で
// ErrArtifactNotAllowed (拒否優先) が返ることを確認する (design.md Failure mode policy)。
func TestValidateAllowlist_RillRootMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cwd := t.TempDir()
	path := filepath.Join(cwd, ".rilll", "wf", "run", "result.json")

	rec := openArtifactDB(t)
	_, err := ValidateAllowlist(ctx, rec.db, cwd, path)
	if !errors.Is(err, ErrArtifactNotAllowed) {
		t.Fatalf("err = %v, want ErrArtifactNotAllowed", err)
	}
}

// --- 補助テスト: RillRootFromCWD 正常系 ---------------------------------------

// TestRillRootFromCWD_Normal は `.rilll` 実在時に絶対 clean path が返ることを確認する。
func TestRillRootFromCWD_Normal(t *testing.T) {
	t.Parallel()
	cwd, rillRoot := setupRillCWD(t)

	got, err := RillRootFromCWD(cwd)
	if err != nil {
		t.Fatalf("RillRootFromCWD: %v", err)
	}
	if got != rillRoot {
		t.Fatalf("got %q, want %q", got, rillRoot)
	}
}

// TestNormalizeArtifactPath_RelativeCWD は cwd 引数が相対パスでも内部で絶対化されて
// 動作することを確認する (Req 1.2)。
func TestNormalizeArtifactPath_RelativeCWD(t *testing.T) {
	t.Parallel()
	cwd, rillRoot := setupRillCWD(t)
	abs := writeArtifactFile(t, rillRoot, "wf/run/result.json", []byte("{}"))

	// cwd を相対形に変換 (`os.Getwd()` 起点で算出)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	rel, err := filepath.Rel(wd, cwd)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	// `..` で始まる極端な相対パスでも filepath.Abs で吸収できる
	if !strings.Contains(rel, "..") && !filepath.IsAbs(rel) {
		// 相対パス変換が想定通り
	}

	got, err := NormalizeArtifactPath(rel, abs)
	if err != nil {
		t.Fatalf("NormalizeArtifactPath(relative cwd): %v", err)
	}
	if got != abs {
		t.Fatalf("got %q, want %q", got, abs)
	}
}
