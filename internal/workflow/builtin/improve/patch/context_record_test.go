// 本ファイルは tasks.md 9.2 の context.json schema 型 (ContextRecord /
// SampleRunSummary / BuildInputSummary) と、PromptContext + BuildInput から
// ContextRecord を構築する helper の契約を機械検証するテスト群。
//
// 検証観点 (Req 2.1, 2.4, 2.5 / design.md §Data Contracts & Integration §8):
//  1. JSON marshal → unmarshal round-trip で全フィールド等価。
//  2. snake_case JSON タグの機械検証 (design.md §8 と整合)。
//  3. current_source_sha256 が CurrentSource 全文の SHA-256 hex 64 文字であること。
//  4. sample_runs[] のサマリ化 (agent_call_count / raw_event_count / artifact_count)
//     が PromptContext.SampleRuns の各要素 length から正しく算出されること。
//  5. build_input_at_capture が BuildInput.WithDefaults() 後の主要パラメータと一致すること。
//  6. JSON 出力が design.md §8 の例と整合する snake_case フィールド名を持つこと。
package patch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

// completeContextRecord は全フィールドを埋めた状態の ContextRecord を返す
// (round-trip テストの起点として再利用)。
func completeContextRecord() ContextRecord {
	return ContextRecord{
		WorkflowRunID:        "improve-run-456",
		TargetWorkflowID:     "git-commit",
		TargetGoFile:         "/abs/path/internal/workflow/git_commit.go",
		RunIDs:               []string{"origin-run-1"},
		CurrentSourceSHA256:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		CurrentSourceBytes:   4321,
		SampleRuns:           []SampleRunSummary{{RunID: "origin-run-1", Success: false, AgentCallCount: 3, RawEventCount: 50, ArtifactCount: 2, IsOriginatedFromHandoff: true}},
		FailureClustersCount: 4,
		Limited:              false,
		BuildInputAtCapture: BuildInputSummary{
			SampleSuccess:         3,
			SampleFailure:         5,
			MaxRawEvents:          50,
			MaxArtifactBytes:      10485760,
			MaxRegenerateAttempts: 5,
		},
		CapturedAt: "2026-05-03T10:00:00.000000000Z",
	}
}

// TestContextRecord_JSONRoundTrip は ContextRecord 全フィールドの round-trip 検証。
func TestContextRecord_JSONRoundTrip(t *testing.T) {
	original := completeContextRecord()

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}

	var decoded ContextRecord
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v", err)
	}

	// 再 marshal で文字列比較 (フィールド順序変動を吸収)。
	reEncoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("json.Marshal of decoded returned error: %v", err)
	}
	if string(reEncoded) != string(encoded) {
		t.Errorf("round-trip mismatch:\noriginal=%s\nreencoded=%s", string(encoded), string(reEncoded))
	}
}

// TestContextRecord_RequiredSnakeCaseKeys は ContextRecord の必須 snake_case キーが
// JSON 出力に必ず含まれることを検証する (design.md §8 / Req 2.1, 2.4, 2.5)。
func TestContextRecord_RequiredSnakeCaseKeys(t *testing.T) {
	rec := completeContextRecord()
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
		`"current_source_sha256"`,
		`"current_source_bytes"`,
		`"sample_runs"`,
		`"failure_clusters_count"`,
		`"limited"`,
		`"build_input_at_capture"`,
		`"captured_at"`,
	}
	for _, key := range requiredKeys {
		if !strings.Contains(out, key) {
			t.Errorf("必須キー %q が JSON 出力に含まれていない: %s", key, out)
		}
	}
}

// TestSampleRunSummary_JSONRoundTrip は SampleRunSummary 単体の round-trip 検証。
func TestSampleRunSummary_JSONRoundTrip(t *testing.T) {
	original := SampleRunSummary{
		RunID:                   "origin-run-1",
		Success:                 false,
		AgentCallCount:          3,
		RawEventCount:           50,
		ArtifactCount:           2,
		IsOriginatedFromHandoff: true,
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	for _, key := range []string{
		`"run_id"`, `"success"`, `"agent_call_count"`, `"raw_event_count"`, `"artifact_count"`, `"is_originated_from_handoff"`,
	} {
		if !strings.Contains(string(encoded), key) {
			t.Errorf("SampleRunSummary JSON に key %q が含まれない: %s", key, encoded)
		}
	}

	var decoded SampleRunSummary
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip mismatch: got=%#v want=%#v", decoded, original)
	}
}

// TestBuildInputSummary_JSONRoundTrip は BuildInputSummary 単体の round-trip 検証。
func TestBuildInputSummary_JSONRoundTrip(t *testing.T) {
	original := BuildInputSummary{
		SampleSuccess:         3,
		SampleFailure:         5,
		MaxRawEvents:          50,
		MaxArtifactBytes:      10485760,
		MaxRegenerateAttempts: 5,
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	for _, key := range []string{
		`"sample_success"`, `"sample_failure"`, `"max_raw_events"`, `"max_artifact_bytes"`, `"max_regenerate_attempts"`,
	} {
		if !strings.Contains(string(encoded), key) {
			t.Errorf("BuildInputSummary JSON に key %q が含まれない: %s", key, encoded)
		}
	}

	var decoded BuildInputSummary
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip mismatch: got=%#v want=%#v", decoded, original)
	}
}

// makeFixturePromptContextForRecord はテスト用の PromptContext を構築する helper
// (prompt_test.go の makeFixturePromptContext と機能が異なるため別名)。
// SampleRuns / FailureClusters まで埋めて、サマリ化検証 (count 系) ができる形にする。
func makeFixturePromptContextForRecord() PromptContext {
	currentSource := "package workflow\n\nfunc Foo() {}\n"
	return PromptContext{
		TargetWorkflowID: "git-commit",
		TargetGoFile:     "/abs/path/internal/workflow/git_commit.go",
		CurrentSource:    currentSource,
		SampleRuns: []SampleRun{
			{
				Run:                     akashic.RunHeader{RunID: "origin-run-1", Success: boolPtr(false)},
				AgentCalls:              make([]akashic.AgentCallRow, 3),
				RawEvents:               make([]akashic.RawEventRow, 50),
				Artifacts:               make([]ArtifactSnippet, 2),
				IsOriginatedFromHandoff: true,
			},
			{
				Run:                     akashic.RunHeader{RunID: "sample-success-1", Success: boolPtr(true)},
				AgentCalls:              make([]akashic.AgentCallRow, 1),
				RawEvents:               make([]akashic.RawEventRow, 10),
				Artifacts:               make([]ArtifactSnippet, 1),
				IsOriginatedFromHandoff: false,
			},
		},
		FailureClusters: []akashic.FailureCluster{
			{FailureCategory: "compile_error"},
			{FailureCategory: "test_failure"},
		},
		Limited: false,
	}
}

// TestBuildContextRecord_FromPromptContext は PromptContext + BuildInput から
// ContextRecord が design.md §8 に従って正しく構築されることを検証する
// (本 task の中心観察可能完了条件)。
func TestBuildContextRecord_FromPromptContext(t *testing.T) {
	prompt := makeFixturePromptContextForRecord()
	in := BuildInput{
		TargetWorkflowID:      "git-commit",
		RunIDs:                []string{"origin-run-1"},
		TargetGoFile:          "/abs/path/internal/workflow/git_commit.go",
		SampleSuccess:         3,
		SampleFailure:         5,
		MaxRawEvents:          50,
		MaxArtifactBytes:      10485760,
		MaxRegenerateAttempts: 5,
	}
	capturedAt := time.Date(2026, 5, 3, 10, 0, 0, 0, time.UTC)

	rec := BuildContextRecord("improve-run-789", prompt, in, capturedAt)

	if rec.WorkflowRunID != "improve-run-789" {
		t.Errorf("WorkflowRunID got=%q want=improve-run-789", rec.WorkflowRunID)
	}
	if rec.TargetWorkflowID != "git-commit" {
		t.Errorf("TargetWorkflowID got=%q want=git-commit", rec.TargetWorkflowID)
	}
	if rec.TargetGoFile != "/abs/path/internal/workflow/git_commit.go" {
		t.Errorf("TargetGoFile got=%q", rec.TargetGoFile)
	}
	if len(rec.RunIDs) != 1 || rec.RunIDs[0] != "origin-run-1" {
		t.Errorf("RunIDs got=%v want=[origin-run-1]", rec.RunIDs)
	}

	// current_source_sha256 = sha256(CurrentSource) hex 64 文字
	wantSum := sha256.Sum256([]byte(prompt.CurrentSource))
	wantHex := hex.EncodeToString(wantSum[:])
	if rec.CurrentSourceSHA256 != wantHex {
		t.Errorf("CurrentSourceSHA256 got=%q want=%q", rec.CurrentSourceSHA256, wantHex)
	}
	if len(rec.CurrentSourceSHA256) != 64 {
		t.Errorf("CurrentSourceSHA256 length got=%d want=64", len(rec.CurrentSourceSHA256))
	}

	// current_source_bytes は CurrentSource のバイト数
	if rec.CurrentSourceBytes != int64(len(prompt.CurrentSource)) {
		t.Errorf("CurrentSourceBytes got=%d want=%d", rec.CurrentSourceBytes, len(prompt.CurrentSource))
	}

	// sample_runs[] サマリ化検証 (agent_call_count / raw_event_count / artifact_count は
	// 各 SampleRun の slice length と一致)
	if len(rec.SampleRuns) != len(prompt.SampleRuns) {
		t.Fatalf("SampleRuns length got=%d want=%d", len(rec.SampleRuns), len(prompt.SampleRuns))
	}
	for i, summary := range rec.SampleRuns {
		want := prompt.SampleRuns[i]
		if summary.RunID != want.Run.RunID {
			t.Errorf("SampleRuns[%d].RunID got=%q want=%q", i, summary.RunID, want.Run.RunID)
		}
		// Run.Success は *bool。nil の場合は false (run 進行中) として記録する契約。
		wantSuccess := want.Run.Success != nil && *want.Run.Success
		if summary.Success != wantSuccess {
			t.Errorf("SampleRuns[%d].Success got=%v want=%v", i, summary.Success, wantSuccess)
		}
		if summary.AgentCallCount != len(want.AgentCalls) {
			t.Errorf("SampleRuns[%d].AgentCallCount got=%d want=%d", i, summary.AgentCallCount, len(want.AgentCalls))
		}
		if summary.RawEventCount != len(want.RawEvents) {
			t.Errorf("SampleRuns[%d].RawEventCount got=%d want=%d", i, summary.RawEventCount, len(want.RawEvents))
		}
		if summary.ArtifactCount != len(want.Artifacts) {
			t.Errorf("SampleRuns[%d].ArtifactCount got=%d want=%d", i, summary.ArtifactCount, len(want.Artifacts))
		}
		if summary.IsOriginatedFromHandoff != want.IsOriginatedFromHandoff {
			t.Errorf("SampleRuns[%d].IsOriginatedFromHandoff got=%v want=%v", i, summary.IsOriginatedFromHandoff, want.IsOriginatedFromHandoff)
		}
	}

	// failure_clusters_count = len(prompt.FailureClusters)
	if rec.FailureClustersCount != len(prompt.FailureClusters) {
		t.Errorf("FailureClustersCount got=%d want=%d", rec.FailureClustersCount, len(prompt.FailureClusters))
	}

	// limited = prompt.Limited
	if rec.Limited != prompt.Limited {
		t.Errorf("Limited got=%v want=%v", rec.Limited, prompt.Limited)
	}

	// build_input_at_capture が BuildInput.WithDefaults() 後の主要パラメータと一致
	wantBuild := in.WithDefaults()
	if rec.BuildInputAtCapture.SampleSuccess != wantBuild.SampleSuccess {
		t.Errorf("BuildInputAtCapture.SampleSuccess got=%d want=%d", rec.BuildInputAtCapture.SampleSuccess, wantBuild.SampleSuccess)
	}
	if rec.BuildInputAtCapture.SampleFailure != wantBuild.SampleFailure {
		t.Errorf("BuildInputAtCapture.SampleFailure got=%d want=%d", rec.BuildInputAtCapture.SampleFailure, wantBuild.SampleFailure)
	}
	if rec.BuildInputAtCapture.MaxRawEvents != wantBuild.MaxRawEvents {
		t.Errorf("BuildInputAtCapture.MaxRawEvents got=%d want=%d", rec.BuildInputAtCapture.MaxRawEvents, wantBuild.MaxRawEvents)
	}
	if rec.BuildInputAtCapture.MaxArtifactBytes != wantBuild.MaxArtifactBytes {
		t.Errorf("BuildInputAtCapture.MaxArtifactBytes got=%d want=%d", rec.BuildInputAtCapture.MaxArtifactBytes, wantBuild.MaxArtifactBytes)
	}
	if rec.BuildInputAtCapture.MaxRegenerateAttempts != wantBuild.MaxRegenerateAttempts {
		t.Errorf("BuildInputAtCapture.MaxRegenerateAttempts got=%d want=%d", rec.BuildInputAtCapture.MaxRegenerateAttempts, wantBuild.MaxRegenerateAttempts)
	}

	// captured_at は RFC3339Nano 形式 (UTC)
	wantCaptured := capturedAt.UTC().Format(time.RFC3339Nano)
	if rec.CapturedAt != wantCaptured {
		t.Errorf("CapturedAt got=%q want=%q", rec.CapturedAt, wantCaptured)
	}
}

// TestBuildContextRecord_AppliesDefaults はゼロ値の BuildInput を渡したとき、
// build_input_at_capture が WithDefaults() 後の既定値で記録されることを検証する。
// (再現性確保: ContextRecord 単独で「実行時に何の defaults が適用されたか」が読み取れる)
func TestBuildContextRecord_AppliesDefaults(t *testing.T) {
	prompt := makeFixturePromptContextForRecord()
	// SampleSuccess / SampleFailure / MaxRawEvents / MaxArtifactBytes /
	// MaxRegenerateAttempts は全て 0 (zero value) のまま渡す。
	in := BuildInput{
		TargetWorkflowID: "git-commit",
		RunIDs:           []string{"origin-run-1"},
		TargetGoFile:     "/abs/path/internal/workflow/git_commit.go",
	}
	rec := BuildContextRecord("improve-run-zero", prompt, in, time.Now().UTC())

	if rec.BuildInputAtCapture.SampleSuccess != DefaultSampleSuccess {
		t.Errorf("SampleSuccess default got=%d want=%d", rec.BuildInputAtCapture.SampleSuccess, DefaultSampleSuccess)
	}
	if rec.BuildInputAtCapture.SampleFailure != DefaultSampleFailure {
		t.Errorf("SampleFailure default got=%d want=%d", rec.BuildInputAtCapture.SampleFailure, DefaultSampleFailure)
	}
	if rec.BuildInputAtCapture.MaxRawEvents != DefaultMaxRawEvents {
		t.Errorf("MaxRawEvents default got=%d want=%d", rec.BuildInputAtCapture.MaxRawEvents, DefaultMaxRawEvents)
	}
	if rec.BuildInputAtCapture.MaxArtifactBytes != akashic.DefaultArtifactReadMaxBytes {
		t.Errorf("MaxArtifactBytes default got=%d want=%d", rec.BuildInputAtCapture.MaxArtifactBytes, akashic.DefaultArtifactReadMaxBytes)
	}
	if rec.BuildInputAtCapture.MaxRegenerateAttempts != DefaultMaxRegenerateAttempts {
		t.Errorf("MaxRegenerateAttempts default got=%d want=%d", rec.BuildInputAtCapture.MaxRegenerateAttempts, DefaultMaxRegenerateAttempts)
	}
}

// TestBuildContextRecord_LimitedTrue は PromptContext.Limited=true のとき
// ContextRecord.Limited も true として伝播することを検証する (Req 2.5)。
func TestBuildContextRecord_LimitedTrue(t *testing.T) {
	prompt := makeFixturePromptContextForRecord()
	prompt.Limited = true
	prompt.SampleRuns = prompt.SampleRuns[:1] // origin run のみ

	in := BuildInput{
		TargetWorkflowID: "git-commit",
		RunIDs:           []string{"origin-run-1"},
		TargetGoFile:     "/abs/path/internal/workflow/git_commit.go",
	}
	rec := BuildContextRecord("improve-run-limited", prompt, in, time.Now().UTC())

	if !rec.Limited {
		t.Errorf("Limited got=false want=true (Req 2.5)")
	}
	if len(rec.SampleRuns) != 1 {
		t.Errorf("SampleRuns length got=%d want=1", len(rec.SampleRuns))
	}
}

// TestBuildContextRecord_MatchesDesignExample は design.md §8 の JSON 例と
// 構造的に整合する出力が生成されることを検証する (本 task の観察可能完了条件)。
func TestBuildContextRecord_MatchesDesignExample(t *testing.T) {
	rec := completeContextRecord()
	encoded, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatalf("json.MarshalIndent returned error: %v", err)
	}

	// design.md §8 の例にあるキーと値の組み合わせを確認 (順不同)。
	out := string(encoded)
	checks := []struct {
		fragment string
		desc     string
	}{
		{`"target_go_file": "/abs/path/internal/workflow/git_commit.go"`, "target_go_file 絶対パス"},
		{`"current_source_sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"`, "current_source_sha256 hex 64 文字"},
		{`"current_source_bytes": 4321`, "current_source_bytes は数値"},
		{`"failure_clusters_count": 4`, "failure_clusters_count は数値"},
		{`"limited": false`, "limited bool"},
		{`"sample_success": 3`, "build_input_at_capture.sample_success"},
		{`"max_artifact_bytes": 10485760`, "build_input_at_capture.max_artifact_bytes"},
		{`"max_regenerate_attempts": 5`, "build_input_at_capture.max_regenerate_attempts"},
	}
	for _, c := range checks {
		if !strings.Contains(out, c.fragment) {
			t.Errorf("design.md §8 例との不一致 (%s): fragment=%q\noutput=%s", c.desc, c.fragment, out)
		}
	}
}
