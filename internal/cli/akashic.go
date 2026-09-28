package cli

import (
	"context"
	"fmt"
	"io"
)

// runAkashic は `rilll akashic` 配下のサブコマンド (op) を振り分けるルータである
// (Req 6.1, 6.11, 6.12)。
//
// 引数 args は `os.Args[2:]` 相当 (`akashic` 以降)。先頭要素を op として解釈し、
// 以降の要素は op 別ハンドラに `args[1:]` として委譲する。
//
//   - "" (op なし)             → ヘルプを stderr に出して exit 2
//   - "--help" / "-h" / "help" → ヘルプを stdout に出して exit 0
//   - "export"                 → Reader nil チェック後 runAkashicExport に委譲
//   - 上記以外                  → 「未知の akashic サブコマンド」を stderr に出して exit 2
//
// Reader nil 検査は Reader を実際に必要とする op (現状 export のみ) に対して
// 行う。help 系では Reader を要求しない。これは NoopRecorder fallback 環境でも
// `rilll akashic --help` がヘルプを返せるようにするため (Req 1.6)。
func runAkashic(ctx context.Context, args []string, deps Deps) int {
	if len(args) == 0 {
		printAkashicHelp(deps.Stderr)
		return ExitUsageError
	}
	op := args[0]
	rest := args[1:]
	switch op {
	case "--help", "-h", "help":
		printAkashicHelp(deps.Stdout)
		return ExitOK
	case "export":
		if deps.Reader == nil {
			fmt.Fprintln(deps.Stderr, "Error: akashic データベースが利用できません")
			return ExitRunError
		}
		return runAkashicExport(ctx, rest, deps)
	default:
		fmt.Fprintf(deps.Stderr, "Error: 未知の akashic サブコマンド %q\n", op)
		printAkashicHelp(deps.Stderr)
		return ExitUsageError
	}
}

// printAkashicHelp は `rilll akashic` のサブコマンド一覧 / 出力規約を w に出力する。
// stdout / stderr は呼び出し側で選択する (help 経路 → stdout、エラー経路 → stderr)。
func printAkashicHelp(w io.Writer) {
	fmt.Fprintln(w, "rilll akashic - アカシックレコード (履歴 SQLite) を読み取るサブコマンド")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  rilll akashic <op> [--flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Subcommands:")
	fmt.Fprintln(w, "  export   workflow_runs を JSON で stdout に出力 (jq 等にパイプ可)")
	fmt.Fprintln(w, "  help     このヘルプを stdout に出力")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "出力:")
	fmt.Fprintln(w, "  進捗 → stderr / 結果 → stdout")
	fmt.Fprintln(w, "Exit code:")
	fmt.Fprintln(w, "  0 成功 / 1 実行時エラー / 2 CLI 誤用")
}

// runAkashicExport の本実装はタスク 3.1 で internal/cli/akashic_export.go に移動した。
// 本ファイルでは router (runAkashic) の責務だけを残す。
