package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

func TestLoadMissingConfigUsesClaudeDefault(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	got, err := LoadFromPath(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("LoadFromPath returned error: %v", err)
	}
	if got.Config.AgentProvider != provider.ProviderClaude {
		t.Fatalf("unexpected default config: %#v", got)
	}
	if got.Config.AkashicLevel != AkashicSummary {
		t.Fatalf("unexpected default akashic level: %#v", got)
	}
	if got.Config.Codex.ModelPreset != CodexModelPresetGPT55XHigh {
		t.Fatalf("unexpected default codex preset: %#v", got.Config.Codex)
	}
	if got.Config.Codex.ReasoningSummary != CodexReasoningSummaryConcise {
		t.Fatalf("unexpected default codex reasoning summary: %#v", got.Config.Codex)
	}
}

func TestSaveThenLoadConfig(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "rilll", "config.json")
	_, err := SaveToPath(path, Config{
		AgentProvider: provider.ProviderCursor,
		AkashicLevel:  AkashicDebug,
		Codex: CodexConfig{
			ModelPreset:      CodexModelPresetGPT6LunaMax,
			ReasoningSummary: CodexReasoningSummaryDetailed,
		},
	})
	if err != nil {
		t.Fatalf("SaveToPath returned error: %v", err)
	}

	got, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath returned error: %v", err)
	}
	if got.Config.AgentProvider != provider.ProviderCursor {
		t.Fatalf("saved config was not loaded: %#v", got)
	}
	if got.Config.AkashicLevel != AkashicDebug {
		t.Fatalf("saved akashic level was not loaded: %#v", got)
	}
	if got.Config.Codex.ModelPreset != CodexModelPresetGPT6LunaMax {
		t.Fatalf("saved codex preset was not loaded: %#v", got.Config.Codex)
	}
	if got.Config.Codex.ReasoningSummary != CodexReasoningSummaryDetailed {
		t.Fatalf("saved codex reasoning summary was not loaded: %#v", got.Config.Codex)
	}
}

func TestLoadOldConfigDefaultsAkashicLevel(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"agentProvider":"claude"}`), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	got, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath returned error: %v", err)
	}
	if got.Warning != "" {
		t.Fatalf("old config should not warn, got %q", got.Warning)
	}
	if got.Config.AkashicLevel != AkashicSummary {
		t.Fatalf("old config did not default akashic level: %#v", got)
	}
	if got.Config.Codex.ModelPreset != CodexModelPresetGPT55XHigh {
		t.Fatalf("old config did not default codex preset: %#v", got.Config)
	}
	if got.Config.Codex.ReasoningSummary != CodexReasoningSummaryConcise {
		t.Fatalf("old config did not default codex reasoning summary: %#v", got.Config)
	}
}

func TestLoadInvalidAkashicLevelUsesDefaultWithWarning(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"agentProvider":"claude","akashicLevel":"verbose"}`), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	got, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath returned error: %v", err)
	}
	if got.Config.AkashicLevel != AkashicSummary || got.Config.AgentProvider != provider.ProviderClaude {
		t.Fatalf("invalid config did not use defaults: %#v", got)
	}
	if !strings.Contains(got.Warning, "invalid akashicLevel") {
		t.Fatalf("warning did not mention akashicLevel: %q", got.Warning)
	}
}

func TestSaveAcceptsCodexProvider(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "rilll", "config.json")
	if _, err := SaveToPath(path, Config{
		AgentProvider: provider.ProviderCodex,
		AkashicLevel:  AkashicFull,
	}); err != nil {
		t.Fatalf("SaveToPath returned error: %v", err)
	}

	got, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath returned error: %v", err)
	}
	if got.Config.AgentProvider != provider.ProviderCodex {
		t.Fatalf("codex config was not loaded: %#v", got)
	}
}

func TestSaveAcceptsCursorSDKProvider(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "rilll", "config.json")
	if _, err := SaveToPath(path, Config{
		AgentProvider: provider.ProviderCursorSDK,
		AkashicLevel:  AkashicSummary,
	}); err != nil {
		t.Fatalf("SaveToPath returned error: %v", err)
	}

	got, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath returned error: %v", err)
	}
	if got.Config.AgentProvider != provider.ProviderCursorSDK {
		t.Fatalf("cursor-sdk config was not loaded: %#v", got)
	}
}

func TestSaveRejectsInvalidAkashicLevel(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "config.json")
	_, err := SaveToPath(path, Config{
		AgentProvider: provider.ProviderClaude,
		AkashicLevel:  AkashicLevel("verbose"),
	})
	if err == nil {
		t.Fatal("expected invalid akashic level error")
	}
}

func TestCodexGPT6LunaMaxPresetRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := Default()
	cfg.AgentProvider = provider.ProviderCodex
	cfg.Codex.ModelPreset = CodexModelPreset("gpt-6-luna-max")
	if _, err := SaveToPath(path, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFromPath(path)
	if err != nil || got.Warning != "" || !reflect.DeepEqual(got.Config, cfg) {
		t.Fatalf("round trip = %#v, %v; want %#v", got, err, cfg)
	}
	want := provider.ModelSettings{Model: "gpt-6-luna", Effort: "max"}
	if selection := CodexModelSelection(got.Config.Codex.ModelPreset); !reflect.DeepEqual(selection, want) {
		t.Fatalf("selection = %#v, want %#v", selection, want)
	}
}

func TestLoadRejectsRetiredCodexPresetsWithoutCompatibility(t *testing.T) {
	for _, preset := range []string{"gpt-5.3-codex-spark-high", "gpt-5.6-luna-max"} {
		t.Run(preset, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			body := `{"agentProvider":"codex","akashicLevel":"debug","codex":{"modelPreset":"` + preset + `","reasoningSummary":"detailed"}}`
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := LoadFromPath(path)
			if err != nil || !strings.Contains(got.Warning, "invalid codex.modelPreset") {
				t.Fatalf("load = %#v, %v; want invalid preset warning", got, err)
			}
			if validCodexModelPreset(CodexModelPreset(preset)) {
				t.Fatalf("retired preset %q must not remain selectable", preset)
			}
		})
	}
}

func TestLoadAcceptsCodexModelPreset(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"agentProvider":"codex","akashicLevel":"summary","codex":{"modelPreset":"gpt-6-luna-max"}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	got, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath returned error: %v", err)
	}
	if got.Warning != "" {
		t.Fatalf("valid codex preset should not warn, got %q", got.Warning)
	}
	if got.Config.Codex.ModelPreset != CodexModelPresetGPT6LunaMax {
		t.Fatalf("codex preset not loaded: %#v", got.Config.Codex)
	}
	selection := CodexModelSelection(got.Config.Codex.ModelPreset)
	if selection.Model != "gpt-6-luna" || selection.Effort != "max" {
		t.Fatalf("codex preset resolved to wrong model settings: %#v", selection)
	}
}

func TestCodexGPT56SolModelPresets(t *testing.T) {
	tests := []struct {
		name       string
		preset     CodexModelPreset
		wantEffort string
	}{
		{name: "high", preset: CodexModelPresetGPT56SolHigh, wantEffort: "high"},
		{name: "xhigh", preset: CodexModelPresetGPT56SolXHigh, wantEffort: "xhigh"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetDeprecationWarningOnceForTest()
			path := filepath.Join(t.TempDir(), "config.json")
			if _, err := SaveToPath(path, Config{
				AgentProvider: provider.ProviderCodex,
				AkashicLevel:  AkashicSummary,
				Codex: CodexConfig{
					ModelPreset:      tt.preset,
					ReasoningSummary: CodexReasoningSummaryConcise,
				},
			}); err != nil {
				t.Fatalf("SaveToPath returned error: %v", err)
			}

			got, err := LoadFromPath(path)
			if err != nil {
				t.Fatalf("LoadFromPath returned error: %v", err)
			}
			if got.Warning != "" {
				t.Fatalf("valid codex preset should not warn, got %q", got.Warning)
			}
			if got.Config.Codex.ModelPreset != tt.preset {
				t.Fatalf("codex preset not loaded: got %q, want %q", got.Config.Codex.ModelPreset, tt.preset)
			}

			want := provider.ModelSettings{Model: "gpt-5.6-sol", Effort: tt.wantEffort}
			if selection := CodexModelSelection(got.Config.Codex.ModelPreset); !reflect.DeepEqual(selection, want) {
				t.Fatalf("codex preset resolved to wrong model settings: got %#v, want %#v", selection, want)
			}
		})
	}
}

func TestLoadInvalidCodexModelPresetUsesDefaultWithWarning(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"agentProvider":"codex","akashicLevel":"summary","codex":{"modelPreset":"gpt-5.2-high"}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	got, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath returned error: %v", err)
	}
	if got.Config.Codex.ModelPreset != CodexModelPresetGPT55XHigh {
		t.Fatalf("invalid codex preset should fall back to default config: %#v", got.Config)
	}
	if !strings.Contains(got.Warning, "invalid codex.modelPreset") {
		t.Fatalf("warning did not mention codex.modelPreset: %q", got.Warning)
	}
}

func TestSaveRejectsInvalidCodexModelPreset(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "config.json")
	_, err := SaveToPath(path, Config{
		AgentProvider: provider.ProviderCodex,
		AkashicLevel:  AkashicSummary,
		Codex:         CodexConfig{ModelPreset: CodexModelPreset("gpt-5.2-high")},
	})
	if err == nil {
		t.Fatal("expected invalid codex model preset error")
	}
}

func TestLoadInvalidCodexReasoningSummaryUsesDefaultWithWarning(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"agentProvider":"codex","akashicLevel":"summary","codex":{"modelPreset":"gpt-5.5-xhigh","reasoningSummary":"verbose"}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	got, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath returned error: %v", err)
	}
	if got.Config.Codex.ReasoningSummary != CodexReasoningSummaryConcise {
		t.Fatalf("invalid reasoning summary should fall back to default config: %#v", got.Config)
	}
	if !strings.Contains(got.Warning, "invalid codex.reasoningSummary") {
		t.Fatalf("warning did not mention codex.reasoningSummary: %q", got.Warning)
	}
}

func TestSaveRejectsInvalidCodexReasoningSummary(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "config.json")
	_, err := SaveToPath(path, Config{
		AgentProvider: provider.ProviderCodex,
		AkashicLevel:  AkashicSummary,
		Codex: CodexConfig{
			ModelPreset:      CodexModelPresetGPT55XHigh,
			ReasoningSummary: CodexReasoningSummary("verbose"),
		},
	})
	if err == nil {
		t.Fatal("expected invalid codex reasoning summary error")
	}
}

// Cursor SDK preset は Codex と同形の (model, effort) ペア選択 + 正規化を行う。
// 既定 (Composer25) は Effort が空文字で、bridge.Request.Effort の omitempty 経由で
// wire から消える / TS bridge が ModelSelection.params を omit する設計。
func TestLoadAcceptsCursorSDKModelPreset(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"agentProvider":"cursor-sdk","akashicLevel":"summary","cursorSDK":{"modelPreset":"gpt-5.5-xhigh"}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	got, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath returned error: %v", err)
	}
	if got.Warning != "" {
		t.Fatalf("valid cursor-sdk preset should not warn, got %q", got.Warning)
	}
	if got.Config.CursorSDK.ModelPreset != CursorSDKModelPresetGPT55XHigh {
		t.Fatalf("cursor-sdk preset not loaded: %#v", got.Config.CursorSDK)
	}
	selection := CursorSDKModelSelection(got.Config.CursorSDK.ModelPreset)
	if selection.Model != "gpt-5.5" || selection.Effort != "xhigh" {
		t.Fatalf("cursor-sdk preset resolved to wrong model settings: %#v", selection)
	}
}

func TestLoadAcceptsGrok45Fast(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"agentProvider":"cursor-sdk","akashicLevel":"summary","cursorSDK":{"modelPreset":"grok-4.5-fast"}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	got, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath returned error: %v", err)
	}
	if got.Warning != "" {
		t.Fatalf("valid grok-4.5-fast preset should not warn, got %q", got.Warning)
	}
	if got.Config.CursorSDK.ModelPreset != CursorSDKModelPresetGrok45Fast {
		t.Fatalf("grok-4.5-fast preset not loaded: %#v", got.Config.CursorSDK)
	}
	selection := CursorSDKModelSelection(got.Config.CursorSDK.ModelPreset)
	if selection.Model != "grok-4.5" || selection.Effort != "" {
		t.Fatalf("grok-4.5-fast preset resolved to wrong model settings: %#v", selection)
	}
}

func TestLoadAcceptsGPT56LunaMax(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"agentProvider":"cursor-sdk","akashicLevel":"summary","cursorSDK":{"modelPreset":"gpt-5.6-luna-max"}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	got, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath returned error: %v", err)
	}
	if got.Warning != "" {
		t.Fatalf("valid gpt-5.6-luna-max preset should not warn, got %q", got.Warning)
	}
	if got.Config.CursorSDK.ModelPreset != CursorSDKModelPresetGPT56LunaMax {
		t.Fatalf("gpt-5.6-luna-max preset not loaded: %#v", got.Config.CursorSDK)
	}
	selection := CursorSDKModelSelection(got.Config.CursorSDK.ModelPreset)
	if selection.Model != "gpt-5.6-luna" || selection.Effort != "" || len(selection.ModelParams) != 1 ||
		selection.ModelParams[0].ID != "reasoning" || selection.ModelParams[0].Value != "max" {
		t.Fatalf("gpt-5.6-luna-max preset resolved to wrong model settings: %#v", selection)
	}
}

func TestLoadInvalidCursorSDKModelPresetUsesDefaultWithWarning(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"agentProvider":"cursor-sdk","akashicLevel":"summary","cursorSDK":{"modelPreset":"gpt-5.2-high"}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	got, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath returned error: %v", err)
	}
	if got.Config.CursorSDK.ModelPreset != CursorSDKModelPresetComposer25 {
		t.Fatalf("invalid cursor-sdk preset should fall back to default config: %#v", got.Config)
	}
	if !strings.Contains(got.Warning, "invalid cursorSDK.modelPreset") {
		t.Fatalf("warning did not mention cursorSDK.modelPreset: %q", got.Warning)
	}
}

func TestSaveRejectsInvalidCursorSDKModelPreset(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "config.json")
	_, err := SaveToPath(path, Config{
		AgentProvider: provider.ProviderCursorSDK,
		AkashicLevel:  AkashicSummary,
		CursorSDK:     CursorSDKConfig{ModelPreset: CursorSDKModelPreset("gpt-5.2-high")},
	})
	if err == nil {
		t.Fatal("expected invalid cursor-sdk model preset error")
	}
}

// composer-2.5 (既定) は fast=false を明示し、Effort は空文字のまま wire 上で omit される。
func TestCursorSDKModelSelectionComposer25UsesFastFalse(t *testing.T) {
	selection := CursorSDKModelSelection(CursorSDKModelPresetComposer25)
	if selection.Model != "composer-2.5" {
		t.Fatalf("expected composer-2.5 model, got %q", selection.Model)
	}
	if selection.Effort != "" {
		t.Fatalf("composer-2.5 must omit effort, got %q", selection.Effort)
	}
	if len(selection.ModelParams) != 1 || selection.ModelParams[0].ID != "fast" || selection.ModelParams[0].Value != "false" {
		t.Fatalf("composer-2.5 must use fast=false, got %#v", selection.ModelParams)
	}
}

// composer-2.5-fast は ModelParams を omit し、bridge が Cursor SDK 既定 variant (fast=true) を選ぶ。
func TestCursorSDKModelSelectionComposer25Fast(t *testing.T) {
	selection := CursorSDKModelSelection(CursorSDKModelPresetComposer25Fast)
	if selection.Model != "composer-2.5" {
		t.Fatalf("expected composer-2.5 model, got %q", selection.Model)
	}
	if selection.Effort != "" {
		t.Fatalf("composer-2.5-fast must omit effort, got %q", selection.Effort)
	}
	if len(selection.ModelParams) != 0 {
		t.Fatalf("composer-2.5-fast must omit model params, got %#v", selection.ModelParams)
	}
}

// grok-4.5-fast は grok-4.5 モデル + effort=high + fast=true を狙うため、reasoning (Effort) は空文字にする。
// model.ts の resolveModelSelection が model.variants の isDefault な variant を選択する。
func TestCursorSDKModelSelectionGrok45Fast(t *testing.T) {
	selection := CursorSDKModelSelection(CursorSDKModelPresetGrok45Fast)
	if selection.Model != "grok-4.5" {
		t.Fatalf("expected grok-4.5 model, got %q", selection.Model)
	}
	if selection.Effort != "" {
		t.Fatalf("grok-4.5-fast must omit effort so default variant is used, got %q", selection.Effort)
	}
}

func TestLoadAcceptsGPT56SolHigh(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"agentProvider":"cursor-sdk","akashicLevel":"summary","cursorSDK":{"modelPreset":"gpt-5.6-sol-high"}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	got, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath returned error: %v", err)
	}
	if got.Warning != "" {
		t.Fatalf("valid gpt-5.6-sol-high preset should not warn, got %q", got.Warning)
	}
	if got.Config.CursorSDK.ModelPreset != CursorSDKModelPresetGPT56SolHigh {
		t.Fatalf("gpt-5.6-sol-high preset not loaded: %#v", got.Config.CursorSDK)
	}
	selection := CursorSDKModelSelection(got.Config.CursorSDK.ModelPreset)
	if selection.Model != "gpt-5.6-sol" || selection.Effort != "high" || len(selection.ModelParams) != 0 {
		t.Fatalf("gpt-5.6-sol-high preset resolved to wrong model settings: %#v", selection)
	}
}

// gpt-5.6-luna-max は Cursor SDK の正規モデル IDへ解決して渡す。
func TestCursorSDKModelSelectionGPT56LunaMax(t *testing.T) {
	selection := CursorSDKModelSelection(CursorSDKModelPresetGPT56LunaMax)
	if selection.Model != "gpt-5.6-luna" {
		t.Fatalf("expected gpt-5.6-luna model, got %q", selection.Model)
	}
	if selection.Effort != "" {
		t.Fatalf("gpt-5.6-luna-max must omit effort, got %q", selection.Effort)
	}
	if len(selection.ModelParams) != 1 {
		t.Fatalf("expected one model param, got %#v", selection.ModelParams)
	}
	if selection.ModelParams[0].ID != "reasoning" || selection.ModelParams[0].Value != "max" {
		t.Fatalf("expected reasoning=max, got %#v", selection.ModelParams[0])
	}
}

func TestCursorSDKModelSelectionGPT56SolHigh(t *testing.T) {
	selection := CursorSDKModelSelection(CursorSDKModelPresetGPT56SolHigh)
	if selection.Model != "gpt-5.6-sol" {
		t.Fatalf("expected gpt-5.6-sol model, got %q", selection.Model)
	}
	if selection.Effort != "high" {
		t.Fatalf("expected high effort, got %q", selection.Effort)
	}
	if len(selection.ModelParams) != 0 {
		t.Fatalf("gpt-5.6-sol-high must not use model params, got %#v", selection.ModelParams)
	}
}

// 新フィールド名 akashicLevel を主として読み込めることを確認 (Req 6.1)
func TestLoadAcceptsAkashicLevel(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"agentProvider":"claude","akashicLevel":"debug"}`), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	got, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath returned error: %v", err)
	}
	if got.Config.AkashicLevel != AkashicDebug {
		t.Fatalf("expected akashicLevel debug, got %#v", got.Config)
	}
	if got.Warning != "" {
		t.Fatalf("akashicLevel only should not warn, got %q", got.Warning)
	}
}

// 旧フィールドのみを fallback として採用、deprecation warning が出る (Req 6.2, 6.3)
func TestLoadFallsBackToObservabilityLevelWithWarning(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"agentProvider":"claude","observabilityLevel":"debug"}`), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	got, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath returned error: %v", err)
	}
	if got.Config.AkashicLevel != AkashicDebug {
		t.Fatalf("legacy field did not populate AkashicLevel: %#v", got.Config)
	}
	if !strings.Contains(got.Warning, "observabilityLevel") || !strings.Contains(got.Warning, "akashicLevel") {
		t.Fatalf("expected deprecation warning mentioning observabilityLevel and akashicLevel, got %q", got.Warning)
	}
}

// 両方のフィールドが指定されている場合は新を採用、旧は無視される旨を warning (Req 6.4)
func TestLoadPrefersAkashicLevelWhenBothPresent(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"agentProvider":"claude","akashicLevel":"summary","observabilityLevel":"debug"}`), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	got, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath returned error: %v", err)
	}
	if got.Config.AkashicLevel != AkashicSummary {
		t.Fatalf("expected akashicLevel to win over observabilityLevel: %#v", got.Config)
	}
	if !strings.Contains(got.Warning, "observabilityLevel") {
		t.Fatalf("expected warning mentioning observabilityLevel ignored, got %q", got.Warning)
	}
}

// 保存時は新フィールドのみ書き出され、旧フィールドが消える (Req 6.5)
func TestSaveDropsLegacyObservabilityLevel(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"agentProvider":"claude","observabilityLevel":"debug"}`), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	loaded, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath returned error: %v", err)
	}
	if _, err := SaveToPath(path, loaded.Config); err != nil {
		t.Fatalf("SaveToPath returned error: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to re-read config: %v", err)
	}
	if strings.Contains(string(raw), "observabilityLevel") {
		t.Fatalf("saved JSON still contains observabilityLevel: %s", raw)
	}
	if !strings.Contains(string(raw), "akashicLevel") {
		t.Fatalf("saved JSON missing akashicLevel: %s", raw)
	}

	// reload で旧 fallback が起動しない (warning が空)
	resetDeprecationWarningOnceForTest()
	reloaded, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("reload returned error: %v", err)
	}
	if reloaded.Warning != "" {
		t.Fatalf("reload should not produce warning, got %q", reloaded.Warning)
	}
	if reloaded.Config.AkashicLevel != AkashicDebug {
		t.Fatalf("reloaded value mismatch: %#v", reloaded.Config)
	}
}

// deprecation warning は process 内で 1 度限り (Req 6.3)
func TestDeprecationWarningEmittedOnce(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"agentProvider":"claude","observabilityLevel":"debug"}`), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	first, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("first LoadFromPath returned error: %v", err)
	}
	if first.Warning == "" {
		t.Fatalf("first load should emit deprecation warning")
	}

	second, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("second LoadFromPath returned error: %v", err)
	}
	if second.Warning != "" {
		t.Fatalf("second load should not re-emit warning, got %q", second.Warning)
	}
	if second.Config.AkashicLevel != AkashicDebug {
		t.Fatalf("second load value mismatch: %#v", second.Config)
	}
}

// 中間構造体追従ガード: legacyConfig は Config の全フィールド + 旧 fallback 1 フィールドを持つ (Req 6.2 / 6.5 リグレッション防止)
func TestConfigStructFieldCountMatchesLegacyConfig(t *testing.T) {
	cfgFields := reflect.TypeOf(Config{}).NumField()
	legacyFields := reflect.TypeOf(legacyConfig{}).NumField()
	if legacyFields-1 != cfgFields {
		t.Fatalf("Config has %d fields, legacyConfig has %d (expected legacyConfig = Config + 1 legacy fallback). Config に新規フィールドを追加した場合は legacyConfig にも追従させてください", cfgFields, legacyFields)
	}
}

// Workflows 未設定の旧 config を round-trip した際に JSON 出力へ "workflows" キーが現れない (omitempty 不変式、memory-distiller Req 1.3)
func TestSaveOmitsEmptyWorkflows(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "config.json")
	// Workflows フィールドを持たない旧 config を入力として書き込む
	if err := os.WriteFile(path, []byte(`{"agentProvider":"claude","akashicLevel":"summary"}`), 0o644); err != nil {
		t.Fatalf("failed to seed config: %v", err)
	}

	loaded, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath returned error: %v", err)
	}
	if loaded.Config.Workflows != nil {
		t.Fatalf("expected Workflows to be nil for legacy config, got %#v", loaded.Config.Workflows)
	}

	if _, err := SaveToPath(path, loaded.Config); err != nil {
		t.Fatalf("SaveToPath returned error: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to re-read config: %v", err)
	}
	if strings.Contains(string(raw), "workflows") {
		t.Fatalf("saved JSON should omit workflows when nil, got: %s", raw)
	}
}

// 保存後の JSON シリアライゼーションが新フィールド名のみであることをスキーマレベルで確認 (Req 6.5)
func TestConfigJSONShapeOnlyHasAkashicLevel(t *testing.T) {
	body, err := json.Marshal(Config{
		AgentProvider: provider.ProviderClaude,
		AkashicLevel:  AkashicSummary,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "observabilityLevel") {
		t.Fatalf("marshalled Config contains observabilityLevel: %s", body)
	}
	if !strings.Contains(string(body), "akashicLevel") {
		t.Fatalf("marshalled Config missing akashicLevel: %s", body)
	}
}

// WorkflowConfig.DistillLearningsAfter=false は omitempty で省略される (agent-learnings Req 1.4)。
// 未設定 = false の状態を JSON 出力から消すことで legacy config 形と区別不能にする。
func TestSaveOmitsFalseDistillLearningsAfter(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	body, err := json.Marshal(WorkflowConfig{DistillLearningsAfter: false})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "distillLearningsAfter") {
		t.Fatalf("marshalled WorkflowConfig should omit distillLearningsAfter when false, got: %s", body)
	}
}

// Workflows{"git-commit":{DistillLearningsAfter:true}} を含む config の Save → Load round-trip で値が保持される
// (agent-learnings Req 1.2: tail wrap exposure の opt-in flag)
func TestSaveLoadWorkflowDistillLearningsAfterRoundTrip(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "rilll", "config.json")
	original := Config{
		AgentProvider: provider.ProviderClaude,
		AkashicLevel:  AkashicSummary,
		Workflows: map[string]WorkflowConfig{
			"git-commit": {DistillLearningsAfter: true},
		},
	}
	if _, err := SaveToPath(path, original); err != nil {
		t.Fatalf("SaveToPath returned error: %v", err)
	}

	// JSON にキーが現れることを文字列レベルで確認 (omitempty が誤って key を消していない)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read saved config: %v", err)
	}
	if !strings.Contains(string(raw), "distillLearningsAfter") {
		t.Fatalf("saved JSON missing distillLearningsAfter key: %s", raw)
	}

	got, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath returned error: %v", err)
	}
	wc, ok := got.Config.Workflows["git-commit"]
	if !ok {
		t.Fatalf("workflows entry git-commit not loaded: %#v", got.Config.Workflows)
	}
	if !wc.DistillLearningsAfter {
		t.Fatalf("DistillLearningsAfter not preserved through round-trip: %#v", wc)
	}
}

// legacyConfig 経由 (LoadFromPath) でも distillLearningsAfter が WorkflowConfig に正しく流れる
// (legacyConfig.Workflows map[string]WorkflowConfig 経由の自動連動を verify、agent-learnings Req 1.2)
func TestLoadAcceptsDistillLearningsAfterViaLegacyPath(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"agentProvider":"claude","akashicLevel":"summary","workflows":{"git-commit":{"distillLearningsAfter":true}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	got, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath returned error: %v", err)
	}
	wc, ok := got.Config.Workflows["git-commit"]
	if !ok {
		t.Fatalf("workflows entry git-commit not loaded: %#v", got.Config.Workflows)
	}
	if !wc.DistillLearningsAfter {
		t.Fatalf("DistillLearningsAfter not loaded via legacy path: %#v", wc)
	}
}

// distillLearningsAfter キーが JSON に無い場合、unmarshal 後の値は false (Go bool zero value)
// (agent-learnings Req 1.4: 未設定 = wrap 非露出 を保証する基盤)
func TestUnsetDistillLearningsAfterDefaultsToFalse(t *testing.T) {
	resetDeprecationWarningOnceForTest()
	path := filepath.Join(t.TempDir(), "config.json")
	// distillLearningsAfter キーを敢えて欠落させた config を入力にする
	body := `{"agentProvider":"claude","akashicLevel":"summary","workflows":{"git-commit":{}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	got, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath returned error: %v", err)
	}
	wc, ok := got.Config.Workflows["git-commit"]
	if !ok {
		t.Fatalf("workflows entry git-commit not loaded: %#v", got.Config.Workflows)
	}
	if wc.DistillLearningsAfter {
		t.Fatalf("DistillLearningsAfter should default to false when unset, got %#v", wc)
	}
}
