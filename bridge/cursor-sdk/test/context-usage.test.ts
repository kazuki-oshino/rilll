import assert from 'node:assert/strict';
import test from 'node:test';
import type {
  ModelSelection,
  Run,
  SDKAgent,
  SDKMessage,
  SendOptions,
} from '@cursor/sdk';
import {
  computeContextUsage,
  CONTEXT_SIZE_TABLE,
  parseContextString,
  resolveContextWindow,
} from '../src/context-usage.js';
import { runCursor, type CursorRuntimeDeps } from '../src/index.js';
import type { ModelListItem } from '@cursor/sdk';

type BridgeEvent = Record<string, unknown>;
type DeltaUpdate = Parameters<NonNullable<SendOptions['onDelta']>>[0]['update'];

test('resolveContextWindow resolves from SDK context parameter default variant', () => {
  const modelsList: ModelListItem[] = [
    {
      id: 'gpt-5.5',
      displayName: 'GPT-5.5',
      parameters: [
        {
          id: 'context',
          values: [{ value: '272k' }, { value: '1m' }],
        },
      ],
      variants: [
        {
          displayName: 'Long context',
          params: [{ id: 'context', value: '1m' }],
        },
        {
          displayName: 'Default context',
          isDefault: true,
          params: [{ id: 'context', value: '272k' }],
        },
      ],
    },
  ];

  assert.deepEqual(resolveContextWindow('gpt-5.5', modelsList), {
    contextWindow: 272000,
    resolution: 'sdk_parameters',
  });
});

test('resolveContextWindow falls back to first variant when no SDK default variant exists', () => {
  const modelsList: ModelListItem[] = [
    {
      id: 'gpt-5.5',
      displayName: 'GPT-5.5',
      parameters: [
        {
          id: 'context',
          values: [{ value: '272k' }, { value: '1m' }],
        },
      ],
      variants: [
        {
          displayName: 'First context',
          params: [{ id: 'context', value: '1m' }],
        },
        {
          displayName: 'Second context',
          params: [{ id: 'context', value: '272k' }],
        },
      ],
    },
  ];

  assert.deepEqual(resolveContextWindow('gpt-5.5', modelsList), {
    contextWindow: 1000000,
    resolution: 'sdk_parameters',
  });
});

test('resolveContextWindow uses fallback table only after SDK parameters miss', () => {
  assert.deepEqual(CONTEXT_SIZE_TABLE, {});
  assert.equal(resolveContextWindow('custom-model', []), undefined);

  assert.deepEqual(
    resolveContextWindow('custom-model', [], {
      'custom-model': 123456,
    }),
    {
      contextWindow: 123456,
      resolution: 'fallback_table',
    },
  );
});

test('resolveContextWindow uses fallback table when SDK context variants are unusable', () => {
  const modelsList: ModelListItem[] = [
    {
      id: 'custom-model',
      displayName: 'Custom Model',
      parameters: [
        {
          id: 'context',
          values: [{ value: '272k' }],
        },
      ],
      variants: [
        {
          displayName: 'Default',
          isDefault: true,
          params: [{ id: 'reasoning', value: 'high' }],
        },
      ],
    },
  ];

  assert.deepEqual(
    resolveContextWindow('custom-model', modelsList, {
      'custom-model': 123456,
    }),
    {
      contextWindow: 123456,
      resolution: 'fallback_table',
    },
  );
});

test('resolveContextWindow returns undefined when SDK parameters and fallback table both miss', () => {
  const modelsList: ModelListItem[] = [
    {
      id: 'gpt-5.5',
      displayName: 'GPT-5.5',
      parameters: [
        {
          id: 'reasoning',
          values: [{ value: 'high' }],
        },
      ],
      variants: [
        {
          displayName: 'Default',
          isDefault: true,
          params: [{ id: 'reasoning', value: 'high' }],
        },
      ],
    },
  ];

  assert.equal(resolveContextWindow('gpt-5.5', modelsList), undefined);
});

test('parseContextString parses supported k and m token suffixes', () => {
  assert.equal(parseContextString('272k'), 272000);
  assert.equal(parseContextString('1m'), 1000000);
  assert.equal(parseContextString('1.5m'), 1500000);
  assert.equal(parseContextString('bad'), undefined);
  assert.equal(parseContextString('272'), undefined);
});

test('computeContextUsage uses cumulative input plus cache tokens and excludes output tokens', () => {
  assert.deepEqual(
    computeContextUsage({
      modelId: 'gpt-5.5',
      contextWindow: 1000,
      resolution: 'sdk_parameters',
      usageTotals: {
        inputTokens: 100,
        outputTokens: 900,
        cacheReadTokens: 200,
        cacheWriteTokens: 300,
      },
    }),
    {
      model_id: 'gpt-5.5',
      context_window: 1000,
      used_tokens: 600,
      usage_percent: 60,
      resolution: 'sdk_parameters',
    },
  );
});

test('computeContextUsage clamps usage_percent into the 0-100 range', () => {
  assert.equal(
    computeContextUsage({
      modelId: 'zero',
      contextWindow: 1000,
      resolution: 'fallback_table',
      usageTotals: {
        inputTokens: 0,
        outputTokens: 100,
        cacheReadTokens: 0,
        cacheWriteTokens: 0,
      },
    }).usage_percent,
    0,
  );

  assert.equal(
    computeContextUsage({
      modelId: 'within',
      contextWindow: 1000,
      resolution: 'fallback_table',
      usageTotals: {
        inputTokens: 250,
        outputTokens: 0,
        cacheReadTokens: 0,
        cacheWriteTokens: 0,
      },
    }).usage_percent,
    25,
  );

  assert.equal(
    computeContextUsage({
      modelId: 'over',
      contextWindow: 1000,
      resolution: 'fallback_table',
      usageTotals: {
        inputTokens: 1001,
        outputTokens: 0,
        cacheReadTokens: 0,
        cacheWriteTokens: 0,
      },
    }).usage_percent,
    100,
  );
});

async function runCursorForContextUsage(input: {
  readonly modelId: string;
  readonly modelsList: readonly ModelListItem[];
  readonly deltas: readonly DeltaUpdate[];
}): Promise<BridgeEvent[]> {
  const chunks: string[] = [];
  const run: Run = {
    async *stream(): AsyncGenerator<SDKMessage> {
      yield {
        type: 'assistant',
        message: {
          content: [{ type: 'text', text: 'ok' }],
        },
      } as SDKMessage;
    },
    async wait() {
      return {
        status: 'completed',
        result: 'ok',
        durationMs: 1,
      };
    },
    async cancel() {},
  } as unknown as Run;

  const agent: SDKAgent = {
    agentId: 'agent-ctx',
    async send(_prompt: string, options?: SendOptions) {
      for (const update of input.deltas) {
        options?.onDelta?.({ update });
      }
      return run;
    },
    close() {},
  } as SDKAgent;

  const deps: CursorRuntimeDeps = {
    env: {},
    write: (chunk) => {
      chunks.push(chunk);
    },
    cursor: {
      models: {
        async list() {
          return input.modelsList as ModelListItem[];
        },
      },
    },
    agent: {
      async resume() {
        return agent;
      },
      async create(options) {
        const model = options.model as ModelSelection;
        assert.equal(model.id, input.modelId);
        return agent;
      },
    },
  };

  await runCursor({
    type: 'run',
    id: 'req-context',
    prompt: 'hello',
    cwd: '/repo',
    model: input.modelId,
    akashicLevel: 'summary',
  }, deps);

  return chunks.join('').trim().split('\n').filter(Boolean).map((line) => JSON.parse(line) as BridgeEvent);
}

function controlEvents(events: readonly BridgeEvent[], observationType: string): BridgeEvent[] {
  return events.filter(
    (event) =>
      event.type === 'observation' &&
      event.source === 'cursor-control' &&
      event.observationType === observationType,
  );
}

const modelWithSdkContext: ModelListItem = {
  id: 'gpt-5.5',
  displayName: 'GPT-5.5',
  parameters: [
    {
      id: 'context',
      values: [{ value: '1m' }],
    },
  ],
  variants: [
    {
      displayName: 'Default',
      isDefault: true,
      params: [{ id: 'context', value: '1m' }],
    },
  ],
};

test('runCursor emits context_usage after a turn-ended usage delta', async () => {
  const events = await runCursorForContextUsage({
    modelId: 'gpt-5.5',
    modelsList: [modelWithSdkContext],
    deltas: [
      {
        type: 'turn-ended',
        usage: {
          inputTokens: 1000,
          outputTokens: 999,
          cacheReadTokens: 2000,
          cacheWriteTokens: 3000,
        },
      } as DeltaUpdate,
    ],
  });

  const contextUsageEvents = controlEvents(events, 'context_usage');
  assert.equal(contextUsageEvents.length, 1);
  assert.deepEqual(contextUsageEvents[0].payload, {
    model_id: 'gpt-5.5',
    context_window: 1000000,
    used_tokens: 6000,
    usage_percent: 0.6,
    resolution: 'sdk_parameters',
  });
});

test('runCursor does not emit context usage observations before usage arrives', async () => {
  const events = await runCursorForContextUsage({
    modelId: 'gpt-5.5',
    modelsList: [modelWithSdkContext],
    deltas: [
      {
        type: 'turn-ended',
      } as DeltaUpdate,
    ],
  });

  assert.equal(controlEvents(events, 'context_usage').length, 0);
  assert.equal(controlEvents(events, 'context_usage_error').length, 0);
});

test('runCursor emits context_usage_error once for unknown context window', async () => {
  const events = await runCursorForContextUsage({
    modelId: 'unknown-model',
    modelsList: [{ id: 'unknown-model', displayName: 'Unknown', parameters: [] }],
    deltas: [
      {
        type: 'turn-ended',
        usage: { inputTokens: 100 },
      } as DeltaUpdate,
      {
        type: 'turn-ended',
        usage: { inputTokens: 200 },
      } as DeltaUpdate,
    ],
  });

  const errorEvents = controlEvents(events, 'context_usage_error');
  assert.equal(errorEvents.length, 1);
  assert.deepEqual(errorEvents[0].payload, {
    model_id: 'unknown-model',
    reason: 'unknown_model',
  });
  assert.equal(controlEvents(events, 'context_usage').length, 0);
});
