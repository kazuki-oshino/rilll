// Package learnings の本 file は task 8.3 / design.md §Tail Mode Activation /
// Req 1.2, 1.3 で要求される `SequentialWithLearningsTail` Sequential 合成 Definition
// を提供する (v1 distiller の `SequentialWithDistillerTail` と同形再実装)。
//
// 役割:
//
//   - `SequentialWithLearningsTail(id, label, agent, base)` で base Definition と
//     `LearningsTailDef(agent)` を 2 段固定で連結した Sequential-equivalent な
//     `workflow.Definition` を返す。
//   - 内部 Fn は base Definition の Fn → TailDef の Fn の 2 段固定パイプラインを駆動し、
//     前段 base の `WorkflowEvent.RunID` から最初の non-empty 値を `parentRunID` として固定して
//     `ContextWithParentRunID(ctx, parentRunID)` で後段 TailDef の Fn 呼び出し前に注入する。
//   - 親 onEvent への event 転送、前段 error 検知 (戻り値 / WorkflowError event)、
//     `WorkflowActive` emit を `workflow.Sequential` の挙動とパリティで再現する。
//   - Label に `learningsTailLabelMarker` (` 📚`) を suffix して TUI 上で wrap 後の
//     workflow を視覚的に識別可能にする (v1 distiller の ` 🧠` と衝突しない値)。
//
// 設計契約 (両端):
//
//   - `WorkflowEvent.RunID` は `Definition.Run` の wrappedOnEvent が「空のときのみ自身の
//     runID を埋める」契約 (types.go)。本 file の frontWrapped は最初に non-empty な
//     RunID を観測した時点で `parentRunID` として固定する (= 前段 base.Run の runID)。
//   - 後段 LearningsTailDef の Fn (= `runLearningsOrchestration`) は ctx 経由で
//     parent_run_id を取得する (`ParentRunIDFromContext`)。本 file は ctx 注入のみを
//     担当し TailDef を変更しない。
//   - `parentRunID == ""` のときは `ContextWithParentRunID` の注入を skip し、後段の
//     orchestration 内 precondition 違反経路 (verdict=applied-no-op early return) に委ねる。
//
// 関連 Requirement: 1.2 (`DistillLearningsAfter=true` で base def を `<base-id>-with-learnings`
// として wrap 露出 / parent_run_id ctx hand-off) / 1.3 (Hidden=true tail Definition の
// SequentialWithLearningsTail 経由起動)。
package learnings

import (
	"context"
	"errors"

	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// learningsTailLabelMarker は Sequential 合成後の Label に suffix する marker。
// TUI 上で wrap 後 workflow を視覚的に識別するために用いる (Req 1.2 視覚区別)。
// v1 distiller の ` 🧠` (memory-distiller) と衝突しない値 ` 📚` を採用 (design.md L186)。
const learningsTailLabelMarker = " 📚"

// sequentialWithLearningsTailDeps は SequentialWithLearningsTail.Fn の orchestration が
// 利用する子 Definition の DI 構造体 (v1 distiller `sequentialWithDistillerTailDeps`
// と同形)。production 経路では `defaultSequentialWithLearningsTailDeps(agent, base)` が
// base Definition と `LearningsTailDef(agent)` を直接埋める。テスト経路では stub
// Definition を渡して parity / runID hand-off 不変式を機械検証する。
type workflowScopedEarlyReflectorRuntime interface {
	Notify()
	Shutdown()
}

type startWorkflowScopedEarlyReflectorRuntimeFunc func(
	ctx context.Context,
	cwd string,
	agent workflowctx.AgentCaller,
	config EarlyReflectorRuntimeConfig,
) (workflowScopedEarlyReflectorRuntime, bool, error)

type sequentialWithLearningsTailDeps struct {
	agent             workflowctx.AgentCaller
	frontDef          workflow.Definition
	tailDef           workflow.Definition
	startEarlyRuntime startWorkflowScopedEarlyReflectorRuntimeFunc
}

func defaultStartWorkflowScopedEarlyReflectorRuntime(
	ctx context.Context,
	cwd string,
	agent workflowctx.AgentCaller,
	config EarlyReflectorRuntimeConfig,
) (workflowScopedEarlyReflectorRuntime, bool, error) {
	return StartWorkflowScopedEarlyReflectorRuntime(ctx, cwd, agent, config)
}

// defaultSequentialWithLearningsTailDeps は production 経路で利用する deps 既定値を構築する。
func defaultSequentialWithLearningsTailDeps(agent workflowctx.AgentCaller, base workflow.Definition) sequentialWithLearningsTailDeps {
	return sequentialWithLearningsTailDeps{
		agent:             agent,
		frontDef:          base,
		tailDef:           LearningsTailDef(agent),
		startEarlyRuntime: defaultStartWorkflowScopedEarlyReflectorRuntime,
	}
}

// SequentialWithLearningsTail は base Definition と `LearningsTailDef(agent)` を
// 2 段固定で連結した Sequential 合成 Definition を返す
// (tasks.md 8.3 / design.md §Tail Mode Activation / Req 1.2, 1.3)。
//
// Definition のフィールド契約:
//   - ID: 関数引数 `id` で指定 (registry 側で `<base-id>-with-learnings` として渡される)
//   - Label: `<base.Label> + learningsTailLabelMarker` (TUI 視覚識別)
//   - Args: `base.Args` を直接コピー (= 元の base Definition と同一の引数で起動可能)
//   - Hidden: `base.Hidden` を継承
//   - Recorder: `workflow.RecorderFromAgent(agent)`
//
// agent==nil でも Definition 構築は成功する (`LearningsTailDef(nil)` が nil-safe に
// Definition を返すため)。
func SequentialWithLearningsTail(id string, agent workflowctx.AgentCaller, base workflow.Definition) workflow.Definition {
	deps := defaultSequentialWithLearningsTailDeps(agent, base)
	return workflow.Definition{
		ID:       id,
		Label:    base.Label + learningsTailLabelMarker,
		Args:     base.Args,
		Hidden:   base.Hidden,
		Recorder: workflow.RecorderFromAgent(agent),
		Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			return runSequentialWithLearningsTailOrchestration(ctx, cwd, onEvent, ask, deps)
		},
	}
}

// runSequentialWithLearningsTailOrchestration は SequentialWithLearningsTail.Fn の本体実装。
// deps 経由で child Definition を受け取り、テスト経路で stub を差し替え可能とする
// (v1 distiller `runSequentialWithDistillerTailOrchestration` と同形)。
//
// 処理順序 (厳守):
//
//  1. 前段 base.Definition.Run を起動する。
//     - frontWrapped は `WorkflowEvent.RunID` を傍受し、最初に non-empty な値を
//     `parentRunID` に固定する。
//     - 同時に `WorkflowError` event の Message を `firstFrontError` に保持する (Sequential parity)。
//     - `WorkflowActive` event を 1 件、base.Run 開始前に親 onEvent に流す (parity)。
//  2. 前段が non-nil error を返したらそのまま return する。
//  3. 戻り値が nil でも `firstFrontError != ""` なら `errors.New(firstFrontError)` を返し
//     後段未起動 (Sequential parity)。
//  4. ctx に `ContextWithParentRunID(ctx, parentRunID)` で前段 runID を注入する。
//     `parentRunID == ""` のときは注入 skip (後段 orchestration の precondition 違反経路に委ねる)。
//  5. 後段 tailDef.Definition.Run を起動する。
//     - tailWrapped は `WorkflowError` を `firstTailError` に保持する。
//     - `WorkflowActive` event を 1 件、tail.Run 開始前に親 onEvent に流す。
//  6. 後段の error / WorkflowError を Sequential parity で外向きに伝播する。
//
// 副作用契約: 本 orchestration は `def.Run(...)` を 2 回呼ぶのみで、それ以外の副作用
// (filesystem / git / LLM) を発火しない。
func runSequentialWithLearningsTailOrchestration(
	ctx context.Context,
	cwd string,
	onEvent func(workflow.WorkflowEvent),
	ask workflow.WorkflowAsk,
	deps sequentialWithLearningsTailDeps,
) error {
	emit := func(event workflow.WorkflowEvent) {
		if onEvent != nil {
			onEvent(event)
		}
	}

	frontCtx := ctx
	earlyAggregationEnabled := false
	if deps.startEarlyRuntime != nil {
		runtime, started, err := deps.startEarlyRuntime(ctx, cwd, deps.agent, EarlyReflectorRuntimeConfig{})
		if err != nil {
			return err
		}
		if started && runtime != nil {
			defer runtime.Shutdown()
			frontCtx = workflowctx.ContextWithEarlyReflectionEnabled(ctx, newEarlyReflectionPolicySnapshot(runtime.Notify))
			earlyAggregationEnabled = true
		}
	}

	// (1) 前段 base.Definition.Run。
	emit(workflow.WorkflowEvent{Type: workflow.WorkflowActive, Label: deps.frontDef.Label})

	var (
		parentRunID     string
		firstFrontError string
	)
	frontWrapped := func(event workflow.WorkflowEvent) {
		if parentRunID == "" && event.RunID != "" {
			parentRunID = event.RunID
		}
		if event.Type == workflow.WorkflowError && firstFrontError == "" {
			firstFrontError = event.Message
		}
		if onEvent != nil {
			onEvent(event)
		}
	}

	// (2) 前段 Run() の戻り値: non-nil なら即時伝播 (後段未起動)。
	if err := deps.frontDef.Run(frontCtx, cwd, frontWrapped, ask); err != nil {
		return err
	}

	// (3) 戻り値が nil でも WorkflowError event を観測していたら error として伝播。
	if firstFrontError != "" {
		return errors.New(firstFrontError)
	}

	// (4) 後段 ctx に parentRunID を注入する。
	tailCtx := ctx
	if parentRunID != "" {
		tailCtx = ContextWithParentRunID(ctx, parentRunID)
	}
	if earlyAggregationEnabled {
		tailCtx = ContextWithEarlyAggregationEnabled(tailCtx)
	}

	// (5) 後段 tailDef.Definition.Run。
	emit(workflow.WorkflowEvent{Type: workflow.WorkflowActive, Label: deps.tailDef.Label})

	var firstTailError string
	tailWrapped := func(event workflow.WorkflowEvent) {
		if event.Type == workflow.WorkflowError && firstTailError == "" {
			firstTailError = event.Message
		}
		if onEvent != nil {
			onEvent(event)
		}
	}

	// (6) 後段 Run() 戻り値 / WorkflowError event を Sequential parity で外向きに伝播。
	if err := deps.tailDef.Run(tailCtx, cwd, tailWrapped, ask); err != nil {
		return err
	}
	if firstTailError != "" {
		return errors.New(firstTailError)
	}
	return nil
}

// ApplyLearningsTailWraps は registry seam (task 9) で使う in-place wrap 関数。
// `defs[]` の中で `cfg.Workflows[def.ID].DistillLearningsAfter == true` の base def を
// `SequentialWithLearningsTail(<id>-with-learnings, agent, base)` で in-place 置換する。
//
// 不明 base ID は warning ログのみで registry 起動を停止しない (defensive)。
//
// 戻り値: in-place 修正された defs (caller は同 slice を使い続けて良い)。
//
// 設計契約:
//
//   - `<base-id>-with-learnings` の suffix は v1 distiller の `<base-id>-with-distill` と
//     衝突しない値 (Req 1.2 視覚区別)。
//   - cfg からの DistillLearningsAfter 判定は呼び出し側 (registry.go) で行う設計とし、
//     本関数は wrap 対象 ID list (`baseIDsToWrap`) を引数で受け取る純関数として閉じる。
//   - registryWarnLog 等の logger は引数として受け取らず、呼び出し側で warning を
//     emit する (本関数は in-place 修正のみで side-effect を持たない)。
//
// 用途は task 9 (registry seam) で具現化する。本 file では factory + helper のみを
// export し、registry.go 側で `ApplyLearningsTailWraps(defs, agent, baseIDs)` を呼ぶ。
func ApplyLearningsTailWraps(defs []workflow.Definition, agent workflowctx.AgentCaller, baseIDsToWrap map[string]struct{}) []workflow.Definition {
	if len(baseIDsToWrap) == 0 {
		return defs
	}
	for i := range defs {
		if _, ok := baseIDsToWrap[defs[i].ID]; !ok {
			continue
		}
		// in-place 置換: 同じ ID を維持し、Args/Hidden は base 継承、Label に 📚 marker。
		// (v1 distiller の applyDistillerTailWraps と同形: 旧 `<id>-with-distill` 別 ID 方式は採らない)
		defs[i] = SequentialWithLearningsTail(defs[i].ID, agent, defs[i])
	}
	return defs
}
