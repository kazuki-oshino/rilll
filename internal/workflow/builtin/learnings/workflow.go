// Package learnings の本 file は agent-learnings 4 phase pipeline の orchestration を
// 担う `Workflow(agent)` factory と内部 driver `runLearningsOrchestration` を提供する
// (tasks.md 8.1 / design.md §LearningsWorkflow / Req 2.1, 2.2, 2.3, 2.4, 2.5, 8.1, 8.5,
// 8.6, 9.1)。
//
// 役割:
//
//   - PreFetch → Reflector → Curator → Apply の 4 phase を直列駆動。
//   - 全 exit 経路で `<runDir>/apply-result.json` を defer 書き出し + artifact 登録 (Req 8.6)。
//   - phase error で残り phase を skip し verdict 確定 (Req 2.3)。
//   - StepID closed set 4 値 (`pre-fetch` / `reflector` / `curator` / `apply`) を使用 (Req 2.2)。
//   - pre-fetch / apply phase は `agent.CallAgent` を呼ばない (Req 2.4 / 2.5)。
//     これは phase 関数の引数に `AgentCaller` を渡さないことで物理的に保証される。
//   - `WorkflowDone` event を 1 度だけ emit (TUI Result panel 用)。
//
// 設計契約:
//
//   - **Manual mode 不在 (Req 1.1)**: 本 workflow は tail wrap 経路 (task 8.3) でのみ
//     起動される設計。`ParentRunIDFromContext(ctx)` が空文字なら verdict=applied-no-op
//     で副作用ゼロ early return (precondition 違反として扱う)。
//   - **Reader / Recorder 解決**: `workflow.ReaderFromAgent(agent)` /
//     `workflow.RecorderFromAgent(agent)` で agent 経路から取得。nil の場合は
//     `verdict=reader-unavailable` で early return。
//   - **artifact 登録**: 各 phase の output JSON を `workflow.RecordRunArtifact` で
//     ArtifactKindContext / ArtifactKindProposal / ArtifactKindResult に振り分けて登録。
//   - **defer 経路**: 全 exit 経路で apply-result.json を 1 度書き出す。Apply phase 経由
//     なら apply.go 自身が書き、defer 側は再書き出し (idempotent path 上書き) として
//     許容する。
//
// 関連 Requirement: 2.1 (4 phase orchestration) / 2.2 (StepID 4 値) / 2.3 (phase error
// で残り skip) / 2.4 (pre-fetch LLM 不使用) / 2.5 (apply LLM 不使用) / 8.1 (workflow
// orchestration) / 8.5 (verdict closed set) / 8.6 (apply-result.json artifact) / 9.1
// (reader-unavailable).
package learnings

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// learningsWorkflowID は agent-learnings tail workflow の ID (Req 1.1)。
// `rilll list` / TUI には Hidden=true で非表示だが、akashic 上の identifier として使う。
const learningsWorkflowID = "agent-learnings"

// learningsWorkflowLabel は TUI / akashic に表示される人間可読ラベル。
// 通常表示されないが debug 用 (TailDef factory で Label 不変)。
const learningsWorkflowLabel = "agent-learnings tail (post-run reflection)"

// stepIDPreFetch / stepIDApply は agent_calls.step_id closed set 4 値 (Req 2.2)。
// stepIDReflector / stepIDCurator は reflector.go / curator.go で既に定義済み。
const (
	stepIDPreFetch = "pre-fetch"
	stepIDApply    = "apply"
)

// Workflow は agent-learnings tail workflow の Definition factory (tasks.md 8.1)。
//
// 戻り値の Definition は通常 `LearningsTailDef` (task 8.2) 経由で `Hidden=true` 化され、
// `SequentialWithLearningsTail` (task 8.3) で base workflow 後段に挿し込まれる。
// 本 factory は `LearningsTailDef` から `Workflow(agent).Fn` を流用する形で参照される。
func Workflow(agent workflowctx.AgentCaller) workflow.Definition {
	return workflow.Definition{
		ID:       learningsWorkflowID,
		Label:    learningsWorkflowLabel,
		Args:     nil, // manual mode 不在のため arg なし (Req 1.1)
		Recorder: workflow.RecorderFromAgent(agent),
		Hidden:   true, // tail mode 専用 (`rilll list` / TUI 非表示)
		Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			return runLearningsOrchestration(ctx, cwd, onEvent, ask, agent)
		},
	}
}

// runLearningsOrchestration は `Workflow.Fn` 本体実装 (task 8.1)。
//
// 4 phase pipeline を駆動し、各 phase の artifact を `RecordRunArtifact` 登録する。
// 全 exit 経路で `apply-result.json` を defer 経由で書き出す不変式を保つ (Req 8.6)。
func runLearningsOrchestration(
	ctx context.Context,
	cwd string,
	onEvent func(workflow.WorkflowEvent),
	_ workflow.WorkflowAsk, // ask は本 workflow では未使用 (manual mode 不在 / Req 1.1)
	agent workflowctx.AgentCaller,
) error {
	onEventSafe := func(event workflow.WorkflowEvent) {
		if onEvent != nil {
			onEvent(event)
		}
	}

	// runID / runDir 解決 (defer の apply-result.json 書き出しで必要)。
	runID := workflow.RunIDFromContext(ctx)
	runDir := filepath.Join(cwd, ".rilll", learningsWorkflowID, runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return fmt.Errorf("learnings: mkdir runDir %q: %w", runDir, err)
	}

	// parentRunID 解決 (tail wrap 経路で必須 / Req 1.1)。
	parentRunID := ParentRunIDFromContext(ctx)

	// ApplyResult を mutable state として保持し、全 exit 経路で defer 書き出し。
	result := ApplyResult{
		Verdict:          "",
		Appended:         []AppendedEntry{},
		Rejected:         []RejectedOperation{},
		Deduped:          []DedupSkippedOperation{},
		NewTopicsCreated: []string{},
		StartedAt:        nowRFC3339(),
	}

	defer func() {
		if result.FinishedAt == "" {
			result.FinishedAt = nowRFC3339()
		}
		// verdict が立っていない経路 (途中 panic 等) は applied-no-op を fallback に。
		if result.Verdict == "" {
			result.Verdict = VerdictAppliedNoOp
		}
		applyResultPath := filepath.Join(runDir, applyResultFilename)
		_ = writeJSONAtomic(applyResultPath, result)
		_ = workflow.RecordRunArtifact(ctx, akashic.ArtifactKindResult, applyResultPath)

		// TUI / CLI の最終 Result パネル向けに WorkflowDone を 1 回 emit する。
		onEventSafe(workflow.WorkflowEvent{
			Type:   workflow.WorkflowDone,
			Result: buildLearningsResultSummary(result),
		})
	}()

	// precondition: parent_run_id が ctx に注入されていない場合は副作用ゼロ early return。
	// 本 workflow は tail wrap 経路でのみ起動される (Req 1.1)。
	if parentRunID == "" {
		result.Verdict = VerdictAppliedNoOp
		onEventSafe(workflow.WorkflowEvent{
			Type: workflow.WorkflowText,
			Text: "agent-learnings: parent_run_id 未注入 — tail wrap 経路ではない可能性 (skip)",
		})
		return nil
	}

	// ---- Phase 1: pre-fetch ----
	reader := workflow.ReaderFromAgent(agent)
	preFetchInput := PreFetchInput{
		CWD:         cwd,
		ParentRunID: parentRunID,
	}
	preFetchResult, err := PreFetch(ctx, runDir, preFetchInput, reader)
	if err != nil {
		if errors.Is(err, ErrReaderUnavailable) {
			result.Verdict = VerdictReaderUnavailable
			onEventSafe(workflow.WorkflowEvent{
				Type: workflow.WorkflowText,
				Text: "agent-learnings: akashic reader unavailable — pipeline skip",
			})
			return nil
		}
		// PreFetch 失敗 (reader 以外の filesystem 等) は applied-no-op に降格 + error 伝播。
		result.Verdict = VerdictAppliedNoOp
		return fmt.Errorf("learnings: pre-fetch: %w", err)
	}
	if preFetchResult.SessionsJSONPath != "" {
		_ = workflow.RecordRunArtifact(ctx, akashic.ArtifactKindContext, preFetchResult.SessionsJSONPath)
	}

	// agent_calls 0 件 / 全 row 空 session_id → applied-no-op (Req 3.3)。
	if len(preFetchResult.UniqueSessionIDs) == 0 {
		result.Verdict = VerdictAppliedNoOp
		onEventSafe(workflow.WorkflowEvent{
			Type: workflow.WorkflowText,
			Text: "agent-learnings: 0 session_id — pipeline skip",
		})
		return nil
	}

	// ---- Phase 2: reflector ----
	recorder := workflow.RecorderFromAgent(agent)
	limiter := NewLimiter(envRillLearningsMaxParallel, defaultMaxParallel)
	usedEarlyAggregation := false
	if EarlyAggregationEnabledFromContext(ctx) {
		if store, ok := recorder.(akashic.LearningReflectionJobStore); ok && store != nil {
			aggregationResult, err := AggregateEarlyReflection(ctx, EarlyAggregationInput{
				CWD:         cwd,
				ParentRunID: parentRunID,
				TailRunID:   runID,
				RunDir:      runDir,
				WaitBudget:  resolveEarlyReflectionWaitBudget(),
			}, reader, store)
			if err != nil {
				result.Verdict = VerdictAppliedNoOp
				return fmt.Errorf("learnings: early aggregation: %w", err)
			}
			if aggregationResult.ReflectorJSONPath != "" {
				_ = workflow.RecordRunArtifact(ctx, akashic.ArtifactKindContext, aggregationResult.ReflectorJSONPath)
			}
			if aggregationResult.LearningsJSONPath != "" {
				_ = workflow.RecordRunArtifact(ctx, akashic.ArtifactKindContext, aggregationResult.LearningsJSONPath)
			}
			usedEarlyAggregation = true
		}
	}

	if !usedEarlyAggregation {
		// orchestration 層が parent workflow_run を Reader 経由で引いて workflow_id を解決する
		// 設計だが、本 MVP では空文字を許容する (learnings.workflow_id 列は optional)。
		// Reader.Detail が利用可能なら追加で引くことも可能 (将来拡張)。
		reflectorInput := ReflectorInput{
			CWD:         cwd,
			ParentRunID: parentRunID,
			WorkflowID:  resolveParentWorkflowID(ctx, reader, parentRunID),
			Sessions:    preFetchResult.Sessions,
		}
		reflectorResult, err := Reflector(ctx, runDir, reflectorInput, agent, recorder, limiter)
		if err != nil {
			// 致命的失敗 (artifact 書き出し等) → applied-no-op + error 伝播。
			result.Verdict = VerdictAppliedNoOp
			return fmt.Errorf("learnings: reflector: %w", err)
		}
		if reflectorResult.ReflectorJSONPath != "" {
			_ = workflow.RecordRunArtifact(ctx, akashic.ArtifactKindContext, reflectorResult.ReflectorJSONPath)
		}
		// learnings.json artifact 登録 (curator phase 入力契約 / Req 8.7)。
		if reflectorResult.LearningsJSONPath != "" {
			_ = workflow.RecordRunArtifact(ctx, akashic.ArtifactKindContext, reflectorResult.LearningsJSONPath)
		}

		// 全 session 失敗 → resume-all-failed (Req 4.6)。
		if reflectorResult.AllFailed && reflectorResult.PerSessionErrors > 0 {
			result.Verdict = VerdictResumeAllFailed
			onEventSafe(workflow.WorkflowEvent{
				Type: workflow.WorkflowText,
				Text: "agent-learnings: 全 session resume call 失敗 — pipeline skip",
			})
			return nil
		}
		// credential 不在 → applied-no-op (Req 9.2)。
		if reflectorResult.NoCredentials {
			result.Verdict = VerdictAppliedNoOp
			onEventSafe(workflow.WorkflowEvent{
				Type: workflow.WorkflowText,
				Text: "agent-learnings: provider credential 不在 — applied-no-op",
			})
			return nil
		}
	}

	// ---- Phase 3: curator ----
	// reflector 0 accept でも curator は呼ぶ設計 (既存 playbook の novelty 判定で
	// 既に蓄積された knowledge を活用するシナリオを想定するため)。
	// curator skill は filesystem 上の `<runDir>/learnings.json` (Reflector が書き出した
	// payload artifact / Req 8.7) と `<runDir>/existing-rules.txt` (既存 playbook concat)
	// の 2 種を Read tool で読む両端契約。本 input は akashic 直読みの余地を持たない。
	curatorInput := CuratorInput{
		CWD:         cwd,
		ParentRunID: parentRunID,
	}
	curatorResult, err := Curator(ctx, runDir, curatorInput, agent)
	if err != nil {
		result.Verdict = VerdictAppliedNoOp
		return fmt.Errorf("learnings: curator: %w", err)
	}
	if curatorResult.CuratorJSONPath != "" {
		_ = workflow.RecordRunArtifact(ctx, akashic.ArtifactKindProposal, curatorResult.CuratorJSONPath)
	}

	// curator が早期確定 verdict を返したらそのまま採用して apply 不発火。
	if curatorResult.Verdict != "" {
		result.Verdict = curatorResult.Verdict
		onEventSafe(workflow.WorkflowEvent{
			Type: workflow.WorkflowText,
			Text: fmt.Sprintf("agent-learnings: curator verdict=%s — apply skip", curatorResult.Verdict),
		})
		return nil
	}
	if err := ctx.Err(); err != nil {
		result.Verdict = VerdictAppliedNoOp
		onEventSafe(workflow.WorkflowEvent{
			Type: workflow.WorkflowText,
			Text: "agent-learnings: context canceled before Apply — apply skip",
		})
		return err
	}

	// ---- Phase 4: apply ----
	applyInput := ApplyInput{
		CWD:         cwd,
		ParentRunID: parentRunID,
		Operations:  curatorResult.Operations,
	}
	_, applyResult, err := Apply(ctx, runDir, applyInput)
	if err != nil {
		// apply 内致命的失敗 (scaffold / file write 等)。途中状態の result を採用 + error 伝播。
		result = applyResult
		if result.Verdict == "" {
			result.Verdict = VerdictAppliedNoOp
		}
		return fmt.Errorf("learnings: apply: %w", err)
	}
	// 成功 → applyResult を最終 result として採用 (defer 経路で再書き出し)。
	result = applyResult
	return nil
}

// resolveParentWorkflowID は parent workflow_run の workflow_id を Reader.Detail 経由で
// 引く best-effort helper。失敗時は空文字を返す (learnings.workflow_id 列は optional)。
//
// MVP では `Reader.Detail(ctx, runID)` の存在を前提としない。reader が nil ならば空文字。
// 将来 `Reader` interface に Detail が必須化されたら本 helper の実装を差し替える。
func resolveParentWorkflowID(_ context.Context, _ akashic.Reader, _ string) string {
	// MVP: 空文字で返す (learnings.workflow_id 列は optional)。
	// 将来拡張: reader.Detail(ctx, parentRunID).WorkflowID を返す。
	return ""
}

// buildLearningsResultSummary は agent-learnings workflow の最終 verdict + 統計を
// TUI の Result パネル向け 1 ブロック文字列に整形する (v1 distiller buildResultSummary
// と同形)。
//
// 不変式: 戻り値は決して空文字を返さない。
func buildLearningsResultSummary(r ApplyResult) string {
	verdict := r.Verdict
	if verdict == "" {
		verdict = "(verdict 未設定)"
	}
	header := fmt.Sprintf("agent-learnings (%s)", verdict)

	var lines []string
	switch r.Verdict {
	case VerdictApplied:
		lines = append(lines, fmt.Sprintf("追記 %d 件 / dedup %d 件 / reject %d 件",
			len(r.Appended), len(r.Deduped), len(r.Rejected)))
		if len(r.NewTopicsCreated) > 0 {
			lines = append(lines, fmt.Sprintf("新規 topic: %v", r.NewTopicsCreated))
		}
	case VerdictAppliedNoOp:
		lines = append(lines, "操作なし (curator 提案 0 件 / 全 dedup / precondition 違反等)")
	case VerdictReaderUnavailable:
		lines = append(lines, "akashic reader 未注入 — 副作用ゼロで早期終了")
	case VerdictResumeAllFailed:
		lines = append(lines, "全 session resume call 失敗 — Apply skip")
	case VerdictRejectedInvalidJSON:
		lines = append(lines, "Curator JSON 抽出失敗 / parse 失敗 — Apply skip")
	case VerdictRejectedInvalidCategory,
		VerdictRejectedInvalidTopicName,
		VerdictRejectedEmptyPathsGlob,
		VerdictRejectedUnboundedPathsGlob:
		lines = append(lines, fmt.Sprintf("全 op が %s で reject", r.Verdict))
	default:
		lines = append(lines, fmt.Sprintf("verdict=%q (詳細は apply-result.json を参照)", r.Verdict))
	}
	return header + "\n" + joinLines(lines)
}

// joinLines は lines を改行区切りで結合する (strings.Join(lines, "\n") 等価)。
// 標準 strings.Join と等価だが import 数を抑えるため小ヘルパとして閉じる。
func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	out := lines[0]
	for _, l := range lines[1:] {
		out += "\n" + l
	}
	return out
}
