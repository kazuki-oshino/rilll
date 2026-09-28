// Package akashic の reader_sqlite_list_test.go は、reader_sqlite_list.go (List 系) の
// TDD discipline を担保する companion test。
//
// 各タスクの「観測可能な完了条件」を最小限の smoke test として担保し、
// 包括的な table-driven 検証はタスク 4.1 で reader_test.go に追加される想定。
package akashic

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSQLiteRecorderListReturnsLimitedRunsInDescOrder はタスク 2.1 の観測可能な完了条件
// を担保する smoke test:
//
//	30 件 fixture 投入後、List(Limit:10, Offset:0) が
//	  - Items[0].StartedAt > Items[9].StartedAt (started_at DESC 並び)
//	  - *NextOffset == 10 (len(Items) == Limit のとき次オフセット)
//	を返すことを検証する。
//
// 包括的なテストはタスク 4.1 で reader_test.go に追加される想定。
func TestSQLiteRecorderListReturnsLimitedRunsInDescOrder(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	// 30 件の workflow_run を 1 分間隔で散らした fixture を投入。
	// run-00 (最古) ... run-29 (最新)。
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	for i := range 30 {
		runID := fmt.Sprintf("run-%02d", i)
		startedAt := base.Add(time.Duration(i) * time.Minute)
		if err := recorder.RecordWorkflowRunStart(ctx, WorkflowRun{
			ID:           runID,
			WorkflowID:   "wf",
			WorkflowName: "WF",
			CWD:          "/repo",
			StartedAt:    startedAt,
		}); err != nil {
			t.Fatalf("RecordWorkflowRunStart[%d]: %v", i, err)
		}
	}

	result, err := recorder.List(ctx, ListOptions{Limit: 10, Offset: 0})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := len(result.Items); got != 10 {
		t.Fatalf("len(Items) = %d, want 10", got)
	}
	if !result.Items[0].StartedAt.After(result.Items[9].StartedAt) {
		t.Fatalf("Items[0].StartedAt (%v) は Items[9].StartedAt (%v) より新しい必要がある (DESC 並び)",
			result.Items[0].StartedAt, result.Items[9].StartedAt)
	}
	// 最新 10 件は run-29 ... run-20 のはず。
	if result.Items[0].RunID != "run-29" {
		t.Fatalf("Items[0].RunID = %q, want run-29 (最新)", result.Items[0].RunID)
	}
	if result.Items[9].RunID != "run-20" {
		t.Fatalf("Items[9].RunID = %q, want run-20 (10 件目)", result.Items[9].RunID)
	}
	if result.NextOffset == nil {
		t.Fatalf("NextOffset = nil, want &10 (len(Items)==Limit のとき次オフセット)")
	}
	if *result.NextOffset != 10 {
		t.Fatalf("*NextOffset = %d, want 10", *result.NextOffset)
	}
	if result.Limit != 10 {
		t.Fatalf("result.Limit = %d, want 10", result.Limit)
	}
	if result.Offset != 0 {
		t.Fatalf("result.Offset = %d, want 0", result.Offset)
	}
}

// TestSQLiteRecorderListFiltersByProjectID は ListOptions.ProjectID で project ごとに
// workflow_run を絞り込めることを確認する。空文字列なら全 project を返し、
// 値指定時はその project_id を持つ run のみが返る。
func TestSQLiteRecorderListFiltersByProjectID(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	fixtures := []struct {
		runID     string
		projectID string
	}{
		{"run-rilll-1", "rilll"},
		{"run-rilll-2", "rilll"},
		{"run-other-1", "other"},
	}
	for i, f := range fixtures {
		if err := recorder.RecordWorkflowRunStart(ctx, WorkflowRun{
			ID:           f.runID,
			WorkflowID:   "wf",
			WorkflowName: "WF",
			CWD:          "/repo/" + f.projectID,
			ProjectID:    f.projectID,
			StartedAt:    base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("RecordWorkflowRunStart[%d]: %v", i, err)
		}
	}

	// 空文字列 → 全 project (3 件)
	all, err := recorder.List(ctx, ListOptions{Limit: 50})
	if err != nil {
		t.Fatalf("List(all): %v", err)
	}
	if got := len(all.Items); got != 3 {
		t.Fatalf("空 ProjectID では全 project が返るべき: got=%d want=3", got)
	}

	// "rilll" 指定 → 2 件
	rilll, err := recorder.List(ctx, ListOptions{Limit: 50, ProjectID: "rilll"})
	if err != nil {
		t.Fatalf("List(rilll): %v", err)
	}
	if got := len(rilll.Items); got != 2 {
		t.Fatalf("ProjectID=rilll では 2 件返るべき: got=%d", got)
	}
	for _, item := range rilll.Items {
		if !strings.HasPrefix(item.RunID, "run-rilll-") {
			t.Fatalf("ProjectID=rilll フィルタ漏れ: RunID=%q", item.RunID)
		}
	}

	// "other" 指定 → 1 件
	other, err := recorder.List(ctx, ListOptions{Limit: 50, ProjectID: "other"})
	if err != nil {
		t.Fatalf("List(other): %v", err)
	}
	if got := len(other.Items); got != 1 || other.Items[0].RunID != "run-other-1" {
		t.Fatalf("ProjectID=other 結果が一致しない: %+v", other.Items)
	}
}

// TestSQLiteRecorderListAppliesDefaultLimit は Limit==0 の場合に DefaultListLimit が
// 適用されることを検証する (Req 2.9: 未指定時の暴走防止)。
//
// 件数 0 件の DB に対して List を呼び、結果の Limit が DefaultListLimit に
// 補完されること、NextOffset が nil (len(Items) < Limit) であることを確認する。
func TestSQLiteRecorderListAppliesDefaultLimit(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	result, err := recorder.List(ctx, ListOptions{Limit: 0, Offset: 0})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if result.Limit != DefaultListLimit {
		t.Fatalf("result.Limit = %d, want DefaultListLimit (%d)", result.Limit, DefaultListLimit)
	}
	if len(result.Items) != 0 {
		t.Fatalf("len(Items) = %d, want 0 (空 DB)", len(result.Items))
	}
	if result.NextOffset != nil {
		t.Fatalf("NextOffset = %v, want nil (len(Items) < Limit)", result.NextOffset)
	}
}

// TestSQLiteRecorderListValidationRejectsBadOptions は ListOptions.Validate ゲートが
// 負値 limit / 上限超過 limit / 負 offset を sentinel error で拒否することを検証する
// (Req 2.10 / 7.4)。
func TestSQLiteRecorderListValidationRejectsBadOptions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	cases := []struct {
		name    string
		opts    ListOptions
		wantErr error
	}{
		{"negative limit", ListOptions{Limit: -1}, ErrInvalidLimit},
		{"limit over max", ListOptions{Limit: MaxListLimit + 1}, ErrInvalidLimit},
		{"negative offset", ListOptions{Offset: -1}, ErrInvalidOffset},
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

// TestSQLiteRecorderListGroupsAgentCallsByRun は LEFT JOIN 結果の Go 側 grouping ロジックが
// workflow_run_id 単位で agent_calls を AgentCallSummary としてまとめ上げることを検証する
// (Req 2.7)。
//
// 検証項目:
//   - run-A (新): 2 agent_calls を保持
//   - run-B (古): agent_calls なし (LEFT JOIN による空配列または nil 配列)
//   - run-A は started_at が新しいため Items[0] に並ぶ
//   - run-A の AgentCalls は started_at ASC (ac-A1 → ac-A2)
func TestSQLiteRecorderListGroupsAgentCallsByRun(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	if err := recorder.RecordWorkflowRunStart(ctx, WorkflowRun{
		ID: "run-A", WorkflowID: "wf", WorkflowName: "WF", CWD: "/repo",
		StartedAt: base.Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("RecordWorkflowRunStart run-A: %v", err)
	}
	if err := recorder.RecordWorkflowRunStart(ctx, WorkflowRun{
		ID: "run-B", WorkflowID: "wf", WorkflowName: "WF", CWD: "/repo",
		StartedAt: base.Add(time.Minute),
	}); err != nil {
		t.Fatalf("RecordWorkflowRunStart run-B: %v", err)
	}
	if err := recorder.RecordAgentCallStart(ctx, AgentCall{
		ID: "ac-A1", WorkflowRunID: "run-A", WorkflowID: "wf", WorkflowName: "WF",
		StepID: "s1", StepName: "S1", ProviderID: "claude", CWD: "/repo",
		StartedAt: base.Add(2 * time.Minute).Add(time.Second),
	}); err != nil {
		t.Fatalf("RecordAgentCallStart ac-A1: %v", err)
	}
	if err := recorder.RecordAgentCallStart(ctx, AgentCall{
		ID: "ac-A2", WorkflowRunID: "run-A", WorkflowID: "wf", WorkflowName: "WF",
		StepID: "s2", StepName: "S2", ProviderID: "codex", CWD: "/repo",
		StartedAt: base.Add(2 * time.Minute).Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("RecordAgentCallStart ac-A2: %v", err)
	}

	result, err := recorder.List(ctx, ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := len(result.Items); got != 2 {
		t.Fatalf("len(Items) = %d, want 2", got)
	}

	// run-A は started_at が新しいため Items[0]
	if result.Items[0].RunID != "run-A" {
		t.Fatalf("Items[0].RunID = %q, want run-A (newer)", result.Items[0].RunID)
	}
	if got := len(result.Items[0].AgentCalls); got != 2 {
		t.Fatalf("Items[0].AgentCalls len = %d, want 2", got)
	}
	if result.Items[0].AgentCalls[0].ID != "ac-A1" {
		t.Fatalf("Items[0].AgentCalls[0].ID = %q, want ac-A1 (started_at ASC)", result.Items[0].AgentCalls[0].ID)
	}
	if result.Items[0].AgentCalls[1].ID != "ac-A2" {
		t.Fatalf("Items[0].AgentCalls[1].ID = %q, want ac-A2", result.Items[0].AgentCalls[1].ID)
	}
	// agent_call の provider_id が scan 経由で正しく取れる (sql.NullString → string)。
	if result.Items[0].AgentCalls[0].ProviderID != "claude" {
		t.Fatalf("Items[0].AgentCalls[0].ProviderID = %q, want claude", result.Items[0].AgentCalls[0].ProviderID)
	}

	// run-B は agent_calls なし (LEFT JOIN で ac.id IS NULL になり、AgentCalls は空)。
	if result.Items[1].RunID != "run-B" {
		t.Fatalf("Items[1].RunID = %q, want run-B", result.Items[1].RunID)
	}
	if got := len(result.Items[1].AgentCalls); got != 0 {
		t.Fatalf("Items[1].AgentCalls len = %d, want 0 (LEFT JOIN no-match)", got)
	}

	// 未完了 run の Success は nil (finished_at IS NULL のため)。
	if result.Items[0].Success != nil {
		t.Fatalf("Items[0].Success = %v, want nil (未完了)", result.Items[0].Success)
	}
	if result.Items[0].FinishedAt != nil {
		t.Fatalf("Items[0].FinishedAt = %v, want nil (未完了)", result.Items[0].FinishedAt)
	}
}
