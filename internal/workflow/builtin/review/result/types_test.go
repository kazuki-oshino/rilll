package result

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestRuleResult_RoundTrip は RuleResult が JSON へ Marshal / Unmarshal で
// ラウンドトリップ可能であることを検証する (design.md §result.types Logical Schema)。
//
// 後方互換契約: 新フィールド追加可能 / 既存フィールドの意味変更不可 / strict decode しない。
// 検証対象は per-rule.json artifact の両端契約 (review-apply-workflow が消費する schema)。
func TestRuleResult_RoundTrip(t *testing.T) {
	t.Parallel()

	original := RuleResult{
		Name:            "security-scan",
		Description:     "セキュリティ観点のレビュー",
		Applied:         true,
		Verdict:         VerdictFindingsReported,
		FindingsPresent: true,
		Summary:         "認証経路に脆弱性候補が 1 件",
		Findings: []Finding{
			{Severity: SeverityError, File: "auth/login.go", Location: "auth/login.go:42", Message: "SQL インジェクション疑い"},
			{Severity: SeverityWarn, File: "auth/audit.go", Message: "ログ出力にトークンが含まれている"},
		},
		Provider:        "claude",
		Model:           "claude-sonnet-4-5",
		FailureCategory: "",
		StartedAt:       "2026-05-04T12:00:00.000000Z",
		FinishedAt:      "2026-05-04T12:00:42.123456Z",
		SourceFile:      "security-scan.yaml",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// JSON tag が design schema と一致することを部分検証 (主要キー)。
	for _, want := range []string{
		`"name":"security-scan"`,
		`"description":"セキュリティ観点のレビュー"`,
		`"applied":true`,
		`"verdict":"findings_reported"`,
		`"findings_present":true`,
		`"provider":"claude"`,
		`"model":"claude-sonnet-4-5"`,
		`"started_at":"2026-05-04T12:00:00.000000Z"`,
		`"finished_at":"2026-05-04T12:00:42.123456Z"`,
		`"source_file":"security-scan.yaml"`,
		`"severity":"error"`,
		`"file":"auth/login.go"`,
		`"location":"auth/login.go:42"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("Marshal output missing %q\nfull: %s", want, string(data))
		}
	}

	var decoded RuleResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(original, decoded) {
		t.Errorf("round-trip mismatch\nwant: %#v\n got: %#v", original, decoded)
	}
}

// TestRuleResult_OmitemptyFields は omitempty 指定フィールドが zero-value のとき
// JSON 出力から省略されることを検証する (design.md schema と一致)。
func TestRuleResult_OmitemptyFields(t *testing.T) {
	t.Parallel()

	r := RuleResult{
		Name:        "docs-lint",
		Description: "docs lint",
		Applied:     false,
		Verdict:     VerdictSkipped,
		// SkipReason / Findings / FailureCategory は zero-value
		Provider:   "cursor",
		Model:      "",
		StartedAt:  "2026-05-04T12:00:00Z",
		FinishedAt: "2026-05-04T12:00:01Z",
		SourceFile: "docs-lint.yaml",
	}

	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(data)

	for _, omitted := range []string{
		`"skip_reason"`,
		`"findings"`,
		`"failure_category"`,
	} {
		if strings.Contains(s, omitted) {
			t.Errorf("expected %s to be omitted from JSON, got: %s", omitted, s)
		}
	}

	// applied / findings_present / model のような non-omitempty フィールドは zero-value でも出力される。
	for _, required := range []string{
		`"applied":false`,
		`"findings_present":false`,
		`"model":""`,
	} {
		if !strings.Contains(s, required) {
			t.Errorf("expected %s to be present in JSON, got: %s", required, s)
		}
	}
}

// TestFinding_OmitemptyLocation は Finding.Location が空のとき omitempty で省略されることを検証する。
// File は両端契約上必須 (omitempty 無し) のため、本テストでは File を明示してから Location の挙動を検証する。
func TestFinding_OmitemptyLocation(t *testing.T) {
	t.Parallel()

	f := Finding{Severity: SeverityInfo, File: "main.go", Message: "memo"}
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), `"location"`) {
		t.Errorf("expected location to be omitted, got: %s", string(data))
	}

	// 逆に Location 指定時は出力に含まれる。
	f.Location = "main.go:1"
	data, err = json.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"location":"main.go:1"`) {
		t.Errorf("expected location to be present, got: %s", string(data))
	}
}

// TestFinding_FileRequired は File フィールドが両端契約上必須 (omitempty 無し) で
// 空文字でも JSON 出力に現れることを検証する (review-apply-workflow design.md §Patch
// fail-closed 前提と整合)。
func TestFinding_FileRequired(t *testing.T) {
	t.Parallel()

	// File 未指定 (空文字) の場合でも JSON にキーが現れる契約。
	// 上流 parser (review/parse.go validateAndConvert) が空文字を failed に集約する責務。
	f := Finding{Severity: SeverityInfo, Message: "memo"}
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"file":""`) {
		t.Errorf("expected file key with empty string to be present, got: %s", string(data))
	}

	// File 指定時は値が反映される。
	f.File = "auth/login.go"
	data, err = json.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"file":"auth/login.go"`) {
		t.Errorf("expected file value to be present, got: %s", string(data))
	}
}

// TestSummary_RoundTrip は Summary 全体の round-trip を検証する。
func TestSummary_RoundTrip(t *testing.T) {
	t.Parallel()

	original := Summary{
		WorkflowRunID:  "run-2026-05-04-001",
		PRNumber:       42,
		OverallVerdict: VerdictFindingsReported,
		OverallSummary: "3 rule 中 1 rule で指摘あり",
		Rules: []RuleResult{
			{
				Name:            "security-scan",
				Description:     "sec",
				Applied:         true,
				Verdict:         VerdictFindingsReported,
				FindingsPresent: true,
				Summary:         "1 finding",
				Provider:        "claude",
				Model:           "claude-sonnet-4-5",
				StartedAt:       "2026-05-04T12:00:00Z",
				FinishedAt:      "2026-05-04T12:00:30Z",
				SourceFile:      "security-scan.yaml",
			},
			{
				Name:        "docs-lint",
				Description: "docs",
				Applied:     false,
				Verdict:     VerdictSkipped,
				SkipReason:  "PR には docs 変更が含まれていない",
				Summary:     "skipped",
				Provider:    "cursor",
				StartedAt:   "2026-05-04T12:00:00Z",
				FinishedAt:  "2026-05-04T12:00:02Z",
				SourceFile:  "docs-lint.yaml",
			},
		},
		PrecheckWarnings: []string{"rule-x.yaml: unknown field `foo`"},
		StartedAt:        "2026-05-04T12:00:00Z",
		FinishedAt:       "2026-05-04T12:00:42Z",
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	for _, want := range []string{
		`"workflow_run_id":"run-2026-05-04-001"`,
		`"pr_number":42`,
		`"overall_verdict":"findings_reported"`,
		`"overall_summary":"3 rule 中 1 rule で指摘あり"`,
		`"precheck_warnings":["rule-x.yaml: unknown field ` + "`foo`" + `"]`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("Marshal output missing %q\nfull: %s", want, string(data))
		}
	}

	var decoded Summary
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(original, decoded) {
		t.Errorf("round-trip mismatch\nwant: %#v\n got: %#v", original, decoded)
	}
}

// TestSummary_PrecheckWarningsOmitempty は PrecheckWarnings が nil/空のとき
// JSON 出力から省略されることを検証する。
func TestSummary_PrecheckWarningsOmitempty(t *testing.T) {
	t.Parallel()

	s := Summary{
		WorkflowRunID:  "run-1",
		PRNumber:       1,
		OverallVerdict: VerdictNoFindings,
		OverallSummary: "all clear",
		Rules:          []RuleResult{},
		StartedAt:      "2026-05-04T12:00:00Z",
		FinishedAt:     "2026-05-04T12:00:01Z",
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), `"precheck_warnings"`) {
		t.Errorf("expected precheck_warnings to be omitted, got: %s", string(data))
	}
}

// TestUnknownField_NotStrictlyRejected は schema に存在しない unknown フィールドが
// 旧 reader (review-apply-workflow) でも decode 失敗しないことを検証する
// (design.md Implementation Notes / 後方互換契約)。
//
// encoding/json デフォルトの非 strict decode を採用していることの保証テスト。
func TestUnknownField_NotStrictlyRejected(t *testing.T) {
	t.Parallel()

	// 将来 review-workflow が追加するかもしれない unknown フィールドを含む JSON。
	raw := []byte(`{
		"name": "future-rule",
		"description": "",
		"applied": true,
		"verdict": "no_findings",
		"findings_present": false,
		"summary": "",
		"provider": "claude",
		"model": "",
		"started_at": "2026-05-04T12:00:00Z",
		"finished_at": "2026-05-04T12:00:01Z",
		"source_file": "future-rule.yaml",
		"future_field_not_in_schema": "ignored"
	}`)

	var rr RuleResult
	if err := json.Unmarshal(raw, &rr); err != nil {
		t.Fatalf("expected unknown field to be silently ignored, got error: %v", err)
	}
	if rr.Name != "future-rule" || rr.Verdict != VerdictNoFindings {
		t.Errorf("unexpected decode result: %#v", rr)
	}
}

// TestOverallVerdict_Priority は OverallVerdict ヘルパが
// precheck_failed > cancelled > findings_reported > failed > no_findings の
// 優先順位を厳密に守ることを table-driven で検証する (design.md §Logical Schema 末尾)。
func TestOverallVerdict_Priority(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []Verdict
		want Verdict
	}{
		{
			name: "empty slice falls back to no_findings",
			in:   nil,
			want: VerdictNoFindings,
		},
		{
			name: "all unset falls back to no_findings",
			in:   []Verdict{VerdictUnset, VerdictUnset},
			want: VerdictNoFindings,
		},
		{
			name: "all skipped falls back to no_findings",
			in:   []Verdict{VerdictSkipped, VerdictSkipped},
			want: VerdictNoFindings,
		},
		{
			name: "single no_findings stays no_findings",
			in:   []Verdict{VerdictNoFindings},
			want: VerdictNoFindings,
		},
		{
			name: "skipped + no_findings still no_findings",
			in:   []Verdict{VerdictSkipped, VerdictNoFindings, VerdictSkipped},
			want: VerdictNoFindings,
		},
		{
			name: "failed alone wins over no_findings",
			in:   []Verdict{VerdictNoFindings, VerdictFailed, VerdictSkipped},
			want: VerdictFailed,
		},
		{
			name: "findings_reported wins over failed",
			in:   []Verdict{VerdictFailed, VerdictFindingsReported, VerdictNoFindings},
			want: VerdictFindingsReported,
		},
		{
			name: "cancelled wins over findings_reported",
			in:   []Verdict{VerdictFindingsReported, VerdictCancelled, VerdictFailed},
			want: VerdictCancelled,
		},
		{
			name: "precheck_failed wins over cancelled",
			in:   []Verdict{VerdictCancelled, VerdictPrecheckFailed},
			want: VerdictPrecheckFailed,
		},
		{
			name: "precheck_failed wins over everything else",
			in: []Verdict{
				VerdictNoFindings,
				VerdictFindingsReported,
				VerdictFailed,
				VerdictCancelled,
				VerdictPrecheckFailed,
				VerdictSkipped,
			},
			want: VerdictPrecheckFailed,
		},
		{
			name: "single failed without findings",
			in:   []Verdict{VerdictFailed},
			want: VerdictFailed,
		},
		{
			name: "findings_reported wins over no_findings",
			in:   []Verdict{VerdictNoFindings, VerdictFindingsReported},
			want: VerdictFindingsReported,
		},
		{
			name: "single cancelled",
			in:   []Verdict{VerdictCancelled},
			want: VerdictCancelled,
		},
		{
			name: "single precheck_failed",
			in:   []Verdict{VerdictPrecheckFailed},
			want: VerdictPrecheckFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := OverallVerdict(tc.in)
			if got != tc.want {
				t.Errorf("OverallVerdict(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestVerdict_StringValues は Verdict 定数のリテラル値が design schema と一致することを検証する。
// この値は per-rule.json / result.json の JSON 表現として両端契約となるため、
// ここで誤って rename / 値変更すると review-apply-workflow が壊れる。
func TestVerdict_StringValues(t *testing.T) {
	t.Parallel()

	cases := map[Verdict]string{
		VerdictUnset:            "",
		VerdictPrecheckFailed:   "precheck_failed",
		VerdictSkipped:          "skipped",
		VerdictNoFindings:       "no_findings",
		VerdictFindingsReported: "findings_reported",
		VerdictFailed:           "failed",
		VerdictCancelled:        "cancelled",
	}
	for v, want := range cases {
		if string(v) != want {
			t.Errorf("Verdict const mismatch: got %q, want %q", string(v), want)
		}
	}
}

// TestSeverity_StringValues は Severity 定数の値が design schema と一致することを検証する。
func TestSeverity_StringValues(t *testing.T) {
	t.Parallel()

	cases := map[Severity]string{
		SeverityInfo:  "info",
		SeverityWarn:  "warn",
		SeverityError: "error",
	}
	for s, want := range cases {
		if string(s) != want {
			t.Errorf("Severity const mismatch: got %q, want %q", string(s), want)
		}
	}
}
