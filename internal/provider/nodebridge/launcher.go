// Package nodebridge は Node プロセスとして起動する bridge スクリプト
// (claude-agent-sdk / codex-sdk / cursor-sdk) を駆動する共通ランチャを提供する。
//
// 各 provider 実装 (internal/provider/{claude,codex,cursorsdk}) は本パッケージの
// Run / ConsumeEvents / BuildRunRequest を介して bridge プロセスを起動し、
// provider 固有の差分 (ProviderID / 既定スクリプトパス / 環境変数名 /
// observation event からの metrics 抽出関数) のみを Config として渡す。
package nodebridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/kazuki-oshino/rilll/internal/bridge"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/telemetry"
)

const (
	// DefaultNodeCommand は bridge スクリプト起動時のデフォルト node コマンド名。
	DefaultNodeCommand = "node"
	// StderrLimitBytes は bridge の stderr バッファの上限。
	StderrLimitBytes = 1024 * 1024
	// hookBlockErrorCode は bridge が PreToolUse user hook の block を運搬する error event の code (issue #47)。
	// bridge/claude-agent-sdk/src/user-hook.ts と wire-level で 1:1 対応する。
	hookBlockErrorCode = "hook_block"
)

var errBridgeUnsuccessful = errors.New("node bridge returned unsuccessful response")

func resolveStdoutDumpPath(providerID, runID, envValue string, pid int) string {
	if envValue != "enable" {
		return ""
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("rilll-bridge-%s-%d-%s.log", providerID, pid, runID))
}

// MetricsExtractor は observation event を見て response.Metrics を更新する関数。
// provider ごとの observation 形式に応じた抽出ロジックを差し込む。nil の場合は
// 抽出を行わない。
type MetricsExtractor func(*provider.AgentResponse, bridge.Event)

// Config は provider 固有のメタデータ。Run / ConsumeEvents が参照する。
type Config struct {
	// ProviderID は AgentResponse.SystemInfo に埋め込む provider 識別子。
	ProviderID provider.ProviderID
	// BridgeEnvVar は ResolveDefaultBridgePath が最初に参照する環境変数名。
	BridgeEnvVar string
	// DefaultBridgePath はリポジトリルート相対の bridge スクリプト既定パス。
	DefaultBridgePath string
	// BuildHint は bridge dist が未ビルドだったときにエラーへ含める npm スクリプト名。
	// 例: "npm run build:claude-bridge"。空のときは `just build-bridge` のみを案内する。
	BuildHint string
	// ExtractMetrics は observation event から metrics を抽出する関数 (nil 可)。
	ExtractMetrics MetricsExtractor
}

// RunOptions は Run の呼び出しパラメータ。Config に加えて、Provider が保持する
// NodeCommand / BridgeScript の上書き値を渡す。
type RunOptions struct {
	Config
	NodeCommand  string
	BridgeScript string
}

// Run は Node bridge プロセスを起動し、stdin に Run リクエストを書き込み、
// stdout を ConsumeEvents で逐次解釈して AgentResponse を返す。
//
// 失敗時は failureResponse を介して error を AgentResponse.Content に格納した上で
// nil error を返す (呼び出し側 = agent ループ側で再試行/中断の判断を行う既存設計)。
func Run(ctx context.Context, opts RunOptions, request provider.AgentRequest, onEvent provider.AgentEventCallback) (provider.AgentResponse, error) {
	telemetryID := telemetry.NodeBridgeIdentity{
		ProviderID:    string(opts.ProviderID),
		WorkflowRunID: request.Metadata.WorkflowRunID,
		WorkflowID:    request.Metadata.WorkflowID,
		StepID:        request.Metadata.StepID,
		AgentCallID:   request.Metadata.AgentCallID,
		Model:         request.Model,
		Effort:        request.Effort,
	}
	startedAt := time.Now()
	ctx, span := telemetry.StartNodeBridgeRun(ctx, telemetryID)
	var runErr error
	defer func() {
		telemetry.EndNodeBridgeRun(ctx, span, telemetryID, startedAt, runErr)
	}()

	nodeCommand := opts.NodeCommand
	if nodeCommand == "" {
		nodeCommand = DefaultNodeCommand
	}
	bridgeScript := opts.BridgeScript
	if bridgeScript == "" {
		bridgeScript = opts.DefaultBridgePath
	}

	// dist 未ビルド等で bridge スクリプトが見つからない場合、Node の生スタックを
	// そのまま failureResponse に詰めると初見ユーザーには読めないので、ビルド手順
	// 込みの分かりやすいエラーに置き換える。
	if err := ensureBridgeScriptExists(bridgeScript, opts.Config); err != nil {
		runErr = err
		telemetry.AddSpanEvent(span, "nodebridge.bridge_script_missing")
		return FailureResponse(err.Error(), request, opts.ProviderID), nil
	}

	cmd := exec.CommandContext(ctx, nodeCommand, bridgeScript)
	cmd.Dir = request.CWD
	cmd.Env = telemetry.BridgeProcessEnv(os.Environ())

	stdin, err := cmd.StdinPipe()
	if err != nil {
		runErr = err
		return provider.AgentResponse{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		runErr = err
		return provider.AgentResponse{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		runErr = err
		return provider.AgentResponse{}, err
	}

	requestID := NewRequestID()
	var stdoutReader io.Reader = stdout
	var stdoutDumpFile *os.File
	if dumpPath := resolveStdoutDumpPath(string(opts.ProviderID), requestID, os.Getenv("RILLL_BRIDGE_STDOUT_DUMP"), os.Getpid()); dumpPath != "" {
		// デバッグ: bridge stdout の生データをファイルにダンプする。作成失敗時も NDJSON 受信は継続する。
		if file, err := os.OpenFile(dumpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
			stdoutDumpFile = file
			stdoutReader = io.TeeReader(stdout, stdoutDumpFile)
		}
	}

	var stderrBuf limitedBuffer
	stderrBuf.limit = StderrLimitBytes
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&stderrBuf, stderr)
	}()

	if err := cmd.Start(); err != nil {
		runErr = err
		return provider.AgentResponse{}, err
	}
	telemetry.AddSpanEvent(span, "nodebridge.process_started")

	encoder := bridge.NewEncoder(stdin)
	request.TraceContext = telemetry.InjectTraceContext(ctx)
	if err := encoder.Encode(BuildRunRequest(requestID, request)); err != nil {
		_ = cmd.Process.Kill()
		runErr = err
		return provider.AgentResponse{}, err
	}

	done := make(chan struct{})
	// cancelWG は cancel goroutine の終了を待機するための WaitGroup。
	// stdin.Close() と encoder.Encode() が並行して走らないことを保証し、
	// race detector による検出と goroutine leak を防ぐ。
	var cancelWG sync.WaitGroup
	cancelWG.Go(func() {
		select {
		case <-ctx.Done():
			_ = encoder.Encode(bridge.Request{Type: bridge.RequestCancel, ID: requestID})
		case <-done:
		}
	})

	response, consumeErr := ConsumeEvents(ctx, stdoutReader, request, onEvent, opts.Config)
	close(done)
	cancelWG.Wait()
	_ = stdin.Close()
	if stdoutDumpFile != nil {
		_ = stdoutDumpFile.Close()
	}

	waitErr := cmd.Wait()
	wg.Wait()

	if consumeErr != nil {
		runErr = consumeErr
		return FailureResponse(consumeErr.Error(), request, opts.ProviderID), nil
	}
	if waitErr != nil && !response.Success {
		runErr = waitErr
		return FailureResponse(fmt.Sprintf("%v: %s", waitErr, stderrBuf.String()), request, opts.ProviderID), nil
	}
	if !response.Success {
		runErr = errBridgeUnsuccessful
	}
	return response, nil
}

// BuildRunRequest は AgentRequest を bridge.Request (type=run) にマップする。
// provider 間で完全に同一なため共通化している。
func BuildRunRequest(id string, request provider.AgentRequest) bridge.Request {
	var outputFormat *bridge.OutputFormat
	if request.OutputFormat != nil {
		outputFormat = &bridge.OutputFormat{
			Type:   request.OutputFormat.Type,
			Schema: request.OutputFormat.Schema,
		}
	}
	// UserHooks は zero-value のとき nil のまま渡し、json:"userHooks,omitempty" で wire 上に
	// 出さないようにする (bridge 側 zod は optional)。非 zero のときは Command スライスを浅くコピーして
	// 呼び出し元の slice 共有を避ける (将来 worker が複数 request を並列で扱う場合の安全側マージン)。
	var userHooks []bridge.UserHook
	if len(request.UserHooks) > 0 {
		userHooks = make([]bridge.UserHook, 0, len(request.UserHooks))
		for _, h := range request.UserHooks {
			userHooks = append(userHooks, bridge.UserHook{
				Matcher: h.Matcher,
				Command: append([]string(nil), h.Command...),
			})
		}
	}
	var modelParams []bridge.ModelParam
	if len(request.ModelParams) > 0 {
		modelParams = make([]bridge.ModelParam, 0, len(request.ModelParams))
		for _, param := range request.ModelParams {
			modelParams = append(modelParams, bridge.ModelParam{
				ID:    param.ID,
				Value: param.Value,
			})
		}
	}
	return bridge.Request{
		Type:            bridge.RequestRun,
		ID:              id,
		Prompt:          request.Prompt,
		CWD:             request.CWD,
		ResumeSessionID: request.ResumeSessionID,
		OutputFormat:    outputFormat,
		AkashicLevel:    request.AkashicLevel,
		TraceContext: bridge.TraceContext{
			TraceParent: request.TraceContext.TraceParent,
			TraceState:  request.TraceContext.TraceState,
			Baggage:     request.TraceContext.Baggage,
		},
		Metadata: bridge.Metadata{
			WorkflowRunID: request.Metadata.WorkflowRunID,
			WorkflowID:    request.Metadata.WorkflowID,
			WorkflowName:  request.Metadata.WorkflowName,
			StepID:        request.Metadata.StepID,
			StepName:      request.Metadata.StepName,
			AgentCallID:   request.Metadata.AgentCallID,
		},
		// review-workflow Task 1.3: model 上書き seam を bridge へ素通し。zero-value のときは
		// json:"model,omitempty" で送信を抑止し、bridge 側は provider 既定モデルを維持する。
		Model: request.Model,
		// issue #65 A2/A3/A4: per-call reasoning effort / sandbox / approval を bridge へ素通し。
		// 空文字のとき omitempty で送信を抑止し、bridge 側は各 SDK の既定値を維持する。
		Effort:           request.Effort,
		ModelParams:      modelParams,
		ReasoningSummary: request.ReasoningSummary,
		SandboxMode:      request.SandboxMode,
		ApprovalPolicy:   request.ApprovalPolicy,
		// issue #66 A7: cwd 外 read 許可 (additionalDirectories) を bridge へ素通し。
		// nil のとき `omitzero` で送信を抑止し、bridge 側は既定値 `['/']` (= デフォルト許可) を適用する。
		// 空配列 `[]string{}` のときは制限意図とみなして bridge に空配列のまま渡す
		// (`omitempty` だと空配列も omit されてこの seam が壊れる。詳細は bridge.Request.AdditionalDirectories の json tag コメント参照)。
		AdditionalDirectories: request.AdditionalDirectories,
		// kiro-spec-loop fix-judge ターン等で Edit/Write を禁じるための seam (Phase C)。
		// nil / 空のときは json:"allowedTools,omitempty" で送信を抑止し、bridge 側は SDK 既定 (全ツール許可) を維持する。
		AllowedTools: request.AllowedTools,
		// issue #47: PreToolUse user hook を bridge へ素通し。
		// nil / 空のときは json:"userHooks,omitempty" で送信を抑止し、bridge 側は既存 observationHook のみが走る。
		UserHooks: userHooks,
		// output-contract-2phase task 3.1/3.2/3.3: per-call permissionMode / maxTurns を bridge へ素通し。
		// zero-value (空文字 / 0) のとき bridge 側 omitempty で wire 上から omit され、各 provider bridge は
		// SDK 既定値 (Claude=`acceptEdits` / Codex / Cursor=既定 turn budget) を維持する後方互換ガードを保つ。
		// 各 provider bridge 側の挙動:
		//   - Claude bridge: SDK Options.permissionMode / Options.maxTurns に native 配線
		//   - Codex bridge:  maxTurns は prompt fallback 配線、permissionMode は documented no-op
		//   - Cursor bridge: 両 field とも documented no-op
		PermissionMode: request.PermissionMode,
		MaxTurns:       request.MaxTurns,
	}
}

// ConsumeEvents は bridge stdout を逐次デコードして provider.AgentEvent に変換する。
// observation event 受信時は cfg.ExtractMetrics を呼び出して metrics を更新する。
func ConsumeEvents(ctx context.Context, r io.Reader, request provider.AgentRequest, onEvent provider.AgentEventCallback, cfg Config) (provider.AgentResponse, error) {
	decoder := bridge.NewDecoder(r)
	response := provider.AgentResponse{
		SystemInfo: &provider.AgentSystemInfo{
			ProviderID: cfg.ProviderID,
			CWD:        request.CWD,
		},
	}

	for {
		select {
		case <-ctx.Done():
			return response, ctx.Err()
		default:
		}

		event, err := decoder.Decode()
		if err != nil {
			if err == io.EOF {
				return response, nil
			}
			return response, err
		}

		switch event.Type {
		case bridge.EventReady:
			continue
		case bridge.EventSystemInit:
			response.SystemInfo = ToProviderSystemInfo(event.SystemInfo, cfg.ProviderID)
		case bridge.EventText:
			if onEvent != nil {
				onEvent(provider.AgentEvent{Type: provider.EventText, Text: event.Text})
			}
		case bridge.EventToolUse:
			if onEvent != nil {
				onEvent(provider.AgentEvent{
					Type:       provider.EventToolUse,
					Tool:       event.Tool,
					ToolStatus: event.ToolStatus,
				})
			}
		case bridge.EventReasoning:
			// issue #66 A15+D7: 思考プロセス (Codex ReasoningItem / Claude thinking_delta) を
			// agent_message / tool_use と別 channel で上位に伝える。response.Content には混入させない。
			// ItemID は bridge.Event.ItemID (= Codex ReasoningItem.id) を素通しし、
			// 上位 (UI / akashic) で reasoning を item 単位に集約できるようにする。
			if onEvent != nil {
				onEvent(provider.AgentEvent{
					Type:   provider.EventReasoning,
					Text:   event.Text,
					ItemID: event.ItemID,
				})
			}
		case bridge.EventObservation:
			if cfg.ExtractMetrics != nil {
				cfg.ExtractMetrics(&response, event)
			}
			if onEvent != nil {
				onEvent(provider.AgentEvent{
					Type:               provider.EventObservation,
					Source:             event.Source,
					ObservationType:    event.ObservationType,
					ObservationSubtype: event.ObservationSubtype,
					Sequence:           event.Sequence,
					Payload:            event.Payload,
					PayloadTruncated:   event.PayloadTruncated,
				})
			}
		case bridge.EventResult:
			response.Content = event.Result
			response.SessionID = event.SessionID
			response.StructuredOutput = event.StructuredOutput
			response.Success = event.Result != "" || event.StructuredOutput != nil
			// task 2.6 (Req 3.2): cancelled な result event は origin ('user'/'engine') を伴う。
			// ctx に attach 済みの atomic ref に Store して agent 層 (recordAgentCallFinish) へ
			// 単方向に伝搬する。正常終了 (origin 未指定 = 空文字) では bridge.StoreCancelOrigin
			// が no-op となり ctx atomic は空文字のまま (= cancel 不在) の契約を保つ。
			bridge.StoreCancelOrigin(ctx, event.Origin)
		case bridge.EventDone:
			if event.Result != "" {
				response.Content = event.Result
				response.Success = true
			}
			if event.SessionID != "" {
				response.SessionID = event.SessionID
			}
			if response.SystemInfo == nil {
				response.SystemInfo = &provider.AgentSystemInfo{ProviderID: cfg.ProviderID, CWD: request.CWD}
			}
			response.SystemInfo.SessionID = response.SessionID
			if onEvent != nil {
				onEvent(provider.AgentEvent{Type: provider.EventDone, Content: response.Content, SessionID: response.SessionID})
			}
			return response, nil
		case bridge.EventError:
			// issue #47: PreToolUse user hook が block を返したときに bridge が emit する
			// `error` event (code='hook_block', retryable=false) は run を中断せず観測のみ行う。
			// 理由: bridge 側 SDK ループはその後も継続するため、Go 側だけ run を畳むと両者の状態が
			// 乖離し akashic / 上位 workflow の整合性が崩れる。上位 workflow は AgentEventCallback で
			// EventError を受信して、次回 run の判断材料 (例: 再試行抑制 / human escalation) に使う。
			if event.Code == hookBlockErrorCode {
				if onEvent != nil {
					onEvent(provider.AgentEvent{
						Type:      provider.EventError,
						Message:   event.Message,
						Code:      event.Code,
						Retryable: event.Retryable,
					})
				}
				continue
			}
			return provider.AgentResponse{
				Content:   event.Message,
				SessionID: request.ResumeSessionID,
				Success:   false,
				SystemInfo: &provider.AgentSystemInfo{
					ProviderID: cfg.ProviderID,
					SessionID:  request.ResumeSessionID,
					CWD:        request.CWD,
				},
			}, nil
		}
	}
}

// ToProviderSystemInfo は bridge.SystemInfo を provider.AgentSystemInfo に変換する。
func ToProviderSystemInfo(info *bridge.SystemInfo, providerID provider.ProviderID) *provider.AgentSystemInfo {
	if info == nil {
		return &provider.AgentSystemInfo{ProviderID: providerID}
	}
	servers := make([]provider.AgentMCPServerInfo, 0, len(info.MCPServers))
	for _, server := range info.MCPServers {
		servers = append(servers, provider.AgentMCPServerInfo{Name: server.Name, Status: server.Status})
	}
	return &provider.AgentSystemInfo{
		ProviderID:           providerID,
		UUID:                 info.UUID,
		SessionID:            info.SessionID,
		APIKeySource:         info.APIKeySource,
		CWD:                  info.CWD,
		Tools:                info.Tools,
		MCPServers:           servers,
		Model:                info.Model,
		ModelReasoningEffort: info.ModelReasoningEffort,
		PermissionMode:       info.PermissionMode,
		SlashCommands:        info.SlashCommands,
		OutputStyle:          info.OutputStyle,
	}
}

// FailureResponse は bridge プロセス起動 / 通信失敗時の AgentResponse を組み立てる。
func FailureResponse(message string, request provider.AgentRequest, providerID provider.ProviderID) provider.AgentResponse {
	return provider.AgentResponse{
		Content:   message,
		SessionID: request.ResumeSessionID,
		Success:   false,
		SystemInfo: &provider.AgentSystemInfo{
			ProviderID: providerID,
			SessionID:  request.ResumeSessionID,
			CWD:        request.CWD,
		},
	}
}

// NewRequestID は bridge へ渡す Run リクエスト ID を生成する。
func NewRequestID() string {
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

// ResolveDefaultBridgePath は次の優先順で bridge スクリプトの絶対パスを決める:
//  1. 環境変数 envVar が設定されていればその値
//  2. cwd から親ディレクトリを辿って defaultRel が存在すればそのパス
//  3. このソースファイル位置からリポジトリルートを推定したパス
//  4. 上記いずれも見つからなければ defaultRel をそのまま返す
//
// 注: runtime.Caller(0) はこのファイル (internal/provider/nodebridge/launcher.go)
// の絶対パスを返すため、3 回の `..` でリポジトリルートに到達する。各 provider 配下に
// 配置していた旧コードと同じ階層関係なので、引き継ぎ後も挙動は等価。
func ResolveDefaultBridgePath(envVar, defaultRel string) string {
	if envVar != "" {
		if path := os.Getenv(envVar); path != "" {
			return path
		}
	}
	if path := findBridgeFromWorkingDirectory(defaultRel); path != "" {
		return path
	}
	if _, file, _, ok := runtime.Caller(0); ok {
		repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
		path := filepath.Join(repoRoot, defaultRel)
		if fileExists(path) {
			return path
		}
	}
	return defaultRel
}

func findBridgeFromWorkingDirectory(defaultRel string) string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, defaultRel)
		if fileExists(candidate) {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// ensureBridgeScriptExists は bridge スクリプトの実体が file として存在することを
// 確認する。dist 未ビルド時の Node スタックトレースを置き換えるため、見つからない
// ときはビルド手順を含む案内メッセージを返す。
func ensureBridgeScriptExists(path string, cfg Config) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("bridge script not found: %s\n%s", path, buildHintMessage(cfg))
		}
		return fmt.Errorf("bridge script を確認できませんでした (%s): %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("bridge script のパスがディレクトリを指しています (%s): ファイルを指定してください", path)
	}
	return nil
}

// buildHintMessage は欠落時に表示する 1 行ヒントを構築する。BuildHint が指定されて
// いれば provider 個別の npm スクリプトを案内し、未指定の場合は `just build-bridge`
// のみに留める。
func buildHintMessage(cfg Config) string {
	if cfg.BuildHint != "" {
		return fmt.Sprintf("ヒント: `%s` (または `just build-bridge`) を実行してから再試行してください。", cfg.BuildHint)
	}
	return "ヒント: `just build-bridge` を実行してから再試行してください。"
}

// PayloadAsMap は observation event の payload を map にデコードするヘルパ。
// MetricsExtractor 実装で共通的に使う。
func PayloadAsMap(event bridge.Event) (map[string]any, bool) {
	if len(event.Payload) == 0 {
		return nil, false
	}
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return nil, false
	}
	return payload, true
}

// Number は any 型の数値を float64 として読み出す。MetricsExtractor 用ヘルパ。
func Number(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
}

// IntNumber は any 型の数値を int64 として読み出す。MetricsExtractor 用ヘルパ。
func IntNumber(value any) (int64, bool) {
	if n, ok := Number(value); ok {
		return int64(n), true
	}
	return 0, false
}

// limitedBuffer は bytes.Buffer を一定サイズで打ち止めにするラッパ。bridge stderr の
// 暴走で OOM するのを防ぐ。
type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit > 0 && b.Len()+len(p) > b.limit {
		return 0, fmt.Errorf("buffer exceeded limit")
	}
	return b.Buffer.Write(p)
}
