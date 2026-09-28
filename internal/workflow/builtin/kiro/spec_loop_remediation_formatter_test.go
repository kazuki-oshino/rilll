package kiro

import (
	"testing"

	"github.com/kazuki-oshino/rilll/internal/facet"
)

func TestFormatValidationRemediationPreviousResponseIncludesPacketFields(t *testing.T) {
	packet := testOwnershipPacket()
	packet.Advisory = "Add the shared turn contract envelope to design.md."

	got := formatValidationRemediationPreviousResponse(packet, nil)
	want := `# Validation Remediation

- Source: validate-design
- Verdict Class: critical
- Blocker: cross_spec_contract / shared-contract-gap
- Owner Boundary: current_spec
- Owner Spec: (none)
- Affected Artifacts:
  - .kiro/specs/demo-feature/design.md
- Recommended Action: revise_current_spec

## Feedback

### Summary

Design references a mismatched shared contract.

### Advisory

Add the shared turn contract envelope to design.md.
`
	if got != want {
		t.Fatalf("formatValidationRemediationPreviousResponse mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestFormatValidationRemediationPreviousResponseIncludesDeterministicSignals(t *testing.T) {
	packet := testOwnershipPacket()
	packet.OwnerBoundary = ValidationOwnerBoundaryUpstreamSpec
	packet.OwnerSpec = "shared-contracts"
	packet.RecommendedAction = ValidationRecommendedActionConfirmUpstreamContract
	packet.AffectedArtifacts = []string{
		".kiro/specs/shared-contracts/design.md",
		".kiro/specs/demo-feature/design.md",
	}
	packet.StableKey = validationRemediationStableKey(&packet)
	signals := []ValidationDeterministicSignal{{
		Kind:                  ValidationDeterministicSignalAffectedArtifactOutsideCurrentSpec,
		Artifact:              ".kiro/specs/shared-contracts/design.md",
		Message:               "affected_artifacts points to another spec; routing requires upstream owner confirmation",
		DetectedOwnerBoundary: ValidationOwnerBoundaryUpstreamSpec,
		DetectedOwnerSpec:     "shared-contracts",
	}}

	got := formatValidationRemediationPreviousResponse(packet, signals)
	want := `# Validation Remediation

- Source: validate-design
- Verdict Class: critical
- Blocker: cross_spec_contract / shared-contract-gap
- Owner Boundary: upstream_spec
- Owner Spec: shared-contracts
- Affected Artifacts:
  - .kiro/specs/shared-contracts/design.md
  - .kiro/specs/demo-feature/design.md
- Recommended Action: confirm_upstream_contract

## Deterministic Signals

- Kind: affected_artifact_outside_current_spec
  - Artifact: .kiro/specs/shared-contracts/design.md
  - Message: affected_artifacts points to another spec; routing requires upstream owner confirmation
  - Detected Owner Boundary: upstream_spec
  - Detected Owner Spec: shared-contracts

## Feedback

### Summary

Design references a mismatched shared contract.
`
	if got != want {
		t.Fatalf("formatValidationRemediationPreviousResponse with signals mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestValidationRemediationPreviousResponseRemainsDistinctInsidePreviousResponseBlock(t *testing.T) {
	packet := testOwnershipPacket()
	remediation := formatValidationRemediationPreviousResponse(packet, nil)

	got := previousResponseBlock(remediation, facet.PromptContextDiagnostic)
	want := "\n\n---\n\n# Previous Response\n\n" + remediation
	if got != want {
		t.Fatalf("previous response wrapper mismatch:\n got=%q\nwant=%q", got, want)
	}
}
