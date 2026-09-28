package kiro

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kazuki-oshino/rilll/internal/inquiryloop"
	"github.com/kazuki-oshino/rilll/internal/provider"
)

type ValidationFeedbackSource string

const (
	ValidationFeedbackSourceValidateDesign ValidationFeedbackSource = "validate-design"
	ValidationFeedbackSourcePreflight      ValidationFeedbackSource = "preflight"
)

type ValidationRetryAction string

const (
	ValidationRetryActionRetrySpecDesign    ValidationRetryAction = "retry_spec_design"
	ValidationRetryActionStopForHuman       ValidationRetryAction = "stop_for_human"
	ValidationRetryActionDegradeAndContinue ValidationRetryAction = "degrade_and_continue"
	ValidationRetryActionContinue           ValidationRetryAction = "continue"
)

const (
	maxSameCriticalBlockerAttempts = 3
	maxSameAdvisoryBlockerAttempts = 3
	maxCriticalFeedbackAttempts    = 9
	maxAdvisoryFeedbackAttempts    = 9
)

type validationRemediationDecision struct {
	RetryAction       ValidationRetryAction
	Terminal          bool
	Abort             bool
	TerminationReason RunTerminationReason
	Message           string
	SameBlockerCount  int
}

// validationBlockerRetryState tracks same/new blocker repetition for validate-design
// feedback separately from preflight retry counters.
type validationBlockerRetryState struct {
	LastCriticalKey       string
	CriticalRepeatCount   int
	LastAdvisoryKey       string
	AdvisoryRepeatCount   int
	CriticalTotalAttempts int
	AdvisoryTotalAttempts int
}

// observeBlocker updates retry state for the given verdict class and stable key,
// returning the same-blocker count after the observation.
func (s *validationBlockerRetryState) observeBlocker(verdictClass ValidationVerdictClass, stableKey string) int {
	switch verdictClass {
	case ValidationVerdictClassCritical:
		if stableKey == s.LastCriticalKey {
			s.CriticalRepeatCount++
		} else {
			s.LastCriticalKey = stableKey
			s.CriticalRepeatCount = 1
		}
		s.CriticalTotalAttempts++
		return s.CriticalRepeatCount
	case ValidationVerdictClassAdvisory:
		if stableKey == s.LastAdvisoryKey {
			s.AdvisoryRepeatCount++
		} else {
			s.LastAdvisoryKey = stableKey
			s.AdvisoryRepeatCount = 1
		}
		s.AdvisoryTotalAttempts++
		return s.AdvisoryRepeatCount
	default:
		return 0
	}
}

func buildPreflightValidationFeedback(
	order int,
	phase string,
	attempt int,
	message string,
	retryAction ValidationRetryAction,
	terminal bool,
) RunResultValidationFeedback {
	return RunResultValidationFeedback{
		Source:      string(ValidationFeedbackSourcePreflight),
		Order:       order,
		Phase:       phase,
		Attempt:     attempt,
		Verdict:     string(inquiryloop.VerdictNoGo),
		RetryAction: string(retryAction),
		Message:     message,
		Terminal:    terminal,
	}
}

func (state *runState) recordPreflightValidationFeedback(
	order int,
	phase string,
	attempt int,
	message string,
	retryAction ValidationRetryAction,
	terminal bool,
) {
	state.ValidationFeedback = append(state.ValidationFeedback, buildPreflightValidationFeedback(
		order,
		phase,
		attempt,
		message,
		retryAction,
		terminal,
	))
}

func buildRunResultValidationFeedback(
	order int,
	phase string,
	packet ValidationRemediationPacket,
	signals []ValidationDeterministicSignal,
	retryAction ValidationRetryAction,
	sameBlockerCount int,
	terminal bool,
	verdict string,
	message string,
) RunResultValidationFeedback {
	feedback := RunResultValidationFeedback{
		Source:           string(packet.Source),
		Order:            order,
		Phase:            phase,
		Attempt:          packet.Attempt,
		Verdict:          verdict,
		VerdictClass:     string(packet.VerdictClass),
		RetryAction:      string(retryAction),
		SameBlockerCount: sameBlockerCount,
		Message:          message,
		Terminal:         terminal,
	}
	if packet.StableKey != "" {
		blocker := projectRunResultValidationBlocker(packet)
		feedback.Blocker = &blocker
	}
	if len(signals) > 0 {
		feedback.DeterministicSignals = projectRunResultValidationSignals(signals)
	}
	return feedback
}

func projectRunResultValidationBlocker(packet ValidationRemediationPacket) RunResultValidationBlocker {
	return RunResultValidationBlocker{
		Category:          string(packet.Category),
		Fingerprint:       packet.Fingerprint,
		StableKey:         packet.StableKey,
		AffectedArtifacts: append([]string(nil), packet.AffectedArtifacts...),
		RecommendedAction: string(packet.RecommendedAction),
		OwnerBoundary:     string(packet.OwnerBoundary),
		OwnerSpec:         packet.OwnerSpec,
	}
}

func projectRunResultValidationSignals(signals []ValidationDeterministicSignal) []RunResultValidationSignal {
	out := make([]RunResultValidationSignal, 0, len(signals))
	for _, signal := range signals {
		out = append(out, RunResultValidationSignal{
			Kind:                  string(signal.Kind),
			Artifact:              signal.Artifact,
			Message:               signal.Message,
			DetectedOwnerBoundary: string(signal.DetectedOwnerBoundary),
			DetectedOwnerSpec:     signal.DetectedOwnerSpec,
		})
	}
	return out
}

func routeValidationRemediation(
	retryState *validationBlockerRetryState,
	packet ValidationRemediationPacket,
) validationRemediationDecision {
	if isUpstreamRemediationStop(packet) {
		sameCount := retryState.observeBlocker(packet.VerdictClass, packet.StableKey)
		return validationRemediationDecision{
			RetryAction:       ValidationRetryActionStopForHuman,
			Terminal:          true,
			Abort:             true,
			TerminationReason: RunReasonValidationNoGo,
			Message:           formatUpstreamRemediationStopMessage(packet),
			SameBlockerCount:  sameCount,
		}
	}

	switch packet.VerdictClass {
	case ValidationVerdictClassCritical:
		return routeCriticalValidationRemediation(retryState, packet)
	case ValidationVerdictClassAdvisory:
		return routeAdvisoryValidationRemediation(retryState, packet)
	default:
		return validationRemediationDecision{
			RetryAction: ValidationRetryActionContinue,
		}
	}
}

func routeCriticalValidationRemediation(
	retryState *validationBlockerRetryState,
	packet ValidationRemediationPacket,
) validationRemediationDecision {
	sameCount := retryState.observeBlocker(packet.VerdictClass, packet.StableKey)
	if sameCount >= maxSameCriticalBlockerAttempts {
		return validationRemediationDecision{
			RetryAction:       ValidationRetryActionStopForHuman,
			Terminal:          true,
			Abort:             true,
			TerminationReason: RunReasonValidationNoGo,
			Message:           formatSameCriticalBlockerStopMessage(packet, sameCount),
			SameBlockerCount:  sameCount,
		}
	}
	if retryState.CriticalTotalAttempts >= maxCriticalFeedbackAttempts {
		return validationRemediationDecision{
			RetryAction:       ValidationRetryActionStopForHuman,
			Terminal:          true,
			Abort:             true,
			TerminationReason: RunReasonValidationNoGo,
			Message:           formatCriticalFeedbackCapStopMessage(packet, retryState.CriticalTotalAttempts),
			SameBlockerCount:  sameCount,
		}
	}
	return validationRemediationDecision{
		RetryAction:      ValidationRetryActionRetrySpecDesign,
		SameBlockerCount: sameCount,
	}
}

func routeAdvisoryValidationRemediation(
	retryState *validationBlockerRetryState,
	packet ValidationRemediationPacket,
) validationRemediationDecision {
	sameCount := retryState.observeBlocker(packet.VerdictClass, packet.StableKey)
	if sameCount >= maxSameAdvisoryBlockerAttempts {
		return validationRemediationDecision{
			RetryAction:      ValidationRetryActionDegradeAndContinue,
			Terminal:         true,
			Message:          formatSameAdvisoryBlockerDegradeMessage(packet, sameCount),
			SameBlockerCount: sameCount,
		}
	}
	if retryState.AdvisoryTotalAttempts >= maxAdvisoryFeedbackAttempts {
		return validationRemediationDecision{
			RetryAction:      ValidationRetryActionDegradeAndContinue,
			Terminal:         true,
			Message:          formatAdvisoryFeedbackCapDegradeMessage(packet, retryState.AdvisoryTotalAttempts),
			SameBlockerCount: sameCount,
		}
	}
	return validationRemediationDecision{
		RetryAction:      ValidationRetryActionRetrySpecDesign,
		SameBlockerCount: sameCount,
	}
}

func (state *runState) routeValidationRemediation(
	order int,
	phase string,
	packet ValidationRemediationPacket,
	signals []ValidationDeterministicSignal,
	verdict string,
	message string,
) validationRemediationDecision {
	decision := routeValidationRemediation(&state.BlockerRetry, packet)
	feedback := buildRunResultValidationFeedback(
		order,
		phase,
		packet,
		signals,
		decision.RetryAction,
		decision.SameBlockerCount,
		decision.Terminal,
		verdict,
		firstNonEmpty(message, decision.Message),
	)
	state.ValidationFeedback = append(state.ValidationFeedback, feedback)
	return decision
}

func isUpstreamRemediationStop(packet ValidationRemediationPacket) bool {
	if packet.OwnerBoundary == ValidationOwnerBoundaryUpstreamSpec {
		return true
	}
	if packet.RecommendedAction == ValidationRecommendedActionStopForHuman {
		return true
	}
	return packet.RecommendedAction == ValidationRecommendedActionConfirmUpstreamContract &&
		packet.OwnerBoundary == ValidationOwnerBoundaryUpstreamSpec
}

func formatUpstreamRemediationStopMessage(packet ValidationRemediationPacket) string {
	owner := packet.OwnerSpec
	if owner == "" {
		owner = "(unknown)"
	}
	return fmt.Sprintf(
		"validation stopped: upstream owner %q requires human confirmation; blocker: %s / %s",
		owner,
		packet.Category,
		packet.Fingerprint,
	)
}

func formatSameCriticalBlockerStopMessage(packet ValidationRemediationPacket, sameCount int) string {
	return fmt.Sprintf(
		"validation stopped: same critical blocker %s / %s repeated %d times",
		packet.Category,
		packet.Fingerprint,
		sameCount,
	)
}

func formatCriticalFeedbackCapStopMessage(packet ValidationRemediationPacket, totalAttempts int) string {
	return fmt.Sprintf(
		"validation stopped: critical feedback attempt cap reached (%d); last blocker: %s / %s",
		totalAttempts,
		packet.Category,
		packet.Fingerprint,
	)
}

func formatSameAdvisoryBlockerDegradeMessage(packet ValidationRemediationPacket, sameCount int) string {
	return fmt.Sprintf(
		"advisory degradation: same advisory blocker %s / %s repeated %d times",
		packet.Category,
		packet.Fingerprint,
		sameCount,
	)
}

func formatAdvisoryFeedbackCapDegradeMessage(packet ValidationRemediationPacket, totalAttempts int) string {
	return fmt.Sprintf(
		"advisory degradation: advisory feedback attempt cap reached (%d); last blocker: %s / %s",
		totalAttempts,
		packet.Category,
		packet.Fingerprint,
	)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (state *runState) recordValidationFeedback(
	order int,
	phase string,
	packet ValidationRemediationPacket,
	signals []ValidationDeterministicSignal,
	retryAction ValidationRetryAction,
	terminal bool,
	verdict string,
	message string,
) RunResultValidationFeedback {
	sameBlockerCount := state.BlockerRetry.observeBlocker(packet.VerdictClass, packet.StableKey)
	feedback := buildRunResultValidationFeedback(
		order,
		phase,
		packet,
		signals,
		retryAction,
		sameBlockerCount,
		terminal,
		verdict,
		message,
	)
	state.ValidationFeedback = append(state.ValidationFeedback, feedback)
	return feedback
}

type ValidationVerdictClass string

const (
	ValidationVerdictClassCritical ValidationVerdictClass = "critical"
	ValidationVerdictClassAdvisory ValidationVerdictClass = "advisory"
)

type ValidationBlockerCategory string

const (
	ValidationBlockerCategoryRequirementsTraceability ValidationBlockerCategory = "requirements_traceability"
	ValidationBlockerCategoryBoundaryViolation        ValidationBlockerCategory = "boundary_violation"
	ValidationBlockerCategoryFileStructure            ValidationBlockerCategory = "file_structure"
	ValidationBlockerCategoryTestingGap               ValidationBlockerCategory = "testing_gap"
	ValidationBlockerCategoryDataContract             ValidationBlockerCategory = "data_contract"
	ValidationBlockerCategoryCrossSpecContract        ValidationBlockerCategory = "cross_spec_contract"
	ValidationBlockerCategoryArtifactContract         ValidationBlockerCategory = "artifact_contract"
	ValidationBlockerCategoryOtherDesignGap           ValidationBlockerCategory = "other_design_gap"
)

var validationBlockerCategoryValues = []string{
	string(ValidationBlockerCategoryRequirementsTraceability),
	string(ValidationBlockerCategoryBoundaryViolation),
	string(ValidationBlockerCategoryFileStructure),
	string(ValidationBlockerCategoryTestingGap),
	string(ValidationBlockerCategoryDataContract),
	string(ValidationBlockerCategoryCrossSpecContract),
	string(ValidationBlockerCategoryArtifactContract),
	string(ValidationBlockerCategoryOtherDesignGap),
}

type ValidationRecommendedAction string

const (
	ValidationRecommendedActionReviseCurrentSpec       ValidationRecommendedAction = "revise_current_spec"
	ValidationRecommendedActionClarifyRequirements     ValidationRecommendedAction = "clarify_requirements"
	ValidationRecommendedActionConfirmUpstreamContract ValidationRecommendedAction = "confirm_upstream_contract"
	ValidationRecommendedActionRecordAdvisory          ValidationRecommendedAction = "record_advisory"
	ValidationRecommendedActionStopForHuman            ValidationRecommendedAction = "stop_for_human"
)

var validationRecommendedActionValues = []string{
	string(ValidationRecommendedActionReviseCurrentSpec),
	string(ValidationRecommendedActionClarifyRequirements),
	string(ValidationRecommendedActionConfirmUpstreamContract),
	string(ValidationRecommendedActionRecordAdvisory),
	string(ValidationRecommendedActionStopForHuman),
}

type ValidationOwnerBoundary string

const (
	ValidationOwnerBoundaryCurrentSpec  ValidationOwnerBoundary = "current_spec"
	ValidationOwnerBoundaryUpstreamSpec ValidationOwnerBoundary = "upstream_spec"
)

var validationOwnerBoundaryValues = []string{
	string(ValidationOwnerBoundaryCurrentSpec),
	string(ValidationOwnerBoundaryUpstreamSpec),
}

var validationRemediationFields = []struct {
	name        string
	description string
	schema      map[string]any
}{
	{
		name:        "blocker_category",
		description: "remediation blocker category",
		schema: map[string]any{
			"type": "string",
			"enum": stringEnumSchema(validationBlockerCategoryValues),
		},
	},
	{
		name:        "blocker_fingerprint",
		description: "stable lower-case slug; do not use a summary hash or full natural-language summary",
		schema: map[string]any{
			"type": "string",
		},
	},
	{
		name:        "affected_artifacts",
		description: "array of repo-relative or current-spec-relative artifact paths",
		schema: map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "string",
			},
		},
	},
	{
		name:        "recommended_action",
		description: "recommended remediation action",
		schema: map[string]any{
			"type": "string",
			"enum": stringEnumSchema(validationRecommendedActionValues),
		},
	},
	{
		name:        "owner_boundary",
		description: "whether the blocker is owned by the current spec or an upstream spec",
		schema: map[string]any{
			"type": "string",
			"enum": stringEnumSchema(validationOwnerBoundaryValues),
		},
	},
	{
		name:        "owner_spec",
		description: "upstream feature name when owner_boundary=upstream_spec",
		schema: map[string]any{
			"type": "string",
		},
	},
}

type ValidationRemediationPacket struct {
	Source            ValidationFeedbackSource
	VerdictClass      ValidationVerdictClass
	Category          ValidationBlockerCategory
	Fingerprint       string
	StableKey         string
	AffectedArtifacts []string
	RecommendedAction ValidationRecommendedAction
	OwnerBoundary     ValidationOwnerBoundary
	OwnerSpec         string
	Summary           string
	Advisory          string
	Attempt           int
}

type ValidationDeterministicSignalKind string

const (
	ValidationDeterministicSignalAffectedArtifactOutsideCurrentSpec ValidationDeterministicSignalKind = "affected_artifact_outside_current_spec"
)

type ValidationDeterministicSignal struct {
	Kind                  ValidationDeterministicSignalKind
	Artifact              string
	Message               string
	DetectedOwnerBoundary ValidationOwnerBoundary
	DetectedOwnerSpec     string
}

var (
	validationBlockerFingerprintPattern = regexp.MustCompile(`^[a-z0-9]+(?:[-_][a-z0-9]+)*$`)
	validationHashLikeFingerprint       = regexp.MustCompile(`^[a-f0-9]{32,128}$`)
)

// validationRemediationSchema clones the generic validation schema and allows
// validate-design to return workflow-local remediation fields.
func validationRemediationSchema() *provider.OutputFormat {
	base := inquiryloop.ValidationSchema()
	root, ok := cloneSchemaValue(base.Schema).(map[string]any)
	if !ok {
		return &provider.OutputFormat{Type: base.Type, Schema: base.Schema}
	}
	props, ok := root["properties"].(map[string]any)
	if !ok {
		props = map[string]any{}
		root["properties"] = props
	}
	for _, field := range validationRemediationFields {
		props[field.name] = cloneSchemaMap(field.schema)
	}
	return &provider.OutputFormat{
		Type:   base.Type,
		Schema: root,
	}
}

// validationRemediationContractBlock documents the parser-enforced remediation
// contract for validate-design without changing the generic inquiryloop schema.
func validationRemediationContractBlock(requiresVerdict bool, phase string) string {
	if !requiresVerdict || phase != "validate-design" {
		return ""
	}

	var b strings.Builder
	b.WriteString("# Output Contract (validate-design remediation routing)\n\n")
	b.WriteString("This phase is a workflow gate. Return the base fields `has_inquiry`, `summary`, `verdict`, `has_advisory`, and `advisory` according to the existing validation contract, plus the remediation fields below when feedback must be routed.\n\n")
	b.WriteString("Conditional required rules:\n")
	b.WriteString("- verdict=go && has_advisory=false: clean pass; remediation fields are not required.\n")
	b.WriteString("- verdict=no_go: `blocker_category`, `blocker_fingerprint`, `affected_artifacts`, `recommended_action`, and `owner_boundary` are required.\n")
	b.WriteString("- verdict=go && has_advisory=true: treat the advisory as lightweight remediation; the same remediation fields are required.\n")
	b.WriteString("- owner_boundary=upstream_spec: `owner_spec` is required and must name the upstream feature that owns the contract.\n")
	b.WriteString("- owner_boundary=current_spec: `owner_spec` must be empty or the current feature name.\n")
	b.WriteString("- `blocker_fingerprint` must be a stable slug. Do not use a summary hash, the full summary text, or another natural-language fallback.\n\n")
	b.WriteString("Remediation fields and enum values:\n")
	for _, field := range validationRemediationFields {
		b.WriteString("- `")
		b.WriteString(field.name)
		b.WriteString("`: ")
		b.WriteString(field.description)
		if enumValues := schemaStringEnumValues(field.schema); len(enumValues) > 0 {
			b.WriteString(". Allowed values: ")
			b.WriteString(strings.Join(enumValues, ", "))
		}
		b.WriteString(".\n")
	}
	b.WriteString("\nJSON Schema keeps these remediation properties optional; the workflow parser enforces the conditional required rules above after the response is parsed.\n")
	return b.String()
}

func validationContractBlockForStep(requiresVerdict bool, phase string) string {
	if remediation := validationRemediationContractBlock(requiresVerdict, phase); remediation != "" {
		return "\n\n---\n\n" + remediation
	}
	return validationContractBlock(requiresVerdict)
}

func validationSchemaForStep(step promptStep) *provider.OutputFormat {
	if !step.RequiresVerdict {
		return nil
	}
	if step.Phase == "validate-design" {
		return validationRemediationSchema()
	}
	return inquiryloop.ValidationSchema()
}

func parseValidationRemediationPacket(result inquiryloop.FinalResult, attempt int) (*ValidationRemediationPacket, error) {
	verdictClass, required, err := validationVerdictClassForResult(result)
	if err != nil {
		return nil, err
	}
	if !required {
		return nil, nil
	}

	category, err := parseValidationBlockerCategory(result.Extensions)
	if err != nil {
		return nil, err
	}
	fingerprint, err := requiredRemediationString(result.Extensions, "blocker_fingerprint")
	if err != nil {
		return nil, err
	}
	if err := validateValidationBlockerFingerprint(fingerprint); err != nil {
		return nil, err
	}
	affectedArtifacts, err := requiredRemediationStringSlice(result.Extensions, "affected_artifacts")
	if err != nil {
		return nil, err
	}
	recommendedAction, err := parseValidationRecommendedAction(result.Extensions)
	if err != nil {
		return nil, err
	}
	ownerBoundary, err := parseValidationOwnerBoundary(result.Extensions)
	if err != nil {
		return nil, err
	}
	ownerSpec, err := optionalRemediationString(result.Extensions, "owner_spec")
	if err != nil {
		return nil, err
	}

	packet := &ValidationRemediationPacket{
		Source:            ValidationFeedbackSourceValidateDesign,
		VerdictClass:      verdictClass,
		Category:          category,
		Fingerprint:       fingerprint,
		AffectedArtifacts: affectedArtifacts,
		RecommendedAction: recommendedAction,
		OwnerBoundary:     ownerBoundary,
		OwnerSpec:         ownerSpec,
		Summary:           result.Summary,
		Advisory:          result.Advisory,
		Attempt:           attempt,
	}
	packet.StableKey = validationRemediationStableKey(packet)
	return packet, nil
}

func validateRemediationOwnership(cwd, currentFeature string, packet ValidationRemediationPacket) (ValidationRemediationPacket, []ValidationDeterministicSignal, error) {
	currentFeature = strings.TrimSpace(currentFeature)
	if currentFeature == "" {
		return packet, nil, remediationSchemaViolation("current feature is required for owner boundary validation")
	}
	packet.OwnerSpec = strings.TrimSpace(packet.OwnerSpec)

	switch packet.OwnerBoundary {
	case ValidationOwnerBoundaryCurrentSpec:
		if packet.OwnerSpec != "" && packet.OwnerSpec != currentFeature {
			return packet, nil, remediationSchemaViolation("owner_spec must be empty or %q when owner_boundary=current_spec, got %q", currentFeature, packet.OwnerSpec)
		}
	case ValidationOwnerBoundaryUpstreamSpec:
		if packet.OwnerSpec == "" {
			return packet, nil, remediationSchemaViolation("owner_spec is required when owner_boundary=upstream_spec")
		}
		if packet.OwnerSpec == currentFeature {
			return packet, nil, remediationSchemaViolation("owner_spec must differ from current feature %q when owner_boundary=upstream_spec", currentFeature)
		}
	default:
		return packet, nil, remediationSchemaViolation("owner_boundary must be one of %s, got %q", strings.Join(validationOwnerBoundaryValues, ", "), packet.OwnerBoundary)
	}

	signals := make([]ValidationDeterministicSignal, 0)
	for _, artifact := range packet.AffectedArtifacts {
		ownerSpec, ok := featureFromAffectedSpecArtifact(cwd, artifact)
		if !ok || ownerSpec == currentFeature {
			continue
		}
		if packet.OwnerBoundary == ValidationOwnerBoundaryCurrentSpec {
			packet.OwnerBoundary = ValidationOwnerBoundaryUpstreamSpec
			packet.OwnerSpec = ownerSpec
			packet.RecommendedAction = ValidationRecommendedActionConfirmUpstreamContract
			signals = append(signals, ValidationDeterministicSignal{
				Kind:                  ValidationDeterministicSignalAffectedArtifactOutsideCurrentSpec,
				Artifact:              artifact,
				Message:               "affected_artifacts points to another spec; routing requires upstream owner confirmation",
				DetectedOwnerBoundary: ValidationOwnerBoundaryUpstreamSpec,
				DetectedOwnerSpec:     ownerSpec,
			})
			break
		}
	}

	packet.StableKey = validationRemediationStableKey(&packet)
	return packet, signals, nil
}

func formatValidationRemediationPreviousResponse(packet ValidationRemediationPacket, signals []ValidationDeterministicSignal) string {
	var b strings.Builder
	b.WriteString("# Validation Remediation\n\n")
	b.WriteString("- Source: ")
	b.WriteString(string(packet.Source))
	b.WriteString("\n")
	b.WriteString("- Verdict Class: ")
	b.WriteString(string(packet.VerdictClass))
	b.WriteString("\n")
	b.WriteString("- Blocker: ")
	b.WriteString(string(packet.Category))
	b.WriteString(" / ")
	b.WriteString(packet.Fingerprint)
	b.WriteString("\n")
	b.WriteString("- Owner Boundary: ")
	b.WriteString(string(packet.OwnerBoundary))
	b.WriteString("\n")
	b.WriteString("- Owner Spec: ")
	if packet.OwnerSpec == "" {
		b.WriteString("(none)")
	} else {
		b.WriteString(packet.OwnerSpec)
	}
	b.WriteString("\n")
	b.WriteString("- Affected Artifacts:\n")
	for _, artifact := range packet.AffectedArtifacts {
		b.WriteString("  - ")
		b.WriteString(artifact)
		b.WriteString("\n")
	}
	b.WriteString("- Recommended Action: ")
	b.WriteString(string(packet.RecommendedAction))
	b.WriteString("\n")

	if len(signals) > 0 {
		b.WriteString("\n## Deterministic Signals\n\n")
		for _, signal := range signals {
			b.WriteString("- Kind: ")
			b.WriteString(string(signal.Kind))
			b.WriteString("\n")
			b.WriteString("  - Artifact: ")
			b.WriteString(signal.Artifact)
			b.WriteString("\n")
			b.WriteString("  - Message: ")
			b.WriteString(signal.Message)
			b.WriteString("\n")
			b.WriteString("  - Detected Owner Boundary: ")
			b.WriteString(string(signal.DetectedOwnerBoundary))
			b.WriteString("\n")
			b.WriteString("  - Detected Owner Spec: ")
			b.WriteString(signal.DetectedOwnerSpec)
			b.WriteString("\n")
		}
	}

	b.WriteString("\n## Feedback\n")
	hasSummary := strings.TrimSpace(packet.Summary) != ""
	hasAdvisory := strings.TrimSpace(packet.Advisory) != ""
	if hasSummary {
		b.WriteString("\n### Summary\n\n")
		b.WriteString(packet.Summary)
		b.WriteString("\n")
	}
	if hasAdvisory {
		b.WriteString("\n### Advisory\n\n")
		b.WriteString(packet.Advisory)
		b.WriteString("\n")
	}
	if !hasSummary && !hasAdvisory {
		b.WriteString("\nNo validator feedback was provided.\n")
	}
	return b.String()
}

func validationVerdictClassForResult(result inquiryloop.FinalResult) (ValidationVerdictClass, bool, error) {
	switch result.Verdict {
	case inquiryloop.VerdictGo:
		if result.HasAdvisory {
			return ValidationVerdictClassAdvisory, true, nil
		}
		return "", false, nil
	case inquiryloop.VerdictNoGo:
		return ValidationVerdictClassCritical, true, nil
	default:
		return "", false, remediationSchemaViolation("verdict must be %q or %q, got %q", inquiryloop.VerdictGo, inquiryloop.VerdictNoGo, result.Verdict)
	}
}

func parseValidationBlockerCategory(extensions map[string]any) (ValidationBlockerCategory, error) {
	value, err := requiredRemediationString(extensions, "blocker_category")
	if err != nil {
		return "", err
	}
	if !validationStringInSlice(value, validationBlockerCategoryValues) {
		return "", remediationSchemaViolation("blocker_category must be one of %s, got %q", strings.Join(validationBlockerCategoryValues, ", "), value)
	}
	return ValidationBlockerCategory(value), nil
}

func parseValidationRecommendedAction(extensions map[string]any) (ValidationRecommendedAction, error) {
	value, err := requiredRemediationString(extensions, "recommended_action")
	if err != nil {
		return "", err
	}
	if !validationStringInSlice(value, validationRecommendedActionValues) {
		return "", remediationSchemaViolation("recommended_action must be one of %s, got %q", strings.Join(validationRecommendedActionValues, ", "), value)
	}
	return ValidationRecommendedAction(value), nil
}

func parseValidationOwnerBoundary(extensions map[string]any) (ValidationOwnerBoundary, error) {
	value, err := requiredRemediationString(extensions, "owner_boundary")
	if err != nil {
		return "", err
	}
	if !validationStringInSlice(value, validationOwnerBoundaryValues) {
		return "", remediationSchemaViolation("owner_boundary must be one of %s, got %q", strings.Join(validationOwnerBoundaryValues, ", "), value)
	}
	return ValidationOwnerBoundary(value), nil
}

func requiredRemediationString(extensions map[string]any, name string) (string, error) {
	value, ok := extensions[name]
	if !ok || value == nil {
		return "", remediationSchemaViolation("%s is required", name)
	}
	text, ok := value.(string)
	if !ok {
		return "", remediationSchemaViolation("%s must be a string", name)
	}
	if strings.TrimSpace(text) == "" {
		return "", remediationSchemaViolation("%s must not be empty", name)
	}
	return text, nil
}

func optionalRemediationString(extensions map[string]any, name string) (string, error) {
	value, ok := extensions[name]
	if !ok || value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", remediationSchemaViolation("%s must be a string", name)
	}
	return strings.TrimSpace(text), nil
}

func requiredRemediationStringSlice(extensions map[string]any, name string) ([]string, error) {
	value, ok := extensions[name]
	if !ok || value == nil {
		return nil, remediationSchemaViolation("%s is required", name)
	}

	var items []string
	switch typed := value.(type) {
	case []any:
		items = make([]string, 0, len(typed))
		for i, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, remediationSchemaViolation("%s[%d] must be a string", name, i)
			}
			items = append(items, strings.TrimSpace(text))
		}
	case []string:
		items = make([]string, 0, len(typed))
		for _, item := range typed {
			items = append(items, strings.TrimSpace(item))
		}
	default:
		return nil, remediationSchemaViolation("%s must be an array of strings", name)
	}

	if len(items) == 0 {
		return nil, remediationSchemaViolation("%s must not be empty", name)
	}
	for i, item := range items {
		if item == "" {
			return nil, remediationSchemaViolation("%s[%d] must not be empty", name, i)
		}
	}
	return items, nil
}

func validateValidationBlockerFingerprint(fingerprint string) error {
	if !validationBlockerFingerprintPattern.MatchString(fingerprint) {
		return remediationSchemaViolation("blocker_fingerprint must be a stable lower-case slug, got %q", fingerprint)
	}
	if validationHashLikeFingerprint.MatchString(fingerprint) {
		return remediationSchemaViolation("blocker_fingerprint must not be a summary hash fallback, got %q", fingerprint)
	}
	return nil
}

func validationRemediationStableKey(packet *ValidationRemediationPacket) string {
	return strings.Join([]string{
		string(packet.VerdictClass),
		string(packet.Category),
		string(packet.OwnerBoundary),
		packet.OwnerSpec,
		packet.Fingerprint,
	}, ":")
}

func featureFromAffectedSpecArtifact(cwd, artifact string) (string, bool) {
	artifact = strings.TrimSpace(artifact)
	if artifact == "" {
		return "", false
	}

	candidates := []string{artifact}
	if filepath.IsAbs(artifact) && strings.TrimSpace(cwd) != "" {
		if rel, err := filepath.Rel(cwd, artifact); err == nil {
			candidates = append(candidates, rel)
		}
	}

	for _, candidate := range candidates {
		clean := filepath.ToSlash(filepath.Clean(candidate))
		parts := strings.Split(clean, "/")
		for i := 0; i+2 < len(parts); i++ {
			if parts[i] != ".kiro" || parts[i+1] != "specs" {
				continue
			}
			feature := parts[i+2]
			if feature == "" || feature == "." || feature == ".." {
				continue
			}
			return feature, true
		}
	}
	return "", false
}

func validationStringInSlice(needle string, haystack []string) bool {
	for _, value := range haystack {
		if value == needle {
			return true
		}
	}
	return false
}

func remediationSchemaViolation(format string, args ...any) error {
	return fmt.Errorf("%w: validation remediation: %s", inquiryloop.ErrSchemaViolation, fmt.Sprintf(format, args...))
}

func stringEnumSchema(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

func schemaStringEnumValues(schema map[string]any) []string {
	raw, ok := schema["enum"].([]any)
	if !ok {
		return nil
	}
	values := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			values = append(values, s)
		}
	}
	return values
}

func cloneSchemaMap(in map[string]any) map[string]any {
	cloned, _ := cloneSchemaValue(in).(map[string]any)
	return cloned
}

func cloneSchemaValue(in any) any {
	switch v := in.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, value := range v {
			out[key] = cloneSchemaValue(value)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, value := range v {
			out[i] = cloneSchemaValue(value)
		}
		return out
	default:
		return v
	}
}
