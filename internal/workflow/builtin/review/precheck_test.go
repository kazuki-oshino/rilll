// Package review のテスト本ファイルは runPrecheck の公開契約 (tasks.md 2.3 /
// design.md §review.precheck / Req 1.4, 2.1, 2.2, 2.3, 2.4, 3.1, 3.2, 3.3) を
// fake gh + fake CLIAsk + fake TUI WorkflowAsk で網羅的に検証する。
//
// 検証観点 (tasks.md 2.3 観察可能完了条件):
//
//   - gh 不在 / 未認証 / rules dir 不在 / rule 0 件 / 重複 name / --pr 不在 / --pr 非数値 /
//     PR 不存在 の失敗ケースで Verdict=precheck_failed + 該当 summary を返す。
//   - 成功ケースで Rules (≥1 件、ファイル名昇順) + PRMeta + PRDiff が埋まる。
//   - CLI 経路では `gh pr list` を呼ばない (latency regression 回避)。
//   - TUI 経路では `gh pr list --state all --limit 200` で Items を構築 → ask.Select → ViewPR。
package review

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/gh"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/result"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/rules"
)

// fakeGhClient は precheckGhClient の test 専用 fake。各メソッドの戻り値を field で構成し、
// 呼び出し回数 / 受け取った引数を観測する。
type fakeGhClient struct {
	checkCLIErr error

	listPRsItems []gh.PRListItem
	listPRsErr   error

	viewPRMeta gh.PRMeta
	viewPRErr  error

	diffPRStr string
	diffPRErr error

	// 呼び出し観測 (latency regression / TUI seam の検証用)。
	listPRsCalls int
	viewPRCalls  int
	diffPRCalls  int
	checkCalls   int

	// listPRs の最終呼び出し引数 (state="all", limit=200 を検証する)。
	lastListState string
	lastListLimit int
}

func (f *fakeGhClient) CheckCLI(_ context.Context) error {
	f.checkCalls++
	return f.checkCLIErr
}

func (f *fakeGhClient) ListPRs(_ context.Context, _ string, state string, limit int) ([]gh.PRListItem, error) {
	f.listPRsCalls++
	f.lastListState = state
	f.lastListLimit = limit
	return f.listPRsItems, f.listPRsErr
}

func (f *fakeGhClient) ViewPR(_ context.Context, _ string, _ int) (gh.PRMeta, error) {
	f.viewPRCalls++
	return f.viewPRMeta, f.viewPRErr
}

func (f *fakeGhClient) DiffPR(_ context.Context, _ string, _ int) (string, error) {
	f.diffPRCalls++
	return f.diffPRStr, f.diffPRErr
}

var _ precheckGhClient = (*fakeGhClient)(nil)

// fakeWorkflowAsk は workflow.WorkflowAsk の test 専用 fake (TUI 経路用)。
//
// `ID` に返したい PR 番号文字列を、`OK` に選択完了フラグを設定する。
// `Err` を non-nil にすると ask.Select 自体の失敗をシミュレートする。
type fakeWorkflowAsk struct {
	ID  string
	OK  bool
	Err error

	// 観測フィールド。
	calls          int
	receivedKey    string
	receivedItems  []workflow.SelectItem
	receivedPrompt string
}

func (f *fakeWorkflowAsk) Select(_ context.Context, params workflow.SelectParams) (string, bool, error) {
	f.calls++
	f.receivedKey = params.Key
	f.receivedPrompt = params.Prompt
	f.receivedItems = append([]workflow.SelectItem(nil), params.Items...)
	if f.Err != nil {
		return "", false, f.Err
	}
	return f.ID, f.OK, nil
}

var _ workflow.WorkflowAsk = (*fakeWorkflowAsk)(nil)

// stubLoaderOK は採用 rule 1〜N 件 + 任意の Warnings を返す stub loader を組み立てる。
func stubLoaderOK(loaded []rules.Rule, warnings []rules.Warning) precheckRulesLoader {
	return func(_ string) ([]rules.Rule, []rules.Warning, error) {
		return loaded, warnings, nil
	}
}

// stubLoaderErr は sentinel error を返す stub loader を組み立てる。
func stubLoaderErr(err error, warnings []rules.Warning) precheckRulesLoader {
	return func(_ string) ([]rules.Rule, []rules.Warning, error) {
		return nil, warnings, err
	}
}

// happyRule はテスト用に最小有効な Rule を返す。
func happyRule(name, source string) rules.Rule {
	return rules.Rule{
		Name:        name,
		Description: "テスト用 rule",
		Provider:    "claude",
		Model:       "claude-sonnet-4-5",
		AppliesWhen: "常に適用",
		Prompt:      "レビューしてください",
		SourceFile:  source,
	}
}

// ---------------------- gh CheckCLI 経路 (Req 3.1) ----------------------

// TestRunPrecheck_GhCLIMissing は gh CLI 不在で precheck_failed + インストール案内が出ることを検証する。
func TestRunPrecheck_GhCLIMissing(t *testing.T) {
	t.Parallel()

	fakeGh := &fakeGhClient{checkCLIErr: gh.ErrCLIMissing}

	got, err := runPrecheck(context.Background(), precheckInput{
		cwd:       t.TempDir(),
		ask:       &workflow.CLIAsk{Values: map[string]string{"pr": "42"}},
		gh:        fakeGh,
		loadRules: stubLoaderOK([]rules.Rule{happyRule("a", "a.yaml")}, nil),
	})
	if err != nil {
		t.Fatalf("runPrecheck error = %v, want nil", err)
	}
	if got.Verdict != result.VerdictPrecheckFailed {
		t.Errorf("Verdict = %q, want %q", got.Verdict, result.VerdictPrecheckFailed)
	}
	if !strings.Contains(got.Summary, "gh CLI が見つかりません") {
		t.Errorf("Summary に gh CLI 不在の説明が含まれない: %q", got.Summary)
	}
	// precheck 失敗時は副作用 (rules ロード以降) を起動しない契約 (Req 3.3)。
	if fakeGh.listPRsCalls != 0 || fakeGh.viewPRCalls != 0 || fakeGh.diffPRCalls != 0 {
		t.Errorf("CheckCLI 失敗で後段副作用が走った: list=%d view=%d diff=%d",
			fakeGh.listPRsCalls, fakeGh.viewPRCalls, fakeGh.diffPRCalls)
	}
}

// TestRunPrecheck_GhUnauthorized は gh 未認証で precheck_failed + 認証案内が出ることを検証する。
func TestRunPrecheck_GhUnauthorized(t *testing.T) {
	t.Parallel()

	fakeGh := &fakeGhClient{checkCLIErr: gh.ErrUnauthorized}

	got, _ := runPrecheck(context.Background(), precheckInput{
		cwd:       t.TempDir(),
		ask:       &workflow.CLIAsk{Values: map[string]string{"pr": "42"}},
		gh:        fakeGh,
		loadRules: stubLoaderOK([]rules.Rule{happyRule("a", "a.yaml")}, nil),
	})
	if got.Verdict != result.VerdictPrecheckFailed {
		t.Errorf("Verdict = %q, want precheck_failed", got.Verdict)
	}
	if !strings.Contains(got.Summary, "gh auth login") {
		t.Errorf("Summary に gh auth login の案内が含まれない: %q", got.Summary)
	}
}

// ---------------------- rules.LoadRules 経路 (Req 1.4 / 3.2) ----------------------

// TestRunPrecheck_RulesDirMissing は rules dir 不在で precheck_failed が出ることを検証する。
func TestRunPrecheck_RulesDirMissing(t *testing.T) {
	t.Parallel()

	got, _ := runPrecheck(context.Background(), precheckInput{
		cwd:       t.TempDir(),
		ask:       &workflow.CLIAsk{Values: map[string]string{"pr": "42"}},
		gh:        &fakeGhClient{},
		loadRules: stubLoaderErr(rules.ErrRulesDirMissing, nil),
	})
	if got.Verdict != result.VerdictPrecheckFailed {
		t.Errorf("Verdict = %q, want precheck_failed", got.Verdict)
	}
	if !strings.Contains(got.Summary, ".rilll-config/review-rules/") {
		t.Errorf("Summary に rules dir のパスが含まれない: %q", got.Summary)
	}
}

// TestRunPrecheck_NoRulesFound は rule 0 件 (全 skip 含む) で precheck_failed が出ることを検証する。
func TestRunPrecheck_NoRulesFound(t *testing.T) {
	t.Parallel()

	warnings := []rules.Warning{
		{File: "broken.yaml", Reason: "yaml decode failed"},
	}

	got, _ := runPrecheck(context.Background(), precheckInput{
		cwd:       t.TempDir(),
		ask:       &workflow.CLIAsk{Values: map[string]string{"pr": "42"}},
		gh:        &fakeGhClient{},
		loadRules: stubLoaderErr(rules.ErrNoRulesFound, warnings),
	})
	if got.Verdict != result.VerdictPrecheckFailed {
		t.Errorf("Verdict = %q, want precheck_failed", got.Verdict)
	}
	if !strings.Contains(got.Summary, "1 件も見つかりません") {
		t.Errorf("Summary に 0 件の説明が含まれない: %q", got.Summary)
	}
	// LoadRules で集約された Warnings が precheckResult に転記されていること。
	if len(got.Warnings) != 1 || got.Warnings[0].File != "broken.yaml" {
		t.Errorf("Warnings = %+v, want broken.yaml の 1 件", got.Warnings)
	}
}

// TestRunPrecheck_DuplicateRuleName は同一 name の YAML が複数あるとき precheck_failed が出ることを検証する。
func TestRunPrecheck_DuplicateRuleName(t *testing.T) {
	t.Parallel()

	dupErr := &rules.DuplicateRuleNameError{
		Name:  "security-scan",
		Files: []string{"a.yaml", "b.yaml"},
	}

	got, _ := runPrecheck(context.Background(), precheckInput{
		cwd:       t.TempDir(),
		ask:       &workflow.CLIAsk{Values: map[string]string{"pr": "42"}},
		gh:        &fakeGhClient{},
		loadRules: stubLoaderErr(dupErr, nil),
	})
	if got.Verdict != result.VerdictPrecheckFailed {
		t.Errorf("Verdict = %q, want precheck_failed", got.Verdict)
	}
	if !strings.Contains(got.Summary, "security-scan") {
		t.Errorf("Summary に重複 name が含まれない: %q", got.Summary)
	}
	if !strings.Contains(got.Summary, "a.yaml") || !strings.Contains(got.Summary, "b.yaml") {
		t.Errorf("Summary に該当 YAML ファイル名が含まれない: %q", got.Summary)
	}
}

// ---------------------- --pr 引数 (Req 2.4) ----------------------

// TestRunPrecheck_PRArgMissing は CLIAsk で --pr が無いとき precheck_failed になることを検証する。
func TestRunPrecheck_PRArgMissing(t *testing.T) {
	t.Parallel()

	fakeGh := &fakeGhClient{}

	got, _ := runPrecheck(context.Background(), precheckInput{
		cwd: t.TempDir(),
		// `--pr` 不在 (空 map)
		ask:       &workflow.CLIAsk{Values: map[string]string{}},
		gh:        fakeGh,
		loadRules: stubLoaderOK([]rules.Rule{happyRule("a", "a.yaml")}, nil),
	})
	if got.Verdict != result.VerdictPrecheckFailed {
		t.Errorf("Verdict = %q, want precheck_failed", got.Verdict)
	}
	if !strings.Contains(got.Summary, "--pr") {
		t.Errorf("Summary に --pr の案内が含まれない: %q", got.Summary)
	}
	// CLI 経路では gh pr list を絶対に呼ばない (design.md §review.precheck の latency regression 回避)。
	if fakeGh.listPRsCalls != 0 {
		t.Errorf("CLI 経路で gh pr list が呼ばれた (latency regression): %d", fakeGh.listPRsCalls)
	}
	// PR 検証側 (ViewPR / DiffPR) も走らないこと。
	if fakeGh.viewPRCalls != 0 || fakeGh.diffPRCalls != 0 {
		t.Errorf("--pr 不在で ViewPR/DiffPR が走った: view=%d diff=%d", fakeGh.viewPRCalls, fakeGh.diffPRCalls)
	}
}

// TestRunPrecheck_PRArgNonNumeric は CLIAsk の --pr が非数値のとき precheck_failed になることを検証する。
func TestRunPrecheck_PRArgNonNumeric(t *testing.T) {
	t.Parallel()

	got, _ := runPrecheck(context.Background(), precheckInput{
		cwd:       t.TempDir(),
		ask:       &workflow.CLIAsk{Values: map[string]string{"pr": "not-a-number"}},
		gh:        &fakeGhClient{},
		loadRules: stubLoaderOK([]rules.Rule{happyRule("a", "a.yaml")}, nil),
	})
	if got.Verdict != result.VerdictPrecheckFailed {
		t.Errorf("Verdict = %q, want precheck_failed", got.Verdict)
	}
	if !strings.Contains(got.Summary, "正の整数") {
		t.Errorf("Summary に 正の整数 の説明が含まれない: %q", got.Summary)
	}
}

// TestRunPrecheck_PRArgZeroOrNegative は --pr=0 / --pr=-1 で precheck_failed になることを検証する。
func TestRunPrecheck_PRArgZeroOrNegative(t *testing.T) {
	t.Parallel()

	cases := []string{"0", "-1"}
	for _, raw := range cases {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			got, _ := runPrecheck(context.Background(), precheckInput{
				cwd:       t.TempDir(),
				ask:       &workflow.CLIAsk{Values: map[string]string{"pr": raw}},
				gh:        &fakeGhClient{},
				loadRules: stubLoaderOK([]rules.Rule{happyRule("a", "a.yaml")}, nil),
			})
			if got.Verdict != result.VerdictPrecheckFailed {
				t.Errorf("Verdict = %q, want precheck_failed", got.Verdict)
			}
		})
	}
}

// ---------------------- gh pr view (Req 2.3) ----------------------

// TestRunPrecheck_PRNotFound は ViewPR が ErrPRNotFound を返すとき precheck_failed になることを検証する。
func TestRunPrecheck_PRNotFound(t *testing.T) {
	t.Parallel()

	fakeGh := &fakeGhClient{
		viewPRErr: gh.ErrPRNotFound,
	}

	got, _ := runPrecheck(context.Background(), precheckInput{
		cwd:       t.TempDir(),
		ask:       &workflow.CLIAsk{Values: map[string]string{"pr": "999"}},
		gh:        fakeGh,
		loadRules: stubLoaderOK([]rules.Rule{happyRule("a", "a.yaml")}, nil),
	})
	if got.Verdict != result.VerdictPrecheckFailed {
		t.Errorf("Verdict = %q, want precheck_failed", got.Verdict)
	}
	if !strings.Contains(got.Summary, "PR #999") {
		t.Errorf("Summary に PR 番号が含まれない: %q", got.Summary)
	}
	// ViewPR は 1 回呼ばれるが、Diff 取得は走らない (PR 不存在で副作用を停止する契約)。
	if fakeGh.viewPRCalls != 1 || fakeGh.diffPRCalls != 0 {
		t.Errorf("副作用順序違反: view=%d diff=%d", fakeGh.viewPRCalls, fakeGh.diffPRCalls)
	}
}

// TestRunPrecheck_DiffPRFails は DiffPR が失敗するとき precheck_failed になることを検証する。
func TestRunPrecheck_DiffPRFails(t *testing.T) {
	t.Parallel()

	fakeGh := &fakeGhClient{
		viewPRMeta: gh.PRMeta{Number: 42, Title: "fix bug"},
		diffPRErr:  errors.New("disk full"),
	}

	got, _ := runPrecheck(context.Background(), precheckInput{
		cwd:       t.TempDir(),
		ask:       &workflow.CLIAsk{Values: map[string]string{"pr": "42"}},
		gh:        fakeGh,
		loadRules: stubLoaderOK([]rules.Rule{happyRule("a", "a.yaml")}, nil),
	})
	if got.Verdict != result.VerdictPrecheckFailed {
		t.Errorf("Verdict = %q, want precheck_failed", got.Verdict)
	}
	if !strings.Contains(got.Summary, "PR #42") {
		t.Errorf("Summary に PR 番号が含まれない: %q", got.Summary)
	}
}

// ---------------------- happy path: CLI 経路 ----------------------

// TestRunPrecheck_CLIPath_Happy は CLI 経路 happy path で Rules / PRMeta / PRDiff が埋まり、
// gh pr list が呼ばれない (latency regression 回避) ことを検証する。
func TestRunPrecheck_CLIPath_Happy(t *testing.T) {
	t.Parallel()

	wantMeta := gh.PRMeta{
		Number:    42,
		Title:     "fix bug",
		Body:      "body text",
		Files:     []string{"a.go"},
		Additions: 10,
		Deletions: 2,
	}
	wantDiff := "diff --git a/a.go b/a.go\n+ added line\n"

	fakeGh := &fakeGhClient{
		viewPRMeta: wantMeta,
		diffPRStr:  wantDiff,
	}

	loaded := []rules.Rule{
		happyRule("aaa", "aaa.yaml"),
		happyRule("bbb", "bbb.yaml"),
	}

	got, err := runPrecheck(context.Background(), precheckInput{
		cwd:       t.TempDir(),
		ask:       &workflow.CLIAsk{Values: map[string]string{"pr": "42"}},
		gh:        fakeGh,
		loadRules: stubLoaderOK(loaded, nil),
	})
	if err != nil {
		t.Fatalf("runPrecheck error = %v", err)
	}
	if got.Verdict != result.VerdictUnset {
		t.Errorf("Verdict = %q, want unset (precheck 通過)", got.Verdict)
	}
	if got.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", got.PRNumber)
	}
	if got.PRMeta.Title != "fix bug" {
		t.Errorf("PRMeta.Title = %q, want fix bug", got.PRMeta.Title)
	}
	if got.PRDiff != wantDiff {
		t.Errorf("PRDiff = %q, want %q", got.PRDiff, wantDiff)
	}
	if len(got.Rules) != 2 || got.Rules[0].Name != "aaa" || got.Rules[1].Name != "bbb" {
		t.Errorf("Rules = %+v, want [aaa, bbb] (ファイル名昇順)", got.Rules)
	}

	// CLI 経路では `gh pr list` は呼ばれない (Req 2.4 / design.md §review.precheck Risks)。
	if fakeGh.listPRsCalls != 0 {
		t.Errorf("CLI 経路で gh pr list が呼ばれた (latency regression): %d 回", fakeGh.listPRsCalls)
	}
	// CheckCLI / ViewPR / DiffPR は 1 回ずつ呼ばれる。
	if fakeGh.checkCalls != 1 || fakeGh.viewPRCalls != 1 || fakeGh.diffPRCalls != 1 {
		t.Errorf("呼び出し回数: check=%d view=%d diff=%d, want 1/1/1",
			fakeGh.checkCalls, fakeGh.viewPRCalls, fakeGh.diffPRCalls)
	}
}

// ---------------------- happy path: TUI 経路 ----------------------

// TestRunPrecheck_TUIPath_Happy は TUI 経路 happy path で gh pr list で Items を構築し、
// ask.Select で番号を取得して PRMeta / PRDiff が埋まることを検証する。
func TestRunPrecheck_TUIPath_Happy(t *testing.T) {
	t.Parallel()

	listItems := []gh.PRListItem{
		{Number: 41, Title: "old PR"},
		{Number: 42, Title: "fix bug"},
	}
	wantMeta := gh.PRMeta{Number: 42, Title: "fix bug"}

	fakeGh := &fakeGhClient{
		listPRsItems: listItems,
		viewPRMeta:   wantMeta,
		diffPRStr:    "fake diff",
	}
	ask := &fakeWorkflowAsk{ID: "42", OK: true}

	got, err := runPrecheck(context.Background(), precheckInput{
		cwd:       t.TempDir(),
		ask:       ask,
		gh:        fakeGh,
		loadRules: stubLoaderOK([]rules.Rule{happyRule("a", "a.yaml")}, nil),
	})
	if err != nil {
		t.Fatalf("runPrecheck error = %v", err)
	}
	if got.Verdict != result.VerdictUnset {
		t.Errorf("Verdict = %q, want unset (precheck 通過)", got.Verdict)
	}
	if got.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", got.PRNumber)
	}
	if got.PRMeta.Title != "fix bug" {
		t.Errorf("PRMeta.Title = %q, want fix bug", got.PRMeta.Title)
	}

	// TUI 経路: gh pr list は state="all" / limit=200 で 1 回呼ばれる (design.md §review.precheck)。
	if fakeGh.listPRsCalls != 1 {
		t.Errorf("ListPRs 呼び出し回数: %d, want 1", fakeGh.listPRsCalls)
	}
	if fakeGh.lastListState != "all" {
		t.Errorf("ListPRs state = %q, want \"all\"", fakeGh.lastListState)
	}
	if fakeGh.lastListLimit != 200 {
		t.Errorf("ListPRs limit = %d, want 200", fakeGh.lastListLimit)
	}

	// ask.Select に渡された Items が ListPRs の出力から構築されていること。
	if ask.calls != 1 {
		t.Fatalf("ask.Select 呼び出し回数: %d, want 1", ask.calls)
	}
	if ask.receivedKey != askPRKey {
		t.Errorf("Select Key = %q, want %q", ask.receivedKey, askPRKey)
	}
	if len(ask.receivedItems) != 2 {
		t.Fatalf("Select Items 件数 = %d, want 2", len(ask.receivedItems))
	}
	if ask.receivedItems[0].ID != "41" || ask.receivedItems[1].ID != "42" {
		t.Errorf("Select Items IDs = [%q, %q], want [\"41\", \"42\"]",
			ask.receivedItems[0].ID, ask.receivedItems[1].ID)
	}
	if !strings.Contains(ask.receivedItems[1].Label, "#42") || !strings.Contains(ask.receivedItems[1].Label, "fix bug") {
		t.Errorf("Select Items[1].Label = %q (PR 番号 + title を含むべき)", ask.receivedItems[1].Label)
	}
}

// TestRunPrecheck_TUIPath_ListPRsEmpty は TUI 経路で gh pr list が 0 件のとき
// precheck_failed になることを検証する。
func TestRunPrecheck_TUIPath_ListPRsEmpty(t *testing.T) {
	t.Parallel()

	fakeGh := &fakeGhClient{
		listPRsItems: nil,
	}
	ask := &fakeWorkflowAsk{}

	got, _ := runPrecheck(context.Background(), precheckInput{
		cwd:       t.TempDir(),
		ask:       ask,
		gh:        fakeGh,
		loadRules: stubLoaderOK([]rules.Rule{happyRule("a", "a.yaml")}, nil),
	})
	if got.Verdict != result.VerdictPrecheckFailed {
		t.Errorf("Verdict = %q, want precheck_failed", got.Verdict)
	}
	if !strings.Contains(got.Summary, "1 件も見つかりません") {
		t.Errorf("Summary に PR 0 件の説明が含まれない: %q", got.Summary)
	}
	// ask.Select は 0 件のとき呼ばない (Items 構築前で停止する契約)。
	if ask.calls != 0 {
		t.Errorf("PR 0 件で ask.Select が呼ばれた: %d", ask.calls)
	}
	// ViewPR / DiffPR も走らない。
	if fakeGh.viewPRCalls != 0 || fakeGh.diffPRCalls != 0 {
		t.Errorf("PR 0 件で副作用が走った: view=%d diff=%d", fakeGh.viewPRCalls, fakeGh.diffPRCalls)
	}
}

// TestRunPrecheck_TUIPath_ListPRsError は gh pr list が失敗したとき precheck_failed になることを検証する。
func TestRunPrecheck_TUIPath_ListPRsError(t *testing.T) {
	t.Parallel()

	fakeGh := &fakeGhClient{
		listPRsErr: gh.ErrUnauthorized,
	}
	ask := &fakeWorkflowAsk{}

	got, _ := runPrecheck(context.Background(), precheckInput{
		cwd:       t.TempDir(),
		ask:       ask,
		gh:        fakeGh,
		loadRules: stubLoaderOK([]rules.Rule{happyRule("a", "a.yaml")}, nil),
	})
	if got.Verdict != result.VerdictPrecheckFailed {
		t.Errorf("Verdict = %q, want precheck_failed", got.Verdict)
	}
	if !strings.Contains(got.Summary, "gh auth login") {
		t.Errorf("Summary に gh auth login 案内が含まれない: %q", got.Summary)
	}
}

// TestRunPrecheck_TUIPath_AskCancelled は ask.Select が ok=false (ESC) を返すとき
// precheck_failed になることを検証する。
func TestRunPrecheck_TUIPath_AskCancelled(t *testing.T) {
	t.Parallel()

	fakeGh := &fakeGhClient{
		listPRsItems: []gh.PRListItem{{Number: 42, Title: "fix bug"}},
	}
	ask := &fakeWorkflowAsk{ID: "", OK: false}

	got, _ := runPrecheck(context.Background(), precheckInput{
		cwd:       t.TempDir(),
		ask:       ask,
		gh:        fakeGh,
		loadRules: stubLoaderOK([]rules.Rule{happyRule("a", "a.yaml")}, nil),
	})
	if got.Verdict != result.VerdictPrecheckFailed {
		t.Errorf("Verdict = %q, want precheck_failed", got.Verdict)
	}
	if !strings.Contains(got.Summary, "キャンセル") {
		t.Errorf("Summary にキャンセル説明が含まれない: %q", got.Summary)
	}
	// 後段 ViewPR / DiffPR は走らない。
	if fakeGh.viewPRCalls != 0 || fakeGh.diffPRCalls != 0 {
		t.Errorf("ESC 後に副作用が走った: view=%d diff=%d", fakeGh.viewPRCalls, fakeGh.diffPRCalls)
	}
}

// TestRunPrecheck_TUIPath_AskError は ask.Select 自体が error を返すとき precheck_failed になることを検証する。
func TestRunPrecheck_TUIPath_AskError(t *testing.T) {
	t.Parallel()

	fakeGh := &fakeGhClient{
		listPRsItems: []gh.PRListItem{{Number: 42, Title: "fix bug"}},
	}
	ask := &fakeWorkflowAsk{Err: fmt.Errorf("BubbleTea I/O 失敗")}

	got, _ := runPrecheck(context.Background(), precheckInput{
		cwd:       t.TempDir(),
		ask:       ask,
		gh:        fakeGh,
		loadRules: stubLoaderOK([]rules.Rule{happyRule("a", "a.yaml")}, nil),
	})
	if got.Verdict != result.VerdictPrecheckFailed {
		t.Errorf("Verdict = %q, want precheck_failed", got.Verdict)
	}
	if !strings.Contains(got.Summary, "PR 選択に失敗") {
		t.Errorf("Summary に PR 選択失敗の説明が含まれない: %q", got.Summary)
	}
}

// TestRunPrecheck_NoSideEffectOnFailure は precheck 失敗時に副作用 (per-rule artifact 等) が
// 一切発生しない契約 (Req 3.3) を、gh / loader の各失敗経路で stub 呼び出し回数として検証する。
func TestRunPrecheck_NoSideEffectOnFailure(t *testing.T) {
	t.Parallel()

	// CheckCLI 失敗 → loader / list / view / diff いずれも呼ばれない。
	loaderCalls := 0
	loader := func(_ string) ([]rules.Rule, []rules.Warning, error) {
		loaderCalls++
		return []rules.Rule{happyRule("a", "a.yaml")}, nil, nil
	}
	fakeGh := &fakeGhClient{checkCLIErr: gh.ErrCLIMissing}

	_, _ = runPrecheck(context.Background(), precheckInput{
		cwd:       t.TempDir(),
		ask:       &workflow.CLIAsk{Values: map[string]string{"pr": "42"}},
		gh:        fakeGh,
		loadRules: loader,
	})

	if loaderCalls != 0 {
		t.Errorf("CheckCLI 失敗で loader が呼ばれた: %d", loaderCalls)
	}
	if fakeGh.listPRsCalls != 0 || fakeGh.viewPRCalls != 0 || fakeGh.diffPRCalls != 0 {
		t.Errorf("CheckCLI 失敗後に副作用が走った: list=%d view=%d diff=%d",
			fakeGh.listPRsCalls, fakeGh.viewPRCalls, fakeGh.diffPRCalls)
	}
}
