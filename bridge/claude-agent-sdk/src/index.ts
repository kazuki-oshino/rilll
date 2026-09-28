#!/usr/bin/env node

import readline from 'node:readline';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import { query } from '@anthropic-ai/claude-agent-sdk';
import type { HookCallback, Options, OutputFormat as SdkOutputFormat, Query } from '@anthropic-ai/claude-agent-sdk';
import {
  requestSchema,
  serializeEvent,
  type BridgeEvent,
  type RunRequest,
} from './protocol.js';
import {
  createContextUsageScheduler,
  normalizeAkashicLevel,
  shouldWriteObservation,
  type ContextUsageScheduler,
  type AkashicLevel,
} from './akashic.js';
// issue #65 B2: dangerous bash パターンと cwd 外 write 判定は shared/deny-patterns.ts に
// 集約済み。Codex bridge と完全に共通化することで、片側だけ更新されたときの drift を防ぐ。
import {
  isWriteTargetOutsideCwd,
  matchDangerousBashCommand,
} from '@rilll/bridge-protocol/deny-patterns';
// issue #66 A7: cwd 外 read 許可 (additionalDirectories) の解釈ルールは Codex bridge と共通化。
import { resolveAdditionalDirectories } from '@rilll/bridge-protocol/additional-directories';
import { buildCancelResultEvent } from './cancel.js';
import { userHookCallback } from './user-hook.js';
import { resolveClaudeEffort } from './effort.js';
import { startBridgeRunTelemetry } from '@rilll/bridge-protocol/telemetry';

// bridge-result-error-propagation Task 1.1: 単体テストから fake SDK / fake stdout / fake env を
// 注入できるよう、副作用 (query 構築 / stdout 書き込み / 環境変数読み出し) を deps として外出しする。
// codex bridge `CodexRuntimeDeps` / cursor-sdk bridge `CursorRuntimeDeps` と同形。
// defaultRuntimeDeps は実 process / 実 SDK を返し、production 経路では従来挙動を保つ。
export interface ClaudeRuntimeDeps {
  readonly env: Readonly<Record<string, string | undefined>>;
  readonly write: (chunk: string) => void;
  readonly query: typeof query;
}

const defaultRuntimeDeps: ClaudeRuntimeDeps = {
  env: process.env,
  write: (chunk) => {
    process.stdout.write(chunk);
  },
  query,
};

type ClaudeSystemInitMessage = {
  type: 'system';
  subtype: 'init';
  uuid?: string;
  session_id?: string;
  apiKeySource?: string;
  cwd?: string;
  tools?: string[];
  mcp_servers?: { name?: string; status?: string }[];
  model?: string;
  permissionMode?: string;
  slash_commands?: string[];
  output_style?: string;
};

let activeID: string | undefined;
let activeQuery: AsyncGenerator<unknown, void, unknown> | undefined;
let cancelled = false;
// memory-distiller-trigger-policy task 2.4: cancel handler が保持する起源値。
// finally ブロックで cancelled === true のとき、result event payload に透過するために参照する。
// run 開始 / 終了で undefined にリセットし、cancel 不在の正常終了では origin を残さない。
let cancelOrigin: 'user' | 'engine' | undefined;
let observationSequence = 0;
// bridge-result-error-propagation Task 1.1: NDJSON 出力先を runClaude 呼び出し時に
// deps.write へ差し替えるためのモジュール変数 (codex bridge `eventWriter` と同形)。
// 既定は default deps の write を指し、stdio bridge 起動時の従来挙動を保つ。
let eventWriter: ClaudeRuntimeDeps['write'] = defaultRuntimeDeps.write;

const maxObservationPayloadBytes = 8 * 1024 * 1024;
const contextUsageTimeoutMs = 5000;
const contextUsageDrainTimeoutMs = 500;
const initialContextUsageDelayMs = 1500;

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

function observationType(message: unknown): { type: string; subtype?: string } {
  if (typeof message !== 'object' || message === null) return { type: typeof message };
  const record = message as { type?: unknown; subtype?: unknown; hook_event_name?: unknown };
  const type = typeof record.type === 'string'
    ? record.type
    : typeof record.hook_event_name === 'string'
      ? 'hook'
      : 'unknown';
  const subtype = typeof record.subtype === 'string'
    ? record.subtype
    : typeof record.hook_event_name === 'string'
      ? record.hook_event_name
      : undefined;
  return { type, subtype };
}

function writeObservation(
  level: AkashicLevel,
  source: string,
  payload: unknown,
  explicitType?: string,
  explicitSubtype?: string,
  id = activeID ?? '',
): void {
  const inferred = observationType(payload);
  const type = explicitType ?? inferred.type;
  const subtype = explicitSubtype ?? inferred.subtype;
  if (!shouldWriteObservation(level, source, type, subtype)) return;
  const normalized = normalizeObservationPayload(payload);
  const event: BridgeEvent = {
    type: 'observation',
    id,
    source,
    observationType: type,
    observationSubtype: subtype,
    sequence: observationSequence++,
    payload: normalized.payload,
    payloadTruncated: normalized.truncated,
  };
  try {
    writeEvent(event);
  } catch (error) {
    writeEvent({
      type: 'observation',
      id,
      source: 'claude-bridge',
      observationType: 'observation_error',
      sequence: observationSequence++,
      payload: { message: error instanceof Error ? error.message : String(error) },
    });
  }
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

function observationHook(level: AkashicLevel): HookCallback {
  return async (input, toolUseID) => {
    writeObservation(level, 'claude-hook', { input, toolUseID }, 'hook', input.hook_event_name);
    return {};
  };
}

// canUseTool 受け側で deny 判定を行うエントリポイント。deny に該当する場合は
// 拒否理由文字列を返し、該当しない場合は undefined を返す (= allow に倒す)。
// パターン本体は shared/deny-patterns.ts に集約してあり、ここでは tool 名から
// 該当ブランチに振り分けるだけのアダプタ。
function evaluateDeny(toolName: string, input: unknown, cwd: string): string | undefined {
  const record = (typeof input === 'object' && input !== null) ? (input as Record<string, unknown>) : {};

  if (toolName === 'Write' || toolName === 'Edit' || toolName === 'MultiEdit') {
    const filePath = record.file_path;
    if (typeof filePath === 'string' && filePath !== '' && isWriteTargetOutsideCwd(filePath, cwd)) {
      return `${toolName}: cwd (${path.resolve(cwd)}) 外への書き込みは拒否されました: ${filePath}`;
    }
  }
  if (toolName === 'NotebookEdit') {
    const notebookPath = record.notebook_path;
    if (typeof notebookPath === 'string' && notebookPath !== '' && isWriteTargetOutsideCwd(notebookPath, cwd)) {
      return `${toolName}: cwd (${path.resolve(cwd)}) 外への書き込みは拒否されました: ${notebookPath}`;
    }
  }
  if (toolName === 'Bash') {
    const command = record.command;
    if (typeof command === 'string') {
      const reason = matchDangerousBashCommand(command);
      if (reason) {
        return `Bash deny: ${reason} (command="${command.slice(0, 200)}")`;
      }
    }
  }
  return undefined;
}

// Agent SDK v0.1.0 以降は settingSources を明示しないと .claude/settings(.local).json の
// permissions.allow を読まない。その結果 .claude/** への Write など acceptEdits でカバーされない
// 操作が ask に流れ、canUseTool 未定義のままだと SDK が
// "Claude requested permissions to write to ..., but you haven't granted it yet."
// を返してブロックする (akashic 調査で 5.1 / 6.5 タスクが 8 回連続でこの理由で停滞)。
// bridge は rilll engine 配下の自動 workflow 用 subprocess で対話 UI を持たないため、
// 通常は ask に到達した tool を allow し agent を継続させる。ただし上の evaluateDeny で
// ハードコードした「絶対やられたら困る」最小集合だけは deny に倒し、permission_deny event を
// observation として垂れ流して akashic で追跡可能にする。allow / deny いずれの判定も
// claude-bridge source の observation event として記録される。
function permissionAllowAll(level: AkashicLevel, cwd: string): NonNullable<Options['canUseTool']> {
  return async (toolName, input) => {
    const denyReason = evaluateDeny(toolName, input, cwd);
    if (denyReason) {
      writeObservation(level, 'claude-bridge', { toolName, input, reason: denyReason }, 'permission_deny');
      return { behavior: 'deny', message: denyReason, interrupt: false };
    }
    writeObservation(level, 'claude-bridge', { toolName, input }, 'permission_auto_allow');
    return { behavior: 'allow', updatedInput: input };
  };
}

function isSystemInitMessage(message: unknown): message is ClaudeSystemInitMessage {
  return (
    typeof message === 'object'
    && message !== null
    && 'type' in message
    && (message as { type?: unknown }).type === 'system'
    && 'subtype' in message
    && (message as { subtype?: unknown }).subtype === 'init'
  );
}

function toSystemInfo(message: ClaudeSystemInitMessage): BridgeEvent {
  return {
    type: 'system_init',
    id: activeID ?? '',
    systemInfo: {
      uuid: message.uuid,
      sessionId: message.session_id,
      apiKeySource: message.apiKeySource,
      cwd: message.cwd,
      tools: message.tools,
      mcpServers: message.mcp_servers?.map((server) => ({
        name: server.name,
        status: server.status,
      })),
      model: message.model,
      permissionMode: message.permissionMode,
      slashCommands: message.slash_commands,
      outputStyle: message.output_style,
    },
  };
}

function toSdkOutputFormat(format: RunRequest['outputFormat']): SdkOutputFormat | undefined {
  if (!format) return undefined;
  return format as SdkOutputFormat;
}


// bridge-result-error-propagation Task 1.1: deps オプション引数で test から SDK / writeEvent を
// 差し替えられるようにする (codex bridge `runCodex(req, deps?)` と同形)。
// production の stdio bridge 経路では defaultRuntimeDeps が使われ、従来挙動を保つ。
export async function runClaude(
  request: RunRequest,
  deps: ClaudeRuntimeDeps = defaultRuntimeDeps,
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
  const bridgeTelemetry = startBridgeRunTelemetry(request, 'claude');

  let result = '';
  let sessionId: string | undefined;
  let structuredOutput: unknown;
  let contextUsageScheduler: ContextUsageScheduler | undefined;

  try {
    const options: Options = {
      cwd: request.cwd,
      // output-contract-2phase task 2.1: per-call permissionMode 上書き seam。
      // request.permissionMode が指定されていればその値を SDK Options.permissionMode に流し、
      // 未指定 (undefined) のときは bridge 既定の 'acceptEdits' を維持する (後方互換)。
      // 値域は protocol の closed enum (`'default' | 'acceptEdits' | 'plan' | 'bypassPermissions'`)
      // と Claude Agent SDK の Options.permissionMode が 1:1 で揃っているため、追加の mapping は不要。
      // permissionMode='acceptEdits' でも .claude/** や新規 Bash 等は ask に流れる。
      // settingSources を渡していないため .claude/settings.local.json の allow rule も
      // 効かない。bridge は対話 UI を持たないので canUseTool で全 allow する。
      // ただし「絶対やられたら困る」最小集合 (cwd 外 Write / 致命的 Bash パターン) は
      // permissionAllowAll 内で deny に倒している (verify/claude-permission-smoke 試験段階)。
      permissionMode: request.permissionMode ?? 'acceptEdits',
      canUseTool: permissionAllowAll(akashicLevel, request.cwd),
      includePartialMessages: true,
      includeHookEvents: true,
      hooks: {
        // issue #47: PreToolUse のみ user 定義 hook の block 経路を許す (Phase 1)。
        // 既存 observationHook は最初の hook entry として維持し、akashic 観測経路を温存する。
        // user hook は entry を分けて matcher を SDK 側の正規表現マッチに任せる構成にし、
        // hook 同士の分離を保つ。block 判定は user hook entry 側でのみ行う。
        PreToolUse: [
          { hooks: [observationHook(akashicLevel)] },
          ...(request.userHooks ?? []).map((h) => ({
            matcher: h.matcher,
            hooks: [userHookCallback(akashicLevel, h.command, request.cwd, request.id, {
              writeEvent,
              writeObservation: (level, source, payload, explicitType, explicitSubtype) =>
                writeObservation(level, source, payload, explicitType, explicitSubtype),
            })],
          })),
        ],
        PostToolUse: [{ hooks: [observationHook(akashicLevel)] }],
        PostToolUseFailure: [{ hooks: [observationHook(akashicLevel)] }],
        PostToolBatch: [{ hooks: [observationHook(akashicLevel)] }],
        PermissionRequest: [{ hooks: [observationHook(akashicLevel)] }],
        PermissionDenied: [{ hooks: [observationHook(akashicLevel)] }],
      },
      ...(request.resumeSessionId ? { resume: request.resumeSessionId } : { continue: false }),
      ...(request.outputFormat ? { outputFormat: toSdkOutputFormat(request.outputFormat) } : {}),
      // kiro-spec-loop Phase C: fix-judge ターン等で Edit/Write を禁じるため
      // SDK Options.allowedTools に許可ツール一覧を素通し。
      // undefined / 空配列のときは SDK 既定 (全ツール許可) を維持する。
      ...(request.allowedTools && request.allowedTools.length > 0
        ? { allowedTools: request.allowedTools }
        : {}),
      // output-contract-2phase task 2.1: per-call maxTurns 上書き seam。
      // request.maxTurns が指定されていれば SDK Options.maxTurns に流し、未指定 (undefined) の
      // ときは Options からキー自体を omit して SDK 既定 turn budget を維持する (後方互換)。
      // protocol schema 側で positive integer に限定済みのため、ここでは追加検証はしない。
      ...(request.maxTurns !== undefined ? { maxTurns: request.maxTurns } : {}),
      // issue #65 A2: per-call reasoning effort を SDK Options.effort に流す。
      // protocol effortSchema は 'minimal' を含むが、Claude SDK EffortLevel は
      // 'low'|'medium'|'high'|'xhigh'|'max' のため 'minimal' は 'low' にマッピングする。
      // request.effort が undefined のときは rilll 側既定 (DEFAULT_CLAUDE_EFFORT = 'xhigh') を適用する
      // (SDK 既定値ではなく rilll bridge 層で reasoning を引き上げる方針)。
      effort: resolveClaudeEffort(request.effort),
      // issue #66 A7: cwd 外 read 許可。undefined は `['/']` (デフォルト許可)、
      // 空配列は restrict 意図として尊重 (Codex bridge と共通仕様 / resolveAdditionalDirectories)。
      //
      // Claude Agent SDK 仕様 (`@anthropic-ai/claude-agent-sdk` Options):
      //   additionalDirectories (string[], default []): "Additional directories Claude can access."
      // 既定は cwd のみへのアクセス。`'/'` を渡すとルート以下を read 可能ディレクトリとして追加する
      // (filesystem read 制御は read deny rule で別途絞れる)。ルート許可は Codex 側 sandbox 既定
      // (`danger-full-access`) との対称性のためで、Claude SDK 仕様としては副作用なし。
      additionalDirectories: resolveAdditionalDirectories(request.additionalDirectories),
    };

    bridgeTelemetry.addEvent('claude.query.start');
    const q = deps.query({ prompt: request.prompt, options });
    activeQuery = q as AsyncGenerator<unknown, void, unknown>;
    contextUsageScheduler = createContextUsageScheduler({
      getUsage: () => (q as Query).getContextUsage(),
      writeUsage: (usage) => writeObservation(
        akashicLevel,
        'claude-control',
        usage,
        'context_usage',
        undefined,
        request.id,
      ),
      writeError: (error) => writeObservation(
        akashicLevel,
        'claude-control',
        { message: error.message },
        'context_usage_error',
        undefined,
        request.id,
      ),
      timeoutMs: contextUsageTimeoutMs,
      drainTimeoutMs: contextUsageDrainTimeoutMs,
    });

    for await (const message of q) {
      if (cancelled) break;
      writeObservation(akashicLevel, 'claude-sdk', message);

      if (typeof message === 'object' && message !== null && 'session_id' in message) {
        const value = (message as { session_id?: unknown }).session_id;
        if (typeof value === 'string') sessionId = value;
      }

      if (isSystemInitMessage(message)) {
        writeEvent(toSystemInfo(message));
        void contextUsageScheduler.schedule(initialContextUsageDelayMs);
      }

      if (typeof message === 'object' && message !== null && (message as { type?: unknown }).type === 'assistant') {
        const content = (message as { message?: { content?: unknown[] } }).message?.content ?? [];
        for (const block of content) {
          if (
            typeof block === 'object'
            && block !== null
            && (block as { type?: unknown }).type === 'tool_use'
            && typeof (block as { name?: unknown }).name === 'string'
          ) {
            writeEvent({ type: 'tool_use', id: request.id, tool: (block as { name: string }).name });
          }
        }
      }

      if (typeof message === 'object' && message !== null && (message as { type?: unknown }).type === 'stream_event') {
        const event = (message as { event?: { type?: string; delta?: { type?: string; text?: string } } }).event;
        if (event?.type === 'content_block_delta' && event.delta?.type === 'text_delta' && event.delta.text) {
          writeEvent({ type: 'text', id: request.id, text: event.delta.text });
        }
        if (event?.type === 'message_stop') {
          void contextUsageScheduler.schedule();
        }
      }

      if (typeof message === 'object' && message !== null && (message as { type?: unknown }).type === 'result') {
        // bridge-result-error-propagation Task 2.1: SDK の SDKResultMessage を subtype に応じて
        // bridge event に振り分ける subtype dispatcher。
        // - subtype === 'success' は既存挙動を完全保持 (result event with result/structuredOutput/sessionId)。
        // - subtype === 'error_max_structured_output_retries' は parse 失敗扱いで result event を
        //   result='' / structuredOutput=undefined / sessionId 付きで emit (codex/cursor と対称契約)。
        // - subtype === 'error_during_execution' | 'error_max_turns' | 'error_max_budget_usd' は
        //   error event を code=<subtype> / retryable=false で emit する。
        // - 未知 subtype は defensive fallback として code='unknown' の error event を emit する
        //   (SDK enum の forward 互換)。
        // 全分岐の前段で実行される writeObservation(akashicLevel, 'claude-sdk', message) は
        // ループ先頭 (line 390) で既に呼ばれており、akashic 観測経路は維持される。
        // done event の emit (line 443) と finally の cancel result event 経路にも触らない。
        const resultMessage = message as {
          subtype?: string;
          result?: string;
          structured_output?: unknown;
          session_id?: string;
          errors?: unknown;
        };
        if (typeof resultMessage.session_id === 'string') sessionId = resultMessage.session_id;

        if (resultMessage.subtype === 'success') {
          result = resultMessage.result ?? '';
          structuredOutput = resultMessage.structured_output;
          writeEvent({
            type: 'result',
            id: request.id,
            result,
            sessionId,
            structuredOutput,
          });
        } else if (resultMessage.subtype === 'error_max_structured_output_retries') {
          // SDKResultError には result field が無いため空文字を載せる。
          // structuredOutput=undefined を運搬することで codex/cursor の parse 失敗契約と対称化する。
          // result 変数 (finally / done 経路) も空文字のまま保持する。
          structuredOutput = undefined;
          writeEvent({
            type: 'result',
            id: request.id,
            result: '',
            sessionId,
            structuredOutput: undefined,
          });
        } else if (
          resultMessage.subtype === 'error_during_execution'
          || resultMessage.subtype === 'error_max_turns'
          || resultMessage.subtype === 'error_max_budget_usd'
        ) {
          // SDK 由来の文字列があれば message に使う。SDKResultError.errors: string[] を持つ場合は
          // 連結し、無ければ subtype 文字列を message に流用する (regression 検知の cross-check 用途も兼ねる)。
          const sdkErrors = Array.isArray(resultMessage.errors)
            ? resultMessage.errors.filter((e): e is string => typeof e === 'string')
            : [];
          const messageText = sdkErrors.length > 0 ? sdkErrors.join('; ') : resultMessage.subtype;
          writeEvent({
            type: 'error',
            id: request.id,
            code: resultMessage.subtype,
            message: messageText,
            retryable: false,
          });
          // result 変数 (finally / done 経路) は空文字のまま保持する (success 以外では更新しない)。
        } else {
          // 未知 subtype の defensive fallback。SDK enum が将来拡張された場合の forward 互換。
          writeEvent({
            type: 'error',
            id: request.id,
            code: 'unknown',
            message: JSON.stringify(message),
            retryable: false,
          });
        }
      }
    }

    writeEvent({ type: 'done', id: request.id, result, sessionId });
  } catch (error) {
    bridgeTelemetry.recordError(error);
    writeProtocolError(request.id, error);
  } finally {
    await contextUsageScheduler?.drain();
    // memory-distiller-trigger-policy task 2.4: cancel が走った場合のみ result event payload に
    // origin を含めて運搬する。cancel 不在の正常終了は origin 省略 (Req 6 後方互換)。
    if (cancelled) {
      bridgeTelemetry.addEvent('bridge.cancelled', { origin: cancelOrigin });
      writeEvent(buildCancelResultEvent({
        id: request.id,
        origin: cancelOrigin,
        result,
        sessionId,
        structuredOutput,
      }));
    }
    activeID = undefined;
    activeQuery = undefined;
    cancelled = false;
    cancelOrigin = undefined;
    await bridgeTelemetry.end();
    eventWriter = previousEventWriter;
  }
}

// memory-distiller-trigger-policy task 2.4: cancel handler は req.origin を
// モジュールスコープの cancelOrigin に保持し、finally ブロックで result event payload に透過する。
// cancelRequestSchema.origin は zod の `.default('engine')` で常に解決済みのため、
// req.origin はランタイムでは必ず 'user' | 'engine' になる。
async function cancel(req: { id: string; origin: 'user' | 'engine' }): Promise<void> {
  if (activeID !== req.id) return;
  cancelled = true;
  cancelOrigin = req.origin;
  await activeQuery?.return?.();
}

// bridge-result-error-propagation Task 1.1: readline + stdio 副作用を関数に切り出し、
// main module 実行時のみ起動する構造に揃える (codex bridge `startStdioBridge` と同形)。
// test から import されたときには `ready` event の writeEvent も readline 購読も発動しない。
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
      // memory-distiller-trigger-policy task 2.4: cancel handler に origin を渡すため、
      // 解決済みの request.data ({ id, origin }) をそのまま受け渡す。
      void cancel(request.data);
      return;
    }

    void runClaude(request.data);
  });
}

function isMainModule(): boolean {
  return process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href;
}

if (isMainModule()) {
  startStdioBridge();
}
