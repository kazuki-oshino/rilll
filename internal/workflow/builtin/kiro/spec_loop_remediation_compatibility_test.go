package kiro

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
)

// TestSpecLoopCompatibilityPhaseOrderRegression は task 5.3 / Req 6.1 の
// 6 phase 固定順契約が remediation routing 導入後も維持されることを regression-lock する。
func TestSpecLoopCompatibilityPhaseOrderRegression(t *testing.T) {
	t.Parallel()

	if len(steps) != 6 {
		t.Fatalf("steps len = %d, want 6", len(steps))
	}
	wantPhases := []string{
		"spec-init",
		"spec-requirements",
		"validate-gap",
		"spec-design",
		"validate-design",
		"spec-tasks",
	}
	for i, want := range wantPhases {
		if steps[i].Order != i+1 {
			t.Fatalf("steps[%d].Order = %d, want %d", i, steps[i].Order, i+1)
		}
		if steps[i].Phase != want {
			t.Fatalf("steps[%d].Phase = %q, want %q", i, steps[i].Phase, want)
		}
	}
}

// TestRunResultAdvisoryJSONShapeRegression は advisories[] の既存 wire shape
// (order / phase / attempt / verdict / advisory) が additive field 追加後も維持されることを固定する。
func TestRunResultAdvisoryJSONShapeRegression(t *testing.T) {
	t.Parallel()

	advisory := RunResultAdvisory{
		Order:    5,
		Phase:    "validate-design",
		Attempt:  2,
		Verdict:  "go",
		Advisory: "legacy advisory projection body",
	}
	raw, err := json.Marshal(advisory)
	if err != nil {
		t.Fatalf("marshal advisory: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal advisory map: %v", err)
	}
	for _, key := range []string{"order", "phase", "attempt", "verdict", "advisory"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("advisories[] wire shape lost key %q: %#v", key, decoded)
		}
	}
	for _, absent := range []string{"validationFeedback", "blocker", "retryAction", "verdictClass"} {
		if _, ok := decoded[absent]; ok {
			t.Fatalf("advisories[] must not gain additive remediation keys: found %q", absent)
		}
	}
}

// TestSpecLoopCompatibilityHappyPathPreservesLegacyResultContract は clean completion 経路が
// 6 steps / terminationReason=completed / advisories 空 / validationFeedback 省略可能であることを固定する。
func TestSpecLoopCompatibilityHappyPathPreservesLegacyResultContract(t *testing.T) {
	feature := "compat-happy-path"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}
	agent := &recordedAgent{
		SkillResponse: provider.AgentResponse{
			Success: true,
			StructuredOutput: map[string]any{
				"has_inquiry": false,
				"summary":     "ok",
			},
			SessionID: "session-skill",
		},
		CommitResponse: provider.AgentResponse{
			Success:   true,
			Content:   "commit ok",
			SessionID: "session-commit",
		},
	}

	if err := runKiroSpecLoop(context.Background(), cwd, onEvent, ask, agent); err != nil {
		t.Fatalf("runKiroSpecLoop returned error: %v", err)
	}

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonCompleted {
		t.Fatalf("terminationReason=%q, want completed", rr.TerminationReason)
	}
	if len(rr.Steps) != 6 {
		t.Fatalf("completed path must keep 6 steps[] entries: got %d", len(rr.Steps))
	}
	for i, step := range rr.Steps {
		if step.Order != i+1 {
			t.Fatalf("steps[%d].Order=%d, want %d", i, step.Order, i+1)
		}
		if !step.CommitOK {
			t.Fatalf("steps[%d].CommitOK=false, want true on happy path", i)
		}
		if step.Attempt != 1 {
			t.Fatalf("steps[%d].Attempt=%d, want 1 on non-retry happy path", i, step.Attempt)
		}
	}
	if len(rr.Advisories) != 0 {
		t.Fatalf("happy path advisories[] must stay empty: got %d", len(rr.Advisories))
	}
	if len(rr.ValidationFeedback) != 0 {
		t.Fatalf("happy path validationFeedback[] must stay empty: got %d", len(rr.ValidationFeedback))
	}

	_, err := extractKiroSpecLoopOutput(context.Background(), cwd, workflow.ChildResult{
		DefinitionID: kiroSpecLoopID,
		RunID:        rr.RunID,
	})
	if err != nil {
		t.Fatalf("impl-loop completion gate must accept legacy completed result: %v", err)
	}
}

// TestSpecLoopCompatibilityValidateDesignRetryKeepsFinalAttemptSteps は validate-design retry 後も
// steps[] が最終 attempt の spec-design + validate-design の 2 件だけを保持する契約を固定する。
func TestSpecLoopCompatibilityValidateDesignRetryKeepsFinalAttemptSteps(t *testing.T) {
	feature := "compat-final-attempt"
	const fingerprint = "compat-same-blocker"
	specJSON := `{
  "feature_name": "compat-final-attempt",
  "phase": "requirements-generated",
  "approvals": {
    "requirements": {"generated": true, "approved": true},
    "design": {"generated": true, "approved": false},
    "tasks": {"generated": false, "approved": false}
  }
}`
	cwd := setupFeatureCWD(t, feature, "Brief 本文", specJSON)
	onEvent, _ := collectEvents()
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
					"NO-GO retry",
					"",
					feature,
					fingerprint,
				)
				resp.SessionID = fmt.Sprintf("vd-%d", validateCount)
				return resp, nil
			default:
				return skillNoInquiryResp(), nil
			}
		},
	}

	if err := runKiroSpecLoop(context.Background(), cwd, onEvent, ask, agent); err != nil {
		t.Fatalf("runKiroSpecLoop returned error: %v", err)
	}
	if validateCount != 3 {
		t.Fatalf("validate-design attempts = %d, want 3", validateCount)
	}

	rr := mustReadResultJSON(t, cwd)
	if len(rr.Steps) != 2 {
		t.Fatalf("retry abort must keep only final attempt steps[]: got %d (%#v)", len(rr.Steps), rr.Steps)
	}
	if rr.Steps[0].Phase != "spec-design" || rr.Steps[1].Phase != "validate-design" {
		t.Fatalf("final attempt steps[] phases = [%q, %q], want [spec-design, validate-design]",
			rr.Steps[0].Phase, rr.Steps[1].Phase)
	}
	if rr.Steps[1].Attempt != 3 {
		t.Fatalf("final validate-design step.Attempt=%d, want 3", rr.Steps[1].Attempt)
	}
	if len(rr.Advisories) != 3 {
		t.Fatalf("advisories[] must keep one entry per attempt: got %d", len(rr.Advisories))
	}
	for i, advisory := range rr.Advisories {
		if advisory.Order != 5 || advisory.Phase != "validate-design" {
			t.Fatalf("Advisories[%d] order/phase = (%d, %q), want (5, validate-design)", i, advisory.Order, advisory.Phase)
		}
		if advisory.Attempt != i+1 {
			t.Fatalf("Advisories[%d].Attempt=%d, want %d", i, advisory.Attempt, i+1)
		}
	}
}

// TestSpecImplLoopCompletionGateRejectsValidationTerminationReasons は
// validation-no-go / schema-violation が従来通り impl-loop gate を拒否することを固定する。
func TestSpecImplLoopCompletionGateRejectsValidationTerminationReasons(t *testing.T) {
	t.Parallel()

	const runID = "spec-run-non-completed"
	const feature = "gate-feature"

	tests := []struct {
		name              string
		terminationReason RunTerminationReason
	}{
		{name: "validation-no-go", terminationReason: RunReasonValidationNoGo},
		{name: "schema-violation", terminationReason: RunReasonSchemaViolation},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cwd := t.TempDir()
			writeSpecLoopResult(t, cwd, runID, RunResult{
				RunID:             runID,
				WorkflowID:        kiroSpecLoopID,
				Feature:           feature,
				TerminationReason: tt.terminationReason,
				Steps:             []RunResultStep{},
			})

			_, err := extractKiroSpecLoopOutput(context.Background(), cwd, workflow.ChildResult{
				DefinitionID: kiroSpecLoopID,
				RunID:        runID,
			})
			if err == nil {
				t.Fatalf("extractKiroSpecLoopOutput error = nil, want rejection for %q", tt.terminationReason)
			}
			assertErrorContainsAll(
				t,
				err,
				`terminationReason="`+string(tt.terminationReason)+`"`,
				`want "completed"`,
				`feature="`+feature+`"`,
			)
		})
	}
}

// TestSpecImplLoopCompletionGateAcceptsCompletedIgnoringValidationFeedback は
// validationFeedback[] を読まなくても completed gate を通過できることを固定する (Req 6.5)。
func TestSpecImplLoopCompletionGateAcceptsCompletedIgnoringValidationFeedback(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	runID := "spec-run-with-feedback"
	writeSpecLoopResult(t, cwd, runID, RunResult{
		RunID:             runID,
		WorkflowID:        kiroSpecLoopID,
		Feature:           "demo-feature",
		TerminationReason: RunReasonCompleted,
		Steps:             []RunResultStep{},
		ValidationFeedback: []RunResultValidationFeedback{
			{
				Source:       string(ValidationFeedbackSourceValidateDesign),
				Order:        5,
				Phase:        "validate-design",
				Attempt:      1,
				Verdict:      "go",
				VerdictClass: string(ValidationVerdictClassAdvisory),
				RetryAction:  string(ValidationRetryActionDegradeAndContinue),
			},
		},
	})

	output, err := extractKiroSpecLoopOutput(context.Background(), cwd, workflow.ChildResult{
		DefinitionID: kiroSpecLoopID,
		RunID:        runID,
	})
	if err != nil {
		t.Fatalf("extractKiroSpecLoopOutput with validationFeedback: %v", err)
	}
	if output.Feature != "demo-feature" {
		t.Fatalf("feature = %q, want demo-feature", output.Feature)
	}
}
