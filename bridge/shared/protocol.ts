import { z } from 'zod';

const outputFormatSchema = z.union([
  z.object({ type: z.literal('text') }),
  z.object({ type: z.literal('json_schema'), schema: z.unknown() }),
]);

const metadataSchema = z.object({
  workflowRunId: z.string().optional(),
  workflowId: z.string().optional(),
  workflowName: z.string().optional(),
  stepId: z.string().optional(),
  stepName: z.string().optional(),
  agentCallId: z.string().optional(),
}).optional();

const traceContextSchema = z.object({
  traceparent: z.string().min(1).optional(),
  tracestate: z.string().optional(),
  baggage: z.string().optional(),
}).optional();

const akashicLevelSchema = z.enum(['summary', 'full', 'debug']);

// issue #47: PreToolUse hook で block 判定を行う user 定義 hook の宣言。
// matcher は SDK の hook config と同じく対象 tool 名 (e.g. "Edit") の正規表現。
// command は injection 対策で **配列形式に固定** (例: ["bash", "-lc", "/path/to/verify-gate.sh"])。
// 空配列の command や空文字 matcher は zod の min(1) で reject する。
const userHookSchema = z.object({
  matcher: z.string().min(1),
  command: z.array(z.string().min(1)).min(1),
});

// issue #65 A2: per-call modelReasoningEffort 切替の closed enum。
// Codex の max は SDK config 経由で渡す。Claude bridge では 'minimal' を 'low'
// にマッピングして SDK Options.effort に流す。Cursor bridge では reasoning param value
// として preferredThinking 経路に流す。
const effortSchema = z.enum(['minimal', 'low', 'medium', 'high', 'xhigh', 'max']);

// Cursor SDK のモデル選択時に渡す明示的な parameter。
const modelParamSchema = z.object({
  id: z.string().min(1),
  value: z.string().min(1),
});

// Codex が公開する reasoning summary の生成粒度。raw chain-of-thought は対象外。
const reasoningSummarySchema = z.enum(['auto', 'concise', 'detailed', 'none']);

// issue #65 A3: per-call sandboxMode 切替の closed enum。
// Codex SDK の SandboxMode と同じ語彙。bridge 既定値は danger-full-access
// (workspace-write は .git/ 配下への書き込みを拒否し git-commit 系 workflow が止まるため)。
const sandboxModeSchema = z.enum(['read-only', 'workspace-write', 'danger-full-access']);

// issue #65 A4: per-call approvalPolicy 切替の closed enum。
// Codex SDK の ApprovalMode と同じ語彙。bridge 既定値は 'never' (bridge は対話 UI を持たないため)。
const approvalPolicySchema = z.enum(['never', 'on-request', 'on-failure', 'untrusted']);

export const runRequestSchema = z.object({
  type: z.literal('run'),
  id: z.string().min(1),
  prompt: z.string(),
  cwd: z.string().min(1),
  resumeSessionId: z.string().optional(),
  outputFormat: outputFormatSchema.optional(),
  akashicLevel: akashicLevelSchema.optional(),
  traceContext: traceContextSchema,
  metadata: metadataSchema,
  // review-workflow Task 1.3: per-rule provider/model 上書き seam。zero-value (undefined) のとき
  // bridge 側は provider 既定モデルを維持する。
  model: z.string().optional(),
  // issue #65 A2: per-call reasoning effort。3 provider 共通の closed enum で運び、
  // 各 bridge 側で SDK 固有の語彙にマッピングする (Claude: minimal→low, Codex: そのまま,
  // Cursor: reasoning param value)。
  effort: effortSchema.optional(),
  // Cursor SDK の model ID と parameter/variant の組み合わせを明示する seam。
  modelParams: z.array(modelParamSchema).min(1).optional(),
  // CodexOptions.config.model_reasoning_summary に流す Codex 専用 seam。
  reasoningSummary: reasoningSummarySchema.optional(),
  // issue #65 A3: per-call sandbox 制御 (Codex bridge 専用 seam)。
  // undefined のとき bridge 既定値 'danger-full-access' を維持する。
  sandboxMode: sandboxModeSchema.optional(),
  // issue #65 A4: per-call approval policy (Codex bridge 専用 seam)。
  // undefined のとき bridge 既定値 'never' を維持する。
  approvalPolicy: approvalPolicySchema.optional(),
  // issue #66 A7: per-call additionalDirectories (cwd 外 read 許可)。
  // codex: ThreadOptions.additionalDirectories に流す。
  // claude: SDK Options.additionalDirectories に流す。
  // undefined のとき bridge 既定値 `['/']` を適用し「デフォルト許可」(= cwd 外 read を広めに許可) する。
  // 空配列 `[]` を明示指定したときは制限意図とみなしてそのまま渡す。
  additionalDirectories: z.array(z.string()).optional(),
  // kiro-spec-loop Phase C: judge ターン等で Edit/Write を禁じるための allowedTools 制限を
  // SDK Options.allowedTools に流すための seam。undefined / 空配列のときは SDK 既定 (= 全ツール許可) を維持する。
  allowedTools: z.array(z.string()).optional(),
  // issue #47: PreToolUse hook chain に追加する user 定義 hook 一覧。
  // 各 entry の matcher にマッチした tool 呼び出しの直前に command を child_process.spawn で起動し、
  // stdout JSON `{"decision":"block","reason":"..."}` を SDK の `permissionDecision: "deny"` に翻訳する。
  // undefined / 空配列のときは hook chain を変更しない (= 既存 observationHook のみ)。
  userHooks: z.array(userHookSchema).optional(),
  // output-contract-2phase task 1.2: per-call permissionMode 上書き seam。
  // Claude Agent SDK の Options.permissionMode と語彙を合わせた closed enum。
  // undefined のとき各 bridge 既定値を維持する (Claude bridge は 'acceptEdits' 固定の既定を継続)。
  // Codex bridge / Cursor bridge では documented no-op (SDK レベルの対応 option がないため)。
  // Go 側 `internal/bridge/protocol.go` の Request.PermissionMode と wire-level で 1:1 対応する。
  permissionMode: z.enum(['default', 'acceptEdits', 'plan', 'bypassPermissions']).optional(),
  // output-contract-2phase task 1.2: per-call maxTurns 上書き seam。
  // Claude bridge は SDK Options.maxTurns に配線、Codex bridge は SDK turn-budget 経路に配線、
  // Cursor bridge は documented no-op。undefined のとき SDK 既定 turn budget を維持する。
  // Go 側 `internal/bridge/protocol.go` の Request.MaxTurns と wire-level で 1:1 対応する。
  // 0 / 負数 / 小数は zod schema で reject される (positive int 限定)。
  maxTurns: z.number().int().positive().optional(),
}).strict();

export const cancelRequestSchema = z.object({
  type: z.literal('cancel'),
  id: z.string().min(1),
  // memory-distiller-trigger-policy task 2.1: cancel イベントの起源 discriminator。
  // UI 層 (user 起源) と internal cancel 経路 (engine 起源) を bridge で区別するための seam。
  // 未指定時は default 'engine' に解決され、既存 cancel 送出側 (Req 6 後方互換) を壊さない。
  origin: z.enum(['user', 'engine']).optional().default('engine'),
});

export const requestSchema = z.discriminatedUnion('type', [
  runRequestSchema,
  cancelRequestSchema,
]);

export const systemInfoSchema = z.object({
  uuid: z.string().optional(),
  sessionId: z.string().optional(),
  apiKeySource: z.string().optional(),
  cwd: z.string().optional(),
  tools: z.array(z.string()).optional(),
  mcpServers: z.array(z.object({
    name: z.string().optional(),
    status: z.string().optional(),
  })).optional(),
  model: z.string().optional(),
  permissionMode: z.string().optional(),
  slashCommands: z.array(z.string()).optional(),
  outputStyle: z.string().optional(),
  modelReasoningEffort: z.string().optional(),
});

export const eventSchema = z.discriminatedUnion('type', [
  z.object({ type: z.literal('ready') }),
  z.object({ type: z.literal('system_init'), id: z.string(), systemInfo: systemInfoSchema }),
  z.object({ type: z.literal('text'), id: z.string(), text: z.string() }),
  // issue #66 A15+D7: reasoning item の専用 event。
  // codex: ReasoningItem (type='reasoning') を agent_message / tool_use と分離して expose する。
  // claude: stream_event の thinking_delta 経路で同 event を emit する (provider 共通化、後続実装で対応)。
  // 既存 result event の `result` フィールドには混入させない (= reasoning text は別 channel)。
  //
  // id: 他の event と同じく run id (request.id) を載せる (run 横断の correlation 用、全 event 共通の契約)。
  // itemId: ReasoningItem 等の item-level identity (省略可)。UI / Go 側で reasoning を item 単位に集約する用途。
  //         Claude bridge の thinking_delta のように item id を持たない provider では省略する。
  z.object({
    type: z.literal('reasoning'),
    id: z.string(),
    itemId: z.string().optional(),
    text: z.string(),
  }),
  // issue #65 D6: tool item の status 遷移を細粒度に伝えるため status field を追加 (省略可で後方互換)。
  // Codex bridge は item.started / item.updated / item.completed / item.failed の各段階で個別 emit。
  // Claude / Cursor bridge は当面 status 未設定で動作する (omitempty で wire 上に出ない)。
  z.object({
    type: z.literal('tool_use'),
    id: z.string(),
    tool: z.string(),
    status: z.enum(['started', 'updated', 'completed', 'failed']).optional(),
  }),
  // bridge-result-error-propagation task 2.2 (Req 2.2 / 2.3 / 3.1-3.3): `structuredOutput` が
  // undefined の result event は「structured output parse 失敗 または 構造化出力なし」を意味する
  // 3 provider 共通の契約として運用する。caller (Go provider 層 / workflow 層) はこの semantics
  // のみで provider 切替時のロジックを揃えられる (Req 2.3)。
  // - codex-sdk bridge: 自前 repair turn 後にも parse 失敗が確定したとき `result=text` /
  //   `structuredOutput=undefined` で emit する (bridge/codex-sdk/src/index.ts:659-690)。
  // - cursor-sdk bridge: 多段 parse 失敗時に `structuredOutput=undefined` で emit する
  //   (bridge/cursor-sdk/src/structured.ts)。
  // - claude-agent-sdk bridge: Claude SDK の `subtype === 'error_max_structured_output_retries'`
  //   を `result=''` / `structuredOutput=undefined` にマッピングして codex / cursor と対称化する
  //   (bridge/claude-agent-sdk/src/index.ts の subtype dispatcher; spec task 2.1)。
  // 関連 issue: #70 (bridge-result-error-propagation spec)。
  z.object({
    type: z.literal('result'),
    id: z.string(),
    result: z.string().optional(),
    sessionId: z.string().optional(),
    structuredOutput: z.unknown().optional(),
    // memory-distiller-trigger-policy task 2.1 (R-1 closing): cancel 起源を bridge → Go 側へ
    // 運搬するため result event payload に origin を透過する。cancel 不在時は省略 (= omitempty
    // で wire 上に出ない) ため default を持たない。
    origin: z.enum(['user', 'engine']).optional(),
  }),
  // bridge-result-error-propagation task 2.2 (Req 2.2 / 2.3 / 3.1-3.3): `code` は自由文字列だが、
  // claude-agent-sdk bridge では Claude SDK の `SDKResultError.subtype` 文字列
  // (`error_during_execution` / `error_max_turns` / `error_max_budget_usd` / `unknown`) を
  // 運搬する運用規約がある。issue #47 の `hook_block` / `hook_spawn_error` 等と同じ自由規約
  // namespace で運用し、3 provider 横断で caller は `code` 値の比較によって意味的に分類する。
  // - `error_max_structured_output_retries` は `error` event ではなく `result` event の
  //   `structuredOutput=undefined` で表現する (codex / cursor の parse 失敗契約と対称化、上記
  //   resultEventSchema コメント参照)。
  // - `retryable=false` は SDK 内部の limit 到達などにより既に terminal な subtype に対して
  //   必ず付与する (これらは bridge / caller 側で retry 不可)。
  // - 実装上の参照ポイント: bridge/claude-agent-sdk/src/index.ts の subtype dispatcher
  //   (spec task 2.1 で導入)。
  // 関連 issue: #70 (bridge-result-error-propagation spec)。
  z.object({
    type: z.literal('error'),
    id: z.string().optional(),
    message: z.string(),
    code: z.string().optional(),
    retryable: z.boolean().optional(),
  }),
  z.object({
    type: z.literal('done'),
    id: z.string(),
    result: z.string().optional(),
    sessionId: z.string().optional(),
  }),
  z.object({
    type: z.literal('observation'),
    id: z.string(),
    source: z.string(),
    observationType: z.string(),
    observationSubtype: z.string().optional(),
    sequence: z.number().int().nonnegative(),
    payload: z.unknown(),
    payloadTruncated: z.boolean().optional(),
  }),
]);

export type BridgeRequest = z.infer<typeof requestSchema>;
export type RunRequest = z.infer<typeof runRequestSchema>;
export type BridgeEvent = z.infer<typeof eventSchema>;

export function serializeEvent(event: BridgeEvent): string {
  return `${JSON.stringify(event)}\n`;
}
