// issue #65 B1/B2/B3: deny gate / allowedTools 違反 / user hook の擬似実装が
// item.started 段階で abort 経路に流れることを fake Codex 経由で検証する。
// AC「rm -rf / を含む command_execution が deny されてターン全体が abort する」もここで担保する。

import assert from 'node:assert/strict';
import test from 'node:test';
import type {
  Codex,
  CodexOptions,
  Thread,
  ThreadEvent,
  ThreadItem,
} from '@openai/codex-sdk';
import { runCodex, evaluateAllowedToolsViolation, type CodexRuntimeDeps } from '../src/index.js';
import { evaluateDeny } from '../src/deny.js';
import {
  evaluateUserHooks,
  toolNamesForCodexItem,
  spawnUserHook,
  type UserHookOutput,
} from '../src/user-hook.js';

type BridgeEvent = Record<string, unknown>;

function makeCodex(events: ThreadEvent[]): Codex {
  const buildThread = (): Thread => ({
    async runStreamed() {
      return {
        events: (async function* (): AsyncGenerator<ThreadEvent> {
          for (const ev of events) yield ev;
        })(),
      };
    },
    async run() { throw new Error('not used'); },
    get id(): string | null { return 'thread-x'; },
  } as unknown as Thread);
  return {
    startThread(): Thread { return buildThread(); },
    resumeThread(): Thread { return buildThread(); },
  } as unknown as Codex;
}

async function runWithEvents(
  request: Parameters<typeof runCodex>[0],
  threadEvents: ThreadEvent[],
  spawnImpl?: typeof spawnUserHook,
): Promise<BridgeEvent[]> {
  const chunks: string[] = [];
  const deps: CodexRuntimeDeps = {
    env: {},
    write: (chunk) => { chunks.push(chunk); },
    createCodex: (_opts: CodexOptions) => makeCodex(threadEvents),
    ...(spawnImpl ? { spawnUserHook: spawnImpl } : {}),
  };
  await runCodex(request, deps);
  return chunks.join('').trim().split('\n').filter(Boolean).map((line) => JSON.parse(line) as BridgeEvent);
}

const dummyTurnComplete: ThreadEvent = {
  type: 'turn.completed',
  usage: {
    input_tokens: 0,
    cached_input_tokens: 0,
    cache_write_input_tokens: 0,
    output_tokens: 0,
    reasoning_output_tokens: 0,
  },
};

// ---------- B2: deny gate ユニット ----------

test('B2: evaluateDeny detects rm -rf / in command_execution', () => {
  const item: ThreadItem = {
    id: 'cmd-1',
    type: 'command_execution',
    command: 'rm -rf /',
    aggregated_output: '',
    status: 'in_progress',
  };
  const result = evaluateDeny(item, '/repo');
  assert.ok(result, 'rm -rf / should be denied');
  assert.equal(result?.source, 'dangerous_bash');
});

test('B2: evaluateDeny allows safe commands like rm -rf /tmp/foo', () => {
  const item: ThreadItem = {
    id: 'cmd-2',
    type: 'command_execution',
    command: 'rm -rf /tmp/foo',
    aggregated_output: '',
    status: 'in_progress',
  };
  assert.equal(evaluateDeny(item, '/repo'), undefined);
});

test('B2: evaluateDeny rejects file_change with cwd 外 path', () => {
  const item: ThreadItem = {
    id: 'fc-1',
    type: 'file_change',
    changes: [{ path: '/etc/passwd', kind: 'update' }],
    status: 'completed',
  };
  const result = evaluateDeny(item, '/repo');
  assert.ok(result, 'cwd 外 write should be denied');
  assert.equal(result?.source, 'write_outside_cwd');
});

test('B2: evaluateDeny passes through file_change inside cwd', () => {
  const item: ThreadItem = {
    id: 'fc-2',
    type: 'file_change',
    changes: [{ path: 'src/index.ts', kind: 'update' }],
    status: 'completed',
  };
  assert.equal(evaluateDeny(item, '/repo'), undefined);
});

// ---------- B3: allowedTools 違反 ユニット ----------

test('B3: evaluateAllowedToolsViolation passes through when allowedTools omitted', () => {
  const item: ThreadItem = {
    id: 'fc',
    type: 'file_change',
    changes: [{ path: 'a.txt', kind: 'add' }],
    status: 'completed',
  };
  assert.equal(evaluateAllowedToolsViolation(item, undefined), undefined);
  assert.equal(evaluateAllowedToolsViolation(item, []), undefined);
});

test('B3: evaluateAllowedToolsViolation accepts file_change when "Write" is allowed', () => {
  const item: ThreadItem = {
    id: 'fc',
    type: 'file_change',
    changes: [{ path: 'a.txt', kind: 'add' }],
    status: 'completed',
  };
  assert.equal(evaluateAllowedToolsViolation(item, ['Write']), undefined);
});

test('B3: evaluateAllowedToolsViolation rejects file_change when only command_execution is allowed', () => {
  const item: ThreadItem = {
    id: 'fc',
    type: 'file_change',
    changes: [{ path: 'a.txt', kind: 'add' }],
    status: 'completed',
  };
  const result = evaluateAllowedToolsViolation(item, ['command_execution']);
  assert.ok(result, 'file_change should be rejected when only command_execution is allowed');
  assert.deepEqual(result?.toolNames, toolNamesForCodexItem(item));
});

// ---------- B1: user hook ユニット ----------

test('B1: evaluateUserHooks returns block when hook decision is block', async () => {
  const item: ThreadItem = {
    id: 'cmd',
    type: 'command_execution',
    command: 'ls',
    aggregated_output: '',
    status: 'in_progress',
  };
  const fakeSpawn = async (
    _cmd: readonly string[],
    _payload: unknown,
    _cwd: string,
  ): Promise<UserHookOutput> => ({ decision: 'block', reason: 'gate says no' });
  const result = await evaluateUserHooks(
    item,
    [{ matcher: 'Bash', command: ['/usr/bin/true'] }],
    '/repo',
    fakeSpawn as unknown as typeof spawnUserHook,
  );
  assert.equal(result.decision, 'block');
  if (result.decision !== 'block') return;
  assert.equal(result.reason, 'gate says no');
});

test('B1: evaluateUserHooks returns allow when hook decision is empty', async () => {
  const item: ThreadItem = {
    id: 'cmd',
    type: 'command_execution',
    command: 'ls',
    aggregated_output: '',
    status: 'in_progress',
  };
  const fakeSpawn = async (): Promise<UserHookOutput> => ({});
  const result = await evaluateUserHooks(
    item,
    [{ matcher: 'Bash', command: ['/usr/bin/true'] }],
    '/repo',
    fakeSpawn as unknown as typeof spawnUserHook,
  );
  assert.equal(result.decision, 'allow');
});

test('B1: evaluateUserHooks returns error when spawn fails', async () => {
  const item: ThreadItem = {
    id: 'cmd',
    type: 'command_execution',
    command: 'ls',
    aggregated_output: '',
    status: 'in_progress',
  };
  const fakeSpawn = async (): Promise<UserHookOutput> => {
    throw new Error('boom');
  };
  const result = await evaluateUserHooks(
    item,
    [{ matcher: 'Bash', command: ['/usr/bin/true'] }],
    '/repo',
    fakeSpawn as unknown as typeof spawnUserHook,
  );
  assert.equal(result.decision, 'error');
  if (result.decision !== 'error') return;
  assert.match(result.spawnError, /boom/);
});

test('B1: evaluateUserHooks skips non-matching hooks', async () => {
  const item: ThreadItem = {
    id: 'cmd',
    type: 'command_execution',
    command: 'ls',
    aggregated_output: '',
    status: 'in_progress',
  };
  let spawnCalled = 0;
  const fakeSpawn = async (): Promise<UserHookOutput> => {
    spawnCalled += 1;
    return { decision: 'block', reason: 'should not be called' };
  };
  const result = await evaluateUserHooks(
    item,
    [{ matcher: 'Edit', command: ['/usr/bin/true'] }],
    '/repo',
    fakeSpawn as unknown as typeof spawnUserHook,
  );
  assert.equal(result.decision, 'allow');
  assert.equal(spawnCalled, 0);
});

// ---------- B1+B2+B3 e2e abort 経路 (issue Acceptance Criteria) ----------

test('AC: rm -rf / in command_execution aborts the run with codex-sdk source observation and cancel result event', async () => {
  const events = await runWithEvents({
    type: 'run',
    id: 'req-rmrf',
    prompt: 'do bad',
    cwd: '/repo',
    akashicLevel: 'summary',
  }, [
    { type: 'thread.started', thread_id: 'thread-x' },
    {
      type: 'item.started',
      item: {
        id: 'cmd-1',
        type: 'command_execution',
        command: 'rm -rf /',
        aggregated_output: '',
        status: 'in_progress',
      },
    },
    dummyTurnComplete,
  ]);

  // permission_deny observation が source='codex-sdk' で 1 つ以上 emit されている
  const denyObs = events.filter((e) =>
    e.type === 'observation'
    && e.source === 'codex-sdk'
    && e.observationType === 'permission_deny',
  );
  assert.ok(denyObs.length >= 1, 'permission_deny observation must be emitted');

  // cancel された result event (origin='engine') が emit されている
  const resultEvents = events.filter((e) => e.type === 'result');
  assert.equal(resultEvents.length, 1);
  assert.equal((resultEvents[0] as { origin?: string }).origin, 'engine');

  // 通常の tool_use event は abort された command については emit されない
  const toolUseForCmd1 = events.filter(
    (e) => e.type === 'tool_use' && e.tool === 'command_execution',
  );
  assert.equal(toolUseForCmd1.length, 0);
});

test('B3: file_change item is aborted when allowedTools restricts to command_execution', async () => {
  const events = await runWithEvents({
    type: 'run',
    id: 'req-allow',
    prompt: 'edit a file',
    cwd: '/repo',
    akashicLevel: 'summary',
    allowedTools: ['command_execution'],
  }, [
    { type: 'thread.started', thread_id: 'thread-x' },
    {
      type: 'item.started',
      item: {
        id: 'fc-1',
        type: 'file_change',
        changes: [{ path: 'src/a.ts', kind: 'add' }],
        status: 'completed',
      },
    },
    dummyTurnComplete,
  ]);

  const violations = events.filter((e) =>
    e.type === 'observation'
    && e.source === 'codex-sdk'
    && e.observationType === 'allowed_tools_violation',
  );
  assert.ok(violations.length >= 1, 'allowed_tools_violation observation must be emitted');

  const resultEvents = events.filter((e) => e.type === 'result');
  assert.equal(resultEvents.length, 1);
  assert.equal((resultEvents[0] as { origin?: string }).origin, 'engine');
});

// ---------- N1: user hook spawn dedup ----------

test('N1: user hook is spawned at most once per item.id across started/updated/completed', async () => {
  let spawnCount = 0;
  const fakeSpawn = async (): Promise<UserHookOutput> => {
    spawnCount += 1;
    return {};
  };
  await runWithEvents({
    type: 'run',
    id: 'req-n1-dedup',
    prompt: 'do work',
    cwd: '/repo',
    akashicLevel: 'summary',
    userHooks: [{ matcher: 'Bash', command: ['/bin/true'] }],
  }, [
    { type: 'thread.started', thread_id: 'thread-x' },
    {
      type: 'item.started',
      item: { id: 'cmd-1', type: 'command_execution', command: 'ls', aggregated_output: '', status: 'in_progress' },
    },
    {
      type: 'item.updated',
      item: { id: 'cmd-1', type: 'command_execution', command: 'ls', aggregated_output: 'foo', status: 'in_progress' },
    },
    {
      type: 'item.completed',
      item: { id: 'cmd-1', type: 'command_execution', command: 'ls', aggregated_output: 'foo', status: 'completed', exit_code: 0 },
    },
    dummyTurnComplete,
  ], fakeSpawn as typeof spawnUserHook);
  assert.equal(spawnCount, 1, 'user hook should be spawned exactly once per item.id');
});

// ---------- N4: item.completed 段の deny gate 保険経路 ----------

test('N4: deny gate fires on item.completed when SDK skips started/updated stages', async () => {
  const events = await runWithEvents({
    type: 'run',
    id: 'req-n4-completed-only',
    prompt: 'write to /etc',
    cwd: '/repo',
    akashicLevel: 'summary',
  }, [
    { type: 'thread.started', thread_id: 'thread-x' },
    // SDK 仕様変更等で started/updated を飛ばして completed だけ届くケース。
    // file_change の PatchApplyStatus は 'completed' | 'failed' のみで in_progress が無いため
    // 現実的に起こり得る経路。bridge/codex-sdk/src/index.ts:480-482 の保険評価が効くことを担保する。
    {
      type: 'item.completed',
      item: {
        id: 'fc-completed-only',
        type: 'file_change',
        changes: [{ path: '/etc/passwd', kind: 'update' }],
        status: 'completed',
      },
    },
    dummyTurnComplete,
  ]);

  const denyObs = events.filter((e) =>
    e.type === 'observation'
    && e.source === 'codex-sdk'
    && e.observationType === 'permission_deny',
  );
  assert.ok(denyObs.length >= 1, 'permission_deny should fire even when only item.completed is observed');

  const resultEvents = events.filter((e) => e.type === 'result');
  assert.equal(resultEvents.length, 1);
  assert.equal((resultEvents[0] as { origin?: string }).origin, 'engine');

  // abort 経路に倒れるため file_change の tool_use event は emit されない
  const toolUseForFc = events.filter(
    (e) => e.type === 'tool_use' && e.tool === 'file_change',
  );
  assert.equal(toolUseForFc.length, 0);
});

test('N1: different item.ids still spawn user hook independently', async () => {
  let spawnCount = 0;
  const fakeSpawn = async (): Promise<UserHookOutput> => {
    spawnCount += 1;
    return {};
  };
  await runWithEvents({
    type: 'run',
    id: 'req-n1-multi',
    prompt: 'do work',
    cwd: '/repo',
    akashicLevel: 'summary',
    userHooks: [{ matcher: 'Bash', command: ['/bin/true'] }],
  }, [
    { type: 'thread.started', thread_id: 'thread-x' },
    {
      type: 'item.started',
      item: { id: 'cmd-1', type: 'command_execution', command: 'ls', aggregated_output: '', status: 'in_progress' },
    },
    {
      type: 'item.started',
      item: { id: 'cmd-2', type: 'command_execution', command: 'pwd', aggregated_output: '', status: 'in_progress' },
    },
    dummyTurnComplete,
  ], fakeSpawn as typeof spawnUserHook);
  assert.equal(spawnCount, 2, 'user hook should fire once for each distinct item.id');
});
