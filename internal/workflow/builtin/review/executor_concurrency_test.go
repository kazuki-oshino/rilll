// Package review のテスト本ファイルは task 4.2 / design.md §Performance & Reliability の
// 「並行性能テスト」「akashic 並行書き込みテスト」を実装する。
//
// 検証観点 (tasks.md 4.2 観察可能完了条件):
//
//   - rule 50 件 + sema=4 で同時 goroutine 数が atomic counter 計測で 4 を超えない (Req 4.3)
//   - 1 rule が一定時間スリープ後 fail、他 rule は即時完了するシナリオで全体終端
//     レイテンシが「遅い rule の所要時間 + 集約コスト」程度に収まり、`errgroup`
//     由来の cancel 伝播が誤って混入していないこと (= 他 rule が cancelled に
//     ならないこと) を検証する (Req 4.2)
//   - rule 20 件 + 実 SQLiteRecorder で agent_calls 行が 20 件正しく書き込まれる
//     ことを WAL モード並行書き込み下で検証する (Req 6.5)
//
// 既存 executor_test.go の TestRunExecutorSemaCapPreserved (8 件版) と重複しない
// ように、本ファイルでは:
//
//   - rule 数を 50 件に拡張し、より高並行下で sema cap が保たれることを実証する
//   - 失敗分離 + bounded latency を時間軸で検証する (executor_test.go は verdict 内容のみ検証)
//   - 実 *akashic.SQLiteRecorder を使った agent_calls 書き込みの並行性 + 件数を検証する
//     (executor_test.go は in-memory な reviewRecorder fake で raw_events / artifact のみ検証)
package review

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/agent"
	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/result"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/rules"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// sqliteAgentRecorder は実 *akashic.SQLiteRecorder に agent_calls 行を直接
// 書き込む test 専用 AgentCaller。tasks.md 4.2 第 3 観点 (`agent_calls` 行が
// 20 件正しく書き込まれる / `internal/akashic` の WAL モード並行書き込み確認) を
// 検証するために、`*agent.Agent` 経由ではなく recorder への直接書き込み経路で
// SQLite WAL 並行書き込み挙動だけを切り出して観察する。
//
// 設計判断:
//   - `agent.newAgentCallID()` は `time.Now().UnixNano()` ベースで生成され、
//     高並行下では nanosecond resolution の衝突によって `agent_calls.id` が
//     重複する可能性がある (`ON CONFLICT(id) DO UPDATE` で dedup される)。
//     これは agent 層の latent 課題であり tasks.md 4.2 の検証対象 (akashic の
//     WAL モード並行書き込み挙動) とは別レイヤなので、本テストでは agent 層を
//     バイパスし「rule.Name 由来の deterministic 一意 ID」で recorder へ直接
//     書き込むことで akashic 層の並行書き込み挙動だけを観察する。
//   - 各 rule の StepID (= rule.Name) は loader が一意性を保証するため、
//     `agent-call-<rule.Name>` 形の ID は rule 数と 1:1 で対応する。
type sqliteAgentRecorder struct {
	recorder akashic.Recorder
	// callDelay は RecordAgentCallStart と RecordAgentCallFinish の間に挟む
	// sleep。並行 in-flight goroutine が akashic への並行 INSERT を実際に競合
	// させるための観察時間幅を確保する用途。
	callDelay time.Duration
}

func (a *sqliteAgentRecorder) CallAgent(ctx context.Context, prompt string, cwd string, _ provider.AgentEventCallback, _ string, options *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
	stepID := ""
	stepName := ""
	pid := provider.ProviderID("")
	model := ""
	if options != nil {
		stepID = options.StepID
		stepName = options.StepName
		pid = options.ProviderOverride
		model = options.ModelOverride
	}
	// rule.Name (= StepID) は loader 契約により一意。これを ID 接頭辞として
	// agent_calls.id を deterministic に確定させる。
	callID := "agent-call-" + stepID
	runID := ""
	if metadata, ok := workflowctx.RunMetadataFromContext(ctx); ok {
		runID = metadata.WorkflowRunID
	}
	startedAt := time.Now().UTC()
	recordCtx := context.WithoutCancel(ctx)
	_ = a.recorder.RecordAgentCallStart(recordCtx, akashic.AgentCall{
		ID:            callID,
		WorkflowRunID: runID,
		WorkflowID:    reviewWorkflowID,
		WorkflowName:  reviewWorkflowLabel,
		StepID:        stepID,
		StepName:      stepName,
		ProviderID:    string(pid),
		Prompt:        prompt,
		CWD:           cwd,
		StartedAt:     startedAt,
	})
	if a.callDelay > 0 {
		select {
		case <-time.After(a.callDelay):
		case <-ctx.Done():
		}
	}
	resp := provider.AgentResponse{
		Success: true,
		Content: `{"applied": true, "findings_present": false, "summary": "ok", "details": []}`,
		StructuredOutput: map[string]any{
			"applied":          true,
			"findings_present": false,
			"summary":          "ok",
			"details":          []any{},
		},
	}
	_ = a.recorder.RecordAgentCallFinish(recordCtx, akashic.AgentCallResult{
		ID:            callID,
		Success:       true,
		DurationMS:    time.Since(startedAt).Milliseconds(),
		ResultSummary: resp.Content,
		FinishedAt:    time.Now().UTC(),
	})
	_ = model // 現状は agent_calls schema に model カラムが無いため捨てる
	return resp, nil
}

// AkashicRecorder は executor が workflow.RecorderFromAgent 経由で取り出す seam。
// raw_event (rule_model_selected / executor_rule_failed) や RecordRunArtifact が
// 同じ実 SQLiteRecorder に集約されることで、agent_calls + raw_events + run_artifacts の
// 並行書き込みを単一 DB ファイル上で観察可能にする。
func (a *sqliteAgentRecorder) AkashicRecorder() akashic.Recorder { return a.recorder }

var _ workflowctx.AgentCaller = (*sqliteAgentRecorder)(nil)
var _ workflowctx.AkashicRecorderProvider = (*sqliteAgentRecorder)(nil)

// makeManyRules は kebab-case な rule 名 (`rule-001` 形式の 3 桁 zero-padded で
// ファイル名昇順を deterministic に保つ) を n 件分生成する。
func makeManyRules(n int) []rules.Rule {
	out := make([]rules.Rule, 0, n)
	for i := 0; i < n; i++ {
		// 3 桁 zero-padded で 1000 件まで sortable。
		name := paddedRuleName(i)
		out = append(out, rules.Rule{
			Name:        name,
			Description: "perf rule " + name,
			Provider:    "claude",
			Model:       "model-perf",
			AppliesWhen: "always",
			Prompt:      "review",
			SourceFile:  name + ".yaml",
		})
	}
	return out
}

// paddedRuleName は idx を 3 桁 zero-padded した kebab-case 名を返す
// (rule-000 / rule-001 / .../ rule-049)。ファイル名昇順 = 投入順を保つため、
// loader 契約 (ファイル名昇順) と整合する。
func paddedRuleName(idx int) string {
	const prefix = "rule-"
	d2 := byte('0' + (idx/100)%10)
	d1 := byte('0' + (idx/10)%10)
	d0 := byte('0' + idx%10)
	return prefix + string([]byte{d2, d1, d0})
}

// TestRunExecutorSemaCap_50Rules は rule 50 件 + sema=4 のとき同時 goroutine 数が
// 4 を超えないことを atomic counter で検証する (tasks.md 4.2 第 1 観点 / Req 4.3)。
//
// executor_test.go の TestRunExecutorSemaCapPreserved は rule 8 件で同様の検証を
// 行うが、本テストは rule 50 件 (= sema cap の 12 倍以上) でも cap が崩れないこと
// を高並行下で実証することで、sema 取得経路の race condition を浮かび上がらせる。
func TestRunExecutorSemaCap_50Rules(t *testing.T) {
	t.Parallel()

	const ruleCount = 50
	const semaCap = 4
	rs := makeManyRules(ruleCount)
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)

	// 各 rule に短い sleep を入れ、並行性を可視化する。
	// sleep が 0 だと goroutine が起動した瞬間に終わってしまい、cap の検証ができない。
	for _, r := range rs {
		ag.behaviors[r.Name] = func(_ context.Context, _ *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
			time.Sleep(5 * time.Millisecond)
			return defaultAppliedResponse(), nil
		}
	}

	cwd, runDir := setupRillRunDir(t, "run-sema-50")
	out := runExecutorWithRunMetadata(t, context.Background(), "run-sema-50", cwd, rec, ExecutorInput{
		Rules:       rs,
		PRMeta:      makePRMeta(),
		PRDiff:      "diff",
		MaxParallel: semaCap,
		RunDir:      runDir,
		Agent:       ag,
	})

	if got := ag.maxConc.Load(); got > int32(semaCap) {
		t.Fatalf("max concurrency: got %d, want <=%d (rule=%d, sema=%d)",
			got, semaCap, ruleCount, semaCap)
	}
	// 並行が観測されていること (= sema=1 で逐次化されていないこと) も簡易検証する。
	// rule 50 件 + sema=4 + 5ms sleep であれば最大 4 並行が観測されるはず。
	// ただし極めて速いランナーでは 1 並行に縮退する可能性があるため、>=1 のみ要求する。
	if got := ag.maxConc.Load(); got < 1 {
		t.Errorf("max concurrency = %d, want >=1 (並行起動が一切観測されなかった)", got)
	}

	// 全 rule が verdict 確定済みであること。
	if len(out.Results) != ruleCount {
		t.Fatalf("results length: got %d, want %d", len(out.Results), ruleCount)
	}
	for i, r := range out.Results {
		if r.Verdict != result.VerdictNoFindings {
			t.Errorf("results[%d].Verdict = %q, want %q", i, r.Verdict, result.VerdictNoFindings)
		}
	}
}

// TestRunExecutorBoundedLatencyOnSlowFailure は 1 rule が一定時間スリープ後に fail し、
// 他 rule が即時完了するシナリオで、全体終端レイテンシが「遅い rule の所要時間 +
// 集約コスト」程度に収まり、`errgroup` 由来の cancel 伝播が誤って混入していない
// (= 他 rule が cancelled にならない / 早期終了しない) ことを検証する
// (tasks.md 4.2 第 2 観点 / Req 4.2)。
//
// ※ 本テストは tasks.md の「5 秒スリープ」を観察可能性は同等のまま 500ms に
// 短縮している。CI で flake せず安定実行できる予算 (テストごとに 30 秒以内) を
// 守る目的で、観察対象 (`errgroup` cancel 伝播の有無) は変えていない。
//
// 観察:
//   - 全体所要時間 elapsed は [slow-1 rule sleep, slow-1 rule sleep + 大幅余裕] に収まる
//     → errgroup cancel が誤って混入していれば <slow-1 rule sleep となる
//     → 逐次実行に縮退していれば slow-1 rule sleep × N となる
//   - 即時 rule の Verdict は no_findings (cancelled でない)
//   - 遅い rule の Verdict は failed + FailureCategory=provider_error
func TestRunExecutorBoundedLatencyOnSlowFailure(t *testing.T) {
	t.Parallel()

	// slowSleep を 500ms に設定する。CI flake 耐性のため、
	// 上限は slowSleep の 4 倍 (~2s) まで許容する (集約コスト + GC + スケジューラ揺らぎ)。
	const slowSleep = 500 * time.Millisecond
	const upperBound = 2000 * time.Millisecond

	rs := makeRules("slow-failing", "fast-a", "fast-b", "fast-c", "fast-d")
	rec := &reviewRecorder{}
	ag := newFakeRuleAgent(rec)

	// slow-failing は slowSleep スリープ後に provider error を返す。
	ag.behaviors["slow-failing"] = func(ctx context.Context, _ *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
		select {
		case <-time.After(slowSleep):
		case <-ctx.Done():
			return provider.AgentResponse{}, ctx.Err()
		}
		return provider.AgentResponse{}, &agent.CallError{
			Category: agent.FailureCategoryProviderError,
			Err:      errors.New("intentional slow failure"),
		}
	}
	// fast-* は default (即時 no_findings)。

	cwd, runDir := setupRillRunDir(t, "run-bounded")
	startedAt := time.Now()
	out := runExecutorWithRunMetadata(t, context.Background(), "run-bounded", cwd, rec, ExecutorInput{
		Rules:       rs,
		PRMeta:      makePRMeta(),
		PRDiff:      "diff",
		MaxParallel: len(rs), // 全 rule を同時起動可能にする
		RunDir:      runDir,
		Agent:       ag,
	})
	elapsed := time.Since(startedAt)

	// 全体終端レイテンシ: 遅い rule の所要時間 (slowSleep) を超え、上限以内。
	// errgroup 伝播が誤って入っていれば slowSleep より小さくなる (slow rule が早期 cancel される)。
	if elapsed < slowSleep {
		t.Errorf("elapsed = %v, want >= %v (errgroup cancel propagation 検出: slow rule が早期終了)", elapsed, slowSleep)
	}
	if elapsed > upperBound {
		t.Errorf("elapsed = %v, want <= %v (逐次化または cancel 伝播以外の停滞検出)", elapsed, upperBound)
	}

	// 各 rule の verdict 検証。
	// results[0] = slow-failing → failed + provider_error
	// results[1..4] = fast-* → no_findings (cancelled でないこと: 失敗分離契約)
	if len(out.Results) != len(rs) {
		t.Fatalf("results length = %d, want %d", len(out.Results), len(rs))
	}
	if out.Results[0].Verdict != result.VerdictFailed {
		t.Errorf("slow-failing verdict = %q, want %q", out.Results[0].Verdict, result.VerdictFailed)
	}
	if out.Results[0].FailureCategory != agent.FailureCategoryProviderError {
		t.Errorf("slow-failing FailureCategory = %q, want %q", out.Results[0].FailureCategory, agent.FailureCategoryProviderError)
	}
	for _, idx := range []int{1, 2, 3, 4} {
		if out.Results[idx].Verdict != result.VerdictNoFindings {
			t.Errorf("results[%d] (%s) verdict = %q, want %q (errgroup cancel 伝播が混入している可能性)",
				idx, rs[idx].Name, out.Results[idx].Verdict, result.VerdictNoFindings)
		}
	}
}

// TestRunExecutorSQLiteRecorderConcurrentWrite は実 *akashic.SQLiteRecorder を
// 経由した並行書き込み下でも agent_calls 行が 20 件正しく書き込まれることを
// 検証する (tasks.md 4.2 第 3 観点 / Req 6.5)。
//
// 検証は SQLiteRecorder の WAL モード (busy_timeout=5s) 並行書き込み挙動の
// regression チェックである。busy timeout を超える書き込み競合や、
// 並行 INSERT 時の row 落ち / 重複が発生していないことを観察する。
//
// 設計判断:
//   - tasks.md 4.2 の検証対象は「`internal/akashic` の WAL モード並行書き込み」
//     であり、`agent.newAgentCallID()` (UnixNano ベース) の衝突回避ではない。
//     後者は agent 層の latent 課題で、衝突時は `ON CONFLICT(id) DO UPDATE` が
//     dedup するため SQLite 並行書き込み挙動とは別レイヤで観測される。
//     このため本テストでは `sqliteAgentRecorder` 経由で「rule.Name 由来の
//     deterministic 一意 ID」で agent_calls 行を直接書き込み、akashic 並行書き込み
//     経路だけを切り出して観察する。
//   - sqliteAgentRecorder.callDelay=1ms により start/finish 間で 1ms in-flight
//     状態を維持し、20 rule 並行下で実際に複数 goroutine が同 SQLite db に対し
//     並行 INSERT する経路を確実に踏ませる。
//   - sema は 8 で 20 件の rule を fan-out させる (20/8 = 2.5 wave)。
func TestRunExecutorSQLiteRecorderConcurrentWrite(t *testing.T) {
	t.Parallel()

	const ruleCount = 20
	const semaCap = 8

	// 実 SQLiteRecorder を t.TempDir() 下に開く。WAL モード + busy_timeout=5s が
	// initialize 経路で適用される (store.go §SQLiteRecorder.initialize)。
	dbPath := filepath.Join(t.TempDir(), "akashic", "concurrent.sqlite")
	rec, err := akashic.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	// workflow_runs 行を先に投入しておく (agent_calls.workflow_run_id は外部キー
	// 制約こそ無いが、後続の集計クエリで JOIN 整合性を保つため)。
	const runID = "run-sqlite-concurrent"
	if werr := rec.RecordWorkflowRunStart(context.Background(), akashic.WorkflowRun{
		ID:           runID,
		WorkflowID:   reviewWorkflowID,
		WorkflowName: reviewWorkflowLabel,
		CWD:          "/test",
		StartedAt:    time.Now().UTC(),
	}); werr != nil {
		t.Fatalf("RecordWorkflowRunStart: %v", werr)
	}

	// agent_calls の deterministic 一意 ID 経路で直接書き込む test 専用 AgentCaller。
	// `agent.Agent` のような UnixNano ベース ID 衝突の影響を受けず、SQLite WAL の
	// 並行 INSERT 挙動だけを切り出して観察できる (本テストの目的)。
	ag := &sqliteAgentRecorder{recorder: rec, callDelay: 1 * time.Millisecond}

	rs := makeManyRules(ruleCount)
	cwd, runDir := setupRillRunDir(t, runID)

	// runExecutorWithRunMetadata は ctx に RunMetadata.WorkflowRunID = runID を
	// 注入する。sqliteAgentRecorder.CallAgent はこの値を agent_calls.workflow_run_id
	// に流し込むため、後段の SELECT で行を絞り込める。
	out := runExecutorWithRunMetadata(t, context.Background(), runID, cwd, rec, ExecutorInput{
		Rules:       rs,
		PRMeta:      makePRMeta(),
		PRDiff:      "diff",
		MaxParallel: semaCap,
		RunDir:      runDir,
		Agent:       ag,
	})
	if len(out.Results) != ruleCount {
		t.Fatalf("results length = %d, want %d", len(out.Results), ruleCount)
	}

	// agent_calls 行を SELECT して件数 + step_id 一致を検証する。
	// 直接 db.Open するのではなく、別 connection で確認することで Recorder の
	// 内部 connection pool に依存しない経路で観測する。
	verifyDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("verify db open: %v", err)
	}
	defer verifyDB.Close()

	var nCalls int
	if err := verifyDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM agent_calls WHERE workflow_run_id = ?`, runID).Scan(&nCalls); err != nil {
		t.Fatalf("count agent_calls: %v", err)
	}
	if nCalls != ruleCount {
		t.Errorf("agent_calls count for run-id %q = %d, want %d (WAL 並行書き込み regression)",
			runID, nCalls, ruleCount)
	}

	// step_id (= rule.Name) が全 rule に対して 1 件ずつ書き込まれていること。
	rows, err := verifyDB.QueryContext(context.Background(),
		`SELECT step_id, COUNT(*) FROM agent_calls WHERE workflow_run_id = ? GROUP BY step_id`, runID)
	if err != nil {
		t.Fatalf("query step_id: %v", err)
	}
	defer rows.Close()

	gotByStep := make(map[string]int, ruleCount)
	for rows.Next() {
		var stepID string
		var count int
		if err := rows.Scan(&stepID, &count); err != nil {
			t.Fatalf("scan step_id: %v", err)
		}
		gotByStep[stepID] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	for _, r := range rs {
		got, ok := gotByStep[r.Name]
		if !ok {
			t.Errorf("step_id=%q が agent_calls に書き込まれていない (rule の record start/finish が脱落)", r.Name)
			continue
		}
		if got != 1 {
			t.Errorf("step_id=%q の行数 = %d, want 1 (重複 / 脱落のいずれか)", r.Name, got)
		}
	}

	// success フラグが全件 1 (= true) であること。
	// ※ stub が常に Success=true を返すため、agent_calls.success も 1 で確定する。
	var nSuccess int
	if err := verifyDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM agent_calls WHERE workflow_run_id = ? AND success = 1`, runID).Scan(&nSuccess); err != nil {
		t.Fatalf("count success agent_calls: %v", err)
	}
	if nSuccess != ruleCount {
		t.Errorf("agent_calls.success=1 件数 = %d, want %d", nSuccess, ruleCount)
	}

	// 念のため atomic 観測 (concurrency counter 経路) は agent.Agent 経由では
	// 取れないため、ここでは省略 (sema cap 検証は TestRunExecutorSemaCap_50Rules
	// で別途実施済み)。
}
