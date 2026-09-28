// Package apply のテスト本ファイルは task 2.1 / design.md §Loader の契約を検証する
// (Req 1.1, 1.4, 1.5, 1.6, 7.5)。
//
// 検証観点 (tasks.md 2.1 観察可能完了条件):
//   - 正常: findings_reported / no_findings / failed が混在するディレクトリで
//     TargetRules / SkippedSummary / FindingsFiles の振り分けが正しい
//   - 空ディレクトリ: AllRules / TargetRules がいずれも 0 件、エラーなし
//   - verdict 混在: SkippedSummary に verdict 別件数が積まれる
//   - schema 違反: 必須フィールド (name / verdict) 欠落で LoaderSchemaError
//   - decode 失敗 (壊れた JSON): LoaderSchemaError (Cause に json.SyntaxError 系)
//   - FindingsFiles 空集合 (契約違反防御): TargetRules の Findings.File が全て空文字列でも
//     panic せず空 slice を返す (4.1 fail-closed の入力前提と整合)
//   - reviewRunID 空 / 不存在 dir / 非ディレクトリ: 各 sentinel error を返す
//   - result.json はスキップされる (per-rule artifact ではないため)
package apply

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/result"
)

// loaderTestEnv は Loader テスト用の <cwd>/.rilll/review-workflow/<reviewRunID>/ を
// t.TempDir() 配下に構築する helper。返り値は (cwd, reviewRunID, runDir 絶対パス)。
//
// 各テストが独立した temp dir を持つため `t.Parallel()` 安全。
func loaderTestEnv(t *testing.T, runID string) (cwd, reviewRunID, runDir string) {
	t.Helper()
	cwd = t.TempDir()
	reviewRunID = runID
	runDir = filepath.Join(cwd, ".rilll", "review-workflow", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", runDir, err)
	}
	return cwd, reviewRunID, runDir
}

// writeRuleResultJSON は per-rule 結果ファイル `<runDir>/<ruleName>.json` を書き出す helper。
// 値は構造体ベースで指定し、json.MarshalIndent で書き出す。
func writeRuleResultJSON(t *testing.T, runDir, ruleName string, rr result.RuleResult) {
	t.Helper()
	body, err := json.MarshalIndent(rr, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	path := filepath.Join(runDir, ruleName+".json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

// writeRawJSON は壊れた JSON / 必須フィールド欠落 fixture を書き出す helper。
// `body` をそのまま `<runDir>/<ruleName>.json` に書き出す (json.Marshal を経由しない)。
func writeRawJSON(t *testing.T, runDir, ruleName, body string) {
	t.Helper()
	path := filepath.Join(runDir, ruleName+".json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

// TestLoad_HappyMixedVerdicts は findings_reported / no_findings / failed が混在する
// 標準ケースで、TargetRules / SkippedSummary / FindingsFiles が期待通り計算されることを検証する
// (Req 1.1, 1.5, 1.6 の正常経路)。
func TestLoad_HappyMixedVerdicts(t *testing.T) {
	t.Parallel()
	cwd, runID, runDir := loaderTestEnv(t, "run-mixed")

	// alpha: findings_reported (1 finding, file = "auth/login.go")
	writeRuleResultJSON(t, runDir, "alpha-rule", result.RuleResult{
		Name:            "alpha-rule",
		Description:     "alpha",
		Applied:         true,
		Verdict:         result.VerdictFindingsReported,
		FindingsPresent: true,
		Summary:         "1 finding",
		Findings: []result.Finding{
			{Severity: result.SeverityError, File: "auth/login.go", Location: "auth/login.go:42", Message: "x"},
		},
		Provider:   "claude",
		Model:      "claude-sonnet-4-5",
		StartedAt:  "2026-05-04T12:00:00Z",
		FinishedAt: "2026-05-04T12:00:30Z",
		SourceFile: "alpha-rule.yaml",
	})
	// bravo: no_findings (副作用ゼロ rule、修正対象外)
	writeRuleResultJSON(t, runDir, "bravo-rule", result.RuleResult{
		Name:        "bravo-rule",
		Description: "bravo",
		Applied:     true,
		Verdict:     result.VerdictNoFindings,
		Summary:     "no findings",
		Provider:    "claude",
		StartedAt:   "2026-05-04T12:00:00Z",
		FinishedAt:  "2026-05-04T12:00:01Z",
		SourceFile:  "bravo-rule.yaml",
	})
	// charlie: findings_reported (2 findings、うち 1 件は alpha と同じ file → 重複除去対象)
	writeRuleResultJSON(t, runDir, "charlie-rule", result.RuleResult{
		Name:            "charlie-rule",
		Description:     "charlie",
		Applied:         true,
		Verdict:         result.VerdictFindingsReported,
		FindingsPresent: true,
		Summary:         "2 findings",
		Findings: []result.Finding{
			{Severity: result.SeverityWarn, File: "auth/login.go", Message: "重複ファイル"}, // 重複
			{Severity: result.SeverityInfo, File: "logger/log.go", Message: "ログ出力"},
		},
		Provider:   "claude",
		StartedAt:  "2026-05-04T12:00:00Z",
		FinishedAt: "2026-05-04T12:00:10Z",
		SourceFile: "charlie-rule.yaml",
	})
	// delta: failed (修正対象外)
	writeRuleResultJSON(t, runDir, "delta-rule", result.RuleResult{
		Name:            "delta-rule",
		Description:     "delta",
		Applied:         true,
		Verdict:         result.VerdictFailed,
		Summary:         "agent error",
		FailureCategory: "provider_error",
		Provider:        "claude",
		StartedAt:       "2026-05-04T12:00:00Z",
		FinishedAt:      "2026-05-04T12:00:01Z",
		SourceFile:      "delta-rule.yaml",
	})
	// echo: skipped (修正対象外)
	writeRuleResultJSON(t, runDir, "echo-rule", result.RuleResult{
		Name:        "echo-rule",
		Description: "echo",
		Applied:     false,
		Verdict:     result.VerdictSkipped,
		SkipReason:  "Go コードを含まない",
		Summary:     "skip",
		Provider:    "claude",
		StartedAt:   "2026-05-04T12:00:00Z",
		FinishedAt:  "2026-05-04T12:00:01Z",
		SourceFile:  "echo-rule.yaml",
	})

	// review-workflow 側が同ディレクトリに書き出す全体サマリ。Loader は skip すべき。
	writeRawJSON(t, runDir, "result", `{"workflow_run_id":"run-mixed","overall_verdict":"findings_reported"}`)

	got, err := Load(t.Context(), cwd, runID)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	// AllRules: 5 件 (result.json は skip される)
	if len(got.AllRules) != 5 {
		t.Errorf("len(AllRules) = %d, want 5 (result.json は skip される契約)", len(got.AllRules))
	}
	// AllRules はファイル名昇順 (alpha, bravo, charlie, delta, echo)
	wantOrder := []string{"alpha-rule", "bravo-rule", "charlie-rule", "delta-rule", "echo-rule"}
	for i, name := range wantOrder {
		if i >= len(got.AllRules) {
			break
		}
		if got.AllRules[i].Name != name {
			t.Errorf("AllRules[%d].Name = %q, want %q (ファイル名昇順)", i, got.AllRules[i].Name, name)
		}
	}

	// TargetRules: alpha, charlie の 2 件 (filter 後も AllRules と同じ順序)
	if len(got.TargetRules) != 2 {
		t.Fatalf("len(TargetRules) = %d, want 2", len(got.TargetRules))
	}
	if got.TargetRules[0].Name != "alpha-rule" {
		t.Errorf("TargetRules[0].Name = %q, want %q", got.TargetRules[0].Name, "alpha-rule")
	}
	if got.TargetRules[1].Name != "charlie-rule" {
		t.Errorf("TargetRules[1].Name = %q, want %q", got.TargetRules[1].Name, "charlie-rule")
	}

	// SkippedSummary: no_findings=1, failed=1, skipped=1
	wantSkipped := map[result.Verdict]int{
		result.VerdictNoFindings: 1,
		result.VerdictFailed:     1,
		result.VerdictSkipped:    1,
	}
	if !reflect.DeepEqual(got.SkippedSummary, wantSkipped) {
		t.Errorf("SkippedSummary = %v, want %v", got.SkippedSummary, wantSkipped)
	}

	// 不変式: len(TargetRules) + sum(SkippedSummary) == len(AllRules)
	sumSkipped := 0
	for _, n := range got.SkippedSummary {
		sumSkipped += n
	}
	if len(got.TargetRules)+sumSkipped != len(got.AllRules) {
		t.Errorf("invariant violated: %d (TargetRules) + %d (Skipped) != %d (AllRules)",
			len(got.TargetRules), sumSkipped, len(got.AllRules))
	}

	// FindingsFiles: 重複除去 + 順序保持 (alpha:auth/login.go → charlie の重複は無視 → charlie:logger/log.go)
	wantFiles := []string{"auth/login.go", "logger/log.go"}
	if !reflect.DeepEqual(got.FindingsFiles(), wantFiles) {
		t.Errorf("FindingsFiles() = %v, want %v (重複除去 + 順序保持)", got.FindingsFiles(), wantFiles)
	}

	// ReviewRunID / ReviewRunDir
	if got.ReviewRunID != runID {
		t.Errorf("ReviewRunID = %q, want %q", got.ReviewRunID, runID)
	}
	if got.ReviewRunDir != runDir {
		t.Errorf("ReviewRunDir = %q, want %q", got.ReviewRunDir, runDir)
	}
}

// TestLoad_EmptyDir は per-rule ファイルが 1 件もない (review-workflow が precheck で
// 落ちた等) ディレクトリで AllRules / TargetRules / SkippedSummary が空になることを検証する
// (Req 1.6 no_findings 経路の入力)。
func TestLoad_EmptyDir(t *testing.T) {
	t.Parallel()
	cwd, runID, _ := loaderTestEnv(t, "run-empty")

	got, err := Load(t.Context(), cwd, runID)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if len(got.AllRules) != 0 {
		t.Errorf("len(AllRules) = %d, want 0", len(got.AllRules))
	}
	if len(got.TargetRules) != 0 {
		t.Errorf("len(TargetRules) = %d, want 0", len(got.TargetRules))
	}
	if len(got.SkippedSummary) != 0 {
		t.Errorf("SkippedSummary = %v, want empty", got.SkippedSummary)
	}
	if got.FindingsFiles() == nil {
		t.Errorf("FindingsFiles() = nil, want empty []string (空でも non-nil 契約は維持しない: 0 件のとき空 slice)")
	}
	if len(got.FindingsFiles()) != 0 {
		t.Errorf("len(FindingsFiles()) = %d, want 0", len(got.FindingsFiles()))
	}
}

// TestLoad_OnlyResultJSONIsSkipped は result.json のみの入力で AllRules が 0 件になることを検証する。
// Loader は per-rule artifact ではない result.json をスキップする契約。
func TestLoad_OnlyResultJSONIsSkipped(t *testing.T) {
	t.Parallel()
	cwd, runID, runDir := loaderTestEnv(t, "run-only-result")
	writeRawJSON(t, runDir, "result", `{"workflow_run_id":"run-only-result"}`)

	got, err := Load(t.Context(), cwd, runID)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if len(got.AllRules) != 0 {
		t.Errorf("len(AllRules) = %d, want 0 (result.json はスキップ)", len(got.AllRules))
	}
}

// TestLoad_SchemaViolationDecodeError は壊れた JSON で LoaderSchemaError が返ることを検証する。
func TestLoad_SchemaViolationDecodeError(t *testing.T) {
	t.Parallel()
	cwd, runID, runDir := loaderTestEnv(t, "run-broken")
	writeRawJSON(t, runDir, "broken-rule", `{not a valid json}`)

	_, err := Load(t.Context(), cwd, runID)
	if err == nil {
		t.Fatalf("Load() error = nil, want non-nil (decode error)")
	}
	var schemaErr *LoaderSchemaError
	if !errors.As(err, &schemaErr) {
		t.Fatalf("Load() err = %T %v, want *LoaderSchemaError", err, err)
	}
	if schemaErr.File == "" {
		t.Errorf("LoaderSchemaError.File is empty")
	}
	if schemaErr.Cause == nil {
		t.Errorf("LoaderSchemaError.Cause is nil (decode error が wrap されていない)")
	}
}

// TestLoad_SchemaViolationMissingVerdict は verdict 必須フィールド欠落で LoaderSchemaError が返ることを検証する。
func TestLoad_SchemaViolationMissingVerdict(t *testing.T) {
	t.Parallel()
	cwd, runID, runDir := loaderTestEnv(t, "run-missing-verdict")
	// name はあるが verdict が空 (空文字 = JSON 上 "" / VerdictUnset)
	writeRawJSON(t, runDir, "no-verdict-rule", `{"name":"no-verdict-rule","verdict":""}`)

	_, err := Load(t.Context(), cwd, runID)
	if err == nil {
		t.Fatalf("Load() error = nil, want non-nil (missing verdict)")
	}
	var schemaErr *LoaderSchemaError
	if !errors.As(err, &schemaErr) {
		t.Fatalf("Load() err = %T %v, want *LoaderSchemaError", err, err)
	}
	if schemaErr.Field != requiredVerdictField {
		t.Errorf("LoaderSchemaError.Field = %q, want %q", schemaErr.Field, requiredVerdictField)
	}
}

// TestLoad_SchemaViolationMissingName は name 必須フィールド欠落で LoaderSchemaError が返ることを検証する。
func TestLoad_SchemaViolationMissingName(t *testing.T) {
	t.Parallel()
	cwd, runID, runDir := loaderTestEnv(t, "run-missing-name")
	writeRawJSON(t, runDir, "no-name-rule", `{"name":"","verdict":"no_findings"}`)

	_, err := Load(t.Context(), cwd, runID)
	if err == nil {
		t.Fatalf("Load() error = nil, want non-nil (missing name)")
	}
	var schemaErr *LoaderSchemaError
	if !errors.As(err, &schemaErr) {
		t.Fatalf("Load() err = %T %v, want *LoaderSchemaError", err, err)
	}
	if schemaErr.Field != requiredRuleNameField {
		t.Errorf("LoaderSchemaError.Field = %q, want %q", schemaErr.Field, requiredRuleNameField)
	}
}

// TestLoad_EmptyReviewRunID は reviewRunID 空文字で ErrEmptyReviewRunID が返ることを検証する。
// Workflow 層が ctx 注入も arg もなしで Load を呼ぶような実装ミスを構造的に検出する。
func TestLoad_EmptyReviewRunID(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	_, err := Load(t.Context(), cwd, "")
	if !errors.Is(err, ErrEmptyReviewRunID) {
		t.Errorf("Load() err = %v, want ErrEmptyReviewRunID", err)
	}
}

// TestLoad_ReviewRunDirNotFound は <cwd>/.rilll/review-workflow/<reviewRunID> 不在で
// ErrReviewRunDirNotFound が返ることを検証する (Precheck 経由しない統合経路の防御)。
func TestLoad_ReviewRunDirNotFound(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	_, err := Load(t.Context(), cwd, "nonexistent-run-id")
	if !errors.Is(err, ErrReviewRunDirNotFound) {
		t.Errorf("Load() err = %v, want ErrReviewRunDirNotFound", err)
	}
}

// TestLoad_ReviewRunDirIsFile は review run dir が file (ディレクトリでない) のとき
// ErrReviewRunDirNotFound が返ることを検証する (defensive)。
func TestLoad_ReviewRunDirIsFile(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	// `<cwd>/.rilll/review-workflow/file-run` を **ファイル** として作成する。
	parent := filepath.Join(cwd, ".rilll", "review-workflow")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", parent, err)
	}
	if err := os.WriteFile(filepath.Join(parent, "file-run"), []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := Load(t.Context(), cwd, "file-run")
	if !errors.Is(err, ErrReviewRunDirNotFound) {
		t.Errorf("Load() err = %v, want ErrReviewRunDirNotFound", err)
	}
}

// TestFindingsFiles_EmptyContractDefense は TargetRules.Findings の File が全て空文字の
// 契約違反入力でも FindingsFiles() が空 slice を返すことを検証する
// (review-apply design.md L640 fail-closed 前提と整合)。
//
// 通常 parser 側で File 空文字は failed に集約される (review/parse.go) が、
// Loader は per-rule JSON を直接 decode するため schema 上空文字を許容する経路がある。
// その場合でも scope guard は fail-closed (ErrOutOfScope) にすべきため、
// Loader 側は「空文字 File を除外して空集合を返す」責務を持つ。
func TestFindingsFiles_EmptyContractDefense(t *testing.T) {
	t.Parallel()
	cwd, runID, runDir := loaderTestEnv(t, "run-empty-files")
	writeRuleResultJSON(t, runDir, "alpha-rule", result.RuleResult{
		Name:            "alpha-rule",
		Verdict:         result.VerdictFindingsReported,
		FindingsPresent: true,
		Summary:         "with empty file findings",
		Findings: []result.Finding{
			{Severity: result.SeverityWarn, File: "", Location: "x:1", Message: "no file"},
			{Severity: result.SeverityInfo, File: "", Message: "still no file"},
		},
		StartedAt:  "2026-05-04T12:00:00Z",
		FinishedAt: "2026-05-04T12:00:01Z",
		SourceFile: "alpha-rule.yaml",
	})

	got, err := Load(t.Context(), cwd, runID)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if len(got.TargetRules) != 1 {
		t.Fatalf("len(TargetRules) = %d, want 1", len(got.TargetRules))
	}
	files := got.FindingsFiles()
	if len(files) != 0 {
		t.Errorf("FindingsFiles() = %v, want empty slice (空文字 File 除外契約)", files)
	}
}

// TestFindingsFiles_PreservesFirstOccurrenceOrder は FindingsFiles の重複除去が
// 「最初の出現順序」を保つこと (deterministic) を検証する。
// review-apply の scope guard allowlist が一意の deterministic な順序で組まれることが
// 後段 (Patch.ExtractAndGuard) のテスト容易性に寄与する。
func TestFindingsFiles_PreservesFirstOccurrenceOrder(t *testing.T) {
	t.Parallel()
	cwd, runID, runDir := loaderTestEnv(t, "run-order")

	// alpha が先 (a/x.go, b/y.go)、charlie が後 (b/y.go 重複, c/z.go) → ["a/x.go", "b/y.go", "c/z.go"]
	writeRuleResultJSON(t, runDir, "alpha-rule", result.RuleResult{
		Name:            "alpha-rule",
		Verdict:         result.VerdictFindingsReported,
		FindingsPresent: true,
		Summary:         "x",
		Findings: []result.Finding{
			{Severity: result.SeverityInfo, File: "a/x.go", Message: "1"},
			{Severity: result.SeverityInfo, File: "b/y.go", Message: "2"},
		},
		StartedAt: "t", FinishedAt: "t", SourceFile: "alpha-rule.yaml",
	})
	writeRuleResultJSON(t, runDir, "charlie-rule", result.RuleResult{
		Name:            "charlie-rule",
		Verdict:         result.VerdictFindingsReported,
		FindingsPresent: true,
		Summary:         "x",
		Findings: []result.Finding{
			{Severity: result.SeverityInfo, File: "b/y.go", Message: "dup"},
			{Severity: result.SeverityInfo, File: "c/z.go", Message: "3"},
		},
		StartedAt: "t", FinishedAt: "t", SourceFile: "charlie-rule.yaml",
	})

	got, err := Load(t.Context(), cwd, runID)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	want := []string{"a/x.go", "b/y.go", "c/z.go"}
	if !reflect.DeepEqual(got.FindingsFiles(), want) {
		t.Errorf("FindingsFiles() = %v, want %v", got.FindingsFiles(), want)
	}
}

// TestLoad_ReviewOverallSummaryPresent は <runDir>/result.json に overall_summary を持つ
// JSON が存在するときに LoadResult.ReviewOverallSummary が当該文字列を保持することを検証する
// (Req 7.1 / task 2.2 観察可能完了条件、best-effort 成功経路)。
//
// 前提:
//   - result.json は per-rule artifact ではなく review-workflow の全体サマリ。
//   - Loader は本ファイルを per-rule 列挙からは依然としてスキップする
//     (TestLoad_OnlyResultJSONIsSkipped と整合)。
//   - 同時に ReviewOverallSummary 取得経路として **読み込み対象** にもなる。
func TestLoad_ReviewOverallSummaryPresent(t *testing.T) {
	t.Parallel()
	cwd, runID, runDir := loaderTestEnv(t, "run-summary-present")

	// 全体サマリ (Summary schema 準拠) を書き出す。
	summary := result.Summary{
		WorkflowRunID:  runID,
		PRNumber:       42,
		OverallVerdict: result.VerdictFindingsReported,
		OverallSummary: "全体として 2 件の改善余地あり",
		StartedAt:      "2026-05-04T12:00:00Z",
		FinishedAt:     "2026-05-04T12:00:30Z",
	}
	body, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, loaderResultJSONFile), body, 0o644); err != nil {
		t.Fatalf("WriteFile result.json: %v", err)
	}

	got, err := Load(t.Context(), cwd, runID)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if got.ReviewOverallSummary != summary.OverallSummary {
		t.Errorf("ReviewOverallSummary = %q, want %q", got.ReviewOverallSummary, summary.OverallSummary)
	}
	// per-rule artifact が 0 件であり result.json は per-rule 列挙では依然スキップされていることを確認。
	if len(got.AllRules) != 0 {
		t.Errorf("len(AllRules) = %d, want 0 (result.json は per-rule 列挙対象外)", len(got.AllRules))
	}
}

// TestLoad_ReviewOverallSummaryFileNotFound は result.json が存在しないとき
// ReviewOverallSummary が空文字列となり Loader 全体が失敗しないことを検証する
// (Req 7.1 / task 2.2 best-effort: os.IsNotExist 経路)。
func TestLoad_ReviewOverallSummaryFileNotFound(t *testing.T) {
	t.Parallel()
	cwd, runID, _ := loaderTestEnv(t, "run-summary-missing")
	// 意図的に result.json を書き出さない。

	got, err := Load(t.Context(), cwd, runID)
	if err != nil {
		t.Fatalf("Load() unexpected error (best-effort で失敗にしてはならない): %v", err)
	}
	if got.ReviewOverallSummary != "" {
		t.Errorf("ReviewOverallSummary = %q, want \"\" (result.json 不在時は空文字列に倒れる契約)",
			got.ReviewOverallSummary)
	}
}

// TestLoad_ReviewOverallSummaryDecodeError は result.json が壊れた JSON のとき
// ReviewOverallSummary が空文字列となり Loader 全体が失敗しないことを検証する
// (Req 7.1 / task 2.2 best-effort: decode 失敗経路)。
//
// 注意: 既存 TestLoad_SchemaViolationDecodeError は per-rule JSON の decode 失敗で
// LoaderSchemaError を返すが、result.json の decode 失敗は best-effort で吸収する設計
// (per-rule artifact の schema 違反と review-workflow 全体サマリの破損は責務が異なる)。
func TestLoad_ReviewOverallSummaryDecodeError(t *testing.T) {
	t.Parallel()
	cwd, runID, runDir := loaderTestEnv(t, "run-summary-broken")
	if err := os.WriteFile(filepath.Join(runDir, loaderResultJSONFile),
		[]byte(`{this is not valid json`), 0o644); err != nil {
		t.Fatalf("WriteFile result.json: %v", err)
	}

	got, err := Load(t.Context(), cwd, runID)
	if err != nil {
		t.Fatalf("Load() unexpected error (best-effort で失敗にしてはならない): %v", err)
	}
	if got.ReviewOverallSummary != "" {
		t.Errorf("ReviewOverallSummary = %q, want \"\" (result.json decode 失敗時は空文字列に倒れる契約)",
			got.ReviewOverallSummary)
	}
}

// TestLoad_SkipsNonJSONFiles は `.json` 以外のファイルが Loader に無視されることを検証する。
// review-workflow が将来 `.txt` / `.md` を同ディレクトリに置いても Loader が壊れないこと。
func TestLoad_SkipsNonJSONFiles(t *testing.T) {
	t.Parallel()
	cwd, runID, runDir := loaderTestEnv(t, "run-mixed-ext")

	writeRuleResultJSON(t, runDir, "alpha-rule", result.RuleResult{
		Name:       "alpha-rule",
		Verdict:    result.VerdictNoFindings,
		Summary:    "ok",
		StartedAt:  "t",
		FinishedAt: "t",
		SourceFile: "alpha-rule.yaml",
	})
	if err := os.WriteFile(filepath.Join(runDir, "notes.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatalf("WriteFile notes.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "diff.patch"), []byte("ignored"), 0o644); err != nil {
		t.Fatalf("WriteFile diff.patch: %v", err)
	}

	got, err := Load(t.Context(), cwd, runID)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if len(got.AllRules) != 1 {
		t.Errorf("len(AllRules) = %d, want 1 (.txt / .patch は skip)", len(got.AllRules))
	}
}
