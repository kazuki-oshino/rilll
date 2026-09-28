// Package review の本ファイルは task 2.4 / design.md §review.Executor を実装する。
//
// 役割: precheck で確定した rule 列を並行に fan-out 起動し、1 rule の失敗を他 rule に
// 伝播させずに `<rule-name>.json` artifact + `result.RuleResult` を生成する
// (Req 4.1, 4.2, 4.3, 4.4, 4.5, 5.5, 5.6, 6.1, 6.4, 6.5)。
//
// 実装方針 (design.md §review.Executor Implementation Notes):
//
//   - 同時実行数上限は `chan struct{}` semaphore で制御する。default は
//     `defaultMaxParallel = 4`、`MaxParallel<=0` のとき `len(rules)` (= 上限なし) として扱う。
//   - 受付順序は rule slice 順 (loader が保証するファイル名昇順)。
//   - `errgroup.WithContext` は使わない: errgroup は 1 つの error で全 goroutine を
//     cancel する仕様だが、本 spec は「1 rule の失敗を他 rule に伝播させない」(Req 4.2)
//     ため、素の `sync.WaitGroup` + 個別 ctx 制御に統一する。
//   - 結果は固定 index で `Results []RuleResult` に書き込む。各 goroutine は自分の
//     index のみに書き込むため mutex 不要 (design.md §review.Executor State Management)。
//   - 中断 (Req 4.5): `<-ctx.Done()` を sema 取得 select に含めて in-progress rule を
//     即時 cancel + `Verdict=cancelled` で record。完了済 rule の RuleResult は破棄しない。
//   - 失敗分類:
//     a) panic recover で `Verdict=failed` + `FailureCategory="executor_panic"` に丸める
//     b) `errors.Is(err, agent.ErrUnknownProvider)` を `provider_unknown` に分類
//     c) それ以外の `*agent.CallError` は `Category` を転記
//     d) parse 失敗 (`ParseRuleResponse`) は `response_unparseable` を維持
//     e) ctx 起因 (context.Canceled / DeadlineExceeded) は `Verdict=cancelled` に丸める
//   - akashic 集計: rule.Name + provider + model を `rule_model_selected` raw_event として
//     emit し、失敗時は `executor_rule_failed` raw_event を emit する (Subtype=rule.Name で
//     agent_calls.step_id と join 可能)。書き出し / record 失敗は best-effort で握りつぶす。
//
// 依存方向 (design.md §Allowed Dependencies):
//
//	provider → agent → workflowctx → workflow → workflow/builtin/review
//
// 本ファイルは review-workflow パッケージの terminal node であり、
// `internal/agent` / `internal/workflow` 以外のレビュー固有 leaf (rules / result / gh) のみ
// を import する。
package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kazuki-oshino/rilll/internal/agent"
	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/gh"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/result"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/rules"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// defaultMaxParallel は ExecutorInput.MaxParallel が未指定 (>0 でない) かつ
// 「上限なし扱い」(`len(rules)` フォールバック) を希望しない呼び出し側で利用される
// per-call 既定上限。本 spec の MVP は 4 (rule 数 ~= provider 数 + α、design.md §Performance)。
//
// runExecutor 自体は MaxParallel<=0 のとき `len(rules)` を使う設計のため、本定数は
// 直接の callsite (review.Workflow Definition の Fn 内など、上位層) が「明示的に default を
// 採用する」ことを示すための公開定数。
const defaultMaxParallel = 4

// FailureCategoryExecutorPanic は executor goroutine 内で発生した panic を
// recover() で捕捉した際の `RuleResult.FailureCategory` 値 (design.md §Monitoring)。
//
// 既存 agent 層の `FailureCategoryProviderError` 等とは別の axis として、executor 固有の
// 異常終端を分類する。akashic `agent_calls.failure_category` には agent 層の値が記録され、
// 本値は per-rule artifact / result.json および raw_events `executor_rule_failed` 経由でのみ観測される。
const FailureCategoryExecutorPanic = "executor_panic"

// RawEventTypeRuleModelSelected は rule の provider/model 選択を akashic に記録する
// raw_event の Type 値 (design.md §Monitoring / Req 5.5 後半)。Subtype に rule.Name を入れ、
// agent_calls.step_id と join 可能にする。
const RawEventTypeRuleModelSelected = "rule_model_selected"

// RawEventTypeExecutorRuleFailed は rule 単位失敗を akashic に集約記録する
// raw_event の Type 値 (design.md §Monitoring)。Subtype に rule.Name を入れる。
const RawEventTypeExecutorRuleFailed = "executor_rule_failed"

// ExecutorInput は runExecutor への入力 (design.md §review.Executor Service Interface)。
//
// 設計判断:
//   - `Rules` は dedup 済み + ファイル名昇順を loader が保証する前提 (precheck 通過後)。
//   - `MaxParallel<=0` のとき `len(rules)` (= 上限なし) として扱う (Req 4.3 default 値)。
//   - `Agent` は `workflowctx.AgentCaller` interface 限定。recorder は agent が
//     `workflowctx.AkashicRecorderProvider` を実装している場合のみ raw_event を発行する経路で利用する。
//   - `RunDir` は `<cwd>/.rilll/review-workflow/<runID>/` の absolute clean path (workflow.go が事前に MkdirAll 済)。
type ExecutorInput struct {
	// Rules は対象 rule 列。dedup 済 + ファイル名昇順 (loader 契約)。
	Rules []rules.Rule

	// PRMeta は precheck で取得済の PR メタデータ (全 rule に共有される)。
	PRMeta gh.PRMeta

	// PRDiff は precheck で取得済の unified diff 文字列 (全 rule に共有される)。
	PRDiff string

	// MaxParallel は同時実行数上限 (Req 4.3)。<=0 のとき `len(rules)` を採用 (上限なし)。
	MaxParallel int

	// RunDir は per-rule artifact (`<rule-name>.json`) を書き出す絶対 clean path。
	RunDir string

	// Agent は per-rule の agent 呼び出しに利用する AgentCaller。
	Agent workflowctx.AgentCaller
}

// ExecutorOutput は runExecutor の戻り値 (design.md §review.Executor Service Interface)。
//
// `Results` の長さは `len(in.Rules)` と一致し、各要素の Verdict は確定済
// (`skipped` / `findings_reported` / `no_findings` / `failed` / `cancelled` のいずれか)。
// runExecutor 自体は error を返さず rule 単位失敗は `Verdict=failed` に集約するため、
// 本構造体に error フィールドは持たない (design.md §review.Executor Postconditions)。
type ExecutorOutput struct {
	// Results は per-rule 結果。rules slice と同 index 順 (= ファイル名昇順)。
	Results []result.RuleResult
}

// runExecutor は rule 列を並行に fan-out 起動し、各 rule の RuleResult と
// `<rule-name>.json` artifact を生成する (design.md §review.Executor)。
//
// 本関数は error を返さない: rule 単位の失敗はすべて `Results[i].Verdict=failed` または
// `cancelled` に集約され、呼び出し側 (workflow.go orchestration) は `Results` を
// `OverallVerdict` ヘルパで集約する。
//
// onEvent は agent 出力 (text / tool_use) を上位 (TUI / CLI) にストリーム転送する callback。
// nil 可 (CLI 単独経路 / テスト経路の defensive 経路)。
func runExecutor(ctx context.Context, in ExecutorInput, onEvent func(workflow.WorkflowEvent)) ExecutorOutput {
	n := len(in.Rules)
	out := ExecutorOutput{Results: make([]result.RuleResult, n)}
	if n == 0 {
		return out
	}

	maxP := in.MaxParallel
	if maxP <= 0 {
		maxP = n
	}
	if maxP > n {
		maxP = n
	}

	// recorder は agent から取り出す。AkashicRecorderProvider 未実装 / nil のとき raw_event は best-effort で no-op。
	recorder := workflow.RecorderFromAgent(in.Agent)

	sema := make(chan struct{}, maxP)
	var wg sync.WaitGroup

	for i := range in.Rules {
		i := i
		r := in.Rules[i]
		wg.Add(1)
		go func() {
			defer wg.Done()

			// sema 取得 (cancel 検知付き)。
			// ctx 中断時に sema が満杯のまま waiting している rule は in-flight 化させずに即 cancelled とする。
			select {
			case sema <- struct{}{}:
				defer func() { <-sema }()
			case <-ctx.Done():
				out.Results[i] = cancelledBeforeStart(ctx, r, in.RunDir)
				return
			}

			// sema 取得直後にも cancel チェック (race window 縮小)。
			if ctx.Err() != nil {
				out.Results[i] = cancelledBeforeStart(ctx, r, in.RunDir)
				return
			}

			out.Results[i] = runOneRule(ctx, r, in, recorder, onEvent)
		}()
	}
	wg.Wait()
	return out
}

// runOneRule は単一 rule を実行し、RuleResult を生成する。panic recover を含む全終端経路で
// `<rule-name>.json` artifact を best-effort で書き出す。
//
// 終端経路 (design.md §Per-rule 実行フロー):
//
//   - cancelled: ctx.Err() != nil または agent から context.Canceled が返ったとき
//   - failed (provider_unknown): `agent.ErrUnknownProvider` 経由
//   - failed (provider_error 等): その他の `*agent.CallError`
//   - failed (response_unparseable): ParseRuleResponse 失敗
//   - failed (executor_panic): goroutine 内の panic を recover で捕捉
//   - skipped: applies_when 自己判定で false
//   - no_findings: review 実行後 findings 0 件
//   - findings_reported: review 実行後 findings 1 件以上
func runOneRule(ctx context.Context, r rules.Rule, in ExecutorInput, recorder akashic.Recorder, onEvent func(workflow.WorkflowEvent)) (res result.RuleResult) {
	startedAt := time.Now().UTC()
	res = result.RuleResult{
		Name:        r.Name,
		Description: r.Description,
		Provider:    r.Provider,
		Model:       r.Model,
		SourceFile:  r.SourceFile,
		StartedAt:   startedAt.Format(time.RFC3339Nano),
	}

	// panic recover (Req 4.2): executor 内部の panic を Verdict=failed + executor_panic に丸める。
	// recover() は defer の中で呼ぶ必要があるため、関数末尾の named return `res` を更新する形で実装する。
	defer func() {
		if rec := recover(); rec != nil {
			res = makeFailedResult(r, startedAt, FailureCategoryExecutorPanic, fmt.Sprintf("executor panic: %v", rec))
			emitExecutorRuleFailedRawEvent(ctx, recorder, r, FailureCategoryExecutorPanic, res.Summary)
			_ = writeRuleArtifact(ctx, in.RunDir, &res)
		}
	}()

	// PR コンテキストを連結した 1-call combined prompt を構築 (review.Prompt / task 2.1)。
	promptStr := BuildRulePrompt(r, in.PRMeta, in.PRDiff)

	// CallAgentOptions: per-call provider/model 上書き seam (task 1.3 で追加)。
	// JSONSchemaForReview を OutputFormat に渡すことで claude / codex 経路の構造化出力を強制する。
	//
	// OutputContracts (task 7.1 / output-contract-2phase spec PoC 配線):
	//   - Phase 1 の json_schema 経路 (OutputFormat=JSONSchemaForReview) は変更しない。
	//   - Phase 1 成功後に同一呼び出し内で Phase 2 (resume session + AllowedTools=['Write'] +
	//     review-gather facet 埋め込み) が走り、`<RunDir>/<rule>.md` を書き出す (R9.1, R9.2)。
	//   - Phase 2 失敗時も RuleResult.Verdict には影響を与えない: 既存 writeRuleArtifact による
	//     `<rule>.json` 出力と verdict 決定ロジックは無変更で、Phase 2 は付加的な人間可読 report と
	//     して `<RunDir>/<rule>.md` に共存する (R9.3 / design.md §workflow/builtin/review Layer)。
	//   - FormatFacet="review-gather" は internal/facet/facets/output-contracts/review-gather.md を
	//     embed 経由で解決する (Phase 2 は task 7.2 の fake provider テストで初めて exercise される)。
	callOpts := &workflowctx.CallAgentOptions{
		OutputFormat:     JSONSchemaForReview,
		StepID:           r.Name,
		StepName:         "review rule " + r.Name,
		ProviderOverride: provider.ProviderID(r.Provider),
		ModelOverride:    r.Model,
		OutputContracts: []workflowctx.OutputContract{
			{Name: r.Name, FormatFacet: "review-gather"},
		},
	}

	// CallAgent の cwd は workflow run の CWD (RunMetadata 経由)。RunDir ではなく project root を渡す。
	callCWD := callAgentCWD(ctx, in.RunDir)

	resp, callErr := in.Agent.CallAgent(ctx, promptStr, callCWD, workflow.ForwardEvents(onEvent), "", callOpts)

	// rule_model_selected raw_event を best-effort で発行 (Req 5.5 後半)。
	// agent_calls の StepID=rule.Name で join 可能 (design.md §Monitoring)。
	emitRuleModelSelectedRawEvent(ctx, recorder, r)

	if callErr != nil {
		// ctx 起因の中断は cancelled に丸める (Req 4.5)。
		if isCtxCancelError(ctx, callErr) {
			res = makeCancelledResult(r, startedAt, "ctx cancelled during agent call")
			_ = writeRuleArtifact(ctx, in.RunDir, &res)
			return res
		}
		// provider 未登録は専用カテゴリ (Req 5.6)。errors.Is で sentinel 検出する構造化分岐。
		if errors.Is(callErr, agent.ErrUnknownProvider) {
			res = makeFailedResult(r, startedAt, agent.FailureCategoryProviderUnknown,
				fmt.Sprintf("provider %q is not registered: %v", r.Provider, callErr))
			emitExecutorRuleFailedRawEvent(ctx, recorder, r, res.FailureCategory, res.Summary)
			_ = writeRuleArtifact(ctx, in.RunDir, &res)
			return res
		}
		// それ以外の CallError は Category を転記。CallError 以外の error は provider_error にフォールバック。
		category := agent.FailureCategoryProviderError
		var ce *agent.CallError
		if errors.As(callErr, &ce) && ce.Category != "" {
			category = ce.Category
		}
		res = makeFailedResult(r, startedAt, category, callErr.Error())
		emitExecutorRuleFailedRawEvent(ctx, recorder, r, res.FailureCategory, res.Summary)
		_ = writeRuleArtifact(ctx, in.RunDir, &res)
		return res
	}

	// 応答パース (task 2.2)。失敗時は parsedRule.Verdict=failed + FailureCategory=response_unparseable が返る。
	parsed, parseErr := ParseRuleResponse(resp)
	if parseErr != nil || parsed.Verdict == result.VerdictFailed {
		summary := parsed.Summary
		if summary == "" {
			summary = "agent 応答の解析に失敗しました"
			if parseErr != nil {
				summary = parseErr.Error()
			}
		}
		res = makeFailedResult(r, startedAt, FailureCategoryResponseUnparseable, summary)
		emitExecutorRuleFailedRawEvent(ctx, recorder, r, res.FailureCategory, res.Summary)
		_ = writeRuleArtifact(ctx, in.RunDir, &res)
		return res
	}

	// 成功経路: skipped / no_findings / findings_reported のいずれか。
	res.Applied = parsed.Applied
	res.Verdict = parsed.Verdict
	res.SkipReason = parsed.SkipReason
	res.FindingsPresent = parsed.FindingsPresent
	res.Summary = parsed.Summary
	if len(parsed.Findings) > 0 {
		res.Findings = parsed.Findings
	}
	res.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	_ = writeRuleArtifact(ctx, in.RunDir, &res)
	return res
}

// cancelledBeforeStart は rule 起動前に ctx が cancel された場合の終端処理を集約する。
//
// `<rule-name>.json` は best-effort で書き出し、Verdict=cancelled の RuleResult を返す。
// `executor_rule_failed` は emit しない (failure ではなく cancel の判別を保つため)。
func cancelledBeforeStart(ctx context.Context, r rules.Rule, runDir string) result.RuleResult {
	res := makeCancelledResult(r, time.Now().UTC(), "ctx cancelled before rule started")
	_ = writeRuleArtifact(ctx, runDir, &res)
	return res
}

// makeCancelledResult は ctx 中断時の RuleResult を組み立てる (Req 4.5)。
//
// `Applied` / `FindingsPresent` は zero-value (false) のまま残す (review 未実行のため)。
// `FinishedAt` は呼び出し時点の wall clock を入れる (cancel 検知時刻として観測可能にする)。
func makeCancelledResult(r rules.Rule, startedAt time.Time, summary string) result.RuleResult {
	return result.RuleResult{
		Name:        r.Name,
		Description: r.Description,
		Verdict:     result.VerdictCancelled,
		Summary:     summary,
		Provider:    r.Provider,
		Model:       r.Model,
		SourceFile:  r.SourceFile,
		StartedAt:   startedAt.Format(time.RFC3339Nano),
		FinishedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}
}

// makeFailedResult は rule 単位失敗時の RuleResult を組み立てる (Req 4.4 / 5.6)。
//
// `Applied` / `FindingsPresent` は zero-value (false) のまま残す。
// `FailureCategory` は呼び出し側が確定した分類 (provider_unknown / provider_error /
// response_unparseable / executor_panic 等) を転記する。
func makeFailedResult(r rules.Rule, startedAt time.Time, category, summary string) result.RuleResult {
	return result.RuleResult{
		Name:            r.Name,
		Description:     r.Description,
		Verdict:         result.VerdictFailed,
		FailureCategory: category,
		Summary:         summary,
		Provider:        r.Provider,
		Model:           r.Model,
		SourceFile:      r.SourceFile,
		StartedAt:       startedAt.Format(time.RFC3339Nano),
		FinishedAt:      time.Now().UTC().Format(time.RFC3339Nano),
	}
}

// writeRuleArtifact は per-rule 結果 (`<rule-name>.json`) を runDir 配下に書き出し、
// `RecordRunArtifact(review, ...)` を best-effort で登録する (Req 6.1, 6.4)。
//
// 失敗は呼び出し側に伝播させない (best-effort)。書き出し失敗は raw_event
// `artifact_record_failed` warning として `RecordRunArtifact` ヘルパが集約する経路に委ねる。
//
// `rule.Name` は kebab-case + 予約名検証通過済 (loader 契約) のため、`result.json` /
// `context.json` との path 衝突は構造的に発生しない (design.md §review.Executor Validation)。
func writeRuleArtifact(ctx context.Context, runDir string, res *result.RuleResult) error {
	body, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(runDir, res.Name+".json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return err
	}
	// RecordRunArtifact の失敗は best-effort で握りつぶす (Req 2.6)。
	_ = workflow.RecordRunArtifact(ctx, akashic.ArtifactKindReview, path)
	return nil
}

// callAgentCWD は CallAgent に渡す cwd 値を確定する。
//
// 優先順位: ctx 上の RunMetadata.CWD → fallback として RunDir。RunMetadata が
// 不在のテスト経路 (Definition.Run を経由しない直接呼び出し) でも安全に動作する。
func callAgentCWD(ctx context.Context, runDir string) string {
	if metadata, ok := workflowctx.RunMetadataFromContext(ctx); ok && metadata.CWD != "" {
		return metadata.CWD
	}
	return runDir
}

// isCtxCancelError は CallAgent から返った error が ctx 起因の中断かを判定する (Req 4.5)。
//
// 検出条件:
//   - ctx.Err() != nil (ctx 自体が完了している)
//   - errors.Is(err, context.Canceled) または errors.Is(err, context.DeadlineExceeded)
//
// agent 層は ctx 起因 cancel を `FailureCategoryExternalAbort` に分類するが、
// review-workflow の verdict は `failed` ではなく `cancelled` を要求する (Req 4.5) ため、
// 本判定で executor 側で前段ガードする。
func isCtxCancelError(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return false
}

// emitRuleModelSelectedRawEvent は成功 / 失敗を問わず rule の provider/model 選択を
// raw_event として 1 件発行する (Req 5.5 後半 / design.md §Monitoring)。
//
// PayloadJSON: `{"provider": "<rule.Provider>", "model": "<rule.Model>"}`。
// Subtype: rule.Name (= agent_calls.step_id と join 可能)。
//
// recorder == nil (agent が AkashicRecorderProvider を未実装 / NoopRecorder 経路) のときは no-op。
// JSON marshal 失敗 / RecordRawEvent 失敗は best-effort で握りつぶす (failure of failure logging を許容)。
func emitRuleModelSelectedRawEvent(ctx context.Context, recorder akashic.Recorder, r rules.Rule) {
	if recorder == nil {
		return
	}
	payload, err := json.Marshal(map[string]string{
		"provider": r.Provider,
		"model":    r.Model,
	})
	if err != nil {
		payload = []byte("{}")
	}
	_ = recorder.RecordRawEvent(context.WithoutCancel(ctx), akashic.RawEvent{
		Type:        RawEventTypeRuleModelSelected,
		Subtype:     r.Name,
		ObservedAt:  time.Now().UTC(),
		PayloadJSON: payload,
	})
}

// executorRuleFailedPayload は executor_rule_failed raw_event の PayloadJSON 形。
// {category, error} の 2 フィールド固定 (akashic 上で SQL 検索しやすい最小スキーマ)。
type executorRuleFailedPayload struct {
	Category string `json:"category"`
	Error    string `json:"error"`
}

// emitExecutorRuleFailedRawEvent は rule 単位失敗を raw_event として 1 件発行する
// (Req 4.4 / design.md §Monitoring)。
//
// PayloadJSON: `{"category": "<failureCategory>", "error": "<summary>"}`。
// Subtype: rule.Name。recorder == nil / JSON marshal 失敗 / RecordRawEvent 失敗は best-effort。
func emitExecutorRuleFailedRawEvent(ctx context.Context, recorder akashic.Recorder, r rules.Rule, category, summary string) {
	if recorder == nil {
		return
	}
	payload, err := json.Marshal(executorRuleFailedPayload{
		Category: category,
		Error:    summary,
	})
	if err != nil {
		payload = []byte("{}")
	}
	_ = recorder.RecordRawEvent(context.WithoutCancel(ctx), akashic.RawEvent{
		Type:        RawEventTypeExecutorRuleFailed,
		Subtype:     r.Name,
		ObservedAt:  time.Now().UTC(),
		PayloadJSON: payload,
	})
}
