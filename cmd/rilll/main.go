package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/kazuki-oshino/rilll/internal/agent"
	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/cli"
	"github.com/kazuki-oshino/rilll/internal/config"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/provider/claude"
	"github.com/kazuki-oshino/rilll/internal/provider/codex"
	"github.com/kazuki-oshino/rilll/internal/provider/cursor"
	"github.com/kazuki-oshino/rilll/internal/provider/cursorsdk"
	"github.com/kazuki-oshino/rilll/internal/provider/middleware/streamidle"
	"github.com/kazuki-oshino/rilll/internal/telemetry"
	"github.com/kazuki-oshino/rilll/internal/ui"
	"github.com/kazuki-oshino/rilll/internal/version"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin"
)

func main() {
	if output, ok := versionOutput(os.Args, version.BuildVersion); ok {
		fmt.Print(output)
		return
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	loaded, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	otelShutdown, otelWarning := initTelemetry(loaded.Config)
	defer func() { _ = shutdownTelemetry(otelShutdown) }()

	// 非対話 CLI 経路 (`rilll list` / `rilll run` / `rilll help`) は TUI を起動せず、
	// ワークフローを直接実行して exit code を返す。引数なし / `--version` は
	// 既存通り TUI / バージョン表示にフォールバックする。
	if cli.IsSubcommand(os.Args) {
		if warning := mergeWarnings(loaded.Warning, otelWarning); warning != "" {
			fmt.Fprintln(os.Stderr, warning)
		}
		recorder, _ := initAkashicRecorder()
		defer recorder.Close()
		agentClient := buildAgentClient(loaded.Config, recorder)
		os.Exit(cli.Dispatch(context.Background(), os.Args, cli.Deps{
			Stdout:    os.Stdout,
			Stderr:    os.Stderr,
			Workflows: builtin.Registry(agentClient),
			CWD:       cwd,
			Reader:    asReader(recorder),
		}))
	}

	// アカシックレコード DB の初期化:
	//   1. ResolveDBPath() で env (RILLL_AKASHIC_DB / RILLL_OBSERVABILITY_DB) と XDG default を解決
	//   2. Migrate() で旧 observability.sqlite (および WAL 副ファイル群) を新 path へ自動移行
	//   3. OpenSQLite() で recorder を構築 (失敗時は NoopRecorder fallback)
	// 各ステップの warning は改行で merge され、loaded.Warning と合流して UI に伝播する。
	recorder, recorderWarning := initAkashicRecorder()
	defer recorder.Close()
	warning := mergeWarnings(loaded.Warning, recorderWarning, otelWarning)

	agentClient := buildAgentClient(loaded.Config, recorder)

	// activeCancelOrigin は ctrl+c で foreground の agent_call に "user" 起源の
	// cancel origin を伝播するための UI 寿命と等価な単一 Pointer
	// (memory-distiller-trigger-policy task 4.1, Req 3.1 / 3.2)。
	// 指す先 (call スコープ *atomic.Value) は agent.CallAgent 入口/return で
	// register/unregister される (task 4.2 で配線)。Pointer 自体は
	// プロセス起動時に 1 個だけ生成し、UI Model および agent client にそのまま渡す。
	activeCancelOrigin := &atomic.Pointer[atomic.Value]{}

	// agent.CallAgent 入口で foreground register / 出口で CAS unregister する経路を
	// 有効化する (task 4.2)。CLI 経路 (上の cli.IsSubcommand 分岐) では
	// SetActiveCancelOrigin を呼ばず Pointer 不在のまま動作させ、UI register を
	// skip する契約を維持する (= 背景 subagent dispatch / 並列 evaluator 経路と同形)。
	agentClient.SetActiveCancelOrigin(activeCancelOrigin)

	// Reader / ArtifactReader はアカシックモード (PhaseAkashic) のデータ源として
	// ui.Options に注入する (akashic-mode-tui Task 9.1)。
	// NoopRecorder fallback 時は両者 nil で渡され、TUI 側が「履歴データが利用できません」
	// / 「成果物データが利用できません」の graceful 表示にフォールバックする (Req 11.1〜11.5)。
	model := ui.NewModel(ui.Options{
		CWD:                cwd,
		BuildVersion:       version.BuildVersion,
		Config:             loaded.Config,
		Warning:            warning,
		Workflows:          builtin.Registry(agentClient),
		Reader:             asReader(recorder),
		ArtifactReader:     asArtifactReader(recorder),
		ActiveCancelOrigin: activeCancelOrigin,
		SaveConfig: func(cfg config.Config) error {
			agentClient.SetProvider(cfg.AgentProvider)
			agentClient.SetAkashicLevel(string(cfg.AkashicLevel))
			agentClient.SetProviderModelDefaults(providerModelDefaultsFromConfig(cfg))
			_, err := config.Save(cfg)
			return err
		},
	})

	if _, err := tea.NewProgram(model, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout), tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// buildAgentClient は loaded config と recorder から
// streamidle middleware 込みの agent.Agent を組み立てる。
// TUI 経路と CLI 経路の双方から共有する。
func buildAgentClient(cfg config.Config, recorder akashic.Recorder) *agent.Agent {
	providers := map[provider.ProviderID]provider.AgentProvider{
		provider.ProviderClaude:    streamidle.Wrap(claude.New()),
		provider.ProviderCursor:    streamidle.Wrap(cursor.New()),
		provider.ProviderCodex:     streamidle.Wrap(codex.New()),
		provider.ProviderCursorSDK: streamidle.Wrap(cursorsdk.New()),
	}
	agentClient := agent.New(cfg.AgentProvider, providers, recorder)
	agentClient.SetAkashicLevel(string(cfg.AkashicLevel))
	agentClient.SetProviderModelDefaults(providerModelDefaultsFromConfig(cfg))
	return agentClient
}

func providerModelDefaultsFromConfig(cfg config.Config) map[provider.ProviderID]provider.ModelSettings {
	return map[provider.ProviderID]provider.ModelSettings{
		provider.ProviderCodex:     config.CodexProviderSettings(cfg.Codex),
		provider.ProviderCursorSDK: config.CursorSDKModelSelection(cfg.CursorSDK.ModelPreset),
	}
}

const telemetryShutdownTimeout = 500 * time.Millisecond

func initTelemetry(cfg config.Config) (func(context.Context) error, string) {
	shutdown, err := telemetry.Init(context.Background(), cfg, version.BuildVersion)
	if err != nil {
		return func(context.Context) error { return nil }, "OpenTelemetry を無効化しました: " + err.Error()
	}
	return shutdown, ""
}

func shutdownTelemetry(shutdown func(context.Context) error) error {
	if shutdown == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), telemetryShutdownTimeout)
	defer cancel()
	return shutdown(ctx)
}

// initAkashicRecorder は DB path 解決 → 旧 path からの自動移行 → SQLite open
// を順に実施し、各ステップで生じた warning を改行で merge して返す。
//
// 返り値 warning は loaded.Warning (config 由来 deprecation 通知) と更に merge され、
// 既存の warningStyle 経路 (ui.Model.Warning) で 1 行ずつ表示される。
func initAkashicRecorder() (akashic.Recorder, string) {
	pathResult := akashic.ResolveDBPath()
	migrationResult := akashic.Migrate(pathResult.Path)
	recorder, recorderWarning := newAkashicRecorder(migrationResult.Path)
	return recorder, mergeWarnings(pathResult.Warning, migrationResult.Warning, recorderWarning)
}

// newAkashicRecorder は SQLite を open し、失敗時は NoopRecorder にフォールバックする。
// 失敗時の warning は呼び出し側で merge され UI に表示される (Req 8.4: NoopRecorder 差し替えの維持)。
func newAkashicRecorder(path string) (akashic.Recorder, string) {
	recorder, err := akashic.OpenSQLite(path)
	if err != nil {
		return akashic.NoopRecorder{}, "観測データの保存を無効化しました: " + err.Error()
	}
	return recorder, ""
}

// asReader は recorder を read-only な akashic.Reader として cli.Deps へ注入するヘルパ
// (Req 1.4 / 1.6)。Reader インターフェースを満たす recorder のみを Reader として返し、
// それ以外 (NoopRecorder fallback / nil 等) では nil を返す。
//
// nil 戻り値は CLI 経路の cli.runAkashic で「akashic データベースが利用できません」
// と判定され、exit 1 で応答される (internal/cli/akashic.go の export op 分岐参照)。
//
// 設計判断: 静的型 assertion (`r.(*akashic.SQLiteRecorder)`) ではなく interface 型
// assertion (`r.(akashic.Reader)`) を使う。これにより以下を達成する:
//
//   - boundary 遵守: 1.5 のスコープは cmd/rilll/main.go に閉じる。
//     internal/akashic/ 配下に Reader 実装スタブを足す必要がない
//   - 進化的整合: タスク 2.1〜2.5 で *akashic.SQLiteRecorder が Reader を段階的に満たす
//     につれ、本ヘルパは実装側に手を入れずに自動的に Reader を返すようになる
//   - NoopRecorder 経路: NoopRecorder は Reader を実装しないため、本 assertion は
//     必ず ok=false で nil を返す (Req 1.6)
//
// 1.5 単独では *SQLiteRecorder が Reader を満たさないため、本番 CLI 経路でも常に nil
// が返る。Reader 経由のクエリ到達は 2.5 完了後に成立する (タスク 1.5 の verification
// 「Reader nil ではない」は 2.x の段階的実装と組み合わせて満たされる)。
func asReader(r akashic.Recorder) akashic.Reader {
	if reader, ok := r.(akashic.Reader); ok {
		return reader
	}
	return nil
}

// asArtifactReader は recorder を read-only な akashic.ArtifactReader として
// ui.Options へ注入するヘルパ (akashic-mode-tui Task 9.1 / Req 11.3)。
// asReader と同じパターンで、ArtifactReader を実装する Recorder のみを
// ArtifactReader として返し、それ以外 (NoopRecorder fallback / nil 等) では
// nil を返す。
//
// nil 戻り値は TUI 側で「成果物データが利用できません」相当の graceful
// 表示にフォールバックする入力を作る (Req 11.3)。本番経路では
// *akashic.SQLiteRecorder が ArtifactReader を満たすため、SQLite open 成功時は
// 非 nil の ArtifactReader が ui.Options に渡る。NoopRecorder fallback 時
// (DB open 失敗) は nil が渡される。
//
// 設計判断: asReader と同様、interface 型 assertion (`r.(akashic.ArtifactReader)`)
// を使う。これにより:
//
//   - boundary 遵守: 9.1 のスコープは cmd/rilll/main.go に閉じる。
//     internal/akashic/ 配下に追加実装は不要
//   - NoopRecorder 経路: NoopRecorder は ArtifactReader を実装しないため、
//     本 assertion は必ず ok=false で nil を返す (Req 11.3)
func asArtifactReader(r akashic.Recorder) akashic.ArtifactReader {
	if reader, ok := r.(akashic.ArtifactReader); ok {
		return reader
	}
	return nil
}

// mergeWarnings は空文字列を除外した warning を改行で結合する。
// すべて空なら空文字列を返す (warningStyle が 1 行も表示しない条件と整合)。
func mergeWarnings(parts ...string) string {
	collected := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			collected = append(collected, p)
		}
	}
	return strings.Join(collected, "\n")
}

func versionOutput(args []string, buildVersion string) (string, bool) {
	if len(args) > 1 && args[1] == "--version" {
		return fmt.Sprintf("rilll v%s\n", buildVersion), true
	}
	return "", false
}
