// issue #66 A7: claude bridge から resolveAdditionalDirectories (shared util) を import し、
// Codex bridge と同じデフォルト挙動 (undefined → ['/'] / 空配列はそのまま / 非空はそのまま) で
// 解決されることを provider 共通仕様として検証する。
//
// Claude SDK の query() を mock するのは大改修が必要なため、ここでは shared util の動作確認に絞り、
// Options.additionalDirectories へ流入する経路は code review レベルで verify する (index.ts:319)。

import assert from 'node:assert/strict';
import test from 'node:test';
import { resolveAdditionalDirectories } from '@rilll/bridge-protocol/additional-directories';

test('A7: resolveAdditionalDirectories returns ["/"] when undefined (default-allow)', () => {
  assert.deepEqual(resolveAdditionalDirectories(undefined), ['/']);
});

test('A7: resolveAdditionalDirectories returns empty array as-is (restrict intent)', () => {
  assert.deepEqual(resolveAdditionalDirectories([]), []);
});

test('A7: resolveAdditionalDirectories passes through non-empty array', () => {
  assert.deepEqual(
    resolveAdditionalDirectories(['/usr/local/lib', '/opt/data']),
    ['/usr/local/lib', '/opt/data'],
  );
});

test('A7: resolveAdditionalDirectories returns a shallow copy (does not alias the input)', () => {
  const input = ['/a', '/b'];
  const result = resolveAdditionalDirectories(input);
  assert.deepEqual(result, input);
  assert.notEqual(result, input, 'should not alias the same reference');
});
