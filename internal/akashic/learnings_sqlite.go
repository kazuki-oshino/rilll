// Package akashic の learnings_sqlite.go は agent-learnings spec
// (task 3.2 / Req 3.1, 3.6, 5.1, 5.2, 5.3, 9.6) の SQLiteRecorder 拡張を提供する。
//
// 提供するメソッド:
//   - RecordLearning       : Recorder 拡張 (learnings table への 1 件 INSERT + validation)
//   - RecordLearningOnce   : early aggregation 専用の deterministic ID insert-once
//   - LearningsByWorkflowRun : Reader 拡張 (workflow_run_id の learnings 行を読み戻す)
//   - AgentCallsByWorkflowRun : Reader 拡張 (PreFetch phase の session_id 集約用)
//
// 設計契約:
//   - learnings は append-only (UPDATE / DELETE 不可)。INSERT が PK 衝突した場合は
//     呼び出し側 (Reflector phase) のバグとして上位に伝播する (ON CONFLICT 句なし)。
//   - paths_glob は Go 側で `[]string` として持ち、SQL 永続化のときだけ JSON encode する。
//     読み戻し時は逆に JSON decode して `[]string` に戻す (round-trip)。
//   - Recorder 物理層の validation は Category enum と paths_glob 個数の 2 つのみ。
//     paths_glob の内容 ("**" / "**/*" / "*" 単独 reject 等) は上位 Apply phase の責務。
//   - AgentCallRow.ProjectID は AgentCallsByWorkflowRun 経由でのみ populate される。
//     Detail 経由で取得した AgentCallRow は ProjectID が空文字のままになる
//     (既存 Detail SQL を改変せず、PreFetch phase の post-filter 用 column のみを追加)。
package akashic

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// RecordLearning は learnings table に 1 行 INSERT する (agent-learnings spec Req 5.1)。
//
// validation (失敗すると INSERT を発行せず sentinel error で早期 return):
//   - Category が ValidLearningCategory(closed enum 4 値) の外 → ErrInvalidLearningCategory
//   - len(PathsGlob) > MaxPathsGlobEntries (5)              → ErrTooManyPathsGlob
//
// PathsGlob == nil は空 slice として扱い `[]` で JSON encode する (空配列も有効入力)。
// Apply phase が空 paths_glob を上位の reject 対象とするのは別レイヤーの責務 (Req 7.4)。
//
// RecordedAt がゼロ値の場合は formatTime が UTC now を補完する (他 Recorder メソッドと統一)。
func (r *SQLiteRecorder) RecordLearning(ctx context.Context, row LearningRow) error {
	pathsGlobJSON, err := encodeLearningPathsGlob(row)
	if err != nil {
		return err
	}
	if _, err := r.db.ExecContext(ctx, queryInsertLearning,
		row.ID, row.AgentCallID, row.WorkflowRunID, row.WorkflowID, row.StepID,
		row.Category, row.Context, row.Insight, pathsGlobJSON, row.Source, formatTime(row.RecordedAt),
	); err != nil {
		return fmt.Errorf("akashic: insert learning: %w", err)
	}
	return nil
}

// RecordLearningOnce は deterministic learning ID の insert-once 経路。
//
// existing RecordLearning は append-only contract を維持するため PK 衝突を error として
// 返す。一方 early aggregation は retry / tail 再実行で同じ deterministic ID を再処理
// し得るため、衝突を fatal にせず inserted=false として返す。
func (r *SQLiteRecorder) RecordLearningOnce(ctx context.Context, row LearningRow) (bool, error) {
	pathsGlobJSON, err := encodeLearningPathsGlob(row)
	if err != nil {
		return false, err
	}
	result, err := r.db.ExecContext(ctx, queryInsertLearningOnce,
		row.ID, row.AgentCallID, row.WorkflowRunID, row.WorkflowID, row.StepID,
		row.Category, row.Context, row.Insight, pathsGlobJSON, row.Source, formatTime(row.RecordedAt),
	)
	if err != nil {
		return false, fmt.Errorf("akashic: insert learning once: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("akashic: learning once rows affected: %w", err)
	}
	return rowsAffected > 0, nil
}

func encodeLearningPathsGlob(row LearningRow) (string, error) {
	if !ValidLearningCategory(row.Category) {
		return "", ErrInvalidLearningCategory
	}
	if len(row.PathsGlob) > MaxPathsGlobEntries {
		return "", ErrTooManyPathsGlob
	}
	pathsGlob := row.PathsGlob
	if pathsGlob == nil {
		// json.Marshal(nil) は "null" を返すため、空 slice として encode する。
		pathsGlob = []string{}
	}
	pathsGlobJSON, err := json.Marshal(pathsGlob)
	if err != nil {
		return "", fmt.Errorf("akashic: marshal paths_glob: %w", err)
	}
	return string(pathsGlobJSON), nil
}

// LearningsByWorkflowRun は指定 workflow_run_id の learnings 行を recorded_at ASC,
// id ASC の安定順序で返す (agent-learnings spec Req 5.1)。
//
// 該当 0 件は空 slice + nil error を返す (ErrWorkflowRunNotFound のような sentinel は
// 持たない / append-only domain なので「行が無い」は正常状態)。
//
// paths_glob_json は JSON decode して LearningRow.PathsGlob に戻す。recorded_at は
// parseStoredTime で UTC time.Time に戻す (RecordLearning 側の formatTime と表裏一致)。
func (r *SQLiteRecorder) LearningsByWorkflowRun(ctx context.Context, workflowRunID string) ([]LearningRow, error) {
	rows, err := r.db.QueryContext(ctx, querySelectLearningsByWorkflowRun, workflowRunID)
	if err != nil {
		return nil, fmt.Errorf("akashic: select learnings: %w", err)
	}
	defer rows.Close()

	var out []LearningRow
	for rows.Next() {
		var (
			row           LearningRow
			pathsGlobJSON string
			recordedAtStr string
		)
		if err := rows.Scan(
			&row.ID, &row.AgentCallID, &row.WorkflowRunID, &row.WorkflowID, &row.StepID,
			&row.Category, &row.Context, &row.Insight, &pathsGlobJSON, &row.Source, &recordedAtStr,
		); err != nil {
			return nil, fmt.Errorf("akashic: scan learning: %w", err)
		}
		// 空配列 "[]" / JSON encoded slice の両方を decode する。
		if err := json.Unmarshal([]byte(pathsGlobJSON), &row.PathsGlob); err != nil {
			return nil, fmt.Errorf("akashic: unmarshal paths_glob for learning %s: %w", row.ID, err)
		}
		recordedAt, err := parseStoredTime(recordedAtStr)
		if err != nil {
			return nil, fmt.Errorf("akashic: parse recorded_at for learning %s: %w", row.ID, err)
		}
		row.RecordedAt = recordedAt
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("akashic: learnings iter: %w", err)
	}
	return out, nil
}

// AgentCallsByWorkflowRun は指定 workflow_run_id の agent_calls を started_at ASC,
// id ASC の安定順序で返す (agent-learnings spec Req 3.1, 3.6 の素材)。
//
// PreFetch phase が parent_run_id から session_id 集約を行うときに使う。Detail とは
// 別 SQL を発行し、AgentCallRow.ProjectID を populate する点が異なる (ProjectScopedReader
// が wrapper 側で project scope を post-filter するため、project_id を Reader 戻り値に
// 含める必要がある)。
//
// 該当 0 件は空 slice + nil error を返す。
//
// SELECT 列は PreFetch / ProjectScopedReader が必要とする最小 subset (id /
// workflow_run_id / step_id / step_name / provider_id / cwd / project_id / session_id /
// success / started_at / finished_at)。result_payload_json / failure_category 等の
// 重い列は Detail 経由で取得する。
func (r *SQLiteRecorder) AgentCallsByWorkflowRun(ctx context.Context, workflowRunID string) ([]AgentCallRow, error) {
	rows, err := r.db.QueryContext(ctx, querySelectAgentCallsByWorkflowRunForLearnings, workflowRunID)
	if err != nil {
		return nil, fmt.Errorf("akashic: select agent_calls: %w", err)
	}
	defer rows.Close()

	var out []AgentCallRow
	for rows.Next() {
		var (
			id            string
			workflowRunID sql.NullString
			stepID        sql.NullString
			stepName      sql.NullString
			providerID    sql.NullString
			cwd           sql.NullString
			projectID     sql.NullString
			sessionID     sql.NullString
			success       sql.NullBool
			startedAtStr  sql.NullString
			finishedAtStr sql.NullString
		)
		if err := rows.Scan(
			&id, &workflowRunID, &stepID, &stepName, &providerID, &cwd, &projectID, &sessionID,
			&success, &startedAtStr, &finishedAtStr,
		); err != nil {
			return nil, fmt.Errorf("akashic: scan agent_call: %w", err)
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
		out = append(out, AgentCallRow{
			ID:            id,
			WorkflowRunID: nullStringValue(workflowRunID),
			StepID:        nullStringValue(stepID),
			StepName:      nullStringValue(stepName),
			ProviderID:    nullStringValue(providerID),
			CWD:           nullStringValue(cwd),
			ProjectID:     nullStringValue(projectID),
			SessionID:     nullStringValue(sessionID),
			Success:       nullBoolPtr(success),
			StartedAt:     startedAt,
			FinishedAt:    finishedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("akashic: agent_calls iter: %w", err)
	}
	return out, nil
}
