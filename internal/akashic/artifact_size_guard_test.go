// Package akashic の artifact_size_guard_test.go は、akashic-artifacts spec
// Task 6.2 が求める「ReadArtifact 大容量サイズガード性能テスト」を担う。
//
// 検証目的 (Req 3.4 / design.md "Performance/Load"):
//   - DefaultArtifactReadMaxBytes (10 MiB) を大きく超えるサイズのファイルに対し
//     ReadArtifact が以下を満たすことを実 SQLite + 実 sparse file で観測する。
//     (a) errors.Is(err, ErrArtifactTooLarge) が真
//     (b) ファイル本体の os.ReadFile (osReadFile 差し替えポイント) が呼ばれない
//     — size 観測のみで早期 return する経路の直接観測
//     (c) wall-clock 1 秒以内で完了する
//     — 10 GiB を実際に load していないことの間接観測 (二重防御)
//
// ファイル準備:
//   - `.rilll/` 配下に 10 GiB の sparse file を `os.Truncate` で作成する。
//     物理ブロック割当は行われないため、APFS / ext4 等のスパース対応 FS では
//     ディスク消費はメタデータ程度に収まる。
//   - sparse file をサポートしない FS や、TempDir が物理割当を強制する環境では
//     `os.Truncate` が ENOSPC 等で失敗するため `t.Skip` で portable に保つ。
//
// 設計メモ:
//   - 既存の osReadFile 差し替えポイント (artifact_reader_sqlite.go の package-level var)
//     を再利用するため、本テストは t.Parallel() を使わない
//     (Task 3.2 Implementation Notes と同じ理由)。
//   - DefaultArtifactReadMaxBytes は本テストでは差し替えない (本番の 10 MiB 上限と
//     10 GiB ファイルとの倍率差を実際に観測する意図のため)。
package akashic

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSQLiteRecorder_ReadArtifact_TenGiBSparseGuard は Task 6.2 の本体ケース。
// 10 GiB の sparse file に対し ReadArtifact が ErrArtifactTooLarge を size 観測のみで
// 返し、ファイル本体読み込み (osReadFile) を発行せず、wall-clock 1 秒以内で完了することを
// 検証する。
func TestSQLiteRecorder_ReadArtifact_TenGiBSparseGuard(t *testing.T) {
	// osReadFile (package-level var) を差し替えるため、t.Parallel() は使わない。

	cwd, rillRoot := setupRillCWD(t)
	rec := openArtifactDB(t)

	const tenGiB = int64(10) * 1024 * 1024 * 1024

	// 10 GiB sparse file を `.rilll/` 配下に配置する。
	// Create + Close で 0 byte の file を作り、os.Truncate で論理サイズだけ拡張する。
	// 物理ブロックは APFS / ext4 等で 0 のまま (メタデータのみ消費)。
	path := filepath.Join(rillRoot, "huge-sparse-10gib.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create(%s): %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close(%s): %v", path, err)
	}
	if err := os.Truncate(path, tenGiB); err != nil {
		// sparse file 非対応 FS や物理割当強制環境では Truncate が失敗する。
		// その場合は portable に保つため skip する (FAT32 等)。
		_ = os.Remove(path)
		t.Skipf("sparse file 非対応 FS のため Task 6.2 性能テストをスキップ: os.Truncate(%d): %v", tenGiB, err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	// 論理サイズが 10 GiB として観測されることを前提条件として確認する。
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}
	if info.Size() != tenGiB {
		t.Fatalf("post-truncate size = %d, want %d (sparse file 作成失敗)", info.Size(), tenGiB)
	}

	// run_artifacts に登録する (登録は本テスト固有の準備、Recorder 経路は Task 3.1 で実装済み)。
	const runID = "run-sparse-10gib"
	if err := rec.RecordRunArtifact(context.Background(), RunArtifact{
		WorkflowRunID: runID,
		Kind:          ArtifactKindContext,
		Path:          path,
		SizeBytes:     tenGiB,
		Mtime:         time.Now().UTC(),
		MIME:          "",
	}); err != nil {
		t.Fatalf("RecordRunArtifact: %v", err)
	}

	// osReadFile を観測用に差し替える。本体読み込み発行を直接検出するため。
	originalReadFile := osReadFile
	readCalled := false
	osReadFile = func(name string) ([]byte, error) {
		readCalled = true
		// 万が一呼ばれてしまった場合のセーフティ: 10 GiB を実 load しないように
		// オリジナルの os.ReadFile は意図的に呼ばず、空バイト列で返す
		// (テストはこのあと readCalled で fail させる)。
		return nil, nil
	}
	t.Cleanup(func() { osReadFile = originalReadFile })

	// wall-clock 1 秒以内で完了することの観測 (二重防御)。
	start := time.Now()
	_, err = rec.ReadArtifact(withCWD(cwd), path)
	elapsed := time.Since(start)

	// (a) ErrArtifactTooLarge を返すこと。
	if err == nil {
		t.Fatalf("ReadArtifact: expected ErrArtifactTooLarge, got nil")
	}
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("errors.Is(err, ErrArtifactTooLarge) = false; err=%v", err)
	}

	// (b) ファイル本体読み込みが発行されていないこと (Req 3.4: size 観測のみ)。
	if readCalled {
		t.Fatalf("osReadFile was called for 10 GiB sparse file; expected size-only check (Req 3.4)")
	}

	// (c) wall-clock 1 秒以内で完了 (大容量ファイルの早期 return を間接観測)。
	if elapsed > time.Second {
		t.Fatalf("ReadArtifact wall-clock = %v, want <1s (10 GiB を実 load している疑い)", elapsed)
	}
}
