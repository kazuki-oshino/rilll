// Package learnings の本 file は agent-learnings 4 phase pipeline の最初の phase
// (pre-fetch) を担う `PreFetch` 関数の契約を検証する単体テスト群を保持する
// (tasks.md 7.1 / Requirement 3.1〜3.6, 8.2, 9.1)。
//
// 検証観点:
//
//   - (a) `reader == nil` のときは `ErrReaderUnavailable` sentinel を返し、
//     副作用 (sessions.json 生成) を起こさない (Req 9.1)。
//   - (b) `AgentCallsByWorkflowRun` 由来の error が同 sentinel に wrap されて伝播し
//     `errors.Is` で識別できる (Req 9.1 / design.md L504)。
//   - (c) `agent_calls` 0 件で `UniqueSessionIDs == []` を返し、上位が
//     `verdict=applied-no-op` 経路に進める seam を提供する (Req 3.3)。
//   - (d) `session_id` が空文字 / NULL 相当の row は dedup 集合から除外され、
//     `sessions.json.skipped[]` に trace される (Req 3.4)。
//   - (e) 同一 session_id を複数 agent_call が持つ場合は最初の出現を保ち、後続を
//     `SessionEntry.AgentCallIDs` に追記し `UniqueSessionIDs` には 1 度だけ含む
//     (Req 3.2 dedup + 安定順序: started_at ASC tiebreaker)。
//   - (f) `<runDir>/sessions.json` に `parent_run_id` / `sessions[]` /
//     `skipped[]` が atomic に書き出される (Req 3.5, 8.2)。
//   - (g) `ProjectScopedReader` 経由で `AgentCallsByWorkflowRun` が呼ばれ、
//     project scope 強制が物理保証される (Req 3.6)。fake reader の inner 戻り値に
//     別 project の row を混ぜ、cwd basename と一致しない row が除外されることで
//     decorator の post-filter 経路を検証する。
//
// テスト戦略:
//
//   - akashic.Reader interface を満たす最小 stub を定義し、`AgentCallsByWorkflowRun`
//     のみ非自明な値を返す (他メソッドは zero / nil を返すだけ)。
//   - `ProjectScopedReader` は本番経路と同じ decorator を使うため、stub の戻り値に
//     project_id 違いの row を混ぜることで decorator の post-filter が観測できる。
//   - artifact は path ベースで read し、`SessionsArtifact` に decode して構造的に比較する
//     (`time.Time` の RFC3339Nano 既定 marshal は GeneratedAt の存在のみ確認)。
package learnings

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

// fakePrefetchReader は `akashic.Reader` を満たす最小 test stub。
// `AgentCallsByWorkflowRun` のみ非自明な値を返し、他メソッドは zero / nil。
//
// `gotWorkflowRunID` には直近の `AgentCallsByWorkflowRun` 呼び出し時に渡された
// workflow_run_id を 1 件だけ記録する。`ProjectScopedReader` は inner の戻り値を
// post-filter するだけなので opts.ProjectID は元 reader 側に到達しないが、本 stub では
// runID 引数で「PreFetch が PreFetchInput.ParentRunID を渡しているか」を検証する。
type fakePrefetchReader struct {
	rows                 []akashic.AgentCallRow
	rowsByRunID          map[string][]akashic.AgentCallRow
	err                  error
	gotWorkflowRunID     string
	gotWorkflowRunIDs    []string
	calls                int
	descendantRunIDs     []string
	descendantErr        error
	gotDescendantRootRun string
}

// AgentCallsByWorkflowRun は fakePrefetchReader の非自明な実装。
// 他テストが PreFetch の post-filter 経路を観測できるよう、`rows` を呼び出し側が
// セットした順番のまま返す (`AgentCallsByWorkflowRun` の本物実装は started_at ASC,
// id ASC で sort するが、本 stub では呼び出し側が sort 済みの fixture を渡す前提)。
func (r *fakePrefetchReader) AgentCallsByWorkflowRun(_ context.Context, workflowRunID string) ([]akashic.AgentCallRow, error) {
	r.gotWorkflowRunID = workflowRunID
	r.gotWorkflowRunIDs = append(r.gotWorkflowRunIDs, workflowRunID)
	r.calls++
	if r.rowsByRunID != nil {
		return r.rowsByRunID[workflowRunID], r.err
	}
	return r.rows, r.err
}

func (r *fakePrefetchReader) DescendantWorkflowRunIDs(_ context.Context, workflowRunID string) ([]string, error) {
	r.gotDescendantRootRun = workflowRunID
	return r.descendantRunIDs, r.descendantErr
}

// 残り Reader メソッドは PreFetch では呼ばれないため zero / nil を返す。
func (r *fakePrefetchReader) List(_ context.Context, _ akashic.ListOptions) (akashic.ListResult, error) {
	return akashic.ListResult{}, nil
}
func (r *fakePrefetchReader) AggregateByWorkflow(_ context.Context, _ akashic.WorkflowAggregateOptions) (akashic.WorkflowAggregateResult, error) {
	return akashic.WorkflowAggregateResult{}, nil
}
func (r *fakePrefetchReader) AggregateByProvider(_ context.Context, _ akashic.ProviderAggregateOptions) (akashic.ProviderAggregateResult, error) {
	return akashic.ProviderAggregateResult{}, nil
}
func (r *fakePrefetchReader) Detail(_ context.Context, _ akashic.DetailOptions) (akashic.RunDetail, error) {
	return akashic.RunDetail{}, nil
}
func (r *fakePrefetchReader) FailureClusters(_ context.Context, _ akashic.FailureOptions) (akashic.FailureClustersResult, error) {
	return akashic.FailureClustersResult{}, nil
}
func (r *fakePrefetchReader) InRunResolutionsForCluster(_ context.Context, _ akashic.InRunResolutionOptions) (akashic.InRunResolutionsResult, error) {
	return akashic.InRunResolutionsResult{}, nil
}
func (r *fakePrefetchReader) NextSuccessAcrossRuns(_ context.Context, _ akashic.NextSuccessOptions) (akashic.NextSuccessResult, error) {
	return akashic.NextSuccessResult{}, nil
}
func (r *fakePrefetchReader) LearningsByWorkflowRun(_ context.Context, _ string) ([]akashic.LearningRow, error) {
	return nil, nil
}

// makeFixtureProjectCWD は `t.TempDir()` 配下に project ID を basename とする
// サブディレクトリを作って絶対パスを返す test helper (v1 distiller test と同形)。
//
// `akashic.ProjectIDFromCWD` は `filepath.Abs` 後の `filepath.Base` を取るため、
// この helper で生成した cwd を渡せば `projectID == projectName` が決定的になる
// (テスト golden 比較に必須)。
func makeFixtureProjectCWD(t *testing.T, projectName string) string {
	t.Helper()
	root := t.TempDir()
	cwd := filepath.Join(root, projectName)
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir cwd %q: %v", cwd, err)
	}
	return cwd
}

// readSessionsArtifact は path に書かれた sessions.json を `SessionsArtifact` に
// decode する test helper。
func readSessionsArtifact(t *testing.T, path string) SessionsArtifact {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%q): %v", path, err)
	}
	var out SessionsArtifact
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("json.Unmarshal: %v\nbody=%s", err, body)
	}
	return out
}

// TestPreFetchReturnsErrReaderUnavailableWhenReaderNil は `reader == nil` で
// `ErrReaderUnavailable` を返し artifact を生成しないことを検証する (Req 9.1)。
func TestPreFetchReturnsErrReaderUnavailableWhenReaderNil(t *testing.T) {
	cwd := makeFixtureProjectCWD(t, "rilll")
	runDir := t.TempDir()

	got, err := PreFetch(context.Background(), runDir, PreFetchInput{
		CWD:         cwd,
		ParentRunID: "20260508T010203Z-parent01",
	}, nil)

	if !errors.Is(err, ErrReaderUnavailable) {
		t.Fatalf("err = %v, want errors.Is(err, ErrReaderUnavailable)", err)
	}
	if got.SessionsJSONPath != "" {
		t.Errorf("got.SessionsJSONPath = %q, want empty (no artifact written)", got.SessionsJSONPath)
	}
	// artifact ファイルが書かれていないことを確認 (副作用なし)。
	if _, statErr := os.Stat(filepath.Join(runDir, sessionsFilename)); !os.IsNotExist(statErr) {
		t.Errorf("sessions.json should not exist on nil reader; statErr = %v", statErr)
	}
}

// TestPreFetchPropagatesReaderErrorAsErrReaderUnavailable は
// `AgentCallsByWorkflowRun` 由来の error が `ErrReaderUnavailable` に wrap されて
// 伝播し、`errors.Is` で識別できることを検証する (design.md L504 / Req 9.1)。
func TestPreFetchPropagatesReaderErrorAsErrReaderUnavailable(t *testing.T) {
	cwd := makeFixtureProjectCWD(t, "rilll")
	runDir := t.TempDir()
	innerErr := errors.New("simulated db lock")

	reader := &fakePrefetchReader{err: innerErr}

	got, err := PreFetch(context.Background(), runDir, PreFetchInput{
		CWD:         cwd,
		ParentRunID: "20260508T010203Z-parent01",
	}, reader)

	if err == nil {
		t.Fatal("err = nil, want non-nil error")
	}
	if !errors.Is(err, ErrReaderUnavailable) {
		t.Fatalf("err = %v, want errors.Is(err, ErrReaderUnavailable)", err)
	}
	if !errors.Is(err, innerErr) {
		t.Errorf("err = %v, want errors.Is(err, innerErr) so original cause is preserved", err)
	}
	if got.SessionsJSONPath != "" {
		t.Errorf("got.SessionsJSONPath = %q, want empty on reader error", got.SessionsJSONPath)
	}
}

// TestPreFetchEmptyAgentCallsReturnsEmptyUniqueIDs は agent_calls 0 件で
// `UniqueSessionIDs == []` を返し sessions.json は依然として書き出されることを
// 検証する (Req 3.3 の applied-no-op seam + 8.2 artifact 常時生成)。
func TestPreFetchEmptyAgentCallsReturnsEmptyUniqueIDs(t *testing.T) {
	cwd := makeFixtureProjectCWD(t, "rilll")
	runDir := t.TempDir()

	reader := &fakePrefetchReader{rows: nil}

	got, err := PreFetch(context.Background(), runDir, PreFetchInput{
		CWD:         cwd,
		ParentRunID: "20260508T010203Z-parent01",
	}, reader)
	if err != nil {
		t.Fatalf("PreFetch: %v", err)
	}
	if len(got.UniqueSessionIDs) != 0 {
		t.Errorf("UniqueSessionIDs = %v, want empty", got.UniqueSessionIDs)
	}
	if got.SkippedRowCount != 0 {
		t.Errorf("SkippedRowCount = %d, want 0", got.SkippedRowCount)
	}
	if got.SessionsJSONPath == "" {
		t.Fatal("SessionsJSONPath empty; artifact should be written even with 0 agent_calls")
	}
	artifact := readSessionsArtifact(t, got.SessionsJSONPath)
	if artifact.SchemaVersion != sessionsSchemaVersion {
		t.Errorf("artifact.SchemaVersion = %d, want %d", artifact.SchemaVersion, sessionsSchemaVersion)
	}
	if artifact.ParentRunID != "20260508T010203Z-parent01" {
		t.Errorf("artifact.ParentRunID = %q, want parent_run_id", artifact.ParentRunID)
	}
	if len(artifact.Sessions) != 0 {
		t.Errorf("artifact.Sessions = %v, want empty", artifact.Sessions)
	}
	if len(artifact.Skipped) != 0 {
		t.Errorf("artifact.Skipped = %v, want empty", artifact.Skipped)
	}
}

// TestPreFetchSkipsEmptySessionIDRows は空 session_id を持つ row が dedup 集合から
// 除外され `sessions.json.skipped[]` に trace されることを検証する (Req 3.4)。
//
// ProjectScopedReader 経由の post-filter では project_id 一致 row のみが残るため、
// fixture 全 row に同じ project_id を埋めて project filter を通過させる。
func TestPreFetchSkipsEmptySessionIDRows(t *testing.T) {
	cwd := makeFixtureProjectCWD(t, "rilll")
	runDir := t.TempDir()

	t1 := time.Date(2026, 5, 8, 1, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 8, 1, 0, 1, 0, time.UTC)
	t3 := time.Date(2026, 5, 8, 1, 0, 2, 0, time.UTC)

	reader := &fakePrefetchReader{
		rows: []akashic.AgentCallRow{
			{ID: "ac-1", ProjectID: "rilll", SessionID: "sess-A", StepID: "compose", ProviderID: "claude", StartedAt: t1},
			{ID: "ac-2", ProjectID: "rilll", SessionID: "", StepID: "review", ProviderID: "cursor", StartedAt: t2},
			{ID: "ac-3", ProjectID: "rilll", SessionID: "sess-B", StepID: "implement", ProviderID: "claude", StartedAt: t3},
		},
	}

	got, err := PreFetch(context.Background(), runDir, PreFetchInput{
		CWD:         cwd,
		ParentRunID: "20260508T010203Z-parent01",
	}, reader)
	if err != nil {
		t.Fatalf("PreFetch: %v", err)
	}

	wantIDs := []string{"sess-A", "sess-B"}
	if !reflect.DeepEqual(got.UniqueSessionIDs, wantIDs) {
		t.Errorf("UniqueSessionIDs = %v, want %v", got.UniqueSessionIDs, wantIDs)
	}
	if got.SkippedRowCount != 1 {
		t.Errorf("SkippedRowCount = %d, want 1", got.SkippedRowCount)
	}

	artifact := readSessionsArtifact(t, got.SessionsJSONPath)
	if len(artifact.Skipped) != 1 {
		t.Fatalf("artifact.Skipped len = %d, want 1; got = %v", len(artifact.Skipped), artifact.Skipped)
	}
	if artifact.Skipped[0].AgentCallID != "ac-2" {
		t.Errorf("Skipped[0].AgentCallID = %q, want ac-2", artifact.Skipped[0].AgentCallID)
	}
	if artifact.Skipped[0].Reason != SkipReasonEmptySessionID {
		t.Errorf("Skipped[0].Reason = %q, want %q", artifact.Skipped[0].Reason, SkipReasonEmptySessionID)
	}
	if artifact.Skipped[0].StepID != "review" {
		t.Errorf("Skipped[0].StepID = %q, want review", artifact.Skipped[0].StepID)
	}
}

// TestPreFetchDedupsAndStableOrdersSessionIDs は同一 session_id を複数 agent_call が
// 持つ場合に最初の出現を保ち、`UniqueSessionIDs` には 1 度だけ含み、後続を
// `SessionEntry.AgentCallIDs` に追記することを検証する (Req 3.2 dedup + 安定順序)。
//
// fixture rows は `AgentCallsByWorkflowRun` が返す前提の started_at ASC 順で並べる。
func TestPreFetchDedupsAndStableOrdersSessionIDs(t *testing.T) {
	cwd := makeFixtureProjectCWD(t, "rilll")
	runDir := t.TempDir()

	t1 := time.Date(2026, 5, 8, 1, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 8, 1, 0, 1, 0, time.UTC)
	t3 := time.Date(2026, 5, 8, 1, 0, 2, 0, time.UTC)
	t4 := time.Date(2026, 5, 8, 1, 0, 3, 0, time.UTC)

	reader := &fakePrefetchReader{
		rows: []akashic.AgentCallRow{
			// session-X が最初に t1 で出現
			{ID: "ac-1", ProjectID: "rilll", SessionID: "sess-X", StartedAt: t1},
			// session-Y が t2 で出現
			{ID: "ac-2", ProjectID: "rilll", SessionID: "sess-Y", StartedAt: t2},
			// session-X が再出現 (resume case): UniqueSessionIDs に追加せず AgentCallIDs だけ追記
			{ID: "ac-3", ProjectID: "rilll", SessionID: "sess-X", StartedAt: t3},
			// session-Z が t4 で出現
			{ID: "ac-4", ProjectID: "rilll", SessionID: "sess-Z", StartedAt: t4},
		},
	}

	got, err := PreFetch(context.Background(), runDir, PreFetchInput{
		CWD:         cwd,
		ParentRunID: "20260508T010203Z-parent01",
	}, reader)
	if err != nil {
		t.Fatalf("PreFetch: %v", err)
	}

	wantIDs := []string{"sess-X", "sess-Y", "sess-Z"}
	if !reflect.DeepEqual(got.UniqueSessionIDs, wantIDs) {
		t.Errorf("UniqueSessionIDs = %v, want %v (started_at ASC tiebreaker)", got.UniqueSessionIDs, wantIDs)
	}
	if got.SkippedRowCount != 0 {
		t.Errorf("SkippedRowCount = %d, want 0", got.SkippedRowCount)
	}

	artifact := readSessionsArtifact(t, got.SessionsJSONPath)
	if len(artifact.Sessions) != 3 {
		t.Fatalf("artifact.Sessions len = %d, want 3; got = %+v", len(artifact.Sessions), artifact.Sessions)
	}
	// session-X は ac-1, ac-3 の 2 件を保持する (resume case)。
	if artifact.Sessions[0].SessionID != "sess-X" {
		t.Errorf("Sessions[0].SessionID = %q, want sess-X", artifact.Sessions[0].SessionID)
	}
	wantSessionXAgentCallIDs := []string{"ac-1", "ac-3"}
	if !reflect.DeepEqual(artifact.Sessions[0].AgentCallIDs, wantSessionXAgentCallIDs) {
		t.Errorf("Sessions[0].AgentCallIDs = %v, want %v", artifact.Sessions[0].AgentCallIDs, wantSessionXAgentCallIDs)
	}
	if artifact.Sessions[1].SessionID != "sess-Y" || len(artifact.Sessions[1].AgentCallIDs) != 1 {
		t.Errorf("Sessions[1] = %+v, want sess-Y with 1 agent_call_id", artifact.Sessions[1])
	}
	if artifact.Sessions[2].SessionID != "sess-Z" || len(artifact.Sessions[2].AgentCallIDs) != 1 {
		t.Errorf("Sessions[2] = %+v, want sess-Z with 1 agent_call_id", artifact.Sessions[2])
	}
}

func TestPreFetchCollectsSessionsFromDescendantWorkflowRuns(t *testing.T) {
	cwd := makeFixtureProjectCWD(t, "rilll")
	runDir := t.TempDir()
	parentRunID := "sequence-run"
	specAt := time.Date(2026, 5, 24, 2, 56, 19, 0, time.UTC)
	implAt := time.Date(2026, 5, 24, 3, 32, 57, 0, time.UTC)
	reader := &fakePrefetchReader{
		descendantRunIDs: []string{"composite-run", "spec-run", "impl-run"},
		rowsByRunID: map[string][]akashic.AgentCallRow{
			parentRunID:     {},
			"composite-run": {},
			"spec-run": {
				{ID: "ac-spec", WorkflowRunID: "spec-run", ProjectID: "rilll", SessionID: "sess-spec", ProviderID: "codex", StepID: "spec", StartedAt: specAt},
			},
			"impl-run": {
				{ID: "ac-impl", WorkflowRunID: "impl-run", ProjectID: "rilll", SessionID: "sess-impl", ProviderID: "codex", StepID: "impl", StartedAt: implAt},
			},
		},
	}

	got, err := PreFetch(context.Background(), runDir, PreFetchInput{
		CWD:         cwd,
		ParentRunID: parentRunID,
	}, reader)
	if err != nil {
		t.Fatalf("PreFetch: %v", err)
	}

	wantQueried := []string{parentRunID, "composite-run", "spec-run", "impl-run"}
	if !reflect.DeepEqual(reader.gotWorkflowRunIDs, wantQueried) {
		t.Fatalf("AgentCallsByWorkflowRun queries = %v, want %v", reader.gotWorkflowRunIDs, wantQueried)
	}
	if reader.gotDescendantRootRun != parentRunID {
		t.Fatalf("DescendantWorkflowRunIDs root = %q, want %q", reader.gotDescendantRootRun, parentRunID)
	}
	wantSessions := []string{"sess-spec", "sess-impl"}
	if !reflect.DeepEqual(got.UniqueSessionIDs, wantSessions) {
		t.Fatalf("UniqueSessionIDs = %v, want %v", got.UniqueSessionIDs, wantSessions)
	}
	artifact := readSessionsArtifact(t, got.SessionsJSONPath)
	if artifact.ParentRunID != parentRunID {
		t.Fatalf("artifact.ParentRunID = %q, want %q", artifact.ParentRunID, parentRunID)
	}
	if len(artifact.Sessions) != 2 {
		t.Fatalf("artifact.Sessions len = %d, want 2: %#v", len(artifact.Sessions), artifact.Sessions)
	}
	if artifact.Sessions[0].AgentCallIDs[0] != "ac-spec" || artifact.Sessions[1].AgentCallIDs[0] != "ac-impl" {
		t.Fatalf("artifact.Sessions = %#v, want spec then impl calls", artifact.Sessions)
	}
}

func TestPreFetchCollectsReflectionTargetsByAgentCallIDFromParentAndDescendants(t *testing.T) {
	cwd := makeFixtureProjectCWD(t, "rilll")
	runDir := t.TempDir()
	parentRunID := "parent-run"
	parentFirstAt := time.Date(2026, 5, 24, 9, 0, 0, 0, time.UTC)
	childAt := parentFirstAt.Add(time.Second)
	parentSecondAt := parentFirstAt.Add(2 * time.Second)
	reader := &fakePrefetchReader{
		descendantRunIDs: []string{"child-run"},
		rowsByRunID: map[string][]akashic.AgentCallRow{
			parentRunID: {
				{ID: "ac-parent-1", WorkflowRunID: parentRunID, ProjectID: "rilll", SessionID: "sess-shared", ProviderID: "codex", StepID: "plan", StepName: "Plan", StartedAt: parentFirstAt},
				{ID: "ac-parent-2", WorkflowRunID: parentRunID, ProjectID: "rilll", SessionID: "sess-shared", ProviderID: "codex", StepID: "apply", StepName: "Apply", StartedAt: parentSecondAt},
			},
			"child-run": {
				{ID: "ac-child-1", WorkflowRunID: "child-run", ProjectID: "rilll", SessionID: "sess-shared", ProviderID: "claude", StepID: "review", StepName: "Review", StartedAt: childAt},
			},
		},
	}

	got, err := PreFetch(context.Background(), runDir, PreFetchInput{
		CWD:         cwd,
		ParentRunID: parentRunID,
	}, reader)
	if err != nil {
		t.Fatalf("PreFetch: %v", err)
	}

	wantTargetIDs := []string{"ac-parent-1", "ac-child-1", "ac-parent-2"}
	if !reflect.DeepEqual(reflectionTargetIDs(got.ReflectionTargets), wantTargetIDs) {
		t.Fatalf("ReflectionTargets IDs = %v, want %v", reflectionTargetIDs(got.ReflectionTargets), wantTargetIDs)
	}
	if len(got.ReflectionTargets) != 3 {
		t.Fatalf("ReflectionTargets len = %d, want 3", len(got.ReflectionTargets))
	}
	if got.ReflectionTargets[1].WorkflowRunID != "child-run" ||
		got.ReflectionTargets[1].SessionID != "sess-shared" ||
		got.ReflectionTargets[1].ProviderID != "claude" ||
		got.ReflectionTargets[1].StepID != "review" ||
		got.ReflectionTargets[1].StepName != "Review" {
		t.Fatalf("child target = %+v, want workflow/session/provider/step metadata", got.ReflectionTargets[1])
	}
	if !reflect.DeepEqual(got.UniqueSessionIDs, []string{"sess-shared"}) {
		t.Fatalf("UniqueSessionIDs = %v, want legacy session collapse to remain separate from target identity", got.UniqueSessionIDs)
	}
}

// TestPreFetchUsesProjectScopedReader は ProjectScopedReader 経由で post-filter が
// 効いていることを検証する (Req 3.6)。fake reader が cwd basename と一致しない
// project_id の row を返したとき、PreFetch 戻り値からは除外される。
//
// テスト構成:
//   - cwd basename = "rilll" → ProjectScopedReader.projectID = "rilll"
//   - reader が返す rows: {project_id="rilll", project_id="other-project", project_id="rilll"}
//   - post-filter で 2 行のみ残ることを検証
func TestPreFetchUsesProjectScopedReader(t *testing.T) {
	cwd := makeFixtureProjectCWD(t, "rilll")
	runDir := t.TempDir()

	t1 := time.Date(2026, 5, 8, 1, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 8, 1, 0, 1, 0, time.UTC)
	t3 := time.Date(2026, 5, 8, 1, 0, 2, 0, time.UTC)

	reader := &fakePrefetchReader{
		rows: []akashic.AgentCallRow{
			{ID: "ac-1", ProjectID: "rilll", SessionID: "sess-A", StartedAt: t1},
			// 別 project の row は post-filter で除外される
			{ID: "ac-2", ProjectID: "other-project", SessionID: "sess-OTHER", StartedAt: t2},
			{ID: "ac-3", ProjectID: "rilll", SessionID: "sess-B", StartedAt: t3},
		},
	}

	got, err := PreFetch(context.Background(), runDir, PreFetchInput{
		CWD:         cwd,
		ParentRunID: "20260508T010203Z-parent01",
	}, reader)
	if err != nil {
		t.Fatalf("PreFetch: %v", err)
	}

	wantIDs := []string{"sess-A", "sess-B"}
	if !reflect.DeepEqual(got.UniqueSessionIDs, wantIDs) {
		t.Errorf("UniqueSessionIDs = %v, want %v (other-project row should be filtered out)", got.UniqueSessionIDs, wantIDs)
	}

	// ProjectScopedReader が PreFetchInput.ParentRunID をそのまま渡していることを検証。
	if reader.gotWorkflowRunID != "20260508T010203Z-parent01" {
		t.Errorf("reader.gotWorkflowRunID = %q, want parent_run_id pass-through", reader.gotWorkflowRunID)
	}
	if reader.calls != 1 {
		t.Errorf("reader.calls = %d, want 1 (single AgentCallsByWorkflowRun call)", reader.calls)
	}
}

// TestPreFetchWritesSessionsJSONAtomically は sessions.json が
// `<runDir>/sessions.json` に atomic に書き出され、tmp ファイルが残らないことを
// 検証する (Req 3.5, 8.2 + writeJSONAtomic 契約)。
func TestPreFetchWritesSessionsJSONAtomically(t *testing.T) {
	cwd := makeFixtureProjectCWD(t, "rilll")
	runDir := t.TempDir()

	reader := &fakePrefetchReader{
		rows: []akashic.AgentCallRow{
			{ID: "ac-1", ProjectID: "rilll", SessionID: "sess-A", StartedAt: time.Date(2026, 5, 8, 1, 0, 0, 0, time.UTC)},
		},
	}

	got, err := PreFetch(context.Background(), runDir, PreFetchInput{
		CWD:         cwd,
		ParentRunID: "20260508T010203Z-parent01",
	}, reader)
	if err != nil {
		t.Fatalf("PreFetch: %v", err)
	}

	// SessionsJSONPath が <runDir>/sessions.json に一致する。
	wantPath := filepath.Join(runDir, sessionsFilename)
	if got.SessionsJSONPath != wantPath {
		t.Errorf("SessionsJSONPath = %q, want %q", got.SessionsJSONPath, wantPath)
	}
	// 実ファイルが存在し、`.tmp` ファイルが残っていない。
	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("sessions.json missing: %v", err)
	}
	if _, err := os.Stat(wantPath + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("sessions.json.tmp should not remain after atomic rename: err = %v", err)
	}

	// artifact body に GeneratedAt が non-zero RFC3339 で含まれる。
	artifact := readSessionsArtifact(t, wantPath)
	if artifact.GeneratedAt.IsZero() {
		t.Errorf("artifact.GeneratedAt is zero; want non-zero RFC3339 timestamp")
	}
}

// TestCollectUniqueSessionIDsPureFunction は内部 helper の純関数性を検証する。
// PreFetch の sessions.json 生成経路を経由せず dedup / skipped 振り分けのみを
// 単独で確認する (関数境界の単体検証)。
func TestCollectUniqueSessionIDsPureFunction(t *testing.T) {
	t.Run("nil rows returns empty non-nil slices", func(t *testing.T) {
		ids, sessions, skipped := collectUniqueSessionIDs(nil)
		if ids == nil || len(ids) != 0 {
			t.Errorf("uniqueIDs = %v, want empty non-nil", ids)
		}
		if sessions == nil || len(sessions) != 0 {
			t.Errorf("sessions = %v, want empty non-nil", sessions)
		}
		if skipped == nil || len(skipped) != 0 {
			t.Errorf("skipped = %v, want empty non-nil", skipped)
		}
	})

	t.Run("dedup and skip mix", func(t *testing.T) {
		rows := []akashic.AgentCallRow{
			{ID: "a1", SessionID: "s1"},
			{ID: "a2", SessionID: ""},
			{ID: "a3", SessionID: "s2"},
			{ID: "a4", SessionID: "s1"},
		}
		ids, sessions, skipped := collectUniqueSessionIDs(rows)
		if !reflect.DeepEqual(ids, []string{"s1", "s2"}) {
			t.Errorf("uniqueIDs = %v, want [s1 s2]", ids)
		}
		if len(sessions) != 2 {
			t.Fatalf("sessions len = %d, want 2", len(sessions))
		}
		if !reflect.DeepEqual(sessions[0].AgentCallIDs, []string{"a1", "a4"}) {
			t.Errorf("sessions[0].AgentCallIDs = %v, want [a1 a4]", sessions[0].AgentCallIDs)
		}
		if !reflect.DeepEqual(sessions[1].AgentCallIDs, []string{"a3"}) {
			t.Errorf("sessions[1].AgentCallIDs = %v, want [a3]", sessions[1].AgentCallIDs)
		}
		if len(skipped) != 1 || skipped[0].AgentCallID != "a2" {
			t.Errorf("skipped = %v, want single entry for a2", skipped)
		}
	})
}

func TestCollectReflectionTargetsPureFunctionKeepsSameSessionAgentCalls(t *testing.T) {
	rows := []akashic.AgentCallRow{
		{ID: "a1", WorkflowRunID: "parent-run", SessionID: "same-session", ProviderID: "codex", StepID: "step-a"},
		{ID: "a2", WorkflowRunID: "child-run", SessionID: "same-session", ProviderID: "codex", StepID: "step-b"},
	}

	got := collectReflectionTargets(rows)

	wantIDs := []string{"a1", "a2"}
	if !reflect.DeepEqual(reflectionTargetIDs(got), wantIDs) {
		t.Fatalf("collectReflectionTargets IDs = %v, want %v", reflectionTargetIDs(got), wantIDs)
	}
	if got[0].SessionID != got[1].SessionID {
		t.Fatalf("fixture setup invalid: sessions differ")
	}
	if got[0].AgentCallID == got[1].AgentCallID {
		t.Fatalf("targets collapsed by session: %+v", got)
	}
}

func reflectionTargetIDs(targets []ReflectionTarget) []string {
	out := make([]string, 0, len(targets))
	for _, target := range targets {
		out = append(out, target.AgentCallID)
	}
	return out
}
