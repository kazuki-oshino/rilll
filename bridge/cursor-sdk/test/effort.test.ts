// issue #65 A2: request.effort が Cursor bridge で reasoning param value (preferredThinking)
// として注入されることを fake SDK 経由で検証する。
// requested model がある場合でも effort が指定されたら reasoning override を効かせる仕様。

import assert from 'node:assert/strict';
import test from 'node:test';
import type {
  Cursor,
  Agent,
  ModelListItem,
  ModelSelection,
  Run,
  RunResult,
  SDKMessage,
  SDKAgent,
  SendOptions,
} from '@cursor/sdk';
import { runCursor, type CursorRuntimeDeps } from '../src/index.js';

interface CapturedAgentCreate {
  model: ModelSelection;
}

function makeFakeAgent(captured: CapturedAgentCreate[]): typeof Agent {
  const fake = {
    async create(opts: { model: ModelSelection }): Promise<SDKAgent> {
      captured.push({ model: opts.model });
      const stream = (async function* (): AsyncGenerator<SDKMessage> {
        // 最低限の終了経路: tool 系メッセージなしで終わる
      })();
      const fakeRun: Run = {
        stream(): AsyncGenerator<SDKMessage> { return stream; },
        async wait(): Promise<RunResult> {
          return { id: 'run-x', result: 'ok', durationMs: 1, status: 'finished' };
        },
        async cancel() { /* noop */ },
      } as unknown as Run;
      return {
        agentId: 'agent-x',
        async send(_input: string, _opts?: SendOptions): Promise<Run> { return fakeRun; },
        close() { /* noop */ },
      } as unknown as SDKAgent;
    },
    async resume(_id: string, _opts: unknown): Promise<SDKAgent> {
      throw new Error('not used');
    },
  };
  return fake as unknown as typeof Agent;
}

function makeFakeCursor(modelList: ModelListItem[]): typeof Cursor {
  return {
    models: {
      async list(_opts: { apiKey?: string }): Promise<ModelListItem[]> {
        return modelList;
      },
    },
  } as unknown as typeof Cursor;
}

async function runWith(
  request: Parameters<typeof runCursor>[0],
  modelList: ModelListItem[],
): Promise<CapturedAgentCreate[]> {
  const captured: CapturedAgentCreate[] = [];
  const deps: CursorRuntimeDeps = {
    env: { CURSOR_API_KEY: 'sk-test' },
    write: () => undefined,
    cursor: makeFakeCursor(modelList),
    agent: makeFakeAgent(captured),
  };
  await runCursor(request, deps);
  return captured;
}

test('A2: request.effort overrides preferredThinking on Cursor agent.create', async () => {
  // requested model が指定されているケースでも effort が reasoning param に流れる
  const captured = await runWith({
    type: 'run',
    id: 'req-effort',
    prompt: 'hi',
    cwd: '/repo',
    model: 'gpt-5.5',
    effort: 'medium',
    akashicLevel: 'summary',
  }, [
    {
      id: 'gpt-5.5',
      displayName: 'gpt-5.5',
      parameters: [
        { id: 'reasoning', values: [{ value: 'low' }, { value: 'medium' }, { value: 'high' }] },
      ],
    } as ModelListItem,
  ]);

  assert.equal(captured.length, 1);
  const params = (captured[0].model as { params?: { id: string; value: string }[] }).params ?? [];
  const reasoningParam = params.find((p) => p.id === 'reasoning');
  assert.ok(reasoningParam, 'reasoning param should be set when effort is requested');
  assert.equal(reasoningParam?.value, 'medium');
});

test('A2: omitted request.effort retains existing default behavior', async () => {
  // effort を指定しない場合は requested model の reasoning override は効かない (preferredThinking=undefined)
  const captured = await runWith({
    type: 'run',
    id: 'req-no-effort',
    prompt: 'hi',
    cwd: '/repo',
    model: 'gpt-5.5',
    akashicLevel: 'summary',
  }, [
    {
      id: 'gpt-5.5',
      displayName: 'gpt-5.5',
      parameters: [
        { id: 'reasoning', values: [{ value: 'low' }, { value: 'medium' }, { value: 'high' }] },
      ],
    } as ModelListItem,
  ]);

  const params = (captured[0].model as { params?: { id: string; value: string }[] }).params;
  // requested model + no effort のときは params が undefined または空
  assert.ok(params === undefined || params.length === 0);
});

test('explicit model params are forwarded to Cursor agent.create', async () => {
  const request = {
    type: 'run' as const,
    id: 'req-model-params',
    prompt: 'hi',
    cwd: '/repo',
    model: 'gpt-5.6-luna',
    modelParams: [{ id: 'reasoning', value: 'max' }],
    akashicLevel: 'summary' as const,
  };
  const captured = await runWith(request, [
    {
      id: 'gpt-5.6-luna',
      displayName: 'GPT-5.6 Luna',
      parameters: [
        { id: 'reasoning', values: [{ value: 'none' }, { value: 'max' }] },
      ],
    } as ModelListItem,
  ]);

  assert.deepEqual(captured[0].model, {
    id: 'gpt-5.6-luna',
    params: [{ id: 'reasoning', value: 'max' }],
  });
});

test('gpt-5.6-sol-high preset maps effort to reasoning param on Cursor agent.create', async () => {
  const captured = await runWith({
    type: 'run',
    id: 'req-sol-high',
    prompt: 'hi',
    cwd: '/repo',
    model: 'gpt-5.6-sol',
    effort: 'high',
    akashicLevel: 'summary',
  }, [
    {
      id: 'gpt-5.6-sol',
      displayName: 'GPT-5.6 Sol',
      parameters: [
        { id: 'reasoning', values: [{ value: 'low' }, { value: 'high' }, { value: 'max' }] },
      ],
    } as ModelListItem,
  ]);

  assert.deepEqual(captured[0].model, {
    id: 'gpt-5.6-sol',
    params: [{ id: 'reasoning', value: 'high' }],
  });
});
