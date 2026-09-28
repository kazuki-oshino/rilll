// output-contract-2phase Task 2.3: Cursor bridge SDK の permissionMode / maxTurns 配線テスト。
//
// Cursor SDK (@cursor/sdk) には permissionMode / maxTurns のいずれにも直接対応する option が
// 存在しないため、Cursor bridge では両 field を **documented no-op** として扱う。Phase 2 の
// ツール制限 (Write のみ許可など) は Phase 2 instruction 内 prompt fallback で別途実現する
// (Phase 2 helper / agent 層側の責務)。
//
// 検証する 4 シナリオ (tasks.md task 2.3):
//   1. permissionMode='plan' → Agent.create() / agent.send() の引数が baseline と完全一致 (no-op)
//   2. maxTurns=2          → Agent.create() / agent.send() の引数が baseline と完全一致 (no-op)
//   3. permissionMode + maxTurns 同時指定 → 同上 (no-op)
//   4. 両 field 未指定 (baseline) → Agent.create() / agent.send() に新 field が混入しない
//
// 本テストは「将来 cursor-sdk index.ts が誤って permissionMode / maxTurns を SDK に流す
// 配線を追加した場合に CI で検知する」ための regression guard として機能する。
// 実装 (src/index.ts) は SDK Options / sendOptions / 環境変数を一切変更しない documented no-op で
// 据え置く。

import assert from 'node:assert/strict';
import test from 'node:test';
import type {
  Agent,
  Cursor,
  ModelListItem,
  ModelSelection,
  Run,
  RunResult,
  SDKAgent,
  SDKMessage,
  SendOptions,
} from '@cursor/sdk';
import { runCursor, type CursorRuntimeDeps } from '../src/index.js';

// Cursor SDK 呼び出しの引数を運搬する captured 構造。
// - createCalls / resumeCalls: Agent.create / Agent.resume に渡された options 全体 (cwd, model 等)。
// - sendCalls: agent.send() に渡された prompt と sendOptions のキー列。
//   sendOptions.onDelta は function なので deep-equal 比較から除外し、構造 (key の集合) のみを assert する。
interface Captured {
  readonly createCalls: unknown[];
  readonly resumeCalls: { id: string; options: unknown }[];
  readonly sendCalls: { prompt: string; sendOptionKeys: string[] }[];
  readonly envSnapshot: Record<string, string | undefined>;
}

function makeFakeRun(): Run {
  const stream = (async function* (): AsyncGenerator<SDKMessage> {
    // 最低限の終了経路: tool 系メッセージなしで終わる
  })();
  return {
    stream(): AsyncGenerator<SDKMessage> { return stream; },
    async wait(): Promise<RunResult> {
      return { id: 'run-noop', result: 'ok', durationMs: 1, status: 'finished' } as RunResult;
    },
    async cancel() { /* noop */ },
  } as unknown as Run;
}

function makeFakeAgent(captured: Captured): SDKAgent {
  return {
    agentId: 'agent-noop',
    async send(prompt: string, sendOptions?: SendOptions): Promise<Run> {
      captured.sendCalls.push({
        prompt,
        // sendOptions に含まれる key 名一覧だけを記録する (onDelta は function で deep-equal 不能)。
        sendOptionKeys: sendOptions ? Object.keys(sendOptions).sort() : [],
      });
      return makeFakeRun();
    },
    close() { /* noop */ },
  } as unknown as SDKAgent;
}

function makeFakeAgentApi(captured: Captured): typeof Agent {
  return {
    async create(opts: { model: ModelSelection; local?: { cwd: string } }): Promise<SDKAgent> {
      // options 全体を JSON クローンで保存し、後段で deep-equal できるようにする
      // (Cursor SDK の ModelSelection は plain object のため structuredClone で安全に複製可能)。
      captured.createCalls.push(JSON.parse(JSON.stringify(opts)));
      return makeFakeAgent(captured);
    },
    async resume(id: string, opts: unknown): Promise<SDKAgent> {
      captured.resumeCalls.push({ id, options: JSON.parse(JSON.stringify(opts)) });
      return makeFakeAgent(captured);
    },
  } as unknown as typeof Agent;
}

function makeFakeCursor(): typeof Cursor {
  return {
    models: {
      async list(_opts: { apiKey?: string }): Promise<ModelListItem[]> {
        return [
          {
            id: 'gpt-5.5',
            displayName: 'gpt-5.5',
            parameters: [],
          } as unknown as ModelListItem,
        ];
      },
    },
  } as unknown as typeof Cursor;
}

async function runWith(
  request: Parameters<typeof runCursor>[0],
): Promise<Captured> {
  const captured: Captured = {
    createCalls: [],
    resumeCalls: [],
    sendCalls: [],
    // 環境変数は deps.env で固定する (実プロセス env への副作用がないことを担保するため snapshot)。
    envSnapshot: { CURSOR_API_KEY: 'sk-test' },
  };
  const deps: CursorRuntimeDeps = {
    env: { CURSOR_API_KEY: 'sk-test' },
    write: () => undefined,
    cursor: makeFakeCursor(),
    agent: makeFakeAgentApi(captured),
  };
  await runCursor(request, deps);
  return captured;
}

// --- 2.3 documented no-op: permissionMode ---

test('2.3 permissionMode=plan → Cursor SDK 呼び出し (Agent.create / agent.send) は baseline と完全一致 (no-op)', async () => {
  const baseline = await runWith({
    type: 'run',
    id: 'req-pm-baseline',
    prompt: 'work',
    cwd: '/repo',
    akashicLevel: 'summary',
  });
  const withPlan = await runWith({
    type: 'run',
    id: 'req-pm-plan',
    prompt: 'work',
    cwd: '/repo',
    akashicLevel: 'summary',
    permissionMode: 'plan',
  });

  // Agent.create / Agent.resume / agent.send の呼び出し回数と引数すべてが baseline と一致する
  // (= permissionMode は SDK に何も流れない documented no-op)。
  assert.deepEqual(withPlan.createCalls, baseline.createCalls);
  assert.deepEqual(withPlan.resumeCalls, baseline.resumeCalls);
  assert.deepEqual(withPlan.sendCalls, baseline.sendCalls);
});

// --- 2.3 documented no-op: maxTurns ---

test('2.3 maxTurns=2 → Cursor SDK 呼び出し (Agent.create / agent.send) は baseline と完全一致 (no-op)', async () => {
  const baseline = await runWith({
    type: 'run',
    id: 'req-mt-baseline',
    prompt: 'work',
    cwd: '/repo',
    akashicLevel: 'summary',
  });
  const withMaxTurns = await runWith({
    type: 'run',
    id: 'req-mt-2',
    prompt: 'work',
    cwd: '/repo',
    akashicLevel: 'summary',
    maxTurns: 2,
  });

  // maxTurns は Cursor SDK の Agent / Run option に流れない (no-op)。prompt も改変されない。
  assert.deepEqual(withMaxTurns.createCalls, baseline.createCalls);
  assert.deepEqual(withMaxTurns.resumeCalls, baseline.resumeCalls);
  assert.deepEqual(withMaxTurns.sendCalls, baseline.sendCalls);
});

// --- 2.3 documented no-op: 両 field 同時指定 ---

test('2.3 permissionMode と maxTurns を同時指定 → Cursor SDK 呼び出しは baseline と完全一致 (no-op)', async () => {
  const baseline = await runWith({
    type: 'run',
    id: 'req-both-baseline',
    prompt: 'work',
    cwd: '/repo',
    akashicLevel: 'summary',
  });
  const withBoth = await runWith({
    type: 'run',
    id: 'req-both',
    prompt: 'work',
    cwd: '/repo',
    akashicLevel: 'summary',
    permissionMode: 'plan',
    maxTurns: 3,
  });

  assert.deepEqual(withBoth.createCalls, baseline.createCalls);
  assert.deepEqual(withBoth.resumeCalls, baseline.resumeCalls);
  assert.deepEqual(withBoth.sendCalls, baseline.sendCalls);
});

// --- 2.3 baseline 構造 sanity check ---
//
// baseline (両 field 未指定) のときに Agent.create の options に余計な field が混入していないことを
// 構造的に確認する (regression guard: 将来 permissionMode / maxTurns を流す配線が誤って追加された場合、
// この test と上記 3 test の少なくとも 1 つが失敗する設計)。

test('2.3 baseline (両 field 未指定) → Agent.create options に permissionMode / maxTurns が混入しない', async () => {
  const baseline = await runWith({
    type: 'run',
    id: 'req-baseline-sanity',
    prompt: 'work',
    cwd: '/repo',
    akashicLevel: 'summary',
  });
  assert.equal(baseline.createCalls.length, 1, 'Agent.create should be called once for a new session run');
  const created = baseline.createCalls[0] as Record<string, unknown>;
  // documented no-op: permissionMode / maxTurns key 自体が SDK options に乗らない。
  assert.equal('permissionMode' in created, false, 'permissionMode must not appear in Agent.create options (Cursor bridge no-op)');
  assert.equal('maxTurns' in created, false, 'maxTurns must not appear in Agent.create options (Cursor bridge no-op)');
});
