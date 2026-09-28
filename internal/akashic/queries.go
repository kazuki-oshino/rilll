// Package akashic の queries.go は、Reader 実装 (reader_sqlite.go) が発行する SQL 文を
// package private な名前付き定数として集約する。
//
// 設計上の方針:
//   - すべての値は "?" プレースホルダで bind し、文字列連結による値埋め込みを排除する
//     (security: SQL injection の根絶 / 設計: 文と値の関心分離)
//   - 一部の WHERE 句はオプショナル絞り込みを表現するため `(? = ” OR col = ?)` 形式の
//     sentinel-binding パターンを採用する。呼び出し側 (reader_sqlite.go) は同一の値を
//     2 つの ? に bind するか、空文字列を 2 回 bind することで「絞り込みなし」を表現する
//   - Success のような tri-state 絞り込みは int sentinel (-1: 全件 / 1: 成功のみ /
//     0: 失敗のみ) を 2 つの ? に同値で bind する形にする
//   - querySelectFailureRawEventSubtypes は IN-list の placeholder 数が動的なため、
//     fmt.Sprintf 用の format template として "%s" を残す。値は依然 "?" でしか bind しない
//     (テンプレート展開はあくまで構造的な placeholder 数の調整)
//
// すべての定数は package private (lowercase first letter) として定義し export しない。
// 呼び出しは同パッケージの reader_sqlite.go (タスク 2.1〜2.5) に閉じる。
package akashic

// querySelectRunsList は List API のメインクエリ (Req 2.1〜2.7, 7.1, 7.3, 7.4)。
//
// workflow_runs を限定する内側 SELECT で limit/offset を適用し、その上で agent_calls を
// LEFT JOIN することで、N*M 行への展開後の LIMIT 暴発を防ぎ「List 結果が呼び出し側
// 指定 limit を超えない」(Req 7.1) を満たす。
//
// 引数順序 (合計 16 個):
//
//  1. workflow_id sentinel (空文字 = 全 workflow)
//  2. workflow_id value
//  3. since sentinel (空文字 = since 無制限)
//  4. since value (RFC3339Nano フォーマット文字列)
//  5. until sentinel
//  6. until value
//  7. success sentinel (-1 = 全件 / 1 = 成功のみ / 0 = 失敗のみ)
//  8. success value (sentinel と同値を bind)
//  9. cwd sentinel
//  10. cwd value
//  11. provider sentinel
//  12. provider value
//  13. project_id sentinel (空文字 = 全 project)
//  14. project_id value
//  15. limit
//  16. offset
//
// 並び順: workflow_runs を started_at DESC, id DESC で安定ソート (Req 2.6 / 7.3)。
// 内部 agent_calls は started_at ASC, id ASC で並べ、Go 側の workflow_run_id 単位
// グルーピング後の AgentCallSummary 配列順序を決定する。
const querySelectRunsList = `
SELECT
    r.id,
    r.workflow_id,
    r.workflow_name,
    r.cwd,
    r.started_at,
    r.finished_at,
    r.error,
    ac.id,
    ac.provider_id,
    ac.step_id,
    ac.step_name,
    ac.success,
    ac.duration_ms,
    ac.total_cost_usd,
    ac.input_tokens,
    ac.output_tokens,
    ac.failure_category,
    ac.error,
    ac.started_at,
    ac.finished_at
FROM workflow_runs AS r
LEFT JOIN agent_calls AS ac ON ac.workflow_run_id = r.id
WHERE r.id IN (
    SELECT id FROM workflow_runs
    WHERE (? = '' OR workflow_id = ?)
      AND (? = '' OR started_at >= ?)
      AND (? = '' OR started_at <= ?)
      AND (? = -1 OR (CASE WHEN error IS NULL AND finished_at IS NOT NULL THEN 1 ELSE 0 END) = ?)
      AND (? = '' OR cwd = ?)
      AND (? = '' OR id IN (SELECT workflow_run_id FROM agent_calls WHERE provider_id = ?))
      AND (? = '' OR project_id = ?)
    ORDER BY started_at DESC, id DESC
    LIMIT ? OFFSET ?
)
ORDER BY r.started_at DESC, r.id DESC, ac.started_at ASC, ac.id ASC
`

// querySelectRunByID は Detail の workflow_run スカラ列を取得する (Req 4.1, 4.7)。
//
// 引数: workflow_run_id (1 つ)。
// 該当 0 行は呼び出し側 (reader_sqlite.go) で sql.ErrNoRows を捉えて
// ErrWorkflowRunNotFound に変換する (Req 4.7)。
const querySelectRunByID = `
SELECT id, workflow_id, workflow_name, cwd, started_at, finished_at, error
FROM workflow_runs
WHERE id = ?
`

// querySelectAgentCallsByRunID は Detail の AgentCalls (AgentCallRow) を完全に取得する
// (Req 4.1, 4.6)。
//
// 引数: workflow_run_id (1 つ)。
// 並び順: started_at ASC, id ASC で安定ソート (Req 4.6)。
const querySelectAgentCallsByRunID = `
SELECT
    id,
    workflow_run_id,
    step_id,
    step_name,
    provider_id,
    prompt,
    cwd,
    session_id,
    model,
    model_reasoning_effort,
    success,
    duration_ms,
    total_cost_usd,
    input_tokens,
    output_tokens,
    result_summary,
    error,
    failure_category,
    started_at,
    finished_at,
    result_payload_json
FROM agent_calls
WHERE workflow_run_id = ?
ORDER BY started_at ASC, id ASC
`

// querySelectContextUsageByRunID は Detail の AgentContextUsage を取得する (Req 4.2)。
//
// 引数: workflow_run_id (1 つ)。
// 並び順: sequence ASC で安定ソート (Req 4.6 と整合: context usage は sequence 単調増加)。
const querySelectContextUsageByRunID = `
SELECT
    agent_call_id,
    workflow_run_id,
    raw_event_id,
    sequence,
    observed_at,
    model,
    total_tokens,
    max_tokens,
    raw_max_tokens,
    percentage,
    is_auto_compact_enabled,
    auto_compact_threshold,
    categories_json,
    memory_files_json,
    mcp_tools_json,
    agents_json,
    message_breakdown_json,
    api_usage_json
FROM agent_context_usage
WHERE workflow_run_id = ?
ORDER BY sequence ASC
`

// querySelectRawEventsByRunID は Detail の RawEvents を type/subtype/limit/offset 付きで
// 取得する (Req 4.3〜4.5, 4.8)。
//
// 引数順序 (合計 7 個):
//
//  1. workflow_run_id
//  2. type sentinel (空文字 = 全 type)
//  3. type value
//  4. subtype sentinel (空文字 = 全 subtype)
//  5. subtype value
//  6. limit (DefaultRawEventsLimit / RawEventsQuery.Limit に従ってクランプ済みの値)
//  7. offset
//
// 並び順: sequence ASC, id ASC で安定ソート (Req 4.6)。
const querySelectRawEventsByRunID = `
SELECT
    id,
    agent_call_id,
    workflow_run_id,
    sequence,
    source,
    type,
    subtype,
    observed_at,
    payload_json,
    payload_truncated
FROM raw_events
WHERE workflow_run_id = ?
  AND (? = '' OR type = ?)
  AND (? = '' OR subtype = ?)
ORDER BY sequence ASC, id ASC
LIMIT ? OFFSET ?
`

// querySelectWorkflowAggregateRuns は AggregateByWorkflow の workflow_runs 母数側集計
// (Req 3.1)。
//
// run_count (workflow_run 総数) と success_count (error IS NULL かつ
// finished_at IS NOT NULL の件数) を workflow_id 単位で取得する。SuccessRate は
// 呼び出し側 (reader_sqlite.go) で success_count / run_count を計算する。
//
// 期間絞り込みは workflow_runs.started_at に適用する (Req 3.6: 期間内に開始した run を
// 集計対象とする)。
//
// 引数順序 (合計 10 個):
//
//  1. since sentinel
//  2. since value
//  3. until sentinel
//  4. until value
//  5. workflow_id sentinel
//  6. workflow_id value
//  7. provider sentinel (provider 絞り込みは agent_calls 経由の subquery)
//  8. provider value
//  9. project_id sentinel (workflow_runs.project_id 直接絞り込み)
//  10. project_id value
//
// 並び順: workflow_id ASC で安定ソート (Req 3.9 冪等性)。
const querySelectWorkflowAggregateRuns = `
SELECT
    workflow_id,
    COUNT(*) AS run_count,
    SUM(CASE WHEN error IS NULL AND finished_at IS NOT NULL THEN 1 ELSE 0 END) AS success_count
FROM workflow_runs
WHERE (? = '' OR started_at >= ?)
  AND (? = '' OR started_at <= ?)
  AND (? = '' OR workflow_id = ?)
  AND (? = '' OR id IN (SELECT workflow_run_id FROM agent_calls WHERE provider_id = ?))
  AND (? = '' OR project_id = ?)
GROUP BY workflow_id
ORDER BY workflow_id ASC
`

// querySelectWorkflowAggregateCalls は AggregateByWorkflow の agent_calls 側統計集計
// (Req 3.2〜3.4, 3.7)。
//
// 当該 workflow に紐づく agent_calls の AVG(total_cost_usd) / SUM(input_tokens) /
// SUM(output_tokens) / GROUP_CONCAT(duration_ms) を workflow_id 単位で取得する。
// duration_ms は呼び出し側で split + sort して percentile (p50/p95) を算出する
// (modernc.org/sqlite に PERCENTILE_CONT がないため Go 側計算)。
// AVG / SUM は SQLite が NULL を母数除外して計算するため Req 3.7 を自動的に満たす。
//
// 期間絞り込みは workflow_runs.started_at に適用する (Req 3.6: 期間内に開始した run の
// agent_calls を集計対象とする)。このため workflow_runs と JOIN する。
//
// 引数順序 (合計 10 個):
//
//  1. since sentinel
//  2. since value
//  3. until sentinel
//  4. until value
//  5. workflow_id sentinel (ac.workflow_id を比較。agent_calls 側に denormalize 済み)
//  6. workflow_id value
//  7. provider sentinel (ac.provider_id を比較)
//  8. provider value
//  9. project_id sentinel (r.project_id を比較。母集団は workflow_runs に揃える)
//  10. project_id value
//
// 並び順: workflow_id ASC で安定ソート (Req 3.9)。
const querySelectWorkflowAggregateCalls = `
SELECT
    ac.workflow_id,
    AVG(ac.total_cost_usd) AS avg_cost_usd,
    SUM(ac.input_tokens) AS sum_input_tokens,
    SUM(ac.output_tokens) AS sum_output_tokens,
    GROUP_CONCAT(ac.duration_ms) AS durations
FROM agent_calls AS ac
JOIN workflow_runs AS r ON r.id = ac.workflow_run_id
WHERE (? = '' OR r.started_at >= ?)
  AND (? = '' OR r.started_at <= ?)
  AND (? = '' OR ac.workflow_id = ?)
  AND (? = '' OR ac.provider_id = ?)
  AND (? = '' OR r.project_id = ?)
GROUP BY ac.workflow_id
ORDER BY ac.workflow_id ASC
`

// querySelectProviderAggregateBuckets は AggregateByProvider の集計クエリ (Req 3.5〜3.9, 7.5)。
//
// 母数は agent_call (workflow_run 横断、Req 3.5)。COUNT(*) で agent_call 全件を数え、
// SUM(success) で success=1 の件数を集計する (NULL は除外され in-progress を success
// 扱いしない)。AVG / SUM の NULL 除外も SQLite の集約関数挙動に従う (Req 3.7)。
//
// 期間絞り込みは agent_calls.started_at に直接適用する (Req 3.6: 期間内に開始した
// agent_call を集計対象とする)。agent_calls には workflow_id が denormalize されている
// ため workflow_runs との JOIN は不要。
//
// 集計不変条件 (Req 3.5): 全 provider の CallCount 合算 = フィルタ範囲内の agent_call 総数。
// 本クエリは agent_calls 母数を直接 GROUP BY ac.provider_id しているため自動的に成立する。
//
// 引数順序 (合計 10 個):
//
//  1. since sentinel (ac.started_at を比較)
//  2. since value
//  3. until sentinel
//  4. until value
//  5. workflow_id sentinel (ac.workflow_id を直接比較)
//  6. workflow_id value
//  7. provider sentinel
//  8. provider value
//  9. project_id sentinel (ac.project_id を直接比較)
//  10. project_id value
//
// 並び順: provider_id ASC で安定ソート (Req 3.9)。
const querySelectProviderAggregateBuckets = `
SELECT
    ac.provider_id,
    COUNT(*) AS call_count,
    SUM(ac.success) AS success_count,
    AVG(ac.total_cost_usd) AS avg_cost_usd,
    SUM(ac.input_tokens) AS sum_input_tokens,
    SUM(ac.output_tokens) AS sum_output_tokens,
    GROUP_CONCAT(ac.duration_ms) AS durations
FROM agent_calls AS ac
WHERE (? = '' OR ac.started_at >= ?)
  AND (? = '' OR ac.started_at <= ?)
  AND (? = '' OR ac.workflow_id = ?)
  AND (? = '' OR ac.provider_id = ?)
  AND (? = '' OR ac.project_id = ?)
GROUP BY ac.provider_id
ORDER BY ac.provider_id ASC
`

// querySelectFailureRows は FailureClusters の失敗 agent_call ロードクエリ (Req 5.1, 5.5, 5.8)。
//
// SQL は filter+sort のみで対象 agent_call をロードし、クラスタリングは Go 側で
// normalizeError を適用して行う (Req 5.2 / reader_internal.go の責務)。
//
// 失敗判定は ac.success = 0 (NULL は in-progress / 未完了として除外。SQLite の `=` は
// NULL に対して NULL を返すため自動的に除外される)。
//
// COALESCE で NULL カラムを空文字列に正規化することで、Go 側 scan の型を string に
// 揃え、グルーピングキー比較を簡潔にする。
//
// 引数順序 (合計 11 個):
//
//  1. since sentinel (ac.started_at を比較)
//  2. since value
//  3. until sentinel
//  4. until value
//  5. workflow_id sentinel (ac.workflow_id を直接比較)
//  6. workflow_id value
//  7. provider sentinel
//  8. provider value
//  9. project_id sentinel (ac.project_id を直接比較)
//  10. project_id value
//  11. limit (MaxFailureRowsLoaded を bind し、LoadedRows == MaxFailureRowsLoaded で
//     Truncated=true 判定を行う - Req 5.8)
//
// 並び順: finished_at DESC で最新失敗を優先取得し、クラスタ内代表 run の切り出し
// (RepresentativeRunIDs) に使う順序として一貫させる (Req 5.4 / 5.6)。
//
// user_corrected 列 (memory-distiller-trigger-policy Req 4.1, 6.3) は COALESCE で
// NULL を 0 に正規化し、legacy row (migration 前の DB から ALTER TABLE で追加された行)
// が安全に「user-correction なし」として読めるようにする (defense-in-depth)。
// 集計は Reader 層 (reader_sqlite_failure.go) の Go 側 group ループで SUM 相当として加算する。
const querySelectFailureRows = `
SELECT
    ac.id,
    ac.workflow_run_id,
    COALESCE(ac.workflow_id, '') AS workflow_id,
    COALESCE(ac.step_id, '') AS step_id,
    COALESCE(ac.failure_category, '') AS failure_category,
    COALESCE(ac.provider_id, '') AS provider_id,
    COALESCE(ac.error, '') AS error,
    COALESCE(ac.result_summary, '') AS result_summary,
    ac.finished_at,
    COALESCE(ac.user_corrected, 0) AS user_corrected
FROM agent_calls AS ac
WHERE ac.success = 0
  AND (? = '' OR ac.started_at >= ?)
  AND (? = '' OR ac.started_at <= ?)
  AND (? = '' OR ac.workflow_id = ?)
  AND (? = '' OR ac.provider_id = ?)
  AND (? = '' OR ac.project_id = ?)
ORDER BY ac.finished_at DESC
LIMIT ?
`

// queryUpsertRunArtifact は run_artifacts への 1 件 upsert クエリ
// (akashic-artifacts spec Req 1.1, 1.6)。
//
// `(workflow_run_id, path)` 複合 PK に対する upsert で、既存行がある場合は
// `kind` / `size_bytes` / `mtime` / `mime` を最新値で上書きし、重複行を作らない (Req 1.6 冪等)。
//
// 引数順序 (合計 6 個):
//
//  1. workflow_run_id (TEXT NOT NULL)
//  2. path            (TEXT NOT NULL、絶対 clean path、呼び出し側で正規化済み)
//  3. kind            (TEXT NOT NULL、closed set: proposal / review / result / context)
//  4. size_bytes      (INTEGER NOT NULL)
//  5. mtime           (TEXT NOT NULL、RFC3339Nano UTC)
//  6. mime            (TEXT NULLABLE、検出失敗時は sql.NullString で NULL)
const queryUpsertRunArtifact = `
INSERT INTO run_artifacts (workflow_run_id, path, kind, size_bytes, mtime, mime)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(workflow_run_id, path) DO UPDATE SET
  kind = excluded.kind,
  size_bytes = excluded.size_bytes,
  mtime = excluded.mtime,
  mime = excluded.mime
`

// querySelectArtifactsByRunID は ArtifactReader.GetArtifacts のメイン取得クエリ
// (akashic-artifacts spec Req 3.1, 3.2, 5.4)。
//
// 指定 workflow_run_id に紐づくすべての run_artifacts 行を path 昇順で返す。
// 0 件は呼び出し側 (artifact_reader_sqlite.go) で空 slice + nil error として扱う (Req 3.2 / 5.4)。
//
// 引数: workflow_run_id (1 つ)。
// 並び順: path ASC で安定ソート (path は (workflow_run_id, path) 複合 PK の一部、決定論的)。
const querySelectArtifactsByRunID = `
SELECT workflow_run_id, path, kind, size_bytes, mtime, mime
FROM run_artifacts
WHERE workflow_run_id = ?
ORDER BY path ASC
`

// queryInsertLearning は agent-learnings spec (task 3.2 / Req 5.1, 5.2, 5.3, 9.6)
// の learnings table への 1 件 INSERT クエリ。
//
// 設計契約:
//   - learnings は append-only (UPDATE / DELETE なし) のため ON CONFLICT 句を持たない。
//     PK 衝突は呼び出し側 (Reflector phase) のバグとして上位に伝播させる。
//   - paths_glob_json は呼び出し側 (learnings_sqlite.go RecordLearning) で `[]string` を
//     `json.Marshal` で encode した文字列を bind する。
//   - recorded_at は formatTime で RFC3339Nano UTC に整形して bind する (他テーブルと統一)。
//
// 引数順序 (合計 11 個):
//
//  1. id
//  2. agent_call_id
//  3. workflow_run_id
//  4. workflow_id
//  5. step_id
//  6. category
//  7. context
//  8. insight
//  9. paths_glob_json (TEXT, JSON encoded []string)
//  10. source
//  11. recorded_at (TEXT, RFC3339Nano UTC)
const queryInsertLearning = `
INSERT INTO learnings (
  id, agent_call_id, workflow_run_id, workflow_id, step_id,
  category, context, insight, paths_glob_json, source, recorded_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`

// querySelectLearningsByWorkflowRun は agent-learnings spec (task 3.2 / Req 5.1) の
// LearningsByWorkflowRun 用 SELECT クエリ。
//
// 引数: workflow_run_id (1 つ)。
// 並び順: recorded_at ASC, id ASC で安定ソート (round-trip テストの決定論性 + Curator
// phase の入力順序保証)。
//
// 戻り値カラム順序は LearningRow フィールド順と一致させる (learnings_sqlite.go の Scan
// と表裏一致)。
const querySelectLearningsByWorkflowRun = `
SELECT
    id, agent_call_id, workflow_run_id, workflow_id, step_id,
    category, context, insight, paths_glob_json, source, recorded_at
FROM learnings
WHERE workflow_run_id = ?
ORDER BY recorded_at ASC, id ASC
`

// querySelectAgentCallsByWorkflowRunForLearnings は agent-learnings spec
// (task 3.2 / Req 3.1, 3.6) の Reader.AgentCallsByWorkflowRun 用 SELECT クエリ。
//
// PreFetch phase が parent_run_id から session_id を集約するために使う。Detail とは
// 別 SQL を持つ理由:
//   - PreFetch は project_id を post-filter するため AgentCallRow.ProjectID を populate
//     する必要がある (Detail SQL は project_id を読まない)。
//   - PreFetch は session_id 集約 + project scope 強制 (Req 3.6) が中心で、
//     result_payload_json / failure_category 等の重い列は不要。
//
// 戻り値カラム (合計 11 個): AgentCallRow の subset (PreFetch / ProjectScopedReader が
// 必要とする列のみ)。
//
// 並び順: started_at ASC, id ASC で安定ソート (Detail と同形 / Req 3.2 の dedup 順序の
// tiebreaker として使われる)。
//
// 引数: workflow_run_id (1 つ)。
const querySelectAgentCallsByWorkflowRunForLearnings = `
SELECT
    id,
    workflow_run_id,
    step_id,
    step_name,
    provider_id,
    cwd,
    project_id,
    session_id,
    success,
    started_at,
    finished_at
FROM agent_calls
WHERE workflow_run_id = ?
ORDER BY started_at ASC, id ASC
`

// querySelectArtifactByPath は ArtifactReader の allowlist 検証で使う登録確認クエリ
// (akashic-artifacts spec Req 4.1, 4.2)。
//
// `(workflow_run_id, path)` 複合 PK のヒット有無のみを判定するための SELECT 1 形式。
// 行が無ければ allowlist 不合格 (= run_artifacts に未登録) として呼び出し側が
// `ErrArtifactNotAllowed` を返す。
//
// 引数順序 (合計 2 個):
//
//  1. workflow_run_id
//  2. path (絶対 clean path、呼び出し側で正規化済み)
const querySelectArtifactByPath = `
SELECT 1
FROM run_artifacts
WHERE workflow_run_id = ? AND path = ?
`

const queryUpsertWorkflowRunChild = `
INSERT INTO workflow_run_edges (
  parent_run_id, child_run_id, child_workflow_id, child_workflow_name, recorded_at
) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(parent_run_id, child_run_id) DO UPDATE SET
  child_workflow_id = excluded.child_workflow_id,
  child_workflow_name = excluded.child_workflow_name,
  recorded_at = excluded.recorded_at
`

const querySelectDescendantWorkflowRunIDs = `
WITH RECURSIVE descendants(child_run_id) AS (
  SELECT child_run_id
  FROM workflow_run_edges
  WHERE parent_run_id = ?

  UNION

  SELECT edge.child_run_id
  FROM workflow_run_edges AS edge
  JOIN descendants AS d ON edge.parent_run_id = d.child_run_id
)
SELECT d.child_run_id
FROM descendants AS d
LEFT JOIN workflow_runs AS run ON run.id = d.child_run_id
ORDER BY run.started_at ASC, d.child_run_id ASC
`

// querySelectFailureRawEventSubtypes は FailureClusters の補助クエリ (Req 5.3)。
//
// querySelectFailureRows でロードした agent_call_id 群に対して raw_events の error
// subtype を batched 単一 SELECT で取得し N+1 を排除する。
//
// "%s" は呼び出し側 (reader_sqlite.go) で fmt.Sprintf を用いて agent_call_id 件数分の
// "?,?,...,?" に展開する。展開後はすべての値が "?" プレースホルダ経由で bind される
// ため、文字列連結による値埋め込みは発生しない (security 観点での連結は構造的
// placeholder 数の調整のみで、値の連結は一切行わない)。
//
// COALESCE で subtype の NULL を空文字列に正規化し、Go 側 scan の型を string に揃える。
//
// 引数: agent_call_id を IN-list 件数分 bind する。
const querySelectFailureRawEventSubtypes = `
SELECT agent_call_id, COALESCE(subtype, '') AS subtype
FROM raw_events
WHERE type = 'error' AND agent_call_id IN (%s)
`

// querySelectInRunResolutions は InRunResolutionsForCluster (Pattern 1: 同 run 内
// resolution lookup) のメインクエリ。
//
// 各 failing agent_call (id 指定) について、同 workflow_run_id / 同 step_id で
// success=1 かつ started_at > failing.started_at の最初の成功 agent_call を 1 件だけ返す。
// SQLite ではウィンドウ関数を使うより self-join + GROUP BY MIN(started_at) のほうが
// SQLite 3.30 未満の互換性が高いが、failing IDs は一般に小さい (代表 sample 数 = 5 程度)
// ため correlated subquery で十分高速。
//
// "%s" は IN-list (failing agent_call IDs) を呼び出し側で "?,?,...,?" に展開する。
// 値はすべて "?" 経由で bind され、文字列連結による値埋め込みは無い (placeholder 数の
// 構造的調整のみ)。
//
// project_id sentinel ("? = ” OR ...") は ProjectScopedReader 経由の上書きで
// 設定される。空文字なら全 project (Reader を直接呼ぶ経路用)。
//
// 戻り値カラム順序 (合計 7 個):
//
//  1. failing.id            (入力 ID と一致 — caller の map key になる)
//  2. success.id            (成功 agent_call の id)
//  3. success.workflow_run_id
//  4. success.step_id
//  5. success.started_at
//  6. success.result_summary (COALESCE で空文字に正規化)
//  7. success.prompt         (COALESCE で空文字に正規化)
//
// 引数: project_id sentinel + value + failing IDs (IN-list)。
const querySelectInRunResolutions = `
SELECT
    failing.id           AS failing_id,
    success.id           AS success_id,
    success.workflow_run_id,
    COALESCE(success.step_id, '') AS step_id,
    success.started_at,
    COALESCE(success.result_summary, '') AS result_summary,
    COALESCE(success.prompt, '') AS prompt
FROM agent_calls AS failing
JOIN agent_calls AS success
  ON success.workflow_run_id = failing.workflow_run_id
 AND success.step_id         = failing.step_id
 AND success.success          = 1
 AND success.started_at       > failing.started_at
WHERE failing.id IN (%s)
  AND (? = '' OR failing.project_id = ?)
  AND success.id = (
    SELECT s2.id
    FROM agent_calls AS s2
    WHERE s2.workflow_run_id = failing.workflow_run_id
      AND s2.step_id         = failing.step_id
      AND s2.success         = 1
      AND s2.started_at      > failing.started_at
    ORDER BY s2.started_at ASC
    LIMIT 1
  )
`

// querySelectNextSuccessAcrossRuns は NextSuccessAcrossRuns (Pattern 2: 別 run での
// next success lookup) のメインクエリ。
//
// 失敗 cluster の (project_id, workflow_id, step_id, after_time) に対し、
// after_time より後で最初に success=1 となった agent_call を 1 件返す。
// 「同 run 内」制約は一切持たない (Pattern 1 で同 run 内が見つからなかった cluster に
// 対する fallback として呼ばれる)。
//
// 引数順序 (合計 8 個):
//
//  1. project_id sentinel  (空文字 → 全 project)
//  2. project_id value
//  3. workflow_id sentinel (空文字 → 全 workflow)
//  4. workflow_id value
//  5. step_id sentinel     (空文字 → 全 step)
//  6. step_id value
//  7. after_time sentinel  (空文字 → 制約なし)
//  8. after_time value     (RFC3339Nano 文字列。ゼロ値は呼び出し側で空文字に正規化)
//
// 戻り値カラム順序 (合計 3 個):
//
//  1. id              (agent_call_id)
//  2. workflow_run_id
//  3. started_at
const querySelectNextSuccessAcrossRuns = `
SELECT
    ac.id,
    ac.workflow_run_id,
    ac.started_at
FROM agent_calls AS ac
WHERE ac.success = 1
  AND (? = '' OR ac.project_id  = ?)
  AND (? = '' OR ac.workflow_id = ?)
  AND (? = '' OR ac.step_id     = ?)
  AND (? = '' OR ac.started_at  > ?)
ORDER BY ac.started_at ASC
LIMIT 1
`
