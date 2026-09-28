package kiro

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/inquiryloop"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
)

// TestParseChangedFilesCount は `parseChangedFilesCount` の境界値挙動を固定する。
// 受理条件: 行頭固定の `Files Modified: <decimal>$` (大文字小文字一致 / ASCII 半角数字のみ)。
// それ以外 (マーカ未出力 / 行頭以外 / 大文字小文字違い / 全角数字 / 数値以外) は error を返す。
func TestParseChangedFilesCount(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		want      int
		expectErr bool
	}{
		{
			name:  "zero count on dedicated line",
			input: "summary line\n\nFiles Modified: 0\n",
			want:  0,
		},
		{
			name:  "single digit count on dedicated line",
			input: "fix summary\n\nFiles Modified: 3\n",
			want:  3,
		},
		{
			name:  "double digit count",
			input: "Files Modified: 12\n",
			want:  12,
		},
		{
			name:  "tab whitespace between colon and digits is allowed by \\s*",
			input: "Files Modified:\t7\n",
			want:  7,
		},
		{
			name:      "missing marker returns error",
			input:     "ファイルを修正しました。詳細は省略。\n",
			expectErr: true,
		},
		{
			name:      "leading whitespace breaks line head anchor",
			input:     "summary\n   Files Modified: 3\n",
			expectErr: true,
		},
		{
			name:      "inline occurrence (mid-line) breaks line head anchor",
			input:     "Note: Files Modified: 3 in this run.\n",
			expectErr: true,
		},
		{
			name:      "lowercase prefix is rejected (case sensitive)",
			input:     "files modified: 3\n",
			expectErr: true,
		},
		{
			name:      "fullwidth digits are rejected by ASCII \\d",
			input:     "Files Modified: ３\n",
			expectErr: true,
		},
		{
			name:      "non-numeric value is rejected",
			input:     "Files Modified: many\n",
			expectErr: true,
		},
		{
			name:      "trailing characters after number break $ anchor",
			input:     "Files Modified: 3 files\n",
			expectErr: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseChangedFilesCount(tc.input)
			if tc.expectErr {
				if err == nil {
					t.Fatalf("parseChangedFilesCount(%q) = (%d, nil), want error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseChangedFilesCount(%q) returned unexpected error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("parseChangedFilesCount(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

// TestParseTestResult は `parseTestResult` の境界値挙動を固定する。
// 受理条件: 行頭固定の `Test Result: pass$` または `Test Result: fail$`。
// それ以外 (マーカ未出力 / 行頭以外 / 大文字小文字違い / 想定外の値) は error を返す。
func TestParseTestResult(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		want      bool
		expectErr bool
	}{
		{
			name:  "pass on dedicated line",
			input: "summary\n\nTest Result: pass\nFiles Modified: 3\n",
			want:  true,
		},
		{
			name:  "fail on dedicated line",
			input: "summary\n\nTest Result: fail\nFiles Modified: 0\n",
			want:  false,
		},
		{
			name:      "missing marker returns error",
			input:     "テストを実行したが結果サマリ行を含めないケース\n",
			expectErr: true,
		},
		{
			name:      "leading whitespace breaks line head anchor",
			input:     "  Test Result: pass\n",
			expectErr: true,
		},
		{
			name:      "inline occurrence (mid-line) breaks line head anchor",
			input:     "Note: Test Result: pass in this run.\n",
			expectErr: true,
		},
		{
			name:      "uppercase value is rejected (case sensitive)",
			input:     "Test Result: PASS\n",
			expectErr: true,
		},
		{
			name:      "unexpected value is rejected",
			input:     "Test Result: skipped\n",
			expectErr: true,
		},
		{
			name:      "trailing characters after value break $ anchor",
			input:     "Test Result: pass (3 suites)\n",
			expectErr: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTestResult(tc.input)
			if tc.expectErr {
				if err == nil {
					t.Fatalf("parseTestResult(%q) = (%v, nil), want error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTestResult(%q) returned unexpected error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("parseTestResult(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// containsAll は wantPhrases に含まれる substring がすべて s に含まれていれば true を返す。
// 1 つでも欠けると最初に見つかった欠落 phrase を返す。プロンプトビルダーの「必須文面」検査で
// substring 一致 (exact phrase 含有) を多重 assert するために使う。
func containsAll(s string, wantPhrases ...string) (string, bool) {
	for _, phrase := range wantPhrases {
		if !strings.Contains(s, phrase) {
			return phrase, false
		}
	}
	return "", true
}

// TestBuildFinalReviewEditPrompt は Phase 2 (fix-edit) prompt builder の必須要素を固定する。
// 必須要素: [final-review-fix-edit] マーカ / feature 名埋め込み / 「実装コード」「Edit / Write」
// 「該当 feature 責務範囲内」「markdown」 の意味的キーワード。
func TestBuildFinalReviewEditPrompt(t *testing.T) {
	const feature = "kiro-validate-impl-loop"
	got := buildFinalReviewEditPrompt(feature)

	if missing, ok := containsAll(got,
		"[final-review-fix-edit]",
		feature,
		"実装コード",
		"Edit",
		"Write",
		"責務範囲",
		"markdown",
	); !ok {
		t.Fatalf("buildFinalReviewEditPrompt: missing required phrase %q\n--- prompt ---\n%s", missing, got)
	}

	// header 行が prompt 先頭に位置することを確認 (classifyAgentCall 等の運用ログ判定向け)。
	if !strings.HasPrefix(got, "[final-review-fix-edit]") {
		t.Fatalf("buildFinalReviewEditPrompt: marker must be at the very beginning, got prefix=%q", firstLine(got))
	}
}

// TestBuildFinalReviewJudgePrompt は Phase 3 (fix-judge) prompt builder の必須要素を固定する。
// 必須要素: [final-review-fix-judge] マーカ / feature 名 / `Files Modified: <count>` 必須行の出力指示 /
// 構造化出力 (json_schema / has_inquiry=false) の指示。
func TestBuildFinalReviewJudgePrompt(t *testing.T) {
	const feature = "kiro-validate-impl-loop"
	got := buildFinalReviewJudgePrompt(feature)

	if missing, ok := containsAll(got,
		"[final-review-fix-judge]",
		feature,
		"Files Modified:",
		"必須",
		"has_inquiry",
		"summary",
	); !ok {
		t.Fatalf("buildFinalReviewJudgePrompt: missing required phrase %q\n--- prompt ---\n%s", missing, got)
	}

	if !strings.HasPrefix(got, "[final-review-fix-judge]") {
		t.Fatalf("buildFinalReviewJudgePrompt: marker must be at the very beginning, got prefix=%q", firstLine(got))
	}
}

// TestBuildFinalReviewTestEditPrompt は Phase 4 Turn 1 (test-edit) prompt builder の必須要素を固定する。
// 必須要素: [final-review-test-edit] マーカ / feature 名 / test 規約発見指示 (justfile / Makefile /
// package.json) / iterate 上限 5 回 / 上限到達時の試行履歴 markdown 出力規約 / Bash と Edit/Write の許可。
func TestBuildFinalReviewTestEditPrompt(t *testing.T) {
	const feature = "kiro-validate-impl-loop"
	got := buildFinalReviewTestEditPrompt(feature)

	if missing, ok := containsAll(got,
		"[final-review-test-edit]",
		feature,
		"justfile",
		"Makefile",
		"package.json",
		"全件",
		"5",
		"iterate",
		"上限",
		"Bash",
		"Edit",
		"Write",
	); !ok {
		t.Fatalf("buildFinalReviewTestEditPrompt: missing required phrase %q\n--- prompt ---\n%s", missing, got)
	}

	// 具体 test コマンド (just test / npm test / go test) を直書きしていないことを保証する。
	// design.md "Test command discovery" の規約に従い、コマンド発見指示のみ含めること。
	for _, banned := range []string{"just test", "npm test", "go test", "yarn test", "pnpm test"} {
		if strings.Contains(got, banned) {
			t.Fatalf("buildFinalReviewTestEditPrompt: must not hardcode test command %q (discovery responsibility belongs to agent)", banned)
		}
	}

	if !strings.HasPrefix(got, "[final-review-test-edit]") {
		t.Fatalf("buildFinalReviewTestEditPrompt: marker must be at the very beginning, got prefix=%q", firstLine(got))
	}
}

// TestBuildFinalReviewTestJudgePrompt は Phase 4 Turn 2 (test-judge) prompt builder の必須要素を固定する。
// 必須要素: [final-review-test-judge] マーカ / feature 名 / `Test Result: pass|fail` 必須行の出力指示 /
// `Files Modified: <count>` 必須行の出力指示 / 構造化出力 (json_schema / has_inquiry=false) の指示。
func TestBuildFinalReviewTestJudgePrompt(t *testing.T) {
	const feature = "kiro-validate-impl-loop"
	got := buildFinalReviewTestJudgePrompt(feature)

	if missing, ok := containsAll(got,
		"[final-review-test-judge]",
		feature,
		"Test Result:",
		"pass",
		"fail",
		"Files Modified:",
		"必須",
		"has_inquiry",
		"summary",
	); !ok {
		t.Fatalf("buildFinalReviewTestJudgePrompt: missing required phrase %q\n--- prompt ---\n%s", missing, got)
	}

	if !strings.HasPrefix(got, "[final-review-test-judge]") {
		t.Fatalf("buildFinalReviewTestJudgePrompt: marker must be at the very beginning, got prefix=%q", firstLine(got))
	}
}

// firstLine はエラーメッセージ用に s の最初の改行までを返す薄いヘルパ。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// TestFormatTestResultMarkdown は `formatTestResultMarkdown` の出力に Phase 4 専用永続化に
// 必須となる 7 セクション (`Test Result` / `Files Modified` / `Summary` / `Message` /
// `SessionID` / `EditSession` / `JudgeSession`) と `Test Result:` 必須行が含まれることを固定する。
//
// pass / fail の双方の variant を確認し、`formatFixResultMarkdown` (spec_loop.go、Phase 2-3 専用)
// と独立形式 (heading 名・行ラベルが衝突しない) であることもあわせて検証する。
func TestFormatTestResultMarkdown(t *testing.T) {
	t.Run("pass variant", func(t *testing.T) {
		test := finalReviewTestResult{
			Ran:          true,
			OK:           true,
			TestPassed:   true,
			ChangedFiles: 3,
			Summary:      "全件 test pass、修正 3 件",
			Message:      "",
			SessionID:    "session-judge-id",
			EditSession:  "session-edit-id",
			JudgeSession: "session-judge-id",
		}
		got := formatTestResultMarkdown(7, "final-review", "impl-direction",
			"edit prompt body",
			"judge prompt body",
			test,
		)

		if missing, ok := containsAll(got,
			"# Test Result 07-final-review",
			"## Test Result",
			"## Files Modified",
			"## Summary",
			"## Message",
			"## SessionID",
			"## EditSession",
			"## JudgeSession",
			"## Edit Phase Prompt",
			"## Judge Phase Prompt",
			"edit prompt body",
			"judge prompt body",
			"Test Result: pass",
			"Files Modified: 3",
			"全件 test pass、修正 3 件",
			"session-edit-id",
			"session-judge-id",
		); !ok {
			t.Fatalf("formatTestResultMarkdown(pass): missing required content %q\n--- output ---\n%s", missing, got)
		}

		// formatFixResultMarkdown (Phase 2-3) との独立性: fix-result.md 固有の見出し
		// "# Fix Result" を含まないこと。
		if strings.Contains(got, "# Fix Result") {
			t.Fatalf("formatTestResultMarkdown(pass): must not share the `# Fix Result` heading with formatFixResultMarkdown\n--- output ---\n%s", got)
		}
	})

	t.Run("fail variant with judge skipped", func(t *testing.T) {
		test := finalReviewTestResult{
			Ran:          true,
			OK:           false,
			TestPassed:   false,
			ChangedFiles: 0,
			Summary:      "",
			Message:      "Test Result marker not found in summary",
			SessionID:    "session-edit-id",
			EditSession:  "session-edit-id",
			JudgeSession: "",
		}
		got := formatTestResultMarkdown(2, "final-review", "impl-direction",
			"edit prompt body",
			"", // Turn 2 (judge) skipped
			test,
		)

		if missing, ok := containsAll(got,
			"# Test Result 02-final-review",
			"## Test Result",
			"## Files Modified",
			"## Message",
			"Test Result: fail",
			"Files Modified: 0",
			"Test Result marker not found in summary",
		); !ok {
			t.Fatalf("formatTestResultMarkdown(fail): missing required content %q\n--- output ---\n%s", missing, got)
		}

		// Turn 2 skip 時は judge prompt セクションを出さない (空 prompt は section 自体を省略)。
		if strings.Contains(got, "## Judge Phase Prompt") {
			t.Fatalf("formatTestResultMarkdown(fail): empty judgePrompt should suppress `## Judge Phase Prompt` section\n--- output ---\n%s", got)
		}
	})
}

// TestPersistTestResult は `persistTestResult` が
// `<runDir>/<NN>-final-review/test-result.md` を作成すること、必須セクションを含むこと、
// `<NN>` が zero-padded 2 桁で per-task ループ artifact (`<NN>-impl-<task>/`) と path 衝突
// しないことを固定する (Req 5.1, 5.2)。
func TestPersistTestResult(t *testing.T) {
	t.Run("writes test-result.md with required sections", func(t *testing.T) {
		runDir := t.TempDir()
		test := finalReviewTestResult{
			Ran:          true,
			OK:           true,
			TestPassed:   true,
			ChangedFiles: 5,
			Summary:      "全件 pass",
			Message:      "",
			SessionID:    "sid-judge",
			EditSession:  "sid-edit",
			JudgeSession: "sid-judge",
		}

		if err := persistTestResult(runDir, 7, "final-review", "impl-direction",
			"edit prompt", "judge prompt", test); err != nil {
			t.Fatalf("persistTestResult unexpected error: %v", err)
		}

		// path 規約: <runDir>/07-final-review/test-result.md
		expected := filepath.Join(runDir, "07-final-review", "test-result.md")
		body, err := os.ReadFile(expected)
		if err != nil {
			t.Fatalf("expected file %s not found: %v", expected, err)
		}

		bodyStr := string(body)
		if missing, ok := containsAll(bodyStr,
			"## Test Result",
			"## Files Modified",
			"## Summary",
			"## Message",
			"## SessionID",
			"## EditSession",
			"## JudgeSession",
			"Test Result: pass",
			"Files Modified: 5",
		); !ok {
			t.Fatalf("persistTestResult output missing %q\n--- file %s ---\n%s", missing, expected, bodyStr)
		}
	})

	t.Run("zero-pads single-digit order to 2 digits", func(t *testing.T) {
		runDir := t.TempDir()
		test := finalReviewTestResult{Ran: true, OK: true, TestPassed: true}

		if err := persistTestResult(runDir, 3, "final-review", "impl-direction", "", "", test); err != nil {
			t.Fatalf("persistTestResult: %v", err)
		}
		expected := filepath.Join(runDir, "03-final-review", "test-result.md")
		if _, err := os.Stat(expected); err != nil {
			t.Fatalf("expected zero-padded path %s, stat err: %v", expected, err)
		}

		// per-task ループ artifact (`03-impl-<task>/`) と path 衝突しないこと。
		conflict := filepath.Join(runDir, "03-impl-something")
		if _, err := os.Stat(conflict); !os.IsNotExist(err) {
			t.Fatalf("persistTestResult must not create per-task path %s; stat=%v", conflict, err)
		}
	})

	t.Run("preserves 2-digit padding for double-digit order", func(t *testing.T) {
		runDir := t.TempDir()
		test := finalReviewTestResult{Ran: true, OK: true, TestPassed: true}

		if err := persistTestResult(runDir, 15, "final-review", "impl-direction", "", "", test); err != nil {
			t.Fatalf("persistTestResult: %v", err)
		}
		expected := filepath.Join(runDir, "15-final-review", "test-result.md")
		if _, err := os.Stat(expected); err != nil {
			t.Fatalf("expected %s, stat err: %v", expected, err)
		}
	})

	t.Run("creates parent directory when missing (MkdirAll)", func(t *testing.T) {
		// runDir は存在するが、<NN>-final-review/ subdirectory はまだ存在しない状態で
		// persistTestResult が成功することを確認する (best-effort 経路の前提条件)。
		runDir := t.TempDir()
		dir := filepath.Join(runDir, "09-final-review")
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("precondition: %s should not exist before call; stat=%v", dir, err)
		}

		test := finalReviewTestResult{Ran: true, OK: true, TestPassed: true}
		if err := persistTestResult(runDir, 9, "final-review", "impl-direction", "", "", test); err != nil {
			t.Fatalf("persistTestResult: %v", err)
		}

		expected := filepath.Join(dir, "test-result.md")
		if _, err := os.Stat(expected); err != nil {
			t.Fatalf("expected %s after MkdirAll, stat err: %v", expected, err)
		}
	})
}

// =============================================================================
// runFinalReviewFixPhase (Phase 2-3: fix-edit + fix-judge) のテスト
// =============================================================================
//
// design.md「runFinalReviewFixPhase」契約と Task 2.1 観測可能完了条件に従う:
//   - Turn 1 (fix-edit): resume=lastSession, OutputFormat=nil, AllowedTools=nil,
//     StepID="final-review-fix-edit", prompt=buildFinalReviewEditPrompt(feature)
//   - Turn 2 (fix-judge): resume=Turn1.SessionID, OutputFormat=inquiryloop.Schema(),
//     AllowedTools=fixJudgeAllowedTools, StepID="final-review-fix-judge",
//     prompt=buildFinalReviewJudgePrompt(feature)
//   - Turn 1 失敗 → Turn 2 skip + OK=false
//   - Turn 2 失敗 (agent err / success=false / schema 違反 / has_inquiry=true /
//     Files Modified marker 未出力) → OK=false で Turn 1 Content を Summary に救出
//   - 成功時: OK=true, ChangedFiles=parseChangedFilesCount(summary), SessionID=Turn2.session

// finalReviewFixEditOKResp は Phase 2 (fix-edit) Turn 1 が成功した場合の典型応答。
// OutputFormat=nil (markdown 自由記述) なので Content ベースで返す。
func finalReviewFixEditOKResp() provider.AgentResponse {
	return provider.AgentResponse{
		Success:   true,
		Content:   "実装コード修正を完了しました。\n\n## Summary\n- 修正したファイル: internal/workflow/builtin/kiro/impl_loop_final_review.go\n- 反映内容: Phase 2-3 の挙動契約を確定",
		SessionID: "session-final-review-fix-edit",
	}
}

// finalReviewFixJudgeOKResp は Phase 3 (fix-judge) Turn 2 が成功した場合の典型応答。
// json_schema 強制下で has_inquiry=false / summary に "Files Modified: <count>" を含める。
// count を 0 / 3 など可変にできるよう引数で受ける。
func finalReviewFixJudgeOKResp(changedFiles int) provider.AgentResponse {
	summary := "修正完了: impl_loop_final_review.go の Phase 2-3 を確定\n\nFiles Modified: " +
		strconv.Itoa(changedFiles)
	return provider.AgentResponse{
		Success: true,
		StructuredOutput: map[string]any{
			"has_inquiry": false,
			"summary":     summary,
		},
		SessionID: "session-final-review-fix-judge",
	}
}

// TestRunFinalReviewFixPhase_BothTurnsSucceed_FilesModified3 は Phase 2-3 が両ターン成功し、
// judge summary の "Files Modified: 3" が抽出されて ChangedFiles=3 になることを固定する
// (Task 2.1 観測可能完了)。
//
// 検証項目:
//   - finalReviewFixResult: Ran=true, OK=true, ChangedFiles=3,
//     SessionID=session-final-review-fix-judge (Turn 2 のもの),
//     EditSession=session-final-review-fix-edit, JudgeSession=session-final-review-fix-judge
//   - agent.Calls[0] (fix-edit): resume=lastSession, OutputFormat=nil, AllowedTools=nil,
//     StepID="final-review-fix-edit", prompt prefix="[final-review-fix-edit]"
//   - agent.Calls[1] (fix-judge): resume=session-final-review-fix-edit (Turn 1 SessionID),
//     OutputFormat 非 nil (= inquiryloop.Schema()),
//     AllowedTools=fixJudgeAllowedTools (Read/Glob/Grep),
//     StepID="final-review-fix-judge", prompt prefix="[final-review-fix-judge]"
//   - 永続化: <runDir>/<NN>-final-review/fix-result.md が書き出される
func TestRunFinalReviewFixPhase_BothTurnsSucceed_FilesModified3(t *testing.T) {
	const (
		feature     = "kiro-validate-impl-loop"
		order       = 7
		phase       = "final-review"
		lastSession = "session-final-review-skill"
		personaName = "impl-direction"
		promptLabel = "最終検証"
	)

	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "fix-edit":
			return finalReviewFixEditOKResp(), nil
		case "fix-judge":
			return finalReviewFixJudgeOKResp(3), nil
		}
		t.Fatalf("予期しない agent.Call kind=%q prompt=%q", call.Kind, call.Prompt)
		return provider.AgentResponse{}, nil
	}

	onEvent, _ := collectEvents()
	got := runFinalReviewFixPhase(
		context.Background(),
		agent,
		cwd, feature, runDir,
		order, phase,
		lastSession, personaName, promptLabel,
		onEvent,
	)

	// === finalReviewFixResult 検証 ===
	if !got.Ran {
		t.Errorf("Ran=true を期待: got=%+v", got)
	}
	if !got.OK {
		t.Errorf("OK=true を期待 (両ターン成功 + Files Modified: 3 抽出成功): got=%+v", got)
	}
	if got.ChangedFiles != 3 {
		t.Errorf("ChangedFiles=3 を期待 (judge summary の Files Modified: 3 から抽出): got=%d (full=%+v)",
			got.ChangedFiles, got)
	}
	if got.SessionID != "session-final-review-fix-judge" {
		t.Errorf("SessionID=session-final-review-fix-judge を期待 (Turn 2 sessionId が表に出る): got=%q",
			got.SessionID)
	}
	if got.EditSession != "session-final-review-fix-edit" {
		t.Errorf("EditSession=session-final-review-fix-edit を期待: got=%q", got.EditSession)
	}
	if got.JudgeSession != "session-final-review-fix-judge" {
		t.Errorf("JudgeSession=session-final-review-fix-judge を期待: got=%q", got.JudgeSession)
	}
	if !strings.Contains(got.Summary, "修正完了") {
		t.Errorf("Summary は judge summary を保持すべき: got=%q", got.Summary)
	}

	// === Turn 1 (fix-edit) call options 検証 ===
	if len(agent.Calls) != 2 {
		t.Fatalf("agent.Calls 件数 2 を期待 (Turn 1 + Turn 2): got=%d", len(agent.Calls))
	}
	editCall := agent.Calls[0]
	if editCall.Kind != "fix-edit" {
		t.Errorf("Calls[0].Kind=fix-edit を期待: got=%q", editCall.Kind)
	}
	if editCall.ResumeSessionID != lastSession {
		t.Errorf("Turn 1 resume=lastSession=%q を期待: got=%q", lastSession, editCall.ResumeSessionID)
	}
	if editCall.Options == nil {
		t.Fatalf("Turn 1 Options=nil; 必須")
	}
	if editCall.Options.OutputFormat != nil {
		t.Errorf("Turn 1 OutputFormat=nil を期待 (Edit/Write 解放、json_schema 非強制): got=%+v",
			editCall.Options.OutputFormat)
	}
	if editCall.Options.AllowedTools != nil {
		t.Errorf("Turn 1 AllowedTools=nil を期待 (SDK 既定 = 全ツール許可): got=%v",
			editCall.Options.AllowedTools)
	}
	if editCall.Options.StepID != "final-review-fix-edit" {
		t.Errorf("Turn 1 StepID=final-review-fix-edit を期待 (per-task 経路と区別): got=%q",
			editCall.Options.StepID)
	}
	if !strings.HasPrefix(editCall.Prompt, "[final-review-fix-edit]") {
		t.Errorf("Turn 1 prompt 先頭が [final-review-fix-edit] マーカで始まることを期待: got prefix=%q",
			firstLine(editCall.Prompt))
	}

	// === Turn 2 (fix-judge) call options 検証 ===
	judgeCall := agent.Calls[1]
	if judgeCall.Kind != "fix-judge" {
		t.Errorf("Calls[1].Kind=fix-judge を期待: got=%q", judgeCall.Kind)
	}
	if judgeCall.ResumeSessionID != "session-final-review-fix-edit" {
		t.Errorf("Turn 2 resume=Turn1.SessionID を期待: got=%q", judgeCall.ResumeSessionID)
	}
	if judgeCall.Options == nil {
		t.Fatalf("Turn 2 Options=nil; 必須")
	}
	if judgeCall.Options.OutputFormat == nil {
		t.Errorf("Turn 2 OutputFormat=inquiryloop.Schema() を期待 (構造化抽出強制): got=nil")
	} else if judgeCall.Options.OutputFormat.Type != "json_schema" {
		t.Errorf("Turn 2 OutputFormat.Type=json_schema を期待: got=%q", judgeCall.Options.OutputFormat.Type)
	}
	wantTools := []string{"Read", "Glob", "Grep"}
	if len(judgeCall.Options.AllowedTools) != len(wantTools) {
		t.Errorf("Turn 2 AllowedTools=fixJudgeAllowedTools を期待: got=%v want=%v",
			judgeCall.Options.AllowedTools, wantTools)
	} else {
		for i, w := range wantTools {
			if judgeCall.Options.AllowedTools[i] != w {
				t.Errorf("Turn 2 AllowedTools[%d]=%q want %q", i,
					judgeCall.Options.AllowedTools[i], w)
			}
		}
	}
	if judgeCall.Options.StepID != "final-review-fix-judge" {
		t.Errorf("Turn 2 StepID=final-review-fix-judge を期待: got=%q", judgeCall.Options.StepID)
	}
	if !strings.HasPrefix(judgeCall.Prompt, "[final-review-fix-judge]") {
		t.Errorf("Turn 2 prompt 先頭が [final-review-fix-judge] マーカで始まることを期待: got prefix=%q",
			firstLine(judgeCall.Prompt))
	}

	// === 永続化 fix-result.md 検証 ===
	expected := filepath.Join(runDir, "07-final-review", "fix-result.md")
	body, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("fix-result.md が書き出されていない: %s err=%v", expected, err)
	}
	bodyStr := string(body)
	if !strings.Contains(bodyStr, "ok: true") {
		t.Errorf("fix-result.md に 'ok: true' を期待: %q", bodyStr)
	}
	if !strings.Contains(bodyStr, "session-final-review-fix-judge") {
		t.Errorf("fix-result.md に Turn 2 sessionId を期待: %q", bodyStr)
	}
}

// TestRunFinalReviewFixPhase_FilesModified0_OK は judge summary の "Files Modified: 0" でも
// schema 適合扱いで OK=true / ChangedFiles=0 になることを固定する (Req 2.5: 修正なし許容)。
func TestRunFinalReviewFixPhase_FilesModified0_OK(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "fix-edit":
			return finalReviewFixEditOKResp(), nil
		case "fix-judge":
			return finalReviewFixJudgeOKResp(0), nil
		}
		return provider.AgentResponse{}, nil
	}
	onEvent, _ := collectEvents()

	got := runFinalReviewFixPhase(
		context.Background(),
		agent,
		cwd, "demo", runDir,
		3, "final-review",
		"session-skill", "impl-direction", "最終検証",
		onEvent,
	)

	if !got.OK {
		t.Errorf("OK=true を期待 (Files Modified: 0 でも schema 適合は OK): got=%+v", got)
	}
	if got.ChangedFiles != 0 {
		t.Errorf("ChangedFiles=0 を期待: got=%d", got.ChangedFiles)
	}
}

// TestRunFinalReviewFixPhase_Turn1AgentError_SkipsTurn2 は Turn 1 が agent error を返したとき
// Turn 2 が呼ばれず OK=false で return することを固定する (Task 2.1 観測可能完了 後半)。
func TestRunFinalReviewFixPhase_Turn1AgentError_SkipsTurn2(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "fix-edit":
			return provider.AgentResponse{}, errors.New("simulated final-review fix-edit agent error")
		case "fix-judge":
			t.Errorf("Turn 1 が agent error の場合 Turn 2 (fix-judge) は呼ばれてはならない: prompt=%q", call.Prompt)
			return provider.AgentResponse{}, nil
		}
		return provider.AgentResponse{}, nil
	}
	onEvent, _ := collectEvents()

	got := runFinalReviewFixPhase(
		context.Background(),
		agent,
		cwd, "demo", runDir,
		3, "final-review",
		"session-skill", "impl-direction", "最終検証",
		onEvent,
	)

	if !got.Ran {
		t.Errorf("Ran=true を期待 (helper を呼んだ事実): got=%+v", got)
	}
	if got.OK {
		t.Errorf("OK=false を期待 (Turn 1 agent error): got=%+v", got)
	}
	if got.JudgeSession != "" {
		t.Errorf("Turn 1 失敗時 JudgeSession は空であるべき: got=%q", got.JudgeSession)
	}
	if !strings.Contains(got.Message, "agent error") {
		t.Errorf("Message に 'agent error' を期待: got=%q", got.Message)
	}
	// fix-judge が呼ばれていないこと
	if cnt := countCallsByKind(agent.Calls, "fix-judge"); cnt != 0 {
		t.Errorf("Turn 1 失敗で fix-judge は 0 件であるべき: got=%d", cnt)
	}
}

// TestRunFinalReviewFixPhase_Turn1SuccessFalse_SkipsTurn2 は Turn 1 が success=false を返したとき
// Turn 2 が呼ばれず OK=false で return し、EditSession が記録されることを固定する。
func TestRunFinalReviewFixPhase_Turn1SuccessFalse_SkipsTurn2(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "fix-edit":
			return provider.AgentResponse{
				Success:   false,
				Content:   "Edit が allowed-tools で blocked された (試験用)",
				SessionID: "session-final-review-fix-edit-failed",
			}, nil
		case "fix-judge":
			t.Errorf("Turn 1 success=false 時 Turn 2 は呼ばれてはならない")
			return provider.AgentResponse{}, nil
		}
		return provider.AgentResponse{}, nil
	}
	onEvent, _ := collectEvents()

	got := runFinalReviewFixPhase(
		context.Background(),
		agent,
		cwd, "demo", runDir,
		3, "final-review",
		"session-skill", "impl-direction", "最終検証",
		onEvent,
	)

	if got.OK {
		t.Errorf("OK=false を期待 (Turn 1 success=false): got=%+v", got)
	}
	if got.EditSession != "session-final-review-fix-edit-failed" {
		t.Errorf("EditSession=session-final-review-fix-edit-failed を期待 (Turn 1 sessionId 保持): got=%q",
			got.EditSession)
	}
	if got.SessionID != "session-final-review-fix-edit-failed" {
		t.Errorf("SessionID は Turn 1 sessionId をフォールバックとして保持すべき: got=%q", got.SessionID)
	}
	if got.JudgeSession != "" {
		t.Errorf("Turn 1 失敗時 JudgeSession は空: got=%q", got.JudgeSession)
	}
}

// TestRunFinalReviewFixPhase_Turn2AgentError_RecoversTurn1Content は Turn 2 (fix-judge) が
// agent error で失敗したとき、Turn 1 (fix-edit) の Content (markdown) が Summary に救出される
// ことを固定する (best-effort 規約)。
func TestRunFinalReviewFixPhase_Turn2AgentError_RecoversTurn1Content(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()

	const editContent = "TURN1_MARKER: Phase 2-3 の helper を実装しました"
	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "fix-edit":
			return provider.AgentResponse{
				Success:   true,
				Content:   editContent,
				SessionID: "session-final-review-fix-edit",
			}, nil
		case "fix-judge":
			return provider.AgentResponse{}, errors.New("simulated final-review fix-judge agent error")
		}
		return provider.AgentResponse{}, nil
	}
	onEvent, _ := collectEvents()

	got := runFinalReviewFixPhase(
		context.Background(),
		agent,
		cwd, "demo", runDir,
		3, "final-review",
		"session-skill", "impl-direction", "最終検証",
		onEvent,
	)

	if got.OK {
		t.Errorf("OK=false を期待 (Turn 2 agent error): got=%+v", got)
	}
	if got.EditSession != "session-final-review-fix-edit" {
		t.Errorf("EditSession=session-final-review-fix-edit を期待: got=%q", got.EditSession)
	}
	if got.JudgeSession != "" {
		t.Errorf("Turn 2 agent error 時 JudgeSession は空: got=%q", got.JudgeSession)
	}
	if !strings.Contains(got.Summary, "TURN1_MARKER") {
		t.Errorf("Turn 2 失敗時 Summary に Turn 1 Content を救出するべき: got=%q", got.Summary)
	}
	if !strings.Contains(got.Message, "judge") {
		t.Errorf("Message に 'judge' を期待 (失敗フェーズ識別): got=%q", got.Message)
	}
}

// TestRunFinalReviewFixPhase_Turn2SchemaViolation_NoFilesModifiedMarker は Turn 2 が
// "Files Modified: <count>" 行を含まない summary を返したとき、schema-violation 扱いで
// OK=false になることを固定する (Req 2.5 / parseChangedFilesCount 契約)。
func TestRunFinalReviewFixPhase_Turn2SchemaViolation_NoFilesModifiedMarker(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "fix-edit":
			return finalReviewFixEditOKResp(), nil
		case "fix-judge":
			return provider.AgentResponse{
				Success: true,
				StructuredOutput: map[string]any{
					"has_inquiry": false,
					// 必須行 "Files Modified: <count>" を意図的に欠落させる
					"summary": "修正完了したが Files Modified の必須行を出していない",
				},
				SessionID: "session-final-review-fix-judge",
			}, nil
		}
		return provider.AgentResponse{}, nil
	}
	onEvent, _ := collectEvents()

	got := runFinalReviewFixPhase(
		context.Background(),
		agent,
		cwd, "demo", runDir,
		3, "final-review",
		"session-skill", "impl-direction", "最終検証",
		onEvent,
	)

	if got.OK {
		t.Errorf("OK=false を期待 (Files Modified marker 未出力 = schema-violation): got=%+v", got)
	}
	if !strings.Contains(got.Message, "Files Modified") {
		t.Errorf("Message に 'Files Modified' を期待 (失敗理由の特定): got=%q", got.Message)
	}
	// JudgeSession は記録されていること (schema-violation でも sessionId は取れる)
	if got.JudgeSession != "session-final-review-fix-judge" {
		t.Errorf("JudgeSession=session-final-review-fix-judge を期待: got=%q", got.JudgeSession)
	}
}

// TestRunFinalReviewFixPhase_Turn2HasInquiryTrue は Turn 2 が has_inquiry=true を返したとき
// (judge ターンが構造化応答を破った状態) OK=false になることを固定する (spec_loop 規約と対称)。
func TestRunFinalReviewFixPhase_Turn2HasInquiryTrue(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "fix-edit":
			return finalReviewFixEditOKResp(), nil
		case "fix-judge":
			return provider.AgentResponse{
				Success: true,
				StructuredOutput: map[string]any{
					"has_inquiry": true,
					"summary":     "判定中に追加質問が出た",
					"inquiry": map[string]any{
						"title":    "追加質問",
						"question": "判定基準は何か？",
					},
				},
				SessionID: "session-final-review-fix-judge",
			}, nil
		}
		return provider.AgentResponse{}, nil
	}
	onEvent, _ := collectEvents()

	got := runFinalReviewFixPhase(
		context.Background(),
		agent,
		cwd, "demo", runDir,
		3, "final-review",
		"session-skill", "impl-direction", "最終検証",
		onEvent,
	)

	if got.OK {
		t.Errorf("OK=false を期待 (judge ターンが has_inquiry=true で構造化応答を破った): got=%+v", got)
	}
	if !strings.Contains(got.Message, "has_inquiry") {
		t.Errorf("Message に 'has_inquiry' を期待: got=%q", got.Message)
	}
}

// =============================================================================
// runFinalReviewTestVerification (Phase 4: test-edit + test-judge) のテスト
// =============================================================================
//
// design.md「runFinalReviewTestVerification」契約と Task 2.2 観測可能完了条件に従う:
//   - Turn 1 (test-edit): resume=editSession (Phase 2 fix-edit sessionId、Phase 3 fix-judge ではない、
//     cross-restriction resume を避ける), OutputFormat=nil, AllowedTools=nil,
//     StepID="final-review-test-edit", prompt=buildFinalReviewTestEditPrompt(feature)
//   - Turn 2 (test-judge): resume=Turn1.SessionID, OutputFormat=inquiryloop.Schema(),
//     AllowedTools=fixJudgeAllowedTools, StepID="final-review-test-judge",
//     prompt=buildFinalReviewTestJudgePrompt(feature)
//   - Turn 1 失敗 → Turn 2 skip + OK=false
//   - Turn 2 失敗 (agent err / success=false / schema 違反 / has_inquiry=true /
//     Test Result marker 未出力) → OK=false で Turn 1 Content を Summary に救出
//   - 成功時 (`Test Result: pass`): OK=true, TestPassed=true,
//     ChangedFiles=parseChangedFilesCount(summary), SessionID=Turn2.session
//   - `Test Result: fail` 時: OK=false, TestPassed=false (schema 適合 + テスト不通)

// finalReviewTestEditOKResp は Phase 4 Turn 1 (test-edit) が成功した場合の典型応答。
// OutputFormat=nil (markdown 自由記述) なので Content ベースで返す。
func finalReviewTestEditOKResp() provider.AgentResponse {
	return provider.AgentResponse{
		Success: true,
		Content: "全件 test を実行し pass を確認しました。\n\n## Summary\n" +
			"- 実行した test コマンド: just test\n" +
			"- iterate 回数: 0\n" +
			"- 修正したファイル: なし\n" +
			"- test 結果: pass",
		SessionID: "session-final-review-test-edit",
	}
}

// finalReviewTestJudgePassResp は Phase 4 Turn 2 (test-judge) で `Test Result: pass`
// + `Files Modified: <count>` の構造化応答を返す典型応答。
func finalReviewTestJudgePassResp(changedFiles int) provider.AgentResponse {
	summary := "全件 test pass、修正は Turn 1 内 iterate で完結\n\n" +
		"Test Result: pass\n" +
		"Files Modified: " + strconv.Itoa(changedFiles)
	return provider.AgentResponse{
		Success: true,
		StructuredOutput: map[string]any{
			"has_inquiry": false,
			"summary":     summary,
		},
		SessionID: "session-final-review-test-judge",
	}
}

// finalReviewTestJudgeFailResp は Phase 4 Turn 2 (test-judge) で `Test Result: fail`
// + `Files Modified: <count>` を返す典型応答。schema 自体は適合しているため parser は成功するが、
// 上位 (runFinalReview) では Phase 5 を skip する判定材料となる。
func finalReviewTestJudgeFailResp(changedFiles int) provider.AgentResponse {
	summary := "全件 test 不通、5 回 iterate で解消できず\n\n" +
		"Test Result: fail\n" +
		"Files Modified: " + strconv.Itoa(changedFiles)
	return provider.AgentResponse{
		Success: true,
		StructuredOutput: map[string]any{
			"has_inquiry": false,
			"summary":     summary,
		},
		SessionID: "session-final-review-test-judge",
	}
}

// TestRunFinalReviewTestVerification_BothTurnsSucceed_TestPassFiles3 は Phase 4 Turn 1/2 両方が
// 成功し、judge summary の `Test Result: pass` + `Files Modified: 3` から TestPassed=true /
// ChangedFiles=3 が抽出されることを固定する (Task 2.2 観測可能完了の主要パス)。
//
// 検証項目:
//   - finalReviewTestResult: Ran=true, OK=true, TestPassed=true, ChangedFiles=3,
//     SessionID=session-final-review-test-judge,
//     EditSession=session-final-review-test-edit, JudgeSession=session-final-review-test-judge
//   - agent.Calls[0] (test-edit): resume=editSession (= Phase 2 fix-edit sessionId),
//     OutputFormat=nil, AllowedTools=nil, StepID="final-review-test-edit",
//     prompt prefix="[final-review-test-edit]"
//   - agent.Calls[1] (test-judge): resume=session-final-review-test-edit (Turn 1 sessionId),
//     OutputFormat 非 nil (= inquiryloop.Schema()),
//     AllowedTools=fixJudgeAllowedTools (Read/Glob/Grep),
//     StepID="final-review-test-judge", prompt prefix="[final-review-test-judge]"
//   - 永続化: <runDir>/<NN>-final-review/test-result.md が書き出される
func TestRunFinalReviewTestVerification_BothTurnsSucceed_TestPassFiles3(t *testing.T) {
	const (
		feature     = "kiro-validate-impl-loop"
		order       = 7
		phase       = "final-review"
		editSession = "session-final-review-fix-edit" // Phase 2 fix-edit sessionId を Turn 1 resume の起点にする
		personaName = "impl-direction"
		promptLabel = "最終検証"
	)

	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "test-edit":
			return finalReviewTestEditOKResp(), nil
		case "test-judge":
			return finalReviewTestJudgePassResp(3), nil
		}
		t.Fatalf("予期しない agent.Call kind=%q prompt=%q", call.Kind, call.Prompt)
		return provider.AgentResponse{}, nil
	}

	onEvent, _ := collectEvents()
	got := runFinalReviewTestVerification(
		context.Background(),
		agent,
		cwd, feature, runDir,
		order, phase,
		editSession, personaName, promptLabel,
		onEvent,
	)

	// === finalReviewTestResult 検証 ===
	if !got.Ran {
		t.Errorf("Ran=true を期待: got=%+v", got)
	}
	if !got.OK {
		t.Errorf("OK=true を期待 (両ターン成功 + Test Result: pass + Files Modified: 3 抽出成功): got=%+v", got)
	}
	if !got.TestPassed {
		t.Errorf("TestPassed=true を期待 (judge summary の Test Result: pass): got=%+v", got)
	}
	if got.ChangedFiles != 3 {
		t.Errorf("ChangedFiles=3 を期待 (judge summary の Files Modified: 3 から抽出): got=%d (full=%+v)",
			got.ChangedFiles, got)
	}
	if got.SessionID != "session-final-review-test-judge" {
		t.Errorf("SessionID=session-final-review-test-judge を期待 (Turn 2 sessionId が表に出る): got=%q",
			got.SessionID)
	}
	if got.EditSession != "session-final-review-test-edit" {
		t.Errorf("EditSession=session-final-review-test-edit を期待: got=%q", got.EditSession)
	}
	if got.JudgeSession != "session-final-review-test-judge" {
		t.Errorf("JudgeSession=session-final-review-test-judge を期待: got=%q", got.JudgeSession)
	}
	if !strings.Contains(got.Summary, "全件 test pass") {
		t.Errorf("Summary は judge summary を保持すべき: got=%q", got.Summary)
	}

	// === Turn 1 (test-edit) call options 検証 ===
	if len(agent.Calls) != 2 {
		t.Fatalf("agent.Calls 件数 2 を期待 (Turn 1 + Turn 2): got=%d", len(agent.Calls))
	}
	editCall := agent.Calls[0]
	if editCall.Kind != "test-edit" {
		t.Errorf("Calls[0].Kind=test-edit を期待: got=%q", editCall.Kind)
	}
	if editCall.ResumeSessionID != editSession {
		t.Errorf("Turn 1 resume=editSession=%q を期待 (Phase 2 fix-edit sessionId): got=%q",
			editSession, editCall.ResumeSessionID)
	}
	if editCall.Options == nil {
		t.Fatalf("Turn 1 Options=nil; 必須")
	}
	if editCall.Options.OutputFormat != nil {
		t.Errorf("Turn 1 OutputFormat=nil を期待 (Bash + Edit/Write 解放、json_schema 非強制): got=%+v",
			editCall.Options.OutputFormat)
	}
	if editCall.Options.AllowedTools != nil {
		t.Errorf("Turn 1 AllowedTools=nil を期待 (SDK 既定 = 全ツール許可): got=%v",
			editCall.Options.AllowedTools)
	}
	if editCall.Options.StepID != "final-review-test-edit" {
		t.Errorf("Turn 1 StepID=final-review-test-edit を期待 (per-task 経路と区別): got=%q",
			editCall.Options.StepID)
	}
	if !strings.HasPrefix(editCall.Prompt, "[final-review-test-edit]") {
		t.Errorf("Turn 1 prompt 先頭が [final-review-test-edit] マーカで始まることを期待: got prefix=%q",
			firstLine(editCall.Prompt))
	}

	// === Turn 2 (test-judge) call options 検証 ===
	judgeCall := agent.Calls[1]
	if judgeCall.Kind != "test-judge" {
		t.Errorf("Calls[1].Kind=test-judge を期待: got=%q", judgeCall.Kind)
	}
	if judgeCall.ResumeSessionID != "session-final-review-test-edit" {
		t.Errorf("Turn 2 resume=Turn1.SessionID を期待: got=%q", judgeCall.ResumeSessionID)
	}
	if judgeCall.Options == nil {
		t.Fatalf("Turn 2 Options=nil; 必須")
	}
	if judgeCall.Options.OutputFormat == nil {
		t.Errorf("Turn 2 OutputFormat=inquiryloop.Schema() を期待 (構造化抽出強制): got=nil")
	} else if judgeCall.Options.OutputFormat.Type != "json_schema" {
		t.Errorf("Turn 2 OutputFormat.Type=json_schema を期待: got=%q", judgeCall.Options.OutputFormat.Type)
	}
	wantTools := []string{"Read", "Glob", "Grep"}
	if len(judgeCall.Options.AllowedTools) != len(wantTools) {
		t.Errorf("Turn 2 AllowedTools=fixJudgeAllowedTools を期待: got=%v want=%v",
			judgeCall.Options.AllowedTools, wantTools)
	} else {
		for i, w := range wantTools {
			if judgeCall.Options.AllowedTools[i] != w {
				t.Errorf("Turn 2 AllowedTools[%d]=%q want %q", i,
					judgeCall.Options.AllowedTools[i], w)
			}
		}
	}
	if judgeCall.Options.StepID != "final-review-test-judge" {
		t.Errorf("Turn 2 StepID=final-review-test-judge を期待: got=%q", judgeCall.Options.StepID)
	}
	if !strings.HasPrefix(judgeCall.Prompt, "[final-review-test-judge]") {
		t.Errorf("Turn 2 prompt 先頭が [final-review-test-judge] マーカで始まることを期待: got prefix=%q",
			firstLine(judgeCall.Prompt))
	}

	// === 永続化 test-result.md 検証 ===
	expected := filepath.Join(runDir, "07-final-review", "test-result.md")
	body, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("test-result.md が書き出されていない: %s err=%v", expected, err)
	}
	bodyStr := string(body)
	if !strings.Contains(bodyStr, "ok: true") {
		t.Errorf("test-result.md に 'ok: true' を期待: %q", bodyStr)
	}
	if !strings.Contains(bodyStr, "Test Result: pass") {
		t.Errorf("test-result.md に 'Test Result: pass' を期待: %q", bodyStr)
	}
	if !strings.Contains(bodyStr, "Files Modified: 3") {
		t.Errorf("test-result.md に 'Files Modified: 3' を期待: %q", bodyStr)
	}
	if !strings.Contains(bodyStr, "session-final-review-test-judge") {
		t.Errorf("test-result.md に Turn 2 sessionId を期待: %q", bodyStr)
	}
}

// TestRunFinalReviewTestVerification_TestResultFail_OKFalse は judge summary の
// `Test Result: fail` で TestPassed=false / OK=false になることを固定する。
// schema 自体は適合する (Test Result マーカは存在する) が、テスト失敗を上位に伝える経路。
func TestRunFinalReviewTestVerification_TestResultFail_OKFalse(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "test-edit":
			return finalReviewTestEditOKResp(), nil
		case "test-judge":
			return finalReviewTestJudgeFailResp(2), nil
		}
		return provider.AgentResponse{}, nil
	}
	onEvent, _ := collectEvents()

	got := runFinalReviewTestVerification(
		context.Background(),
		agent,
		cwd, "demo", runDir,
		3, "final-review",
		"session-fix-edit", "impl-direction", "最終検証",
		onEvent,
	)

	if got.OK {
		t.Errorf("OK=false を期待 (Test Result: fail): got=%+v", got)
	}
	if got.TestPassed {
		t.Errorf("TestPassed=false を期待 (Test Result: fail): got=%+v", got)
	}
	if got.ChangedFiles != 2 {
		t.Errorf("ChangedFiles=2 を期待 (Files Modified: 2 から抽出、fail でも抽出は通る): got=%d", got.ChangedFiles)
	}
	if !strings.Contains(got.Summary, "Test Result: fail") {
		t.Errorf("Summary は judge summary を保持すべき (Test Result: fail を含む): got=%q", got.Summary)
	}
	// JudgeSession は記録される (schema 適合のため)
	if got.JudgeSession != "session-final-review-test-judge" {
		t.Errorf("JudgeSession=session-final-review-test-judge を期待: got=%q", got.JudgeSession)
	}
}

// TestRunFinalReviewTestVerification_NoTestResultMarker_SchemaViolation は Turn 2 が
// `Test Result:` 行を含まない summary を返したとき、schema-violation 扱いで OK=false に
// なることを固定する (Req 3.5 / parseTestResult 契約)。
func TestRunFinalReviewTestVerification_NoTestResultMarker_SchemaViolation(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "test-edit":
			return finalReviewTestEditOKResp(), nil
		case "test-judge":
			return provider.AgentResponse{
				Success: true,
				StructuredOutput: map[string]any{
					"has_inquiry": false,
					// 必須行 "Test Result: <pass|fail>" を意図的に欠落させる (Files Modified だけ書く)
					"summary": "test 実行は完了したが Test Result の必須行を出していない\n\nFiles Modified: 1",
				},
				SessionID: "session-final-review-test-judge",
			}, nil
		}
		return provider.AgentResponse{}, nil
	}
	onEvent, _ := collectEvents()

	got := runFinalReviewTestVerification(
		context.Background(),
		agent,
		cwd, "demo", runDir,
		3, "final-review",
		"session-fix-edit", "impl-direction", "最終検証",
		onEvent,
	)

	if got.OK {
		t.Errorf("OK=false を期待 (Test Result marker 未出力 = schema-violation): got=%+v", got)
	}
	if got.TestPassed {
		t.Errorf("TestPassed=false を期待 (schema-violation): got=%+v", got)
	}
	if !strings.Contains(got.Message, "Test Result") {
		t.Errorf("Message に 'Test Result' を期待 (失敗理由の特定): got=%q", got.Message)
	}
	// JudgeSession は記録されていること (schema-violation でも sessionId は取れる)
	if got.JudgeSession != "session-final-review-test-judge" {
		t.Errorf("JudgeSession=session-final-review-test-judge を期待: got=%q", got.JudgeSession)
	}
}

// TestRunFinalReviewTestVerification_Turn1AgentError_SkipsTurn2 は Turn 1 が agent error を返したとき
// Turn 2 が呼ばれず OK=false で return することを固定する。
func TestRunFinalReviewTestVerification_Turn1AgentError_SkipsTurn2(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "test-edit":
			return provider.AgentResponse{}, errors.New("simulated final-review test-edit agent error")
		case "test-judge":
			t.Errorf("Turn 1 が agent error の場合 Turn 2 (test-judge) は呼ばれてはならない: prompt=%q", call.Prompt)
			return provider.AgentResponse{}, nil
		}
		return provider.AgentResponse{}, nil
	}
	onEvent, _ := collectEvents()

	got := runFinalReviewTestVerification(
		context.Background(),
		agent,
		cwd, "demo", runDir,
		3, "final-review",
		"session-fix-edit", "impl-direction", "最終検証",
		onEvent,
	)

	if !got.Ran {
		t.Errorf("Ran=true を期待 (helper を呼んだ事実): got=%+v", got)
	}
	if got.OK {
		t.Errorf("OK=false を期待 (Turn 1 agent error): got=%+v", got)
	}
	if got.JudgeSession != "" {
		t.Errorf("Turn 1 失敗時 JudgeSession は空であるべき: got=%q", got.JudgeSession)
	}
	if !strings.Contains(got.Message, "agent error") {
		t.Errorf("Message に 'agent error' を期待: got=%q", got.Message)
	}
	// test-judge が呼ばれていないこと
	if cnt := countCallsByKind(agent.Calls, "test-judge"); cnt != 0 {
		t.Errorf("Turn 1 失敗で test-judge は 0 件であるべき: got=%d", cnt)
	}
}

// TestRunFinalReviewTestVerification_Turn1SuccessFalse_SkipsTurn2 は Turn 1 が success=false を
// 返したとき Turn 2 が呼ばれず OK=false で return し、EditSession が記録されることを固定する。
func TestRunFinalReviewTestVerification_Turn1SuccessFalse_SkipsTurn2(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "test-edit":
			return provider.AgentResponse{
				Success:   false,
				Content:   "Bash が allowed-tools で blocked された (試験用)",
				SessionID: "session-final-review-test-edit-failed",
			}, nil
		case "test-judge":
			t.Errorf("Turn 1 success=false 時 Turn 2 は呼ばれてはならない")
			return provider.AgentResponse{}, nil
		}
		return provider.AgentResponse{}, nil
	}
	onEvent, _ := collectEvents()

	got := runFinalReviewTestVerification(
		context.Background(),
		agent,
		cwd, "demo", runDir,
		3, "final-review",
		"session-fix-edit", "impl-direction", "最終検証",
		onEvent,
	)

	if got.OK {
		t.Errorf("OK=false を期待 (Turn 1 success=false): got=%+v", got)
	}
	if got.EditSession != "session-final-review-test-edit-failed" {
		t.Errorf("EditSession=session-final-review-test-edit-failed を期待 (Turn 1 sessionId 保持): got=%q",
			got.EditSession)
	}
	if got.SessionID != "session-final-review-test-edit-failed" {
		t.Errorf("SessionID は Turn 1 sessionId をフォールバックとして保持すべき: got=%q", got.SessionID)
	}
	if got.JudgeSession != "" {
		t.Errorf("Turn 1 失敗時 JudgeSession は空: got=%q", got.JudgeSession)
	}
}

// TestRunFinalReviewTestVerification_Turn2AgentError_RecoversTurn1Content は Turn 2 (test-judge) が
// agent error で失敗したとき、Turn 1 (test-edit) の Content (markdown) が Summary に救出される
// ことを固定する (best-effort 規約)。
func TestRunFinalReviewTestVerification_Turn2AgentError_RecoversTurn1Content(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()

	const editContent = "TURN1_TEST_MARKER: Phase 4 Turn 1 で全件 test pass を確認しました"
	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "test-edit":
			return provider.AgentResponse{
				Success:   true,
				Content:   editContent,
				SessionID: "session-final-review-test-edit",
			}, nil
		case "test-judge":
			return provider.AgentResponse{}, errors.New("simulated final-review test-judge agent error")
		}
		return provider.AgentResponse{}, nil
	}
	onEvent, _ := collectEvents()

	got := runFinalReviewTestVerification(
		context.Background(),
		agent,
		cwd, "demo", runDir,
		3, "final-review",
		"session-fix-edit", "impl-direction", "最終検証",
		onEvent,
	)

	if got.OK {
		t.Errorf("OK=false を期待 (Turn 2 agent error): got=%+v", got)
	}
	if got.EditSession != "session-final-review-test-edit" {
		t.Errorf("EditSession=session-final-review-test-edit を期待: got=%q", got.EditSession)
	}
	if got.JudgeSession != "" {
		t.Errorf("Turn 2 agent error 時 JudgeSession は空: got=%q", got.JudgeSession)
	}
	if !strings.Contains(got.Summary, "TURN1_TEST_MARKER") {
		t.Errorf("Turn 2 失敗時 Summary に Turn 1 Content を救出するべき: got=%q", got.Summary)
	}
	if !strings.Contains(got.Message, "judge") {
		t.Errorf("Message に 'judge' を期待 (失敗フェーズ識別): got=%q", got.Message)
	}
}

// TestRunFinalReviewTestVerification_Turn2HasInquiryTrue は Turn 2 が has_inquiry=true を返したとき
// (judge ターンが構造化応答を破った状態) OK=false になることを固定する (spec_loop 規約と対称)。
func TestRunFinalReviewTestVerification_Turn2HasInquiryTrue(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "test-edit":
			return finalReviewTestEditOKResp(), nil
		case "test-judge":
			return provider.AgentResponse{
				Success: true,
				StructuredOutput: map[string]any{
					"has_inquiry": true,
					"summary":     "判定中に追加質問が出た",
					"inquiry": map[string]any{
						"title":      "追加質問",
						"background": "test 結果の解釈に判断必要",
						"question":   "失敗 test を許容するか？",
					},
				},
				SessionID: "session-final-review-test-judge",
			}, nil
		}
		return provider.AgentResponse{}, nil
	}
	onEvent, _ := collectEvents()

	got := runFinalReviewTestVerification(
		context.Background(),
		agent,
		cwd, "demo", runDir,
		3, "final-review",
		"session-fix-edit", "impl-direction", "最終検証",
		onEvent,
	)

	if got.OK {
		t.Errorf("OK=false を期待 (judge ターンが has_inquiry=true で構造化応答を破った): got=%+v", got)
	}
	if !strings.Contains(got.Message, "has_inquiry") {
		t.Errorf("Message に 'has_inquiry' を期待: got=%q", got.Message)
	}
}

// =============================================================================
// Phase 5 Service Helper (runFinalReviewCommit) - Task 2.3
// =============================================================================
//
// Phase 5 (commit) は per-task 経路 (impl_loop.go:279) と完全同形式で `/git-commit` を
// fresh session 起動するだけの薄い helper。
//
// 契約 (design.md「runFinalReviewCommit」/ tasks.md 2.3):
//   - prompt        = "/git-commit"
//   - resume        = ""                                    (fresh session 強制、Req 4.1 / 4.2)
//   - StepID        = "commit-final-review"                 (akashic フィルタ用、Req 5.4)
//   - StepName      = "最終検証 commit"
//   - OutputFormat  = nil                                   (`/git-commit` 既存挙動に委譲)
//   - AllowedTools  = nil                                   (per-task 経路と一致)
//   - onEvent forward: workflow.ForwardEvents(onEvent)      (per-task 経路と一致)
//
// 戻り値: agent.CallAgent の返り値をそのまま伝播 (本 helper は判定をしない)。
//   - success=true → response を素通しで返却
//   - success=false → AgentResponse.Success=false を素通し (上位 runFinalReview が WorkflowError 発火)
//   - agent error → error を素通し (同上)

// TestRunFinalReviewCommit_Success_RecordsStepIDAndFreshSession は Phase 5 の最低限の
// 観測可能完了 (tasks.md 2.3) を固定する。
//
//   - fakeAgent fixture で /git-commit 起動時に
//     prompt = "/git-commit"、resume = ""、StepID = "commit-final-review"、
//     StepName = "最終検証 commit" が記録される
//   - fakeAgent が success=true を返したとき、その response が呼び出し側に素通しで返却される
//   - 呼び出し側に error は伝播しない (err == nil)
func TestRunFinalReviewCommit_Success_RecordsStepIDAndFreshSession(t *testing.T) {
	cwd := t.TempDir()
	agent := &fakeAgent{
		resp: provider.AgentResponse{
			Content:   "committed",
			Success:   true,
			SessionID: "session-final-review-commit",
		},
	}

	onEvent, eventsPtr := collectEvents()
	resp, err := runFinalReviewCommit(context.Background(), agent, cwd, onEvent)

	// === 戻り値検証 ===
	if err != nil {
		t.Fatalf("err=nil を期待 (success=true 経路): got=%v", err)
	}
	if !resp.Success {
		t.Errorf("resp.Success=true を期待 (fakeAgent.resp の素通し): got=%+v", resp)
	}
	if resp.Content != "committed" {
		t.Errorf("resp.Content=committed を期待 (fakeAgent.resp の素通し): got=%q", resp.Content)
	}
	if resp.SessionID != "session-final-review-commit" {
		t.Errorf("resp.SessionID=session-final-review-commit を期待: got=%q", resp.SessionID)
	}

	// === fakeAgent への投入値検証 (per-task /git-commit と完全同形式) ===
	if len(agent.prompts) != 1 {
		t.Fatalf("agent.prompts 件数 1 を期待: got=%d (%v)", len(agent.prompts), agent.prompts)
	}
	if agent.prompts[0] != "/git-commit" {
		t.Errorf("prompt=/git-commit を期待 (per-task 経路 impl_loop.go:279 と完全同形式): got=%q",
			agent.prompts[0])
	}
	if len(agent.options) != 1 {
		t.Fatalf("agent.options 件数 1 を期待: got=%d", len(agent.options))
	}
	opts := agent.options[0]
	if opts == nil {
		t.Fatalf("Options=nil; WithStep が *CallAgentOptions を返すはず")
	}
	if opts.StepID != "commit-final-review" {
		t.Errorf("StepID=commit-final-review を期待 (akashic フィルタ用、Req 5.4): got=%q", opts.StepID)
	}
	if opts.StepName != "最終検証 commit" {
		t.Errorf("StepName=最終検証 commit を期待: got=%q", opts.StepName)
	}
	assertGitCommitProviderOverrides(t, opts)
	if opts.OutputFormat != nil {
		t.Errorf("OutputFormat=nil を期待 (per-task 経路と一致): got=%+v", opts.OutputFormat)
	}
	if opts.AllowedTools != nil {
		t.Errorf("AllowedTools=nil を期待 (per-task 経路と一致): got=%v", opts.AllowedTools)
	}

	// === onEvent forward 検証 (workflow.ForwardEvents の通過確認) ===
	// fakeAgent は CallAgent 内で onEvent(EventText{"stream"}) を 1 件発火する。
	// runFinalReviewCommit は workflow.ForwardEvents で adapter を被せて渡すので、
	// ここで WorkflowText イベントが 1 件届くはず。
	if len(*eventsPtr) != 1 {
		t.Errorf("WorkflowText イベント 1 件を期待 (ForwardEvents 通過): got %d 件 (%+v)",
			len(*eventsPtr), *eventsPtr)
	} else if (*eventsPtr)[0].Type != workflow.WorkflowText {
		t.Errorf("Type=WorkflowText を期待: got=%v", (*eventsPtr)[0].Type)
	}
}

// TestRunFinalReviewCommit_FreshSessionResumeEmpty は resume が空文字列で起動されることを
// 別途明示的に検証する (Req 4.2: per-task commit 列の後ろに新規 commit、final-review session 群と分離)。
//
// scriptedAgent を使って ResumeSessionID を直接観測する (fakeAgent は resume を保存しないため)。
func TestRunFinalReviewCommit_FreshSessionResumeEmpty(t *testing.T) {
	cwd := t.TempDir()

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		if call.Kind != "commit" {
			t.Errorf("Calls[0].Kind=commit を期待 (prompt=/git-commit): got kind=%q prompt=%q",
				call.Kind, call.Prompt)
		}
		return provider.AgentResponse{Success: true, SessionID: "s-commit"}, nil
	}

	onEvent, _ := collectEvents()
	_, err := runFinalReviewCommit(context.Background(), agent, cwd, onEvent)
	if err != nil {
		t.Fatalf("err=nil を期待: got=%v", err)
	}

	if len(agent.Calls) != 1 {
		t.Fatalf("agent.Calls 件数 1 を期待: got=%d", len(agent.Calls))
	}
	c := agent.Calls[0]
	if c.ResumeSessionID != "" {
		t.Errorf("resume=\"\" を期待 (fresh session 強制、final-review session 群と分離): got=%q",
			c.ResumeSessionID)
	}
	if c.CWD != cwd {
		t.Errorf("CWD=%q を期待: got=%q", cwd, c.CWD)
	}
}

// TestRunFinalReviewCommit_AgentError_Propagates は agent error が呼び出し側に
// 素通しで伝播することを固定する (本 helper は判定をしない、design.md「runFinalReviewCommit」)。
func TestRunFinalReviewCommit_AgentError_Propagates(t *testing.T) {
	cwd := t.TempDir()

	wantErr := errors.New("simulated /git-commit agent error")
	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		return provider.AgentResponse{}, wantErr
	}

	onEvent, _ := collectEvents()
	resp, err := runFinalReviewCommit(context.Background(), agent, cwd, onEvent)

	if err == nil {
		t.Fatalf("err 非 nil を期待 (agent error 素通し): got resp=%+v", resp)
	}
	if !errors.Is(err, wantErr) && err.Error() != wantErr.Error() {
		t.Errorf("agent error が素通しされることを期待 (wrap せず原 error を伝播): got=%v want=%v",
			err, wantErr)
	}
}

// TestRunFinalReviewCommit_SuccessFalse_Propagates は agent が success=false を返したとき、
// AgentResponse.Success=false が呼び出し側に素通しで伝播することを固定する
// (本 helper は判定をしない、上位 runFinalReview が WorkflowError 発火する規約)。
func TestRunFinalReviewCommit_SuccessFalse_Propagates(t *testing.T) {
	cwd := t.TempDir()

	agent := &fakeAgent{
		resp: provider.AgentResponse{
			Content:   "commit hook failed",
			Success:   false,
			SessionID: "session-failed",
		},
	}

	onEvent, _ := collectEvents()
	resp, err := runFinalReviewCommit(context.Background(), agent, cwd, onEvent)

	if err != nil {
		t.Errorf("err=nil を期待 (本 helper は success=false を error にしない): got=%v", err)
	}
	if resp.Success {
		t.Errorf("resp.Success=false を期待 (素通し): got=%+v", resp)
	}
	if resp.Content != "commit hook failed" {
		t.Errorf("resp.Content の素通しを期待: got=%q", resp.Content)
	}
}

// =============================================================================
// runFinalReview (Phase 1-5 5-Phase Sequential Pipeline) のテスト (Task 3.1)
// =============================================================================
//
// design.md「runFinalReview」「System Flows」「Stop Policy by Failure Region」契約と
// Task 3.1 観測可能完了条件に従う:
//   - Phase 1 (inquiry loop): inquiryloop.Engine.RunPrompt で /kiro-validate-impl を MaxRounds=15 で駆動
//   - Phase 2-3 (fix-edit + fix-judge): runFinalReviewFixPhase
//   - Phase 4 (test-edit + test-judge): runFinalReviewTestVerification
//   - Phase 5 (commit): runFinalReviewCommit
//
// 各 phase の正常完了 → WorkflowDone 1 件 / 失敗 → WorkflowError 1 件 + run 停止 (err=nil で
// return)。permanent な I/O 致命エラーのみ err 経由で propagate する。

// testImplPersona は runFinalReview テストで使う最小限の impl-direction persona。
// inquiryloop.Engine が buildAnswerPrompt 内で `# Persona` セクションに Content を埋め込むため
// 非空の Content を持たせる必要がある (空 Content は engine の前提崩しになる)。
func testImplPersona() inquiryloop.AnswerPersona {
	return inquiryloop.AnswerPersona{
		Name:       "impl-direction",
		SourcePath: "test:impl-direction",
		Content:    "テスト用 impl-direction persona の最小内容",
	}
}

// TestRunFinalReviewUsesSuppliedAnswerPersonaWithoutResolver は Task 3.3 の Red:
// final-review Phase 1 は impl-loop で解決済みの AnswerPersona をそのまま消費し、
// final-review 内で Persona resolver を再実行しない。cwd に facet override がなくても、
// 引数で渡した content が answerer prompt に出ることを観測する。
func TestRunFinalReviewUsesSuppliedAnswerPersonaWithoutResolver(t *testing.T) {
	const (
		feature         = "demo"
		order           = 6
		suppliedContent = "# supplied final-review impl-direction persona\nonly this supplied persona should appear.\n"
		suppliedSource  = "fixture:pre-resolved/impl-direction-answerer.md"
	)

	cwd := t.TempDir()
	runDir := t.TempDir()
	implPersona := inquiryloop.AnswerPersona{
		Name:       implPersonaName,
		SourcePath: suppliedSource,
		Content:    suppliedContent,
	}

	var finalAnswererPrompt string
	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "skill":
			skillCount := countCallsByKind(agent.Calls, "skill")
			if skillCount == 1 {
				return skillInquiryResp(), nil
			}
			return skillNoInquiryResp(), nil
		case "answerer":
			finalAnswererPrompt = call.Prompt
			return answerOKResp(), nil
		case "fix-edit":
			return finalReviewFixEditOKResp(), nil
		case "fix-judge":
			return finalReviewFixJudgeOKResp(0), nil
		case "test-edit", "test-judge", "commit":
			t.Fatalf("Files Modified: 0 のとき Phase 4/5 (%s) は呼ばれてはならない", call.Kind)
		}
		t.Fatalf("予期しない agent.Call kind=%q prompt=%q", call.Kind, firstLine(call.Prompt))
		return provider.AgentResponse{}, nil
	}

	onEvent, _ := collectEvents()
	err := runFinalReview(
		context.Background(),
		agent,
		cwd, runDir, feature,
		order,
		onEvent,
		implPersona,
	)
	if err != nil {
		t.Fatalf("err=nil を期待: got=%v", err)
	}

	if finalAnswererPrompt == "" {
		t.Fatalf("final-review answerer prompt が観測されていない: calls=%+v", agent.Calls)
	}
	if !strings.Contains(finalAnswererPrompt, suppliedContent) {
		t.Fatalf("final-review answerer prompt は引数の AnswerPersona.Content を含むべき:\n%s", finalAnswererPrompt)
	}
	if strings.Contains(finalAnswererPrompt, "# Persona: Implementation Direction Setter") {
		t.Fatalf("final-review が built-in resolver を再実行した可能性がある。supplied content 以外の built-in marker が混入:\n%s", finalAnswererPrompt)
	}
	if strings.Contains(finalAnswererPrompt, suppliedSource) {
		t.Fatalf("final-review answerer prompt は AnswerPersona.SourcePath を LLM-visible prompt に漏らしてはならない:\n%s", finalAnswererPrompt)
	}

	answerPath := filepath.Join(runDir, "06-final-review", "round-1-answer.md")
	answerBody, readErr := os.ReadFile(answerPath)
	if readErr != nil {
		t.Fatalf("round-1-answer.md が永続化されていない: %s err=%v", answerPath, readErr)
	}
	answerText := string(answerBody)
	if !strings.Contains(answerText, "- persona: "+implPersonaName) {
		t.Fatalf("final-review answer artifact は logical persona name %q を保持すべき:\n%s", implPersonaName, answerText)
	}
	if strings.Contains(answerText, suppliedSource) {
		t.Fatalf("final-review answer artifact の persona metadata に source path を混入させてはならない:\n%s", answerText)
	}
}

func TestRunFinalReviewAnswerPolicyWhitespaceOnlyOverrideEmitsWorkflowError(t *testing.T) {
	const (
		feature = "demo"
		order   = 6
	)
	cwd := t.TempDir()
	runDir := t.TempDir()
	writeFile(t, filepath.Join(cwd, ".rilll-config", "facets", "policies", implAnswerPolicyName+".md"), " \n\t\n")

	agent := &scriptedAgent{}
	onEvent, eventsPtr := collectEvents()
	err := runFinalReview(
		context.Background(),
		agent,
		cwd, runDir, feature,
		order,
		onEvent,
		testImplPersona(),
	)
	if err != nil {
		t.Fatalf("expected policy resolution abort to return nil error, got %v", err)
	}
	if len(agent.Calls) != 0 {
		t.Fatalf("empty answer policy must abort before final-review agent call: got=%d calls", len(agent.Calls))
	}
	if !hasErrorEvent(*eventsPtr, implAnswerPolicyName) {
		t.Fatalf("WorkflowError event should mention policy %q: %+v", implAnswerPolicyName, *eventsPtr)
	}
}

// TestRunFinalReview_FullPath_AllFiveArtifactsPersisted は Task 3.1 の観測可能完了条件を固定する:
//
//	scriptedAgent + fakeAgent fixture で Phase 1-5 を一巡するシナリオを実行すると
//	runFinalReview が err==nil で終了し、WorkflowDone が 1 回発火、
//	<runDir>/<NN>-final-review/{round-1-inquiry.md, round-1-answer.md, fix-result.md, test-result.md}
//	の 4 artifact が永続化される。
//
// シナリオ (8 件の agent.CallAgent):
//  1. final-review-skill-r0 (validate-impl initial) → has_inquiry=true (1 round 開始)
//  2. final-review-answer-r1 (impl-direction persona answer) → has_inquiry=false (回答テキスト)
//  3. final-review-skill-r1 (validate-impl resume) → has_inquiry=false (round 1 完走)
//  4. final-review-fix-edit (Phase 2) → markdown summary
//  5. final-review-fix-judge (Phase 3) → schema, summary に "Files Modified: 3"
//  6. final-review-test-edit (Phase 4 Turn 1) → markdown summary
//  7. final-review-test-judge (Phase 4 Turn 2) → schema, summary に "Test Result: pass" + "Files Modified: 3"
//  8. commit-final-review (Phase 5) → success=true
func TestRunFinalReview_FullPath_AllFiveArtifactsPersisted(t *testing.T) {
	const (
		feature = "demo"
		order   = 7
	)

	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "skill":
			// 1 回目 (initial) → has_inquiry=true、2 回目以降 (resume) → has_inquiry=false。
			// Calls にこの呼び出しは既に append 済み (scriptedAgent.CallAgent 仕様)。
			skillCount := countCallsByKind(agent.Calls, "skill")
			if skillCount == 1 {
				return skillInquiryResp(), nil
			}
			return skillNoInquiryResp(), nil
		case "answerer":
			return answerOKResp(), nil
		case "fix-edit":
			return finalReviewFixEditOKResp(), nil
		case "fix-judge":
			return finalReviewFixJudgeOKResp(3), nil
		case "test-edit":
			return finalReviewTestEditOKResp(), nil
		case "test-judge":
			return finalReviewTestJudgePassResp(3), nil
		case "commit":
			return commitOKResp(), nil
		}
		t.Fatalf("予期しない agent.Call kind=%q prompt=%q", call.Kind, firstLine(call.Prompt))
		return provider.AgentResponse{}, nil
	}

	onEvent, eventsPtr := collectEvents()
	err := runFinalReview(
		context.Background(),
		agent,
		cwd, runDir, feature,
		order,
		onEvent,
		testImplPersona(),
	)

	// === 戻り値検証 ===
	if err != nil {
		t.Fatalf("err=nil を期待 (Phase 1-5 正常完走): got=%v", err)
	}

	// === agent.Calls 件数 + Kind 順序検証 ===
	wantKinds := []string{
		"skill",      // 1: validate-impl initial (has_inquiry=true)
		"answerer",   // 2: impl-direction persona answer (round 1)
		"skill",      // 3: validate-impl resume (round 1 完走 has_inquiry=false)
		"fix-edit",   // 4: Phase 2 fix-edit
		"fix-judge",  // 5: Phase 3 fix-judge (Files Modified: 3)
		"test-edit",  // 6: Phase 4 Turn 1 test-edit
		"test-judge", // 7: Phase 4 Turn 2 test-judge (Test Result: pass)
		"commit",     // 8: Phase 5 commit-final-review
	}
	if len(agent.Calls) != len(wantKinds) {
		t.Fatalf("agent.Calls 件数 %d を期待 (Phase 1-5 一巡): got=%d", len(wantKinds), len(agent.Calls))
	}
	for i, want := range wantKinds {
		if agent.Calls[i].Kind != want {
			t.Errorf("Calls[%d].Kind=%q を期待: got=%q (prompt=%q)", i, want, agent.Calls[i].Kind,
				firstLine(agent.Calls[i].Prompt))
		}
	}

	// === StepID 検証 (akashic フィルタ用、Req 5.4) ===
	wantStepIDs := []string{
		"final-review-skill-r0",
		"final-review-answer-r1",
		"final-review-skill-r1",
		"final-review-fix-edit",
		"final-review-fix-judge",
		"final-review-test-edit",
		"final-review-test-judge",
		"commit-final-review",
	}
	for i, want := range wantStepIDs {
		if agent.Calls[i].Options == nil {
			t.Errorf("Calls[%d].Options=nil; StepID 観測不可", i)
			continue
		}
		if agent.Calls[i].Options.StepID != want {
			t.Errorf("Calls[%d].Options.StepID=%q を期待: got=%q",
				i, want, agent.Calls[i].Options.StepID)
		}
	}

	// === WorkflowDone 1 件発火 / WorkflowError 0 件 ===
	doneCount := 0
	errCount := 0
	for _, e := range *eventsPtr {
		switch e.Type {
		case workflow.WorkflowDone:
			doneCount++
		case workflow.WorkflowError:
			errCount++
		}
	}
	if doneCount != 1 {
		t.Errorf("WorkflowDone 1 件発火を期待: got=%d (events=%+v)", doneCount, *eventsPtr)
	}
	if errCount != 0 {
		t.Errorf("WorkflowError 0 件を期待 (正常完走): got=%d", errCount)
	}

	// === 4 artifact 永続化検証 ===
	stepDir := filepath.Join(runDir, "07-final-review")
	wantFiles := []string{
		"round-1-inquiry.md",
		"round-1-answer.md",
		"fix-result.md",
		"test-result.md",
	}
	for _, f := range wantFiles {
		path := filepath.Join(stepDir, f)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("artifact %s が永続化されていない: %v", path, err)
		}
	}

	// === fix-result.md / test-result.md の内容検証 (sanity check) ===
	fixBody, err := os.ReadFile(filepath.Join(stepDir, "fix-result.md"))
	if err != nil {
		t.Fatalf("fix-result.md read: %v", err)
	}
	if !strings.Contains(string(fixBody), "ok: true") {
		t.Errorf("fix-result.md に 'ok: true' を期待: %q", string(fixBody))
	}

	testBody, err := os.ReadFile(filepath.Join(stepDir, "test-result.md"))
	if err != nil {
		t.Fatalf("test-result.md read: %v", err)
	}
	if !strings.Contains(string(testBody), "Test Result: pass") {
		t.Errorf("test-result.md に 'Test Result: pass' を期待: %q", string(testBody))
	}
	if !strings.Contains(string(testBody), "Files Modified: 3") {
		t.Errorf("test-result.md に 'Files Modified: 3' を期待: %q", string(testBody))
	}
}

// TestRunFinalReview_Rounds0_ImmediateDone_NoFixNoTestNoCommit は Phase 1 で初回応答が
// has_inquiry=false (rounds=0) のとき、Phase 2-5 が完全 skip され WorkflowDone が即発火する
// ことを固定する (Req 1.3)。
//
// 期待 agent.Calls 順: final-review-skill-r0 (validate-impl initial) のみ。
// 期待永続化: <runDir>/<NN>-final-review/ ディレクトリが作成されない (round 0 では onRound 未発火)、
//
//	fix-result.md / test-result.md も書かれない。
func TestRunFinalReview_Rounds0_ImmediateDone_NoFixNoTestNoCommit(t *testing.T) {
	const (
		feature = "demo"
		order   = 3
	)

	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "skill":
			// 初回応答で has_inquiry=false → Phase 1 が rounds=0 で完走。
			return skillNoInquiryResp(), nil
		}
		t.Fatalf("予期しない agent.Call kind=%q (rounds=0 では skill 1 件のみのはず)", call.Kind)
		return provider.AgentResponse{}, nil
	}

	onEvent, eventsPtr := collectEvents()
	err := runFinalReview(
		context.Background(),
		agent,
		cwd, runDir, feature,
		order,
		onEvent,
		testImplPersona(),
	)
	if err != nil {
		t.Fatalf("err=nil を期待: got=%v", err)
	}

	// === agent.Calls 件数 = 1 (validate-impl initial のみ) ===
	if len(agent.Calls) != 1 {
		t.Fatalf("agent.Calls 件数 1 を期待 (rounds=0 即完走): got=%d (%+v)",
			len(agent.Calls), agent.Calls)
	}
	if agent.Calls[0].Kind != "skill" {
		t.Errorf("Calls[0].Kind=skill を期待: got=%q", agent.Calls[0].Kind)
	}

	// === WorkflowDone 1 件 / WorkflowError 0 件 ===
	doneCount := 0
	errCount := 0
	for _, e := range *eventsPtr {
		switch e.Type {
		case workflow.WorkflowDone:
			doneCount++
		case workflow.WorkflowError:
			errCount++
		}
	}
	if doneCount != 1 {
		t.Errorf("WorkflowDone 1 件発火を期待: got=%d", doneCount)
	}
	if errCount != 0 {
		t.Errorf("WorkflowError 0 件を期待: got=%d", errCount)
	}

	// === fix-result.md / test-result.md は不在 (Phase 2-5 skip) ===
	stepDir := filepath.Join(runDir, "03-final-review")
	if _, err := os.Stat(filepath.Join(stepDir, "fix-result.md")); !os.IsNotExist(err) {
		t.Errorf("fix-result.md は rounds=0 即完走時に作成されないはず: stat=%v", err)
	}
	if _, err := os.Stat(filepath.Join(stepDir, "test-result.md")); !os.IsNotExist(err) {
		t.Errorf("test-result.md は rounds=0 即完走時に作成されないはず: stat=%v", err)
	}
}

// TestRunFinalReview_FilesModified0_SkipsPhase4And5 は Phase 3 (fix-judge) summary が
// "Files Modified: 0" を返したとき、Phase 4 (test verification) と Phase 5 (commit) を skip
// して WorkflowDone が発火することを固定する (Req 2.5 / 3.6: 修正なし許容)。
//
// 期待 agent.Calls 順:
//
//	final-review-skill-r0 → final-review-answer-r1 → final-review-skill-r1 →
//	final-review-fix-edit → final-review-fix-judge (5 件)
//
// 期待: test-result.md 不在、commit 系 event 不在、fix-result.md 存在。
func TestRunFinalReview_FilesModified0_SkipsPhase4And5(t *testing.T) {
	const (
		feature = "demo"
		order   = 5
	)

	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "skill":
			skillCount := countCallsByKind(agent.Calls, "skill")
			if skillCount == 1 {
				return skillInquiryResp(), nil
			}
			return skillNoInquiryResp(), nil
		case "answerer":
			return answerOKResp(), nil
		case "fix-edit":
			return finalReviewFixEditOKResp(), nil
		case "fix-judge":
			// Files Modified: 0 → Phase 4-5 skip 経路。
			return finalReviewFixJudgeOKResp(0), nil
		case "test-edit", "test-judge", "commit":
			t.Errorf("Phase 3 で Files Modified: 0 のとき Phase 4/5 (%s) は呼ばれてはならない", call.Kind)
			return provider.AgentResponse{}, nil
		}
		t.Fatalf("予期しない agent.Call kind=%q", call.Kind)
		return provider.AgentResponse{}, nil
	}

	onEvent, eventsPtr := collectEvents()
	err := runFinalReview(
		context.Background(),
		agent,
		cwd, runDir, feature,
		order,
		onEvent,
		testImplPersona(),
	)
	if err != nil {
		t.Fatalf("err=nil を期待: got=%v", err)
	}

	// === agent.Calls = 5 件 (Phase 1 inquiry round 3 件 + Phase 2-3 fix 2 件) ===
	if len(agent.Calls) != 5 {
		t.Fatalf("agent.Calls 件数 5 を期待: got=%d", len(agent.Calls))
	}
	if cnt := countCallsByKind(agent.Calls, "test-edit"); cnt != 0 {
		t.Errorf("test-edit 0 件を期待 (Phase 4 skip): got=%d", cnt)
	}
	if cnt := countCallsByKind(agent.Calls, "commit"); cnt != 0 {
		t.Errorf("commit 0 件を期待 (Phase 5 skip): got=%d", cnt)
	}

	// === WorkflowDone 1 件 / WorkflowError 0 件 ===
	doneCount := 0
	errCount := 0
	for _, e := range *eventsPtr {
		switch e.Type {
		case workflow.WorkflowDone:
			doneCount++
		case workflow.WorkflowError:
			errCount++
		}
	}
	if doneCount != 1 {
		t.Errorf("WorkflowDone 1 件発火を期待: got=%d", doneCount)
	}
	if errCount != 0 {
		t.Errorf("WorkflowError 0 件を期待: got=%d", errCount)
	}

	// === artifact 検証: fix-result.md は存在、test-result.md は不在 ===
	stepDir := filepath.Join(runDir, "05-final-review")
	if _, err := os.Stat(filepath.Join(stepDir, "fix-result.md")); err != nil {
		t.Errorf("fix-result.md が永続化されているべき: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stepDir, "test-result.md")); !os.IsNotExist(err) {
		t.Errorf("test-result.md は Phase 4 skip 時に作成されないはず: stat=%v", err)
	}
}

// TestRunFinalReview_Phase1Abort_PromptFailure_StopsRunWithErrorEvent は Phase 1 で
// validate-impl が success=false で abort したとき、Phase 2-5 を skip して WorkflowError を
// 1 件発火し err=nil で return することを固定する (Req 1.6)。
//
// inquiryloop.Engine.RunPrompt は agent.success=false を ReasonPromptFailure → StatusAborted
// として詰めて返す契約 (engine.go:144) なので、このテストは prompt-failure 経路を駆動する。
func TestRunFinalReview_Phase1Abort_PromptFailure_StopsRunWithErrorEvent(t *testing.T) {
	const (
		feature = "demo"
		order   = 4
	)

	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "skill":
			// validate-impl が success=false → engine が prompt-failure abort で詰めて返す。
			return provider.AgentResponse{
				Success:   false,
				Content:   "agent intentionally returns success=false for test",
				SessionID: "session-skill-failed",
			}, nil
		}
		t.Errorf("Phase 1 abort 後に kind=%q が呼ばれてはならない", call.Kind)
		return provider.AgentResponse{}, nil
	}

	onEvent, eventsPtr := collectEvents()
	err := runFinalReview(
		context.Background(),
		agent,
		cwd, runDir, feature,
		order,
		onEvent,
		testImplPersona(),
	)
	if err != nil {
		t.Fatalf("err=nil を期待 (想定済み abort は err=nil で return): got=%v", err)
	}

	// === Phase 2-5 が skip されたこと ===
	if cnt := countCallsByKind(agent.Calls, "fix-edit"); cnt != 0 {
		t.Errorf("Phase 1 abort 後に fix-edit 0 件を期待: got=%d", cnt)
	}
	if cnt := countCallsByKind(agent.Calls, "test-edit"); cnt != 0 {
		t.Errorf("Phase 1 abort 後に test-edit 0 件を期待: got=%d", cnt)
	}
	if cnt := countCallsByKind(agent.Calls, "commit"); cnt != 0 {
		t.Errorf("Phase 1 abort 後に commit 0 件を期待: got=%d", cnt)
	}

	// === WorkflowError 1 件 / WorkflowDone 0 件 ===
	doneCount := 0
	errCount := 0
	var errMsg string
	for _, e := range *eventsPtr {
		switch e.Type {
		case workflow.WorkflowDone:
			doneCount++
		case workflow.WorkflowError:
			errCount++
			errMsg = e.Message
		}
	}
	if errCount != 1 {
		t.Errorf("WorkflowError 1 件発火を期待: got=%d (events=%+v)", errCount, *eventsPtr)
	}
	if doneCount != 0 {
		t.Errorf("WorkflowDone 0 件を期待 (abort 経路): got=%d", doneCount)
	}
	// Termination 種別が Message に観測可能であること (シナリオ 4a の prep)。
	if !strings.Contains(errMsg, "prompt-failure") {
		t.Errorf("Message に Termination 種別 'prompt-failure' を期待 (Req 6.1 観測可能性): got=%q", errMsg)
	}
}

// TestRunFinalReview_Phase5CommitFailure_EmitsWorkflowError は Phase 5 で /git-commit が
// success=false を返したとき、WorkflowError を 1 件発火して run を停止することを固定する
// (Req 4.4)。 Phase 1-4 は正常完走させ、Phase 5 commit のみで失敗を再現する。
func TestRunFinalReview_Phase5CommitFailure_EmitsWorkflowError(t *testing.T) {
	const (
		feature = "demo"
		order   = 6
	)

	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "skill":
			skillCount := countCallsByKind(agent.Calls, "skill")
			if skillCount == 1 {
				return skillInquiryResp(), nil
			}
			return skillNoInquiryResp(), nil
		case "answerer":
			return answerOKResp(), nil
		case "fix-edit":
			return finalReviewFixEditOKResp(), nil
		case "fix-judge":
			return finalReviewFixJudgeOKResp(3), nil
		case "test-edit":
			return finalReviewTestEditOKResp(), nil
		case "test-judge":
			return finalReviewTestJudgePassResp(3), nil
		case "commit":
			// /git-commit が success=false (commit hook 失敗等の想定)。
			return commitFailResp(), nil
		}
		t.Fatalf("予期しない agent.Call kind=%q", call.Kind)
		return provider.AgentResponse{}, nil
	}

	onEvent, eventsPtr := collectEvents()
	err := runFinalReview(
		context.Background(),
		agent,
		cwd, runDir, feature,
		order,
		onEvent,
		testImplPersona(),
	)
	if err != nil {
		t.Fatalf("err=nil を期待 (Phase 5 失敗は WorkflowError 経由 + err=nil 規約): got=%v", err)
	}

	// === WorkflowError 1 件 / WorkflowDone 0 件 ===
	doneCount := 0
	errCount := 0
	var errMsg string
	for _, e := range *eventsPtr {
		switch e.Type {
		case workflow.WorkflowDone:
			doneCount++
		case workflow.WorkflowError:
			errCount++
			errMsg = e.Message
		}
	}
	if errCount != 1 {
		t.Errorf("WorkflowError 1 件発火を期待: got=%d", errCount)
	}
	if doneCount != 0 {
		t.Errorf("WorkflowDone 0 件を期待 (commit 失敗 abort): got=%d", doneCount)
	}
	if !strings.Contains(errMsg, "commit") {
		t.Errorf("Message に 'commit' を期待 (シナリオ 4d の prep): got=%q", errMsg)
	}

	// === Phase 5 で commit が呼ばれたこと (8 件中 1 件 = commit) ===
	if cnt := countCallsByKind(agent.Calls, "commit"); cnt != 1 {
		t.Errorf("commit 1 件を期待 (Phase 5 で /git-commit 起動): got=%d", cnt)
	}
}

// TestRunFinalReviewNoIssueImmediateDone は Task 5.2 (シナリオ 1 / Req 1.3 / Req 8.1) の
// 観測可能完了条件を固定する: validate-impl が初回応答で has_inquiry=false (rounds=0) を
// 返すと、runFinalReview が Phase 2-5 を完全 skip して WorkflowDone を即発火する。
//
// 期待 agent.Calls 順 (1 件のみ):
//   - kind=skill / Options.StepID="final-review-skill-r0" (validate-impl initial)
//
// 期待 event:
//   - WorkflowDone 1 件 / WorkflowError 0 件
//
// 期待 artifact / Phase 5 不在:
//   - <runDir>/<NN>-final-review/fix-result.md は不在 (Phase 2-3 skip)
//   - <runDir>/<NN>-final-review/test-result.md は不在 (Phase 4 skip)
//   - StepID="commit-final-review" の Call は 0 件 (Phase 5 skip)
//
// 既存の TestRunFinalReview_Rounds0_ImmediateDone_NoFixNoTestNoCommit と挙動主体は重なるが、
// 本テストは tasks.md 5.2 が要求する **StepID 一致** と **commit-final-review の不在** を
// 明示的に多軸 assert するシナリオ 1 専用の regression lock として独立させる。
func TestRunFinalReviewNoIssueImmediateDone(t *testing.T) {
	const (
		feature = "demo"
		order   = 4
	)

	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "skill":
			// 初回応答で has_inquiry=false → Phase 1 が rounds=0 で完走。
			return skillNoInquiryResp(), nil
		}
		// rounds=0 即完走では Phase 2-5 は呼ばれない契約。
		// 万一 skill 以外の kind が観測されたら、Phase 2-5 skip 規約 (Req 1.3) が壊れている。
		t.Fatalf("予期しない agent.Call kind=%q (rounds=0 即完走では skill 1 件のみのはず): prompt=%q",
			call.Kind, firstLine(call.Prompt))
		return provider.AgentResponse{}, nil
	}

	onEvent, eventsPtr := collectEvents()
	err := runFinalReview(
		context.Background(),
		agent,
		cwd, runDir, feature,
		order,
		onEvent,
		testImplPersona(),
	)
	if err != nil {
		t.Fatalf("err=nil を期待 (rounds=0 即完走は WorkflowDone 経由 + err=nil 規約): got=%v", err)
	}

	// === agent.Calls = 1 件のみ (validate-impl initial) ===
	if len(agent.Calls) != 1 {
		t.Fatalf("agent.Calls 件数 1 を期待 (rounds=0 即完走、final-review-skill-r0 のみ): got=%d (%+v)",
			len(agent.Calls), agent.Calls)
	}

	// === Calls[0] の kind / StepID 一致検証 (validate-impl 1 件 = final-review-skill-r0) ===
	c0 := agent.Calls[0]
	if c0.Kind != "skill" {
		t.Errorf("Calls[0].Kind=skill を期待 (validate-impl initial): got=%q", c0.Kind)
	}
	if c0.Options == nil {
		t.Fatalf("Calls[0].Options=nil; StepID 観測不可 (akashic フィルタ用 Req 5.4)")
	}
	if got := c0.Options.StepID; got != "final-review-skill-r0" {
		t.Errorf("Calls[0].Options.StepID=final-review-skill-r0 を期待 (initial round=0 / kind=skill): got=%q", got)
	}
	// initial 呼び出しは fresh session (resume="" 固定) を使う。
	if c0.ResumeSessionID != "" {
		t.Errorf("Calls[0].ResumeSessionID=\"\" を期待 (validate-impl initial は fresh session): got=%q",
			c0.ResumeSessionID)
	}

	// === Phase 2-5 が呼ばれていないことを kind ベースで多軸 assert ===
	// `commit` を含むため、Phase 5 (commit-final-review) の不在を兼ねる。
	for _, banned := range []string{"answerer", "fix-edit", "fix-judge", "test-edit", "test-judge", "commit"} {
		if cnt := countCallsByKind(agent.Calls, banned); cnt != 0 {
			t.Errorf("kind=%q の Call は 0 件を期待 (rounds=0 即完走で Phase 2-5 skip): got=%d", banned, cnt)
		}
	}

	// === StepID="commit-final-review" の Call が無いこと (Phase 5 skip 観測の独立軸) ===
	// 上記 kind=commit の検査と多軸を取るため、StepID 文字列でも明示的に確認する。
	for i, c := range agent.Calls {
		if c.Options != nil && c.Options.StepID == "commit-final-review" {
			t.Errorf("Calls[%d].Options.StepID=commit-final-review は rounds=0 即完走時に存在してはならない", i)
		}
	}

	// === WorkflowDone 1 件 / WorkflowError 0 件 ===
	doneCount := 0
	errCount := 0
	for _, e := range *eventsPtr {
		switch e.Type {
		case workflow.WorkflowDone:
			doneCount++
		case workflow.WorkflowError:
			errCount++
		}
	}
	if doneCount != 1 {
		t.Errorf("WorkflowDone 1 件発火を期待 (rounds=0 即完走): got=%d (events=%+v)",
			doneCount, *eventsPtr)
	}
	if errCount != 0 {
		t.Errorf("WorkflowError 0 件を期待 (正常完走): got=%d", errCount)
	}

	// === artifact 不在検証: fix-result.md (Phase 2-3 skip) / test-result.md (Phase 4 skip) ===
	stepDir := filepath.Join(runDir, "04-final-review")
	if _, err := os.Stat(filepath.Join(stepDir, "fix-result.md")); !os.IsNotExist(err) {
		t.Errorf("fix-result.md は rounds=0 即完走時に作成されないはず (Phase 2-3 skip): stat=%v", err)
	}
	if _, err := os.Stat(filepath.Join(stepDir, "test-result.md")); !os.IsNotExist(err) {
		t.Errorf("test-result.md は rounds=0 即完走時に作成されないはず (Phase 4 skip): stat=%v", err)
	}
}

// TestRunFinalReviewFullPath は Task 5.3 (シナリオ 2 / Req 8.2) の観測可能完了条件を
// 固定する: validate-impl が rounds=1 で has_inquiry=true → answer → has_inquiry=false で
// 完走したのち、Phase 2 (fix-edit) → Phase 3 (fix-judge `Files Modified: 3`) → Phase 4
// Turn 1 (test-edit) → Phase 4 Turn 2 (test-judge `Test Result: pass` + `Files Modified: 3`)
// → Phase 5 (commit ok) を一気通貫で完走することを agent.Calls 順 / StepID / 4 artifact
// の永続化 / WorkflowDone 1 件で多軸 assert する。
//
// 期待 agent.Calls 順 (8 件、StepID も同時固定):
//  1. kind=skill     / StepID=final-review-skill-r0  (validate-impl initial → has_inquiry=true)
//  2. kind=answerer  / StepID=final-review-answer-r1 (impl-direction persona answer)
//  3. kind=skill     / StepID=final-review-skill-r1  (validate-impl resume → has_inquiry=false)
//  4. kind=fix-edit  / StepID=final-review-fix-edit  (Phase 2 fix-edit)
//  5. kind=fix-judge / StepID=final-review-fix-judge (Phase 3 fix-judge / Files Modified: 3)
//  6. kind=test-edit / StepID=final-review-test-edit (Phase 4 Turn 1 test-edit)
//  7. kind=test-judge/ StepID=final-review-test-judge (Phase 4 Turn 2 / Test Result: pass)
//  8. kind=commit    / StepID=commit-final-review    (Phase 5 /git-commit ok)
//
// 期待 artifact (<runDir>/<NN>-final-review/ 配下、4 件すべて存在):
//   - round-1-inquiry.md    (Phase 1 round 1 の inquiry)
//   - round-1-answer.md     (Phase 1 round 1 の answer)
//   - fix-result.md         (Phase 2-3)
//   - test-result.md        (Phase 4)
//
// 期待 event:
//   - WorkflowDone 1 件 / WorkflowError 0 件
//
// 既存の TestRunFinalReview_FullPath_AllFiveArtifactsPersisted と挙動主体は重なるが、
// 本テストは tasks.md 5.3 が要求する「8 件の Calls 順」「StepID 一致」「4 artifact」を
// シナリオ 2 専用の regression lock として独立させる (タスク 5.2 の
// TestRunFinalReviewNoIssueImmediateDone と同方針)。
func TestRunFinalReviewFullPath(t *testing.T) {
	const (
		feature = "demo"
		order   = 7
	)

	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "skill":
			// Calls には append 済 (scriptedAgent.CallAgent 仕様)。
			// 1 回目 (initial) → has_inquiry=true で round 1 を開始、
			// 2 回目以降 (resume) → has_inquiry=false で round 1 完走。
			skillCount := countCallsByKind(agent.Calls, "skill")
			if skillCount == 1 {
				return skillInquiryResp(), nil
			}
			return skillNoInquiryResp(), nil
		case "answerer":
			return answerOKResp(), nil
		case "fix-edit":
			return finalReviewFixEditOKResp(), nil
		case "fix-judge":
			// Files Modified: 3 → Phase 4 へ進行する経路。
			return finalReviewFixJudgeOKResp(3), nil
		case "test-edit":
			return finalReviewTestEditOKResp(), nil
		case "test-judge":
			// Test Result: pass + Files Modified: 3 → Phase 5 へ進行する経路。
			return finalReviewTestJudgePassResp(3), nil
		case "commit":
			return commitOKResp(), nil
		}
		t.Fatalf("予期しない agent.Call kind=%q prompt=%q", call.Kind, firstLine(call.Prompt))
		return provider.AgentResponse{}, nil
	}

	onEvent, eventsPtr := collectEvents()
	err := runFinalReview(
		context.Background(),
		agent,
		cwd, runDir, feature,
		order,
		onEvent,
		testImplPersona(),
	)
	if err != nil {
		t.Fatalf("err=nil を期待 (Phase 1-5 一気通貫の正常完走): got=%v", err)
	}

	// === agent.Calls 件数 = 8 (Phase 1 round 1 一巡 3 件 + Phase 2-3 2 件 + Phase 4 2 件 + Phase 5 1 件) ===
	wantKinds := []string{
		"skill",      // 1: validate-impl initial (has_inquiry=true)
		"answerer",   // 2: impl-direction persona answer (round 1)
		"skill",      // 3: validate-impl resume (has_inquiry=false で完走)
		"fix-edit",   // 4: Phase 2
		"fix-judge",  // 5: Phase 3 (Files Modified: 3)
		"test-edit",  // 6: Phase 4 Turn 1
		"test-judge", // 7: Phase 4 Turn 2 (Test Result: pass)
		"commit",     // 8: Phase 5 /git-commit
	}
	if len(agent.Calls) != len(wantKinds) {
		t.Fatalf("agent.Calls 件数 %d を期待 (Phase 1-5 一気通貫): got=%d (calls=%+v)",
			len(wantKinds), len(agent.Calls), agent.Calls)
	}
	for i, want := range wantKinds {
		if agent.Calls[i].Kind != want {
			t.Errorf("Calls[%d].Kind=%q を期待: got=%q (prompt=%q)",
				i, want, agent.Calls[i].Kind, firstLine(agent.Calls[i].Prompt))
		}
	}

	// === StepID 一致検証 (akashic フィルタ用、Req 5.4 / シナリオ 2 の独立軸) ===
	wantStepIDs := []string{
		"final-review-skill-r0",
		"final-review-answer-r1",
		"final-review-skill-r1",
		"final-review-fix-edit",
		"final-review-fix-judge",
		"final-review-test-edit",
		"final-review-test-judge",
		"commit-final-review",
	}
	for i, want := range wantStepIDs {
		if agent.Calls[i].Options == nil {
			t.Errorf("Calls[%d].Options=nil; StepID 観測不可", i)
			continue
		}
		if got := agent.Calls[i].Options.StepID; got != want {
			t.Errorf("Calls[%d].Options.StepID=%q を期待: got=%q", i, want, got)
		}
	}

	// === WorkflowDone 1 件 / WorkflowError 0 件 (正常完走) ===
	doneCount := 0
	errCount := 0
	for _, e := range *eventsPtr {
		switch e.Type {
		case workflow.WorkflowDone:
			doneCount++
		case workflow.WorkflowError:
			errCount++
		}
	}
	if doneCount != 1 {
		t.Errorf("WorkflowDone 1 件発火を期待: got=%d (events=%+v)", doneCount, *eventsPtr)
	}
	if errCount != 0 {
		t.Errorf("WorkflowError 0 件を期待 (一気通貫の正常完走): got=%d", errCount)
	}

	// === 4 artifact 永続化検証 (<runDir>/<NN>-final-review/ 配下) ===
	stepDir := filepath.Join(runDir, "07-final-review")
	wantFiles := []string{
		"round-1-inquiry.md",
		"round-1-answer.md",
		"fix-result.md",
		"test-result.md",
	}
	for _, f := range wantFiles {
		path := filepath.Join(stepDir, f)
		if _, statErr := os.Stat(path); statErr != nil {
			t.Errorf("artifact %s が永続化されていない (Req 5.1 / シナリオ 2 の必須 4 件): %v", path, statErr)
		}
	}
}

// TestRunFinalReviewNoChangeSkipsCommit は Task 5.4 (シナリオ 3 / Req 2.5 / Req 3.6 /
// Req 4.3 / Req 8.3) の観測可能完了条件を固定する: validate-impl が rounds=1 で
// has_inquiry=true → answer → has_inquiry=false で完走したのち、Phase 2 (fix-edit) →
// Phase 3 (fix-judge `Files Modified: 0`) で「修正対象なし」と要約された場合、
// Phase 4 (test verification) と Phase 5 (commit) を完全 skip して WorkflowDone を
// 発火することを多軸 assert する。
//
// 期待 agent.Calls 順 (5 件、StepID も同時固定):
//  1. kind=skill     / StepID=final-review-skill-r0  (validate-impl initial → has_inquiry=true)
//  2. kind=answerer  / StepID=final-review-answer-r1 (impl-direction persona answer)
//  3. kind=skill     / StepID=final-review-skill-r1  (validate-impl resume → has_inquiry=false)
//  4. kind=fix-edit  / StepID=final-review-fix-edit  (Phase 2 fix-edit)
//  5. kind=fix-judge / StepID=final-review-fix-judge (Phase 3 fix-judge / Files Modified: 0)
//
// 期待: test-edit / test-judge / commit の各 Call は 0 件、commit-final-review StepID
// の Call も 0 件。
//
// 期待 artifact (<runDir>/<NN>-final-review/ 配下):
//   - fix-result.md       (Phase 2-3 完走したので存在)
//   - test-result.md      (Phase 4 skip 時は不在)
//
// 期待 event:
//   - WorkflowDone 1 件 / WorkflowError 0 件
//
// 既存の TestRunFinalReview_FilesModified0_SkipsPhase4And5 と挙動主体は重なるが、
// 本テストは tasks.md 5.4 が要求する「5 件の Calls 順」「StepID 一致」「test-result.md
// 不在」「commit-final-review StepID 不在」をシナリオ 3 専用の regression lock として
// 独立させる (タスク 5.2 の TestRunFinalReviewNoIssueImmediateDone / 5.3 の
// TestRunFinalReviewFullPath と同方針)。
func TestRunFinalReviewNoChangeSkipsCommit(t *testing.T) {
	const (
		feature = "demo"
		order   = 8
	)

	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "skill":
			// 1 回目 (initial) → has_inquiry=true で round 1 を開始、
			// 2 回目以降 (resume) → has_inquiry=false で round 1 完走。
			skillCount := countCallsByKind(agent.Calls, "skill")
			if skillCount == 1 {
				return skillInquiryResp(), nil
			}
			return skillNoInquiryResp(), nil
		case "answerer":
			return answerOKResp(), nil
		case "fix-edit":
			return finalReviewFixEditOKResp(), nil
		case "fix-judge":
			// Files Modified: 0 → Phase 4-5 完全 skip 経路 (Req 2.5 / 3.6 / 4.3)。
			return finalReviewFixJudgeOKResp(0), nil
		case "test-edit", "test-judge", "commit":
			// Phase 3 で Files Modified: 0 のとき Phase 4-5 が呼ばれたら
			// シナリオ 3 の skip 規約 (Req 2.5 / 3.6 / 4.3) が壊れている。
			t.Fatalf("予期しない agent.Call kind=%q (Files Modified: 0 時は Phase 4-5 skip): prompt=%q",
				call.Kind, firstLine(call.Prompt))
			return provider.AgentResponse{}, nil
		}
		t.Fatalf("予期しない agent.Call kind=%q prompt=%q", call.Kind, firstLine(call.Prompt))
		return provider.AgentResponse{}, nil
	}

	onEvent, eventsPtr := collectEvents()
	err := runFinalReview(
		context.Background(),
		agent,
		cwd, runDir, feature,
		order,
		onEvent,
		testImplPersona(),
	)
	if err != nil {
		t.Fatalf("err=nil を期待 (Files Modified: 0 でも WorkflowDone 経由完走): got=%v", err)
	}

	// === agent.Calls 件数 = 5 (Phase 1 round 1 一巡 3 件 + Phase 2-3 fix 2 件) ===
	wantKinds := []string{
		"skill",     // 1: validate-impl initial (has_inquiry=true)
		"answerer",  // 2: impl-direction persona answer (round 1)
		"skill",     // 3: validate-impl resume (has_inquiry=false で完走)
		"fix-edit",  // 4: Phase 2
		"fix-judge", // 5: Phase 3 (Files Modified: 0)
	}
	if len(agent.Calls) != len(wantKinds) {
		t.Fatalf("agent.Calls 件数 %d を期待 (Phase 1 round 1 + Phase 2-3 のみ): got=%d (calls=%+v)",
			len(wantKinds), len(agent.Calls), agent.Calls)
	}
	for i, want := range wantKinds {
		if agent.Calls[i].Kind != want {
			t.Errorf("Calls[%d].Kind=%q を期待: got=%q (prompt=%q)",
				i, want, agent.Calls[i].Kind, firstLine(agent.Calls[i].Prompt))
		}
	}

	// === StepID 一致検証 (akashic フィルタ用、Req 5.4 / シナリオ 3 の独立軸) ===
	wantStepIDs := []string{
		"final-review-skill-r0",
		"final-review-answer-r1",
		"final-review-skill-r1",
		"final-review-fix-edit",
		"final-review-fix-judge",
	}
	for i, want := range wantStepIDs {
		if agent.Calls[i].Options == nil {
			t.Errorf("Calls[%d].Options=nil; StepID 観測不可", i)
			continue
		}
		if got := agent.Calls[i].Options.StepID; got != want {
			t.Errorf("Calls[%d].Options.StepID=%q を期待: got=%q", i, want, got)
		}
	}

	// === Phase 4/5 不在の独立軸検証 (kind ベース) ===
	for _, banned := range []string{"test-edit", "test-judge", "commit"} {
		if cnt := countCallsByKind(agent.Calls, banned); cnt != 0 {
			t.Errorf("kind=%q の Call は 0 件を期待 (Phase 4/5 skip): got=%d", banned, cnt)
		}
	}

	// === StepID="commit-final-review" の Call が無いこと (Phase 5 skip 観測の独立軸) ===
	// 上記 kind=commit の検査と多軸を取るため、StepID 文字列でも明示的に確認する。
	for i, c := range agent.Calls {
		if c.Options != nil && c.Options.StepID == "commit-final-review" {
			t.Errorf("Calls[%d].Options.StepID=commit-final-review は Files Modified: 0 時に存在してはならない", i)
		}
	}

	// === WorkflowDone 1 件 / WorkflowError 0 件 (skip も正常完走として扱う) ===
	doneCount := 0
	errCount := 0
	for _, e := range *eventsPtr {
		switch e.Type {
		case workflow.WorkflowDone:
			doneCount++
		case workflow.WorkflowError:
			errCount++
		}
	}
	if doneCount != 1 {
		t.Errorf("WorkflowDone 1 件発火を期待: got=%d (events=%+v)", doneCount, *eventsPtr)
	}
	if errCount != 0 {
		t.Errorf("WorkflowError 0 件を期待 (skip も正常完走): got=%d", errCount)
	}

	// === artifact 検証: fix-result.md は Phase 2-3 完走で永続化、test-result.md は Phase 4 skip で不在 ===
	stepDir := filepath.Join(runDir, "08-final-review")
	if _, statErr := os.Stat(filepath.Join(stepDir, "fix-result.md")); statErr != nil {
		t.Errorf("fix-result.md は Phase 2-3 完走で永続化されているべき (Req 5.1): %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(stepDir, "test-result.md")); !os.IsNotExist(statErr) {
		t.Errorf("test-result.md は Phase 4 skip 時に作成されないはず: stat=%v", statErr)
	}
}

// TestRunFinalReviewAbortRegression は Task 5.5 (シナリオ 4 / Req 1.6, 2.6, 3.5, 4.4, 6.1, 6.2,
// 6.3, 8.4) の観測可能完了条件を固定する: 各 Phase で発生し得る abort / 失敗ケースを 6 サブテストに
// 分けて regression lock する。各サブテストは独立した cwd / runDir / scriptedAgent を使い、
// 状態漏洩を避ける (副作用ゼロ regression、Req 6.3)。
//
// 各サブテストの責務:
//
//	4a/Phase1SchemaViolation:        validate-impl が schema-violation で abort
//	                                 → Phase 2-5 skip / WorkflowError 1 件 / Message に "schema-violation"
//	4b/Phase2FixEditAgentError:      fix-edit Turn 1 で agent error
//	                                 → Phase 4-5 skip / Message に "fix-edit" を含む
//	4c/Phase4TestEditAgentError:     test-edit Turn 1 で agent error
//	                                 → Phase 5 skip / Message に "test-edit" を含む
//	4c2/Phase4TestJudgeFail:         test-judge が `Test Result: fail` を返す
//	                                 → Phase 5 skip / Message に "test-failed" を含む
//	4c3/Phase4TestJudgeSchemaViolation: test-judge summary が `Test Result` 行を含まない
//	                                 → Phase 5 skip / Message に "schema-violation" を含む
//	4d/Phase5CommitFailure:          /git-commit が success=false を返す
//	                                 → WorkflowError 1 件 / Message に "commit" を含む
//
// 共通の検証軸 (各サブテストで多軸 assert):
//  1. runFinalReview は err=nil で return する (想定済み abort 規約)
//  2. WorkflowError event が 1 件発火し、その Message が想定 substring を含む
//  3. WorkflowDone event は 0 件 (abort 経路では発火しない)
//  4. abort 後の Phase に対応する agent.Call kind が呼ばれていない (skip 規約)
//
// per-task 経路 (`impl_loop.go:172-291`) の動作不変は、本ファイルが直接 `runFinalReview` を
// 呼び分離する設計と、各サブテストが独立した t.TempDir() を使うことで自然に担保される
// (impl_loop_inquiry_test.go / impl_loop_skip_test.go / impl_loop_test.go の既存テストが
// 並走し全件 green を維持することで cross-test regression が観測される、Req 6.3)。
func TestRunFinalReviewAbortRegression(t *testing.T) {
	// schemaViolationSkillResp は Phase 1 で inquiryloop.Engine.RunPrompt が schema-violation
	// として詰めて return する応答を作る。StructuredOutput=nil + Content に JSON 抽出不能な平文を
	// 詰めることで `inquiryloop.ParseResponse` が `ErrSchemaViolation` を返し、Engine が
	// `ReasonSchemaViolation` で abort する経路を駆動する (engine_test.go の (c) ケースと同型)。
	schemaViolationSkillResp := func() provider.AgentResponse {
		return provider.AgentResponse{
			Success: true,
			// StructuredOutput=nil、Content に中括弧無し平文 → ParseResponse の全 3 経路が失敗 → ErrSchemaViolation
			Content:   "no json blob, plain text only — schema violation forcing input",
			SessionID: "session-skill-schema-violation",
		}
	}

	// agentErrFn は agent.CallAgent が Go レベルの error (= agent error) を返したことを再現するヘルパ。
	agentErrFn := func(msg string) error { return errors.New(msg) }

	// assertNoPerTaskCalls は per-task ループ (impl_loop.go:172-291) が起動していないことを
	// agent.Calls の StepID prefix で多軸検証するヘルパ。Req 6.3 (副作用ゼロ regression) の
	// 共通 assertion として 6 サブテスト全てで使う。
	//
	// per-task ループは以下の StepID prefix を出す契約 (impl_loop.go の StepIDFunc / WithStep):
	//   - "impl-<taskID>-<kind>-r<N>"   (inquiryloop の skill / answerer round 番号付き)
	//   - "commit-<taskID>"             (per-task /git-commit; final-review の "commit-final-review"
	//                                    と prefix で衝突しないよう "-final-review" のみ除外する)
	//
	// runFinalReview を直接駆動する本テストでは per-task StepID は本来 1 件も観測されないが、
	// 万一 final-review pipeline が per-task ループのどこかにレートリミット越境した実装に
	// regression したら、本 assertion で全 6 サブテスト同時に検出できる。
	assertNoPerTaskCalls := func(t *testing.T, calls []recordedAgentCall) {
		t.Helper()
		for i, c := range calls {
			if c.Options == nil {
				continue
			}
			stepID := c.Options.StepID
			if strings.HasPrefix(stepID, "impl-") {
				t.Errorf("Calls[%d].Options.StepID=%q: per-task ループの StepID prefix \"impl-\" が"+
					"含まれてはならない (副作用ゼロ regression / Req 6.3)", i, stepID)
			}
			if strings.HasPrefix(stepID, "commit-") && stepID != "commit-final-review" {
				t.Errorf("Calls[%d].Options.StepID=%q: per-task ループの StepID prefix \"commit-<taskID>\""+
					" が含まれてはならない (final-review 経路は \"commit-final-review\" のみ、Req 6.3)", i, stepID)
			}
		}
	}

	// 各サブテストで使う共通の event カウント検証ヘルパ。
	// 期待: WorkflowError 件数 = wantErrCount / WorkflowDone 件数 = wantDoneCount。
	// errMsgSubstr が空でない場合、最後の WorkflowError event の Message が当該 substring を含むこと。
	verifyEventCounts := func(t *testing.T, events []workflow.WorkflowEvent, wantErrCount, wantDoneCount int, errMsgSubstr string) {
		t.Helper()
		doneCount := 0
		errCount := 0
		var lastErrMsg string
		for _, e := range events {
			switch e.Type {
			case workflow.WorkflowDone:
				doneCount++
			case workflow.WorkflowError:
				errCount++
				lastErrMsg = e.Message
			}
		}
		if errCount != wantErrCount {
			t.Errorf("WorkflowError 件数 %d を期待: got=%d (events=%+v)", wantErrCount, errCount, events)
		}
		if doneCount != wantDoneCount {
			t.Errorf("WorkflowDone 件数 %d を期待 (abort 経路): got=%d", wantDoneCount, doneCount)
		}
		if errMsgSubstr != "" && !strings.Contains(lastErrMsg, errMsgSubstr) {
			t.Errorf("WorkflowError.Message に %q を期待 (失敗フェーズ識別、Req 6.1): got=%q",
				errMsgSubstr, lastErrMsg)
		}
	}

	t.Run("4a/Phase1SchemaViolation", func(t *testing.T) {
		const (
			feature = "demo"
			order   = 4
		)
		cwd := t.TempDir()
		runDir := t.TempDir()

		agent := &scriptedAgent{}
		agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
			switch call.Kind {
			case "skill":
				// Phase 1 初回応答で schema-violation → engine が ReasonSchemaViolation で abort。
				return schemaViolationSkillResp(), nil
			}
			t.Fatalf("Phase 1 schema-violation 後に kind=%q が呼ばれてはならない (Phase 2-5 skip 規約)",
				call.Kind)
			return provider.AgentResponse{}, nil
		}

		onEvent, eventsPtr := collectEvents()
		err := runFinalReview(
			context.Background(),
			agent,
			cwd, runDir, feature,
			order,
			onEvent,
			testImplPersona(),
		)
		if err != nil {
			t.Fatalf("err=nil を期待 (想定済み abort は err=nil 規約): got=%v", err)
		}

		// === Phase 2-5 が呼ばれていないこと ===
		for _, banned := range []string{"answerer", "fix-edit", "fix-judge", "test-edit", "test-judge", "commit"} {
			if cnt := countCallsByKind(agent.Calls, banned); cnt != 0 {
				t.Errorf("Phase 1 schema-violation 後に kind=%q は 0 件を期待 (Phase 2-5 skip): got=%d",
					banned, cnt)
			}
		}

		// === WorkflowError 1 件 / WorkflowDone 0 件 / Message に "schema-violation" ===
		verifyEventCounts(t, *eventsPtr, 1, 0, "schema-violation")

		// === 副作用ゼロ regression: per-task ループ StepID 不在 (Req 6.3) ===
		assertNoPerTaskCalls(t, agent.Calls)
	})

	t.Run("4b/Phase2FixEditAgentError", func(t *testing.T) {
		const (
			feature = "demo"
			order   = 4
		)
		cwd := t.TempDir()
		runDir := t.TempDir()

		agent := &scriptedAgent{}
		agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
			switch call.Kind {
			case "skill":
				skillCount := countCallsByKind(agent.Calls, "skill")
				if skillCount == 1 {
					return skillInquiryResp(), nil
				}
				return skillNoInquiryResp(), nil
			case "answerer":
				return answerOKResp(), nil
			case "fix-edit":
				// Phase 2 Turn 1 (fix-edit) で agent error → fix-judge も Phase 4-5 も呼ばれない。
				return provider.AgentResponse{}, agentErrFn("simulated final-review fix-edit agent error")
			case "fix-judge", "test-edit", "test-judge", "commit":
				t.Fatalf("Phase 2 fix-edit 失敗後に kind=%q が呼ばれてはならない (Phase 4-5 skip 規約)",
					call.Kind)
			}
			return provider.AgentResponse{}, nil
		}

		onEvent, eventsPtr := collectEvents()
		err := runFinalReview(
			context.Background(),
			agent,
			cwd, runDir, feature,
			order,
			onEvent,
			testImplPersona(),
		)
		if err != nil {
			t.Fatalf("err=nil を期待: got=%v", err)
		}

		// === Phase 4-5 が呼ばれていないこと ===
		for _, banned := range []string{"test-edit", "test-judge", "commit"} {
			if cnt := countCallsByKind(agent.Calls, banned); cnt != 0 {
				t.Errorf("Phase 2 fix-edit 失敗後に kind=%q は 0 件を期待 (Phase 4-5 skip): got=%d",
					banned, cnt)
			}
		}

		// === fix-edit が 1 件呼ばれた / fix-judge は呼ばれていないこと (Turn 1 失敗で Turn 2 skip) ===
		if cnt := countCallsByKind(agent.Calls, "fix-edit"); cnt != 1 {
			t.Errorf("fix-edit 1 件を期待 (Phase 2 Turn 1 起動): got=%d", cnt)
		}
		if cnt := countCallsByKind(agent.Calls, "fix-judge"); cnt != 0 {
			t.Errorf("fix-judge 0 件を期待 (Turn 1 agent error で Turn 2 skip): got=%d", cnt)
		}

		// === WorkflowError 1 件 / WorkflowDone 0 件 / Message に "fix-edit" ===
		verifyEventCounts(t, *eventsPtr, 1, 0, "fix-edit")

		// === 副作用ゼロ regression: per-task ループ StepID 不在 (Req 6.3) ===
		assertNoPerTaskCalls(t, agent.Calls)
	})

	t.Run("4c/Phase4TestEditAgentError", func(t *testing.T) {
		const (
			feature = "demo"
			order   = 4
		)
		cwd := t.TempDir()
		runDir := t.TempDir()

		agent := &scriptedAgent{}
		agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
			switch call.Kind {
			case "skill":
				skillCount := countCallsByKind(agent.Calls, "skill")
				if skillCount == 1 {
					return skillInquiryResp(), nil
				}
				return skillNoInquiryResp(), nil
			case "answerer":
				return answerOKResp(), nil
			case "fix-edit":
				return finalReviewFixEditOKResp(), nil
			case "fix-judge":
				return finalReviewFixJudgeOKResp(3), nil
			case "test-edit":
				// Phase 4 Turn 1 (test-edit) で agent error → test-judge も Phase 5 も呼ばれない。
				return provider.AgentResponse{}, agentErrFn("simulated final-review test-edit agent error")
			case "test-judge", "commit":
				t.Fatalf("Phase 4 test-edit 失敗後に kind=%q が呼ばれてはならない (Phase 5 skip 規約)",
					call.Kind)
			}
			return provider.AgentResponse{}, nil
		}

		onEvent, eventsPtr := collectEvents()
		err := runFinalReview(
			context.Background(),
			agent,
			cwd, runDir, feature,
			order,
			onEvent,
			testImplPersona(),
		)
		if err != nil {
			t.Fatalf("err=nil を期待: got=%v", err)
		}

		// === Phase 5 (commit) が呼ばれていないこと ===
		if cnt := countCallsByKind(agent.Calls, "commit"); cnt != 0 {
			t.Errorf("commit 0 件を期待 (Phase 5 skip): got=%d", cnt)
		}
		// === test-edit は呼ばれた / test-judge は呼ばれていないこと (Turn 1 失敗で Turn 2 skip) ===
		if cnt := countCallsByKind(agent.Calls, "test-edit"); cnt != 1 {
			t.Errorf("test-edit 1 件を期待 (Phase 4 Turn 1 起動): got=%d", cnt)
		}
		if cnt := countCallsByKind(agent.Calls, "test-judge"); cnt != 0 {
			t.Errorf("test-judge 0 件を期待 (Turn 1 agent error で Turn 2 skip): got=%d", cnt)
		}

		// === WorkflowError 1 件 / WorkflowDone 0 件 / Message に "test-edit" ===
		verifyEventCounts(t, *eventsPtr, 1, 0, "test-edit")

		// === 副作用ゼロ regression: per-task ループ StepID 不在 (Req 6.3) ===
		assertNoPerTaskCalls(t, agent.Calls)
	})

	t.Run("4c2/Phase4TestJudgeFail", func(t *testing.T) {
		const (
			feature = "demo"
			order   = 4
		)
		cwd := t.TempDir()
		runDir := t.TempDir()

		agent := &scriptedAgent{}
		agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
			switch call.Kind {
			case "skill":
				skillCount := countCallsByKind(agent.Calls, "skill")
				if skillCount == 1 {
					return skillInquiryResp(), nil
				}
				return skillNoInquiryResp(), nil
			case "answerer":
				return answerOKResp(), nil
			case "fix-edit":
				return finalReviewFixEditOKResp(), nil
			case "fix-judge":
				return finalReviewFixJudgeOKResp(3), nil
			case "test-edit":
				return finalReviewTestEditOKResp(), nil
			case "test-judge":
				// Test Result: fail → schema 適合だがテスト不通解消不能 → Phase 5 skip。
				return finalReviewTestJudgeFailResp(2), nil
			case "commit":
				t.Fatalf("test-judge fail 後に commit が呼ばれてはならない (Phase 5 skip 規約)")
			}
			return provider.AgentResponse{}, nil
		}

		onEvent, eventsPtr := collectEvents()
		err := runFinalReview(
			context.Background(),
			agent,
			cwd, runDir, feature,
			order,
			onEvent,
			testImplPersona(),
		)
		if err != nil {
			t.Fatalf("err=nil を期待: got=%v", err)
		}

		// === Phase 5 (commit) が呼ばれていないこと ===
		if cnt := countCallsByKind(agent.Calls, "commit"); cnt != 0 {
			t.Errorf("commit 0 件を期待 (Phase 5 skip): got=%d", cnt)
		}
		// === Phase 4 は両 Turn 起動した (schema 適合のため Turn 2 まで進んだ) ===
		if cnt := countCallsByKind(agent.Calls, "test-edit"); cnt != 1 {
			t.Errorf("test-edit 1 件を期待: got=%d", cnt)
		}
		if cnt := countCallsByKind(agent.Calls, "test-judge"); cnt != 1 {
			t.Errorf("test-judge 1 件を期待: got=%d", cnt)
		}

		// === WorkflowError 1 件 / WorkflowDone 0 件 / Message に "test-failed" ===
		verifyEventCounts(t, *eventsPtr, 1, 0, "test-failed")

		// === 副作用ゼロ regression: per-task ループ StepID 不在 (Req 6.3) ===
		assertNoPerTaskCalls(t, agent.Calls)
	})

	t.Run("4c3/Phase4TestJudgeSchemaViolation", func(t *testing.T) {
		const (
			feature = "demo"
			order   = 4
		)
		cwd := t.TempDir()
		runDir := t.TempDir()

		agent := &scriptedAgent{}
		agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
			switch call.Kind {
			case "skill":
				skillCount := countCallsByKind(agent.Calls, "skill")
				if skillCount == 1 {
					return skillInquiryResp(), nil
				}
				return skillNoInquiryResp(), nil
			case "answerer":
				return answerOKResp(), nil
			case "fix-edit":
				return finalReviewFixEditOKResp(), nil
			case "fix-judge":
				return finalReviewFixJudgeOKResp(3), nil
			case "test-edit":
				return finalReviewTestEditOKResp(), nil
			case "test-judge":
				// `Test Result:` 行を意図的に欠落させる (Files Modified だけ) → schema-violation。
				// parseTestResult が error を返し runFinalReviewTestVerification が
				// "Test Result marker missing" を Message に詰める → runFinalReview が
				// "schema-violation" prefix を付与する経路。
				return provider.AgentResponse{
					Success: true,
					StructuredOutput: map[string]any{
						"has_inquiry": false,
						"summary":     "test 実行は完了したが Test Result の必須行を出していない\n\nFiles Modified: 1",
					},
					SessionID: "session-final-review-test-judge",
				}, nil
			case "commit":
				t.Fatalf("test-judge schema-violation 後に commit が呼ばれてはならない (Phase 5 skip 規約)")
			}
			return provider.AgentResponse{}, nil
		}

		onEvent, eventsPtr := collectEvents()
		err := runFinalReview(
			context.Background(),
			agent,
			cwd, runDir, feature,
			order,
			onEvent,
			testImplPersona(),
		)
		if err != nil {
			t.Fatalf("err=nil を期待: got=%v", err)
		}

		// === Phase 5 (commit) が呼ばれていないこと ===
		if cnt := countCallsByKind(agent.Calls, "commit"); cnt != 0 {
			t.Errorf("commit 0 件を期待 (Phase 5 skip): got=%d", cnt)
		}
		// === Phase 4 は両 Turn 起動した (schema-violation 観測も Turn 2 が起動して初めて分かる) ===
		if cnt := countCallsByKind(agent.Calls, "test-edit"); cnt != 1 {
			t.Errorf("test-edit 1 件を期待: got=%d", cnt)
		}
		if cnt := countCallsByKind(agent.Calls, "test-judge"); cnt != 1 {
			t.Errorf("test-judge 1 件を期待: got=%d", cnt)
		}

		// === WorkflowError 1 件 / WorkflowDone 0 件 / Message に "schema-violation" ===
		verifyEventCounts(t, *eventsPtr, 1, 0, "schema-violation")

		// === 副作用ゼロ regression: per-task ループ StepID 不在 (Req 6.3) ===
		assertNoPerTaskCalls(t, agent.Calls)
	})

	t.Run("4d/Phase5CommitFailure", func(t *testing.T) {
		const (
			feature = "demo"
			order   = 4
		)
		cwd := t.TempDir()
		runDir := t.TempDir()

		agent := &scriptedAgent{}
		agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
			switch call.Kind {
			case "skill":
				skillCount := countCallsByKind(agent.Calls, "skill")
				if skillCount == 1 {
					return skillInquiryResp(), nil
				}
				return skillNoInquiryResp(), nil
			case "answerer":
				return answerOKResp(), nil
			case "fix-edit":
				return finalReviewFixEditOKResp(), nil
			case "fix-judge":
				return finalReviewFixJudgeOKResp(3), nil
			case "test-edit":
				return finalReviewTestEditOKResp(), nil
			case "test-judge":
				return finalReviewTestJudgePassResp(3), nil
			case "commit":
				// /git-commit が success=false (例: pre-commit hook 失敗 / 空 commit 等)。
				return commitFailResp(), nil
			}
			return provider.AgentResponse{}, nil
		}

		onEvent, eventsPtr := collectEvents()
		err := runFinalReview(
			context.Background(),
			agent,
			cwd, runDir, feature,
			order,
			onEvent,
			testImplPersona(),
		)
		if err != nil {
			t.Fatalf("err=nil を期待 (Phase 5 失敗は WorkflowError 経由 + err=nil 規約): got=%v", err)
		}

		// === Phase 5 で commit が 1 件呼ばれた ===
		if cnt := countCallsByKind(agent.Calls, "commit"); cnt != 1 {
			t.Errorf("commit 1 件を期待 (Phase 5 で /git-commit 起動): got=%d", cnt)
		}

		// === WorkflowError 1 件 / WorkflowDone 0 件 / Message に "commit" ===
		verifyEventCounts(t, *eventsPtr, 1, 0, "commit")

		// === 副作用ゼロ regression: per-task ループ StepID 不在 (Req 6.3) ===
		assertNoPerTaskCalls(t, agent.Calls)
	})
}

// =============================================================================
// Divergence observability test (Task 5.6 / Req 7.1)
// =============================================================================
//
// design.md「Divergence observability (Issue 3 解消)」契約に従い、spec_loop の
// `runFixPhase` (L792 起点) と impl_loop_final_review.go の `runFinalReviewFixPhase`
// が共有する 4 軸の挙動契約が片側だけ書き換わった瞬間に CI で fail させる
// "観察可能性" を担保する。
//
// 4 軸:
//
//	(a) Turn 数 (= 2)
//	(b) Turn 1 の OutputFormat 設定 (= nil)
//	(c) Turn 2 の OutputFormat 設定 (= inquiryloop.Schema())
//	(d) Turn 2 の AllowedTools 設定 (= fixJudgeAllowedTools)
//
// 対象外: prompt 文面 / 引数列 (signature の差) / sessionId 受け渡し / StepID 文字列など、
// 意図的に独自化される領域。これらが divergence しても本テストは fail しない。
//
// mutation 確認: 4 軸のいずれか片側を書き換える (例: runFinalReviewFixPhase の Turn 1
// OutputFormat に inquiryloop.Schema() を渡す変更を加える) と本テストは個別に fail する。
// 4 軸のいずれかに divergence が発生したら共通 package
// (`internal/workflow/builtin/kiro/fixphase/`) 抽出 spec の起票 signal となる。
func TestFinalReviewFixPhaseDivergenceObservability(t *testing.T) {
	ctx := context.Background()

	// --- (1) spec_loop runFixPhase 起動 ---
	specAgent := &scriptedAgent{}
	specAgent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "fix-edit":
			// spec_loop fixture (Turn 1 markdown 自由記述、SessionID=session-fix-edit)
			return fixEditOKResp(), nil
		case "fix-judge":
			// spec_loop fixture (Turn 2 構造化出力、has_inquiry=false)。spec_loop の
			// `runFixPhase` は Files Modified marker を parse しないので summary に含めない。
			return fixJudgeOKResp(), nil
		}
		t.Fatalf("予期しない spec_loop agent.Call kind=%q prompt=%q", call.Kind, call.Prompt)
		return provider.AgentResponse{}, nil
	}

	specOnEvent, _ := collectEvents()
	_ = runFixPhase(
		ctx,
		specAgent,
		t.TempDir(),
		"demo",
		t.TempDir(),
		// step.Order / step.Phase は StepID と persistence path 命名に使われるだけなので、
		// 4 軸 assert への影響はない。
		promptStep{Order: 5, Phase: "validate-design", MaxRounds: 15},
		"session-skill",
		"validate-design-director",
		"validate-design",
		specOnEvent,
	)

	// --- (2) impl_loop_final_review.go runFinalReviewFixPhase 起動 ---
	finalAgent := &scriptedAgent{}
	finalAgent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "fix-edit":
			// final-review fixture (Turn 1、SessionID=session-final-review-fix-edit)
			return finalReviewFixEditOKResp(), nil
		case "fix-judge":
			// final-review fixture (Turn 2、summary に "Files Modified: 3" を含む。
			// runFinalReviewFixPhase は parseChangedFilesCount で構造化抽出する)。
			return finalReviewFixJudgeOKResp(3), nil
		}
		t.Fatalf("予期しない final-review agent.Call kind=%q prompt=%q", call.Kind, call.Prompt)
		return provider.AgentResponse{}, nil
	}

	finalOnEvent, _ := collectEvents()
	_ = runFinalReviewFixPhase(
		ctx,
		finalAgent,
		t.TempDir(),
		"demo",
		t.TempDir(),
		// order=7 / phase="final-review" は StepID = "final-review-fix-edit" 等の固定文字列に
		// 直接寄与しない (final-review 系の StepID は impl_loop_final_review.go の中で
		// 固定文字列になっているため)。4 軸 assert への影響はない。
		7,
		"final-review",
		"session-skill",
		"impl-direction",
		"最終検証",
		finalOnEvent,
	)

	// --- (3) 4 軸 assert (両側で同一の helper を使い対称性を担保) ---
	assertContract := func(t *testing.T, name string, calls []recordedAgentCall) {
		t.Helper()

		// (a) Turn 数 = 2
		if len(calls) != 2 {
			t.Errorf("[%s] axis (a): Turn 数 = 2 を期待 (fix-edit + fix-judge): got=%d", name, len(calls))
			return
		}
		edit, judge := calls[0], calls[1]

		if edit.Options == nil {
			t.Fatalf("[%s] Turn 1 Options が nil; CallAgentOptions が渡されていない", name)
		}
		if judge.Options == nil {
			t.Fatalf("[%s] Turn 2 Options が nil; CallAgentOptions が渡されていない", name)
		}

		// (b) Turn 1 OutputFormat = nil (json_schema 非強制で Edit/Write 解放)
		if edit.Options.OutputFormat != nil {
			t.Errorf("[%s] axis (b): Turn 1 OutputFormat=nil を期待 (Edit/Write 解放、json_schema 非強制): got=%+v",
				name, edit.Options.OutputFormat)
		}

		// (c) Turn 2 OutputFormat = inquiryloop.Schema() (構造化抽出強制)
		// inquiryloop.Schema() は呼び出しごとに新規インスタンスを返すため reflect.DeepEqual で
		// 値同値を比較する。ポインタ同値は使えない。
		wantSchema := inquiryloop.Schema()
		if !reflect.DeepEqual(judge.Options.OutputFormat, wantSchema) {
			t.Errorf("[%s] axis (c): Turn 2 OutputFormat=inquiryloop.Schema() を期待: got=%+v want=%+v",
				name, judge.Options.OutputFormat, wantSchema)
		}

		// (d) Turn 2 AllowedTools = fixJudgeAllowedTools (Read/Glob/Grep のみ)
		if !reflect.DeepEqual(judge.Options.AllowedTools, fixJudgeAllowedTools) {
			t.Errorf("[%s] axis (d): Turn 2 AllowedTools=fixJudgeAllowedTools を期待: got=%v want=%v",
				name, judge.Options.AllowedTools, fixJudgeAllowedTools)
		}
	}

	assertContract(t, "runFixPhase (spec_loop)", specAgent.Calls)
	assertContract(t, "runFinalReviewFixPhase (impl_loop)", finalAgent.Calls)
}
