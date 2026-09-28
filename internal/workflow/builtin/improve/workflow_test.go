// Package workflow のテスト本ファイルは ImproveWorkflow workflow.Definition と
// runImproveWorkflowOrchestration の precheck / propose ループ / review (3 択) +
// apply + test + record フェーズを検証する
// (tasks.md 11.1 / 11.2 / 11.3 / design.md §ImproveWorkflow /
// Req 1.1〜1.7, 2.1, 2.6, 3.1〜3.6, 4.1〜4.6, 5.5〜5.7, 5.9, 6.1〜6.5, 7.4)。
//
// 検証観点:
//   - workflow.Definition フィールドの基本契約 (ID / Label / Args / Recorder / Fn 非 nil)
//   - precheck の全 6 失敗分岐で VerdictPreconditionFailed verdict と
//     `[verdict:precondition_failed]` プレフィックス付き error が返ること
//   - 全 precheck 通過時に context 構築 + propose ループ (task 11.2) を実行する
//   - ContextBuilder.Build 失敗時 VerdictContextLoadFailed verdict を返す (Req 2.6)
//   - regenerate ループが MaxRegenerateAttempts まで回り、各 attempt の
//     proposal-N.diff artifact が登録され、上限到達で VerdictRegenerateLoopAborted (Req 3.1〜3.6, 4.5)
//   - review 3 択 (accept/reject/regenerate) + apply + go test + revert / 復旧手順案内
//     (Req 4.1〜4.6, 5.5〜5.7, 5.9 / task 11.3)
//   - 全 verdict 経路で result.json artifact が必ず登録される (Req 6.5 記録欠落禁止)
package improve

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/improve/patch"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// ----- 本ファイル専用 fake 実装 (improvepatch interface を満たす最小モック) -----

// fakeImproveGoMod は GoModInspector を満たす最小 fake。
// 構成された (modulePath, moduleRoot, err) を返し、呼び出し回数を記録する。
type fakeImproveGoMod struct {
	modulePath string
	moduleRoot string
	err        error
	callCount  int
}

func (f *fakeImproveGoMod) ModulePath(_ context.Context, _ string) (string, string, error) {
	f.callCount++
	if f.err != nil {
		return "", "", f.err
	}
	return f.modulePath, f.moduleRoot, nil
}

var _ patch.GoModInspector = (*fakeImproveGoMod)(nil)

// fakeImproveResolver は TargetResolver を満たす最小 fake。
// 構成された (resolved, err) を返し、呼び出し回数 + 受領 cwd を記録する
// (cwd には orchestration が moduleRoot を渡す契約を検証するため受領値を保持する)。
type fakeImproveResolver struct {
	resolved    string
	err         error
	callCount   int
	receivedCwd string
	receivedID  string
}

func (f *fakeImproveResolver) Resolve(_ context.Context, cwd string, workflowID string) (string, error) {
	f.callCount++
	f.receivedCwd = cwd
	f.receivedID = workflowID
	if f.err != nil {
		return "", f.err
	}
	return f.resolved, nil
}

var _ patch.TargetResolver = (*fakeImproveResolver)(nil)

// fakeImproveGit は GitOps を満たす最小 fake。
// 各メソッドの戻り値を構成可能にしつつ全 5 メソッドの呼び出し回数を記録する。
// precheck からは Snapshot / ApplyCheck / Apply / Revert が呼ばれない契約と、
// 11.3 accept 経路で順序通り (Snapshot → ApplyCheck → Apply → (test fail 時) Revert)
// に呼ばれる契約の双方を検証する用途。
type fakeImproveGit struct {
	statusClean bool
	statusErr   error

	// Snapshot 戻り値の構成。zero 値のときは空 BaselineRef を返す。
	snapshotResult patch.BaselineRef
	snapshotErr    error

	// ApplyCheck / Apply / Revert で返したい error (nil なら成功)。
	applyCheckErr error
	applyErr      error
	revertErr     error

	statusCalls     int
	snapshotCalls   int
	applyCheckCalls int
	applyCalls      int
	revertCalls     int

	receivedStatusCwd      string
	receivedSnapshotCwd    string
	receivedApplyCheckDiff []byte
	receivedApplyDiff      []byte
	receivedRevertRef      patch.BaselineRef
	receivedRevertTargets  []string
}

func (f *fakeImproveGit) Status(_ context.Context, cwd string) (bool, error) {
	f.statusCalls++
	f.receivedStatusCwd = cwd
	return f.statusClean, f.statusErr
}

func (f *fakeImproveGit) Snapshot(_ context.Context, cwd string) (patch.BaselineRef, error) {
	f.snapshotCalls++
	f.receivedSnapshotCwd = cwd
	if f.snapshotErr != nil {
		return patch.BaselineRef{}, f.snapshotErr
	}
	return f.snapshotResult, nil
}

func (f *fakeImproveGit) ApplyCheck(_ context.Context, _ string, diff []byte) error {
	f.applyCheckCalls++
	f.receivedApplyCheckDiff = append([]byte(nil), diff...)
	return f.applyCheckErr
}

func (f *fakeImproveGit) Apply(_ context.Context, _ string, diff []byte) error {
	f.applyCalls++
	f.receivedApplyDiff = append([]byte(nil), diff...)
	return f.applyErr
}

func (f *fakeImproveGit) Revert(_ context.Context, _ string, ref patch.BaselineRef, targets []string) error {
	f.revertCalls++
	f.receivedRevertRef = ref
	f.receivedRevertTargets = append([]string(nil), targets...)
	return f.revertErr
}

var _ patch.GitOps = (*fakeImproveGit)(nil)

// sideEffectCalls は副作用メソッド (Snapshot/ApplyCheck/Apply/Revert) の呼び出し合計を返す。
// precheck がこれらを一切呼ばないこと (== 0) を機械検証する用途。
func (f *fakeImproveGit) sideEffectCalls() int {
	return f.snapshotCalls + f.applyCheckCalls + f.applyCalls + f.revertCalls
}

// fakeImproveContextBuilder は patch.ContextBuilder を満たす最小 fake。
// task 11.2 の orchestration が ContextBuilder.Build を 1 度だけ呼ぶ契約を検証する用途。
//
// 既定で result に有効な PromptContext を返し、err を設定すると ErrContextLoad 経路を再現できる。
type fakeImproveContextBuilder struct {
	callCount      int
	receivedInputs []patch.BuildInput
	result         patch.PromptContext
	err            error
}

func (f *fakeImproveContextBuilder) Build(_ context.Context, in patch.BuildInput) (patch.PromptContext, error) {
	f.callCount++
	f.receivedInputs = append(f.receivedInputs, in)
	if f.err != nil {
		return patch.PromptContext{}, f.err
	}
	return f.result, nil
}

var _ patch.ContextBuilder = (*fakeImproveContextBuilder)(nil)

// fakeImprovePromptTemplate は patch.PromptTemplate を満たす最小 fake。
// orchestration が attempt / previousProposals / providerID をどう渡すかを観測する用途。
//
// previousProposals は呼び出し時点の slice をスナップショットとして保持する
// (orchestration 側で append が走り、参照だけ持つと最終形しか観測できないため)。
type fakeImprovePromptTemplate struct {
	callCount           int
	receivedAttempts    []int
	receivedPrevious    [][]patch.Patch
	receivedProviderIDs []string
	rendered            string
}

func (f *fakeImprovePromptTemplate) Render(_ context.Context, _ patch.PromptContext, attempt int, previous []patch.Patch, providerID string) string {
	f.callCount++
	f.receivedAttempts = append(f.receivedAttempts, attempt)
	cloned := make([]patch.Patch, len(previous))
	copy(cloned, previous)
	f.receivedPrevious = append(f.receivedPrevious, cloned)
	f.receivedProviderIDs = append(f.receivedProviderIDs, providerID)
	if f.rendered == "" {
		return "<test prompt>"
	}
	return f.rendered
}

var _ patch.PromptTemplate = (*fakeImprovePromptTemplate)(nil)

// fakeImproveGoTest は patch.GoTestRunner を満たす最小 fake。
// `Run` 呼び出し回数 + 受領 cwd を記録し、構成された (result, err) を返す。
// 11.3 accept 経路で apply 後に 1 回だけ呼ばれる契約を検証する用途。
type fakeImproveGoTest struct {
	result      patch.TestResult
	err         error
	callCount   int
	receivedCwd string
}

func (f *fakeImproveGoTest) Run(_ context.Context, cwd string) (patch.TestResult, error) {
	f.callCount++
	f.receivedCwd = cwd
	return f.result, f.err
}

var _ patch.GoTestRunner = (*fakeImproveGoTest)(nil)

// fakeImproveAsk は workflow.WorkflowAsk を満たす最小 fake。
// 構成された responses を順に返し、不足時は最後の応答を再利用する。
// workflow.SelectParams.Key / Items を観測して 11.3 review 3 択提示契約を検証する。
type fakeImproveAsk struct {
	responses       []fakeImproveAskResponse
	callCount       int
	receivedKeys    []string
	receivedItemIDs [][]string
	err             error // 全呼び出しでこの err を返す (構成された responses は無視)
}

type fakeImproveAskResponse struct {
	id string
	ok bool
}

func (f *fakeImproveAsk) Select(_ context.Context, params workflow.SelectParams) (string, bool, error) {
	f.callCount++
	f.receivedKeys = append(f.receivedKeys, params.Key)
	itemIDs := make([]string, 0, len(params.Items))
	for _, it := range params.Items {
		itemIDs = append(itemIDs, it.ID)
	}
	f.receivedItemIDs = append(f.receivedItemIDs, itemIDs)
	if f.err != nil {
		return "", false, f.err
	}
	if len(f.responses) == 0 {
		return "", false, nil
	}
	idx := f.callCount - 1
	if idx >= len(f.responses) {
		idx = len(f.responses) - 1
	}
	r := f.responses[idx]
	return r.id, r.ok, nil
}

var _ workflow.WorkflowAsk = (*fakeImproveAsk)(nil)

// fakeImproveAgent は test 専用 workflowctx.AgentCaller。各呼び出しで responses[i] を順に返す。
// responses が尽きたら最後の応答を再利用する (loop max まで安定して同応答が必要なケース対応)。
type fakeImproveAgent struct {
	responses       []provider.AgentResponse
	callCount       int
	receivedPrompts []string
	receivedCwds    []string
	receivedOptions []*workflowctx.CallAgentOptions
	recorder        akashic.Recorder
}

func (f *fakeImproveAgent) CallAgent(_ context.Context, prompt string, cwd string, _ provider.AgentEventCallback, _ string, options *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
	f.callCount++
	f.receivedPrompts = append(f.receivedPrompts, prompt)
	f.receivedCwds = append(f.receivedCwds, cwd)
	f.receivedOptions = append(f.receivedOptions, options)
	if len(f.responses) == 0 {
		return provider.AgentResponse{Success: true}, nil
	}
	idx := f.callCount - 1
	if idx >= len(f.responses) {
		idx = len(f.responses) - 1
	}
	return f.responses[idx], nil
}

func (f *fakeImproveAgent) AkashicRecorder() akashic.Recorder { return f.recorder }

var _ workflowctx.AgentCaller = (*fakeImproveAgent)(nil)
var _ workflowctx.AkashicRecorderProvider = (*fakeImproveAgent)(nil)

// ----- テスト helper -----

// successDeps は「全 precheck 通過 + propose ループも (default fakes で) 動作する」
// セットアップを返す helper。個別テストは本 helper の戻り値を起点に必要なフィールドだけ上書きする。
//
// task 11.2 で contextBuilder / promptTemplate を deps に追加したため、successDeps も
// それらの default fake を含めて返す (precheck-only テストの下位互換を維持しつつ propose ループも動く)。
func successDeps() (improveWorkflowDeps, *fakeImproveGoMod, *fakeImproveResolver, *fakeImproveGit) {
	gomod := &fakeImproveGoMod{
		modulePath: "github.com/kazuki-oshino/rilll",
		moduleRoot: "/abs/module/root",
	}
	resolver := &fakeImproveResolver{
		resolved: "/abs/module/root/internal/workflow/git_commit.go",
	}
	git := &fakeImproveGit{statusClean: true}
	cb := &fakeImproveContextBuilder{
		result: patch.PromptContext{
			TargetWorkflowID: "git-commit",
			TargetGoFile:     "/abs/module/root/internal/workflow/git_commit.go",
			CurrentSource:    "package workflow\n",
		},
	}
	pt := &fakeImprovePromptTemplate{}
	gotest := &fakeImproveGoTest{}
	return improveWorkflowDeps{
		gitOps:         git,
		goMod:          gomod,
		resolver:       resolver,
		contextBuilder: cb,
		promptTemplate: pt,
		gotest:         gotest,
	}, gomod, resolver, git
}

// proposeLoopFakes は 11.2 / 11.3 propose ループ + review + apply テストの全 fake を束ねる構造体。
// proposeLoopSetup から返され、テストは個別 fake を取り出して assertion に使う。
type proposeLoopFakes struct {
	gomod          *fakeImproveGoMod
	resolver       *fakeImproveResolver
	git            *fakeImproveGit
	contextBuilder *fakeImproveContextBuilder
	promptTemplate *fakeImprovePromptTemplate
	gotest         *fakeImproveGoTest
}

// proposeLoopSetup は 11.2 propose ループテスト用の標準セットアップを構築する。
// cwd には t.TempDir() を採用し、moduleRoot も同じ値に設定することで
// `.rilll/` ルート解決と workflow.RecordRunArtifact の path 正規化が成立する状態を作る。
//
// 返却:
//   - cwd: テスト用一時ディレクトリ (= moduleRoot = workflow.RunMetadata.CWD)
//   - runID: 固定 "test-run-1" (artifact 経路の一意性確保用)
//   - recorder: workflowRecorder (workflow.RecordRunArtifact 受信を観測する)
//   - ctx: handoff + workflow.RunMetadata + Recorder が注入された context
//   - deps: improveWorkflowDeps (全 5 fake をセット済)
//   - fakes: 個別 fake への参照集約
func proposeLoopSetup(t *testing.T) (string, string, *workflowRecorder, context.Context, improveWorkflowDeps, *proposeLoopFakes) {
	t.Helper()
	cwd := t.TempDir()
	runID := "test-run-1"
	recorder := &workflowRecorder{}

	targetFile := filepath.Join(cwd, "internal", "workflow", "git_commit.go")

	gomod := &fakeImproveGoMod{
		modulePath: "github.com/kazuki-oshino/rilll",
		moduleRoot: cwd,
	}
	resolver := &fakeImproveResolver{resolved: targetFile}
	git := &fakeImproveGit{statusClean: true}
	cb := &fakeImproveContextBuilder{
		result: patch.PromptContext{
			TargetWorkflowID: "git-commit",
			TargetGoFile:     targetFile,
			CurrentSource:    "package workflow\n",
		},
	}
	pt := &fakeImprovePromptTemplate{}
	gotest := &fakeImproveGoTest{}

	deps := improveWorkflowDeps{
		gitOps:         git,
		goMod:          gomod,
		resolver:       resolver,
		contextBuilder: cb,
		promptTemplate: pt,
		gotest:         gotest,
	}

	ctx := workflow.ContextWithHandoff(context.Background(), workflow.HandoffParams{
		TargetWorkflowID: "git-commit",
		RunIDs:           []string{"run-source-1"},
	})
	ctx = workflowctx.ContextWithRunMetadata(ctx, workflowctx.RunMetadata{
		WorkflowRunID: runID,
		WorkflowID:    improveWorkflowID,
		WorkflowName:  improveWorkflowLabel,
		CWD:           cwd,
	})
	ctx = workflow.ContextWithRecorder(ctx, recorder)

	fakes := &proposeLoopFakes{
		gomod: gomod, resolver: resolver, git: git,
		contextBuilder: cb, promptTemplate: pt, gotest: gotest,
	}
	return cwd, runID, recorder, ctx, deps, fakes
}

// validDiffFor は ExtractAndGuard が成功する形の最小 unified diff を構築する。
// targetFile は absolute clean path、cwd は moduleRoot 相当の絶対パス。
// rel = filepath.Rel(cwd, targetFile) を求めて `diff --git a/<rel> b/<rel>` 行を含む
// diff 本文を返す。
//
// extractDiffBody (improvepatch/patch.go) の生 text 経路 fallback が要求する
// ` ```diff\n...\n``` ` フェンスでラップして返す (codeBlockDiffRegex 一致)。
// JSON 形式は本テスト群では使わない (raw-text fallback の方が短く可読)。
func validDiffFor(t *testing.T, cwd string, targetFile string) string {
	t.Helper()
	rel, err := filepath.Rel(cwd, targetFile)
	if err != nil {
		t.Fatalf("filepath.Rel(%q, %q): %v", cwd, targetFile, err)
	}
	return "```diff\n" +
		"diff --git a/" + rel + " b/" + rel + "\n" +
		"--- a/" + rel + "\n" +
		"+++ b/" + rel + "\n" +
		"@@ -1,1 +1,1 @@\n" +
		"-old\n" +
		"+new\n" +
		"```\n"
}

// countArtifactKind は recorder.runArtifacts の中で Kind == kind の件数を返す。
func countArtifactKind(recorder *workflowRecorder, kind string) int {
	n := 0
	for _, a := range recorder.runArtifacts {
		if a.Kind == kind {
			n++
		}
	}
	return n
}

// validHandoffCtx は登録済み workflow ID を target にした有効な handoff を ctx に注入する。
// "git-commit" は Registry に常時登録されている既知 ID であり、registry lookup を確実に通過させる。
func validHandoffCtx() context.Context {
	return workflow.ContextWithHandoff(context.Background(), workflow.HandoffParams{
		TargetWorkflowID: "git-commit",
		RunIDs:           []string{"run-1"},
	})
}

// assertPreconditionFailedError は err が `[verdict:precondition_failed]` プレフィックス付き
// であることを検証する (Req 6.2 / 6.5 の error 載せ方契約)。
func assertPreconditionFailedError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("error が返されることを期待: got nil")
	}
	wantPrefix := "[verdict:" + string(patch.VerdictPreconditionFailed) + "]"
	if !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Errorf("error prefix: got %q, want HasPrefix %q", err.Error(), wantPrefix)
	}
}

// assertNoSideEffects は GitOps の副作用メソッド呼び出し数が 0 であることを検証する
// (Req 1.6 hard-fail / Wave 0 task 2.4 negative test と整合)。
func assertNoSideEffects(t *testing.T, git *fakeImproveGit) {
	t.Helper()
	if got := git.sideEffectCalls(); got != 0 {
		t.Errorf("Snapshot/ApplyCheck/Apply/Revert 合計呼び出し回数: got %d, want 0 (precheck から副作用は呼ばれない)", got)
	}
}

// ----- workflow.Definition フィールド契約 -----

// TestImproveWorkflow_DefinitionFields は ImproveWorkflow が返す workflow.Definition の
// 基本契約 (ID / Label / Args / Recorder / Fn 非 nil) を検証する。
func TestImproveWorkflow_DefinitionFields(t *testing.T) {
	def := Workflow(&fakeAgent{}, validRegistryFn())

	if def.ID != "improve-workflow" {
		t.Errorf("workflow.Definition.ID: got %q, want %q", def.ID, "improve-workflow")
	}
	if def.Label != "improve workflow" {
		t.Errorf("workflow.Definition.Label: got %q, want %q", def.Label, "improve workflow")
	}
	if len(def.Args) != 0 {
		t.Errorf("workflow.Definition.Args: got %d 件, want 0 件 (hand-off ctx 経由のため CLI フラグ無し)", len(def.Args))
	}
	if def.Fn == nil {
		t.Error("workflow.Definition.Fn が nil。orchestration が組み立てられていない")
	}
	// Recorder は agent が AkashicRecorder() で nil を返せば nil となる契約 (recorderFromAgent)。
	// fakeAgent の recorder フィールドはデフォルトで nil なので Recorder は nil を期待。
	if def.Recorder != nil {
		t.Errorf("workflow.Definition.Recorder: agent.AkashicRecorder() が nil のとき Recorder も nil を期待: got %#v", def.Recorder)
	}
}

// ----- precheck 失敗分岐 (全 6 種、すべて副作用ゼロ) -----

// TestImproveWorkflow_HandoffMissing は ctx に handoff が注入されていない場合
// VerdictPreconditionFailed が返ることを検証する (Req 1.1)。
func TestImproveWorkflow_HandoffMissing(t *testing.T) {
	deps, gomod, resolver, git := successDeps()
	// handoff を注入せずに直接呼び出す。
	err := runImproveWorkflowOrchestration(context.Background(), "/some/cwd", nil, nil, &fakeAgent{}, validRegistryFn(), deps)

	assertPreconditionFailedError(t, err)
	assertNoSideEffects(t, git)
	// handoff 不在で early return するため、後続コンポーネントは呼ばれない (順序契約)。
	if gomod.callCount != 0 {
		t.Errorf("handoff 不在時 GoModInspector は呼ばれない (順序契約): got %d", gomod.callCount)
	}
	if resolver.callCount != 0 {
		t.Errorf("handoff 不在時 TargetResolver は呼ばれない (順序契約): got %d", resolver.callCount)
	}
	if git.statusCalls != 0 {
		t.Errorf("handoff 不在時 GitOps.Status は呼ばれない (順序契約): got %d", git.statusCalls)
	}
}

// TestImproveWorkflow_RunIDsEmpty は handoff の RunIDs が空配列の場合
// VerdictPreconditionFailed が返ることを検証する (Req 1.2 / 1.3)。
func TestImproveWorkflow_RunIDsEmpty(t *testing.T) {
	deps, gomod, resolver, git := successDeps()
	ctx := workflow.ContextWithHandoff(context.Background(), workflow.HandoffParams{
		TargetWorkflowID: "git-commit",
		RunIDs:           []string{}, // 空配列
	})

	err := runImproveWorkflowOrchestration(ctx, "/some/cwd", nil, nil, &fakeAgent{}, validRegistryFn(), deps)

	assertPreconditionFailedError(t, err)
	assertNoSideEffects(t, git)
	// RunIDs 空で early return するため、後続コンポーネントは呼ばれない (順序契約)。
	if gomod.callCount != 0 {
		t.Errorf("RunIDs 空時 GoModInspector は呼ばれない (順序契約): got %d", gomod.callCount)
	}
	if resolver.callCount != 0 {
		t.Errorf("RunIDs 空時 TargetResolver は呼ばれない (順序契約): got %d", resolver.callCount)
	}
	if git.statusCalls != 0 {
		t.Errorf("RunIDs 空時 GitOps.Status は呼ばれない (順序契約): got %d", git.statusCalls)
	}
}

// TestImproveWorkflow_GoModMismatch は GoModInspector が error を返した場合
// (cwd identity 不一致 / go.mod 不在 / パース不能) で VerdictPreconditionFailed が返ることを検証する
// (Req 1.4 / 1.6 / Resolved §5)。
func TestImproveWorkflow_GoModMismatch(t *testing.T) {
	deps, gomod, resolver, git := successDeps()
	gomod.err = errors.New("module path mismatch (got=github.com/other/repo, want=github.com/kazuki-oshino/rilll)")
	gomod.modulePath = ""
	gomod.moduleRoot = ""

	err := runImproveWorkflowOrchestration(validHandoffCtx(), "/some/cwd", nil, nil, &fakeAgent{}, validRegistryFn(), deps)

	assertPreconditionFailedError(t, err)
	assertNoSideEffects(t, git)
	if gomod.callCount != 1 {
		t.Errorf("GoModInspector.ModulePath 呼び出し回数: got %d, want 1", gomod.callCount)
	}
	// gomod 失敗で early return するため、後続コンポーネントは呼ばれない (順序契約)。
	if resolver.callCount != 0 {
		t.Errorf("gomod 失敗時 TargetResolver は呼ばれない (順序契約): got %d", resolver.callCount)
	}
	if git.statusCalls != 0 {
		t.Errorf("gomod 失敗時 GitOps.Status は呼ばれない (順序契約): got %d", git.statusCalls)
	}
}

// TestImproveWorkflow_UnknownWorkflowID は handoff の TargetWorkflowID が
// Registry に登録されていない場合 VerdictPreconditionFailed が返ることを検証する (Req 1.4)。
func TestImproveWorkflow_UnknownWorkflowID(t *testing.T) {
	deps, gomod, resolver, git := successDeps()
	ctx := workflow.ContextWithHandoff(context.Background(), workflow.HandoffParams{
		TargetWorkflowID: "no-such-workflow-9999",
		RunIDs:           []string{"run-1"},
	})

	err := runImproveWorkflowOrchestration(ctx, "/some/cwd", nil, nil, &fakeAgent{}, validRegistryFn(), deps)

	assertPreconditionFailedError(t, err)
	assertNoSideEffects(t, git)
	// gomod は通過して呼ばれるが、registry lookup で失敗するため resolver / git status は呼ばれない。
	if gomod.callCount != 1 {
		t.Errorf("GoModInspector.ModulePath 呼び出し回数: got %d, want 1", gomod.callCount)
	}
	if resolver.callCount != 0 {
		t.Errorf("未知 workflow ID 時 TargetResolver は呼ばれない (順序契約): got %d", resolver.callCount)
	}
	if git.statusCalls != 0 {
		t.Errorf("未知 workflow ID 時 GitOps.Status は呼ばれない (順序契約): got %d", git.statusCalls)
	}
}

// TestImproveWorkflow_TargetUnresolved は TargetResolver が ErrTargetNotResolved を返した場合
// VerdictPreconditionFailed が返ることを検証する (Req 1.5 / 7.4)。
func TestImproveWorkflow_TargetUnresolved(t *testing.T) {
	deps, gomod, resolver, git := successDeps()
	resolver.err = patch.ErrTargetNotResolved
	resolver.resolved = ""

	err := runImproveWorkflowOrchestration(validHandoffCtx(), "/some/cwd", nil, nil, &fakeAgent{}, validRegistryFn(), deps)

	assertPreconditionFailedError(t, err)
	assertNoSideEffects(t, git)
	if gomod.callCount != 1 {
		t.Errorf("GoModInspector.ModulePath 呼び出し回数: got %d, want 1", gomod.callCount)
	}
	if resolver.callCount != 1 {
		t.Errorf("TargetResolver.Resolve 呼び出し回数: got %d, want 1", resolver.callCount)
	}
	// resolver 失敗で early return するため、git status は呼ばれない (順序契約)。
	if git.statusCalls != 0 {
		t.Errorf("resolver 失敗時 GitOps.Status は呼ばれない (順序契約): got %d", git.statusCalls)
	}
}

// TestImproveWorkflow_GitDirty は GitOps.Status が clean=false を返した場合
// VerdictPreconditionFailed が返ることを検証する (Req 1.6 / Resolved §7)。
func TestImproveWorkflow_GitDirty(t *testing.T) {
	deps, gomod, resolver, git := successDeps()
	git.statusClean = false

	err := runImproveWorkflowOrchestration(validHandoffCtx(), "/some/cwd", nil, nil, &fakeAgent{}, validRegistryFn(), deps)

	assertPreconditionFailedError(t, err)
	assertNoSideEffects(t, git) // Snapshot/ApplyCheck/Apply/Revert は依然 0
	if gomod.callCount != 1 {
		t.Errorf("GoModInspector.ModulePath 呼び出し回数: got %d, want 1", gomod.callCount)
	}
	if resolver.callCount != 1 {
		t.Errorf("TargetResolver.Resolve 呼び出し回数: got %d, want 1", resolver.callCount)
	}
	if git.statusCalls != 1 {
		t.Errorf("GitOps.Status 呼び出し回数: got %d, want 1", git.statusCalls)
	}
}

// TestImproveWorkflow_GitStatusError は GitOps.Status が error を返した場合
// (git バイナリ不在 / cwd 不正等) でも VerdictPreconditionFailed が返ることを検証する。
// dirty (clean=false) と error は同じ verdict にマップされる契約 (Resolved §7)。
func TestImproveWorkflow_GitStatusError(t *testing.T) {
	deps, _, _, git := successDeps()
	git.statusClean = false
	git.statusErr = errors.New("exec: git not found")

	err := runImproveWorkflowOrchestration(validHandoffCtx(), "/some/cwd", nil, nil, &fakeAgent{}, validRegistryFn(), deps)

	assertPreconditionFailedError(t, err)
	assertNoSideEffects(t, git)
	if git.statusCalls != 1 {
		t.Errorf("GitOps.Status 呼び出し回数: got %d, want 1", git.statusCalls)
	}
}

// ----- precheck 全段通過 (success path) -----

// TestImproveWorkflow_AllPrecheckPasses は 6 段の precheck がすべて通過した場合
// 11.2 で追加された context 構築 + propose ループに進み、最終的に
// VerdictRegenerateLoopAborted で終了することを検証する (Req 1.7 / Req 3.1〜3.6 / 4.5)。
//
// 本テストでは fakeAgent (空 resp.Content) を使うため ExtractAndGuard が ErrUnparseable で
// 全 attempt を discard し、loop が MaxRegenerateAttempts まで回って上限到達 verdict が返る。
// 副作用契約 (Snapshot/ApplyCheck/Apply/Revert == 0) は 11.2 でも維持される (apply は 11.3 で追加)。
func TestImproveWorkflow_AllPrecheckPasses(t *testing.T) {
	deps, gomod, resolver, git := successDeps()
	var events []workflow.WorkflowEvent
	onEvent := func(e workflow.WorkflowEvent) { events = append(events, e) }

	err := runImproveWorkflowOrchestration(validHandoffCtx(), "/some/cwd", onEvent, nil, &fakeAgent{}, validRegistryFn(), deps)
	// 11.2 では propose ループが MaxRegenerateAttempts まで回って verdict_regenerate_loop_aborted で終了する。
	if err == nil {
		t.Fatalf("全 precheck 通過後 propose ループで regenerate_loop_aborted を期待: got nil")
	}
	wantPrefix := "[verdict:" + string(patch.VerdictRegenerateLoopAborted) + "]"
	if !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Errorf("error prefix: got %q, want HasPrefix %q", err.Error(), wantPrefix)
	}

	// 副作用契約: 11.2 でも Snapshot/ApplyCheck/Apply/Revert は 0 (apply フェーズは 11.3)。
	assertNoSideEffects(t, git)

	// 順序契約: gomod / resolver / git status はそれぞれ 1 回呼ばれる。
	if gomod.callCount != 1 {
		t.Errorf("GoModInspector.ModulePath 呼び出し回数: got %d, want 1", gomod.callCount)
	}
	if resolver.callCount != 1 {
		t.Errorf("TargetResolver.Resolve 呼び出し回数: got %d, want 1", resolver.callCount)
	}
	if git.statusCalls != 1 {
		t.Errorf("GitOps.Status 呼び出し回数: got %d, want 1", git.statusCalls)
	}

	// cwd 伝播契約: resolver と git status の cwd には moduleRoot が渡される (cwd 正規化責務を呼び出し側に押し付けない)。
	if resolver.receivedCwd != "/abs/module/root" {
		t.Errorf("TargetResolver に渡された cwd: got %q, want %q (moduleRoot)", resolver.receivedCwd, "/abs/module/root")
	}
	if git.receivedStatusCwd != "/abs/module/root" {
		t.Errorf("GitOps.Status に渡された cwd: got %q, want %q (moduleRoot)", git.receivedStatusCwd, "/abs/module/root")
	}

	// resolver には handoff の TargetWorkflowID がそのまま渡される。
	if resolver.receivedID != "git-commit" {
		t.Errorf("TargetResolver に渡された workflowID: got %q, want %q", resolver.receivedID, "git-commit")
	}

	// Req 1.7: 起動受領を workflow.WorkflowText で notify する。
	foundNotify := false
	for _, e := range events {
		if e.Type == workflow.WorkflowText && strings.Contains(e.Text, "precheck passed") {
			foundNotify = true
			break
		}
	}
	if !foundNotify {
		t.Errorf("Req 1.7: precheck 通過後の workflow.WorkflowText 通知が見つからない: events=%#v", events)
	}
}

// TestImproveWorkflow_DefinitionRunIntegration は ImproveWorkflow().Run() 経由で
// CLI 単独起動 (handoff 注入なし) のケースが正しく VerdictPreconditionFailed として
// workflow.Definition.Run の workflow_runs.error 経路に流れることを検証する。
//
// workflow.Definition.Fn が直接エラーを返す経路 (本 spec の verdict 契約) と workflow.Definition.Run の
// 自動 finish 記録 (workflow_runs.error 格納) が連携することを確認する目的の統合テスト。
func TestImproveWorkflow_DefinitionRunIntegration_HandoffMissing(t *testing.T) {
	recorder := &workflowRecorder{}
	agent := &fakeAgent{recorder: recorder}

	def := Workflow(agent, validRegistryFn())
	// CLI 単独起動を模擬: handoff を ctx に注入せず Run を呼ぶ。
	err := def.Run(context.Background(), t.TempDir(), func(workflow.WorkflowEvent) {}, nil)

	assertPreconditionFailedError(t, err)

	// Recorder の finish 記録に verdict 付きエラーが格納される。
	if len(recorder.finishes) != 1 {
		t.Fatalf("workflow finish 記録が 1 件であることを期待: got %d (%#v)", len(recorder.finishes), recorder.finishes)
	}
	wantPrefix := "[verdict:" + string(patch.VerdictPreconditionFailed) + "]"
	if !strings.HasPrefix(recorder.finishes[0].Error, wantPrefix) {
		t.Errorf("workflow_runs.error prefix: got %q, want HasPrefix %q",
			recorder.finishes[0].Error, wantPrefix)
	}
}

// ----- 11.2: context 構築 + propose ループ -----

// TestImproveWorkflow_ContextLoadFailed は ContextBuilder.Build が ErrContextLoad を
// 返した場合 VerdictContextLoadFailed verdict 付き error が返り、後続の propose ループや
// artifact 登録に進まないことを検証する (Req 2.6)。
func TestImproveWorkflow_ContextLoadFailed(t *testing.T) {
	cwd, _, recorder, ctx, deps, fakes := proposeLoopSetup(t)

	// ContextBuilder を ErrContextLoad chain で失敗させる。
	fakes.contextBuilder.err = patch.ErrContextLoad

	agent := &fakeImproveAgent{}
	err := runImproveWorkflowOrchestration(ctx, cwd, nil, nil, agent, validRegistryFn(), deps)
	if err == nil {
		t.Fatalf("ErrContextLoad 経路では context_load_failed verdict 付き error を期待: got nil")
	}
	wantPrefix := "[verdict:" + string(patch.VerdictContextLoadFailed) + "]"
	if !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Errorf("error prefix: got %q, want HasPrefix %q", err.Error(), wantPrefix)
	}

	// 副作用契約: 11.2 / 11.3 のいずれの apply 系も呼ばれない。
	assertNoSideEffects(t, fakes.git)

	// ContextBuilder は 1 回だけ呼ばれる。
	if fakes.contextBuilder.callCount != 1 {
		t.Errorf("ContextBuilder.Build 呼び出し回数: got %d, want 1", fakes.contextBuilder.callCount)
	}

	// PromptTemplate / Agent は呼ばれない (early return)。
	if fakes.promptTemplate.callCount != 0 {
		t.Errorf("ContextLoadFailed 経路で PromptTemplate.Render は呼ばれない: got %d", fakes.promptTemplate.callCount)
	}
	if agent.callCount != 0 {
		t.Errorf("ContextLoadFailed 経路で workflowctx.AgentCaller.CallAgent は呼ばれない: got %d", agent.callCount)
	}

	// 11.3: 全 verdict 経路で result.json は必ず登録される (Req 6.5 記録欠落禁止)。
	// ContextLoadFailed 経路では context.json / proposal-N.diff は登録しないが、
	// defer 経路で result.json のみ 1 件登録される。
	if got, want := countArtifactKind(recorder, akashic.ArtifactKindResult), 1; got != want {
		t.Errorf("ContextLoadFailed 経路でも result.json artifact 登録件数: got %d, want %d", got, want)
	}
	if got := countArtifactKind(recorder, akashic.ArtifactKindContext); got != 0 {
		t.Errorf("ContextLoadFailed 経路で context artifact は登録しない: got %d", got)
	}
	if got := countArtifactKind(recorder, akashic.ArtifactKindProposal); got != 0 {
		t.Errorf("ContextLoadFailed 経路で proposal artifact は登録しない: got %d", got)
	}
	if got := countArtifactKind(recorder, akashic.ArtifactKindReview); got != 0 {
		t.Errorf("ContextLoadFailed 経路で review artifact は登録しない: got %d", got)
	}
}

// TestImproveWorkflow_RegenerateLoopAborted_ParseableLoop は AI 応答が常に
// 抽出可能な diff の場合でも 11.2 単独 (Select 未実装) では loop が
// MaxRegenerateAttempts まで回り、最終的に VerdictRegenerateLoopAborted で終了することを検証する。
//
// 観察可能完了条件 (tasks.md 11.2):
//   - regenerate ループが MaxRegenerateAttempts まで回る
//   - 各 attempt の proposal-N.diff artifact が確実に登録される
//   - 上限到達時 verdict が `regenerate_loop_aborted` になる
func TestImproveWorkflow_RegenerateLoopAborted_ParseableLoop(t *testing.T) {
	cwd, _, recorder, ctx, deps, fakes := proposeLoopSetup(t)

	diff := validDiffFor(t, cwd, fakes.contextBuilder.result.TargetGoFile)
	agent := &fakeImproveAgent{
		responses: []provider.AgentResponse{
			{Content: diff, Success: true, SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderClaude}},
		},
	}

	err := runImproveWorkflowOrchestration(ctx, cwd, nil, nil, agent, validRegistryFn(), deps)
	if err == nil {
		t.Fatalf("loop 上限到達後は regenerate_loop_aborted verdict を期待: got nil")
	}
	wantPrefix := "[verdict:" + string(patch.VerdictRegenerateLoopAborted) + "]"
	if !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Errorf("error prefix: got %q, want HasPrefix %q", err.Error(), wantPrefix)
	}

	// 副作用契約: 11.2 では Snapshot/ApplyCheck/Apply/Revert は呼ばれない。
	assertNoSideEffects(t, fakes.git)

	// loop は MaxRegenerateAttempts (default 5) 回 attempt を行う。
	if want, got := patch.DefaultMaxRegenerateAttempts, agent.callCount; got != want {
		t.Errorf("workflowctx.AgentCaller.CallAgent 呼び出し回数: got %d, want %d (MaxRegenerateAttempts)", got, want)
	}
	if want, got := patch.DefaultMaxRegenerateAttempts, fakes.promptTemplate.callCount; got != want {
		t.Errorf("PromptTemplate.Render 呼び出し回数: got %d, want %d", got, want)
	}

	// artifact 登録: context kind 1 件 + proposal kind MaxRegenerateAttempts 件。
	if got, want := countArtifactKind(recorder, akashic.ArtifactKindContext), 1; got != want {
		t.Errorf("context artifact 登録件数: got %d, want %d", got, want)
	}
	if got, want := countArtifactKind(recorder, akashic.ArtifactKindProposal), patch.DefaultMaxRegenerateAttempts; got != want {
		t.Errorf("proposal artifact 登録件数: got %d, want %d", got, want)
	}

	// workflowctx.AgentCaller には moduleRoot (= cwd in test setup) が渡される。
	for i, c := range agent.receivedCwds {
		if c != cwd {
			t.Errorf("CallAgent 第 %d 回の cwd: got %q, want %q (moduleRoot)", i+1, c, cwd)
		}
	}
}

// TestImproveWorkflow_RegenerateLoopAborted_AllUnparseable は AI 応答が抽出不能な場合でも
// loop が 5 回回り、各 attempt で proposal artifact が登録される (生 raw response 保存)
// ことを検証する (Req 3.4)。
func TestImproveWorkflow_RegenerateLoopAborted_AllUnparseable(t *testing.T) {
	cwd, _, recorder, ctx, deps, fakes := proposeLoopSetup(t)

	agent := &fakeImproveAgent{
		responses: []provider.AgentResponse{
			{Content: "I cannot generate a diff for this file.", Success: true},
		},
	}

	err := runImproveWorkflowOrchestration(ctx, cwd, nil, nil, agent, validRegistryFn(), deps)
	wantPrefix := "[verdict:" + string(patch.VerdictRegenerateLoopAborted) + "]"
	if err == nil || !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Fatalf("error: got %v, want HasPrefix %q", err, wantPrefix)
	}

	// loop 上限まで回る。
	if want, got := patch.DefaultMaxRegenerateAttempts, agent.callCount; got != want {
		t.Errorf("workflowctx.AgentCaller.CallAgent 呼び出し回数: got %d, want %d", got, want)
	}

	// 抽出不能でも proposal artifact は MaxRegenerateAttempts 件登録される (raw response 保存)。
	if got, want := countArtifactKind(recorder, akashic.ArtifactKindProposal), patch.DefaultMaxRegenerateAttempts; got != want {
		t.Errorf("proposal artifact 登録件数: got %d, want %d (unparseable でも raw response を残す)", got, want)
	}
	if got := countArtifactKind(recorder, akashic.ArtifactKindContext); got != 1 {
		t.Errorf("context artifact 登録件数: got %d, want 1", got)
	}

	assertNoSideEffects(t, fakes.git)
}

// TestImproveWorkflow_PreviousProposalsAccumulated は ExtractAndGuard が成功する
// (outcome=ok) 攻撃で previousProposals が attempt ごとに 1 増加することを検証する
// (Resolved Decisions §4 / Req 4.5)。
//
// PromptTemplate.Render の receivedPrevious は呼び出し時点の slice を clone して保持する設計。
// attempt=1 では空、attempt=2 では 1 件 (= 直前 ok の Patch)、... と growing する。
func TestImproveWorkflow_PreviousProposalsAccumulated(t *testing.T) {
	cwd, _, _, ctx, deps, fakes := proposeLoopSetup(t)

	diff := validDiffFor(t, cwd, fakes.contextBuilder.result.TargetGoFile)
	agent := &fakeImproveAgent{
		responses: []provider.AgentResponse{
			{Content: diff, Success: true},
		},
	}

	_ = runImproveWorkflowOrchestration(ctx, cwd, nil, nil, agent, validRegistryFn(), deps)

	// PromptTemplate は MaxRegenerateAttempts 回呼ばれる。
	if want, got := patch.DefaultMaxRegenerateAttempts, fakes.promptTemplate.callCount; got != want {
		t.Fatalf("PromptTemplate.Render 呼び出し回数: got %d, want %d", got, want)
	}

	// attempt 番号は 1 始まりで N まで連番。
	for i, attempt := range fakes.promptTemplate.receivedAttempts {
		if want := i + 1; attempt != want {
			t.Errorf("Render 第 %d 回の attempt: got %d, want %d (1-origin 連番)", i+1, attempt, want)
		}
	}

	// previousProposals は ok outcome の累積。全試行 ok のため第 N 回には N-1 件。
	for i, prev := range fakes.promptTemplate.receivedPrevious {
		if want := i; len(prev) != want {
			t.Errorf("Render 第 %d 回の previousProposals 件数: got %d, want %d (ok outcome 累積)",
				i+1, len(prev), want)
		}
	}
}

// TestImproveWorkflow_ProviderIDPropagation は providerID が response.SystemInfo.ProviderID から
// 次の attempt の Render に伝播することを検証する。
//
// attempt=1: providerID は "" (初回 response 受領前のため未確定、cursor-fallback prompt が使われる)
// attempt=2 以降: 直前 response の SystemInfo.ProviderID
func TestImproveWorkflow_ProviderIDPropagation(t *testing.T) {
	cwd, _, _, ctx, deps, fakes := proposeLoopSetup(t)

	diff := validDiffFor(t, cwd, fakes.contextBuilder.result.TargetGoFile)
	agent := &fakeImproveAgent{
		responses: []provider.AgentResponse{
			{Content: diff, Success: true, SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderClaude}},
			{Content: diff, Success: true, SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderClaude}},
			{Content: diff, Success: true, SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderClaude}},
			{Content: diff, Success: true, SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderClaude}},
			{Content: diff, Success: true, SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderClaude}},
		},
	}

	_ = runImproveWorkflowOrchestration(ctx, cwd, nil, nil, agent, validRegistryFn(), deps)

	if got := len(fakes.promptTemplate.receivedProviderIDs); got != patch.DefaultMaxRegenerateAttempts {
		t.Fatalf("Render 呼び出し回数: got %d, want %d", got, patch.DefaultMaxRegenerateAttempts)
	}

	// 第 1 回の providerID は "" (初回 response 受領前)。
	if got := fakes.promptTemplate.receivedProviderIDs[0]; got != "" {
		t.Errorf("Render 第 1 回の providerID: got %q, want %q (初回 response 受領前)", got, "")
	}
	// 第 2 回以降は前回 response の SystemInfo.ProviderID = "claude"。
	for i := 1; i < len(fakes.promptTemplate.receivedProviderIDs); i++ {
		if got, want := fakes.promptTemplate.receivedProviderIDs[i], string(provider.ProviderClaude); got != want {
			t.Errorf("Render 第 %d 回の providerID: got %q, want %q", i+1, got, want)
		}
	}
}

// TestImproveWorkflow_OutputFormatForKnownProviders は providerID が claude / codex のとき
// workflowctx.CallAgentOptions.OutputFormat に patch.JSONSchemaForDiff が設定されることを検証する
// (Provider Compatibility Matrix / Req 3.3)。
//
// 第 1 回は providerID 不明のため OutputFormat は nil。
// 第 2 回以降は前回 response の SystemInfo.ProviderID で決定する。
func TestImproveWorkflow_OutputFormatForKnownProviders(t *testing.T) {
	cwd, _, _, ctx, deps, fakes := proposeLoopSetup(t)

	diff := validDiffFor(t, cwd, fakes.contextBuilder.result.TargetGoFile)
	agent := &fakeImproveAgent{
		responses: []provider.AgentResponse{
			{Content: diff, Success: true, SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderClaude}},
		},
	}

	_ = runImproveWorkflowOrchestration(ctx, cwd, nil, nil, agent, validRegistryFn(), deps)

	if len(agent.receivedOptions) != patch.DefaultMaxRegenerateAttempts {
		t.Fatalf("workflowctx.CallAgentOptions 観測件数: got %d, want %d", len(agent.receivedOptions), patch.DefaultMaxRegenerateAttempts)
	}

	// 第 1 回: providerID 不明のため OutputFormat=nil
	if opts := agent.receivedOptions[0]; opts != nil && opts.OutputFormat != nil {
		t.Errorf("第 1 回 OutputFormat: got %#v, want nil (providerID 未確定)", opts.OutputFormat)
	}
	// 第 2 回以降: providerID=claude のため OutputFormat=JSONSchemaForDiff
	for i := 1; i < len(agent.receivedOptions); i++ {
		opts := agent.receivedOptions[i]
		if opts == nil || opts.OutputFormat == nil {
			t.Errorf("第 %d 回 OutputFormat: got nil, want JSONSchemaForDiff (providerID=claude)", i+1)
			continue
		}
		if opts.OutputFormat.Type != patch.JSONSchemaForDiff.Type {
			t.Errorf("第 %d 回 OutputFormat.Type: got %q, want %q",
				i+1, opts.OutputFormat.Type, patch.JSONSchemaForDiff.Type)
		}
	}

	// 第 2 回以降は step 名/ID も attempt 番号を含む (akashic 集計のため stepID をユニークに保つ)。
	for i, opts := range agent.receivedOptions {
		if opts == nil {
			t.Errorf("第 %d 回 workflowctx.CallAgentOptions が nil (stepID/Name 必須)", i+1)
			continue
		}
		if opts.StepID == "" || opts.StepName == "" {
			t.Errorf("第 %d 回 workflowctx.CallAgentOptions: StepID=%q StepName=%q (両方非空を期待)", i+1, opts.StepID, opts.StepName)
		}
	}
}

// TestImproveWorkflow_ContextBuilderInputPropagation は ContextBuilder.Build に渡される
// BuildInput が handoff + resolver の結果を正しく伝播していることを検証する (Req 2.1)。
func TestImproveWorkflow_ContextBuilderInputPropagation(t *testing.T) {
	cwd, _, _, ctx, deps, fakes := proposeLoopSetup(t)

	agent := &fakeImproveAgent{}
	_ = runImproveWorkflowOrchestration(ctx, cwd, nil, nil, agent, validRegistryFn(), deps)

	if fakes.contextBuilder.callCount != 1 {
		t.Fatalf("ContextBuilder.Build 呼び出し回数: got %d, want 1", fakes.contextBuilder.callCount)
	}
	in := fakes.contextBuilder.receivedInputs[0]
	if in.TargetWorkflowID != "git-commit" {
		t.Errorf("BuildInput.TargetWorkflowID: got %q, want %q", in.TargetWorkflowID, "git-commit")
	}
	if len(in.RunIDs) != 1 || in.RunIDs[0] != "run-source-1" {
		t.Errorf("BuildInput.RunIDs: got %#v, want [run-source-1]", in.RunIDs)
	}
	if in.TargetGoFile != fakes.resolver.resolved {
		t.Errorf("BuildInput.TargetGoFile: got %q, want %q (resolver 結果)", in.TargetGoFile, fakes.resolver.resolved)
	}
}

// ----- 11.3: review (3 択 Select) + apply + go test + revert -----

// proposeLoopSetupWithDiff は 11.3 review 経路テスト共通の起点 setup。
// 単一 ok diff を返す agent を構築し、ask 等は呼び出し側で構成する。
func proposeLoopSetupWithDiff(t *testing.T) (
	string, string, *workflowRecorder, context.Context,
	improveWorkflowDeps, *proposeLoopFakes, *fakeImproveAgent,
) {
	t.Helper()
	cwd, runID, recorder, ctx, deps, fakes := proposeLoopSetup(t)
	diff := validDiffFor(t, cwd, fakes.contextBuilder.result.TargetGoFile)
	agent := &fakeImproveAgent{
		responses: []provider.AgentResponse{
			{Content: diff, Success: true, SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderClaude}},
		},
	}
	return cwd, runID, recorder, ctx, deps, fakes, agent
}

// assertVerdictPrefix は err が `[verdict:<id>]` プレフィックス付きであることを検証する。
func assertVerdictPrefix(t *testing.T, err error, v patch.Verdict) {
	t.Helper()
	if err == nil {
		t.Fatalf("error が返ることを期待 (want verdict=%s): got nil", v)
	}
	wantPrefix := "[verdict:" + string(v) + "]"
	if !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Errorf("error prefix: got %q, want HasPrefix %q", err.Error(), wantPrefix)
	}
}

// assertResultJSONRecorded は result.json が 1 件だけ登録されていることを検証する
// (Req 6.5 記録欠落禁止 + 重複登録禁止)。
func assertResultJSONRecorded(t *testing.T, recorder *workflowRecorder) {
	t.Helper()
	if got, want := countArtifactKind(recorder, akashic.ArtifactKindResult), 1; got != want {
		t.Errorf("result.json artifact 登録件数: got %d, want %d (全 verdict 経路で 1 件のみ登録)", got, want)
	}
}

// TestImproveWorkflow_AcceptPath_TestPass は accept 選択 → Snapshot → ApplyCheck → Apply →
// go test pass → applied_passed (Fn nil) 経路を検証する (Req 4.3, 5.1, 5.2, 5.4, 5.5, 5.7)。
func TestImproveWorkflow_AcceptPath_TestPass(t *testing.T) {
	cwd, _, recorder, ctx, deps, fakes, agent := proposeLoopSetupWithDiff(t)

	fakes.git.snapshotResult = patch.BaselineRef{HeadCommit: "abcdef1234567890abcdef1234567890abcdef12"}
	fakes.gotest.result = patch.TestResult{ExitCode: 0, DurationMS: 42}
	ask := &fakeImproveAsk{
		responses: []fakeImproveAskResponse{{id: improveSelectAccept, ok: true}},
	}

	err := runImproveWorkflowOrchestration(ctx, cwd, nil, ask, agent, validRegistryFn(), deps)
	if err != nil {
		t.Fatalf("applied_passed は IsIntent=true のため Fn は nil を期待: got %v", err)
	}

	// 副作用順序契約: Snapshot → ApplyCheck → Apply → go test (Revert 不実行)。
	if fakes.git.snapshotCalls != 1 {
		t.Errorf("Snapshot 呼び出し回数: got %d, want 1", fakes.git.snapshotCalls)
	}
	if fakes.git.applyCheckCalls != 1 {
		t.Errorf("ApplyCheck 呼び出し回数: got %d, want 1", fakes.git.applyCheckCalls)
	}
	if fakes.git.applyCalls != 1 {
		t.Errorf("Apply 呼び出し回数: got %d, want 1", fakes.git.applyCalls)
	}
	if fakes.gotest.callCount != 1 {
		t.Errorf("GoTestRunner.Run 呼び出し回数: got %d, want 1", fakes.gotest.callCount)
	}
	if fakes.git.revertCalls != 0 {
		t.Errorf("test pass 経路で Revert は呼ばれない: got %d", fakes.git.revertCalls)
	}

	// review.json + result.json が登録される。context は 1 件、proposal は 1 件 (1 attempt で accept)。
	assertResultJSONRecorded(t, recorder)
	if got, want := countArtifactKind(recorder, akashic.ArtifactKindReview), 1; got != want {
		t.Errorf("review.json artifact 登録件数: got %d, want %d", got, want)
	}
	if got, want := countArtifactKind(recorder, akashic.ArtifactKindContext), 1; got != want {
		t.Errorf("context artifact 登録件数: got %d, want %d", got, want)
	}
	if got, want := countArtifactKind(recorder, akashic.ArtifactKindProposal), 1; got != want {
		t.Errorf("proposal artifact 登録件数: got %d, want %d", got, want)
	}

	// workflow.SelectParams.Key は CLIAsk 連携契約で `review` 固定。
	if len(ask.receivedKeys) != 1 || ask.receivedKeys[0] != improveSelectKey {
		t.Errorf("workflow.SelectParams.Key: got %#v, want [%q]", ask.receivedKeys, improveSelectKey)
	}
	// Items の ID は accept/reject/regenerate の 3 つを必ず提示する (Req 4.2)。
	if len(ask.receivedItemIDs) != 1 {
		t.Fatalf("ask.receivedItemIDs len: got %d, want 1", len(ask.receivedItemIDs))
	}
	gotIDs := ask.receivedItemIDs[0]
	wantIDs := []string{improveSelectAccept, improveSelectReject, improveSelectRegenerate}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("Select Items 件数: got %d, want %d", len(gotIDs), len(wantIDs))
	}
	for i, want := range wantIDs {
		if gotIDs[i] != want {
			t.Errorf("Select Items[%d].ID: got %q, want %q", i, gotIDs[i], want)
		}
	}
}

// TestImproveWorkflow_AcceptPath_TestFail_Reverted は accept → apply → go test 失敗 → Revert 成功
// → applied_test_failed_reverted 経路を検証する (Req 5.5, 5.6, 5.7)。
func TestImproveWorkflow_AcceptPath_TestFail_Reverted(t *testing.T) {
	cwd, _, recorder, ctx, deps, fakes, agent := proposeLoopSetupWithDiff(t)

	fakes.git.snapshotResult = patch.BaselineRef{HeadCommit: "1111111111111111111111111111111111111111"}
	// テスト失敗 (exec error なし、ExitCode != 0)。
	fakes.gotest.result = patch.TestResult{ExitCode: 1, DurationMS: 30}
	ask := &fakeImproveAsk{
		responses: []fakeImproveAskResponse{{id: improveSelectAccept, ok: true}},
	}

	err := runImproveWorkflowOrchestration(ctx, cwd, nil, ask, agent, validRegistryFn(), deps)
	assertVerdictPrefix(t, err, patch.VerdictAppliedTestFailedReverted)

	// Snapshot / ApplyCheck / Apply / go test / Revert すべて 1 回。
	if fakes.git.snapshotCalls != 1 || fakes.git.applyCheckCalls != 1 ||
		fakes.git.applyCalls != 1 || fakes.gotest.callCount != 1 ||
		fakes.git.revertCalls != 1 {
		t.Errorf("呼び出し回数 want=(1,1,1,1,1) got=(snapshot=%d apply-check=%d apply=%d gotest=%d revert=%d)",
			fakes.git.snapshotCalls, fakes.git.applyCheckCalls, fakes.git.applyCalls,
			fakes.gotest.callCount, fakes.git.revertCalls)
	}
	// Revert に渡される ref / targets が Snapshot 結果 + Patch.TargetFiles と一致する。
	if fakes.git.receivedRevertRef.HeadCommit != fakes.git.snapshotResult.HeadCommit {
		t.Errorf("Revert.ref.HeadCommit: got %q, want %q",
			fakes.git.receivedRevertRef.HeadCommit, fakes.git.snapshotResult.HeadCommit)
	}
	if len(fakes.git.receivedRevertTargets) == 0 {
		t.Errorf("Revert.targetFiles: got empty, want non-empty (Patch.TargetFiles)")
	}

	// review.json + result.json は登録される。
	assertResultJSONRecorded(t, recorder)
	if got, want := countArtifactKind(recorder, akashic.ArtifactKindReview), 1; got != want {
		t.Errorf("review.json artifact 登録件数: got %d, want %d", got, want)
	}
}

// TestImproveWorkflow_AcceptPath_ApplyCheckFail は accept → ApplyCheck 失敗 → git_apply_failed
// 経路を検証する。Apply / Revert は呼ばれない契約 (Req 5.2, 5.3)。
func TestImproveWorkflow_AcceptPath_ApplyCheckFail(t *testing.T) {
	cwd, _, recorder, ctx, deps, fakes, agent := proposeLoopSetupWithDiff(t)

	fakes.git.snapshotResult = patch.BaselineRef{HeadCommit: "2222222222222222222222222222222222222222"}
	fakes.git.applyCheckErr = errors.New("does not apply cleanly")
	ask := &fakeImproveAsk{
		responses: []fakeImproveAskResponse{{id: improveSelectAccept, ok: true}},
	}

	err := runImproveWorkflowOrchestration(ctx, cwd, nil, ask, agent, validRegistryFn(), deps)
	assertVerdictPrefix(t, err, patch.VerdictGitApplyFailed)

	// Snapshot + ApplyCheck だけが呼ばれる。Apply / go test / Revert は不実行。
	if fakes.git.snapshotCalls != 1 {
		t.Errorf("Snapshot 呼び出し回数: got %d, want 1", fakes.git.snapshotCalls)
	}
	if fakes.git.applyCheckCalls != 1 {
		t.Errorf("ApplyCheck 呼び出し回数: got %d, want 1", fakes.git.applyCheckCalls)
	}
	if fakes.git.applyCalls != 0 {
		t.Errorf("ApplyCheck 失敗時 Apply は呼ばれない: got %d", fakes.git.applyCalls)
	}
	if fakes.gotest.callCount != 0 {
		t.Errorf("ApplyCheck 失敗時 go test は呼ばれない: got %d", fakes.gotest.callCount)
	}
	if fakes.git.revertCalls != 0 {
		t.Errorf("ApplyCheck 失敗時 Revert は呼ばれない (Apply 未実行): got %d", fakes.git.revertCalls)
	}

	assertResultJSONRecorded(t, recorder)
	// review.json は accept 経路に入った時点で登録される。
	if got, want := countArtifactKind(recorder, akashic.ArtifactKindReview), 1; got != want {
		t.Errorf("review.json artifact 登録件数: got %d, want %d", got, want)
	}
}

// TestImproveWorkflow_AcceptPath_RevertFailed は accept → apply → test 失敗 → Revert 自体失敗
// → revert_failed verdict + 復旧手順 workflow.WorkflowText 経路を検証する (Req 5.9)。
func TestImproveWorkflow_AcceptPath_RevertFailed(t *testing.T) {
	cwd, _, recorder, ctx, deps, fakes, agent := proposeLoopSetupWithDiff(t)

	headCommit := "3333333333333333333333333333333333333333"
	fakes.git.snapshotResult = patch.BaselineRef{HeadCommit: headCommit}
	fakes.gotest.result = patch.TestResult{ExitCode: 1, DurationMS: 10}
	fakes.git.revertErr = patch.ErrRevertFailed
	ask := &fakeImproveAsk{
		responses: []fakeImproveAskResponse{{id: improveSelectAccept, ok: true}},
	}

	var events []workflow.WorkflowEvent
	onEvent := func(e workflow.WorkflowEvent) { events = append(events, e) }

	err := runImproveWorkflowOrchestration(ctx, cwd, onEvent, ask, agent, validRegistryFn(), deps)
	assertVerdictPrefix(t, err, patch.VerdictRevertFailed)

	// Revert は呼ばれた (失敗を返す)。
	if fakes.git.revertCalls != 1 {
		t.Errorf("Revert 呼び出し回数: got %d, want 1", fakes.git.revertCalls)
	}

	// 復旧手順 (`git checkout <head_commit> -- <file>`) を workflow.WorkflowText で案内する (Req 5.9)。
	foundGuidance := false
	for _, e := range events {
		if e.Type == workflow.WorkflowText &&
			strings.Contains(e.Text, "git checkout "+headCommit+" -- ") {
			foundGuidance = true
			break
		}
	}
	if !foundGuidance {
		t.Errorf("Req 5.9: revert 失敗時に `git checkout %s -- <file>` 形式の復旧手順を workflow.WorkflowText で案内すること: events=%#v",
			headCommit, events)
	}

	assertResultJSONRecorded(t, recorder)
}

// TestImproveWorkflow_AcceptPath_SnapshotFail は accept 選択直後の Snapshot 失敗 →
// git_apply_failed verdict 経路を検証する。ApplyCheck / Apply / Revert は不実行。
func TestImproveWorkflow_AcceptPath_SnapshotFail(t *testing.T) {
	cwd, _, recorder, ctx, deps, fakes, agent := proposeLoopSetupWithDiff(t)

	fakes.git.snapshotErr = errors.New("rev-parse failed")
	ask := &fakeImproveAsk{
		responses: []fakeImproveAskResponse{{id: improveSelectAccept, ok: true}},
	}

	err := runImproveWorkflowOrchestration(ctx, cwd, nil, ask, agent, validRegistryFn(), deps)
	assertVerdictPrefix(t, err, patch.VerdictGitApplyFailed)

	if fakes.git.snapshotCalls != 1 {
		t.Errorf("Snapshot 呼び出し回数: got %d, want 1", fakes.git.snapshotCalls)
	}
	if fakes.git.applyCheckCalls+fakes.git.applyCalls+fakes.git.revertCalls+fakes.gotest.callCount != 0 {
		t.Errorf("Snapshot 失敗時 後続フェーズは呼ばれない: apply-check=%d apply=%d revert=%d gotest=%d",
			fakes.git.applyCheckCalls, fakes.git.applyCalls, fakes.git.revertCalls, fakes.gotest.callCount)
	}
	assertResultJSONRecorded(t, recorder)
}

// TestImproveWorkflow_RejectPath は reject 選択 → user_rejected verdict (Fn nil) 経路を検証する
// (Req 4.4)。副作用 (Snapshot/Apply/Revert/go test) は一切呼ばれない。
func TestImproveWorkflow_RejectPath(t *testing.T) {
	cwd, _, recorder, ctx, deps, fakes, agent := proposeLoopSetupWithDiff(t)

	ask := &fakeImproveAsk{
		responses: []fakeImproveAskResponse{{id: improveSelectReject, ok: true}},
	}

	err := runImproveWorkflowOrchestration(ctx, cwd, nil, ask, agent, validRegistryFn(), deps)
	if err != nil {
		t.Fatalf("user_rejected は IsIntent=true のため Fn は nil を期待: got %v", err)
	}

	// 副作用は precheck の Status のみで Snapshot/ApplyCheck/Apply/Revert は 0。
	if fakes.git.snapshotCalls != 0 || fakes.git.applyCheckCalls != 0 ||
		fakes.git.applyCalls != 0 || fakes.git.revertCalls != 0 ||
		fakes.gotest.callCount != 0 {
		t.Errorf("reject 経路で apply 系副作用は呼ばれない: snapshot=%d apply-check=%d apply=%d revert=%d gotest=%d",
			fakes.git.snapshotCalls, fakes.git.applyCheckCalls, fakes.git.applyCalls,
			fakes.git.revertCalls, fakes.gotest.callCount)
	}

	// review.json + result.json は登録される。
	assertResultJSONRecorded(t, recorder)
	if got, want := countArtifactKind(recorder, akashic.ArtifactKindReview), 1; got != want {
		t.Errorf("review.json artifact 登録件数: got %d, want %d", got, want)
	}
}

// TestImproveWorkflow_RejectPath_ESCCancel は ask.Select が ok==false (ESC キャンセル) を返した場合
// reject 扱いとなり user_rejected verdict (Fn nil) を返すことを検証する。
func TestImproveWorkflow_RejectPath_ESCCancel(t *testing.T) {
	cwd, _, recorder, ctx, deps, _, agent := proposeLoopSetupWithDiff(t)

	ask := &fakeImproveAsk{
		responses: []fakeImproveAskResponse{{id: "", ok: false}}, // ESC キャンセル
	}

	err := runImproveWorkflowOrchestration(ctx, cwd, nil, ask, agent, validRegistryFn(), deps)
	if err != nil {
		t.Fatalf("ESC キャンセル → reject 扱い → user_rejected (IsIntent=true) のため Fn は nil を期待: got %v", err)
	}

	// review.json + result.json が登録される (reject と同じ経路)。
	assertResultJSONRecorded(t, recorder)
	if got, want := countArtifactKind(recorder, akashic.ArtifactKindReview), 1; got != want {
		t.Errorf("review.json artifact 登録件数: got %d, want %d", got, want)
	}
}

// TestImproveWorkflow_RegenerateThenAccept は regenerate → 次 attempt → accept 経路を検証する
// (Req 4.5 / 4.3)。proposal artifact が attempt 数だけ登録され、accept 後 apply が走る。
func TestImproveWorkflow_RegenerateThenAccept(t *testing.T) {
	cwd, _, recorder, ctx, deps, fakes := proposeLoopSetup(t)

	diff := validDiffFor(t, cwd, fakes.contextBuilder.result.TargetGoFile)
	agent := &fakeImproveAgent{
		responses: []provider.AgentResponse{
			{Content: diff, Success: true, SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderClaude}},
		},
	}
	fakes.git.snapshotResult = patch.BaselineRef{HeadCommit: "4444444444444444444444444444444444444444"}
	fakes.gotest.result = patch.TestResult{ExitCode: 0, DurationMS: 5}

	ask := &fakeImproveAsk{
		responses: []fakeImproveAskResponse{
			{id: improveSelectRegenerate, ok: true}, // attempt 1: 再生成
			{id: improveSelectAccept, ok: true},     // attempt 2: 採用
		},
	}

	err := runImproveWorkflowOrchestration(ctx, cwd, nil, ask, agent, validRegistryFn(), deps)
	if err != nil {
		t.Fatalf("attempt 2 で applied_passed → Fn nil 期待: got %v", err)
	}

	// agent / promptTemplate は 2 回呼ばれる。
	if agent.callCount != 2 {
		t.Errorf("CallAgent 呼び出し回数: got %d, want 2 (regenerate → accept)", agent.callCount)
	}
	if fakes.promptTemplate.callCount != 2 {
		t.Errorf("PromptTemplate.Render 呼び出し回数: got %d, want 2", fakes.promptTemplate.callCount)
	}

	// attempt 2 で previousProposals は 1 件 (attempt 1 の ok proposal が累積)。
	if got := len(fakes.promptTemplate.receivedPrevious); got != 2 {
		t.Fatalf("Render previousProposals 観測件数: got %d, want 2", got)
	}
	if got := len(fakes.promptTemplate.receivedPrevious[1]); got != 1 {
		t.Errorf("attempt 2 の previousProposals 件数: got %d, want 1 (attempt 1 ok 累積)", got)
	}

	// ask.Select は 2 回呼ばれる (各 ok attempt で 1 回ずつ)。
	if ask.callCount != 2 {
		t.Errorf("workflow.WorkflowAsk.Select 呼び出し回数: got %d, want 2", ask.callCount)
	}

	// proposal artifact は attempt 数だけ登録される。review.json + result.json も登録。
	if got, want := countArtifactKind(recorder, akashic.ArtifactKindProposal), 2; got != want {
		t.Errorf("proposal artifact 登録件数: got %d, want %d", got, want)
	}
	assertResultJSONRecorded(t, recorder)
	if got, want := countArtifactKind(recorder, akashic.ArtifactKindReview), 1; got != want {
		t.Errorf("review.json artifact 登録件数: got %d, want %d (accept 確定時に 1 回登録)", got, want)
	}

	// accept 経路 → Snapshot/ApplyCheck/Apply/go test が各 1 回呼ばれる。
	if fakes.git.snapshotCalls != 1 || fakes.git.applyCheckCalls != 1 ||
		fakes.git.applyCalls != 1 || fakes.gotest.callCount != 1 {
		t.Errorf("accept 経路の副作用呼び出し: snapshot=%d apply-check=%d apply=%d gotest=%d (各 1 回期待)",
			fakes.git.snapshotCalls, fakes.git.applyCheckCalls, fakes.git.applyCalls, fakes.gotest.callCount)
	}
}

// TestImproveWorkflow_AskSelectError は ask.Select 自体が error を返した場合
// precondition_failed verdict が返ることを検証する (ユーザ意思取得不能)。
func TestImproveWorkflow_AskSelectError(t *testing.T) {
	cwd, _, recorder, ctx, deps, _, agent := proposeLoopSetupWithDiff(t)

	ask := &fakeImproveAsk{
		err: errors.New("tty unavailable"),
	}

	err := runImproveWorkflowOrchestration(ctx, cwd, nil, ask, agent, validRegistryFn(), deps)
	assertVerdictPrefix(t, err, patch.VerdictPreconditionFailed)

	// result.json は記録される (Req 6.5)。
	assertResultJSONRecorded(t, recorder)
}

// TestImproveWorkflow_ResultJSONAlwaysRecorded は task 11.3 の中核契約
// 「全 verdict 経路で result.json が必ず登録される」を verdict ごとに走査して検証する
// (Req 6.5 記録欠落禁止)。10 verdict 全網羅は task 12.2 の責務だが、本テストでは
// 11.3 単独で踏める verdict 群 (precondition_failed / context_load_failed /
// regenerate_loop_aborted / git_apply_failed / applied_test_failed_reverted /
// revert_failed / user_rejected / applied_passed) を網羅する。
func TestImproveWorkflow_ResultJSONAlwaysRecorded(t *testing.T) {
	type scenario struct {
		name     string
		setup    func(t *testing.T, fakes *proposeLoopFakes) (workflow.WorkflowAsk, *fakeImproveAgent)
		verdict  patch.Verdict
		isIntent bool
	}

	// 各 scenario は同じ proposeLoopSetup を起点にし、setup で fakes を構成する。
	scenarios := []scenario{
		{
			name: "context_load_failed",
			setup: func(t *testing.T, fakes *proposeLoopFakes) (workflow.WorkflowAsk, *fakeImproveAgent) {
				fakes.contextBuilder.err = patch.ErrContextLoad
				return nil, &fakeImproveAgent{}
			},
			verdict: patch.VerdictContextLoadFailed,
		},
		{
			name: "regenerate_loop_aborted_with_nil_ask",
			setup: func(t *testing.T, fakes *proposeLoopFakes) (workflow.WorkflowAsk, *fakeImproveAgent) {
				return nil, &fakeImproveAgent{} // 空応答 → 全 attempt unparseable
			},
			verdict: patch.VerdictRegenerateLoopAborted,
		},
		{
			name: "user_rejected",
			setup: func(t *testing.T, fakes *proposeLoopFakes) (workflow.WorkflowAsk, *fakeImproveAgent) {
				return &fakeImproveAsk{
					responses: []fakeImproveAskResponse{{id: improveSelectReject, ok: true}},
				}, makeOkDiffAgent(t, fakes)
			},
			verdict:  patch.VerdictUserRejected,
			isIntent: true,
		},
		{
			name: "applied_passed",
			setup: func(t *testing.T, fakes *proposeLoopFakes) (workflow.WorkflowAsk, *fakeImproveAgent) {
				fakes.git.snapshotResult = patch.BaselineRef{HeadCommit: "5555555555555555555555555555555555555555"}
				fakes.gotest.result = patch.TestResult{ExitCode: 0}
				return &fakeImproveAsk{
					responses: []fakeImproveAskResponse{{id: improveSelectAccept, ok: true}},
				}, makeOkDiffAgent(t, fakes)
			},
			verdict:  patch.VerdictAppliedPassed,
			isIntent: true,
		},
		{
			name: "git_apply_failed",
			setup: func(t *testing.T, fakes *proposeLoopFakes) (workflow.WorkflowAsk, *fakeImproveAgent) {
				fakes.git.applyCheckErr = errors.New("apply-check failed")
				return &fakeImproveAsk{
					responses: []fakeImproveAskResponse{{id: improveSelectAccept, ok: true}},
				}, makeOkDiffAgent(t, fakes)
			},
			verdict: patch.VerdictGitApplyFailed,
		},
		{
			name: "applied_test_failed_reverted",
			setup: func(t *testing.T, fakes *proposeLoopFakes) (workflow.WorkflowAsk, *fakeImproveAgent) {
				fakes.git.snapshotResult = patch.BaselineRef{HeadCommit: "6666666666666666666666666666666666666666"}
				fakes.gotest.result = patch.TestResult{ExitCode: 1}
				return &fakeImproveAsk{
					responses: []fakeImproveAskResponse{{id: improveSelectAccept, ok: true}},
				}, makeOkDiffAgent(t, fakes)
			},
			verdict: patch.VerdictAppliedTestFailedReverted,
		},
		{
			name: "revert_failed",
			setup: func(t *testing.T, fakes *proposeLoopFakes) (workflow.WorkflowAsk, *fakeImproveAgent) {
				fakes.git.snapshotResult = patch.BaselineRef{HeadCommit: "7777777777777777777777777777777777777777"}
				fakes.gotest.result = patch.TestResult{ExitCode: 1}
				fakes.git.revertErr = patch.ErrRevertFailed
				return &fakeImproveAsk{
					responses: []fakeImproveAskResponse{{id: improveSelectAccept, ok: true}},
				}, makeOkDiffAgent(t, fakes)
			},
			verdict: patch.VerdictRevertFailed,
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			cwd, _, recorder, ctx, deps, fakes := proposeLoopSetup(t)
			ask, agent := sc.setup(t, fakes)

			err := runImproveWorkflowOrchestration(ctx, cwd, nil, ask, agent, validRegistryFn(), deps)

			if sc.isIntent {
				if err != nil {
					t.Fatalf("verdict=%s は IsIntent=true のため Fn nil 期待: got %v", sc.verdict, err)
				}
			} else {
				assertVerdictPrefix(t, err, sc.verdict)
			}
			assertResultJSONRecorded(t, recorder)
		})
	}
}

// makeOkDiffAgent は ok outcome を返す validDiff を 1 度返す fake agent を構築する helper。
func makeOkDiffAgent(t *testing.T, fakes *proposeLoopFakes) *fakeImproveAgent {
	t.Helper()
	cwd := filepath.Dir(filepath.Dir(filepath.Dir(fakes.contextBuilder.result.TargetGoFile))) // <cwd>/internal/workflow/git_commit.go → <cwd>
	diff := validDiffFor(t, cwd, fakes.contextBuilder.result.TargetGoFile)
	return &fakeImproveAgent{
		responses: []provider.AgentResponse{
			{Content: diff, Success: true, SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderClaude}},
		},
	}
}

// ----- 12.2: ImproveWorkflow.Fn の verdict 別統合テスト -----
//
// task 12.2 (tasks.md §Wave 6) の中核契約:
//   - improvepatch 全コンポーネントを fake で差し替え (real な GitOps fixture も別 fixture path で併用)
//   - 全 verdict 経路 (closed set 10 値全部) で result.json の中身と workflow.RecordRunArtifact 呼び出し回数
//     (context=1, proposal=N, review=1, result=1) を検証
//   - IsIntent=true (`applied_passed` / `user_rejected`) で Fn が nil、その他 8 値で
//     `[verdict:<id>] ...` プレフィックス付き error が返る
//
// closed set 10 値の踏み分け方針:
//   - 最終 verdict として確定する 8 値は `result.json.verdict` の値で観察する
//     (precondition_failed / context_load_failed / regenerate_loop_aborted / user_rejected /
//      applied_passed / git_apply_failed / applied_test_failed_reverted / revert_failed)
//   - `proposal_unparseable` / `out_of_scope_diff` は orchestration の最終 verdict にはならず
//     (continue で次 attempt → 上限到達で regenerate_loop_aborted に集約)、
//     `result.json.attempts[].outcome` の closed set 値として観察する (Req 3.4 / 3.5 / 3.6 / 6.4)。

// loadResultJSON は recorder に登録された kind=result の最初の RunArtifact から
// result.json を読み出し、ResultRecord に decode して返す helper。
//
// 12.2 の中核検証 (result.json の中身) で再利用するため、artifact 探索 + 読み出し +
// JSON decode の 3 ステップを 1 度の呼び出しに集約する。
func loadResultJSON(t *testing.T, recorder *workflowRecorder) patch.ResultRecord {
	t.Helper()
	for _, a := range recorder.runArtifacts {
		if a.Kind != akashic.ArtifactKindResult {
			continue
		}
		data, err := os.ReadFile(a.Path)
		if err != nil {
			t.Fatalf("read result.json (%q): %v", a.Path, err)
		}
		var rec patch.ResultRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			t.Fatalf("unmarshal result.json: %v\n%s", err, string(data))
		}
		return rec
	}
	t.Fatalf("result.json artifact not registered (recorder=%#v)", recorder.runArtifacts)
	return patch.ResultRecord{}
}

// unparseableContent は ExtractAndGuard を ErrUnparseable で discard させる素朴な AI 応答。
// JSON 構造でも ` ```diff ...``` ` フェンスでもないため両 fallback ともに失敗する。
const unparseableContent = "I cannot generate a diff for this file."

// outOfScopeDiff は ExtractAndGuard を ErrOutOfScope で discard させる diff。
// `diff --git a/<別 path> b/<別 path>` 形で expectedTarget と異なる path を指定する。
// orchestration の AttemptRecord.Outcome として `out_of_scope_diff` を踏ませる用途。
const outOfScopeDiff = "```diff\n" +
	"diff --git a/totally/other/path.go b/totally/other/path.go\n" +
	"--- a/totally/other/path.go\n" +
	"+++ b/totally/other/path.go\n" +
	"@@ -1,1 +1,1 @@\n" +
	"-old\n" +
	"+new\n" +
	"```\n"

// verdictExpectation は TestImproveWorkflow_AllVerdicts_ResultJSONContent の 1 行ぶんの
// 期待値テーブルを表す。各 scenario で result.json と workflow.RecordRunArtifact マトリクスを
// 同一構造で検証するために使う。
type verdictExpectation struct {
	name string

	// setup は scenario 固有の deps / agent / ask / cwd / ctx 構成を行う。
	// 戻り値: cwd, runID, recorder, ctx, deps, agent, ask, expectedTargetGoFile (空文字なら検証スキップ)
	setup func(t *testing.T) (
		cwd string, runID string, recorder *workflowRecorder, ctx context.Context,
		deps improveWorkflowDeps, agent *fakeImproveAgent, ask workflow.WorkflowAsk,
		expectedTargetGoFile string,
	)

	// expectedVerdict は result.json.verdict の期待値 (closed set 10 値のいずれか)。
	expectedVerdict patch.Verdict

	// isIntent は Fn が nil を返すべきかどうか (true=nil, false=ErrorPrefixForVerdict)。
	isIntent bool

	// expectedArtifactCounts は kind 別 RunArtifact 登録回数の期待値。
	// "context" / "proposal" / "review" / "result" の 4 つを指定する。
	expectedContextCount  int
	expectedProposalCount int
	expectedReviewCount   int
	expectedResultCount   int

	// expectBaselineRef / expectTestResult / expectDiffSummary は result.json の
	// 該当フィールドが非 nil であるべきかを示す (apply 経路に進んだか否か)。
	expectBaselineRef bool
	expectTestResult  bool
	expectDiffSummary bool
}

// proposeLoopSetupFromBase は proposeLoopSetup の戻り値構造を踏襲しつつ、
// fakeImproveAgent と ask を nil で返す形を持つ helper。各 scenario の setup は
// 本 helper を起点に individual fake を上書きする。
func proposeLoopSetupFromBase(t *testing.T) (
	string, string, *workflowRecorder, context.Context,
	improveWorkflowDeps, *proposeLoopFakes,
) {
	t.Helper()
	return proposeLoopSetup(t)
}

// scenarioAcceptedPassed は applied_passed (IsIntent=true / Fn nil) scenario を構築する。
func scenarioAcceptedPassed(t *testing.T) (
	string, string, *workflowRecorder, context.Context,
	improveWorkflowDeps, *fakeImproveAgent, workflow.WorkflowAsk, string,
) {
	t.Helper()
	cwd, runID, recorder, ctx, deps, fakes := proposeLoopSetupFromBase(t)
	fakes.git.snapshotResult = patch.BaselineRef{HeadCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	fakes.gotest.result = patch.TestResult{ExitCode: 0, DurationMS: 7}
	diff := validDiffFor(t, cwd, fakes.contextBuilder.result.TargetGoFile)
	agent := &fakeImproveAgent{
		responses: []provider.AgentResponse{
			{Content: diff, Success: true, SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderClaude}},
		},
	}
	ask := &fakeImproveAsk{responses: []fakeImproveAskResponse{{id: improveSelectAccept, ok: true}}}
	return cwd, runID, recorder, ctx, deps, agent, ask, fakes.contextBuilder.result.TargetGoFile
}

// scenarioUserRejected は user_rejected (IsIntent=true / Fn nil) scenario を構築する。
func scenarioUserRejected(t *testing.T) (
	string, string, *workflowRecorder, context.Context,
	improveWorkflowDeps, *fakeImproveAgent, workflow.WorkflowAsk, string,
) {
	t.Helper()
	cwd, runID, recorder, ctx, deps, fakes := proposeLoopSetupFromBase(t)
	diff := validDiffFor(t, cwd, fakes.contextBuilder.result.TargetGoFile)
	agent := &fakeImproveAgent{
		responses: []provider.AgentResponse{
			{Content: diff, Success: true, SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderClaude}},
		},
	}
	ask := &fakeImproveAsk{responses: []fakeImproveAskResponse{{id: improveSelectReject, ok: true}}}
	return cwd, runID, recorder, ctx, deps, agent, ask, fakes.contextBuilder.result.TargetGoFile
}

// scenarioAppliedTestFailedReverted は accept → apply → go test 失敗 → Revert 成功の scenario。
func scenarioAppliedTestFailedReverted(t *testing.T) (
	string, string, *workflowRecorder, context.Context,
	improveWorkflowDeps, *fakeImproveAgent, workflow.WorkflowAsk, string,
) {
	t.Helper()
	cwd, runID, recorder, ctx, deps, fakes := proposeLoopSetupFromBase(t)
	fakes.git.snapshotResult = patch.BaselineRef{HeadCommit: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	fakes.gotest.result = patch.TestResult{ExitCode: 1, DurationMS: 12}
	diff := validDiffFor(t, cwd, fakes.contextBuilder.result.TargetGoFile)
	agent := &fakeImproveAgent{
		responses: []provider.AgentResponse{
			{Content: diff, Success: true, SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderClaude}},
		},
	}
	ask := &fakeImproveAsk{responses: []fakeImproveAskResponse{{id: improveSelectAccept, ok: true}}}
	return cwd, runID, recorder, ctx, deps, agent, ask, fakes.contextBuilder.result.TargetGoFile
}

// scenarioGitApplyFailed_Snapshot は accept 直後の Snapshot 失敗 → git_apply_failed scenario。
func scenarioGitApplyFailed_Snapshot(t *testing.T) (
	string, string, *workflowRecorder, context.Context,
	improveWorkflowDeps, *fakeImproveAgent, workflow.WorkflowAsk, string,
) {
	t.Helper()
	cwd, runID, recorder, ctx, deps, fakes := proposeLoopSetupFromBase(t)
	fakes.git.snapshotErr = errors.New("rev-parse failed")
	diff := validDiffFor(t, cwd, fakes.contextBuilder.result.TargetGoFile)
	agent := &fakeImproveAgent{
		responses: []provider.AgentResponse{
			{Content: diff, Success: true, SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderClaude}},
		},
	}
	ask := &fakeImproveAsk{responses: []fakeImproveAskResponse{{id: improveSelectAccept, ok: true}}}
	return cwd, runID, recorder, ctx, deps, agent, ask, fakes.contextBuilder.result.TargetGoFile
}

// scenarioGitApplyFailed_ApplyCheck は accept → Snapshot 成功 → ApplyCheck 失敗 scenario。
func scenarioGitApplyFailed_ApplyCheck(t *testing.T) (
	string, string, *workflowRecorder, context.Context,
	improveWorkflowDeps, *fakeImproveAgent, workflow.WorkflowAsk, string,
) {
	t.Helper()
	cwd, runID, recorder, ctx, deps, fakes := proposeLoopSetupFromBase(t)
	fakes.git.snapshotResult = patch.BaselineRef{HeadCommit: "cccccccccccccccccccccccccccccccccccccccc"}
	fakes.git.applyCheckErr = errors.New("does not apply cleanly")
	diff := validDiffFor(t, cwd, fakes.contextBuilder.result.TargetGoFile)
	agent := &fakeImproveAgent{
		responses: []provider.AgentResponse{
			{Content: diff, Success: true, SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderClaude}},
		},
	}
	ask := &fakeImproveAsk{responses: []fakeImproveAskResponse{{id: improveSelectAccept, ok: true}}}
	return cwd, runID, recorder, ctx, deps, agent, ask, fakes.contextBuilder.result.TargetGoFile
}

// scenarioGitApplyFailed_Apply は ApplyCheck 通過後の Apply 失敗 scenario (race 防御経路)。
func scenarioGitApplyFailed_Apply(t *testing.T) (
	string, string, *workflowRecorder, context.Context,
	improveWorkflowDeps, *fakeImproveAgent, workflow.WorkflowAsk, string,
) {
	t.Helper()
	cwd, runID, recorder, ctx, deps, fakes := proposeLoopSetupFromBase(t)
	fakes.git.snapshotResult = patch.BaselineRef{HeadCommit: "dddddddddddddddddddddddddddddddddddddddd"}
	fakes.git.applyErr = errors.New("apply race")
	diff := validDiffFor(t, cwd, fakes.contextBuilder.result.TargetGoFile)
	agent := &fakeImproveAgent{
		responses: []provider.AgentResponse{
			{Content: diff, Success: true, SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderClaude}},
		},
	}
	ask := &fakeImproveAsk{responses: []fakeImproveAskResponse{{id: improveSelectAccept, ok: true}}}
	return cwd, runID, recorder, ctx, deps, agent, ask, fakes.contextBuilder.result.TargetGoFile
}

// scenarioRevertFailed は accept → apply → test 失敗 → Revert 自体失敗 scenario。
func scenarioRevertFailed(t *testing.T) (
	string, string, *workflowRecorder, context.Context,
	improveWorkflowDeps, *fakeImproveAgent, workflow.WorkflowAsk, string,
) {
	t.Helper()
	cwd, runID, recorder, ctx, deps, fakes := proposeLoopSetupFromBase(t)
	fakes.git.snapshotResult = patch.BaselineRef{HeadCommit: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}
	fakes.gotest.result = patch.TestResult{ExitCode: 1, DurationMS: 8}
	fakes.git.revertErr = patch.ErrRevertFailed
	diff := validDiffFor(t, cwd, fakes.contextBuilder.result.TargetGoFile)
	agent := &fakeImproveAgent{
		responses: []provider.AgentResponse{
			{Content: diff, Success: true, SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderClaude}},
		},
	}
	ask := &fakeImproveAsk{responses: []fakeImproveAskResponse{{id: improveSelectAccept, ok: true}}}
	return cwd, runID, recorder, ctx, deps, agent, ask, fakes.contextBuilder.result.TargetGoFile
}

// scenarioContextLoadFailed は ContextBuilder.Build が ErrContextLoad を返す scenario。
func scenarioContextLoadFailed(t *testing.T) (
	string, string, *workflowRecorder, context.Context,
	improveWorkflowDeps, *fakeImproveAgent, workflow.WorkflowAsk, string,
) {
	t.Helper()
	cwd, runID, recorder, ctx, deps, fakes := proposeLoopSetupFromBase(t)
	fakes.contextBuilder.err = patch.ErrContextLoad
	return cwd, runID, recorder, ctx, deps, &fakeImproveAgent{}, nil, fakes.contextBuilder.result.TargetGoFile
}

// scenarioRegenerateLoopAborted_Mixed は agent が unparseable / out_of_scope を交互に返す scenario。
// AttemptRecord.Outcome として `proposal_unparseable` と `out_of_scope_diff` の両 closed set 値を
// 1 ケースで観察可能にする (12.2 の closed set 10 値全網羅契約の補完)。
func scenarioRegenerateLoopAborted_Mixed(t *testing.T) (
	string, string, *workflowRecorder, context.Context,
	improveWorkflowDeps, *fakeImproveAgent, workflow.WorkflowAsk, string,
) {
	t.Helper()
	cwd, runID, recorder, ctx, deps, fakes := proposeLoopSetupFromBase(t)
	// 5 attempts: unparseable / out_of_scope / unparseable / out_of_scope / unparseable
	agent := &fakeImproveAgent{
		responses: []provider.AgentResponse{
			{Content: unparseableContent, Success: true},
			{Content: outOfScopeDiff, Success: true},
			{Content: unparseableContent, Success: true},
			{Content: outOfScopeDiff, Success: true},
			{Content: unparseableContent, Success: true},
		},
	}
	return cwd, runID, recorder, ctx, deps, agent, nil, fakes.contextBuilder.result.TargetGoFile
}

// scenarioPreconditionFailed_HandoffMissing は handoff 不在経路の scenario。
// handoff 不在は precheck 内で context 構築前に early return するため、
// ctx に handoff を注入しない形で構築する。proposeLoopSetup は handoff を ctx に
// 入れて返すため、本 scenario では successDeps と context.Background() を組み合わせる。
func scenarioPreconditionFailed_HandoffMissing(t *testing.T) (
	string, string, *workflowRecorder, context.Context,
	improveWorkflowDeps, *fakeImproveAgent, workflow.WorkflowAsk, string,
) {
	t.Helper()
	cwd := t.TempDir()
	runID := "test-run-precondition"
	recorder := &workflowRecorder{}
	deps, _, _, _ := successDeps()
	// gomod / resolver の moduleRoot を cwd に揃え、defer の result.json 書き出しが
	// 実 path に書けるようにする。
	deps.goMod.(*fakeImproveGoMod).moduleRoot = cwd
	deps.resolver.(*fakeImproveResolver).resolved = filepath.Join(cwd, "internal", "workflow", "git_commit.go")
	// handoff を入れずに ctx を構築 (workflow.RunMetadata + Recorder のみ注入)。
	ctx := workflowctx.ContextWithRunMetadata(context.Background(), workflowctx.RunMetadata{
		WorkflowRunID: runID,
		WorkflowID:    improveWorkflowID,
		WorkflowName:  improveWorkflowLabel,
		CWD:           cwd,
	})
	ctx = workflow.ContextWithRecorder(ctx, recorder)
	return cwd, runID, recorder, ctx, deps, &fakeImproveAgent{}, nil, ""
}

// TestImproveWorkflow_AllVerdicts_ResultJSONContent は task 12.2 の中核契約を verdict 別に検証する。
//
// 検証マトリクス (10 verdict 全網羅):
//
//	最終 verdict として確定する 8 値 → result.json.verdict で観察 (本テスト)
//	  precondition_failed / context_load_failed / regenerate_loop_aborted /
//	  user_rejected / applied_passed / git_apply_failed / applied_test_failed_reverted /
//	  revert_failed
//	AttemptOutcome のみで観察できる 2 値 → attempts[].outcome で観察
//	  proposal_unparseable / out_of_scope_diff
//
// 各 scenario で:
//   - Fn の戻り err が IsIntent と整合する (IsIntent=true → nil / false → `[verdict:<id>]` プレフィックス)
//   - result.json.verdict が期待値と一致
//   - result.json.target_workflow_id / target_go_file / run_ids が handoff + resolver と整合
//   - apply 経路の verdict では baseline_ref / test_result / diff_summary が非 nil
//   - workflow.RecordRunArtifact 呼び出し回数 (context / proposal / review / result) が期待マトリクスと一致
func TestImproveWorkflow_AllVerdicts_ResultJSONContent(t *testing.T) {
	scenarios := []verdictExpectation{
		{
			name:                  "applied_passed",
			setup:                 scenarioAcceptedPassed,
			expectedVerdict:       patch.VerdictAppliedPassed,
			isIntent:              true,
			expectedContextCount:  1,
			expectedProposalCount: 1,
			expectedReviewCount:   1,
			expectedResultCount:   1,
			expectBaselineRef:     true,
			expectTestResult:      true,
			expectDiffSummary:     true,
		},
		{
			name:                  "user_rejected",
			setup:                 scenarioUserRejected,
			expectedVerdict:       patch.VerdictUserRejected,
			isIntent:              true,
			expectedContextCount:  1,
			expectedProposalCount: 1,
			expectedReviewCount:   1,
			expectedResultCount:   1,
			expectBaselineRef:     false,
			expectTestResult:      false,
			expectDiffSummary:     true, // ok outcome 1 件で DiffSummary が record される
		},
		{
			name:                  "applied_test_failed_reverted",
			setup:                 scenarioAppliedTestFailedReverted,
			expectedVerdict:       patch.VerdictAppliedTestFailedReverted,
			isIntent:              false,
			expectedContextCount:  1,
			expectedProposalCount: 1,
			expectedReviewCount:   1,
			expectedResultCount:   1,
			expectBaselineRef:     true,
			expectTestResult:      true,
			expectDiffSummary:     true,
		},
		{
			name:                  "git_apply_failed_snapshot",
			setup:                 scenarioGitApplyFailed_Snapshot,
			expectedVerdict:       patch.VerdictGitApplyFailed,
			isIntent:              false,
			expectedContextCount:  1,
			expectedProposalCount: 1,
			expectedReviewCount:   1,
			expectedResultCount:   1,
			expectBaselineRef:     false, // Snapshot 失敗のため baseline_ref は record されない
			expectTestResult:      false,
			expectDiffSummary:     true,
		},
		{
			name:                  "git_apply_failed_apply_check",
			setup:                 scenarioGitApplyFailed_ApplyCheck,
			expectedVerdict:       patch.VerdictGitApplyFailed,
			isIntent:              false,
			expectedContextCount:  1,
			expectedProposalCount: 1,
			expectedReviewCount:   1,
			expectedResultCount:   1,
			expectBaselineRef:     true, // Snapshot 通過後のため baseline_ref は record 済
			expectTestResult:      false,
			expectDiffSummary:     true,
		},
		{
			name:                  "git_apply_failed_apply",
			setup:                 scenarioGitApplyFailed_Apply,
			expectedVerdict:       patch.VerdictGitApplyFailed,
			isIntent:              false,
			expectedContextCount:  1,
			expectedProposalCount: 1,
			expectedReviewCount:   1,
			expectedResultCount:   1,
			expectBaselineRef:     true,
			expectTestResult:      false,
			expectDiffSummary:     true,
		},
		{
			name:                  "revert_failed",
			setup:                 scenarioRevertFailed,
			expectedVerdict:       patch.VerdictRevertFailed,
			isIntent:              false,
			expectedContextCount:  1,
			expectedProposalCount: 1,
			expectedReviewCount:   1,
			expectedResultCount:   1,
			expectBaselineRef:     true,
			expectTestResult:      true,
			expectDiffSummary:     true,
		},
		{
			name:                  "context_load_failed",
			setup:                 scenarioContextLoadFailed,
			expectedVerdict:       patch.VerdictContextLoadFailed,
			isIntent:              false,
			expectedContextCount:  0, // context 構築失敗のため context.json も登録されない
			expectedProposalCount: 0,
			expectedReviewCount:   0,
			expectedResultCount:   1,
			expectBaselineRef:     false,
			expectTestResult:      false,
			expectDiffSummary:     false,
		},
		{
			name:                  "regenerate_loop_aborted_mixed_outcomes",
			setup:                 scenarioRegenerateLoopAborted_Mixed,
			expectedVerdict:       patch.VerdictRegenerateLoopAborted,
			isIntent:              false,
			expectedContextCount:  1,
			expectedProposalCount: patch.DefaultMaxRegenerateAttempts, // 5
			expectedReviewCount:   0,                                  // ok outcome 不在のため Select は呼ばれず review.json 不登録
			expectedResultCount:   1,
			expectBaselineRef:     false,
			expectTestResult:      false,
			expectDiffSummary:     false, // ok outcome なし → DiffSummary 非 record
		},
		{
			name:                  "precondition_failed_handoff_missing",
			setup:                 scenarioPreconditionFailed_HandoffMissing,
			expectedVerdict:       patch.VerdictPreconditionFailed,
			isIntent:              false,
			expectedContextCount:  0,
			expectedProposalCount: 0,
			expectedReviewCount:   0,
			expectedResultCount:   1,
			expectBaselineRef:     false,
			expectTestResult:      false,
			expectDiffSummary:     false,
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			cwd, runID, recorder, ctx, deps, agent, ask, expectedTargetGoFile := sc.setup(t)

			err := runImproveWorkflowOrchestration(ctx, cwd, nil, ask, agent, validRegistryFn(), deps)

			// Fn 戻り err と IsIntent の整合 (12.2 中核契約)。
			if sc.isIntent {
				if err != nil {
					t.Fatalf("verdict=%s は IsIntent=true のため Fn nil 期待: got %v", sc.expectedVerdict, err)
				}
			} else {
				assertVerdictPrefix(t, err, sc.expectedVerdict)
			}

			// result.json は全 verdict 経路で必ず登録される (Req 6.5 / 観察可能完了条件)。
			if got, want := countArtifactKind(recorder, akashic.ArtifactKindResult), sc.expectedResultCount; got != want {
				t.Errorf("result.json 登録件数: got %d, want %d", got, want)
			}
			if got, want := countArtifactKind(recorder, akashic.ArtifactKindContext), sc.expectedContextCount; got != want {
				t.Errorf("context artifact 登録件数: got %d, want %d", got, want)
			}
			if got, want := countArtifactKind(recorder, akashic.ArtifactKindProposal), sc.expectedProposalCount; got != want {
				t.Errorf("proposal artifact 登録件数: got %d, want %d", got, want)
			}
			if got, want := countArtifactKind(recorder, akashic.ArtifactKindReview), sc.expectedReviewCount; got != want {
				t.Errorf("review artifact 登録件数: got %d, want %d", got, want)
			}

			// result.json の中身を検証 (12.2 中核契約)。
			rec := loadResultJSON(t, recorder)
			if rec.WorkflowRunID != runID {
				t.Errorf("ResultRecord.WorkflowRunID: got %q, want %q", rec.WorkflowRunID, runID)
			}
			if rec.Verdict != sc.expectedVerdict {
				t.Errorf("ResultRecord.Verdict: got %q, want %q", rec.Verdict, sc.expectedVerdict)
			}
			if strings.TrimSpace(rec.Summary) == "" {
				t.Errorf("ResultRecord.Summary: 空文字列禁止 (verdict=%s)", sc.expectedVerdict)
			}
			// IsIntent=false で precondition_failed 経路 (handoff missing) を除く全 scenario は
			// handoff の TargetWorkflowID と RunIDs が record される。handoff missing 経路は
			// handoff 不在のため空 string / 空 slice となる。
			if expectedTargetGoFile != "" {
				if rec.TargetWorkflowID != "git-commit" {
					t.Errorf("ResultRecord.TargetWorkflowID: got %q, want %q", rec.TargetWorkflowID, "git-commit")
				}
				if rec.TargetGoFile != expectedTargetGoFile {
					t.Errorf("ResultRecord.TargetGoFile: got %q, want %q", rec.TargetGoFile, expectedTargetGoFile)
				}
				if len(rec.RunIDs) != 1 || rec.RunIDs[0] != "run-source-1" {
					t.Errorf("ResultRecord.RunIDs: got %#v, want [run-source-1]", rec.RunIDs)
				}
			} else {
				if rec.TargetWorkflowID != "" {
					t.Errorf("handoff 不在 scenario で TargetWorkflowID 非空: got %q", rec.TargetWorkflowID)
				}
				if len(rec.RunIDs) != 0 {
					t.Errorf("handoff 不在 scenario で RunIDs 非空: got %#v", rec.RunIDs)
				}
			}

			// apply 経路に進んだか否かで baseline_ref / test_result / diff_summary の null 化を検証。
			if (rec.BaselineRef != nil) != sc.expectBaselineRef {
				t.Errorf("ResultRecord.BaselineRef nil 化: got non-nil=%v, want non-nil=%v",
					rec.BaselineRef != nil, sc.expectBaselineRef)
			}
			if (rec.TestResult != nil) != sc.expectTestResult {
				t.Errorf("ResultRecord.TestResult nil 化: got non-nil=%v, want non-nil=%v",
					rec.TestResult != nil, sc.expectTestResult)
			}
			if (rec.DiffSummary != nil) != sc.expectDiffSummary {
				t.Errorf("ResultRecord.DiffSummary nil 化: got non-nil=%v, want non-nil=%v",
					rec.DiffSummary != nil, sc.expectDiffSummary)
			}

			// regenerate_loop_aborted_mixed_outcomes scenario では attempts[].outcome に
			// closed set 2 値 (proposal_unparseable / out_of_scope_diff) が両方含まれる
			// (12.2 closed set 10 値全網羅契約の補完)。
			if sc.name == "regenerate_loop_aborted_mixed_outcomes" {
				assertAttemptOutcomesContain(t, rec.Attempts,
					string(patch.VerdictProposalUnparseable),
					string(patch.VerdictOutOfScopeDiff),
				)
			}

			// applied_passed / user_rejected / その他 ok 経路を踏んだ scenario は attempts に
			// 少なくとも 1 件 outcome=ok のレコードがある。
			if sc.expectDiffSummary && sc.expectedProposalCount > 0 {
				foundOK := false
				for _, at := range rec.Attempts {
					if at.Outcome == attemptOutcomeOK {
						foundOK = true
						break
					}
				}
				if !foundOK {
					t.Errorf("attempts[].outcome に ok が含まれない (verdict=%s, attempts=%#v)",
						sc.expectedVerdict, rec.Attempts)
				}
			}
		})
	}
}

// assertAttemptOutcomesContain は AttemptRecord 群の Outcome 集合に want の全要素が含まれることを検証する。
func assertAttemptOutcomesContain(t *testing.T, attempts []patch.AttemptRecord, want ...string) {
	t.Helper()
	got := make(map[string]struct{}, len(attempts))
	for _, at := range attempts {
		got[at.Outcome] = struct{}{}
	}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("attempts[].outcome に %q が含まれない (got=%v)", w, got)
		}
	}
}

// TestImproveWorkflow_RealGitOpsFixture_GitApplyFailed は task 12.2 の追加要求
// 「real な GitOps fixture も別 fixture path で併用」を満たす integration テスト。
//
// real な os/exec git CLI を `os.MkdirTemp` 上に init した fixture repo に対して呼び出し、
// fixture と一致しない invalid diff で `git apply --check` が失敗 → git_apply_failed verdict
// の経路を踏む。改善対象 Go ファイル / go.mod / 1 commit を fixture に揃えることで、
// gomod inspector / resolver / status / snapshot / apply-check の 5 段が real path で走る
// (gotest と context_builder / prompt_template だけ fake)。
//
// 観点:
//   - real な git CLI 呼び出し経路 (status / snapshot / apply-check) で patch.NewGitOps()
//     が回ること
//   - 失敗経路 (apply-check failed) でも全 verdict 経路保証 (result.json + workflow.RecordRunArtifact 計数)
//     が成立すること
func TestImproveWorkflow_RealGitOpsFixture_GitApplyFailed(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git CLI が利用できない環境のためスキップ")
	}

	cwd := t.TempDir()
	runID := "real-fixture-run-1"
	recorder := &workflowRecorder{}

	// fixture repo: git init + go.mod + internal/workflow/git_commit.go + 1 commit。
	// improvepatch の git_test.go が用いる setupFixtureRepo と同形だが、本テストは
	// workflow パッケージ側のため再実装する (improvepatch のテスト helper は package-private)。
	runFixtureGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = cwd
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\noutput: %s", args, err, string(out))
		}
	}
	runFixtureGit("init", "--initial-branch=main")
	runFixtureGit("config", "user.email", "test@example.com")
	runFixtureGit("config", "user.name", "Test User")

	// go.mod: GoModInspector が `module github.com/kazuki-oshino/rilll` 完全一致を要求する。
	if err := os.WriteFile(filepath.Join(cwd, "go.mod"),
		[]byte("module github.com/kazuki-oshino/rilll\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	// 改善対象ファイル: TargetResolver は `internal/workflow/<snake>.go` を期待する。
	targetDir := filepath.Join(cwd, "internal", "workflow")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatalf("mkdir target dir: %v", err)
	}
	targetFile := filepath.Join(targetDir, "git_commit.go")
	if err := os.WriteFile(targetFile, []byte("package workflow\n\nfunc placeholder() {}\n"), 0o644); err != nil {
		t.Fatalf("write target file: %v", err)
	}
	runFixtureGit("add", "-A")
	runFixtureGit("commit", "-m", "fixture initial commit")

	// invalid diff: fixture には存在しない context 行を含む形にして `git apply --check` を失敗させる。
	// scope guard を通過させるため `diff --git a/internal/workflow/git_commit.go ...` 形は維持する。
	rel := "internal/workflow/git_commit.go"
	invalidDiffStr := "```diff\n" +
		"diff --git a/" + rel + " b/" + rel + "\n" +
		"--- a/" + rel + "\n" +
		"+++ b/" + rel + "\n" +
		"@@ -1,3 +1,4 @@\n" +
		" package workflow\n" +
		" \n" +
		"-non_existing_line()\n" +
		"+replaced_line()\n" +
		"```\n"

	// real GitOps + GoModInspector + DefaultResolver を併用、ContextBuilder / PromptTemplate /
	// GoTestRunner は本テスト主旨外のため fake。
	deps := improveWorkflowDeps{
		gitOps:   patch.NewGitOps(),
		goMod:    patch.NewGoModInspector(),
		resolver: patch.NewDefaultResolver(),
		contextBuilder: &fakeImproveContextBuilder{
			result: patch.PromptContext{
				TargetWorkflowID: "git-commit",
				TargetGoFile:     targetFile,
				CurrentSource:    "package workflow\n",
			},
		},
		promptTemplate: &fakeImprovePromptTemplate{},
		gotest:         &fakeImproveGoTest{},
	}

	agent := &fakeImproveAgent{
		responses: []provider.AgentResponse{
			{Content: invalidDiffStr, Success: true, SystemInfo: &provider.AgentSystemInfo{ProviderID: provider.ProviderClaude}},
		},
	}
	ask := &fakeImproveAsk{
		responses: []fakeImproveAskResponse{{id: improveSelectAccept, ok: true}},
	}

	ctx := workflow.ContextWithHandoff(context.Background(), workflow.HandoffParams{
		TargetWorkflowID: "git-commit",
		RunIDs:           []string{"run-source-1"},
	})
	ctx = workflowctx.ContextWithRunMetadata(ctx, workflowctx.RunMetadata{
		WorkflowRunID: runID,
		WorkflowID:    improveWorkflowID,
		WorkflowName:  improveWorkflowLabel,
		CWD:           cwd,
	})
	ctx = workflow.ContextWithRecorder(ctx, recorder)

	err := runImproveWorkflowOrchestration(ctx, cwd, nil, ask, agent, validRegistryFn(), deps)
	assertVerdictPrefix(t, err, patch.VerdictGitApplyFailed)

	// real な git status / snapshot / apply-check が全部走る + 失敗経路でも result.json は登録される。
	assertResultJSONRecorded(t, recorder)
	rec := loadResultJSON(t, recorder)
	if rec.Verdict != patch.VerdictGitApplyFailed {
		t.Errorf("ResultRecord.Verdict: got %q, want %q", rec.Verdict, patch.VerdictGitApplyFailed)
	}
	// Snapshot は real な `git rev-parse HEAD` を呼んでおり、BaselineRef.HeadCommit は
	// 40 文字 hex SHA-1 形式 (improvepatch/git_test.go の sha1Re と同形)。
	if rec.BaselineRef == nil {
		t.Fatalf("Snapshot 通過後のため BaselineRef は非 nil 期待: got nil")
	}
	if matched, _ := regexp.MatchString(`^[0-9a-f]{40}$`, rec.BaselineRef.HeadCommit); !matched {
		t.Errorf("BaselineRef.HeadCommit: got %q, want 40-char hex SHA-1", rec.BaselineRef.HeadCommit)
	}
	if rec.TargetGoFile != filepath.Clean(targetFile) {
		t.Errorf("ResultRecord.TargetGoFile: got %q, want %q", rec.TargetGoFile, filepath.Clean(targetFile))
	}
}

// ----- registryHasWorkflowID の単体検証 -----

// TestRegistryHasWorkflowID_KnownAndUnknown は既知 / 未知 ID の判定契約を検証する (Req 1.4)。
func TestRegistryHasWorkflowID_KnownAndUnknown(t *testing.T) {
	registryFn := validRegistryFn()

	// 既知 ID: validRegistryFn が常時登録する "git-commit"
	if !registryHasWorkflowID(registryFn, "git-commit") {
		t.Error("既知 ID 'git-commit' が registry に見つからない")
	}
	// 未知 ID
	if registryHasWorkflowID(registryFn, "no-such-workflow-9999") {
		t.Error("未知 ID 'no-such-workflow-9999' が誤って既知扱いされた")
	}
	// 空文字列 / 空白のみは false (防御的拒否)
	if registryHasWorkflowID(registryFn, "") {
		t.Error("空文字列 ID が誤って既知扱いされた")
	}
	if registryHasWorkflowID(registryFn, "   ") {
		t.Error("空白のみ ID が誤って既知扱いされた")
	}
	// nil registryFn 防御 (closure 注入忘れ)
	if registryHasWorkflowID(nil, "git-commit") {
		t.Error("nil registryFn 時に true が返された")
	}
}
