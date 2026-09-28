// Package apply の本ファイルは task 5.5 / design.md §Workflow §中断 §revert 失敗 /
// Req 5.6, 6.1, 6.2, 6.3, 6.4 の cancel-aware revert + RecoveryGuidance helper を提供する。
//
// 役割:
//   - `runOrCancel(ctx, fn)`: 副作用呼び出し直前の `select { case <-ctx.Done(): ...; default: fn() }`
//     を集約する helper。`gitOps.Apply` / `Add` / `Commit` / `Revert` の各呼び出しを本 helper で
//     包むことで、5.6 で実装される AST invariant test (helper 経由を機械的に強制) の前提を満たす。
//   - `buildRevertRecoveryGuidance(baseline, files, note)`: Revert 失敗時に利用者へ提示する
//     `git checkout <baseline> -- <file>` 形の手動復旧手順を構築する。Files / Commands は 1 対 1 で
//     生成し、Note には日本語の補足説明を入れる。
//
// 設計契約:
//   - `runOrCancel` の `fn` 引数は `error` を返すが、本 helper 内では `ctx.Err()` 経路と
//     `fn()` の正常 / 失敗経路を識別しない (caller が ctx.Err() を別途観測して判定する)。
//   - cancel 経路で発火する `Revert` 自身は **非 cancel 済 ctx (context.Background())** で
//     呼び出さなければならない (Req 6.4)。さもなくば runOrCancel の select が即時 ctx.Err() を
//     返し Revert が走らずに RecoveryGuidance も生成されない循環不変式違反となる。
//     本ファイルの helper は ctx 引数を caller に委ね、apply_phase.go 側で `context.Background()`
//     を渡す責務を持たせる (関心の分離)。
//   - RecoveryGuidance.Commands の formatting は test (cancel_guard_test.go) の文字列リテラルと
//     完全一致させる契約: `"git checkout " + baseline.HeadCommit + " -- " + file`。スペース / 順序を
//     変更する場合はテストも同時改訂する (両端契約)。
package apply

import (
	"context"
	"fmt"

	improvepatch "github.com/kazuki-oshino/rilll/internal/workflow/builtin/improve/patch"
)

// runOrCancel は副作用呼び出し `fn` の直前に `ctx.Done()` を観測する集約 helper。
//
// 振る舞い:
//   - ctx が既に cancel 済なら `fn` を呼ばずに `ctx.Err()` を返す (副作用ゼロ契約 Req 6.4)。
//   - そうでなければ `fn()` を呼んでその戻り値を返す (通常経路)。
//
// 呼び出し側の使い方 (apply_phase.go):
//
//	if err := runOrCancel(ctx, func() error { return gitOps.Apply(ctx, cwd, diff) }); err != nil {
//	    if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
//	        // ... cancel 検出経路 (5.5 で Revert を background ctx で発火)
//	    }
//	    // ... 副作用失敗経路 (5.4 既存の verdict マップ)
//	}
//
// 5.6 で実装される AST invariant test は本 helper の使用 (`runOrCancel(ctx, ...)`) を
// `gitOps.Apply` / `Add` / `Commit` / `Revert` の各呼び出し直前に検出する契約。
// helper 名・引数順は 5.6 invariant test と同期する (改名時は invariant test も同時改訂)。
func runOrCancel(ctx context.Context, fn func() error) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return fn()
	}
}

// buildRevertRecoveryGuidance は Revert 失敗時の `RecoveryGuidance` を構築する
// (design.md §Result / Req 5.6, 6.2)。
//
// 出力契約:
//   - `Files`: targetFiles をそのままコピー (順序保持、空 slice なら空 slice)。
//   - `Commands`: 各 file ごとに `git checkout <baseline.HeadCommit> -- <file>` を生成し
//     1 対 1 で並べる (利用者が `Files[i]` と `Commands[i]` を対応付けて参照可能)。
//   - `Note`: 日本語の補足説明 (`note` 引数; 空文字なら `omitempty` で JSON 省略)。
//
// `baseline.HeadCommit` が空文字 (Snapshot 失敗経路で baseline 未取得) の場合は
// Commands も `git checkout  -- <file>` 形となるが、本 helper は防御的に空文字を埋め込まず
// commit SHA placeholder `(baseline 未取得)` を入れる: 本来 baseline 不在経路では
// Apply / Add / Commit のいずれも未起動のため revert 自体不要であり、このフォールバックは
// 実装ミス検出のための backstop である (production では到達しない)。
func buildRevertRecoveryGuidance(
	baseline improvepatch.BaselineRef,
	targetFiles []string,
	note string,
) *RecoveryGuidance {
	files := make([]string, len(targetFiles))
	copy(files, targetFiles)

	baselineSHA := baseline.HeadCommit
	if baselineSHA == "" {
		baselineSHA = "(baseline 未取得)"
	}

	commands := make([]string, 0, len(targetFiles))
	for _, f := range targetFiles {
		commands = append(commands, fmt.Sprintf("git checkout %s -- %s", baselineSHA, f))
	}

	return &RecoveryGuidance{
		Files:    files,
		Commands: commands,
		Note:     note,
	}
}
