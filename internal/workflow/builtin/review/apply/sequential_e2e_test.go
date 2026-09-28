// Package apply のテスト本ファイルは task 8.1 / design.md §End-to-End Validation §Acceptance Tests
// (design.md L1020-L1022) / Req 1.2, 8.3, 8.4, 8.5 で要求される `review-and-apply` Sequential
// 経路の **End-to-End** 統合テストを実装する。
//
// 6.1 (sequential_test.go) は orchestration ラッパ単体の **parity** 不変式を機械検証するが、
// 本ファイルはそれより一段上の **E2E** スコープで以下 3 シナリオを通しで駆動する:
//
//  1. **happy** (前段 findings_reported → 後段 applied):
//     reviewDef stub が `findings_reported` per-rule fixture を書き出し nil を返す。
//     後段 apply.Workflow が production 同等の coreFn (newProposeLoopCoreFn) を経由して
//     plan/diff/self-review accept/commit-message → Snapshot/ApplyCheck/Apply/Add/Commit を完走し、
//     `result.json.final_verdict=applied` / `result.json.review_run_id` が前段 runID と一致する
//     ことを検証する (Req 1.2, 8.3, 8.5)。
//
//  2. **no_findings fast-path** (前段 no_findings → 後段 起動するが副作用ゼロ):
//     reviewDef stub が `no_findings` のみの per-rule fixture を書き出し nil を返す。
//     後段 apply.Workflow は precheck → loader を通過するが TargetRules == 0 件のため
//     `VerdictNoFindings` (intent → Fn nil) で副作用 (LLM/git apply/commit) ゼロで終端する
//     ことを検証する (Req 8.4 補足 / design.md §4.1 採用方針)。
//
//  3. **precheck failure** (前段 precheck_failed → 後段 未起動):
//     reviewDef stub が `errors.New("[verdict:precheck_failed] ...")` で error 返却し、
//     `runReviewAndApplyOrchestration` が後段 apply.Definition.Run を起動せず同 error を
//     外向きに伝播することを検証する (Req 8.4 / 既存 Sequential 等価性)。
//
// 各シナリオで akashic 記録 (workflow_runs) の `Error` 列の値が design.md §Error Strategy
// (L931 「intent verdict では Fn から nil → error 列空、内部失敗は `[verdict:<id>] <summary>`」)
// の規約と一致することを e2eRecorder で機械検証する:
//
//   - happy: review finish.Error == "" / apply finish.Error == "" (双方 intent)
//   - no_findings: review finish.Error == "" / apply finish.Error == "" (双方 intent)
//   - precheck_failed: review finish.Error が `[verdict:precheck_failed]` プレフィクスを持つ /
//     apply は finish 記録が一切行われない (Run() 自体が起動しないため)
//
// 副作用 zero 観点 (Req 2.3 / design.md §4.1 副作用契約):
//
//   - no_findings: scriptedProposeAgent は呼ばれない (LLM 0 件) / fakePrecheckGitOps の
//     Snapshot/ApplyCheck/Apply/Add/Commit/Revert は 0 件 (Status のみ 1 件、precheck 用)。
//   - precheck_failed: applyDef.Fn 自体が呼ばれないため scriptedProposeAgent / fakeGitOps の
//     全メソッド呼び出しが 0 件。
//
// E2E 設計上の決定:
//
//   - reviewDef は **stub** とし、内部で自前のロジックで per-rule fixture JSON を書き出し、
//     `WorkflowEvent` を 1 件 emit する (Definition.Run の wrappedOnEvent が event.RunID を
//     埋める契約を発動させ、後段 ctx 注入 seam を通過させるため)。これは production の
//     `review.Workflow(agent)` を直接呼ぶと gh CLI / LLM が必要となり E2E が不可能になるため。
//   - applyDef は **production の `Workflow(agent)` ではなく**、production 同等の
//     `runApplyWorkflowOrchestration` を deps 注入で wrap した薄い Definition とする。これに
//     より GitOpsExt / Agent を fake に差し替えつつ、coreFn / applyHook 等の production wiring
//     を 100% 通過させる (design.md §Workflow §Implementation Notes)。
//
// 6.1 との重複回避: 本ファイルは 6.1 の parity test と同じ `runReviewAndApplyOrchestration`
// を呼ぶが、観測対象が異なる (6.1 = orchestration 不変式 / 8.1 = E2E result.json + akashic 記録)。
package apply

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	improvepatch "github.com/kazuki-oshino/rilll/internal/workflow/builtin/improve/patch"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/result"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// e2eRecorder は E2E テスト用の最小 akashic.Recorder 実装で、
// 親 (review-and-apply 全体) と child (review / apply) の WorkflowRunStart /
// WorkflowRunFinish 呼び出しをそのまま蓄積する (design.md §Error Strategy / L993)。
//
// 設計判断:
//   - 各 child Definition の Recorder フィールドに本 recorder を割り当てて、Definition.Run の
//     RecordWorkflowRunFinish 経路 (types.go L121-L126) で error 文字列が記録されることを
//     observable に保つ。
//   - apply 側の RecordRunArtifact (result.json 等の artifact 登録) も蓄積するが、本 E2E
//     では verdict / error 列の検証が主目的のため artifact 蓄積は副次的な観測点として保持する。
//   - workflow_test.go の applyWorkflowFakeRecorder と類似だが、starts / finishes を別途
//     蓄積する点が異なる (E2E では「いつ Recorder が呼ばれたか」「何回呼ばれたか」が観測対象)。
type e2eRecorder struct {
	starts    []akashic.WorkflowRun
	finishes  []akashic.WorkflowRunFinish
	artifacts []akashic.RunArtifact
}

func (r *e2eRecorder) RecordWorkflowRunStart(_ context.Context, run akashic.WorkflowRun) error {
	r.starts = append(r.starts, run)
	return nil
}
func (r *e2eRecorder) RecordWorkflowRunFinish(_ context.Context, finish akashic.WorkflowRunFinish) error {
	r.finishes = append(r.finishes, finish)
	return nil
}
func (r *e2eRecorder) RecordAgentCallStart(context.Context, akashic.AgentCall) error { return nil }
func (r *e2eRecorder) RecordAgentCallFinish(context.Context, akashic.AgentCallResult) error {
	return nil
}
func (r *e2eRecorder) RecordRawEvent(context.Context, akashic.RawEvent) error { return nil }
func (r *e2eRecorder) RecordRunArtifact(_ context.Context, a akashic.RunArtifact) error {
	r.artifacts = append(r.artifacts, a)
	return nil
}

// RecordUserCorrection は memory-distiller-trigger-policy spec Task 3.3 で追加した
// Recorder interface 拡張への追従 (no-op)。本 E2E は workflow_run / artifact / verdict の
// 蓄積観測が責務で user-correction marker 経路は対象外のため履歴は保持しない。
func (r *e2eRecorder) RecordUserCorrection(context.Context, string) error {
	return nil
}

// RecordLearning は agent-learnings spec (task 3.2) で Recorder interface に追加された
// メソッドへの no-op 追従。本 E2E は workflow_run / artifact / verdict の蓄積観測が責務で
// learnings 経路は対象外のため履歴は保持しない。
func (r *e2eRecorder) RecordLearning(context.Context, akashic.LearningRow) error {
	return nil
}

func (r *e2eRecorder) Close() error { return nil }

// 型レベル assertion: e2eRecorder が akashic.Recorder を満たすことを compile time で保証する。
var _ akashic.Recorder = (*e2eRecorder)(nil)

// e2eReviewDef は E2E テスト用の review-workflow stub Definition factory。
//
// stubFn は Definition.Fn 本体で、ctx から RunMetadata.WorkflowRunID を取り出し
// `<cwd>/.rilll/review-workflow/<reviewRunID>/` に per-rule fixture JSON を書き出す
// (caller 側で responsibility を持たせる)。emit は Fn 内で Definition.Run の wrappedOnEvent
// を経由して親へ流すイベント (event.RunID 注入の発動条件として 1 件以上必要)。
//
// recorder を渡すことで Definition.Run の RecordWorkflowRunStart/Finish 経路が活きる。
// recorder == nil でも Definition.Run は no-op で動作する (types.go L88, L114)。
func e2eReviewDef(
	stubFn func(ctx context.Context, cwd string, reviewRunID string) error,
	emit []workflow.WorkflowEvent,
	recorder akashic.Recorder,
) workflow.Definition {
	return workflow.Definition{
		ID:       "review-workflow",
		Label:    "レビュー実行",
		Recorder: recorder,
		Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			md, _ := workflowctx.RunMetadataFromContext(ctx)
			reviewRunID := md.WorkflowRunID
			if stubFn != nil {
				if err := stubFn(ctx, cwd, reviewRunID); err != nil {
					return err
				}
			}
			for _, e := range emit {
				if onEvent != nil {
					onEvent(e)
				}
			}
			return nil
		},
	}
}

// e2eApplyDef は E2E テスト用の review-apply-workflow Definition factory。
//
// production の `Workflow(agent)` ではなく、`runApplyWorkflowOrchestration` を直接呼び出し
// applyWorkflowDeps を test 側で組み立てた値で wrap する。これにより GitOpsExt / Agent を
// fake に差し替えつつ、coreFn (`newProposeLoopCoreFn`) や applyHook (`newApplyHook`) 等の
// production wiring を 100% 通過させる。
//
// applyRunIDOut は Fn 起動時に RunMetadata.WorkflowRunID を保存する out 引数で、テスト側で
// `<cwd>/.rilll/review-apply-workflow/<applyRunID>/result.json` を読むために必要 (Definition.Run
// が auto-generate する applyRunID をテスト側で観測するため)。
func e2eApplyDef(
	deps applyWorkflowDeps,
	recorder akashic.Recorder,
	applyRunIDOut *string,
) workflow.Definition {
	return workflow.Definition{
		ID:    applyWorkflowID,
		Label: applyWorkflowLabel,
		Args: []workflow.ArgSpec{
			{Key: argReviewRunID, Required: false},
		},
		Recorder: recorder,
		Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			if md, ok := workflowctx.RunMetadataFromContext(ctx); ok && applyRunIDOut != nil {
				*applyRunIDOut = md.WorkflowRunID
			}
			return runApplyWorkflowOrchestration(ctx, cwd, onEvent, ask, deps)
		},
	}
}

// writeReviewFindingsFixture は per-rule fixture (`findings_reported` 1 件) を書き出す helper。
// 後段 apply の Loader が `auth-rule` を TargetRules に拾い、propose loop が `auth/login.go`
// 1 ファイルを修正対象として patch.ExtractAndGuard の allowlist に乗せる契約。
func writeReviewFindingsFixture(t *testing.T, cwd, reviewRunID string) {
	t.Helper()
	reviewRunDir := filepath.Join(cwd, ".rilll", "review-workflow", reviewRunID)
	if err := os.MkdirAll(reviewRunDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", reviewRunDir, err)
	}
	writeRuleResultJSON(t, reviewRunDir, "auth-rule", result.RuleResult{
		Name:            "auth-rule",
		Description:     "auth checks",
		Applied:         true,
		Verdict:         result.VerdictFindingsReported,
		FindingsPresent: true,
		Summary:         "1 finding",
		Findings: []result.Finding{
			{Severity: result.SeverityError, File: "auth/login.go", Location: "auth/login.go:10", Message: "login guard is missing"},
		},
		Provider:   "claude",
		StartedAt:  "2026-05-04T12:00:00Z",
		FinishedAt: "2026-05-04T12:00:30Z",
		SourceFile: "auth-rule.yaml",
	})
}

// writeReviewNoFindingsFixture は per-rule fixture (`no_findings` 2 件) を書き出す helper。
// 後段 apply の Loader はこれらをすべて SkippedSummary に集計し、TargetRules == 0 件で
// no_findings fast-path を発動する。
func writeReviewNoFindingsFixture(t *testing.T, cwd, reviewRunID string) {
	t.Helper()
	reviewRunDir := filepath.Join(cwd, ".rilll", "review-workflow", reviewRunID)
	if err := os.MkdirAll(reviewRunDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", reviewRunDir, err)
	}
	for _, name := range []string{"alpha-rule", "bravo-rule"} {
		writeRuleResultJSON(t, reviewRunDir, name, result.RuleResult{
			Name:        name,
			Description: name,
			Applied:     true,
			Verdict:     result.VerdictNoFindings,
			Summary:     "no findings",
			Provider:    "claude",
			StartedAt:   "2026-05-04T12:00:00Z",
			FinishedAt:  "2026-05-04T12:00:01Z",
			SourceFile:  name + ".yaml",
		})
	}
}

// findApplyRunIDFromFS は `<cwd>/.rilll/review-apply-workflow/` 配下を読み、
// 唯一の child directory 名 (= applyRunID) を返す defensive helper。
// applyRunIDOut 経由で取得できなかった経路 (E2E for precheck_failed では applyDef.Fn 未起動
// なので applyRunID は存在しない) と区別するため、本 helper は呼び出し側が applyRunID を
// 明示的に観測する経路でのみ利用する。
func findApplyRunIDFromFS(t *testing.T, cwd string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(cwd, ".rilll", "review-apply-workflow"))
	if err != nil {
		t.Fatalf("ReadDir(%s/.rilll/review-apply-workflow): %v", cwd, err)
	}
	if len(entries) != 1 {
		t.Fatalf("review-apply-workflow run dirs=%d, want 1: %+v", len(entries), entries)
	}
	return entries[0].Name()
}

// findFinishByRunID は e2eRecorder.finishes から指定 runID の WorkflowRunFinish を返す。
// 見つからない場合は (ZeroValue, false)。
func findFinishByRunID(rec *e2eRecorder, runID string) (akashic.WorkflowRunFinish, bool) {
	for _, f := range rec.finishes {
		if f.ID == runID {
			return f, true
		}
	}
	return akashic.WorkflowRunFinish{}, false
}

// =============================================================================
// E2E test 1: happy path (前段 findings_reported → 後段 applied)
// =============================================================================

// TestReviewAndApplyE2E_HappyAppliedPropagatesReviewRunID は happy 経路を E2E で駆動し、
// `result.json.review_run_id` が前段 review の runID と一致 (Req 8.5)、`final_verdict=applied`
// (Req 5.3)、akashic 記録の review/apply 双方の error 列が空 (intent → nil → error 空) で
// あることを検証する (design.md §End-to-End §1)。
func TestReviewAndApplyE2E_HappyAppliedPropagatesReviewRunID(t *testing.T) {
	cwd := t.TempDir()
	rec := &e2eRecorder{}

	// 前段 review stub: findings_reported fixture を書き出し、event 1 件 emit。
	reviewDef := e2eReviewDef(
		func(_ context.Context, cwd string, reviewRunID string) error {
			writeReviewFindingsFixture(t, cwd, reviewRunID)
			return nil
		},
		// emit 1 件: Definition.Run の wrappedOnEvent が event.RunID = reviewRunID を埋め、
		// runReviewAndApplyOrchestration の reviewWrapped が reviewRunID を抽出する契約を発動。
		[]workflow.WorkflowEvent{{Type: workflow.WorkflowText, Text: "review-finished"}},
		rec,
	)

	// 後段 apply: production wiring (newProposeLoopCoreFn + newApplyHook) を fake gitOps + scripted agent で駆動。
	const baselineSHA = "0123456789abcdef0123456789abcdef01234567"
	const commitSHA = "fedcba9876543210fedcba9876543210fedcba98"
	fake := &fakeApplyGitOpsExt{
		statusClean: true,
		snapshotResults: []improvepatch.BaselineRef{
			{HeadCommit: baselineSHA},
			{HeadCommit: commitSHA},
		},
	}
	agent := &scriptedProposeAgent{responses: []provider.AgentResponse{
		planResp("E2E happy plan"),
		diffResp(makePatchAgentJSON(t, proposeLoopAuthLoginDiff, []string{"auth/login.go"})),
		selfReviewResp(t, SelfReviewAccept, "適用してよい"),
		commitMessageResp("fix(auth): tighten login guard\n"),
	}}
	applyHook := newApplyHook(fake, agent)
	applyDeps := applyWorkflowDeps{
		gitOps:    fake,
		load:      Load,
		agent:     agent,
		applyHook: applyHook,
		coreFn:    newProposeLoopCoreFn(fake, Load, agent, applyHook),
	}
	var applyRunID string
	applyDef := e2eApplyDef(applyDeps, rec, &applyRunID)

	deps := reviewAndApplyDeps{reviewDef: reviewDef, applyDef: applyDef}

	// orchestration 駆動。
	var events []workflow.WorkflowEvent
	err := runReviewAndApplyOrchestration(context.Background(), cwd, recordingOnEvent(&events), &workflow.CLIAsk{}, deps)
	if err != nil {
		t.Fatalf("E2E happy: orchestration returned error: %v (intent → nil 期待)", err)
	}

	// review finish 記録から reviewRunID を抽出 (= Definition.Run が採番した runID)。
	if len(rec.starts) != 2 {
		t.Fatalf("recorder starts=%d, want 2 (review + apply)", len(rec.starts))
	}
	reviewRunID := rec.starts[0].ID
	if rec.starts[0].WorkflowID != "review-workflow" {
		t.Errorf("starts[0].WorkflowID=%q, want %q", rec.starts[0].WorkflowID, "review-workflow")
	}
	if rec.starts[1].WorkflowID != applyWorkflowID {
		t.Errorf("starts[1].WorkflowID=%q, want %q", rec.starts[1].WorkflowID, applyWorkflowID)
	}
	if applyRunID == "" {
		t.Fatalf("applyRunID が空 (applyDef.Fn 未起動の疑い)")
	}
	if applyRunID == reviewRunID {
		t.Fatalf("applyRunID と reviewRunID が同値 (%q): apply は別途採番される契約 (design.md §Sequential §runID hand-off)", applyRunID)
	}
	if rec.starts[1].ID != applyRunID {
		t.Errorf("starts[1].ID=%q, want applyRunID=%q", rec.starts[1].ID, applyRunID)
	}

	// **核心 assertion (Req 8.5)**: result.json.review_run_id が前段 runID と一致。
	resultRec := readResultJSON(t, cwd, applyRunID)
	if resultRec.ReviewRunID != reviewRunID {
		t.Errorf("result.json.review_run_id=%q, want %q (前段 review.Run の runID と一致 / Req 8.5)",
			resultRec.ReviewRunID, reviewRunID)
	}
	if resultRec.FinalVerdict != VerdictApplied {
		t.Errorf("result.json.final_verdict=%q, want %q", resultRec.FinalVerdict, VerdictApplied)
	}
	if resultRec.CommitSHA != commitSHA {
		t.Errorf("result.json.commit_sha=%q, want %q", resultRec.CommitSHA, commitSHA)
	}

	// akashic 記録 (workflow_runs.error) 検証 (design.md §Error Strategy):
	//   - review: intent (findings_reported / no_findings いずれも) → Fn nil → finish.Error == ""
	//   - apply: intent (applied) → Fn nil → finish.Error == ""
	if len(rec.finishes) != 2 {
		t.Fatalf("recorder finishes=%d, want 2 (review + apply)", len(rec.finishes))
	}
	if reviewFinish, ok := findFinishByRunID(rec, reviewRunID); !ok {
		t.Errorf("review finish 記録が見つからない: runID=%q", reviewRunID)
	} else if reviewFinish.Error != "" {
		t.Errorf("review finish.Error=%q, want \"\" (intent → 空)", reviewFinish.Error)
	}
	if applyFinish, ok := findFinishByRunID(rec, applyRunID); !ok {
		t.Errorf("apply finish 記録が見つからない: runID=%q", applyRunID)
	} else if applyFinish.Error != "" {
		t.Errorf("apply finish.Error=%q, want \"\" (applied は intent → 空)", applyFinish.Error)
	}
}

// =============================================================================
// E2E test 2: no_findings fast-path (前段 no_findings → 後段 副作用ゼロ終端)
// =============================================================================

// TestReviewAndApplyE2E_NoFindingsFastPathZeroSideEffects は no_findings fast-path 経路を
// E2E で駆動し、後段 apply が `VerdictNoFindings` (intent → Fn nil) で終端し、LLM / git apply /
// commit のいずれも呼ばれないことを検証する (design.md §End-to-End §2 / Req 1.6, 2.3, 8.4 補足)。
func TestReviewAndApplyE2E_NoFindingsFastPathZeroSideEffects(t *testing.T) {
	cwd := t.TempDir()
	rec := &e2eRecorder{}

	// 前段 review stub: no_findings fixture を書き出し、event 1 件 emit。
	reviewDef := e2eReviewDef(
		func(_ context.Context, cwd string, reviewRunID string) error {
			writeReviewNoFindingsFixture(t, cwd, reviewRunID)
			return nil
		},
		[]workflow.WorkflowEvent{{Type: workflow.WorkflowText, Text: "review-finished-no-findings"}},
		rec,
	)

	// 後段 apply: precheck だけ通過する fake (LLM は呼ばれない前提だが、安全のため scripted は空応答)。
	fake := &fakeApplyGitOpsExt{statusClean: true}
	agent := &scriptedProposeAgent{} // responses 空: 1 件でも呼ばれたら "exhausted" error で test fail
	applyHook := newApplyHook(fake, agent)
	applyDeps := applyWorkflowDeps{
		gitOps:    fake,
		load:      Load,
		agent:     agent,
		applyHook: applyHook,
		coreFn:    newProposeLoopCoreFn(fake, Load, agent, applyHook),
	}
	var applyRunID string
	applyDef := e2eApplyDef(applyDeps, rec, &applyRunID)

	deps := reviewAndApplyDeps{reviewDef: reviewDef, applyDef: applyDef}

	var events []workflow.WorkflowEvent
	err := runReviewAndApplyOrchestration(context.Background(), cwd, recordingOnEvent(&events), &workflow.CLIAsk{}, deps)
	if err != nil {
		t.Fatalf("E2E no_findings: orchestration returned error: %v (intent → nil 期待)", err)
	}

	if applyRunID == "" {
		t.Fatalf("applyRunID が空 (applyDef.Fn 未起動 = 後段未起動の疑い): no_findings 経路では後段が起動する契約 (design.md §Sequential §振り分け方針)")
	}

	// 核心 assertion: applyRunID は採番されるが result.json.final_verdict=no_findings で終端、
	// review_run_id は前段 review runID と一致する。
	if len(rec.starts) != 2 {
		t.Fatalf("recorder starts=%d, want 2 (review + apply 起動)", len(rec.starts))
	}
	reviewRunID := rec.starts[0].ID
	resultRec := readResultJSON(t, cwd, applyRunID)
	if resultRec.FinalVerdict != VerdictNoFindings {
		t.Errorf("result.json.final_verdict=%q, want %q (no_findings fast-path / Req 1.6)",
			resultRec.FinalVerdict, VerdictNoFindings)
	}
	if resultRec.ReviewRunID != reviewRunID {
		t.Errorf("result.json.review_run_id=%q, want %q (前段 review runID と一致 / Req 8.5)",
			resultRec.ReviewRunID, reviewRunID)
	}
	if len(resultRec.Cycles) != 0 {
		t.Errorf("Cycles 長=%d, want 0 (no_findings fast-path は cycle 未起動)", len(resultRec.Cycles))
	}

	// 副作用ゼロ契約検証 (Req 2.3 / 1.6):
	//   - LLM: scriptedProposeAgent.calls が 0 件 (responses 空配列で呼ばれたら exhausted error が発火する保険)。
	//   - git: Snapshot/ApplyCheck/Apply/Add/Commit/Revert すべて 0 件。Status のみ precheck で 1 回。
	if len(agent.calls) != 0 {
		t.Errorf("scriptedProposeAgent.calls=%d, want 0 (no_findings fast-path は LLM 呼び出しゼロ)", len(agent.calls))
	}
	if fake.statusCalls != 1 {
		t.Errorf("fake.statusCalls=%d, want 1 (precheck のみ)", fake.statusCalls)
	}
	if fake.snapshotCalls != 0 {
		t.Errorf("fake.snapshotCalls=%d, want 0", fake.snapshotCalls)
	}
	if fake.applyCheckCalls != 0 {
		t.Errorf("fake.applyCheckCalls=%d, want 0", fake.applyCheckCalls)
	}
	if fake.applyCalls != 0 {
		t.Errorf("fake.applyCalls=%d, want 0", fake.applyCalls)
	}
	if fake.addCalls != 0 {
		t.Errorf("fake.addCalls=%d, want 0", fake.addCalls)
	}
	if fake.commitCalls != 0 {
		t.Errorf("fake.commitCalls=%d, want 0", fake.commitCalls)
	}
	if fake.revertCalls != 0 {
		t.Errorf("fake.revertCalls=%d, want 0", fake.revertCalls)
	}

	// akashic 記録 (workflow_runs.error) 検証: review/apply 双方 intent → 空。
	if len(rec.finishes) != 2 {
		t.Fatalf("recorder finishes=%d, want 2 (review + apply)", len(rec.finishes))
	}
	if reviewFinish, ok := findFinishByRunID(rec, reviewRunID); !ok {
		t.Errorf("review finish 記録が見つからない: runID=%q", reviewRunID)
	} else if reviewFinish.Error != "" {
		t.Errorf("review finish.Error=%q, want \"\" (intent → 空)", reviewFinish.Error)
	}
	if applyFinish, ok := findFinishByRunID(rec, applyRunID); !ok {
		t.Errorf("apply finish 記録が見つからない: runID=%q", applyRunID)
	} else if applyFinish.Error != "" {
		t.Errorf("apply finish.Error=%q, want \"\" (no_findings は intent → 空)", applyFinish.Error)
	}
}

// =============================================================================
// E2E test 3: precheck failure (前段 precheck_failed → 後段 未起動)
// =============================================================================

// TestReviewAndApplyE2E_PrecheckFailureSkipsApplyAndPropagatesError は precheck failure 経路
// を E2E で駆動し、後段 apply.Definition.Run が起動せず前段 error がそのまま外向きに伝播する
// ことを検証する (design.md §End-to-End §3 / Req 8.4 既存 Sequential 等価性 / Req 2.4)。
func TestReviewAndApplyE2E_PrecheckFailureSkipsApplyAndPropagatesError(t *testing.T) {
	cwd := t.TempDir()
	rec := &e2eRecorder{}

	// 前段 review stub: precheck_failed 形 error を返す (gh 認証失敗の friendly verdict 模倣)。
	const precheckErrMsg = "[verdict:precheck_failed] gh authentication required: run `gh auth login`"
	reviewDef := e2eReviewDef(
		func(_ context.Context, _ string, _ string) error {
			return errors.New(precheckErrMsg)
		},
		nil, // emit 無し: precheck 失敗時は WorkflowEvent.RunID 抽出も発動しない (Sequential parity)。
		rec,
	)

	// 後段 apply: 起動された場合即座に test fail させるための「呼ばれたら panic する」hook を組み込む。
	// ここで applyDef.Fn が呼ばれてしまうと Sequential 等価性が崩れているサインなので、Fatalf で停止する。
	fake := &fakeApplyGitOpsExt{statusClean: true}
	agent := &scriptedProposeAgent{} // 同上
	applyHook := newApplyHook(fake, agent)
	applyDeps := applyWorkflowDeps{
		gitOps:    fake,
		load:      Load,
		agent:     agent,
		applyHook: applyHook,
		coreFn:    newProposeLoopCoreFn(fake, Load, agent, applyHook),
	}
	var applyFnCalled bool
	applyDef := workflow.Definition{
		ID:       applyWorkflowID,
		Label:    applyWorkflowLabel,
		Recorder: rec,
		Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			applyFnCalled = true
			return runApplyWorkflowOrchestration(ctx, cwd, onEvent, ask, applyDeps)
		},
	}

	deps := reviewAndApplyDeps{reviewDef: reviewDef, applyDef: applyDef}

	var events []workflow.WorkflowEvent
	err := runReviewAndApplyOrchestration(context.Background(), cwd, recordingOnEvent(&events), &workflow.CLIAsk{}, deps)
	if err == nil {
		t.Fatalf("E2E precheck_failed: orchestration が nil 返却 (前段 error 期待)")
	}
	if err.Error() != precheckErrMsg {
		t.Errorf("orchestration error=%q, want %q (前段 error が外向きに伝播)", err.Error(), precheckErrMsg)
	}
	if !strings.HasPrefix(err.Error(), "[verdict:precheck_failed]") {
		t.Errorf("error prefix=%q, want [verdict:precheck_failed] ...", err.Error())
	}

	// 核心 assertion: 後段未起動 (Sequential 等価性 / Req 8.4)。
	if applyFnCalled {
		t.Fatalf("applyDef.Fn が呼ばれた (Sequential 等価性違反: 前段 error → 後段未起動の契約)")
	}

	// 副作用ゼロ契約検証: applyDef.Fn 未起動 = 全 fake 呼び出しゼロ。
	if len(agent.calls) != 0 {
		t.Errorf("scriptedProposeAgent.calls=%d, want 0 (apply 未起動)", len(agent.calls))
	}
	if fake.statusCalls != 0 || fake.snapshotCalls != 0 || fake.applyCheckCalls != 0 ||
		fake.applyCalls != 0 || fake.addCalls != 0 || fake.commitCalls != 0 || fake.revertCalls != 0 {
		t.Errorf("apply 未起動なのに fake が呼ばれた: status=%d snap=%d check=%d apply=%d add=%d commit=%d revert=%d",
			fake.statusCalls, fake.snapshotCalls, fake.applyCheckCalls,
			fake.applyCalls, fake.addCalls, fake.commitCalls, fake.revertCalls)
	}

	// `<cwd>/.rilll/review-apply-workflow/` 配下に何も書かれていないこと (apply 未起動 = result.json も無い)。
	applyRunDir := filepath.Join(cwd, ".rilll", "review-apply-workflow")
	if entries, err := os.ReadDir(applyRunDir); err == nil && len(entries) > 0 {
		t.Errorf("apply 未起動なのに %s 配下に entry=%d (副作用ゼロ違反)", applyRunDir, len(entries))
	}
	// 防御確認 (apply runID を applyRunIDOut 経由でなく FS 経由で取りに行ったときの動作確認):
	// findApplyRunIDFromFS は呼ばない (応答 0 件で test fail させる挙動になるため、本テストの責務外)。
	_ = findApplyRunIDFromFS

	// akashic 記録 (workflow_runs) 検証:
	//   - review: 1 件 start + 1 件 finish (finish.Error == precheckErrMsg)
	//   - apply: 0 件 start / 0 件 finish (Definition.Run 未起動)
	if len(rec.starts) != 1 {
		t.Errorf("recorder starts=%d, want 1 (review のみ起動)", len(rec.starts))
	}
	if len(rec.finishes) != 1 {
		t.Fatalf("recorder finishes=%d, want 1 (review のみ完了)", len(rec.finishes))
	}
	if rec.starts[0].WorkflowID != "review-workflow" {
		t.Errorf("starts[0].WorkflowID=%q, want %q", rec.starts[0].WorkflowID, "review-workflow")
	}
	reviewRunID := rec.starts[0].ID
	if reviewFinish, ok := findFinishByRunID(rec, reviewRunID); !ok {
		t.Errorf("review finish 記録が見つからない: runID=%q", reviewRunID)
	} else if reviewFinish.Error != precheckErrMsg {
		t.Errorf("review finish.Error=%q, want %q (内部失敗 → error 列に [verdict:precheck_failed] 記録)",
			reviewFinish.Error, precheckErrMsg)
	}
}
