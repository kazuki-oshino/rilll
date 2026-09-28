// Package apply の本ファイルは task 3.2 / design.md §Parse の Service Interface を実装する
// (Req 4.1, 4.5)。
//
// 役割: self-review LLM からの provider.AgentResponse を SelfReviewRecord (result.go で定義)
// に正規化する。Workflow 層 (task 5.3) は本関数の戻り値を CycleRecord.SelfReview に格納し、
// `accept` で apply フェーズへ、`regenerate` で attempt++ で再生成ループに進む。失敗時の
// sentinel error は Workflow 層で `VerdictFailed` に変換される (Req 4.5 / verdict.go §Verdict)。
//
// パース戦略 (design.md §Parse Implementation Notes):
//
//  1. resp.StructuredOutput を最優先で decode する経路。claude / codex bridge は
//     OutputFormat=json_schema で JSONSchemaForSelfReview (prompt.go) に基づく構造化出力を
//     返すため、本経路が happy path となる。
//  2. StructuredOutput が nil もしくは構造体に decode できない場合は resp.Content から
//     greedy regex (`(?s)\{.*\}`) で JSON オブジェクトを抽出する fallback 経路。cursor 経路
//     (CLI が OutputFormat を解釈しない) と、claude / codex が稀に schema を破った場合の救済を兼ねる
//     (review/parse.go と同形)。
//  3. 上記いずれの decode 経路でも JSON 構造を取り出せなかった場合は ErrSelfReviewUnparseable を
//     返す。
//
// 検証 (design.md §Parse):
//
//   - verdict が "accept" / "regenerate" 以外 (空文字 / 任意の他文字列) は
//     ErrSelfReviewSchemaViolation を返す (closed set 二値 enum の構造的強制)。
//     decode 経路 (StructuredOutput / Content fallback) のいずれで成功した場合でも、
//     verdict 値が範囲外であれば即座に schema violation として終端する (fallthrough しない)。
//     reason は空でも許容する (LLM が無理由な regenerate を返した場合は JSONSchemaForSelfReview
//     の required 制約と prompt 指示で上流防御する; Parse 層では verdict のみを検証)。
//
// 失敗時の戻り値:
//
//   - StructuredOutput / Content の双方で decode 失敗 → (zero-value SelfReviewRecord,
//     ErrSelfReviewUnparseable) を返す。Workflow 層は VerdictFailed に変換 (Req 4.5)。
//   - decode 成功 + verdict が closed set 違反 → (zero-value SelfReviewRecord,
//     ErrSelfReviewSchemaViolation) を返す。Workflow 層は VerdictFailed に変換 (Req 4.5)。
//
// 副作用なし、決定的純関数 (review/parse.go と同方針)。
package apply

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

// ErrSelfReviewUnparseable は self-review LLM 応答から JSON 構造を抽出できなかったときに
// 返す sentinel (errors.Is で判別可能)。
//
// StructuredOutput が nil もしくは struct decode に失敗し、かつ Content にも JSON
// 候補が見つからない (greedy regex match なし) / もしくは候補は見つかったが json.Unmarshal
// に失敗した、いずれかの状態で本 sentinel が返される。
//
// 呼び出し側 (Workflow task 5.3) は `errors.Is(err, ErrSelfReviewUnparseable)` で
// 失敗カテゴリを判別し、`VerdictFailed` に変換する責務を持つ (Req 4.5)。
var ErrSelfReviewUnparseable = errors.New("apply: self-review レスポンスから JSON schema 適合構造を抽出できませんでした")

// ErrSelfReviewSchemaViolation は self-review LLM 応答の verdict 値が closed set
// (`accept` / `regenerate`) を逸脱したときに返す sentinel (errors.Is で判別可能)。
//
// JSONSchemaForSelfReview (prompt.go) の enum 制約を構造的に二重防御する位置付けで、
// LLM が schema を破って空文字や任意の他文字列を返した場合に本 sentinel で reject する。
// 呼び出し側 (Workflow task 5.3) は本 sentinel も `VerdictFailed` に変換する (Req 4.5)。
var ErrSelfReviewSchemaViolation = errors.New("apply: self-review レスポンスの verdict が closed set (accept|regenerate) を逸脱しました")

// selfReviewJSONPattern は Content fallback 経路で agent 応答から JSON オブジェクトを
// 抽出する greedy regex (review/parse.go の jsonObjectPattern と同形)。
//
// (?s) で `.` を改行にも match させ、最初の `{` から最後の `}` までを greedy にキャプチャする。
// fenced code block (```json ... ```) や前後の散文に包まれていても、外側の self-review
// オブジェクトを 1 ショットで取り出せる。抽出した候補は json.Unmarshal で再検証されるため、
// 厳密な JSON パーサとしての性質は持たない (「JSON 候補 substring」の抽出に過ぎない)。
var selfReviewJSONPattern = regexp.MustCompile(`(?s)\{.*\}`)

// rawSelfReview は self-review JSON schema (prompt.go §JSONSchemaForSelfReview) に対応する
// 中間 decode 型。StructuredOutput direct decode と Content fallback の両経路で共有する。
//
// JSON tag は schema (両端契約 design.md §Parse) と一致する。Verdict / Reason のいずれも
// 必須フィールドとし、未設定 (空文字) は schema 違反として扱う。
type rawSelfReview struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

// ParseSelfReview は self-review LLM 応答を SelfReviewRecord に正規化する
// (design.md §Parse / Req 4.1, 4.5)。
//
// 戻り値:
//   - 成功時: (SelfReviewRecord{Verdict, Reason, Attempt}, nil)。DurationMs は 0 のままとし、
//     Workflow 層 (task 5.3) が agent 呼び出しの計測値で別途上書きする責務を持つ
//     (本関数は Content / StructuredOutput からは duration を導けないため SRP に従う)。
//   - 失敗時: (zero-value SelfReviewRecord, sentinel error)。
//     sentinel は ErrSelfReviewUnparseable / ErrSelfReviewSchemaViolation のいずれか。
//
// パース順序:
//  1. resp.StructuredOutput が non-nil の場合、JSON 経由で rawSelfReview に decode を試みる
//     (direct decode 経路)。decode 成功時は verdict 検証へ進み、verdict が closed set 違反なら
//     即 ErrSelfReviewSchemaViolation を返す (Content fallback には流さない: schema 違反は
//     fundamental contract violation のため、cursor 経路だろうと許容しない)。
//  2. (1) が decode 失敗 / StructuredOutput が nil の場合、resp.Content から selfReviewJSONPattern で
//     JSON オブジェクトを抽出して decode を試みる (fallback 経路)。decode 成功時は verdict 検証へ進む。
//  3. 両経路の decode が失敗した場合 (StructuredOutput decode 失敗 + Content から JSON を抽出できない、
//     または抽出した JSON が rawSelfReview に decode できない) は ErrSelfReviewUnparseable を返す。
//
// attempt は呼び出し側 (Workflow 5.3) が 1 始まりの cycle 番号を指定する。本関数では検証せず
// SelfReviewRecord.Attempt にそのまま転記する (1.3 の CycleRecord.Cycle と一致する契約は
// Workflow 層で保証される)。
func ParseSelfReview(resp provider.AgentResponse, attempt int) (SelfReviewRecord, error) {
	// (1) StructuredOutput direct decode 経路 (claude / codex bridge happy path)。
	if resp.StructuredOutput != nil {
		if raw, err := decodeStructuredSelfReview(resp.StructuredOutput); err == nil {
			// decode 成功 → verdict 検証 (closed set 違反は即 SchemaViolation で終端)。
			return validateSelfReview(raw, attempt)
		}
		// StructuredOutput decode 失敗時は cursor 経路や schema 破壊応答の救済として
		// Content fallback にフォールスルー (review/parse.go と同方針)。
	}

	// (2) Content fallback regex 経路 (cursor 経路 + schema 破壊応答の救済)。
	if raw, err := extractSelfReviewFromContent(resp.Content); err == nil {
		return validateSelfReview(raw, attempt)
	}

	// (3) 両経路 decode 失敗 → ErrSelfReviewUnparseable。
	//     Workflow 層は VerdictFailed に変換する (Req 4.5)。
	return SelfReviewRecord{}, fmt.Errorf("%w", ErrSelfReviewUnparseable)
}

// decodeStructuredSelfReview は resp.StructuredOutput (any 型) を rawSelfReview に変換する。
// bridge 実装により map[string]any / 構造化型 / json.RawMessage 等の様々な実体型が来るため、
// いったん json.Marshal で正規化してから json.Unmarshal で型強制する 2 段階方式とする
// (review/parse.go の decodeStructuredOutput と同形)。
func decodeStructuredSelfReview(structured any) (rawSelfReview, error) {
	var raw rawSelfReview
	b, err := json.Marshal(structured)
	if err != nil {
		return raw, fmt.Errorf("apply: StructuredOutput を JSON にマーシャルできませんでした: %w", err)
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return raw, fmt.Errorf("apply: StructuredOutput を rawSelfReview に decode できませんでした: %w", err)
	}
	return raw, nil
}

// extractSelfReviewFromContent は resp.Content から selfReviewJSONPattern で JSON オブジェクトを
// 抽出して rawSelfReview に decode する (cursor 経路 fallback)。
//
// 失敗パターン:
//   - Content に `{...}` が一切無い → match 失敗で error。
//   - 抽出した substring が JSON として valid でない → Unmarshal 失敗で error。
//
// いずれも error を返し、呼び出し側 (ParseSelfReview) が ErrSelfReviewUnparseable に集約する。
func extractSelfReviewFromContent(content string) (rawSelfReview, error) {
	var raw rawSelfReview
	match := selfReviewJSONPattern.FindString(content)
	if match == "" {
		return raw, errors.New("apply: Content から JSON オブジェクト候補を抽出できませんでした")
	}
	if err := json.Unmarshal([]byte(match), &raw); err != nil {
		return raw, fmt.Errorf("apply: Content から抽出した JSON が decode できませんでした: %w", err)
	}
	return raw, nil
}

// validateSelfReview は decode 済 rawSelfReview の verdict を closed set で検証し、
// 通過すれば SelfReviewRecord に変換する。
//
// 検証ルール (design.md §Parse):
//   - verdict が "accept" / "regenerate" のいずれかなら通過 (SelfReviewVerdict 型に変換)。
//   - 上記以外 (空文字 / "MAYBE" / その他任意の文字列) は ErrSelfReviewSchemaViolation を返す。
//
// reason は空でも許容する (Parse 層では verdict のみを検証する契約 / 本ファイル冒頭の検証セクション参照)。
func validateSelfReview(raw rawSelfReview, attempt int) (SelfReviewRecord, error) {
	var verdict SelfReviewVerdict
	switch raw.Verdict {
	case string(SelfReviewAccept):
		verdict = SelfReviewAccept
	case string(SelfReviewRegenerate):
		verdict = SelfReviewRegenerate
	default:
		// closed set 違反 → ErrSelfReviewSchemaViolation (fallthrough しない)。
		// raw.Verdict の生値を fmt.Errorf で error 文字列に含めて debug 容易性を確保する。
		return SelfReviewRecord{}, fmt.Errorf("%w: verdict=%q", ErrSelfReviewSchemaViolation, raw.Verdict)
	}
	return SelfReviewRecord{
		Verdict: verdict,
		Reason:  raw.Reason,
		Attempt: attempt,
		// DurationMs は Workflow 層 (task 5.3) が agent 呼び出しの計測値で上書きする (本関数では 0)。
	}, nil
}
