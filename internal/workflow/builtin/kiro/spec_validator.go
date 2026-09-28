package kiro

import "fmt"

type SpecValidationResult struct {
	OK     bool
	Reason string
}

func ValidateSpec(jsonValue any) SpecValidationResult {
	spec, ok := jsonValue.(map[string]any)
	if !ok {
		return SpecValidationResult{Reason: "spec.jsonがオブジェクトではありません"}
	}
	if spec["ready_for_implementation"] != true {
		return SpecValidationResult{Reason: "ready_for_implementationがtrueではありません"}
	}
	approvals, ok := spec["approvals"].(map[string]any)
	if !ok {
		return SpecValidationResult{Reason: "approvalsが存在しません"}
	}
	for _, key := range []string{"requirements", "design", "tasks"} {
		section, ok := approvals[key].(map[string]any)
		if !ok || section["approved"] != true {
			return SpecValidationResult{Reason: fmt.Sprintf("approvals.%s.approvedがtrueではありません", key)}
		}
	}
	return SpecValidationResult{OK: true}
}
