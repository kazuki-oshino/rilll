// Package learnings の本 file は LearningsTailDef factory の単体テスト (tasks.md 8.2)。
//
// 検証観点:
//
//   - (a) Definition.Hidden = true (Req 1.1)
//   - (b) Definition.ID = "agent-learnings" (Req 1.1)
//   - (c) Definition.Args = nil (manual mode 不在)
//   - (d) Definition.Fn が呼ばれると runLearningsOrchestration が起動される
//     (precondition 違反でも early return + WorkflowDone emit が観測される)
package learnings

import (
	"context"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

func TestLearningsTailDefHiddenAndID(t *testing.T) {
	def := LearningsTailDef(&fakeWorkflowAgent{})
	if !def.Hidden {
		t.Errorf("Hidden = false, want true")
	}
	if def.ID != learningsWorkflowID {
		t.Errorf("ID = %q, want %q", def.ID, learningsWorkflowID)
	}
	if len(def.Args) != 0 {
		t.Errorf("Args = %+v, want empty", def.Args)
	}
}

// TestLearningsTailDefFnRunsOrchestration: Fn が runLearningsOrchestration 経由で動作することを
// 観測する (parent_run_id 未注入経路で applied-no-op + WorkflowDone emit を観測)。
func TestLearningsTailDefFnRunsOrchestration(t *testing.T) {
	cwd := t.TempDir()
	rec := &fakeWorkflowRecorder{}
	agent := &fakeWorkflowAgent{recorder: rec}
	def := LearningsTailDef(agent)

	ctx := makeRunCtx(t, cwd, "run-1", "" /* parent_run_id 未注入 */, rec)
	var doneCount int
	onEvent := func(e workflow.WorkflowEvent) {
		if e.Type == workflow.WorkflowDone {
			doneCount++
		}
	}
	if err := def.Fn(ctx, cwd, onEvent, nil); err != nil {
		t.Fatalf("Fn: %v", err)
	}
	if doneCount != 1 {
		t.Errorf("WorkflowDone emit = %d, want 1", doneCount)
	}
	a := readApplyArtifact(t, cwd, "run-1")
	if a.Verdict != VerdictAppliedNoOp {
		t.Errorf("Verdict = %q, want applied-no-op", a.Verdict)
	}
}

// TestLearningsTailDefAgentNilSafety: agent==nil でも Definition 構築は成功する。
func TestLearningsTailDefAgentNilSafety(t *testing.T) {
	var agent workflowctx.AgentCaller
	def := LearningsTailDef(agent)
	if def.ID != learningsWorkflowID {
		t.Errorf("ID = %q, want %q", def.ID, learningsWorkflowID)
	}
}

// 互換性 noop: context import を使う test を 1 つ残しておく
func TestLearningsTailDefContextNoOp(t *testing.T) {
	_ = context.Background()
}
