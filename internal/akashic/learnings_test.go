// Package akashic の learnings_test.go は agent-learnings spec Task 3.2 / Req 3.1, 3.6,
// 5.1, 5.2, 5.3, 5.5, 5.6, 9.6 の単体テスト群。
//
// 検証対象:
//   - SQLiteRecorder.RecordLearning の round-trip (Req 5.1, 5.2)
//   - RecordLearning の category enum 違反 reject (Req 5.3)
//   - RecordLearning の paths_glob > 5 entries reject (task 3.2 観測可能完了)
//   - SQLiteRecorder.LearningsByWorkflowRun の安定順序読み取り (Req 5.1)
//   - SQLiteRecorder.AgentCallsByWorkflowRun の workflow_run_id 絞り込み + project_id 保持 (Req 3.1, 3.6)
package akashic

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// newTestRecorder は in-memory ではなく t.TempDir() 配下に SQLite ファイルを作って
// SQLiteRecorder を返す test helper (modernc.org/sqlite は :memory: より file の方が
// schema migration を含む動作確認に適している)。
func newTestRecorder(t *testing.T) *SQLiteRecorder {
	t.Helper()
	path := filepath.Join(t.TempDir(), "learnings.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	return recorder
}

// TestRecordLearningRoundTrip は agent-learnings Req 5.1 の核心:
// RecordLearning で insert した行が LearningsByWorkflowRun で完全に読み戻せることを
// 全 column について照合する (paths_glob は JSON encode/decode の round-trip も込み)。
func TestRecordLearningRoundTrip(t *testing.T) {
	ctx := context.Background()
	recorder := newTestRecorder(t)

	now := time.Date(2026, 5, 8, 12, 30, 0, 0, time.UTC)
	row := LearningRow{
		ID:            "lrn-1",
		AgentCallID:   "ac-1",
		WorkflowRunID: "run-1",
		WorkflowID:    "kiro-impl-loop",
		StepID:        "implement",
		Category:      CategoryFailureRecovery,
		Context:       "test context",
		Insight:       "test insight",
		PathsGlob:     []string{"internal/akashic/**", "cmd/rilll/**"},
		Source:        SourceAgentSelfReport,
		RecordedAt:    now,
	}

	if err := recorder.RecordLearning(ctx, row); err != nil {
		t.Fatalf("RecordLearning: %v", err)
	}

	got, err := recorder.LearningsByWorkflowRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("LearningsByWorkflowRun: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got)=%d want=1 (got=%v)", len(got), got)
	}

	want := row
	// SQLite は RFC3339Nano で保存し parseStoredTime で UTC time.Time に戻すため
	// 入力値もすでに UTC で揃えてある。
	want.RecordedAt = now.UTC()

	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("LearningRow mismatch:\n got=%+v\nwant=%+v", got[0], want)
	}
}

func TestRecordLearningOnceTreatsDuplicateIDAsAlreadyInserted(t *testing.T) {
	ctx := context.Background()
	recorder := newTestRecorder(t)
	now := time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC)

	row := LearningRow{
		ID:            "learning-deterministic-1",
		AgentCallID:   "call-early-1",
		WorkflowRunID: "run-early",
		WorkflowID:    "kiro-impl-loop",
		StepID:        "reflect",
		Category:      CategoryFailureRecovery,
		Context:       "deterministic context",
		Insight:       "insert once",
		PathsGlob:     []string{"internal/akashic/**"},
		Source:        SourceReflectorDerived,
		RecordedAt:    now,
	}

	inserted, err := recorder.RecordLearningOnce(ctx, row)
	if err != nil {
		t.Fatalf("RecordLearningOnce first: %v", err)
	}
	if !inserted {
		t.Fatal("RecordLearningOnce first inserted=false, want true")
	}

	duplicate := row
	duplicate.WorkflowRunID = "run-overwrite"
	duplicate.AgentCallID = "call-overwrite"
	duplicate.Context = "overwritten context"
	duplicate.Insight = "overwritten insight"
	duplicate.PathsGlob = []string{"overwritten/**"}
	duplicate.RecordedAt = now.Add(time.Minute)

	inserted, err = recorder.RecordLearningOnce(ctx, duplicate)
	if err != nil {
		t.Fatalf("RecordLearningOnce duplicate: %v", err)
	}
	if inserted {
		t.Fatal("RecordLearningOnce duplicate inserted=true, want false")
	}

	got, err := recorder.LearningsByWorkflowRun(ctx, "run-early")
	if err != nil {
		t.Fatalf("LearningsByWorkflowRun run-early: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("run-early learnings len=%d want=1 (got=%v)", len(got), got)
	}
	want := row
	want.RecordedAt = now.UTC()
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("duplicate changed stored row:\n got=%+v\nwant=%+v", got[0], want)
	}

	overwritten, err := recorder.LearningsByWorkflowRun(ctx, "run-overwrite")
	if err != nil {
		t.Fatalf("LearningsByWorkflowRun run-overwrite: %v", err)
	}
	if len(overwritten) != 0 {
		t.Fatalf("duplicate conflict created overwrite row: %+v", overwritten)
	}
}

func TestRecordLearningDuplicateIDRemainsFatalAppendOnly(t *testing.T) {
	ctx := context.Background()
	recorder := newTestRecorder(t)
	row := LearningRow{
		ID:            "learning-append-only-duplicate",
		AgentCallID:   "call-legacy",
		WorkflowRunID: "run-legacy",
		WorkflowID:    "kiro-impl-loop",
		StepID:        "reflect",
		Category:      CategorySuccessProcedure,
		Context:       "legacy context",
		Insight:       "legacy stays append-only",
		PathsGlob:     []string{"internal/workflow/**"},
		Source:        SourceAgentSelfReport,
		RecordedAt:    time.Date(2026, 5, 24, 10, 5, 0, 0, time.UTC),
	}

	if err := recorder.RecordLearning(ctx, row); err != nil {
		t.Fatalf("RecordLearning first: %v", err)
	}
	if err := recorder.RecordLearning(ctx, row); err == nil {
		t.Fatal("RecordLearning duplicate err=nil, want primary-key conflict")
	}

	got, err := recorder.LearningsByWorkflowRun(ctx, "run-legacy")
	if err != nil {
		t.Fatalf("LearningsByWorkflowRun: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("RecordLearning duplicate row count=%d want=1", len(got))
	}
}

// TestRecordLearningRejectsInvalidCategory は agent-learnings Req 5.3 の核心:
// closed enum 4 値 (failure-recovery / success-procedure / antipattern / tool-pattern)
// 以外の category は ErrInvalidLearningCategory で reject され、行が DB に書き込まれない。
func TestRecordLearningRejectsInvalidCategory(t *testing.T) {
	ctx := context.Background()
	recorder := newTestRecorder(t)

	row := LearningRow{
		ID:            "lrn-bad-cat",
		AgentCallID:   "ac-1",
		WorkflowRunID: "run-1",
		WorkflowID:    "wf",
		StepID:        "step",
		Category:      "unknown-category",
		Context:       "ctx",
		Insight:       "ins",
		PathsGlob:     []string{"a"},
		Source:        SourceAgentSelfReport,
		RecordedAt:    time.Now().UTC(),
	}

	err := recorder.RecordLearning(ctx, row)
	if !errors.Is(err, ErrInvalidLearningCategory) {
		t.Fatalf("RecordLearning err: got=%v want=ErrInvalidLearningCategory", err)
	}

	got, err := recorder.LearningsByWorkflowRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("LearningsByWorkflowRun: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("invalid category 行が DB に書かれてはならない: got=%v", got)
	}
}

// TestRecordLearningRejectsTooManyPathsGlob は task 3.2 の観測可能完了条件:
// paths_glob > 5 entries は ErrTooManyPathsGlob で reject される。
func TestRecordLearningRejectsTooManyPathsGlob(t *testing.T) {
	ctx := context.Background()
	recorder := newTestRecorder(t)

	row := LearningRow{
		ID:            "lrn-bad-paths",
		AgentCallID:   "ac-1",
		WorkflowRunID: "run-1",
		WorkflowID:    "wf",
		StepID:        "step",
		Category:      CategoryToolPattern,
		Context:       "ctx",
		Insight:       "ins",
		// 6 entries — MaxPathsGlobEntries = 5 を超える
		PathsGlob:  []string{"a", "b", "c", "d", "e", "f"},
		Source:     SourceAgentSelfReport,
		RecordedAt: time.Now().UTC(),
	}

	err := recorder.RecordLearning(ctx, row)
	if !errors.Is(err, ErrTooManyPathsGlob) {
		t.Fatalf("RecordLearning err: got=%v want=ErrTooManyPathsGlob", err)
	}

	got, err := recorder.LearningsByWorkflowRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("LearningsByWorkflowRun: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("paths_glob 上限超過行が DB に書かれてはならない: got=%v", got)
	}
}

// TestRecordLearningAcceptsAllValidCategories は Req 5.3 の closed enum 4 値が
// すべて accept されることを確認する (regression alarm)。
func TestRecordLearningAcceptsAllValidCategories(t *testing.T) {
	ctx := context.Background()
	recorder := newTestRecorder(t)

	categories := []string{
		CategoryFailureRecovery,
		CategorySuccessProcedure,
		CategoryAntipattern,
		CategoryToolPattern,
	}
	for i, cat := range categories {
		row := LearningRow{
			ID:            "lrn-" + cat,
			AgentCallID:   "ac-1",
			WorkflowRunID: "run-cat",
			WorkflowID:    "wf",
			StepID:        "step",
			Category:      cat,
			Context:       "ctx",
			Insight:       "ins",
			PathsGlob:     []string{"a"},
			Source:        SourceAgentSelfReport,
			// 同 run_id への同時 insert で順序が安定するように offset を付ける
			RecordedAt: time.Date(2026, 5, 8, 0, 0, i, 0, time.UTC),
		}
		if err := recorder.RecordLearning(ctx, row); err != nil {
			t.Fatalf("RecordLearning(%s): %v", cat, err)
		}
	}

	got, err := recorder.LearningsByWorkflowRun(ctx, "run-cat")
	if err != nil {
		t.Fatalf("LearningsByWorkflowRun: %v", err)
	}
	if len(got) != len(categories) {
		t.Fatalf("len(got)=%d want=%d", len(got), len(categories))
	}
}

// TestRecordLearningEmptyPathsGlob は paths_glob == nil / 空 slice でも reject されず
// JSON encode で `[]` として保存され、読み戻し時に空 slice として復元できることを確認する。
// Apply phase が paths_glob 空を「validation で別途 reject」する Req 7.4 とは別レイヤー
// (Recorder は最低限の structural 検証のみ)。
func TestRecordLearningEmptyPathsGlob(t *testing.T) {
	ctx := context.Background()
	recorder := newTestRecorder(t)

	row := LearningRow{
		ID:            "lrn-empty-paths",
		AgentCallID:   "ac-1",
		WorkflowRunID: "run-empty",
		WorkflowID:    "wf",
		StepID:        "step",
		Category:      CategoryAntipattern,
		Context:       "ctx",
		Insight:       "ins",
		PathsGlob:     nil,
		Source:        SourceAgentSelfReport,
		RecordedAt:    time.Now().UTC(),
	}
	if err := recorder.RecordLearning(ctx, row); err != nil {
		t.Fatalf("RecordLearning: %v", err)
	}

	got, err := recorder.LearningsByWorkflowRun(ctx, "run-empty")
	if err != nil {
		t.Fatalf("LearningsByWorkflowRun: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got)=%d want=1", len(got))
	}
	if len(got[0].PathsGlob) != 0 {
		t.Fatalf("PathsGlob: got=%v want=空 slice", got[0].PathsGlob)
	}
}

// TestRecordLearningExactlyFivePathsGlob は MaxPathsGlobEntries = 5 (上限ぎりぎり) は
// accept されることを確認する境界値テスト。
func TestRecordLearningExactlyFivePathsGlob(t *testing.T) {
	ctx := context.Background()
	recorder := newTestRecorder(t)

	row := LearningRow{
		ID:            "lrn-five",
		AgentCallID:   "ac-1",
		WorkflowRunID: "run-five",
		WorkflowID:    "wf",
		StepID:        "step",
		Category:      CategoryToolPattern,
		Context:       "ctx",
		Insight:       "ins",
		PathsGlob:     []string{"a", "b", "c", "d", "e"},
		Source:        SourceAgentSelfReport,
		RecordedAt:    time.Now().UTC(),
	}
	if err := recorder.RecordLearning(ctx, row); err != nil {
		t.Fatalf("RecordLearning(5 entries): %v", err)
	}
}

// TestLearningsByWorkflowRunReturnsEmptyForUnknownRunID は未知 run_id への問い合わせで
// 空 slice + nil error を返すことを確認する (Detail のような sentinel error 経路ではない)。
func TestLearningsByWorkflowRunReturnsEmptyForUnknownRunID(t *testing.T) {
	ctx := context.Background()
	recorder := newTestRecorder(t)

	got, err := recorder.LearningsByWorkflowRun(ctx, "unknown-run")
	if err != nil {
		t.Fatalf("LearningsByWorkflowRun: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got=%v want=空 slice", got)
	}
}

// TestLearningsByWorkflowRunFiltersByRunID は別 workflow_run_id の learning 行が
// 混在しても、指定 workflow_run_id の行のみが返ることを確認する (Req 5.1)。
func TestLearningsByWorkflowRunFiltersByRunID(t *testing.T) {
	ctx := context.Background()
	recorder := newTestRecorder(t)

	mkRow := func(id, runID string) LearningRow {
		return LearningRow{
			ID:            id,
			AgentCallID:   "ac-x",
			WorkflowRunID: runID,
			WorkflowID:    "wf",
			StepID:        "step",
			Category:      CategoryFailureRecovery,
			Context:       "ctx",
			Insight:       "ins",
			PathsGlob:     []string{"a"},
			Source:        SourceAgentSelfReport,
			RecordedAt:    time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC),
		}
	}
	for _, r := range []LearningRow{
		mkRow("l-A1", "run-A"),
		mkRow("l-A2", "run-A"),
		mkRow("l-B1", "run-B"),
	} {
		if err := recorder.RecordLearning(ctx, r); err != nil {
			t.Fatalf("RecordLearning(%s): %v", r.ID, err)
		}
	}

	got, err := recorder.LearningsByWorkflowRun(ctx, "run-A")
	if err != nil {
		t.Fatalf("LearningsByWorkflowRun: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("run-A の learnings: got len=%d want=2 (got=%v)", len(got), got)
	}
	for _, r := range got {
		if r.WorkflowRunID != "run-A" {
			t.Errorf("混入 row: %+v", r)
		}
	}
}

// TestAgentCallsByWorkflowRunReturnsAllForRun は AgentCallsByWorkflowRun が指定
// workflow_run_id の agent_calls をすべて返し、project_id を populate することを確認する
// (Req 3.1, 3.6 の素材)。
func TestAgentCallsByWorkflowRunReturnsAllForRun(t *testing.T) {
	ctx := context.Background()
	recorder := newTestRecorder(t)

	// 同 run に 2 行 + 別 run に 1 行 insert
	calls := []AgentCall{
		{
			ID:            "ac-1",
			WorkflowRunID: "run-X",
			WorkflowID:    "wf",
			StepID:        "step1",
			ProviderID:    "claude",
			ProjectID:     "rilll",
			StartedAt:     time.Date(2026, 5, 8, 10, 0, 0, 0, time.UTC),
		},
		{
			ID:            "ac-2",
			WorkflowRunID: "run-X",
			WorkflowID:    "wf",
			StepID:        "step2",
			ProviderID:    "codex",
			ProjectID:     "rilll",
			StartedAt:     time.Date(2026, 5, 8, 10, 0, 1, 0, time.UTC),
		},
		{
			ID:            "ac-3",
			WorkflowRunID: "run-Y",
			WorkflowID:    "wf",
			StepID:        "step1",
			ProviderID:    "claude",
			ProjectID:     "rilll",
			StartedAt:     time.Date(2026, 5, 8, 10, 0, 2, 0, time.UTC),
		},
	}
	for _, c := range calls {
		if err := recorder.RecordAgentCallStart(ctx, c); err != nil {
			t.Fatalf("RecordAgentCallStart(%s): %v", c.ID, err)
		}
	}
	// session_id を埋めるため finish も発行
	for _, c := range calls {
		if err := recorder.RecordAgentCallFinish(ctx, AgentCallResult{
			ID:         c.ID,
			SessionID:  "sess-" + c.ID,
			Success:    true,
			DurationMS: 100,
			FinishedAt: c.StartedAt.Add(time.Second),
		}); err != nil {
			t.Fatalf("RecordAgentCallFinish(%s): %v", c.ID, err)
		}
	}

	got, err := recorder.AgentCallsByWorkflowRun(ctx, "run-X")
	if err != nil {
		t.Fatalf("AgentCallsByWorkflowRun: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got)=%d want=2 (got=%v)", len(got), got)
	}
	// started_at ASC, id ASC で安定ソート
	if got[0].ID != "ac-1" || got[1].ID != "ac-2" {
		t.Fatalf("順序が不正: got=[%s, %s] want=[ac-1, ac-2]", got[0].ID, got[1].ID)
	}
	for _, r := range got {
		if r.WorkflowRunID != "run-X" {
			t.Errorf("混入 row: %+v", r)
		}
		if r.ProjectID != "rilll" {
			t.Errorf("ProjectID が populate されていない: row=%+v", r)
		}
		if r.SessionID != "sess-"+r.ID {
			t.Errorf("SessionID round-trip 失敗: row=%+v", r)
		}
	}
}

// TestAgentCallsByWorkflowRunEmpty は未知 run_id への問い合わせで空 slice + nil error を返す。
func TestAgentCallsByWorkflowRunEmpty(t *testing.T) {
	ctx := context.Background()
	recorder := newTestRecorder(t)

	got, err := recorder.AgentCallsByWorkflowRun(ctx, "unknown-run")
	if err != nil {
		t.Fatalf("AgentCallsByWorkflowRun: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got=%v want=空 slice", got)
	}
}

// TestValidLearningCategory は ValidLearningCategory が closed enum 4 値以外を reject する
// 単体テスト (Recorder validation の根拠)。
func TestValidLearningCategory(t *testing.T) {
	for _, valid := range []string{
		CategoryFailureRecovery,
		CategorySuccessProcedure,
		CategoryAntipattern,
		CategoryToolPattern,
	} {
		if !ValidLearningCategory(valid) {
			t.Errorf("ValidLearningCategory(%q) = false, want true", valid)
		}
	}
	for _, invalid := range []string{
		"",
		"failure",
		"failure_recovery",
		"FailureRecovery",
		"unknown",
		"failure-recovery ", // trailing space
	} {
		if ValidLearningCategory(invalid) {
			t.Errorf("ValidLearningCategory(%q) = true, want false", invalid)
		}
	}
}
