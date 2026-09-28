import assert from 'node:assert/strict';
import test from 'node:test';
import {
  buildSystemInitApiKeyFields,
  resolveCodexApiKey,
} from '../src/api-key.js';

test('resolveCodexApiKey prefers RILLL_CODEX_API_KEY when both env vars are set', () => {
  const result = resolveCodexApiKey({
    env: {
      RILLL_CODEX_API_KEY: 'rilll-key',
      OPENAI_API_KEY: 'openai-key',
    },
  });

  assert.deepEqual(result, {
    apiKey: 'rilll-key',
    source: 'RILLL_CODEX_API_KEY',
  });
});

test('resolveCodexApiKey uses RILLL_CODEX_API_KEY when it is the only non-empty key', () => {
  const result = resolveCodexApiKey({
    env: {
      RILLL_CODEX_API_KEY: 'rilll-key',
    },
  });

  assert.deepEqual(result, {
    apiKey: 'rilll-key',
    source: 'RILLL_CODEX_API_KEY',
  });
});

test('resolveCodexApiKey falls back to OPENAI_API_KEY when rilll-specific key is absent', () => {
  const result = resolveCodexApiKey({
    env: {
      OPENAI_API_KEY: 'openai-key',
    },
  });

  assert.deepEqual(result, {
    apiKey: 'openai-key',
    source: 'OPENAI_API_KEY',
  });
});

test('resolveCodexApiKey returns unspecified when no key is set', () => {
  const result = resolveCodexApiKey({ env: {} });

  assert.deepEqual(result, {
    apiKey: undefined,
    source: 'unspecified',
  });
});

test('resolveCodexApiKey treats empty strings as unset', () => {
  const result = resolveCodexApiKey({
    env: {
      RILLL_CODEX_API_KEY: '',
      OPENAI_API_KEY: 'openai-key',
    },
  });

  assert.deepEqual(result, {
    apiKey: 'openai-key',
    source: 'OPENAI_API_KEY',
  });
});

test('resolveCodexApiKey treats empty OPENAI_API_KEY as unset and falls through to unspecified', () => {
  const result = resolveCodexApiKey({
    env: {
      OPENAI_API_KEY: '',
    },
  });

  assert.deepEqual(result, {
    apiKey: undefined,
    source: 'unspecified',
  });
});

test('buildSystemInitApiKeyFields exposes the resolved source for system_init payloads', () => {
  const cases = [
    {
      env: { RILLL_CODEX_API_KEY: 'rilll-secret', OPENAI_API_KEY: 'openai-secret' },
      expectedSource: 'RILLL_CODEX_API_KEY',
      secret: 'rilll-secret',
    },
    {
      env: { OPENAI_API_KEY: 'openai-secret' },
      expectedSource: 'OPENAI_API_KEY',
      secret: 'openai-secret',
    },
    {
      env: {},
      expectedSource: 'unspecified',
      secret: 'openai-secret',
    },
  ] as const;

  for (const { env, expectedSource, secret } of cases) {
    const resolved = resolveCodexApiKey({ env });
    const fields = buildSystemInitApiKeyFields(resolved);
    const systemInitPayload = {
      type: 'system_init',
      systemInfo: {
        sessionId: 'thread-1',
        cwd: '/repo',
        apiKeySource: fields.apiKeySource,
      },
    };

    assert.deepEqual(fields, { apiKeySource: expectedSource });
    assert.equal(Object.hasOwn(fields, 'apiKey'), false);
    assert.equal(JSON.stringify(systemInitPayload).includes(secret), false);
  }
});
