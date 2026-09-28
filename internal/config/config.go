package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

// Config は rilll の永続化設定。観測粒度は AkashicLevel で表現する (Req 1.4)。
type Config struct {
	AgentProvider provider.ProviderID `json:"agentProvider"`
	AkashicLevel  AkashicLevel        `json:"akashicLevel"`
	// OtelEnabled は OpenTelemetry (trace + metric) の opt-in スイッチ。
	// false のとき OTEL_* 環境変数があっても exporter を初期化しない。
	OtelEnabled bool            `json:"otelEnabled,omitempty"`
	Codex       CodexConfig     `json:"codex,omitzero"`
	CursorSDK   CursorSDKConfig `json:"cursorSDK,omitzero"`
	// Workflows は workflow 単位の opt-in 設定 (memory-distiller Req 1.3)。
	// 未設定 (nil map) は omitempty で JSON 出力から省略され、後方互換を保つ。
	Workflows map[string]WorkflowConfig `json:"workflows,omitempty"`
}

// CodexConfig は Codex SDK provider 固有の設定。
type CodexConfig struct {
	// ModelPreset は rilll が許可する Codex model/reasoning の組み合わせ。
	// 空文字列のときは CodexModelPresetGPT55XHigh に正規化される。
	ModelPreset CodexModelPreset `json:"modelPreset,omitempty"`
	// ReasoningSummary は Codex SDK が生成する reasoning summary の粒度。
	// raw chain-of-thought ではなく、SDK が公開する要約のみを対象とする。
	// 空文字列のときは CodexReasoningSummaryConcise に正規化される。
	ReasoningSummary CodexReasoningSummary `json:"reasoningSummary,omitempty"`
}

func (c CodexConfig) IsZero() bool {
	return c.ModelPreset == "" && c.ReasoningSummary == ""
}

type CodexModelPreset string

const (
	CodexModelPresetGPT55XHigh    CodexModelPreset = "gpt-5.5-xhigh"
	CodexModelPresetGPT6LunaMax   CodexModelPreset = "gpt-6-luna-max"
	CodexModelPresetGPT56SolHigh  CodexModelPreset = "gpt-5.6-sol-high"
	CodexModelPresetGPT56SolXHigh CodexModelPreset = "gpt-5.6-sol-xhigh"
)

type CodexReasoningSummary string

const (
	CodexReasoningSummaryAuto     CodexReasoningSummary = "auto"
	CodexReasoningSummaryConcise  CodexReasoningSummary = "concise"
	CodexReasoningSummaryDetailed CodexReasoningSummary = "detailed"
	CodexReasoningSummaryNone     CodexReasoningSummary = "none"
)

// CursorSDKConfig は Cursor SDK provider 固有の設定。
type CursorSDKConfig struct {
	// ModelPreset は rilll が許可する Cursor SDK model/reasoning の組み合わせ。
	// 空文字列のときは CursorSDKModelPresetComposer25 に正規化される。
	ModelPreset CursorSDKModelPreset `json:"modelPreset,omitempty"`
}

func (c CursorSDKConfig) IsZero() bool {
	return c.ModelPreset == ""
}

type CursorSDKModelPreset string

const (
	// CursorSDKModelPresetComposer25 は composer-2.5 を fast=false で使うプリセット。
	// reasoning は未指定 (Effort 空文字) のまま fast だけ明示する (= /git-commit と同形)。
	CursorSDKModelPresetComposer25 CursorSDKModelPreset = "composer-2.5"
	// CursorSDKModelPresetComposer25Fast は composer-2.5 を Cursor SDK 既定 variant
	// (fast=true) で使うプリセット。旧 composer-2.5 プリセットと同じ wire 挙動。
	CursorSDKModelPresetComposer25Fast CursorSDKModelPreset = "composer-2.5-fast"
	// CursorSDKModelPresetGPT55XHigh は gpt-5.5 を xhigh reasoning で使うプリセット。
	CursorSDKModelPresetGPT55XHigh CursorSDKModelPreset = "gpt-5.5-xhigh"
	// CursorSDKModelPresetGrok45Fast は grok-4.5-fast を grok-4.5 モデル + effort=high + fast=true に展開するプリセット。
	CursorSDKModelPresetGrok45Fast CursorSDKModelPreset = "grok-4.5-fast"
	// CursorSDKModelPresetGPT56LunaMax は gpt-5.6-luna を reasoning=max で使うプリセット。
	CursorSDKModelPresetGPT56LunaMax CursorSDKModelPreset = "gpt-5.6-luna-max"
	// CursorSDKModelPresetGPT56SolHigh は gpt-5.6-sol を high reasoning で使うプリセット。
	CursorSDKModelPresetGPT56SolHigh CursorSDKModelPreset = "gpt-5.6-sol-high"
)

// WorkflowConfig は workflow 単位の opt-in 設定。
// agent-learnings 用 tail wrap opt-in flag (DistillLearningsAfter) を保持する。
// 旧 v1 memory-distiller 用 field (DistillAfter / MinFailureCount) は
// memory-distiller-removal spec で削除済 (本 file からは消滅)。
// 既存 ~/.config/rilll/config.json に残置された古い distillAfter / minFailureCount キーは
// encoding/json の default 挙動 (未知 key は無視) で unmarshal が壊れない。
type WorkflowConfig struct {
	// DistillLearningsAfter は当該 workflow 完了後に agent-learnings tail wrap
	// (`<base-id>-with-learnings`) を Sequential 合成するか否か。未指定 (false) のときは
	// wrap を露出しない (agent-learnings Req 1.2 / Req 1.4 の opt-in 不変式)。
	// legacyConfig は Workflows map[string]WorkflowConfig 経由でこの field を自動連動するため
	// legacyConfig 側に重複追加は不要 (TestConfigStructFieldCountMatchesLegacyConfig 互換維持)。
	DistillLearningsAfter bool `json:"distillLearningsAfter,omitempty"`
}

// AkashicLevel は記録粒度を表す型。文字列値 (summary/full/debug) は維持する (Req 1.8)。
type AkashicLevel string

const (
	AkashicSummary AkashicLevel = "summary"
	AkashicFull    AkashicLevel = "full"
	AkashicDebug   AkashicLevel = "debug"
)

// legacyConfig は LoadFromPath 専用の中間構造体。
// 旧フィールド observabilityLevel を後方互換で受け取り、unmarshal 後に
// AkashicLevel に merge する。Config に新フィールドを追加する場合は
// この構造体への追従が必須 (TestConfigStructFieldCountMatchesLegacyConfig
// で追従漏れを検出する)。
type legacyConfig struct {
	AgentProvider            provider.ProviderID       `json:"agentProvider"`
	AkashicLevel             AkashicLevel              `json:"akashicLevel"`
	OtelEnabled              bool                      `json:"otelEnabled,omitempty"`
	Codex                    CodexConfig               `json:"codex,omitzero"`
	CursorSDK                CursorSDKConfig           `json:"cursorSDK,omitzero"`
	Workflows                map[string]WorkflowConfig `json:"workflows,omitempty"`
	LegacyObservabilityLevel AkashicLevel              `json:"observabilityLevel,omitempty"`
}

type LoadResult struct {
	Config  Config
	Path    string
	Warning string
}

// deprecationWarningOnce はプロセス内で旧フィールド由来の deprecation warning を
// 1 度だけ発火させるためのゲート (Req 6.3, 6.4)。テストからは
// resetDeprecationWarningOnceForTest でリセットする。
var deprecationWarningOnce sync.Once

func resetDeprecationWarningOnceForTest() {
	deprecationWarningOnce = sync.Once{}
}

// emitDeprecationWarningOnce は最初の呼び出しでのみ msg を返し、以降は空文字列を返す。
func emitDeprecationWarningOnce(msg string) string {
	var out string
	deprecationWarningOnce.Do(func() {
		out = msg
	})
	return out
}

func Default() Config {
	return Config{
		AgentProvider: provider.ProviderClaude,
		AkashicLevel:  AkashicSummary,
		Codex:         DefaultCodexConfig(),
		CursorSDK:     DefaultCursorSDKConfig(),
	}
}

func DefaultCodexConfig() CodexConfig {
	return CodexConfig{
		ModelPreset:      CodexModelPresetGPT55XHigh,
		ReasoningSummary: CodexReasoningSummaryConcise,
	}
}

func DefaultCursorSDKConfig() CursorSDKConfig {
	return CursorSDKConfig{ModelPreset: CursorSDKModelPresetComposer25}
}

func CodexModelSelection(preset CodexModelPreset) provider.ModelSettings {
	switch preset {
	case CodexModelPresetGPT6LunaMax:
		return provider.ModelSettings{Model: "gpt-6-luna", Effort: "max"}
	case CodexModelPresetGPT56SolHigh:
		return provider.ModelSettings{Model: "gpt-5.6-sol", Effort: "high"}
	case CodexModelPresetGPT56SolXHigh:
		return provider.ModelSettings{Model: "gpt-5.6-sol", Effort: "xhigh"}
	default:
		return provider.ModelSettings{Model: "gpt-5.5", Effort: "xhigh"}
	}
}

// CodexProviderSettings は永続化された Codex 設定を provider 呼び出し設定へ展開する。
func CodexProviderSettings(cfg CodexConfig) provider.ModelSettings {
	settings := CodexModelSelection(cfg.ModelPreset)
	settings.ReasoningSummary = string(cfg.ReasoningSummary)
	return settings
}

// CursorSDKModelSelection は preset を model / effort / model params に展開する。
// composer-2.5 は fast=false を明示し、composer-2.5-fast は ModelParams を omit して
// bridge が Cursor SDK 既定 variant (fast=true) を選ぶ。
func CursorSDKModelSelection(preset CursorSDKModelPreset) provider.ModelSettings {
	switch preset {
	case CursorSDKModelPresetComposer25Fast:
		return provider.ModelSettings{Model: "composer-2.5", Effort: ""}
	case CursorSDKModelPresetGPT55XHigh:
		return provider.ModelSettings{Model: "gpt-5.5", Effort: "xhigh"}
	case CursorSDKModelPresetGrok45Fast:
		// model.ts の resolveModelSelection が model.variants の isDefault な variant を選ぶ。
		// grok-4.5 の isDefault は effort=high + fast=true なので、reasoning (Effort) は空文字にしておく。
		return provider.ModelSettings{Model: "grok-4.5", Effort: ""}
	case CursorSDKModelPresetGPT56LunaMax:
		return provider.ModelSettings{
			Model: "gpt-5.6-luna",
			ModelParams: []provider.ModelParam{
				{ID: "reasoning", Value: "max"},
			},
		}
	case CursorSDKModelPresetGPT56SolHigh:
		return provider.ModelSettings{Model: "gpt-5.6-sol", Effort: "high"}
	default:
		return provider.ModelSettings{
			Model:  "composer-2.5",
			Effort: "",
			ModelParams: []provider.ModelParam{
				{ID: "fast", Value: "false"},
			},
		}
	}
}

func Path() string {
	home := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			userHome = "."
		}
		home = filepath.Join(userHome, ".config")
	}
	return filepath.Join(home, "rilll", "config.json")
}

func Load() (LoadResult, error) {
	return LoadFromPath(Path())
}

func LoadFromPath(path string) (LoadResult, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return LoadResult{Config: Default(), Path: path}, nil
		}
		return LoadResult{Config: Default(), Path: path, Warning: "設定ファイルを読み込めなかったため、既定の claude を使用しています: " + err.Error()}, nil
	}

	var legacy legacyConfig
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return LoadResult{Config: Default(), Path: path, Warning: "設定ファイルを読み込めなかったため、既定の claude を使用しています: " + err.Error()}, nil
	}

	cfg := Config{
		AgentProvider: legacy.AgentProvider,
		AkashicLevel:  legacy.AkashicLevel,
		OtelEnabled:   legacy.OtelEnabled,
		Codex:         legacy.Codex,
		CursorSDK:     legacy.CursorSDK,
		Workflows:     legacy.Workflows,
	}

	// Req 6.2 / 6.4: 旧フィールド observabilityLevel との merge ロジック。
	var warning string
	switch {
	case legacy.AkashicLevel != "" && legacy.LegacyObservabilityLevel != "":
		// 両方指定: 新を採用し、旧は無視する旨を 1 度だけ警告する。
		warning = emitDeprecationWarningOnce("設定ファイルの observabilityLevel は廃止されました。akashicLevel を採用したため observabilityLevel は無視されます")
	case legacy.AkashicLevel == "" && legacy.LegacyObservabilityLevel != "":
		// 旧のみ指定: 旧値を採用し deprecation warning を 1 度だけ出す。
		cfg.AkashicLevel = legacy.LegacyObservabilityLevel
		warning = emitDeprecationWarningOnce("設定ファイルの observabilityLevel は廃止されました。akashicLevel に置き換えてください")
	}

	if !validProvider(cfg.AgentProvider) {
		return LoadResult{Config: Default(), Path: path, Warning: "設定ファイルを読み込めなかったため、既定の claude を使用しています: invalid agentProvider"}, nil
	}
	if cfg.AkashicLevel == "" {
		cfg.AkashicLevel = AkashicSummary
	}
	if !validAkashicLevel(cfg.AkashicLevel) {
		return LoadResult{Config: Default(), Path: path, Warning: "設定ファイルを読み込めなかったため、既定の claude を使用しています: invalid akashicLevel"}, nil
	}
	if cfg.Codex.ModelPreset == "" {
		cfg.Codex.ModelPreset = DefaultCodexConfig().ModelPreset
	}
	if !validCodexModelPreset(cfg.Codex.ModelPreset) {
		return LoadResult{Config: Default(), Path: path, Warning: "設定ファイルを読み込めなかったため、既定の claude を使用しています: invalid codex.modelPreset"}, nil
	}
	if cfg.Codex.ReasoningSummary == "" {
		cfg.Codex.ReasoningSummary = DefaultCodexConfig().ReasoningSummary
	}
	if !validCodexReasoningSummary(cfg.Codex.ReasoningSummary) {
		return LoadResult{Config: Default(), Path: path, Warning: "設定ファイルを読み込めなかったため、既定の claude を使用しています: invalid codex.reasoningSummary"}, nil
	}
	if cfg.CursorSDK.ModelPreset == "" {
		cfg.CursorSDK = DefaultCursorSDKConfig()
	}
	if !validCursorSDKModelPreset(cfg.CursorSDK.ModelPreset) {
		return LoadResult{Config: Default(), Path: path, Warning: "設定ファイルを読み込めなかったため、既定の claude を使用しています: invalid cursorSDK.modelPreset"}, nil
	}
	return LoadResult{Config: cfg, Path: path, Warning: warning}, nil
}

func Save(cfg Config) (LoadResult, error) {
	return SaveToPath(Path(), cfg)
}

func SaveToPath(path string, cfg Config) (LoadResult, error) {
	if !validProvider(cfg.AgentProvider) {
		return LoadResult{}, errors.New("invalid agentProvider")
	}
	if cfg.AkashicLevel == "" {
		cfg.AkashicLevel = AkashicSummary
	}
	if !validAkashicLevel(cfg.AkashicLevel) {
		return LoadResult{}, errors.New("invalid akashicLevel")
	}
	if cfg.Codex.ModelPreset == "" {
		cfg.Codex.ModelPreset = DefaultCodexConfig().ModelPreset
	}
	if !validCodexModelPreset(cfg.Codex.ModelPreset) {
		return LoadResult{}, errors.New("invalid codex.modelPreset")
	}
	if cfg.Codex.ReasoningSummary == "" {
		cfg.Codex.ReasoningSummary = DefaultCodexConfig().ReasoningSummary
	}
	if !validCodexReasoningSummary(cfg.Codex.ReasoningSummary) {
		return LoadResult{}, errors.New("invalid codex.reasoningSummary")
	}
	if cfg.CursorSDK.ModelPreset == "" {
		cfg.CursorSDK = DefaultCursorSDKConfig()
	}
	if !validCursorSDKModelPreset(cfg.CursorSDK.ModelPreset) {
		return LoadResult{}, errors.New("invalid cursorSDK.modelPreset")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return LoadResult{}, err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	// Save は Config 構造体のみを marshal するため、旧 observabilityLevel フィールドは
	// 出力されず、次回 Load では旧 fallback が起動しない (Req 6.5)。
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return LoadResult{}, err
	}
	body = append(body, '\n')
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return LoadResult{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return LoadResult{}, err
	}
	return LoadResult{Config: cfg, Path: path}, nil
}

func validAkashicLevel(level AkashicLevel) bool {
	switch level {
	case AkashicSummary, AkashicFull, AkashicDebug:
		return true
	default:
		return false
	}
}

func validProvider(id provider.ProviderID) bool {
	switch id {
	case provider.ProviderClaude, provider.ProviderCursor, provider.ProviderCodex, provider.ProviderCursorSDK:
		return true
	default:
		return false
	}
}

func validCodexModelPreset(preset CodexModelPreset) bool {
	switch preset {
	case CodexModelPresetGPT55XHigh,
		CodexModelPresetGPT6LunaMax,
		CodexModelPresetGPT56SolHigh,
		CodexModelPresetGPT56SolXHigh:
		return true
	default:
		return false
	}
}

func validCodexReasoningSummary(summary CodexReasoningSummary) bool {
	switch summary {
	case CodexReasoningSummaryAuto, CodexReasoningSummaryConcise, CodexReasoningSummaryDetailed, CodexReasoningSummaryNone:
		return true
	default:
		return false
	}
}

func validCursorSDKModelPreset(preset CursorSDKModelPreset) bool {
	switch preset {
	case CursorSDKModelPresetComposer25, CursorSDKModelPresetComposer25Fast, CursorSDKModelPresetGPT55XHigh, CursorSDKModelPresetGrok45Fast, CursorSDKModelPresetGPT56LunaMax, CursorSDKModelPresetGPT56SolHigh:
		return true
	default:
		return false
	}
}
