// Package akashic の reader_sqlite_failure.go は、Reader interface (reader.go) のうち
// FailureClusters メソッドの実装を提供する。
//
// 失敗 agent_call のロード (failureRows) と raw_events.subtype 集計
// (failureSubtypesByAgentCallID) は本ファイル内のヘルパに切り出してある。
package akashic

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// FailureClusters は失敗 agent_call をヒューリスティックでクラスタリングして返す
// (Req 5.1〜5.8, 7.5)。
//
// 動作概要:
//  1. opts.Validate() で Limit / SampleSize の負値・上限超過を sentinel error として弾く
//  2. Limit==0 → DefaultFailureClusterLimit、SampleSize==0 → DefaultFailureSampleSize
//     にそれぞれ補完する (Req 5.5 / 5.4 既定値)
//  3. querySelectFailureRows を発行: ac.success=0 の agent_call を
//     finished_at DESC LIMIT MaxFailureRowsLoaded でロードする (Req 5.1 / 5.8)
//  4. ロードした agent_call_id 群を batched IN-list で querySelectFailureRawEventSubtypes に
//     渡し、type='error' の raw_events.subtype を取得する (Req 5.3 / N+1 排除)
//     - 1 agent_call に複数 subtype がある場合は最頻値を採用する (タイ時はアルファベット順で最小)
//  5. Go 側で (failure_category, provider_id, normalizeError(error), subtype) をキーに
//     グルーピングする (Req 5.2)
//     - クラスタ件数を加算
//     - LastSeen は finished_at の最大値 (rows は DESC ロード順なので初回出現値が最大)
//     - RepresentativeRunIDs はクラスタ内の出現順 (= finished_at DESC) で SampleSize 件切り出し (Req 5.4)
//  6. クラスタを Count DESC → LastSeen DESC の安定ソートで並べ替える (Req 5.6)
//  7. クラスタ数が opts.Limit を超える場合は先頭 Limit 件にトリミングする
//  8. LoadedRows == MaxFailureRowsLoaded のとき Truncated=true で返す (Req 5.8)
//
// 派生定義:
//   - 失敗 agent_call の判定: SQL `ac.success = 0` (NULL は in-progress として除外。
//     SQLite の `=` は NULL に対して NULL を返すため自動的に除外される)
//   - クラスタリングキーの subtype は agent_call ごとに raw_events.type='error' の subtype を
//     最頻値で 1 つに集約する (複数 raw_events.error が同 agent_call に紐づく場合の安定化)
//
// LLM ベース分類は本 spec のスコープ外であり、結果に含めない (Req 5.7)。
func (r *SQLiteRecorder) FailureClusters(ctx context.Context, opts FailureOptions) (FailureClustersResult, error) {
	if err := opts.Validate(); err != nil {
		return FailureClustersResult{}, err
	}
	limit := opts.Limit
	if limit == 0 {
		limit = DefaultFailureClusterLimit
	}
	sampleSize := opts.SampleSize
	if sampleSize == 0 {
		sampleSize = DefaultFailureSampleSize
	}

	// queries.go の sentinel-binding パターン: 空文字 sentinel が真なら絞り込みなし。
	sinceSentinel, sinceValue := timeSentinelAndValue(opts.Range.Since)
	untilSentinel, untilValue := timeSentinelAndValue(opts.Range.Until)
	workflowSentinel := opts.WorkflowID
	providerSentinel := opts.ProviderID
	projectSentinel := opts.ProjectID

	// Step 1: 失敗 agent_call を finished_at DESC でロードする。
	rows, err := r.failureRows(ctx,
		sinceSentinel, sinceValue,
		untilSentinel, untilValue,
		workflowSentinel, opts.WorkflowID,
		providerSentinel, opts.ProviderID,
		projectSentinel, opts.ProjectID,
	)
	if err != nil {
		return FailureClustersResult{}, err
	}

	loadedRows := len(rows)
	// Truncated 判定は MaxFailureRowsLoaded の現在値と比較する (テストから上書き可能な
	// var 設計のため、loadedRows == MaxFailureRowsLoaded の参照は実行時の値を見る)。
	truncated := loadedRows == MaxFailureRowsLoaded

	if loadedRows == 0 {
		// Req 5.8: LoadedRows / Truncated 情報のみを返す。Clusters は nil で
		// 「該当なし」を呼び出し側が判別可能にする。
		return FailureClustersResult{Clusters: nil, LoadedRows: 0, Truncated: false}, nil
	}

	// Step 2: agent_call_id を batched IN-list で raw_events.subtype を取得 (Req 5.3)。
	subtypeByCallID, err := r.failureSubtypesByAgentCallID(ctx, rows)
	if err != nil {
		return FailureClustersResult{}, err
	}

	// Step 3: Go 側でクラスタリング。rows は finished_at DESC のロード順を維持しているため、
	// クラスタ内の出現順 = finished_at DESC が自動的に成立する。
	type clusterKey struct {
		failureCategory string
		providerID      string
		normalizedError string
		subtype         string
	}
	type clusterAcc struct {
		key                  clusterKey
		count                int64
		lastSeen             time.Time
		representativeRunIDs []string
		// representativeAgentCallIDs は representativeRunIDs と同順 / 同件数で切り出した
		// 失敗 agent_call ID 群 (resolution-chain 拡張 Pattern 1 用)。
		representativeAgentCallIDs []string
		// representativeWorkflowIDs / representativeStepIDs / representativeErrors /
		// representativeResultSummaries は representativeAgentCallIDs と同順 / 同件数で
		// 切り出した付随情報 (resolution-chain 拡張 Pattern 2 用)。
		// Pattern 2 では NextSuccessAcrossRuns に (workflow_id, step_id) を渡し、
		// 失敗 call の error / result_summary を gitresolve.go の path 抽出に渡す。
		representativeWorkflowIDs     []string
		representativeStepIDs         []string
		representativeErrors          []string
		representativeResultSummaries []string
		// userCorrectionCount はクラスタ内の user_corrected = 1 件数の合計
		// (memory-distiller-trigger-policy Req 4.1, 6.3)。SQL 側は filter+sort のみで
		// 集計せず、ここで Go 側の row 反復ループで SUM 相当を計算する。
		userCorrectionCount int64
	}
	clusterByKey := make(map[clusterKey]*clusterAcc)
	// クラスタ初出現順を保持 (map iteration は非決定的なため、後続のソート前段階で
	// 安定的な順序を確立する。最終ソートは Count DESC → LastSeen DESC で再整列される)。
	var clusterOrder []clusterKey

	for _, row := range rows {
		key := clusterKey{
			failureCategory: row.failureCategory,
			providerID:      row.providerID,
			normalizedError: normalizeError(row.errorMsg),
			subtype:         subtypeByCallID[row.agentCallID],
		}
		acc, ok := clusterByKey[key]
		if !ok {
			acc = &clusterAcc{key: key}
			clusterByKey[key] = acc
			clusterOrder = append(clusterOrder, key)
		}
		acc.count++
		// rows は finished_at DESC の順なので初出現値が最大。後続の比較は不要だが
		// finished_at が NULL のロード経路 (defensive scan で zero value) を考慮し、
		// 明示的に最大値判定する。
		if row.finishedAt.After(acc.lastSeen) {
			acc.lastSeen = row.finishedAt
		}
		// RepresentativeRunIDs はクラスタ内の出現順 (finished_at DESC) で SampleSize 件
		// 切り出し (Req 5.4)。representativeAgentCallIDs は同順 / 同件数で同じ row 由来の
		// agent_call_id を切り出す (resolution-chain 拡張 Pattern 1 用 / インデックス i で
		// RepresentativeRunIDs[i] と対応するペア)。
		if len(acc.representativeRunIDs) < sampleSize {
			acc.representativeRunIDs = append(acc.representativeRunIDs, row.workflowRunID)
			acc.representativeAgentCallIDs = append(acc.representativeAgentCallIDs, row.agentCallID)
			acc.representativeWorkflowIDs = append(acc.representativeWorkflowIDs, row.workflowID)
			acc.representativeStepIDs = append(acc.representativeStepIDs, row.stepID)
			acc.representativeErrors = append(acc.representativeErrors, row.errorMsg)
			acc.representativeResultSummaries = append(acc.representativeResultSummaries, row.resultSummary)
		}
		// user-correction marker の SUM 相当を加算 (Req 4.1, 6.3)。SQL 側は COALESCE で
		// 0/1 に正規化済みのため、加算で SUM(user_corrected) と等価な集計が得られる。
		acc.userCorrectionCount += row.userCorrected
	}

	// Step 4: クラスタを FailureCluster 配列に変換し、Count DESC → LastSeen DESC で安定ソート。
	clusters := make([]FailureCluster, 0, len(clusterByKey))
	for _, key := range clusterOrder {
		acc := clusterByKey[key]
		clusters = append(clusters, FailureCluster{
			FailureCategory:               acc.key.failureCategory,
			ProviderID:                    acc.key.providerID,
			NormalizedErrorKey:            acc.key.normalizedError,
			RawEventErrorSubtype:          acc.key.subtype,
			Count:                         acc.count,
			LastSeen:                      acc.lastSeen,
			RepresentativeRunIDs:          acc.representativeRunIDs,
			RepresentativeAgentCallIDs:    acc.representativeAgentCallIDs,
			RepresentativeWorkflowIDs:     acc.representativeWorkflowIDs,
			RepresentativeStepIDs:         acc.representativeStepIDs,
			RepresentativeErrors:          acc.representativeErrors,
			RepresentativeResultSummaries: acc.representativeResultSummaries,
			// SUM(user_corrected) 相当 (Req 4.1, 6.3)。severity は workflow 層で導出するため
			// Reader 層の戻り値には含めない (関心の分離)。
			UserCorrectionCount: acc.userCorrectionCount,
		})
	}
	sort.SliceStable(clusters, func(i, j int) bool {
		if clusters[i].Count != clusters[j].Count {
			return clusters[i].Count > clusters[j].Count
		}
		return clusters[i].LastSeen.After(clusters[j].LastSeen)
	})

	// Step 5: クラスタ数を opts.Limit (= DefaultFailureClusterLimit / opts) で先頭からトリミング。
	if len(clusters) > limit {
		clusters = clusters[:limit]
	}

	return FailureClustersResult{
		Clusters:   clusters,
		LoadedRows: loadedRows,
		Truncated:  truncated,
	}, nil
}

// failureRow は FailureClusters 内部で agent_call ロード結果を一時保持する非公開型。
// queries.go の querySelectFailureRows は COALESCE で string カラムを空文字列に正規化済み
// のため、failureCategory / providerID / errorMsg は plain string で受けて良い。
//
// userCorrected は agent_calls.user_corrected 列の値 (0 / 1)。
// COALESCE(user_corrected, 0) を経由するため legacy row (NULL) は 0 として読める
// (memory-distiller-trigger-policy Req 4.1, 6.3)。クラスタリング後の Go 側集計ループで
// `acc.userCorrectionCount += row.userCorrected` の形で SUM 相当を計算する。
type failureRow struct {
	agentCallID   string
	workflowRunID string
	// workflowID / stepID は agent_calls.workflow_id / step_id をそのまま COALESCE で
	// 空文字正規化済みで受ける (resolution-chain 拡張 Pattern 2: NextSuccessAcrossRuns
	// の入力として cluster ごとに代表値を持たせる)。
	workflowID      string
	stepID          string
	failureCategory string
	providerID      string
	errorMsg        string
	// resultSummary は agent_calls.result_summary を空文字正規化済みで受ける
	// (resolution-chain 拡張 Pattern 2: 失敗 call の context として gitresolve.go の
	// path 抽出に渡す。LLM 入力の token 予算 cap は workflow 層で行う)。
	resultSummary string
	finishedAt    time.Time
	userCorrected int64
}

// failureRows は querySelectFailureRows を発行し、失敗 agent_call を finished_at DESC で
// MaxFailureRowsLoaded 件までロードする (Req 5.1 / 5.8)。
//
// rows.Close を defer で確実に行うため小ヘルパに切り出した (FailureClusters 本体の
// 可読性とリソース管理を分離する)。
func (r *SQLiteRecorder) failureRows(ctx context.Context, args ...any) ([]failureRow, error) {
	// args は queries.go の sentinel/value 8 個を呼び出し側で構築済みで渡す前提。
	// LIMIT バインドは MaxFailureRowsLoaded の現在値 (テストから上書き可能) を末尾に追加する。
	queryArgs := append(append([]any{}, args...), MaxFailureRowsLoaded)

	sqlRows, err := r.db.QueryContext(ctx, querySelectFailureRows, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("akashic: failure clusters rows: %w", err)
	}
	defer sqlRows.Close()

	var out []failureRow
	for sqlRows.Next() {
		var (
			agentCallID     string
			workflowRunID   sql.NullString
			workflowID      string
			stepID          string
			failureCategory string
			providerID      string
			errorMsg        string
			resultSummary   string
			finishedAtStr   sql.NullString
			// user_corrected は SQL 側 COALESCE(..., 0) で正規化済みのため
			// plain int64 で受ける (NULL 経路は到達しない / Req 6.3 防御)。
			userCorrected int64
		)
		if err := sqlRows.Scan(&agentCallID, &workflowRunID, &workflowID, &stepID, &failureCategory, &providerID, &errorMsg, &resultSummary, &finishedAtStr, &userCorrected); err != nil {
			return nil, fmt.Errorf("akashic: failure clusters scan: %w", err)
		}
		var finishedAt time.Time
		if finishedAtStr.Valid {
			t, parseErr := parseStoredTime(finishedAtStr.String)
			if parseErr != nil {
				return nil, fmt.Errorf("akashic: parse failure finished_at for agent_call %s: %w", agentCallID, parseErr)
			}
			finishedAt = t
		}
		out = append(out, failureRow{
			agentCallID:     agentCallID,
			workflowRunID:   nullStringValue(workflowRunID),
			workflowID:      workflowID,
			stepID:          stepID,
			failureCategory: failureCategory,
			providerID:      providerID,
			errorMsg:        errorMsg,
			resultSummary:   resultSummary,
			finishedAt:      finishedAt,
			userCorrected:   userCorrected,
		})
	}
	if err := sqlRows.Err(); err != nil {
		return nil, fmt.Errorf("akashic: failure clusters iter: %w", err)
	}
	return out, nil
}

// failureSubtypesByAgentCallID は querySelectFailureRawEventSubtypes を batched IN-list で
// 発行し、agent_call_id ごとの代表 subtype (最頻値) を返す (Req 5.3)。
//
// 方針:
//   - rows から agent_call_id をユニーク化して IN-list を構築 (重複 placeholder を避ける)
//   - SQL 結果の (agent_call_id, subtype) 行をカウント集計
//   - 1 agent_call に複数 subtype が紐づく場合は最頻値を採用。タイ時はアルファベット順で
//     最小の subtype を選ぶことで決定論性を保つ (Req 3.9 / 5.6 と同じ「冪等性」原則)
//
// IN-list の placeholder 数だけが動的に変わり、値は依然 "?" 経由でしか bind しない
// (queries.go コメント参照)。
func (r *SQLiteRecorder) failureSubtypesByAgentCallID(ctx context.Context, rows []failureRow) (map[string]string, error) {
	if len(rows) == 0 {
		return map[string]string{}, nil
	}

	// 1. agent_call_id を初出現順でユニーク化する。順序は IN-list のソートに本質的でないが、
	//    placeholder と引数 slice の対応を保つために配列で持つ。
	idSet := make(map[string]struct{}, len(rows))
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if _, ok := idSet[row.agentCallID]; ok {
			continue
		}
		idSet[row.agentCallID] = struct{}{}
		ids = append(ids, row.agentCallID)
	}

	// 2. IN-list 用の placeholder ("?,?,...,?") を len(ids) 個ぶん展開する。
	//    fmt.Sprintf 経由の構造的 placeholder 数調整であり、値の文字列連結は行わない。
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	sqlStr := fmt.Sprintf(querySelectFailureRawEventSubtypes, placeholders)

	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}

	sqlRows, err := r.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("akashic: failure subtypes: %w", err)
	}
	defer sqlRows.Close()

	// 3. (agent_call_id → (subtype → count)) のネスト集計。SQL の COALESCE で subtype の
	//    NULL は '' に正規化済みのため string で受けて良い。
	countByCallID := make(map[string]map[string]int)
	for sqlRows.Next() {
		var (
			agentCallID string
			subtype     string
		)
		if err := sqlRows.Scan(&agentCallID, &subtype); err != nil {
			return nil, fmt.Errorf("akashic: failure subtypes scan: %w", err)
		}
		bucket := countByCallID[agentCallID]
		if bucket == nil {
			bucket = make(map[string]int)
			countByCallID[agentCallID] = bucket
		}
		bucket[subtype]++
	}
	if err := sqlRows.Err(); err != nil {
		return nil, fmt.Errorf("akashic: failure subtypes iter: %w", err)
	}

	// 4. 各 agent_call の最頻 subtype を選ぶ。タイ時はアルファベット順で最小を採用 (決定論性)。
	result := make(map[string]string, len(countByCallID))
	for callID, counts := range countByCallID {
		subs := make([]string, 0, len(counts))
		for s := range counts {
			subs = append(subs, s)
		}
		sort.Strings(subs)
		var (
			bestSubtype string
			bestCount   int
		)
		for _, s := range subs {
			if c := counts[s]; c > bestCount {
				bestCount = c
				bestSubtype = s
			}
		}
		result[callID] = bestSubtype
	}
	return result, nil
}
