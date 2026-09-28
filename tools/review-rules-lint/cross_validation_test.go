// Package reviewruleslint - cross_validation_test.go は review-rules-skill 配布物
// (SKILL.md) 内部のクロス整合性 (frontmatter pin / Phase 3 ↔ Phase 4 同一規則集合 /
// 未置換 token 規約) を authoring-time に検証する drift gate である。
//
// ----------------------------------------------------------------------------
// 検証項目 (tasks.md task 3.4 / Req 4.4)
// ----------------------------------------------------------------------------
//
//	(a) SKILL.md frontmatter に `disable-model-invocation: true` が存在する
//	    根拠: Req 4.4 + design.md § Verification Contract (v2) item 6 (frontmatter pin)。
//	    Phase 3 明示承認ゲートを skill 起動段階より前に弱める経路 (CC description
//	    matching ベース auto-invoke / model background invocation) を排除する。
//
//	(b) Phase 3 rename_validation と Phase 4 Auto-Rename Cascade Rule が
//	    同一規則集合 (kebab-case 正規表現 / 予約語 result・context / 重複) を
//	    参照する
//	    根拠: Req 4.4 + design.md § Verification Contract (v2) item 6 (Phase 3 ↔
//	    Phase 4 同一規則集合)。drift 時は両セクションの該当文字列が異なる形で
//	    残ると検出可能になる。
//
//	(c) Phase 4 未置換 token 検査の正規表現が templates の token 命名規約と
//	    同一文字列 (`<[A-Z][A-Z0-9_]*>`) である
//	    根拠: Req 4.4 + design.md § Verification Contract (v2) item 6
//	    (未置換 token regex)。
//
// v2 で削除: templates 出荷状態の token-comment 対検査は無効化した。v2 では
// templates/*.yaml が「種 (枠)」となり `applies_when` / `prompt` を出荷状態で
// 持たないため、placeholder token 自体が出荷物に残らない (cf. design.md
// § Verification Contract (v2) item 6 末尾の文脈 / template_v2_shape_test.go)。
//
// ----------------------------------------------------------------------------
// 検査手段
// ----------------------------------------------------------------------------
//
// SKILL.md は markdown テキストを行単位で走査し、frontmatter / Phase 3 /
// Phase 4 セクション境界を `---` / `### Phase N` 行で抽出する。
//
// drift / 違反検出時は (filename + 違反内容) を t.Errorf で出力する。
//
// ----------------------------------------------------------------------------
// Layer Separation 不変式 (design.md § Layer Separation 不変式 / Req 5.1 / 5.2)
// ----------------------------------------------------------------------------
//
// 本ファイルは tools/review-rules-lint/ にのみ存在し、
// .agents/skills/review-rules-skill/ には一切同梱しない (Req 5.1 / Req 5.2)。
package reviewruleslint

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// crossSkillMDPath は SKILL.md (検証対象) への、tools/review-rules-lint/ 作業
// ディレクトリからの相対パス。language_contract_test.go の skillMDPath と値は
// 同じだが、別パッケージ変数として再宣言せず const として独立に保持する。
// 同一パッケージ内に同名 const があるため、本ファイルでは別名で保持して衝突を避ける。
const crossSkillMDPathRel = "../../.agents/skills/review-rules-skill/SKILL.md"

// kebabRegexLiteral は SKILL.md / design.md / references/schema.md で
// rule 名検証に使われる kebab-case 正規表現リテラル文字列。
// Phase 3 / Phase 4 両セクションがこの同一文字列を参照することを (b) 検査が要求する。
const kebabRegexLiteral = "^[a-z0-9]+(-[a-z0-9]+)*$"

// placeholderTokenRegexLiteral は Phase 4 未置換 token 検査の正規表現リテラル。
// templates 内 `<UPPER_SNAKE>` token の命名規約 (design.md § Placeholder 置換契約)
// と同一文字列であることを (c) 検査が要求する。
const placeholderTokenRegexLiteral = "<[A-Z][A-Z0-9_]*>"

// reservedNameLiterals は Phase 3 / Phase 4 が予約語として参照する rule 名集合の
// 文字列表現。両セクションに同じ語が同じ表記で登場することを (b) 検査が要求する。
var reservedNameLiterals = []string{"result", "context"}

// disableModelInvocationLine は SKILL.md frontmatter に存在すべき pin 行の正規表現。
// `disable-model-invocation:` の後に whitespace を挟み `true` が続く形を許容する。
var disableModelInvocationLine = regexp.MustCompile(`(?m)^\s*disable-model-invocation\s*:\s*true\s*$`)

// readSkillMD は SKILL.md 全文を string で読み込むヘルパ。
// io エラーは t.Fatalf でテストを停止させ、後続検査が誤検出しないようにする。
func readSkillMD(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(crossSkillMDPathRel)
	if err != nil {
		t.Fatalf("SKILL.md を読み込めなかった: %s: %v", crossSkillMDPathRel, err)
	}
	return string(data)
}

// extractFrontmatter は markdown 文書から先頭 YAML frontmatter ブロック
// (`---` で囲まれた領域) を取り出す。frontmatter が存在しない場合は ok=false を返す。
// 検出範囲は文書冒頭の `---` から次の `---` まで (両 delimiter 行は除外)。
func extractFrontmatter(content string) (block string, ok bool) {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != frontmatterDelimiter {
		return "", false
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r") == frontmatterDelimiter {
			return strings.Join(lines[1:i], "\n"), true
		}
	}
	return "", false
}

// extractPhaseSection は SKILL.md から `### Phase N: ...` 見出しで始まり、
// 次の `### Phase ` 見出し直前まで (= 同 Phase の本文) を抜き出す。
// phaseHeadingPrefix は `### Phase 3` のような prefix を渡す。
// 該当 Phase が見つからなかった場合は ok=false。
func extractPhaseSection(content, phaseHeadingPrefix string) (section string, ok bool) {
	lines := strings.Split(content, "\n")
	startIdx := -1
	for i, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		if strings.HasPrefix(trimmed, phaseHeadingPrefix) {
			startIdx = i
			break
		}
	}
	if startIdx == -1 {
		return "", false
	}
	endIdx := len(lines)
	for i := startIdx + 1; i < len(lines); i++ {
		trimmed := strings.TrimRight(lines[i], "\r")
		if strings.HasPrefix(trimmed, "### Phase ") {
			endIdx = i
			break
		}
	}
	return strings.Join(lines[startIdx:endIdx], "\n"), true
}

// hasDisableModelInvocationPin は (a) 検査ロジック本体。
// frontmatter 内に `disable-model-invocation: true` 行が含まれることを確認する。
func hasDisableModelInvocationPin(frontmatter string) bool {
	return disableModelInvocationLine.MatchString(frontmatter)
}

// missingFromBoth は s1 / s2 のいずれかにキーワード kw が登場しない場合に
// その由来 (どちらに不足しているか) を文字列化して返す。両方含まれる場合は空文字列。
// (b) 検査で「Phase 3 ↔ Phase 4 同一規則集合の文字列共有」を判定するために用いる。
func missingFromBoth(s1, s2, kw string) string {
	in1 := strings.Contains(s1, kw)
	in2 := strings.Contains(s2, kw)
	switch {
	case !in1 && !in2:
		return "Phase 3 / Phase 4 両方に欠落"
	case !in1:
		return "Phase 3 に欠落"
	case !in2:
		return "Phase 4 に欠落"
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// 単体テスト (lint ロジック自体が drift を検出することを証明する)
// ---------------------------------------------------------------------------

// TestHasDisableModelInvocationPin_DetectsAbsence は (a) 検査ロジックが
// frontmatter から `disable-model-invocation: true` が消えた場合に検出することを
// 合成 frontmatter で証明する。
// 出荷 SKILL.md は pin を保持しているため、TestSkillMDFrontmatterPin だけでは
// 「lint が scaffolding か否か」が判別できない。本テストはその不足を補う。
func TestHasDisableModelInvocationPin_DetectsAbsence(t *testing.T) {
	cases := []struct {
		desc        string
		frontmatter string
		want        bool
	}{
		{
			desc: "正常: disable-model-invocation: true を含む",
			frontmatter: "name: x\n" +
				"description: y\n" +
				"disable-model-invocation: true\n",
			want: true,
		},
		{
			desc: "drift: 行ごと欠落",
			frontmatter: "name: x\n" +
				"description: y\n",
			want: false,
		},
		{
			desc: "drift: false に書き換え",
			frontmatter: "name: x\n" +
				"disable-model-invocation: false\n",
			want: false,
		},
		{
			desc:        "正常: 行頭 whitespace + 値前後 whitespace",
			frontmatter: "  disable-model-invocation:   true   \n",
			want:        true,
		},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got := hasDisableModelInvocationPin(c.frontmatter)
			if got != c.want {
				t.Errorf("hasDisableModelInvocationPin(%q) = %v, 期待 %v", c.frontmatter, got, c.want)
			}
		})
	}
}

// TestMissingFromBoth_DetectsDrift は (b) の同一規則集合判定ロジックが、
// 文字列が片側にしか存在しないケースを正しく検出することを証明する。
func TestMissingFromBoth_DetectsDrift(t *testing.T) {
	cases := []struct {
		desc    string
		s1      string
		s2      string
		kw      string
		wantErr bool // missingFromBoth が非空文字列を返すことを期待するか
	}{
		{
			desc:    "両方含む (drift なし)",
			s1:      "a kebab-case rule",
			s2:      "kebab-case here",
			kw:      "kebab-case",
			wantErr: false,
		},
		{
			desc:    "片側欠落 (Phase 4 側に無い)",
			s1:      "kebab-case",
			s2:      "別の文言",
			kw:      "kebab-case",
			wantErr: true,
		},
		{
			desc:    "両方欠落",
			s1:      "x",
			s2:      "y",
			kw:      "kebab-case",
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got := missingFromBoth(c.s1, c.s2, c.kw)
			if (got != "") != c.wantErr {
				t.Errorf("missingFromBoth(%q, %q, %q) = %q, wantErr=%v", c.s1, c.s2, c.kw, got, c.wantErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 統合テスト (出荷 SKILL.md が全検査を通過することを保証する)
// ---------------------------------------------------------------------------

// TestSkillMDFrontmatterPin は (a) 検査: 出荷 SKILL.md frontmatter に
// `disable-model-invocation: true` が含まれることを確認する
// (Req 4.4 + design.md § Verification Contract (v2) item 6 保護)。
func TestSkillMDFrontmatterPin(t *testing.T) {
	content := readSkillMD(t)
	frontmatter, ok := extractFrontmatter(content)
	if !ok {
		t.Fatalf("SKILL.md frontmatter を抽出できなかった (先頭が `---` で始まっていないか、終端 `---` が無い)")
	}
	if !hasDisableModelInvocationPin(frontmatter) {
		t.Errorf("SKILL.md frontmatter に `disable-model-invocation: true` が見つからない (Req 4.4 / design.md § Verification Contract (v2) item 6 違反)。frontmatter=\n%s", frontmatter)
	}
}

// TestPhase3Phase4SameRuleSet は (b) 検査: Phase 3 rename_validation と
// Phase 4 Auto-Rename Cascade Rule の本文が、同一規則集合 (kebab-case 正規表現 /
// 予約語 result・context / 重複規則) を文字列レベルで共有していることを確認する。
// drift (例: Phase 3 と Phase 4 で kebab-case regex が異なる文字列で記述) を検出する。
func TestPhase3Phase4SameRuleSet(t *testing.T) {
	content := readSkillMD(t)

	phase3, ok := extractPhaseSection(content, "### Phase 3")
	if !ok {
		t.Fatalf("SKILL.md から Phase 3 セクションを抽出できなかった")
	}
	phase4, ok := extractPhaseSection(content, "### Phase 4")
	if !ok {
		t.Fatalf("SKILL.md から Phase 4 セクションを抽出できなかった")
	}

	// (b-1) kebab-case 正規表現リテラルが両セクションに同一文字列で出現する。
	if msg := missingFromBoth(phase3, phase4, kebabRegexLiteral); msg != "" {
		t.Errorf("kebab-case 正規表現 %q の drift: %s (Req 4.4 / design.md § Verification Contract (v2) item 6 違反)", kebabRegexLiteral, msg)
	}

	// (b-2) 予約語 result / context が両セクションに同一文字列で出現する。
	for _, name := range reservedNameLiterals {
		// 予約語の単独文字列だと一般語と衝突するため、SKILL.md の表記揺れ吸収のため
		// バックティック付き形 (`result` / `context`) を契約として要求する。
		quoted := "`" + name + "`"
		if msg := missingFromBoth(phase3, phase4, quoted); msg != "" {
			t.Errorf("予約語表記 %q の drift: %s", quoted, msg)
		}
	}

	// (b-3) 重複規則 (同セッション内重複) を表す文字列を両セクションに要求する。
	// SKILL.md Phase 3 は「同セッション内」「重複」、Phase 4 Auto-Rename Cascade も
	// 「同セッション内」「重複」を文字列として持つ。両表記が drift しないこと。
	for _, kw := range []string{"同セッション内", "重複"} {
		if msg := missingFromBoth(phase3, phase4, kw); msg != "" {
			t.Errorf("重複規則キーワード %q の drift: %s", kw, msg)
		}
	}

	// (b-4) confirmed_rules.rule_name に対する重複規則の参照が両セクションに残っていること。
	// (Phase 3 の rename_validation と Phase 4 Auto-Rename Cascade Rule が
	// 同じ集合 = `confirmed_rules.rule_name` を参照することを文字列で固定)
	if msg := missingFromBoth(phase3, phase4, "confirmed_rules.rule_name"); msg != "" {
		t.Errorf("`confirmed_rules.rule_name` 参照の drift: %s", msg)
	}
}

// TestPhase4UnreplacedTokenRegex は (c) 検査: Phase 4 セクション内に
// 未置換 token 検査の正規表現リテラル `<[A-Z][A-Z0-9_]*>` が含まれることを確認する。
// templates の token 命名規約 (design.md § Catalog domain Placeholder 置換契約) と
// 同一文字列であることが drift gate の本質。
func TestPhase4UnreplacedTokenRegex(t *testing.T) {
	content := readSkillMD(t)
	phase4, ok := extractPhaseSection(content, "### Phase 4")
	if !ok {
		t.Fatalf("SKILL.md から Phase 4 セクションを抽出できなかった")
	}
	if !strings.Contains(phase4, placeholderTokenRegexLiteral) {
		t.Errorf("Phase 4 セクションに未置換 token 検査の正規表現 %q が見つからない (Req 4.4 / design.md § Verification Contract (v2) item 6 違反)", placeholderTokenRegexLiteral)
	}
}
