package provider

import (
	"context"
	"encoding/json"
)

type ProviderID string

const (
	ProviderClaude    ProviderID = "claude"
	ProviderCursor    ProviderID = "cursor"
	ProviderCodex     ProviderID = "codex"
	ProviderCursorSDK ProviderID = "cursor-sdk"
)

type OutputFormat struct {
	Type   string `json:"type"`
	Schema any    `json:"schema,omitempty"`
}

// ModelParam は SDK のモデル選択時に指定するパラメータ。
type ModelParam struct {
	ID    string
	Value string
}

// ModelSettings は provider 呼び出し時の model / reasoning effort / model params。
// 空文字列の field、nil / 空の ModelParams は provider / SDK 既定値を維持する。
type ModelSettings struct {
	Model            string
	Effort           string
	ReasoningSummary string
	ModelParams      []ModelParam
}

type AgentRequest struct {
	Prompt          string        `json:"prompt"`
	CWD             string        `json:"cwd"`
	ResumeSessionID string        `json:"resumeSessionId,omitempty"`
	OutputFormat    *OutputFormat `json:"outputFormat,omitempty"`
	AkashicLevel    string        `json:"akashicLevel,omitempty"`
	TraceContext    TraceContext  `json:"traceContext,omitzero"`
	Metadata        AgentMetadata `json:"metadata,omitempty"`
	// Model は呼び出し時に上書き指定するモデル名。zero-value (空文字列) のときは provider 既定
	// モデルを維持する。各 provider 実装は zero-value のときは bridge / CLI に model 指定を渡さず、
	// 非 zero-value のときのみ素通しする (review-workflow design.md §agent / provider 拡張)。
	Model string `json:"model,omitempty"`
	// Effort は per-call reasoning effort (issue #65 A2)。
	// 'minimal'|'low'|'medium'|'high'|'xhigh'|'max' のいずれか。空文字のとき provider / SDK 既定値を維持する。
	//
	// 各 provider のマッピング:
	//   - codex: ThreadOptions.modelReasoningEffort にそのまま流す
	//   - claude: SDK Options.effort に流す ('minimal' は 'low' にマッピング)
	//   - cursor: reasoning param value (preferredThinking) として注入
	//   - cursor (CLI): 未対応 (no-op)
	Effort string `json:"effort,omitempty"`
	// ModelParams は SDK のモデル選択時に渡す明示的なパラメータ。
	// Cursor SDK のモデル ID と parameter/variant の組み合わせを表す。
	// nil / 空のときは provider / SDK 既定値を維持する。
	ModelParams []ModelParam `json:"modelParams,omitempty"`
	// ReasoningSummary は Codex が公開する reasoning summary の生成粒度。
	// 'auto'|'concise'|'detailed'|'none' のいずれかで、空文字なら SDK 既定値を維持する。
	// Codex provider のみが CodexOptions.config.model_reasoning_summary に伝搬する。
	ReasoningSummary string `json:"reasoningSummary,omitempty"`
	// SandboxMode は per-call sandbox 制御 (issue #65 A3)。
	// 'read-only'|'workspace-write'|'danger-full-access' のいずれか。
	// 空文字のとき provider / SDK 既定値を維持する。
	//
	// 現在は codex provider のみが ThreadOptions.sandboxMode に伝搬する。他 provider は no-op 素通し。
	SandboxMode string `json:"sandboxMode,omitempty"`
	// ApprovalPolicy は per-call approval policy (issue #65 A4)。
	// 'never'|'on-request'|'on-failure'|'untrusted' のいずれか。
	// 空文字のとき provider / SDK 既定値を維持する。
	//
	// 現在は codex provider のみが ThreadOptions.approvalPolicy に伝搬する。他 provider は no-op 素通し。
	ApprovalPolicy string `json:"approvalPolicy,omitempty"`
	// AdditionalDirectories は cwd 外 read を許可する追加ディレクトリ一覧 (issue #66 A7)。
	// nil のとき bridge 側は既定値 `['/']` を適用し「デフォルト許可」(= cwd 外 read を広めに許可) する。
	// 空配列 `[]` のときは制限意図とみなして bridge に空配列をそのまま渡す。
	//
	// 各 provider のマッピング:
	//   - codex: ThreadOptions.additionalDirectories にそのまま流す
	//   - claude: SDK Options.additionalDirectories に流す
	//   - cursor: 未対応 (no-op 素通し)
	//
	// json tag は bridge.Request.AdditionalDirectories と同じ理由で `omitzero` を採用する
	// (空配列の restrict 意図を JSON marshal で消さないため)。
	AdditionalDirectories []string `json:"additionalDirectories,omitzero"`
	// AllowedTools は SDK / CLI に渡す「許可ツール」のホワイトリスト。
	// nil または空のときは provider / SDK 既定 (= 全ツール許可) を維持する後方互換ガード。
	// 非空のときは指定ツールのみ実行可能とすることで、judge ターン等で Edit/Write を禁じる。
	//
	// claude provider は SDK Options.allowedTools に伝搬する。
	// codex provider (issue #65 B3) は prompt suffix で agent に通知 + item.started 段で違反検知 → abort。
	// cursor provider は no-op 素通し。
	AllowedTools []string `json:"allowedTools,omitempty"`
	// UserHooks は PreToolUse hook chain に追加する user 定義 hook 一覧 (issue #47, #65 B1)。
	// 各 entry の Matcher にマッチした tool 呼び出しの直前に Command を child_process.spawn で起動し、
	// stdout JSON `{"decision":"block","reason":"..."}` を SDK の deny / abort に翻訳する。
	// nil / 空のときは hook chain を変更せず既存 observation 経路のみが走る。
	//
	// claude provider は SDK の PreToolUse hook chain にネイティブ統合する。
	// codex provider (issue #65 B1) は item.started / item.updated 観測経路で擬似実装し、
	// block 判定時に AbortController.abort() で run を切る (post-execution kill switch)。
	// cursor provider は no-op 素通し。
	UserHooks []UserHook `json:"userHooks,omitempty"`
	// PermissionMode は per-call permissionMode 上書き seam (output-contract-2phase task 1.3)。
	// 値域は closed enum `"" | "default" | "acceptEdits" | "plan" | "bypassPermissions"` で
	// bridge.Request.PermissionMode と完全同期する (Claude Agent SDK の `permissionMode` 語彙)。
	// 空文字 (zero-value) のとき各 provider 実装は bridge.Request にもそのまま空文字を渡し、
	// bridge 側で SDK 既定 (Claude では `'acceptEdits'`) を維持する後方互換ガードを保つ。
	//
	// claude provider は bridge.Request.PermissionMode に素通し → bridge → SDK Options.permissionMode に流す。
	// codex provider は bridge.Request.PermissionMode に素通しするが、Codex bridge では documented no-op
	// (Codex SDK に直接対応する option がないため)。
	// cursor provider は bridge.Request.PermissionMode に素通しするが、Cursor bridge では documented no-op。
	PermissionMode string `json:"permissionMode,omitempty"`
	// MaxTurns は per-call turn budget 上書き seam (output-contract-2phase task 1.3)。
	// 0 (zero-value) のとき各 provider 実装は bridge.Request にも 0 を渡し、bridge 側 omitempty で
	// SDK 既定 turn budget を維持する後方互換ガードを保つ。
	//
	// claude provider は bridge.Request.MaxTurns に素通し → bridge → SDK Options.maxTurns に流す。
	// codex provider は bridge.Request.MaxTurns に素通し → bridge が SDK turn-budget 経路 (or
	// prompt suffix fallback) に配線する (実装段で確認)。
	// cursor provider は bridge.Request.MaxTurns に素通しするが、Cursor bridge では documented no-op。
	MaxTurns int `json:"maxTurns,omitempty"`
}

// TraceContext carries W3C trace propagation fields across the Go -> Node
// NDJSON boundary. Raw session IDs and prompt content are intentionally not
// part of this contract.
type TraceContext struct {
	TraceParent string `json:"traceparent,omitempty"`
	TraceState  string `json:"tracestate,omitempty"`
	Baggage     string `json:"baggage,omitempty"`
}

func (c TraceContext) IsZero() bool {
	return c.TraceParent == "" && c.TraceState == "" && c.Baggage == ""
}

// UserHook は PreToolUse hook chain に挿入する user 定義 hook の宣言 (issue #47)。
// Matcher は SDK hook config と同じく対象 tool 名 (e.g. "Edit") の正規表現。
// Command は injection 対策で配列形式に固定 (例: ["bash","-lc","/path/to/verify-gate.sh"])。
type UserHook struct {
	Matcher string   `json:"matcher"`
	Command []string `json:"command"`
}

type AgentMetadata struct {
	WorkflowRunID string `json:"workflowRunId,omitempty"`
	WorkflowID    string `json:"workflowId,omitempty"`
	WorkflowName  string `json:"workflowName,omitempty"`
	StepID        string `json:"stepId,omitempty"`
	StepName      string `json:"stepName,omitempty"`
	AgentCallID   string `json:"agentCallId,omitempty"`
}

type AgentEventType string

const (
	EventText    AgentEventType = "text"
	EventToolUse AgentEventType = "tool_use"
	// EventReasoning は思考プロセス (Codex の ReasoningItem / Claude の thinking_delta) を
	// agent_message / tool_use とは別 channel で上位に伝えるイベント型 (issue #66 A15+D7)。
	// Text に reasoning text を載せ、result event の `result` フィールドには混入させない。
	EventReasoning   AgentEventType = "reasoning"
	EventDone        AgentEventType = "done"
	EventObservation AgentEventType = "observation"
	// EventError は bridge が emit する error event のうち、run を中断せず観測のみ行うべきものを
	// 上位 workflow に伝えるイベント型 (issue #47)。現状は code='hook_block' (PreToolUse hook が
	// block を返した場合) を想定。Retryable=false で「同じ query を再投げても無駄」を上位に通知する。
	EventError AgentEventType = "error"
)

type AgentEvent struct {
	Type AgentEventType `json:"type"`
	// ItemID は item-level identity (issue #66 A15+D7)。
	// reasoning event で Codex ReasoningItem.id を運搬し、UI / 上位で reasoning を item 単位に集約するための seam。
	// bridge.Event.ItemID と wire-level で 1:1 対応する。空文字のとき item-level identity なし。
	ItemID string `json:"itemId,omitempty"`
	Text   string `json:"text,omitempty"`
	Tool   string `json:"tool,omitempty"`
	// ToolStatus は tool_use event の status (issue #65 D6)。
	// 'started'|'updated'|'completed'|'failed' のいずれか。
	// Codex bridge は item の各ライフサイクル段階で個別 emit する。
	// 他 provider は当面空文字 (= status 不明 / 既存ストリームと後方互換)。
	ToolStatus         string          `json:"status,omitempty"`
	Content            string          `json:"content,omitempty"`
	SessionID          string          `json:"sessionId,omitempty"`
	Source             string          `json:"source,omitempty"`
	ObservationType    string          `json:"observationType,omitempty"`
	ObservationSubtype string          `json:"observationSubtype,omitempty"`
	Sequence           int64           `json:"sequence,omitempty"`
	Payload            json.RawMessage `json:"payload,omitempty"`
	PayloadTruncated   bool            `json:"payloadTruncated,omitempty"`
	// Message / Code / Retryable は EventError 専用フィールド (issue #47)。
	// 現状は hook_block (PreToolUse user hook 由来の block) を運搬するために使用する。
	Message   string `json:"message,omitempty"`
	Code      string `json:"code,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`
}

type AgentEventCallback func(AgentEvent)

type AgentMCPServerInfo struct {
	Name   string `json:"name,omitempty"`
	Status string `json:"status,omitempty"`
}

type AgentSystemInfo struct {
	ProviderID           ProviderID           `json:"providerId,omitempty"`
	UUID                 string               `json:"uuid,omitempty"`
	SessionID            string               `json:"sessionId,omitempty"`
	APIKeySource         string               `json:"apiKeySource,omitempty"`
	CWD                  string               `json:"cwd,omitempty"`
	Tools                []string             `json:"tools,omitempty"`
	MCPServers           []AgentMCPServerInfo `json:"mcpServers,omitempty"`
	Model                string               `json:"model,omitempty"`
	ModelReasoningEffort string               `json:"modelReasoningEffort,omitempty"`
	PermissionMode       string               `json:"permissionMode,omitempty"`
	SlashCommands        []string             `json:"slashCommands,omitempty"`
	OutputStyle          string               `json:"outputStyle,omitempty"`
}

type AgentResponse struct {
	Content          string           `json:"content"`
	SessionID        string           `json:"sessionId,omitempty"`
	Success          bool             `json:"success"`
	StructuredOutput any              `json:"structuredOutput,omitempty"`
	SystemInfo       *AgentSystemInfo `json:"systemInfo,omitempty"`
	Metrics          AgentMetrics     `json:"metrics,omitempty"`
	// OutputContractResults は Phase 2 (OutputContract 経路) の結果一覧 (output-contract-2phase task 1.3)。
	// zero-value (nil) は「Phase 2 未実行 / OutputContracts 未指定」を示し、本機能追加前と完全に同一の
	// 後方互換挙動を保証する。AgentRequest 側に OutputContracts が無いとき agent 層は本 field を nil
	// のまま return する。
	//
	// 非 nil のとき、agent 層 (runReportPhase) の postcondition により
	// len(OutputContractResults) == len(CallAgentOptions.OutputContracts) かつ
	// OutputContractResults[i].Name == OutputContracts[i].Name (順序対応) が保証される。
	//
	// 既存 field (Content / SessionID / Success / StructuredOutput / SystemInfo / Metrics) の意味は
	// 本 field 追加によって一切変更されない。Phase 1 応答テキストは引き続き Content に格納され、
	// Phase 2 で書き出された report path 群は本 slice の各要素の ReportPath に格納される (R2.5)。
	OutputContractResults []OutputContractResult `json:"outputContractResults,omitempty"`
}

// OutputContractResult は OutputContract 1 件分の Phase 2 実行結果 (output-contract-2phase task 1.3)。
// agent 層の runReportPhase が宣言順に値構築し、AgentResponse.OutputContractResults に集約する。
//
// Status 値域: "success" | "failed" | "skipped"
//   - "success": Phase 2 が成功し report を書き出した。ReportPath が絶対パスで必須、Warning は空。
//   - "failed":  precheck / Phase 2 / retry のいずれかで失敗。Warning に失敗理由を必須で詰める。
//     ReportPath は空。一部の contract のみ failed のとき他 contract は通常通り success を返す (R5.3)。
//   - "skipped": 将来拡張用 (現状未使用)。
//
// Mode 値域: "native" | "fallback" | ""
//   - "native":   provider が AllowedTools / permissionMode を SDK レベルで強制 (claude / codex)。
//   - "fallback": prompt-only best-effort で同等の制限を実現 (cursor)。
//   - "":         precheck で fail し Phase 2 を実行しなかった場合 (Status="failed" と併発し得る)。
//
// 各 field の相互依存:
//   - Status="success" のとき ReportPath は必ず非空 (絶対パス)、Warning は空。
//   - Status="failed"  のとき Warning は必ず非空、ReportPath は空。
//   - RetryUsed=true のとき、Phase 2 main session が失敗し新規 session で 1 回 retry した事実を示す
//     (R5.1 / R7.3)。Status は "success" / "failed" の双方を取り得る (retry で成功 / retry でも失敗)。
type OutputContractResult struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Mode       string `json:"mode,omitempty"`
	ReportPath string `json:"reportPath,omitempty"`
	Warning    string `json:"warning,omitempty"`
	RetryUsed  bool   `json:"retryUsed,omitempty"`
}

type AgentMetrics struct {
	TotalCostUSD  *float64 `json:"totalCostUsd,omitempty"`
	InputTokens   *int64   `json:"inputTokens,omitempty"`
	OutputTokens  *int64   `json:"outputTokens,omitempty"`
	ResultPayload any      `json:"resultPayload,omitempty"`
}

type AgentProvider interface {
	ID() ProviderID
	Call(ctx context.Context, request AgentRequest, onEvent AgentEventCallback) (AgentResponse, error)
}
