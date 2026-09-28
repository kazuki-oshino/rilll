// Package review の本ファイルは task 3.1 / design.md §review.Workflow を実装する。
//
// 役割: review-workflow の workflow.Definition を提供し、Fn 内で precheck → executor →
// 全体集約 (OverallVerdict) → result.json defer の順で副作用 1 本の制御フローを駆動する
// (Req 3.3, 3.4, 6.3, 6.4, 7.2, 7.3, 7.4)。
//
// 副作用契約 (design.md §review.Workflow Implementation Notes):
//
//   - precheck フェーズは executor / per-rule artifact 書き出しを呼ばない (Req 3.3)。
//   - 全 exit 経路 (defer) で result.json artifact 登録を試みる (Req 6.4)。
//   - precheck 失敗時は OverallVerdict=precheck_failed を直接埋め、Sequential 後段を止めるため
//     `[verdict:precheck_failed] <summary>` 形の error を返す (Req 7.4)。
//   - executor 結果集約は result.OverallVerdict() ヘルパで行う (Req 6.3 優先順位)。
//   - IsIntent (verdict が `findings_reported` または `no_findings`) のときのみ Fn は nil を返す。
//     `failed` / `cancelled` / `precheck_failed` 経路は Sequential 失敗伝播のため non-nil error を返す。
//
// improve-workflow との対応:
//
//   - improve.Workflow と同様に `xxxWorkflowDeps` DI 構造体 + `setVerdict` ヘルパ + `defer` で
//     result.json を書き出す orchestration パターンを踏襲する。
//   - IsIntent / setVerdict ヘルパは review-workflow ローカルに閉じる (improve-workflow 側の
//     `patch.Verdict.IsIntent()` を直接呼ばない: package boundary を保つため)。
package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/result"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/rules"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// reviewWorkflowID は本 workflow.Definition の workflow ID (kebab-case)。
// `workflow_runs.workflow_id` に直接格納される (Req 7.1)。
const reviewWorkflowID = "review-workflow"

// reviewWorkflowLabel は TUI トップ / `rilll list` で表示される人間可読ラベル。
const reviewWorkflowLabel = "レビュー実行"

// askPRDescription は Args[0] (`--pr`) のヘルプ表記。
// CLI: `rilll run review-workflow --pr <num>`、TUI: 引数入力プロンプトの説明文として表示される。
const askPRDescription = "対象 PR 番号"

// reviewResultJSONFile は `<runDir>/result.json` のファイル名。
// design.md §Logical Data Model の規約値で、後段 review-apply-workflow の入力としても消費される。
const reviewResultJSONFile = "result.json"

// reviewWorkflowDeps は review.Workflow.Fn 内部で利用する副作用コンポーネントを集約する
// DI 構造体 (improve-workflow と同形)。本番経路では defaultReviewWorkflowDeps が
// 既定実装を組み立て、テスト経路では runReviewWorkflowOrchestration を直接呼び fake を渡す。
//
// パッケージ private に閉じることで Definition.Fn シグネチャを変えずにテスト容易性を確保する。
type reviewWorkflowDeps struct {
	// gh は precheck.go が利用する gh CLI ラッパ seam (本番: defaultGhClient)。
	gh precheckGhClient

	// loadRules は precheck.go の rules.LoadRules 呼び出し seam (本番: rules.LoadRules)。
	loadRules precheckRulesLoader

	// runExec は executor.go の runExecutor 呼び出し seam (本番: runExecutor)。
	// テストでは fake で per-rule artifact 書き出しを再現し、result.json defer 経路を検証する。
	runExec func(ctx context.Context, in ExecutorInput, onEvent func(workflow.WorkflowEvent)) ExecutorOutput
}

// defaultReviewWorkflowDeps は本番経路で利用する既定実装一式を構築する。
// 本関数は Workflow から呼ばれる唯一の場所で、テスト経路では呼ばれない。
func defaultReviewWorkflowDeps() reviewWorkflowDeps {
	return reviewWorkflowDeps{
		gh:        defaultGhClient{},
		loadRules: rules.LoadRules,
		runExec:   runExecutor,
	}
}

// Workflow は review-workflow の workflow.Definition を返す
// (design.md §review.Workflow / tasks.md 3.1 / Req 7.1, 7.2)。
//
// Definition のフィールド契約:
//   - ID: "review-workflow" (Req 7.1)
//   - Label: "レビュー実行"
//   - Args: [{Key:"pr", Required:true, Description:"対象 PR 番号"}] (Req 2.4)
//     CLI 単独経路は `--pr <num>` をそのまま Values["pr"] に流し込む。TUI 経路は
//     argument prompt 経由で空文字列が入る場合があり、precheck.go 側で再度 ListPRs → ask.Select に分岐する。
//   - Hidden: false (rilll list / TUI トップに通常表示する標準 workflow / Req 7.1)
//   - Recorder: workflow.RecorderFromAgent(agent) で取得 (NoopRecorder のときは no-op 記録)。
//
// Fn 本体は runReviewWorkflowOrchestration を deps 経由で駆動する。
func Workflow(agent workflowctx.AgentCaller) workflow.Definition {
	deps := defaultReviewWorkflowDeps()
	return workflow.Definition{
		ID:    reviewWorkflowID,
		Label: reviewWorkflowLabel,
		Args: []workflow.ArgSpec{
			{Key: askPRKey, Description: askPRDescription, Required: true},
		},
		Hidden:   false,
		Recorder: workflow.RecorderFromAgent(agent),
		Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			return runReviewWorkflowOrchestration(ctx, cwd, onEvent, ask, agent, deps)
		},
	}
}

// runReviewWorkflowOrchestration は review.Workflow.Fn の本体実装。
// 副作用コンポーネントを deps 経由で受け取り、テストから fake で差し替えられる seam を提供する。
//
// 全 exit 経路で result.json artifact が登録される契約を defer で保証する (Req 6.4)。
// IsIntent (== findings_reported / no_findings) のとき Fn は nil を返し、
// それ以外の verdict (precheck_failed / failed / cancelled) は Sequential 後段を止めるため
// `[verdict:<id>] <summary>` 形の error を返す (Req 7.4)。
func runReviewWorkflowOrchestration(
	ctx context.Context,
	cwd string,
	onEvent func(workflow.WorkflowEvent),
	ask workflow.WorkflowAsk,
	agent workflowctx.AgentCaller,
	deps reviewWorkflowDeps,
) error {
	// runDir は precheck より前に確定させる (defer の result.json 書き出しで必要)。
	// RunIDFromContext は RunMetadata 不在のとき新規 UUID を返す契約のため、
	// CLI 単独 / テスト経路 (RunMetadata 注入無し) でも常に有効な runID が手に入る。
	startedAt := time.Now().UTC()
	runID := workflow.RunIDFromContext(ctx)
	runDir := filepath.Join(cwd, ".rilll", reviewWorkflowID, runID)
	// MkdirAll 失敗は best-effort で握りつぶす: defer の WriteFile で再度判明する。
	_ = os.MkdirAll(runDir, 0o755)

	// 集約 state (defer から参照される mutable 変数群)。
	var (
		finalVerdict     result.Verdict
		finalSummary     string
		prNumber         int
		precheckWarnings []string
		ruleResults      []result.RuleResult
	)

	// defer での result.json artifact 登録 (Req 6.4 記録欠落禁止)。
	// 全 exit 経路 (precheck 失敗 / executor 通過 / panic 経路) で確実に走らせるため冒頭で defer 登録。
	// ファイル書き出し / RecordRunArtifact の失敗は best-effort で握りつぶす (akashic-artifacts spec Req 2.6)。
	defer func() {
		// Verdict 未確定時は cancelled を default として埋める (panic / 想定外 return path 防御)。
		v := finalVerdict
		if v == result.VerdictUnset {
			v = result.VerdictCancelled
			if finalSummary == "" {
				finalSummary = "review-workflow が verdict 未確定で終了しました (default cancelled)"
			}
		}
		sum := result.Summary{
			WorkflowRunID:    runID,
			PRNumber:         prNumber,
			OverallVerdict:   v,
			OverallSummary:   finalSummary,
			Rules:            ruleResults,
			PrecheckWarnings: precheckWarnings,
			StartedAt:        startedAt.Format(time.RFC3339Nano),
			FinishedAt:       time.Now().UTC().Format(time.RFC3339Nano),
		}
		body, _ := json.MarshalIndent(sum, "", "  ")
		path := filepath.Join(runDir, reviewResultJSONFile)
		_ = os.WriteFile(path, body, 0o644)
		_ = workflow.RecordRunArtifact(ctx, akashic.ArtifactKindResult, path)
	}()

	// setVerdict は finalVerdict / finalSummary を確定し、IsIntent==true のとき nil、
	// それ以外で `[verdict:<id>] <summary>` 形の error を返す (Req 7.4 Sequential 失敗伝播)。
	// 全 exit 経路はこのヘルパを経由して return することで、Req 6.4 / 7.4 の整合を一箇所で担保する。
	setVerdict := func(v result.Verdict, summary string) error {
		finalVerdict = v
		finalSummary = summary
		if isIntentVerdict(v) {
			return nil
		}
		return errors.New(formatVerdictError(v, summary))
	}

	// Step 1: precheck (gh / rules / `--pr` / PR 存在検証) を実行する (Req 3.1, 3.2, 3.3)。
	pre, _ := runPrecheck(ctx, precheckInput{
		cwd:       cwd,
		ask:       ask,
		gh:        deps.gh,
		loadRules: deps.loadRules,
	})
	// LoadRules で集まった非致命的 Warnings は precheck 成功 / 失敗を問わず result.json に転記する (Req 6.3)。
	precheckWarnings = formatPrecheckWarnings(pre.Warnings)

	if pre.Verdict == result.VerdictPrecheckFailed {
		// precheck 失敗: per-rule executor を起動せずに precheck_failed verdict を確定する (Req 3.3)。
		return setVerdict(result.VerdictPrecheckFailed, pre.Summary)
	}

	// Step 2: precheck で確定した PR 番号 + メタを集約 state に反映する。
	prNumber = pre.PRNumber

	// Step 3: executor を起動して per-rule 並行実行 → per-rule artifact 書き出しを行う (Req 4.x, 6.1)。
	// executor 自体は error を返さない契約 (rule 単位失敗は Verdict=failed に集約される) のため、
	// 戻り値は ExecutorOutput のみ受け取る。
	execOut := deps.runExec(ctx, ExecutorInput{
		Rules:       pre.Rules,
		PRMeta:      pre.PRMeta,
		PRDiff:      pre.PRDiff,
		MaxParallel: defaultMaxParallel,
		RunDir:      runDir,
		Agent:       agent,
	}, onEvent)
	ruleResults = execOut.Results

	// Step 4: per-rule verdict のスライスから OverallVerdict を導出する (Req 6.3 優先順位)。
	verdicts := make([]result.Verdict, 0, len(execOut.Results))
	for _, r := range execOut.Results {
		verdicts = append(verdicts, r.Verdict)
	}
	overall := result.OverallVerdict(verdicts)

	// Step 5: OverallVerdict + 簡易 summary を確定する。Verdict 別の人間可読 summary は
	// agent 出力ではなく orchestration 側が組み立てる (Req 6.3 OverallSummary 1 行原則)。
	summary := buildOverallSummary(overall, execOut.Results)
	return setVerdict(overall, summary)
}

// isIntentVerdict は Verdict が「workflow が意図通り完走した」状態かを判定する。
//
// review-workflow のルール:
//   - findings_reported / no_findings → IsIntent=true (Sequential 後段を継続させるため Fn は nil)
//   - precheck_failed / failed / cancelled / skipped / unset → IsIntent=false (non-nil error を返す)
//
// improve-workflow の `patch.Verdict.IsIntent()` と同 semantic だが、本パッケージは
// review/result パッケージの Verdict を消費するため、ここでローカルに判定する
// (review/result/types.go に IsIntent を export しないことで両端契約の汚染を避ける)。
func isIntentVerdict(v result.Verdict) bool {
	switch v {
	case result.VerdictFindingsReported, result.VerdictNoFindings:
		return true
	default:
		return false
	}
}

// formatVerdictError は `[verdict:<id>] <summary>` 形の error メッセージを組み立てる。
//
// Sequential 経路では本 prefix を見て後段を起動しないことが既存の慣習契約で、
// improve-workflow の `patch.ErrorPrefixForVerdict` と同形を踏襲する。
func formatVerdictError(v result.Verdict, summary string) string {
	if summary == "" {
		return fmt.Sprintf("[verdict:%s]", v)
	}
	return fmt.Sprintf("[verdict:%s] %s", v, summary)
}

// formatPrecheckWarnings は rules.Warning スライスを result.Summary.PrecheckWarnings の
// `[]string` 形に整形する (Req 6.3)。
//
// 1 件あたり `<file>: <reason>` 形式で、人間可読 1 行に圧縮する。
// nil / 0 件のときは nil を返し、Summary の omitempty で result.json から省略される契約を維持する。
func formatPrecheckWarnings(warnings []rules.Warning) []string {
	if len(warnings) == 0 {
		return nil
	}
	out := make([]string, 0, len(warnings))
	for _, w := range warnings {
		out = append(out, fmt.Sprintf("%s: %s", w.File, w.Reason))
	}
	return out
}

// buildOverallSummary は OverallVerdict + per-rule 結果から人間可読 1 行 summary を組み立てる。
//
// 形式:
//   - findings_reported: "review 完了: N 件中 M 件で findings あり"
//   - no_findings: "review 完了: 全 N rule で findings なし"
//   - failed: "review 一部失敗: N 件中 M 件 failed"
//   - cancelled: "review 中断: N 件中 M 件 cancelled"
//   - その他 (unset / skipped) は "review 完了: verdict=<v>" で fallback。
func buildOverallSummary(overall result.Verdict, results []result.RuleResult) string {
	total := len(results)
	switch overall {
	case result.VerdictFindingsReported:
		findings := countByVerdict(results, result.VerdictFindingsReported)
		return fmt.Sprintf("review 完了: %d 件中 %d 件で findings あり", total, findings)
	case result.VerdictNoFindings:
		return fmt.Sprintf("review 完了: 全 %d rule で findings なし", total)
	case result.VerdictFailed:
		failed := countByVerdict(results, result.VerdictFailed)
		return fmt.Sprintf("review 一部失敗: %d 件中 %d 件 failed", total, failed)
	case result.VerdictCancelled:
		cancelled := countByVerdict(results, result.VerdictCancelled)
		return fmt.Sprintf("review 中断: %d 件中 %d 件 cancelled", total, cancelled)
	default:
		return fmt.Sprintf("review 完了: verdict=%s", overall)
	}
}

// countByVerdict は results 中の特定 verdict の件数を返す小ヘルパ。
func countByVerdict(results []result.RuleResult, target result.Verdict) int {
	n := 0
	for _, r := range results {
		if r.Verdict == target {
			n++
		}
	}
	return n
}
