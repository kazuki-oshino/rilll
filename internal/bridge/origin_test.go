package bridge

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// WithCancelOrigin で ctx に *atomic.Value を attach した後、
// 値が Store されていれば CancelOriginFromContext が同じ文字列を返すことを確認する。
// design.md Implementation Notes (recordAgentCallFinish extension) で固定された
// 「ctx atomic 経由で origin を運搬する」契約の RED gate。
func TestCancelOriginFromContextReturnsStoredValue(t *testing.T) {
	cases := []struct {
		name  string
		store string
		want  string
	}{
		{name: "user", store: CancelOriginUser, want: CancelOriginUser},
		{name: "engine", store: CancelOriginEngine, want: CancelOriginEngine},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref := &atomic.Value{}
			ctx := WithCancelOrigin(context.Background(), ref)
			ref.Store(tc.store)

			got := CancelOriginFromContext(ctx)
			if got != tc.want {
				t.Fatalf("CancelOriginFromContext = %q, want %q", got, tc.want)
			}
		})
	}
}

// ctx に ref を attach したが Store していないとき、CancelOriginFromContext は
// 空文字を返さなければならない (design.md: 「未 attach / 未 Store なら空文字」)。
func TestCancelOriginFromContextReturnsEmptyWhenNotStored(t *testing.T) {
	ref := &atomic.Value{}
	ctx := WithCancelOrigin(context.Background(), ref)

	got := CancelOriginFromContext(ctx)
	if got != "" {
		t.Fatalf("CancelOriginFromContext = %q, want empty string", got)
	}
}

// ref を ctx に attach していないとき、CancelOriginFromContext は空文字を返さなければならない。
// recordAgentCallFinish などが ctx atomic 未配線な経路で呼ばれた場合の安全弁。
func TestCancelOriginFromContextReturnsEmptyWhenNotAttached(t *testing.T) {
	got := CancelOriginFromContext(context.Background())
	if got != "" {
		t.Fatalf("CancelOriginFromContext = %q, want empty string", got)
	}
}

// design.md task 2.3 観測可能な完了:
// 「ctx に atomic ref を attach した後、別 goroutine から CancelOriginFromContext で同値を読み取れる」
// nodebridge launcher が別 goroutine で result event を Store し、agent 層が ctx 経由で読む経路の前提条件。
func TestCancelOriginFromContextReadableFromAnotherGoroutine(t *testing.T) {
	ref := &atomic.Value{}
	ctx := WithCancelOrigin(context.Background(), ref)
	ref.Store(CancelOriginUser)

	var wg sync.WaitGroup
	wg.Add(1)
	var got string
	go func() {
		defer wg.Done()
		got = CancelOriginFromContext(ctx)
	}()
	wg.Wait()

	if got != CancelOriginUser {
		t.Fatalf("CancelOriginFromContext (goroutine) = %q, want %q", got, CancelOriginUser)
	}
}

// nil ref を attach した場合、CancelOriginFromContext は空文字を返す (defense-in-depth)。
// 呼び出し側が誤って nil を渡しても panic させない契約。
func TestCancelOriginFromContextHandlesNilRef(t *testing.T) {
	ctx := WithCancelOrigin(context.Background(), nil)

	got := CancelOriginFromContext(ctx)
	if got != "" {
		t.Fatalf("CancelOriginFromContext (nil ref) = %q, want empty string", got)
	}
}

// atomic.Value に non-string 値が Store された場合でも、CancelOriginFromContext は
// 空文字を返さなければならない (defense-in-depth: 型安全担保 + panic 抑止)。
func TestCancelOriginFromContextIgnoresNonStringStore(t *testing.T) {
	ref := &atomic.Value{}
	ctx := WithCancelOrigin(context.Background(), ref)
	ref.Store(123) // int を意図的に Store

	got := CancelOriginFromContext(ctx)
	if got != "" {
		t.Fatalf("CancelOriginFromContext = %q, want empty string for non-string store", got)
	}
}

// StoreCancelOrigin は ctx に attach された *atomic.Value に origin 文字列を
// Store する書き込み側ヘルパで、CancelOriginFromContext と対称な writer 経路として
// 機能しなければならない (task 2.6: nodebridge launcher が ConsumeEvents で
// result event の Origin を ctx atomic に伝搬する経路に必要)。
func TestStoreCancelOriginStoresViaContext(t *testing.T) {
	cases := []struct {
		name  string
		store string
	}{
		{name: "user", store: CancelOriginUser},
		{name: "engine", store: CancelOriginEngine},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref := &atomic.Value{}
			ctx := WithCancelOrigin(context.Background(), ref)

			StoreCancelOrigin(ctx, tc.store)

			if got := CancelOriginFromContext(ctx); got != tc.store {
				t.Fatalf("CancelOriginFromContext after StoreCancelOrigin = %q, want %q", got, tc.store)
			}
		})
	}
}

// 空文字 origin での StoreCancelOrigin は no-op であり、既存 Store 値も汚染しないこと。
// nodebridge launcher で正常終了 (origin 未指定 = 空文字) のとき ctx atomic を
// 空のまま保つ契約 (task 2.6 観測可能な完了の対象 negative case) を verify する。
func TestStoreCancelOriginIsNoOpForEmptyString(t *testing.T) {
	ref := &atomic.Value{}
	ctx := WithCancelOrigin(context.Background(), ref)

	StoreCancelOrigin(ctx, "")

	if got := CancelOriginFromContext(ctx); got != "" {
		t.Fatalf("CancelOriginFromContext after empty StoreCancelOrigin = %q, want empty string", got)
	}
}

// ctx に ref が attach されていない場合でも StoreCancelOrigin は panic せず no-op であること。
// 既存呼び出し経路 (UI seam を未配線な provider 単体テスト経路など) との後方互換ガード。
func TestStoreCancelOriginIsNoOpWhenNotAttached(t *testing.T) {
	// panic しないことを期待 (assert は実行完了で十分)。
	StoreCancelOrigin(context.Background(), CancelOriginUser)
}

// nil ref を attach した ctx に対して StoreCancelOrigin が panic / 副作用なく
// no-op で完了すること (defense-in-depth)。
func TestStoreCancelOriginHandlesNilRef(t *testing.T) {
	ctx := WithCancelOrigin(context.Background(), nil)

	StoreCancelOrigin(ctx, CancelOriginUser)

	if got := CancelOriginFromContext(ctx); got != "" {
		t.Fatalf("CancelOriginFromContext (nil ref) = %q, want empty string", got)
	}
}
