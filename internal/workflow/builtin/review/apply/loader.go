// Package apply の本ファイルは task 2.1 / design.md §Loader を実装する。
//
// 役割: review-workflow が出力した per-rule 結果ファイル
// (`<cwd>/.rilll/review-workflow/<reviewRunID>/<rule-name>.json`) を全件ロードし、
// `findings_reported` のみを修正対象 (TargetRules) として抽出する。それ以外の
// verdict (`no_findings` / `skipped` / `failed` / `cancelled` / `precheck_failed` /
// `unset`) は SkippedSummary に集計し、利用者向けに「修正対象外」を明示できるようにする
// (Req 1.1, 1.4, 1.5, 1.6, 7.5)。
//
// design.md §Loader Implementation Notes:
//
//   - ディレクトリ存在検証 → `os.ReadDir` → `*.json` 各ファイルを `json.Unmarshal`
//     (default decode、`DisallowUnknownFields` は使わない、§research §5)。
//   - `result.json` ファイル自体はスキップ (`name == "result.json"`)。
//   - decode 失敗 / 必須フィールド欠落は `LoaderSchemaError` として返却する
//     (Workflow 層で `VerdictPreconditionFailed` に変換)。
//
// `LoadResult.FindingsFiles()` は `TargetRules[].Findings[].File` の和集合 (空文字列
// 除外・重複除去・順序保持) を返す。Patch.ExtractAndGuard (task 4.1) の `findingsFiles`
// 引数として Workflow 層から渡され、scope guard allowlist の基底情報となる
// (Req 3.5 / design.md §Patch fail-closed 前提)。
//
// 単方向依存原則 (tasks.md):
//   - 本タスクは `result.RuleResult` を import する両端契約 consumer 側であり、
//     review-workflow 側 schema が変更された場合は本 spec の改訂と同時に更新する
//     (Req 7.5 compile-time 検出)。
package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/result"
)

// reviewWorkflowDirName は review-workflow が per-rule 結果を出力する固定ディレクトリ名。
// `<cwd>/.rilll/review-workflow/<runID>/` の規約 (review-workflow design.md §Physical Schema /
// review-apply design.md §Loader Path Resolution) と整合する。
const reviewWorkflowDirName = "review-workflow"

// loaderResultJSONFile は review-workflow が同ディレクトリに書き出す全体サマリファイル名。
// Loader はこれをスキップする (per-rule artifact ではないため、design.md §Loader)。
const loaderResultJSONFile = "result.json"

// requiredRuleNameField / requiredVerdictField は LoaderSchemaError.Field に詰める
// 必須フィールド名定数。decode 後の必須性検証 (空文字 reject) に利用する。
const (
	requiredRuleNameField = "name"
	requiredVerdictField  = "verdict"
)

// LoadResult は Loader が返す per-rule ロード結果の集約 (design.md §Loader Service Interface)。
//
// 不変式:
//   - `len(TargetRules) <= len(AllRules)`
//   - `sum(SkippedSummary) + len(TargetRules) == len(AllRules)`
//   - `ReviewRunDir` は cleaned absolute path (filepath.Clean 済み)
//
// SkippedSummary は `findings_reported` 以外の per-rule verdict を verdict 別件数で集計する。
// VerdictUnset (空文字) や schema 上未知の値も検知用にバケットを設けて集計する責務がある
// (利用者表示で「想定外 verdict が混入している」シグナルになる)。
type LoadResult struct {
	// ReviewRunID は入力 review run の workflow_run_id (Workflow 層から受領した値)。
	ReviewRunID string

	// ReviewRunDir は per-rule 結果ファイルが格納されている絶対パス
	// (`<cwd>/.rilll/review-workflow/<reviewRunID>`)。
	ReviewRunDir string

	// AllRules は filter 前の全 per-rule ロード結果。
	// ファイル名昇順で deterministic に並ぶ (sort.Strings 後の os.ReadDir 結果順)。
	AllRules []result.RuleResult

	// TargetRules は `Verdict == VerdictFindingsReported` の rule のみを抽出した修正対象集合。
	// AllRules の subset であり、AllRules と同じ順序を保つ (filter のみ、reorder しない)。
	TargetRules []result.RuleResult

	// SkippedSummary は `findings_reported` 以外の rule を verdict 別に集計した map。
	// key は `result.Verdict` (空文字 / 未知値も bucket として保持)、value は件数。
	// 件数 0 のエントリは含めない (Map 上に出現しない)。
	SkippedSummary map[result.Verdict]int

	// ReviewOverallSummary は review-workflow が出力する全体サマリ
	// (`<ReviewRunDir>/result.json` の `Summary.OverallSummary`) を best-effort で読み込んだもの。
	//
	// 用途: previous-response-passing spec の PoC (review-apply propose loop) が
	// 後段 plan LLM 呼び出しに `PreviousResponseContent` として前置するための原文字列を保持する
	// (Req 7.1 / design.md Decision 3)。
	//
	// best-effort 経路:
	//   - result.json が存在しない (os.IsNotExist)、JSON decode に失敗、または
	//     `Summary.OverallSummary` が空文字列、のいずれの場合も空文字列 "" を採用し、
	//     Loader 全体は失敗させない。
	//   - 呼び出し側 (propose_loop) は空文字列のとき `IncludePreviousResponse=true` のままで
	//     `CallAgent` 側の no-op 分岐 (PreviousResponseContent == "") に倒れる契約。
	ReviewOverallSummary string
}

// LoaderSchemaError は per-rule JSON が両端契約スキーマに非互換のときに返す error
// (design.md §Loader Service Interface)。
//
// File: 違反した per-rule JSON の絶対パス。
// Field: 違反フィールド名 (例: "verdict" / "name")。decode 失敗 (フォーマット違反) の場合は空文字。
// Reason: 人間可読の違反説明。
// Cause: 元 error (decode error 等)。errors.Is / errors.As で型判別可能。
type LoaderSchemaError struct {
	File   string
	Field  string
	Reason string
	Cause  error
}

// Error は error interface 実装。File / Field / Reason / Cause を含む診断文字列を返す。
func (e *LoaderSchemaError) Error() string {
	parts := []string{fmt.Sprintf("apply.Loader: schema violation in %s", e.File)}
	if e.Field != "" {
		parts = append(parts, fmt.Sprintf("field=%q", e.Field))
	}
	if e.Reason != "" {
		parts = append(parts, e.Reason)
	}
	if e.Cause != nil {
		parts = append(parts, fmt.Sprintf("cause: %v", e.Cause))
	}
	return strings.Join(parts, ": ")
}

// Unwrap は errors.Unwrap で Cause を取り出すための実装。
func (e *LoaderSchemaError) Unwrap() error { return e.Cause }

// ErrEmptyReviewRunID は Load の reviewRunID 引数が空文字のときに返す sentinel。
// 呼び出し側 (Workflow 層 / Precheck task 2.2) は本 error を観測した場合
// `VerdictPreconditionFailed` に集約する責務を持つ。
var ErrEmptyReviewRunID = errors.New("apply.Loader: reviewRunID is empty")

// ErrReviewRunDirNotFound は `<cwd>/.rilll/review-workflow/<reviewRunID>` が存在しないときに返す sentinel。
// Precheck (task 2.2) と意味を揃えるため別 sentinel として export する。
var ErrReviewRunDirNotFound = errors.New("apply.Loader: review run directory not found")

// Load は per-rule 結果を全件ロードし findings_reported を抽出する
// (design.md §Loader Service Interface / Req 1.1, 1.4, 1.5, 1.6, 7.5)。
//
// 処理順序:
//  1. reviewRunID 空文字 → ErrEmptyReviewRunID (Precheck と意味を揃える)
//  2. `<cwd>/.rilll/review-workflow/<reviewRunID>` の存在検証 → ErrReviewRunDirNotFound
//  3. `os.ReadDir` で `*.json` ファイル列挙 (ファイル名昇順)。`result.json` はスキップ。
//  4. 各 JSON を `json.Unmarshal` で `result.RuleResult` に decode。失敗時は LoaderSchemaError。
//  5. 必須フィールド検証 (name / verdict 非空)。違反時は LoaderSchemaError。
//  6. verdict が `findings_reported` の rule を TargetRules に振り分け、それ以外は SkippedSummary に集計。
//  7. LoadResult を返す。
//
// 注意:
//   - Loader は副作用を持たない (ファイル書き出し / agent 呼び出しなし)。
//   - cwd は呼び出し側 (Workflow 層 task 5.1) が `filepath.Clean` 済みの絶対パスを渡す責務。
//     Loader 内でも防御的に `filepath.Clean` を適用する。
//   - reviewRunID の `..` / 絶対パス reject は Precheck (task 2.2) の責務であり、本 Loader では行わない
//     (二重検証になるが、Loader は Workflow 層から呼ばれる前提のため Precheck 通過済を信頼する設計)。
//     ただし、`os.Stat` での実体検証は本 Loader でも行う (Precheck 経由しない統合経路の防御)。
func Load(_ context.Context, cwd, reviewRunID string) (LoadResult, error) {
	// (1) reviewRunID 空文字 reject。
	if reviewRunID == "" {
		return LoadResult{}, ErrEmptyReviewRunID
	}

	// (2) review run dir 存在検証。`<cwd>/.rilll/review-workflow/<reviewRunID>`。
	runDir := filepath.Clean(filepath.Join(cwd, ".rilll", reviewWorkflowDirName, reviewRunID))
	info, err := os.Stat(runDir)
	if err != nil {
		if os.IsNotExist(err) {
			return LoadResult{}, fmt.Errorf("%w: %s", ErrReviewRunDirNotFound, runDir)
		}
		return LoadResult{}, fmt.Errorf("apply.Loader: stat %s: %w", runDir, err)
	}
	if !info.IsDir() {
		return LoadResult{}, fmt.Errorf("%w: %s is not a directory", ErrReviewRunDirNotFound, runDir)
	}

	// (3) ディレクトリ列挙 (`os.ReadDir` は名前順だが念のため sort.Strings で deterministic に)。
	entries, err := os.ReadDir(runDir)
	if err != nil {
		return LoadResult{}, fmt.Errorf("apply.Loader: readdir %s: %w", runDir, err)
	}
	jsonFiles := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == loaderResultJSONFile {
			// 全体サマリは skip (per-rule artifact ではない)。
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		jsonFiles = append(jsonFiles, name)
	}
	sort.Strings(jsonFiles)

	// (4)(5)(6) 各 JSON を decode + 必須検証 + verdict 振り分け。
	allRules := make([]result.RuleResult, 0, len(jsonFiles))
	targetRules := make([]result.RuleResult, 0, len(jsonFiles))
	skipped := make(map[result.Verdict]int)

	for _, name := range jsonFiles {
		path := filepath.Join(runDir, name)
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			// I/O エラーは schema violation ではなく一般 error として伝播 (Workflow 層で precheck_failed に集約)。
			return LoadResult{}, fmt.Errorf("apply.Loader: read %s: %w", path, rerr)
		}
		var rr result.RuleResult
		if uerr := json.Unmarshal(body, &rr); uerr != nil {
			return LoadResult{}, &LoaderSchemaError{
				File:   path,
				Reason: "JSON decode failed",
				Cause:  uerr,
			}
		}
		// 必須フィールド検証 (name / verdict)。
		if rr.Name == "" {
			return LoadResult{}, &LoaderSchemaError{
				File:   path,
				Field:  requiredRuleNameField,
				Reason: "name is empty",
			}
		}
		if rr.Verdict == "" {
			return LoadResult{}, &LoaderSchemaError{
				File:   path,
				Field:  requiredVerdictField,
				Reason: "verdict is empty",
			}
		}

		allRules = append(allRules, rr)
		if rr.Verdict == result.VerdictFindingsReported {
			targetRules = append(targetRules, rr)
		} else {
			skipped[rr.Verdict]++
		}
	}

	// (7) review-workflow が同ディレクトリに出力する全体サマリ `result.json` を
	// best-effort で読み込み、`Summary.OverallSummary` を取り出す
	// (previous-response-passing spec / Req 7.1 / design.md Decision 3)。
	//
	// 失敗経路 (ファイル不在 / decode 失敗 / OverallSummary 空) は空文字列に倒し、
	// Loader 全体は失敗させない (per-rule artifact の決定性とは独立した補助情報)。
	reviewOverallSummary := loadReviewOverallSummary(filepath.Join(runDir, loaderResultJSONFile))

	return LoadResult{
		ReviewRunID:          reviewRunID,
		ReviewRunDir:         runDir,
		AllRules:             allRules,
		TargetRules:          targetRules,
		SkippedSummary:       skipped,
		ReviewOverallSummary: reviewOverallSummary,
	}, nil
}

// loadReviewOverallSummary は <ReviewRunDir>/result.json を best-effort で読み込み、
// `Summary.OverallSummary` を取り出す内部 helper。
//
// best-effort 経路 (すべて空文字列 "" を返す):
//   - ファイル不在 (os.IsNotExist) — review-workflow が precheck 失敗等で全体サマリを書かなかったケース。
//   - その他の read I/O エラー — Loader 全体を失敗にすると per-rule 列挙の決定性まで巻き込むため吸収する。
//   - json.Unmarshal 失敗 — 全体サマリ schema が将来変更されたケースでも per-rule loader は壊さない。
//   - Summary.OverallSummary が空文字列 — review-workflow 側で意図的に空のケースも空文字列を返す。
//
// 純関数ではない (filesystem に依存) が、I/O エラーを呼び出し側に伝播せず string に決定的にマッピングする
// ことで、Loader 上位経路の単純性 (best-effort 契約) を保つ。
func loadReviewOverallSummary(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		// os.IsNotExist 含む全 I/O エラーを best-effort で吸収。
		return ""
	}
	var s result.Summary
	if err := json.Unmarshal(body, &s); err != nil {
		// decode 失敗も best-effort で吸収 (per-rule artifact ではないため Loader 全体を落とさない)。
		return ""
	}
	return s.OverallSummary
}

// FindingsFiles は TargetRules 配下の Finding.File の和集合を返す
// (空文字列除外・重複除去・順序保持)。
//
// 用途: Patch.ExtractAndGuard (task 4.1) の `findingsFiles` 引数として
// Workflow 層から渡される scope guard allowlist の基底情報
// (review-apply design.md §Patch / Req 3.5 第二関門)。
//
// 順序: TargetRules の順序 (= ファイル名昇順) でループし、
// その内側で Findings の出現順を保つ。重複は最初の出現を保持し、
// 後続の同 File は無視する (deterministic な順序を保証)。
//
// 空文字列の File は除外する (両端契約上は parser 側で空文字 reject の責務だが、
// Loader 側でも防御的に excludre して fail-closed の前提を崩さない)。
func (r LoadResult) FindingsFiles() []string {
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, rule := range r.TargetRules {
		for _, f := range rule.Findings {
			if f.File == "" {
				continue
			}
			if _, ok := seen[f.File]; ok {
				continue
			}
			seen[f.File] = struct{}{}
			out = append(out, f.File)
		}
	}
	return out
}
