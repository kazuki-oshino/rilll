// Package apply のテスト本ファイルは ParseSelfReview の公開契約 (tasks.md 3.2 /
// design.md §Parse / Req 4.1, 4.5) をテーブル駆動で機械的に検証する。
//
// 検証観点 (tasks.md 3.2 観察可能完了条件):
//   - structured 正常: provider.AgentResponse.StructuredOutput が schema 適合の
//     場合に SelfReviewRecord が verdict / reason / attempt とともに返る
//   - fallback 正常: StructuredOutput が nil の cursor 経路で Content から
//     greedy regex で JSON 抽出して同等結果を得る
//   - verdict 範囲外: verdict が accept / regenerate 以外で
//     ErrSelfReviewSchemaViolation sentinel が返る
//   - 完全 unparseable: StructuredOutput nil + Content に JSON 候補なしで
//     ErrSelfReviewUnparseable sentinel が返る
package apply

import (
	"errors"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

// TestParseSelfReview_StructuredOutputAccept は StructuredOutput direct decode 経路で
// verdict=accept + reason の正常系を検証する (Req 4.1)。
func TestParseSelfReview_StructuredOutputAccept(t *testing.T) {
	resp := provider.AgentResponse{
		Success: true,
		Content: "Content は無視されるべき (StructuredOutput 優先)",
		StructuredOutput: map[string]any{
			"verdict": "accept",
			"reason":  "diff は指摘内容に対して妥当",
		},
	}

	got, err := ParseSelfReview(resp, 1)
	if err != nil {
		t.Fatalf("ParseSelfReview() unexpected error: %v", err)
	}
	if got.Verdict != SelfReviewAccept {
		t.Errorf("Verdict = %q, want %q", got.Verdict, SelfReviewAccept)
	}
	if got.Reason != "diff は指摘内容に対して妥当" {
		t.Errorf("Reason = %q, want %q", got.Reason, "diff は指摘内容に対して妥当")
	}
	if got.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1", got.Attempt)
	}
}

// TestParseSelfReview_ContentFallbackRegenerate は StructuredOutput が nil の
// cursor 経路で Content から JSON 抽出する fallback 経路を検証する (Req 4.1)。
func TestParseSelfReview_ContentFallbackRegenerate(t *testing.T) {
	resp := provider.AgentResponse{
		Success: true,
		// 前後に散文や fenced code block が混じっていても抽出できることを示す。
		Content: "判定結果:\n```json\n" +
			`{"verdict": "regenerate", "reason": "scope 違反の可能性"}` +
			"\n```\nよろしくお願いします。",
		StructuredOutput: nil,
	}

	got, err := ParseSelfReview(resp, 3)
	if err != nil {
		t.Fatalf("ParseSelfReview() unexpected error: %v", err)
	}
	if got.Verdict != SelfReviewRegenerate {
		t.Errorf("Verdict = %q, want %q", got.Verdict, SelfReviewRegenerate)
	}
	if got.Reason != "scope 違反の可能性" {
		t.Errorf("Reason = %q, want %q", got.Reason, "scope 違反の可能性")
	}
	if got.Attempt != 3 {
		t.Errorf("Attempt = %d, want 3", got.Attempt)
	}
}

// TestParseSelfReview_SchemaViolation は verdict が accept/regenerate 以外の値で
// ErrSelfReviewSchemaViolation sentinel が返ることを検証する (Req 4.5)。
func TestParseSelfReview_SchemaViolation(t *testing.T) {
	resp := provider.AgentResponse{
		Success: true,
		StructuredOutput: map[string]any{
			"verdict": "maybe", // closed set 違反
			"reason":  "判断保留",
		},
	}

	got, err := ParseSelfReview(resp, 2)
	if err == nil {
		t.Fatalf("ParseSelfReview() error = nil, want ErrSelfReviewSchemaViolation")
	}
	if !errors.Is(err, ErrSelfReviewSchemaViolation) {
		t.Errorf("error = %v, want errors.Is ErrSelfReviewSchemaViolation", err)
	}
	// 失敗時は zero-value SelfReviewRecord を返す契約 (workflow 5.3 が VerdictFailed に変換)。
	if got.Verdict != "" {
		t.Errorf("Verdict = %q, want empty (zero-value record)", got.Verdict)
	}
}

// TestParseSelfReview_Unparseable は StructuredOutput nil + Content に JSON 候補
// なしの完全失敗で ErrSelfReviewUnparseable sentinel が返ることを検証する (Req 4.5)。
func TestParseSelfReview_Unparseable(t *testing.T) {
	resp := provider.AgentResponse{
		Success:          true,
		Content:          "JSON 構造を含まない散文だけの応答です。",
		StructuredOutput: nil,
	}

	got, err := ParseSelfReview(resp, 1)
	if err == nil {
		t.Fatalf("ParseSelfReview() error = nil, want ErrSelfReviewUnparseable")
	}
	if !errors.Is(err, ErrSelfReviewUnparseable) {
		t.Errorf("error = %v, want errors.Is ErrSelfReviewUnparseable", err)
	}
	if got.Verdict != "" {
		t.Errorf("Verdict = %q, want empty (zero-value record)", got.Verdict)
	}
}

// TestParseSelfReview_StructuredDecodeFailFallthroughToContent は StructuredOutput が
// 構造体に decode できない型 (例: 配列) であっても Content fallback 経路で救済される
// ことを検証する (cursor 等で structured 経路が破綻しても fallback を効かせる契約 / Req 4.1)。
func TestParseSelfReview_StructuredDecodeFailFallthroughToContent(t *testing.T) {
	resp := provider.AgentResponse{
		Success: true,
		Content: `{"verdict":"accept","reason":"fallback OK"}`,
		// 配列は rawSelfReview 構造体への json.Unmarshal で失敗するため、
		// fallback の Content 経路に流れる。
		StructuredOutput: []any{1, 2, 3},
	}

	got, err := ParseSelfReview(resp, 5)
	if err != nil {
		t.Fatalf("ParseSelfReview() unexpected error: %v", err)
	}
	if got.Verdict != SelfReviewAccept {
		t.Errorf("Verdict = %q, want %q", got.Verdict, SelfReviewAccept)
	}
	if got.Reason != "fallback OK" {
		t.Errorf("Reason = %q, want %q", got.Reason, "fallback OK")
	}
	if got.Attempt != 5 {
		t.Errorf("Attempt = %d, want 5", got.Attempt)
	}
}
