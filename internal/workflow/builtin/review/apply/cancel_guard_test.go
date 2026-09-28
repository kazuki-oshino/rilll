// Package apply のテスト本ファイルは task 5.5 / design.md §Workflow §中断 §commit 失敗 §revert
// 失敗 / Req 5.6, 6.1, 6.2, 6.3, 6.4 を検証する。
//
// 本タスクで確定する verdict:
//   - VerdictUserAborted (中断 + revert 成功): IsIntent=true → Fn は nil 返却。
//   - VerdictRevertFailed (中断 revert 失敗 / commit 失敗 + revert 失敗): IsIntent=false →
//     Fn は `[verdict:revert_failed] ...` を返す。
//
// 5.4 が VerdictGitApplyFailed をセット済の経路でも Revert が失敗した場合は
// VerdictRevertFailed で上書きされる契約 (本タスクの責務範囲)。
//
// 検証観点 (tasks.md 5.5 観察可能完了条件):
//   - user_aborted: Fn=nil + RecoveryGuidance=nil + Revert(baseline, files) 1 回呼び出し。
//   - 中断 + revert 失敗: Fn=`[verdict:revert_failed] ...` + RecoveryGuidance.Commands に
//     `git checkout <baseline> -- <file>` が含まれる。
//   - commit 失敗 + revert 失敗: 5.4 の VerdictGitApplyFailed を VerdictRevertFailed で上書き、
//     RecoveryGuidance も同様に commit 失敗経路で生成される。
//   - commit 失敗 + revert 成功: 5.4 既存挙動 (VerdictGitApplyFailed) を保つ (本ファイルでは
//     既存 TestApplyPhase_CommitFailedRevertSuccess が anchor)。
//
// Revert 起動時の ctx 契約:
//   - cancel 経路でも Revert は **非 cancel 済 ctx** で呼ばれること (revertCtxErr==nil で機械検証)。
//     これにより Revert 自身が ctx.Done() を観測して即時失敗する経路を構造的に塞ぐ。
package apply

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	improvepatch "github.com/kazuki-oshino/rilll/internal/workflow/builtin/improve/patch"
)

// runApplyPhaseCancelHarness は task 5.5 の cancel-aware revert テスト用 harness。
// `runApplyPhaseTestHarness` と同等の wiring を行うが、親 ctx を `context.WithCancel` で
// ラップして cancel 関数を返すため、テストは fake のフックから cancel を発火できる。
func runApplyPhaseCancelHarness(
	t *testing.T,
	applyRunID string,
	agent *scriptedProposeAgent,
	fake *fakeApplyGitOpsExt,
	wireFakeHooks func(cancel context.CancelFunc),
) (cwd string, rec ResultRecord, events []workflow.WorkflowEvent, err error) {
	t.Helper()
	const reviewRunID = "review-apply-phase-cancel"
	cwd, parentCtx := proposeLoopTestEnv(t, applyRunID, reviewRunID)
	ctx, cancel := context.WithCancel(parentCtx)
	t.Cleanup(cancel) // ガード: テスト経路全てで goroutine リークを防ぐ。

	if !fake.statusClean && fake.statusErr == nil {
		fake.statusClean = true
	}

	// テスト caller がフック (afterApplyHook 等) と cancel を結びつける。
	if wireFakeHooks != nil {
		wireFakeHooks(cancel)
	}

	applyHook := newApplyHook(fake, agent)
	deps := applyWorkflowDeps{
		gitOps:    fake,
		load:      Load,
		agent:     agent,
		applyHook: applyHook,
		coreFn:    newProposeLoopCoreFn(fake, Load, agent, applyHook),
	}

	err = runApplyWorkflowOrchestration(ctx, cwd, recordingOnEvent(&events), &workflow.CLIAsk{}, deps)
	rec = readResultJSON(t, cwd, applyRunID)
	return cwd, rec, events, err
}

// proposeAcceptResponses は plan / diff / self-review (accept) の 3 件の agent response を返す。
// commit-message 応答は呼び出し側で append する (cancel タイミングにより到達 / 非到達が変わるため)。
func proposeAcceptResponses(t *testing.T) []provider.AgentResponse {
	t.Helper()
	return []provider.AgentResponse{
		planResp("apply cancel plan"),
		diffResp(makePatchAgentJSON(t, proposeLoopAuthLoginDiff, []string{"auth/login.go"})),
		selfReviewResp(t, SelfReviewAccept, "適用してよい"),
	}
}

// TestApplyPhase_UserAbortedAfterApply は Apply 直後に ctx.Cancel が発火した経路で
// `VerdictUserAborted` (intent → Fn nil) + `RecoveryGuidance==nil` + Revert が
// 非 cancel 済 ctx で 1 回呼ばれることを検証する (Req 6.1, 6.3, 6.4)。
//
// シナリオ: Apply 成功 → afterApplyHook が cancel 発火 → 直後の ctx.Done() ガードが検知 →
// Revert(baseline, files) を background ctx で起動 → revert 成功 → user_aborted。
func TestApplyPhase_UserAbortedAfterApply(t *testing.T) {
	const baselineSHA = "0123456789abcdef0123456789abcdef01234567"

	agent := &scriptedProposeAgent{responses: proposeAcceptResponses(t)}
	fake := &fakeApplyGitOpsExt{
		snapshotResults: []improvepatch.BaselineRef{{HeadCommit: baselineSHA}},
		// revertErr=nil → revert 成功 → user_aborted。
	}

	_, rec, _, err := runApplyPhaseCancelHarness(t, "apply-user-aborted", agent, fake,
		func(cancel context.CancelFunc) {
			fake.afterApplyHook = func() { cancel() }
		},
	)

	// user_aborted は intent verdict のため Fn は nil を返す。
	if err != nil {
		t.Fatalf("user_aborted (intent) で Fn が error 返却: %v", err)
	}
	if rec.FinalVerdict != VerdictUserAborted {
		t.Fatalf("final_verdict=%q, want %q", rec.FinalVerdict, VerdictUserAborted)
	}
	if rec.RecoveryGuidance != nil {
		t.Errorf("user_aborted (revert 成功) で RecoveryGuidance 非 nil: %+v", rec.RecoveryGuidance)
	}
	if rec.BaselineCommit != baselineSHA {
		t.Errorf("BaselineCommit=%q, want %q", rec.BaselineCommit, baselineSHA)
	}
	if rec.CommitSHA != "" {
		t.Errorf("CommitSHA=%q, want \"\" (commit 未到達)", rec.CommitSHA)
	}

	// 副作用呼び出し回数: ApplyCheck=1, Apply=1, Add=0, Commit=0, Revert=1。
	if fake.applyCheckCalls != 1 {
		t.Errorf("ApplyCheck calls=%d, want 1", fake.applyCheckCalls)
	}
	if fake.applyCalls != 1 {
		t.Errorf("Apply calls=%d, want 1", fake.applyCalls)
	}
	if fake.addCalls != 0 {
		t.Errorf("Add calls=%d, want 0 (Apply 直後 cancel)", fake.addCalls)
	}
	if fake.commitCalls != 0 {
		t.Errorf("Commit calls=%d, want 0 (Apply 直後 cancel)", fake.commitCalls)
	}
	if fake.revertCalls != 1 {
		t.Fatalf("Revert calls=%d, want 1 (中断 → revert)", fake.revertCalls)
	}
	if fake.revertRef.HeadCommit != baselineSHA {
		t.Errorf("Revert ref=%q, want %q", fake.revertRef.HeadCommit, baselineSHA)
	}
	// Revert は **非 cancel 済 ctx** で呼ばれていること (Req 6.4 / cancel-aware の核)。
	if fake.revertCtxErr != nil {
		t.Errorf("Revert 受領 ctx.Err()=%v, want nil (Revert は cancel 済 ctx で呼ばないこと)", fake.revertCtxErr)
	}

	// commit-message LLM は呼ばれない (Apply 直後 cancel のため)。
	if len(agent.calls) != 3 {
		t.Errorf("agent calls=%d, want 3 (plan/diff/self-review、commit-message 未起動)", len(agent.calls))
	}
}

// TestApplyPhase_UserAbortedRevertFailed は中断時の Revert 自体が失敗した経路で
// `VerdictRevertFailed` + RecoveryGuidance.Commands に `git checkout <baseline> -- <file>` が
// 含まれ、Fn が `[verdict:revert_failed] ...` を返すことを検証する (Req 5.6, 6.2)。
func TestApplyPhase_UserAbortedRevertFailed(t *testing.T) {
	const baselineSHA = "0123456789abcdef0123456789abcdef01234567"
	wantRevertErr := errors.New("simulated revert failure")

	agent := &scriptedProposeAgent{responses: proposeAcceptResponses(t)}
	fake := &fakeApplyGitOpsExt{
		snapshotResults: []improvepatch.BaselineRef{{HeadCommit: baselineSHA}},
		revertErr:       wantRevertErr,
	}

	_, rec, _, err := runApplyPhaseCancelHarness(t, "apply-user-revert-fail", agent, fake,
		func(cancel context.CancelFunc) {
			fake.afterApplyHook = func() { cancel() }
		},
	)

	if err == nil {
		t.Fatalf("中断 + revert 失敗で error が nil")
	}
	if !strings.HasPrefix(err.Error(), "[verdict:revert_failed]") {
		t.Errorf("error prefix=%q, want [verdict:revert_failed] ...", err.Error())
	}
	if rec.FinalVerdict != VerdictRevertFailed {
		t.Fatalf("final_verdict=%q, want %q", rec.FinalVerdict, VerdictRevertFailed)
	}
	if rec.RecoveryGuidance == nil {
		t.Fatalf("RecoveryGuidance が nil (revert 失敗で未復旧手順を生成すべき)")
	}
	// Files は patch.TargetFiles と一致。
	if len(rec.RecoveryGuidance.Files) != 1 || rec.RecoveryGuidance.Files[0] != "auth/login.go" {
		t.Errorf("RecoveryGuidance.Files=%v, want [auth/login.go]", rec.RecoveryGuidance.Files)
	}
	// Commands に `git checkout <baseline> -- <file>` 形が含まれること。
	wantCommand := "git checkout " + baselineSHA + " -- auth/login.go"
	var found bool
	for _, c := range rec.RecoveryGuidance.Commands {
		if c == wantCommand {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("RecoveryGuidance.Commands=%v, want に %q を含む", rec.RecoveryGuidance.Commands, wantCommand)
	}
	if rec.BaselineCommit != baselineSHA {
		t.Errorf("BaselineCommit=%q, want %q", rec.BaselineCommit, baselineSHA)
	}

	// Revert は 1 回呼ばれて失敗。
	if fake.revertCalls != 1 {
		t.Errorf("Revert calls=%d, want 1", fake.revertCalls)
	}
	if fake.revertCtxErr != nil {
		t.Errorf("Revert 受領 ctx.Err()=%v, want nil", fake.revertCtxErr)
	}
}

// TestApplyPhase_CommitFailedRevertFailed は commit 失敗かつ Revert も失敗した経路で
// 5.4 の `VerdictGitApplyFailed` を `VerdictRevertFailed` で上書きし、RecoveryGuidance を
// 生成し、Fn が `[verdict:revert_failed] ...` を返すことを検証する (Req 5.6)。
func TestApplyPhase_CommitFailedRevertFailed(t *testing.T) {
	const baselineSHA = "0123456789abcdef0123456789abcdef01234567"
	wantCommitErr := errors.New("simulated commit failure")
	wantRevertErr := errors.New("simulated revert failure")
	commitMessage := "fix(auth): commit message body\n\nreview_run_id: review-apply-phase-cancel\n"

	agent := &scriptedProposeAgent{responses: append(proposeAcceptResponses(t), commitMessageResp(commitMessage))}
	fake := &fakeApplyGitOpsExt{
		snapshotResults: []improvepatch.BaselineRef{{HeadCommit: baselineSHA}},
		commitErr:       wantCommitErr,
		revertErr:       wantRevertErr,
	}

	_, rec, _, err := runApplyPhaseCancelHarness(t, "apply-commit-revert-fail", agent, fake, nil)

	if err == nil {
		t.Fatalf("commit 失敗 + revert 失敗で error が nil")
	}
	if !strings.HasPrefix(err.Error(), "[verdict:revert_failed]") {
		t.Errorf("error prefix=%q, want [verdict:revert_failed] ... (5.4 の git_apply_failed を上書き)", err.Error())
	}
	if rec.FinalVerdict != VerdictRevertFailed {
		t.Fatalf("final_verdict=%q, want %q (5.4 git_apply_failed を 5.5 で上書き)",
			rec.FinalVerdict, VerdictRevertFailed)
	}
	if rec.RecoveryGuidance == nil {
		t.Fatalf("RecoveryGuidance が nil (commit 失敗 + revert 失敗で未復旧手順を生成すべき)")
	}
	if len(rec.RecoveryGuidance.Files) != 1 || rec.RecoveryGuidance.Files[0] != "auth/login.go" {
		t.Errorf("RecoveryGuidance.Files=%v, want [auth/login.go]", rec.RecoveryGuidance.Files)
	}
	wantCommand := "git checkout " + baselineSHA + " -- auth/login.go"
	var found bool
	for _, c := range rec.RecoveryGuidance.Commands {
		if c == wantCommand {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("RecoveryGuidance.Commands=%v, want に %q を含む", rec.RecoveryGuidance.Commands, wantCommand)
	}

	// 副作用呼び出し回数: 通常経路を抜けるため Apply=1, Add=1, Commit=1, Revert=1。
	if fake.applyCalls != 1 {
		t.Errorf("Apply calls=%d, want 1", fake.applyCalls)
	}
	if fake.addCalls != 1 {
		t.Errorf("Add calls=%d, want 1", fake.addCalls)
	}
	if fake.commitCalls != 1 {
		t.Errorf("Commit calls=%d, want 1", fake.commitCalls)
	}
	if fake.revertCalls != 1 {
		t.Errorf("Revert calls=%d, want 1", fake.revertCalls)
	}

	// commit-message LLM が呼ばれていること (4 回目)。
	if len(agent.calls) != 4 {
		t.Errorf("agent calls=%d, want 4 (plan/diff/self-review/commit-message)", len(agent.calls))
	}
}

// TestApplyPhase_UserAbortedBeforeCommit は Commit 直前 ctx.Done() ガードを検証する。
// シナリオ: Apply 成功 → commit-message → Add 成功 → afterAddHook が cancel 発火 →
// 直後の Commit 直前ガードがトリップ → Revert 起動 → revert 成功 → user_aborted。
//
// 本テストは「3 ガード point すべてが配線されていること」を確認する補完ケース。
// (Apply 直後 ガードは TestApplyPhase_UserAbortedAfterApply で、commit 失敗系は別テストで検証)
func TestApplyPhase_UserAbortedBeforeCommit(t *testing.T) {
	const baselineSHA = "0123456789abcdef0123456789abcdef01234567"
	commitMessage := "fix(auth): pre-commit cancel\n\nreview_run_id: review-apply-phase-cancel\n"

	agent := &scriptedProposeAgent{responses: append(proposeAcceptResponses(t), commitMessageResp(commitMessage))}
	fake := &fakeApplyGitOpsExt{
		snapshotResults: []improvepatch.BaselineRef{{HeadCommit: baselineSHA}},
	}

	_, rec, _, err := runApplyPhaseCancelHarness(t, "apply-pre-commit-cancel", agent, fake,
		func(cancel context.CancelFunc) {
			fake.afterAddHook = func() { cancel() }
		},
	)

	if err != nil {
		t.Fatalf("Commit 直前 cancel + revert 成功 (intent) で error 非 nil: %v", err)
	}
	if rec.FinalVerdict != VerdictUserAborted {
		t.Fatalf("final_verdict=%q, want %q", rec.FinalVerdict, VerdictUserAborted)
	}
	if rec.RecoveryGuidance != nil {
		t.Errorf("revert 成功で RecoveryGuidance 非 nil: %+v", rec.RecoveryGuidance)
	}
	// Add まで実行され Commit は呼ばれない。
	if fake.applyCalls != 1 {
		t.Errorf("Apply calls=%d, want 1", fake.applyCalls)
	}
	if fake.addCalls != 1 {
		t.Errorf("Add calls=%d, want 1 (Commit 直前 cancel のため Add は実行済)", fake.addCalls)
	}
	if fake.commitCalls != 0 {
		t.Errorf("Commit calls=%d, want 0 (Commit 直前で cancel)", fake.commitCalls)
	}
	if fake.revertCalls != 1 {
		t.Errorf("Revert calls=%d, want 1 (中断 revert)", fake.revertCalls)
	}
	if fake.revertCtxErr != nil {
		t.Errorf("Revert 受領 ctx.Err()=%v, want nil", fake.revertCtxErr)
	}
}
