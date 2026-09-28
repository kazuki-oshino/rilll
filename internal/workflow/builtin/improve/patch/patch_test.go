// Package improvepatch のテスト本ファイルは Patch 型と ExtractAndGuard 関数の挙動契約を
// ゴールデン diff fixture (既存 4 ワークフローを題材) で網羅検証する
// (tasks.md 5.1 / design.md §Patch / Req 3.3, 3.4, 3.5, 5.8)。
//
// 検証観点 (tasks.md 5.1 観察可能完了条件):
//   - JSON Schema 経路 (`{"diff": "..."}`) で claude/codex 想定の応答から正常抽出 + scope match
//   - 生 text 経路 (` ```diff ... ``` `) で cursor 想定の応答から正常抽出 (JSON 失敗 → fallback)
//   - 対象ファイル外を変更しようとする diff は ErrOutOfScope sentinel で拒否
//   - 解析不能 (diff 形式を含まない応答) は ErrUnparseable sentinel で拒否
//   - 戻り値 Patch.TargetFiles は cwd 結合後の絶対 clean path、DiffBytes は git apply に渡せる生バイト列
package patch

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeWorkflowGoFixture は Patch test 用に <root>/internal/workflow/<name>.go fixture を作成する。
// resolver_test.go の同名 helper と意図は同じだが、本 test の都合で patch_test.go 内で再宣言する
// (Go の test ヘルパーは関数スコープのため重複定義不可、別 helper 名で書き分ける)。
func makeWorkflowGoFixture(t *testing.T, root string, fileName string) string {
	t.Helper()
	dir := filepath.Join(root, "internal", "workflow")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir internal/workflow: %v", err)
	}
	full := filepath.Join(dir, fileName)
	// fixture 中身は Patch.ExtractAndGuard が見ない (path 文字列のみ抽出する純関数の契約)。
	// resolver_test.go と異なり、本 test では fixture ファイル存在は必須でないが、
	// 将来 ExtractAndGuard が `os.Stat` で存在検証を追加した場合の互換性を残すため作成しておく。
	if err := os.WriteFile(full, []byte("package workflow\n"), 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", fileName, err)
	}
	return full
}

// goldenDiffUpdateReadme は update-readme.go workflow に対する unified diff のゴールデン fixture。
// 「コメント末尾に 1 行追加する」程度の最小変更を表現し、`diff --git a/<path> b/<path>` 行が
// scope guard の検出対象となる。`@@` ハンク内容は ExtractAndGuard の検証対象外で、
// 実際の適用可否は task 1.x (GitOps.ApplyCheck) に委ねられる契約 (design.md §Patch)。
const goldenDiffUpdateReadme = `diff --git a/internal/workflow/update_readme.go b/internal/workflow/update_readme.go
index 1234567..89abcde 100644
--- a/internal/workflow/update_readme.go
+++ b/internal/workflow/update_readme.go
@@ -1,3 +1,4 @@
 package workflow

+// improved by improve-workflow (golden fixture)
 // existing comment
`

// goldenDiffGitCommit は git-commit.go workflow に対する unified diff のゴールデン fixture。
// JSON 経路と生 text 経路の両方で再利用される (raw text fallback の cursor 想定 fixture)。
const goldenDiffGitCommit = `diff --git a/internal/workflow/git_commit.go b/internal/workflow/git_commit.go
index 2345678..9abcdef 100644
--- a/internal/workflow/git_commit.go
+++ b/internal/workflow/git_commit.go
@@ -1,3 +1,4 @@
 package workflow

+// improved by improve-workflow (cursor raw text path)
 // existing comment
`

// TestExtractAndGuard_JSONSchema_HappyPath は claude/codex 想定の `{"diff": "..."}` JSON 形式から
// 正常抽出 + scope match で Patch が返ることを検証する (case 1: 正常)。
//
// Provider Compatibility Matrix (design.md §Patch) における claude / codex 経路の
// 「JSON Schema 優先抽出」契約を機械検証する。
func TestExtractAndGuard_JSONSchema_HappyPath(t *testing.T) {
	root := t.TempDir()
	expectedTarget := makeWorkflowGoFixture(t, root, "update_readme.go")

	// claude/codex 想定: provider が `{"diff": "..."}` JSON 形式で応答を返す。
	// Go の json.Marshal 経由で構築し、改行・ダブルクォートのエスケープを安全に扱う。
	rawJSON, err := jsonEncodeDiff(goldenDiffUpdateReadme)
	if err != nil {
		t.Fatalf("jsonEncodeDiff: %v", err)
	}

	got, err := ExtractAndGuard(rawJSON, expectedTarget, root)
	if err != nil {
		t.Fatalf("ExtractAndGuard JSON path: 予期しない error: %v", err)
	}

	// Postconditions: TargetFiles == [expectedTarget] (絶対 clean path)
	if len(got.TargetFiles) != 1 {
		t.Fatalf("TargetFiles 長さ 1 を期待: got %d (%v)", len(got.TargetFiles), got.TargetFiles)
	}
	if got.TargetFiles[0] != filepath.Clean(expectedTarget) {
		t.Errorf("TargetFiles mismatch\n  got:  %q\n  want: %q", got.TargetFiles[0], expectedTarget)
	}

	// DiffBytes は元の diff 文字列を保持していること (`git apply` に渡せる形式)。
	// JSON 経路では `{"diff": "..."}` から取り出した文字列がそのまま DiffBytes になる契約。
	if !bytes.Contains(got.DiffBytes, []byte("diff --git a/internal/workflow/update_readme.go")) {
		t.Errorf("DiffBytes に diff --git 行が含まれない: got %q", string(got.DiffBytes))
	}
	if !bytes.Contains(got.DiffBytes, []byte("+// improved by improve-workflow (golden fixture)")) {
		t.Errorf("DiffBytes に追加行が含まれない: got %q", string(got.DiffBytes))
	}
}

// TestExtractAndGuard_RawTextFallback_HappyPath は cursor 想定の生 text コードブロック
// (` ```diff ... ``` `) からの抽出 + scope match を検証する (case 2: 不正な JSON → fallback)。
//
// Provider Compatibility Matrix の cursor (best-effort) 経路と、claude/codex 応答が
// JSON parse 失敗するケース (例: ストリーム分割や text 混入) で生 text fallback に到達する
// ことの両方を機械検証する。
func TestExtractAndGuard_RawTextFallback_HappyPath(t *testing.T) {
	root := t.TempDir()
	expectedTarget := makeWorkflowGoFixture(t, root, "git_commit.go")

	// cursor 想定: 生 text にコードブロックを含めて応答する。
	// 前後に説明文を加えることで「生 text に diff コードブロックを含む」典型ケースを再現する。
	var b strings.Builder
	b.WriteString("以下が改善 patch です:\n\n")
	b.WriteString("```diff\n")
	b.WriteString(goldenDiffGitCommit)
	b.WriteString("```\n")
	b.WriteString("\n以上です。")
	rawText := b.String()

	got, err := ExtractAndGuard(rawText, expectedTarget, root)
	if err != nil {
		t.Fatalf("ExtractAndGuard raw text path: 予期しない error: %v", err)
	}

	if len(got.TargetFiles) != 1 {
		t.Fatalf("TargetFiles 長さ 1 を期待: got %d (%v)", len(got.TargetFiles), got.TargetFiles)
	}
	if got.TargetFiles[0] != filepath.Clean(expectedTarget) {
		t.Errorf("TargetFiles mismatch\n  got:  %q\n  want: %q", got.TargetFiles[0], expectedTarget)
	}

	// 生 text 経路でも DiffBytes は コードブロック内側の生 diff バイト列を保持する。
	// バッククォート 3 文字のフェンス自体は DiffBytes に含めない (`git apply` 入力形式整合)。
	if !bytes.Contains(got.DiffBytes, []byte("diff --git a/internal/workflow/git_commit.go")) {
		t.Errorf("DiffBytes に diff --git 行が含まれない: got %q", string(got.DiffBytes))
	}
	if bytes.Contains(got.DiffBytes, []byte("```")) {
		t.Errorf("DiffBytes にコードフェンスが残存: got %q", string(got.DiffBytes))
	}
}

// TestExtractAndGuard_OutOfScope は target ファイル以外を変更しようとする diff が
// ErrOutOfScope で拒否されることを検証する (case 3: 範囲外)。
//
// 例: target が update_readme.go だが diff が git_commit.go を変更しようとする
// → Provider 別 fallback で抽出自体は成功するが、scope guard で ErrOutOfScope を返す。
func TestExtractAndGuard_OutOfScope(t *testing.T) {
	root := t.TempDir()
	// target は update_readme.go (`internal/workflow/update_readme.go`)
	expectedTarget := makeWorkflowGoFixture(t, root, "update_readme.go")
	// 範囲外 fixture (`git_commit.go`) も配置しておく (resolver の存在確認とは独立に scope guard が動くこと)
	makeWorkflowGoFixture(t, root, "git_commit.go")

	// 範囲外 diff (git_commit.go を変更しようとする) を JSON 経路で渡す。
	rawJSON, err := jsonEncodeDiff(goldenDiffGitCommit)
	if err != nil {
		t.Fatalf("jsonEncodeDiff: %v", err)
	}

	got, err := ExtractAndGuard(rawJSON, expectedTarget, root)
	if err == nil {
		t.Fatalf("範囲外 diff で error を期待したが nil が返った (got Patch=%+v)", got)
	}
	if !errors.Is(err, ErrOutOfScope) {
		t.Errorf("範囲外 error は ErrOutOfScope を errors.Is で検出可能な形で返すべき: got %v", err)
	}
}

// TestExtractAndGuard_Unparseable は unified diff 形式を含まない応答が ErrUnparseable で
// 拒否されることを検証する (case 4: 解析不能)。
//
// 「JSON にも diff キーがなく、生 text にもコードブロックがない」「あっても diff --git 行がない」
// の両ケースで ExtractAndGuard が ErrUnparseable に集約することを確認する。
func TestExtractAndGuard_Unparseable(t *testing.T) {
	root := t.TempDir()
	expectedTarget := makeWorkflowGoFixture(t, root, "update_readme.go")

	cases := []struct {
		name    string
		rawText string
	}{
		{
			name:    "plain_text_without_diff",
			rawText: "改善案を生成できませんでした。",
		},
		{
			name:    "json_without_diff_key",
			rawText: `{"answer": "改善できませんでした"}`,
		},
		{
			name: "code_block_without_diff_git_line",
			rawText: "以下です:\n\n```diff\n" +
				"--- a/foo\n+++ b/foo\n@@ -1 +1 @@\n-old\n+new\n" +
				"```\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExtractAndGuard(tc.rawText, expectedTarget, root)
			if err == nil {
				t.Fatalf("解析不能応答で error を期待したが nil が返った (got Patch=%+v)", got)
			}
			if !errors.Is(err, ErrUnparseable) {
				t.Errorf("解析不能 error は ErrUnparseable を errors.Is で検出可能な形で返すべき: got %v", err)
			}
		})
	}
}

// TestExtractAndGuard_Sentinels は ErrUnparseable / ErrOutOfScope が exported sentinel として
// パッケージ外から errors.Is で検出可能であることを構造的に検証する。
// orchestration 側 (task 11.2) が `proposal_unparseable` / `out_of_scope_diff` verdict にマップする際の前提となる。
func TestExtractAndGuard_Sentinels(t *testing.T) {
	if ErrUnparseable == nil {
		t.Error("ErrUnparseable は nil ではない sentinel error を期待")
	}
	if ErrOutOfScope == nil {
		t.Error("ErrOutOfScope は nil ではない sentinel error を期待")
	}
	if !errors.Is(ErrUnparseable, ErrUnparseable) {
		t.Error("ErrUnparseable 自身が errors.Is で一致すべき")
	}
	if !errors.Is(ErrOutOfScope, ErrOutOfScope) {
		t.Error("ErrOutOfScope 自身が errors.Is で一致すべき")
	}
}

// jsonEncodeDiff は test fixture 用に `{"diff": "<diff text>"}` JSON を構築する helper。
// `encoding/json.Marshal` を介して改行 / ダブルクォートのエスケープを正しく行う。
//
// 本 helper は test 専用 (production code では provider が JSON Schema 経由で同等の構造を返す)。
func jsonEncodeDiff(diffText string) (string, error) {
	type payload struct {
		Diff string `json:"diff"`
	}
	buf, err := json.Marshal(payload{Diff: diffText})
	if err != nil {
		return "", err
	}
	return string(buf), nil
}
