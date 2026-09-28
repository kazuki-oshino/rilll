// Package akashic の reader_sqlite_resolution_test.go は、
// reader_sqlite_resolution.go (InRunResolutionsForCluster /
// NextSuccessAcrossRuns) の SQL 実装を担保する。
//
// 検証方針:
//   - 実 SQLite を使った black-box テスト (mock を使わない)
//   - fixture の組み立ては Recorder API (RecordWorkflowRunStart /
//     RecordAgentCallStart / RecordAgentCallFinish) 経由で行い、SQL の
//     実装と独立に「ユーザー視点の振る舞い」を担保する
package akashic

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// inRunFixture は InRunResolutionsForCluster のテスト用 fixture を投入する。
//
// 投入する agent_calls (project_id = "rilll"):
//   - run-A / step=foo / call=fail-A1 (success=0, started_at=base+1s)
//   - run-A / step=foo / call=ok-A2   (success=1, started_at=base+5s, prompt="A2 修正版")
//   - run-A / step=foo / call=ok-A3   (success=1, started_at=base+10s) — A2 より後の重複成功
//   - run-A / step=bar / call=ok-A4   (success=1, started_at=base+6s) — 別 step
//   - run-B / step=foo / call=ok-B1   (success=1, started_at=base+20s) — 別 run
//   - run-A / step=foo / call=fail-A5 (success=0, started_at=base+30s) — 後続成功なし
//   - run-A / step=foo / call=fail-A0 (success=0, started_at=base-5s) — A2 より前の失敗 (A2 が拾われる)
//
// 別 project (project_id = "other") の干渉確認用:
//   - run-X / step=foo / call=fail-X1 (success=0, started_at=base)
//   - run-X / step=foo / call=ok-X2   (success=1, started_at=base+5s)
//
// 既存の List/Detail fixture と異なり、provider_id / failure_category 等の
// 詳細フィールドは scope 外 (本テストでは触らない)。
func inRunFixture(t *testing.T, ctx context.Context, recorder *SQLiteRecorder, base time.Time) {
	t.Helper()

	// 1. workflow_run を 3 つ用意 (run-A / run-B はプロジェクト rilll、run-X は other)。
	for _, run := range []struct {
		id, projectID, workflowID string
		started                   time.Time
	}{
		{"run-A", "rilll", "wf-1", base},
		{"run-B", "rilll", "wf-1", base.Add(15 * time.Second)},
		{"run-X", "other", "wf-1", base},
	} {
		if err := recorder.RecordWorkflowRunStart(ctx, WorkflowRun{
			ID:           run.id,
			WorkflowID:   run.workflowID,
			WorkflowName: "WF1",
			CWD:          "/tmp/" + run.projectID,
			ProjectID:    run.projectID,
			StartedAt:    run.started,
		}); err != nil {
			t.Fatalf("RecordWorkflowRunStart %s: %v", run.id, err)
		}
	}

	// 2. agent_calls を投入。Success の値は RecordAgentCallFinish 経由でのみ書ける。
	type call struct {
		id, runID, projectID, workflowID, stepID, prompt string
		startedAt                                        time.Time
		success                                          bool
		resultSummary                                    string
	}
	calls := []call{
		{id: "fail-A0", runID: "run-A", projectID: "rilll", workflowID: "wf-1", stepID: "foo", startedAt: base.Add(-5 * time.Second), success: false},
		{id: "fail-A1", runID: "run-A", projectID: "rilll", workflowID: "wf-1", stepID: "foo", startedAt: base.Add(1 * time.Second), success: false},
		{id: "ok-A2", runID: "run-A", projectID: "rilll", workflowID: "wf-1", stepID: "foo", startedAt: base.Add(5 * time.Second), success: true, prompt: "A2 修正版", resultSummary: "A2 で成功"},
		{id: "ok-A3", runID: "run-A", projectID: "rilll", workflowID: "wf-1", stepID: "foo", startedAt: base.Add(10 * time.Second), success: true},
		{id: "ok-A4", runID: "run-A", projectID: "rilll", workflowID: "wf-1", stepID: "bar", startedAt: base.Add(6 * time.Second), success: true},
		{id: "ok-B1", runID: "run-B", projectID: "rilll", workflowID: "wf-1", stepID: "foo", startedAt: base.Add(20 * time.Second), success: true},
		{id: "fail-A5", runID: "run-A", projectID: "rilll", workflowID: "wf-1", stepID: "foo", startedAt: base.Add(30 * time.Second), success: false},
		{id: "fail-X1", runID: "run-X", projectID: "other", workflowID: "wf-1", stepID: "foo", startedAt: base.Add(0 * time.Second), success: false},
		{id: "ok-X2", runID: "run-X", projectID: "other", workflowID: "wf-1", stepID: "foo", startedAt: base.Add(5 * time.Second), success: true},
	}
	for _, c := range calls {
		if err := recorder.RecordAgentCallStart(ctx, AgentCall{
			ID:            c.id,
			WorkflowRunID: c.runID,
			WorkflowID:    c.workflowID,
			WorkflowName:  "WF1",
			StepID:        c.stepID,
			StepName:      c.stepID,
			ProviderID:    "claude",
			CWD:           "/tmp/" + c.projectID,
			ProjectID:     c.projectID,
			Prompt:        c.prompt,
			StartedAt:     c.startedAt,
		}); err != nil {
			t.Fatalf("RecordAgentCallStart %s: %v", c.id, err)
		}
		errStr := ""
		if !c.success {
			errStr = "boom"
		}
		if err := recorder.RecordAgentCallFinish(ctx, AgentCallResult{
			ID:            c.id,
			Success:       c.success,
			ResultSummary: c.resultSummary,
			Error:         errStr,
			FinishedAt:    c.startedAt.Add(100 * time.Millisecond),
		}); err != nil {
			t.Fatalf("RecordAgentCallFinish %s: %v", c.id, err)
		}
	}
}

// TestSQLiteRecorderInRunResolutionsForClusterReturnsFirstSubsequentSuccess は
// 同 workflow_run_id 内で時系列的に最初の後続成功 agent_call を返すことを検証する。
func TestSQLiteRecorderInRunResolutionsForClusterReturnsFirstSubsequentSuccess(t *testing.T) {
	ctx := context.Background()
	recorder, err := OpenSQLite(filepath.Join(t.TempDir(), "akashic.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	base := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	inRunFixture(t, ctx, recorder, base)

	res, err := recorder.InRunResolutionsForCluster(ctx, InRunResolutionOptions{
		ProjectID:           "rilll",
		FailingAgentCallIDs: []string{"fail-A0", "fail-A1", "fail-A5"},
	})
	if err != nil {
		t.Fatalf("InRunResolutionsForCluster: %v", err)
	}

	// fail-A0 と fail-A1 は同 run / 同 step で後続成功 ok-A2 を持つ。
	// 最も早い後続成功 ok-A2 が両方の failing.id に対し採用される。
	row, ok := res.ResolutionsByFailingID["fail-A1"]
	if !ok {
		t.Fatalf("fail-A1 の resolution が見つからない: got map=%v", res.ResolutionsByFailingID)
	}
	if row.SuccessAgentCallID != "ok-A2" {
		t.Errorf("fail-A1 の SuccessAgentCallID = %q, want ok-A2 (ok-A3 ではなく最も早い ok-A2)", row.SuccessAgentCallID)
	}
	if row.WorkflowRunID != "run-A" {
		t.Errorf("fail-A1 の WorkflowRunID = %q, want run-A", row.WorkflowRunID)
	}
	if row.StepID != "foo" {
		t.Errorf("fail-A1 の StepID = %q, want foo", row.StepID)
	}
	if row.ResultSummary != "A2 で成功" {
		t.Errorf("fail-A1 の ResultSummary = %q, want \"A2 で成功\"", row.ResultSummary)
	}
	if row.Prompt != "A2 修正版" {
		t.Errorf("fail-A1 の Prompt = %q, want \"A2 修正版\"", row.Prompt)
	}

	row0, ok := res.ResolutionsByFailingID["fail-A0"]
	if !ok {
		t.Fatalf("fail-A0 の resolution が見つからない: got map=%v", res.ResolutionsByFailingID)
	}
	if row0.SuccessAgentCallID != "ok-A2" {
		t.Errorf("fail-A0 の SuccessAgentCallID = %q, want ok-A2", row0.SuccessAgentCallID)
	}

	// fail-A5 は後続成功が無い → map に含まれてはならない。
	if _, ok := res.ResolutionsByFailingID["fail-A5"]; ok {
		t.Errorf("fail-A5 は後続成功が無いので map に含まれてはならない: got=%v", res.ResolutionsByFailingID["fail-A5"])
	}
}

// TestSQLiteRecorderInRunResolutionsForClusterRespectsScope は
// 別 run / 別 step / 別 project の成功を取り違えないことを検証する。
func TestSQLiteRecorderInRunResolutionsForClusterRespectsScope(t *testing.T) {
	ctx := context.Background()
	recorder, err := OpenSQLite(filepath.Join(t.TempDir(), "akashic.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	base := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	inRunFixture(t, ctx, recorder, base)

	// project=other の fail-X1 について、project="rilll" でクエリすると見つからない。
	res, err := recorder.InRunResolutionsForCluster(ctx, InRunResolutionOptions{
		ProjectID:           "rilll",
		FailingAgentCallIDs: []string{"fail-X1"},
	})
	if err != nil {
		t.Fatalf("InRunResolutionsForCluster (project rilll, fail-X1): %v", err)
	}
	if _, ok := res.ResolutionsByFailingID["fail-X1"]; ok {
		t.Errorf("project=rilll scope で project=other の fail-X1 が拾われてはならない")
	}

	// project=other の scope なら拾える。
	resOther, err := recorder.InRunResolutionsForCluster(ctx, InRunResolutionOptions{
		ProjectID:           "other",
		FailingAgentCallIDs: []string{"fail-X1"},
	})
	if err != nil {
		t.Fatalf("InRunResolutionsForCluster (project other): %v", err)
	}
	rowX, ok := resOther.ResolutionsByFailingID["fail-X1"]
	if !ok {
		t.Fatalf("project=other scope で fail-X1 が拾われない: got=%v", resOther.ResolutionsByFailingID)
	}
	if rowX.SuccessAgentCallID != "ok-X2" {
		t.Errorf("fail-X1 の SuccessAgentCallID = %q, want ok-X2", rowX.SuccessAgentCallID)
	}
}

// TestSQLiteRecorderInRunResolutionsForClusterEmptyIDs は空 IDs で SQL を発行せず
// 空 map を返すことを検証する。
func TestSQLiteRecorderInRunResolutionsForClusterEmptyIDs(t *testing.T) {
	ctx := context.Background()
	recorder, err := OpenSQLite(filepath.Join(t.TempDir(), "akashic.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	res, err := recorder.InRunResolutionsForCluster(ctx, InRunResolutionOptions{
		ProjectID:           "rilll",
		FailingAgentCallIDs: nil,
	})
	if err != nil {
		t.Fatalf("InRunResolutionsForCluster (empty IDs): %v", err)
	}
	if len(res.ResolutionsByFailingID) != 0 {
		t.Errorf("空 IDs で空 map を返さない: got=%v", res.ResolutionsByFailingID)
	}
}

// TestSQLiteRecorderNextSuccessAcrossRunsReturnsEarliest は
// (workflow_id, step_id, AfterTime) に該当する最も早い成功 agent_call を返すことを検証する。
func TestSQLiteRecorderNextSuccessAcrossRunsReturnsEarliest(t *testing.T) {
	ctx := context.Background()
	recorder, err := OpenSQLite(filepath.Join(t.TempDir(), "akashic.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	base := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	inRunFixture(t, ctx, recorder, base)

	// fail-A5 (run-A / step=foo / started_at=base+30s) より後の成功は run-B / ok-B1
	// (started_at=base+20s ではなく、AfterTime=base+30s より後の成功は無いはず…と思いきや
	//  fixture では ok-B1 が base+20s なので AfterTime=base+30s より前。
	// AfterTime=base+15s で問い合わせると ok-B1 (base+20s) が hit する。
	res, err := recorder.NextSuccessAcrossRuns(ctx, NextSuccessOptions{
		ProjectID:  "rilll",
		WorkflowID: "wf-1",
		StepID:     "foo",
		AfterTime:  base.Add(15 * time.Second),
	})
	if err != nil {
		t.Fatalf("NextSuccessAcrossRuns: %v", err)
	}
	if !res.Found {
		t.Fatalf("Found=false, want true")
	}
	if res.Row.AgentCallID != "ok-B1" {
		t.Errorf("AgentCallID = %q, want ok-B1 (run-A の同 step success より後の最初の成功)", res.Row.AgentCallID)
	}
	if res.Row.WorkflowRunID != "run-B" {
		t.Errorf("WorkflowRunID = %q, want run-B", res.Row.WorkflowRunID)
	}
}

// TestSQLiteRecorderNextSuccessAcrossRunsReturnsNotFound は AfterTime より後の
// 成功 agent_call が無い場合 Found=false を返すことを検証する。
func TestSQLiteRecorderNextSuccessAcrossRunsReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	recorder, err := OpenSQLite(filepath.Join(t.TempDir(), "akashic.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	base := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	inRunFixture(t, ctx, recorder, base)

	// AfterTime を fixture の最大時刻より後にすると hit 0 件。
	res, err := recorder.NextSuccessAcrossRuns(ctx, NextSuccessOptions{
		ProjectID:  "rilll",
		WorkflowID: "wf-1",
		StepID:     "foo",
		AfterTime:  base.Add(60 * time.Second),
	})
	if err != nil {
		t.Fatalf("NextSuccessAcrossRuns: %v", err)
	}
	if res.Found {
		t.Errorf("Found=true, want false (AfterTime を fixture 最大時刻より後にしたので)")
	}
}

// TestSQLiteRecorderNextSuccessAcrossRunsRespectsProjectScope は
// ProjectID 上書きで別 project の成功を拾わないことを検証する。
func TestSQLiteRecorderNextSuccessAcrossRunsRespectsProjectScope(t *testing.T) {
	ctx := context.Background()
	recorder, err := OpenSQLite(filepath.Join(t.TempDir(), "akashic.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	base := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	inRunFixture(t, ctx, recorder, base)

	// project="rilll" の wf-1 / foo で、AfterTime=base + 4s で問い合わせると ok-A2 (base+5s) が拾われる。
	resRill, err := recorder.NextSuccessAcrossRuns(ctx, NextSuccessOptions{
		ProjectID:  "rilll",
		WorkflowID: "wf-1",
		StepID:     "foo",
		AfterTime:  base.Add(4 * time.Second),
	})
	if err != nil {
		t.Fatalf("NextSuccessAcrossRuns (rilll): %v", err)
	}
	if !resRill.Found || resRill.Row.AgentCallID != "ok-A2" {
		t.Errorf("project=rilll: got %+v, want ok-A2", resRill)
	}

	// project="other" だと ok-X2 (base+5s) が拾われる。
	resOther, err := recorder.NextSuccessAcrossRuns(ctx, NextSuccessOptions{
		ProjectID:  "other",
		WorkflowID: "wf-1",
		StepID:     "foo",
		AfterTime:  base.Add(4 * time.Second),
	})
	if err != nil {
		t.Fatalf("NextSuccessAcrossRuns (other): %v", err)
	}
	if !resOther.Found || resOther.Row.AgentCallID != "ok-X2" {
		t.Errorf("project=other: got %+v, want ok-X2", resOther)
	}
}

// TestSQLiteRecorderFailureClustersExposesPattern2ParallelSlices は FailureClusters が
// resolution-chain 拡張 Pattern 2 用の parallel slice
// (RepresentativeWorkflowIDs / RepresentativeStepIDs / RepresentativeErrors /
//
//	RepresentativeResultSummaries) を RepresentativeAgentCallIDs と同順 / 同件数で
//
// 公開することを検証する。
//
// fixture: 同一 cluster (failure_category="provider_error", provider="claude",
// error="connection refused") に属する 2 つの失敗 agent_call を異なる workflow_id /
// step_id で投入する。Pattern 2 の NextSuccessAcrossRuns 入力に必要な (workflow_id,
// step_id) が cluster 経由で取れることを担保する。
func TestSQLiteRecorderFailureClustersExposesPattern2ParallelSlices(t *testing.T) {
	ctx := context.Background()
	recorder, err := OpenSQLite(filepath.Join(t.TempDir(), "akashic.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	base := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)

	// run と call を各 1 件ずつ投入。
	runs := []struct {
		runID     string
		startedAt time.Time
	}{
		{"run-1", base},
		{"run-2", base.Add(time.Minute)},
	}
	for _, r := range runs {
		if err := recorder.RecordWorkflowRunStart(ctx, WorkflowRun{
			ID:           r.runID,
			WorkflowID:   "wf-shared",
			WorkflowName: "WFShared",
			CWD:          "/tmp/rilll",
			ProjectID:    "rilll",
			StartedAt:    r.startedAt,
		}); err != nil {
			t.Fatalf("RecordWorkflowRunStart %s: %v", r.runID, err)
		}
	}

	// 2 つの失敗 call: 同 cluster (provider_error / claude / "Connection refused") だが
	// workflow_id / step_id / result_summary は異なる。
	calls := []struct {
		id, runID, workflowID, stepID, resultSummary string
		startedAt                                    time.Time
	}{
		{
			id: "ac-2", runID: "run-2", workflowID: "wf-newer", stepID: "step-newer",
			resultSummary: "newer summary at internal/foo.go:10",
			startedAt:     base.Add(time.Minute),
		},
		{
			id: "ac-1", runID: "run-1", workflowID: "wf-older", stepID: "step-older",
			resultSummary: "older summary",
			startedAt:     base,
		},
	}
	for _, c := range calls {
		if err := recorder.RecordAgentCallStart(ctx, AgentCall{
			ID:            c.id,
			WorkflowRunID: c.runID,
			WorkflowID:    c.workflowID,
			WorkflowName:  c.workflowID,
			StepID:        c.stepID,
			StepName:      c.stepID,
			ProviderID:    "claude",
			CWD:           "/tmp/rilll",
			ProjectID:     "rilll",
			StartedAt:     c.startedAt,
		}); err != nil {
			t.Fatalf("RecordAgentCallStart %s: %v", c.id, err)
		}
		if err := recorder.RecordAgentCallFinish(ctx, AgentCallResult{
			ID:              c.id,
			Success:         false,
			FailureCategory: "provider_error",
			Error:           "Connection refused",
			ResultSummary:   c.resultSummary,
			FinishedAt:      c.startedAt.Add(time.Second),
		}); err != nil {
			t.Fatalf("RecordAgentCallFinish %s: %v", c.id, err)
		}
	}

	res, err := recorder.FailureClusters(ctx, FailureOptions{ProjectID: "rilll"})
	if err != nil {
		t.Fatalf("FailureClusters: %v", err)
	}
	if len(res.Clusters) != 1 {
		t.Fatalf("len(Clusters) = %d, want 1 (同 cluster にまとまる)", len(res.Clusters))
	}
	c := res.Clusters[0]

	// finished_at DESC 順 = ac-2 (newer) が先頭。
	wantAgentCallIDs := []string{"ac-2", "ac-1"}
	if !equalStringSlices(c.RepresentativeAgentCallIDs, wantAgentCallIDs) {
		t.Errorf("RepresentativeAgentCallIDs = %v, want %v", c.RepresentativeAgentCallIDs, wantAgentCallIDs)
	}
	wantWorkflowIDs := []string{"wf-newer", "wf-older"}
	if !equalStringSlices(c.RepresentativeWorkflowIDs, wantWorkflowIDs) {
		t.Errorf("RepresentativeWorkflowIDs = %v, want %v", c.RepresentativeWorkflowIDs, wantWorkflowIDs)
	}
	wantStepIDs := []string{"step-newer", "step-older"}
	if !equalStringSlices(c.RepresentativeStepIDs, wantStepIDs) {
		t.Errorf("RepresentativeStepIDs = %v, want %v", c.RepresentativeStepIDs, wantStepIDs)
	}
	wantErrors := []string{"Connection refused", "Connection refused"}
	if !equalStringSlices(c.RepresentativeErrors, wantErrors) {
		t.Errorf("RepresentativeErrors = %v, want %v", c.RepresentativeErrors, wantErrors)
	}
	wantSummaries := []string{"newer summary at internal/foo.go:10", "older summary"}
	if !equalStringSlices(c.RepresentativeResultSummaries, wantSummaries) {
		t.Errorf("RepresentativeResultSummaries = %v, want %v", c.RepresentativeResultSummaries, wantSummaries)
	}
}
