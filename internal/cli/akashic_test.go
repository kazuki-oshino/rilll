package cli

import (
	"bytes"
	"context"
	"database/sql"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

// task 1.4 router behavior tests.
//
// 4.7 で table-driven 形式に拡張する想定。本ファイル冒頭は 1.4 の RED → GREEN を
// 駆動するための最小カバレッジに留め、4.7 がさらにケースを追加する。

// updateGoldens は -update フラグで指定されると testdata/ 配下の golden file を
// 現在の出力で書き換える (task 4.6)。
//
// 使い方:
//
//	go test ./internal/cli/ -run TestRunAkashicExport -update
//
// 通常テスト時は false。CI では -update を渡さないので golden 突合が行われる。
var updateGoldens = flag.Bool("update", false, "regenerate golden files under internal/cli/testdata/")

func TestIsSubcommandRecognizesAkashic(t *testing.T) {
	if !IsSubcommand([]string{"rilll", "akashic"}) {
		t.Errorf("IsSubcommand should recognize 'akashic' as a subcommand")
	}
	if !IsSubcommand([]string{"rilll", "akashic", "export"}) {
		t.Errorf("IsSubcommand should recognize 'akashic export' as a subcommand")
	}
	// 既存 TUI 経路の非破壊性
	if IsSubcommand([]string{"rilll"}) {
		t.Errorf("IsSubcommand should NOT match 'rilll' alone")
	}
	if IsSubcommand([]string{"rilll", "--version"}) {
		t.Errorf("IsSubcommand should NOT match '--version'")
	}
}

func TestDispatchAkashicNoOpExitsTwoWithStderrHelp(t *testing.T) {
	deps, _, stderr := defaultDeps(nil)
	exit := Dispatch(context.Background(), []string{"rilll", "akashic"}, deps)
	if exit != ExitUsageError {
		t.Fatalf("expected exit %d for 'rilll akashic', got %d. stderr=%s", ExitUsageError, exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "akashic") {
		t.Errorf("expected stderr to include akashic help, got:\n%s", stderr.String())
	}
}

func TestDispatchAkashicUnknownOpExitsTwo(t *testing.T) {
	deps, _, stderr := defaultDeps(nil)
	exit := Dispatch(context.Background(), []string{"rilll", "akashic", "weird-op"}, deps)
	if exit != ExitUsageError {
		t.Fatalf("expected exit %d for unknown akashic op, got %d. stderr=%s", ExitUsageError, exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "weird-op") {
		t.Errorf("expected stderr to echo unknown op, got %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "未知") {
		t.Errorf("expected stderr to mention 未知, got %s", stderr.String())
	}
}

func TestDispatchAkashicHelpExitsZeroOnStdout(t *testing.T) {
	for _, op := range []string{"--help", "-h", "help"} {
		t.Run(op, func(t *testing.T) {
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}
			deps := Deps{Stdout: stdout, Stderr: stderr}
			exit := Dispatch(context.Background(), []string{"rilll", "akashic", op}, deps)
			if exit != ExitOK {
				t.Fatalf("expected exit 0 for 'rilll akashic %s', got %d. stderr=%s", op, exit, stderr.String())
			}
			if !strings.Contains(stdout.String(), "akashic") {
				t.Errorf("expected stdout to include akashic help, got:\n%s", stdout.String())
			}
		})
	}
}

func TestDispatchAkashicExportReaderNilExitsOne(t *testing.T) {
	deps, _, stderr := defaultDeps(nil)
	// Reader is intentionally not set (nil) to simulate NoopRecorder fallback
	exit := Dispatch(context.Background(), []string{"rilll", "akashic", "export"}, deps)
	if exit != ExitRunError {
		t.Fatalf("expected exit %d when Reader is nil, got %d. stderr=%s", ExitRunError, exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "akashic データベースが利用できません") {
		t.Errorf("expected stderr to include 'akashic データベースが利用できません', got:\n%s", stderr.String())
	}
}

// ---------------------------------------------------------------------------
// task 4.6 export section: `rilll akashic export` の golden 突合テスト
// ---------------------------------------------------------------------------
//
// 目的 (Req 6.6, 6.10, 7.6, 8.2, 8.4):
//   - fakeReader (akashic_export_test.go 同 package) に固定 fixture を返させ、
//     runAkashicExport の stdout を internal/cli/testdata/ 配下の golden file と
//     byte 単位で突合する。
//   - --pretty 有無で 2 つの golden を生成・比較する。
//   - 不正フラグ (--limit=-1 / --success=foo / --since=invalid-date) で
//     exit 2 + stderr メッセージを assert する。
//   - フラグ未指定 (既定 limit 適用) の golden 比較。
//   - AgentCalls が空の run の出力が "agent_calls": [] (null ではない) になる。
//
// fixture の安定性:
//   - goldenFixture() は決定的な値で構成し、後方互換テストの基盤になるため
//     既存値を変えるときは golden file の更新 (-update) と下流影響レビューが必要
//     (Revalidation Triggers 参照)。
//   - 時刻はすべて UTC で固定し、time.RFC3339Nano フォーマットの安定性に依存する。

// goldenFixture は default / pretty 両 golden で共通利用する固定 fixture である。
// 並びは List 規約 (started_at DESC, id DESC) に揃えて run-003 (最新, in-progress)
// → run-001 (中間, success) → run-002 (最古, failure) の順で構築する。
//
// 含めるバリエーション:
//   - run-003: in-progress (Success=nil / FinishedAt=nil) + AgentCalls=nil
//     → JSON で success/finished_at が null、agent_calls が [] になることを検証
//   - run-001: success + 2 agent_calls (claude / codex) + フル数値フィールド
//     → 全フィールドが非 null で出力されることを検証
//   - run-002: failure + 1 agent_call + error / failure_category 設定
//     → エラー文字列とカテゴリが出力されることを検証
func goldenFixture() akashic.ListResult {
	loc := time.UTC
	started1 := time.Date(2024, 1, 1, 9, 0, 0, 0, loc)
	finished1 := time.Date(2024, 1, 1, 9, 5, 0, 0, loc)
	started1ac1 := time.Date(2024, 1, 1, 9, 0, 30, 0, loc)
	finished1ac1 := time.Date(2024, 1, 1, 9, 1, 0, 0, loc)
	started1ac2 := time.Date(2024, 1, 1, 9, 1, 30, 0, loc)
	finished1ac2 := time.Date(2024, 1, 1, 9, 4, 30, 0, loc)
	started2 := time.Date(2024, 1, 1, 8, 0, 0, 0, loc)
	finished2 := time.Date(2024, 1, 1, 8, 1, 0, 0, loc)
	started2ac1 := time.Date(2024, 1, 1, 8, 0, 30, 0, loc)
	finished2ac1 := time.Date(2024, 1, 1, 8, 0, 50, 0, loc)
	started3 := time.Date(2024, 1, 1, 10, 0, 0, 0, loc)

	successTrue := true
	successFalse := false
	cost1 := 0.012
	cost2 := 0.034
	costFail := 0.001
	durFast := int64(30000)
	durSlow := int64(180000)
	durFail := int64(20000)
	in1 := int64(1500)
	out1 := int64(800)
	in2 := int64(2500)
	out2 := int64(1200)
	inFail := int64(500)
	outFail := int64(0)

	return akashic.ListResult{
		Items: []akashic.WorkflowRunListItem{
			// 最新: in-progress (Success=nil, FinishedAt=nil, AgentCalls=nil)。
			// JSON 出力で success/finished_at が null、agent_calls が [] になる。
			{
				RunHeader: akashic.RunHeader{
					RunID:        "run-003",
					WorkflowID:   "git-commit",
					WorkflowName: "Git Commit",
					CWD:          "/repo",
					StartedAt:    started3,
					// FinishedAt: nil (未完了)
					// Success:    nil (未確定)
				},
				AgentCalls: nil,
			},
			// 中間: success + 2 agent_calls (claude / codex)。
			{
				RunHeader: akashic.RunHeader{
					RunID:        "run-001",
					WorkflowID:   "git-commit",
					WorkflowName: "Git Commit",
					CWD:          "/repo",
					StartedAt:    started1,
					FinishedAt:   &finished1,
					Success:      &successTrue,
				},
				AgentCalls: []akashic.AgentCallSummary{
					{
						ID:           "ac-001",
						ProviderID:   "claude",
						StepID:       "step-1",
						StepName:     "Propose",
						Success:      &successTrue,
						DurationMS:   &durFast,
						TotalCostUSD: &cost1,
						InputTokens:  &in1,
						OutputTokens: &out1,
						StartedAt:    started1ac1,
						FinishedAt:   &finished1ac1,
					},
					{
						ID:           "ac-002",
						ProviderID:   "codex",
						StepID:       "step-2",
						StepName:     "Review",
						Success:      &successTrue,
						DurationMS:   &durSlow,
						TotalCostUSD: &cost2,
						InputTokens:  &in2,
						OutputTokens: &out2,
						StartedAt:    started1ac2,
						FinishedAt:   &finished1ac2,
					},
				},
			},
			// 最古: failure + 1 agent_call + error / failure_category。
			{
				RunHeader: akashic.RunHeader{
					RunID:        "run-002",
					WorkflowID:   "kiro-impl",
					WorkflowName: "Kiro Impl",
					CWD:          "/repo",
					StartedAt:    started2,
					FinishedAt:   &finished2,
					Success:      &successFalse,
					Error:        "agent_call timed out",
				},
				AgentCalls: []akashic.AgentCallSummary{
					{
						ID:              "ac-003",
						ProviderID:      "claude",
						StepID:          "step-1",
						StepName:        "Implement",
						Success:         &successFalse,
						DurationMS:      &durFail,
						TotalCostUSD:    &costFail,
						InputTokens:     &inFail,
						OutputTokens:    &outFail,
						FailureCategory: "timeout",
						Error:           "agent_call timed out",
						StartedAt:       started2ac1,
						FinishedAt:      &finished2ac1,
					},
				},
			},
		},
		Limit:      50,
		Offset:     0,
		NextOffset: nil,
	}
}

// assertGolden は got と path 配下の golden file を byte 単位で比較する。
// -update フラグ指定時は got で path を上書きする (golden 再生成)。
// path はリポジトリルート相対ではなく package カレント相対 (例: "testdata/foo.json")。
func assertGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir failed: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden %s failed: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s failed: %v\n(generate via: go test ./internal/cli/ -run %s -update)", path, err, t.Name())
	}
	if !bytes.Equal(got, want) {
		t.Errorf("golden mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", path, string(got), string(want))
	}
}

// TestRunAkashicExport_DefaultGolden は --pretty 無し (json.Encoder 経路) の出力を
// testdata/akashic_export_default.json と byte 単位で突合する (Req 6.6, 6.10, 8.4)。
//
// この golden は task 4.6 の決定的フォーマット契約を表明する gate であり、
// 出力スキーマ (フィールド名 / 並び順 / 時刻フォーマット / nil slice → []) を
// 後方互換のフィールド追加のみ許容する形で固定する (Revalidation Triggers 参照)。
func TestRunAkashicExport_DefaultGolden(t *testing.T) {
	reader := &fakeReader{listResult: goldenFixture()}
	deps, stdout, stderr := depsWithReader(reader)

	if exit := runAkashicExport(context.Background(), nil, deps); exit != ExitOK {
		t.Fatalf("expected exit 0, got %d. stderr=%s", exit, stderr.String())
	}
	assertGolden(t, "testdata/akashic_export_default.json", stdout.Bytes())
}

// TestRunAkashicExport_PrettyGolden は --pretty (json.MarshalIndent 経路) の出力を
// testdata/akashic_export_pretty.json と byte 単位で突合する (Req 6.7, 6.10, 8.4)。
func TestRunAkashicExport_PrettyGolden(t *testing.T) {
	reader := &fakeReader{listResult: goldenFixture()}
	deps, stdout, stderr := depsWithReader(reader)

	if exit := runAkashicExport(context.Background(), []string{"--pretty"}, deps); exit != ExitOK {
		t.Fatalf("expected exit 0, got %d. stderr=%s", exit, stderr.String())
	}
	assertGolden(t, "testdata/akashic_export_pretty.json", stdout.Bytes())
}

// TestRunAkashicExport_AgentCallsEmptyEmitsArrayInGolden は default golden 内の
// in-progress run (run-003, AgentCalls=nil) の出力が "agent_calls": [] になり、
// "agent_calls": null は一切出ないことを assert する (Req 6.10)。
//
// golden file 突合 (TestRunAkashicExport_DefaultGolden) でも担保されるが、
// 「nil slice → 空配列 [] 出力」契約を独立した assert として明示する。
func TestRunAkashicExport_AgentCallsEmptyEmitsArrayInGolden(t *testing.T) {
	reader := &fakeReader{listResult: goldenFixture()}
	deps, stdout, _ := depsWithReader(reader)
	if exit := runAkashicExport(context.Background(), nil, deps); exit != ExitOK {
		t.Fatalf("unexpected exit code: %d", exit)
	}
	out := stdout.String()
	if !strings.Contains(out, `"workflow_run_id":"run-003"`) {
		t.Fatalf("fixture missing run-003: %s", out)
	}
	if strings.Contains(out, `"agent_calls":null`) {
		t.Errorf("agent_calls should never be null in JSON output: %s", out)
	}
	if !strings.Contains(out, `"agent_calls":[]`) {
		t.Errorf("expected agent_calls:[] in output, got: %s", out)
	}
}

// ---------------------------------------------------------------------------
// task 4.7 router section: `runAkashic` ルータの完成形 table-driven テスト
// ---------------------------------------------------------------------------
//
// 目的 (Req 1.6, 6.1, 6.11, 6.12, 8.1):
//   - task 1.4 で書かれた 4 つの bara バラ TestDispatchAkashic* を補完し、
//     `runAkashic` ルータを直接呼ぶ table-driven 形式の完成形テストを提供する。
//   - 既存 TestDispatchAkashic* (Dispatch 経由) は assertion 減らさない原則で
//     そのまま維持し、本テストはルータの直接検証で相補的にカバレッジを上げる。
//   - 既存 dispatch テスト (TestIsSubcommand を含む) を Reader: nil 経路で
//     動かす際の非破壊性を保証する (Req 8.1)。
//
// カバーする経路:
//   - args == [] (op なし)             → exit 2 + stderr にヘルプ (Req 6.12)
//   - args == ["weird-op"]             → exit 2 + stderr に未知サブコマンド (Req 6.12)
//   - args == ["--help"|"-h"|"help"]   → exit 0 + stdout にヘルプ
//   - args == ["export"] (Reader=nil)  → exit 1 + stderr に
//     「akashic データベースが利用できません」(Req 1.6)
//
// 設計判断:
//   - runAkashic を直接呼ぶことで Dispatch 経由の switch 配線とは独立にルータ自体の
//     挙動を検証する。Dispatch 配線の非破壊性は既存 TestDispatchAkashic* と
//     TestIsSubcommand (dispatch_test.go) で担保される。
//   - サブコマンド未指定 (`rilll` 単独実行) が TUI 経路に流れる非破壊性は
//     TestIsSubcommand の `{[]string{"rilll"}, false}` ケースで担保済み (本タスク 4.7
//     では dispatch_test.go の TestIsSubcommand に akashic / akashic export の
//     新規ケースを追加して、CLI 経路に流れる側の網羅性も増やしている)。
func TestRunAkashicRouter(t *testing.T) {
	cases := []struct {
		name             string
		args             []string
		wantExit         int
		wantStdoutSubstr []string
		wantStderrSubstr []string
	}{
		{
			// Req 6.12: op なしは usage error として ヘルプを stderr に出す。
			name:             "no_op_exits_two_with_stderr_help",
			args:             []string{},
			wantExit:         ExitUsageError,
			wantStderrSubstr: []string{"akashic", "Usage:"},
		},
		{
			// Req 6.12: 未知の akashic サブコマンドは usage error。
			name:             "unknown_op_exits_two_with_stderr_unknown",
			args:             []string{"weird-op"},
			wantExit:         ExitUsageError,
			wantStderrSubstr: []string{"weird-op", "未知"},
		},
		{
			name:             "double_dash_help_exits_zero_on_stdout",
			args:             []string{"--help"},
			wantExit:         ExitOK,
			wantStdoutSubstr: []string{"akashic", "Usage:"},
		},
		{
			name:             "short_help_exits_zero_on_stdout",
			args:             []string{"-h"},
			wantExit:         ExitOK,
			wantStdoutSubstr: []string{"akashic", "Usage:"},
		},
		{
			name:             "help_word_exits_zero_on_stdout",
			args:             []string{"help"},
			wantExit:         ExitOK,
			wantStdoutSubstr: []string{"akashic", "Usage:"},
		},
		{
			// Req 1.6: NoopRecorder fallback (Reader=nil) で export を呼ぶと、
			// CLI は exit 1 + stderr の固定メッセージを返す (Req 8.1: 既存
			// テスト経路を壊さない設計)。
			name:             "export_with_nil_reader_exits_one_with_message",
			args:             []string{"export"},
			wantExit:         ExitRunError,
			wantStderrSubstr: []string{"akashic データベースが利用できません"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}
			deps := Deps{Stdout: stdout, Stderr: stderr} // Reader: nil 経路
			exit := runAkashic(context.Background(), c.args, deps)
			if exit != c.wantExit {
				t.Fatalf("runAkashic(%v) exit = %d, want %d. stdout=%s stderr=%s",
					c.args, exit, c.wantExit, stdout.String(), stderr.String())
			}
			for _, want := range c.wantStdoutSubstr {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("expected stdout to contain %q, got:\n%s", want, stdout.String())
				}
			}
			for _, want := range c.wantStderrSubstr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("expected stderr to contain %q, got:\n%s", want, stderr.String())
				}
			}
		})
	}
}

// TestRunAkashicRouter_TUIPathIsNotBroken は `rilll` 単独実行 (サブコマンド未指定)
// が TUI 経路に流れる非破壊性を表明する (Req 6.11)。
//
// IsSubcommand が `[]string{"rilll"}` で false を返すことが TUI 起動経路の
// gate であり、本テストはタスク 4.7 のチェックリスト「サブコマンド未指定が
// TUI 経路に流れることを IsSubcommand テストで確認する」を独立した assert
// として明示する (TestIsSubcommand のテーブル内ケースと冗長だが、Req 6.11 の
// non-regression を任意の改修で外しにくくする gate として残す)。
func TestRunAkashicRouter_TUIPathIsNotBroken(t *testing.T) {
	if IsSubcommand([]string{"rilll"}) {
		t.Errorf("Req 6.11 違反: `rilll` 単独実行が CLI 経路に分類されてしまっている (TUI 経路に流れる必要がある)")
	}
	if IsSubcommand([]string{"rilll", "--version"}) {
		t.Errorf("Req 6.11 違反: `rilll --version` が CLI 経路に分類されている (cmd/rilll 側で従来どおり処理する想定)")
	}
}

// ---------------------------------------------------------------------------
// task 5.1 e2e section: 実 DB Insert → Reader → CLI export → golden 比較
// ---------------------------------------------------------------------------
//
// 目的 (Req 8.2, 8.4):
//   - akashic.OpenSQLite で実 DB を t.TempDir() 上に作成し、Recorder 経由で
//     fixture を insert する。
//   - 同 *SQLiteRecorder を Reader として runAkashicExport に注入し、stdout JSON を
//     internal/cli/testdata/akashic_export_e2e.json と byte 単位で突合する。
//   - fake Reader (4.6) と異なり、SQL 発行 → scan → JSON エンコードの全層を貫く
//     e2e 検証である。スキーマ定義 / Reader SQL / Go 側 group / encoding/json まで
//     一気通貫で回帰防止する。
//
// MaxOpenConns(1) + WAL + busy_timeout=5000 環境で Recorder と Reader が同じ
// *sql.DB を共有しても読み書きが競合しないことを、Recorder で fixture insert →
// 同 *SQLiteRecorder を Reader として export する経路で確認する。
//
// fixture 設計 (4.6 の goldenFixture とは独立に定義):
//   - run-e2e-001: success + 2 agent_calls (claude / codex) → 全フィールド非 null
//   - run-e2e-002: failure + 1 agent_call (failure_category=timeout / error あり)
//   - run-e2e-003: in-progress (FinishedAt=null / Success=null) + AgentCalls=[]
//
// golden の決定論性:
//   - 時刻はすべて UTC + zero nanosecond で固定 (RFC3339Nano が "Z" 終端の安定形に潰れる)
//   - id / step_id / step_name / provider_id / cost / tokens / duration_ms / cwd は
//     人間可読な固定値で構成
//   - List 既定 limit=50 が適用された JSON 出力 ("limit":50, "next_offset":null)

// insertE2EFixture は実 SQLiteRecorder に対して run-e2e-001 / run-e2e-002 / run-e2e-003 の
// 3 件の workflow_run と紐づく agent_calls を Recorder メソッド経由で insert する。
//
// Recorder 経路:
//  1. RecordWorkflowRunStart で workflow_run スカラ列を入れる
//  2. RecordAgentCallStart で agent_call の入力側カラム (provider_id / step / started_at 等) を入れる
//  3. RecordAgentCallFinish で agent_call の結果側カラム (success / duration / cost / tokens /
//     error / failure_category / finished_at) を入れる
//  4. (run が完了した場合のみ) RecordWorkflowRunFinish で finished_at / error を入れる
//
// 並びは List 規約 (started_at DESC) に揃えるが、insert 順は実装非依存にするため意図的に
// 異なる順 (run-e2e-001 → run-e2e-002 → run-e2e-003) で投入する。
func insertE2EFixture(t *testing.T, ctx context.Context, recorder *akashic.SQLiteRecorder) {
	t.Helper()
	loc := time.UTC

	// ---- run-e2e-001: success + 2 agent_calls (claude / codex) ----
	r1Started := time.Date(2024, 1, 15, 10, 0, 0, 0, loc)
	r1Finished := time.Date(2024, 1, 15, 10, 5, 0, 0, loc)
	if err := recorder.RecordWorkflowRunStart(ctx, akashic.WorkflowRun{
		ID:           "run-e2e-001",
		WorkflowID:   "git-commit",
		WorkflowName: "Git Commit",
		CWD:          "/repo",
		StartedAt:    r1Started,
	}); err != nil {
		t.Fatalf("RecordWorkflowRunStart run-e2e-001 failed: %v", err)
	}

	r1ac1Started := time.Date(2024, 1, 15, 10, 0, 30, 0, loc)
	r1ac1Finished := time.Date(2024, 1, 15, 10, 1, 0, 0, loc)
	if err := recorder.RecordAgentCallStart(ctx, akashic.AgentCall{
		ID:            "ac-e2e-001",
		WorkflowRunID: "run-e2e-001",
		WorkflowID:    "git-commit",
		WorkflowName:  "Git Commit",
		StepID:        "step-1",
		StepName:      "Propose",
		ProviderID:    "claude",
		Prompt:        "propose commit message",
		CWD:           "/repo",
		StartedAt:     r1ac1Started,
	}); err != nil {
		t.Fatalf("RecordAgentCallStart ac-e2e-001 failed: %v", err)
	}
	if err := recorder.RecordAgentCallFinish(ctx, akashic.AgentCallResult{
		ID:           "ac-e2e-001",
		Success:      true,
		DurationMS:   30000,
		TotalCostUSD: sql.NullFloat64{Float64: 0.012, Valid: true},
		InputTokens:  sql.NullInt64{Int64: 1500, Valid: true},
		OutputTokens: sql.NullInt64{Int64: 800, Valid: true},
		FinishedAt:   r1ac1Finished,
	}); err != nil {
		t.Fatalf("RecordAgentCallFinish ac-e2e-001 failed: %v", err)
	}

	r1ac2Started := time.Date(2024, 1, 15, 10, 1, 30, 0, loc)
	r1ac2Finished := time.Date(2024, 1, 15, 10, 4, 30, 0, loc)
	if err := recorder.RecordAgentCallStart(ctx, akashic.AgentCall{
		ID:            "ac-e2e-002",
		WorkflowRunID: "run-e2e-001",
		WorkflowID:    "git-commit",
		WorkflowName:  "Git Commit",
		StepID:        "step-2",
		StepName:      "Review",
		ProviderID:    "codex",
		Prompt:        "review proposal",
		CWD:           "/repo",
		StartedAt:     r1ac2Started,
	}); err != nil {
		t.Fatalf("RecordAgentCallStart ac-e2e-002 failed: %v", err)
	}
	if err := recorder.RecordAgentCallFinish(ctx, akashic.AgentCallResult{
		ID:           "ac-e2e-002",
		Success:      true,
		DurationMS:   180000,
		TotalCostUSD: sql.NullFloat64{Float64: 0.034, Valid: true},
		InputTokens:  sql.NullInt64{Int64: 2500, Valid: true},
		OutputTokens: sql.NullInt64{Int64: 1200, Valid: true},
		FinishedAt:   r1ac2Finished,
	}); err != nil {
		t.Fatalf("RecordAgentCallFinish ac-e2e-002 failed: %v", err)
	}
	if err := recorder.RecordWorkflowRunFinish(ctx, akashic.WorkflowRunFinish{
		ID:         "run-e2e-001",
		FinishedAt: r1Finished,
	}); err != nil {
		t.Fatalf("RecordWorkflowRunFinish run-e2e-001 failed: %v", err)
	}

	// ---- run-e2e-002: failure + 1 agent_call ----
	r2Started := time.Date(2024, 1, 15, 9, 0, 0, 0, loc)
	r2Finished := time.Date(2024, 1, 15, 9, 1, 0, 0, loc)
	if err := recorder.RecordWorkflowRunStart(ctx, akashic.WorkflowRun{
		ID:           "run-e2e-002",
		WorkflowID:   "kiro-impl",
		WorkflowName: "Kiro Impl",
		CWD:          "/repo",
		StartedAt:    r2Started,
	}); err != nil {
		t.Fatalf("RecordWorkflowRunStart run-e2e-002 failed: %v", err)
	}

	r2ac1Started := time.Date(2024, 1, 15, 9, 0, 30, 0, loc)
	r2ac1Finished := time.Date(2024, 1, 15, 9, 0, 50, 0, loc)
	if err := recorder.RecordAgentCallStart(ctx, akashic.AgentCall{
		ID:            "ac-e2e-003",
		WorkflowRunID: "run-e2e-002",
		WorkflowID:    "kiro-impl",
		WorkflowName:  "Kiro Impl",
		StepID:        "step-1",
		StepName:      "Implement",
		ProviderID:    "claude",
		Prompt:        "implement feature",
		CWD:           "/repo",
		StartedAt:     r2ac1Started,
	}); err != nil {
		t.Fatalf("RecordAgentCallStart ac-e2e-003 failed: %v", err)
	}
	if err := recorder.RecordAgentCallFinish(ctx, akashic.AgentCallResult{
		ID:              "ac-e2e-003",
		Success:         false,
		DurationMS:      20000,
		TotalCostUSD:    sql.NullFloat64{Float64: 0.001, Valid: true},
		InputTokens:     sql.NullInt64{Int64: 500, Valid: true},
		OutputTokens:    sql.NullInt64{Int64: 0, Valid: true},
		Error:           "agent_call timed out",
		FailureCategory: "timeout",
		FinishedAt:      r2ac1Finished,
	}); err != nil {
		t.Fatalf("RecordAgentCallFinish ac-e2e-003 failed: %v", err)
	}
	if err := recorder.RecordWorkflowRunFinish(ctx, akashic.WorkflowRunFinish{
		ID:         "run-e2e-002",
		Error:      "agent_call timed out",
		FinishedAt: r2Finished,
	}); err != nil {
		t.Fatalf("RecordWorkflowRunFinish run-e2e-002 failed: %v", err)
	}

	// ---- run-e2e-003: in-progress (FinishedAt=NULL / Success=NULL) + 0 agent_calls ----
	r3Started := time.Date(2024, 1, 15, 11, 0, 0, 0, loc)
	if err := recorder.RecordWorkflowRunStart(ctx, akashic.WorkflowRun{
		ID:           "run-e2e-003",
		WorkflowID:   "git-commit",
		WorkflowName: "Git Commit",
		CWD:          "/repo",
		StartedAt:    r3Started,
	}); err != nil {
		t.Fatalf("RecordWorkflowRunStart run-e2e-003 failed: %v", err)
	}
	// 意図的に Finish を呼ばない (in-progress 状態を作る)
}

// TestAkashicExportE2E は実 SQLiteRecorder を Recorder + Reader として共有する経路で
// `rilll akashic export` を実行し、stdout JSON を testdata/akashic_export_e2e.json と
// byte 単位で突合する (Req 8.2, 8.4)。
//
// 4.6 の golden test (TestRunAkashicExport_DefaultGolden) が fake Reader 経由で
// JSON 出力契約 (フィールド名 / 並び順 / null↔[] / RFC3339Nano) を担保するのに対し、
// 本テストは実 SQL を貫いて以下を回帰防止する:
//
//   - schema 定義 (store.go の CREATE TABLE) と Reader SQL (queries.go) の整合
//   - LEFT JOIN scan + Go 側 group の正しさ (List)
//   - sentinel-binding パターン (workflow_id / since / until / success / cwd / provider) の
//     未指定時の「全件」挙動 (本テストは未指定で 3 件すべてが返ることを確認する)
//   - Recorder の formatTime (UTC + RFC3339Nano) と Reader の parseStoredTime ↔ JSON 出力
//     の Format(RFC3339Nano) が同一文字列を生成すること
//   - RunHeader.Success の派生 (workflow_runs.error / finished_at から) と JSON null 出力
//
// MaxOpenConns(1) + WAL + busy_timeout=5000:
//   - Recorder で 3 件 insert 後、同 *SQLiteRecorder を Reader として export を呼ぶ。
//     同 *sql.DB の経由が直列化され、busy_timeout 内で待機なく動作することを確認する。
func TestAkashicExportE2E(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "akashic.sqlite")
	recorder, err := akashic.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite failed: %v", err)
	}
	t.Cleanup(func() {
		if cerr := recorder.Close(); cerr != nil {
			t.Errorf("recorder Close failed: %v", cerr)
		}
	})

	insertE2EFixture(t, ctx, recorder)

	// 同 *SQLiteRecorder を Reader として CLI export に注入する。
	// MaxOpenConns(1) + WAL + busy_timeout 環境で読み書き経路が同 *sql.DB を共有しても
	// 直列化により競合しないことを、本テスト全体の成功で担保する。
	deps, stdout, stderr := depsWithReader(recorder)

	if exit := runAkashicExport(ctx, nil, deps); exit != ExitOK {
		t.Fatalf("expected exit 0, got %d. stderr=%s", exit, stderr.String())
	}
	assertGolden(t, "testdata/akashic_export_e2e.json", stdout.Bytes())
}

// TestRunAkashicExport_InvalidFlagsExitTwo は不正フラグで exit 2 + stderr に
// 該当フラグ名 / メッセージが含まれることを table-driven で検証する
// (Req 6.3, 6.4, 6.9, 7.4)。
//
// 各ケースは akashic_export_test.go の単独テスト群 (3.1 companion smoke)
// と相補的に、4.6 では 1 つの table-driven test に揃えて完成形を提示する。
func TestRunAkashicExport_InvalidFlagsExitTwo(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		listErr   error
		wantExit  int
		wantInErr string
	}{
		{
			// --limit=-1 → ListOptions.Validate が ErrInvalidLimit を返す経路を
			// fake で再現 (3.1 Implementation Note 参照)。
			name:      "limit_negative",
			args:      []string{"--limit", "-1"},
			listErr:   akashic.ErrInvalidLimit,
			wantExit:  ExitUsageError,
			wantInErr: "limit",
		},
		{
			// --success=foo は CLI 自身が parseSuccessFlag で弾く (Reader 未呼び出し)。
			name:      "success_invalid",
			args:      []string{"--success", "foo"},
			wantExit:  ExitUsageError,
			wantInErr: "--success",
		},
		{
			// --since=invalid-date は CLI 自身が parseRFC3339Flag で弾く (Reader 未呼び出し)。
			name:      "since_invalid",
			args:      []string{"--since", "invalid-date"},
			wantExit:  ExitUsageError,
			wantInErr: "--since",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reader := &fakeReader{listErr: c.listErr}
			deps, _, stderr := depsWithReader(reader)
			exit := runAkashicExport(context.Background(), c.args, deps)
			if exit != c.wantExit {
				t.Fatalf("expected exit %d, got %d. stderr=%s", c.wantExit, exit, stderr.String())
			}
			if !strings.Contains(stderr.String(), c.wantInErr) {
				t.Errorf("expected stderr to contain %q, got: %s", c.wantInErr, stderr.String())
			}
		})
	}
}
