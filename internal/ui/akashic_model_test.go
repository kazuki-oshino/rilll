// Package ui のアカシックモードに関する Model 層 (TUI シェル) のテスト。
//
// internal/ui/akashic/ サブパッケージ化 (issue #17) に伴い、State 単体で
// 観測可能な振る舞いは package akashic 配下のテストに残し、本ファイルでは
// `Model.Update` 経由で Phase ガード越し挙動 (HandoffStartedMsg / HandoffFailedMsg
// による Phase 遷移、stale msg 排除、再突入時の State 初期化、Reader/ArtifactReader
// nil graceful) を観測する。
package ui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	record "github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/config"
	"github.com/kazuki-oshino/rilll/internal/provider"
	uiakashic "github.com/kazuki-oshino/rilll/internal/ui/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflow"
)

// ---- HandoffStartedMsg / HandoffFailedMsg を Model.Update に流す経路 (Task 7.1 / Req 7.7 / 7.8) ----

func TestModelUpdateHandoffStartedMsgTransitionsToPhaseWorkflow(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	model.Phase = PhaseAkashic

	updated, cmd := model.Update(uiakashic.HandoffStartedMsg{
		Params: workflow.HandoffParams{
			TargetWorkflowID: "wf-target",
			RunIDs:           []string{"run-7"},
		},
	})
	got := updated.(Model)

	if got.Phase != PhaseWorkflow {
		t.Fatalf("expected Phase=PhaseWorkflow after HandoffStartedMsg (Req 7.8), got %q", got.Phase)
	}
	if cmd != nil {
		gotMsg := cmd()
		if _, isQuit := gotMsg.(tea.QuitMsg); isQuit {
			t.Fatal("Model.Update on HandoffStartedMsg must not return tea.Quit")
		}
	}
}

func TestModelUpdateHandoffFailedMsgKeepsPhaseAkashicAndSetsHandoffErr(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	model.Phase = PhaseAkashic
	model.Akashic = uiakashic.State{
		Subview: uiakashic.SubviewDetail,
		Detail: uiakashic.DetailState{
			RunID: "run-7",
			Focus: uiakashic.DetailFocusHeader,
		},
	}

	wantErr := errors.New("boom hand-off")
	params := workflow.HandoffParams{
		TargetWorkflowID: "wf-target",
		RunIDs:           []string{"run-7"},
	}

	updated, cmd := model.Update(uiakashic.HandoffFailedMsg{Params: params, Err: wantErr})
	got := updated.(Model)

	if got.Phase != PhaseAkashic {
		t.Fatalf("expected Phase=PhaseAkashic on hand-off failure (Req 7.7), got %q", got.Phase)
	}
	if !errors.Is(got.Akashic.Detail.HandoffErr, wantErr) {
		t.Fatalf("expected Akashic.Detail.HandoffErr=%v, got %v", wantErr, got.Akashic.Detail.HandoffErr)
	}
	if cmd != nil {
		gotMsg := cmd()
		if _, isQuit := gotMsg.(tea.QuitMsg); isQuit {
			t.Fatal("Model.Update on HandoffFailedMsg must not return tea.Quit")
		}
	}
}

func TestModelUpdateHandoffMsgsIgnoredOutsidePhaseAkashic(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	model.Phase = PhaseMenu

	params := workflow.HandoffParams{
		TargetWorkflowID: "wf-target",
		RunIDs:           []string{"run-7"},
	}

	updated, _ := model.Update(uiakashic.HandoffStartedMsg{Params: params})
	if got := updated.(Model); got.Phase != PhaseMenu {
		t.Fatalf("expected Phase unchanged (PhaseMenu) on stale HandoffStartedMsg, got %q", got.Phase)
	}

	updated, _ = model.Update(uiakashic.HandoffFailedMsg{Params: params, Err: errors.New("ignored")})
	got := updated.(Model)
	if got.Phase != PhaseMenu {
		t.Fatalf("expected Phase unchanged (PhaseMenu) on stale HandoffFailedMsg, got %q", got.Phase)
	}
	if got.Akashic.Detail.HandoffErr != nil {
		t.Fatalf("expected Akashic.Detail.HandoffErr unchanged on stale msg outside PhaseAkashic, got %v", got.Akashic.Detail.HandoffErr)
	}
}

// ---- akashic 関連 msg の Phase ガード越しディスパッチ (Task 8.2 / Req 9.4 / 10.1 / 10.3) ----

func TestModelUpdateAkashicMsgsIgnoredOutsidePhaseAkashic(t *testing.T) {
	cases := []struct {
		name string
		msg  tea.Msg
	}{
		{
			name: "ListRunsMsg",
			msg: uiakashic.ListRunsMsg{
				Result: record.ListResult{
					Items: []record.WorkflowRunListItem{
						{RunHeader: record.RunHeader{RunID: "stale-run"}},
					},
				},
			},
		},
		{
			name: "AggregateByWorkflowMsg",
			msg: uiakashic.AggregateByWorkflowMsg{
				Result: record.WorkflowAggregateResult{
					Buckets: []record.WorkflowAggregateBucket{
						{WorkflowID: "stale-wf"},
					},
				},
			},
		},
		{
			name: "AggregateByProviderMsg",
			msg: uiakashic.AggregateByProviderMsg{
				Result: record.ProviderAggregateResult{
					Buckets: []record.ProviderAggregateBucket{
						{ProviderID: "stale-pr"},
					},
				},
			},
		},
		{
			name: "RunDetailMsg",
			msg: uiakashic.RunDetailMsg{
				Detail: record.RunDetail{
					Run: record.RunHeader{RunID: "stale-run", StartedAt: time.Now()},
				},
			},
		},
		{
			name: "GetArtifactsMsg",
			msg: uiakashic.GetArtifactsMsg{
				Rows: []record.ArtifactRow{
					{WorkflowRunID: "stale", Path: "/stale/path"},
				},
			},
		},
		{
			name: "ReadArtifactMsg",
			msg: uiakashic.ReadArtifactMsg{
				Path: "/stale/path",
				Data: []byte("stale data"),
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			model := NewModel(Options{
				CWD:    "/repo",
				Config: config.Config{AgentProvider: provider.ProviderClaude},
			})
			if model.Phase != PhaseMenu {
				t.Fatalf("precondition: expected PhaseMenu, got %s", model.Phase)
			}

			before := uiakashic.State{
				Subview: uiakashic.SubviewList,
				List: uiakashic.ListState{
					Items: []record.WorkflowRunListItem{
						{RunHeader: record.RunHeader{RunID: "preserved-run"}},
					},
				},
			}
			model.Akashic = before

			updated, cmd := model.Update(c.msg)
			got := updated.(Model)

			if got.Phase != PhaseMenu {
				t.Fatalf("expected Phase=PhaseMenu unchanged on stale %s, got %s", c.name, got.Phase)
			}
			if len(got.Akashic.List.Items) != 1 || got.Akashic.List.Items[0].RunID != "preserved-run" {
				t.Fatalf("expected Akashic.List.Items unchanged on stale %s, got %#v", c.name, got.Akashic.List.Items)
			}
			if got.Akashic.Subview != uiakashic.SubviewList {
				t.Fatalf("expected Akashic.Subview=SubviewList unchanged on stale %s, got %q", c.name, got.Akashic.Subview)
			}
			if cmd != nil {
				m := cmd()
				if _, isQuit := m.(tea.QuitMsg); isQuit {
					t.Fatalf("Model.Update on stale %s must not return tea.Quit", c.name)
				}
			}
		})
	}
}

func TestModelUpdateAkashicMsgsDispatchedInPhaseAkashic(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	model.Phase = PhaseAkashic
	model.Akashic = uiakashic.State{
		Subview: uiakashic.SubviewList,
		Reader:  &fakeReader{},
	}
	model.Akashic.List.Loading = true

	msg := uiakashic.ListRunsMsg{
		Result: record.ListResult{
			Items: []record.WorkflowRunListItem{
				{RunHeader: record.RunHeader{RunID: "run-X"}},
			},
		},
	}
	updated, _ := model.Update(msg)
	got := updated.(Model)

	if got.Akashic.List.Loading {
		t.Fatalf("expected List.Loading=false after ListRunsMsg dispatch in PhaseAkashic, got true")
	}
	if len(got.Akashic.List.Items) != 1 || got.Akashic.List.Items[0].RunID != "run-X" {
		t.Fatalf("expected ListRunsMsg to populate Items in PhaseAkashic, got %#v", got.Akashic.List.Items)
	}
}

func TestModelUpdateAkashicMsgIgnoredAfterModeExit(t *testing.T) {
	reader := &fakeReader{
		listResult: record.ListResult{
			Items: []record.WorkflowRunListItem{
				{RunHeader: record.RunHeader{RunID: "stale-from-async-cmd"}},
			},
		},
	}
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
		Reader: reader,
	})

	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	model = updated.(Model)
	if model.Phase != PhaseAkashic {
		t.Fatalf("precondition: expected PhaseAkashic, got %s", model.Phase)
	}
	if cmd == nil {
		t.Fatal("precondition: expected non-nil Cmd from initial ListRunsCmd")
	}

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(Model)
	if model.Phase != PhaseMenu {
		t.Fatalf("precondition: expected PhaseMenu after esc, got %s", model.Phase)
	}

	beforeView := model.View()

	staleMsg := cmd()
	updated, _ = model.Update(staleMsg)
	got := updated.(Model)

	if got.Phase != PhaseMenu {
		t.Fatalf("expected Phase=PhaseMenu unchanged on stale ListRunsMsg, got %s", got.Phase)
	}
	if len(got.Akashic.List.Items) != 0 {
		t.Fatalf("expected Akashic.List.Items to remain empty (stale msg discarded), got %#v", got.Akashic.List.Items)
	}
	afterView := got.View()
	if !strings.Contains(afterView, "Workflows") {
		t.Fatalf("expected menu view unchanged after stale msg, got:\n%s", afterView)
	}
	_ = beforeView
}

// ---- PhaseAkashic 再突入で State が初期化される (Task 8.2 / Req 10.1 / 10.3) ----

func TestPhaseAkashicReentryProducesFreshState(t *testing.T) {
	reader := &fakeReader{
		listResult: record.ListResult{
			Items: []record.WorkflowRunListItem{
				{RunHeader: record.RunHeader{RunID: "from-prev-session"}},
			},
		},
	}
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
		Reader: reader,
	})

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	model = updated.(Model)
	updated, _ = model.Update(uiakashic.ListRunsMsg{Result: reader.listResult})
	model = updated.(Model)
	if len(model.Akashic.List.Items) != 1 {
		t.Fatalf("precondition: expected List.Items populated in first session, got %#v", model.Akashic.List.Items)
	}

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = updated.(Model)
	if model.Akashic.Subview != uiakashic.SubviewSummary {
		t.Fatalf("precondition: expected Subview=SubviewSummary after tab, got %q", model.Akashic.Subview)
	}

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(Model)
	if model.Phase != PhaseMenu {
		t.Fatalf("precondition: expected PhaseMenu after esc, got %s", model.Phase)
	}

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	model = updated.(Model)

	if model.Phase != PhaseAkashic {
		t.Fatalf("expected PhaseAkashic on re-entry, got %s", model.Phase)
	}
	if model.Akashic.Subview != uiakashic.SubviewList {
		t.Fatalf("expected Subview=SubviewList on re-entry (fresh state), got %q", model.Akashic.Subview)
	}
	if len(model.Akashic.List.Items) != 0 {
		t.Fatalf("expected List.Items=nil on re-entry (Items=%#v reset)", model.Akashic.List.Items)
	}
	if !model.Akashic.List.Loading {
		t.Fatalf("expected List.Loading=true on re-entry (initial ListRunsCmd dispatched)")
	}
}

func TestPhaseAkashicReentryWithoutReaderProducesFreshState(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = updated.(Model)
	if model.Akashic.Subview != uiakashic.SubviewSummary {
		t.Fatalf("precondition: expected SubviewSummary after tab, got %q", model.Akashic.Subview)
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	model = updated.(Model)

	if model.Akashic.Subview != uiakashic.SubviewList {
		t.Fatalf("expected Subview=SubviewList on re-entry (Reader=nil), got %q", model.Akashic.Subview)
	}
	if model.Akashic.List.Loading {
		t.Fatalf("expected List.Loading=false on re-entry when Reader=nil (graceful), got true")
	}
}
