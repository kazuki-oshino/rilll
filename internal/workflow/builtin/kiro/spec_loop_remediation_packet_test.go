package kiro

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/inquiryloop"
)

func TestParseValidationRemediationPacketBypassesCleanGo(t *testing.T) {
	packet, err := parseValidationRemediationPacket(inquiryloop.FinalResult{
		Verdict:     inquiryloop.VerdictGo,
		HasAdvisory: false,
		Summary:     "design is ready",
	}, 1)
	if err != nil {
		t.Fatalf("parseValidationRemediationPacket clean go returned error: %v", err)
	}
	if packet != nil {
		t.Fatalf("clean go should bypass remediation packet parse, got %#v", packet)
	}
}

func TestParseValidationRemediationPacketRejectsMalformedRequiredFields(t *testing.T) {
	base := map[string]any{
		"blocker_category":    "cross_spec_contract",
		"blocker_fingerprint": "combat-domain-turn-contract-envelope",
		"affected_artifacts":  []any{".kiro/specs/demo/design.md"},
		"recommended_action":  "revise_current_spec",
		"owner_boundary":      "current_spec",
	}

	tests := []struct {
		name       string
		mutate     func(map[string]any)
		wantDetail string
	}{
		{
			name:       "missing category",
			mutate:     func(m map[string]any) { delete(m, "blocker_category") },
			wantDetail: "blocker_category",
		},
		{
			name:       "invalid category enum",
			mutate:     func(m map[string]any) { m["blocker_category"] = "architecture_gap" },
			wantDetail: "blocker_category",
		},
		{
			name:       "missing fingerprint",
			mutate:     func(m map[string]any) { delete(m, "blocker_fingerprint") },
			wantDetail: "blocker_fingerprint",
		},
		{
			name:       "natural language fingerprint",
			mutate:     func(m map[string]any) { m["blocker_fingerprint"] = "Combat domain turn contract envelope is missing." },
			wantDetail: "blocker_fingerprint",
		},
		{
			name:       "missing affected artifacts",
			mutate:     func(m map[string]any) { delete(m, "affected_artifacts") },
			wantDetail: "affected_artifacts",
		},
		{
			name:       "empty affected artifacts",
			mutate:     func(m map[string]any) { m["affected_artifacts"] = []any{} },
			wantDetail: "affected_artifacts",
		},
		{
			name:       "invalid recommended action enum",
			mutate:     func(m map[string]any) { m["recommended_action"] = "rewrite_upstream_spec" },
			wantDetail: "recommended_action",
		},
		{
			name:       "invalid owner boundary enum",
			mutate:     func(m map[string]any) { m["owner_boundary"] = "external_project" },
			wantDetail: "owner_boundary",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			extensions := cloneTestExtensions(base)
			tt.mutate(extensions)
			_, err := parseValidationRemediationPacket(inquiryloop.FinalResult{
				Verdict:     inquiryloop.VerdictNoGo,
				Summary:     "design still has a blocker",
				Extensions:  extensions,
				HasAdvisory: false,
			}, 2)
			if err == nil {
				t.Fatalf("expected schema violation")
			}
			if !errors.Is(err, inquiryloop.ErrSchemaViolation) {
				t.Fatalf("expected ErrSchemaViolation chain, got %v", err)
			}
			if tt.wantDetail != "" && !strings.Contains(err.Error(), tt.wantDetail) {
				t.Fatalf("error %q should mention %q", err.Error(), tt.wantDetail)
			}
		})
	}
}

func TestParseValidationRemediationPacketBuildsCurrentSpecNoGo(t *testing.T) {
	packet, err := parseValidationRemediationPacket(inquiryloop.FinalResult{
		Verdict:     inquiryloop.VerdictNoGo,
		Summary:     "Design omits the shared turn contract.",
		HasAdvisory: true,
		Advisory:    "Add the shared turn contract envelope to design.md.",
		Extensions: map[string]any{
			"blocker_category":    "cross_spec_contract",
			"blocker_fingerprint": "combat-domain-turn-contract-envelope",
			"affected_artifacts":  []any{".kiro/specs/demo/design.md", ".kiro/specs/demo/requirements.md"},
			"recommended_action":  "revise_current_spec",
			"owner_boundary":      "current_spec",
		},
	}, 2)
	if err != nil {
		t.Fatalf("parseValidationRemediationPacket returned error: %v", err)
	}
	if packet == nil {
		t.Fatalf("packet is nil")
	}

	want := &ValidationRemediationPacket{
		Source:            ValidationFeedbackSourceValidateDesign,
		VerdictClass:      ValidationVerdictClassCritical,
		Category:          ValidationBlockerCategoryCrossSpecContract,
		Fingerprint:       "combat-domain-turn-contract-envelope",
		StableKey:         "critical:cross_spec_contract:current_spec::combat-domain-turn-contract-envelope",
		AffectedArtifacts: []string{".kiro/specs/demo/design.md", ".kiro/specs/demo/requirements.md"},
		RecommendedAction: ValidationRecommendedActionReviseCurrentSpec,
		OwnerBoundary:     ValidationOwnerBoundaryCurrentSpec,
		Summary:           "Design omits the shared turn contract.",
		Advisory:          "Add the shared turn contract envelope to design.md.",
		Attempt:           2,
	}
	if !reflect.DeepEqual(packet, want) {
		t.Fatalf("packet mismatch:\n got=%#v\nwant=%#v", packet, want)
	}
}

func TestParseValidationRemediationPacketBuildsAdvisoryPacket(t *testing.T) {
	packet, err := parseValidationRemediationPacket(inquiryloop.FinalResult{
		Verdict:     inquiryloop.VerdictGo,
		Summary:     "Design is acceptable with minor follow-up.",
		HasAdvisory: true,
		Advisory:    "Add a test note for the retry prompt.",
		Extensions: map[string]any{
			"blocker_category":    "testing_gap",
			"blocker_fingerprint": "design-test-coverage-gap",
			"affected_artifacts":  []string{".kiro/specs/demo/design.md"},
			"recommended_action":  "record_advisory",
			"owner_boundary":      "current_spec",
			"owner_spec":          "kiro-spec-loop-remediation-routing",
		},
	}, 3)
	if err != nil {
		t.Fatalf("parseValidationRemediationPacket returned error: %v", err)
	}
	if packet == nil {
		t.Fatalf("packet is nil")
	}
	if packet.VerdictClass != ValidationVerdictClassAdvisory {
		t.Fatalf("VerdictClass=%q, want %q", packet.VerdictClass, ValidationVerdictClassAdvisory)
	}
	if packet.StableKey != "advisory:testing_gap:current_spec:kiro-spec-loop-remediation-routing:design-test-coverage-gap" {
		t.Fatalf("StableKey=%q", packet.StableKey)
	}
	if packet.RecommendedAction != ValidationRecommendedActionRecordAdvisory {
		t.Fatalf("RecommendedAction=%q", packet.RecommendedAction)
	}
	if packet.OwnerSpec != "kiro-spec-loop-remediation-routing" {
		t.Fatalf("OwnerSpec=%q", packet.OwnerSpec)
	}
	if !reflect.DeepEqual(packet.AffectedArtifacts, []string{".kiro/specs/demo/design.md"}) {
		t.Fatalf("AffectedArtifacts=%v", packet.AffectedArtifacts)
	}
}

func cloneTestExtensions(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
