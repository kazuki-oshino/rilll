package hooks

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/config"
)

// unsetEnvForTest は env var を test 期間中だけ unset し、test 終了時に元の状態
// (set / unset いずれも) へ復元する helper。
//
// t.Setenv は "値を上書きする" 用途しか提供しないため、明示的な unset 検証
// (R4.2 の "未設定" ケース) では本 helper を使用する。
func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	orig, ok := os.LookupEnv(key)
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv(key, orig)
			return
		}
		_ = os.Unsetenv(key)
	})
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("os.Unsetenv(%q): %v", key, err)
	}
}

// TestHooksDirUsesXDGWhenSet は R4.1 を検証する。
// XDG_CONFIG_HOME が空でない値で設定されている場合、
// hooksDir() は <XDG_CONFIG_HOME>/rilll/hooks を返す。
func TestHooksDirUsesXDGWhenSet(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	// HOME も sandbox 化することで、誤って HOME fallback 経路に流れた場合の影響を遮断する。
	t.Setenv("HOME", t.TempDir())

	got, err := hooksDir()
	if err != nil {
		t.Fatalf("hooksDir() error = %v, want nil", err)
	}

	want := filepath.Join(xdg, "rilll", "hooks")
	if got != want {
		t.Errorf("hooksDir() = %q, want %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("hooksDir() = %q, want absolute path", got)
	}
}

// TestHooksDirFallbackHomeWhenXDGEmpty は R4.2 を検証する。
// XDG_CONFIG_HOME が空文字列で設定されている場合、
// hooksDir() は $HOME/.config/rilll/hooks にフォールバックする。
func TestHooksDirFallbackHomeWhenXDGEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)

	got, err := hooksDir()
	if err != nil {
		t.Fatalf("hooksDir() error = %v, want nil", err)
	}

	want := filepath.Join(home, ".config", "rilll", "hooks")
	if got != want {
		t.Errorf("hooksDir() = %q, want %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("hooksDir() = %q, want absolute path", got)
	}
}

// TestHooksDirFallbackHomeWhenXDGUnset は R4.2 を検証する。
// XDG_CONFIG_HOME が未設定 (env から削除されている) 場合、
// hooksDir() は $HOME/.config/rilll/hooks にフォールバックする。
func TestHooksDirFallbackHomeWhenXDGUnset(t *testing.T) {
	home := t.TempDir()
	unsetEnvForTest(t, "XDG_CONFIG_HOME")
	t.Setenv("HOME", home)

	got, err := hooksDir()
	if err != nil {
		t.Fatalf("hooksDir() error = %v, want nil", err)
	}

	want := filepath.Join(home, ".config", "rilll", "hooks")
	if got != want {
		t.Errorf("hooksDir() = %q, want %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("hooksDir() = %q, want absolute path", got)
	}
}

// TestHooksDirEquivalentToConfigXDG は R4.4 の regression。
// 同一 env 状態で hooksDir() のディレクトリルート (<XDG or HOME/.config>/rilll/) が
// internal/config.Path() の親ディレクトリと一致することを、出力の同値性のみで確認する。
//
// 本テストは internal/config を import するが、これは検証用 (test 側) のみであり、
// 本 package 実装側は internal/config を import しない (循環依存回避)。
func TestHooksDirEquivalentToConfigXDG(t *testing.T) {
	cases := []struct {
		name string
		env  func(t *testing.T)
	}{
		{
			name: "XDG_CONFIG_HOME set",
			env: func(t *testing.T) {
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				t.Setenv("HOME", t.TempDir())
			},
		},
		{
			name: "XDG_CONFIG_HOME empty",
			env: func(t *testing.T) {
				t.Setenv("XDG_CONFIG_HOME", "")
				t.Setenv("HOME", t.TempDir())
			},
		},
		{
			name: "XDG_CONFIG_HOME unset",
			env: func(t *testing.T) {
				unsetEnvForTest(t, "XDG_CONFIG_HOME")
				t.Setenv("HOME", t.TempDir())
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.env(t)

			hookPath, err := hooksDir()
			if err != nil {
				t.Fatalf("hooksDir() error = %v, want nil", err)
			}
			cfgPath := config.Path() // 期待値: <root>/rilll/config.json

			// 親ディレクトリの一致を比較する:
			//   hooksDir()   = <root>/rilll/hooks       → 親は <root>/rilll
			//   config.Path() = <root>/rilll/config.json → 親は <root>/rilll
			hooksParent := filepath.Dir(hookPath)
			cfgParent := filepath.Dir(cfgPath)
			if hooksParent != cfgParent {
				t.Errorf("ディレクトリルート不一致 (R4.4): hooksDir parent = %q, config.Path parent = %q",
					hooksParent, cfgParent)
			}

			// hooksDir の末尾セグメントは "hooks" であること
			if base := filepath.Base(hookPath); base != "hooks" {
				t.Errorf("hooksDir() base = %q, want %q", base, "hooks")
			}
		})
	}
}
