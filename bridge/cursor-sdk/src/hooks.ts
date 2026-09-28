// claude bridge の `claude-hook` source と対称な hook event 定義を 1 箇所に集約する。
// ここを index.ts に置くと、テストから import した瞬間に readline.createInterface 等の
// top-level 副作用が走って test runner が stdin を保持したままハングするため、副作用ゼロの
// 純モジュールとして切り出している。

export type ToolHookSubtype = 'PreToolUse' | 'PostToolUse' | 'PostToolUseFailure';

export const hookObservationSource = 'cursor-hook';

// SDK の tool_call SDKMessage が持つ status (`running` | `completed` | `error`) を、
// claude-hook と同型の hook subtype にマップする。
// running → Pre, completed → Post, error → PostToolUseFailure。未知値は安全側で
// PostToolUse に倒す (akashic 側で「失敗扱いにしない」方針)。
export function toolHookSubtype(
  status: 'running' | 'completed' | 'error' | string,
): ToolHookSubtype {
  if (status === 'running') return 'PreToolUse';
  if (status === 'error') return 'PostToolUseFailure';
  return 'PostToolUse';
}
