// issue #47: PreToolUse 用 user 定義 hook を SDK 互換の HookCallback に翻訳するモジュール。
// 本ファイルは index.ts から writers (writeEvent / writeObservation) を依存注入で受け取る形にし、
// テストではモック writers を渡して spawn 経路だけを実 bash で検証可能にする。

import { spawn } from 'node:child_process';
import type { HookCallback } from '@anthropic-ai/claude-agent-sdk';
import type { AkashicLevel } from './akashic.js';
import type { BridgeEvent } from './protocol.js';

// user 定義 hook の spawn timeout。verify-gate 等は通常 100ms 未満で終わる想定だが、
// 暴走 hook がワークフロー全体を止めないよう 30s で打ち止めにする。timeout 時は best-effort
// allow に倒し、hook_spawn_error observation で監査可能にする (block しない)。
export const userHookTimeoutMs = 30_000;

// user 定義 hook の spawn 結果を表す型。`decision === "block"` のとき bridge は SDK callback の
// 戻り値を `decision: "block"` + `systemMessage: reason` に翻訳して SDK に当該 tool 呼び出しを拒否させる。
// それ以外 (undefined / 不明値) はすべて allow 扱いで素通しする。
export type UserHookOutput = {
  decision?: 'block';
  reason?: string;
};

// userHookCallback / spawnUserHook が akashic / bridge 出力を行うための writer 注入型。
// 本番環境では index.ts の writeEvent / writeObservation を closure として渡す。
// テストでは記録用配列に push する mock を渡し、外部副作用を観測する。
export type UserHookWriters = {
  writeEvent: (event: BridgeEvent) => void;
  writeObservation: (
    level: AkashicLevel,
    source: string,
    payload: unknown,
    explicitType?: string,
    explicitSubtype?: string,
  ) => void;
};

// spawnUserHook は user 定義 hook の shell command を child_process.spawn で起動し、
// payload(JSON) を stdin に流して stdout の JSON を解釈する。
// hook 設計指針 (verify-gate.sh 等) は claude-code の hook と同じ contract:
//   - stdin: SDK PreToolUse payload (tool_name, tool_input, hook_event_name 等を含む JSON)
//   - stdout: 空 / `{}` → allow 扱い / `{"decision":"block","reason":"..."}` → SDK が当該 tool を deny
//   - exit code: 反映しない (claude-code の hook 仕様に揃え、JSON だけを判定基準にする)
//
// timeout / spawn 失敗 / 不正 JSON はすべて throw し、呼び出し側 (userHookCallback) で best-effort
// allow に倒す。理由: hook 自体のバグや一時的な fs エラーで agent ループ全体が止まると運用負荷が高く、
// verify-gate のような「無いより有るほうがマシ」設計の hook が壊れたときに workflow 全体を巻き込む
// のは過剰反応のため。
export async function spawnUserHook(
  command: string[],
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
      // SIGKILL で確実に kill (SIGTERM だと shell トラップで握りつぶされる可能性がある)
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
      // stdin write 失敗 (まれだが pipe 早期 close など) はそのまま reject 経路へ。
      clearTimeout(timer);
      settle(() => reject(writeErr as Error));
    }
  });
}

// userHookCallback は user 定義 PreToolUse hook を SDK の HookCallback に翻訳するファクトリ。
// SDK の hook config では既に matcher (正規表現) で対象 tool を絞ることができるので、
// 本 callback は matcher 一致前提で呼ばれる想定。block 時のみ
// `decision: "block"` + `systemMessage: reason` を返し、SDK に当該 tool 呼び出しを拒否させる。
//
// 副作用 (writers 経由):
//   - block 時: hook_block observation を akashic に書き、error event (retryable=false, code='hook_block')
//     を bridge stdout に流す。Go 側 ConsumeEvents は code='hook_block' のとき run を中断せず観測のみ行う
//     (bridge SDK ループ継続中なのに Go だけ終了する乖離を避けるため)
//   - spawn 失敗 / timeout / 不正 JSON: hook_spawn_error observation を出して allow に倒す (block しない)
export function userHookCallback(
  level: AkashicLevel,
  command: string[],
  cwd: string,
  requestID: string,
  writers: UserHookWriters,
  spawnImpl: typeof spawnUserHook = spawnUserHook,
): HookCallback {
  return async (input, toolUseID) => {
    let result: UserHookOutput;
    try {
      result = await spawnImpl(command, input, cwd);
    } catch (err) {
      // best-effort: hook 自身の失敗で agent を止めないため、observation を出して allow 継続。
      writers.writeObservation(
        level,
        'claude-hook',
        {
          command,
          toolUseID,
          hookEventName: input.hook_event_name,
          error: err instanceof Error ? err.message : String(err),
        },
        'hook_spawn_error',
      );
      return {};
    }

    if (result.decision !== 'block') {
      return {};
    }

    const reason = result.reason ?? 'user hook blocked tool use (no reason provided)';
    writers.writeObservation(
      level,
      'claude-hook',
      {
        command,
        toolUseID,
        hookEventName: input.hook_event_name,
        reason,
      },
      'hook_block',
    );
    // 受入条件: block 発生時 Go 側 provider.AgentEvent で error/retryable=false が伝播する。
    // ただし SDK 側 query ループは継続するため、Go 側 ConsumeEvents は本 error を観測したら
    // code='hook_block' を見て run 終了せず onEvent でだけ伝える (整合性確保)。
    writers.writeEvent({
      type: 'error',
      id: requestID,
      message: `hook_block: ${reason}`,
      code: 'hook_block',
      retryable: false,
    });
    return {
      decision: 'block',
      systemMessage: reason,
    } as Awaited<ReturnType<HookCallback>>;
  };
}
