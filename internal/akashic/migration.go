package akashic

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MigrationResult は Migrate の実行結果。
//
//   - Path:    起動時に採用すべき DB ファイル path。rename 成功時は新 path、
//     失敗 / 部分失敗時は旧 path、no-op 時は引数 path を返す。
//     いずれの場合も呼び出し側はこの Path を OpenSQLite に渡せる
//   - Warning: 移行通知 / 失敗通知。空文字列は no-op (warning なし) を意味する
type MigrationResult struct {
	Path    string
	Warning string
}

// Migrate は採用予定の SQLite path を受け取り、必要に応じて旧名 (`observability.sqlite`)
// から新名 (`akashic.sqlite`) への自動移行を実行する。
//
// この関数は SQLite open より前に呼び出す前提で設計されている (open 中の rename は
// SQLite が保証しないため)。
//
// # 旧 path 導出
//
// basename 内 `akashic` を `observability` に文字列置換して旧 path を生成する。
//   - 限界 1: env override で旧名 path (`<dir>/observability.sqlite`) を強制した場合は
//     basename に `akashic` を含まないため no-op となる。これは意図された挙動であり、
//     ユーザが旧パス継続を明示しているケースに該当する (Req 3.3 の deprecation warning
//     で別途案内する)
//   - 限界 2: `RILLL_AKASHIC_DB` にカスタム名 (例: `/data/akashic-backup.sqlite`) を
//     指定された場合、`/data/observability-backup.sqlite` が存在すれば silent に
//     rename を試行する。ファイル不在時は no-op となるため実害は限定的だが、
//     想定外の rename を完全に排除したい場合は将来「default XDG path 利用時のみ
//     migration を起動」へ制限する設計に切り替える余地を残す
//
// # 冪等判定
//
// 「新パスが存在する」または「旧パスが存在しない」場合は no-op (warning なし)。
//
// # Rename 順序と部分失敗
//
// rename は `<base>-shm` → `<base>-wal` → `<base>` (main) の順で実施する。
// main rename が成功した時点で migration は成功とみなし、新 path を返す。
//
//   - `-shm` 成功 → `-wal` 失敗時: `-shm` を旧名に戻すロールバックを試み、
//     main rename は実行せず旧 path を採用する (warning に明示)
//   - main rename 失敗時: `os.Stat(oldPath)` で旧ファイル健在を確認し、
//     旧 path を採用 (-shm / -wal が新 path 側に rename 済みの場合はそのまま残置;
//     SQLite は次回 open 時に main と同 basename で WAL/SHM を再生成するため、
//     運用上の損失は clean shutdown 前提で許容する)
//   - 補助ファイル (-shm / -wal) のみの単独失敗: best-effort で warning に記録するが
//     main rename は通常通り続行する
//
// # データ無改変
//
// rename は os.Rename 経由のため SQLite ファイルバイナリは変更されない (POSIX
// rename(2) は atomic で、失敗時は元の状態が保持される)。
func Migrate(path string) MigrationResult {
	base := filepath.Base(path)
	if !strings.Contains(base, "akashic") {
		// basename に "akashic" を含まない → 旧名導出不可 (env override で旧パス強制時の no-op)
		return MigrationResult{Path: path}
	}

	// 冪等判定 1: 新パスが既に存在する → no-op (両方存在時も新優先で旧保持; Req 2.5, 2.8)
	if _, err := os.Stat(path); err == nil {
		return MigrationResult{Path: path}
	}

	dir := filepath.Dir(path)
	oldBase := strings.Replace(base, "akashic", "observability", 1)
	oldPath := filepath.Join(dir, oldBase)

	// 冪等判定 2: 旧パスが存在しない → no-op (fresh install / 既に移行済みで旧ファイルが消えた状態)
	if _, err := os.Stat(oldPath); err != nil {
		return MigrationResult{Path: path}
	}

	auxWarnings := []string{}
	shmRenamed := false

	// -shm rename (best-effort、存在時のみ)
	shmOld := oldPath + "-shm"
	shmNew := path + "-shm"
	if _, err := os.Stat(shmOld); err == nil {
		if rerr := os.Rename(shmOld, shmNew); rerr != nil {
			auxWarnings = append(auxWarnings,
				fmt.Sprintf("WAL 副ファイル %s の rename に失敗しました: %v", shmOld, rerr))
		} else {
			shmRenamed = true
		}
	}

	// -wal rename (best-effort、存在時のみ)
	walOld := oldPath + "-wal"
	walNew := path + "-wal"
	if _, err := os.Stat(walOld); err == nil {
		if rerr := os.Rename(walOld, walNew); rerr != nil {
			// -shm 成功 → -wal 失敗 のロールバック (main rename をスキップ)
			rollbackMsg := ""
			if shmRenamed {
				if rbErr := os.Rename(shmNew, shmOld); rbErr != nil {
					rollbackMsg = fmt.Sprintf(" (-shm のロールバックも失敗しました: %v)", rbErr)
				} else {
					rollbackMsg = " (-shm を旧名にロールバックしました)"
				}
			}
			warning := fmt.Sprintf(
				"アカシックレコード DB の自動移行を中止しました (WAL 副ファイル %s の rename 失敗: %v)。旧 path %s を継続使用します。%s",
				walOld, rerr, oldPath, rollbackMsg)
			return MigrationResult{Path: oldPath, Warning: warning}
		}
	}

	// main rename
	if rerr := os.Rename(oldPath, path); rerr != nil {
		// main rename 失敗 → 旧ファイル健在を確認して旧 path 採用 (起動継続; Req 2.6)
		// 注: -shm / -wal が新 path 側に rename 済みの場合はそのまま残置する (spec 通り)
		if _, statErr := os.Stat(oldPath); statErr != nil {
			warning := fmt.Sprintf(
				"アカシックレコード DB の自動移行に失敗しました (%v)。旧 path %s も見つかりません。",
				rerr, oldPath)
			return MigrationResult{Path: path, Warning: warning}
		}
		warning := fmt.Sprintf(
			"アカシックレコード DB の自動移行に失敗しました (%v)。旧 path %s を継続使用します。",
			rerr, oldPath)
		if len(auxWarnings) > 0 {
			warning = warning + "\n" + strings.Join(auxWarnings, "\n")
		}
		return MigrationResult{Path: oldPath, Warning: warning}
	}

	// 成功: 1 行ログとして旧 path → 新 path を含む通知 (Req 2.4, 2.9)
	successMsg := fmt.Sprintf("アカシックレコード DB を移行しました: %s -> %s", oldPath, path)
	if len(auxWarnings) > 0 {
		successMsg = successMsg + "\n" + strings.Join(auxWarnings, "\n")
	}
	return MigrationResult{Path: path, Warning: successMsg}
}
