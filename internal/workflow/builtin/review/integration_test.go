// Package review のテスト本ファイルは task 4.1 / design.md §Testing Strategy
// (Integration Tests) の end-to-end 統合テストを実装する。
//
// 検証観点 (tasks.md 4.1 観察可能完了条件):
//
//   - happy path: precheck 全段階通過 → 3 rule 並行実行 (本番 runExecutor 経由) →
//     `<runDir>/result.json` 出力 → akashic に 3 件の agent_call (StepID = rule.Name) と
//     4 件の run_artifact (3 review + 1 result) が記録される (Req 6.4, 6.5)
//   - precheck 失敗経路 (gh CheckCLI fail / rules 0 件 / PR 不在): result.json 出力 +
//     `OverallVerdict=precheck_failed` + 副作用 (executor 起動 / per-rule artifact /
//     agent_call) が一切発生しない (Req 3.3, 3.4)
//   - Sequential 互換 (`Sequential("review-and-apply", review.Workflow(agent), <fake apply>)`):
//     前段 review が `findings_reported` → 後段 fake apply 起動 + review の per-rule
//     artifact と result.json が同 run-id 配下に揃う (Req 7.3); 前段が `precheck_failed` /
//     `failed` のとき後段は起動しない (Req 7.4)
//
// 本ファイルは workflow_test.go (executor を mock した unit-style テスト) と異なり、
// 本番実装の `runExecutor` を deps.runExec に注入することで、precheck → 並行 executor →
// per-rule artifact 書き出し → 集約 verdict → result.json defer の積分契約を E2E で検証する。
//
// 使用 fake (executor_test.go / precheck_test.go から再利用):
//
//   - fakeGhClient: precheckGhClient を満たす in-process gh ラッパ fake
//   - fakeRuleAgent: workflowctx.AgentCaller + AkashicRecorderProvider を満たす agent fake
//   - reviewRecorder: akashic.Recorder の test 専用 fake (artifacts / raw_events を蓄積)
//   - setupRillRunDir / happyRule / stubLoaderOK / stubLoaderErr / happyPRMeta
package review

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/agent"
	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/gh"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/result"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/rules"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// makeIntegrationDeps は precheck 部分のみ fake (gh + loader)、executor は本番
// `runExecutor` を使う E2E 統合テスト用の deps を組み立てる。
//
// workflow_test.go の `makeReviewDeps` と異なり、第 3 引数の executor を本番実装に
// 固定することで「precheck 通過後に実際に runExecutor が起動して per-rule artifact を
// 書き出す」契約を E2E で検証可能にする (Req 6.4, 6.5)。
func makeIntegrationDeps(ghc precheckGhClient, loader precheckRulesLoader) reviewWorkflowDeps {
	return reviewWorkflowDeps{
		gh:        ghc,
		loadRules: loader,
		runExec:   runExecutor, // 本番実装 (workflow_test.go では mock を渡している)
	}
}

// findingsReportedResponse は fakeRuleAgent.behaviors に流し込む「findings あり」応答。
// applied=true / findings_present=true で 1 件の info finding を含む構造化出力を返す。
func findingsReportedResponse() provider.AgentResponse {
	return provider.AgentResponse{
		Success: true,
		Content: `{"applied": true, "findings_present": true, "summary": "found 1 issue", "details": [{"severity": "warn", "file": "a.go", "location": "a.go:1", "message": "test finding"}]}`,
		StructuredOutput: map[string]any{
			"applied":          true,
			"findings_present": true,
			"summary":          "found 1 issue",
			"details": []any{
				map[string]any{
					"severity": "warn",
					"file":     "a.go",
					"location": "a.go:1",
					"message":  "test finding",
				},
			},
		},
	}
}

// providerErrorBehavior は agent.CallError{Category=provider_error} を返す behavior。
// Sequential failed verdict 経路の検証に使う (全 rule failed → OverallVerdict=failed)。
func providerErrorBehavior(_ context.Context, _ *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
	return provider.AgentResponse{}, &agent.CallError{
		Category: agent.FailureCategoryProviderError,
		Err:      errors.New("integration test: simulated provider error"),
	}
}

// listReviewRunDirs は `<cwd>/.rilll/review-workflow/*` 配下のサブディレクトリ一覧を返す。
// Sequential テストで「review-workflow が単一 run-id 配下に成果物を集約する」契約を
// 検証する用途。glob 結果はソートされる (deterministic)。
func listReviewRunDirs(t *testing.T, cwd string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(cwd, ".rilll", "review-workflow", "*"))
	if err != nil {
		t.Fatalf("glob run dirs: %v", err)
	}
	sort.Strings(matches)
	return matches
}

// ---------------------- Happy path E2E (Req 6.4, 6.5) ----------------------

// TestIntegration_HappyPath3RulesEndToEnd は precheck 全段階通過 + 3 rule 並行実行 +
// result.json 出力 + akashic 記録 (3 agent_call + 4 run_artifact) を本番 runExecutor
// 経由で E2E 検証する (tasks.md 4.1 / Req 6.4, 6.5)。
//
// 既存の TestRunReviewWorkflow_HappyPathAggregatesNoFindings は executor を mock 化
// して result.json defer 経路のみを検証していたが、本テストは runExecutor が実際に
// 並行起動 → per-rule artifact 書き出し → RecordRunArtifact(review) を呼ぶ経路を含む
// 完全な E2E 統合契約を検証する。
func TestIntegration_HappyPath3RulesEndToEnd(t *testing.T) {
	t.Parallel()

	cwd, runDir := setupRillRunDir(t, "run-int-happy")
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)

	// fake gh: 全 4 段階 (CheckCLI / ListPRs / ViewPR / DiffPR) で成功させる。
	fakeGh := &fakeGhClient{
		viewPRMeta: happyPRMeta(),
		diffPRStr:  "diff --git a/a.go b/a.go\n",
	}

	// 3 件の rule (ファイル名昇順 alpha / bravo / charlie)。
	loadedRules := []rules.Rule{
		happyRule("alpha-rule", "alpha-rule.yaml"),
		happyRule("bravo-rule", "bravo-rule.yaml"),
		happyRule("charlie-rule", "charlie-rule.yaml"),
	}
	loader := stubLoaderOK(loadedRules, nil)

	deps := makeIntegrationDeps(fakeGh, loader)
	ask := &workflow.CLIAsk{Values: map[string]string{"pr": "42"}}

	err := runReviewWorkflowWithRunMetadata(t, context.Background(), "run-int-happy", cwd, rec, ag, ask, deps)

	// no_findings (= IsIntent=true) は nil error を返す契約 (Req 7.3)。
	if err != nil {
		t.Fatalf("orchestration error = %v, want nil (no_findings は IsIntent=true)", err)
	}

	// --- result.json 検証 (Req 6.3) ---
	sum := readResultJSON(t, runDir)
	if sum.OverallVerdict != result.VerdictNoFindings {
		t.Errorf("OverallVerdict = %q, want %q", sum.OverallVerdict, result.VerdictNoFindings)
	}
	if sum.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", sum.PRNumber)
	}
	if sum.WorkflowRunID != "run-int-happy" {
		t.Errorf("WorkflowRunID = %q, want %q", sum.WorkflowRunID, "run-int-happy")
	}
	if len(sum.Rules) != 3 {
		t.Fatalf("Summary.Rules length = %d, want 3", len(sum.Rules))
	}
	// rule 順序がファイル名昇順 (= loader 契約) で保たれていること。
	for i, r := range loadedRules {
		if sum.Rules[i].Name != r.Name {
			t.Errorf("Summary.Rules[%d].Name = %q, want %q (ファイル名昇順)", i, sum.Rules[i].Name, r.Name)
		}
		if sum.Rules[i].Verdict != result.VerdictNoFindings {
			t.Errorf("Summary.Rules[%d].Verdict = %q, want %q", i, sum.Rules[i].Verdict, result.VerdictNoFindings)
		}
	}

	// --- agent_calls 検証 (Req 6.5: StepID = rule.Name × 3 件) ---
	ag.mu.Lock()
	calls := append([]fakeCallRecord(nil), ag.calls...)
	ag.mu.Unlock()
	if len(calls) != 3 {
		t.Errorf("agent_call 件数 = %d, want 3 (rule 数と一致)", len(calls))
	}
	gotStepIDs := make(map[string]int)
	for _, c := range calls {
		gotStepIDs[c.StepID]++
	}
	for _, r := range loadedRules {
		if gotStepIDs[r.Name] != 1 {
			t.Errorf("StepID=%q の呼び出し回数 = %d, want 1 (Req 6.5)", r.Name, gotStepIDs[r.Name])
		}
	}

	// --- run_artifacts 検証 (Req 6.4: 3 review + 1 result = 4 件) ---
	arts := rec.snapshotArtifacts()
	if len(arts) != 4 {
		t.Errorf("RecordRunArtifact 件数 = %d, want 4 (3 review + 1 result)", len(arts))
	}
	var nReview, nResult int
	for _, a := range arts {
		switch a.Kind {
		case akashic.ArtifactKindReview:
			nReview++
		case akashic.ArtifactKindResult:
			nResult++
		}
		if a.WorkflowRunID != "run-int-happy" {
			t.Errorf("artifact.WorkflowRunID = %q, want %q", a.WorkflowRunID, "run-int-happy")
		}
	}
	if nReview != 3 {
		t.Errorf("review artifact 件数 = %d, want 3", nReview)
	}
	if nResult != 1 {
		t.Errorf("result artifact 件数 = %d, want 1", nResult)
	}

	// --- per-rule artifact ファイルが runDir に存在すること (Req 6.1) ---
	for _, r := range loadedRules {
		path := filepath.Join(runDir, r.Name+".json")
		body, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("per-rule artifact %s 不在: %v", path, err)
			continue
		}
		var parsed result.RuleResult
		if uerr := json.Unmarshal(body, &parsed); uerr != nil {
			t.Errorf("per-rule artifact %s parse 失敗: %v", path, uerr)
			continue
		}
		if parsed.Name != r.Name {
			t.Errorf("per-rule artifact %s Name = %q, want %q", path, parsed.Name, r.Name)
		}
	}
}

// ---------------------- Precheck 失敗 (副作用ゼロ契約 Req 3.3) ----------------------

// runPrecheckFailureCase は precheck 失敗 3 ケース (gh / rules / PR) で共通する
// 観測ロジックを集約する: result.json 出力 + OverallVerdict=precheck_failed +
// 副作用ゼロ (executor 未起動 / per-rule artifact 0 件 / agent_call 0 件) を検証する。
//
// `setupCase` がケース固有の fake gh / loader を構築する。
func runPrecheckFailureCase(t *testing.T, name string, setupCase func() (precheckGhClient, precheckRulesLoader)) {
	t.Helper()

	cwd, runDir := setupRillRunDir(t, "run-int-pre-"+name)
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)

	ghc, loader := setupCase()
	deps := makeIntegrationDeps(ghc, loader)
	ask := &workflow.CLIAsk{Values: map[string]string{"pr": "999"}}

	err := runReviewWorkflowWithRunMetadata(t, context.Background(), "run-int-pre-"+name, cwd, rec, ag, ask, deps)

	// Req 7.4: precheck_failed は Sequential 失敗伝播のため non-nil error を返す。
	if err == nil {
		t.Fatalf("orchestration error = nil, want non-nil (precheck_failed → Sequential 失敗伝播)")
	}
	if !strings.Contains(err.Error(), "[verdict:precheck_failed]") {
		t.Errorf("err = %q, want prefix [verdict:precheck_failed]", err.Error())
	}

	// result.json 出力 + OverallVerdict=precheck_failed (Req 3.4)。
	sum := readResultJSON(t, runDir)
	if sum.OverallVerdict != result.VerdictPrecheckFailed {
		t.Errorf("OverallVerdict = %q, want %q", sum.OverallVerdict, result.VerdictPrecheckFailed)
	}
	if sum.OverallSummary == "" {
		t.Errorf("OverallSummary is empty (人間可読 summary が無い)")
	}

	// Req 3.3: 副作用 (executor 起動 / per-rule artifact / agent_call) が一切走らないこと。
	if files := listPerRuleArtifactFiles(t, runDir); len(files) != 0 {
		t.Errorf("per-rule artifact files = %v, want [] (precheck 失敗で副作用禁止)", files)
	}
	ag.mu.Lock()
	ncalls := len(ag.calls)
	ag.mu.Unlock()
	if ncalls != 0 {
		t.Errorf("agent_call 件数 = %d, want 0 (precheck 失敗で executor 起動禁止 Req 3.3)", ncalls)
	}

	// run_artifacts は result.json の 1 件のみ (per-rule review artifact は記録されない)。
	arts := rec.snapshotArtifacts()
	var nReview, nResult int
	for _, a := range arts {
		switch a.Kind {
		case akashic.ArtifactKindReview:
			nReview++
		case akashic.ArtifactKindResult:
			nResult++
		}
	}
	if nReview != 0 {
		t.Errorf("review artifact 件数 = %d, want 0 (precheck 失敗で per-rule artifact 記録禁止)", nReview)
	}
	if nResult != 1 {
		t.Errorf("result artifact 件数 = %d, want 1 (Req 3.4)", nResult)
	}
}

// TestIntegration_PrecheckFailureGhCheckCLI は gh CheckCLI 失敗時の precheck 副作用ゼロ契約を検証する。
func TestIntegration_PrecheckFailureGhCheckCLI(t *testing.T) {
	t.Parallel()
	runPrecheckFailureCase(t, "gh-cli", func() (precheckGhClient, precheckRulesLoader) {
		return &fakeGhClient{checkCLIErr: gh.ErrCLIMissing},
			stubLoaderOK([]rules.Rule{happyRule("alpha", "alpha.yaml")}, nil)
	})
}

// TestIntegration_PrecheckFailureNoRulesFound は rule 0 件時の precheck 副作用ゼロ契約を検証する。
func TestIntegration_PrecheckFailureNoRulesFound(t *testing.T) {
	t.Parallel()
	runPrecheckFailureCase(t, "no-rules", func() (precheckGhClient, precheckRulesLoader) {
		return &fakeGhClient{viewPRMeta: happyPRMeta(), diffPRStr: "diff"},
			stubLoaderErr(rules.ErrNoRulesFound, nil)
	})
}

// TestIntegration_PrecheckFailurePRNotFound は ViewPR が ErrPRNotFound を返した場合の
// precheck 副作用ゼロ契約を検証する。
func TestIntegration_PrecheckFailurePRNotFound(t *testing.T) {
	t.Parallel()
	runPrecheckFailureCase(t, "pr-missing", func() (precheckGhClient, precheckRulesLoader) {
		return &fakeGhClient{viewPRErr: gh.ErrPRNotFound},
			stubLoaderOK([]rules.Rule{happyRule("alpha", "alpha.yaml")}, nil)
	})
}

// ---------------------- Sequential 互換 (Req 7.3, 7.4) ----------------------

// buildIntegrationReviewDef は本番経路の review.Workflow と等価な Definition を組み立てる。
// 内部で `runReviewWorkflowOrchestration` を deps 経由で呼ぶ (テスト用 fake gh / loader を注入)。
//
// `Recorder: rec` を明示することで、Definition.Run が ctx に正しい recorder を流し、
// `RecordRunArtifact` が `rec` に集約される。
func buildIntegrationReviewDef(rec akashic.Recorder, ag workflowctx.AgentCaller, deps reviewWorkflowDeps) workflow.Definition {
	return workflow.Definition{
		ID:    reviewWorkflowID,
		Label: reviewWorkflowLabel,
		Args: []workflow.ArgSpec{
			{Key: askPRKey, Description: askPRDescription, Required: true},
		},
		Recorder: rec,
		Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			return runReviewWorkflowOrchestration(ctx, cwd, onEvent, ask, ag, deps)
		},
	}
}

// TestIntegration_SequentialRunsApplyOnFindingsReported は review-workflow が
// findings_reported を返したとき後段 fake apply が起動し、両 run の artifact が
// review-workflow の単一 run-id 配下 (`<cwd>/.rilll/review-workflow/<runID>/`) に
// 揃うことを E2E で検証する (Req 7.3)。
func TestIntegration_SequentialRunsApplyOnFindingsReported(t *testing.T) {
	t.Parallel()

	cwd, _ := setupRillRunDir(t, "ignored-runid-seq-ok")
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)
	// alpha rule に findings あり応答を仕込む。bravo は default (no_findings) だが、
	// findings_reported 1 件で OverallVerdict=findings_reported に集約される (Req 6.3 優先順位)。
	ag.behaviors["alpha-rule"] = func(_ context.Context, _ *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
		return findingsReportedResponse(), nil
	}

	fakeGh := &fakeGhClient{viewPRMeta: happyPRMeta(), diffPRStr: "diff"}
	loadedRules := []rules.Rule{
		happyRule("alpha-rule", "alpha-rule.yaml"),
		happyRule("bravo-rule", "bravo-rule.yaml"),
	}
	loader := stubLoaderOK(loadedRules, nil)
	deps := makeIntegrationDeps(fakeGh, loader)

	applyCalls := 0
	apply := workflow.Definition{
		ID:    "fake-apply",
		Label: "fake apply",
		Fn: func(_ context.Context, _ string, _ func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			applyCalls++
			return nil
		},
		Recorder: rec,
	}

	reviewDef := buildIntegrationReviewDef(rec, ag, deps)
	seq := workflow.Sequential("review-and-apply", "review-and-apply", ag, reviewDef, apply)

	err := seq.Run(context.Background(), cwd, nil, &workflow.CLIAsk{Values: map[string]string{"pr": "42"}})
	if err != nil {
		t.Fatalf("seq.Run error = %v, want nil (findings_reported は IsIntent=true)", err)
	}
	if applyCalls != 1 {
		t.Errorf("apply called %d times, want 1 (Req 7.3)", applyCalls)
	}

	// review-workflow run-id 配下に result.json + per-rule artifact が揃うこと (Req 7.3)。
	runDirs := listReviewRunDirs(t, cwd)
	// setupRillRunDir で作った "ignored-runid-seq-ok" + Sequential 経由で動的採番された
	// review-workflow の本番 run-id の 2 ディレクトリが存在しうる。本番 run-id 側に
	// result.json + 全 rule の per-rule artifact が揃っていることを確認する。
	if len(runDirs) == 0 {
		t.Fatalf("no run-id directories under %s/.rilll/review-workflow/", cwd)
	}
	var foundRunID string
	for _, dir := range runDirs {
		if dir == filepath.Join(cwd, ".rilll", "review-workflow", "ignored-runid-seq-ok") {
			continue // setupRillRunDir で先に作った placeholder は無視
		}
		// per-rule artifact が全 rule 揃っているディレクトリを探す。
		allPresent := true
		for _, r := range loadedRules {
			if _, err := os.Stat(filepath.Join(dir, r.Name+".json")); err != nil {
				allPresent = false
				break
			}
		}
		if !allPresent {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "result.json")); err != nil {
			continue
		}
		foundRunID = dir
		break
	}
	if foundRunID == "" {
		t.Fatalf("review-workflow の run-id 配下に result.json + 全 per-rule artifact が揃ったディレクトリが見つからない (run dirs: %v)", runDirs)
	}

	// Sequential 経路でも recorder には review artifact (= rule 件数) + result artifact (1) が
	// 記録されている。
	arts := rec.snapshotArtifacts()
	var nReview, nResult int
	for _, a := range arts {
		switch a.Kind {
		case akashic.ArtifactKindReview:
			nReview++
		case akashic.ArtifactKindResult:
			nResult++
		}
	}
	if nReview != len(loadedRules) {
		t.Errorf("review artifact 件数 = %d, want %d", nReview, len(loadedRules))
	}
	if nResult < 1 {
		t.Errorf("result artifact 件数 = %d, want >=1", nResult)
	}
}

// TestIntegration_SequentialSkipsApplyOnPrecheckFailed は review-workflow が
// precheck_failed のとき後段 fake apply が起動しないことを E2E で検証する (Req 7.4)。
func TestIntegration_SequentialSkipsApplyOnPrecheckFailed(t *testing.T) {
	t.Parallel()

	cwd, _ := setupRillRunDir(t, "ignored-runid-seq-pre")
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)

	// gh CheckCLI を失敗させ precheck_failed を強制する。
	fakeGh := &fakeGhClient{checkCLIErr: gh.ErrCLIMissing}
	loader := stubLoaderOK([]rules.Rule{happyRule("alpha", "alpha.yaml")}, nil)
	deps := makeIntegrationDeps(fakeGh, loader)

	applyCalls := 0
	apply := workflow.Definition{
		ID:    "fake-apply",
		Label: "fake apply",
		Fn: func(_ context.Context, _ string, _ func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			applyCalls++
			return nil
		},
		Recorder: rec,
	}

	reviewDef := buildIntegrationReviewDef(rec, ag, deps)
	seq := workflow.Sequential("review-and-apply", "review-and-apply", ag, reviewDef, apply)

	err := seq.Run(context.Background(), cwd, nil, &workflow.CLIAsk{Values: map[string]string{"pr": "42"}})

	if err == nil {
		t.Fatalf("seq.Run error = nil, want non-nil (precheck_failed → Sequential 後段起動禁止)")
	}
	if applyCalls != 0 {
		t.Errorf("apply called %d times, want 0 (Req 7.4)", applyCalls)
	}
	// agent_call は 0 件 (precheck で停止)。
	ag.mu.Lock()
	ncalls := len(ag.calls)
	ag.mu.Unlock()
	if ncalls != 0 {
		t.Errorf("agent_call 件数 = %d, want 0 (precheck 失敗で executor 起動禁止)", ncalls)
	}
}

// TestIntegration_SequentialSkipsApplyOnAllFailed は executor が全 rule failed を
// 返したとき (= OverallVerdict=failed) Sequential 後段が起動しないことを E2E で検証する (Req 7.4)。
func TestIntegration_SequentialSkipsApplyOnAllFailed(t *testing.T) {
	t.Parallel()

	cwd, _ := setupRillRunDir(t, "ignored-runid-seq-fail")
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)

	// 全 rule で provider error を返し OverallVerdict=failed を強制する。
	loadedRules := []rules.Rule{
		happyRule("alpha-rule", "alpha-rule.yaml"),
		happyRule("bravo-rule", "bravo-rule.yaml"),
	}
	for _, r := range loadedRules {
		ag.behaviors[r.Name] = providerErrorBehavior
	}

	fakeGh := &fakeGhClient{viewPRMeta: happyPRMeta(), diffPRStr: "diff"}
	loader := stubLoaderOK(loadedRules, nil)
	deps := makeIntegrationDeps(fakeGh, loader)

	applyCalls := 0
	apply := workflow.Definition{
		ID:    "fake-apply",
		Label: "fake apply",
		Fn: func(_ context.Context, _ string, _ func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			applyCalls++
			return nil
		},
		Recorder: rec,
	}

	reviewDef := buildIntegrationReviewDef(rec, ag, deps)
	seq := workflow.Sequential("review-and-apply", "review-and-apply", ag, reviewDef, apply)

	err := seq.Run(context.Background(), cwd, nil, &workflow.CLIAsk{Values: map[string]string{"pr": "42"}})

	if err == nil {
		t.Fatalf("seq.Run error = nil, want non-nil (failed → Sequential 後段起動禁止)")
	}
	if !strings.Contains(err.Error(), "[verdict:failed]") {
		t.Errorf("err = %q, want prefix [verdict:failed]", err.Error())
	}
	if applyCalls != 0 {
		t.Errorf("apply called %d times, want 0 (Req 7.4)", applyCalls)
	}
	// agent_call は 2 件 (executor が rule 数だけ呼んだ)。
	ag.mu.Lock()
	ncalls := len(ag.calls)
	ag.mu.Unlock()
	if ncalls != len(loadedRules) {
		t.Errorf("agent_call 件数 = %d, want %d", ncalls, len(loadedRules))
	}
}
