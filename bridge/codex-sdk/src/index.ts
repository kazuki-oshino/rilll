#!/usr/bin/env node

import { randomUUID } from 'node:crypto';
import readline from 'node:readline';
import { pathToFileURL } from 'node:url';
import { Codex } from '@openai/codex-sdk';
import type {
  CodexOptions,
  Thread,
  ThreadEvent,
  ThreadOptions,
  ThreadItem,
  TurnOptions,
} from '@openai/codex-sdk';
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
import { buildCancelResultEvent } from './cancel.js';
import {
  buildSystemInitApiKeyFields,
  resolveCodexApiKey,
} from './api-key.js';
import {
  computeContextUsage,
  resolveContextWindow,
} from './context-window.js';
// issue #65 B1/B2/B3: Codex SDK には canUseTool / PreToolUse の事前介入経路が無いため、
// item.started / item.updated を観測経路として、deny gate / allowedTools 違反 / user hook を
// 評価し AbortController.abort() で run を切る (post-execution kill switch)。
import { evaluateDeny, type DenyResult } from './deny.js';
// issue #66 A7: cwd 外 read 許可 (additionalDirectories) の解釈ルールは Claude bridge と共通化。
import { resolveAdditionalDirectories } from '@rilll/bridge-protocol/additional-directories';
import { startBridgeRunTelemetry } from '@rilll/bridge-protocol/telemetry';
import {
  evaluateUserHooks,
  spawnUserHook,
  toolNamesForCodexItem,
  type UserHookEvaluation,
} from './user-hook.js';

const observationSource = 'codex-sdk';
const hookObservationSource = 'codex-hook';
const maxObservationPayloadBytes = 8 * 1024 * 1024;

// Issue #64 B5: Codex SDK は thread.started に model を載せないため、bridge 側で
// modelId を解決する。優先順: request.model → RILLL_CODEX_MODEL → 既定値 (gpt-5-codex)。
// 既定値は Codex CLI のデフォルトモデルと一致させ、推定 context_usage の主観を
// 一致させる (table 未収載モデルでは context_usage_error を 1 回だけ emit する)。
const defaultModelId = 'gpt-5-codex';

// Issue #64 A16+D4: 単体テストから fake Codex を注入できるよう、
// 副作用 (Codex 構築 / stdout 書き込み / 環境変数読み出し) を deps として外出しする。
// defaultRuntimeDeps は実 process / 実 SDK を返し、production 経路では従来挙動を保つ。
export interface CodexRuntimeDeps {
  readonly env: Readonly<Record<string, string | undefined>>;
  readonly write: (chunk: string) => void;
  readonly createCodex: (options: CodexOptions) => Codex;
  // issue #65 N1: テストから user hook の spawn を差し替えるための seam (省略時は実装の spawnUserHook を使う)。
  readonly spawnUserHook?: typeof spawnUserHook;
}

const defaultRuntimeDeps: CodexRuntimeDeps = {
  env: process.env,
  write: (chunk) => {
    process.stdout.write(chunk);
  },
  createCodex: (options) => new Codex(options),
};

let activeID: string | undefined;
let activeAbort: AbortController | undefined;
let cancelled = false;
// memory-distiller-trigger-policy task 2.5: cancel handler が保持する起源値。
// finally ブロックで cancelled === true のとき、result event payload に透過するために参照する。
// run 開始 / 終了で undefined にリセットし、cancel 不在の正常終了では origin を残さない
// (claude bridge と同形 / 両 provider 同一契約 / Req 3.2)。
let cancelOrigin: 'user' | 'engine' | undefined;
let observationSequence = 0;
let eventWriter: CodexRuntimeDeps['write'] = defaultRuntimeDeps.write;

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

// issue #65 B1: source='codex-hook' の observation を書くヘルパ。Claude bridge `claude-hook` と
// 同じ source カテゴリで akashic 解析できるようにする (hook_block / hook_spawn_error)。
function writeHookObservation(
  level: AkashicLevel,
  type: string,
  payload: unknown,
  subtype?: string,
  id = activeID ?? '',
): void {
  if (!shouldWriteObservation(level, hookObservationSource, type, subtype)) return;
  const normalized = normalizeObservationPayload(payload);
  writeEvent({
    type: 'observation',
    id,
    source: hookObservationSource,
    observationType: type,
    observationSubtype: subtype,
    sequence: observationSequence++,
    payload: normalized.payload,
    payloadTruncated: normalized.truncated,
  });
}

function toolNameForItem(item: ThreadItem): string | undefined {
  switch (item.type) {
    case 'command_execution':
      return 'command_execution';
    case 'file_change':
      return 'file_change';
    case 'mcp_tool_call':
      return `mcp:${item.server}/${item.tool}`;
    case 'web_search':
      return 'web_search';
    case 'todo_list':
      return 'todo_list';
    default:
      return undefined;
  }
}

// issue #65 D6: ThreadEvent.type と item.status から tool_use event の status を導く。
// item.started/updated の段階では status を素直にマップ。item.completed の段階で
// item.status === 'failed' (command_execution / file_change / mcp_tool_call で発生し得る) なら
// 'failed' に倒す (Codex SDK 仕様: PatchApplyStatus / McpToolCallStatus / CommandExecutionStatus)。
function toolStatusForEvent(
  eventType: 'item.started' | 'item.updated' | 'item.completed',
  item: ThreadItem,
): 'started' | 'updated' | 'completed' | 'failed' {
  if (eventType === 'item.completed') {
    if (
      (item.type === 'command_execution' && item.status === 'failed')
      || (item.type === 'file_change' && item.status === 'failed')
      || (item.type === 'mcp_tool_call' && item.status === 'failed')
    ) {
      return 'failed';
    }
    return 'completed';
  }
  if (eventType === 'item.updated') return 'updated';
  return 'started';
}

function isAgentMessage(item: ThreadItem): item is Extract<ThreadItem, { type: 'agent_message' }> {
  return item.type === 'agent_message';
}

// issue #66 A15+D7: reasoning item の type guard。専用 event 経路を扱う分岐で参照する。
function isReasoning(item: ThreadItem): item is Extract<ThreadItem, { type: 'reasoning' }> {
  return item.type === 'reasoning';
}

// issue #65 B3: ThreadItem の論理 tool 名 (toolNamesForCodexItem の戻り値) のいずれかが
// allowedTools に含まれていれば違反なし。1 つも含まれていなければ違反として情報を返す。
// allowedTools が undefined / 空のときは制限なしで素通し (= undefined を返す)。
// agent_message / reasoning など tool 名を持たない item は違反対象外で素通し。
export function evaluateAllowedToolsViolation(
  item: ThreadItem,
  allowedTools: readonly string[] | undefined,
): { reason: string; toolNames: string[] } | undefined {
  if (!allowedTools || allowedTools.length === 0) return undefined;
  const toolNames = toolNamesForCodexItem(item);
  if (toolNames.length === 0) return undefined;
  const matched = toolNames.some((name) => allowedTools.includes(name));
  if (matched) return undefined;
  return {
    reason: `tool "${toolNames[0]}" is not in allowedTools (${allowedTools.join(', ')})`,
    toolNames,
  };
}

// Claude Code 互換ワークフローは prompt 先頭にスキル名を `/skill-name` 形式で
// 単独もしくは引数つきで渡してくる (例: internal/workflow/builtin/git/commit.go の
// agent.CallAgent(ctx, "/git-commit", ...))。codex SDK 側は `$skill-name` 構文で
// スキルを発動するため、bridge 入口で先頭スラッシュ 1 個のみを `$` に書き換える。
// 本文中盤に出現する `/skill` 風文字列やパス引数は触らない (先頭限定の単発置換)。
export function normalizeSkillPrefix(prompt: string): string {
  return prompt.replace(/^\/([a-zA-Z][\w:-]*)/, (_match, name: string) => `$${name}`);
}

type JsonObject = Record<string, unknown>;

function isJsonObject(value: unknown): value is JsonObject {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isObjectSchema(schema: JsonObject): boolean {
  const type = schema.type;
  if (type === 'object') return true;
  if (Array.isArray(type) && type.includes('object')) return true;
  return isJsonObject(schema.properties);
}

function requiredNameSet(required: unknown): Set<string> {
  if (!Array.isArray(required)) return new Set();
  return new Set(required.filter((name): name is string => typeof name === 'string'));
}

function schemaAllowsNull(schema: JsonObject): boolean {
  const type = schema.type;
  if (type === 'null') return true;
  if (Array.isArray(type) && type.includes('null')) return true;
  const anyOf = schema.anyOf;
  return Array.isArray(anyOf)
    && anyOf.some((candidate) => isJsonObject(candidate) && candidate.type === 'null');
}

function withNullEnumValue(schema: JsonObject): JsonObject {
  const enumValues = schema.enum;
  if (!Array.isArray(enumValues) || enumValues.includes(null)) return schema;
  return { ...schema, enum: [...enumValues, null] };
}

function makeNullableSchema(schema: unknown): unknown {
  if (!isJsonObject(schema)) {
    return { anyOf: [schema, { type: 'null' }] };
  }
  if (schemaAllowsNull(schema)) {
    return withNullEnumValue(schema);
  }

  const type = schema.type;
  if (typeof type === 'string') {
    return withNullEnumValue({ ...schema, type: [type, 'null'] });
  }
  if (Array.isArray(type)) {
    return withNullEnumValue({ ...schema, type: [...type, 'null'] });
  }

  const anyOf = schema.anyOf;
  if (Array.isArray(anyOf)) {
    return { ...schema, anyOf: [...anyOf, { type: 'null' }] };
  }
  return { anyOf: [schema, { type: 'null' }] };
}

function normalizeSchemaNode(schema: unknown): unknown {
  if (Array.isArray(schema)) {
    return schema.map((item) => normalizeSchemaNode(item));
  }
  if (!isJsonObject(schema)) return schema;

  const normalized: JsonObject = {};
  const originallyRequired = requiredNameSet(schema.required);
  for (const [key, value] of Object.entries(schema)) {
    if (key === 'properties' && isJsonObject(value)) {
      const properties: JsonObject = {};
      for (const [propertyName, propertySchema] of Object.entries(value)) {
        const normalizedProperty = normalizeSchemaNode(propertySchema);
        properties[propertyName] = originallyRequired.has(propertyName)
          ? normalizedProperty
          : makeNullableSchema(normalizedProperty);
      }
      normalized.properties = properties;
      continue;
    }
    normalized[key] = normalizeSchemaNode(value);
  }

  if (isObjectSchema(schema)) {
    normalized.additionalProperties = false;
    if (isJsonObject(schema.properties)) {
      normalized.required = Object.keys(schema.properties);
    }
  }

  return normalized;
}

// Codex SDK passes outputSchema directly to the OpenAI structured-output path.
// That path is stricter than rilll's provider-level OutputFormat contract: every
// object schema must reject extra keys, and all declared properties must be
// required. Bridge-level normalization keeps those SDK quirks out of shared
// workflow schemas while preserving optional fields as nullable values.
export function normalizeCodexOutputSchema(schema: unknown): unknown {
  return normalizeSchemaNode(schema);
}

// issue #65 B3: allowedTools が指定された run の prompt 末尾に注記を加え、agent 側で
// self-restraint を発動できるようにする。Codex SDK は SDK レベルの allowedTools 経路を
// 持たないため、bridge 側で「prompt で通知 + item.started 段で違反検知 → abort」の
// 二段構えにする。B3 仕様に従い "Only use the following tools: X, Y, Z" 形式で付与する。
// 末尾追記の前に normalizeSkillPrefix で先頭スキル記法を Codex SDK 構文へ揃える。
export function buildPromptWithAllowedTools(
  prompt: string,
  allowedTools: readonly string[] | undefined,
): string {
  const normalized = normalizeSkillPrefix(prompt);
  if (!allowedTools || allowedTools.length === 0) return normalized;
  const constraint = `\n\n[System constraint] Only use the following tools: ${allowedTools.join(', ')}. Other tool use will be rejected and the run aborted.`;
  return `${normalized}${constraint}`;
}

// output-contract-2phase task 2.2: Codex SDK には maxTurns に直接対応する option がない
// (ThreadOptions / TurnOptions のいずれにも turn-budget field が存在しない) ため、
// best-effort fallback として prompt 末尾に turn-budget 制約注記を追記し、agent 側に
// self-restraint を促す。SDK 側の強制力はないので、上位 (Phase 2 helper / akashic 観測)
// で turn 数の実測 / 異常検知をあわせて行う運用前提。
// maxTurns が undefined のときは原文を破壊せずそのまま返す (= 後方互換 / SDK 既定維持)。
// 0 / 負数は protocol schema 側 (z.number().int().positive().optional()) で reject 済みのため、
// ここでは正の整数のみが渡る想定だが、防御的に <= 0 も no-op に倒す。
export function buildPromptWithMaxTurns(
  prompt: string,
  maxTurns: number | undefined,
): string {
  if (maxTurns === undefined || maxTurns <= 0) return prompt;
  const constraint = `\n\n[System constraint] You have a strict turn budget of ${maxTurns} turn(s) for this run. Complete the task within this budget; do not request additional turns.`;
  return `${prompt}${constraint}`;
}

function resolveModelId(request: RunRequest, env: CodexRuntimeDeps['env']): string {
  const fromRequest = request.model?.trim();
  if (fromRequest) return fromRequest;
  const fromEnv = env.RILLL_CODEX_MODEL?.trim();
  if (fromEnv) return fromEnv;
  return defaultModelId;
}

// gpt-5.3-codex-spark は reasoning effort を受け付ける一方、reasoning.summary を拒否する。
// CodexOptions.config は provider 全体へ適用されるため、model 解決後に capability guard する。
function supportsReasoningSummary(modelId: string): boolean {
  return modelId !== 'gpt-5.3-codex-spark';
}

// issue #65 B4: structured output が parse 失敗したときの修復プロンプトを構築する。
// 元の出力をそのまま渡し、JSON だけを返すよう agent に再依頼する。
// 同じ thread を継続するため再実行は thread.run / runStreamed 経由で行う (呼び出し側で 1 回 retry)。
//
// issue #65 N2: マーカーに UUID nonce を付与し、agent 出力 (originalOutput) に
// 旧固定マーカー文字列がたまたま含まれていても境界が壊れないようにする。
// nonce は引数で上書きできる (省略時は randomUUID) ため、テストから deterministic に検証できる。
export function buildStructuredOutputRepairPrompt(
  originalOutput: string,
  nonce: string = randomUUID(),
): string {
  const startMarker = `--- previous response [${nonce}] ---`;
  const endMarker = `--- end previous response [${nonce}] ---`;
  return [
    'The previous response could not be parsed as valid JSON.',
    'Please respond again with ONLY a single valid JSON value that conforms to the schema.',
    'Do not include any prose, code fences, or commentary — output the JSON value alone.',
    '',
    startMarker,
    originalOutput,
    endMarker,
  ].join('\n');
}

export async function runCodex(
  request: RunRequest,
  deps: CodexRuntimeDeps = defaultRuntimeDeps,
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
  const bridgeTelemetry = startBridgeRunTelemetry(request, 'codex');

  let result = '';
  let sessionId: string | undefined;
  let structuredOutput: unknown;
  let usage: unknown;
  // Issue #64 B5: turn.completed.usage.input_tokens を runCodex 内で累計する。
  // 1 run 内 reset (cross-run 持ち越しなし) を保つため、関数スコープに置く。
  let cumulativeInputTokens = 0;
  // issue #65 N1: user hook (B1) の重複 spawn を抑止する dedup 集合。
  // started/updated/completed の各段で evaluateItemGates が呼ばれるため、
  // 同じ item.id について子プロセス起動を 1 回だけに絞り、N倍の spawn コストを避ける。
  // B2/B3 は pure 判定なので毎回呼ぶ (副作用なし)。
  const userHookEvaluatedItemIds = new Set<string>();
  // ReasoningItem の text は lifecycle ごとの最新スナップショットとして届く。
  // 同一 text の started/updated/completed 重複だけを落とし、変化は即座に UI へ流す。
  const lastReasoningTextByItem = new Map<string, string>();
  const abort = new AbortController();
  activeAbort = abort;

  try {
    // Issue #64 D3: API key を RILLL_CODEX_API_KEY → OPENAI_API_KEY → undefined の
    // 優先順で解決し、Codex SDK へ明示注入する (undefined のときは SDK 既定の
    // env 解決に委譲する)。空文字 env も unset 扱いに倒す (resolveCodexApiKey 内)。
    const resolvedApiKey = resolveCodexApiKey({ env: deps.env });
    const systemInitApiKeyFields = buildSystemInitApiKeyFields(resolvedApiKey);
    // Issue #64 B5: modelId を解決し table から context window を引く。
    // unknown_model のときは context_usage を emit せず、代わりに 1 回だけ
    // context_usage_error を吐いて以降は静黙する (akashic 重複を避けるため)。
    const modelId = resolveModelId(request, deps.env);
    const contextWindowResolved = resolveContextWindow(modelId);
    let contextUsageErrorEmitted = false;

    // issue #65 A3: sandbox の bridge 既定値は 'danger-full-access' に倒す。
    // 理由: 'workspace-write' は Seatbelt のデフォルトポリシーで .git/ 配下への書き込みを
    // 拒否するため、git-commit のような .git 操作を含む workflow が
    // "Unable to create .git/index.lock: Operation not permitted" で失敗する。
    // approvalPolicy: 'never' で escalate もできないため、bridge は sandbox を無効化する側に倒す。
    // ただし呼び出し側が request.sandboxMode を明示指定したときは尊重する (per-call 切替)。
    const sandboxMode = request.sandboxMode ?? 'danger-full-access';
    // issue #65 A4: bridge は対話 UI を持たないため、approval の bridge 既定値は 'never'。
    // 呼び出し側が 'on-request' / 'on-failure' / 'untrusted' を明示指定したときは尊重するが、
    // bridge では UI escalation 経路がないため、これらは実質的に turn を hang させる
    // (README に記載)。
    const approvalPolicy = request.approvalPolicy ?? 'never';
    // issue #65 N3: bridge は UI escalation 経路を持たないため、'never' 以外は実質 hang する。
    // 運用 / 監査でデバッグできるよう warning observation を 1 回 emit する。
    if (approvalPolicy !== 'never') {
      writeObservation(akashicLevel, 'approval_policy_warning', {
        policy: approvalPolicy,
        reason: 'bridge has no UI escalation path; non-never policies are expected to hang the turn',
      });
    }

    const threadOptions: ThreadOptions = {
      workingDirectory: request.cwd,
      sandboxMode,
      approvalPolicy,
      skipGitRepoCheck: true,
      webSearchMode: 'live',
      // issue #66 A7: 未指定なら `['/']` を渡して「デフォルト許可」に倒す (resolveAdditionalDirectories)。
      // 明示的に空配列を渡された場合は空配列のまま (= 制限意図を尊重)。
      additionalDirectories: resolveAdditionalDirectories(request.additionalDirectories),
    };

    // issue #65 A1: per-call model を ThreadOptions.model に流す (空文字 / undefined は SDK 既定へ)。
    // 既に上で resolveModelId 経由で system_init に伝える modelId を作っているが、
    // SDK へ実際に渡すかどうかは「明示指定があったときのみ」に絞る (env / 既定値は SDK 内部で解決させる)。
    if (request.model && request.model.trim() !== '') {
      threadOptions.model = request.model;
    }
    // issue #65 A2: per-call reasoning effort を ThreadOptions.modelReasoningEffort に流す。
    // SDK 0.146 の ThreadOptions 型にない max は CodexOptions.config 経由で渡す。
    if (request.effort && request.effort !== 'max') {
      threadOptions.modelReasoningEffort = request.effort;
    }

    const codexOptions: CodexOptions = {};
    if (request.reasoningSummary && supportsReasoningSummary(modelId)) {
      codexOptions.config = { model_reasoning_summary: request.reasoningSummary };
    }
    if (request.effort === 'max') {
      codexOptions.config = { ...codexOptions.config, model_reasoning_effort: request.effort };
    }
    if (resolvedApiKey.apiKey !== undefined) {
      codexOptions.apiKey = resolvedApiKey.apiKey;
    }
    const codex = deps.createCodex(codexOptions);

    const turnOptions: TurnOptions = { signal: abort.signal };
    if (request.outputFormat?.type === 'json_schema') {
      turnOptions.outputSchema = normalizeCodexOutputSchema(request.outputFormat.schema);
    }

    // issue #65 B1/B2/B3 abort helper。
    // gate 種別ごとに observation source を切り替え (B1=codex-hook, B2/B3=codex-sdk)、
    // cancelled / cancelOrigin / abort.abort() を 1 箇所にまとめる。
    // 呼び出し後は次のループ先頭で `if (cancelled) break` により event loop を抜け、
    // finally で cancel result event 経路 (origin='engine') へ合流する。
    const triggerGateAbort = (
      gate: 'permission_deny' | 'allowed_tools_violation' | 'hook_block',
      payload: unknown,
    ): void => {
      if (gate === 'hook_block') {
        writeHookObservation(akashicLevel, 'hook_block', payload);
      } else {
        writeObservation(akashicLevel, gate, payload);
      }
      cancelled = true;
      cancelOrigin = 'engine';
      try {
        abort.abort();
      } catch {
        // SDK によっては既に abort 済みで例外を投げるが best-effort で握りつぶす。
      }
    };

    // issue #65 B1/B2/B3: ThreadItem 単位で deny gate / allowedTools 違反 / user hook を順に評価し、
    // 最初にヒットしたゲートで abort する。spawn 失敗 (B1 'error') は abort せず hook_spawn_error
    // observation を emit して allow に倒す (Claude bridge と同じ best-effort 方針)。
    // 戻り値 true は「abort された (or B1 spawn error で観測のみ)」を意味し、呼び出し側は当該 item の
    // 後続処理 (tool_use emit) を抑止できる。
    const evaluateItemGates = async (item: ThreadItem): Promise<boolean> => {
      // B2: dangerous bash / cwd 外 write
      const denyResult: DenyResult | undefined = evaluateDeny(item, request.cwd);
      if (denyResult) {
        triggerGateAbort('permission_deny', {
          source: denyResult.source,
          reason: denyResult.reason,
          item_id: item.id,
          item_type: item.type,
          details: denyResult.details,
        });
        return true;
      }
      // B3: allowedTools 違反
      const allowedResult = evaluateAllowedToolsViolation(item, request.allowedTools);
      if (allowedResult) {
        triggerGateAbort('allowed_tools_violation', {
          reason: allowedResult.reason,
          item_id: item.id,
          item_type: item.type,
          tool_names: allowedResult.toolNames,
          allowed_tools: request.allowedTools,
        });
        return true;
      }
      // B1: user 定義 PreToolUse hook。
      // issue #65 N1: 同じ item.id の started/updated/completed で重複 spawn しないよう dedup する。
      // 既に spawn 済みの item.id については block/error 判定済みなので、ここで「allow 確定」として扱う。
      if (
        request.userHooks
        && request.userHooks.length > 0
        && !userHookEvaluatedItemIds.has(item.id)
      ) {
        userHookEvaluatedItemIds.add(item.id);
        const userHookResult: UserHookEvaluation = await evaluateUserHooks(
          item,
          request.userHooks,
          request.cwd,
          deps.spawnUserHook,
        );
        if (userHookResult.decision === 'block') {
          triggerGateAbort('hook_block', {
            reason: userHookResult.reason,
            matcher: userHookResult.matcher,
            command: userHookResult.command,
            item_id: item.id,
            item_type: item.type,
          });
          return true;
        }
        if (userHookResult.decision === 'error') {
          // hook 自身の失敗で run を止めるのは過剰反応 (Claude bridge と同じ best-effort 方針)。
          // observation だけ吐いて呼び出し側は素通しさせる。
          writeHookObservation(akashicLevel, 'hook_spawn_error', {
            matcher: userHookResult.matcher,
            command: userHookResult.command,
            spawn_error: userHookResult.spawnError,
            item_id: item.id,
            item_type: item.type,
          });
        }
      }
      return false;
    };

    // issue #65 B4: structured output retry helper。
    // 1st turn の結果が parse 失敗のときに 1 回だけ呼ばれる。同じ Thread を継続使用するため、
    // 上位 (runOneTurn の呼び出しサイト) が Thread 参照を保持して repair turn を回す。
    const handleEvent = async (event: ThreadEvent): Promise<void> => {
      writeObservation(akashicLevel, event.type, event);
      switch (event.type) {
        case 'thread.started':
          sessionId = event.thread_id;
          writeEvent({
            type: 'system_init',
            id: request.id,
            systemInfo: {
              sessionId: event.thread_id,
              cwd: request.cwd,
              model: modelId,
              modelReasoningEffort: request.effort,
              permissionMode: 'never',
              ...systemInitApiKeyFields,
            },
          });
          break;
        case 'item.started':
        case 'item.updated': {
          // issue #65 B1/B2/B3: tool 系 item は emit より先に gate 評価を行う。
          // gate hit 時は abort 済みで、tool_use event は emit しない (run が止まる前提)。
          const aborted = await evaluateItemGates(event.item);
          if (aborted) break;
          if (isReasoning(event.item)) {
            const text = event.item.text ?? '';
            if (text && lastReasoningTextByItem.get(event.item.id) !== text) {
              lastReasoningTextByItem.set(event.item.id, text);
              writeEvent({ type: 'reasoning', id: request.id, itemId: event.item.id, text });
            }
            break;
          }
          const tool = toolNameForItem(event.item);
          if (tool) {
            // issue #65 D6: 各ライフサイクル段階で個別 emit (dedup なし)。
            writeEvent({
              type: 'tool_use',
              id: request.id,
              tool,
              status: toolStatusForEvent(event.type, event.item),
            });
          }
          break;
        }
        case 'item.completed': {
          if (isAgentMessage(event.item)) {
            const text = event.item.text ?? '';
            if (text) {
              writeEvent({ type: 'text', id: request.id, text });
              result = text;
            }
          } else if (isReasoning(event.item)) {
            // issue #66 A15+D7: reasoning text は agent_message と別 channel で expose する。
            // result event の `result` フィールドには混入させない (result は agent_message のみ)。
            // id は他 event と揃えて run id (request.id)、ReasoningItem.id は itemId に載せて
            // 「id semantics は event 種別を跨いで一貫 / item-level identity は別フィールド」を保つ。
            const text = event.item.text ?? '';
            if (text && lastReasoningTextByItem.get(event.item.id) !== text) {
              lastReasoningTextByItem.set(event.item.id, text);
              writeEvent({ type: 'reasoning', id: request.id, itemId: event.item.id, text });
            }
          } else {
            // 完了段でも (started/updated を取り逃がす SDK 挙動への保険として) gate 評価する。
            const aborted = await evaluateItemGates(event.item);
            if (aborted) break;
            const tool = toolNameForItem(event.item);
            if (tool) {
              // issue #65 D6: completed 段階。item.status が 'failed' のときは 'failed' に倒す。
              writeEvent({
                type: 'tool_use',
                id: request.id,
                tool,
                status: toolStatusForEvent('item.completed', event.item),
              });
            }
          }
          break;
        }
        case 'turn.completed':
          usage = event.usage;
          writeObservation(akashicLevel, 'usage', event.usage, 'turn');
          // Issue #64 B5: turn 毎の input_tokens を累計し、context_usage を emit する。
          if (typeof event.usage?.input_tokens === 'number') {
            cumulativeInputTokens += event.usage.input_tokens;
          }
          if (contextWindowResolved) {
            writeObservation(
              akashicLevel,
              'context_usage',
              computeContextUsage({
                modelId,
                contextWindow: contextWindowResolved.contextWindow,
                cumulativeInputTokens,
              }),
              'estimated',
            );
          } else if (!contextUsageErrorEmitted) {
            writeObservation(akashicLevel, 'context_usage_error', {
              model_id: modelId,
              reason: 'unknown_model',
            });
            contextUsageErrorEmitted = true;
          }
          // result event の emit は呼び出し側が retry 判定後に行う。
          break;
        case 'turn.failed':
          writeEvent({
            type: 'error',
            id: request.id,
            message: event.error?.message ?? 'turn failed',
            retryable: false,
          });
          break;
        case 'error':
          writeEvent({
            type: 'error',
            id: request.id,
            message: event.message,
            retryable: false,
          });
          break;
        default:
          break;
      }
    };

    // event 生成器を EOF まで消化する。1 run 内で複数 turn が発生する SDK 挙動も含めて、
    // ストリームが終わるまで処理する (= turn.completed では break しない)。
    // result / structuredOutput の最終 emit は呼び出し側が retry 判定後に行う。
    const drainEvents = async (events: AsyncGenerator<ThreadEvent>): Promise<void> => {
      for await (const event of events) {
        if (cancelled) break;
        await handleEvent(event);
      }
    };

    // issue #65 A1/A3/A4: Codex SDK は SDK レベルで allowedTools 経路を持たないため、
    // bridge 側で agent に self-restraint を促す注記を prompt 末尾に加える (B3)。
    // output-contract-2phase task 2.2: 同様に Codex SDK には maxTurns に対応する option が
    // 無いため、prompt 末尾に turn-budget 注記を best-effort fallback として追記する
    // (buildPromptWithMaxTurns)。両 helper は順序保持の合成 (allowedTools 注記 → maxTurns 注記)
    // で構成し、いずれも未指定なら原文を破壊せず素通しする。
    //
    // output-contract-2phase task 2.2 (documented no-op): request.permissionMode は
    // Codex SDK 側に直接対応する option が存在しない (ThreadOptions / TurnOptions / CodexOptions
    // のいずれにも permission mode field がない) ため、Codex bridge では documented no-op として
    // 受理するのみで SDK 呼び出しには一切影響させない。Phase 2 のツール制限は allowedTools
    // (= SDK 経路なし → prompt 注記 + item gate 評価) で別途実現する。protocol schema は
    // closed enum で受理しているので、wire 上で reject されることはない (= caller / 上位 layer は
    // 3 provider 共通の宣言 API として扱える)。
    const effectivePrompt = buildPromptWithMaxTurns(
      buildPromptWithAllowedTools(request.prompt, request.allowedTools),
      request.maxTurns,
    );

    // Issue #64 A16+D4: resumeThread が同期 / 非同期どちらで throw しても
    // startThread にフォールバックする。Issue #65 B4: retry のため Thread 参照を保持する。
    let thread: Thread;
    let events: AsyncGenerator<ThreadEvent>;
    if (request.resumeSessionId) {
      try {
        thread = codex.resumeThread(request.resumeSessionId, threadOptions);
        const streamed = await thread.runStreamed(effectivePrompt, turnOptions);
        events = streamed.events as AsyncGenerator<ThreadEvent>;
      } catch (resumeError) {
        bridgeTelemetry.addEvent('codex.resume_fallback');
        writeObservation(akashicLevel, 'resume_fallback', {
          requestedSessionId: request.resumeSessionId,
          fallbackReason: resumeError instanceof Error ? resumeError.message : String(resumeError),
        });
        thread = codex.startThread(threadOptions);
        const streamed = await thread.runStreamed(effectivePrompt, turnOptions);
        events = streamed.events as AsyncGenerator<ThreadEvent>;
      }
    } else {
      thread = codex.startThread(threadOptions);
      const streamed = await thread.runStreamed(effectivePrompt, turnOptions);
      events = streamed.events as AsyncGenerator<ThreadEvent>;
    }

    await drainEvents(events);

    // issue #65 B4: structured output retry。
    // 1st turn の result が parse 失敗のときに 1 回だけ修復ターンを投げる。retry でも parse 失敗なら
    // structuredOutput=undefined のまま result event を emit する (上位で text として扱える)。
    if (
      !cancelled
      && request.outputFormat?.type === 'json_schema'
      && result
    ) {
      const firstParse = parseStructuredOutput(result);
      if (firstParse === undefined) {
        writeObservation(akashicLevel, 'structured_output_retry', {
          reason: 'parse_failed',
          attempt: 1,
          original_output_preview: result.slice(0, 200),
        });
        const repairPrompt = buildStructuredOutputRepairPrompt(result);
        const previousResult = result;
        result = '';
        try {
          const streamed2 = await thread.runStreamed(repairPrompt, turnOptions);
          await drainEvents(streamed2.events as AsyncGenerator<ThreadEvent>);
          structuredOutput = parseStructuredOutput(result);
        } catch (retryError) {
          // 修復 turn 自体が失敗した場合は元の出力を保持し、structuredOutput=undefined のままにする。
          writeObservation(akashicLevel, 'structured_output_retry', {
            reason: 'retry_failed',
            attempt: 2,
            error_message: retryError instanceof Error ? retryError.message : String(retryError),
          });
          result = previousResult;
        }
      } else {
        structuredOutput = firstParse;
      }
    }

    // 通常終了 (cancel されていない) のときのみ result event を emit する。
    // cancel された場合は finally の cancel result event 経路に任せる (Req 3.2 / 同 origin)。
    if (!cancelled) {
      writeEvent({
        type: 'result',
        id: request.id,
        result,
        sessionId,
        structuredOutput,
      });
    }

    writeEvent({ type: 'done', id: request.id, result, sessionId });
    void usage;
  } catch (error) {
    bridgeTelemetry.recordError(error);
    writeProtocolError(request.id, error);
  } finally {
    // memory-distiller-trigger-policy task 2.5: cancel が走った場合のみ result event payload に
    // origin を含めて運搬する。cancel 不在の正常終了は origin 省略 (Req 6 後方互換)。
    // claude bridge と同形 (両 provider 同一契約 / Req 3.2)。
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
    activeAbort = undefined;
    cancelled = false;
    cancelOrigin = undefined;
    await bridgeTelemetry.end();
    eventWriter = previousEventWriter;
  }
}

function parseStructuredOutput(text: string): unknown {
  const trimmed = text.trim();
  try {
    return JSON.parse(trimmed);
  } catch {
    return undefined;
  }
}

// memory-distiller-trigger-policy task 2.5: cancel handler は req.origin を
// モジュールスコープの cancelOrigin に保持し、finally ブロックで result event payload に透過する。
// cancelRequestSchema.origin は zod の `.default('engine')` で常に解決済みのため、
// req.origin はランタイムでは必ず 'user' | 'engine' になる (claude bridge と同形 / Req 3.2)。
async function cancel(req: { id: string; origin: 'user' | 'engine' }): Promise<void> {
  if (activeID !== req.id) return;
  cancelled = true;
  cancelOrigin = req.origin;
  activeAbort?.abort();
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
      // memory-distiller-trigger-policy task 2.5: cancel handler に origin を渡すため、
      // 解決済みの request.data ({ id, origin }) をそのまま受け渡す (claude bridge と同形)。
      void cancel(request.data);
      return;
    }

    void runCodex(request.data);
  });
}

function isMainModule(): boolean {
  return process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href;
}

if (isMainModule()) {
  startStdioBridge();
}
