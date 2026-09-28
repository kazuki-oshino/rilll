package apply

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/result"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

type scriptedProposeAgent struct {
	responses []provider.AgentResponse
	errs      []error
	calls     []scriptedProposeAgentCall
}

type scriptedProposeAgentCall struct {
	prompt  string
	cwd     string
	options workflowctx.CallAgentOptions
}

func (f *scriptedProposeAgent) CallAgent(
	_ context.Context,
	prompt string,
	cwd string,
	_ provider.AgentEventCallback,
	_ string,
	options *workflowctx.CallAgentOptions,
) (provider.AgentResponse, error) {
	call := scriptedProposeAgentCall{prompt: prompt, cwd: cwd}
	if options != nil {
		call.options = *options
	}
	f.calls = append(f.calls, call)

	idx := len(f.calls) - 1
	if idx >= len(f.responses) {
		return provider.AgentResponse{}, errors.New("scriptedProposeAgent: response script exhausted")
	}
	var err error
	if idx < len(f.errs) {
		err = f.errs[idx]
	}
	return f.responses[idx], err
}

const proposeLoopAuthLoginDiff = `diff --git a/auth/login.go b/auth/login.go
index 1234567..89abcde 100644
--- a/auth/login.go
+++ b/auth/login.go
@@ -1,3 +1,4 @@
 package auth
 
+// added by review-apply-workflow test
 func Login() {}
`

const proposeLoopOutOfScopeDiff = `diff --git a/billing/charge.go b/billing/charge.go
index 1234567..89abcde 100644
--- a/billing/charge.go
+++ b/billing/charge.go
@@ -1,3 +1,4 @@
 package billing
 
+// out of scope change
 func Charge() {}
`

func proposeLoopTestEnv(t *testing.T, applyRunID, reviewRunID string) (cwd string, ctx context.Context) {
	t.Helper()
	cwd, ctx, reviewRunDir := fastPathTestEnv(t, applyRunID, reviewRunID)
	writeRuleResultJSON(t, reviewRunDir, "auth-rule", result.RuleResult{
		Name:            "auth-rule",
		Description:     "auth checks",
		Applied:         true,
		Verdict:         result.VerdictFindingsReported,
		FindingsPresent: true,
		Summary:         "1 finding",
		Findings: []result.Finding{
			{Severity: result.SeverityError, File: "auth/login.go", Location: "auth/login.go:10", Message: "login guard is missing"},
		},
		Provider:   "claude",
		StartedAt:  "2026-05-04T12:00:00Z",
		FinishedAt: "2026-05-04T12:00:30Z",
		SourceFile: "auth-rule.yaml",
	})
	return cwd, ctx
}

func runProposeLoopWorkflowForTest(
	t *testing.T,
	applyRunID string,
	agent *scriptedProposeAgent,
	applyHook ProposeApplyHook,
) (cwd string, rec ResultRecord, events []workflow.WorkflowEvent, err error) {
	t.Helper()
	const reviewRunID = "review-propose"
	cwd, ctx := proposeLoopTestEnv(t, applyRunID, reviewRunID)
	git := &fakePrecheckGitOps{statusClean: true}
	deps := applyWorkflowDeps{
		gitOps:    git,
		load:      Load,
		agent:     agent,
		applyHook: applyHook,
		coreFn:    newProposeLoopCoreFn(git, Load, agent, applyHook),
	}

	err = runApplyWorkflowOrchestration(ctx, cwd, recordingOnEvent(&events), &workflow.CLIAsk{}, deps)
	rec = readResultJSON(t, cwd, applyRunID)
	assertNoSideEffects(t, git, applyRunID)
	return cwd, rec, events, err
}

func planResp(body string) provider.AgentResponse {
	return provider.AgentResponse{Content: body, Success: true}
}

func diffResp(body string) provider.AgentResponse {
	return provider.AgentResponse{Content: body, Success: true}
}

func TestDiffResponseJSONPrefersStructuredOutputOverPunctuationContent(t *testing.T) {
	resp := provider.AgentResponse{
		Content: "。",
		Success: true,
		StructuredOutput: map[string]any{
			"diff":          proposeLoopAuthLoginDiff,
			"applied_files": []any{"auth/login.go"},
		},
	}

	raw, err := diffResponseJSON(resp)
	if err != nil {
		t.Fatalf("diffResponseJSON returned unexpected error: %v", err)
	}
	var payload patchAgentPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("diffResponseJSON raw output is not JSON: %q: %v", raw, err)
	}
	if payload.Diff != proposeLoopAuthLoginDiff {
		t.Fatalf("payload.Diff = %q, want structured diff", payload.Diff)
	}
	if !reflect.DeepEqual(payload.AppliedFiles, []string{"auth/login.go"}) {
		t.Fatalf("payload.AppliedFiles = %#v, want [auth/login.go]", payload.AppliedFiles)
	}
}

func selfReviewResp(t *testing.T, verdict SelfReviewVerdict, reason string) provider.AgentResponse {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"verdict": string(verdict),
		"reason":  reason,
	})
	if err != nil {
		t.Fatalf("json.Marshal self-review: %v", err)
	}
	return provider.AgentResponse{Content: string(body), Success: true}
}

func assertCycleArtifactExists(t *testing.T, cwd, applyRunID, name string) {
	t.Helper()
	path := filepath.Join(cwd, ".rilll", "review-apply-workflow", applyRunID, name)
	if _, err := filepath.Abs(path); err != nil {
		t.Fatalf("filepath.Abs(%s): %v", path, err)
	}
	if !fileExists(path) {
		t.Fatalf("artifact %s does not exist", path)
	}
}

func assertCycleArtifactMissing(t *testing.T, cwd, applyRunID, name string) {
	t.Helper()
	path := filepath.Join(cwd, ".rilll", "review-apply-workflow", applyRunID, name)
	if fileExists(path) {
		t.Fatalf("artifact %s exists, want missing", path)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestProposeLoop_CycleExhausted(t *testing.T) {
	validDiffJSON := makePatchAgentJSON(t, proposeLoopAuthLoginDiff, []string{"auth/login.go"})
	responses := make([]provider.AgentResponse, 0, MaxRegenerateAttempts*3)
	for i := 1; i <= MaxRegenerateAttempts; i++ {
		responses = append(responses,
			planResp("cycle plan"),
			diffResp(validDiffJSON),
			selfReviewResp(t, SelfReviewRegenerate, "まだ不十分です"),
		)
	}
	agent := &scriptedProposeAgent{responses: responses}

	cwd, rec, _, err := runProposeLoopWorkflowForTest(t, "propose-exhausted", agent, nil)
	if err == nil {
		t.Fatalf("cycle_exhausted で error が nil")
	}
	if !strings.HasPrefix(err.Error(), "[verdict:cycle_exhausted]") {
		t.Fatalf("error prefix = %q, want [verdict:cycle_exhausted]", err.Error())
	}
	if rec.FinalVerdict != VerdictCycleExhausted {
		t.Fatalf("final_verdict=%q, want %q", rec.FinalVerdict, VerdictCycleExhausted)
	}
	if len(rec.Cycles) != MaxRegenerateAttempts {
		t.Fatalf("len(Cycles)=%d, want %d", len(rec.Cycles), MaxRegenerateAttempts)
	}
	for i, cycle := range rec.Cycles {
		wantCycle := i + 1
		if cycle.Cycle != wantCycle {
			t.Errorf("Cycles[%d].Cycle=%d, want %d", i, cycle.Cycle, wantCycle)
		}
		if cycle.Verdict != VerdictUnset {
			t.Errorf("Cycles[%d].Verdict=%q, want VerdictUnset for regenerate", i, cycle.Verdict)
		}
	}
	runDir := filepath.Join(cwd, ".rilll", "review-apply-workflow", "propose-exhausted")
	matches, err := filepath.Glob(filepath.Join(runDir, "cycle-*"))
	if err != nil {
		t.Fatalf("filepath.Glob: %v", err)
	}
	if len(matches) != MaxRegenerateAttempts*3 {
		t.Fatalf("cycle artifact files=%d, want %d (%v)", len(matches), MaxRegenerateAttempts*3, matches)
	}
	if len(agent.calls) != MaxRegenerateAttempts*3 {
		t.Fatalf("agent calls=%d, want %d", len(agent.calls), MaxRegenerateAttempts*3)
	}
}

func TestProposeLoop_OutOfScopeDiff(t *testing.T) {
	agent := &scriptedProposeAgent{responses: []provider.AgentResponse{
		planResp("cycle plan"),
		diffResp(makePatchAgentJSON(t, proposeLoopOutOfScopeDiff, []string{"billing/charge.go"})),
	}}

	cwd, rec, events, err := runProposeLoopWorkflowForTest(t, "propose-outscope", agent, nil)
	if err == nil {
		t.Fatalf("out_of_scope_diff で error が nil")
	}
	if rec.FinalVerdict != VerdictOutOfScopeDiff {
		t.Fatalf("final_verdict=%q, want %q", rec.FinalVerdict, VerdictOutOfScopeDiff)
	}
	if len(rec.Cycles) != 1 {
		t.Fatalf("len(Cycles)=%d, want 1", len(rec.Cycles))
	}
	assertCycleArtifactExists(t, cwd, "propose-outscope", "cycle-1-plan.md")
	assertCycleArtifactExists(t, cwd, "propose-outscope", "cycle-1-diff.patch")
	assertCycleArtifactMissing(t, cwd, "propose-outscope", "cycle-1-review.json")
	if !hasWorkflowTextContaining(events, "billing/charge.go") {
		t.Fatalf("scope violation path event missing: %+v", events)
	}
	if len(agent.calls) != 2 {
		t.Fatalf("agent calls=%d, want 2", len(agent.calls))
	}
}

func TestProposeLoop_ProposalUnparseable(t *testing.T) {
	agent := &scriptedProposeAgent{responses: []provider.AgentResponse{
		planResp("cycle plan"),
		diffResp(makePatchAgentJSON(t, "this is not a unified diff", []string{"auth/login.go"})),
	}}

	cwd, rec, _, err := runProposeLoopWorkflowForTest(t, "propose-unparseable", agent, nil)
	if err == nil {
		t.Fatalf("proposal_unparseable で error が nil")
	}
	if rec.FinalVerdict != VerdictProposalUnparseable {
		t.Fatalf("final_verdict=%q, want %q", rec.FinalVerdict, VerdictProposalUnparseable)
	}
	if len(rec.Cycles) != 1 {
		t.Fatalf("len(Cycles)=%d, want 1", len(rec.Cycles))
	}
	assertCycleArtifactExists(t, cwd, "propose-unparseable", "cycle-1-plan.md")
	assertCycleArtifactExists(t, cwd, "propose-unparseable", "cycle-1-diff.patch")
	assertCycleArtifactMissing(t, cwd, "propose-unparseable", "cycle-1-review.json")
}

func TestProposeLoop_SelfReviewParseFailure(t *testing.T) {
	agent := &scriptedProposeAgent{responses: []provider.AgentResponse{
		planResp("cycle plan"),
		diffResp(makePatchAgentJSON(t, proposeLoopAuthLoginDiff, []string{"auth/login.go"})),
		{Content: "not json", Success: true},
	}}

	cwd, rec, _, err := runProposeLoopWorkflowForTest(t, "propose-review-fail", agent, nil)
	if err == nil {
		t.Fatalf("failed で error が nil")
	}
	if rec.FinalVerdict != VerdictFailed {
		t.Fatalf("final_verdict=%q, want %q", rec.FinalVerdict, VerdictFailed)
	}
	if len(rec.Cycles) != 1 {
		t.Fatalf("len(Cycles)=%d, want 1", len(rec.Cycles))
	}
	assertCycleArtifactExists(t, cwd, "propose-review-fail", "cycle-1-plan.md")
	assertCycleArtifactExists(t, cwd, "propose-review-fail", "cycle-1-diff.patch")
	assertCycleArtifactMissing(t, cwd, "propose-review-fail", "cycle-1-review.json")
}

// writeReviewResultJSON は <runDir>/result.json に review-workflow 全体サマリを書き出す helper。
// previous-response-passing PoC (task 3.2) で Load() が loaded.ReviewOverallSummary に流し込む
// 経路 (Decision 3) を test fixture として再現するために利用する。
func writeReviewResultJSON(t *testing.T, runDir string, summary result.Summary) {
	t.Helper()
	body, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent summary: %v", err)
	}
	path := filepath.Join(runDir, "result.json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

// runProposeLoopWithReviewSummary は propose loop test を任意の reviewRunID と
// 事前 fixture setup callback とで実行する helper。
// task 3.2 で reviewRunDir に result.json を書き込んでから orchestration を起動する経路を必要とするため、
// 既存 runProposeLoopWorkflowForTest を base に「reviewRunDir に対する任意の追加 fixture」を
// 差し込めるよう抽象化する。
func runProposeLoopWithReviewSummary(
	t *testing.T,
	applyRunID string,
	reviewRunID string,
	agent *scriptedProposeAgent,
	applyHook ProposeApplyHook,
	setupReviewRunDir func(reviewRunDir string),
) (cwd string, rec ResultRecord, events []workflow.WorkflowEvent, err error) {
	t.Helper()
	cwd, ctx := proposeLoopTestEnv(t, applyRunID, reviewRunID)
	if setupReviewRunDir != nil {
		reviewRunDir := filepath.Join(cwd, ".rilll", "review-workflow", reviewRunID)
		setupReviewRunDir(reviewRunDir)
	}
	git := &fakePrecheckGitOps{statusClean: true}
	deps := applyWorkflowDeps{
		gitOps:    git,
		load:      Load,
		agent:     agent,
		applyHook: applyHook,
		coreFn:    newProposeLoopCoreFn(git, Load, agent, applyHook),
	}

	err = runApplyWorkflowOrchestration(ctx, cwd, recordingOnEvent(&events), &workflow.CLIAsk{}, deps)
	rec = readResultJSON(t, cwd, applyRunID)
	assertNoSideEffects(t, git, applyRunID)
	return cwd, rec, events, err
}

// TestProposeLoop_PrependsReviewOverallSummaryOnPlanCalls は task 3.2 / Req 7.1, 7.2 の PoC 配線を検証する。
//
// 検証観点:
//   - <ReviewRunDir>/result.json の OverallSummary が Load() 経由で loaded.ReviewOverallSummary に
//     流入し、propose loop 内の plan 呼び出し (only) に options として届くこと。
//   - 全 plan attempt (regenerate を挟んだ後段 cycle も含む) で同一の summary が options に詰まること
//     (Req 2.5 / 6.1 決定性)。Decision 2 に従い summary は loop 開始前に確定し attempt 間で変化しない。
//   - diff / self-review 呼び出しの options には IncludePreviousResponse=true が現れないこと
//     (Decision 2: 局所化方針、他 3 呼び出しは触らない)。
//   - stubAgent (scriptedProposeAgent) は前置を simulate しないため、raw plan prompt の prepend は
//     検証対象外。実際の前置不変式は agent パッケージの TestCallAgentPreviousResponse 系で担保済み。
func TestProposeLoop_PrependsReviewOverallSummaryOnPlanCalls(t *testing.T) {
	const reviewRunID = "review-poc-hello"
	const applyRunID = "propose-poc-hello"
	const summaryText = "hello"

	validDiffJSON := makePatchAgentJSON(t, proposeLoopAuthLoginDiff, []string{"auth/login.go"})
	// 2 regenerate + 1 accept = 3 plan calls (attempt=1,2,3)。各 cycle は plan→diff→self-review の 3 呼び出し。
	agent := &scriptedProposeAgent{responses: []provider.AgentResponse{
		planResp("cycle plan 1"),
		diffResp(validDiffJSON),
		selfReviewResp(t, SelfReviewRegenerate, "まだ不十分"),
		planResp("cycle plan 2"),
		diffResp(validDiffJSON),
		selfReviewResp(t, SelfReviewRegenerate, "まだ不十分"),
		planResp("cycle plan 3"),
		diffResp(validDiffJSON),
		selfReviewResp(t, SelfReviewAccept, "OK"),
	}}
	hook := func(
		_ context.Context,
		_ string,
		_ string,
		_ string,
		_ string,
		_ func(workflow.WorkflowEvent),
		record *ResultRecord,
		_ *LoadResult,
		_ Patch,
	) error {
		record.FinalVerdict = VerdictApplied
		record.FinalSummary = "apply hook test applied"
		return nil
	}

	_, _, _, err := runProposeLoopWithReviewSummary(t, applyRunID, reviewRunID, agent, hook,
		func(reviewRunDir string) {
			writeReviewResultJSON(t, reviewRunDir, result.Summary{
				WorkflowRunID:  reviewRunID,
				OverallVerdict: result.VerdictFindingsReported,
				OverallSummary: summaryText,
				StartedAt:      "2026-05-04T12:00:00Z",
				FinishedAt:     "2026-05-04T12:00:30Z",
			})
		},
	)
	if err != nil {
		t.Fatalf("accept 経路で error 非 nil: %v", err)
	}

	// 9 calls = 3 cycles × {plan, diff, self-review}。plan は index 0, 3, 6。
	if len(agent.calls) != 9 {
		t.Fatalf("agent.calls=%d, want 9 (3 cycles × 3 calls)", len(agent.calls))
	}
	planIndices := []int{0, 3, 6}
	for _, idx := range planIndices {
		call := agent.calls[idx]
		if !call.options.IncludePreviousResponse {
			t.Errorf("calls[%d] (plan): IncludePreviousResponse=false, want true (task 3.2 配線)", idx)
		}
		if call.options.PreviousResponseContent != summaryText {
			t.Errorf("calls[%d] (plan): PreviousResponseContent=%q, want %q (全 attempt で同一 summary)",
				idx, call.options.PreviousResponseContent, summaryText)
		}
		// StepID / StepName は cycle ごとに変動するので、prefix のみ確認する。
		wantStepIDPrefix := "review-apply-plan-"
		if !strings.HasPrefix(call.options.StepID, wantStepIDPrefix) {
			t.Errorf("calls[%d] (plan): StepID=%q, want prefix %q", idx, call.options.StepID, wantStepIDPrefix)
		}
	}
	// diff / self-review (index 1,2,4,5,7,8) は IncludePreviousResponse=true が現れないこと
	// (Decision 2: 局所化方針)。
	nonPlanIndices := []int{1, 2, 4, 5, 7, 8}
	for _, idx := range nonPlanIndices {
		call := agent.calls[idx]
		if call.options.IncludePreviousResponse {
			t.Errorf("calls[%d] (non-plan): IncludePreviousResponse=true, want false (Decision 2 局所化違反)", idx)
		}
		if call.options.PreviousResponseContent != "" {
			t.Errorf("calls[%d] (non-plan): PreviousResponseContent=%q, want \"\" (Decision 2)",
				idx, call.options.PreviousResponseContent)
		}
	}
}

// TestProposeLoop_EmptyReviewOverallSummaryFallsBackToNoOp は task 3.2 / Req 7.4 (および Req 1.4) を検証する。
//
// result.json が存在しない (= loaded.ReviewOverallSummary が空文字列) ケースで、
// plan options は IncludePreviousResponse=true を保持しつつ PreviousResponseContent=""
// を渡すことで agent.CallAgent 側の no-op 分岐 (Req 1.4) に倒れ、既存挙動 (raw plan prompt 素通し)
// が維持されることを確認する。
//
// stubAgent は前置を simulate しないため、prompt は RenderPlan の出力 (例: 改行と spec ID を含むテンプレ)
// そのままが届く。本テストでは「options 側の no-op 経路成立」を検証することで、
// agent.CallAgent (task 3.1 で網羅) の no-op 不変式と組み合わさり Req 7.4 を満たす。
func TestProposeLoop_EmptyReviewOverallSummaryFallsBackToNoOp(t *testing.T) {
	const reviewRunID = "review-poc-empty"
	const applyRunID = "propose-poc-empty"

	agent := &scriptedProposeAgent{responses: []provider.AgentResponse{
		planResp("cycle plan"),
		diffResp(makePatchAgentJSON(t, proposeLoopAuthLoginDiff, []string{"auth/login.go"})),
		selfReviewResp(t, SelfReviewAccept, "OK"),
	}}
	hook := func(
		_ context.Context,
		_ string,
		_ string,
		_ string,
		_ string,
		_ func(workflow.WorkflowEvent),
		record *ResultRecord,
		_ *LoadResult,
		_ Patch,
	) error {
		record.FinalVerdict = VerdictApplied
		record.FinalSummary = "apply hook test applied"
		return nil
	}

	// 意図的に result.json を書き出さない → Load() は best-effort で空文字列を採用する。
	_, _, _, err := runProposeLoopWithReviewSummary(t, applyRunID, reviewRunID, agent, hook, nil)
	if err != nil {
		t.Fatalf("accept 経路で error 非 nil: %v", err)
	}

	if len(agent.calls) != 3 {
		t.Fatalf("agent.calls=%d, want 3 (1 cycle × 3 calls)", len(agent.calls))
	}
	planCall := agent.calls[0]
	if !planCall.options.IncludePreviousResponse {
		t.Errorf("plan options.IncludePreviousResponse=false, want true (空 summary でも flag は常時 true)")
	}
	if planCall.options.PreviousResponseContent != "" {
		t.Errorf("plan options.PreviousResponseContent=%q, want \"\" (空 summary → no-op)",
			planCall.options.PreviousResponseContent)
	}
	// stubAgent は前置を simulate しないため、prompt は RenderPlan の素の出力。
	// "# Previous Response" prefix を含まないことを negative check として確認する。
	if strings.HasPrefix(planCall.prompt, "# Previous Response") {
		t.Errorf("plan raw prompt が `# Previous Response` で始まる: stubAgent は前置を simulate しない契約")
	}
}

func TestProposeLoop_AcceptTransitionsToApplyHook(t *testing.T) {
	agent := &scriptedProposeAgent{responses: []provider.AgentResponse{
		planResp("cycle plan"),
		diffResp(makePatchAgentJSON(t, proposeLoopAuthLoginDiff, []string{"auth/login.go"})),
		selfReviewResp(t, SelfReviewAccept, "適用してよい"),
	}}
	var hookCalls int
	var gotFiles []string
	var gotRuleNames []string
	hook := func(
		_ context.Context,
		_ string,
		_ string,
		_ string,
		_ string,
		_ func(workflow.WorkflowEvent),
		record *ResultRecord,
		loaded *LoadResult,
		patch Patch,
	) error {
		hookCalls++
		gotFiles = append(gotFiles, patch.TargetFiles...)
		for _, rule := range loaded.TargetRules {
			gotRuleNames = append(gotRuleNames, rule.Name)
		}
		record.FinalVerdict = VerdictApplied
		record.FinalSummary = "apply hook test applied"
		return nil
	}

	cwd, rec, _, err := runProposeLoopWorkflowForTest(t, "propose-accept", agent, hook)
	if err != nil {
		t.Fatalf("accept/apply hook 経路で error 非 nil: %v", err)
	}
	if hookCalls != 1 {
		t.Fatalf("applyHook calls=%d, want 1", hookCalls)
	}
	if !reflect.DeepEqual(gotFiles, []string{"auth/login.go"}) {
		t.Fatalf("hook patch.TargetFiles=%v, want [auth/login.go]", gotFiles)
	}
	if !reflect.DeepEqual(gotRuleNames, []string{"auth-rule"}) {
		t.Fatalf("hook rule names=%v, want [auth-rule]", gotRuleNames)
	}
	if rec.FinalVerdict != VerdictApplied {
		t.Fatalf("final_verdict=%q, want %q", rec.FinalVerdict, VerdictApplied)
	}
	if len(rec.Cycles) != 1 {
		t.Fatalf("len(Cycles)=%d, want 1", len(rec.Cycles))
	}
	if rec.Cycles[0].Verdict != VerdictApplied {
		t.Fatalf("cycle verdict=%q, want %q", rec.Cycles[0].Verdict, VerdictApplied)
	}
	assertCycleArtifactExists(t, cwd, "propose-accept", "cycle-1-plan.md")
	assertCycleArtifactExists(t, cwd, "propose-accept", "cycle-1-diff.patch")
	assertCycleArtifactExists(t, cwd, "propose-accept", "cycle-1-review.json")
}
