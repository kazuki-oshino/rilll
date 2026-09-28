// Package result は review-workflow の per-rule 結果 (`<rule-name>.json`) と
// 全体サマリ (`result.json`) の正規型ファミリを提供する。
//
// 公開契約 (両端契約):
//
//   - 本パッケージで export される `Verdict` / `Severity` / `Finding` / `RuleResult` /
//     `Summary` は、review-workflow と review-apply-workflow が同 module から
//     直接 import して consume する schema である (design.md §result.types,
//     Requirements 6.2 / 6.3 / 6.6)。
//   - JSON tag は snake_case 固定。フィールド名・型・JSON tag の rename や
//     既存フィールドの意味変更は破壊的変更扱いで、両 spec の同時改訂を要する
//     (Requirements 6.6 / Revalidation Triggers)。
//
// 後方互換ガード:
//
//   - 新規フィールド追加は許可 (zero-value で旧 reader が壊れないこと)。
//   - フィールド削除 / 既存フィールドの意味変更は禁止。
//   - 旧 reader を壊さないため、本 schema を decode する側は `json.Decoder.DisallowUnknownFields()`
//     のような strict decode を採用してはならない (encoding/json デフォルト挙動を維持)。
//   - Verdict / Severity の文字列 enum 値は固定。値追加は OK だが、値の rename / 意味変更は禁止。
//
// `OverallVerdict` 関数は per-rule verdict のスライスから全体 verdict を導出する集約ヘルパで、
// 優先順位 `precheck_failed` > `cancelled` > `findings_reported` > `failed` > `no_findings` を実装する。
package result

// Verdict は per-rule および全体の終端 verdict を表す文字列 enum。
//
// 値は両端契約として固定されており、追加は可能だが既存値の rename / 意味変更は禁止される
// (Requirements 6.6)。
type Verdict string

const (
	// VerdictUnset は未確定 verdict (defer 経路の default)。
	// 永続化された JSON 上で空文字となるため、この値を最終 verdict として書き出してはならない。
	// orchestration 側 (workflow.go) が defer で必ず確定値に置き換える契約。
	VerdictUnset Verdict = ""

	// VerdictPrecheckFailed は precheck フェーズが停止した verdict (Req 3.4)。
	// per-rule artifact は一切書き出されず、全体 result.json に summary と warnings のみが残る。
	VerdictPrecheckFailed Verdict = "precheck_failed"

	// VerdictSkipped は applies_when 自己判定で「適用不要」と返した rule の verdict (Req 5.2)。
	// review prompt は実行されず、SkipReason に agent からの理由が格納される。
	VerdictSkipped Verdict = "skipped"

	// VerdictNoFindings は review が実行されたが指摘なしと判定された rule の verdict (Req 5.4)。
	VerdictNoFindings Verdict = "no_findings"

	// VerdictFindingsReported は review が実行され指摘ありと判定された rule の verdict (Req 5.4)。
	VerdictFindingsReported Verdict = "findings_reported"

	// VerdictFailed は agent 呼び出しエラー / 出力解釈不能 / provider 未登録 等で失敗した
	// rule の verdict (Req 4.4 / 5.6)。FailureCategory に分類詳細が格納される。
	VerdictFailed Verdict = "failed"

	// VerdictCancelled は外部中断 (ESC / SIGINT 等) で in-progress rule が打ち切られた verdict (Req 4.5)。
	// 完了済 rule の結果は破棄せず保持される。
	VerdictCancelled Verdict = "cancelled"
)

// Severity は Finding の重要度を表す closed set 文字列 enum。
//
// 値は固定。parser 側で closed set 違反を検出した場合は当該 rule を Verdict=failed に丸める契約
// (design.md §parse.go)。
type Severity string

const (
	// SeverityInfo は情報レベルの指摘 (注意喚起、提案)。
	SeverityInfo Severity = "info"

	// SeverityWarn は警告レベルの指摘 (改善推奨、要確認)。
	SeverityWarn Severity = "warn"

	// SeverityError は問題レベルの指摘 (修正必須、致命的)。
	SeverityError Severity = "error"
)

// Finding は per-rule の単一指摘を表す (design.md §result.types)。
//
// JSON tag は snake_case 固定。File は両端契約上 **必須(非空文字列)** で、
// review-apply-workflow 側 scope guard (`applied_files ⊆ findingsFiles`) の
// allowlist 算出に使われる (review-apply-workflow design.md §Patch / Req 3.5)。
// Location は file:line / file:func 形式の補助情報 (例: "auth/login.go:42") で、
// agent 出力に含まれない場合は zero-value で omitempty 省略される。
//
// File と Location の役割分担:
//   - File: scope guard の機械的判定に使う pure な相対パス (例: "auth/login.go")。
//     parser 側で必須化し、空文字列なら failed 扱い (review/parse.go validateAndConvert)。
//   - Location: 人間向けの「どこで」表現で、行番号や関数名を含む自由文。
//     File と重複してもよく、空でもよい (omitempty)。
type Finding struct {
	// Severity は指摘の重要度。closed set: info / warn / error。
	Severity Severity `json:"severity"`

	// File は指摘対象ファイルの相対パス (例: "auth/login.go")。
	// 両端契約上 **必須(非空文字列)** であり、review-apply-workflow の scope guard
	// allowlist (`LoadResult.FindingsFiles()`) を構成する基底情報となる。
	// agent 応答で空 / 欠落の場合は parser 側で failed 扱い (review/parse.go)。
	File string `json:"file"`

	// Location は指摘箇所の自由文表現 (例: "auth/login.go:42")。空でも構わない。
	// File 必須化後は補助情報の扱いとなり、行番号や関数名等を含めるための
	// 任意フィールドとして保持する (旧 reader 互換のため fields は保持)。
	Location string `json:"location,omitempty"`

	// Message は指摘内容の人間可読テキスト (改行を含んで構わない)。
	Message string `json:"message"`
}

// RuleResult は per-rule 結果 (`<cwd>/.rilll/review-workflow/<runID>/<rule-name>.json`) の
// 正規型 (Req 6.1, 6.2)。
//
// JSON tag は snake_case 固定。フィールド追加時は本 spec の改訂を伴う (Req 6.6)。
//
// omitempty 設計:
//   - SkipReason: applied=true (review 実行) のときは空。
//   - Findings: findings_present=false のとき省略。
//   - FailureCategory: verdict=failed 以外では省略。
//
// non-omitempty 設計:
//   - Applied / FindingsPresent: bool zero-value (false) 自体が意味を持つため明示出力する。
//   - Model: provider 既定モデル選択 (zero-value) と「設定漏れ」を区別しないため、
//     現契約では空文字でも明示的に書き出す (将来 omitempty 化する場合は両端契約を改訂する)。
type RuleResult struct {
	// Name は rule 識別子 (kebab-case、YAML の `name` と一致)。
	Name string `json:"name"`

	// Description は YAML の `description` をそのまま転記した人間可読説明。
	Description string `json:"description"`

	// Applied は applies_when 自己判定の結果。true=review 実行 / false=skip (Req 5.1, 5.2)。
	Applied bool `json:"applied"`

	// Verdict は本 rule の終端 verdict (Verdict enum の closed set のいずれか)。
	Verdict Verdict `json:"verdict"`

	// SkipReason は Applied=false のとき agent が返した skip 理由 (Req 5.2)。
	SkipReason string `json:"skip_reason,omitempty"`

	// FindingsPresent は review 結果に指摘が 1 件以上あるか (Req 5.4)。
	// Applied=false のときは false 固定。
	FindingsPresent bool `json:"findings_present"`

	// Summary は人間可読の要約 (改行を含まない 1 行が望ましい)。
	// Applied=false のときは skip 概要、Verdict=failed のときは失敗概要を入れる。
	Summary string `json:"summary"`

	// Findings は review prompt 経由で抽出された個別指摘の配列 (Req 5.4)。
	// FindingsPresent=false のときは省略。
	Findings []Finding `json:"findings,omitempty"`

	// Provider は本 rule の実行に利用した provider ID (例: "claude" / "cursor" / "codex") (Req 5.5)。
	Provider string `json:"provider"`

	// Model は本 rule の実行に利用した model 名。zero-value (空文字) の場合は provider 既定モデル (Req 5.5)。
	Model string `json:"model"`

	// FailureCategory は Verdict=failed のときの失敗分類 (例: "provider_unknown" /
	// "response_unparseable" / "executor_panic")。それ以外では省略 (Req 4.4 / 5.6)。
	FailureCategory string `json:"failure_category,omitempty"`

	// StartedAt は rule 実行開始時刻 (RFC3339Nano、Req 6.2)。
	StartedAt string `json:"started_at"`

	// FinishedAt は rule 実行終了時刻 (RFC3339Nano、Req 6.2)。
	FinishedAt string `json:"finished_at"`

	// SourceFile は本 rule を定義する元 YAML ファイル名 (`.rilll-config/review-rules/` 直下の basename)。
	SourceFile string `json:"source_file"`
}

// Summary は全体サマリ (`<cwd>/.rilll/review-workflow/<runID>/result.json`) の正規型 (Req 6.3)。
//
// JSON tag は snake_case 固定。フィールド追加時は本 spec の改訂を伴う (Req 6.6)。
//
// Sequential 合成 (review-and-apply) の前段としても同形 schema を出力するため、
// 後段 review-apply-workflow は本構造体を import してそのまま consume できる契約。
type Summary struct {
	// WorkflowRunID は本 review-workflow run の workflow_run_id (akashic 突合キー)。
	WorkflowRunID string `json:"workflow_run_id"`

	// PRNumber は対象 PR 番号。precheck 失敗時は 0 を許容する。
	PRNumber int `json:"pr_number"`

	// OverallVerdict は全 rule verdict を OverallVerdict() で集約した全体 verdict。
	// precheck 失敗 / 外部中断時はここに直接 VerdictPrecheckFailed / VerdictCancelled を入れる。
	OverallVerdict Verdict `json:"overall_verdict"`

	// OverallSummary は全体の人間可読サマリ (1 行が望ましい)。
	OverallSummary string `json:"overall_summary"`

	// Rules は per-rule 結果のスライス。ファイル名昇順で deterministic に並ぶ (Req 1.5)。
	Rules []RuleResult `json:"rules"`

	// PrecheckWarnings は precheck フェーズで発生した警告のサマリ (Req 6.3)。
	// 0 件のときは省略 (omitempty)。
	PrecheckWarnings []string `json:"precheck_warnings,omitempty"`

	// StartedAt はワークフロー全体の開始時刻 (RFC3339Nano)。
	StartedAt string `json:"started_at"`

	// FinishedAt はワークフロー全体の終了時刻 (RFC3339Nano)。
	FinishedAt string `json:"finished_at"`
}

// verdictPriority は OverallVerdict の集約優先順位を表す内部マップ。
// 値が大きいほど優先度が高い。スライスに含まれる verdict のうち最も高い優先度のものが
// OverallVerdict() の結果となる。
//
// 優先順位 (design.md §Logical Schema 末尾, Req 4.4 / 6.3):
//   - precheck_failed (5)  > cancelled (4) > findings_reported (3) > failed (2) > no_findings (1)
//   - skipped / unset は 0 (no_findings の default に吸収される)。
var verdictPriority = map[Verdict]int{
	VerdictPrecheckFailed:   5,
	VerdictCancelled:        4,
	VerdictFindingsReported: 3,
	VerdictFailed:           2,
	VerdictNoFindings:       1,
	VerdictSkipped:          0,
	VerdictUnset:            0,
}

// OverallVerdict は per-rule verdict のスライスから全体 verdict を導出する集約ヘルパ。
//
// 優先順位 (design.md §Logical Schema 末尾):
//
//	precheck_failed > cancelled > findings_reported > failed > no_findings
//
// skipped / unset / 未知の値は no_findings の default に吸収される。
//
// 振る舞い:
//   - 入力スライスが nil または空の場合は VerdictNoFindings を返す (安全な default)。
//   - すべて skipped / unset の場合は VerdictNoFindings を返す。
//   - 入力に含まれる verdict のうち、verdictPriority マップで最も大きい値の verdict を返す。
//
// 用途:
//   - executor が返した []RuleResult から Verdict を抜き出して本関数に渡し、
//     Summary.OverallVerdict を確定する (workflow.go orchestration から呼び出される)。
//   - precheck 失敗 / 外部中断のような workflow-level verdict は呼び出し側が
//     直接 Summary.OverallVerdict に書き込む経路もあり、本関数は集約用途に限定される。
func OverallVerdict(verdicts []Verdict) Verdict {
	best := VerdictNoFindings
	bestRank := verdictPriority[best]
	for _, v := range verdicts {
		rank, ok := verdictPriority[v]
		if !ok {
			// 未知の verdict は最低優先度 (0) として扱い、no_findings の default を維持する。
			rank = 0
		}
		if rank > bestRank {
			best = v
			bestRank = rank
		}
	}
	return best
}
