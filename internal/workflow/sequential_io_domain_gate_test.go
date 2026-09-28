package workflow

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

type sequentialIODomainGateCompletedResult struct {
	GatePassed  bool
	FeatureLike string
}

type sequentialIODomainGateOutput struct {
	FeatureLike string
}

type sequentialIODomainGateInput struct {
	FeatureLike string
}

type sequentialIODomainGateInputKey struct{}

var (
	errSequentialIODomainGateRejected       = errors.New("domain gate rejected completed result")
	errSequentialIODomainGateMissingFeature = errors.New("completed result missing feature-like value")
)

func TestSequentialIODomainGateApplicability(t *testing.T) {
	t.Run("completed result handoff", func(t *testing.T) {
		key := sequentialIODomainGateInputKey{}
		var deliveredFeature string
		var sequence []string

		pipeline := Then(
			Start(Step[NoInput, sequentialIODomainGateOutput]{
				Definition: sequentialIODomainGateProducer(t, "gate=passed;feature=future-feature-alpha", &sequence),
				Capture:    sequentialIODomainGateCapturePolicy(),
				Extract: func(_ context.Context, _ string, result ChildResult) (sequentialIODomainGateOutput, error) {
					output, err := extractSequentialIODomainGateOutput(result)
					if err == nil {
						sequence = append(sequence, "extract:"+output.FeatureLike)
					}
					return output, err
				},
			}),
			func(output sequentialIODomainGateOutput) (sequentialIODomainGateInput, error) {
				sequence = append(sequence, "transform:"+output.FeatureLike)
				return sequentialIODomainGateInput{FeatureLike: output.FeatureLike}, nil
			},
			Step[sequentialIODomainGateInput, NoOutput]{
				Definition: Definition{
					ID:    "future-consumer-stub",
					Label: "Future Consumer Stub",
					Fn: func(ctx context.Context, _ string, _ func(WorkflowEvent), _ WorkflowAsk) error {
						sequence = append(sequence, "fn:consumer")
						got, ok := ctx.Value(key).(string)
						if !ok {
							t.Fatalf("feature-like input was not delivered")
						}
						deliveredFeature = got
						return nil
					},
				},
				Deliver: func(ctx context.Context, _ string, ask WorkflowAsk, input sequentialIODomainGateInput) (context.Context, WorkflowAsk, error) {
					sequence = append(sequence, "deliver:"+input.FeatureLike)
					return context.WithValue(ctx, key, input.FeatureLike), ask, nil
				},
			},
		)
		def := BuildSequentialIO(sequentialIODomainGateConfig(), pipeline)

		err := def.Run(context.Background(), t.TempDir(), func(event WorkflowEvent) {
			if event.Type == WorkflowActive {
				sequence = append(sequence, "event:active:"+event.Label)
			}
		}, nil)
		if err != nil {
			t.Fatalf("Run error = %v, want nil", err)
		}

		if deliveredFeature != "future-feature-alpha" {
			t.Fatalf("delivered feature-like value = %q, want future-feature-alpha", deliveredFeature)
		}
		wantSequence := []string{
			"event:active:Domain Producer Stub",
			"fn:producer",
			"extract:future-feature-alpha",
			"transform:future-feature-alpha",
			"deliver:future-feature-alpha",
			"event:active:Future Consumer Stub",
			"fn:consumer",
		}
		if !reflect.DeepEqual(sequence, wantSequence) {
			t.Fatalf("sequence = %v, want %v", sequence, wantSequence)
		}
	})

	t.Run("gate failure stops before consumer as pipeline-owned failure", func(t *testing.T) {
		gotErr, gotEvents := runSequentialIODomainGateFailureScenario(t, "gate=blocked;feature=future-feature-alpha")

		wantError := `sequential IO stage "Domain Producer Stub" output extraction failed: domain gate rejected completed result`
		assertSequentialIOWrappedError(t, gotErr, wantError, errSequentialIODomainGateRejected)
		assertSequentialIOWorkflowError(t, gotEvents, wantError)
		assertSequentialIONoActiveEvent(t, gotEvents, "Future Consumer Stub")
	})

	t.Run("missing feature-like value stops before consumer as pipeline-owned failure", func(t *testing.T) {
		gotErr, gotEvents := runSequentialIODomainGateFailureScenario(t, "gate=passed")

		wantError := `sequential IO stage "Domain Producer Stub" output extraction failed: completed result missing feature-like value`
		assertSequentialIOWrappedError(t, gotErr, wantError, errSequentialIODomainGateMissingFeature)
		assertSequentialIOWorkflowError(t, gotEvents, wantError)
		assertSequentialIONoActiveEvent(t, gotEvents, "Future Consumer Stub")
	})

	t.Run("workflow package import graph stays free of builtin domain packages", func(t *testing.T) {
		cmd := exec.Command("go", "list", "-deps", "github.com/kazuki-oshino/rilll/internal/workflow")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go list -deps internal/workflow: %v\n%s", err, string(output))
		}
		for _, pkg := range strings.Fields(string(output)) {
			if strings.Contains(pkg, "/internal/workflow/builtin") {
				t.Fatalf("internal/workflow import graph includes builtin domain package %q", pkg)
			}
		}
	})
}

func runSequentialIODomainGateFailureScenario(t *testing.T, resultFixture string) (error, []WorkflowEvent) {
	t.Helper()

	pipeline := Then(
		Start(Step[NoInput, sequentialIODomainGateOutput]{
			Definition: sequentialIODomainGateProducer(t, resultFixture, nil),
			Capture:    sequentialIODomainGateCapturePolicy(),
			Extract: func(_ context.Context, _ string, result ChildResult) (sequentialIODomainGateOutput, error) {
				return extractSequentialIODomainGateOutput(result)
			},
		}),
		func(output sequentialIODomainGateOutput) (sequentialIODomainGateInput, error) {
			return sequentialIODomainGateInput{FeatureLike: output.FeatureLike}, nil
		},
		Step[sequentialIODomainGateInput, NoOutput]{
			Definition: sequentialIOUnexpectedChild(t, "future-consumer-stub", "Future Consumer Stub"),
		},
	)
	def := BuildSequentialIO(sequentialIODomainGateConfig(), pipeline)

	return runSequentialIOTestDefinition(t, def)
}

func sequentialIODomainGateProducer(t *testing.T, resultFixture string, sequence *[]string) Definition {
	t.Helper()

	return Definition{
		ID:    "domain-producer-stub",
		Label: "Domain Producer Stub",
		Fn: func(_ context.Context, _ string, onEvent func(WorkflowEvent), _ WorkflowAsk) error {
			if sequence != nil {
				*sequence = append(*sequence, "fn:producer")
			}
			onEvent(WorkflowEvent{Type: WorkflowDone, Result: resultFixture})
			return nil
		},
	}
}

func sequentialIODomainGateConfig() DefinitionConfig {
	return DefinitionConfig{
		ID:     "generic-domain-gate-sequential-io",
		Label:  "Generic Domain Gate Sequential IO",
		Hidden: true,
	}
}

func sequentialIODomainGateCapturePolicy() EventCapturePolicy {
	return EventCapturePolicy{
		Predicate: func(event WorkflowEvent) bool {
			return event.Type == WorkflowDone
		},
		MaxEvents: 1,
	}
}

func extractSequentialIODomainGateOutput(result ChildResult) (sequentialIODomainGateOutput, error) {
	completed, ok := sequentialIODomainGateCompletedResultFromChildResult(result)
	if !ok || completed.FeatureLike == "" {
		return sequentialIODomainGateOutput{}, errSequentialIODomainGateMissingFeature
	}
	if !completed.GatePassed {
		return sequentialIODomainGateOutput{}, errSequentialIODomainGateRejected
	}
	return sequentialIODomainGateOutput{FeatureLike: completed.FeatureLike}, nil
}

func sequentialIODomainGateCompletedResultFromChildResult(result ChildResult) (sequentialIODomainGateCompletedResult, bool) {
	for _, event := range result.CapturedEvents {
		if event.Type != WorkflowDone {
			continue
		}
		return sequentialIODomainGateParseCompletedResult(event.Result), true
	}
	return sequentialIODomainGateCompletedResult{}, false
}

func sequentialIODomainGateParseCompletedResult(raw string) sequentialIODomainGateCompletedResult {
	var completed sequentialIODomainGateCompletedResult
	for _, field := range strings.Split(raw, ";") {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch key {
		case "gate":
			completed.GatePassed = value == "passed"
		case "feature":
			completed.FeatureLike = value
		}
	}
	return completed
}
