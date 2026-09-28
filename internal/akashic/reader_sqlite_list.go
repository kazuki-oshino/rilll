// Package akashic の reader_sqlite_list.go は、Reader interface (reader.go) のうち
// List メソッドの実装と、reader_sqlite_*.go 群が共有する低レイヤヘルパを提供する。
//
// 配置方針:
//   - List / scanListRows: Reader.List 実装
//   - timeSentinelAndValue / successSentinelAndValue: queries.go の sentinel-binding
//     パターンに対する共通変換ヘルパ (Aggregate 系・Failure 系からも参照される)
//   - parseStoredTime / parseOptionalStoredTime: SQLite TEXT 形式の時刻文字列を time.Time
//     に復号する共通ヘルパ (Detail 系・Failure 系からも参照される)
//   - deriveRunSuccess: workflow_runs の finished_at と error から RunHeader.Success を
//     派生させる共通ヘルパ (List / Detail で共有)
//
// 元ファイル (reader_sqlite.go) はクエリ族ごとに分割されており、本ファイルは
// `reader_sqlite_aggregate.go` / `reader_sqlite_detail.go` / `reader_sqlite_failure.go`
// と同じ *SQLiteRecorder 型に対するメソッド実装を持つ。
package akashic

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// List は workflow_runs と紐づく agent_calls をフィルタ + ページネーション付きで返す
// (Req 2.1〜2.11, 7.1, 7.3, 7.4)。
//
// 動作概要:
//  1. opts.Validate() で limit/offset の負値・上限超過を sentinel error として弾く
//  2. Limit==0 → DefaultListLimit に補完 (Req 2.9: 未指定時の暴走防止)
//  3. querySelectRunsList を発行。inner subquery で workflow_runs を limit/offset し、
//     その上で LEFT JOIN agent_calls を展開する (N*M 行への展開後 LIMIT 暴発を防ぐ)
//  4. 同一 workflow_run_id のスキャン行を Go 側で group して AgentCallSummary 配列に
//     まとめる。並び順は SQL の ORDER BY (workflow_run は started_at DESC + id DESC、
//     agent_call は started_at ASC + id ASC) で安定化されている前提
//  5. RunHeader.Success は workflow_runs.error と finished_at から派生
//     (true: finished_at NOT NULL かつ error NULL / false: finished_at NOT NULL かつ
//     error NOT NULL / nil: finished_at NULL)
//  6. NextOffset: len(Items) == Limit のとき &(Offset + Limit)、それ以外 nil
func (r *SQLiteRecorder) List(ctx context.Context, opts ListOptions) (ListResult, error) {
	if err := opts.Validate(); err != nil {
		return ListResult{}, err
	}
	limit := opts.Limit
	if limit == 0 {
		limit = DefaultListLimit
	}

	// querySelectRunsList の sentinel/value bind を構築する。
	// sentinel が "" / -1 のとき該当フィルタは適用されない。
	workflowSentinel := opts.WorkflowID
	sinceSentinel, sinceValue := timeSentinelAndValue(opts.Range.Since)
	untilSentinel, untilValue := timeSentinelAndValue(opts.Range.Until)
	successSentinel, successValue := successSentinelAndValue(opts.Success)
	cwdSentinel := opts.CWD
	providerSentinel := opts.ProviderID
	projectSentinel := opts.ProjectID

	rows, err := r.db.QueryContext(ctx, querySelectRunsList,
		workflowSentinel, opts.WorkflowID,
		sinceSentinel, sinceValue,
		untilSentinel, untilValue,
		successSentinel, successValue,
		cwdSentinel, opts.CWD,
		providerSentinel, opts.ProviderID,
		projectSentinel, opts.ProjectID,
		limit, opts.Offset,
	)
	if err != nil {
		return ListResult{}, fmt.Errorf("akashic: list runs: %w", err)
	}
	defer rows.Close()

	items, err := scanListRows(rows)
	if err != nil {
		return ListResult{}, err
	}
	if err := rows.Err(); err != nil {
		return ListResult{}, fmt.Errorf("akashic: list runs iter: %w", err)
	}

	// NextOffset: len(Items) == limit のとき次ページ取得用ヒントを返す (Req 2.8)。
	var nextOffset *int
	if len(items) == limit {
		next := opts.Offset + limit
		nextOffset = &next
	}

	return ListResult{
		Items:      items,
		Limit:      limit,
		Offset:     opts.Offset,
		NextOffset: nextOffset,
	}, nil
}

// scanListRows は querySelectRunsList の rows を workflow_run_id 単位に group して
// []WorkflowRunListItem を構築する。
//
// 入力契約: rows は SQL の ORDER BY によって同一 r.id の行が連続して並んでいること。
// LEFT JOIN による「agent_calls なし」の行 (ac.* 全 NULL) も grouping 対象に含まれ、
// 該当 run の AgentCalls は空のままで処理する。
func scanListRows(rows *sql.Rows) ([]WorkflowRunListItem, error) {
	var items []WorkflowRunListItem
	// lastRunID と lastIdx で group 境界を追跡する。runID は実運用で空文字に
	// ならないため、""(空文字) は「未初期化」sentinel として安全に使える。
	lastRunID := ""
	lastIdx := -1

	for rows.Next() {
		var (
			// workflow_runs スカラ列 (NOT NULL: id / workflow_id / workflow_name / cwd / started_at)
			runID, workflowID, workflowName, cwd, startedAtStr string
			// nullable 列
			finishedAtStr sql.NullString
			runErrorStr   sql.NullString
			// agent_calls 列 (LEFT JOIN により全 NULL の可能性あり)
			acID              sql.NullString
			acProviderID      sql.NullString
			acStepID          sql.NullString
			acStepName        sql.NullString
			acSuccess         sql.NullBool
			acDurationMS      sql.NullInt64
			acTotalCostUSD    sql.NullFloat64
			acInputTokens     sql.NullInt64
			acOutputTokens    sql.NullInt64
			acFailureCategory sql.NullString
			acError           sql.NullString
			acStartedAtStr    sql.NullString
			acFinishedAtStr   sql.NullString
		)
		if err := rows.Scan(
			&runID, &workflowID, &workflowName, &cwd, &startedAtStr, &finishedAtStr, &runErrorStr,
			&acID, &acProviderID, &acStepID, &acStepName,
			&acSuccess, &acDurationMS, &acTotalCostUSD, &acInputTokens, &acOutputTokens,
			&acFailureCategory, &acError, &acStartedAtStr, &acFinishedAtStr,
		); err != nil {
			return nil, fmt.Errorf("akashic: list runs scan: %w", err)
		}

		// 新しい workflow_run_id に遭遇したら新しい WorkflowRunListItem を push する。
		if runID != lastRunID {
			startedAt, err := parseStoredTime(startedAtStr)
			if err != nil {
				return nil, fmt.Errorf("akashic: parse started_at for run %s: %w", runID, err)
			}
			finishedAt, err := parseOptionalStoredTime(finishedAtStr)
			if err != nil {
				return nil, fmt.Errorf("akashic: parse finished_at for run %s: %w", runID, err)
			}
			items = append(items, WorkflowRunListItem{
				RunHeader: RunHeader{
					RunID:        runID,
					WorkflowID:   workflowID,
					WorkflowName: workflowName,
					CWD:          cwd,
					StartedAt:    startedAt,
					FinishedAt:   finishedAt,
					Success:      deriveRunSuccess(finishedAt, runErrorStr),
					Error:        nullStringValue(runErrorStr),
				},
				// AgentCalls は nil で初期化し、対応 agent_call があれば append する
				// (run に紐づく agent_call が 0 件の場合は nil のまま返す)。
				AgentCalls: nil,
			})
			lastRunID = runID
			lastIdx = len(items) - 1
		}

		// agent_call が NULL (LEFT JOIN no-match) ならスキップ。
		if !acID.Valid {
			continue
		}
		acStartedAt, err := parseStoredTime(acStartedAtStr.String)
		if err != nil {
			return nil, fmt.Errorf("akashic: parse agent_call started_at for %s: %w", acID.String, err)
		}
		acFinishedAt, err := parseOptionalStoredTime(acFinishedAtStr)
		if err != nil {
			return nil, fmt.Errorf("akashic: parse agent_call finished_at for %s: %w", acID.String, err)
		}
		items[lastIdx].AgentCalls = append(items[lastIdx].AgentCalls, AgentCallSummary{
			ID:              acID.String,
			ProviderID:      nullStringValue(acProviderID),
			StepID:          nullStringValue(acStepID),
			StepName:        nullStringValue(acStepName),
			Success:         nullBoolPtr(acSuccess),
			DurationMS:      nullInt64Ptr(acDurationMS),
			TotalCostUSD:    nullFloat64Ptr(acTotalCostUSD),
			InputTokens:     nullInt64Ptr(acInputTokens),
			OutputTokens:    nullInt64Ptr(acOutputTokens),
			FailureCategory: nullStringValue(acFailureCategory),
			Error:           nullStringValue(acError),
			StartedAt:       acStartedAt,
			FinishedAt:      acFinishedAt,
		})
	}
	return items, nil
}

// timeSentinelAndValue は TimeRange の Since/Until を querySelectRunsList の
// (sentinel, value) 形式に変換する。zero time → ("", "") で「フィルタなし」を表現する。
//
// queries.go の sentinel-binding パターン (空文字 sentinel が真なら絞り込みなし) に対し、
// sentinel 値は空文字 (フィルタなし) または RFC3339Nano 文字列 (フィルタあり) の 2 値で
// 運用する。
func timeSentinelAndValue(t time.Time) (sentinel, value string) {
	if t.IsZero() {
		return "", ""
	}
	formatted := t.UTC().Format(time.RFC3339Nano)
	return formatted, formatted
}

// successSentinelAndValue は ListOptions.Success (*bool) を querySelectRunsList の
// success sentinel/value (int) に変換する。
//
// queries.go の sentinel-binding パターン `(? = -1 OR ... = ?)` に対し:
//   - nil       → (-1, -1) フィルタなし (全件)
//   - &true     → (1, 1)   成功のみ (error IS NULL かつ finished_at IS NOT NULL)
//   - &false    → (0, 0)   失敗のみ
func successSentinelAndValue(s *bool) (sentinel, value int) {
	if s == nil {
		return -1, -1
	}
	if *s {
		return 1, 1
	}
	return 0, 0
}

// parseStoredTime は SQLite TEXT として保存された RFC3339Nano 文字列を time.Time に
// パースする (formatTime と対称な逆変換)。タイムゾーンは UTC を維持する。
func parseStoredTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// parseOptionalStoredTime は sql.NullString を *time.Time に変換する。
// Valid=false → nil / Valid=true → ポインタ経由で値を返す (NULL とゼロ時刻を区別)。
func parseOptionalStoredTime(s sql.NullString) (*time.Time, error) {
	if !s.Valid {
		return nil, nil
	}
	t, err := parseStoredTime(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// deriveRunSuccess は workflow_runs の finished_at と error から RunHeader.Success を
// 派生する (Req 3.1 の派生定義に準拠、agent_calls は参照しない)。
//
//   - finished_at IS NULL                 → nil  (未完了)
//   - finished_at NOT NULL かつ error NULL  → &true  (成功)
//   - finished_at NOT NULL かつ error 値あり → &false (失敗)
func deriveRunSuccess(finishedAt *time.Time, runError sql.NullString) *bool {
	if finishedAt == nil {
		return nil
	}
	success := !runError.Valid || runError.String == ""
	return &success
}
