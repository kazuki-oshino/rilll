package kiro

import (
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/inquiryloop"
)

func TestBuildPreflightValidationFeedbackSeparatesFromValidateDesign(t *testing.T) {
	message := "validate-design preflight failed: 04-spec-design の成果物が validate-design に進める状態ではありません。"

	feedback := buildPreflightValidationFeedback(
		5,
		"validate-design",
		1,
		message,
		ValidationRetryActionRetrySpecDesign,
		false,
	)

	if feedback.Source != string(ValidationFeedbackSourcePreflight) {
		t.Fatalf("source = %q, want preflight", feedback.Source)
	}
	if feedback.Verdict != string(inquiryloop.VerdictNoGo) {
		t.Fatalf("verdict = %q, want no_go", feedback.Verdict)
	}
	if feedback.VerdictClass != "" {
		t.Fatalf("verdictClass = %q, want empty for preflight", feedback.VerdictClass)
	}
	if feedback.RetryAction != string(ValidationRetryActionRetrySpecDesign) {
		t.Fatalf("retryAction = %q, want retry_spec_design", feedback.RetryAction)
	}
	if feedback.Blocker != nil {
		t.Fatalf("blocker must be nil for preflight: %#v", feedback.Blocker)
	}
	if feedback.SameBlockerCount != 0 {
		t.Fatalf("sameBlockerCount = %d, want 0", feedback.SameBlockerCount)
	}
	if feedback.Message != message {
		t.Fatalf("message = %q", feedback.Message)
	}
	if feedback.Terminal {
		t.Fatalf("terminal = true, want false for retry preflight")
	}
	if !strings.Contains(feedback.Message, "validate-design preflight failed") {
		t.Fatalf("message must preserve deterministic preflight marker: %q", feedback.Message)
	}
}

func TestRecordPreflightValidationFeedbackDoesNotTouchBlockerRetryState(t *testing.T) {
	state := &runState{}
	message := "validate-design preflight failed: design.md が空です"

	state.recordPreflightValidationFeedback(
		5,
		"validate-design",
		1,
		message,
		ValidationRetryActionRetrySpecDesign,
		false,
	)
	state.recordPreflightValidationFeedback(
		5,
		"validate-design",
		2,
		message,
		ValidationRetryActionStopForHuman,
		true,
	)

	if len(state.ValidationFeedback) != 2 {
		t.Fatalf("ValidationFeedback len = %d, want 2", len(state.ValidationFeedback))
	}
	for i, got := range state.ValidationFeedback {
		if got.Source != string(ValidationFeedbackSourcePreflight) {
			t.Fatalf("feedback[%d].source = %q, want preflight", i, got.Source)
		}
		if got.Blocker != nil {
			t.Fatalf("feedback[%d].blocker must be nil", i)
		}
	}
	if state.ValidationFeedback[1].Terminal != true {
		t.Fatalf("terminal abort feedback must be terminal=true")
	}
	if state.BlockerRetry.CriticalTotalAttempts != 0 || state.BlockerRetry.AdvisoryTotalAttempts != 0 {
		t.Fatalf("preflight feedback must not mutate BlockerRetry: %+v", state.BlockerRetry)
	}
}
