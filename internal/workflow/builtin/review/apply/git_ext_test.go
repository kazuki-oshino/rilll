// Package apply のテスト本ファイルは task 4.2 / design.md §GitOpsExt の契約を検証する
// (Req 5.3, 5.4)。
//
// 検証観点 (tasks.md 4.2 観察可能完了条件):
//   - argv 構築: Add は `add -- <files...>`、Commit は `commit -m <msg> -- <files...>` を組み立てる
//     (`git add .` / `git commit -a` を使わない構造的保証)。
//   - 失敗時: `improvepatch.GitError{Stage:"add"|"commit"}` でラップされ、
//     errors.As 経由で Stage / ExitCode / Cause が検査可能。
//   - 空 files の防御: `Add` / `Commit` が空 files で呼ばれた経路は git CLI を起動せず、
//     `improvepatch.GitError{Stage, Cause: ErrEmptyFiles}` を返す。
//   - 空 commit message の防御: `Commit` が空 message で呼ばれた経路は git CLI を起動せず、
//     `improvepatch.GitError{Stage:"commit", Cause: ErrEmptyCommitMessage}` を返す。
//   - 既存 GitOps メソッド (Status / Snapshot / ApplyCheck / Apply / Revert) は embedding 経由で
//     委譲され、GitOpsExt が `improvepatch.GitOps` interface を充足する (compile-time 保証 + runtime smoke)。
//
// 副作用契約 (design.md §GitOpsExt Responsibilities):
//   - LLM 呼び出しは GitOpsExt 内で行わない (関心の分離)。
//   - `Add` / `Commit` は files を明示的に渡し、`.` / `-a` を構造的に使わない。
package apply

import (
	"context"
	"errors"
	"reflect"
	"testing"

	improvepatch "github.com/kazuki-oshino/rilll/internal/workflow/builtin/improve/patch"
)

// fakeGitRunner は GitOpsExt の os/exec 経路を mock するための test double。
// 呼び出し argv / cwd / 呼び出し回数を capture し、stub 戻り値 (stdout / stderr / err) を返す。
// design.md §Testing Strategy L1005 で要求される argv 検証の最小単位。
type fakeGitRunner struct {
	// capturedCwd は最後に run() に渡された cwd 値。
	capturedCwd string
	// capturedArgs は最後に run() に渡された argv の copy。
	capturedArgs []string
	// callCount は run() の総呼び出し回数。空 files / 空 message 防御で 0 を期待する。
	callCount int

	// stub 戻り値。
	stdout []byte
	stderr []byte
	err    error
}

// run は gitRunner 関数型を満たす method。run の signature を gitRunner 型に揃えるため、
// receiver は &fakeGitRunner として渡す。
func (f *fakeGitRunner) run(_ context.Context, cwd string, args ...string) ([]byte, []byte, error) {
	f.capturedCwd = cwd
	// args は varargs なので、テスト側で後から書き換えられないよう deep copy する。
	f.capturedArgs = append([]string(nil), args...)
	f.callCount++
	return f.stdout, f.stderr, f.err
}

// stubGitOps は improvepatch.GitOps interface を満たすだけの最小 stub。
// GitOpsExt の Add / Commit の振る舞い検証では embedded GitOps の method は呼ばれないため、
// 戻り値はゼロ値で十分。embedding が interface を完全に充足することのコンパイル時保証も兼ねる。
type stubGitOps struct{}

func (stubGitOps) Status(_ context.Context, _ string) (bool, error) { return true, nil }
func (stubGitOps) Snapshot(_ context.Context, _ string) (improvepatch.BaselineRef, error) {
	return improvepatch.BaselineRef{}, nil
}
func (stubGitOps) ApplyCheck(_ context.Context, _ string, _ []byte) error { return nil }
func (stubGitOps) Apply(_ context.Context, _ string, _ []byte) error      { return nil }
func (stubGitOps) Revert(_ context.Context, _ string, _ improvepatch.BaselineRef, _ []string) error {
	return nil
}

// 型レベル assertion: stubGitOps が improvepatch.GitOps を満たすことをコンパイル時に保証する。
var _ improvepatch.GitOps = stubGitOps{}

// TestGitOpsExt_Add_ArgvConstruction は `Add(ctx, cwd, files)` が
// `git add -- file1 file2` 形式の argv を組み立てることを検証する (Req 5.3)。
// `git add .` / `git add -A` を構造的に使わないことの機械検証。
func TestGitOpsExt_Add_ArgvConstruction(t *testing.T) {
	t.Parallel()
	fake := &fakeGitRunner{}
	g := newGitOpsExtForTest(stubGitOps{}, fake.run)

	if err := g.Add(context.Background(), "/repo/root", []string{"a.go", "dir/b.go"}); err != nil {
		t.Fatalf("Add returned unexpected error: %v", err)
	}

	wantArgs := []string{"add", "--", "a.go", "dir/b.go"}
	if !reflect.DeepEqual(fake.capturedArgs, wantArgs) {
		t.Fatalf("expected argv %v, got %v", wantArgs, fake.capturedArgs)
	}
	if fake.capturedCwd != "/repo/root" {
		t.Fatalf("expected cwd=/repo/root, got %q", fake.capturedCwd)
	}
	if fake.callCount != 1 {
		t.Fatalf("expected runner call count == 1, got %d", fake.callCount)
	}
}

// TestGitOpsExt_Add_EmptyFiles は `Add` が空 files で呼ばれた経路で
// git CLI を一切起動せず `*improvepatch.GitError{Stage:"add", Cause: ErrEmptyFiles}` を返すことを検証する
// (design.md §GitOpsExt L700: `Add` / `Commit` は files を明示的に渡す不変式)。
func TestGitOpsExt_Add_EmptyFiles(t *testing.T) {
	t.Parallel()
	fake := &fakeGitRunner{}
	g := newGitOpsExtForTest(stubGitOps{}, fake.run)

	err := g.Add(context.Background(), "/repo/root", nil)
	if err == nil {
		t.Fatalf("expected Add(empty files) to return error, got nil")
	}
	var gitErr *improvepatch.GitError
	if !errors.As(err, &gitErr) {
		t.Fatalf("expected *improvepatch.GitError, got %T: %v", err, err)
	}
	if gitErr.Stage != "add" {
		t.Fatalf("expected Stage=add, got %q", gitErr.Stage)
	}
	if !errors.Is(err, ErrEmptyFiles) {
		t.Fatalf("expected errors.Is(err, ErrEmptyFiles) == true, got err=%v", err)
	}
	if fake.callCount != 0 {
		t.Fatalf("expected runner not to be called, got %d call(s)", fake.callCount)
	}

	// 空 slice 経路 (nil ではなく len==0) も同じ防御に乗ることを検証する。
	err2 := g.Add(context.Background(), "/repo/root", []string{})
	if !errors.Is(err2, ErrEmptyFiles) {
		t.Fatalf("expected empty slice to also trigger ErrEmptyFiles, got %v", err2)
	}
	if fake.callCount != 0 {
		t.Fatalf("expected runner still not to be called, got %d call(s)", fake.callCount)
	}
}

// TestGitOpsExt_Add_FailureWraps は runner が error を返した経路で
// `*improvepatch.GitError{Stage:"add"}` に Stdout / Stderr / Cause が詰められることを検証する (Req 5.5)。
func TestGitOpsExt_Add_FailureWraps(t *testing.T) {
	t.Parallel()
	wantCause := errors.New("simulated git add failure")
	fake := &fakeGitRunner{
		stdout: []byte("stdout-line\n"),
		stderr: []byte("fatal: pathspec 'a.go' did not match\n"),
		err:    wantCause,
	}
	g := newGitOpsExtForTest(stubGitOps{}, fake.run)

	err := g.Add(context.Background(), "/repo/root", []string{"a.go"})
	if err == nil {
		t.Fatalf("expected Add to return error, got nil")
	}
	var gitErr *improvepatch.GitError
	if !errors.As(err, &gitErr) {
		t.Fatalf("expected *improvepatch.GitError, got %T: %v", err, err)
	}
	if gitErr.Stage != "add" {
		t.Fatalf("expected Stage=add, got %q", gitErr.Stage)
	}
	if gitErr.Stdout != "stdout-line\n" {
		t.Fatalf("expected Stdout to be preserved, got %q", gitErr.Stdout)
	}
	if gitErr.Stderr != "fatal: pathspec 'a.go' did not match\n" {
		t.Fatalf("expected Stderr to be preserved, got %q", gitErr.Stderr)
	}
	if !errors.Is(err, wantCause) {
		t.Fatalf("expected errors.Is(err, wantCause) == true, got err=%v", err)
	}
	// 非 *exec.ExitError な原因の場合 ExitCode は -1 (improve/patch 規約 git.go L218)。
	if gitErr.ExitCode != -1 {
		t.Fatalf("expected ExitCode=-1 for non-ExitError cause, got %d", gitErr.ExitCode)
	}
}

// TestGitOpsExt_Commit_ArgvConstruction は `Commit(ctx, cwd, message, files)` が
// `git commit -m <message> -- file1 file2` 形式の argv を組み立てることを検証する (Req 5.3, 5.4)。
// `git commit -a` を構造的に使わないこと、message が `-m` の値として直接渡ること、
// files が `--` separator 後に展開されることの 3 点を機械検証する。
func TestGitOpsExt_Commit_ArgvConstruction(t *testing.T) {
	t.Parallel()
	fake := &fakeGitRunner{}
	g := newGitOpsExtForTest(stubGitOps{}, fake.run)

	msg := "review-apply: review-run-id=run-42 rules=[r1,r2]"
	if err := g.Commit(context.Background(), "/repo/root", msg, []string{"a.go", "dir/b.go"}); err != nil {
		t.Fatalf("Commit returned unexpected error: %v", err)
	}

	wantArgs := []string{"commit", "-m", msg, "--", "a.go", "dir/b.go"}
	if !reflect.DeepEqual(fake.capturedArgs, wantArgs) {
		t.Fatalf("expected argv %v, got %v", wantArgs, fake.capturedArgs)
	}
	if fake.capturedCwd != "/repo/root" {
		t.Fatalf("expected cwd=/repo/root, got %q", fake.capturedCwd)
	}
	if fake.callCount != 1 {
		t.Fatalf("expected runner call count == 1, got %d", fake.callCount)
	}
}

// TestGitOpsExt_Commit_EmptyFiles は `Commit` が空 files で呼ばれた経路で
// git CLI を一切起動せず `*improvepatch.GitError{Stage:"commit", Cause: ErrEmptyFiles}` を返すことを検証する。
func TestGitOpsExt_Commit_EmptyFiles(t *testing.T) {
	t.Parallel()
	fake := &fakeGitRunner{}
	g := newGitOpsExtForTest(stubGitOps{}, fake.run)

	err := g.Commit(context.Background(), "/repo/root", "msg", nil)
	if err == nil {
		t.Fatalf("expected Commit(empty files) to return error, got nil")
	}
	var gitErr *improvepatch.GitError
	if !errors.As(err, &gitErr) {
		t.Fatalf("expected *improvepatch.GitError, got %T: %v", err, err)
	}
	if gitErr.Stage != "commit" {
		t.Fatalf("expected Stage=commit, got %q", gitErr.Stage)
	}
	if !errors.Is(err, ErrEmptyFiles) {
		t.Fatalf("expected errors.Is(err, ErrEmptyFiles) == true, got err=%v", err)
	}
	if fake.callCount != 0 {
		t.Fatalf("expected runner not to be called, got %d call(s)", fake.callCount)
	}
}

// TestGitOpsExt_Commit_EmptyMessage は `Commit` が空 message で呼ばれた経路で
// git CLI を一切起動せず `*improvepatch.GitError{Stage:"commit", Cause: ErrEmptyCommitMessage}` を返すことを検証する。
// design.md §GitOpsExt L680: commit メッセージは Workflow 層で agent から取得し、`Commit` には完成形のみを渡す契約。
// 空メッセージは契約違反であり pre-flight で reject する。
func TestGitOpsExt_Commit_EmptyMessage(t *testing.T) {
	t.Parallel()
	fake := &fakeGitRunner{}
	g := newGitOpsExtForTest(stubGitOps{}, fake.run)

	err := g.Commit(context.Background(), "/repo/root", "", []string{"a.go"})
	if err == nil {
		t.Fatalf("expected Commit(empty message) to return error, got nil")
	}
	var gitErr *improvepatch.GitError
	if !errors.As(err, &gitErr) {
		t.Fatalf("expected *improvepatch.GitError, got %T: %v", err, err)
	}
	if gitErr.Stage != "commit" {
		t.Fatalf("expected Stage=commit, got %q", gitErr.Stage)
	}
	if !errors.Is(err, ErrEmptyCommitMessage) {
		t.Fatalf("expected errors.Is(err, ErrEmptyCommitMessage) == true, got err=%v", err)
	}
	if fake.callCount != 0 {
		t.Fatalf("expected runner not to be called, got %d call(s)", fake.callCount)
	}
}

// TestGitOpsExt_Commit_FailureWraps は runner が error を返した経路で
// `*improvepatch.GitError{Stage:"commit"}` に Stdout / Stderr / Cause が詰められることを検証する (Req 5.5)。
func TestGitOpsExt_Commit_FailureWraps(t *testing.T) {
	t.Parallel()
	wantCause := errors.New("simulated git commit failure")
	fake := &fakeGitRunner{
		stdout: []byte(""),
		stderr: []byte("error: nothing to commit, working tree clean\n"),
		err:    wantCause,
	}
	g := newGitOpsExtForTest(stubGitOps{}, fake.run)

	err := g.Commit(context.Background(), "/repo/root", "msg", []string{"a.go"})
	if err == nil {
		t.Fatalf("expected Commit to return error, got nil")
	}
	var gitErr *improvepatch.GitError
	if !errors.As(err, &gitErr) {
		t.Fatalf("expected *improvepatch.GitError, got %T: %v", err, err)
	}
	if gitErr.Stage != "commit" {
		t.Fatalf("expected Stage=commit, got %q", gitErr.Stage)
	}
	if gitErr.Stderr != "error: nothing to commit, working tree clean\n" {
		t.Fatalf("expected Stderr to be preserved, got %q", gitErr.Stderr)
	}
	if !errors.Is(err, wantCause) {
		t.Fatalf("expected errors.Is(err, wantCause) == true, got err=%v", err)
	}
	if gitErr.ExitCode != -1 {
		t.Fatalf("expected ExitCode=-1 for non-ExitError cause, got %d", gitErr.ExitCode)
	}
}

// TestGitOpsExt_EmbedsGitOps は `GitOpsExt` interface が `improvepatch.GitOps` の
// 全メソッドを embedding 経由で公開し、コンストラクタ `NewGitOpsExt()` が
// improvepatch.GitOps interface を完全に充足する値を返すことを runtime で smoke 検証する。
// design.md §GitOpsExt L692: `GitOpsExt` interface は improve/patch.GitOps を embedding する。
//
// なお、interface assertion (var _ improvepatch.GitOps = NewGitOpsExt()) は git_ext.go 側で
// compile-time に固定する (本テストは runtime での非 nil 性のみ確認する補強)。
func TestGitOpsExt_EmbedsGitOps(t *testing.T) {
	t.Parallel()
	g := NewGitOpsExt()
	if g == nil {
		t.Fatalf("NewGitOpsExt() returned nil")
	}
	// improvepatch.GitOps への代入が compile すれば embedding 経由の method 委譲は成立している。
	var asGitOps improvepatch.GitOps = g
	if asGitOps == nil {
		t.Fatalf("GitOpsExt does not satisfy improvepatch.GitOps at runtime")
	}
}
