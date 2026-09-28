package kiro

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/inquiryloop"
)

func TestValidateRemediationOwnershipAcceptsCurrentSpecOwner(t *testing.T) {
	tests := []struct {
		name          string
		ownerSpec     string
		wantStableKey string
	}{
		{
			name:          "empty owner spec",
			wantStableKey: "critical:cross_spec_contract:current_spec::shared-contract-gap",
		},
		{
			name:          "current feature owner spec",
			ownerSpec:     "demo-feature",
			wantStableKey: "critical:cross_spec_contract:current_spec:demo-feature:shared-contract-gap",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			packet := testOwnershipPacket()
			packet.OwnerSpec = tt.ownerSpec
			packet.AffectedArtifacts = []string{
				".kiro/specs/demo-feature/design.md",
				"requirements.md",
			}

			got, signals, err := validateRemediationOwnership(t.TempDir(), "demo-feature", packet)
			if err != nil {
				t.Fatalf("validateRemediationOwnership returned error: %v", err)
			}
			if len(signals) != 0 {
				t.Fatalf("signals=%#v, want none", signals)
			}
			if got.OwnerBoundary != ValidationOwnerBoundaryCurrentSpec {
				t.Fatalf("OwnerBoundary=%q, want %q", got.OwnerBoundary, ValidationOwnerBoundaryCurrentSpec)
			}
			if got.OwnerSpec != tt.ownerSpec {
				t.Fatalf("OwnerSpec=%q, want %q", got.OwnerSpec, tt.ownerSpec)
			}
			if got.StableKey != tt.wantStableKey {
				t.Fatalf("StableKey=%q", got.StableKey)
			}
		})
	}
}

func TestValidateRemediationOwnershipRejectsInvalidOwnerSpec(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ValidationRemediationPacket)
		wantErr string
	}{
		{
			name: "current spec cannot name different owner",
			mutate: func(packet *ValidationRemediationPacket) {
				packet.OwnerBoundary = ValidationOwnerBoundaryCurrentSpec
				packet.OwnerSpec = "upstream-feature"
			},
			wantErr: "current_spec",
		},
		{
			name: "upstream owner spec is required",
			mutate: func(packet *ValidationRemediationPacket) {
				packet.OwnerBoundary = ValidationOwnerBoundaryUpstreamSpec
				packet.OwnerSpec = ""
			},
			wantErr: "owner_spec",
		},
		{
			name: "upstream owner spec cannot be current feature",
			mutate: func(packet *ValidationRemediationPacket) {
				packet.OwnerBoundary = ValidationOwnerBoundaryUpstreamSpec
				packet.OwnerSpec = "demo-feature"
			},
			wantErr: "upstream_spec",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			packet := testOwnershipPacket()
			tt.mutate(&packet)

			_, _, err := validateRemediationOwnership(t.TempDir(), "demo-feature", packet)
			if err == nil {
				t.Fatalf("expected schema violation")
			}
			if !errors.Is(err, inquiryloop.ErrSchemaViolation) {
				t.Fatalf("expected ErrSchemaViolation chain, got %v", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q should mention %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestValidateRemediationOwnershipAcceptsUpstreamSpecOwner(t *testing.T) {
	packet := testOwnershipPacket()
	packet.OwnerBoundary = ValidationOwnerBoundaryUpstreamSpec
	packet.OwnerSpec = "shared-contracts"
	packet.RecommendedAction = ValidationRecommendedActionConfirmUpstreamContract

	got, signals, err := validateRemediationOwnership(t.TempDir(), "demo-feature", packet)
	if err != nil {
		t.Fatalf("validateRemediationOwnership returned error: %v", err)
	}
	if len(signals) != 0 {
		t.Fatalf("signals=%#v, want none", signals)
	}
	if got.OwnerBoundary != ValidationOwnerBoundaryUpstreamSpec {
		t.Fatalf("OwnerBoundary=%q, want upstream", got.OwnerBoundary)
	}
	if got.OwnerSpec != "shared-contracts" {
		t.Fatalf("OwnerSpec=%q, want upstream feature", got.OwnerSpec)
	}
	if got.StableKey != "critical:cross_spec_contract:upstream_spec:shared-contracts:shared-contract-gap" {
		t.Fatalf("StableKey=%q", got.StableKey)
	}
}

func TestValidateRemediationOwnershipConvertsCurrentSpecPacketPointingAtOtherSpec(t *testing.T) {
	packet := testOwnershipPacket()
	packet.AffectedArtifacts = []string{
		".kiro/specs/shared-contracts/design.md",
		".kiro/specs/demo-feature/design.md",
	}

	got, signals, err := validateRemediationOwnership(t.TempDir(), "demo-feature", packet)
	if err != nil {
		t.Fatalf("validateRemediationOwnership returned error: %v", err)
	}
	if got.OwnerBoundary != ValidationOwnerBoundaryUpstreamSpec {
		t.Fatalf("OwnerBoundary=%q, want upstream", got.OwnerBoundary)
	}
	if got.OwnerSpec != "shared-contracts" {
		t.Fatalf("OwnerSpec=%q, want detected upstream feature", got.OwnerSpec)
	}
	if got.RecommendedAction != ValidationRecommendedActionConfirmUpstreamContract {
		t.Fatalf("RecommendedAction=%q, want %q", got.RecommendedAction, ValidationRecommendedActionConfirmUpstreamContract)
	}
	if got.StableKey != "critical:cross_spec_contract:upstream_spec:shared-contracts:shared-contract-gap" {
		t.Fatalf("StableKey=%q", got.StableKey)
	}

	wantSignals := []ValidationDeterministicSignal{{
		Kind:                  ValidationDeterministicSignalAffectedArtifactOutsideCurrentSpec,
		Artifact:              ".kiro/specs/shared-contracts/design.md",
		Message:               "affected_artifacts points to another spec; routing requires upstream owner confirmation",
		DetectedOwnerBoundary: ValidationOwnerBoundaryUpstreamSpec,
		DetectedOwnerSpec:     "shared-contracts",
	}}
	if !reflect.DeepEqual(signals, wantSignals) {
		t.Fatalf("signals mismatch:\n got=%#v\nwant=%#v", signals, wantSignals)
	}
}

func testOwnershipPacket() ValidationRemediationPacket {
	packet := ValidationRemediationPacket{
		Source:            ValidationFeedbackSourceValidateDesign,
		VerdictClass:      ValidationVerdictClassCritical,
		Category:          ValidationBlockerCategoryCrossSpecContract,
		Fingerprint:       "shared-contract-gap",
		AffectedArtifacts: []string{".kiro/specs/demo-feature/design.md"},
		RecommendedAction: ValidationRecommendedActionReviseCurrentSpec,
		OwnerBoundary:     ValidationOwnerBoundaryCurrentSpec,
		Summary:           "Design references a mismatched shared contract.",
		Attempt:           1,
	}
	packet.StableKey = validationRemediationStableKey(&packet)
	return packet
}
