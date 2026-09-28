// Package improvepatch の本ファイルは GoModInspector interface 定義と
// 既定実装 (cwd から親方向に walk up + `go.mod` の `module` 宣言行パース +
// `github.com/kazuki-oshino/rilll` 一致検証 + moduleRoot 解決) を提供する
// (tasks.md 2.2, 3.1 / design.md §5 Resolved Decisions / Req 1.4, 1.6)。
//
// 既定実装は副作用として `os.ReadFile` のみを使用する (CGO 非依存維持 / steering tech.md)。
// orchestration 側 (ImproveWorkflow.Fn) は `Definition.Run` が受領した任意ディレクトリの
// cwd を本実装に渡し、moduleRoot を以降の正規化済み cwd として TargetResolver / GitOps /
// GoTestRunner へ伝播させる契約 (design.md §5 Resolved Decisions)。
package patch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// expectedModulePath は本リポジトリ (rilll) の go.mod が宣言する Go module path。
// cwd identity precheck において walk up 先の go.mod が宣言する module path と
// 完全一致しなければ error を返す契約 (design.md §5 / Req 1.4, 1.6)。
//
// 本値は `go.mod` の `module ` 宣言行をリポジトリ移動 / fork 時に追従更新する責務を持つ。
// 値変更時は本ファイル + tasks.md / design.md の参照箇所を同時更新する (Resolved Decisions §5)。
const expectedModulePath = "github.com/kazuki-oshino/rilll"

// ErrGoModNotFound は cwd から親方向に walk up しても `go.mod` が一切見つからない場合に
// 既定実装が返す sentinel error。orchestration 側はこれを VerdictPreconditionFailed verdict
// にマップする (precheck.go / Req 1.4, 1.6)。
var ErrGoModNotFound = errors.New("improvepatch: go.mod not found in cwd or any parent directory (cwd identity precheck failed)")

// ErrGoModParse は `go.mod` を読み込めたものの `module ` 宣言行が抽出できない
// (構文不正 / 宣言欠落) 場合に既定実装が返す sentinel error。
var ErrGoModParse = errors.New("improvepatch: go.mod is missing a parseable `module` declaration")

// ErrModulePathMismatch は walk up で見つけた `go.mod` の module path が
// 期待値 (`github.com/kazuki-oshino/rilll`) と一致しない場合に既定実装が返す sentinel error。
// orchestration 側はこれを VerdictPreconditionFailed verdict にマップし、
// 「rilll リポジトリ以外の cwd で起動された」破壊的操作を hard fail させる (Resolved Decisions §5)。
var ErrModulePathMismatch = errors.New("improvepatch: go.mod module path does not match expected rilll module path")

// GoModInspector は cwd から `go.mod` を解決して module path と module root を返す契約を表す。
// improve-workflow の precheck フェーズで cwd identity を検証する用途で使われ、
// `Definition.Run` が受領する任意ディレクトリの cwd を rilll repo 内のいずれの階層から
// 起動されても module root に正規化する責務を担う (design.md §5 Resolved Decisions)。
//
// 既定実装 (task 3.1) の挙動契約:
//   - cwd から親方向に walk up し、最初に見つかった `go.mod` を `os.ReadFile` で読む。
//   - `module ` 宣言行を line-based parse し、`github.com/kazuki-oshino/rilll` と完全一致しなければ error。
//   - moduleRoot は `go.mod` が見つかったディレクトリの絶対 clean path。
//   - `go.mod` がツリー上に見つからない / `module` 宣言が無い (パース不能) 場合も error。
//
// orchestration 側 (ImproveWorkflow.Fn) は本 interface が error を返した場合、
// `VerdictPreconditionFailed` verdict で hard fail し、Snapshot / ApplyCheck / Apply / Revert に
// 進まない契約 (Req 1.6 / design.md §7 Resolved Decisions)。
type GoModInspector interface {
	// ModulePath は cwd から `go.mod` を解決し、(modulePath, moduleRoot, err) を返す。
	//
	// 戻り値:
	//   - modulePath: `go.mod` の `module` 宣言行が指す Go module path (例: "github.com/kazuki-oshino/rilll")。
	//   - moduleRoot: `go.mod` が見つかったディレクトリの絶対 clean path。
	//   - err: 解決失敗 (go.mod 不在 / パース不能 / module path 不一致など) を示す error。
	//
	// err != nil のとき modulePath / moduleRoot は空文字列 (呼び出し側は err を優先して扱う契約)。
	ModulePath(ctx context.Context, cwd string) (modulePath, moduleRoot string, err error)
}

// NewGoModInspector は os.ReadFile + walk up 経由で go.mod を解決する既定実装を返す。
// 本実装は CGO 非依存 (`os.ReadFile` / `filepath` のみ使用) で、`improvepatch` パッケージ内に閉じる
// (steering tech.md / design.md §5 Resolved Decisions)。
//
// ファクトリ命名は既存の `NewGitOps` (git.go) と同パターンに揃え、orchestration 側で
// `improvepatch.NewGoModInspector()` の 1 行で利用できるようにする。
func NewGoModInspector() GoModInspector {
	return &defaultGoModInspector{}
}

// defaultGoModInspector は GoModInspector interface の既定実装。
// cwd から親方向に walk up し、最初に見つかった `go.mod` を `os.ReadFile` で読み込み、
// `module` 宣言行をパースして expectedModulePath との一致を検証する。
type defaultGoModInspector struct{}

// ModulePath は cwd から walk up で `go.mod` を探索し、module path / moduleRoot を返す。
// 失敗経路 (go.mod 不在 / パース不能 / module path 不一致) ではいずれも (modulePath, moduleRoot)
// を空文字列で返し、err を非 nil にする (interface 契約 / gomod.go ヘッダコメント参照)。
func (i *defaultGoModInspector) ModulePath(_ context.Context, cwd string) (string, string, error) {
	// cwd を絶対 clean path に正規化してから walk up を始める。
	// 相対パス (`.` 等) で渡されても親階層解決が決定的に動作するようにする。
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return "", "", fmt.Errorf("improvepatch: cwd を絶対パスに正規化できなかった (cwd=%q): %w", cwd, err)
	}
	absCwd = filepath.Clean(absCwd)

	// walk up: 各階層で `go.mod` の存在を `os.Stat` で確認 (filesystem root に到達したら break)。
	moduleRoot, err := findGoModDir(absCwd)
	if err != nil {
		return "", "", err
	}

	// 見つかった `go.mod` を読み、`module` 宣言行をパースする。
	goModPath := filepath.Join(moduleRoot, "go.mod")
	content, err := os.ReadFile(goModPath)
	if err != nil {
		return "", "", fmt.Errorf("improvepatch: go.mod 読み込み失敗 (path=%q): %w", goModPath, err)
	}

	modulePath, ok := parseModulePath(content)
	if !ok {
		return "", "", fmt.Errorf("%w (path=%q)", ErrGoModParse, goModPath)
	}

	// expectedModulePath との完全一致を検証する。前後空白除去後の literal 一致のみを許容し、
	// 部分一致や case-insensitive 比較は採らない (Resolved Decisions §5)。
	if modulePath != expectedModulePath {
		return "", "", fmt.Errorf("%w (got=%q, want=%q, path=%q)", ErrModulePathMismatch, modulePath, expectedModulePath, goModPath)
	}

	return modulePath, moduleRoot, nil
}

// findGoModDir は startDir から filesystem root まで親方向に走査し、`go.mod` を含む
// 最初のディレクトリの絶対 clean path を返す。
// 一切見つからなかった場合は ErrGoModNotFound を返す。
//
// 走査打ち切り条件: 親ディレクトリが自身と一致する状態 (filepath.Dir(dir) == dir、
// すなわち filesystem root) に到達したら停止する。これにより無限ループを防ぐ。
func findGoModDir(startDir string) (string, error) {
	dir := startDir
	for {
		candidate := filepath.Join(dir, "go.mod")
		info, err := os.Stat(candidate)
		switch {
		case err == nil && !info.IsDir():
			// `go.mod` ファイル発見。
			return dir, nil
		case err != nil && !errors.Is(err, os.ErrNotExist):
			// `os.Stat` 自体の予期しない失敗 (権限不足等) は呼び出し側に伝える。
			return "", fmt.Errorf("improvepatch: go.mod 探索中の os.Stat 失敗 (path=%q): %w", candidate, err)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			// filesystem root に到達。これ以上 walk up しても結果は同じになるため打ち切る。
			return "", ErrGoModNotFound
		}
		dir = parent
	}
}

// parseModulePath は go.mod の内容から `module ` 宣言行を line-based に抽出する。
// 抽出可能なら (path, true)、`module` 宣言行が見つからない / 不正な場合は ("", false) を返す。
//
// 簡易パース契約 (本実装で取り扱う形態):
//   - 行頭の空白 (`	` / ` `) を除去後、行頭が `// ` で始まる行はコメントとしてスキップする。
//   - `module ` で始まる最初の有効行を採用する。
//   - module path は `module` キーワードの直後 (空白区切り) を採用し、行末 (改行 / コメント `//`) で切り詰める。
//     ダブルクォート (`"github.com/foo/bar"`) で囲まれている場合は外側のクォートを 1 組だけ剥がす。
//   - `module ( ... )` 形式 (block) は go.mod の文法上存在しないため対応しない。
//
// 本パースは `golang.org/x/mod/modfile` を導入せず標準ライブラリのみで完結させる
// (CGO / 外部依存追加禁止 / steering tech.md)。ケース 5 (パース不能) を機械検証可能にする最小実装。
func parseModulePath(content []byte) (string, bool) {
	// strings.SplitSeq は中間スライスを生成せず行ごとに iterate するため、
	// go.mod 全体を一度メモリに展開せずに済む (modernize linter 推奨形)。
	for raw := range strings.SplitSeq(string(content), "\n") {
		line := strings.TrimLeft(raw, " \t")
		if strings.HasPrefix(line, "//") {
			continue
		}
		if !strings.HasPrefix(line, "module") {
			continue
		}
		// `module` の直後がスペース or タブでない場合 (例: `modulefoo`) は宣言行ではない。
		rest := line[len("module"):]
		if len(rest) == 0 || (rest[0] != ' ' && rest[0] != '\t') {
			continue
		}
		// 行末コメント (`// ...`) は切り捨てる。
		if idx := strings.Index(rest, "//"); idx >= 0 {
			rest = rest[:idx]
		}
		path := strings.TrimSpace(rest)
		// 引用符付き (`"github.com/foo/bar"`) を許容: 両端を 1 組だけ剥がす。
		if len(path) >= 2 && path[0] == '"' && path[len(path)-1] == '"' {
			path = path[1 : len(path)-1]
		}
		if path == "" {
			// `module ` の後に空白だけが続くケースは宣言として不正。
			return "", false
		}
		return path, true
	}
	return "", false
}
