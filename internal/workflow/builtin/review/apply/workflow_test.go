// Package apply のテスト本ファイルは Workflow Definition skeleton (task 5.1) の
// 不変式を検証する (design.md §Workflow / Req 1.2, 2.5, 7.3, 7.4, 8.1, 8.2)。
//
// 検証観点:
//   - TestApplyWorkflowDefinition: Definition の ID / ArgSpec / Hidden / Recorder などの
//     基本契約 (design.md §Workflow Service Interface / Req 8.1, 8.2)。
//   - TestReviewRunIDResolution: ctx 注入 (第一優先) / `--review-run-id` arg (第二優先) /
//     双方空 の 3 経路で reviewRunID 解決優先順位が期待通りであることを検証する
//     (design.md §Workflow §reviewRunID resolution / Req 1.2, 8.5)。
//   - TestResultJSONInvariant: 任意の早期 return (precheck 前 panic / ctx・arg 双方空 /
//     即時 cancel) で `result.json` が必ず書き出され、panic 経路で `final_verdict=failed`
//     が記録されることを検証する (design.md §Workflow §defer 不変式 / Req 7.3, 7.4)。
package apply

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	improvepatch "github.com/kazuki-oshino/rilll/internal/workflow/builtin/improve/patch"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/result"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// applyWorkflowFakeRecorder は workflow_test.go 専用の最小 akashic.Recorder 実装。
// RecordRunArtifact のみ蓄積し、それ以外のメソッドは no-op で nil を返す。
// result_test.go の resultFakeRecorder と同形だが、本ファイル独自で抱えることで
// 将来 result_test.go 側の fake 拡張による予期せぬ干渉を避ける。
type applyWorkflowFakeRecorder struct {
	artifacts []akashic.RunArtifact
}

func (r *applyWorkflowFakeRecorder) RecordWorkflowRunStart(context.Context, akashic.WorkflowRun) error {
	return nil
}
func (r *applyWorkflowFakeRecorder) RecordWorkflowRunFinish(context.Context, akashic.WorkflowRunFinish) error {
	return nil
}
func (r *applyWorkflowFakeRecorder) RecordAgentCallStart(context.Context, akashic.AgentCall) error {
	return nil
}
func (r *applyWorkflowFakeRecorder) RecordAgentCallFinish(context.Context, akashic.AgentCallResult) error {
	return nil
}
func (r *applyWorkflowFakeRecorder) RecordRawEvent(context.Context, akashic.RawEvent) error {
	return nil
}
func (r *applyWorkflowFakeRecorder) RecordRunArtifact(_ context.Context, a akashic.RunArtifact) error {
	r.artifacts = append(r.artifacts, a)
	return nil
}

// RecordUserCorrection は memory-distiller-trigger-policy spec Task 3.3 で追加した
// Recorder interface 拡張への追従 (no-op)。本 test は apply workflow の RunArtifact 経路
// 観測が責務で user-correction marker 経路は対象外のため履歴は保持しない。
func (r *applyWorkflowFakeRecorder) RecordUserCorrection(context.Context, string) error {
	return nil
}

// RecordLearning は agent-learnings spec (task 3.2) で Recorder interface に追加された
// メソッドへの no-op 追従。本 test は apply workflow の RunArtifact 経路観測が責務で
// learnings 経路は対象外のため履歴は保持しない。
func (r *applyWorkflowFakeRecorder) RecordLearning(context.Context, akashic.LearningRow) error {
	return nil
}

func (r *applyWorkflowFakeRecorder) Close() error { return nil }

// newApplyWorkflowCtx は <cwd> + RunMetadata + Recorder を ctx に注入した
// テスト用 (cwd, ctx, recorder) 三つ組を返す。
// runApplyWorkflowOrchestration はその内部で <cwd>/.rilll/review-apply-workflow/<runID>
// を MkdirAll するため、本 helper では runDir を作らない。
func newApplyWorkflowCtx(t *testing.T, runID string) (cwd string, ctx context.Context, recorder *applyWorkflowFakeRecorder) {
	t.Helper()
	cwd = t.TempDir()
	recorder = &applyWorkflowFakeRecorder{}
	ctx = context.Background()
	ctx = workflowctx.ContextWithRunMetadata(ctx, workflowctx.RunMetadata{
		WorkflowRunID: runID,
		WorkflowID:    "review-apply-workflow",
		WorkflowName:  "review-apply-workflow",
		CWD:           cwd,
	})
	ctx = workflow.ContextWithRecorder(ctx, recorder)
	return cwd, ctx, recorder
}

// readResultJSON は <cwd>/.rilll/review-apply-workflow/<runID>/result.json を読み出して
// ResultRecord にデコードする。テスト経路で defer の書き出しを観測するための共通 helper。
func readResultJSON(t *testing.T, cwd, runID string) ResultRecord {
	t.Helper()
	path := filepath.Join(cwd, ".rilll", "review-apply-workflow", runID, "result.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%s): %v", path, err)
	}
	var rec ResultRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatalf("json.Unmarshal(result.json): %v\n%s", err, string(body))
	}
	return rec
}

// TestApplyWorkflowDefinition は Workflow(agent) が返す Definition の基本契約を検証する。
// Req 8.1: ID = "review-apply-workflow"、Req 8.2: ArgSpec{Key:"review-run-id"} を含む。
// design.md §Workflow Service Interface: Sequential 経路では ctx 注入で満たされるため
// Required:false、Hidden:false で `rilll list` / TUI に通常表示される。
func TestApplyWorkflowDefinition(t *testing.T) {
	def := Workflow(nil)

	if def.ID != "review-apply-workflow" {
		t.Errorf("Definition.ID=%q, want %q", def.ID, "review-apply-workflow")
	}
	// Args に review-run-id が登録されていること。
	var found bool
	for _, a := range def.Args {
		if a.Key == "review-run-id" {
			found = true
			if a.Required {
				t.Errorf("ArgSpec[review-run-id].Required=true, want false (ctx 注入経路で満たされるため)")
			}
		}
	}
	if !found {
		t.Errorf("Definition.Args に review-run-id が無い: %+v", def.Args)
	}
	// Fn が non-nil であること (空 Definition で Run できないことを防ぐ)。
	if def.Fn == nil {
		t.Errorf("Definition.Fn が nil")
	}
}

// TestReviewRunIDResolutionFromContext は ctx 注入 (第一優先) を用いた経路で
// reviewRunID が ctx から解決され ResultRecord.ReviewRunID に転記されることを検証する。
// (design.md §Workflow §reviewRunID resolution / Req 1.2, 8.5)
func TestReviewRunIDResolutionFromContext(t *testing.T) {
	cwd, ctx, _ := newApplyWorkflowCtx(t, "ctx-run")
	ctx = ContextWithReviewRunID(ctx, "ctx-review-id")

	// coreFn が呼ばれた reviewRunID を観測する。
	var captured string
	deps := applyWorkflowDeps{
		coreFn: func(_ context.Context, _ string, _ string, _ string, reviewRunID string, _ func(workflow.WorkflowEvent), record *ResultRecord) error {
			captured = reviewRunID
			record.FinalVerdict = VerdictNoFindings
			record.FinalSummary = "test stub: ctx 解決経路を通過"
			return nil
		},
	}

	err := runApplyWorkflowOrchestration(ctx, cwd, nil, &workflow.CLIAsk{Values: map[string]string{"review-run-id": "arg-review-id"}}, deps)
	// VerdictNoFindings は intent のため Fn は nil。
	if err != nil {
		t.Fatalf("Fn returned error (intent でなく失敗): %v", err)
	}
	if captured != "ctx-review-id" {
		t.Errorf("coreFn 受領 reviewRunID=%q, want %q (ctx 第一優先)", captured, "ctx-review-id")
	}
	// result.json の review_run_id が ctx 注入値と一致すること。
	rec := readResultJSON(t, cwd, "ctx-run")
	if rec.ReviewRunID != "ctx-review-id" {
		t.Errorf("result.json.review_run_id=%q, want %q", rec.ReviewRunID, "ctx-review-id")
	}
	if rec.FinalVerdict != VerdictNoFindings {
		t.Errorf("result.json.final_verdict=%q, want %q", rec.FinalVerdict, VerdictNoFindings)
	}
}

// TestReviewRunIDResolutionFromArg は ctx 注入が無く `--review-run-id` arg のみが指定された
// 経路で reviewRunID が arg から解決されることを検証する (Req 8.2 Sequential 経路外)。
func TestReviewRunIDResolutionFromArg(t *testing.T) {
	cwd, ctx, _ := newApplyWorkflowCtx(t, "arg-run")

	var captured string
	deps := applyWorkflowDeps{
		coreFn: func(_ context.Context, _ string, _ string, _ string, reviewRunID string, _ func(workflow.WorkflowEvent), record *ResultRecord) error {
			captured = reviewRunID
			record.FinalVerdict = VerdictNoFindings
			record.FinalSummary = "test stub: arg 解決経路を通過"
			return nil
		},
	}

	err := runApplyWorkflowOrchestration(ctx, cwd, nil, &workflow.CLIAsk{Values: map[string]string{"review-run-id": "arg-review-id"}}, deps)
	if err != nil {
		t.Fatalf("Fn returned error: %v", err)
	}
	if captured != "arg-review-id" {
		t.Errorf("coreFn 受領 reviewRunID=%q, want %q (arg 解決経路)", captured, "arg-review-id")
	}
	rec := readResultJSON(t, cwd, "arg-run")
	if rec.ReviewRunID != "arg-review-id" {
		t.Errorf("result.json.review_run_id=%q, want %q", rec.ReviewRunID, "arg-review-id")
	}
}

// TestReviewRunIDResolutionBothEmpty は ctx 注入も arg も空のとき
// `VerdictPreconditionFailed` で副作用ゼロ終端することを検証する (Req 1.3, 2.5, 8.2)。
// 副作用ゼロ = coreFn が呼ばれないこと。
func TestReviewRunIDResolutionBothEmpty(t *testing.T) {
	cwd, ctx, _ := newApplyWorkflowCtx(t, "empty-run")

	var coreCalled bool
	deps := applyWorkflowDeps{
		coreFn: func(_ context.Context, _ string, _ string, _ string, _ string, _ func(workflow.WorkflowEvent), _ *ResultRecord) error {
			coreCalled = true
			return nil
		},
	}

	err := runApplyWorkflowOrchestration(ctx, cwd, nil, &workflow.CLIAsk{Values: map[string]string{}}, deps)
	if err == nil {
		t.Fatalf("Fn returned nil (precondition_failed は intent でないため non-nil error 期待)")
	}
	if !strings.HasPrefix(err.Error(), "[verdict:precondition_failed]") {
		t.Errorf("error prefix mismatch: got %q", err.Error())
	}
	if coreCalled {
		t.Errorf("coreFn が呼ばれた (precondition_failed 経路で副作用ゼロを期待)")
	}
	rec := readResultJSON(t, cwd, "empty-run")
	if rec.FinalVerdict != VerdictPreconditionFailed {
		t.Errorf("result.json.final_verdict=%q, want %q", rec.FinalVerdict, VerdictPreconditionFailed)
	}
}

// TestReviewRunIDResolutionContextWinsOverArg は ctx 注入と arg の双方が
// 指定された場合に ctx 第一優先で解決されることを検証する (Req 8.5)。
func TestReviewRunIDResolutionContextWinsOverArg(t *testing.T) {
	cwd, ctx, _ := newApplyWorkflowCtx(t, "ctx-wins-run")
	ctx = ContextWithReviewRunID(ctx, "ctx-review-id")

	var captured string
	deps := applyWorkflowDeps{
		coreFn: func(_ context.Context, _ string, _ string, _ string, reviewRunID string, _ func(workflow.WorkflowEvent), record *ResultRecord) error {
			captured = reviewRunID
			record.FinalVerdict = VerdictNoFindings
			record.FinalSummary = "ctx vs arg priority test"
			return nil
		},
	}

	if err := runApplyWorkflowOrchestration(ctx, cwd, nil, &workflow.CLIAsk{Values: map[string]string{"review-run-id": "arg-review-id"}}, deps); err != nil {
		t.Fatalf("Fn returned error: %v", err)
	}
	if captured != "ctx-review-id" {
		t.Errorf("ctx 第一優先違反: got %q, want %q", captured, "ctx-review-id")
	}
}

// TestResultJSONInvariantOnPanic は panic 経路でも `defer` の result.json 書き出しが
// 必ず実行され、未設定 verdict が `VerdictFailed` に fallback されることを検証する
// (design.md §Workflow §defer 不変式 / Req 7.3, 7.4)。
func TestResultJSONInvariantOnPanic(t *testing.T) {
	cwd, ctx, _ := newApplyWorkflowCtx(t, "panic-run")
	ctx = ContextWithReviewRunID(ctx, "panic-review-id")

	deps := applyWorkflowDeps{
		coreFn: func(_ context.Context, _ string, _ string, _ string, _ string, _ func(workflow.WorkflowEvent), _ *ResultRecord) error {
			panic("intentional panic in coreFn for invariant test")
		},
	}

	// panic を recover で握り、error として呼び出し元に返す。Fn 呼び出し自体は panic しない契約。
	err := runApplyWorkflowOrchestration(ctx, cwd, nil, &workflow.CLIAsk{}, deps)
	if err == nil {
		t.Fatalf("panic 経路で error が nil (recover 失敗 / panic 抑止失敗)")
	}
	// final_verdict=failed が記録されていること。
	rec := readResultJSON(t, cwd, "panic-run")
	if rec.FinalVerdict != VerdictFailed {
		t.Errorf("panic 経路 result.json.final_verdict=%q, want %q (Req 7.4 fallback 不変式)",
			rec.FinalVerdict, VerdictFailed)
	}
	if rec.ReviewRunID != "panic-review-id" {
		t.Errorf("panic 経路 result.json.review_run_id=%q, want %q (panic 前に解決済み reviewRunID が転記される)",
			rec.ReviewRunID, "panic-review-id")
	}
	// error prefix が [verdict:failed] であること。
	if !strings.HasPrefix(err.Error(), "[verdict:failed]") {
		t.Errorf("panic 経路 error prefix mismatch: got %q", err.Error())
	}
}

// TestResultJSONInvariantOnImmediateCancel は ctx が事前に cancel されている経路でも
// `defer` の result.json 書き出しが必ず実行されることを検証する (Req 7.3)。
// 即時 cancel 経路の verdict は skeleton では coreFn 呼び出し成否次第のため、本テストでは
// 「result.json が存在し、final_verdict が VerdictUnset (空文字) でないこと」のみを assert する
// (design.md §Verdict Invariants: VerdictUnset を最終 verdict として書き出してはならない)。
func TestResultJSONInvariantOnImmediateCancel(t *testing.T) {
	cwd, ctx, _ := newApplyWorkflowCtx(t, "cancel-run")
	ctx = ContextWithReviewRunID(ctx, "cancel-review-id")
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel() // 即座に cancel

	deps := applyWorkflowDeps{
		coreFn: func(ctx context.Context, _ string, _ string, _ string, _ string, _ func(workflow.WorkflowEvent), record *ResultRecord) error {
			// coreFn 側で ctx.Done() を観測したら VerdictFailed (skeleton: 5.5 で UserAborted/RevertFailed に振り分け)
			if ctx.Err() != nil {
				record.FinalVerdict = VerdictFailed
				record.FinalSummary = "ctx canceled before core work"
				return errors.New(ErrorPrefixForVerdict(VerdictFailed, record.FinalSummary))
			}
			record.FinalVerdict = VerdictNoFindings
			record.FinalSummary = "should not happen on canceled ctx"
			return nil
		},
	}

	_ = runApplyWorkflowOrchestration(cancelCtx, cwd, nil, &workflow.CLIAsk{}, deps)

	// result.json が存在すること。
	rec := readResultJSON(t, cwd, "cancel-run")
	if rec.FinalVerdict == VerdictUnset {
		t.Errorf("即時 cancel 経路で result.json.final_verdict が VerdictUnset (空文字) (Req 7.4 不変式違反)")
	}
}

// TestResultJSONInvariantUnsetFallback は coreFn が verdict を設定せずに帰った経路で
// defer が `VerdictFailed` に fallback して result.json を書き出すことを検証する
// (Req 7.4 / design.md §Verdict Invariants)。
func TestResultJSONInvariantUnsetFallback(t *testing.T) {
	cwd, ctx, _ := newApplyWorkflowCtx(t, "unset-run")
	ctx = ContextWithReviewRunID(ctx, "unset-review-id")

	deps := applyWorkflowDeps{
		coreFn: func(_ context.Context, _ string, _ string, _ string, _ string, _ func(workflow.WorkflowEvent), _ *ResultRecord) error {
			// verdict をセットせずに nil 返却 (実装ミスの想定経路)。
			return nil
		},
	}

	err := runApplyWorkflowOrchestration(ctx, cwd, nil, &workflow.CLIAsk{}, deps)
	if err == nil {
		t.Fatalf("verdict 未設定経路で error が nil (fallback 不変式違反)")
	}
	rec := readResultJSON(t, cwd, "unset-run")
	if rec.FinalVerdict != VerdictFailed {
		t.Errorf("verdict 未設定経路 result.json.final_verdict=%q, want %q",
			rec.FinalVerdict, VerdictFailed)
	}
}

// TestApplyWorkflowDefaultCoreFnIntegratesPrecheck は production 経路
// (`defaultApplyWorkflowDeps()` 由来 deps) において、5.2 で実装された coreFn が
// Precheck と接続されており、`<cwd>/.rilll/review-workflow/<reviewRunID>` 不在で
// `VerdictPreconditionFailed` を返すことを検証する。
//
// 5.1 段階では coreFn=nil で常に VerdictFailed を返していたが、5.2 で
// `newPrecheckLoaderFastPathCoreFn` が default 化され、precheck (dir 不在検知) が
// 即座に friendly verdict を確定するため、5.1 期と異なる verdict が観測される。
// 本テストは production wiring の回帰検出 anchor として残す (5.3+ で coreFn が
// propose loop に拡張されても、precheck 失敗経路の verdict は変わらない契約)。
func TestApplyWorkflowDefaultCoreFnIntegratesPrecheck(t *testing.T) {
	cwd, ctx, _ := newApplyWorkflowCtx(t, "default-run")
	ctx = ContextWithReviewRunID(ctx, "default-review-id")
	// `<cwd>/.rilll/review-workflow/default-review-id/` を作らないため、
	// Precheck は「review run dir 不在」で VerdictPreconditionFailed を返す。

	deps := defaultApplyWorkflowDeps(nil)
	err := runApplyWorkflowOrchestration(ctx, cwd, nil, &workflow.CLIAsk{}, deps)
	if err == nil {
		t.Fatalf("default deps で error が nil (precheck dir 不在の precondition_failed を期待)")
	}
	if !strings.HasPrefix(err.Error(), "[verdict:precondition_failed]") {
		t.Errorf("error prefix mismatch: got %q, want [verdict:precondition_failed] ...", err.Error())
	}
	rec := readResultJSON(t, cwd, "default-run")
	if rec.FinalVerdict != VerdictPreconditionFailed {
		t.Errorf("default deps final_verdict=%q, want %q (precheck dir 不在経路)",
			rec.FinalVerdict, VerdictPreconditionFailed)
	}
	if rec.ReviewRunID != "default-review-id" {
		t.Errorf("default deps result.json.review_run_id=%q, want %q",
			rec.ReviewRunID, "default-review-id")
	}
	// 副作用ゼロ契約: Cycles が空 (cycle 1 すら開始していない)。
	if len(rec.Cycles) != 0 {
		t.Errorf("default deps result.json.cycles 長=%d, want 0 (副作用ゼロ)", len(rec.Cycles))
	}
}

// ============================================================================
// task 5.2: precheck → loader → no_findings fast-path 統合 integration tests
// (design.md §Single-Run §precheck §Loader §findings 0 件 / Req 1.1, 1.3, 1.5,
// 1.6, 2.1, 2.2, 2.3, 2.5)
//
// 検証観点 (tasks.md 5.2 観察可能完了条件):
//   - 各経路で result.json.final_verdict が期待値かつ Cycles が空。
//   - stub LLM/Git の呼び出し回数が 0 (副作用ゼロ契約 Req 2.3 / 2.5)。
//
// 共通方針:
//   - GitOps は fakePrecheckGitOps (precheck_test.go と同形) を流用しつつ、本ファイルでは
//     Snapshot/ApplyCheck/Apply/Revert の呼び出し回数を 0 として assert するため
//     `sideEffectCalls()` をそのまま利用する。
//   - Loader は real `Load` を使い、`<cwd>/.rilll/review-workflow/<reviewRunID>/` 配下に
//     fixture JSON を書き出す (`writeRuleResultJSON` ヘルパは loader_test.go と共有)。
// ============================================================================

// fastPathTestEnv は 5.2 fast-path 統合テスト用の (cwd, ctx, reviewRunID, applyRunID)
// 4 つ組と `<cwd>/.rilll/review-workflow/<reviewRunID>/` ディレクトリを構築する。
// 戻り値の reviewRunDir は per-rule fixture を書き出す先 (caller 責務)。
func fastPathTestEnv(t *testing.T, applyRunID, reviewRunID string) (cwd string, ctx context.Context, reviewRunDir string) {
	t.Helper()
	cwd, ctx, _ = newApplyWorkflowCtx(t, applyRunID)
	ctx = ContextWithReviewRunID(ctx, reviewRunID)
	reviewRunDir = filepath.Join(cwd, ".rilll", "review-workflow", reviewRunID)
	if err := os.MkdirAll(reviewRunDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", reviewRunDir, err)
	}
	return cwd, ctx, reviewRunDir
}

// recordingOnEvent は WorkflowEvent を蓄積する onEvent helper。
// 5.2 では SkippedSummary の WorkflowText 表示を機械検証するために利用する。
func recordingOnEvent(events *[]workflow.WorkflowEvent) func(workflow.WorkflowEvent) {
	return func(ev workflow.WorkflowEvent) {
		*events = append(*events, ev)
	}
}

// hasWorkflowTextContaining は events 中に Type==WorkflowText かつ Text が substr を
// 含むイベントが少なくとも 1 件あれば true を返す。
func hasWorkflowTextContaining(events []workflow.WorkflowEvent, substr string) bool {
	for _, ev := range events {
		if ev.Type == workflow.WorkflowText && strings.Contains(ev.Text, substr) {
			return true
		}
	}
	return false
}

// assertNoSideEffects は fakePrecheckGitOps の Snapshot/ApplyCheck/Apply/Revert 合計
// 呼び出し回数が 0 であることを assert する (Req 2.3 副作用ゼロ契約)。
func assertNoSideEffects(t *testing.T, git *fakePrecheckGitOps, label string) {
	t.Helper()
	if c := git.sideEffectCalls(); c != 0 {
		t.Errorf("[%s] Snapshot/ApplyCheck/Apply/Revert 合計呼び出し回数=%d, want 0 (副作用ゼロ違反)", label, c)
	}
}

// TestFastPath_DirtyTreeReturnsPreconditionFailed は git working tree dirty 経路で
// `VerdictPreconditionFailed` を返し、Loader / LLM / git apply が呼ばれないことを検証する
// (Req 2.1, 2.2, 2.3, 2.5)。
func TestFastPath_DirtyTreeReturnsPreconditionFailed(t *testing.T) {
	cwd, ctx, _ := fastPathTestEnv(t, "fast-dirty", "review-dirty")

	git := &fakePrecheckGitOps{statusClean: false}
	deps := applyWorkflowDeps{
		gitOps: git,
		load:   Load,
		coreFn: newPrecheckLoaderFastPathCoreFn(git, Load),
	}

	var events []workflow.WorkflowEvent
	err := runApplyWorkflowOrchestration(ctx, cwd, recordingOnEvent(&events), &workflow.CLIAsk{}, deps)
	if err == nil {
		t.Fatalf("dirty tree で error が nil (precondition_failed 期待)")
	}
	if !strings.HasPrefix(err.Error(), "[verdict:precondition_failed]") {
		t.Errorf("error prefix mismatch: %q", err.Error())
	}

	rec := readResultJSON(t, cwd, "fast-dirty")
	if rec.FinalVerdict != VerdictPreconditionFailed {
		t.Errorf("final_verdict=%q, want %q", rec.FinalVerdict, VerdictPreconditionFailed)
	}
	if len(rec.Cycles) != 0 {
		t.Errorf("Cycles 長=%d, want 0 (副作用ゼロ)", len(rec.Cycles))
	}
	// Loader が呼ばれていないため TargetRuleNames は初期値の空 slice、SkippedRuleSummary は nil。
	if len(rec.TargetRuleNames) != 0 {
		t.Errorf("TargetRuleNames 長=%d, want 0 (Loader 未到達)", len(rec.TargetRuleNames))
	}
	if rec.SkippedRuleSummary != nil {
		t.Errorf("SkippedRuleSummary=%v, want nil (Loader 未到達)", rec.SkippedRuleSummary)
	}
	// 副作用契約: Status は 1 回呼ばれる、Snapshot/ApplyCheck/Apply/Revert は 0 回。
	if git.statusCalls != 1 {
		t.Errorf("GitOps.Status 呼び出し回数=%d, want 1", git.statusCalls)
	}
	assertNoSideEffects(t, git, "dirty")
}

// TestFastPath_ReviewRunDirNotFoundReturnsPreconditionFailed は
// `<cwd>/.rilll/review-workflow/<reviewRunID>/` 不在経路で `VerdictPreconditionFailed`
// を返し、GitOps.Status / Loader が呼ばれないことを検証する (Req 1.3, 2.5)。
//
// Precheck は cheap な path 検証 (空 / 不安全 path) → dir 検証 → status 検証 の順に
// 実行する契約のため、reviewRunDir 不在では Status すら呼ばれない (precheck_test.go と同等の不変式)。
func TestFastPath_ReviewRunDirNotFoundReturnsPreconditionFailed(t *testing.T) {
	cwd, ctx, _ := newApplyWorkflowCtx(t, "fast-notfound")
	ctx = ContextWithReviewRunID(ctx, "review-notfound")
	// fastPathTestEnv ではなく newApplyWorkflowCtx を使う: review-workflow dir を作らない。

	git := &fakePrecheckGitOps{statusClean: true}
	// Loader が呼ばれていないことを機械検証するため counter 付き fake load を注入する。
	var loadCalls int
	fakeLoad := func(ctx context.Context, cwd, reviewRunID string) (LoadResult, error) {
		loadCalls++
		return Load(ctx, cwd, reviewRunID)
	}
	deps := applyWorkflowDeps{
		gitOps: git,
		load:   fakeLoad,
		coreFn: newPrecheckLoaderFastPathCoreFn(git, fakeLoad),
	}

	var events []workflow.WorkflowEvent
	err := runApplyWorkflowOrchestration(ctx, cwd, recordingOnEvent(&events), &workflow.CLIAsk{}, deps)
	if err == nil {
		t.Fatalf("dir 不在で error が nil (precondition_failed 期待)")
	}
	if !strings.HasPrefix(err.Error(), "[verdict:precondition_failed]") {
		t.Errorf("error prefix mismatch: %q", err.Error())
	}

	rec := readResultJSON(t, cwd, "fast-notfound")
	if rec.FinalVerdict != VerdictPreconditionFailed {
		t.Errorf("final_verdict=%q, want %q", rec.FinalVerdict, VerdictPreconditionFailed)
	}
	if len(rec.Cycles) != 0 {
		t.Errorf("Cycles 長=%d, want 0", len(rec.Cycles))
	}
	// Precheck の path 検証で reject されているため Status / Loader は呼ばれない。
	if git.statusCalls != 0 {
		t.Errorf("GitOps.Status 呼び出し回数=%d, want 0 (dir 不在は Status より前で reject)", git.statusCalls)
	}
	if loadCalls != 0 {
		t.Errorf("Loader.Load 呼び出し回数=%d, want 0 (precheck 失敗時は Loader 未起動)", loadCalls)
	}
	assertNoSideEffects(t, git, "notfound")
}

// TestFastPath_AllNoFindingsTakesIntentFastPath は全 rule が `no_findings` の経路で
// `VerdictNoFindings` (intent → Fn nil 返却) を返し、副作用 (LLM / git apply / commit) が
// 呼ばれないことを検証する (Req 1.1, 1.5, 1.6, 2.3)。
func TestFastPath_AllNoFindingsTakesIntentFastPath(t *testing.T) {
	cwd, ctx, reviewRunDir := fastPathTestEnv(t, "fast-nofind", "review-nofind")

	// 2 件の rule: いずれも verdict=no_findings (修正対象外)。
	writeRuleResultJSON(t, reviewRunDir, "alpha-rule", result.RuleResult{
		Name:        "alpha-rule",
		Description: "alpha",
		Applied:     true,
		Verdict:     result.VerdictNoFindings,
		Summary:     "no findings",
		Provider:    "claude",
		StartedAt:   "2026-05-04T12:00:00Z",
		FinishedAt:  "2026-05-04T12:00:01Z",
		SourceFile:  "alpha-rule.yaml",
	})
	writeRuleResultJSON(t, reviewRunDir, "bravo-rule", result.RuleResult{
		Name:        "bravo-rule",
		Description: "bravo",
		Applied:     true,
		Verdict:     result.VerdictNoFindings,
		Summary:     "no findings",
		Provider:    "claude",
		StartedAt:   "2026-05-04T12:00:00Z",
		FinishedAt:  "2026-05-04T12:00:01Z",
		SourceFile:  "bravo-rule.yaml",
	})

	git := &fakePrecheckGitOps{statusClean: true}
	deps := applyWorkflowDeps{
		gitOps: git,
		load:   Load,
		coreFn: newPrecheckLoaderFastPathCoreFn(git, Load),
	}

	var events []workflow.WorkflowEvent
	err := runApplyWorkflowOrchestration(ctx, cwd, recordingOnEvent(&events), &workflow.CLIAsk{}, deps)
	if err != nil {
		t.Fatalf("no_findings 経路で error 非 nil: %v (intent → nil 期待)", err)
	}

	rec := readResultJSON(t, cwd, "fast-nofind")
	if rec.FinalVerdict != VerdictNoFindings {
		t.Errorf("final_verdict=%q, want %q (intent fast-path)", rec.FinalVerdict, VerdictNoFindings)
	}
	if len(rec.Cycles) != 0 {
		t.Errorf("Cycles 長=%d, want 0 (副作用ゼロ)", len(rec.Cycles))
	}
	if len(rec.TargetRuleNames) != 0 {
		t.Errorf("TargetRuleNames=%v, want 空集合 (全 no_findings は対象外)", rec.TargetRuleNames)
	}
	// SkippedRuleSummary に no_findings=2 が記録されていること。
	if got := rec.SkippedRuleSummary["no_findings"]; got != 2 {
		t.Errorf("SkippedRuleSummary[no_findings]=%d, want 2 (alpha+bravo)", got)
	}
	// SkippedSummary 表示 WorkflowText が onEvent に流れていること。
	if !hasWorkflowTextContaining(events, "対象外") || !hasWorkflowTextContaining(events, "no_findings=2") {
		t.Errorf("SkippedSummary 表示 WorkflowText が見つからない: events=%+v", events)
	}
	// Status は 1 回、Snapshot/ApplyCheck/Apply/Revert は 0 回。
	if git.statusCalls != 1 {
		t.Errorf("GitOps.Status 呼び出し回数=%d, want 1", git.statusCalls)
	}
	assertNoSideEffects(t, git, "no_findings")
}

// TestFastPath_MixedFindingsDisplaysSkippedSummary は findings_reported と他 verdict
// (no_findings / failed 等) が混在する経路で:
//   - record.TargetRuleNames に findings_reported rule のみが入る
//   - record.SkippedRuleSummary に findings_reported 以外の verdict が verdict 別に集計される
//   - SkippedSummary が WorkflowText イベントで表示される
//   - LLM / git apply / commit が呼ばれない (5.2 段階の副作用ゼロ契約; 5.3+ で propose loop 統合)
//
// を検証する (Req 1.1, 1.5, 2.3)。
//
// 5.2 段階では findings_reported >= 1 件は coreFn の skeleton fallback に到達するため、
// final_verdict は VerdictFailed (5.3 で propose loop 接続後は applied / cycle_exhausted /
// out_of_scope_diff / proposal_unparseable 等に置き換わる)。本テストは 5.2 単独段階の
// observable を anchor として固定する。
func TestFastPath_MixedFindingsDisplaysSkippedSummary(t *testing.T) {
	cwd, ctx, reviewRunDir := fastPathTestEnv(t, "fast-mixed", "review-mixed")

	// alpha: findings_reported (修正対象 1 件)
	writeRuleResultJSON(t, reviewRunDir, "alpha-rule", result.RuleResult{
		Name:            "alpha-rule",
		Description:     "alpha",
		Applied:         true,
		Verdict:         result.VerdictFindingsReported,
		FindingsPresent: true,
		Summary:         "1 finding",
		Findings: []result.Finding{
			{Severity: result.SeverityError, File: "auth/login.go", Message: "x"},
		},
		Provider:   "claude",
		StartedAt:  "2026-05-04T12:00:00Z",
		FinishedAt: "2026-05-04T12:00:30Z",
		SourceFile: "alpha-rule.yaml",
	})
	// bravo: no_findings (修正対象外)
	writeRuleResultJSON(t, reviewRunDir, "bravo-rule", result.RuleResult{
		Name:        "bravo-rule",
		Description: "bravo",
		Applied:     true,
		Verdict:     result.VerdictNoFindings,
		Summary:     "no findings",
		Provider:    "claude",
		StartedAt:   "2026-05-04T12:00:00Z",
		FinishedAt:  "2026-05-04T12:00:01Z",
		SourceFile:  "bravo-rule.yaml",
	})
	// charlie: failed (修正対象外)
	writeRuleResultJSON(t, reviewRunDir, "charlie-rule", result.RuleResult{
		Name:            "charlie-rule",
		Description:     "charlie",
		Applied:         true,
		Verdict:         result.VerdictFailed,
		Summary:         "agent error",
		FailureCategory: "provider_error",
		Provider:        "claude",
		StartedAt:       "2026-05-04T12:00:00Z",
		FinishedAt:      "2026-05-04T12:00:01Z",
		SourceFile:      "charlie-rule.yaml",
	})

	git := &fakePrecheckGitOps{statusClean: true}
	deps := applyWorkflowDeps{
		gitOps: git,
		load:   Load,
		coreFn: newPrecheckLoaderFastPathCoreFn(git, Load),
	}

	var events []workflow.WorkflowEvent
	err := runApplyWorkflowOrchestration(ctx, cwd, recordingOnEvent(&events), &workflow.CLIAsk{}, deps)
	if err == nil {
		t.Fatalf("mixed 経路で error が nil (5.3 未実装の verdict=failed skeleton fallback を期待)")
	}
	// 5.2 段階の skeleton fallback verdict (5.3 で applied / cycle_exhausted 等に置換予定)。
	if !strings.HasPrefix(err.Error(), "[verdict:failed]") {
		t.Errorf("error prefix mismatch (5.2 段階の skeleton fallback 期待): %q", err.Error())
	}

	rec := readResultJSON(t, cwd, "fast-mixed")
	if rec.FinalVerdict != VerdictFailed {
		t.Errorf("final_verdict=%q, want %q (5.2 段階 skeleton fallback)", rec.FinalVerdict, VerdictFailed)
	}
	// TargetRuleNames は alpha-rule のみ (findings_reported)。
	if len(rec.TargetRuleNames) != 1 || rec.TargetRuleNames[0] != "alpha-rule" {
		t.Errorf("TargetRuleNames=%v, want [alpha-rule]", rec.TargetRuleNames)
	}
	// SkippedRuleSummary には no_findings=1 / failed=1 が含まれる。
	if got := rec.SkippedRuleSummary["no_findings"]; got != 1 {
		t.Errorf("SkippedRuleSummary[no_findings]=%d, want 1", got)
	}
	if got := rec.SkippedRuleSummary["failed"]; got != 1 {
		t.Errorf("SkippedRuleSummary[failed]=%d, want 1", got)
	}
	// SkippedSummary 表示 WorkflowText が出力されていること (Req 1.5)。
	if !hasWorkflowTextContaining(events, "対象外") {
		t.Errorf("SkippedSummary 表示 WorkflowText が見つからない: events=%+v", events)
	}
	if !hasWorkflowTextContaining(events, "alpha-rule") {
		t.Errorf("対象 rule 表示が見つからない: events=%+v", events)
	}
	// Cycles は空 (propose loop 未到達 / 副作用ゼロ)。
	if len(rec.Cycles) != 0 {
		t.Errorf("Cycles 長=%d, want 0 (5.2 段階で propose loop 未到達)", len(rec.Cycles))
	}
	// 副作用契約: Status は 1 回、Snapshot/ApplyCheck/Apply/Revert は 0 回 (5.2 では LLM / commit 未起動)。
	if git.statusCalls != 1 {
		t.Errorf("GitOps.Status 呼び出し回数=%d, want 1", git.statusCalls)
	}
	assertNoSideEffects(t, git, "mixed")
}

// TestFastPath_LoaderSchemaErrorMapsToPreconditionFailed は Loader が
// LoaderSchemaError を返した経路で `VerdictPreconditionFailed` に集約され、
// 副作用が走らないことを検証する (Req 1.4 / Loader と Workflow の合流)。
//
// fixture: 必須フィールド (verdict) が空の per-rule JSON を 1 件配置する。
// real Loader が `*LoaderSchemaError` を返し、coreFn 側で precondition_failed にマップする。
func TestFastPath_LoaderSchemaErrorMapsToPreconditionFailed(t *testing.T) {
	cwd, ctx, reviewRunDir := fastPathTestEnv(t, "fast-schema", "review-schema")

	// 必須 verdict が空の壊れ fixture (loader_test.go と同形)。
	brokenPath := filepath.Join(reviewRunDir, "broken-rule.json")
	if err := os.WriteFile(brokenPath, []byte(`{"name":"broken-rule","verdict":""}`), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", brokenPath, err)
	}

	git := &fakePrecheckGitOps{statusClean: true}
	deps := applyWorkflowDeps{
		gitOps: git,
		load:   Load,
		coreFn: newPrecheckLoaderFastPathCoreFn(git, Load),
	}

	err := runApplyWorkflowOrchestration(ctx, cwd, nil, &workflow.CLIAsk{}, deps)
	if err == nil {
		t.Fatalf("schema 違反で error が nil (precondition_failed 期待)")
	}
	if !strings.HasPrefix(err.Error(), "[verdict:precondition_failed]") {
		t.Errorf("error prefix mismatch: %q", err.Error())
	}
	rec := readResultJSON(t, cwd, "fast-schema")
	if rec.FinalVerdict != VerdictPreconditionFailed {
		t.Errorf("final_verdict=%q, want %q", rec.FinalVerdict, VerdictPreconditionFailed)
	}
	if !strings.Contains(rec.FinalSummary, "broken-rule.json") {
		t.Errorf("FinalSummary が違反 file 名を含まない: %q", rec.FinalSummary)
	}
	if len(rec.Cycles) != 0 {
		t.Errorf("Cycles 長=%d, want 0", len(rec.Cycles))
	}
	assertNoSideEffects(t, git, "schema")
	// Loader-level errors do not advance to git apply/commit; ensure improvepatch.GitError 等の
	// 副作用 sentinel が一切出現していないことを言及するのみ (上記 assertNoSideEffects で十分)。
	_ = improvepatch.ErrRevertFailed // 参照のみ (improvepatch import を保持し、5.3+ 拡張時の依存契約を可視化)。
}
