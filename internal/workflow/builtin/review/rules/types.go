// Package rules は review-workflow の YAML rule 定義 (`<cwd>/.rilll-config/review-rules/<rule>.yaml`)
// の SSoT 型と loader を提供する。
//
// 公開契約 (両端契約):
//
//   - 本パッケージで export される `Rule` / `Warning` / `ReservedRuleNames` /
//     `ErrNoRulesFound` / `ErrRulesDirMissing` / `ErrDuplicateRuleName` は、
//     review-workflow の precheck と executor、および将来の review-rules-skill
//     (rule YAML 自動生成) が同 module から直接参照する schema である
//     (design.md §rules.Rule, Requirements 1.1, 1.2, 1.3, 1.4, 1.5, 3.2)。
//   - YAML / JSON tag は固定 (`name` / `description` / `provider` / `model` /
//     `applies_when` / `prompt`)。tag の rename は破壊的変更扱いで、関連 spec の
//     同時改訂を要する。
//
// 後方互換ガード:
//
//   - 新規フィールド追加は許可。ただし `LoadRules` の `Decoder.KnownFields(true)` 契約上、
//     未知フィールドを含む YAML は当該 rule のみ Warning + skip となる点に注意。
//   - 既存フィールドの削除 / 意味変更は禁止。
package rules

import (
	"errors"
	"regexp"
)

// Rule は YAML rule 定義の正規型 (design.md §rules.Rule)。
//
// YAML / JSON tag は snake_case 固定。`SourceFile` は YAML には現れず、loader が
// パース成功時に basename を埋める (`yaml:"-"`)。
//
// フィールド意味 (Requirements 1.2):
//   - Name: rule 識別子。kebab-case (`^[a-z0-9]+(-[a-z0-9]+)*$`) のみ許容。
//     `result` / `context` は予約名として reject される。
//   - Description: 人間可読な rule 説明 (TUI 一覧 / artifact 表示等で利用)。
//   - Provider: agent provider ID (例: "claude" / "cursor" / "codex")。
//     loader では値検証しない (provider 未登録は executor が `Verdict=failed` に集約)。
//   - Model: provider のモデル名 (zero-value 許容、provider 既定モデル委任)。
//   - AppliesWhen: 適用判断のための自然文 (agent 自己判定の入力)。
//   - Prompt: レビュー実行時のプロンプト本文 (適用時のみ agent に渡る)。
//   - SourceFile: パース時に loader が basename を埋める (例: "security-scan.yaml")。
type Rule struct {
	// Name は rule 識別子 (kebab-case)。
	Name string `yaml:"name"         json:"name"`

	// Description は人間可読な rule 説明。
	Description string `yaml:"description"  json:"description"`

	// Provider は agent provider 識別子 (例: "claude" / "cursor" / "codex")。
	Provider string `yaml:"provider"     json:"provider"`

	// Model は provider のモデル名 (空文字許容、provider 既定モデル委任)。
	Model string `yaml:"model"        json:"model"`

	// AppliesWhen は適用判断のための自然文 (agent 自己判定の入力)。
	AppliesWhen string `yaml:"applies_when" json:"applies_when"`

	// Prompt はレビュー実行時のプロンプト本文。
	Prompt string `yaml:"prompt"       json:"prompt"`

	// SourceFile は本 rule を定義する YAML ファイルの basename。loader が埋める。
	SourceFile string `yaml:"-"            json:"source_file"`
}

// Warning は loader が当該 rule を skip した理由を表す non-fatal な記録 (Requirements 1.3)。
//
// 呼び出し側 (precheck.go) はこの slice を recorder.RecordRawEvent で warning 化する責務を持つ。
type Warning struct {
	// File は当該 YAML ファイルの basename (例: "broken.yaml")。
	File string

	// Reason は skip 理由の人間可読テキスト (例: "name は予約済 (result.json と衝突するため)")。
	Reason string
}

// ReservedRuleNames は per-rule artifact path (`<runDir>/<rule.Name>.json`) が
// 集約 artifact (`<runDir>/result.json`) や将来の `<runDir>/context.json`
// (PR snapshot, design.md Open Questions 4) と衝突する rule 名の予約集合。
//
// loader はこの集合に一致する `Name` を持つ rule を Warning + skip 対象とする
// (Requirements 1.3 / Reserved name 規約, design.md §rules.Rule)。
var ReservedRuleNames = map[string]struct{}{
	"result":  {},
	"context": {},
}

// ruleNamePattern は `Name` フィールドの kebab-case 検証正規表現 (design.md §rules.Rule Invariants)。
//
// 許容: 小文字英数字を `-` で連結 (例: "security-scan", "doc-style", "k8s-1-25")。
// 不許可: 大文字 / 空文字 / 先頭末尾の `-` / 連続する `-` / `_` などの記号。
var ruleNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// IsValidRuleName は `name` が kebab-case 規約 + 予約名チェックを通過するかを返す
// (Requirements 1.3 + Reserved name 規約)。
//
// loader が rule を採用するか Warning に回すかの判定で利用される。
// 単独 export はテスト用途と将来の review-rules-skill (生成側) からの再利用を想定している。
func IsValidRuleName(name string) bool {
	if !ruleNamePattern.MatchString(name) {
		return false
	}
	if _, reserved := ReservedRuleNames[name]; reserved {
		return false
	}
	return true
}

// Sentinel errors (design.md §rules.Rule Service Interface):
//
//   - ErrRulesDirMissing: `<cwd>/.rilll-config/review-rules/` ディレクトリが存在しない。
//     precheck で `verdict=precheck_failed` に集約される (Requirements 3.2)。
//   - ErrNoRulesFound: ディレクトリは存在するが `*.yaml` 候補が 0 件、または
//     全候補が Warning に落ちて採用 0 件になった。precheck で同 verdict に集約。
//   - ErrDuplicateRuleName: 採用候補内で同一 `Name` が複数の YAML に存在する。
//     precheck で `verdict=precheck_failed` に集約される (Requirements 1.4)。
var (
	// ErrRulesDirMissing は `.rilll-config/review-rules` ディレクトリ不在を表す sentinel。
	ErrRulesDirMissing = errors.New("review: .rilll-config/review-rules ディレクトリが存在しない")

	// ErrNoRulesFound は採用 rule が 0 件であることを表す sentinel。
	ErrNoRulesFound = errors.New("review: rules dir に rule が無い")

	// ErrDuplicateRuleName は同一 `Name` の重複を表す sentinel。
	// エラーメッセージには `errors.As` で取り出し可能な `*DuplicateRuleNameError` をラップする。
	ErrDuplicateRuleName = errors.New("review: rule name が重複")
)

// DuplicateRuleNameError は ErrDuplicateRuleName の詳細情報 (重複 name と該当ファイル名一覧) を保持する。
//
// precheck 側が利用者向けメッセージを組み立てる際に、`errors.As` で取り出して衝突した
// rule 名と該当 YAML ファイル名を提示できる (Requirements 1.4)。
type DuplicateRuleNameError struct {
	// Name は衝突した rule の `Name`。
	Name string

	// Files は同名 rule を含む YAML ファイルの basename 一覧 (ファイル名昇順)。
	Files []string
}

// Error は ErrDuplicateRuleName をラップした人間可読メッセージを返す。
func (e *DuplicateRuleNameError) Error() string {
	return ErrDuplicateRuleName.Error() + ": " + e.Name
}

// Unwrap は errors.Is(err, ErrDuplicateRuleName) を成立させる。
func (e *DuplicateRuleNameError) Unwrap() error {
	return ErrDuplicateRuleName
}
