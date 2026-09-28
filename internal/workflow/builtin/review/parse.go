// Package review の本ファイルは agent (claude / cursor / codex) からの
// AgentResponse を per-rule 結果型 (parsedRule) に抽出するパーサを実装する
// (tasks.md 2.2 / design.md §review.Parser / Req 5.2, 5.4)。
//
// パース戦略 (design.md §review.Parser Implementation Notes):
//
//  1. `resp.StructuredOutput` が JSON schema (`JSONSchemaForReview` 互換) に従う
//     場合は直接デコードする経路を最優先する。claude / codex bridge が
//     OutputFormat=json_schema で構造化出力を返す経路に対応する。
//  2. StructuredOutput が nil / スキーマ違反の場合は `resp.Content` から
//     最初の `{` から最後の `}` までを greedy regex で抽出して JSON decode
//     する fallback を持つ。cursor 経路の自由文出力 (CLI が OutputFormat を
//     解釈しない) と、claude / codex が稀に schema を破った場合の救済手段。
//  3. 上記いずれも失敗した場合は `Verdict=failed` + `FailureCategory="response_unparseable"`
//     を呼び出し側 (executor) に返す。
//
// 必須フィールド検証 (design.md §review.Parser Validation):
//
//   - `applied=false` のとき `skip_reason` 必須 (空は failed)
//   - `applied=true` のとき `summary` 必須 (空は failed)
//   - `details[].severity` は `info|warn|error` の closed set のみ許容
//
// Verdict マッピング (Req 5.2 / 5.4):
//
//   - applied=false (skip 検証通過) → VerdictSkipped
//   - applied=true && findings_present=false → VerdictNoFindings
//   - applied=true && findings_present=true → VerdictFindingsReported
//   - パース / 検証失敗 → VerdictFailed + FailureCategory="response_unparseable"
//
// `parsedRule` は executor (task 2.4) が `result.RuleResult` に詰め直すための
// 中間型。executor 側で Provider/Model/StartedAt/FinishedAt 等の周辺情報を
// 補完するため、本パッケージでは parser 単独で確定できるフィールドのみを保持する。
package review

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/result"
)

// FailureCategoryResponseUnparseable は agent 応答が JSON schema にも regex 抽出にも
// 適合しない場合の失敗カテゴリ文字列 (design.md §Error Categories and Responses)。
//
// executor 側 (task 2.4) は本定数を `result.RuleResult.FailureCategory` および
// akashic `agent_calls.failure_category` に転記する。
const FailureCategoryResponseUnparseable = "response_unparseable"

// jsonObjectPattern は Content fallback 経路で agent 応答から JSON オブジェクトを
// 抽出する greedy regex (design.md §review.Parser Implementation Notes)。
//
// (?s) で `.` を改行にも match させ、最初の `{` から最後の `}` までを
// greedy にキャプチャする。fenced code block (```json ... ```) や前後の散文に
// 包まれていても、外側の applied オブジェクトを 1 ショットで取り出せる。
//
// 注意: 厳密な JSON parse ではなく「JSON 候補 substring」の抽出に過ぎない。
// 抽出した候補は `json.Unmarshal` で再検証され、不正なら失敗扱いになる。
var jsonObjectPattern = regexp.MustCompile(`(?s)\{.*\}`)

// parsedRule は agent 応答パース結果の中間型。executor (task 2.4) が
// `result.RuleResult` に詰め直す際に Provider / Model / StartedAt /
// FinishedAt / Description / Name / SourceFile を補完する前提で、本パッケージ
// では parser 単独で確定できるフィールドのみを保持する。
//
// 本型は package 内非公開 (lowercase)。executor が同 package 内から直接参照する
// ことを想定している (design.md §workflow/builtin/review domain)。
type parsedRule struct {
	// Applied は agent の applies_when 自己判定結果。
	Applied bool

	// SkipReason は Applied=false のとき agent が返した skip 理由。
	SkipReason string

	// FindingsPresent は review 結果に指摘 1 件以上があるか。Applied=false のときは false 固定。
	FindingsPresent bool

	// Summary は Applied=true のとき必須となる人間可読要約。
	// Applied=false のときも skip 概要を入れる場合がある (executor 側でフォーマット可能)。
	Summary string

	// Findings は agent 応答の details[] を `result.Finding` に変換した個別指摘配列。
	// Applied=false / FindingsPresent=false のときは nil または空。
	Findings []result.Finding

	// Verdict は parser が確定した本 rule の終端 verdict。
	//   - skipped         (applied=false 検証通過)
	//   - no_findings     (applied=true && findings_present=false)
	//   - findings_reported (applied=true && findings_present=true)
	//   - failed          (パース / 検証失敗)
	Verdict result.Verdict

	// FailureCategory は Verdict=failed のときの失敗分類。それ以外では空文字。
	// 現契約では parser 失敗は常に "response_unparseable"。
	FailureCategory string
}

// rawAgentOutput は agent 応答の JSON schema (`JSONSchemaForReview`) に対応する
// 中間 decode 型。`StructuredOutput` の direct decode と Content fallback の
// 両経路で共有する。
//
// JSON tag は schema (両端契約 design.md §review.Parser L520-534) と一致する。
// `Applied` は applied=false が省略 (omitempty) されないように明示出力する。
// `*bool` ではなく必須として扱い、未設定は schema 違反とする。
type rawAgentOutput struct {
	Applied         *bool        `json:"applied"`
	SkipReason      string       `json:"skip_reason"`
	FindingsPresent bool         `json:"findings_present"`
	Summary         string       `json:"summary"`
	Details         []rawFinding `json:"details"`
}

// rawFinding は agent 応答の details[] を decode する中間型。
//
// File は両端契約 (result.Finding) で必須化されたため本中間型でも必須扱いとし、
// 空文字 / 欠落は validateAndConvert 段で failed (response_unparseable) に集約する
// (review-apply-workflow design.md §Patch / Req 3.5 fail-closed 前提)。
type rawFinding struct {
	Severity string `json:"severity"`
	File     string `json:"file"`
	Location string `json:"location"`
	Message  string `json:"message"`
}

// ParseRuleResponse は agent からの AgentResponse を parsedRule に変換する
// (design.md §review.Parser / Req 5.2, 5.4)。
//
// 戻り値:
//   - 成功時: (parsedRule{Verdict=skipped|no_findings|findings_reported}, nil)
//   - 失敗時: (parsedRule{Verdict=failed, FailureCategory="response_unparseable"}, error)
//
// 失敗時も parsedRule は valid な値を持つ (executor が `result.RuleResult` に
// 転記して artifact / akashic に記録できる契約)。error は失敗の付加情報として
// 並行して返す (executor 側が `errors.Is` 判定や log 出力に使える)。
//
// パース順序:
//  1. resp.StructuredOutput が non-nil の場合、JSON 経由で rawAgentOutput に
//     decode を試みる (direct decode 経路)。
//  2. (1) が失敗 / StructuredOutput が nil の場合、resp.Content から greedy
//     regex で JSON オブジェクトを抽出して decode を試みる (fallback 経路)。
//  3. 両方失敗した場合、parsedRule{Verdict=failed,
//     FailureCategory="response_unparseable"} と非 nil error を返す。
//
// 検証:
//   - applied フィールドが nil (未指定) は schema 違反 → failed
//   - applied=false のとき skip_reason 空は失敗
//   - applied=true のとき summary 空は失敗
//   - details[].severity が closed set 違反は失敗
func ParseRuleResponse(resp provider.AgentResponse) (parsedRule, error) {
	// (1) StructuredOutput direct decode 経路
	if resp.StructuredOutput != nil {
		if raw, err := decodeStructuredOutput(resp.StructuredOutput); err == nil {
			if pr, verr := validateAndConvert(raw); verr == nil {
				return pr, nil
			}
			// validation 失敗時は (2) Content fallback 経路にフォールスルー。
			// agent が StructuredOutput と Content の両方を返し、StructuredOutput
			// 側だけ schema 違反のような実例 (cursor 経路) で救済を効かせる。
		}
	}

	// (2) Content fallback regex 経路
	if raw, err := extractFromContent(resp.Content); err == nil {
		if pr, verr := validateAndConvert(raw); verr == nil {
			return pr, nil
		} else {
			// JSON は抽出 + decode できたが必須フィールド or severity 違反。
			// failed として扱い、抽出元 error を伝播する。
			return failedParsed(verr), verr
		}
	}

	// (3) 両経路失敗 → failed + response_unparseable
	err := errors.New("review: agent 応答から JSON schema 適合構造を抽出できませんでした")
	return failedParsed(err), err
}

// decodeStructuredOutput は `resp.StructuredOutput` (any 型) を rawAgentOutput
// に変換する。bridge 実装によって map[string]any / 構造化型 / json.RawMessage 等
// 様々な実体型が来るため、いったん `json.Marshal` で正規化してから
// `json.Unmarshal` で型強制する 2 段階方式とする。
//
// この方式を選ぶ理由:
//   - reflection ベースの構造体マッピングを 1 関数に閉じ込められる
//   - bridge 側の細部 (any vs struct vs RawMessage) に依存しない
//   - decode 失敗時は通常の error として呼び出し側で fallback 判定できる
func decodeStructuredOutput(structured any) (rawAgentOutput, error) {
	var raw rawAgentOutput
	b, err := json.Marshal(structured)
	if err != nil {
		return raw, fmt.Errorf("review: StructuredOutput を JSON にマーシャルできませんでした: %w", err)
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return raw, fmt.Errorf("review: StructuredOutput を rawAgentOutput に decode できませんでした: %w", err)
	}
	return raw, nil
}

// extractFromContent は resp.Content の文字列から JSON オブジェクトを greedy
// regex で抽出して rawAgentOutput に decode する。
//
// 抽出ロジック:
//   - jsonObjectPattern (`(?s)\{.*\}`) で最初の `{` から最後の `}` を取り出す
//   - 抽出した substring を `json.Unmarshal` で rawAgentOutput に decode
//
// 失敗パターン:
//   - Content に `{...}` が一切無い → match 失敗
//   - 抽出した substring が JSON として valid でない → Unmarshal 失敗
//
// いずれも error を返し、呼び出し側 (ParseRuleResponse) が
// FailureCategoryResponseUnparseable に集約する。
func extractFromContent(content string) (rawAgentOutput, error) {
	var raw rawAgentOutput
	match := jsonObjectPattern.FindString(content)
	if match == "" {
		return raw, errors.New("review: Content から JSON オブジェクト候補を抽出できませんでした")
	}
	if err := json.Unmarshal([]byte(match), &raw); err != nil {
		return raw, fmt.Errorf("review: Content から抽出した JSON が decode できませんでした: %w", err)
	}
	return raw, nil
}

// validateAndConvert は decode 済 rawAgentOutput を必須フィールド検証 +
// closed-set severity 検証を経て parsedRule に変換する。
//
// 検証ルール (design.md §review.Parser Validation):
//   - applied フィールドが nil (未指定) → schema 違反
//   - applied=false のとき skip_reason 空 → 失敗
//   - applied=true のとき summary 空 → 失敗
//   - details[].severity が "info"|"warn"|"error" 以外 → 失敗
//
// 成功時の Verdict 決定:
//   - applied=false → VerdictSkipped
//   - applied=true && findings_present=false → VerdictNoFindings
//   - applied=true && findings_present=true → VerdictFindingsReported
func validateAndConvert(raw rawAgentOutput) (parsedRule, error) {
	if raw.Applied == nil {
		return parsedRule{}, errors.New("review: applied フィールドが未指定です")
	}

	if !*raw.Applied {
		// applied=false 経路: skip_reason 必須。
		if raw.SkipReason == "" {
			return parsedRule{}, errors.New("review: applied=false のとき skip_reason が必須です")
		}
		return parsedRule{
			Applied:    false,
			SkipReason: raw.SkipReason,
			Summary:    raw.SkipReason, // 後段の result.RuleResult.Summary に転用しやすいよう転記
			Verdict:    result.VerdictSkipped,
		}, nil
	}

	// applied=true 経路: summary 必須。
	if raw.Summary == "" {
		return parsedRule{}, errors.New("review: applied=true のとき summary が必須です")
	}

	// details[] の severity を closed set で検証しつつ result.Finding に変換する。
	// File は両端契約上必須 (空文字 / 欠落は failed) であり、
	// review-apply-workflow の scope guard allowlist の基底情報となる
	// (result/types.go Finding doc / review-apply-workflow design.md §Patch)。
	findings := make([]result.Finding, 0, len(raw.Details))
	for i, d := range raw.Details {
		sev, ok := canonicalSeverity(d.Severity)
		if !ok {
			return parsedRule{}, fmt.Errorf("review: details[%d].severity %q は info|warn|error 以外で許容されません", i, d.Severity)
		}
		if d.File == "" {
			return parsedRule{}, fmt.Errorf("review: details[%d].file が空文字です (両端契約上 file は必須)", i)
		}
		findings = append(findings, result.Finding{
			Severity: sev,
			File:     d.File,
			Location: d.Location,
			Message:  d.Message,
		})
	}

	verdict := result.VerdictNoFindings
	if raw.FindingsPresent {
		verdict = result.VerdictFindingsReported
	}

	pr := parsedRule{
		Applied:         true,
		FindingsPresent: raw.FindingsPresent,
		Summary:         raw.Summary,
		Verdict:         verdict,
	}
	if len(findings) > 0 {
		pr.Findings = findings
	}
	return pr, nil
}

// canonicalSeverity は agent 応答の severity 文字列を `result.Severity` の closed
// set に正規化する。closed set 違反は (zero-value, false) を返し、
// 呼び出し側で failed 扱いにする。
//
// 設計判断: 大文字小文字の正規化は行わない。schema (両端契約) で
// "info"|"warn"|"error" の小文字に固定されているため、大文字混入は
// 「agent が schema を破った」シグナルとして failed に集約する。
func canonicalSeverity(s string) (result.Severity, bool) {
	switch result.Severity(s) {
	case result.SeverityInfo, result.SeverityWarn, result.SeverityError:
		return result.Severity(s), true
	default:
		return "", false
	}
}

// failedParsed は parser 失敗時の parsedRule を組み立てるヘルパ。
//
// `Verdict=failed` + `FailureCategory="response_unparseable"` を固定で設定し、
// `Summary` には人間可読な失敗理由 (err.Error()) を入れて executor 側の
// `result.RuleResult.Summary` で利用できるようにする。
//
// `Applied` / `FindingsPresent` は zero-value (false) のまま残す。
func failedParsed(err error) parsedRule {
	summary := "agent 応答の解析に失敗しました"
	if err != nil {
		summary = err.Error()
	}
	return parsedRule{
		Verdict:         result.VerdictFailed,
		FailureCategory: FailureCategoryResponseUnparseable,
		Summary:         summary,
	}
}
