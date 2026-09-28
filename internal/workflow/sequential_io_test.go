package workflow

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestSequentialIOHandsOffTypedRunIDThroughTransformAndDelivery(t *testing.T) {
	type reviewOutput struct {
		RunID        string
		DefinitionID string
	}
	type applyInput struct {
		ReviewRunID  string
		SourceStepID string
	}
	type inputKey struct{}

	var sequence []string
	var extractedRunID string
	key := inputKey{}

	pipeline := Then(
		Start(Step[NoInput, reviewOutput]{
			Definition: Definition{
				ID:    "review",
				Label: "Review",
				Fn: func(context.Context, string, func(WorkflowEvent), WorkflowAsk) error {
					sequence = append(sequence, "fn:review")
					return nil
				},
			},
			Extract: func(_ context.Context, _ string, result ChildResult) (reviewOutput, error) {
				sequence = append(sequence, "extract:"+result.DefinitionID)
				if result.RunID == "" {
					t.Fatalf("ChildResult.RunID is empty")
				}
				extractedRunID = result.RunID
				return reviewOutput{RunID: result.RunID, DefinitionID: result.DefinitionID}, nil
			},
		}),
		func(output reviewOutput) (applyInput, error) {
			sequence = append(sequence, "transform:"+output.DefinitionID)
			return applyInput{
				ReviewRunID:  output.RunID,
				SourceStepID: output.DefinitionID,
			}, nil
		},
		Step[applyInput, NoOutput]{
			Definition: Definition{
				ID:    "apply",
				Label: "Apply",
				Fn: func(ctx context.Context, _ string, _ func(WorkflowEvent), _ WorkflowAsk) error {
					sequence = append(sequence, "fn:apply")
					got, ok := ctx.Value(key).(applyInput)
					if !ok {
						t.Fatalf("apply input was not delivered to child context")
					}
					if got.ReviewRunID == "" {
						t.Fatalf("apply input ReviewRunID is empty")
					}
					if got.ReviewRunID != extractedRunID {
						t.Fatalf("apply input ReviewRunID = %q, want extracted runID %q", got.ReviewRunID, extractedRunID)
					}
					if got.SourceStepID != "review" {
						t.Fatalf("apply input SourceStepID = %q, want review", got.SourceStepID)
					}
					return nil
				},
			},
			Deliver: func(ctx context.Context, _ string, ask WorkflowAsk, input applyInput) (context.Context, WorkflowAsk, error) {
				sequence = append(sequence, "deliver:"+input.SourceStepID)
				return context.WithValue(ctx, key, input), ask, nil
			},
		},
	)
	def := BuildSequentialIO(validSequentialIOTestConfig(), pipeline)

	var gotEvents []WorkflowEvent
	gotErr := def.Run(context.Background(), t.TempDir(), func(event WorkflowEvent) {
		gotEvents = append(gotEvents, event)
		if observed, ok := sequentialIOEventObservation(event); ok {
			sequence = append(sequence, observed)
		}
	}, nil)
	if gotErr != nil {
		t.Fatalf("Run error = %v, want nil", gotErr)
	}

	wantSequence := []string{
		"event:active:Review",
		"fn:review",
		"extract:review",
		"transform:review",
		"deliver:review",
		"event:active:Apply",
		"fn:apply",
	}
	if !reflect.DeepEqual(sequence, wantSequence) {
		t.Fatalf("sequence = %v, want %v", sequence, wantSequence)
	}

	gotActive := sequentialIOEventSequence(gotEvents)
	wantActive := []string{"event:active:Review", "event:active:Apply"}
	if !reflect.DeepEqual(gotActive, wantActive) {
		t.Fatalf("active event sequence = %v, want %v", gotActive, wantActive)
	}
}

func TestSequentialIONoPayloadConnectionRunsInOrderAndForwardsEvents(t *testing.T) {
	var sequence []string
	pipeline := Then(
		Start(Step[NoInput, NoOutput]{
			Definition: Definition{
				ID:    "first",
				Label: "First",
				Fn: func(_ context.Context, _ string, onEvent func(WorkflowEvent), _ WorkflowAsk) error {
					sequence = append(sequence, "fn:first")
					onEvent(WorkflowEvent{Type: WorkflowStep, Label: "first-step"})
					onEvent(WorkflowEvent{Type: WorkflowText, Text: "first-text"})
					return nil
				},
			},
		}),
		func(NoOutput) (NoInput, error) {
			return NoInput{}, nil
		},
		Step[NoInput, NoOutput]{
			Definition: Definition{
				ID:    "second",
				Label: "Second",
				Fn: func(_ context.Context, _ string, onEvent func(WorkflowEvent), _ WorkflowAsk) error {
					sequence = append(sequence, "fn:second")
					onEvent(WorkflowEvent{Type: WorkflowStep, Label: "second-step"})
					return nil
				},
			},
		},
	)
	def := BuildSequentialIO(validSequentialIOTestConfig(), pipeline)

	var gotEvents []WorkflowEvent
	gotErr := def.Run(context.Background(), t.TempDir(), func(event WorkflowEvent) {
		gotEvents = append(gotEvents, event)
		if observed, ok := sequentialIOEventObservation(event); ok {
			sequence = append(sequence, observed)
		}
	}, nil)
	if gotErr != nil {
		t.Fatalf("Run error = %v, want nil", gotErr)
	}

	wantEvents := []string{
		"event:active:First",
		"event:step:first-step",
		"event:text:first-text",
		"event:active:Second",
		"event:step:second-step",
	}
	if !reflect.DeepEqual(sequentialIOEventSequence(gotEvents), wantEvents) {
		t.Fatalf("event sequence = %v, want %v", sequentialIOEventSequence(gotEvents), wantEvents)
	}
	wantSequence := []string{
		"event:active:First",
		"fn:first",
		"event:step:first-step",
		"event:text:first-text",
		"event:active:Second",
		"fn:second",
		"event:step:second-step",
	}
	if !reflect.DeepEqual(sequence, wantSequence) {
		t.Fatalf("sequence = %v, want %v", sequence, wantSequence)
	}
}

func TestSequentialIODefinitionUsesExplicitConfigMetadata(t *testing.T) {
	recorder := &workflowRecorder{}
	args := []ArgSpec{
		{Key: "feature", Description: "feature name", Required: true},
	}
	def := BuildSequentialIO(DefinitionConfig{
		ID:       "generated-id",
		Label:    "Generated Label",
		Args:     args,
		Hidden:   true,
		Recorder: recorder,
	}, Start(Step[NoInput, NoOutput]{
		Definition: Definition{
			ID:    "child-id",
			Label: "Child Label",
			Fn: func(context.Context, string, func(WorkflowEvent), WorkflowAsk) error {
				return nil
			},
		},
	}))

	if def.ID != "generated-id" {
		t.Fatalf("def.ID = %q, want generated-id", def.ID)
	}
	if def.Label != "Generated Label" {
		t.Fatalf("def.Label = %q, want Generated Label", def.Label)
	}
	if !reflect.DeepEqual(def.Args, args) {
		t.Fatalf("def.Args = %#v, want %#v", def.Args, args)
	}
	if !def.Hidden {
		t.Fatalf("def.Hidden = false, want true")
	}
	if def.Recorder != recorder {
		t.Fatalf("def.Recorder = %#v, want configured recorder", def.Recorder)
	}
	if def.Fn == nil {
		t.Fatalf("def.Fn is nil")
	}
}

func TestSequentialIODefinitionRejectsEmptyGeneratedMetadataAtRunTime(t *testing.T) {
	tests := []struct {
		name   string
		config DefinitionConfig
		want   string
	}{
		{
			name:   "id",
			config: DefinitionConfig{Label: "Generated Label"},
			want:   "sequential IO definition metadata invalid: id is empty",
		},
		{
			name:   "label",
			config: DefinitionConfig{ID: "generated-id"},
			want:   "sequential IO definition metadata invalid: label is empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := BuildSequentialIO(tt.config, Start(Step[NoInput, NoOutput]{
				Definition: validSequentialIOTestChild("child-id", "Child Label"),
			}))
			gotErr, gotEvents := runSequentialIOTestDefinition(t, def)

			if gotErr == nil {
				t.Fatalf("Run error is nil, want %q", tt.want)
			}
			if gotErr.Error() != tt.want {
				t.Fatalf("Run error = %q, want %q", gotErr.Error(), tt.want)
			}
			assertSequentialIOWorkflowError(t, gotEvents, tt.want)
		})
	}
}

func TestSequentialIODefinitionRejectsEmptyChildMetadataAtRunTime(t *testing.T) {
	tests := []struct {
		name  string
		child Definition
		want  string
	}{
		{
			name: "id",
			child: Definition{
				Label: "Child Label",
				Fn: func(context.Context, string, func(WorkflowEvent), WorkflowAsk) error {
					return errors.New("child should not run")
				},
			},
			want: "sequential IO child metadata invalid: stage 1 id is empty",
		},
		{
			name: "label",
			child: Definition{
				ID: "child-id",
				Fn: func(context.Context, string, func(WorkflowEvent), WorkflowAsk) error {
					return errors.New("child should not run")
				},
			},
			want: "sequential IO child metadata invalid: stage 1 label is empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := BuildSequentialIO(validSequentialIOTestConfig(), Start(Step[NoInput, NoOutput]{
				Definition: tt.child,
			}))
			gotErr, gotEvents := runSequentialIOTestDefinition(t, def)

			if gotErr == nil {
				t.Fatalf("Run error is nil, want %q", tt.want)
			}
			if gotErr.Error() != tt.want {
				t.Fatalf("Run error = %q, want %q", gotErr.Error(), tt.want)
			}
			assertSequentialIOWorkflowError(t, gotEvents, tt.want)
			for _, event := range gotEvents {
				if event.Type == WorkflowActive {
					t.Fatalf("got unexpected WorkflowActive event %#v", event)
				}
			}
		})
	}
}

func TestSequentialIODefinitionHeterogeneousChainCompiles(t *testing.T) {
	type reviewOutput struct {
		RunID string
	}
	type applyInput struct {
		ReviewRunID string
	}
	type applyOutput struct {
		Applied bool
	}

	pipeline := Then(
		Start(Step[NoInput, reviewOutput]{
			Definition: validSequentialIOTestChild("review", "Review"),
			Extract: func(_ context.Context, _ string, result ChildResult) (reviewOutput, error) {
				return reviewOutput{RunID: result.RunID}, nil
			},
		}),
		func(output reviewOutput) (applyInput, error) {
			return applyInput{ReviewRunID: output.RunID}, nil
		},
		Step[applyInput, applyOutput]{
			Definition: validSequentialIOTestChild("apply", "Apply"),
			Deliver: func(ctx context.Context, _ string, ask WorkflowAsk, input applyInput) (context.Context, WorkflowAsk, error) {
				if input.ReviewRunID == "" {
					return ctx, ask, errors.New("missing review run id")
				}
				return ctx, ask, nil
			},
			Extract: func(context.Context, string, ChildResult) (applyOutput, error) {
				return applyOutput{Applied: true}, nil
			},
		},
	)
	def := BuildSequentialIO(validSequentialIOTestConfig(), pipeline)

	if def.ID != "generated-id" {
		t.Fatalf("def.ID = %q, want generated-id", def.ID)
	}
}

func TestSequentialIOPipelineOwnedFailuresEmitWorkflowErrorAndWrapCause(t *testing.T) {
	type firstOutput struct {
		Value string
	}
	type nextInput struct {
		Value string
	}

	tests := []struct {
		name      string
		pipeline  Pipeline[NoOutput]
		wantError string
		wantCause error
	}{
		{
			name: "output extraction",
			pipeline: Then(
				Start(Step[NoInput, firstOutput]{
					Definition: validSequentialIOTestChild("first", "First"),
					Extract: func(context.Context, string, ChildResult) (firstOutput, error) {
						return firstOutput{}, errSequentialIOTestExtraction
					},
				}),
				func(output firstOutput) (nextInput, error) {
					return nextInput{Value: output.Value}, nil
				},
				Step[nextInput, NoOutput]{
					Definition: sequentialIOUnexpectedChild(t, "second", "Second"),
				},
			),
			wantError: `sequential IO stage "First" output extraction failed: extraction failed`,
			wantCause: errSequentialIOTestExtraction,
		},
		{
			name: "typed transform",
			pipeline: Then(
				Start(Step[NoInput, firstOutput]{
					Definition: validSequentialIOTestChild("first", "First"),
					Extract: func(context.Context, string, ChildResult) (firstOutput, error) {
						return firstOutput{Value: "ok"}, nil
					},
				}),
				func(firstOutput) (nextInput, error) {
					return nextInput{}, errSequentialIOTestTransform
				},
				Step[nextInput, NoOutput]{
					Definition: sequentialIOUnexpectedChild(t, "second", "Second"),
				},
			),
			wantError: `sequential IO stage "Second" typed transform failed: transform failed`,
			wantCause: errSequentialIOTestTransform,
		},
		{
			name: "input delivery",
			pipeline: Then(
				Start(Step[NoInput, firstOutput]{
					Definition: validSequentialIOTestChild("first", "First"),
					Extract: func(context.Context, string, ChildResult) (firstOutput, error) {
						return firstOutput{Value: "ok"}, nil
					},
				}),
				func(output firstOutput) (nextInput, error) {
					return nextInput{Value: output.Value}, nil
				},
				Step[nextInput, NoOutput]{
					Definition: sequentialIOUnexpectedChild(t, "second", "Second"),
					Deliver: func(ctx context.Context, _ string, ask WorkflowAsk, _ nextInput) (context.Context, WorkflowAsk, error) {
						return ctx, ask, errSequentialIOTestDelivery
					},
				},
			),
			wantError: `sequential IO stage "Second" input delivery failed: delivery failed`,
			wantCause: errSequentialIOTestDelivery,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := BuildSequentialIO(validSequentialIOTestConfig(), tt.pipeline)
			gotErr, gotEvents := runSequentialIOTestDefinition(t, def)

			assertSequentialIOWrappedError(t, gotErr, tt.wantError, tt.wantCause)
			assertSequentialIOWorkflowError(t, gotEvents, tt.wantError)
			assertSequentialIONoActiveEvent(t, gotEvents, "Second")
		})
	}
}

func TestSequentialIOChildFailuresStopWithoutExtraPipelineWorkflowError(t *testing.T) {
	t.Run("returned error", func(t *testing.T) {
		childErr := errors.New("child returned failure")
		pipeline := Then(
			Start(Step[NoInput, NoOutput]{
				Definition: Definition{
					ID:    "first",
					Label: "First",
					Fn: func(context.Context, string, func(WorkflowEvent), WorkflowAsk) error {
						return childErr
					},
				},
			}),
			func(NoOutput) (NoInput, error) {
				return NoInput{}, nil
			},
			Step[NoInput, NoOutput]{
				Definition: sequentialIOUnexpectedChild(t, "second", "Second"),
			},
		)
		def := BuildSequentialIO(validSequentialIOTestConfig(), pipeline)
		gotErr, gotEvents := runSequentialIOTestDefinition(t, def)

		if !errors.Is(gotErr, childErr) {
			t.Fatalf("Run error = %v, want child returned failure", gotErr)
		}
		if gotErr.Error() != "child returned failure" {
			t.Fatalf("Run error = %q, want child returned failure", gotErr.Error())
		}
		assertSequentialIOWorkflowErrorCount(t, gotEvents, 0)
		assertSequentialIONoActiveEvent(t, gotEvents, "Second")
	})

	t.Run("workflow error", func(t *testing.T) {
		pipeline := Then(
			Start(Step[NoInput, NoOutput]{
				Definition: Definition{
					ID:    "first",
					Label: "First",
					Fn: func(_ context.Context, _ string, onEvent func(WorkflowEvent), _ WorkflowAsk) error {
						onEvent(WorkflowEvent{Type: WorkflowError, Message: "child logical failure"})
						return nil
					},
				},
			}),
			func(NoOutput) (NoInput, error) {
				return NoInput{}, nil
			},
			Step[NoInput, NoOutput]{
				Definition: sequentialIOUnexpectedChild(t, "second", "Second"),
			},
		)
		def := BuildSequentialIO(validSequentialIOTestConfig(), pipeline)
		gotErr, gotEvents := runSequentialIOTestDefinition(t, def)

		if gotErr == nil {
			t.Fatalf("Run error is nil, want child logical failure")
		}
		if gotErr.Error() != "child logical failure" {
			t.Fatalf("Run error = %q, want child logical failure", gotErr.Error())
		}
		assertSequentialIOWorkflowError(t, gotEvents, "child logical failure")
		assertSequentialIOWorkflowErrorCount(t, gotEvents, 1)
		assertSequentialIONoActiveEvent(t, gotEvents, "Second")
	})
}

var (
	errSequentialIOTestExtraction = errors.New("extraction failed")
	errSequentialIOTestTransform  = errors.New("transform failed")
	errSequentialIOTestDelivery   = errors.New("delivery failed")
)

func validSequentialIOTestConfig() DefinitionConfig {
	return DefinitionConfig{ID: "generated-id", Label: "Generated Label"}
}

func validSequentialIOTestChild(id, label string) Definition {
	return Definition{
		ID:    id,
		Label: label,
		Fn: func(context.Context, string, func(WorkflowEvent), WorkflowAsk) error {
			return nil
		},
	}
}

func sequentialIOUnexpectedChild(t *testing.T, id string, label string) Definition {
	t.Helper()

	return Definition{
		ID:    id,
		Label: label,
		Fn: func(context.Context, string, func(WorkflowEvent), WorkflowAsk) error {
			t.Fatalf("%s child should not run", label)
			return nil
		},
	}
}

func runSequentialIOTestDefinition(t *testing.T, def Definition) (error, []WorkflowEvent) {
	t.Helper()

	var events []WorkflowEvent
	err := def.Run(context.Background(), t.TempDir(), func(event WorkflowEvent) {
		events = append(events, event)
	}, nil)
	return err, events
}

func assertSequentialIOWorkflowError(t *testing.T, events []WorkflowEvent, want string) {
	t.Helper()

	for _, event := range events {
		if event.Type == WorkflowError {
			if event.Message != want {
				t.Fatalf("WorkflowError message = %q, want %q", event.Message, want)
			}
			if event.RunID == "" {
				t.Fatalf("WorkflowError RunID is empty")
			}
			return
		}
	}
	t.Fatalf("WorkflowError event not found in %#v", events)
}

func assertSequentialIOWrappedError(t *testing.T, got error, wantMessage string, wantCause error) {
	t.Helper()

	if got == nil {
		t.Fatalf("Run error is nil, want %q", wantMessage)
	}
	if got.Error() != wantMessage {
		t.Fatalf("Run error = %q, want %q", got.Error(), wantMessage)
	}
	if !errors.Is(got, wantCause) {
		t.Fatalf("Run error = %v, want cause %v", got, wantCause)
	}
	if errors.Unwrap(got) == nil {
		t.Fatalf("Run error does not wrap a cause")
	}
}

func assertSequentialIONoActiveEvent(t *testing.T, events []WorkflowEvent, label string) {
	t.Helper()

	for _, event := range events {
		if event.Type == WorkflowActive && event.Label == label {
			t.Fatalf("got unexpected WorkflowActive for %q in %#v", label, events)
		}
	}
}

func assertSequentialIOWorkflowErrorCount(t *testing.T, events []WorkflowEvent, want int) {
	t.Helper()

	got := 0
	for _, event := range events {
		if event.Type == WorkflowError {
			got++
		}
	}
	if got != want {
		t.Fatalf("WorkflowError count = %d, want %d in %#v", got, want, events)
	}
}

func sequentialIOEventSequence(events []WorkflowEvent) []string {
	got := make([]string, 0, len(events))
	for _, event := range events {
		if observed, ok := sequentialIOEventObservation(event); ok {
			got = append(got, observed)
		}
	}
	return got
}

func sequentialIOEventObservation(event WorkflowEvent) (string, bool) {
	switch event.Type {
	case WorkflowActive:
		return "event:active:" + event.Label, true
	case WorkflowStep:
		return "event:step:" + event.Label, true
	case WorkflowText:
		return "event:text:" + event.Text, true
	default:
		return "", false
	}
}
