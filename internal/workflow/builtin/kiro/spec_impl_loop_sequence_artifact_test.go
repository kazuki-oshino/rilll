package kiro

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

func TestSequenceArtifactStoreWritesInitialRunningSuccessFailureAndCompletedCheckpoints(t *testing.T) {
	cwd := t.TempDir()
	runID := "sequence-run-1"
	startedAt := time.Date(2026, 5, 24, 9, 0, 0, 0, time.UTC)
	runningAt := startedAt.Add(time.Minute)
	succeededAt := startedAt.Add(2 * time.Minute)
	failedAt := startedAt.Add(3 * time.Minute)
	completedAt := startedAt.Add(4 * time.Minute)

	store := newSequenceArtifactStore(cwd)
	recorder := &sequenceArtifactRecorder{}
	ctx := sequenceArtifactTestContext(cwd, runID, recorder)

	artifact := NewKiroSpecImplLoopSequenceArtifact(runID, []string{"foundation", "feature-a", "feature-b"}, startedAt)
	if path, err := store.Save(ctx, artifact); err != nil {
		t.Fatalf("Save initial artifact: %v", err)
	} else if path != filepath.Join(cwd, ".rilll", kiroSpecImplLoopSequenceID, runID, "sequence.json") {
		t.Fatalf("Save path = %q, want sequence.json path", path)
	}

	initial := readSequenceArtifact(t, store.Path(runID))
	if initial.SchemaVersion != 1 {
		t.Fatalf("schemaVersion = %d, want 1", initial.SchemaVersion)
	}
	if initial.WorkflowID != kiroSpecImplLoopSequenceID || initial.RunID != runID {
		t.Fatalf("workflow/run IDs = %q/%q, want %q/%q", initial.WorkflowID, initial.RunID, kiroSpecImplLoopSequenceID, runID)
	}
	assertStringSlice(t, "selectedFeatures", initial.SelectedFeatures, []string{"foundation", "feature-a", "feature-b"})
	if initial.Status != SequenceStatusRunning {
		t.Fatalf("initial status = %q, want %q", initial.Status, SequenceStatusRunning)
	}
	if initial.CurrentIndex != nil || initial.CurrentFeature != "" {
		t.Fatalf("initial current = %#v/%q, want unset", initial.CurrentIndex, initial.CurrentFeature)
	}
	if len(initial.Items) != 3 {
		t.Fatalf("initial items len = %d, want 3", len(initial.Items))
	}
	for i, item := range initial.Items {
		if item.Index != i || item.Feature != initial.SelectedFeatures[i] || item.Status != SequenceItemPending {
			t.Fatalf("initial item[%d] = %#v, want pending selected feature", i, item)
		}
	}

	artifact.MarkItemRunning(0, kiroSpecImplLoopID, runningAt)
	if _, err := store.Save(ctx, artifact); err != nil {
		t.Fatalf("Save running artifact: %v", err)
	}
	running := readSequenceArtifact(t, store.Path(runID))
	if running.CurrentIndex == nil || *running.CurrentIndex != 0 || running.CurrentFeature != "foundation" {
		t.Fatalf("running current = %#v/%q, want index 0 foundation", running.CurrentIndex, running.CurrentFeature)
	}
	if running.Items[0].Status != SequenceItemRunning || running.Items[0].ChildWorkflowID != kiroSpecImplLoopID || running.Items[0].StartedAt == nil {
		t.Fatalf("running item[0] = %#v, want running with child workflow and startedAt", running.Items[0])
	}

	artifact.MarkItemSucceeded(0, kiroSpecImplLoopID, "child-run-foundation", succeededAt)
	if _, err := store.Save(ctx, artifact); err != nil {
		t.Fatalf("Save succeeded artifact: %v", err)
	}
	succeeded := readSequenceArtifact(t, store.Path(runID))
	if succeeded.Items[0].Status != SequenceItemSucceeded || succeeded.Items[0].ChildRunID != "child-run-foundation" || succeeded.Items[0].Outcome != "succeeded" || succeeded.Items[0].FinishedAt == nil {
		t.Fatalf("succeeded item[0] = %#v, want succeeded child outcome", succeeded.Items[0])
	}

	artifact.MarkItemRunning(1, kiroSpecImplLoopID, failedAt)
	artifact.MarkItemFailed(1, kiroSpecImplLoopID, "child-run-a", "child_returned_error", "child failed", failedAt)
	if _, err := store.Save(ctx, artifact); err != nil {
		t.Fatalf("Save failed artifact: %v", err)
	}
	failed := readSequenceArtifact(t, store.Path(runID))
	if failed.Status != SequenceStatusFailed || failed.FinishedAt == nil {
		t.Fatalf("failed status/finishedAt = %q/%#v, want failed with finishedAt", failed.Status, failed.FinishedAt)
	}
	if failed.CurrentIndex == nil || *failed.CurrentIndex != 1 || failed.CurrentFeature != "feature-a" {
		t.Fatalf("failed current = %#v/%q, want index 1 feature-a", failed.CurrentIndex, failed.CurrentFeature)
	}
	if failed.Items[1].Status != SequenceItemFailed || failed.Items[1].Outcome != "child_returned_error" || failed.Items[1].ErrorMessage != "child failed" {
		t.Fatalf("failed item[1] = %#v, want failed outcome and error", failed.Items[1])
	}
	if failed.Items[2].Status != SequenceItemUnstarted || !strings.Contains(failed.Items[2].UnstartedReason, "child_returned_error") {
		t.Fatalf("tail item[2] = %#v, want unstarted tail with reason", failed.Items[2])
	}
	if failed.Failure == nil {
		t.Fatal("failure envelope is nil")
	}
	if failed.Failure.Kind != "child_returned_error" || failed.Failure.Message != "child failed" || failed.Failure.Index != 1 || failed.Failure.Feature != "feature-a" {
		t.Fatalf("failure envelope = %#v, want failed feature details", failed.Failure)
	}
	assertStringSlice(t, "unstartedFeatures", failed.Failure.UnstartedFeatures, []string{"feature-b"})

	completed := NewKiroSpecImplLoopSequenceArtifact(runID, []string{"foundation", "feature-a"}, startedAt)
	completed.MarkItemRunning(0, kiroSpecImplLoopID, runningAt)
	completed.MarkItemSucceeded(0, kiroSpecImplLoopID, "child-run-foundation", succeededAt)
	completed.MarkItemRunning(1, kiroSpecImplLoopID, failedAt)
	completed.MarkItemSucceeded(1, kiroSpecImplLoopID, "child-run-a", failedAt)
	completed.MarkCompleted(completedAt)
	if _, err := store.Save(ctx, completed); err != nil {
		t.Fatalf("Save completed artifact: %v", err)
	}
	done := readSequenceArtifact(t, store.Path(runID))
	if done.Status != SequenceStatusCompleted || done.CurrentIndex != nil || done.CurrentFeature != "" || done.FinishedAt == nil {
		t.Fatalf("completed status/current/finishedAt = %q/%#v/%q/%#v, want completed with current cleared", done.Status, done.CurrentIndex, done.CurrentFeature, done.FinishedAt)
	}
	if done.Items[0].Status != SequenceItemSucceeded || done.Items[1].Status != SequenceItemSucceeded {
		t.Fatalf("completed items = %#v, want all succeeded", done.Items)
	}

	if _, err := filepath.Glob(filepath.Join(cwd, ".rilll", kiroSpecImplLoopSequenceID, runID, "*.tmp")); err != nil {
		t.Fatalf("Glob tmp files: %v", err)
	} else if tmp, _ := filepath.Glob(filepath.Join(cwd, ".rilll", kiroSpecImplLoopSequenceID, runID, "*.tmp")); len(tmp) != 0 {
		t.Fatalf("tmp files remain after atomic save: %#v", tmp)
	}
	if len(recorder.artifacts) == 0 {
		t.Fatal("RecordRunArtifact was not called")
	}
	last := recorder.artifacts[len(recorder.artifacts)-1]
	wantArtifactPath, err := filepath.EvalSymlinks(store.Path(runID))
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", store.Path(runID), err)
	}
	if last.Kind != akashic.ArtifactKindResult || last.Path != wantArtifactPath || last.WorkflowRunID != runID {
		t.Fatalf("last RunArtifact = %#v, want result artifact for sequence.json", last)
	}
}

func TestSequenceArtifactStoreHardFailsOnFileWriteError(t *testing.T) {
	cwd := t.TempDir()
	runID := "sequence-run-2"
	if err := os.WriteFile(filepath.Join(cwd, ".rilll"), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("seed .rilll blocker: %v", err)
	}
	recorder := &sequenceArtifactRecorder{}
	artifact := NewKiroSpecImplLoopSequenceArtifact(runID, []string{"feature-a"}, time.Now().UTC())

	_, err := newSequenceArtifactStore(cwd).Save(sequenceArtifactTestContext(cwd, runID, recorder), artifact)
	if err == nil {
		t.Fatal("Save returned nil error, want hard fail")
	}
	if !strings.Contains(err.Error(), "sequence.json") && !strings.Contains(err.Error(), ".rilll") {
		t.Fatalf("Save error = %q, want path diagnostic", err.Error())
	}
	if len(recorder.artifacts) != 0 {
		t.Fatalf("RecordRunArtifact calls = %d, want 0 after file write failure", len(recorder.artifacts))
	}
}

func TestSequenceArtifactStoreIgnoresArtifactRegistrationFailure(t *testing.T) {
	cwd := t.TempDir()
	runID := "sequence-run-3"
	recorder := &sequenceArtifactRecorder{artifactErr: errors.New("sqlite down")}
	artifact := NewKiroSpecImplLoopSequenceArtifact(runID, []string{"feature-a"}, time.Now().UTC())

	path, err := newSequenceArtifactStore(cwd).Save(sequenceArtifactTestContext(cwd, runID, recorder), artifact)
	if err != nil {
		t.Fatalf("Save returned error from best-effort registration: %v", err)
	}
	if path == "" {
		t.Fatal("Save path is empty")
	}
	if got := readSequenceArtifact(t, path); got.RunID != runID {
		t.Fatalf("readback runID = %q, want %q", got.RunID, runID)
	}
	if recorder.artifactCalls != 1 {
		t.Fatalf("RecordRunArtifact calls = %d, want 1", recorder.artifactCalls)
	}
}

func readSequenceArtifact(t *testing.T, path string) SequenceArtifact {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var artifact SequenceArtifact
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatalf("Unmarshal sequence artifact: %v\n%s", err, raw)
	}
	return artifact
}

func sequenceArtifactTestContext(cwd, runID string, recorder akashic.Recorder) context.Context {
	ctx := workflowctx.ContextWithRunMetadata(context.Background(), workflowctx.RunMetadata{
		WorkflowRunID: runID,
		WorkflowID:    kiroSpecImplLoopSequenceID,
		WorkflowName:  "Kiro 仕様作成・実装シーケンス",
		CWD:           cwd,
	})
	return workflow.ContextWithRecorder(ctx, recorder)
}

func assertStringSlice(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s len = %d, want %d: %#v", name, len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s[%d] = %q, want %q; full result %#v", name, i, got[i], want[i], got)
		}
	}
}

type sequenceArtifactRecorder struct {
	artifacts     []akashic.RunArtifact
	artifactErr   error
	artifactCalls int
}

func (r *sequenceArtifactRecorder) RecordWorkflowRunStart(context.Context, akashic.WorkflowRun) error {
	return nil
}

func (r *sequenceArtifactRecorder) RecordWorkflowRunFinish(context.Context, akashic.WorkflowRunFinish) error {
	return nil
}

func (r *sequenceArtifactRecorder) RecordAgentCallStart(context.Context, akashic.AgentCall) error {
	return nil
}

func (r *sequenceArtifactRecorder) RecordAgentCallFinish(context.Context, akashic.AgentCallResult) error {
	return nil
}

func (r *sequenceArtifactRecorder) RecordRawEvent(context.Context, akashic.RawEvent) error {
	return nil
}

func (r *sequenceArtifactRecorder) RecordRunArtifact(_ context.Context, artifact akashic.RunArtifact) error {
	r.artifactCalls++
	if r.artifactErr != nil {
		return r.artifactErr
	}
	r.artifacts = append(r.artifacts, artifact)
	return nil
}

func (r *sequenceArtifactRecorder) RecordUserCorrection(context.Context, string) error {
	return nil
}

func (r *sequenceArtifactRecorder) RecordLearning(context.Context, akashic.LearningRow) error {
	return nil
}

func (r *sequenceArtifactRecorder) Close() error { return nil }
