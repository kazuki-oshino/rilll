package bridge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

type RequestType string

const (
	RequestRun    RequestType = "run"
	RequestCancel RequestType = "cancel"
)

// CancelOrigin 系の closed enum 値。
// bridge/shared/protocol.ts の cancelRequestSchema.origin (`z.enum(['user', 'engine'])`)
// および result event の `origin` field と wire-level で 1:1 対応する。
// bridge `tech.md` 不変式により片側変更は禁止で、両端 + bridge 実装 + テストを同期して更新する。
const (
	CancelOriginUser   = "user"
	CancelOriginEngine = "engine"
)

// ErrorCode 系の closed enum 値。
// bridge/shared/protocol.ts の errorEventSchema.code に運用規約として定義されている
// Claude SDK terminal error subtype 文字列を Go 側 caller が type-safe に参照するための
// const 定義 (issue #70 / bridge-result-error-propagation spec)。
// bridge `tech.md` 不変式により片側変更は禁止で、TS / Go / bridge 実装 / テストを同期して更新する。
//
// ErrorCodeStructuredOutputRetries は wire 上は `error` event ではなく `result` event の
// `StructuredOutput=nil` で表現される (codex / cursor の parse 失敗契約と対称化のため)。
// この const は Claude SDK の `SDKResultError.subtype` 文字列 そのものを保持するため、
// akashic observation 等で raw subtype 文字列を識別したい caller が比較に使う用途を兼ねる。
const (
	ErrorCodeStructuredOutputRetries = "error_max_structured_output_retries"
	ErrorCodeDuringExecution         = "error_during_execution"
	ErrorCodeMaxTurns                = "error_max_turns"
	ErrorCodeMaxBudgetUSD            = "error_max_budget_usd"
	ErrorCodeUnknown                 = "unknown"
)

// CancelRequest は cancel リクエストの NDJSON ペイロードを Go 側で構造的に扱うためのミラー型。
// bridge/shared/protocol.ts の cancelRequestSchema (`{type: 'cancel', id, origin?}`) と対称で、
// `Origin` は空文字 / "user" / "engine" の 3 値を取り、空文字は default 'engine' と同義 (Req 6 後方互換)。
// 既存の汎用 Request 構造体が NDJSON プロトコルの run/cancel 両方を担っているが、
// design.md (Bridge `cancelRequestSchema.origin`) では cancel-only の型レベル契約を別途明示するため
// このミラー型を導入する。
type CancelRequest struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Origin string `json:"origin,omitempty"`
}

type EventType string

const (
	EventReady       EventType = "ready"
	EventSystemInit  EventType = "system_init"
	EventText        EventType = "text"
	EventToolUse     EventType = "tool_use"
	EventReasoning   EventType = "reasoning"
	EventResult      EventType = "result"
	EventError       EventType = "error"
	EventDone        EventType = "done"
	EventObservation EventType = "observation"
)

type OutputFormat struct {
	Type   string `json:"type"`
	Schema any    `json:"schema,omitempty"`
}

type ModelParam struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

type Request struct {
	Type            RequestType   `json:"type"`
	ID              string        `json:"id"`
	Prompt          string        `json:"prompt,omitempty"`
	CWD             string        `json:"cwd,omitempty"`
	ResumeSessionID string        `json:"resumeSessionId,omitempty"`
	OutputFormat    *OutputFormat `json:"outputFormat,omitempty"`
	AkashicLevel    string        `json:"akashicLevel,omitempty"`
	TraceContext    TraceContext  `json:"traceContext,omitzero"`
	Metadata        Metadata      `json:"metadata,omitempty"`
	// Model は呼び出し時に上書き指定するモデル名 (review-workflow design.md §agent / provider 拡張)。
	// zero-value のときは bridge 側で provider 既定モデルを維持する。
	Model string `json:"model,omitempty"`
	// Effort は per-call reasoning effort (issue #65 A2)。
	// 'minimal'|'low'|'medium'|'high'|'xhigh'|'max' のいずれか。空文字のとき bridge 側は SDK 既定値を維持する。
	// bridge/shared/protocol.ts の effortSchema と wire-level で 1:1 対応する。
	Effort string `json:"effort,omitempty"`
	// ModelParams は Cursor SDK のモデル選択時に渡す明示的なパラメータ。
	// nil / 空のときは bridge 側で SDK 既定の params を維持する。
	ModelParams []ModelParam `json:"modelParams,omitempty"`
	// ReasoningSummary は Codex reasoning summary の生成粒度。
	// bridge/shared/protocol.ts の reasoningSummarySchema と wire-level で 1:1 対応する。
	ReasoningSummary string `json:"reasoningSummary,omitempty"`
	// SandboxMode は per-call sandbox 制御 (issue #65 A3, Codex bridge 専用)。
	// 'read-only'|'workspace-write'|'danger-full-access' のいずれか。
	// 空文字のとき bridge 側は既定値 'danger-full-access' を維持する。
	SandboxMode string `json:"sandboxMode,omitempty"`
	// ApprovalPolicy は per-call approval policy (issue #65 A4, Codex bridge 専用)。
	// 'never'|'on-request'|'on-failure'|'untrusted' のいずれか。
	// 空文字のとき bridge 側は既定値 'never' を維持する。
	ApprovalPolicy string `json:"approvalPolicy,omitempty"`
	// AdditionalDirectories は cwd 外 read を許可する追加ディレクトリ一覧 (issue #66 A7)。
	// nil のとき bridge 側は既定値 `['/']` (= cwd 外 read を広めに許可) を適用する。
	// 空配列 `[]` のときは制限意図とみなしてそのまま bridge に渡す。
	// bridge/shared/protocol.ts の additionalDirectories と wire-level で 1:1 対応する。
	//
	// json tag は `omitzero` を採用する (Go 1.24+)。`omitempty` だと空スライス `[]string{}` も
	// omit されてしまい、bridge 側で `undefined` と解釈 → デフォルト許可 `['/']` に倒れて
	// 「明示的空配列 = restrict 意図」という seam が wire 上で破綻する。
	// `omitzero` は nil のみを omit し、空スライスは `"additionalDirectories":[]` として残す。
	AdditionalDirectories []string `json:"additionalDirectories,omitzero"`
	// AllowedTools は SDK Options.allowedTools に流す許可ツール一覧。
	// nil / 空のときは bridge 側で SDK 既定 (= 全ツール許可) を維持する。
	// 非空のときは bridge が SDK に allowedTools を渡し、判定ターン等で Edit/Write を禁じる。
	AllowedTools []string `json:"allowedTools,omitempty"`
	// UserHooks は PreToolUse hook chain に挿入する user 定義 hook 一覧 (issue #47)。
	// bridge/shared/protocol.ts の userHookSchema と wire-level で 1:1 対応する。
	// nil / 空のときは bridge 側で hook chain を変更しない (既存 observationHook のみ)。
	UserHooks []UserHook `json:"userHooks,omitempty"`
	// PermissionMode は per-call permissionMode 上書き seam (output-contract-2phase task 1.2)。
	// 値域は closed enum `"" | "default" | "acceptEdits" | "plan" | "bypassPermissions"`
	// (Claude Agent SDK の `permissionMode` 語彙と同期)。空文字 = SDK / bridge 既定を維持する zero-value。
	// Claude bridge は SDK Options.permissionMode に流す。Codex / Cursor bridge では documented no-op。
	// bridge/shared/protocol.ts の runRequestSchema.permissionMode と wire-level で 1:1 対応する。
	PermissionMode string `json:"permissionMode,omitempty"`
	// MaxTurns は per-call turn budget 上書き seam (output-contract-2phase task 1.2)。
	// 0 = SDK 既定 turn budget を維持する zero-value (omitempty)。
	// 正の int を渡したとき Claude bridge は SDK Options.maxTurns に配線、Codex bridge は SDK turn-budget
	// 経路に配線、Cursor bridge は documented no-op。
	// bridge/shared/protocol.ts の runRequestSchema.maxTurns と wire-level で 1:1 対応する。
	MaxTurns int `json:"maxTurns,omitempty"`
}

// TraceContext mirrors provider.TraceContext and bridge/shared/protocol.ts.
// It carries only W3C propagation headers, not raw prompts or session IDs.
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
// bridge/shared/protocol.ts の userHookSchema と wire-level で 1:1 対応する。
type UserHook struct {
	Matcher string   `json:"matcher"`
	Command []string `json:"command"`
}

type Metadata struct {
	WorkflowRunID string `json:"workflowRunId,omitempty"`
	WorkflowID    string `json:"workflowId,omitempty"`
	WorkflowName  string `json:"workflowName,omitempty"`
	StepID        string `json:"stepId,omitempty"`
	StepName      string `json:"stepName,omitempty"`
	AgentCallID   string `json:"agentCallId,omitempty"`
}

type MCPServerInfo struct {
	Name   string `json:"name,omitempty"`
	Status string `json:"status,omitempty"`
}

type SystemInfo struct {
	UUID                 string          `json:"uuid,omitempty"`
	SessionID            string          `json:"sessionId,omitempty"`
	APIKeySource         string          `json:"apiKeySource,omitempty"`
	CWD                  string          `json:"cwd,omitempty"`
	Tools                []string        `json:"tools,omitempty"`
	MCPServers           []MCPServerInfo `json:"mcpServers,omitempty"`
	Model                string          `json:"model,omitempty"`
	ModelReasoningEffort string          `json:"modelReasoningEffort,omitempty"`
	PermissionMode       string          `json:"permissionMode,omitempty"`
	SlashCommands        []string        `json:"slashCommands,omitempty"`
	OutputStyle          string          `json:"outputStyle,omitempty"`
}

type Event struct {
	Type EventType `json:"type"`
	ID   string    `json:"id,omitempty"`
	// ItemID は item-level identity (issue #66 A15+D7)。
	// 現状 reasoning event でのみ運搬する (Codex ReasoningItem.id)。`ID` (= run id, 全 event 共通) と
	// 分離することで「event 種別を跨いで id semantics を一貫させる」protocol 契約を維持する。
	// Claude bridge の thinking_delta のように item id を持たない provider では空文字 (omit)。
	ItemID string `json:"itemId,omitempty"`
	Text   string `json:"text,omitempty"`
	Tool   string `json:"tool,omitempty"`
	// ToolStatus は tool_use event の status (issue #65 D6)。
	// 'started'|'updated'|'completed'|'failed' のいずれか。
	// 空文字のとき status 不明 / 既存ストリームと後方互換の動作 (Codex bridge 以外は空のまま)。
	ToolStatus       string      `json:"status,omitempty"`
	Result           string      `json:"result,omitempty"`
	SessionID        string      `json:"sessionId,omitempty"`
	StructuredOutput any         `json:"structuredOutput,omitempty"`
	SystemInfo       *SystemInfo `json:"systemInfo,omitempty"`
	Message          string      `json:"message,omitempty"`
	// Code は error event の自由文字列識別子で、issue #47 の `hook_block` / `hook_spawn_error`
	// 等と同じ自由規約 namespace で運用する。claude-agent-sdk bridge では Claude SDK の
	// `SDKResultError.subtype` 文字列 (`error_during_execution` / `error_max_turns` /
	// `error_max_budget_usd` / `error_max_structured_output_retries` / `unknown`) を運搬する
	// 規約があり、type-safe な参照のため上記 `ErrorCode*` const を併設している
	// (issue #70 / bridge-result-error-propagation spec)。
	//
	// `error_max_structured_output_retries` は `error` event ではなく `result` event の
	// `StructuredOutput=nil` で表現される (codex / cursor の parse 失敗契約と対称化のため)。
	// このフィールドが `error_max_structured_output_retries` 値で観測されるのは bridge の
	// defensive code path のみで、通常運用では現れない。
	// bridge/shared/protocol.ts の errorEventSchema.code と wire-level で 1:1 対応する。
	Code               string          `json:"code,omitempty"`
	Retryable          bool            `json:"retryable,omitempty"`
	Source             string          `json:"source,omitempty"`
	ObservationType    string          `json:"observationType,omitempty"`
	ObservationSubtype string          `json:"observationSubtype,omitempty"`
	Sequence           int64           `json:"sequence,omitempty"`
	Payload            json.RawMessage `json:"payload,omitempty"`
	PayloadTruncated   bool            `json:"payloadTruncated,omitempty"`
	// Origin は result event 等で cancel 起源を後段に運ぶための field。
	// 空文字 / "user" / "engine" の 3 値を取り、空文字は default 'engine' と同義 (Req 6 後方互換)。
	// bridge/shared/protocol.ts の eventSchema `result` member の `origin` と wire-level で対応する。
	// 後続 task (2.6 Nodebridge launcher) で ConsumeEvents が読み取り、ctx atomic に Store する。
	Origin string `json:"origin,omitempty"`
}

type Encoder struct {
	encoder *json.Encoder
}

func NewEncoder(w io.Writer) *Encoder {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	return &Encoder{encoder: encoder}
}

func (e *Encoder) Encode(v any) error {
	return e.encoder.Encode(v)
}

type Decoder struct {
	scanner *bufio.Scanner
}

func NewDecoder(r io.Reader) *Decoder {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 100*1024*1024)
	return &Decoder{scanner: scanner}
}

func (d *Decoder) Decode() (Event, error) {
	for {
		if !d.scanner.Scan() {
			if err := d.scanner.Err(); err != nil {
				return Event{}, err
			}
			return Event{}, io.EOF
		}

		line := d.scanner.Bytes()
		if len(line) == 0 || line[0] != '{' {
			// JSON オブジェクトでない行はスキップ（ランタイムのログ出力等）
			continue
		}

		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			// JSON パース失敗もスキップ
			continue
		}
		if !knownEventType(event.Type) {
			return Event{}, fmt.Errorf("unknown bridge event type %q", event.Type)
		}
		return event, nil
	}
}

func knownEventType(t EventType) bool {
	switch t {
	case EventReady, EventSystemInit, EventText, EventToolUse, EventReasoning, EventResult, EventError, EventDone, EventObservation:
		return true
	default:
		return false
	}
}
