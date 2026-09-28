// Package improvepatch のテスト本ファイルは PromptTemplate.Render の挙動契約を
// 6 ケース (attempt=1 / attempt≥2 × claude / cursor / codex) で網羅検証する
// (tasks.md 6.1 / design.md §PromptTemplate / Resolved Decisions §4 / §6 / Req 3.2, 3.3, 4.5)。
//
// 検証観点 (tasks.md 6.1 観察可能完了条件):
//   - claude / codex 経路: JSON Schema 出力指示 (`{"diff": "..."}`) が prompt に含まれる
//   - cursor 経路: 生 text コードブロック (` ```diff ... ``` `) 指示が prompt に含まれる
//   - attempt=1: 直前提案セクションが prompt 末尾に存在しない (substring match で否定検証)
//   - attempt≥2: Resolved Decisions §4 のセクションヘッダ文と previousProposals の本文が
//     prompt 末尾に inject される (substring match で肯定検証)
//   - 共通: PromptContext.CurrentSource / TargetGoFile / TargetWorkflowID の各情報が prompt に含まれる
//
// JSON Schema 定義 (`JSONSchemaForDiff`) も同ファイルに集約されることを構造的に検証する
// (Req 3.3 / design.md §Patch Provider Compatibility Matrix)。
package patch

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

// promptSectionHeaderPreviousProposals は Resolved Decisions §4 で確定したセクションヘッダ文。
// テストは本定数を「prompt 文字列に含まれる / 含まれない」の判定に使う。
// 実装側が Render 出力に同文を埋め込む契約を機械検証する。
const promptSectionHeaderPreviousProposals = "以下は直前 N-1 試行で生成済の差分。これらと異なる方向性で改善案を生成せよ"

// makeFixturePromptContext は 6 ケース共通の PromptContext fixture を返す test helper。
// CurrentSource / TargetGoFile / TargetWorkflowID は substring match で識別可能な
// 一意のマーカー文字列を含み、Render 出力に確実に取り込まれていることを検証できる。
func makeFixturePromptContext() PromptContext {
	return PromptContext{
		TargetWorkflowID: "git-commit",
		TargetGoFile:     "/tmp/repo/internal/workflow/git_commit.go",
		CurrentSource:    "package workflow\n// FIXTURE_CURRENT_SOURCE_MARKER\n",
		Limited:          false,
	}
}

// makeFixturePreviousProposals は attempt≥2 ケース用の previousProposals fixture を返す。
// DiffBytes には substring match で識別可能な一意マーカーを含め、Render 出力に取り込まれて
// いることを検証可能にする。
func makeFixturePreviousProposals() []Patch {
	return []Patch{
		{
			TargetFiles: []string{"/tmp/repo/internal/workflow/git_commit.go"},
			DiffBytes: []byte("diff --git a/internal/workflow/git_commit.go b/internal/workflow/git_commit.go\n" +
				"index 1111..2222 100644\n" +
				"--- a/internal/workflow/git_commit.go\n" +
				"+++ b/internal/workflow/git_commit.go\n" +
				"@@ -1 +1,2 @@\n" +
				" package workflow\n" +
				"+// PREVIOUS_PROPOSAL_ATTEMPT_1_MARKER\n"),
		},
	}
}

// TestRender_Attempt1_Claude は attempt=1 の claude 経路で
// JSON Schema 出力指示が含まれ、かつ直前提案セクションが含まれないことを検証する (case 1)。
func TestRender_Attempt1_Claude(t *testing.T) {
	tmpl := NewDefaultPromptTemplate()
	got := tmpl.Render(context.Background(), makeFixturePromptContext(), 1, nil, string(provider.ProviderClaude))

	// PromptContext の各情報が prompt に含まれる (Req 3.2: 現状ソース等)
	assertPromptContainsAll(t, got, []string{
		"git-commit",
		"git_commit.go",
		"FIXTURE_CURRENT_SOURCE_MARKER",
	})

	// claude 経路: JSON Schema 出力指示が含まれる (Req 3.3)
	assertPromptContainsAll(t, got, []string{
		"JSON",
		`"diff"`,
	})

	// attempt=1: 直前提案セクションは含まれない (Resolved Decisions §4)
	if strings.Contains(got, promptSectionHeaderPreviousProposals) {
		t.Errorf("attempt=1 で直前提案セクションが含まれてはならない:\n%s", got)
	}
}

// TestRender_Attempt1_Cursor は attempt=1 の cursor 経路で
// 生 text コードブロック指示が含まれ、JSON Schema 指示が支配的でないことを検証する (case 2)。
func TestRender_Attempt1_Cursor(t *testing.T) {
	tmpl := NewDefaultPromptTemplate()
	got := tmpl.Render(context.Background(), makeFixturePromptContext(), 1, nil, string(provider.ProviderCursor))

	// PromptContext の各情報が prompt に含まれる
	assertPromptContainsAll(t, got, []string{
		"git-commit",
		"git_commit.go",
		"FIXTURE_CURRENT_SOURCE_MARKER",
	})

	// cursor 経路: ```diff コードブロック指示が含まれる (Req 3.3 / Provider Compatibility Matrix)
	assertPromptContainsAll(t, got, []string{
		"```diff",
	})

	// attempt=1: 直前提案セクションは含まれない
	if strings.Contains(got, promptSectionHeaderPreviousProposals) {
		t.Errorf("attempt=1 で直前提案セクションが含まれてはならない:\n%s", got)
	}
}

// TestRender_Attempt1_Codex は attempt=1 の codex 経路で
// JSON Schema 出力指示が含まれ (claude と同等 fallback)、直前提案セクションが含まれないことを検証する (case 3)。
func TestRender_Attempt1_Codex(t *testing.T) {
	tmpl := NewDefaultPromptTemplate()
	got := tmpl.Render(context.Background(), makeFixturePromptContext(), 1, nil, string(provider.ProviderCodex))

	// codex 経路: claude と同等の JSON Schema 出力指示 (Provider Compatibility Matrix)
	assertPromptContainsAll(t, got, []string{
		"JSON",
		`"diff"`,
	})

	// attempt=1: 直前提案セクションは含まれない
	if strings.Contains(got, promptSectionHeaderPreviousProposals) {
		t.Errorf("attempt=1 で直前提案セクションが含まれてはならない:\n%s", got)
	}
}

// TestRender_Attempt2_Claude は attempt=2 の claude 経路で
// 直前提案セクションが prompt 末尾に inject され、previousProposals 本文が含まれることを検証する (case 4)。
func TestRender_Attempt2_Claude(t *testing.T) {
	tmpl := NewDefaultPromptTemplate()
	prev := makeFixturePreviousProposals()
	got := tmpl.Render(context.Background(), makeFixturePromptContext(), 2, prev, string(provider.ProviderClaude))

	// attempt≥2: Resolved Decisions §4 のセクションヘッダ文が含まれる
	if !strings.Contains(got, promptSectionHeaderPreviousProposals) {
		t.Errorf("attempt≥2 で直前提案セクションヘッダが含まれない:\n%s", got)
	}

	// previousProposals[0].DiffBytes 内のマーカーが prompt に含まれる (substring match)
	assertPromptContainsAll(t, got, []string{
		"PREVIOUS_PROPOSAL_ATTEMPT_1_MARKER",
		"diff --git a/internal/workflow/git_commit.go",
	})

	// claude 経路: JSON Schema 出力指示が引き続き含まれる
	assertPromptContainsAll(t, got, []string{
		"JSON",
		`"diff"`,
	})
}

// TestRender_Attempt2_Cursor は attempt=2 の cursor 経路で
// 直前提案セクションが inject され、生 text コードブロック指示も維持されることを検証する (case 5)。
func TestRender_Attempt2_Cursor(t *testing.T) {
	tmpl := NewDefaultPromptTemplate()
	prev := makeFixturePreviousProposals()
	got := tmpl.Render(context.Background(), makeFixturePromptContext(), 2, prev, string(provider.ProviderCursor))

	// attempt≥2: セクションヘッダ + previousProposals 本文
	if !strings.Contains(got, promptSectionHeaderPreviousProposals) {
		t.Errorf("attempt≥2 で直前提案セクションヘッダが含まれない:\n%s", got)
	}
	assertPromptContainsAll(t, got, []string{
		"PREVIOUS_PROPOSAL_ATTEMPT_1_MARKER",
	})

	// cursor 経路: ```diff コードブロック指示は維持される
	assertPromptContainsAll(t, got, []string{
		"```diff",
	})
}

// TestRender_Attempt2_Codex は attempt=2 の codex 経路で
// 直前提案セクションが inject され、JSON Schema 指示が維持されることを検証する (case 6)。
func TestRender_Attempt2_Codex(t *testing.T) {
	tmpl := NewDefaultPromptTemplate()
	prev := makeFixturePreviousProposals()
	got := tmpl.Render(context.Background(), makeFixturePromptContext(), 2, prev, string(provider.ProviderCodex))

	if !strings.Contains(got, promptSectionHeaderPreviousProposals) {
		t.Errorf("attempt≥2 で直前提案セクションヘッダが含まれない:\n%s", got)
	}
	assertPromptContainsAll(t, got, []string{
		"PREVIOUS_PROPOSAL_ATTEMPT_1_MARKER",
		"JSON",
		`"diff"`,
	})
}

// TestRender_Attempt2_PreviousProposalsInputThinning は Issue #46 input thinning の
// 二条件 (Source-path 行 + 長文切り詰め) を検証する。
//
//   - Patch.AbsolutePath が非空のとき、各 attempt セクションに "- source: <path>" 行が含まれる
//   - DiffBytes が previousProposalDiffMaxChars (2000 rune) を超えると切り詰めマーカが含まれる
//   - AbsolutePath が空のときは Source 行が含まれない (後方互換)
func TestRender_Attempt2_PreviousProposalsInputThinning(t *testing.T) {
	const sourcePath = "/tmp/runDir/proposal-1.diff"
	// 2500 rune の長文 diff (rune 単位で 2000 を超える)。先頭マーカで先頭部分が
	// prompt に残ることも併せて確認する。
	longDiff := []byte("LONG_DIFF_HEAD_MARKER\n" + strings.Repeat("あ", 2500))

	cases := []struct {
		name        string
		previous    []Patch
		wantSubstrs []string
		notWant     []string
	}{
		{
			name: "AbsolutePath 供給 + 長文 DiffBytes → Source 行と切り詰めマーカが両方出る",
			previous: []Patch{
				{
					TargetFiles:  []string{"/tmp/repo/internal/workflow/git_commit.go"},
					DiffBytes:    longDiff,
					AbsolutePath: sourcePath,
				},
			},
			wantSubstrs: []string{
				"- source: " + sourcePath,
				"LONG_DIFF_HEAD_MARKER",
				"...(truncated; full content at Source path)",
			},
		},
		{
			name: "AbsolutePath 空 → Source 行は含まれない (後方互換)",
			previous: []Patch{
				{
					TargetFiles: []string{"/tmp/repo/internal/workflow/git_commit.go"},
					DiffBytes:   []byte("BACKWARD_COMPAT_DIFF_MARKER"),
				},
			},
			wantSubstrs: []string{
				"BACKWARD_COMPAT_DIFF_MARKER",
			},
			notWant: []string{
				"- source:",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := NewDefaultPromptTemplate()
			got := tmpl.Render(context.Background(), makeFixturePromptContext(), 2, tc.previous, string(provider.ProviderClaude))
			assertPromptContainsAll(t, got, tc.wantSubstrs)
			for _, ne := range tc.notWant {
				if strings.Contains(got, ne) {
					t.Fatalf("prompt unexpectedly contains %q\n--- prompt ---\n%s\n---", ne, got)
				}
			}
		})
	}
}

// TestRender_Attempt2_EmptyPreviousProposals は attempt=2 だが previousProposals が空配列のとき、
// セクション自体が省略されることを検証する (Resolved Decisions §4: `len(previousProposals) == 0` は省略)。
//
// 本ケースは 6 ケースの数え上げに含めない補完テストだが、attempt と previousProposals の
// 二条件のうち previousProposals 不在側で section 省略が成立することを構造的に保証する。
func TestRender_Attempt2_EmptyPreviousProposals(t *testing.T) {
	tmpl := NewDefaultPromptTemplate()
	got := tmpl.Render(context.Background(), makeFixturePromptContext(), 2, nil, string(provider.ProviderClaude))

	if strings.Contains(got, promptSectionHeaderPreviousProposals) {
		t.Errorf("previousProposals 空のとき直前提案セクションが含まれてはならない:\n%s", got)
	}
}

// TestJSONSchemaForDiff_Shape は JSON Schema 定義 (`JSONSchemaForDiff`) が
// `provider.OutputFormat{Type:"json_schema"}` 形を持ち、`diff` フィールドを必須とすることを検証する。
// claude / codex 経路で `provider.OutputFormat` として provider 層に渡せる契約を機械検証する
// (tasks.md 6.1: 「JSON Schema 形式のスキーマ定義も同ファイルに集約」観察可能完了条件)。
func TestJSONSchemaForDiff_Shape(t *testing.T) {
	if JSONSchemaForDiff == nil {
		t.Fatal("JSONSchemaForDiff は nil 不可")
	}
	if JSONSchemaForDiff.Type != "json_schema" {
		t.Errorf("Type = %q, want %q", JSONSchemaForDiff.Type, "json_schema")
	}
	// Schema は any 型なので map[string]any として cast し、構造を確認する。
	schema, ok := JSONSchemaForDiff.Schema.(map[string]any)
	if !ok {
		t.Fatalf("Schema は map[string]any であるべき: got %T", JSONSchemaForDiff.Schema)
	}
	// "type": "object" の最小構造を持つこと。
	if schema["type"] != "object" {
		t.Errorf("schema.type = %v, want %q", schema["type"], "object")
	}
	// "required" に "diff" を含むこと。
	required, ok := schema["required"].([]string)
	if !ok {
		t.Fatalf("schema.required は []string であるべき: got %T", schema["required"])
	}
	if !slices.Contains(required, "diff") {
		t.Errorf("schema.required に \"diff\" が含まれない: got %v", required)
	}
}

// TestPromptTemplate_InterfaceConformance は DefaultPromptTemplate が PromptTemplate interface を
// 静的に満たしていることを compile-time に検証する (interface 配線の構造検証)。
func TestPromptTemplate_InterfaceConformance(t *testing.T) {
	var _ PromptTemplate = (*DefaultPromptTemplate)(nil)
	// runtime 側でも nil 不可で初期化できることを確認する。
	if NewDefaultPromptTemplate() == nil {
		t.Error("NewDefaultPromptTemplate は nil ではない値を返すべき")
	}
}

// 確認用: provider 抽象が package import 可能であることを構造的に検証する。
// (本パッケージから internal/provider への片方向依存が成立していること。)
func TestRender_ProviderImportAvailable(t *testing.T) {
	// 何もしない compile-time 検証。`provider.ProviderClaude` 等が import 経由で参照可能なら通る。
	_ = provider.ProviderClaude
	_ = provider.ProviderCursor
	_ = provider.ProviderCodex
	_ = provider.ProviderCursorSDK
}

// assertPromptContainsAll は got 文字列に want substring 列が全て含まれることを検証する小ヘルパー。
// substring match による prompt 出力検証を 6 ケースで使い回す。
func assertPromptContainsAll(t *testing.T, got string, wants []string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("prompt に substring %q が含まれない:\n%s", w, got)
		}
	}
}
