// Package akashic の artifact_reader_sqlite_test.go は、artifact_reader_sqlite.go
// (タスク 3.2) の TDD discipline を担保する companion test。
//
// design.md "ArtifactReader" / requirements.md Req 3.x / 4.x / 5.4 を踏まえ、
// 以下の観測可能な完了条件を網羅する:
//
//	(a) GetArtifacts 0 件 → 空 slice + nil error (Req 3.2 / 5.4)
//	(b) GetArtifacts 1+ 件 → ArtifactRow 列挙でメタ一致 (Req 3.1)
//	(c) ReadArtifact 上限内 → 期待バイト列 (Req 3.3)
//	(d) ReadArtifact 上限超過 → ErrArtifactTooLarge 真 + ファイル本体未読 (Req 3.4)
//	(e) ReadArtifact target 不在 → ErrArtifactFileMissing + run_artifacts 行不変 (Req 3.7)
//	(f) ReadArtifactUnlimited 上限超過でも全量読出 (Req 3.5)
//	(g) run_artifacts 未登録パス → ErrArtifactNotAllowed (Req 4.1, 4.2)
package akashic

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// recordArtifactViaRecorder は SQLiteRecorder.RecordRunArtifact 経由で 1 件登録する。
// タスク 3.1 で実装済みの正規経路を再利用し、テストが本番経路と同じ書き込み契約を使う
// ことを保証する。Path は呼び出し側で正規化済みの絶対 clean path であることを前提とする。
func recordArtifactViaRecorder(t *testing.T, rec *SQLiteRecorder, runID, kind, path string, mtime time.Time, mime string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}
	if mtime.IsZero() {
		mtime = info.ModTime().UTC()
	}
	if err := rec.RecordRunArtifact(context.Background(), RunArtifact{
		WorkflowRunID: runID,
		Kind:          kind,
		Path:          path,
		SizeBytes:     info.Size(),
		Mtime:         mtime,
		MIME:          mime,
	}); err != nil {
		t.Fatalf("RecordRunArtifact(%s): %v", path, err)
	}
}

// countArtifactRowsForRun は当該 workflow_run_id の run_artifacts 行数を返す。
// Req 3.7 の「DB レコードは変更しない」検証に使う。
func countArtifactRowsForRun(t *testing.T, rec *SQLiteRecorder, runID string) int {
	t.Helper()
	var count int
	err := rec.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM run_artifacts WHERE workflow_run_id = ?`, runID).Scan(&count)
	if err != nil {
		t.Fatalf("count run_artifacts: %v", err)
	}
	return count
}

// withCWD は ReadArtifact / ReadArtifactUnlimited 用の ctx を組み立てる。
// 循環依存 (workflow → akashic → workflow) を避けるため、akashic 側で独立した
// ContextWithRunCWD 経由で cwd を注入する。
func withCWD(cwd string) context.Context {
	return ContextWithRunCWD(context.Background(), cwd)
}

// --- (a) GetArtifacts 0 件 ----------------------------------------------------

// TestSQLiteRecorder_GetArtifacts_Empty は run_artifacts に 1 件もないとき
// 空 slice + nil error を返すことを検証する (Req 3.2 / 5.4)。
func TestSQLiteRecorder_GetArtifacts_Empty(t *testing.T) {
	t.Parallel()

	rec := openArtifactDB(t)

	got, err := rec.GetArtifacts(context.Background(), "missing-run-id")
	if err != nil {
		t.Fatalf("GetArtifacts: unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("GetArtifacts(missing) len = %d, want 0", len(got))
	}
}

// TestSQLiteRecorder_GetArtifacts_EmptyWorkflowRunID は workflowRunID が空文字でも
// SQL レベルで CHECK 制約があるため登録不可、検索結果も 0 件であることを検証する。
// (Req 3.2 の解釈: 空文字 workflowRunID も 0 件で返しエラーにしない)
func TestSQLiteRecorder_GetArtifacts_EmptyWorkflowRunID(t *testing.T) {
	t.Parallel()

	rec := openArtifactDB(t)

	got, err := rec.GetArtifacts(context.Background(), "")
	if err != nil {
		t.Fatalf("GetArtifacts(\"\"): unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("GetArtifacts(\"\") len = %d, want 0", len(got))
	}
}

// --- (b) GetArtifacts 1+ 件 メタ一致 ------------------------------------------

// TestSQLiteRecorder_GetArtifacts_MultipleArtifacts は同一 run に複数 kind を登録し、
// path 昇順で ArtifactRow が返り、メタ (Kind / Path / SizeBytes / Mtime UTC / MIME) が
// 登録値と一致することを検証する (Req 3.1)。
func TestSQLiteRecorder_GetArtifacts_MultipleArtifacts(t *testing.T) {
	t.Parallel()

	cwd, rillRoot := setupRillCWD(t)
	_ = cwd // 本テストは登録 path のみ使用するため cwd は別経路で必要なし
	rec := openArtifactDB(t)

	// 3 ファイルを `.rilll/` 配下に作成 (path 昇順は a < b < c)。
	pathA := writeArtifactFile(t, rillRoot, "a-proposal.md", []byte("proposal body"))
	pathB := writeArtifactFile(t, rillRoot, "b-review.md", []byte("review body"))
	pathC := writeArtifactFile(t, rillRoot, "c-result.json", []byte(`{"ok":true}`))

	const runID = "run-2026-05-02"
	mtime := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	recordArtifactViaRecorder(t, rec, runID, ArtifactKindProposal, pathA, mtime, "text/markdown")
	recordArtifactViaRecorder(t, rec, runID, ArtifactKindReview, pathB, mtime, "text/markdown")
	recordArtifactViaRecorder(t, rec, runID, ArtifactKindResult, pathC, mtime, "application/json")

	got, err := rec.GetArtifacts(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetArtifacts: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("GetArtifacts len = %d, want 3", len(got))
	}

	type expectation struct {
		kind, path, mime string
		size             int64
	}
	expectations := []expectation{
		{kind: ArtifactKindProposal, path: pathA, mime: "text/markdown", size: int64(len("proposal body"))},
		{kind: ArtifactKindReview, path: pathB, mime: "text/markdown", size: int64(len("review body"))},
		{kind: ArtifactKindResult, path: pathC, mime: "application/json", size: int64(len(`{"ok":true}`))},
	}

	for i, exp := range expectations {
		row := got[i]
		if row.WorkflowRunID != runID {
			t.Errorf("row[%d].WorkflowRunID = %q, want %q", i, row.WorkflowRunID, runID)
		}
		if row.Kind != exp.kind {
			t.Errorf("row[%d].Kind = %q, want %q", i, row.Kind, exp.kind)
		}
		if row.Path != exp.path {
			t.Errorf("row[%d].Path = %q, want %q", i, row.Path, exp.path)
		}
		if row.SizeBytes != exp.size {
			t.Errorf("row[%d].SizeBytes = %d, want %d", i, row.SizeBytes, exp.size)
		}
		if row.MIME != exp.mime {
			t.Errorf("row[%d].MIME = %q, want %q", i, row.MIME, exp.mime)
		}
		// Mtime は UTC + RFC3339Nano 経由でラウンドトリップするため秒精度は不変。
		if !row.Mtime.Equal(mtime) {
			t.Errorf("row[%d].Mtime = %v, want %v", i, row.Mtime, mtime)
		}
		if row.Mtime.Location() != time.UTC {
			t.Errorf("row[%d].Mtime.Location = %v, want UTC", i, row.Mtime.Location())
		}
	}
}

// TestSQLiteRecorder_GetArtifacts_NullMIME は MIME 空文字 (DB NULL) で登録した行が
// 取得時にも空文字で返ることを検証する。
func TestSQLiteRecorder_GetArtifacts_NullMIME(t *testing.T) {
	t.Parallel()

	_, rillRoot := setupRillCWD(t)
	rec := openArtifactDB(t)

	path := writeArtifactFile(t, rillRoot, "blob.bin", []byte{0x00, 0x01, 0x02})
	const runID = "run-null-mime"
	recordArtifactViaRecorder(t, rec, runID, ArtifactKindContext, path, time.Time{}, "")

	got, err := rec.GetArtifacts(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetArtifacts: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("GetArtifacts len = %d, want 1", len(got))
	}
	if got[0].MIME != "" {
		t.Errorf("got[0].MIME = %q, want \"\"", got[0].MIME)
	}
}

// --- (c) ReadArtifact 上限内 ---------------------------------------------------

// TestSQLiteRecorder_ReadArtifact_WithinLimit は登録済み + `.rilll/` 配下 +
// サイズ上限内のファイルを ReadArtifact で読み出せることを検証する (Req 3.3)。
func TestSQLiteRecorder_ReadArtifact_WithinLimit(t *testing.T) {
	t.Parallel()

	cwd, rillRoot := setupRillCWD(t)
	rec := openArtifactDB(t)

	want := []byte("hello, akashic-artifacts!")
	path := writeArtifactFile(t, rillRoot, "ok.md", want)
	const runID = "run-within"
	recordArtifactViaRecorder(t, rec, runID, ArtifactKindResult, path, time.Time{}, "text/markdown")

	got, err := rec.ReadArtifact(withCWD(cwd), path)
	if err != nil {
		t.Fatalf("ReadArtifact: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ReadArtifact bytes mismatch:\n got = %q\nwant = %q", got, want)
	}
}

// --- (d) ReadArtifact 上限超過 → ErrArtifactTooLarge + 本体未読 ----------------

// TestSQLiteRecorder_ReadArtifact_ExceedsLimit は DefaultArtifactReadMaxBytes を
// 一時的に小さく差し替えたうえで、上限を超えるファイルに対する ReadArtifact が
// (a) ErrArtifactTooLarge を返す、(b) ファイル本体読み込み (osReadFile) を発行しない、
// の両方を検証する (Req 3.4)。
func TestSQLiteRecorder_ReadArtifact_ExceedsLimit(t *testing.T) {
	cwd, rillRoot := setupRillCWD(t)
	rec := openArtifactDB(t)

	// テスト中だけ上限を 8 バイトに差し替える。t.Parallel() を使わないのは
	// DefaultArtifactReadMaxBytes が package-global で他テストと衝突するため。
	originalLimit := DefaultArtifactReadMaxBytes
	DefaultArtifactReadMaxBytes = 8
	t.Cleanup(func() { DefaultArtifactReadMaxBytes = originalLimit })

	// 9 バイトのファイル (8 バイト上限超過)。
	body := []byte("123456789")
	path := writeArtifactFile(t, rillRoot, "too-large.bin", body)
	const runID = "run-too-large"
	recordArtifactViaRecorder(t, rec, runID, ArtifactKindContext, path, time.Time{}, "")

	// osReadFile を観測用に差し替え、本体読み込みが発生していないことを確認する。
	originalReadFile := osReadFile
	readCalled := false
	osReadFile = func(name string) ([]byte, error) {
		readCalled = true
		return originalReadFile(name)
	}
	t.Cleanup(func() { osReadFile = originalReadFile })

	_, err := rec.ReadArtifact(withCWD(cwd), path)
	if err == nil {
		t.Fatalf("ReadArtifact: expected error, got nil")
	}
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("errors.Is(err, ErrArtifactTooLarge) = false; err=%v", err)
	}
	if readCalled {
		t.Fatalf("osReadFile was called; expected size-only check (Req 3.4)")
	}
}

// --- (e) ReadArtifact target 不在 → ErrArtifactFileMissing + run_artifacts 行不変 -

// TestSQLiteRecorder_ReadArtifact_FileMissing は登録後にファイル本体を削除した
// シナリオで ReadArtifact が ErrArtifactFileMissing を返し、かつ run_artifacts の
// 行数が変化しないことを検証する (Req 3.7)。
func TestSQLiteRecorder_ReadArtifact_FileMissing(t *testing.T) {
	t.Parallel()

	cwd, rillRoot := setupRillCWD(t)
	rec := openArtifactDB(t)

	path := writeArtifactFile(t, rillRoot, "missing-target.md", []byte("content"))
	const runID = "run-missing-target"
	recordArtifactViaRecorder(t, rec, runID, ArtifactKindResult, path, time.Time{}, "text/markdown")

	beforeCount := countArtifactRowsForRun(t, rec, runID)
	if beforeCount != 1 {
		t.Fatalf("precondition: count = %d, want 1", beforeCount)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("os.Remove(%s): %v", path, err)
	}

	_, err := rec.ReadArtifact(withCWD(cwd), path)
	if err == nil {
		t.Fatalf("ReadArtifact: expected error, got nil")
	}
	if !errors.Is(err, ErrArtifactFileMissing) {
		t.Fatalf("errors.Is(err, ErrArtifactFileMissing) = false; err=%v", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("errors.Is(err, fs.ErrNotExist) = false; err=%v", err)
	}

	afterCount := countArtifactRowsForRun(t, rec, runID)
	if afterCount != beforeCount {
		t.Fatalf("run_artifacts count changed: before=%d, after=%d", beforeCount, afterCount)
	}
}

// --- (f) ReadArtifactUnlimited 上限超過でも全量読出 ---------------------------

// TestSQLiteRecorder_ReadArtifactUnlimited_OverLimit は DefaultArtifactReadMaxBytes
// を一時的に小さくしたまま、上限を超えるファイルでも全量バイト列が返ることを検証する
// (Req 3.5)。
func TestSQLiteRecorder_ReadArtifactUnlimited_OverLimit(t *testing.T) {
	cwd, rillRoot := setupRillCWD(t)
	rec := openArtifactDB(t)

	// 上限を 4 バイトに差し替え、9 バイトのファイルを用意する。
	originalLimit := DefaultArtifactReadMaxBytes
	DefaultArtifactReadMaxBytes = 4
	t.Cleanup(func() { DefaultArtifactReadMaxBytes = originalLimit })

	want := []byte("0123456789ABCDEF")
	path := writeArtifactFile(t, rillRoot, "unlimited.bin", want)
	const runID = "run-unlimited"
	recordArtifactViaRecorder(t, rec, runID, ArtifactKindContext, path, time.Time{}, "")

	got, err := rec.ReadArtifactUnlimited(withCWD(cwd), path)
	if err != nil {
		t.Fatalf("ReadArtifactUnlimited: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ReadArtifactUnlimited bytes mismatch:\n got = %q\nwant = %q", got, want)
	}
}

// --- (g) 未登録パス → ErrArtifactNotAllowed ----------------------------------

// TestSQLiteRecorder_ReadArtifact_Unregistered は `.rilll/` 配下に実在するが
// run_artifacts に未登録のパスが ErrArtifactNotAllowed を返すことを検証する
// (Req 4.1, 4.2)。
func TestSQLiteRecorder_ReadArtifact_Unregistered(t *testing.T) {
	t.Parallel()

	cwd, rillRoot := setupRillCWD(t)
	rec := openArtifactDB(t)

	// ファイルは作成するが run_artifacts には登録しない。
	path := writeArtifactFile(t, rillRoot, "unregistered.md", []byte("body"))

	_, err := rec.ReadArtifact(withCWD(cwd), path)
	if err == nil {
		t.Fatalf("ReadArtifact: expected error, got nil")
	}
	if !errors.Is(err, ErrArtifactNotAllowed) {
		t.Fatalf("errors.Is(err, ErrArtifactNotAllowed) = false; err=%v", err)
	}

	// ReadArtifactUnlimited も同様 (Req 4.4 の対称性)。
	_, err = rec.ReadArtifactUnlimited(withCWD(cwd), path)
	if err == nil {
		t.Fatalf("ReadArtifactUnlimited: expected error, got nil")
	}
	if !errors.Is(err, ErrArtifactNotAllowed) {
		t.Fatalf("errors.Is(err, ErrArtifactNotAllowed) = false; err=%v", err)
	}
}

// TestSQLiteRecorder_ReadArtifact_OutsideRill は `.rilll/` 外の絶対パスが
// ErrArtifactNotAllowed を返すことを検証する (Req 4.3 の `.rilll/` 配下のみ許可)。
func TestSQLiteRecorder_ReadArtifact_OutsideRill(t *testing.T) {
	t.Parallel()

	cwd, _ := setupRillCWD(t)
	rec := openArtifactDB(t)

	// cwd 外 (= .rilll 外) に普通のファイルを作る。
	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "evil.txt")
	if err := os.WriteFile(outsidePath, []byte("nope"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := rec.ReadArtifact(withCWD(cwd), outsidePath)
	if err == nil {
		t.Fatalf("ReadArtifact: expected error, got nil")
	}
	if !errors.Is(err, ErrArtifactNotAllowed) {
		t.Fatalf("errors.Is(err, ErrArtifactNotAllowed) = false; err=%v", err)
	}
}

// TestSQLiteRecorder_ReadArtifact_MissingCWD は ctx に cwd が無い場合に
// ErrArtifactNotAllowed を返し、ファイルへ到達しないことを検証する (防御的)。
func TestSQLiteRecorder_ReadArtifact_MissingCWD(t *testing.T) {
	t.Parallel()

	rec := openArtifactDB(t)
	// withCWD を経由しない素の ctx を渡す。
	_, err := rec.ReadArtifact(context.Background(), "/tmp/anything")
	if err == nil {
		t.Fatalf("ReadArtifact: expected error, got nil")
	}
	if !errors.Is(err, ErrArtifactNotAllowed) {
		t.Fatalf("errors.Is(err, ErrArtifactNotAllowed) = false; err=%v", err)
	}
}
