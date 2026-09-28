import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import test from 'node:test';

import { startBridgeRunTelemetry } from '../dist/telemetry.js';

const invalidTraceId = '00000000000000000000000000000000';
const childExitTimeoutMs = 3_000;

test('bridge telemetry end is best-effort when OTLP export fails', async (t) => {
  const oldEnabled = process.env.RILLL_OTEL_ENABLED;
  const oldTraceEndpoint = process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT;
  t.after(() => {
    restoreEnv('RILLL_OTEL_ENABLED', oldEnabled);
    restoreEnv('OTEL_EXPORTER_OTLP_TRACES_ENDPOINT', oldTraceEndpoint);
  });
  process.env.RILLL_OTEL_ENABLED = 'true';
  process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT = 'http://127.0.0.1:1/v1/traces';

  const telemetry = startBridgeRunTelemetry({
    type: 'run',
    id: 'req-1',
    prompt: 'hello',
    cwd: process.cwd(),
    metadata: {},
  }, 'test-provider');

  const startedAt = Date.now();
  await assert.doesNotReject(telemetry.end());
  assert.ok(Date.now() - startedAt < 500, 'telemetry flush should not delay bridge completion');
});

test('bridge telemetry does not keep the process alive after export failure', async () => {
  const child = spawnTelemetryProbe(`
    await telemetry.end();
  `, {
    RILLL_OTEL_ENABLED: 'true',
    OTEL_EXPORTER_OTLP_TRACES_ENDPOINT: 'http://127.0.0.1:1/v1/traces',
  });

  const { exitCode } = await waitForChild(child);
  assert.equal(exitCode, 0);
});

test('bridge telemetry stays disabled without RILLL_OTEL_ENABLED even when OTLP endpoint is set', async () => {
  const child = spawnTelemetryProbe(`
    console.log(telemetry.span.spanContext().traceId);
    await telemetry.end();
  `, {
    RILLL_OTEL_ENABLED: '',
    OTEL_EXPORTER_OTLP_ENDPOINT: 'http://127.0.0.1:1',
    OTEL_EXPORTER_OTLP_TRACES_ENDPOINT: '',
    OTEL_EXPORTER_OTLP_METRICS_ENDPOINT: 'http://127.0.0.1:1/v1/metrics',
  }, ['ignore', 'pipe', 'ignore']);

  const { exitCode, stdout } = await waitForChild(child);
  assert.equal(exitCode, 0);
  assert.equal(stdout.trim(), invalidTraceId);
});

function spawnTelemetryProbe(body, env, stdio = 'ignore') {
  return spawn(process.execPath, [
    '--input-type=module',
    '-e',
    `
      const m = await import(new URL('../dist/telemetry.js', import.meta.url));
      const telemetry = m.startBridgeRunTelemetry({
        type: 'run',
        id: 'req-1',
        prompt: 'hello',
        cwd: process.cwd(),
        metadata: {},
      }, 'test-provider');
      ${body}
    `,
  ], {
    cwd: new URL('.', import.meta.url),
    env: {
      ...process.env,
      ...env,
    },
    stdio,
  });
}

async function waitForChild(child) {
  let stdout = '';
  child.stdout?.setEncoding('utf8');
  child.stdout?.on('data', (chunk) => {
    stdout += chunk;
  });

  const exitCode = await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => {
      child.kill();
      reject(new Error('telemetry child process did not exit promptly'));
    }, childExitTimeoutMs);
    child.once('error', (error) => {
      clearTimeout(timeout);
      reject(error);
    });
    child.once('exit', (code) => {
      clearTimeout(timeout);
      resolve(code);
    });
  });

  return { exitCode, stdout };
}

function restoreEnv(key, value) {
  if (value === undefined) {
    delete process.env[key];
    return;
  }
  process.env[key] = value;
}
