// Package learnings の本 file は agent-learnings reflector phase の
// 単体テスト群を保持する (tasks.md 7.2 / Req 4.1〜4.7, 5.4, 9.2, 9.4, 9.5)。
//
// 検証観点:
//
//   - (a) sessions 0 件 → 空 artifact + AcceptedCount=0 / AllFailed=false (early return)。
//   - (b) per-session 成功 → learnings table に accept された learning を insert し
//     reflector.json `per_session[]` に accepted_count を反映する (Req 4.1, 4.4, 5.4)。
//   - (c) category enum 違反 learning は drop し `dropped[]` に trace される
//     ことで Req 9.5 の経路を観測する。
//   - (d) per-session call 失敗は他 session を止めず best-effort 続行し
//     `per_session_failures[]` に記録される (Req 4.5, 9.4)。
//   - (e) 全 session 失敗 → `AllFailed=true` を返す (Req 4.6)。
//   - (f) ErrNoCredentials 相当 (heuristic 文字列) → `NoCredentials=true` を返す
//     (Req 9.2)。
//   - (g) 空 learnings 配列レスポンス → insert 0 件で `AllFailed=true` 経路にせず
//     `accept=0` のみ (Req 5.4)。
//   - (h) allowlist 外 provider session は `skipped_provider[]` に trace され
//     LLM call が発火しない。
//   - (i) ParallelLimiter の上限を超える同時 in-flight が発生しない。
package learnings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// fakeReflectorAgent は workflowctx.AgentCaller を満たす test stub。
// CallAgent は per-session で `responses[sessionID]` を返し、key 不在は errors[sessionID]
// を返す。並列度観測のため、pre-call で peakInflight を増減させて max を計測する。
type fakeReflectorAgent struct {
	mu          sync.Mutex
	responses   map[string]provider.AgentResponse
	errors      map[string]error
	calls       []string // 呼び出された session_id 順
	inflight    int32
	peak        int32
	delay       time.Duration
	checkOpts   func(opts *workflowctx.CallAgentOptions)
	gotPrompts  []string
	gotResumeID []string
}

func (f *fakeReflectorAgent) CallAgent(ctx context.Context, prompt, cwd string, onEvent provider.AgentEventCallback, resumeSessionID string, options *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
	cur := atomic.AddInt32(&f.inflight, 1)
	for {
		old := atomic.LoadInt32(&f.peak)
		if cur <= old || atomic.CompareAndSwapInt32(&f.peak, old, cur) {
			break
		}
	}
	defer atomic.AddInt32(&f.inflight, -1)

	f.mu.Lock()
	f.calls = append(f.calls, resumeSessionID)
	f.gotPrompts = append(f.gotPrompts, prompt)
	f.gotResumeID = append(f.gotResumeID, resumeSessionID)
	f.mu.Unlock()

	if f.checkOpts != nil {
		f.checkOpts(options)
	}

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return provider.AgentResponse{}, ctx.Err()
		}
	}

	if err, ok := f.errors[resumeSessionID]; ok && err != nil {
		return provider.AgentResponse{}, err
	}
	if resp, ok := f.responses[resumeSessionID]; ok {
		return resp, nil
	}
	return provider.AgentResponse{Content: `{"learnings":[]}`}, nil
}

// fakeReflectorRecorder は akashic.Recorder を満たす test stub。
// RecordLearning は内部 slice に row を蓄え、category enum 違反は ErrInvalidLearningCategory を返す。
type fakeReflectorRecorder struct {
	mu          sync.Mutex
	rows        []akashic.LearningRow
	failOnIndex int // -1 なら fail しない、>= 0 なら N 番目の RecordLearning で error を返す
	failErr     error
}

func newFakeRecorder() *fakeReflectorRecorder {
	return &fakeReflectorRecorder{failOnIndex: -1}
}

// 既存 Recorder メソッドは reflector phase で呼ばれないため zero / nil を返す。
func (r *fakeReflectorRecorder) RecordWorkflowRunStart(_ context.Context, _ akashic.WorkflowRun) error {
	return nil
}
func (r *fakeReflectorRecorder) RecordWorkflowRunFinish(_ context.Context, _ akashic.WorkflowRunFinish) error {
	return nil
}
func (r *fakeReflectorRecorder) RecordAgentCallStart(_ context.Context, _ akashic.AgentCall) error {
	return nil
}
func (r *fakeReflectorRecorder) RecordAgentCallFinish(_ context.Context, _ akashic.AgentCallResult) error {
	return nil
}
func (r *fakeReflectorRecorder) RecordRawEvent(_ context.Context, _ akashic.RawEvent) error {
	return nil
}
func (r *fakeReflectorRecorder) RecordRunArtifact(_ context.Context, _ akashic.RunArtifact) error {
	return nil
}
func (r *fakeReflectorRecorder) RecordUserCorrection(_ context.Context, _ string) error {
	return nil
}
func (r *fakeReflectorRecorder) Close() error { return nil }

func (r *fakeReflectorRecorder) RecordLearning(_ context.Context, row akashic.LearningRow) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !akashic.ValidLearningCategory(row.Category) {
		return akashic.ErrInvalidLearningCategory
	}
	if r.failOnIndex >= 0 && len(r.rows) == r.failOnIndex {
		// 1 度だけ失敗させる
		err := r.failErr
		if err == nil {
			err = errors.New("simulated record-learning failure")
		}
		r.failOnIndex = -1
		return err
	}
	r.rows = append(r.rows, row)
	return nil
}

// readReflectorArtifactT は test 専用 helper。reflector.json を decode して assertion に渡す。
func readReflectorArtifactT(t *testing.T, path string) ReflectorArtifact {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read reflector.json: %v", err)
	}
	var a ReflectorArtifact
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatalf("unmarshal reflector.json: %v\nbody = %s", err, body)
	}
	return a
}

// withProviderEnv は env 上書きで allowlist を test 内だけ制御するヘルパ。
func withProviderEnv(t *testing.T, value string) {
	t.Helper()
	prev, hadPrev := os.LookupEnv(envRillLearningsProviders)
	if err := os.Setenv(envRillLearningsProviders, value); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	t.Cleanup(func() {
		if hadPrev {
			_ = os.Setenv(envRillLearningsProviders, prev)
		} else {
			_ = os.Unsetenv(envRillLearningsProviders)
		}
	})
}

// TestReflectorEmptySessions: 観点 (a) sessions 0 件 → 空 artifact + AcceptedCount=0
// AllFailed=false で early return する。
func TestReflectorEmptySessions(t *testing.T) {
	withProviderEnv(t, "codex")
	runDir := t.TempDir()

	got, err := Reflector(context.Background(), runDir, ReflectorInput{
		ParentRunID: "20260508T010203Z-parent01",
		Sessions:    nil,
	}, &fakeReflectorAgent{}, newFakeRecorder(), NewLimiter(envRillLearningsMaxParallel, defaultMaxParallel))
	if err != nil {
		t.Fatalf("Reflector: %v", err)
	}
	if got.AcceptedCount != 0 {
		t.Errorf("AcceptedCount = %d, want 0", got.AcceptedCount)
	}
	if got.AllFailed {
		t.Errorf("AllFailed = true, want false (no sessions = no failure)")
	}
	a := readReflectorArtifactT(t, got.ReflectorJSONPath)
	if a.SchemaVersion != reflectorSchemaVersion {
		t.Errorf("schema_version = %d, want %d", a.SchemaVersion, reflectorSchemaVersion)
	}
	if len(a.PerSession) != 0 {
		t.Errorf("PerSession len = %d, want 0", len(a.PerSession))
	}
}

// TestReflectorAcceptsValidLearnings: 観点 (b) 成功 session → learnings table に insert
// + reflector.json に accepted_count 反映。
func TestReflectorAcceptsValidLearnings(t *testing.T) {
	withProviderEnv(t, "codex")
	runDir := t.TempDir()

	agent := &fakeReflectorAgent{
		responses: map[string]provider.AgentResponse{
			"sess-A": {Content: `{"learnings":[{"category":"failure-recovery","context":"ctx","insight":"ins","paths_glob":["a/**"]}]}`},
		},
	}
	rec := newFakeRecorder()
	got, err := Reflector(context.Background(), runDir, ReflectorInput{
		ParentRunID: "run-1",
		WorkflowID:  "wf-id",
		Sessions: []SessionEntry{
			{SessionID: "sess-A", AgentCallIDs: []string{"ac-1"}, ProviderID: "codex", StepID: "implement"},
		},
	}, agent, rec, NewLimiter(envRillLearningsMaxParallel, defaultMaxParallel))
	if err != nil {
		t.Fatalf("Reflector: %v", err)
	}
	if got.AcceptedCount != 1 {
		t.Fatalf("AcceptedCount = %d, want 1", got.AcceptedCount)
	}
	if got.AllFailed {
		t.Errorf("AllFailed = true, want false")
	}
	if len(rec.rows) != 1 {
		t.Fatalf("recorded rows = %d, want 1", len(rec.rows))
	}
	r := rec.rows[0]
	if r.Category != "failure-recovery" {
		t.Errorf("Category = %q, want failure-recovery", r.Category)
	}
	if r.WorkflowRunID != "run-1" {
		t.Errorf("WorkflowRunID = %q, want run-1", r.WorkflowRunID)
	}
	if r.WorkflowID != "wf-id" {
		t.Errorf("WorkflowID = %q, want wf-id", r.WorkflowID)
	}
	if r.StepID != "implement" {
		t.Errorf("StepID = %q, want implement", r.StepID)
	}
	if r.AgentCallID != "ac-1" {
		t.Errorf("AgentCallID = %q, want ac-1", r.AgentCallID)
	}
	if r.Source != akashic.SourceAgentSelfReport {
		t.Errorf("Source = %q, want %q", r.Source, akashic.SourceAgentSelfReport)
	}
	if len(r.PathsGlob) != 1 || r.PathsGlob[0] != "a/**" {
		t.Errorf("PathsGlob = %v, want [a/**]", r.PathsGlob)
	}

	a := readReflectorArtifactT(t, got.ReflectorJSONPath)
	if len(a.PerSession) != 1 {
		t.Fatalf("PerSession len = %d, want 1", len(a.PerSession))
	}
	if a.PerSession[0].AcceptedCount != 1 {
		t.Errorf("PerSession[0].AcceptedCount = %d, want 1", a.PerSession[0].AcceptedCount)
	}
}

// TestReflectorDropsInvalidCategory: 観点 (c) category enum 違反 → drop + dropped[] trace。
func TestReflectorDropsInvalidCategory(t *testing.T) {
	withProviderEnv(t, "codex")
	runDir := t.TempDir()

	agent := &fakeReflectorAgent{
		responses: map[string]provider.AgentResponse{
			"sess-A": {Content: `{"learnings":[{"category":"unknown-cat","context":"c","insight":"i","paths_glob":["a/**"]},{"category":"antipattern","context":"c2","insight":"i2","paths_glob":["b/**"]}]}`},
		},
	}
	rec := newFakeRecorder()
	got, err := Reflector(context.Background(), runDir, ReflectorInput{
		ParentRunID: "run-1",
		Sessions: []SessionEntry{
			{SessionID: "sess-A", AgentCallIDs: []string{"ac-1"}, ProviderID: "codex"},
		},
	}, agent, rec, NewLimiter(envRillLearningsMaxParallel, defaultMaxParallel))
	if err != nil {
		t.Fatalf("Reflector: %v", err)
	}
	if got.AcceptedCount != 1 {
		t.Errorf("AcceptedCount = %d, want 1 (only valid category accepted)", got.AcceptedCount)
	}
	if got.DroppedCount != 1 {
		t.Errorf("DroppedCount = %d, want 1", got.DroppedCount)
	}
	if len(rec.rows) != 1 {
		t.Errorf("recorded rows = %d, want 1", len(rec.rows))
	}
	a := readReflectorArtifactT(t, got.ReflectorJSONPath)
	if len(a.Dropped) != 1 {
		t.Fatalf("Dropped len = %d, want 1; %+v", len(a.Dropped), a.Dropped)
	}
	if a.Dropped[0].Reason != dropReasonInvalidCategory {
		t.Errorf("Dropped[0].Reason = %q, want %q", a.Dropped[0].Reason, dropReasonInvalidCategory)
	}
}

// TestReflectorBestEffortPerSessionFailure: 観点 (d) 1 session が CallAgent error で
// 失敗しても他 session は続行し per_session_failures に記録される。
func TestReflectorBestEffortPerSessionFailure(t *testing.T) {
	withProviderEnv(t, "codex")
	runDir := t.TempDir()

	agent := &fakeReflectorAgent{
		responses: map[string]provider.AgentResponse{
			"sess-OK": {Content: `{"learnings":[{"category":"tool-pattern","context":"c","insight":"i","paths_glob":["x/**"]}]}`},
		},
		errors: map[string]error{
			"sess-FAIL": errors.New("simulated network error"),
		},
	}
	rec := newFakeRecorder()
	got, err := Reflector(context.Background(), runDir, ReflectorInput{
		ParentRunID: "run-1",
		Sessions: []SessionEntry{
			{SessionID: "sess-FAIL", AgentCallIDs: []string{"ac-1"}, ProviderID: "codex"},
			{SessionID: "sess-OK", AgentCallIDs: []string{"ac-2"}, ProviderID: "codex"},
		},
	}, agent, rec, NewLimiter(envRillLearningsMaxParallel, defaultMaxParallel))
	if err != nil {
		t.Fatalf("Reflector: %v", err)
	}
	if got.AcceptedCount != 1 {
		t.Errorf("AcceptedCount = %d, want 1 (sess-OK 成功)", got.AcceptedCount)
	}
	if got.PerSessionErrors != 1 {
		t.Errorf("PerSessionErrors = %d, want 1", got.PerSessionErrors)
	}
	if got.AllFailed {
		t.Errorf("AllFailed = true, want false")
	}
	a := readReflectorArtifactT(t, got.ReflectorJSONPath)
	if len(a.PerSessionFailures) != 1 {
		t.Fatalf("PerSessionFailures len = %d, want 1", len(a.PerSessionFailures))
	}
	if a.PerSessionFailures[0].SessionID != "sess-FAIL" {
		t.Errorf("PerSessionFailures[0].SessionID = %q, want sess-FAIL", a.PerSessionFailures[0].SessionID)
	}
}

// TestReflectorAllFailedFlag: 観点 (e) 全 session 失敗 → AllFailed=true。
func TestReflectorAllFailedFlag(t *testing.T) {
	withProviderEnv(t, "codex")
	runDir := t.TempDir()

	agent := &fakeReflectorAgent{
		errors: map[string]error{
			"sess-A": errors.New("err A"),
			"sess-B": errors.New("err B"),
		},
	}
	got, err := Reflector(context.Background(), runDir, ReflectorInput{
		ParentRunID: "run-1",
		Sessions: []SessionEntry{
			{SessionID: "sess-A", AgentCallIDs: []string{"ac-1"}, ProviderID: "codex"},
			{SessionID: "sess-B", AgentCallIDs: []string{"ac-2"}, ProviderID: "codex"},
		},
	}, agent, newFakeRecorder(), NewLimiter(envRillLearningsMaxParallel, defaultMaxParallel))
	if err != nil {
		t.Fatalf("Reflector: %v", err)
	}
	if !got.AllFailed {
		t.Errorf("AllFailed = false, want true (全 session 失敗)")
	}
	if got.AcceptedCount != 0 {
		t.Errorf("AcceptedCount = %d, want 0", got.AcceptedCount)
	}
}

// TestReflectorErrNoCredentials: 観点 (f) credential 不在を heuristic 検出 → NoCredentials=true。
func TestReflectorErrNoCredentials(t *testing.T) {
	withProviderEnv(t, "codex")
	runDir := t.TempDir()

	agent := &fakeReflectorAgent{
		errors: map[string]error{
			"sess-A": fmt.Errorf("provider auth: no credentials configured"),
		},
	}
	got, err := Reflector(context.Background(), runDir, ReflectorInput{
		ParentRunID: "run-1",
		Sessions: []SessionEntry{
			{SessionID: "sess-A", AgentCallIDs: []string{"ac-1"}, ProviderID: "codex"},
		},
	}, agent, newFakeRecorder(), NewLimiter(envRillLearningsMaxParallel, defaultMaxParallel))
	if err != nil {
		t.Fatalf("Reflector: %v", err)
	}
	if !got.NoCredentials {
		t.Errorf("NoCredentials = false, want true")
	}
}

// TestReflectorEmptyLearningsArrayValid: 観点 (g) 空 learnings 配列 → insert 0 件
// で OK (Req 5.4)。AllFailed は accept = 0 だが「全 session 失敗」ではないので true になるが、
// orchestration 側で skipped_provider 0 / failures 0 を観測して resume-all-failed を回避する設計。
// 本テストは「空配列が parse 失敗扱いにならない」ことを観測する。
func TestReflectorEmptyLearningsArrayValid(t *testing.T) {
	withProviderEnv(t, "codex")
	runDir := t.TempDir()

	agent := &fakeReflectorAgent{
		responses: map[string]provider.AgentResponse{
			"sess-A": {Content: `{"learnings":[]}`},
		},
	}
	rec := newFakeRecorder()
	got, err := Reflector(context.Background(), runDir, ReflectorInput{
		ParentRunID: "run-1",
		Sessions: []SessionEntry{
			{SessionID: "sess-A", AgentCallIDs: []string{"ac-1"}, ProviderID: "codex"},
		},
	}, agent, rec, NewLimiter(envRillLearningsMaxParallel, defaultMaxParallel))
	if err != nil {
		t.Fatalf("Reflector: %v", err)
	}
	if got.AcceptedCount != 0 {
		t.Errorf("AcceptedCount = %d, want 0", got.AcceptedCount)
	}
	if got.PerSessionErrors != 0 {
		t.Errorf("PerSessionErrors = %d, want 0 (empty array は valid)", got.PerSessionErrors)
	}
	if len(rec.rows) != 0 {
		t.Errorf("recorded rows = %d, want 0", len(rec.rows))
	}
	a := readReflectorArtifactT(t, got.ReflectorJSONPath)
	if len(a.PerSessionFailures) != 0 {
		t.Errorf("PerSessionFailures len = %d, want 0", len(a.PerSessionFailures))
	}
}

// TestReflectorSkipsProviderNotInAllowlist: 観点 (h) allowlist 外 provider session は
// skipped_provider に trace され LLM call は発火しない。
func TestReflectorSkipsProviderNotInAllowlist(t *testing.T) {
	// allowlist は codex のみ
	withProviderEnv(t, "codex")
	runDir := t.TempDir()

	agent := &fakeReflectorAgent{
		responses: map[string]provider.AgentResponse{},
	}
	got, err := Reflector(context.Background(), runDir, ReflectorInput{
		ParentRunID: "run-1",
		Sessions: []SessionEntry{
			{SessionID: "sess-claude", AgentCallIDs: []string{"ac-1"}, ProviderID: "claude"},
			{SessionID: "sess-cursor", AgentCallIDs: []string{"ac-2"}, ProviderID: "cursor"},
		},
	}, agent, newFakeRecorder(), NewLimiter(envRillLearningsMaxParallel, defaultMaxParallel))
	if err != nil {
		t.Fatalf("Reflector: %v", err)
	}
	if got.SkippedProviderCount != 2 {
		t.Errorf("SkippedProviderCount = %d, want 2", got.SkippedProviderCount)
	}
	agent.mu.Lock()
	calls := len(agent.calls)
	agent.mu.Unlock()
	if calls != 0 {
		t.Errorf("CallAgent invocations = %d, want 0 (allowlist filter で all skip)", calls)
	}
	a := readReflectorArtifactT(t, got.ReflectorJSONPath)
	if len(a.SkippedProvider) != 2 {
		t.Fatalf("SkippedProvider len = %d, want 2", len(a.SkippedProvider))
	}
	if a.SkippedProvider[0].Reason != SkipReasonProviderNotAllowed {
		t.Errorf("SkippedProvider[0].Reason = %q, want %q", a.SkippedProvider[0].Reason, SkipReasonProviderNotAllowed)
	}
}

// TestReflectorParallelLimit: 観点 (i) ParallelLimiter の上限を超える同時 in-flight が
// 起きないことを観測する。N=10 sessions / max=3 なので peak inflight <= 3 が不変式。
func TestReflectorParallelLimit(t *testing.T) {
	withProviderEnv(t, "codex")
	runDir := t.TempDir()

	const numSessions = 10
	const maxParallel = 3

	sessions := make([]SessionEntry, numSessions)
	resps := map[string]provider.AgentResponse{}
	for i := 0; i < numSessions; i++ {
		sid := fmt.Sprintf("sess-%02d", i)
		sessions[i] = SessionEntry{SessionID: sid, AgentCallIDs: []string{fmt.Sprintf("ac-%02d", i)}, ProviderID: "codex"}
		resps[sid] = provider.AgentResponse{Content: `{"learnings":[]}`}
	}
	agent := &fakeReflectorAgent{
		responses: resps,
		delay:     10 * time.Millisecond,
	}
	limiter := NewLimiterForTest(maxParallel)

	_, err := Reflector(context.Background(), runDir, ReflectorInput{
		ParentRunID: "run-1",
		Sessions:    sessions,
	}, agent, newFakeRecorder(), limiter)
	if err != nil {
		t.Fatalf("Reflector: %v", err)
	}
	peak := atomic.LoadInt32(&agent.peak)
	if peak > int32(maxParallel) {
		t.Errorf("peak inflight = %d, want <= %d (ParallelLimiter 上限超え)", peak, maxParallel)
	}
}

// NewLimiterForTest は test 専用の ParallelLimiter constructor (env を介さず直接 N を指定)。
// production は NewLimiter(envName, defaultN) のみ exposed なので、test での parallel
// 上限差し替えのために本 helper を export する。本ファイル外でも使える形にしておく。
func NewLimiterForTest(n int) *ParallelLimiter {
	return &ParallelLimiter{sem: make(chan struct{}, n)}
}

// TestDefaultProviderAllowlistIncludesClaudeAndCodex は production の default 値が
// `claude,codex` であることを直接 assert するリグレッションテスト。
// 経緯: claude provider の parent loop と codex provider の parent loop の両方を、env 未設定時から
// Reflector の学習対象に含める。
func TestDefaultProviderAllowlistIncludesClaudeAndCodex(t *testing.T) {
	const want = "claude,codex"
	if defaultProviderAllowlist != want {
		t.Fatalf("defaultProviderAllowlist = %q, want %q (claude/codex provider の parent loop の学びを reflector に流すための default)", defaultProviderAllowlist, want)
	}
}

// TestResolveProviderAllowlist は CSV parse の挙動 4 経路を観測する純関数テスト。
func TestResolveProviderAllowlist(t *testing.T) {
	t.Run("env empty -> default", func(t *testing.T) {
		t.Setenv(envRillLearningsProviders, "")
		got := resolveProviderAllowlist(envRillLearningsProviders, "codex")
		want := []provider.ProviderID{provider.ProviderCodex}
		if !equalProviderIDSlice(got, want) {
			t.Errorf("got=%v want=%v", got, want)
		}
	})
	t.Run("env set with multiple", func(t *testing.T) {
		t.Setenv(envRillLearningsProviders, "claude,codex,cursor")
		got := resolveProviderAllowlist(envRillLearningsProviders, "codex")
		want := []provider.ProviderID{provider.ProviderClaude, provider.ProviderCodex, provider.ProviderCursor}
		if !equalProviderIDSlice(got, want) {
			t.Errorf("got=%v want=%v", got, want)
		}
	})
	t.Run("env with whitespace + dup", func(t *testing.T) {
		t.Setenv(envRillLearningsProviders, " codex , codex , claude ")
		got := resolveProviderAllowlist(envRillLearningsProviders, "codex")
		want := []provider.ProviderID{provider.ProviderCodex, provider.ProviderClaude}
		if !equalProviderIDSlice(got, want) {
			t.Errorf("got=%v want=%v", got, want)
		}
	})
	t.Run("env all empty trim -> default", func(t *testing.T) {
		t.Setenv(envRillLearningsProviders, ", , ")
		got := resolveProviderAllowlist(envRillLearningsProviders, "codex")
		want := []provider.ProviderID{provider.ProviderCodex}
		if !equalProviderIDSlice(got, want) {
			t.Errorf("got=%v want=%v", got, want)
		}
	})
}

// TestExtractAgentResponseJSON_StructuredOutputPath は claude bridge の json_schema 経路
// (resp.Content="" / resp.StructuredOutput=非nil) で JSON が正しく抽出されることを検証する。
// spike-results.md Findings #2 の seam に対応。
func TestExtractAgentResponseJSON_StructuredOutputPath(t *testing.T) {
	resp := provider.AgentResponse{
		StructuredOutput: map[string]any{
			"learnings": []any{
				map[string]any{
					"category":   "tool-pattern",
					"context":    "c",
					"insight":    "i",
					"paths_glob": []any{"x/**"},
				},
			},
		},
	}
	var out reflectorResponse
	raw, ok := extractAgentResponseJSON(resp, &out)
	if !ok {
		t.Fatalf("expected ok=true (StructuredOutput)")
	}
	if !strings.Contains(raw, "tool-pattern") {
		t.Errorf("raw = %q, want contains tool-pattern", raw)
	}
	if len(out.Learnings) != 1 || out.Learnings[0].Category != "tool-pattern" {
		t.Errorf("out = %+v", out)
	}
}

// TestBuildReflectorPromptDefault は in.Skill 空のとき slash command が組み立てられることを確認する。
func TestBuildReflectorPromptDefault(t *testing.T) {
	got := buildReflectorPrompt("", "/tmp/run-dir-1")
	want := "/agent-learnings-curator reflector /tmp/run-dir-1"
	if got != want {
		t.Errorf("got=%q want=%q", got, want)
	}
}

// TestBuildReflectorPromptOverride は in.Skill に明示値 / dummy が入っているとそのまま返ることを確認する。
func TestBuildReflectorPromptOverride(t *testing.T) {
	got := buildReflectorPrompt("custom-skill", "/tmp/x")
	if got != "custom-skill" {
		t.Errorf("got=%q want=custom-skill", got)
	}
}

// TestIsErrNoCredentials は heuristic 検出の正と偽を観測する。
func TestIsErrNoCredentials(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("network timeout"), false},
		{"no credentials", errors.New("auth failed: no credentials available"), true},
		{"unauthorized", errors.New("status 401 unauthorized"), true},
		{"missing api key", errors.New("MISSING API KEY"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isErrNoCredentials(tc.err); got != tc.want {
				t.Errorf("got=%v want=%v", got, tc.want)
			}
		})
	}
}

// TestReflectorWritesLearningsJSONWithProvenance: Reflector が Recorder accept 済み learning を
// `<runDir>/learnings.json` に provenance (session_id / step_id / provider_id / akashic learnings.id /
// recorded_at) 付きで集約して書き出すことを観測する。
//
// この test は Curator 入力契約 (skill SSoT 側 curator.md が `<runDir>/learnings.json` を Read する)
// の Go 側端を pin する両端契約テストの片端である (もう片端は schema_test.go の
// TestSkillContractIntegrity_LearningsArtifactSchema)。
//
// 検証観点:
//
//   - (a) `<runDir>/learnings.json` が書き出される
//   - (b) artifact 内の learnings 配列件数 = Recorder accept 件数 (sess-A 2 件 + sess-B 1 件 = 3)
//   - (c) 各 entry は LLM-emitted (category / context / insight / paths_glob) と provenance
//     (id / session_id / step_id / provider_id / recorded_at) を保持
//   - (d) 各 entry の `id` は akashic.LearningRow.ID と一致 (cross-reference 可能性の保証)
//   - (e) 順序は (session_id ASC, intra-session index ASC) — 並列 goroutine の race を許容しない
//     決定的順序であること (curator novelty 判定の再現性のため)
func TestReflectorWritesLearningsJSONWithProvenance(t *testing.T) {
	withProviderEnv(t, "codex")
	runDir := t.TempDir()

	agent := &fakeReflectorAgent{
		responses: map[string]provider.AgentResponse{
			"sess-A": {Content: `{"learnings":[
				{"category":"failure-recovery","context":"ctx-A1","insight":"ins-A1","paths_glob":["a/**"]},
				{"category":"tool-pattern","context":"ctx-A2","insight":"ins-A2","paths_glob":["b/**","c/**"]}
			]}`},
			"sess-B": {Content: `{"learnings":[
				{"category":"antipattern","context":"ctx-B1","insight":"ins-B1","paths_glob":["x/**"]}
			]}`},
		},
	}
	rec := newFakeRecorder()
	got, err := Reflector(context.Background(), runDir, ReflectorInput{
		ParentRunID: "parent-run-1",
		WorkflowID:  "kiro-impl-loop",
		Sessions: []SessionEntry{
			{SessionID: "sess-B", AgentCallIDs: []string{"ac-B"}, ProviderID: "codex", StepID: "review"},
			{SessionID: "sess-A", AgentCallIDs: []string{"ac-A"}, ProviderID: "codex", StepID: "implement"},
		},
	}, agent, rec, NewLimiter(envRillLearningsMaxParallel, defaultMaxParallel))
	if err != nil {
		t.Fatalf("Reflector: %v", err)
	}
	if got.AcceptedCount != 3 {
		t.Fatalf("AcceptedCount = %d, want 3", got.AcceptedCount)
	}

	// (a) artifact が書き出されている
	if got.LearningsJSONPath == "" {
		t.Fatalf("LearningsJSONPath empty")
	}
	if filepath.Base(got.LearningsJSONPath) != learningsArtifactFilename {
		t.Errorf("LearningsJSONPath basename = %q, want %q", filepath.Base(got.LearningsJSONPath), learningsArtifactFilename)
	}

	body, rerr := os.ReadFile(got.LearningsJSONPath)
	if rerr != nil {
		t.Fatalf("read learnings.json: %v", rerr)
	}
	var artifact LearningsArtifact
	if uerr := json.Unmarshal(body, &artifact); uerr != nil {
		t.Fatalf("unmarshal learnings.json: %v\nbody=%s", uerr, body)
	}

	// (b) 件数
	if len(artifact.Learnings) != 3 {
		t.Fatalf("artifact.Learnings len = %d, want 3", len(artifact.Learnings))
	}

	// (c) provenance / LLM-emitted 両方が揃っている (代表 1 件で確認)
	first := artifact.Learnings[0]
	if first.SessionID != "sess-A" || first.StepID != "implement" || first.ProviderID != "codex" {
		t.Errorf("provenance mismatch on first entry: %+v", first)
	}
	if first.Category == "" || first.Context == "" || first.Insight == "" || len(first.PathsGlob) == 0 {
		t.Errorf("LLM-emitted fields missing on first entry: %+v", first)
	}
	if first.RecordedAt.IsZero() {
		t.Errorf("RecordedAt is zero on first entry")
	}

	// (d) id が akashic LearningRow.ID と一致
	if len(rec.rows) != 3 {
		t.Fatalf("recorder rows = %d, want 3", len(rec.rows))
	}
	idsInArtifact := map[string]struct{}{}
	for _, e := range artifact.Learnings {
		idsInArtifact[e.ID] = struct{}{}
	}
	for _, row := range rec.rows {
		if _, ok := idsInArtifact[row.ID]; !ok {
			t.Errorf("akashic row.ID %q missing from learnings.json", row.ID)
		}
	}

	// (e) 決定的順序 = (session_id ASC, intra-session index ASC)。
	// 期待: sess-A.idx0, sess-A.idx1, sess-B.idx0 → context が ctx-A1, ctx-A2, ctx-B1 順。
	wantOrder := []string{"ctx-A1", "ctx-A2", "ctx-B1"}
	for i, want := range wantOrder {
		if artifact.Learnings[i].Context != want {
			t.Errorf("order[%d].Context = %q, want %q (want deterministic (session_id ASC, idx ASC))", i, artifact.Learnings[i].Context, want)
		}
	}

	// schema metadata も pin する (schema_version / parent_run_id)
	if artifact.SchemaVersion != learningsArtifactSchemaVersion {
		t.Errorf("schema_version = %d, want %d", artifact.SchemaVersion, learningsArtifactSchemaVersion)
	}
	if artifact.ParentRunID != "parent-run-1" {
		t.Errorf("parent_run_id = %q, want %q", artifact.ParentRunID, "parent-run-1")
	}
}

// TestReflectorWritesLearningsJSONEmpty: accept 0 件のときも空 artifact (`learnings: []`) が
// 書き出されることを観測する。Curator phase は空でも learnings.json が存在することを前提に動く
// (skill 側 Read tool の path traversal 防御と整合)。
func TestReflectorWritesLearningsJSONEmpty(t *testing.T) {
	withProviderEnv(t, "codex")
	runDir := t.TempDir()

	agent := &fakeReflectorAgent{
		responses: map[string]provider.AgentResponse{
			"sess-A": {Content: `{"learnings":[]}`},
		},
	}
	got, err := Reflector(context.Background(), runDir, ReflectorInput{
		ParentRunID: "parent-run-1",
		Sessions: []SessionEntry{
			{SessionID: "sess-A", AgentCallIDs: []string{"ac-A"}, ProviderID: "codex"},
		},
	}, agent, newFakeRecorder(), NewLimiter(envRillLearningsMaxParallel, defaultMaxParallel))
	if err != nil {
		t.Fatalf("Reflector: %v", err)
	}
	if got.LearningsJSONPath == "" {
		t.Fatalf("LearningsJSONPath empty even on 0 accept")
	}
	body, rerr := os.ReadFile(got.LearningsJSONPath)
	if rerr != nil {
		t.Fatalf("read learnings.json: %v", rerr)
	}
	var artifact LearningsArtifact
	if uerr := json.Unmarshal(body, &artifact); uerr != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", uerr, body)
	}
	if len(artifact.Learnings) != 0 {
		t.Errorf("Learnings len = %d, want 0", len(artifact.Learnings))
	}
}

// 以下、test stub の package 互換 stub 用 helper (未使用 import 警告の回避を兼ねる)。
var _ = filepath.Join
var _ = errors.Is
