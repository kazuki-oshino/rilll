package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

func TestRunDefinitionWithResultReturnsEventlessRunIDAndKeepsRunMetadataParity(t *testing.T) {
	recorder := &workflowRecorder{}
	cwd := t.TempDir()
	var captured workflowctx.RunMetadata
	var captureOK bool
	def := Definition{
		ID:       "definition-run-core",
		Label:    "Definition Run Core",
		Recorder: recorder,
		Fn: func(ctx context.Context, gotCWD string, _ func(WorkflowEvent), _ WorkflowAsk) error {
			if gotCWD != cwd {
				t.Fatalf("Fn cwd = %q, want %q", gotCWD, cwd)
			}
			captured, captureOK = workflowctx.RunMetadataFromContext(ctx)
			return nil
		},
	}

	result := runDefinitionWithResult(context.Background(), def, cwd, func(WorkflowEvent) {}, nil, definitionRunOptions{})

	if result.RunID == "" {
		t.Fatalf("result.RunID is empty")
	}
	if result.ReturnedError != nil {
		t.Fatalf("result.ReturnedError = %v, want nil", result.ReturnedError)
	}
	if result.FirstWorkflowError != nil {
		t.Fatalf("result.FirstWorkflowError = %#v, want nil", result.FirstWorkflowError)
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
	if !captureOK {
		t.Fatalf("expected RunMetadata in Fn context")
	}
	if captured.WorkflowRunID != result.RunID {
		t.Fatalf("captured WorkflowRunID = %q, want result.RunID %q", captured.WorkflowRunID, result.RunID)
	}
	if captured.WorkflowID != def.ID || captured.WorkflowName != def.Label || captured.CWD != cwd {
		t.Fatalf("captured RunMetadata = %#v, want workflow ID/name/cwd from Definition", captured)
	}
}

func TestRunDefinitionWithResultCapturesReturnedErrorAndFinishError(t *testing.T) {
	recorder := &workflowRecorder{}
	wantErr := errors.New("returned failure")
	def := Definition{
		ID:       "definition-run-core-returned-error",
		Label:    "Definition Run Core Returned Error",
		Recorder: recorder,
		Fn: func(context.Context, string, func(WorkflowEvent), WorkflowAsk) error {
			return wantErr
		},
	}

	result := runDefinitionWithResult(context.Background(), def, t.TempDir(), func(WorkflowEvent) {}, nil, definitionRunOptions{})

	if !errors.Is(result.ReturnedError, wantErr) {
		t.Fatalf("result.ReturnedError = %v, want errors.Is returned failure", result.ReturnedError)
	}
	if result.FirstWorkflowError != nil {
		t.Fatalf("result.FirstWorkflowError = %#v, want nil", result.FirstWorkflowError)
	}
	if len(recorder.finishes) != 1 {
		t.Fatalf("RecordWorkflowRunFinish count = %d, want 1", len(recorder.finishes))
	}
	if recorder.finishes[0].Error != wantErr.Error() {
		t.Fatalf("finish.Error = %q, want returned error %q", recorder.finishes[0].Error, wantErr.Error())
	}
}

func TestRunDefinitionWithResultCapturesFirstWorkflowErrorWithRunID(t *testing.T) {
	recorder := &workflowRecorder{}
	var events []WorkflowEvent
	def := Definition{
		ID:       "definition-run-core-workflow-error",
		Label:    "Definition Run Core Workflow Error",
		Recorder: recorder,
		Fn: func(_ context.Context, _ string, onEvent func(WorkflowEvent), _ WorkflowAsk) error {
			onEvent(WorkflowEvent{Type: WorkflowError, Message: "first logical failure"})
			onEvent(WorkflowEvent{Type: WorkflowError, Message: "second logical failure"})
			return nil
		},
	}

	result := runDefinitionWithResult(context.Background(), def, t.TempDir(), func(event WorkflowEvent) {
		events = append(events, event)
	}, nil, definitionRunOptions{})

	if result.ReturnedError != nil {
		t.Fatalf("result.ReturnedError = %v, want nil", result.ReturnedError)
	}
	if result.FirstWorkflowError == nil {
		t.Fatalf("result.FirstWorkflowError is nil, want first WorkflowError event")
	}
	if result.FirstWorkflowError.Message != "first logical failure" {
		t.Fatalf("result.FirstWorkflowError.Message = %q, want first WorkflowError message", result.FirstWorkflowError.Message)
	}
	if result.FirstWorkflowError.RunID != result.RunID {
		t.Fatalf("result.FirstWorkflowError.RunID = %q, want result.RunID %q", result.FirstWorkflowError.RunID, result.RunID)
	}
	if len(events) != 2 {
		t.Fatalf("events count = %d, want 2", len(events))
	}
	if events[0].RunID != result.RunID || events[1].RunID != result.RunID {
		t.Fatalf("forwarded event runIDs = %q, %q; want %q", events[0].RunID, events[1].RunID, result.RunID)
	}
	if len(recorder.finishes) != 1 {
		t.Fatalf("RecordWorkflowRunFinish count = %d, want 1", len(recorder.finishes))
	}
	if recorder.finishes[0].Error != "first logical failure" {
		t.Fatalf("finish.Error = %q, want first WorkflowError message", recorder.finishes[0].Error)
	}
}

func TestDefinitionRunReturnsSharedCoreReturnedErrorOnly(t *testing.T) {
	workflowErrOnly := Definition{
		ID:    "definition-run-core-workflow-error-only",
		Label: "Definition Run Core Workflow Error Only",
		Fn: func(_ context.Context, _ string, onEvent func(WorkflowEvent), _ WorkflowAsk) error {
			onEvent(WorkflowEvent{Type: WorkflowError, Message: "logical failure"})
			return nil
		},
	}
	if err := workflowErrOnly.Run(context.Background(), t.TempDir(), func(WorkflowEvent) {}, nil); err != nil {
		t.Fatalf("Run returned %v, want nil when Fn returned nil", err)
	}

	wantErr := errors.New("returned failure")
	returnedError := Definition{
		ID:    "definition-run-core-returned-error-only",
		Label: "Definition Run Core Returned Error Only",
		Fn: func(context.Context, string, func(WorkflowEvent), WorkflowAsk) error {
			return wantErr
		},
	}
	if err := returnedError.Run(context.Background(), t.TempDir(), func(WorkflowEvent) {}, nil); !errors.Is(err, wantErr) {
		t.Fatalf("Run returned %v, want returned failure", err)
	}
}

func TestRunDefinitionWithResultInjectsNilRecorderInvariant(t *testing.T) {
	var captured akashic.Recorder
	var capturedOK bool
	def := Definition{
		ID:       "definition-run-core-nil-recorder",
		Label:    "Definition Run Core Nil Recorder",
		Recorder: nil,
		Fn: func(ctx context.Context, _ string, _ func(WorkflowEvent), _ WorkflowAsk) error {
			captured, capturedOK = recorderFromContext(ctx)
			return nil
		},
	}

	result := runDefinitionWithResult(context.Background(), def, t.TempDir(), func(WorkflowEvent) {}, nil, definitionRunOptions{})

	if result.RunID == "" {
		t.Fatalf("result.RunID is empty")
	}
	if capturedOK {
		t.Fatalf("expected ok=false for nil recorder injection, got rec=%v ok=true", captured)
	}
	if captured != nil {
		t.Fatalf("expected nil recorder, got %v", captured)
	}
}
