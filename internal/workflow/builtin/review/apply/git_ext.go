// Package apply の本ファイルは task 4.2 / design.md §GitOpsExt を実装する
// (Req 5.3, 5.4)。
//
// 役割: review-apply-workflow の apply 経路で「accept された diff の作業ツリー反映」後段に
// 当たる `git add -- <files>` / `git commit -m <message> -- <files>` を、
// 既存 `improve/patch.GitOps` (Status / Snapshot / ApplyCheck / Apply / Revert) を破壊変更せず
// embedding によって seam 拡張する。
//
// 設計契約 (design.md §GitOpsExt L669-700):
//   - `GitOpsExt` interface は `improve/patch.GitOps` を embedding し、
//     Status / Snapshot / ApplyCheck / Apply / Revert を流用する。
//   - 追加メソッド: `Add(ctx, cwd, files)` / `Commit(ctx, cwd, message, files)`。
//   - default 実装は `os/exec.CommandContext` で git CLI を直叩きする。
//   - commit メッセージは Workflow 層 (task 5.4) で agent から取得し、`Commit` には完成形のみを渡す
//     (LLM 呼び出しは GitOpsExt 内で行わない、関心の分離)。
//   - 失敗時は `improvepatch.GitError{Stage:"add"|"commit"}` で wrap する。
//   - 不変式: `Add` / `Commit` は files を **明示的に渡す** (`git add .` / `git commit -a` を使わない)。
//     これにより 1 review run = 1 commit + scope guard で許可された file のみが commit される (Req 5.7, 5.3)。
//
// pre-flight 防御 (本パッケージで追加):
//   - 空 files は契約違反として `*improvepatch.GitError{Stage, Cause: ErrEmptyFiles}` を返し、
//     git CLI を一切起動しない。空 files を渡されたまま `git add --` / `git commit -m -- ` を
//     実行すると、git は「pathspec が無い」エラーで失敗するが、本実装は呼び出し前に reject
//     することで意図しない副作用 (例: 引数省略時に git の global config を読みに行く経路) を構造的に塞ぐ。
//   - 空 commit message は `*improvepatch.GitError{Stage:"commit", Cause: ErrEmptyCommitMessage}` を返し、
//     git CLI を一切起動しない。message は Workflow 層で agent 出力を確定させた完成形を渡す契約のため、
//     空文字列は契約違反であり pre-flight で reject する。
//
// 副作用契約 (design.md §GitOpsExt L676-684):
//   - LLM / network / filesystem (cwd 配下以外) を一切叩かない。
//   - 呼び出すのは `git` バイナリのみ (status / snapshot / apply-check / apply / revert は embedding 元、
//     add / commit は本実装で追加)。
//   - GitError.Stage は `improve/patch.GitError` の既存 5 値 (status / snapshot / apply-check / apply / revert)
//     に加えて "add" / "commit" を本パッケージから設定する (改めて design.md L678-679 で固定された契約)。
package apply

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"

	improvepatch "github.com/kazuki-oshino/rilll/internal/workflow/builtin/improve/patch"
)

// ErrEmptyFiles は `Add` / `Commit` が空 files で呼ばれたときに `*improvepatch.GitError.Cause` に
// 詰める sentinel error。errors.Is での識別を可能にする。
//
// 契約: design.md §GitOpsExt L700 不変式 (`git add .` / `git commit -a` を使わない) を満たすため、
// 空 files を渡された経路は git CLI を起動せず本 sentinel を Cause に詰めて即座に return する。
var ErrEmptyFiles = errors.New("review/apply/git_ext: files is empty (`git add .` / `git commit -a` are forbidden)")

// ErrEmptyCommitMessage は `Commit` が空 message で呼ばれたときに `*improvepatch.GitError.Cause` に
// 詰める sentinel error。errors.Is での識別を可能にする。
//
// 契約: design.md §GitOpsExt L680 (`Commit` には完成形のみを渡す) を満たすため、
// 空 message を渡された経路は git CLI を起動せず本 sentinel を Cause に詰めて即座に return する。
var ErrEmptyCommitMessage = errors.New("review/apply/git_ext: commit message is empty")

// GitOpsExt は `improve/patch.GitOps` を embedding し、Add / Commit を追加した
// review-apply 専用 git 操作 interface (design.md §GitOpsExt L691-695)。
//
// Workflow 層 (task 5.4) は accept 経路で次の順に呼び出す:
//  1. Snapshot (embedded) で BaselineRef を取得
//  2. ApplyCheck (embedded) で適用可能性を検証
//  3. Apply (embedded) で diff を作業ツリーへ反映
//  4. Add で変更された files を index に追加
//  5. Commit で 1 commit を生成
//  6. (失敗時) Revert (embedded) で baseline 状態へ復元
type GitOpsExt interface {
	improvepatch.GitOps // Status / Snapshot / ApplyCheck / Apply / Revert を流用する。

	// Add は `git add -- <files...>` を実行し、files を index に追加する (Req 5.3)。
	// 不変式: files は呼び出し側で明示的に列挙する (`.` / `-A` を使わない)。空 files は契約違反として
	// `*improvepatch.GitError{Stage:"add", Cause: ErrEmptyFiles}` を返し git CLI を起動しない。
	// 失敗時は `*improvepatch.GitError{Stage:"add", ExitCode, Stdout, Stderr, Cause}` を返す。
	Add(ctx context.Context, cwd string, files []string) error

	// Commit は `git commit -m <message> -- <files...>` を実行し、index 上の files を 1 commit にまとめる
	// (Req 5.3, 5.4)。message は Workflow 層で agent から取得した完成形を渡す契約 (LLM 呼び出しは行わない)。
	// 不変式: files は明示的に列挙し、`-a` を使わない。空 files / 空 message はいずれも契約違反として
	// `*improvepatch.GitError{Stage:"commit", Cause: ErrEmptyFiles | ErrEmptyCommitMessage}` を返し
	// git CLI を起動しない。失敗時は `*improvepatch.GitError{Stage:"commit", ExitCode, Stdout, Stderr, Cause}` を返す。
	Commit(ctx context.Context, cwd string, message string, files []string) error
}

// gitRunner は git サブプロセス起動の関数型 (テスト時 mock 注入用 seam)。
// 既定は `defaultGitRunner` (os/exec.CommandContext); テストでは `fakeGitRunner.run` を注入する。
//
// 戻り値: (stdout, stderr, err)。失敗時 err は通常 `*exec.ExitError` または `*exec.Error` (PATH 不在等)
// を含むが、本パッケージは err 種別を内部で判別せず GitError.Cause にそのまま詰める
// (`improve/patch.exitCodeFromErr` と同形)。
type gitRunner func(ctx context.Context, cwd string, args ...string) (stdout, stderr []byte, err error)

// defaultGitOpsExt は `GitOpsExt` interface の既定実装。
// `improvepatch.GitOps` を interface embedding することで Status / Snapshot / ApplyCheck / Apply / Revert
// を委譲し、本構造体は Add / Commit のみを追加実装する。
//
// 内部 seam: `runGit` 関数フィールドは os/exec を直叩きする `defaultGitRunner` を保持し、
// テストでは `newGitOpsExtForTest` 経由で fake runner を注入する。
type defaultGitOpsExt struct {
	improvepatch.GitOps // embedded interface (Status / Snapshot / ApplyCheck / Apply / Revert)。

	// runGit は git サブプロセス起動の seam。default は defaultGitRunner。
	runGit gitRunner
}

// 型レベル assertion: defaultGitOpsExt が GitOpsExt interface を満たすことをコンパイル時に保証する。
var _ GitOpsExt = (*defaultGitOpsExt)(nil)

// NewGitOpsExt は `os/exec.CommandContext` 経由で git CLI を直叩きする
// 既定 `GitOpsExt` 実装を返す (design.md §GitOpsExt L697)。
//
// embedded GitOps は `improvepatch.NewGitOps()` を流用するため、
// Status / Snapshot / ApplyCheck / Apply / Revert は既存の実装に委譲される。
func NewGitOpsExt() GitOpsExt {
	return &defaultGitOpsExt{
		GitOps: improvepatch.NewGitOps(),
		runGit: defaultGitRunner,
	}
}

// newGitOpsExtForTest は test 専用 constructor (lowercase = package private)。
// embedded GitOps と gitRunner を注入できるため、副作用ゼロで argv 構築 / 失敗 wrap / 防御を検証できる。
//
// production code path (workflow 層 task 5.4) からは呼び出さない契約 (lowercase 公開で構造的に塞ぐ)。
func newGitOpsExtForTest(base improvepatch.GitOps, runner gitRunner) GitOpsExt {
	return &defaultGitOpsExt{
		GitOps: base,
		runGit: runner,
	}
}

// Add は `git add -- <files...>` を実行し、files を index に追加する。
//
// 構築する argv: `["add", "--", file1, file2, ...]` (`git` バイナリは runner 側で固定、
// 本関数は引数のみを組み立てる)。`--` separator により後続要素は path 扱い (オプション誤認識防御)。
//
// 失敗経路:
//   - len(files) == 0: ErrEmptyFiles を Cause に詰めた `*improvepatch.GitError{Stage:"add"}` を返す
//     (runner は呼ばない)。
//   - runner 実行失敗: `*improvepatch.GitError{Stage:"add", ExitCode, Stdout, Stderr, Cause}` を返す。
func (g *defaultGitOpsExt) Add(ctx context.Context, cwd string, files []string) error {
	if len(files) == 0 {
		return &improvepatch.GitError{
			Stage:    "add",
			ExitCode: -1,
			Cause:    ErrEmptyFiles,
		}
	}
	// argv: `add -- file1 file2 ...`
	args := make([]string, 0, len(files)+2)
	args = append(args, "add", "--")
	args = append(args, files...)
	stdout, stderr, err := g.runGit(ctx, cwd, args...)
	if err != nil {
		return &improvepatch.GitError{
			Stage:    "add",
			ExitCode: exitCodeFromErr(err),
			Stdout:   string(stdout),
			Stderr:   string(stderr),
			Cause:    err,
		}
	}
	return nil
}

// Commit は `git commit -m <message> -- <files...>` を実行し、index 上の files を 1 commit にまとめる。
//
// 構築する argv: `["commit", "-m", message, "--", file1, file2, ...]`。
// `-m <message>` の組で commit メッセージを直接渡すため editor 起動は発生しない (決定的)。
// `--` separator により後続要素は path 扱い (オプション誤認識防御)。
//
// 失敗経路:
//   - len(files) == 0: ErrEmptyFiles を Cause に詰めた `*improvepatch.GitError{Stage:"commit"}` を返す
//     (runner は呼ばない)。
//   - message == "": ErrEmptyCommitMessage を Cause に詰めた `*improvepatch.GitError{Stage:"commit"}` を返す
//     (runner は呼ばない)。
//   - runner 実行失敗: `*improvepatch.GitError{Stage:"commit", ExitCode, Stdout, Stderr, Cause}` を返す。
func (g *defaultGitOpsExt) Commit(ctx context.Context, cwd string, message string, files []string) error {
	if len(files) == 0 {
		return &improvepatch.GitError{
			Stage:    "commit",
			ExitCode: -1,
			Cause:    ErrEmptyFiles,
		}
	}
	if message == "" {
		return &improvepatch.GitError{
			Stage:    "commit",
			ExitCode: -1,
			Cause:    ErrEmptyCommitMessage,
		}
	}
	// argv: `commit -m <message> -- file1 file2 ...`
	args := make([]string, 0, len(files)+4)
	args = append(args, "commit", "-m", message, "--")
	args = append(args, files...)
	stdout, stderr, err := g.runGit(ctx, cwd, args...)
	if err != nil {
		return &improvepatch.GitError{
			Stage:    "commit",
			ExitCode: exitCodeFromErr(err),
			Stdout:   string(stdout),
			Stderr:   string(stderr),
			Cause:    err,
		}
	}
	return nil
}

// defaultGitRunner は os/exec.CommandContext で `git <args...>` を実行する既定 runner。
// `improvepatch.defaultGitOps.runGit` と同形 (cmd.Dir = cwd, cmd.Env = os.Environ() を継承)。
//
// stdin は使わない (Add / Commit は標準入力を要求しない)。
// stdout / stderr はそれぞれ buffer に集約して呼び出し側 (`*defaultGitOpsExt.Add|Commit`) に返す。
func defaultGitRunner(ctx context.Context, cwd string, args ...string) ([]byte, []byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// exitCodeFromErr は err から子プロセスの終了コードを抽出する。
// `*exec.ExitError` でない (例: `*exec.Error` で git バイナリ不在 / ctx cancel / pre-flight reject)
// 場合は -1 を返す (`improvepatch.exitCodeFromErr` の規約と同形 / git.go L218)。
func exitCodeFromErr(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
