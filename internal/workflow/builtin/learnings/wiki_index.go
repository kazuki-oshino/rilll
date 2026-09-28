// Package learnings — wiki_index.go
//
// 本 file は agent-learnings の wiki helper 層のうち、`docs_dev/playbook/index.md`
// を 4 category file から full rebuild する `RebuildIndex` 実装を提供する。
//
// 担当範囲:
//   - 構造体 RebuildIndexInput / IndexWarning の export
//   - RebuildIndex: 4 category file を順に ParseCategoryFile で読み、index.md を rewrite
//   - per-category fault tolerance: 1 category の read/parse 失敗で全体を fail させず
//     当該 section に `_(unreadable, skipped)_` プレースホルダ + IndexWarning として
//     呼び出し側に返す
//
// 標準ライブラリのみで完結する。markdown parser には依存しない。
//
// 関連 Requirement: 4.1 (Apply phase 後の index 更新) / 4.2 (4 category 固定順序) /
// 4.3 (各 category section の 1-2 行 intro) / 4.4 (topic 一覧の link 行 format) /
// 4.5 (削除済 topic 行の除外 = 単に再生成で達成) / 4.6 (frontmatter 無し) /
// 4.7 (冒頭 navigation 説明)。design.md L353-413 (WikiIndexBuilder) も参照。
package learnings

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RebuildIndexInput は RebuildIndex 呼び出しの引数。
//
// design.md L394-396 に対応。PlaybookRoot は `docs_dev/playbook/` の絶対 path
// (or 呼び出し側で組み立てた path) を想定する。本関数自身は path 種別を区別しない。
type RebuildIndexInput struct {
	// PlaybookRoot は category file 群 (`<category>.md`) と index.md (`index.md`) を
	// 含む directory の path。directory 自体が存在しない場合は error。
	PlaybookRoot string
}

// IndexWarning は per-category fault tolerance で生じた警告 1 件分。
//
// design.md L398-404 に対応。Apply phase が ApplyResult.IndexWarnings に転写し、
// 後段 analytics で観測可能にすることを想定する。
type IndexWarning struct {
	// Category は影響を受けた category enum 値 (4 値 closed enum のいずれか)。
	Category string
	// Reason は短い識別子。"parse-failed" / "read-failed" のいずれかを使う。
	Reason string
	// Detail は debug 用の error 文字列化。空でない error.Error() をそのまま入れる。
	Detail string
}

// IndexWarning.Reason の closed set。文字列の typo で warning 経路が壊れないよう定数化する。
const (
	indexWarningParseFailed = "parse-failed"
	indexWarningReadFailed  = "read-failed"
)

// indexCategoryOrder は index.md に書き出す category section の固定順序。
//
// Req 4.2 の要求: `failure-recovery` → `antipattern` → `success-procedure` → `tool-pattern`。
// 順序を変更したら index.md の出力 layout が変わるため破壊的変更扱い。
var indexCategoryOrder = []string{
	CategoryFailureRecovery,
	CategoryAntipattern,
	CategorySuccessProcedure,
	CategoryToolPattern,
}

// indexHeader は index.md 先頭の title + navigation 説明 (2-3 行)。
//
// Req 4.7: 「ファイル編集前にこの index を読み、該当 entry の `**適用条件:**` を参照する」旨を
// 2-3 行で記述する。design.md L633-654 の example と整合させる。
const indexHeader = `# Playbook Index

ファイル編集前にこの index を読み、該当 entry の ` + "`**適用条件:**`" + ` を確認する。
追記は agent-learnings ワークフローが自動で行う。手動編集は Curator dedup を回避するため非推奨。
`

// indexUnreadablePlaceholder は per-category fault tolerance で書き出すプレースホルダ行。
// design.md L367 の `_(unreadable, skipped)_`。
const indexUnreadablePlaceholder = "_(unreadable, skipped)_"

// RebuildIndex は 4 category file を再 parse し index.md を rewrite する。
//
// 動作:
//   - PlaybookRoot 直下を category 順に走査
//   - 各 category file (`<category>.md`) を `os.ReadFile` → `ParseCategoryFile`
//   - file 不在 (os.IsNotExist) は warning 対象外。section 無し空 category として扱う
//   - read 失敗 (permission 等) → IndexWarning(read-failed) + プレースホルダ書き出し
//   - parse 失敗 → IndexWarning(parse-failed) + プレースホルダ書き出し
//   - 正常 category は各 section を topic 一覧 (`- [topic](./category.md#topic) — summary`) として書き出す
//   - 集約した body を `<PlaybookRoot>/index.md` に writeFileAtomic で書く
//
// 戻り値:
//   - warnings: per-category の read/parse 失敗で発生した警告列 (致命扱いしない)
//   - error: index.md 自体の write 失敗、または PlaybookRoot 不在など本関数が継続不能な障害のみ
//
// design.md L406-413 と Req 4.1-4.7 のカバレッジ。
func RebuildIndex(in RebuildIndexInput) (warnings []IndexWarning, err error) {
	var b strings.Builder
	b.WriteString(indexHeader)

	for _, cat := range indexCategoryOrder {
		b.WriteString("\n")
		b.WriteString("## ")
		b.WriteString(cat)
		b.WriteString("\n")
		// category intro 1-2 行 (Req 4.3)
		tmpl, ok := CategoryHeaderTemplateFor(cat)
		if ok {
			intro := strings.TrimRight(tmpl.Intro, "\n")
			if intro != "" {
				b.WriteString(intro)
				b.WriteString("\n")
			}
		}
		b.WriteString("\n")

		path := filepath.Join(in.PlaybookRoot, cat+".md")
		body, readErr := os.ReadFile(path)
		switch {
		case readErr == nil:
			// continue to parse
		case errors.Is(readErr, os.ErrNotExist):
			// 不在は warning 対象外。section heading + intro のみで終わらせる。
			continue
		default:
			// permission denied 等。プレースホルダ + warning に格納。
			warnings = append(warnings, IndexWarning{
				Category: cat,
				Reason:   indexWarningReadFailed,
				Detail:   readErr.Error(),
			})
			b.WriteString(indexUnreadablePlaceholder)
			b.WriteString("\n")
			continue
		}

		parsed, parseErr := ParseCategoryFile(body)
		if parseErr != nil {
			warnings = append(warnings, IndexWarning{
				Category: cat,
				Reason:   indexWarningParseFailed,
				Detail:   parseErr.Error(),
			})
			b.WriteString(indexUnreadablePlaceholder)
			b.WriteString("\n")
			continue
		}

		// 各 section を topic 一覧 (`- [topic](./category.md#topic) — summary`) として書き出す
		// (Req 4.4)。
		for _, s := range parsed.Sections {
			b.WriteString(formatTopicLine(cat, s))
			b.WriteString("\n")
		}
	}

	// 末尾改行を 1 つに揃える: indexHeader / section block の組み合わせで末尾は既に `\n` 終わり。
	// (Builder の最終 WriteString が `\n` で終わる limb を保つことで保証している。)

	indexPath := filepath.Join(in.PlaybookRoot, "index.md")
	if err := writeFileAtomic(indexPath, []byte(b.String()), 0o644); err != nil {
		return warnings, fmt.Errorf("RebuildIndex: write %q: %w", indexPath, err)
	}
	return warnings, nil
}

// formatTopicLine は 1 section から index.md の topic 行 1 行分を組み立てる。
//
// 例: `- [edit-tool](./failure-recovery.md#edit-tool) — Edit failure ノウハウ`
// summary が空文字でも ` — ` separator は保持する (Req 4.4 の format 固定)。
func formatTopicLine(category string, s Section) string {
	return "- [" + s.Topic + "](./" + category + ".md#" + s.Topic + ") — " + s.Summary
}
