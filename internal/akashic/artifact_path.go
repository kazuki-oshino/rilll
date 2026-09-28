// Package akashic の artifact_path.go は、`run_artifacts` の path 正規化と allowlist
// 検証ロジックを集約する。
//
// 責務:
//   - 登録経路 (NormalizeArtifactPath): `.rilll` ルート配下に位置する絶対 clean path への
//     正規化を行う。symlink / `..` 経由の `.rilll` 外脱出は ErrArtifactOutsideRillRoot
//     で拒否し、ファイル不在は ErrArtifactFileMissing (fs.ErrNotExist で wrap) を返す
//     (Req 1.2, 1.5, 4.5)。
//   - 読み取り経路 (ValidateAllowlist): 上記正規化に加えて run_artifacts への登録確認を
//     行い、`.rilll` ルート再確認で登録後の symlink 張り替え攻撃を排除する (Req 4.1〜4.5)。
//     allowlist 不成立はすべて ErrArtifactNotAllowed、target 不在は ErrArtifactFileMissing
//     を返す (Req 3.7)。
//
// 設計上の重要ポイント:
//   - `RillRootFromCWD` は登録経路のセマンティクス (ErrArtifactFileMissing) で sentinel を
//     返す。読み取り経路の ValidateAllowlist は内部で path を再構築する際に
//     ErrArtifactNotAllowed (拒否優先) に変換する (design.md "Failure mode policy"
//     の経路別 sentinel マッピング)。
//   - 正規化規則 (Abs → EvalSymlinks → Clean) は登録経路と読み取り経路で完全一致させ、
//     登録時に保存した path と読み取り時の検索 key が必ず一致するようにする。
package akashic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// queryCheckArtifactPathRegistered は ValidateAllowlist 内で使う path-only の
// 登録確認 SQL。任意 workflow_run_id について path が `run_artifacts` に登録済みかを
// SELECT 1 で判定する (Req 4.1, 4.2: ReadArtifact 系は path のみで allowlist 判定する)。
//
// `querySelectArtifactByPath` (queries.go) が `(workflow_run_id, path)` の複合 PK で
// 検索するのに対し、ValidateAllowlist は workflow_run_id を持たないため別 SQL を使う。
const queryCheckArtifactPathRegistered = `SELECT 1 FROM run_artifacts WHERE path = ? LIMIT 1`

// RillRootFromCWD は cwd から `.rilll` ルート (= cwd/.rilll の EvalSymlinks 結果) を
// 絶対 clean path として返す。
//
// 正規化手順:
//  1. filepath.Abs(cwd) で cwd を絶対化
//  2. cwd/.rilll を構築
//  3. filepath.EvalSymlinks で symlink 展開 (`.rilll` 自体が symlink でも正しく解決)
//  4. filepath.Clean で末尾区切り等を整える
//
// `.rilll` ルート不在 / EvalSymlinks 失敗時は ErrArtifactFileMissing (fs.ErrNotExist で
// wrap 済) を返す。読み取り経路で ErrArtifactNotAllowed が必要な場合は ValidateAllowlist
// 側で意図的に変換する (design.md Failure mode policy)。
func RillRootFromCWD(cwd string) (string, error) {
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("RillRootFromCWD: cwd を絶対パス化できない: %w", err)
	}
	rillCandidate := filepath.Join(absCWD, ".rilll")
	resolved, err := filepath.EvalSymlinks(rillCandidate)
	if err != nil {
		// `.rilll` 不在を含む全 EvalSymlinks 失敗を ErrArtifactFileMissing として扱う。
		// ErrArtifactFileMissing 自体が fs.ErrNotExist を wrap しているので、
		// errors.Is(err, fs.ErrNotExist) も真になる。
		return "", fmt.Errorf("RillRootFromCWD: .rilll ルートを解決できない (%s): %w",
			rillCandidate, ErrArtifactFileMissing)
	}
	return filepath.Clean(resolved), nil
}

// NormalizeArtifactPath は登録経路の path 正規化を行い、絶対 clean path を返す。
//
// 正規化手順:
//  1. RillRootFromCWD(cwd) で `.rilll` ルートの canonical 形を取得
//  2. filepath.Abs(path) で path を絶対化
//  3. filepath.EvalSymlinks で symlink を展開 (`..` を含むパスでも安全に解決)
//  4. filepath.Clean で `..` を解消し冗長な区切りを除去
//  5. RillRootFromCWD 結果を prefix とする領域内に位置することを filepath.Rel で確認
//
// 失敗時の sentinel:
//   - `.rilll` ルート不在 / target 不在 → ErrArtifactFileMissing (fs.ErrNotExist 互換)
//   - 正規化結果が `.rilll` 外 → ErrArtifactOutsideRillRoot (Req 1.5, 4.5)
func NormalizeArtifactPath(cwd, path string) (string, error) {
	rillRoot, err := RillRootFromCWD(cwd)
	if err != nil {
		return "", err
	}
	return resolveUnderRillRoot(path, rillRoot)
}

// resolveUnderRillRoot は登録 / 読み取りで共有する path 正規化コア。
// rillRoot は EvalSymlinks 済みの canonical 絶対 clean path であることを前提とする。
//
// 失敗時の sentinel:
//   - target 不在 / EvalSymlinks 失敗 → ErrArtifactFileMissing
//   - canonical 形が rillRoot 配下でない → ErrArtifactOutsideRillRoot
func resolveUnderRillRoot(path, rillRoot string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("artifact_path: path を絶対パス化できない (%s): %w", path, err)
	}
	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		// target 不在 / symlink chain 切断などはすべて ErrArtifactFileMissing で吸収する。
		return "", fmt.Errorf("artifact_path: path を解決できない (%s): %w",
			absPath, ErrArtifactFileMissing)
	}
	cleaned := filepath.Clean(resolved)
	if !isUnderRoot(cleaned, rillRoot) {
		return "", fmt.Errorf("artifact_path: path が .rilll ルート外を指す (root=%s, path=%s): %w",
			rillRoot, cleaned, ErrArtifactOutsideRillRoot)
	}
	return cleaned, nil
}

// ValidateAllowlist は読み取り経路 (ReadArtifact / ReadArtifactUnlimited) で
// path が allowlist を満たすことを検証し、絶対 clean path を返す。
//
// 検証手順:
//  1. `.rilll` ルート解決
//     - 不在 → ErrArtifactNotAllowed (拒否優先、design.md Failure mode policy)
//  2. NormalizeArtifactPath と同じ正規化規則を適用 (resolveUnderRillRoot を共有)
//     - target 不在 → ErrArtifactFileMissing (Req 3.7、DB レコードは変更しない)
//     - `.rilll` 外 → ErrArtifactNotAllowed
//  3. run_artifacts に登録済みか SELECT 1 で確認 (path-only)
//     - 未登録 → ErrArtifactNotAllowed (Req 4.1, 4.2)
//  4. `.rilll` ルート配下の再確認 (登録後に symlink を張り替えられたケースを排除)
//     - 2 で同等の検査済みのため通常は no-op だが、防御的に再評価する
//
// 戻り値: 正規化済み絶対 clean path (実 I/O 用キー、run_artifacts.path と一致)。
func ValidateAllowlist(ctx context.Context, db *sql.DB, cwd, path string) (string, error) {
	rillRoot, err := RillRootFromCWD(cwd)
	if err != nil {
		// `.rilll` ルート不在は読み取り経路では拒否優先 (ErrArtifactNotAllowed) に変換する。
		// 元 error も errors.Is で観測可能にするため Unwrap 連鎖は保ちつつ sentinel を差し替え。
		return "", fmt.Errorf("ValidateAllowlist: .rilll ルート不在: %w", ErrArtifactNotAllowed)
	}
	normalized, err := resolveUnderRillRoot(path, rillRoot)
	if err != nil {
		// target 不在は ErrArtifactFileMissing のまま伝搬 (Req 3.7)。
		// `.rilll` 外 (ErrArtifactOutsideRillRoot) は読み取り経路の総合拒否 sentinel に変換。
		if errors.Is(err, ErrArtifactOutsideRillRoot) {
			return "", fmt.Errorf("ValidateAllowlist: path が .rilll ルート外を指す: %w",
				ErrArtifactNotAllowed)
		}
		return "", err
	}
	registered, err := isPathRegistered(ctx, db, normalized)
	if err != nil {
		return "", fmt.Errorf("ValidateAllowlist: 登録確認に失敗 (%s): %w", normalized, err)
	}
	if !registered {
		return "", fmt.Errorf("ValidateAllowlist: path が run_artifacts に未登録 (%s): %w",
			normalized, ErrArtifactNotAllowed)
	}
	// `.rilll` ルート配下の再確認 (登録後に symlink を張り替えられたケースを排除)。
	// resolveUnderRillRoot で既に同等の検査済みのため通常は通過する (防御的二重化)。
	if !isUnderRoot(normalized, rillRoot) {
		return "", fmt.Errorf("ValidateAllowlist: 再評価で path が .rilll ルート外 (root=%s, path=%s): %w",
			rillRoot, normalized, ErrArtifactNotAllowed)
	}
	return normalized, nil
}

// isPathRegistered は run_artifacts に該当 path が 1 件でも登録されているかを返す。
// 任意 workflow_run_id について path が記録済みであることを判定する (Req 4.1, 4.2)。
func isPathRegistered(ctx context.Context, db *sql.DB, path string) (bool, error) {
	if db == nil {
		return false, errors.New("akashic: db is nil")
	}
	var dummy int
	err := db.QueryRowContext(ctx, queryCheckArtifactPathRegistered, path).Scan(&dummy)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, err
}

// isUnderRoot は child が root の配下 (root 自身も許容) に位置するかを判定する。
// 両者は EvalSymlinks 済みの絶対 clean path であることを前提とする。
//
// filepath.Rel で相対化し、結果が `..` で始まる / 絶対パスのままなら root 外と判定する。
// "." (root 自身) と root 配下の通常パスは true を返す。
func isUnderRoot(child, root string) bool {
	rel, err := filepath.Rel(root, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if strings.HasPrefix(rel, "..") {
		return false
	}
	if filepath.IsAbs(rel) {
		return false
	}
	return true
}
