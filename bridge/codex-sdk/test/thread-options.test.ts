// issue #65 A1/A2/A3/A4: per-call な model / effort / sandbox / approval が
// Codex SDK の ThreadOptions に正しく到達することを fake Codex 経由で検証する。

import assert from 'node:assert/strict';
import test from 'node:test';
import type {
  Codex,
  CodexOptions,
  Thread,
  ThreadEvent,
  ThreadOptions,
} from '@openai/codex-sdk';
import { runCodex, type CodexRuntimeDeps } from '../src/index.js';
import { runRequestSchema } from '../src/protocol.js';

interface CapturedOptions {
  codex: CodexOptions[];
  startThread: ThreadOptions[];
  resumeThread: { id: string; options: ThreadOptions | undefined }[];
}

function makeCodex(captured: CapturedOptions, fakeEvents: ThreadEvent[]): Codex {
  const buildThread = (): Thread => {
    const thread = {
      async runStreamed() {
        return {
          events: (async function* (): AsyncGenerator<ThreadEvent> {
            for (const ev of fakeEvents) yield ev;
          })(),
        };
      },
      async run() {
        throw new Error('not used');
      },
      get id(): string | null { return 'thread-x'; },
    };
    return thread as unknown as Thread;
  };
  return {
    startThread(options?: ThreadOptions): Thread {
      captured.startThread.push(options ?? {});
      return buildThread();
    },
    resumeThread(id: string, options?: ThreadOptions): Thread {
      captured.resumeThread.push({ id, options });
      return buildThread();
    },
  } as unknown as Codex;
}

type BridgeEvent = Record<string, unknown>;

async function runWith(request: Parameters<typeof runCodex>[0]): Promise<CapturedOptions> {
  const captured: CapturedOptions = { codex: [], startThread: [], resumeThread: [] };
  const fakeEvents: ThreadEvent[] = [
    { type: 'thread.started', thread_id: 'thread-x' },
    {
      type: 'turn.completed',
      usage: {
        input_tokens: 0,
        cached_input_tokens: 0,
        cache_write_input_tokens: 0,
        output_tokens: 0,
        reasoning_output_tokens: 0,
      },
    },
    { type: 'item.completed', item: { id: 'msg', type: 'agent_message', text: 'ok' } },
  ];
  const deps: CodexRuntimeDeps = {
    env: {},
    write: () => undefined,
    createCodex: (options: CodexOptions) => {
      captured.codex.push(options);
      return makeCodex(captured, fakeEvents);
    },
  };
  await runCodex(request, deps);
  return captured;
}

async function runWithEvents(request: Parameters<typeof runCodex>[0]): Promise<BridgeEvent[]> {
  const captured: CapturedOptions = { codex: [], startThread: [], resumeThread: [] };
  const chunks: string[] = [];
  const fakeEvents: ThreadEvent[] = [
    { type: 'thread.started', thread_id: 'thread-x' },
    {
      type: 'turn.completed',
      usage: {
        input_tokens: 0,
        cached_input_tokens: 0,
        cache_write_input_tokens: 0,
        output_tokens: 0,
        reasoning_output_tokens: 0,
      },
    },
    { type: 'item.completed', item: { id: 'msg', type: 'agent_message', text: 'ok' } },
  ];
  const deps: CodexRuntimeDeps = {
    env: {},
    write: (chunk) => { chunks.push(chunk); },
    createCodex: (options: CodexOptions) => {
      captured.codex.push(options);
      return makeCodex(captured, fakeEvents);
    },
  };
  await runCodex(request, deps);
  return chunks.join('').trim().split('\n').filter(Boolean).map((line) => JSON.parse(line) as BridgeEvent);
}

test('GPT-6 Luna max reaches SDK config alongside reasoning summary on fresh and resumed threads', async () => {
  for (const resumeSessionId of [undefined, 'existing-thread']) {
    const request = runRequestSchema.parse({
      type: 'run', id: 'req-luna', prompt: 'hi', cwd: '/repo',
      model: 'gpt-6-luna', effort: 'max', reasoningSummary: 'concise', resumeSessionId,
    });
    const captured = await runWith(request);
    assert.deepEqual(captured.codex[0].config, {
      model_reasoning_summary: 'concise', model_reasoning_effort: 'max',
    });
    const options = resumeSessionId ? captured.resumeThread[0].options : captured.startThread[0];
    assert.equal(options?.model, 'gpt-6-luna');
    assert.equal(options?.modelReasoningEffort, undefined);
  }
});

test('reasoningSummary reaches CodexOptions config', async () => {
  const captured = await runWith({
    type: 'run',
    id: 'req-reasoning-summary',
    prompt: 'hi',
    cwd: '/repo',
    reasoningSummary: 'detailed',
    akashicLevel: 'summary',
  });
  assert.deepEqual(captured.codex[0].config, { model_reasoning_summary: 'detailed' });
});

test('reasoningSummary remains omitted when request omits it', async () => {
  const captured = await runWith({
    type: 'run',
    id: 'req-no-reasoning-summary',
    prompt: 'hi',
    cwd: '/repo',
    akashicLevel: 'summary',
  });
  assert.equal(captured.codex[0].config, undefined);
});

test('reasoningSummary is omitted for gpt-5.3-codex-spark because the model rejects reasoning.summary', async () => {
  const captured = await runWith({
    type: 'run',
    id: 'req-spark-reasoning-summary',
    prompt: 'hi',
    cwd: '/repo',
    model: 'gpt-5.3-codex-spark',
    effort: 'high',
    reasoningSummary: 'concise',
    akashicLevel: 'summary',
  });
  assert.equal(
    'config' in (captured.codex[0] as unknown as Record<string, unknown>),
    false,
  );
});

test('A3: sandboxMode defaults to danger-full-access when request omits it', async () => {
  const captured = await runWith({
    type: 'run',
    id: 'req-a3-default',
    prompt: 'hi',
    cwd: '/repo',
    akashicLevel: 'summary',
  });
  assert.equal(captured.startThread.length, 1);
  assert.equal(captured.startThread[0].sandboxMode, 'danger-full-access');
});

test('A3: request.sandboxMode overrides default and reaches ThreadOptions', async () => {
  const captured = await runWith({
    type: 'run',
    id: 'req-a3-override',
    prompt: 'hi',
    cwd: '/repo',
    sandboxMode: 'read-only',
    akashicLevel: 'summary',
  });
  assert.equal(captured.startThread[0].sandboxMode, 'read-only');
});

test('A4: approvalPolicy defaults to never when request omits it', async () => {
  const captured = await runWith({
    type: 'run',
    id: 'req-a4-default',
    prompt: 'hi',
    cwd: '/repo',
    akashicLevel: 'summary',
  });
  assert.equal(captured.startThread[0].approvalPolicy, 'never');
});

test('A4: request.approvalPolicy overrides default and reaches ThreadOptions', async () => {
  const captured = await runWith({
    type: 'run',
    id: 'req-a4-override',
    prompt: 'hi',
    cwd: '/repo',
    approvalPolicy: 'on-failure',
    akashicLevel: 'summary',
  });
  assert.equal(captured.startThread[0].approvalPolicy, 'on-failure');
});

test('A1: request.model is forwarded to ThreadOptions.model', async () => {
  const captured = await runWith({
    type: 'run',
    id: 'req-a1-model',
    prompt: 'hi',
    cwd: '/repo',
    model: 'gpt-5',
    akashicLevel: 'summary',
  });
  assert.equal(captured.startThread[0].model, 'gpt-5');
});

test('A1: ThreadOptions.model stays unset when request.model is omitted', async () => {
  const captured = await runWith({
    type: 'run',
    id: 'req-a1-omitted',
    prompt: 'hi',
    cwd: '/repo',
    akashicLevel: 'summary',
  });
  assert.equal(captured.startThread[0].model, undefined);
});

test('A1: empty-string request.model leaves ThreadOptions.model unset (SDK default)', async () => {
  const captured = await runWith({
    type: 'run',
    id: 'req-a1-empty',
    prompt: 'hi',
    cwd: '/repo',
    model: '   ',
    akashicLevel: 'summary',
  });
  assert.equal(captured.startThread[0].model, undefined);
});

test('A2: request.effort is forwarded as ThreadOptions.modelReasoningEffort (codex)', async () => {
 for (const effort of ['minimal', 'low', 'medium', 'high', 'xhigh'] as const) {
   const captured = await runWith({
      type: 'run',
      id: `req-a2-${effort}`,
      prompt: 'hi',
      cwd: '/repo',
      effort,
      akashicLevel: 'summary',
    });
    assert.equal(
      captured.startThread[0].modelReasoningEffort,
      effort,
      `effort=${effort} should reach ThreadOptions.modelReasoningEffort`,
    );
 }
});

test('A2: system_init exposes resolved model and reasoning effort for Akashic persistence', async () => {
  const events = await runWithEvents({
    type: 'run',
    id: 'req-a2-system-init',
    prompt: 'hi',
    cwd: '/repo',
    model: 'gpt-5.3-codex-spark',
    effort: 'high',
    akashicLevel: 'summary',
  });
  const systemInit = events.find((event) => event.type === 'system_init');
  assert.ok(systemInit, 'system_init event should be emitted');
  const systemInfo = systemInit.systemInfo as Record<string, unknown>;
  assert.equal(systemInfo.sessionId, 'thread-x');
  assert.equal(systemInfo.cwd, '/repo');
  assert.equal(systemInfo.model, 'gpt-5.3-codex-spark');
  assert.equal(systemInfo.modelReasoningEffort, 'high');
  assert.equal(systemInfo.permissionMode, 'never');
});

test('A2: ThreadOptions.modelReasoningEffort stays unset when request.effort is omitted', async () => {
 const captured = await runWith({
   type: 'run',
    id: 'req-a2-omitted',
    prompt: 'hi',
    cwd: '/repo',
    akashicLevel: 'summary',
  });
  assert.equal(captured.startThread[0].modelReasoningEffort, undefined);
});

test('A7: additionalDirectories defaults to ["/"] when request omits it (default-allow)', async () => {
  const captured = await runWith({
    type: 'run',
    id: 'req-a7-default',
    prompt: 'hi',
    cwd: '/repo',
    akashicLevel: 'summary',
  });
  assert.deepEqual(captured.startThread[0].additionalDirectories, ['/']);
});

test('A7: request.additionalDirectories overrides default and reaches ThreadOptions', async () => {
  const captured = await runWith({
    type: 'run',
    id: 'req-a7-override',
    prompt: 'hi',
    cwd: '/repo',
    additionalDirectories: ['/usr/local/lib', '/opt/data'],
    akashicLevel: 'summary',
  });
  assert.deepEqual(
    captured.startThread[0].additionalDirectories,
    ['/usr/local/lib', '/opt/data'],
  );
});

test('A7: explicit empty array is honored as restrict-intent (no default-allow injection)', async () => {
  const captured = await runWith({
    type: 'run',
    id: 'req-a7-empty',
    prompt: 'hi',
    cwd: '/repo',
    additionalDirectories: [],
    akashicLevel: 'summary',
  });
  assert.deepEqual(captured.startThread[0].additionalDirectories, []);
});

test('N3: approvalPolicy non-never emits approval_policy_warning observation', async () => {
  const events = await runWithEvents({
    type: 'run',
    id: 'req-n3-warn',
    prompt: 'hi',
    cwd: '/repo',
    approvalPolicy: 'on-failure',
    akashicLevel: 'summary',
  });
  const warning = events.find((e) =>
    e.type === 'observation'
    && e.observationType === 'approval_policy_warning',
  );
  assert.ok(warning, 'approval_policy_warning observation should be emitted');
  const payload = (warning as { payload?: { policy?: string } }).payload;
  assert.equal(payload?.policy, 'on-failure');
});

test('N3: approvalPolicy never does not emit approval_policy_warning', async () => {
  const events = await runWithEvents({
    type: 'run',
    id: 'req-n3-never',
    prompt: 'hi',
    cwd: '/repo',
    akashicLevel: 'summary',
  });
  const warning = events.find((e) =>
    e.type === 'observation'
    && e.observationType === 'approval_policy_warning',
  );
  assert.equal(warning, undefined);
});
