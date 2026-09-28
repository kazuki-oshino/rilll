// Package cli は `rilll` の非対話サブコマンド (`list` / `run` / `help`) を
// 実装する。引数なしまたは `--version` は cmd/rilll 側で従来どおり処理し、
// それ以外のサブコマンド指定時に Dispatch が exit code を返す。
//
// 設計方針:
//   - 進捗イベント (WorkflowText / WorkflowToolUse / WorkflowStep) は stderr。
//   - 最終結果 (WorkflowDone.Result) は stdout（スクリプトから捕捉できる）。
//   - WorkflowError は stderr へ出して exit 1。
//   - CLI 誤用 (未知サブコマンド / 未知 ID / 引数不足) は exit 2。
//   - サードパーティ依存は導入せず、標準 flag パッケージのみで実装する。
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/notify"
	"github.com/kazuki-oshino/rilll/internal/workflow"
)

const (
	ExitOK         = 0
	ExitRunError   = 1
	ExitUsageError = 2
)

// Deps は Dispatch に注入する外部依存を表す。
// テストは stdout/stderr をバッファに差し替え、Workflows をモックして検証する。
type Deps struct {
	Stdout    io.Writer
	Stderr    io.Writer
	Workflows []workflow.Definition
	// CWD は --cwd 未指定時のデフォルト作業ディレクトリ。
	// 通常は os.Getwd() の結果を渡す。
	CWD string
	// Reader は `rilll akashic` 配下の read-only クエリで利用するアカシックレコード
	// 読取契約。NoopRecorder fallback 経路では nil を渡し、`runAkashic` 側で
	// 「akashic データベースが利用できません」と返す (Req 1.6)。
	Reader akashic.Reader
}

// IsSubcommand は args[1] がサブコマンド経路かを判定する。
// `--version` や引数なしは TUI 経路へ任せるため false を返す。
func IsSubcommand(args []string) bool {
	if len(args) < 2 {
		return false
	}
	switch args[1] {
	case "list", "run", "help", "--help", "-h", "akashic":
		return true
	}
	return false
}

// Dispatch はサブコマンドを解釈し exit code を返す。
// Dispatch を呼ぶ前に IsSubcommand で TUI 経路と分岐させる想定。
func Dispatch(ctx context.Context, args []string, deps Deps) int {
	if deps.Stdout == nil {
		deps.Stdout = os.Stdout
	}
	if deps.Stderr == nil {
		deps.Stderr = os.Stderr
	}
	if len(args) < 2 {
		printTopLevelHelp(deps.Stderr, deps.Workflows)
		return ExitUsageError
	}

	switch args[1] {
	case "--help", "-h", "help":
		return runHelp(args[2:], deps)
	case "list":
		return runList(deps)
	case "run":
		return runRun(ctx, args[2:], deps)
	case "akashic":
		return runAkashic(ctx, args[2:], deps)
	default:
		fmt.Fprintf(deps.Stderr, "Error: 未知のサブコマンド %q\n", args[1])
		printTopLevelHelp(deps.Stderr, deps.Workflows)
		return ExitUsageError
	}
}

func runList(deps Deps) int {
	w := tabwriter.NewWriter(deps.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tLABEL\tARGS")
	defs := visibleDefinitions(deps.Workflows)
	sort.Slice(defs, func(i, j int) bool { return defs[i].ID < defs[j].ID })
	for _, def := range defs {
		fmt.Fprintf(w, "%s\t%s\t%s\n", def.ID, def.Label, formatArgsColumn(def.Args))
	}
	_ = w.Flush()
	return ExitOK
}

// visibleDefinitions は Hidden=true の Definition を除外した一覧を返す。
// `rilll list` / `printTopLevelHelp` / `printRunUsage` の表示用。
// findDefinition (`rilll run <id>` の名指し起動) は意図的に Hidden を弾かないので、
// hand-off 専用ワークフローでも CLI から直接起動はできる (precondition_failed で fail する)。
func visibleDefinitions(defs []workflow.Definition) []workflow.Definition {
	visible := make([]workflow.Definition, 0, len(defs))
	for _, def := range defs {
		if def.Hidden {
			continue
		}
		visible = append(visible, def)
	}
	return visible
}

func formatArgsColumn(args []workflow.ArgSpec) string {
	if len(args) == 0 {
		return "(なし)"
	}
	parts := make([]string, 0, len(args))
	for _, a := range args {
		s := "--" + a.Key
		if a.Required {
			s += " (必須)"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

func runRun(ctx context.Context, args []string, deps Deps) int {
	if len(args) == 0 {
		fmt.Fprintln(deps.Stderr, "Error: 'rilll run' にはワークフロー ID が必要です")
		printRunUsage(deps.Stderr, deps.Workflows)
		return ExitUsageError
	}
	id := args[0]
	rest := args[1:]
	def, ok := findDefinition(deps.Workflows, id)
	if !ok {
		fmt.Fprintf(deps.Stderr, "Error: 未知のワークフロー ID %q\n", id)
		printRunUsage(deps.Stderr, deps.Workflows)
		return ExitUsageError
	}

	fs := flag.NewFlagSet("rilll run "+id, flag.ContinueOnError)
	fs.SetOutput(deps.Stderr)
	cwdFlag := fs.String("cwd", "", "ワークフロー実行時の作業ディレクトリ（未指定時はプロセス CWD）")
	quiet := fs.Bool("quiet", false, "進捗イベントを stderr に出さない")
	q := fs.Bool("q", false, "--quiet の別名")
	fs.Usage = func() {
		printWorkflowHelp(deps.Stderr, def)
	}

	values := map[string]*cliArgValue{}
	for _, arg := range def.Args {
		desc := arg.Description
		if arg.Required {
			desc = "[必須] " + desc
		}
		value := &cliArgValue{}
		values[arg.Key] = value
		fs.Var(value, arg.Key, desc)
	}

	if err := fs.Parse(rest); err != nil {
		// flag が ErrHelp を返した場合は exit 2 ではなく 0 で抜ける。
		if err == flag.ErrHelp {
			return ExitOK
		}
		return ExitUsageError
	}

	cwd := *cwdFlag
	if cwd == "" {
		cwd = deps.CWD
	}
	if cwd == "" {
		got, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(deps.Stderr, "Error: cwd の取得に失敗しました: %v\n", err)
			return ExitRunError
		}
		cwd = got
	}

	askValues := map[string]string{}
	askCounts := map[string]int{}
	for key, value := range values {
		askValues[key] = value.value
		askCounts[key] = value.count
	}
	ask := workflow.CLIAsk{Values: askValues, Counts: askCounts}

	suppressEvents := *quiet || *q
	finalResult := ""
	hadWorkflowError := false
	onEvent := func(event workflow.WorkflowEvent) {
		switch event.Type {
		case workflow.WorkflowDone:
			finalResult = event.Result
		case workflow.WorkflowError:
			hadWorkflowError = true
			fmt.Fprintf(deps.Stderr, "Error: %s\n", event.Message)
		default:
			if suppressEvents {
				return
			}
			writeProgressEvent(deps.Stderr, event)
		}
	}

	// workflow 実行に入った時点で終了通知音再生を予約する。
	// 成功 / WorkflowError / def.Run の error / CLIAskError いずれの経路でも鳴らす。
	defer notify.PlayCompletionSound()

	if err := def.Run(ctx, cwd, onEvent, ask); err != nil {
		var askErr *workflow.CLIAskError
		if errors.As(err, &askErr) {
			fmt.Fprintf(deps.Stderr, "Error: %s\n", askErr.Message)
			fmt.Fprintf(deps.Stderr, "Hint: rilll run %s --help を参照してください\n", def.ID)
			return ExitUsageError
		}
		fmt.Fprintf(deps.Stderr, "Error: %v\n", err)
		return ExitRunError
	}
	if hadWorkflowError {
		return ExitRunError
	}
	if finalResult != "" {
		fmt.Fprintln(deps.Stdout, finalResult)
	}
	return ExitOK
}

type cliArgValue struct {
	value string
	count int
}

func (v *cliArgValue) Set(value string) error {
	v.value = value
	v.count++
	return nil
}

func (v *cliArgValue) String() string {
	return v.value
}

func runHelp(args []string, deps Deps) int {
	if len(args) == 0 {
		printTopLevelHelp(deps.Stdout, deps.Workflows)
		return ExitOK
	}
	switch args[0] {
	case "list":
		fmt.Fprintln(deps.Stdout, "rilll list")
		fmt.Fprintln(deps.Stdout, "  登録ワークフロー一覧 (ID / Label / 必要引数) を stdout に出力します。")
		return ExitOK
	case "run":
		if len(args) >= 2 {
			def, ok := findDefinition(deps.Workflows, args[1])
			if !ok {
				fmt.Fprintf(deps.Stderr, "Error: 未知のワークフロー ID %q\n", args[1])
				return ExitUsageError
			}
			printWorkflowHelp(deps.Stdout, def)
			return ExitOK
		}
		printRunUsage(deps.Stdout, deps.Workflows)
		return ExitOK
	case "help":
		printTopLevelHelp(deps.Stdout, deps.Workflows)
		return ExitOK
	default:
		// `rilll help <workflow-id>` の短縮形にも対応。
		def, ok := findDefinition(deps.Workflows, args[0])
		if !ok {
			fmt.Fprintf(deps.Stderr, "Error: 未知のサブコマンド/ワークフロー %q\n", args[0])
			return ExitUsageError
		}
		printWorkflowHelp(deps.Stdout, def)
		return ExitOK
	}
}

func writeProgressEvent(w io.Writer, event workflow.WorkflowEvent) {
	switch event.Type {
	case workflow.WorkflowText:
		// Text イベントは改行を保持して逐次表示。
		fmt.Fprint(w, event.Text)
	case workflow.WorkflowToolUse:
		fmt.Fprintf(w, "[tool] %s\n", event.Tool)
	case workflow.WorkflowStep:
		if event.TotalSteps > 0 {
			fmt.Fprintf(w, "[step %d/%d] %s\n", event.Step, event.TotalSteps, event.Label)
		} else {
			fmt.Fprintf(w, "[step] %s\n", event.Label)
		}
	case workflow.WorkflowActive:
		fmt.Fprintf(w, "[active] %s\n", event.Label)
	}
}

func findDefinition(defs []workflow.Definition, id string) (workflow.Definition, bool) {
	for _, def := range defs {
		if def.ID == id {
			return def, true
		}
	}
	return workflow.Definition{}, false
}

func printTopLevelHelp(w io.Writer, defs []workflow.Definition) {
	fmt.Fprintln(w, "rilll - ワークフローランナー")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  rilll                          TUI を起動 (既存挙動)")
	fmt.Fprintln(w, "  rilll list                     ワークフロー一覧を表示")
	fmt.Fprintln(w, "  rilll run <id> [--flags]       ワークフローを非対話で実行")
	fmt.Fprintln(w, "  rilll help [command|id]        ヘルプを表示")
	fmt.Fprintln(w, "  rilll --version                バージョンを表示")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "出力:")
	fmt.Fprintln(w, "  進捗 → stderr / 最終結果 → stdout")
	fmt.Fprintln(w, "Exit code:")
	fmt.Fprintln(w, "  0 成功 / 1 実行時エラー / 2 CLI 誤用")
	visible := visibleDefinitions(defs)
	if len(visible) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "登録ワークフロー:")
		ids := make([]string, 0, len(visible))
		for _, d := range visible {
			ids = append(ids, d.ID)
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Fprintf(w, "  %s\n", id)
		}
	}
}

func printRunUsage(w io.Writer, defs []workflow.Definition) {
	fmt.Fprintln(w, "Usage: rilll run <workflow-id> [--flags]")
	fmt.Fprintln(w, "  利用可能なワークフロー一覧は `rilll list` を参照してください。")
	visible := visibleDefinitions(defs)
	if len(visible) > 0 {
		ids := make([]string, 0, len(visible))
		for _, d := range visible {
			ids = append(ids, d.ID)
		}
		sort.Strings(ids)
		fmt.Fprintf(w, "  ID: %s\n", strings.Join(ids, ", "))
	}
}

func printWorkflowHelp(w io.Writer, def workflow.Definition) {
	fmt.Fprintf(w, "rilll run %s\n", def.ID)
	fmt.Fprintf(w, "  %s\n", def.Label)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  --cwd <dir>   作業ディレクトリ (default: プロセス CWD)")
	fmt.Fprintln(w, "  --quiet, -q   進捗イベントを抑制")
	if len(def.Args) == 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "このワークフローは追加引数を必要としません。")
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Workflow args:")
	for _, a := range def.Args {
		req := ""
		if a.Required {
			req = " [必須]"
		}
		fmt.Fprintf(w, "  --%s%s   %s\n", a.Key, req, a.Description)
	}
}
