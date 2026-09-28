// issue #47: user 定義 PreToolUse hook の spawn / block 経路の単体テスト。
// 実 bash を spawn する本物テストと、writers/spawnImpl を mock した単体テストの両方を含む。
//
// SDK 自体は起動しない (Claude API key 不要)。block 翻訳は callback の戻り値レベルで検証する。

import assert from 'node:assert/strict';
import test from 'node:test';
import { mkdtempSync, writeFileSync, chmodSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import {
  spawnUserHook,
  userHookCallback,
  type UserHookWriters,
} from '../src/user-hook.js';
import type { BridgeEvent } from '../src/protocol.js';
import type { AkashicLevel } from '../src/akashic.js';

// 単体テストごとに使い捨ての shell script を作るヘルパ。
// 実 bash で起動するので OS の bash 依存。CI が POSIX-like であれば動く想定。
function withTempScript(body: string, fn: (scriptPath: string) => Promise<void>): () => Promise<void> {
  return async () => {
    const dir = mkdtempSync(path.join(tmpdir(), 'rilll-user-hook-test-'));
    const scriptPath = path.join(dir, 'hook.sh');
    writeFileSync(scriptPath, `#!/usr/bin/env bash\n${body}\n`, { mode: 0o755 });
    chmodSync(scriptPath, 0o755);
    try {
      await fn(scriptPath);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  };
}

// ObservationCall は writeObservation が呼ばれた回数 / 引数を記録するヘルパ。
type ObservationCall = {
  level: AkashicLevel;
  source: string;
  payload: unknown;
  explicitType?: string;
  explicitSubtype?: string;
};

// makeWriters は writers モックを生成し、ベタな配列に push する。
function makeWriters(): UserHookWriters & {
  events: BridgeEvent[];
  observations: ObservationCall[];
} {
  const events: BridgeEvent[] = [];
  const observations: ObservationCall[] = [];
  return {
    events,
    observations,
    writeEvent: (event) => { events.push(event); },
    writeObservation: (level, source, payload, explicitType, explicitSubtype) => {
      observations.push({ level, source, payload, explicitType, explicitSubtype });
    },
  };
}

// PreToolUse hook payload の最小モック。SDK 型 (HookInput) は構造的型として
// `hook_event_name` 等を要求するので、unknown 経由で as でキャストする。
const samplePayload = {
  hook_event_name: 'PreToolUse',
  tool_name: 'Edit',
  tool_input: { file_path: '/repo/.kiro/specs/foo/tasks.md', new_string: '- [x] task' },
  cwd: '/repo',
  session_id: 'sess-1',
} as unknown as Parameters<ReturnType<typeof userHookCallback>>[0];

test('spawnUserHook returns parsed block decision when stdout is JSON', withTempScript(
  `printf '{"decision":"block","reason":"verify ログ未読"}'`,
  async (scriptPath) => {
    const result = await spawnUserHook(['bash', scriptPath], samplePayload, '/tmp');
    assert.deepEqual(result, { decision: 'block', reason: 'verify ログ未読' });
  },
));

test('spawnUserHook resolves to empty object when stdout is empty', withTempScript(
  `exit 0`,
  async (scriptPath) => {
    const result = await spawnUserHook(['bash', scriptPath], samplePayload, '/tmp');
    assert.deepEqual(result, {});
  },
));

test('spawnUserHook ignores non-block decision values', withTempScript(
  `printf '{"decision":"approve"}'`,
  async (scriptPath) => {
    // claude-code の hook 仕様では decision = 'block' 以外は意味を持たないため、
    // bridge 側でも block 以外は素通し (allow) に倒す。
    const result = await spawnUserHook(['bash', scriptPath], samplePayload, '/tmp');
    assert.deepEqual(result, {});
  },
));

test('spawnUserHook rejects when command does not exist', async () => {
  await assert.rejects(
    () => spawnUserHook(['/nonexistent/binary-for-test'], samplePayload, '/tmp'),
    (err: unknown) => err instanceof Error,
  );
});

test('spawnUserHook rejects when stdout is not JSON', withTempScript(
  `printf 'this is not json'`,
  async (scriptPath) => {
    await assert.rejects(
      () => spawnUserHook(['bash', scriptPath], samplePayload, '/tmp'),
      (err: unknown) => err instanceof Error && /not JSON/.test(err.message),
    );
  },
));

test('spawnUserHook times out and rejects when script blocks', withTempScript(
  `sleep 5`,
  async (scriptPath) => {
    await assert.rejects(
      () => spawnUserHook(['bash', scriptPath], samplePayload, '/tmp', 100),
      (err: unknown) => err instanceof Error && /timed out/.test(err.message),
    );
  },
));

test('spawnUserHook receives the payload on stdin', withTempScript(
  // stdin を受け取って tool_input.file_path をそのまま reason として返す。
  `payload="$(cat)"
echo "$payload" | grep -q tasks.md && printf '{"decision":"block","reason":"received"}'`,
  async (scriptPath) => {
    const result = await spawnUserHook(['bash', scriptPath], samplePayload, '/tmp');
    assert.deepEqual(result, { decision: 'block', reason: 'received' });
  },
));

test('userHookCallback emits hook_block + error event when spawn returns block', async () => {
  // spawn を mock 化し、block を返すようにして writers の副作用を検証する。
  const writers = makeWriters();
  const cb = userHookCallback(
    'summary',
    ['bash', '/tmp/dummy.sh'],
    '/repo',
    'req-test',
    writers,
    async () => ({ decision: 'block', reason: 'verify ログ未読' }),
  );
  const out = await cb(samplePayload, 'tool-use-1', { signal: new AbortController().signal });
  assert.deepEqual(out, { decision: 'block', systemMessage: 'verify ログ未読' });

  assert.equal(writers.observations.length, 1);
  const obs = writers.observations[0];
  assert.equal(obs?.explicitType, 'hook_block');
  assert.equal(obs?.source, 'claude-hook');

  assert.equal(writers.events.length, 1);
  const event = writers.events[0];
  if (event?.type !== 'error') throw new Error('expected error event');
  assert.equal(event.code, 'hook_block');
  assert.equal(event.retryable, false);
  assert.equal(event.id, 'req-test');
  assert.match(event.message, /verify ログ未読/);
});

test('userHookCallback returns empty object when spawn returns no decision', async () => {
  const writers = makeWriters();
  const cb = userHookCallback(
    'summary',
    ['bash', '/tmp/dummy.sh'],
    '/repo',
    'req-test',
    writers,
    async () => ({}),
  );
  const out = await cb(samplePayload, 'tool-use-1', { signal: new AbortController().signal });
  assert.deepEqual(out, {});
  // 副作用なし: 観測も error event も出さない (allow 経路)。
  assert.equal(writers.observations.length, 0);
  assert.equal(writers.events.length, 0);
});

test('userHookCallback emits hook_spawn_error and allows when spawn throws', async () => {
  // spawn 失敗 / timeout / 不正 JSON は best-effort allow に倒し、observation だけ残す。
  const writers = makeWriters();
  const cb = userHookCallback(
    'summary',
    ['bash', '/tmp/dummy.sh'],
    '/repo',
    'req-test',
    writers,
    async () => { throw new Error('synthetic spawn failure'); },
  );
  const out = await cb(samplePayload, 'tool-use-1', { signal: new AbortController().signal });
  assert.deepEqual(out, {});

  assert.equal(writers.observations.length, 1);
  const obs = writers.observations[0];
  assert.equal(obs?.explicitType, 'hook_spawn_error');
  assert.equal(obs?.source, 'claude-hook');
  // hook_spawn_error 時は error event を emit しない (block 判定が出せていないため)。
  assert.equal(writers.events.length, 0);
});

test('userHookCallback uses default reason when block has no reason', async () => {
  const writers = makeWriters();
  const cb = userHookCallback(
    'summary',
    ['bash', '/tmp/dummy.sh'],
    '/repo',
    'req-test',
    writers,
    async () => ({ decision: 'block' }),
  );
  const out = await cb(samplePayload, 'tool-use-1', { signal: new AbortController().signal });
  assert.deepEqual(out, {
    decision: 'block',
    systemMessage: 'user hook blocked tool use (no reason provided)',
  });
  assert.equal(writers.events.length, 1);
});
