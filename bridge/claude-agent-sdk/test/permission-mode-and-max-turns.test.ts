// output-contract-2phase Task 2.1: Claude bridge SDK の permissionMode / maxTurns 配線テスト。
// runClaude が `request.permissionMode` / `request.maxTurns` をどう SDK `Options` に流すかを
// mock SDK (deps.query stub) で captured Options を assert する。
//
// 検証する 4 ケース (tasks.md task 2.1):
// 1. `permissionMode` 未指定 → 既定 `acceptEdits` が SDK Options に渡る
// 2. `permissionMode='plan'` → SDK Options に伝搬
// 3. `maxTurns=2` → SDK Options に伝搬
// 4. `maxTurns` 未指定 → SDK Options に渡されない (= 既定維持、key 自体が無い)
//
// structured-error.test.ts と同形の makeScriptedQuery / runWithMessages パターンを踏襲し、
// deps.query の呼び出し引数を捕捉して assert する点だけ異なる。

import assert from 'node:assert/strict';
import test from 'node:test';
import type {
  Options,
  Query,
  SDKMessage,
} from '@anthropic-ai/claude-agent-sdk';
import { runClaude, type ClaudeRuntimeDeps } from '../src/index.js';

// 最小限の success result を 1 件だけ yield する Query 風 fake。
// runClaude の result event 処理が 1 度走り、finally で normal exit する最短経路を作る。
function makeScriptedQuery(sessionId: string): Query {
  const gen = (async function* (): AsyncGenerator<SDKMessage, void, unknown> {
    yield {
      type: 'result',
      subtype: 'success',
      session_id: sessionId,
      result: '',
      structured_output: undefined,
      // SDK 型必須フィールドの最小値。Bridge は参照しないが TS 型を満たすため埋める。
      duration_ms: 0,
      duration_api_ms: 0,
      is_error: false,
      num_turns: 1,
      stop_reason: null,
      total_cost_usd: 0,
      usage: {} as never,
      modelUsage: {},
      permission_denials: [],
      uuid: 'uuid-result-min',
    } as unknown as SDKMessage;
  })();
  Object.assign(gen, {
    async getContextUsage() { return { totalTokens: 0 } as unknown; },
    async interrupt() { /* noop */ },
    async setPermissionMode() { /* noop */ },
    async setModel() { /* noop */ },
    async setMaxThinkingTokens() { /* noop */ },
    async applyFlagSettings() { /* noop */ },
  });
  return gen as unknown as Query;
}

// runClaude を 1 回呼んで deps.query に渡された Options を返す。
// request 側に渡したい permissionMode / maxTurns はそのまま spread する。
async function captureOptions(extra: {
  permissionMode?: 'default' | 'acceptEdits' | 'plan' | 'bypassPermissions';
  maxTurns?: number;
}): Promise<Options> {
  let captured: Options | undefined;
  const deps: ClaudeRuntimeDeps = {
    env: {},
    write: () => { /* drop NDJSON */ },
    // SDK query 引数の prompt 型は `string | AsyncIterable<SDKUserMessage>` のため、
    // ここでは options のみキャプチャし、prompt は触らない (Phase 2 配線は options 経由)。
    query: (_params) => {
      captured = _params.options ?? ({} as Options);
      return makeScriptedQuery('session-test');
    },
  };
  await runClaude(
    {
      type: 'run',
      id: 'req-permission-maxturns',
      prompt: 'p',
      cwd: '/repo',
      akashicLevel: 'summary',
      ...extra,
    },
    deps,
  );
  if (!captured) throw new Error('deps.query was not invoked by runClaude');
  return captured;
}

test('2.1 permissionMode 未指定 → 既定 acceptEdits が SDK Options に渡る', async () => {
  // request.permissionMode を渡さない既存呼び出しは、bridge 側で従来通り
  // 'acceptEdits' を SDK Options に固定する (後方互換: bridge tech.md の不変式)。
  const options = await captureOptions({});
  assert.equal(options.permissionMode, 'acceptEdits');
});

test('2.1 permissionMode=plan → SDK Options に伝搬', async () => {
  // request.permissionMode を 'plan' で渡したとき、SDK Options.permissionMode が
  // 'plan' に切り替わる (Phase 2 等で per-call 上書きできる seam)。
  const options = await captureOptions({ permissionMode: 'plan' });
  assert.equal(options.permissionMode, 'plan');
});

test('2.1 maxTurns=2 → SDK Options に伝搬', async () => {
  // request.maxTurns を 2 で渡したとき、SDK Options.maxTurns に 2 が運ばれる
  // (Phase 2 の "instruction turn + Write turn" 計 2 turn を制限する seam)。
  const options = await captureOptions({ maxTurns: 2 });
  assert.equal(options.maxTurns, 2);
});

test('2.1 maxTurns 未指定 → SDK Options.maxTurns に渡されない (= SDK 既定維持)', async () => {
  // request.maxTurns 未指定のときは SDK Options.maxTurns が key ごと欠落することを確認する。
  // SDK 側既定 turn budget を bridge が上書きしない後方互換ガード。
  const options = await captureOptions({});
  // 'maxTurns' プロパティが Options に乗っていない (= 完全 omit) ことを確認する。
  // undefined 値が明示的にセットされていても assert.equal は通るが、本来 key 自体が無いべきなので
  // 'in' 演算子で構造を直接 assert する。
  assert.equal('maxTurns' in (options as Record<string, unknown>), false, 'maxTurns key should be omitted from Options when request.maxTurns is undefined');
});
