// Package akashic の reader_sqlite_detail_test.go は、reader_sqlite_detail.go (Detail 系)
// の TDD discipline を担保する companion test。
//
// 各タスクの「観測可能な完了条件」を最小限の smoke test として担保し、
// 包括的な table-driven 検証はタスク 4.4 で reader_test.go に追加される想定。
package akashic

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// insertDetailFixture はタスク 2.4 の観測可能な完了条件を担保するための fixture を投入する
// 共通ヘルパ。
//
// 投入内容:
//   - 1 workflow_run (finished_at NOT NULL / error NULL → Success=true)
//   - 3 agent_calls (started_at が秒単位で昇順: ac-1 < ac-2 < ac-3)
//   - 2 context_usage (raw_events の source=claude-control / type=context_usage 経由で
//     auto-project される。agent_call_id は ac-1 / ac-2 を使用)
//   - 計 250 件の raw_events (上記 2 件の context_usage + 248 件の "provider"/"event")
//
// auto-projection の前提 (store.go projectRawEvent):
//   - AgentCallID 非空、Source=="claude-control"、Type=="context_usage"、PayloadTruncated=false、
//     PayloadJSON が valid JSON である必要がある
func insertDetailFixture(
	t *testing.T,
	ctx context.Context,
	recorder *SQLiteRecorder,
	runID string,
	base time.Time,
) {
	t.Helper()

	// 1. workflow_run (Success=true: finished_at NOT NULL かつ error NULL)
	if err := recorder.RecordWorkflowRunStart(ctx, WorkflowRun{
		ID:           runID,
		WorkflowID:   "wf",
		WorkflowName: "WF",
		CWD:          "/repo",
		StartedAt:    base,
	}); err != nil {
		t.Fatalf("RecordWorkflowRunStart: %v", err)
	}
	if err := recorder.RecordWorkflowRunFinish(ctx, WorkflowRunFinish{
		ID:         runID,
		FinishedAt: base.Add(time.Minute),
	}); err != nil {
		t.Fatalf("RecordWorkflowRunFinish: %v", err)
	}

	// 2. 3 agent_calls (started_at ASC: ac-1 < ac-2 < ac-3)
	for i := 1; i <= 3; i++ {
		callID := fmt.Sprintf("ac-%d", i)
		if err := recorder.RecordAgentCallStart(ctx, AgentCall{
			ID:            callID,
			WorkflowRunID: runID,
			WorkflowID:    "wf",
			WorkflowName:  "WF",
			StepID:        "s1",
			StepName:      "S1",
			ProviderID:    "claude",
			CWD:           "/repo",
			StartedAt:     base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("RecordAgentCallStart %s: %v", callID, err)
		}
	}

	// 3. context_usage 用の raw_events 2 件 (auto-project されて agent_context_usage に
	//    2 行が作られる)。Sequence は raw_events 全体で 1〜250 の連番にする。
	contextPayload := []byte(`{"totalTokens":100,"maxTokens":200,"rawMaxTokens":200,"percentage":50}`)
	for i := 1; i <= 2; i++ {
		if err := recorder.RecordRawEvent(ctx, RawEvent{
			AgentCallID:   fmt.Sprintf("ac-%d", i),
			WorkflowRunID: runID,
			Sequence:      int64(i),
			Source:        "claude-control",
			Type:          "context_usage",
			ObservedAt:    base.Add(time.Duration(i) * time.Second),
			PayloadJSON:   contextPayload,
		}); err != nil {
			t.Fatalf("RecordRawEvent context_usage %d: %v", i, err)
		}
	}

	// 4. 248 件の追加 raw_events (auto-project の対象外: source!=claude-control)
	for i := 3; i <= 250; i++ {
		if err := recorder.RecordRawEvent(ctx, RawEvent{
			AgentCallID:   "ac-1",
			WorkflowRunID: runID,
			Sequence:      int64(i),
			Source:        "provider",
			Type:          "event",
			ObservedAt:    base.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("RecordRawEvent %d: %v", i, err)
		}
	}
}

// TestSQLiteRecorderDetailReturnsRunWithSubresources はタスク 2.4 の観測可能な完了条件を
// 担保する smoke test:
//
//	1 run + 3 agent_calls + 2 context_usage + 250 raw_events の fixture で
//	Detail(ctx, DetailOptions{WorkflowRunID: id}) が
//	  - len(RawEvents) == 100 (DefaultRawEventsLimit でクランプ、Req 4.8 / 7.1)
//	  - *NextRawEventsOffset == 100 (len==Limit のとき次オフセット、Req 4.4)
//	  - len(AgentCalls) == 3 で started_at ASC 順 (Req 4.1 / 4.6)
//	  - len(ContextUsage) == 2 (Req 4.2)
//	  - Run.RunID / Run.Success が正しく組み立てられる (Req 4.1)
//	を返すことを検証する。
//
// 包括的なテストはタスク 4.4 で reader_test.go に追加される想定。
func TestSQLiteRecorderDetailReturnsRunWithSubresources(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	runID := "run-detail"
	insertDetailFixture(t, ctx, recorder, runID, base)

	detail, err := recorder.Detail(ctx, DetailOptions{WorkflowRunID: runID})
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}

	// Run header (Req 4.1)
	if detail.Run.RunID != runID {
		t.Fatalf("Run.RunID = %q, want %q", detail.Run.RunID, runID)
	}
	if detail.Run.WorkflowID != "wf" {
		t.Errorf("Run.WorkflowID = %q, want wf", detail.Run.WorkflowID)
	}
	// finished_at NOT NULL かつ error NULL → Success=&true (Req 3.1 派生定義と整合)
	if detail.Run.Success == nil || !*detail.Run.Success {
		t.Errorf("Run.Success = %v, want &true (finished_at NOT NULL かつ error NULL)", detail.Run.Success)
	}

	// 3 agent_calls (started_at ASC: ac-1 → ac-2 → ac-3、Req 4.6)
	if got := len(detail.AgentCalls); got != 3 {
		t.Fatalf("len(AgentCalls) = %d, want 3", got)
	}
	for i, want := range []string{"ac-1", "ac-2", "ac-3"} {
		if detail.AgentCalls[i].ID != want {
			t.Errorf("AgentCalls[%d].ID = %q, want %q (started_at ASC)", i, detail.AgentCalls[i].ID, want)
		}
	}
	// AgentCallRow の必須フィールド (provider_id / cwd / step_id 等) が scan 経由で取得できる
	if detail.AgentCalls[0].ProviderID != "claude" {
		t.Errorf("AgentCalls[0].ProviderID = %q, want claude", detail.AgentCalls[0].ProviderID)
	}

	// 2 context_usage (Req 4.2)
	if got := len(detail.ContextUsage); got != 2 {
		t.Errorf("len(ContextUsage) = %d, want 2", got)
	}

	// 250 raw_events → デフォルト limit 100 でクランプ (Req 4.8 / 7.1)
	if got := len(detail.RawEvents); got != 100 {
		t.Errorf("len(RawEvents) = %d, want 100 (DefaultRawEventsLimit クランプ)", got)
	}
	if detail.RawEventsLimit != DefaultRawEventsLimit {
		t.Errorf("RawEventsLimit = %d, want %d", detail.RawEventsLimit, DefaultRawEventsLimit)
	}
	if detail.RawEventsOffset != 0 {
		t.Errorf("RawEventsOffset = %d, want 0", detail.RawEventsOffset)
	}
	if detail.NextRawEventsOffset == nil {
		t.Fatalf("NextRawEventsOffset = nil, want &100 (len(RawEvents)==Limit のとき次オフセット)")
	}
	if *detail.NextRawEventsOffset != 100 {
		t.Errorf("*NextRawEventsOffset = %d, want 100", *detail.NextRawEventsOffset)
	}

	// raw_events は sequence ASC, id ASC 順で並ぶ (Req 4.6)
	for i := 1; i < len(detail.RawEvents); i++ {
		if detail.RawEvents[i-1].Sequence > detail.RawEvents[i].Sequence {
			t.Errorf("RawEvents[%d].Sequence (%d) > RawEvents[%d].Sequence (%d) — sequence ASC が崩れている",
				i-1, detail.RawEvents[i-1].Sequence, i, detail.RawEvents[i].Sequence)
		}
	}
}

// TestSQLiteRecorderDetailNotFound は存在しない workflow_run_id を指定したとき
// ErrWorkflowRunNotFound を返すことを検証する (Req 4.7)。
func TestSQLiteRecorderDetailNotFound(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	_, err = recorder.Detail(ctx, DetailOptions{WorkflowRunID: "missing-run"})
	if !errors.Is(err, ErrWorkflowRunNotFound) {
		t.Fatalf("Detail(missing-run) error = %v, want errors.Is(ErrWorkflowRunNotFound)", err)
	}
}

// TestSQLiteRecorderDetailValidationRejectsBadOptions は RawEventsQuery の
// 負値 limit / 上限超過 limit / 負 offset を sentinel error で拒否することを検証する
// (Req 7.4: 極端 limit を内部上限でクランプかエラー、本実装はエラー)。
func TestSQLiteRecorderDetailValidationRejectsBadOptions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	cases := []struct {
		name    string
		opts    DetailOptions
		wantErr error
	}{
		{
			name: "raw events negative limit",
			opts: DetailOptions{
				WorkflowRunID: "any",
				RawEvents:     RawEventsQuery{Limit: -1},
			},
			wantErr: ErrInvalidLimit,
		},
		{
			name: "raw events limit over max",
			opts: DetailOptions{
				WorkflowRunID: "any",
				RawEvents:     RawEventsQuery{Limit: MaxRawEventsLimit + 1},
			},
			wantErr: ErrInvalidLimit,
		},
		{
			name: "raw events negative offset",
			opts: DetailOptions{
				WorkflowRunID: "any",
				RawEvents:     RawEventsQuery{Offset: -1},
			},
			wantErr: ErrInvalidOffset,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := recorder.Detail(ctx, c.opts)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("Detail(%+v) error = %v, want errors.Is(%v)", c.opts, err, c.wantErr)
			}
		})
	}
}

// TestSQLiteRecorderDetailRawEventsTypeFilter は raw_events の type フィルタが
// 該当タイプのみを返すことを検証する (Req 4.5)。
//
// fixture: 248 件の type="event" + 2 件の type="context_usage" のうち、
// type="event" 指定で 100 件にクランプされ、Type 列が全件 "event" であることを確認する。
func TestSQLiteRecorderDetailRawEventsTypeFilter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer recorder.Close()

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	runID := "run-detail-type"
	insertDetailFixture(t, ctx, recorder, runID, base)

	detail, err := recorder.Detail(ctx, DetailOptions{
		WorkflowRunID: runID,
		RawEvents:     RawEventsQuery{Type: "event"},
	})
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	// type="event" 248 件 → DefaultRawEventsLimit=100 でクランプ
	if got := len(detail.RawEvents); got != 100 {
		t.Errorf("len(RawEvents) = %d, want 100 (type=event 248 件を limit クランプ)", got)
	}
	for i, ev := range detail.RawEvents {
		if ev.Type != "event" {
			t.Errorf("RawEvents[%d].Type = %q, want %q (Type フィルタ漏れ)", i, ev.Type, "event")
		}
	}
}
