import assert from 'node:assert/strict';
import test from 'node:test';
import {
  normalizeAkashicLevel,
  shouldWriteObservation,
} from '../src/akashic.js';

test('normalizeAkashicLevel defaults unknown values to summary', () => {
  assert.equal(normalizeAkashicLevel(undefined), 'summary');
  assert.equal(normalizeAkashicLevel('full'), 'full');
  assert.equal(normalizeAkashicLevel('debug'), 'debug');
  assert.equal(normalizeAkashicLevel('verbose'), 'summary');
});

test('shouldWriteObservation summary keeps only key cursor-sdk lifecycle events', () => {
  // 残すべき: 起点/終端/メトリクス
  assert.equal(shouldWriteObservation('summary', 'cursor-sdk', 'system'), true);
  assert.equal(shouldWriteObservation('summary', 'cursor-sdk', 'status'), true);
  assert.equal(shouldWriteObservation('summary', 'cursor-sdk', 'usage'), true);
  assert.equal(shouldWriteObservation('summary', 'cursor-sdk', 'error'), true);
  // 落とすもの: 細粒度の内部イベント
  assert.equal(shouldWriteObservation('summary', 'cursor-sdk', 'assistant'), false);
  assert.equal(shouldWriteObservation('summary', 'cursor-sdk', 'user'), false);
  assert.equal(shouldWriteObservation('summary', 'cursor-sdk', 'thinking'), false);
  assert.equal(shouldWriteObservation('summary', 'cursor-sdk', 'tool_call'), false);
  assert.equal(shouldWriteObservation('summary', 'cursor-sdk', 'task'), false);
});

test('shouldWriteObservation full excludes only thinking/user noise', () => {
  assert.equal(shouldWriteObservation('full', 'cursor-sdk', 'assistant'), true);
  assert.equal(shouldWriteObservation('full', 'cursor-sdk', 'tool_call'), true);
  assert.equal(shouldWriteObservation('full', 'cursor-sdk', 'task'), true);
  assert.equal(shouldWriteObservation('full', 'cursor-sdk', 'status'), true);
  assert.equal(shouldWriteObservation('full', 'cursor-sdk', 'thinking'), false);
  assert.equal(shouldWriteObservation('full', 'cursor-sdk', 'user'), false);
});

test('shouldWriteObservation debug writes everything regardless of source', () => {
  assert.equal(shouldWriteObservation('debug', 'cursor-sdk', 'thinking'), true);
  assert.equal(shouldWriteObservation('debug', 'cursor-sdk', 'user'), true);
  assert.equal(shouldWriteObservation('debug', 'other', 'whatever'), true);
});

test('shouldWriteObservation summary forwards non-cursor-sdk sources', () => {
  // codex bridge と同じく、外部 source は summary でも素通しする
  assert.equal(shouldWriteObservation('summary', 'other', 'something'), false);
  assert.equal(shouldWriteObservation('full', 'other', 'something'), true);
});

test('shouldWriteObservation summary keeps PreToolUse / PostToolUse / PostToolUseFailure under cursor-hook', () => {
  // claude-hook と対称な closed enum (item 5)。summary level でも tool フックは常に残す。
  assert.equal(shouldWriteObservation('summary', 'cursor-hook', 'hook', 'PreToolUse'), true);
  assert.equal(shouldWriteObservation('summary', 'cursor-hook', 'hook', 'PostToolUse'), true);
  assert.equal(shouldWriteObservation('summary', 'cursor-hook', 'hook', 'PostToolUseFailure'), true);
});

test('shouldWriteObservation summary drops unknown hook subtype under cursor-hook', () => {
  // closed enum 違反の subtype は summary で落とす (filter rule の安全網)。
  assert.equal(shouldWriteObservation('summary', 'cursor-hook', 'hook', 'PostToolBatch'), false);
  assert.equal(shouldWriteObservation('summary', 'cursor-hook', 'hook', undefined), false);
  // observationType が 'hook' でない場合も summary で落とす。
  assert.equal(shouldWriteObservation('summary', 'cursor-hook', 'tool_call', 'PostToolUse'), false);
});

test('shouldWriteObservation full keeps all cursor-hook events', () => {
  // full level では fullExclude を定義していないので、cursor-hook 配下の hook イベントは
  // subtype に関係なく素通しする (claude-hook と同じ取り回し)。
  assert.equal(shouldWriteObservation('full', 'cursor-hook', 'hook', 'PreToolUse'), true);
  assert.equal(shouldWriteObservation('full', 'cursor-hook', 'hook', 'PostToolBatch'), true);
});

test('shouldWriteObservation summary keeps cursor-control events', () => {
  assert.equal(shouldWriteObservation('summary', 'cursor-control', 'resume_fallback'), true);
  assert.equal(shouldWriteObservation('summary', 'cursor-control', 'context_usage'), true);
  assert.equal(shouldWriteObservation('summary', 'cursor-control', 'context_usage_error'), true);
});

test('shouldWriteObservation summary drops unknown cursor-control events', () => {
  assert.equal(shouldWriteObservation('summary', 'cursor-control', 'other_control_event'), false);
});
