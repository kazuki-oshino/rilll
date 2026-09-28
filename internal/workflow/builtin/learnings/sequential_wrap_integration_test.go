// Package learnings の本 file は agent-learnings spec / tasks.md 10.3
// (Boundary: SequentialWrapTest) で要求される
// `SequentialWithLearningsTail` × 実 `LearningsTailDef` × `runLearningsOrchestration` の
// integration 不変式を検証する。
//
// 単体テスト (`sequential_test.go`) は tail Definition を stub に差し替えて parity 観点
// (a) ~ (f) を検証するが、本 file は **実 LearningsTailDef** を後段に置き、
// `runLearningsOrchestration` の入口で `ParentRunIDFromContext(ctx)` が前段 base.Run の
// `WorkflowEvent.RunID` を取得できる経路を end-to-end で観測する。
//
// 検証観点 (tasks.md 10.3):
//
//   - (i) 前段 base.Run が `WorkflowEvent.RunID` を出した時、後段 LearningsTailDef の
//     orchestration 入口で同じ RunID が観測される (= ctx hand-off が成立する)。
//     観測手段: 後段 orchestration が Reader.AgentCallsByWorkflowRun(ctx, parentRunID) を
//     呼ぶので、spy reader で「実際に query されたパラメータ」を記録する。
//   - (ii) `SequentialWithLearningsTail` factory が返す Definition の Fn は base.Fn の
//     単純パススルーではなく前段 + 後段の合成 Fn であり、駆動すると後段 LearningsTailDef
//     の orchestration まで実行されることを観測する。
//
// 関連 Requirement: 1.2 (DistillLearningsAfter=true で wrap exposure / parent_run_id ctx
// hand-off) / 1.3 (Hidden=true tail Definition の SequentialWithLearningsTail 経由起動) /
// 1.4 (`rilll run agent-learnings` not-found) / 3.1 (PreFetch が parent_run_id 起点)。
package learnings

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflow"
)

// osStat は os.Stat の seam (test 用; 実装は標準呼び出しに委譲)。
var osStat = os.Stat

// spyReader は fakeReader の薄い拡張で、AgentCallsByWorkflowRun の query 履歴を記録する。
// 他 method は fakeReader の panic 経路を流用 (= 想定外呼び出しを fail-fast 検出する)。
type spyReader struct {
	mu            sync.Mutex
	queriedRunIDs []string
	rowsForRunID  map[string][]akashic.AgentCallRow
	defaultRows   []akashic.AgentCallRow
}

func (s *spyReader) AgentCallsByWorkflowRun(_ context.Context, runID string) ([]akashic.AgentCallRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queriedRunIDs = append(s.queriedRunIDs, runID)
	if rows, ok := s.rowsForRunID[runID]; ok {
		return rows, nil
	}
	return s.defaultRows, nil
}
func (s *spyReader) LearningsByWorkflowRun(_ context.Context, _ string) ([]akashic.LearningRow, error) {
	return nil, nil
}
func (s *spyReader) List(context.Context, akashic.ListOptions) (akashic.ListResult, error) {
	panic("spyReader.List unexpected call")
}
func (s *spyReader) AggregateByWorkflow(context.Context, akashic.WorkflowAggregateOptions) (akashic.WorkflowAggregateResult, error) {
	panic("spyReader.AggregateByWorkflow unexpected call")
}
func (s *spyReader) AggregateByProvider(context.Context, akashic.ProviderAggregateOptions) (akashic.ProviderAggregateResult, error) {
	panic("spyReader.AggregateByProvider unexpected call")
}
func (s *spyReader) Detail(context.Context, akashic.DetailOptions) (akashic.RunDetail, error) {
	panic("spyReader.Detail unexpected call")
}
func (s *spyReader) FailureClusters(context.Context, akashic.FailureOptions) (akashic.FailureClustersResult, error) {
	panic("spyReader.FailureClusters unexpected call")
}
func (s *spyReader) InRunResolutionsForCluster(context.Context, akashic.InRunResolutionOptions) (akashic.InRunResolutionsResult, error) {
	panic("spyReader.InRunResolutionsForCluster unexpected call")
}
func (s *spyReader) NextSuccessAcrossRuns(context.Context, akashic.NextSuccessOptions) (akashic.NextSuccessResult, error) {
	panic("spyReader.NextSuccessAcrossRuns unexpected call")
}

func (s *spyReader) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.queriedRunIDs))
	copy(out, s.queriedRunIDs)
	return out
}

// stubBaseDefEmittingRunID は WorkflowEvent.RunID を 1 件 emit する base def の test stub。
// Sequential 合成の前段として使い、後段に「この RunID」が hand-off されることを観測させる。
func stubBaseDefEmittingRunID(id, label, frontRunID string) workflow.Definition {
	return workflow.Definition{
		ID:    id,
		Label: label,
		Fn: func(ctx context.Context, _ string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			if onEvent != nil {
				onEvent(workflow.WorkflowEvent{
					Type:  workflow.WorkflowText,
					Text:  "front emitting run id",
					RunID: frontRunID,
				})
			}
			return nil
		},
	}
}

// TestSequentialWrapIntegration_ParentRunIDHandoff: 観点 (i)。
// 前段 base.Run が WorkflowEvent.RunID="front-run-X" を emit した時、
// 後段 LearningsTailDef の orchestration 入口で同 ID が ParentRunIDFromContext で取得され、
// Reader.AgentCallsByWorkflowRun が "front-run-X" を引数に呼ばれることを観測する。
func TestSequentialWrapIntegration_ParentRunIDHandoff(t *testing.T) {
	cwd := t.TempDir()
	const frontRunID = "front-run-X"

	rec := &fakeWorkflowRecorder{}
	spy := &spyReader{defaultRows: nil} // 0 件で applied-no-op に落とす (precondition 違反ではない)
	agent := &fakeWorkflowAgent{
		reader:   spy,
		recorder: rec,
	}

	// 前段: stub base def が WorkflowEvent.RunID="front-run-X" を 1 件 emit。
	front := stubBaseDefEmittingRunID("base-X", "Base X", frontRunID)

	// 後段: 実 LearningsTailDef を使う。
	// SequentialWithLearningsTail の deps を直接組まずに、production 経路と同じ
	// `defaultSequentialWithLearningsTailDeps(agent, base)` 相当を構築する。
	deps := defaultSequentialWithLearningsTailDeps(agent, front)

	// orchestration を直接駆動。outermost ctx には RunMetadata + Recorder のみ注入する
	// (parent_run_id は前段 base.Run の WorkflowEvent.RunID から captured されることを観測したい)。
	tailRunID := "tail-run-Y"
	ctx := makeRunCtx(t, cwd, tailRunID, "" /* parentRunID 未注入 */, rec)

	if err := runSequentialWithLearningsTailOrchestration(ctx, cwd, nil, nil, deps); err != nil {
		t.Fatalf("orchestration: %v", err)
	}

	// 観点 (i): Reader が "front-run-X" を引数に query された。
	queried := spy.snapshot()
	if len(queried) != 1 {
		t.Fatalf("Reader.AgentCallsByWorkflowRun query count = %d, want 1 (queried=%v)", len(queried), queried)
	}
	if queried[0] != frontRunID {
		t.Errorf("Reader queried with %q, want %q (前段 base.Run の WorkflowEvent.RunID が ctx 経由で hand-off される必要)", queried[0], frontRunID)
	}
}

// TestSequentialWrapIntegration_FactoryReturnsCompositeFn: 観点 (ii)。
// `SequentialWithLearningsTail` factory が返す Definition の Fn は、base.Fn の単純コピーではなく
// 前段 + 後段の合成 Fn であることを観測する (Fn pointer 比較ではなく挙動で確認)。
//
// 観測手段: factory 経由 Fn を駆動すると、base.Fn だけでなく LearningsTailDef の orchestration が
// 起動し、Reader.AgentCallsByWorkflowRun が前段の RunID で 1 度 call される。
// 加えて factory metadata (ID 同一 / Label に marker / Args 継承) を確認する。
func TestSequentialWrapIntegration_FactoryReturnsCompositeFn(t *testing.T) {
	cwd := t.TempDir()
	rec := &fakeWorkflowRecorder{}
	spy := &spyReader{defaultRows: nil}
	agent := &fakeWorkflowAgent{
		reader:   spy,
		recorder: rec,
	}

	const frontRunID = "front-run-W"
	base := stubBaseDefEmittingRunID("base-W", "Base W", frontRunID)
	wrapped := SequentialWithLearningsTail(base.ID, agent, base)

	// 不変式: ID 同 base、Label に marker。
	if wrapped.ID != base.ID {
		t.Errorf("wrapped.ID = %q, want %q (in-place 同 ID 維持契約)", wrapped.ID, base.ID)
	}
	if wrapped.Label != base.Label+learningsTailLabelMarker {
		t.Errorf("wrapped.Label = %q, want %q+marker", wrapped.Label, base.Label)
	}

	// wrapped.Fn を駆動すると base.Fn だけでなく LearningsTailDef も走るため Reader が query される。
	tailRunID := "tail-run-W"
	ctx := makeRunCtx(t, cwd, tailRunID, "" /* parentRunID 未注入 */, rec)
	if wrapped.Fn == nil {
		t.Fatalf("wrapped.Fn is nil")
	}
	if err := wrapped.Fn(ctx, cwd, nil, nil); err != nil {
		t.Fatalf("wrapped.Fn: %v", err)
	}
	queried := spy.snapshot()
	if len(queried) != 1 || queried[0] != frontRunID {
		t.Errorf("Reader queried = %v, want [%q] (factory が wrap 後 Fn を返している証跡)", queried, frontRunID)
	}

	// 確認: wrap 経由 Fn が `.rilll/agent-learnings/<新規 runID>/apply-result.json` を生む。
	// `tailDef.Run` は内部で新規 RunMetadata を割り当てるため、artifact path の runID 部は
	// 予測できない。よってディレクトリ走査で apply-result.json の existence を確認する。
	if !applyArtifactExistsAnywhere(t, filepath.Join(cwd, ".rilll", learningsWorkflowID)) {
		t.Errorf("wrap 後 tail orchestration が apply-result.json を書いていない (tail 内部で新規 runID が割り当てられるためディレクトリ走査で確認)")
	}
}

// applyArtifactExistsAnywhere は `<root>/<runID>/apply-result.json` のいずれかが存在するかを返す。
// `tailDef.Run` が内部で新規 RunMetadata を生成するため、runID を予測できない artifact 検証で使う。
func applyArtifactExistsAnywhere(t *testing.T, root string) bool {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := osStat(filepath.Join(root, e.Name(), applyResultFilename)); err == nil {
			return true
		}
	}
	return false
}
