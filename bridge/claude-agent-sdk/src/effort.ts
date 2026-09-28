import type { RunRequest } from '@rilll/bridge-protocol';

// issue #65 A2: protocol の effort enum を Claude SDK の EffortLevel にマッピングする pure 関数。
// index.ts に置くと top-level 副作用 (readline.createInterface) が import 時に走り、
// 単体テストプロセスが stdin を保持したままハングするため、副作用ゼロの専用モジュールに切り出している。
//
// Claude SDK は 'low'|'medium'|'high'|'xhigh'|'max' を受けるが、protocol は
// 'minimal'|'low'|'medium'|'high'|'xhigh'|'max' を運ぶ。'minimal' は SDK では受けないため
// 最も控えめな 'low' に倒す (SDK 仕様: minimal 相当の値はないのでこれが下限)。

export type ClaudeEffort = 'low' | 'medium' | 'high' | 'xhigh' | 'max';
export type ProtocolEffort = NonNullable<RunRequest['effort']>;

// rilll bridge 側の既定 effort。request.effort が undefined のとき適用する。
// SDK 既定値ではなく、rilll bridge 層で reasoning を引き上げる方針 (上流の指定がなければ高め)。
export const DEFAULT_CLAUDE_EFFORT: ClaudeEffort = 'xhigh';

export function mapClaudeEffort(effort: ProtocolEffort): ClaudeEffort {
  if (effort === 'minimal') return 'low';
  return effort;
}

// resolveClaudeEffort は request.effort (undefined 含む) を SDK へ渡す最終値に解決する。
// undefined → DEFAULT_CLAUDE_EFFORT、それ以外 → mapClaudeEffort のマッピング結果。
export function resolveClaudeEffort(effort: ProtocolEffort | undefined): ClaudeEffort {
  if (effort === undefined) return DEFAULT_CLAUDE_EFFORT;
  return mapClaudeEffort(effort);
}
