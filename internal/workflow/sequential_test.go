package workflow

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// recordingDef はテスト用に「実行されたら id をログに残す Definition」を返す。
// errReturn が non-nil なら Fn から返す。emitWorkflowError が空でなければ
// onEvent で WorkflowError イベントを発する（既存ワークフローの罠を再現）。
func recordingDef(id, label string, log *[]string, errReturn error, emitWorkflowError string) Definition {
	return Definition{
		ID:    id,
		Label: label,
		Fn: func(_ context.Context, _ string, onEvent func(WorkflowEvent), _ WorkflowAsk) error {
			*log = append(*log, id)
			if emitWorkflowError != "" {
				onEvent(WorkflowEvent{Type: WorkflowError, Message: emitWorkflowError})
			}
			return errReturn
		},
	}
}

func TestSequentialRunsChildrenInOrder(t *testing.T) {
	agent := &fakeAgent{}
	var log []string
	seq := Sequential("seq", "Seq",
		agent,
		recordingDef("first", "First", &log, nil, ""),
		recordingDef("second", "Second", &log, nil, ""),
	)
	if err := seq.Run(context.Background(), t.TempDir(), func(WorkflowEvent) {}, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(log) != 2 || log[0] != "first" || log[1] != "second" {
		t.Fatalf("expected [first, second], got %v", log)
	}
}

func TestSequentialStopsOnFirstChildReturningError(t *testing.T) {
	agent := &fakeAgent{}
	var log []string
	var activeLabels []string
	seq := Sequential("seq", "Seq",
		agent,
		recordingDef("first", "First", &log, errors.New("boom"), ""),
		recordingDef("second", "Second", &log, nil, ""),
	)
	err := seq.Run(context.Background(), t.TempDir(), func(e WorkflowEvent) {
		if e.Type == WorkflowActive {
			activeLabels = append(activeLabels, e.Label)
		}
	}, nil)
	if err == nil {
		t.Fatal("expected error from first child")
	}
	if len(log) != 1 || log[0] != "first" {
		t.Fatalf("expected only first child to run, got %v", log)
	}
	if !reflect.DeepEqual(activeLabels, []string{"First"}) {
		t.Fatalf("expected only first child active event, got %v", activeLabels)
	}
}

func TestSequentialStopsOnWorkflowErrorEvent(t *testing.T) {
	agent := &fakeAgent{}
	var log []string
	var activeLabels []string
	seq := Sequential("seq", "Seq",
		agent,
		recordingDef("first", "First", &log, nil, "agent failed"),
		recordingDef("second", "Second", &log, nil, ""),
	)
	err := seq.Run(context.Background(), t.TempDir(), func(e WorkflowEvent) {
		if e.Type == WorkflowActive {
			activeLabels = append(activeLabels, e.Label)
		}
	}, nil)
	if err == nil {
		t.Fatal("expected error captured from WorkflowError event")
	}
	if err.Error() != "agent failed" {
		t.Fatalf("expected error message %q, got %v", "agent failed", err)
	}
	if len(log) != 1 || log[0] != "first" {
		t.Fatalf("expected only first child to run, got %v", log)
	}
	if !reflect.DeepEqual(activeLabels, []string{"First"}) {
		t.Fatalf("expected only first child active event, got %v", activeLabels)
	}
}

func TestSequentialEmitsActiveEventBeforeEachChild(t *testing.T) {
	agent := &fakeAgent{}
	var log []string
	var activeLabels []string
	seq := Sequential("seq", "Seq",
		agent,
		recordingDef("first", "First Label", &log, nil, ""),
		recordingDef("second", "Second Label", &log, nil, ""),
	)
	err := seq.Run(context.Background(), t.TempDir(), func(e WorkflowEvent) {
		if e.Type == WorkflowActive {
			activeLabels = append(activeLabels, e.Label)
		}
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(activeLabels) != 2 || activeLabels[0] != "First Label" || activeLabels[1] != "Second Label" {
		t.Fatalf("expected active labels [First Label, Second Label], got %v", activeLabels)
	}
}

func TestSequentialForwardsChildEventsToParent(t *testing.T) {
	agent := &fakeAgent{}
	var log []string
	var parentEvents []WorkflowEvent
	seq := Sequential("seq", "Seq",
		agent,
		Definition{
			ID:    "child",
			Label: "Child",
			Fn: func(_ context.Context, _ string, onEvent func(WorkflowEvent), _ WorkflowAsk) error {
				onEvent(WorkflowEvent{Type: WorkflowStep, Step: 1, TotalSteps: 2, Label: "step1"})
				onEvent(WorkflowEvent{Type: WorkflowText, Text: "hello"})
				return nil
			},
		},
		recordingDef("after", "After", &log, nil, ""),
	)
	err := seq.Run(context.Background(), t.TempDir(), func(e WorkflowEvent) {
		parentEvents = append(parentEvents, e)
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	foundStep, foundText := false, false
	for _, e := range parentEvents {
		if e.Type == WorkflowStep && e.Label == "step1" {
			foundStep = true
		}
		if e.Type == WorkflowText && e.Text == "hello" {
			foundText = true
		}
	}
	if !foundStep || !foundText {
		t.Fatalf("expected child events forwarded to parent, got %#v", parentEvents)
	}
	if len(log) != 1 || log[0] != "after" {
		t.Fatalf("expected after-child to also run after first child succeeded, got %v", log)
	}
}

func TestSequentialPreservesActiveAndForwardedChildEventOrder(t *testing.T) {
	agent := &fakeAgent{}
	var sequence []string
	seq := Sequential("seq", "Seq",
		agent,
		Definition{
			ID:    "first",
			Label: "First",
			Fn: func(_ context.Context, _ string, onEvent func(WorkflowEvent), _ WorkflowAsk) error {
				sequence = append(sequence, "fn:first")
				onEvent(WorkflowEvent{Type: WorkflowStep, Label: "first-step"})
				onEvent(WorkflowEvent{Type: WorkflowText, Text: "first-text"})
				return nil
			},
		},
		Definition{
			ID:    "second",
			Label: "Second",
			Fn: func(_ context.Context, _ string, onEvent func(WorkflowEvent), _ WorkflowAsk) error {
				sequence = append(sequence, "fn:second")
				onEvent(WorkflowEvent{Type: WorkflowStep, Label: "second-step"})
				return nil
			},
		},
	)

	err := seq.Run(context.Background(), t.TempDir(), func(e WorkflowEvent) {
		if e.RunID == "" {
			t.Fatalf("forwarded event %#v has empty RunID", e)
		}
		switch e.Type {
		case WorkflowActive:
			sequence = append(sequence, "event:active:"+e.Label)
		case WorkflowStep:
			sequence = append(sequence, "event:step:"+e.Label)
		case WorkflowText:
			sequence = append(sequence, "event:text:"+e.Text)
		}
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{
		"event:active:First",
		"fn:first",
		"event:step:first-step",
		"event:text:first-text",
		"event:active:Second",
		"fn:second",
		"event:step:second-step",
	}
	if !reflect.DeepEqual(sequence, want) {
		t.Fatalf("event/function sequence = %v, want %v", sequence, want)
	}
}
