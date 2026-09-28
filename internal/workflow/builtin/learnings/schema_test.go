// Package learnings の本 test ファイルは task 6.5 (`JSONSchemaForReflector` /
// `JSONSchemaForCurator` + `CuratorOperation` 型 + skill 両端契約テスト) の RED → GREEN
// 検証を担う。設計契約は design.md §Data Contracts & Integration の
// Reflector / Curator response schema および requirements.md Req 4.4 / 6.2 / 6.4 を
// SSoT とし、JSON Schema 構造を機械的に assert する。
//
// 両端契約テスト (`TestSkillContractIntegrity_*`) は task 5.1 (skill SSoT 配置) が
// harness permission 都合で物理配置されていないとき `t.Skip` する seam を持つ。
// 5.1 解決後は skill 側 markdown 内に併記された JSON schema を抽出して Go 側
// schema 関数の戻り値と完全一致させる assert に自動で切替わる (再配線不要)。
//
// skip 理由文字列 ("skill SSoT not yet placed (task 5.1 blocked)") は固定で、
// 5.1 解決後に grep で除去対象を機械的に発見できるよう保つ。
package learnings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// skillRootCandidates は両端契約テストが skill SSoT を探索する candidate root の集合。
// テストは `internal/workflow/builtin/learnings` から実行されるため、repo root までの
// 相対 path で `.agents/skills/agent-learnings-curator/rules/` を解決する。
var skillRootCandidates = []string{
	filepath.Join("..", "..", "..", "..", ".agents", "skills", "agent-learnings-curator", "rules"),
}

// skillSkipReason は 5.1 解決後に grep で除去対象を機械的に発見できるよう固定する skip 理由。
const skillSkipReason = "skill SSoT not yet placed (task 5.1 blocked)"

// TestJSONSchemaForReflector_Shape は Reflector schema が
// `provider.OutputFormat{Type:"json_schema"}` 形を持ち、design.md API Contract — Reflector
// で固定された必須フィールド (learnings[] と learnings[] の各項目) を required に含み、
// `additionalProperties:false` を持つことを検証する (Req 4.4 / design.md API Contract — Reflector)。
func TestJSONSchemaForReflector_Shape(t *testing.T) {
	of := JSONSchemaForReflector()
	if of == nil {
		t.Fatal("JSONSchemaForReflector() は nil 不可")
	}
	if of.Type != "json_schema" {
		t.Errorf("Type = %q, want %q", of.Type, "json_schema")
	}
	schema, ok := of.Schema.(map[string]any)
	if !ok {
		t.Fatalf("Schema は map[string]any であるべき: got %T", of.Schema)
	}
	if schema["type"] != "object" {
		t.Errorf("schema.type = %v, want %q", schema["type"], "object")
	}
	if schema["additionalProperties"] != false {
		t.Errorf("schema.additionalProperties = %v, want false", schema["additionalProperties"])
	}
	// top-level required: learnings
	topRequired := requiredAsStrings(t, schema, "Reflector top-level")
	if !slices.Contains(topRequired, "learnings") {
		t.Errorf("Reflector top-level required に %q が含まれない: got %v", "learnings", topRequired)
	}

	// learnings プロパティ
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("Reflector schema.properties は map[string]any であるべき: got %T", schema["properties"])
	}
	learnings, ok := props["learnings"].(map[string]any)
	if !ok {
		t.Fatalf("Reflector schema.properties.learnings は map[string]any であるべき: got %T", props["learnings"])
	}
	if learnings["type"] != "array" {
		t.Errorf("learnings.type = %v, want %q", learnings["type"], "array")
	}
	items, ok := learnings["items"].(map[string]any)
	if !ok {
		t.Fatalf("Reflector learnings.items は map[string]any であるべき: got %T", learnings["items"])
	}
	if items["type"] != "object" {
		t.Errorf("learnings.items.type = %v, want %q", items["type"], "object")
	}
	if items["additionalProperties"] != false {
		t.Errorf("learnings.items.additionalProperties = %v, want false", items["additionalProperties"])
	}

	// items required: API Contract — Reflector の 4 フィールド
	itemRequired := requiredAsStrings(t, items, "Reflector learnings.items")
	wants := []string{"category", "context", "insight", "paths_glob"}
	for _, w := range wants {
		if !slices.Contains(itemRequired, w) {
			t.Errorf("Reflector learnings.items.required に %q が含まれない: got %v", w, itemRequired)
		}
	}

	// items.properties が string / array で型固定されていること
	itemProps, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("Reflector learnings.items.properties は map[string]any であるべき: got %T", items["properties"])
	}

	// category: closed enum 4 値
	cat, ok := itemProps["category"].(map[string]any)
	if !ok {
		t.Fatalf("Reflector learnings.items.properties.category は map[string]any であるべき: got %T", itemProps["category"])
	}
	if cat["type"] != "string" {
		t.Errorf("category.type = %v, want %q", cat["type"], "string")
	}
	catEnum, ok := cat["enum"].([]string)
	if !ok {
		t.Fatalf("category.enum は []string であるべき: got %T", cat["enum"])
	}
	wantCategories := []string{
		CategoryFailureRecovery,
		CategorySuccessProcedure,
		CategoryAntipattern,
		CategoryToolPattern,
	}
	if !equalUnordered(catEnum, wantCategories) {
		t.Errorf("category.enum = %v, want (任意順) %v", catEnum, wantCategories)
	}

	// context / insight: maxLength 512
	for _, key := range []string{"context", "insight"} {
		p, ok := itemProps[key].(map[string]any)
		if !ok {
			t.Fatalf("Reflector learnings.items.properties.%s は map[string]any であるべき: got %T", key, itemProps[key])
			continue
		}
		if p["type"] != "string" {
			t.Errorf("%s.type = %v, want %q", key, p["type"], "string")
		}
		if p["maxLength"] != reflectorTextMaxLength {
			t.Errorf("%s.maxLength = %v, want %d", key, p["maxLength"], reflectorTextMaxLength)
		}
	}

	// paths_glob: array, maxItems 5
	pg, ok := itemProps["paths_glob"].(map[string]any)
	if !ok {
		t.Fatalf("Reflector learnings.items.properties.paths_glob は map[string]any であるべき: got %T", itemProps["paths_glob"])
	}
	if pg["type"] != "array" {
		t.Errorf("paths_glob.type = %v, want %q", pg["type"], "array")
	}
	if pg["maxItems"] != reflectorPathsGlobMaxItems {
		t.Errorf("paths_glob.maxItems = %v, want %d", pg["maxItems"], reflectorPathsGlobMaxItems)
	}
	pgItems, ok := pg["items"].(map[string]any)
	if !ok {
		t.Fatalf("paths_glob.items は map[string]any であるべき: got %T", pg["items"])
	}
	if pgItems["type"] != "string" {
		t.Errorf("paths_glob.items.type = %v, want %q", pgItems["type"], "string")
	}
}

// TestJSONSchemaForReflector_AcceptsEmptyArray は learnings 配列が空でも schema が valid と
// 解釈される (= minItems を持たず空配列を accept する) ことを構造的に確認する (Req 4.4)。
func TestJSONSchemaForReflector_AcceptsEmptyArray(t *testing.T) {
	of := JSONSchemaForReflector()
	schema := of.Schema.(map[string]any)
	props := schema["properties"].(map[string]any)
	learnings := props["learnings"].(map[string]any)
	if _, hasMinItems := learnings["minItems"]; hasMinItems {
		t.Errorf("learnings.minItems を設定すべきでない (空配列が許容されないため): got %v", learnings["minItems"])
	}
}

// TestJSONSchemaForCurator_Shape は Curator schema が
// `provider.OutputFormat{Type:"json_schema"}` 形を持ち、design.md API Contract — Curator
// で固定された必須フィールド (operations[] と operations[] の各項目) を required に含み、
// `additionalProperties:false` を持つことを検証する (Req 6.2 / design.md API Contract — Curator)。
func TestJSONSchemaForCurator_Shape(t *testing.T) {
	of := JSONSchemaForCurator()
	if of == nil {
		t.Fatal("JSONSchemaForCurator() は nil 不可")
	}
	if of.Type != "json_schema" {
		t.Errorf("Type = %q, want %q", of.Type, "json_schema")
	}
	schema, ok := of.Schema.(map[string]any)
	if !ok {
		t.Fatalf("Schema は map[string]any であるべき: got %T", of.Schema)
	}
	if schema["type"] != "object" {
		t.Errorf("schema.type = %v, want %q", schema["type"], "object")
	}
	if schema["additionalProperties"] != false {
		t.Errorf("schema.additionalProperties = %v, want false", schema["additionalProperties"])
	}

	// top-level required: operations
	topRequired := requiredAsStrings(t, schema, "Curator top-level")
	if !slices.Contains(topRequired, "operations") {
		t.Errorf("Curator top-level required に %q が含まれない: got %v", "operations", topRequired)
	}

	// operations プロパティ
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("Curator schema.properties は map[string]any であるべき: got %T", schema["properties"])
	}
	operations, ok := props["operations"].(map[string]any)
	if !ok {
		t.Fatalf("Curator schema.properties.operations は map[string]any であるべき: got %T", props["operations"])
	}
	if operations["type"] != "array" {
		t.Errorf("operations.type = %v, want %q", operations["type"], "array")
	}
	items, ok := operations["items"].(map[string]any)
	if !ok {
		t.Fatalf("Curator operations.items は map[string]any であるべき: got %T", operations["items"])
	}
	if items["type"] != "object" {
		t.Errorf("operations.items.type = %v, want %q", items["type"], "object")
	}
	if items["additionalProperties"] != false {
		t.Errorf("operations.items.additionalProperties = %v, want false", items["additionalProperties"])
	}

	// items required: API Contract — Curator の 5 フィールド (op / category / topic / paths_glob / entry)
	itemRequired := requiredAsStrings(t, items, "Curator operations.items")
	wants := []string{"op", "category", "topic", "paths_glob", "entry"}
	for _, w := range wants {
		if !slices.Contains(itemRequired, w) {
			t.Errorf("Curator operations.items.required に %q が含まれない: got %v", w, itemRequired)
		}
	}

	itemProps, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("Curator operations.items.properties は map[string]any であるべき: got %T", items["properties"])
	}

	// op: closed enum ["add"] only
	op, ok := itemProps["op"].(map[string]any)
	if !ok {
		t.Fatalf("Curator operations.items.properties.op は map[string]any であるべき: got %T", itemProps["op"])
	}
	if op["type"] != "string" {
		t.Errorf("op.type = %v, want %q", op["type"], "string")
	}
	opEnum, ok := op["enum"].([]string)
	if !ok {
		t.Fatalf("op.enum は []string であるべき: got %T", op["enum"])
	}
	if !equalUnordered(opEnum, []string{"add"}) {
		t.Errorf("op.enum = %v, want %v", opEnum, []string{"add"})
	}

	// category: closed enum 4 値
	cat, ok := itemProps["category"].(map[string]any)
	if !ok {
		t.Fatalf("Curator operations.items.properties.category は map[string]any であるべき: got %T", itemProps["category"])
	}
	catEnum, ok := cat["enum"].([]string)
	if !ok {
		t.Fatalf("category.enum は []string であるべき: got %T", cat["enum"])
	}
	wantCategories := []string{
		CategoryFailureRecovery,
		CategorySuccessProcedure,
		CategoryAntipattern,
		CategoryToolPattern,
	}
	if !equalUnordered(catEnum, wantCategories) {
		t.Errorf("category.enum = %v, want (任意順) %v", catEnum, wantCategories)
	}

	// topic: string (kebab-case 検証は ValidTopicName で apply phase が行うため schema は string のみ)
	topic, ok := itemProps["topic"].(map[string]any)
	if !ok {
		t.Fatalf("Curator operations.items.properties.topic は map[string]any であるべき: got %T", itemProps["topic"])
	}
	if topic["type"] != "string" {
		t.Errorf("topic.type = %v, want %q", topic["type"], "string")
	}

	// entry: string maxLength 320
	entry, ok := itemProps["entry"].(map[string]any)
	if !ok {
		t.Fatalf("Curator operations.items.properties.entry は map[string]any であるべき: got %T", itemProps["entry"])
	}
	if entry["type"] != "string" {
		t.Errorf("entry.type = %v, want %q", entry["type"], "string")
	}
	if entry["maxLength"] != curatorEntryMaxLength {
		t.Errorf("entry.maxLength = %v, want %d", entry["maxLength"], curatorEntryMaxLength)
	}

	// paths_glob: array of string
	pg, ok := itemProps["paths_glob"].(map[string]any)
	if !ok {
		t.Fatalf("Curator operations.items.properties.paths_glob は map[string]any であるべき: got %T", itemProps["paths_glob"])
	}
	if pg["type"] != "array" {
		t.Errorf("paths_glob.type = %v, want %q", pg["type"], "array")
	}
	pgItems, ok := pg["items"].(map[string]any)
	if !ok {
		t.Fatalf("paths_glob.items は map[string]any であるべき: got %T", pg["items"])
	}
	if pgItems["type"] != "string" {
		t.Errorf("paths_glob.items.type = %v, want %q", pgItems["type"], "string")
	}
}

// TestJSONSchemaForCurator_AcceptsEmptyArray は operations 配列が空でも schema が valid と
// 解釈される (= minItems を持たず空配列を accept する) ことを構造的に確認する (Req 6.5 で
// 空配列は applied-no-op verdict 経路に使われるため空 accept が必須)。
func TestJSONSchemaForCurator_AcceptsEmptyArray(t *testing.T) {
	of := JSONSchemaForCurator()
	schema := of.Schema.(map[string]any)
	props := schema["properties"].(map[string]any)
	operations := props["operations"].(map[string]any)
	if _, hasMinItems := operations["minItems"]; hasMinItems {
		t.Errorf("operations.minItems を設定すべきでない (空配列が applied-no-op で accept されるため): got %v", operations["minItems"])
	}
}

// TestCuratorOperation_JSONRoundTrip は CuratorOperation 型が API Contract — Curator
// の JSON 形式 (`{op, category, topic, paths_glob, entry, summary, applies_to}`) と
// double-quoted snake_case で完全に round-trip することを検証する (Req 2.1 / 2.4 / 6.2)。
func TestCuratorOperation_JSONRoundTrip(t *testing.T) {
	src := CuratorOperation{
		Op:        "add",
		Category:  CategoryFailureRecovery,
		Topic:     "akashic",
		PathsGlob: []string{"internal/akashic/**", "internal/workflow/builtin/learnings/**"},
		Entry:     "Recorder.RecordLearning は Apply phase で paths_glob が空なら reject すること",
		Summary:   "akashic Recorder は paths_glob 空を reject する",
		AppliesTo: "agent-learnings の Apply phase で operation を validate するとき",
	}

	encoded, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("json.Marshal(CuratorOperation) = %v, want nil", err)
	}

	// 文字列 key が JSON Schema と一致するか (snake_case の paths_glob / summary / applies_to)
	if !strings.Contains(string(encoded), `"paths_glob"`) {
		t.Errorf("encoded JSON が paths_glob key を含まない: got %s", string(encoded))
	}
	if strings.Contains(string(encoded), `"PathsGlob"`) {
		t.Errorf("encoded JSON に PascalCase の PathsGlob が混入: got %s", string(encoded))
	}
	if !strings.Contains(string(encoded), `"summary"`) {
		t.Errorf("encoded JSON が summary key を含まない: got %s", string(encoded))
	}
	if strings.Contains(string(encoded), `"Summary"`) {
		t.Errorf("encoded JSON に PascalCase の Summary が混入: got %s", string(encoded))
	}
	if !strings.Contains(string(encoded), `"applies_to"`) {
		t.Errorf("encoded JSON が applies_to key を含まない: got %s", string(encoded))
	}
	if strings.Contains(string(encoded), `"AppliesTo"`) {
		t.Errorf("encoded JSON に PascalCase の AppliesTo が混入: got %s", string(encoded))
	}

	var dst CuratorOperation
	if err := json.Unmarshal(encoded, &dst); err != nil {
		t.Fatalf("json.Unmarshal(encoded) = %v, want nil", err)
	}
	if !reflect.DeepEqual(src, dst) {
		t.Errorf("round-trip 不一致:\nsrc = %+v\ndst = %+v", src, dst)
	}
}

// TestJSONSchemaForCurator_SummaryAndAppliesTo は Curator schema が
// design.md §Data Models (CuratorOperation Schema 拡張) で固定された summary /
// applies_to フィールドを required に含み、両者とも type:string + maxLength:100
// で properties に登録されていることを検証する (Req 2.1 / 2.4)。
//
// summary / applies_to は 1 行 100 字以下の必須フィールドであり、Apply phase が
// section heading 直下の `**summary:**` / `**適用条件:**` 行に変換して書き出す。
// schema レベルで maxLength を 100 に固定することで、LLM 出力段階で長文や改行混入を
// 弾く (改行を含むかどうかの validation は Apply phase が担当)。
func TestJSONSchemaForCurator_SummaryAndAppliesTo(t *testing.T) {
	of := JSONSchemaForCurator()
	schema := of.Schema.(map[string]any)
	props := schema["properties"].(map[string]any)
	operations := props["operations"].(map[string]any)
	items := operations["items"].(map[string]any)

	// items.required に summary / applies_to が含まれること
	itemRequired := requiredAsStrings(t, items, "Curator operations.items")
	for _, w := range []string{"summary", "applies_to"} {
		if !slices.Contains(itemRequired, w) {
			t.Errorf("Curator operations.items.required に %q が含まれない: got %v", w, itemRequired)
		}
	}

	itemProps := items["properties"].(map[string]any)

	// summary: type:string, maxLength:100
	summary, ok := itemProps["summary"].(map[string]any)
	if !ok {
		t.Fatalf("Curator operations.items.properties.summary は map[string]any であるべき: got %T", itemProps["summary"])
	}
	if summary["type"] != "string" {
		t.Errorf("summary.type = %v, want %q", summary["type"], "string")
	}
	if summary["maxLength"] != curatorSummaryMaxLength {
		t.Errorf("summary.maxLength = %v, want %d", summary["maxLength"], curatorSummaryMaxLength)
	}

	// applies_to: type:string, maxLength:100
	appliesTo, ok := itemProps["applies_to"].(map[string]any)
	if !ok {
		t.Fatalf("Curator operations.items.properties.applies_to は map[string]any であるべき: got %T", itemProps["applies_to"])
	}
	if appliesTo["type"] != "string" {
		t.Errorf("applies_to.type = %v, want %q", appliesTo["type"], "string")
	}
	if appliesTo["maxLength"] != curatorAppliesToMaxLength {
		t.Errorf("applies_to.maxLength = %v, want %d", appliesTo["maxLength"], curatorAppliesToMaxLength)
	}

	// 既存 paths_glob フィールドは後方互換のため schema に残る (Req 2.4)
	if _, exists := itemProps["paths_glob"]; !exists {
		t.Errorf("Curator operations.items.properties に paths_glob が残っていない (Req 2.4 後方互換)")
	}
}

// TestSkillContractIntegrity_ReflectorRules は task 5.1 が配置する
// `.agents/skills/agent-learnings-curator/rules/reflector.md` 内に併記された
// JSON schema が、Go 側 `JSONSchemaForReflector()` の戻り値と完全一致することを検証する
// 両端契約テスト (task 6.5 観測可能完了条件)。
//
// task 5.1 が harness permission で物理配置されていない間は `t.Skip` し、
// 5.1 解決後に自動で assert 経路に切替わる skip seam を持つ。
func TestSkillContractIntegrity_ReflectorRules(t *testing.T) {
	skillFile, ok := findSkillFile(t, "reflector.md")
	if !ok {
		t.Skipf("%s: %s", skillSkipReason, "reflector.md not found")
	}

	body, err := os.ReadFile(skillFile)
	if err != nil {
		t.Fatalf("os.ReadFile(%s) = %v", skillFile, err)
	}

	skillSchema, err := extractFencedJSON(string(body))
	if err != nil {
		t.Fatalf("extractFencedJSON(%s) = %v", skillFile, err)
	}

	// skill 側 fenced JSON は inner schema object のみを含むため、Go 側も
	// `*provider.OutputFormat.Schema` (= 内側 map[string]any) を比較対象にする。
	goSchema := mustMarshalGoSchema(t, JSONSchemaForReflector().Schema)
	if !jsonEqual(t, skillSchema, goSchema) {
		t.Errorf("両端契約不一致 (reflector):\nskill = %s\ngo    = %s", string(skillSchema), string(goSchema))
	}
}

// TestSkillContractIntegrity_CuratorRules は task 5.1 が配置する
// `.agents/skills/agent-learnings-curator/rules/curator.md` 内に併記された
// **出力 schema** (Curator API Contract / `{operations: [...]}`) が、Go 側 `JSONSchemaForCurator()` の
// 戻り値と完全一致することを検証する両端契約テスト (task 6.5 観測可能完了条件)。
//
// curator.md は schema を 2 つ持つ:
//   - 1 つ目: 入力 schema (`learnings.json`) — `TestSkillContractIntegrity_LearningsArtifactSchema` が pin
//   - 2 つ目: 出力 schema (`{operations: [...]}`) — 本 test が pin
//
// task 5.1 が harness permission で物理配置されていない間は `t.Skip` し、
// 5.1 解決後に自動で assert 経路に切替わる skip seam を持つ。
func TestSkillContractIntegrity_CuratorRules(t *testing.T) {
	skillFile, ok := findSkillFile(t, "curator.md")
	if !ok {
		t.Skipf("%s: %s", skillSkipReason, "curator.md not found")
	}

	body, err := os.ReadFile(skillFile)
	if err != nil {
		t.Fatalf("os.ReadFile(%s) = %v", skillFile, err)
	}

	skillSchema, err := extractSecondFencedJSON(string(body))
	if err != nil {
		t.Fatalf("extractSecondFencedJSON(%s) = %v", skillFile, err)
	}

	// task 1.2 で Go 側 schema に summary / applies_to を追加した時点で
	// skill 側 fenced JSON はまだ task 4.2 で更新されておらず両端契約不一致になる。
	// task 4.2 が curator.md を新 schema に同期するまでの間は skip し、
	// 同期完了後は自動で assert 経路に切替わる skip seam を入れる。
	// 判定は skill 側 fenced JSON に `"summary"` / `"applies_to"` keys が存在するかで行う。
	if !strings.Contains(string(skillSchema), `"summary"`) || !strings.Contains(string(skillSchema), `"applies_to"`) {
		t.Skipf("%s: %s", skillSkipReason, "curator.md output schema not yet updated for summary/applies_to (task 4.2 pending)")
	}

	// skill 側 fenced JSON は inner schema object のみを含むため、Go 側も
	// `*provider.OutputFormat.Schema` (= 内側 map[string]any) を比較対象にする。
	goSchema := mustMarshalGoSchema(t, JSONSchemaForCurator().Schema)
	if !jsonEqual(t, skillSchema, goSchema) {
		t.Errorf("両端契約不一致 (curator):\nskill = %s\ngo    = %s", string(skillSchema), string(goSchema))
	}
}

// findSkillFile は skillRootCandidates のいずれかに `<name>` が存在するか調べ、
// 最初に見つかった絶対 path を返す。1 件も無ければ false を返す (5.1 未配置経路)。
func findSkillFile(t *testing.T, name string) (string, bool) {
	t.Helper()
	for _, root := range skillRootCandidates {
		candidate := filepath.Join(root, name)
		if _, err := os.Stat(candidate); err == nil {
			abs, err := filepath.Abs(candidate)
			if err != nil {
				t.Fatalf("filepath.Abs(%s) = %v", candidate, err)
			}
			return abs, true
		}
	}
	return "", false
}

// extractFencedJSON は markdown 内の最初の `\n```json\n ... \n```` 行頭 fenced block を抽出する。
// skill 側 markdown は schema を行頭の ```json ... ``` で囲み、説明文中で inline backtick
// (` ```json `) を持つことがあるため、開始 / 終端ともに行頭に anchor して誤検出を避ける。
func extractFencedJSON(body string) ([]byte, error) {
	const opening = "\n```json\n"
	const closing = "\n```"
	// body 先頭から fenced block が始まる場合 (= newline anchor 不在) の救済として "\n" prefix
	probe := "\n" + body
	start := strings.Index(probe, opening)
	if start < 0 {
		return nil, &skillSchemaParseError{Reason: "行頭 ```json fenced block not found"}
	}
	rest := probe[start+len(opening):]
	end := strings.Index(rest, closing)
	if end < 0 {
		return nil, &skillSchemaParseError{Reason: "fenced block の終端 ``` が見つからない (行頭 anchor)"}
	}
	return []byte(strings.TrimSpace(rest[:end])), nil
}

// mustMarshalGoSchema は Go 側 *provider.OutputFormat を skill 側 fenced JSON と
// 比較可能な canonical JSON に変換する。比較は構造比較 (jsonEqual) で行うため、
// この時点の bytes は assertion error メッセージにのみ使われる。
func mustMarshalGoSchema(t *testing.T, of any) []byte {
	t.Helper()
	encoded, err := json.Marshal(of)
	if err != nil {
		t.Fatalf("json.Marshal(go schema) = %v", err)
	}
	return encoded
}

// jsonEqual は 2 つの JSON byte slice を構造的に等価判定する (key 順序や空白に依存しない)。
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("json.Unmarshal(skill schema) = %v\nbody = %s", err, string(a))
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("json.Unmarshal(go schema) = %v\nbody = %s", err, string(b))
	}
	return reflect.DeepEqual(av, bv)
}

// skillSchemaParseError は extractFencedJSON が skill markdown から JSON block を切り出せ
// なかった場合に返される。assertion error 文に reason を含めるため value type を持つ。
type skillSchemaParseError struct{ Reason string }

func (e *skillSchemaParseError) Error() string { return "skill schema parse error: " + e.Reason }

// requiredAsStrings は schema map から `required` フィールドを []string として取り出す小ヘルパー。
// distiller / improve patch の同パターン (`schema.required` を []string で cast) を踏襲。
func requiredAsStrings(t *testing.T, schema map[string]any, label string) []string {
	t.Helper()
	required, ok := schema["required"].([]string)
	if !ok {
		t.Fatalf("%s.required は []string であるべき: got %T", label, schema["required"])
	}
	return required
}

// TestJSONSchemaForLearningsArtifact_Shape は `<runDir>/learnings.json` 用 schema が
// design.md / requirements.md Req 8.7 で固定された artifact 構造 (top-level メタ +
// learnings[] の各 entry が provenance + LLM-emitted を持つ) を反映することを検証する。
//
// 本 schema は LLM の OutputFormat ではなく Go 側 writer / curator skill (Read 経路) の
// 両端契約 SSoT として使われる。検証は内側 schema map (top-level required / properties /
// learnings.items.required の 9 fields) を pin する。
func TestJSONSchemaForLearningsArtifact_Shape(t *testing.T) {
	schema := JSONSchemaForLearningsArtifact()
	if schema == nil {
		t.Fatal("JSONSchemaForLearningsArtifact() は nil 不可")
	}
	if schema["type"] != "object" {
		t.Errorf("type = %v, want %q", schema["type"], "object")
	}
	if schema["additionalProperties"] != false {
		t.Errorf("additionalProperties = %v, want false", schema["additionalProperties"])
	}
	topRequired := requiredAsStrings(t, schema, "Learnings artifact top-level")
	for _, w := range []string{"schema_version", "parent_run_id", "generated_at", "learnings"} {
		if !slices.Contains(topRequired, w) {
			t.Errorf("top-level required に %q が含まれない: got %v", w, topRequired)
		}
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties は map[string]any: got %T", schema["properties"])
	}
	learnings, ok := props["learnings"].(map[string]any)
	if !ok {
		t.Fatalf("properties.learnings は map[string]any: got %T", props["learnings"])
	}
	if learnings["type"] != "array" {
		t.Errorf("learnings.type = %v, want array", learnings["type"])
	}
	items, ok := learnings["items"].(map[string]any)
	if !ok {
		t.Fatalf("learnings.items は map[string]any: got %T", learnings["items"])
	}
	if items["type"] != "object" {
		t.Errorf("learnings.items.type = %v, want object", items["type"])
	}
	if items["additionalProperties"] != false {
		t.Errorf("learnings.items.additionalProperties = %v, want false", items["additionalProperties"])
	}
	itemRequired := requiredAsStrings(t, items, "Learnings artifact learnings.items")
	wantItemRequired := []string{
		"id", "session_id", "step_id", "provider_id",
		"category", "context", "insight", "paths_glob", "recorded_at",
	}
	for _, w := range wantItemRequired {
		if !slices.Contains(itemRequired, w) {
			t.Errorf("learnings.items.required に %q が含まれない: got %v", w, itemRequired)
		}
	}

	itemProps, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("learnings.items.properties は map[string]any: got %T", items["properties"])
	}
	// category enum 4 値を pin (reflector schema と SoT 一致)
	cat, ok := itemProps["category"].(map[string]any)
	if !ok {
		t.Fatalf("category は map[string]any: got %T", itemProps["category"])
	}
	catEnum, ok := cat["enum"].([]string)
	if !ok {
		t.Fatalf("category.enum は []string: got %T", cat["enum"])
	}
	wantCategories := []string{
		CategoryFailureRecovery, CategorySuccessProcedure,
		CategoryAntipattern, CategoryToolPattern,
	}
	if !equalUnordered(catEnum, wantCategories) {
		t.Errorf("category.enum = %v, want %v", catEnum, wantCategories)
	}
	// paths_glob 上限 / 下限を reflector schema と SoT 一致させる
	pg, ok := itemProps["paths_glob"].(map[string]any)
	if !ok {
		t.Fatalf("paths_glob は map[string]any: got %T", itemProps["paths_glob"])
	}
	if pg["minItems"] != pathsGlobMinItems {
		t.Errorf("paths_glob.minItems = %v, want %d", pg["minItems"], pathsGlobMinItems)
	}
	if pg["maxItems"] != reflectorPathsGlobMaxItems {
		t.Errorf("paths_glob.maxItems = %v, want %d", pg["maxItems"], reflectorPathsGlobMaxItems)
	}
}

// TestSkillContractIntegrity_LearningsArtifactSchema は curator.md 内に併記された
// `learnings.json` schema の fenced JSON block が `JSONSchemaForLearningsArtifact()` の
// 戻り値と完全一致することを検証する両端契約テスト。
//
// curator.md は schema を **2 つ** 持つ:
//
//   - 1 つ目: Curator phase の入力 (`<runDir>/learnings.json`) — 本 test が pin
//   - 2 つ目: Curator phase 自身の出力 (`{operations: [...]}`) —
//     `TestSkillContractIntegrity_CuratorRules` が pin
//
// 順序は curator.md の節順 (## 入力 → ## 出力 schema) に合わせて固定する。
// 抽出は curator.md 内の **1 つ目の** `\n```json\n ... \n```` block を対象にする
// (2 つ目は出力 schema)。
func TestSkillContractIntegrity_LearningsArtifactSchema(t *testing.T) {
	skillFile, ok := findSkillFile(t, "curator.md")
	if !ok {
		t.Skipf("%s: %s", skillSkipReason, "curator.md not found")
	}
	body, err := os.ReadFile(skillFile)
	if err != nil {
		t.Fatalf("os.ReadFile(%s) = %v", skillFile, err)
	}
	skillSchema, err := extractFencedJSON(string(body))
	if err != nil {
		t.Fatalf("extractFencedJSON(%s) = %v", skillFile, err)
	}
	goSchema := mustMarshalGoSchema(t, JSONSchemaForLearningsArtifact())
	if !jsonEqual(t, skillSchema, goSchema) {
		t.Errorf("両端契約不一致 (learnings artifact):\nskill = %s\ngo    = %s", string(skillSchema), string(goSchema))
	}
}

// extractSecondFencedJSON は markdown 内の **2 つ目の** 行頭 `\n```json\n ... \n```\n` block を抽出する。
// curator.md は出力 schema (1 つ目) と入力 learnings.json schema (2 つ目) を併記するため
// 本 helper で 2 つ目を取り出す。
func extractSecondFencedJSON(body string) ([]byte, error) {
	const opening = "\n```json\n"
	const closing = "\n```"
	probe := "\n" + body
	first := strings.Index(probe, opening)
	if first < 0 {
		return nil, &skillSchemaParseError{Reason: "1 つ目の ```json fenced block not found"}
	}
	rest := probe[first+len(opening):]
	end := strings.Index(rest, closing)
	if end < 0 {
		return nil, &skillSchemaParseError{Reason: "1 つ目の fenced block の終端 ``` が見つからない"}
	}
	rest = rest[end+len(closing):]
	second := strings.Index(rest, opening)
	if second < 0 {
		return nil, &skillSchemaParseError{Reason: "2 つ目の ```json fenced block not found"}
	}
	rest = rest[second+len(opening):]
	end2 := strings.Index(rest, closing)
	if end2 < 0 {
		return nil, &skillSchemaParseError{Reason: "2 つ目の fenced block の終端 ``` が見つからない"}
	}
	return []byte(strings.TrimSpace(rest[:end2])), nil
}

// equalUnordered は 2 つの []string を順不同で要素一致判定する。
// closed enum の順序は API Contract で固定されていないため (= map iteration 由来でも OK)、
// 順序非依存で比較する。
func equalUnordered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := map[string]int{}
	for _, s := range a {
		counts[s]++
	}
	for _, s := range b {
		counts[s]--
	}
	for _, v := range counts {
		if v != 0 {
			return false
		}
	}
	return true
}
