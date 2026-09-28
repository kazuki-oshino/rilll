// Package review のテスト本ファイルは ParseRuleResponse の公開契約 (tasks.md 2.2 /
// design.md §review.Parser / Req 5.2, 5.4) をテーブル駆動で機械的に検証する。
//
// 検証観点 (tasks.md 2.2 観察可能完了条件):
//   - JSON schema 適合経路: StructuredOutput が schema どおりに decode できる場合は
//     直接デコード経路で Verdict / FindingsPresent / FailureCategory が期待通り
//   - fallback regex 経路: StructuredOutput が nil / 不正な場合でも Content から
//     greedy regex で JSON オブジェクトを抽出して decode 経路と同等の結果を得る
//   - 完全な散文経路: JSON が一切抽出できない場合に Verdict=failed +
//     FailureCategory="response_unparseable" になる
//   - severity 不正経路: closed set (info/warn/error) 違反を検知して
//     Verdict=failed + FailureCategory="response_unparseable" にする
//   - 必須フィールド検証: applied=false のとき skip_reason 空 / applied=true のとき
//     summary 空は同じく failed 扱い
package review

import (
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/result"
)

// TestParseRuleResponse_StructuredOutputApplied は applied=true + findings あり経路の
// StructuredOutput direct decode を検証する (Req 5.2, 5.4)。
func TestParseRuleResponse_StructuredOutputApplied(t *testing.T) {
	// agent SDK 由来の StructuredOutput は any 型の任意 JSON 互換構造。bridge
	// 実装によっては map[string]any / 構造化された型が来るため map 形を fixture とする。
	resp := provider.AgentResponse{
		Success: true,
		Content: "本文 (StructuredOutput を使うので無視されるべき)",
		StructuredOutput: map[string]any{
			"applied":          true,
			"skip_reason":      "",
			"findings_present": true,
			"summary":          "認証経路に潜在的な脆弱性が 1 件あり",
			"details": []any{
				map[string]any{
					"severity": "warn",
					"file":     "auth/login.go",
					"location": "auth/login.go:42",
					"message":  "input validation がない",
				},
			},
		},
	}

	got, err := ParseRuleResponse(resp)
	if err != nil {
		t.Fatalf("ParseRuleResponse() unexpected error: %v", err)
	}
	if !got.Applied {
		t.Errorf("Applied = false, want true")
	}
	if got.Verdict != result.VerdictFindingsReported {
		t.Errorf("Verdict = %q, want %q", got.Verdict, result.VerdictFindingsReported)
	}
	if !got.FindingsPresent {
		t.Errorf("FindingsPresent = false, want true")
	}
	if got.Summary == "" {
		t.Errorf("Summary should be non-empty")
	}
	if got.FailureCategory != "" {
		t.Errorf("FailureCategory = %q, want empty", got.FailureCategory)
	}
	if len(got.Findings) != 1 {
		t.Fatalf("len(Findings) = %d, want 1", len(got.Findings))
	}
	if got.Findings[0].Severity != result.SeverityWarn {
		t.Errorf("Findings[0].Severity = %q, want %q", got.Findings[0].Severity, result.SeverityWarn)
	}
	if got.Findings[0].File != "auth/login.go" {
		t.Errorf("Findings[0].File = %q, want %q", got.Findings[0].File, "auth/login.go")
	}
	if got.Findings[0].Location != "auth/login.go:42" {
		t.Errorf("Findings[0].Location = %q", got.Findings[0].Location)
	}
}

// TestParseRuleResponse_StructuredOutputNoFindings は applied=true + findings なし経路を検証する。
func TestParseRuleResponse_StructuredOutputNoFindings(t *testing.T) {
	resp := provider.AgentResponse{
		StructuredOutput: map[string]any{
			"applied":          true,
			"findings_present": false,
			"summary":          "問題なし",
			"details":          []any{},
		},
	}

	got, err := ParseRuleResponse(resp)
	if err != nil {
		t.Fatalf("ParseRuleResponse() unexpected error: %v", err)
	}
	if got.Verdict != result.VerdictNoFindings {
		t.Errorf("Verdict = %q, want %q", got.Verdict, result.VerdictNoFindings)
	}
	if got.FindingsPresent {
		t.Errorf("FindingsPresent = true, want false")
	}
	if got.FailureCategory != "" {
		t.Errorf("FailureCategory = %q, want empty", got.FailureCategory)
	}
}

// TestParseRuleResponse_StructuredOutputSkipped は applied=false (skip) 経路を検証する (Req 5.2)。
func TestParseRuleResponse_StructuredOutputSkipped(t *testing.T) {
	resp := provider.AgentResponse{
		StructuredOutput: map[string]any{
			"applied":     false,
			"skip_reason": "Go ソースを含まないため適用外",
		},
	}

	got, err := ParseRuleResponse(resp)
	if err != nil {
		t.Fatalf("ParseRuleResponse() unexpected error: %v", err)
	}
	if got.Applied {
		t.Errorf("Applied = true, want false")
	}
	if got.Verdict != result.VerdictSkipped {
		t.Errorf("Verdict = %q, want %q", got.Verdict, result.VerdictSkipped)
	}
	if got.SkipReason == "" {
		t.Errorf("SkipReason should be non-empty")
	}
	if got.FailureCategory != "" {
		t.Errorf("FailureCategory = %q, want empty", got.FailureCategory)
	}
}

// TestParseRuleResponse_FallbackRegexApplied は StructuredOutput が nil で Content
// 内に余分な散文 + JSON オブジェクトが混在するとき、greedy regex で JSON を
// 抽出してデコードする fallback 経路を検証する (Req 5.4)。
func TestParseRuleResponse_FallbackRegexApplied(t *testing.T) {
	resp := provider.AgentResponse{
		Content: "解析結果は以下の通りです:\n" +
			"```json\n" +
			`{"applied": true, "findings_present": true, "summary": "1 件の指摘", "details": [{"severity": "error", "file": "main.go", "message": "致命的エラー"}]}` + "\n" +
			"```\n" +
			"以上です。",
	}

	got, err := ParseRuleResponse(resp)
	if err != nil {
		t.Fatalf("ParseRuleResponse() unexpected error: %v", err)
	}
	if got.Verdict != result.VerdictFindingsReported {
		t.Errorf("Verdict = %q, want %q", got.Verdict, result.VerdictFindingsReported)
	}
	if !got.FindingsPresent {
		t.Errorf("FindingsPresent = false, want true")
	}
	if got.FailureCategory != "" {
		t.Errorf("FailureCategory = %q, want empty", got.FailureCategory)
	}
}

// TestParseRuleResponse_FallbackRegexSkipped は applied=false の fallback regex 経路を検証する。
func TestParseRuleResponse_FallbackRegexSkipped(t *testing.T) {
	resp := provider.AgentResponse{
		Content: `この PR には Go コードが含まれません。{"applied": false, "skip_reason": "Go コードを含まないため"}`,
	}

	got, err := ParseRuleResponse(resp)
	if err != nil {
		t.Fatalf("ParseRuleResponse() unexpected error: %v", err)
	}
	if got.Verdict != result.VerdictSkipped {
		t.Errorf("Verdict = %q, want %q", got.Verdict, result.VerdictSkipped)
	}
	if !strings.Contains(got.SkipReason, "Go コード") {
		t.Errorf("SkipReason = %q, want substring 'Go コード'", got.SkipReason)
	}
}

// TestParseRuleResponse_StructuredOutputFallsBackToContent は StructuredOutput の
// 形がスキーマ違反 (applied フィールド欠落) のとき、Content からの regex 抽出に
// フォールバックする経路を検証する。
func TestParseRuleResponse_StructuredOutputFallsBackToContent(t *testing.T) {
	resp := provider.AgentResponse{
		// StructuredOutput が JSON schema に従わない (applied 欠落) ケース。
		StructuredOutput: map[string]any{
			"unrelated": "noise",
		},
		Content: `{"applied": true, "findings_present": false, "summary": "OK"}`,
	}

	got, err := ParseRuleResponse(resp)
	if err != nil {
		t.Fatalf("ParseRuleResponse() unexpected error: %v", err)
	}
	if got.Verdict != result.VerdictNoFindings {
		t.Errorf("Verdict = %q, want %q", got.Verdict, result.VerdictNoFindings)
	}
}

// TestParseRuleResponse_FullProse は Content が完全な自由文で JSON が抽出できない
// ケースで Verdict=failed + FailureCategory="response_unparseable" になることを
// 検証する (Req 5.4 / design.md §review.Parser)。
func TestParseRuleResponse_FullProse(t *testing.T) {
	resp := provider.AgentResponse{
		Content: "この PR は素晴らしいと思います。特に問題は見当たりません。",
	}

	got, err := ParseRuleResponse(resp)
	if err == nil {
		t.Errorf("ParseRuleResponse() error = nil, want non-nil")
	}
	if got.Verdict != result.VerdictFailed {
		t.Errorf("Verdict = %q, want %q", got.Verdict, result.VerdictFailed)
	}
	if got.FailureCategory != "response_unparseable" {
		t.Errorf("FailureCategory = %q, want %q", got.FailureCategory, "response_unparseable")
	}
}

// TestParseRuleResponse_InvalidSeverity は details[].severity が closed set 違反の
// ケースで Verdict=failed + FailureCategory="response_unparseable" になることを
// 検証する (design.md §review.Parser Validation)。
func TestParseRuleResponse_InvalidSeverity(t *testing.T) {
	resp := provider.AgentResponse{
		Content: `{"applied": true, "findings_present": true, "summary": "x", "details": [{"severity": "critical", "file": "x.go", "message": "y"}]}`,
	}

	got, err := ParseRuleResponse(resp)
	if err == nil {
		t.Errorf("ParseRuleResponse() error = nil, want non-nil")
	}
	if got.Verdict != result.VerdictFailed {
		t.Errorf("Verdict = %q, want %q", got.Verdict, result.VerdictFailed)
	}
	if got.FailureCategory != "response_unparseable" {
		t.Errorf("FailureCategory = %q, want %q", got.FailureCategory, "response_unparseable")
	}
}

// TestParseRuleResponse_AppliedTrueMissingSummary は applied=true で summary 欠落
// (空文字) の必須フィールド検証違反を failed 扱いにすることを検証する。
func TestParseRuleResponse_AppliedTrueMissingSummary(t *testing.T) {
	resp := provider.AgentResponse{
		Content: `{"applied": true, "findings_present": false, "summary": ""}`,
	}

	got, err := ParseRuleResponse(resp)
	if err == nil {
		t.Errorf("ParseRuleResponse() error = nil, want non-nil")
	}
	if got.Verdict != result.VerdictFailed {
		t.Errorf("Verdict = %q, want %q", got.Verdict, result.VerdictFailed)
	}
	if got.FailureCategory != "response_unparseable" {
		t.Errorf("FailureCategory = %q, want %q", got.FailureCategory, "response_unparseable")
	}
}

// TestParseRuleResponse_AppliedFalseMissingSkipReason は applied=false で
// skip_reason 欠落の必須フィールド検証違反を failed 扱いにすることを検証する。
func TestParseRuleResponse_AppliedFalseMissingSkipReason(t *testing.T) {
	resp := provider.AgentResponse{
		Content: `{"applied": false, "skip_reason": ""}`,
	}

	got, err := ParseRuleResponse(resp)
	if err == nil {
		t.Errorf("ParseRuleResponse() error = nil, want non-nil")
	}
	if got.Verdict != result.VerdictFailed {
		t.Errorf("Verdict = %q, want %q", got.Verdict, result.VerdictFailed)
	}
	if got.FailureCategory != "response_unparseable" {
		t.Errorf("FailureCategory = %q, want %q", got.FailureCategory, "response_unparseable")
	}
}

// TestParseRuleResponse_MissingFileIsFailed は details[].file が欠落 / 空文字の
// ケースで Verdict=failed + FailureCategory="response_unparseable" になることを
// 検証する (review/result/types.go Finding doc 必須化契約 / review-apply-workflow
// design.md §Patch fail-closed 前提)。
func TestParseRuleResponse_MissingFileIsFailed(t *testing.T) {
	resp := provider.AgentResponse{
		Content: `{"applied": true, "findings_present": true, "summary": "x", "details": [{"severity": "warn", "message": "missing file"}]}`,
	}

	got, err := ParseRuleResponse(resp)
	if err == nil {
		t.Errorf("ParseRuleResponse() error = nil, want non-nil (file 欠落で failed)")
	}
	if got.Verdict != result.VerdictFailed {
		t.Errorf("Verdict = %q, want %q", got.Verdict, result.VerdictFailed)
	}
	if got.FailureCategory != "response_unparseable" {
		t.Errorf("FailureCategory = %q, want %q", got.FailureCategory, "response_unparseable")
	}
}

// TestParseRuleResponse_GreedyRegexNested は ネストした JSON オブジェクトを含む
// Content からも greedy regex (最初の `{` から最後の `}` まで) で外側の applied
// オブジェクトを正しく抽出できることを検証する。
func TestParseRuleResponse_GreedyRegexNested(t *testing.T) {
	resp := provider.AgentResponse{
		Content: `prefix {"applied": true, "findings_present": true, "summary": "ok", "details": [{"severity": "info", "file": "a/b.go", "location": "a/b.go:1", "message": "msg"}]} suffix`,
	}

	got, err := ParseRuleResponse(resp)
	if err != nil {
		t.Fatalf("ParseRuleResponse() unexpected error: %v", err)
	}
	if got.Verdict != result.VerdictFindingsReported {
		t.Errorf("Verdict = %q, want %q", got.Verdict, result.VerdictFindingsReported)
	}
	if len(got.Findings) != 1 || got.Findings[0].Severity != result.SeverityInfo {
		t.Errorf("Findings = %+v, want 1 info finding", got.Findings)
	}
}
