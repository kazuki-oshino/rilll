// Package improvepatch は workflow-self-improve の improve-workflow が利用する
// 副作用コンポーネント (GitOps / GoTestRunner) と純粋ロジックコンポーネント
// (TargetResolver / Patch / ContextBuilder / PromptTemplate / Verdict / GoModInspector) の
// 契約と既定実装を提供するサブパッケージである。
//
// 本ファイルは GitOps interface と関連型 (BaselineRef / GitError / ErrRevertFailed)
// の型定義および既定実装 (os/exec 経由の git CLI ラッパー) を提供する。
package patch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// GitOps は improve-workflow が使用する git 操作の契約を表す。
// すべてのメソッドは cwd を `cmd.Dir` に設定する前提で実装され、
// orchestration 側で正規化済みの module root が渡される (design.md §5)。
//
// 呼び出し順序契約:
//   - Status: precheck フェーズで呼ばれる (Req 1.6)。
//   - Snapshot: precheck 通過後 (Status==clean かつ module path 一致) でのみ呼ばれる
//     (Req 5.1 / Resolved Decisions §7)。
//   - ApplyCheck → Apply: Snapshot 後の accept 経路で順に呼ばれる (Req 5.2, 5.4)。
//   - Revert: Apply 成功後の test 失敗経路でのみ呼ばれる (Req 5.6, 5.9)。
//     Apply 未実行 (`git_apply_failed` 経路) では呼ばれない契約。
type GitOps interface {
	// Status は `git status --porcelain` の出力 0 行を clean == true として返す (Req 1.6)。
	// staged / unstaged / 未追跡 (untracked) のいずれかが 1 件でも存在する場合 clean == false。
	Status(ctx context.Context, cwd string) (clean bool, err error)

	// Snapshot は `git rev-parse HEAD` の出力 (40 文字 hex SHA-1) を BaselineRef.HeadCommit に格納する
	// (Req 5.1 / Resolved Decisions §7)。precheck 通過後にのみ呼ばれる契約。
	Snapshot(ctx context.Context, cwd string) (BaselineRef, error)

	// ApplyCheck は diff を標準入力経由で `git apply --check` に渡し、適用可能性を検証する (Req 5.2)。
	// 適用不能な diff の場合 GitError を返し、Apply は呼ばれない契約。
	ApplyCheck(ctx context.Context, cwd string, diff []byte) error

	// Apply は diff を標準入力経由で `git apply` に渡し、作業ツリーへ反映する (Req 5.4)。
	// `--3way` は使用しない (single-file 限定の MVP 契約)。
	Apply(ctx context.Context, cwd string, diff []byte) error

	// Revert は targetFiles の各要素について `git checkout <ref.HeadCommit> -- <file>` を実行し、
	// baseline 状態に復元する (Req 5.6, 5.9)。Apply 成功後の test 失敗経路でのみ呼ばれる契約。
	// Revert 自体が失敗した場合は ErrRevertFailed を wrap した error を返す。
	Revert(ctx context.Context, cwd string, ref BaselineRef, targetFiles []string) error
}

// BaselineRef は Snapshot 時点の baseline git 状態を表す。
// MVP では HEAD コミット SHA のみを保持し、Revert の対象 ref として再利用する
// (design.md §7 Resolved Decisions / Req 5.1, 5.6, 5.9)。
type BaselineRef struct {
	// HeadCommit は `git rev-parse HEAD` の出力をそのまま格納した 40 文字 hex SHA-1。
	HeadCommit string
}

// GitError は git CLI 子プロセスの失敗情報を wrap する。
// orchestration 側で `result.json` に Stage / ExitCode / Stdout / Stderr を載せるため、
// 構造化された値として保持する (Req 5.9 / design.md GitOps 節)。
type GitError struct {
	// Stage は失敗が発生した契約上のフェーズ識別子。
	// 値は "status" / "snapshot" / "apply-check" / "apply" / "revert" のいずれか。
	Stage string
	// ExitCode は子プロセスの終了コード。LookPath 失敗等で取得不能な場合は -1 を格納する。
	ExitCode int
	// Stdout は子プロセスの標準出力 (任意で末尾を切り詰めて記録される想定)。
	Stdout string
	// Stderr は子プロセスの標準エラー出力 (任意で末尾を切り詰めて記録される想定)。
	Stderr string
	// Cause は元になった error (`*exec.ExitError` 等)。`errors.Is` / `errors.As` で
	// 連鎖判定できるよう Unwrap で公開する。
	Cause error
}

// Error は GitError の人間可読サマリを返す。result.json への載せ方は
// 既存 `[stream_idle_timeout]` パターンと整合させ、orchestration 側で
// `[verdict:<id>] ...` 形にラップされる (Verdict コンポーネント側責務)。
func (e *GitError) Error() string {
	if e == nil {
		return "improvepatch: <nil git error>"
	}
	if e.Cause != nil {
		return fmt.Sprintf("improvepatch: git %s failed (exit %d): %v", e.Stage, e.ExitCode, e.Cause)
	}
	return fmt.Sprintf("improvepatch: git %s failed (exit %d)", e.Stage, e.ExitCode)
}

// Unwrap は errors.Is / errors.As による連鎖判定を可能にする。
func (e *GitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// ErrRevertFailed は Revert 自体が失敗したことを示す sentinel error。
// orchestration 側で `revert_failed` verdict と BaselineRef / TargetFiles を result.json に保存し、
// ユーザに手動復旧手順 (`git checkout <head_commit> -- <file>`) を案内するシグナルとして利用する (Req 5.9)。
var ErrRevertFailed = errors.New("improvepatch: git checkout for revert failed (manual recovery required)")

// NewGitOps は os/exec 経由で git CLI を起動する既定 GitOps 実装を返す。
// すべての git サブプロセスは渡された cwd を `cmd.Dir` に設定し、
// `os.Environ()` をそのまま継承する (rilll が走る環境の git config を尊重 / design.md GitOps 節)。
func NewGitOps() GitOps {
	return &defaultGitOps{}
}

// defaultGitOps は GitOps interface の既定実装。
// git CLI を子プロセスとして起動し、stdout / stderr / 終了コードを GitError に wrap して返す。
type defaultGitOps struct{}

// Status は `git status --porcelain` を実行し、出力 0 行 (空白除去後 0 バイト) を
// `clean == true` として返す (Req 1.6)。
// staged / unstaged / 未追跡のいずれかが 1 件でもあると 1 行以上の出力になり clean == false。
func (g *defaultGitOps) Status(ctx context.Context, cwd string) (bool, error) {
	stdout, stderr, err := g.runGit(ctx, cwd, nil, "status", "--porcelain")
	if err != nil {
		return false, wrapGitError("status", stdout, stderr, err)
	}
	// `git status --porcelain` は変更が無いとき空出力を返すため、
	// 空白除去後 0 バイトを「clean」の唯一判定条件とする。
	return len(bytes.TrimSpace(stdout)) == 0, nil
}

// Snapshot は `git rev-parse HEAD` を実行し、40 文字 hex SHA-1 を BaselineRef.HeadCommit に格納する
// (Req 5.1 / Resolved Decisions §7)。
// precheck (Status clean かつ module path 一致) を通過した経路でのみ呼ばれる契約。
func (g *defaultGitOps) Snapshot(ctx context.Context, cwd string) (BaselineRef, error) {
	stdout, stderr, err := g.runGit(ctx, cwd, nil, "rev-parse", "HEAD")
	if err != nil {
		return BaselineRef{}, wrapGitError("snapshot", stdout, stderr, err)
	}
	// `git rev-parse HEAD` は 40 文字 hex SHA-1 + 改行を返す。改行のみ除去して格納する
	// (40 文字 hex 形式の検証は task 1.3 の契約検証テストで行う)。
	return BaselineRef{HeadCommit: strings.TrimSpace(string(stdout))}, nil
}

// ApplyCheck は diff を標準入力経由で `git apply --check` に渡し、適用可能性を検証する (Req 5.2)。
// 適用不能な場合は GitError を返し、Apply は呼ばれない契約 (orchestration 側責務)。
func (g *defaultGitOps) ApplyCheck(ctx context.Context, cwd string, diff []byte) error {
	stdout, stderr, err := g.runGit(ctx, cwd, diff, "apply", "--check")
	if err != nil {
		return wrapGitError("apply-check", stdout, stderr, err)
	}
	return nil
}

// Apply は diff を標準入力経由で `git apply` に渡し、作業ツリーへ反映する (Req 5.4)。
// `--3way` は使用しない (single-file 限定の MVP 契約 / design.md GitOps 節)。
func (g *defaultGitOps) Apply(ctx context.Context, cwd string, diff []byte) error {
	stdout, stderr, err := g.runGit(ctx, cwd, diff, "apply")
	if err != nil {
		return wrapGitError("apply", stdout, stderr, err)
	}
	return nil
}

// Revert は targetFiles の各要素について `git checkout <ref.HeadCommit> -- <file>` を実行し、
// baseline 状態へ復元する (Req 5.6, 5.9)。
// いずれかの checkout が失敗した時点で ErrRevertFailed を wrap した error を返し、
// 残りの targetFiles に対する checkout は行わない (部分復元の検出を呼び出し側に委ねる)。
func (g *defaultGitOps) Revert(ctx context.Context, cwd string, ref BaselineRef, targetFiles []string) error {
	for _, file := range targetFiles {
		stdout, stderr, err := g.runGit(ctx, cwd, nil, "checkout", ref.HeadCommit, "--", file)
		if err != nil {
			gitErr := wrapGitError("revert", stdout, stderr, err)
			// ErrRevertFailed と GitError の双方を errors.Is / errors.As で辿れるよう
			// 複数 %w で wrap する (Go 1.20+)。
			return fmt.Errorf("%w: file=%s: %w", ErrRevertFailed, file, gitErr)
		}
	}
	return nil
}

// runGit は git サブプロセスを起動し stdout / stderr / 実行 error を返す内部 helper。
// cmd.Dir = cwd、cmd.Env = os.Environ() を一律設定し、stdin が非 nil のときのみ
// `bytes.NewReader(stdin)` を cmd.Stdin に接続する。
func (g *defaultGitOps) runGit(ctx context.Context, cwd string, stdin []byte, args ...string) ([]byte, []byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// wrapGitError は git サブプロセスの失敗情報を GitError に詰める。
// stdout / stderr は文字列化のみで切り詰めは行わない (orchestration 側で result.json 化時に
// 任意で切り詰める設計 / design.md GitOps 節)。
func wrapGitError(stage string, stdout, stderr []byte, cause error) *GitError {
	return &GitError{
		Stage:    stage,
		ExitCode: exitCodeFromErr(cause),
		Stdout:   string(stdout),
		Stderr:   string(stderr),
		Cause:    cause,
	}
}

// exitCodeFromErr は err から子プロセスの終了コードを抽出する。
// `*exec.ExitError` でない (例: `*exec.Error` で git バイナリ不在 / ctx cancel) 場合は -1 を返す。
func exitCodeFromErr(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
