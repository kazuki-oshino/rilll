package learnings

// 本ファイルは task 6.3 (ParallelLimiter) の TDD 用テストを集約する。
// design.md §Service Catalog L341 / Req 4.3 が要求する以下を検証する:
//
//   - 環境変数 RILLL_LEARNINGS_MAX_PARALLEL 未設定 → default=5 / warning 0 回
//   - parse 不能 → default fallback + warning 1 回
//   - 値 < 1     → default fallback + warning 1 回
//   - 正常値      → 採用 / warning 0 回
//   - N=10 / max=5 で同時実行が 5 を超えない
//
// 設計上の境界:
//   - test seam として package level の learningsWarnLog を一時的に捕捉する
//     withLearningsWarnLog ヘルパーを使い production log への副流出を防ぐ。
//   - env var 名は test 並走時の汚染を避けるため t.Setenv のみで設定する。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withLearningsWarnLog は learningsWarnLog test seam を一時的に override し、
// 受信した warning メッセージを slice として収集して返すヘルパー。
// 戻り値の slice 参照は test 終了時まで有効。
func withLearningsWarnLog(t *testing.T) *[]string {
	t.Helper()
	prev := learningsWarnLog
	captured := []string{}
	var mu sync.Mutex
	learningsWarnLog = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { learningsWarnLog = prev })
	return &captured
}

// TestNewLimiter_DefaultWhenEnvUnset は env 未設定時に default を採用し、
// warning が出ないこと (Req 4.3 の "default 5") を検証する。
func TestNewLimiter_DefaultWhenEnvUnset(t *testing.T) {
	t.Setenv(envRillLearningsMaxParallel, "")
	captured := withLearningsWarnLog(t)

	lim := NewLimiter(envRillLearningsMaxParallel, defaultMaxParallel)
	if got := lim.Limit(); got != defaultMaxParallel {
		t.Fatalf("Limit()=%d, want %d", got, defaultMaxParallel)
	}
	if len(*captured) != 0 {
		t.Fatalf("warning が予期せず出力された: %q", *captured)
	}
}

// TestNewLimiter_ParseValidEnv は env=3 のとき採用され、warning が出ないことを検証する。
func TestNewLimiter_ParseValidEnv(t *testing.T) {
	t.Setenv(envRillLearningsMaxParallel, "3")
	captured := withLearningsWarnLog(t)

	lim := NewLimiter(envRillLearningsMaxParallel, defaultMaxParallel)
	if got := lim.Limit(); got != 3 {
		t.Fatalf("Limit()=%d, want 3", got)
	}
	if len(*captured) != 0 {
		t.Fatalf("warning が予期せず出力された: %q", *captured)
	}
}

// TestNewLimiter_ParseFailFallsBackToDefault は env=非整数のとき default にフォールバックし、
// warning がちょうど 1 回出ることを検証する (Req 4.3 / "warning ログを 1 回だけ出す seam")。
func TestNewLimiter_ParseFailFallsBackToDefault(t *testing.T) {
	t.Setenv(envRillLearningsMaxParallel, "not-a-number")
	captured := withLearningsWarnLog(t)

	lim := NewLimiter(envRillLearningsMaxParallel, defaultMaxParallel)
	if got := lim.Limit(); got != defaultMaxParallel {
		t.Fatalf("Limit()=%d, want %d (fallback)", got, defaultMaxParallel)
	}
	if len(*captured) != 1 {
		t.Fatalf("warning 回数 = %d, want 1: %q", len(*captured), *captured)
	}
	if !strings.Contains((*captured)[0], envRillLearningsMaxParallel) {
		t.Fatalf("warning は env var 名を含むべき: %q", (*captured)[0])
	}
	if !strings.Contains((*captured)[0], "not-a-number") {
		t.Fatalf("warning は不正値そのものを含むべき: %q", (*captured)[0])
	}
}

// TestNewLimiter_ZeroFallsBackToDefault は env=0 のとき default にフォールバックし、
// warning が 1 回出ることを検証する (Req 4.3 の "< 1 で default fallback")。
func TestNewLimiter_ZeroFallsBackToDefault(t *testing.T) {
	t.Setenv(envRillLearningsMaxParallel, "0")
	captured := withLearningsWarnLog(t)

	lim := NewLimiter(envRillLearningsMaxParallel, defaultMaxParallel)
	if got := lim.Limit(); got != defaultMaxParallel {
		t.Fatalf("Limit()=%d, want %d (fallback)", got, defaultMaxParallel)
	}
	if len(*captured) != 1 {
		t.Fatalf("warning 回数 = %d, want 1: %q", len(*captured), *captured)
	}
}

// TestNewLimiter_NegativeFallsBackToDefault は env=-3 のとき default にフォールバックし、
// warning が 1 回出ることを検証する (Req 4.3 の "< 1 で default fallback")。
func TestNewLimiter_NegativeFallsBackToDefault(t *testing.T) {
	t.Setenv(envRillLearningsMaxParallel, "-3")
	captured := withLearningsWarnLog(t)

	lim := NewLimiter(envRillLearningsMaxParallel, defaultMaxParallel)
	if got := lim.Limit(); got != defaultMaxParallel {
		t.Fatalf("Limit()=%d, want %d (fallback)", got, defaultMaxParallel)
	}
	if len(*captured) != 1 {
		t.Fatalf("warning 回数 = %d, want 1: %q", len(*captured), *captured)
	}
}

// TestNewLimiter_TrimsWhitespace は env 値の前後空白を trim して採用することを検証する
// (運用者が `export RILLL_LEARNINGS_MAX_PARALLEL=" 4 "` のように設定した場合の救済)。
func TestNewLimiter_TrimsWhitespace(t *testing.T) {
	t.Setenv(envRillLearningsMaxParallel, "  4  ")
	captured := withLearningsWarnLog(t)

	lim := NewLimiter(envRillLearningsMaxParallel, defaultMaxParallel)
	if got := lim.Limit(); got != 4 {
		t.Fatalf("Limit()=%d, want 4", got)
	}
	if len(*captured) != 0 {
		t.Fatalf("warning が予期せず出力された: %q", *captured)
	}
}

// TestParallelLimiter_DoesNotExceedMaxConcurrency は N=10 goroutine が max=5 の
// limiter で同時実行される際、観測される並列度が 5 を超えないことを検証する
// (Req 4.3 / task 6.3 完了条件 "N=10 / max=5 で同時実行が 5 を超えない")。
//
// 観測のため Acquire 中の goroutine 数を atomic counter で追跡し、Release 時に減算。
// timing dependent なので各 goroutine に十分な hold 時間 (40ms) を与え、雑音を下げる。
func TestParallelLimiter_DoesNotExceedMaxConcurrency(t *testing.T) {
	t.Setenv(envRillLearningsMaxParallel, "5")
	lim := NewLimiter(envRillLearningsMaxParallel, defaultMaxParallel)

	var (
		active  atomic.Int32
		maxSeen atomic.Int32
		wg      sync.WaitGroup
	)
	const total = 10
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := lim.Acquire(context.Background()); err != nil {
				t.Errorf("Acquire error: %v", err)
				return
			}
			defer lim.Release()
			cur := active.Add(1)
			defer active.Add(-1)
			// CAS loop で観測 max を更新 (lockless)
			for {
				prev := maxSeen.Load()
				if cur <= prev {
					break
				}
				if maxSeen.CompareAndSwap(prev, cur) {
					break
				}
			}
			// 並列実行 window を十分長く取り、5 並列が安定して観測されるようにする。
			time.Sleep(40 * time.Millisecond)
		}()
	}
	wg.Wait()

	if got := maxSeen.Load(); got > 5 {
		t.Fatalf("max concurrent acquires = %d, want <= 5", got)
	}
	// sanity: 並列性が機能していることの最小検証。直列実行 (max=1) を取り違えていないこと。
	if got := maxSeen.Load(); got < 2 {
		t.Fatalf("max concurrent acquires = %d, expected >= 2 to demonstrate parallelism", got)
	}
}

// TestParallelLimiter_AcquireRespectsContextCancel は Acquire が Capacity 超過で
// blocking 中に ctx が cancel された場合、ctx.Err() を返すことを検証する。
// reflector phase (task 7.2) の cancel 経路で必要な不変式。
func TestParallelLimiter_AcquireRespectsContextCancel(t *testing.T) {
	t.Setenv(envRillLearningsMaxParallel, "1")
	lim := NewLimiter(envRillLearningsMaxParallel, defaultMaxParallel)

	// 1 枠を消費し、後続 Acquire は block する。
	if err := lim.Acquire(context.Background()); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer lim.Release()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- lim.Acquire(ctx)
	}()

	// goroutine が block 状態に入るのを少し待つ。
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("Acquire returned nil after cancel; expected ctx error")
		}
		if err != context.Canceled {
			t.Fatalf("Acquire err = %v, want context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("Acquire did not return after ctx cancel")
	}
}

// TestParallelLimiter_AcquireSucceedsWhenSlotAvailable は枠空き時に Acquire が
// 即座に nil で返ることを検証する基本 happy path。
func TestParallelLimiter_AcquireSucceedsWhenSlotAvailable(t *testing.T) {
	lim := NewLimiter("", 2) // env 未設定 → default 経路、ただし caller default を 2 に override

	if err := lim.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire 1: %v", err)
	}
	if err := lim.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire 2: %v", err)
	}
	lim.Release()
	lim.Release()
}
