// bridge-result-error-propagation Task 1.1: runClaude が test ファイルから import 可能になり、
// dep 注入 seam (ClaudeRuntimeDeps) が型として export されていることを担保する smoke test。
// 本 test の存在自体が「stdio bridge 副作用 (readline / writeEvent({type:'ready'})) が
// import 時に発火しない」ことの保証でもある (発火した場合 node:test の出力が NDJSON で汚染される)。

import assert from 'node:assert/strict';
import test from 'node:test';
import { runClaude, type ClaudeRuntimeDeps } from '../src/index.js';

test('runClaude is exported as a function so tests can drive it directly', () => {
  // Task 1.1 観測完了条件: `runClaude` が test ファイルから import 可能であることの最小保証。
  // 関数として解決され、引数 2 つを受け取る seam (request + optional deps) が成立していることだけを
  // ここで assert する。4 subtype の振り分けや SDK message 列の駆動は後続 task で検証する。
  assert.equal(typeof runClaude, 'function');
  // 関数 signature 上の必須パラメータは request 1 件のみ (deps は default 付き optional)。
  assert.equal(runClaude.length, 1);
});

test('ClaudeRuntimeDeps type allows providing write/query/env seams', () => {
  // ClaudeRuntimeDeps が public type として export され、test 側で fake deps を構築できることを
  // 構造的型としてだけ確認する (実行時 dep 注入のフルシナリオは後続 task でカバー)。
  // ここで型エラーが出れば export が壊れていることに即気付ける。
  const fakeDeps: ClaudeRuntimeDeps = {
    env: {},
    write: () => {},
    // query は typeof query で受けるため、最小限の型互換 stub を渡す。
    // 戻り値の Query 型に合わせて async generator + getContextUsage を備えた最小オブジェクトを返す。
    query: (() => {
      const stub = (async function* () {})();
      Object.assign(stub, { getContextUsage: async () => ({}) });
      return stub as never;
    }) as ClaudeRuntimeDeps['query'],
  };
  assert.equal(typeof fakeDeps.write, 'function');
  assert.equal(typeof fakeDeps.query, 'function');
});
