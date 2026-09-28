// Package akashic の scoped.go は ProjectScopedReader を提供する。
//
// 設計意図:
//   - グローバル akashic SQLite に対し「プロジェクト局所な見え方」を提供する薄い decorator。
//   - List / AggregateByWorkflow / AggregateByProvider / FailureClusters は Options.ProjectID を
//     コンストラクタで受けた projectID で常に上書きしてから inner Reader に委譲する。
//   - Detail は run ID 直指定なので scope 強制はしない (global 横断が意味的に正しい)。
//   - decorator は副作用を持たず (DB 接続を保持しない)、ライフサイクル管理は inner Reader 側の責務。
//
// 想定ユースケース:
//   - memory-distiller / 各プロジェクトでの統計 → ProjectScopedReader 経由で叩く。
//   - improve-workflow / rilll 自己改善 → 素の Reader (グローバル全宇宙) を直接使う。
package akashic

import "context"

// ProjectScopedReader は Reader を wrap し、Options.ProjectID を強制的に上書きする decorator。
// Detail は run ID 直指定のため上書き対象外 (global 横断クエリとして透過する)。
type ProjectScopedReader struct {
	inner     Reader
	projectID string
}

// NewProjectScopedReader は ProjectScopedReader を構築する。
// projectID が空文字列のとき本 decorator は実質 no-op になる (空文字 sentinel = 全 project)。
// 呼び出し側で empty な project に絞りたい場合は別途 sentinel を使い分けること。
func NewProjectScopedReader(inner Reader, projectID string) *ProjectScopedReader {
	return &ProjectScopedReader{inner: inner, projectID: projectID}
}

// List は inner.List に opts.ProjectID を r.projectID で上書きしてから委譲する。
func (r *ProjectScopedReader) List(ctx context.Context, opts ListOptions) (ListResult, error) {
	opts.ProjectID = r.projectID
	return r.inner.List(ctx, opts)
}

// AggregateByWorkflow は inner.AggregateByWorkflow に opts.ProjectID を上書きしてから委譲する。
func (r *ProjectScopedReader) AggregateByWorkflow(ctx context.Context, opts WorkflowAggregateOptions) (WorkflowAggregateResult, error) {
	opts.ProjectID = r.projectID
	return r.inner.AggregateByWorkflow(ctx, opts)
}

// AggregateByProvider は inner.AggregateByProvider に opts.ProjectID を上書きしてから委譲する。
func (r *ProjectScopedReader) AggregateByProvider(ctx context.Context, opts ProviderAggregateOptions) (ProviderAggregateResult, error) {
	opts.ProjectID = r.projectID
	return r.inner.AggregateByProvider(ctx, opts)
}

// FailureClusters は inner.FailureClusters に opts.ProjectID を上書きしてから委譲する。
func (r *ProjectScopedReader) FailureClusters(ctx context.Context, opts FailureOptions) (FailureClustersResult, error) {
	opts.ProjectID = r.projectID
	return r.inner.FailureClusters(ctx, opts)
}

// InRunResolutionsForCluster は inner.InRunResolutionsForCluster に opts.ProjectID を
// 上書きしてから委譲する (Pattern 1: 同 run 内 resolution lookup)。
func (r *ProjectScopedReader) InRunResolutionsForCluster(ctx context.Context, opts InRunResolutionOptions) (InRunResolutionsResult, error) {
	opts.ProjectID = r.projectID
	return r.inner.InRunResolutionsForCluster(ctx, opts)
}

// NextSuccessAcrossRuns は inner.NextSuccessAcrossRuns に opts.ProjectID を
// 上書きしてから委譲する (Pattern 2: 別 run での next success lookup)。
func (r *ProjectScopedReader) NextSuccessAcrossRuns(ctx context.Context, opts NextSuccessOptions) (NextSuccessResult, error) {
	opts.ProjectID = r.projectID
	return r.inner.NextSuccessAcrossRuns(ctx, opts)
}

// Detail は run ID 直指定の単一 run 取得のため、project scope を強制しない。
// 別 project の run でも (allowlist 制約に従えば) 取得できる。
func (r *ProjectScopedReader) Detail(ctx context.Context, opts DetailOptions) (RunDetail, error) {
	return r.inner.Detail(ctx, opts)
}

// AgentCallsByWorkflowRun は inner.AgentCallsByWorkflowRun の戻り値を r.projectID で
// post-filter してから返す (agent-learnings spec task 3.2 / Req 3.6)。
//
// 設計上の選択:
//   - Reader interface 側は (ctx, runID) の単純シグネチャを保つ (design.md L502)。
//   - PreFetch phase は本 wrapper 経由でしか agent_calls を読まないため、project scope
//     強制は wrapper 側の post-filter で物理的に保証される。
//   - r.projectID == "" (= 全 project sentinel) のときは inner 戻り値をそのまま透過する
//     (NewProjectScopedReader の no-op 挙動と整合)。
//   - 比較対象 row.ProjectID が空文字の row (legacy migration 経路で project_id 未設定
//     だった agent_call) は scope 不一致として除外される。これは project_id backfill
//     migration が走った後の DB 状態を前提とする (legacy fallback 経路は残さない)。
func (r *ProjectScopedReader) AgentCallsByWorkflowRun(ctx context.Context, workflowRunID string) ([]AgentCallRow, error) {
	rows, err := r.inner.AgentCallsByWorkflowRun(ctx, workflowRunID)
	if err != nil {
		return nil, err
	}
	if r.projectID == "" {
		return rows, nil
	}
	filtered := make([]AgentCallRow, 0, len(rows))
	for _, row := range rows {
		if row.ProjectID == r.projectID {
			filtered = append(filtered, row)
		}
	}
	return filtered, nil
}

type descendantWorkflowRunReader interface {
	DescendantWorkflowRunIDs(ctx context.Context, parentRunID string) ([]string, error)
}

// DescendantWorkflowRunIDs は inner が lineage 読み取りを提供する場合だけ委譲する。
// 旧 Reader 実装では empty slice を返し、parent run 直下のみを読む従来挙動にフォールバックする。
func (r *ProjectScopedReader) DescendantWorkflowRunIDs(ctx context.Context, parentRunID string) ([]string, error) {
	reader, ok := r.inner.(descendantWorkflowRunReader)
	if !ok {
		return []string{}, nil
	}
	return reader.DescendantWorkflowRunIDs(ctx, parentRunID)
}

// LearningsByWorkflowRun は run ID 直指定パターン (Detail と同形) のため scope 強制せず、
// inner 戻り値をそのまま透過する (agent-learnings spec task 3.2 / design.md L671)。
//
// learnings table は project_id 列を持たず、行は workflow_run_id 単位で論理的に親 run に
// 紐づいているため、別 project の row が混入する経路は存在しない。
func (r *ProjectScopedReader) LearningsByWorkflowRun(ctx context.Context, workflowRunID string) ([]LearningRow, error) {
	return r.inner.LearningsByWorkflowRun(ctx, workflowRunID)
}
