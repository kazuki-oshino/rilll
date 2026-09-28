import {
  createObservationFilter,
  normalizeAkashicLevel,
  type AkashicLevel,
} from '@rilll/bridge-protocol/akashic';

type ContextUsageSchedulerOptions = {
  getUsage: () => Promise<unknown>;
  writeUsage: (usage: unknown) => void;
  writeError: (error: Error) => void;
  timeoutMs: number;
  drainTimeoutMs?: number;
};

export type ContextUsageScheduler = {
  schedule: (delayMs?: number) => Promise<void>;
  drain: () => Promise<void>;
};

const summaryHookSubtypes = new Set([
  'PreToolUse',
  'PostToolUse',
  'PostToolUseFailure',
  'PermissionRequest',
  'PermissionDenied',
]);

const filter = createObservationFilter({
  'claude-sdk': {
    // summary: 結果と system_init のみ残す
    summary: (type, subtype) => type === 'result' || (type === 'system' && subtype === 'init'),
    // full: token 単位で大量に流れる stream_event は落とす
    fullExclude: (type) => type === 'stream_event',
  },
  'claude-hook': {
    // summary: 主要フックのみ。PostToolBatch などは間引く。
    // issue #47: user 定義 PreToolUse hook が block を返した際の hook_block と、
    // spawn 自体が失敗した hook_spawn_error は監査価値が高い (誰がどの tool を止めたか / hook が壊れていないか)
    // ので summary でも常に残す。subtype は不問。
    summary: (type, subtype) =>
      (type === 'hook' && subtype !== undefined && summaryHookSubtypes.has(subtype))
      || type === 'hook_block'
      || type === 'hook_spawn_error',
  },
  'claude-control': {
    // summary: 自前で出している context usage 系は常に残す
    summary: (type) => type === 'context_usage' || type === 'context_usage_error',
  },
  'claude-bridge': {
    // summary: canUseTool 経路で吐く permission_auto_allow / permission_deny は
    // 監査価値が高い (誰がどの tool 呼び出しを通した・止めたかの裏取り) ので常に残す。
    summary: (type) => type === 'permission_auto_allow' || type === 'permission_deny',
  },
});

export const shouldWriteObservation = filter;
export { normalizeAkashicLevel };
export type { AkashicLevel };

export function createContextUsageScheduler(options: ContextUsageSchedulerOptions): ContextUsageScheduler {
  let inFlight: Promise<void> | undefined;
  let pendingTimer: NodeJS.Timeout | undefined;
  let pendingDelayed: Promise<void> | undefined;
  let resolvePendingDelayed: (() => void) | undefined;

  const runSnapshot = async (): Promise<void> => {
    try {
      const usage = await withTimeout(options.getUsage(), options.timeoutMs, 'getContextUsage timed out');
      options.writeUsage(usage);
    } catch (error) {
      options.writeError(error instanceof Error ? error : new Error(String(error)));
    } finally {
      inFlight = undefined;
    }
  };

  const clearPendingDelayed = (): void => {
    if (pendingTimer) {
      clearTimeout(pendingTimer);
      pendingTimer = undefined;
    }
    resolvePendingDelayed?.();
    pendingDelayed = undefined;
    resolvePendingDelayed = undefined;
  };

  const startSnapshot = (): Promise<void> => {
    if (!inFlight) {
      inFlight = runSnapshot();
    }
    return inFlight;
  };

  return {
    schedule: async (delayMs = 0) => {
      if (inFlight) {
        await inFlight;
        return;
      }
      if (delayMs <= 0) {
        clearPendingDelayed();
        await startSnapshot();
        return;
      }
      if (!pendingDelayed) {
        pendingDelayed = new Promise((resolve) => {
          resolvePendingDelayed = resolve;
          pendingTimer = setTimeout(() => {
            pendingTimer = undefined;
            void startSnapshot().finally(() => {
              pendingDelayed = undefined;
              resolvePendingDelayed = undefined;
              resolve();
            });
          }, delayMs);
        });
      }
      await pendingDelayed;
    },
    drain: async () => {
      clearPendingDelayed();
      if (!inFlight) return;
      try {
        await withTimeout(
          inFlight,
          options.drainTimeoutMs ?? options.timeoutMs,
          'context usage drain timed out',
        );
      } catch {
        // Best-effort cleanup only. Recording must not block the agent result.
      }
    },
  };
}

async function withTimeout<T>(promise: Promise<T>, timeoutMs: number, message: string): Promise<T> {
  let timeout: NodeJS.Timeout | undefined;
  try {
    return await Promise.race([
      promise,
      new Promise<T>((_, reject) => {
        timeout = setTimeout(() => reject(new Error(message)), timeoutMs);
      }),
    ]);
  } finally {
    if (timeout) clearTimeout(timeout);
  }
}
