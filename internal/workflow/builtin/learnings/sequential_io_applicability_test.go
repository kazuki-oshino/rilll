package learnings

import (
	"context"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

func TestSequentialIOParentRunIDHandOffCanUseEventlessChildResultRunID(t *testing.T) {
	type baseOutput struct {
		ParentRunID string
	}
	type tailInput struct {
		ParentRunID string
	}

	var baseRunIDFromMetadata string
	var parentRunIDFromExtractor string
	var parentRunIDFromTail string
	var activeLabels []string

	baseDef := workflow.Definition{
		ID:    "learnings-base-stub",
		Label: "Learnings Base Stub",
		Fn: func(ctx context.Context, _ string, _ func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			metadata, ok := workflowctx.RunMetadataFromContext(ctx)
			if !ok {
				t.Fatalf("base child missing run metadata")
			}
			baseRunIDFromMetadata = metadata.WorkflowRunID
			if baseRunIDFromMetadata == "" {
				t.Fatalf("base child metadata WorkflowRunID is empty")
			}
			return nil
		},
	}
	tailDef := workflow.Definition{
		ID:    "learnings-tail-stub",
		Label: "Learnings Tail Stub",
		Fn: func(ctx context.Context, _ string, _ func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			parentRunIDFromTail = ParentRunIDFromContext(ctx)
			return nil
		},
	}

	pipeline := workflow.Then(
		workflow.Start(workflow.Step[workflow.NoInput, baseOutput]{
			Definition: baseDef,
			Extract: func(_ context.Context, _ string, result workflow.ChildResult) (baseOutput, error) {
				parentRunIDFromExtractor = result.RunID
				return baseOutput{ParentRunID: result.RunID}, nil
			},
		}),
		func(output baseOutput) (tailInput, error) {
			return tailInput{ParentRunID: output.ParentRunID}, nil
		},
		workflow.Step[tailInput, workflow.NoOutput]{
			Definition: tailDef,
			Deliver: func(ctx context.Context, _ string, ask workflow.WorkflowAsk, input tailInput) (context.Context, workflow.WorkflowAsk, error) {
				return ContextWithParentRunID(ctx, input.ParentRunID), ask, nil
			},
		},
	)
	def := workflow.BuildSequentialIO(workflow.DefinitionConfig{
		ID:     "learnings-sequential-io-applicability",
		Label:  "Learnings Sequential IO Applicability",
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

	if parentRunIDFromTail == "" {
		t.Fatalf("ParentRunIDFromContext(ctx) is empty, want injected parent runID")
	}
	if parentRunIDFromExtractor == "" {
		t.Fatalf("ChildResult.RunID extracted for base child is empty")
	}
	if parentRunIDFromExtractor != baseRunIDFromMetadata {
		t.Fatalf("ChildResult.RunID = %q, want base metadata runID %q", parentRunIDFromExtractor, baseRunIDFromMetadata)
	}
	if parentRunIDFromTail != parentRunIDFromExtractor {
		t.Fatalf("tail parentRunID = %q, want extracted ChildResult.RunID %q", parentRunIDFromTail, parentRunIDFromExtractor)
	}

	wantActiveLabels := []string{"Learnings Base Stub", "Learnings Tail Stub"}
	if len(activeLabels) != len(wantActiveLabels) {
		t.Fatalf("active labels = %v, want %v", activeLabels, wantActiveLabels)
	}
	for i, want := range wantActiveLabels {
		if activeLabels[i] != want {
			t.Fatalf("active labels = %v, want %v", activeLabels, wantActiveLabels)
		}
	}
}
