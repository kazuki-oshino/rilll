// Package improvepatch の本ファイルは Verdict 列挙 (closed set 10 値) と
// IsIntent / ErrorPrefixForVerdict helper を提供する
// (design.md §Verdict / Req 6.2, 6.5)。
//
// 設計契約:
//   - 列挙値は文字列定数の closed set として固定する。10 値外の verdict が
//     使われた場合は requirements.md と design.md の改訂を伴う変更とする
//     (Invariants: design.md §Verdict)。
//   - IsIntent() == true の verdict (`applied_passed` / `user_rejected`) は
//     orchestration 側で Fn が nil を返し、`workflow_runs.error` は空文字列
//     のままとなる (Q1 確定 / design.md §Verdict)。
//   - それ以外 8 値 (パイプライン / システム失敗) は ErrorPrefixForVerdict が
//     `[verdict:<id>] <summary>` 形のエラー文字列を返し、`Definition.Run` が
//     `workflow_runs.error` に格納する (既存 `[stream_idle_timeout]` パターンと整合)。
package patch

import "fmt"

// Verdict は improve-workflow の最終結果を表す closed set 列挙の文字列型。
// design.md §Verdict 規定の 10 値以外を取らない契約。
type Verdict string

const (
	// VerdictAppliedPassed は accept → apply → go test 成功で patch が作業ツリーに残った状態。
	// IsIntent==true (Fn は nil を返す)。Req 5.7。
	VerdictAppliedPassed Verdict = "applied_passed"

	// VerdictUserRejected はユーザがレビューで reject を選択し、patch を適用せずに終了した状態。
	// IsIntent==true (Fn は nil を返す)。Req 4.4。
	VerdictUserRejected Verdict = "user_rejected"

	// VerdictAppliedTestFailedReverted は apply 後 go test が失敗し、Revert で baseline 復元された状態。
	// IsIntent==false (Fn は ErrorPrefixForVerdict 付き error を返す)。Req 5.6。
	VerdictAppliedTestFailedReverted Verdict = "applied_test_failed_reverted"

	// VerdictProposalUnparseable は AI レスポンスを unified diff として解釈できず破棄した状態。
	// IsIntent==false。Req 3.4 / 5.8。
	VerdictProposalUnparseable Verdict = "proposal_unparseable"

	// VerdictOutOfScopeDiff は AI 提案 diff が対象 Go ファイル以外を変更しようとした状態。
	// IsIntent==false。Req 3.5 / 5.8。
	VerdictOutOfScopeDiff Verdict = "out_of_scope_diff"

	// VerdictGitApplyFailed は `git apply --check` が失敗して Apply 未実行となった状態
	// (Apply 未実行のため Revert 不要、BaselineRef は record のみ)。
	// IsIntent==false。Req 5.3。
	VerdictGitApplyFailed Verdict = "git_apply_failed"

	// VerdictPreconditionFailed は precheck (git status / module path / target resolve) が失敗した状態。
	// IsIntent==false。Req 1.2, 1.4, 1.5, 1.6, 7.4。
	VerdictPreconditionFailed Verdict = "precondition_failed"

	// VerdictContextLoadFailed は akashic Reader / ArtifactReader へのアクセスが失敗した状態。
	// IsIntent==false。Req 2.6。
	VerdictContextLoadFailed Verdict = "context_load_failed"

	// VerdictRevertFailed は Revert (`git checkout <BaselineRef> -- <TargetFiles>`) 自体が失敗した状態
	// (orchestration 側で BaselineRef + TargetFiles + cause を result.json に保存し、
	//  手動復旧手順をユーザに案内する)。
	// IsIntent==false。Req 5.9。
	VerdictRevertFailed Verdict = "revert_failed"

	// VerdictRegenerateLoopAborted は regenerate ループが MaxRegenerateAttempts (default 5) に到達して
	// 強制脱出した状態。
	// IsIntent==false。Q2 確定 / design.md §Verdict。
	VerdictRegenerateLoopAborted Verdict = "regenerate_loop_aborted"
)

// IsIntent は verdict が「ユーザ意思 / 正常成功」を表すかを返す。
// true のとき orchestration 側は Fn から nil を返し、`workflow_runs.error` は空のままとする
// (Q1 確定 / design.md §Verdict)。
//
// 該当 verdict: VerdictAppliedPassed / VerdictUserRejected の 2 値のみ。
// 残り 8 値 (パイプライン / システム失敗) では false を返し、
// orchestration 側は ErrorPrefixForVerdict 付き error を Fn から返す。
func (v Verdict) IsIntent() bool {
	switch v {
	case VerdictAppliedPassed, VerdictUserRejected:
		return true
	default:
		return false
	}
}

// ErrorPrefixForVerdict は `[verdict:<id>] <summary>` 形のエラー文字列を返す。
// 既存 kiro_impl_loop の `[stream_idle_timeout]` プレフィックス形と整合させ、
// akashic-query 表示・集計経路で verdict 別の集計が容易になるよう統一する
// (design.md §Verdict / Resolved Decisions §1)。
//
// summary は単一行 (改行を含まない) を前提とする (design.md §Verdict Preconditions)。
func ErrorPrefixForVerdict(v Verdict, summary string) string {
	return fmt.Sprintf("[verdict:%s] %s", string(v), summary)
}
