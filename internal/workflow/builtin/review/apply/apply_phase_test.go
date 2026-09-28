// Package apply のテスト本ファイルは task 5.4 / design.md §Workflow §accept §commit の
// apply happy-path 統合 (Snapshot → ApplyCheck → Apply → commit-message → Add → Commit)
// と失敗系経路 (ApplyCheck 失敗 / Apply 失敗 / Commit 失敗 → revert 成功) を検証する
// (Req 5.1, 5.2, 5.3, 5.4, 5.5, 5.7)。
//
// 検証観点 (tasks.md 5.4 観察可能完了条件):
//   - applied happy: result.json.commit_sha が非空かつ final_verdict=applied、
//     baseline_commit が Snapshot 1 回目で取得した HEAD と一致。
//   - ApplyCheck 失敗: VerdictGitApplyFailed、Apply / Add / Commit / Revert は呼ばれない。
//   - Apply 失敗: VerdictGitApplyFailed、Add / Commit / Revert は呼ばれない (apply 自体が失敗のため revert 不要)。
//   - Commit 失敗 → revert 成功: VerdictGitApplyFailed、Revert(baseline, files) が 1 回呼ばれ、
//     baseline.HeadCommit が記録通り。Revert 失敗系 verdict 上書きは task 5.5 の責務。
//
// 副作用契約:
//   - applyHook は precheck 通過 + Loader 通過 + propose loop accept を経た後でのみ起動するため、
//     fakeApplyGitOpsExt.Status は precheck から 1 回呼ばれる (clean tree 検証)。
//   - applyHook 経路ではさらに Snapshot / ApplyCheck / Apply / Add / Commit / Revert が
//     経路ごとに決定的な回数呼ばれる (本ファイル各テストで assert)。
//
// 5.5 への seam:
//   - 中断 (ctx.Done()) 経路 / commit 失敗 → revert 失敗の verdict 上書き (`VerdictRevertFailed`)
//     は task 5.5 の責務であるため、本ファイルでは検証しない。
package apply

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	improvepatch "github.com/kazuki-oshino/rilll/internal/workflow/builtin/improve/patch"
)

// fakeApplyGitOpsExt は GitOpsExt interface を満たす test double。
// 全メソッドの呼び出し回数 / 引数 / 戻り値を構成可能にし、apply phase の副作用契約を機械検証する。
//
// 設計方針:
//   - Status: precheck から 1 回呼ばれる前提で statusClean=true (テスト caller がセット)。
//   - Snapshot: 順番に snapshotResults / snapshotErrs から取り出す (キュー方式)。
//     happy 経路では 2 回 (baseline → 新 HEAD) 呼ばれるため 2 件用意する。
//   - ApplyCheck / Apply / Add / Commit / Revert: 単発呼び出しを想定し、エラーは
//     applyCheckErr 等の単一フィールドで構成する (Snapshot のみ二段階のためキュー化)。
//   - afterApplyHook / afterAddHook / afterCommitHook: task 5.5 の cancel-aware revert
//     テスト用に副作用呼び出し直後に発火する optional callback。Apply 直後 / Add 直前 /
//     Commit 直前の ctx.Done() ガードをトリガするため、ここで親 ctx の cancel を発火する。
//     Add / Commit hook は「次の副作用呼び出し直前にガードを評価したい」という意味から
//     "after" でなく前段で fired される (Add hook は Apply 後 = Add 直前、Commit hook は
//     Add 後 = Commit 直前)。afterApplyHook のみ Apply 成功直後 (= Apply 直後ガードの前) に
//     発火する。helper の意味が混乱しないよう個別に名前付けする。
type fakeApplyGitOpsExt struct {
	// Status (precheck 用)。precheck.go が `improvepatch.ErrDirtyWorkingTree` を返すので
	// statusClean=true で clean tree を表現する。
	statusClean bool
	statusErr   error
	statusCalls int

	// Snapshot は apply hook で 1〜2 回呼ばれるためキュー。0 件目: baseline、1 件目: 新 HEAD。
	snapshotResults []improvepatch.BaselineRef
	snapshotErrs    []error
	snapshotCalls   int

	applyCheckErr   error
	applyCheckCalls int
	applyCheckDiff  []byte

	applyErr   error
	applyCalls int
	applyDiff  []byte
	// afterApplyHook は Apply の return 直前に発火する (Apply の result 自体は applyErr で決定)。
	// task 5.5 の「Apply 直後 ctx.Done() ガード」を triggering するためのフックで、テストケースが
	// ここで親 ctx を cancel すれば、後続の runOrCancel が select で <-ctx.Done() を観測する。
	afterApplyHook func()

	addErr   error
	addCalls int
	addArgv  [][]string
	// afterAddHook は Add の return 直前に発火する (Commit 直前ガードの triggering 用)。
	afterAddHook func()

	commitErr   error
	commitCalls int
	commitMsgs  []string
	commitFiles [][]string

	revertErr   error
	revertCalls int
	revertRef   improvepatch.BaselineRef
	revertFiles []string
	// revertCtxErr は Revert が呼ばれた瞬間の ctx.Err() の観測値を保存する。
	// task 5.5: 中断経路で Revert は **非 cancel 済 ctx** で起動しなければならないため、
	// Revert 受領 ctx が Done でないことを機械検証する用途。
	revertCtxErr error
}

func (f *fakeApplyGitOpsExt) Status(_ context.Context, _ string) (bool, error) {
	f.statusCalls++
	return f.statusClean, f.statusErr
}

func (f *fakeApplyGitOpsExt) Snapshot(_ context.Context, _ string) (improvepatch.BaselineRef, error) {
	idx := f.snapshotCalls
	f.snapshotCalls++
	var ref improvepatch.BaselineRef
	if idx < len(f.snapshotResults) {
		ref = f.snapshotResults[idx]
	}
	var err error
	if idx < len(f.snapshotErrs) {
		err = f.snapshotErrs[idx]
	}
	return ref, err
}

func (f *fakeApplyGitOpsExt) ApplyCheck(_ context.Context, _ string, diff []byte) error {
	f.applyCheckCalls++
	f.applyCheckDiff = append([]byte(nil), diff...)
	return f.applyCheckErr
}

func (f *fakeApplyGitOpsExt) Apply(_ context.Context, _ string, diff []byte) error {
	f.applyCalls++
	f.applyDiff = append([]byte(nil), diff...)
	if f.applyErr == nil && f.afterApplyHook != nil {
		f.afterApplyHook()
	}
	return f.applyErr
}

func (f *fakeApplyGitOpsExt) Add(_ context.Context, _ string, files []string) error {
	f.addCalls++
	f.addArgv = append(f.addArgv, append([]string(nil), files...))
	if f.addErr == nil && f.afterAddHook != nil {
		f.afterAddHook()
	}
	return f.addErr
}

func (f *fakeApplyGitOpsExt) Commit(_ context.Context, _ string, msg string, files []string) error {
	f.commitCalls++
	f.commitMsgs = append(f.commitMsgs, msg)
	f.commitFiles = append(f.commitFiles, append([]string(nil), files...))
	return f.commitErr
}

func (f *fakeApplyGitOpsExt) Revert(ctx context.Context, _ string, ref improvepatch.BaselineRef, files []string) error {
	f.revertCalls++
	f.revertRef = ref
	f.revertCtxErr = ctx.Err()
	f.revertFiles = append(f.revertFiles, files...)
	return f.revertErr
}

// 型レベル assertion: fakeApplyGitOpsExt が GitOpsExt interface を満たすことを compile time で保証。
var _ GitOpsExt = (*fakeApplyGitOpsExt)(nil)

// runApplyPhaseTestHarness は task 5.4 の apply hook を production と同等に wire し、
// scripted agent + fakeApplyGitOpsExt + 実 Loader を組み合わせて orchestrator を実行する helper。
//
// 戻り値:
//   - cwd: t.TempDir() 配下に作成された workflow 作業ディレクトリ。
//   - rec: result.json をパースした ResultRecord。
//   - events: 蓄積された WorkflowEvent。
//   - err: orchestrator の Fn 戻り値。
func runApplyPhaseTestHarness(
	t *testing.T,
	applyRunID string,
	agent *scriptedProposeAgent,
	fake *fakeApplyGitOpsExt,
) (cwd string, rec ResultRecord, events []workflow.WorkflowEvent, err error) {
	t.Helper()
	const reviewRunID = "review-apply-phase"
	cwd, ctx := proposeLoopTestEnv(t, applyRunID, reviewRunID)

	// precheck を通過させるため Status は clean を返す。テスト caller が statusClean を上書きしても
	// 既に false のときは尊重する (副作用観測の柔軟性)。
	if !fake.statusClean && fake.statusErr == nil {
		fake.statusClean = true
	}

	applyHook := newApplyHook(fake, agent)
	deps := applyWorkflowDeps{
		gitOps:    fake, // GitOpsExt は improvepatch.GitOps を embedding するため互換。
		load:      Load,
		agent:     agent,
		applyHook: applyHook,
		coreFn:    newProposeLoopCoreFn(fake, Load, agent, applyHook),
	}

	err = runApplyWorkflowOrchestration(ctx, cwd, recordingOnEvent(&events), &workflow.CLIAsk{}, deps)
	rec = readResultJSON(t, cwd, applyRunID)
	return cwd, rec, events, err
}

// commitMessageResp は commit-message LLM の応答 (markdown プレーンテキスト) を組み立てる helper。
func commitMessageResp(body string) provider.AgentResponse {
	return provider.AgentResponse{Content: body, Success: true}
}

// TestApplyPhase_AppliedHappy は accept された diff が Snapshot → ApplyCheck → Apply →
// commit-message → Add → Commit を全て成功で抜け、`VerdictApplied` (intent → Fn nil) と
// commit_sha / baseline_commit が記録されることを検証する (Req 5.1, 5.3, 5.4, 5.7)。
func TestApplyPhase_AppliedHappy(t *testing.T) {
	const baselineSHA = "0123456789abcdef0123456789abcdef01234567"
	const commitSHA = "fedcba9876543210fedcba9876543210fedcba98"
	const commitMessage = "fix(auth): tighten login guard\n\nreview_run_id: review-apply-phase\n"

	agent := &scriptedProposeAgent{responses: []provider.AgentResponse{
		planResp("apply happy plan"),
		diffResp(makePatchAgentJSON(t, proposeLoopAuthLoginDiff, []string{"auth/login.go"})),
		selfReviewResp(t, SelfReviewAccept, "適用してよい"),
		commitMessageResp(commitMessage),
	}}
	fake := &fakeApplyGitOpsExt{
		snapshotResults: []improvepatch.BaselineRef{
			{HeadCommit: baselineSHA},
			{HeadCommit: commitSHA},
		},
	}

	cwd, rec, _, err := runApplyPhaseTestHarness(t, "apply-happy", agent, fake)
	_ = cwd
	if err != nil {
		t.Fatalf("apply happy で error 非 nil: %v", err)
	}
	if rec.FinalVerdict != VerdictApplied {
		t.Fatalf("final_verdict=%q, want %q", rec.FinalVerdict, VerdictApplied)
	}
	if rec.CommitSHA != commitSHA {
		t.Fatalf("CommitSHA=%q, want %q", rec.CommitSHA, commitSHA)
	}
	if rec.BaselineCommit != baselineSHA {
		t.Fatalf("BaselineCommit=%q, want %q", rec.BaselineCommit, baselineSHA)
	}
	if rec.RecoveryGuidance != nil {
		t.Errorf("happy 経路で RecoveryGuidance 非 nil: %+v", rec.RecoveryGuidance)
	}
	if len(rec.Cycles) != 1 {
		t.Fatalf("len(Cycles)=%d, want 1", len(rec.Cycles))
	}
	if rec.Cycles[0].Verdict != VerdictApplied {
		t.Errorf("Cycles[0].Verdict=%q, want %q", rec.Cycles[0].Verdict, VerdictApplied)
	}

	// 副作用呼び出し回数 (precheck Status は 1 回 / apply hook 内部の各副作用は 1 回 / Snapshot は 2 回)。
	if fake.statusCalls != 1 {
		t.Errorf("Status calls=%d, want 1 (precheck)", fake.statusCalls)
	}
	if fake.snapshotCalls != 2 {
		t.Errorf("Snapshot calls=%d, want 2 (baseline + commit-after HEAD)", fake.snapshotCalls)
	}
	if fake.applyCheckCalls != 1 {
		t.Errorf("ApplyCheck calls=%d, want 1", fake.applyCheckCalls)
	}
	if fake.applyCalls != 1 {
		t.Errorf("Apply calls=%d, want 1", fake.applyCalls)
	}
	if fake.addCalls != 1 {
		t.Errorf("Add calls=%d, want 1", fake.addCalls)
	}
	if fake.commitCalls != 1 {
		t.Errorf("Commit calls=%d, want 1", fake.commitCalls)
	}
	if fake.revertCalls != 0 {
		t.Errorf("Revert calls=%d, want 0 (happy 経路)", fake.revertCalls)
	}

	// commit message に reviewRunID が含まれること (Req 5.4)。
	if len(fake.commitMsgs) != 1 {
		t.Fatalf("commitMsgs=%v, want 1 entry", fake.commitMsgs)
	}
	if !strings.Contains(fake.commitMsgs[0], "review-apply-phase") {
		t.Errorf("commit message に review run id (review-apply-phase) が含まれない: %q", fake.commitMsgs[0])
	}

	// Add / Commit に渡された files が patch.TargetFiles と一致すること。
	wantFiles := []string{"auth/login.go"}
	if !reflect.DeepEqual(fake.addArgv[0], wantFiles) {
		t.Errorf("Add files=%v, want %v", fake.addArgv[0], wantFiles)
	}
	if !reflect.DeepEqual(fake.commitFiles[0], wantFiles) {
		t.Errorf("Commit files=%v, want %v", fake.commitFiles[0], wantFiles)
	}

	// commit-message LLM が agent に投げられていること (4 番目の呼び出し)。
	if len(agent.calls) != 4 {
		t.Fatalf("agent calls=%d, want 4 (plan/diff/self-review/commit-message)", len(agent.calls))
	}
	commitCall := agent.calls[3]
	if !strings.Contains(commitCall.prompt, "review-apply-phase") {
		t.Errorf("commit-message prompt に review run id が含まれない: %q", commitCall.prompt)
	}
	if !strings.Contains(commitCall.prompt, "auth-rule") {
		t.Errorf("commit-message prompt に rule 名が含まれない: %q", commitCall.prompt)
	}
	if commitCall.options.StepID != "review-apply-commit-message" {
		t.Errorf("commit-message StepID=%q, want %q", commitCall.options.StepID, "review-apply-commit-message")
	}
}

// TestApplyPhase_ApplyCheckFailed は ApplyCheck 失敗で `VerdictGitApplyFailed` をセットし、
// Apply / Add / Commit / Revert が呼ばれないことを検証する (Req 5.2)。
// Snapshot で baseline は取得済みのため、result.json.baseline_commit には baseline SHA が記録される。
func TestApplyPhase_ApplyCheckFailed(t *testing.T) {
	const baselineSHA = "0123456789abcdef0123456789abcdef01234567"
	wantCheckErr := errors.New("simulated apply --check failure")

	agent := &scriptedProposeAgent{responses: []provider.AgentResponse{
		planResp("plan"),
		diffResp(makePatchAgentJSON(t, proposeLoopAuthLoginDiff, []string{"auth/login.go"})),
		selfReviewResp(t, SelfReviewAccept, "適用してよい"),
	}}
	fake := &fakeApplyGitOpsExt{
		snapshotResults: []improvepatch.BaselineRef{{HeadCommit: baselineSHA}},
		applyCheckErr:   wantCheckErr,
	}

	_, rec, _, err := runApplyPhaseTestHarness(t, "apply-check-fail", agent, fake)
	if err == nil {
		t.Fatalf("ApplyCheck 失敗で error が nil")
	}
	if !strings.HasPrefix(err.Error(), "[verdict:git_apply_failed]") {
		t.Errorf("error prefix mismatch: %q", err.Error())
	}
	if rec.FinalVerdict != VerdictGitApplyFailed {
		t.Errorf("final_verdict=%q, want %q", rec.FinalVerdict, VerdictGitApplyFailed)
	}
	if rec.BaselineCommit != baselineSHA {
		t.Errorf("BaselineCommit=%q, want %q (Snapshot は ApplyCheck より前で記録される)",
			rec.BaselineCommit, baselineSHA)
	}
	if rec.CommitSHA != "" {
		t.Errorf("CommitSHA=%q, want \"\" (commit 未到達)", rec.CommitSHA)
	}
	// Apply / Add / Commit / Revert が呼ばれないこと。
	if fake.applyCalls != 0 {
		t.Errorf("Apply calls=%d, want 0", fake.applyCalls)
	}
	if fake.addCalls != 0 {
		t.Errorf("Add calls=%d, want 0", fake.addCalls)
	}
	if fake.commitCalls != 0 {
		t.Errorf("Commit calls=%d, want 0", fake.commitCalls)
	}
	if fake.revertCalls != 0 {
		t.Errorf("Revert calls=%d, want 0 (Apply 未起動なので revert 不要)", fake.revertCalls)
	}
	// agent は plan / diff / self-review の 3 回のみ (commit-message 未起動)。
	if len(agent.calls) != 3 {
		t.Errorf("agent calls=%d, want 3 (commit-message 未起動)", len(agent.calls))
	}
}

// TestApplyPhase_ApplyFailed は Apply 失敗で `VerdictGitApplyFailed` をセットし、
// Add / Commit / Revert が呼ばれないことを検証する (Apply 自体が失敗のため revert 不要)。
func TestApplyPhase_ApplyFailed(t *testing.T) {
	const baselineSHA = "0123456789abcdef0123456789abcdef01234567"
	wantApplyErr := errors.New("simulated git apply failure")

	agent := &scriptedProposeAgent{responses: []provider.AgentResponse{
		planResp("plan"),
		diffResp(makePatchAgentJSON(t, proposeLoopAuthLoginDiff, []string{"auth/login.go"})),
		selfReviewResp(t, SelfReviewAccept, "適用してよい"),
	}}
	fake := &fakeApplyGitOpsExt{
		snapshotResults: []improvepatch.BaselineRef{{HeadCommit: baselineSHA}},
		applyErr:        wantApplyErr,
	}

	_, rec, _, err := runApplyPhaseTestHarness(t, "apply-apply-fail", agent, fake)
	if err == nil {
		t.Fatalf("Apply 失敗で error が nil")
	}
	if !strings.HasPrefix(err.Error(), "[verdict:git_apply_failed]") {
		t.Errorf("error prefix mismatch: %q", err.Error())
	}
	if rec.FinalVerdict != VerdictGitApplyFailed {
		t.Errorf("final_verdict=%q, want %q", rec.FinalVerdict, VerdictGitApplyFailed)
	}
	if rec.BaselineCommit != baselineSHA {
		t.Errorf("BaselineCommit=%q, want %q", rec.BaselineCommit, baselineSHA)
	}
	if rec.CommitSHA != "" {
		t.Errorf("CommitSHA=%q, want \"\"", rec.CommitSHA)
	}
	if fake.applyCheckCalls != 1 {
		t.Errorf("ApplyCheck calls=%d, want 1", fake.applyCheckCalls)
	}
	if fake.applyCalls != 1 {
		t.Errorf("Apply calls=%d, want 1", fake.applyCalls)
	}
	if fake.addCalls != 0 {
		t.Errorf("Add calls=%d, want 0", fake.addCalls)
	}
	if fake.commitCalls != 0 {
		t.Errorf("Commit calls=%d, want 0", fake.commitCalls)
	}
	if fake.revertCalls != 0 {
		t.Errorf("Revert calls=%d, want 0 (Apply 自体が失敗したため revert 不要)", fake.revertCalls)
	}
	// commit-message 未起動。
	if len(agent.calls) != 3 {
		t.Errorf("agent calls=%d, want 3 (commit-message 未起動)", len(agent.calls))
	}
}

// TestApplyPhase_CommitFailedRevertSuccess は git commit が失敗 → Revert(baseline, files) が
// 1 回呼ばれて成功し、`VerdictGitApplyFailed` がセットされることを検証する (Req 5.5)。
// Revert 失敗系の verdict 上書きは task 5.5 の責務 (本テスト範囲外)。
func TestApplyPhase_CommitFailedRevertSuccess(t *testing.T) {
	const baselineSHA = "0123456789abcdef0123456789abcdef01234567"
	wantCommitErr := errors.New("simulated git commit failure")
	commitMessage := "fix(auth): tighten login guard\n\nreview_run_id: review-apply-phase\n"

	agent := &scriptedProposeAgent{responses: []provider.AgentResponse{
		planResp("plan"),
		diffResp(makePatchAgentJSON(t, proposeLoopAuthLoginDiff, []string{"auth/login.go"})),
		selfReviewResp(t, SelfReviewAccept, "適用してよい"),
		commitMessageResp(commitMessage),
	}}
	fake := &fakeApplyGitOpsExt{
		snapshotResults: []improvepatch.BaselineRef{{HeadCommit: baselineSHA}},
		commitErr:       wantCommitErr,
	}

	_, rec, _, err := runApplyPhaseTestHarness(t, "apply-commit-fail-revert-ok", agent, fake)
	if err == nil {
		t.Fatalf("Commit 失敗 → revert 成功 で error が nil")
	}
	if !strings.HasPrefix(err.Error(), "[verdict:git_apply_failed]") {
		t.Errorf("error prefix mismatch: %q (5.4: revert 成功時は git_apply_failed)", err.Error())
	}
	if rec.FinalVerdict != VerdictGitApplyFailed {
		t.Errorf("final_verdict=%q, want %q", rec.FinalVerdict, VerdictGitApplyFailed)
	}
	if rec.BaselineCommit != baselineSHA {
		t.Errorf("BaselineCommit=%q, want %q", rec.BaselineCommit, baselineSHA)
	}
	if rec.CommitSHA != "" {
		t.Errorf("CommitSHA=%q, want \"\" (commit 失敗のため空)", rec.CommitSHA)
	}
	// 副作用の呼び出し: ApplyCheck=1, Apply=1, Add=1, Commit=1, Revert=1。
	if fake.applyCheckCalls != 1 {
		t.Errorf("ApplyCheck calls=%d, want 1", fake.applyCheckCalls)
	}
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
		t.Errorf("Revert calls=%d, want 1 (commit 失敗 → revert)", fake.revertCalls)
	}
	if fake.revertRef.HeadCommit != baselineSHA {
		t.Errorf("Revert ref=%q, want %q", fake.revertRef.HeadCommit, baselineSHA)
	}
	if !reflect.DeepEqual(fake.revertFiles, []string{"auth/login.go"}) {
		t.Errorf("Revert files=%v, want [auth/login.go]", fake.revertFiles)
	}
	// commit-message 起動済 (4 回目の agent 呼び出し)。
	if len(agent.calls) != 4 {
		t.Errorf("agent calls=%d, want 4 (plan/diff/self-review/commit-message)", len(agent.calls))
	}
}
