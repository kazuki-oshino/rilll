package kiro

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/inquiryloop"
)

// TestAuditPersistContext は persistContext が 00-context.json を期待した内容で書き出すことを検証する。
func TestAuditPersistContext(t *testing.T) {
	runDir := t.TempDir()
	startedAt := time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC)
	ctx := runContext{
		Feature:     "demo-feature",
		BriefPath:   "/abs/path/to/.kiro/specs/demo-feature/brief.md",
		PersonaName: "architecture-quality",
		PersonaPath: "embed:facets/personas/architecture-quality.md",
		StartedAt:   startedAt,
	}

	if err := persistContext(runDir, ctx); err != nil {
		t.Fatalf("persistContext returned error: %v", err)
	}

	contextFile := filepath.Join(runDir, "00-context.json")
	raw, err := os.ReadFile(contextFile)
	if err != nil {
		t.Fatalf("00-context.json not written: %v", err)
	}

	var got runContext
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("00-context.json must be valid JSON: %v\ncontent=%s", err, string(raw))
	}
	if got.Feature != ctx.Feature {
		t.Errorf("feature mismatch: got=%q want=%q", got.Feature, ctx.Feature)
	}
	if got.BriefPath != ctx.BriefPath {
		t.Errorf("briefPath mismatch: got=%q want=%q", got.BriefPath, ctx.BriefPath)
	}
	if got.PersonaName != ctx.PersonaName {
		t.Errorf("personaName mismatch: got=%q want=%q", got.PersonaName, ctx.PersonaName)
	}
	if got.PersonaPath != ctx.PersonaPath {
		t.Errorf("personaPath mismatch: got=%q want=%q", got.PersonaPath, ctx.PersonaPath)
	}
	if !got.StartedAt.Equal(startedAt) {
		t.Errorf("startedAt mismatch: got=%v want=%v", got.StartedAt, startedAt)
	}
}

// TestAuditPersistInquiry は persistInquiry が round-N-inquiry.md を期待したフォーマットで書き出すことを検証する。
func TestAuditPersistInquiry(t *testing.T) {
	runDir := t.TempDir()
	inq := inquiryloop.Inquiry{
		Title:      "コミット粒度の決定",
		Background: "1 プロンプト = 1 コミットで運用したい",
		Question:   "/git-commit を各段で呼ぶ方針で問題ないか？",
		Options: []inquiryloop.InquiryOption{
			{Label: "yes", Description: "提案通り進める"},
			{Label: "no", Description: "別案を提示してほしい"},
			{Label: "skip-only"},
		},
	}

	if err := persistInquiry(runDir, 2, "spec-requirements", 1, inq); err != nil {
		t.Fatalf("persistInquiry returned error: %v", err)
	}

	inquiryFile := filepath.Join(runDir, "02-spec-requirements", "round-1-inquiry.md")
	raw, err := os.ReadFile(inquiryFile)
	if err != nil {
		t.Fatalf("round-1-inquiry.md not written: %v", err)
	}
	content := string(raw)

	mustContain(t, content, "spec-requirements")
	mustContain(t, content, "round")
	mustContain(t, content, inq.Title)
	mustContain(t, content, inq.Background)
	mustContain(t, content, inq.Question)
	mustContain(t, content, "yes")
	mustContain(t, content, "提案通り進める")
	mustContain(t, content, "no")
	mustContain(t, content, "別案を提示してほしい")
	mustContain(t, content, "skip-only")
}

// TestAuditPersistAnswer は persistAnswer が round-N-answer.md を期待したフォーマットで書き出すことを検証する。
func TestAuditPersistAnswer(t *testing.T) {
	runDir := t.TempDir()
	personaName := "architecture-quality"
	answer := "工数を惜しまず、長期保守性を最優先する設計を採用してください。"

	if err := persistAnswer(runDir, 4, "spec-design", 2, personaName, answer); err != nil {
		t.Fatalf("persistAnswer returned error: %v", err)
	}

	answerFile := filepath.Join(runDir, "04-spec-design", "round-2-answer.md")
	raw, err := os.ReadFile(answerFile)
	if err != nil {
		t.Fatalf("round-2-answer.md not written: %v", err)
	}
	content := string(raw)

	mustContain(t, content, "spec-design")
	mustContain(t, content, "round")
	mustContain(t, content, personaName)
	mustContain(t, content, answer)
}

// TestAuditPersistStepResult は persistStepResult が <NN>-<phase>/result.json を期待した内容で書き出すことを検証する。
func TestAuditPersistStepResult(t *testing.T) {
	runDir := t.TempDir()
	sr := stepResult{
		Order:        3,
		Phase:        "validate-gap",
		Rounds:       2,
		LastSession:  "session-xyz",
		Summary:      "差分なし。次へ進めて問題ない。",
		CommitOK:     true,
		CommitOutput: "no diff to commit",
	}

	if err := persistStepResult(runDir, sr); err != nil {
		t.Fatalf("persistStepResult returned error: %v", err)
	}

	resultFile := filepath.Join(runDir, "03-validate-gap", "result.json")
	raw, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatalf("step result.json not written: %v", err)
	}

	var got RunResultStep
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("step result.json must be valid JSON: %v\ncontent=%s", err, string(raw))
	}
	if got.Order != sr.Order {
		t.Errorf("order mismatch: got=%d want=%d", got.Order, sr.Order)
	}
	if got.Phase != sr.Phase {
		t.Errorf("phase mismatch: got=%q want=%q", got.Phase, sr.Phase)
	}
	if got.Rounds != sr.Rounds {
		t.Errorf("rounds mismatch: got=%d want=%d", got.Rounds, sr.Rounds)
	}
	if got.LastSession != sr.LastSession {
		t.Errorf("lastSession mismatch: got=%q want=%q", got.LastSession, sr.LastSession)
	}
	if got.Summary != sr.Summary {
		t.Errorf("summary mismatch: got=%q want=%q", got.Summary, sr.Summary)
	}
	if got.CommitOK != sr.CommitOK {
		t.Errorf("commitOK mismatch: got=%v want=%v", got.CommitOK, sr.CommitOK)
	}
	if got.CommitOutput != sr.CommitOutput {
		t.Errorf("commitOutput mismatch: got=%q want=%q", got.CommitOutput, sr.CommitOutput)
	}
}

// TestAuditPersistRunResult は persistRunResult が top-level result.json を期待した内容で書き出すことを検証する。
// すべての RunTerminationReason 8 値が JSON にラウンドトリップ可能であることもここで担保する。
func TestAuditPersistRunResult(t *testing.T) {
	allReasons := []RunTerminationReason{
		RunReasonCompleted,
		RunReasonCancelledByUser,
		RunReasonMissingFeature,
		RunReasonMissingPersona,
		RunReasonPromptFailure,
		RunReasonSchemaViolation,
		RunReasonLoopLimit,
		RunReasonCommitFailure,
	}
	if len(allReasons) != 8 {
		t.Fatalf("RunTerminationReason は 8 値であること: got=%d", len(allReasons))
	}

	for _, reason := range allReasons {
		t.Run(string(reason), func(t *testing.T) {
			runDir := t.TempDir()
			startedAt := time.Date(2026, 4, 29, 9, 0, 0, 0, time.UTC)
			completedAt := startedAt.Add(3 * time.Minute)
			rr := RunResult{
				RunID:             "run-001",
				WorkflowID:        "kiro-spec-loop",
				Feature:           "demo-feature",
				PersonaName:       "architecture-quality",
				PersonaPath:       "embed:facets/personas/architecture-quality.md",
				StartedAt:         startedAt,
				CompletedAt:       &completedAt,
				TerminationReason: reason,
				Steps: []RunResultStep{
					{Order: 1, Phase: "spec-init", Rounds: 0, LastSession: "s1", Summary: "ok", CommitOK: true},
				},
			}
			if reason != RunReasonCompleted {
				rr.AbortStep = &RunResultAbortStep{Order: 1, Phase: "spec-init"}
				rr.AbortMessage = "テスト abort: " + string(reason)
			}

			if err := persistRunResult(runDir, rr); err != nil {
				t.Fatalf("persistRunResult returned error: %v", err)
			}

			resultFile := filepath.Join(runDir, "result.json")
			raw, err := os.ReadFile(resultFile)
			if err != nil {
				t.Fatalf("result.json not written: %v", err)
			}

			// (1) JSON として valid
			var got RunResult
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("result.json must be valid JSON: %v\ncontent=%s", err, string(raw))
			}
			// (2) terminationReason フィールドが書かれている
			if !strings.Contains(string(raw), "\"terminationReason\"") {
				t.Errorf("result.json に terminationReason フィールドが存在しない:\n%s", string(raw))
			}
			// (3) 値が 8 値のいずれか
			if !isValidRunTerminationReason(got.TerminationReason) {
				t.Errorf("terminationReason が 8 値の範囲外: got=%q", got.TerminationReason)
			}
			if got.TerminationReason != reason {
				t.Errorf("terminationReason mismatch: got=%q want=%q", got.TerminationReason, reason)
			}
			// (4) 主要フィールドのラウンドトリップ
			if got.RunID != rr.RunID {
				t.Errorf("runId mismatch: got=%q want=%q", got.RunID, rr.RunID)
			}
			if got.WorkflowID != rr.WorkflowID {
				t.Errorf("workflowId mismatch: got=%q want=%q", got.WorkflowID, rr.WorkflowID)
			}
			if got.Feature != rr.Feature {
				t.Errorf("feature mismatch: got=%q want=%q", got.Feature, rr.Feature)
			}
			if got.PersonaName != rr.PersonaName {
				t.Errorf("personaName mismatch: got=%q want=%q", got.PersonaName, rr.PersonaName)
			}
			if !got.StartedAt.Equal(startedAt) {
				t.Errorf("startedAt mismatch: got=%v want=%v", got.StartedAt, startedAt)
			}
			if reason != RunReasonCompleted {
				if got.AbortStep == nil {
					t.Errorf("abortStep が nil（abort シナリオでは必須）")
				} else if got.AbortStep.Phase != "spec-init" {
					t.Errorf("abortStep.phase mismatch: got=%q", got.AbortStep.Phase)
				}
				if got.AbortMessage == "" {
					t.Errorf("abortMessage が空（abort シナリオでは必須）")
				}
			}
		})
	}
}

// mustContain は content に sub が含まれていなければテスト失敗させる小さなヘルパー。
func mustContain(t *testing.T, content, sub string) {
	t.Helper()
	if !strings.Contains(content, sub) {
		t.Errorf("expected output to contain %q, but got:\n%s", sub, content)
	}
}

// isValidRunTerminationReason は RunTerminationReason が定義済み 8 値のいずれかかを判定する。
func isValidRunTerminationReason(r RunTerminationReason) bool {
	switch r {
	case RunReasonCompleted,
		RunReasonCancelledByUser,
		RunReasonMissingFeature,
		RunReasonMissingPersona,
		RunReasonPromptFailure,
		RunReasonSchemaViolation,
		RunReasonLoopLimit,
		RunReasonCommitFailure:
		return true
	}
	return false
}
