package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/config"
	"github.com/kazuki-oshino/rilll/internal/provider"
)

// fakeAkashicReader は asReader の挙動検証用のテストダブル。
// akashic.NoopRecorder を埋め込み Recorder インターフェースを満たしつつ、
// Reader インターフェースの 5 メソッドを no-op で実装する。
//
// 1.5 時点で *akashic.SQLiteRecorder はまだ Reader を実装していない (タスク 2.1〜2.5 で
// 段階的に実装される) ため、asReader が「Reader を実装する Recorder」を非 nil で返す
// 経路の検証はテストダブル経由で行う。
type fakeAkashicReader struct {
	akashic.NoopRecorder
}

func (fakeAkashicReader) List(ctx context.Context, opts akashic.ListOptions) (akashic.ListResult, error) {
	return akashic.ListResult{}, nil
}
func (fakeAkashicReader) AggregateByWorkflow(ctx context.Context, opts akashic.WorkflowAggregateOptions) (akashic.WorkflowAggregateResult, error) {
	return akashic.WorkflowAggregateResult{}, nil
}
func (fakeAkashicReader) AggregateByProvider(ctx context.Context, opts akashic.ProviderAggregateOptions) (akashic.ProviderAggregateResult, error) {
	return akashic.ProviderAggregateResult{}, nil
}
func (fakeAkashicReader) Detail(ctx context.Context, opts akashic.DetailOptions) (akashic.RunDetail, error) {
	return akashic.RunDetail{}, nil
}
func (fakeAkashicReader) FailureClusters(ctx context.Context, opts akashic.FailureOptions) (akashic.FailureClustersResult, error) {
	return akashic.FailureClustersResult{}, nil
}
func (fakeAkashicReader) InRunResolutionsForCluster(ctx context.Context, opts akashic.InRunResolutionOptions) (akashic.InRunResolutionsResult, error) {
	return akashic.InRunResolutionsResult{}, nil
}
func (fakeAkashicReader) NextSuccessAcrossRuns(ctx context.Context, opts akashic.NextSuccessOptions) (akashic.NextSuccessResult, error) {
	return akashic.NextSuccessResult{}, nil
}

// AgentCallsByWorkflowRun / LearningsByWorkflowRun は agent-learnings spec (task 3.2)
// で Reader interface に追加されたメソッドへの no-op 追従。本 main_test は asReader 経路の
// 構造的検証が責務で、新 method の挙動は akashic パッケージ単体テストでカバーする。
func (fakeAkashicReader) AgentCallsByWorkflowRun(ctx context.Context, workflowRunID string) ([]akashic.AgentCallRow, error) {
	return nil, nil
}
func (fakeAkashicReader) LearningsByWorkflowRun(ctx context.Context, workflowRunID string) ([]akashic.LearningRow, error) {
	return nil, nil
}

// fakeAkashicArtifactReader は asArtifactReader の挙動検証用のテストダブル。
// akashic.NoopRecorder を埋め込み Recorder インターフェースを満たしつつ、
// ArtifactReader インターフェースの 3 メソッドを no-op で実装する。
//
// 本番経路では *akashic.SQLiteRecorder が ArtifactReader を満たしているが、
// NoopRecorder fallback 経路では満たさない。本ヘルパは「ArtifactReader を
// 実装する Recorder が渡されたとき非 nil を返す」経路をテストダブル経由で
// 検証する。
type fakeAkashicArtifactReader struct {
	akashic.NoopRecorder
}

func (fakeAkashicArtifactReader) GetArtifacts(ctx context.Context, workflowRunID string) ([]akashic.ArtifactRow, error) {
	return nil, nil
}
func (fakeAkashicArtifactReader) ReadArtifact(ctx context.Context, path string) ([]byte, error) {
	return nil, nil
}
func (fakeAkashicArtifactReader) ReadArtifactUnlimited(ctx context.Context, path string) ([]byte, error) {
	return nil, nil
}

func TestVersionOutputReturnsBuildVersionForVersionFlag(t *testing.T) {
	got, ok := versionOutput([]string{"rilll", "--version"}, "1.2.3")
	if !ok {
		t.Fatal("expected --version to be handled")
	}
	if got != "rilll v1.2.3\n" {
		t.Fatalf("unexpected version output: %q", got)
	}
}

func TestVersionOutputIgnoresOtherArgs(t *testing.T) {
	if got, ok := versionOutput([]string{"rilll"}, "1.2.3"); ok || got != "" {
		t.Fatalf("expected no version output, got %q ok=%v", got, ok)
	}
}

// 旧テスト名 (newObservabilityRecorder) を新名 newAkashicRecorder に追従。
// path 文字列も akashic.sqlite に揃える (Req 1.6 シンボル統一)。
func TestNewAkashicRecorderFallsBackToNoop(t *testing.T) {
	parentFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parentFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("failed to create parent file: %v", err)
	}

	recorder, warning := newAkashicRecorder(filepath.Join(parentFile, "akashic.sqlite"))
	defer recorder.Close()

	if _, ok := recorder.(akashic.NoopRecorder); !ok {
		t.Fatalf("expected noop recorder, got %T", recorder)
	}
	if !strings.Contains(warning, "観測データ") {
		t.Fatalf("expected fallback warning, got %q", warning)
	}
}

// mergeWarnings は空文字列を除外して改行で結合する仕様。
// loaded.Warning と migration / recorder の warning を 1 つの表示文字列に集約する用途。
func TestMergeWarningsJoinsNonEmptyWithNewline(t *testing.T) {
	got := mergeWarnings("config warn", "", "migration warn", "")
	want := "config warn\nmigration warn"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}

	if mergeWarnings("", "", "") != "" {
		t.Fatalf("expected empty when all empty")
	}

	if mergeWarnings("only") != "only" {
		t.Fatalf("expected single warning to pass through")
	}
}

func TestProviderModelDefaultsFromConfigUsesCodexPreset(t *testing.T) {
	got := providerModelDefaultsFromConfig(config.Config{
		AgentProvider: provider.ProviderCodex,
		AkashicLevel:  config.AkashicSummary,
		Codex: config.CodexConfig{
			ModelPreset:      config.CodexModelPresetGPT6LunaMax,
			ReasoningSummary: config.CodexReasoningSummaryDetailed,
		},
	})
	codex := got[provider.ProviderCodex]
	if codex.Model != "gpt-6-luna" || codex.Effort != "max" {
		t.Fatalf("codex defaults mismatch: %#v", got)
	}
	if codex.ReasoningSummary != "detailed" {
		t.Fatalf("codex reasoning summary default mismatch: %#v", got)
	}
	if _, ok := got[provider.ProviderClaude]; ok {
		t.Fatalf("non-codex provider should not receive model defaults: %#v", got)
	}
}

// Cursor SDK 用 preset も Codex と同じ map 経由で provider に注入される。
// composer-2.5 は Effort が空文字で、bridge.Request.Effort の omitempty 経由で wire から消える。
func TestProviderModelDefaultsFromConfigUsesCursorSDKPreset(t *testing.T) {
	t.Run("gpt-5.5-xhigh", func(t *testing.T) {
		got := providerModelDefaultsFromConfig(config.Config{
			AgentProvider: provider.ProviderCursorSDK,
			AkashicLevel:  config.AkashicSummary,
			CursorSDK:     config.CursorSDKConfig{ModelPreset: config.CursorSDKModelPresetGPT55XHigh},
		})
		cs := got[provider.ProviderCursorSDK]
		if cs.Model != "gpt-5.5" || cs.Effort != "xhigh" {
			t.Fatalf("cursor-sdk defaults mismatch: %#v", got)
		}
	})
	t.Run("composer-2.5 keeps effort empty and uses fast=false", func(t *testing.T) {
		got := providerModelDefaultsFromConfig(config.Config{
			AgentProvider: provider.ProviderCursorSDK,
			AkashicLevel:  config.AkashicSummary,
			CursorSDK:     config.CursorSDKConfig{ModelPreset: config.CursorSDKModelPresetComposer25},
		})
		cs := got[provider.ProviderCursorSDK]
		if cs.Model != "composer-2.5" {
			t.Fatalf("expected composer-2.5 model, got %#v", cs)
		}
		if cs.Effort != "" {
			t.Fatalf("composer-2.5 must keep effort empty so wire omits it, got %q", cs.Effort)
		}
		if len(cs.ModelParams) != 1 || cs.ModelParams[0].ID != "fast" || cs.ModelParams[0].Value != "false" {
			t.Fatalf("composer-2.5 must use fast=false, got %#v", cs.ModelParams)
		}
	})
	t.Run("composer-2.5-fast omits model params", func(t *testing.T) {
		got := providerModelDefaultsFromConfig(config.Config{
			AgentProvider: provider.ProviderCursorSDK,
			AkashicLevel:  config.AkashicSummary,
			CursorSDK:     config.CursorSDKConfig{ModelPreset: config.CursorSDKModelPresetComposer25Fast},
		})
		cs := got[provider.ProviderCursorSDK]
		if cs.Model != "composer-2.5" {
			t.Fatalf("expected composer-2.5 model, got %#v", cs)
		}
		if cs.Effort != "" {
			t.Fatalf("composer-2.5-fast must keep effort empty, got %q", cs.Effort)
		}
		if len(cs.ModelParams) != 0 {
			t.Fatalf("composer-2.5-fast must omit model params, got %#v", cs.ModelParams)
		}
	})
	t.Run("grok-4.5-fast maps to grok-4.5 default variant", func(t *testing.T) {
		got := providerModelDefaultsFromConfig(config.Config{
			AgentProvider: provider.ProviderCursorSDK,
			AkashicLevel:  config.AkashicSummary,
			CursorSDK:     config.CursorSDKConfig{ModelPreset: config.CursorSDKModelPresetGrok45Fast},
		})
		cs := got[provider.ProviderCursorSDK]
		if cs.Model != "grok-4.5" {
			t.Fatalf("expected grok-4.5 model, got %#v", cs)
		}
		if cs.Effort != "" {
			t.Fatalf("grok-4.5-fast must keep effort empty so default variant is used, got %q", cs.Effort)
		}
	})
}

func TestShutdownTelemetryUsesBoundedContext(t *testing.T) {
	var sawDeadline atomic.Bool
	err := shutdownTelemetry(func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if ok && time.Until(deadline) <= telemetryShutdownTimeout {
			sawDeadline.Store(true)
		}
		<-ctx.Done()
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("expected deadline cancellation error")
	}
	if !sawDeadline.Load() {
		t.Fatal("telemetry shutdown should receive a bounded context")
	}
}

// 旧 observability.sqlite を事前配置 → initAkashicRecorder() で
// 自動移行が走り、移行通知 warning が返り値に含まれることを確認 (Req 2.4, 2.9)。
// これは task 3 で要求される「DB パスメッセージが akashic.sqlite を指していることを
// 目視確認できる出力形式」の証跡となる。
func TestInitAkashicRecorderMigratesLegacyFile(t *testing.T) {
	dir := t.TempDir()
	newPath := filepath.Join(dir, "akashic.sqlite")
	oldPath := filepath.Join(dir, "observability.sqlite")

	// 旧 path に有効な SQLite ファイルを作成 (OpenSQLite で初期化)。
	seed, err := akashic.OpenSQLite(oldPath)
	if err != nil {
		t.Fatalf("failed to seed legacy DB: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("failed to close seed DB: %v", err)
	}

	// 環境変数で新 path を強制 → ResolveDBPath が newPath を返し、
	// Migrate が旧→新 rename を実行する。
	t.Setenv(akashic.DBEnvName, newPath)
	t.Setenv(akashic.LegacyDBEnvName, "")

	recorder, warning := initAkashicRecorder()
	defer recorder.Close()

	// 1) recorder は SQLiteRecorder (Noop ではない) であること
	if _, ok := recorder.(akashic.NoopRecorder); ok {
		t.Fatalf("expected SQLiteRecorder after successful migration, got NoopRecorder. warning=%q", warning)
	}

	// 2) 旧 path は消え、新 path に移行されている
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("expected migrated file at new path: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("expected legacy file to be moved away, got err %v", err)
	}

	// 3) 移行通知 warning が UI に伝播する形 (旧 / 新 path を含む 1 行) で返ること
	//    Req 2.4: 1 行ログでユーザに通知 / Req 2.9: akashic.sqlite を指す path
	if !strings.Contains(warning, "移行しました") {
		t.Fatalf("expected migration notice in warning, got %q", warning)
	}
	if !strings.Contains(warning, oldPath) || !strings.Contains(warning, newPath) {
		t.Fatalf("warning should contain both old and new paths, got %q", warning)
	}
	if !strings.Contains(warning, "akashic.sqlite") {
		t.Fatalf("warning should mention akashic.sqlite path, got %q", warning)
	}

	// 4) recorder が実際に書き込み可能であること (= SQLiteRecorder が動作中)
	sqliteRec, ok := recorder.(*akashic.SQLiteRecorder)
	if !ok {
		t.Fatalf("expected *akashic.SQLiteRecorder, got %T", recorder)
	}
	if err := sqliteRec.RecordWorkflowRunStart(context.Background(), akashic.WorkflowRun{
		ID:           "run-1",
		WorkflowID:   "wf",
		WorkflowName: "Test",
		CWD:          dir,
	}); err != nil {
		t.Fatalf("failed to record after migration: %v", err)
	}
}

// asReader は cli.Deps.Reader フィールドへ注入する読取契約を選別するヘルパ (Req 1.4 / 1.6)。
// Reader を実装する Recorder のときに interface 値を返し、それ以外 (NoopRecorder fallback /
// nil 等) では nil を返すことで、CLI 経路の runAkashic が「akashic データベースが利用できません」
// を表示できる入力を作る。
//
// 1.5 時点では *akashic.SQLiteRecorder の Reader メソッド群が未実装のため、本番経路では
// 必ず nil 戻りになる。タスク 2.1〜2.5 で SQLiteRecorder の Reader 実装が揃うと、本ヘルパは
// 自動的に Reader を返すようになる (実装側に手を入れずに動作する設計)。
func TestAsReaderReturnsNilForNoopRecorder(t *testing.T) {
	if got := asReader(akashic.NoopRecorder{}); got != nil {
		t.Fatalf("expected nil Reader for NoopRecorder, got %T", got)
	}
}

func TestAsReaderReturnsNilForNilRecorder(t *testing.T) {
	if got := asReader(nil); got != nil {
		t.Fatalf("expected nil Reader for nil recorder, got %T", got)
	}
}

// Reader インターフェースを満たす Recorder が渡されたときは、asReader が同じ参照を
// Reader として返すこと。これにより、タスク 2.5 完了後に *SQLiteRecorder が Reader
// を満たすようになった瞬間、本番 CLI 経路でも自動的に Reader が cli.Deps へ注入される。
func TestAsReaderReturnsReaderForReaderImplementingRecorder(t *testing.T) {
	fake := fakeAkashicReader{}
	got := asReader(fake)
	if got == nil {
		t.Fatalf("expected non-nil Reader for Reader-implementing recorder, got nil")
	}
	// 同じインスタンスが返ることを assert (interface 値の動的型比較)。
	if _, ok := got.(fakeAkashicReader); !ok {
		t.Fatalf("expected got to be fakeAkashicReader, got %T", got)
	}
}

// asArtifactReader は recorder を read-only な akashic.ArtifactReader として
// ui.Options へ注入するヘルパ (akashic-mode-tui Task 9.1 / Req 11.3)。
// ArtifactReader を実装する Recorder のみを ArtifactReader として返し、
// それ以外 (NoopRecorder fallback / nil 等) では nil を返す。
//
// 本番経路では *akashic.SQLiteRecorder が ArtifactReader を満たすため、
// SQLite open 成功時は非 nil の ArtifactReader が ui.Options に渡る。
// NoopRecorder fallback 時は nil が渡され、TUI 側の graceful 表示
// (「成果物データが利用できません」) 経路にフォールバックする (Req 11.3)。
func TestAsArtifactReaderReturnsNilForNoopRecorder(t *testing.T) {
	if got := asArtifactReader(akashic.NoopRecorder{}); got != nil {
		t.Fatalf("expected nil ArtifactReader for NoopRecorder, got %T", got)
	}
}

func TestAsArtifactReaderReturnsNilForNilRecorder(t *testing.T) {
	if got := asArtifactReader(nil); got != nil {
		t.Fatalf("expected nil ArtifactReader for nil recorder, got %T", got)
	}
}

// ArtifactReader インターフェースを満たす Recorder が渡されたときは、
// asArtifactReader が同じ参照を ArtifactReader として返すこと。
// これにより、本番経路で *SQLiteRecorder が渡されたときに ui.Options
// 経由で TUI が成果物にアクセスできるようになる。
func TestAsArtifactReaderReturnsArtifactReaderForArtifactReaderImplementingRecorder(t *testing.T) {
	fake := fakeAkashicArtifactReader{}
	got := asArtifactReader(fake)
	if got == nil {
		t.Fatalf("expected non-nil ArtifactReader for ArtifactReader-implementing recorder, got nil")
	}
	if _, ok := got.(fakeAkashicArtifactReader); !ok {
		t.Fatalf("expected got to be fakeAkashicArtifactReader, got %T", got)
	}
}
