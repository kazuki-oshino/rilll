// issue #66 A15+D7: ReasoningItem を専用 reasoning event として expose することを検証する。
// - item.started / item.updated / item.completed の最新スナップショットを変化時だけ emit する
// - reasoning text が result event の `result` フィールド (= agent_message text 経路) に混入しないこと
// - reasoning は tool_use event としては emit されない (toolNameForItem が undefined を返すため)

import assert from 'node:assert/strict';
import test from 'node:test';
import type {
  Codex,
  CodexOptions,
  Thread,
  ThreadEvent,
  ThreadOptions,
} from '@openai/codex-sdk';
import { runCodex, type CodexRuntimeDeps } from '../src/index.js';

interface BridgeEvent extends Record<string, unknown> {
  type: string;
}

function makeCodex(fakeEvents: ThreadEvent[]): Codex {
  const buildThread = (): Thread => {
    const thread = {
      async runStreamed() {
        return {
          events: (async function* (): AsyncGenerator<ThreadEvent> {
            for (const ev of fakeEvents) yield ev;
          })(),
        };
      },
      async run() {
        throw new Error('not used');
      },
      get id(): string | null { return 'thread-x'; },
    };
    return thread as unknown as Thread;
  };
  return {
    startThread(_options?: ThreadOptions): Thread {
      return buildThread();
    },
    resumeThread(_id: string, _options?: ThreadOptions): Thread {
      return buildThread();
    },
  } as unknown as Codex;
}

async function runWithEvents(fakeEvents: ThreadEvent[]): Promise<BridgeEvent[]> {
  const chunks: string[] = [];
  const deps: CodexRuntimeDeps = {
    env: {},
    write: (chunk) => { chunks.push(chunk); },
    createCodex: (_options: CodexOptions) => makeCodex(fakeEvents),
  };
  await runCodex({
    type: 'run',
    id: 'req-reasoning',
    prompt: 'hi',
    cwd: '/repo',
    akashicLevel: 'summary',
  }, deps);
  return chunks.join('').trim().split('\n').filter(Boolean).map((line) => JSON.parse(line) as BridgeEvent);
}

test('A15+D7: reasoning item.completed emits a reasoning event with id (run id) + itemId (ReasoningItem.id) + text', async () => {
  // id semantics は全 event 種別で run id に統一 / item-level identity は itemId に分離する設計 (protocol.ts コメント参照)。
  const events = await runWithEvents([
    { type: 'thread.started', thread_id: 'thread-x' },
    {
      type: 'item.completed',
      item: { id: 'reason-1', type: 'reasoning', text: 'I should call ls first' },
    },
    {
      type: 'turn.completed',
      usage: {
        input_tokens: 0,
        cached_input_tokens: 0,
        cache_write_input_tokens: 0,
        output_tokens: 0,
        reasoning_output_tokens: 0,
      },
    },
    { type: 'item.completed', item: { id: 'msg-1', type: 'agent_message', text: 'done' } },
  ]);
  const reasoning = events.find((e) => e.type === 'reasoning');
  assert.ok(reasoning, 'reasoning event should be emitted');
  assert.equal(reasoning.id, 'req-reasoning', 'id must carry run id (request.id), not item id');
  assert.equal(reasoning.itemId, 'reason-1', 'itemId must carry ReasoningItem.id');
  assert.equal(reasoning.text, 'I should call ls first');
});

test('A15+D7: reasoning text does NOT leak into result event result field', async () => {
  const events = await runWithEvents([
    { type: 'thread.started', thread_id: 'thread-x' },
    {
      type: 'item.completed',
      item: { id: 'reason-1', type: 'reasoning', text: 'I should think carefully' },
    },
    {
      type: 'turn.completed',
      usage: {
        input_tokens: 0,
        cached_input_tokens: 0,
        cache_write_input_tokens: 0,
        output_tokens: 0,
        reasoning_output_tokens: 0,
      },
    },
    { type: 'item.completed', item: { id: 'msg-1', type: 'agent_message', text: 'final answer' } },
  ]);
  const result = events.find((e) => e.type === 'result') as { result?: string } | undefined;
  assert.ok(result, 'result event should be emitted');
  // result event の result は agent_message のみで、reasoning text は混入しない。
  assert.equal(result.result, 'final answer');
});

test('A15+D7: reasoning item is NOT emitted as tool_use event', async () => {
  const events = await runWithEvents([
    { type: 'thread.started', thread_id: 'thread-x' },
    {
      type: 'item.started',
      item: { id: 'reason-1', type: 'reasoning', text: 'thinking...' },
    },
    {
      type: 'item.completed',
      item: { id: 'reason-1', type: 'reasoning', text: 'thinking done' },
    },
    {
      type: 'turn.completed',
      usage: {
        input_tokens: 0,
        cached_input_tokens: 0,
        cache_write_input_tokens: 0,
        output_tokens: 0,
        reasoning_output_tokens: 0,
      },
    },
    { type: 'item.completed', item: { id: 'msg-1', type: 'agent_message', text: 'ok' } },
  ]);
  const toolUseEvents = events.filter((e) => e.type === 'tool_use');
  assert.equal(toolUseEvents.length, 0, 'reasoning item must not emit tool_use events');
});

test('reasoning snapshots are emitted while an item is in progress', async () => {
  const events = await runWithEvents([
    { type: 'thread.started', thread_id: 'thread-x' },
    {
      type: 'item.started',
      item: { id: 'reason-1', type: 'reasoning', text: 'initial reasoning' },
    },
    {
      type: 'item.updated',
      item: { id: 'reason-1', type: 'reasoning', text: 'more complete reasoning' },
    },
    {
      type: 'turn.completed',
      usage: {
        input_tokens: 0,
        cached_input_tokens: 0,
        cache_write_input_tokens: 0,
        output_tokens: 0,
        reasoning_output_tokens: 0,
      },
    },
    { type: 'item.completed', item: { id: 'msg-1', type: 'agent_message', text: 'ok' } },
  ]);
  const reasoningEvents = events.filter((e) => e.type === 'reasoning');
  assert.deepEqual(
    reasoningEvents.map((event) => event.text),
    ['initial reasoning', 'more complete reasoning'],
  );
});

test('unchanged reasoning snapshots are deduplicated across lifecycle stages', async () => {
  const events = await runWithEvents([
    { type: 'thread.started', thread_id: 'thread-x' },
    {
      type: 'item.started',
      item: { id: 'reason-1', type: 'reasoning', text: 'thinking...' },
    },
    {
      type: 'item.updated',
      item: { id: 'reason-1', type: 'reasoning', text: 'thinking...' },
    },
    {
      type: 'item.completed',
      item: { id: 'reason-1', type: 'reasoning', text: 'thinking...' },
    },
    {
      type: 'turn.completed',
      usage: {
        input_tokens: 0,
        cached_input_tokens: 0,
        cache_write_input_tokens: 0,
        output_tokens: 0,
        reasoning_output_tokens: 0,
      },
    },
    { type: 'item.completed', item: { id: 'msg-1', type: 'agent_message', text: 'ok' } },
  ]);
  const reasoningEvents = events.filter((e) => e.type === 'reasoning');
  assert.equal(reasoningEvents.length, 1, `expected exactly 1 reasoning event, got ${reasoningEvents.length}`);
  assert.equal(reasoningEvents[0].id, 'req-reasoning', 'id must carry run id');
  assert.equal(reasoningEvents[0].itemId, 'reason-1', 'itemId must carry ReasoningItem.id');
  assert.equal(reasoningEvents[0].text, 'thinking...');
});

test('A15+D7: empty reasoning text does not emit a reasoning event', async () => {
  const events = await runWithEvents([
    { type: 'thread.started', thread_id: 'thread-x' },
    {
      type: 'item.started',
      item: { id: 'reason-1', type: 'reasoning', text: '' },
    },
    {
      type: 'item.completed',
      item: { id: 'reason-1', type: 'reasoning', text: '' },
    },
    {
      type: 'turn.completed',
      usage: {
        input_tokens: 0,
        cached_input_tokens: 0,
        cache_write_input_tokens: 0,
        output_tokens: 0,
        reasoning_output_tokens: 0,
      },
    },
    { type: 'item.completed', item: { id: 'msg-1', type: 'agent_message', text: 'ok' } },
  ]);
  const reasoningEvents = events.filter((e) => e.type === 'reasoning');
  assert.equal(reasoningEvents.length, 0, 'empty reasoning text should not emit a reasoning event');
});
