// Package workflow の本テストファイルは kiro-impl-loop の最終検証 (final-review) で
// inquiryloop.Engine.InquiryDirective に finalReviewInquiryDirective が配線されている
// ことを regression-lock するテストを集約する。
//
// 背景 (issue #61):
//
//	/kiro-validate-impl が build / test / spec 充足の不合格を検出しても、skill 側が
//	「自分で直せる」と判断して has_inquiry=false を返すと、runFinalReview は
//	result.Rounds==0 を「問題なし」とみなして Phase 2-5 を全 skip してしまう
//	(akashic 2026-05-12T04-06-05 で再現)。第 3 層防御として final-review 専用の
//	loop policy を inquiry-skill 側に毎ラウンド注入し、不合格検出時は必ず
//	has_inquiry=true で workflow に明示通知させる。
package kiro

import (
	"context"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

// implFinalReviewInquiryResp は /kiro-validate-impl が初回呼び出しで
// 「不合格を検出 → 修正方針を inquiry として workflow に問い合わせる」典型応答。
// finalReviewInquiryDirective が配線されていることを再現するための fixture。
func implFinalReviewInquiryResp() provider.AgentResponse {
	return provider.AgentResponse{
		Success: true,
		StructuredOutput: map[string]any{
			"has_inquiry": true,
			"summary":     "go test ./cmd/di が失敗。wire_gen.go の追従が必要。",
			"inquiry": map[string]any{
				"title":      "wire_gen.go 不整合",
				"background": "新引数追加に追従していない",
				"question":   "wire_gen.go を再生成すべきか、手動で追従させるか？",
			},
		},
		SessionID: "session-final-inquiry",
	}
}

// implFinalReviewResumeOKResp は /kiro-validate-impl の resume 応答 (修正方針確定後)。
// has_inquiry=false で Phase 1 inquiry loop を rounds=1 で完走させる。
func implFinalReviewResumeOKResp() provider.AgentResponse {
	return provider.AgentResponse{
		Success: true,
		StructuredOutput: map[string]any{
			"has_inquiry": false,
			"summary":     "方針確定: wire_gen.go を再生成する",
		},
		SessionID: "session-final-resume",
	}
}

// implFinalReviewFixEditOKResp は Phase 2 (fix-edit, Turn 1 OutputFormat=nil) の成功応答。
// Content は markdown (Turn 2 失敗時の救出用)、SessionID は Phase 4 の resume 起点になる。
func implFinalReviewFixEditOKResp() provider.AgentResponse {
	return provider.AgentResponse{
		Success:   true,
		Content:   "wire_gen.go を再生成しました。",
		SessionID: "session-fix-edit",
	}
}

// implFinalReviewFixJudgeNoChangeResp は Phase 3 (fix-judge, Turn 2 schema 強制) の応答で、
// summary 末尾に "Files Modified: 0" を持ち Phase 4-5 を skip させる。
//
// 注意: 本テストは final-review inquiry loop の prompt 配線検証が目的なので、
// Phase 2-3 は OK=true で素通りすれば十分。changedFiles=0 にすることで Phase 4 の
// test-edit/test-judge fixture が不要になりテストが薄くなる。
func implFinalReviewFixJudgeNoChangeResp() provider.AgentResponse {
	return provider.AgentResponse{
		Success: true,
		StructuredOutput: map[string]any{
			"has_inquiry": false,
			"summary":     "再生成完了。\n\nFiles Modified: 0",
		},
		SessionID: "session-fix-judge",
	}
}

// TestKiroImplLoopFinalReview_InquiryDirectiveWiring: issue #61 対応の regression test。
//
// 配線契約 (Engine.InquiryDirective を通じた第 3 層防御):
//   - 最終検証 inquiry loop の **初回 skill call** prompt に "[final-review-policy]" マーカーが含まれる
//   - 最終検証 inquiry loop の **resume skill call** prompt にも "[final-review-policy]" マーカーが含まれる
//     (long-context drift 対策。engine.go L136 / L208 で initial / resume 双方に再付加される契約)
//   - 最終検証 inquiry loop の **answerer prompt** には "[final-review-policy]" マーカーが含まれない
//     (answer 側は AnswerContext + answerPolicySection + persona でガード済み)
//   - directive 本文に「has_inquiry=true」の文言が含まれる (workflow 意図の再強調)
//
// per-task ループ向け "[loop-policy]" は本テストでも初回 /kiro-impl 呼び出し prompt に
// 含まれるが、それは TestKiroImplLoopInquiry_InquiryDirectiveWiring 側で検証済みなので
// ここでは触れない (本テストは final-review 側 prompt 配線のみを検証する)。
func TestKiroImplLoopFinalReview_InquiryDirectiveWiring(t *testing.T) {
	feature := "demo"
	cwd := setupImplFeatureCWD(t, feature, "- [ ] 1. 単独タスク\n")
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}

	// per-task /kiro-impl は 0 round で完走させ、最終検証側の inquiry loop で 1 round 回す。
	// final-review fix-judge が "Files Modified: 0" を返すので Phase 4-5 はスキップされ、
	// WorkflowDone「最終検証で修正なし」で run が終わる。
	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "commit":
			return commitOKResp(), nil
		case "fix-edit":
			return implFinalReviewFixEditOKResp(), nil
		case "fix-judge":
			return implFinalReviewFixJudgeNoChangeResp(), nil
		case "answerer":
			return answerOKResp(), nil
		case "skill":
			switch {
			case strings.HasPrefix(call.Prompt, "/kiro-validate-impl"):
				// 最終検証 inquiry loop:
				//  - 初回呼び出し (resume=="") → has_inquiry=true (inquiry を workflow に通知)
				//  - resume 呼び出し (resume!="") → has_inquiry=false (方針確定で完走)
				if call.ResumeSessionID == "" {
					return implFinalReviewInquiryResp(), nil
				}
				return implFinalReviewResumeOKResp(), nil
			default:
				// per-task /kiro-impl は 0 round で即完走。
				return skillNoInquiryResp(), nil
			}
		}
		return skillNoInquiryResp(), nil
	}

	if err := runKiroImplLoop(
		context.Background(),
		cwd,
		onEvent,
		ask,
		agent,
	); err != nil {
		t.Fatalf("runKiroImplLoop returned error: %v", err)
	}

	// /kiro-validate-impl 関連の skill / answerer 呼び出しを抽出して prompt を観測する。
	// 期待される call 系列 (per-task 完了後の最終検証部分):
	//   - skill (initial) : prompt 先頭が "/kiro-validate-impl <feature>"、ResumeSessionID==""
	//   - answerer        : prompt 先頭が "# Persona" (buildAnswerPrompt 出力)
	//   - skill (resume)  : prompt は回答テキスト + final-review-policy + runner-directive。
	//                       prefix は "/kiro-validate-impl" ではなく "Source: ..." or 回答本文。
	//                       classifyAgentCall は "skill" を返す。ResumeSessionID は initial 応答の
	//                       SessionID (本テスト fixture では "session-final-inquiry")。
	//   - fix-edit / fix-judge (本テストでは prompt 観測対象外)
	var (
		validateImplInitial *recordedAgentCall
		validateImplResume  *recordedAgentCall
		finalAnswerer       *recordedAgentCall
	)
	var afterValidateImpl bool
	for i := range agent.Calls {
		c := &agent.Calls[i]
		switch {
		case c.Kind == "skill" && strings.HasPrefix(c.Prompt, "/kiro-validate-impl"):
			// 初回 /kiro-validate-impl call (ResumeSessionID="")
			validateImplInitial = c
			afterValidateImpl = true
		case c.Kind == "answerer" && afterValidateImpl && finalAnswerer == nil:
			// 初回 validate-impl 後の最初の answerer call (per-task 側は skillNoInquiryResp で
			// 即完走するため、answerer call は最終検証側でしか発生しない)。
			finalAnswerer = c
		case c.Kind == "skill" && afterValidateImpl && c.ResumeSessionID != "" && validateImplResume == nil:
			// 初回 validate-impl 後の最初の "resume 付き skill call" が validate-impl の resume。
			// per-task /kiro-impl は本テストでは has_inquiry=false で完走するため resume は発生しない。
			validateImplResume = c
		}
	}

	if validateImplInitial == nil {
		t.Fatalf("/kiro-validate-impl の初回 skill call が見つからない: calls=%+v", agent.Calls)
	}
	if validateImplResume == nil {
		t.Fatalf("/kiro-validate-impl の resume skill call が見つからない (inquiry loop が 1 round 回るはず): calls=%+v", agent.Calls)
	}
	if finalAnswerer == nil {
		t.Fatalf("最終検証 inquiry loop の answerer call が見つからない: calls=%+v", agent.Calls)
	}

	// (1) 初回 /kiro-validate-impl prompt に [final-review-policy] マーカーが含まれる。
	if !strings.Contains(validateImplInitial.Prompt, "[final-review-policy]") {
		t.Errorf("初回 /kiro-validate-impl prompt は [final-review-policy] マーカーを含むべき (issue #61):\n%s", validateImplInitial.Prompt)
	}

	// (2) resume /kiro-validate-impl prompt にも [final-review-policy] マーカーが含まれる
	//     (long-context drift 対策。engine.go が initial / resume 双方に再付加する契約)。
	if !strings.Contains(validateImplResume.Prompt, "[final-review-policy]") {
		t.Errorf("resume /kiro-validate-impl prompt は [final-review-policy] マーカーを含むべき (drift 対策):\n%s", validateImplResume.Prompt)
	}

	// (3) answerer prompt には [final-review-policy] を含めない
	//     (answer 側は AnswerContext + answerPolicySection + persona でガード済み)。
	if strings.Contains(finalAnswerer.Prompt, "[final-review-policy]") {
		t.Errorf("最終検証 answerer prompt は [final-review-policy] を含まないべき (別レイヤでガード済み):\n%s", finalAnswerer.Prompt)
	}
	for _, want := range []string{
		"# Policy",
		"[policy:ai-antipattern-guard]",
		"配線忘れ",
		"スコープクリープ",
	} {
		if !strings.Contains(finalAnswerer.Prompt, want) {
			t.Errorf("最終検証 answerer prompt は ai-antipattern guard policy marker %q を含むべき:\n%s", want, finalAnswerer.Prompt)
		}
	}
	if strings.Contains(finalAnswerer.Prompt, "embed:facets/policies/ai-antipattern-guard.md") {
		t.Errorf("最終検証 answerer prompt は policy SourcePath を LLM-visible に漏らしてはならない:\n%s", finalAnswerer.Prompt)
	}

	// (4) directive 本文に「has_inquiry=true」相当の規律が含まれる
	//     (workflow が問題検出時に必ず inquiry を立てさせる意図の再強調)。
	if !strings.Contains(validateImplInitial.Prompt, "has_inquiry=true") {
		t.Errorf("初回 /kiro-validate-impl prompt は final-review-policy 本文 (has_inquiry=true で workflow 通知) を含むべき:\n%s", validateImplInitial.Prompt)
	}
}
