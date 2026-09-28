package workflow

import (
	"context"
	"errors"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// ChildRunOptions は child workflow 実行時の補助 contract を指定する。
type ChildRunOptions struct {
	Capture EventCapturePolicy
}

// RunChild は input delivery 済みの child workflow を実行し、completion result を返す。
func RunChild(
	ctx context.Context,
	cwd string,
	def Definition,
	onEvent func(WorkflowEvent),
	ask WorkflowAsk,
	options ChildRunOptions,
) (ChildResult, error) {
	if onEvent != nil {
		onEvent(WorkflowEvent{Type: WorkflowActive, Label: def.Label})
	}

	result := runDefinitionWithResult(ctx, def, cwd, onEvent, ask, definitionRunOptions{
		Capture: options.Capture,
	}).ChildResult
	recordWorkflowRunChild(ctx, def, result)
	if result.ReturnedError != nil {
		return result, result.ReturnedError
	}
	if result.FirstWorkflowError != nil {
		return result, errors.New(result.FirstWorkflowError.Message)
	}
	return result, nil
}

type workflowRunChildRecorder interface {
	RecordWorkflowRunChild(context.Context, akashic.WorkflowRunChild) error
}

func recordWorkflowRunChild(ctx context.Context, def Definition, result ChildResult) {
	parent, ok := workflowctx.RunMetadataFromContext(ctx)
	if !ok || parent.WorkflowRunID == "" || result.RunID == "" || parent.WorkflowRunID == result.RunID {
		return
	}
	var recorder akashic.Recorder
	if def.Recorder != nil {
		recorder = def.Recorder
	} else if ctxRecorder, ok := recorderFromContext(ctx); ok {
		recorder = ctxRecorder
	}
	lineageRecorder, ok := recorder.(workflowRunChildRecorder)
	if !ok {
		return
	}
	childWorkflowID := result.DefinitionID
	if childWorkflowID == "" {
		childWorkflowID = def.ID
	}
	childWorkflowName := result.Label
	if childWorkflowName == "" {
		childWorkflowName = def.Label
	}
	_ = lineageRecorder.RecordWorkflowRunChild(context.WithoutCancel(ctx), akashic.WorkflowRunChild{
		ParentRunID:       parent.WorkflowRunID,
		ChildRunID:        result.RunID,
		ChildWorkflowID:   childWorkflowID,
		ChildWorkflowName: childWorkflowName,
		RecordedAt:        time.Now().UTC(),
	})
}
