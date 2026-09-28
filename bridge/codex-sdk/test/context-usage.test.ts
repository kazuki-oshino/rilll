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
}

function makeThread(opts: FakeThreadOptions): Thread {
  const thread = {
    async runStreamed(_input: unknown, _turnOptions?: unknown) {
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

interface TurnUsage {
  readonly input_tokens: number;
  readonly cached_input_tokens?: number;
  readonly output_tokens?: number;
  readonly reasoning_output_tokens?: number;
}

interface RunInput {
  readonly model?: string;
  readonly env?: Readonly<Record<string, string | undefined>>;
  readonly turnUsages: readonly TurnUsage[];
}

async function runForContextUsage(input: RunInput): Promise<BridgeEvent[]> {
  const chunks: string[] = [];

  const fakeEvents: ThreadEvent[] = [
    { type: 'thread.started', thread_id: 'thread-ctx' },
  ];
  for (const usage of input.turnUsages) {
    fakeEvents.push({
      type: 'turn.completed',
      usage: {
        input_tokens: usage.input_tokens,
        cached_input_tokens: usage.cached_input_tokens ?? 0,
        cache_write_input_tokens: 0,
        output_tokens: usage.output_tokens ?? 0,
        reasoning_output_tokens: usage.reasoning_output_tokens ?? 0,
      },
    });
  }
  fakeEvents.push({
    type: 'item.completed',
    item: { id: 'msg-final', type: 'agent_message', text: 'final' },
  });

  const fakeCodex: Codex = {
    startThread(_options?: ThreadOptions): Thread {
      return makeThread({ threadId: 'thread-ctx', events: fakeEvents });
    },
    resumeThread(_id: string, _options?: ThreadOptions): Thread {
      return makeThread({ threadId: 'thread-ctx', events: fakeEvents });
    },
  } as unknown as Codex;

  const deps: CodexRuntimeDeps = {
    env: input.env ?? {},
    write: (chunk) => {
      chunks.push(chunk);
    },
    createCodex: (_options: CodexOptions) => fakeCodex,
  };

  await runCodex({
    type: 'run',
    id: 'req-ctx',
    prompt: 'hi',
    cwd: '/repo',
    model: input.model,
    akashicLevel: 'summary',
  }, deps);

  return chunks.join('').trim().split('\n').filter(Boolean)
    .map((line) => JSON.parse(line) as BridgeEvent);
}

function contextUsageObservations(events: BridgeEvent[]): BridgeEvent[] {
  return events.filter(
    (event) =>
      event.type === 'observation' &&
      event.source === 'codex-sdk' &&
      event.observationType === 'context_usage',
  );
}

function contextUsageErrorObservations(events: BridgeEvent[]): BridgeEvent[] {
  return events.filter(
    (event) =>
      event.type === 'observation' &&
      event.source === 'codex-sdk' &&
      event.observationType === 'context_usage_error',
  );
}

test('runCodex emits a context_usage observation per turn.completed for a known model', async () => {
  const events = await runForContextUsage({
    model: 'gpt-5-codex',
    turnUsages: [{ input_tokens: 27200 }],
  });

  const observations = contextUsageObservations(events);
  assert.equal(observations.length, 1);
  assert.equal(observations[0].observationSubtype, 'estimated');
  assert.deepEqual(observations[0].payload, {
    model_id: 'gpt-5-codex',
    context_window: 272000,
    cumulative_input_tokens: 27200,
    usage_percent: 10,
  });
});

test('runCodex accumulates input_tokens across multiple turns within a single run', async () => {
  const events = await runForContextUsage({
    model: 'gpt-5-codex',
    turnUsages: [
      { input_tokens: 50000 },
      { input_tokens: 80000 },
    ],
  });

  const observations = contextUsageObservations(events);
  assert.equal(observations.length, 2);
  assert.deepEqual(observations[0].payload, {
    model_id: 'gpt-5-codex',
    context_window: 272000,
    cumulative_input_tokens: 50000,
    usage_percent: (50000 / 272000) * 100,
  });
  assert.deepEqual(observations[1].payload, {
    model_id: 'gpt-5-codex',
    context_window: 272000,
    cumulative_input_tokens: 130000,
    usage_percent: (130000 / 272000) * 100,
  });
});

test('runCodex emits context_usage at 100% (clamped) once cumulative tokens exceed context window', async () => {
  const events = await runForContextUsage({
    model: 'gpt-5-codex',
    turnUsages: [
      { input_tokens: 200000 },
      { input_tokens: 200000 },
    ],
  });

  const observations = contextUsageObservations(events);
  assert.equal(observations.length, 2);
  assert.equal(observations[1].payload && (observations[1].payload as { usage_percent: number }).usage_percent, 100);
});

test('runCodex emits context_usage_error once and no context_usage for unknown model', async () => {
  const events = await runForContextUsage({
    model: 'unknown-codex-model',
    turnUsages: [
      { input_tokens: 1000 },
      { input_tokens: 2000 },
    ],
  });

  assert.equal(contextUsageObservations(events).length, 0);
  const errors = contextUsageErrorObservations(events);
  assert.equal(errors.length, 1);
  assert.deepEqual(errors[0].payload, {
    model_id: 'unknown-codex-model',
    reason: 'unknown_model',
  });
});

test('runCodex resets cumulative tokens between runs (no cross-run carryover)', async () => {
  // 1st run
  const events1 = await runForContextUsage({
    model: 'gpt-5-codex',
    turnUsages: [{ input_tokens: 27200 }],
  });
  const o1 = contextUsageObservations(events1);
  assert.equal(o1.length, 1);
  assert.equal((o1[0].payload as { cumulative_input_tokens: number }).cumulative_input_tokens, 27200);

  // 2nd run should start over at 0
  const events2 = await runForContextUsage({
    model: 'gpt-5-codex',
    turnUsages: [{ input_tokens: 54400 }],
  });
  const o2 = contextUsageObservations(events2);
  assert.equal(o2.length, 1);
  assert.equal((o2[0].payload as { cumulative_input_tokens: number }).cumulative_input_tokens, 54400);
});

test('runCodex falls back to RILLL_CODEX_MODEL env override when request.model is unspecified', async () => {
  const events = await runForContextUsage({
    env: { RILLL_CODEX_MODEL: 'gpt-5' },
    turnUsages: [{ input_tokens: 40000 }],
  });

  const observations = contextUsageObservations(events);
  assert.equal(observations.length, 1);
  assert.deepEqual(observations[0].payload, {
    model_id: 'gpt-5',
    context_window: 400000,
    cumulative_input_tokens: 40000,
    usage_percent: 10,
  });
});

test('runCodex defaults to gpt-5-codex when neither request.model nor env override is set', async () => {
  const events = await runForContextUsage({
    turnUsages: [{ input_tokens: 13600 }],
  });

  const observations = contextUsageObservations(events);
  assert.equal(observations.length, 1);
  assert.deepEqual(observations[0].payload, {
    model_id: 'gpt-5-codex',
    context_window: 272000,
    cumulative_input_tokens: 13600,
    usage_percent: 5,
  });
});
