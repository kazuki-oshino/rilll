package hooks

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupHooksSandbox は extract_test 専用の sandbox helper。
// XDG_CONFIG_HOME / HOME を t.TempDir に向けて hooksDir() の戻り値を
// テスト用一時ディレクトリ配下に閉じ込める (R6.3 sandbox 性)。
//
// 戻り値はこのテストに対する hooksDir() の絶対パス (まだ未作成)。
// 親ディレクトリ自動作成 (R2.1) のテストで「未作成状態から始まる」
// 前提を担保する。
func setupHooksSandbox(t *testing.T) string {
	t.Helper()
	xdg := t.TempDir()
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", home)
	dir, err := hooksDir()
	if err != nil {
		t.Fatalf("hooksDir() error = %v", err)
	}
	return dir
}

// TestExtractCreatesTargetWithMode0755 は R2.2 / R2.4 を検証する。
// 成功時に target が mode 0755 で content と byte-equal な状態で存在し、
// 戻り値は nil となる (実行ビット 0o111 を含む)。
func TestExtractCreatesTargetWithMode0755(t *testing.T) {
	dir := setupHooksSandbox(t)
	target := filepath.Join(dir, "foo.sh")
	content := []byte("#!/usr/bin/env bash\necho ok\n")

	if err := extractTo(target, content); err != nil {
		t.Fatalf("extractTo() error = %v, want nil", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(target) error = %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("ファイル内容が一致しない:\n got = %q\nwant = %q", got, content)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat(target) error = %v", err)
	}
	perm := info.Mode().Perm()
	if perm&0o111 == 0 {
		t.Errorf("mode = %#o, want execute bits set (0o111)", perm)
	}
	if perm != 0o755 {
		t.Errorf("mode = %#o, want exactly 0o755 (R2.2)", perm)
	}
}

// TestExtractCreatesParentDir は R2.1 を検証する。
// 親ディレクトリ (hooksDir) が存在しない状態で extractTo を呼んでも
// os.MkdirAll(..., 0o755) で自動作成される。
func TestExtractCreatesParentDir(t *testing.T) {
	dir := setupHooksSandbox(t)

	// 前提条件: hooksDir 自体がまだ存在しない
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("前提条件違反: hooksDir が既に存在する (err=%v)", err)
	}

	target := filepath.Join(dir, "bar.sh")
	if err := extractTo(target, []byte("payload")); err != nil {
		t.Fatalf("extractTo() error = %v, want nil", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("hooksDir Stat: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("hooksDir is not directory")
	}

	if _, err := os.Stat(target); err != nil {
		t.Errorf("target が作成されていない: %v", err)
	}
}

// TestExtractRejectsOutsideHooksDir は R4.3 を検証する。
// hooksDir 配下でない target に対して extractTo は I/O 副作用なしで即時拒否する。
//
// 拒否時に「hooks: target outside hooksDir」プレフィックスが返り、
// ErrHookNotFound / ErrInvalidName とは sentinel として区別される。
func TestExtractRejectsOutsideHooksDir(t *testing.T) {
	_ = setupHooksSandbox(t)

	// hooksDir とは別の TempDir を作って「外」を表現する。
	// t.TempDir() は呼び出しごとにユニークなパスを返すため、
	// XDG_CONFIG_HOME / HOME とは確実に重ならない。
	outsideDir := t.TempDir()

	cases := []struct {
		name   string
		target string
	}{
		{"絶対パスで外", filepath.Join(outsideDir, "outside.sh")},
		{"hooksDir/.. 経由で外", filepath.Join(setupHooksSandbox(t), "..", "escape.sh")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := extractTo(tc.target, []byte("payload"))
			if err == nil {
				t.Fatal("extractTo() error = nil, want non-nil")
			}
			if !strings.HasPrefix(err.Error(), "hooks: target outside hooksDir") {
				t.Errorf("error message = %q, want prefix %q",
					err.Error(), "hooks: target outside hooksDir")
			}

			// R6.1 排他性: ErrHookNotFound / ErrInvalidName とは区別される
			if errors.Is(err, ErrHookNotFound) {
				t.Error("sandbox 違反 error が ErrHookNotFound として識別された (排他性違反)")
			}
			if errors.Is(err, ErrInvalidName) {
				t.Error("sandbox 違反 error が ErrInvalidName として識別された (排他性違反)")
			}

			// I/O 副作用がないこと: target も Clean 済み path も作成されない
			cleaned := filepath.Clean(tc.target)
			if _, err := os.Stat(cleaned); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("target が作成されてしまった (path=%s, err=%v)", cleaned, err)
				_ = os.Remove(cleaned)
			}
		})
	}
}

// TestExtractRejectsTmpSymlinkPrePlaced は R6.3 を検証する。
// 攻撃者が tmp filename にあらかじめ symlink を置いていた場合でも、
// O_CREATE|O_EXCL|O_WRONLY で open が失敗し、symlink 先のファイルへの
// 書き込みは発生しない (design.md Security 節で明示された防御線)。
func TestExtractRejectsTmpSymlinkPrePlaced(t *testing.T) {
	dir := setupHooksSandbox(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("hooksDir mkdir: %v", err)
	}

	target := filepath.Join(dir, "foo.sh")

	// 外部 victim ファイル (攻撃者が書き込みたい先)
	outsideDir := t.TempDir()
	victim := filepath.Join(outsideDir, "victim.txt")
	original := []byte("ORIGINAL")
	if err := os.WriteFile(victim, original, 0o644); err != nil {
		t.Fatalf("victim 配置: %v", err)
	}

	// tmp filename を予測して symlink で victim に向ける。
	// extractTo の実装は <target>.tmp を tmp 名として使う前提。
	tmpPath := target + ".tmp"
	if err := os.Symlink(victim, tmpPath); err != nil {
		t.Fatalf("symlink 配置: %v", err)
	}

	err := extractTo(target, []byte("ATTACKER_PAYLOAD"))
	if err == nil {
		t.Fatal("extractTo() error = nil, want non-nil (O_EXCL で拒否されるべき)")
	}

	// victim が改変されていないこと
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("victim ReadFile: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("victim が改変された: got %q, want %q", got, original)
	}

	// target も作成されていないこと (rename まで到達していない)
	if _, statErr := os.Stat(target); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("target が作成されてしまった: %v", statErr)
	}
}

// TestExtractAtomicNoLeftoverTmp は R2.x の atomic 書き出しの観測可能側面を検証する。
// 成功時に target ファイルだけが残り、tmp ファイルは残らない (rename 成功で消費される)。
func TestExtractAtomicNoLeftoverTmp(t *testing.T) {
	dir := setupHooksSandbox(t)
	target := filepath.Join(dir, "foo.sh")

	if err := extractTo(target, []byte("hi")); err != nil {
		t.Fatalf("extractTo() error = %v", err)
	}

	if _, err := os.Stat(target); err != nil {
		t.Errorf("target が存在しない: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("hooksDir ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("tmp file が残存している: %s", e.Name())
		}
	}
}

// TestExtractWrapsIOError は R6.2 を検証する。
// 書き込み権限のない hooksDir に対する extractTo の I/O 失敗が
// fmt.Errorf("hooks: write ...: %w", err) 形式で wrap されて返り、
// errors.Unwrap で原因 OS error を取り出せること、ErrHookNotFound /
// ErrInvalidName とは sentinel として排他であることを確認する。
func TestExtractWrapsIOError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 実行時は chmod 0o500 でも書き込みが許可されるためスキップ")
	}

	dir := setupHooksSandbox(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod read-only: %v", err)
	}
	t.Cleanup(func() {
		// テスト終了後に書き込み権限を戻す (TempDir cleanup を可能にするため)
		_ = os.Chmod(dir, 0o755)
	})

	target := filepath.Join(dir, "foo.sh")
	err := extractTo(target, []byte("hi"))
	if err == nil {
		t.Fatal("extractTo() error = nil, want non-nil")
	}

	if !strings.HasPrefix(err.Error(), "hooks: ") {
		t.Errorf("error message = %q, want prefix %q", err.Error(), "hooks: ")
	}

	if errors.Is(err, ErrHookNotFound) {
		t.Error("I/O error が ErrHookNotFound として識別された (排他性違反)")
	}
	if errors.Is(err, ErrInvalidName) {
		t.Error("I/O error が ErrInvalidName として識別された (排他性違反)")
	}

	if cause := errors.Unwrap(err); cause == nil {
		t.Error("errors.Unwrap(err) = nil, want non-nil (原因 error が wrap されていない)")
	}
}
