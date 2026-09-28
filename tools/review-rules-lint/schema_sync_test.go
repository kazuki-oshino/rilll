// schema_sync_test.go は references/schema.md (skill 側 / 下流 / SSoT 参照のみ) と
// 上流 SSoT (`internal/workflow/builtin/review/rules/types.go` §`Rule` / §`ReservedRuleNames`
// + `internal/provider/types.go` §`ProviderID` 列挙) の間で 4 種の schema 規約値が
// drift していないことを文字列抽出ベースで検証する。
//
// 上流 SSoT は元々 `.kiro/specs/review-workflow/design.md` だったが、当該 spec ディレクトリは
// 実装が main に取り込まれた後 (commit 1e58d08) に削除された。設計ドキュメント (markdown)
// ではなく runtime 実体である Go コードそのものが現行の SSoT であり、本テストはその実コードを
// 上流として参照する。
//
// 検証対象 (drift gate):
//
//	(s1) 6 必須フィールド名集合: { name, description, provider, model, applies_when, prompt }
//	(s2) kebab-case 正規表現: ^[a-z0-9]+(-[a-z0-9]+)*$
//	(s3) 予約語集合: { result, context }
//	(s4) provider 有効値集合: { claude, cursor, codex, cursor-sdk }
//
// 本テストは references/schema.md ↔ 上流 Go コード (rules/types.go + provider/types.go) の
// 双方を独立に文字列抽出し、抽出結果が 4 種すべてで一致することを集合比較で検証する。
// drift 検出時は「references/schema.md 側 / 上流側でどう違うか」を t.Errorf で明示する。
//
// 配布物には含めない (Req 1.2 / Req 7.1)。本ファイルは tools/review-rules-lint/ にのみ存在し、
// .agents/skills/review-rules-skill/ には一切同梱しない (Layer Separation 不変式)。
//
// 参照: design.md § Verification Contract / § Layer Separation 不変式 / § Revalidation
// Triggers / Req 6.1 / Req 6.2 / Req 1.2 / Req 7.1。
package reviewruleslint

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// upstreamRulesTypesPath は上流 SSoT (`Rule` struct / `ReservedRuleNames` map /
// kebab-case 正規表現を保持する Go コード) への、tools/review-rules-lint/ 作業
// ディレクトリからの相対パス。
const upstreamRulesTypesPath = "../../internal/workflow/builtin/review/rules/types.go"

// upstreamProviderTypesPath は ProviderID 列挙 (claude / cursor / codex / cursor-sdk)
// の SSoT を保持する Go コードへの、tools/review-rules-lint/ 作業ディレクトリからの相対パス。
const upstreamProviderTypesPath = "../../internal/provider/types.go"

// referenceSchemaPath は本 skill の references/schema.md (下流 / SSoT 参照側) への、
// tools/review-rules-lint/ 作業ディレクトリからの相対パス。
const referenceSchemaPath = "../../.agents/skills/review-rules-skill/references/schema.md"

// schemaValues は schema 同期検証で比較する 4 種の値を保持する。
// 集合比較を deterministic にするため、フィールド名と provider はソート済みの
// []string、予約語も sort 済みの []string で表現する。
type schemaValues struct {
	requiredFields []string // (s1) 6 必須フィールド名 (sorted)
	kebabCaseRe    string   // (s2) kebab-case 正規表現 (raw 文字列)
	reservedNames  []string // (s3) 予約語集合 (sorted)
	validProviders []string // (s4) provider 有効値集合 (sorted)
}

// extractReservedKeysBlock は上流 design.md (または同形式の fixture) から
// `var ReservedRuleNames = map[string]struct{}{ ... }` 内の予約語キー集合を
// 行ベースで抽出する。map literal 内に `{}` (struct{} の zero value) が含まれる
// ため、単純な regex `\{(.*?)\}` では最初の inner `{}` で打ち切られてしまう。
// 本ヘルパは「`var ReservedRuleNames` 行を見つけ、続く行を順に走査し、行頭が
// 単独の `}` (インデントのない閉じ波括弧) になった時点で終了」する。
//
// 各キーは `"<key>": {},` 形式から抽出する。先頭 `var ReservedRuleNames` 行が
// 見つからない場合は (nil, false) を返す。
func extractReservedKeysBlock(designMD string) ([]string, bool) {
	lines := strings.Split(designMD, "\n")
	var inBlock bool
	keyRe := regexp.MustCompile(`"([^"]+)"\s*:\s*\{\}`)
	var keys []string
	for _, ln := range lines {
		if !inBlock {
			if strings.Contains(ln, "var ReservedRuleNames") &&
				strings.Contains(ln, "map[string]struct{}{") {
				inBlock = true
			}
			continue
		}
		// inBlock = true: map literal の終端 (= 行頭の `}` のみ) まで収集
		trimmed := strings.TrimSpace(ln)
		if trimmed == "}" {
			return keys, true
		}
		if m := keyRe.FindStringSubmatch(ln); len(m) >= 2 {
			keys = append(keys, m[1])
		}
	}
	if !inBlock {
		return nil, false
	}
	// inBlock のまま EOF: 終端 `}` が無いため抽出失敗扱いにする
	return nil, false
}

// extractUpstreamSchema は上流 SSoT の Go コードから 4 種の schema 規約値を文字列抽出する。
// 本関数は drift gate の上流側抽出器であり、入力は 2 ファイル分のテキスト:
//
//   - rulesGo: `internal/workflow/builtin/review/rules/types.go` のテキスト。
//     §`Rule` struct (yaml tag を含む) / §`ReservedRuleNames` map literal /
//     kebab-case 正規表現 (raw string literal および docstring 内 backtick 引用) を保持する。
//   - providerGo: `internal/provider/types.go` のテキスト。
//     §`ProviderID` 列挙 (`Provider<Name> ProviderID = "<id>"`) を保持する。
//
// いずれかの抽出に失敗した場合は (zero schemaValues, false) を返す。
func extractUpstreamSchema(rulesGo, providerGo string) (schemaValues, bool) {
	var v schemaValues

	// (s1) 6 必須フィールド名: `type Rule struct { ... }` ブロック内の yaml tag から抽出
	// 例: `Name        string `yaml:"name"        json:"name"`` から "name" を取り出す。
	// SourceFile (`yaml:"-"`) は除外する (loader 内部用フィールドであり schema 必須項目ではない)。
	ruleStructRe := regexp.MustCompile(`(?s)type\s+Rule\s+struct\s*\{(.*?)\}`)
	mStruct := ruleStructRe.FindStringSubmatch(rulesGo)
	if len(mStruct) < 2 {
		return schemaValues{}, false
	}
	yamlTagRe := regexp.MustCompile("yaml:\"([^\"]+)\"")
	tags := yamlTagRe.FindAllStringSubmatch(mStruct[1], -1)
	if len(tags) == 0 {
		return schemaValues{}, false
	}
	seen := make(map[string]struct{})
	for _, tg := range tags {
		tag := tg[1]
		if tag == "-" {
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		v.requiredFields = append(v.requiredFields, tag)
	}
	sort.Strings(v.requiredFields)

	// (s2) kebab-case 正規表現: 「kebab-case 正規表現 `^[a-z0-9]+(-[a-z0-9]+)*$`」のような
	// インライン言及から抽出する。types.go では raw string literal (`regexp.MustCompile(\`^[a-z...$\`)`)
	// と docstring 内 backtick 引用の双方に出現するが、すべて同一値であることを確認する。
	// drift 検出時 (`^[a-z_]+$` 等) も同経路で抽出できるよう、先頭は `^[a-z` 固定、
	// 内部は `[^\`]*` で柔軟に許容する。
	kebabRe := regexp.MustCompile("`(\\^\\[a-z[^`]*)`")
	kebabMatches := kebabRe.FindAllStringSubmatch(rulesGo, -1)
	if len(kebabMatches) == 0 {
		return schemaValues{}, false
	}
	first := kebabMatches[0][1]
	for _, m := range kebabMatches {
		if m[1] != first {
			// 上流内で kebab 正規表現が複数値存在する = 内部不整合
			return schemaValues{}, false
		}
	}
	v.kebabCaseRe = first

	// (s3) 予約語集合: `var ReservedRuleNames = map[string]struct{}{ ... }` から抽出。
	// map literal 内には `{}` (struct{}{} の zero value) が含まれるため、非貪欲な
	// `(?s).*?\}` で囲もうとすると最初の `{}` で停止して内部を空に拾ってしまう。
	// 行ベースに切り替え、`var ReservedRuleNames` 行から始めて、map literal の
	// 終了 (= 行頭から始まる単独の `}`) までをスキャンする。
	reservedKeys, ok := extractReservedKeysBlock(rulesGo)
	if !ok || len(reservedKeys) == 0 {
		return schemaValues{}, false
	}
	seenR := make(map[string]struct{})
	for _, key := range reservedKeys {
		if _, ok := seenR[key]; ok {
			continue
		}
		seenR[key] = struct{}{}
		v.reservedNames = append(v.reservedNames, key)
	}
	sort.Strings(v.reservedNames)

	// (s4) provider 有効値: provider/types.go の ProviderID 列挙
	//   `Provider<Name>    ProviderID = "<id>"`
	// から抽出する。文字列値部分 (`"<id>"`) が rule YAML schema の `provider` 有効値リストの SSoT。
	// loader 自体は `Provider` 文字列値を検証しないが、agent.CallAgent 経由の dispatch 先が
	// この列挙によって決定するため、本列挙が「review rule で書ける有効 provider 集合」と等価。
	providerConstRe := regexp.MustCompile(`Provider\w+\s+ProviderID\s*=\s*"([^"]+)"`)
	pHits := providerConstRe.FindAllStringSubmatch(providerGo, -1)
	if len(pHits) == 0 {
		return schemaValues{}, false
	}
	seenP := make(map[string]struct{})
	for _, h := range pHits {
		val := h[1]
		if _, ok := seenP[val]; ok {
			continue
		}
		seenP[val] = struct{}{}
		v.validProviders = append(v.validProviders, val)
	}
	sort.Strings(v.validProviders)

	return v, true
}

// extractReferenceSchema は references/schema.md (skill 側 / 下流) のテキストから
// 4 種の schema 規約値を文字列抽出する。本関数は drift gate の下流側抽出器。
//
// 抽出契約:
//   - (s1) 6 必須フィールド名は markdown 表「YAML Schema (6 必須フィールド)」の各行
//     先頭セル「| <name> |」から抽出する。
//   - (s2) kebab-case 正規表現は backtick 引用された ^[a-z0-9]... 文字列から抽出。
//   - (s3) 予約語集合は表の name 行 / または「予約語 `result` / `context`」言及から抽出。
//   - (s4) provider 有効値は表の provider 行 / または「`claude` / `cursor` / `codex` / `cursor-sdk`」
//     列挙から抽出。
func extractReferenceSchema(refMD string) (schemaValues, bool) {
	var v schemaValues

	// (s1) 6 必須フィールド名: 「YAML Schema (6 必須フィールド)」セクション以降の表行から抽出。
	// 表の行は `| `<field>` | <type> | ... |` 形式。最初のセルが backtick 囲みの識別子。
	// セクション header は本テスト fixture と同じ "## YAML Schema" を allow する。
	headerIdx := strings.Index(refMD, "## YAML Schema")
	if headerIdx < 0 {
		return schemaValues{}, false
	}
	// 次のセクション (## で始まる) までを切り出す
	rest := refMD[headerIdx+len("## YAML Schema"):]
	if next := strings.Index(rest, "\n## "); next >= 0 {
		rest = rest[:next]
	}
	tableRowRe := regexp.MustCompile("(?m)^\\|\\s*`([a-z_]+)`\\s*\\|")
	rows := tableRowRe.FindAllStringSubmatch(rest, -1)
	if len(rows) == 0 {
		return schemaValues{}, false
	}
	seen := make(map[string]struct{})
	for _, r := range rows {
		f := r[1]
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		v.requiredFields = append(v.requiredFields, f)
	}
	sort.Strings(v.requiredFields)

	// (s2) kebab-case 正規表現: backtick で囲まれた `^[a-z0-9]+...$` を抽出。
	// kebab-case 正規表現 (rule name 用) は backtick 引用で、`^[a-z` で始まる
	// 正規表現リテラルとして表記される。drift 検出時 (`^[a-z_]+$` 等) も同経路で
	// 抽出できるよう、先頭は `^[a-z` 固定、内部は `[^\`]*` で柔軟に許容する。
	kebabRe := regexp.MustCompile("`(\\^\\[a-z[^`]*)`")
	kebabMatches := kebabRe.FindAllStringSubmatch(refMD, -1)
	if len(kebabMatches) == 0 {
		return schemaValues{}, false
	}
	first := kebabMatches[0][1]
	for _, m := range kebabMatches {
		if m[1] != first {
			return schemaValues{}, false
		}
	}
	v.kebabCaseRe = first

	// (s3) 予約語集合: 「予約語 `result` / `context`」のような言及から抽出。
	// references/schema.md L17 では markdown table 行内に
	//   `| `name` | ... | 予約語 `result` / `context` は禁止 |`
	// のように `name` も同じ行に backtick 引用で出現するため、行全体を走査すると
	// rule field 名 (`name` 等) を予約語として誤抽出する。
	// 「予約語」キーワード以降の部分文字列に対してのみ backtick 識別子を拾う。
	for line := range strings.SplitSeq(refMD, "\n") {
		idx := strings.Index(line, "予約語")
		if idx < 0 {
			continue
		}
		tail := line[idx:]
		btRe := regexp.MustCompile("`([a-z_][a-z0-9_-]*)`")
		hits := btRe.FindAllStringSubmatch(tail, -1)
		if len(hits) < 2 {
			continue
		}
		seenR := make(map[string]struct{})
		for _, h := range hits {
			key := h[1]
			if _, ok := seenR[key]; ok {
				continue
			}
			seenR[key] = struct{}{}
			v.reservedNames = append(v.reservedNames, key)
		}
		break
	}
	if len(v.reservedNames) == 0 {
		return schemaValues{}, false
	}
	sort.Strings(v.reservedNames)

	// (s4) provider 有効値: 表の provider 行 (`| `provider` | ... | <enum> のいずれか |`)
	// 内に backtick 引用の有効値リストが書かれる。「のいずれか」の語を含む行から
	// 「provider」keyword 以降の backtick 識別子を抽出する (drift して openai 等が
	// 含まれた場合も同経路で抽出できるよう、有効値リストを hardcode 比較しない)。
	providerLineFound := false
	for line := range strings.SplitSeq(refMD, "\n") {
		if !strings.Contains(line, "のいずれか") {
			continue
		}
		idx := strings.Index(line, "provider")
		if idx < 0 {
			continue
		}
		tail := line[idx:]
		// 行内 backtick 識別子から、行頭付近の `provider` フィールド名自体は除外。
		// kebab-case 風の英小文字 + 数字 + hyphen + underscore の識別子を拾う。
		btRe := regexp.MustCompile("`([a-z][a-z0-9_-]*)`")
		hits := btRe.FindAllStringSubmatch(tail, -1)
		if len(hits) < 2 {
			// provider field 行ではあるが backtick リストが薄い → 別行を探す
			continue
		}
		seenP := make(map[string]struct{})
		for _, h := range hits {
			val := h[1]
			if val == "provider" {
				// table 先頭の field 名そのものは除外
				continue
			}
			if _, ok := seenP[val]; ok {
				continue
			}
			seenP[val] = struct{}{}
			v.validProviders = append(v.validProviders, val)
		}
		if len(v.validProviders) > 0 {
			providerLineFound = true
			break
		}
	}
	if !providerLineFound {
		return schemaValues{}, false
	}
	sort.Strings(v.validProviders)

	return v, true
}

// schemaValuesEqual は 2 つの schemaValues が完全一致するかを deep-equal で判定する。
// 一致しない場合は drift の検出になり、本テストが FAIL する。
func schemaValuesEqual(a, b schemaValues) bool {
	if a.kebabCaseRe != b.kebabCaseRe {
		return false
	}
	if !reflect.DeepEqual(a.requiredFields, b.requiredFields) {
		return false
	}
	if !reflect.DeepEqual(a.reservedNames, b.reservedNames) {
		return false
	}
	if !reflect.DeepEqual(a.validProviders, b.validProviders) {
		return false
	}
	return true
}

// formatSchemaDiff は references/schema.md 側 (ref) / 上流 design.md 側 (up) の
// schemaValues 差分を人間可読形式で書式化する。drift 検出時のメッセージ用。
func formatSchemaDiff(ref, up schemaValues) string {
	var b strings.Builder
	if !reflect.DeepEqual(ref.requiredFields, up.requiredFields) {
		b.WriteString("  (s1) requiredFields:\n")
		b.WriteString("    references/schema.md: " + strings.Join(ref.requiredFields, ",") + "\n")
		b.WriteString("    upstream design.md  : " + strings.Join(up.requiredFields, ",") + "\n")
	}
	if ref.kebabCaseRe != up.kebabCaseRe {
		b.WriteString("  (s2) kebabCaseRe:\n")
		b.WriteString("    references/schema.md: " + ref.kebabCaseRe + "\n")
		b.WriteString("    upstream design.md  : " + up.kebabCaseRe + "\n")
	}
	if !reflect.DeepEqual(ref.reservedNames, up.reservedNames) {
		b.WriteString("  (s3) reservedNames:\n")
		b.WriteString("    references/schema.md: " + strings.Join(ref.reservedNames, ",") + "\n")
		b.WriteString("    upstream design.md  : " + strings.Join(up.reservedNames, ",") + "\n")
	}
	if !reflect.DeepEqual(ref.validProviders, up.validProviders) {
		b.WriteString("  (s4) validProviders:\n")
		b.WriteString("    references/schema.md: " + strings.Join(ref.validProviders, ",") + "\n")
		b.WriteString("    upstream design.md  : " + strings.Join(up.validProviders, ",") + "\n")
	}
	return b.String()
}

// TestSchemaSync_UpstreamReferenceMatch は実ファイル (references/schema.md と
// 上流 Go コード `internal/workflow/builtin/review/rules/types.go` +
// `internal/provider/types.go`) を読み込み、4 種の schema 規約値
// (6 必須フィールド / kebab-case 正規表現 / 予約語 / provider 有効値) が一致
// していることを検証する。drift 検出時は references/schema.md 側 / 上流側で
// どの値がどう違うかを明示する。
//
// 本テストは Verification Contract の表 §「references/schema.md に記載された 6
// 必須フィールド名・kebab-case 正規表現・予約語集合が、上流 SSoT (rules/types.go
// §`Rule` / §`ReservedRuleNames` + provider/types.go §`ProviderID`) の現行値と
// 一致すること」を満たす drift gate である (Req 6.1 / Req 6.2)。
func TestSchemaSync_UpstreamReferenceMatch(t *testing.T) {
	rulesRaw, err := os.ReadFile(upstreamRulesTypesPath)
	if err != nil {
		t.Fatalf("上流 SSoT (rules) 読み込み失敗: %s: %v", upstreamRulesTypesPath, err)
	}
	providerRaw, err := os.ReadFile(upstreamProviderTypesPath)
	if err != nil {
		t.Fatalf("上流 SSoT (provider) 読み込み失敗: %s: %v", upstreamProviderTypesPath, err)
	}
	upstream, ok := extractUpstreamSchema(string(rulesRaw), string(providerRaw))
	if !ok {
		t.Fatalf("上流 SSoT (%s + %s) から schema 値を抽出できなかった (drift gate 機能不全): "+
			"§`Rule` struct / §`ReservedRuleNames` map / kebab-case 正規表現 / "+
			"§`ProviderID` 列挙のいずれかが想定形式から外れている可能性がある",
			upstreamRulesTypesPath, upstreamProviderTypesPath)
	}

	refRaw, err := os.ReadFile(referenceSchemaPath)
	if err != nil {
		t.Fatalf("references/schema.md 読み込み失敗: %s: %v", referenceSchemaPath, err)
	}
	reference, ok := extractReferenceSchema(string(refRaw))
	if !ok {
		t.Fatalf("references/schema.md (%s) から schema 値を抽出できなかった (drift gate 機能不全): "+
			"## YAML Schema 表 / kebab-case backtick / 予約語行 / provider 行のいずれかが "+
			"想定形式から外れている可能性がある", referenceSchemaPath)
	}

	if !schemaValuesEqual(reference, upstream) {
		t.Errorf("references/schema.md と上流 SSoT (rules/types.go + provider/types.go) の間で "+
			"schema 規約値の drift を検出しました (上流改訂を本 skill 側に追従していない可能性):\n%s",
			formatSchemaDiff(reference, upstream))
	}

	// 上流側の値が本パッケージ内 (lint_test.go) の lint 用定数と一致することを確認する。
	// これにより lint パスと drift gate パスの両方が同一 SSoT を参照していることを保証する。
	//
	// v2 メモ: 本パッケージ内 `requiredFields` は v2 で「種テンプレ (templates/*.yaml) の必須 4 項目」
	// に縮退したため、上流 Rule struct (loader output の 6 必須フィールド) とは
	// 集合が一致しない。`requiredFields` の cross-check は v2 で意味を失うため除外し、
	// references/schema.md (下流) ↔ 上流 Go コード (loader 6 必須) の sync 検査は本テスト前段
	// (上記 schemaValuesEqual) で継続する。
	// 一方 reservedNames / validProviders / kebabCaseRe は v2 でも上流と完全一致する SSoT を
	// 参照する (v1 から不変) ため、cross-check は維持する。
	pkgReserved := make([]string, 0, len(reservedNames))
	for k := range reservedNames {
		pkgReserved = append(pkgReserved, k)
	}
	sort.Strings(pkgReserved)
	if !reflect.DeepEqual(pkgReserved, upstream.reservedNames) {
		t.Errorf("lint パッケージ内 reservedNames と上流 SSoT が不一致 (lint 側の定数追従漏れ):\n"+
			"  package reservedNames     : %v\n  upstream rules/types.go  : %v",
			pkgReserved, upstream.reservedNames)
	}
	pkgProviders := make([]string, 0, len(validProviders))
	for k := range validProviders {
		pkgProviders = append(pkgProviders, k)
	}
	sort.Strings(pkgProviders)
	if !reflect.DeepEqual(pkgProviders, upstream.validProviders) {
		t.Errorf("lint パッケージ内 validProviders と上流 SSoT が不一致 (lint 側の定数追従漏れ):\n"+
			"  package validProviders     : %v\n  upstream provider/types.go: %v",
			pkgProviders, upstream.validProviders)
	}
	if kebabCaseRe.String() != upstream.kebabCaseRe {
		t.Errorf("lint パッケージ内 kebabCaseRe と上流 SSoT が不一致 (lint 側の定数追従漏れ):\n"+
			"  package kebabCaseRe       : %s\n  upstream rules/types.go  : %s",
			kebabCaseRe.String(), upstream.kebabCaseRe)
	}
}

// TestExtractUpstreamSchema_DetectsDrift は extractUpstreamSchema が合成 fixture
// (上流 SSoT を擬似的に変更した synthetic 文字列) に対して、drift を実値レベルで
// 検出することを単体テストする。実ファイル test (TestSchemaSync_*) が PASS している
// だけでは「抽出器が scaffolding (常に同じ値を返すスタブ) でないこと」を保証できない
// ため、本テストで抽出ロジックの実体性を担保する。
//
// fixture は 2 つに分割する:
//   - rulesFixture:    rules/types.go と同形式 (Rule struct / ReservedRuleNames / kebab regex)
//   - providerFixture: provider/types.go と同形式 (ProviderID const 列挙)
func TestExtractUpstreamSchema_DetectsDrift(t *testing.T) {
	// 上流 SSoT (rules/types.go) の正常 fixture
	rulesFixture := `package rules

import "regexp"

// Rule の docstring 内で kebab-case 正規表現 ` + "`^[a-z0-9]+(-[a-z0-9]+)*$`" + ` に言及する。
type Rule struct {
    Name        string ` + "`yaml:\"name\"        json:\"name\"`" + `
    Description string ` + "`yaml:\"description\" json:\"description\"`" + `
    Provider    string ` + "`yaml:\"provider\"    json:\"provider\"`" + `
    Model       string ` + "`yaml:\"model\"       json:\"model\"`" + `
    AppliesWhen string ` + "`yaml:\"applies_when\" json:\"applies_when\"`" + `
    Prompt      string ` + "`yaml:\"prompt\"      json:\"prompt\"`" + `
    SourceFile  string ` + "`yaml:\"-\"           json:\"source_file\"`" + `
}

var ReservedRuleNames = map[string]struct{}{
    "result":  {},
    "context": {},
}

var ruleNamePattern = regexp.MustCompile(` + "`^[a-z0-9]+(-[a-z0-9]+)*$`" + `)
`

	// 上流 SSoT (provider/types.go) の正常 fixture
	providerFixture := `package provider

type ProviderID string

const (
    ProviderClaude    ProviderID = "claude"
    ProviderCursor    ProviderID = "cursor"
    ProviderCodex     ProviderID = "codex"
    ProviderCursorSDK ProviderID = "cursor-sdk"
)
`

	t.Run("正常 fixture から 4 種すべて抽出できる", func(t *testing.T) {
		v, ok := extractUpstreamSchema(rulesFixture, providerFixture)
		if !ok {
			t.Fatalf("baseFixture からの抽出が失敗した (false 返却)")
		}
		wantFields := []string{"applies_when", "description", "model", "name", "prompt", "provider"}
		if !reflect.DeepEqual(v.requiredFields, wantFields) {
			t.Errorf("requiredFields: got=%v, want=%v", v.requiredFields, wantFields)
		}
		if v.kebabCaseRe != "^[a-z0-9]+(-[a-z0-9]+)*$" {
			t.Errorf("kebabCaseRe: got=%q, want=%q", v.kebabCaseRe, "^[a-z0-9]+(-[a-z0-9]+)*$")
		}
		wantReserved := []string{"context", "result"}
		if !reflect.DeepEqual(v.reservedNames, wantReserved) {
			t.Errorf("reservedNames: got=%v, want=%v", v.reservedNames, wantReserved)
		}
		wantProviders := []string{"claude", "codex", "cursor", "cursor-sdk"}
		if !reflect.DeepEqual(v.validProviders, wantProviders) {
			t.Errorf("validProviders: got=%v, want=%v", v.validProviders, wantProviders)
		}
	})

	t.Run("予約語が変更された fixture では drift を検出 (resultが消えてsessionが追加)", func(t *testing.T) {
		drifted := strings.Replace(rulesFixture,
			`"result":  {},`,
			`"session":  {},`, 1)
		v, ok := extractUpstreamSchema(drifted, providerFixture)
		if !ok {
			t.Fatalf("drifted fixture の抽出が失敗した")
		}
		// session / context が抽出され、result は含まれない
		got := strings.Join(v.reservedNames, ",")
		if !strings.Contains(got, "session") {
			t.Errorf("drift 後の reservedNames に session が含まれない: %s", got)
		}
		if strings.Contains(got, "result") {
			t.Errorf("drift 後の reservedNames から result が消えていない: %s", got)
		}
	})

	t.Run("provider 有効値が変更された fixture では drift を検出", func(t *testing.T) {
		drifted := strings.Replace(providerFixture,
			`ProviderCursorSDK ProviderID = "cursor-sdk"`,
			`ProviderOpenAI    ProviderID = "openai"`, 1)
		v, ok := extractUpstreamSchema(rulesFixture, drifted)
		if !ok {
			t.Fatalf("drifted fixture の抽出が失敗した")
		}
		got := strings.Join(v.validProviders, ",")
		if !strings.Contains(got, "openai") {
			t.Errorf("drift 後の validProviders に openai が含まれない: %s", got)
		}
		if strings.Contains(got, "cursor-sdk") {
			t.Errorf("drift 後の validProviders から cursor-sdk が消えていない: %s", got)
		}
	})

	t.Run("kebab-case 正規表現が変更された fixture では drift を検出", func(t *testing.T) {
		drifted := strings.ReplaceAll(rulesFixture,
			"`^[a-z0-9]+(-[a-z0-9]+)*$`",
			"`^[a-z0-9_]+$`")
		v, ok := extractUpstreamSchema(drifted, providerFixture)
		if !ok {
			t.Fatalf("drifted fixture の抽出が失敗した")
		}
		if v.kebabCaseRe != "^[a-z0-9_]+$" {
			t.Errorf("drift 後の kebabCaseRe が想定値と不一致: got=%q, want=%q",
				v.kebabCaseRe, "^[a-z0-9_]+$")
		}
	})

	t.Run("Rule struct 不在の fixture では抽出失敗 (drift gate FATAL 経路)", func(t *testing.T) {
		broken := strings.Replace(rulesFixture, "type Rule struct", "type RuleX struct", 1)
		_, ok := extractUpstreamSchema(broken, providerFixture)
		if ok {
			t.Errorf("Rule struct 不在でも抽出が成功してしまった (drift gate FATAL 経路が機能していない)")
		}
	})

	t.Run("ReservedRuleNames 不在の fixture では抽出失敗", func(t *testing.T) {
		broken := strings.Replace(rulesFixture, "var ReservedRuleNames", "var SomethingElse", 1)
		_, ok := extractUpstreamSchema(broken, providerFixture)
		if ok {
			t.Errorf("ReservedRuleNames 不在でも抽出が成功してしまった")
		}
	})

	t.Run("ProviderID 列挙不在の fixture では抽出失敗", func(t *testing.T) {
		broken := strings.ReplaceAll(providerFixture, "ProviderID = ", "OtherKind = ")
		_, ok := extractUpstreamSchema(rulesFixture, broken)
		if ok {
			t.Errorf("ProviderID 列挙不在でも抽出が成功してしまった")
		}
	})
}

// TestExtractReferenceSchema_DetectsDrift は extractReferenceSchema が合成 fixture に
// 対して drift を実値レベルで検出することを単体テストする。
func TestExtractReferenceSchema_DetectsDrift(t *testing.T) {
	baseFixture := `# review-workflow YAML Schema Reference

## YAML Schema (6 必須フィールド)

| フィールド | 型 | 必須 | 説明 | 制約 |
|---|---|---|---|---|
| ` + "`name`" + ` | string | ✓ | rule 識別子 | kebab-case ` + "`^[a-z0-9]+(-[a-z0-9]+)*$`" + ` / 予約語 ` + "`result`" + ` / ` + "`context`" + ` 禁止 |
| ` + "`description`" + ` | string | ✓ | rule の 1 行要約 | — |
| ` + "`provider`" + ` | string | ✓ | review 実行 provider | ` + "`claude`" + ` / ` + "`cursor`" + ` / ` + "`codex`" + ` / ` + "`cursor-sdk`" + ` のいずれか |
| ` + "`model`" + ` | string | ✓ | model 識別子 | — |
| ` + "`applies_when`" + ` | string | ✓ | 適用条件 | string |
| ` + "`prompt`" + ` | string | ✓ | レビュー指示文 | string |

## 検証規則
`

	t.Run("正常 fixture から 4 種すべて抽出できる", func(t *testing.T) {
		v, ok := extractReferenceSchema(baseFixture)
		if !ok {
			t.Fatalf("baseFixture からの抽出が失敗した")
		}
		wantFields := []string{"applies_when", "description", "model", "name", "prompt", "provider"}
		if !reflect.DeepEqual(v.requiredFields, wantFields) {
			t.Errorf("requiredFields: got=%v, want=%v", v.requiredFields, wantFields)
		}
		if v.kebabCaseRe != "^[a-z0-9]+(-[a-z0-9]+)*$" {
			t.Errorf("kebabCaseRe: got=%q, want=%q", v.kebabCaseRe, "^[a-z0-9]+(-[a-z0-9]+)*$")
		}
		wantReserved := []string{"context", "result"}
		if !reflect.DeepEqual(v.reservedNames, wantReserved) {
			t.Errorf("reservedNames: got=%v, want=%v", v.reservedNames, wantReserved)
		}
		wantProviders := []string{"claude", "codex", "cursor", "cursor-sdk"}
		if !reflect.DeepEqual(v.validProviders, wantProviders) {
			t.Errorf("validProviders: got=%v, want=%v", v.validProviders, wantProviders)
		}
	})

	t.Run("provider 有効値が変更された fixture では drift を検出", func(t *testing.T) {
		drifted := strings.Replace(baseFixture, "`cursor-sdk`", "`openai`", 1)
		v, ok := extractReferenceSchema(drifted)
		if !ok {
			t.Fatalf("drifted fixture の抽出が失敗した")
		}
		got := strings.Join(v.validProviders, ",")
		if strings.Contains(got, "cursor-sdk") {
			t.Errorf("drift 後の validProviders から cursor-sdk が消えていない: %s", got)
		}
	})

	t.Run("予約語が変更された fixture では drift を検出", func(t *testing.T) {
		drifted := strings.Replace(baseFixture, "`result`", "`session`", 1)
		v, ok := extractReferenceSchema(drifted)
		if !ok {
			t.Fatalf("drifted fixture の抽出が失敗した")
		}
		got := strings.Join(v.reservedNames, ",")
		if strings.Contains(got, "result") {
			t.Errorf("drift 後の reservedNames から result が消えていない: %s", got)
		}
		if !strings.Contains(got, "session") {
			t.Errorf("drift 後の reservedNames に session が含まれない: %s", got)
		}
	})

	t.Run("YAML Schema セクション不在では抽出失敗", func(t *testing.T) {
		broken := strings.Replace(baseFixture, "## YAML Schema", "## Other Section", 1)
		_, ok := extractReferenceSchema(broken)
		if ok {
			t.Errorf("YAML Schema セクション不在でも抽出が成功してしまった")
		}
	})
}

// TestSchemaValuesEqual_RejectsAnyDrift は schemaValuesEqual が 4 種いずれかの
// drift で false を返すことを単体テストする。drift gate の中核であるため、
// 比較ロジック自体に regression が入らないことを保証する。
func TestSchemaValuesEqual_RejectsAnyDrift(t *testing.T) {
	base := schemaValues{
		requiredFields: []string{"applies_when", "description", "model", "name", "prompt", "provider"},
		kebabCaseRe:    "^[a-z0-9]+(-[a-z0-9]+)*$",
		reservedNames:  []string{"context", "result"},
		validProviders: []string{"claude", "codex", "cursor", "cursor-sdk"},
	}

	if !schemaValuesEqual(base, base) {
		t.Errorf("同一値同士で false を返した")
	}

	// (s1) requiredFields drift
	d1 := base
	d1.requiredFields = []string{"description", "model", "name", "prompt", "provider"} // applies_when 欠落
	if schemaValuesEqual(base, d1) {
		t.Errorf("requiredFields drift を検出できなかった")
	}

	// (s2) kebabCaseRe drift
	d2 := base
	d2.kebabCaseRe = "^[a-z0-9_]+$"
	if schemaValuesEqual(base, d2) {
		t.Errorf("kebabCaseRe drift を検出できなかった")
	}

	// (s3) reservedNames drift
	d3 := base
	d3.reservedNames = []string{"context", "result", "session"}
	if schemaValuesEqual(base, d3) {
		t.Errorf("reservedNames drift を検出できなかった")
	}

	// (s4) validProviders drift
	d4 := base
	d4.validProviders = []string{"claude", "codex", "cursor", "openai"}
	if schemaValuesEqual(base, d4) {
		t.Errorf("validProviders drift を検出できなかった")
	}
}
