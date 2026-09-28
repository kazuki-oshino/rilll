// output-contract-2phase Task 2.2: Codex bridge SDK の maxTurns / permissionMode 配線テスト。
//
// Codex SDK は SDK レベルで maxTurns / permissionMode のいずれも持たないため、bridge は以下の
// 戦略で 2 field を扱う:
//   - maxTurns: prompt 末尾に turn-budget 制約注記を best-effort fallback として追記する
//   - permissionMode: documented no-op (SDK 呼び出しに影響を与えない)
//
// 検証する 4 シナリオ:
//   1. maxTurns=2 → SDK へ渡る prompt 末尾に "turn budget" を含む System constraint が付与される
//   2. maxTurns 未指定 → SDK へ渡る prompt には turn-budget 注記が付与されない (= 後方互換)
//   3. permissionMode='plan' → ThreadOptions / TurnOptions / prompt が permissionMode 未指定時と
//      バイト一致する (= no-op)
//   4. buildPromptWithMaxTurns 単体: maxTurns 指定 / 未指定で挙動が正しい

import assert from 'node:assert/strict';
import test from 'node:test';
import type {
  Codex,
  CodexOptions,
  Thread,
  ThreadEvent,
  ThreadOptions,
  TurnOptions,
} from '@openai/codex-sdk';
import {
  buildPromptWithMaxTurns,
  runCodex,
  type CodexRuntimeDeps,
} from '../src/index.js';

interface Captured {
  readonly startThread: ThreadOptions[];
  readonly resumeThread: { id: string; options: ThreadOptions | undefined }[];
  readonly prompts: string[];
  readonly turnOptions: (TurnOptions | undefined)[];
}

function makeCodex(captured: Captured, fakeEvents: ThreadEvent[]): Codex {
  const buildThread = (): Thread => {
    const thread = {
      async runStreamed(input: unknown, turnOptions?: TurnOptions) {
        captured.prompts.push(String(input));
        captured.turnOptions.push(turnOptions);
        return {
          events: (async function* (): AsyncGenerator<ThreadEvent> {
            for (const ev of fakeEvents) yield ev;
          })(),
        };
      },
      async run() {
        throw new Error('not used');
      },
      get id(): string | null { return 'thread-mt'; },
    };
    return thread as unknown as Thread;
  };
  return {
    startThread(options?: ThreadOptions): Thread {
      captured.startThread.push(options ?? {});
      return buildThread();
    },
    resumeThread(id: string, options?: ThreadOptions): Thread {
      captured.resumeThread.push({ id, options });
      return buildThread();
    },
  } as unknown as Codex;
}

async function runWith(
  request: Parameters<typeof runCodex>[0],
): Promise<Captured> {
  const captured: Captured = {
    startThread: [],
    resumeThread: [],
    prompts: [],
    turnOptions: [],
  };
  const fakeEvents: ThreadEvent[] = [
    { type: 'thread.started', thread_id: 'thread-mt' },
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
    { type: 'item.completed', item: { id: 'msg', type: 'agent_message', text: 'ok' } },
  ];
  const deps: CodexRuntimeDeps = {
    env: {},
    write: () => undefined,
    createCodex: (_options: CodexOptions) => makeCodex(captured, fakeEvents),
  };
  await runCodex(request, deps);
  return captured;
}

// --- buildPromptWithMaxTurns 単体テスト ---

test('buildPromptWithMaxTurns: maxTurns 未指定 (undefined) なら prompt をそのまま返す', () => {
  assert.equal(buildPromptWithMaxTurns('hello', undefined), 'hello');
});

test('buildPromptWithMaxTurns: maxTurns=2 で turn budget 注記を末尾に追記する', () => {
  const out = buildPromptWithMaxTurns('hello', 2);
  // 元の prompt を破壊せず先頭に残す
  assert.ok(out.startsWith('hello'));
  // 注記には [System constraint] と "turn budget" / "2" が含まれる
  assert.match(out, /\[System constraint\]/);
  assert.match(out, /turn budget/i);
  assert.match(out, /\b2\b/);
});

test('buildPromptWithMaxTurns: maxTurns=1 でも turn budget 注記の数値が反映される', () => {
  const out = buildPromptWithMaxTurns('hi', 1);
  assert.ok(out.startsWith('hi'));
  assert.match(out, /\b1\b/);
});

// --- runCodex integration テスト ---

test('2.2 maxTurns=2 → SDK 渡し prompt 末尾に turn budget 注記が追記される', async () => {
  const captured = await runWith({
    type: 'run',
    id: 'req-mt-2',
    prompt: 'do the work',
    cwd: '/repo',
    akashicLevel: 'summary',
    maxTurns: 2,
  });
  assert.equal(captured.prompts.length, 1);
  const prompt = captured.prompts[0];
  // 元 prompt が残っていること
  assert.match(prompt, /^do the work/);
  // turn-budget 注記が追加されていること
  assert.match(prompt, /\[System constraint\]/);
  assert.match(prompt, /turn budget/i);
  assert.match(prompt, /\b2\b/);
});

test('2.2 maxTurns 未指定 → SDK 渡し prompt は turn-budget 注記なしで原文のまま (後方互換)', async () => {
  const captured = await runWith({
    type: 'run',
    id: 'req-mt-omit',
    prompt: 'do the work',
    cwd: '/repo',
    akashicLevel: 'summary',
  });
  assert.equal(captured.prompts.length, 1);
  const prompt = captured.prompts[0];
  // turn-budget 注記は存在しない
  assert.doesNotMatch(prompt, /turn budget/i);
  // allowedTools も未指定なので、prompt は原文と完全一致する
  assert.equal(prompt, 'do the work');
});

test('2.2 permissionMode=plan → 受理されるが Codex SDK 呼び出し (ThreadOptions / TurnOptions / prompt) は変化しない (no-op)', async () => {
  const baseline = await runWith({
    type: 'run',
    id: 'req-pm-baseline',
    prompt: 'work',
    cwd: '/repo',
    akashicLevel: 'summary',
  });
  const withPlan = await runWith({
    type: 'run',
    id: 'req-pm-plan',
    prompt: 'work',
    cwd: '/repo',
    akashicLevel: 'summary',
    permissionMode: 'plan',
  });
  // ThreadOptions / TurnOptions / prompt はすべて baseline と一致する (no-op)
  assert.deepEqual(withPlan.startThread, baseline.startThread);
  assert.deepEqual(withPlan.resumeThread, baseline.resumeThread);
  assert.deepEqual(withPlan.prompts, baseline.prompts);
  assert.deepEqual(withPlan.turnOptions, baseline.turnOptions);
});

test('2.2 permissionMode と maxTurns を同時指定 → permissionMode は no-op、maxTurns のみ prompt に反映', async () => {
  const captured = await runWith({
    type: 'run',
    id: 'req-pm-mt',
    prompt: 'work',
    cwd: '/repo',
    akashicLevel: 'summary',
    permissionMode: 'plan',
    maxTurns: 3,
  });
  const prompt = captured.prompts[0];
  // permissionMode は SDK 呼び出しに何も足さない
  assert.match(prompt, /^work/);
  // maxTurns の注記だけが追加される
  assert.match(prompt, /\[System constraint\]/);
  assert.match(prompt, /turn budget/i);
  assert.match(prompt, /\b3\b/);
});
