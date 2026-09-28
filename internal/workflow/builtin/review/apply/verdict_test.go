// Package apply のテスト本ファイルは Verdict 列挙 (closed set 10 値 + VerdictUnset) と
// IsIntent / ErrorPrefixForVerdict helper の契約を検証する
// (tasks.md 1.1 / design.md §Verdict Catalog / Req 6.3, 7.4)。
//
// 検証観点:
//   - 列挙文字列値の不変性 (akashic 表示契約 / result.json schema と整合)
//   - IsIntent: applied / no_findings / user_aborted の 3 値で true、残り 7 値で false
//   - ErrorPrefixForVerdict: `[verdict:<id>] <summary>` 形 (improve-workflow 既存規約と整合)
package apply

import "testing"

// TestVerdictStringValues は 10 値 + VerdictUnset の Verdict 文字列定数が
// design.md §Verdict Catalog で規定された不変文字列と一致することを検証する。
// この値は result.json schema / workflow_runs.error プレフィックスで参照されるため、
// 誤って変更されないことを機械検証する。
func TestVerdictStringValues(t *testing.T) {
	cases := []struct {
		verdict Verdict
		want    string
	}{
		{VerdictUnset, ""},
		{VerdictApplied, "applied"},
		{VerdictNoFindings, "no_findings"},
		{VerdictUserAborted, "user_aborted"},
		{VerdictCycleExhausted, "cycle_exhausted"},
		{VerdictGitApplyFailed, "git_apply_failed"},
		{VerdictRevertFailed, "revert_failed"},
		{VerdictProposalUnparseable, "proposal_unparseable"},
		{VerdictOutOfScopeDiff, "out_of_scope_diff"},
		{VerdictPreconditionFailed, "precondition_failed"},
		{VerdictFailed, "failed"},
	}
	for _, tc := range cases {
		if string(tc.verdict) != tc.want {
			t.Errorf("verdict string mismatch: got %q, want %q", string(tc.verdict), tc.want)
		}
	}
	// VerdictUnset を除いた closed set が 10 値であることを契約として固定する。
	if got, want := len(cases)-1, 10; got != want {
		t.Fatalf("expected closed set of %d verdicts (excluding VerdictUnset), got %d", want, got)
	}
}

// TestVerdictIsIntent は IsIntent() が「ユーザ意思 / 正常成功」verdict (3 値) で true、
// パイプライン / システム失敗 verdict (7 値) で false を返すことを検証する。
// IsIntent==true のとき orchestration 側は Fn から nil を返す契約 (design.md §Verdict / Req 6.3)。
//
// VerdictUnset は orchestration 終端で書き出されない契約のため、IsIntent としては false を返す
// (intent 経路に乗せると result.json invariant を破るため)。
func TestVerdictIsIntent(t *testing.T) {
	cases := []struct {
		verdict Verdict
		want    bool
	}{
		{VerdictApplied, true},
		{VerdictNoFindings, true},
		{VerdictUserAborted, true},
		{VerdictCycleExhausted, false},
		{VerdictGitApplyFailed, false},
		{VerdictRevertFailed, false},
		{VerdictProposalUnparseable, false},
		{VerdictOutOfScopeDiff, false},
		{VerdictPreconditionFailed, false},
		{VerdictFailed, false},
	}
	for _, tc := range cases {
		if got := tc.verdict.IsIntent(); got != tc.want {
			t.Errorf("IsIntent(%q): got %v, want %v", string(tc.verdict), got, tc.want)
		}
	}
	if got, want := len(cases), 10; got != want {
		t.Fatalf("expected coverage of all %d verdicts, got %d", want, got)
	}

	// VerdictUnset の防御確認: 未設定 verdict が intent 経路に紛れ込まないこと。
	if VerdictUnset.IsIntent() {
		t.Errorf("VerdictUnset.IsIntent() must be false to preserve result.json invariant")
	}
}

// TestErrorPrefixForVerdict は ErrorPrefixForVerdict が `[verdict:<id>] <summary>` 形を
// 返すことを 10 値全てに対して検証する。improve-workflow 既存規約 (`[verdict:applied_passed] ...`)
// と整合する形式を保つ。
func TestErrorPrefixForVerdict(t *testing.T) {
	const summary = "test summary"
	cases := []struct {
		verdict Verdict
		want    string
	}{
		{VerdictApplied, "[verdict:applied] test summary"},
		{VerdictNoFindings, "[verdict:no_findings] test summary"},
		{VerdictUserAborted, "[verdict:user_aborted] test summary"},
		{VerdictCycleExhausted, "[verdict:cycle_exhausted] test summary"},
		{VerdictGitApplyFailed, "[verdict:git_apply_failed] test summary"},
		{VerdictRevertFailed, "[verdict:revert_failed] test summary"},
		{VerdictProposalUnparseable, "[verdict:proposal_unparseable] test summary"},
		{VerdictOutOfScopeDiff, "[verdict:out_of_scope_diff] test summary"},
		{VerdictPreconditionFailed, "[verdict:precondition_failed] test summary"},
		{VerdictFailed, "[verdict:failed] test summary"},
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
