package akashic

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"
)

// Artifact kind 定数。`run_artifacts.kind` は closed set として運用し、
// 新規 kind を追加する場合は requirements.md の改訂を伴う (Req 1.7)。
const (
	ArtifactKindProposal = "proposal"
	ArtifactKindReview   = "review"
	ArtifactKindResult   = "result"
	ArtifactKindContext  = "context"
)

// DefaultArtifactReadMaxBytes は ArtifactReader.ReadArtifact が許容する
// 最大バイト数 (10 MiB)。テスト用に var として export する。
// ReadArtifactUnlimited は本値を参照しない (Req 3.4 / 3.5)。
var DefaultArtifactReadMaxBytes int64 = 10 * 1024 * 1024

// Artifact 関連の sentinel error。呼び出し側は errors.Is で判別する。
//
//   - ErrArtifactNotAllowed: 読み取り経路の総合拒否 sentinel。
//     allowlist 不成立 (.rilll ルート不在 / .rilll 外 / DB 未登録) すべてを覆う (Req 4.1〜4.4)。
//   - ErrArtifactFileMissing: ファイル不在 (FS 状態起因)。fs.ErrNotExist で wrap し、
//     errors.Is(err, fs.ErrNotExist) でも真になる (登録経路 / 読み取り経路の両方で使用)。
//   - ErrArtifactTooLarge: ReadArtifact がデフォルト上限を超過したファイルを読まずに返す (Req 3.4)。
//   - ErrUnknownArtifactKind: closed set 外の kind を受け取ったときに返す (Req 1.7)。
//   - ErrArtifactOutsideRillRoot: 登録経路専用。.rilll ルート外のパスを登録しようとしたときに返す (Req 1.5)。
var (
	ErrArtifactNotAllowed      = errors.New("akashic: artifact path not allowlisted")
	ErrArtifactFileMissing     = fmt.Errorf("akashic: artifact file missing on disk: %w", fs.ErrNotExist)
	ErrArtifactTooLarge        = errors.New("akashic: artifact exceeds default read size limit")
	ErrUnknownArtifactKind     = errors.New("akashic: unknown artifact kind")
	ErrArtifactOutsideRillRoot = errors.New("akashic: artifact path is outside the .rilll root")
)

// ArtifactRow は ArtifactReader.GetArtifacts の結果 1 件を表す。
// Path は登録時に正規化済みの絶対 clean path、Mtime は UTC、
// MIME は検出失敗時に空文字列 (DB では NULL)。
type ArtifactRow struct {
	WorkflowRunID string
	Kind          string
	Path          string
	SizeBytes     int64
	Mtime         time.Time
	MIME          string
}

// ArtifactReader は run_artifacts のメタ一覧と、allowlist を満たすファイルの
// 内容を返す読み取り契約。既存の Reader (純 SQL 5 メソッド) と分離した独立
// interface として定義することで、ファイル I/O を伴う API 表面を局所化する。
//
// 実装は *SQLiteRecorder が満たし (Reader と兼用)、CLI / TUI / 自律改善
// ワークフロー側は本 interface を経由してメタと内容にアクセスする。
type ArtifactReader interface {
	// GetArtifacts は workflowRunID に紐付く成果物のメタ一覧を返す。
	// 0 件の場合は空 slice + nil error を返し、エラーにしない (Req 3.2)。
	// ファイル本体は読まない (Req 3.3 の lazy 読み出し前提)。
	GetArtifacts(ctx context.Context, workflowRunID string) ([]ArtifactRow, error)

	// ReadArtifact は path のファイル本体をデフォルト上限 (DefaultArtifactReadMaxBytes)
	// 以内で読み出して返す。
	//   - allowlist 不合格 → ErrArtifactNotAllowed (Req 4.1〜4.4)
	//   - ファイル不在 → ErrArtifactFileMissing (Req 3.7、DB レコードは変更しない)
	//   - サイズ上限超過 → ErrArtifactTooLarge (Req 3.4、ファイル本体を読まない)
	ReadArtifact(ctx context.Context, path string) ([]byte, error)

	// ReadArtifactUnlimited は path のファイル本体をサイズ上限なしで読み出す (Req 3.5)。
	// allowlist 検証は ReadArtifact と同一 (Req 4.4 の対称性)。OOM リスクは
	// 呼び出し側責任で、GetArtifacts の SizeBytes を確認したうえで明示的に呼ぶ。
	ReadArtifactUnlimited(ctx context.Context, path string) ([]byte, error)
}

// ValidateArtifactKind は kind が closed set のいずれかであることを検証する (Req 1.7)。
// 未知の kind は ErrUnknownArtifactKind で wrap したエラーを返す。
func ValidateArtifactKind(kind string) error {
	switch kind {
	case ArtifactKindProposal, ArtifactKindReview, ArtifactKindResult, ArtifactKindContext:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrUnknownArtifactKind, kind)
	}
}
