// プロンプト先頭の /skill-name を $skill-name に正規化する前処理の検証。
// Claude Code 互換ワークフロー (例: agent.CallAgent(ctx, "/git-commit", ...)) を
// codex SDK の $skill 構文へ橋渡しするための薄いプリプロセッサ。

import assert from 'node:assert/strict';
import test from 'node:test';
import type {
  Codex,
  CodexOptions,
  Thread,
  ThreadEvent,
  ThreadOptions,
} from '@openai/codex-sdk';
import {
  buildPromptWithAllowedTools,
  normalizeSkillPrefix,
  runCodex,
  type CodexRuntimeDeps,
} from '../src/index.js';

test('normalizeSkillPrefix: 先頭の /skill-name を $skill-name に置換する', () => {
  assert.equal(normalizeSkillPrefix('/git-commit'), '$git-commit');
});

test('normalizeSkillPrefix: 後続の引数 (パス含む) は無傷で先頭スラッシュのみ置換する', () => {
  assert.equal(
    normalizeSkillPrefix('/agent-learnings-curator reflector /tmp/x'),
    '$agent-learnings-curator reflector /tmp/x',
  );
});

test('normalizeSkillPrefix: 既に $skill で始まる場合は変換しない', () => {
  assert.equal(normalizeSkillPrefix('$git-commit'), '$git-commit');
});

test('normalizeSkillPrefix: 空文字は変換しない', () => {
  assert.equal(normalizeSkillPrefix(''), '');
});

test('normalizeSkillPrefix: 先頭が空白の場合は変換しない', () => {
  assert.equal(normalizeSkillPrefix(' /git-commit'), ' /git-commit');
});

test('normalizeSkillPrefix: 通常の英文 (先頭がスラッシュでない) は変換しない', () => {
  assert.equal(
    normalizeSkillPrefix('Please run the build.'),
    'Please run the build.',
  );
});

test('normalizeSkillPrefix: コロン区切りのスキル名 (例 example:facet-builder) も置換される', () => {
  assert.equal(
    normalizeSkillPrefix('/example:facet-builder arg'),
    '$example:facet-builder arg',
  );
});

test('normalizeSkillPrefix: /<数字始まり> はスキル名形式ではないので変換しない', () => {
  // 正規表現は [a-zA-Z] 始まりに限定しているため、/1foo は対象外
  assert.equal(normalizeSkillPrefix('/1foo bar'), '/1foo bar');
});

test('buildPromptWithAllowedTools: 先頭 /skill を正規化し、allowedTools 制約注記を末尾に追記する', () => {
  const result = buildPromptWithAllowedTools('/git-commit', ['Bash', 'Read']);
  assert.match(result, /^\$git-commit/);
  assert.match(result, /Only use the following tools: Bash, Read\./);
});

test('buildPromptWithAllowedTools: allowedTools 未指定でも先頭 /skill は正規化される', () => {
  assert.equal(
    buildPromptWithAllowedTools('/git-commit', undefined),
    '$git-commit',
  );
});

test('buildPromptWithAllowedTools: allowedTools 空配列でも先頭 /skill は正規化される', () => {
  assert.equal(
    buildPromptWithAllowedTools('/git-commit', []),
    '$git-commit',
  );
});

test('buildPromptWithAllowedTools: 後続引数つきプロンプトでも先頭のみ置換する', () => {
  const result = buildPromptWithAllowedTools(
    '/agent-learnings-curator reflector /tmp/x',
    undefined,
  );
  assert.equal(result, '$agent-learnings-curator reflector /tmp/x');
});

// integration: codex SDK bridge (runCodex) の入口に `/git-commit` を渡したとき、
// 実際に Codex SDK の thread.runStreamed へ渡る prompt 第 1 引数が `$git-commit`
// 始まりになっていることを fake Codex で観察し、アカシックレコードに残らない
// prompt 経路を bridge 単体テストで担保する。
test('runCodex: /git-commit を渡すと thread.runStreamed には $git-commit が渡る', async () => {
  const capturedPrompts: string[] = [];

  function makeThread(): Thread {
    return {
      async runStreamed(input: unknown, _turnOptions?: unknown) {
        capturedPrompts.push(String(input));
        return {
          events: (async function* (): AsyncGenerator<ThreadEvent> {
            yield { type: 'thread.started', thread_id: 'fresh-thread' } as ThreadEvent;
            yield {
              type: 'item.completed',
              item: { id: 'msg-1', type: 'agent_message', text: 'ok' },
            } as ThreadEvent;
            yield {
              type: 'turn.completed',
              usage: {
                input_tokens: 1,
                cached_input_tokens: 0,
                cache_write_input_tokens: 0,
                output_tokens: 1,
                reasoning_output_tokens: 0,
              },
            } as ThreadEvent;
          })(),
        };
      },
      async run() {
        throw new Error('not used');
      },
      get id(): string | null {
        return 'fresh-thread';
      },
    } as unknown as Thread;
  }

  const fakeCodex: Codex = {
    startThread(_options?: ThreadOptions): Thread {
      return makeThread();
    },
    resumeThread(_id: string, _options?: ThreadOptions): Thread {
      return makeThread();
    },
  } as unknown as Codex;

  const deps: CodexRuntimeDeps = {
    env: {},
    write: () => {},
    createCodex: (_options: CodexOptions) => fakeCodex,
  };

  await runCodex({
    type: 'run',
    id: 'req-skill-1',
    prompt: '/git-commit',
    cwd: '/repo',
    akashicLevel: 'summary',
  }, deps);

  assert.equal(capturedPrompts.length, 1);
  assert.match(capturedPrompts[0], /^\$git-commit/);
  // 念のため元の "/git-commit" が prompt 先頭に残っていないことも確認する
  assert.equal(capturedPrompts[0].startsWith('/git-commit'), false);
});

test('runCodex: allowedTools 付きでも先頭が $skill に置換され、末尾制約注記は維持される', async () => {
  const capturedPrompts: string[] = [];

  function makeThread(): Thread {
    return {
      async runStreamed(input: unknown, _turnOptions?: unknown) {
        capturedPrompts.push(String(input));
        return {
          events: (async function* (): AsyncGenerator<ThreadEvent> {
            yield { type: 'thread.started', thread_id: 'fresh-thread' } as ThreadEvent;
            yield {
              type: 'item.completed',
              item: { id: 'msg-1', type: 'agent_message', text: 'ok' },
            } as ThreadEvent;
            yield {
              type: 'turn.completed',
              usage: {
                input_tokens: 1,
                cached_input_tokens: 0,
                cache_write_input_tokens: 0,
                output_tokens: 1,
                reasoning_output_tokens: 0,
              },
            } as ThreadEvent;
          })(),
        };
      },
      async run() {
        throw new Error('not used');
      },
      get id(): string | null {
        return 'fresh-thread';
      },
    } as unknown as Thread;
  }

  const fakeCodex: Codex = {
    startThread(): Thread {
      return makeThread();
    },
    resumeThread(): Thread {
      return makeThread();
    },
  } as unknown as Codex;

  const deps: CodexRuntimeDeps = {
    env: {},
    write: () => {},
    createCodex: (_options: CodexOptions) => fakeCodex,
  };

  await runCodex({
    type: 'run',
    id: 'req-skill-2',
    prompt: '/agent-learnings-curator reflector /tmp/x',
    cwd: '/repo',
    akashicLevel: 'summary',
    allowedTools: ['Bash', 'Read'],
  }, deps);

  assert.equal(capturedPrompts.length, 1);
  assert.match(capturedPrompts[0], /^\$agent-learnings-curator reflector \/tmp\/x/);
  assert.match(capturedPrompts[0], /Only use the following tools: Bash, Read\./);
});
