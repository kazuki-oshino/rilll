// Package learnings の本 file は Reflector / Curator phase の LLM 応答から JSON object
// を多段 fallback で抽出する helper を提供する (design.md L591 / Req 4.4 / 6.2 / 9.3)。
//
// v1 distiller の `extractAgentResponseJSON` (`internal/workflow/builtin/distiller/jsonextract.go`)
// と機能等価の同形再実装。design.md L372 が「v1 helper を import 流用 (本 spec では
// import path 変更なし)」と明示しているため、cross-package 依存を避けて本 package 内に
// 閉じる方針 (将来 cleanup 系 spec で SSoT 化を検討)。
//
// 解釈順 (kiro `inquiryloop.ParseResponse` と同形):
//
//  1. `resp.StructuredOutput` が `map[string]any` ならそれを再 marshal して `out` に Unmarshal
//     (claude bridge の `outputFormat=json_schema` 経路では `result.structured_output` に値が
//     入り `result.result` が空になるため、本経路の優先採用が claude 互換の必須条件 /
//     spike-results.md Findings #2 で確認済み)。
//  2. `resp.Content` を直接 `json.Unmarshal`。
//  3. ```json … ``` コードフェンス内を抽出して `json.Unmarshal`。
//  4. 最初の '{' と最後の '}' で切り出して `json.Unmarshal`。
//
// 戻り値:
//
//   - rawJSON: out に詰め込んだ JSON の正規化 source (artifact 書き出し用)。
//     抽出経路が `StructuredOutput` の場合は再 marshal 結果、Content 経路は採用された
//     範囲の文字列を返す。
//   - ok: いずれかの段階で `json.Unmarshal` に成功すれば true。全段階失敗で false。
//
// 注意: 本関数は構造的に妥当な JSON object を取り出すだけで、`out` の意味的検証
// (required field 充足等) は呼び出し側の責務。`out` 側の field tag が match しなければ
// `json.Unmarshal` は当然 error を返す。
package learnings

import (
	"encoding/json"
	"strings"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

// extractAgentResponseJSON は Reflector / Curator の agent 応答から JSON object を
// 抽出して与えられた `out` 構造体に Unmarshal する。
func extractAgentResponseJSON(resp provider.AgentResponse, out any) (rawJSON string, ok bool) {
	// (1) StructuredOutput 優先 (claude bridge の json_schema 経路)
	if record, isMap := resp.StructuredOutput.(map[string]any); isMap && record != nil {
		bs, err := json.Marshal(record)
		if err == nil {
			if err := json.Unmarshal(bs, out); err == nil {
				return string(bs), true
			}
		}
	}

	content := strings.TrimSpace(resp.Content)
	if content == "" {
		return "", false
	}

	// (2) Content 全体を直接 decode
	if err := json.Unmarshal([]byte(content), out); err == nil {
		return content, true
	}

	// (3) ```json … ``` フェンス
	if fenced, fenceOK := extractFencedJSONFromContent(content); fenceOK {
		if err := json.Unmarshal([]byte(fenced), out); err == nil {
			return fenced, true
		}
	}

	// (4) 最初の '{' から最後の '}' で切り出し
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start >= 0 && end > start {
		candidate := content[start : end+1]
		if err := json.Unmarshal([]byte(candidate), out); err == nil {
			return candidate, true
		}
	}

	return "", false
}

// extractFencedJSONFromContent は ```json\n … \n``` ブロックの中身を返す。
// 言語タグなし ``` … ``` も「JSON っぽい」場合のサルベージ用に試行する。
//
// schema_test.go 側の `extractFencedJSON([]byte) ([]byte, error)` と機能領域は別
// (本関数は LLM 応答の content 抽出用 / 後者は skill SSoT contract 検証用)。
// 関数名を分離して同 package 内の名前衝突を避ける。
//
// 見つからない場合 ("", false)。
func extractFencedJSONFromContent(s string) (string, bool) {
	// ```json\n ... \n``` を最優先で探す
	if idx := strings.Index(s, "```json"); idx >= 0 {
		rest := s[idx+len("```json"):]
		// 開き fence の直後の改行を skip
		rest = strings.TrimPrefix(rest, "\n")
		if end := strings.Index(rest, "```"); end >= 0 {
			candidate := strings.TrimSpace(rest[:end])
			if candidate != "" {
				return candidate, true
			}
		}
	}
	// 言語タグなし ``` … ``` も試行 (中身が JSON らしいか後続の Unmarshal で判定)
	if idx := strings.Index(s, "```"); idx >= 0 {
		rest := s[idx+3:]
		rest = strings.TrimPrefix(rest, "\n")
		if end := strings.Index(rest, "```"); end >= 0 {
			candidate := strings.TrimSpace(rest[:end])
			if strings.HasPrefix(candidate, "{") {
				return candidate, true
			}
		}
	}
	return "", false
}
