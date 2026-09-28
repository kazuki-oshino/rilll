package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/bridge"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/provider/middleware/streamidle"
	"github.com/kazuki-oshino/rilll/internal/telemetry"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// 失敗カテゴリ。`agent_calls.failure_category` および workflow_runs.error の
// プレフィックスで使用する。issue #2 参照。
const (
	FailureCategoryStreamIdleTimeout = "stream_idle_timeout"
	FailureCategoryExternalAbort     = "external_abort"
	FailureCategoryPartTimeout       = "part_timeout"
	FailureCategoryProviderError     = "provider_error"
	// FailureCategoryProviderUnknown は ProviderOverride / a.providerID で指定された provider が
	// agent.providers registry に未登録だったときの分類 (review-workflow design.md Req 5.6)。
	// 呼び出し側 (review.Executor 等) は errors.Is(err, ErrUnknownProvider) で構造化分岐する。
	FailureCategoryProviderUnknown = "provider_unknown"
	// FailureCategoryInvalidOptions は output-contract-2phase spec で導入された CallAgentOptions の
	// 宣言型エラー (OutputContract.Name 重複 / OutputContract.FormatFacet 空文字) を CallAgent 入口
	// precheck (validateContracts) が検出したときの分類 (R1.4 / R1.5)。
	//
	// 本カテゴリの error は Phase 1 dispatch (provider.Call) も akashic agent_calls 行追加も
	// 発生させずに即時 return される構造的ガードであり、呼び出し側 (workflow など) は
	// CategoryFromError(err) == FailureCategoryInvalidOptions で「options 自体が不正」という
	// 意味的分類を取り出せる。sentinel error (ErrDuplicateContractName / ErrEmptyFormatFacet) と
	// errors.Is を併用すれば具体的な違反種別まで識別可能。
	FailureCategoryInvalidOptions = "invalid_options"
)

// ErrUnknownProvider は ProviderOverride / a.providerID で指定された provider が
// agent.providers registry に未登録だったときに返るセンチネル。CallError でラップされ、
// 呼び出し側は errors.Is(err, ErrUnknownProvider) で分岐できる
// (review-workflow design.md §agent / provider 拡張)。
var ErrUnknownProvider = errors.New("unknown agent provider")

type Agent struct {
	providerID provider.ProviderID
	providers  map[provider.ProviderID]provider.AgentProvider
	recorder   akashic.Recorder
	// reader / artifactReader は recorder と同 instance を別 interface 視点で保持する seam。
	// recorder が *akashic.SQLiteRecorder のときのみ非 nil で、Reader / ArtifactReader を兼用している
	// 事実 (workflow-self-improve design.md §workflow/optional iface) を利用する。
	// recorder が NoopRecorder のときは両者 nil となり、improve-workflow 側は context_load_failed
	// verdict で安全に終了できる。
	reader         akashic.Reader
	artifactReader akashic.ArtifactReader
	akashicLevel   string
	modelDefaults  map[provider.ProviderID]provider.ModelSettings
	// activeCancelOrigin は UI の foreground call cancel-origin Pointer
	// (memory-distiller-trigger-policy task 4.2, Req 3.1 / 3.2)。
	// SetActiveCancelOrigin で UI Model 寿命と等価な単一 Pointer が注入されると、
	// CallAgent 入口で call スコープの *atomic.Value (callRef) を Store して
	// foreground register し、defer で CompareAndSwap(callRef, nil) する (LIFO 安全)。
	// nil 許容: TUI 不在経路 (CLI / 旧 callsite / 背景 subagent dispatch / 並列 evaluator)
	// では未注入のまま、CallAgent は ctx attach のみ行い UI register を skip する
	// (= 「背景 call が UI register をスキップする契約」を Pointer 不在で構造的に表現)。
	activeCancelOrigin *atomic.Pointer[atomic.Value]
}

func New(providerID provider.ProviderID, providers map[provider.ProviderID]provider.AgentProvider, recorders ...akashic.Recorder) *Agent {
	recorder := akashic.Recorder(akashic.NoopRecorder{})
	if len(recorders) > 0 && recorders[0] != nil {
		recorder = recorders[0]
	}
	// recorder が akashic.Reader / ArtifactReader を兼ねている場合のみ取り出す。
	// *SQLiteRecorder は両 interface を実装しているが、NoopRecorder は実装しない。
	// type assertion 失敗時はゼロ値 (nil) のまま残す。
	reader, _ := recorder.(akashic.Reader)
	artifactReader, _ := recorder.(akashic.ArtifactReader)
	return &Agent{
		providerID:     providerID,
		providers:      providers,
		recorder:       recorder,
		reader:         reader,
		artifactReader: artifactReader,
		akashicLevel:   "summary",
	}
}

func (a *Agent) SetProvider(providerID provider.ProviderID) {
	a.providerID = providerID
}

// SetAkashicLevel はアカシックレコードへの記録粒度を設定する。
// 空文字列が渡された場合は既定値 "summary" にフォールバックする。
func (a *Agent) SetAkashicLevel(level string) {
	if level == "" {
		level = "summary"
	}
	a.akashicLevel = level
}

// SetProviderModelDefaults は provider ごとの model/reasoning 既定値を設定する。
// 空の ModelSettings は削除相当として扱い、provider / SDK 側の既定値へ戻す。
func (a *Agent) SetProviderModelDefaults(defaults map[provider.ProviderID]provider.ModelSettings) {
	a.modelDefaults = make(map[provider.ProviderID]provider.ModelSettings)
	for id, settings := range defaults {
		if settings.Model == "" && settings.Effort == "" && settings.ReasoningSummary == "" && len(settings.ModelParams) == 0 {
			continue
		}
		a.modelDefaults[id] = settings
	}
}

// AkashicRecorder は Agent が保持しているアカシックレコード Recorder を返す。
func (a *Agent) AkashicRecorder() akashic.Recorder {
	return a.recorder
}

// SetActiveCancelOrigin は UI の foreground call cancel-origin Pointer を Agent に注入する
// (memory-distiller-trigger-policy task 4.2, Req 3.1 / 3.2)。
//
// pointer が非 nil の場合のみ、後続の CallAgent は入口で call スコープ *atomic.Value を
// Store して foreground register し、defer で CompareAndSwap(callRef, nil) する (LIFO 安全)。
//
// nil / 未呼び出しのときは CallAgent は UI register を skip し、ctx attach
// (bridge.WithCancelOrigin) のみ行う。これにより TUI 不在の CLI 経路や、
// 背景 subagent dispatch / 並列 evaluator 経路で「UI register をスキップする契約」を
// Pointer 不在で構造的に表現できる (design.md §UI seam closing / Issue 1/2 closing)。
func (a *Agent) SetActiveCancelOrigin(pointer *atomic.Pointer[atomic.Value]) {
	a.activeCancelOrigin = pointer
}

// AkashicReader は Agent が保持している akashic.Reader を返す。
// recorder が *akashic.SQLiteRecorder のときは Reader と同 instance を返し、
// NoopRecorder などで Reader interface を満たさない場合は nil を返す
// (workflowctx.AkashicReaderProvider optional interface を満たす seam)。
func (a *Agent) AkashicReader() akashic.Reader {
	return a.reader
}

// AkashicArtifactReader は Agent が保持している akashic.ArtifactReader を返す。
// recorder が *akashic.SQLiteRecorder のときは ArtifactReader と同 instance を返し、
// NoopRecorder などで ArtifactReader interface を満たさない場合は nil を返す
// (workflowctx.AkashicArtifactReaderProvider optional interface を満たす seam)。
func (a *Agent) AkashicArtifactReader() akashic.ArtifactReader {
	return a.artifactReader
}

func (a *Agent) CallAgent(ctx context.Context, prompt string, cwd string, onEvent provider.AgentEventCallback, resumeSessionID string, options *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
	// ★ memory-distiller-trigger-policy task 4.2 (Req 3.1 / 3.2):
	// call スコープ *atomic.Value を 1 個生成し、ctx に attach する。これにより
	// 1 cancel request → 1 agent_call の物理 1:1 対応が保たれ、並列 in-flight 時の
	// origin 混線を構造的に防ぐ (design.md §UI seam closing / Issue 1/2 closing)。
	// ctx attach は activeCancelOrigin Pointer の有無に依らず常に行う:
	// nodebridge launcher (task 2.6) が result event の origin を ctx atomic に
	// Store する経路は UI 不在環境 (CLI / test) でも動作する必要があるため。
	callRef := &atomic.Value{}
	ctx = bridge.WithCancelOrigin(ctx, callRef)

	// activeCancelOrigin Pointer が注入されている場合のみ、UI に foreground register
	// する。Pointer 未注入 (= TUI 不在 / 背景 subagent dispatch / 並列 evaluator) は
	// register skip + ctx attach のみで「背景 call が UI register をスキップする契約」を
	// 維持する (Issue 2 closing で確定した境界劣化防止)。
	if a.activeCancelOrigin != nil {
		a.activeCancelOrigin.Store(callRef)
		// LIFO 安全: 入れ子 call が後から Store した別 ref を上書きしないよう、
		// CompareAndSwap で「自分の callRef」のみクリアする。現 active が他 call の
		// ref を指しているときは no-op。
		defer a.activeCancelOrigin.CompareAndSwap(callRef, nil)
	}

	// ★ output-contract-2phase Task 6.1 (R1.4 / R1.5):
	// CallAgentOptions.OutputContracts が 1 件以上指定されているときに限り、Phase 1 dispatch
	// より前に validateContracts (純関数 precheck) を呼ぶ。Name 重複 / FormatFacet 空文字を
	// 検出したら provider.Call も recordAgentCallStart も呼ばず、即時に
	// CallError{Category: FailureCategoryInvalidOptions} を return することで
	// 「Phase 1 / Phase 2 のいずれも実行しない」R1.4 / R1.5 を構造的に保証する。
	//
	// OutputContracts 未指定 (nil / 空 slice) の経路では precheck をスキップし、
	// legacy path と完全に同一挙動を維持する (R1.3 後方互換ガード)。
	if options != nil && len(options.OutputContracts) > 0 {
		if err := validateContracts(options.OutputContracts); err != nil {
			return provider.AgentResponse{}, &CallError{Category: FailureCategoryInvalidOptions, Err: err}
		}
	}

	// ProviderOverride 指定時は a.providerID を上書きする。zero-value のときは既存挙動を維持
	// (review-workflow design.md §agent / provider 拡張、Req 5.5)。
	pid := a.providerID
	if options != nil && options.ProviderOverride != "" {
		pid = options.ProviderOverride
	}
	var outputFormat *provider.OutputFormat
	if options != nil {
		outputFormat = options.OutputFormat
	}
	metadata := provider.AgentMetadata{AgentCallID: newAgentCallID()}
	if runMetadata, ok := workflowctx.RunMetadataFromContext(ctx); ok {
		metadata.WorkflowRunID = runMetadata.WorkflowRunID
		metadata.WorkflowID = runMetadata.WorkflowID
		metadata.WorkflowName = runMetadata.WorkflowName
	}
	if options != nil {
		metadata.StepID = options.StepID
		metadata.StepName = options.StepName
	}
	startedAt := time.Now().UTC()
	recordCtx := context.WithoutCancel(ctx)
	// ★ previous-response-passing Task 3.1 (Req 1.2 / 5.1 / 6.1):
	// 前段応答前置を有効化された呼び出しでは、recordAgentCallStart より前で prompt を
	// combined prompt に置き換える。これにより akashic agent_calls.prompt と
	// provider.AgentRequest.Prompt が同一文字列になり、auditability の構造的不変式が
	// 実装順序として固定される (playbook: audit-log-mutation-order)。
	// content == "" のときは前置を行わず raw prompt のまま (Req 1.4 no-op 経路)。
	if options != nil && options.IncludePreviousResponse && options.PreviousResponseContent != "" {
		prompt = prependPreviousResponse(prompt, options.PreviousResponseContent)
	}
	outputFormatType := ""
	if outputFormat != nil {
		outputFormatType = outputFormat.Type
	}
	telemetryID := telemetry.AgentIdentity{
		WorkflowRunID:   metadata.WorkflowRunID,
		WorkflowID:      metadata.WorkflowID,
		WorkflowName:    metadata.WorkflowName,
		StepID:          metadata.StepID,
		StepName:        metadata.StepName,
		AgentCallID:     metadata.AgentCallID,
		ProviderID:      string(pid),
		ResumeSessionID: resumeSessionID,
		OutputFormat:    outputFormatType,
	}
	ctx, span := telemetry.StartAgentCall(ctx, telemetryID)
	// provider lookup は start レコードを必ず残せるよう、lookup 前に start を記録する。
	// override 指定時/未指定時の両経路で agent_calls.provider_id に override 後の値が入る (Req 5.5)。
	a.recordAgentCallStart(recordCtx, pid, prompt, cwd, resumeSessionID, outputFormat, metadata, startedAt)
	p, ok := a.providers[pid]
	if !ok {
		// override 経路 / single-provider 経路の両方で同一センチネルを返し、呼び出し側が
		// errors.Is(err, ErrUnknownProvider) で構造化分岐できるようにする (Req 5.6)。
		wrapped := fmt.Errorf("%w: %q", ErrUnknownProvider, pid)
		callErr := &CallError{Category: FailureCategoryProviderUnknown, Err: wrapped}
		finishedAt := time.Now().UTC()
		a.recordAgentCallFinish(recordCtx, metadata.AgentCallID, provider.AgentResponse{}, callErr, FailureCategoryProviderUnknown, time.Since(startedAt), finishedAt)
		telemetry.EndAgentCall(ctx, span, telemetryID, FailureCategoryProviderUnknown, time.Since(startedAt), telemetry.AgentCallMetrics{}, callErr)
		a.recordLearningReflectionJob(recordCtx, metadata, pid, cwd, provider.AgentResponse{}, finishedAt)
		return provider.AgentResponse{
			SystemInfo: &provider.AgentSystemInfo{ProviderID: pid, CWD: cwd},
		}, callErr
	}
	wrappedOnEvent := func(event provider.AgentEvent) {
		if event.Type == provider.EventObservation {
			a.recordRawEvent(recordCtx, metadata, event)
			return
		}
		if onEvent != nil {
			onEvent(event)
		}
	}
	req := provider.AgentRequest{
		Prompt:          prompt,
		CWD:             cwd,
		ResumeSessionID: resumeSessionID,
		OutputFormat:    outputFormat,
		AkashicLevel:    a.akashicLevel,
		TraceContext:    telemetry.InjectTraceContext(ctx),
		Metadata:        metadata,
	}
	if settings, ok := a.modelDefaults[pid]; ok {
		req.Model = settings.Model
		req.Effort = settings.Effort
		req.ModelParams = append([]provider.ModelParam(nil), settings.ModelParams...)
		req.ReasoningSummary = settings.ReasoningSummary
	}
	if options != nil {
		// ProviderModelOverrides / ProviderEffortOverrides は解決後 provider ID に対する
		// per-provider 上書き。map に key が存在すれば値が空でも採用し、provider default
		// (modelDefaults / TUI config) を明示的にクリアできる semantics を提供する。
		// (例: /git-commit は CursorSDK に composer-2.5 + effort="" を強制し、TUI で
		//  gpt-5.5-xhigh を選んでいても effort を確実に omit する。)
		// 汎用 ModelOverride / EffortOverride も非空のみ採用しつつ、provider-specific
		// override が最終的に勝つ優先順位は維持。
		if options.ModelOverride != "" {
			req.Model = options.ModelOverride
			req.ModelParams = nil
		}
		if model, ok := options.ProviderModelOverrides[pid]; ok {
			req.Model = model
			req.ModelParams = nil
		}
		if options.EffortOverride != "" {
			req.Effort = options.EffortOverride
			req.ModelParams = nil
		}
		if effort, ok := options.ProviderEffortOverrides[pid]; ok {
			req.Effort = effort
			req.ModelParams = nil
		}
		// ProviderModelParamsOverrides は effort override の後に適用する。
		// /git-commit は effort="" と fast=false を同時に指定するため、effort 適用時の
		// ModelParams クリア後に variant params を再設定する必要がある。
		if params, ok := options.ProviderModelParamsOverrides[pid]; ok {
			req.ModelParams = append([]provider.ModelParam(nil), params...)
		}
		// kiro-spec-loop fix-judge 等で Edit/Write を禁じる allowedTools 制限を AgentRequest に伝搬。
		// nil / 空のときは provider 側で omitempty により送信抑止 → SDK 既定 (全ツール許可) を維持。
		req.AllowedTools = options.AllowedTools
	}
	telemetryID.Model = req.Model
	telemetryID.Effort = req.Effort
	telemetry.SetSpanAttributes(span, telemetry.AgentSpanAttributes(telemetryID)...)
	resp, err := p.Call(ctx, req, wrappedOnEvent)
	if resp.SystemInfo == nil {
		resp.SystemInfo = &provider.AgentSystemInfo{}
	}
	if resp.SystemInfo.Model == "" {
		resp.SystemInfo.Model = req.Model
	}
	if resp.SystemInfo.ModelReasoningEffort == "" {
		resp.SystemInfo.ModelReasoningEffort = req.Effort
	}
	resp.SystemInfo.ProviderID = p.ID()
	if resp.SystemInfo.CWD == "" {
		resp.SystemInfo.CWD = cwd
	}
	failureCategory := classifyFailure(ctx, resp, err)
	finishedAt := time.Now().UTC()
	a.recordAgentCallFinish(recordCtx, metadata.AgentCallID, resp, err, failureCategory, time.Since(startedAt), finishedAt)
	telemetry.EndAgentCall(ctx, span, telemetryID, failureCategory, time.Since(startedAt), telemetry.AgentCallMetrics{
		TotalCostUSD: resp.Metrics.TotalCostUSD,
		InputTokens:  resp.Metrics.InputTokens,
		OutputTokens: resp.Metrics.OutputTokens,
	}, err)
	a.recordLearningReflectionJob(recordCtx, metadata, pid, cwd, resp, finishedAt)
	if err != nil {
		return resp, &CallError{Category: failureCategory, Err: err}
	}

	// ★ output-contract-2phase Task 6.2 (R1.3 / R2.1 / R2.2 / R2.3 / R2.5 / R5.2 / R5.3):
	// Phase 1 が成功完了した経路でのみ (= 上の err != nil 早期 return をすり抜けた経路)、
	// OutputContracts が 1 件以上指定されていれば runReportPhase を呼び、戻り値
	// []OutputContractResult を resp.OutputContractResults に詰めて return する (R2.2 / R2.5)。
	//
	// Phase 1 失敗時 (err != nil) は上の CallError 早期 return で既に帰っているため、ここに到達
	// しない。結果として「Phase 1 失敗 → Phase 2 未実行 / resp.OutputContractResults == nil」が
	// 構造的に成立する (R2.3)。
	//
	// OutputContracts 未指定 (nil / 空 slice) の経路では本ブロックを skip し、
	// resp.OutputContractResults を nil のまま保ち legacy path と完全に同一挙動を維持する
	// (R1.3 後方互換ガード)。
	//
	// runReportPhase は contract 単位の失敗を OutputContractResult.Warning に集約し、helper
	// 自体は error を返さない契約 (R5.2 / R5.3) のため、本 hook で workflow を abort しない。
	// 既存 field (resp.Content / resp.SessionID / resp.Metrics / resp.SystemInfo) は Phase 2
	// hook によって一切変更されない (R2.5 既存応答テキスト不変)。
	//
	// 渡す引数の根拠:
	//   - pid: 解決済み provider ID (ProviderOverride 反映後)。runReportPhase 内で
	//     a.providers[pid] を再 lookup し、Phase 2 Mode (native/fallback) の static mapping にも使う。
	//   - resp.SessionID: Phase 1 で確立した session ID。Phase 2 main attempt の
	//     ResumeSessionID として渡されて Phase 1 文脈を継続する (R3.1)。空文字なら provider 仕様で
	//     新規 session 同等に倒れる。
	//   - metadata: Phase 1 で記録済みの AgentMetadata。WorkflowID / RunID / StepID を継承し、
	//     runReportPhase 内で StepID に :phase2:<name> suffix を付け、AgentCallID は attempt ごとに
	//     新規発行される (R7.1 / R7.2)。
	if options != nil && len(options.OutputContracts) > 0 {
		resp.OutputContractResults = a.runReportPhase(ctx, cwd, pid, resp.SessionID, metadata, options.OutputContracts)
	}
	return resp, nil
}

// CallError は CallAgent の失敗時に返るラッパー。failure_category を上位層
// (workflow など) に伝播し、error 文字列のプレフィックスとして使えるようにする。
type CallError struct {
	Category string
	Err      error
}

func (e *CallError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	if e.Category == "" {
		return e.Err.Error()
	}
	return "[" + e.Category + "] " + e.Err.Error()
}

func (e *CallError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// CategoryFromError は error から failure_category を抽出する。CallError 以外は空文字列。
func CategoryFromError(err error) string {
	var ce *CallError
	if errors.As(err, &ce) {
		return ce.Category
	}
	return ""
}

func classifyFailure(ctx context.Context, resp provider.AgentResponse, callErr error) string {
	if callErr == nil && resp.Success {
		return ""
	}
	// ErrUnknownProvider は CallAgent 本体が早期に検出してラップする経路が主だが、
	// provider 実装が将来 sentinel を返した場合にも統一カテゴリで分類できるよう先頭で分岐する
	// (review-workflow design.md §agent / provider 拡張)。
	if errors.Is(callErr, ErrUnknownProvider) {
		return FailureCategoryProviderUnknown
	}
	if errors.Is(callErr, streamidle.ErrStreamIdleTimeout) {
		return FailureCategoryStreamIdleTimeout
	}
	if errors.Is(callErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return FailureCategoryPartTimeout
	}
	if errors.Is(callErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return FailureCategoryExternalAbort
	}
	if callErr != nil {
		return FailureCategoryProviderError
	}
	// resp.Success=false, err=nil: provider 側がアプリケーション失敗を返したケース
	return FailureCategoryProviderError
}

func newAgentCallID() string {
	return fmt.Sprintf("agent-call-%d", time.Now().UnixNano())
}

func (a *Agent) recordAgentCallStart(ctx context.Context, providerID provider.ProviderID, prompt string, cwd string, resumeSessionID string, outputFormat *provider.OutputFormat, metadata provider.AgentMetadata, startedAt time.Time) {
	outputFormatType := ""
	if outputFormat != nil {
		outputFormatType = outputFormat.Type
	}
	_ = a.recorder.RecordAgentCallStart(ctx, akashic.AgentCall{
		ID:               metadata.AgentCallID,
		WorkflowRunID:    metadata.WorkflowRunID,
		WorkflowID:       metadata.WorkflowID,
		WorkflowName:     metadata.WorkflowName,
		StepID:           metadata.StepID,
		StepName:         metadata.StepName,
		ProviderID:       string(providerID),
		Prompt:           prompt,
		CWD:              cwd,
		ProjectID:        akashic.ProjectIDFromCWD(cwd),
		ResumeSessionID:  resumeSessionID,
		OutputFormatType: outputFormatType,
		StartedAt:        startedAt,
	})
}

func (a *Agent) recordRawEvent(ctx context.Context, metadata provider.AgentMetadata, event provider.AgentEvent) {
	_ = a.recorder.RecordRawEvent(ctx, akashic.RawEvent{
		AgentCallID:      metadata.AgentCallID,
		WorkflowRunID:    metadata.WorkflowRunID,
		Sequence:         event.Sequence,
		Source:           event.Source,
		Type:             event.ObservationType,
		Subtype:          event.ObservationSubtype,
		ObservedAt:       time.Now().UTC(),
		PayloadJSON:      event.Payload,
		PayloadTruncated: event.PayloadTruncated,
	})
}

func (a *Agent) recordLearningReflectionJob(ctx context.Context, metadata provider.AgentMetadata, providerID provider.ProviderID, cwd string, resp provider.AgentResponse, queuedAt time.Time) {
	policy, ok := workflowctx.EarlyReflectionPolicyFromContext(ctx)
	if !ok {
		return
	}
	recorder, ok := a.recorder.(akashic.LearningReflectionJobRecorder)
	if !ok {
		return
	}

	job := akashic.LearningReflectionJob{
		AgentCallID:   metadata.AgentCallID,
		WorkflowRunID: metadata.WorkflowRunID,
		WorkflowID:    metadata.WorkflowID,
		WorkflowName:  metadata.WorkflowName,
		StepID:        metadata.StepID,
		StepName:      metadata.StepName,
		ProviderID:    string(providerID),
		CWD:           cwd,
		ProjectID:     akashic.ProjectIDFromCWD(cwd),
		SessionID:     resp.SessionID,
		Status:        akashic.LearningReflectionStatusPending,
		QueuedAt:      queuedAt,
		UpdatedAt:     queuedAt,
	}
	if resp.SessionID == "" {
		job.Status = akashic.LearningReflectionStatusSkipped
		job.SkipReason = akashic.LearningReflectionSkipEmptySessionID
		job.FinishedAt = &queuedAt
	} else if !policy.AllowsProvider(providerID) {
		job.Status = akashic.LearningReflectionStatusSkipped
		job.SkipReason = akashic.LearningReflectionSkipProviderNotInAllowlist
		job.FinishedAt = &queuedAt
	}

	if _, err := recorder.RecordLearningReflectionJob(ctx, job); err != nil {
		return
	}
	if policy.Notify != nil {
		go policy.Notify()
	}
}

func (a *Agent) recordAgentCallFinish(ctx context.Context, agentCallID string, resp provider.AgentResponse, callErr error, failureCategory string, duration time.Duration, finishedAt time.Time) {
	errorMessage := ""
	if callErr != nil {
		errorMessage = callErr.Error()
	} else if !resp.Success {
		errorMessage = resp.Content
	}
	_ = a.recorder.RecordAgentCallFinish(ctx, akashic.AgentCallResult{
		ID:                   agentCallID,
		SessionID:            resp.SessionID,
		Success:              resp.Success && callErr == nil,
		DurationMS:           duration.Milliseconds(),
		TotalCostUSD:         nullFloat(resp.Metrics.TotalCostUSD),
		InputTokens:          nullInt(resp.Metrics.InputTokens),
		OutputTokens:         nullInt(resp.Metrics.OutputTokens),
		Model:                responseModel(resp),
		ModelReasoningEffort: responseModelReasoningEffort(resp),
		ResultSummary:        resp.Content,
		Error:                errorMessage,
		FailureCategory:      failureCategory,
		FinishedAt:           finishedAt,
		ResultPayloadJSON:    marshalJSON(resp.Metrics.ResultPayload),
	})

	// ★ memory-distiller-trigger-policy task 4.3 (Req 3.1 / 3.3 / 3.4):
	// 既存 critical section 内で ctx atomic に Store された cancel origin を読み、
	// "user" のときに限り RecordUserCorrection を発火する。
	//   - origin の判定ロジックは持たない (passthrough)。bridge result event の
	//     payload を nodebridge launcher が ctx atomic に Store した値をそのまま分岐に使う。
	//   - origin と failure_category は直交 (classifyFailure は不変)。
	//   - error は best-effort で握りつぶす (既存 recorder error 取り扱いと同形)。
	//   - 重複呼び出しの冪等性は Recorder.RecordUserCorrection 側 contract
	//     (UPDATE SET 1) に委譲する。
	if bridge.CancelOriginFromContext(ctx) == bridge.CancelOriginUser {
		_ = a.recorder.RecordUserCorrection(ctx, agentCallID)
	}
}

func responseModel(resp provider.AgentResponse) string {
	if resp.SystemInfo == nil {
		return ""
	}
	return resp.SystemInfo.Model
}

func responseModelReasoningEffort(resp provider.AgentResponse) string {
	if resp.SystemInfo == nil {
		return ""
	}
	return resp.SystemInfo.ModelReasoningEffort
}

func nullFloat(value *float64) sql.NullFloat64 {
	if value == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *value, Valid: true}
}

func nullInt(value *int64) sql.NullInt64 {
	if value == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *value, Valid: true}
}

func marshalJSON(value any) []byte {
	if value == nil {
		return nil
	}
	body, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return body
}
