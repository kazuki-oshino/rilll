// Package notify はワークフロー実行イベントに付随する副次通知 (音など) を提供する。
// 完了音は環境変数 RILLL_NOTIFY_SOUND で再生対象パスをユーザーが opt-in で設定する。
// 未設定時は完全に無音 (子プロセスも起動しない) — このため zombie 化や
// マシン固有パスへの依存を持ち込まない。
package notify

import (
	"log"
	"os"
	"os/exec"
	"strings"
)

// SoundEnvName は完了音ファイルパスを指定する環境変数名。
// 未設定 / 空文字 / 空白のみ → 完了音は再生されない。
const SoundEnvName = "RILLL_NOTIFY_SOUND"

// playerExecutable は完了音再生に使うバイナリ名。
// 現状は macOS の afplay 固定 (cross-OS 対応はスコープ外)。
// 他 OS で env を設定した場合は Start() が失敗してログに残る。
const playerExecutable = "afplay"

// startPlayer はテストから差し替えるためのフック。
// 戻り値の wait は exec.Cmd.Wait と同等で、zombie 回避のために goroutine で
// 必ず呼ぶ。Start に失敗した場合は err を返し wait は nil。
var startPlayer = defaultStartPlayer

func defaultStartPlayer(executable string, args ...string) (func() error, error) {
	cmd := exec.Command(executable, args...)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Wait, nil
}

// PlayCompletionSound は workflow 終了通知音を非同期で再生する。
//
//   - RILLL_NOTIFY_SOUND 未設定 → 何もしない (子プロセスを一切起動しない)
//   - 設定あり → afplay を非同期起動し、別 goroutine で Wait してから reap する
//   - Start エラー (afplay 不在など) → log.Printf で stderr へ警告
//
// 呼び出し側 (CLI dispatch / TUI PhaseDone 遷移) は即座に return するため、
// Wait は親フローを待たせない goroutine 内で行う。
func PlayCompletionSound() {
	soundPath := strings.TrimSpace(os.Getenv(SoundEnvName))
	if soundPath == "" {
		return
	}
	// -v 0.5 で音量を約半分に下げる (afplay の -v は 0.0〜1.0 を取る)。
	wait, err := startPlayer(playerExecutable, "-v", "0.5", soundPath)
	if err != nil {
		log.Printf("notify: 完了音の起動に失敗 (%s=%q): %v", SoundEnvName, soundPath, err)
		return
	}
	// afplay 終了後に reap しないと zombie として残るため必ず goroutine で Wait する。
	// Wait の戻り値 (afplay の non-zero exit など) はユーザーが「音が鳴らない」で
	// 気づくため、ここでは握りつぶす。
	go func() {
		_ = wait()
	}()
}
