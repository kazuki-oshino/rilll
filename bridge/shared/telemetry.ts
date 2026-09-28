import {
  context,
  propagation,
  SpanStatusCode,
  trace,
  type Span,
} from '@opentelemetry/api';
import {
  CompositePropagator,
  W3CBaggagePropagator,
  W3CTraceContextPropagator,
} from '@opentelemetry/core';
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-http';
import { BatchSpanProcessor } from '@opentelemetry/sdk-trace-base';
import { NodeTracerProvider } from '@opentelemetry/sdk-trace-node';
import type { RunRequest } from './protocol.js';

const instrumentationName = '@rilll/bridge';
const forceFlushTimeoutMs = 250;
const exportTimeoutMs = 200;

propagation.setGlobalPropagator(new CompositePropagator({
  propagators: [
    new W3CTraceContextPropagator(),
    new W3CBaggagePropagator(),
  ],
}));

let provider: NodeTracerProvider | undefined;
let initialized = false;

function telemetryEnabled(env: Readonly<Record<string, string | undefined>> = process.env): boolean {
  const explicit = env.RILLL_OTEL_ENABLED?.trim().toLowerCase();
  if (explicit === '1' || explicit === 'true' || explicit === 'yes' || explicit === 'on') return true;
  if (explicit === '0' || explicit === 'false' || explicit === 'no' || explicit === 'off') return false;
  return false;
}

function ensureProvider(): void {
  if (initialized) return;
  initialized = true;
  if (!telemetryEnabled()) return;

  provider = new NodeTracerProvider({
    spanProcessors: [
      new BatchSpanProcessor(new OTLPTraceExporter({ timeoutMillis: exportTimeoutMs })),
    ],
  });
  provider.register();
}

export type BridgeRunTelemetry = {
  readonly span: Span;
  addEvent(name: string, attrs?: Record<string, string | number | boolean | undefined>): void;
  recordError(error: unknown): void;
  end(): Promise<void>;
};

export function startBridgeRunTelemetry(request: RunRequest, providerId: string): BridgeRunTelemetry {
  ensureProvider();
  const parentContext = propagation.extract(context.active(), {
    traceparent: request.traceContext?.traceparent ?? '',
    tracestate: request.traceContext?.tracestate ?? '',
    baggage: request.traceContext?.baggage ?? '',
  });
  const span = trace.getTracer(instrumentationName).startSpan('bridge.run', {
    attributes: compactAttributes({
      'rilll.agent.provider_id': providerId,
      'rilll.workflow.run_id': request.metadata?.workflowRunId,
      'rilll.workflow.id': request.metadata?.workflowId,
      'rilll.workflow.step_id': request.metadata?.stepId,
      'rilll.agent.call_id': request.metadata?.agentCallId,
      'rilll.agent.model': request.model,
      'rilll.agent.effort': request.effort,
      'rilll.agent.resume_session_present': Boolean(request.resumeSessionId),
      'rilll.agent.output_format': request.outputFormat?.type,
    }),
  }, parentContext);

  return {
    span,
    addEvent(name, attrs = {}) {
      span.addEvent(name, compactAttributes(attrs));
    },
    recordError(error) {
      const message = 'bridge_run_failed';
      if (error instanceof Error) {
        span.recordException({ name: error.name, message });
        span.setStatus({ code: SpanStatusCode.ERROR, message });
        return;
      }
      span.recordException({ name: 'Error', message });
      span.setStatus({ code: SpanStatusCode.ERROR, message });
    },
    async end() {
      span.end();
      if (provider) {
        await forceFlushBestEffort(provider);
      }
    },
  };
}

async function forceFlushBestEffort(currentProvider: NodeTracerProvider): Promise<void> {
  let timeout: NodeJS.Timeout | undefined;
  try {
    await Promise.race([
      currentProvider.forceFlush().catch(() => undefined),
      new Promise<void>((resolve) => {
        timeout = setTimeout(resolve, forceFlushTimeoutMs);
      }),
    ]);
  } finally {
    if (timeout) {
      clearTimeout(timeout);
    }
  }
}

function compactAttributes(
  attrs: Record<string, string | number | boolean | undefined>,
): Record<string, string | number | boolean> {
  const out: Record<string, string | number | boolean> = {};
  for (const [key, value] of Object.entries(attrs)) {
    if (value !== undefined && value !== '') {
      out[key] = value;
    }
  }
  return out;
}
