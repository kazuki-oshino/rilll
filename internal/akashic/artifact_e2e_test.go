// Package akashic の artifact_e2e_test.go は、akashic-artifacts spec Task 6.1 が
// 求める "実 SQLite + 実ファイル" の end-to-end 検証を担う。
//
// 検証目的:
//   - 4 kind (proposal / review / result / context) を SQLiteRecorder.RecordRunArtifact
//     で登録し、ArtifactReader.GetArtifacts で取得したメタが登録時と完全一致することを
//     確認する (Req 3.1)
//   - ファイル本体は `.rilll/` 配下に残ったままであることを確認する (Req 5.1)
//   - SQLite の run_artifacts テーブルに本体複製用の列が存在せず、path + メタのみで
//     索引していることを PRAGMA で確認する (Req 5.2)
//   - workflow_runs に行はあるが run_artifacts に未登録の旧 run シナリオで
//     GetArtifacts が空 slice + nil error を返すことを確認する (Req 5.4)
//   - ReadArtifact / ReadArtifactUnlimited で正常な読み出しと、未登録パスに対する
//     ErrArtifactNotAllowed を E2E でまとめて観測する (Req 4.1, 4.2)
//
// 設計メモ:
//   - 既存の test fixture (setupRillCWD / writeArtifactFile / openArtifactDB) を再利用し、
//     artifact_path_test.go / artifact_reader_sqlite_test.go と同じ "実 I/O ベースの
//     `.rilll/` レイアウト" を共有する
//   - MIME は本ファイル内で `mime.TypeByExtension` を呼んで自前検出し、登録値と取得値の
//     ラウンドトリップを観測する。`workflow.RecordRunArtifact` には依存しない
//     (本 E2E は akashic パッケージ内の契約閉包を確認することが目的)
package akashic

import (
	"bytes"
	"context"
	"errors"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// e2eArtifactSpec は登録対象 1 件分の入力をまとめた小ヘルパ型。
// Path は writeArtifactFile が返す絶対 clean path、ExpectedMIME は
// mime.TypeByExtension の戻り値で「拡張子から検出された値」(Task 6.1 の観測条件)。
type e2eArtifactSpec struct {
	Kind         string
	RelPath      string // `.rilll/<rel>` の rel 部分
	Body         []byte
	AbsPath      string // writeArtifactFile が返す絶対 clean path
	ExpectedMIME string // mime.TypeByExtension 結果 (空文字列なら DB は NULL)
}

// computeExpectedMIME は path の拡張子から MIME を返す。
// `mime.TypeByExtension` は環境差で値が変わりうるため、登録時と取得時の比較を
// 同じ関数経由で揃え、テストを環境非依存に保つ。
func computeExpectedMIME(path string) string {
	if ext := filepath.Ext(path); ext != "" {
		return mime.TypeByExtension(ext)
	}
	return ""
}

// runArtifactsTableColumns は PRAGMA table_info(run_artifacts) の結果から
// 列名のみを昇順で返す (Req 5.2: 本体複製列が無いことの観察)。
func runArtifactsTableColumns(t *testing.T, rec *SQLiteRecorder) []string {
	t.Helper()
	rows, err := rec.db.QueryContext(context.Background(), `PRAGMA table_info(run_artifacts)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(run_artifacts): %v", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    *string
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("Scan PRAGMA row: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("PRAGMA iter: %v", err)
	}
	sort.Strings(names)
	return names
}

// TestArtifactE2E_RegisterRetrieveRead は Task 6.1 の本体ケース。
// 4 kind を 1 件ずつ登録し、メタの round-trip と ReadArtifact / ReadArtifactUnlimited
// の正常系・拒否系を 1 つの fixture でまとめて観測する。
func TestArtifactE2E_RegisterRetrieveRead(t *testing.T) {
	t.Parallel()

	cwd, rillRoot := setupRillCWD(t)
	rec := openArtifactDB(t)

	const (
		runID    = "run-e2e-2026-05-02"
		workflow = "kiro-impl-loop"
	)

	// 既存 spec 規約に揃えた `.rilll/<workflow>/<runID>/<file>` レイアウトで実 I/O 配置する。
	// 拡張子は決定論性を優先し、Go 標準の builtin に含まれる .json / .html / .txt と、
	// 環境差を許容する .md を混ぜてメタの round-trip を観測する。
	relBase := filepath.Join(workflow, runID)
	specs := []e2eArtifactSpec{
		{Kind: ArtifactKindContext, RelPath: filepath.Join(relBase, "00-context.json"), Body: []byte(`{"task":"6.1"}`)},
		{Kind: ArtifactKindProposal, RelPath: filepath.Join(relBase, "01-proposal.md"), Body: []byte("# proposal\n\nbody\n")},
		{Kind: ArtifactKindReview, RelPath: filepath.Join(relBase, "02-review.html"), Body: []byte("<p>review</p>")},
		{Kind: ArtifactKindResult, RelPath: filepath.Join(relBase, "03-result.txt"), Body: []byte("result-ok")},
	}

	// path 昇順で GetArtifacts が返ることを assert するため、specs を path 順に揃えておく。
	// 上記の relBase 配下では数字 prefix によって 00 < 01 < 02 < 03 の順序が確定する。
	mtime := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	for i := range specs {
		specs[i].AbsPath = writeArtifactFile(t, rillRoot, specs[i].RelPath, specs[i].Body)
		specs[i].ExpectedMIME = computeExpectedMIME(specs[i].AbsPath)

		info, err := os.Stat(specs[i].AbsPath)
		if err != nil {
			t.Fatalf("Stat(%s): %v", specs[i].AbsPath, err)
		}
		if err := rec.RecordRunArtifact(context.Background(), RunArtifact{
			WorkflowRunID: runID,
			Kind:          specs[i].Kind,
			Path:          specs[i].AbsPath,
			SizeBytes:     info.Size(),
			Mtime:         mtime,
			MIME:          specs[i].ExpectedMIME,
		}); err != nil {
			t.Fatalf("RecordRunArtifact(%s): %v", specs[i].AbsPath, err)
		}
	}

	// Req 5.1: ファイル本体は `.rilll/` 配下にそのまま残っていること。
	for _, s := range specs {
		if _, err := os.Stat(s.AbsPath); err != nil {
			t.Errorf("post-register Stat(%s) failed (Req 5.1): %v", s.AbsPath, err)
		}
	}

	// Req 5.2: run_artifacts は本体複製列を持たず、path + メタのみで索引していること。
	gotColumns := runArtifactsTableColumns(t, rec)
	wantColumns := []string{"kind", "mime", "mtime", "path", "size_bytes", "workflow_run_id"}
	if !equalStringSlices(gotColumns, wantColumns) {
		t.Errorf("run_artifacts columns = %v, want %v (Req 5.2: 本体複製なし)", gotColumns, wantColumns)
	}

	// GetArtifacts: 4 件のメタが path 昇順で取得でき、登録値と完全一致する (Req 3.1)。
	rows, err := rec.GetArtifacts(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetArtifacts: %v", err)
	}
	if len(rows) != len(specs) {
		t.Fatalf("GetArtifacts len = %d, want %d", len(rows), len(specs))
	}
	for i, s := range specs {
		row := rows[i]
		if row.WorkflowRunID != runID {
			t.Errorf("rows[%d].WorkflowRunID = %q, want %q", i, row.WorkflowRunID, runID)
		}
		if row.Kind != s.Kind {
			t.Errorf("rows[%d].Kind = %q, want %q", i, row.Kind, s.Kind)
		}
		if row.Path != s.AbsPath {
			t.Errorf("rows[%d].Path = %q, want %q", i, row.Path, s.AbsPath)
		}
		// SizeBytes は os.Stat と一致する (DB に保存した値と round-trip)。
		info, err := os.Stat(s.AbsPath)
		if err != nil {
			t.Fatalf("Stat(%s) for size assertion: %v", s.AbsPath, err)
		}
		if row.SizeBytes != info.Size() {
			t.Errorf("rows[%d].SizeBytes = %d, want %d (os.Stat)", i, row.SizeBytes, info.Size())
		}
		if row.MIME != s.ExpectedMIME {
			t.Errorf("rows[%d].MIME = %q, want %q (mime.TypeByExtension)", i, row.MIME, s.ExpectedMIME)
		}
		if !row.Mtime.Equal(mtime) {
			t.Errorf("rows[%d].Mtime = %v, want %v", i, row.Mtime, mtime)
		}
		if row.Mtime.Location() != time.UTC {
			t.Errorf("rows[%d].Mtime.Location = %v, want UTC", i, row.Mtime.Location())
		}
	}

	// ReadArtifact: 上限内の登録済みパスは期待バイト列を返す (Req 3.3)。
	ctx := withCWD(cwd)
	for _, s := range specs {
		got, err := rec.ReadArtifact(ctx, s.AbsPath)
		if err != nil {
			t.Errorf("ReadArtifact(%s): %v", s.AbsPath, err)
			continue
		}
		if !bytes.Equal(got, s.Body) {
			t.Errorf("ReadArtifact(%s) bytes mismatch:\n got = %q\nwant = %q", s.AbsPath, got, s.Body)
		}
	}

	// ReadArtifactUnlimited: 上限なし経路でも同じバイト列が返ること (Req 3.5)。
	for _, s := range specs {
		got, err := rec.ReadArtifactUnlimited(ctx, s.AbsPath)
		if err != nil {
			t.Errorf("ReadArtifactUnlimited(%s): %v", s.AbsPath, err)
			continue
		}
		if !bytes.Equal(got, s.Body) {
			t.Errorf("ReadArtifactUnlimited(%s) bytes mismatch:\n got = %q\nwant = %q", s.AbsPath, got, s.Body)
		}
	}

	// 未登録パス: `.rilll/` 配下に実在するが run_artifacts に登録していないファイルは
	// ErrArtifactNotAllowed で拒否される (Req 4.1, 4.2)。
	unregistered := writeArtifactFile(t, rillRoot, filepath.Join(relBase, "99-unregistered.txt"), []byte("nope"))
	if _, err := rec.ReadArtifact(ctx, unregistered); err == nil {
		t.Errorf("ReadArtifact(%s): expected ErrArtifactNotAllowed, got nil", unregistered)
	} else if !errors.Is(err, ErrArtifactNotAllowed) {
		t.Errorf("ReadArtifact(%s): errors.Is(ErrArtifactNotAllowed) = false; err=%v", unregistered, err)
	}
	if _, err := rec.ReadArtifactUnlimited(ctx, unregistered); err == nil {
		t.Errorf("ReadArtifactUnlimited(%s): expected ErrArtifactNotAllowed, got nil", unregistered)
	} else if !errors.Is(err, ErrArtifactNotAllowed) {
		t.Errorf("ReadArtifactUnlimited(%s): errors.Is(ErrArtifactNotAllowed) = false; err=%v", unregistered, err)
	}
}

// TestArtifactE2E_OldRunGetArtifactsEmpty は Req 5.4 の旧 run シナリオを観測する。
// workflow_runs には行があるが run_artifacts には未登録の workflowRunID で
// GetArtifacts が空 slice + nil error を返すこと (= 過去 run の閲覧を阻害しない) を assert する。
func TestArtifactE2E_OldRunGetArtifactsEmpty(t *testing.T) {
	t.Parallel()

	rec := openArtifactDB(t)

	// workflow_runs に「旧 run」の行を 1 つ仕込む。run_artifacts には何も書かない。
	const oldRunID = "old-run-2025-12-31"
	if err := rec.RecordWorkflowRunStart(context.Background(), WorkflowRun{
		ID:           oldRunID,
		WorkflowID:   "git-commit",
		WorkflowName: "git commit",
		CWD:          "/tmp/old-cwd",
		StartedAt:    time.Date(2025, 12, 31, 23, 59, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("RecordWorkflowRunStart(old run): %v", err)
	}

	// 旧 run シナリオ: GetArtifacts は 0 件で nil error (Req 5.4)。
	rows, err := rec.GetArtifacts(context.Background(), oldRunID)
	if err != nil {
		t.Fatalf("GetArtifacts(old run): unexpected error: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("GetArtifacts(old run) len = %d, want 0 (Req 5.4)", len(rows))
	}
}

// equalStringSlices は 2 つの string slice が完全一致するかを判定する小ヘルパ。
// (sort.Strings 済みの想定で順序込みで比較する。)
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
