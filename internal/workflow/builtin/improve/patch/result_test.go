// 本ファイルは tasks.md 9.1 の result.json schema 型 (ResultRecord / AttemptRecord /
// TestResultRecord / BaselineRefRecord / DiffSummaryRecord) と DiffSummaryRecord プレビュー
// 生成 helper の契約を機械検証するテスト群。
//
// 検証観点 (Req 5.9, 6.2, 6.3, 6.4 / design.md §Data Contracts & Integration §7):
//  1. JSON marshal → unmarshal round-trip で全フィールド等価。
//  2. apply 未到達 verdict (precondition_failed / context_load_failed 等) で
//     test_result / baseline_ref / diff_summary が nil の場合、JSON 出力に
//     当該キーが**現れない** (omitempty の機械検証)。
//  3. DiffSummaryRecord プレビュー生成は先頭 50 行 / 4 KiB cap (Q3 / Resolved Decisions §3)。
//  4. AddedLines / DeletedLines は unified diff 本文 `+` / `-` 行のみをカウントし、
//     ヘッダ (`+++` / `---`) や hunk マーカ (`@@`) はカウント対象外。
package patch

import (
	"encoding/json"
	"strings"
	"testing"
)

// completeResultRecord はすべてのオプションフィールドを埋めた状態の ResultRecord を返す。
// JSON round-trip テストの起点として再利用する。
func completeResultRecord() ResultRecord {
	return ResultRecord{
		WorkflowRunID:    "run-123",
		TargetWorkflowID: "git-commit",
		TargetGoFile:     "/abs/path/internal/workflow/git_commit.go",
		RunIDs:           []string{"origin-run-1"},
		Verdict:          VerdictAppliedPassed,
		Summary:          "applied patch and tests passed",
		Attempts: []AttemptRecord{
			{Attempt: 1, Provider: "claude", PatchArtifact: "proposal-1.diff", Outcome: "ok"},
		},
		TestResult: &TestResultRecord{
			Executed:   true,
			ExitCode:   0,
			DurationMS: 12345,
			StdoutTail: "PASS\nok  github.com/.../workflow",
			StderrTail: "",
			Truncated:  false,
		},
		BaselineRef: &BaselineRefRecord{
			HeadCommit:  "abc123def4567890abc123def4567890abc12345",
			TargetFiles: []string{"/abs/path/internal/workflow/git_commit.go"},
		},
		DiffSummary: &DiffSummaryRecord{
			TargetFile:       "/abs/path/internal/workflow/git_commit.go",
			AddedLines:       12,
			DeletedLines:     3,
			PreviewHead:      "diff --git a/internal/workflow/git_commit.go b/internal/workflow/git_commit.go\n@@ -1,3 +1,5 @@\n",
			FullDiffArtifact: "proposal-1.diff",
		},
		StartedAt:  "2026-05-03T10:00:00.000000000Z",
		FinishedAt: "2026-05-03T10:01:00.000000000Z",
	}
}

// TestResultRecord_JSONRoundTrip は完全フィールドを埋めた ResultRecord が
// marshal → unmarshal を経て元の値と等価であることを検証する (Req 6.2, 6.3, 6.4)。
func TestResultRecord_JSONRoundTrip(t *testing.T) {
	original := completeResultRecord()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}

	var decoded ResultRecord
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v", err)
	}

	// reflect.DeepEqual に頼らずフィールド単位で diff を見る方が失敗時の
	// 原因切り分けが速いが、本テストは契約の総合一致確認なので encoding/json の
	// 再 marshal で文字列比較する (フィールド順序変動を吸収)。
	reEncoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("json.Marshal of decoded returned error: %v", err)
	}
	if string(reEncoded) != string(encoded) {
		t.Errorf("round-trip mismatch:\noriginal=%s\nreencoded=%s", string(encoded), string(reEncoded))
	}
}

// TestResultRecord_OmitemptyOnPreApplyVerdicts は apply 未到達 verdict
// (precondition_failed / context_load_failed) で TestResult / BaselineRef /
// DiffSummary を nil にした際、JSON 出力に当該 3 キーが現れないことを検証する
// (Req 6.5 / design.md §7 omitempty 規定)。
func TestResultRecord_OmitemptyOnPreApplyVerdicts(t *testing.T) {
	cases := []struct {
		name    string
		verdict Verdict
	}{
		{name: "precondition_failed", verdict: VerdictPreconditionFailed},
		{name: "context_load_failed", verdict: VerdictContextLoadFailed},
	}
	// 確認対象キー (これらが**現れない**ことを assert する)。
	omittedKeys := []string{`"test_result"`, `"baseline_ref"`, `"diff_summary"`}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := ResultRecord{
				WorkflowRunID:    "run-precheck-fail",
				TargetWorkflowID: "git-commit",
				TargetGoFile:     "/abs/path/internal/workflow/git_commit.go",
				RunIDs:           []string{"origin-run-1"},
				Verdict:          c.verdict,
				Summary:          "precondition failed",
				Attempts:         []AttemptRecord{}, // apply 未到達でも空 slice として記録
				// TestResult / BaselineRef / DiffSummary は nil のまま (apply 未到達)。
				StartedAt:  "2026-05-03T10:00:00Z",
				FinishedAt: "2026-05-03T10:00:00Z",
			}

			encoded, err := json.Marshal(rec)
			if err != nil {
				t.Fatalf("json.Marshal returned error: %v", err)
			}
			out := string(encoded)

			for _, key := range omittedKeys {
				if strings.Contains(out, key) {
					t.Errorf("omitempty 違反: %q が JSON 出力に含まれている (verdict=%s, output=%s)",
						key, c.verdict, out)
				}
			}

			// verdict 値そのものは必ず出る。
			if !strings.Contains(out, string(c.verdict)) {
				t.Errorf("verdict 値 %q が JSON 出力に含まれていない: %s", c.verdict, out)
			}
		})
	}
}

// TestResultRecord_RequiredKeysAlwaysPresent は apply 未到達 verdict でも
// 「常に出るべきキー」(workflow_run_id / verdict / summary / attempts / started_at /
// finished_at) が JSON 出力に必ず含まれることを検証する (Req 6.5 記録欠落禁止)。
func TestResultRecord_RequiredKeysAlwaysPresent(t *testing.T) {
	rec := ResultRecord{
		WorkflowRunID:    "run-precheck-fail",
		TargetWorkflowID: "git-commit",
		TargetGoFile:     "/abs/path/internal/workflow/git_commit.go",
		RunIDs:           []string{"origin-run-1"},
		Verdict:          VerdictPreconditionFailed,
		Summary:          "precondition failed",
		Attempts:         []AttemptRecord{},
		StartedAt:        "2026-05-03T10:00:00Z",
		FinishedAt:       "2026-05-03T10:00:00Z",
	}
	encoded, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	out := string(encoded)
	requiredKeys := []string{
		`"workflow_run_id"`,
		`"target_workflow_id"`,
		`"target_go_file"`,
		`"run_ids"`,
		`"verdict"`,
		`"summary"`,
		`"attempts"`,
		`"started_at"`,
		`"finished_at"`,
	}
	for _, key := range requiredKeys {
		if !strings.Contains(out, key) {
			t.Errorf("必須キー %q が JSON 出力に含まれていない: %s", key, out)
		}
	}
}

// TestAttemptRecord_JSONRoundTrip は AttemptRecord 単体の round-trip 検証 (Req 3.6 / 6.4)。
func TestAttemptRecord_JSONRoundTrip(t *testing.T) {
	original := AttemptRecord{
		Attempt:       3,
		Provider:      "codex",
		PatchArtifact: "proposal-3.diff",
		Outcome:       "out_of_scope_diff",
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	// 期待 JSON キー (snake_case) が含まれる。
	for _, key := range []string{`"attempt"`, `"provider"`, `"patch_artifact"`, `"outcome"`} {
		if !strings.Contains(string(encoded), key) {
			t.Errorf("AttemptRecord JSON に key %q が含まれない: %s", key, encoded)
		}
	}

	var decoded AttemptRecord
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip mismatch: got=%#v want=%#v", decoded, original)
	}
}

// TestTestResultRecord_JSONRoundTrip は TestResultRecord 単体の round-trip 検証 (Req 5.5〜5.7)。
func TestTestResultRecord_JSONRoundTrip(t *testing.T) {
	original := TestResultRecord{
		Executed:   true,
		ExitCode:   1,
		DurationMS: 5432,
		StdoutTail: "FAIL\n--- FAIL: TestFoo (0.01s)",
		StderrTail: "ginkgo: timed out",
		Truncated:  true,
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	for _, key := range []string{
		`"executed"`, `"exit_code"`, `"duration_ms"`, `"stdout_tail"`, `"stderr_tail"`, `"truncated"`,
	} {
		if !strings.Contains(string(encoded), key) {
			t.Errorf("TestResultRecord JSON に key %q が含まれない: %s", key, encoded)
		}
	}
	var decoded TestResultRecord
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip mismatch: got=%#v want=%#v", decoded, original)
	}
}

// TestBaselineRefRecord_JSONRoundTrip は BaselineRefRecord 単体の round-trip 検証
// (Req 5.9 / Resolved Decisions §7)。
func TestBaselineRefRecord_JSONRoundTrip(t *testing.T) {
	original := BaselineRefRecord{
		HeadCommit:  "abc123def4567890abc123def4567890abc12345",
		TargetFiles: []string{"/abs/a.go", "/abs/b.go"},
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	for _, key := range []string{`"head_commit"`, `"target_files"`} {
		if !strings.Contains(string(encoded), key) {
			t.Errorf("BaselineRefRecord JSON に key %q が含まれない: %s", key, encoded)
		}
	}
	var decoded BaselineRefRecord
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v", err)
	}
	// slice の比較のため deep-equal 相当 (再 marshal で文字列比較)。
	reEnc, _ := json.Marshal(decoded)
	if string(reEnc) != string(encoded) {
		t.Errorf("round-trip mismatch: got=%s want=%s", reEnc, encoded)
	}
}

// TestDiffSummaryRecord_JSONRoundTrip は DiffSummaryRecord 単体の round-trip 検証
// (Q3 / Resolved Decisions §3 / Req 6.3)。
func TestDiffSummaryRecord_JSONRoundTrip(t *testing.T) {
	original := DiffSummaryRecord{
		TargetFile:       "/abs/internal/workflow/git_commit.go",
		AddedLines:       12,
		DeletedLines:     3,
		PreviewHead:      "diff --git a/foo b/foo\n",
		FullDiffArtifact: "proposal-1.diff",
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	for _, key := range []string{
		`"target_file"`, `"added_lines"`, `"deleted_lines"`, `"preview_head"`, `"full_diff_artifact"`,
	} {
		if !strings.Contains(string(encoded), key) {
			t.Errorf("DiffSummaryRecord JSON に key %q が含まれない: %s", key, encoded)
		}
	}
	var decoded DiffSummaryRecord
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip mismatch: got=%#v want=%#v", decoded, original)
	}
}

// TestBuildDiffSummary_AddedDeletedLineCount は unified diff から
// AddedLines / DeletedLines が正しくカウントされることを検証する。
//
// 仕様:
//   - `+` で始まる行のうち、`+++` (file ヘッダ) は除外して `AddedLines` にカウント。
//   - `-` で始まる行のうち、`---` (file ヘッダ) は除外して `DeletedLines` にカウント。
//   - hunk マーカ (`@@`) や `diff --git` 行はカウント対象外。
func TestBuildDiffSummary_AddedDeletedLineCount(t *testing.T) {
	diff := []byte(`diff --git a/foo.go b/foo.go
--- a/foo.go
+++ b/foo.go
@@ -1,5 +1,7 @@
 line1
-line2_old
+line2_new1
+line2_new2
 line3
-line4_old
+line4_new
 line5
`)

	got := BuildDiffSummary("/abs/foo.go", diff, "proposal-1.diff")
	if got == nil {
		t.Fatal("BuildDiffSummary returned nil")
	}
	if got.AddedLines != 3 {
		t.Errorf("AddedLines: got=%d want=3", got.AddedLines)
	}
	if got.DeletedLines != 2 {
		t.Errorf("DeletedLines: got=%d want=2", got.DeletedLines)
	}
	if got.TargetFile != "/abs/foo.go" {
		t.Errorf("TargetFile: got=%q want=%q", got.TargetFile, "/abs/foo.go")
	}
	if got.FullDiffArtifact != "proposal-1.diff" {
		t.Errorf("FullDiffArtifact: got=%q want=%q", got.FullDiffArtifact, "proposal-1.diff")
	}
}

// TestBuildDiffSummary_PreviewHead_50LinesCap は preview_head が先頭 50 行で
// 切り詰められることを検証する (Resolved Decisions §3)。
//
// 入力: 100 行の小さい diff (各行 < 80 バイト、合計 4 KiB 未満)。
// 期待: PreviewHead が改行区切りで 50 行 + 末尾改行のみ (51 個目以降は捨てられる)。
func TestBuildDiffSummary_PreviewHead_50LinesCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("diff --git a/foo.go b/foo.go\n")
	b.WriteString("--- a/foo.go\n")
	b.WriteString("+++ b/foo.go\n")
	b.WriteString("@@ -1,100 +1,100 @@\n")
	for range 100 {
		// 各行は短い ASCII で 4 KiB cap には届かない (Go 1.25 range-over-int)。
		b.WriteString(" context_line\n")
	}
	got := BuildDiffSummary("/abs/foo.go", []byte(b.String()), "proposal-1.diff")
	if got == nil {
		t.Fatal("BuildDiffSummary returned nil")
	}
	// 改行で split し、末尾の空 element を除いて行数をカウント。
	lines := strings.Split(strings.TrimSuffix(got.PreviewHead, "\n"), "\n")
	if len(lines) != 50 {
		t.Errorf("PreviewHead 行数: got=%d want=50 (4 KiB 未満のため 50 行 cap が効くべき)", len(lines))
	}
}

// TestBuildDiffSummary_PreviewHead_4KiBCap は preview_head が 4 KiB を超えないことを検証する
// (Resolved Decisions §3)。
//
// 入力: 1 行 200 バイトの diff を 50 行未満で 4 KiB を超える形 (例: 30 行 × 200 = 6000 バイト)。
// 期待: PreviewHead が 4 KiB (= 4096 バイト) 以下に切り詰められる。
func TestBuildDiffSummary_PreviewHead_4KiBCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("diff --git a/foo.go b/foo.go\n")
	b.WriteString("--- a/foo.go\n")
	b.WriteString("+++ b/foo.go\n")
	b.WriteString("@@ -1,30 +1,30 @@\n")
	// 1 行 ~200 バイト × 30 行 = ~6000 バイトで 4 KiB を超える。50 行 cap には届かない。
	longSegment := strings.Repeat("X", 195) // 195 + " " (先頭) + "\n" (末尾) ~ 197 バイト
	for range 30 {
		b.WriteString(" " + longSegment + "\n")
	}
	got := BuildDiffSummary("/abs/foo.go", []byte(b.String()), "proposal-1.diff")
	if got == nil {
		t.Fatal("BuildDiffSummary returned nil")
	}
	if len(got.PreviewHead) > 4096 {
		t.Errorf("PreviewHead size: got=%d bytes, want <= 4096 bytes (4 KiB cap)", len(got.PreviewHead))
	}
	// 50 行よりは少ないところで切れているはず (各行 ~200 バイトで 4 KiB なら 20 行強)。
	lineCount := strings.Count(got.PreviewHead, "\n")
	if lineCount >= 30 {
		t.Errorf("PreviewHead 行数: got=%d, 4 KiB cap で 30 行未満で切れるべき", lineCount)
	}
}

// TestBuildDiffSummary_NilDiff は空 diff (DiffBytes = nil/empty) のとき nil を返さず、
// 0 行 + 空 PreviewHead の DiffSummaryRecord を返すことを検証する。
//
// orchestration 側で `diff_summary` を nil にする判断は呼び出し側責務 (apply 未到達経路)。
// 本 helper は「diff があった経路で sumamry を組む」前提のため、空入力でも
// 構造体は返し、AddedLines/DeletedLines = 0 にする (orchestration 側で nil 化判定の
// ロジックを混在させない / SRP)。
func TestBuildDiffSummary_NilDiff(t *testing.T) {
	got := BuildDiffSummary("/abs/foo.go", nil, "proposal-1.diff")
	if got == nil {
		t.Fatal("BuildDiffSummary(nil) returned nil; want non-nil with zero counts")
	}
	if got.AddedLines != 0 || got.DeletedLines != 0 {
		t.Errorf("空 diff の counts: got added=%d deleted=%d, want 0/0", got.AddedLines, got.DeletedLines)
	}
	if got.PreviewHead != "" {
		t.Errorf("空 diff の PreviewHead: got=%q, want=\"\"", got.PreviewHead)
	}
	if got.TargetFile != "/abs/foo.go" {
		t.Errorf("TargetFile: got=%q want=%q", got.TargetFile, "/abs/foo.go")
	}
	if got.FullDiffArtifact != "proposal-1.diff" {
		t.Errorf("FullDiffArtifact: got=%q want=%q", got.FullDiffArtifact, "proposal-1.diff")
	}
}
