// Package hooks は rilll binary に同梱した既定 hook script を、
// リポジトリ override → ユーザグローバル → embed 自動展開の 3 段階で解決する。
//
// 公開 API は ResolveHookPath(cwd, name) string, error の 1 関数と、
// 解決失敗を区別するための 2 つの sentinel エラーから構成される。
//
// 解決順:
//
//  1. name が ^[a-zA-Z0-9_-]+$ を満たさない → ErrInvalidName
//     (I/O 副作用前に即時返却。path traversal 防止 / 公開契約)
//  2. cwd が空でなく <cwd>/.claude/hooks/<name>.sh が存在 → そのパスを返す
//  3. <hooksDir>/<name>.sh が存在 → 「現 embed の SHA-256」と
//     「.embed-version の Files[name] 記録 SHA-256」の per-file 比較で drift 判定
//     を行ったうえで、そのパスを返す (content-addressable; stamp.Version には
//     依存しない)。embed 内容に追従できる場合は user-global を上書きし、
//     ユーザ改変が検出された場合はディスク内容を保持する。
//  4. embed FS に <name>.sh が同梱 → user-global へ atomic 書き出し後にそのパスを返す。
//  5. いずれにも該当しない → ErrHookNotFound
//
// ユーザグローバル hook ディレクトリは XDG_CONFIG_HOME 規約に従い
// $XDG_CONFIG_HOME/rilll/hooks/ または $HOME/.config/rilll/hooks/ に解決する。
// internal/config への import は循環依存回避のため行わない (規約のみを踏襲する)。
//
// I/O エラー (権限不足 / ディスクフル / .embed-version の JSON parse 失敗 等) は
// fmt.Errorf("hooks: ...: %w", err) 形式で wrap して呼び出し側に伝播する。
// ErrHookNotFound / ErrInvalidName とは errors.Is で互いに区別可能であり、
// 3 系統 (NotFound / InvalidName / wrapped I/O) は排他である。
//
// 本パッケージはプロセス内の可変グローバル状態に依存せず、ResolveHookPath の
// 戻り値は (cwd, name, 環境変数, ファイルシステム状態) のみから決定される。
package hooks

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/kazuki-oshino/rilll/internal/version"
)

// embedFS には rilll binary に同梱する既定 hook script 群を焼き込む。
// 後続 issue が新しい hook を追加する場合は internal/hooks/hooks/<name>.sh を
// 1 ファイル足すだけで ResolveHookPath から解決可能になるよう、
// glob パターンで宣言する (R5.2)。
//
//go:embed hooks/*.sh
var embedFS embed.FS

// ErrHookNotFound は repo override / user global / embed のいずれにも
// 該当 hook が存在しないことを示す sentinel。
//
// errors.Is(err, ErrHookNotFound) で識別可能 (R6.1)。
// I/O エラー (権限不足・ディスクフル等) は ErrHookNotFound と区別され、
// fmt.Errorf("hooks: ...: %w", err) 形式で wrap して伝播する (R6.2)。
var ErrHookNotFound = errors.New("hooks: not found")

// ErrInvalidName は name 引数が公開契約 (^[a-zA-Z0-9_-]+$) を満たさない
// ことを示す sentinel。
//
// 全ての I/O 副作用 (ディレクトリ作成 / ファイル書き出し / stamp 更新) が
// 発生する前に即時返却される。Tier 1 (repo override) と Tier 2/3
// (user-global / embed) の両経路に共通の path traversal 防御線として機能する。
var ErrInvalidName = errors.New("hooks: invalid name")

// nameRegexp は ResolveHookPath の name 引数公開契約 ^[a-zA-Z0-9_-]+$ を表す
// パッケージ起動時に 1 回だけ MustCompile される immutable な参照値 (R6.4)。
//
// regexp.Regexp 自体は内部で並行安全な状態管理を行うため、複数 goroutine から
// MatchString を並行に呼んでも問題ない (公式 doc 保証)。本パッケージは
// 「プロセス内グローバル可変状態を持たない」契約 (R6.4) を保ちたいので、
// この変数は const-equivalent な「初期化後に書き換えられない値」として扱う。
var nameRegexp = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ResolveHookPath は hook 名を 3 段解決し、呼び出し可能な絶対パスを返す。
//
// 解決順:
//
//  1. name が ^[a-zA-Z0-9_-]+$ を満たさない → ErrInvalidName を即時返却
//     (I/O 副作用前 / path traversal 防止 / Tier 1, Tier 2, Tier 3 にも一切触らない)
//  2. cwd が空でなく <cwd>/.claude/hooks/<name>.sh が存在 → そのパスを返す (Tier 1)
//  3. <hooksDir>/<name>.sh が存在 → drift 判定後に user-global パスを返す (Tier 2)
//  4. embed FS に <name>.sh が同梱 → user-global へ atomic 書き出し後に返す (Tier 3)
//  5. いずれにも該当しない → ErrHookNotFound
//
// 本関数の戻り値は (cwd, name, 環境変数, ファイルシステム状態) のみから決定される
// 純粋関数として振る舞う (R6.4)。パッケージ内の可変グローバル状態には依存しない。
//
// 実装ステータス: Tier 0 (name 検証) / Tier 1 (repo override) / Tier 2 (per-file
// content-addressable drift 検知 + 改変保護) / Tier 3 (embed → user-global 自動
// 展開 + .embed-version 記録) まで組み込まれている (task 3.1, 3.2, 3.3, 3.4)。
// I/O エラー wrap の網羅は後続 task (3.5) で総点検される。
func ResolveHookPath(cwd, name string) (string, error) {
	// Tier 0: name の正規表現検証 (Security / 公開契約 / R6.1 区別可能性)。
	// 違反時は ディレクトリ作成 / ファイル書き出し / stamp 更新 のいずれも発生しない。
	// regex はパッケージ初期化時に MustCompile 済みのため、ここでの compile コストはない。
	if !nameRegexp.MatchString(name) {
		return "", ErrInvalidName
	}

	// Tier 1: repo override <cwd>/.claude/hooks/<name>.sh の同期読み取り (R1.1, R1.5)。
	// cwd が空文字列のときは Tier 1 探索をスキップし、user-global / embed のみで解決する
	// (R1.5)。filepath.Join("", ...) は相対パスになり得るため、cwd の skip 判定は
	// Join の前に行う必要がある。
	//
	// os.Stat が ErrNotExist を返す場合は Tier 2 にフォールスルーする。それ以外の
	// I/O エラー (権限不足等) は ErrHookNotFound と区別するために wrap して返却する
	// (R6.2 と整合 / 排他性は task 3.5 のテスト 8 で網羅検証される)。
	if cwd != "" {
		repoPath := filepath.Join(cwd, ".claude", "hooks", name+".sh")
		if _, err := os.Stat(repoPath); err == nil {
			return repoPath, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("hooks: stat repo override %s: %w", repoPath, err)
		}
	}

	// hooksDir 解決 (Tier 2 / Tier 3 共通)。
	// 解決失敗 (HOME / XDG_CONFIG_HOME いずれも解けない極端ケース) は I/O 失敗の
	// 一種として ErrHookNotFound と区別して伝播させる (R6.2)。
	dir, err := hooksDir()
	if err != nil {
		return "", fmt.Errorf("hooks: resolve hooksDir: %w", err)
	}
	userPath := filepath.Join(dir, name+".sh")

	// user-global <hooksDir>/<name>.sh の存在確認 (R1.2 / R1.3 の分岐起点)。
	// ErrNotExist は「ファイルなし」として Tier 3 (embed → 自動展開) に進む。
	// それ以外の I/O エラーは ErrHookNotFound と区別するため wrap して伝播する (R6.2)。
	_, statErr := os.Stat(userPath)
	userExists := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return "", fmt.Errorf("hooks: stat user-global %s: %w", userPath, statErr)
	}

	// embed FS から <name>.sh を試読する。同梱されていれば Tier 2 の per-file SHA
	// 比較 / Tier 3 の自動展開のいずれにも使う (currentEmbedSHA = sha256(embedContent))。
	// embed FS は in-memory のため fs.ErrNotExist 以外のエラーは通常発生しないが、
	// 防御的に「embed なし扱い」へ丸めて続行する (task 3.2 で確立済の方針を踏襲)。
	embedPath := "hooks/" + name + ".sh"
	embedContent, embedErr := embedFS.ReadFile(embedPath)
	embedHasName := embedErr == nil

	// 全 tier 不在 (user-global なし + embed なし) → ErrHookNotFound (R1.4 / R6.1)。
	// 副作用が発生する前に sentinel を返すこと。
	if !userExists && !embedHasName {
		return "", ErrHookNotFound
	}

	// user-global は既存だが embed FS に該当 name が無いケース (= ユーザ独自 hook /
	// 旧 binary 由来の legacy hook 等)。drift 比較は不能なので read-only で返す。
	// stamp も触らない (記録は将来 embed が同名 hook を取り込んだ時点での再評価で
	// 行えばよく、本 spec の責務外)。
	if userExists && !embedHasName {
		return userPath, nil
	}

	// この時点で embedHasName == true (embedContent / embedSHA を扱える)。
	currentEmbedSHA := sha256Bytes(embedContent)

	if !userExists {
		// Tier 3: embed → user-global 自動展開 (R1.3 / R2.1 / R2.2 / R2.4 / R2.5)。
		// extractTo は親ディレクトリ (hooksDir) の自動作成 / sandbox 検証 / atomic
		// 書き出し / mode 0o755 の Chmod を一手に担う。I/O 失敗時は内部で
		// fmt.Errorf("hooks: write %s: %w", ...) 形式に wrap 済みのため、本関数では
		// 戻り値をそのまま伝播する (R6.2)。
		if err := extractTo(userPath, embedContent); err != nil {
			return "", err
		}

		// .embed-version への per-file 記録 (R3.1 / R3.5 / R3.6)。
		// 既存 stamp に他 hook の Files 記録が含まれる場合があるため、loadStamp で
		// 読み出して Files map を保持しつつ、Files[name] のみを差し替える
		// (per-file 独立性 / Critical Issue 2 構造的回避)。stamp が不在の場合は
		// loadStamp がゼロ値 (Version: "", Files: nil) を返すので、Files が nil
		// のときだけ make でマップを初期化する。
		stamp, err := loadStamp(dir)
		if err != nil {
			return "", err
		}
		if stamp.Files == nil {
			stamp.Files = make(map[string]string)
		}
		stamp.Files[name] = currentEmbedSHA
		stamp.Version = version.BuildVersion
		if err := saveStamp(dir, stamp); err != nil {
			return "", err
		}
		return userPath, nil
	}

	// ─── Tier 2: user-global 既存 + embed 同梱あり → per-file content-addressable
	// drift 判定と改変保護 (R3.2 / R3.3 / R3.4 / R3.5 / R3.6) ───
	//
	// drift 判定は stamp.Version には依存させず、Files[name] と currentEmbedSHA の
	// per-file SHA 比較で行う (Open Question 1 構造的解消 / R3.6)。stamp.Version は
	// 追従処理完了時の informational record として書き込むのみ。
	stamp, err := loadStamp(dir)
	if err != nil {
		return "", err
	}
	recordedSHA, recorded := stamp.Files[name]

	switch {
	case !recorded:
		// Case A: Files[name] 未記録 + user-global 既存 (旧バージョン or 手動配置)。
		// manual placement 保護 (R3.4 を保守的に適用) として、ディスクの SHA を
		// 計算して Files[name] に記録する。ファイルは上書きしない (返却するだけ)。
		diskSHA, err := sha256File(userPath)
		if err != nil {
			return "", err
		}
		if stamp.Files == nil {
			stamp.Files = make(map[string]string)
		}
		stamp.Files[name] = diskSHA
		stamp.Version = version.BuildVersion
		if err := saveStamp(dir, stamp); err != nil {
			return "", err
		}

	case recordedSHA == currentEmbedSHA:
		// Case B: 整合状態 (R3.2)。再書き出しせず stamp も触らずそのまま返却する。
		// stamp.Version の更新も行わない (drift 検知は per-file SHA で行うため
		// Version の bookkeeping は追従処理発生時のみで十分 / 不要 I/O の抑制)。

	default:
		// Files[name] != currentEmbedSHA (= embed drift 検知)。
		// 次にディスク内容が記録 SHA と一致するか (= ユーザ未改変か) を判定する。
		diskSHA, err := sha256File(userPath)
		if err != nil {
			return "", err
		}
		if diskSHA == recordedSHA {
			// Case C: drift + 未改変 (R3.3)。新しい embed 内容で上書き、
			// Files[name] を currentEmbedSHA に更新する。
			if err := extractTo(userPath, embedContent); err != nil {
				return "", err
			}
			stamp.Files[name] = currentEmbedSHA
		} else {
			// Case D: drift + ユーザ改変あり (R3.4)。ファイルは保持し、
			// Files[name] をディスク現状の SHA に更新する。これにより次回
			// 評価時には Case B (recordedSHA == currentEmbedSHA でない限り
			// recordedSHA == diskSHA) として扱われ、再評価が収束する。
			stamp.Files[name] = diskSHA
		}
		stamp.Version = version.BuildVersion
		if err := saveStamp(dir, stamp); err != nil {
			return "", err
		}
	}

	return userPath, nil
}
