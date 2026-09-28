// Package learnings — wiki_section.go
//
// 本 file は agent-learnings の wiki helper 層のうち、line-based parser /
// category header template / section-aware appender の primitives を提供する。
//
// 担当範囲:
//   - 構造体 Section / ParsedCategory / CategoryHeaderTemplate の定義
//   - ParseCategoryFile: category file 本文 → ParsedCategory への分解
//   - ExtractBullets: dedup pool 構築用の全 bullet 平坦化
//   - CategoryHeaderTemplateFor: category 別の新規 file template 検索
//   - AppendBullet: section-aware insertion (first-write-wins for applies_to / summary)
//
// 標準ライブラリのみで完結する line-based 実装 (markdown parser 依存禁止)。
//
// 関連 Requirement: 1.3 (category file の `# <category>` heading + 1-3 行概要) /
// 1.4 (`## <topic-slug>` heading で section を区切る) / 1.6 (applies_to 行) /
// 3.1-3.7 (section-aware append) / 5.1 (dedup pool は全 section の全 bullet) /
// 5.2 (heading / 適用条件行を bullet から除外) / 5.5 (bullet 行頭 `- ` を除いた本文のみ比較)。
package learnings

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Section は category file 内の 1 つの `## <topic-slug>` 単位を表す。
//
// design.md L271-276 の data model と、本 design で追加した summary 行 (L376) に対応する。
// 順序保持の不変式は ParsedCategory.Sections / Bullets 両方で守る。
type Section struct {
	// Topic は `## ` の slug 部分 (kebab-case)。
	Topic string
	// AppliesTo は `**適用条件:**` 行の本文 (`**適用条件:** ` prefix を除いた残り)。
	// 適用条件行が無い malformed section では空文字。
	AppliesTo string
	// Summary は `**summary:**` 行の本文 (`**summary:** ` prefix を除いた残り)。
	// summary 行が無い malformed section では空文字。
	Summary string
	// Bullets は section 内の `- ` で始まる行から先頭の `- ` を除いた本文の列。
	// 出現順を保持する。
	Bullets []string
}

// ParsedCategory は category file 1 つを section 列に分解した結果。
//
// design.md L278-282 の data model に対応。
type ParsedCategory struct {
	// Header は category file 先頭の `# <category>` heading + 概要 (改行込み、
	// 最初の `## ` heading が現れる直前まで)。section が 1 つも無い file では
	// 全文が Header に入る。
	Header string
	// Sections は `## <topic>` heading で区切られた section 列 (file 出現順)。
	Sections []Section
}

// CategoryHeaderTemplate は category file が存在しないときに作成する初期内容のテンプレート。
//
// design.md L323-328 の data model に対応。
type CategoryHeaderTemplate struct {
	// Heading は `# <category-name>` 1 行。
	Heading string
	// Intro は category の目的を 1-3 行で説明する本文。改行を含み、末尾改行は含まない。
	Intro string
}

// categoryHeaderTemplates は 4 値 closed enum 毎の初期 template を保持する。
//
// 各 Intro は旧 .claude/rules/playbook/README.md (L41-44) と design.md L633-654 の
// index example の category 説明と整合する 1 行短文。Apply phase が file 新規作成時に
// `Heading + "\n\n" + Intro + "\n"` 形式で書き込む想定 (実際の組み立ては Appender 側)。
//
// 値は immutable として扱う。Go では map literal を真に const にできないため var で宣言し、
// 書き込みは行わない (本 file 内でも書き込み無し)。
var categoryHeaderTemplates = map[string]CategoryHeaderTemplate{
	CategoryFailureRecovery: {
		Heading: "# " + CategoryFailureRecovery,
		Intro:   "失敗 → 復旧の手順。同じ失敗状況に再度ぶつかったとき、即座に正しい復旧経路へ復帰するための知見を記録する。",
	},
	CategorySuccessProcedure: {
		Heading: "# " + CategorySuccessProcedure,
		Intro:   "一発で通った成功手順。再現性のある手順として固定化することで、次回以降も同じ品質を担保する。",
	},
	CategoryAntipattern: {
		Heading: "# " + CategoryAntipattern,
		Intro:   "二度とやってはならない手筋。同じ失敗の構造を再生産しないために、結果ではなく構造を記述する。",
	},
	CategoryToolPattern: {
		Heading: "# " + CategoryToolPattern,
		Intro:   "特定ツールの効率的な使い方。tool / API のクセや組み合わせなど、知っていれば短時間で正解に辿り着けるパターンを集める。",
	},
}

// CategoryHeaderTemplateFor は category 名に対応する初期 file template を返す。
//
// closed enum 4 値以外の name に対しては `(zero, false)` を返す。
// Apply phase の新規 file 作成 path で enum 検証と template lookup を同じ呼び出しで行えるよう、
// `ok` 返し形式にする。
func CategoryHeaderTemplateFor(name string) (CategoryHeaderTemplate, bool) {
	t, ok := categoryHeaderTemplates[name]
	return t, ok
}

// errNilCategoryBody は ParseCategoryFile に nil body が渡されたときの sentinel error。
// 呼び出し側 (Apply phase) が原因型を判別しやすくするため文字列 "nil" を含める。
var errNilCategoryBody = errors.New("ParseCategoryFile: body is nil")

// 行先頭判定 prefix。bufio.Scanner で 1 行ずつ読みながら strings.HasPrefix で分類する。
// markdown parser を入れない方針 (design.md L52) なので line-prefix match のみで分類する。
const (
	headingPrefixCategory = "# "           // 先頭 `# ` (= category heading)
	headingPrefixSection  = "## "          // 先頭 `## ` (= topic section heading)
	appliesToPrefix       = "**適用条件:**"    // 適用条件行 marker (本文側に `:` 直後の半角空白は任意)
	summaryPrefix         = "**summary:**" // summary 行 marker
	bulletPrefix          = "- "           // bullet 行 marker
)

// ParseCategoryFile は category file 本文を ParsedCategory に分解する。
//
//   - body == nil: error を返す (Preconditions 違反)
//   - body == "" : Header="" / Sections=空 で error nil
//   - 最初の `## ` 行までを Header にコピーする
//   - `## ` 行で新 section を開き、次の `## ` または EOF まで集める
//   - section 内の `**適用条件:** ...` / `**summary:** ...` を AppliesTo / Summary に格納
//   - section 内の `- ...` 行を Bullets に積む (`- ` prefix を除いた本文)
//   - 適用条件 / summary 行が無い malformed section も error にせず空文字のまま section を返す
//
// 入力 body slice の中身は読み取り専用で、scanner を通すだけで破壊的変更しない。
// 戻り値の Bullets は slice copy せず作るが、入力 body とは独立した string なので
// 上書きしても body には波及しない (Go string は immutable)。
func ParseCategoryFile(body []byte) (ParsedCategory, error) {
	if body == nil {
		return ParsedCategory{}, errNilCategoryBody
	}

	// 空 body は Header 空 / Sections 空で early return。
	if len(body) == 0 {
		return ParsedCategory{}, nil
	}

	scanner := bufio.NewScanner(bytes.NewReader(body))
	// 既存 playbook entry には長文 bullet も含まれうるため、scanner buffer 上限を拡張する。
	// 1MB 1 行を許容するのは過剰だが、デフォルト 64KB だと長文 bullet で SCAN ERROR の risk があるため。
	const maxLine = 1 << 20
	scanner.Buffer(make([]byte, 0, 64*1024), maxLine)

	var (
		headerBuilder strings.Builder
		sections      []Section
		current       *Section // 現在処理中の section (nil = まだ section に入っていない / header フェーズ)
	)

	flushSection := func() {
		if current == nil {
			return
		}
		// Bullets が一度も追加されていなくても、適用条件 / summary だけ持つ section は valid。
		// (新規 section 作成直後 + Apply 失敗で bullet 未書込みの中間状態など)
		sections = append(sections, *current)
		current = nil
	}

	for scanner.Scan() {
		line := scanner.Text()

		// section heading: 新 section 開始
		if strings.HasPrefix(line, headingPrefixSection) {
			flushSection()
			topic := strings.TrimSpace(strings.TrimPrefix(line, headingPrefixSection))
			current = &Section{Topic: topic}
			continue
		}

		// section に入っていない (header フェーズ): Header にそのまま append
		if current == nil {
			headerBuilder.WriteString(line)
			headerBuilder.WriteByte('\n')
			continue
		}

		// section 内の meta 行 / bullet 行を仕分け
		switch {
		case strings.HasPrefix(line, appliesToPrefix):
			// 初出のみ採用 (重複出現は中間状態として最初の値を保持)
			if current.AppliesTo == "" {
				current.AppliesTo = strings.TrimSpace(strings.TrimPrefix(line, appliesToPrefix))
			}
		case strings.HasPrefix(line, summaryPrefix):
			if current.Summary == "" {
				current.Summary = strings.TrimSpace(strings.TrimPrefix(line, summaryPrefix))
			}
		case strings.HasPrefix(line, bulletPrefix):
			current.Bullets = append(current.Bullets, strings.TrimPrefix(line, bulletPrefix))
		default:
			// 空行 / その他のテキストは無視。dedup 比較対象にも index 出力にも入れない。
		}
	}

	// scanner エラーは入力長 buffer overflow 以外発生しない想定だが、念のため伝搬。
	if err := scanner.Err(); err != nil {
		return ParsedCategory{}, err
	}

	flushSection()

	return ParsedCategory{
		Header:   headerBuilder.String(),
		Sections: sections,
	}, nil
}

// ExtractBullets は ParsedCategory の全 section の Bullets を出現順で平坦化して返す。
//
// dedup pool 構築用 (Requirement 5.1)。heading 行 / `**適用条件:**` 行 / `**summary:**` 行 /
// 空行は ParseCategoryFile の段階で Bullets に含めない設計のため、本関数では追加 filter は不要。
//
// 戻り値は常に non-nil。section ゼロの ParsedCategory に対しても `[]string{}` を返し、
// 呼び出し側の nil チェックを省略可能にする。
func ExtractBullets(p ParsedCategory) []string {
	out := make([]string, 0)
	for _, s := range p.Sections {
		out = append(out, s.Bullets...)
	}
	return out
}

// AppendBulletInput は AppendBullet 呼び出しの引数。
//
// design.md L330-338 / Req 3.1-3.7 に対応。CategoryFilePath は呼び出し側で
// CategoryPath helper と root path を組み合わせて構築する想定。
type AppendBulletInput struct {
	// CategoryFilePath は対象 category file の絶対 or 相対 path。
	CategoryFilePath string
	// Category は 4 値 closed enum の category 名 (file 新規作成時の header template 選択)。
	Category string
	// Topic は kebab-case の section slug (`## <Topic>` heading に書かれる)。
	Topic string
	// AppliesTo は新規 section 作成時のみ書き込まれる `**適用条件:**` 行の本文。
	// 既存 section へ append する場合は無視される (first-write-wins)。
	AppliesTo string
	// Summary は新規 section 作成時のみ書き込まれる `**summary:**` 行の本文。
	// 既存 section へ append する場合は無視される (first-write-wins)。
	Summary string
	// Entry は新規追加する bullet 本文 (行頭 `- ` は付けない)。
	Entry string
}

// AppendBullet は category file に対し section-aware insertion を行う。
//
// 動作:
//   - in.Category が 4 値 enum に無い → error (`invalid category`)
//   - in.Topic が kebab-case 規約違反 → error (`invalid topic`)
//   - file が存在しない → CategoryHeaderTemplateFor の Heading + Intro で初期化
//   - file 内に同 topic の `## <Topic>` section が既に存在 → 当該 section 内の bullet 末尾に
//     `- <Entry>\n` を挿入する。`**適用条件:**` / `**summary:**` 行は一切変更しない (first-write-wins)
//   - 同 topic section が無ければ file 末尾に新規 section (`## <Topic>` + applies_to + summary + bullet)
//     を append する
//
// 書き込みは writeFileAtomic 経由 (perm 0o644)。戻り値 createdNewSection は
// 「新規 section を作成した (= file 新規作成 or 末尾 append)」場合に true。
// Apply phase が NewTopicsCreated 集計に使う判定材料。
//
// 関連 Req: 1.3 / 1.4 / 1.6 / 3.1-3.7。
func AppendBullet(in AppendBulletInput) (createdNewSection bool, err error) {
	if !ValidCategory(in.Category) {
		return false, fmt.Errorf("AppendBullet: invalid category %q", in.Category)
	}
	if !ValidTopicName(in.Topic) {
		return false, fmt.Errorf("AppendBullet: invalid topic %q", in.Topic)
	}

	// 既存 file の body 取得 (不在時は template から初期化)。
	body, err := os.ReadFile(in.CategoryFilePath)
	switch {
	case err == nil:
		// existing file
	case errors.Is(err, os.ErrNotExist):
		tmpl, ok := CategoryHeaderTemplateFor(in.Category)
		if !ok {
			// ValidCategory が通っているので理論上 unreachable。防御的に error 化。
			return false, fmt.Errorf("AppendBullet: no template for category %q", in.Category)
		}
		body = []byte(tmpl.Heading + "\n\n" + tmpl.Intro + "\n")
	default:
		return false, fmt.Errorf("AppendBullet: read %q: %w", in.CategoryFilePath, err)
	}

	// section の有無を line-based scan で判定し、見つかれば section 内の挿入位置 (byte offset) を返す。
	// 見つからなければ file 末尾に新規 section を append する。
	newBody, created := insertBullet(body, in.Topic, in.AppliesTo, in.Summary, in.Entry)

	if err := writeFileAtomic(in.CategoryFilePath, newBody, 0o644); err != nil {
		return false, fmt.Errorf("AppendBullet: write %q: %w", in.CategoryFilePath, err)
	}
	return created, nil
}

// insertBullet は body に対し section-aware insertion を行い、新しい body と createdNewSection を返す。
//
// アルゴリズム:
//   - body を line 単位で走査し、`## <topic>` heading を探す
//   - 見つかった場合: 当該 section の終端 (次の `## ` 行直前 or EOF) を計算し、その直前に `- <entry>\n` を挿入
//   - 見つからなかった場合: body 末尾に新規 section (空行 + `## <topic>` + applies_to + summary + 空行 + bullet) を append
//
// line-based string manipulation。既存 body の改行コード / layout を可能な限り保つために
// "section の終端位置 (= line index based)" を計算してから byte-level に再結合する。
func insertBullet(body []byte, topic, appliesTo, summary, entry string) (newBody []byte, createdNewSection bool) {
	headingLine := headingPrefixSection + topic

	// body を行単位に分解。trailing newline 1 つで終わる場合の split は最後の要素が空文字列になる挙動を持つ。
	// Strings.Split を使うと末尾の空 element が出てしまうので、`strings.SplitAfter` で改行を line に残す方針を採る。
	//
	// SplitAfter で得た各 line は末尾の `\n` を含む (最後の line のみ trailing newline が無い場合あり)。
	// 例: "a\nb\n" → ["a\n", "b\n"]
	// 例: "a\nb"   → ["a\n", "b"]
	lines := strings.SplitAfter(string(body), "\n")

	// 末尾の空文字 element ("a\n" -> Split で ["a", ""] になる方の SplitAfter は ["a\n", ""])
	// SplitAfter は実は末尾改行に対しても末尾に空 element を出す動作なので除去する。
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}

	// section 開始 line index を探す。
	sectionStart := -1
	for i, ln := range lines {
		stripped := strings.TrimRight(ln, "\n")
		if stripped == headingLine {
			sectionStart = i
			break
		}
	}

	if sectionStart < 0 {
		// 新規 section を末尾に append。
		// body が改行で終わっていない場合は 1 行追加してから section 開始。
		// section と既存 body の境界には空行 1 行を挟む (`...\n\n## ...`)。
		appended := appendNewSection(lines, topic, appliesTo, summary, entry)
		return []byte(appended), true
	}

	// section 終端 (次の `## ` line index or len(lines)) を探す。
	sectionEnd := len(lines)
	for i := sectionStart + 1; i < len(lines); i++ {
		stripped := strings.TrimRight(lines[i], "\n")
		if strings.HasPrefix(stripped, headingPrefixSection) {
			sectionEnd = i
			break
		}
	}

	// 当該 section ([sectionStart, sectionEnd)) 内に bullet 挿入位置を決める。
	// 仕様: section 内最後の bullet 行の直後に挿入。bullet が 1 つも無ければ
	//      `**summary:**` 行 (or `**適用条件:**` 行) の直後の空行を挟んでから挿入する。
	//      meta 行も無い malformed section の場合は section heading の直後に挿入する。
	insertAt := -1
	for i := sectionEnd - 1; i > sectionStart; i-- {
		stripped := strings.TrimRight(lines[i], "\n")
		// 空行は skip して上方向の bullet を探す。
		if stripped == "" {
			continue
		}
		if strings.HasPrefix(stripped, bulletPrefix) {
			insertAt = i + 1
			break
		}
	}
	if insertAt < 0 {
		// bullet が無い → meta 行 (summary / applies_to) の最終行を探し、その直後に空行付きで挿入。
		lastMeta := -1
		for i := sectionStart + 1; i < sectionEnd; i++ {
			stripped := strings.TrimRight(lines[i], "\n")
			if strings.HasPrefix(stripped, summaryPrefix) || strings.HasPrefix(stripped, appliesToPrefix) {
				lastMeta = i
			}
		}
		if lastMeta >= 0 {
			// meta 行直後に空行を確保したうえで bullet を挿入する。
			// `lastMeta` の直後 line が既に空行ならそのまま insertAt = lastMeta + 2、
			// そうでなければ空行を 1 つ追加 + insertAt = lastMeta + 2。
			if lastMeta+1 < sectionEnd && strings.TrimRight(lines[lastMeta+1], "\n") == "" {
				insertAt = lastMeta + 2
			} else {
				// 空行を挿入してから bullet 挿入位置を確定。
				lines = append(lines[:lastMeta+1], append([]string{"\n"}, lines[lastMeta+1:]...)...)
				sectionEnd++ // section 終端も 1 行下にずれる
				insertAt = lastMeta + 2
			}
		} else {
			// 完全 malformed (heading のみの section) → heading 直後に空行 + bullet を挿入。
			if sectionStart+1 < sectionEnd && strings.TrimRight(lines[sectionStart+1], "\n") == "" {
				insertAt = sectionStart + 2
			} else {
				lines = append(lines[:sectionStart+1], append([]string{"\n"}, lines[sectionStart+1:]...)...)
				sectionEnd++
				insertAt = sectionStart + 2
			}
		}
	}

	bulletLine := bulletPrefix + entry + "\n"
	lines = append(lines[:insertAt], append([]string{bulletLine}, lines[insertAt:]...)...)

	return []byte(strings.Join(lines, "")), false
}

// appendNewSection は既存 body の末尾に新規 section を append する。
//
// 挿入レイアウト:
//
//	<既存 body (trailing newline 保証)>
//	\n
//	## <topic>\n
//	**適用条件:** <appliesTo>\n
//	**summary:** <summary>\n
//	\n
//	- <entry>\n
//
// 既存 body の末尾が改行で終わっていない場合は 1 つ追加してから section を続ける。
// body が空 (= "") の場合は空行を挟まずに section を書く (file 全体の先頭が空行から始まらないよう)。
func appendNewSection(lines []string, topic, appliesTo, summary, entry string) string {
	var b strings.Builder
	for _, ln := range lines {
		b.WriteString(ln)
	}
	current := b.String()

	// body が空でない場合は末尾を改行 + 空行で締めてから section を続ける。
	if current != "" {
		// 末尾を `\n\n` に揃える: 既存末尾が `\n` で終わっていれば空行 1 行追加 (`\n`)、
		// `\n\n` で既に終わっていれば追加なし、改行で終わっていなければ `\n\n` を追加。
		if !strings.HasSuffix(current, "\n") {
			b.WriteString("\n\n")
		} else if !strings.HasSuffix(current, "\n\n") {
			b.WriteString("\n")
		}
	}

	b.WriteString(headingPrefixSection)
	b.WriteString(topic)
	b.WriteString("\n")
	b.WriteString(appliesToPrefix)
	b.WriteString(" ")
	b.WriteString(appliesTo)
	b.WriteString("\n")
	b.WriteString(summaryPrefix)
	b.WriteString(" ")
	b.WriteString(summary)
	b.WriteString("\n")
	b.WriteString("\n")
	b.WriteString(bulletPrefix)
	b.WriteString(entry)
	b.WriteString("\n")

	return b.String()
}
