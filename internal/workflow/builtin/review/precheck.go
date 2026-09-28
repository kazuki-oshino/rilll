// Package review の本ファイルは task 2.3 / design.md §review.precheck を実装する。
//
// 役割: workflow.Definition.Fn 副作用 (per-rule executor 起動 / per-rule artifact 書き出し) の
// 手前で gh / rules / PR 引数 / PR 存在の前提条件を一括検証する (Req 1.4, 2.1, 2.2, 2.3, 2.4,
// 3.1, 3.2, 3.3)。
//
// 実行順序 (Flow 2 の決定木):
//
//  1. gh CLI 存在 + auth 検証 (gh.CheckCLI 一括)
//  2. rules dir + 件数 + 重複 name 検証 (rules.LoadRules)
//  3. `--pr` 引数解決 (CLI 単独経路: Values["pr"] 直読み / TUI 経路: gh pr list で Items 構築)
//  4. gh pr view + gh pr diff で PR 存在検証 + コンテキスト取得
//
// 失敗時は precheckResult.Verdict = VerdictPrecheckFailed + 人間可読 summary を確定させ、
// 副作用 (executor 起動 / per-rule artifact 書き出し) を一切開始しない契約を厳守する (Req 3.3)。
//
// 経路分岐 (design.md §review.precheck Implementation Notes):
//
//   - CLI 単独経路 (`ask` が `*workflow.CLIAsk`): `gh pr list` を呼ばず Values["pr"] を
//     `strconv.Atoi` → そのまま `gh pr view <num>` で存在検証する。Items 構築コスト
//     (~1-2 秒) と 200 件上限を回避する。
//   - TUI 経路 (それ以外): `gh pr list --state all --limit 200` で Items を構築 →
//     `ask.Select(SelectParams{Key:"pr", Items: ...})` で番号を取得する (Approach D)。
//
// 両経路とも最終的に `gh pr view <num>` の成功で PR の存在を確認する (検証強度等価)。
package review

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/kazuki-oshino/rilll/internal/gh"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/result"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/rules"
)

// askPRKey は CLIAsk.Values の semantic key + WorkflowAsk.SelectParams.Key 共通値。
// CLI フラグ `--pr` と対応し、両経路で同一の key を使うことで意味の一貫性を担保する。
const askPRKey = "pr"

// precheckGhClient は precheck.go が `internal/gh` パッケージ関数群を呼び出すための seam。
// 本番経路では `defaultGhClient` が gh.* を素直に委譲し、テスト経路では fake 実装で
// gh CLI を完全にモックする (PATH に偽 gh を差し込まずに in-process テスト可能)。
type precheckGhClient interface {
	CheckCLI(ctx context.Context) error
	ListPRs(ctx context.Context, cwd, state string, limit int) ([]gh.PRListItem, error)
	ViewPR(ctx context.Context, cwd string, num int) (gh.PRMeta, error)
	DiffPR(ctx context.Context, cwd string, num int) (string, error)
}

// defaultGhClient は precheckGhClient の本番経路実装。`internal/gh` パッケージ関数を素通しする。
type defaultGhClient struct{}

func (defaultGhClient) CheckCLI(ctx context.Context) error { return gh.CheckCLI(ctx) }
func (defaultGhClient) ListPRs(ctx context.Context, cwd, state string, limit int) ([]gh.PRListItem, error) {
	return gh.ListPRs(ctx, cwd, state, limit)
}
func (defaultGhClient) ViewPR(ctx context.Context, cwd string, num int) (gh.PRMeta, error) {
	return gh.ViewPR(ctx, cwd, num)
}
func (defaultGhClient) DiffPR(ctx context.Context, cwd string, num int) (string, error) {
	return gh.DiffPR(ctx, cwd, num)
}

// precheckRulesLoader は `rules.LoadRules` の seam (テスト容易性のため func 値で差し替え可能)。
// 本番経路では `rules.LoadRules` を直接渡す。
type precheckRulesLoader func(cwd string) ([]rules.Rule, []rules.Warning, error)

// precheckInput は runPrecheck への入力を集約する構造体。
//
// `gh` / `loadRules` が nil のときは本番経路 (`defaultGhClient` / `rules.LoadRules`) が使われる。
// テストでは fake 実装を直接渡せる seam として保持する。
type precheckInput struct {
	cwd       string
	ask       workflow.WorkflowAsk
	gh        precheckGhClient
	loadRules precheckRulesLoader
}

// precheckResult は runPrecheck の出力。
//
// Verdict==VerdictUnset のときは precheck 通過: Rules (≥1 件、ファイル名昇順) /
// PRMeta / PRDiff が埋まる。Verdict==VerdictPrecheckFailed のときは Summary に人間可読
// メッセージが入り、Rules/PRMeta/PRDiff は zero-value のまま (副作用は開始しない契約、Req 3.3)。
type precheckResult struct {
	Verdict  result.Verdict
	Summary  string
	PRNumber int
	PRMeta   gh.PRMeta
	PRDiff   string
	Rules    []rules.Rule
	Warnings []rules.Warning
}

// runPrecheck は Flow 2 の決定木を順守して前提条件を一括検証する
// (design.md §review.precheck / Req 1.4, 2.1, 2.2, 2.3, 2.4, 3.1, 3.2, 3.3)。
//
// 戻り値 error は本実装では常に nil。precheck の結果は precheckResult.Verdict に集約する
// (`VerdictPrecheckFailed` または `VerdictUnset`)。Service Interface の error 型は
// 将来 ctx 起因のキャンセルなど純粋な infra error を伝える余地のために保持する。
func runPrecheck(ctx context.Context, in precheckInput) (precheckResult, error) {
	client := in.gh
	if client == nil {
		client = defaultGhClient{}
	}
	load := in.loadRules
	if load == nil {
		load = rules.LoadRules
	}

	// Step 1: gh CLI 存在 + auth 検証 (Req 3.1)。
	// `gh.CheckCLI` は binary 存在 + auth status を 1 つにまとめている。
	// sentinel error (`ErrCLIMissing` / `ErrUnauthorized`) を区別して friendly summary に整形する。
	if err := client.CheckCLI(ctx); err != nil {
		return precheckFailed(formatCheckCLIError(err)), nil
	}

	// Step 2: rules dir + 件数 + 重複 name 検証 (Req 1.4 / 3.2)。
	// LoadRules 内部で kebab-case / 予約名 / 必須欠落 / unknown フィールドの skip + Warning 集約と
	// `ErrDuplicateRuleName` / `ErrRulesDirMissing` / `ErrNoRulesFound` の sentinel 分類を行う。
	loadedRules, warnings, lerr := load(in.cwd)
	if lerr != nil {
		return precheckFailedWithWarnings(formatRulesLoadError(lerr), warnings), nil
	}

	// Step 3: --pr 引数解決 (Req 2.4) — CLI 経路 / TUI 経路の type-switch seam。
	// CLI 経路は `gh pr list` を呼ばずに Values["pr"] を直接 `strconv.Atoi` する。
	// TUI 経路は `gh pr list --state all --limit 200` で Items を構築 → ask.Select で取得する。
	prNum, askSummary, askFailed := resolvePRNumber(ctx, in.ask, client, in.cwd)
	if askFailed {
		return precheckFailedWithWarnings(askSummary, warnings), nil
	}

	// Step 4: gh pr view + diff (Req 2.1, 2.2, 2.3)。
	// view 失敗時の sentinel `ErrPRNotFound` は Req 2.3 の friendly error に直結する。
	meta, viewErr := client.ViewPR(ctx, in.cwd, prNum)
	if viewErr != nil {
		return precheckFailedWithWarnings(formatViewPRError(viewErr, prNum), warnings), nil
	}
	diff, diffErr := client.DiffPR(ctx, in.cwd, prNum)
	if diffErr != nil {
		return precheckFailedWithWarnings(formatDiffPRError(diffErr, prNum), warnings), nil
	}

	return precheckResult{
		Verdict:  result.VerdictUnset,
		PRNumber: prNum,
		PRMeta:   meta,
		PRDiff:   diff,
		Rules:    loadedRules,
		Warnings: warnings,
	}, nil
}

// precheckFailed は warning 無しの precheck 失敗結果を組み立てる。
func precheckFailed(summary string) precheckResult {
	return precheckResult{
		Verdict: result.VerdictPrecheckFailed,
		Summary: summary,
	}
}

// precheckFailedWithWarnings は rules.LoadRules 経由で集まった Warnings を保持しつつ
// precheck 失敗結果を組み立てる (result.json の precheck_warnings に転記される想定)。
func precheckFailedWithWarnings(summary string, warnings []rules.Warning) precheckResult {
	return precheckResult{
		Verdict:  result.VerdictPrecheckFailed,
		Summary:  summary,
		Warnings: warnings,
	}
}

// resolvePRNumber は `--pr` の解決を CLI 経路と TUI 経路で分岐する type-switch seam。
//
// 戻り値:
//   - num: 解決された PR 番号 (失敗時は 0)
//   - summary: 失敗時の人間可読メッセージ (成功時は空)
//   - failed: 失敗フラグ (true なら summary を採用、precheck 失敗として確定)
func resolvePRNumber(ctx context.Context, ask workflow.WorkflowAsk, client precheckGhClient, cwd string) (int, string, bool) {
	if cli, ok := ask.(*workflow.CLIAsk); ok {
		return resolvePRNumberCLI(cli)
	}
	return resolvePRNumberTUI(ctx, ask, client, cwd)
}

// resolvePRNumberCLI は CLIAsk 経由の Values["pr"] を直読みする CLI 単独経路。
//
// `gh pr list` を呼ばずに 200 件上限と latency regression を回避する
// (design.md §review.precheck Risks / Req 2.4 friendly error)。
func resolvePRNumberCLI(cli *workflow.CLIAsk) (int, string, bool) {
	raw, present := cli.Values[askPRKey]
	if !present || raw == "" {
		return 0, "--pr 引数が必要です。`rilll run review-workflow --pr <num>` の形で対象 PR 番号を指定してください。", true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Sprintf("--pr=%q は正の整数として解釈できません。`--pr <正の整数>` を指定してください。", raw), true
	}
	return n, "", false
}

// resolvePRNumberTUI は TUI 経路 (CLIAsk 以外の WorkflowAsk 実装) の番号取得経路。
//
// `gh pr list --state all --limit 200` で Items を構築 → ask.Select で番号を取得する
// (design.md §review.precheck Approach D)。`--state all` で close 済 PR の review も可能にする。
func resolvePRNumberTUI(ctx context.Context, ask workflow.WorkflowAsk, client precheckGhClient, cwd string) (int, string, bool) {
	items, err := client.ListPRs(ctx, cwd, "all", 200)
	if err != nil {
		return 0, formatListPRsError(err), true
	}
	if len(items) == 0 {
		return 0, "対象 PR が 1 件も見つかりません (`gh pr list --state all` が空)。レビュー対象 PR を作成してから再実行してください。", true
	}
	selectItems := make([]workflow.SelectItem, 0, len(items))
	for _, it := range items {
		selectItems = append(selectItems, workflow.SelectItem{
			ID:    strconv.Itoa(it.Number),
			Label: fmt.Sprintf("#%d %s", it.Number, it.Title),
		})
	}
	id, ok, selErr := ask.Select(ctx, workflow.SelectParams{
		Key:    askPRKey,
		Prompt: "レビュー対象 PR を選択してください",
		Items:  selectItems,
	})
	if selErr != nil {
		return 0, fmt.Sprintf("PR 選択に失敗しました: %v", selErr), true
	}
	if !ok {
		return 0, "PR 選択がキャンセルされました。レビュー対象 PR を選択してから再実行してください。", true
	}
	n, convErr := strconv.Atoi(id)
	if convErr != nil || n <= 0 {
		// ask.Select は Items 内 ID のみ返す契約のため、ここに到達するのは Items 構築側のバグ。
		// defensive に precheck_failed に丸める。
		return 0, fmt.Sprintf("選択された PR ID %q を正の整数に変換できません: %v", id, convErr), true
	}
	return n, "", false
}

// formatCheckCLIError は gh.CheckCLI の sentinel error を人間可読 summary に整形する。
func formatCheckCLIError(err error) string {
	switch {
	case errors.Is(err, gh.ErrCLIMissing):
		return "gh CLI が見つかりません。https://cli.github.com/ からインストールし、`gh auth login` で認証してください。"
	case errors.Is(err, gh.ErrUnauthorized):
		return "gh CLI が未認証です。`gh auth login` で GitHub アカウントの認証を完了してください。"
	default:
		return fmt.Sprintf("gh CLI の検証に失敗しました: %v", err)
	}
}

// formatRulesLoadError は rules.LoadRules の sentinel error を summary に整形する。
//
// `ErrDuplicateRuleName` のときは `*DuplicateRuleNameError` から重複 name と該当ファイル一覧を
// 取り出して提示する (Req 1.4)。
func formatRulesLoadError(err error) string {
	switch {
	case errors.Is(err, rules.ErrRulesDirMissing):
		return "review rule ディレクトリ `.rilll-config/review-rules/` が存在しません。最低 1 件の rule YAML を配置してから再実行してください。"
	case errors.Is(err, rules.ErrNoRulesFound):
		return "review rule が 1 件も見つかりません (`.rilll-config/review-rules/*.yaml` が 0 件、または全て skip)。最低 1 件の有効な rule を配置してください。"
	case errors.Is(err, rules.ErrDuplicateRuleName):
		var dup *rules.DuplicateRuleNameError
		if errors.As(err, &dup) {
			return fmt.Sprintf("rule name %q が複数の YAML で重複しています: %v。各 rule の `name` をユニークにしてください。", dup.Name, dup.Files)
		}
		return fmt.Sprintf("rule name の重複を検出しました: %v", err)
	default:
		return fmt.Sprintf("review rule の読み込みに失敗しました: %v", err)
	}
}

// formatViewPRError は gh.ViewPR の sentinel error を summary に整形する (Req 2.3)。
func formatViewPRError(err error, num int) string {
	switch {
	case errors.Is(err, gh.ErrPRNotFound):
		return fmt.Sprintf("PR #%d が見つかりません。現在の repo に該当 PR が存在し、アクセス権があることを確認してください。", num)
	case errors.Is(err, gh.ErrUnauthorized):
		return "gh CLI が未認証になりました。`gh auth login` で再認証してください。"
	case errors.Is(err, gh.ErrCLIMissing):
		return "gh CLI が利用条件を満たしません (古い gh の可能性)。`gh --version` で更新を確認してください。"
	default:
		return fmt.Sprintf("PR #%d のメタ情報取得に失敗しました: %v", num, err)
	}
}

// formatDiffPRError は gh.DiffPR の sentinel error を summary に整形する。
func formatDiffPRError(err error, num int) string {
	switch {
	case errors.Is(err, gh.ErrPRNotFound):
		return fmt.Sprintf("PR #%d の diff 取得時に PR が見つかりませんでした (ViewPR 後に削除された可能性)。", num)
	case errors.Is(err, gh.ErrUnauthorized):
		return "gh CLI が未認証になりました。`gh auth login` で再認証してください。"
	default:
		return fmt.Sprintf("PR #%d の diff 取得に失敗しました: %v", num, err)
	}
}

// formatListPRsError は gh.ListPRs の sentinel error を summary に整形する (TUI 経路)。
func formatListPRsError(err error) string {
	switch {
	case errors.Is(err, gh.ErrUnauthorized):
		return "gh CLI が未認証です。`gh auth login` で認証を完了してから再実行してください。"
	case errors.Is(err, gh.ErrCLIMissing):
		return "gh CLI が利用条件を満たしません。`gh --version` で更新を確認してください。"
	default:
		return fmt.Sprintf("PR 一覧 (`gh pr list`) の取得に失敗しました: %v", err)
	}
}
