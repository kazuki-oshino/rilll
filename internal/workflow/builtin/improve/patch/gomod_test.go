// Package improvepatch のテスト本ファイルは GoModInspector interface 契約の
// 構造的検証 (interface 充足性 + 戻り値タプル形状) と、既定実装
// (DefaultGoModInspector) の挙動契約 (cwd から walk up + module 宣言パース +
// `github.com/kazuki-oshino/rilll` 一致検証 + moduleRoot 解決) の双方を行う
// (tasks.md 2.2, 3.1 / design.md §5 Resolved Decisions / Req 1.4, 1.6)。
//
// 検証観点 (interface 契約):
//   - GoModInspector が exported な interface としてパッケージ外から利用可能であること
//   - ModulePath のシグネチャが (ctx, cwd) → (modulePath, moduleRoot, err) であること
//   - パッケージ外で fake 実装を型定義し、GoModInspector に代入可能であること
//
// 検証観点 (既定実装 5 ケース / task 3.1):
//  1. repo root に直接 cwd
//  2. `internal/workflow` サブディレクトリに cwd (walk up で repo root の go.mod を発見)
//  3. 別 module repo (module path が `github.com/kazuki-oshino/rilll` 以外)
//  4. `go.mod` がツリー上に存在しない
//  5. `go.mod` パース不能 (`module` 宣言行欠落)
package patch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeGoModInspector は GoModInspector interface を満たす最小 fake 実装。
// テスト用途のみで、本物の go.mod パースは行わない。後続タスク (2.3 precheck /
// 2.4 negative test / 11.1 orchestration テスト) で同型 fake を再利用する想定。
type fakeGoModInspector struct {
	modulePath string
	moduleRoot string
	err        error

	// callCount は fake が呼ばれた回数を記録する (副作用契約検証で利用)。
	callCount int
}

// ModulePath は fake が保持する固定値を返す。引数は型シグネチャ検証のためだけに受け取る。
func (f *fakeGoModInspector) ModulePath(ctx context.Context, cwd string) (string, string, error) {
	f.callCount++
	return f.modulePath, f.moduleRoot, f.err
}

// 型レベル assertion: fakeGoModInspector が GoModInspector interface を満たすことを
// コンパイル時に保証する。task 2.2 観察可能完了条件「fake 実装が型として定義可能」を機械検証する。
var _ GoModInspector = (*fakeGoModInspector)(nil)

// TestGoModInspector_FakeSatisfiesInterface は fake 実装が GoModInspector interface を満たし、
// ModulePath の戻り値タプル (modulePath, moduleRoot, err) が呼び出し側に正しく伝播することを
// 構造的に検証する。
//
// 既定実装の挙動 (walk up / module 宣言パース / `github.com/kazuki-oshino/rilll` 一致検証)
// は task 3.1 で実装され、そちらでケースカバレッジを取る。
func TestGoModInspector_FakeSatisfiesInterface(t *testing.T) {
	const (
		wantModulePath = "github.com/kazuki-oshino/rilll"
		wantModuleRoot = "/abs/path/to/rilll"
	)

	// fake を GoModInspector 型として宣言し、interface 経由で呼び出せることを検証する。
	var inspector GoModInspector = &fakeGoModInspector{
		modulePath: wantModulePath,
		moduleRoot: wantModuleRoot,
		err:        nil,
	}

	gotModulePath, gotModuleRoot, err := inspector.ModulePath(context.Background(), "/some/cwd")
	if err != nil {
		t.Fatalf("ModulePath: 予期しない error: %v", err)
	}
	if gotModulePath != wantModulePath {
		t.Errorf("modulePath: got %q, want %q", gotModulePath, wantModulePath)
	}
	if gotModuleRoot != wantModuleRoot {
		t.Errorf("moduleRoot: got %q, want %q", gotModuleRoot, wantModuleRoot)
	}
}

// TestGoModInspector_FakePropagatesError は fake が返した error がそのまま呼び出し側に
// 届くことを検証する。後続 task 2.3 (precheck) / 2.4 (negative test) で error 経路の
// `precondition_failed` verdict マッピングを行う際の前提となる。
func TestGoModInspector_FakePropagatesError(t *testing.T) {
	wantErr := errors.New("improvepatch: fake module path mismatch")

	var inspector GoModInspector = &fakeGoModInspector{
		modulePath: "",
		moduleRoot: "",
		err:        wantErr,
	}

	gotModulePath, gotModuleRoot, err := inspector.ModulePath(context.Background(), "/some/cwd")
	if !errors.Is(err, wantErr) {
		t.Errorf("error: got %v, want %v (errors.Is で一致)", err, wantErr)
	}
	if gotModulePath != "" {
		t.Errorf("error 経路では modulePath は空文字列を期待: got %q", gotModulePath)
	}
	if gotModuleRoot != "" {
		t.Errorf("error 経路では moduleRoot は空文字列を期待: got %q", gotModuleRoot)
	}
}

// rillGoMod は本リポジトリと同じ module path を持つ go.mod の最小内容。
// fixture repo 構築時に repo root へ書き出して「rilll リポジトリ相当の cwd」を再現する。
const rillGoMod = `module github.com/kazuki-oshino/rilll

go 1.25.0
`

// foreignGoMod は別 module path を持つ go.mod の最小内容 (cwd identity 不一致ケース用)。
const foreignGoMod = `module example.com/some/other-project

go 1.25.0
`

// brokenGoMod は `module` 宣言行が完全に欠落している不正な go.mod (パース不能ケース用)。
// 本ケースでは require ブロックのみを書き、`module` 宣言が無いため既定実装が error を返す
// 構造的契約 (Resolved Decisions §5 / tasks.md 3.1 ケース 5) を検証する。
const brokenGoMod = `go 1.25.0

require (
	example.com/foo v1.0.0
)
`

// writeFixtureGoMod は fixture repo root に go.mod を書き出す helper。
// 5 ケーステストはいずれも `t.TempDir()` を repo root として、必要なら `internal/workflow`
// サブディレクトリも作成する。
func writeFixtureGoMod(t *testing.T, dir string, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(content), 0o644); err != nil {
		t.Fatalf("write go.mod fixture: %v", err)
	}
}

// TestDefaultGoModInspector_ModulePath は既定実装 (NewGoModInspector) の 5 ケース挙動を
// 網羅する (tasks.md 3.1 / design.md §5 Resolved Decisions / Req 1.4, 1.6)。
//
// 観察可能完了条件: 5 ケース全てで期待される (modulePath, moduleRoot, err) を返すテストが PASS する。
func TestDefaultGoModInspector_ModulePath(t *testing.T) {
	ctx := context.Background()

	t.Run("repo root に直接 cwd を渡したケース", func(t *testing.T) {
		// repo root 直下に rilll 相当の go.mod を置き、cwd = repo root として呼び出す。
		// 期待: modulePath = github.com/kazuki-oshino/rilll / moduleRoot = repo root の絶対 clean path / err = nil
		repoRoot := t.TempDir()
		writeFixtureGoMod(t, repoRoot, rillGoMod)

		inspector := NewGoModInspector()
		gotModulePath, gotModuleRoot, err := inspector.ModulePath(ctx, repoRoot)
		if err != nil {
			t.Fatalf("予期しない error: %v", err)
		}
		if gotModulePath != "github.com/kazuki-oshino/rilll" {
			t.Errorf("modulePath: got %q, want %q", gotModulePath, "github.com/kazuki-oshino/rilll")
		}
		// moduleRoot は go.mod が見つかったディレクトリの絶対 clean path。
		// `t.TempDir()` は既に絶対パスを返すが、シンボリックリンク解決後の比較で
		// 偽陽性を避けるため filepath.Clean を経由した値を期待値とする。
		wantRoot := filepath.Clean(repoRoot)
		if gotModuleRoot != wantRoot {
			t.Errorf("moduleRoot: got %q, want %q", gotModuleRoot, wantRoot)
		}
	})

	t.Run("internal/workflow サブディレクトリ cwd で walk up により repo root の go.mod を発見", func(t *testing.T) {
		// repo root の go.mod を置いた上で、cwd は深いサブディレクトリ (internal/workflow) を渡す。
		// 期待: walk up で repo root の go.mod に到達し、moduleRoot は repo root を返す。
		repoRoot := t.TempDir()
		writeFixtureGoMod(t, repoRoot, rillGoMod)

		subDir := filepath.Join(repoRoot, "internal", "workflow")
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			t.Fatalf("mkdir subdir: %v", err)
		}

		inspector := NewGoModInspector()
		gotModulePath, gotModuleRoot, err := inspector.ModulePath(ctx, subDir)
		if err != nil {
			t.Fatalf("予期しない error: %v", err)
		}
		if gotModulePath != "github.com/kazuki-oshino/rilll" {
			t.Errorf("modulePath: got %q, want %q", gotModulePath, "github.com/kazuki-oshino/rilll")
		}
		// moduleRoot はサブディレクトリではなく walk up 先の repo root であることを検証。
		wantRoot := filepath.Clean(repoRoot)
		if gotModuleRoot != wantRoot {
			t.Errorf("moduleRoot: got %q, want %q (walk up が repo root を返すべき)", gotModuleRoot, wantRoot)
		}
	})

	t.Run("別 module path の go.mod を持つ repo (cwd identity 不一致)", func(t *testing.T) {
		// `module example.com/some/other-project` を持つ別 module の cwd で呼び出す。
		// 期待: error が返り、modulePath / moduleRoot は空文字列 (interface 契約)。
		// orchestration 側はこの error を VerdictPreconditionFailed にマップする (precheck.go)。
		repoRoot := t.TempDir()
		writeFixtureGoMod(t, repoRoot, foreignGoMod)

		inspector := NewGoModInspector()
		gotModulePath, gotModuleRoot, err := inspector.ModulePath(ctx, repoRoot)
		if err == nil {
			t.Fatalf("別 module path で error を期待したが nil が返った")
		}
		if gotModulePath != "" {
			t.Errorf("error 経路では modulePath は空文字列を期待: got %q", gotModulePath)
		}
		if gotModuleRoot != "" {
			t.Errorf("error 経路では moduleRoot は空文字列を期待: got %q", gotModuleRoot)
		}
	})

	t.Run("go.mod がツリー上に存在しないケース", func(t *testing.T) {
		// go.mod を一切置かない一時ディレクトリで呼び出す。
		// 期待: walk up が root まで到達しても見つからず error を返す。
		// `t.TempDir()` は OS 一時領域配下なので親に rilll の go.mod が存在しないことを前提にする。
		// (rilll リポジトリは `t.TempDir()` の親に存在しない設計の OS 一時領域配下に配置されるため、
		// 偶発的な親 go.mod の発見が起きない契約。)
		emptyDir := t.TempDir()

		inspector := NewGoModInspector()
		gotModulePath, gotModuleRoot, err := inspector.ModulePath(ctx, emptyDir)
		if err == nil {
			t.Fatalf("go.mod 不在で error を期待したが nil が返った")
		}
		if gotModulePath != "" {
			t.Errorf("error 経路では modulePath は空文字列を期待: got %q", gotModulePath)
		}
		if gotModuleRoot != "" {
			t.Errorf("error 経路では moduleRoot は空文字列を期待: got %q", gotModuleRoot)
		}
	})

	t.Run("go.mod パース不能 (module 宣言行欠落) のケース", func(t *testing.T) {
		// `module` 宣言行を含まない不正な go.mod を置いて呼び出す。
		// 期待: パース失敗で error が返り、modulePath / moduleRoot は空文字列。
		repoRoot := t.TempDir()
		writeFixtureGoMod(t, repoRoot, brokenGoMod)

		inspector := NewGoModInspector()
		gotModulePath, gotModuleRoot, err := inspector.ModulePath(ctx, repoRoot)
		if err == nil {
			t.Fatalf("module 宣言欠落で error を期待したが nil が返った")
		}
		if gotModulePath != "" {
			t.Errorf("error 経路では modulePath は空文字列を期待: got %q", gotModulePath)
		}
		if gotModuleRoot != "" {
			t.Errorf("error 経路では moduleRoot は空文字列を期待: got %q", gotModuleRoot)
		}
	})
}
