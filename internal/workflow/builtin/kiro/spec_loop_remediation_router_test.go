package kiro

import (
	"strings"
	"testing"
)

func TestRouteValidationRemediationTable(t *testing.T) {
	t.Parallel()

	keyA := "critical:cross_spec_contract:current_spec::fingerprint-a"
	keyB := "critical:boundary_violation:current_spec::fingerprint-b"
	advisoryKey := "advisory:testing_gap:current_spec::gap-one"
	upstreamKey := "critical:cross_spec_contract:upstream_spec:upstream-feature:upstream-contract"

	currentCriticalPacket := func(stableKey string) ValidationRemediationPacket {
		return ValidationRemediationPacket{
			VerdictClass:      ValidationVerdictClassCritical,
			Category:          ValidationBlockerCategoryCrossSpecContract,
			Fingerprint:       strings.TrimPrefix(stableKey, "critical:cross_spec_contract:current_spec::"),
			StableKey:         stableKey,
			AffectedArtifacts: []string{".kiro/specs/demo/design.md"},
			RecommendedAction: ValidationRecommendedActionReviseCurrentSpec,
			OwnerBoundary:     ValidationOwnerBoundaryCurrentSpec,
		}
	}
	advisoryPacket := ValidationRemediationPacket{
		VerdictClass:      ValidationVerdictClassAdvisory,
		Category:          ValidationBlockerCategoryTestingGap,
		Fingerprint:       "gap-one",
		StableKey:         advisoryKey,
		AffectedArtifacts: []string{".kiro/specs/demo/design.md"},
		RecommendedAction: ValidationRecommendedActionRecordAdvisory,
		OwnerBoundary:     ValidationOwnerBoundaryCurrentSpec,
	}
	upstreamPacket := ValidationRemediationPacket{
		VerdictClass:      ValidationVerdictClassCritical,
		Category:          ValidationBlockerCategoryCrossSpecContract,
		Fingerprint:       "upstream-contract",
		StableKey:         upstreamKey,
		AffectedArtifacts: []string{".kiro/specs/upstream-feature/design.md"},
		RecommendedAction: ValidationRecommendedActionConfirmUpstreamContract,
		OwnerBoundary:     ValidationOwnerBoundaryUpstreamSpec,
		OwnerSpec:         "upstream-feature",
	}

	tests := []struct {
		name       string
		setup      func(*validationBlockerRetryState)
		packet     ValidationRemediationPacket
		wantAction ValidationRetryAction
		wantTerm   bool
		wantAbort  bool
		wantReason RunTerminationReason
		wantSame   int
		wantMsgSub string
	}{
		{
			name:       "critical current spec first attempt retries",
			packet:     currentCriticalPacket(keyA),
			wantAction: ValidationRetryActionRetrySpecDesign,
			wantSame:   1,
		},
		{
			name: "critical same blocker second attempt retries",
			setup: func(s *validationBlockerRetryState) {
				s.observeBlocker(ValidationVerdictClassCritical, keyA)
			},
			packet:     currentCriticalPacket(keyA),
			wantAction: ValidationRetryActionRetrySpecDesign,
			wantSame:   2,
		},
		{
			name: "critical same blocker third attempt terminal validation-no-go",
			setup: func(s *validationBlockerRetryState) {
				s.observeBlocker(ValidationVerdictClassCritical, keyA)
				s.observeBlocker(ValidationVerdictClassCritical, keyA)
			},
			packet:     currentCriticalPacket(keyA),
			wantAction: ValidationRetryActionStopForHuman,
			wantTerm:   true,
			wantAbort:  true,
			wantReason: RunReasonValidationNoGo,
			wantSame:   3,
			wantMsgSub: "fingerprint-a",
		},
		{
			name: "critical different blocker resets repeat count",
			setup: func(s *validationBlockerRetryState) {
				s.observeBlocker(ValidationVerdictClassCritical, keyA)
				s.observeBlocker(ValidationVerdictClassCritical, keyA)
			},
			packet:     currentCriticalPacket(keyB),
			wantAction: ValidationRetryActionRetrySpecDesign,
			wantSame:   1,
		},
		{
			name: "critical total cap stops with last blocker in message",
			setup: func(s *validationBlockerRetryState) {
				for i := 0; i < maxCriticalFeedbackAttempts-1; i++ {
					s.observeBlocker(ValidationVerdictClassCritical, "critical:testing_gap:current_spec::fresh-"+string(rune('a'+i)))
				}
			},
			packet:     currentCriticalPacket(keyB),
			wantAction: ValidationRetryActionStopForHuman,
			wantTerm:   true,
			wantAbort:  true,
			wantReason: RunReasonValidationNoGo,
			wantSame:   1,
			wantMsgSub: "fingerprint-b",
		},
		{
			name:       "advisory first attempt retries without touching critical state",
			packet:     advisoryPacket,
			wantAction: ValidationRetryActionRetrySpecDesign,
			wantSame:   1,
		},
		{
			name: "advisory same blocker third attempt degrades and continues",
			setup: func(s *validationBlockerRetryState) {
				s.observeBlocker(ValidationVerdictClassAdvisory, advisoryKey)
				s.observeBlocker(ValidationVerdictClassAdvisory, advisoryKey)
			},
			packet:     advisoryPacket,
			wantAction: ValidationRetryActionDegradeAndContinue,
			wantTerm:   true,
			wantSame:   3,
			wantMsgSub: "gap-one",
		},
		{
			name: "advisory total cap degrades with last blocker in message",
			setup: func(s *validationBlockerRetryState) {
				for i := 0; i < maxAdvisoryFeedbackAttempts-1; i++ {
					s.observeBlocker(ValidationVerdictClassAdvisory, "advisory:testing_gap:current_spec::fresh-"+string(rune('a'+i)))
				}
			},
			packet:     advisoryPacket,
			wantAction: ValidationRetryActionDegradeAndContinue,
			wantTerm:   true,
			wantSame:   1,
			wantMsgSub: "gap-one",
		},
		{
			name:       "upstream owner stops without retry regardless of repeat count",
			packet:     upstreamPacket,
			wantAction: ValidationRetryActionStopForHuman,
			wantTerm:   true,
			wantAbort:  true,
			wantReason: RunReasonValidationNoGo,
			wantSame:   1,
			wantMsgSub: "upstream-feature",
		},
		{
			name: "upstream owner stops even after prior critical retries",
			setup: func(s *validationBlockerRetryState) {
				s.observeBlocker(ValidationVerdictClassCritical, keyA)
				s.observeBlocker(ValidationVerdictClassCritical, keyA)
			},
			packet:     upstreamPacket,
			wantAction: ValidationRetryActionStopForHuman,
			wantTerm:   true,
			wantAbort:  true,
			wantReason: RunReasonValidationNoGo,
			wantSame:   1,
			wantMsgSub: "upstream owner",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var state validationBlockerRetryState
			if tc.setup != nil {
				tc.setup(&state)
			}

			decision := routeValidationRemediation(&state, tc.packet)

			if decision.RetryAction != tc.wantAction {
				t.Fatalf("RetryAction = %q, want %q", decision.RetryAction, tc.wantAction)
			}
			if decision.Terminal != tc.wantTerm {
				t.Fatalf("Terminal = %v, want %v", decision.Terminal, tc.wantTerm)
			}
			if decision.Abort != tc.wantAbort {
				t.Fatalf("Abort = %v, want %v", decision.Abort, tc.wantAbort)
			}
			if tc.wantReason != "" && decision.TerminationReason != tc.wantReason {
				t.Fatalf("TerminationReason = %q, want %q", decision.TerminationReason, tc.wantReason)
			}
			if decision.SameBlockerCount != tc.wantSame {
				t.Fatalf("SameBlockerCount = %d, want %d", decision.SameBlockerCount, tc.wantSame)
			}
			if tc.wantMsgSub != "" && !strings.Contains(decision.Message, tc.wantMsgSub) {
				t.Fatalf("Message = %q, want substring %q", decision.Message, tc.wantMsgSub)
			}
		})
	}
}

func TestRouteValidationRemediationRecordsFeedbackTrace(t *testing.T) {
	state := &runState{}
	packet := ValidationRemediationPacket{
		Source:            ValidationFeedbackSourceValidateDesign,
		VerdictClass:      ValidationVerdictClassCritical,
		Category:          ValidationBlockerCategoryTestingGap,
		Fingerprint:       "missing-tests",
		StableKey:         "critical:testing_gap:current_spec::missing-tests",
		AffectedArtifacts: []string{".kiro/specs/demo/design.md"},
		RecommendedAction: ValidationRecommendedActionReviseCurrentSpec,
		OwnerBoundary:     ValidationOwnerBoundaryCurrentSpec,
		Attempt:           1,
	}

	decision := state.routeValidationRemediation(5, "validate-design", packet, nil, "no_go", "")
	if decision.RetryAction != ValidationRetryActionRetrySpecDesign {
		t.Fatalf("RetryAction = %q, want retry_spec_design", decision.RetryAction)
	}
	if len(state.ValidationFeedback) != 1 {
		t.Fatalf("ValidationFeedback len = %d, want 1", len(state.ValidationFeedback))
	}
	got := state.ValidationFeedback[0]
	if got.RetryAction != string(ValidationRetryActionRetrySpecDesign) {
		t.Fatalf("feedback retryAction = %q", got.RetryAction)
	}
	if got.SameBlockerCount != 1 {
		t.Fatalf("feedback sameBlockerCount = %d, want 1", got.SameBlockerCount)
	}
}

func TestRouteValidationRemediationAdvisoryDoesNotConsumeCriticalState(t *testing.T) {
	state := &runState{}
	critical := ValidationRemediationPacket{
		VerdictClass: ValidationVerdictClassCritical,
		StableKey:    "critical:testing_gap:current_spec::gap-a",
		Fingerprint:  "gap-a",
		Category:     ValidationBlockerCategoryTestingGap,
	}
	advisory := ValidationRemediationPacket{
		VerdictClass: ValidationVerdictClassAdvisory,
		StableKey:    "advisory:testing_gap:current_spec::gap-b",
		Fingerprint:  "gap-b",
		Category:     ValidationBlockerCategoryTestingGap,
	}

	state.routeValidationRemediation(5, "validate-design", critical, nil, "no_go", "")
	state.routeValidationRemediation(5, "validate-design", advisory, nil, "go", "")

	if state.BlockerRetry.CriticalRepeatCount != 1 {
		t.Fatalf("CriticalRepeatCount = %d, want 1", state.BlockerRetry.CriticalRepeatCount)
	}
	if state.BlockerRetry.AdvisoryRepeatCount != 1 {
		t.Fatalf("AdvisoryRepeatCount = %d, want 1", state.BlockerRetry.AdvisoryRepeatCount)
	}
}
