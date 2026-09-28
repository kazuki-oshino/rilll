// Package improvepatch のテスト本ファイルは GitOps 既定実装の契約検証を行う。
// 実 git CLI subprocess を用いて os.MkdirTemp 上の fixture repo に対して
// Status / Snapshot / ApplyCheck / Apply / Revert と冪等性サイクルを検証する
// (tasks.md 1.3 / design.md §5, §7 / Req 1.6, 5.1, 5.2, 5.3, 5.4, 5.9)。
package patch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// sha1Re は BaselineRef.HeadCommit が 40 文字 hex SHA-1 形式であることを検証する。
// `git rev-parse HEAD` の出力契約 (design.md §7 Resolved Decisions) を機械検証する。
var sha1Re = regexp.MustCompile(`^[0-9a-f]{40}$`)

// fixtureContent は git init 直後に 1 commit する Go ファイルの初期内容。
// Apply / Revert で diff 適用先となる "baseline" 状態を構成する。
const fixtureContent = `package main

func main() {}
`

// validDiff は fixtureContent の末尾に "// added line" を 1 行追加する unified diff。
// `git apply --check` / `git apply` が成功する想定の正常 diff。
const validDiff = `diff --git a/sample.go b/sample.go
--- a/sample.go
+++ b/sample.go
@@ -1,3 +1,4 @@
 package main

 func main() {}
+// added line
`

// invalidDiff は context 行が fixture に存在しないため `git apply --check` / `git apply`
// が必ず失敗する不能 diff (失敗系の GitError 検証用)。
const invalidDiff = `diff --git a/sample.go b/sample.go
--- a/sample.go
+++ b/sample.go
@@ -1,3 +1,4 @@
 package other_package

 func main() {}
+// added line
`

// setupFixtureRepo は `os.MkdirTemp` ベース (t.TempDir 経由で auto cleanup) の
// 新規 git repo を init し、`sample.go` 1 ファイル + 1 commit の baseline 状態を作って返す。
// `user.name` / `user.email` は repo-local config として設定し、ユーザのグローバル
// git config を一切汚染しない (tasks.md 1.3 fixture 契約)。
func setupFixtureRepo(t *testing.T) (cwd string) {
	t.Helper()
	cwd = t.TempDir()

	// git init: master/main どちらでも構わないが、後続の HEAD checkout が動けばよいので
	// `--initial-branch=main` を明示して環境依存を排除する。
	runOrFatal(t, cwd, "git", "init", "--initial-branch=main")
	// repo-local user.name / user.email (グローバル設定不要 + commit 必須項目)。
	runOrFatal(t, cwd, "git", "config", "user.email", "test@example.com")
	runOrFatal(t, cwd, "git", "config", "user.name", "Test User")
	// fixture ファイル書き出し → add → commit。
	if err := os.WriteFile(filepath.Join(cwd, "sample.go"), []byte(fixtureContent), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	runOrFatal(t, cwd, "git", "add", "sample.go")
	runOrFatal(t, cwd, "git", "commit", "-m", "initial")
	return cwd
}

// runOrFatal は fixture セットアップ用の git/外部コマンドを失敗時に Fatalf で停止させる helper。
// 本体の defaultGitOps テスト経路 (= テスト対象) では使わず、fixture セットアップ専用。
func runOrFatal(t *testing.T, cwd string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\noutput: %s", name, args, err, string(out))
	}
}

// TestGitOps_Status は git status --porcelain が clean / staged dirty / unstaged dirty
// / untracked dirty の 4 ケースで期待通り (clean だけ true、それ以外 false) を返すことを検証する。
// Req 1.6 のクリーンツリー保証は 1 コマンドで機械検証可能 (design.md §5)。
func TestGitOps_Status(t *testing.T) {
	ctx := context.Background()
	g := NewGitOps()

	t.Run("clean", func(t *testing.T) {
		cwd := setupFixtureRepo(t)
		clean, err := g.Status(ctx, cwd)
		if err != nil {
			t.Fatalf("Status returned error: %v", err)
		}
		if !clean {
			t.Fatalf("expected clean=true, got false")
		}
	})

	t.Run("staged dirty", func(t *testing.T) {
		cwd := setupFixtureRepo(t)
		// 既存ファイルを書き換えて add すると "staged" 状態になる。
		if err := os.WriteFile(filepath.Join(cwd, "sample.go"), []byte(fixtureContent+"// staged\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		runOrFatal(t, cwd, "git", "add", "sample.go")
		clean, err := g.Status(ctx, cwd)
		if err != nil {
			t.Fatalf("Status returned error: %v", err)
		}
		if clean {
			t.Fatalf("expected clean=false (staged dirty), got true")
		}
	})

	t.Run("unstaged dirty", func(t *testing.T) {
		cwd := setupFixtureRepo(t)
		// 既存ファイルを書き換えるだけで add しないと "unstaged" 状態になる。
		if err := os.WriteFile(filepath.Join(cwd, "sample.go"), []byte(fixtureContent+"// unstaged\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		clean, err := g.Status(ctx, cwd)
		if err != nil {
			t.Fatalf("Status returned error: %v", err)
		}
		if clean {
			t.Fatalf("expected clean=false (unstaged dirty), got true")
		}
	})

	t.Run("untracked dirty", func(t *testing.T) {
		cwd := setupFixtureRepo(t)
		// 新規ファイルを add せずに置くと "untracked" 状態になる。
		if err := os.WriteFile(filepath.Join(cwd, "new.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		clean, err := g.Status(ctx, cwd)
		if err != nil {
			t.Fatalf("Status returned error: %v", err)
		}
		if clean {
			t.Fatalf("expected clean=false (untracked dirty), got true")
		}
	})
}

// TestGitOps_Snapshot は `git rev-parse HEAD` の出力が 40 文字 hex SHA-1 形式であることを
// regex `^[0-9a-f]{40}$` で検証する (design.md §7 Resolved Decisions / Req 5.1)。
func TestGitOps_Snapshot(t *testing.T) {
	ctx := context.Background()
	cwd := setupFixtureRepo(t)
	g := NewGitOps()

	ref, err := g.Snapshot(ctx, cwd)
	if err != nil {
		t.Fatalf("Snapshot returned error: %v", err)
	}
	if !sha1Re.MatchString(ref.HeadCommit) {
		t.Fatalf("expected 40-char hex SHA-1, got %q", ref.HeadCommit)
	}
}

// TestGitOps_ApplyCheck は適用可能 / 不能 2 ケースで `git apply --check` の振る舞いを検証する。
// 失敗時は GitError.Stage == "apply-check" であることも併せて検証する (Req 5.2 / design.md §5)。
func TestGitOps_ApplyCheck(t *testing.T) {
	ctx := context.Background()
	g := NewGitOps()

	t.Run("appliable", func(t *testing.T) {
		cwd := setupFixtureRepo(t)
		if err := g.ApplyCheck(ctx, cwd, []byte(validDiff)); err != nil {
			t.Fatalf("ApplyCheck returned error for valid diff: %v", err)
		}
	})

	t.Run("not appliable", func(t *testing.T) {
		cwd := setupFixtureRepo(t)
		err := g.ApplyCheck(ctx, cwd, []byte(invalidDiff))
		if err == nil {
			t.Fatalf("expected ApplyCheck to fail for invalid diff, got nil")
		}
		var gitErr *GitError
		if !errors.As(err, &gitErr) {
			t.Fatalf("expected *GitError, got %T: %v", err, err)
		}
		if gitErr.Stage != "apply-check" {
			t.Fatalf("expected Stage=apply-check, got %q", gitErr.Stage)
		}
		if gitErr.ExitCode == 0 {
			t.Fatalf("expected non-zero ExitCode, got 0")
		}
	})
}

// TestGitOps_Apply は実 apply の成功 / 失敗 2 ケースを検証する。
// 失敗時 GitError.Stage == "apply" / ExitCode != 0 が保証されること (Req 5.4 / design.md §5)。
func TestGitOps_Apply(t *testing.T) {
	ctx := context.Background()
	g := NewGitOps()

	t.Run("success", func(t *testing.T) {
		cwd := setupFixtureRepo(t)
		if err := g.Apply(ctx, cwd, []byte(validDiff)); err != nil {
			t.Fatalf("Apply returned error for valid diff: %v", err)
		}
		// 適用後、sample.go の末尾に "// added line" が追加されていることを確認。
		got, err := os.ReadFile(filepath.Join(cwd, "sample.go"))
		if err != nil {
			t.Fatalf("read sample.go: %v", err)
		}
		if !strings.Contains(string(got), "// added line") {
			t.Fatalf("expected file to contain '// added line', got:\n%s", string(got))
		}
	})

	t.Run("failure", func(t *testing.T) {
		cwd := setupFixtureRepo(t)
		err := g.Apply(ctx, cwd, []byte(invalidDiff))
		if err == nil {
			t.Fatalf("expected Apply to fail for invalid diff, got nil")
		}
		var gitErr *GitError
		if !errors.As(err, &gitErr) {
			t.Fatalf("expected *GitError, got %T: %v", err, err)
		}
		if gitErr.Stage != "apply" {
			t.Fatalf("expected Stage=apply, got %q", gitErr.Stage)
		}
		if gitErr.ExitCode == 0 {
			t.Fatalf("expected non-zero ExitCode, got 0")
		}
	})
}

// TestGitOps_Revert は (a) HEAD checkout で baseline が復元される正常経路と
// (b) 削除済み / 未追跡ファイル等で `git checkout` 自体が失敗し ErrRevertFailed が wrap される経路を検証する
// (Req 5.6, 5.9 / design.md §7)。
func TestGitOps_Revert(t *testing.T) {
	ctx := context.Background()
	g := NewGitOps()

	t.Run("restore baseline from HEAD", func(t *testing.T) {
		cwd := setupFixtureRepo(t)
		ref, err := g.Snapshot(ctx, cwd)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if err := g.Apply(ctx, cwd, []byte(validDiff)); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		// 適用済みであることを念のため確認。
		modified, err := os.ReadFile(filepath.Join(cwd, "sample.go"))
		if err != nil {
			t.Fatalf("read modified: %v", err)
		}
		if !strings.Contains(string(modified), "// added line") {
			t.Fatalf("pre-revert file should contain added line")
		}

		if err := g.Revert(ctx, cwd, ref, []string{"sample.go"}); err != nil {
			t.Fatalf("Revert returned error: %v", err)
		}

		restored, err := os.ReadFile(filepath.Join(cwd, "sample.go"))
		if err != nil {
			t.Fatalf("read restored: %v", err)
		}
		if string(restored) != fixtureContent {
			t.Fatalf("expected baseline content, got:\n%s", string(restored))
		}
	})

	t.Run("ErrRevertFailed when target unknown to HEAD", func(t *testing.T) {
		cwd := setupFixtureRepo(t)
		ref, err := g.Snapshot(ctx, cwd)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		// HEAD に存在しないパス指定で `git checkout` は failure exit する。
		err = g.Revert(ctx, cwd, ref, []string{"does_not_exist_in_head.go"})
		if err == nil {
			t.Fatalf("expected Revert to fail, got nil")
		}
		if !errors.Is(err, ErrRevertFailed) {
			t.Fatalf("expected errors.Is(err, ErrRevertFailed) == true, got err=%v", err)
		}
		var gitErr *GitError
		if !errors.As(err, &gitErr) {
			t.Fatalf("expected wrapped *GitError, got %T: %v", err, err)
		}
		if gitErr.Stage != "revert" {
			t.Fatalf("expected Stage=revert, got %q", gitErr.Stage)
		}
	})
}

// TestGitOps_IdempotencyCycle は Snapshot → Apply → Revert → Status clean → Snapshot 再取得
// が同じ SHA-1 を返すことを検証する (tasks.md 1.3 観察可能完了条件 / design.md §7)。
// この性質は Req 5.9 (revert 後の baseline 一致) と Req 1.6 (clean tree 復元)
// を機械検証する最小サイクルである。
func TestGitOps_IdempotencyCycle(t *testing.T) {
	ctx := context.Background()
	cwd := setupFixtureRepo(t)
	g := NewGitOps()

	ref1, err := g.Snapshot(ctx, cwd)
	if err != nil {
		t.Fatalf("first Snapshot: %v", err)
	}
	if !sha1Re.MatchString(ref1.HeadCommit) {
		t.Fatalf("first Snapshot SHA-1 invalid: %q", ref1.HeadCommit)
	}

	if err := g.Apply(ctx, cwd, []byte(validDiff)); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if err := g.Revert(ctx, cwd, ref1, []string{"sample.go"}); err != nil {
		t.Fatalf("Revert: %v", err)
	}

	clean, err := g.Status(ctx, cwd)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !clean {
		t.Fatalf("expected clean=true after Revert, got false")
	}

	ref2, err := g.Snapshot(ctx, cwd)
	if err != nil {
		t.Fatalf("second Snapshot: %v", err)
	}
	if ref1.HeadCommit != ref2.HeadCommit {
		t.Fatalf("expected same HeadCommit after cycle, got %q vs %q", ref1.HeadCommit, ref2.HeadCommit)
	}
}
