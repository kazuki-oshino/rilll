package kiro

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/workflow"
)

func TestValidationBlockerRetryStateTracksSameAndNewBlockers(t *testing.T) {
	var state validationBlockerRetryState
	keyA := "critical:cross_spec_contract:current_spec::fingerprint-a"
	keyB := "critical:boundary_violation:current_spec::fingerprint-b"
	advisoryKey := "advisory:testing_gap:current_spec::gap-one"

	count := state.observeBlocker(ValidationVerdictClassCritical, keyA)
	if count != 1 || state.LastCriticalKey != keyA || state.CriticalRepeatCount != 1 || state.CriticalTotalAttempts != 1 {
		t.Fatalf("first critical blocker: count=%d state=%+v", count, state)
	}

	count = state.observeBlocker(ValidationVerdictClassCritical, keyA)
	if count != 2 || state.CriticalRepeatCount != 2 || state.CriticalTotalAttempts != 2 {
		t.Fatalf("same critical blocker: count=%d state=%+v", count, state)
	}

	count = state.observeBlocker(ValidationVerdictClassCritical, keyB)
	if count != 1 || state.LastCriticalKey != keyB || state.CriticalRepeatCount != 1 || state.CriticalTotalAttempts != 3 {
		t.Fatalf("new critical blocker resets repeat count: count=%d state=%+v", count, state)
	}

	advCount := state.observeBlocker(ValidationVerdictClassAdvisory, advisoryKey)
	if advCount != 1 || state.LastAdvisoryKey != advisoryKey || state.AdvisoryRepeatCount != 1 || state.AdvisoryTotalAttempts != 1 {
		t.Fatalf("advisory uses separate state: advCount=%d state=%+v", advCount, state)
	}
	if state.CriticalRepeatCount != 1 {
		t.Fatalf("advisory observation must not mutate critical repeat count: state=%+v", state)
	}
}

func TestBuildRunResultProjectsValidationFeedback(t *testing.T) {
	blocker := &RunResultValidationBlocker{
		Category:          "cross_spec_contract",
		Fingerprint:       "combat-domain-turn-contract-envelope",
		StableKey:         "critical:cross_spec_contract:current_spec::combat-domain-turn-contract-envelope",
		AffectedArtifacts: []string{".kiro/specs/demo/design.md"},
		RecommendedAction: "revise_current_spec",
		OwnerBoundary:     "current_spec",
	}
	state := &runState{
		RunID:       "run-1",
		Feature:     "demo",
		Termination: RunReasonCompleted,
		ValidationFeedback: []RunResultValidationFeedback{
			{
				Source:           string(ValidationFeedbackSourceValidateDesign),
				Order:            5,
				Phase:            "validate-design",
				Attempt:          2,
				Verdict:          "no_go",
				VerdictClass:     string(ValidationVerdictClassCritical),
				RetryAction:      string(ValidationRetryActionRetrySpecDesign),
				SameBlockerCount: 1,
				Blocker:          blocker,
			},
		},
		Advisories: []RunResultAdvisory{
			{Order: 5, Phase: "validate-design", Attempt: 2, Verdict: "no_go", Advisory: "legacy advisory projection"},
		},
	}

	rr := buildRunResult(state, time.Now(), nil, "")
	if len(rr.ValidationFeedback) != 1 {
		t.Fatalf("ValidationFeedback len = %d, want 1", len(rr.ValidationFeedback))
	}
	got := rr.ValidationFeedback[0]
	if got.Source != string(ValidationFeedbackSourceValidateDesign) {
		t.Fatalf("source = %q, want validate-design", got.Source)
	}
	if got.RetryAction != string(ValidationRetryActionRetrySpecDesign) {
		t.Fatalf("retryAction = %q, want retry_spec_design", got.RetryAction)
	}
	if got.Blocker == nil || got.Blocker.Fingerprint != blocker.Fingerprint {
		t.Fatalf("blocker projection missing: %#v", got.Blocker)
	}
	if len(rr.Advisories) != 1 {
		t.Fatalf("Advisories len = %d, want 1 (compat projection preserved)", len(rr.Advisories))
	}
}

func TestRunResultValidationFeedbackMarshalRoundTrip(t *testing.T) {
	rr := RunResult{
		RunID:             "run-marshal",
		WorkflowID:        "kiro-spec-loop",
		Feature:           "demo",
		TerminationReason: RunReasonValidationNoGo,
		Steps:             []RunResultStep{},
		ValidationFeedback: []RunResultValidationFeedback{
			{
				Source:           string(ValidationFeedbackSourceValidateDesign),
				Order:            5,
				Phase:            "validate-design",
				Attempt:          3,
				Verdict:          "no_go",
				VerdictClass:     string(ValidationVerdictClassCritical),
				RetryAction:      string(ValidationRetryActionStopForHuman),
				SameBlockerCount: 3,
				Terminal:           true,
				Blocker: &RunResultValidationBlocker{
					Category:          "cross_spec_contract",
					Fingerprint:       "upstream-contract",
					StableKey:         "critical:cross_spec_contract:upstream_spec:upstream-feature:upstream-contract",
					AffectedArtifacts: []string{".kiro/specs/upstream-feature/design.md"},
					RecommendedAction: "confirm_upstream_contract",
					OwnerBoundary:     "upstream_spec",
					OwnerSpec:         "upstream-feature",
				},
				DeterministicSignals: []RunResultValidationSignal{
					{
						Kind:                  string(ValidationDeterministicSignalAffectedArtifactOutsideCurrentSpec),
						Artifact:              ".kiro/specs/upstream-feature/design.md",
						Message:               "affected artifact outside current spec",
						DetectedOwnerBoundary: string(ValidationOwnerBoundaryUpstreamSpec),
						DetectedOwnerSpec:     "upstream-feature",
					},
				},
			},
		},
	}

	raw, err := json.MarshalIndent(rr, "", "  ")
	if err != nil {
		t.Fatalf("marshal RunResult: %v", err)
	}

	var decoded RunResult
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal RunResult: %v\nraw=%s", err, string(raw))
	}
	if len(decoded.ValidationFeedback) != 1 {
		t.Fatalf("decoded ValidationFeedback len = %d, want 1", len(decoded.ValidationFeedback))
	}
	got := decoded.ValidationFeedback[0]
	if got.Terminal != true || got.SameBlockerCount != 3 {
		t.Fatalf("terminal/sameBlockerCount not preserved: %#v", got)
	}
	if got.Blocker == nil || got.Blocker.OwnerSpec != "upstream-feature" {
		t.Fatalf("blocker not preserved: %#v", got.Blocker)
	}
	if len(got.DeterministicSignals) != 1 || got.DeterministicSignals[0].DetectedOwnerSpec != "upstream-feature" {
		t.Fatalf("deterministic signals not preserved: %#v", got.DeterministicSignals)
	}
	if decoded.TerminationReason != RunReasonValidationNoGo {
		t.Fatalf("existing terminationReason lost: %q", decoded.TerminationReason)
	}
}

func TestSpecImplLoopGateAcceptsResultWithValidationFeedback(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	runID := "spec-run-with-feedback"
	resultPath := filepath.Join(cwd, ".rilll", "kiro-spec-loop", runID, "result.json")
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	rr := RunResult{
		RunID:             runID,
		WorkflowID:        "kiro-spec-loop",
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
	}
	raw, err := json.MarshalIndent(rr, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(resultPath, raw, 0o644); err != nil {
		t.Fatalf("write result.json: %v", err)
	}

	output, err := extractKiroSpecLoopOutput(t.Context(), cwd, workflow.ChildResult{RunID: runID})
	if err != nil {
		t.Fatalf("extractKiroSpecLoopOutput with validationFeedback: %v", err)
	}
	if output.Feature != "demo-feature" {
		t.Fatalf("feature = %q, want demo-feature", output.Feature)
	}
}

func TestProjectRunResultValidationFeedbackFromPacket(t *testing.T) {
	packet := ValidationRemediationPacket{
		Source:            ValidationFeedbackSourceValidateDesign,
		VerdictClass:      ValidationVerdictClassCritical,
		Category:          ValidationBlockerCategoryCrossSpecContract,
		Fingerprint:       "combat-domain-turn-contract-envelope",
		StableKey:         "critical:cross_spec_contract:current_spec::combat-domain-turn-contract-envelope",
		AffectedArtifacts: []string{".kiro/specs/demo/design.md"},
		RecommendedAction: ValidationRecommendedActionReviseCurrentSpec,
		OwnerBoundary:     ValidationOwnerBoundaryCurrentSpec,
		Attempt:           2,
	}
	signals := []ValidationDeterministicSignal{
		{
			Kind:                  ValidationDeterministicSignalAffectedArtifactOutsideCurrentSpec,
			Artifact:              ".kiro/specs/other/design.md",
			Message:               "outside current spec",
			DetectedOwnerBoundary: ValidationOwnerBoundaryUpstreamSpec,
			DetectedOwnerSpec:     "other",
		},
	}

	feedback := buildRunResultValidationFeedback(
		5,
		"validate-design",
		packet,
		signals,
		ValidationRetryActionRetrySpecDesign,
		1,
		false,
		"no_go",
		"",
	)

	if feedback.Source != string(ValidationFeedbackSourceValidateDesign) {
		t.Fatalf("source = %q", feedback.Source)
	}
	if feedback.Blocker == nil || feedback.Blocker.StableKey != packet.StableKey {
		t.Fatalf("blocker projection = %#v", feedback.Blocker)
	}
	if len(feedback.DeterministicSignals) != 1 {
		t.Fatalf("deterministic signals = %#v", feedback.DeterministicSignals)
	}
}

func TestRecordValidationFeedbackUpdatesRetryState(t *testing.T) {
	state := &runState{}
	packet := ValidationRemediationPacket{
		VerdictClass: ValidationVerdictClassCritical,
		StableKey:    "critical:testing_gap:current_spec::missing-tests",
		Attempt:      1,
		Category:     ValidationBlockerCategoryTestingGap,
		Fingerprint:  "missing-tests",
	}

	state.recordValidationFeedback(5, "validate-design", packet, nil, ValidationRetryActionRetrySpecDesign, false, "no_go", "")
	state.recordValidationFeedback(5, "validate-design", packet, nil, ValidationRetryActionRetrySpecDesign, false, "no_go", "")

	if len(state.ValidationFeedback) != 2 {
		t.Fatalf("ValidationFeedback len = %d, want 2", len(state.ValidationFeedback))
	}
	if state.ValidationFeedback[1].SameBlockerCount != 2 {
		t.Fatalf("second feedback sameBlockerCount = %d, want 2", state.ValidationFeedback[1].SameBlockerCount)
	}
	if state.BlockerRetry.CriticalTotalAttempts != 2 {
		t.Fatalf("CriticalTotalAttempts = %d, want 2", state.BlockerRetry.CriticalTotalAttempts)
	}
}
