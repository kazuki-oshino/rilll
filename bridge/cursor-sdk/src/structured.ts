// Cursor SDK は claude-agent-sdk のような Options.outputFormat: { type: 'json_schema' } 相当の
// ネイティブ seam を持たない (1.0.12 時点)。`AgentOptions / SendOptions` のいずれにも
// system prompt や response format の注入口は無いので、bridge 層で prompt を機械加工して
// モデルに JSON 縛りを依頼するしかない。本モジュールはその prompt 注入と、応答が
// markdown フェンス付きで返ってきた場合の救出 parser を提供する。
//
// 副作用ゼロの純モジュールに切り出してあるので、テストから直接 import できる
// (index.ts は readline を握る top-level 副作用があるためテストから import すると
// stdin が解放されず test runner がハングする)。

import type { RunRequest } from './protocol.js';

// 注入する suffix。prompt 末尾に置くのは last-instruction-wins 傾向に乗りやすく、
// 長いユーザー prompt のときも指示が薄まりにくいため (推奨選択)。
//
// `IMPORTANT:` で始まり、`Reply with **only** a single JSON value` を強調する。
// 余計な prose / markdown フェンス / 説明を生成させないため、明示的に禁止項目も並べる。
export function buildPromptWithSchema(
  prompt: string,
  outputFormat: RunRequest['outputFormat'],
): string {
  if (
    !outputFormat
    || outputFormat.type !== 'json_schema'
    || outputFormat.schema === undefined
  ) {
    return prompt;
  }
  const schemaJson = JSON.stringify(outputFormat.schema, null, 2);
  return [
    prompt,
    '',
    '---',
    '',
    'IMPORTANT: Reply with **only** a single JSON value that strictly conforms to the following JSON schema. Do not include any prose, markdown fences, or commentary outside the JSON value.',
    '',
    'JSON schema:',
    '```json',
    schemaJson,
    '```',
  ].join('\n');
}

// モデルが指示通りに「JSON だけ」で返さないケース (markdown フェンスを付けてしまう /
// "Here is the JSON:" のような前置きを付ける) を救出する parser。
// 1) JSON.parse 直接 → 2) ```json ... ``` フェンス内を取り出して再 parse →
// 3) 文字列内で最初の `{...}` または `[...]` ブロックを取り出して parse、の順に試す。
// すべて失敗したら undefined を返す (= structuredOutput 欠落として上位で扱う)。
export function parseStructuredOutput(text: string): unknown {
  const trimmed = text.trim();
  try {
    return JSON.parse(trimmed);
  } catch {
    // fallthrough
  }
  const fenced = trimmed.match(/^```(?:json)?\s*([\s\S]*?)```$/);
  if (fenced) {
    try {
      return JSON.parse(fenced[1].trim());
    } catch {
      // fallthrough
    }
  }
  const objectMatch = trimmed.match(/[{\[][\s\S]*[}\]]/);
  if (objectMatch) {
    try {
      return JSON.parse(objectMatch[0]);
    } catch {
      // fallthrough
    }
  }
  return undefined;
}
