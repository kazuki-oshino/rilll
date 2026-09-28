// Package akashic の reader_sqlite_resolution.go は、Reader interface (reader.go) のうち
// memory-distiller の resolution-chain 拡張で追加された 2 メソッド
// (InRunResolutionsForCluster / NextSuccessAcrossRuns) の実装を提供する。
//
// 設計契約:
//   - Pattern 1 (InRunResolutionsForCluster): 失敗 cluster の代表 failing agent_call ID 群を
//     batched IN-list で渡し、各 ID について同 workflow_run_id 内で時系列的に後続する成功
//     agent_call を 1 件ずつ map で返す。SQL は agent_calls テーブル単独で完結し、JOIN を
//     持つが副問い合わせで「最も早い後続 success」を 1 件に絞り込む。
//   - Pattern 2 (NextSuccessAcrossRuns): 失敗 cluster の (workflow_id, step_id) に対し、
//     AfterTime より後で最初に成功した agent_call を 1 件返す。同 run 内の制約は持たない
//     (Pattern 1 が見つからなかった cluster への fallback として呼ばれる)。
//
// すべての値バインドは "?" プレースホルダ経由で行い、文字列連結による値埋め込みを排除する
// (queries.go の設計方針と整合)。IN-list の placeholder 数だけが動的に変わる。
package akashic

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// InRunResolutionsForCluster は Pattern 1 (同 run 内 resolution lookup) の実装
// (reader.go の Reader interface 契約)。
//
// 動作概要:
//  1. opts.Validate() で軽微検証 (現状 no-op)
//  2. opts.FailingAgentCallIDs が空なら空 map を即返し、SQL を発行しない
//  3. IN-list placeholder を failing IDs 件数に展開し querySelectInRunResolutions を発行
//  4. 各行を InRunResolutionRow に scan し、failing.id をキーに map 化して返す
//
// 1 つの failing.id につき必ず 1 件以下の row が返る (SQL の `success.id = (... LIMIT 1)`
// 副問い合わせで担保)。後続成功が無い failing.id は map に含まれない。
//
// 値バインド順序: ["IN-list placeholders ...", project_sentinel, project_value]
// (querySelectInRunResolutions の WHERE 句順序と整合)。
func (r *SQLiteRecorder) InRunResolutionsForCluster(ctx context.Context, opts InRunResolutionOptions) (InRunResolutionsResult, error) {
	if err := opts.Validate(); err != nil {
		return InRunResolutionsResult{}, err
	}
	if len(opts.FailingAgentCallIDs) == 0 {
		return InRunResolutionsResult{ResolutionsByFailingID: map[string]InRunResolutionRow{}}, nil
	}

	// 1. IN-list ユニーク化 (重複 placeholder を避ける、副問い合わせの計算回数も減らす)。
	idSet := make(map[string]struct{}, len(opts.FailingAgentCallIDs))
	ids := make([]string, 0, len(opts.FailingAgentCallIDs))
	for _, id := range opts.FailingAgentCallIDs {
		if id == "" {
			continue
		}
		if _, ok := idSet[id]; ok {
			continue
		}
		idSet[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return InRunResolutionsResult{ResolutionsByFailingID: map[string]InRunResolutionRow{}}, nil
	}

	// 2. placeholder 文字列 (?,?,...,?) を ids 件数に応じて生成。
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1] // 末尾カンマを除去
	query := fmt.Sprintf(querySelectInRunResolutions, placeholders)

	// 3. bind 値を構築: IN-list IDs ... + project_id sentinel + project_id value。
	args := make([]any, 0, len(ids)+2)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, opts.ProjectID, opts.ProjectID)

	// 4. SELECT 発行。
	sqlRows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return InRunResolutionsResult{}, fmt.Errorf("akashic: in-run resolutions query: %w", err)
	}
	defer sqlRows.Close()

	out := make(map[string]InRunResolutionRow, len(ids))
	for sqlRows.Next() {
		var (
			failingID     string
			successID     string
			workflowRunID sql.NullString
			stepID        string
			startedAtStr  string
			resultSummary string
			prompt        string
		)
		if err := sqlRows.Scan(&failingID, &successID, &workflowRunID, &stepID, &startedAtStr, &resultSummary, &prompt); err != nil {
			return InRunResolutionsResult{}, fmt.Errorf("akashic: in-run resolutions scan: %w", err)
		}
		startedAt, parseErr := parseStoredTime(startedAtStr)
		if parseErr != nil {
			return InRunResolutionsResult{}, fmt.Errorf("akashic: parse in-run resolution started_at for failing %s: %w", failingID, parseErr)
		}
		out[failingID] = InRunResolutionRow{
			FailingAgentCallID: failingID,
			SuccessAgentCallID: successID,
			WorkflowRunID:      nullStringValue(workflowRunID),
			StepID:             stepID,
			StartedAt:          startedAt,
			ResultSummary:      resultSummary,
			Prompt:             prompt,
		}
	}
	if err := sqlRows.Err(); err != nil {
		return InRunResolutionsResult{}, fmt.Errorf("akashic: in-run resolutions iter: %w", err)
	}
	return InRunResolutionsResult{ResolutionsByFailingID: out}, nil
}

// NextSuccessAcrossRuns は Pattern 2 (別 run での next success lookup) の実装
// (reader.go の Reader interface 契約)。
//
// 動作概要:
//  1. opts.Validate() (現状 no-op)
//  2. AfterTime → time sentinel/value 変換 (timeSentinelAndValue を流用)
//  3. querySelectNextSuccessAcrossRuns を発行 (LIMIT 1)
//  4. 0 件 → Found=false, ゼロ値 row。1 件 → Found=true で詰めて返す。
//
// 値バインド順序: project_sentinel, project_value, workflow_sentinel, workflow_value,
// step_sentinel, step_value, after_sentinel, after_value (querySelectNextSuccessAcrossRuns
// の WHERE 句順序と整合)。
func (r *SQLiteRecorder) NextSuccessAcrossRuns(ctx context.Context, opts NextSuccessOptions) (NextSuccessResult, error) {
	if err := opts.Validate(); err != nil {
		return NextSuccessResult{}, err
	}

	afterSentinel, afterValue := timeSentinelAndValue(opts.AfterTime)

	row := r.db.QueryRowContext(ctx, querySelectNextSuccessAcrossRuns,
		opts.ProjectID, opts.ProjectID,
		opts.WorkflowID, opts.WorkflowID,
		opts.StepID, opts.StepID,
		afterSentinel, afterValue,
	)

	var (
		id            string
		workflowRunID sql.NullString
		startedAtStr  string
	)
	if err := row.Scan(&id, &workflowRunID, &startedAtStr); err != nil {
		if err == sql.ErrNoRows {
			return NextSuccessResult{Found: false}, nil
		}
		return NextSuccessResult{}, fmt.Errorf("akashic: next-success scan: %w", err)
	}
	startedAt, parseErr := parseStoredTime(startedAtStr)
	if parseErr != nil {
		return NextSuccessResult{}, fmt.Errorf("akashic: parse next-success started_at for agent_call %s: %w", id, parseErr)
	}
	return NextSuccessResult{
		Found: true,
		Row: NextSuccessRow{
			AgentCallID:   id,
			WorkflowRunID: nullStringValue(workflowRunID),
			StartedAt:     startedAt,
		},
	}, nil
}
