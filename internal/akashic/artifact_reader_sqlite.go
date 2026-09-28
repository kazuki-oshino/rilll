// Package akashic の artifact_reader_sqlite.go は、ArtifactReader interface
// (artifact_reader.go) の実装を *SQLiteRecorder のメソッドとして提供する。
//
// 実装方針:
//   - 既存 *sql.DB を共有し、Recorder と同じハンドル経由で読み取りを直列化する
//   - GetArtifacts は querySelectArtifactsByRunID を発行して run_artifacts のメタを返す
//     (ファイル本体を読まない、Req 3.3 lazy 読み出しの基礎)
//   - ReadArtifact / ReadArtifactUnlimited は ValidateAllowlist (artifact_path.go) で
//     allowlist 検証を行ってから os.Stat でサイズを観測し、ReadArtifact のみ
//     DefaultArtifactReadMaxBytes 超過時は ErrArtifactTooLarge を返してファイル本体を
//     読まない (Req 3.4 の "size 観測のみ" コントラクト)
//   - sentinel error (ErrArtifactNotAllowed / ErrArtifactFileMissing /
//     ErrArtifactTooLarge) は包んで返し、呼び出し側は errors.Is で判別する
//
// cwd の取り扱い:
//   - workflowctx.RunMetadata.CWD を直接参照すると workflowctx → akashic → workflowctx という
//     循環依存になるため、akashic 側で独立した context-key (runCWDKey / ContextWithRunCWD)
//     を提供する。
//   - ArtifactReader 呼び出し元 (TUI / 自律改善ワークフロー / 本パッケージ内テスト) は
//     ContextWithRunCWD で cwd を ctx に注入してから ReadArtifact / ReadArtifactUnlimited を
//     呼ぶ責務を持つ。注入無しの ctx は ErrArtifactNotAllowed として拒否する。
//   - 設計意図 (Recorder は process-scoped、cwd は run-scoped) は維持: *SQLiteRecorder には
//     cwd を持たせず、呼び出しごとに ctx 経由で受け取る。
package akashic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// runCWDKey は ArtifactReader の Read 経路で使う cwd を ctx 上で運ぶ private 型。
// 別パッケージから ctx を直接覗けないようにする (workflow.runMetadataKey と同パターン)。
type runCWDKey struct{}

// ContextWithRunCWD は ArtifactReader.ReadArtifact / ReadArtifactUnlimited が
// allowlist 検証で必要とする cwd を ctx に注入する。
//
// 設計上の役割: workflowctx.RunMetadata.CWD と同じ「run-scoped 作業ディレクトリ」を
// akashic 側に伝えるための独立経路。`workflowctx` パッケージは既に `akashic` を
// import しているため、akashic から workflowctx.RunMetadataFromContext を直接呼ぶと
// 循環依存になる。本関数で akashic 側に閉じた経路を提供することで、Read 経路の
// 呼び出し元 (TUI / 自律改善ワークフロー) は cwd を明示的に渡せる。
//
// 呼び出し例:
//
//	ctx = akashic.ContextWithRunCWD(ctx, cwd)
//	bytes, err := reader.ReadArtifact(ctx, path)
func ContextWithRunCWD(ctx context.Context, cwd string) context.Context {
	return context.WithValue(ctx, runCWDKey{}, cwd)
}

// runCWDFromContext は ctx に注入された cwd を返す。
// 未注入 / 空文字列 / 型不一致のいずれの場合も (空文字列, false) を返す
// (workflow.recorderFromContext と同等の "未注入を一意の (nil, false) に正規化" パターン)。
func runCWDFromContext(ctx context.Context) (string, bool) {
	raw := ctx.Value(runCWDKey{})
	if raw == nil {
		return "", false
	}
	cwd, ok := raw.(string)
	if !ok || cwd == "" {
		return "", false
	}
	return cwd, true
}

// osReadFile はテスト差し替え可能な os.ReadFile アダプタ。
// ReadArtifact のサイズ超過時にファイル本体が読まれないことをテストで観測するため、
// 関数値として持つ (本体は通常 os.ReadFile)。テストは package 内 (artifact_reader_sqlite_test.go)
// から書き換えてアサーションを行う。本番コード経路では本変数を書き換えない。
var osReadFile = os.ReadFile

// GetArtifacts は workflowRunID に紐づく run_artifacts のメタ一覧を path 昇順で返す
// (Req 3.1, 3.2, 5.4)。
//
// 動作概要:
//  1. querySelectArtifactsByRunID を db.QueryContext で発行
//  2. 各行を ArtifactRow に scan (mtime は parseStoredTime で UTC time.Time へ復元、
//     mime は sql.NullString → 空文字列フォールバックで返す)
//  3. 0 件 (workflowRunID == "" や旧 run の遡及未登録ケースを含む) は空 slice + nil error
//
// ファイル本体は読まない (Req 3.3 lazy 読み出し)。
// 呼び出し側は ArtifactRow.SizeBytes を見てから ReadArtifact / ReadArtifactUnlimited を
// 選択する想定。
func (r *SQLiteRecorder) GetArtifacts(ctx context.Context, workflowRunID string) ([]ArtifactRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("akashic: SQLiteRecorder is not initialized")
	}
	rows, err := r.db.QueryContext(ctx, querySelectArtifactsByRunID, workflowRunID)
	if err != nil {
		return nil, fmt.Errorf("akashic: get artifacts (run=%s): %w", workflowRunID, err)
	}
	defer rows.Close()

	var result []ArtifactRow
	for rows.Next() {
		var (
			runID, path, kind string
			sizeBytes         int64
			mtimeStr          string
			mime              sql.NullString
		)
		if err := rows.Scan(&runID, &path, &kind, &sizeBytes, &mtimeStr, &mime); err != nil {
			return nil, fmt.Errorf("akashic: get artifacts scan (run=%s): %w", workflowRunID, err)
		}
		mtime, err := parseStoredTime(mtimeStr)
		if err != nil {
			return nil, fmt.Errorf("akashic: get artifacts parse mtime (run=%s, path=%s): %w", runID, path, err)
		}
		result = append(result, ArtifactRow{
			WorkflowRunID: runID,
			Kind:          kind,
			Path:          path,
			SizeBytes:     sizeBytes,
			Mtime:         mtime,
			MIME:          nullStringValue(mime),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("akashic: get artifacts iter (run=%s): %w", workflowRunID, err)
	}
	return result, nil
}

// ReadArtifact は path のファイル本体をデフォルト上限 (DefaultArtifactReadMaxBytes)
// 以内で読み出して返す (Req 3.3, 3.4, 4.1〜4.5)。
//
// 失敗時の sentinel:
//   - allowlist 不合格 (cwd 未注入 / `.rilll` 外 / 未登録) → ErrArtifactNotAllowed
//   - target 不在 → ErrArtifactFileMissing (run_artifacts は変更しない、Req 3.7)
//   - サイズ上限超過 → ErrArtifactTooLarge (ファイル本体を読まない、Req 3.4)
func (r *SQLiteRecorder) ReadArtifact(ctx context.Context, path string) ([]byte, error) {
	return r.readArtifactWithLimit(ctx, path, true)
}

// ReadArtifactUnlimited は path のファイル本体をサイズ上限なしで読み出して返す (Req 3.5)。
// allowlist 検証は ReadArtifact と同一 (Req 4.4 の対称性)。
// OOM リスクは呼び出し側責任で、GetArtifacts.SizeBytes を確認してから明示的に呼ぶこと。
func (r *SQLiteRecorder) ReadArtifactUnlimited(ctx context.Context, path string) ([]byte, error) {
	return r.readArtifactWithLimit(ctx, path, false)
}

// readArtifactWithLimit は ReadArtifact / ReadArtifactUnlimited の共通実装。
//
// 共通フロー:
//  1. ctx から cwd を解決 (runCWDFromContext) — 未注入は ErrArtifactNotAllowed で拒否
//  2. ValidateAllowlist で allowlist 検証 — 失敗 sentinel はそのまま返す
//  3. os.Stat でサイズと存在を観測 (target 不在は ErrArtifactFileMissing)
//  4. applyLimit==true の場合のみ DefaultArtifactReadMaxBytes 超過チェック
//     超過時は ErrArtifactTooLarge を返してファイル本体を読まない (Req 3.4)
//  5. osReadFile で全量読み出し
func (r *SQLiteRecorder) readArtifactWithLimit(ctx context.Context, path string, applyLimit bool) ([]byte, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("akashic: SQLiteRecorder is not initialized")
	}

	cwd, ok := runCWDFromContext(ctx)
	if !ok {
		// cwd 未注入は allowlist 評価不能なので、安全側に倒して総合拒否 sentinel を返す。
		// 呼び出し元 (TUI / self-improve / テスト) は ContextWithRunCWD で必ず cwd を
		// 注入してから本メソッドを呼ぶ責任を持つ。
		return nil, fmt.Errorf("akashic: ReadArtifact requires cwd in ctx (use ContextWithRunCWD): %w", ErrArtifactNotAllowed)
	}

	normalized, err := ValidateAllowlist(ctx, r.db, cwd, path)
	if err != nil {
		// ValidateAllowlist は読み取り経路の sentinel に既に正規化済み
		// (ErrArtifactNotAllowed / ErrArtifactFileMissing) なので素通しで返す。
		return nil, err
	}

	info, err := os.Stat(normalized)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// 登録時には存在し、その後削除されたケース (Req 3.7)。
			// run_artifacts レコードは変更しない (本メソッドは読み取り専用)。
			return nil, fmt.Errorf("akashic: artifact file missing on disk (%s): %w", normalized, ErrArtifactFileMissing)
		}
		return nil, fmt.Errorf("akashic: stat artifact (%s): %w", normalized, err)
	}

	if applyLimit && info.Size() > DefaultArtifactReadMaxBytes {
		// Req 3.4: サイズ上限超過は os.Stat の観測のみで早期 return し、
		// osReadFile を呼ばない (大容量ファイルを誤って load しない安全網)。
		return nil, fmt.Errorf("akashic: artifact %s size=%d exceeds limit %d: %w",
			normalized, info.Size(), DefaultArtifactReadMaxBytes, ErrArtifactTooLarge)
	}

	data, err := osReadFile(normalized)
	if err != nil {
		// os.Stat と os.ReadFile の間にレースで削除された場合のフォールバック。
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("akashic: artifact file missing on disk (%s): %w", normalized, ErrArtifactFileMissing)
		}
		return nil, fmt.Errorf("akashic: read artifact (%s): %w", normalized, err)
	}
	return data, nil
}
