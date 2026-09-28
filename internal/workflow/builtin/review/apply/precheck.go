// Package apply の本ファイルは task 2.2 / design.md §Precheck を実装する。
//
// 役割: review-apply-workflow の起動プリコンディションを副作用ゼロで検証する gate 層。
// review-workflow の per-rule 結果ファイル群を入力として受け取る前段として、
// 「reviewRunID が安全な相対 path であること」「`<cwd>/.rilll/review-workflow/<reviewRunID>/`
// が実在すること」「git working tree が clean であること」の 3 段を順次検証する
// (Req 1.3, 2.1, 2.2, 2.5)。
//
// design.md §Precheck Responsibilities:
//   - `improve/patch.GitOps.Status(ctx, cwd)` で clean tree を検証する。dirty なら
//     improvepatch.ErrDirtyWorkingTree を Cause に詰めて VerdictPreconditionFailed を返す。
//   - reviewRunID が空文字 / 絶対パス / `..` を経由する経路を `filepath.Clean` + prefix 検証で reject する
//     (design.md §10 Threats / 任意 path attack 防御)。
//   - `<cwd>/.rilll/review-workflow/<reviewRunID>/` が存在しなければ VerdictPreconditionFailed。
//   - per-rule JSON schema 互換は Loader (task 2.1) が `LoaderSchemaError` を返した時点で
//     Workflow 層が precheck 失敗扱いに合流させるため、本 Precheck では行わない。
//   - go.mod identity 検証は行わない (本 spec は任意 repo を対象とするため / design.md §Precheck)。
//
// 副作用契約 (Req 2.3):
//   - 本関数は GitOps.Status のみを呼ぶ。
//   - GitOps.Snapshot / ApplyCheck / Apply / Revert は呼ばない (apply フェーズに委ねる)。
//   - 順序契約により reviewRunID 検証 / dir 不在検証で fail する経路では Status すら呼ばない
//     (cheap な path 検証 → dir 検証 → status 検証 の順)。
//
// 戻り値契約 (design.md §Precheck Service Interface):
//   - 成功時: PrecheckResult{Verdict: VerdictUnset, Summary: "", Cause: nil}
//   - 失敗時: PrecheckResult{Verdict: VerdictPreconditionFailed, Summary: <利用者向け短文>, Cause: <内部 error>}
//   - Workflow 層 (task 5.1/5.2) は Verdict != VerdictUnset を観測した時点で副作用ゼロで終端し、
//     Summary を WorkflowText / ErrorPrefixForVerdict に流し込む責務を持つ。
package apply

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"context"

	improvepatch "github.com/kazuki-oshino/rilll/internal/workflow/builtin/improve/patch"
)

// ErrUnsafeReviewRunID は reviewRunID が絶対パス / `..` を含むなど、
// `<cwd>/.rilll/review-workflow/<reviewRunID>/` の prefix を escape する経路を持つときに
// Cause に詰める sentinel error。errors.Is での識別を可能にする
// (design.md §10 Threats / Req 1.3 友好的 verdict 経路)。
var ErrUnsafeReviewRunID = errors.New("apply.Precheck: reviewRunID is unsafe (absolute path or escapes review-workflow prefix)")

// ErrEmptyReviewRunIDPrecheck は Precheck の reviewRunID 引数が空文字のときに Cause に詰める sentinel。
// Loader 側の ErrEmptyReviewRunID と意味は同じだが、Precheck 経由の経路を errors.Is で
// 識別できるよう別変数として export する (Req 1.3 friendly verdict 経路)。
var ErrEmptyReviewRunIDPrecheck = errors.New("apply.Precheck: reviewRunID is empty")

// ErrReviewRunDirNotFoundPrecheck は `<cwd>/.rilll/review-workflow/<reviewRunID>` が存在しないときに
// Cause に詰める sentinel。Loader 側の ErrReviewRunDirNotFound と意味は同じだが、
// Precheck 経由の経路を errors.Is で識別できるよう別変数として export する (Req 1.3)。
var ErrReviewRunDirNotFoundPrecheck = errors.New("apply.Precheck: review run directory not found")

// PrecheckInput は Precheck の入力。design.md §Precheck Service Interface に対応する。
type PrecheckInput struct {
	// Cwd は workflow が実行された作業ディレクトリ。
	// 呼び出し側 (Workflow 層 task 5.1) は filepath.Clean 済みの絶対パスを渡す責務を持つ。
	Cwd string

	// ReviewRunID は前段 review-workflow の workflow run id。
	// `<Cwd>/.rilll/review-workflow/<ReviewRunID>/` を per-rule 結果ファイル群のロード起点とする。
	// 空文字 / 絶対パス / `..` 経路は Precheck で reject する。
	ReviewRunID string

	// GitOps は git working tree clean 検証用に既存 improve/patch.GitOps を流用する。
	// Precheck は GitOps.Status のみを呼び出し、Snapshot/ApplyCheck/Apply/Revert は呼ばない契約。
	GitOps improvepatch.GitOps
}

// PrecheckResult は Precheck の出力。Verdict が VerdictUnset 以外なら precheck 失敗を表す。
// Workflow 層 (task 5.1/5.2) は Verdict / Summary / Cause を `result.json` と
// WorkflowText / WorkflowError に流し込む責務を持つ。
type PrecheckResult struct {
	// Verdict は precheck 結果。VerdictUnset は成功 (continue を表す sentinel)、
	// それ以外 (現状は VerdictPreconditionFailed のみ) は失敗を表す。
	Verdict Verdict

	// Summary は利用者向け短文 (改行を含まない単一行)。
	// ErrorPrefixForVerdict と組み合わせて `[verdict:precondition_failed] <summary>` 形になる。
	Summary string

	// Cause は内部 error (akashic raw_event 用)。errors.Is / errors.As で sentinel 判定可能。
	// 成功時は nil。
	Cause error
}

// Precheck は副作用のない検証段。Verdict が VerdictUnset なら成功、
// それ以外なら precheck 失敗 (Workflow 層は副作用ゼロで終端する責務)。
//
// 検証順序 (cheap な path 検証 → dir 検証 → status 検証):
//  1. ReviewRunID 空文字 reject (ErrEmptyReviewRunIDPrecheck)。
//  2. ReviewRunID 安全性検証 (絶対パス / `..` 経路を `filepath.Clean` + prefix 検証で reject)
//     (ErrUnsafeReviewRunID)。
//  3. `<Cwd>/.rilll/review-workflow/<ReviewRunID>/` 存在検証 (ErrReviewRunDirNotFoundPrecheck)。
//  4. GitOps.Status 呼び出しで clean tree 検証 (improvepatch.ErrDirtyWorkingTree)。
//
// 全段通過時: PrecheckResult{Verdict: VerdictUnset, Summary: "", Cause: nil} を返す。
// 失敗時: PrecheckResult{Verdict: VerdictPreconditionFailed, Summary, Cause} を返す
// (副作用は Status 呼び出しまでに留め、Snapshot/ApplyCheck/Apply/Revert は一切呼ばない)。
func Precheck(ctx context.Context, in PrecheckInput) PrecheckResult {
	// (1) reviewRunID 空文字 reject。
	if in.ReviewRunID == "" {
		return failed(
			"review-run-id が指定されていません。--review-run-id <id> を指定して再実行してください。",
			ErrEmptyReviewRunIDPrecheck,
		)
	}

	// (2) reviewRunID 安全性検証。
	//
	// 攻撃面: reviewRunID は filesystem path に含まれるため、`..` や絶対パスで
	// `<cwd>/.rilll/review-workflow/` の外を指す可能性がある。
	// `filepath.Clean` で正規化した上で、絶対パス / `..` 経由 / prefix escape を一括 reject する。
	if isUnsafeReviewRunID(in.ReviewRunID) {
		return failed(
			fmt.Sprintf("review-run-id %q が不正です (絶対パス / `..` を含む経路は許可されていません)。", in.ReviewRunID),
			fmt.Errorf("%w: %q", ErrUnsafeReviewRunID, in.ReviewRunID),
		)
	}

	// (3) `<cwd>/.rilll/review-workflow/<reviewRunID>/` 存在検証。
	// Loader と同じ命名規約 (`reviewWorkflowDirName == "review-workflow"`) を流用する。
	runDir := filepath.Clean(filepath.Join(in.Cwd, ".rilll", reviewWorkflowDirName, in.ReviewRunID))
	info, err := os.Stat(runDir)
	if err != nil {
		if os.IsNotExist(err) {
			return failed(
				fmt.Sprintf("review-run-id %q に対応するディレクトリが見つかりません: %s", in.ReviewRunID, runDir),
				fmt.Errorf("%w: %s", ErrReviewRunDirNotFoundPrecheck, runDir),
			)
		}
		// I/O エラー (権限不足など) も precheck 失敗として友好的 verdict に集約する。
		return failed(
			fmt.Sprintf("review-run-id %q のディレクトリ確認に失敗しました: %v", in.ReviewRunID, err),
			fmt.Errorf("apply.Precheck: stat %s: %w", runDir, err),
		)
	}
	if !info.IsDir() {
		return failed(
			fmt.Sprintf("review-run-id %q のパスはディレクトリではありません: %s", in.ReviewRunID, runDir),
			fmt.Errorf("%w: %s is not a directory", ErrReviewRunDirNotFoundPrecheck, runDir),
		)
	}

	// (4) git working tree clean 検証。
	// 副作用契約: GitOps.Status のみを呼び、Snapshot/ApplyCheck/Apply/Revert は呼ばない。
	clean, statusErr := in.GitOps.Status(ctx, in.Cwd)
	if statusErr != nil {
		return failed(
			fmt.Sprintf("git status の確認に失敗しました: %v", statusErr),
			fmt.Errorf("apply.Precheck: git status: %w", statusErr),
		)
	}
	if !clean {
		return failed(
			"git の作業ツリーに未コミット変更があります。退避してから再実行してください (git stash / commit など)。",
			fmt.Errorf("apply.Precheck: %w", improvepatch.ErrDirtyWorkingTree),
		)
	}

	// 全段通過: VerdictUnset (continue を表す sentinel) を返す。
	return PrecheckResult{Verdict: VerdictUnset}
}

// failed は VerdictPreconditionFailed の PrecheckResult を組み立てる internal helper。
// summary / cause の null チェックと verdict のセットを 1 箇所に集約する。
func failed(summary string, cause error) PrecheckResult {
	return PrecheckResult{
		Verdict: VerdictPreconditionFailed,
		Summary: summary,
		Cause:   cause,
	}
}

// isUnsafeReviewRunID は reviewRunID が「絶対パス」または「`..` を経由して
// `<cwd>/.rilll/review-workflow/` prefix を escape する」経路を含むかを判定する。
//
// 検査手順:
//  1. `filepath.IsAbs` で絶対パスを reject。
//  2. `filepath.Clean` で正規化した上で、結果が `..` 単独 / `..` で始まる相対パスとなるものを reject。
//     (例: `..`, `../escape`, `foo/../../bar` の Clean 結果はいずれも `..` で始まる)
//
// design.md §10 Threats: review run id は filesystem path に含まれるため、
// `..` / 絶対パス を含む値は precheck 段で reject (`filepath.Clean` + prefix 検証)。
func isUnsafeReviewRunID(id string) bool {
	if filepath.IsAbs(id) {
		return true
	}
	cleaned := filepath.Clean(id)
	if cleaned == ".." {
		return true
	}
	// 相対パス先頭が `..<sep>` で始まる経路は prefix escape を意味する。
	// filepath.Separator はプラットフォーム依存だが、Clean 後の結果に対して
	// `..` + sep の prefix 一致を検証すれば十分。
	if strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return true
	}
	return false
}
