// Package apply の本ファイルは task 6.1 / design.md §Sequential / Req 1.2, 8.3, 8.4, 8.5 で
// 要求される `review-and-apply` Sequential 合成 Definition を提供する。
//
// 役割:
//   - `ReviewAndApply(agent)` で `review-and-apply` ID の workflow.Definition を返す。
//     内部 Fn は `review.Workflow(agent)` → `apply.Workflow(agent)` の 2 段固定パイプラインを
//     駆動し、前段 review の `WorkflowRunID` を `WorkflowEvent.RunID` 経由で抽出して
//     `ContextWithReviewRunID(ctx, reviewRunID)` で後段 apply の Fn 呼び出し前に注入する。
//
// `workflow.Sequential` を **そのまま流用しない理由** (design.md §Architecture Decisions):
//   - 既存 `workflow.Sequential` には前段 child の runID を後段 ctx に渡す seam が無く、
//     `ContextWithReviewRunID` 注入を行う差し込みポイントが不在。本ファイルでは Sequential と
//     等価な orchestration を最小限に再実装し、その間に runID hand-off を 1 段挿入する。
//
// 責務限定 (design.md §Sequential §責務限定):
//   - 2 段固定 (review → apply) のチェーンに限定する。3 段以上 / event filter / 任意 handoff
//     汎化は本ファイルの責務外で、別 spec で `workflow.Sequential` 拡張に乗せる。
//   - 親 onEvent への event 転送、前段 error 検知 (戻り値 / WorkflowError event)、`WorkflowActive`
//     emit を `workflow.Sequential` の挙動とパリティで再現する (parity test / 5.6 invariant 同等)。
//
// 設計契約 (両端):
//   - `WorkflowEvent.RunID` は `Definition.Run` の wrappedOnEvent が「空のときのみ自身の runID を
//     埋める」契約。本ファイルの reviewWrapped は最初に non-empty な RunID を観測した時点で
//     `reviewRunID` として固定する (= 前段 review.Run の runID)。空 RunID イベントは無視する。
//   - 後段 apply.Workflow の Fn は ctx 第一優先 / `--review-run-id` arg 第二優先で reviewRunID を
//     解決する (task 5.1 で実装済)。本ファイルでは ctx 注入のみを担当し apply.Workflow を変更しない。
package apply

import (
	"context"
	"errors"

	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// reviewAndApplyID は本 workflow.Definition の workflow ID (kebab-case)。
// `workflow_runs.workflow_id` に直接格納される (Req 8.3)。
const reviewAndApplyID = "review-and-apply"

// reviewAndApplyLabel は TUI / `rilll list` で表示される人間可読ラベル。
const reviewAndApplyLabel = "レビュー実行と結果適用"

// reviewAndApplyDeps は ReviewAndApply.Fn の orchestration が利用する子 Definition の
// DI 構造体。production 経路では `defaultReviewAndApplyDeps(agent)` が `review.Workflow(agent)` /
// `Workflow(agent)` を直接埋める。テスト経路では stub Definition を渡して parity / runID hand-off
// 不変式を機械検証する (sequential_test.go)。
//
// 構造的 SRP:
//   - 本 deps は「2 段の child Definition」のみを抱える。agent / recorder 等の cross-cutting な
//     dep は親側 (`ReviewAndApply` 関数本体) が握り、各 child Definition の構築時に注入する。
type reviewAndApplyDeps struct {
	// reviewDef は前段 review-workflow の Definition。production では `review.Workflow(agent)`。
	reviewDef workflow.Definition

	// applyDef は後段 review-apply-workflow の Definition。production では `Workflow(agent)`。
	applyDef workflow.Definition
}

// defaultReviewAndApplyDeps は production 経路で利用する deps 既定値を構築する。
// agent を 1 度だけ各 child Definition factory に渡し、不要な複数回 factory 呼び出しを避ける。
//
// 注意: `review.Workflow(agent)` は内部で `defaultReviewWorkflowDeps()` を構築し、`Workflow(agent)`
// は `defaultApplyWorkflowDeps(agent)` を構築する。両者とも agent==nil 受け入れ可 (Definition 構築
// 自体は agent を直接呼ばないため)。Definition.Recorder は `RecorderFromAgent(agent)` 経由で
// nil-safe に解決される。
func defaultReviewAndApplyDeps(agent workflowctx.AgentCaller) reviewAndApplyDeps {
	return reviewAndApplyDeps{
		reviewDef: review.Workflow(agent),
		applyDef:  Workflow(agent),
	}
}

// ReviewAndApply は `review-and-apply` Sequential 合成 Definition を返す
// (design.md §Sequential / tasks.md 6.1 / Req 8.3, 8.4, 8.5)。
//
// Definition のフィールド契約:
//   - ID: "review-and-apply" (Req 8.3)
//   - Label: "レビュー実行と結果適用"
//   - Args: `review.Workflow(agent).Args` を直接コピー (= --pr 必須 / Req 8.3 / boundary 違反回避)
//   - Hidden: false (rilll list / TUI トップに通常表示)
//   - Recorder: `workflow.RecorderFromAgent(agent)` (NoopRecorder 経路では no-op 記録)
//
// Fn 本体は `runReviewAndApplyOrchestration` を deps 経由で駆動する。
// agent==nil でも Definition 構築は成功する (review.Workflow / Workflow が nil-safe に Definition を返す)。
func ReviewAndApply(agent workflowctx.AgentCaller) workflow.Definition {
	deps := defaultReviewAndApplyDeps(agent)
	return workflow.Definition{
		ID:    reviewAndApplyID,
		Label: reviewAndApplyLabel,
		// review.Workflow.Args (= [{Key:"pr", Required:true}]) を継承する契約 (design.md §Sequential)。
		// review-workflow 側が Args を変更しても自動的に追従する (両端契約 SSoT)。
		Args:     deps.reviewDef.Args,
		Hidden:   false,
		Recorder: workflow.RecorderFromAgent(agent),
		Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			return runReviewAndApplyOrchestration(ctx, cwd, onEvent, ask, deps)
		},
	}
}

// runReviewAndApplyOrchestration は ReviewAndApply.Fn の本体実装。deps 経由で child Definition
// を受け取り、テスト経路で stub を差し替え可能とする。
//
// 処理順序 (厳守):
//
//  1. 前段 review.Definition.Run を起動する。
//     - reviewWrapped は `WorkflowEvent.RunID` を傍受し、最初に non-empty な値を `reviewRunID` に固定する。
//     - 同時に `WorkflowError` event の Message を `firstReviewError` に保持する (Sequential parity)。
//     - `WorkflowActive` event を 1 件、review.Run 開始前に親 onEvent に流す (workflow.Sequential parity)。
//  2. 前段が non-nil error を返したらそのまま return する (`workflow.Sequential` と同等 / Req 8.4)。
//  3. 戻り値が nil でも `firstReviewError != ""` なら `errors.New(firstReviewError)` を返し後段未起動。
//  4. ctx に `ContextWithReviewRunID(ctx, reviewRunID)` で前段 runID を注入する
//     (`reviewRunID == ""` のときは注入をスキップする防御: 後段は arg fallback / precondition_failed
//     経路で自前ハンドリングする)。
//  5. 後段 apply.Definition.Run を起動する。
//     - applyWrapped は `WorkflowError` を `firstApplyError` に保持する (前段と同形)。
//     - `WorkflowActive` event を 1 件、apply.Run 開始前に親 onEvent に流す。
//  6. 後段の error / WorkflowError を Sequential parity で外向きに伝播する。
//
// 副作用契約:
//   - 本 orchestration は `def.Run(...)` を 2 回呼ぶのみで、それ以外の副作用 (filesystem / git / LLM)
//     を発火しない。各 child Definition 内の副作用契約は child の責務 (review-workflow / apply-workflow
//     spec) に閉じる。
func runReviewAndApplyOrchestration(
	ctx context.Context,
	cwd string,
	onEvent func(workflow.WorkflowEvent),
	ask workflow.WorkflowAsk,
	deps reviewAndApplyDeps,
) error {
	// onEvent==nil 防御 helper: CLI 単独 / テスト経路で nil で渡されることがあるため吸収する。
	emit := func(event workflow.WorkflowEvent) {
		if onEvent != nil {
			onEvent(event)
		}
	}

	// (1) 前段 review.Definition.Run。
	// reviewWrapped で event.RunID 抽出 + WorkflowError 検知を集約する。
	emit(workflow.WorkflowEvent{Type: workflow.WorkflowActive, Label: deps.reviewDef.Label})

	var (
		reviewRunID      string
		firstReviewError string
	)
	reviewWrapped := func(event workflow.WorkflowEvent) {
		// `Definition.Run` の wrappedOnEvent が event.RunID を埋めて流してくる契約 (types.go)。
		// 最初に non-empty な値を観測した時点で reviewRunID として固定する。
		// review.Workflow が複数回 (理論上は 1 回) Run() を呼ぶことは無いため、後段への hand-off は
		// 「最初の」runID を採用すれば十分。
		if reviewRunID == "" && event.RunID != "" {
			reviewRunID = event.RunID
		}
		// Sequential parity: WorkflowError の最初の Message を捕捉する。
		// (workflow.Sequential `runChildSequentially` と同等の挙動。)
		if event.Type == workflow.WorkflowError && firstReviewError == "" {
			firstReviewError = event.Message
		}
		// 親 onEvent へ転送 (順序保持: 前段 events は後段起動前に親に届く / parity (c))。
		if onEvent != nil {
			onEvent(event)
		}
	}

	// (2) 前段 Run() の戻り値: non-nil なら即時伝播 (後段未起動)。
	if err := deps.reviewDef.Run(ctx, cwd, reviewWrapped, ask); err != nil {
		return err
	}

	// (3) 戻り値が nil でも WorkflowError event を観測していたら error として伝播 (parity (b'))。
	if firstReviewError != "" {
		return errors.New(firstReviewError)
	}

	// (4) 後段 ctx に reviewRunID を注入する。
	// reviewRunID == "" の経路 (前段が一度も event を emit しなかった理論上のケース) では
	// 注入をスキップし、後段の Fn 内 fallback (arg / precondition_failed) に委ねる。
	applyCtx := ctx
	if reviewRunID != "" {
		applyCtx = ContextWithReviewRunID(ctx, reviewRunID)
	}

	// (5) 後段 apply.Definition.Run。
	emit(workflow.WorkflowEvent{Type: workflow.WorkflowActive, Label: deps.applyDef.Label})

	var firstApplyError string
	applyWrapped := func(event workflow.WorkflowEvent) {
		if event.Type == workflow.WorkflowError && firstApplyError == "" {
			firstApplyError = event.Message
		}
		if onEvent != nil {
			onEvent(event)
		}
	}

	// (6) 後段 Run() 戻り値 / WorkflowError event を Sequential parity で外向きに伝播する。
	if err := deps.applyDef.Run(applyCtx, cwd, applyWrapped, ask); err != nil {
		return err
	}
	if firstApplyError != "" {
		return errors.New(firstApplyError)
	}
	return nil
}
