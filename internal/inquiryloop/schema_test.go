package inquiryloop

import (
	"testing"
)

// Schema() は kiro-spec-loop の各 kiro プロンプト呼び出しに渡す json_schema を返す。
// 1 箇所定義で運用するため、フォーマットの最低条件を厳しめに固定する。
func TestSchemaShape(t *testing.T) {
	out := Schema()
	if out == nil {
		t.Fatalf("Schema() returned nil")
	}
	if out.Type != "json_schema" {
		t.Fatalf("Schema().Type = %q, want %q", out.Type, "json_schema")
	}

	root, ok := out.Schema.(map[string]any)
	if !ok {
		t.Fatalf("Schema().Schema is not map[string]any: %T", out.Schema)
	}

	if got, want := root["type"], "object"; got != want {
		t.Fatalf("root type = %v, want %v", got, want)
	}

	// 最上位 required は has_inquiry / summary の 2 件
	requiredAny, ok := root["required"].([]any)
	if !ok {
		t.Fatalf("root required is not []any: %T", root["required"])
	}
	requiredSet := map[string]bool{}
	for _, v := range requiredAny {
		if s, ok := v.(string); ok {
			requiredSet[s] = true
		}
	}
	for _, name := range []string{"has_inquiry", "summary"} {
		if !requiredSet[name] {
			t.Fatalf("root required missing %q (got %v)", name, requiredAny)
		}
	}

	// properties は has_inquiry / summary / inquiry の 3 キーを含む
	props, ok := root["properties"].(map[string]any)
	if !ok {
		t.Fatalf("root properties is not map[string]any: %T", root["properties"])
	}
	for _, name := range []string{"has_inquiry", "summary", "inquiry"} {
		if _, exists := props[name]; !exists {
			t.Fatalf("root properties missing key %q (keys=%v)", name, mapKeys(props))
		}
	}

	// has_inquiry は boolean、summary は string であること
	if got := props["has_inquiry"].(map[string]any)["type"]; got != "boolean" {
		t.Fatalf("has_inquiry.type = %v, want boolean", got)
	}
	if got := props["summary"].(map[string]any)["type"]; got != "string" {
		t.Fatalf("summary.type = %v, want string", got)
	}

	// inquiry サブオブジェクトの required は title / question
	inquiry, ok := props["inquiry"].(map[string]any)
	if !ok {
		t.Fatalf("inquiry is not map[string]any: %T", props["inquiry"])
	}
	if got := inquiry["type"]; got != "object" {
		t.Fatalf("inquiry.type = %v, want object", got)
	}
	inquiryReq, ok := inquiry["required"].([]any)
	if !ok {
		t.Fatalf("inquiry.required is not []any: %T", inquiry["required"])
	}
	inqReqSet := map[string]bool{}
	for _, v := range inquiryReq {
		if s, ok := v.(string); ok {
			inqReqSet[s] = true
		}
	}
	for _, name := range []string{"title", "question"} {
		if !inqReqSet[name] {
			t.Fatalf("inquiry.required missing %q (got %v)", name, inquiryReq)
		}
	}

	// inquiry.properties は thin schema 化により title / question の 2 キーのみ
	// (background / options は stream_idle_timeout 対策で schema から除外、
	//  長文判断材料は per-round markdown に Source: パス参照で逃がす)。
	inquiryProps, ok := inquiry["properties"].(map[string]any)
	if !ok {
		t.Fatalf("inquiry.properties is not map[string]any: %T", inquiry["properties"])
	}
	for _, name := range []string{"title", "question"} {
		if _, exists := inquiryProps[name]; !exists {
			t.Fatalf("inquiry.properties missing key %q (keys=%v)", name, mapKeys(inquiryProps))
		}
	}
	for _, name := range []string{"background", "options"} {
		if _, exists := inquiryProps[name]; exists {
			t.Fatalf("inquiry.properties must NOT include %q in thin schema (keys=%v)", name, mapKeys(inquiryProps))
		}
	}
}

// ValidationSchema() は validate-design など GO/NO-GO gate を持つ skill 呼び出しに渡す
// json_schema を返し、通常 schema に verdict を追加必須化する。
func TestValidationSchemaRequiresVerdict(t *testing.T) {
	out := ValidationSchema()
	if out == nil {
		t.Fatalf("ValidationSchema() returned nil")
	}
	if out.Type != "json_schema" {
		t.Fatalf("ValidationSchema().Type = %q, want %q", out.Type, "json_schema")
	}
	root, ok := out.Schema.(map[string]any)
	if !ok {
		t.Fatalf("ValidationSchema().Schema is not map[string]any: %T", out.Schema)
	}
	requiredAny, ok := root["required"].([]any)
	if !ok {
		t.Fatalf("root required is not []any: %T", root["required"])
	}
	requiredSet := map[string]bool{}
	for _, v := range requiredAny {
		if s, ok := v.(string); ok {
			requiredSet[s] = true
		}
	}
	for _, name := range []string{"has_inquiry", "summary", "verdict", "has_advisory"} {
		if !requiredSet[name] {
			t.Fatalf("validation schema required missing %q (got %v)", name, requiredAny)
		}
	}
	props, ok := root["properties"].(map[string]any)
	if !ok {
		t.Fatalf("root properties is not map[string]any: %T", root["properties"])
	}
	verdict, ok := props["verdict"].(map[string]any)
	if !ok {
		t.Fatalf("verdict property missing or wrong type: %T", props["verdict"])
	}
	if got := verdict["type"]; got != "string" {
		t.Fatalf("verdict.type = %v, want string", got)
	}
	enumAny, ok := verdict["enum"].([]any)
	if !ok {
		t.Fatalf("verdict.enum is not []any: %T", verdict["enum"])
	}
	enumSet := map[string]bool{}
	for _, v := range enumAny {
		if s, ok := v.(string); ok {
			enumSet[s] = true
		}
	}
	for _, name := range []string{"go", "no_go"} {
		if !enumSet[name] {
			t.Fatalf("verdict.enum missing %q (got %v)", name, enumAny)
		}
	}

	// has_advisory は boolean、advisory は string (省略可だが定義は必須)
	hasAdv, ok := props["has_advisory"].(map[string]any)
	if !ok {
		t.Fatalf("has_advisory property missing or wrong type: %T", props["has_advisory"])
	}
	if got := hasAdv["type"]; got != "boolean" {
		t.Fatalf("has_advisory.type = %v, want boolean", got)
	}
	adv, ok := props["advisory"].(map[string]any)
	if !ok {
		t.Fatalf("advisory property missing or wrong type: %T", props["advisory"])
	}
	if got := adv["type"]; got != "string" {
		t.Fatalf("advisory.type = %v, want string", got)
	}
}

func TestValidationSchemaDoesNotIncludeExtensionFields(t *testing.T) {
	out := ValidationSchema()
	root, ok := out.Schema.(map[string]any)
	if !ok {
		t.Fatalf("ValidationSchema().Schema is not map[string]any: %T", out.Schema)
	}
	requiredAny, ok := root["required"].([]any)
	if !ok {
		t.Fatalf("root required is not []any: %T", root["required"])
	}
	requiredSet := map[string]bool{}
	for _, v := range requiredAny {
		if s, ok := v.(string); ok {
			requiredSet[s] = true
		}
	}
	for _, name := range []string{"has_inquiry", "summary", "verdict", "has_advisory"} {
		if !requiredSet[name] {
			t.Fatalf("validation schema required missing %q (got %v)", name, requiredAny)
		}
	}
	for _, name := range []string{"blocker_category", "blocker_fingerprint", "affected_artifacts", "recommended_action", "owner_boundary", "owner_spec"} {
		if requiredSet[name] {
			t.Fatalf("ValidationSchema() must not require workflow-specific extension field %q", name)
		}
	}

	props, ok := root["properties"].(map[string]any)
	if !ok {
		t.Fatalf("root properties is not map[string]any: %T", root["properties"])
	}
	for _, name := range []string{"has_inquiry", "summary", "inquiry", "verdict", "has_advisory", "advisory"} {
		if _, exists := props[name]; !exists {
			t.Fatalf("validation schema properties missing %q (keys=%v)", name, mapKeys(props))
		}
	}
	for _, name := range []string{"blocker_category", "blocker_fingerprint", "affected_artifacts", "recommended_action", "owner_boundary", "owner_spec"} {
		if _, exists := props[name]; exists {
			t.Fatalf("ValidationSchema() must remain generic and not define workflow-specific extension field %q", name)
		}
	}
}

// Schema() は呼び出しごとに新しい値を返す（共有された map を呼び出し側が書き換えても影響しない）
func TestSchemaIsFreshPerCall(t *testing.T) {
	a := Schema()
	b := Schema()
	if a == b {
		t.Fatalf("Schema() should return a fresh pointer each call")
	}
	// 中身の map も別インスタンスであるべき
	rootA, _ := a.Schema.(map[string]any)
	rootB, _ := b.Schema.(map[string]any)
	if rootA == nil || rootB == nil {
		t.Fatalf("Schema()'s root map missing")
	}
	// A 側を書き換えても B に波及しない
	rootA["properties"].(map[string]any)["has_inquiry"].(map[string]any)["type"] = "MUTATED"
	if got := rootB["properties"].(map[string]any)["has_inquiry"].(map[string]any)["type"]; got != "boolean" {
		t.Fatalf("Schema() leaked shared state across calls: got %v", got)
	}
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
