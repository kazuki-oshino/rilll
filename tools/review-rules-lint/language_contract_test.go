// language_contract_test.go は review-rules-skill 配布物 (SKILL.md / templates/*.yaml)
// が design.md § Language Contract に列挙された言語分担規則を満たすことを authoring-time に
// 検証する drift gate である。
//
// ----------------------------------------------------------------------------
// 検出ルール (機械的に列挙された言語分担規則の検査契約)
// ----------------------------------------------------------------------------
//
// design.md § Language Contract 由来の固定規則を以下のとおり機械検査する:
//
//	(L1) SKILL.md frontmatter `description` 行は ASCII printable のみで構成される
//	     (= 全文字の rune が 0x20 (space) 〜 0x7E (~) の範囲)。
//	     根拠: § Language Contract「frontmatter `description` の英語固定は本 spec で
//	     唯一許容される target language 例外」(design.md L92)。
//	(L2) SKILL.md 本文 (frontmatter ブロック外、コードフェンス外) の各行に
//	     「英語のみで書かれた本文行」が混入していない。
//	     根拠: § Language Contract「SKILL.md 本文 (Core Mission / Execution Steps 等) は
//	     日本語」(design.md L82)。
//	(L3) templates/*.yaml の `description` / `applies_when` / `prompt` フィールド値
//	     (block scalar 内の本文テキスト) および `# ...` コメント行に「英語のみで
//	     書かれた本文行」が混入していない。
//	     根拠: § Language Contract「templates/*.yaml の YAML フィールド値
//	     (`description` / `applies_when` / `prompt`) と YAML コメント (`# ...`) は日本語」
//	     (design.md L85-L86)。
//	(L4) references/aspects/*.md の本文 (見出し外 / コードフェンス外) に「英語のみで
//	     書かれた本文行」が混入していない。
//	     根拠: design.md § Verification Contract (v2) item 5「language_contract_test.go
//	     (維持): 日本語領域への英語のみ行混入検出を継続。検査範囲に
//	     `references/aspects/*.md` を追加」/ Req 4.4。aspect ガイドは frontmatter を
//	     持たず `# <title>` 見出しから始まるため、走査側は「先頭非空行が `---` の
//	     ファイル」のみを frontmatter ありとして扱う (= 本文中の水平線 `---` を
//	     frontmatter と誤認しない)。
//
// ----------------------------------------------------------------------------
// 「英語のみで書かれた本文行」の機械的定義
// ----------------------------------------------------------------------------
//
// 行が以下を全て満たす場合に「英語のみで書かれた本文行 (English-only prose line)」と判定する:
//
//	(D1) 当該行に日本語文字 (ひらがな / カタカナ / CJK 統合漢字 / CJK 句読点) を一切含まない
//	(D2) backtick で囲まれた inline code (`...`) / `<UPPER_SNAKE>` token /
//	     `# <PLACEHOLDER: ...>` 注記 / URL を「除去」した残りの素テキストに、
//	     アルファベット連続 3 文字以上の英単語が 2 個以上含まれる
//	     (= 識別子参照 1 つだけの行や、コメント `# config/` のような単一識別子行は除外)
//	(D3) 行末記号・装飾のみの行ではない (D2 の素テキストに最低限の語数を要求することで担保)
//
// ----------------------------------------------------------------------------
// 例外領域 (検査対象から外す行 / 領域)
// ----------------------------------------------------------------------------
//
//	(E1) SKILL.md frontmatter ブロック (`---` ペアで囲まれた YAML 領域) の全行
//	     ※ frontmatter `description` 行のみ別途 (L1) で ASCII 検査する
//	(E2) コードフェンス (```) 内の行
//	     ※ コードフェンスは ``` で開始し、次の ``` で終了する
//	(E3) 空行 (trim 後 0 文字)
//	(E4) markdown 見出し行 (行頭が `#`+ 半角 space)
//	(E5) markdown 区切り行 (`---` 単独 / `===` 単独 / 連続 hyphen)
//	(E6) markdown 表の罫線行 (`|`, `-`, `:`, space のみで構成)
//	(E7) YAML スカラー key 行 (`<key>:` のみ / `<key>: <kebab-case 値>` / `<key>: |`)
//	(E8) YAML リスト要素のうち backtick 識別子・URL・パス参照のみで構成された行
//	     (ヘルパ `removeNoise` で全要素が除去された結果、英単語が 1 個以下になる行)
//
// 例外領域不変式: 上記以外の領域 (= 散文領域) で英語のみ行が見つかった場合は契約違反として
// FAIL させる。新規例外領域を追加する場合は本テストファイル冒頭の (E*) を更新するとともに
// design.md § Language Contract への追記を必須とする (暗黙拡張禁止)。
//
// ----------------------------------------------------------------------------
// drift 検出時の出力契約
// ----------------------------------------------------------------------------
//
// 違反検出時は (filename + 行番号 + 違反行の内容) を含む t.Errorf を発行し、
// 利用者がエディタで即座に該当行に移動できる粒度の情報を出す。
//
// 配布物には含めない (Req 5.1 / Req 5.2)。本ファイルは tools/review-rules-lint/ にのみ存在し、
// .agents/skills/review-rules-skill/ には一切同梱しない (Layer Separation 不変式)。
//
// 参照: design.md § Language Contract / § Verification Contract (v2) item 5 /
// § Layer Separation 不変式 / 検査範囲: SKILL.md + templates/*.yaml +
// references/aspects/*.md / Req 4.4 / Req 5.1 / Req 5.2。
package reviewruleslint

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// skillMDPath は SKILL.md (検証対象) への、tools/review-rules-lint/ 作業ディレクトリからの相対パス。
const skillMDPath = "../../.agents/skills/review-rules-skill/SKILL.md"

// frontmatterDelimiter は YAML frontmatter ブロックの区切り行 (上下対称)。
const frontmatterDelimiter = "---"

// codeFenceMarker は markdown コードフェンスの開始 / 終了行 prefix。
const codeFenceMarker = "```"

// japaneseRangeTable は「日本語文字」と判定する Unicode 範囲集合。
// ひらがな / カタカナ (半角・全角) / CJK 統合漢字 / CJK 句読点を網羅する。
//
// CJK 句読点 (`、` `。` `「」` `『』` `・` 等) は U+3000-U+303F の CJK Symbols and Punctuation に含まれ、
// 全角コロン `：` は U+FF1A (Halfwidth and Fullwidth Forms) に含まれる。日本語文字判定は
// Hiragana / Katakana / Han (CJK Unified Ideographs) に加えて上記 2 範囲を含めて判定する。
var japaneseRangeTable = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x3000, Hi: 0x303F, Stride: 1}, // CJK Symbols and Punctuation
		{Lo: 0x3040, Hi: 0x309F, Stride: 1}, // Hiragana
		{Lo: 0x30A0, Hi: 0x30FF, Stride: 1}, // Katakana
		{Lo: 0x3400, Hi: 0x4DBF, Stride: 1}, // CJK Unified Ideographs Extension A
		{Lo: 0x4E00, Hi: 0x9FFF, Stride: 1}, // CJK Unified Ideographs
		{Lo: 0xFF00, Hi: 0xFFEF, Stride: 1}, // Halfwidth and Fullwidth Forms (全角英数 / 全角記号)
	},
}

// inlineCodeRe は backtick で囲まれた inline code (`...`) の正規表現。
// 1 行内の最短一致で複数 inline code をすべて除去するために使う。
var inlineCodeRe = regexp.MustCompile("`[^`]+`")

// placeholderTokenRe はテンプレート本文中の `<UPPER_SNAKE>` token の正規表現。
// design.md § Catalog domain Placeholder 置換契約 / SKILL.md Phase 4 と同一規約。
var placeholderTokenRe = regexp.MustCompile(`<[A-Z][A-Z0-9_]*>`)

// placeholderCommentRe は templates 内の `<PLACEHOLDER: ...>` 注記 (山括弧内 colon 付き) の正規表現。
// `# <PLACEHOLDER: 差し替え方針>` 全体ではなく、内側の `<PLACEHOLDER: ...>` 部を識別する。
var placeholderCommentRe = regexp.MustCompile(`<PLACEHOLDER:[^>]*>`)

// urlRe は http(s) URL を行から除去するための正規表現。URL 内の英単語を散文と誤判定しないため。
var urlRe = regexp.MustCompile(`https?://\S+`)

// markdownLinkURLRe は markdown link `[text](url)` の `(url)` 部分を除去するための正規表現。
// `(url)` 内が英字パスや URL の場合、英単語を散文と誤判定しないため。
var markdownLinkURLRe = regexp.MustCompile(`\([a-zA-Z0-9:/._\-~?#%&=]+\)`)

// englishWordRe は「アルファベット連続 3 文字以上」を英単語として抽出する。
// 短い接続詞や記号 (例: `a` / `or`) を除外し、誤検出を避ける。
var englishWordRe = regexp.MustCompile(`[A-Za-z]{3,}`)

// headingRe は markdown 見出し行 (`#` 1〜6 個 + 半角 space + 内容) を識別する。
var headingRe = regexp.MustCompile(`^#{1,6} `)

// horizontalRuleRe は markdown 水平線 (連続 hyphen / equals / asterisk) のみで構成された行。
var horizontalRuleRe = regexp.MustCompile(`^[-=*]{3,}$`)

// tableSeparatorRe は markdown 表の罫線行 (`|`, `-`, `:`, space のみで構成) を識別する。
var tableSeparatorRe = regexp.MustCompile(`^\|?[\s|:\-]+\|?$`)

// yamlKeyOnlyRe は YAML スカラー key 行 (`<key>:` 単独 / `<key>: |` / `<key>: <kebab-case 値>`) を識別する。
// 値が日本語散文の場合 (`description: |` のような block scalar 開始は値が次行以降にあるため key 行のみ) は
// 本 regex で除外し、続く block scalar 内の各行は別途散文行として検査する。
var yamlKeyOnlyRe = regexp.MustCompile(`^[a-z_][a-zA-Z0-9_]*:(\s*\|?\s*|\s+[a-zA-Z0-9._\-/]+\s*)$`)

// hasJapanese は s に日本語文字が 1 文字でも含まれるかを判定する。
func hasJapanese(s string) bool {
	for _, r := range s {
		if unicode.In(r, japaneseRangeTable) {
			return true
		}
	}
	return false
}

// removeNoise は行から「英単語ではないノイズ要素」を除去し、英語散文判定の対象となる素テキストを返す。
// 除去する要素:
//   - backtick で囲まれた inline code (例: `templates/security-review.yaml`)
//   - `<UPPER_SNAKE>` token (例: `<MAIN_LANG>`)
//   - `<PLACEHOLDER: ...>` 注記 (templates 内の差し替え方針コメント)
//   - markdown link の URL 部分 (`[text](url)` の `(url)`)
//   - http(s) URL
//
// 除去後の素テキストに対して英単語連続を数えることで、識別子参照のみの行を散文と誤検出することを避ける。
func removeNoise(line string) string {
	cleaned := inlineCodeRe.ReplaceAllString(line, " ")
	cleaned = placeholderTokenRe.ReplaceAllString(cleaned, " ")
	cleaned = placeholderCommentRe.ReplaceAllString(cleaned, " ")
	cleaned = markdownLinkURLRe.ReplaceAllString(cleaned, " ")
	cleaned = urlRe.ReplaceAllString(cleaned, " ")
	return cleaned
}

// fileKind はファイル種別を表す列挙。markdown と YAML で例外領域 (見出し vs コメント) の
// 解釈が異なるため、判定ヘルパに kind を伝達する。
type fileKind int

const (
	fileKindMarkdown fileKind = iota // SKILL.md など。`# ` 始まりは見出しとして除外
	fileKindYAML                     // templates/*.yaml。`# ` 始まりは YAML コメントとして検査対象
)

// isEnglishOnlyProseLine は line が「英語のみで書かれた本文行」(D1)(D2)(D3) を満たすかを判定する。
// 例外領域 (E1)〜(E8) のうち、ファイル種別に依存しない (E3)〜(E8) を本関数内で除外する。
// (E1)(E2) はファイル走査側 (frontmatter / コードフェンス追跡) で除外する責務とする。
//
// kind パラメータでファイル種別を指定する:
//   - fileKindMarkdown: SKILL.md など。`# ` で始まる行は見出しとして除外する (E4)
//   - fileKindYAML:     templates/*.yaml。`# ` で始まる行は YAML コメントとして「散文行」検査対象に含める
//     (= 見出しとしては除外しない。YAML コメント内に英語のみ散文行が混入した場合は検出する)
func isEnglishOnlyProseLine(line string, kind fileKind) bool {
	stripped := strings.TrimSpace(line)
	// (E3) 空行
	if stripped == "" {
		return false
	}
	// (D1) 日本語文字を 1 つでも含む行は対象外
	if hasJapanese(line) {
		return false
	}
	// (E4) markdown 見出し行 (`# ` 〜 `###### `)
	// YAML ファイルでは `# ` は YAML コメントなので除外しない。
	if kind == fileKindMarkdown && headingRe.MatchString(stripped) {
		return false
	}
	// (E5) 水平線 / 区切り行
	if horizontalRuleRe.MatchString(stripped) {
		return false
	}
	// (E6) 表の罫線行 (例: `|---|---|---|`)
	if tableSeparatorRe.MatchString(stripped) {
		return false
	}
	// (E7) YAML スカラー key 行 (block scalar 開始 `desc: |` を含む)
	if yamlKeyOnlyRe.MatchString(stripped) {
		return false
	}
	// (D2)(E8) ノイズ除去後の素テキストに英単語が 2 個以上含まれるかで判定
	cleaned := removeNoise(line)
	words := englishWordRe.FindAllString(cleaned, -1)
	return len(words) >= 2
}

// englishOnlyViolation は 1 件の Language Contract 違反 (ファイル名 + 行番号 + 違反行内容) を表す。
type englishOnlyViolation struct {
	filename   string
	lineNumber int
	content    string
}

// scanFileForEnglishOnly はファイルテキスト全体を走査し、(E1) frontmatter / (E2) コードフェンスを
// 動的に追跡しながら、本文領域における英語のみ行 (D1)(D2)(D3) を検出する。
//
// kind パラメータ:
//   - fileKindMarkdown: `---` で囲まれた frontmatter ブロック (E1) と ``` で囲まれた
//     コードフェンス (E2) を本走査の対象外とする。本文中の `# ` 始まり行は (E4) 見出しとして除外。
//     ただし frontmatter 判定は「先頭非空行が `---` の場合のみ」発動する: SKILL.md は
//     line 1 が `---` のため通常通り frontmatter ありとして扱われる一方、aspect ガイド
//     (`references/aspects/*.md`) は `# <title>` から始まるため frontmatter なしと判定され、
//     本文中に `---` (水平線) が出現しても frontmatter ブロックと誤認しない。
//   - fileKindYAML:     templates/*.yaml。frontmatter ブロックは存在しない (= 全行を本文扱い)。
//     コードフェンスも存在しないが、安全側で ``` 始まり行があれば追跡する (templates 内に
//     ``` は出現しない設計だが、誤って混入した場合の挙動を予測可能にしておく)。
//
// frontmatter description 行は別ロジック (extractFrontmatterDescription + isASCIIPrintableOnly) で
// 個別に検査するため、本走査では検出しない (= 二重検出を避ける)。
func scanFileForEnglishOnly(filename, content string, kind fileKind) []englishOnlyViolation {
	var violations []englishOnlyViolation
	lines := strings.Split(content, "\n")

	// hasFrontmatter は「先頭非空行が `---` か」で動的に決定する。
	// markdown ファイルでも aspect ガイドのように先頭が見出しから始まるファイルは
	// frontmatter なしと判定し、本文中の `---` (水平線 / 区切り) を frontmatter
	// 開始記号と誤認しないようにする。
	hasFrontmatter := false
	if kind == fileKindMarkdown {
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			hasFrontmatter = trimmed == frontmatterDelimiter
			break
		}
	}

	inFrontmatter := false
	frontmatterDelimiterCount := 0
	inCodeFence := false

	for i, line := range lines {
		lineNumber := i + 1
		trimmed := strings.TrimSpace(line)

		// (E1) frontmatter ブロック追跡 (先頭非空行が `---` であった markdown のみ)
		// 1 つ目の `---` で開始、2 つ目の `---` で終了。先頭非空行ガードにより、
		// 本文中の水平線 `---` は frontmatter delimiter として解釈されない。
		if hasFrontmatter && trimmed == frontmatterDelimiter {
			frontmatterDelimiterCount++
			if frontmatterDelimiterCount == 1 {
				inFrontmatter = true
				continue
			} else if frontmatterDelimiterCount == 2 {
				inFrontmatter = false
				continue
			}
		}
		if inFrontmatter {
			continue
		}

		// (E2) コードフェンス追跡: ``` で開始し、次の ``` で終了
		if strings.HasPrefix(trimmed, codeFenceMarker) {
			inCodeFence = !inCodeFence
			continue
		}
		if inCodeFence {
			continue
		}

		if isEnglishOnlyProseLine(line, kind) {
			violations = append(violations, englishOnlyViolation{
				filename:   filename,
				lineNumber: lineNumber,
				content:    line,
			})
		}
	}

	return violations
}

// extractFrontmatterDescription は SKILL.md 等の YAML frontmatter から `description:` 行
// (key + value 全体) を抽出する。frontmatter ブロックが見つからない場合は ("", false) を返す。
//
// description 値は YAML 上 single-line 文字列 (`description: "..."`) または block scalar (`description: |`)
// のいずれもありうるが、本 skill の SKILL.md frontmatter は single-line double-quoted 文字列で
// 固定されており (design.md § frontmatter L428 参照)、本テストもその前提で 1 行抽出を行う。
// block scalar 形式に変更された場合は本テストが PASS 続行しないよう、value が空 / 改行のみの場合は
// (line, true) を返してそのまま (L1) ASCII printable 検査にかけ、value 行が見つからない frontmatter
// 構造の drift を検出させる。
func extractFrontmatterDescription(content string) (string, bool) {
	lines := strings.Split(content, "\n")
	inFrontmatter := false
	frontmatterDelimiterCount := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == frontmatterDelimiter {
			frontmatterDelimiterCount++
			if frontmatterDelimiterCount == 1 {
				inFrontmatter = true
				continue
			} else if frontmatterDelimiterCount == 2 {
				return "", false
			}
		}
		if !inFrontmatter {
			continue
		}
		if strings.HasPrefix(line, "description:") {
			return line, true
		}
	}
	return "", false
}

// isASCIIPrintableOnly は s の全 rune が ASCII printable (0x20-0x7E) のみで構成されているかを判定する。
// space (0x20) も printable に含む。tab / newline / その他 control character / 非 ASCII は不許可。
func isASCIIPrintableOnly(s string) bool {
	for _, r := range s {
		if r < 0x20 || r > 0x7E {
			return false
		}
	}
	return true
}

// TestSKILLMDFrontmatterDescription_ASCIIPrintable は (L1) を検証する。
// SKILL.md の frontmatter `description:` 行が ASCII printable のみで構成されていることを確認する。
// 違反検出時は当該行内容と最初の非 ASCII rune の位置 / Unicode code point を出力する。
func TestSKILLMDFrontmatterDescription_ASCIIPrintable(t *testing.T) {
	data, err := os.ReadFile(skillMDPath)
	if err != nil {
		t.Fatalf("SKILL.md 読み込み失敗: %s: %v", skillMDPath, err)
	}
	descLine, ok := extractFrontmatterDescription(string(data))
	if !ok {
		t.Fatalf("SKILL.md frontmatter から `description:` 行を抽出できなかった: %s "+
			"(frontmatter `---` ペアまたは description 行が想定構造から外れている可能性)", skillMDPath)
	}
	if isASCIIPrintableOnly(descLine) {
		return
	}
	// 違反: 最初の非 ASCII rune の position と code point を抽出して報告
	for idx, r := range descLine {
		if r < 0x20 || r > 0x7E {
			t.Errorf("%s: frontmatter description 行 (L1 違反) に非 ASCII printable 文字が混入:\n"+
				"  内容: %s\n"+
				"  位置: byte offset %d, rune U+%04X (%q)",
				filepath.Base(skillMDPath), descLine, idx, r, string(r))
			return
		}
	}
}

// TestSKILLMDBody_NoEnglishOnlyProseLines は (L2) を検証する。
// SKILL.md の本文領域 (frontmatter 外、コードフェンス外) に英語のみ散文行が混入していないことを
// 確認する。違反時は filename + 行番号 + 違反行内容を出力する。
func TestSKILLMDBody_NoEnglishOnlyProseLines(t *testing.T) {
	data, err := os.ReadFile(skillMDPath)
	if err != nil {
		t.Fatalf("SKILL.md 読み込み失敗: %s: %v", skillMDPath, err)
	}
	violations := scanFileForEnglishOnly(filepath.Base(skillMDPath), string(data), fileKindMarkdown)
	for _, v := range violations {
		t.Errorf("%s:L%d: 英語のみ散文行検出 (L2 違反): %s", v.filename, v.lineNumber, v.content)
	}
}

// TestTemplates_NoEnglishOnlyProseLines は (L3) を検証する。
// templates/*.yaml の本文 (コメント行 / block scalar 内の散文行) に英語のみ散文行が
// 混入していないことを確認する。templates は frontmatter を持たないため hasFrontmatter=false。
// 違反時は filename + 行番号 + 違反行内容を出力する。
func TestTemplates_NoEnglishOnlyProseLines(t *testing.T) {
	files := listTemplateFiles(t)
	for _, path := range files {
		filename := filepath.Base(path)
		t.Run(filename, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s: 読み込み失敗: %v", filename, err)
			}
			violations := scanFileForEnglishOnly(filename, string(data), fileKindYAML)
			for _, v := range violations {
				t.Errorf("%s:L%d: 英語のみ散文行検出 (L3 違反): %s", v.filename, v.lineNumber, v.content)
			}
		})
	}
}

// TestAspectGuides_NoEnglishOnlyProseLines は (L4) を検証する。
// references/aspects/<aspect>.md の本文 (見出し外 / コードフェンス外) に英語のみ
// 散文行が混入していないことを確認する (Req 4.4 / design.md § Verification
// Contract (v2) item 5)。aspect ガイドは frontmatter を持たず `# <title>` 見出しから
// 始まるため、scanFileForEnglishOnly は「先頭非空行が `---` か」で frontmatter
// 有無を動的判定する。これにより本文中に水平線 `---` が出現しても frontmatter
// 開始と誤認せず、本文走査が正しく行われる。
//
// listAspectFiles / aspectsDir は同 package の aspect_guide_test.go で定義済みの
// SSoT を再利用する (重複宣言を避け、走査対象集合を 1 箇所に固定するため)。
func TestAspectGuides_NoEnglishOnlyProseLines(t *testing.T) {
	files := listAspectFiles(t)
	for _, path := range files {
		filename := filepath.Base(path)
		t.Run(filename, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s: 読み込み失敗: %v", filename, err)
			}
			violations := scanFileForEnglishOnly(filename, string(data), fileKindMarkdown)
			for _, v := range violations {
				t.Errorf("%s:L%d: 英語のみ散文行検出 (L4 違反): %s", v.filename, v.lineNumber, v.content)
			}
		})
	}
}

// TestIsEnglishOnlyProseLine_DetectsViolations は判定ヘルパ isEnglishOnlyProseLine が
// 「英語のみ散文行」の判定基準 (D1)(D2)(D3) を実体的に満たすことを単体テストする。
// 出荷状態のファイルが contract を満たしているため、本テストが無いとロジックが scaffolding
// (常に false 返却) であるか実検査かを判別できない。本テストはその不足を補う。
func TestIsEnglishOnlyProseLine_DetectsViolations(t *testing.T) {
	type tc struct {
		desc string
		line string
		kind fileKind
		want bool
	}
	cases := []tc{
		// 違反パターン (true を期待)
		{
			desc: "英語のみ散文行 (SKILL.md 本文に英語短文が混入)",
			line: `This skill bootstraps review rules from a built-in template catalog.`,
			kind: fileKindMarkdown,
			want: true,
		},
		{
			desc: "英語のみ散文行 (templates/*.yaml の description に英語混入)",
			line: `  Reviews the security perimeter of authentication and credential handling.`,
			kind: fileKindYAML,
			want: true,
		},
		{
			desc: "英語のみ散文行 (templates/*.yaml の YAML コメントに英語混入)",
			line: `# This template is for security review of the target repo.`,
			kind: fileKindYAML,
			want: true,
		},
		// 非違反パターン (false を期待)
		{
			desc: "日本語散文行 (適合)",
			line: "対象 repo の sensitive area と main 言語の脆弱性パターンに焦点を絞る",
			kind: fileKindMarkdown,
			want: false,
		},
		{
			desc: "空行",
			line: "   ",
			kind: fileKindMarkdown,
			want: false,
		},
		{
			desc: "markdown 見出し (markdown 文脈では除外)",
			line: "## Execution Steps",
			kind: fileKindMarkdown,
			want: false,
		},
		{
			desc: "水平線 (markdown 区切り)",
			line: "---",
			kind: fileKindMarkdown,
			want: false,
		},
		{
			desc: "表罫線",
			line: "|---|---|---|",
			kind: fileKindMarkdown,
			want: false,
		},
		{
			desc: "YAML key 行 + 値が kebab-case (機械識別子)",
			line: "name: security-review",
			kind: fileKindYAML,
			want: false,
		},
		{
			desc: "YAML block scalar 開始 (key のみ)",
			line: "description: |",
			kind: fileKindYAML,
			want: false,
		},
		{
			desc: "識別子参照のみの箇条書き行 (backtick 内 path のみ)",
			line: "- `templates/security-review.yaml`",
			kind: fileKindMarkdown,
			want: false,
		},
		{
			desc: "placeholder token + 単一識別子のみ (英単語 1 個未満として除外)",
			line: "  <MAIN_LANG>",
			kind: fileKindYAML,
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got := isEnglishOnlyProseLine(c.line, c.kind)
			if got != c.want {
				t.Errorf("isEnglishOnlyProseLine(%q, %v) = %v, want %v", c.line, c.kind, got, c.want)
			}
		})
	}
}

// TestScanFileForEnglishOnly_DetectsDrift は scanFileForEnglishOnly が drifted fixture
// (frontmatter 外への英語散文行混入 / コードフェンス内英語は無視 / templates 形式での
// 英語混入) を実値レベルで検出することを単体テストする。出荷ファイルが clean なため
// 本テストが無いと走査ロジックが scaffolding か実体かを判別できない。
func TestScanFileForEnglishOnly_DetectsDrift(t *testing.T) {
	t.Run("SKILL.md drifted fixture: frontmatter 外本文に英語散文行が混入", func(t *testing.T) {
		fixture := "---\n" +
			"name: review-rules-skill\n" +
			"description: \"Bootstrap review rules by analyzing the target repo.\"\n" +
			"---\n" +
			"\n" +
			"# review-rules-skill\n" +
			"\n" +
			"## Overview\n" +
			"\n" +
			"This skill bootstraps review rules using a built-in template catalog.\n" + // L10: 違反 (英語散文)
			"本 skill は templates/ から rule を生成する。\n" +
			""
		violations := scanFileForEnglishOnly("SKILL.md", fixture, fileKindMarkdown)
		if len(violations) != 1 {
			t.Fatalf("英語散文行を 1 件検出することを期待したが %d 件: %+v", len(violations), violations)
		}
		if violations[0].lineNumber != 10 {
			t.Errorf("違反行番号: got=%d, want=10", violations[0].lineNumber)
		}
		if !strings.Contains(violations[0].content, "bootstraps review rules") {
			t.Errorf("違反内容に英語散文が含まれていない: %s", violations[0].content)
		}
	})

	t.Run("SKILL.md fixture: frontmatter 内 description は本走査の対象外 (個別検査側に委譲)", func(t *testing.T) {
		// frontmatter description が英語でも、本走査では検出しない (= 本走査は本文専用)
		fixture := "---\n" +
			"name: review-rules-skill\n" +
			"description: \"Bootstrap review rules by analyzing the target repo.\"\n" +
			"---\n" +
			"\n" +
			"本 skill は templates/ から rule を生成する。\n"
		violations := scanFileForEnglishOnly("SKILL.md", fixture, fileKindMarkdown)
		if len(violations) != 0 {
			t.Errorf("frontmatter 内 description は本走査の対象外であるべきだが検出された: %+v", violations)
		}
	})

	t.Run("SKILL.md fixture: コードフェンス内英語は除外される", func(t *testing.T) {
		fixture := "---\n" +
			"name: review-rules-skill\n" +
			"description: \"x\"\n" +
			"---\n" +
			"\n" +
			"本文の日本語行。\n" +
			"\n" +
			"```yaml\n" +
			"phase_1_output:\n" +
			"  tech_stack:\n" +
			"    languages: [string]\n" +
			"```\n" +
			"\n" +
			"続く日本語本文。\n"
		violations := scanFileForEnglishOnly("SKILL.md", fixture, fileKindMarkdown)
		// frontmatter (`---`...`---`) と コードフェンス内 yaml は (E1)(E2) で除外される。
		// したがって 0 件。
		if len(violations) != 0 {
			t.Errorf("frontmatter / コードフェンス内英語は除外されるべきだが検出された: %+v", violations)
		}
	})

	t.Run("templates fixture: コメント / block scalar 内に英語散文行が混入", func(t *testing.T) {
		fixture := "# This template is for security review of the target repo.\n" + // L1: 違反 (英語コメント)
			"name: security-review\n" +
			"description: |\n" +
			"  対象 repo のセキュリティ観点。\n" +
			"  Reviews authentication boundaries against common vulnerability patterns.\n" + // L5: 違反 (英語散文)
			"provider: claude\n"
		violations := scanFileForEnglishOnly("security-review.yaml", fixture, fileKindYAML)
		if len(violations) != 2 {
			t.Fatalf("templates fixture で 2 件検出を期待したが %d 件: %+v", len(violations), violations)
		}
		// 行番号で sort されている前提
		gotLines := []int{violations[0].lineNumber, violations[1].lineNumber}
		wantLines := []int{1, 5}
		for i := range gotLines {
			if gotLines[i] != wantLines[i] {
				t.Errorf("違反行番号 #%d: got=%d, want=%d (全違反: %+v)", i, gotLines[i], wantLines[i], violations)
			}
		}
	})

	t.Run("templates fixture: backtick 識別子だけのコメント行は除外", func(t *testing.T) {
		fixture := "# `templates/security-review.yaml` 経由で Phase 2 から読み込まれる。\n" + // 日本語があるので除外
			"# `config/`\n" + // 識別子だけ (英単語 1 つ) なので除外
			"name: security-review\n"
		violations := scanFileForEnglishOnly("security-review.yaml", fixture, fileKindYAML)
		if len(violations) != 0 {
			t.Errorf("backtick 識別子のみの行は除外されるべきだが検出された: %+v", violations)
		}
	})
}

// TestExtractFrontmatterDescription_DetectsDrift は extractFrontmatterDescription が
// 合成 fixture から description 行を抽出 / 抽出失敗を正しく判定することを単体テストする。
func TestExtractFrontmatterDescription_DetectsDrift(t *testing.T) {
	t.Run("正常 fixture から description 行を抽出", func(t *testing.T) {
		fixture := "---\n" +
			"name: review-rules-skill\n" +
			"description: \"Bootstrap review rules.\"\n" +
			"---\n" +
			"\n" +
			"本文\n"
		got, ok := extractFrontmatterDescription(fixture)
		if !ok {
			t.Fatalf("description 行の抽出に失敗")
		}
		if !strings.HasPrefix(got, "description:") {
			t.Errorf("抽出行が description で始まっていない: %q", got)
		}
	})

	t.Run("frontmatter 不在 fixture では抽出失敗", func(t *testing.T) {
		fixture := "# review-rules-skill\n本文\n"
		_, ok := extractFrontmatterDescription(fixture)
		if ok {
			t.Errorf("frontmatter 不在でも抽出が成功した")
		}
	})

	t.Run("frontmatter 内に description 行が無い fixture では抽出失敗", func(t *testing.T) {
		fixture := "---\nname: x\n---\n本文\n"
		_, ok := extractFrontmatterDescription(fixture)
		if ok {
			t.Errorf("description 行不在でも抽出が成功した")
		}
	})
}

// TestIsASCIIPrintableOnly_DetectsDrift は ASCII printable 検査ヘルパが境界条件を
// 正しく判定することを単体テストする。
func TestIsASCIIPrintableOnly_DetectsDrift(t *testing.T) {
	cases := []struct {
		desc string
		s    string
		want bool
	}{
		{desc: "全 ASCII printable", s: "Hello, World!", want: true},
		{desc: "space 含む", s: "  ", want: true},
		{desc: "tab 含む (control character → 不許可)", s: "a\tb", want: false},
		{desc: "newline 含む (control character → 不許可)", s: "a\nb", want: false},
		{desc: "日本語混入", s: "Hello 世界", want: false},
		{desc: "全角スペース混入", s: "Hello　World", want: false},
		{desc: "DEL (0x7F) 混入 (printable 範囲外)", s: "abc\x7Fdef", want: false},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got := isASCIIPrintableOnly(c.s)
			if got != c.want {
				t.Errorf("isASCIIPrintableOnly(%q) = %v, want %v", c.s, got, c.want)
			}
		})
	}
}
