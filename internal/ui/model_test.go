package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/kazuki-oshino/rilll/internal/bridge"
	"github.com/kazuki-oshino/rilll/internal/config"
	"github.com/kazuki-oshino/rilll/internal/provider"
	uiakashic "github.com/kazuki-oshino/rilll/internal/ui/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/kiro"
)

func TestMenuSelectsWorkflow(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
		Workflows: []workflow.Definition{{
			ID:    "test",
			Label: "Test Workflow",
			Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
				return nil
			},
		}},
	})

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(Model)
	if got.Phase != PhaseWorkflow {
		t.Fatalf("expected workflow phase, got %s", got.Phase)
	}
}

// TestMenuExcludesHiddenWorkflows は Definition.Hidden=true のワークフローが
// トップメニュー (renderMenu) と menuItems の双方から除外されることを観測する。
//
// 動機: improve-workflow は handoff 専用 (akashic Run 詳細から `i` キーで起動) で、
// トップメニューから引数なしで起動すると precondition_failed に落ちるため、
// メニュー項目としては露出させない契約 (Hidden bool) を導入した。
//
// hidden ではない別ワークフローはそのまま並ぶことを併せて観測する。
func TestMenuExcludesHiddenWorkflows(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
		Workflows: []workflow.Definition{
			{
				ID:    "visible-one",
				Label: "Visible One",
				Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
					return nil
				},
			},
			{
				ID:     "improve-workflow",
				Label:  "improve workflow",
				Hidden: true,
				Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
					return nil
				},
			},
			{
				ID:    "visible-two",
				Label: "Visible Two",
				Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
					return nil
				},
			},
		},
	})

	items := model.menuItems()
	if got, want := len(items), 2; got != want {
		t.Fatalf("expected %d menuItems (Hidden を除外), got %d", want, got)
	}
	for _, item := range items {
		if item.workflow.ID == "improve-workflow" {
			t.Fatalf("Hidden workflow が menuItems に残っている: %+v", item)
		}
	}

	view := model.View()
	if strings.Contains(view, "improve workflow") {
		t.Fatalf("Hidden workflow Label が renderMenu に出ている:\n%s", view)
	}
	for _, want := range []string{"Visible One", "Visible Two"} {
		if !strings.Contains(view, want) {
			t.Fatalf("非 Hidden の Label %q が renderMenu に出ていない:\n%s", want, view)
		}
	}
}

func TestMenuIncludesKiroSpecImplLoopSequenceWorkflow(t *testing.T) {
	sequence := kiro.KiroSpecImplLoopSequence(nil)
	model := NewModel(Options{
		CWD:       "/repo",
		Config:    config.Config{AgentProvider: provider.ProviderClaude},
		Workflows: []workflow.Definition{sequence},
	})

	items := model.menuItems()
	if got, want := len(items), 1; got != want {
		t.Fatalf("menuItems length = %d, want %d: %+v", got, want, items)
	}
	if items[0].workflow.ID != "kiro-spec-impl-loop-sequence" {
		t.Fatalf("menu workflow ID = %q, want %q", items[0].workflow.ID, "kiro-spec-impl-loop-sequence")
	}
	if items[0].label != sequence.Label {
		t.Fatalf("menu label = %q, want %q", items[0].label, sequence.Label)
	}

	view := model.View()
	if !strings.Contains(view, sequence.Label) {
		t.Fatalf("sequence workflow label is not rendered in menu:\n%s", view)
	}
}

func TestMenuShowsOnlyWorkflowsAndGlobalHelp(t *testing.T) {
	model := NewModel(Options{
		CWD:          "/repo",
		BuildVersion: "1.2.3",
		Config:       config.Config{AgentProvider: provider.ProviderClaude},
		Workflows: []workflow.Definition{{
			ID:    "test",
			Label: "Test Workflow",
			Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
				return nil
			},
		}},
	})

	view := model.View()
	for _, want := range []string{"rilll v1.2.3", "provider: claude", "Workflows", "Test Workflow", "s 設定", "q 終了"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected view to contain %q, got:\n%s", want, view)
		}
	}
	if strings.Contains(view, "エージェントプロバイダー設定") {
		t.Fatalf("provider settings should not be rendered as a workflow item:\n%s", view)
	}
}

func TestMenuShortcutOpensProviderSettings(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	got := updated.(Model)
	if got.Phase != PhaseProviderSettings {
		t.Fatalf("expected provider settings phase, got %s", got.Phase)
	}
}

func TestProviderSettingsCanChangeProvider(t *testing.T) {
	var saved config.Config
	model := NewModel(Options{
		CWD: "/repo",
		Config: config.Config{
			AgentProvider: provider.ProviderClaude,
			AkashicLevel:  config.AkashicSummary,
		},
		SaveConfig: func(cfg config.Config) error {
			saved = cfg
			return nil
		},
	})
	model.Phase = PhaseProviderSettings

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(Model)
	if got.Config.AgentProvider != provider.ProviderCursor {
		t.Fatalf("unexpected provider: %#v", got.Config)
	}
	if saved.AgentProvider != provider.ProviderCursor || saved.AkashicLevel != config.AkashicSummary {
		t.Fatalf("saved config did not preserve settings: %#v", saved)
	}
}

func TestProviderSettingsCanChangeObservabilityLevel(t *testing.T) {
	var saved config.Config
	model := NewModel(Options{
		CWD: "/repo",
		Config: config.Config{
			AgentProvider: provider.ProviderClaude,
			AkashicLevel:  config.AkashicSummary,
		},
		SaveConfig: func(cfg config.Config) error {
			saved = cfg
			return nil
		},
	})
	model.Phase = PhaseProviderSettings

	// 4 つの provider (claude/cursor/codex/cursor-sdk) を飛ばして
	// 1 つ目の akashic level (summary) からさらに 1 つ進めて "full" に合わせる
	for range len(providerOptions()) + 1 {
		updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
		model = updated.(Model)
	}
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(Model)
	if got.Config.AkashicLevel != config.AkashicFull {
		t.Fatalf("unexpected akashic level: %#v", got.Config)
	}
	if saved.AgentProvider != provider.ProviderClaude || saved.AkashicLevel != config.AkashicFull {
		t.Fatalf("saved config did not include akashic level: %#v", saved)
	}
}

func TestProviderSettingsEnterOnCodexProviderOpensModelSelection(t *testing.T) {
	saveCount := 0
	model := NewModel(Options{
		CWD: "/repo",
		Config: config.Config{
			AgentProvider: provider.ProviderClaude,
			AkashicLevel:  config.AkashicSummary,
		},
		SaveConfig: func(cfg config.Config) error {
			saveCount++
			return nil
		},
	})
	model.Phase = PhaseProviderSettings

	for range 2 {
		updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
		model = updated.(Model)
	}
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(Model)
	if got.Phase != PhaseCodexModelSettings {
		t.Fatalf("expected codex model settings phase, got %s", got.Phase)
	}
	if got.Config.AgentProvider != provider.ProviderClaude {
		t.Fatalf("Codex model selection should not commit provider before model Enter: %#v", got.Config)
	}
	if saveCount != 0 {
		t.Fatalf("Codex provider Enter should defer SaveConfig until model selection, got %d calls", saveCount)
	}
}

func TestCodexModelSettingsCanSelectLunaMaxPresetAndSave(t *testing.T) {
	var saved config.Config
	model := NewModel(Options{
		CWD: "/repo",
		Config: config.Config{
			AgentProvider: provider.ProviderCodex,
			AkashicLevel:  config.AkashicSummary,
			Codex:         config.CodexConfig{ModelPreset: config.CodexModelPresetGPT55XHigh},
		},
		SaveConfig: func(cfg config.Config) error {
			saved = cfg
			return nil
		},
	})
	model.Phase = PhaseCodexModelSettings

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(Model)
	if got.Phase != PhaseMenu {
		t.Fatalf("expected phase menu after codex model save, got %s", got.Phase)
	}
	if got.Config.AgentProvider != provider.ProviderCodex {
		t.Fatalf("expected codex provider to remain selected: %#v", got.Config)
	}
	if got.Config.Codex.ModelPreset != config.CodexModelPresetGPT6LunaMax {
		t.Fatalf("unexpected codex preset: %#v", got.Config.Codex)
	}
	if saved.AgentProvider != provider.ProviderCodex || saved.Codex.ModelPreset != config.CodexModelPresetGPT6LunaMax {
		t.Fatalf("saved config did not include codex provider + model preset: %#v", saved)
	}
}

func TestCodexModelSettingsViewShowsPresetChoices(t *testing.T) {
	model := NewModel(Options{
		CWD: "/repo",
		Config: config.Config{
			AgentProvider: provider.ProviderCodex,
			AkashicLevel:  config.AkashicSummary,
			Codex:         config.CodexConfig{ModelPreset: config.CodexModelPresetGPT55XHigh},
		},
	})
	model.Phase = PhaseCodexModelSettings

	view := model.View()
	if strings.Contains(view, "spark") {
		t.Fatalf("retired model remains in picker: %s", view)
	}
	for _, want := range []string{
		"Codex model",
		"gpt-5.6-sol / high",
		"gpt-5.6-sol / xhigh",
		"gpt-5.5 / xhigh",
		"gpt-6-luna / max",
		"現在",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected codex model view to contain %q, got:\n%s", want, view)
		}
	}
}

func TestCodexModelSettingsEscReturnsProviderSettings(t *testing.T) {
	model := NewModel(Options{
		CWD: "/repo",
		Config: config.Config{
			AgentProvider: provider.ProviderCodex,
			AkashicLevel:  config.AkashicSummary,
		},
	})
	model.Phase = PhaseCodexModelSettings

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got := updated.(Model)
	if got.Phase != PhaseProviderSettings {
		t.Fatalf("expected esc to return to provider settings, got %s", got.Phase)
	}
}

// Cursor SDK provider Enter は Codex と同形で model 選択画面に遷移し、
// provider commit と SaveConfig は model 選択の Enter まで遅延される。
func TestProviderSettingsEnterOnCursorSDKProviderOpensModelSelection(t *testing.T) {
	saveCount := 0
	model := NewModel(Options{
		CWD: "/repo",
		Config: config.Config{
			AgentProvider: provider.ProviderClaude,
			AkashicLevel:  config.AkashicSummary,
		},
		SaveConfig: func(cfg config.Config) error {
			saveCount++
			return nil
		},
	})
	model.Phase = PhaseProviderSettings

	// Cursor SDK は providerOptions の 4 つ目 (index 3)
	for range 3 {
		updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
		model = updated.(Model)
	}
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(Model)
	if got.Phase != PhaseCursorSDKModelSettings {
		t.Fatalf("expected cursor-sdk model settings phase, got %s", got.Phase)
	}
	if got.Config.AgentProvider != provider.ProviderClaude {
		t.Fatalf("Cursor SDK model selection should not commit provider before model Enter: %#v", got.Config)
	}
	if saveCount != 0 {
		t.Fatalf("Cursor SDK provider Enter should defer SaveConfig until model selection, got %d calls", saveCount)
	}
}

func TestCursorSDKModelSettingsCanSelectGPT55XHighAndSave(t *testing.T) {
	var saved config.Config
	model := NewModel(Options{
		CWD: "/repo",
		Config: config.Config{
			AgentProvider: provider.ProviderClaude,
			AkashicLevel:  config.AkashicSummary,
			CursorSDK:     config.CursorSDKConfig{ModelPreset: config.CursorSDKModelPresetComposer25},
		},
		SaveConfig: func(cfg config.Config) error {
			saved = cfg
			return nil
		},
	})
	model.Phase = PhaseCursorSDKModelSettings

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(Model)
	if got.Phase != PhaseMenu {
		t.Fatalf("expected phase menu after cursor-sdk model save, got %s", got.Phase)
	}
	if got.Config.AgentProvider != provider.ProviderCursorSDK {
		t.Fatalf("expected cursor-sdk provider to be committed: %#v", got.Config)
	}
	if got.Config.CursorSDK.ModelPreset != config.CursorSDKModelPresetGPT55XHigh {
		t.Fatalf("unexpected cursor-sdk preset: %#v", got.Config.CursorSDK)
	}
	if saved.AgentProvider != provider.ProviderCursorSDK || saved.CursorSDK.ModelPreset != config.CursorSDKModelPresetGPT55XHigh {
		t.Fatalf("saved config did not include cursor-sdk provider + model preset: %#v", saved)
	}
}

func TestCursorSDKModelSettingsViewShowsPresetChoices(t *testing.T) {
	model := NewModel(Options{
		CWD: "/repo",
		Config: config.Config{
			AgentProvider: provider.ProviderCursorSDK,
			AkashicLevel:  config.AkashicSummary,
			CursorSDK:     config.CursorSDKConfig{ModelPreset: config.CursorSDKModelPresetComposer25},
		},
	})
	model.Phase = PhaseCursorSDKModelSettings

	view := model.View()
	for _, want := range []string{"Cursor SDK model", "composer-2.5", "composer-2.5 / fast", "gpt-5.5 / xhigh", "grok-4.5 / fast / high", "gpt-5.6-luna-max", "gpt-5.6-sol / high", "現在"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected cursor-sdk model view to contain %q, got:\n%s", want, view)
		}
	}
}

func TestCursorSDKModelSettingsEscReturnsProviderSettings(t *testing.T) {
	model := NewModel(Options{
		CWD: "/repo",
		Config: config.Config{
			AgentProvider: provider.ProviderClaude,
			AkashicLevel:  config.AkashicSummary,
		},
	})
	model.Phase = PhaseCursorSDKModelSettings

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got := updated.(Model)
	if got.Phase != PhaseProviderSettings {
		t.Fatalf("expected esc to return to provider settings, got %s", got.Phase)
	}
}

func TestProviderSettingsViewShowsObservabilitySection(t *testing.T) {
	model := NewModel(Options{
		CWD: "/repo",
		Config: config.Config{
			AgentProvider: provider.ProviderClaude,
			AkashicLevel:  config.AkashicSummary,
		},
	})
	model.Phase = PhaseProviderSettings

	view := model.View()
	for _, want := range []string{"Agent provider", "アカシックレコード記録レベル", "summary", "おすすめ", "full", "debug"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected settings view to contain %q, got:\n%s", want, view)
		}
	}
}

// agent-learnings opt-in TUI 関連テスト群。
// 設定画面 (PhaseProviderSettings) に opt-in セクションを追加した経路を検証する。
// これらは「config.json の直接編集を不要にする」UI 経路 (m.Config.Workflows[id].DistillLearningsAfter
// を space 経由で toggle + SaveConfig) の機械的検査。

// noopOptInWorkflows は opt-in toggle テスト用の minimal な workflow definition を返す。
// 本物の Fn は呼ばれないので nil を許容する (workflow.Definition.Fn は Run 経路のみで触られる)。
func noopOptInWorkflows() []workflow.Definition {
	return []workflow.Definition{
		{ID: "wf-a", Label: "Workflow A"},
		{ID: "wf-b", Label: "Workflow B"},
	}
}

// TestProviderSettingsListsOptInRows は opt-in セクションに workflow が列挙されることを確認する。
func TestProviderSettingsListsOptInRows(t *testing.T) {
	model := NewModel(Options{
		CWD: "/repo",
		Config: config.Config{
			AgentProvider: provider.ProviderClaude,
			AkashicLevel:  config.AkashicSummary,
			Workflows: map[string]config.WorkflowConfig{
				"wf-a": {DistillLearningsAfter: true},
			},
		},
		Workflows: noopOptInWorkflows(),
	})
	model.Phase = PhaseProviderSettings

	view := model.View()
	for _, want := range []string{"agent-learnings 学習対象", "Workflow A", "Workflow B", "(wf-a)", "(wf-b)", "[✓]", "[ ]"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected opt-in section to contain %q, got:\n%s", want, view)
		}
	}
}

// TestProviderSettingsHidesHiddenWorkflowsFromOptIn は Hidden=true な def が opt-in に出ないことを確認する。
func TestProviderSettingsHidesHiddenWorkflowsFromOptIn(t *testing.T) {
	model := NewModel(Options{
		CWD: "/repo",
		Config: config.Config{
			AgentProvider: provider.ProviderClaude,
			AkashicLevel:  config.AkashicSummary,
		},
		Workflows: []workflow.Definition{
			{ID: "wf-visible", Label: "Visible"},
			{ID: "wf-hidden", Label: "Hidden Internal", Hidden: true},
		},
	})
	model.Phase = PhaseProviderSettings

	view := model.View()
	if !strings.Contains(view, "(wf-visible)") {
		t.Fatalf("expected opt-in to list visible workflow, got:\n%s", view)
	}
	if strings.Contains(view, "(wf-hidden)") {
		t.Fatalf("expected hidden workflow NOT to appear in opt-in section, got:\n%s", view)
	}
}

// TestProviderSettingsEnterOnOtelRowSavesEnabled は OpenTelemetry ON 選択が保存されることを確認する。
func TestProviderSettingsEnterOnOtelRowSavesEnabled(t *testing.T) {
	var saved config.Config
	model := NewModel(Options{
		CWD: "/repo",
		Config: config.Config{
			AgentProvider: provider.ProviderClaude,
			AkashicLevel:  config.AkashicSummary,
		},
		SaveConfig: func(cfg config.Config) error {
			saved = cfg
			return nil
		},
	})
	model.Phase = PhaseProviderSettings
	model.SettingsCursor = settingsOtelOffset() + 1 // ON

	updated, _ := model.handleEnter()
	got := updated.(Model)
	if !got.Config.OtelEnabled {
		t.Fatalf("expected otelEnabled=true after selecting ON, got %#v", got.Config)
	}
	if got.Phase != PhaseMenu {
		t.Fatalf("expected phase to return to menu, got %s", got.Phase)
	}
	if !saved.OtelEnabled {
		t.Fatalf("expected SaveConfig to persist otelEnabled=true, got %#v", saved)
	}
}

// TestProviderSettingsViewShowsOtelSection は OpenTelemetry 設定行が view に含まれることを確認する。
func TestProviderSettingsViewShowsOtelSection(t *testing.T) {
	model := NewModel(Options{
		CWD: "/repo",
		Config: config.Config{
			AgentProvider: provider.ProviderClaude,
			AkashicLevel:  config.AkashicSummary,
		},
	})
	model.Phase = PhaseProviderSettings

	view := model.View()
	if !strings.Contains(view, "OpenTelemetry") {
		t.Fatalf("expected view to include OpenTelemetry section, got:\n%s", view)
	}
}

// TestProviderSettingsSpaceTogglesOptIn は space で opt-in が反転 + SaveConfig 即発火を確認する。
func TestProviderSettingsSpaceTogglesOptIn(t *testing.T) {
	var saved config.Config
	saveCount := 0
	model := NewModel(Options{
		CWD: "/repo",
		Config: config.Config{
			AgentProvider: provider.ProviderClaude,
			AkashicLevel:  config.AkashicSummary,
		},
		Workflows: noopOptInWorkflows(),
		SaveConfig: func(cfg config.Config) error {
			saved = cfg
			saveCount++
			return nil
		},
	})
	model.Phase = PhaseProviderSettings
	// cursor を opt-in セクションの先頭 (wf-a) に合わせる。
	model.SettingsCursor = settingsOptInOffset()

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")})
	got := updated.(Model)
	if !got.Config.Workflows["wf-a"].DistillLearningsAfter {
		t.Fatalf("expected wf-a opt-in to be true after toggle, got %#v", got.Config.Workflows)
	}
	if got.Phase != PhaseProviderSettings {
		t.Fatalf("expected phase to remain on settings, got %s", got.Phase)
	}
	if saveCount != 1 {
		t.Fatalf("expected SaveConfig to be called exactly once, got %d", saveCount)
	}
	if !saved.Workflows["wf-a"].DistillLearningsAfter {
		t.Fatalf("expected SaveConfig to receive opt-in=true, got %#v", saved.Workflows)
	}
}

// TestProviderSettingsSpaceTogglesOptInTwiceReturnsToFalse は 2 回 toggle で false に戻ることを確認する。
func TestProviderSettingsSpaceTogglesOptInTwiceReturnsToFalse(t *testing.T) {
	model := NewModel(Options{
		CWD: "/repo",
		Config: config.Config{
			AgentProvider: provider.ProviderClaude,
			AkashicLevel:  config.AkashicSummary,
		},
		Workflows:  noopOptInWorkflows(),
		SaveConfig: func(cfg config.Config) error { return nil },
	})
	model.Phase = PhaseProviderSettings
	model.SettingsCursor = settingsOptInOffset()

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")})
	mid := updated.(Model)
	updated, _ = mid.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")})
	got := updated.(Model)
	if got.Config.Workflows["wf-a"].DistillLearningsAfter {
		t.Fatalf("expected wf-a opt-in to be false after double toggle, got %#v", got.Config.Workflows)
	}
}

// TestProviderSettingsEnterOnOptInRowIsNoOp は opt-in 行で Enter が PhaseMenu に飛ばないことを確認する。
func TestProviderSettingsEnterOnOptInRowIsNoOp(t *testing.T) {
	saveCount := 0
	model := NewModel(Options{
		CWD: "/repo",
		Config: config.Config{
			AgentProvider: provider.ProviderClaude,
			AkashicLevel:  config.AkashicSummary,
		},
		Workflows: noopOptInWorkflows(),
		SaveConfig: func(cfg config.Config) error {
			saveCount++
			return nil
		},
	})
	model.Phase = PhaseProviderSettings
	model.SettingsCursor = settingsOptInOffset()

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(Model)
	if got.Phase != PhaseProviderSettings {
		t.Fatalf("expected phase to remain on settings (Enter is no-op on opt-in row), got %s", got.Phase)
	}
	if saveCount != 0 {
		t.Fatalf("expected SaveConfig NOT to be called for opt-in Enter, got %d calls", saveCount)
	}
}

// TestProviderSettingsCursorTraversesAllSections は SettingsCursor が
// provider+level+opt-in の全行を上限としてクランプされることを確認する。
func TestProviderSettingsCursorTraversesAllSections(t *testing.T) {
	model := NewModel(Options{
		CWD: "/repo",
		Config: config.Config{
			AgentProvider: provider.ProviderClaude,
			AkashicLevel:  config.AkashicSummary,
		},
		Workflows: noopOptInWorkflows(),
	})
	model.Phase = PhaseProviderSettings

	totalRows := settingsOptInOffset() + 2 // 2 = noopOptInWorkflows の長さ
	// cursor 上限まで Down を送る (上限到達後に余分に送ってもクランプされる)。
	for range totalRows + 5 {
		updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
		model = updated.(Model)
	}
	want := totalRows - 1
	if model.SettingsCursor != want {
		t.Fatalf("expected SettingsCursor to clamp at %d (last opt-in row), got %d", want, model.SettingsCursor)
	}
}

// TestProviderSettingsViewShowsRestartHint は view に「次回起動時から反映」相当の注釈が含まれることを確認する。
func TestProviderSettingsViewShowsRestartHint(t *testing.T) {
	model := NewModel(Options{
		CWD: "/repo",
		Config: config.Config{
			AgentProvider: provider.ProviderClaude,
			AkashicLevel:  config.AkashicSummary,
		},
		Workflows: noopOptInWorkflows(),
	})
	model.Phase = PhaseProviderSettings

	view := model.View()
	if !strings.Contains(view, "次回起動") {
		t.Fatalf("expected view to mention restart-required hint, got:\n%s", view)
	}
	if !strings.Contains(view, "space toggle") {
		t.Fatalf("expected footer to mention space toggle, got:\n%s", view)
	}
}

// 修正方針: workflowEventMsg{WorkflowDone} 単体では Phase 遷移しない
// (子の WorkflowDone でも届くため)。Phase=PhaseDone 遷移と Result 維持は、
// 続いて送る workflowDoneMsg が一括で担当する (issue #9)。
func TestWorkflowDoneEventShowsResultScreen(t *testing.T) {
	resetPlayCompletionSoundSpy(t)

	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	model.Phase = PhaseWorkflow

	updated, _ := model.Update(workflowEventMsg{event: workflow.WorkflowEvent{
		Type:   workflow.WorkflowDone,
		Result: "done",
	}})
	mid := updated.(Model)
	if mid.Phase != PhaseWorkflow {
		t.Fatalf("expected phase to stay workflow until workflowDoneMsg, got %s", mid.Phase)
	}
	if len(mid.Results) != 1 || mid.Results[0].Body != "done" {
		t.Fatalf("expected Results to contain single 'done' entry from WorkflowDone event, got %#v", mid.Results)
	}

	updated, cmd := mid.Update(workflowDoneMsg{})
	if cmd != nil {
		t.Fatal("expected workflowDoneMsg to terminate the events pump")
	}
	got := updated.(Model)
	if got.Phase != PhaseDone {
		t.Fatalf("expected done phase after workflowDoneMsg, got %s", got.Phase)
	}
	if !strings.Contains(got.View(), "done") {
		t.Fatalf("expected result screen to include workflow result, got:\n%s", got.View())
	}
}

func TestDoneScreenShowsTotalElapsedTime(t *testing.T) {
	resetPlayCompletionSoundSpy(t)

	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	model.Phase = PhaseWorkflow
	// 65 秒前にワークフローを開始したことにする → 完了時点で total 01:05 になることを期待
	model.WorkflowStartedAt = time.Now().Add(-65 * time.Second)

	// 子の WorkflowDone event だけが先行しても Duration は確定しない (修正方針 3)。
	updated, _ := model.Update(workflowEventMsg{event: workflow.WorkflowEvent{
		Type:   workflow.WorkflowDone,
		Result: "done",
	}})
	mid := updated.(Model)
	if mid.WorkflowDuration != 0 {
		t.Fatalf("expected WorkflowDuration to remain unset until workflowDoneMsg, got %v", mid.WorkflowDuration)
	}

	updated, _ = mid.Update(workflowDoneMsg{})
	got := updated.(Model)
	if got.Phase != PhaseDone {
		t.Fatalf("expected done phase, got %s", got.Phase)
	}
	if got.WorkflowDuration <= 0 {
		t.Fatalf("expected WorkflowDuration to be captured, got %v", got.WorkflowDuration)
	}

	view := got.View()
	if !strings.Contains(view, "total") {
		t.Fatalf("expected done screen to include total elapsed time, got:\n%s", view)
	}
	if !strings.Contains(view, "01:05") && !strings.Contains(view, "01:06") {
		// 計測誤差で 01:05 〜 01:06 のどちらかになりうる
		t.Fatalf("expected done screen to include formatted total around 01:05, got:\n%s", view)
	}
}

func TestDoneScreenShowsTotalElapsedTimeOnError(t *testing.T) {
	resetPlayCompletionSoundSpy(t)

	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	model.Phase = PhaseWorkflow
	model.WorkflowStartedAt = time.Now().Add(-10 * time.Second)

	// WorkflowError event 経路 (workflowDoneMsg{err: nil} 並走) と
	// workflowDoneMsg{err: ...} 経路の両方を踏む。
	updated, _ := model.Update(workflowEventMsg{event: workflow.WorkflowEvent{
		Type:    workflow.WorkflowError,
		Message: "boom",
	}})
	mid := updated.(Model)
	if mid.Phase != PhaseWorkflow {
		t.Fatalf("expected phase to stay workflow until workflowDoneMsg, got %s", mid.Phase)
	}
	if len(mid.Results) != 1 || mid.Results[0].Body != "boom" {
		t.Fatalf("expected Results to contain single 'boom' entry from WorkflowError event, got %#v", mid.Results)
	}

	updated, _ = mid.Update(workflowDoneMsg{})
	got := updated.(Model)
	if got.Phase != PhaseDone {
		t.Fatalf("expected done phase after error + done msg, got %s", got.Phase)
	}
	if got.WorkflowDuration <= 0 {
		t.Fatalf("expected WorkflowDuration to be captured on error, got %v", got.WorkflowDuration)
	}
	if !strings.Contains(got.View(), "total") {
		t.Fatalf("expected done screen to include total elapsed time on error, got:\n%s", got.View())
	}
	if !strings.Contains(got.View(), "boom") {
		t.Fatalf("expected error message to be preserved on done screen, got:\n%s", got.View())
	}
}

// TestRenderDone_StacksBaseAndLearningsResultsForTailOptIn は
// SequentialWithLearningsTail (agent-learnings tail opt-in) 経由で base / tail それぞれの
// WorkflowDone が連続して届いた場合に、Result 画面で両方が縦に並んで表示されること、
// および base 結果が agent-learnings 結果より上に出ることを観測する。
//
// 観測した不具合: もとの実装では `m.Result string` を後段の WorkflowDone が上書きしていたため、
// tail opt-in 時に最終画面が agent-learnings の結果のみになり、base workflow (例: kiro 系) の
// 結果が消えていた。本テストは Results []resultEntry 化と renderDone の縦スタック化の不変式を機械検証する。
func TestRenderDone_StacksBaseAndLearningsResultsForTailOptIn(t *testing.T) {
	resetPlayCompletionSoundSpy(t)

	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	model.Phase = PhaseWorkflow

	const baseLabel = "Kiro spec design"
	const baseResult = "Kiro 仕様作成ループ完了: feature-x（6 プロンプト完了）"
	const tailLabel = "agent-learnings tail (post-run reflection)"
	const tailResult = "agent-learnings (applied-no-op)\n操作なし (curator 提案 0 件 / 全 dedup / precondition 違反等)"

	// SequentialWithLearningsTail が emit する event 列を擬似再生する:
	// Active(base) → Done(base) → Active(tail) → Done(tail) → workflowDoneMsg。
	updated, _ := model.Update(workflowEventMsg{event: workflow.WorkflowEvent{
		Type:  workflow.WorkflowActive,
		Label: baseLabel,
	}})
	model = updated.(Model)
	updated, _ = model.Update(workflowEventMsg{event: workflow.WorkflowEvent{
		Type:   workflow.WorkflowDone,
		Result: baseResult,
	}})
	model = updated.(Model)
	updated, _ = model.Update(workflowEventMsg{event: workflow.WorkflowEvent{
		Type:  workflow.WorkflowActive,
		Label: tailLabel,
	}})
	model = updated.(Model)
	updated, _ = model.Update(workflowEventMsg{event: workflow.WorkflowEvent{
		Type:   workflow.WorkflowDone,
		Result: tailResult,
	}})
	model = updated.(Model)
	updated, _ = model.Update(workflowDoneMsg{})
	model = updated.(Model)

	if got, want := len(model.Results), 2; got != want {
		t.Fatalf("expected Results len=%d, got %d (%#v)", want, got, model.Results)
	}
	if model.Results[0].Body != baseResult {
		t.Fatalf("Results[0].Body = %q, want %q", model.Results[0].Body, baseResult)
	}
	if model.Results[0].Label != baseLabel {
		t.Fatalf("Results[0].Label = %q, want %q", model.Results[0].Label, baseLabel)
	}
	if model.Results[1].Body != tailResult {
		t.Fatalf("Results[1].Body = %q, want %q", model.Results[1].Body, tailResult)
	}
	if model.Results[1].Label != tailLabel {
		t.Fatalf("Results[1].Label = %q, want %q", model.Results[1].Label, tailLabel)
	}

	view := model.View()
	if !strings.Contains(view, baseResult) {
		t.Fatalf("expected View to contain base result %q, got:\n%s", baseResult, view)
	}
	if !strings.Contains(view, "agent-learnings (applied-no-op)") {
		t.Fatalf("expected View to contain agent-learnings tail result, got:\n%s", view)
	}
	if !strings.Contains(view, baseLabel) {
		t.Fatalf("expected View to contain base label %q, got:\n%s", baseLabel, view)
	}
	if !strings.Contains(view, tailLabel) {
		t.Fatalf("expected View to contain tail label %q, got:\n%s", tailLabel, view)
	}
	baseIdx := strings.Index(view, baseResult)
	tailIdx := strings.Index(view, "agent-learnings (applied-no-op)")
	if baseIdx < 0 || tailIdx < 0 || baseIdx >= tailIdx {
		t.Fatalf("expected base result above tail result (baseIdx=%d, tailIdx=%d) in view:\n%s", baseIdx, tailIdx, view)
	}
}

// TestRenderDone_DedupesIdenticalConsecutiveResults は同じ Label / Body を持つ
// WorkflowDone event が二重に届いたケース (Sequential ラッパが両端で event を流す
// 経路を防御) で、Results に重複 entry が積まれないことを観測する。
func TestRenderDone_DedupesIdenticalConsecutiveResults(t *testing.T) {
	resetPlayCompletionSoundSpy(t)

	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	model.Phase = PhaseWorkflow
	model.CurrentWorkflowLabel = "Same"

	for i := 0; i < 2; i++ {
		updated, _ := model.Update(workflowEventMsg{event: workflow.WorkflowEvent{
			Type:   workflow.WorkflowDone,
			Result: "duplicate-result",
		}})
		model = updated.(Model)
	}

	if got, want := len(model.Results), 1; got != want {
		t.Fatalf("expected dedup to keep Results len=%d, got %d (%#v)", want, got, model.Results)
	}
}

// TestRenderDone_FallsBackToEmptyMessageWhenNoResults は Results が空のときに
// 従来挙動 (「結果はありません」を 1 ボックスで表示) を維持することを観測する。
func TestRenderDone_FallsBackToEmptyMessageWhenNoResults(t *testing.T) {
	resetPlayCompletionSoundSpy(t)

	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	model.Phase = PhaseDone

	view := model.View()
	if !strings.Contains(view, "結果はありません") {
		t.Fatalf("expected fallback message in View, got:\n%s", view)
	}
}

// TestRenderDone_LabelEmptyDoesNotRenderLabelLine は CurrentWorkflowLabel 未観測経路
// (top-level Definition.Run が直接 WorkflowDone を発した場合) で、ボックス内に
// 余計な空行 / ラベル行が出ず Body のみが表示されることを観測する。
func TestRenderDone_LabelEmptyDoesNotRenderLabelLine(t *testing.T) {
	resetPlayCompletionSoundSpy(t)

	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	model.Phase = PhaseWorkflow
	// CurrentWorkflowLabel をあえて空のまま WorkflowDone を流す。
	updated, _ := model.Update(workflowEventMsg{event: workflow.WorkflowEvent{
		Type:   workflow.WorkflowDone,
		Result: "no-label-body",
	}})
	model = updated.(Model)
	updated, _ = model.Update(workflowDoneMsg{})
	model = updated.(Model)

	if len(model.Results) != 1 {
		t.Fatalf("expected single Results entry, got %#v", model.Results)
	}
	if model.Results[0].Label != "" {
		t.Fatalf("expected empty Label, got %q", model.Results[0].Label)
	}
	view := model.View()
	if !strings.Contains(view, "no-label-body") {
		t.Fatalf("expected View to contain body, got:\n%s", view)
	}
	// Label が空のときに余計に挿入される "\n\n" prefix が出ないことを確認:
	// renderDone は Label != "" の場合のみ "Label\n\nBody" を組み立てるので、
	// Body の直前に余計な空行が積まれていないこと = view が Body をそのまま含むこと。
	if strings.Contains(view, "\n\nno-label-body") {
		t.Fatalf("expected no extra leading blank line before body, got:\n%s", view)
	}
}

func TestResultViewWrapsLongUnbrokenTextWithinTerminalWidth(t *testing.T) {
	const terminalWidth = 32
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: terminalWidth})
	model = updated.(Model)
	model.Phase = PhaseDone
	model.Results = []resultEntry{{Body: strings.Repeat("a", 120)}}

	assertViewWithinWidth(t, model.View(), terminalWidth)
}

func TestWorkflowViewWrapsLongUnbrokenStreamTextWithinTerminalWidth(t *testing.T) {
	const terminalWidth = 32
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: terminalWidth})
	model = updated.(Model)
	model.Phase = PhaseWorkflow
	model.StreamText = strings.Repeat("b", 120)

	assertViewWithinWidth(t, model.View(), terminalWidth)
}

func TestPromptViewWrapsLongUnbrokenTextWithinTerminalWidth(t *testing.T) {
	const terminalWidth = 32
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: terminalWidth})
	model = updated.(Model)
	model.Phase = PhasePrompt
	model.Prompt = strings.Repeat("p", 80)
	model.PromptItems = []workflow.SelectItem{{
		ID:    "long",
		Label: strings.Repeat("l", 80),
	}}

	assertViewWithinWidth(t, model.View(), terminalWidth)
}

func TestWorkflowAskSelectOrderedPreservesSelectionOrderAndMovesReselectedItemToTail(t *testing.T) {
	events := make(chan tea.Msg, 4)
	ask := workflowAsk{events: events}
	result := make(chan struct {
		ids []string
		ok  bool
		err error
	}, 1)

	go func() {
		ids, ok, err := workflow.SelectOrdered(context.Background(), ask, workflow.OrderedSelectParams{
			Key:    "features",
			Prompt: "対象のfeatureを順番に選択してください:",
			Items: []workflow.SelectItem{
				{ID: "alpha", Label: "Alpha"},
				{ID: "beta", Label: "Beta"},
			},
			MinItems:        1,
			DuplicatePolicy: workflow.DuplicatePolicyReject,
		})
		result <- struct {
			ids []string
			ok  bool
			err error
		}{ids: ids, ok: ok, err: err}
	}()

	model := NewModel(Options{
		CWD:         "/repo",
		Config:      config.Config{AgentProvider: provider.ProviderClaude},
		EventBuffer: events,
	})
	model.Phase = PhaseWorkflow
	updated, _ := model.Update(receiveTeaMsg(t, events))
	model = updated.(Model)

	if model.Phase != PhasePrompt {
		t.Fatalf("expected ordered prompt to enter PhasePrompt, got %s", model.Phase)
	}
	if !model.promptOrdered {
		t.Fatal("expected ordered prompt mode")
	}

	// alpha を選択 → beta を選択 → alpha を解除 → alpha を再選択。
	// 再選択した alpha は末尾に移動し、確定順は beta,alpha になる。
	model = updateModelWithKey(t, model, "space")
	model = updateModelWithKey(t, model, "down")
	model = updateModelWithKey(t, model, "space")
	model = updateModelWithKey(t, model, "up")
	model = updateModelWithKey(t, model, "space")
	model = updateModelWithKey(t, model, "space")
	if got, want := strings.Join(model.promptSelectedIDs, ","), "beta,alpha"; got != want {
		t.Fatalf("promptSelectedIDs = %q, want %q", got, want)
	}

	view := model.View()
	for _, want := range []string{"選択 #1", "選択 #2", "space 選択/解除"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expected ordered prompt view to contain %q, got:\n%s", want, view)
		}
	}

	model = updateModelWithKey(t, model, "enter")
	if model.Phase != PhaseWorkflow {
		t.Fatalf("expected enter to return to PhaseWorkflow, got %s", model.Phase)
	}

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("SelectOrdered returned error: %v", got.err)
		}
		if !got.ok {
			t.Fatal("SelectOrdered returned ok=false")
		}
		if want := []string{"beta", "alpha"}; !equalStringSlices(got.ids, want) {
			t.Fatalf("SelectOrdered ids = %#v, want %#v", got.ids, want)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("SelectOrdered did not return after enter")
	}
}

func TestWorkflowAskSelectOrderedCancelReturnsNotOK(t *testing.T) {
	events := make(chan tea.Msg, 4)
	ask := workflowAsk{events: events}
	result := make(chan struct {
		ids []string
		ok  bool
		err error
	}, 1)

	go func() {
		ids, ok, err := workflow.SelectOrdered(context.Background(), ask, workflow.OrderedSelectParams{
			Key:             "features",
			Items:           []workflow.SelectItem{{ID: "alpha", Label: "Alpha"}},
			MinItems:        1,
			DuplicatePolicy: workflow.DuplicatePolicyReject,
		})
		result <- struct {
			ids []string
			ok  bool
			err error
		}{ids: ids, ok: ok, err: err}
	}()

	model := NewModel(Options{
		CWD:         "/repo",
		Config:      config.Config{AgentProvider: provider.ProviderClaude},
		EventBuffer: events,
	})
	model.Phase = PhaseWorkflow
	updated, _ := model.Update(receiveTeaMsg(t, events))
	model = updated.(Model)
	model = updateModelWithKey(t, model, "esc")

	if model.Phase != PhaseWorkflow {
		t.Fatalf("expected esc to return to PhaseWorkflow, got %s", model.Phase)
	}

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("SelectOrdered cancel returned error: %v", got.err)
		}
		if got.ok {
			t.Fatalf("SelectOrdered cancel ok = true, ids=%#v", got.ids)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("SelectOrdered did not return after esc")
	}
}

func TestWorkflowAskSingleSelectStillReturnsCursorItem(t *testing.T) {
	events := make(chan tea.Msg, 4)
	ask := workflowAsk{events: events}
	result := make(chan struct {
		id  string
		ok  bool
		err error
	}, 1)

	go func() {
		id, ok, err := ask.Select(context.Background(), workflow.SelectParams{
			Prompt: "対象を選択してください:",
			Items: []workflow.SelectItem{
				{ID: "alpha", Label: "Alpha"},
				{ID: "beta", Label: "Beta"},
			},
		})
		result <- struct {
			id  string
			ok  bool
			err error
		}{id: id, ok: ok, err: err}
	}()

	model := NewModel(Options{
		CWD:         "/repo",
		Config:      config.Config{AgentProvider: provider.ProviderClaude},
		EventBuffer: events,
	})
	model.Phase = PhaseWorkflow
	updated, _ := model.Update(receiveTeaMsg(t, events))
	model = updated.(Model)
	if model.promptOrdered {
		t.Fatal("single select prompt unexpectedly entered ordered mode")
	}

	model = updateModelWithKey(t, model, "down")
	model = updateModelWithKey(t, model, "enter")

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("Select returned error: %v", got.err)
		}
		if !got.ok || got.id != "beta" {
			t.Fatalf("Select returned id=%q ok=%v, want beta/true", got.id, got.ok)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Select did not return after enter")
	}
}

// 修正方針 (issue #9): startWorkflow goroutine は def.Run の戻り値に関わらず
// (成功時 err==nil でも) workflowDoneMsg を必ず queue する。これにより
// workflowDoneMsg がワークフロー全体完了の唯一のシグナルとなり、TUI 側の
// Phase 遷移と afplay が 1 回に収束する。
func TestStartWorkflowQueuesWorkflowDoneMessageAfterSuccessfulTerminalEvent(t *testing.T) {
	events := make(chan tea.Msg, 4)
	definition := workflow.Definition{
		ID:    "test",
		Label: "Test Workflow",
		Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowDone, Result: "done"})
			return nil
		},
	}

	if msg := startWorkflow(events, definition, "/repo")(); msg != nil {
		t.Fatalf("expected startWorkflow command to return nil, got %#v", msg)
	}

	select {
	case msg := <-events:
		if _, ok := msg.(workflowEventMsg); !ok {
			t.Fatalf("expected workflow event, got %#v", msg)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("workflow event was not queued")
	}

	select {
	case msg := <-events:
		done, ok := msg.(workflowDoneMsg)
		if !ok {
			t.Fatalf("expected workflowDoneMsg after WorkflowDone event, got %#v", msg)
		}
		if done.err != nil {
			t.Fatalf("expected workflowDoneMsg.err to be nil on success, got %v", done.err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("workflowDoneMsg was not queued after successful workflow")
	}
}

func TestRunningWorkflowHeaderShowsSelectedWorkflowLabel(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
		Workflows: []workflow.Definition{{
			ID:    "test",
			Label: "Test Workflow",
			Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
				return nil
			},
		}},
	})

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(Model)

	view := got.View()
	if !strings.Contains(view, "Running workflow: Test Workflow") {
		t.Fatalf("expected header to include workflow label, got:\n%s", view)
	}
}

func TestRunningWorkflowHeaderUpdatesOnWorkflowActiveEvent(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	model.Phase = PhaseWorkflow
	model.CurrentWorkflowLabel = "Parent"

	updated, _ := model.Update(workflowEventMsg{event: workflow.WorkflowEvent{
		Type:  workflow.WorkflowActive,
		Label: "Child A",
	}})
	got := updated.(Model)

	if got.CurrentWorkflowLabel != "Child A" {
		t.Fatalf("expected CurrentWorkflowLabel to be updated to %q, got %q", "Child A", got.CurrentWorkflowLabel)
	}
	if !strings.Contains(got.View(), "Running workflow: Child A") {
		t.Fatalf("expected header to reflect active child workflow, got:\n%s", got.View())
	}
}

func TestViewDisplaysBuildVersionInHeader(t *testing.T) {
	model := NewModel(Options{
		CWD:          "/repo",
		Config:       config.Config{AgentProvider: provider.ProviderClaude},
		BuildVersion: "1.2.3",
	})

	if got := model.View(); !strings.Contains(got, "rilll v1.2.3") {
		t.Fatalf("expected header to include build version, got:\n%s", got)
	}
}

func TestApplyWorkflowEventUpdatesLastEventAtForTextToolUseAndReasoning(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	model.Phase = PhaseWorkflow
	baseline := time.Now().Add(-1 * time.Hour)
	model.LastEventAt = baseline

	updated, _ := model.Update(workflowEventMsg{event: workflow.WorkflowEvent{
		Type: workflow.WorkflowText,
		Text: "hello",
	}})
	got := updated.(Model)
	if !got.LastEventAt.After(baseline) {
		t.Fatalf("expected LastEventAt to advance after WorkflowText, baseline=%v got=%v", baseline, got.LastEventAt)
	}

	got.LastEventAt = baseline
	updated, _ = got.Update(workflowEventMsg{event: workflow.WorkflowEvent{
		Type: workflow.WorkflowToolUse,
		Tool: "Read",
	}})
	got = updated.(Model)
	if !got.LastEventAt.After(baseline) {
		t.Fatalf("expected LastEventAt to advance after WorkflowToolUse, baseline=%v got=%v", baseline, got.LastEventAt)
	}

	got.LastEventAt = baseline
	updated, _ = got.Update(workflowEventMsg{event: workflow.WorkflowEvent{
		Type: workflow.WorkflowReasoning,
		Text: "Inspecting tests",
	}})
	got = updated.(Model)
	if !got.LastEventAt.After(baseline) {
		t.Fatalf("expected LastEventAt to advance after WorkflowReasoning, baseline=%v got=%v", baseline, got.LastEventAt)
	}
}

func TestWorkflowViewRendersReasoningSeparatelyFromAgentText(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderCodex},
	})
	model.Phase = PhaseWorkflow
	model.applyWorkflowEvent(workflow.WorkflowEvent{Type: workflow.WorkflowReasoning, ItemID: "reason-1", Text: "Inspecting"})
	model.applyWorkflowEvent(workflow.WorkflowEvent{Type: workflow.WorkflowReasoning, ItemID: "reason-1", Text: "Inspecting tests"})
	model.applyWorkflowEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "Implementation update"})

	if model.ReasoningText != "Inspecting tests" {
		t.Fatalf("reasoning text was not stored separately: %#v", model)
	}
	view := model.View()
	if !strings.Contains(view, "thinking: Inspecting tests") || !strings.Contains(view, "Implementation update") {
		t.Fatalf("reasoning and agent text were not both rendered:\n%s", view)
	}

	model.applyWorkflowEvent(workflow.WorkflowEvent{Type: workflow.WorkflowStep, Step: 2, TotalSteps: 3, Label: "Next"})
	if model.ReasoningText != "" {
		t.Fatalf("reasoning text should clear at step boundary, got %q", model.ReasoningText)
	}
}

func TestHeartbeatMessageAdvancesSpinnerAndReturnsTickCmd(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	before := model.SpinnerFrame

	updated, cmd := model.Update(heartbeatMsg(time.Now()))
	got := updated.(Model)
	if got.SpinnerFrame == before {
		t.Fatalf("expected spinner frame to advance from %d, got %d", before, got.SpinnerFrame)
	}
	if cmd == nil {
		t.Fatal("expected heartbeat update to return a follow-up tick command")
	}
}

func TestViewDoesNotPanicWithoutWindowSize(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	// Width=0, Height=0 のままレンダリングしても panic しないこと
	_ = model.View()
	model.Phase = PhaseWorkflow
	model.StreamText = "line1\nline2\nline3"
	_ = model.View()
}

func TestWorkflowStreamRendersTailOnlyWhenHeightConstrained(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 12})
	model = updated.(Model)
	model.Phase = PhaseWorkflow
	model.CurrentWorkflowLabel = "TailTest"

	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, fmt.Sprintf("line-%03d", i))
	}
	model.StreamText = strings.Join(lines, "\n")

	view := model.View()
	if strings.Contains(view, "line-000") {
		t.Fatalf("expected first stream line to be trimmed away, got:\n%s", view)
	}
	if !strings.Contains(view, "line-199") {
		t.Fatalf("expected last stream line to be visible, got:\n%s", view)
	}
}

func TestWorkflowFooterShowsActiveWhenRecent(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = updated.(Model)
	model.Phase = PhaseWorkflow
	model.WorkflowStartedAt = time.Now().Add(-3 * time.Second)
	model.LastEventAt = time.Now()

	view := model.View()
	if !strings.Contains(view, "active") {
		t.Fatalf("expected footer to show 'active' when last event is recent, got:\n%s", view)
	}
	if !strings.Contains(view, "total") {
		t.Fatalf("expected footer to show total elapsed time, got:\n%s", view)
	}
}

func TestWorkflowFooterShowsNoEventWhenStale(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = updated.(Model)
	model.Phase = PhaseWorkflow
	model.WorkflowStartedAt = time.Now().Add(-30 * time.Second)
	model.LastEventAt = time.Now().Add(-12 * time.Second)

	view := model.View()
	if !strings.Contains(view, "no event for") {
		t.Fatalf("expected footer to warn about stalled agent, got:\n%s", view)
	}
}

func TestWorkflowFooterShowsIdleWhenBetween1And5s(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = updated.(Model)
	model.Phase = PhaseWorkflow
	model.WorkflowStartedAt = time.Now().Add(-10 * time.Second)
	model.LastEventAt = time.Now().Add(-3 * time.Second)

	view := model.View()
	if !strings.Contains(view, "idle") {
		t.Fatalf("expected footer to show idle when 1s <= silence < 5s, got:\n%s", view)
	}
}

func TestPinnedHeaderRetainsWorkflowSectionEvenWhenStreamOverflows(t *testing.T) {
	const terminalHeight = 16
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: terminalHeight})
	model = updated.(Model)
	model.Phase = PhaseWorkflow
	model.CurrentWorkflowLabel = "Pinned Section"
	model.WorkflowStartedAt = time.Now().Add(-30 * time.Second)
	model.LastEventAt = time.Now()

	var lines []string
	for i := 0; i < 500; i++ {
		lines = append(lines, fmt.Sprintf("stream-line-%03d", i))
	}
	model.StreamText = strings.Join(lines, "\n")

	view := model.View()
	if !strings.Contains(view, "Running workflow: Pinned Section") {
		t.Fatalf("expected pinned workflow section to remain visible, got:\n%s", view)
	}
	// 同時に overflow しないことも保証する。
	if got := lipgloss.Height(view); got > terminalHeight {
		t.Fatalf("View height %d exceeds terminal height %d", got, terminalHeight)
	}
}

func TestPinnedHeaderRetainsStepTextEvenWhenStreamOverflows(t *testing.T) {
	const terminalHeight = 16
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: terminalHeight})
	model = updated.(Model)
	model.Phase = PhaseWorkflow
	model.CurrentWorkflowLabel = "Pinned Section"
	model.StepText = "ステップ 17/23: タスク 10.2"
	model.WorkflowStartedAt = time.Now().Add(-30 * time.Second)
	model.LastEventAt = time.Now()

	var lines []string
	for i := 0; i < 500; i++ {
		lines = append(lines, fmt.Sprintf("stream-line-%03d", i))
	}
	model.StreamText = strings.Join(lines, "\n")

	view := model.View()
	if !strings.Contains(view, "ステップ 17/23") {
		t.Fatalf("expected pinned step text to remain visible after stream overflow, got:\n%s", view)
	}
	// header の行追加分が budget に正しく反映されていることの回帰防止。
	if got := lipgloss.Height(view); got > terminalHeight {
		t.Fatalf("View height %d exceeds terminal height %d", got, terminalHeight)
	}
}

func TestViewHeightDoesNotExceedTerminalHeightWhenStreamOverflows(t *testing.T) {
	const terminalHeight = 24
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: terminalHeight})
	model = updated.(Model)
	model.Phase = PhaseWorkflow
	model.CurrentWorkflowLabel = "Long Run"
	model.WorkflowStartedAt = time.Now().Add(-30 * time.Second)
	model.LastEventAt = time.Now()

	// 終端ウィンドウより大幅に長いストリームを流し込む。
	var lines []string
	for i := 0; i < 500; i++ {
		lines = append(lines, fmt.Sprintf("line-%03d", i))
	}
	model.StreamText = strings.Join(lines, "\n")

	view := model.View()
	got := lipgloss.Height(view)
	// View() の rendered height は terminalHeight 以下でなければ altscreen がスクロールしてヘッダーが流れる。
	// 末尾の "\n" 一つ分を許容するため +1 まで許す。
	if got > terminalHeight {
		t.Fatalf("View height %d exceeds terminal height %d (would push header off-screen)", got, terminalHeight)
	}
}

func TestWindowSizeMessageStoresHeight(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	got := updated.(Model)
	if got.Width != 100 || got.Height != 40 {
		t.Fatalf("expected Width=100 Height=40, got Width=%d Height=%d", got.Width, got.Height)
	}
}

// --- Task 8.1: handleKey / View の Phase 依存ディスパッチ ---
//
// Req 1.1 / 1.2 / 1.3 / 1.4 / 1.5 / 1.6 / 1.7 / 1.8 / 2.7 / 9.3 / 9.4 / 10.1 / 10.3
// に対応する 6 ケース (a)〜(f) を独立に検証する。
//
// 既存の model_test.go テスト群は本タスクで一切変更しない (Req 9.3 / 9.4 の
// 機械的検査: 既存挙動の非破壊性は「既存テストが無変更で通る」をもって担保)。

// (a) PhaseMenu で `a` 押下 → Phase=PhaseAkashic、Akashic.Subview=uiakashic.SubviewList
// (Req 1.1 / 1.2 / 2.1)
func TestMenuShortcutOpensAkashicMode(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	got := updated.(Model)
	if got.Phase != PhaseAkashic {
		t.Fatalf("expected akashic phase, got %s", got.Phase)
	}
	if got.Akashic.Subview != uiakashic.SubviewList {
		t.Fatalf("expected initial subview uiakashic.SubviewList, got %s", got.Akashic.Subview)
	}
}

// (b) PhaseAkashic で `Esc` 押下 → Phase=PhaseMenu (Req 2.7)
func TestAkashicModeEscReturnsToMenu(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	model.Phase = PhaseAkashic
	model.Akashic.Subview = uiakashic.SubviewList

	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got := updated.(Model)
	if got.Phase != PhaseMenu {
		t.Fatalf("expected esc to return to menu phase, got %s", got.Phase)
	}
	if cmd != nil {
		// Esc 経路では Cmd は発行されない (退出のみ)。
		t.Fatalf("expected nil Cmd from esc in akashic, got %T", cmd)
	}
}

// (c) PhaseAkashic で `q` 押下 → Phase=PhaseMenu かつ tea.Quit を返さない
// (Req 1.4: モード退出として再解釈)
func TestAkashicModeQuitKeyReturnsToMenuWithoutQuit(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	model.Phase = PhaseAkashic
	model.Akashic.Subview = uiakashic.SubviewList

	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	got := updated.(Model)
	if got.Phase != PhaseMenu {
		t.Fatalf("expected q in akashic to return to menu, got phase %s", got.Phase)
	}
	if cmd != nil {
		// PhaseAkashic 中の q は tea.Quit を発行しない (Req 1.4)。
		t.Fatalf("expected nil Cmd (no tea.Quit) from q in akashic, got %T", cmd)
	}
}

// (d) PhaseMenu で `q` 押下 → tea.Quit を返す (Req 1.5: 既存挙動の確認再掲)
func TestMenuQuitKeyReturnsQuitCmd(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})

	_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("expected tea.Quit Cmd from q in menu, got nil")
	}
	// tea.Cmd を invoke して tea.QuitMsg が返ることを確認する
	// (tea.Quit() は呼び出すと tea.QuitMsg を返す関数)。
	if msg := cmd(); msg == nil {
		t.Fatal("expected tea.Quit Cmd to produce non-nil QuitMsg")
	} else if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", msg)
	}
}

// (e) PhaseAkashic で `s` 押下 → Phase が変わらない (Req 1.7)
func TestAkashicModeSKeyDoesNotEnterProviderSettings(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	model.Phase = PhaseAkashic
	model.Akashic.Subview = uiakashic.SubviewList

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	got := updated.(Model)
	if got.Phase != PhaseAkashic {
		t.Fatalf("expected phase to remain akashic, got %s", got.Phase)
	}
}

// (f) PhaseAkashic で `ctrl+c` 押下 → tea.Quit を返す (Req 1.8)
func TestAkashicModeCtrlCReturnsQuitCmd(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	model.Phase = PhaseAkashic
	model.Akashic.Subview = uiakashic.SubviewList

	_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("expected tea.Quit Cmd from ctrl+c in akashic, got nil")
	}
	if msg := cmd(); msg == nil {
		t.Fatal("expected tea.Quit Cmd to produce non-nil QuitMsg")
	} else if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", msg)
	}
}

// memory-distiller-trigger-policy task 4.1 (Req 3.1 / 3.2):
// ctrl+c 押下時、UI Options 経由で渡された ActiveCancelOrigin が
// foreground call の *atomic.Value を指している場合、その ref に
// "user" が Store され、かつ tea.QuitMsg が返ること。
//
// design.md "Ctrl+C cancel-origin emitter" の Implementation Notes
// (atomic 直接観測で mock interface 不要) を追従する単体観察テスト。
func TestCtrlCStoresUserOriginToForegroundRef(t *testing.T) {
	// foreground call スコープの atomic.Value (= bridge.WithCancelOrigin で
	// agent.CallAgent 入口に attach される予定の ref と同形)。
	callRef := &atomic.Value{}

	// UI Model 寿命と等価な Pointer。cmd/rilll/main.go から渡されることを模擬。
	activeCancelOrigin := &atomic.Pointer[atomic.Value]{}
	// agent.CallAgent 入口で UI に foreground register された状態を模擬。
	activeCancelOrigin.Store(callRef)

	model := NewModel(Options{
		CWD:                "/repo",
		Config:             config.Config{AgentProvider: provider.ProviderClaude},
		ActiveCancelOrigin: activeCancelOrigin,
	})

	_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyCtrlC})

	// ctrl+c 直後に foreground ref へ "user" が Store されている (Req 3.2)。
	loaded := callRef.Load()
	if loaded == nil {
		t.Fatalf("expected foreground ref to hold cancel origin, got nil")
	}
	got, ok := loaded.(string)
	if !ok {
		t.Fatalf("expected foreground ref value to be string, got %T", loaded)
	}
	if got != bridge.CancelOriginUser {
		t.Fatalf("foreground ref Load() = %q, want %q", got, bridge.CancelOriginUser)
	}

	// tea.Quit Cmd が返ること (Req 1.8 既存挙動保全)。
	if cmd == nil {
		t.Fatal("expected tea.Quit Cmd from ctrl+c, got nil")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", msg)
	}
}

// memory-distiller-trigger-policy task 4.1 (Req 6 既存挙動保全):
// ActiveCancelOrigin == nil (= 旧 Options 経路 / call register 前) で
// ctrl+c が押された場合、no-op (panic / nil deref しない) で tea.QuitMsg のみ
// 返ること。既存 ctrl+c テストが Options に ActiveCancelOrigin を渡さなくても
// 従来挙動を維持できることを構造的に保証する。
func TestCtrlCWithoutActiveCancelOriginReturnsQuitOnly(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
		// ActiveCancelOrigin は未注入 (nil)。
	})

	_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyCtrlC})

	if cmd == nil {
		t.Fatal("expected tea.Quit Cmd from ctrl+c without ActiveCancelOrigin, got nil")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", msg)
	}
}

// memory-distiller-trigger-policy task 4.1 (defense-in-depth):
// ActiveCancelOrigin が UI Model に注入済みでも、まだ foreground call が
// register されていない (= activeCancelOrigin.Load() == nil) 状態で ctrl+c
// が押された場合、no-op + tea.QuitMsg のみ返ること (panic / nil deref しない)。
//
// 想定シナリオ: call return 後の re-prompt 中など、`ActiveCancelOrigin` が
// CompareAndSwap で nil 化されている瞬間に ctrl+c が割り込んだケース。
func TestCtrlCWithEmptyForegroundRefReturnsQuitOnly(t *testing.T) {
	activeCancelOrigin := &atomic.Pointer[atomic.Value]{}
	// 意図的に Store しない (= Load() は nil を返す)。

	model := NewModel(Options{
		CWD:                "/repo",
		Config:             config.Config{AgentProvider: provider.ProviderClaude},
		ActiveCancelOrigin: activeCancelOrigin,
	})

	_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyCtrlC})

	// foreground ref 未 register なので panic せず Quit のみ返る。
	if cmd == nil {
		t.Fatal("expected tea.Quit Cmd from ctrl+c with empty foreground ref, got nil")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", msg)
	}

	// 副作用として ref は依然 nil のまま (Store されていない)。
	if loaded := activeCancelOrigin.Load(); loaded != nil {
		t.Fatalf("expected activeCancelOrigin to remain unset, got %v", loaded)
	}
}

// PhaseMenu で `a` 押下時に Reader が注入されていれば akashic.ListRunsCmd が発行され、
// Reader.List が呼び出されること (Req 1.1: 初回 akashic.ListRunsCmd 発行)。
func TestMenuShortcutAkashicIssuesListRunsCmdWhenReaderInjected(t *testing.T) {
	reader := &fakeReader{}
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
		Reader: reader,
	})

	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	got := updated.(Model)
	if got.Phase != PhaseAkashic {
		t.Fatalf("expected akashic phase, got %s", got.Phase)
	}
	if cmd == nil {
		t.Fatal("expected non-nil Cmd from `a` press when Reader is injected")
	}

	// Cmd を invoke すると Reader.List が呼ばれて uiakashic.ListRunsMsg が返ること
	msg := cmd()
	if _, ok := msg.(uiakashic.ListRunsMsg); !ok {
		t.Fatalf("expected uiakashic.ListRunsMsg, got %T", msg)
	}
	if reader.listCalls != 1 {
		t.Fatalf("expected Reader.List to be called once, got %d", reader.listCalls)
	}
	if reader.listOpts.Limit != 50 {
		// akashic.ListRunsCmd は ListOptions.Limit を必ず DefaultListLimit (=50) に強制する (Req 9.5)
		t.Fatalf("expected akashic.ListRunsCmd to force Limit=50, got %d", reader.listOpts.Limit)
	}
}

// PhaseMenu で `a` 押下時に Reader 未配線なら Cmd=nil で Phase 遷移のみ起きる (Req 11.1 / 11.2)。
func TestMenuShortcutAkashicWithoutReaderReturnsNilCmd(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
		// Reader は未注入 (nil)
	})

	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	got := updated.(Model)
	if got.Phase != PhaseAkashic {
		t.Fatalf("expected akashic phase even without reader, got %s", got.Phase)
	}
	if cmd != nil {
		t.Fatalf("expected nil Cmd when Reader is nil, got %T", cmd)
	}
}

// PhaseAkashic 突入時に Akashic.View が呼ばれて Body に反映されること (View ディスパッチ)。
func TestViewDispatchesToAkashicViewWhenPhaseAkashic(t *testing.T) {
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	got := updated.(Model)
	if got.Phase != PhaseAkashic {
		t.Fatalf("expected akashic phase, got %s", got.Phase)
	}

	view := got.View()
	// akashic.State.View(uiakashic.SubviewList) が描画する section ヘッダ (akashic/view.go renderList) と
	// Reader=nil graceful 文言 (Req 11.2) が表示されることを確認する。
	if !strings.Contains(view, "アカシックモード - Run 一覧") {
		t.Fatalf("expected akashic list section header in view, got:\n%s", view)
	}
	if !strings.Contains(view, "履歴データが利用できません") {
		t.Fatalf("expected reader-unavailable graceful message in view, got:\n%s", view)
	}
}

func assertViewWithinWidth(t *testing.T, view string, width int) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(view, "\n"), "\n") {
		if got := lipgloss.Width(line); got > width {
			t.Fatalf("expected all lines to fit within width %d, got line width %d:\n%s", width, got, view)
		}
	}
}

func receiveTeaMsg(t *testing.T, events <-chan tea.Msg) tea.Msg {
	t.Helper()
	select {
	case msg := <-events:
		return msg
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for tea message")
		return nil
	}
}

func updateModelWithKey(t *testing.T, model Model, key string) Model {
	t.Helper()
	var msg tea.KeyMsg
	switch key {
	case "up":
		msg = tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "space":
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	updated, _ := model.Update(msg)
	return updated.(Model)
}

func equalStringSlices(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// resetPlayCompletionSoundSpy は ui パッケージの playCompletionSound フックを
// no-op の spy に差し替え、呼び出し回数を記録するカウンタへのポインタを返す。
// テスト終了時に元のフックへ自動復元する。issue #9 で導入: テスト中に実マシンの
// afplay が鳴るのを防ぎつつ、TUI 経路で afplay が 1 回だけ起動されることを
// 検証する目的で使う。
func resetPlayCompletionSoundSpy(t *testing.T) *int {
	t.Helper()
	original := playCompletionSound
	var count int
	playCompletionSound = func() { count++ }
	t.Cleanup(func() {
		playCompletionSound = original
	})
	return &count
}

// (1) 複合ワークフロー想定: 子の WorkflowDone event が複数飛んでも afplay は
// 鳴らず、最後に届く workflowDoneMsg で 1 回だけ呼ばれることを検証する
// (issue #9 受け入れ基準: Sequential 実行で afplay が 1 回だけ鳴る)。
func TestPlayCompletionSoundFiresOnceForSequentialChildDoneEvents(t *testing.T) {
	count := resetPlayCompletionSoundSpy(t)

	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	model.Phase = PhaseWorkflow
	model.WorkflowStartedAt = time.Now().Add(-1 * time.Second)

	// Sequential(A, B, C) を模した子の WorkflowDone event 列。
	// 修正方針: 子の WorkflowDone では afplay は鳴らない。
	for _, label := range []string{"A", "B", "C"} {
		updated, _ := model.Update(workflowEventMsg{event: workflow.WorkflowEvent{
			Type:   workflow.WorkflowDone,
			Result: "child " + label + " done",
		}})
		model = updated.(Model)
	}
	if *count != 0 {
		t.Fatalf("expected playCompletionSound NOT to fire for child WorkflowDone events, got %d calls", *count)
	}

	// トップレベル Run の戻り = workflowDoneMsg。ここで唯一 afplay が鳴る。
	updated, _ := model.Update(workflowDoneMsg{})
	model = updated.(Model)
	if *count != 1 {
		t.Fatalf("expected playCompletionSound to fire exactly once at workflowDoneMsg, got %d calls", *count)
	}
	if model.Phase != PhaseDone {
		t.Fatalf("expected phase to be done after workflowDoneMsg, got %s", model.Phase)
	}
}

// (2) Sequential 実行中に最初の子が WorkflowDone を発した後でも、後続の子の
// WorkflowActive event が UI に届き CurrentWorkflowLabel が更新されること。
// 修正方針 4: workflowEventMsg の早期 return を撤去し events ポンプを継続する。
func TestSubsequentChildEventsKeepUpdatingUIAfterChildDone(t *testing.T) {
	resetPlayCompletionSoundSpy(t)

	events := make(chan tea.Msg, 16)
	model := NewModel(Options{
		CWD:         "/repo",
		Config:      config.Config{AgentProvider: provider.ProviderClaude},
		EventBuffer: events,
	})
	model.Phase = PhaseWorkflow
	model.CurrentWorkflowLabel = "Sequential"

	// 子 A の完了 (WorkflowDone) → 子 B の開始 (WorkflowActive)。
	updated, cmdAfterDone := model.Update(workflowEventMsg{event: workflow.WorkflowEvent{
		Type:   workflow.WorkflowDone,
		Result: "child A done",
	}})
	model = updated.(Model)
	if cmdAfterDone == nil {
		t.Fatal("expected events pump to continue (non-nil Cmd) after child WorkflowDone")
	}

	updated, _ = model.Update(workflowEventMsg{event: workflow.WorkflowEvent{
		Type:  workflow.WorkflowActive,
		Label: "Child B",
	}})
	model = updated.(Model)
	if model.CurrentWorkflowLabel != "Child B" {
		t.Fatalf("expected CurrentWorkflowLabel to update to subsequent child, got %q", model.CurrentWorkflowLabel)
	}
	if model.Phase != PhaseWorkflow {
		t.Fatalf("expected phase to remain workflow during sequential execution, got %s", model.Phase)
	}
}

// (3) エラー終了経路でも afplay が 1 回だけ鳴ることを検証する。
// workflowDoneMsg{err: someError} が単独で届くケース (def.Run が error を
// 返したケース) を想定。
func TestPlayCompletionSoundFiresOnceOnError(t *testing.T) {
	count := resetPlayCompletionSoundSpy(t)

	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})
	model.Phase = PhaseWorkflow
	model.WorkflowStartedAt = time.Now().Add(-1 * time.Second)

	updated, _ := model.Update(workflowDoneMsg{err: errors.New("boom")})
	got := updated.(Model)
	if *count != 1 {
		t.Fatalf("expected playCompletionSound to fire exactly once on error, got %d calls", *count)
	}
	if got.Phase != PhaseDone {
		t.Fatalf("expected phase to be done after workflowDoneMsg with error, got %s", got.Phase)
	}
	if !strings.Contains(got.View(), "boom") {
		t.Fatalf("expected error message to be reflected on done screen, got:\n%s", got.View())
	}
}
