// Package apply は review-apply-workflow の実装を提供する
// (design.md §Architecture / requirements.md §Requirements)。
//
// 本ファイルは review-apply 専用 Verdict 列挙 (closed set 10 値 + VerdictUnset) と
// IsIntent / ErrorPrefixForVerdict helper を提供する
// (design.md §Verdict Catalog / Req 6.3, 7.4)。
//
// 設計契約:
//   - 列挙値は文字列定数の closed set として固定する。10 値外の verdict が
//     使われた場合は requirements.md と design.md の改訂を伴う変更とする
//     (Invariants: design.md §Verdict Catalog)。
//   - VerdictUnset は最終 verdict として `result.json.final_verdict` に
//     書き出してはならない (Req 7.4 / design.md §Verdict Invariants)。
//   - IsIntent() == true の verdict (`applied` / `no_findings` / `user_aborted`)
//     は orchestration 側で Fn が nil を返し、`workflow_runs.error` は空のまま
//     となる (Req 6.3 / design.md §Verdict)。
//   - 残り 7 値 (パイプライン / システム失敗) では ErrorPrefixForVerdict が
//     `[verdict:<id>] <summary>` 形のエラー文字列を返し、orchestration が
//     `errors.New(ErrorPrefixForVerdict(...))` で Fn から返却する
//     (improve-workflow 既存規約と整合)。
package apply

import "fmt"

// Verdict は review-apply-workflow の最終結果を表す closed set 列挙の文字列型。
// design.md §Verdict Catalog 規定の 10 値以外を取らない契約。
// VerdictUnset (空文字) は初期化値専用で、最終 verdict としては書き出さない。
type Verdict string

const (
	// VerdictUnset は Verdict 未設定 (初期化値) を表す。
	// 最終 verdict として `result.json.final_verdict` に書き出してはならない
	// (Req 7.4 / design.md §Verdict Invariants)。
	VerdictUnset Verdict = ""

	// VerdictApplied は accept → apply → commit 成功で patch が作業ツリー + commit に残った状態。
	// IsIntent==true (Fn は nil を返す)。Req 5.3, 5.7。
	VerdictApplied Verdict = "applied"

	// VerdictNoFindings は findings_reported な rule が 0 件で、副作用ゼロで終了した状態。
	// IsIntent==true (Fn は nil を返す)。Req 1.6。
	VerdictNoFindings Verdict = "no_findings"

	// VerdictUserAborted は ESC / SIGINT 中断 + revert 成功で作業ツリーが baseline に復元された状態。
	// IsIntent==true (Fn は nil を返す)。Req 6.1。
	VerdictUserAborted Verdict = "user_aborted"

	// VerdictCycleExhausted は self-review が MaxRegenerateAttempts (default 5) 連続 regenerate で
	// 上限到達したため副作用なく終了した状態。
	// IsIntent==false。Req 4.3。
	VerdictCycleExhausted Verdict = "cycle_exhausted"

	// VerdictGitApplyFailed は ApplyCheck / Apply / Commit 失敗 (revert 成功) の状態。
	// IsIntent==false。Req 5.2, 5.5。
	VerdictGitApplyFailed Verdict = "git_apply_failed"

	// VerdictRevertFailed は Revert 自体が失敗し、未復旧ファイルが残った状態
	// (orchestration 側で RecoveryGuidance を result.json と利用者向け表示の双方に出力)。
	// IsIntent==false。Req 5.6, 6.2。
	VerdictRevertFailed Verdict = "revert_failed"

	// VerdictProposalUnparseable は AI 提案 diff を unified diff として解釈できなかった状態。
	// IsIntent==false。Req 3.4。
	VerdictProposalUnparseable Verdict = "proposal_unparseable"

	// VerdictOutOfScopeDiff は AI 提案 diff が指摘対象から導かれる修正範囲外を変更しようとした状態
	// (scope guard 違反)。
	// IsIntent==false。Req 3.5。
	VerdictOutOfScopeDiff Verdict = "out_of_scope_diff"

	// VerdictPreconditionFailed は precheck フェーズ (run id 不在 / dirty / schema 違反等) で
	// 副作用ゼロで停止した状態。
	// IsIntent==false。Req 1.3, 2.2。
	VerdictPreconditionFailed Verdict = "precondition_failed"

	// VerdictFailed は self-review LLM 呼び出し失敗や想定外の内部失敗で終了した状態。
	// IsIntent==false。Req 4.5。
	VerdictFailed Verdict = "failed"
)

// IsIntent は verdict が「ユーザ意思 / 正常成功」を表すかを返す。
// true のとき orchestration 側は Fn から nil を返し、`workflow_runs.error` は空のままとする
// (design.md §Verdict / Req 6.3)。
//
// 該当 verdict: VerdictApplied / VerdictNoFindings / VerdictUserAborted の 3 値のみ。
// 残り 7 値 (パイプライン / システム失敗) および VerdictUnset では false を返し、
// orchestration 側は ErrorPrefixForVerdict 付き error を Fn から返す。
//
// VerdictUnset は intent 経路に乗せると `result.json.final_verdict` に空文字が書き出される
// invariant 違反を招くため、明示的に false を返して防御する。
func (v Verdict) IsIntent() bool {
	switch v {
	case VerdictApplied, VerdictNoFindings, VerdictUserAborted:
		return true
	default:
		return false
	}
}

// ErrorPrefixForVerdict は `[verdict:<id>] <summary>` 形のエラー文字列を返す。
// improve-workflow 既存の `[verdict:applied_passed] ...` 規約と整合させ、
// akashic-query 表示・集計経路で verdict 別の集計が容易になるよう統一する
// (design.md §Verdict / improve/patch.ErrorPrefixForVerdict と同形)。
//
// summary は単一行 (改行を含まない) を前提とする (design.md §Verdict Preconditions)。
func ErrorPrefixForVerdict(v Verdict, summary string) string {
	return fmt.Sprintf("[verdict:%s] %s", string(v), summary)
}
