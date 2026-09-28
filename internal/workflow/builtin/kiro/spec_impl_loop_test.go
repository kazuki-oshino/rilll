package kiro

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/workflow"
)

func TestKiroSpecImplLoopDefinitionContract(t *testing.T) {
	recorder := &workflowRecorder{}
	agent := &fakeAgent{recorder: recorder}

	def := KiroSpecImplLoop(agent)

	if def.ID != "kiro-spec-impl-loop" {
		t.Errorf("Definition.ID=%q, want %q", def.ID, "kiro-spec-impl-loop")
	}
	if def.Label != "Kiro 仕様作成・実装ループ" {
		t.Errorf("Definition.Label=%q, want %q", def.Label, "Kiro 仕様作成・実装ループ")
	}
	if def.Hidden {
		t.Errorf("Definition.Hidden=true, want false")
	}
	if def.Recorder != recorder {
		t.Errorf("Definition.Recorder=%#v, want agent recorder %#v", def.Recorder, recorder)
	}
	if def.Fn == nil {
		t.Fatalf("Definition.Fn is nil")
	}

	if len(def.Args) != 1 {
		t.Fatalf("len(Definition.Args)=%d, want 1", len(def.Args))
	}
	featureArg := def.Args[0]
	if featureArg.Key != "feature" {
		t.Errorf("feature Arg Key=%q, want %q", featureArg.Key, "feature")
	}
	if !featureArg.Required {
		t.Errorf("feature Arg Required=false, want true")
	}
	if !strings.Contains(featureArg.Description, "spec 作成対象") || !strings.Contains(featureArg.Description, "実装対象") {
		t.Errorf("feature Arg Description=%q, want spec and impl target wording", featureArg.Description)
	}
}

func TestKiroSpecImplLoopSuccessHandsOffCompletedSpecFeatureToImplChild(t *testing.T) {
	cwd := t.TempDir()
	specFeature := "feature-under-spec"
	otherFeature := "other-ready-feature"
	writeFeature(t, cwd, specFeature, map[string]string{
		"brief.md": "feature brief\n",
	})
	writeFeature(t, cwd, otherFeature, map[string]string{
		"spec.json": validImplSpecJSON,
		"tasks.md":  "- [ ] 1. pending\n",
	})

	specDef := workflow.Definition{
		ID:    "kiro-spec-loop",
		Label: "Kiro 仕様作成ループ",
		Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			runID := workflow.RunIDFromContext(ctx)
			resultPath := filepath.Join(cwd, ".rilll", "kiro-spec-loop", runID, "result.json")
			if err := os.MkdirAll(filepath.Dir(resultPath), 0o755); err != nil {
				return err
			}
			result := RunResult{
				RunID:             runID,
				WorkflowID:        "kiro-spec-loop",
				Feature:           specFeature,
				TerminationReason: RunReasonCompleted,
				Steps:             []RunResultStep{},
			}
			raw, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				return err
			}
			if err := os.WriteFile(resultPath, raw, 0o644); err != nil {
				return err
			}
			writeFeature(t, cwd, specFeature, map[string]string{
				"spec.json": validImplSpecJSON,
				"tasks.md":  "- [ ] 1. pending\n",
			})
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "spec stage progress"})
			return nil
		},
	}

	var implSelectedFeature string
	implDef := workflow.Definition{
		ID:    "kiro-impl-loop",
		Label: "Kiro 実装ループ",
		Fn: func(ctx context.Context, _ string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			if ask == nil {
				return errors.New("impl child received nil ask")
			}
			selected, ok, err := ask.Select(ctx, workflow.SelectParams{
				Key:    "feature",
				Prompt: "impl feature",
				Items: []workflow.SelectItem{
					{ID: otherFeature, Label: otherFeature},
					{ID: specFeature, Label: specFeature},
				},
			})
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("impl child feature selection was cancelled")
			}
			implSelectedFeature = selected
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "impl stage progress"})
			return nil
		},
	}

	parentAsk := &recordingSpecImplLoopAsk{selected: specFeature, ok: true}
	def := buildKiroSpecImplLoop(&fakeAgent{}, specDef, implDef)
	var events []workflow.WorkflowEvent
	err := def.Run(context.Background(), cwd, func(event workflow.WorkflowEvent) {
		events = append(events, event)
	}, parentAsk)
	if err != nil {
		t.Fatalf("KiroSpecImplLoop Run error = %v", err)
	}

	if implSelectedFeature != specFeature {
		t.Fatalf("impl selected feature = %q, want completed spec feature %q", implSelectedFeature, specFeature)
	}
	if len(parentAsk.calls) != 1 {
		t.Fatalf("parent ask Select calls = %d, want 1 (parent selects target once)", len(parentAsk.calls))
	}
	if parentAsk.calls[0].Key != "feature" {
		t.Fatalf("parent ask Select key = %q, want feature", parentAsk.calls[0].Key)
	}

	wantEventOrder := []string{
		"active:Kiro 仕様作成ループ",
		"text:spec stage progress",
		"active:Kiro 実装ループ",
		"text:impl stage progress",
	}
	if got := specImplLoopEventOrder(events); !reflect.DeepEqual(got, wantEventOrder) {
		t.Fatalf("event order = %v, want %v", got, wantEventOrder)
	}

	specRunID := runIDForTextEvent(events, "spec stage progress")
	if specRunID == "" {
		t.Fatalf("spec child progress event did not carry child RunID: %+v", events)
	}
	resultPath := filepath.Join(cwd, ".rilll", "kiro-spec-loop", specRunID, "result.json")
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("spec result artifact traceability failed: %v", err)
	}
	var result RunResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("spec result artifact must be valid JSON: %v", err)
	}
	if result.Feature != specFeature || result.RunID != specRunID || result.TerminationReason != RunReasonCompleted {
		t.Fatalf("spec result = %+v, want feature=%q runID=%q termination=%q", result, specFeature, specRunID, RunReasonCompleted)
	}
}

func TestListSpecImplLoopTargetsUnionsSpecEligibleAndImplementableFeatures(t *testing.T) {
	cwd := t.TempDir()
	writeFeature(t, cwd, "needs-spec", map[string]string{
		"brief.md": "needs spec brief\n",
	})
	writeFeature(t, cwd, "ready-impl", map[string]string{
		"spec.json": validImplSpecJSON,
		"tasks.md":  "- [ ] 1. pending\n",
	})
	writeFeature(t, cwd, "both-prefers-impl", map[string]string{
		"brief.md": "brief\n",
		"spec.json": `{
		  "phase": "design-generated",
		  "ready_for_implementation": true,
		  "approvals": {
		    "requirements": {"approved": true},
		    "design": {"approved": true},
		    "tasks": {"approved": true}
		  }
		}`,
		"tasks.md": "- [ ] 1. pending\n",
	})

	got, err := listSpecImplLoopTargets(cwd)
	if err != nil {
		t.Fatalf("listSpecImplLoopTargets error = %v", err)
	}
	want := []specImplLoopTarget{
		{Feature: "both-prefers-impl", Mode: specImplLoopTargetModeImplOnly},
		{Feature: "needs-spec", Mode: specImplLoopTargetModeSpecThenImpl},
		{Feature: "ready-impl", Mode: specImplLoopTargetModeImplOnly},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listSpecImplLoopTargets = %#v, want %#v", got, want)
	}
}

func TestKiroSpecImplLoopImplOnlyTargetSkipsSpecChildAndStartsImplChild(t *testing.T) {
	cwd := t.TempDir()
	feature := "ready-impl-only"
	writeFeature(t, cwd, feature, map[string]string{
		"spec.json": validImplSpecJSON,
		"tasks.md":  "- [ ] 1. pending\n",
	})

	specStarts := 0
	specDef := workflow.Definition{
		ID:    "kiro-spec-loop",
		Label: "Kiro 仕様作成ループ",
		Fn: func(context.Context, string, func(workflow.WorkflowEvent), workflow.WorkflowAsk) error {
			specStarts++
			return errors.New("spec child must not start for impl-only target")
		},
	}

	implStarts := 0
	var implSelectedFeature string
	implDef := workflow.Definition{
		ID:    "kiro-impl-loop",
		Label: "Kiro 実装ループ",
		Fn: func(ctx context.Context, _ string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			implStarts++
			selected, ok, err := ask.Select(ctx, workflow.SelectParams{
				Key: "feature",
				Items: []workflow.SelectItem{
					{ID: feature, Label: feature},
				},
			})
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("impl child feature selection was cancelled")
			}
			implSelectedFeature = selected
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "impl-only selected " + selected})
			return nil
		},
	}

	parentAsk := &recordingSpecImplLoopAsk{selected: feature, ok: true}
	def := buildKiroSpecImplLoop(&fakeAgent{}, specDef, implDef)
	var events []workflow.WorkflowEvent
	err := def.Run(context.Background(), cwd, func(event workflow.WorkflowEvent) {
		events = append(events, event)
	}, parentAsk)
	if err != nil {
		t.Fatalf("KiroSpecImplLoop Run error = %v", err)
	}
	if specStarts != 0 {
		t.Fatalf("spec child starts = %d, want 0 for impl-only target", specStarts)
	}
	if implStarts != 1 {
		t.Fatalf("impl child starts = %d, want 1", implStarts)
	}
	if implSelectedFeature != feature {
		t.Fatalf("impl selected feature = %q, want %q", implSelectedFeature, feature)
	}
	if len(parentAsk.calls) != 1 {
		t.Fatalf("parent ask Select calls = %d, want 1", len(parentAsk.calls))
	}
	wantEventOrder := []string{
		"active:Kiro 実装ループ",
		"text:impl-only selected " + feature,
	}
	if got := specImplLoopEventOrder(events); !reflect.DeepEqual(got, wantEventOrder) {
		t.Fatalf("event order = %v, want %v", got, wantEventOrder)
	}
}

func TestSpecLoopCompletionExtractorValidCompletedResult(t *testing.T) {
	cwd := t.TempDir()
	runID := "spec-run-valid"
	feature := "feature-from-result"
	resultPath := writeSpecLoopResult(t, cwd, runID, RunResult{
		RunID:             runID,
		WorkflowID:        kiroSpecLoopID,
		Feature:           feature,
		TerminationReason: RunReasonCompleted,
		Steps:             []RunResultStep{},
	})

	got, err := extractKiroSpecLoopOutput(context.Background(), cwd, workflow.ChildResult{
		DefinitionID: kiroSpecLoopID,
		RunID:        runID,
	})
	if err != nil {
		t.Fatalf("extractKiroSpecLoopOutput valid completed result error = %v", err)
	}

	want := specLoopOutput{
		Feature:    feature,
		SpecRunID:  runID,
		ResultPath: resultPath,
	}
	if got != want {
		t.Fatalf("extractKiroSpecLoopOutput = %+v, want %+v", got, want)
	}
}

func TestSpecLoopCompletionExtractorRejectsEmptyChildRunID(t *testing.T) {
	cwd := t.TempDir()
	resultPath := writeSpecLoopResult(t, cwd, "", RunResult{
		RunID:             "",
		WorkflowID:        kiroSpecLoopID,
		Feature:           "root-result-feature",
		TerminationReason: RunReasonCompleted,
		Steps:             []RunResultStep{},
	})

	_, err := extractKiroSpecLoopOutput(context.Background(), cwd, workflow.ChildResult{
		DefinitionID: kiroSpecLoopID,
		RunID:        "",
	})
	if err == nil {
		t.Fatal("extractKiroSpecLoopOutput error = nil, want empty child runID gate failure")
	}
	assertErrorContainsAll(t, err, resultPath, "specRunID=", "child runID is empty")
}

func TestSpecLoopCompletionExtractorArtifactGateFailures(t *testing.T) {
	const runID = "spec-run-gate"
	const feature = "feature-from-result"

	tests := []struct {
		name              string
		seed              func(t *testing.T, cwd string) string
		wantErrorContains []string
		wantErrorAbsent   []string
	}{
		{
			name: "missing result does not fall back to context",
			seed: func(t *testing.T, cwd string) string {
				writeSpecLoopContextFeature(t, cwd, runID, "context-only-feature")
				return specLoopResultPath(cwd, runID)
			},
			wantErrorContains: []string{
				"read result.json",
				"specRunID=" + runID,
			},
			wantErrorAbsent: []string{"context-only-feature"},
		},
		{
			name: "invalid json",
			seed: func(t *testing.T, cwd string) string {
				return writeSpecLoopResultRaw(t, cwd, runID, "{not json")
			},
			wantErrorContains: []string{
				"decode result.json",
				"specRunID=" + runID,
			},
		},
		{
			name: "wrong workflow id",
			seed: func(t *testing.T, cwd string) string {
				return writeSpecLoopResult(t, cwd, runID, RunResult{
					RunID:             runID,
					WorkflowID:        "other-workflow",
					Feature:           feature,
					TerminationReason: RunReasonCompleted,
					Steps:             []RunResultStep{},
				})
			},
			wantErrorContains: []string{
				`workflowId="other-workflow"`,
				`feature="feature-from-result"`,
				`terminationReason="completed"`,
			},
		},
		{
			name: "wrong run id",
			seed: func(t *testing.T, cwd string) string {
				return writeSpecLoopResult(t, cwd, runID, RunResult{
					RunID:             "different-spec-run",
					WorkflowID:        kiroSpecLoopID,
					Feature:           feature,
					TerminationReason: RunReasonCompleted,
					Steps:             []RunResultStep{},
				})
			},
			wantErrorContains: []string{
				`runId="different-spec-run"`,
				`want child runID "spec-run-gate"`,
				`feature="feature-from-result"`,
				`terminationReason="completed"`,
			},
		},
		{
			name: "empty feature",
			seed: func(t *testing.T, cwd string) string {
				return writeSpecLoopResult(t, cwd, runID, RunResult{
					RunID:             runID,
					WorkflowID:        kiroSpecLoopID,
					Feature:           "  ",
					TerminationReason: RunReasonCompleted,
					Steps:             []RunResultStep{},
				})
			},
			wantErrorContains: []string{
				"feature is empty",
				`terminationReason="completed"`,
			},
		},
		{
			name: "non completed termination reason",
			seed: func(t *testing.T, cwd string) string {
				return writeSpecLoopResult(t, cwd, runID, RunResult{
					RunID:             runID,
					WorkflowID:        kiroSpecLoopID,
					Feature:           feature,
					TerminationReason: RunReasonCancelledByUser,
					Steps:             []RunResultStep{},
				})
			},
			wantErrorContains: []string{
				`feature="feature-from-result"`,
				`terminationReason="cancelled-by-user"`,
				`want "completed"`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cwd := t.TempDir()
			resultPath := tt.seed(t, cwd)

			_, err := extractKiroSpecLoopOutput(context.Background(), cwd, workflow.ChildResult{
				DefinitionID: kiroSpecLoopID,
				RunID:        runID,
			})
			if err == nil {
				t.Fatal("extractKiroSpecLoopOutput error = nil, want artifact gate failure")
			}
			assertErrorContainsAll(t, err, append([]string{resultPath, "specRunID=" + runID}, tt.wantErrorContains...)...)
			for _, absent := range tt.wantErrorAbsent {
				if strings.Contains(err.Error(), absent) {
					t.Fatalf("error %q contains fallback-only value %q", err.Error(), absent)
				}
			}
		})
	}
}

func TestKiroSpecImplLoopArtifactGateFailuresDoNotStartImplChild(t *testing.T) {
	const gateFeature = "gate-feature"
	const contextOnlyFeature = "context-only-feature"

	tests := []struct {
		name              string
		seed              func(t *testing.T, cwd string, runID string) string
		wantErrorContains []string
	}{
		{
			name: "missing result",
			seed: func(t *testing.T, cwd string, runID string) string {
				writeSpecLoopContextFeature(t, cwd, runID, contextOnlyFeature)
				return specLoopResultPath(cwd, runID)
			},
			wantErrorContains: []string{"read result.json"},
		},
		{
			name: "invalid json",
			seed: func(t *testing.T, cwd string, runID string) string {
				return writeSpecLoopResultRaw(t, cwd, runID, "{not json")
			},
			wantErrorContains: []string{"decode result.json"},
		},
		{
			name: "wrong workflow id",
			seed: func(t *testing.T, cwd string, runID string) string {
				return writeSpecLoopResult(t, cwd, runID, RunResult{
					RunID:             runID,
					WorkflowID:        "other-workflow",
					Feature:           gateFeature,
					TerminationReason: RunReasonCompleted,
					Steps:             []RunResultStep{},
				})
			},
			wantErrorContains: []string{`workflowId="other-workflow"`, `feature="gate-feature"`, `terminationReason="completed"`},
		},
		{
			name: "wrong run id",
			seed: func(t *testing.T, cwd string, runID string) string {
				return writeSpecLoopResult(t, cwd, runID, RunResult{
					RunID:             "different-spec-run",
					WorkflowID:        kiroSpecLoopID,
					Feature:           gateFeature,
					TerminationReason: RunReasonCompleted,
					Steps:             []RunResultStep{},
				})
			},
			wantErrorContains: []string{`runId="different-spec-run"`, `feature="gate-feature"`, `terminationReason="completed"`},
		},
		{
			name: "empty feature",
			seed: func(t *testing.T, cwd string, runID string) string {
				return writeSpecLoopResult(t, cwd, runID, RunResult{
					RunID:             runID,
					WorkflowID:        kiroSpecLoopID,
					Feature:           "",
					TerminationReason: RunReasonCompleted,
					Steps:             []RunResultStep{},
				})
			},
			wantErrorContains: []string{"feature is empty", `terminationReason="completed"`},
		},
		{
			name: "non completed termination reason",
			seed: func(t *testing.T, cwd string, runID string) string {
				return writeSpecLoopResult(t, cwd, runID, RunResult{
					RunID:             runID,
					WorkflowID:        kiroSpecLoopID,
					Feature:           gateFeature,
					TerminationReason: RunReasonCancelledByUser,
					Steps:             []RunResultStep{},
				})
			},
			wantErrorContains: []string{`feature="gate-feature"`, `terminationReason="cancelled-by-user"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cwd := t.TempDir()
			writeFeature(t, cwd, gateFeature, map[string]string{
				"brief.md": "gate feature brief\n",
			})
			writeFeature(t, cwd, contextOnlyFeature, map[string]string{
				"spec.json": validImplSpecJSON,
				"tasks.md":  "- [ ] 1. pending\n",
			})

			var resultPath string
			var specRunID string
			specDef := workflow.Definition{
				ID:    "kiro-spec-loop",
				Label: "Kiro 仕様作成ループ",
				Fn: func(ctx context.Context, cwd string, _ func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
					runID := workflow.RunIDFromContext(ctx)
					specRunID = runID
					resultPath = tt.seed(t, cwd, runID)
					return nil
				},
			}

			implStarts := 0
			implDef := workflow.Definition{
				ID:    "kiro-impl-loop",
				Label: "Kiro 実装ループ",
				Fn: func(context.Context, string, func(workflow.WorkflowEvent), workflow.WorkflowAsk) error {
					implStarts++
					return nil
				},
			}

			def := buildKiroSpecImplLoop(&fakeAgent{}, specDef, implDef)
			var events []workflow.WorkflowEvent
			err := def.Run(context.Background(), cwd, func(event workflow.WorkflowEvent) {
				events = append(events, event)
			}, &recordingSpecImplLoopAsk{selected: gateFeature, ok: true})
			if err == nil {
				t.Fatal("KiroSpecImplLoop Run error = nil, want artifact gate failure")
			}
			if implStarts != 0 {
				t.Fatalf("impl child starts = %d, want 0", implStarts)
			}
			assertErrorContainsAll(t, err, append([]string{resultPath, "specRunID=" + specRunID}, tt.wantErrorContains...)...)
			assertWorkflowErrorEventContainsAll(t, events, append([]string{resultPath, "specRunID=" + specRunID}, tt.wantErrorContains...)...)
			assertWorkflowErrorEventCount(t, events, 1)
		})
	}
}

func TestKiroSpecImplLoopSpecChildFailuresStopBeforeExtractorAndImplChild(t *testing.T) {
	const feature = "feature-with-valid-result"
	const workflowErrorMessage = "spec child logical failure"
	returnedErr := errors.New("spec child returned failure")

	tests := []struct {
		name                   string
		specFailure            func(onEvent func(workflow.WorkflowEvent)) error
		assertErr              func(t *testing.T, err error)
		wantWorkflowErrorEvent string
		wantWorkflowErrorCount int
	}{
		{
			name: "returned error",
			specFailure: func(func(workflow.WorkflowEvent)) error {
				return returnedErr
			},
			assertErr: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, returnedErr) {
					t.Fatalf("KiroSpecImplLoop Run error = %v, want returned spec child error", err)
				}
			},
			wantWorkflowErrorCount: 0,
		},
		{
			name: "workflow error event",
			specFailure: func(onEvent func(workflow.WorkflowEvent)) error {
				onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: workflowErrorMessage})
				return nil
			},
			assertErr: func(t *testing.T, err error) {
				t.Helper()
				if err == nil {
					t.Fatal("KiroSpecImplLoop Run error = nil, want spec child WorkflowError")
				}
				if err.Error() != workflowErrorMessage {
					t.Fatalf("KiroSpecImplLoop Run error = %q, want %q", err.Error(), workflowErrorMessage)
				}
			},
			wantWorkflowErrorEvent: workflowErrorMessage,
			wantWorkflowErrorCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cwd := t.TempDir()
			writeFeature(t, cwd, feature, map[string]string{
				"brief.md": "feature brief\n",
			})

			specDef := workflow.Definition{
				ID:    "kiro-spec-loop",
				Label: "Kiro 仕様作成ループ",
				Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
					runID := workflow.RunIDFromContext(ctx)
					writeSpecLoopResult(t, cwd, runID, RunResult{
						RunID:             runID,
						WorkflowID:        kiroSpecLoopID,
						Feature:           feature,
						TerminationReason: RunReasonCompleted,
						Steps:             []RunResultStep{},
					})
					return tt.specFailure(onEvent)
				},
			}

			implStarts := 0
			implDef := workflow.Definition{
				ID:    "kiro-impl-loop",
				Label: "Kiro 実装ループ",
				Fn: func(context.Context, string, func(workflow.WorkflowEvent), workflow.WorkflowAsk) error {
					implStarts++
					return errors.New("impl child must not start after spec child failure")
				},
			}

			def := buildKiroSpecImplLoop(&fakeAgent{}, specDef, implDef)
			var events []workflow.WorkflowEvent
			err := def.Run(context.Background(), cwd, func(event workflow.WorkflowEvent) {
				events = append(events, event)
			}, &recordingSpecImplLoopAsk{selected: feature, ok: true})
			tt.assertErr(t, err)
			if err != nil && strings.Contains(err.Error(), "spec result gate failed") {
				t.Fatalf("KiroSpecImplLoop Run error = %q, want child failure before output extraction", err.Error())
			}
			if implStarts != 0 {
				t.Fatalf("impl child starts = %d, want 0", implStarts)
			}
			assertWorkflowActiveLabel(t, events, "Kiro 仕様作成ループ")
			assertNoWorkflowActiveLabel(t, events, "Kiro 実装ループ")
			if tt.wantWorkflowErrorEvent != "" {
				assertWorkflowErrorEventContainsAll(t, events, tt.wantWorkflowErrorEvent)
			}
			assertWorkflowErrorEventCount(t, events, tt.wantWorkflowErrorCount)
		})
	}
}

func TestKiroSpecImplLoopDeliveryFailureStopsBeforeImplChildWithoutFallback(t *testing.T) {
	cwd := t.TempDir()
	specFeature := "spec-completed-but-not-implementable"
	otherFeature := "other-ready-feature"
	writeFeature(t, cwd, specFeature, map[string]string{
		"brief.md": "spec feature brief\n",
	})
	writeFeature(t, cwd, otherFeature, map[string]string{
		"spec.json": validImplSpecJSON,
		"tasks.md":  "- [ ] 1. pending\n",
	})

	var resultPath string
	var specRunID string
	specDef := workflow.Definition{
		ID:    "kiro-spec-loop",
		Label: "Kiro 仕様作成ループ",
		Fn: func(ctx context.Context, cwd string, _ func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			specRunID = workflow.RunIDFromContext(ctx)
			resultPath = writeSpecLoopResult(t, cwd, specRunID, RunResult{
				RunID:             specRunID,
				WorkflowID:        kiroSpecLoopID,
				Feature:           specFeature,
				TerminationReason: RunReasonCompleted,
				Steps:             []RunResultStep{},
			})
			return nil
		},
	}

	implStarts := 0
	implDef := workflow.Definition{
		ID:    "kiro-impl-loop",
		Label: "Kiro 実装ループ",
		Fn: func(context.Context, string, func(workflow.WorkflowEvent), workflow.WorkflowAsk) error {
			implStarts++
			return errors.New("impl child must not start when completed spec feature is not implementable")
		},
	}

	def := buildKiroSpecImplLoop(&fakeAgent{}, specDef, implDef)
	var events []workflow.WorkflowEvent
	err := def.Run(context.Background(), cwd, func(event workflow.WorkflowEvent) {
		events = append(events, event)
	}, &recordingSpecImplLoopAsk{selected: specFeature, ok: true})
	if err == nil {
		t.Fatal("KiroSpecImplLoop Run error = nil, want impl delivery failure")
	}
	if implStarts != 0 {
		t.Fatalf("impl child starts = %d, want 0", implStarts)
	}
	assertErrorContainsAll(t, err,
		"input delivery failed",
		`feature="`+specFeature+`"`,
		"specRunID="+specRunID,
		"resultPath="+resultPath,
		"not implementable",
		"available: "+otherFeature,
		"no fallback",
	)
	assertWorkflowErrorEventContainsAll(t, events,
		"input delivery failed",
		`feature="`+specFeature+`"`,
		"specRunID="+specRunID,
		"resultPath="+resultPath,
		"not implementable",
		"available: "+otherFeature,
		"no fallback",
	)
	assertWorkflowErrorEventCount(t, events, 1)
	assertWorkflowActiveLabel(t, events, "Kiro 仕様作成ループ")
	assertNoWorkflowActiveLabel(t, events, "Kiro 実装ループ")
	if resultPath == "" {
		t.Fatal("spec child did not write result path")
	}
}

func TestKiroSpecImplLoopImplChildFailuresPropagateWithoutTailProcessing(t *testing.T) {
	const feature = "impl-failure-feature"
	const specText = "spec stage completed feature=impl-failure-feature"
	const implText = "impl stage selected feature=impl-failure-feature"
	const implWorkflowError = "impl stage logical failure for feature=impl-failure-feature"
	returnedErr := errors.New("impl child returned failure for feature=impl-failure-feature")

	tests := []struct {
		name                string
		implFailure         func(onEvent func(workflow.WorkflowEvent)) error
		assertErr           func(t *testing.T, err error)
		wantEventSummary    []string
		wantWorkflowErrRuns bool
	}{
		{
			name: "returned error",
			implFailure: func(func(workflow.WorkflowEvent)) error {
				return returnedErr
			},
			assertErr: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, returnedErr) {
					t.Fatalf("KiroSpecImplLoop Run error = %v, want impl returned error", err)
				}
			},
			wantEventSummary: []string{
				"active:Kiro 仕様作成ループ",
				"text:" + specText,
				"active:Kiro 実装ループ",
				"text:" + implText,
			},
		},
		{
			name: "workflow error event",
			implFailure: func(onEvent func(workflow.WorkflowEvent)) error {
				onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: implWorkflowError})
				return nil
			},
			assertErr: func(t *testing.T, err error) {
				t.Helper()
				if err == nil {
					t.Fatal("KiroSpecImplLoop Run error = nil, want impl WorkflowError")
				}
				if err.Error() != implWorkflowError {
					t.Fatalf("KiroSpecImplLoop Run error = %q, want %q", err.Error(), implWorkflowError)
				}
			},
			wantEventSummary: []string{
				"active:Kiro 仕様作成ループ",
				"text:" + specText,
				"active:Kiro 実装ループ",
				"text:" + implText,
				"error:" + implWorkflowError,
			},
			wantWorkflowErrRuns: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cwd := t.TempDir()
			writeFeature(t, cwd, feature, map[string]string{
				"brief.md": "feature brief\n",
			})

			specDef := workflow.Definition{
				ID:    "kiro-spec-loop",
				Label: "Kiro 仕様作成ループ",
				Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
					runID := workflow.RunIDFromContext(ctx)
					writeSpecLoopResult(t, cwd, runID, RunResult{
						RunID:             runID,
						WorkflowID:        kiroSpecLoopID,
						Feature:           feature,
						TerminationReason: RunReasonCompleted,
						Steps:             []RunResultStep{},
					})
					writeFeature(t, cwd, feature, map[string]string{
						"spec.json": validImplSpecJSON,
						"tasks.md":  "- [ ] 1. pending\n",
					})
					onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: specText})
					return nil
				},
			}

			implStarts := 0
			implDef := workflow.Definition{
				ID:    "kiro-impl-loop",
				Label: "Kiro 実装ループ",
				Fn: func(ctx context.Context, _ string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
					implStarts++
					selected, ok, err := ask.Select(ctx, workflow.SelectParams{
						Key: "feature",
						Items: []workflow.SelectItem{
							{ID: feature, Label: feature},
						},
					})
					if err != nil {
						return err
					}
					if !ok {
						return errors.New("impl child feature selection was cancelled")
					}
					if selected != feature {
						return errors.New("impl child selected a different feature: " + selected)
					}
					onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: implText})
					return tt.implFailure(onEvent)
				},
			}

			parentAsk := &recordingSpecImplLoopAsk{selected: feature, ok: true}
			def := buildKiroSpecImplLoop(&fakeAgent{}, specDef, implDef)
			var events []workflow.WorkflowEvent
			err := def.Run(context.Background(), cwd, func(event workflow.WorkflowEvent) {
				events = append(events, event)
			}, parentAsk)

			tt.assertErr(t, err)
			if implStarts != 1 {
				t.Fatalf("impl child starts = %d, want 1 (no retry or recovery re-entry)", implStarts)
			}
			if len(parentAsk.calls) != 1 {
				t.Fatalf("parent ask Select calls = %d, want 1 (parent selects target once)", len(parentAsk.calls))
			}
			if got := workflowEventSummary(events); !reflect.DeepEqual(got, tt.wantEventSummary) {
				t.Fatalf("event summary = %v, want %v", got, tt.wantEventSummary)
			}

			specRunID := runIDForTextEvent(events, specText)
			if specRunID == "" {
				t.Fatalf("spec child event RunID is empty: %+v", events)
			}
			implRunID := runIDForTextEvent(events, implText)
			if implRunID == "" {
				t.Fatalf("impl child event RunID is empty: %+v", events)
			}
			if implRunID == specRunID {
				t.Fatalf("impl child RunID = %q, want distinct from spec child RunID", implRunID)
			}
			if tt.wantWorkflowErrRuns {
				errRunID := runIDForWorkflowErrorEvent(events, implWorkflowError)
				if errRunID != implRunID {
					t.Fatalf("WorkflowError RunID = %q, want impl child RunID %q", errRunID, implRunID)
				}
				assertWorkflowErrorEventCount(t, events, 1)
			} else {
				assertWorkflowErrorEventCount(t, events, 0)
			}
		})
	}
}

func TestDeliverKiroImplFeaturePrechecksImplementableFeatureAndReturnsFixedAsk(t *testing.T) {
	cwd := t.TempDir()
	specFeature := "spec-ready-feature"
	otherFeature := "other-ready-feature"
	writeFeature(t, cwd, specFeature, map[string]string{
		"spec.json": validImplSpecJSON,
		"tasks.md":  "- [ ] 1. pending\n",
	})
	writeFeature(t, cwd, otherFeature, map[string]string{
		"spec.json": validImplSpecJSON,
		"tasks.md":  "- [ ] 1. pending\n",
	})

	delegate := &recordingSpecImplLoopAsk{selected: otherFeature, ok: true}
	input := implLoopInput{
		Feature:    specFeature,
		SpecRunID:  "spec-run-delivery",
		ResultPath: filepath.Join(cwd, ".rilll", kiroSpecLoopID, "spec-run-delivery", "result.json"),
	}

	gotCtx, gotAsk, err := deliverKiroImplFeature(context.Background(), cwd, delegate, input)
	if err != nil {
		t.Fatalf("deliverKiroImplFeature error = %v", err)
	}
	if gotCtx == nil {
		t.Fatal("deliverKiroImplFeature returned nil context")
	}
	selected, ok, err := gotAsk.Select(gotCtx, workflow.SelectParams{
		Key: "feature",
		Items: []workflow.SelectItem{
			{ID: otherFeature, Label: otherFeature},
			{ID: specFeature, Label: specFeature},
		},
	})
	if err != nil {
		t.Fatalf("fixed ask feature Select error = %v", err)
	}
	if !ok || selected != specFeature {
		t.Fatalf("fixed ask feature Select = (%q, %v), want (%q, true)", selected, ok, specFeature)
	}
	if len(delegate.calls) != 0 {
		t.Fatalf("delegate Select calls = %d, want 0 for fixed feature selection", len(delegate.calls))
	}
}

func TestDeliverKiroImplFeatureRejectsNonImplementableFeatureWithoutFallback(t *testing.T) {
	cwd := t.TempDir()
	specFeature := "spec-not-ready-feature"
	otherFeature := "other-ready-feature"
	writeFeature(t, cwd, specFeature, map[string]string{
		"spec.json": validImplSpecJSON,
		"tasks.md":  "- [x] 1. already completed\n",
	})
	writeFeature(t, cwd, otherFeature, map[string]string{
		"spec.json": validImplSpecJSON,
		"tasks.md":  "- [ ] 1. pending\n",
	})

	delegate := &recordingSpecImplLoopAsk{selected: otherFeature, ok: true}
	resultPath := filepath.Join(cwd, ".rilll", kiroSpecLoopID, "spec-run-non-implementable", "result.json")
	_, gotAsk, err := deliverKiroImplFeature(context.Background(), cwd, delegate, implLoopInput{
		Feature:    specFeature,
		SpecRunID:  "spec-run-non-implementable",
		ResultPath: resultPath,
	})
	if err == nil {
		t.Fatal("deliverKiroImplFeature error = nil, want non-implementable feature failure")
	}
	assertErrorContainsAll(t, err,
		`feature="`+specFeature+`"`,
		"specRunID=spec-run-non-implementable",
		"resultPath="+resultPath,
		"not implementable",
		"available: "+otherFeature,
		"no fallback",
	)
	if gotAsk != delegate {
		t.Fatalf("deliverKiroImplFeature returned ask %#v, want original delegate on failure", gotAsk)
	}
	if len(delegate.calls) != 0 {
		t.Fatalf("delegate Select calls = %d, want 0 before child start", len(delegate.calls))
	}
}

func TestFixedFeatureAskDetectsDriftWhenFeatureMissingFromItems(t *testing.T) {
	delegate := &recordingSpecImplLoopAsk{selected: "delegate-choice", ok: true}
	ask := fixedFeatureAsk{feature: "spec-feature", delegate: delegate}

	selected, ok, err := ask.Select(context.Background(), workflow.SelectParams{
		Key: "feature",
		Items: []workflow.SelectItem{
			{ID: "other-feature", Label: "other-feature"},
		},
	})
	if err == nil {
		t.Fatal("fixedFeatureAsk.Select error = nil, want drift detection")
	}
	if selected != "" || ok {
		t.Fatalf("fixedFeatureAsk.Select = (%q, %v), want empty/false on drift", selected, ok)
	}
	assertErrorContainsAll(t, err, `feature="spec-feature"`, "not present in impl selection items")
	if len(delegate.calls) != 0 {
		t.Fatalf("delegate Select calls = %d, want 0 for feature drift", len(delegate.calls))
	}
}

func TestFixedFeatureAskDelegatesNonFeatureSelection(t *testing.T) {
	delegate := &recordingSpecImplLoopAsk{selected: "delegate-choice", ok: true}
	ask := fixedFeatureAsk{feature: "spec-feature", delegate: delegate}

	selected, ok, err := ask.Select(context.Background(), workflow.SelectParams{
		Key: "review-action",
		Items: []workflow.SelectItem{
			{ID: "delegate-choice", Label: "delegate-choice"},
		},
	})
	if err != nil {
		t.Fatalf("fixedFeatureAsk.Select non-feature error = %v", err)
	}
	if !ok || selected != "delegate-choice" {
		t.Fatalf("fixedFeatureAsk.Select non-feature = (%q, %v), want delegated choice", selected, ok)
	}
	if len(delegate.calls) != 1 {
		t.Fatalf("delegate Select calls = %d, want 1", len(delegate.calls))
	}
	if delegate.calls[0].Key != "review-action" {
		t.Fatalf("delegate Select Key = %q, want review-action", delegate.calls[0].Key)
	}
}

type rejectingSpecImplLoopAsk struct {
	calls int
}

func (a *rejectingSpecImplLoopAsk) Select(context.Context, workflow.SelectParams) (string, bool, error) {
	a.calls++
	return "", false, errors.New("parent ask must not be used for impl feature selection")
}

type recordingSpecImplLoopAsk struct {
	calls    []workflow.SelectParams
	selected string
	ok       bool
	err      error
}

func (a *recordingSpecImplLoopAsk) Select(_ context.Context, params workflow.SelectParams) (string, bool, error) {
	a.calls = append(a.calls, params)
	if a.err != nil {
		return "", false, a.err
	}
	return a.selected, a.ok, nil
}

func specImplLoopEventOrder(events []workflow.WorkflowEvent) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		switch event.Type {
		case workflow.WorkflowActive:
			out = append(out, "active:"+event.Label)
		case workflow.WorkflowText:
			out = append(out, "text:"+event.Text)
		}
	}
	return out
}

func runIDForTextEvent(events []workflow.WorkflowEvent, text string) string {
	for _, event := range events {
		if event.Type == workflow.WorkflowText && event.Text == text {
			return event.RunID
		}
	}
	return ""
}

func runIDForWorkflowErrorEvent(events []workflow.WorkflowEvent, message string) string {
	for _, event := range events {
		if event.Type == workflow.WorkflowError && event.Message == message {
			return event.RunID
		}
	}
	return ""
}

func workflowEventSummary(events []workflow.WorkflowEvent) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		switch event.Type {
		case workflow.WorkflowActive:
			out = append(out, "active:"+event.Label)
		case workflow.WorkflowText:
			out = append(out, "text:"+event.Text)
		case workflow.WorkflowError:
			out = append(out, "error:"+event.Message)
		}
	}
	return out
}

func assertWorkflowActiveLabel(t *testing.T, events []workflow.WorkflowEvent, label string) {
	t.Helper()
	for _, event := range events {
		if event.Type == workflow.WorkflowActive && event.Label == label {
			return
		}
	}
	t.Fatalf("WorkflowActive label %q not found in %+v", label, events)
}

func assertNoWorkflowActiveLabel(t *testing.T, events []workflow.WorkflowEvent, label string) {
	t.Helper()
	for _, event := range events {
		if event.Type == workflow.WorkflowActive && event.Label == label {
			t.Fatalf("unexpected WorkflowActive label %q in %+v", label, events)
		}
	}
}

func specLoopResultPath(cwd string, runID string) string {
	return filepath.Join(cwd, ".rilll", kiroSpecLoopID, runID, "result.json")
}

func specLoopContextPath(cwd string, runID string) string {
	return filepath.Join(cwd, ".rilll", kiroSpecLoopID, runID, "00-context.json")
}

func writeSpecLoopResult(t *testing.T, cwd string, runID string, result RunResult) string {
	t.Helper()
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent result: %v", err)
	}
	return writeSpecLoopResultRaw(t, cwd, runID, string(raw))
}

func writeSpecLoopResultRaw(t *testing.T, cwd string, runID string, raw string) string {
	t.Helper()
	path := specLoopResultPath(cwd, runID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
	return path
}

func writeSpecLoopContextFeature(t *testing.T, cwd string, runID string, feature string) string {
	t.Helper()
	path := specLoopContextPath(cwd, runID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	raw, err := json.Marshal(map[string]string{"feature": feature})
	if err != nil {
		t.Fatalf("Marshal context feature: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
	return path
}

func assertErrorContainsAll(t *testing.T, err error, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("error is nil")
	}
	message := err.Error()
	for _, part := range parts {
		if !strings.Contains(message, part) {
			t.Fatalf("error %q does not contain %q", message, part)
		}
	}
}

func assertWorkflowErrorEventContainsAll(t *testing.T, events []workflow.WorkflowEvent, parts ...string) {
	t.Helper()
	for _, event := range events {
		if event.Type != workflow.WorkflowError {
			continue
		}
		missing := false
		for _, part := range parts {
			if !strings.Contains(event.Message, part) {
				missing = true
				break
			}
		}
		if !missing {
			return
		}
	}
	t.Fatalf("WorkflowError event containing %v not found in %+v", parts, events)
}

func assertWorkflowErrorEventCount(t *testing.T, events []workflow.WorkflowEvent, want int) {
	t.Helper()
	got := 0
	for _, event := range events {
		if event.Type == workflow.WorkflowError {
			got++
		}
	}
	if got != want {
		t.Fatalf("WorkflowError event count = %d, want %d in %+v", got, want, events)
	}
}
