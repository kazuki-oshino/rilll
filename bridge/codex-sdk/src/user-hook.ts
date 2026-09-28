// issue #65 B1: Codex bridge 用の PreToolUse user hook 擬似実装。
// Claude bridge の userHookCallback は SDK ネイティブの PreToolUse hook chain で実行されるが、
// Codex SDK には canUseTool 相当の事前介入経路が無い。そのため item.started / item.updated を
// 観測経路として、matcher hit → spawn → block 判定 → AbortController.abort() で
// post-execution kill switch を実現する。

import { spawn } from 'node:child_process';
import type { ThreadItem } from '@openai/codex-sdk';

// user 定義 hook の spawn timeout。verify-gate 等は通常 100ms 未満で終わる想定だが、
// 暴走 hook がワークフロー全体を止めないよう 30s で打ち止めにする (Claude bridge と揃える)。
export const userHookTimeoutMs = 30_000;

export interface UserHookConfig {
  matcher: string;
  command: string[];
}

export type UserHookOutput = {
  decision?: 'block';
  reason?: string;
};

// 評価結果。allow / block / error のいずれかを取り、上位はこれをもとに abort 判定 / observation 送出を行う。
export type UserHookEvaluation =
  | { decision: 'allow' }
  | {
      decision: 'block';
      reason: string;
      matcher: string;
      command: readonly string[];
    }
  | {
      decision: 'error';
      matcher: string;
      command: readonly string[];
      spawnError: string;
    };

// Claude bridge の matcher 仕様 (tool 名の正規表現) を Codex ThreadItem に適用するための変換。
// 1 item あたり複数の論理 tool 名を返し、ユーザは Claude / Codex どちらの語彙でも matcher を書けるようにする。
// 例: matcher='Bash' / 'command_execution' のどちらでも command_execution item にヒットする。
export function toolNamesForCodexItem(item: ThreadItem): string[] {
  switch (item.type) {
    case 'command_execution':
      return ['Bash', 'command_execution'];
    case 'file_change':
      return ['Write', 'Edit', 'MultiEdit', 'file_change'];
    case 'mcp_tool_call':
      return [`mcp:${item.server}/${item.tool}`, `mcp__${item.server}__${item.tool}`];
    case 'web_search':
      return ['WebSearch', 'web_search'];
    case 'todo_list':
      return ['TodoWrite', 'todo_list'];
    default:
      return [];
  }
}

// user 定義 hook の shell command を child_process.spawn で起動し、payload(JSON) を stdin に
// 流して stdout の JSON を解釈する。Claude bridge の spawnUserHook と同じ contract:
//   - stdin: hook payload (item / toolNames / hook_event_name 等を含む JSON)
//   - stdout: 空 / `{}` → allow / `{"decision":"block","reason":"..."}` → block
//   - exit code: 反映しない (JSON だけを判定基準にする)
//
// timeout / spawn 失敗 / 不正 JSON はすべて throw し、呼び出し側 (evaluateUserHooks) で
// 'error' decision に倒す。
export async function spawnUserHook(
  command: readonly string[],
  payload: unknown,
  cwd: string,
  timeoutMs: number = userHookTimeoutMs,
): Promise<UserHookOutput> {
  if (command.length === 0) {
    throw new Error('spawnUserHook: command must be non-empty');
  }
  const [bin, ...args] = command;
  return await new Promise<UserHookOutput>((resolve, reject) => {
    const child = spawn(bin, args, { cwd, stdio: ['pipe', 'pipe', 'pipe'] });
    const stdoutChunks: Buffer[] = [];
    const stderrChunks: Buffer[] = [];
    let settled = false;
    const settle = (fn: () => void): void => {
      if (settled) return;
      settled = true;
      fn();
    };
    const timer = setTimeout(() => {
      try { child.kill('SIGKILL'); } catch { /* best-effort */ }
      settle(() => reject(new Error(`user hook timed out after ${timeoutMs}ms`)));
    }, timeoutMs);
    child.stdout.on('data', (chunk: Buffer) => stdoutChunks.push(chunk));
    child.stderr.on('data', (chunk: Buffer) => stderrChunks.push(chunk));
    child.on('error', (err) => {
      clearTimeout(timer);
      settle(() => reject(err));
    });
    child.on('close', () => {
      clearTimeout(timer);
      const stdout = Buffer.concat(stdoutChunks).toString('utf8').trim();
      if (stdout === '') {
        settle(() => resolve({}));
        return;
      }
      try {
        const parsed = JSON.parse(stdout) as unknown;
        if (typeof parsed !== 'object' || parsed === null) {
          settle(() => resolve({}));
          return;
        }
        const obj = parsed as { decision?: unknown; reason?: unknown };
        const out: UserHookOutput = {};
        if (obj.decision === 'block') out.decision = 'block';
        if (typeof obj.reason === 'string') out.reason = obj.reason;
        settle(() => resolve(out));
      } catch (parseErr) {
        const stderr = Buffer.concat(stderrChunks).toString('utf8').slice(0, 1024);
        settle(() => reject(new Error(`user hook stdout is not JSON: ${(parseErr as Error).message} (stderr: ${stderr})`)));
      }
    });
    try {
      child.stdin.write(JSON.stringify(payload));
      child.stdin.end();
    } catch (writeErr) {
      clearTimeout(timer);
      settle(() => reject(writeErr as Error));
    }
  });
}

// item に対して userHooks の matcher を順に評価し、最初に block を返した hook の結果で確定する。
// 1 つも block / spawn 失敗が無いときは allow を返す。spawn 失敗時は 'error' を返し、
// 上位 (index.ts) で hook_spawn_error observation を emit したうえで allow に倒す。
export async function evaluateUserHooks(
  item: ThreadItem,
  userHooks: readonly UserHookConfig[],
  cwd: string,
  spawnImpl: typeof spawnUserHook = spawnUserHook,
): Promise<UserHookEvaluation> {
  if (userHooks.length === 0) return { decision: 'allow' };
  const toolNames = toolNamesForCodexItem(item);
  if (toolNames.length === 0) return { decision: 'allow' };

  for (const hook of userHooks) {
    let re: RegExp;
    try {
      re = new RegExp(hook.matcher);
    } catch {
      // 不正な正規表現は安全側 (= skip) に倒す。akashic 側の検出は上位で行う前提。
      continue;
    }
    const matched = toolNames.some((name) => re.test(name));
    if (!matched) continue;

    let result: UserHookOutput;
    try {
      result = await spawnImpl(
        hook.command,
        { item, toolNames, hook_event_name: 'PreToolUse' },
        cwd,
      );
    } catch (err) {
      return {
        decision: 'error',
        matcher: hook.matcher,
        command: hook.command,
        spawnError: err instanceof Error ? err.message : String(err),
      };
    }
    if (result.decision === 'block') {
      return {
        decision: 'block',
        reason: result.reason ?? 'user hook blocked tool use (no reason provided)',
        matcher: hook.matcher,
        command: hook.command,
      };
    }
  }
  return { decision: 'allow' };
}
