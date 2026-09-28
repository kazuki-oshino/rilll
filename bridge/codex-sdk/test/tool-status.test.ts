// issue #65 D6: 1 つの command_execution item が started/updated/completed の各段階で
// 個別の tool_use event として観測できることを fake Codex で検証する。

import assert from 'node:assert/strict';
import test from 'node:test';
import type {
  Codex,
  CodexOptions,
  Thread,
  ThreadEvent,
} from '@openai/codex-sdk';
import { runCodex, type CodexRuntimeDeps } from '../src/index.js';

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

async function runWithEvents(threadEvents: ThreadEvent[]): Promise<BridgeEvent[]> {
  const chunks: string[] = [];
  const deps: CodexRuntimeDeps = {
    env: {},
    write: (chunk) => { chunks.push(chunk); },
    createCodex: (_opts: CodexOptions) => makeCodex(threadEvents),
  };
  await runCodex({
    type: 'run',
    id: 'req-tool-status',
    prompt: 'do work',
    cwd: '/repo',
    akashicLevel: 'summary',
  }, deps);
  return chunks.join('').trim().split('\n').filter(Boolean).map((line) => JSON.parse(line) as BridgeEvent);
}

test('D6: one command_execution item is observed at started / updated / completed stages', async () => {
  const events = await runWithEvents([
    { type: 'thread.started', thread_id: 'thread-x' },
    {
      type: 'item.started',
      item: { id: 'cmd-1', type: 'command_execution', command: 'ls', aggregated_output: '', status: 'in_progress' },
    },
    {
      type: 'item.updated',
      item: { id: 'cmd-1', type: 'command_execution', command: 'ls', aggregated_output: 'foo\n', status: 'in_progress' },
    },
    {
      type: 'item.completed',
      item: { id: 'cmd-1', type: 'command_execution', command: 'ls', aggregated_output: 'foo\n', status: 'completed', exit_code: 0 },
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
  ]);

  const toolUses = events.filter((e) => e.type === 'tool_use' && e.tool === 'command_execution');
  assert.equal(toolUses.length, 3, '3 tool_use events should be emitted (one per stage)');
  assert.equal(toolUses[0].status, 'started');
  assert.equal(toolUses[1].status, 'updated');
  assert.equal(toolUses[2].status, 'completed');
});

test('D6: failed command_execution maps the completed stage to status="failed"', async () => {
  const events = await runWithEvents([
    { type: 'thread.started', thread_id: 'thread-x' },
    {
      type: 'item.started',
      item: { id: 'cmd-2', type: 'command_execution', command: 'false', aggregated_output: '', status: 'in_progress' },
    },
    {
      type: 'item.completed',
      item: { id: 'cmd-2', type: 'command_execution', command: 'false', aggregated_output: '', status: 'failed', exit_code: 1 },
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
  ]);

  const toolUses = events.filter((e) => e.type === 'tool_use' && e.tool === 'command_execution');
  assert.equal(toolUses.length, 2);
  assert.equal(toolUses[0].status, 'started');
  assert.equal(toolUses[1].status, 'failed');
});
