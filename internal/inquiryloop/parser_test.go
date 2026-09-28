package inquiryloop

import (
	"reflect"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

// ParseResponseDetail が advisory フィールド (has_advisory + advisory) を抽出することを確認する。
// optional 扱い: フィールド未存在 / has_advisory=false でも違反にしない。
func TestParseResponseDetailExtractsAdvisory(t *testing.T) {
	tests := []struct {
		name        string
		structured  map[string]any
		wantHas     bool
		wantText    string
		wantVerdict ValidationVerdict
	}{
		{
			name: "advisory absent",
			structured: map[string]any{
				"has_inquiry": false,
				"summary":     "no advisory case",
				"verdict":     "go",
			},
			wantHas:     false,
			wantText:    "",
			wantVerdict: VerdictGo,
		},
		{
			name: "has_advisory false with empty advisory",
			structured: map[string]any{
				"has_inquiry":  false,
				"summary":      "clean go",
				"verdict":      "go",
				"has_advisory": false,
				"advisory":     "",
			},
			wantHas:     false,
			wantText:    "",
			wantVerdict: VerdictGo,
		},
		{
			name: "go with advisory",
			structured: map[string]any{
				"has_inquiry":  false,
				"summary":      "概ね整理されているがエッジケース要確認",
				"verdict":      "go",
				"has_advisory": true,
				"advisory":     "FTS5 fallback の動作を tasks 側で検証してください",
			},
			wantHas:     true,
			wantText:    "FTS5 fallback の動作を tasks 側で検証してください",
			wantVerdict: VerdictGo,
		},
		{
			name: "no_go can also carry advisory text (treated as fix guidance)",
			structured: map[string]any{
				"has_inquiry":  false,
				"summary":      "境界が曖昧",
				"verdict":      "no_go",
				"has_advisory": true,
				"advisory":     "Tauri command の serde 変換を修正",
			},
			wantHas:     true,
			wantText:    "Tauri command の serde 変換を修正",
			wantVerdict: VerdictNoGo,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := provider.AgentResponse{Success: true, StructuredOutput: tc.structured}
			parsed, err := ParseResponseDetail(resp)
			if err != nil {
				t.Fatalf("ParseResponseDetail unexpected error: %v", err)
			}
			if parsed.HasAdvisory != tc.wantHas {
				t.Fatalf("HasAdvisory=%v, want %v (parsed=%+v)", parsed.HasAdvisory, tc.wantHas, parsed)
			}
			if parsed.Advisory != tc.wantText {
				t.Fatalf("Advisory=%q, want %q", parsed.Advisory, tc.wantText)
			}
			if parsed.Verdict != tc.wantVerdict {
				t.Fatalf("Verdict=%q, want %q", parsed.Verdict, tc.wantVerdict)
			}
		})
	}
}

// has_advisory=true なのに advisory が string でない / 欠落のときは ErrSchemaViolation。
// 検証 gate を「アドバイス本文を伴う」契約として固定する。
func TestParseResponseDetailRejectsAdvisoryWithoutText(t *testing.T) {
	resp := provider.AgentResponse{Success: true, StructuredOutput: map[string]any{
		"has_inquiry":  false,
		"summary":      "境界が曖昧",
		"verdict":      "no_go",
		"has_advisory": true,
		// advisory フィールドが存在しない
	}}
	if _, err := ParseResponseDetail(resp); err == nil {
		t.Fatalf("expected ErrSchemaViolation when has_advisory=true but advisory missing")
	}
}

func TestParseResponseDetailPreservesExtensions(t *testing.T) {
	raw := map[string]any{
		"has_inquiry":        false,
		"summary":            "design has a boundary issue",
		"verdict":            "no_go",
		"has_advisory":       true,
		"advisory":           "Clarify artifact ownership before retrying.",
		"blocker_category":   "boundary_violation",
		"affected_artifacts": []any{"design.md", "tasks.md"},
		"owner_boundary":     "current_spec",
	}

	parsed, err := ParseResponseDetail(provider.AgentResponse{Success: true, StructuredOutput: raw})
	if err != nil {
		t.Fatalf("ParseResponseDetail unexpected error: %v", err)
	}
	if parsed.HasInquiry {
		t.Fatalf("HasInquiry=true, want false")
	}
	if parsed.Summary != "design has a boundary issue" {
		t.Fatalf("Summary=%q", parsed.Summary)
	}
	if parsed.Verdict != VerdictNoGo {
		t.Fatalf("Verdict=%q, want %q", parsed.Verdict, VerdictNoGo)
	}
	if !parsed.HasAdvisory || parsed.Advisory != "Clarify artifact ownership before retrying." {
		t.Fatalf("advisory fields changed: has=%v text=%q", parsed.HasAdvisory, parsed.Advisory)
	}

	extensions := requireExtensions(t, parsed)
	for _, known := range []string{"has_inquiry", "summary", "verdict", "has_advisory", "advisory", "inquiry"} {
		if _, ok := extensions[known]; ok {
			t.Fatalf("Extensions must not include known field %q: %v", known, extensions)
		}
	}
	if got := extensions["blocker_category"]; got != "boundary_violation" {
		t.Fatalf("Extensions[blocker_category]=%v, want boundary_violation", got)
	}
	if got := extensions["owner_boundary"]; got != "current_spec" {
		t.Fatalf("Extensions[owner_boundary]=%v, want current_spec", got)
	}
	artifacts, ok := extensions["affected_artifacts"].([]any)
	if !ok {
		t.Fatalf("Extensions[affected_artifacts] type=%T, want []any", extensions["affected_artifacts"])
	}
	if len(artifacts) != 2 || artifacts[0] != "design.md" || artifacts[1] != "tasks.md" {
		t.Fatalf("Extensions[affected_artifacts]=%v", artifacts)
	}

	raw["blocker_category"] = "mutated-after-parse"
	if got := extensions["blocker_category"]; got != "boundary_violation" {
		t.Fatalf("Extensions must be top-level cloned, got blocker_category=%v", got)
	}
}

func requireExtensions(t *testing.T, target any) map[string]any {
	t.Helper()
	v := reflect.ValueOf(target)
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			t.Fatalf("target is nil")
		}
		v = v.Elem()
	}
	field := v.FieldByName("Extensions")
	if !field.IsValid() {
		t.Fatalf("%T is missing Extensions map[string]any field", target)
	}
	wantType := reflect.TypeOf(map[string]any{})
	if field.Type() != wantType {
		t.Fatalf("%T.Extensions type=%v, want %v", target, field.Type(), wantType)
	}
	if field.IsNil() {
		t.Fatalf("%T.Extensions is nil, want preserved unknown fields", target)
	}
	extensions, ok := field.Interface().(map[string]any)
	if !ok {
		t.Fatalf("%T.Extensions is not map[string]any", target)
	}
	return extensions
}
