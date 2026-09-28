package apply

import (
	"context"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

func TestSequentialIOReviewRunIDHandOffCanUseEventlessChildResultRunID(t *testing.T) {
	type reviewOutput struct {
		RunID string
	}
	type applyInput struct {
		ReviewRunID string
	}

	var reviewRunIDFromMetadata string
	var reviewRunIDFromExtractor string
	var reviewRunIDFromApply string
	var reviewRunIDInjected bool
	var activeLabels []string

	reviewDef := workflow.Definition{
		ID:    "review-workflow-stub",
		Label: "Review Stub",
		Fn: func(ctx context.Context, _ string, _ func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			metadata, ok := workflowctx.RunMetadataFromContext(ctx)
			if !ok {
				t.Fatalf("review child missing run metadata")
			}
			reviewRunIDFromMetadata = metadata.WorkflowRunID
			if reviewRunIDFromMetadata == "" {
				t.Fatalf("review child metadata WorkflowRunID is empty")
			}
			return nil
		},
	}
	applyDef := workflow.Definition{
		ID:    "review-apply-workflow-stub",
		Label: "Apply Stub",
		Fn: func(ctx context.Context, _ string, _ func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			reviewRunIDFromApply, reviewRunIDInjected = ReviewRunIDFromContext(ctx)
			return nil
		},
	}

	pipeline := workflow.Then(
		workflow.Start(workflow.Step[workflow.NoInput, reviewOutput]{
			Definition: reviewDef,
			Extract: func(_ context.Context, _ string, result workflow.ChildResult) (reviewOutput, error) {
				reviewRunIDFromExtractor = result.RunID
				return reviewOutput{RunID: result.RunID}, nil
			},
		}),
		func(output reviewOutput) (applyInput, error) {
			return applyInput{ReviewRunID: output.RunID}, nil
		},
		workflow.Step[applyInput, workflow.NoOutput]{
			Definition: applyDef,
			Deliver: func(ctx context.Context, _ string, ask workflow.WorkflowAsk, input applyInput) (context.Context, workflow.WorkflowAsk, error) {
				return ContextWithReviewRunID(ctx, input.ReviewRunID), ask, nil
			},
		},
	)
	def := workflow.BuildSequentialIO(workflow.DefinitionConfig{
		ID:     "review-and-apply-sequential-io-applicability",
		Label:  "Review and Apply Sequential IO Applicability",
		Hidden: true,
	}, pipeline)

	err := def.Run(context.Background(), t.TempDir(), func(event workflow.WorkflowEvent) {
		if event.Type == workflow.WorkflowActive {
			activeLabels = append(activeLabels, event.Label)
		}
	}, nil)
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}

	if !reviewRunIDInjected {
		t.Fatalf("ReviewRunIDFromContext ok = false, want true")
	}
	if reviewRunIDFromExtractor == "" {
		t.Fatalf("ChildResult.RunID extracted for review child is empty")
	}
	if reviewRunIDFromExtractor != reviewRunIDFromMetadata {
		t.Fatalf("ChildResult.RunID = %q, want review metadata runID %q", reviewRunIDFromExtractor, reviewRunIDFromMetadata)
	}
	if reviewRunIDFromApply != reviewRunIDFromExtractor {
		t.Fatalf("apply reviewRunID = %q, want extracted ChildResult.RunID %q", reviewRunIDFromApply, reviewRunIDFromExtractor)
	}

	wantActiveLabels := []string{"Review Stub", "Apply Stub"}
	if len(activeLabels) != len(wantActiveLabels) {
		t.Fatalf("active labels = %v, want %v", activeLabels, wantActiveLabels)
	}
	for i, want := range wantActiveLabels {
		if activeLabels[i] != want {
			t.Fatalf("active labels = %v, want %v", activeLabels, wantActiveLabels)
		}
	}
}
