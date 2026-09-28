// Package learnings は agent-learnings tail workflow を構成する 4 phase pipeline
// (pre-fetch / reflector / curator / apply) の中核 package である。
//
// 本 file は Apply phase / Recorder validation / Curator 出力 schema が共通参照する
// プリミティブのみを置く:
//
//   - 4 値 closed enum (`failure-recovery` / `success-procedure` / `antipattern` /
//     `tool-pattern`) の定数定義と ValidCategory による所属判定
//   - Topic 名の文法検証 ValidTopicName (regex `^[a-z][a-z0-9-]*$`)
//   - playbook root からの相対 path を組み立てる CategoryPath
//
// 関連 Requirement: 5.3 (category closed set) / 7.1 (playbook layout) /
// 7.6 (topic name validation)。
package learnings

import "regexp"

// 4 値 closed enum。design.md と requirements.md が定める唯一の category 集合であり、
// akashic learnings.category 列 / curator JSON Schema enum / playbook ディレクトリ名と
// 1:1 で対応する。値の変更は破壊的 schema 変更に等しいため避ける。
const (
	// CategoryFailureRecovery は失敗から回復した手順 / 修復ノウハウを表す。
	CategoryFailureRecovery = "failure-recovery"
	// CategorySuccessProcedure は再現性のある成功手順 (procedural memory) を表す。
	CategorySuccessProcedure = "success-procedure"
	// CategoryAntipattern は再現性のある失敗パターン / 避けるべき選択を表す。
	CategoryAntipattern = "antipattern"
	// CategoryToolPattern は特定 tool / API の効果的な使い方を表す。
	CategoryToolPattern = "tool-pattern"
)

// allCategories は 4 値 closed enum の検索集合。ValidCategory が O(1) で参照する。
// 順序は要件には現れないが、closed set の確認のしやすさのため design.md 記載順を保つ。
var allCategories = map[string]struct{}{
	CategoryFailureRecovery:  {},
	CategorySuccessProcedure: {},
	CategoryAntipattern:      {},
	CategoryToolPattern:      {},
}

// topicNameRe は topic 名の文法規則 (kebab-case) を表す regex。
// Requirement 7.6 が指定する `^[a-z][a-z0-9-]*$` を素直に表現する。
//
//   - 先頭は小文字英字 1 文字
//   - 2 文字目以降は小文字英字 / 数字 / hyphen のみ
//   - 空文字 / 大文字 / underscore / Japanese / 数字始まり / hyphen 始まりはすべて reject
var topicNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// ValidCategory は s が 4 値 closed enum のいずれかに完全一致するときのみ true を返す。
// 大文字小文字違い / hyphen 違い (`anti-pattern`) / 前後空白付きはすべて false。
//
// Requirement 5.3 (category closed set + 不一致 reject) と Requirement 6.4
// (curator operation の category 不一致 reject) で参照される primary 判定関数。
func ValidCategory(s string) bool {
	_, ok := allCategories[s]
	return ok
}

// ValidTopicName は s が `^[a-z][a-z0-9-]*$` に合致する topic 名であるときのみ true を返す。
//
// Requirement 7.6 で要求される topic 名検証。Apply phase は本関数で false を返す
// operation を `rejected-invalid-topic-name` として reject する。
func ValidTopicName(s string) bool {
	if s == "" {
		return false
	}
	return topicNameRe.MatchString(s)
}

// CategoryPath は playbook 旧 category subdirectory layout における category / topic
// 結合表記 `<category>/<topic>.md` を返す。OS 依存セパレータではなく forward slash を
// 使う (design.md の glob 表記と整合)。
//
// 注: agent-learnings-wiki spec で playbook の物理 layout は
// `docs_dev/playbook/<category>.md` の wiki 単一 file + `## <topic>` section に
// 移行済。本関数は legacy file 単位の表記を返す純関数として残るが、Apply phase は
// 直接利用しない (wiki layout では `<category>.md` のみで file が一意に決まる)。
// 文字列結合のみで副作用を持たない。
func CategoryPath(category, topic string) string {
	return category + "/" + topic + ".md"
}
