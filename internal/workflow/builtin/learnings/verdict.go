package learnings

// 本 file は agent-learnings workflow の最終結果を表す Verdict 文字列定数 9 値と、
// `<runDir>/apply-result.json` の root struct となる ApplyResult を定義する。
//
// Verdict 値は CLI / TUI / 後続 spec が verdict 文字列を直接参照する契約面であり、
// 値の変更は破壊的 API 変更に等しい。要件 8.5 が定める closed set 9 値を逐語的に保つ。
//
// ApplyResult は task 7.4 (learnings.Apply) が artifact として書き出す struct で、
// 本 task 6.2 では struct skeleton と field 名 / 型を確定する。要素型 (AppendedEntry /
// RejectedOperation / DedupSkippedOperation) は task 7.4 で field を追加する余地を残し、
// 本 file では struct 宣言のみ行う (additive 拡張ポイント)。
//
// 関連 Requirement: 8.5 (verdict closed set + apply-result.json artifact)。

// Verdict 文字列定数 (Requirement 8.5 / design.md §State Management §Verdict closed set)。
//
// 9 値 closed set:
//
//   - VerdictApplied                    : 1 件以上の operation が playbook に追記された
//   - VerdictAppliedNoOp                : 0 件 accept (agent_calls 0 件 / ErrNoCredentials /
//     curator 出力 0 件 等)。playbook 改変なし
//   - VerdictRejectedInvalidJSON        : curator LLM 応答の JSON parse 失敗
//   - VerdictRejectedEmptyPathsGlob     : operation の paths_glob が空
//   - VerdictRejectedUnboundedPathsGlob : paths_glob が `**` / `**/*` / `*` の単独 unbounded pattern
//   - VerdictRejectedInvalidCategory    : operation の category が closed enum 4 値に不一致
//   - VerdictRejectedInvalidTopicName   : operation の topic 名が `^[a-z][a-z0-9-]*$` に違反
//   - VerdictReaderUnavailable          : akashic Reader nil / 接続 error
//   - VerdictResumeAllFailed            : 全 session resume call が失敗
//
// 値そのもの (kebab-case リテラル) は requirements.md / design.md と完全一致させる。
const (
	VerdictApplied                    = "applied"
	VerdictAppliedNoOp                = "applied-no-op"
	VerdictRejectedInvalidJSON        = "rejected-invalid-json"
	VerdictRejectedEmptyPathsGlob     = "rejected-empty-paths-glob"
	VerdictRejectedUnboundedPathsGlob = "rejected-unbounded-paths-glob"
	VerdictRejectedInvalidCategory    = "rejected-invalid-category"
	VerdictRejectedInvalidTopicName   = "rejected-invalid-topic-name"
	VerdictReaderUnavailable          = "reader-unavailable"
	VerdictResumeAllFailed            = "resume-all-failed"
)

// allVerdicts は 9 値 closed set の検索集合。IsValidVerdict が O(1) で参照する。
// 順序は要件には現れないが、closed set の確認しやすさのため design.md 記載順を保つ。
var allVerdicts = map[string]struct{}{
	VerdictApplied:                    {},
	VerdictAppliedNoOp:                {},
	VerdictRejectedInvalidJSON:        {},
	VerdictRejectedEmptyPathsGlob:     {},
	VerdictRejectedUnboundedPathsGlob: {},
	VerdictRejectedInvalidCategory:    {},
	VerdictRejectedInvalidTopicName:   {},
	VerdictReaderUnavailable:          {},
	VerdictResumeAllFailed:            {},
}

// IsValidVerdict は s が 9 値 closed set のいずれかに完全一致するときのみ true を返す。
// 大文字小文字違い / hyphen 違い / underscore 違い / 前後空白付き / v1 distiller 由来の
// 旧 verdict (`prefetch-failed` 等) はすべて false。
//
// Requirement 8.5 (verdict closed set 9 値以外を reject) で参照される primary 判定関数。
// task 7.4 (learnings.Apply) が ApplyResult.Verdict を artifact 化する直前に本関数で
// defense-in-depth 検証する想定。
func IsValidVerdict(s string) bool {
	_, ok := allVerdicts[s]
	return ok
}

// AppendedEntry / RejectedOperation / DedupSkippedOperation は task 7.4 で apply.go に
// 具現化された (本 file の placeholder は削除済み)。3 type は同 package 内で apply.go に
// 集約され、本 file の ApplyResult.{Appended, Rejected, Deduped} field がそれを参照する。

// ApplyResult は agent-learnings apply phase の最終結果を保持し、`<runDir>/apply-result.json`
// に書き出される (Requirement 8.5)。
//
// Field 8 値:
//
//   - Verdict          : 9 値 closed set のいずれか (IsValidVerdict が判定)
//   - Appended         : 実際に追記された entry 集合
//   - Rejected         : validation で reject された operation 集合
//   - Deduped          : jaccard dedup で skip された operation 集合
//   - NewTopicsCreated : 新規 scaffold された topic 名 (`<category>/<topic>` 相対 path) の集合
//   - StartedAt        : apply phase 開始時刻 (RFC3339 UTC)
//   - FinishedAt       : apply phase 終了時刻 (RFC3339 UTC)
//   - IndexWarnings    : RebuildIndex の per-category fault tolerance による警告列 (空 / nil 許容)
//
// task 7.4 (learnings.Apply) が本 struct を埋めて artifact 化する。要素型 3 種は本 file で
// placeholder として宣言済みであり、task 7.4 が field を additive に追加して具現化する。
// agent-learnings-wiki spec (Task 3.3) で IndexWarnings を additive 追加した。
type ApplyResult struct {
	Verdict          string
	Appended         []AppendedEntry
	Rejected         []RejectedOperation
	Deduped          []DedupSkippedOperation
	NewTopicsCreated []string
	StartedAt        string
	FinishedAt       string
	IndexWarnings    []IndexWarning `json:"index_warnings,omitempty"`
}
