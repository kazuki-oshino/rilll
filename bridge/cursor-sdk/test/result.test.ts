import assert from 'node:assert/strict';
import test from 'node:test';
import { resolveFinalResult } from '../src/result.js';

test('resolveFinalResult prefers run.wait final result over streamed assistant fragment', () => {
  const got = resolveFinalResult('。', 'コミットしました。\n\n10 files changed.');

  assert.equal(got, 'コミットしました。\n\n10 files changed.');
});

test('resolveFinalResult falls back to streamed assistant text when run.wait has no result', () => {
  const got = resolveFinalResult('streamed summary', undefined);

  assert.equal(got, 'streamed summary');
});
