// Package review のテスト本ファイルは BuildRulePrompt と JSONSchemaForReview の
// 公開契約 (tasks.md 2.1 / design.md §review.Prompt / Req 2.5, 5.1, 5.3, 5.4) を
// substring match と構造検証で機械的に検証する。
//
// 検証観点 (tasks.md 2.1 観察可能完了条件):
//   - applies_when 自然文と review prompt 本文が prompt に含まれる
//   - PR メタデータ (title / body / files / additions / deletions) が prompt に含まれる
//   - PR diff が prompt に含まれる
//   - prompt 末尾に JSON schema 指示 (applied / skip_reason / findings_present /
//     summary / details) が含まれる
//   - JSONSchemaForReview の Type / Schema 構造が design.md の両端契約と一致する
package review

import (
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/gh"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/rules"
)

// makeFixtureRule は BuildRulePrompt 検証用の Rule fixture を返す。
// AppliesWhen / Prompt / Description / Name には substring match 用のユニーク
// マーカー文字列を含めて、Render 出力に確実に取り込まれていることを検証する。
func makeFixtureRule() rules.Rule {
	return rules.Rule{
		Name:        "security-scan",
		Description: "FIXTURE_RULE_DESCRIPTION_MARKER security review",
		Provider:    "claude",
		Model:       "claude-sonnet-4-5",
		AppliesWhen: "FIXTURE_APPLIES_WHEN_MARKER この PR が Go コードを含むとき。",
		Prompt:      "FIXTURE_REVIEW_PROMPT_MARKER 認証経路に対する脆弱性指摘を出してください。",
		SourceFile:  "security-scan.yaml",
	}
}

// makeFixturePRMeta は BuildRulePrompt 検証用の PRMeta fixture を返す。
// 各フィールドに substring match 用のユニークマーカーを含む。
func makeFixturePRMeta() gh.PRMeta {
	return gh.PRMeta{
		Number:    42,
		Title:     "FIXTURE_PR_TITLE_MARKER feat: add auth flow",
		Body:      "FIXTURE_PR_BODY_MARKER PR description body",
		Files:     []string{"auth/login.go", "auth/login_test.go"},
		Additions: 120,
		Deletions: 8,
	}
}

// fixtureDiff は BuildRulePrompt 検証用の diff fixture を返す。
const fixtureDiff = "diff --git a/auth/login.go b/auth/login.go\n" +
	"index 1111..2222 100644\n" +
	"--- a/auth/login.go\n" +
	"+++ b/auth/login.go\n" +
	"@@ -1 +1,2 @@\n" +
	" package auth\n" +
	"+// FIXTURE_DIFF_MARKER\n"

// TestBuildRulePrompt_IncludesRuleAndPRContext は applies_when / prompt / PR meta /
// diff の各情報が必ず prompt に含まれることを substring match で検証する
// (Req 2.5 / 5.1 / 5.3)。
func TestBuildRulePrompt_IncludesRuleAndPRContext(t *testing.T) {
	got := BuildRulePrompt(makeFixtureRule(), makeFixturePRMeta(), fixtureDiff)

	requiredSubstrings := []string{
		// rule metadata
		"security-scan",
		"FIXTURE_RULE_DESCRIPTION_MARKER",
		// applies_when
		"FIXTURE_APPLIES_WHEN_MARKER",
		// review prompt
		"FIXTURE_REVIEW_PROMPT_MARKER",
		// PR メタデータ
		"FIXTURE_PR_TITLE_MARKER",
		"FIXTURE_PR_BODY_MARKER",
		"auth/login.go",
		"auth/login_test.go",
		"120",
		"8",
		// PR diff
		"FIXTURE_DIFF_MARKER",
	}
	for _, s := range requiredSubstrings {
		if !strings.Contains(got, s) {
			t.Errorf("BuildRulePrompt 出力に %q が含まれていない\n--- prompt ---\n%s", s, got)
		}
	}
}

// TestBuildRulePrompt_EndsWithJSONSchemaInstructions は prompt 末尾に
// JSON schema 出力指示 (applied / skip_reason / findings_present / summary /
// details / severity の closed set) が含まれることを検証する
// (design.md §review.Prompt / §review.Parser, Req 5.4)。
func TestBuildRulePrompt_EndsWithJSONSchemaInstructions(t *testing.T) {
	got := BuildRulePrompt(makeFixtureRule(), makeFixturePRMeta(), fixtureDiff)

	requiredSubstrings := []string{
		// 両端契約 schema フィールド名
		`"applied"`,
		`"skip_reason"`,
		`"findings_present"`,
		`"summary"`,
		`"details"`,
		`"severity"`,
		`"file"`,
		`"location"`,
		`"message"`,
		// closed set severity
		"info",
		"warn",
		"error",
		// cursor 経路向け明示指示 (両分岐の例示)
		`"applied": false`,
		`"applied": true`,
	}
	for _, s := range requiredSubstrings {
		if !strings.Contains(got, s) {
			t.Errorf("JSON schema 指示に %q が含まれていない\n--- prompt ---\n%s", s, got)
		}
	}

	// 出力形式指示は prompt 末尾セクションに位置する契約 (design.md §review.Prompt L515)。
	// PR diff の後ろに schema 指示が来ていることを順序検証する。
	diffIdx := strings.Index(got, "FIXTURE_DIFF_MARKER")
	schemaIdx := strings.Index(got, `"applied"`)
	if diffIdx < 0 || schemaIdx < 0 {
		t.Fatalf("prompt から diff / schema 指示の位置が特定できない: diffIdx=%d schemaIdx=%d", diffIdx, schemaIdx)
	}
	if schemaIdx <= diffIdx {
		t.Errorf("JSON schema 指示が PR diff より前に配置されている (末尾配置契約違反): diffIdx=%d schemaIdx=%d", diffIdx, schemaIdx)
	}
}

// TestBuildRulePrompt_HandlesEmptyPRBodyAndFiles は PR body 空 / files 空のときも
// テンプレートが panic せず、いずれかの placeholder を出力することを検証する。
func TestBuildRulePrompt_HandlesEmptyPRBodyAndFiles(t *testing.T) {
	rule := makeFixtureRule()
	prMeta := gh.PRMeta{
		Number:    1,
		Title:     "empty body PR",
		Body:      "",
		Files:     nil,
		Additions: 0,
		Deletions: 0,
	}
	got := BuildRulePrompt(rule, prMeta, "")

	if !strings.Contains(got, "empty body PR") {
		t.Errorf("PR title が prompt に含まれていない")
	}
	// 空配列・空文字列でも prompt 自体は組み立てられること (panic しない)
	if got == "" {
		t.Errorf("BuildRulePrompt が空文字列を返した")
	}
}

// TestJSONSchemaForReview_Structure は JSONSchemaForReview の Type / Schema が
// 両端契約 (design.md §review.Parser) どおりのフィールドセットを持つことを検証する。
func TestJSONSchemaForReview_Structure(t *testing.T) {
	if JSONSchemaForReview == nil {
		t.Fatal("JSONSchemaForReview が nil")
	}
	if JSONSchemaForReview.Type != "json_schema" {
		t.Errorf("JSONSchemaForReview.Type = %q want %q", JSONSchemaForReview.Type, "json_schema")
	}

	schema, ok := JSONSchemaForReview.Schema.(map[string]any)
	if !ok {
		t.Fatalf("JSONSchemaForReview.Schema は map[string]any 期待: got %T", JSONSchemaForReview.Schema)
	}
	if schema["type"] != "object" {
		t.Errorf("schema.type = %v want %q", schema["type"], "object")
	}

	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema.properties は map[string]any 期待: got %T", schema["properties"])
	}

	wantTopLevel := []string{"applied", "skip_reason", "findings_present", "summary", "details"}
	for _, k := range wantTopLevel {
		if _, ok := props[k]; !ok {
			t.Errorf("schema.properties に %q が無い", k)
		}
	}

	// details は array で items が severity / location / message を持つ
	details, ok := props["details"].(map[string]any)
	if !ok {
		t.Fatalf("schema.properties.details は map[string]any 期待: got %T", props["details"])
	}
	if details["type"] != "array" {
		t.Errorf("schema.properties.details.type = %v want %q", details["type"], "array")
	}
	items, ok := details["items"].(map[string]any)
	if !ok {
		t.Fatalf("details.items は map[string]any 期待: got %T", details["items"])
	}
	itemProps, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("details.items.properties は map[string]any 期待: got %T", items["properties"])
	}
	for _, k := range []string{"severity", "file", "location", "message"} {
		if _, ok := itemProps[k]; !ok {
			t.Errorf("details.items.properties に %q が無い", k)
		}
	}

	// items.required に "file" が含まれること (両端契約必須化)。
	itemRequiredRaw, ok := items["required"]
	if !ok {
		t.Fatal("details.items.required が定義されていない")
	}
	itemRequired, ok := itemRequiredRaw.([]string)
	if !ok {
		t.Fatalf("details.items.required は []string 期待: got %T", itemRequiredRaw)
	}
	wantItemRequired := map[string]bool{"severity": false, "file": false, "message": false}
	for _, v := range itemRequired {
		if _, known := wantItemRequired[v]; known {
			wantItemRequired[v] = true
		}
	}
	for v, found := range wantItemRequired {
		if !found {
			t.Errorf("details.items.required に %q が含まれていない", v)
		}
	}
	// severity は info / warn / error の closed set を enum 表現する
	severity, ok := itemProps["severity"].(map[string]any)
	if !ok {
		t.Fatalf("details.items.properties.severity は map[string]any 期待: got %T", itemProps["severity"])
	}
	enumRaw, ok := severity["enum"]
	if !ok {
		t.Fatal("severity.enum が定義されていない")
	}
	enum, ok := enumRaw.([]string)
	if !ok {
		t.Fatalf("severity.enum は []string 期待: got %T", enumRaw)
	}
	wantEnum := map[string]bool{"info": false, "warn": false, "error": false}
	for _, v := range enum {
		if _, known := wantEnum[v]; known {
			wantEnum[v] = true
		} else {
			t.Errorf("severity.enum に未知の値 %q が含まれる", v)
		}
	}
	for v, found := range wantEnum {
		if !found {
			t.Errorf("severity.enum に %q が含まれていない", v)
		}
	}
}
