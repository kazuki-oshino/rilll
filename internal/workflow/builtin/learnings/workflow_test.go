// Package learnings の本 file は agent-learnings workflow orchestration (`Workflow` /
// `runLearningsOrchestration`) の単体テスト群を保持する (tasks.md 8.1)。
//
// 検証観点:
//
//   - (a) parent_run_id 未注入 → applied-no-op で副作用ゼロ early return + WorkflowDone
//     1 度 emit (Req 1.1 precondition)。
//   - (b) reader 未注入 (agent が AkashicReaderProvider 未実装) → reader-unavailable。
//   - (c) agent_calls 0 件 → applied-no-op (Req 3.3 seam)。
//   - (d) 4 phase 直列駆動成功 → applied + apply-result.json artifact + WorkflowDone emit。
//   - (e) curator 早期 verdict (`rejected-invalid-json`) → 採用して apply skip。
//   - (f) reflector 全 session 失敗 → resume-all-failed (Req 4.6)。
//   - (g) Workflow factory が Hidden=true / ID=agent-learnings / Args=nil で返ること。
package learnings

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// fakeWorkflowAgent は workflowctx.AgentCaller + AkashicReaderProvider +
// AkashicRecorderProvider を満たすテスト stub。phase 別に response を制御する。
type fakeWorkflowAgent struct {
	mu            sync.Mutex
	reader        akashic.Reader
	recorder      akashic.Recorder
	reflectorResp provider.AgentResponse
	reflectorErr  error
	curatorResp   provider.AgentResponse
	curatorErr    error
	calls         []string // step_id 順
}

func (f *fakeWorkflowAgent) AkashicReader() akashic.Reader     { return f.reader }
func (f *fakeWorkflowAgent) AkashicRecorder() akashic.Recorder { return f.recorder }

func (f *fakeWorkflowAgent) CallAgent(_ context.Context, _, _ string, _ provider.AgentEventCallback, _ string, opts *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	step := ""
	if opts != nil {
		step = opts.StepID
	}
	f.calls = append(f.calls, step)
	switch step {
	case stepIDReflector:
		return f.reflectorResp, f.reflectorErr
	case stepIDCurator:
		return f.curatorResp, f.curatorErr
	}
	return provider.AgentResponse{}, nil
}

// fakeReader は akashic.Reader の最小 stub。AgentCallsByWorkflowRun のみ実装する。
// workflow.go では Reader.Detail も resolveParentWorkflowID 経由で呼べるが MVP では
// 空文字を返すだけなので Detail は呼ばれない。他 method は panic で「想定外呼び出し」を可視化する。
type fakeReader struct {
	rows []akashic.AgentCallRow
	err  error
}

func (r *fakeReader) AgentCallsByWorkflowRun(_ context.Context, _ string) ([]akashic.AgentCallRow, error) {
	return r.rows, r.err
}
func (r *fakeReader) LearningsByWorkflowRun(_ context.Context, _ string) ([]akashic.LearningRow, error) {
	return nil, nil
}

// 残りの Reader 必須メソッドは stub では使われない。panic で fail-fast。
func (r *fakeReader) List(context.Context, akashic.ListOptions) (akashic.ListResult, error) {
	panic("fakeReader.List unexpected call")
}
func (r *fakeReader) AggregateByWorkflow(context.Context, akashic.WorkflowAggregateOptions) (akashic.WorkflowAggregateResult, error) {
	panic("fakeReader.AggregateByWorkflow unexpected call")
}
func (r *fakeReader) AggregateByProvider(context.Context, akashic.ProviderAggregateOptions) (akashic.ProviderAggregateResult, error) {
	panic("fakeReader.AggregateByProvider unexpected call")
}
func (r *fakeReader) Detail(context.Context, akashic.DetailOptions) (akashic.RunDetail, error) {
	panic("fakeReader.Detail unexpected call")
}
func (r *fakeReader) FailureClusters(context.Context, akashic.FailureOptions) (akashic.FailureClustersResult, error) {
	panic("fakeReader.FailureClusters unexpected call")
}
func (r *fakeReader) InRunResolutionsForCluster(context.Context, akashic.InRunResolutionOptions) (akashic.InRunResolutionsResult, error) {
	panic("fakeReader.InRunResolutionsForCluster unexpected call")
}
func (r *fakeReader) NextSuccessAcrossRuns(context.Context, akashic.NextSuccessOptions) (akashic.NextSuccessResult, error) {
	panic("fakeReader.NextSuccessAcrossRuns unexpected call")
}

// fakeWorkflowRecorder は前 reflector_test.go の同名構造体と機能等価。
// ここでは workflow.go test 用に reuse 名前衝突回避のため別名で定義する。
type fakeWorkflowRecorder struct {
	mu        sync.Mutex
	rows      []akashic.LearningRow
	artifacts []akashic.RunArtifact
}

func (r *fakeWorkflowRecorder) RecordWorkflowRunStart(context.Context, akashic.WorkflowRun) error {
	return nil
}
func (r *fakeWorkflowRecorder) RecordWorkflowRunFinish(context.Context, akashic.WorkflowRunFinish) error {
	return nil
}
func (r *fakeWorkflowRecorder) RecordAgentCallStart(context.Context, akashic.AgentCall) error {
	return nil
}
func (r *fakeWorkflowRecorder) RecordAgentCallFinish(context.Context, akashic.AgentCallResult) error {
	return nil
}
func (r *fakeWorkflowRecorder) RecordRawEvent(context.Context, akashic.RawEvent) error { return nil }
func (r *fakeWorkflowRecorder) RecordRunArtifact(_ context.Context, artifact akashic.RunArtifact) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.artifacts = append(r.artifacts, artifact)
	return nil
}
func (r *fakeWorkflowRecorder) RecordUserCorrection(context.Context, string) error { return nil }
func (r *fakeWorkflowRecorder) Close() error                                       { return nil }
func (r *fakeWorkflowRecorder) RecordLearning(_ context.Context, row akashic.LearningRow) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, row)
	return nil
}

func (r *fakeWorkflowRecorder) hasArtifactBase(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, artifact := range r.artifacts {
		if filepath.Base(artifact.Path) == name {
			return true
		}
	}
	return false
}

type fakeWorkflowJobStoreRecorder struct {
	*fakeWorkflowRecorder
	*earlyAggregationStore
}

func newFakeWorkflowJobStoreRecorder() *fakeWorkflowJobStoreRecorder {
	return &fakeWorkflowJobStoreRecorder{
		fakeWorkflowRecorder:  &fakeWorkflowRecorder{},
		earlyAggregationStore: newEarlyAggregationStore(),
	}
}

// makeRunCtx は RunMetadata + parentRunID + recorder 注入済みの ctx を返す test helper。
func makeRunCtx(t *testing.T, cwd, runID, parentRunID string, recorder akashic.Recorder) context.Context {
	t.Helper()
	ctx := context.Background()
	ctx = workflowctx.ContextWithRunMetadata(ctx, workflowctx.RunMetadata{
		WorkflowRunID: runID,
		WorkflowID:    learningsWorkflowID,
		WorkflowName:  learningsWorkflowLabel,
		CWD:           cwd,
	})
	ctx = workflow.ContextWithRecorder(ctx, recorder)
	if parentRunID != "" {
		ctx = ContextWithParentRunID(ctx, parentRunID)
	}
	return ctx
}

// readApplyArtifact は <cwd>/.rilll/agent-learnings/<runID>/apply-result.json を decode する。
func readApplyArtifact(t *testing.T, cwd, runID string) ApplyResult {
	t.Helper()
	path := filepath.Join(cwd, ".rilll", learningsWorkflowID, runID, applyResultFilename)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read apply-result.json: %v", err)
	}
	var r ApplyResult
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("unmarshal apply-result.json: %v", err)
	}
	return r
}

// TestWorkflowFactoryHiddenAndID: 観点 (g) factory metadata。
func TestWorkflowFactoryHiddenAndID(t *testing.T) {
	def := Workflow(&fakeWorkflowAgent{})
	if def.ID != learningsWorkflowID {
		t.Errorf("ID = %q, want %q", def.ID, learningsWorkflowID)
	}
	if !def.Hidden {
		t.Errorf("Hidden = false, want true")
	}
	if len(def.Args) != 0 {
		t.Errorf("Args = %+v, want empty", def.Args)
	}
}

// TestRunLearningsOrchestrationNoParentRunID: 観点 (a) parent_run_id 未注入 → applied-no-op。
func TestRunLearningsOrchestrationNoParentRunID(t *testing.T) {
	cwd := t.TempDir()
	runID := "run-1"
	rec := &fakeWorkflowRecorder{}
	agent := &fakeWorkflowAgent{recorder: rec}

	ctx := makeRunCtx(t, cwd, runID, "" /* parentRunID 未注入 */, rec)
	var doneCount int
	onEvent := func(e workflow.WorkflowEvent) {
		if e.Type == workflow.WorkflowDone {
			doneCount++
		}
	}
	if err := runLearningsOrchestration(ctx, cwd, onEvent, nil, agent); err != nil {
		t.Fatalf("runLearningsOrchestration: %v", err)
	}
	if doneCount != 1 {
		t.Errorf("WorkflowDone emit count = %d, want 1", doneCount)
	}
	a := readApplyArtifact(t, cwd, runID)
	if a.Verdict != VerdictAppliedNoOp {
		t.Errorf("Verdict = %q, want applied-no-op", a.Verdict)
	}
	if len(agent.calls) != 0 {
		t.Errorf("CallAgent invoked = %v, want 0 (precondition skip)", agent.calls)
	}
}

// TestRunLearningsOrchestrationReaderUnavailable: 観点 (b) reader nil → reader-unavailable。
func TestRunLearningsOrchestrationReaderUnavailable(t *testing.T) {
	cwd := t.TempDir()
	runID := "run-1"
	rec := &fakeWorkflowRecorder{}
	agent := &fakeWorkflowAgent{recorder: rec /* reader 未注入 */}

	ctx := makeRunCtx(t, cwd, runID, "parent-run-1", rec)
	if err := runLearningsOrchestration(ctx, cwd, nil, nil, agent); err != nil {
		t.Fatalf("runLearningsOrchestration: %v", err)
	}
	a := readApplyArtifact(t, cwd, runID)
	if a.Verdict != VerdictReaderUnavailable {
		t.Errorf("Verdict = %q, want %q", a.Verdict, VerdictReaderUnavailable)
	}
}

// TestRunLearningsOrchestrationZeroSessions: 観点 (c) 0 session → applied-no-op。
func TestRunLearningsOrchestrationZeroSessions(t *testing.T) {
	cwd := t.TempDir()
	runID := "run-1"
	rec := &fakeWorkflowRecorder{}
	reader := &fakeReader{rows: nil} // 0 件
	agent := &fakeWorkflowAgent{reader: reader, recorder: rec}

	ctx := makeRunCtx(t, cwd, runID, "parent-run-1", rec)
	if err := runLearningsOrchestration(ctx, cwd, nil, nil, agent); err != nil {
		t.Fatalf("runLearningsOrchestration: %v", err)
	}
	a := readApplyArtifact(t, cwd, runID)
	if a.Verdict != VerdictAppliedNoOp {
		t.Errorf("Verdict = %q, want applied-no-op", a.Verdict)
	}
	if len(agent.calls) != 0 {
		t.Errorf("CallAgent invoked = %v, want 0", agent.calls)
	}
}

// TestRunLearningsOrchestrationFullPipeline: 観点 (d) 4 phase 駆動 → applied + artifact + WorkflowDone。
func TestRunLearningsOrchestrationFullPipeline(t *testing.T) {
	withProviderEnv(t, "codex")
	cwd := t.TempDir()
	runID := "run-1"
	rec := &fakeWorkflowRecorder{}
	projectID := akashic.ProjectIDFromCWD(cwd)
	reader := &fakeReader{rows: []akashic.AgentCallRow{
		{ID: "ac-1", SessionID: "sess-A", ProviderID: "codex", StepID: "implement", ProjectID: projectID},
	}}
	agent := &fakeWorkflowAgent{
		reader:   reader,
		recorder: rec,
		reflectorResp: provider.AgentResponse{
			Content: `{"learnings":[{"category":"failure-recovery","context":"c","insight":"i","paths_glob":["a/**"]}]}`,
		},
		curatorResp: provider.AgentResponse{
			Content: `{"operations":[{"op":"add","category":"failure-recovery","topic":"sample-topic","paths_glob":["a/**"],"entry":"valid bullet entry","summary":"sample summary","applies_to":"sample applies"}]}`,
		},
	}

	ctx := makeRunCtx(t, cwd, runID, "parent-run-1", rec)
	var doneCount int
	onEvent := func(e workflow.WorkflowEvent) {
		if e.Type == workflow.WorkflowDone {
			doneCount++
		}
	}
	if err := runLearningsOrchestration(ctx, cwd, onEvent, nil, agent); err != nil {
		t.Fatalf("runLearningsOrchestration: %v", err)
	}
	if doneCount != 1 {
		t.Errorf("WorkflowDone emit = %d, want 1", doneCount)
	}
	a := readApplyArtifact(t, cwd, runID)
	if a.Verdict != VerdictApplied {
		t.Errorf("Verdict = %q, want applied", a.Verdict)
	}
	if len(a.Appended) != 1 {
		t.Errorf("Appended = %d, want 1", len(a.Appended))
	}
	// agent.CallAgent が reflector + curator の 2 回呼ばれた
	if len(agent.calls) != 2 {
		t.Errorf("CallAgent invoked = %v, want [reflector, curator]", agent.calls)
	}
	if agent.calls[0] != stepIDReflector || agent.calls[1] != stepIDCurator {
		t.Errorf("call order = %v, want [reflector, curator]", agent.calls)
	}
	// playbook file が作られた (wiki layout: `<cwd>/docs_dev/playbook/<category>.md`)
	playbook := filepath.Join(cwd, "docs_dev", "playbook", "failure-recovery.md")
	if _, err := os.Stat(playbook); err != nil {
		t.Errorf("playbook file not created: %v", err)
	}
	// learnings が akashic に inserted された
	if len(rec.rows) != 1 {
		t.Errorf("RecordLearning rows = %d, want 1", len(rec.rows))
	}
}

func TestRunLearningsOrchestrationEarlyAggregationUsesArtifactsAndSkipsLegacyReflector(t *testing.T) {
	cwd := makeFixtureProjectCWD(t, "project-rilll")
	runID := "tail-run-early"
	rec := newFakeWorkflowJobStoreRecorder()
	base := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	reader := &fakeReader{rows: []akashic.AgentCallRow{
		earlyAggregationAgentCallRow("ac-early", "parent-run-early", "sess-early", "codex", "implement", "project-rilll", base),
		earlyAggregationAgentCallRow("ac-missing", "parent-run-early", "sess-missing", "codex", "review", "project-rilll", base.Add(time.Second)),
	}}
	succeeded := earlyAggregationJob("ac-early", "parent-run-early", "sess-early", "codex", "implement", akashic.LearningReflectionStatusSucceeded, base)
	succeeded.ResultJSON = earlyAggregationResultJSON(t, "ac-early", "sess-early", "codex", "implement", []LearningReflectionAcceptedLearning{
		{
			ID:        "learning-early",
			Category:  akashic.CategoryFailureRecovery,
			Context:   "ctx early",
			Insight:   "ins early",
			PathsGlob: []string{"internal/workflow/**"},
		},
	})
	rec.jobs["ac-early"] = succeeded
	agent := &fakeWorkflowAgent{
		reader:       reader,
		recorder:     rec,
		reflectorErr: errors.New("legacy reflector must not run in early aggregation mode"),
		curatorResp: provider.AgentResponse{
			Content: `{"operations":[{"op":"add","category":"failure-recovery","topic":"early-topic","paths_glob":["internal/workflow/**"],"entry":"early aggregation bullet","summary":"early summary","applies_to":"early applies"}]}`,
		},
	}

	ctx := makeRunCtx(t, cwd, runID, "parent-run-early", rec)
	ctx = ContextWithEarlyAggregationEnabled(ctx)
	if err := runLearningsOrchestration(ctx, cwd, nil, nil, agent); err != nil {
		t.Fatalf("runLearningsOrchestration: %v", err)
	}

	a := readApplyArtifact(t, cwd, runID)
	if a.Verdict != VerdictApplied {
		t.Fatalf("Verdict = %q, want %q", a.Verdict, VerdictApplied)
	}
	if len(agent.calls) != 1 || agent.calls[0] != stepIDCurator {
		t.Fatalf("CallAgent steps = %v, want [%s] (legacy reflector must be skipped)", agent.calls, stepIDCurator)
	}
	if len(rec.rows) != 0 {
		t.Fatalf("RecordLearning rows = %d, want 0 (early aggregation must use RecordLearningOnce)", len(rec.rows))
	}
	if got := len(rec.learningRows); got != 1 {
		t.Fatalf("RecordLearningOnce rows = %d, want 1", got)
	}

	for _, name := range []string{reflectorFilename, learningsArtifactFilename, earlyReflectionArtifactFilename} {
		if !rec.hasArtifactBase(name) {
			t.Fatalf("artifact %s was not registered; artifacts=%+v", name, rec.artifacts)
		}
	}
	runDir := filepath.Join(cwd, ".rilll", learningsWorkflowID, runID)
	earlyArtifact := readEarlyReflectionArtifact(t, filepath.Join(runDir, earlyReflectionArtifactFilename))
	if earlyArtifact.Summary.TotalTargets != 2 || earlyArtifact.Summary.Included != 1 || earlyArtifact.Summary.MissingJob != 1 {
		t.Fatalf("early summary=%+v, want total=2 included=1 missing_job=1", earlyArtifact.Summary)
	}
	targets := earlyAggregationTargetsByID(earlyArtifact.Targets)
	assertEarlyAggregationTarget(t, targets["ac-early"], EarlyReflectionTargetStatusSucceeded, "", true)
	assertEarlyAggregationTarget(t, targets["ac-missing"], EarlyReflectionTargetStatusMissingJob, earlyAggregationMissingJobReason, false)

	learningsArtifact := readLearningsArtifact(t, filepath.Join(runDir, learningsArtifactFilename))
	if len(learningsArtifact.Learnings) != 1 || learningsArtifact.Learnings[0].ID != "learning-early" {
		t.Fatalf("learnings artifact = %+v, want learning-early only", learningsArtifact.Learnings)
	}
	if _, err := os.Stat(filepath.Join(cwd, "docs_dev", "playbook", "failure-recovery.md")); err != nil {
		t.Fatalf("playbook category file not written at existing schema path: %v", err)
	}
}

func TestRunLearningsOrchestrationEarlyAggregationWithoutStoreFallsBackToLegacyReflector(t *testing.T) {
	withProviderEnv(t, "codex")
	cwd := makeFixtureProjectCWD(t, "project-rilll")
	runID := "tail-run-legacy-fallback"
	rec := &fakeWorkflowRecorder{}
	projectID := akashic.ProjectIDFromCWD(cwd)
	reader := &fakeReader{rows: []akashic.AgentCallRow{
		{ID: "ac-legacy", SessionID: "sess-legacy", ProviderID: "codex", StepID: "implement", ProjectID: projectID},
	}}
	agent := &fakeWorkflowAgent{
		reader:   reader,
		recorder: rec,
		reflectorResp: provider.AgentResponse{
			Content: `{"learnings":[{"category":"failure-recovery","context":"c","insight":"i","paths_glob":["internal/workflow/**"]}]}`,
		},
		curatorResp: provider.AgentResponse{
			Content: `{"operations":[{"op":"add","category":"failure-recovery","topic":"legacy-fallback-topic","paths_glob":["internal/workflow/**"],"entry":"legacy fallback bullet","summary":"legacy summary","applies_to":"legacy applies"}]}`,
		},
	}

	ctx := makeRunCtx(t, cwd, runID, "parent-run-legacy", rec)
	ctx = ContextWithEarlyAggregationEnabled(ctx)
	if err := runLearningsOrchestration(ctx, cwd, nil, nil, agent); err != nil {
		t.Fatalf("runLearningsOrchestration: %v", err)
	}

	if len(agent.calls) != 2 || agent.calls[0] != stepIDReflector || agent.calls[1] != stepIDCurator {
		t.Fatalf("CallAgent steps = %v, want [%s %s] legacy fallback", agent.calls, stepIDReflector, stepIDCurator)
	}
	if rec.hasArtifactBase(earlyReflectionArtifactFilename) {
		t.Fatalf("early-reflection artifact registered despite job store being unavailable")
	}
	if !rec.hasArtifactBase(reflectorFilename) || !rec.hasArtifactBase(learningsArtifactFilename) {
		t.Fatalf("legacy fallback did not register reflector/learnings artifacts; artifacts=%+v", rec.artifacts)
	}
}

// TestRunLearningsOrchestrationCuratorEarlyVerdict: 観点 (e) curator parse 失敗 → rejected-invalid-json。
func TestRunLearningsOrchestrationCuratorEarlyVerdict(t *testing.T) {
	withProviderEnv(t, "codex")
	cwd := t.TempDir()
	runID := "run-1"
	rec := &fakeWorkflowRecorder{}
	projectID := akashic.ProjectIDFromCWD(cwd)
	reader := &fakeReader{rows: []akashic.AgentCallRow{
		{ID: "ac-1", SessionID: "sess-A", ProviderID: "codex", StepID: "implement", ProjectID: projectID},
	}}
	agent := &fakeWorkflowAgent{
		reader:   reader,
		recorder: rec,
		reflectorResp: provider.AgentResponse{
			Content: `{"learnings":[]}`, // empty learnings
		},
		curatorResp: provider.AgentResponse{
			Content: `not a json`, // parse 失敗
		},
	}

	ctx := makeRunCtx(t, cwd, runID, "parent-run-1", rec)
	if err := runLearningsOrchestration(ctx, cwd, nil, nil, agent); err != nil {
		t.Fatalf("runLearningsOrchestration: %v", err)
	}
	a := readApplyArtifact(t, cwd, runID)
	if a.Verdict != VerdictRejectedInvalidJSON {
		t.Errorf("Verdict = %q, want %q", a.Verdict, VerdictRejectedInvalidJSON)
	}
	// curator.raw.txt が保存された
	rawPath := filepath.Join(cwd, ".rilll", learningsWorkflowID, runID, curatorRawFilename)
	if _, err := os.Stat(rawPath); err != nil {
		t.Errorf("curator.raw.txt not saved: %v", err)
	}
}

// TestRunLearningsOrchestrationReflectorAllFailed: 観点 (f) 全 session 失敗 → resume-all-failed。
func TestRunLearningsOrchestrationReflectorAllFailed(t *testing.T) {
	withProviderEnv(t, "codex")
	cwd := t.TempDir()
	runID := "run-1"
	rec := &fakeWorkflowRecorder{}
	projectID := akashic.ProjectIDFromCWD(cwd)
	reader := &fakeReader{rows: []akashic.AgentCallRow{
		{ID: "ac-1", SessionID: "sess-A", ProviderID: "codex", ProjectID: projectID},
		{ID: "ac-2", SessionID: "sess-B", ProviderID: "codex", ProjectID: projectID},
	}}
	agent := &fakeWorkflowAgent{
		reader:       reader,
		recorder:     rec,
		reflectorErr: errors.New("simulated network failure"),
	}

	ctx := makeRunCtx(t, cwd, runID, "parent-run-1", rec)
	if err := runLearningsOrchestration(ctx, cwd, nil, nil, agent); err != nil {
		t.Fatalf("runLearningsOrchestration: %v", err)
	}
	a := readApplyArtifact(t, cwd, runID)
	if a.Verdict != VerdictResumeAllFailed {
		t.Errorf("Verdict = %q, want %q", a.Verdict, VerdictResumeAllFailed)
	}
	// curator は呼ばれていない
	for _, c := range agent.calls {
		if c == stepIDCurator {
			t.Errorf("curator should not be called when all reflectors fail")
		}
	}
}

// TestBuildLearningsResultSummaryNeverEmpty: 戻り値が空文字にならない不変式。
func TestBuildLearningsResultSummaryNeverEmpty(t *testing.T) {
	cases := []ApplyResult{
		{Verdict: VerdictApplied, Appended: []AppendedEntry{{}}},
		{Verdict: VerdictAppliedNoOp},
		{Verdict: VerdictReaderUnavailable},
		{Verdict: VerdictResumeAllFailed},
		{Verdict: VerdictRejectedInvalidJSON},
		{Verdict: VerdictRejectedInvalidCategory},
		{Verdict: ""},
		{Verdict: "unknown-verdict-foo"},
	}
	for _, r := range cases {
		got := buildLearningsResultSummary(r)
		if got == "" {
			t.Errorf("buildLearningsResultSummary returned empty for verdict=%q", r.Verdict)
		}
	}
}
