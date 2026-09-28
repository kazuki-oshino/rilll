package kiro

import (
	"strings"
	"testing"
)

var remediationFieldExpectations = []struct {
	name string
	typ  string
	enum []string
}{
	{
		name: "blocker_category",
		typ:  "string",
		enum: []string{
			"requirements_traceability",
			"boundary_violation",
			"file_structure",
			"testing_gap",
			"data_contract",
			"cross_spec_contract",
			"artifact_contract",
			"other_design_gap",
		},
	},
	{
		name: "blocker_fingerprint",
		typ:  "string",
	},
	{
		name: "affected_artifacts",
		typ:  "array",
	},
	{
		name: "recommended_action",
		typ:  "string",
		enum: []string{
			"revise_current_spec",
			"clarify_requirements",
			"confirm_upstream_contract",
			"record_advisory",
			"stop_for_human",
		},
	},
	{
		name: "owner_boundary",
		typ:  "string",
		enum: []string{"current_spec", "upstream_spec"},
	},
	{
		name: "owner_spec",
		typ:  "string",
	},
}

func TestValidationRemediationSchemaAllowsWorkflowLocalFields(t *testing.T) {
	out := validationRemediationSchema()
	if out == nil {
		t.Fatalf("validationRemediationSchema() returned nil")
	}
	if out.Type != "json_schema" {
		t.Fatalf("validationRemediationSchema().Type=%q, want json_schema", out.Type)
	}

	root := requireRemediationSchemaRoot(t, out.Schema)
	required := remediationRequiredSet(t, root)
	for _, name := range []string{"has_inquiry", "summary", "verdict", "has_advisory"} {
		if !required[name] {
			t.Fatalf("base validation required field %q missing: %v", name, required)
		}
	}
	for _, field := range remediationFieldExpectations {
		if required[field.name] {
			t.Fatalf("workflow-local remediation field %q must be optional in JSON Schema required set", field.name)
		}
	}

	props := remediationProperties(t, root)
	for _, field := range remediationFieldExpectations {
		prop := remediationProperty(t, props, field.name)
		if got := prop["type"]; got != field.typ {
			t.Fatalf("%s.type=%v, want %s", field.name, got, field.typ)
		}
		for _, enumValue := range field.enum {
			if !schemaEnumContains(prop, enumValue) {
				t.Fatalf("%s.enum missing %q: %v", field.name, enumValue, prop["enum"])
			}
		}
		if len(field.enum) > 0 {
			for _, enumValue := range schemaEnumValues(t, prop) {
				if !stringInSlice(enumValue, field.enum) {
					t.Fatalf("%s.enum contains unexpected value %q; want only %v", field.name, enumValue, field.enum)
				}
			}
		}
	}

	affected := remediationProperty(t, props, "affected_artifacts")
	items, ok := affected["items"].(map[string]any)
	if !ok {
		t.Fatalf("affected_artifacts.items missing or wrong type: %T", affected["items"])
	}
	if got := items["type"]; got != "string" {
		t.Fatalf("affected_artifacts.items.type=%v, want string", got)
	}
}

func TestValidationRemediationContractStatesConditionalRequirements(t *testing.T) {
	block := validationRemediationContractBlock(true, "validate-design")
	if block == "" {
		t.Fatalf("validationRemediationContractBlock(true, validate-design) returned empty")
	}

	for _, marker := range []string{
		"# Output Contract",
		"verdict=go && has_advisory=false",
		"remediation fields are not required",
		"verdict=no_go",
		"verdict=go && has_advisory=true",
		"blocker_category",
		"blocker_fingerprint",
		"affected_artifacts",
		"recommended_action",
		"owner_boundary",
		"owner_boundary=upstream_spec",
		"owner_spec",
		"stable slug",
		"summary hash",
	} {
		if !strings.Contains(block, marker) {
			t.Fatalf("contract block missing %q:\n%s", marker, block)
		}
	}

	for _, field := range remediationFieldExpectations {
		if !strings.Contains(block, field.name) {
			t.Fatalf("contract block missing remediation field %q", field.name)
		}
		for _, enumValue := range field.enum {
			if !strings.Contains(block, enumValue) {
				t.Fatalf("contract block missing enum %q for %s", enumValue, field.name)
			}
		}
	}
}

func TestValidationRemediationContractOnlyTargetsValidateDesign(t *testing.T) {
	if got := validationRemediationContractBlock(false, "validate-design"); got != "" {
		t.Fatalf("requiresVerdict=false must not emit remediation contract: %q", got)
	}
	if got := validationRemediationContractBlock(true, "validate-gap"); got != "" {
		t.Fatalf("non validate-design phase must not emit remediation contract: %q", got)
	}
}

func TestValidationRemediationSchemaAndContractStayInSync(t *testing.T) {
	out := validationRemediationSchema()
	root := requireRemediationSchemaRoot(t, out.Schema)
	props := remediationProperties(t, root)
	block := validationRemediationContractBlock(true, "validate-design")

	for _, field := range remediationFieldExpectations {
		if _, ok := props[field.name]; !ok {
			t.Fatalf("schema missing remediation property %q", field.name)
		}
		if !strings.Contains(block, field.name) {
			t.Fatalf("contract block missing remediation property %q", field.name)
		}
		for _, enumValue := range schemaEnumValues(t, remediationProperty(t, props, field.name)) {
			if !strings.Contains(block, enumValue) {
				t.Fatalf("contract block missing schema enum %q for %s", enumValue, field.name)
			}
		}
	}
}

func requireRemediationSchemaRoot(t *testing.T, schema any) map[string]any {
	t.Helper()
	root, ok := schema.(map[string]any)
	if !ok {
		t.Fatalf("schema root type=%T, want map[string]any", schema)
	}
	if got := root["type"]; got != "object" {
		t.Fatalf("schema root type=%v, want object", got)
	}
	return root
}

func remediationRequiredSet(t *testing.T, root map[string]any) map[string]bool {
	t.Helper()
	requiredAny, ok := root["required"].([]any)
	if !ok {
		t.Fatalf("root.required type=%T, want []any", root["required"])
	}
	required := make(map[string]bool, len(requiredAny))
	for _, v := range requiredAny {
		name, ok := v.(string)
		if !ok {
			t.Fatalf("required entry type=%T, want string", v)
		}
		required[name] = true
	}
	return required
}

func remediationProperties(t *testing.T, root map[string]any) map[string]any {
	t.Helper()
	props, ok := root["properties"].(map[string]any)
	if !ok {
		t.Fatalf("root.properties type=%T, want map[string]any", root["properties"])
	}
	return props
}

func remediationProperty(t *testing.T, props map[string]any, name string) map[string]any {
	t.Helper()
	prop, ok := props[name].(map[string]any)
	if !ok {
		t.Fatalf("property %q missing or wrong type: %T", name, props[name])
	}
	return prop
}

func schemaEnumContains(prop map[string]any, want string) bool {
	for _, got := range schemaEnumValuesNoFatal(prop) {
		if got == want {
			return true
		}
	}
	return false
}

func schemaEnumValues(t *testing.T, prop map[string]any) []string {
	t.Helper()
	valuesAny, ok := prop["enum"].([]any)
	if !ok {
		if _, exists := prop["enum"]; !exists {
			return nil
		}
		t.Fatalf("enum type=%T, want []any", prop["enum"])
	}
	values := make([]string, 0, len(valuesAny))
	for _, v := range valuesAny {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("enum entry type=%T, want string", v)
		}
		values = append(values, s)
	}
	return values
}

func schemaEnumValuesNoFatal(prop map[string]any) []string {
	valuesAny, ok := prop["enum"].([]any)
	if !ok {
		return nil
	}
	values := make([]string, 0, len(valuesAny))
	for _, v := range valuesAny {
		if s, ok := v.(string); ok {
			values = append(values, s)
		}
	}
	return values
}

func stringInSlice(needle string, haystack []string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
