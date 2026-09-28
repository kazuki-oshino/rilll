// Package apply の本ファイルは task 5.4 + 5.5 / design.md §Workflow §accept §commit §中断
// §revert 失敗 / Req 5.1, 5.2, 5.3, 5.4, 5.5, 5.6, 5.7, 6.1, 6.2, 6.3, 6.4, 7.4 を実装する。
//
// 役割: propose loop の `accept` 分岐から渡された `Patch` を、
// 副作用フェーズ (Snapshot → ApplyCheck → Apply → commit-message LLM → Add → Commit) を
// 経由して 1 review run = 1 commit に集約し、`VerdictApplied` / `VerdictGitApplyFailed` /
// `VerdictUserAborted` / `VerdictRevertFailed` のいずれかを `ResultRecord` にセットする。
//
// 5.4 が確定する verdict (本ファイル内 Apply 系統):
//   - VerdictApplied: commit 成功。
//   - VerdictGitApplyFailed: ApplyCheck 失敗 / Apply 失敗 / Commit 失敗 → revert 成功。
//
// 5.5 で本ファイルに追加した責務 (cancel-aware revert + RecoveryGuidance + Fn 振り分け):
//   - `runOrCancel(ctx, fn)` を `cancel_guard.go` に追加し、`gitOps.Apply` / `Add` / `Commit` /
//     `Revert` の 4 副作用呼び出しを本 helper 経由に統一する (5.6 invariant test の前提)。
//   - ctx.Done() 機能ガードを Apply 直後 / Add 直前 / Commit 直前 の 3 ポイントに挿入し、
//     検出時は `Revert(baseline, files)` を **非 cancel 済 ctx (context.Background())** で発火する。
//   - Revert 成功 → VerdictUserAborted (intent / Fn nil)。
//   - 中断時 Revert 失敗 → VerdictRevertFailed + RecoveryGuidance を ResultRecord と
//     WorkflowText の双方に出力。
//   - 5.4 の commit-message-fail / Add-fail / Commit-fail 経路でも Revert が失敗した場合は
//     5.4 がセットした VerdictGitApplyFailed を VerdictRevertFailed で **上書き** する
//     (RecoveryGuidance も同様に生成)。
//
// Fn 最終振り分け (intent → nil / 内部失敗 → `errors.New(ErrorPrefixForVerdict(...))`):
//   - 本ファイルでは `setRecordVerdict` (`record` のみ更新する 5.5 改修版) でのみ verdict を
//     確定し、Fn 戻り値の組み立ては `runApplyWorkflowOrchestration` の終端 finalize 段に
//     一本化する (5.5 で workflow.go に `finalizeReturn(record, retErr)` を追加)。
//   - これにより「intent vs error の分岐は record.FinalVerdict.IsIntent() に基づき 1 箇所で決定」
//     という不変式 (Req 6.3 / design.md §Verdict) が構造的に守られる。
//
// 副作用契約:
//   - Snapshot は最大 2 回呼ぶ: (a) baseline 取得時、(b) commit 成功後の HEAD 取得時。
//   - ApplyCheck 失敗経路では Apply / Add / Commit / Revert を呼ばない。
//   - Apply 失敗経路では Add / Commit / Revert を呼ばない (Apply 自体が失敗のため revert 不要)。
//   - Apply 直後 cancel 経路では commit-message LLM / Add / Commit を呼ばずに Revert を発火する。
//   - Add 失敗経路では Commit を呼ばずに `Revert(baseline, TargetFiles)` を呼ぶ。
//   - Commit 失敗経路では `Revert(baseline, TargetFiles)` を呼ぶ。
//   - commit-message LLM の応答が空文字列 / err の経路でも Apply 済みの差分が
//     作業ツリーに残っているため `Revert(baseline, TargetFiles)` で baseline へ戻す。
//
// commit メッセージ (Req 5.4):
//   - `RenderCommitMessage(CommitMessageInput)` で reviewRunID + 対象 rule 名を
//     preamble に含む prompt を組み立て、`agent.CallAgent` で取得する。
//   - StepID は `review-apply-commit-message` 固定 (1 review run = 1 commit / Req 5.7)。
//
// ResultRecord 反映:
//   - `record.BaselineCommit` ← Snapshot (1 回目) の `HeadCommit`。
//   - `record.CommitSHA` ← Snapshot (2 回目) の `HeadCommit` (commit 成功経路でのみ非空)。
//   - 失敗経路でも `record.BaselineCommit` は記録される (Snapshot 1 回目通過後に失敗する経路)。
//   - `record.RecoveryGuidance` ← Revert 失敗経路でのみ非 nil (Req 5.6, 6.2)。
package apply

import (
	"context"
	"errors"
	"fmt"

	"github.com/kazuki-oshino/rilll/internal/workflow"
	improvepatch "github.com/kazuki-oshino/rilll/internal/workflow/builtin/improve/patch"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// stepIDCommitMessage は commit-message LLM 呼び出しの StepID 固定値。
// 1 review run = 1 commit (Req 5.7) のため attempt suffix は付けない (plan/diff/self-review と異なる)。
const stepIDCommitMessage = "review-apply-commit-message"

// stepNameCommitMessage は commit-message LLM 呼び出しの人間可読 StepName。
const stepNameCommitMessage = "review apply commit message"

// newApplyHook は task 5.4 の apply phase を `ProposeApplyHook` seam に適合させる factory。
//
// 戻り値の closure は propose loop の accept 分岐から `applyHook(ctx, ...)` 形で呼び出され、
// `record.FinalVerdict` を `VerdictApplied` または `VerdictGitApplyFailed` に確定して return する。
//
// 5.5 (cancel-aware revert + RecoveryGuidance + Fn 最終振り分け) で本 hook の closure 引数 /
// 戻り値契約を変更する場合、`ProposeApplyHook` 型自体を改訂し本 factory を追従させる必要がある。
func newApplyHook(gitOps GitOpsExt, agent workflowctx.AgentCaller) ProposeApplyHook {
	return func(
		ctx context.Context,
		cwd string,
		_ string, // applyRunID は本 phase では参照しない (record.WorkflowRunID で既に保持済)
		_ string, // runDir は本 phase では参照しない (cycle artifact は propose loop 側で書き出し済)
		reviewRunID string,
		onEvent func(workflow.WorkflowEvent),
		record *ResultRecord,
		loaded *LoadResult,
		patch Patch,
	) error {
		return runApplyPhase(ctx, cwd, reviewRunID, onEvent, record, loaded, patch, gitOps, agent)
	}
}

// runApplyPhase は accept 経路の apply フェーズ本体 (design.md §Workflow §accept §commit §中断)。
//
// 処理順序 (厳守):
//  1. baseline Snapshot を取得し `record.BaselineCommit` に記録する。
//  2. ApplyCheck で適用可能性を検証する (失敗 → `VerdictGitApplyFailed`)。
//  3. Apply (`runOrCancel` 経由) で diff を作業ツリーに反映する。
//     - cancel 検出 → Apply 未実行のため revert 不要 → `VerdictUserAborted` (intent)
//     (注: Apply 起動前の cancel のみ; Apply 起動済の cancel は (3.5) で扱う)。
//     - Apply 失敗 → `VerdictGitApplyFailed` (revert 不要)。
//     3.5. Apply 直後 ctx.Done() ガード (Req 6.4):
//     - Apply は成功したが ctx が cancel 済 → 作業ツリーには差分が残っているため
//     Revert(baseline, files) を **非 cancel 済 ctx** で発火し、user_aborted / revert_failed に分岐。
//  4. commit-message LLM (`RenderCommitMessage` + `agent.CallAgent`) でメッセージ本文を生成する。
//     失敗 / 空応答経路では Revert 起動 + 5.4 の VerdictGitApplyFailed をセット。Revert 失敗時は
//     finalizeRevertOutcome が VerdictRevertFailed に上書き + RecoveryGuidance を生成する。
//  5. Add (`runOrCancel` 経由) で変更ファイルを index に積む。
//     - cancel 検出 (Add 直前ガード Req 6.4) → Revert + user_aborted / revert_failed。
//     - Add 失敗 → Revert + VerdictGitApplyFailed (revert 失敗時は VerdictRevertFailed に上書き)。
//  6. Commit (`runOrCancel` 経由) で 1 commit を生成する。
//     - cancel 検出 (Commit 直前ガード Req 6.4) → Revert + user_aborted / revert_failed。
//     - Commit 失敗 → Revert + VerdictGitApplyFailed (revert 失敗時は VerdictRevertFailed に上書き)。
//  7. commit 成功 → Snapshot 再実行で `record.CommitSHA` を埋め `VerdictApplied` (intent) をセットする。
//
// Fn 最終振り分け:
//   - 本関数は `setRecordVerdict` (5.5 改修により record のみ更新) で verdict を確定し、
//     `runApplyWorkflowOrchestration` の終端 `finalizeReturn` が `record.FinalVerdict.IsIntent()` を
//     参照して Fn 戻り値 (nil / `[verdict:<id>] <summary>`) を 1 箇所に集約する。
func runApplyPhase(
	ctx context.Context,
	cwd string,
	reviewRunID string,
	onEvent func(workflow.WorkflowEvent),
	record *ResultRecord,
	loaded *LoadResult,
	patch Patch,
	gitOps GitOpsExt,
	agent workflowctx.AgentCaller,
) error {
	// (0) 防御: loaded が nil なら apply phase は副作用なしで即座に失敗扱い。
	// propose_loop.runProposeLoop は loaded != nil を前提に hook を呼ぶ契約だが、
	// 将来的な再利用に備えて内部で nil 防御する。
	if loaded == nil {
		return setRecordVerdict(record, VerdictGitApplyFailed,
			"apply phase: loaded LoadResult が nil のため処理できません。")
	}

	// (1) baseline Snapshot
	baseline, err := gitOps.Snapshot(ctx, cwd)
	if err != nil {
		return setRecordVerdict(record, VerdictGitApplyFailed,
			fmt.Sprintf("baseline snapshot に失敗しました: %v", err))
	}
	record.BaselineCommit = baseline.HeadCommit
	emitWorkflowText(onEvent, fmt.Sprintf("baseline snapshot: %s", baseline.HeadCommit))

	// (2) ApplyCheck (失敗経路では Apply / Add / Commit / Revert を呼ばない)
	if err := gitOps.ApplyCheck(ctx, cwd, patch.DiffBytes); err != nil {
		return setRecordVerdict(record, VerdictGitApplyFailed,
			fmt.Sprintf("git apply --check に失敗しました: %v", err))
	}

	// (3) Apply (runOrCancel 経由 / 5.6 invariant test 必須)
	applyErr := runOrCancel(ctx, func() error { return gitOps.Apply(ctx, cwd, patch.DiffBytes) })
	if applyErr != nil {
		if isCtxCanceled(applyErr) {
			// Apply 起動前に cancel 検出 → 作業ツリーに差分が無いため Revert 不要 → user_aborted (intent)。
			return setRecordVerdict(record, VerdictUserAborted,
				"利用者により中断されました (Apply 起動前)。作業ツリーは変更されていません。")
		}
		return setRecordVerdict(record, VerdictGitApplyFailed,
			fmt.Sprintf("git apply に失敗しました: %v", applyErr))
	}

	// (3.5) Apply 直後 ctx.Done() ガード (Req 6.4):
	// Apply は成功したが ctx が cancel 済 → 作業ツリーには差分が残っているため Revert 必須。
	if err := ctx.Err(); err != nil {
		emitWorkflowText(onEvent, "中断要求を検出しました (Apply 直後)。Revert を起動します。")
		return finalizeCancelRevert(cwd, baseline, patch.TargetFiles, gitOps, record, onEvent,
			"利用者により中断されました (Apply 直後)。")
	}

	// (4) commit-message LLM (Req 5.4)
	commitPrompt := RenderCommitMessage(CommitMessageInput{
		ReviewRunID:  reviewRunID,
		Rules:        loaded.TargetRules,
		AppliedFiles: patch.TargetFiles,
	})
	commitResp, callErr := agent.CallAgent(ctx, commitPrompt, cwd, nil, "", &workflowctx.CallAgentOptions{
		StepID:   stepIDCommitMessage,
		StepName: stepNameCommitMessage,
	})
	if callErr != nil {
		// Apply 済みの差分が作業ツリーに残っているため baseline まで戻す。
		// Revert 失敗時は finalizeApplyFailureRevert が VerdictRevertFailed + RecoveryGuidance に上書きする。
		return finalizeApplyFailureRevert(cwd, baseline, patch.TargetFiles, gitOps, record, onEvent,
			fmt.Sprintf("commit-message 生成 LLM の呼び出しに失敗しました: %v", callErr))
	}
	commitMessage := agentResponseContent(commitResp)
	if commitMessage == "" {
		return finalizeApplyFailureRevert(cwd, baseline, patch.TargetFiles, gitOps, record, onEvent,
			"commit-message 生成 LLM が空応答を返しました。")
	}

	// (5) Add 直前 ctx.Done() ガード (runOrCancel 経由 / Req 6.4)
	addErr := runOrCancel(ctx, func() error { return gitOps.Add(ctx, cwd, patch.TargetFiles) })
	if addErr != nil {
		if isCtxCanceled(addErr) {
			emitWorkflowText(onEvent, "中断要求を検出しました (Add 直前)。Revert を起動します。")
			return finalizeCancelRevert(cwd, baseline, patch.TargetFiles, gitOps, record, onEvent,
				"利用者により中断されました (Add 直前)。")
		}
		return finalizeApplyFailureRevert(cwd, baseline, patch.TargetFiles, gitOps, record, onEvent,
			fmt.Sprintf("git add に失敗しました: %v", addErr))
	}

	// (6) Commit 直前 ctx.Done() ガード (runOrCancel 経由 / Req 6.4)
	commitErr := runOrCancel(ctx, func() error { return gitOps.Commit(ctx, cwd, commitMessage, patch.TargetFiles) })
	if commitErr != nil {
		if isCtxCanceled(commitErr) {
			emitWorkflowText(onEvent, "中断要求を検出しました (Commit 直前)。Revert を起動します。")
			return finalizeCancelRevert(cwd, baseline, patch.TargetFiles, gitOps, record, onEvent,
				"利用者により中断されました (Commit 直前)。")
		}
		return finalizeApplyFailureRevert(cwd, baseline, patch.TargetFiles, gitOps, record, onEvent,
			fmt.Sprintf("git commit に失敗しました: %v", commitErr))
	}

	// (7) commit 成功 → 新 HEAD SHA を CommitSHA に記録 (best-effort)
	post, postErr := gitOps.Snapshot(ctx, cwd)
	if postErr != nil {
		// commit は成功しているため VerdictApplied のまま、CommitSHA は空とする
		// (post-commit Snapshot 失敗を理由に applied verdict を覆さない)。
		emitWorkflowText(onEvent, fmt.Sprintf("post-commit snapshot に失敗しました (CommitSHA は空のまま記録します): %v", postErr))
	} else {
		record.CommitSHA = post.HeadCommit
		emitWorkflowText(onEvent, fmt.Sprintf("commit %s を生成しました (review-run-id=%s)", post.HeadCommit, reviewRunID))
	}

	return setRecordVerdict(record, VerdictApplied,
		fmt.Sprintf("review-apply-workflow が %d 件の rule に対する修正を 1 commit に集約しました。", len(loaded.TargetRules)))
}

// isCtxCanceled は err が context.Canceled / DeadlineExceeded のいずれかであるかを返す。
// runOrCancel が ctx.Err() を返した経路 (副作用未実行) と副作用そのものが失敗した経路を
// 識別するための判定 helper。
func isCtxCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// finalizeCancelRevert は ctx.Done() 検出経路の Revert を非 cancel 済 ctx で発火し、
// 結果に応じて VerdictUserAborted (revert 成功) / VerdictRevertFailed (revert 失敗) を
// `record` に確定する (Req 6.1, 6.2, 6.4)。
//
// 戻り値は `setRecordVerdict` 同等の record-only 更新 helper の戻り値で、Fn 終端の
// `finalizeReturn` が IsIntent を見て Fn 戻り値を組み立てる契約 (5.5 routing 集約)。
//
// Revert は **`context.Background()`** で起動する: caller の ctx は cancel 済のため、
// 同じ ctx を `runOrCancel` 経由で渡すと `select <-ctx.Done()` が即時に発火し、Revert 自体が
// 走らずに RecoveryGuidance が生成される循環不変式違反となる。これは Req 6.4 の意図
// (中断時こそ revert を完遂する) と相反するため、ここで明示的に bg ctx を採用する。
func finalizeCancelRevert(
	cwd string,
	baseline improvepatch.BaselineRef,
	files []string,
	gitOps GitOpsExt,
	record *ResultRecord,
	onEvent func(workflow.WorkflowEvent),
	abortNote string,
) error {
	bgCtx := context.Background()
	revertErr := runOrCancel(bgCtx, func() error { return gitOps.Revert(bgCtx, cwd, baseline, files) })
	if revertErr == nil {
		// revert 成功 → 中断 + 復元完了 → user_aborted (intent)。
		return setRecordVerdict(record, VerdictUserAborted,
			abortNote+" 作業ツリーは元の状態に戻りました。")
	}

	// revert 失敗 → RecoveryGuidance を生成し VerdictRevertFailed に確定する。
	note := abortNote + " ただし Revert 自体に失敗したため、未復旧ファイルが残っています。下記コマンドで手動復旧してください。"
	guidance := buildRevertRecoveryGuidance(baseline, files, note)
	record.RecoveryGuidance = guidance
	emitRecoveryGuidanceText(onEvent, guidance, revertErr)
	return setRecordVerdict(record, VerdictRevertFailed,
		fmt.Sprintf("中断後の Revert に失敗しました: %v", revertErr))
}

// finalizeApplyFailureRevert は commit-message-fail / Add-fail / Commit-fail 経路の Revert を
// 起動し、結果に応じて 5.4 の VerdictGitApplyFailed (revert 成功) または 5.5 の VerdictRevertFailed
// (revert 失敗) に振り分ける (Req 5.5, 5.6)。
//
// Revert は **caller の ctx (apply phase の親 ctx)** をそのまま使う: ctx.Done() 経路は
// (3.5) / (5) / (6) のガードで先に捕捉済のため、ここに到達した時点で ctx.Err()==nil である契約。
// 防御として bgCtx fallback は不要 (上位ガードが先に発火する設計不変式)。
func finalizeApplyFailureRevert(
	cwd string,
	baseline improvepatch.BaselineRef,
	files []string,
	gitOps GitOpsExt,
	record *ResultRecord,
	onEvent func(workflow.WorkflowEvent),
	failureSummary string,
) error {
	// caller ctx は本到達点では cancel 済でない契約だが、Revert は背景 ctx で発火するのが安全
	// (commit 失敗直後に親 ctx が cancel された競合状態でも Revert を完遂させる)。
	bgCtx := context.Background()
	revertErr := runOrCancel(bgCtx, func() error { return gitOps.Revert(bgCtx, cwd, baseline, files) })
	if revertErr == nil {
		// 5.4 既存挙動: revert 成功で VerdictGitApplyFailed (commit メッセージ生成失敗 / Add 失敗 /
		// Commit 失敗 のいずれも合流する集約 verdict)。
		return setRecordVerdict(record, VerdictGitApplyFailed, failureSummary)
	}

	// 5.5: revert 失敗で 5.4 の VerdictGitApplyFailed を VerdictRevertFailed に上書きし、
	// RecoveryGuidance を ResultRecord と WorkflowText の双方に出力する (Req 5.6)。
	note := "git 操作に失敗した後の Revert にも失敗しました。下記コマンドで手動復旧してください。"
	guidance := buildRevertRecoveryGuidance(baseline, files, note)
	record.RecoveryGuidance = guidance
	emitRecoveryGuidanceText(onEvent, guidance, revertErr)
	return setRecordVerdict(record, VerdictRevertFailed,
		fmt.Sprintf("%s さらに Revert にも失敗したため未復旧ファイルが残っています: %v", failureSummary, revertErr))
}

// emitRecoveryGuidanceText は RecoveryGuidance を WorkflowText としても利用者に提示する
// (Req 5.6: result.json と利用者向け表示の双方に出力)。
//
// 出力フォーマット (1 イベントに集約; 改行込みで複数行表示):
//
//	revert に失敗しました: <revertErr>
//	未復旧ファイル: file1, file2
//	手動復旧コマンド:
//	  git checkout <sha> -- file1
//	  git checkout <sha> -- file2
//	備考: <note>
func emitRecoveryGuidanceText(onEvent func(workflow.WorkflowEvent), guidance *RecoveryGuidance, revertErr error) {
	if onEvent == nil || guidance == nil {
		return
	}
	text := fmt.Sprintf("revert に失敗しました: %v\n", revertErr)
	if len(guidance.Files) > 0 {
		text += "未復旧ファイル: "
		for i, f := range guidance.Files {
			if i > 0 {
				text += ", "
			}
			text += f
		}
		text += "\n"
	}
	if len(guidance.Commands) > 0 {
		text += "手動復旧コマンド:\n"
		for _, c := range guidance.Commands {
			text += "  " + c + "\n"
		}
	}
	if guidance.Note != "" {
		text += "備考: " + guidance.Note
	}
	onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: text})
}
