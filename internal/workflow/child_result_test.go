package workflow

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestRunDefinitionWithResultBuildsChildResultContract(t *testing.T) {
	wantErr := errors.New("returned failure")
	def := Definition{
		ID:    "child-result-contract",
		Label: "Child Result Contract",
		Fn: func(_ context.Context, _ string, onEvent func(WorkflowEvent), _ WorkflowAsk) error {
			onEvent(WorkflowEvent{Type: WorkflowText, Text: "before-error"})
			onEvent(WorkflowEvent{Type: WorkflowError, Message: "first logical failure"})
			onEvent(WorkflowEvent{Type: WorkflowError, Message: "second logical failure"})
			return wantErr
		},
	}

	result := runDefinitionWithResult(context.Background(), def, t.TempDir(), func(WorkflowEvent) {}, nil, definitionRunOptions{
		Capture: EventCapturePolicy{
			Predicate: func(event WorkflowEvent) bool {
				return event.Type == WorkflowText || event.Type == WorkflowError
			},
			MaxEvents: 3,
		},
	})

	if result.DefinitionID != def.ID {
		t.Fatalf("result.DefinitionID = %q, want %q", result.DefinitionID, def.ID)
	}
	if result.Label != def.Label {
		t.Fatalf("result.Label = %q, want %q", result.Label, def.Label)
	}
	if result.RunID == "" {
		t.Fatalf("result.RunID is empty")
	}
	if !errors.Is(result.ReturnedError, wantErr) {
		t.Fatalf("result.ReturnedError = %v, want errors.Is returned failure", result.ReturnedError)
	}
	if result.FirstWorkflowError == nil {
		t.Fatalf("result.FirstWorkflowError is nil, want first WorkflowError")
	}
	if result.FirstWorkflowError.Message != "first logical failure" {
		t.Fatalf("result.FirstWorkflowError.Message = %q, want first logical failure", result.FirstWorkflowError.Message)
	}
	if result.FirstWorkflowError.RunID != result.RunID {
		t.Fatalf("result.FirstWorkflowError.RunID = %q, want %q", result.FirstWorkflowError.RunID, result.RunID)
	}
	if result.CapturedEventLimitReached {
		t.Fatalf("result.CapturedEventLimitReached = true, want false")
	}
	var got []string
	for _, event := range result.CapturedEvents {
		if event.RunID != result.RunID {
			t.Fatalf("captured event %#v has RunID %q, want %q", event, event.RunID, result.RunID)
		}
		got = append(got, string(event.Type)+":"+event.Text+event.Message)
	}
	want := []string{
		"text:before-error",
		"error:first logical failure",
		"error:second logical failure",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("captured events = %v, want %v", got, want)
	}
}

func TestRunDefinitionWithResultDefaultDoesNotCaptureEvents(t *testing.T) {
	var forwarded []WorkflowEvent
	def := Definition{
		ID:    "child-result-default-capture",
		Label: "Child Result Default Capture",
		Fn: func(_ context.Context, _ string, onEvent func(WorkflowEvent), _ WorkflowAsk) error {
			onEvent(WorkflowEvent{Type: WorkflowText, Text: "forwarded"})
			onEvent(WorkflowEvent{Type: WorkflowStep, Label: "also-forwarded"})
			return nil
		},
	}

	result := runDefinitionWithResult(context.Background(), def, t.TempDir(), func(event WorkflowEvent) {
		forwarded = append(forwarded, event)
	}, nil, definitionRunOptions{})

	if len(forwarded) != 2 {
		t.Fatalf("forwarded event count = %d, want 2", len(forwarded))
	}
	if len(result.CapturedEvents) != 0 {
		t.Fatalf("CapturedEvents count = %d, want 0 by default", len(result.CapturedEvents))
	}
	if result.CapturedEventLimitReached {
		t.Fatalf("CapturedEventLimitReached = true, want false by default")
	}
}

func TestRunDefinitionWithResultCapturesOnlyPredicateMatchesWithinExplicitMax(t *testing.T) {
	def := Definition{
		ID:    "child-result-bounded-capture",
		Label: "Child Result Bounded Capture",
		Fn: func(_ context.Context, _ string, onEvent func(WorkflowEvent), _ WorkflowAsk) error {
			onEvent(WorkflowEvent{Type: WorkflowText, Text: "keep-1"})
			onEvent(WorkflowEvent{Type: WorkflowStep, Label: "skip"})
			onEvent(WorkflowEvent{Type: WorkflowText, Text: "keep-2"})
			onEvent(WorkflowEvent{Type: WorkflowText, Text: "over-limit"})
			return nil
		},
	}

	result := runDefinitionWithResult(context.Background(), def, t.TempDir(), func(WorkflowEvent) {}, nil, definitionRunOptions{
		Capture: EventCapturePolicy{
			Predicate: func(event WorkflowEvent) bool {
				return event.Type == WorkflowText
			},
			MaxEvents: 2,
		},
	})

	got := make([]string, 0, len(result.CapturedEvents))
	for _, event := range result.CapturedEvents {
		got = append(got, event.Text)
	}
	want := []string{"keep-1", "keep-2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("captured text events = %v, want %v", got, want)
	}
	if !result.CapturedEventLimitReached {
		t.Fatalf("CapturedEventLimitReached = false, want true after third matching event")
	}
}

func TestRunDefinitionWithResultRequiresPredicateAndPositiveMaxForCapture(t *testing.T) {
	def := Definition{
		ID:    "child-result-explicit-capture",
		Label: "Child Result Explicit Capture",
		Fn: func(_ context.Context, _ string, onEvent func(WorkflowEvent), _ WorkflowAsk) error {
			onEvent(WorkflowEvent{Type: WorkflowText, Text: "not-captured"})
			return nil
		},
	}

	tests := map[string]definitionRunOptions{
		"predicate without max": {
			Capture: EventCapturePolicy{
				Predicate: func(WorkflowEvent) bool { return true },
			},
		},
		"max without predicate": {
			Capture: EventCapturePolicy{MaxEvents: 1},
		},
	}
	for name, options := range tests {
		t.Run(name, func(t *testing.T) {
			result := runDefinitionWithResult(context.Background(), def, t.TempDir(), func(WorkflowEvent) {}, nil, options)
			if len(result.CapturedEvents) != 0 {
				t.Fatalf("CapturedEvents count = %d, want 0 without predicate and positive max", len(result.CapturedEvents))
			}
			if result.CapturedEventLimitReached {
				t.Fatalf("CapturedEventLimitReached = true, want false without active capture policy")
			}
		})
	}
}
