// issue #65 A2: protocol effort enum を Claude SDK Options.effort へマッピングする経路の単体テスト。
// 'minimal' は SDK が受けないため 'low' に倒す仕様 (issue 仕様)。

import assert from 'node:assert/strict';
import test from 'node:test';
import { DEFAULT_CLAUDE_EFFORT, mapClaudeEffort, resolveClaudeEffort } from '../src/effort.js';

test('A2: minimal maps to low (SDK does not accept minimal)', () => {
  assert.equal(mapClaudeEffort('minimal'), 'low');
});

test('A2: low/medium/high/xhigh/max pass through unchanged', () => {
  assert.equal(mapClaudeEffort('low'), 'low');
  assert.equal(mapClaudeEffort('medium'), 'medium');
  assert.equal(mapClaudeEffort('high'), 'high');
  assert.equal(mapClaudeEffort('xhigh'), 'xhigh');
  assert.equal(mapClaudeEffort('max'), 'max');
});

test('A2: DEFAULT_CLAUDE_EFFORT is xhigh (rilll bridge layer default)', () => {
  assert.equal(DEFAULT_CLAUDE_EFFORT, 'xhigh');
});

test('A2: resolveClaudeEffort returns xhigh when request.effort is undefined', () => {
  assert.equal(resolveClaudeEffort(undefined), 'xhigh');
});

test('A2: resolveClaudeEffort applies mapClaudeEffort when request.effort is defined', () => {
  assert.equal(resolveClaudeEffort('minimal'), 'low');
  assert.equal(resolveClaudeEffort('low'), 'low');
  assert.equal(resolveClaudeEffort('medium'), 'medium');
  assert.equal(resolveClaudeEffort('high'), 'high');
  assert.equal(resolveClaudeEffort('xhigh'), 'xhigh');
});
