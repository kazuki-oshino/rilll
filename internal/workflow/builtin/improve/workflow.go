// Package workflow の本ファイルは workflow-self-improve spec の improve-workflow workflow.Definition を提供する
// (tasks.md 11.1 / 11.2 / 11.3 / design.md §ImproveWorkflow / Req 1.1〜1.7, 2.1, 2.6, 3.1〜3.6, 4.x, 5.x, 6.x, 7.4)。
//
// 本ファイルの責務は task 11.1 / 11.2 / 11.3 のフェーズを束ねた orchestration 本体:
//
// task 11.1 (precheck フェーズ):
//
//  1. workflow.HandoffFromContext から TargetWorkflowID + RunIDs を受領 (Req 1.1)
//  2. RunIDs が空でないことを検証 (Req 1.2 / 1.3、配列形は length>1 拡張に備えて維持)
//  3. GoModInspector.ModulePath で cwd identity 検証 + moduleRoot 解決 (Req 1.4 / 1.6 / Resolved §5)
//  4. workflow.Registry(agent) lookup で TargetWorkflowID が既知であることを確認 (Req 1.4)
//  5. TargetResolver.Resolve で改善対象 Go ファイルを解決 (Req 1.5 / 7.1, 7.2, 7.4)
//  6. GitOps.Status で git 作業ツリーが clean であることを確認 (Req 1.6 / Resolved §7)
//
// task 11.2 (context 構築 + propose ループ):
//
//  7. ContextBuilder.Build で PromptContext を構築 → 失敗時 VerdictContextLoadFailed (Req 2.6)
//  8. ContextRecord を 00-context.json として `workflow.RecordRunArtifact(ContextKind, ...)` で登録 (Req 2.1)
//  9. attempt = 1..MaxRegenerateAttempts の regenerate ループ:
//     - PromptTemplate.Render(ctx, promptContext, attempt, previousProposals, providerID)
//     - workflowctx.AgentCaller.CallAgent (provider が claude/codex のとき OutputFormat=JSONSchemaForDiff)
//     - Patch.ExtractAndGuard で diff 抽出 + scope guard
//     - proposal-N.diff を `workflow.RecordRunArtifact(ProposalKind, ...)` で登録
//     - AttemptRecord{Attempt, Provider, PatchArtifact, Outcome} を蓄積
//     - outcome="ok" のときは previousProposals に追加 + DiffSummaryRecord 短縮表示を workflow.WorkflowText
//
// task 11.3 (review 3 択 Select + apply + test + record):
//
//  10. outcome=ok の attempt で workflow.WorkflowAsk.Select(accept/reject/regenerate) を提示 (Req 4.1, 4.2)。
//     ask が nil の場合は 11.2 互換で auto-regenerate (loop 継続) として扱う。
//  11. reject 選択時 (または ESC キャンセル) は review.json を登録し VerdictUserRejected で終了 (Req 4.4 / 6.5)。
//  12. accept 選択時は review.json を登録し以下の apply フェーズに進む (Req 4.3, 4.6):
//     - GitOps.Snapshot で BaselineRef を取得 (Req 5.1)
//     - GitOps.ApplyCheck → 失敗で VerdictGitApplyFailed (Apply / Revert 不実行、Req 5.2, 5.3)
//     - GitOps.Apply → 失敗で VerdictGitApplyFailed (Req 5.4)
//     - GoTestRunner.Run → ExitCode==0 で VerdictAppliedPassed (Req 5.5, 5.7)
//     - 非ゼロ終了で GitOps.Revert を実行 (Req 5.6):
//     - 成功: VerdictAppliedTestFailedReverted
//     - 失敗: VerdictRevertFailed + 復旧手順 (`git checkout <head_commit> -- <file>`) を workflow.WorkflowText で案内 (Req 5.9)
//  13. 全 verdict 経路で result.json を `workflow.RecordRunArtifact(ResultKind, "result.json")` で必ず登録する
//     (Req 6.5 記録欠落禁止)。defer 経路で集約済み state を ResultRecord に詰めて出力する。
//
// いずれかの段階で失敗した場合は対応する verdict と
// `[verdict:<id>] <summary>` 形のエラーを返す (Req 6.2 / 6.5)。Verdict.IsIntent() == true
// (`applied_passed` / `user_rejected`) のときは Fn から nil を返し、
// `workflow_runs.error` は空のままとする (既存 `kiro_spec_loop` の `cancelled-by-user` 形と整合)。
//
// 副作用契約:
//   - precheck / context 構築 / propose ループ (outcome=ok 検出前まで) は GitOps.Status と
//     GoModInspector.ModulePath / TargetResolver.Resolve のみが副作用候補で、
//     Snapshot / ApplyCheck / Apply / Revert は呼ばれない (Wave 0 task 2.4 と整合)
//   - apply 系副作用は accept 選択 → Snapshot 以降の経路でのみ発火する
//   - 全 exit 経路 (defer) で result.json artifact 登録を試みる
package improve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/improve/patch"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// improveWorkflowID は本 workflow.Definition の workflow ID (kebab-case)。
// 既存 4 ワークフローと同形式で命名し、akashic レコードでも `workflow_runs.workflow_id`
// にそのまま格納される (Req 6.1)。
const improveWorkflowID = "improve-workflow"

// improveWorkflowLabel は TUI 上で表示される人間可読ラベル。
// 既存 `git commit` / `Kiro 仕様作成ループ` 等と並ぶ表示行で利用される。
const improveWorkflowLabel = "improve workflow"

// attemptOutcomeOK は AttemptRecord.Outcome の正常値。ExtractAndGuard 成功時に設定する。
const attemptOutcomeOK = "ok"

// improveSelectKey は workflow.WorkflowAsk.Select の semantic key。
// CLIAsk が `--review=accept|reject|regenerate` を解決するためのフラグ名と対応する。
const improveSelectKey = "review"

// 改善 diff のレビュー 3 択 (Req 4.2)。workflow.SelectItem.ID にそのまま格納する。
const (
	improveSelectAccept     = "accept"
	improveSelectReject     = "reject"
	improveSelectRegenerate = "regenerate"
)

// improveResultJSONFile / improveContextJSONFile / improveReviewJSONFile は
// `.rilll/improve-workflow/<runID>/` 直下に書き出す artifact ファイル名。
// design.md §Logical Data Model の規約値。
const (
	improveResultJSONFile  = "result.json"
	improveContextJSONFile = "00-context.json"
	improveReviewJSONFile  = "review.json"
)

// improveWorkflowDeps は ImproveWorkflow の Fn 内部で利用する副作用 / 純関数コンポーネントを集約する DI 構造体。
//
// 本番経路では ImproveWorkflow が defaultImproveWorkflowDeps(agent) を介して構築するが、
// テスト経路では runImproveWorkflowOrchestration を直接呼び、fake 実装を渡せる seam として保持する。
// workflow.Definition.Fn シグネチャを変更せずにテスト容易性を確保するため、本構造体は package-private に閉じる。
type improveWorkflowDeps struct {
	gitOps         patch.GitOps
	goMod          patch.GoModInspector
	resolver       patch.TargetResolver
	contextBuilder patch.ContextBuilder
	promptTemplate patch.PromptTemplate
	gotest         patch.GoTestRunner
}

// defaultImproveWorkflowDeps は本番経路で利用する improvepatch 既定実装一式を構築する。
// 本関数は ImproveWorkflow から呼ばれる唯一の場所であり、テスト経路では呼ばれない
// (テストは runImproveWorkflowOrchestration を直接呼び、独自 fake 実装を渡す)。
//
// agent から AkashicReader / AkashicArtifactReader を optional interface 経由で取り出し、
// DefaultContextBuilder を構築する (workflow.types.go の readerFromAgent / artifactReaderFromAgent 経由)。
// agent が両 optional iface を実装していない場合 (NoopRecorder 経路) は nil の reader / artifacts で
// DefaultContextBuilder を構築するため、Build 呼び出し時に ErrContextLoad 経路へ落ちる
// (orchestration 側で `context_load_failed` verdict にマップされる)。
func defaultImproveWorkflowDeps(agent workflowctx.AgentCaller) improveWorkflowDeps {
	return improveWorkflowDeps{
		gitOps:         patch.NewGitOps(),
		goMod:          patch.NewGoModInspector(),
		resolver:       patch.NewDefaultResolver(),
		contextBuilder: patch.NewDefaultContextBuilder(workflow.ReaderFromAgent(agent), workflow.ArtifactReaderFromAgent(agent)),
		promptTemplate: patch.NewDefaultPromptTemplate(),
		gotest:         patch.NewGoTestRunner(),
	}
}

// Workflow は workflow-self-improve spec の improve-workflow Definition を返す
// (design.md §ImproveWorkflow / tasks.md 11.1 / 11.2 / 11.3)。
//
// Definition のフィールド契約:
//   - ID: "improve-workflow" (Req 6.1)
//   - Label: "improve workflow"
//   - Args: 空 (hand-off ctx 経由で TargetWorkflowID / RunIDs を受領するため CLI フラグは取らない)。
//     CLI 単独起動時は HandoffFromContext が false を返し、precondition_failed で hard fail する。
//   - Hidden: true (hand-off 専用ワークフロー)。
//     TUI トップメニュー / `rilll list` / `rilll help` の一覧から本ワークフローを除外する。
//     起動経路は akashic Run 詳細サブビューの `i` キー (workflow.HandoffParams 注入) のみ。
//     `rilll run improve-workflow` の直叩き自体は findDefinition が弾かないので可能だが、
//     handoff 不在で precondition_failed に落ちる従来動作のまま。
//   - Recorder: RecorderFromAgent(agent) で取得 (既存規約。NoopRecorder のときは no-op 記録)。
//
// registryFn は precheck の workflow ID 検証 (Req 1.4) に使う Definition リストを供給する。
// builtin.Registry は自身を含むスライスを closure で渡すことで improve→builtin の循環参照を回避する。
//
// Fn 本体は precheck → context 構築 + propose ループ → review 3 択 → apply + test + record を駆動する。
func Workflow(agent workflowctx.AgentCaller, registryFn func() []workflow.Definition) workflow.Definition {
	deps := defaultImproveWorkflowDeps(agent)
	return workflow.Definition{
		ID:       improveWorkflowID,
		Label:    improveWorkflowLabel,
		Args:     nil,
		Recorder: workflow.RecorderFromAgent(agent),
		Hidden:   true,
		Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			return runImproveWorkflowOrchestration(ctx, cwd, onEvent, ask, agent, registryFn, deps)
		},
	}
}

// improveReviewSelection は review.json 内の各レビュー選択ログ。
// regenerate / reject / accept のいずれかを attempt 番号 + 確定時刻で保持する。
type improveReviewSelection struct {
	Attempt    int    `json:"attempt"`
	SelectedID string `json:"selected_id"` // accept | reject | regenerate
	AtTime     string `json:"at_time"`     // RFC3339Nano
}

// improveReviewRecord は review.json artifact の正規型 (MVP は本ファイル内に閉じる)。
// design.md §Logical Data Model: `kind = review → review.json (verdict + ユーザ選択履歴)`。
type improveReviewRecord struct {
	WorkflowRunID string                   `json:"workflow_run_id"`
	Selections    []improveReviewSelection `json:"selections"`
}

// runImproveWorkflowOrchestration は ImproveWorkflow.Fn の本体実装。
// 副作用コンポーネントを deps 経由で受け取り、テストから fake で差し替えられる seam を提供する。
//
// 全 verdict 経路で result.json artifact が登録される契約を defer で保証する (Req 6.5)。
// IsIntent() == true (`applied_passed` / `user_rejected`) のとき Fn は nil を返し、
// それ以外の verdict は `[verdict:<id>] <summary>` 形の error を返す (Req 6.2)。
func runImproveWorkflowOrchestration(
	ctx context.Context,
	cwd string,
	onEvent func(workflow.WorkflowEvent),
	ask workflow.WorkflowAsk,
	agent workflowctx.AgentCaller,
	registryFn func() []workflow.Definition,
	deps improveWorkflowDeps,
) error {
	// onEventSafe は onEvent == nil の防御的呼び出し helper。
	// CLI 単独起動 / テスト経路で onEvent が nil で渡されてもパニックしない。
	onEventSafe := func(event workflow.WorkflowEvent) {
		if onEvent != nil {
			onEvent(event)
		}
	}

	// runDir は precheck より前に確定させる (defer の result.json 登録で必要)。
	// runIDFromContext は RunMetadata 不在のとき新規 UUID を生成する契約のため、
	// CLI 単独起動 (RunMetadata 注入無し) でも常に有効な runID が手に入る。
	startedAt := time.Now()
	runID := workflow.RunIDFromContext(ctx)
	runDir := filepath.Join(cwd, ".rilll", improveWorkflowID, runID)
	_ = os.MkdirAll(runDir, 0o755)

	// 最終 result.json 登録のために集約する mutable state。
	// 各 exit 経路は finalize* でこれらを更新してから return する。
	var (
		finalVerdict     patch.Verdict
		finalSummary     string
		targetWorkflowID string
		targetGoFile     string
		runIDs           []string
		attempts         []patch.AttemptRecord
		testResultRec    *patch.TestResultRecord
		baselineRefRec   *patch.BaselineRefRecord
		diffSummaryRec   *patch.DiffSummaryRecord
	)

	// defer での result.json artifact 登録 (Req 6.5 記録欠落禁止)。
	// 全 exit 経路で確実に走らせるため、本関数の冒頭で defer に積む。
	// ファイル書き出しと workflow.RecordRunArtifact 呼び出しの失敗は best-effort で握りつぶす
	// (akashic-artifacts spec Req 2.6 と整合)。
	defer func() {
		rec := patch.ResultRecord{
			WorkflowRunID:    runID,
			TargetWorkflowID: targetWorkflowID,
			TargetGoFile:     targetGoFile,
			RunIDs:           runIDs,
			Verdict:          finalVerdict,
			Summary:          finalSummary,
			Attempts:         attempts,
			TestResult:       testResultRec,
			BaselineRef:      baselineRefRec,
			DiffSummary:      diffSummaryRec,
			StartedAt:        startedAt.UTC().Format(time.RFC3339Nano),
			FinishedAt:       time.Now().UTC().Format(time.RFC3339Nano),
		}
		body, _ := json.MarshalIndent(rec, "", "  ")
		path := filepath.Join(runDir, improveResultJSONFile)
		_ = os.WriteFile(path, body, 0o644)
		_ = workflow.RecordRunArtifact(ctx, akashic.ArtifactKindResult, path)
	}()

	// setVerdict は finalVerdict / finalSummary を確定し、
	// IsIntent==true のとき nil、それ以外で `[verdict:<id>] <summary>` 形の error を返す。
	// 全 exit 経路はこの helper を経由して return することで、Req 6.2 / 6.5 の整合を一箇所で担保する。
	setVerdict := func(v patch.Verdict, summary string) error {
		finalVerdict = v
		finalSummary = summary
		if v.IsIntent() {
			return nil
		}
		return errors.New(patch.ErrorPrefixForVerdict(v, summary))
	}

	// Step 1: handoff 受領 (Req 1.1)
	handoff, ok := workflow.HandoffFromContext(ctx)
	if !ok {
		return setVerdict(patch.VerdictPreconditionFailed,
			"hand-off パラメータが ctx に注入されていない (TUI からの起動契約違反)")
	}
	targetWorkflowID = handoff.TargetWorkflowID
	runIDs = append([]string(nil), handoff.RunIDs...)

	// Step 2: RunIDs 空チェック (Req 1.2 / 1.3)
	if len(handoff.RunIDs) == 0 {
		return setVerdict(patch.VerdictPreconditionFailed,
			"hand-off の RunIDs が空 (起点 run ID が必須)")
	}

	// Step 3: GoModInspector.ModulePath で cwd identity 検証 + moduleRoot 解決 (Req 1.4 / 1.6 / Resolved §5)
	_, moduleRoot, gomodErr := deps.goMod.ModulePath(ctx, cwd)
	if gomodErr != nil {
		return setVerdict(patch.VerdictPreconditionFailed,
			fmt.Sprintf("go.mod identity 検証失敗: %v", gomodErr))
	}

	// Step 4: registryFn() lookup で未知 ID 拒否 (Req 1.4)
	if !registryHasWorkflowID(registryFn, handoff.TargetWorkflowID) {
		return setVerdict(patch.VerdictPreconditionFailed,
			fmt.Sprintf("未知の workflow ID: %q (registry に登録されていない)", handoff.TargetWorkflowID))
	}

	// Step 5: TargetResolver.Resolve で改善対象 Go ファイルを解決 (Req 1.5 / 7.1, 7.2, 7.4)
	resolved, resolveErr := deps.resolver.Resolve(ctx, moduleRoot, handoff.TargetWorkflowID)
	if resolveErr != nil {
		return setVerdict(patch.VerdictPreconditionFailed,
			fmt.Sprintf("改善対象ファイル未解決: %v", resolveErr))
	}
	targetGoFile = resolved

	// Step 6: GitOps.Status で git 作業ツリーが clean であることを検証 (Req 1.6 / Resolved §7)
	clean, statusErr := deps.gitOps.Status(ctx, moduleRoot)
	if statusErr != nil {
		return setVerdict(patch.VerdictPreconditionFailed,
			fmt.Sprintf("git status 失敗: %v", statusErr))
	}
	if !clean {
		return setVerdict(patch.VerdictPreconditionFailed,
			"git 作業ツリーに未コミット変更がある (precondition: clean tree required)")
	}

	// 全 precheck 通過: 起動受領をユーザに通知 (Req 1.7)。
	onEventSafe(workflow.WorkflowEvent{
		Type: workflow.WorkflowText,
		Text: fmt.Sprintf("improve-workflow precheck passed: target=%q module_root=%q run_ids=%v",
			handoff.TargetWorkflowID, moduleRoot, handoff.RunIDs),
	})

	// ----- task 11.2: context 構築 + propose ループ -----

	// Step 7: ContextBuilder.Build (Req 2.1, 2.6)
	buildInput := patch.BuildInput{
		TargetWorkflowID: handoff.TargetWorkflowID,
		RunIDs:           handoff.RunIDs,
		TargetGoFile:     targetGoFile,
	}
	promptContext, ctxErr := deps.contextBuilder.Build(ctx, buildInput)
	if ctxErr != nil {
		return setVerdict(patch.VerdictContextLoadFailed,
			fmt.Sprintf("ContextBuilder.Build 失敗: %v", ctxErr))
	}

	// Step 8: 00-context.json を ContextKind で 1 件登録 (Req 2.1)。
	// 書き出し / 登録失敗は best-effort で握りつぶす (akashic-artifacts spec Req 2.6)。
	contextRecord := patch.BuildContextRecord(runID, promptContext, buildInput, time.Now())
	contextJSON, _ := json.MarshalIndent(contextRecord, "", "  ")
	contextPath := filepath.Join(runDir, improveContextJSONFile)
	_ = os.WriteFile(contextPath, contextJSON, 0o644)
	_ = workflow.RecordRunArtifact(ctx, akashic.ArtifactKindContext, contextPath)

	// Step 9: regenerate ループ (Req 3.1〜3.6, 4.5)
	maxAttempts := buildInput.WithDefaults().MaxRegenerateAttempts
	var (
		previousProposals []patch.Patch
		providerID        string // 初回は ""、2 回目以降は前回 SystemInfo.ProviderID
		reviewSelections  []improveReviewSelection
	)

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		promptStr := deps.promptTemplate.Render(ctx, promptContext, attempt, previousProposals, providerID)
		callOpts := buildAgentCallOptions(providerID, attempt)
		resp, callErr := agent.CallAgent(ctx, promptStr, moduleRoot, workflow.ForwardEvents(onEvent), "", callOpts)

		// providerID は次回 attempt の Render に伝播。
		if resp.SystemInfo != nil && resp.SystemInfo.ProviderID != "" {
			providerID = string(resp.SystemInfo.ProviderID)
		}

		// outcome 判定: ExtractAndGuard 成功 → "ok"、それ以外は verdict 文字列。
		// CallAgent 自体の失敗は raw response を残しつつ proposal_unparseable に集約する。
		var (
			outcome   string
			diffBytes []byte
			proposal  patch.Patch
		)
		if callErr != nil {
			outcome = string(patch.VerdictProposalUnparseable)
			diffBytes = []byte(resp.Content)
		} else {
			var parseErr error
			proposal, parseErr = patch.ExtractAndGuard(resp.Content, promptContext.TargetGoFile, moduleRoot)
			outcome = classifyExtractOutcome(parseErr)
			if parseErr == nil {
				diffBytes = proposal.DiffBytes
			} else {
				diffBytes = []byte(resp.Content)
			}
		}

		// proposal-N.diff 登録 (Req 3.6 全試行記録)。
		proposalArtifactName := fmt.Sprintf("proposal-%d.diff", attempt)
		proposalPath := filepath.Join(runDir, proposalArtifactName)
		_ = os.WriteFile(proposalPath, diffBytes, 0o644)
		_ = workflow.RecordRunArtifact(ctx, akashic.ArtifactKindProposal, proposalPath)

		attempts = append(attempts, patch.AttemptRecord{
			Attempt:       attempt,
			Provider:      providerID,
			PatchArtifact: proposalArtifactName,
			Outcome:       outcome,
		})

		if outcome != attemptOutcomeOK {
			// unparseable / out_of_scope_diff は次 attempt へ continue。
			continue
		}

		// outcome=ok: previousProposals に追加 + DiffSummaryRecord 短縮表示。
		// Issue #46 input thinning: 永続化済み proposal-N.diff の絶対パスを Patch に
		// 載せ、次 attempt の renderPreviousProposalsSection が Source 行 + 切り詰めで
		// prompt 入力長を抑えられるようにする。
		proposal.AbsolutePath = proposalPath
		previousProposals = append(previousProposals, proposal)
		diffSummary := patch.BuildDiffSummary(promptContext.TargetGoFile, diffBytes, proposalArtifactName)
		diffSummaryRec = diffSummary
		onEventSafe(workflow.WorkflowEvent{
			Type: workflow.WorkflowText,
			Text: fmt.Sprintf("propose attempt %d: +%d -%d (full diff: %s; Run 詳細 Artifact pane で参照)",
				attempt, diffSummary.AddedLines, diffSummary.DeletedLines, diffSummary.FullDiffArtifact),
		})

		// ----- task 11.3: review 3 択 Select -----

		// ask == nil の場合は 11.2 互換で auto-regenerate (loop 継続)。
		// CLI 単独起動時の defensive 経路でもあり、副作用 (Snapshot/ApplyCheck/Apply/Revert) を
		// 一切呼ばないことで Wave 0 task 2.4 の no-side-effect 構造保証を維持する。
		if ask == nil {
			continue
		}

		selectedID, selErr := promptReviewSelection(ctx, ask, attempt, diffSummary)
		if selErr != nil {
			// Select 自体の失敗は precondition_failed に集約する (Req 1.7 系のユーザ意思取得不能)。
			return setVerdict(patch.VerdictPreconditionFailed,
				fmt.Sprintf("workflow.WorkflowAsk.Select 失敗: %v", selErr))
		}
		reviewSelections = append(reviewSelections, improveReviewSelection{
			Attempt:    attempt,
			SelectedID: selectedID,
			AtTime:     time.Now().UTC().Format(time.RFC3339Nano),
		})

		switch selectedID {
		case improveSelectRegenerate:
			// 次 attempt へ。review.json はまだ登録しない (regenerate は途中状態)。
			continue

		case improveSelectReject:
			// review.json 登録 → user_rejected verdict (IsIntent=true のため Fn は nil)
			recordReviewArtifact(ctx, runDir, runID, reviewSelections)
			return setVerdict(patch.VerdictUserRejected,
				fmt.Sprintf("ユーザが attempt %d で reject を選択", attempt))

		case improveSelectAccept:
			// review.json 登録 → apply フェーズへ
			recordReviewArtifact(ctx, runDir, runID, reviewSelections)
			return runApplyAndTestPhase(
				ctx, onEventSafe, deps,
				moduleRoot, attempt, proposal,
				&baselineRefRec, &testResultRec,
				setVerdict,
			)

		default:
			// ask が想定外の id を返した場合の defensive 経路。
			return setVerdict(patch.VerdictPreconditionFailed,
				fmt.Sprintf("workflow.WorkflowAsk.Select が想定外の id を返した: %q", selectedID))
		}
	}

	// 上限到達: 全 attempt が discard / regenerate された場合。
	return setVerdict(patch.VerdictRegenerateLoopAborted,
		fmt.Sprintf("MaxRegenerateAttempts (%d) 到達: 全試行 discard / regenerate", maxAttempts))
}

// promptReviewSelection は workflow.WorkflowAsk.Select で accept / reject / regenerate の 3 択を提示し、
// 選択 ID を返す (Req 4.1, 4.2)。ESC キャンセル (ok==false) は reject 扱い。
//
// 戻り値:
//   - selectedID: improveSelectAccept / improveSelectReject / improveSelectRegenerate のいずれか
//   - err: ask.Select 自体の失敗 (CLIAsk の Key 不在 / I/O エラー等)
func promptReviewSelection(
	ctx context.Context,
	ask workflow.WorkflowAsk,
	attempt int,
	diffSummary *patch.DiffSummaryRecord,
) (string, error) {
	prompt := fmt.Sprintf(
		"attempt %d の改善 diff を確認してください (+%d -%d)。全文は Run 詳細 Artifact pane の %s を参照してください。",
		attempt,
		diffSummary.AddedLines,
		diffSummary.DeletedLines,
		diffSummary.FullDiffArtifact,
	)
	id, ok, err := ask.Select(ctx, workflow.SelectParams{
		Key:    improveSelectKey,
		Prompt: prompt,
		Items: []workflow.SelectItem{
			{ID: improveSelectAccept, Label: "accept (適用 + go test ./...)"},
			{ID: improveSelectReject, Label: "reject (破棄して終了)"},
			{ID: improveSelectRegenerate, Label: "regenerate (同コンテキストで再生成)"},
		},
	})
	if err != nil {
		return "", err
	}
	if !ok {
		// ESC キャンセル: ユーザの「破棄」意思として reject 扱いする。
		return improveSelectReject, nil
	}
	return id, nil
}

// runApplyAndTestPhase は accept 選択後の apply + go test + 必要なら revert 経路を駆動する
// (tasks.md 11.3 / Req 4.3, 4.6, 5.1〜5.7, 5.9)。
//
// baselineRefOut / testResultOut は orchestration 側の defer (result.json 登録) で参照される
// 集約 state へのポインタ。各フェーズ通過で順次埋まり、result.json に反映される。
//
// verdict 経路:
//   - Snapshot 失敗 → VerdictGitApplyFailed (precheck 通過後だが Snapshot で失敗するのは git レイヤ問題)
//   - ApplyCheck 失敗 → VerdictGitApplyFailed (Apply / Revert 不実行、BaselineRef は record のみ)
//   - Apply 失敗 → VerdictGitApplyFailed (ApplyCheck 通過後の race / 環境問題)
//   - go test 成功 → VerdictAppliedPassed (IsIntent=true / Fn nil)
//   - go test 失敗 + Revert 成功 → VerdictAppliedTestFailedReverted
//   - go test 失敗 + Revert 失敗 → VerdictRevertFailed + workflow.WorkflowText 復旧手順案内
func runApplyAndTestPhase(
	ctx context.Context,
	onEventSafe func(workflow.WorkflowEvent),
	deps improveWorkflowDeps,
	moduleRoot string,
	attempt int,
	proposal patch.Patch,
	baselineRefOut **patch.BaselineRefRecord,
	testResultOut **patch.TestResultRecord,
	setVerdict func(patch.Verdict, string) error,
) error {
	// Snapshot: HEAD コミット SHA を取得 (Req 5.1 / Resolved §7)。
	ref, snapErr := deps.gitOps.Snapshot(ctx, moduleRoot)
	if snapErr != nil {
		return setVerdict(patch.VerdictGitApplyFailed,
			fmt.Sprintf("attempt %d: GitOps.Snapshot 失敗: %v", attempt, snapErr))
	}
	// BaselineRef は Snapshot 成功時点で record のみに反映する (Req 5.9)。
	// targetFiles はそのまま参照されるため slice clone で副作用切り離す。
	*baselineRefOut = &patch.BaselineRefRecord{
		HeadCommit:  ref.HeadCommit,
		TargetFiles: append([]string(nil), proposal.TargetFiles...),
	}

	// ApplyCheck: git apply --check で適用可能性検証 (Req 5.2)。
	// 失敗 → git_apply_failed (Apply / Revert は実行せず、BaselineRef は record のみ / Req 5.3)。
	if checkErr := deps.gitOps.ApplyCheck(ctx, moduleRoot, proposal.DiffBytes); checkErr != nil {
		return setVerdict(patch.VerdictGitApplyFailed,
			fmt.Sprintf("attempt %d: GitOps.ApplyCheck 失敗: %v", attempt, checkErr))
	}

	// Apply: 作業ツリーへ反映 (Req 5.4)。
	// ApplyCheck 通過後の Apply 失敗は本来稀だが、race 等を防御的に git_apply_failed に集約する。
	if applyErr := deps.gitOps.Apply(ctx, moduleRoot, proposal.DiffBytes); applyErr != nil {
		return setVerdict(patch.VerdictGitApplyFailed,
			fmt.Sprintf("attempt %d: GitOps.Apply 失敗: %v", attempt, applyErr))
	}

	// GoTestRunner.Run: 子プロセスで `go test ./... -count=1` 実行 (Req 5.5)。
	onEventSafe(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: fmt.Sprintf("attempt %d: go test 実行中…", attempt)})
	testRes, testErr := deps.gotest.Run(ctx, moduleRoot)
	*testResultOut = &patch.TestResultRecord{
		Executed:   true,
		ExitCode:   testRes.ExitCode,
		DurationMS: testRes.DurationMS,
		StdoutTail: testRes.StdoutTail,
		StderrTail: testRes.StderrTail,
		Truncated:  testRes.Truncated,
	}

	// テスト成功判定: error == nil かつ ExitCode == 0 のみ「テスト成功」。
	// (テスト失敗は exec エラーではないため testErr nil + ExitCode 非ゼロもありうる)
	if testErr == nil && testRes.ExitCode == 0 {
		// applied_passed: IsIntent=true → Fn は nil 返却、変更は作業ブランチに残る (Req 5.7)。
		return setVerdict(patch.VerdictAppliedPassed,
			fmt.Sprintf("attempt %d: apply + go test pass (duration=%dms)", attempt, testRes.DurationMS))
	}

	// テスト失敗 → Revert で baseline に戻す (Req 5.6)。
	revertErr := deps.gitOps.Revert(ctx, moduleRoot, ref, proposal.TargetFiles)
	if revertErr != nil {
		// Revert 失敗 → revert_failed verdict + 復旧手順案内 (Req 5.9)。
		//
		// 設計判断: spec L221 / design.md L1005 は「BaselineRef + cause を result.json に保存」と
		// プロセを記述するが、`BaselineRefRecord` 型 (design.md §Logical Data Model L851-856) には
		// `cause` フィールドが存在せず、`ResultRecord` も独立した cause フィールドを持たない。
		// そのため cause は `Summary` 文字列に埋め込み、result.json の `summary` に文字列として保存する。
		// `BaselineRefRecord` (HeadCommit + TargetFiles) は orchestrate 側 state の `baselineRefRec`
		// が Snapshot 成功時点で record されており、defer の result.json 書き出しで `baseline_ref` に
		// そのまま反映される。これにより spec の意図 (BaselineRef + cause が result.json に揃う) は
		// 「BaselineRefRecord は構造化フィールド + cause は Summary 文字列」の組合せで満たされる。
		// 構造化 cause フィールドの追加が必要な場合は後続 spec 改訂 + ResultRecord 拡張で対応する。
		emitRevertRecoveryGuidance(onEventSafe, ref.HeadCommit, proposal.TargetFiles)
		return setVerdict(patch.VerdictRevertFailed,
			fmt.Sprintf("attempt %d: go test 失敗 (exit=%d) 後の Revert 自体が失敗: %v (manual recovery required)",
				attempt, testRes.ExitCode, revertErr))
	}
	// Revert 成功: applied_test_failed_reverted。
	summary := fmt.Sprintf("attempt %d: apply 後 go test 失敗 (exit=%d)、Revert 成功", attempt, testRes.ExitCode)
	if testErr != nil {
		summary = fmt.Sprintf("attempt %d: apply 後 go test 異常終了 (err=%v / exit=%d)、Revert 成功",
			attempt, testErr, testRes.ExitCode)
	}
	return setVerdict(patch.VerdictAppliedTestFailedReverted, summary)
}

// recordReviewArtifact は review.json を runDir に書き出し、ReviewKind の artifact として登録する。
// accept / reject 経路で 1 度ずつ呼ばれる (regenerate 中継では呼ばない)。
//
// best-effort: 書き出し / 登録失敗は握りつぶす (akashic-artifacts spec Req 2.6)。
func recordReviewArtifact(
	ctx context.Context,
	runDir string,
	runID string,
	selections []improveReviewSelection,
) {
	rec := improveReviewRecord{
		WorkflowRunID: runID,
		Selections:    append([]improveReviewSelection(nil), selections...),
	}
	body, _ := json.MarshalIndent(rec, "", "  ")
	path := filepath.Join(runDir, improveReviewJSONFile)
	_ = os.WriteFile(path, body, 0o644)
	_ = workflow.RecordRunArtifact(ctx, akashic.ArtifactKindReview, path)
}

// emitRevertRecoveryGuidance は Revert 失敗時に workflow.WorkflowText で `git checkout <ref> -- <file>`
// 形式の手動復旧手順をユーザに案内する (Req 5.9 / Resolved §7)。
func emitRevertRecoveryGuidance(onEvent func(workflow.WorkflowEvent), headCommit string, targetFiles []string) {
	var b strings.Builder
	b.WriteString("revert 失敗 (manual recovery required):\n")
	for _, f := range targetFiles {
		b.WriteString(fmt.Sprintf("  git checkout %s -- %s\n", headCommit, f))
	}
	onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: b.String()})
}

// classifyExtractOutcome は Patch.ExtractAndGuard の戻り error を AttemptRecord.Outcome 文字列に変換する
// (design.md §AttemptRecord / Req 3.4, 3.5)。
//
// 戻り値:
//   - nil → "ok" (= 抽出成功、レビュー候補)
//   - errors.Is(err, ErrUnparseable) → string(VerdictProposalUnparseable) ("proposal_unparseable")
//   - errors.Is(err, ErrOutOfScope) → string(VerdictOutOfScopeDiff) ("out_of_scope_diff")
//   - その他の error → "proposal_unparseable" (安全側 fallback)
func classifyExtractOutcome(err error) string {
	if err == nil {
		return attemptOutcomeOK
	}
	if errors.Is(err, patch.ErrOutOfScope) {
		return string(patch.VerdictOutOfScopeDiff)
	}
	if errors.Is(err, patch.ErrUnparseable) {
		return string(patch.VerdictProposalUnparseable)
	}
	return string(patch.VerdictProposalUnparseable)
}

// buildAgentCallOptions は providerID + attempt に応じた workflowctx.CallAgentOptions を構築する。
//
//   - providerID=claude / codex: OutputFormat=JSONSchemaForDiff で構造化出力を強制 (Provider Compatibility Matrix)
//   - providerID=cursor / cursor-sdk / 未確定: OutputFormat=nil (cursor は CLI 経由で OutputFormat が no-op)
//   - StepID / StepName は attempt 番号入りでユニーク化 (akashic 集計で attempt 別 cost を分離)
func buildAgentCallOptions(providerID string, attempt int) *workflowctx.CallAgentOptions {
	stepID := fmt.Sprintf("propose-attempt-%d", attempt)
	stepName := fmt.Sprintf("propose attempt %d", attempt)
	switch providerID {
	case string(provider.ProviderClaude), string(provider.ProviderCodex):
		return &workflowctx.CallAgentOptions{
			OutputFormat: patch.JSONSchemaForDiff,
			StepID:       stepID,
			StepName:     stepName,
		}
	default:
		return &workflowctx.CallAgentOptions{
			StepID:   stepID,
			StepName: stepName,
		}
	}
}

// registryHasWorkflowID は registryFn() の結果に targetWorkflowID が含まれるかを返す
// (Req 1.4 の未知 workflow ID 拒否)。
//
// 設計契約:
//   - registryFn が nil の場合は false を返す (closure 注入忘れに対する防御)
//   - 空文字列 / 空白のみの ID は false を返す (防御的に拒否)
//   - registry に登録されている ID とのみ完全一致する (case-sensitive)
//   - registry の構築は副作用を持たない (各 workflow.Definition.Fn は値として保持されるだけで実行されない)
func registryHasWorkflowID(registryFn func() []workflow.Definition, targetWorkflowID string) bool {
	if registryFn == nil {
		return false
	}
	if strings.TrimSpace(targetWorkflowID) == "" {
		return false
	}
	for _, def := range registryFn() {
		if def.ID == targetWorkflowID {
			return true
		}
	}
	return false
}
