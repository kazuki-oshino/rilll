package learnings

// 本ファイルは agent-learnings reflector phase の並列度制限プリミティブ ParallelLimiter
// と、その constructor NewLimiter を提供する (design.md §Service Catalog L341 / Req 4.3)。
//
// 役割の境界:
//
//   - ParallelLimiter は context-aware な semaphore で、Reflector phase が N session
//     を `RILLL_LEARNINGS_MAX_PARALLEL` 上限で並列発行する際の枠管理に使う。
//   - NewLimiter は env / default の優先順で枠数を 1 度だけ解決し、parse 失敗 / `< 1`
//     のときは default にフォールバックして warning ログ 1 回を出す seam を提供する
//     (v1 distiller の `resolveMinFailureCount` と同形のフォールバック方針)。
//
// 設計ノート:
//
//   - 並列制御は標準 `sync` のみで実装し、外部依存 (`golang.org/x/sync/semaphore` 等) を
//     追加しない (research.md §Concurrency Stack の方針)。
//   - 内部状態は buffered channel `chan struct{}` 1 つに閉じ、Acquire/Release は
//     channel の send/recv で表現する。これにより goroutine safety は channel 自身が保証する。
//   - warning ログ seam (`learningsWarnLog`) は package-level var を test 側で
//     差し替え可能にすることで、log の副流出なしに単体テストで warning 回数を観測できる。

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

// envRillLearningsMaxParallel は agent-learnings reflector phase の並列度上限を上書きする
// 環境変数名 (Req 4.3)。typo / case sensitivity gate のため定数化し、test 側も
// 同じ定数を参照する。本 package 内のみで使うため unexported (v1 distiller と同形)。
const envRillLearningsMaxParallel = "RILLL_LEARNINGS_MAX_PARALLEL"

// defaultMaxParallel は env 未設定 / parse 失敗 / `< 1` のいずれの経路でも採用される
// fallback 並列度 (Req 4.3 の "default 5")。
//
// 5 は claude-agent-sdk の典型的な per-account rate limit (~10 req/sec) の半分以下を
// 選ぶことで burst を吸収する design.md §Cross-Cutting Concerns の値。
const defaultMaxParallel = 5

// learningsWarnLog は env が parse 不能 / `< 1` のときに NewLimiter から呼ばれる warning
// 出力 seam。既定値は標準 `log.Printf`。テストでは captured slice に流し込む stub に
// 置き換えてメッセージ内容と発火回数を検証する (parallel_test.go の
// `withLearningsWarnLog` ヘルパーが Cleanup 込みで使用)。
var learningsWarnLog = func(format string, args ...any) {
	log.Printf(format, args...)
}

// ParallelLimiter は agent-learnings reflector phase が複数 session を並列発行する際の
// 同時実行枠 semaphore。`Acquire` で 1 枠取得し、処理終了時に `Release` で返却する
// (defer 推奨)。`Limit` は構築時に決定された枠数を返し、観測 / テスト用途に使う。
//
// 並列安全性: 内部 channel が channel-level の goroutine safety を提供するため、
// 複数 goroutine からの同時 Acquire/Release は安全。
//
// 構築は必ず NewLimiter 経由で行うこと (zero value 構造体は内部 channel が nil で
// Acquire が永久 block する)。
type ParallelLimiter struct {
	// sem は枠数 N の buffered channel。送信成功で枠取得、受信で枠返却を表す。
	sem chan struct{}
}

// NewLimiter は envName を読み、default=defaultN にフォールバックする ParallelLimiter
// を構築する (design.md §Service Catalog L341 / Req 4.3)。
//
// 解決順序:
//
//  1. envName が non-empty かつ parseable int で `>= 1` → その値を採用
//  2. envName が空文字 → defaultN を採用 (warning なし)
//  3. envName が parse 不能 / `< 1` → defaultN にフォールバック + warning ログ 1 回
//
// envName が空文字の場合は env を読まない (caller が "default 経路を強制" したい場合の seam)。
//
// defaultN は呼び出し側が必ず `>= 1` を渡す前提 (programmer contract)。
// 通常 `defaultMaxParallel` を渡す。`defaultN < 1` は意図しない使い方なので panic。
func NewLimiter(envName string, defaultN int) *ParallelLimiter {
	if defaultN < 1 {
		panic(fmt.Sprintf(
			"learnings.NewLimiter: defaultN must be >= 1, got %d",
			defaultN,
		))
	}
	n := resolveMaxParallel(envName, defaultN)
	return &ParallelLimiter{sem: make(chan struct{}, n)}
}

// resolveMaxParallel は envName を読み、parse 失敗 / `< 1` のときは defaultN にフォール
// バックして warning ログ 1 回を出す純関数 (NewLimiter から 1 回だけ呼ばれる前提)。
//
// envName が空文字 (caller が env を明示無効化) の場合は env 読み出しをスキップする。
// 戻り値は常に `>= 1` (defaultN >= 1 が contract で保証されているため)。
func resolveMaxParallel(envName string, defaultN int) int {
	if envName == "" {
		return defaultN
	}
	raw := strings.TrimSpace(os.Getenv(envName))
	if raw == "" {
		return defaultN
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		learningsWarnLog(
			"%s=%q を整数として解釈できません。default=%d にフォールバックします: %v",
			envName, raw, defaultN, err,
		)
		return defaultN
	}
	if v < 1 {
		learningsWarnLog(
			"%s=%d は >= 1 である必要があります。default=%d にフォールバックします",
			envName, v, defaultN,
		)
		return defaultN
	}
	return v
}

// Acquire は枠が空くまで block する (context-aware)。ctx が cancel された場合は
// ctx.Err() を返し、枠は取得しない。成功時は nil を返し、呼び出し側は処理完了後に
// 必ず Release を 1 回だけ呼ぶ責務を持つ (defer 推奨)。
//
// nil 安全性: receiver が nil の場合は panic (zero value 構造体経由の誤用検知)。
func (l *ParallelLimiter) Acquire(ctx context.Context) error {
	select {
	case l.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release は Acquire で取得した枠を 1 つ返却する。Acquire 成功なしで呼び出した場合は
// channel underflow で deadlock する (programmer contract: Acquire success と 1:1 対応)。
//
// 内部実装: buffered channel から 1 要素受信して空きを 1 つ作る。
func (l *ParallelLimiter) Release() {
	<-l.sem
}

// Limit は構築時に決定された並列度上限 (枠数) を返す。Reflector phase の観測 / テストで
// 「envName 経由で正しい値が解決されたか」を検証する用途に使う。
func (l *ParallelLimiter) Limit() int {
	return cap(l.sem)
}
