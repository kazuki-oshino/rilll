import assert from 'node:assert/strict';
import test from 'node:test';
import {
  pickThinkingValue,
  resolveModelSelection,
  resolvePreferredModelOptions,
} from '../src/model.js';

test('pickThinkingValue prefers exact match', () => {
  const values = [
    { value: 'low' },
    { value: 'high' },
    { value: 'extra-high', displayName: 'Extra High' },
  ];
  assert.equal(pickThinkingValue(values, 'extra-high'), 'extra-high');
});

test('pickThinkingValue matches case-insensitively', () => {
  const values = [
    { value: 'Low' },
    { value: 'High' },
    { value: 'Extra-High' },
  ];
  assert.equal(pickThinkingValue(values, 'extra-high'), 'Extra-High');
});

test('pickThinkingValue resolves "extra high" preference via substring', () => {
  // 公式値が "extra-high" の代わりに "xhigh" や "extraHigh" だった場合でも拾える
  const xhighValues = [
    { value: 'low' },
    { value: 'high' },
    { value: 'xhigh', displayName: 'Extra High' },
  ];
  assert.equal(pickThinkingValue(xhighValues, 'extra-high'), 'xhigh');

  const extraHighValues = [
    { value: 'low' },
    { value: 'high' },
    { value: 'extraHigh' },
  ];
  assert.equal(pickThinkingValue(extraHighValues, 'extra-high'), 'extraHigh');
});

// rilll の preset 命名は `gpt-5.5-xhigh` で preferred="xhigh" を渡す。
// values 側に "extra-high" / "extraHigh" / displayName "Extra High" がある場合に
// xhigh を正しく拾えること (preferred="xhigh" の extra マッチ経路リグレッション防止)。
test('pickThinkingValue resolves preferred="xhigh" via extra match instead of falling back to high', () => {
  const extraHighDashValues = [
    { value: 'low' },
    { value: 'medium' },
    { value: 'high' },
    { value: 'extra-high' },
  ];
  assert.equal(pickThinkingValue(extraHighDashValues, 'xhigh'), 'extra-high');

  const extraHighCamelValues = [
    { value: 'low' },
    { value: 'high' },
    { value: 'extraHigh' },
  ];
  assert.equal(pickThinkingValue(extraHighCamelValues, 'xhigh'), 'extraHigh');

  const displayNameValues = [
    { value: 'low' },
    { value: 'high' },
    { value: 'r1', displayName: 'Extra High' },
  ];
  assert.equal(pickThinkingValue(displayNameValues, 'xhigh'), 'r1');
});

test('pickThinkingValue falls back to high when extra-high unavailable', () => {
  const values = [
    { value: 'low' },
    { value: 'high' },
  ];
  assert.equal(pickThinkingValue(values, 'extra-high'), 'high');
});

test('pickThinkingValue falls back to first value when nothing matches', () => {
  const values = [
    { value: 'medium' },
    { value: 'low' },
  ];
  assert.equal(pickThinkingValue(values, 'extra-high'), 'medium');
});

test('pickThinkingValue returns preferred when value list is empty', () => {
  assert.equal(pickThinkingValue([], 'extra-high'), 'extra-high');
});

test('resolveModelSelection uses reasoning parameter when thinking is unavailable', async () => {
  const result = await resolveModelSelection({
    preferredModelId: 'gpt-5.5',
    preferredThinking: 'high',
    listModels: async () => [
      {
        id: 'gpt-5.5',
        displayName: 'GPT-5.5',
        parameters: [
          {
            id: 'reasoning',
            values: [
              { value: 'medium' },
              { value: 'high' },
              { value: 'extra-high' },
            ],
          },
        ],
      },
    ],
  });

  assert.deepEqual(result.model, {
    id: 'gpt-5.5',
    params: [{ id: 'reasoning', value: 'high' }],
  });
});

test('resolvePreferredModelOptions uses requested model without reasoning override', () => {
  assert.deepEqual(
    resolvePreferredModelOptions({
      requestedModelId: 'composer-2.5',
      defaultModelId: 'gpt-5.5',
      defaultThinking: 'high',
    }),
    {
      preferredModelId: 'composer-2.5',
      preferredThinking: undefined,
    },
  );
});

test('resolveModelSelection uses explicit fast=false when requestedParams specify it', async () => {
  const result = await resolveModelSelection({
    preferredModelId: 'composer-2.5',
    preferredThinking: undefined,
    requestedParams: [{ id: 'fast', value: 'false' }],
    listModels: async () => [
      {
        id: 'composer-2.5',
        displayName: 'Composer 2.5',
        parameters: [
          {
            id: 'fast',
            values: [{ value: 'true' }, { value: 'false' }],
          },
        ],
        variants: [
          {
            params: [{ id: 'fast', value: 'true' }],
            displayName: 'Composer 2.5',
            isDefault: true,
          },
        ],
      },
    ],
  });

  assert.deepEqual(result.model, { id: 'composer-2.5', params: [{ id: 'fast', value: 'false' }] });
});

test('resolveModelSelection uses default variant params when preferred thinking is undefined', async () => {
  const result = await resolveModelSelection({
    preferredModelId: 'composer-2.5',
    preferredThinking: undefined,
    listModels: async () => [
      {
        id: 'composer-2.5',
        displayName: 'Composer 2.5',
        parameters: [
          {
            id: 'fast',
            values: [{ value: 'true' }, { value: 'false' }],
          },
        ],
        variants: [
          {
            params: [{ id: 'fast', value: 'true' }],
            displayName: 'Composer 2.5',
            isDefault: true,
          },
          {
            params: [{ id: 'fast', value: 'false' }],
            displayName: 'Composer 2.5',
          },
        ],
      },
    ],
  });

  assert.deepEqual(result.model, { id: 'composer-2.5', params: [{ id: 'fast', value: 'true' }] });
});

test('resolveModelSelection preserves explicitly requested model params', async () => {
  const options = {
    preferredModelId: 'gpt-5.6-luna',
    preferredThinking: undefined,
    requestedParams: [{ id: 'reasoning', value: 'max' }],
    listModels: async () => [
      {
        id: 'gpt-5.6-luna',
        displayName: 'GPT-5.6 Luna',
        parameters: [
          {
            id: 'reasoning',
            values: [{ value: 'none' }, { value: 'max' }],
          },
        ],
      },
    ],
  };

  const result = await resolveModelSelection(options);

  assert.deepEqual(result.model, {
    id: 'gpt-5.6-luna',
    params: [{ id: 'reasoning', value: 'max' }],
  });
});

test('resolveModelSelection omits params when preferred thinking is undefined and no default variant exists', async () => {
  const result = await resolveModelSelection({
    preferredModelId: 'composer-2.5',
    preferredThinking: undefined,
    listModels: async () => [
      {
        id: 'composer-2.5',
        displayName: 'Composer 2.5',
        parameters: [
          {
            id: 'fast',
            values: [{ value: 'true' }, { value: 'false' }],
          },
        ],
      },
    ],
  });

  assert.deepEqual(result.model, { id: 'composer-2.5' });
});

test('resolveModelSelection selects grok-4.5 default variant with effort=high and fast=true', async () => {
  const result = await resolveModelSelection({
    preferredModelId: 'grok-4.5',
    preferredThinking: undefined,
    listModels: async () => [
      {
        id: 'grok-4.5',
        displayName: 'Cursor Grok 4.5',
        parameters: [
          {
            id: 'effort',
            values: [{ value: 'low' }, { value: 'medium' }, { value: 'high' }],
          },
          {
            id: 'fast',
            values: [{ value: 'false' }, { value: 'true' }],
          },
        ],
        variants: [
          {
            params: [
              { id: 'effort', value: 'high' },
              { id: 'fast', value: 'true' },
            ],
            displayName: 'Cursor Grok 4.5',
            isDefault: true,
          },
          {
            params: [
              { id: 'effort', value: 'low' },
              { id: 'fast', value: 'false' },
            ],
            displayName: 'Cursor Grok 4.5',
          },
        ],
      },
    ],
  });

  assert.deepEqual(result.model, {
    id: 'grok-4.5',
    params: [
      { id: 'effort', value: 'high' },
      { id: 'fast', value: 'true' },
    ],
  });
});

test('resolveModelSelection exposes models list and calls SDK only once', async () => {
  let listCallCount = 0;
  const modelsList = [
    {
      id: 'gpt-5.5',
      displayName: 'GPT-5.5',
      parameters: [
        {
          id: 'reasoning',
          values: [
            { value: 'medium' },
            { value: 'high' },
          ],
        },
      ],
    },
  ];

  const result = await resolveModelSelection({
    preferredModelId: 'gpt-5.5',
    preferredThinking: 'high',
    listModels: async () => {
      listCallCount += 1;
      return modelsList;
    },
  });

  assert.equal(listCallCount, 1);
  assert.deepEqual(result.model, {
    id: 'gpt-5.5',
    params: [{ id: 'reasoning', value: 'high' }],
  });
  assert.strictEqual(result.modelsList, modelsList);
});
