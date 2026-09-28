package kiro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflow"
)

func TestKiroSpecImplLoopSequenceDefinitionContract(t *testing.T) {
	recorder := &workflowRecorder{}
	agent := &fakeAgent{recorder: recorder}

	def := KiroSpecImplLoopSequence(agent)

	if def.ID != kiroSpecImplLoopSequenceID {
		t.Errorf("Definition.ID=%q, want %q", def.ID, kiroSpecImplLoopSequenceID)
	}
	if def.Label != "Kiro 仕様作成・実装シーケンス" {
		t.Errorf("Definition.Label=%q, want %q", def.Label, "Kiro 仕様作成・実装シーケンス")
	}
	if def.Hidden {
		t.Errorf("Definition.Hidden=true, want false")
	}
	if def.Recorder != recorder {
		t.Errorf("Definition.Recorder=%#v, want agent recorder %#v", def.Recorder, recorder)
	}
	if def.Fn == nil {
		t.Fatal("Definition.Fn is nil")
	}
	if len(def.Args) != 1 {
		t.Fatalf("len(Definition.Args)=%d, want 1", len(def.Args))
	}
	featuresArg := def.Args[0]
	if featuresArg.Key != "features" {
		t.Errorf("features Arg Key=%q, want %q", featuresArg.Key, "features")
	}
	if !featuresArg.Required {
		t.Errorf("features Arg Required=false, want true")
	}
	if !strings.Contains(featuresArg.Description, "comma-separated") || !strings.Contains(featuresArg.Description, "順序") {
		t.Errorf("features Arg Description=%q, want comma-separated ordered usage", featuresArg.Description)
	}
}

func TestKiroSpecImplLoopSequenceWritesRunningCheckpointBeforeFirstChild(t *testing.T) {
	cwd := t.TempDir()
	runID := "sequence-run-initial"
	writeFeature(t, cwd, "alpha", map[string]string{
		"brief.md": "alpha brief\n",
	})
	writeFeature(t, cwd, "beta", map[string]string{
		"brief.md": "beta brief\n",
	})

	childStarts := 0
	var firstChildArtifact SequenceArtifact
	firstChildArtifactObserved := false
	child := workflow.Definition{
		ID:    kiroSpecImplLoopID,
		Label: kiroSpecImplLoopLabel,
		Fn: func(ctx context.Context, _ string, _ func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			childStarts++
			selected, ok, err := ask.Select(ctx, workflow.SelectParams{
				Key: "feature",
				Items: []workflow.SelectItem{
					{ID: "alpha", Label: "Alpha"},
					{ID: "beta", Label: "Beta"},
				},
			})
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("child feature selection was cancelled")
			}
			if childStarts == 1 {
				if selected != "beta" {
					return fmt.Errorf("first child feature = %q, want beta", selected)
				}
				firstChildArtifact = readSequenceArtifact(t, newSequenceArtifactStore(cwd).Path(runID))
				firstChildArtifactObserved = true
			}
			return nil
		},
	}
	def := buildKiroSpecImplLoopSequence(&fakeAgent{}, child)
	ask := &sequenceOrderedAsk{ids: []string{"beta", "alpha"}, ok: true}

	err := def.Fn(sequenceArtifactTestContext(cwd, runID, nil), cwd, nil, ask)
	if err != nil {
		t.Fatalf("sequence runner returned error: %v", err)
	}
	if childStarts != 2 {
		t.Fatalf("child starts = %d, want 2", childStarts)
	}
	if len(ask.orderedParams) != 1 {
		t.Fatalf("SelectOrdered calls = %d, want 1", len(ask.orderedParams))
	}
	if ask.orderedParams[0].Key != "features" || ask.orderedParams[0].MinItems != 1 || ask.orderedParams[0].DuplicatePolicy != workflow.DuplicatePolicyReject {
		t.Fatalf("SelectOrdered params = %#v, want features/min=1/reject", ask.orderedParams[0])
	}

	if !firstChildArtifactObserved {
		t.Fatal("first child did not observe sequence artifact")
	}
	assertStringSlice(t, "selectedFeatures", firstChildArtifact.SelectedFeatures, []string{"beta", "alpha"})
	if firstChildArtifact.Status != SequenceStatusRunning {
		t.Fatalf("first child artifact status = %q, want %q", firstChildArtifact.Status, SequenceStatusRunning)
	}
	if firstChildArtifact.CurrentIndex == nil || *firstChildArtifact.CurrentIndex != 0 || firstChildArtifact.CurrentFeature != "beta" {
		t.Fatalf("first child artifact current = %#v/%q, want index 0 beta", firstChildArtifact.CurrentIndex, firstChildArtifact.CurrentFeature)
	}
	if len(firstChildArtifact.Items) != 2 {
		t.Fatalf("first child artifact items len = %d, want 2", len(firstChildArtifact.Items))
	}
	if firstChildArtifact.Items[0].Status != SequenceItemRunning || firstChildArtifact.Items[0].Feature != "beta" || firstChildArtifact.Items[0].ChildWorkflowID != kiroSpecImplLoopID {
		t.Fatalf("first child artifact item[0] = %#v, want beta running", firstChildArtifact.Items[0])
	}
	if firstChildArtifact.Items[1].Status != SequenceItemPending || firstChildArtifact.Items[1].Feature != "alpha" {
		t.Fatalf("first child artifact item[1] = %#v, want alpha pending", firstChildArtifact.Items[1])
	}
}

func TestKiroSpecImplLoopSequenceRunsChildrenInSelectedOrderAndCompletesArtifact(t *testing.T) {
	cwd := t.TempDir()
	runID := "sequence-run-success"
	writeFeature(t, cwd, "alpha", map[string]string{
		"brief.md": "alpha brief\n",
	})
	writeFeature(t, cwd, "beta", map[string]string{
		"brief.md": "beta brief\n",
	})
	writeFeature(t, cwd, "gamma", map[string]string{
		"brief.md": "gamma brief\n",
	})

	var childStarts []string
	childRunIDs := map[string]string{}
	inFlight := 0
	maxInFlight := 0
	child := workflow.Definition{
		ID:    kiroSpecImplLoopID,
		Label: kiroSpecImplLoopLabel,
		Fn: func(ctx context.Context, _ string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			if inFlight != 0 {
				return fmt.Errorf("parallel child execution detected: inFlight=%d", inFlight)
			}
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			defer func() {
				inFlight--
			}()

			selected, ok, err := ask.Select(ctx, workflow.SelectParams{
				Key: "feature",
				Items: []workflow.SelectItem{
					{ID: "alpha", Label: "Alpha"},
					{ID: "beta", Label: "Beta"},
					{ID: "gamma", Label: "Gamma"},
				},
			})
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("child feature selection was cancelled")
			}
			childStarts = append(childStarts, selected)
			childRunIDs[selected] = workflow.RunIDFromContext(ctx)
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "child selected " + selected})
			return nil
		},
	}
	def := buildKiroSpecImplLoopSequence(&fakeAgent{}, child)
	var events []workflow.WorkflowEvent
	recorder := &sequenceCheckpointRecorder{}

	err := def.Fn(sequenceArtifactTestContext(cwd, runID, recorder), cwd, func(event workflow.WorkflowEvent) {
		events = append(events, event)
	}, &sequenceOrderedAsk{ids: []string{"beta", "alpha"}, ok: true})
	if err != nil {
		t.Fatalf("sequence runner returned error: %v", err)
	}
	if recorder.readErr != nil {
		t.Fatalf("checkpoint recorder read error: %v", recorder.readErr)
	}
	if maxInFlight != 1 || inFlight != 0 {
		t.Fatalf("child concurrency = max %d current %d, want max 1 and current 0", maxInFlight, inFlight)
	}
	if len(recorder.snapshots) != 6 {
		t.Fatalf("checkpoint snapshot count = %d, want 6 (initial, two running, two succeeded, completed)", len(recorder.snapshots))
	}

	assertStringSlice(t, "childStarts", childStarts, []string{"beta", "alpha"})
	assertSequenceCheckpoint(t, "initial checkpoint", recorder.snapshots[0], SequenceStatusRunning, nil, "", []SequenceItemStatus{
		SequenceItemPending,
		SequenceItemPending,
	})
	assertSequenceCheckpoint(t, "beta running checkpoint", recorder.snapshots[1], SequenceStatusRunning, intPtr(0), "beta", []SequenceItemStatus{
		SequenceItemRunning,
		SequenceItemPending,
	})
	assertSequenceCheckpoint(t, "beta succeeded checkpoint", recorder.snapshots[2], SequenceStatusRunning, intPtr(0), "beta", []SequenceItemStatus{
		SequenceItemSucceeded,
		SequenceItemPending,
	})
	if recorder.snapshots[2].Items[0].ChildRunID != childRunIDs["beta"] || recorder.snapshots[2].Items[0].Outcome != sequenceItemOutcomeSucceeded {
		t.Fatalf("beta succeeded checkpoint item[0] = %#v, want child run %q and succeeded outcome", recorder.snapshots[2].Items[0], childRunIDs["beta"])
	}
	assertSequenceCheckpoint(t, "alpha running checkpoint", recorder.snapshots[3], SequenceStatusRunning, intPtr(1), "alpha", []SequenceItemStatus{
		SequenceItemSucceeded,
		SequenceItemRunning,
	})
	assertSequenceCheckpoint(t, "alpha succeeded checkpoint", recorder.snapshots[4], SequenceStatusRunning, intPtr(1), "alpha", []SequenceItemStatus{
		SequenceItemSucceeded,
		SequenceItemSucceeded,
	})
	if recorder.snapshots[4].Items[1].ChildRunID != childRunIDs["alpha"] || recorder.snapshots[4].Items[1].Outcome != sequenceItemOutcomeSucceeded {
		t.Fatalf("alpha succeeded checkpoint item[1] = %#v, want child run %q and succeeded outcome", recorder.snapshots[4].Items[1], childRunIDs["alpha"])
	}
	assertSequenceCheckpoint(t, "completed checkpoint", recorder.snapshots[5], SequenceStatusCompleted, nil, "", []SequenceItemStatus{
		SequenceItemSucceeded,
		SequenceItemSucceeded,
	})

	artifact := readSequenceArtifact(t, newSequenceArtifactStore(cwd).Path(runID))
	if artifact.Status != SequenceStatusCompleted {
		t.Fatalf("completed artifact status = %q, want %q", artifact.Status, SequenceStatusCompleted)
	}
	if artifact.CurrentIndex != nil || artifact.CurrentFeature != "" {
		t.Fatalf("completed artifact current = %#v/%q, want cleared", artifact.CurrentIndex, artifact.CurrentFeature)
	}
	assertStringSlice(t, "selectedFeatures", artifact.SelectedFeatures, []string{"beta", "alpha"})
	if len(artifact.Items) != 2 {
		t.Fatalf("completed artifact items len = %d, want 2", len(artifact.Items))
	}
	for i, item := range artifact.Items {
		feature := artifact.SelectedFeatures[i]
		if item.Index != i || item.Feature != feature {
			t.Fatalf("completed artifact item[%d] identity = %#v, want feature %q at index %d", i, item, feature, i)
		}
		if item.Status != SequenceItemSucceeded || item.ChildWorkflowID != kiroSpecImplLoopID || item.Outcome != sequenceItemOutcomeSucceeded {
			t.Fatalf("completed artifact item[%d] outcome = %#v, want succeeded child outcome", i, item)
		}
		if item.ChildRunID == "" || item.ChildRunID != childRunIDs[feature] {
			t.Fatalf("completed artifact item[%d] childRunID = %q, want child run %q", i, item.ChildRunID, childRunIDs[feature])
		}
		if item.StartedAt == nil || item.FinishedAt == nil {
			t.Fatalf("completed artifact item[%d] timestamps = %#v/%#v, want both set", i, item.StartedAt, item.FinishedAt)
		}
	}

	assertStringSlice(t, "sequence events", sequenceExecutionEventSummary(events), []string{
		"step:1/2:beta",
		"active:" + kiroSpecImplLoopLabel,
		"text:child selected beta",
		"step:2/2:alpha",
		"active:" + kiroSpecImplLoopLabel,
		"text:child selected alpha",
		"done:kiro-spec-impl-loop-sequence completed: beta, alpha",
	})
}

func TestKiroSpecImplLoopSequenceFailFastOnChildReturnedErrorRecordsFailureAndTail(t *testing.T) {
	cwd := t.TempDir()
	runID := "sequence-run-returned-error"
	seedSequenceFeatures(t, cwd, "alpha", "beta", "gamma")

	var childStarts []string
	var failedRunID string
	child := workflow.Definition{
		ID:    kiroSpecImplLoopID,
		Label: kiroSpecImplLoopLabel,
		Fn: func(ctx context.Context, _ string, _ func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			selected, err := selectSequenceChildFeature(ctx, ask, "alpha", "beta", "gamma")
			if err != nil {
				return err
			}
			childStarts = append(childStarts, selected)
			if selected == "beta" {
				failedRunID = workflow.RunIDFromContext(ctx)
				return errors.New("child failed beta")
			}
			return nil
		},
	}
	def := buildKiroSpecImplLoopSequence(&fakeAgent{}, child)
	var events []workflow.WorkflowEvent

	err := def.Fn(sequenceArtifactTestContext(cwd, runID, nil), cwd, func(event workflow.WorkflowEvent) {
		events = append(events, event)
	}, &sequenceOrderedAsk{ids: []string{"alpha", "beta", "gamma"}, ok: true})
	assertErrorContainsAll(t, err, "child failed beta")
	assertWorkflowErrorEventContainsAll(t, events, "child failed beta")
	assertStringSlice(t, "childStarts", childStarts, []string{"alpha", "beta"})

	artifact := readSequenceArtifact(t, newSequenceArtifactStore(cwd).Path(runID))
	assertSequenceFailure(t, artifact, "child_returned_error", "child failed beta", 1, "beta", []string{"gamma"})
	if artifact.Items[0].Status != SequenceItemSucceeded {
		t.Fatalf("item[0] status = %q, want succeeded", artifact.Items[0].Status)
	}
	if artifact.Items[1].Status != SequenceItemFailed || artifact.Items[1].ChildRunID != failedRunID {
		t.Fatalf("failed item = %#v, want child run ID %q", artifact.Items[1], failedRunID)
	}
	if artifact.Items[2].Status != SequenceItemUnstarted {
		t.Fatalf("tail item = %#v, want unstarted", artifact.Items[2])
	}
}

func TestKiroSpecImplLoopSequenceFailFastOnChildWorkflowErrorRecordsFailureAndTail(t *testing.T) {
	cwd := t.TempDir()
	runID := "sequence-run-workflow-error"
	seedSequenceFeatures(t, cwd, "alpha", "beta", "gamma")

	var childStarts []string
	var failedRunID string
	child := workflow.Definition{
		ID:    kiroSpecImplLoopID,
		Label: kiroSpecImplLoopLabel,
		Fn: func(ctx context.Context, _ string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			selected, err := selectSequenceChildFeature(ctx, ask, "alpha", "beta", "gamma")
			if err != nil {
				return err
			}
			childStarts = append(childStarts, selected)
			if selected == "beta" {
				failedRunID = workflow.RunIDFromContext(ctx)
				onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: "workflow failed beta"})
				onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: "later workflow failure"})
			}
			return nil
		},
	}
	def := buildKiroSpecImplLoopSequence(&fakeAgent{}, child)
	var events []workflow.WorkflowEvent

	err := def.Fn(sequenceArtifactTestContext(cwd, runID, nil), cwd, func(event workflow.WorkflowEvent) {
		events = append(events, event)
	}, &sequenceOrderedAsk{ids: []string{"alpha", "beta", "gamma"}, ok: true})
	assertErrorContainsAll(t, err, "workflow failed beta")
	assertWorkflowErrorEventContainsAll(t, events, "workflow failed beta")
	assertStringSlice(t, "childStarts", childStarts, []string{"alpha", "beta"})

	artifact := readSequenceArtifact(t, newSequenceArtifactStore(cwd).Path(runID))
	assertSequenceFailure(t, artifact, "child_workflow_error", "workflow failed beta", 1, "beta", []string{"gamma"})
	if artifact.Items[1].ChildRunID != failedRunID {
		t.Fatalf("failed item childRunID = %q, want %q", artifact.Items[1].ChildRunID, failedRunID)
	}
	if artifact.Items[2].Status != SequenceItemUnstarted {
		t.Fatalf("tail item = %#v, want unstarted", artifact.Items[2])
	}
}

func TestKiroSpecImplLoopSequenceFailFastOnDeliveryFailureRecordsFailureAndTail(t *testing.T) {
	cwd := t.TempDir()
	runID := "sequence-run-delivery-failure"
	seedSequenceFeatures(t, cwd, "alpha", "beta", "gamma")

	var childStarts []string
	child := workflow.Definition{
		ID:    kiroSpecImplLoopID,
		Label: kiroSpecImplLoopLabel,
		Fn: func(ctx context.Context, cwd string, _ func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			selected, err := selectSequenceChildFeature(ctx, ask, "alpha", "beta", "gamma")
			if err != nil {
				return err
			}
			childStarts = append(childStarts, selected)
			if selected == "alpha" {
				if err := os.RemoveAll(filepath.Join(cwd, ".kiro", "specs", "beta")); err != nil {
					return fmt.Errorf("remove beta fixture: %w", err)
				}
			}
			return nil
		},
	}
	def := buildKiroSpecImplLoopSequence(&fakeAgent{}, child)
	var events []workflow.WorkflowEvent

	err := def.Fn(sequenceArtifactTestContext(cwd, runID, nil), cwd, func(event workflow.WorkflowEvent) {
		events = append(events, event)
	}, &sequenceOrderedAsk{ids: []string{"alpha", "beta", "gamma"}, ok: true})
	assertErrorContainsAll(t, err, `feature="beta"`, "delivery failure", "no fallback")
	assertWorkflowErrorEventContainsAll(t, events, `feature="beta"`, "delivery failure", "no fallback")
	assertStringSlice(t, "childStarts", childStarts, []string{"alpha"})

	artifact := readSequenceArtifact(t, newSequenceArtifactStore(cwd).Path(runID))
	assertSequenceFailure(t, artifact, "delivery_failed", err.Error(), 1, "beta", []string{"gamma"})
	if artifact.Items[1].ChildRunID != "" {
		t.Fatalf("delivery failed item childRunID = %q, want empty because child was not started", artifact.Items[1].ChildRunID)
	}
	if artifact.Items[2].Status != SequenceItemUnstarted {
		t.Fatalf("tail item = %#v, want unstarted", artifact.Items[2])
	}
}

func TestKiroSpecImplLoopSequenceFailureCheckpointWriteFailureHardFailsBeforeTail(t *testing.T) {
	cwd := t.TempDir()
	runID := "sequence-run-failure-checkpoint-error"
	seedSequenceFeatures(t, cwd, "alpha", "beta")

	var childStarts []string
	child := workflow.Definition{
		ID:    kiroSpecImplLoopID,
		Label: kiroSpecImplLoopLabel,
		Fn: func(ctx context.Context, cwd string, _ func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			selected, err := selectSequenceChildFeature(ctx, ask, "alpha", "beta")
			if err != nil {
				return err
			}
			childStarts = append(childStarts, selected)
			if selected == "alpha" {
				runDir := filepath.Dir(newSequenceArtifactStore(cwd).Path(runID))
				if err := os.RemoveAll(runDir); err != nil {
					return fmt.Errorf("remove run dir: %w", err)
				}
				if err := os.WriteFile(runDir, []byte("not a directory"), 0o644); err != nil {
					return fmt.Errorf("seed run dir blocker: %w", err)
				}
				return errors.New("child failed before failure checkpoint")
			}
			return nil
		},
	}
	def := buildKiroSpecImplLoopSequence(&fakeAgent{}, child)
	var events []workflow.WorkflowEvent

	err := def.Fn(sequenceArtifactTestContext(cwd, runID, nil), cwd, func(event workflow.WorkflowEvent) {
		events = append(events, event)
	}, &sequenceOrderedAsk{ids: []string{"alpha", "beta"}, ok: true})
	assertErrorContainsAll(t, err, "write sequence.json")
	assertWorkflowErrorEventContainsAll(t, events, "write sequence.json")
	assertStringSlice(t, "childStarts", childStarts, []string{"alpha"})
}

func TestKiroSpecImplLoopSequenceRejectsInvalidOrderedSelectionBeforeChildAndArtifact(t *testing.T) {
	tests := []struct {
		name string
		ids  []string
		want []string
	}{
		{
			name: "empty list",
			ids:  []string{},
			want: []string{"--features", "at least 1", "alpha, beta"},
		},
		{
			name: "unknown feature",
			ids:  []string{"gamma"},
			want: []string{"--features", "unknown feature", "gamma", "alpha, beta"},
		},
		{
			name: "duplicate feature",
			ids:  []string{"alpha", "beta", "alpha"},
			want: []string{"--features", "duplicate feature", "alpha", "alpha, beta"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cwd := t.TempDir()
			runID := "sequence-invalid-" + strings.ReplaceAll(tt.name, " ", "-")
			writeFeature(t, cwd, "alpha", map[string]string{
				"brief.md": "alpha brief\n",
			})
			writeFeature(t, cwd, "beta", map[string]string{
				"brief.md": "beta brief\n",
			})

			childStarts := 0
			child := workflow.Definition{
				ID:    kiroSpecImplLoopID,
				Label: kiroSpecImplLoopLabel,
				Fn: func(context.Context, string, func(workflow.WorkflowEvent), workflow.WorkflowAsk) error {
					childStarts++
					return nil
				},
			}
			def := buildKiroSpecImplLoopSequence(&fakeAgent{}, child)
			var events []workflow.WorkflowEvent

			err := def.Fn(sequenceArtifactTestContext(cwd, runID, nil), cwd, func(event workflow.WorkflowEvent) {
				events = append(events, event)
			}, &sequenceOrderedAsk{ids: tt.ids, ok: true})
			assertErrorContainsAll(t, err, tt.want...)
			assertWorkflowErrorEventContainsAll(t, events, tt.want...)
			if childStarts != 0 {
				t.Fatalf("child starts = %d, want 0 after invalid ordered selection", childStarts)
			}
			assertSequenceArtifactAbsent(t, newSequenceArtifactStore(cwd).Path(runID))
		})
	}
}

func TestKiroSpecImplLoopSequenceInitialArtifactWriteFailureStopsBeforeChild(t *testing.T) {
	cwd := t.TempDir()
	runID := "sequence-initial-write-failure"
	writeFeature(t, cwd, "alpha", map[string]string{
		"brief.md": "alpha brief\n",
	})
	if err := os.WriteFile(filepath.Join(cwd, ".rilll"), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("seed .rilll blocker: %v", err)
	}

	childStarts := 0
	child := workflow.Definition{
		ID:    kiroSpecImplLoopID,
		Label: kiroSpecImplLoopLabel,
		Fn: func(context.Context, string, func(workflow.WorkflowEvent), workflow.WorkflowAsk) error {
			childStarts++
			return nil
		},
	}
	def := buildKiroSpecImplLoopSequence(&fakeAgent{}, child)
	var events []workflow.WorkflowEvent

	err := def.Fn(sequenceArtifactTestContext(cwd, runID, nil), cwd, func(event workflow.WorkflowEvent) {
		events = append(events, event)
	}, &sequenceOrderedAsk{ids: []string{"alpha"}, ok: true})
	assertErrorContainsAll(t, err, "write sequence.json", ".rilll")
	assertWorkflowErrorEventContainsAll(t, events, "write sequence.json", ".rilll")
	if childStarts != 0 {
		t.Fatalf("child starts = %d, want 0 after initial artifact write failure", childStarts)
	}
}

func TestSequenceFeatureAskReturnsCurrentFeatureWithoutFallback(t *testing.T) {
	delegate := &recordingSpecImplLoopAsk{selected: "other-feature", ok: true}
	ask := sequenceFeatureAsk{feature: "current-feature", delegate: delegate}

	selected, ok, err := ask.Select(context.Background(), workflow.SelectParams{
		Key: "feature",
		Items: []workflow.SelectItem{
			{ID: "other-feature", Label: "Other"},
			{ID: "current-feature", Label: "Current"},
		},
	})
	if err != nil {
		t.Fatalf("sequenceFeatureAsk.Select feature error = %v", err)
	}
	if !ok || selected != "current-feature" {
		t.Fatalf("sequenceFeatureAsk.Select feature = (%q, %v), want current-feature/true", selected, ok)
	}
	if len(delegate.calls) != 0 {
		t.Fatalf("delegate Select calls = %d, want 0 for fixed feature selection", len(delegate.calls))
	}
}

func TestSequenceFeatureAskDetectsDriftWhenFeatureMissingFromItems(t *testing.T) {
	delegate := &recordingSpecImplLoopAsk{selected: "other-feature", ok: true}
	ask := sequenceFeatureAsk{feature: "current-feature", delegate: delegate}

	selected, ok, err := ask.Select(context.Background(), workflow.SelectParams{
		Key: "feature",
		Items: []workflow.SelectItem{
			{ID: "other-feature", Label: "Other"},
		},
	})
	if err == nil {
		t.Fatal("sequenceFeatureAsk.Select drift error = nil, want error")
	}
	if selected != "" || ok {
		t.Fatalf("sequenceFeatureAsk.Select drift = (%q, %v), want empty/false", selected, ok)
	}
	assertErrorContainsAll(t, err, `feature="current-feature"`, "delivery failure", "not present", "no fallback")
	if len(delegate.calls) != 0 {
		t.Fatalf("delegate Select calls = %d, want 0 for feature drift", len(delegate.calls))
	}
}

func TestSequenceFeatureAskDelegatesNonFeatureSelection(t *testing.T) {
	delegate := &recordingSpecImplLoopAsk{selected: "delegate-choice", ok: true}
	ask := sequenceFeatureAsk{feature: "current-feature", delegate: delegate}

	selected, ok, err := ask.Select(context.Background(), workflow.SelectParams{
		Key: "review-action",
		Items: []workflow.SelectItem{
			{ID: "delegate-choice", Label: "Delegate"},
		},
	})
	if err != nil {
		t.Fatalf("sequenceFeatureAsk.Select delegated error = %v", err)
	}
	if !ok || selected != "delegate-choice" {
		t.Fatalf("sequenceFeatureAsk.Select delegated = (%q, %v), want delegate-choice/true", selected, ok)
	}
	if len(delegate.calls) != 1 {
		t.Fatalf("delegate Select calls = %d, want 1", len(delegate.calls))
	}
	if delegate.calls[0].Key != "review-action" {
		t.Fatalf("delegate Select key = %q, want review-action", delegate.calls[0].Key)
	}
}

func TestSequenceFeatureAskRejectsNonFeatureSelectionWithoutDelegate(t *testing.T) {
	ask := sequenceFeatureAsk{feature: "current-feature"}

	_, _, err := ask.Select(context.Background(), workflow.SelectParams{Key: "review-action"})
	if err == nil {
		t.Fatal("sequenceFeatureAsk.Select non-feature without delegate error = nil, want error")
	}
	assertErrorContainsAll(t, err, "cannot delegate", "review-action")
}

func TestPrepareSequenceFeatureDeliveryPrechecksEligibleFeatureAndReturnsFixedAsk(t *testing.T) {
	cwd := t.TempDir()
	currentFeature := "current-feature"
	otherFeature := "other-feature"
	writeFeature(t, cwd, currentFeature, map[string]string{
		"brief.md": "current brief\n",
	})
	writeFeature(t, cwd, otherFeature, map[string]string{
		"brief.md": "other brief\n",
	})

	delegate := &recordingSpecImplLoopAsk{selected: otherFeature, ok: true}
	gotCtx, gotAsk, err := prepareSequenceFeatureDelivery(context.Background(), cwd, delegate, currentFeature)
	if err != nil {
		t.Fatalf("prepareSequenceFeatureDelivery error = %v", err)
	}
	if gotCtx == nil {
		t.Fatal("prepareSequenceFeatureDelivery returned nil context")
	}

	selected, ok, err := gotAsk.Select(gotCtx, workflow.SelectParams{
		Key: "feature",
		Items: []workflow.SelectItem{
			{ID: otherFeature, Label: "Other"},
			{ID: currentFeature, Label: "Current"},
		},
	})
	if err != nil {
		t.Fatalf("delivered ask feature Select error = %v", err)
	}
	if !ok || selected != currentFeature {
		t.Fatalf("delivered ask feature Select = (%q, %v), want %q/true", selected, ok, currentFeature)
	}
	if len(delegate.calls) != 0 {
		t.Fatalf("delegate Select calls = %d, want 0 for fixed sequence feature", len(delegate.calls))
	}
}

func TestKiroSpecImplLoopSequenceSelectsSpecEligibleAndImplementableTargets(t *testing.T) {
	cwd := t.TempDir()
	runID := "sequence-union-targets"
	specFeature := "needs-spec"
	implFeature := "ready-impl"
	writeFeature(t, cwd, specFeature, map[string]string{
		"brief.md": "needs spec brief\n",
	})
	writeFeature(t, cwd, implFeature, map[string]string{
		"spec.json": validImplSpecJSON,
		"tasks.md":  "- [ ] 1. pending\n",
	})

	var childStarts []string
	child := workflow.Definition{
		ID:    kiroSpecImplLoopID,
		Label: kiroSpecImplLoopLabel,
		Fn: func(ctx context.Context, _ string, _ func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			selected, ok, err := ask.Select(ctx, workflow.SelectParams{
				Key: "feature",
				Items: []workflow.SelectItem{
					{ID: specFeature, Label: specFeature},
					{ID: implFeature, Label: implFeature},
				},
			})
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("child feature selection was cancelled")
			}
			childStarts = append(childStarts, selected)
			return nil
		},
	}
	def := buildKiroSpecImplLoopSequence(&fakeAgent{}, child)
	ask := &sequenceOrderedAsk{ids: []string{implFeature, specFeature}, ok: true}

	err := def.Fn(sequenceArtifactTestContext(cwd, runID, nil), cwd, nil, ask)
	if err != nil {
		t.Fatalf("sequence runner returned error: %v", err)
	}
	if len(ask.orderedParams) != 1 {
		t.Fatalf("SelectOrdered calls = %d, want 1", len(ask.orderedParams))
	}
	var itemIDs []string
	for _, item := range ask.orderedParams[0].Items {
		itemIDs = append(itemIDs, item.ID)
	}
	assertStringSlice(t, "ordered selection item IDs", itemIDs, []string{specFeature, implFeature})
	assertStringSlice(t, "childStarts", childStarts, []string{implFeature, specFeature})
}

func TestRunSequenceChildWithFixedFeatureStopsBeforeChildWhenDeliveryPrecheckFails(t *testing.T) {
	cwd := t.TempDir()
	writeFeature(t, cwd, "other-feature", map[string]string{
		"brief.md": "other brief\n",
	})

	childStarts := 0
	child := workflow.Definition{
		ID:    kiroSpecImplLoopID,
		Label: kiroSpecImplLoopLabel,
		Fn: func(context.Context, string, func(workflow.WorkflowEvent), workflow.WorkflowAsk) error {
			childStarts++
			return errors.New("child must not start after delivery failure")
		},
	}
	delegate := &recordingSpecImplLoopAsk{selected: "other-feature", ok: true}
	var events []workflow.WorkflowEvent

	result, err := runSequenceChildWithFixedFeature(context.Background(), cwd, child, "missing-feature", func(event workflow.WorkflowEvent) {
		events = append(events, event)
	}, delegate)
	if err == nil {
		t.Fatal("runSequenceChildWithFixedFeature error = nil, want delivery failure")
	}
	assertErrorContainsAll(t, err, `feature="missing-feature"`, "delivery failure", "available: other-feature", "no fallback")
	if result.RunID != "" {
		t.Fatalf("child result RunID = %q, want empty when child is not started", result.RunID)
	}
	if childStarts != 0 {
		t.Fatalf("child starts = %d, want 0", childStarts)
	}
	if len(delegate.calls) != 0 {
		t.Fatalf("delegate Select calls = %d, want 0 before child start", len(delegate.calls))
	}
	if len(events) != 0 {
		t.Fatalf("events = %+v, want none before child start", events)
	}
}

func TestRunSequenceChildWithFixedFeatureStartsChildWithCurrentFeature(t *testing.T) {
	cwd := t.TempDir()
	currentFeature := "current-feature"
	writeFeature(t, cwd, currentFeature, map[string]string{
		"brief.md": "current brief\n",
	})
	writeFeature(t, cwd, "other-feature", map[string]string{
		"brief.md": "other brief\n",
	})

	childStarts := 0
	child := workflow.Definition{
		ID:    kiroSpecImplLoopID,
		Label: kiroSpecImplLoopLabel,
		Fn: func(ctx context.Context, _ string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			childStarts++
			selected, ok, err := ask.Select(ctx, workflow.SelectParams{
				Key: "feature",
				Items: []workflow.SelectItem{
					{ID: "other-feature", Label: "Other"},
					{ID: currentFeature, Label: "Current"},
				},
			})
			if err != nil {
				return err
			}
			if !ok || selected != currentFeature {
				return errors.New("child did not receive current feature")
			}
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "selected:" + selected})
			return nil
		},
	}
	delegate := &recordingSpecImplLoopAsk{selected: "other-feature", ok: true}
	var events []workflow.WorkflowEvent

	result, err := runSequenceChildWithFixedFeature(context.Background(), cwd, child, currentFeature, func(event workflow.WorkflowEvent) {
		events = append(events, event)
	}, delegate)
	if err != nil {
		t.Fatalf("runSequenceChildWithFixedFeature error = %v", err)
	}
	if result.DefinitionID != kiroSpecImplLoopID || result.RunID == "" {
		t.Fatalf("child result = %#v, want started %s child with runID", result, kiroSpecImplLoopID)
	}
	if childStarts != 1 {
		t.Fatalf("child starts = %d, want 1", childStarts)
	}
	if len(delegate.calls) != 0 {
		t.Fatalf("delegate Select calls = %d, want 0 for child feature selection", len(delegate.calls))
	}
	summary := workflowEventSummary(events)
	if !hasString(summary, "active:"+kiroSpecImplLoopLabel) || !hasString(summary, "text:selected:"+currentFeature) {
		t.Fatalf("events summary = %#v, want active child and selected feature text", summary)
	}
}

type sequenceOrderedAsk struct {
	ids           []string
	ok            bool
	err           error
	selectCalls   int
	orderedParams []workflow.OrderedSelectParams
}

func (a *sequenceOrderedAsk) Select(context.Context, workflow.SelectParams) (string, bool, error) {
	a.selectCalls++
	return "", false, errors.New("single Select must not be called for sequence feature selection")
}

func (a *sequenceOrderedAsk) SelectOrdered(_ context.Context, params workflow.OrderedSelectParams) ([]string, bool, error) {
	a.orderedParams = append(a.orderedParams, params)
	return a.ids, a.ok, a.err
}

type sequenceCheckpointRecorder struct {
	workflowRecorder
	snapshots []SequenceArtifact
	readErr   error
}

func (r *sequenceCheckpointRecorder) RecordRunArtifact(ctx context.Context, artifact akashic.RunArtifact) error {
	if err := r.workflowRecorder.RecordRunArtifact(ctx, artifact); err != nil {
		return err
	}
	if r.readErr != nil {
		return nil
	}
	raw, err := os.ReadFile(artifact.Path)
	if err != nil {
		r.readErr = fmt.Errorf("read checkpoint artifact %s: %w", artifact.Path, err)
		return nil
	}
	var snapshot SequenceArtifact
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		r.readErr = fmt.Errorf("unmarshal checkpoint artifact %s: %w", artifact.Path, err)
		return nil
	}
	r.snapshots = append(r.snapshots, snapshot)
	return nil
}

func assertSequenceArtifactAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("sequence artifact %s exists, want absent before initial save", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(%s) error = %v, want not exist", path, err)
	}
}

func sequenceExecutionEventSummary(events []workflow.WorkflowEvent) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		switch event.Type {
		case workflow.WorkflowStep:
			out = append(out, fmt.Sprintf("step:%d/%d:%s", event.Step, event.TotalSteps, event.Label))
		case workflow.WorkflowActive:
			out = append(out, "active:"+event.Label)
		case workflow.WorkflowText:
			out = append(out, "text:"+event.Text)
		case workflow.WorkflowDone:
			out = append(out, "done:"+event.Result)
		case workflow.WorkflowError:
			out = append(out, "error:"+event.Message)
		}
	}
	return out
}

func hasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func assertSequenceCheckpoint(t *testing.T, name string, artifact SequenceArtifact, status SequenceStatus, currentIndex *int, currentFeature string, itemStatuses []SequenceItemStatus) {
	t.Helper()
	if artifact.Status != status {
		t.Fatalf("%s status = %q, want %q", name, artifact.Status, status)
	}
	if currentIndex == nil {
		if artifact.CurrentIndex != nil {
			t.Fatalf("%s currentIndex = %#v, want nil", name, artifact.CurrentIndex)
		}
	} else if artifact.CurrentIndex == nil || *artifact.CurrentIndex != *currentIndex {
		t.Fatalf("%s currentIndex = %#v, want %d", name, artifact.CurrentIndex, *currentIndex)
	}
	if artifact.CurrentFeature != currentFeature {
		t.Fatalf("%s currentFeature = %q, want %q", name, artifact.CurrentFeature, currentFeature)
	}
	assertStringSlice(t, name+" selectedFeatures", artifact.SelectedFeatures, []string{"beta", "alpha"})
	if len(artifact.Items) != len(itemStatuses) {
		t.Fatalf("%s item len = %d, want %d", name, len(artifact.Items), len(itemStatuses))
	}
	for i, wantStatus := range itemStatuses {
		item := artifact.Items[i]
		if item.Index != i || item.Feature != artifact.SelectedFeatures[i] || item.Status != wantStatus {
			t.Fatalf("%s item[%d] = %#v, want index=%d feature=%q status=%q", name, i, item, i, artifact.SelectedFeatures[i], wantStatus)
		}
	}
}

func seedSequenceFeatures(t *testing.T, cwd string, features ...string) {
	t.Helper()
	for _, feature := range features {
		writeFeature(t, cwd, feature, map[string]string{
			"brief.md": feature + " brief\n",
		})
	}
}

func selectSequenceChildFeature(ctx context.Context, ask workflow.WorkflowAsk, features ...string) (string, error) {
	items := make([]workflow.SelectItem, 0, len(features))
	for _, feature := range features {
		items = append(items, workflow.SelectItem{ID: feature, Label: feature})
	}
	selected, ok, err := ask.Select(ctx, workflow.SelectParams{
		Key:   "feature",
		Items: items,
	})
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("child feature selection was cancelled")
	}
	return selected, nil
}

func assertSequenceFailure(t *testing.T, artifact SequenceArtifact, kind, message string, index int, feature string, unstarted []string) {
	t.Helper()
	if artifact.Status != SequenceStatusFailed {
		t.Fatalf("artifact status = %q, want failed", artifact.Status)
	}
	if artifact.CurrentIndex == nil || *artifact.CurrentIndex != index || artifact.CurrentFeature != feature {
		t.Fatalf("artifact current = %#v/%q, want index %d feature %q", artifact.CurrentIndex, artifact.CurrentFeature, index, feature)
	}
	if artifact.Failure == nil {
		t.Fatal("failure envelope is nil")
	}
	if artifact.Failure.Kind != kind || artifact.Failure.Message != message || artifact.Failure.Index != index || artifact.Failure.Feature != feature {
		t.Fatalf("failure envelope = %#v, want kind=%q message=%q index=%d feature=%q", artifact.Failure, kind, message, index, feature)
	}
	assertStringSlice(t, "unstartedFeatures", artifact.Failure.UnstartedFeatures, unstarted)
	item := artifact.Items[index]
	if item.Status != SequenceItemFailed || item.Outcome != kind || item.ErrorMessage != message {
		t.Fatalf("failed item = %#v, want status failed outcome %q message %q", item, kind, message)
	}
	for _, i := range unstarted {
		found := false
		for _, item := range artifact.Items[index+1:] {
			if item.Feature == i {
				found = true
				if item.Status != SequenceItemUnstarted || !strings.Contains(item.UnstartedReason, kind) {
					t.Fatalf("unstarted item = %#v, want unstarted with reason %q", item, kind)
				}
			}
		}
		if !found {
			t.Fatalf("unstarted feature %q not found in tail %#v", i, artifact.Items[index+1:])
		}
	}
}
