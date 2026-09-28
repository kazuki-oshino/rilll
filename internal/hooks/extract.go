package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// extractTo は content を target に atomic に書き出す。
//
// アルゴリズム:
//
//  1. hooksDir() を解決し、filepath.Clean(target) が hooksDir 配下
//     (= prefix が hooksDir + Separator あるいは hooksDir 自身) にあることを
//     検証する (R4.3 / R6.3 sandbox 性)。違反時は I/O 副作用を一切起こさず
//     fmt.Errorf("hooks: target outside hooksDir: %s", target) で即時拒否する。
//  2. target の親ディレクトリを os.MkdirAll(..., 0o755) で遅延作成する (R2.1)。
//  3. <target>.tmp を os.OpenFile(..., O_CREATE|O_EXCL|O_WRONLY, 0o600) で
//     新規作成する。既に file / symlink / dir として存在する場合は EEXIST で
//     失敗するため、攻撃者が tmp filename に symlink を pre-place しても
//     symlink 先への書き込みは発生しない (design.md Security 節の防御線 / R6.3)。
//  4. content を tmp に書き込み、os.Chmod(tmpPath, 0o755) で実行ビット
//     (POSIX `x`) を含む mode 0755 を強制する (R2.2 / R2.4)。
//  5. os.Rename(tmpPath, target) で atomic 化する。中断時に partial file が
//     残らない (rename は POSIX で atomic) ことを保証する。
//
// I/O エラーは fmt.Errorf("hooks: write %s: %w", target, err) 形式で wrap して
// 返却し、errors.Unwrap で原因の OS error を取り出せる (R6.2)。
// ErrHookNotFound / ErrInvalidName とは sentinel として互いに排他である。
//
// 並行呼び出しは informational に許容する: 複数 goroutine / process が同じ
// target に対して extractTo を呼ぶと、各々が独自の tmp file を作って rename
// で last-writer-wins になる。書き込み内容が決定的 (= 同一 embed バイト列)
// である限り結果は等価である (Flow 2 / R6.4)。
func extractTo(target string, content []byte) error {
	sandboxDir, err := hooksDir()
	if err != nil {
		// hooksDir 解決失敗 (HOME も XDG_CONFIG_HOME も解けない極端ケース) も
		// I/O 失敗の一種として wrap して伝播させる。target は呼び出し側が
		// debug できるよう error message に含める。
		return fmt.Errorf("hooks: write %s: %w", target, err)
	}

	sandboxClean := filepath.Clean(sandboxDir)
	cleaned := filepath.Clean(target)

	// R4.3 / R6.3: Clean 後の path string が hooksDir 配下にあることを検証する。
	// Clean は ".." を解消するため、`hooksDir/../escape.sh` のような traversal は
	// hooksDir の親を指す絶対パスに正規化されて prefix 検証で弾かれる。
	if !isInsideDir(cleaned, sandboxClean) {
		return fmt.Errorf("hooks: target outside hooksDir: %s", target)
	}

	// R2.1: 親ディレクトリの遅延作成。hooksDir 自体が未作成の状態でも
	// MkdirAll は中間ディレクトリも合わせて 0o755 で作成する。
	parent := filepath.Dir(cleaned)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("hooks: write %s: %w", target, err)
	}

	// R6.3: tmp file の symlink pre-placement 攻撃防御。
	// O_CREATE|O_EXCL|O_WRONLY は POSIX 上 EEXIST で失敗するため、
	// tmpPath が file / symlink / directory のいずれであっても新規作成を強制する。
	// mode 0o600 は tmp の一時状態における安全側、最終的に Chmod で 0o755 へ移行する。
	tmpPath := cleaned + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("hooks: write %s: %w", target, err)
	}
	// rename 成功までは tmp が残るが、途中失敗時は best-effort で削除する。
	// rename 成功時は tmp は target に "消費" されるため Remove は no-op となる。
	cleanupTmp := func() {
		_ = os.Remove(tmpPath)
	}

	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		cleanupTmp()
		return fmt.Errorf("hooks: write %s: %w", target, err)
	}
	if err := f.Close(); err != nil {
		cleanupTmp()
		return fmt.Errorf("hooks: write %s: %w", target, err)
	}

	// R2.2 / R2.4: tmp を 0o755 に Chmod してから rename することで、
	// rename の atomic 性で「現れた瞬間に既に実行可能」な状態になる。
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		cleanupTmp()
		return fmt.Errorf("hooks: write %s: %w", target, err)
	}

	if err := os.Rename(tmpPath, cleaned); err != nil {
		cleanupTmp()
		return fmt.Errorf("hooks: write %s: %w", target, err)
	}
	return nil
}

// isInsideDir は path が dir 配下 (dir 自身も含む) にあるかを文字列レベルで判定する。
//
// path / dir はいずれも filepath.Clean 済みの絶対パスであることを前提とする。
// 単純な strings.HasPrefix だと "/foo" と "/foobar" を混同してしまうため、
// セパレータ境界も合わせて検証する (例: dir="/a/b" のとき "/a/bc" は inside ではない)。
func isInsideDir(path, dir string) bool {
	if path == dir {
		return true
	}
	sep := string(filepath.Separator)
	prefix := dir
	if !strings.HasSuffix(prefix, sep) {
		prefix += sep
	}
	return strings.HasPrefix(path, prefix)
}
