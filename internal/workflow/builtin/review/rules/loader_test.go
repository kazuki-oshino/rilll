package rules

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// validRuleYAML はテスト用に有効な rule YAML を組み立てるヘルパ。
//
// テスト 1 ファイルあたりに必要な必須 6 フィールドをすべて備えた YAML を返す。
// `name` のみ呼び出し側で差し替え可能。
func validRuleYAML(name string) string {
	return strings.Join([]string{
		"name: " + name,
		"description: テスト用 rule",
		"provider: claude",
		"model: claude-sonnet-4-5",
		"applies_when: 常に適用",
		"prompt: |",
		"  この rule をレビューしてください",
		"",
	}, "\n")
}

// writeRuleFile は rules dir 配下に YAML ファイルを書き出すテストヘルパ。
//
// `dir` は `<cwd>/.rilll-config/review-rules` を想定。`basename` には `.yaml` 拡張子を含める。
func writeRuleFile(t *testing.T, dir, basename, content string) {
	t.Helper()
	path := filepath.Join(dir, basename)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// setupRulesDir は cwd 配下に `.rilll-config/review-rules` を作成し、
// rules dir の絶対パスと cwd 絶対パスを返すヘルパ。
func setupRulesDir(t *testing.T) (cwd, rulesDir string) {
	t.Helper()
	cwd = t.TempDir()
	rulesDir = filepath.Join(cwd, ".rilll-config", "review-rules")
	if err := os.MkdirAll(rulesDir, 0o755); err != nil {
		t.Fatalf("mkdir rules dir: %v", err)
	}
	return cwd, rulesDir
}

// TestIsValidRuleName は kebab-case 規約と予約名の組み合わせを検証する
// (Requirements 1.3 + Reserved name 規約)。
func TestIsValidRuleName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		want bool
	}{
		// 採用される kebab-case
		{"security-scan", true},
		{"docstyle", true},
		{"k8s-1-25", true},
		{"a", true},
		{"123", true},
		// kebab-case 違反
		{"", false},               // 空文字
		{"Security-Scan", false},  // 大文字
		{"-leading", false},       // 先頭 -
		{"trailing-", false},      // 末尾 -
		{"double--hyphen", false}, // 連続 -
		{"under_score", false},    // _ 不可
		{"with space", false},     // 空白不可
		{"日本語", false},            // 非 ASCII
		// 予約名 (kebab-case には合致するが reject)
		{"result", false},
		{"context", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsValidRuleName(tc.name); got != tc.want {
				t.Fatalf("IsValidRuleName(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestLoadRules_HappyPath は採用される YAML が複数あるとき、ファイル名昇順で
// `[]Rule` が返り、各 Rule の `SourceFile` に basename が埋まることを検証する
// (Requirements 1.1, 1.2, 1.5)。
func TestLoadRules_HappyPath(t *testing.T) {
	t.Parallel()

	cwd, dir := setupRulesDir(t)

	// 故意に逆順で書き出して、ファイル名昇順に正規化されることを確かめる。
	writeRuleFile(t, dir, "z-last.yaml", validRuleYAML("z-last"))
	writeRuleFile(t, dir, "a-first.yaml", validRuleYAML("a-first"))
	writeRuleFile(t, dir, "m-middle.yaml", validRuleYAML("m-middle"))

	rules, warnings, err := LoadRules(cwd)
	if err != nil {
		t.Fatalf("LoadRules error: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings expected empty, got: %+v", warnings)
	}

	gotNames := make([]string, len(rules))
	gotFiles := make([]string, len(rules))
	for i, r := range rules {
		gotNames[i] = r.Name
		gotFiles[i] = r.SourceFile
	}

	wantNames := []string{"a-first", "m-middle", "z-last"}
	wantFiles := []string{"a-first.yaml", "m-middle.yaml", "z-last.yaml"}

	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Errorf("rule order mismatch: got=%v want=%v", gotNames, wantNames)
	}
	if !reflect.DeepEqual(gotFiles, wantFiles) {
		t.Errorf("source files mismatch: got=%v want=%v", gotFiles, wantFiles)
	}

	// 必須フィールドが全て埋まっていることを 1 件代表で確認 (Requirements 1.2)。
	first := rules[0]
	if first.Description == "" || first.Provider == "" || first.Model == "" ||
		first.AppliesWhen == "" || first.Prompt == "" {
		t.Errorf("required fields missing in parsed rule: %+v", first)
	}
}

// TestLoadRules_SubdirectoryIgnored はサブディレクトリ配下の YAML が無視されることを
// 検証する (Requirements 1.1)。
func TestLoadRules_SubdirectoryIgnored(t *testing.T) {
	t.Parallel()

	cwd, dir := setupRulesDir(t)
	writeRuleFile(t, dir, "top-level.yaml", validRuleYAML("top-level"))

	// サブディレクトリを作って同名でない rule を置く -> 採用されないこと。
	sub := filepath.Join(dir, "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	writeRuleFile(t, sub, "nested-rule.yaml", validRuleYAML("nested-rule"))

	rules, warnings, err := LoadRules(cwd)
	if err != nil {
		t.Fatalf("LoadRules error: %v", err)
	}
	if len(rules) != 1 || rules[0].Name != "top-level" {
		t.Errorf("expected only top-level rule, got: %+v", rules)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings expected empty, got: %+v", warnings)
	}
}

// TestLoadRules_NonYamlIgnored は `.yaml` 以外のファイルが無視されることを検証する
// (Requirements 1.1, `.yml` も対象外)。
func TestLoadRules_NonYamlIgnored(t *testing.T) {
	t.Parallel()

	cwd, dir := setupRulesDir(t)
	writeRuleFile(t, dir, "good.yaml", validRuleYAML("good"))
	writeRuleFile(t, dir, "ignored.yml", validRuleYAML("ignored-yml"))
	writeRuleFile(t, dir, "README.md", "not a rule")
	writeRuleFile(t, dir, "config.json", `{"name":"json-rule"}`)

	rules, warnings, err := LoadRules(cwd)
	if err != nil {
		t.Fatalf("LoadRules error: %v", err)
	}
	if len(rules) != 1 || rules[0].Name != "good" {
		t.Errorf("expected only good rule, got: %+v", rules)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings expected empty, got: %+v", warnings)
	}
}

// TestLoadRules_RulesDirMissing はディレクトリ自体が無い場合に `ErrRulesDirMissing`
// が返ることを検証する (Requirements 3.2)。
func TestLoadRules_RulesDirMissing(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir() // .rilll-config/review-rules を作らない

	rules, warnings, err := LoadRules(cwd)
	if !errors.Is(err, ErrRulesDirMissing) {
		t.Fatalf("expected ErrRulesDirMissing, got: %v", err)
	}
	if rules != nil {
		t.Errorf("expected nil rules, got: %+v", rules)
	}
	if warnings != nil {
		t.Errorf("expected nil warnings, got: %+v", warnings)
	}
}

// TestLoadRules_NoRulesFound_EmptyDir はディレクトリは存在するが `*.yaml` が 0 件のとき
// `ErrNoRulesFound` を返すことを検証する (Requirements 3.2)。
func TestLoadRules_NoRulesFound_EmptyDir(t *testing.T) {
	t.Parallel()

	cwd, _ := setupRulesDir(t)

	rules, warnings, err := LoadRules(cwd)
	if !errors.Is(err, ErrNoRulesFound) {
		t.Fatalf("expected ErrNoRulesFound, got: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("expected 0 rules, got: %+v", rules)
	}
	// 空ディレクトリでは Warning も無いはず。
	if len(warnings) != 0 {
		t.Errorf("expected 0 warnings, got: %+v", warnings)
	}
}

// TestLoadRules_NoRulesFound_AllSkipped は YAML は存在するが全て Warning に落ちて
// 採用 0 件になった場合も `ErrNoRulesFound` を返すこと、Warning は維持されることを検証する。
func TestLoadRules_NoRulesFound_AllSkipped(t *testing.T) {
	t.Parallel()

	cwd, dir := setupRulesDir(t)
	// 必須欠落 (description が無い) のため skip される。
	writeRuleFile(t, dir, "broken.yaml", strings.Join([]string{
		"name: broken",
		"provider: claude",
		"model: m",
		"applies_when: when",
		"prompt: p",
		"",
	}, "\n"))

	rules, warnings, err := LoadRules(cwd)
	if !errors.Is(err, ErrNoRulesFound) {
		t.Fatalf("expected ErrNoRulesFound, got: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("expected 0 rules, got: %+v", rules)
	}
	if len(warnings) != 1 || warnings[0].File != "broken.yaml" {
		t.Errorf("expected single warning for broken.yaml, got: %+v", warnings)
	}
	if !strings.Contains(warnings[0].Reason, "description") {
		t.Errorf("warning reason should mention description, got: %q", warnings[0].Reason)
	}
}

// TestLoadRules_RequiredFieldMissing は必須フィールド欠落時に当該 rule のみ Warning + skip
// されつつ、他 rule は採用継続されることを検証する (Requirements 1.3)。
func TestLoadRules_RequiredFieldMissing(t *testing.T) {
	t.Parallel()

	missingTargets := []string{"name", "description", "provider", "model", "applies_when", "prompt"}

	for _, missing := range missingTargets {
		t.Run("missing-"+missing, func(t *testing.T) {
			t.Parallel()
			cwd, dir := setupRulesDir(t)

			// 1 件は欠落、もう 1 件は valid。valid 側が採用継続することを確認する。
			lines := []string{
				"name: bad-rule",
				"description: 不完全な rule",
				"provider: claude",
				"model: m",
				"applies_when: when",
				"prompt: p",
			}
			// 該当行を除外。
			filtered := make([]string, 0, len(lines)-1)
			for _, ln := range lines {
				if strings.HasPrefix(ln, missing+":") {
					continue
				}
				filtered = append(filtered, ln)
			}
			writeRuleFile(t, dir, "bad.yaml", strings.Join(filtered, "\n")+"\n")
			writeRuleFile(t, dir, "good.yaml", validRuleYAML("good"))

			rules, warnings, err := LoadRules(cwd)
			if err != nil {
				t.Fatalf("LoadRules error: %v", err)
			}
			if len(rules) != 1 || rules[0].Name != "good" {
				t.Errorf("expected only good rule, got: %+v", rules)
			}
			if len(warnings) != 1 || warnings[0].File != "bad.yaml" {
				t.Errorf("expected single warning for bad.yaml, got: %+v", warnings)
			}
			// warning reason には欠落フィールド名が含まれるはず。
			if !strings.Contains(warnings[0].Reason, missing) {
				t.Errorf("warning reason should mention %q, got: %q", missing, warnings[0].Reason)
			}
		})
	}
}

// TestLoadRules_KebabCaseViolation は `name` が kebab-case 違反のとき当該 rule のみ
// Warning + skip され、有効な rule は採用継続されることを検証する (Requirements 1.3)。
func TestLoadRules_KebabCaseViolation(t *testing.T) {
	t.Parallel()

	cwd, dir := setupRulesDir(t)

	// PascalCase / 末尾ハイフン / アンダースコアは kebab-case 違反。
	writeRuleFile(t, dir, "pascal.yaml", validRuleYAML("PascalCase"))
	writeRuleFile(t, dir, "trailing.yaml", validRuleYAML("trailing-"))
	writeRuleFile(t, dir, "underscore.yaml", validRuleYAML("under_score"))
	writeRuleFile(t, dir, "good.yaml", validRuleYAML("good"))

	rules, warnings, err := LoadRules(cwd)
	if err != nil {
		t.Fatalf("LoadRules error: %v", err)
	}
	if len(rules) != 1 || rules[0].Name != "good" {
		t.Errorf("expected only good rule, got: %+v", rules)
	}
	if len(warnings) != 3 {
		t.Fatalf("expected 3 warnings, got %d: %+v", len(warnings), warnings)
	}
	for _, w := range warnings {
		if !strings.Contains(w.Reason, "kebab-case") {
			t.Errorf("warning %q should mention kebab-case, got: %q", w.File, w.Reason)
		}
	}
}

// TestLoadRules_ReservedName は予約名 (`result` / `context`) を持つ rule が
// Warning + skip されることを検証する (Requirements 1.3 + Reserved name 規約)。
func TestLoadRules_ReservedName(t *testing.T) {
	t.Parallel()

	cwd, dir := setupRulesDir(t)
	writeRuleFile(t, dir, "ctx.yaml", validRuleYAML("context"))
	writeRuleFile(t, dir, "res.yaml", validRuleYAML("result"))
	writeRuleFile(t, dir, "good.yaml", validRuleYAML("good"))

	rules, warnings, err := LoadRules(cwd)
	if err != nil {
		t.Fatalf("LoadRules error: %v", err)
	}
	if len(rules) != 1 || rules[0].Name != "good" {
		t.Errorf("expected only good rule, got: %+v", rules)
	}
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings, got %d: %+v", len(warnings), warnings)
	}
	for _, w := range warnings {
		if !strings.Contains(w.Reason, "予約") {
			t.Errorf("warning %q should mention reserved, got: %q", w.File, w.Reason)
		}
	}
}

// TestLoadRules_UnknownField は YAML に schema 外フィールドが含まれる rule が
// `Decoder.KnownFields(true)` により Warning + skip されることを検証する (Requirements 1.3)。
func TestLoadRules_UnknownField(t *testing.T) {
	t.Parallel()

	cwd, dir := setupRulesDir(t)

	// `unknown` フィールドを含む YAML。`KnownFields(true)` で skip されるはず。
	withUnknown := validRuleYAML("with-unknown") + "unknown_field: extra\n"
	writeRuleFile(t, dir, "extra.yaml", withUnknown)
	writeRuleFile(t, dir, "good.yaml", validRuleYAML("good"))

	rules, warnings, err := LoadRules(cwd)
	if err != nil {
		t.Fatalf("LoadRules error: %v", err)
	}
	if len(rules) != 1 || rules[0].Name != "good" {
		t.Errorf("expected only good rule, got: %+v", rules)
	}
	if len(warnings) != 1 || warnings[0].File != "extra.yaml" {
		t.Errorf("expected single warning for extra.yaml, got: %+v", warnings)
	}
	if !strings.Contains(warnings[0].Reason, "yaml decode") {
		t.Errorf("warning reason should mention yaml decode, got: %q", warnings[0].Reason)
	}
}

// TestLoadRules_YAMLSyntaxError は構文エラー YAML が当該 rule のみ Warning + skip
// されることを検証する。
func TestLoadRules_YAMLSyntaxError(t *testing.T) {
	t.Parallel()

	cwd, dir := setupRulesDir(t)
	writeRuleFile(t, dir, "broken.yaml", ":\n  invalid: : yaml ::\n")
	writeRuleFile(t, dir, "good.yaml", validRuleYAML("good"))

	rules, warnings, err := LoadRules(cwd)
	if err != nil {
		t.Fatalf("LoadRules error: %v", err)
	}
	if len(rules) != 1 || rules[0].Name != "good" {
		t.Errorf("expected only good rule, got: %+v", rules)
	}
	if len(warnings) != 1 || warnings[0].File != "broken.yaml" {
		t.Errorf("expected single warning for broken.yaml, got: %+v", warnings)
	}
}

// TestLoadRules_DuplicateName は採用候補内で同一 `Name` が複数存在するとき
// `*DuplicateRuleNameError` を返し、`errors.Is(err, ErrDuplicateRuleName)` が
// 成立することを検証する (Requirements 1.4)。
func TestLoadRules_DuplicateName(t *testing.T) {
	t.Parallel()

	cwd, dir := setupRulesDir(t)

	// 同一 name "dup" を 2 ファイルに書く。
	writeRuleFile(t, dir, "first.yaml", validRuleYAML("dup"))
	writeRuleFile(t, dir, "second.yaml", validRuleYAML("dup"))

	rules, _, err := LoadRules(cwd)
	if err == nil {
		t.Fatalf("expected error, got nil (rules=%+v)", rules)
	}
	if !errors.Is(err, ErrDuplicateRuleName) {
		t.Fatalf("expected errors.Is(err, ErrDuplicateRuleName), got: %v", err)
	}
	var dup *DuplicateRuleNameError
	if !errors.As(err, &dup) {
		t.Fatalf("expected *DuplicateRuleNameError, got: %v", err)
	}
	if dup.Name != "dup" {
		t.Errorf("dup.Name = %q, want %q", dup.Name, "dup")
	}
	gotFiles := append([]string(nil), dup.Files...)
	sort.Strings(gotFiles)
	wantFiles := []string{"first.yaml", "second.yaml"}
	if !reflect.DeepEqual(gotFiles, wantFiles) {
		t.Errorf("dup.Files = %v, want %v", gotFiles, wantFiles)
	}
	// 重複検出時は採用 rule slice は nil (precheck で fatal にする契約)。
	if rules != nil {
		t.Errorf("expected nil rules on duplicate, got: %+v", rules)
	}
}

// TestLoadRules_DuplicateExcludesSkipped は Warning に落ちた rule (予約名等) は
// 重複検出の対象外であることを検証する
// (design.md §rules.Loader Postcondition: 「重複の前段階で Warning を出す rule は重複検出から除外」)。
func TestLoadRules_DuplicateExcludesSkipped(t *testing.T) {
	t.Parallel()

	cwd, dir := setupRulesDir(t)

	// 採用される rule は 1 件のみ ("good")。
	// "result" は予約名で skip されるが、`good.yaml` 側の name と一致しないので
	// 重複扱いにはならない。Warning + 1 件採用が期待結果。
	writeRuleFile(t, dir, "reserved.yaml", validRuleYAML("result"))
	writeRuleFile(t, dir, "good.yaml", validRuleYAML("good"))

	rules, warnings, err := LoadRules(cwd)
	if err != nil {
		t.Fatalf("LoadRules error: %v", err)
	}
	if len(rules) != 1 || rules[0].Name != "good" {
		t.Errorf("expected only good rule, got: %+v", rules)
	}
	if len(warnings) != 1 || warnings[0].File != "reserved.yaml" {
		t.Errorf("expected single warning for reserved.yaml, got: %+v", warnings)
	}
}

// TestRule_YAMLTagMatchesDesign は YAML tag が design schema (snake_case) と一致することを検証する。
//
// `applies_when` のような snake_case key を含む YAML が正しくパースできることを確認することで、
// tag 名 rename に対する後方互換ガードとする。
func TestRule_YAMLTagMatchesDesign(t *testing.T) {
	t.Parallel()

	cwd, dir := setupRulesDir(t)
	// `applies_when` は snake_case の tag。tag 名が `appliesWhen` 等になっていればパース不能。
	writeRuleFile(t, dir, "snake.yaml", validRuleYAML("snake-case"))

	rules, _, err := LoadRules(cwd)
	if err != nil {
		t.Fatalf("LoadRules error: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got: %+v", rules)
	}
	if rules[0].AppliesWhen == "" {
		t.Error("AppliesWhen should be parsed from snake_case YAML key")
	}
}
