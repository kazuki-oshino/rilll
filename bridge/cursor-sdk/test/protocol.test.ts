import assert from 'node:assert/strict';
import test from 'node:test';
import {
  eventSchema,
  requestSchema,
  serializeEvent,
} from '../src/protocol.js';
import { buildCancelResultEvent } from '../src/cancel.js';
import { toolHookSubtype } from '../src/hooks.js';
import {
  buildPromptWithSchema,
  parseStructuredOutput,
} from '../src/structured.js';

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
    modelParams: [{ id: 'reasoning', value: 'max' }],
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
  assert.deepEqual(
    (parsed as { modelParams?: { id: string; value: string }[] }).modelParams,
    [{ id: 'reasoning', value: 'max' }],
  );
});

test('requestSchema accepts cancel requests', () => {
  const parsed = requestSchema.parse({ type: 'cancel', id: 'req-1' });
  assert.equal(parsed.type, 'cancel');
  assert.equal(parsed.id, 'req-1');
});

test('cancelRequestSchema parses origin=\'user\' literally', () => {
  // memory-distiller-trigger-policy task 2.6: cursor bridge も claude / codex と同形に
  // user 起源 cancel を literal として保持することを検証する (3 provider 同一契約)。
  const parsed = requestSchema.parse({ type: 'cancel', id: 'req-1', origin: 'user' });
  assert.equal(parsed.type, 'cancel');
  if (parsed.type !== 'cancel') return;
  assert.equal(parsed.origin, 'user');
});

test('cancelRequestSchema parses origin=\'engine\' literally', () => {
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
  // closed enum 'user' | 'engine' のため、未知値は zod が reject する (claude / codex と同形)。
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
  // memory-distiller-trigger-policy task 2.6: cancel(origin='user') 後の result event 透過を検証する。
  // cursor bridge cancel handler が保持した origin を result event payload にそのまま乗せることを確認する
  // (claude / codex bridge と同形の契約 / 3 provider 同一 fixture)。
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
  // memory-distiller-trigger-policy task 2.6 (Req 6 後方互換): cancel handler が origin を保持し損ねた
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

test('requestSchema rejects unknown request types', () => {
  assert.throws(() => requestSchema.parse({ type: 'bogus', id: 'req-1' }));
});

test('eventSchema accepts text and tool_use events', () => {
  const text = eventSchema.parse({ type: 'text', id: 'req-1', text: 'hi' });
  assert.equal(text.type, 'text');
  const tool = eventSchema.parse({ type: 'tool_use', id: 'req-1', tool: 'shell' });
  assert.equal(tool.type, 'tool_use');
});

test('eventSchema accepts observation events with cursor-sdk source', () => {
  const parsed = eventSchema.parse({
    type: 'observation',
    id: 'req-1',
    source: 'cursor-sdk',
    observationType: 'status',
    sequence: 1,
    payload: { status: 'FINISHED' },
  });

  assert.equal(parsed.type, 'observation');
  assert.equal(parsed.source, 'cursor-sdk');
  assert.equal(parsed.observationType, 'status');
});

test('eventSchema accepts done events', () => {
  const parsed = eventSchema.parse({
    type: 'done',
    id: 'req-1',
    result: 'ok',
    sessionId: 'agent-1',
  });
  assert.equal(parsed.type, 'done');
  assert.equal(parsed.sessionId, 'agent-1');
});

test('serializeEvent writes one JSON object per line', () => {
  assert.equal(
    serializeEvent({ type: 'done', id: 'req-1', result: 'ok' }),
    '{"type":"done","id":"req-1","result":"ok"}\n',
  );
});

test('toolHookSubtype maps SDK status to PreToolUse / PostToolUse / PostToolUseFailure', () => {
  // claude-hook の closed enum と対称 (item 5)。running → Pre, completed → Post,
  // error → PostToolUseFailure にマップし、未知値は安全側 PostToolUse に倒す。
  assert.equal(toolHookSubtype('running'), 'PreToolUse');
  assert.equal(toolHookSubtype('completed'), 'PostToolUse');
  assert.equal(toolHookSubtype('error'), 'PostToolUseFailure');
  assert.equal(toolHookSubtype('something-else'), 'PostToolUse');
});

test('buildPromptWithSchema returns prompt unchanged when outputFormat is undefined or text', () => {
  // item 6: outputFormat 未指定 / 'text' は no-op パス。prompt が破壊されないことを検証する。
  assert.equal(buildPromptWithSchema('hello', undefined), 'hello');
  assert.equal(buildPromptWithSchema('hello', { type: 'text' }), 'hello');
});

test('buildPromptWithSchema returns prompt unchanged when json_schema lacks schema body', () => {
  // schema が undefined のときは注入をスキップする (Cursor SDK に投げる文字列を壊さない)。
  assert.equal(
    buildPromptWithSchema('hello', { type: 'json_schema', schema: undefined }),
    'hello',
  );
});

test('buildPromptWithSchema appends JSON schema suffix when json_schema is provided', () => {
  // item 6: prompt 末尾に schema 注入。last-instruction-wins 傾向に乗せるため suffix。
  const schema = {
    type: 'object',
    required: ['ok'],
    properties: { ok: { type: 'boolean' } },
  };
  const result = buildPromptWithSchema('do thing', { type: 'json_schema', schema });
  // prompt 本体は先頭に残る
  assert.ok(result.startsWith('do thing\n'));
  // 強調指示が含まれる
  assert.ok(result.includes('Reply with **only** a single JSON value'));
  // schema 本体が JSON で含まれる
  assert.ok(result.includes('"required"'));
  assert.ok(result.includes('"ok"'));
});

test('parseStructuredOutput returns parsed JSON when text is pure JSON', () => {
  // 通常パス: モデルが指示通り JSON だけで返したケース。
  assert.deepEqual(parseStructuredOutput('{"ok":true}'), { ok: true });
  assert.deepEqual(parseStructuredOutput('  [1,2,3]  '), [1, 2, 3]);
});

test('parseStructuredOutput strips ```json fences before parsing', () => {
  // モデルが markdown フェンスを付けてしまったケースを救出する (item 6)。
  assert.deepEqual(parseStructuredOutput('```json\n{"ok":true}\n```'), { ok: true });
  assert.deepEqual(parseStructuredOutput('```\n{"ok":true}\n```'), { ok: true });
});

test('parseStructuredOutput extracts first JSON object/array from prose', () => {
  // "Here is the JSON: {...} thanks" のような前置き付きでも本体を救出する。
  assert.deepEqual(
    parseStructuredOutput('Here is the JSON: {"ok":true} thanks'),
    { ok: true },
  );
  assert.deepEqual(
    parseStructuredOutput('result: [1,2,3] end'),
    [1, 2, 3],
  );
});

test('parseStructuredOutput returns undefined when no JSON value can be extracted', () => {
  // 完全にテキストのみのケース (= structuredOutput 欠落として上位で扱う)。
  assert.equal(parseStructuredOutput('hello world'), undefined);
  assert.equal(parseStructuredOutput(''), undefined);
});
