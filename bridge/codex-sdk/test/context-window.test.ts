import assert from 'node:assert/strict';
import test from 'node:test';
import {
  CONTEXT_SIZE_TABLE,
  computeContextUsage,
  resolveContextWindow,
} from '../src/context-window.js';

test('CONTEXT_SIZE_TABLE covers gpt-5 series and gpt-5-codex', () => {
  assert.equal(CONTEXT_SIZE_TABLE['gpt-5'], 400000);
  assert.equal(CONTEXT_SIZE_TABLE['gpt-5-mini'], 400000);
  assert.equal(CONTEXT_SIZE_TABLE['gpt-5-nano'], 400000);
  assert.equal(CONTEXT_SIZE_TABLE['gpt-5-codex'], 272000);
});

test('resolveContextWindow returns table entry for known model', () => {
  assert.deepEqual(resolveContextWindow('gpt-5-codex'), { contextWindow: 272000 });
  assert.deepEqual(resolveContextWindow('gpt-5'), { contextWindow: 400000 });
});

test('resolveContextWindow returns undefined for unknown model', () => {
  assert.equal(resolveContextWindow('unknown-model'), undefined);
  assert.equal(resolveContextWindow(''), undefined);
});

test('resolveContextWindow uses injected table when provided', () => {
  const table = { 'custom-model': 123456 };
  assert.deepEqual(resolveContextWindow('custom-model', table), { contextWindow: 123456 });
  // 既定 table は依然 fallback しない (注入された table のみ参照する)
  assert.equal(resolveContextWindow('gpt-5', table), undefined);
});

test('computeContextUsage divides cumulative input tokens by context window', () => {
  assert.deepEqual(
    computeContextUsage({
      modelId: 'gpt-5-codex',
      contextWindow: 1000,
      cumulativeInputTokens: 250,
    }),
    {
      model_id: 'gpt-5-codex',
      context_window: 1000,
      cumulative_input_tokens: 250,
      usage_percent: 25,
    },
  );
});

test('computeContextUsage clamps usage_percent into the 0-100 range', () => {
  assert.equal(
    computeContextUsage({
      modelId: 'gpt-5',
      contextWindow: 1000,
      cumulativeInputTokens: 0,
    }).usage_percent,
    0,
  );

  assert.equal(
    computeContextUsage({
      modelId: 'gpt-5',
      contextWindow: 1000,
      cumulativeInputTokens: 2500,
    }).usage_percent,
    100,
  );
});

test('computeContextUsage returns 0 percent when context window is 0 to avoid Infinity', () => {
  assert.equal(
    computeContextUsage({
      modelId: 'broken',
      contextWindow: 0,
      cumulativeInputTokens: 100,
    }).usage_percent,
    0,
  );
});
