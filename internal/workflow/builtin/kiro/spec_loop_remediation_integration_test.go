package kiro

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

func currentSpecRemediationFields(feature, fingerprint string) map[string]any {
	return map[string]any{
		"blocker_category":    string(ValidationBlockerCategoryCrossSpecContract),
		"blocker_fingerprint": fingerprint,
		"affected_artifacts":  []any{".kiro/specs/" + feature + "/design.md"},
		"recommended_action":  string(ValidationRecommendedActionReviseCurrentSpec),
		"owner_boundary":      string(ValidationOwnerBoundaryCurrentSpec),
	}
}

func validateDesignStructuredOutput(
	verdict string,
	hasAdvisory bool,
	summary string,
	advisory string,
	remediation map[string]any,
) map[string]any {
	out := map[string]any{
		"has_inquiry":  false,
		"summary":      summary,
		"verdict":      verdict,
		"has_advisory": hasAdvisory,
	}
	if hasAdvisory {
		out["advisory"] = advisory
	}
	for key, value := range remediation {
		out[key] = value
	}
	return out
}

func validateDesignRemediationResp(
	verdict string,
	hasAdvisory bool,
	summary string,
	advisory string,
	feature string,
	fingerprint string,
) provider.AgentResponse {
	return provider.AgentResponse{
		Success:          true,
		SessionID:        "vd-remediation",
		StructuredOutput: validateDesignStructuredOutput(verdict, hasAdvisory, summary, advisory, currentSpecRemediationFields(feature, fingerprint)),
	}
}

func upstreamSpecRemediationFields(upstreamFeature, fingerprint string) map[string]any {
	return map[string]any{
		"blocker_category":    string(ValidationBlockerCategoryCrossSpecContract),
		"blocker_fingerprint": fingerprint,
		"affected_artifacts":  []any{".kiro/specs/" + upstreamFeature + "/design.md"},
		"recommended_action":  string(ValidationRecommendedActionConfirmUpstreamContract),
		"owner_boundary":      string(ValidationOwnerBoundaryUpstreamSpec),
		"owner_spec":          upstreamFeature,
	}
}

func validateDesignUpstreamRemediationResp(
	summary string,
	upstreamFeature string,
	fingerprint string,
) provider.AgentResponse {
	return provider.AgentResponse{
		Success:   true,
		SessionID: "vd-upstream",
		StructuredOutput: validateDesignStructuredOutput(
			"no_go",
			false,
			summary,
			"",
			upstreamSpecRemediationFields(upstreamFeature, fingerprint),
		),
	}
}

func countSkillCallsContaining(calls []recordedAgentCall, substr string) int {
	n := 0
	for _, call := range calls {
		if call.Kind == "skill" && strings.Contains(call.Prompt, substr) {
			n++
		}
	}
	return n
}

// TestKiroSpecLoopValidateDesignRemediationRetriesSpecDesignThenCleanGo は task 5.1 の
// 観察可能完了条件を固定する:
//   - valid current-spec no_go は packet 化され validationFeedback[] に記録される
//   - 04-spec-design retry prompt に # Validation Remediation / fingerprint / affected artifact / recommended action が入る
//   - 次の clean go で 06-spec-tasks へ進む
func TestKiroSpecLoopValidateDesignRemediationRetriesSpecDesignThenCleanGo(t *testing.T) {
	feature := "remediation-no-go-then-go"
	const fingerprint = "combat-domain-turn-contract-envelope"
	specJSON := fmt.Sprintf(`{
  "feature_name": %q,
  "phase": "requirements-generated",
  "approvals": {
    "requirements": {"generated": true, "approved": true},
    "design": {"generated": true, "approved": false},
    "tasks": {"generated": false, "approved": false}
  }
}`, feature)
	cwd := setupFeatureCWD(t, feature, "Brief 本文", specJSON)
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}
	validateCount := 0
	const noGoSummary = "NO-GO: combat domain turn contract envelope が design.md に未記載"
	agent := &scriptedAgent{
		Respond: func(call recordedAgentCall) (provider.AgentResponse, error) {
			switch {
			case call.Kind == "commit":
				return commitOKResp(), nil
			case strings.Contains(call.Prompt, "/kiro-validate-design "+feature):
				validateCount++
				if validateCount == 1 {
					resp := validateDesignRemediationResp("no_go", false, noGoSummary, "", feature, fingerprint)
					resp.SessionID = "vd-1"
					return resp, nil
				}
				return provider.AgentResponse{
					Success:   true,
					SessionID: "vd-2",
					StructuredOutput: map[string]any{
						"has_inquiry":  false,
						"summary":      "GO",
						"verdict":      "go",
						"has_advisory": false,
					},
				}, nil
			default:
				return skillNoInquiryResp(), nil
			}
		},
	}

	if err := runKiroSpecLoop(context.Background(), cwd, onEvent, ask, agent); err != nil {
		t.Fatalf("runKiroSpecLoop returned error: %v", err)
	}

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonCompleted {
		t.Fatalf("terminationReason=%q, want completed", rr.TerminationReason)
	}
	if validateCount != 2 {
		t.Fatalf("validate-design は 2 回呼ばれるべき (1 no_go + 1 go): got=%d", validateCount)
	}
	if len(rr.Steps) != 3 {
		t.Fatalf("rr.Steps は 3 件 (spec-design + validate-design + spec-tasks): got=%d", len(rr.Steps))
	}
	if rr.Steps[1].Verdict != "go" {
		t.Fatalf("validate-design 最終 verdict=%q, want go", rr.Steps[1].Verdict)
	}

	hasSpecTasks := false
	for _, c := range agent.Calls {
		if strings.Contains(c.Prompt, "/kiro-spec-tasks "+feature) {
			hasSpecTasks = true
			break
		}
	}
	if !hasSpecTasks {
		t.Fatalf("clean go 後は spec-tasks まで進むべき")
	}

	if len(rr.ValidationFeedback) != 1 {
		t.Fatalf("validationFeedback len = %d, want 1: %#v", len(rr.ValidationFeedback), rr.ValidationFeedback)
	}
	fb := rr.ValidationFeedback[0]
	if fb.Source != string(ValidationFeedbackSourceValidateDesign) {
		t.Fatalf("validationFeedback[0].source = %q, want validate-design", fb.Source)
	}
	if fb.RetryAction != string(ValidationRetryActionRetrySpecDesign) {
		t.Fatalf("validationFeedback[0].retryAction = %q, want retry_spec_design", fb.RetryAction)
	}
	if fb.Blocker == nil || fb.Blocker.Fingerprint != fingerprint {
		t.Fatalf("validationFeedback[0].blocker = %#v, want fingerprint %q", fb.Blocker, fingerprint)
	}
	if fb.Terminal {
		t.Fatalf("retry feedback must not be terminal: %#v", fb)
	}

	var specDesignPrompts []string
	for _, c := range agent.Calls {
		if c.Kind == "skill" && strings.Contains(c.Prompt, "/kiro-spec-design "+feature) {
			specDesignPrompts = append(specDesignPrompts, c.Prompt)
		}
	}
	if len(specDesignPrompts) != 2 {
		t.Fatalf("spec-design skill は 2 回呼ばれるべき: got=%d", len(specDesignPrompts))
	}
	retryPrompt := specDesignPrompts[1]
	for _, want := range []string{
		"# Validation Remediation",
		fingerprint,
		".kiro/specs/" + feature + "/design.md",
		string(ValidationRecommendedActionReviseCurrentSpec),
		noGoSummary,
	} {
		if !strings.Contains(retryPrompt, want) {
			t.Errorf("2 回目 spec-design prompt に %q が含まれていない:\n%s", want, retryPrompt)
		}
	}
}

// TestKiroSpecLoopValidateDesignRemediationSchemaViolationTerminal は task 5.2 の
// schema violation terminal 経路を固定する:
//   - remediation required fields 欠落は validation-no-go ではなく schema-violation で停止する
//   - failure.rawContent に診断情報が残り validationFeedback[] は空のまま区別できる
//   - spec-design retry は起動されない
func TestKiroSpecLoopValidateDesignRemediationSchemaViolationTerminal(t *testing.T) {
	feature := "remediation-schema-violation"
	specJSON := fmt.Sprintf(`{
  "feature_name": %q,
  "phase": "requirements-generated",
  "approvals": {
    "requirements": {"generated": true, "approved": true},
    "design": {"generated": true, "approved": false},
    "tasks": {"generated": false, "approved": false}
  }
}`, feature)
	cwd := setupFeatureCWD(t, feature, "Brief 本文", specJSON)
	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Selected: feature}
	const noGoSummary = "NO-GO: remediation fields intentionally omitted"
	agent := &scriptedAgent{
		Respond: func(call recordedAgentCall) (provider.AgentResponse, error) {
			switch {
			case call.Kind == "commit":
				return commitOKResp(), nil
			case strings.Contains(call.Prompt, "/kiro-validate-design "+feature):
				return provider.AgentResponse{
					Success:   true,
					SessionID: "vd-schema-violation",
					StructuredOutput: map[string]any{
						"has_inquiry":  false,
						"summary":      noGoSummary,
						"verdict":      "no_go",
						"has_advisory": false,
						// blocker_fingerprint など remediation fields を意図的に欠落
					},
				}, nil
			default:
				return skillNoInquiryResp(), nil
			}
		},
	}

	if err := runKiroSpecLoop(context.Background(), cwd, onEvent, ask, agent); err != nil {
		t.Fatalf("runKiroSpecLoop returned error: %v", err)
	}

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonSchemaViolation {
		t.Fatalf("terminationReason=%q, want schema-violation (not validation-no-go)", rr.TerminationReason)
	}
	if rr.AbortStep == nil || rr.AbortStep.Order != 5 || rr.AbortStep.Phase != "validate-design" {
		t.Fatalf("abortStep should point to validate-design: %+v", rr.AbortStep)
	}
	if rr.Failure == nil {
		t.Fatalf("schema-violation では Failure 非 nil を期待")
	}
	if rr.Failure.Reason != string(RunReasonSchemaViolation) {
		t.Fatalf("Failure.Reason=%q, want schema-violation", rr.Failure.Reason)
	}
	if !strings.Contains(rr.Failure.Message, "blocker_category") {
		t.Fatalf("Failure.Message should diagnose missing remediation field: %q", rr.Failure.Message)
	}
	if rr.Failure.RawContent == "" {
		t.Fatalf("Failure.RawContent should preserve validator response for diagnosis")
	}
	if len(rr.ValidationFeedback) != 0 {
		t.Fatalf("schema violation must not populate validationFeedback[]: %#v", rr.ValidationFeedback)
	}
	if countSkillCallsContaining(agent.Calls, "/kiro-spec-design "+feature) != 1 {
		t.Fatalf("schema violation must not launch spec-design retry")
	}
	if !hasErrorEventAll(*eventsPtr, "validate-design", string(RunReasonSchemaViolation)) {
		t.Fatalf("WorkflowError should mention validate-design and schema-violation: %+v", *eventsPtr)
	}
}

// TestKiroSpecLoopValidateDesignUpstreamStopTerminal は task 5.2 の upstream stop 経路を固定する:
//   - upstream owner は validation-no-go で停止し owner spec / blocker / recommended action を trace に残す
//   - 04-spec-design retry は起動されない
func TestKiroSpecLoopValidateDesignUpstreamStopTerminal(t *testing.T) {
	feature := "remediation-upstream-stop"
	const upstreamFeature = "shared-contracts"
	const fingerprint = "shared-contract-gap"
	specJSON := fmt.Sprintf(`{
  "feature_name": %q,
  "phase": "requirements-generated",
  "approvals": {
    "requirements": {"generated": true, "approved": true},
    "design": {"generated": true, "approved": false},
    "tasks": {"generated": false, "approved": false}
  }
}`, feature)
	cwd := setupFeatureCWD(t, feature, "Brief 本文", specJSON)
	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Selected: feature}
	const noGoSummary = "NO-GO: upstream shared contract requires owner confirmation"
	agent := &scriptedAgent{
		Respond: func(call recordedAgentCall) (provider.AgentResponse, error) {
			switch {
			case call.Kind == "commit":
				return commitOKResp(), nil
			case strings.Contains(call.Prompt, "/kiro-validate-design "+feature):
				return validateDesignUpstreamRemediationResp(noGoSummary, upstreamFeature, fingerprint), nil
			default:
				return skillNoInquiryResp(), nil
			}
		},
	}

	if err := runKiroSpecLoop(context.Background(), cwd, onEvent, ask, agent); err != nil {
		t.Fatalf("runKiroSpecLoop returned error: %v", err)
	}

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonValidationNoGo {
		t.Fatalf("terminationReason=%q, want validation-no-go (not schema-violation)", rr.TerminationReason)
	}
	if rr.Failure != nil {
		t.Fatalf("upstream stop must not use failure path: %#v", rr.Failure)
	}
	if rr.AbortStep == nil || rr.AbortStep.Phase != "validate-design" {
		t.Fatalf("abortStep should point to validate-design: %+v", rr.AbortStep)
	}
	if !strings.Contains(rr.AbortMessage, upstreamFeature) {
		t.Fatalf("abortMessage should include upstream owner: %q", rr.AbortMessage)
	}
	if !strings.Contains(rr.AbortMessage, fingerprint) {
		t.Fatalf("abortMessage should include blocker fingerprint: %q", rr.AbortMessage)
	}
	if len(rr.ValidationFeedback) != 1 {
		t.Fatalf("validationFeedback len = %d, want 1: %#v", len(rr.ValidationFeedback), rr.ValidationFeedback)
	}
	fb := rr.ValidationFeedback[0]
	if !fb.Terminal {
		t.Fatalf("upstream feedback must be terminal: %#v", fb)
	}
	if fb.RetryAction != string(ValidationRetryActionStopForHuman) {
		t.Fatalf("validationFeedback retryAction = %q, want stop_for_human", fb.RetryAction)
	}
	if fb.Blocker == nil || fb.Blocker.OwnerSpec != upstreamFeature {
		t.Fatalf("validationFeedback blocker = %#v, want ownerSpec %q", fb.Blocker, upstreamFeature)
	}
	if fb.Blocker.RecommendedAction != string(ValidationRecommendedActionConfirmUpstreamContract) {
		t.Fatalf("validationFeedback recommendedAction = %q", fb.Blocker.RecommendedAction)
	}
	if countSkillCallsContaining(agent.Calls, "/kiro-spec-design "+feature) != 1 {
		t.Fatalf("upstream stop must not launch spec-design retry")
	}
	if !hasErrorEvent(*eventsPtr, string(RunReasonValidationNoGo)) {
		t.Fatalf("WorkflowError should mention validation-no-go: %+v", *eventsPtr)
	}
}

// TestKiroSpecLoopValidateDesignSameBlockerExhaustionTerminal は task 5.2 の
// same blocker exhaustion terminal 経路を固定する:
//   - 同一 critical blocker 3 回で validation-no-go に停止し schema-violation / upstream stop と区別できる
//   - 最後の blocker が abortMessage と validationFeedback[] の双方から追跡できる
func TestKiroSpecLoopValidateDesignSameBlockerExhaustionTerminal(t *testing.T) {
	feature := "remediation-same-blocker-exhaustion"
	const fingerprint = "same-critical-blocker"
	specJSON := fmt.Sprintf(`{
  "feature_name": %q,
  "phase": "requirements-generated",
  "approvals": {
    "requirements": {"generated": true, "approved": true},
    "design": {"generated": true, "approved": false},
    "tasks": {"generated": false, "approved": false}
  }
}`, feature)
	cwd := setupFeatureCWD(t, feature, "Brief 本文", specJSON)
	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Selected: feature}
	validateCount := 0
	agent := &scriptedAgent{
		Respond: func(call recordedAgentCall) (provider.AgentResponse, error) {
			switch {
			case call.Kind == "commit":
				return commitOKResp(), nil
			case strings.Contains(call.Prompt, "/kiro-validate-design "+feature):
				validateCount++
				resp := validateDesignRemediationResp(
					"no_go",
					false,
					fmt.Sprintf("NO-GO attempt %d", validateCount),
					"",
					feature,
					fingerprint,
				)
				resp.SessionID = fmt.Sprintf("vd-session-%d", validateCount)
				return resp, nil
			default:
				return skillNoInquiryResp(), nil
			}
		},
	}

	if err := runKiroSpecLoop(context.Background(), cwd, onEvent, ask, agent); err != nil {
		t.Fatalf("runKiroSpecLoop returned error: %v", err)
	}

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonValidationNoGo {
		t.Fatalf("terminationReason=%q, want validation-no-go", rr.TerminationReason)
	}
	if rr.Failure != nil {
		t.Fatalf("same blocker exhaustion must not use failure path: %#v", rr.Failure)
	}
	if validateCount != 3 {
		t.Fatalf("validate-design は 3 回呼ばれるべき: got=%d", validateCount)
	}
	if !strings.Contains(rr.AbortMessage, fingerprint) {
		t.Fatalf("abortMessage should include last blocker fingerprint: %q", rr.AbortMessage)
	}
	if len(rr.ValidationFeedback) != 3 {
		t.Fatalf("validationFeedback len = %d, want 3: %#v", len(rr.ValidationFeedback), rr.ValidationFeedback)
	}
	last := rr.ValidationFeedback[2]
	if !last.Terminal {
		t.Fatalf("last validationFeedback must be terminal: %#v", last)
	}
	if last.RetryAction != string(ValidationRetryActionStopForHuman) {
		t.Fatalf("last validationFeedback retryAction = %q, want stop_for_human", last.RetryAction)
	}
	if last.Blocker == nil || last.Blocker.Fingerprint != fingerprint {
		t.Fatalf("last validationFeedback blocker = %#v, want fingerprint %q", last.Blocker, fingerprint)
	}
	if last.SameBlockerCount != 3 {
		t.Fatalf("last validationFeedback sameBlockerCount = %d, want 3", last.SameBlockerCount)
	}
	if !hasErrorEvent(*eventsPtr, string(RunReasonValidationNoGo)) {
		t.Fatalf("WorkflowError should mention validation-no-go: %+v", *eventsPtr)
	}
}
