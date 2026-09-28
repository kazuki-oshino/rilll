// Package improvepatch の本ファイルは Precheck helper 関数 (純粋 orchestration) を
// 提供する (tasks.md 2.3 / design.md §System Flows / Req 1.4, 1.6)。
//
// 設計契約:
//   - 順序: GoModInspector.ModulePath → 一致しなければ VerdictPreconditionFailed →
//     GitOps.Status → clean=false なら VerdictPreconditionFailed → 通過時 verdict は
//     空文字列 sentinel (continue を表す) を返す。
//   - 副作用は GitOps.Status と GoModInspector.ModulePath の呼び出しに集約する。
//     Snapshot / ApplyCheck / Apply / Revert は precheck 関数内から呼ばない契約
//     (Req 1.6 hard-fail 経路で副作用が一切走らないことの構造的保証)。
//   - soft warning として「警告だけ流して続行」する経路は戻り値型に存在しない。
//     戻り値は (moduleRoot, verdict, err) の 3 つだけで warning ステータスを返す型を持たない
//     (design.md §10 Resolved Decisions / Req 1.6 hard-fail)。
package patch

import (
	"context"
	"errors"
	"fmt"
)

// ErrDirtyWorkingTree は git 作業ツリーに未コミット変更が存在することを示す sentinel error。
// orchestration 側は本 sentinel を観測したら VerdictPreconditionFailed verdict にマップし、
// ユーザにクリーンなツリーで再実行するよう案内する (Req 1.6)。
//
// errors.Is を介した検出を可能にするため、本パッケージの公開 sentinel として保持する。
var ErrDirtyWorkingTree = errors.New("improvepatch: git working tree has uncommitted changes (precondition: clean tree required)")

// Precheck は improve-workflow の起動プリコンディションを順次検証する純粋 orchestration helper。
//
// 検証順序 (design.md §System Flows / Req 1.4, 1.6):
//  1. GoModInspector.ModulePath で cwd identity を検証し、module root を解決する。
//     失敗時: ("", VerdictPreconditionFailed, gomod の error を wrap した error) を返す。
//  2. GitOps.Status で git 作業ツリーがクリーン (`git status --porcelain` 出力 0 行) であることを確認する。
//     Status 自体が error を返した場合: ("", VerdictPreconditionFailed, error を wrap) を返す。
//     clean == false の場合: ("", VerdictPreconditionFailed, ErrDirtyWorkingTree を返す)。
//
// 全段通過時の戻り値: (moduleRoot, Verdict(""), nil)
//   - 第 2 戻り値は continue を表す空文字列 sentinel。
//   - moduleRoot は GoModInspector が返した正規化済み (絶対 clean) パス。
//
// 副作用契約:
//   - 本関数は GitOps.Status と GoModInspector.ModulePath のみを呼ぶ。
//   - GitOps.Snapshot / ApplyCheck / Apply / Revert は呼ばない (apply フェーズに委ねる)。
//   - これにより Req 1.6 hard-fail 経路で baseline 保存 / patch 適用 / revert が一切走らないことを
//     構造的に保証する (task 2.4 で機械検証)。
//
// GitOps.Status の cwd 引数には GoModInspector が返した moduleRoot (正規化済み) を渡し、
// 呼び出し側が任意のサブディレクトリを cwd として渡してきても整合した検証になるようにする
// (design.md §5 Resolved Decisions「moduleRoot を以降の正規化済み cwd として伝播」)。
func Precheck(ctx context.Context, cwd string, gitOps GitOps, goMod GoModInspector) (moduleRoot string, verdict Verdict, err error) {
	// Step 1: cwd identity 検証 + module root 解決 (Req 1.4 / Resolved Decisions §5)
	_, root, gomodErr := goMod.ModulePath(ctx, cwd)
	if gomodErr != nil {
		return "", VerdictPreconditionFailed, fmt.Errorf("improvepatch: precheck failed (go.mod identity): %w", gomodErr)
	}

	// Step 2: git 作業ツリー clean 検証 (Req 1.6 / Resolved Decisions §7)
	clean, statusErr := gitOps.Status(ctx, root)
	if statusErr != nil {
		return "", VerdictPreconditionFailed, fmt.Errorf("improvepatch: precheck failed (git status): %w", statusErr)
	}
	if !clean {
		return "", VerdictPreconditionFailed, ErrDirtyWorkingTree
	}

	// 全段通過: verdict は空文字列 sentinel (continue を表す)
	return root, Verdict(""), nil
}
