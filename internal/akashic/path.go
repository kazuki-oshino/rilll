package akashic

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// DBEnvName は SQLite アカシックレコード DB パスを上書きする主環境変数。
	DBEnvName = "RILLL_AKASHIC_DB"
	// LegacyDBEnvName は旧名の環境変数。後方互換のため fallback として読み取る。
	// 設定されていた場合はプロセス起動ごと 1 度だけ deprecation warning を出力する。
	LegacyDBEnvName = "RILLL_OBSERVABILITY_DB"
	// defaultDBName はデフォルト DB ファイル名。
	defaultDBName = "akashic.sqlite"
)

// PathResult は ResolveDBPath の解決結果。
//   - Path: OpenSQLite に渡せる絶対パス
//   - Warning: deprecation 通知 (空文字列 = warning なし)
type PathResult struct {
	Path    string
	Warning string
}

// deprecationWarningOnce は同一プロセス内での deprecation warning 出力を 1 度に制限する。
// テストから resetDeprecationWarningOnce() でリセットできる。
var deprecationWarningOnce sync.Once

// resetDeprecationWarningOnce はテスト用の内部 helper。
// 同一プロセス内で複数のテストケースが warning 発火を検証する場合に使用する。
// 公開 API ではない (export しない)。
func resetDeprecationWarningOnce() {
	deprecationWarningOnce = sync.Once{}
}

// DefaultDBPath は ResolveDBPath().Path と等価な後方互換 API。
// deprecation warning は破棄されるため、warning を回収したい呼び出し側は
// ResolveDBPath を使うこと。なお内部の sync.Once は本関数の呼び出しでも消費される。
func DefaultDBPath() string {
	return ResolveDBPath().Path
}

// ResolveDBPath は env (`RILLL_AKASHIC_DB` / `RILLL_OBSERVABILITY_DB`) を順に評価し、
// 採用 path と (必要なら) deprecation warning を返す。
//
// 採用ルール:
//   - 新 env のみ設定        → 新値採用、warning なし
//   - 旧 env のみ設定        → 旧値採用、deprecation warning
//   - 両方設定               → 新値採用、旧無視を明示する warning
//   - 両方未設定 / 空 / 空白 → XDG default (`$XDG_DATA_HOME/rilll/akashic.sqlite`)
//
// `strings.TrimSpace` 結果が空の値は未設定として扱う (Req 3.5)。
// warning は同一プロセス内で sync.Once により 1 度のみ出力される (Req 3.3)。
func ResolveDBPath() PathResult {
	newVal := strings.TrimSpace(os.Getenv(DBEnvName))
	oldVal := strings.TrimSpace(os.Getenv(LegacyDBEnvName))

	switch {
	case newVal != "" && oldVal != "":
		// 両方設定時は新を採用、旧は無視される旨を warning に含める。
		msg := "環境変数 " + LegacyDBEnvName + " は無視されました (" + DBEnvName + " が設定されているため)。" + LegacyDBEnvName + " は将来削除されます。"
		return PathResult{Path: newVal, Warning: emitDeprecationWarning(msg)}
	case newVal != "":
		return PathResult{Path: newVal}
	case oldVal != "":
		// 旧のみ設定時は旧値を採用しつつ、deprecation warning を出力。
		msg := "環境変数 " + LegacyDBEnvName + " は deprecated です。" + DBEnvName + " への移行を推奨します。"
		return PathResult{Path: oldVal, Warning: emitDeprecationWarning(msg)}
	default:
		return PathResult{Path: defaultDataPath()}
	}
}

// emitDeprecationWarning は sync.Once を介して warning 文字列を 1 度だけ返す。
// 2 回目以降の呼び出しは空文字列を返す (Req 3.3)。
func emitDeprecationWarning(msg string) string {
	var out string
	deprecationWarningOnce.Do(func() {
		out = msg
	})
	return out
}

// UnknownProjectID は ProjectIDFromCWD が cwd から有効な project ID を取り出せなかったときの sentinel 値。
// 空 cwd / "/" / "." 等、ベースネームが取れないケースに使われる。
const UnknownProjectID = "unknown"

// ProjectIDFromCWD は cwd の絶対パス basename を project_id として返す。
//
// 設計意図:
//   - グローバル akashic SQLite に対し「プロジェクトごとの絞り込み」を実現するためのキー。
//   - 衝突可能性は受け入れる (例: 別マシンから同名 repo を持ち込まれた場合)。
//     現状の運用 (1 マシン / 1 repo = 1 ディレクトリ名) では十分。
//
// ルール:
//  1. 入力 cwd を `filepath.Abs` で正規化する (`.` / 相対パスでも安定動作)。
//  2. `filepath.Base` で末尾セグメントを取り出す。
//  3. 取り出したセグメントが "." / "/" / "" のいずれかなら UnknownProjectID を返す。
//
// 副作用なし (純関数)。Abs 失敗時はフォールバックとして cwd 生値の Base を試み、
// それも空ならば UnknownProjectID を返す。
func ProjectIDFromCWD(cwd string) string {
	trimmed := strings.TrimSpace(cwd)
	if trimmed == "" {
		return UnknownProjectID
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		// Abs 失敗時のフォールバック: 生値の Base を試す
		abs = trimmed
	}
	base := filepath.Base(abs)
	switch base {
	case ".", "/", "":
		return UnknownProjectID
	}
	return base
}

// defaultDataPath は env 未設定時に採用するデフォルト path を返す。
// XDG_DATA_HOME を尊重し、未設定時は `~/.local/share/rilll/akashic.sqlite`。
func defaultDataPath() string {
	dataHome := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
	if dataHome == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			userHome = "."
		}
		dataHome = filepath.Join(userHome, ".local", "share")
	}
	return filepath.Join(dataHome, "rilll", defaultDBName)
}
