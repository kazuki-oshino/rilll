package kiro

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

type implLoopContextJSON struct {
	Feature      string   `json:"feature"`
	PendingTasks []string `json:"pendingTasks"`
	PersonaName  string   `json:"personaName"`
	PersonaPath  string   `json:"personaPath"`
}

func TestKiroImplLoopSessionBPersonaUsesBuiltInFacetProvenanceAndIgnoresOldKiroOverride(t *testing.T) {
	const feature = "demo"
	const oldPathLoser = "# old impl-direction override\nthis old path must not win.\n"
	cwd := setupImplFeatureCWD(t, feature, "- [ ] 1. 単独タスク\n")
	writeFile(t, filepath.Join(cwd, ".kiro", "personas", implPersonaName+".md"), oldPathLoser)

	runID := "impl-session-b-builtin"
	ctx := workflowctx.ContextWithRunMetadata(context.Background(), workflowctx.RunMetadata{WorkflowRunID: runID})
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}
	agent := scriptedImplLoopOneRoundAgent()

	if err := runKiroImplLoop(ctx, cwd, onEvent, ask, agent); err != nil {
		t.Fatalf("runKiroImplLoop returned error: %v", err)
	}

	gotCtx := mustReadImplLoopContextJSON(t, cwd, runID)
	if gotCtx.PersonaName != implPersonaName {
		t.Fatalf("00-context.json personaName = %q, want logical name %q", gotCtx.PersonaName, implPersonaName)
	}
	if gotCtx.PersonaPath != "embed:facets/personas/impl-direction-answerer.md" {
		t.Fatalf("00-context.json personaPath = %q, want embedded facet provenance", gotCtx.PersonaPath)
	}
	if strings.Contains(gotCtx.PersonaPath, ".kiro/personas") {
		t.Fatalf("old .kiro/personas path must not be persisted: %q", gotCtx.PersonaPath)
	}

	answerPrompt := firstAgentPromptByKind(t, agent.Calls, "answerer")
	if !strings.Contains(answerPrompt, "# Persona: Implementation Direction Setter") {
		t.Fatalf("answerer prompt does not contain built-in impl-direction answerer content:\n%s", answerPrompt)
	}
	if strings.Contains(answerPrompt, oldPathLoser) {
		t.Fatalf("answerer prompt contains old .kiro/personas loser content:\n%s", answerPrompt)
	}
	if strings.Contains(answerPrompt, "embed:facets/personas/impl-direction-answerer.md") {
		t.Fatalf("answerer prompt leaked facet source path metadata:\n%s", answerPrompt)
	}
}

func TestKiroImplLoopSessionBPersonaProjectOverrideFeedsAnswererAndContext(t *testing.T) {
	const feature = "demo"
	const overrideBody = "# project impl-direction-answerer override\nsession B answerer must see this content.\n"
	const oldPathLoser = "# old impl-direction loser\nthis old path must not win.\n"
	cwd := setupImplFeatureCWD(t, feature, "- [ ] 1. 単独タスク\n")
	overridePath := filepath.Join(cwd, ".rilll-config", "facets", "personas", "impl-direction-answerer.md")
	writeFile(t, overridePath, overrideBody)
	writeFile(t, filepath.Join(cwd, ".kiro", "personas", implPersonaName+".md"), oldPathLoser)

	runID := "impl-session-b-override"
	ctx := workflowctx.ContextWithRunMetadata(context.Background(), workflowctx.RunMetadata{WorkflowRunID: runID})
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}
	agent := scriptedImplLoopOneRoundAgent()

	if err := runKiroImplLoop(ctx, cwd, onEvent, ask, agent); err != nil {
		t.Fatalf("runKiroImplLoop returned error: %v", err)
	}

	gotCtx := mustReadImplLoopContextJSON(t, cwd, runID)
	if gotCtx.PersonaName != implPersonaName {
		t.Fatalf("00-context.json personaName = %q, want logical name %q", gotCtx.PersonaName, implPersonaName)
	}
	if gotCtx.PersonaPath != overridePath {
		t.Fatalf("00-context.json personaPath = %q, want project facet override path %q", gotCtx.PersonaPath, overridePath)
	}

	answerPrompt := firstAgentPromptByKind(t, agent.Calls, "answerer")
	if !strings.Contains(answerPrompt, overrideBody) {
		t.Fatalf("answerer prompt does not contain project facet override content:\n%s", answerPrompt)
	}
	if strings.Contains(answerPrompt, oldPathLoser) {
		t.Fatalf("answerer prompt contains old .kiro/personas loser content:\n%s", answerPrompt)
	}
}

func TestKiroImplLoopAnswerPolicyWhitespaceOnlyOverrideAbortsBeforeAgentCall(t *testing.T) {
	const feature = "demo"
	cwd := setupImplFeatureCWD(t, feature, "- [ ] 1. 単独タスク\n")
	writeFile(t, filepath.Join(cwd, ".rilll-config", "facets", "policies", implAnswerPolicyName+".md"), " \n\t\n")

	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Selected: feature}
	agent := scriptedImplLoopOneRoundAgent()

	if err := runKiroImplLoop(context.Background(), cwd, onEvent, ask, agent); err != nil {
		t.Fatalf("runKiroImplLoop returned error: %v", err)
	}
	if len(agent.Calls) != 0 {
		t.Fatalf("empty answer policy must abort before agent call: got=%d calls", len(agent.Calls))
	}
	if !hasErrorEvent(*eventsPtr, implAnswerPolicyName) {
		t.Fatalf("WorkflowError event should mention policy %q: %+v", implAnswerPolicyName, *eventsPtr)
	}
}

func TestKiroImplLoopFinalReviewReusesResolvedImplPersona(t *testing.T) {
	const feature = "demo"
	const overrideBody = "# final-review impl-direction-answerer override\nfinal-review session B must see this content.\n"
	const oldPathLoser = "# old impl-direction loser\nthis old path must not win final-review.\n"
	cwd := setupImplFeatureCWD(t, feature, "- [ ] 1. 単独タスク\n")
	overridePath := filepath.Join(cwd, ".rilll-config", "facets", "personas", "impl-direction-answerer.md")
	writeFile(t, overridePath, overrideBody)
	writeFile(t, filepath.Join(cwd, ".kiro", "personas", implPersonaName+".md"), oldPathLoser)

	runID := "impl-session-b-final-review"
	ctx := workflowctx.ContextWithRunMetadata(context.Background(), workflowctx.RunMetadata{WorkflowRunID: runID})
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}
	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "commit":
			return commitOKResp(), nil
		case "answerer":
			return answerOKResp(), nil
		case "fix-edit":
			return implFinalReviewFixEditOKResp(), nil
		case "fix-judge":
			return implFinalReviewFixJudgeNoChangeResp(), nil
		case "skill":
			if strings.HasPrefix(call.Prompt, "/kiro-validate-impl") {
				if call.ResumeSessionID == "" {
					return implFinalReviewInquiryResp(), nil
				}
				return implFinalReviewResumeOKResp(), nil
			}
			return skillNoInquiryResp(), nil
		default:
			return provider.AgentResponse{}, nil
		}
	}

	if err := runKiroImplLoop(ctx, cwd, onEvent, ask, agent); err != nil {
		t.Fatalf("runKiroImplLoop returned error: %v", err)
	}

	gotCtx := mustReadImplLoopContextJSON(t, cwd, runID)
	if gotCtx.PersonaName != implPersonaName {
		t.Fatalf("00-context.json personaName = %q, want logical name %q", gotCtx.PersonaName, implPersonaName)
	}
	if gotCtx.PersonaPath != overridePath {
		t.Fatalf("00-context.json personaPath = %q, want project facet override path %q", gotCtx.PersonaPath, overridePath)
	}

	finalAnswererPrompt := firstFinalReviewAnswererPrompt(t, agent.Calls)
	if !strings.Contains(finalAnswererPrompt, overrideBody) {
		t.Fatalf("final-review answerer prompt does not contain resolved impl Persona override:\n%s", finalAnswererPrompt)
	}
	if strings.Contains(finalAnswererPrompt, oldPathLoser) {
		t.Fatalf("final-review answerer prompt contains old .kiro/personas loser content:\n%s", finalAnswererPrompt)
	}
	if strings.Contains(finalAnswererPrompt, overridePath) {
		t.Fatalf("final-review answerer prompt leaked facet source path metadata:\n%s", finalAnswererPrompt)
	}
}

func TestKiroImplLoopFinalReviewKeepsInitialResolvedPersonaWhenOverrideChanges(t *testing.T) {
	const feature = "demo"
	const initialOverride = "# initial impl-direction-answerer override\nfinal-review must keep this pre-resolved content.\n"
	const changedOverride = "# changed impl-direction-answerer override\nfinal-review must not re-resolve this later content.\n"
	cwd := setupImplFeatureCWD(t, feature, "- [ ] 1. 単独タスク\n")
	overridePath := filepath.Join(cwd, ".rilll-config", "facets", "personas", "impl-direction-answerer.md")
	writeFile(t, overridePath, initialOverride)

	runID := "impl-session-b-final-review-reuse"
	ctx := workflowctx.ContextWithRunMetadata(context.Background(), workflowctx.RunMetadata{WorkflowRunID: runID})
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}
	overrideChanged := false
	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "commit":
			if !overrideChanged {
				if err := os.WriteFile(overridePath, []byte(changedOverride), 0o644); err != nil {
					return provider.AgentResponse{}, err
				}
				overrideChanged = true
			}
			return commitOKResp(), nil
		case "answerer":
			return answerOKResp(), nil
		case "fix-edit":
			return implFinalReviewFixEditOKResp(), nil
		case "fix-judge":
			return implFinalReviewFixJudgeNoChangeResp(), nil
		case "skill":
			if strings.HasPrefix(call.Prompt, "/kiro-validate-impl") {
				if call.ResumeSessionID == "" {
					return implFinalReviewInquiryResp(), nil
				}
				return implFinalReviewResumeOKResp(), nil
			}
			if call.ResumeSessionID == "" {
				return skillInquiryResp(), nil
			}
			return skillNoInquiryResp(), nil
		default:
			return provider.AgentResponse{}, nil
		}
	}

	if err := runKiroImplLoop(ctx, cwd, onEvent, ask, agent); err != nil {
		t.Fatalf("runKiroImplLoop returned error: %v", err)
	}
	if !overrideChanged {
		t.Fatal("test setup did not mutate the facet override before final-review")
	}

	gotCtx := mustReadImplLoopContextJSON(t, cwd, runID)
	if gotCtx.PersonaName != implPersonaName {
		t.Fatalf("00-context.json personaName = %q, want logical name %q", gotCtx.PersonaName, implPersonaName)
	}
	if gotCtx.PersonaPath != overridePath {
		t.Fatalf("00-context.json personaPath = %q, want initially resolved override path %q", gotCtx.PersonaPath, overridePath)
	}

	finalAnswererPrompt := firstFinalReviewAnswererPrompt(t, agent.Calls)
	if !strings.Contains(finalAnswererPrompt, initialOverride) {
		t.Fatalf("final-review answerer prompt should keep pre-resolved Persona content:\n%s", finalAnswererPrompt)
	}
	if strings.Contains(finalAnswererPrompt, changedOverride) {
		t.Fatalf("final-review answerer prompt re-resolved changed facet override:\n%s", finalAnswererPrompt)
	}
}

func scriptedImplLoopOneRoundAgent() *scriptedAgent {
	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "commit":
			return commitOKResp(), nil
		case "answerer":
			return answerOKResp(), nil
		case "skill":
			if strings.HasPrefix(call.Prompt, "/kiro-validate-impl") {
				return implFinalReviewOKResp(), nil
			}
			if call.ResumeSessionID == "" {
				return skillInquiryResp(), nil
			}
			return skillNoInquiryResp(), nil
		default:
			return provider.AgentResponse{}, nil
		}
	}
	return agent
}

func mustReadImplLoopContextJSON(t *testing.T, cwd, runID string) implLoopContextJSON {
	t.Helper()
	path := filepath.Join(cwd, ".rilll", "kiro-impl-loop", runID, "00-context.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var got implLoopContextJSON
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal(%s): %v\n%s", path, err, data)
	}
	return got
}

func firstAgentPromptByKind(t *testing.T, calls []recordedAgentCall, kind string) string {
	t.Helper()
	for _, call := range calls {
		if call.Kind == kind {
			return call.Prompt
		}
	}
	t.Fatalf("agent call kind %q was not observed: %v", kind, summarizeCalls(calls))
	return ""
}

func firstFinalReviewAnswererPrompt(t *testing.T, calls []recordedAgentCall) string {
	t.Helper()
	afterValidateImpl := false
	for _, call := range calls {
		if call.Kind == "skill" && strings.HasPrefix(call.Prompt, "/kiro-validate-impl") {
			afterValidateImpl = true
			continue
		}
		if afterValidateImpl && call.Kind == "answerer" {
			return call.Prompt
		}
	}
	t.Fatalf("final-review answerer call was not observed: %v", summarizeCalls(calls))
	return ""
}
