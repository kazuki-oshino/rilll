// Package improvepatch のテスト本ファイルは GoTestRunner interface 契約の
// 構造的検証 (interface 充足性 + 戻り値タプル形状) と、既定実装の挙動契約
// (`go test ./... -count=1` 子プロセス起動 + stdout/stderr 64 KiB バッファ +
// ctx cancel で kill) の双方を行う (tasks.md 7.1 / design.md §GoTestRunner /
// Req 5.5, 5.6, 5.7)。
//
// 検証観点 (interface 契約):
//   - GoTestRunner が exported な interface としてパッケージ外から利用可能であること
//   - Run のシグネチャが (ctx, cwd) → (TestResult, error) であること
//   - TestResult が ExitCode / DurationMS / StdoutTail / StderrTail / Truncated を持つこと
//
// 検証観点 (既定実装 3 ケース / task 7.1):
//  1. 成功: 空 test 関数を 1 件含む fixture Go module で ExitCode == 0
//  2. 失敗: t.Fatal を呼ぶ test 関数を 1 件含む fixture で ExitCode != 0
//  3. timeout: 30s sleep する test を含む fixture を ctx 200ms timeout で kill
//
// 加えて truncate helper の単体テストで「中央切り詰め + Truncated フラグ」を機械検証する
// (design.md §GoTestRunner: 「最大 64 KiB をバッファリング、超過時は先頭と末尾を残して
// 中央を `... [truncated N bytes] ...` で省略」)。
package patch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGoTestRunner は GoTestRunner interface を満たす最小 fake 実装。
// テスト用途のみで、本物の go test 子プロセス起動は行わない。後続タスク
// (11.3 orchestration テスト / 12.2 verdict 別統合テスト) で同型 fake を再利用する想定。
type fakeGoTestRunner struct {
	result TestResult
	err    error

	// callCount は fake が呼ばれた回数を記録する (副作用契約検証で利用)。
	callCount int
}

// Run は fake が保持する固定値を返す。引数は型シグネチャ検証のためだけに受け取る。
func (f *fakeGoTestRunner) Run(ctx context.Context, cwd string) (TestResult, error) {
	f.callCount++
	return f.result, f.err
}

// 型レベル assertion: fakeGoTestRunner が GoTestRunner interface を満たすことを
// コンパイル時に保証する。task 7.1 観察可能完了条件「interface が型システム上で
// 利用可能」を機械検証する。
var _ GoTestRunner = (*fakeGoTestRunner)(nil)

// TestGoTestRunner_FakeSatisfiesInterface は fake 実装が GoTestRunner interface を
// 満たし、変数代入可能であることを検証する (パッケージ外利用契約の構造検証)。
func TestGoTestRunner_FakeSatisfiesInterface(t *testing.T) {
	var runner GoTestRunner = &fakeGoTestRunner{
		result: TestResult{ExitCode: 0, DurationMS: 42},
	}
	got, err := runner.Run(context.Background(), "/tmp")
	if err != nil {
		t.Fatalf("fake Run unexpectedly returned error: %v", err)
	}
	if got.ExitCode != 0 || got.DurationMS != 42 {
		t.Fatalf("fake Run returned unexpected TestResult: %+v", got)
	}
}

// goTestFixtureGoMod は fixture Go module の最小 go.mod。
// `go 1.25` で固定し、外部依存を一切持たない (CGO 非依存維持 / steering tech.md)。
const goTestFixtureGoMod = `module fixture

go 1.25
`

// goTestFixturePassSource はテスト関数 1 件 (空 body) を含む成功用 fixture ソース。
// `go test ./...` で ExitCode==0 になる最小ケース。
const goTestFixturePassSource = `package fixture

import "testing"

func TestPass(t *testing.T) {
	// 空 body: 即時成功する。
}
`

// goTestFixtureFailSource はテスト関数 1 件 (t.Fatal を呼ぶ) を含む失敗用 fixture ソース。
// `go test ./...` で ExitCode!=0 になる最小ケース。
const goTestFixtureFailSource = `package fixture

import "testing"

func TestFail(t *testing.T) {
	t.Fatal("intentional failure for GoTestRunner fixture")
}
`

// goTestFixtureSlowSource は 30 秒 sleep する test を含む timeout 検証用 fixture ソース。
// テスト本体側で ctx に 200ms timeout を設定し、`exec.CommandContext` 経由で kill されることを検証する。
// 30 秒の sleep は十分長く、kill されずに完走すると即座にテスト失敗 (timeout) を引き起こす。
const goTestFixtureSlowSource = `package fixture

import (
	"testing"
	"time"
)

func TestSlow(t *testing.T) {
	time.Sleep(30 * time.Second)
}
`

// writeGoTestFixture は cwd 直下に go.mod と <name>_test.go を書き出す helper。
// fixture セットアップ専用で、テスト対象の defaultGoTestRunner は経由しない。
func writeGoTestFixture(t *testing.T, cwd, testFileName, testSource string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cwd, "go.mod"), []byte(goTestFixtureGoMod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cwd, testFileName), []byte(testSource), 0o644); err != nil {
		t.Fatalf("write %s: %v", testFileName, err)
	}
}

// TestGoTestRunner_Success は成功用 fixture (空 body の TestPass) を実行し、
// ExitCode==0 / err==nil / DurationMS>=0 を確認する (Req 5.5 / design.md §GoTestRunner)。
func TestGoTestRunner_Success(t *testing.T) {
	cwd := t.TempDir()
	writeGoTestFixture(t, cwd, "pass_test.go", goTestFixturePassSource)

	runner := NewGoTestRunner()
	// 余裕を持たせた timeout (60s)。fixture は 1 ファイル test なので通常 5s 以内で完走する。
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := runner.Run(ctx, cwd)
	if err != nil {
		t.Fatalf("Run returned error for passing fixture: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected ExitCode=0, got %d (stdout=%q stderr=%q)",
			result.ExitCode, result.StdoutTail, result.StderrTail)
	}
	if result.DurationMS < 0 {
		t.Fatalf("expected DurationMS >= 0, got %d", result.DurationMS)
	}
}

// TestGoTestRunner_Failure は失敗用 fixture (t.Fatal を呼ぶ TestFail) を実行し、
// ExitCode!=0 / err==nil (テスト失敗は exec エラーではない契約) を確認する
// (Req 5.6 / design.md §GoTestRunner Postconditions)。
func TestGoTestRunner_Failure(t *testing.T) {
	cwd := t.TempDir()
	writeGoTestFixture(t, cwd, "fail_test.go", goTestFixtureFailSource)

	runner := NewGoTestRunner()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := runner.Run(ctx, cwd)
	if err != nil {
		t.Fatalf("Run unexpectedly returned error for failing fixture (test failure should not surface as exec error): %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("expected ExitCode != 0 for failing fixture, got 0 (stdout=%q stderr=%q)",
			result.StdoutTail, result.StderrTail)
	}
}

// TestGoTestRunner_Timeout は 30s sleep する fixture を ctx 200ms timeout で起動し、
// `ctx.Done()` 経由で `exec.CommandContext` が子プロセスを kill することを検証する
// (Req 5.7 / design.md §GoTestRunner Invariants)。
//
// 観察方法: 子プロセスが kill されれば Run は ctx.Deadline 後すぐ (5 秒以内目安) に
// 戻る。30 秒 sleep を完走してしまった場合、ctx kill 経路が機能していないことになる。
func TestGoTestRunner_Timeout(t *testing.T) {
	cwd := t.TempDir()
	writeGoTestFixture(t, cwd, "slow_test.go", goTestFixtureSlowSource)

	runner := NewGoTestRunner()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	result, err := runner.Run(ctx, cwd)
	elapsed := time.Since(start)

	// kill されたことを「30 秒の sleep を完走しなかった」事実で確認する。
	// テスト環境のばらつきを許容するため上限は 25 秒とする (sleep は 30 秒)。
	if elapsed >= 25*time.Second {
		t.Fatalf("expected ctx-cancel kill, but Run took %v (sleep was 30s, suggests no kill)", elapsed)
	}

	// 仕様契約: ctx cancel 経路では err != nil で返る (ExitCode は実装依存だが
	// `go test` 完走による 0 ではない、または -1 / 非ゼロ のいずれか)。
	// テスト失敗 (test 関数の Fatal) ではないので、err != nil が期待値。
	if err == nil {
		t.Fatalf("expected non-nil error on ctx timeout, got nil (result=%+v elapsed=%v)", result, elapsed)
	}
}

// TestGoTestRunner_TruncateBuffer は truncate helper が 64 KiB 超の入力に対して
// 「先頭 32 KiB + `... [truncated N bytes] ...` + 末尾 32 KiB」形に切り詰め、
// Truncated==true を立てることを検証する (design.md §GoTestRunner)。
// 同関数は Run 本体から切り出された純粋関数として単体テストする。
func TestGoTestRunner_TruncateBuffer(t *testing.T) {
	t.Run("under limit returns as-is", func(t *testing.T) {
		input := strings.Repeat("a", 1024) // 1 KiB
		got, truncated := truncateForTail([]byte(input))
		if truncated {
			t.Fatalf("expected truncated=false for 1 KiB input, got true")
		}
		if got != input {
			t.Fatalf("expected unchanged content for under-limit input")
		}
	})

	t.Run("over limit truncates middle", func(t *testing.T) {
		// 100 KiB の入力 (32 KiB 'A' + 36 KiB 'M' + 32 KiB 'Z' に相当する量)。
		head := strings.Repeat("A", 40*1024) // 40 KiB head
		tail := strings.Repeat("Z", 40*1024) // 40 KiB tail
		input := head + tail                 // 80 KiB total

		got, truncated := truncateForTail([]byte(input))
		if !truncated {
			t.Fatalf("expected truncated=true for 80 KiB input")
		}
		// 64 KiB cap (32 KiB head + 32 KiB tail) + 中央 marker。
		if len(got) >= len(input) {
			t.Fatalf("expected truncated output shorter than input, got %d bytes (input %d)", len(got), len(input))
		}
		// 先頭 / 末尾の元の文字が残っていることと、marker substring が含まれることを検証する。
		if !strings.HasPrefix(got, "A") {
			t.Fatalf("expected output to start with 'A' (head preserved)")
		}
		if !strings.HasSuffix(got, "Z") {
			t.Fatalf("expected output to end with 'Z' (tail preserved)")
		}
		// marker substring の包含確認 (先頭何バイトかをデバッグ出力に使う際は
		// 組み込み min を利用して上限を切る)。
		if !strings.Contains(got, "[truncated") {
			limit := min(len(got), 200)
			t.Fatalf("expected truncate marker in output, got %q", got[:limit])
		}
	})
}
