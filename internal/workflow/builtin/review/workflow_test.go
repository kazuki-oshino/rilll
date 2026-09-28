// Package review のテスト本ファイルは review.Workflow workflow.Definition と
// runReviewWorkflowOrchestration の precheck → executor → result.json defer
// オーケストレーションを検証する (tasks.md 3.1 / design.md §review.Workflow /
// Req 3.3, 3.4, 6.3, 6.4, 7.2, 7.3, 7.4)。
//
// 検証観点 (tasks.md 3.1 観察可能完了条件):
//
//   - workflow.Definition フィールドの基本契約 (ID="review-workflow" / Label / Args (pr,
//     Required=true) / Hidden=false / Recorder / Fn 非 nil)
//   - precheck 失敗経路: `<runDir>/result.json` が存在し OverallVerdict=precheck_failed、
//     per-rule artifact が一切書き出されないこと (Req 3.3, 3.4)
//   - precheck 成功経路: `<runDir>/result.json` + per-rule `<rule-name>.json` が揃い、
//     OverallVerdict が `findings_reported|no_findings|failed|cancelled` のいずれかに
//     正しく確定すること (Req 6.3, 6.4)
//   - precheck 失敗時に Sequential 後段が起動しない契約に整合する error が返ること (Req 7.4)
//   - Sequential 合成時に同 run-id 配下に artifact が揃うこと (Req 7.3)
package review

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/gh"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/result"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/rules"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// happyPRMeta は precheck 成功経路で fakeGhClient.viewPRMeta に流し込む PR メタ。
func happyPRMeta() gh.PRMeta {
	return gh.PRMeta{
		Number:    42,
		Title:     "happy",
		Body:      "body",
		Files:     []string{"a.go"},
		Additions: 1,
		Deletions: 0,
	}
}

// makeReviewDeps は本番の review.Workflow と等価なオーケストレーションテストを
// 行うための DI ヘルパ。fake gh / fake loadRules / fake executor を渡せる。
func makeReviewDeps(ghc precheckGhClient, loader precheckRulesLoader, exec func(ctx context.Context, in ExecutorInput, onEvent func(workflow.WorkflowEvent)) ExecutorOutput) reviewWorkflowDeps {
	return reviewWorkflowDeps{
		gh:        ghc,
		loadRules: loader,
		runExec:   exec,
	}
}

// runReviewWorkflowWithRunMetadata は ctx に RunMetadata + Recorder を注入してから
// runReviewWorkflowOrchestration を呼ぶテストヘルパ。本番経路 (Definition.Run) と
// 等価な ctx を構築する。
func runReviewWorkflowWithRunMetadata(t *testing.T, ctx context.Context, runID, cwd string, recorder akashic.Recorder, agent workflowctx.AgentCaller, ask workflow.WorkflowAsk, deps reviewWorkflowDeps) error {
	t.Helper()
	ctx = workflowctx.ContextWithRunMetadata(ctx, workflowctx.RunMetadata{
		WorkflowRunID: runID,
		WorkflowID:    reviewWorkflowID,
		WorkflowName:  reviewWorkflowLabel,
		CWD:           cwd,
	})
	ctx = workflow.ContextWithRecorder(ctx, recorder)
	return runReviewWorkflowOrchestration(ctx, cwd, nil, ask, agent, deps)
}

// readResultJSON は runDir 直下の result.json をパースして Summary を返す。
// ファイル不在 / JSON 不正は t.Fatalf で停止する (defer 経路の result.json 出力を契約として要求)。
func readResultJSON(t *testing.T, runDir string) result.Summary {
	t.Helper()
	path := filepath.Join(runDir, "result.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var sum result.Summary
	if err := json.Unmarshal(body, &sum); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return sum
}

// listPerRuleArtifactFiles は runDir 配下の `*.json` のうち result.json 以外を返す。
// precheck 失敗時に per-rule artifact が一切書き出されないことを検証する用途。
func listPerRuleArtifactFiles(t *testing.T, runDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(runDir)
	if err != nil {
		// runDir 不在も「per-rule artifact 0 件」として扱う。
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("readdir %s: %v", runDir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "result.json" {
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		out = append(out, name)
	}
	return out
}

// ---------------------- Definition フィールド契約 (Req 7.1, 7.2) ----------------------

// TestWorkflow_DefinitionFields は Workflow() が返す Definition の基本契約を検証する
// (tasks.md 3.1 / Req 7.1, 7.2)。
func TestWorkflow_DefinitionFields(t *testing.T) {
	t.Parallel()

	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)
	def := Workflow(ag)

	if def.ID != "review-workflow" {
		t.Errorf("Definition.ID = %q, want %q", def.ID, "review-workflow")
	}
	if def.Label == "" {
		t.Errorf("Definition.Label is empty")
	}
	if def.Hidden {
		t.Errorf("Definition.Hidden = true, want false (Req 7.1: rilll list / TUI 表示)")
	}
	if def.Fn == nil {
		t.Errorf("Definition.Fn is nil")
	}
	if def.Recorder == nil {
		t.Errorf("Definition.Recorder is nil (RecorderFromAgent から取得されるべき)")
	}
	// Args に pr 引数 (Required=true) が含まれること。
	if len(def.Args) != 1 {
		t.Fatalf("Definition.Args length = %d, want 1", len(def.Args))
	}
	a := def.Args[0]
	if a.Key != "pr" {
		t.Errorf("Args[0].Key = %q, want %q", a.Key, "pr")
	}
	if !a.Required {
		t.Errorf("Args[0].Required = false, want true")
	}
	if a.Description == "" {
		t.Errorf("Args[0].Description is empty")
	}
}

// ---------------------- precheck 失敗経路 (Req 3.3, 3.4, 7.4) ----------------------

// TestRunReviewWorkflow_PrecheckFailureWritesResultJSON は gh CheckCLI 失敗で
// runDir/result.json が出力され、OverallVerdict=precheck_failed になり、
// per-rule artifact が一切書き出されないことを検証する (Req 3.3, 3.4)。
func TestRunReviewWorkflow_PrecheckFailureWritesResultJSON(t *testing.T) {
	t.Parallel()

	cwd, runDir := setupRillRunDir(t, "run-precheck-fail")
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)

	// gh CheckCLI を ErrCLIMissing で fail させる。後段 (loadRules / executor) は呼ばれない契約。
	fakeGh := &fakeGhClient{checkCLIErr: gh.ErrCLIMissing}

	loaderCalls := 0
	loader := func(_ string) ([]rules.Rule, []rules.Warning, error) {
		loaderCalls++
		return []rules.Rule{happyRule("a", "a.yaml")}, nil, nil
	}

	execCalls := 0
	exec := func(_ context.Context, _ ExecutorInput, _ func(workflow.WorkflowEvent)) ExecutorOutput {
		execCalls++
		return ExecutorOutput{}
	}

	deps := makeReviewDeps(fakeGh, loader, exec)
	ask := &workflow.CLIAsk{Values: map[string]string{"pr": "42"}}

	err := runReviewWorkflowWithRunMetadata(t, context.Background(), "run-precheck-fail", cwd, rec, ag, ask, deps)

	// Req 7.4: precheck_failed は Sequential 後段を止めるため non-nil error を返す。
	if err == nil {
		t.Fatalf("orchestration error = nil, want non-nil (precheck_failed → Sequential 失敗伝播)")
	}
	// `[verdict:precheck_failed]` プレフィックス + 人間可読 summary を含む契約 (Req 7.4)。
	if !strings.Contains(err.Error(), "[verdict:precheck_failed]") {
		t.Errorf("err = %q, want prefix [verdict:precheck_failed]", err.Error())
	}

	// result.json が出力され、OverallVerdict=precheck_failed であること。
	sum := readResultJSON(t, runDir)
	if sum.OverallVerdict != result.VerdictPrecheckFailed {
		t.Errorf("OverallVerdict = %q, want %q", sum.OverallVerdict, result.VerdictPrecheckFailed)
	}
	if sum.WorkflowRunID != "run-precheck-fail" {
		t.Errorf("WorkflowRunID = %q, want %q", sum.WorkflowRunID, "run-precheck-fail")
	}
	if sum.OverallSummary == "" {
		t.Errorf("OverallSummary is empty")
	}
	if sum.StartedAt == "" || sum.FinishedAt == "" {
		t.Errorf("missing timestamps: %+v", sum)
	}

	// Req 3.3: precheck 失敗時に副作用 (loadRules / executor / per-rule artifact) を起動しない。
	// loaderCalls は 1 まで許容 (precheck の Step 2 で呼ばれる) — ただし executor は 0 でなければならない。
	if execCalls != 0 {
		t.Errorf("executor calls = %d, want 0 (precheck 失敗時の副作用禁止 Req 3.3)", execCalls)
	}
	files := listPerRuleArtifactFiles(t, runDir)
	if len(files) != 0 {
		t.Errorf("per-rule artifact files = %v, want []  (precheck 失敗時に per-rule artifact 一切書き出さない Req 3.3)", files)
	}

	// result.json artifact は recorder に登録されている (Req 6.4)。
	gotResultArtifact := false
	for _, a := range rec.snapshotArtifacts() {
		if a.Kind == akashic.ArtifactKindResult {
			gotResultArtifact = true
			break
		}
	}
	if !gotResultArtifact {
		t.Errorf("RecordRunArtifact(kind=result) not called (Req 6.4)")
	}
}

// TestRunReviewWorkflow_PrecheckFailureGhCheckShortCircuitsLoader は gh CheckCLI 失敗時に
// loadRules すら呼ばれないこと (Flow 2 決定木の早期 short-circuit) を検証する。
func TestRunReviewWorkflow_PrecheckFailureGhCheckShortCircuitsLoader(t *testing.T) {
	t.Parallel()

	cwd, _ := setupRillRunDir(t, "run-short-circuit")
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)
	fakeGh := &fakeGhClient{checkCLIErr: gh.ErrCLIMissing}

	loaderCalls := 0
	loader := func(_ string) ([]rules.Rule, []rules.Warning, error) {
		loaderCalls++
		return nil, nil, rules.ErrRulesDirMissing
	}
	exec := func(_ context.Context, _ ExecutorInput, _ func(workflow.WorkflowEvent)) ExecutorOutput {
		return ExecutorOutput{}
	}

	deps := makeReviewDeps(fakeGh, loader, exec)
	ask := &workflow.CLIAsk{Values: map[string]string{"pr": "1"}}

	_ = runReviewWorkflowWithRunMetadata(t, context.Background(), "run-short-circuit", cwd, rec, ag, ask, deps)

	if loaderCalls != 0 {
		t.Errorf("loadRules calls = %d, want 0 (gh CLI 失敗で short-circuit すべき)", loaderCalls)
	}
}

// ---------------------- precheck 成功 → executor → result.json (Req 6.3, 6.4) ----------------------

// TestRunReviewWorkflow_HappyPathAggregatesNoFindings は precheck 通過 + 全 rule
// no_findings の経路で OverallVerdict=no_findings、result.json + per-rule artifact が揃い、
// Sequential 互換のため nil error を返すことを検証する (Req 6.3, 7.3)。
func TestRunReviewWorkflow_HappyPathAggregatesNoFindings(t *testing.T) {
	t.Parallel()

	cwd, runDir := setupRillRunDir(t, "run-happy")
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)

	fakeGh := &fakeGhClient{
		viewPRMeta: happyPRMeta(),
		diffPRStr:  "diff body",
	}
	loadedRules := []rules.Rule{
		happyRule("alpha", "alpha.yaml"),
		happyRule("bravo", "bravo.yaml"),
	}
	loader := stubLoaderOK(loadedRules, nil)

	exec := func(_ context.Context, in ExecutorInput, _ func(workflow.WorkflowEvent)) ExecutorOutput {
		// runDir に per-rule artifact を書き出す本番経路を fake で再現する。
		out := ExecutorOutput{Results: make([]result.RuleResult, len(in.Rules))}
		for i, r := range in.Rules {
			res := result.RuleResult{
				Name:        r.Name,
				Description: r.Description,
				Applied:     true,
				Verdict:     result.VerdictNoFindings,
				Provider:    r.Provider,
				Model:       r.Model,
				SourceFile:  r.SourceFile,
				StartedAt:   "2026-01-01T00:00:00Z",
				FinishedAt:  "2026-01-01T00:00:01Z",
			}
			out.Results[i] = res
			body, _ := json.Marshal(res)
			_ = os.WriteFile(filepath.Join(in.RunDir, r.Name+".json"), body, 0o644)
		}
		return out
	}

	deps := makeReviewDeps(fakeGh, loader, exec)
	ask := &workflow.CLIAsk{Values: map[string]string{"pr": "42"}}

	err := runReviewWorkflowWithRunMetadata(t, context.Background(), "run-happy", cwd, rec, ag, ask, deps)
	if err != nil {
		t.Fatalf("orchestration error = %v, want nil (no_findings は IsIntent=true で nil 返却)", err)
	}

	sum := readResultJSON(t, runDir)
	if sum.OverallVerdict != result.VerdictNoFindings {
		t.Errorf("OverallVerdict = %q, want %q", sum.OverallVerdict, result.VerdictNoFindings)
	}
	if sum.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", sum.PRNumber)
	}
	if len(sum.Rules) != len(loadedRules) {
		t.Errorf("Summary.Rules length = %d, want %d", len(sum.Rules), len(loadedRules))
	}
	for i, r := range sum.Rules {
		if r.Name != loadedRules[i].Name {
			t.Errorf("Rules[%d].Name = %q, want %q", i, r.Name, loadedRules[i].Name)
		}
	}
	// per-rule artifact が runDir 配下に存在する。
	for _, r := range loadedRules {
		path := filepath.Join(runDir, r.Name+".json")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("per-rule artifact %s missing: %v", path, err)
		}
	}
}

// TestRunReviewWorkflow_HappyPathAggregatesFindingsReported は executor が
// findings_reported を返した rule を 1 件含むとき OverallVerdict=findings_reported に
// 集約されることを検証する (Req 6.3, OverallVerdict 優先順位)。
func TestRunReviewWorkflow_HappyPathAggregatesFindingsReported(t *testing.T) {
	t.Parallel()

	cwd, runDir := setupRillRunDir(t, "run-findings")
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)

	fakeGh := &fakeGhClient{
		viewPRMeta: happyPRMeta(),
		diffPRStr:  "diff",
	}
	loadedRules := []rules.Rule{
		happyRule("alpha", "alpha.yaml"),
		happyRule("bravo", "bravo.yaml"),
	}
	loader := stubLoaderOK(loadedRules, nil)
	exec := func(_ context.Context, in ExecutorInput, _ func(workflow.WorkflowEvent)) ExecutorOutput {
		out := ExecutorOutput{Results: make([]result.RuleResult, len(in.Rules))}
		// alpha=no_findings, bravo=findings_reported を返す。
		out.Results[0] = result.RuleResult{
			Name: in.Rules[0].Name, Verdict: result.VerdictNoFindings, Applied: true,
			Provider: in.Rules[0].Provider, Model: in.Rules[0].Model, SourceFile: in.Rules[0].SourceFile,
		}
		out.Results[1] = result.RuleResult{
			Name: in.Rules[1].Name, Verdict: result.VerdictFindingsReported, Applied: true,
			FindingsPresent: true, Summary: "found",
			Provider: in.Rules[1].Provider, Model: in.Rules[1].Model, SourceFile: in.Rules[1].SourceFile,
		}
		return out
	}

	deps := makeReviewDeps(fakeGh, loader, exec)
	ask := &workflow.CLIAsk{Values: map[string]string{"pr": "42"}}

	err := runReviewWorkflowWithRunMetadata(t, context.Background(), "run-findings", cwd, rec, ag, ask, deps)
	if err != nil {
		t.Fatalf("orchestration error = %v, want nil (findings_reported は IsIntent=true)", err)
	}
	sum := readResultJSON(t, runDir)
	if sum.OverallVerdict != result.VerdictFindingsReported {
		t.Errorf("OverallVerdict = %q, want %q", sum.OverallVerdict, result.VerdictFindingsReported)
	}
}

// TestRunReviewWorkflow_FailedVerdictReturnsError は executor が全 rule failed を返したとき、
// OverallVerdict=failed かつ Sequential 後段を止めるため non-nil error を返すことを検証する (Req 7.4)。
func TestRunReviewWorkflow_FailedVerdictReturnsError(t *testing.T) {
	t.Parallel()

	cwd, runDir := setupRillRunDir(t, "run-failed")
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)

	fakeGh := &fakeGhClient{viewPRMeta: happyPRMeta(), diffPRStr: "diff"}
	loadedRules := []rules.Rule{happyRule("alpha", "alpha.yaml")}
	loader := stubLoaderOK(loadedRules, nil)
	exec := func(_ context.Context, in ExecutorInput, _ func(workflow.WorkflowEvent)) ExecutorOutput {
		return ExecutorOutput{Results: []result.RuleResult{
			{
				Name: in.Rules[0].Name, Verdict: result.VerdictFailed, FailureCategory: "provider_error",
				Summary: "boom", Provider: in.Rules[0].Provider, Model: in.Rules[0].Model, SourceFile: in.Rules[0].SourceFile,
			},
		}}
	}

	deps := makeReviewDeps(fakeGh, loader, exec)
	ask := &workflow.CLIAsk{Values: map[string]string{"pr": "42"}}

	err := runReviewWorkflowWithRunMetadata(t, context.Background(), "run-failed", cwd, rec, ag, ask, deps)
	if err == nil {
		t.Fatalf("orchestration error = nil, want non-nil (failed → Sequential 失敗伝播)")
	}
	if !strings.Contains(err.Error(), "[verdict:failed]") {
		t.Errorf("err = %q, want prefix [verdict:failed]", err.Error())
	}
	sum := readResultJSON(t, runDir)
	if sum.OverallVerdict != result.VerdictFailed {
		t.Errorf("OverallVerdict = %q, want %q", sum.OverallVerdict, result.VerdictFailed)
	}
}

// ---------------------- Sequential 互換 (Req 7.3, 7.4) ----------------------

// TestRunReviewWorkflow_SequentialPropagatesFailureForPrecheckFailed は
// review.Workflow が precheck_failed のとき Sequential が後段を起動しないことを検証する (Req 7.4)。
func TestRunReviewWorkflow_SequentialPropagatesFailureForPrecheckFailed(t *testing.T) {
	t.Parallel()

	cwd, _ := setupRillRunDir(t, "ignored-runid")

	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)

	// 後段の fake apply: 起動回数を観測する。Sequential が後段を起動しない契約 (Req 7.4) を検証する。
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

	// review.Workflow を本番経路で組み立てるが、production の deps では実 gh / 実 LoadRules を
	// 呼んでしまう。テスト用に runReviewWorkflowOrchestration を直接呼ぶ薄いラッパ Definition を組む。
	fakeGh := &fakeGhClient{checkCLIErr: gh.ErrCLIMissing}
	loader := stubLoaderOK([]rules.Rule{happyRule("a", "a.yaml")}, nil)
	exec := func(_ context.Context, _ ExecutorInput, _ func(workflow.WorkflowEvent)) ExecutorOutput {
		return ExecutorOutput{}
	}
	deps := makeReviewDeps(fakeGh, loader, exec)

	reviewDef := workflow.Definition{
		ID:    reviewWorkflowID,
		Label: reviewWorkflowLabel,
		Fn: func(ctx context.Context, _ string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			return runReviewWorkflowOrchestration(ctx, cwd, onEvent, ask, ag, deps)
		},
		Recorder: rec,
	}

	seq := workflow.Sequential("review-and-apply", "review-and-apply", ag, reviewDef, apply)
	err := seq.Run(context.Background(), cwd, nil, &workflow.CLIAsk{Values: map[string]string{"pr": "1"}})

	if err == nil {
		t.Fatalf("seq.Run error = nil, want non-nil (precheck_failed → Sequential 後段起動禁止)")
	}
	if applyCalls != 0 {
		t.Errorf("apply called %d times, want 0 (Req 7.4)", applyCalls)
	}
}

// TestRunReviewWorkflow_SequentialFindingsReportedRunsApply は review が findings_reported を
// 返したとき Sequential 後段が起動することを検証する (Req 7.3)。
func TestRunReviewWorkflow_SequentialFindingsReportedRunsApply(t *testing.T) {
	t.Parallel()

	cwd, runDir := setupRillRunDir(t, "run-seq-ok")

	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)

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

	fakeGh := &fakeGhClient{viewPRMeta: happyPRMeta(), diffPRStr: "diff"}
	loadedRules := []rules.Rule{happyRule("alpha", "alpha.yaml")}
	loader := stubLoaderOK(loadedRules, nil)
	exec := func(_ context.Context, in ExecutorInput, _ func(workflow.WorkflowEvent)) ExecutorOutput {
		return ExecutorOutput{Results: []result.RuleResult{
			{
				Name: in.Rules[0].Name, Verdict: result.VerdictFindingsReported, Applied: true,
				FindingsPresent: true, Summary: "found",
				Provider: in.Rules[0].Provider, Model: in.Rules[0].Model, SourceFile: in.Rules[0].SourceFile,
			},
		}}
	}
	deps := makeReviewDeps(fakeGh, loader, exec)

	reviewDef := workflow.Definition{
		ID:    reviewWorkflowID,
		Label: reviewWorkflowLabel,
		Fn: func(ctx context.Context, _ string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			return runReviewWorkflowOrchestration(ctx, cwd, onEvent, ask, ag, deps)
		},
		Recorder: rec,
	}

	seq := workflow.Sequential("review-and-apply", "review-and-apply", ag, reviewDef, apply)
	err := seq.Run(context.Background(), cwd, nil, &workflow.CLIAsk{Values: map[string]string{"pr": "42"}})
	if err != nil {
		t.Fatalf("seq.Run error = %v, want nil (findings_reported → Sequential 後段継続)", err)
	}
	if applyCalls != 1 {
		t.Errorf("apply called %d times, want 1 (Req 7.3)", applyCalls)
	}
	// review.Workflow 単独経路と等価の result.json が出力されている (Req 7.3)。
	// runID は Sequential 経由で動的に採番されるため、runDir を直接走査するのではなく
	// `<cwd>/.rilll/review-workflow/*` 配下に result.json が作られていることだけ確認する。
	matches, _ := filepath.Glob(filepath.Join(cwd, ".rilll", "review-workflow", "*", "result.json"))
	if len(matches) == 0 {
		t.Errorf("no result.json found under <cwd>/.rilll/review-workflow/*/ (runDir=%s); cwd entries:", runDir)
	}
}

// ---------------------- defer 経路 (Req 6.3, 6.4) ----------------------

// TestRunReviewWorkflow_ResultJSONIncludesPrecheckWarnings は precheck 段階で
// LoadRules から非致命的 Warnings (skip 通知) が返ったときも、それが result.json の
// PrecheckWarnings に反映されることを検証する (Req 6.3)。
func TestRunReviewWorkflow_ResultJSONIncludesPrecheckWarnings(t *testing.T) {
	t.Parallel()

	cwd, runDir := setupRillRunDir(t, "run-warn")
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)

	warnings := []rules.Warning{
		{File: "broken.yaml", Reason: "yaml decode failed"},
	}
	fakeGh := &fakeGhClient{viewPRMeta: happyPRMeta(), diffPRStr: "diff"}
	loadedRules := []rules.Rule{happyRule("ok", "ok.yaml")}
	loader := stubLoaderOK(loadedRules, warnings)
	exec := func(_ context.Context, in ExecutorInput, _ func(workflow.WorkflowEvent)) ExecutorOutput {
		return ExecutorOutput{Results: []result.RuleResult{
			{
				Name: in.Rules[0].Name, Verdict: result.VerdictNoFindings, Applied: true,
				Provider: in.Rules[0].Provider, Model: in.Rules[0].Model, SourceFile: in.Rules[0].SourceFile,
			},
		}}
	}
	deps := makeReviewDeps(fakeGh, loader, exec)
	ask := &workflow.CLIAsk{Values: map[string]string{"pr": "42"}}

	_ = runReviewWorkflowWithRunMetadata(t, context.Background(), "run-warn", cwd, rec, ag, ask, deps)

	sum := readResultJSON(t, runDir)
	if len(sum.PrecheckWarnings) != 1 {
		t.Fatalf("PrecheckWarnings length = %d, want 1; got = %+v", len(sum.PrecheckWarnings), sum.PrecheckWarnings)
	}
	if !strings.Contains(sum.PrecheckWarnings[0], "broken.yaml") {
		t.Errorf("PrecheckWarnings[0] = %q, want to contain broken.yaml", sum.PrecheckWarnings[0])
	}
}
