package workflow

import (
	"context"
	"errors"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/telemetry"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

type definitionRunResult struct {
	ChildResult
}

type definitionRunOptions struct {
	Capture EventCapturePolicy
}

func runDefinitionWithResult(ctx context.Context, d Definition, cwd string, onEvent func(WorkflowEvent), ask WorkflowAsk, options definitionRunOptions) definitionRunResult {
	runID := generateRunID()
	startedAt := time.Now().UTC()
	projectID := akashic.ProjectIDFromCWD(cwd)
	telemetryID := telemetry.WorkflowIdentity{
		WorkflowRunID: runID,
		WorkflowID:    d.ID,
		WorkflowName:  d.Label,
		ProjectID:     projectID,
	}
	ctx = workflowctx.ContextWithRunMetadata(ctx, workflowctx.RunMetadata{
		WorkflowRunID: runID,
		WorkflowID:    d.ID,
		WorkflowName:  d.Label,
		CWD:           cwd,
	})
	// d.Recorder == nil であっても常に ctx に注入する。
	// これにより recorderFromContext は「未注入」と「nil 注入」を一意の (nil, false) として扱える。
	ctx = ContextWithRecorder(ctx, d.Recorder)
	ctx, span := telemetry.StartWorkflowRun(ctx, telemetryID)
	if d.Recorder != nil {
		_ = d.Recorder.RecordWorkflowRunStart(context.WithoutCancel(ctx), akashic.WorkflowRun{
			ID:           runID,
			WorkflowID:   d.ID,
			WorkflowName: d.Label,
			CWD:          cwd,
			ProjectID:    projectID,
			StartedAt:    startedAt,
		})
	}

	var workflowErr *WorkflowEvent
	var capturedEvents []WorkflowEvent
	captureLimitReached := false
	capturePolicy := options.Capture
	wrappedOnEvent := func(event WorkflowEvent) {
		// event.RunID 不変式 (review-apply-workflow tasks.md 6.1 / Req 1.2, 8.5):
		//   - 子 Run が先に runID を埋めた経路では既存値を保持する (ネスト合成で内側 runID 優先)。
		//   - 空のときに限り自身の runID を埋め、Sequential ラッパ側が `WorkflowEvent.RunID`
		//     を傍受して後段 ctx に hand-off できる契約を守る。
		if event.RunID == "" {
			event.RunID = runID
		}
		if event.Type == WorkflowError && workflowErr == nil {
			captured := event
			workflowErr = &captured
		}
		if capturePolicy.active() && capturePolicy.Predicate(event) {
			if len(capturedEvents) < capturePolicy.MaxEvents {
				capturedEvents = append(capturedEvents, event)
			} else {
				captureLimitReached = true
			}
		}
		if onEvent != nil {
			onEvent(event)
		}
	}

	err := d.Fn(ctx, cwd, wrappedOnEvent, ask)
	var telemetryErr error
	if d.Recorder != nil {
		errorMessage := ""
		if err != nil {
			errorMessage = err.Error()
		} else if workflowErr != nil {
			errorMessage = workflowErr.Message
		}
		_ = d.Recorder.RecordWorkflowRunFinish(context.WithoutCancel(ctx), akashic.WorkflowRunFinish{
			ID:         runID,
			Error:      errorMessage,
			FinishedAt: time.Now().UTC(),
		})
	}
	if err != nil {
		telemetryErr = err
	} else if workflowErr != nil {
		telemetryErr = errors.New(workflowErr.Message)
	}
	telemetry.EndWorkflowRun(ctx, span, telemetryID, startedAt, telemetryErr)

	return definitionRunResult{
		ChildResult: ChildResult{
			DefinitionID:              d.ID,
			Label:                     d.Label,
			RunID:                     runID,
			ReturnedError:             err,
			FirstWorkflowError:        workflowErr,
			CapturedEvents:            capturedEvents,
			CapturedEventLimitReached: captureLimitReached,
		},
	}
}
