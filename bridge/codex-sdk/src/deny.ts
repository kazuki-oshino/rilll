// issue #65 B2: Codex bridge 用の deny gate。
// Claude bridge `evaluateDeny` (bridge/claude-agent-sdk/src/index.ts) と同パターンで、
// command_execution.command と file_change.changes[].path を判定する。
// 判定ロジック (regex / cwd 外チェック) は shared/deny-patterns.ts に集約してあり、
// 本ファイルは ThreadItem 形状からそれらを呼び出す薄い adapter。

import path from 'node:path';
import type { ThreadItem } from '@openai/codex-sdk';
import {
  isWriteTargetOutsideCwd,
  matchDangerousBashCommand,
} from '@rilll/bridge-protocol/deny-patterns';

export interface DenyResult {
  reason: string;
  // どの判定経路でブロックされたかを akashic 識別用に保持する。
  // 'dangerous_bash' | 'write_outside_cwd' のいずれか。
  source: 'dangerous_bash' | 'write_outside_cwd';
  // ブロックの根拠となった部分情報 (akashic 観測用)。
  details: Record<string, unknown>;
}

// ThreadItem を deny するべきか判定する。
// 判定対象:
//   - command_execution: item.command を dangerousBashPatterns に通す
//   - file_change: item.changes[].path を isWriteTargetOutsideCwd で判定
// それ以外の item type (mcp_tool_call / web_search / agent_message 等) は対象外で undefined を返す。
export function evaluateDeny(item: ThreadItem, cwd: string): DenyResult | undefined {
  if (item.type === 'command_execution') {
    const reason = matchDangerousBashCommand(item.command);
    if (reason) {
      return {
        source: 'dangerous_bash',
        reason: `command_execution deny: ${reason} (command="${item.command.slice(0, 200)}")`,
        details: { command: item.command, matched: reason },
      };
    }
    return undefined;
  }
  if (item.type === 'file_change') {
    for (const change of item.changes) {
      if (!change.path) continue;
      if (isWriteTargetOutsideCwd(change.path, cwd)) {
        return {
          source: 'write_outside_cwd',
          reason: `file_change deny: cwd (${path.resolve(cwd)}) 外への書き込みは拒否されました: ${change.path}`,
          details: { filePath: change.path, kind: change.kind },
        };
      }
    }
    return undefined;
  }
  return undefined;
}
