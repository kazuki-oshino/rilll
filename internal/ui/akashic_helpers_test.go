// Package ui のアカシックモード関連 Model 層テスト共通ヘルパ。
//
// fakeReader / fakeArtifactReader は record.Reader / record.ArtifactReader を
// 満たす最小実装で、`a` キー押下経路や Phase ガード越しディスパッチの
// Model 層テストから注入する。詳細な観測 (呼び出し回数 / 受領した Options 等)
// は本パッケージの個別テストが必要に応じて参照する。
package ui

import (
	"context"

	record "github.com/kazuki-oshino/rilll/internal/akashic"
)

type fakeReader struct {
	listCalls  int
	listOpts   record.ListOptions
	listResult record.ListResult
	listErr    error

	aggWorkflowCalls  int
	aggWorkflowOpts   record.WorkflowAggregateOptions
	aggWorkflowResult record.WorkflowAggregateResult
	aggWorkflowErr    error

	aggProviderCalls  int
	aggProviderOpts   record.ProviderAggregateOptions
	aggProviderResult record.ProviderAggregateResult
	aggProviderErr    error

	detailCalls  int
	detailOpts   record.DetailOptions
	detailResult record.RunDetail
	detailErr    error

	failureCalls  int
	failureOpts   record.FailureOptions
	failureResult record.FailureClustersResult
	failureErr    error
}

func (r *fakeReader) List(_ context.Context, opts record.ListOptions) (record.ListResult, error) {
	r.listCalls++
	r.listOpts = opts
	return r.listResult, r.listErr
}

func (r *fakeReader) AggregateByWorkflow(_ context.Context, opts record.WorkflowAggregateOptions) (record.WorkflowAggregateResult, error) {
	r.aggWorkflowCalls++
	r.aggWorkflowOpts = opts
	return r.aggWorkflowResult, r.aggWorkflowErr
}

func (r *fakeReader) AggregateByProvider(_ context.Context, opts record.ProviderAggregateOptions) (record.ProviderAggregateResult, error) {
	r.aggProviderCalls++
	r.aggProviderOpts = opts
	return r.aggProviderResult, r.aggProviderErr
}

func (r *fakeReader) Detail(_ context.Context, opts record.DetailOptions) (record.RunDetail, error) {
	r.detailCalls++
	r.detailOpts = opts
	return r.detailResult, r.detailErr
}

func (r *fakeReader) FailureClusters(_ context.Context, opts record.FailureOptions) (record.FailureClustersResult, error) {
	r.failureCalls++
	r.failureOpts = opts
	return r.failureResult, r.failureErr
}

func (r *fakeReader) InRunResolutionsForCluster(_ context.Context, _ record.InRunResolutionOptions) (record.InRunResolutionsResult, error) {
	return record.InRunResolutionsResult{}, nil
}

func (r *fakeReader) NextSuccessAcrossRuns(_ context.Context, _ record.NextSuccessOptions) (record.NextSuccessResult, error) {
	return record.NextSuccessResult{}, nil
}

// AgentCallsByWorkflowRun は agent-learnings spec (task 3.2) で追加された Reader 拡張への
// no-op 追従。本 ui test は Akashic Model 経路の検証が責務で agent-learnings 経路は
// 対象外のため履歴は保持しない。
func (r *fakeReader) AgentCallsByWorkflowRun(_ context.Context, _ string) ([]record.AgentCallRow, error) {
	return nil, nil
}

// LearningsByWorkflowRun は agent-learnings spec (task 3.2) で追加された Reader 拡張への
// no-op 追従。同上の理由で履歴は保持しない。
func (r *fakeReader) LearningsByWorkflowRun(_ context.Context, _ string) ([]record.LearningRow, error) {
	return nil, nil
}
