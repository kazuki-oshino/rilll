// Package apply のテスト本ファイルは task 4.1 / design.md §Patch の契約を検証する
// (Req 3.4, 3.5)。
//
// 検証観点 (tasks.md 4.1 観察可能完了条件):
//   - 正常: JSON `{diff, applied_files}` から unified diff を抽出し、self-consistency
//     (diff 内 `+++ b/<path>` 集合 == declaredFiles) と allowlist (declaredFiles ⊆ findingsFiles)
//     を通過した場合に Patch.TargetFiles (cwd 起点 relative cleaned path) と DiffBytes が返る
//   - unparseable: 入力が JSON 不正 / diff key 不在 / unified diff として解釈不能で ErrUnparseable
//   - declared と diff の path 集合不一致: declaredFiles に diff `+++ b/` 集合と異なる path が
//     含まれる (宣言外 path 混在 / 宣言された path が diff に出てこない) → ErrOutOfScope
//   - allowlist 違反: declaredFiles に findingsFiles 外 path が含まれる → ErrOutOfScope
//   - fail-closed (空集合): findingsFiles が空集合のとき無条件に ErrOutOfScope
//     (両端契約上 Finding.File 必須 = findingsFiles が空集合 = 契約違反のため)
//   - `..` 含み path: declaredFiles または diff path が cwd を抜けると ErrOutOfScope
//   - 絶対パス: declaredFiles または diff path が絶対パスで始まると ErrOutOfScope
package apply

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// makePatchAgentJSON は LLM 想定の `{"diff": "...", "applied_files": [...]}` 形式の
// raw text を組み立てる helper。json.Marshal 経由で改行・ダブルクォートのエスケープを
// 安全に扱う (improve/patch の patch_test.go と同形)。
func makePatchAgentJSON(t *testing.T, diff string, appliedFiles []string) string {
	t.Helper()
	payload := map[string]any{
		"diff":          diff,
		"applied_files": appliedFiles,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(body)
}

// goldenDiffSingleFile は単一ファイル `internal/workflow/foo.go` を変更する unified diff
// fixture。`+++ b/<path>` / `--- a/<path>` / `@@` ハンクを含む最小 unified diff として
// ExtractAndGuard の self-consistency 検証 (path 集合一致) と diff parse 可能性検証の
// 両方の期待値となる。
const goldenDiffSingleFile = `diff --git a/internal/workflow/foo.go b/internal/workflow/foo.go
index 1234567..89abcde 100644
--- a/internal/workflow/foo.go
+++ b/internal/workflow/foo.go
@@ -1,3 +1,4 @@
 package workflow

+// added by review-apply-workflow (test fixture)
 // existing comment
`

// goldenDiffTwoFiles は 2 ファイル `internal/workflow/foo.go` と `internal/workflow/bar.go`
// を変更する unified diff fixture。multi-file scope guard の正常経路を検証するための
// 最小 fixture (Req 3.5 multi-file 対応の core 動作)。
const goldenDiffTwoFiles = `diff --git a/internal/workflow/foo.go b/internal/workflow/foo.go
index 1111111..2222222 100644
--- a/internal/workflow/foo.go
+++ b/internal/workflow/foo.go
@@ -1,3 +1,4 @@
 package workflow

+// foo.go: review-apply diff
 // existing
diff --git a/internal/workflow/bar.go b/internal/workflow/bar.go
index 3333333..4444444 100644
--- a/internal/workflow/bar.go
+++ b/internal/workflow/bar.go
@@ -1,3 +1,4 @@
 package workflow

+// bar.go: review-apply diff
 // existing
`

// TestExtractAndGuard_HappyPath_SingleFile は single-file 正常経路を検証する。
//
// declaredFiles == diff `+++ b/` 集合 == {"internal/workflow/foo.go"}、
// findingsFiles も同 path を含む allowlist であるとき、Patch が返り
// TargetFiles に cleaned relative path が、DiffBytes に元 diff bytes が入ることを確認。
func TestExtractAndGuard_HappyPath_SingleFile(t *testing.T) {
	cwd := "/tmp/repo"
	files := []string{"internal/workflow/foo.go"}
	rawAgentText := makePatchAgentJSON(t, goldenDiffSingleFile, files)

	got, err := ExtractAndGuard(rawAgentText, files, files, cwd)
	if err != nil {
		t.Fatalf("ExtractAndGuard: unexpected error %v", err)
	}
	if len(got.TargetFiles) != 1 || got.TargetFiles[0] != "internal/workflow/foo.go" {
		t.Errorf("TargetFiles = %v, want [internal/workflow/foo.go]", got.TargetFiles)
	}
	if !strings.Contains(string(got.DiffBytes), "+++ b/internal/workflow/foo.go") {
		t.Errorf("DiffBytes does not contain expected `+++ b/` line; got %q", string(got.DiffBytes))
	}
}

// TestExtractAndGuard_HappyPath_MultiFile は multi-file 正常経路を検証する
// (Req 3.5 multi-file scope guard core 動作)。
//
// declaredFiles == diff `+++ b/` 集合 == {"foo.go", "bar.go"}、
// findingsFiles に両 path が含まれる allowlist のとき、TargetFiles に両 path が
// declaredFiles の順序で入ることを確認 (重複除去・宣言順保持の挙動)。
func TestExtractAndGuard_HappyPath_MultiFile(t *testing.T) {
	cwd := "/tmp/repo"
	declared := []string{"internal/workflow/foo.go", "internal/workflow/bar.go"}
	findings := []string{"internal/workflow/foo.go", "internal/workflow/bar.go", "other.go"}
	rawAgentText := makePatchAgentJSON(t, goldenDiffTwoFiles, declared)

	got, err := ExtractAndGuard(rawAgentText, declared, findings, cwd)
	if err != nil {
		t.Fatalf("ExtractAndGuard: unexpected error %v", err)
	}
	if len(got.TargetFiles) != 2 {
		t.Fatalf("TargetFiles len = %d, want 2 (got %v)", len(got.TargetFiles), got.TargetFiles)
	}
	// declaredFiles 順序保持を検証 (foo.go → bar.go)。
	if got.TargetFiles[0] != "internal/workflow/foo.go" || got.TargetFiles[1] != "internal/workflow/bar.go" {
		t.Errorf("TargetFiles order = %v, want [foo.go, bar.go]", got.TargetFiles)
	}
}

// TestExtractAndGuard_Unparseable_InvalidJSON は rawAgentText が JSON ではない
// 場合に ErrUnparseable が返ることを検証する。
func TestExtractAndGuard_Unparseable_InvalidJSON(t *testing.T) {
	cwd := "/tmp/repo"
	rawAgentText := "not a JSON at all"

	_, err := ExtractAndGuard(rawAgentText, []string{"foo.go"}, []string{"foo.go"}, cwd)
	if !errors.Is(err, ErrUnparseable) {
		t.Errorf("err = %v, want ErrUnparseable", err)
	}
}

// TestExtractAndGuard_Unparseable_EmptyDiff は JSON は valid だが diff field が
// 空文字列のとき ErrUnparseable が返ることを検証する。
func TestExtractAndGuard_Unparseable_EmptyDiff(t *testing.T) {
	cwd := "/tmp/repo"
	rawAgentText := makePatchAgentJSON(t, "", []string{"foo.go"})

	_, err := ExtractAndGuard(rawAgentText, []string{"foo.go"}, []string{"foo.go"}, cwd)
	if !errors.Is(err, ErrUnparseable) {
		t.Errorf("err = %v, want ErrUnparseable", err)
	}
}

// TestExtractAndGuard_Unparseable_NotUnifiedDiff は diff field に unified diff
// 形式でない文字列が入っている (`+++ b/` 行が無い) 場合に ErrUnparseable が返ることを検証する。
func TestExtractAndGuard_Unparseable_NotUnifiedDiff(t *testing.T) {
	cwd := "/tmp/repo"
	rawAgentText := makePatchAgentJSON(t, "this is some text but not a unified diff", []string{"foo.go"})

	_, err := ExtractAndGuard(rawAgentText, []string{"foo.go"}, []string{"foo.go"}, cwd)
	if !errors.Is(err, ErrUnparseable) {
		t.Errorf("err = %v, want ErrUnparseable", err)
	}
}

// TestExtractAndGuard_Unparseable_NoHunk は `+++ b/` 行はあるが `@@` ハンクが
// 全く無い (= 実際には unified diff として apply 不能) 場合に ErrUnparseable が
// 返ることを検証する。
func TestExtractAndGuard_Unparseable_NoHunk(t *testing.T) {
	cwd := "/tmp/repo"
	noHunk := `diff --git a/foo.go b/foo.go
--- a/foo.go
+++ b/foo.go
`
	rawAgentText := makePatchAgentJSON(t, noHunk, []string{"foo.go"})

	_, err := ExtractAndGuard(rawAgentText, []string{"foo.go"}, []string{"foo.go"}, cwd)
	if !errors.Is(err, ErrUnparseable) {
		t.Errorf("err = %v, want ErrUnparseable", err)
	}
}

// TestExtractAndGuard_OutOfScope_DiffPathNotInDeclared は diff `+++ b/` 集合に
// declaredFiles 外の path が含まれる (= LLM が宣言していない path を diff に
// 紛れ込ませた) 場合に ErrOutOfScope が返ることを検証する (self-consistency 違反、
// Req 3.5 第一関門)。
func TestExtractAndGuard_OutOfScope_DiffPathNotInDeclared(t *testing.T) {
	cwd := "/tmp/repo"
	// diff は foo.go と bar.go を変更するが declaredFiles は foo.go のみ。
	rawAgentText := makePatchAgentJSON(t, goldenDiffTwoFiles, []string{"internal/workflow/foo.go"})

	_, err := ExtractAndGuard(rawAgentText, []string{"internal/workflow/foo.go"},
		[]string{"internal/workflow/foo.go", "internal/workflow/bar.go"}, cwd)
	if !errors.Is(err, ErrOutOfScope) {
		t.Errorf("err = %v, want ErrOutOfScope", err)
	}
}

// TestExtractAndGuard_OutOfScope_DeclaredNotInDiff は declaredFiles に diff には
// 出てこない path が含まれる (= LLM が宣言だけして diff には反映しなかった) 場合に
// ErrOutOfScope が返ることを検証する (self-consistency 違反、Req 3.5 第一関門)。
func TestExtractAndGuard_OutOfScope_DeclaredNotInDiff(t *testing.T) {
	cwd := "/tmp/repo"
	// diff は foo.go のみ。declaredFiles は foo.go + bar.go を主張。
	rawAgentText := makePatchAgentJSON(t, goldenDiffSingleFile,
		[]string{"internal/workflow/foo.go", "internal/workflow/bar.go"})

	_, err := ExtractAndGuard(rawAgentText,
		[]string{"internal/workflow/foo.go", "internal/workflow/bar.go"},
		[]string{"internal/workflow/foo.go", "internal/workflow/bar.go"}, cwd)
	if !errors.Is(err, ErrOutOfScope) {
		t.Errorf("err = %v, want ErrOutOfScope", err)
	}
}

// TestExtractAndGuard_OutOfScope_AllowlistViolation は declaredFiles に
// findingsFiles 外の path が含まれる (= LLM が findings 由来でない file を変更しようとする)
// 場合に ErrOutOfScope が返ることを検証する (Req 3.5 第二関門)。
func TestExtractAndGuard_OutOfScope_AllowlistViolation(t *testing.T) {
	cwd := "/tmp/repo"
	declared := []string{"internal/workflow/foo.go"}
	findings := []string{"internal/workflow/bar.go"} // foo.go は findings 集合外
	rawAgentText := makePatchAgentJSON(t, goldenDiffSingleFile, declared)

	_, err := ExtractAndGuard(rawAgentText, declared, findings, cwd)
	if !errors.Is(err, ErrOutOfScope) {
		t.Errorf("err = %v, want ErrOutOfScope", err)
	}
}

// TestExtractAndGuard_OutOfScope_FailClosedEmptyFindings は findingsFiles が
// 空集合のとき無条件に ErrOutOfScope が返ることを検証する
// (両端契約上 Finding.File 必須 → 空集合は契約違反 → fail-closed; design.md §Patch L640)。
func TestExtractAndGuard_OutOfScope_FailClosedEmptyFindings(t *testing.T) {
	cwd := "/tmp/repo"
	declared := []string{"internal/workflow/foo.go"}
	rawAgentText := makePatchAgentJSON(t, goldenDiffSingleFile, declared)

	_, err := ExtractAndGuard(rawAgentText, declared, []string{}, cwd)
	if !errors.Is(err, ErrOutOfScope) {
		t.Errorf("err = %v, want ErrOutOfScope (fail-closed for empty findingsFiles)", err)
	}

	// nil findingsFiles も同様に fail-closed であることを検証 (空集合と意味的に等価)。
	_, err2 := ExtractAndGuard(rawAgentText, declared, nil, cwd)
	if !errors.Is(err2, ErrOutOfScope) {
		t.Errorf("err = %v, want ErrOutOfScope (fail-closed for nil findingsFiles)", err2)
	}
}

// TestExtractAndGuard_OutOfScope_DotDotInDeclared は declaredFiles に `..` を含む
// 相対パス (cwd を抜ける) が指定された場合に ErrOutOfScope が返ることを検証する。
func TestExtractAndGuard_OutOfScope_DotDotInDeclared(t *testing.T) {
	cwd := "/tmp/repo"
	declared := []string{"../escape.go"}
	findings := []string{"../escape.go"} // findings に同 path を入れて allowlist を通過させる
	// diff も `+++ b/../escape.go` を含むよう構築 (path validation だけが原因で reject されることを確認)。
	diff := `diff --git a/../escape.go b/../escape.go
index 1111111..2222222 100644
--- a/../escape.go
+++ b/../escape.go
@@ -1,3 +1,4 @@
 package x

+// escape attempt
 // existing
`
	rawAgentText := makePatchAgentJSON(t, diff, declared)

	_, err := ExtractAndGuard(rawAgentText, declared, findings, cwd)
	if !errors.Is(err, ErrOutOfScope) {
		t.Errorf("err = %v, want ErrOutOfScope (`..` in declared path)", err)
	}
}

// TestExtractAndGuard_OutOfScope_AbsolutePathInDeclared は declaredFiles に
// 絶対パスが指定された場合に ErrOutOfScope が返ることを検証する。
func TestExtractAndGuard_OutOfScope_AbsolutePathInDeclared(t *testing.T) {
	cwd := "/tmp/repo"
	declared := []string{"/etc/passwd"}
	findings := []string{"/etc/passwd"} // findings に同 path を入れても absolute であれば reject
	diff := `diff --git a//etc/passwd b//etc/passwd
index 1111111..2222222 100644
--- a//etc/passwd
+++ b//etc/passwd
@@ -1,3 +1,4 @@
 root:x:0:0
+evil:x:0:0
 daemon:x:1:1
`
	rawAgentText := makePatchAgentJSON(t, diff, declared)

	_, err := ExtractAndGuard(rawAgentText, declared, findings, cwd)
	if !errors.Is(err, ErrOutOfScope) {
		t.Errorf("err = %v, want ErrOutOfScope (absolute path in declared)", err)
	}
}
