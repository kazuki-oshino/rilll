package kiro

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
)

func TestKiroSpecLoopSessionBPersonaUsesBuiltInFacetProvenanceAndIgnoresOldKiroOverride(t *testing.T) {
	const feature = "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	writeFile(t, filepath.Join(cwd, ".kiro", "personas", "architecture-quality.md"), "# old architecture override\nthis old path must not win.\n")

	onEvent, _ := collectEvents()
	agent := &recordedAgent{
		SkillResponse: provider.AgentResponse{
			Success: true,
			StructuredOutput: map[string]any{
				"has_inquiry": false,
				"summary":     "ok",
			},
			SessionID: "session-skill",
		},
		CommitResponse: commitOKResp(),
	}

	if err := runKiroSpecLoop(
		context.Background(),
		cwd,
		onEvent,
		&fakeAsk{Selected: feature},
		agent,
	); err != nil {
		t.Fatalf("runKiroSpecLoop returned error: %v", err)
	}

	ctx := mustReadRunContextJSON(t, cwd)
	if ctx.PersonaName != "architecture-quality" {
		t.Fatalf("00-context.json personaName = %q, want logical name architecture-quality", ctx.PersonaName)
	}
	if ctx.PersonaPath != "embed:facets/personas/architecture-quality.md" {
		t.Fatalf("00-context.json personaPath = %q, want embedded facet provenance", ctx.PersonaPath)
	}

	rr := mustReadResultJSON(t, cwd)
	if rr.PersonaName != "architecture-quality" {
		t.Fatalf("result.json personaName = %q, want logical name architecture-quality", rr.PersonaName)
	}
	if rr.PersonaPath != "embed:facets/personas/architecture-quality.md" {
		t.Fatalf("result.json personaPath = %q, want embedded facet provenance", rr.PersonaPath)
	}
	if strings.Contains(ctx.PersonaPath, ".kiro/personas") || strings.Contains(rr.PersonaPath, ".kiro/personas") {
		t.Fatalf("old .kiro/personas path must not be persisted: context=%q result=%q", ctx.PersonaPath, rr.PersonaPath)
	}
}

func TestKiroSpecLoopSessionBPersonaProjectOverrideFeedsAnswererAndArtifacts(t *testing.T) {
	const feature = "demo"
	const overrideBody = "# project architecture-quality override\nsession B answerer must see this content.\n"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	overridePath := filepath.Join(cwd, ".rilll-config", "facets", "personas", "architecture-quality.md")
	writeFile(t, overridePath, overrideBody)
	writeFile(t, filepath.Join(cwd, ".kiro", "personas", "architecture-quality.md"), "# old architecture loser\n")

	onEvent, _ := collectEvents()
	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "answerer":
			return answerOKResp(), nil
		case "commit":
			return commitOKResp(), nil
		case "fix-edit":
			return fixEditOKResp(), nil
		case "fix-judge":
			return fixJudgeOKResp(), nil
		case "skill":
			if call.ResumeSessionID == "" && strings.Contains(call.Prompt, "/kiro-spec-init") {
				return skillInquiryResp(), nil
			}
			return skillNoInquiryResp(), nil
		default:
			return provider.AgentResponse{}, nil
		}
	}

	if err := runKiroSpecLoop(
		context.Background(),
		cwd,
		onEvent,
		&fakeAsk{Selected: feature},
		agent,
	); err != nil {
		t.Fatalf("runKiroSpecLoop returned error: %v", err)
	}

	ctx := mustReadRunContextJSON(t, cwd)
	if ctx.PersonaName != "architecture-quality" {
		t.Fatalf("00-context.json personaName = %q, want logical name architecture-quality", ctx.PersonaName)
	}
	if ctx.PersonaPath != overridePath {
		t.Fatalf("00-context.json personaPath = %q, want project facet override path %q", ctx.PersonaPath, overridePath)
	}
	rr := mustReadResultJSON(t, cwd)
	if rr.PersonaPath != overridePath {
		t.Fatalf("result.json personaPath = %q, want project facet override path %q", rr.PersonaPath, overridePath)
	}

	var answererPrompt string
	for _, c := range agent.Calls {
		if c.Kind == "answerer" {
			answererPrompt = c.Prompt
			break
		}
	}
	if answererPrompt == "" {
		t.Fatalf("answerer call was not observed: calls=%+v", summarizeCalls(agent.Calls))
	}
	if !strings.Contains(answererPrompt, overrideBody) {
		t.Fatalf("answerer prompt does not contain project facet override content:\n%s", answererPrompt)
	}
	if strings.Contains(answererPrompt, "old architecture loser") {
		t.Fatalf("answerer prompt contains old .kiro/personas loser content:\n%s", answererPrompt)
	}
}

func TestKiroSpecLoopSessionBPersonaEmptyFacetOverrideAbortsBeforeAgent(t *testing.T) {
	const feature = "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	writeFile(t, filepath.Join(cwd, ".rilll-config", "facets", "personas", "architecture-quality.md"), "")

	onEvent, eventsPtr := collectEvents()
	agent := &recordedAgent{}

	if err := runKiroSpecLoop(
		context.Background(),
		cwd,
		onEvent,
		&fakeAsk{Selected: feature},
		agent,
	); err != nil {
		t.Fatalf("runKiroSpecLoop returned error: %v", err)
	}

	if len(agent.Calls) != 0 {
		t.Fatalf("empty session B persona must abort before agent call: got=%d calls", len(agent.Calls))
	}
	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonMissingPersona {
		t.Fatalf("terminationReason = %q, want %q", rr.TerminationReason, RunReasonMissingPersona)
	}
	if rr.Failure != nil {
		t.Fatalf("missing-persona abort must not be recorded as engine failure: %+v", rr.Failure)
	}

	message := workflowErrorMessage(t, *eventsPtr)
	for _, want := range []string{
		"architecture-quality",
		".rilll-config/facets/personas/architecture-quality.md",
		"embed:facets/personas/architecture-quality.md",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("WorkflowError message %q does not contain %q", message, want)
		}
	}
	if strings.Contains(message, ".kiro/personas") {
		t.Fatalf("WorkflowError message must not guide to old persona path: %q", message)
	}
}

func TestKiroSpecLoopSessionBPersonaFatalFacetErrorPropagates(t *testing.T) {
	const feature = "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	if err := os.MkdirAll(filepath.Join(cwd, ".rilll-config", "facets", "personas", "architecture-quality.md"), 0o755); err != nil {
		t.Fatalf("mkdir guarded override path: %v", err)
	}

	onEvent, eventsPtr := collectEvents()
	agent := &recordedAgent{}

	err := runKiroSpecLoop(
		context.Background(),
		cwd,
		onEvent,
		&fakeAsk{Selected: feature},
		agent,
	)
	if err == nil {
		t.Fatal("runKiroSpecLoop error = nil, want fatal facet resolver error")
	}
	if !strings.Contains(err.Error(), "specLoopRunner.setup: resolve session B persona") {
		t.Fatalf("fatal error lacks setup wrapper: %v", err)
	}
	if !strings.Contains(err.Error(), ".rilll-config/facets/personas/architecture-quality.md") {
		t.Fatalf("fatal error lacks facet override path: %v", err)
	}
	if strings.Contains(err.Error(), ".kiro/personas") {
		t.Fatalf("fatal error must not mention old persona path: %v", err)
	}
	if len(agent.Calls) != 0 {
		t.Fatalf("fatal session B persona error must occur before agent call: got=%d calls", len(agent.Calls))
	}
	if len(*eventsPtr) != 0 {
		t.Fatalf("fatal resolver error should propagate instead of missing-persona WorkflowError: %+v", *eventsPtr)
	}
}

func mustReadRunContextJSON(t *testing.T, cwd string) runContext {
	t.Helper()
	runDir := filepath.Join(cwd, ".rilll", "kiro-spec-loop")
	entries, err := os.ReadDir(runDir)
	if err != nil {
		t.Fatalf("runDir not found: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("runDir に唯一の run-id ディレクトリを期待: got=%d entries=%v", len(entries), entries)
	}
	raw, err := os.ReadFile(filepath.Join(runDir, entries[0].Name(), "00-context.json"))
	if err != nil {
		t.Fatalf("00-context.json not written: %v", err)
	}
	var ctx runContext
	if err := json.Unmarshal(raw, &ctx); err != nil {
		t.Fatalf("00-context.json must be valid JSON: %v\ncontent=%s", err, string(raw))
	}
	return ctx
}

func workflowErrorMessage(t *testing.T, events []workflow.WorkflowEvent) string {
	t.Helper()
	for _, e := range events {
		if e.Type == workflow.WorkflowError {
			return e.Message
		}
	}
	t.Fatalf("WorkflowError event not found: %+v", events)
	return ""
}
