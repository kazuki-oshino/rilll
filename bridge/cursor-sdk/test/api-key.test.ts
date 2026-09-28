import assert from 'node:assert/strict';
import test from 'node:test';
import {
  buildSystemInitApiKeyFields,
  resolveCursorApiKey,
} from '../src/api-key.js';

test('resolveCursorApiKey prefers RILLL_CURSOR_API_KEY when both env vars are set', () => {
  const result = resolveCursorApiKey({
    env: {
      RILLL_CURSOR_API_KEY: 'rilll-key',
      CURSOR_API_KEY: 'cursor-key',
    },
  });

  assert.deepEqual(result, {
    apiKey: 'rilll-key',
    source: 'RILLL_CURSOR_API_KEY',
  });
});

test('resolveCursorApiKey uses RILLL_CURSOR_API_KEY when it is the only non-empty key', () => {
  const result = resolveCursorApiKey({
    env: {
      RILLL_CURSOR_API_KEY: 'rilll-key',
    },
  });

  assert.deepEqual(result, {
    apiKey: 'rilll-key',
    source: 'RILLL_CURSOR_API_KEY',
  });
});

test('resolveCursorApiKey falls back to CURSOR_API_KEY when rilll-specific key is absent', () => {
  const result = resolveCursorApiKey({
    env: {
      CURSOR_API_KEY: 'cursor-key',
    },
  });

  assert.deepEqual(result, {
    apiKey: 'cursor-key',
    source: 'CURSOR_API_KEY',
  });
});

test('resolveCursorApiKey returns unspecified when no key is set', () => {
  const result = resolveCursorApiKey({ env: {} });

  assert.deepEqual(result, {
    apiKey: undefined,
    source: 'unspecified',
  });
});

test('resolveCursorApiKey treats empty strings as unset', () => {
  const result = resolveCursorApiKey({
    env: {
      RILLL_CURSOR_API_KEY: '',
      CURSOR_API_KEY: 'cursor-key',
    },
  });

  assert.deepEqual(result, {
    apiKey: 'cursor-key',
    source: 'CURSOR_API_KEY',
  });
});

test('buildSystemInitApiKeyFields exposes the resolved source for system_init payloads', () => {
  const cases = [
    {
      env: { RILLL_CURSOR_API_KEY: 'rilll-secret', CURSOR_API_KEY: 'cursor-secret' },
      expectedSource: 'RILLL_CURSOR_API_KEY',
      secret: 'rilll-secret',
    },
    {
      env: { CURSOR_API_KEY: 'cursor-secret' },
      expectedSource: 'CURSOR_API_KEY',
      secret: 'cursor-secret',
    },
    {
      env: {},
      expectedSource: 'unspecified',
      secret: 'cursor-secret',
    },
  ] as const;

  for (const { env, expectedSource, secret } of cases) {
    const resolved = resolveCursorApiKey({ env });
    const fields = buildSystemInitApiKeyFields(resolved);
    const systemInitPayload = {
      type: 'system_init',
      systemInfo: {
        sessionId: 'agent-1',
        cwd: '/repo',
        model: 'gpt-5.5',
        ...fields,
      },
    };

    assert.deepEqual(fields, { apiKeySource: expectedSource });
    assert.equal(Object.hasOwn(fields, 'apiKey'), false);
    assert.equal(JSON.stringify(systemInitPayload).includes(secret), false);
  }
});
