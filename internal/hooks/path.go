package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// hooksDir はユーザグローバル hook ディレクトリの絶対パスを返す。
//
// 解決規約 (XDG Base Directory; internal/config.Path() と等価 / R4.4):
//   - XDG_CONFIG_HOME が空でない値で設定されている場合: $XDG_CONFIG_HOME/rilll/hooks (R4.1)
//   - 未設定または空文字列の場合: $HOME/.config/rilll/hooks (R4.2)
//
// 戻り値はディレクトリ自体の作成を保証しない (extractTo が遅延作成する)。
// internal/config への import は循環依存回避のため行わず、規約のみを踏襲する
// (config.Path() と同様に strings.TrimSpace で whitespace-only XDG_CONFIG_HOME を
// 空扱いとして HOME fallback に流す)。
//
// HOME も XDG_CONFIG_HOME も解決できない場合は wrap した I/O エラーを返す。
// この経路は実環境ではほぼ発生しないが、契約として呼び出し側に伝播させる
// (R6.2 で I/O エラーを ErrHookNotFound と区別する設計に整合)。
func hooksDir() (string, error) {
	base := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if base == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("hooks: resolve user home: %w", err)
		}
		base = filepath.Join(userHome, ".config")
	}
	return filepath.Join(base, "rilll", "hooks"), nil
}
