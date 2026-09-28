// Package akashic の reader_test.go は、Reader API (List / Aggregate / Detail /
// FailureClusters) の包括的な table-driven テストを提供する。
//
// 本ファイルの責務 (タスク 4.1 で List 部分、タスク 4.2 で AggregateByWorkflow 部分を担当):
//   - openTestReader / fixture insert ヘルパを定義し、後続タスク (4.3〜4.5) と共有する
//   - List API のフィルタ / ページネーション / 安定ソート / 検証ゲート / 冪等性を網羅検証する
//   - AggregateByWorkflow API の SuccessRate / AvgCostUSD (NULL 除外) /
//     P50DurationMS / P95DurationMS / SumInputTokens / 範囲外 nil / 冪等性を検証する
//
// 既存 store_test.go (Recorder の書き込みテスト) を破壊しないため、本ファイルは
// 純粋な追加であり、共通シンボル (TestXxx) は openTestReader / insertListRun /
// insertListAgentCall / aggregateAgentCallSpec / aggregateRunSpec /
// insertAggregateWorkflowRun の各ヘルパ + List/AggregateByWorkflow 系テスト関数のみに限定する。
//
// 既存 reader_sqlite_test.go (各タスク 2.x の companion smoke test) と同パッケージで
// 共存するためシンボル名は重複させず、List 系テストはすべて Test"Reader"List"..." の
// プレフィックスに、Aggregate 系テストはすべて Test"Reader"AggregateByWorkflow"..." の
// プレフィックスに揃えて `go test -run TestReaderList` / `... TestReaderAggregateByWorkflow`
// で抽出可能にする。
package akashic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// fixture helpers
// -----------------------------------------------------------------------------

// openTestReader は t.TempDir() 上に SQLite ベース akashic データベースを構築し、
// テスト終了時に自動クローズする Reader (= *SQLiteRecorder) を返すヘルパ。
//
// concrete *SQLiteRecorder を返す理由:
//   - テスト内で fixture 投入のために Recorder メソッドも併用するため
//     (Recorder と Reader は同一 *SQLiteRecorder が両方を満たす設計)
//   - akashic.Reader interface への型 assertion を強制するテストを書きたい場合は
//     呼び出し側で `var _ akashic.Reader = recorder` を書けば十分
func openTestReader(t *testing.T) *SQLiteRecorder {
	t.Helper()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	rec, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite(%s): %v", path, err)
	}
	t.Cleanup(func() {
		_ = rec.Close()
	})
	return rec
}

// listRunSpec は List テスト用 workflow_run 1 件のテストフィクスチャ仕様。
//
// Finish が nil なら未完了 (finished_at IS NULL → Success=nil)。
// Finish.Error が空文字なら成功 (finished_at NOT NULL かつ error NULL → Success=&true)。
// Finish.Error が非空なら失敗 (finished_at NOT NULL かつ error 非 NULL → Success=&false)。
type listRunSpec struct {
	ID         string
	WorkflowID string
	CWD        string
	StartedAt  time.Time
	Finish     *listRunFinishSpec
}

type listRunFinishSpec struct {
	Error      string
	FinishedAt time.Time
}

// insertListRun は List テスト用の workflow_run を Recorder API 経由で投入する。
//
// WorkflowName は WorkflowID と同じ値を埋める (List 結果の検証では WorkflowName を
// 直接 assert しないため、衝突回避と最小限のセットアップを優先)。
func insertListRun(t *testing.T, ctx context.Context, recorder *SQLiteRecorder, spec listRunSpec) {
	t.Helper()
	if err := recorder.RecordWorkflowRunStart(ctx, WorkflowRun{
		ID:           spec.ID,
		WorkflowID:   spec.WorkflowID,
		WorkflowName: spec.WorkflowID,
		CWD:          spec.CWD,
		StartedAt:    spec.StartedAt,
	}); err != nil {
		t.Fatalf("RecordWorkflowRunStart %s: %v", spec.ID, err)
	}
	if spec.Finish != nil {
		if err := recorder.RecordWorkflowRunFinish(ctx, WorkflowRunFinish{
			ID:         spec.ID,
			Error:      spec.Finish.Error,
			FinishedAt: spec.Finish.FinishedAt,
		}); err != nil {
			t.Fatalf("RecordWorkflowRunFinish %s: %v", spec.ID, err)
		}
	}
}

// listAgentCallSpec は List テストで provider フィルタ等を検証するための agent_call
// フィクスチャ仕様。WorkflowID は agent_calls.workflow_id にも denormalize される
// (Recorder API に一括で投入される)。
type listAgentCallSpec struct {
	ID            string
	WorkflowRunID string
	WorkflowID    string
	ProviderID    string
	StartedAt     time.Time
}

// insertListAgentCall は List テスト用の agent_call を Recorder API 経由で投入する。
// step_id / step_name / cwd は固定値 ("s1" / "S1" / "/repo") を埋める (List 結果の
// 検証では agent_call サマリの provider_id / id を中心に assert するため)。
func insertListAgentCall(t *testing.T, ctx context.Context, recorder *SQLiteRecorder, spec listAgentCallSpec) {
	t.Helper()
	if err := recorder.RecordAgentCallStart(ctx, AgentCall{
		ID:            spec.ID,
		WorkflowRunID: spec.WorkflowRunID,
		WorkflowID:    spec.WorkflowID,
		WorkflowName:  spec.WorkflowID,
		StepID:        "s1",
		StepName:      "S1",
		ProviderID:    spec.ProviderID,
		CWD:           "/repo",
		StartedAt:     spec.StartedAt,
	}); err != nil {
		t.Fatalf("RecordAgentCallStart %s: %v", spec.ID, err)
	}
}

// runIDsFromList は ListResult から RunID 列を抽出するヘルパ (table-driven 比較用)。
func runIDsFromList(items []WorkflowRunListItem) []string {
	if len(items) == 0 {
		// 空集合は nil として扱うことで、テストの期待値 (nil) と
		// reflect.DeepEqual が一致するようにする。
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.RunID)
	}
	return out
}

// -----------------------------------------------------------------------------
// TestReaderListFilters: フィルタ各種 (workflow_id / since-until / success /
// provider / cwd) の table-driven test (Req 2.1〜2.5)
// -----------------------------------------------------------------------------

// TestReaderListFiltersWorkflowID は workflow_id フィルタが該当 workflow_id の
// workflow_runs のみを返すことを検証する (Req 2.1)。
//
// fixture: 2 つの workflow_id (wfA, wfB) を持つ 4 件の run。
func TestReaderListFiltersWorkflowID(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	runs := []listRunSpec{
		{ID: "run-A1", WorkflowID: "wfA", CWD: "/repo", StartedAt: base.Add(1 * time.Minute)},
		{ID: "run-A2", WorkflowID: "wfA", CWD: "/repo", StartedAt: base.Add(2 * time.Minute)},
		{ID: "run-B1", WorkflowID: "wfB", CWD: "/repo", StartedAt: base.Add(3 * time.Minute)},
		{ID: "run-B2", WorkflowID: "wfB", CWD: "/repo", StartedAt: base.Add(4 * time.Minute)},
	}
	for _, r := range runs {
		insertListRun(t, ctx, recorder, r)
	}

	cases := []struct {
		name       string
		workflowID string
		wantIDs    []string // started_at DESC 順
	}{
		{"all (empty workflow_id sentinel)", "", []string{"run-B2", "run-B1", "run-A2", "run-A1"}},
		{"workflow wfA only", "wfA", []string{"run-A2", "run-A1"}},
		{"workflow wfB only", "wfB", []string{"run-B2", "run-B1"}},
		{"unknown workflow returns empty", "wfMissing", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, err := recorder.List(ctx, ListOptions{WorkflowID: c.workflowID, Limit: 100})
			if err != nil {
				t.Fatalf("List(workflow=%q): %v", c.workflowID, err)
			}
			gotIDs := runIDsFromList(result.Items)
			if !reflect.DeepEqual(gotIDs, c.wantIDs) {
				t.Errorf("RunIDs = %v, want %v", gotIDs, c.wantIDs)
			}
		})
	}
}

// TestReaderListFiltersTimeRange は since / until フィルタが期間内の workflow_runs
// のみを返すことを検証する (Req 2.2)。両端は inclusive。
func TestReaderListFiltersTimeRange(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	t0 := base
	t1 := base.Add(1 * time.Minute)
	t2 := base.Add(2 * time.Minute)
	t3 := base.Add(3 * time.Minute)
	runs := []listRunSpec{
		{ID: "run-t0", WorkflowID: "wf", CWD: "/repo", StartedAt: t0},
		{ID: "run-t1", WorkflowID: "wf", CWD: "/repo", StartedAt: t1},
		{ID: "run-t2", WorkflowID: "wf", CWD: "/repo", StartedAt: t2},
		{ID: "run-t3", WorkflowID: "wf", CWD: "/repo", StartedAt: t3},
	}
	for _, r := range runs {
		insertListRun(t, ctx, recorder, r)
	}

	cases := []struct {
		name    string
		since   time.Time
		until   time.Time
		wantIDs []string
	}{
		{"no range (all 4)", time.Time{}, time.Time{}, []string{"run-t3", "run-t2", "run-t1", "run-t0"}},
		{"since=t1 (inclusive lower)", t1, time.Time{}, []string{"run-t3", "run-t2", "run-t1"}},
		{"until=t2 (inclusive upper)", time.Time{}, t2, []string{"run-t2", "run-t1", "run-t0"}},
		{"since=t1 & until=t2 (closed range)", t1, t2, []string{"run-t2", "run-t1"}},
		{"since=t3 (only newest)", t3, time.Time{}, []string{"run-t3"}},
		{"until=t0 (only oldest)", time.Time{}, t0, []string{"run-t0"}},
		{"empty range (since > until)", t3.Add(time.Minute), t3.Add(2 * time.Minute), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, err := recorder.List(ctx, ListOptions{
				Range: TimeRange{Since: c.since, Until: c.until},
				Limit: 100,
			})
			if err != nil {
				t.Fatalf("List(since=%v, until=%v): %v", c.since, c.until, err)
			}
			gotIDs := runIDsFromList(result.Items)
			if !reflect.DeepEqual(gotIDs, c.wantIDs) {
				t.Errorf("RunIDs = %v, want %v", gotIDs, c.wantIDs)
			}
		})
	}
}

// TestReaderListFiltersSuccess は success フラグ (*bool) が
// workflow_runs.error / finished_at から派生する success 状態に基づいて絞り込まれる
// ことを検証する (Req 2.3)。
//
// SQL の派生定義:
//
//	error IS NULL AND finished_at IS NOT NULL → success
//	otherwise (failure or in-progress)        → not success
//
// 本テストは挙動を明示するため、in-progress (finished_at NULL) を含めず、
// 「成功 1 件 + 失敗 1 件」だけで filter の効きを assert する。
func TestReaderListFiltersSuccess(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	insertListRun(t, ctx, recorder, listRunSpec{
		ID: "run-success", WorkflowID: "wf", CWD: "/repo", StartedAt: base.Add(1 * time.Minute),
		Finish: &listRunFinishSpec{Error: "", FinishedAt: base.Add(1*time.Minute + 30*time.Second)},
	})
	insertListRun(t, ctx, recorder, listRunSpec{
		ID: "run-failure", WorkflowID: "wf", CWD: "/repo", StartedAt: base.Add(2 * time.Minute),
		Finish: &listRunFinishSpec{Error: "boom", FinishedAt: base.Add(2*time.Minute + 30*time.Second)},
	})

	successTrue := true
	successFalse := false
	cases := []struct {
		name    string
		success *bool
		wantIDs []string
	}{
		{"nil → both", nil, []string{"run-failure", "run-success"}},
		{"&true → success only", &successTrue, []string{"run-success"}},
		{"&false → failure only (本フィクスチャは in-progress を持たない)", &successFalse, []string{"run-failure"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, err := recorder.List(ctx, ListOptions{Success: c.success, Limit: 100})
			if err != nil {
				t.Fatalf("List(success=%v): %v", c.success, err)
			}
			gotIDs := runIDsFromList(result.Items)
			if !reflect.DeepEqual(gotIDs, c.wantIDs) {
				t.Errorf("RunIDs = %v, want %v", gotIDs, c.wantIDs)
			}
		})
	}
}

// TestReaderListFiltersProvider は provider フィルタが「該当 provider の agent_call を
// 1 件以上含む workflow_runs のみ」を返すことを検証する (Req 2.4)。
//
// fixture:
//   - run-claude: agent_call provider=claude が紐づく
//   - run-codex:  agent_call provider=codex が紐づく
//   - run-empty:  agent_call なし
func TestReaderListFiltersProvider(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	insertListRun(t, ctx, recorder, listRunSpec{ID: "run-claude", WorkflowID: "wf", CWD: "/repo", StartedAt: base.Add(1 * time.Minute)})
	insertListRun(t, ctx, recorder, listRunSpec{ID: "run-codex", WorkflowID: "wf", CWD: "/repo", StartedAt: base.Add(2 * time.Minute)})
	insertListRun(t, ctx, recorder, listRunSpec{ID: "run-empty", WorkflowID: "wf", CWD: "/repo", StartedAt: base.Add(3 * time.Minute)})

	insertListAgentCall(t, ctx, recorder, listAgentCallSpec{
		ID: "ac-claude", WorkflowRunID: "run-claude", WorkflowID: "wf",
		ProviderID: "claude", StartedAt: base.Add(1*time.Minute + time.Second),
	})
	insertListAgentCall(t, ctx, recorder, listAgentCallSpec{
		ID: "ac-codex", WorkflowRunID: "run-codex", WorkflowID: "wf",
		ProviderID: "codex", StartedAt: base.Add(2*time.Minute + time.Second),
	})

	cases := []struct {
		name       string
		providerID string
		wantIDs    []string
	}{
		{"all (provider sentinel empty)", "", []string{"run-empty", "run-codex", "run-claude"}},
		{"claude only", "claude", []string{"run-claude"}},
		{"codex only", "codex", []string{"run-codex"}},
		{"unknown provider returns empty", "missing", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, err := recorder.List(ctx, ListOptions{ProviderID: c.providerID, Limit: 100})
			if err != nil {
				t.Fatalf("List(provider=%q): %v", c.providerID, err)
			}
			gotIDs := runIDsFromList(result.Items)
			if !reflect.DeepEqual(gotIDs, c.wantIDs) {
				t.Errorf("RunIDs = %v, want %v", gotIDs, c.wantIDs)
			}
		})
	}
}

// TestReaderListFiltersCWD は cwd フィルタが該当 cwd で実行された workflow_runs のみを
// 返すことを検証する (Req 2.5)。
func TestReaderListFiltersCWD(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	runs := []listRunSpec{
		{ID: "run-A1", WorkflowID: "wf", CWD: "/repoA", StartedAt: base.Add(1 * time.Minute)},
		{ID: "run-B1", WorkflowID: "wf", CWD: "/repoB", StartedAt: base.Add(2 * time.Minute)},
		{ID: "run-A2", WorkflowID: "wf", CWD: "/repoA", StartedAt: base.Add(3 * time.Minute)},
	}
	for _, r := range runs {
		insertListRun(t, ctx, recorder, r)
	}

	cases := []struct {
		name    string
		cwd     string
		wantIDs []string
	}{
		{"all (cwd sentinel empty)", "", []string{"run-A2", "run-B1", "run-A1"}},
		{"/repoA only", "/repoA", []string{"run-A2", "run-A1"}},
		{"/repoB only", "/repoB", []string{"run-B1"}},
		{"unknown cwd returns empty", "/missing", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, err := recorder.List(ctx, ListOptions{CWD: c.cwd, Limit: 100})
			if err != nil {
				t.Fatalf("List(cwd=%q): %v", c.cwd, err)
			}
			gotIDs := runIDsFromList(result.Items)
			if !reflect.DeepEqual(gotIDs, c.wantIDs) {
				t.Errorf("RunIDs = %v, want %v", gotIDs, c.wantIDs)
			}
		})
	}
}

// TestReaderListFiltersCombination は複数フィルタを同時指定した時に
// AND 条件として効くことを検証する (Req 2.1〜2.5 の組み合わせ)。
//
// fixture:
//   - run-A-claude: workflow=wfA / cwd=/repoA / provider=claude
//   - run-A-codex:  workflow=wfA / cwd=/repoA / provider=codex
//   - run-B-claude: workflow=wfB / cwd=/repoB / provider=claude
//
// workflow=wfA & provider=claude → run-A-claude のみ (workflow=wfA で run-B-claude が外れ、
// provider=claude で run-A-codex が外れる)。
func TestReaderListFiltersCombination(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	insertListRun(t, ctx, recorder, listRunSpec{ID: "run-A-claude", WorkflowID: "wfA", CWD: "/repoA", StartedAt: base.Add(1 * time.Minute)})
	insertListRun(t, ctx, recorder, listRunSpec{ID: "run-A-codex", WorkflowID: "wfA", CWD: "/repoA", StartedAt: base.Add(2 * time.Minute)})
	insertListRun(t, ctx, recorder, listRunSpec{ID: "run-B-claude", WorkflowID: "wfB", CWD: "/repoB", StartedAt: base.Add(3 * time.Minute)})

	insertListAgentCall(t, ctx, recorder, listAgentCallSpec{
		ID: "ac-A-claude", WorkflowRunID: "run-A-claude", WorkflowID: "wfA",
		ProviderID: "claude", StartedAt: base.Add(1*time.Minute + time.Second),
	})
	insertListAgentCall(t, ctx, recorder, listAgentCallSpec{
		ID: "ac-A-codex", WorkflowRunID: "run-A-codex", WorkflowID: "wfA",
		ProviderID: "codex", StartedAt: base.Add(2*time.Minute + time.Second),
	})
	insertListAgentCall(t, ctx, recorder, listAgentCallSpec{
		ID: "ac-B-claude", WorkflowRunID: "run-B-claude", WorkflowID: "wfB",
		ProviderID: "claude", StartedAt: base.Add(3*time.Minute + time.Second),
	})

	result, err := recorder.List(ctx, ListOptions{
		WorkflowID: "wfA",
		ProviderID: "claude",
		CWD:        "/repoA",
		Limit:      100,
	})
	if err != nil {
		t.Fatalf("List(combined): %v", err)
	}
	gotIDs := runIDsFromList(result.Items)
	wantIDs := []string{"run-A-claude"}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Errorf("RunIDs = %v, want %v (workflow=wfA AND provider=claude AND cwd=/repoA)", gotIDs, wantIDs)
	}
}

// -----------------------------------------------------------------------------
// TestReaderListPagination: 30 件 fixture でページネーション + 安定ソート (Req 2.6,
// 2.8〜2.11, 7.1, 7.3)
// -----------------------------------------------------------------------------

// TestReaderListPaginationStableSort は 30 件の workflow_run (started_at が秒単位で散らばる)
// に対して Limit=10 で 3 ページに分けて取得した結果が
//
//   - started_at DESC 順で並ぶ (Req 2.6)
//   - 各ページに重複・抜け漏れがない (Req 2.8 / 7.3)
//   - len(Items) == Limit のとき NextOffset = &(Offset + Limit) (Req 2.8)
//   - 最終ページ (len(Items) < Limit) で NextOffset = nil
//
// を満たすことを検証する。
func TestReaderListPaginationStableSort(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	for i := range 30 {
		runID := fmt.Sprintf("page-run-%02d", i)
		insertListRun(t, ctx, recorder, listRunSpec{
			ID: runID, WorkflowID: "wf", CWD: "/repo",
			StartedAt: base.Add(time.Duration(i) * time.Second),
		})
	}

	// 期待ページ列: page-run-29, ..., page-run-00 を 3 ページ (10 + 10 + 10) に切る。
	wantPages := [][]string{
		makeDescIDRange("page-run-", 29, 20),
		makeDescIDRange("page-run-", 19, 10),
		makeDescIDRange("page-run-", 9, 0),
	}

	collectedIDs := make([]string, 0, 30)
	for page, wantIDs := range wantPages {
		offset := page * 10
		result, err := recorder.List(ctx, ListOptions{Limit: 10, Offset: offset})
		if err != nil {
			t.Fatalf("List(page=%d, offset=%d): %v", page, offset, err)
		}
		gotIDs := runIDsFromList(result.Items)
		if !reflect.DeepEqual(gotIDs, wantIDs) {
			t.Errorf("page %d: RunIDs = %v, want %v", page, gotIDs, wantIDs)
		}
		// Limit / Offset が結果メタに反映される。
		if result.Limit != 10 {
			t.Errorf("page %d: result.Limit = %d, want 10", page, result.Limit)
		}
		if result.Offset != offset {
			t.Errorf("page %d: result.Offset = %d, want %d", page, result.Offset, offset)
		}
		// 全 3 ページとも len==Limit なので NextOffset は常に &(Offset+Limit)。
		// 30 件ぴったりの場合、最終ページ (offset=20, len=10) も NextOffset=&30 を返す
		// (実装契約: len(Items) == Limit のとき次オフセットを返す)。
		if result.NextOffset == nil {
			t.Errorf("page %d: NextOffset = nil, want &%d (len(Items)==Limit)", page, offset+10)
		} else if *result.NextOffset != offset+10 {
			t.Errorf("page %d: *NextOffset = %d, want %d", page, *result.NextOffset, offset+10)
		}
		collectedIDs = append(collectedIDs, gotIDs...)
	}

	// 重複・抜け漏れ確認 (全 30 件が 1 度ずつ現れる)。
	if len(collectedIDs) != 30 {
		t.Fatalf("collected %d items across 3 pages, want 30", len(collectedIDs))
	}
	seen := make(map[string]bool, 30)
	for _, id := range collectedIDs {
		if seen[id] {
			t.Errorf("duplicate RunID across pages: %s", id)
		}
		seen[id] = true
	}

	// 最終ページの「次に進んだ空ページ」: offset=30 で 0 件 + NextOffset=nil。
	tail, err := recorder.List(ctx, ListOptions{Limit: 10, Offset: 30})
	if err != nil {
		t.Fatalf("List(offset=30): %v", err)
	}
	if len(tail.Items) != 0 {
		t.Errorf("len(Items) at offset=30 = %d, want 0", len(tail.Items))
	}
	if tail.NextOffset != nil {
		t.Errorf("NextOffset at offset=30 = %v, want nil", tail.NextOffset)
	}
}

// TestReaderListPaginationIDTiebreaker は同一 started_at の workflow_run が
// id DESC 順で並ぶことを検証する (Req 2.6 / 7.3 安定ソート tiebreak)。
//
// 同 started_at の 3 つの run (id: page-tie-A / page-tie-B / page-tie-C) を投入し、
// id DESC 順 (C → B → A) で返ることを assert する。
func TestReaderListPaginationIDTiebreaker(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	tieAt := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"page-tie-A", "page-tie-B", "page-tie-C"} {
		insertListRun(t, ctx, recorder, listRunSpec{
			ID: id, WorkflowID: "wf", CWD: "/repo", StartedAt: tieAt,
		})
	}

	result, err := recorder.List(ctx, ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	gotIDs := runIDsFromList(result.Items)
	wantIDs := []string{"page-tie-C", "page-tie-B", "page-tie-A"}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Errorf("RunIDs = %v, want %v (id DESC tiebreak)", gotIDs, wantIDs)
	}
}

// TestReaderListPaginationDefaultLimit は Limit==0 のとき DefaultListLimit が
// 適用されることを検証する (Req 2.9)。
//
// fixture: 1 件のみ → Limit に補完された値より少ないため NextOffset=nil。
func TestReaderListPaginationDefaultLimit(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	insertListRun(t, ctx, recorder, listRunSpec{
		ID: "page-default", WorkflowID: "wf", CWD: "/repo", StartedAt: base,
	})

	result, err := recorder.List(ctx, ListOptions{Limit: 0, Offset: 0})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if result.Limit != DefaultListLimit {
		t.Errorf("result.Limit = %d, want DefaultListLimit (%d)", result.Limit, DefaultListLimit)
	}
	if len(result.Items) != 1 {
		t.Errorf("len(Items) = %d, want 1", len(result.Items))
	}
	if result.NextOffset != nil {
		t.Errorf("NextOffset = %v, want nil (len(Items) < Limit)", result.NextOffset)
	}
}

// -----------------------------------------------------------------------------
// TestReaderListValidation: 負値 Limit / Offset / Limit > MaxListLimit の sentinel
// error 判定 (Req 2.10 / 7.4)
// -----------------------------------------------------------------------------

// TestReaderListValidationRejectsBadOptions は ListOptions.Validate ゲートが
// 負値 / 上限超過の Limit / 負値 Offset を sentinel error で拒否することを検証する。
//
// errors.Is で sentinel に到達できることを assert (呼び出し側がエラー種別を
// プログラムから判別できる契約)。
func TestReaderListValidationRejectsBadOptions(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	cases := []struct {
		name    string
		opts    ListOptions
		wantErr error
	}{
		{"negative limit", ListOptions{Limit: -1}, ErrInvalidLimit},
		{"limit just over max", ListOptions{Limit: MaxListLimit + 1}, ErrInvalidLimit},
		{"negative offset", ListOptions{Offset: -1}, ErrInvalidOffset},
		{
			name:    "negative limit takes precedence over negative offset",
			opts:    ListOptions{Limit: -1, Offset: -1},
			wantErr: ErrInvalidLimit,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := recorder.List(ctx, c.opts)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("List(%+v) error = %v, want errors.Is(%v)", c.opts, err, c.wantErr)
			}
		})
	}
}

// TestReaderListValidationAcceptsBoundaryLimits は境界値 (Limit=MaxListLimit /
// Limit=1 / Offset=0) が validation を通過することを補強的に検証する。
//
// MaxListLimit ぴったりはエラーにせず、超過のみエラー (Req 2.10 の "上限超過"
// 解釈の明示)。fixture を投入しないため空結果でも構わない。
func TestReaderListValidationAcceptsBoundaryLimits(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	cases := []struct {
		name string
		opts ListOptions
	}{
		{"limit exactly MaxListLimit", ListOptions{Limit: MaxListLimit}},
		{"limit = 1, offset = 0", ListOptions{Limit: 1, Offset: 0}},
		{"limit = 0 (default applied)", ListOptions{Limit: 0, Offset: 0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := recorder.List(ctx, c.opts); err != nil {
				t.Fatalf("List(%+v) unexpected error: %v", c.opts, err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// TestReaderListIdempotent: 同一 (filter, limit, offset) 二回呼び出しで結果が
// 完全一致することを検証する (Req 2.11 / 7.3)
// -----------------------------------------------------------------------------

// TestReaderListIdempotentSameOpts は同一 (filter, limit, offset) を 2 回呼び出した時に
// ListResult が並び順を含めて完全に一致することを検証する (Req 2.11)。
//
// fixture: 5 件の workflow_run (workflow_id / cwd / started_at がバラバラ) +
// 一部 run に agent_call を紐づける。reflect.DeepEqual で構造的等価を判定する。
//
// 等価判定の根拠:
//   - parseStoredTime が time.Parse + .UTC() で monotonic clock を持たない time.Time を返す
//     (loc=*time.UTC で揃うため、2 回の SQL 結果から構築される ListResult は構造的に等価)
//   - NextOffset は *int (ポインタ経由の値比較は reflect.DeepEqual が再帰展開する)
//   - AgentCalls は []AgentCallSummary。順序も SQL の ORDER BY で安定化済み
func TestReaderListIdempotentSameOpts(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	runs := []listRunSpec{
		{ID: "idem-1", WorkflowID: "wfA", CWD: "/repoA", StartedAt: base.Add(1 * time.Minute)},
		{ID: "idem-2", WorkflowID: "wfA", CWD: "/repoA", StartedAt: base.Add(2 * time.Minute),
			Finish: &listRunFinishSpec{Error: "", FinishedAt: base.Add(2*time.Minute + 30*time.Second)}},
		{ID: "idem-3", WorkflowID: "wfB", CWD: "/repoB", StartedAt: base.Add(3 * time.Minute),
			Finish: &listRunFinishSpec{Error: "boom", FinishedAt: base.Add(3*time.Minute + 30*time.Second)}},
		{ID: "idem-4", WorkflowID: "wfA", CWD: "/repoA", StartedAt: base.Add(4 * time.Minute)},
		{ID: "idem-5", WorkflowID: "wfB", CWD: "/repoA", StartedAt: base.Add(5 * time.Minute)},
	}
	for _, r := range runs {
		insertListRun(t, ctx, recorder, r)
	}
	insertListAgentCall(t, ctx, recorder, listAgentCallSpec{
		ID: "ac-idem-1a", WorkflowRunID: "idem-1", WorkflowID: "wfA",
		ProviderID: "claude", StartedAt: base.Add(1*time.Minute + time.Second),
	})
	insertListAgentCall(t, ctx, recorder, listAgentCallSpec{
		ID: "ac-idem-1b", WorkflowRunID: "idem-1", WorkflowID: "wfA",
		ProviderID: "codex", StartedAt: base.Add(1*time.Minute + 2*time.Second),
	})
	insertListAgentCall(t, ctx, recorder, listAgentCallSpec{
		ID: "ac-idem-3", WorkflowRunID: "idem-3", WorkflowID: "wfB",
		ProviderID: "claude", StartedAt: base.Add(3*time.Minute + time.Second),
	})

	cases := []struct {
		name string
		opts ListOptions
	}{
		{"no filter, default limit", ListOptions{}},
		{"workflow=wfA + limit=2 + offset=1", ListOptions{WorkflowID: "wfA", Limit: 2, Offset: 1}},
		{"cwd=/repoA + limit=10", ListOptions{CWD: "/repoA", Limit: 10}},
		{"provider=claude + limit=10", ListOptions{ProviderID: "claude", Limit: 10}},
		{"since=t2 + limit=10", ListOptions{Range: TimeRange{Since: base.Add(2 * time.Minute)}, Limit: 10}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			first, err := recorder.List(ctx, c.opts)
			if err != nil {
				t.Fatalf("List #1: %v", err)
			}
			second, err := recorder.List(ctx, c.opts)
			if err != nil {
				t.Fatalf("List #2: %v", err)
			}
			if !reflect.DeepEqual(first, second) {
				t.Errorf("List 二回呼び出しで結果が異なる:\n#1 = %+v\n#2 = %+v", first, second)
			}
			// 並び順の一致を補強的に明示 (DeepEqual に含まれるが、失敗時の
			// 診断性を上げるために RunID 列を比較)。
			firstIDs := runIDsFromList(first.Items)
			secondIDs := runIDsFromList(second.Items)
			if !reflect.DeepEqual(firstIDs, secondIDs) {
				t.Errorf("RunID 並び順が異なる:\n#1 = %v\n#2 = %v", firstIDs, secondIDs)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// AggregateByWorkflow テスト用 fixture ヘルパ (タスク 4.2)
//
// 既存 reader_sqlite_test.go の `insertAggregateWorkflowFixture` は cost=0.01 / duration=100
// の固定値を埋めるため、p50/p95 の線形補間や AvgCost の NULL 除外を検証できない。
// 本セクションでは run / agent_call ごとに individual な値 (cost / tokens / duration) を
// 駆動できる柔軟なヘルパを提供する。
// -----------------------------------------------------------------------------

// aggregateAgentCallSpec は AggregateByWorkflow / AggregateByProvider テスト用
// agent_call 1 件のフィクスチャ仕様。
//
// TotalCostUSD / InputTokens / OutputTokens は sql.Null* 型で受けることで、
// Valid=false (= NULL) の挙動 (Req 3.7: NULL 除外) を駆動できるようにする。
// DurationMS は非ポインタ (常に値あり) として扱う — 実運用でも RecordAgentCallFinish は
// duration_ms を必ず数値で記録するため、percentile テストでは既知配列を渡す前提。
//
// ProviderID は AggregateByProvider テスト (タスク 4.3) で provider 単位集計を駆動するため
// 後付けで追加した。空文字のときは insertAggregateWorkflowRun 側で既定値 "claude" に
// 補完するため、既存 AggregateByWorkflow テスト (タスク 4.2) のフィクスチャ仕様
// (ProviderID 未指定) は無変更で動作し続ける。
type aggregateAgentCallSpec struct {
	ID           string
	ProviderID   string // 空文字 → insertAggregateWorkflowRun が "claude" を埋める (4.2 互換)
	StartedAt    time.Time
	FinishedAt   time.Time
	DurationMS   int64
	Success      bool            // agent_calls.success に直接反映 (true=1 / false=0)
	TotalCostUSD sql.NullFloat64 // Valid=false → NULL を保存
	InputTokens  sql.NullInt64
	OutputTokens sql.NullInt64
}

// aggregateRunSpec は AggregateByWorkflow テスト用 workflow_run 1 件のフィクスチャ仕様。
//
// Failed=true なら RecordWorkflowRunFinish で Error="boom" を埋め、
// workflow_runs.error/finished_at から派生する Success=false ステートを再現する。
// Failed=false なら Error 空文字 → Success=true。
//
// Calls は同 run に紐づく agent_call 群。集計は agent_calls 全行を走査するため、
// run の成功/失敗とは独立に cost/tokens/duration が積み上がる仕様であることを
// 駆動するため、Failed=true の run にも非 NULL の cost/tokens を持つ Call を載せられる
// 設計とする。
type aggregateRunSpec struct {
	RunID     string
	StartedAt time.Time
	Failed    bool
	Calls     []aggregateAgentCallSpec
}

// insertAggregateWorkflowRun は aggregateRunSpec を Recorder API 経由で投入する。
//
// WorkflowName は workflowID と同値、CWD は固定値 "/repo" を埋める (集計テストでは
// 直接 assert しないため最小限のセットアップで十分)。
//
// Recorder API の都合で、agent_call の cost / tokens / duration を埋めるためには
// RecordAgentCallStart → RecordAgentCallFinish の 2 段階呼び出しが必要。Finish 側で
// success / duration_ms / total_cost_usd / input_tokens / output_tokens を更新する。
func insertAggregateWorkflowRun(t *testing.T, ctx context.Context, recorder *SQLiteRecorder, workflowID string, spec aggregateRunSpec) {
	t.Helper()

	// 1. workflow_run の Start (started_at は spec から、CWD は固定)。
	if err := recorder.RecordWorkflowRunStart(ctx, WorkflowRun{
		ID:           spec.RunID,
		WorkflowID:   workflowID,
		WorkflowName: workflowID,
		CWD:          "/repo",
		StartedAt:    spec.StartedAt,
	}); err != nil {
		t.Fatalf("RecordWorkflowRunStart %s: %v", spec.RunID, err)
	}

	// 2. workflow_run の Finish。Failed=true の run は error="boom" を埋めて Success=false にする。
	finishError := ""
	if spec.Failed {
		finishError = "boom"
	}
	if err := recorder.RecordWorkflowRunFinish(ctx, WorkflowRunFinish{
		ID:         spec.RunID,
		Error:      finishError,
		FinishedAt: spec.StartedAt.Add(time.Minute),
	}); err != nil {
		t.Fatalf("RecordWorkflowRunFinish %s: %v", spec.RunID, err)
	}

	// 3. agent_calls の Start → Finish を spec 件数ぶん繰り返す。
	//    StepID / StepName / CWD は固定値 (集計対象外のため最小限)。
	//    ProviderID は call.ProviderID を尊重し、未指定 (空文字) なら "claude" に補完する。
	//    これにより 4.2 (AggregateByWorkflow) のフィクスチャは無変更で claude 単一プロバイダ
	//    として振る舞いつつ、4.3 (AggregateByProvider) では複数プロバイダの混在を駆動できる。
	for _, call := range spec.Calls {
		providerID := call.ProviderID
		if providerID == "" {
			providerID = "claude"
		}
		if err := recorder.RecordAgentCallStart(ctx, AgentCall{
			ID:            call.ID,
			WorkflowRunID: spec.RunID,
			WorkflowID:    workflowID,
			WorkflowName:  workflowID,
			StepID:        "s1",
			StepName:      "S1",
			ProviderID:    providerID,
			CWD:           "/repo",
			StartedAt:     call.StartedAt,
		}); err != nil {
			t.Fatalf("RecordAgentCallStart %s: %v", call.ID, err)
		}
		if err := recorder.RecordAgentCallFinish(ctx, AgentCallResult{
			ID:           call.ID,
			SessionID:    "sess",
			Success:      call.Success,
			DurationMS:   call.DurationMS,
			TotalCostUSD: call.TotalCostUSD,
			InputTokens:  call.InputTokens,
			OutputTokens: call.OutputTokens,
			FinishedAt:   call.FinishedAt,
		}); err != nil {
			t.Fatalf("RecordAgentCallFinish %s: %v", call.ID, err)
		}
	}
}

// nullF は sql.NullFloat64{Valid:true} のショートカット (リテラル可読性向上)。
func nullF(v float64) sql.NullFloat64 { return sql.NullFloat64{Float64: v, Valid: true} }

// nullI は sql.NullInt64{Valid:true} のショートカット。
func nullI(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

// -----------------------------------------------------------------------------
// TestReaderAggregateByWorkflowMixedRuns: 成功 / 失敗 / NULL cost / 既知 duration 配列を
// 含む fixture で SuccessRate / AvgCostUSD (NULL 除外) / P50DurationMS / P95DurationMS /
// SumInputTokens / SumOutputTokens を厳密値で assert する (Req 3.1, 3.2, 3.3, 3.4, 3.7)。
// -----------------------------------------------------------------------------

// TestReaderAggregateByWorkflowMixedRuns は workflow "wf-mix" 配下に 5 run × 5 agent_call の
// fixture を投入し、AggregateByWorkflow が以下の値を厳密に返すことを検証する。
//
// Fixture 設計:
//
//	+-----+-----------+-----------+-------+--------+----------+
//	| Run | run.error | call cost | input | output | duration |
//	+-----+-----------+-----------+-------+--------+----------+
//	| r1  | NULL      | 1.0       | 100   | 200    | 10       |
//	| r2  | NULL      | NULL      | NULL  | NULL   | 20       |
//	| r3  | NULL      | 2.0       | 300   | 600    | 30       |
//	| r4  | NULL      | NULL      | NULL  | NULL   | 40       |
//	| r5  | "boom"    | 3.0       | 500   | 1000   | 50       |
//	+-----+-----------+-----------+-------+--------+----------+
//
// 期待値 (workflow-id 単位):
//   - RunCount = 5
//   - SuccessRate = 4/5 = 0.8 (r1〜r4 が成功 run、r5 が失敗 run)
//   - AvgCostUSD = (1.0+2.0+3.0)/3 = 2.0 (NULL 除外、IEEE 754 で厳密に表現可能)
//   - SumInputTokens = 100+300+500 = 900 (NULL 除外で実質 0 加算)
//   - SumOutputTokens = 200+600+1000 = 1800
//   - 既知 duration 配列 [10,20,30,40,50] (sorted) より:
//   - P50 = sorted[0.5*4=2.0] = 30 (整数 index)
//   - P95 = interp(sorted[3], sorted[4], 0.8) = 40*0.2 + 50*0.8 = 48
//
// 値選定の根拠:
//   - cost を 1.0 / 2.0 / 3.0 とし、平均 2.0 を IEEE 754 で厳密に表現可能にする
//     (0.10 系の小数は丸め誤差で == 比較が落ちるため意図的に避ける)
//   - duration を 10, 20, 30, 40, 50 とし、p50 が整数 index に当たる (補間不要)、
//     p95 のみ線形補間が走る配置にする
//   - 失敗 run (r5) にも非 NULL の cost/tokens を持たせ、「集計は agent_calls 全行を
//     走査する」(run 成功と独立) という仕様を駆動する
func TestReaderAggregateByWorkflowMixedRuns(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	runs := []aggregateRunSpec{
		{
			RunID:     "mix-r1",
			StartedAt: base.Add(1 * time.Minute),
			Failed:    false,
			Calls: []aggregateAgentCallSpec{{
				ID:           "mix-c1",
				StartedAt:    base.Add(1 * time.Minute).Add(time.Second),
				FinishedAt:   base.Add(1 * time.Minute).Add(2 * time.Second),
				DurationMS:   10,
				Success:      true,
				TotalCostUSD: nullF(1.0),
				InputTokens:  nullI(100),
				OutputTokens: nullI(200),
			}},
		},
		{
			RunID:     "mix-r2",
			StartedAt: base.Add(2 * time.Minute),
			Failed:    false,
			Calls: []aggregateAgentCallSpec{{
				ID:         "mix-c2",
				StartedAt:  base.Add(2 * time.Minute).Add(time.Second),
				FinishedAt: base.Add(2 * time.Minute).Add(2 * time.Second),
				DurationMS: 20,
				Success:    true,
				// cost / tokens は sql.Null* の zero value (Valid=false) → NULL
			}},
		},
		{
			RunID:     "mix-r3",
			StartedAt: base.Add(3 * time.Minute),
			Failed:    false,
			Calls: []aggregateAgentCallSpec{{
				ID:           "mix-c3",
				StartedAt:    base.Add(3 * time.Minute).Add(time.Second),
				FinishedAt:   base.Add(3 * time.Minute).Add(2 * time.Second),
				DurationMS:   30,
				Success:      true,
				TotalCostUSD: nullF(2.0),
				InputTokens:  nullI(300),
				OutputTokens: nullI(600),
			}},
		},
		{
			RunID:     "mix-r4",
			StartedAt: base.Add(4 * time.Minute),
			Failed:    false,
			Calls: []aggregateAgentCallSpec{{
				ID:         "mix-c4",
				StartedAt:  base.Add(4 * time.Minute).Add(time.Second),
				FinishedAt: base.Add(4 * time.Minute).Add(2 * time.Second),
				DurationMS: 40,
				Success:    true,
			}},
		},
		{
			RunID:     "mix-r5",
			StartedAt: base.Add(5 * time.Minute),
			Failed:    true, // run.error="boom" → 失敗 run として SuccessRate に効く
			Calls: []aggregateAgentCallSpec{{
				ID:           "mix-c5",
				StartedAt:    base.Add(5 * time.Minute).Add(time.Second),
				FinishedAt:   base.Add(5 * time.Minute).Add(2 * time.Second),
				DurationMS:   50,
				Success:      false,
				TotalCostUSD: nullF(3.0),
				InputTokens:  nullI(500),
				OutputTokens: nullI(1000),
			}},
		},
	}
	for _, r := range runs {
		insertAggregateWorkflowRun(t, ctx, recorder, "wf-mix", r)
	}

	result, err := recorder.AggregateByWorkflow(ctx, WorkflowAggregateOptions{})
	if err != nil {
		t.Fatalf("AggregateByWorkflow: %v", err)
	}
	if got := len(result.Buckets); got != 1 {
		t.Fatalf("len(Buckets) = %d, want 1 (workflow wf-mix のみ)", got)
	}
	b := result.Buckets[0]
	if b.WorkflowID != "wf-mix" {
		t.Fatalf("Buckets[0].WorkflowID = %q, want wf-mix", b.WorkflowID)
	}

	// RunCount: workflow_run 5 件すべてが集計母数。
	if b.RunCount != 5 {
		t.Errorf("RunCount = %d, want 5", b.RunCount)
	}
	// SuccessRate (Req 3.1): 4/5 = 0.8 (r1〜r4 が success run、r5 が failure run)。
	// 値の比較は浮動小数だが、4/5 は IEEE 754 で厳密に表現可能 (0x3FE99999... ではなく
	// 0.8 = 0.110011001100... の繰り返しのため非 exact だが、SQLite の整数除算経路から
	// SQL 側で float に昇格する経路 ではなく、Go 側で float64(4)/float64(5) を計算する
	// reader_sqlite.go 実装に従う。同一プロセスで同じ計算をすれば bit-identical な値が返る)。
	if b.SuccessRate != 0.8 {
		t.Errorf("SuccessRate = %v, want 0.8 (4/5)", b.SuccessRate)
	}
	// AvgCostUSD (Req 3.2 / 3.7): NULL を除外した 3 件の平均 (1.0+2.0+3.0)/3 = 2.0。
	// 値選定により IEEE 754 で厳密 2.0 が返るため pointer dereference の値比較で安全。
	if b.AvgCostUSD == nil {
		t.Fatalf("AvgCostUSD = nil, want non-nil (3 件の cost が非 NULL)")
	}
	if *b.AvgCostUSD != 2.0 {
		t.Errorf("AvgCostUSD = %v, want 2.0 ((1.0+2.0+3.0)/3、NULL 除外)", *b.AvgCostUSD)
	}
	// SumInputTokens (Req 3.4 / 3.7): NULL を除外して 100+300+500 = 900。
	if b.SumInputTokens != 900 {
		t.Errorf("SumInputTokens = %d, want 900 (100+300+500、NULL 除外)", b.SumInputTokens)
	}
	// SumOutputTokens: 200+600+1000 = 1800。
	if b.SumOutputTokens != 1800 {
		t.Errorf("SumOutputTokens = %d, want 1800 (200+600+1000、NULL 除外)", b.SumOutputTokens)
	}
	// P50DurationMS (Req 3.3): sorted=[10,20,30,40,50] / pos=0.5*4=2.0 / sorted[2]=30。
	if b.P50DurationMS == nil {
		t.Fatalf("P50DurationMS = nil, want &30")
	}
	if *b.P50DurationMS != 30 {
		t.Errorf("P50DurationMS = %d, want 30 (sorted[2]=30)", *b.P50DurationMS)
	}
	// P95DurationMS (Req 3.3): pos=0.95*4=3.8 / lo=3, hi=4 / weight=0.8 /
	// interp = 40*0.2 + 50*0.8 = 8 + 40 = 48 / round → 48。
	if b.P95DurationMS == nil {
		t.Fatalf("P95DurationMS = nil, want &48")
	}
	if *b.P95DurationMS != 48 {
		t.Errorf("P95DurationMS = %d, want 48 (線形補間: 40*0.2 + 50*0.8)", *b.P95DurationMS)
	}
}

// -----------------------------------------------------------------------------
// TestReaderAggregateByWorkflowOutOfRangeReturnsNilBuckets: フィルタ範囲外で
// Buckets == nil を返すことを検証する (Req 3.8: 該当データなしで空集計 / エラーにしない)。
// -----------------------------------------------------------------------------

// TestReaderAggregateByWorkflowOutOfRangeReturnsNilBuckets は run を 1 件 insert した上で、
// その started_at よりも未来の Range を指定すると Buckets == nil で返ることを検証する。
//
// 設計の意図:
//   - 「データはあるが、フィルタを通すと該当 0 件」の境界を駆動する
//     (空 DB の挙動は別 smoke test でカバー済み)
//   - エラーを返さない (Req 3.8 後段) ことも併せて確認
//   - LoadedRows / Truncated 等の補助フィールドは AggregateByWorkflow には無いため、
//     Buckets == nil の有無のみが完了条件
func TestReaderAggregateByWorkflowOutOfRangeReturnsNilBuckets(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	insertAggregateWorkflowRun(t, ctx, recorder, "wf-range", aggregateRunSpec{
		RunID:     "range-r1",
		StartedAt: base,
		Failed:    false,
		Calls: []aggregateAgentCallSpec{{
			ID:           "range-c1",
			StartedAt:    base.Add(time.Second),
			FinishedAt:   base.Add(2 * time.Second),
			DurationMS:   100,
			Success:      true,
			TotalCostUSD: nullF(1.0),
			InputTokens:  nullI(10),
			OutputTokens: nullI(20),
		}},
	})

	// fixture started_at は base = 2026-04-01。フィルタは 2027-01-01 〜 2027-12-31 を指定。
	since := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2027, 12, 31, 23, 59, 59, 0, time.UTC)
	result, err := recorder.AggregateByWorkflow(ctx, WorkflowAggregateOptions{
		Range: TimeRange{Since: since, Until: until},
	})
	if err != nil {
		t.Fatalf("AggregateByWorkflow (out of range): %v (Req 3.8: 範囲外でもエラーにしない)", err)
	}
	if result.Buckets != nil {
		t.Errorf("Buckets = %v, want nil (Req 3.8: 集計対象なし時 nil で返す)", result.Buckets)
	}
}

// -----------------------------------------------------------------------------
// TestReaderAggregateByWorkflowIdempotent: 同一パラメータ二回呼び出しで結果が
// 完全に一致することを検証する (Req 3.9: 冪等性)。
// -----------------------------------------------------------------------------

// TestReaderAggregateByWorkflowIdempotent は MixedRuns と同じフィクスチャを再利用しつつ
// (workflow_id を変えて干渉を避ける)、同一 WorkflowAggregateOptions で
// AggregateByWorkflow を 2 回呼び、結果が reflect.DeepEqual で等価であることを検証する。
//
// 等価性の根拠:
//   - 結果型 WorkflowAggregateBucket は string / int64 / float64 / *float64 / *int64 のみで構成
//   - SQL の ORDER BY workflow_id ASC で並びが安定 (Req 3.9 補強)
//   - reflect.DeepEqual はポインタ越しの値も再帰展開して比較する
//
// 同一パラメータで結果が異なれば「ページング順揺れ」「集計の非決定性」を疑うべきテスト失敗。
func TestReaderAggregateByWorkflowIdempotent(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	// 2 つの workflow を投入し、Buckets 並び (workflow_id ASC) も同時に検証する。
	// wfA: 3 run (2 success + 1 failure) / 2 call が NULL cost
	insertAggregateWorkflowRun(t, ctx, recorder, "wfA", aggregateRunSpec{
		RunID: "idem-a-r1", StartedAt: base.Add(1 * time.Minute), Failed: false,
		Calls: []aggregateAgentCallSpec{{
			ID: "idem-a-c1", StartedAt: base.Add(1*time.Minute + time.Second),
			FinishedAt: base.Add(1*time.Minute + 2*time.Second),
			DurationMS: 10, Success: true,
			TotalCostUSD: nullF(1.0), InputTokens: nullI(100), OutputTokens: nullI(200),
		}},
	})
	insertAggregateWorkflowRun(t, ctx, recorder, "wfA", aggregateRunSpec{
		RunID: "idem-a-r2", StartedAt: base.Add(2 * time.Minute), Failed: false,
		Calls: []aggregateAgentCallSpec{{
			ID: "idem-a-c2", StartedAt: base.Add(2*time.Minute + time.Second),
			FinishedAt: base.Add(2*time.Minute + 2*time.Second),
			DurationMS: 20, Success: true,
			// cost / tokens NULL
		}},
	})
	insertAggregateWorkflowRun(t, ctx, recorder, "wfA", aggregateRunSpec{
		RunID: "idem-a-r3", StartedAt: base.Add(3 * time.Minute), Failed: true,
		Calls: []aggregateAgentCallSpec{{
			ID: "idem-a-c3", StartedAt: base.Add(3*time.Minute + time.Second),
			FinishedAt: base.Add(3*time.Minute + 2*time.Second),
			DurationMS: 30, Success: false,
			TotalCostUSD: nullF(3.0), InputTokens: nullI(300), OutputTokens: nullI(600),
		}},
	})
	// wfB: 2 run (両方 success) / 全 call が非 NULL cost
	insertAggregateWorkflowRun(t, ctx, recorder, "wfB", aggregateRunSpec{
		RunID: "idem-b-r1", StartedAt: base.Add(10 * time.Minute), Failed: false,
		Calls: []aggregateAgentCallSpec{{
			ID: "idem-b-c1", StartedAt: base.Add(10*time.Minute + time.Second),
			FinishedAt: base.Add(10*time.Minute + 2*time.Second),
			DurationMS: 100, Success: true,
			TotalCostUSD: nullF(0.5), InputTokens: nullI(50), OutputTokens: nullI(100),
		}},
	})
	insertAggregateWorkflowRun(t, ctx, recorder, "wfB", aggregateRunSpec{
		RunID: "idem-b-r2", StartedAt: base.Add(11 * time.Minute), Failed: false,
		Calls: []aggregateAgentCallSpec{{
			ID: "idem-b-c2", StartedAt: base.Add(11*time.Minute + time.Second),
			FinishedAt: base.Add(11*time.Minute + 2*time.Second),
			DurationMS: 200, Success: true,
			TotalCostUSD: nullF(1.5), InputTokens: nullI(150), OutputTokens: nullI(300),
		}},
	})

	cases := []struct {
		name string
		opts WorkflowAggregateOptions
	}{
		{"no filter (両 workflow)", WorkflowAggregateOptions{}},
		{"workflow_id=wfA で絞り込み", WorkflowAggregateOptions{WorkflowID: "wfA"}},
		{"workflow_id=wfB で絞り込み", WorkflowAggregateOptions{WorkflowID: "wfB"}},
		{"since 指定 (wfB のみ範囲内)", WorkflowAggregateOptions{Range: TimeRange{Since: base.Add(5 * time.Minute)}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			first, err := recorder.AggregateByWorkflow(ctx, c.opts)
			if err != nil {
				t.Fatalf("AggregateByWorkflow #1: %v", err)
			}
			second, err := recorder.AggregateByWorkflow(ctx, c.opts)
			if err != nil {
				t.Fatalf("AggregateByWorkflow #2: %v", err)
			}
			if !reflect.DeepEqual(first, second) {
				t.Errorf("AggregateByWorkflow 二回呼び出しで結果が異なる (Req 3.9 冪等性):\n#1 = %+v\n#2 = %+v", first, second)
			}
			// 並び順の一致を補強的に明示 (workflow_id ASC、DeepEqual に含まれるが診断性向上)。
			firstIDs := workflowIDsFromAggregate(first.Buckets)
			secondIDs := workflowIDsFromAggregate(second.Buckets)
			if !reflect.DeepEqual(firstIDs, secondIDs) {
				t.Errorf("WorkflowID 並び順が異なる:\n#1 = %v\n#2 = %v", firstIDs, secondIDs)
			}
		})
	}
}

// workflowIDsFromAggregate は Buckets から WorkflowID 列を抽出するヘルパ
// (table-driven 比較・並び順診断用)。空入力は nil を返し、reflect.DeepEqual の
// nil vs []string{} 罠を回避する (4.1 Implementation Notes に同種の対処あり)。
func workflowIDsFromAggregate(buckets []WorkflowAggregateBucket) []string {
	if len(buckets) == 0 {
		return nil
	}
	out := make([]string, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, b.WorkflowID)
	}
	return out
}

// -----------------------------------------------------------------------------
// AggregateByProvider テスト (タスク 4.3)
//
// 既存ヘルパ (aggregateAgentCallSpec / aggregateRunSpec / insertAggregateWorkflowRun) を
// 再利用しつつ、aggregateAgentCallSpec.ProviderID を call ごとに切り替えて
// 複数プロバイダ混在のフィクスチャを構築する。
// -----------------------------------------------------------------------------

// providerIDsFromAggregate は ProviderAggregateBucket 配列から ProviderID 列を抽出する
// ヘルパ (並び順診断用)。空入力 → nil で reflect.DeepEqual の nil vs []string{} 罠を回避。
func providerIDsFromAggregate(buckets []ProviderAggregateBucket) []string {
	if len(buckets) == 0 {
		return nil
	}
	out := make([]string, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, b.ProviderID)
	}
	return out
}

// findProviderBucket は Buckets から指定 ProviderID の bucket を検索するヘルパ。
// 見つからなければ t.Fatalf でテストを停止する (テストの前提崩壊を即時に検出するため)。
func findProviderBucket(t *testing.T, buckets []ProviderAggregateBucket, providerID string) ProviderAggregateBucket {
	t.Helper()
	for _, b := range buckets {
		if b.ProviderID == providerID {
			return b
		}
	}
	t.Fatalf("ProviderID=%q が Buckets に存在しない (got providers = %v)", providerID, providerIDsFromAggregate(buckets))
	return ProviderAggregateBucket{} // unreachable
}

// TestReaderAggregateByProviderTwoProvidersTenCalls は 2 provider × 10 agent_call の
// fixture で AggregateByProvider が以下を満たすことを厳密値で検証する (タスク 4.3 中核)。
//
// Fixture 設計 (1 workflow_run "wf-prov" / "run-prov" 配下に 20 agent_call を投入):
//
//	+-----------+---------+---------+-------+--------+----------+
//	| Provider  | Success | Failure | cost  | tokens | duration |
//	+-----------+---------+---------+-------+--------+----------+
//	| providerA | 7       | 3       | 1.0   | 10/20  | 10..100  |
//	| providerB | 5       | 5       | 2.0   | 10/20  | 全 200   |
//	+-----------+---------+---------+-------+--------+----------+
//
// 失敗 call は cost / tokens を NULL にし、Req 3.7 (NULL 除外) を駆動する。
// duration は providerA を [10,20,30,40,50,60,70,80,90,100] に散らし percentile 線形補間を、
// providerB を全 200 に揃えて percentile が単一値で確定するパターンを混在させる。
//
// 期待値:
//   - providerA: CallCount=10 / SuccessRate=0.7 (7/10) / AvgCostUSD=1.0 (NULL 除外で 7 件平均) /
//     SumInputTokens=70 / SumOutputTokens=140 / P50DurationMS=55 (50*0.5+60*0.5) /
//     P95DurationMS=96 (90*0.45+100*0.55=95.5 → math.Round で 96)
//   - providerB: CallCount=10 / SuccessRate=0.5 (5/10) / AvgCostUSD=2.0 / SumInputTokens=50 /
//     SumOutputTokens=100 / P50=200 / P95=200
//   - 集計不変条件 (Req 3.5): Σ CallCount = 20 (= 投入 agent_call 総数)
//   - 並び順: provider_id ASC で providerA → providerB (Req 3.9)
//
// 値選定の根拠:
//   - cost を 1.0 / 2.0 (整数) にして AvgCostUSD == 厳密 == を IEEE 754 安全に成立させる
//   - duration を providerA だけ heterogeneous にし percentile の線形補間を駆動 (10..100)、
//     providerB は均一にしてエッジ (= 全要素同値時の percentile 確定) を駆動
//   - Req 3.5 の集計不変条件「全 provider の CallCount 合算 = フィルタ範囲内 agent_call 総数」を
//     Σ で明示的に assert する
func TestReaderAggregateByProviderTwoProvidersTenCalls(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	// providerA call durations: 10, 20, 30, ..., 100 (1〜10 のインデックスを 10 倍)。
	// 先頭 7 件 (success) には cost=1.0 / tokens を埋め、後半 3 件 (failure) は NULL。
	// providerB は durations を全 200 に揃え、5 件 success (cost=2.0 / tokens) + 5 件 failure (NULL)。
	calls := make([]aggregateAgentCallSpec, 0, 20)
	for i := range 10 {
		dur := int64((i + 1) * 10)
		spec := aggregateAgentCallSpec{
			ID:         fmt.Sprintf("prov-A-c%02d", i),
			ProviderID: "providerA",
			StartedAt:  base.Add(time.Duration(i) * time.Second),
			FinishedAt: base.Add(time.Duration(i)*time.Second + time.Millisecond*time.Duration(dur)),
			DurationMS: dur,
		}
		if i < 7 {
			spec.Success = true
			spec.TotalCostUSD = nullF(1.0)
			spec.InputTokens = nullI(10)
			spec.OutputTokens = nullI(20)
		} else {
			spec.Success = false
			// cost / tokens は zero value (Valid=false) → NULL
		}
		calls = append(calls, spec)
	}
	for i := range 10 {
		spec := aggregateAgentCallSpec{
			ID:         fmt.Sprintf("prov-B-c%02d", i),
			ProviderID: "providerB",
			StartedAt:  base.Add(time.Hour + time.Duration(i)*time.Second),
			FinishedAt: base.Add(time.Hour + time.Duration(i)*time.Second + 200*time.Millisecond),
			DurationMS: 200,
		}
		if i < 5 {
			spec.Success = true
			spec.TotalCostUSD = nullF(2.0)
			spec.InputTokens = nullI(10)
			spec.OutputTokens = nullI(20)
		} else {
			spec.Success = false
		}
		calls = append(calls, spec)
	}

	insertAggregateWorkflowRun(t, ctx, recorder, "wf-prov", aggregateRunSpec{
		RunID:     "run-prov",
		StartedAt: base,
		Failed:    false,
		Calls:     calls,
	})

	result, err := recorder.AggregateByProvider(ctx, ProviderAggregateOptions{})
	if err != nil {
		t.Fatalf("AggregateByProvider: %v", err)
	}
	if got := len(result.Buckets); got != 2 {
		t.Fatalf("len(Buckets) = %d, want 2 (providerA + providerB)", got)
	}

	// 並び順: provider_id ASC (Req 3.9)。
	if gotIDs, wantIDs := providerIDsFromAggregate(result.Buckets), []string{"providerA", "providerB"}; !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Errorf("ProviderID 並び順 = %v, want %v (provider_id ASC)", gotIDs, wantIDs)
	}

	// providerA の厳密値検証。
	bA := findProviderBucket(t, result.Buckets, "providerA")
	if bA.CallCount != 10 {
		t.Errorf("providerA.CallCount = %d, want 10", bA.CallCount)
	}
	if bA.SuccessRate != 0.7 {
		t.Errorf("providerA.SuccessRate = %v, want 0.7 (7/10)", bA.SuccessRate)
	}
	if bA.AvgCostUSD == nil {
		t.Fatalf("providerA.AvgCostUSD = nil, want &1.0 (NULL 除外で 7 件平均)")
	}
	if *bA.AvgCostUSD != 1.0 {
		t.Errorf("providerA.AvgCostUSD = %v, want 1.0 (NULL 除外で 7 件平均)", *bA.AvgCostUSD)
	}
	if bA.SumInputTokens != 70 {
		t.Errorf("providerA.SumInputTokens = %d, want 70 (7*10、NULL 除外)", bA.SumInputTokens)
	}
	if bA.SumOutputTokens != 140 {
		t.Errorf("providerA.SumOutputTokens = %d, want 140 (7*20、NULL 除外)", bA.SumOutputTokens)
	}
	// P50: pos = 0.5*9 = 4.5、sorted[4]=50 / sorted[5]=60、interp = 50*0.5 + 60*0.5 = 55。
	if bA.P50DurationMS == nil || *bA.P50DurationMS != 55 {
		t.Errorf("providerA.P50DurationMS = %v, want 55 (sorted=[10..100]、線形補間 50*0.5+60*0.5)", bA.P50DurationMS)
	}
	// P95: pos = 0.95*9 = 8.55、sorted[8]=90 / sorted[9]=100、interp = 90*0.45 + 100*0.55 = 95.5、
	// math.Round(95.5) = 96。
	if bA.P95DurationMS == nil || *bA.P95DurationMS != 96 {
		t.Errorf("providerA.P95DurationMS = %v, want 96 (線形補間 90*0.45+100*0.55=95.5 → round 96)", bA.P95DurationMS)
	}

	// providerB の厳密値検証。
	bB := findProviderBucket(t, result.Buckets, "providerB")
	if bB.CallCount != 10 {
		t.Errorf("providerB.CallCount = %d, want 10", bB.CallCount)
	}
	if bB.SuccessRate != 0.5 {
		t.Errorf("providerB.SuccessRate = %v, want 0.5 (5/10)", bB.SuccessRate)
	}
	if bB.AvgCostUSD == nil {
		t.Fatalf("providerB.AvgCostUSD = nil, want &2.0 (NULL 除外で 5 件平均)")
	}
	if *bB.AvgCostUSD != 2.0 {
		t.Errorf("providerB.AvgCostUSD = %v, want 2.0 (NULL 除外で 5 件平均)", *bB.AvgCostUSD)
	}
	if bB.SumInputTokens != 50 {
		t.Errorf("providerB.SumInputTokens = %d, want 50 (5*10、NULL 除外)", bB.SumInputTokens)
	}
	if bB.SumOutputTokens != 100 {
		t.Errorf("providerB.SumOutputTokens = %d, want 100 (5*20、NULL 除外)", bB.SumOutputTokens)
	}
	// 全 200 で揃っているため percentile は線形補間しても 200 で確定する。
	if bB.P50DurationMS == nil || *bB.P50DurationMS != 200 {
		t.Errorf("providerB.P50DurationMS = %v, want 200 (全要素 200)", bB.P50DurationMS)
	}
	if bB.P95DurationMS == nil || *bB.P95DurationMS != 200 {
		t.Errorf("providerB.P95DurationMS = %v, want 200 (全要素 200)", bB.P95DurationMS)
	}

	// Req 3.5 集計不変条件: Σ CallCount = 20 (= フィルタ範囲内の agent_call 総数)。
	var sumCallCount int64
	for _, b := range result.Buckets {
		sumCallCount += b.CallCount
	}
	if sumCallCount != 20 {
		t.Errorf("Σ CallCount = %d, want 20 (集計不変条件: 全 provider 合算 = フィルタ範囲内 agent_call 総数)", sumCallCount)
	}
}

// TestReaderAggregateByProviderWorkflowIDFilter は WorkflowID 追加絞り込みで
// 該当 workflow の agent_calls のみが集計対象になることを検証する (Req 3.6)。
//
// Fixture 設計:
//   - workflow "wfA" / run "wf-a-run": providerA × 3 call (全成功)
//   - workflow "wfB" / run "wf-b-run": providerA × 2 call (全成功)
//
// 期待値:
//   - WorkflowID="" (全 workflow): providerA.CallCount = 5
//   - WorkflowID="wfA": providerA.CallCount = 3 (wfA の 3 call のみ)
//   - WorkflowID="wfB": providerA.CallCount = 2 (wfB の 2 call のみ)
//   - WorkflowID="missing": Buckets == nil (集計対象なし、Req 3.8)
//
// 設計の意図:
//   - 同一 provider が複数 workflow にまたがるケースを駆動し、provider 単位集計の
//     母数 (agent_call) を WorkflowID で絞り込むと該当 workflow 配下の call のみが
//     集計対象になることを assert する
//   - WorkflowID="missing" で Buckets nil を併せて確認し、Req 3.8 後段の
//     「該当データなし → エラーにせず空集計」も触る
func TestReaderAggregateByProviderWorkflowIDFilter(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	// wfA: providerA × 3 call (全成功、cost=1.0 / tokens=10/20 / duration=100)
	wfACalls := make([]aggregateAgentCallSpec, 0, 3)
	for i := range 3 {
		wfACalls = append(wfACalls, aggregateAgentCallSpec{
			ID:           fmt.Sprintf("wf-filter-a-c%d", i),
			ProviderID:   "providerA",
			StartedAt:    base.Add(time.Duration(i) * time.Second),
			FinishedAt:   base.Add(time.Duration(i)*time.Second + 100*time.Millisecond),
			DurationMS:   100,
			Success:      true,
			TotalCostUSD: nullF(1.0),
			InputTokens:  nullI(10),
			OutputTokens: nullI(20),
		})
	}
	insertAggregateWorkflowRun(t, ctx, recorder, "wfA", aggregateRunSpec{
		RunID:     "wf-a-run",
		StartedAt: base,
		Failed:    false,
		Calls:     wfACalls,
	})

	// wfB: providerA × 2 call (全成功、cost=1.0 / tokens=10/20 / duration=100)
	wfBCalls := make([]aggregateAgentCallSpec, 0, 2)
	for i := range 2 {
		wfBCalls = append(wfBCalls, aggregateAgentCallSpec{
			ID:           fmt.Sprintf("wf-filter-b-c%d", i),
			ProviderID:   "providerA",
			StartedAt:    base.Add(time.Hour + time.Duration(i)*time.Second),
			FinishedAt:   base.Add(time.Hour + time.Duration(i)*time.Second + 100*time.Millisecond),
			DurationMS:   100,
			Success:      true,
			TotalCostUSD: nullF(1.0),
			InputTokens:  nullI(10),
			OutputTokens: nullI(20),
		})
	}
	insertAggregateWorkflowRun(t, ctx, recorder, "wfB", aggregateRunSpec{
		RunID:     "wf-b-run",
		StartedAt: base.Add(time.Hour),
		Failed:    false,
		Calls:     wfBCalls,
	})

	cases := []struct {
		name          string
		workflowID    string
		wantBuckets   bool  // true: providerA の bucket がある / false: Buckets == nil
		wantCallCount int64 // wantBuckets=true のときの providerA.CallCount
	}{
		{"全 workflow (フィルタなし)", "", true, 5},
		{"wfA のみ", "wfA", true, 3},
		{"wfB のみ", "wfB", true, 2},
		{"存在しない workflow → Buckets nil", "missing", false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, err := recorder.AggregateByProvider(ctx, ProviderAggregateOptions{WorkflowID: c.workflowID})
			if err != nil {
				t.Fatalf("AggregateByProvider(workflow=%q): %v", c.workflowID, err)
			}
			if !c.wantBuckets {
				if result.Buckets != nil {
					t.Errorf("Buckets = %+v, want nil (Req 3.8: 該当データなし時 nil)", result.Buckets)
				}
				return
			}
			if len(result.Buckets) != 1 {
				t.Fatalf("len(Buckets) = %d, want 1 (providerA のみ)", len(result.Buckets))
			}
			b := findProviderBucket(t, result.Buckets, "providerA")
			if b.CallCount != c.wantCallCount {
				t.Errorf("providerA.CallCount = %d, want %d (workflow=%q)", b.CallCount, c.wantCallCount, c.workflowID)
			}
			// WorkflowID で絞り込みが効くなら、該当 workflow に属する agent_call の Σ も
			// CallCount と一致するはず (集計不変条件の workflow フィルタ版)。
			var sumCallCount int64
			for _, bk := range result.Buckets {
				sumCallCount += bk.CallCount
			}
			if sumCallCount != c.wantCallCount {
				t.Errorf("Σ CallCount = %d, want %d (workflow=%q フィルタ後の agent_call 総数)", sumCallCount, c.wantCallCount, c.workflowID)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Detail テスト (タスク 4.4)
//
// 観測可能な完了条件:
//   - 1 run + 3 agent_calls + 2 context_usage + 250 raw_events の fixture で
//     raw_events が 100 件にクランプされ、`*RunDetail.NextRawEventsOffset == 100` を assert
//   - `RawEventsQuery.Type = "error"` フィルタで raw_events 絞り込み確認
//   - 存在しない `WorkflowRunID` で `errors.Is(err, ErrWorkflowRunNotFound)` を assert
//   - `agent_calls` の並びが `started_at ASC, id ASC`、`raw_events` の並びが
//     `sequence ASC, id ASC` であることを assert (id tiebreaker)
//
// 既存 reader_sqlite_test.go の `insertDetailFixture` (タスク 2.4 の companion smoke test 用)
// は 1 run + 3 agent_calls + 2 context_usage + 250 raw_events の構成と一致するため、
// 主要観測条件のテストではこれを再利用する (4.1 / 4.2 / 4.3 と同じく同 package シンボル共有)。
//
// 一方、Type="error" 絞り込みおよび id tiebreaker の検証には専用フィクスチャが必要なため、
// 本セクションで局所ヘルパ `insertDetailRunBare` / `insertDetailRawEvent` を追加する。
// -----------------------------------------------------------------------------

// insertDetailRunBare は最低限の workflow_run + agent_call を投入する Detail テスト用ヘルパ。
//
// raw_events の type/subtype フィルタ検証や id tiebreaker 検証では agent_calls の cost/
// duration/tokens を見ないため、最小限のセットアップで十分。WorkflowName は WorkflowID と
// 同値、CWD は固定値 "/repo" を埋める。
//
// 戻り値の `agentCallID` は raw_events.agent_call_id に紐づける既定の ID。raw_events を
// 直接埋める Type フィルタテストでは、別 ID を任意に指定して構わない (raw_events.agent_call_id
// は schema NULLABLE のため空文字でも問題ないが、insertDetailRawEvent では明示的に渡す)。
func insertDetailRunBare(
	t *testing.T,
	ctx context.Context,
	recorder *SQLiteRecorder,
	runID, workflowID, agentCallID string,
	startedAt time.Time,
) {
	t.Helper()
	if err := recorder.RecordWorkflowRunStart(ctx, WorkflowRun{
		ID:           runID,
		WorkflowID:   workflowID,
		WorkflowName: workflowID,
		CWD:          "/repo",
		StartedAt:    startedAt,
	}); err != nil {
		t.Fatalf("RecordWorkflowRunStart %s: %v", runID, err)
	}
	if err := recorder.RecordWorkflowRunFinish(ctx, WorkflowRunFinish{
		ID:         runID,
		FinishedAt: startedAt.Add(time.Minute),
	}); err != nil {
		t.Fatalf("RecordWorkflowRunFinish %s: %v", runID, err)
	}
	if agentCallID != "" {
		if err := recorder.RecordAgentCallStart(ctx, AgentCall{
			ID:            agentCallID,
			WorkflowRunID: runID,
			WorkflowID:    workflowID,
			WorkflowName:  workflowID,
			StepID:        "s1",
			StepName:      "S1",
			ProviderID:    "claude",
			CWD:           "/repo",
			StartedAt:     startedAt.Add(time.Second),
		}); err != nil {
			t.Fatalf("RecordAgentCallStart %s: %v", agentCallID, err)
		}
	}
}

// insertDetailRawEvent は Detail テスト用の raw_event を Recorder API 経由で投入する。
//
// Type="context_usage" の auto-projection を意図的に避けたいテスト (Type="error" フィルタ)
// で使うことを想定しており、Source は呼び出し側で "provider" 等の非 "claude-control" を
// 渡すことで auto-projection 経路を回避する。
func insertDetailRawEvent(
	t *testing.T,
	ctx context.Context,
	recorder *SQLiteRecorder,
	event RawEvent,
) {
	t.Helper()
	if err := recorder.RecordRawEvent(ctx, event); err != nil {
		t.Fatalf("RecordRawEvent (seq=%d, type=%q, subtype=%q): %v", event.Sequence, event.Type, event.Subtype, err)
	}
}

// TestReaderDetailReturnsRunWithSubresources は 1 run + 3 agent_calls + 2 context_usage +
// 250 raw_events の fixture で Detail がデフォルト limit でクランプし、NextRawEventsOffset を
// 返すことを検証する (Req 4.1, 4.2, 4.3, 4.4, 4.6, 4.8, 7.1, 7.2)。
//
// 観測可能な完了条件 (タスク 4.4):
//   - len(RawEvents) == 100 (DefaultRawEventsLimit でクランプ)
//   - *RunDetail.NextRawEventsOffset == 100 (len(RawEvents)==Limit のとき次オフセット)
//   - len(AgentCalls) == 3 (started_at ASC: ac-1 < ac-2 < ac-3)
//   - len(ContextUsage) == 2 (auto-project された 2 件)
//
// reader_sqlite_test.go の `TestSQLiteRecorderDetailReturnsRunWithSubresources` は
// タスク 2.4 の companion smoke test だが、本テストは 4.4 boundary (reader_test.go) で
// 仕様の観測条件を独立に再表明する役割 (重複は意図的、4.1/4.2/4.3 と同じ pattern)。
func TestReaderDetailReturnsRunWithSubresources(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	runID := "detail-reader-1"
	insertDetailFixture(t, ctx, recorder, runID, base)

	detail, err := recorder.Detail(ctx, DetailOptions{WorkflowRunID: runID})
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}

	// Run header (Req 4.1): finished_at NOT NULL かつ error NULL → Success=&true。
	if detail.Run.RunID != runID {
		t.Errorf("Run.RunID = %q, want %q", detail.Run.RunID, runID)
	}
	if detail.Run.WorkflowID != "wf" {
		t.Errorf("Run.WorkflowID = %q, want wf", detail.Run.WorkflowID)
	}
	if detail.Run.Success == nil || !*detail.Run.Success {
		t.Errorf("Run.Success = %v, want &true (finished_at NOT NULL かつ error NULL)", detail.Run.Success)
	}

	// 3 agent_calls (started_at ASC: ac-1 → ac-2 → ac-3、Req 4.6)。
	if got := len(detail.AgentCalls); got != 3 {
		t.Fatalf("len(AgentCalls) = %d, want 3", got)
	}
	wantAgentCallIDs := []string{"ac-1", "ac-2", "ac-3"}
	for i, want := range wantAgentCallIDs {
		if detail.AgentCalls[i].ID != want {
			t.Errorf("AgentCalls[%d].ID = %q, want %q (started_at ASC)", i, detail.AgentCalls[i].ID, want)
		}
	}

	// 2 context_usage (Req 4.2)。
	if got := len(detail.ContextUsage); got != 2 {
		t.Errorf("len(ContextUsage) = %d, want 2", got)
	}

	// 250 raw_events → DefaultRawEventsLimit=100 でクランプ (Req 4.8 / 7.1)。
	if got := len(detail.RawEvents); got != 100 {
		t.Errorf("len(RawEvents) = %d, want 100 (DefaultRawEventsLimit クランプ)", got)
	}
	if detail.RawEventsLimit != DefaultRawEventsLimit {
		t.Errorf("RawEventsLimit = %d, want %d", detail.RawEventsLimit, DefaultRawEventsLimit)
	}
	if detail.RawEventsOffset != 0 {
		t.Errorf("RawEventsOffset = %d, want 0", detail.RawEventsOffset)
	}
	// NextRawEventsOffset (Req 4.4): len(RawEvents)==Limit のとき &(Offset+Limit)。
	if detail.NextRawEventsOffset == nil {
		t.Fatalf("NextRawEventsOffset = nil, want &100 (len(RawEvents)==Limit)")
	}
	if *detail.NextRawEventsOffset != 100 {
		t.Errorf("*NextRawEventsOffset = %d, want 100", *detail.NextRawEventsOffset)
	}

	// raw_events は sequence ASC で並ぶことを補強的に確認 (id tiebreaker は別テストで詳述)。
	for i := 1; i < len(detail.RawEvents); i++ {
		if detail.RawEvents[i-1].Sequence > detail.RawEvents[i].Sequence {
			t.Errorf("RawEvents[%d].Sequence (%d) > RawEvents[%d].Sequence (%d) — sequence ASC が崩れている",
				i-1, detail.RawEvents[i-1].Sequence, i, detail.RawEvents[i].Sequence)
		}
	}
}

// TestReaderDetailRawEventsTypeFilterError は RawEventsQuery.Type="error" フィルタが
// type="error" の raw_events のみを返すことを検証する (Req 4.5)。
//
// fixture: 1 run + 1 agent_call + (3 raw_events type="error" + 2 raw_events type="event")。
// 期待:
//   - Type="error" → 3 件すべて Type=="error"
//   - Type="event" → 2 件すべて Type=="event"
//   - Type="" (sentinel)  → 5 件すべて取得
//   - Type="missing" → 0 件
//
// 既存 reader_sqlite_test.go の TestSQLiteRecorderDetailRawEventsTypeFilter は
// type="event" 絞り込み単体を確認しているが、4.4 はタスク仕様で type="error" を明示しており
// クラスタリング (Req 5.3) で参照される type='error' 経路と整合性を持たせるためにも
// 本テストでは "error" を主軸に据える。
func TestReaderDetailRawEventsTypeFilterError(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	runID := "detail-type-filter"
	agentCallID := "ac-type-filter"
	insertDetailRunBare(t, ctx, recorder, runID, "wf-type", agentCallID, base)

	// type="error" 3 件 + type="event" 2 件を投入する。Subtype は assert に直接使わないが
	// type フィルタが Subtype を巻き込まないことを確認するため、両 type で同じ subtype 値を
	// 含める。
	type eventSpec struct {
		typ     string
		subtype string
	}
	events := []eventSpec{
		{typ: "error", subtype: "rate_limit"},
		{typ: "error", subtype: "rate_limit"},
		{typ: "error", subtype: "stream_idle_timeout"},
		{typ: "event", subtype: "tool_use"},
		{typ: "event", subtype: "rate_limit"}, // 意図的に "error" と同じ subtype を含める
	}
	for i, ev := range events {
		insertDetailRawEvent(t, ctx, recorder, RawEvent{
			AgentCallID:   agentCallID,
			WorkflowRunID: runID,
			Sequence:      int64(i + 1),
			Source:        "provider", // "claude-control" でないため auto-project 対象外
			Type:          ev.typ,
			Subtype:       ev.subtype,
			ObservedAt:    base.Add(time.Duration(i) * time.Second),
		})
	}

	cases := []struct {
		name      string
		typeQuery string
		wantCount int
		wantType  string // wantCount > 0 のとき全行 Type==wantType を満たす必要あり
	}{
		{"type=error → 3 件", "error", 3, "error"},
		{"type=event → 2 件", "event", 2, "event"},
		{"type=missing → 0 件", "missing", 0, ""},
		{"type=\"\" sentinel → 全件", "", 5, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			detail, err := recorder.Detail(ctx, DetailOptions{
				WorkflowRunID: runID,
				RawEvents:     RawEventsQuery{Type: c.typeQuery},
			})
			if err != nil {
				t.Fatalf("Detail(type=%q): %v", c.typeQuery, err)
			}
			if got := len(detail.RawEvents); got != c.wantCount {
				t.Fatalf("len(RawEvents) = %d, want %d (type=%q)", got, c.wantCount, c.typeQuery)
			}
			if c.wantType != "" {
				for i, ev := range detail.RawEvents {
					if ev.Type != c.wantType {
						t.Errorf("RawEvents[%d].Type = %q, want %q (type=%q フィルタ漏れ)",
							i, ev.Type, c.wantType, c.typeQuery)
					}
				}
			}
		})
	}
}

// TestReaderDetailNotFound は存在しない workflow_run_id に対して
// errors.Is(err, ErrWorkflowRunNotFound) を満たすエラーを返すことを検証する (Req 4.7 / 8.3)。
//
// Reader 契約上、sentinel error は wrap せずに直接返す (errors.Is で判別可能)。
// 「該当なし」を error 経由で表現する設計のため、空 RunDetail でなく error を確認する。
func TestReaderDetailNotFound(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	// 別 run を 1 件投入し、対象 ID と紛れないことも併せて確認する。
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	insertDetailRunBare(t, ctx, recorder, "exists-run", "wf", "ac-exists", base)

	cases := []struct {
		name  string
		runID string
	}{
		{"未存在 run id", "missing-run"},
		{"空文字 run id", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := recorder.Detail(ctx, DetailOptions{WorkflowRunID: c.runID})
			if !errors.Is(err, ErrWorkflowRunNotFound) {
				t.Fatalf("Detail(run=%q) error = %v, want errors.Is(ErrWorkflowRunNotFound)", c.runID, err)
			}
		})
	}
}

// TestReaderDetailAgentCallsOrderingWithIDTiebreaker は同一 started_at の 3 agent_calls を
// 投入し、Detail が `started_at ASC, id ASC` の安定ソートで返すことを検証する (Req 4.6)。
//
// fixture: 同一 startedAt で 3 つの agent_call を id="ac-detail-c" / "ac-detail-a" /
// "ac-detail-b" の順 (= 投入順) で挿入する。
// 期待: 結果は id ASC で "ac-detail-a" → "ac-detail-b" → "ac-detail-c" の順。
//
// 投入順と異なる結果順序を要求することで、SQL の ORDER BY が文字列比較で id を tiebreak
// できているかを駆動する (投入順=結果順なら ORDER BY が無効でも通過してしまう)。
func TestReaderDetailAgentCallsOrderingWithIDTiebreaker(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	runID := "detail-ac-tiebreak"
	if err := recorder.RecordWorkflowRunStart(ctx, WorkflowRun{
		ID: runID, WorkflowID: "wf", WorkflowName: "wf", CWD: "/repo", StartedAt: base,
	}); err != nil {
		t.Fatalf("RecordWorkflowRunStart: %v", err)
	}
	tieAt := base.Add(time.Minute) // 3 agent_calls すべて同一 started_at
	insertOrder := []string{"ac-detail-c", "ac-detail-a", "ac-detail-b"}
	for _, id := range insertOrder {
		if err := recorder.RecordAgentCallStart(ctx, AgentCall{
			ID: id, WorkflowRunID: runID, WorkflowID: "wf", WorkflowName: "wf",
			StepID: "s1", StepName: "S1", ProviderID: "claude", CWD: "/repo",
			StartedAt: tieAt,
		}); err != nil {
			t.Fatalf("RecordAgentCallStart %s: %v", id, err)
		}
	}

	detail, err := recorder.Detail(ctx, DetailOptions{WorkflowRunID: runID})
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if got := len(detail.AgentCalls); got != 3 {
		t.Fatalf("len(AgentCalls) = %d, want 3", got)
	}
	gotIDs := make([]string, 0, len(detail.AgentCalls))
	for _, ac := range detail.AgentCalls {
		gotIDs = append(gotIDs, ac.ID)
	}
	wantIDs := []string{"ac-detail-a", "ac-detail-b", "ac-detail-c"}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Errorf("AgentCalls IDs = %v, want %v (started_at 同値 → id ASC tiebreaker)", gotIDs, wantIDs)
	}
}

// TestReaderDetailRawEventsOrderingWithIDTiebreaker は同一 sequence の 3 raw_events を
// 投入し、Detail が `sequence ASC, id ASC` の安定ソートで返すことを検証する (Req 4.6)。
//
// fixture:
//   - 同一 sequence=1 の 3 件 (id は AUTOINCREMENT で投入順 = 1, 2, 3)
//   - 別 sequence=2 の 1 件 (id=4)
//
// 期待: 結果は (sequence=1, id=1) → (sequence=1, id=2) → (sequence=1, id=3) →
//
//	(sequence=2, id=4)。
//
// id は AUTOINCREMENT のため検証側では「ID が単調増加」のみ確認する (具体値 1/2/3/4 は
// 他のテストとの並行実行 (同 DB は共有しないため実質 fresh) で 1 から始まるが、検証は
// 並行/再実行に強い形で書く)。
func TestReaderDetailRawEventsOrderingWithIDTiebreaker(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	runID := "detail-rev-tiebreak"
	agentCallID := "ac-rev-tiebreak"
	insertDetailRunBare(t, ctx, recorder, runID, "wf", agentCallID, base)

	// 3 件 sequence=1 + 1 件 sequence=2 を投入。raw_events.id (= AUTOINCREMENT) は
	// 投入順に確定するため、(sequence ASC, id ASC) の正しい挙動を駆動できる。
	type seqMarker struct {
		seq    int64
		marker string // 投入順の検証用に Source へ埋める
	}
	insertOrder := []seqMarker{
		{seq: 1, marker: "first"},
		{seq: 1, marker: "second"},
		{seq: 1, marker: "third"},
		{seq: 2, marker: "fourth"},
	}
	for i, ev := range insertOrder {
		insertDetailRawEvent(t, ctx, recorder, RawEvent{
			AgentCallID:   agentCallID,
			WorkflowRunID: runID,
			Sequence:      ev.seq,
			Source:        ev.marker, // marker を Source 列で運ぶ (auto-project は claude-control のみ反応)
			Type:          "event",
			ObservedAt:    base.Add(time.Duration(i) * time.Second),
		})
	}

	detail, err := recorder.Detail(ctx, DetailOptions{WorkflowRunID: runID})
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if got := len(detail.RawEvents); got != 4 {
		t.Fatalf("len(RawEvents) = %d, want 4", got)
	}

	// (sequence ASC, id ASC) を満たす: 直前要素より sequence が小さい、または sequence 同値で
	// id が小さい状態は許容しない。
	for i := 1; i < len(detail.RawEvents); i++ {
		prev := detail.RawEvents[i-1]
		curr := detail.RawEvents[i]
		switch {
		case prev.Sequence > curr.Sequence:
			t.Errorf("RawEvents[%d].Sequence (%d) > RawEvents[%d].Sequence (%d) — sequence ASC 違反",
				i-1, prev.Sequence, i, curr.Sequence)
		case prev.Sequence == curr.Sequence && prev.ID >= curr.ID:
			t.Errorf("同一 sequence で RawEvents[%d].ID (%d) >= RawEvents[%d].ID (%d) — id ASC tiebreaker 違反",
				i-1, prev.ID, i, curr.ID)
		}
	}

	// 投入順 = id ASC が成立しているなら、Source 列に埋めた marker も投入順で並ぶ。
	gotMarkers := make([]string, 0, len(detail.RawEvents))
	for _, ev := range detail.RawEvents {
		gotMarkers = append(gotMarkers, ev.Source)
	}
	wantMarkers := []string{"first", "second", "third", "fourth"}
	if !reflect.DeepEqual(gotMarkers, wantMarkers) {
		t.Errorf("Source markers = %v, want %v (id AUTOINCREMENT = 投入順 → 結果は投入順を保つ)",
			gotMarkers, wantMarkers)
	}
}

// -----------------------------------------------------------------------------
// FailureClusters / normalizeError テスト (タスク 4.5)
//
// 観測可能な完了条件:
//   - failure_category × provider × error pattern で失敗 agent_call を 10 件以上 fixture insert
//   - normalizeError 単体テスト: 数字 / UUID / パス / 連続空白圧縮 / 80 文字切詰め
//   - クラスタ件数降順 → last_seen 降順を assert (タイブレーカ)
//   - RepresentativeRunIDs が finished_at DESC で SampleSize 件
//   - MaxFailureRowsLoaded = 10 で Truncated == true && LoadedRows == 10
//
// 既存 reader_sqlite_test.go の `insertFailureClusterFixture` (タスク 2.5 の companion smoke
// test 用) は同 package のためそのまま再利用する (4.1 / 4.2 / 4.3 / 4.4 と同じ pattern)。
// 一方、RepresentativeRunIDs の finished_at DESC 検証 / フィルタ各種 (workflow_id /
// provider_id / 期間) には複数 workflow_run にまたがる failure 母集団が必要なため、
// 本セクションで局所ヘルパ insertFailureRunWithCall を追加する。
// -----------------------------------------------------------------------------

// insertFailureRunWithCall は 1 つの workflow_run + 1 つの failure agent_call を投入する
// FailureClusters テスト用ヘルパ。
//
// failureCategory / providerID / errorMsg は agent_call に格納され、クラスタリングキーを
// 駆動する。runStartedAt は workflow_run の started_at 兼 (+ 1 秒シフトで) agent_call の
// started_at。finishedAt は agent_call の finished_at + workflow_run の finished_at に
// 採用する (FailureClusters は agent_call.finished_at のみ参照するが、一貫性のため
// workflow_run も close する)。
//
// `insertFailureClusterFixture` (reader_sqlite_test.go) との違い: 本ヘルパは agent_call と
// workflow_run を 1:1 で対応させるため、RepresentativeRunIDs が複数の異なる workflow_run_id を
// 持つ状況を fixture で組める。
func insertFailureRunWithCall(
	t *testing.T,
	ctx context.Context,
	recorder *SQLiteRecorder,
	runID, agentCallID, workflowID, providerID, failureCategory, errorMsg string,
	runStartedAt, finishedAt time.Time,
) {
	t.Helper()
	if err := recorder.RecordWorkflowRunStart(ctx, WorkflowRun{
		ID:           runID,
		WorkflowID:   workflowID,
		WorkflowName: workflowID,
		CWD:          "/repo",
		StartedAt:    runStartedAt,
	}); err != nil {
		t.Fatalf("RecordWorkflowRunStart %s: %v", runID, err)
	}
	if err := recorder.RecordAgentCallStart(ctx, AgentCall{
		ID:            agentCallID,
		WorkflowRunID: runID,
		WorkflowID:    workflowID,
		WorkflowName:  workflowID,
		StepID:        "s1",
		StepName:      "S1",
		ProviderID:    providerID,
		CWD:           "/repo",
		StartedAt:     runStartedAt.Add(time.Second),
	}); err != nil {
		t.Fatalf("RecordAgentCallStart %s: %v", agentCallID, err)
	}
	if err := recorder.RecordAgentCallFinish(ctx, AgentCallResult{
		ID:              agentCallID,
		SessionID:       "sess",
		Success:         false,
		DurationMS:      100,
		FailureCategory: failureCategory,
		Error:           errorMsg,
		FinishedAt:      finishedAt,
	}); err != nil {
		t.Fatalf("RecordAgentCallFinish %s: %v", agentCallID, err)
	}
	if err := recorder.RecordWorkflowRunFinish(ctx, WorkflowRunFinish{
		ID:         runID,
		Error:      errorMsg,
		FinishedAt: finishedAt,
	}); err != nil {
		t.Fatalf("RecordWorkflowRunFinish %s: %v", runID, err)
	}
}

// TestNormalizeError は normalizeError (reader_internal.go) の正規化ヒューリスティックを
// table-driven で検証する (Req 5.2 / タスク 4.5)。
//
// 検証対象:
//   - 数字マスキング (連続数字列 → <NUM>)
//   - UUID マスキング (8-4-4-4-12 形式 → <UUID>、大文字小文字両対応)
//   - パスマスキング (絶対パス /... → <PATH>、内部の数字を巻き込む)
//   - 連続空白圧縮 (空白 / タブ / 改行 → 半角空白 1 つ)
//   - 80 文字 (バイト) 切り詰め
//
// 適用順序の検証 (reader_internal.go コメント参照):
//
//	空白圧縮 → UUID → パス → 数字 → 80 バイト切り詰め
//
// 既存 reader_internal_test.go の TestNormalizeError_VerificationExample / _Empty は
// タスク 1.3 の最小限 smoke test。本テストは 4.5 の包括的 table-driven 検証として
// 重複を厭わず仕様の観測条件を網羅する (4.x 系の他テストと同 pattern)。
func TestNormalizeError(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "empty input returns empty",
			input: "",
			want:  "",
		},
		{
			name:  "no special pattern - identity",
			input: "Connection refused",
			want:  "Connection refused",
		},
		{
			name:  "consecutive spaces collapsed to single space",
			input: "foo   bar",
			want:  "foo bar",
		},
		{
			name:  "tab and newline collapsed to single space",
			input: "foo\t\nbar",
			want:  "foo bar",
		},
		{
			name:  "leading and trailing whitespace trimmed",
			input: "  foo bar  ",
			want:  "foo bar",
		},
		{
			name:  "single number masked",
			input: "got 12345 errors",
			want:  "got <NUM> errors",
		},
		{
			name:  "multiple numbers masked separately",
			input: "10 of 20",
			want:  "<NUM> of <NUM>",
		},
		{
			name:  "lowercase UUID masked",
			input: "id 550e8400-e29b-41d4-a716-446655440000",
			want:  "id <UUID>",
		},
		{
			name:  "uppercase UUID masked",
			input: "ID 550E8400-E29B-41D4-A716-446655440000",
			want:  "ID <UUID>",
		},
		{
			name:  "path masking absorbs embedded number (path before number)",
			input: "open /tmp/sock-1234",
			want:  "open <PATH>",
		},
		{
			name:  "path masked then standalone number masked",
			input: "open /tmp/sock-1234 retry 5 times",
			want:  "open <PATH> retry <NUM> times",
		},
		{
			name:  "verification example (path + UUID composite)",
			input: "Connection refused at /tmp/sock-1234 with id 550e8400-e29b-41d4-a716-446655440000",
			want:  "Connection refused at <PATH> with id <UUID>",
		},
		{
			name:  "exact 80 byte input unchanged (boundary)",
			input: strings.Repeat("a", 80),
			want:  strings.Repeat("a", 80),
		},
		{
			name:  "100 byte input truncated to 80",
			input: strings.Repeat("a", 100),
			// 80 バイト切り詰めで前 80 バイトのみ残る。
			want: strings.Repeat("a", 80),
		},
		{
			name:  "truncation occurs after masking (87 byte input)",
			input: "Error: " + strings.Repeat("z", 80),
			// "Error: " (7 byte) + "z"*80 = 87 byte → 80 byte 切り詰めで "Error: " + "z"*73
			want: "Error: " + strings.Repeat("z", 73),
		},
		{
			name:  "multiple consecutive whitespace types collapsed",
			input: "foo \t \n \r bar",
			want:  "foo bar",
		},
		{
			name:  "UUID masking happens before number masking (else UUID's digits would split)",
			input: "550e8400-e29b-41d4-a716-446655440000",
			// もし数字マスキングが先だった場合、"<NUM>e<NUM>-e<NUM>b-<NUM>d<NUM>-a<NUM>-..." と
			// UUID パターンが破壊されてしまう。UUID 先行で <UUID> 1 つに正規化されることを確認。
			want: "<UUID>",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeError(c.input)
			if got != c.want {
				t.Errorf("normalizeError(%q):\n  got:  %q\n  want: %q", c.input, got, c.want)
			}
			// 切り詰め boundary を独立に確認 (80 バイト超えを許容しない)。
			if len(got) > maxNormalizedErrorLen {
				t.Errorf("normalizeError(%q) returned %d bytes, exceeds maxNormalizedErrorLen=%d",
					c.input, len(got), maxNormalizedErrorLen)
			}
		})
	}
}

// TestReaderFailureClustersGroupsByKey は failure_category × provider × error の異なる
// 4 パターンで合計 13 件 (≥ 10 件) の failure agent_call を投入し、FailureClusters が
// クラスタリングキーごとに正しく分離した上で Count DESC で並ぶことを検証する
// (Req 5.1, 5.2, 5.6, 5.8 / タスク 4.5)。
//
// fixture 構成 (合計 13 件、4 クラスタ):
//   - A: provider_error / claude / "Connection refused" → 5 calls
//   - B: external_abort / codex  / "User aborted"       → 4 calls
//   - C: provider_error / claude / "Rate limit exceeded" → 3 calls (A と同 category/provider, error 違い)
//   - D: provider_error / codex  / "Connection refused" → 1 call  (A と同 category/error, provider 違い)
//
// 期待: 4 クラスタが Count DESC で A → B → C → D の順、LoadedRows=13、Truncated=false。
func TestReaderFailureClustersGroupsByKey(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	insertFailureClusterFixture(t, ctx, recorder, "A", "provider_error", "claude", "Connection refused", 5, base)
	insertFailureClusterFixture(t, ctx, recorder, "B", "external_abort", "codex", "User aborted", 4, base.Add(time.Hour))
	insertFailureClusterFixture(t, ctx, recorder, "C", "provider_error", "claude", "Rate limit exceeded", 3, base.Add(2*time.Hour))
	insertFailureClusterFixture(t, ctx, recorder, "D", "provider_error", "codex", "Connection refused", 1, base.Add(3*time.Hour))

	result, err := recorder.FailureClusters(ctx, FailureOptions{})
	if err != nil {
		t.Fatalf("FailureClusters: %v", err)
	}

	if got := len(result.Clusters); got != 4 {
		t.Fatalf("len(Clusters) = %d, want 4 (failure_category × provider × error の異なる 4 組)", got)
	}
	if result.LoadedRows != 13 {
		t.Errorf("LoadedRows = %d, want 13", result.LoadedRows)
	}
	if result.Truncated {
		t.Errorf("Truncated = true, want false (LoadedRows=13 < MaxFailureRowsLoaded=%d)", MaxFailureRowsLoaded)
	}

	// Req 5.6: Count DESC で並ぶ。
	wantCounts := []int64{5, 4, 3, 1}
	for i, want := range wantCounts {
		if got := result.Clusters[i].Count; got != want {
			t.Errorf("Clusters[%d].Count = %d, want %d (Count DESC)", i, got, want)
		}
	}

	// クラスタリングキー (failure_category, provider_id, normalized_error) がすべて
	// 一意であることを確認 (D は A と error / failure_category 同値だが provider 違いで別クラスタ)。
	seenKeys := make(map[string]bool, 4)
	for _, c := range result.Clusters {
		key := fmt.Sprintf("%s|%s|%s", c.FailureCategory, c.ProviderID, c.NormalizedErrorKey)
		if seenKeys[key] {
			t.Errorf("duplicate cluster key %q (failure_category × provider × normalized_error が一意でない)", key)
		}
		seenKeys[key] = true
	}

	// 主クラスタ (Count DESC 先頭) の NormalizedErrorKey は normalizeError 適用済み (数字 /
	// UUID / パスを含まない単純文字列のため、入力と同一文字列が返る)。
	if got := result.Clusters[0].NormalizedErrorKey; got != "Connection refused" {
		t.Errorf("Clusters[0].NormalizedErrorKey = %q, want %q", got, "Connection refused")
	}
}

// TestReaderFailureClustersTieBreakLastSeen は Count 同点クラスタが LastSeen DESC で
// タイブレークされることを検証する (Req 5.6 / タスク 4.5)。
//
// fixture 構成: 3 クラスタすべて Count=3 で error 文字列のみ異なる。LastSeen は
// 各クラスタの fixture base が 1 時間ずつずれているため、Z(最新) > Y > X の順。
//
// 期待: クラスタ並び順 Z → Y → X (Count 同値 → LastSeen DESC タイブレーカ)。
func TestReaderFailureClustersTieBreakLastSeen(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	insertFailureClusterFixture(t, ctx, recorder, "X", "provider_error", "claude", "Error X", 3, base)
	insertFailureClusterFixture(t, ctx, recorder, "Y", "provider_error", "claude", "Error Y", 3, base.Add(time.Hour))
	insertFailureClusterFixture(t, ctx, recorder, "Z", "provider_error", "claude", "Error Z", 3, base.Add(2*time.Hour))

	result, err := recorder.FailureClusters(ctx, FailureOptions{})
	if err != nil {
		t.Fatalf("FailureClusters: %v", err)
	}
	if got := len(result.Clusters); got != 3 {
		t.Fatalf("len(Clusters) = %d, want 3", got)
	}

	// Count 同値であることを最初に確認 (タイブレーカテストの前提)。
	for i, c := range result.Clusters {
		if c.Count != 3 {
			t.Errorf("Clusters[%d].Count = %d, want 3 (タイ前提)", i, c.Count)
		}
	}

	// Req 5.6: Count 同点 → LastSeen DESC。期待順序は最新 base から Z → Y → X。
	wantOrder := []string{"Error Z", "Error Y", "Error X"}
	for i, want := range wantOrder {
		if got := result.Clusters[i].NormalizedErrorKey; got != want {
			t.Errorf("Clusters[%d].NormalizedErrorKey = %q, want %q (Count 同点 → LastSeen DESC)",
				i, got, want)
		}
	}

	// LastSeen 自体も降順で並ぶことを連鎖的に検証する。
	for i := 1; i < len(result.Clusters); i++ {
		prev := result.Clusters[i-1]
		curr := result.Clusters[i]
		if !prev.LastSeen.After(curr.LastSeen) {
			t.Errorf("Clusters[%d].LastSeen (%v) は Clusters[%d].LastSeen (%v) より新しいべき (LastSeen DESC)",
				i-1, prev.LastSeen, i, curr.LastSeen)
		}
	}
}

// TestReaderFailureClustersRepresentativeRunIDsFinishedAtDesc は同一クラスタリングキーに
// 属する failure agent_call が異なる workflow_run_id にまたがる場合、RepresentativeRunIDs が
// finished_at DESC で SampleSize (default 5) 件に切り出されることを検証する
// (Req 5.4 / タスク 4.5)。
//
// fixture 構成: 7 つの異なる workflow_run、各 run に 1 つの failure agent_call。
// すべて同じ (failure_category, provider, error) のため 1 クラスタにまとまる。
// finished_at は base から 1 分間隔で増加 (rep-run-0 が最古、rep-run-6 が最新)。
//
// 期待: RepresentativeRunIDs == [rep-run-6, rep-run-5, rep-run-4, rep-run-3, rep-run-2]
// (DefaultFailureSampleSize=5 件、finished_at DESC)。
func TestReaderFailureClustersRepresentativeRunIDsFinishedAtDesc(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	const totalRuns = 7
	for i := range totalRuns {
		runID := fmt.Sprintf("rep-run-%d", i)
		agentCallID := fmt.Sprintf("rep-ac-%d", i)
		// finished_at を 1 分間隔で increasing にすることで、最新 5 件が rep-run-6..2 で
		// 確定する (i=0 は representative から除外、i=1 も除外、i=2..6 が SampleSize 内)。
		finishedAt := base.Add(time.Duration(i) * time.Minute)
		insertFailureRunWithCall(t, ctx, recorder,
			runID, agentCallID, "wf", "claude", "provider_error", "Same error",
			base, finishedAt,
		)
	}

	result, err := recorder.FailureClusters(ctx, FailureOptions{})
	if err != nil {
		t.Fatalf("FailureClusters: %v", err)
	}
	if got := len(result.Clusters); got != 1 {
		t.Fatalf("len(Clusters) = %d, want 1 (同一キーで 1 クラスタにまとまる)", got)
	}
	cluster := result.Clusters[0]

	if cluster.Count != int64(totalRuns) {
		t.Errorf("Clusters[0].Count = %d, want %d", cluster.Count, totalRuns)
	}

	// Req 5.4: SampleSize (default 5) 件、finished_at DESC で切り出し。
	if got := len(cluster.RepresentativeRunIDs); got != DefaultFailureSampleSize {
		t.Errorf("len(RepresentativeRunIDs) = %d, want %d (DefaultFailureSampleSize)",
			got, DefaultFailureSampleSize)
	}
	wantIDs := []string{"rep-run-6", "rep-run-5", "rep-run-4", "rep-run-3", "rep-run-2"}
	if !reflect.DeepEqual(cluster.RepresentativeRunIDs, wantIDs) {
		t.Errorf("RepresentativeRunIDs = %v, want %v (finished_at DESC で 5 件切り出し)",
			cluster.RepresentativeRunIDs, wantIDs)
	}

	// LastSeen はクラスタ内最大 finished_at = base + 6 minutes。
	wantLastSeen := base.Add(time.Duration(totalRuns-1) * time.Minute)
	if !cluster.LastSeen.Equal(wantLastSeen) {
		t.Errorf("LastSeen = %v, want %v (cluster 内最新 finished_at)", cluster.LastSeen, wantLastSeen)
	}
}

// TestReaderFailureClustersTruncatedWhenLoadCapped は MaxFailureRowsLoaded を上書きした
// 状態で SQL ロード打ち切りが発生し Truncated=true / LoadedRows == MaxFailureRowsLoaded を
// 返すことを検証する (Req 5.8 / タスク 4.5)。
//
// 既存 reader_sqlite_test.go の TestSQLiteRecorderFailureClustersTruncatedWhenLoadCapped と
// 重複する観測条件だが、4.5 boundary (reader_test.go) で仕様の観測条件を独立に再表明する
// 役割 (4.1〜4.4 と同じ pattern)。
//
// MaxFailureRowsLoaded は var として宣言されているため、save → defer restore パターンで
// 一時的に 10 に上書きする。Go test は同一プロセス内で順次実行され、t.Parallel() を
// 使わない限り他テストへの干渉は発生しない。
func TestReaderFailureClustersTruncatedWhenLoadCapped(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	saved := MaxFailureRowsLoaded
	MaxFailureRowsLoaded = 10
	defer func() { MaxFailureRowsLoaded = saved }()

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	// 1 クラスタに 15 件投入 (MaxFailureRowsLoaded=10 を超える数)。SQL LIMIT で 10 件にクランプ。
	insertFailureClusterFixture(t, ctx, recorder, "T", "provider_error", "claude", "Connection refused", 15, base)

	result, err := recorder.FailureClusters(ctx, FailureOptions{})
	if err != nil {
		t.Fatalf("FailureClusters: %v", err)
	}
	if result.LoadedRows != 10 {
		t.Errorf("LoadedRows = %d, want 10 (MaxFailureRowsLoaded で SQL LIMIT 切り詰め)", result.LoadedRows)
	}
	if !result.Truncated {
		t.Errorf("Truncated = false, want true (LoadedRows == MaxFailureRowsLoaded=10、Req 5.8)")
	}
	if got := len(result.Clusters); got != 1 {
		t.Fatalf("len(Clusters) = %d, want 1", got)
	}
	if got := result.Clusters[0].Count; got != 10 {
		t.Errorf("Clusters[0].Count = %d, want 10 (打ち切り後の母数)", got)
	}
}

// TestReaderFailureClustersFiltersWorkflowID は workflow_id フィルタが該当 workflow に
// 紐づく failure agent_call のみを集計対象にすることを検証する (Req 5.5 / タスク 4.5)。
//
// fixture: wfA に 3 件、wfB に 2 件 (それぞれ独自の workflow_run / agent_call ID)。
//
// 期待:
//   - フィルタなし → 5 件 / 2 クラスタ (wfA error と wfB error で別)
//   - wfA → 3 件 / 1 クラスタ
//   - wfB → 2 件 / 1 クラスタ
//   - 存在しない workflow → 0 件 / 0 クラスタ
func TestReaderFailureClustersFiltersWorkflowID(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	for i := range 3 {
		insertFailureRunWithCall(t, ctx, recorder,
			fmt.Sprintf("filt-wfA-run-%d", i), fmt.Sprintf("filt-wfA-ac-%d", i),
			"wfA", "claude", "provider_error", "wfA error",
			base.Add(time.Duration(i)*time.Minute),
			base.Add(time.Duration(i)*time.Minute+30*time.Second),
		)
	}
	for i := range 2 {
		insertFailureRunWithCall(t, ctx, recorder,
			fmt.Sprintf("filt-wfB-run-%d", i), fmt.Sprintf("filt-wfB-ac-%d", i),
			"wfB", "claude", "provider_error", "wfB error",
			base.Add(time.Duration(10+i)*time.Minute),
			base.Add(time.Duration(10+i)*time.Minute+30*time.Second),
		)
	}

	cases := []struct {
		name         string
		workflowID   string
		wantCount    int
		wantClusters int
	}{
		{"フィルタなし → 5 件 / 2 クラスタ", "", 5, 2},
		{"wfA のみ → 3 件 / 1 クラスタ", "wfA", 3, 1},
		{"wfB のみ → 2 件 / 1 クラスタ", "wfB", 2, 1},
		{"存在しない workflow → 0 件 / 0 クラスタ", "wfMissing", 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, err := recorder.FailureClusters(ctx, FailureOptions{WorkflowID: c.workflowID})
			if err != nil {
				t.Fatalf("FailureClusters(workflow=%q): %v", c.workflowID, err)
			}
			if result.LoadedRows != c.wantCount {
				t.Errorf("LoadedRows = %d, want %d", result.LoadedRows, c.wantCount)
			}
			if got := len(result.Clusters); got != c.wantClusters {
				t.Errorf("len(Clusters) = %d, want %d", got, c.wantClusters)
			}
		})
	}
}

// TestReaderFailureClustersFiltersProviderID は provider_id フィルタが該当 provider に
// 紐づく failure agent_call のみを集計対象にすることを検証する (Req 5.5 / タスク 4.5)。
//
// fixture: claude provider に 3 件、codex provider に 2 件。
func TestReaderFailureClustersFiltersProviderID(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	for i := range 3 {
		insertFailureRunWithCall(t, ctx, recorder,
			fmt.Sprintf("provf-claude-run-%d", i), fmt.Sprintf("provf-claude-ac-%d", i),
			"wf", "claude", "provider_error", "claude error",
			base.Add(time.Duration(i)*time.Minute),
			base.Add(time.Duration(i)*time.Minute+30*time.Second),
		)
	}
	for i := range 2 {
		insertFailureRunWithCall(t, ctx, recorder,
			fmt.Sprintf("provf-codex-run-%d", i), fmt.Sprintf("provf-codex-ac-%d", i),
			"wf", "codex", "external_abort", "codex error",
			base.Add(time.Duration(10+i)*time.Minute),
			base.Add(time.Duration(10+i)*time.Minute+30*time.Second),
		)
	}

	cases := []struct {
		name         string
		providerID   string
		wantCount    int
		wantClusters int
	}{
		{"フィルタなし → 5 件 / 2 クラスタ", "", 5, 2},
		{"claude のみ → 3 件 / 1 クラスタ", "claude", 3, 1},
		{"codex のみ → 2 件 / 1 クラスタ", "codex", 2, 1},
		{"存在しない provider → 0 件 / 0 クラスタ", "missing", 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, err := recorder.FailureClusters(ctx, FailureOptions{ProviderID: c.providerID})
			if err != nil {
				t.Fatalf("FailureClusters(provider=%q): %v", c.providerID, err)
			}
			if result.LoadedRows != c.wantCount {
				t.Errorf("LoadedRows = %d, want %d", result.LoadedRows, c.wantCount)
			}
			if got := len(result.Clusters); got != c.wantClusters {
				t.Errorf("len(Clusters) = %d, want %d", got, c.wantClusters)
			}
		})
	}
}

// TestReaderFailureClustersFiltersTimeRange は期間フィルタ (Range.Since / Range.Until) が
// agent_calls.started_at に対して inclusive で適用されることを検証する (Req 5.5 / タスク 4.5)。
//
// fixture: 3 件の failure call を runStartedAt = base / base+1h / base+2h で投入する。
// 各 agent_call.started_at は runStartedAt + 1 秒 (insertFailureRunWithCall の仕様)。
//
// 期間フィルタは ac.started_at に対して `>= since AND <= until` の inclusive 比較を行う
// (queries.go の querySelectFailureRows 参照)。
func TestReaderFailureClustersFiltersTimeRange(t *testing.T) {
	ctx := context.Background()
	recorder := openTestReader(t)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	for i := range 3 {
		runStartedAt := base.Add(time.Duration(i) * time.Hour)
		insertFailureRunWithCall(t, ctx, recorder,
			fmt.Sprintf("tr-run-%d", i), fmt.Sprintf("tr-ac-%d", i),
			"wf", "claude", "provider_error", fmt.Sprintf("error-%d", i),
			runStartedAt, runStartedAt.Add(30*time.Second),
		)
	}

	cases := []struct {
		name      string
		since     time.Time
		until     time.Time
		wantCount int
	}{
		{"フィルタなし → 3 件", time.Time{}, time.Time{}, 3},
		// since=base+30min: started_at >= base+30min を満たすのは i=1 (base+1h+1s) と i=2 (base+2h+1s)。
		{"since=base+30min → 2 件 (1h, 2h)", base.Add(30 * time.Minute), time.Time{}, 2},
		// until=base+90min: started_at <= base+90min を満たすのは i=0 (base+1s) と i=1 (base+1h+1s)。
		{"until=base+90min → 2 件 (0, 1h)", time.Time{}, base.Add(90 * time.Minute), 2},
		// since=base+30min かつ until=base+90min: i=1 のみが範囲内。
		{"since=base+30min until=base+90min → 1 件", base.Add(30 * time.Minute), base.Add(90 * time.Minute), 1},
		// since=base+3h: 全 started_at が範囲外。
		{"since=base+3h → 0 件 (範囲外)", base.Add(3 * time.Hour), time.Time{}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, err := recorder.FailureClusters(ctx, FailureOptions{
				Range: TimeRange{Since: c.since, Until: c.until},
			})
			if err != nil {
				t.Fatalf("FailureClusters(since=%v until=%v): %v", c.since, c.until, err)
			}
			if result.LoadedRows != c.wantCount {
				t.Errorf("LoadedRows = %d, want %d (since=%v until=%v)",
					result.LoadedRows, c.wantCount, c.since, c.until)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// 補助ヘルパ
// -----------------------------------------------------------------------------

// makeDescIDRange は "prefix-NN" 形式の id 列を from から to まで降順で生成する
// (例: makeDescIDRange("page-run-", 29, 20) → ["page-run-29", ..., "page-run-20"])。
//
// page-run-NN の NN は 0 埋め 2 桁で、List テストの 30 件 fixture と一致させる。
func makeDescIDRange(prefix string, from, to int) []string {
	if from < to {
		from, to = to, from
	}
	out := make([]string, 0, from-to+1)
	for i := from; i >= to; i-- {
		out = append(out, fmt.Sprintf("%s%02d", prefix, i))
	}
	return out
}
