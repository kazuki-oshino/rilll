import assert from 'node:assert/strict';
import test from 'node:test';
import {
  eventSchema,
  requestSchema,
  serializeEvent,
} from '../src/protocol.js';
import {
  createContextUsageScheduler,
  normalizeAkashicLevel,
  shouldWriteObservation,
} from '../src/akashic.js';
import { buildCancelResultEvent } from '../src/cancel.js';

test('requestSchema accepts run requests', () => {
  const parsed = requestSchema.parse({
    type: 'run',
    id: 'req-1',
    prompt: 'hello',
    cwd: '/repo',
    akashicLevel: 'debug',
    traceContext: {
      traceparent: '00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01',
      tracestate: 'vendor=value',
      baggage: 'project=rilll',
    },
    outputFormat: { type: 'text' },
    metadata: {
      workflowRunId: 'workflow-run-1',
      workflowId: 'update-readme',
      workflowName: 'README更新',
      stepId: 'plan',
      stepName: '提案作成',
      agentCallId: 'agent-call-1',
    },
  });

  assert.equal(parsed.type, 'run');
  assert.equal(parsed.id, 'req-1');
  assert.equal(parsed.akashicLevel, 'debug');
  assert.equal(parsed.metadata?.agentCallId, 'agent-call-1');
  assert.equal(parsed.traceContext?.traceparent, '00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01');
});

test('requestSchema rejects unknown request types', () => {
  assert.throws(() => requestSchema.parse({ type: 'bogus', id: 'req-1' }));
});

test('requestSchema accepts optional model field', () => {
  // review-workflow Task 1.3: per-rule provider/model 上書き seam で `model` を追加した。
  // 指定時には string として保持されることを検証する。
  const parsed = requestSchema.parse({
    type: 'run',
    id: 'req-1',
    prompt: 'hello',
    cwd: '/repo',
    model: 'claude-opus-4-1-20250805',
  });
  assert.equal(parsed.type, 'run');
  if (parsed.type !== 'run') return;
  assert.equal(parsed.model, 'claude-opus-4-1-20250805');
});

test('requestSchema treats model as optional (zero-value omitted)', () => {
  // model を指定しないとき undefined のまま (provider 既定モデルが維持される) ことを検証する。
  const parsed = requestSchema.parse({
    type: 'run',
    id: 'req-1',
    prompt: 'hello',
    cwd: '/repo',
  });
  assert.equal(parsed.type, 'run');
  if (parsed.type !== 'run') return;
  assert.equal(parsed.model, undefined);
});

test('requestSchema rejects non-string model values', () => {
  // model フィールドの型を string に固定し、ブール / 数値などの誤指定を検出することを検証する。
  assert.throws(() => requestSchema.parse({
    type: 'run',
    id: 'req-1',
    prompt: 'hello',
    cwd: '/repo',
    model: 42,
  }));
});

test('requestSchema accepts optional allowedTools field', () => {
  // kiro-spec-loop Phase C: fix-judge ターン等で Edit/Write を禁じるための seam。
  // 指定時には string[] として保持されることを検証する。
  const parsed = requestSchema.parse({
    type: 'run',
    id: 'req-1',
    prompt: 'hello',
    cwd: '/repo',
    allowedTools: ['Read', 'Glob', 'Grep'],
  });
  assert.equal(parsed.type, 'run');
  if (parsed.type !== 'run') return;
  assert.deepEqual(parsed.allowedTools, ['Read', 'Glob', 'Grep']);
});

test('requestSchema treats allowedTools as optional (zero-value omitted)', () => {
  // allowedTools を指定しないとき undefined のまま (SDK 既定 = 全ツール許可) であることを検証する。
  const parsed = requestSchema.parse({
    type: 'run',
    id: 'req-1',
    prompt: 'hello',
    cwd: '/repo',
  });
  assert.equal(parsed.type, 'run');
  if (parsed.type !== 'run') return;
  assert.equal(parsed.allowedTools, undefined);
});

test('requestSchema rejects non-string allowedTools entries', () => {
  // 配列要素の型を string に固定し、数値 / オブジェクトなどの誤指定を検出することを検証する。
  assert.throws(() => requestSchema.parse({
    type: 'run',
    id: 'req-1',
    prompt: 'hello',
    cwd: '/repo',
    allowedTools: ['Read', 42],
  }));
});

test('requestSchema accepts optional userHooks field', () => {
  // issue #47: PreToolUse hook で block 判定を行う user 定義 hook を runRequest に乗せる。
  // matcher と command(配列) を保持することを検証する。
  const parsed = requestSchema.parse({
    type: 'run',
    id: 'req-1',
    prompt: 'hello',
    cwd: '/repo',
    userHooks: [
      { matcher: 'Edit', command: ['bash', '-lc', '/tmp/verify-gate.sh'] },
      { matcher: 'Read', command: ['bash', '-lc', '/tmp/track-read.sh'] },
    ],
  });
  assert.equal(parsed.type, 'run');
  if (parsed.type !== 'run') return;
  assert.deepEqual(parsed.userHooks, [
    { matcher: 'Edit', command: ['bash', '-lc', '/tmp/verify-gate.sh'] },
    { matcher: 'Read', command: ['bash', '-lc', '/tmp/track-read.sh'] },
  ]);
});

test('requestSchema treats userHooks as optional (zero-value omitted)', () => {
  // userHooks 未指定のときは undefined のまま (= 既存 observationHook のみで block 経路無効) を維持することを検証する。
  const parsed = requestSchema.parse({
    type: 'run',
    id: 'req-1',
    prompt: 'hello',
    cwd: '/repo',
  });
  assert.equal(parsed.type, 'run');
  if (parsed.type !== 'run') return;
  assert.equal(parsed.userHooks, undefined);
});

test('requestSchema rejects userHooks with empty matcher', () => {
  // matcher は SDK hook config の正規表現として spawn 判定に使うため、空文字を弾いて誤指定を防ぐ。
  assert.throws(() => requestSchema.parse({
    type: 'run',
    id: 'req-1',
    prompt: 'hello',
    cwd: '/repo',
    userHooks: [{ matcher: '', command: ['bash', '-lc', 'exit 0'] }],
  }));
});

test('requestSchema rejects userHooks with empty command array', () => {
  // command は injection 対策で配列形式必須。空配列は spawn できないため zod が reject する。
  assert.throws(() => requestSchema.parse({
    type: 'run',
    id: 'req-1',
    prompt: 'hello',
    cwd: '/repo',
    userHooks: [{ matcher: 'Edit', command: [] }],
  }));
});

test('requestSchema rejects userHooks with non-string command entries', () => {
  // command 配列要素の型を string に固定し、数値 / オブジェクト混入を検出する。
  assert.throws(() => requestSchema.parse({
    type: 'run',
    id: 'req-1',
    prompt: 'hello',
    cwd: '/repo',
    userHooks: [{ matcher: 'Edit', command: ['bash', 42] }],
  }));
});

test('requestSchema accepts permissionMode closed enum values', () => {
  // output-contract-2phase task 1.2: per-call permissionMode 上書き seam を追加。
  // 4 値 ('default' / 'acceptEdits' / 'plan' / 'bypassPermissions') が closed enum として
  // literal 保持されることを検証する。Claude Agent SDK Options.permissionMode と語彙を合わせる。
  for (const mode of ['default', 'acceptEdits', 'plan', 'bypassPermissions'] as const) {
    const parsed = requestSchema.parse({
      type: 'run',
      id: 'req-1',
      prompt: 'hello',
      cwd: '/repo',
      permissionMode: mode,
    });
    assert.equal(parsed.type, 'run');
    if (parsed.type !== 'run') return;
    assert.equal(parsed.permissionMode, mode);
  }
});

test('requestSchema treats permissionMode as optional (zero-value omitted)', () => {
  // permissionMode 未指定のときは undefined のまま (bridge 既定 'acceptEdits' を Claude 側が維持) を検証する。
  const parsed = requestSchema.parse({
    type: 'run',
    id: 'req-1',
    prompt: 'hello',
    cwd: '/repo',
  });
  assert.equal(parsed.type, 'run');
  if (parsed.type !== 'run') return;
  assert.equal(parsed.permissionMode, undefined);
});

test('requestSchema rejects unknown permissionMode values', () => {
  // closed enum なので 'readonly' 等の未知値は zod が reject することを検証する。
  assert.throws(() => requestSchema.parse({
    type: 'run',
    id: 'req-1',
    prompt: 'hello',
    cwd: '/repo',
    permissionMode: 'readonly',
  }));
});

test('requestSchema accepts maxTurns positive integer', () => {
  // output-contract-2phase task 1.2: per-call maxTurns 上書き seam を追加。
  // 正の整数が int として literal 保持されることを検証する。
  const parsed = requestSchema.parse({
    type: 'run',
    id: 'req-1',
    prompt: 'hello',
    cwd: '/repo',
    maxTurns: 2,
  });
  assert.equal(parsed.type, 'run');
  if (parsed.type !== 'run') return;
  assert.equal(parsed.maxTurns, 2);
});

test('requestSchema treats maxTurns as optional (zero-value omitted)', () => {
  // maxTurns 未指定のときは undefined のまま (SDK 既定 turn budget を維持) を検証する。
  const parsed = requestSchema.parse({
    type: 'run',
    id: 'req-1',
    prompt: 'hello',
    cwd: '/repo',
  });
  assert.equal(parsed.type, 'run');
  if (parsed.type !== 'run') return;
  assert.equal(parsed.maxTurns, undefined);
});

test('requestSchema rejects non-positive maxTurns', () => {
  // 0 / 負数 / 小数 は zod の int().positive() で reject されることを検証する。
  // 0 は wire 上で omitempty として落ちる前提なので、ここに到達する 0 は protocol 違反として弾く。
  for (const invalid of [0, -1, 1.5]) {
    assert.throws(() => requestSchema.parse({
      type: 'run',
      id: 'req-1',
      prompt: 'hello',
      cwd: '/repo',
      maxTurns: invalid,
    }));
  }
});

test('requestSchema preserves permissionMode and maxTurns through JSON round-trip', () => {
  // wire round-trip: serialize → JSON.parse → schema.parse の経路で値が保持されることを確認する。
  // Go 側 protocol_test.go の TestRequestPermissionModeAndMaxTurnsRoundTrip と対称。
  const original = {
    type: 'run',
    id: 'req-1',
    prompt: 'hello',
    cwd: '/repo',
    permissionMode: 'plan',
    maxTurns: 5,
  };
  const wire = JSON.stringify(original);
  const decoded = requestSchema.parse(JSON.parse(wire));
  assert.equal(decoded.type, 'run');
  if (decoded.type !== 'run') return;
  assert.equal(decoded.permissionMode, 'plan');
  assert.equal(decoded.maxTurns, 5);
});

test('requestSchema rejects legacy observabilityLevel field', () => {
  assert.throws(() => requestSchema.parse({
    type: 'run',
    id: 'req-1',
    prompt: 'hello',
    cwd: '/repo',
    observabilityLevel: 'debug',
  }));
});

test('cancelRequestSchema parses origin=\'user\' literally', () => {
  // memory-distiller-trigger-policy task 2.1: cancel origin discriminator を
  // user 起源で明示指定するケース。zod が literal として保持することを検証する。
  const parsed = requestSchema.parse({ type: 'cancel', id: 'req-1', origin: 'user' });
  assert.equal(parsed.type, 'cancel');
  if (parsed.type !== 'cancel') return;
  assert.equal(parsed.origin, 'user');
});

test('cancelRequestSchema parses origin=\'engine\' literally', () => {
  // engine 起源の明示指定 (例: ctx cancel 経由の internal cancel) を literal として保持することを検証する。
  const parsed = requestSchema.parse({ type: 'cancel', id: 'req-1', origin: 'engine' });
  assert.equal(parsed.type, 'cancel');
  if (parsed.type !== 'cancel') return;
  assert.equal(parsed.origin, 'engine');
});

test('cancelRequestSchema defaults origin to \'engine\' when unspecified', () => {
  // origin 未指定時は default 'engine' に解決され、既存 cancel 送出側 (Req 6 後方互換) を壊さないことを検証する。
  const parsed = requestSchema.parse({ type: 'cancel', id: 'req-1' });
  assert.equal(parsed.type, 'cancel');
  if (parsed.type !== 'cancel') return;
  assert.equal(parsed.origin, 'engine');
});

test('cancelRequestSchema rejects unknown origin values', () => {
  // closed enum 'user' | 'engine' のため、'system' 等の未知値は zod が reject することを検証する。
  assert.throws(() => requestSchema.parse({ type: 'cancel', id: 'req-1', origin: 'system' }));
});

test('eventSchema result event passes through origin field', () => {
  // R-1 closing: result event payload に origin を運搬する経路を検証する。
  // 指定時は literal を保持し、未指定時は undefined (= omitempty で wire に出ない) のままであること。
  const parsedWithOrigin = eventSchema.parse({ type: 'result', id: 'req-1', origin: 'user' });
  assert.equal(parsedWithOrigin.type, 'result');
  if (parsedWithOrigin.type !== 'result') return;
  assert.equal(parsedWithOrigin.origin, 'user');

  const parsedWithoutOrigin = eventSchema.parse({ type: 'result', id: 'req-1' });
  assert.equal(parsedWithoutOrigin.type, 'result');
  if (parsedWithoutOrigin.type !== 'result') return;
  assert.equal(parsedWithoutOrigin.origin, undefined);
});

test('buildCancelResultEvent passes origin=\'user\' through to result event', () => {
  // memory-distiller-trigger-policy task 2.4: cancel(origin='user') 後の result event 透過を検証する。
  // bridge cancel handler が保持した origin を result event payload にそのまま乗せることを確認する。
  const event = buildCancelResultEvent({
    id: 'req-1',
    origin: 'user',
    result: 'partial output',
    sessionId: 'session-abc',
    structuredOutput: undefined,
  });
  assert.equal(event.type, 'result');
  if (event.type !== 'result') return;
  assert.equal(event.id, 'req-1');
  assert.equal(event.origin, 'user');
  assert.equal(event.result, 'partial output');
  assert.equal(event.sessionId, 'session-abc');
});

test('buildCancelResultEvent passes origin=\'engine\' through to result event', () => {
  // engine 起源の cancel (内部 ctx cancel など) を result event に透過することを検証する。
  const event = buildCancelResultEvent({
    id: 'req-2',
    origin: 'engine',
    result: '',
    sessionId: undefined,
    structuredOutput: undefined,
  });
  assert.equal(event.type, 'result');
  if (event.type !== 'result') return;
  assert.equal(event.origin, 'engine');
});

test('buildCancelResultEvent defaults to \'engine\' when origin is undefined', () => {
  // memory-distiller-trigger-policy task 2.4 (Req 6 後方互換): cancel handler が origin を保持し損ねた
  // 場合の安全網として、undefined → default 'engine' に解決することを検証する。
  // cancelRequestSchema の zod default ('engine') と整合する。
  const event = buildCancelResultEvent({
    id: 'req-3',
    origin: undefined,
    result: '',
    sessionId: undefined,
    structuredOutput: undefined,
  });
  assert.equal(event.type, 'result');
  if (event.type !== 'result') return;
  assert.equal(event.origin, 'engine');
});

test('buildCancelResultEvent forwards structuredOutput unchanged', () => {
  // structuredOutput など他 field を欠落させずに透過することを確認する (回帰防止)。
  const structured = { foo: 'bar' };
  const event = buildCancelResultEvent({
    id: 'req-4',
    origin: 'user',
    result: 'cancelled mid-flight',
    sessionId: 'session-xyz',
    structuredOutput: structured,
  });
  assert.equal(event.type, 'result');
  if (event.type !== 'result') return;
  assert.equal(event.structuredOutput, structured);
  assert.equal(event.origin, 'user');
});

test('eventSchema accepts text events', () => {
  const parsed = eventSchema.parse({ type: 'text', id: 'req-1', text: 'hello' });
  assert.equal(parsed.type, 'text');
  assert.equal(parsed.text, 'hello');
});

test('eventSchema accepts observation events', () => {
  const parsed = eventSchema.parse({
    type: 'observation',
    id: 'req-1',
    source: 'claude-sdk',
    observationType: 'result',
    observationSubtype: 'success',
    sequence: 1,
    payload: { type: 'result', total_cost_usd: 0.12 },
  });

  assert.equal(parsed.type, 'observation');
  assert.equal(parsed.source, 'claude-sdk');
  assert.equal(parsed.observationType, 'result');
});

test('serializeEvent writes one JSON object per line', () => {
  assert.equal(
    serializeEvent({ type: 'done', id: 'req-1', result: 'ok' }),
    '{"type":"done","id":"req-1","result":"ok"}\n',
  );
});

test('normalizeAkashicLevel defaults unknown values to summary', () => {
  assert.equal(normalizeAkashicLevel(undefined), 'summary');
  assert.equal(normalizeAkashicLevel('full'), 'full');
  assert.equal(normalizeAkashicLevel('verbose'), 'summary');
});

test('shouldWriteObservation keeps summary small and analysis focused', () => {
  assert.equal(shouldWriteObservation('summary', 'claude-sdk', 'result', 'success'), true);
  assert.equal(shouldWriteObservation('summary', 'claude-sdk', 'system', 'init'), true);
  assert.equal(shouldWriteObservation('summary', 'claude-sdk', 'assistant'), false);
  assert.equal(shouldWriteObservation('summary', 'claude-sdk', 'stream_event'), false);
  assert.equal(shouldWriteObservation('summary', 'claude-hook', 'hook', 'PreToolUse'), true);
  assert.equal(shouldWriteObservation('summary', 'claude-hook', 'hook', 'PostToolBatch'), false);
  // issue #47: user 定義 PreToolUse hook が block を返した hook_block / spawn 失敗の hook_spawn_error は
  // summary level でも常に残す (誰がどの tool を止めたか / hook が壊れていないかの監査価値が高いため)。
  assert.equal(shouldWriteObservation('summary', 'claude-hook', 'hook_block'), true);
  assert.equal(shouldWriteObservation('summary', 'claude-hook', 'hook_spawn_error'), true);
  assert.equal(shouldWriteObservation('summary', 'claude-control', 'context_usage'), true);
  assert.equal(shouldWriteObservation('summary', 'claude-control', 'context_usage_error'), true);
});

test('shouldWriteObservation full excludes only stream events and debug writes all', () => {
  assert.equal(shouldWriteObservation('full', 'claude-sdk', 'assistant'), true);
  assert.equal(shouldWriteObservation('full', 'claude-sdk', 'stream_event'), false);
  assert.equal(shouldWriteObservation('full', 'claude-hook', 'hook', 'PostToolBatch'), true);
  assert.equal(shouldWriteObservation('debug', 'claude-sdk', 'stream_event'), true);
});

test('context usage scheduler writes successful snapshots', async () => {
  const usages: unknown[] = [];
  const errors: Error[] = [];
  const scheduler = createContextUsageScheduler({
    getUsage: async () => ({ totalTokens: 42 }),
    writeUsage: (usage: unknown) => usages.push(usage),
    writeError: (error: Error) => errors.push(error),
    timeoutMs: 100,
  });

  await scheduler.schedule();

  assert.deepEqual(usages, [{ totalTokens: 42 }]);
  assert.deepEqual(errors, []);
});

test('context usage scheduler skips snapshots while one is in flight', async () => {
  let resolveUsage: (value: unknown) => void = () => {};
  let calls = 0;
  const usages: unknown[] = [];
  const scheduler = createContextUsageScheduler({
    getUsage: () => {
      calls += 1;
      return new Promise((resolve) => {
        resolveUsage = resolve;
      });
    },
    writeUsage: (usage: unknown) => usages.push(usage),
    writeError: () => {},
    timeoutMs: 100,
  });

  const first = scheduler.schedule();
  const second = scheduler.schedule();
  resolveUsage({ totalTokens: 7 });
  await Promise.all([first, second]);

  assert.equal(calls, 1);
  assert.deepEqual(usages, [{ totalTokens: 7 }]);
});

test('context usage scheduler writes one error when snapshot times out', async () => {
  const errors: Error[] = [];
  const scheduler = createContextUsageScheduler({
    getUsage: () => new Promise(() => {}),
    writeUsage: () => {},
    writeError: (error: Error) => errors.push(error),
    timeoutMs: 0,
  });

  await scheduler.schedule();

  assert.equal(errors.length, 1);
  assert.equal(errors[0]?.message, 'getContextUsage timed out');
});

test('context usage scheduler lets immediate snapshots replace pending delayed snapshots', async () => {
  let calls = 0;
  const usages: unknown[] = [];
  const scheduler = createContextUsageScheduler({
    getUsage: async () => {
      calls += 1;
      return { totalTokens: 99 };
    },
    writeUsage: (usage: unknown) => usages.push(usage),
    writeError: () => {},
    timeoutMs: 100,
  });

  const delayed = scheduler.schedule(50);
  const immediate = scheduler.schedule();
  await Promise.all([delayed, immediate]);

  assert.equal(calls, 1);
  assert.deepEqual(usages, [{ totalTokens: 99 }]);
});
