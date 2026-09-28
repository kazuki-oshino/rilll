import assert from 'node:assert/strict';
import test from 'node:test';
import {
  eventSchema,
  requestSchema,
  serializeEvent,
} from '../src/protocol.js';
import {
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
    model: 'gpt-5-codex',
  });
  assert.equal(parsed.type, 'run');
  if (parsed.type !== 'run') return;
  assert.equal(parsed.model, 'gpt-5-codex');
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

test('requestSchema accepts Codex reasoning summary modes', () => {
  for (const reasoningSummary of ['auto', 'concise', 'detailed', 'none'] as const) {
    const parsed = requestSchema.parse({
      type: 'run',
      id: `req-${reasoningSummary}`,
      prompt: 'hello',
      cwd: '/repo',
      reasoningSummary,
    });
    assert.equal(parsed.type, 'run');
    if (parsed.type !== 'run') continue;
    assert.equal(parsed.reasoningSummary, reasoningSummary);
  }
});

test('requestSchema rejects unknown Codex reasoning summary modes', () => {
  assert.throws(() => requestSchema.parse({
    type: 'run',
    id: 'req-1',
    prompt: 'hello',
    cwd: '/repo',
    reasoningSummary: 'verbose',
  }));
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
  // memory-distiller-trigger-policy task 2.5: cancel(origin='user') 後の result event 透過を検証する。
  // codex bridge cancel handler が保持した origin を result event payload にそのまま乗せることを確認する
  // (claude bridge と同形の契約 / 両 provider 同一 fixture)。
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
  // memory-distiller-trigger-policy task 2.5 (Req 6 後方互換): cancel handler が origin を保持し損ねた
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
    source: 'codex-sdk',
    observationType: 'turn.completed',
    sequence: 1,
    payload: { type: 'turn.completed', usage: { input_tokens: 10, output_tokens: 5 } },
  });

  assert.equal(parsed.type, 'observation');
  assert.equal(parsed.source, 'codex-sdk');
  assert.equal(parsed.observationType, 'turn.completed');
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

test('shouldWriteObservation summary keeps only key codex events', () => {
  assert.equal(shouldWriteObservation('summary', 'codex-sdk', 'thread.started'), true);
  assert.equal(shouldWriteObservation('summary', 'codex-sdk', 'turn.completed'), true);
  assert.equal(shouldWriteObservation('summary', 'codex-sdk', 'usage'), true);
  assert.equal(shouldWriteObservation('summary', 'codex-sdk', 'turn.failed'), true);
  assert.equal(shouldWriteObservation('summary', 'codex-sdk', 'error'), true);
  assert.equal(shouldWriteObservation('summary', 'codex-sdk', 'item.started'), false);
  assert.equal(shouldWriteObservation('summary', 'codex-sdk', 'item.updated'), false);
  assert.equal(shouldWriteObservation('summary', 'codex-sdk', 'item.completed'), false);
  assert.equal(shouldWriteObservation('summary', 'codex-sdk', 'turn.started'), false);
});

test('shouldWriteObservation full excludes only noisy updates and debug writes all', () => {
  assert.equal(shouldWriteObservation('full', 'codex-sdk', 'item.completed'), true);
  assert.equal(shouldWriteObservation('full', 'codex-sdk', 'item.started'), true);
  assert.equal(shouldWriteObservation('full', 'codex-sdk', 'item.updated'), false);
  assert.equal(shouldWriteObservation('full', 'codex-sdk', 'turn.started'), false);
  assert.equal(shouldWriteObservation('debug', 'codex-sdk', 'item.updated'), true);
  assert.equal(shouldWriteObservation('debug', 'codex-sdk', 'turn.started'), true);
});
