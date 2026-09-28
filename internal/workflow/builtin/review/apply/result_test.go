// Package apply のテスト本ファイルは Result 永続化型 (CycleRecord /
// RecoveryGuidance / SelfReviewRecord / ResultRecord) と WriteResultJSON helper
// の契約を検証する (tasks.md 1.3 / design.md §Result / Req 5.6, 6.2, 7.1, 7.2,
// 7.3, 7.4)。
//
// 検証観点:
//   - JSON round-trip: 各型が snake_case JSON tag で marshal/unmarshal 可能で
//     値が保存されること (design.md §Result / §Parse)。
//   - VerdictUnset 拒否: WriteResultJSON が FinalVerdict==VerdictUnset を渡された
//     場合に error を返し、ファイル書き出しと artifact 登録のいずれも実行しない
//     不変式 (design.md §Verdict Invariants / Req 7.4)。
//   - RecoveryGuidance omitempty: ResultRecord.RecoveryGuidance が nil のとき
//     JSON 出力に `recovery_guidance` キーが現れないこと (Req 5.6 / 6.2)。
//   - WriteResultJSON 副作用: <runDir>/result.json への書き出しと
//     RecordRunArtifact(ArtifactKindResult, path) の双方が観測できること
//     (Req 7.2 / 7.3)。
package apply

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// resultFakeRecorder は WriteResultJSON 経由の workflow.RecordRunArtifact 呼び出しを
// 観測する test 用 Recorder。akashic.Recorder を満たす最小実装で、本テスト範囲では
// RecordRunArtifact のみ蓄積し、それ以外のメソッドは no-op で nil を返す。
type resultFakeRecorder struct {
	artifacts []akashic.RunArtifact
}

func (r *resultFakeRecorder) RecordWorkflowRunStart(context.Context, akashic.WorkflowRun) error {
	return nil
}
func (r *resultFakeRecorder) RecordWorkflowRunFinish(context.Context, akashic.WorkflowRunFinish) error {
	return nil
}
func (r *resultFakeRecorder) RecordAgentCallStart(context.Context, akashic.AgentCall) error {
	return nil
}
func (r *resultFakeRecorder) RecordAgentCallFinish(context.Context, akashic.AgentCallResult) error {
	return nil
}
func (r *resultFakeRecorder) RecordRawEvent(context.Context, akashic.RawEvent) error {
	return nil
}
func (r *resultFakeRecorder) RecordRunArtifact(_ context.Context, a akashic.RunArtifact) error {
	r.artifacts = append(r.artifacts, a)
	return nil
}

// RecordUserCorrection は memory-distiller-trigger-policy spec Task 3.3 で追加した
// Recorder interface 拡張への追従 (no-op)。本 test は WriteResultJSON 経由の
// RunArtifact 観測が責務で user-correction marker 経路は対象外のため履歴は保持しない。
func (r *resultFakeRecorder) RecordUserCorrection(context.Context, string) error {
	return nil
}

// RecordLearning は agent-learnings spec (task 3.2) で Recorder interface に追加された
// メソッドへの no-op 追従。本 test は WriteResultJSON 経由の RunArtifact 観測が責務で
// learnings 経路は対象外のため履歴は保持しない。
func (r *resultFakeRecorder) RecordLearning(context.Context, akashic.LearningRow) error {
	return nil
}

func (r *resultFakeRecorder) Close() error { return nil }

// newResultCtx は <cwd>/.rilll/review-apply-workflow/<runID>/ ディレクトリを実体作成し、
// RunMetadata + Recorder を ctx に注入した上で (cwd, runDir, ctx, recorder) を返す。
// design.md §Workflow §runDir resolution の規約 (runDir = <cwd>/.rilll/review-apply-workflow/<runID>)
// に揃え、Sequential / 単独経路双方の Workflow 層 defer 経路と同じ前提を作る。
func newResultCtx(t *testing.T, runID string) (cwd, runDir string, ctx context.Context, recorder *resultFakeRecorder) {
	t.Helper()
	cwd = t.TempDir()
	runDir = filepath.Join(cwd, ".rilll", "review-apply-workflow", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", runDir, err)
	}
	recorder = &resultFakeRecorder{}
	ctx = context.Background()
	ctx = workflowctx.ContextWithRunMetadata(ctx, workflowctx.RunMetadata{
		WorkflowRunID: runID,
		WorkflowID:    "review-apply-workflow",
		WorkflowName:  "review-apply-workflow",
		CWD:           cwd,
	})
	ctx = workflow.ContextWithRecorder(ctx, recorder)
	return cwd, runDir, ctx, recorder
}

// TestSelfReviewVerdictStringValues は SelfReviewVerdict の closed set 2 値が
// design.md §Parse 規定の文字列値と一致することを検証する。
// JSONSchemaForSelfReview の `enum` 制約 (task 3.1) と同形を保つ契約。
func TestSelfReviewVerdictStringValues(t *testing.T) {
	if got, want := string(SelfReviewAccept), "accept"; got != want {
		t.Errorf("SelfReviewAccept=%q, want %q", got, want)
	}
	if got, want := string(SelfReviewRegenerate), "regenerate"; got != want {
		t.Errorf("SelfReviewRegenerate=%q, want %q", got, want)
	}
}

// TestSelfReviewRecordRoundTrip は SelfReviewRecord が snake_case JSON tag で
// marshal/unmarshal を round-trip できることを検証する (design.md §Parse)。
func TestSelfReviewRecordRoundTrip(t *testing.T) {
	want := SelfReviewRecord{
		Verdict:    SelfReviewAccept,
		Reason:     "diff is sound",
		Attempt:    1,
		DurationMs: 1234,
	}
	body, err := json.Marshal(&want)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	// snake_case key 検証 (camelCase 派生が紛れ込まないこと)。
	for _, key := range []string{`"verdict"`, `"reason"`, `"attempt"`, `"duration_ms"`} {
		if !strings.Contains(string(body), key) {
			t.Errorf("SelfReviewRecord JSON に snake_case key %s が含まれない: %s", key, string(body))
		}
	}
	var got SelfReviewRecord
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got != want {
		t.Errorf("round-trip: got %+v, want %+v", got, want)
	}
}

// TestCycleRecordRoundTrip は CycleRecord の必須/任意フィールドが snake_case JSON tag で
// round-trip できることを検証する (design.md §Result)。
// AppliedFiles / SelfReview / Diff/ReviewArtifact の omitempty も合わせて確認する。
func TestCycleRecordRoundTrip(t *testing.T) {
	self := SelfReviewRecord{
		Verdict:    SelfReviewRegenerate,
		Reason:     "需要不足",
		Attempt:    2,
		DurationMs: 4567,
	}
	want := CycleRecord{
		Cycle:          2,
		PlanArtifact:   "cycle-2-plan.md",
		DiffArtifact:   "cycle-2-diff.patch",
		ReviewArtifact: "cycle-2-review.json",
		AppliedFiles:   []string{"a.go", "b.go"},
		Verdict:        VerdictApplied,
		SelfReview:     &self,
		DurationMs:     8888,
	}
	body, err := json.Marshal(&want)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	for _, key := range []string{
		`"cycle"`, `"plan_artifact"`, `"diff_artifact"`, `"review_artifact"`,
		`"applied_files"`, `"verdict"`, `"self_review"`, `"duration_ms"`,
	} {
		if !strings.Contains(string(body), key) {
			t.Errorf("CycleRecord JSON に snake_case key %s が含まれない: %s", key, string(body))
		}
	}
	var got CycleRecord
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got.Cycle != want.Cycle || got.PlanArtifact != want.PlanArtifact ||
		got.DiffArtifact != want.DiffArtifact || got.ReviewArtifact != want.ReviewArtifact ||
		got.Verdict != want.Verdict || got.DurationMs != want.DurationMs {
		t.Errorf("CycleRecord scalar mismatch:\n got=%+v\nwant=%+v", got, want)
	}
	if len(got.AppliedFiles) != len(want.AppliedFiles) {
		t.Fatalf("AppliedFiles len: got %d, want %d", len(got.AppliedFiles), len(want.AppliedFiles))
	}
	for i := range want.AppliedFiles {
		if got.AppliedFiles[i] != want.AppliedFiles[i] {
			t.Errorf("AppliedFiles[%d]: got %q, want %q", i, got.AppliedFiles[i], want.AppliedFiles[i])
		}
	}
	if got.SelfReview == nil || *got.SelfReview != self {
		t.Errorf("SelfReview round-trip mismatch: got %+v, want %+v", got.SelfReview, self)
	}
}

// TestCycleRecordOmitEmpty は optional フィールド (DiffArtifact / ReviewArtifact /
// AppliedFiles / SelfReview) が zero 値のとき JSON 出力から omit されることを検証する。
func TestCycleRecordOmitEmpty(t *testing.T) {
	rec := CycleRecord{
		Cycle:        1,
		PlanArtifact: "cycle-1-plan.md",
		Verdict:      VerdictNoFindings,
		DurationMs:   100,
	}
	body, err := json.Marshal(&rec)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	for _, key := range []string{
		`"diff_artifact"`, `"review_artifact"`, `"applied_files"`, `"self_review"`,
	} {
		if strings.Contains(string(body), key) {
			t.Errorf("CycleRecord JSON に omitempty key %s が現れている: %s", key, string(body))
		}
	}
}

// TestRecoveryGuidanceRoundTrip は RecoveryGuidance の round-trip と
// Note omitempty 振る舞いを検証する (design.md §Result)。
func TestRecoveryGuidanceRoundTrip(t *testing.T) {
	want := RecoveryGuidance{
		Files:    []string{"a.go", "b.go"},
		Commands: []string{"git checkout abc123 -- a.go", "git checkout abc123 -- b.go"},
		Note:     "手動で復旧してください",
	}
	body, err := json.Marshal(&want)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	for _, key := range []string{`"files"`, `"commands"`, `"note"`} {
		if !strings.Contains(string(body), key) {
			t.Errorf("RecoveryGuidance JSON に snake_case key %s が含まれない: %s", key, string(body))
		}
	}
	var got RecoveryGuidance
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got.Note != want.Note ||
		len(got.Files) != len(want.Files) || len(got.Commands) != len(want.Commands) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, want)
	}

	// Note 空のときは omitempty で消える。Files / Commands は契約上必須のため空 slice でも残る。
	noNote := RecoveryGuidance{
		Files:    []string{"a.go"},
		Commands: []string{"git checkout HEAD -- a.go"},
	}
	body2, err := json.Marshal(&noNote)
	if err != nil {
		t.Fatalf("json.Marshal(noNote): %v", err)
	}
	if strings.Contains(string(body2), `"note"`) {
		t.Errorf("Note 空のとき omitempty で省略されない: %s", string(body2))
	}
}

// TestResultRecordOmitEmpty は ResultRecord の optional フィールド
// (SkippedRuleSummary / BaselineCommit / CommitSHA / RecoveryGuidance) が
// zero 値のとき JSON 出力から omit されることを検証する (Req 5.6 / 6.2 の
// RecoveryGuidance omitempty 不変式を含む)。
func TestResultRecordOmitEmpty(t *testing.T) {
	rec := ResultRecord{
		WorkflowRunID:   "apply-run-1",
		ReviewRunID:     "review-run-1",
		StartedAt:       "2025-01-01T00:00:00Z",
		FinishedAt:      "2025-01-01T00:00:01Z",
		FinalVerdict:    VerdictNoFindings,
		FinalSummary:    "no findings",
		TargetRuleNames: []string{},
		Cycles:          []CycleRecord{},
	}
	body, err := json.Marshal(&rec)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	for _, key := range []string{
		`"skipped_rule_summary"`, `"baseline_commit"`, `"commit_sha"`, `"recovery_guidance"`,
	} {
		if strings.Contains(string(body), key) {
			t.Errorf("ResultRecord JSON に omitempty key %s が現れている: %s", key, string(body))
		}
	}
	// 必須フィールドは存在を確認する (snake_case の存在検証)。
	for _, key := range []string{
		`"workflow_run_id"`, `"review_run_id"`, `"started_at"`, `"finished_at"`,
		`"final_verdict"`, `"final_summary"`, `"target_rule_names"`, `"cycles"`,
	} {
		if !strings.Contains(string(body), key) {
			t.Errorf("ResultRecord JSON に必須 snake_case key %s が含まれない: %s", key, string(body))
		}
	}
}

// TestResultRecordRoundTrip は ResultRecord 全フィールドの round-trip を検証する。
// RecoveryGuidance / Cycles / SkippedRuleSummary / BaselineCommit / CommitSHA を
// すべて埋めた状態で marshal/unmarshal が値を保つことを確認する。
func TestResultRecordRoundTrip(t *testing.T) {
	rg := &RecoveryGuidance{
		Files:    []string{"a.go"},
		Commands: []string{"git checkout abc -- a.go"},
		Note:     "手動復旧",
	}
	cycle := CycleRecord{
		Cycle:        1,
		PlanArtifact: "cycle-1-plan.md",
		Verdict:      VerdictApplied,
		DurationMs:   100,
	}
	want := ResultRecord{
		WorkflowRunID:      "apply-run-2",
		ReviewRunID:        "review-run-2",
		StartedAt:          "2025-05-04T00:00:00Z",
		FinishedAt:         "2025-05-04T00:00:10Z",
		FinalVerdict:       VerdictApplied,
		FinalSummary:       "applied 1 commit",
		TargetRuleNames:    []string{"rule-a", "rule-b"},
		SkippedRuleSummary: map[string]int{"no_findings": 2, "skipped": 1},
		Cycles:             []CycleRecord{cycle},
		BaselineCommit:     "abcdef1234",
		CommitSHA:          "1234567890",
		RecoveryGuidance:   rg,
	}
	body, err := json.Marshal(&want)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var got ResultRecord
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got.WorkflowRunID != want.WorkflowRunID || got.ReviewRunID != want.ReviewRunID ||
		got.StartedAt != want.StartedAt || got.FinishedAt != want.FinishedAt ||
		got.FinalVerdict != want.FinalVerdict || got.FinalSummary != want.FinalSummary ||
		got.BaselineCommit != want.BaselineCommit || got.CommitSHA != want.CommitSHA {
		t.Errorf("scalar mismatch:\n got=%+v\nwant=%+v", got, want)
	}
	if len(got.Cycles) != 1 || got.Cycles[0].Cycle != cycle.Cycle ||
		got.Cycles[0].Verdict != cycle.Verdict {
		t.Errorf("Cycles round-trip mismatch: got %+v", got.Cycles)
	}
	if got.SkippedRuleSummary["no_findings"] != 2 || got.SkippedRuleSummary["skipped"] != 1 {
		t.Errorf("SkippedRuleSummary round-trip mismatch: got %+v", got.SkippedRuleSummary)
	}
	if got.RecoveryGuidance == nil || got.RecoveryGuidance.Note != rg.Note ||
		len(got.RecoveryGuidance.Files) != 1 || len(got.RecoveryGuidance.Commands) != 1 {
		t.Errorf("RecoveryGuidance round-trip mismatch: got %+v", got.RecoveryGuidance)
	}
}

// TestWriteResultJSONHappyPath は WriteResultJSON が <runDir>/result.json を書き出し、
// workflow.RecordRunArtifact(ArtifactKindResult, path) を 1 回呼び出すことを検証する
// (Req 7.2 / 7.3)。
func TestWriteResultJSONHappyPath(t *testing.T) {
	_, runDir, ctx, rec := newResultCtx(t, "happy-run")
	record := &ResultRecord{
		WorkflowRunID:   "happy-run",
		ReviewRunID:     "review-run",
		StartedAt:       "2025-05-04T00:00:00Z",
		FinishedAt:      "2025-05-04T00:00:01Z",
		FinalVerdict:    VerdictApplied,
		FinalSummary:    "applied",
		TargetRuleNames: []string{"rule-a"},
		Cycles:          []CycleRecord{{Cycle: 1, PlanArtifact: "cycle-1-plan.md", Verdict: VerdictApplied, DurationMs: 1}},
	}

	if err := WriteResultJSON(ctx, runDir, record); err != nil {
		t.Fatalf("WriteResultJSON: %v", err)
	}

	// (1) ファイル書き出しの観測。
	path := filepath.Join(runDir, "result.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%s): %v", path, err)
	}
	var disk ResultRecord
	if err := json.Unmarshal(body, &disk); err != nil {
		t.Fatalf("disk JSON unmarshal: %v\n%s", err, string(body))
	}
	if disk.FinalVerdict != VerdictApplied || disk.WorkflowRunID != "happy-run" {
		t.Errorf("disk record mismatch: %+v", disk)
	}

	// (2) RecordRunArtifact 呼び出し観測 (kind=result, path=<runDir>/result.json)。
	if got, want := len(rec.artifacts), 1; got != want {
		t.Fatalf("RecordRunArtifact 呼び出し件数=%d, want %d", got, want)
	}
	a := rec.artifacts[0]
	if a.Kind != akashic.ArtifactKindResult {
		t.Errorf("artifact.Kind=%q, want %q", a.Kind, akashic.ArtifactKindResult)
	}
	// path は NormalizeArtifactPath で EvalSymlinks 適用済み絶対 clean path に揃う。
	wantSuffix := filepath.Join(".rilll", "review-apply-workflow", "happy-run", "result.json")
	if !strings.HasSuffix(a.Path, wantSuffix) {
		t.Errorf("artifact.Path=%q, want suffix %q", a.Path, wantSuffix)
	}
	if a.WorkflowRunID != "happy-run" {
		t.Errorf("artifact.WorkflowRunID=%q, want %q", a.WorkflowRunID, "happy-run")
	}
}

// TestWriteResultJSONRejectsVerdictUnset は FinalVerdict==VerdictUnset を
// 渡された場合に WriteResultJSON が error を返し、ファイル書き出しと artifact 登録の
// いずれも実行しないことを検証する (Req 7.4 / design.md §Verdict Invariants)。
func TestWriteResultJSONRejectsVerdictUnset(t *testing.T) {
	_, runDir, ctx, rec := newResultCtx(t, "unset-run")
	record := &ResultRecord{
		WorkflowRunID: "unset-run",
		ReviewRunID:   "review-run",
		FinalVerdict:  VerdictUnset, // 不変式違反: invariant test の観測点。
		FinalSummary:  "should not be persisted",
	}

	err := WriteResultJSON(ctx, runDir, record)
	if err == nil {
		t.Fatal("WriteResultJSON: error が返らない (VerdictUnset 拒否の不変式違反)")
	}

	path := filepath.Join(runDir, "result.json")
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("VerdictUnset 拒否経路で result.json が書き出された (path=%s, statErr=%v)", path, statErr)
	}
	if got := len(rec.artifacts); got != 0 {
		t.Errorf("VerdictUnset 拒否経路で RecordRunArtifact が呼ばれた (件数=%d)", got)
	}
}

// TestWriteResultJSONRejectsNilRecord は record==nil を渡された場合に
// WriteResultJSON が error を返し、ファイル書き出しと artifact 登録のいずれも
// 実行しないことを検証する (defensive contract: 5.1 defer から呼ばれる経路で
// nil pointer dereference を起こさない)。
func TestWriteResultJSONRejectsNilRecord(t *testing.T) {
	_, runDir, ctx, rec := newResultCtx(t, "nil-run")

	err := WriteResultJSON(ctx, runDir, nil)
	if err == nil {
		t.Fatal("WriteResultJSON(nil): error が返らない")
	}

	path := filepath.Join(runDir, "result.json")
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("nil record 経路で result.json が書き出された (path=%s)", path)
	}
	if got := len(rec.artifacts); got != 0 {
		t.Errorf("nil record 経路で RecordRunArtifact が呼ばれた (件数=%d)", got)
	}
}

// TestWriteResultJSONOmitsRecoveryGuidance は RecoveryGuidance が nil のとき
// 書き出された JSON に `recovery_guidance` キーが現れないことを検証する
// (Req 5.6 / 6.2 の omitempty 不変式 / 利用者向け表示で RecoveryGuidance ありが
// 「未復旧ファイル発生」のシグナルとして使われるため)。
func TestWriteResultJSONOmitsRecoveryGuidance(t *testing.T) {
	_, runDir, ctx, _ := newResultCtx(t, "omit-run")
	record := &ResultRecord{
		WorkflowRunID:   "omit-run",
		ReviewRunID:     "review-run",
		StartedAt:       "2025-05-04T00:00:00Z",
		FinishedAt:      "2025-05-04T00:00:01Z",
		FinalVerdict:    VerdictNoFindings,
		FinalSummary:    "no findings",
		TargetRuleNames: []string{},
		Cycles:          []CycleRecord{},
		// RecoveryGuidance は nil のまま。
	}

	if err := WriteResultJSON(ctx, runDir, record); err != nil {
		t.Fatalf("WriteResultJSON: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(runDir, "result.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(body), `"recovery_guidance"`) {
		t.Errorf("RecoveryGuidance nil で result.json に `recovery_guidance` キーが現れる: %s", string(body))
	}
}
