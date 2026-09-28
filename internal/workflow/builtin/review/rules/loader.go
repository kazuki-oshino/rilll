package rules

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// rulesDirRel は rule YAML を発見する `<cwd>` 配下の相対パス (Requirements 1.1)。
const rulesDirRel = ".rilll-config/review-rules"

// LoadRules は `<cwd>/.rilll-config/review-rules/` 直下の `*.yaml` ファイル群を
// 列挙・パースし、採用 rule slice + skip Warning slice + sentinel error を返す
// (design.md §rules.Loader, Requirements 1.1, 1.2, 1.3, 1.4, 1.5, 3.2)。
//
// 振る舞い:
//
//   - サブディレクトリは対象外 (直下 `*.yaml` のみ)。
//   - ファイル名昇順で deterministic に列挙する (Requirements 1.5)。
//   - 必須 6 フィールド (`name` / `description` / `provider` / `model` /
//     `applies_when` / `prompt`) のいずれかが欠落 (空文字含む) した rule は当該ファイルのみ
//     skip + Warning に集約。
//   - `name` が kebab-case 規約 (`^[a-z0-9]+(-[a-z0-9]+)*$`) を満たさない rule は skip + Warning。
//   - `name` が `ReservedRuleNames` (`result` / `context`) と一致した rule は skip + Warning。
//   - YAML decode 失敗 (構文エラー / 型不一致 / 未知フィールド) は当該ファイルのみ skip + Warning。
//     未知フィールド検出には `Decoder.KnownFields(true)` を利用する。
//   - 採用候補内で同一 `Name` を持つ rule が複数存在した場合、`*DuplicateRuleNameError`
//     (`errors.Is(err, ErrDuplicateRuleName)` で判定可能) を返す。
//   - rules dir 自体が存在しない場合は `ErrRulesDirMissing` を返す。
//   - rules dir は存在するが採用 rule が 0 件 (全 skip 含む) の場合は `ErrNoRulesFound` を返す。
//
// 戻り値:
//
//   - `[]Rule` は採用 rule のスライス (ファイル名昇順)。`SourceFile` には basename が埋まる。
//   - `[]Warning` は skip された rule の File / Reason のスライス (ファイル名昇順)。
//   - `error` は sentinel (上記)。`error != nil` のときも `[]Warning` は意味を持つことがある
//     (例: `ErrNoRulesFound` のとき全 skip 理由が Warning に残る)。
//
// Preconditions: `cwd` は絶対 clean path であることを呼び出し側が保証する。
func LoadRules(cwd string) ([]Rule, []Warning, error) {
	dir := filepath.Join(cwd, rulesDirRel)

	entries, err := os.ReadDir(dir)
	if err != nil {
		// fs.ErrNotExist 系は専用 sentinel に丸める (Requirements 3.2)。
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, ErrRulesDirMissing
		}
		// 権限エラー等は raw error を返して呼び出し側に診断させる。
		return nil, nil, fmt.Errorf("read rules dir %q: %w", dir, err)
	}

	// `*.yaml` のみを抽出してファイル名昇順に並べる (Requirements 1.5)。
	// サブディレクトリは無視する (Requirements 1.1)。
	candidates := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// `.yml` は仕様外 (`*.yaml` 限定、Requirements 1.1)。
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		candidates = append(candidates, name)
	}
	sort.Strings(candidates)

	rules := make([]Rule, 0, len(candidates))
	warnings := make([]Warning, 0)

	for _, basename := range candidates {
		path := filepath.Join(dir, basename)
		rule, reason := loadOneRule(path)
		if reason != "" {
			warnings = append(warnings, Warning{File: basename, Reason: reason})
			continue
		}
		rule.SourceFile = basename
		rules = append(rules, rule)
	}

	// 採用 rule 内で同一 Name の重複を検出する (Requirements 1.4)。
	// パース失敗・予約名・必須欠落で skip された rule は重複判定の対象外
	// (design.md §rules.Loader Postcondition: 「重複の前段階で Warning を出す rule は重複検出から除外」)。
	if dup := findDuplicateName(rules); dup != nil {
		return nil, warnings, dup
	}

	if len(rules) == 0 {
		return nil, warnings, ErrNoRulesFound
	}

	return rules, warnings, nil
}

// loadOneRule は単一 YAML ファイルを Rule にパースし、
// skip すべき場合は人間可読な reason 文字列を返す。reason が空の場合は採用候補。
//
// `Decoder.KnownFields(true)` で未知フィールドを reject して Warning に丸める
// (design.md §rules.Loader Implementation Notes)。
func loadOneRule(path string) (Rule, string) {
	f, err := os.Open(path)
	if err != nil {
		// 個別ファイルの open 失敗は当該 rule のみ Warning にして他 rule を継続する。
		// (rules dir 全体の不在は呼び出し元で別経路 ErrRulesDirMissing として処理済み)
		return Rule{}, fmt.Sprintf("open failed: %v", err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)

	var rule Rule
	if err := dec.Decode(&rule); err != nil {
		// YAML 構文エラー / 型不一致 / 未知フィールドはまとめてここに来る。
		// 利用者が原因を特定しやすいよう raw error message を含める。
		return Rule{}, fmt.Sprintf("yaml decode failed: %v", err)
	}

	if reason := validateRequiredFields(rule); reason != "" {
		return Rule{}, reason
	}

	if !ruleNamePattern.MatchString(rule.Name) {
		return Rule{}, fmt.Sprintf("name %q が kebab-case 規約 (^[a-z0-9]+(-[a-z0-9]+)*$) に違反", rule.Name)
	}

	if _, reserved := ReservedRuleNames[rule.Name]; reserved {
		return Rule{}, fmt.Sprintf("rule name %q は予約済 (result.json / context.json と衝突するため)", rule.Name)
	}

	return rule, ""
}

// validateRequiredFields は必須 6 フィールド (Requirements 1.2) の欠落を検査し、
// 欠落していれば人間可読な reason 文字列を返す。空文字列フィールドも欠落として扱う。
func validateRequiredFields(r Rule) string {
	switch {
	case r.Name == "":
		return "必須フィールド `name` が欠落"
	case r.Description == "":
		return "必須フィールド `description` が欠落"
	case r.Provider == "":
		return "必須フィールド `provider` が欠落"
	case r.Model == "":
		return "必須フィールド `model` が欠落"
	case r.AppliesWhen == "":
		return "必須フィールド `applies_when` が欠落"
	case r.Prompt == "":
		return "必須フィールド `prompt` が欠落"
	}
	return ""
}

// findDuplicateName は採用 rule 内で同一 Name を 2 件以上持つものを検出し、
// 検出時は `*DuplicateRuleNameError` を返す (Requirements 1.4)。
//
// 衝突が複数の Name にまたがる場合は、Name 昇順で最初の衝突を返す
// (precheck メッセージの deterministic 性のため)。
func findDuplicateName(rules []Rule) error {
	groups := make(map[string][]string)
	for _, r := range rules {
		groups[r.Name] = append(groups[r.Name], r.SourceFile)
	}

	dupNames := make([]string, 0)
	for name, files := range groups {
		if len(files) > 1 {
			dupNames = append(dupNames, name)
		}
	}
	if len(dupNames) == 0 {
		return nil
	}

	sort.Strings(dupNames)
	first := dupNames[0]
	files := append([]string(nil), groups[first]...)
	sort.Strings(files)
	return &DuplicateRuleNameError{Name: first, Files: files}
}
