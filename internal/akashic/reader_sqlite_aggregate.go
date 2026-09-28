// Package akashic の reader_sqlite_aggregate.go は、Reader interface (reader.go) のうち
// 集計系メソッド (AggregateByWorkflow / AggregateByProvider) の実装を提供する。
//
// duration_ms の percentile 計算用ヘルパ parseDurationsCSV は本ファイル内で定義し、
// reader_internal.go の percentile() に sorted []int64 として渡す。
package akashic

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// AggregateByWorkflow は workflow_id 単位で workflow_run / agent_call の指標を集計する
// (Req 3.1〜3.4, 3.6, 3.7, 3.8, 3.9, 7.5)。
//
// 動作概要:
//  1. opts.Validate() で options を検証 (現状追加検証なしだが将来フックを尊重)
//  2. 2 クエリ発行:
//     - querySelectWorkflowAggregateRuns: workflow_runs 母数の COUNT(*) / 成功 run 数
//     - querySelectWorkflowAggregateCalls: agent_calls の AVG(cost) / SUM(tokens) /
//     GROUP_CONCAT(duration_ms)
//  3. workflow_id をキーに 2 結果をマージ。母数 (workflow_runs) 側にしか現れない
//     workflow (= 当該 workflow に agent_call が 1 件もない) は AvgCost/Sum*/percentile を
//     nil/0 のまま返す
//  4. duration_ms の GROUP_CONCAT を Go 側で split + sort し percentile ヘルパで p50/p95
//     を算出 (SQLite に PERCENTILE_CONT がないため Go 側計算)
//  5. 集計対象なし (workflow_run が 1 件もない) → WorkflowAggregateResult{Buckets: nil}
//     を返す (Req 3.8: エラーにせず空集計)
//
// 派生定義:
//   - SuccessRate: success_count / RunCount (RunCount==0 のとき 0)。
//     成功 run の SQL 派生は queries.go の `error IS NULL AND finished_at IS NOT NULL`
//     (workflow_runs テーブル単独で完結し agent_calls は参照しない、Req 3.1)
//   - AvgCostUSD: NULL のみ / 該当なし → nil。SQLite の AVG が NULL を母数除外する
//     ため Req 3.7 を自動的に満たす
//   - SumInputTokens / SumOutputTokens: NULL only / 該当なしのとき 0 を返す。SQL の SUM が
//     全 NULL のとき NULL を返すため、Go 側で Valid=false → 0 にマッピングする
//
// 並び順: querySelectWorkflowAggregateRuns の ORDER BY workflow_id ASC で安定化される
// 前提。これにより同一パラメータでの再呼び出しでも結果が等しい (Req 3.9 冪等性)。
func (r *SQLiteRecorder) AggregateByWorkflow(ctx context.Context, opts WorkflowAggregateOptions) (WorkflowAggregateResult, error) {
	if err := opts.Validate(); err != nil {
		return WorkflowAggregateResult{}, err
	}

	// queries.go の sentinel-binding パターン: 空文字 sentinel が真なら絞り込みなし。
	sinceSentinel, sinceValue := timeSentinelAndValue(opts.Range.Since)
	untilSentinel, untilValue := timeSentinelAndValue(opts.Range.Until)
	workflowSentinel := opts.WorkflowID
	providerSentinel := opts.ProviderID
	projectSentinel := opts.ProjectID

	// Step 1: workflow_runs 母数集計 (run_count / success_count) を取得。
	// 集計順序の真のソース (workflowOrder) は本クエリの ORDER BY workflow_id ASC を採用。
	type runStats struct {
		RunCount     int64
		SuccessCount int64
	}
	runStatsByID := make(map[string]runStats)
	var workflowOrder []string
	{
		rows, err := r.db.QueryContext(ctx, querySelectWorkflowAggregateRuns,
			sinceSentinel, sinceValue,
			untilSentinel, untilValue,
			workflowSentinel, opts.WorkflowID,
			providerSentinel, opts.ProviderID,
			projectSentinel, opts.ProjectID,
		)
		if err != nil {
			return WorkflowAggregateResult{}, fmt.Errorf("akashic: aggregate workflow runs: %w", err)
		}
		// rows.Close() は内側 block 終了時に確実に呼ぶ。defer は外側関数終了まで遅延する
		// ため意図的にスコープを区切って早期 Close を行う (callRows の Open 前にリソース解放)。
		err = func() error {
			defer rows.Close()
			for rows.Next() {
				var (
					workflowID           string
					runCount             int64
					successCountNullable sql.NullInt64 // SUM(CASE...) は GROUP の対象が空のとき NULL を返すため NullInt64 で受ける
				)
				if err := rows.Scan(&workflowID, &runCount, &successCountNullable); err != nil {
					return fmt.Errorf("akashic: aggregate workflow runs scan: %w", err)
				}
				if _, ok := runStatsByID[workflowID]; !ok {
					workflowOrder = append(workflowOrder, workflowID)
				}
				stats := runStats{RunCount: runCount}
				if successCountNullable.Valid {
					stats.SuccessCount = successCountNullable.Int64
				}
				runStatsByID[workflowID] = stats
			}
			return rows.Err()
		}()
		if err != nil {
			return WorkflowAggregateResult{}, fmt.Errorf("akashic: aggregate workflow runs iter: %w", err)
		}
	}

	// 集計対象 workflow_run が 0 件なら空集計を返す (Req 3.8)。Buckets を nil で返す
	// ことで「対象なし」を呼び出し側が明示的に判定できる。
	if len(workflowOrder) == 0 {
		return WorkflowAggregateResult{Buckets: nil}, nil
	}

	// Step 2: agent_calls 側統計 (AVG cost / SUM tokens / GROUP_CONCAT durations) を取得。
	type callStats struct {
		AvgCost         sql.NullFloat64
		SumInputTokens  sql.NullInt64
		SumOutputTokens sql.NullInt64
		Durations       sql.NullString // GROUP_CONCAT 結果。NULL は空配列扱い
	}
	callStatsByID := make(map[string]callStats)
	{
		rows, err := r.db.QueryContext(ctx, querySelectWorkflowAggregateCalls,
			sinceSentinel, sinceValue,
			untilSentinel, untilValue,
			workflowSentinel, opts.WorkflowID,
			providerSentinel, opts.ProviderID,
			projectSentinel, opts.ProjectID,
		)
		if err != nil {
			return WorkflowAggregateResult{}, fmt.Errorf("akashic: aggregate workflow calls: %w", err)
		}
		err = func() error {
			defer rows.Close()
			for rows.Next() {
				var (
					workflowID string
					stats      callStats
				)
				if err := rows.Scan(&workflowID, &stats.AvgCost, &stats.SumInputTokens, &stats.SumOutputTokens, &stats.Durations); err != nil {
					return fmt.Errorf("akashic: aggregate workflow calls scan: %w", err)
				}
				callStatsByID[workflowID] = stats
			}
			return rows.Err()
		}()
		if err != nil {
			return WorkflowAggregateResult{}, fmt.Errorf("akashic: aggregate workflow calls iter: %w", err)
		}
	}

	// Step 3: workflow_id 単位にマージ。出力順序は runStatsByID 構築時の workflowOrder
	// (= querySelectWorkflowAggregateRuns の ORDER BY workflow_id ASC) を採用する。
	// 当該 workflow に agent_call が 1 件もない場合 callStatsByID にエントリは無く、
	// AvgCost/Sum* は zero value (nil/0)、p50/p95 も nil で返る。
	buckets := make([]WorkflowAggregateBucket, 0, len(workflowOrder))
	for _, workflowID := range workflowOrder {
		rs := runStatsByID[workflowID]
		cs := callStatsByID[workflowID] // 存在しなければ zero value

		bucket := WorkflowAggregateBucket{
			WorkflowID: workflowID,
			RunCount:   rs.RunCount,
		}
		if rs.RunCount > 0 {
			bucket.SuccessRate = float64(rs.SuccessCount) / float64(rs.RunCount)
		}
		// AvgCostUSD: SQLite の AVG が NULL only のとき NULL を返す → nil で伝搬。
		bucket.AvgCostUSD = nullFloat64Ptr(cs.AvgCost)
		// SumInputTokens / SumOutputTokens: SQLite の SUM が NULL only のとき NULL を返す
		// → bucket では 0 で表現する (Req 3.7: NULL を 0 と区別する判定は AvgCost で行う)。
		if cs.SumInputTokens.Valid {
			bucket.SumInputTokens = cs.SumInputTokens.Int64
		}
		if cs.SumOutputTokens.Valid {
			bucket.SumOutputTokens = cs.SumOutputTokens.Int64
		}
		// p50/p95 を Go 側計算 (SQLite に PERCENTILE_CONT がないため)。
		durations := parseDurationsCSV(cs.Durations)
		bucket.P50DurationMS = percentile(durations, 0.5)
		bucket.P95DurationMS = percentile(durations, 0.95)

		buckets = append(buckets, bucket)
	}

	return WorkflowAggregateResult{Buckets: buckets}, nil
}

// AggregateByProvider は provider_id 単位で agent_call の指標を集計する
// (Req 3.5〜3.9, 7.5)。
//
// 動作概要:
//  1. opts.Validate() で options を検証 (現状追加検証なしだが将来フックを尊重)
//  2. querySelectProviderAggregateBuckets を 1 本発行:
//     - COUNT(*) AS call_count
//     - SUM(success) AS success_count (NULL は in-progress として除外)
//     - AVG(total_cost_usd) / SUM(input_tokens) / SUM(output_tokens) /
//     GROUP_CONCAT(duration_ms)
//  3. 行ごとに ProviderAggregateBucket を組み立てる。母数は agent_call (workflow_run
//     横断) のため、AggregateByWorkflow とは異なり 1 クエリで完結する
//  4. duration_ms の GROUP_CONCAT を Go 側で split + sort し percentile ヘルパで
//     p50/p95 を算出 (parseDurationsCSV を再利用)
//  5. 集計対象なし (agent_call が 1 件もない) → ProviderAggregateResult{Buckets: nil}
//     を返す (Req 3.8: エラーにせず空集計)
//
// 派生定義:
//   - SuccessRate: success_count / CallCount (CallCount==0 のとき 0)。SQL の
//     SUM(success) は success=1 の合計を返すため、NULL (in-progress) は自動除外
//   - AvgCostUSD: NULL only / 該当なし → nil。SQLite の AVG が NULL を母数除外する
//   - SumInputTokens / SumOutputTokens: NULL only / 該当なし → 0。SQL SUM が全 NULL
//     のとき NULL を返すため、Go 側で Valid=false → 0 にマッピングする
//
// 集計不変条件 (Req 3.5): 全 provider の CallCount 合算 = フィルタ範囲内の agent_call
// 総数。SQL は agent_calls 母数を直接 GROUP BY ac.provider_id しているため自動的に
// 成立する。
//
// 並び順: querySelectProviderAggregateBuckets の ORDER BY provider_id ASC で安定化
// される前提。同一パラメータでの再呼び出しでも結果が等しい (Req 3.9 冪等性)。
func (r *SQLiteRecorder) AggregateByProvider(ctx context.Context, opts ProviderAggregateOptions) (ProviderAggregateResult, error) {
	if err := opts.Validate(); err != nil {
		return ProviderAggregateResult{}, err
	}

	// queries.go の sentinel-binding パターン: 空文字 sentinel が真なら絞り込みなし。
	sinceSentinel, sinceValue := timeSentinelAndValue(opts.Range.Since)
	untilSentinel, untilValue := timeSentinelAndValue(opts.Range.Until)
	workflowSentinel := opts.WorkflowID
	providerSentinel := opts.ProviderID
	projectSentinel := opts.ProjectID

	rows, err := r.db.QueryContext(ctx, querySelectProviderAggregateBuckets,
		sinceSentinel, sinceValue,
		untilSentinel, untilValue,
		workflowSentinel, opts.WorkflowID,
		providerSentinel, opts.ProviderID,
		projectSentinel, opts.ProjectID,
	)
	if err != nil {
		return ProviderAggregateResult{}, fmt.Errorf("akashic: aggregate provider: %w", err)
	}
	defer rows.Close()

	var buckets []ProviderAggregateBucket
	for rows.Next() {
		var (
			// agent_calls.provider_id はスキーマ的に NULLABLE (store.go の schema 参照)。
			// 現状の書き込み経路 (RecordAgentCallStart) は string を直接 INSERT するため
			// NULL は発生しないが、defensive に NullString で受けて空文字に正規化する。
			providerID           sql.NullString
			callCount            int64
			successCountNullable sql.NullInt64   // SUM(success) は対象 0 件のとき NULL
			avgCost              sql.NullFloat64 // AVG は全 NULL のとき NULL
			sumInputTokens       sql.NullInt64   // SUM は全 NULL のとき NULL
			sumOutputTokens      sql.NullInt64
			durations            sql.NullString // GROUP_CONCAT は対象 0 件のとき NULL
		)
		if err := rows.Scan(&providerID, &callCount, &successCountNullable, &avgCost, &sumInputTokens, &sumOutputTokens, &durations); err != nil {
			return ProviderAggregateResult{}, fmt.Errorf("akashic: aggregate provider scan: %w", err)
		}

		bucket := ProviderAggregateBucket{
			ProviderID: nullStringValue(providerID),
			CallCount:  callCount,
		}
		// SuccessRate: success_count / CallCount (CallCount==0 のとき 0、Req 3.5 後段)。
		// SQL の SUM(success) は NULL (in-progress) を除外して 1 の合計を返す。
		if callCount > 0 && successCountNullable.Valid {
			bucket.SuccessRate = float64(successCountNullable.Int64) / float64(callCount)
		}
		// AvgCostUSD: SQLite の AVG が NULL only のとき NULL を返す → nil で伝搬 (Req 3.7)。
		bucket.AvgCostUSD = nullFloat64Ptr(avgCost)
		// SumInputTokens / SumOutputTokens: NULL only のとき 0 で表現する (Req 3.7 の
		// "NULL を 0 と区別" は AvgCostUSD で実現する design 意図に合致)。
		if sumInputTokens.Valid {
			bucket.SumInputTokens = sumInputTokens.Int64
		}
		if sumOutputTokens.Valid {
			bucket.SumOutputTokens = sumOutputTokens.Int64
		}
		// p50/p95 を Go 側計算 (parseDurationsCSV を AggregateByWorkflow と共用)。
		ds := parseDurationsCSV(durations)
		bucket.P50DurationMS = percentile(ds, 0.5)
		bucket.P95DurationMS = percentile(ds, 0.95)

		buckets = append(buckets, bucket)
	}
	if err := rows.Err(); err != nil {
		return ProviderAggregateResult{}, fmt.Errorf("akashic: aggregate provider iter: %w", err)
	}

	// 集計対象 agent_call が 0 件なら Buckets nil で返す (Req 3.8)。
	// SQL の GROUP BY は対象行 0 件のとき行を返さないため、buckets は append されず
	// nil のまま。明示的な分岐は不要だが、契約を読み手に伝えるためコメントで残す。
	return ProviderAggregateResult{Buckets: buckets}, nil
}

// parseDurationsCSV は GROUP_CONCAT(duration_ms) の出力 (sql.NullString) をパースし、
// 昇順ソートした []int64 を返す。percentile ヘルパへの入力契約 (ソート済み配列) を
// 満たすための前処理。
//
// 仕様:
//   - Valid=false (SQL の GROUP_CONCAT 結果が NULL — = 全 NULL) → nil
//   - 空文字列 → nil
//   - 数値以外 / パース不能なトークン → 該当トークンをスキップ (defensive)
//
// SQLite の GROUP_CONCAT は NULL 値を除外して連結するため、結果文字列に "NULL" 文字列
// リテラルが現れることは無い (実装定義動作に依存しない)。
func parseDurationsCSV(s sql.NullString) []int64 {
	if !s.Valid || s.String == "" {
		return nil
	}
	parts := strings.Split(s.String, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			// defensive: 不正値はクラスタ集計のキー揺れを避けるためスキップ。
			// 実運用では duration_ms カラムは INTEGER のため到達不能だが、上書き
			// 経路 (e.g. 将来の手動 SQL 実行) を考慮して暴発させない設計とする。
			continue
		}
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}
