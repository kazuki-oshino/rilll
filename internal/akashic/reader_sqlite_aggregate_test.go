// Package akashic の reader_sqlite_aggregate_test.go は、reader_sqlite_aggregate.go
// (AggregateByWorkflow / AggregateByProvider) の TDD discipline を担保する companion test。
//
// 各タスクの「観測可能な完了条件」を最小限の smoke test として担保し、
// 包括的な table-driven 検証はタスク 4.2 / 4.3 で reader_test.go に追加される想定。
package akashic

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// insertAggregateWorkflowFixture はタスク 2.2 の観測可能な完了条件を担保するための
// 共通 fixture を投入するヘルパ。
//
// 投入内容: 1 workflow につき 5 run × 1 agent_call。
//   - successCount 件は success 扱い (run.error 空 / agent_call.success=1)、input_tokens=10,
//     output_tokens=20, total_cost_usd=0.01 を埋める
//   - 残り (5 - successCount) 件は failure 扱い (run.error 非空 / agent_call.success=0)、
//     input_tokens / output_tokens / total_cost_usd は NULL のまま (Req 3.7 検証用)
//
// duration_ms は全件 100 を埋め、percentile 算出時に決定的な値を返すようにする。
func insertAggregateWorkflowFixture(
	t *testing.T,
	ctx context.Context,
	recorder *SQLiteRecorder,
	workflowID string,
	successCount int,
	totalCount int,
	base time.Time,
) {
	t.Helper()
	for i := range totalCount {
		runID := fmt.Sprintf("%s-run-%d", workflowID, i)
		started := base.Add(time.Duration(i) * time.Minute)
		if err := recorder.RecordWorkflowRunStart(ctx, WorkflowRun{
			ID:           runID,
			WorkflowID:   workflowID,
			WorkflowName: workflowID,
			CWD:          "/repo",
			StartedAt:    started,
		}); err != nil {
			t.Fatalf("RecordWorkflowRunStart %s: %v", runID, err)
		}
		isFailure := i >= successCount
		runError := ""
		if isFailure {
			runError = "fixture-failure"
		}
		if err := recorder.RecordWorkflowRunFinish(ctx, WorkflowRunFinish{
			ID:         runID,
			Error:      runError,
			FinishedAt: started.Add(time.Second),
		}); err != nil {
			t.Fatalf("RecordWorkflowRunFinish %s: %v", runID, err)
		}
		callID := fmt.Sprintf("%s-call-%d", workflowID, i)
		if err := recorder.RecordAgentCallStart(ctx, AgentCall{
			ID:            callID,
			WorkflowRunID: runID,
			WorkflowID:    workflowID,
			WorkflowName:  workflowID,
			StepID:        "s1",
			StepName:      "S1",
			ProviderID:    "claude",
			CWD:           "/repo",
			StartedAt:     started.Add(100 * time.Millisecond),
		}); err != nil {
			t.Fatalf("RecordAgentCallStart %s: %v", callID, err)
		}
		// 成功時はトークン / コストを埋める。失敗時は NULL のまま (Req 3.7 検証用)。
		var (
			inputTokens  sql.NullInt64
			outputTokens sql.NullInt64
			costUSD      sql.NullFloat64
		)
		if !isFailure {
			inputTokens = sql.NullInt64{Int64: 10, Valid: true}
			outputTokens = sql.NullInt64{Int64: 20, Valid: true}
			costUSD = sql.NullFloat64{Float64: 0.01, Valid: true}
		}
		if err := recorder.RecordAgentCallFinish(ctx, AgentCallResult{
			ID:           callID,
			SessionID:    "sess",
			Success:      !isFailure,
			DurationMS:   100,
			TotalCostUSD: costUSD,
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			FinishedAt:   started.Add(time.Second).Add(-100 * time.Millisecond),
		}); err != nil {
			t.Fatalf("RecordAgentCallFinish %s: %v", callID, err)
		}
	}
}

// TestSQLiteRecorderAggregateByWorkflowReturnsSuccessRateAndSumTokens はタスク 2.2 の
// 観測可能な完了条件を担保する smoke test:
//
//	2 workflow × 5 run の fixture (wfA: 4 success + 1 failure / wfB: 3 success + 2 failure)
//	で AggregateByWorkflow が
//	  - SuccessRate を 0.8 / 0.6 で返す (Req 3.1)
//	  - SumInputTokens / SumOutputTokens を NULL 込みでも数値で正しく集計する (Req 3.4 / 3.7)
//	  - workflow_id ASC で安定ソートして返す (Req 3.9 冪等性)
//	を検証する。
//
// 包括的なテストはタスク 4.2 で reader_test.go に追加される想定。
func TestSQLiteRecorderAggregateByWorkflowReturnsSuccessRateAndSumTokens(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	insertAggregateWorkflowFixture(t, ctx, recorder, "wfA", 4, 5, base)
	insertAggregateWorkflowFixture(t, ctx, recorder, "wfB", 3, 5, base.Add(10*time.Minute))

	result, err := recorder.AggregateByWorkflow(ctx, WorkflowAggregateOptions{})
	if err != nil {
		t.Fatalf("AggregateByWorkflow: %v", err)
	}
	if got := len(result.Buckets); got != 2 {
		t.Fatalf("len(Buckets) = %d, want 2", got)
	}

	// workflow_id ASC: wfA, wfB の順 (Req 3.9 冪等性)。
	bA := result.Buckets[0]
	if bA.WorkflowID != "wfA" {
		t.Fatalf("Buckets[0].WorkflowID = %q, want wfA", bA.WorkflowID)
	}
	if bA.RunCount != 5 {
		t.Errorf("Buckets[0].RunCount = %d, want 5", bA.RunCount)
	}
	if bA.SuccessRate != 0.8 {
		t.Errorf("Buckets[0].SuccessRate = %v, want 0.8 (4/5)", bA.SuccessRate)
	}
	if bA.SumInputTokens != 40 {
		t.Errorf("Buckets[0].SumInputTokens = %d, want 40 (4*10、NULL 除外)", bA.SumInputTokens)
	}
	if bA.SumOutputTokens != 80 {
		t.Errorf("Buckets[0].SumOutputTokens = %d, want 80 (4*20、NULL 除外)", bA.SumOutputTokens)
	}
	// AvgCostUSD は 4 件の 0.01 平均 = 0.01 (NULL 除外)。
	if bA.AvgCostUSD == nil {
		t.Errorf("Buckets[0].AvgCostUSD = nil, want non-nil (4 件の cost が非 NULL)")
	} else if *bA.AvgCostUSD != 0.01 {
		t.Errorf("Buckets[0].AvgCostUSD = %v, want 0.01", *bA.AvgCostUSD)
	}
	// duration_ms は全 5 件 100 で揃えているため p50=p95=100。
	if bA.P50DurationMS == nil || *bA.P50DurationMS != 100 {
		t.Errorf("Buckets[0].P50DurationMS = %v, want 100", bA.P50DurationMS)
	}
	if bA.P95DurationMS == nil || *bA.P95DurationMS != 100 {
		t.Errorf("Buckets[0].P95DurationMS = %v, want 100", bA.P95DurationMS)
	}

	bB := result.Buckets[1]
	if bB.WorkflowID != "wfB" {
		t.Fatalf("Buckets[1].WorkflowID = %q, want wfB", bB.WorkflowID)
	}
	if bB.RunCount != 5 {
		t.Errorf("Buckets[1].RunCount = %d, want 5", bB.RunCount)
	}
	if bB.SuccessRate != 0.6 {
		t.Errorf("Buckets[1].SuccessRate = %v, want 0.6 (3/5)", bB.SuccessRate)
	}
	if bB.SumInputTokens != 30 {
		t.Errorf("Buckets[1].SumInputTokens = %d, want 30 (3*10、NULL 除外)", bB.SumInputTokens)
	}
	if bB.SumOutputTokens != 60 {
		t.Errorf("Buckets[1].SumOutputTokens = %d, want 60 (3*20、NULL 除外)", bB.SumOutputTokens)
	}

	// Req 3.9: 同一パラメータで二回呼び出して結果が等しい (冪等性)。
	result2, err := recorder.AggregateByWorkflow(ctx, WorkflowAggregateOptions{})
	if err != nil {
		t.Fatalf("AggregateByWorkflow (2nd call): %v", err)
	}
	if len(result2.Buckets) != len(result.Buckets) {
		t.Fatalf("二回目の Buckets len = %d, 一回目 = %d (冪等性破れ)",
			len(result2.Buckets), len(result.Buckets))
	}
	for i := range result.Buckets {
		if result.Buckets[i].WorkflowID != result2.Buckets[i].WorkflowID {
			t.Errorf("二回目 Buckets[%d].WorkflowID = %q, 一回目 = %q",
				i, result2.Buckets[i].WorkflowID, result.Buckets[i].WorkflowID)
		}
		if result.Buckets[i].SuccessRate != result2.Buckets[i].SuccessRate {
			t.Errorf("二回目 Buckets[%d].SuccessRate = %v, 一回目 = %v",
				i, result2.Buckets[i].SuccessRate, result.Buckets[i].SuccessRate)
		}
	}
}

// TestSQLiteRecorderAggregateByWorkflowEmptyReturnsNilBuckets は集計対象なしのとき
// Buckets == nil で返ることを検証する (Req 3.8: 該当データなしで空集計)。
func TestSQLiteRecorderAggregateByWorkflowEmptyReturnsNilBuckets(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	result, err := recorder.AggregateByWorkflow(ctx, WorkflowAggregateOptions{})
	if err != nil {
		t.Fatalf("AggregateByWorkflow: %v", err)
	}
	if result.Buckets != nil {
		t.Fatalf("Buckets = %v, want nil (集計対象なし、Req 3.8)", result.Buckets)
	}
}

// insertAggregateProviderFixture はタスク 2.3 の観測可能な完了条件を担保するための
// 共通 fixture を投入するヘルパ。
//
// 投入内容: providerID で識別される agent_calls を totalCount 件、同一 workflow_run
// 配下に作る (provider 集計母数は agent_call 横断のため、workflow_run は 1 つで十分)。
//
//   - successCount 件は success=true、duration_ms=100、input_tokens=10、output_tokens=20、
//     total_cost_usd=0.01 を埋める
//   - 残り (totalCount - successCount) 件は success=false、duration_ms=200、
//     total_cost_usd / input_tokens / output_tokens は NULL のまま (Req 3.7 検証用)
//
// 同 providerID で複数回呼ぶと同じ workflow_run を共有することを避けるため、
// providerID を workflow_run_id にも含めて衝突を防ぐ。base 時刻を引数で取り、
// agent_calls.started_at を分単位で散らすことで filter テストの土台にも使える。
func insertAggregateProviderFixture(
	t *testing.T,
	ctx context.Context,
	recorder *SQLiteRecorder,
	providerID string,
	successCount int,
	totalCount int,
	base time.Time,
) {
	t.Helper()
	runID := fmt.Sprintf("run-%s", providerID)
	if err := recorder.RecordWorkflowRunStart(ctx, WorkflowRun{
		ID:           runID,
		WorkflowID:   "wf",
		WorkflowName: "WF",
		CWD:          "/repo",
		StartedAt:    base,
	}); err != nil {
		t.Fatalf("RecordWorkflowRunStart %s: %v", runID, err)
	}
	for i := range totalCount {
		callID := fmt.Sprintf("ac-%s-%d", providerID, i)
		started := base.Add(time.Duration(i) * time.Minute)
		if err := recorder.RecordAgentCallStart(ctx, AgentCall{
			ID:            callID,
			WorkflowRunID: runID,
			WorkflowID:    "wf",
			WorkflowName:  "WF",
			StepID:        "s1",
			StepName:      "S1",
			ProviderID:    providerID,
			CWD:           "/repo",
			StartedAt:     started,
		}); err != nil {
			t.Fatalf("RecordAgentCallStart %s: %v", callID, err)
		}
		isFailure := i >= successCount
		var (
			inputTokens  sql.NullInt64
			outputTokens sql.NullInt64
			costUSD      sql.NullFloat64
			durationMS   int64 = 100
		)
		if isFailure {
			durationMS = 200
		} else {
			inputTokens = sql.NullInt64{Int64: 10, Valid: true}
			outputTokens = sql.NullInt64{Int64: 20, Valid: true}
			costUSD = sql.NullFloat64{Float64: 0.01, Valid: true}
		}
		if err := recorder.RecordAgentCallFinish(ctx, AgentCallResult{
			ID:           callID,
			SessionID:    "sess",
			Success:      !isFailure,
			DurationMS:   durationMS,
			TotalCostUSD: costUSD,
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			FinishedAt:   started.Add(time.Second),
		}); err != nil {
			t.Fatalf("RecordAgentCallFinish %s: %v", callID, err)
		}
	}
}

// TestSQLiteRecorderAggregateByProviderReturnsSuccessRateAndCallCount はタスク 2.3 の
// 観測可能な完了条件を担保する smoke test:
//
//	2 provider × 10 agent_call (providerA: 7 成功 / 3 失敗、providerB: 5 成功 / 5 失敗)
//	の fixture で AggregateByProvider が
//	  - SuccessRate を 0.7 / 0.5 で返す (Req 3.5)
//	  - 両 provider の CallCount 合算 = フィルタ範囲内 agent_call 総数 (20) と一致する
//	    (Req 3.5 集計不変条件)
//	  - provider_id ASC で安定ソートして返す (Req 3.9 冪等性)
//	を検証する。
//
// 包括的なテスト (workflow_id 追加絞り込み / NULL only AvgCost / 複数 provider の
// percentile 揃え 等) はタスク 4.3 で reader_test.go に追加される想定。
func TestSQLiteRecorderAggregateByProviderReturnsSuccessRateAndCallCount(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	insertAggregateProviderFixture(t, ctx, recorder, "providerA", 7, 10, base)
	insertAggregateProviderFixture(t, ctx, recorder, "providerB", 5, 10, base.Add(time.Hour))

	result, err := recorder.AggregateByProvider(ctx, ProviderAggregateOptions{})
	if err != nil {
		t.Fatalf("AggregateByProvider: %v", err)
	}
	if got := len(result.Buckets); got != 2 {
		t.Fatalf("len(Buckets) = %d, want 2", got)
	}

	// provider_id ASC: providerA, providerB (Req 3.9 冪等性)。
	bA := result.Buckets[0]
	if bA.ProviderID != "providerA" {
		t.Fatalf("Buckets[0].ProviderID = %q, want providerA", bA.ProviderID)
	}
	if bA.CallCount != 10 {
		t.Errorf("Buckets[0].CallCount = %d, want 10", bA.CallCount)
	}
	if bA.SuccessRate != 0.7 {
		t.Errorf("Buckets[0].SuccessRate = %v, want 0.7 (7/10)", bA.SuccessRate)
	}
	// AvgCostUSD: 7 件の 0.01 平均 = 0.01 (NULL 除外)。
	if bA.AvgCostUSD == nil {
		t.Errorf("Buckets[0].AvgCostUSD = nil, want non-nil")
	} else if *bA.AvgCostUSD != 0.01 {
		t.Errorf("Buckets[0].AvgCostUSD = %v, want 0.01", *bA.AvgCostUSD)
	}
	// SumInputTokens / SumOutputTokens は NULL 除外で 7*10=70 / 7*20=140。
	if bA.SumInputTokens != 70 {
		t.Errorf("Buckets[0].SumInputTokens = %d, want 70 (7*10、NULL 除外)", bA.SumInputTokens)
	}
	if bA.SumOutputTokens != 140 {
		t.Errorf("Buckets[0].SumOutputTokens = %d, want 140 (7*20、NULL 除外)", bA.SumOutputTokens)
	}
	// duration_ms は成功 7 件 100 / 失敗 3 件 200 = ソート後 [100,100,100,100,100,100,100,200,200,200]。
	// p50 (= index 4.5 を線形補間) → 100、p95 (= index 8.55 を線形補間) → round(0.45*200 + 0.55*200) = 200。
	if bA.P50DurationMS == nil || *bA.P50DurationMS != 100 {
		t.Errorf("Buckets[0].P50DurationMS = %v, want 100", bA.P50DurationMS)
	}
	if bA.P95DurationMS == nil || *bA.P95DurationMS != 200 {
		t.Errorf("Buckets[0].P95DurationMS = %v, want 200", bA.P95DurationMS)
	}

	bB := result.Buckets[1]
	if bB.ProviderID != "providerB" {
		t.Fatalf("Buckets[1].ProviderID = %q, want providerB", bB.ProviderID)
	}
	if bB.CallCount != 10 {
		t.Errorf("Buckets[1].CallCount = %d, want 10", bB.CallCount)
	}
	if bB.SuccessRate != 0.5 {
		t.Errorf("Buckets[1].SuccessRate = %v, want 0.5 (5/10)", bB.SuccessRate)
	}

	// Req 3.5 集計不変条件: 全 provider の CallCount 合算 = フィルタ範囲内の agent_call 総数 (20)。
	var sumCallCount int64
	for _, b := range result.Buckets {
		sumCallCount += b.CallCount
	}
	if sumCallCount != 20 {
		t.Errorf("Σ CallCount = %d, want 20 (集計不変条件: provider 合算 = フィルタ範囲内 agent_call 総数)", sumCallCount)
	}

	// Req 3.9: 同一パラメータで二回呼び出して結果が等しい (冪等性)。
	result2, err := recorder.AggregateByProvider(ctx, ProviderAggregateOptions{})
	if err != nil {
		t.Fatalf("AggregateByProvider (2nd call): %v", err)
	}
	if len(result2.Buckets) != len(result.Buckets) {
		t.Fatalf("二回目の Buckets len = %d, 一回目 = %d (冪等性破れ)",
			len(result2.Buckets), len(result.Buckets))
	}
	for i := range result.Buckets {
		if result.Buckets[i].ProviderID != result2.Buckets[i].ProviderID {
			t.Errorf("二回目 Buckets[%d].ProviderID = %q, 一回目 = %q",
				i, result2.Buckets[i].ProviderID, result.Buckets[i].ProviderID)
		}
		if result.Buckets[i].SuccessRate != result2.Buckets[i].SuccessRate {
			t.Errorf("二回目 Buckets[%d].SuccessRate = %v, 一回目 = %v",
				i, result2.Buckets[i].SuccessRate, result.Buckets[i].SuccessRate)
		}
	}
}

// TestSQLiteRecorderAggregateByProviderEmptyReturnsNilBuckets は集計対象なしのとき
// Buckets == nil で返ることを検証する (Req 3.8: 該当データなしで空集計)。
func TestSQLiteRecorderAggregateByProviderEmptyReturnsNilBuckets(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	result, err := recorder.AggregateByProvider(ctx, ProviderAggregateOptions{})
	if err != nil {
		t.Fatalf("AggregateByProvider: %v", err)
	}
	if result.Buckets != nil {
		t.Fatalf("Buckets = %v, want nil (集計対象なし、Req 3.8)", result.Buckets)
	}
}
