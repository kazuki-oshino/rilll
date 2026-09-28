package akashic

import (
	"context"
	"testing"
)

// fakeReader は ProjectScopedReader が委譲先に渡した opts を捕捉する spy。
// 各メソッドで Options を保存し、テストから ProjectID 上書きが効いていることを確認する。
type fakeReader struct {
	listOpts        ListOptions
	wfAggOpts       WorkflowAggregateOptions
	providerAggOpts ProviderAggregateOptions
	failureOpts     FailureOptions
	detailOpts      DetailOptions
	inRunOpts       InRunResolutionOptions
	nextSuccessOpts NextSuccessOptions

	// agent-learnings spec Task 3.2: 新規 method の引数捕捉と固定戻り値。
	agentCallsRunID string
	agentCallsRows  []AgentCallRow
	learningsRunID  string
	learningsRows   []LearningRow
}

func (f *fakeReader) List(_ context.Context, opts ListOptions) (ListResult, error) {
	f.listOpts = opts
	return ListResult{}, nil
}

func (f *fakeReader) AggregateByWorkflow(_ context.Context, opts WorkflowAggregateOptions) (WorkflowAggregateResult, error) {
	f.wfAggOpts = opts
	return WorkflowAggregateResult{}, nil
}

func (f *fakeReader) AggregateByProvider(_ context.Context, opts ProviderAggregateOptions) (ProviderAggregateResult, error) {
	f.providerAggOpts = opts
	return ProviderAggregateResult{}, nil
}

func (f *fakeReader) Detail(_ context.Context, opts DetailOptions) (RunDetail, error) {
	f.detailOpts = opts
	return RunDetail{}, nil
}

func (f *fakeReader) FailureClusters(_ context.Context, opts FailureOptions) (FailureClustersResult, error) {
	f.failureOpts = opts
	return FailureClustersResult{}, nil
}

func (f *fakeReader) InRunResolutionsForCluster(_ context.Context, opts InRunResolutionOptions) (InRunResolutionsResult, error) {
	f.inRunOpts = opts
	return InRunResolutionsResult{}, nil
}

func (f *fakeReader) NextSuccessAcrossRuns(_ context.Context, opts NextSuccessOptions) (NextSuccessResult, error) {
	f.nextSuccessOpts = opts
	return NextSuccessResult{}, nil
}

// AgentCallsByWorkflowRun は agent-learnings spec Task 3.2 で追加された Reader 拡張。
// 引数の workflow_run_id を捕捉し、固定戻り値 agentCallsRows を返す。
func (f *fakeReader) AgentCallsByWorkflowRun(_ context.Context, workflowRunID string) ([]AgentCallRow, error) {
	f.agentCallsRunID = workflowRunID
	return f.agentCallsRows, nil
}

// LearningsByWorkflowRun は agent-learnings spec Task 3.2 で追加された Reader 拡張。
// 引数の workflow_run_id を捕捉し、固定戻り値 learningsRows を返す。
func (f *fakeReader) LearningsByWorkflowRun(_ context.Context, workflowRunID string) ([]LearningRow, error) {
	f.learningsRunID = workflowRunID
	return f.learningsRows, nil
}

func TestProjectScopedReaderOverridesProjectIDOnList(t *testing.T) {
	inner := &fakeReader{}
	scoped := NewProjectScopedReader(inner, "rilll")

	// 呼び出し側が ProjectID="other" を渡しても、scope の "rilll" に上書きされる
	if _, err := scoped.List(context.Background(), ListOptions{ProjectID: "other", WorkflowID: "wf"}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if inner.listOpts.ProjectID != "rilll" {
		t.Fatalf("List ProjectID 上書き失敗: got=%q want=rilll", inner.listOpts.ProjectID)
	}
	if inner.listOpts.WorkflowID != "wf" {
		t.Fatalf("List 他フィールド (WorkflowID) は破壊しない: got=%q want=wf", inner.listOpts.WorkflowID)
	}
}

func TestProjectScopedReaderOverridesProjectIDOnAggregateByWorkflow(t *testing.T) {
	inner := &fakeReader{}
	scoped := NewProjectScopedReader(inner, "rilll")

	if _, err := scoped.AggregateByWorkflow(context.Background(), WorkflowAggregateOptions{ProjectID: "other"}); err != nil {
		t.Fatalf("AggregateByWorkflow: %v", err)
	}
	if inner.wfAggOpts.ProjectID != "rilll" {
		t.Fatalf("AggregateByWorkflow ProjectID 上書き失敗: got=%q", inner.wfAggOpts.ProjectID)
	}
}

func TestProjectScopedReaderOverridesProjectIDOnAggregateByProvider(t *testing.T) {
	inner := &fakeReader{}
	scoped := NewProjectScopedReader(inner, "rilll")

	if _, err := scoped.AggregateByProvider(context.Background(), ProviderAggregateOptions{ProjectID: "other"}); err != nil {
		t.Fatalf("AggregateByProvider: %v", err)
	}
	if inner.providerAggOpts.ProjectID != "rilll" {
		t.Fatalf("AggregateByProvider ProjectID 上書き失敗: got=%q", inner.providerAggOpts.ProjectID)
	}
}

func TestProjectScopedReaderOverridesProjectIDOnFailureClusters(t *testing.T) {
	inner := &fakeReader{}
	scoped := NewProjectScopedReader(inner, "rilll")

	if _, err := scoped.FailureClusters(context.Background(), FailureOptions{ProjectID: "other"}); err != nil {
		t.Fatalf("FailureClusters: %v", err)
	}
	if inner.failureOpts.ProjectID != "rilll" {
		t.Fatalf("FailureClusters ProjectID 上書き失敗: got=%q", inner.failureOpts.ProjectID)
	}
}

func TestProjectScopedReaderOverridesProjectIDOnInRunResolutionsForCluster(t *testing.T) {
	inner := &fakeReader{}
	scoped := NewProjectScopedReader(inner, "rilll")

	if _, err := scoped.InRunResolutionsForCluster(context.Background(), InRunResolutionOptions{ProjectID: "other", FailingAgentCallIDs: []string{"a"}}); err != nil {
		t.Fatalf("InRunResolutionsForCluster: %v", err)
	}
	if inner.inRunOpts.ProjectID != "rilll" {
		t.Fatalf("InRunResolutionsForCluster ProjectID 上書き失敗: got=%q want=rilll", inner.inRunOpts.ProjectID)
	}
	if len(inner.inRunOpts.FailingAgentCallIDs) != 1 || inner.inRunOpts.FailingAgentCallIDs[0] != "a" {
		t.Fatalf("InRunResolutionsForCluster 他フィールドは破壊しない: got=%v", inner.inRunOpts.FailingAgentCallIDs)
	}
}

func TestProjectScopedReaderOverridesProjectIDOnNextSuccessAcrossRuns(t *testing.T) {
	inner := &fakeReader{}
	scoped := NewProjectScopedReader(inner, "rilll")

	if _, err := scoped.NextSuccessAcrossRuns(context.Background(), NextSuccessOptions{ProjectID: "other", WorkflowID: "wf"}); err != nil {
		t.Fatalf("NextSuccessAcrossRuns: %v", err)
	}
	if inner.nextSuccessOpts.ProjectID != "rilll" {
		t.Fatalf("NextSuccessAcrossRuns ProjectID 上書き失敗: got=%q want=rilll", inner.nextSuccessOpts.ProjectID)
	}
	if inner.nextSuccessOpts.WorkflowID != "wf" {
		t.Fatalf("NextSuccessAcrossRuns 他フィールドは破壊しない: got=%q want=wf", inner.nextSuccessOpts.WorkflowID)
	}
}

func TestProjectScopedReaderDoesNotOverrideDetailOpts(t *testing.T) {
	// Detail は run ID 直指定なので scope 強制しない (global 横断クエリ)。
	inner := &fakeReader{}
	scoped := NewProjectScopedReader(inner, "rilll")

	if _, err := scoped.Detail(context.Background(), DetailOptions{WorkflowRunID: "run-123"}); err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if inner.detailOpts.WorkflowRunID != "run-123" {
		t.Fatalf("Detail WorkflowRunID は変更されない: got=%q want=run-123", inner.detailOpts.WorkflowRunID)
	}
}

// TestProjectScopedReaderEnforcesProjectScopeOnAgentCallsByWorkflowRun は
// agent-learnings Task 3.2 の核心: ProjectScopedReader.AgentCallsByWorkflowRun は
// inner Reader 戻り値を r.projectID で post-filter し、別 project の row を排除する。
// PreFetch (Req 3.6) の project scoping 不変式の根拠テスト。
func TestProjectScopedReaderEnforcesProjectScopeOnAgentCallsByWorkflowRun(t *testing.T) {
	inner := &fakeReader{
		agentCallsRows: []AgentCallRow{
			{ID: "ac-rilll-1", WorkflowRunID: "run-1", ProjectID: "rilll"},
			{ID: "ac-other-1", WorkflowRunID: "run-1", ProjectID: "other"},
			{ID: "ac-rilll-2", WorkflowRunID: "run-1", ProjectID: "rilll"},
		},
	}
	scoped := NewProjectScopedReader(inner, "rilll")

	got, err := scoped.AgentCallsByWorkflowRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("AgentCallsByWorkflowRun: %v", err)
	}
	if inner.agentCallsRunID != "run-1" {
		t.Fatalf("AgentCallsByWorkflowRun runID 透過失敗: got=%q want=run-1", inner.agentCallsRunID)
	}
	if len(got) != 2 {
		t.Fatalf("len(got)=%d want=2 (rilll 2 件のみ残るはず): got=%v", len(got), got)
	}
	for _, r := range got {
		if r.ProjectID != "rilll" {
			t.Errorf("scope 外 row が混入: %+v", r)
		}
	}
}

// TestProjectScopedReaderEmptyProjectIDPassesThroughAgentCalls は
// projectID == "" の wrapper (= 全 project) は inner 戻り値をそのまま透過させることを確認する
// (空文字 sentinel = 全 project / scoped.go コメント参照)。
func TestProjectScopedReaderEmptyProjectIDPassesThroughAgentCalls(t *testing.T) {
	inner := &fakeReader{
		agentCallsRows: []AgentCallRow{
			{ID: "ac-rilll", ProjectID: "rilll"},
			{ID: "ac-other", ProjectID: "other"},
		},
	}
	scoped := NewProjectScopedReader(inner, "")

	got, err := scoped.AgentCallsByWorkflowRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("AgentCallsByWorkflowRun: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("空 projectID は全件透過: got len=%d want=2", len(got))
	}
}

// TestProjectScopedReaderDoesNotFilterLearningsByWorkflowRun は
// LearningsByWorkflowRun が run ID 直指定パターン (Detail 同形) で scope 強制しないことを
// 確認する (task 3.2 / design.md L671)。learnings table 行は project_id を持たない。
func TestProjectScopedReaderDoesNotFilterLearningsByWorkflowRun(t *testing.T) {
	inner := &fakeReader{
		learningsRows: []LearningRow{
			{ID: "l-1", WorkflowRunID: "run-1"},
			{ID: "l-2", WorkflowRunID: "run-1"},
		},
	}
	scoped := NewProjectScopedReader(inner, "rilll")

	got, err := scoped.LearningsByWorkflowRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("LearningsByWorkflowRun: %v", err)
	}
	if inner.learningsRunID != "run-1" {
		t.Fatalf("LearningsByWorkflowRun runID 透過失敗: got=%q want=run-1", inner.learningsRunID)
	}
	if len(got) != 2 {
		t.Fatalf("LearningsByWorkflowRun は scope 強制しないため透過: got len=%d want=2", len(got))
	}
}
