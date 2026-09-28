#!/usr/bin/env node

import readline from 'node:readline';
import { pathToFileURL } from 'node:url';
import { Agent, Cursor } from '@cursor/sdk';
import type {
  ModelSelection,
  Run,
  SDKAgent,
  SDKMessage,
  SendOptions,
} from '@cursor/sdk';
import {
  requestSchema,
  serializeEvent,
  type BridgeEvent,
  type RunRequest,
} from './protocol.js';
import {
  normalizeAkashicLevel,
  shouldWriteObservation,
  type AkashicLevel,
} from './akashic.js';
import {
  resolveModelSelection,
  resolvePreferredModelOptions,
} from './model.js';
import { buildCancelResultEvent } from './cancel.js';
import {
  hookObservationSource,
  toolHookSubtype,
  type ToolHookSubtype,
} from './hooks.js';
import {
  buildPromptWithSchema,
  parseStructuredOutput,
} from './structured.js';
import { resolveFinalResult } from './result.js';
import {
  buildSystemInitApiKeyFields,
  resolveCursorApiKey,
} from './api-key.js';
import {
  computeContextUsage,
  resolveContextWindow,
} from './context-usage.js';
import { startBridgeRunTelemetry } from '@rilll/bridge-protocol/telemetry';

const observationSource = 'cursor-sdk';
const controlObservationSource = 'cursor-control';
const maxObservationPayloadBytes = 8 * 1024 * 1024;
type ControlObservationType = 'resume_fallback' | 'context_usage' | 'context_usage_error';

export interface CursorRuntimeDeps {
  readonly env: Readonly<Record<string, string | undefined>>;
  readonly write: (chunk: string) => void;
  readonly cursor: {
    readonly models: {
      readonly list: typeof Cursor.models.list;
    };
  };
  readonly agent: {
    readonly resume: typeof Agent.resume;
    readonly create: typeof Agent.create;
  };
}

const defaultRuntimeDeps: CursorRuntimeDeps = {
  env: process.env,
  write: (chunk) => {
    process.stdout.write(chunk);
  },
  cursor: Cursor,
  agent: Agent,
};

// rilll 既定 (model 系設定の入口は TUI config に一本化済): provider が request.model を
// 必ず送ってくる前提のため、ここでは「provider が何も送らなかった場合の防御フォールバック」
// として composer-2.5 / reasoning 未指定をハードコードする。実運用では Go 側
// providerModelDefaultsFromConfig が request.model / request.effort を必ず注入するので、
// この fallback には到達しない (= TUI で選んだ preset がそのまま使われる)。
const bridgeFallbackModelId = 'composer-2.5';
const bridgeFallbackThinking = '';

let activeID: string | undefined;
let activeRun: Run | undefined;
let cancelled = false;
// memory-distiller-trigger-policy task 2.6: cancel handler が保持する起源値。
// finally ブロックで cancelled === true のとき、result event payload に透過するために参照する。
// run 開始 / 終了で undefined にリセットし、cancel 不在の正常終了では origin を残さない
// (claude / codex bridge と同形 / 3 provider 同一契約 / Req 3.2)。
let cancelOrigin: 'user' | 'engine' | undefined;
let observationSequence = 0;
let eventWriter = defaultRuntimeDeps.write;

function writeEvent(event: BridgeEvent): void {
  eventWriter(serializeEvent(event));
}

function writeProtocolError(id: string | undefined, error: unknown): void {
  writeEvent({
    type: 'error',
    id,
    message: error instanceof Error ? error.message : String(error),
    retryable: false,
  });
}

function normalizeObservationPayload(payload: unknown): { payload: unknown; truncated: boolean } {
  try {
    const serialized = JSON.stringify(payload);
    const bytes = Buffer.byteLength(serialized, 'utf8');
    if (bytes <= maxObservationPayloadBytes) return { payload, truncated: false };
    return {
      payload: {
        truncated: true,
        originalBytes: bytes,
        preview: serialized.slice(0, maxObservationPayloadBytes),
      },
      truncated: true,
    };
  } catch (error) {
    return {
      payload: {
        serializationError: error instanceof Error ? error.message : String(error),
      },
      truncated: true,
    };
  }
}

function writeObservation(
  level: AkashicLevel,
  type: string,
  payload: unknown,
  subtype?: string,
  id = activeID ?? '',
): void {
  if (!shouldWriteObservation(level, observationSource, type, subtype)) return;
  const normalized = normalizeObservationPayload(payload);
  writeEvent({
    type: 'observation',
    id,
    source: observationSource,
    observationType: type,
    observationSubtype: subtype,
    sequence: observationSequence++,
    payload: normalized.payload,
    payloadTruncated: normalized.truncated,
  });
}

// claude bridge の `claude-hook` source と対称な hook event を `cursor-hook` source で
// 吐く helper。akashic 側から provider 横断 (claude-hook / cursor-hook) の述語で
// PreToolUse/PostToolUse/PostToolUseFailure を検索できるよう、observationType='hook' で固定する。
// generic な writeObservation は source='cursor-sdk' に固定されているため、別経路として用意する。
// `toolHookSubtype` / `ToolHookSubtype` / `hookObservationSource` は副作用ゼロの ./hooks.js
// に切り出してある (テストから直接 import するため)。
function writeHookObservation(
  level: AkashicLevel,
  subtype: ToolHookSubtype,
  payload: unknown,
  id = activeID ?? '',
): void {
  if (!shouldWriteObservation(level, hookObservationSource, 'hook', subtype)) return;
  const normalized = normalizeObservationPayload(payload);
  writeEvent({
    type: 'observation',
    id,
    source: hookObservationSource,
    observationType: 'hook',
    observationSubtype: subtype,
    sequence: observationSequence++,
    payload: normalized.payload,
    payloadTruncated: normalized.truncated,
  });
}

function writeControlObservation(
  level: AkashicLevel,
  type: ControlObservationType,
  payload: unknown,
  id = activeID ?? '',
): void {
  if (!shouldWriteObservation(level, controlObservationSource, type)) return;
  const normalized = normalizeObservationPayload(payload);
  writeEvent({
    type: 'observation',
    id,
    source: controlObservationSource,
    observationType: type,
    sequence: observationSequence++,
    payload: normalized.payload,
    payloadTruncated: normalized.truncated,
  });
}

function toolNameForMessage(message: SDKMessage): string | undefined {
  if (message.type === 'tool_call') return message.name;
  return undefined;
}

function emitAssistantContent(
  requestId: string,
  message: Extract<SDKMessage, { type: 'assistant' }>,
  emittedToolIds: Set<string>,
): string {
  let text = '';
  for (const block of message.message.content) {
    if (block.type === 'text') {
      if (block.text) {
        writeEvent({ type: 'text', id: requestId, text: block.text });
        text += block.text;
      }
    } else if (block.type === 'tool_use') {
      if (!emittedToolIds.has(block.id)) {
        emittedToolIds.add(block.id);
        writeEvent({ type: 'tool_use', id: requestId, tool: block.name });
      }
    }
  }
  return text;
}

export async function runCursor(
  request: RunRequest,
  deps: CursorRuntimeDeps = defaultRuntimeDeps,
): Promise<void> {
  const previousEventWriter = eventWriter;
  eventWriter = deps.write;

  if (activeID) {
    writeEvent({
      type: 'error',
      id: request.id,
      message: `bridge is already running request ${activeID}`,
      retryable: true,
    });
    eventWriter = previousEventWriter;
    return;
  }

  activeID = request.id;
  cancelled = false;
  cancelOrigin = undefined;
  observationSequence = 0;
  const akashicLevel = normalizeAkashicLevel(request.akashicLevel);
  const bridgeTelemetry = startBridgeRunTelemetry(request, 'cursor-sdk');

  let result = '';
  let agentId: string | undefined;
  // task 2.6: cancel finally ブロックから参照できるよう外側スコープに持ち上げる。
  // 正常終了時は line 282 周辺で再代入され、cancel 時は undefined のまま透過する。
  let structuredOutput: unknown;
  const usageTotals = {
    inputTokens: 0,
    outputTokens: 0,
    cacheReadTokens: 0,
    cacheWriteTokens: 0,
  };
  let usageReceived = false;
  const emittedToolIds = new Set<string>();
  let agent: SDKAgent | undefined;

  try {
    const resolvedApiKey = resolveCursorApiKey({ env: deps.env });
    const { apiKey } = resolvedApiKey;
    const systemInitApiKeyFields = buildSystemInitApiKeyFields(resolvedApiKey);
    const preferredModel = resolvePreferredModelOptions({
      requestedModelId: request.model,
      defaultModelId: bridgeFallbackModelId,
      defaultThinking: bridgeFallbackThinking,
    });
    // issue #65 A2: per-call reasoning effort が指定されたら preferredThinking を上書きする。
    // resolvePreferredModelOptions は requestedModelId がある場合 preferredThinking を undefined にするが、
    // effort 明示時はその意図を尊重するため reasoning param value として注入する。
    // 'minimal'|'low'|'medium'|'high'|'xhigh' は resolveModelSelection 内 pickThinkingValue で
    // モデル許容値 (例: ['low','medium','high']) との近似マッチが行われる。
    const effectivePreferredThinking = request.effort ?? preferredModel.preferredThinking;
    const resolvedModel = await resolveModelSelection({
      preferredModelId: preferredModel.preferredModelId,
      preferredThinking: effectivePreferredThinking,
      requestedParams: request.modelParams,
      apiKey,
      listModels: (opts) => deps.cursor.models.list(opts),
    });
    const model: ModelSelection = resolvedModel.model;
    const modelsList = resolvedModel.modelsList;
    const contextWindowResolved = resolveContextWindow(model.id, modelsList);
    let contextUsageErrorEmitted = false;

    // output-contract-2phase task 2.3 (documented no-op): request.permissionMode と request.maxTurns
    // は Cursor SDK (@cursor/sdk) に直接対応する option / API が存在しないため、Cursor bridge では
    // **documented no-op** として受理するのみで Agent.create() / Agent.resume() / agent.send() の
    // どこにも一切流さない。protocol schema (bridge/shared/protocol.ts) は closed enum で受理
    // するため wire 上で reject されることはなく、Go 側 provider / agent 層からは 3 provider 共通の
    // 宣言 API として扱える (Req 6.1, 6.3)。
    //
    // Phase 2 のツール制限 (Write のみ許可など) は Cursor bridge では SDK レベルで実現できないため、
    // Phase 2 instruction 内 prompt fallback (= 上位 Phase 2 helper が組み立てる指示文に
    // "下記テンプレートに従って report を書き出す以外の作業はしない" 旨を明示する) で best-effort に
    // 担う (design.md §"Provider Native vs Fallback" / Req 6.3)。
    //
    // 本 no-op の挙動は test/permission-mode-and-max-turns.test.ts で「両 field を渡しても
    // Agent.create() / agent.send() の引数が baseline と完全一致する」ことを assert することで
    // regression guard をかけている (将来 SDK が permissionMode / maxTurns 相当 option を追加した
    // 場合は、本コメントを撤去のうえ test 側を「伝搬を assert する」方向に書き換える)。
    if (request.resumeSessionId) {
      try {
        agent = await deps.agent.resume(request.resumeSessionId, { apiKey, model });
      } catch (resumeError) {
        bridgeTelemetry.addEvent('cursor.resume_fallback');
        writeControlObservation(akashicLevel, 'resume_fallback', {
          requested_session_id: request.resumeSessionId,
          error_message: resumeError instanceof Error ? resumeError.message : String(resumeError),
        });
        agent = await deps.agent.create({
          apiKey,
          model,
          local: { cwd: request.cwd, settingSources: ['all'] },
        });
      }
    } else {
      agent = await deps.agent.create({
        apiKey,
        model,
        local: { cwd: request.cwd, settingSources: ['all'] },
      });
    }
    agentId = agent.agentId;

    // 1 回目の system_init: bridge 側が把握している分だけを通知する。
    // SDK の `system` SDKMessage には tools 等が含まれるが、それは stream 開始後
    // でないと届かない。先行 emit しておくことで Agent.resume などで SDK 側 system
    // メッセージが省かれた場合でも Go 側 (launcher.go ToProviderSystemInfo) に
    // 最低限の SystemInfo を渡せる (claude bridge と同じ取り回し)。
    writeEvent({
      type: 'system_init',
      id: request.id,
      systemInfo: {
        sessionId: agentId,
        cwd: request.cwd,
        model: model.id,
        ...systemInitApiKeyFields,
      },
    });

    const sendOptions: SendOptions = {
      onDelta: ({ update }) => {
        // turn-ended デルタには Cursor SDK が集計したターン単位の usage が乗る
        // RunResult には token 情報が無いため、ここで集計しないと取り出せない
        if (update.type === 'turn-ended' && update.usage) {
          usageTotals.inputTokens += update.usage.inputTokens ?? 0;
          usageTotals.outputTokens += update.usage.outputTokens ?? 0;
          usageTotals.cacheReadTokens += update.usage.cacheReadTokens ?? 0;
          usageTotals.cacheWriteTokens += update.usage.cacheWriteTokens ?? 0;
          usageReceived = true;
          if (contextWindowResolved) {
            writeControlObservation(
              akashicLevel,
              'context_usage',
              computeContextUsage({
                modelId: model.id,
                contextWindow: contextWindowResolved.contextWindow,
                resolution: contextWindowResolved.resolution,
                usageTotals,
              }),
            );
          } else if (!contextUsageErrorEmitted) {
            writeControlObservation(akashicLevel, 'context_usage_error', {
              model_id: model.id,
              reason: 'unknown_model',
            });
            contextUsageErrorEmitted = true;
          }
        }
      },
    };
    // Cursor SDK は outputFormat / json_schema を SDK options で受け取らないので、
    // bridge 層で prompt 末尾に schema を suffix として注入する (item 6)。
    // outputFormat 未指定 / 'text' のときは prompt をそのまま渡す (no-op パス)。
    const finalPrompt = buildPromptWithSchema(request.prompt, request.outputFormat);
    const run = await agent.send(finalPrompt, sendOptions);
    activeRun = run;

    for await (const message of run.stream() as AsyncGenerator<SDKMessage>) {
      if (cancelled) break;

      writeObservation(akashicLevel, message.type, message);

      switch (message.type) {
        case 'system':
          // 2 回目の system_init: SDK が `system` SDKMessage で tools / model.id 等の
          // ランタイム情報を運んでくるので、bridge 既知情報とマージして再 emit する。
          // Go 側 launcher.go は最後の system_init で response.SystemInfo を上書きするため、
          // tools が含まれた状態で akashic に記録される (claude bridge との同形)。
          writeEvent({
            type: 'system_init',
            id: request.id,
            systemInfo: {
              sessionId: message.agent_id ?? agentId,
              cwd: request.cwd,
              model: message.model?.id ?? model.id,
              ...systemInitApiKeyFields,
              tools: message.tools,
            },
          });
          break;
        case 'assistant': {
          const text = emitAssistantContent(request.id, message, emittedToolIds);
          if (text) result = text;
          break;
        }
        case 'tool_call': {
          const tool = toolNameForMessage(message);
          if (tool && !emittedToolIds.has(message.call_id)) {
            emittedToolIds.add(message.call_id);
            writeEvent({ type: 'tool_use', id: request.id, tool });
          }
          // claude bridge の PreToolUse / PostToolUse / PostToolUseFailure フックと同型の
          // observation を `cursor-hook` source で吐く。tool_call SDKMessage は status 遷移
          // ごとに発火するので、emittedToolIds の重複抑止とは独立に毎回書き込む。
          writeHookObservation(akashicLevel, toolHookSubtype(message.status), {
            toolName: message.name,
            status: message.status,
            args: message.args,
            result: message.result,
            callId: message.call_id,
          }, request.id);
          break;
        }
        case 'thinking':
          // observation(debug) として writeObservation で既に流している
          break;
        case 'status':
          writeObservation(akashicLevel, 'status', message, message.status);
          if (message.status === 'ERROR') {
            writeEvent({
              type: 'error',
              id: request.id,
              message: message.message ?? 'cursor agent reported ERROR status',
              retryable: false,
            });
          }
          break;
        case 'task':
          // 進捗マイルストーン。テキスト本文があれば text として通知して UX を補強する
          if (message.text) {
            writeEvent({ type: 'text', id: request.id, text: message.text });
          }
          break;
        case 'user':
        case 'request':
          // ユーザーメッセージのエコーと対話的入力要求は v1 では非対応。observation で見える
          break;
        default:
          break;
      }
    }

    const runResult = await run.wait();
    // Go 側 (internal/provider/cursorsdk) で snake_case を期待しているため、
    // claude/codex provider と揃えた payload 形状で通知する
    const usagePayload = {
      input_tokens: usageTotals.inputTokens,
      output_tokens: usageTotals.outputTokens,
      cache_read_tokens: usageTotals.cacheReadTokens,
      cache_write_tokens: usageTotals.cacheWriteTokens,
      duration_ms: runResult.durationMs,
      status: runResult.status,
      usage_available: usageReceived,
    };
    writeObservation(akashicLevel, 'usage', usagePayload, 'run');

    result = resolveFinalResult(result, runResult.result);

    if (request.outputFormat?.type === 'json_schema' && result) {
      structuredOutput = parseStructuredOutput(result);
    }

    writeEvent({
      type: 'result',
      id: request.id,
      result,
      sessionId: agentId,
      structuredOutput,
    });
    writeEvent({ type: 'done', id: request.id, result, sessionId: agentId });
  } catch (error) {
    bridgeTelemetry.recordError(error);
    writeProtocolError(request.id, error);
  } finally {
    // memory-distiller-trigger-policy task 2.6: cancel が走った場合のみ result event payload に
    // origin を含めて運搬する。cancel 不在の正常終了は origin 省略 (Req 6 後方互換)。
    // claude / codex bridge と同形 (3 provider 同一契約 / Req 3.2)。
    if (cancelled) {
      bridgeTelemetry.addEvent('bridge.cancelled', { origin: cancelOrigin });
      writeEvent(buildCancelResultEvent({
        id: request.id,
        origin: cancelOrigin,
        result,
        sessionId: agentId,
        structuredOutput,
      }));
    }
    activeID = undefined;
    activeRun = undefined;
    cancelled = false;
    cancelOrigin = undefined;
    if (agent) {
      try {
        agent.close();
      } catch {
        // close 失敗は致命的ではないので握りつぶす
      }
    }
    await bridgeTelemetry.end();
    eventWriter = previousEventWriter;
  }
}

// memory-distiller-trigger-policy task 2.6: cancel handler は req.origin を
// モジュールスコープの cancelOrigin に保持し、finally ブロックで result event payload に透過する。
// cancelRequestSchema.origin は zod の `.default('engine')` で常に解決済みのため、
// req.origin はランタイムでは必ず 'user' | 'engine' になる (claude / codex bridge と同形 / Req 3.2)。
async function cancel(req: { id: string; origin: 'user' | 'engine' }): Promise<void> {
  if (activeID !== req.id) return;
  cancelled = true;
  cancelOrigin = req.origin;
  if (activeRun) {
    try {
      await activeRun.cancel();
    } catch (err) {
      writeProtocolError(req.id, err);
    }
  }
}

function startStdioBridge(): void {
  const rl = readline.createInterface({
    input: process.stdin,
    crlfDelay: Infinity,
  });

  writeEvent({ type: 'ready' });

  rl.on('line', (line) => {
    const trimmed = line.trim();
    if (!trimmed) return;

    const parsedJson = (() => {
      try {
        return JSON.parse(trimmed) as unknown;
      } catch (error) {
        writeProtocolError(undefined, error);
        return undefined;
      }
    })();
    if (parsedJson === undefined) return;

    const request = requestSchema.safeParse(parsedJson);
    if (!request.success) {
      writeProtocolError(undefined, request.error);
      return;
    }

    if (request.data.type === 'cancel') {
      // memory-distiller-trigger-policy task 2.6: cancel handler に origin を渡すため、
      // 解決済みの request.data ({ id, origin }) をそのまま受け渡す (claude / codex bridge と同形)。
      void cancel(request.data);
      return;
    }

    void runCursor(request.data);
  });
}

function isMainModule(): boolean {
  return process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href;
}

if (isMainModule()) {
  startStdioBridge();
}
