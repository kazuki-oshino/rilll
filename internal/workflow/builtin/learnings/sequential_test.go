// Package learnings の本 file は task 8.3 で要求される
// `SequentialWithLearningsTail` Sequential ラッパの不変式を検証する
// (v1 distiller `sequential_test.go` と同形 mirror)。
//
// 検証観点:
//
//   - (a) 前段 Fn が nil 返却 → 後段 Fn が 1 回起動 (parity)。
//   - (b) 前段 Fn が non-nil error → 後段未起動 + 同 error 伝播。
//   - (b') 前段 WorkflowError event → 後段未起動 + Message が error として伝播。
//   - (c) 子 events が時系列順で親 onEvent に転送される。
//   - (d) 前段 Run の WorkflowRunID が ctx 経由で後段に hand-off される。
//   - (e) Definition factory の field 契約 (ID / Label に marker / Args 継承 / Hidden 継承)。
//   - (f) applyLearningsTailWraps が指定 base ID のみを wrap する。
package learnings

import (
	"context"
	"errors"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// stubLearningsDef は Sequential ラッパテスト用の最小 Definition stub helper。
func stubLearningsDef(id, label string, fn func(ctx context.Context) error, emit []workflow.WorkflowEvent) workflow.Definition {
	return workflow.Definition{
		ID:    id,
		Label: label,
		Fn: func(ctx context.Context, _ string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			for _, e := range emit {
				if onEvent != nil {
					onEvent(e)
				}
			}
			if fn != nil {
				return fn(ctx)
			}
			return nil
		},
	}
}

// TestSequentialWithLearningsTail_ParityFrontStageNilRunsTail: parity (a)。
func TestSequentialWithLearningsTail_ParityFrontStageNilRunsTail(t *testing.T) {
	var frontCalls, tailCalls int
	frontDef := stubLearningsDef("test-front", "Test Front", func(_ context.Context) error {
		frontCalls++
		return nil
	}, nil)
	tailDef := stubLearningsDef("test-tail", "Test Tail", func(_ context.Context) error {
		tailCalls++
		return nil
	}, nil)

	deps := sequentialWithLearningsTailDeps{frontDef: frontDef, tailDef: tailDef}
	err := runSequentialWithLearningsTailOrchestration(context.Background(), t.TempDir(), nil, nil, deps)
	if err != nil {
		t.Fatalf("orchestration: %v", err)
	}
	if frontCalls != 1 {
		t.Errorf("frontCalls=%d, want 1", frontCalls)
	}
	if tailCalls != 1 {
		t.Errorf("tailCalls=%d, want 1 (parity (a))", tailCalls)
	}
}

// TestSequentialWithLearningsTail_FrontStageErrorSkipsTail: parity (b)。
func TestSequentialWithLearningsTail_FrontStageErrorSkipsTail(t *testing.T) {
	wantErr := errors.New("front-boom")
	var tailCalls int
	frontDef := stubLearningsDef("test-front", "Test Front", func(_ context.Context) error {
		return wantErr
	}, nil)
	tailDef := stubLearningsDef("test-tail", "Test Tail", func(_ context.Context) error {
		tailCalls++
		return nil
	}, nil)

	deps := sequentialWithLearningsTailDeps{frontDef: frontDef, tailDef: tailDef}
	err := runSequentialWithLearningsTailOrchestration(context.Background(), t.TempDir(), nil, nil, deps)
	if err == nil {
		t.Fatalf("orchestration: nil (前段 error を期待)")
	}
	if !errors.Is(err, wantErr) && err.Error() != wantErr.Error() {
		t.Errorf("err=%v, want %v", err, wantErr)
	}
	if tailCalls != 0 {
		t.Errorf("tailCalls=%d, want 0 (parity (b))", tailCalls)
	}
}

// TestSequentialWithLearningsTail_WorkflowErrorEventSkipsTail: parity (b')。
func TestSequentialWithLearningsTail_WorkflowErrorEventSkipsTail(t *testing.T) {
	const errMsg = "[verdict:failed] front failed"
	var tailCalls int
	frontDef := stubLearningsDef("test-front", "Test Front", nil, []workflow.WorkflowEvent{
		{Type: workflow.WorkflowError, Message: errMsg},
	})
	tailDef := stubLearningsDef("test-tail", "Test Tail", func(_ context.Context) error {
		tailCalls++
		return nil
	}, nil)

	deps := sequentialWithLearningsTailDeps{frontDef: frontDef, tailDef: tailDef}
	err := runSequentialWithLearningsTailOrchestration(context.Background(), t.TempDir(), nil, nil, deps)
	if err == nil {
		t.Fatalf("orchestration: nil (WorkflowError event → error 期待)")
	}
	if err.Error() != errMsg {
		t.Errorf("err=%q, want %q", err.Error(), errMsg)
	}
	if tailCalls != 0 {
		t.Errorf("tailCalls=%d, want 0 (parity (b'))", tailCalls)
	}
}

// TestSequentialWithLearningsTail_ForwardsChildEventsInOrder: parity (c)。
func TestSequentialWithLearningsTail_ForwardsChildEventsInOrder(t *testing.T) {
	frontDef := workflow.Definition{
		ID:    "test-front",
		Label: "Test Front",
		Fn: func(_ context.Context, _ string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "front-step-1"})
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "front-step-2"})
			return nil
		},
	}
	tailDef := workflow.Definition{
		ID:    "test-tail",
		Label: "Test Tail",
		Fn: func(_ context.Context, _ string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "tail-step-1"})
			return nil
		},
	}

	deps := sequentialWithLearningsTailDeps{frontDef: frontDef, tailDef: tailDef}
	var parentTexts []string
	parent := func(e workflow.WorkflowEvent) {
		if e.Type == workflow.WorkflowText {
			parentTexts = append(parentTexts, e.Text)
		}
	}
	err := runSequentialWithLearningsTailOrchestration(context.Background(), t.TempDir(), parent, nil, deps)
	if err != nil {
		t.Fatalf("orchestration: %v", err)
	}
	wantTexts := []string{"front-step-1", "front-step-2", "tail-step-1"}
	if len(parentTexts) != len(wantTexts) {
		t.Fatalf("texts=%v, want %v", parentTexts, wantTexts)
	}
	for i, w := range wantTexts {
		if parentTexts[i] != w {
			t.Errorf("texts[%d]=%q, want %q", i, parentTexts[i], w)
		}
	}
}

// TestSequentialWithLearningsTail_InjectsParentRunIDIntoCtx: 観点 (d)。
// 前段 Run() の WorkflowRunID が後段 ctx に hand-off されることを観測する。
func TestSequentialWithLearningsTail_InjectsParentRunIDIntoCtx(t *testing.T) {
	var frontRunIDFromMetadata, parentRunIDFromCtx string

	frontDef := workflow.Definition{
		ID:    "test-front",
		Label: "Test Front",
		Fn: func(ctx context.Context, _ string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			if md, ok := workflowctx.RunMetadataFromContext(ctx); ok {
				frontRunIDFromMetadata = md.WorkflowRunID
			}
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "front-tick"})
			return nil
		},
	}
	tailDef := workflow.Definition{
		ID:    "test-tail",
		Label: "Test Tail",
		Fn: func(ctx context.Context, _ string, _ func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			parentRunIDFromCtx = ParentRunIDFromContext(ctx)
			return nil
		},
	}

	deps := sequentialWithLearningsTailDeps{frontDef: frontDef, tailDef: tailDef}
	err := runSequentialWithLearningsTailOrchestration(context.Background(), t.TempDir(), nil, nil, deps)
	if err != nil {
		t.Fatalf("orchestration: %v", err)
	}
	if parentRunIDFromCtx == "" {
		t.Fatalf("parentRunIDFromCtx empty (ctx hand-off 失敗)")
	}
	if frontRunIDFromMetadata == "" {
		t.Fatalf("frontRunIDFromMetadata empty (前段 RunMetadata 不在)")
	}
	if frontRunIDFromMetadata != parentRunIDFromCtx {
		t.Errorf("parentRunIDFromCtx=%q, want %q (Req 1.2 同 runID 紐付け)",
			parentRunIDFromCtx, frontRunIDFromMetadata)
	}
}

// TestSequentialWithLearningsTail_DefinitionFields: 観点 (e) factory metadata。
func TestSequentialWithLearningsTail_DefinitionFields(t *testing.T) {
	base := workflow.Definition{
		ID:     "base-x",
		Label:  "Base X",
		Args:   []workflow.ArgSpec{{Key: "foo"}},
		Hidden: true,
	}
	def := SequentialWithLearningsTail("base-x-with-learnings", &fakeWorkflowAgent{}, base)
	if def.ID != "base-x-with-learnings" {
		t.Errorf("ID=%q, want base-x-with-learnings", def.ID)
	}
	if def.Label != "Base X"+learningsTailLabelMarker {
		t.Errorf("Label=%q, want %q", def.Label, "Base X"+learningsTailLabelMarker)
	}
	if len(def.Args) != 1 || def.Args[0].Key != "foo" {
		t.Errorf("Args=%v, want base.Args inheritance", def.Args)
	}
	if !def.Hidden {
		t.Errorf("Hidden=false, want true (base 継承)")
	}
}

// TestSequentialWithLearningsTail_AgentNilSafety: agent==nil でも factory 構築成功。
func TestSequentialWithLearningsTail_AgentNilSafety(t *testing.T) {
	var agent workflowctx.AgentCaller
	base := workflow.Definition{ID: "base-x", Label: "Base X"}
	def := SequentialWithLearningsTail("base-x-with-learnings", agent, base)
	if def.ID == "" {
		t.Errorf("Definition not constructed when agent==nil")
	}
}

func TestSequentialWithLearningsTail_RuntimeScopeInjectsFrontAndTailMarkers(t *testing.T) {
	t.Setenv(envRillLearningsProviders, "codex")

	runtime := &sequentialRuntimeSpy{}
	var (
		frontPolicyFound  bool
		frontAllowsCodex  bool
		frontAllowsClaude bool
		tailPolicyFound   bool
		tailEarlyEnabled  bool
		tailParentRunID   string
	)
	frontDef := workflow.Definition{
		ID:    "test-front",
		Label: "Test Front",
		Fn: func(ctx context.Context, _ string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			policy, ok := workflowctx.EarlyReflectionPolicyFromContext(ctx)
			frontPolicyFound = ok
			frontAllowsCodex = policy.AllowsProvider(provider.ProviderID("codex"))
			frontAllowsClaude = policy.AllowsProvider(provider.ProviderID("claude"))
			if policy.Notify != nil {
				policy.Notify()
			}
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "front done"})
			return nil
		},
	}
	tailDef := workflow.Definition{
		ID:    "test-tail",
		Label: "Test Tail",
		Fn: func(ctx context.Context, _ string, _ func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			_, tailPolicyFound = workflowctx.EarlyReflectionPolicyFromContext(ctx)
			tailEarlyEnabled = EarlyAggregationEnabledFromContext(ctx)
			tailParentRunID = ParentRunIDFromContext(ctx)
			return nil
		},
	}

	deps := sequentialWithLearningsTailDeps{
		agent:    &runtimeTestAgent{recorder: newRuntimeTestStore()},
		frontDef: frontDef,
		tailDef:  tailDef,
		startEarlyRuntime: func(context.Context, string, workflowctx.AgentCaller, EarlyReflectorRuntimeConfig) (workflowScopedEarlyReflectorRuntime, bool, error) {
			return runtime, true, nil
		},
	}
	if err := runSequentialWithLearningsTailOrchestration(context.Background(), t.TempDir(), nil, nil, deps); err != nil {
		t.Fatalf("orchestration: %v", err)
	}
	if !frontPolicyFound {
		t.Fatalf("front run did not receive early reflection policy")
	}
	if !frontAllowsCodex || frontAllowsClaude {
		t.Fatalf("front policy allowlist mismatch: codex=%v claude=%v", frontAllowsCodex, frontAllowsClaude)
	}
	if runtime.notifyCount != 1 {
		t.Fatalf("runtime notify count = %d, want 1", runtime.notifyCount)
	}
	if tailPolicyFound {
		t.Fatalf("tail run received early reflection enqueue marker")
	}
	if !tailEarlyEnabled {
		t.Fatalf("tail run did not receive early aggregation marker")
	}
	if tailParentRunID == "" {
		t.Fatalf("tail parent run ID is empty")
	}
	if runtime.shutdownCount != 1 {
		t.Fatalf("runtime shutdown count = %d, want 1", runtime.shutdownCount)
	}
}

func TestSequentialWithLearningsTail_RuntimeUnavailableKeepsLegacyContext(t *testing.T) {
	tests := []struct {
		name  string
		cwd   string
		agent workflowctx.AgentCaller
	}{
		{
			name:  "noop recorder",
			cwd:   t.TempDir(),
			agent: &runtimeTestAgent{recorder: akashic.NoopRecorder{}},
		},
		{
			name:  "unknown project id",
			cwd:   "/",
			agent: &runtimeTestAgent{recorder: newRuntimeTestStore()},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				frontPolicyFound bool
				tailPolicyFound  bool
				tailEarlyEnabled bool
			)
			frontDef := workflow.Definition{
				ID:    "test-front",
				Label: "Test Front",
				Fn: func(ctx context.Context, _ string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
					_, frontPolicyFound = workflowctx.EarlyReflectionPolicyFromContext(ctx)
					onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "front done"})
					return nil
				},
			}
			tailDef := workflow.Definition{
				ID:    "test-tail",
				Label: "Test Tail",
				Fn: func(ctx context.Context, _ string, _ func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
					_, tailPolicyFound = workflowctx.EarlyReflectionPolicyFromContext(ctx)
					tailEarlyEnabled = EarlyAggregationEnabledFromContext(ctx)
					return nil
				},
			}
			deps := sequentialWithLearningsTailDeps{
				agent:             tt.agent,
				frontDef:          frontDef,
				tailDef:           tailDef,
				startEarlyRuntime: defaultStartWorkflowScopedEarlyReflectorRuntime,
			}

			if err := runSequentialWithLearningsTailOrchestration(context.Background(), tt.cwd, nil, nil, deps); err != nil {
				t.Fatalf("orchestration: %v", err)
			}
			if frontPolicyFound {
				t.Fatalf("front run received early marker even though runtime startup was unavailable")
			}
			if tailPolicyFound {
				t.Fatalf("tail run received early reflection enqueue marker")
			}
			if tailEarlyEnabled {
				t.Fatalf("tail run received early aggregation marker even though runtime startup was unavailable")
			}
		})
	}
}

func TestSequentialWithLearningsTail_ShutsDownRuntimeOnExitPaths(t *testing.T) {
	frontErr := errors.New("front failed")
	tests := []struct {
		name          string
		frontDef      workflow.Definition
		tailDef       workflow.Definition
		wantErr       string
		wantTailCalls int
	}{
		{
			name: "front returned error",
			frontDef: stubLearningsDef("test-front", "Test Front", func(context.Context) error {
				return frontErr
			}, nil),
			tailDef:       stubLearningsDef("test-tail", "Test Tail", nil, nil),
			wantErr:       frontErr.Error(),
			wantTailCalls: 0,
		},
		{
			name: "front workflow error event",
			frontDef: stubLearningsDef("test-front", "Test Front", nil, []workflow.WorkflowEvent{
				{Type: workflow.WorkflowError, Message: "front workflow event failed"},
			}),
			tailDef:       stubLearningsDef("test-tail", "Test Tail", nil, nil),
			wantErr:       "front workflow event failed",
			wantTailCalls: 0,
		},
		{
			name:          "tail completed",
			frontDef:      stubLearningsDef("test-front", "Test Front", nil, []workflow.WorkflowEvent{{Type: workflow.WorkflowText, Text: "front done"}}),
			tailDef:       stubLearningsDef("test-tail", "Test Tail", nil, nil),
			wantTailCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime := &sequentialRuntimeSpy{}
			tailCalls := 0
			tailDef := tt.tailDef
			originalTailFn := tailDef.Fn
			tailDef.Fn = func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
				tailCalls++
				if originalTailFn != nil {
					return originalTailFn(ctx, cwd, onEvent, ask)
				}
				return nil
			}

			deps := sequentialWithLearningsTailDeps{
				agent:    &runtimeTestAgent{recorder: newRuntimeTestStore()},
				frontDef: tt.frontDef,
				tailDef:  tailDef,
				startEarlyRuntime: func(context.Context, string, workflowctx.AgentCaller, EarlyReflectorRuntimeConfig) (workflowScopedEarlyReflectorRuntime, bool, error) {
					return runtime, true, nil
				},
			}
			err := runSequentialWithLearningsTailOrchestration(context.Background(), t.TempDir(), nil, nil, deps)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("orchestration: %v", err)
				}
			} else if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if tailCalls != tt.wantTailCalls {
				t.Fatalf("tail calls = %d, want %d", tailCalls, tt.wantTailCalls)
			}
			if runtime.shutdownCount != 1 {
				t.Fatalf("runtime shutdown count = %d, want 1", runtime.shutdownCount)
			}
		})
	}
}

type sequentialRuntimeSpy struct {
	notifyCount   int
	shutdownCount int
}

func (s *sequentialRuntimeSpy) Notify() {
	s.notifyCount++
}

func (s *sequentialRuntimeSpy) Shutdown() {
	s.shutdownCount++
}

// TestApplyLearningsTailWraps_OnlyTargets: 観点 (f) 指定 base ID のみが wrap される。
// 不変式: in-place 置換は同 ID を維持し、Label に ` 📚` marker を suffix する
// (v1 distiller `applyDistillerTailWraps` と同形 / 旧 `<id>-with-learnings` 別 ID 方式は採らない)。
func TestApplyLearningsTailWraps_OnlyTargets(t *testing.T) {
	defs := []workflow.Definition{
		{ID: "base-A", Label: "Base A"},
		{ID: "base-B", Label: "Base B"},
		{ID: "base-C", Label: "Base C"},
	}
	target := map[string]struct{}{
		"base-A": {},
		"base-C": {},
	}
	got := ApplyLearningsTailWraps(defs, &fakeWorkflowAgent{}, target)
	if len(got) != 3 {
		t.Fatalf("len=%d, want 3", len(got))
	}
	// ID は base のまま維持される (in-place 置換契約)。
	if got[0].ID != "base-A" {
		t.Errorf("got[0].ID=%q, want base-A (in-place 同 ID 維持)", got[0].ID)
	}
	if got[1].ID != "base-B" {
		t.Errorf("got[1].ID=%q, want base-B (untouched)", got[1].ID)
	}
	if got[2].ID != "base-C" {
		t.Errorf("got[2].ID=%q, want base-C (in-place 同 ID 維持)", got[2].ID)
	}
	// 対象 def には Label marker が付き、対象外には付かない。
	if got[0].Label != "Base A"+learningsTailLabelMarker {
		t.Errorf("got[0].Label=%q, want %q", got[0].Label, "Base A"+learningsTailLabelMarker)
	}
	if got[1].Label != "Base B" {
		t.Errorf("got[1].Label=%q, want %q (対象外は untouched)", got[1].Label, "Base B")
	}
	if got[2].Label != "Base C"+learningsTailLabelMarker {
		t.Errorf("got[2].Label=%q, want %q", got[2].Label, "Base C"+learningsTailLabelMarker)
	}
}

// TestApplyLearningsTailWraps_EmptyTargets: 空 set ならば変更なし。
func TestApplyLearningsTailWraps_EmptyTargets(t *testing.T) {
	defs := []workflow.Definition{
		{ID: "base-A", Label: "Base A"},
	}
	got := ApplyLearningsTailWraps(defs, &fakeWorkflowAgent{}, nil)
	if got[0].ID != "base-A" {
		t.Errorf("got[0].ID=%q, want base-A (untouched)", got[0].ID)
	}
}
