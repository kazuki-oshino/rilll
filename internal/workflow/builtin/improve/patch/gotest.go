// Package improvepatch の本ファイルは GoTestRunner interface 定義と
// 既定実装 (`go test ./... -count=1` を `os/exec` で起動 + stdout/stderr バッファ +
// 中央切り詰め) を提供する (tasks.md 7.1 / design.md §GoTestRunner / Req 5.5, 5.6, 5.7)。
//
// 既定実装の挙動契約:
//   - `exec.CommandContext(ctx, "go", "test", "./...", "-count=1")` を `cmd.Dir = cwd` で起動。
//     `-count=1` は go test キャッシュを無効化し、改善後の挙動を必ず観測するため (design.md §GoTestRunner)。
//   - stdout / stderr はそれぞれ最大 32 KiB (合計 64 KiB) をバッファし、超過時は中央を
//     `... [truncated N bytes] ...` で省略して `Truncated == true` を立てる。
//   - ctx cancel で子プロセスを kill する (`exec.CommandContext` の標準動作)。
//   - 戻り値: `error == nil` でも `ExitCode != 0` がありうる (テスト失敗は exec エラーではない)。
//     `ctx.Err() != nil` (deadline / cancel) のとき error は非 nil で返る。
package patch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// GoTestRunner は improve-workflow の TDD ガードフェーズで `go test ./...` を実行する契約を表す。
// orchestration 側は `Apply` 成功直後に `Run` を呼び、`TestResult.ExitCode` で
// `applied_passed` / `applied_test_failed_reverted` verdict を判定する (design.md §GoTestRunner)。
//
// Preconditions:
//   - cwd は Go module root (`go.mod` を含むディレクトリ) であること。
//
// Postconditions:
//   - `error == nil` のときも `ExitCode != 0` がありうる (テスト失敗は exec エラーではない)。
//   - `ExitCode == 0 && error == nil` のときのみ「テスト成功」と判定する。
//   - 子プロセスは ctx cancel で確実に kill される (`exec.CommandContext` 標準動作)。
type GoTestRunner interface {
	// Run は cwd 直下で `go test ./... -count=1` を子プロセス実行し、TestResult と error を返す。
	// テスト失敗 (ExitCode != 0) は error として surface しない契約 (Postconditions 参照)。
	Run(ctx context.Context, cwd string) (TestResult, error)
}

// TestResult は GoTestRunner.Run の出力サマリ。result.json の `test_result` フィールド
// (TestResultRecord) に直接マップされる構造を持つ (design.md §7 / Req 6.3)。
type TestResult struct {
	// ExitCode は `go test` 子プロセスの終了コード。
	// テスト成功で 0、テスト失敗で 1 (build error 等で 2 もありうる)、
	// LookPath 失敗 / ctx cancel 等で取得不能な場合は -1 を格納する。
	ExitCode int

	// DurationMS は子プロセス起動から終了までの所要時間 (ミリ秒)。
	// orchestration 側の result.json 集計とユーザ向け表示に利用する。
	DurationMS int64

	// StdoutTail は子プロセス標準出力の末尾サマリ (最大 32 KiB)。
	// 64 KiB を超えた場合は中央が `... [truncated N bytes] ...` に置換され、
	// `Truncated == true` が立つ。
	StdoutTail string

	// StderrTail は子プロセス標準エラー出力の末尾サマリ (最大 32 KiB)。StdoutTail と同様。
	StderrTail string

	// Truncated は stdout / stderr のいずれかで中央切り詰めが発生したことを示す。
	// result.json で「サマリは省略形」とユーザに通知する用途。
	Truncated bool
}

// NewGoTestRunner は os/exec 経由で `go test ./...` を起動する既定 GoTestRunner 実装を返す。
// すべてのサブプロセスは渡された cwd を `cmd.Dir` に設定し、
// `os.Environ()` をそのまま継承する (rilll が走る環境の GOFLAGS / GOPATH 等を尊重)。
func NewGoTestRunner() GoTestRunner {
	return &defaultGoTestRunner{}
}

// defaultGoTestRunner は GoTestRunner interface の既定実装。
// `go test ./... -count=1` を子プロセスとして起動し、stdout / stderr / 終了コード /
// 所要時間を TestResult に詰めて返す。
type defaultGoTestRunner struct{}

// gotestStdoutCapMax / gotestStderrCapMax は stdout / stderr バッファの最大サイズ (32 KiB ずつ)。
// design.md §GoTestRunner: 「最大 64 KiB をバッファリング、超過時は先頭と末尾を残して
// 中央を `... [truncated N bytes] ...` で省略」を分割実装する。
const (
	gotestStdoutCapMax = 32 * 1024
	gotestStderrCapMax = 32 * 1024
	// gotestTotalCap は truncateForTail が単一バッファに対して許容する上限。
	// 32 KiB head + 32 KiB tail = 64 KiB 超で中央切り詰めを発動する。
	gotestTotalCap = gotestStdoutCapMax + gotestStderrCapMax
)

// Run は `go test ./... -count=1` を cwd 直下で起動し、結果を TestResult に詰めて返す。
// テスト失敗は exec エラーではないため (`*exec.ExitError`)、その場合 error は nil で
// ExitCode のみ非ゼロを返す (design.md §GoTestRunner Postconditions)。
func (r *defaultGoTestRunner) Run(ctx context.Context, cwd string) (TestResult, error) {
	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, "go", "test", "./...", "-count=1")
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)

	stdoutTail, stdoutTruncated := truncateForTail(stdout.Bytes())
	stderrTail, stderrTruncated := truncateForTail(stderr.Bytes())
	result := TestResult{
		ExitCode:   exitCodeFromGoTestErr(err),
		DurationMS: duration.Milliseconds(),
		StdoutTail: stdoutTail,
		StderrTail: stderrTail,
		Truncated:  stdoutTruncated || stderrTruncated,
	}

	// ctx cancel / deadline 経路では err 非 nil で返す (Invariants: 子プロセスは ctx cancel で kill)。
	// orchestration 側は `errors.Is(err, context.DeadlineExceeded)` 等で kill 経路を識別できる。
	if ctxErr := ctx.Err(); ctxErr != nil {
		// ctx の状態を保持しつつ exec 側の error も保持するため fmt.Errorf("%w: ...") で wrap する。
		return result, fmt.Errorf("improvepatch: go test killed by ctx: %w", ctxErr)
	}

	// テスト失敗 (`*exec.ExitError`) は error として surface しない契約。
	// LookPath 失敗 / I/O エラー (`*exec.Error` など) のみ error を返す。
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return result, fmt.Errorf("improvepatch: go test 起動失敗: %w", err)
	}

	return result, nil
}

// exitCodeFromGoTestErr は `go test` 子プロセスの err から終了コードを抽出する。
// `*exec.ExitError` を持つ場合 (テスト失敗) はその ExitCode を、
// それ以外 (`*exec.Error` / ctx cancel / LookPath 失敗) は -1 を返す。
func exitCodeFromGoTestErr(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// truncateForTail は buf を「最大 64 KiB (32 KiB head + 32 KiB tail)」に切り詰めた文字列と
// 中央切り詰めが発生したかを返す。64 KiB 以下なら入力をそのまま文字列化して返し、Truncated は false。
//
// 切り詰め形式:
//
//	<先頭 32 KiB> + "\n... [truncated N bytes] ...\n" + <末尾 32 KiB>
//
// design.md §GoTestRunner: 「最大 64 KiB をバッファリング、超過時は先頭と末尾を残して
// 中央を `... [truncated N bytes] ...` で省略」を機械化する。
func truncateForTail(buf []byte) (string, bool) {
	if len(buf) <= gotestTotalCap {
		return string(buf), false
	}
	head := buf[:gotestStdoutCapMax]
	tail := buf[len(buf)-gotestStderrCapMax:]
	omitted := len(buf) - gotestStdoutCapMax - gotestStderrCapMax
	// 区切り改行は前後 1 行ずつ、result.json 表示時に視覚的に区切るため。
	marker := fmt.Sprintf("\n... [truncated %d bytes] ...\n", omitted)
	combined := make([]byte, 0, len(head)+len(marker)+len(tail))
	combined = append(combined, head...)
	combined = append(combined, marker...)
	combined = append(combined, tail...)
	return string(combined), true
}
