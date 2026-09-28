package inquiryloop

import "fmt"

// completedResult は has_inquiry=false で正常終了したときの FinalResult を組み立てる。
//
// hasAdvisory / advisory は validate-* 系の retry ループ駆動用フィールド。
// 通常 phase 呼び出しでは false / "" を渡す。
func completedResult(rounds int, summary, lastSession string, trail []Round, verdict ValidationVerdict, hasAdvisory bool, advisory string, extensions map[string]any) FinalResult {
	return FinalResult{
		Status:      StatusCompleted,
		Termination: ReasonCompleted,
		Rounds:      rounds,
		Summary:     summary,
		Verdict:     verdict,
		HasAdvisory: hasAdvisory,
		Advisory:    advisory,
		Extensions:  cloneExtensions(extensions),
		LastSession: lastSession,
		Trail:       trail,
		Failure:     nil,
	}
}

func cloneExtensions(extensions map[string]any) map[string]any {
	if len(extensions) == 0 {
		return nil
	}
	clone := make(map[string]any, len(extensions))
	for name, value := range extensions {
		clone[name] = value
	}
	return clone
}

// loopLimitResult は MaxRounds 超過 (収束せず) で abort したときの FinalResult を組み立てる。
// FailureDetail に最終ラウンドの問い合わせを文字列化して残し、result.json での原因追跡を支援する。
func loopLimitResult(rounds int, lastSession string, trail []Round, lastInquiry Inquiry) FinalResult {
	return FinalResult{
		Status:      StatusAborted,
		Termination: ReasonLoopLimit,
		Rounds:      rounds,
		LastSession: lastSession,
		Trail:       trail,
		Failure: &FailureDetail{
			Round:      rounds,
			Reason:     ReasonLoopLimit,
			Message:    fmt.Sprintf("inquiry loop did not converge within %d rounds", rounds),
			RawContent: lastInquiry.Question,
		},
	}
}

// promptFailureResult は agent 呼び出しの error / success=false で abort したときの FinalResult を組み立てる。
// rawContent は応答の生本文 (診断用)。
func promptFailureResult(round int, message, rawContent string, trail []Round, lastSession string) FinalResult {
	return FinalResult{
		Status:      StatusAborted,
		Termination: ReasonPromptFailure,
		Rounds:      round,
		LastSession: lastSession,
		Trail:       trail,
		Failure: &FailureDetail{
			Round:      round,
			Reason:     ReasonPromptFailure,
			Message:    message,
			RawContent: rawContent,
		},
	}
}

// schemaViolationResult は parseResponse 失敗 (json_schema 違反) で abort したときの FinalResult を組み立てる。
// rawContent には診断用の生 Content を残す。
func schemaViolationResult(round int, message, rawContent string, trail []Round, lastSession string) FinalResult {
	return FinalResult{
		Status:      StatusAborted,
		Termination: ReasonSchemaViolation,
		Rounds:      round,
		LastSession: lastSession,
		Trail:       trail,
		Failure: &FailureDetail{
			Round:      round,
			Reason:     ReasonSchemaViolation,
			Message:    message,
			RawContent: rawContent,
		},
	}
}
