package agent

import "github.com/kazuki-oshino/rilll/internal/facet"

// prependPreviousResponse は content に summary context の policy を適用した上で
// "# Previous Response" ブロックを prompt 先頭に前置した
// combined prompt を返す純関数 helper である。
//
// 不変式:
//   - 短文 (rune 数 <= facet.CONTEXT_MAX_CHARS): content をそのまま埋め込む。
//   - 長文 (rune 数 >  facet.CONTEXT_MAX_CHARS): Summary context の policy で
//     rune 境界に切り詰め、切り詰め通知を付加する。
//   - content の escape / sanitize は行わない (Req 2.4: LLM 認識精度に委ねる)。
//   - 同一引数で常に同一文字列を返す (純関数 / Req 2.5)。グローバル状態 / I/O 依存ゼロ。
//
// 呼び出し前提:
//   - content == "" のガードは呼び出し側 (agent.CallAgent) の責務 (Req 1.4)。
//     本関数自体は防御的に空 content を受けても panic せず、空ブロックを生成する。
//
// PreviousResponseContent は前段応答の要約として扱うため、
// facet.PromptContextSummary の loss-tolerant policy を適用する。
func prependPreviousResponse(prompt, content string) string {
	embedded := facet.TrimPromptContext(facet.PromptContextSummary, content, "")
	return "# Previous Response\n\n" + embedded + "\n\n---\n\n" + prompt
}
