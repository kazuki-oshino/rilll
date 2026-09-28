package akashic

import (
	"context"
	"fmt"
)

// DescendantWorkflowRunIDs は parentRunID 配下の child / grandchild workflow_run.id を返す。
//
// 返却順序は child workflow_run.started_at ASC, id ASC。親自身は含めない。
// lineage が無い parent は空 slice + nil error を返す。
func (r *SQLiteRecorder) DescendantWorkflowRunIDs(ctx context.Context, parentRunID string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, querySelectDescendantWorkflowRunIDs, parentRunID)
	if err != nil {
		return nil, fmt.Errorf("akashic: select descendant workflow runs: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			return nil, fmt.Errorf("akashic: scan descendant workflow run: %w", err)
		}
		out = append(out, runID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("akashic: descendant workflow runs iter: %w", err)
	}
	return out, nil
}
