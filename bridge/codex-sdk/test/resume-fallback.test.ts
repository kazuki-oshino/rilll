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

type BridgeEvent = Record<string, unknown>;

interface FakeThreadOptions {
  readonly threadId: string;
  readonly events: readonly ThreadEvent[];
  readonly runStreamedThrows?: () => never | Promise<never>;
}

function makeThread(opts: FakeThreadOptions): Thread {
  const thread = {
    async runStreamed(_input: unknown, _turnOptions?: unknown) {
      if (opts.runStreamedThrows) {
        await opts.runStreamedThrows();
      }
      return {
        events: (async function* (): AsyncGenerator<ThreadEvent> {
          for (const ev of opts.events) {
            yield ev;
          }
        })(),
      };
    },
    async run() {
      throw new Error('not used in tests');
    },
    get id(): string | null {
      return opts.threadId;
    },
  };
  return thread as unknown as Thread;
}

function defaultHappyEvents(threadId: string): ThreadEvent[] {
  return [
    { type: 'thread.started', thread_id: threadId },
    {
      type: 'item.completed',
      item: { id: 'msg-1', type: 'agent_message', text: `hello from ${threadId}` },
    },
    {
      type: 'turn.completed',
      usage: {
        input_tokens: 100,
        cached_input_tokens: 0,
        cache_write_input_tokens: 0,
        output_tokens: 50,
        reasoning_output_tokens: 0,
      },
    },
  ];
}

interface RunWithFakeSdkInput {
  readonly resumeSessionId?: string;
  readonly resumeImpl?: (id: string, options?: ThreadOptions) => Thread;
  readonly startImpl?: (options?: ThreadOptions) => Thread;
  readonly model?: string;
}

async function runWithFakeSdk(input: RunWithFakeSdkInput): Promise<{
  events: BridgeEvent[];
  calls: string[];
}> {
  const chunks: string[] = [];
  const calls: string[] = [];

  const fakeCodex: Codex = {
    startThread(options?: ThreadOptions): Thread {
      calls.push(`startThread:${options?.workingDirectory ?? ''}`);
      if (input.startImpl) return input.startImpl(options);
      return makeThread({
        threadId: 'fresh-thread',
        events: defaultHappyEvents('fresh-thread'),
      });
    },
    resumeThread(id: string, options?: ThreadOptions): Thread {
      calls.push(`resumeThread:${id}:${options?.workingDirectory ?? ''}`);
      if (input.resumeImpl) return input.resumeImpl(id, options);
      return makeThread({
        threadId: id,
        events: defaultHappyEvents(id),
      });
    },
  } as unknown as Codex;

  const deps: CodexRuntimeDeps = {
    env: {},
    write: (chunk) => {
      chunks.push(chunk);
    },
    createCodex: (_options: CodexOptions) => fakeCodex,
  };

  await runCodex({
    type: 'run',
    id: 'req-1',
    prompt: 'hello',
    cwd: '/repo',
    resumeSessionId: input.resumeSessionId,
    akashicLevel: 'summary',
    model: input.model,
  }, deps);

  return {
    events: chunks.join('').trim().split('\n').filter(Boolean)
      .map((line) => JSON.parse(line) as BridgeEvent),
    calls,
  };
}

function resumeFallbackObservations(events: BridgeEvent[]): BridgeEvent[] {
  return events.filter(
    (event) =>
      event.type === 'observation' &&
      event.source === 'codex-sdk' &&
      event.observationType === 'resume_fallback',
  );
}

test('runCodex resumes the requested thread when resumeThread succeeds', async () => {
  const { events, calls } = await runWithFakeSdk({
    resumeSessionId: 'old-thread',
  });

  assert.equal(calls.some((c) => c.startsWith('resumeThread:old-thread:')), true);
  assert.equal(calls.some((c) => c.startsWith('startThread:')), false);
  assert.equal(resumeFallbackObservations(events).length, 0);
  assert.equal(
    events.some((event) => event.type === 'done' && event.sessionId === 'old-thread'),
    true,
  );
});

test('runCodex falls back to startThread and emits resume_fallback when resumeThread throws synchronously', async () => {
  const { events, calls } = await runWithFakeSdk({
    resumeSessionId: 'old-thread',
    resumeImpl: () => {
      throw new Error('session expired');
    },
  });

  assert.equal(calls.some((c) => c.startsWith('resumeThread:old-thread:')), true);
  assert.equal(calls.some((c) => c.startsWith('startThread:')), true);

  const fallbacks = resumeFallbackObservations(events);
  assert.equal(fallbacks.length, 1);
  assert.deepEqual(fallbacks[0].payload, {
    requestedSessionId: 'old-thread',
    fallbackReason: 'session expired',
  });
  assert.equal(
    events.some((event) => event.type === 'done' && event.sessionId === 'fresh-thread'),
    true,
  );
});

test('runCodex falls back when thread.runStreamed throws asynchronously', async () => {
  const { events, calls } = await runWithFakeSdk({
    resumeSessionId: 'old-thread',
    resumeImpl: () =>
      makeThread({
        threadId: 'old-thread',
        events: [],
        runStreamedThrows: async () => {
          throw new Error('thread not found');
        },
      }),
  });

  assert.equal(calls.some((c) => c.startsWith('resumeThread:old-thread:')), true);
  assert.equal(calls.some((c) => c.startsWith('startThread:')), true);

  const fallbacks = resumeFallbackObservations(events);
  assert.equal(fallbacks.length, 1);
  assert.deepEqual(fallbacks[0].payload, {
    requestedSessionId: 'old-thread',
    fallbackReason: 'thread not found',
  });
  assert.equal(
    events.some((event) => event.type === 'done' && event.sessionId === 'fresh-thread'),
    true,
  );
});

test('runCodex does not emit resume_fallback for a fresh session', async () => {
  const { events, calls } = await runWithFakeSdk({});

  assert.equal(calls.some((c) => c.startsWith('resumeThread:')), false);
  assert.equal(calls.some((c) => c.startsWith('startThread:')), true);
  assert.equal(resumeFallbackObservations(events).length, 0);
  assert.equal(
    events.some((event) => event.type === 'done' && event.sessionId === 'fresh-thread'),
    true,
  );
});
