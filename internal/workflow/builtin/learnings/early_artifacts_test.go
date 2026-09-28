package learnings

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

func TestWriteEarlyReflectionArtifactWritesSummaryTargetsAndRegistersContextArtifact(t *testing.T) {
	cwd := t.TempDir()
	runID := "tail-run-1"
	runDir := filepath.Join(cwd, ".rilll", learningsWorkflowID, runID)
	recorder := &earlyArtifactRecorder{}
	ctx := workflow.ContextWithRecorder(context.Background(), recorder)
	ctx = workflowctx.ContextWithRunMetadata(ctx, workflowctx.RunMetadata{
		WorkflowRunID: runID,
		WorkflowID:    learningsWorkflowID,
		WorkflowName:  learningsWorkflowLabel,
		CWD:           cwd,
	})

	artifact := BuildEarlyReflectionArtifact("parent-run-1", runID, "bounded-wait-timeout", []EarlyReflectionTargetSummary{
		{
			AgentCallID:       "call-succeeded",
			WorkflowRunID:     "parent-run-1",
			SessionID:         "session-succeeded",
			ProviderID:        "codex",
			StepID:            "step-a",
			Status:            EarlyReflectionTargetStatusSucceeded,
			Reason:            "",
			IncludedInCurator: true,
		},
		{
			AgentCallID:       "call-wait-completed",
			WorkflowRunID:     "child-run-1",
			SessionID:         "session-wait-completed",
			ProviderID:        "claude",
			StepID:            "step-b",
			Status:            EarlyReflectionTargetStatusWaitCompleted,
			Reason:            "",
			IncludedInCurator: true,
		},
		{
			AgentCallID:       "call-skipped",
			WorkflowRunID:     "parent-run-1",
			SessionID:         "",
			ProviderID:        "codex",
			StepID:            "step-c",
			Status:            EarlyReflectionTargetStatusSkipped,
			Reason:            string(akashic.LearningReflectionSkipEmptySessionID),
			IncludedInCurator: false,
		},
		{
			AgentCallID:       "call-failed",
			WorkflowRunID:     "parent-run-1",
			SessionID:         "session-failed",
			ProviderID:        "codex",
			StepID:            "step-d",
			Status:            EarlyReflectionTargetStatusFailed,
			Reason:            failureReasonNoCredentials,
			IncludedInCurator: false,
		},
		{
			AgentCallID:       "call-stale",
			WorkflowRunID:     "child-run-1",
			SessionID:         "session-stale",
			ProviderID:        "codex",
			StepID:            "step-e",
			Status:            EarlyReflectionTargetStatusStale,
			Reason:            "tail-timeout",
			IncludedInCurator: false,
		},
		{
			AgentCallID:       "call-missing",
			WorkflowRunID:     "child-run-2",
			SessionID:         "session-missing",
			ProviderID:        "codex",
			StepID:            "step-f",
			Status:            EarlyReflectionTargetStatusMissingJob,
			Reason:            "job-row-absent",
			IncludedInCurator: false,
		},
		{
			AgentCallID:       "call-pending-timeout",
			WorkflowRunID:     "child-run-2",
			SessionID:         "session-pending-timeout",
			ProviderID:        "codex",
			StepID:            "step-g",
			Status:            EarlyReflectionTargetStatusPendingAtTimeout,
			Reason:            "wait-budget-exhausted",
			IncludedInCurator: false,
		},
	})

	path, err := WriteEarlyReflectionArtifact(ctx, runDir, artifact)
	if err != nil {
		t.Fatalf("WriteEarlyReflectionArtifact: %v", err)
	}
	if got, want := filepath.Base(path), earlyReflectionArtifactFilename; got != want {
		t.Fatalf("artifact filename=%q, want %q", got, want)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read early-reflection.json: %v", err)
	}
	var decoded EarlyReflectionArtifact
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal early-reflection.json: %v", err)
	}
	wantSummary := EarlyReflectionSummary{
		TotalTargets:     7,
		Included:         2,
		WaitCompleted:    1,
		Skipped:          1,
		Failed:           1,
		Stale:            1,
		MissingJob:       1,
		PendingAtTimeout: 1,
		FallbackReason:   "bounded-wait-timeout",
	}
	if decoded.Summary != wantSummary {
		t.Fatalf("summary=%+v, want %+v", decoded.Summary, wantSummary)
	}
	if len(decoded.Targets) != 7 {
		t.Fatalf("targets count=%d, want 7", len(decoded.Targets))
	}
	if !decoded.Targets[0].IncludedInCurator {
		t.Fatalf("targets[0].included_in_curator=false, want true")
	}
	if decoded.Targets[5].Status != EarlyReflectionTargetStatusMissingJob || decoded.Targets[5].Reason != "job-row-absent" {
		t.Fatalf("missing-job target=%+v", decoded.Targets[5])
	}
	if decoded.Targets[6].Status != EarlyReflectionTargetStatusPendingAtTimeout {
		t.Fatalf("pending-at-timeout target status=%q", decoded.Targets[6].Status)
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal raw early-reflection.json: %v", err)
	}
	if _, ok := raw["verdict"]; ok {
		t.Fatalf("early-reflection.json unexpectedly contains apply-result verdict field")
	}
	if _, err := os.Stat(filepath.Join(runDir, applyResultFilename)); err == nil {
		t.Fatalf("WriteEarlyReflectionArtifact should not create apply-result.json")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat apply-result.json: %v", err)
	}

	if len(recorder.artifacts) != 1 {
		t.Fatalf("registered artifacts=%d, want 1: %+v", len(recorder.artifacts), recorder.artifacts)
	}
	gotArtifact := recorder.artifacts[0]
	if gotArtifact.WorkflowRunID != runID || gotArtifact.Kind != akashic.ArtifactKindContext {
		t.Fatalf("registered artifact=(run=%q kind=%q), want (run=%q kind=%q)",
			gotArtifact.WorkflowRunID, gotArtifact.Kind, runID, akashic.ArtifactKindContext)
	}
	expectedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", path, err)
	}
	expectedPath = filepath.Clean(expectedPath)
	if gotArtifact.Path != expectedPath {
		t.Fatalf("registered path=%q, want %q", gotArtifact.Path, expectedPath)
	}
	if gotArtifact.SizeBytes <= 0 || gotArtifact.Mtime.IsZero() {
		t.Fatalf("registered artifact missing size/mtime: %+v", gotArtifact)
	}
}

type earlyArtifactRecorder struct {
	artifacts []akashic.RunArtifact
}

func (r *earlyArtifactRecorder) RecordWorkflowRunStart(context.Context, akashic.WorkflowRun) error {
	return nil
}
func (r *earlyArtifactRecorder) RecordWorkflowRunFinish(context.Context, akashic.WorkflowRunFinish) error {
	return nil
}
func (r *earlyArtifactRecorder) RecordAgentCallStart(context.Context, akashic.AgentCall) error {
	return nil
}
func (r *earlyArtifactRecorder) RecordAgentCallFinish(context.Context, akashic.AgentCallResult) error {
	return nil
}
func (r *earlyArtifactRecorder) RecordRawEvent(context.Context, akashic.RawEvent) error {
	return nil
}
func (r *earlyArtifactRecorder) RecordRunArtifact(_ context.Context, artifact akashic.RunArtifact) error {
	r.artifacts = append(r.artifacts, artifact)
	return nil
}
func (r *earlyArtifactRecorder) RecordUserCorrection(context.Context, string) error {
	return nil
}
func (r *earlyArtifactRecorder) RecordLearning(context.Context, akashic.LearningRow) error {
	return nil
}
func (r *earlyArtifactRecorder) Close() error { return nil }

var _ akashic.Recorder = (*earlyArtifactRecorder)(nil)

func TestBuildEarlyReflectionArtifactInitializesEmptyTargets(t *testing.T) {
	artifact := BuildEarlyReflectionArtifact("parent-run-1", "tail-run-1", "", nil)
	if artifact.SchemaVersion != earlyReflectionArtifactSchemaVersion {
		t.Fatalf("schema_version=%d, want %d", artifact.SchemaVersion, earlyReflectionArtifactSchemaVersion)
	}
	if artifact.ParentRunID != "parent-run-1" || artifact.TailRunID != "tail-run-1" {
		t.Fatalf("run ids=(%q,%q)", artifact.ParentRunID, artifact.TailRunID)
	}
	if artifact.GeneratedAt.IsZero() {
		t.Fatal("generated_at is zero")
	}
	if artifact.Targets == nil {
		t.Fatal("targets is nil, want empty slice for JSON []")
	}
	if artifact.Summary != (EarlyReflectionSummary{}) {
		t.Fatalf("summary=%+v, want zero counts", artifact.Summary)
	}
}

func TestEarlyReflectionArtifactSummaryUsesStatusAndIncludedFlag(t *testing.T) {
	generatedAt := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	artifact := EarlyReflectionArtifact{
		SchemaVersion: earlyReflectionArtifactSchemaVersion,
		ParentRunID:   "parent-run-1",
		TailRunID:     "tail-run-1",
		GeneratedAt:   generatedAt,
		Targets: []EarlyReflectionTargetSummary{
			{Status: EarlyReflectionTargetStatusSucceeded, IncludedInCurator: true},
			{Status: EarlyReflectionTargetStatusSucceeded, IncludedInCurator: false},
			{Status: EarlyReflectionTargetStatusWaitCompleted, IncludedInCurator: true},
			{Status: EarlyReflectionTargetStatusSkipped},
			{Status: EarlyReflectionTargetStatusFailed},
			{Status: EarlyReflectionTargetStatusStale},
			{Status: EarlyReflectionTargetStatusMissingJob},
			{Status: EarlyReflectionTargetStatusPendingAtTimeout},
		},
	}

	artifact.RefreshSummary("fallback-used")
	want := EarlyReflectionSummary{
		TotalTargets:     8,
		Included:         2,
		WaitCompleted:    1,
		Skipped:          1,
		Failed:           1,
		Stale:            1,
		MissingJob:       1,
		PendingAtTimeout: 1,
		FallbackReason:   "fallback-used",
	}
	if artifact.Summary != want {
		t.Fatalf("summary=%+v, want %+v", artifact.Summary, want)
	}
	if !artifact.GeneratedAt.Equal(generatedAt) {
		t.Fatalf("RefreshSummary changed generated_at: %s", artifact.GeneratedAt)
	}
}
