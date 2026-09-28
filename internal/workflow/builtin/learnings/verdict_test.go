package learnings

import (
	"reflect"
	"testing"
)

// TestIsValidVerdict_AcceptsClosedSet は 9 値 closed set がすべて true で受け入れられることを確認する。
// Requirement 8.5 (verdict closed set 9 値) のカバレッジ。
func TestIsValidVerdict_AcceptsClosedSet(t *testing.T) {
	t.Parallel()

	cases := []string{
		VerdictApplied,
		VerdictAppliedNoOp,
		VerdictRejectedInvalidJSON,
		VerdictRejectedEmptyPathsGlob,
		VerdictRejectedUnboundedPathsGlob,
		VerdictRejectedInvalidCategory,
		VerdictRejectedInvalidTopicName,
		VerdictReaderUnavailable,
		VerdictResumeAllFailed,
	}

	if got := len(cases); got != 9 {
		t.Fatalf("verdict closed set size = %d, want 9 (requirement 8.5)", got)
	}

	for _, v := range cases {
		t.Run(v, func(t *testing.T) {
			t.Parallel()
			if !IsValidVerdict(v) {
				t.Fatalf("IsValidVerdict(%q) = false, want true", v)
			}
		})
	}
}

// TestIsValidVerdict_ConstantValues は 9 値 closed set の定数値が requirements.md / design.md に記載された
// リテラルと完全一致することを確認する。値が変わると `<runDir>/apply-result.json` の verdict が
// 既存 downstream (CLI / TUI / 後続 spec) と乖離するため、値そのものを不変式として記録する。
// Requirement 8.5 のカバレッジ。
func TestIsValidVerdict_ConstantValues(t *testing.T) {
	t.Parallel()

	wants := map[string]string{
		"VerdictApplied":                    "applied",
		"VerdictAppliedNoOp":                "applied-no-op",
		"VerdictRejectedInvalidJSON":        "rejected-invalid-json",
		"VerdictRejectedEmptyPathsGlob":     "rejected-empty-paths-glob",
		"VerdictRejectedUnboundedPathsGlob": "rejected-unbounded-paths-glob",
		"VerdictRejectedInvalidCategory":    "rejected-invalid-category",
		"VerdictRejectedInvalidTopicName":   "rejected-invalid-topic-name",
		"VerdictReaderUnavailable":          "reader-unavailable",
		"VerdictResumeAllFailed":            "resume-all-failed",
	}
	gots := map[string]string{
		"VerdictApplied":                    VerdictApplied,
		"VerdictAppliedNoOp":                VerdictAppliedNoOp,
		"VerdictRejectedInvalidJSON":        VerdictRejectedInvalidJSON,
		"VerdictRejectedEmptyPathsGlob":     VerdictRejectedEmptyPathsGlob,
		"VerdictRejectedUnboundedPathsGlob": VerdictRejectedUnboundedPathsGlob,
		"VerdictRejectedInvalidCategory":    VerdictRejectedInvalidCategory,
		"VerdictRejectedInvalidTopicName":   VerdictRejectedInvalidTopicName,
		"VerdictReaderUnavailable":          VerdictReaderUnavailable,
		"VerdictResumeAllFailed":            VerdictResumeAllFailed,
	}

	for name, want := range wants {
		if gots[name] != want {
			t.Fatalf("%s = %q, want %q", name, gots[name], want)
		}
	}
}

// TestIsValidVerdict_RejectsOutOfSet は closed set 外の値がすべて false で reject されることを確認する。
// Requirement 8.5 (verdict closed set 9 値以外を reject) のカバレッジ。
//
// 想定攻撃面: 空文字 / 大文字小文字違い / hyphen 違い / v1 distiller 由来の旧 verdict (`prefetch-failed`
// 等) / underscore 違い / 前後空白 / Japanese / 部分文字列。すべて false を期待する。
func TestIsValidVerdict_RejectsOutOfSet(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
	}{
		{name: "empty", input: ""},
		{name: "unknown lowercase", input: "ok"},
		{name: "uppercase variant", input: "Applied"},
		{name: "all uppercase", input: "APPLIED"},
		{name: "underscore variant", input: "applied_no_op"},
		{name: "trailing space", input: "applied "},
		{name: "leading space", input: " applied"},
		{name: "japanese", input: "適用済み"},
		{name: "v1 distiller verdict prefetch-failed", input: "prefetch-failed"},
		{name: "v1 distiller verdict reflector-call-failed", input: "reflector-call-failed"},
		{name: "v1 distiller verdict skipped-reflector-parse", input: "skipped-reflector-parse"},
		{name: "v1 distiller verdict curator-call-failed", input: "curator-call-failed"},
		{name: "v1 distiller verdict skipped-curator-parse", input: "skipped-curator-parse"},
		{name: "near miss invalid-json", input: "invalid-json"},
		{name: "near miss reject-invalid-json", input: "reject-invalid-json"}, // closed set は rejected-invalid-json
		{name: "near miss empty-paths", input: "rejected-empty-paths"},        // 末尾 -glob 抜け
		{name: "near miss unbounded-paths", input: "rejected-unbounded-paths"},
		{name: "near miss reader_unavailable", input: "reader_unavailable"},
		{name: "near miss resume-failed", input: "resume-failed"},
		{name: "substring of applied", input: "appl"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if IsValidVerdict(tc.input) {
				t.Fatalf("IsValidVerdict(%q) = true, want false", tc.input)
			}
		})
	}
}

// TestApplyResult_FieldShape は ApplyResult struct が design.md §Apply §Service Interface で
// 定義された 7 field (Verdict / Appended / Rejected / Deduped / NewTopicsCreated / StartedAt
// / FinishedAt) を持つことを確認する。task 7.4 (learnings.Apply) が ApplyResult を artifact
// (`apply-result.json`) に書き出すため、field 名 / 型は契約として固定する。
//
// 本 task 6.2 の boundary は learnings.Verdict で、Appended / Rejected / Deduped の要素型
// (AppendedEntry / RejectedOperation / DedupSkippedOperation) は task 7.4 で field を追加する
// 余地を残すため、本テストは「struct が存在し各 field 名と top-level 型が一致する」までを保証する。
// Requirement 8.5 のカバレッジ。
func TestApplyResult_FieldShape(t *testing.T) {
	t.Parallel()

	rt := reflect.TypeOf(ApplyResult{})
	if rt.Kind() != reflect.Struct {
		t.Fatalf("ApplyResult is not a struct (kind=%v)", rt.Kind())
	}

	type wantField struct {
		name     string
		typeName string
	}
	wants := []wantField{
		{name: "Verdict", typeName: "string"},
		{name: "Appended", typeName: "[]learnings.AppendedEntry"},
		{name: "Rejected", typeName: "[]learnings.RejectedOperation"},
		{name: "Deduped", typeName: "[]learnings.DedupSkippedOperation"},
		{name: "NewTopicsCreated", typeName: "[]string"},
		{name: "StartedAt", typeName: "string"},
		{name: "FinishedAt", typeName: "string"},
		{name: "IndexWarnings", typeName: "[]learnings.IndexWarning"},
	}

	if got := rt.NumField(); got != len(wants) {
		t.Fatalf("ApplyResult NumField = %d, want %d (requirement 8.5 + IndexWarnings)", got, len(wants))
	}

	for i, want := range wants {
		f := rt.Field(i)
		if f.Name != want.name {
			t.Errorf("ApplyResult field[%d].Name = %q, want %q", i, f.Name, want.name)
		}
		if got := f.Type.String(); got != want.typeName {
			t.Errorf("ApplyResult field[%d] (%s) Type = %q, want %q", i, want.name, got, want.typeName)
		}
	}
}

// TestApplyResult_VerdictDefaultZero は ApplyResult のゼロ値で Verdict が空文字であることを確認する。
// 実装側は必ず IsValidVerdict を満たす値を代入する責務があり、未代入のまま artifact 化されないよう
// 「ゼロ値が closed set に含まれない」点を不変式として固定する。
func TestApplyResult_VerdictDefaultZero(t *testing.T) {
	t.Parallel()

	var r ApplyResult
	if r.Verdict != "" {
		t.Fatalf("ApplyResult{}.Verdict = %q, want empty string", r.Verdict)
	}
	if IsValidVerdict(r.Verdict) {
		t.Fatalf("IsValidVerdict(\"\") = true, want false (zero value must not be a valid verdict)")
	}
}
