package gh

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// installFakeGh は POSIX shell スクリプトとして偽 gh バイナリを書き出し、
// テスト実行中の PATH をその一時ディレクトリのみに置き換える。
// scriptBody は shebang を含めず、本体だけを渡す。
func installFakeGh(t *testing.T, scriptBody string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake gh は POSIX shell に依存するため Windows ではスキップ")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "gh")
	full := "#!/bin/sh\n" + scriptBody
	if err := os.WriteFile(path, []byte(full), 0o755); err != nil {
		t.Fatalf("fake gh 書き出し失敗: %v", err)
	}
	// 実 gh が混入しないように PATH を fake のディレクトリだけに制限する
	t.Setenv("PATH", dir)
	return dir
}

// ensureGhMissing は PATH を gh が存在しない一時ディレクトリのみに置き換える。
func ensureGhMissing(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir)
}

// ---------------------- CheckCLI ----------------------

func TestCheckCLI_Success(t *testing.T) {
	installFakeGh(t, `
case "$1 $2" in
  "auth status") echo "Logged in to github.com as foo"; exit 0 ;;
  *) echo "unexpected: $@" >&2; exit 99 ;;
esac
`)
	if err := CheckCLI(context.Background()); err != nil {
		t.Fatalf("CheckCLI 成功経路で nil 期待、got %v", err)
	}
}

func TestCheckCLI_CLIMissing(t *testing.T) {
	ensureGhMissing(t)
	err := CheckCLI(context.Background())
	if !errors.Is(err, ErrCLIMissing) {
		t.Fatalf("ErrCLIMissing 期待、got %v", err)
	}
}

func TestCheckCLI_Unauthorized(t *testing.T) {
	installFakeGh(t, `
case "$1 $2" in
  "auth status") echo "You are not logged into any GitHub hosts. Run gh auth login" >&2; exit 1 ;;
  *) exit 99 ;;
esac
`)
	err := CheckCLI(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("ErrUnauthorized 期待、got %v", err)
	}
}

// ---------------------- ListPRs ----------------------

func TestListPRs_Success(t *testing.T) {
	installFakeGh(t, `
case "$1 $2" in
  "pr list") echo '[{"number":42,"title":"Add feature"},{"number":7,"title":"Fix bug"}]'; exit 0 ;;
  *) echo "unexpected: $@" >&2; exit 99 ;;
esac
`)
	items, err := ListPRs(context.Background(), "", "open", 50)
	if err != nil {
		t.Fatalf("ListPRs 成功経路で err: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items 数 2 期待、got %d (%+v)", len(items), items)
	}
	if items[0].Number != 42 || items[0].Title != "Add feature" {
		t.Errorf("items[0] 不一致: %+v", items[0])
	}
	if items[1].Number != 7 || items[1].Title != "Fix bug" {
		t.Errorf("items[1] 不一致: %+v", items[1])
	}
}

func TestListPRs_PassesArgs(t *testing.T) {
	// state / limit が gh の引数に正しく渡っていることを確認するため、
	// 引数文字列を stdout に echo して検査する。
	installFakeGh(t, `
# pr list の引数構造: pr list --state <state> --limit <n> --json number,title
echo "$@" > "$RILLL_GH_ARGS_DUMP"
echo "[]"
exit 0
`)
	tmp := t.TempDir()
	dump := filepath.Join(tmp, "args.txt")
	t.Setenv("RILLL_GH_ARGS_DUMP", dump)

	if _, err := ListPRs(context.Background(), "", "all", 200); err != nil {
		t.Fatalf("ListPRs: %v", err)
	}
	got, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("引数ダンプ読み出し失敗: %v", err)
	}
	gotStr := string(got)
	if !strings.Contains(gotStr, "--state all") {
		t.Errorf("引数に --state all が含まれない: %q", gotStr)
	}
	if !strings.Contains(gotStr, "--limit 200") {
		t.Errorf("引数に --limit 200 が含まれない: %q", gotStr)
	}
	if !strings.Contains(gotStr, "--json number,title") {
		t.Errorf("引数に --json number,title が含まれない: %q", gotStr)
	}
}

func TestListPRs_CLIMissing(t *testing.T) {
	ensureGhMissing(t)
	_, err := ListPRs(context.Background(), "", "open", 50)
	if !errors.Is(err, ErrCLIMissing) {
		t.Fatalf("ErrCLIMissing 期待、got %v", err)
	}
}

func TestListPRs_Unauthorized(t *testing.T) {
	installFakeGh(t, `
echo "You are not logged into any GitHub hosts. Run gh auth login" >&2
exit 1
`)
	_, err := ListPRs(context.Background(), "", "all", 200)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("ErrUnauthorized 期待、got %v", err)
	}
}

// ---------------------- ViewPR ----------------------

func TestViewPR_Success(t *testing.T) {
	installFakeGh(t, `
case "$1 $2" in
  "pr view") echo '{"title":"My PR","body":"hello","files":[{"path":"a.go"},{"path":"sub/b.go"}],"additions":12,"deletions":3}'; exit 0 ;;
  *) exit 99 ;;
esac
`)
	meta, err := ViewPR(context.Background(), "", 42)
	if err != nil {
		t.Fatalf("ViewPR 成功経路で err: %v", err)
	}
	if meta.Number != 42 {
		t.Errorf("Number=42 期待、got %d", meta.Number)
	}
	if meta.Title != "My PR" || meta.Body != "hello" {
		t.Errorf("Title/Body 不一致: %+v", meta)
	}
	if meta.Additions != 12 || meta.Deletions != 3 {
		t.Errorf("Additions/Deletions 不一致: %+v", meta)
	}
	if len(meta.Files) != 2 || meta.Files[0] != "a.go" || meta.Files[1] != "sub/b.go" {
		t.Errorf("Files flatten 失敗: %+v", meta.Files)
	}
}

func TestViewPR_PRNotFound(t *testing.T) {
	installFakeGh(t, `
echo "GraphQL: Could not resolve to a PullRequest with the number of 999. (repository.pullRequest)" >&2
exit 1
`)
	_, err := ViewPR(context.Background(), "", 999)
	if !errors.Is(err, ErrPRNotFound) {
		t.Fatalf("ErrPRNotFound 期待、got %v", err)
	}
}

func TestViewPR_CLIMissing(t *testing.T) {
	ensureGhMissing(t)
	_, err := ViewPR(context.Background(), "", 1)
	if !errors.Is(err, ErrCLIMissing) {
		t.Fatalf("ErrCLIMissing 期待、got %v", err)
	}
}

func TestViewPR_Unauthorized(t *testing.T) {
	installFakeGh(t, `
echo "You are not logged into any GitHub hosts." >&2
exit 1
`)
	_, err := ViewPR(context.Background(), "", 1)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("ErrUnauthorized 期待、got %v", err)
	}
}

func TestViewPR_OldGhJSONUnsupported(t *testing.T) {
	// 古い gh は --json 未対応で exit 0 + warning を stderr に出すが stdout は非 JSON
	installFakeGh(t, `
echo "warning: --json is not supported on this version of gh" >&2
echo "Title: foo"
echo "Body: bar"
exit 0
`)
	_, err := ViewPR(context.Background(), "", 1)
	if !errors.Is(err, ErrCLIMissing) {
		t.Fatalf("古い gh fallback で ErrCLIMissing 派生を期待、got %v", err)
	}
}

// ---------------------- DiffPR ----------------------

func TestDiffPR_Success(t *testing.T) {
	installFakeGh(t, `
case "$1 $2" in
  "pr diff") printf 'diff --git a/a b/a\n@@ -0,0 +1,1 @@\n+added\n'; exit 0 ;;
  *) exit 99 ;;
esac
`)
	diff, err := DiffPR(context.Background(), "", 42)
	if err != nil {
		t.Fatalf("DiffPR 成功経路で err: %v", err)
	}
	if !strings.Contains(diff, "+added") {
		t.Fatalf("diff 内容不一致: %q", diff)
	}
}

func TestDiffPR_PRNotFound(t *testing.T) {
	installFakeGh(t, `
echo "could not resolve to a PullRequest with the number of 0" >&2
exit 1
`)
	_, err := DiffPR(context.Background(), "", 0)
	if !errors.Is(err, ErrPRNotFound) {
		t.Fatalf("ErrPRNotFound 期待、got %v", err)
	}
}

func TestDiffPR_CLIMissing(t *testing.T) {
	ensureGhMissing(t)
	_, err := DiffPR(context.Background(), "", 1)
	if !errors.Is(err, ErrCLIMissing) {
		t.Fatalf("ErrCLIMissing 期待、got %v", err)
	}
}

func TestDiffPR_Unauthorized(t *testing.T) {
	installFakeGh(t, `
echo "You are not logged into any GitHub hosts." >&2
exit 1
`)
	_, err := DiffPR(context.Background(), "", 1)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("ErrUnauthorized 期待、got %v", err)
	}
}

// ---------------------- ctx cancel ----------------------

func TestRun_ContextCancel(t *testing.T) {
	// 5 秒スリープする偽 gh を起動し、起動直後に ctx を cancel して即終了することを確認する。
	installFakeGh(t, `sleep 5`)
	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, err := DiffPR(ctx, "", 1)
		done <- result{err: err}
	}()
	// gh プロセスが立ち上がるまで少し待ってから cancel する
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case r := <-done:
		if r.err == nil {
			t.Fatalf("cancel 後にエラーを期待、got nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("cancel 後 3 秒以内に DiffPR が戻らなかった")
	}
}
