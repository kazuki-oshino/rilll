// Package apply のテスト本ファイルは task 3.1 / design.md §Prompt の公開契約を
// substring match と構造検証で機械的に検証する (Req 3.1, 3.2, 3.3, 4.1, 5.4)。
//
// 検証観点 (tasks.md 3.1 観察可能完了条件 = `prompt_test.go` で 4 種 Render と 2 種 schema の
// 構造検証テストが green):
//
//   - RenderPlan / RenderDiff / RenderSelfReview / RenderCommitMessage が prompt 内に
//     reviewRunID + 対象 rule 名一覧を必須 token として含むこと (preamble 契約 / Req 3.3, 5.4)。
//   - RenderPlan / RenderDiff / RenderSelfReview は attempt 番号も prompt に含めること
//     (regenerate ループ追跡用 / Req 4.1)。RenderCommitMessage は attempt の概念を持たないため
//     attempt token 検証はスコープ外。
//   - RenderDiff / RenderSelfReview は出力 schema として package-level の
//     JSONSchemaForDiff / JSONSchemaForSelfReview をそのまま返すこと (両端契約)。
//   - JSONSchemaForDiff の Required / properties が `{diff, applied_files}` を強制すること
//     (Req 3.5 multi-file scope guard 用 self-consistency 入力)。
//   - JSONSchemaForSelfReview の Required / Enum が `{verdict ∈ {accept, regenerate}, reason}` を
//     強制すること (Req 4.1 二値判定 closed set)。
//
// 後方互換ガード:
//   - prompt 文字列の細部は substring match を要求する箇所のみが契約。セクションヘッダ等の
//     並べ替え / 文言調整は本テストでは検出しない (review/prompt_test.go の方針と整合)。
//   - JSONSchemaForDiff / JSONSchemaForSelfReview のフィールド名・enum 値は両端契約として固定。
//     値追加 / rename は破壊的変更扱い。
package apply

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/result"
)

// fixtureReviewRunID は 4 種 Render が preamble に必須 token として含むべき reviewRunID。
// substring match で識別可能なユニーク値とし、Render 出力への取り込みを機械検証する。
const fixtureReviewRunID = "review-fixture-runid-FIXTURE_REVIEW_RUN_ID_MARKER"

// fixtureRuleNameA / fixtureRuleNameB は対象 rule 名一覧の必須 token。
// preamble に rule 名が列挙されていることを substring match で検証するためのユニーク値。
const (
	fixtureRuleNameA = "security-scan-FIXTURE_RULE_A_MARKER"
	fixtureRuleNameB = "perf-check-FIXTURE_RULE_B_MARKER"
)

// makeFixtureRules は 4 種 Render 共通の `findings_reported` rule fixture (2 件) を返す。
// Name にユニークマーカーを含み、preamble の rule 名列挙が機械的に検証可能となる。
func makeFixtureRules() []result.RuleResult {
	return []result.RuleResult{
		{
			Name:            fixtureRuleNameA,
			Description:     "FIXTURE_RULE_A_DESCRIPTION セキュリティ指摘",
			Applied:         true,
			Verdict:         result.VerdictFindingsReported,
			FindingsPresent: true,
			Summary:         "FIXTURE_RULE_A_SUMMARY 認証経路に脆弱性",
			Findings: []result.Finding{
				{
					Severity: result.SeverityError,
					File:     "auth/login.go",
					Location: "auth/login.go:42",
					Message:  "FIXTURE_RULE_A_FINDING_MARKER 認証 bypass の可能性",
				},
			},
		},
		{
			Name:            fixtureRuleNameB,
			Description:     "FIXTURE_RULE_B_DESCRIPTION 性能指摘",
			Applied:         true,
			Verdict:         result.VerdictFindingsReported,
			FindingsPresent: true,
			Summary:         "FIXTURE_RULE_B_SUMMARY DB クエリ N+1",
			Findings: []result.Finding{
				{
					Severity: result.SeverityWarn,
					File:     "db/query.go",
					Location: "db/query.go:88",
					Message:  "FIXTURE_RULE_B_FINDING_MARKER N+1 クエリ",
				},
			},
		},
	}
}

// fixtureDiffBytes は RenderSelfReview / RenderDiff (attempt>=2 の previousReview) で
// 利用する unified diff fixture。substring match 用のユニークマーカーを含む。
var fixtureDiffBytes = []byte("diff --git a/auth/login.go b/auth/login.go\n" +
	"index 1111..2222 100644\n" +
	"--- a/auth/login.go\n" +
	"+++ b/auth/login.go\n" +
	"@@ -1 +1,2 @@\n" +
	" package auth\n" +
	"+// FIXTURE_DIFF_BYTES_MARKER\n")

// fixturePlanBody は RenderDiff / RenderSelfReview に渡す plan markdown fixture。
const fixturePlanBody = "# 修正計画\n\nFIXTURE_PLAN_BODY_MARKER\n- auth/login.go の認証 bypass を修正\n"

// assertPromptContainsAll は got prompt が wanted の全 substring を含むことを検証する
// テストヘルパ。改行を含む長文 prompt の差分を読みやすくするため、欠落 substring を
// 列挙してまとめて報告する。
func assertPromptContainsAll(t *testing.T, got string, wanted []string) {
	t.Helper()
	var missing []string
	for _, w := range wanted {
		if !strings.Contains(got, w) {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("prompt missing substrings: %v\n--- prompt ---\n%s\n---", missing, got)
	}
}

// TestRenderPlan_PreviousReviewInputThinning は Issue #46 input thinning の
// 二条件 (Source-path 行 + 長文切り詰め) を検証する。
//
//   - PreviousReviewPath が非空のとき prompt に "previous_reason_source: <path>" 行が含まれる
//   - PreviousReview.Reason が previousReviewReasonMaxChars (2000 rune) を超えると、
//     prompt 末尾に切り詰めマーカ (Source path 案内文字列) が含まれる
//   - PreviousReviewPath が空文字列のときは Source 行が含まれない (後方互換)
func TestRenderPlan_PreviousReviewInputThinning(t *testing.T) {
	const sourcePath = "/tmp/runDir/cycle-2-review.json"
	// 2500 rune の長文 reason (rune 単位で 2000 を超える)。fixture に含めるユニーク
	// マーカで先頭部分が prompt に残ることも併せて確認する。
	longReason := "PREV_REASON_HEAD_MARKER " + strings.Repeat("あ", 2500)

	cases := []struct {
		name        string
		input       PlanInput
		wantSubstrs []string
		notWant     []string
	}{
		{
			name: "previous reason path 供給 + 長文 reason → Source 行と切り詰めマーカが両方出る",
			input: PlanInput{
				ReviewRunID:        fixtureReviewRunID,
				Rules:              makeFixtureRules(),
				Attempt:            3,
				PreviousReviewPath: sourcePath,
				PreviousReview: &SelfReviewRecord{
					Verdict: SelfReviewRegenerate,
					Reason:  longReason,
					Attempt: 2,
				},
			},
			wantSubstrs: []string{
				"previous_reason_source: " + sourcePath,
				"PREV_REASON_HEAD_MARKER",
				"...(truncated; full content at Source path)",
			},
		},
		{
			name: "previous reason path 供給 + 短文 reason → Source 行は出るが切り詰めマーカは出ない",
			input: PlanInput{
				ReviewRunID:        fixtureReviewRunID,
				Rules:              makeFixtureRules(),
				Attempt:            2,
				PreviousReviewPath: sourcePath,
				PreviousReview: &SelfReviewRecord{
					Verdict: SelfReviewRegenerate,
					Reason:  "SHORT_REASON 2000 rune 未満なのでそのまま埋め込まれる",
					Attempt: 1,
				},
			},
			wantSubstrs: []string{
				"previous_reason_source: " + sourcePath,
				"SHORT_REASON",
			},
			notWant: []string{
				"...(truncated; full content at Source path)",
			},
		},
		{
			name: "previous reason path 空 → Source 行は含まれない (後方互換)",
			input: PlanInput{
				ReviewRunID: fixtureReviewRunID,
				Rules:       makeFixtureRules(),
				Attempt:     2,
				PreviousReview: &SelfReviewRecord{
					Verdict: SelfReviewRegenerate,
					Reason:  "BACKWARD_COMPAT_REASON",
					Attempt: 1,
				},
			},
			wantSubstrs: []string{
				"BACKWARD_COMPAT_REASON",
			},
			notWant: []string{
				"previous_reason_source:",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderPlan(tc.input)
			assertPromptContainsAll(t, got, tc.wantSubstrs)
			for _, ne := range tc.notWant {
				if strings.Contains(got, ne) {
					t.Fatalf("prompt unexpectedly contains %q\n--- prompt ---\n%s\n---", ne, got)
				}
			}
		})
	}
}

// TestRenderPlan_ContainsRequiredTokens は RenderPlan の preamble が
// reviewRunID + rule 名一覧 + attempt 番号を必須 token として含むことを検証する
// (Req 3.1, 3.3)。
func TestRenderPlan_ContainsRequiredTokens(t *testing.T) {
	cases := []struct {
		name     string
		input    PlanInput
		extra    []string // 追加で含むことを期待する substring (regenerate context 等)
		notExist []string // 含まれないことを期待する substring
	}{
		{
			name: "attempt=1 / no previous review",
			input: PlanInput{
				ReviewRunID: fixtureReviewRunID,
				Rules:       makeFixtureRules(),
				Attempt:     1,
			},
			notExist: []string{"FIXTURE_PREV_REVIEW_REASON"},
		},
		{
			name: "attempt=3 / regenerate context with previous review",
			input: PlanInput{
				ReviewRunID: fixtureReviewRunID,
				Rules:       makeFixtureRules(),
				Attempt:     3,
				PreviousReview: &SelfReviewRecord{
					Verdict: SelfReviewRegenerate,
					Reason:  "FIXTURE_PREV_REVIEW_REASON 不十分なカバレッジ",
					Attempt: 2,
				},
			},
			extra: []string{"FIXTURE_PREV_REVIEW_REASON"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderPlan(tc.input)
			required := []string{
				fixtureReviewRunID,
				fixtureRuleNameA,
				fixtureRuleNameB,
				// attempt 番号 (preamble 内に "attempt: <N>" 等で含まれる契約)
				attemptToken(tc.input.Attempt),
			}
			required = append(required, tc.extra...)
			assertPromptContainsAll(t, got, required)
			for _, ne := range tc.notExist {
				if strings.Contains(got, ne) {
					t.Fatalf("prompt unexpectedly contains %q\n--- prompt ---\n%s\n---", ne, got)
				}
			}
		})
	}
}

// TestRenderDiff_ContainsRequiredTokensAndSchema は RenderDiff の preamble に
// reviewRunID + rule 名 + attempt + plan 本文が含まれ、戻り値の schema が
// 公開 var JSONSchemaForDiff そのものであることを検証する (Req 3.2, 3.3)。
func TestRenderDiff_ContainsRequiredTokensAndSchema(t *testing.T) {
	in := DiffInput{
		ReviewRunID: fixtureReviewRunID,
		Rules:       makeFixtureRules(),
		Plan:        fixturePlanBody,
		Attempt:     2,
	}
	gotPrompt, gotSchema := RenderDiff(in)

	assertPromptContainsAll(t, gotPrompt, []string{
		fixtureReviewRunID,
		fixtureRuleNameA,
		fixtureRuleNameB,
		"FIXTURE_PLAN_BODY_MARKER",
		attemptToken(in.Attempt),
		// JSON schema 出力指示で必須となる field 名 (LLM 指示としても露出する)
		"diff",
		"applied_files",
	})

	if gotSchema != JSONSchemaForDiff {
		t.Fatalf("RenderDiff schema must be the package-level JSONSchemaForDiff: got=%p want=%p", gotSchema, JSONSchemaForDiff)
	}
}

// TestRenderSelfReview_ContainsRequiredTokensAndSchema は RenderSelfReview の preamble に
// reviewRunID + rule 名 + plan + diff bytes (substring match) が含まれ、
// 戻り値の schema が JSONSchemaForSelfReview そのものであることを検証する (Req 4.1)。
func TestRenderSelfReview_ContainsRequiredTokensAndSchema(t *testing.T) {
	in := SelfReviewInput{
		ReviewRunID:  fixtureReviewRunID,
		Rules:        makeFixtureRules(),
		Plan:         fixturePlanBody,
		DiffBytes:    fixtureDiffBytes,
		AppliedFiles: []string{"auth/login.go", "db/query.go"},
		Attempt:      2,
	}
	gotPrompt, gotSchema := RenderSelfReview(in)

	assertPromptContainsAll(t, gotPrompt, []string{
		fixtureReviewRunID,
		fixtureRuleNameA,
		fixtureRuleNameB,
		"FIXTURE_PLAN_BODY_MARKER",
		"FIXTURE_DIFF_BYTES_MARKER",
		"auth/login.go",
		"db/query.go",
		attemptToken(in.Attempt),
		// schema enum 値 (prompt 文中で利用者向けに露出する両端契約)
		"accept",
		"regenerate",
	})

	if gotSchema != JSONSchemaForSelfReview {
		t.Fatalf("RenderSelfReview schema must be the package-level JSONSchemaForSelfReview: got=%p want=%p", gotSchema, JSONSchemaForSelfReview)
	}
}

// TestRenderCommitMessage_ContainsRequiredTokens は RenderCommitMessage の preamble に
// reviewRunID + rule 名一覧 + 変更ファイル一覧が含まれることを検証する (Req 5.4)。
// commit メッセージは attempt の概念を持たないため attempt token は検証しない。
func TestRenderCommitMessage_ContainsRequiredTokens(t *testing.T) {
	in := CommitMessageInput{
		ReviewRunID:  fixtureReviewRunID,
		Rules:        makeFixtureRules(),
		AppliedFiles: []string{"auth/login.go", "db/query.go"},
	}
	got := RenderCommitMessage(in)

	assertPromptContainsAll(t, got, []string{
		fixtureReviewRunID,
		fixtureRuleNameA,
		fixtureRuleNameB,
		"auth/login.go",
		"db/query.go",
	})
}

// TestJSONSchemaForDiff_Structure は JSONSchemaForDiff の Type / Required / properties が
// `{diff: string, applied_files: []string}` を強制する両端契約を満たすことを検証する
// (Req 3.5 multi-file scope guard 用 self-consistency 入力)。
func TestJSONSchemaForDiff_Structure(t *testing.T) {
	if JSONSchemaForDiff == nil {
		t.Fatal("JSONSchemaForDiff must not be nil")
	}
	if JSONSchemaForDiff.Type != "json_schema" {
		t.Fatalf("JSONSchemaForDiff.Type = %q, want %q", JSONSchemaForDiff.Type, "json_schema")
	}
	schemaMap, ok := JSONSchemaForDiff.Schema.(map[string]any)
	if !ok {
		t.Fatalf("JSONSchemaForDiff.Schema must be map[string]any, got %T", JSONSchemaForDiff.Schema)
	}
	if got, _ := schemaMap["type"].(string); got != "object" {
		t.Fatalf("JSONSchemaForDiff.Schema[type] = %q, want %q", got, "object")
	}

	props, ok := schemaMap["properties"].(map[string]any)
	if !ok {
		t.Fatalf("JSONSchemaForDiff.Schema[properties] must be map[string]any, got %T", schemaMap["properties"])
	}

	// diff は string 必須。
	diffProp, ok := props["diff"].(map[string]any)
	if !ok {
		t.Fatalf("JSONSchemaForDiff.Schema.properties.diff must be map[string]any, got %T", props["diff"])
	}
	if got, _ := diffProp["type"].(string); got != "string" {
		t.Fatalf("JSONSchemaForDiff.Schema.properties.diff.type = %q, want %q", got, "string")
	}

	// applied_files は string 配列必須 (Req 3.5)。
	appliedProp, ok := props["applied_files"].(map[string]any)
	if !ok {
		t.Fatalf("JSONSchemaForDiff.Schema.properties.applied_files must be map[string]any, got %T", props["applied_files"])
	}
	if got, _ := appliedProp["type"].(string); got != "array" {
		t.Fatalf("JSONSchemaForDiff.Schema.properties.applied_files.type = %q, want %q", got, "array")
	}
	items, ok := appliedProp["items"].(map[string]any)
	if !ok {
		t.Fatalf("JSONSchemaForDiff.Schema.properties.applied_files.items must be map[string]any, got %T", appliedProp["items"])
	}
	if got, _ := items["type"].(string); got != "string" {
		t.Fatalf("JSONSchemaForDiff.Schema.properties.applied_files.items.type = %q, want %q", got, "string")
	}

	// required = {"diff", "applied_files"} (順序を問わない集合一致)。
	required, ok := schemaMap["required"].([]string)
	if !ok {
		t.Fatalf("JSONSchemaForDiff.Schema.required must be []string, got %T", schemaMap["required"])
	}
	want := []string{"applied_files", "diff"}
	gotSorted := append([]string(nil), required...)
	sortStrings(gotSorted)
	if !reflect.DeepEqual(gotSorted, want) {
		t.Fatalf("JSONSchemaForDiff.Schema.required = %v, want %v (order-insensitive)", required, want)
	}

	// additionalProperties: false (両端契約の輪郭保護)。
	addl, ok := schemaMap["additionalProperties"].(bool)
	if !ok || addl {
		t.Fatalf("JSONSchemaForDiff.Schema.additionalProperties must be false, got %v (ok=%v)", schemaMap["additionalProperties"], ok)
	}
}

// TestJSONSchemaForSelfReview_Structure は JSONSchemaForSelfReview の Type / Required /
// properties.verdict.enum が `{verdict ∈ {accept, regenerate}, reason: string}` を強制する
// 両端契約を満たすことを検証する (Req 4.1 二値判定 closed set)。
func TestJSONSchemaForSelfReview_Structure(t *testing.T) {
	if JSONSchemaForSelfReview == nil {
		t.Fatal("JSONSchemaForSelfReview must not be nil")
	}
	if JSONSchemaForSelfReview.Type != "json_schema" {
		t.Fatalf("JSONSchemaForSelfReview.Type = %q, want %q", JSONSchemaForSelfReview.Type, "json_schema")
	}
	schemaMap, ok := JSONSchemaForSelfReview.Schema.(map[string]any)
	if !ok {
		t.Fatalf("JSONSchemaForSelfReview.Schema must be map[string]any, got %T", JSONSchemaForSelfReview.Schema)
	}
	if got, _ := schemaMap["type"].(string); got != "object" {
		t.Fatalf("JSONSchemaForSelfReview.Schema[type] = %q, want %q", got, "object")
	}

	props, ok := schemaMap["properties"].(map[string]any)
	if !ok {
		t.Fatalf("JSONSchemaForSelfReview.Schema[properties] must be map[string]any, got %T", schemaMap["properties"])
	}

	// verdict は string + enum = {accept, regenerate} closed set。
	verdictProp, ok := props["verdict"].(map[string]any)
	if !ok {
		t.Fatalf("JSONSchemaForSelfReview.Schema.properties.verdict must be map[string]any, got %T", props["verdict"])
	}
	if got, _ := verdictProp["type"].(string); got != "string" {
		t.Fatalf("JSONSchemaForSelfReview.Schema.properties.verdict.type = %q, want %q", got, "string")
	}
	enum, ok := verdictProp["enum"].([]string)
	if !ok {
		t.Fatalf("JSONSchemaForSelfReview.Schema.properties.verdict.enum must be []string, got %T", verdictProp["enum"])
	}
	wantEnum := []string{"accept", "regenerate"}
	gotEnum := append([]string(nil), enum...)
	sortStrings(gotEnum)
	if !reflect.DeepEqual(gotEnum, wantEnum) {
		t.Fatalf("JSONSchemaForSelfReview.Schema.properties.verdict.enum = %v, want %v (order-insensitive)", enum, wantEnum)
	}

	// reason は string 必須。
	reasonProp, ok := props["reason"].(map[string]any)
	if !ok {
		t.Fatalf("JSONSchemaForSelfReview.Schema.properties.reason must be map[string]any, got %T", props["reason"])
	}
	if got, _ := reasonProp["type"].(string); got != "string" {
		t.Fatalf("JSONSchemaForSelfReview.Schema.properties.reason.type = %q, want %q", got, "string")
	}

	// required = {"verdict", "reason"} (順序を問わない集合一致)。
	required, ok := schemaMap["required"].([]string)
	if !ok {
		t.Fatalf("JSONSchemaForSelfReview.Schema.required must be []string, got %T", schemaMap["required"])
	}
	want := []string{"reason", "verdict"}
	gotSorted := append([]string(nil), required...)
	sortStrings(gotSorted)
	if !reflect.DeepEqual(gotSorted, want) {
		t.Fatalf("JSONSchemaForSelfReview.Schema.required = %v, want %v (order-insensitive)", required, want)
	}

	// additionalProperties: false (両端契約の輪郭保護)。
	addl, ok := schemaMap["additionalProperties"].(bool)
	if !ok || addl {
		t.Fatalf("JSONSchemaForSelfReview.Schema.additionalProperties must be false, got %v (ok=%v)", schemaMap["additionalProperties"], ok)
	}
}

// TestRenderDiff_ProviderOutputFormatType は RenderDiff の戻り値 schema の Type が
// `provider.OutputFormat` 経由で構造化出力経路に渡せる json_schema 型であることを念のため明示検証する。
// JSONSchemaForDiff_Structure と冗長だが、`provider` パッケージ依存の型契約 (interface 互換) を
// 別観点で固定するためのガードレール。
func TestRenderDiff_ProviderOutputFormatType(t *testing.T) {
	_, schema := RenderDiff(DiffInput{
		ReviewRunID: fixtureReviewRunID,
		Rules:       makeFixtureRules(),
		Plan:        fixturePlanBody,
		Attempt:     1,
	})
	var _ *provider.OutputFormat = schema // compile-time interface 互換 ガード
	if schema == nil {
		t.Fatal("RenderDiff returned nil schema")
	}
}

// TestRenderSelfReview_ProviderOutputFormatType は RenderSelfReview の戻り値 schema の Type が
// `provider.OutputFormat` 経由で構造化出力経路に渡せる json_schema 型であることを念のため明示検証する。
func TestRenderSelfReview_ProviderOutputFormatType(t *testing.T) {
	_, schema := RenderSelfReview(SelfReviewInput{
		ReviewRunID:  fixtureReviewRunID,
		Rules:        makeFixtureRules(),
		Plan:         fixturePlanBody,
		DiffBytes:    fixtureDiffBytes,
		AppliedFiles: []string{"auth/login.go"},
		Attempt:      1,
	})
	var _ *provider.OutputFormat = schema // compile-time interface 互換 ガード
	if schema == nil {
		t.Fatal("RenderSelfReview returned nil schema")
	}
}

// attemptToken は preamble に attempt 番号が `attempt: N` 形式で含まれることを検証するための
// 期待 substring を返す。実装側は最低でも本 token が prompt 内に出現する契約とする。
func attemptToken(n int) string {
	// strconv ではなく fmt 風の小型 helper を回避するため、簡易整数→文字列変換を行う。
	if n == 0 {
		return "attempt: 0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return "attempt: " + string(buf[i:])
}

// sortStrings は []string の昇順 in-place ソート (sort パッケージへの依存を避けたいテスト向け簡易実装)。
// テスト内で集合一致の比較を deterministic に行うために使用する。
func sortStrings(s []string) {
	// 件数が小さい (<= 4) ことを前提にした素朴な insertion sort。
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
