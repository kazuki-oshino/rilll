package notify

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fakePlayer は startPlayer の差し替え用スパイ。
// 「呼び出された引数」「Wait が走ったか」「エラー注入」を制御する。
type fakePlayer struct {
	mu       sync.Mutex
	calls    [][]string
	startErr error
	waitErr  error
	waitCh   chan struct{}
}

func (f *fakePlayer) start(executable string, args ...string) (func() error, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{executable}, args...))
	startErr := f.startErr
	waitErr := f.waitErr
	waitCh := f.waitCh
	f.mu.Unlock()
	if startErr != nil {
		return nil, startErr
	}
	return func() error {
		if waitCh != nil {
			close(waitCh)
		}
		return waitErr
	}, nil
}

func (f *fakePlayer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// swapPlayer は startPlayer をテスト関数内だけ差し替えるヘルパー。
func swapPlayer(t *testing.T, fp *fakePlayer) {
	t.Helper()
	orig := startPlayer
	startPlayer = fp.start
	t.Cleanup(func() { startPlayer = orig })
}

func TestPlayCompletionSoundSkipsWhenEnvUnset(t *testing.T) {
	t.Setenv(SoundEnvName, "")
	fp := &fakePlayer{}
	swapPlayer(t, fp)

	PlayCompletionSound()

	if fp.callCount() != 0 {
		t.Fatalf("env 未設定時は player を起動しないはず: calls=%v", fp.calls)
	}
}

func TestPlayCompletionSoundSkipsWhenEnvBlank(t *testing.T) {
	t.Setenv(SoundEnvName, "   \t\n")
	fp := &fakePlayer{}
	swapPlayer(t, fp)

	PlayCompletionSound()

	if fp.callCount() != 0 {
		t.Fatalf("空白のみの env は未設定扱いで player を起動しないはず: calls=%v", fp.calls)
	}
}

func TestPlayCompletionSoundInvokesPlayerWithEnvPath(t *testing.T) {
	t.Setenv(SoundEnvName, "/tmp/test.mp3")
	fp := &fakePlayer{waitCh: make(chan struct{})}
	swapPlayer(t, fp)

	PlayCompletionSound()

	if fp.callCount() != 1 {
		t.Fatalf("env 設定時は player を 1 回起動するはず: calls=%v", fp.calls)
	}
	want := []string{"afplay", "-v", "0.5", "/tmp/test.mp3"}
	if !reflect.DeepEqual(fp.calls[0], want) {
		t.Fatalf("player 起動引数が期待と一致しない: got=%v want=%v", fp.calls[0], want)
	}

	// zombie 回避のため、起動後に Wait() goroutine が走ることを検証する。
	// waitCh は fake の wait 関数内で close される。
	select {
	case <-fp.waitCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() を呼ぶ goroutine が起動していない (zombie 化のリスク)")
	}
}

func TestPlayCompletionSoundTrimsEnvWhitespace(t *testing.T) {
	// path 周辺の空白だけはトリムして許容する (typo 対策)。
	t.Setenv(SoundEnvName, "  /tmp/test.mp3  ")
	fp := &fakePlayer{waitCh: make(chan struct{})}
	swapPlayer(t, fp)

	PlayCompletionSound()

	if fp.callCount() != 1 {
		t.Fatalf("env 設定時は player を 1 回起動するはず: calls=%v", fp.calls)
	}
	if got := fp.calls[0][3]; got != "/tmp/test.mp3" {
		t.Fatalf("path の前後空白がトリムされていない: got=%q", got)
	}
}

func TestPlayCompletionSoundSwallowsStartError(t *testing.T) {
	t.Setenv(SoundEnvName, "/tmp/test.mp3")
	fp := &fakePlayer{startErr: errors.New("afplay not found")}
	swapPlayer(t, fp)

	// Start エラー時も panic せず、呼び出し側に影響を与えないことを確認する。
	PlayCompletionSound()

	if fp.callCount() != 1 {
		t.Fatalf("Start は試みるはず: calls=%v", fp.calls)
	}
}

func TestPlayCompletionSoundDoesNotLeakGoroutineWhenStartFails(t *testing.T) {
	// Start が失敗した場合は Wait goroutine を立てないこと
	// (失敗時に nil 関数を呼ぼうとして panic しないこと)。
	t.Setenv(SoundEnvName, "/tmp/test.mp3")
	fp := &fakePlayer{
		startErr: errors.New("boom"),
		// waitCh は close されないことを確認する
		waitCh: make(chan struct{}),
	}
	swapPlayer(t, fp)

	PlayCompletionSound()

	select {
	case <-fp.waitCh:
		t.Fatal("Start が失敗したのに wait 関数が呼ばれた")
	case <-time.After(50 * time.Millisecond):
		// 期待通り wait は呼ばれない
	}
}
