import assert from 'node:assert/strict';
import test from 'node:test';
import type {
  ModelSelection,
  Run,
  SDKAgent,
  SDKMessage,
  SendOptions,
} from '@cursor/sdk';
import { runCursor, type CursorRuntimeDeps } from '../src/index.js';

type BridgeEvent = Record<string, unknown>;
type ResumeOptions = NonNullable<Parameters<CursorRuntimeDeps['agent']['resume']>[1]>;
type CreateOptions = NonNullable<Parameters<CursorRuntimeDeps['agent']['create']>[0]>;

function makeAgent(agentId: string): SDKAgent {
  const run: Run = {
    async *stream(): AsyncGenerator<SDKMessage> {
      yield {
        type: 'assistant',
        message: {
          content: [{ type: 'text', text: `hello from ${agentId}` }],
        },
      } as SDKMessage;
    },
    async wait() {
      return {
        status: 'completed',
        result: `final from ${agentId}`,
        durationMs: 123,
      };
    },
    async cancel() {},
  } as unknown as Run;

  return {
    agentId,
    async send(_prompt: string, _options?: SendOptions) {
      return run;
    },
    close() {},
  } as SDKAgent;
}

async function runWithFakeSdk(input: {
  readonly resumeSessionId?: string;
  readonly resume?: (sessionId: string, options: ResumeOptions) => Promise<SDKAgent>;
  readonly create?: (options: CreateOptions) => Promise<SDKAgent>;
}): Promise<{ events: BridgeEvent[]; calls: string[] }> {
  const chunks: string[] = [];
  const calls: string[] = [];
  const deps: CursorRuntimeDeps = {
    env: {},
    write: (chunk) => {
      chunks.push(chunk);
    },
    cursor: {
      models: {
        async list() {
          calls.push('models.list');
          return [{ id: 'composer-2.5', displayName: 'composer-2.5', parameters: [] }];
        },
      },
    },
    agent: {
      async resume(sessionId, options) {
        if (!options?.model) throw new Error('test fixture expected resume model');
        calls.push(`resume:${sessionId}:${options.model.id}`);
        if (!input.resume) return makeAgent('resumed-agent');
        return input.resume(sessionId, options);
      },
      async create(options) {
        if (!options.model || !options.local) throw new Error('test fixture expected create options');
        calls.push(`create:${options.model.id}:${options.local.cwd}`);
        if (!input.create) return makeAgent('created-agent');
        return input.create(options);
      },
    },
  };

  await runCursor({
    type: 'run',
    id: 'req-1',
    prompt: 'hello',
    cwd: '/repo',
    resumeSessionId: input.resumeSessionId,
    akashicLevel: 'summary',
  }, deps);

  return {
    events: chunks.join('').trim().split('\n').filter(Boolean).map((line) => JSON.parse(line) as BridgeEvent),
    calls,
  };
}

function resumeFallbackEvents(events: BridgeEvent[]): BridgeEvent[] {
  return events.filter(
    (event) =>
      event.type === 'observation' &&
      event.source === 'cursor-control' &&
      event.observationType === 'resume_fallback',
  );
}

test('runCursor keeps the existing session when Agent.resume succeeds', async () => {
  const { events, calls } = await runWithFakeSdk({
    resumeSessionId: 'old-session',
    resume: async () => makeAgent('resumed-agent'),
  });

  assert.equal(calls.some((call) => call.startsWith('resume:old-session:')), true);
  assert.equal(calls.some((call) => call.startsWith('create:')), false);
  assert.equal(resumeFallbackEvents(events).length, 0);
  assert.equal(
    events.some((event) => event.type === 'done' && event.sessionId === 'resumed-agent'),
    true,
  );
});

test('runCursor falls back to Agent.create and emits resume_fallback when Agent.resume fails', async () => {
  const { events, calls } = await runWithFakeSdk({
    resumeSessionId: 'old-session',
    resume: async () => {
      throw new Error('session expired');
    },
    create: async () => makeAgent('new-agent'),
  });

  assert.equal(calls.some((call) => call.startsWith('resume:old-session:')), true);
  assert.equal(calls.some((call) => call.startsWith('create:composer-2.5:/repo')), true);

  const fallbackEvents = resumeFallbackEvents(events);
  assert.equal(fallbackEvents.length, 1);
  assert.deepEqual(fallbackEvents[0].payload, {
    requested_session_id: 'old-session',
    error_message: 'session expired',
  });
  assert.equal(
    events.some(
      (event) =>
        event.type === 'system_init' &&
        (event.systemInfo as { sessionId?: string } | undefined)?.sessionId === 'new-agent',
    ),
    true,
  );
  assert.equal(
    events.some((event) => event.type === 'done' && event.sessionId === 'new-agent'),
    true,
  );
});

test('runCursor reports protocol error after resume and fallback create both fail', async () => {
  const { events } = await runWithFakeSdk({
    resumeSessionId: 'old-session',
    resume: async () => {
      throw new Error('session expired');
    },
    create: async () => {
      throw new Error('create failed');
    },
  });

  assert.equal(resumeFallbackEvents(events).length, 1);
  assert.equal(
    events.some((event) => event.type === 'error' && event.message === 'create failed'),
    true,
  );
  assert.equal(events.some((event) => event.type === 'done'), false);
});

test('runCursor does not emit resume_fallback for a new session run', async () => {
  const { events, calls } = await runWithFakeSdk({});

  assert.equal(calls.some((call) => call.startsWith('resume:')), false);
  assert.equal(calls.some((call) => call.startsWith('create:composer-2.5:/repo')), true);
  assert.equal(resumeFallbackEvents(events).length, 0);
});
