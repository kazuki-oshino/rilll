// Package review のテスト本ファイルは runExecutor の公開契約 (tasks.md 2.4 /
// design.md §review.Executor / Req 4.1〜4.5, 5.5, 5.6, 6.1, 6.4, 6.5) を
// in-process fake AgentCaller + fake Recorder + tmp run dir で網羅的に検証する。
//
// 検証観点 (tasks.md 2.4 観察可能完了条件):
//
//   - rule 8 件 + sema 4 で同時 goroutine 数が atomic counter 計測で 4 を超えない
//   - 1 rule failed で他継続 (失敗分離)
//   - in-progress 中の cancel で `Verdict=cancelled` + 完了済保持
//   - panic recover で `Verdict=failed` + FailureCategory="executor_panic"
//   - provider 未登録で `FailureCategory="provider_unknown"`
//   - 各 rule に対し `<rule-name>.json` が run dir 配下に書き出されている
package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/agent"
	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/gh"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/result"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/rules"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// fakeRuleAgent は workflowctx.AgentCaller + AkashicRecorderProvider の test 専用 fake。
//
// behaviors map に rule.Name (= StepID) -> behavior 関数を登録することで、
// rule 単位で応答 / エラー / blocking / panic の挙動を制御する。behaviors に登録の
// ない rule にはデフォルト動作 (applied=true / findings_present=false の固定 JSON
// 応答) を返す。
type fakeRuleAgent struct {
	mu          sync.Mutex
	calls       []fakeCallRecord
	behaviors   map[string]func(ctx context.Context, opts *workflowctx.CallAgentOptions) (provider.AgentResponse, error)
	recorder    akashic.Recorder
	concurrency *atomic.Int32 // 同時 in-flight ゴルーチン数の観測
	maxConc     *atomic.Int32 // 観測された最大同時実行数
}

type fakeCallRecord struct {
	StepID   string
	Provider provider.ProviderID
	Model    string
	Prompt   string
}

func newFakeRuleAgent(recorder akashic.Recorder) *fakeRuleAgent {
	return &fakeRuleAgent{
		behaviors:   make(map[string]func(ctx context.Context, opts *workflowctx.CallAgentOptions) (provider.AgentResponse, error)),
		recorder:    recorder,
		concurrency: new(atomic.Int32),
		maxConc:     new(atomic.Int32),
	}
}

func (f *fakeRuleAgent) CallAgent(ctx context.Context, prompt string, _ string, _ provider.AgentEventCallback, _ string, options *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
	// 同時実行数を観測 (sema cap の検証用)。
	cur := f.concurrency.Add(1)
	defer f.concurrency.Add(-1)
	for {
		prev := f.maxConc.Load()
		if cur <= prev || f.maxConc.CompareAndSwap(prev, cur) {
			break
		}
	}

	stepID := ""
	var pid provider.ProviderID
	model := ""
	if options != nil {
		stepID = options.StepID
		pid = options.ProviderOverride
		model = options.ModelOverride
	}

	f.mu.Lock()
	f.calls = append(f.calls, fakeCallRecord{StepID: stepID, Provider: pid, Model: model, Prompt: prompt})
	behavior := f.behaviors[stepID]
	f.mu.Unlock()

	if behavior != nil {
		return behavior(ctx, options)
	}
	// デフォルト: applied=true / findings_present=false の構造化応答。
	return defaultAppliedResponse(), nil
}

func (f *fakeRuleAgent) AkashicRecorder() akashic.Recorder { return f.recorder }

var _ workflowctx.AgentCaller = (*fakeRuleAgent)(nil)
var _ workflowctx.AkashicRecorderProvider = (*fakeRuleAgent)(nil)

// reviewRecorder は akashic.Recorder の test 専用 fake。
//
// RecordRunArtifact / RecordRawEvent を時系列順に蓄積する。並行書き込みに耐える
// よう mu で保護する。
type reviewRecorder struct {
	mu         sync.Mutex
	artifacts  []akashic.RunArtifact
	rawEvents  []akashic.RawEvent
	startCalls int
}

func (r *reviewRecorder) RecordWorkflowRunStart(_ context.Context, _ akashic.WorkflowRun) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startCalls++
	return nil
}

func (r *reviewRecorder) RecordWorkflowRunFinish(_ context.Context, _ akashic.WorkflowRunFinish) error {
	return nil
}

func (r *reviewRecorder) RecordAgentCallStart(_ context.Context, _ akashic.AgentCall) error {
	return nil
}

func (r *reviewRecorder) RecordAgentCallFinish(_ context.Context, _ akashic.AgentCallResult) error {
	return nil
}

func (r *reviewRecorder) RecordRawEvent(_ context.Context, e akashic.RawEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rawEvents = append(r.rawEvents, e)
	return nil
}

func (r *reviewRecorder) RecordRunArtifact(_ context.Context, a akashic.RunArtifact) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.artifacts = append(r.artifacts, a)
	return nil
}

// RecordUserCorrection は memory-distiller-trigger-policy spec Task 3.3 で追加した
// Recorder interface 拡張への追従 (no-op)。review executor test は run-artifact /
// raw-event 経路の検証が責務で user-correction 経路は対象外のため履歴は保持しない。
func (r *reviewRecorder) RecordUserCorrection(context.Context, string) error {
	return nil
}

// RecordLearning は agent-learnings spec (task 3.2) で Recorder interface に追加された
// メソッドへの no-op 追従。review executor test は run-artifact / raw-event 経路の
// 検証が責務で learnings 経路は対象外のため履歴は保持しない。
func (r *reviewRecorder) RecordLearning(context.Context, akashic.LearningRow) error {
	return nil
}

func (r *reviewRecorder) Close() error { return nil }

func (r *reviewRecorder) snapshotArtifacts() []akashic.RunArtifact {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]akashic.RunArtifact, len(r.artifacts))
	copy(out, r.artifacts)
	return out
}

func (r *reviewRecorder) snapshotRawEvents() []akashic.RawEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]akashic.RawEvent, len(r.rawEvents))
	copy(out, r.rawEvents)
	return out
}

// defaultAppliedResponse は behavior 未指定 rule のデフォルト応答 (applied=true /
// findings_present=false / summary="ok")。
func defaultAppliedResponse() provider.AgentResponse {
	return provider.AgentResponse{
		Success: true,
		Content: `{"applied": true, "findings_present": false, "summary": "ok", "details": []}`,
		StructuredOutput: map[string]any{
			"applied":          true,
			"findings_present": false,
			"summary":          "ok",
			"details":          []any{},
		},
	}
}

// runExecutorWithRunMetadata は ctx に RunMetadata + Recorder を注入してから
// runExecutor を呼ぶテストヘルパ。本番経路 (Definition.Run) と等価な ctx を構築する。
func runExecutorWithRunMetadata(t *testing.T, ctx context.Context, runID, cwd string, recorder akashic.Recorder, in ExecutorInput) ExecutorOutput {
	t.Helper()
	ctx = workflowctx.ContextWithRunMetadata(ctx, workflowctx.RunMetadata{
		WorkflowRunID: runID,
		WorkflowID:    "review-workflow",
		WorkflowName:  reviewWorkflowLabel,
		CWD:           cwd,
	})
	ctx = workflow.ContextWithRecorder(ctx, recorder)
	return runExecutor(ctx, in, nil)
}

// setupRillRunDir は本番と同等のディレクトリ構造 `<tmpRoot>/.rilll/review-workflow/<runID>/` を
// 作成し、(cwd=tmpRoot, runDir=<tmpRoot>/.rilll/review-workflow/<runID>) を返すテスト fixture。
//
// 背景: workflow.RecordRunArtifact は内部で akashic.NormalizeArtifactPath を呼び、
// artifact path が `<cwd>/.rilll/` ルート配下にあることを要求する (Req 1.2, 4.5)。
// このため tmp dir をそのまま runDir に使うと artifact 登録経路が失敗する。
// 本ヘルパで本番経路 (`<cwd>/.rilll/review-workflow/<runID>/`) を再現する。
func setupRillRunDir(t *testing.T, runID string) (cwd, runDir string) {
	t.Helper()
	cwd = t.TempDir()
	runDir = filepath.Join(cwd, ".rilll", "review-workflow", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("setupRillRunDir: mkdir %s: %v", runDir, err)
	}
	return cwd, runDir
}

// makeRules は kebab-case 名 + ファイル名昇順を満たす rule slice を生成する test fixture。
func makeRules(names ...string) []rules.Rule {
	out := make([]rules.Rule, 0, len(names))
	for _, n := range names {
		out = append(out, rules.Rule{
			Name:        n,
			Description: "test rule " + n,
			Provider:    "claude",
			Model:       "claude-sonnet-4-5",
			AppliesWhen: "always",
			Prompt:      "review",
			SourceFile:  n + ".yaml",
		})
	}
	return out
}

func makePRMeta() gh.PRMeta {
	return gh.PRMeta{
		Number:    7,
		Title:     "fixture",
		Body:      "body",
		Files:     []string{"a.go"},
		Additions: 1,
		Deletions: 0,
	}
}

// TestRunExecutorSemaCapPreserved は rule 8 件 + sema=4 のとき、同時 in-flight
// goroutine 数が atomic counter 計測で 4 を超えないことを検証する (Req 4.3)。
func TestRunExecutorSemaCapPreserved(t *testing.T) {
	rs := makeRules("a-rule", "b-rule", "c-rule", "d-rule", "e-rule", "f-rule", "g-rule", "h-rule")
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)
	// 各 rule に短い sleep を入れ、並行性を可視化する。
	for _, r := range rs {
		ag.behaviors[r.Name] = func(_ context.Context, _ *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
			time.Sleep(20 * time.Millisecond)
			return defaultAppliedResponse(), nil
		}
	}
	cwd, runDir := setupRillRunDir(t, "run-sema")
	out := runExecutorWithRunMetadata(t, context.Background(), "run-sema", cwd, rec, ExecutorInput{
		Rules:       rs,
		PRMeta:      makePRMeta(),
		PRDiff:      "diff",
		MaxParallel: 4,
		RunDir:      runDir,
		Agent:       ag,
	})

	if got := ag.maxConc.Load(); got > 4 {
		t.Fatalf("max concurrency: got %d, want <=4", got)
	}
	if len(out.Results) != len(rs) {
		t.Fatalf("results length: got %d, want %d", len(out.Results), len(rs))
	}
	for i, r := range out.Results {
		if r.Verdict != result.VerdictNoFindings {
			t.Errorf("results[%d].Verdict = %q, want %q", i, r.Verdict, result.VerdictNoFindings)
		}
		if r.Name != rs[i].Name {
			t.Errorf("results[%d].Name = %q, want %q (固定 index 順序契約)", i, r.Name, rs[i].Name)
		}
	}
}

// TestRunExecutorMaxParallelDefaultsToLenWhenZero は MaxParallel<=0 のとき
// 上限が len(rules) になる契約を検証する (Req 4.3 default 値)。
func TestRunExecutorMaxParallelDefaultsToLenWhenZero(t *testing.T) {
	rs := makeRules("rule-a", "rule-b", "rule-c")
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)
	cwd, runDir := setupRillRunDir(t, "run-default")
	out := runExecutorWithRunMetadata(t, context.Background(), "run-default", cwd, rec, ExecutorInput{
		Rules:       rs,
		PRMeta:      makePRMeta(),
		MaxParallel: 0, // <=0 なら len(rules) に解決される
		RunDir:      runDir,
		Agent:       ag,
	})
	if len(out.Results) != 3 {
		t.Fatalf("results length = %d, want 3", len(out.Results))
	}
	// MaxParallel=0 なら len(rules)=3 まで同時実行可能 (上限なし)。
	if got := ag.maxConc.Load(); got > 3 {
		t.Errorf("max conc = %d, want <= 3 (= len(rules))", got)
	}
}

// TestRunExecutorFailureIsolation は 1 rule failed で他 rule は継続することを検証する (Req 4.2)。
func TestRunExecutorFailureIsolation(t *testing.T) {
	rs := makeRules("good-a", "bad-rule", "good-b")
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)
	ag.behaviors["bad-rule"] = func(_ context.Context, _ *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
		return provider.AgentResponse{}, &agent.CallError{Category: agent.FailureCategoryProviderError, Err: errors.New("boom")}
	}
	cwd, runDir := setupRillRunDir(t, "run-iso")
	out := runExecutorWithRunMetadata(t, context.Background(), "run-iso", cwd, rec, ExecutorInput{
		Rules:       rs,
		PRMeta:      makePRMeta(),
		MaxParallel: 4,
		RunDir:      runDir,
		Agent:       ag,
	})

	if len(out.Results) != 3 {
		t.Fatalf("results length = %d, want 3", len(out.Results))
	}
	if out.Results[0].Verdict != result.VerdictNoFindings {
		t.Errorf("good-a verdict = %q, want %q", out.Results[0].Verdict, result.VerdictNoFindings)
	}
	if out.Results[1].Verdict != result.VerdictFailed {
		t.Errorf("bad-rule verdict = %q, want %q", out.Results[1].Verdict, result.VerdictFailed)
	}
	if out.Results[1].FailureCategory != agent.FailureCategoryProviderError {
		t.Errorf("bad-rule FailureCategory = %q, want %q", out.Results[1].FailureCategory, agent.FailureCategoryProviderError)
	}
	if out.Results[2].Verdict != result.VerdictNoFindings {
		t.Errorf("good-b verdict = %q, want %q", out.Results[2].Verdict, result.VerdictNoFindings)
	}
	// 各 rule の <name>.json が runDir 配下に書き出されていること。
	for _, r := range rs {
		path := filepath.Join(runDir, r.Name+".json")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected artifact %s exists: %v", path, err)
		}
	}
}

// TestRunExecutorProviderUnknownClassification は agent.ErrUnknownProvider を
// FailureCategory=provider_unknown に分類することを検証する (Req 5.6)。
func TestRunExecutorProviderUnknownClassification(t *testing.T) {
	rs := makeRules("ghost-rule")
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)
	ag.behaviors["ghost-rule"] = func(_ context.Context, _ *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
		wrapped := fmt.Errorf("%w: %q", agent.ErrUnknownProvider, "ghost")
		return provider.AgentResponse{}, &agent.CallError{Category: agent.FailureCategoryProviderUnknown, Err: wrapped}
	}
	cwd, runDir := setupRillRunDir(t, "run-unknown")
	out := runExecutorWithRunMetadata(t, context.Background(), "run-unknown", cwd, rec, ExecutorInput{
		Rules:       rs,
		PRMeta:      makePRMeta(),
		MaxParallel: 1,
		RunDir:      runDir,
		Agent:       ag,
	})
	if out.Results[0].Verdict != result.VerdictFailed {
		t.Errorf("verdict = %q, want %q", out.Results[0].Verdict, result.VerdictFailed)
	}
	if out.Results[0].FailureCategory != agent.FailureCategoryProviderUnknown {
		t.Errorf("FailureCategory = %q, want %q", out.Results[0].FailureCategory, agent.FailureCategoryProviderUnknown)
	}
}

// TestRunExecutorPanicRecovery は agent 呼び出し経路で panic が起きた場合に
// Verdict=failed + FailureCategory="executor_panic" に丸めることを検証する (Req 4.2)。
func TestRunExecutorPanicRecovery(t *testing.T) {
	rs := makeRules("boom-rule", "ok-rule")
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)
	ag.behaviors["boom-rule"] = func(_ context.Context, _ *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
		panic("intentional panic for executor recover test")
	}
	cwd, runDir := setupRillRunDir(t, "run-panic")
	out := runExecutorWithRunMetadata(t, context.Background(), "run-panic", cwd, rec, ExecutorInput{
		Rules:       rs,
		PRMeta:      makePRMeta(),
		MaxParallel: 2,
		RunDir:      runDir,
		Agent:       ag,
	})
	if out.Results[0].Verdict != result.VerdictFailed {
		t.Fatalf("boom-rule verdict = %q, want %q", out.Results[0].Verdict, result.VerdictFailed)
	}
	if out.Results[0].FailureCategory != FailureCategoryExecutorPanic {
		t.Errorf("boom-rule FailureCategory = %q, want %q", out.Results[0].FailureCategory, FailureCategoryExecutorPanic)
	}
	if out.Results[1].Verdict != result.VerdictNoFindings {
		t.Errorf("ok-rule verdict = %q, want %q (panic 分離が壊れている)", out.Results[1].Verdict, result.VerdictNoFindings)
	}
}

// TestRunExecutorParseFailureClassification は agent 応答が JSON schema にも
// regex fallback にも適合しない場合に Verdict=failed + FailureCategory=
// "response_unparseable" に分類することを検証する (Req 5.4)。
func TestRunExecutorParseFailureClassification(t *testing.T) {
	rs := makeRules("garbage-rule")
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)
	ag.behaviors["garbage-rule"] = func(_ context.Context, _ *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
		return provider.AgentResponse{Success: true, Content: "no json here at all"}, nil
	}
	cwd, runDir := setupRillRunDir(t, "run-unparse")
	out := runExecutorWithRunMetadata(t, context.Background(), "run-unparse", cwd, rec, ExecutorInput{
		Rules:       rs,
		PRMeta:      makePRMeta(),
		MaxParallel: 1,
		RunDir:      runDir,
		Agent:       ag,
	})
	if out.Results[0].Verdict != result.VerdictFailed {
		t.Errorf("verdict = %q, want %q", out.Results[0].Verdict, result.VerdictFailed)
	}
	if out.Results[0].FailureCategory != FailureCategoryResponseUnparseable {
		t.Errorf("FailureCategory = %q, want %q", out.Results[0].FailureCategory, FailureCategoryResponseUnparseable)
	}
}

// TestRunExecutorCancelPropagation は in-progress 中の ctx cancel で
// Verdict=cancelled + 完了済 rule の RuleResult が破棄されないことを検証する (Req 4.5)。
func TestRunExecutorCancelPropagation(t *testing.T) {
	rs := makeRules("a-quick", "b-block", "c-block", "d-block")
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)

	// quickStarted で a-quick が確実に完了したことをテスト側に通知する。
	quickDone := make(chan struct{})
	ag.behaviors["a-quick"] = func(_ context.Context, _ *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
		defer close(quickDone)
		return defaultAppliedResponse(), nil
	}
	// b/c/d は ctx.Done() まで block する (ctx cancel で context.Canceled を返す)。
	blocking := func(ctx context.Context, _ *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
		<-ctx.Done()
		return provider.AgentResponse{}, ctx.Err()
	}
	ag.behaviors["b-block"] = blocking
	ag.behaviors["c-block"] = blocking
	ag.behaviors["d-block"] = blocking

	cwd, runDir := setupRillRunDir(t, "run-cancel")
	ctx, cancel := context.WithCancel(context.Background())

	type runOut struct {
		out ExecutorOutput
	}
	done := make(chan runOut, 1)
	go func() {
		out := runExecutorWithRunMetadata(t, ctx, "run-cancel", cwd, rec, ExecutorInput{
			Rules:       rs,
			PRMeta:      makePRMeta(),
			PRDiff:      "diff",
			MaxParallel: 4, // 全 rule が同時起動できる
			RunDir:      runDir,
			Agent:       ag,
		})
		done <- runOut{out: out}
	}()

	// a-quick が完了するまで待つ → 完了済 rule が cancel 後にも保持されるかを検証可能にする。
	select {
	case <-quickDone:
	case <-time.After(2 * time.Second):
		t.Fatal("a-quick did not complete within timeout")
	}
	cancel()

	var got runOut
	select {
	case got = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runExecutor did not return after cancel")
	}

	if len(got.out.Results) != 4 {
		t.Fatalf("results length = %d, want 4", len(got.out.Results))
	}
	// a-quick は完了済の verdict (no_findings) が保持される
	if got.out.Results[0].Verdict != result.VerdictNoFindings {
		t.Errorf("a-quick verdict = %q, want %q (完了済保持契約)", got.out.Results[0].Verdict, result.VerdictNoFindings)
	}
	// 残り 3 つは cancelled
	for _, idx := range []int{1, 2, 3} {
		if got.out.Results[idx].Verdict != result.VerdictCancelled {
			t.Errorf("results[%d] (%s) verdict = %q, want %q",
				idx, rs[idx].Name, got.out.Results[idx].Verdict, result.VerdictCancelled)
		}
	}
}

// TestRunExecutorWritesPerRuleArtifact は各 rule に対し `<rule-name>.json` が
// run dir 配下に書き出され、RecordRunArtifact (kind=review) が登録されることを検証する
// (Req 6.1, 6.4)。
func TestRunExecutorWritesPerRuleArtifact(t *testing.T) {
	rs := makeRules("alpha", "bravo")
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)
	cwd, runDir := setupRillRunDir(t, "run-arti")
	out := runExecutorWithRunMetadata(t, context.Background(), "run-arti", cwd, rec, ExecutorInput{
		Rules:       rs,
		PRMeta:      makePRMeta(),
		MaxParallel: 2,
		RunDir:      runDir,
		Agent:       ag,
	})

	// ファイル存在 + JSON parse OK + Name フィールドが一致
	for i, r := range rs {
		path := filepath.Join(runDir, r.Name+".json")
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var parsed result.RuleResult
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("unmarshal %s: %v", path, err)
		}
		if parsed.Name != r.Name {
			t.Errorf("artifact[%d].Name = %q, want %q", i, parsed.Name, r.Name)
		}
		if parsed.Provider != r.Provider {
			t.Errorf("artifact[%d].Provider = %q, want %q", i, parsed.Provider, r.Provider)
		}
		if parsed.Model != r.Model {
			t.Errorf("artifact[%d].Model = %q, want %q", i, parsed.Model, r.Model)
		}
		if parsed.StartedAt == "" || parsed.FinishedAt == "" {
			t.Errorf("artifact[%d] missing timestamps: %+v", i, parsed)
		}
		if parsed.SourceFile != r.SourceFile {
			t.Errorf("artifact[%d].SourceFile = %q, want %q", i, parsed.SourceFile, r.SourceFile)
		}
	}
	// RecordRunArtifact が rule 件数だけ呼ばれている
	arts := rec.snapshotArtifacts()
	if len(arts) != len(rs) {
		t.Errorf("RecordRunArtifact count = %d, want %d", len(arts), len(rs))
	}
	for _, a := range arts {
		if a.Kind != akashic.ArtifactKindReview {
			t.Errorf("artifact kind = %q, want %q", a.Kind, akashic.ArtifactKindReview)
		}
	}

	// 同時に Results にも反映されていること
	for i, r := range rs {
		if out.Results[i].Name != r.Name {
			t.Errorf("Results[%d].Name = %q, want %q", i, out.Results[i].Name, r.Name)
		}
	}
}

// TestRunExecutorEmitsRuleModelSelectedRawEvent は成功経路で
// `rule_model_selected` raw_event が rule.Name を Subtype として 1 件発行されることを検証する (Req 5.5)。
func TestRunExecutorEmitsRuleModelSelectedRawEvent(t *testing.T) {
	rs := makeRules("model-test")
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)
	cwd, runDir := setupRillRunDir(t, "run-mod")
	_ = runExecutorWithRunMetadata(t, context.Background(), "run-mod", cwd, rec, ExecutorInput{
		Rules:       rs,
		PRMeta:      makePRMeta(),
		MaxParallel: 1,
		RunDir:      runDir,
		Agent:       ag,
	})
	events := rec.snapshotRawEvents()
	var found bool
	for _, e := range events {
		if e.Type == RawEventTypeRuleModelSelected && e.Subtype == "model-test" {
			found = true
			// payload に provider と model が含まれていること
			var payload map[string]string
			if err := json.Unmarshal(e.PayloadJSON, &payload); err != nil {
				t.Fatalf("payload unmarshal: %v", err)
			}
			if payload["provider"] != "claude" {
				t.Errorf("payload.provider = %q, want %q", payload["provider"], "claude")
			}
			if payload["model"] != "claude-sonnet-4-5" {
				t.Errorf("payload.model = %q, want %q", payload["model"], "claude-sonnet-4-5")
			}
		}
	}
	if !found {
		t.Fatalf("rule_model_selected raw_event not found in %d events", len(events))
	}
}

// TestRunExecutorEmitsExecutorRuleFailedRawEvent は失敗経路で
// `executor_rule_failed` raw_event が 1 件発行されることを検証する (Req 4.4)。
func TestRunExecutorEmitsExecutorRuleFailedRawEvent(t *testing.T) {
	rs := makeRules("fail-rule")
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)
	ag.behaviors["fail-rule"] = func(_ context.Context, _ *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
		return provider.AgentResponse{}, &agent.CallError{Category: agent.FailureCategoryProviderError, Err: errors.New("nope")}
	}
	cwd, runDir := setupRillRunDir(t, "run-fail")
	_ = runExecutorWithRunMetadata(t, context.Background(), "run-fail", cwd, rec, ExecutorInput{
		Rules:       rs,
		PRMeta:      makePRMeta(),
		MaxParallel: 1,
		RunDir:      runDir,
		Agent:       ag,
	})
	events := rec.snapshotRawEvents()
	var found bool
	for _, e := range events {
		if e.Type == RawEventTypeExecutorRuleFailed && e.Subtype == "fail-rule" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("executor_rule_failed raw_event not found in %d events", len(events))
	}
}

// TestRunExecutorEmptyRules は rule 0 件のとき Results が空 + panic しないことを検証する。
func TestRunExecutorEmptyRules(t *testing.T) {
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)
	cwd, runDir := setupRillRunDir(t, "run-empty")
	out := runExecutorWithRunMetadata(t, context.Background(), "run-empty", cwd, rec, ExecutorInput{
		Rules:       nil,
		MaxParallel: 4,
		RunDir:      runDir,
		Agent:       ag,
	})
	if len(out.Results) != 0 {
		t.Errorf("results length = %d, want 0", len(out.Results))
	}
}

// TestRunExecutorPropagatesProviderOverride は CallAgent が rule.Provider /
// rule.Model を CallAgentOptions の ProviderOverride / ModelOverride で受け取ることを検証する (Req 5.5)。
func TestRunExecutorPropagatesProviderOverride(t *testing.T) {
	rs := []rules.Rule{
		{Name: "p-claude", Description: "d", Provider: "claude", Model: "model-a", AppliesWhen: "x", Prompt: "y", SourceFile: "p-claude.yaml"},
		{Name: "p-codex", Description: "d", Provider: "codex", Model: "model-b", AppliesWhen: "x", Prompt: "y", SourceFile: "p-codex.yaml"},
	}
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)
	cwd, runDir := setupRillRunDir(t, "run-over")
	_ = runExecutorWithRunMetadata(t, context.Background(), "run-over", cwd, rec, ExecutorInput{
		Rules:       rs,
		PRMeta:      makePRMeta(),
		MaxParallel: 2,
		RunDir:      runDir,
		Agent:       ag,
	})
	ag.mu.Lock()
	defer ag.mu.Unlock()
	gotByStep := map[string]fakeCallRecord{}
	for _, c := range ag.calls {
		gotByStep[c.StepID] = c
	}
	if c := gotByStep["p-claude"]; c.Provider != provider.ProviderID("claude") || c.Model != "model-a" {
		t.Errorf("p-claude provider/model = %q/%q, want claude/model-a", c.Provider, c.Model)
	}
	if c := gotByStep["p-codex"]; c.Provider != provider.ProviderID("codex") || c.Model != "model-b" {
		t.Errorf("p-codex provider/model = %q/%q, want codex/model-b", c.Provider, c.Model)
	}
}

// --- output-contract-2phase Task 7.2: review-workflow PoC integration test ---
//
// 設計規約 (output-contract-2phase design.md §workflow/builtin/review Layer / Req 9.1〜9.4):
//   - runOneRule は Phase 1 で `<RunDir>/<rule>.json` を書き出し (既存)、
//     Phase 1 成功後に agent 層の runReportPhase が Phase 2 で `<RunDir>/<rule>.md` を
//     書き出す (新規) — 同一 RunDir に JSON / Markdown report が共存する。
//   - Phase 2 で provider は AllowedTools=[Write] / ResumeSessionID=<phase1> / MaxTurns=2 /
//     Metadata.StepID = "<orig>:phase2:<contract>" の AgentRequest を受け取る。
//   - Mode は provider 静的マッピング (claude/codex=native, cursor 系=fallback) で確定する。
//
// 既存 executor_test.go は workflowctx.AgentCaller の test 専用 fake (fakeRuleAgent) を
// 使用し、Phase 2 (runReportPhase) は exercise されない。本 test は real *agent.Agent +
// fake provider 経路で runOneRule を駆動し、Phase 2 まで実装され実際に file 副作用と
// OutputContractResults が返ることを integration レベルで検証する。

// scriptedReviewProvider は task 7.2 PoC integration test 用の fake provider。
// 各 Call() ごとに事前スクリプト (scripts[]) の i 番目を返し、req は calls[] に蓄積する。
// scripts の本数を超える呼び出しは即時 error を返す (= 過剰呼び出しを構造的に検知)。
//
// review-workflow PoC では 1 rule あたり 2 回呼ばれる想定:
//   - call#0: Phase 1 — review JSON schema 応答
//   - call#1: Phase 2 — review-gather 雛形に沿った Markdown body
type scriptedReviewProvider struct {
	id      provider.ProviderID
	mu      sync.Mutex
	calls   []provider.AgentRequest
	scripts []provider.AgentResponse
}

func (p *scriptedReviewProvider) ID() provider.ProviderID { return p.id }

func (p *scriptedReviewProvider) Call(_ context.Context, req provider.AgentRequest, _ provider.AgentEventCallback) (provider.AgentResponse, error) {
	p.mu.Lock()
	idx := len(p.calls)
	p.calls = append(p.calls, req)
	p.mu.Unlock()
	if idx >= len(p.scripts) {
		return provider.AgentResponse{}, fmt.Errorf("scriptedReviewProvider: unexpected call index=%d (scripts=%d)", idx, len(p.scripts))
	}
	return p.scripts[idx], nil
}

// captureAgentCaller は real *agent.Agent を workflowctx.AgentCaller として delegate しつつ
// provider.AgentResponse を per-call capture する test wrapper。
//
// 動機: runOneRule は AgentResponse.OutputContractResults を RuleResult に転写しないため、
// Phase 2 結果 (Status / ReportPath / Mode) を test 側で assert するには delegate 経由で
// 応答を capture する必要がある。AkashicRecorder() は inner agent の recorder をそのまま
// 公開することで、workflow.RecorderFromAgent 経路 (raw_event 発行) も real agent と等価に動く。
type captureAgentCaller struct {
	delegate *agent.Agent
	mu       sync.Mutex
	captured []provider.AgentResponse
}

func (c *captureAgentCaller) CallAgent(ctx context.Context, prompt string, cwd string, cb provider.AgentEventCallback, resumeSessionID string, options *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
	resp, err := c.delegate.CallAgent(ctx, prompt, cwd, cb, resumeSessionID, options)
	c.mu.Lock()
	c.captured = append(c.captured, resp)
	c.mu.Unlock()
	return resp, err
}

func (c *captureAgentCaller) AkashicRecorder() akashic.Recorder {
	return c.delegate.AkashicRecorder()
}

var _ workflowctx.AgentCaller = (*captureAgentCaller)(nil)
var _ workflowctx.AkashicRecorderProvider = (*captureAgentCaller)(nil)

// TestRunExecutorPhase2WritesMarkdownReportAlongsideJSON は task 7.2 の PoC integration test。
// real *agent.Agent + fake provider 経由で runOneRule を駆動し、Phase 1 で `<rule>.json`
// + Phase 2 で `<rule>.md` が同一 RunDir に共存することを検証する (R9.2 / R9.3 / R9.4)。
//
// assertion 観点:
//   - <RunDir>/<rule>.json (writeRuleArtifact 経路 / Phase 1) が存在する
//   - <RunDir>/<rule>.md  (writeReportFile 経路 / Phase 2)  が存在し、本文が fake provider の
//     Phase 2 Content と一致する (writeReportFile は body を改変しない契約)
//   - captured response.OutputContractResults[0]:
//   - Status="success"、Mode="native" (provider=claude → 静的マッピング)
//   - ReportPath が `.rilll/review-workflow/<runID>/<rule>.md` の絶対パス
//   - provider.Call が 2 回呼ばれ、call#1 (Phase 2) で AllowedTools=[Write] /
//     ResumeSessionID=<Phase 1 SessionID> / Metadata.StepID="<rule>:phase2:<rule>" が設定される
//   - Phase 1 既存の RuleResult.Verdict 決定ロジック (= no_findings) は Phase 2 追加で変化しない
func TestRunExecutorPhase2WritesMarkdownReportAlongsideJSON(t *testing.T) {
	runID := "run-poc-7-2"
	rs := makeRules("poc-rule")
	phase1SessionID := "phase1-session-poc"
	phase2Body := "# Review Gather (poc)\n\n## Findings\n\n- none\n"
	sp := &scriptedReviewProvider{
		id: provider.ProviderClaude,
		scripts: []provider.AgentResponse{
			// call#0 = Phase 1 (review JSON schema 応答 / applied=true・findings_present=false)
			{
				Success:   true,
				SessionID: phase1SessionID,
				Content:   `{"applied": true, "findings_present": false, "summary": "ok", "details": []}`,
				StructuredOutput: map[string]any{
					"applied":          true,
					"findings_present": false,
					"summary":          "ok",
					"details":          []any{},
				},
			},
			// call#1 = Phase 2 (review-gather 雛形に沿った Markdown body)
			{
				Success:   true,
				SessionID: "phase2-session-poc",
				Content:   phase2Body,
			},
		},
	}
	rec := &reviewRecorder{}
	realAgent := agent.New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude: sp,
	}, rec)
	wrapped := &captureAgentCaller{delegate: realAgent}

	cwd, runDir := setupRillRunDir(t, runID)
	out := runExecutorWithRunMetadata(t, context.Background(), runID, cwd, rec, ExecutorInput{
		Rules:       rs,
		PRMeta:      makePRMeta(),
		PRDiff:      "diff",
		MaxParallel: 1,
		RunDir:      runDir,
		Agent:       wrapped,
	})

	// --- (1) Phase 1 既存挙動が壊れていないこと: Verdict / artifact ---
	if len(out.Results) != 1 {
		t.Fatalf("results length = %d, want 1", len(out.Results))
	}
	if out.Results[0].Verdict != result.VerdictNoFindings {
		t.Fatalf("poc-rule Verdict = %q, want %q (Phase 2 追加で Verdict 決定ロジックが変化してはならない)",
			out.Results[0].Verdict, result.VerdictNoFindings)
	}
	jsonPath := filepath.Join(runDir, "poc-rule.json")
	if _, err := os.Stat(jsonPath); err != nil {
		t.Fatalf("Phase 1 artifact %s 不在: %v", jsonPath, err)
	}

	// --- (2) Phase 2 report が同 RunDir に共存し、本文が改変されていないこと ---
	mdPath := filepath.Join(runDir, "poc-rule.md")
	if _, err := os.Stat(mdPath); err != nil {
		t.Fatalf("Phase 2 report %s 不在 (Phase 2 が exercise されていない可能性): %v", mdPath, err)
	}
	gotBody, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatalf("Phase 2 report read 失敗: %v", err)
	}
	if string(gotBody) != phase2Body {
		t.Fatalf("Phase 2 report body mismatch:\n got=%q\nwant=%q", string(gotBody), phase2Body)
	}

	// --- (3) captured response.OutputContractResults を assert ---
	if len(wrapped.captured) != 1 {
		t.Fatalf("captured response 数 = %d, want 1 (runOneRule は CallAgent を 1 回しか呼ばない)", len(wrapped.captured))
	}
	captured := wrapped.captured[0]
	if len(captured.OutputContractResults) != 1 {
		t.Fatalf("OutputContractResults 件数 = %d, want 1", len(captured.OutputContractResults))
	}
	r := captured.OutputContractResults[0]
	if r.Name != "poc-rule" {
		t.Errorf("OutputContractResults[0].Name = %q, want poc-rule", r.Name)
	}
	if r.Status != "success" {
		t.Fatalf("OutputContractResults[0].Status = %q, want success (warning=%q)", r.Status, r.Warning)
	}
	if r.Mode != "native" {
		t.Errorf("OutputContractResults[0].Mode = %q, want native (provider=claude → 静的マッピング)", r.Mode)
	}
	if r.ReportPath == "" || !filepath.IsAbs(r.ReportPath) {
		t.Fatalf("OutputContractResults[0].ReportPath は絶対パスである必要がある (got %q)", r.ReportPath)
	}
	wantAbs, err := filepath.Abs(mdPath)
	if err != nil {
		t.Fatalf("filepath.Abs(%q) 失敗: %v", mdPath, err)
	}
	if r.ReportPath != wantAbs {
		t.Errorf("OutputContractResults[0].ReportPath = %q, want %q (.rilll/review-workflow/<runID>/<rule>.md)", r.ReportPath, wantAbs)
	}
	if r.RetryUsed {
		t.Errorf("OutputContractResults[0].RetryUsed = true (PoC 経路では retry なしで成功する想定)")
	}

	// --- (4) provider.Call の呼ばれ方を assert: Phase 1 + Phase 2 = 2 回 ---
	if len(sp.calls) != 2 {
		t.Fatalf("provider.Call 呼び出し回数 = %d (期待 2 = Phase 1 + Phase 2)", len(sp.calls))
	}
	phase1Req := sp.calls[0]
	if phase1Req.ResumeSessionID != "" {
		t.Errorf("Phase 1 ResumeSessionID = %q, want \"\" (Phase 1 は新規 session)", phase1Req.ResumeSessionID)
	}
	if len(phase1Req.AllowedTools) != 0 {
		t.Errorf("Phase 1 AllowedTools = %#v, want empty (Phase 1 はフルツール)", phase1Req.AllowedTools)
	}

	phase2Req := sp.calls[1]
	if phase2Req.ResumeSessionID != phase1SessionID {
		t.Errorf("Phase 2 ResumeSessionID = %q, want %q (Phase 1 SessionID を継続)", phase2Req.ResumeSessionID, phase1SessionID)
	}
	if len(phase2Req.AllowedTools) != 1 || phase2Req.AllowedTools[0] != "Write" {
		t.Errorf("Phase 2 AllowedTools = %#v, want []string{\"Write\"}", phase2Req.AllowedTools)
	}
	if phase2Req.MaxTurns != 2 {
		t.Errorf("Phase 2 MaxTurns = %d, want 2", phase2Req.MaxTurns)
	}
	wantStepID := "poc-rule:phase2:poc-rule"
	if phase2Req.Metadata.StepID != wantStepID {
		t.Errorf("Phase 2 Metadata.StepID = %q, want %q (phase2StepID 規約: <orig>:phase2:<contract>)",
			phase2Req.Metadata.StepID, wantStepID)
	}
}
