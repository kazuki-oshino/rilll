// Package improvepatch のテスト本ファイルは TargetResolver interface と
// 既定実装 (NewDefaultResolver) の挙動契約を網羅検証する
// (tasks.md 4.1 / design.md §TargetResolver / Req 1.5, 7.1〜7.4)。
//
// 検証観点 (tasks.md 4.1 観察可能完了条件):
//   - 既存 4 ワークフロー (update-readme / git-commit / kiro-impl-loop /
//     update-readme-and-commit) 全てで存在パスが返る
//   - 未知 ID で ErrTargetNotResolved がラップされた error が返る
//   - .go 以外 (.txt) のファイル名側 fixture でも ErrTargetNotResolved が返る
//     (resolver は `<snake>.go` のみを構築し .txt にフォールバックしない)
//   - 戻り値は絶対 clean path (Preconditions: cwd は絶対パス、Postconditions: os.Stat 検証済み)
package patch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// 型レベル assertion: NewDefaultResolver() の戻り値が TargetResolver interface を満たす。
// task 4.1 観察可能完了条件「TargetResolver interface + DefaultResolver」のコンパイル時保証。
var _ TargetResolver = newDefaultResolverForTypeCheck()

// newDefaultResolverForTypeCheck は型 assertion 用に NewDefaultResolver を呼ぶ helper。
// init 時の評価で interface 充足性が壊れていればコンパイルエラーになる。
func newDefaultResolverForTypeCheck() TargetResolver {
	return NewDefaultResolver()
}

// makeWorkflowFixture は workflow ファイル fixture を <root>/internal/workflow/<name> に作成する。
// 4 ワークフロー網羅テスト + .txt ケース fixture 両方で再利用される。
func makeWorkflowFixture(t *testing.T, root string, fileName string) string {
	t.Helper()
	dir := filepath.Join(root, "internal", "workflow")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir internal/workflow: %v", err)
	}
	full := filepath.Join(dir, fileName)
	// fixture 中身は空の Go ソース。resolver は内容を見ず os.Stat のみ実施する契約。
	if err := os.WriteFile(full, []byte("package workflow\n"), 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", fileName, err)
	}
	return full
}

// TestDefaultResolver_Resolve_ExistingWorkflows は既存 4 ワークフロー ID の網羅成功ケース。
// kebab→snake 変換が正しく行われ、`internal/workflow/<snake>.go` の絶対 clean path が返ることを検証する
// (tasks.md 4.1 既存 4 ワークフロー網羅 / Req 1.5, 7.2)。
func TestDefaultResolver_Resolve_ExistingWorkflows(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name       string // workflow ID (kebab-case)
		wantSuffix string // 期待 fixture file 名 (snake_case + .go)
	}{
		{name: "update-readme", wantSuffix: "update_readme.go"},
		{name: "git-commit", wantSuffix: "git_commit.go"},
		{name: "kiro-impl-loop", wantSuffix: "kiro_impl_loop.go"},
		{name: "update-readme-and-commit", wantSuffix: "update_readme_and_commit.go"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			wantPath := makeWorkflowFixture(t, root, tc.wantSuffix)

			resolver := NewDefaultResolver()
			got, err := resolver.Resolve(ctx, root, tc.name)
			if err != nil {
				t.Fatalf("Resolve(%q): 予期しない error: %v", tc.name, err)
			}

			// 戻り値は絶対 clean path であること (Postconditions / design.md §TargetResolver)
			if !filepath.IsAbs(got) {
				t.Errorf("Resolve(%q): 戻り値が絶対パスでない: %q", tc.name, got)
			}
			wantClean := filepath.Clean(wantPath)
			if got != wantClean {
				t.Errorf("Resolve(%q): path mismatch\n  got:  %q\n  want: %q", tc.name, got, wantClean)
			}
		})
	}
}

// TestDefaultResolver_Resolve_UnknownID は未知 workflow ID で ErrTargetNotResolved が
// (errors.Is で検出可能な形で) 返ることを検証する (tasks.md 4.1 / Req 7.4)。
func TestDefaultResolver_Resolve_UnknownID(t *testing.T) {
	ctx := context.Background()

	// fixture を一切作らず、未知 ID で呼び出す。
	root := t.TempDir()
	resolver := NewDefaultResolver()

	got, err := resolver.Resolve(ctx, root, "unknown-workflow-id")
	if err == nil {
		t.Fatalf("未知 ID で error を期待したが nil が返った (got path=%q)", got)
	}
	if !errors.Is(err, ErrTargetNotResolved) {
		t.Errorf("未知 ID error は ErrTargetNotResolved を errors.Is で検出可能な形で返すべき: got %v", err)
	}
	if got != "" {
		t.Errorf("error 経路では path は空文字列を期待: got %q", got)
	}
}

// TestDefaultResolver_Resolve_NonGoFile は同名の .txt ファイルだけが存在する場合でも
// ErrTargetNotResolved が返ることを検証する (resolver が `<snake>.go` 以外にフォールバックしないこと、
// および .go 以外の拡張子を解決しないこと / tasks.md 4.1 / Req 7.4)。
func TestDefaultResolver_Resolve_NonGoFile(t *testing.T) {
	ctx := context.Background()

	root := t.TempDir()
	// `<snake>.txt` だけを fixture として配置する。`<snake>.go` は意図的に作らない。
	// resolver は `<snake>.go` のみを構築するため、.txt fixture の存在に引きずられて success にならないはず。
	makeWorkflowFixture(t, root, "fake_text_workflow.txt")

	resolver := NewDefaultResolver()
	got, err := resolver.Resolve(ctx, root, "fake-text-workflow")
	if err == nil {
		t.Fatalf(".txt fixture のみ存在で error を期待したが nil が返った (got path=%q)", got)
	}
	if !errors.Is(err, ErrTargetNotResolved) {
		t.Errorf(".go 以外 error は ErrTargetNotResolved を errors.Is で検出可能な形で返すべき: got %v", err)
	}
	if got != "" {
		t.Errorf("error 経路では path は空文字列を期待: got %q", got)
	}
}

// TestDefaultResolver_Resolve_RelativeCwd は相対パス cwd を渡されても結果が絶対 clean path に
// 正規化されることを検証する (Postconditions: 戻り値 path は絶対 clean path / design.md §TargetResolver)。
//
// 注: macOS では `t.TempDir()` の戻り値が `/var/folders/...` を返すが、chdir 後の `os.Getwd()` は
// symlink 解決後の `/private/var/folders/...` を返す。期待値は chdir 後の `os.Getwd()` から
// 構築することで platform-dependent な symlink を吸収する。
func TestDefaultResolver_Resolve_RelativeCwd(t *testing.T) {
	ctx := context.Background()

	root := t.TempDir()
	makeWorkflowFixture(t, root, "update_readme.go")

	// 一時ディレクトリへ chdir し、`.` を cwd として渡すことで相対パス入力をシミュレートする。
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		if cerr := os.Chdir(origWD); cerr != nil {
			t.Errorf("restore wd: %v", cerr)
		}
	})
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir to fixture root: %v", err)
	}

	// chdir 後の cwd を期待値構築の起点とする (symlink 解決済 path)。
	resolvedRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd after chdir: %v", err)
	}
	wantPath := filepath.Clean(filepath.Join(resolvedRoot, "internal", "workflow", "update_readme.go"))

	resolver := NewDefaultResolver()
	got, err := resolver.Resolve(ctx, ".", "update-readme")
	if err != nil {
		t.Fatalf("Resolve(\".\", update-readme): 予期しない error: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("相対 cwd 入力でも戻り値は絶対パスを期待: got %q", got)
	}
	if got != wantPath {
		t.Errorf("path mismatch\n  got:  %q\n  want: %q", got, wantPath)
	}
}

// TestErrTargetNotResolved_IsSentinel は ErrTargetNotResolved が exported な sentinel error として
// パッケージ外から errors.Is で検出可能であることを構造的に検証する。
// orchestration 側 (task 11.1) が `precondition_failed` verdict にマップする際の前提となる。
func TestErrTargetNotResolved_IsSentinel(t *testing.T) {
	if ErrTargetNotResolved == nil {
		t.Fatal("ErrTargetNotResolved は nil ではない sentinel error を期待")
	}
	// errors.Is 連鎖判定のために自己一致を確認する。
	if !errors.Is(ErrTargetNotResolved, ErrTargetNotResolved) {
		t.Error("ErrTargetNotResolved 自身が errors.Is で一致すべき")
	}
}
