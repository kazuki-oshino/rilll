package akashic

import (
	"strings"
	"testing"
)

// TestQueryConstantsDefined は queries.go の SQL 定数群がすべて非空で定義されている
// ことを保証する (タスク 1.2 完了条件: 各 SQL 定数が package private な named constant
// として export せず定義される)。各定数を参照することでビルド時に未定義検出が効く。
func TestQueryConstantsDefined(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{"querySelectRunsList", querySelectRunsList},
		{"querySelectRunByID", querySelectRunByID},
		{"querySelectAgentCallsByRunID", querySelectAgentCallsByRunID},
		{"querySelectContextUsageByRunID", querySelectContextUsageByRunID},
		{"querySelectRawEventsByRunID", querySelectRawEventsByRunID},
		{"querySelectWorkflowAggregateRuns", querySelectWorkflowAggregateRuns},
		{"querySelectWorkflowAggregateCalls", querySelectWorkflowAggregateCalls},
		{"querySelectProviderAggregateBuckets", querySelectProviderAggregateBuckets},
		{"querySelectFailureRows", querySelectFailureRows},
		{"querySelectFailureRawEventSubtypes", querySelectFailureRawEventSubtypes},
		{"queryUpsertRunArtifact", queryUpsertRunArtifact},
		{"querySelectArtifactsByRunID", querySelectArtifactsByRunID},
		{"querySelectArtifactByPath", querySelectArtifactByPath},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if strings.TrimSpace(c.sql) == "" {
				t.Errorf("%s: 定数が空文字", c.name)
			}
		})
	}
}

// TestQueryConstantsContainExpectedClauses は各 SQL 定数に必要な FROM/JOIN/WHERE/
// ORDER BY/LIMIT 等の構造句が含まれることを検証する (Req 2.6 / 3.1 / 4.6 / 5.4 / 5.6 /
// 7.1 / 7.3 への構造的トレース)。
func TestQueryConstantsContainExpectedClauses(t *testing.T) {
	cases := []struct {
		name        string
		sql         string
		mustContain []string
	}{
		{
			name: "querySelectRunsList",
			sql:  querySelectRunsList,
			mustContain: []string{
				"FROM workflow_runs",
				"LEFT JOIN agent_calls",
				"started_at DESC",
				"id DESC",
				"LIMIT ?",
				"OFFSET ?",
				// provider 絞り込みは agent_calls subquery 経由 (Req 2.4)
				"agent_calls WHERE provider_id",
				// success 派生 (Req 3.1: error IS NULL かつ finished_at IS NOT NULL)
				"error IS NULL AND finished_at IS NOT NULL",
			},
		},
		{
			name: "querySelectRunByID",
			sql:  querySelectRunByID,
			mustContain: []string{
				"FROM workflow_runs",
				"WHERE id = ?",
			},
		},
		{
			name: "querySelectAgentCallsByRunID",
			sql:  querySelectAgentCallsByRunID,
			mustContain: []string{
				"FROM agent_calls",
				"workflow_run_id = ?",
				"started_at ASC",
				"id ASC",
			},
		},
		{
			name: "querySelectContextUsageByRunID",
			sql:  querySelectContextUsageByRunID,
			mustContain: []string{
				"FROM agent_context_usage",
				"workflow_run_id = ?",
				"sequence ASC",
			},
		},
		{
			name: "querySelectRawEventsByRunID",
			sql:  querySelectRawEventsByRunID,
			mustContain: []string{
				"FROM raw_events",
				"workflow_run_id = ?",
				"sequence ASC",
				"id ASC",
				"LIMIT ?",
				"OFFSET ?",
				"type = ?",
				"subtype = ?",
			},
		},
		{
			name: "querySelectWorkflowAggregateRuns",
			sql:  querySelectWorkflowAggregateRuns,
			mustContain: []string{
				"FROM workflow_runs",
				"COUNT(*)",
				"GROUP BY workflow_id",
				// success_count の派生定義 (Req 3.1)
				"error IS NULL AND finished_at IS NOT NULL",
			},
		},
		{
			name: "querySelectWorkflowAggregateCalls",
			sql:  querySelectWorkflowAggregateCalls,
			mustContain: []string{
				"FROM agent_calls",
				"AVG(ac.total_cost_usd)",
				"SUM(ac.input_tokens)",
				"SUM(ac.output_tokens)",
				"GROUP_CONCAT(ac.duration_ms)",
				"GROUP BY ac.workflow_id",
			},
		},
		{
			name: "querySelectProviderAggregateBuckets",
			sql:  querySelectProviderAggregateBuckets,
			mustContain: []string{
				"FROM agent_calls",
				"COUNT(*)",
				"SUM(ac.success)",
				"AVG(ac.total_cost_usd)",
				"GROUP_CONCAT(ac.duration_ms)",
				"GROUP BY ac.provider_id",
			},
		},
		{
			name: "querySelectFailureRows",
			sql:  querySelectFailureRows,
			mustContain: []string{
				"FROM agent_calls",
				"ac.success = 0",
				"finished_at DESC",
				"LIMIT ?",
			},
		},
		{
			name: "querySelectFailureRawEventSubtypes",
			sql:  querySelectFailureRawEventSubtypes,
			mustContain: []string{
				"FROM raw_events",
				"type = 'error'",
				// IN-list は呼び出し側で fmt.Sprintf により ? に展開される構造的 placeholder
				"agent_call_id IN (%s)",
			},
		},
		{
			name: "queryUpsertRunArtifact",
			sql:  queryUpsertRunArtifact,
			mustContain: []string{
				"INSERT INTO run_artifacts",
				"ON CONFLICT(workflow_run_id, path) DO UPDATE",
				"kind = excluded.kind",
				"size_bytes = excluded.size_bytes",
				"mtime = excluded.mtime",
				"mime = excluded.mime",
			},
		},
		{
			name: "querySelectArtifactsByRunID",
			sql:  querySelectArtifactsByRunID,
			mustContain: []string{
				"FROM run_artifacts",
				"workflow_run_id = ?",
				"ORDER BY path ASC",
			},
		},
		{
			name: "querySelectArtifactByPath",
			sql:  querySelectArtifactByPath,
			mustContain: []string{
				"FROM run_artifacts",
				"workflow_run_id = ?",
				"path = ?",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, frag := range c.mustContain {
				if !strings.Contains(c.sql, frag) {
					t.Errorf("期待句 %q が含まれない\n--- SQL ---\n%s", frag, c.sql)
				}
			}
		})
	}
}

// TestQueryConstantsUseOnlyParameterizedBinds は SQL 定数内に Go 側で値を埋め込むような
// 危険なパターン (% フォーマット指定子のうち %s 以外、SQLite 文字列連結 || など) が
// 混入していないことを検証する (タスク 1.2 完了条件: すべての SQL は ? プレースホルダで
// bind し、文字列連結による値埋め込みを排除する)。
//
// "%s" は querySelectFailureRawEventSubtypes のみで、IN-list の placeholder 数を動的に
// 調整するための構造的展開として許容する (展開後も値はすべて ? 経由で bind される)。
func TestQueryConstantsUseOnlyParameterizedBinds(t *testing.T) {
	allConstants := map[string]string{
		"querySelectRunsList":                 querySelectRunsList,
		"querySelectRunByID":                  querySelectRunByID,
		"querySelectAgentCallsByRunID":        querySelectAgentCallsByRunID,
		"querySelectContextUsageByRunID":      querySelectContextUsageByRunID,
		"querySelectRawEventsByRunID":         querySelectRawEventsByRunID,
		"querySelectWorkflowAggregateRuns":    querySelectWorkflowAggregateRuns,
		"querySelectWorkflowAggregateCalls":   querySelectWorkflowAggregateCalls,
		"querySelectProviderAggregateBuckets": querySelectProviderAggregateBuckets,
		"querySelectFailureRows":              querySelectFailureRows,
		"querySelectFailureRawEventSubtypes":  querySelectFailureRawEventSubtypes,
		"queryUpsertRunArtifact":              queryUpsertRunArtifact,
		"querySelectArtifactsByRunID":         querySelectArtifactsByRunID,
		"querySelectArtifactByPath":           querySelectArtifactByPath,
	}
	for name, sql := range allConstants {
		// %s は querySelectFailureRawEventSubtypes でのみ許容。%d / %v は許容しない
		hasOtherFormatVerb := strings.Contains(sql, "%d") || strings.Contains(sql, "%v")
		if hasOtherFormatVerb {
			t.Errorf("%s: %%d / %%v 等の値埋め込み format 指定子が混入", name)
		}
		hasPercentS := strings.Contains(sql, "%s")
		if hasPercentS && name != "querySelectFailureRawEventSubtypes" {
			t.Errorf("%s: %%s は IN-list 動的展開のみで許容。それ以外で発見", name)
		}
		// SQLite 文字列連結 || はテンプレ展開以外の値埋め込み経路となり得るため検出する
		if strings.Contains(sql, "||") {
			t.Errorf("%s: SQLite 文字列連結演算子 (||) を含む。値埋め込みの疑い", name)
		}
	}
}
