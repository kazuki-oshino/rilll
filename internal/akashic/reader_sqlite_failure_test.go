// Package akashic の reader_sqlite_failure_test.go は、reader_sqlite_failure.go
// (FailureClusters 系) の TDD discipline を担保する companion test。
//
// 各タスクの「観測可能な完了条件」を最小限の smoke test として担保し、
// 包括的な table-driven 検証はタスク 4.5 で reader_test.go に追加される想定。
package akashic

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// insertFailureClusterFixture はタスク 2.5 の観測可能な完了条件を担保するための fixture を
// 投入する共通ヘルパ。
//
// keyTag で識別される (failure_category, provider_id, error) の 3 つ組をクラスタリング
// キーとして共有する agent_call 群を、count 件 1 本の workflow_run 配下に作る。
//
// 各 agent_call は finished_at が 1 分間隔で増加するように散らされる (base + minute*N)。
// success=false で RecordAgentCallFinish を呼び、failure_category と error を埋める。
//
// 同一 keyTag で複数回呼ばれた場合は同一 workflow_run / 同一 agent_call_id 接頭辞に
// 衝突するため、呼び出し側は keyTag をユニークに保つこと。
func insertFailureClusterFixture(
	t *testing.T,
	ctx context.Context,
	recorder *SQLiteRecorder,
	keyTag string,
	failureCategory string,
	providerID string,
	errorMsg string,
	count int,
	base time.Time,
) {
	t.Helper()
	runID := fmt.Sprintf("run-%s", keyTag)
	if err := recorder.RecordWorkflowRunStart(ctx, WorkflowRun{
		ID:           runID,
		WorkflowID:   "wf",
		WorkflowName: "WF",
		CWD:          "/repo",
		StartedAt:    base,
	}); err != nil {
		t.Fatalf("RecordWorkflowRunStart %s: %v", runID, err)
	}
	for i := range count {
		callID := fmt.Sprintf("ac-%s-%d", keyTag, i)
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
		if err := recorder.RecordAgentCallFinish(ctx, AgentCallResult{
			ID:              callID,
			SessionID:       "sess",
			Success:         false,
			DurationMS:      100,
			FailureCategory: failureCategory,
			Error:           errorMsg,
			FinishedAt:      started.Add(30 * time.Second),
		}); err != nil {
			t.Fatalf("RecordAgentCallFinish %s: %v", callID, err)
		}
	}
}

// TestSQLiteRecorderFailureClustersGroupsByKey はタスク 2.5 の観測可能な完了条件を
// 担保する smoke test:
//
//	失敗 agent_call 100 件 (3 クラスタ: A=50, B=30, C=20) の fixture で
//	FailureClusters が
//	  - len(Clusters) == 3 を返す (Req 5.2: クラスタリングキー単位)
//	  - Count DESC で並ぶ (Req 5.6: 50 → 30 → 20)
//	  - LoadedRows == 100 / Truncated == false (100 < MaxFailureRowsLoaded=10000)
//	  - 各クラスタの NormalizedErrorKey が normalizeError 適用済み文字列で揃う (Req 5.2)
//	  - RepresentativeRunIDs が SampleSize (default 5) 件以下 (Req 5.4)
//	を返すことを検証する。
//
// 包括的なテスト (期間 / workflow_id / provider_id フィルタ / 代表 run の DESC 順 / raw_events
// subtype キー含み) はタスク 4.5 で reader_test.go に追加される想定。
func TestSQLiteRecorderFailureClustersGroupsByKey(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	// クラスタ A: 50 件、provider=claude / failure_category=provider_error
	insertFailureClusterFixture(t, ctx, recorder, "A", "provider_error", "claude", "Connection refused", 50, base)
	// クラスタ B: 30 件、provider=codex / failure_category=external_abort (異なるキー)
	insertFailureClusterFixture(t, ctx, recorder, "B", "external_abort", "codex", "User aborted", 30, base.Add(time.Hour))
	// クラスタ C: 20 件、A と同 (failure_category, provider) だが error が違うのでクラスタは別
	insertFailureClusterFixture(t, ctx, recorder, "C", "provider_error", "claude", "Rate limit exceeded", 20, base.Add(2*time.Hour))

	result, err := recorder.FailureClusters(ctx, FailureOptions{})
	if err != nil {
		t.Fatalf("FailureClusters: %v", err)
	}

	if got := len(result.Clusters); got != 3 {
		t.Fatalf("len(Clusters) = %d, want 3 (3 つの (failure_category, provider, error) キー)", got)
	}

	// Req 5.6: Count DESC 並び (50 → 30 → 20)。
	wantCounts := []int64{50, 30, 20}
	for i, want := range wantCounts {
		if got := result.Clusters[i].Count; got != want {
			t.Errorf("Clusters[%d].Count = %d, want %d (Count DESC ソートが崩れている)", i, got, want)
		}
	}

	// LoadedRows == 100 / Truncated == false (MaxFailureRowsLoaded=10000 のデフォルト下では未打ち切り)。
	if result.LoadedRows != 100 {
		t.Errorf("LoadedRows = %d, want 100", result.LoadedRows)
	}
	if result.Truncated {
		t.Errorf("Truncated = true, want false (LoadedRows=100 < MaxFailureRowsLoaded=%d)", MaxFailureRowsLoaded)
	}

	// 各クラスタの NormalizedErrorKey は normalizeError 適用済みであること (Req 5.2)。
	// 入力 error は "Connection refused" / "User aborted" / "Rate limit exceeded" で
	// 数字 / UUID / パス / 連続空白を含まないため、normalizeError 適用後も同一文字列が返る。
	for _, c := range result.Clusters {
		if c.NormalizedErrorKey == "" {
			t.Errorf("Cluster %+v: NormalizedErrorKey is empty (normalizeError 適用漏れ?)", c)
		}
	}

	// RepresentativeRunIDs は SampleSize (default=5) 件まで (Req 5.4)。fixture では
	// クラスタ内で同一 workflow_run_id を共有するため、5 件すべて同じ run-A / run-B / run-C で
	// 詰められる (但し件数は SampleSize クランプを超えないことを検証)。
	for _, c := range result.Clusters {
		if got := len(c.RepresentativeRunIDs); got > DefaultFailureSampleSize {
			t.Errorf("Cluster %s.RepresentativeRunIDs len = %d, want <= %d", c.NormalizedErrorKey, got, DefaultFailureSampleSize)
		}
	}
}

// TestSQLiteRecorderFailureClustersTruncatedWhenLoadCapped はタスク 2.5 の観測可能な
// 完了条件 (truncate 経路) を担保する smoke test:
//
//	失敗 agent_call 15 件を 1 クラスタに集中させ、MaxFailureRowsLoaded を一時的に 10 に
//	絞った状態で FailureClusters を呼ぶと
//	  - result.LoadedRows == 10 (SQL LIMIT で切り詰められる、Req 5.8)
//	  - result.Truncated == true (LoadedRows == MaxFailureRowsLoaded のとき)
//	を返すことを検証する。
//
// テスト終了時に MaxFailureRowsLoaded を元の値に戻す (他テストへの干渉を防ぐ)。
func TestSQLiteRecorderFailureClustersTruncatedWhenLoadCapped(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	// MaxFailureRowsLoaded を一時的に 10 に上書き。defer で必ず元の値を復元。
	saved := MaxFailureRowsLoaded
	MaxFailureRowsLoaded = 10
	defer func() { MaxFailureRowsLoaded = saved }()

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	// 1 クラスタに 15 件投入 (MaxFailureRowsLoaded=10 を超える数)。
	insertFailureClusterFixture(t, ctx, recorder, "T", "provider_error", "claude", "Connection refused", 15, base)

	result, err := recorder.FailureClusters(ctx, FailureOptions{})
	if err != nil {
		t.Fatalf("FailureClusters: %v", err)
	}

	if result.LoadedRows != 10 {
		t.Errorf("LoadedRows = %d, want 10 (MaxFailureRowsLoaded で SQL LIMIT 切り詰め)", result.LoadedRows)
	}
	if !result.Truncated {
		t.Errorf("Truncated = false, want true (LoadedRows == MaxFailureRowsLoaded=10、Req 5.8)")
	}
	// クラスタは 1 つで、Count は 10 件 (ロード打ち切りの母数)。
	if got := len(result.Clusters); got != 1 {
		t.Fatalf("len(Clusters) = %d, want 1", got)
	}
	if got := result.Clusters[0].Count; got != 10 {
		t.Errorf("Clusters[0].Count = %d, want 10 (打ち切り後の母数)", got)
	}
}

// TestSQLiteRecorderFailureClustersValidationRejectsBadOptions は FailureOptions の
// 負値 / 上限超過の Limit / SampleSize を sentinel error で拒否することを検証する
// (Req 7.4: 極端 limit を内部上限でクランプかエラー、本実装はエラー)。
func TestSQLiteRecorderFailureClustersValidationRejectsBadOptions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	cases := []struct {
		name    string
		opts    FailureOptions
		wantErr error
	}{
		{"negative limit", FailureOptions{Limit: -1}, ErrInvalidLimit},
		{"limit over max", FailureOptions{Limit: MaxFailureClusterLimit + 1}, ErrInvalidLimit},
		{"negative sample size", FailureOptions{SampleSize: -1}, ErrInvalidLimit},
		{"sample size over max", FailureOptions{SampleSize: MaxFailureSampleSize + 1}, ErrInvalidLimit},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := recorder.FailureClusters(ctx, c.opts)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("FailureClusters(%+v) error = %v, want errors.Is(%v)", c.opts, err, c.wantErr)
			}
		})
	}
}

// TestSQLiteRecorderFailureClustersUserCorrectionCountSumsPerCluster はタスク 5.1 の
// 観測可能な完了条件を担保する smoke test:
//
//	failure cluster fixture で user_corrected = 1 を 2 件付与した状態で
//	FailureClusters が
//	  - cluster.Count == 5 (集計母数は不変)
//	  - cluster.UserCorrectionCount == 2 (SUM(user_corrected) と一致、Req 4.1)
//	を返すことを検証する。
//
// fixture は同一 (failure_category, provider_id, error) で 5 件、うち先頭 2 件に対し
// RecordUserCorrection を呼び user_corrected 列を 1 に立てる。
func TestSQLiteRecorderFailureClustersUserCorrectionCountSumsPerCluster(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	// 1 cluster に 5 件の失敗 agent_call を投入する。
	insertFailureClusterFixture(t, ctx, recorder, "UC", "provider_error", "claude", "Connection refused", 5, base)

	// うち 2 件 (先頭 2 つ) に対して user-correction marker を立てる。
	for i := range 2 {
		callID := fmt.Sprintf("ac-UC-%d", i)
		if err := recorder.RecordUserCorrection(ctx, callID); err != nil {
			t.Fatalf("RecordUserCorrection %s: %v", callID, err)
		}
	}

	result, err := recorder.FailureClusters(ctx, FailureOptions{})
	if err != nil {
		t.Fatalf("FailureClusters: %v", err)
	}
	if got := len(result.Clusters); got != 1 {
		t.Fatalf("len(Clusters) = %d, want 1", got)
	}
	if got := result.Clusters[0].Count; got != 5 {
		t.Errorf("Clusters[0].Count = %d, want 5 (集計母数は user_corrected と直交)", got)
	}
	if got := result.Clusters[0].UserCorrectionCount; got != 2 {
		t.Errorf("Clusters[0].UserCorrectionCount = %d, want 2 (SUM(user_corrected) と一致 / Req 4.1)", got)
	}
}

// TestSQLiteRecorderFailureClustersUserCorrectionCountLegacyRowReadsAsZero はタスク 5.1 の
// 観測可能な完了条件 (legacy row 安全 / Req 6.3) を担保する smoke test:
//
//	user_corrected を一切立てない fixture (= migration 直後の legacy 行と等価) で
//	FailureClusters が
//	  - cluster.UserCorrectionCount == 0 (COALESCE(user_corrected, 0) で legacy 行が安全に読める)
//	を返すことを検証する。
//
// SQL 上では user_corrected は NOT NULL DEFAULT 0 だが、`COALESCE(user_corrected, 0)` で
// 防御的に NULL → 0 へ正規化する経路を test レベルで担保する。
func TestSQLiteRecorderFailureClustersUserCorrectionCountLegacyRowReadsAsZero(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	// user_corrected を立てない fixture を 3 件投入する。
	insertFailureClusterFixture(t, ctx, recorder, "L", "provider_error", "claude", "Connection refused", 3, base)

	result, err := recorder.FailureClusters(ctx, FailureOptions{})
	if err != nil {
		t.Fatalf("FailureClusters: %v", err)
	}
	if got := len(result.Clusters); got != 1 {
		t.Fatalf("len(Clusters) = %d, want 1", got)
	}
	if got := result.Clusters[0].UserCorrectionCount; got != 0 {
		t.Errorf("Clusters[0].UserCorrectionCount = %d, want 0 (未マーク fixture / legacy row 安全)", got)
	}
}

// TestSQLiteRecorderFailureClustersEmptyReturnsNilClusters は失敗 agent_call が 1 件もない
// 状況で Clusters == nil / LoadedRows == 0 / Truncated == false が返ることを検証する。
func TestSQLiteRecorderFailureClustersEmptyReturnsNilClusters(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	result, err := recorder.FailureClusters(ctx, FailureOptions{})
	if err != nil {
		t.Fatalf("FailureClusters: %v", err)
	}
	if result.Clusters != nil {
		t.Errorf("Clusters = %v, want nil (失敗データなし)", result.Clusters)
	}
	if result.LoadedRows != 0 {
		t.Errorf("LoadedRows = %d, want 0", result.LoadedRows)
	}
	if result.Truncated {
		t.Errorf("Truncated = true, want false (LoadedRows=0)")
	}
}
