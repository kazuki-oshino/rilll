package workflow

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

func TestRunChildEmitsActiveAndForwardsEventsInOrder(t *testing.T) {
	var sequence []string
	def := Definition{
		ID:    "child-order",
		Label: "Child Order",
		Fn: func(_ context.Context, _ string, onEvent func(WorkflowEvent), _ WorkflowAsk) error {
			sequence = append(sequence, "fn:start")
			onEvent(WorkflowEvent{Type: WorkflowStep, Label: "step-1"})
			onEvent(WorkflowEvent{Type: WorkflowText, Text: "body"})
			return nil
		},
	}

	result, err := RunChild(context.Background(), t.TempDir(), def, func(event WorkflowEvent) {
		if event.Type != WorkflowActive && event.RunID == "" {
			t.Fatalf("forwarded child event %#v has empty RunID", event)
		}
		switch event.Type {
		case WorkflowActive:
			sequence = append(sequence, "event:active:"+event.Label)
		case WorkflowStep:
			sequence = append(sequence, "event:step:"+event.Label)
		case WorkflowText:
			sequence = append(sequence, "event:text:"+event.Text)
		}
	}, nil, ChildRunOptions{})
	if err != nil {
		t.Fatalf("RunChild returned error: %v", err)
	}
	if result.RunID == "" {
		t.Fatalf("result.RunID is empty")
	}
	want := []string{
		"event:active:Child Order",
		"fn:start",
		"event:step:step-1",
		"event:text:body",
	}
	if !reflect.DeepEqual(sequence, want) {
		t.Fatalf("event/function sequence = %v, want %v", sequence, want)
	}
}

func TestRunChildReturnsEventlessRunIDAndRecorderParity(t *testing.T) {
	recorder := &workflowRecorder{}
	def := Definition{
		ID:       "child-eventless",
		Label:    "Child Eventless",
		Recorder: recorder,
		Fn: func(context.Context, string, func(WorkflowEvent), WorkflowAsk) error {
			return nil
		},
	}

	result, err := RunChild(context.Background(), t.TempDir(), def, func(WorkflowEvent) {}, nil, ChildRunOptions{})
	if err != nil {
		t.Fatalf("RunChild returned error: %v", err)
	}
	if result.RunID == "" {
		t.Fatalf("result.RunID is empty")
	}
	if result.DefinitionID != def.ID {
		t.Fatalf("result.DefinitionID = %q, want %q", result.DefinitionID, def.ID)
	}
	if result.Label != def.Label {
		t.Fatalf("result.Label = %q, want %q", result.Label, def.Label)
	}
	if len(recorder.starts) != 1 {
		t.Fatalf("RecordWorkflowRunStart count = %d, want 1", len(recorder.starts))
	}
	if len(recorder.finishes) != 1 {
		t.Fatalf("RecordWorkflowRunFinish count = %d, want 1", len(recorder.finishes))
	}
	if recorder.starts[0].ID != result.RunID {
		t.Fatalf("start.ID = %q, want result.RunID %q", recorder.starts[0].ID, result.RunID)
	}
	if recorder.finishes[0].ID != result.RunID {
		t.Fatalf("finish.ID = %q, want result.RunID %q", recorder.finishes[0].ID, result.RunID)
	}
}

func TestRunChildRecordsParentChildLineage(t *testing.T) {
	recorder := &workflowRecorder{}
	cwd := t.TempDir()
	ctx := workflowctx.ContextWithRunMetadata(context.Background(), workflowctx.RunMetadata{
		WorkflowRunID: "parent-run",
		WorkflowID:    "parent-workflow",
		WorkflowName:  "Parent Workflow",
		CWD:           cwd,
	})
	ctx = ContextWithRecorder(ctx, recorder)
	def := Definition{
		ID:       "child-workflow",
		Label:    "Child Workflow",
		Recorder: recorder,
		Fn: func(context.Context, string, func(WorkflowEvent), WorkflowAsk) error {
			return nil
		},
	}

	result, err := RunChild(ctx, cwd, def, func(WorkflowEvent) {}, nil, ChildRunOptions{})
	if err != nil {
		t.Fatalf("RunChild returned error: %v", err)
	}

	if len(recorder.childRuns) != 1 {
		t.Fatalf("RecordWorkflowRunChild count = %d, want 1", len(recorder.childRuns))
	}
	got := recorder.childRuns[0]
	if got.ParentRunID != "parent-run" {
		t.Fatalf("ParentRunID = %q, want parent-run", got.ParentRunID)
	}
	if got.ChildRunID != result.RunID {
		t.Fatalf("ChildRunID = %q, want result.RunID %q", got.ChildRunID, result.RunID)
	}
	if got.ChildWorkflowID != def.ID {
		t.Fatalf("ChildWorkflowID = %q, want %q", got.ChildWorkflowID, def.ID)
	}
	if got.ChildWorkflowName != def.Label {
		t.Fatalf("ChildWorkflowName = %q, want %q", got.ChildWorkflowName, def.Label)
	}
	if got.RecordedAt.IsZero() {
		t.Fatalf("RecordedAt is zero")
	}
}

func TestRunChildReturnedErrorStopsWithDiagnostics(t *testing.T) {
	wantErr := errors.New("returned failure")
	def := Definition{
		ID:    "child-returned-error",
		Label: "Child Returned Error",
		Fn: func(context.Context, string, func(WorkflowEvent), WorkflowAsk) error {
			return wantErr
		},
	}

	result, err := RunChild(context.Background(), t.TempDir(), def, func(WorkflowEvent) {}, nil, ChildRunOptions{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("RunChild error = %v, want returned failure", err)
	}
	if !errors.Is(result.ReturnedError, wantErr) {
		t.Fatalf("result.ReturnedError = %v, want returned failure", result.ReturnedError)
	}
	if result.FirstWorkflowError != nil {
		t.Fatalf("result.FirstWorkflowError = %#v, want nil", result.FirstWorkflowError)
	}
}

func TestRunChildWorkflowErrorStopsWithDiagnostics(t *testing.T) {
	def := Definition{
		ID:    "child-workflow-error",
		Label: "Child Workflow Error",
		Fn: func(_ context.Context, _ string, onEvent func(WorkflowEvent), _ WorkflowAsk) error {
			onEvent(WorkflowEvent{Type: WorkflowError, Message: "logical failure"})
			onEvent(WorkflowEvent{Type: WorkflowError, Message: "later failure"})
			return nil
		},
	}

	result, err := RunChild(context.Background(), t.TempDir(), def, func(WorkflowEvent) {}, nil, ChildRunOptions{})
	if err == nil {
		t.Fatalf("RunChild error is nil, want first WorkflowError message")
	}
	if err.Error() != "logical failure" {
		t.Fatalf("RunChild error = %v, want logical failure", err)
	}
	if result.ReturnedError != nil {
		t.Fatalf("result.ReturnedError = %v, want nil", result.ReturnedError)
	}
	if result.FirstWorkflowError == nil {
		t.Fatalf("result.FirstWorkflowError is nil, want first WorkflowError")
	}
	if result.FirstWorkflowError.Message != "logical failure" {
		t.Fatalf("result.FirstWorkflowError.Message = %q, want logical failure", result.FirstWorkflowError.Message)
	}
}

func TestRunChildAppliesCapturePolicyWithoutDroppingForwarding(t *testing.T) {
	var forwarded []WorkflowEvent
	def := Definition{
		ID:    "child-capture",
		Label: "Child Capture",
		Fn: func(_ context.Context, _ string, onEvent func(WorkflowEvent), _ WorkflowAsk) error {
			onEvent(WorkflowEvent{Type: WorkflowText, Text: "keep-1"})
			onEvent(WorkflowEvent{Type: WorkflowStep, Label: "forward-only"})
			onEvent(WorkflowEvent{Type: WorkflowText, Text: "keep-2"})
			onEvent(WorkflowEvent{Type: WorkflowText, Text: "over-limit"})
			return nil
		},
	}

	result, err := RunChild(context.Background(), t.TempDir(), def, func(event WorkflowEvent) {
		forwarded = append(forwarded, event)
	}, nil, ChildRunOptions{
		Capture: EventCapturePolicy{
			Predicate: func(event WorkflowEvent) bool { return event.Type == WorkflowText },
			MaxEvents: 2,
		},
	})
	if err != nil {
		t.Fatalf("RunChild returned error: %v", err)
	}
	if len(forwarded) != 5 {
		t.Fatalf("forwarded event count = %d, want active + 4 child events", len(forwarded))
	}
	got := make([]string, 0, len(result.CapturedEvents))
	for _, event := range result.CapturedEvents {
		got = append(got, event.Text)
	}
	want := []string{"keep-1", "keep-2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("captured text events = %v, want %v", got, want)
	}
	if !result.CapturedEventLimitReached {
		t.Fatalf("CapturedEventLimitReached = false, want true")
	}
}
