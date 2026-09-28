// bridge-result-error-propagation Task 3.1: mock SDK で runClaude を駆動し、
// emit される NDJSON 列を assert する test harness を用意する。
// codex の bridge/codex-sdk/test/structured-retry.test.ts の makeScriptedThread / runWithTurns
// パターンを踏襲した対称構造 (mock SDK helper + chunks 収集 + JSON line parse) で書く。
//
// Task 3.1: success シナリオ 1 件を assert (本ファイル下記の `3.1 success scenario ...`)。
// Task 3.2: 4 種類の terminal error subtype と success regression の bridge event 列を assert
//   (`3.2 error_max_structured_output_retries ...` / `3.2 error_during_execution ...` /
//    `3.2 error_max_turns ...` / `3.2 error_max_budget_usd ...`)。
// Task 3.3: unknown subtype の defensive fallback assert
//   (`3.3 unknown subtype emits error event with code=unknown defensive fallback`)。

import assert from 'node:assert/strict';
import test from 'node:test';
import type {
  Query,
  SDKMessage,
} from '@anthropic-ai/claude-agent-sdk';
import { runClaude, type ClaudeRuntimeDeps } from '../src/index.js';

type BridgeEvent = Record<string, unknown>;

// mock SDK が yield する system_init message を簡潔に組み立てるヘルパー。
// claude bridge の isSystemInitMessage は { type:'system', subtype:'init', session_id, ... } を
// 検出して system_init event を emit する。test では SDK 仕様の最小プロパティだけ詰める。
function buildSystemInitMessage(sessionId: string): SDKMessage {
  return {
    type: 'system',
    subtype: 'init',
    session_id: sessionId,
    cwd: '/repo',
    tools: [],
    mcp_servers: [],
    model: 'mock',
    permissionMode: 'acceptEdits',
    slash_commands: [],
    output_style: 'default',
    // SDK type の必須フィールドだが test では検査対象外。最小値で埋める。
    apiKeySource: 'none',
    claude_code_version: '0.0.0-test',
    skills: [],
    plugins: [],
    uuid: 'uuid-system-init',
  } as unknown as SDKMessage;
}

// makeScriptedQuery: 与えられた SDKMessage 列を順に yield する Query 風 fake。
// codex の makeScriptedThread と同様、real SDK の AsyncGenerator + 追加メソッドだけを最小限満たす。
// claude bridge 側は for await ループに加えて createContextUsageScheduler 経由で getContextUsage() を
// 呼ぶため、Promise を返す stub を実装しておく必要がある。
function makeScriptedQuery(messages: readonly SDKMessage[]): Query {
  const gen = (async function* (): AsyncGenerator<SDKMessage, void, unknown> {
    for (const msg of messages) {
      yield msg;
    }
  })();
  // Query 互換にするため追加メソッドを生成器に attach する。test では getContextUsage 以外は
  // 実行経路に現れない (cancel 等) ため最小限の noop に倒す。
  Object.assign(gen, {
    async getContextUsage() {
      return { totalTokens: 0 } as unknown;
    },
    async interrupt() { /* noop */ },
    async setPermissionMode() { /* noop */ },
    async setModel() { /* noop */ },
    async setMaxThinkingTokens() { /* noop */ },
    async applyFlagSettings() { /* noop */ },
  });
  return gen as unknown as Query;
}

// runWithMessages: mock SDK が messages を順に yield するシナリオを 1 回流し、
// runClaude が deps.write に書いた NDJSON chunks を JSON parse して返す。
// codex の runWithTurns と同形 (chunks.join → split → parse)。
async function runWithMessages(messages: readonly SDKMessage[]): Promise<BridgeEvent[]> {
  const chunks: string[] = [];
  const deps: ClaudeRuntimeDeps = {
    env: {},
    write: (chunk) => { chunks.push(chunk); },
    query: () => makeScriptedQuery(messages),
  };
  await runClaude(
    {
      type: 'run',
      id: 'req-structured-error',
      prompt: 'test prompt',
      cwd: '/repo',
      akashicLevel: 'summary',
    },
    deps,
  );
  return chunks.join('').trim().split('\n').filter(Boolean).map((line) => JSON.parse(line) as BridgeEvent);
}

// bridge-result-error-propagation Task 3.2: 4 種類の terminal error subtype 用の result message を
// 組み立てるヘルパー。SDKResultError 型 (sdk.d.ts:3297-3315) は result field を持たず、errors: string[] /
// session_id / usage / modelUsage 等の必須 field を持つ。test では bridge dispatcher が参照する
// 最小フィールド (type/subtype/session_id/errors) のみを意味的に正しく埋め、他は SDK 型を満たす最小値で
// 補完する (any cast 1 箇所)。Task 3.1 の buildSystemInitMessage と対称構造。
function buildErrorResultMessage(
  subtype: 'error_max_structured_output_retries'
    | 'error_during_execution'
    | 'error_max_turns'
    | 'error_max_budget_usd',
  sessionId: string,
  errors: readonly string[] = [],
): SDKMessage {
  return {
    type: 'result',
    subtype,
    session_id: sessionId,
    errors: [...errors],
    // SDK 型必須フィールドの最小値。Bridge は参照しないが TS 型を満たすため埋める。
    duration_ms: 0,
    duration_api_ms: 0,
    is_error: true,
    num_turns: 1,
    stop_reason: null,
    total_cost_usd: 0,
    usage: {} as never,
    modelUsage: {},
    permission_denials: [],
    uuid: `uuid-result-${subtype}`,
  } as unknown as SDKMessage;
}

test('3.1 success scenario emits system_init / result (with structuredOutput) / done in order', async () => {
  const sessionId = 'session-test';
  const resultText = '{"answer":42}';
  const structured = { answer: 42 };

  const messages: SDKMessage[] = [
    buildSystemInitMessage(sessionId),
    {
      type: 'result',
      subtype: 'success',
      session_id: sessionId,
      result: resultText,
      structured_output: structured,
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
      uuid: 'uuid-result',
    } as unknown as SDKMessage,
  ];

  const events = await runWithMessages(messages);

  // claude bridge の system_init event (Req 4.1 / 5.3 sanity: 既存挙動維持)
  const systemInit = events.find((e) => e.type === 'system_init') as
    | { systemInfo?: { sessionId?: string } }
    | undefined;
  assert.ok(systemInit, 'system_init event should be emitted');
  assert.equal(systemInit?.systemInfo?.sessionId, sessionId);

  // result event (success branch: result / sessionId / structuredOutput が運搬される)
  const resultEvents = events.filter((e) => e.type === 'result');
  assert.equal(resultEvents.length, 1, 'exactly one result event expected on success');
  const resultEvent = resultEvents[0] as {
    result?: string;
    sessionId?: string;
    structuredOutput?: unknown;
  };
  assert.equal(resultEvent.result, resultText);
  assert.equal(resultEvent.sessionId, sessionId);
  assert.deepEqual(resultEvent.structuredOutput, structured);

  // done event はライフサイクル終端 (Req 1.6 success regression)
  const doneEvents = events.filter((e) => e.type === 'done');
  assert.equal(doneEvents.length, 1, 'exactly one done event expected');
  const doneEvent = doneEvents[0] as { result?: string; sessionId?: string };
  assert.equal(doneEvent.result, resultText);
  assert.equal(doneEvent.sessionId, sessionId);

  // success 経路では error event が emit されないことの cross-check (Req 4.1)
  const errorEvents = events.filter((e) => e.type === 'error');
  assert.equal(errorEvents.length, 0, 'no error event expected on success');

  // 観測完了条件 (tasks.md 3.1): event 配列が system_init / result (success 構造) / done を含む
  // ことを assert する。順序は claude bridge の emit 順に従い、system_init が result より先、
  // result が done より先に出現する。
  const systemInitIdx = events.findIndex((e) => e.type === 'system_init');
  const resultIdx = events.findIndex((e) => e.type === 'result');
  const doneIdx = events.findIndex((e) => e.type === 'done');
  assert.ok(systemInitIdx >= 0 && resultIdx >= 0 && doneIdx >= 0, 'all 3 event types present');
  assert.ok(
    systemInitIdx < resultIdx && resultIdx < doneIdx,
    `expected order system_init < result < done; got ${systemInitIdx}/${resultIdx}/${doneIdx}`,
  );
});

// bridge-result-error-propagation Task 3.2 (Req 1.1 / 1.5 / 1.6 / 2.1 / 4.1 / 5.1 / 5.3):
// SDK が `subtype === 'error_max_structured_output_retries'` を返したとき、claude bridge は
// `result` event を `result=''` / `structuredOutput=undefined` / `sessionId` で emit し、
// codex / cursor の parse 失敗契約と対称な意味を caller に伝える。error event は emit せず、
// `done` event はライフサイクル終端として常時 emit される。
test('3.2 error_max_structured_output_retries emits result event with structuredOutput=undefined', async () => {
  const sessionId = 'session-1';
  const events = await runWithMessages([
    buildSystemInitMessage(sessionId),
    buildErrorResultMessage('error_max_structured_output_retries', sessionId, ['parse failed']),
  ]);

  const results = events.filter((e) => e.type === 'result');
  const errors = events.filter((e) => e.type === 'error');
  const dones = events.filter((e) => e.type === 'done');

  // parse 失敗時は result event 1 件のみ。error event 経路へは流れない (codex/cursor と対称)。
  assert.equal(results.length, 1, 'exactly one result event expected');
  assert.equal(errors.length, 0, 'no error event expected on error_max_structured_output_retries');
  assert.equal(dones.length, 1, 'exactly one done event expected (Req 1.6)');

  const resultEvent = results[0] as { result?: string; sessionId?: string; structuredOutput?: unknown };
  assert.equal(resultEvent.result, '', 'result text should be empty string');
  assert.equal(resultEvent.structuredOutput, undefined, 'structuredOutput must be undefined');
  // Req 1.5: SDK session_id が result event の sessionId field に運搬される
  assert.equal(resultEvent.sessionId, sessionId);

  const doneEvent = dones[0] as { sessionId?: string };
  // done event でも sessionId が運搬される (既存挙動)
  assert.equal(doneEvent.sessionId, sessionId);
});

// bridge-result-error-propagation Task 3.2 (Req 1.2 / 1.5 / 1.6 / 4.1 / 5.2 / 5.3):
// SDK が `subtype === 'error_during_execution'` を返したとき、claude bridge は
// `error` event を `code='error_during_execution'` / `retryable=false` / `message=<SDK errors[] join>` で
// emit する。`result` event は emit せず、`done` event でライフサイクルを閉じる。
// sessionId は error event schema (errorEventSchema) には field が無いため、done event 経由で運搬される。
test('3.2 error_during_execution emits error event with code=error_during_execution and retryable=false', async () => {
  const sessionId = 'session-2';
  const events = await runWithMessages([
    buildSystemInitMessage(sessionId),
    buildErrorResultMessage('error_during_execution', sessionId, ['execution failed']),
  ]);

  const results = events.filter((e) => e.type === 'result');
  const errors = events.filter((e) => e.type === 'error');
  const dones = events.filter((e) => e.type === 'done');

  // 異常終了系は error event 1 件のみ。result event は emit されない (区別が崩れないことの cross-check)。
  assert.equal(errors.length, 1, 'exactly one error event expected');
  assert.equal(results.length, 0, 'no result event expected on error_during_execution');
  assert.equal(dones.length, 1, 'exactly one done event expected (Req 1.6)');

  const errorEvent = errors[0] as { code?: string; retryable?: boolean; message?: string };
  assert.equal(errorEvent.code, 'error_during_execution');
  assert.equal(errorEvent.retryable, false);
  // message は SDK errors[] 由来の非空文字列 (Req 1.2)
  assert.equal(typeof errorEvent.message, 'string');
  assert.ok((errorEvent.message ?? '').length > 0, 'message should be non-empty string');

  // Req 1.5: error event schema に sessionId field は無いため、done event 経由で session resume 経路を維持する
  const doneEvent = dones[0] as { sessionId?: string };
  assert.equal(doneEvent.sessionId, sessionId);
});

// bridge-result-error-propagation Task 3.2 (Req 1.3 / 1.5 / 1.6 / 4.1 / 5.2 / 5.3):
// SDK が `subtype === 'error_max_turns'` を返したとき、claude bridge は
// `error` event を `code='error_max_turns'` / `retryable=false` で emit する。
test('3.2 error_max_turns emits error event with code=error_max_turns and retryable=false', async () => {
  const sessionId = 'session-3';
  const events = await runWithMessages([
    buildSystemInitMessage(sessionId),
    buildErrorResultMessage('error_max_turns', sessionId),
  ]);

  const results = events.filter((e) => e.type === 'result');
  const errors = events.filter((e) => e.type === 'error');
  const dones = events.filter((e) => e.type === 'done');

  assert.equal(errors.length, 1, 'exactly one error event expected');
  assert.equal(results.length, 0, 'no result event expected on error_max_turns');
  assert.equal(dones.length, 1, 'exactly one done event expected (Req 1.6)');

  const errorEvent = errors[0] as { code?: string; retryable?: boolean; message?: string };
  assert.equal(errorEvent.code, 'error_max_turns');
  assert.equal(errorEvent.retryable, false);
  // SDK errors[] が空のとき、bridge は subtype 文字列を message に流用する (dispatcher 実装規約)
  assert.equal(typeof errorEvent.message, 'string');
  assert.ok((errorEvent.message ?? '').length > 0, 'message should be non-empty string');

  // Req 1.5: sessionId は done event 経由で運搬
  const doneEvent = dones[0] as { sessionId?: string };
  assert.equal(doneEvent.sessionId, sessionId);
});

// bridge-result-error-propagation Task 3.2 (Req 1.4 / 1.5 / 1.6 / 4.1 / 5.2 / 5.3):
// SDK が `subtype === 'error_max_budget_usd'` を返したとき、claude bridge は
// `error` event を `code='error_max_budget_usd'` / `retryable=false` で emit する。
test('3.2 error_max_budget_usd emits error event with code=error_max_budget_usd and retryable=false', async () => {
  const sessionId = 'session-4';
  const events = await runWithMessages([
    buildSystemInitMessage(sessionId),
    buildErrorResultMessage('error_max_budget_usd', sessionId),
  ]);

  const results = events.filter((e) => e.type === 'result');
  const errors = events.filter((e) => e.type === 'error');
  const dones = events.filter((e) => e.type === 'done');

  assert.equal(errors.length, 1, 'exactly one error event expected');
  assert.equal(results.length, 0, 'no result event expected on error_max_budget_usd');
  assert.equal(dones.length, 1, 'exactly one done event expected (Req 1.6)');

  const errorEvent = errors[0] as { code?: string; retryable?: boolean; message?: string };
  assert.equal(errorEvent.code, 'error_max_budget_usd');
  assert.equal(errorEvent.retryable, false);
  assert.equal(typeof errorEvent.message, 'string');
  assert.ok((errorEvent.message ?? '').length > 0, 'message should be non-empty string');

  // Req 1.5: sessionId は done event 経由で運搬
  const doneEvent = dones[0] as { sessionId?: string };
  assert.equal(doneEvent.sessionId, sessionId);
});

// bridge-result-error-propagation Task 3.3 (Req 5.1 / 5.5):
// SDK が known SDKResultError.subtype enum (error_during_execution / error_max_turns /
// error_max_budget_usd / error_max_structured_output_retries) のいずれにも該当しない未知 subtype
// (例: SDK 将来拡張で増えた `error_future_unknown_x`) を `result` message で返したとき、
// claude bridge は defensive fallback として `error` event を
// `code='unknown'` / `message=JSON.stringify(message)` (非空文字列) / `retryable=false` で emit する。
// `result` event は emit せず、`done` event でライフサイクルを閉じる (Req 1.6)。
// buildErrorResultMessage helper は closed union 型 (4 subtype のみ) を受け付ける設計なので、
// unknown subtype 検証では test 内インラインで `as unknown as SDKMessage` cast により構築する
// (helper の type safety を弱めない)。
test('3.3 unknown subtype emits error event with code=unknown defensive fallback', async () => {
  const sessionId = 'session-unknown';
  const events = await runWithMessages([
    buildSystemInitMessage(sessionId),
    {
      type: 'result',
      subtype: 'error_future_unknown_x',
      session_id: sessionId,
      errors: [],
      // SDK 型必須フィールドの最小値。Bridge は参照しないが TS 型を満たすため埋める。
      duration_ms: 0,
      duration_api_ms: 0,
      is_error: true,
      num_turns: 1,
      stop_reason: null,
      total_cost_usd: 0,
      usage: {} as never,
      modelUsage: {},
      permission_denials: [],
      uuid: 'uuid-result-unknown',
    } as unknown as SDKMessage,
  ]);

  const results = events.filter((e) => e.type === 'result');
  const errors = events.filter((e) => e.type === 'error');
  const dones = events.filter((e) => e.type === 'done');

  // defensive fallback: unknown subtype は error event 経路へ流れる。
  // result event は emit されない (cross-check; success / parse 失敗との混同を防ぐ)。
  assert.equal(errors.length, 1, 'exactly one error event expected for unknown subtype');
  assert.equal(results.length, 0, 'no result event expected for unknown subtype');
  assert.equal(dones.length, 1, 'exactly one done event expected (Req 1.6)');

  const errorEvent = errors[0] as { code?: string; retryable?: boolean; message?: string };
  // Req 5.1 / 5.5: defensive fallback の規約 (design.md `Subtype Dispatcher` Event Contract)。
  assert.equal(errorEvent.code, 'unknown', 'code should be defensive fallback "unknown"');
  assert.equal(errorEvent.retryable, false, 'unknown subtype is terminal; retryable=false');
  // message は JSON.stringify(message) 由来の非空文字列 (dispatcher 実装規約)。
  assert.equal(typeof errorEvent.message, 'string');
  assert.ok((errorEvent.message ?? '').length > 0, 'message should be non-empty string');

  // sessionId は done event 経由で運搬される (error event schema に sessionId field は無いため)。
  const doneEvent = dones[0] as { sessionId?: string };
  assert.equal(doneEvent.sessionId, sessionId);
});
