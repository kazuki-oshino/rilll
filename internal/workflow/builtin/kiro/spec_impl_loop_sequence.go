package kiro

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

const kiroSpecImplLoopSequenceLabel = "Kiro 仕様作成・実装シーケンス"

func KiroSpecImplLoopSequence(agent workflowctx.AgentCaller) workflow.Definition {
	child := KiroSpecImplLoop(agent)
	return buildKiroSpecImplLoopSequence(agent, child)
}

func buildKiroSpecImplLoopSequence(agent workflowctx.AgentCaller, child workflow.Definition) workflow.Definition {
	runner := sequenceRunner{child: child}
	return workflow.Definition{
		ID:     kiroSpecImplLoopSequenceID,
		Label:  kiroSpecImplLoopSequenceLabel,
		Hidden: false,
		Args: []workflow.ArgSpec{
			{
				Key:         "features",
				Description: "順序付き feature 名リスト（spec 作成対象または実装対象、comma-separated: feature-a,feature-b）",
				Required:    true,
			},
		},
		Recorder: workflow.RecorderFromAgent(agent),
		Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			return runner.run(ctx, cwd, onEvent, ask)
		},
	}
}

type sequenceRunner struct {
	child workflow.Definition
}

func (r sequenceRunner) run(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
	selected, ok, err := r.selectFeatures(ctx, cwd, ask)
	if err != nil {
		return failSequenceWorkflow(onEvent, err)
	}
	if !ok {
		emitSequenceWorkflowError(onEvent, "kiro-spec-impl-loop-sequence cancelled before selecting features")
		return nil
	}

	runID := workflow.RunIDFromContext(ctx)
	artifact := NewKiroSpecImplLoopSequenceArtifact(runID, selected, time.Now().UTC())
	store := newSequenceArtifactStore(cwd)
	if _, err := store.Save(ctx, artifact); err != nil {
		return failSequenceWorkflow(onEvent, err)
	}

	childID := strings.TrimSpace(r.child.ID)
	if childID == "" {
		childID = "<unknown>"
	}
	for i, feature := range selected {
		if onEvent != nil {
			onEvent(workflow.WorkflowEvent{
				Type:       workflow.WorkflowStep,
				Step:       i + 1,
				TotalSteps: len(selected),
				Label:      feature,
			})
		}

		artifact.MarkItemRunning(i, childID, time.Now().UTC())
		if _, err := store.Save(ctx, artifact); err != nil {
			return failSequenceWorkflow(onEvent, err)
		}

		result, err := runSequenceChildWithFixedFeature(ctx, cwd, r.child, feature, onEvent, ask)
		if err != nil {
			kind, message := classifySequenceChildFailure(result, err)
			artifact.MarkItemFailed(i, childID, result.RunID, kind, message, time.Now().UTC())
			if _, saveErr := store.Save(ctx, artifact); saveErr != nil {
				return failSequenceWorkflow(onEvent, saveErr)
			}
			return failSequenceWorkflow(onEvent, err)
		}

		artifact.MarkItemSucceeded(i, childID, result.RunID, time.Now().UTC())
		if _, err := store.Save(ctx, artifact); err != nil {
			return failSequenceWorkflow(onEvent, err)
		}
	}

	artifact.MarkCompleted(time.Now().UTC())
	if _, err := store.Save(ctx, artifact); err != nil {
		return failSequenceWorkflow(onEvent, err)
	}
	if onEvent != nil {
		onEvent(workflow.WorkflowEvent{
			Type:   workflow.WorkflowDone,
			Result: fmt.Sprintf("%s completed: %s", kiroSpecImplLoopSequenceID, strings.Join(selected, ", ")),
		})
	}
	return nil
}

func (r sequenceRunner) selectFeatures(ctx context.Context, cwd string, ask workflow.WorkflowAsk) ([]string, bool, error) {
	targets, err := listSpecImplLoopTargets(cwd)
	if err != nil {
		return nil, false, err
	}
	return workflow.SelectOrdered(ctx, ask, workflow.OrderedSelectParams{
		Key:             "features",
		Prompt:          "実行する Kiro feature を順序付きで選択してください",
		Items:           sequenceFeatureSelectItems(targets),
		MinItems:        1,
		DuplicatePolicy: workflow.DuplicatePolicyReject,
	})
}

func sequenceFeatureSelectItems(targets []specImplLoopTarget) []workflow.SelectItem {
	items := make([]workflow.SelectItem, 0, len(targets))
	for _, target := range targets {
		items = append(items, workflow.SelectItem{
			ID:    target.Feature,
			Label: specImplLoopTargetLabel(target),
		})
	}
	return items
}

func failSequenceWorkflow(onEvent func(workflow.WorkflowEvent), err error) error {
	if err == nil {
		return nil
	}
	emitSequenceWorkflowError(onEvent, err.Error())
	return err
}

func emitSequenceWorkflowError(onEvent func(workflow.WorkflowEvent), message string) {
	if onEvent != nil {
		onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: message})
	}
}

func classifySequenceChildFailure(result workflow.ChildResult, err error) (string, string) {
	message := ""
	if err != nil {
		message = err.Error()
	}
	if isSequenceDeliveryFailure(err) || isSequenceDeliveryFailure(result.ReturnedError) {
		return sequenceFailureDeliveryFailed, message
	}
	if result.ReturnedError != nil {
		return sequenceFailureChildReturned, result.ReturnedError.Error()
	}
	if result.FirstWorkflowError != nil {
		return sequenceFailureChildWorkflow, result.FirstWorkflowError.Message
	}
	return sequenceFailureChildReturned, message
}

func isSequenceDeliveryFailure(err error) bool {
	var deliveryErr sequenceDeliveryError
	return errors.As(err, &deliveryErr)
}

type sequenceDeliveryError struct {
	err error
}

func newSequenceDeliveryError(format string, args ...any) error {
	return sequenceDeliveryError{
		err: fmt.Errorf("kiro-spec-impl-loop-sequence delivery failure: "+format, args...),
	}
}

func (e sequenceDeliveryError) Error() string {
	if e.err == nil {
		return "kiro-spec-impl-loop-sequence delivery failure"
	}
	return e.err.Error()
}

func (e sequenceDeliveryError) Unwrap() error {
	if e.err == nil {
		return nil
	}
	return errors.Unwrap(e.err)
}

type sequenceFeatureAsk struct {
	feature  string
	delegate workflow.WorkflowAsk
}

func (a sequenceFeatureAsk) Select(ctx context.Context, params workflow.SelectParams) (string, bool, error) {
	feature := strings.TrimSpace(a.feature)
	if params.Key == "feature" {
		if feature == "" {
			return "", false, newSequenceDeliveryError("feature is empty; no fallback to another feature")
		}
		if !selectParamsContainFeature(params.Items, feature) {
			return "", false, newSequenceDeliveryError("feature=%q is not present in child selection items; no fallback to another feature", feature)
		}
		return feature, true, nil
	}
	if a.delegate == nil {
		return "", false, fmt.Errorf("kiro-spec-impl-loop-sequence feature ask cannot delegate selection key %q", params.Key)
	}
	return a.delegate.Select(ctx, params)
}

func prepareSequenceFeatureDelivery(ctx context.Context, cwd string, ask workflow.WorkflowAsk, feature string) (context.Context, workflow.WorkflowAsk, error) {
	feature = strings.TrimSpace(feature)
	if feature == "" {
		return ctx, ask, newSequenceDeliveryError("feature is empty; no fallback to another feature")
	}

	targets, err := listSpecImplLoopTargets(cwd)
	if err != nil {
		return ctx, ask, newSequenceDeliveryError("feature=%q: %w", feature, err)
	}
	if _, ok := findSpecImplLoopTarget(targets, feature); !ok {
		return ctx, ask, newSequenceDeliveryError("feature=%q is not selectable (available: %s); no fallback to another feature", feature, strings.Join(specImplLoopTargetFeatures(targets), ", "))
	}

	return ctx, sequenceFeatureAsk{feature: feature, delegate: ask}, nil
}

func runSequenceChildWithFixedFeature(
	ctx context.Context,
	cwd string,
	child workflow.Definition,
	feature string,
	onEvent func(workflow.WorkflowEvent),
	ask workflow.WorkflowAsk,
) (workflow.ChildResult, error) {
	childCtx, childAsk, err := prepareSequenceFeatureDelivery(ctx, cwd, ask, feature)
	if err != nil {
		return workflow.ChildResult{}, err
	}
	return workflow.RunChild(childCtx, cwd, child, onEvent, childAsk, workflow.ChildRunOptions{})
}
