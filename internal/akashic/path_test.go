package akashic

import (
	"path/filepath"
	"strings"
	"testing"
)

// resolveSetup はテスト共通の前処理。
// 旧 / 新 env の競合干渉を排除しつつ、sync.Once をリセットして warning 発火を再現可能にする。
func resolveSetup(t *testing.T) {
	t.Helper()
	// XDG_DATA_HOME はテストごとに独立したディレクトリを与える (default 経路の安定化)。
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	// 各ケースで明示的に設定するため、デフォルトは未設定状態に揃える。
	t.Setenv(DBEnvName, "")
	t.Setenv(LegacyDBEnvName, "")
	resetDeprecationWarningOnce()
}

func TestResolveDBPathPrefersNewEnv(t *testing.T) {
	resolveSetup(t)
	t.Setenv(DBEnvName, "/tmp/akashic-new.sqlite")

	got := ResolveDBPath()
	if got.Path != "/tmp/akashic-new.sqlite" {
		t.Fatalf("unexpected path: %q", got.Path)
	}
	if got.Warning != "" {
		t.Fatalf("expected no warning, got %q", got.Warning)
	}
}

func TestResolveDBPathFallsBackToLegacyEnv(t *testing.T) {
	resolveSetup(t)
	t.Setenv(LegacyDBEnvName, "/tmp/akashic-legacy.sqlite")

	got := ResolveDBPath()
	if got.Path != "/tmp/akashic-legacy.sqlite" {
		t.Fatalf("unexpected path: %q", got.Path)
	}
	if got.Warning == "" {
		t.Fatal("expected deprecation warning, got empty")
	}
	if !strings.Contains(got.Warning, LegacyDBEnvName) {
		t.Fatalf("warning should mention legacy env name, got %q", got.Warning)
	}
}

func TestResolveDBPathPrefersNewWhenBothSet(t *testing.T) {
	resolveSetup(t)
	t.Setenv(DBEnvName, "/tmp/akashic-new.sqlite")
	t.Setenv(LegacyDBEnvName, "/tmp/akashic-legacy.sqlite")

	got := ResolveDBPath()
	if got.Path != "/tmp/akashic-new.sqlite" {
		t.Fatalf("expected new path to win, got %q", got.Path)
	}
	if got.Warning == "" {
		t.Fatal("expected warning describing legacy ignored")
	}
	// 両方時の warning は「旧が無視される」旨を含むこと (Req 3.4)
	if !strings.Contains(got.Warning, LegacyDBEnvName) {
		t.Fatalf("warning should mention legacy env name, got %q", got.Warning)
	}
	if !strings.Contains(got.Warning, DBEnvName) {
		t.Fatalf("warning should mention new env name, got %q", got.Warning)
	}
}

func TestResolveDBPathTreatsEmptyAsUnset(t *testing.T) {
	resolveSetup(t)
	// 既に resolveSetup で空文字列を設定済み (両方とも未設定扱い)。

	got := ResolveDBPath()
	// 新も旧も未設定 → XDG default を採用、warning なし
	if !strings.HasSuffix(got.Path, filepath.Join("rilll", defaultDBName)) {
		t.Fatalf("expected default akashic path, got %q", got.Path)
	}
	if got.Warning != "" {
		t.Fatalf("expected no warning when both unset, got %q", got.Warning)
	}
}

func TestResolveDBPathTreatsBlankAsUnset(t *testing.T) {
	resolveSetup(t)
	// 空白のみの値は未設定として扱う (Req 3.5)
	t.Setenv(DBEnvName, "   ")
	t.Setenv(LegacyDBEnvName, "\t  \n")

	got := ResolveDBPath()
	if !strings.HasSuffix(got.Path, filepath.Join("rilll", defaultDBName)) {
		t.Fatalf("expected default akashic path for blank values, got %q", got.Path)
	}
	if got.Warning != "" {
		t.Fatalf("expected no warning when both blank, got %q", got.Warning)
	}
}

func TestResolveDBPathDeprecationWarningEmittedOnlyOnce(t *testing.T) {
	resolveSetup(t)
	t.Setenv(LegacyDBEnvName, "/tmp/akashic-legacy.sqlite")

	first := ResolveDBPath()
	if first.Warning == "" {
		t.Fatal("expected first call to emit warning")
	}

	second := ResolveDBPath()
	if second.Warning != "" {
		t.Fatalf("expected second call to suppress warning via sync.Once, got %q", second.Warning)
	}
	// 採用される path 自体は同じ値であり続ける
	if second.Path != first.Path {
		t.Fatalf("path should be stable across calls: first=%q second=%q", first.Path, second.Path)
	}
}

func TestResolveDBPathBothSetWarningEmittedOnlyOnce(t *testing.T) {
	resolveSetup(t)
	t.Setenv(DBEnvName, "/tmp/akashic-new.sqlite")
	t.Setenv(LegacyDBEnvName, "/tmp/akashic-legacy.sqlite")

	first := ResolveDBPath()
	if first.Warning == "" {
		t.Fatal("expected first call to emit warning")
	}

	second := ResolveDBPath()
	if second.Warning != "" {
		t.Fatalf("expected second call to suppress warning, got %q", second.Warning)
	}
}

func TestProjectIDFromCWDReturnsBaseName(t *testing.T) {
	cases := []struct {
		name string
		cwd  string
		want string
	}{
		{"絶対パス", "/workspace/example/rilll", "rilll"},
		{"末尾スラッシュ付き", "/workspace/example/rilll/", "rilll"},
		{"単一セグメント", "/rilll", "rilll"},
		{"空文字列は unknown", "", UnknownProjectID},
		{"空白のみは unknown", "   ", UnknownProjectID},
		{"ルート単独は unknown", "/", UnknownProjectID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ProjectIDFromCWD(tc.cwd)
			if got != tc.want {
				t.Fatalf("ProjectIDFromCWD(%q) = %q, want %q", tc.cwd, got, tc.want)
			}
		})
	}
}

func TestProjectIDFromCWDRelativePathResolvesToCurrentDirBase(t *testing.T) {
	// "." を渡すと cwd の basename を返すこと (Abs 経由の正規化)。
	got := ProjectIDFromCWD(".")
	if got == "" || got == UnknownProjectID {
		t.Fatalf("expected non-empty / non-unknown project id from \".\", got %q", got)
	}
}

func TestDefaultDBPathReturnsAkashicSqliteByDefault(t *testing.T) {
	resolveSetup(t)
	got := DefaultDBPath()
	if !strings.HasSuffix(got, filepath.Join("rilll", "akashic.sqlite")) {
		t.Fatalf("expected default path to end with rilll/akashic.sqlite, got %q", got)
	}
}

func TestDefaultDBPathDiscardsWarning(t *testing.T) {
	resolveSetup(t)
	t.Setenv(LegacyDBEnvName, "/tmp/akashic-legacy.sqlite")

	// DefaultDBPath は warning を返さない簡易 API。
	// 副作用として sync.Once を消費するため、後続の ResolveDBPath では warning が出ない。
	if got := DefaultDBPath(); got != "/tmp/akashic-legacy.sqlite" {
		t.Fatalf("unexpected default path: %q", got)
	}
	// warning が一度消費されたことを確認
	res := ResolveDBPath()
	if res.Warning != "" {
		t.Fatalf("expected sync.Once to be consumed by DefaultDBPath, got warning %q", res.Warning)
	}
}
