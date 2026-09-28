package workflow

import (
	"context"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

func TestForwardEventsPreservesReasoningAsDedicatedWorkflowEvent(t *testing.T) {
	var got []WorkflowEvent
	forward := ForwardEvents(func(event WorkflowEvent) {
		got = append(got, event)
	})

	forward(provider.AgentEvent{
		Type:   provider.EventReasoning,
		Text:   "Inspecting the implementation path",
		ItemID: "reasoning-1",
	})

	if len(got) != 1 {
		t.Fatalf("forwarded event count = %d, want 1: %#v", len(got), got)
	}
	if got[0].Type != WorkflowReasoning || got[0].Text != "Inspecting the implementation path" || got[0].ItemID != "reasoning-1" {
		t.Fatalf("reasoning event was not preserved: %#v", got[0])
	}
}

// Definition.Run が呼び出された後、ctx 内 RunMetadata.CWD が
// Run 引数の cwd と一致することを確認する (Task 1.3 観測条件 (a))。
func TestDefinitionRunPopulatesCWDInRunMetadata(t *testing.T) {
	var captured workflowctx.RunMetadata
	var captureOK bool
	cwd := t.TempDir()

	def := Definition{
		ID:    "ctx-cwd-test",
		Label: "Ctx CWD Test",
		Fn: func(ctx context.Context, _ string, _ func(WorkflowEvent), _ WorkflowAsk) error {
			captured, captureOK = workflowctx.RunMetadataFromContext(ctx)
			return nil
		},
	}

	if err := def.Run(context.Background(), cwd, func(WorkflowEvent) {}, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if !captureOK {
		t.Fatalf("expected RunMetadata in ctx, but missing")
	}
	if captured.CWD != cwd {
		t.Fatalf("expected CWD=%q in RunMetadata, got %q", cwd, captured.CWD)
	}
}

func TestDefinitionRunRecordsStartFinishAndInjectsMatchingRunMetadata(t *testing.T) {
	recorder := &workflowRecorder{}
	cwd := t.TempDir()
	var captured workflowctx.RunMetadata
	var captureOK bool
	var capturedRecorder akashic.Recorder
	var recorderOK bool

	def := Definition{
		ID:       "definition-run-contract",
		Label:    "Definition Run Contract",
		Recorder: recorder,
		Fn: func(ctx context.Context, gotCWD string, _ func(WorkflowEvent), _ WorkflowAsk) error {
			if gotCWD != cwd {
				t.Fatalf("Fn cwd = %q, want %q", gotCWD, cwd)
			}
			captured, captureOK = workflowctx.RunMetadataFromContext(ctx)
			capturedRecorder, recorderOK = recorderFromContext(ctx)
			return nil
		},
	}

	if err := def.Run(context.Background(), cwd, func(WorkflowEvent) {}, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(recorder.starts) != 1 {
		t.Fatalf("RecordWorkflowRunStart count = %d, want 1", len(recorder.starts))
	}
	if len(recorder.finishes) != 1 {
		t.Fatalf("RecordWorkflowRunFinish count = %d, want 1", len(recorder.finishes))
	}
	start := recorder.starts[0]
	finish := recorder.finishes[0]
	if start.ID == "" {
		t.Fatalf("start.ID is empty")
	}
	if finish.ID != start.ID {
		t.Fatalf("finish.ID = %q, want same runID as start.ID %q", finish.ID, start.ID)
	}
	if start.WorkflowID != def.ID || start.WorkflowName != def.Label || start.CWD != cwd {
		t.Fatalf("start metadata = %#v, want workflow ID/name/cwd from Definition.Run", start)
	}
	if start.ProjectID != akashic.ProjectIDFromCWD(cwd) {
		t.Fatalf("start.ProjectID = %q, want %q", start.ProjectID, akashic.ProjectIDFromCWD(cwd))
	}
	if start.StartedAt.IsZero() {
		t.Fatalf("start.StartedAt is zero")
	}
	if finish.FinishedAt.IsZero() {
		t.Fatalf("finish.FinishedAt is zero")
	}
	if finish.Error != "" {
		t.Fatalf("finish.Error = %q, want empty", finish.Error)
	}
	if !captureOK {
		t.Fatalf("expected RunMetadata in Fn context")
	}
	if captured.WorkflowRunID != start.ID || captured.WorkflowID != def.ID || captured.WorkflowName != def.Label || captured.CWD != cwd {
		t.Fatalf("captured RunMetadata = %#v, want runID=%q workflow=%q label=%q cwd=%q", captured, start.ID, def.ID, def.Label, cwd)
	}
	if !recorderOK || capturedRecorder != recorder {
		t.Fatalf("captured recorder = %#v ok=%v, want injected recorder %#v", capturedRecorder, recorderOK, recorder)
	}
}

func TestDefinitionRunInjectsRunIDOnlyForEventsWithoutRunID(t *testing.T) {
	var runID string
	var events []WorkflowEvent
	def := Definition{
		ID:    "definition-run-events",
		Label: "Definition Run Events",
		Fn: func(ctx context.Context, _ string, onEvent func(WorkflowEvent), _ WorkflowAsk) error {
			metadata, ok := workflowctx.RunMetadataFromContext(ctx)
			if !ok {
				t.Fatalf("expected RunMetadata in Fn context")
			}
			runID = metadata.WorkflowRunID
			onEvent(WorkflowEvent{Type: WorkflowText, Text: "fill me"})
			onEvent(WorkflowEvent{Type: WorkflowText, Text: "keep me", RunID: "upstream-run"})
			return nil
		},
	}

	if err := def.Run(context.Background(), t.TempDir(), func(event WorkflowEvent) {
		events = append(events, event)
	}, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if runID == "" {
		t.Fatalf("captured runID is empty")
	}
	if len(events) != 2 {
		t.Fatalf("events count = %d, want 2: %#v", len(events), events)
	}
	if events[0].RunID != runID {
		t.Fatalf("events[0].RunID = %q, want current runID %q", events[0].RunID, runID)
	}
	if events[1].RunID != "upstream-run" {
		t.Fatalf("events[1].RunID = %q, want preserved upstream runID", events[1].RunID)
	}
}

func TestDefinitionRunRecordsFirstWorkflowErrorEventAsFinishError(t *testing.T) {
	recorder := &workflowRecorder{}
	def := Definition{
		ID:       "definition-run-logical-failure",
		Label:    "Definition Run Logical Failure",
		Recorder: recorder,
		Fn: func(_ context.Context, _ string, onEvent func(WorkflowEvent), _ WorkflowAsk) error {
			onEvent(WorkflowEvent{Type: WorkflowError, Message: "first logical failure"})
			onEvent(WorkflowEvent{Type: WorkflowError, Message: "second logical failure"})
			return nil
		},
	}

	if err := def.Run(context.Background(), t.TempDir(), func(WorkflowEvent) {}, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(recorder.finishes) != 1 {
		t.Fatalf("RecordWorkflowRunFinish count = %d, want 1", len(recorder.finishes))
	}
	if recorder.finishes[0].Error != "first logical failure" {
		t.Fatalf("finish.Error = %q, want first WorkflowError message", recorder.finishes[0].Error)
	}
}

// recorderFromContext は ctx に key 未注入のとき (nil, false) を返す
// (Task 1.3 観測条件 (b) ケース 1)。
func TestRecorderFromContextReturnsFalseWhenKeyMissing(t *testing.T) {
	ctx := context.Background()

	rec, ok := recorderFromContext(ctx)

	if ok {
		t.Fatalf("expected ok=false for missing key, got rec=%v ok=true", rec)
	}
	if rec != nil {
		t.Fatalf("expected rec=nil for missing key, got %v", rec)
	}
}

// recorderFromContext は nil 注入された場合も (nil, false) を返す
// (Task 1.3 観測条件 (b) ケース 2)。
func TestRecorderFromContextReturnsFalseWhenNilInjected(t *testing.T) {
	ctx := ContextWithRecorder(context.Background(), nil)

	rec, ok := recorderFromContext(ctx)

	if ok {
		t.Fatalf("expected ok=false for nil-injected, got rec=%v ok=true", rec)
	}
	if rec != nil {
		t.Fatalf("expected rec=nil for nil-injected, got %v", rec)
	}
}

// recorderFromContext は非 nil recorder 注入時に (recorder, true) を返す
// (Task 1.3 観測条件 (b) ケース 3)。
func TestRecorderFromContextReturnsRecorderWhenInjected(t *testing.T) {
	var injected akashic.Recorder = akashic.NoopRecorder{}
	ctx := ContextWithRecorder(context.Background(), injected)

	rec, ok := recorderFromContext(ctx)

	if !ok {
		t.Fatalf("expected ok=true for non-nil recorder, got rec=%v ok=false", rec)
	}
	if rec != injected {
		t.Fatalf("expected rec=%v, got %v", injected, rec)
	}
}

// Definition.Run は d.Recorder == nil でも ContextWithRecorder を呼ぶ。
// このため Fn 内から recorderFromContext を呼ぶと一意の "Recorder 無し" シグナル
// (nil, false) が得られる (Task 1.3 設計上の不変式)。
func TestDefinitionRunInjectsRecorderEvenWhenNil(t *testing.T) {
	var capturedRec akashic.Recorder
	var capturedOK bool

	def := Definition{
		ID:       "ctx-recorder-nil-test",
		Label:    "Ctx Recorder Nil Test",
		Recorder: nil, // 明示的に nil
		Fn: func(ctx context.Context, _ string, _ func(WorkflowEvent), _ WorkflowAsk) error {
			capturedRec, capturedOK = recorderFromContext(ctx)
			return nil
		},
	}

	if err := def.Run(context.Background(), t.TempDir(), func(WorkflowEvent) {}, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if capturedOK {
		t.Fatalf("expected ok=false for nil Recorder injection, got rec=%v ok=true", capturedRec)
	}
	if capturedRec != nil {
		t.Fatalf("expected nil recorder, got %v", capturedRec)
	}
}

// Definition.Run は d.Recorder が非 nil の場合、ctx 経由で recorder を参照可能にする。
func TestDefinitionRunInjectsRecorderWhenProvided(t *testing.T) {
	var injected akashic.Recorder = akashic.NoopRecorder{}
	var capturedRec akashic.Recorder
	var capturedOK bool

	def := Definition{
		ID:       "ctx-recorder-injected-test",
		Label:    "Ctx Recorder Injected Test",
		Recorder: injected,
		Fn: func(ctx context.Context, _ string, _ func(WorkflowEvent), _ WorkflowAsk) error {
			capturedRec, capturedOK = recorderFromContext(ctx)
			return nil
		},
	}

	if err := def.Run(context.Background(), t.TempDir(), func(WorkflowEvent) {}, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if !capturedOK {
		t.Fatalf("expected ok=true for non-nil Recorder injection, got rec=%v ok=false", capturedRec)
	}
	if capturedRec != injected {
		t.Fatalf("expected captured recorder to be the injected one, got %v", capturedRec)
	}
}

// --- Task 10.1: AkashicReaderProvider / AkashicArtifactReaderProvider seam ---
//
// 設計規約 (design.md §workflow/optional iface):
//   - agent が optional interface を実装しない → helper は nil
//   - 実装していてもメソッドが nil を返す → helper も nil
//   - 実装かつ非 nil → そのまま返す
//
// 既存 recorderFromAgent と完全に同パターン。

// agentNoProvider は AgentCaller のみ実装する最小ダミー (provider 系 interface 未実装)。
type agentNoProvider struct{}

func (agentNoProvider) CallAgent(_ context.Context, _ string, _ string, _ provider.AgentEventCallback, _ string, _ *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
	return provider.AgentResponse{}, nil
}

// agentWithReaders は AkashicReaderProvider と AkashicArtifactReaderProvider を実装する。
type agentWithReaders struct {
	agentNoProvider
	reader   akashic.Reader
	artifact akashic.ArtifactReader
}

func (a agentWithReaders) AkashicReader() akashic.Reader                 { return a.reader }
func (a agentWithReaders) AkashicArtifactReader() akashic.ArtifactReader { return a.artifact }

// fakeReader は akashic.Reader を満たすだけのダミー (Identity 比較用)。
type fakeReader struct{ akashic.Reader }

// fakeArtifactReader は akashic.ArtifactReader を満たすだけのダミー (Identity 比較用)。
type fakeArtifactReader struct{ akashic.ArtifactReader }

func TestReaderFromAgent_ReturnsNilWhenProviderNotImplemented(t *testing.T) {
	got := ReaderFromAgent(agentNoProvider{})
	if got != nil {
		t.Fatalf("expected nil reader for non-provider agent, got %#v", got)
	}
}

func TestReaderFromAgent_ReturnsNilWhenProviderReturnsNil(t *testing.T) {
	got := ReaderFromAgent(agentWithReaders{reader: nil})
	if got != nil {
		t.Fatalf("expected nil reader when provider returns nil, got %#v", got)
	}
}

func TestReaderFromAgent_ReturnsReaderWhenProvided(t *testing.T) {
	r := fakeReader{}
	got := ReaderFromAgent(agentWithReaders{reader: r})
	if got == nil {
		t.Fatalf("expected non-nil reader when provider returns one")
	}
	if got != r {
		t.Fatalf("expected returned reader to be the injected one (identity), got %#v", got)
	}
}

func TestArtifactReaderFromAgent_ReturnsNilWhenProviderNotImplemented(t *testing.T) {
	got := ArtifactReaderFromAgent(agentNoProvider{})
	if got != nil {
		t.Fatalf("expected nil artifact reader for non-provider agent, got %#v", got)
	}
}

func TestArtifactReaderFromAgent_ReturnsNilWhenProviderReturnsNil(t *testing.T) {
	got := ArtifactReaderFromAgent(agentWithReaders{artifact: nil})
	if got != nil {
		t.Fatalf("expected nil artifact reader when provider returns nil, got %#v", got)
	}
}

func TestArtifactReaderFromAgent_ReturnsArtifactReaderWhenProvided(t *testing.T) {
	a := fakeArtifactReader{}
	got := ArtifactReaderFromAgent(agentWithReaders{artifact: a})
	if got == nil {
		t.Fatalf("expected non-nil artifact reader when provider returns one")
	}
	if got != a {
		t.Fatalf("expected returned artifact reader to be the injected one (identity), got %#v", got)
	}
}
