// Package improvepatch のテスト本ファイルは Verdict 列挙 (closed set 10 値) と
// IsIntent / ErrorPrefixForVerdict helper の契約を検証する
// (tasks.md 2.1 / design.md §Verdict / Req 6.2, 6.5)。
//
// 検証観点:
//   - 列挙文字列値の不変性 (akashic-mode-tui 表示契約と整合)
//   - IsIntent: applied_passed / user_rejected の 2 値で true、残り 8 値で false
//   - ErrorPrefixForVerdict: `[verdict:<id>] <summary>` 形 (既存 `[stream_idle_timeout]` パターンと整合)
package patch

import "testing"

// TestVerdictStringValues は 10 値の Verdict 文字列定数が design.md §Verdict で
// 規定された不変文字列と一致することを検証する。
// この値は akashic-mode-tui の表示契約 / result.json schema / workflow_runs.error
// プレフィックスで参照されるため、誤って変更されないことを機械検証する。
func TestVerdictStringValues(t *testing.T) {
	cases := []struct {
		verdict Verdict
		want    string
	}{
		{VerdictAppliedPassed, "applied_passed"},
		{VerdictUserRejected, "user_rejected"},
		{VerdictAppliedTestFailedReverted, "applied_test_failed_reverted"},
		{VerdictProposalUnparseable, "proposal_unparseable"},
		{VerdictOutOfScopeDiff, "out_of_scope_diff"},
		{VerdictGitApplyFailed, "git_apply_failed"},
		{VerdictPreconditionFailed, "precondition_failed"},
		{VerdictContextLoadFailed, "context_load_failed"},
		{VerdictRevertFailed, "revert_failed"},
		{VerdictRegenerateLoopAborted, "regenerate_loop_aborted"},
	}
	for _, tc := range cases {
		if string(tc.verdict) != tc.want {
			t.Errorf("verdict string mismatch: got %q, want %q", string(tc.verdict), tc.want)
		}
	}
	if got, want := len(cases), 10; got != want {
		t.Fatalf("expected closed set of %d verdicts, got %d", want, got)
	}
}

// TestVerdictIsIntent は IsIntent() が「ユーザ意思 / 正常成功」verdict (2 値) で true、
// パイプライン / システム失敗 verdict (8 値) で false を返すことを検証する。
// IsIntent==true のとき orchestration 側は Fn から nil を返す契約 (Q1 確定 / design.md §Verdict)。
func TestVerdictIsIntent(t *testing.T) {
	cases := []struct {
		verdict Verdict
		want    bool
	}{
		{VerdictAppliedPassed, true},
		{VerdictUserRejected, true},
		{VerdictAppliedTestFailedReverted, false},
		{VerdictProposalUnparseable, false},
		{VerdictOutOfScopeDiff, false},
		{VerdictGitApplyFailed, false},
		{VerdictPreconditionFailed, false},
		{VerdictContextLoadFailed, false},
		{VerdictRevertFailed, false},
		{VerdictRegenerateLoopAborted, false},
	}
	for _, tc := range cases {
		if got := tc.verdict.IsIntent(); got != tc.want {
			t.Errorf("IsIntent(%q): got %v, want %v", string(tc.verdict), got, tc.want)
		}
	}
	if got, want := len(cases), 10; got != want {
		t.Fatalf("expected coverage of all %d verdicts, got %d", want, got)
	}
}

// TestErrorPrefixForVerdict は ErrorPrefixForVerdict が `[verdict:<id>] <summary>` 形を
// 返すことを 10 値全てに対して検証する。既存 `[stream_idle_timeout]` パターン
// (kiro_impl_loop.formatStepError) と整合する形式。
func TestErrorPrefixForVerdict(t *testing.T) {
	const summary = "test summary"
	cases := []struct {
		verdict Verdict
		want    string
	}{
		{VerdictAppliedPassed, "[verdict:applied_passed] test summary"},
		{VerdictUserRejected, "[verdict:user_rejected] test summary"},
		{VerdictAppliedTestFailedReverted, "[verdict:applied_test_failed_reverted] test summary"},
		{VerdictProposalUnparseable, "[verdict:proposal_unparseable] test summary"},
		{VerdictOutOfScopeDiff, "[verdict:out_of_scope_diff] test summary"},
		{VerdictGitApplyFailed, "[verdict:git_apply_failed] test summary"},
		{VerdictPreconditionFailed, "[verdict:precondition_failed] test summary"},
		{VerdictContextLoadFailed, "[verdict:context_load_failed] test summary"},
		{VerdictRevertFailed, "[verdict:revert_failed] test summary"},
		{VerdictRegenerateLoopAborted, "[verdict:regenerate_loop_aborted] test summary"},
	}
	for _, tc := range cases {
		if got := ErrorPrefixForVerdict(tc.verdict, summary); got != tc.want {
			t.Errorf("ErrorPrefixForVerdict(%q, %q): got %q, want %q",
				string(tc.verdict), summary, got, tc.want)
		}
	}
	if got, want := len(cases), 10; got != want {
		t.Fatalf("expected coverage of all %d verdicts, got %d", want, got)
	}
}
