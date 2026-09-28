// Package akashic の reader_sqlite_detail.go は、Reader interface (reader.go) のうち
// Detail メソッドの実装を提供する。
//
// JOIN ではなく 4 本の独立 SELECT に分割するため、Detail 本体と
// detailRunHeader / detailAgentCalls / detailContextUsage / detailRawEvents の
// 各サブクエリヘルパを 1 ファイルにまとめている。
//
// nullStringBytes は Detail 系の TEXT 列 (resultPayload / *_json / payload) を []byte
// にマッピングするヘルパで、Detail 系でしか使われないため本ファイルに同居させる。
package akashic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Detail は workflow_run と紐づく agent_calls / agent_context_usage / raw_events を
// ひとまとめで返す (Req 4.1〜4.9, 7.1, 7.2)。
//
// 動作概要:
//  1. opts.Validate() で raw_events の Limit/Offset を sentinel error として弾く
//  2. RawEvents.Limit==0 → DefaultRawEventsLimit に補完 (Req 7.1: 既定 limit を超えない保証)
//  3. 4 本の独立 SELECT を順次発行する (MaxOpenConns(1) のため直列化を尊重し、
//     各 rows は内側スコープで早期 Close する):
//     - querySelectRunByID: workflow_run スカラ列 (該当 0 行 → ErrWorkflowRunNotFound、Req 4.7)
//     - querySelectAgentCallsByRunID: 完全な agent_calls 行を started_at ASC で取得 (Req 4.1, 4.6)
//     - querySelectContextUsageByRunID: 同 run の context_usage を sequence ASC で取得 (Req 4.2)
//     - querySelectRawEventsByRunID: type/subtype/limit/offset 適用の raw_events (Req 4.3〜4.5, 4.6, 4.8)
//  4. RunHeader を組み立て (Success は finished_at と error から派生、Req 3.1 と整合)
//  5. NextRawEventsOffset: len(RawEvents) == RawEventsLimit のとき &(Offset + Limit)、
//     それ以外 nil (Req 4.4)
//
// JOIN ではなく 4 本の独立 SELECT に分割する根拠 (design.md "Flow B"):
//   - raw_events の行爆発を避ける (LEFT JOIN すると agent_calls × raw_events が展開される)
//   - 同一 workflow_run_id 単位なので各 SELECT の行数は限定的
//     (agent_calls < 100、agent_context_usage < 100、raw_events は limit でクランプ)
func (r *SQLiteRecorder) Detail(ctx context.Context, opts DetailOptions) (RunDetail, error) {
	if err := opts.Validate(); err != nil {
		return RunDetail{}, err
	}
	rawLimit := opts.RawEvents.Limit
	if rawLimit == 0 {
		rawLimit = DefaultRawEventsLimit
	}

	// Step 1: workflow_run header (該当なし → ErrWorkflowRunNotFound)
	runHeader, err := r.detailRunHeader(ctx, opts.WorkflowRunID)
	if err != nil {
		return RunDetail{}, err
	}

	// Step 2: agent_calls (started_at ASC, id ASC)
	agentCalls, err := r.detailAgentCalls(ctx, opts.WorkflowRunID)
	if err != nil {
		return RunDetail{}, err
	}

	// Step 3: agent_context_usage (sequence ASC)
	contextUsage, err := r.detailContextUsage(ctx, opts.WorkflowRunID)
	if err != nil {
		return RunDetail{}, err
	}

	// Step 4: raw_events (sequence ASC, id ASC、type/subtype/limit/offset 適用)
	rawEvents, err := r.detailRawEvents(ctx, opts.WorkflowRunID, opts.RawEvents, rawLimit)
	if err != nil {
		return RunDetail{}, err
	}

	// NextRawEventsOffset: len(RawEvents) == rawLimit のとき次オフセットを返す (Req 4.4)
	var nextOffset *int
	if len(rawEvents) == rawLimit {
		next := opts.RawEvents.Offset + rawLimit
		nextOffset = &next
	}

	return RunDetail{
		Run:                 runHeader,
		AgentCalls:          agentCalls,
		ContextUsage:        contextUsage,
		RawEvents:           rawEvents,
		RawEventsLimit:      rawLimit,
		RawEventsOffset:     opts.RawEvents.Offset,
		NextRawEventsOffset: nextOffset,
	}, nil
}

// detailRunHeader は querySelectRunByID を発行して RunHeader を組み立てる (Req 4.1, 4.7)。
//
// QueryRowContext.Scan は該当 0 行のとき sql.ErrNoRows を返す。Reader 契約では
// ErrWorkflowRunNotFound を sentinel として返すため、errors.Is で判定して変換する。
func (r *SQLiteRecorder) detailRunHeader(ctx context.Context, runID string) (RunHeader, error) {
	var (
		id, workflowID, workflowName, cwd, startedAtStr string
		finishedAtStr                                   sql.NullString
		runErrorStr                                     sql.NullString
	)
	err := r.db.QueryRowContext(ctx, querySelectRunByID, runID).Scan(
		&id, &workflowID, &workflowName, &cwd, &startedAtStr, &finishedAtStr, &runErrorStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// sentinel をそのまま返す (errors.Is で判別可能、Req 4.7)。
			return RunHeader{}, ErrWorkflowRunNotFound
		}
		return RunHeader{}, fmt.Errorf("akashic: detail run: %w", err)
	}
	startedAt, err := parseStoredTime(startedAtStr)
	if err != nil {
		return RunHeader{}, fmt.Errorf("akashic: parse started_at for run %s: %w", id, err)
	}
	finishedAt, err := parseOptionalStoredTime(finishedAtStr)
	if err != nil {
		return RunHeader{}, fmt.Errorf("akashic: parse finished_at for run %s: %w", id, err)
	}
	return RunHeader{
		RunID:        id,
		WorkflowID:   workflowID,
		WorkflowName: workflowName,
		CWD:          cwd,
		StartedAt:    startedAt,
		FinishedAt:   finishedAt,
		Success:      deriveRunSuccess(finishedAt, runErrorStr),
		Error:        nullStringValue(runErrorStr),
	}, nil
}

// detailAgentCalls は querySelectAgentCallsByRunID を発行して AgentCallRow 配列を組み立てる
// (Req 4.1, 4.6)。
//
// agent_calls はスキーマ上ほぼ全カラムが NULLABLE のため、defensive に sql.Null* で受けて
// nullStringValue / null*Ptr ヘルパで Reader 結果型に変換する。started_at は実運用では
// RecordAgentCallStart が必ず値を入れるが、スキーマ上 NULLABLE のため Valid 判定して
// time.Time のゼロ値で受ける defensive scan を行う。
func (r *SQLiteRecorder) detailAgentCalls(ctx context.Context, runID string) ([]AgentCallRow, error) {
	rows, err := r.db.QueryContext(ctx, querySelectAgentCallsByRunID, runID)
	if err != nil {
		return nil, fmt.Errorf("akashic: detail agent_calls: %w", err)
	}
	defer rows.Close()

	var calls []AgentCallRow
	for rows.Next() {
		var (
			id                   string
			workflowRunID        sql.NullString
			stepID               sql.NullString
			stepName             sql.NullString
			providerID           sql.NullString
			prompt               sql.NullString
			cwd                  sql.NullString
			sessionID            sql.NullString
			model                sql.NullString
			modelReasoningEffort sql.NullString
			success              sql.NullBool
			durationMS           sql.NullInt64
			totalCostUSD         sql.NullFloat64
			inputTokens          sql.NullInt64
			outputTokens         sql.NullInt64
			resultSummary        sql.NullString
			errStr               sql.NullString
			failureCategory      sql.NullString
			startedAtStr         sql.NullString
			finishedAtStr        sql.NullString
			resultPayload        sql.NullString
		)
		if err := rows.Scan(
			&id, &workflowRunID, &stepID, &stepName, &providerID, &prompt, &cwd, &sessionID,
			&model, &modelReasoningEffort, &success, &durationMS, &totalCostUSD, &inputTokens, &outputTokens,
			&resultSummary, &errStr, &failureCategory, &startedAtStr, &finishedAtStr, &resultPayload,
		); err != nil {
			return nil, fmt.Errorf("akashic: detail agent_calls scan: %w", err)
		}
		var startedAt time.Time
		if startedAtStr.Valid {
			startedAt, err = parseStoredTime(startedAtStr.String)
			if err != nil {
				return nil, fmt.Errorf("akashic: parse agent_call started_at for %s: %w", id, err)
			}
		}
		finishedAt, err := parseOptionalStoredTime(finishedAtStr)
		if err != nil {
			return nil, fmt.Errorf("akashic: parse agent_call finished_at for %s: %w", id, err)
		}
		calls = append(calls, AgentCallRow{
			ID:                   id,
			WorkflowRunID:        nullStringValue(workflowRunID),
			StepID:               nullStringValue(stepID),
			StepName:             nullStringValue(stepName),
			ProviderID:           nullStringValue(providerID),
			Prompt:               nullStringValue(prompt),
			CWD:                  nullStringValue(cwd),
			SessionID:            nullStringValue(sessionID),
			Model:                nullStringValue(model),
			ModelReasoningEffort: nullStringValue(modelReasoningEffort),
			Success:              nullBoolPtr(success),
			DurationMS:           nullInt64Ptr(durationMS),
			TotalCostUSD:         nullFloat64Ptr(totalCostUSD),
			InputTokens:          nullInt64Ptr(inputTokens),
			OutputTokens:         nullInt64Ptr(outputTokens),
			ResultSummary:        nullStringValue(resultSummary),
			Error:                nullStringValue(errStr),
			FailureCategory:      nullStringValue(failureCategory),
			StartedAt:            startedAt,
			FinishedAt:           finishedAt,
			ResultPayload:        nullStringBytes(resultPayload),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("akashic: detail agent_calls iter: %w", err)
	}
	return calls, nil
}

// detailContextUsage は querySelectContextUsageByRunID を発行して AgentContextUsageRow
// 配列を組み立てる (Req 4.2)。
//
// 必須カラム (agent_call_id / raw_event_id / sequence / observed_at / *_tokens / percentage /
// is_auto_compact_enabled) は schema NOT NULL のため通常の型で scan、それ以外は sql.Null*
// で受ける。*_json は TEXT NULLABLE のため nullStringBytes で []byte/nil に変換 (Req 4.9:
// パースは下流の責務)。
func (r *SQLiteRecorder) detailContextUsage(ctx context.Context, runID string) ([]AgentContextUsageRow, error) {
	rows, err := r.db.QueryContext(ctx, querySelectContextUsageByRunID, runID)
	if err != nil {
		return nil, fmt.Errorf("akashic: detail context_usage: %w", err)
	}
	defer rows.Close()

	var usage []AgentContextUsageRow
	for rows.Next() {
		var (
			agentCallID          string
			workflowRunID        sql.NullString
			rawEventID           int64
			sequence             int64
			observedAtStr        string
			model                sql.NullString
			totalTokens          int64
			maxTokens            int64
			rawMaxTokens         int64
			percentage           int64
			isAutoCompactEnabled int
			autoCompactThreshold sql.NullInt64
			categoriesJSON       sql.NullString
			memoryFilesJSON      sql.NullString
			mcpToolsJSON         sql.NullString
			agentsJSON           sql.NullString
			messageBreakdownJSON sql.NullString
			apiUsageJSON         sql.NullString
		)
		if err := rows.Scan(
			&agentCallID, &workflowRunID, &rawEventID, &sequence, &observedAtStr,
			&model, &totalTokens, &maxTokens, &rawMaxTokens, &percentage,
			&isAutoCompactEnabled, &autoCompactThreshold,
			&categoriesJSON, &memoryFilesJSON, &mcpToolsJSON, &agentsJSON, &messageBreakdownJSON, &apiUsageJSON,
		); err != nil {
			return nil, fmt.Errorf("akashic: detail context_usage scan: %w", err)
		}
		observedAt, err := parseStoredTime(observedAtStr)
		if err != nil {
			return nil, fmt.Errorf("akashic: parse context_usage observed_at for agent_call %s: %w", agentCallID, err)
		}
		usage = append(usage, AgentContextUsageRow{
			AgentCallID:          agentCallID,
			WorkflowRunID:        nullStringValue(workflowRunID),
			RawEventID:           rawEventID,
			Sequence:             sequence,
			ObservedAt:           observedAt,
			Model:                nullStringValue(model),
			TotalTokens:          totalTokens,
			MaxTokens:            maxTokens,
			RawMaxTokens:         rawMaxTokens,
			Percentage:           percentage,
			IsAutoCompactEnabled: isAutoCompactEnabled != 0,
			AutoCompactThreshold: nullInt64Ptr(autoCompactThreshold),
			CategoriesJSON:       nullStringBytes(categoriesJSON),
			MemoryFilesJSON:      nullStringBytes(memoryFilesJSON),
			MCPToolsJSON:         nullStringBytes(mcpToolsJSON),
			AgentsJSON:           nullStringBytes(agentsJSON),
			MessageBreakdownJSON: nullStringBytes(messageBreakdownJSON),
			APIUsageJSON:         nullStringBytes(apiUsageJSON),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("akashic: detail context_usage iter: %w", err)
	}
	return usage, nil
}

// detailRawEvents は querySelectRawEventsByRunID を発行して RawEventRow 配列を組み立てる
// (Req 4.3〜4.5, 4.6, 4.8)。
//
// queries.go の sentinel-binding パターン: type/subtype が空文字なら絞り込みなし。
// limit は呼び出し元 (Detail) が opts.RawEvents.Limit==0 を DefaultRawEventsLimit に補完
// 済みの値を渡す前提。
func (r *SQLiteRecorder) detailRawEvents(ctx context.Context, runID string, q RawEventsQuery, limit int) ([]RawEventRow, error) {
	typeSentinel := q.Type
	subtypeSentinel := q.Subtype
	rows, err := r.db.QueryContext(ctx, querySelectRawEventsByRunID,
		runID,
		typeSentinel, q.Type,
		subtypeSentinel, q.Subtype,
		limit, q.Offset,
	)
	if err != nil {
		return nil, fmt.Errorf("akashic: detail raw_events: %w", err)
	}
	defer rows.Close()

	var events []RawEventRow
	for rows.Next() {
		var (
			id               int64
			agentCallID      sql.NullString
			workflowRunID    sql.NullString
			sequence         int64
			source           string
			typeStr          string
			subtype          sql.NullString
			observedAtStr    string
			payload          sql.NullString
			payloadTruncated int
		)
		if err := rows.Scan(
			&id, &agentCallID, &workflowRunID, &sequence, &source, &typeStr, &subtype,
			&observedAtStr, &payload, &payloadTruncated,
		); err != nil {
			return nil, fmt.Errorf("akashic: detail raw_events scan: %w", err)
		}
		observedAt, err := parseStoredTime(observedAtStr)
		if err != nil {
			return nil, fmt.Errorf("akashic: parse raw_event observed_at for id=%d: %w", id, err)
		}
		events = append(events, RawEventRow{
			ID:               id,
			AgentCallID:      nullStringValue(agentCallID),
			WorkflowRunID:    nullStringValue(workflowRunID),
			Sequence:         sequence,
			Source:           source,
			Type:             typeStr,
			Subtype:          nullStringValue(subtype),
			ObservedAt:       observedAt,
			Payload:          nullStringBytes(payload),
			PayloadTruncated: payloadTruncated != 0,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("akashic: detail raw_events iter: %w", err)
	}
	return events, nil
}

// nullStringBytes は sql.NullString を []byte に変換する。
// Valid=false → nil / Valid=true → 値の []byte コピー。
//
// 用途: TEXT 列に格納された JSON / payload bytes を Reader 結果型 ([]byte) として返す。
// 既存の null-conversion 系ヘルパ (nullStringValue / nullBoolPtr 等) と併用する位置付けで、
// Detail (タスク 2.4) の AgentCallRow.ResultPayload / AgentContextUsageRow.*JSON /
// RawEventRow.Payload で使う。
func nullStringBytes(s sql.NullString) []byte {
	if !s.Valid {
		return nil
	}
	return []byte(s.String)
}
