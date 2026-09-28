// Package learnings の本 file は agent-learnings 4 phase pipeline の 4 段目 (apply) を
// 担う純関数 `Apply` と関連型 (`ApplyInput` / `AppendedEntry` / `RejectedOperation` /
// `DedupSkippedOperation`) を提供する
// (tasks.md 3.1 / 3.2 / 3.3 / design.md §ApplyPhase / Req 1.1, 1.2, 1.5, 1.6, 1.7,
// 2.3, 3.1-3.7, 4.1, 5.1, 5.2, 5.3, 5.4, 5.5, 6.6, 7.6, 8.1, 8.2, 8.3, 8.5, 8.6)。
//
// 役割:
//
//   - Curator phase が出力した `[]CuratorOperation` を順に validate / dedup し、
//     `<cwd>/docs_dev/playbook/<category>.md` に section-aware insertion で bullet を追記する。
//   - 既存 section が同 topic に存在すれば末尾 bullet として追記、無ければ新規 section を
//     category file 末尾に append する (Req 3.1, 3.2)。
//   - 新規 category file は WikiSectionAppender 内の category header template で初期化される
//     (Req 3.3)。frontmatter は書かない (Req 1.5)。
//   - category-scoped jaccard dedup (Req 5.1): 同 category file 全 section の全 bullet を
//     pool として `internal/playbook/dedup.IsSimilar(entry, existing, 0.8)` で判定する。
//   - 全 operation 処理完了後に `RebuildIndex` を 1 回呼び出し、index.md を full rebuild する
//     (Req 4.1)。戻り値 `warnings` は `ApplyResult.IndexWarnings` に転写。
//   - artifact `<runDir>/apply-result.json` を atomic write (Req 8.6)。
//
// 設計契約:
//
//   - **op の排他カウント**: 各 op は appended / rejected / deduped のいずれか 1 つにのみ
//     計上される。
//   - **検証順序** (落ちたら次の op へ skip):
//     1. `op.Op == "add"` (Req 6.6) → 不一致は Rejected として記録 (`Reason="op-not-add"`)
//     2. `ValidCategory(op.Category)` (Req 5.3) → 不可は `VerdictRejectedInvalidCategory`
//     3. `ValidTopicName(op.Topic)` (Req 7.6) → 不可は `VerdictRejectedInvalidTopicName`
//     4. `paths_glob` 健全性 (Req 7.4):
//   - 空配列 → `VerdictRejectedEmptyPathsGlob`
//   - 単独 unbounded (`**` / `**/*` / `*`) → `VerdictRejectedUnboundedPathsGlob`
//     5. `summary` 検証 (Req 2.3): 空 / 改行含 / 100 字超過 → `Rejected` reason `summary-invalid`
//     6. `applies_to` 検証 (Req 2.3): 改行含 / 100 字超過 → `Rejected` reason `applies_to-invalid`
//     (空文字列は accept してプレースホルダ `(なし)` で書き出す)
//     7. category-scoped jaccard dedup → 0.8 以上は Deduped として記録
//   - **既存 section の applies_to / summary 行不変** (first-write-wins / Req 3.4): 対象
//     section が既存ならその heading 直下の `**適用条件:**` / `**summary:**` 行を上書きしない。
//     新規 section 作成時のみ op の `AppliesTo` / `Summary` を書き出す。
//   - **global Verdict 決定**: ApplyResult.Verdict は以下のロジックで決定 (Req 8.5):
//   - 0 ops → `applied-no-op`
//   - 1+ accepted → `applied`
//   - 0 accepted かつ全 rejected が同 reason → その reject verdict
//   - 0 accepted かつ rejected reason が混在 → `applied-no-op`
//   - 0 accepted かつ全 op が deduped → `applied-no-op` (副作用なし)
//   - **artifact atomic write**: prefetch.go の `writeJSONAtomic` を再利用。
//
// 注: wiki layout 移行 (本 task 3.1+3.2+3.3) で旧 `.claude/rules/playbook/<category>/<topic>.md`
// path を完全に廃し、新 `docs_dev/playbook/<category>.md` の section-aware insertion へ rewrite
// した。旧 path には一切 read / write を行わない (Req 8.1, 8.3)。
package learnings

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kazuki-oshino/rilll/internal/playbook/dedup"
)

// applyResultFilename は apply phase の最終結果を書き出す JSON artifact 名 (Req 8.6)。
const applyResultFilename = "apply-result.json"

// rejectReasonOpNotAdd は op.Op != "add" による reject の reason (Req 6.6)。
// MVP では `add` のみ受理し、`update` / `delete` は reject する。9 値 closed verdict
// には属さないが Rejected[].Reason は debug 容易性のために自由文字列を許容する設計。
const rejectReasonOpNotAdd = "op-not-add"

// rejectReasonSummaryInvalid は summary 検証失敗の reason (Req 2.3)。
// summary が空 / 改行含 / 100 字超過のいずれかで使用される。
// 9 値 closed verdict 集合外なので Verdict には昇格せず Rejected[].Reason のみに記録される。
const rejectReasonSummaryInvalid = "summary-invalid"

// rejectReasonAppliesToInvalid は applies_to 検証失敗の reason (Req 2.3)。
// applies_to が改行含 / 100 字超過のいずれかで使用される。空文字列は reject せず
// プレースホルダ `(なし)` に置換する経路を後段で処理する。
const rejectReasonAppliesToInvalid = "applies_to-invalid"

// playbookRootDirRel は wiki playbook root の cwd 相対 path (forward slash / Req 1.1)。
// 旧 `.claude/rules/playbook` から `docs_dev/playbook` に移行した (本 task 3.1)。
const playbookRootDirRel = "docs_dev/playbook"

// appliesToPlaceholder は applies_to が空文字列の op を accept したときに section に
// 書き出される `**適用条件:**` 行の本文。設計上 applies_to は ≤100 chars の自然言語であり、
// 空文字列を許容しないと「自然言語が無い」ことを表現できないため、空文字 → `(なし)` の
// 1 段プレースホルダ変換を Apply phase が行う (Req 2.3, 3.7)。
const appliesToPlaceholder = "(なし)"

// summaryMaxLength / appliesToMaxLength は Apply phase 内の op-level validation で使う
// 文字数上限 (Req 2.1 / 2.3)。schema.go の `curatorSummaryMaxLength` / `curatorAppliesToMaxLength`
// と等価だが、本 file で独立定数として保持して validation 経路の self-contained を保つ。
const summaryMaxLength = 100
const appliesToMaxLength = 100

// unboundedPathsGlobs は paths_glob 単独要素として禁止される全網羅 glob 集合 (Req 7.4)。
// これらの単独使用は frontmatter 経由で全 file 常時 load 化を引き起こすため拒否する。
var unboundedPathsGlobs = map[string]struct{}{
	"**":   {},
	"**/*": {},
	"*":    {},
}

// ApplyInput は Apply 関数の入力 (design.md §Apply §Service Interface)。
//
//   - CWD: project の cwd。`docs_dev/playbook/` の親として使用。
//   - Operations: Curator が出力した operation 列。空配列のとき副作用なしで
//     verdict=`applied-no-op` を返す。
//   - ParentRunID: artifact 内の trace 用 metadata。
type ApplyInput struct {
	CWD         string
	ParentRunID string
	Operations  []CuratorOperation
}

// AppendedEntry は実際に topic ファイル末尾に追記された 1 件分の記録。
//
// File は cwd 起点の relative path (`docs_dev/playbook/<category>.md`) で、
// 後段 analytics が file system を再 walk せずに対象を特定できる。
type AppendedEntry struct {
	Op       CuratorOperation `json:"op"`
	Category string           `json:"category"`
	Topic    string           `json:"topic"`
	Entry    string           `json:"entry"`
	File     string           `json:"file"`
}

// RejectedOperation は validation で reject された 1 件分の op の記録。
// Reason は 9 値 closed verdict のいずれか (4 値の reject path) または
// `rejectReasonOpNotAdd` / `rejectReasonSummaryInvalid` / `rejectReasonAppliesToInvalid`
// (closed verdict 集合外の自由文字列)。
type RejectedOperation struct {
	Op     CuratorOperation `json:"op"`
	Reason string           `json:"reason"`
	Detail string           `json:"detail,omitempty"`
}

// DedupSkippedOperation は category-scoped jaccard >= 0.8 で同 category 既存 bullet と
// 同義判定され追記が skip された op の記録 (Req 5.1)。
//
//   - SimilarToEntry: 重複と判定された既存 bullet 文字列。
//   - Jaccard: 算出 jaccard 値 (>= 0.8 の境界判定の事後検証用)。
//   - File: 重複先 bullet が属する file の cwd 相対 path (debug 用)。
type DedupSkippedOperation struct {
	Op             CuratorOperation `json:"op"`
	SimilarToEntry string           `json:"similar_to_entry"`
	Jaccard        float64          `json:"jaccard"`
	File           string           `json:"file"`
}

// nowRFC3339 は時刻 fixture を差し替え可能にするための関数変数 (test 容易性)。
var nowRFC3339 = func() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// Apply は curator operations を解釈し `<cwd>/docs_dev/playbook/<category>.md` に
// section-aware insertion で bullet を追記する。副作用および戻り値は package-level doc-comment
// 通り (apply-result.json + ファイル append + index.md rebuild)。
//
// 戻り値:
//   - 第一: `<runDir>/apply-result.json` の絶対パス
//   - 第二: ApplyResult 値 (途中状態を含む完全な結果)
//   - 第三: artifact 書き出しエラー / scan 失敗 / append 失敗 / RebuildIndex 失敗時に非 nil。
//     op-単位の rejection / dedup は ApplyResult.Rejected / Deduped に記録され error は返さない。
//     index rebuild の per-category warning は ApplyResult.IndexWarnings に記録され error は返さない。
func Apply(ctx context.Context, runDir string, in ApplyInput) (string, ApplyResult, error) {
	result := ApplyResult{
		Verdict:          VerdictApplied,
		Appended:         []AppendedEntry{},
		Rejected:         []RejectedOperation{},
		Deduped:          []DedupSkippedOperation{},
		NewTopicsCreated: []string{},
		StartedAt:        nowRFC3339(),
	}
	if err := ctx.Err(); err != nil {
		result.Verdict = VerdictAppliedNoOp
		result.FinishedAt = nowRFC3339()
		return "", result, err
	}

	// (Path A) Operations 空 → applied-no-op で early return (副作用なし)。
	if len(in.Operations) == 0 {
		result.Verdict = VerdictAppliedNoOp
		result.FinishedAt = nowRFC3339()
		path, err := writeApplyResult(runDir, result)
		if err != nil {
			return "", result, fmt.Errorf("learnings.Apply: write apply-result.json: %w", err)
		}
		return path, result, nil
	}

	// (Path B) Operations 非空 → 既存 playbook scan + op 反復。

	playbookRoot := filepath.Join(in.CWD, playbookRootDirRel)

	// 既存 wiki playbook を 1 度だけ scan して per-category bullet pool を構築する (Req 5.1)。
	categoryPool, err := scanLearningsPlaybook(playbookRoot)
	if err != nil {
		// scan 失敗は致命: filesystem permission 等で本 phase の操作が成立しない。
		return "", result, fmt.Errorf("learnings.Apply: scan playbook: %w", err)
	}

	// per-(category,topic) に「この run 内で既に append した bullet」を蓄積する map。
	// dedup pool は categoryPool[op.Category] + 当 run 内で同 category に append 済の bullets の
	// 和集合に対して判定する (連続 op の自己重複も検出 / Req 5.4)。
	// section-aware だが、dedup は section をまたがる category 全体で行うため (Req 5.1) per-category 単位で持つ。
	perRunAppended := make(map[string][]string) // key = category

	for _, op := range in.Operations {
		// (1) op == "add" 以外は reject (Req 6.6)
		if op.Op != "add" {
			result.Rejected = append(result.Rejected, RejectedOperation{
				Op:     op,
				Reason: rejectReasonOpNotAdd,
				Detail: fmt.Sprintf("op=%q (MVP では \"add\" のみ受理)", op.Op),
			})
			continue
		}

		// (2) ValidCategory (Req 5.3)
		if !ValidCategory(op.Category) {
			result.Rejected = append(result.Rejected, RejectedOperation{
				Op:     op,
				Reason: VerdictRejectedInvalidCategory,
				Detail: fmt.Sprintf("category=%q (closed enum 4 値外)", op.Category),
			})
			continue
		}

		// (3) ValidTopicName (Req 7.6)
		if !ValidTopicName(op.Topic) {
			result.Rejected = append(result.Rejected, RejectedOperation{
				Op:     op,
				Reason: VerdictRejectedInvalidTopicName,
				Detail: fmt.Sprintf("topic=%q (regex `^[a-z][a-z0-9-]*$` 違反)", op.Topic),
			})
			continue
		}

		// (4) paths_glob 健全性 (Req 7.4)
		// 4-1: 空配列 → rejected-empty-paths-glob
		if len(op.PathsGlob) == 0 {
			result.Rejected = append(result.Rejected, RejectedOperation{
				Op:     op,
				Reason: VerdictRejectedEmptyPathsGlob,
			})
			continue
		}
		// 4-2: 単独 unbounded → rejected-unbounded-paths-glob
		if hasUnboundedPathsGlob(op.PathsGlob) {
			result.Rejected = append(result.Rejected, RejectedOperation{
				Op:     op,
				Reason: VerdictRejectedUnboundedPathsGlob,
				Detail: fmt.Sprintf("paths_glob=%v は単独 unbounded (`**` / `**/*` / `*`) を含む", op.PathsGlob),
			})
			continue
		}

		// (5) summary 検証 (Req 2.3): 空 / 改行含 / 100 字超過 → reject
		if reason, ok := validateSummary(op.Summary); !ok {
			result.Rejected = append(result.Rejected, RejectedOperation{
				Op:     op,
				Reason: rejectReasonSummaryInvalid,
				Detail: reason,
			})
			continue
		}

		// (6) applies_to 検証 (Req 2.3): 改行含 / 100 字超過 → reject (空文字列は accept)
		if reason, ok := validateAppliesTo(op.AppliesTo); !ok {
			result.Rejected = append(result.Rejected, RejectedOperation{
				Op:     op,
				Reason: rejectReasonAppliesToInvalid,
				Detail: reason,
			})
			continue
		}

		// (7) category-scoped jaccard dedup (Req 5.1)
		// dedup pool = categoryPool[op.Category] + perRunAppended[op.Category]
		targetFileRel := filepath.Join(playbookRootDirRel, op.Category+".md")
		candidates := make([]learningsBulletEntry, 0,
			len(categoryPool[op.Category])+len(perRunAppended[op.Category]))
		candidates = append(candidates, categoryPool[op.Category]...)
		for _, e := range perRunAppended[op.Category] {
			candidates = append(candidates, learningsBulletEntry{file: targetFileRel, body: e})
		}

		if similar, jacc, file, found := findSimilarLearning(op.Entry, candidates, dedup.DefaultJaccardThreshold); found {
			result.Deduped = append(result.Deduped, DedupSkippedOperation{
				Op:             op,
				SimilarToEntry: similar,
				Jaccard:        jacc,
				File:           file,
			})
			continue
		}

		// (8) accept → 即時 AppendBullet 呼び出し。applies_to 空文字列はプレースホルダに置換。
		appliesTo := op.AppliesTo
		if appliesTo == "" {
			appliesTo = appliesToPlaceholder
		}
		// AppendBullet (writeFileAtomic) は親 directory が事前に存在することを要求するため、
		// 1 op 目で playbookRoot を冪等作成する。新規 layout では playbookRoot 直下に
		// category file (`docs_dev/playbook/<category>.md`) を平置きするため subdirectory は不要。
		if err := os.MkdirAll(playbookRoot, 0o755); err != nil {
			return "", result, fmt.Errorf("learnings.Apply: mkdir playbook root %q: %w", playbookRoot, err)
		}
		categoryFilePath := filepath.Join(playbookRoot, op.Category+".md")
		createdNewSection, appendErr := AppendBullet(AppendBulletInput{
			CategoryFilePath: categoryFilePath,
			Category:         op.Category,
			Topic:            op.Topic,
			AppliesTo:        appliesTo,
			Summary:          op.Summary,
			Entry:            op.Entry,
		})
		if appendErr != nil {
			return "", result, fmt.Errorf("learnings.Apply: AppendBullet %s/%s: %w", op.Category, op.Topic, appendErr)
		}

		perRunAppended[op.Category] = append(perRunAppended[op.Category], op.Entry)
		result.Appended = append(result.Appended, AppendedEntry{
			Op:       op,
			Category: op.Category,
			Topic:    op.Topic,
			Entry:    op.Entry,
			File:     filepath.ToSlash(targetFileRel),
		})

		if createdNewSection {
			result.NewTopicsCreated = append(result.NewTopicsCreated, op.Category+"/"+op.Topic)
		}
	}

	// (9) 全 operation 処理完了後に index.md を full rebuild する (Req 4.1)。
	// 1 op も accept されなかった (全 reject / 全 dedup) ケースでは playbookRoot 自体が
	// まだ存在しない可能性があり、その場合 RebuildIndex の writeFileAtomic は親 dir なしで失敗する。
	// 副作用ゼロの全 reject ケースは「playbook を変更しないことが正解」なので、accept 0 件 +
	// playbookRoot 不在のときは index 再構築を skip する (Req 4.1 「Apply phase が追加・変更した
	// とき更新」の趣旨と整合)。
	if len(result.Appended) > 0 {
		// 1+ accept → playbookRoot は (8) で作成済み。index を rebuild する。
		warnings, rebuildErr := RebuildIndex(RebuildIndexInput{PlaybookRoot: playbookRoot})
		if rebuildErr != nil {
			return "", result, fmt.Errorf("learnings.Apply: RebuildIndex: %w", rebuildErr)
		}
		if len(warnings) > 0 {
			result.IndexWarnings = warnings
		}
	} else if _, statErr := os.Stat(playbookRoot); statErr == nil {
		// 0 accept でも既存 playbookRoot が存在するなら index は最新化しておく
		// (過去 run の category file を反映する index を再生成)。
		warnings, rebuildErr := RebuildIndex(RebuildIndexInput{PlaybookRoot: playbookRoot})
		if rebuildErr != nil {
			return "", result, fmt.Errorf("learnings.Apply: RebuildIndex: %w", rebuildErr)
		}
		if len(warnings) > 0 {
			result.IndexWarnings = warnings
		}
	}

	// (10) global Verdict の決定。
	result.Verdict = decideApplyVerdict(result.Appended, result.Rejected, result.Deduped, len(in.Operations))

	// (11) artifact 書き出し (atomic write)。
	result.FinishedAt = nowRFC3339()
	path, werr := writeApplyResult(runDir, result)
	if werr != nil {
		return "", result, fmt.Errorf("learnings.Apply: write apply-result.json: %w", werr)
	}
	return path, result, nil
}

// learningsBulletEntry は dedup pool の 1 要素 (既存 bullet の本文 + 出所 file)。
type learningsBulletEntry struct {
	file string // cwd 相対 path (debug 用)
	body string
}

// scanLearningsPlaybook は `docs_dev/playbook/<category>.md` (4 file) を scan して
// per-category bullet pool を返す (Req 5.1)。`<category>` は closed enum 4 値のみ採用する。
// playbook root 自体が不在のときは空 pool を返し副作用なしで継続する。
// 各 category file が不在なら空 category として扱う (warning でも error でもない)。
// 各 category file は ParseCategoryFile で section 列に分解し、ExtractBullets で
// section をまたいだ全 bullet を平坦化する。
func scanLearningsPlaybook(playbookRoot string) (map[string][]learningsBulletEntry, error) {
	categoryPool := make(map[string][]learningsBulletEntry)

	// playbook root 自体が無い場合は空 pool で継続。
	if _, err := os.Stat(playbookRoot); err != nil {
		if os.IsNotExist(err) {
			return categoryPool, nil
		}
		return nil, fmt.Errorf("stat playbook root %q: %w", playbookRoot, err)
	}

	for _, category := range []string{
		CategoryFailureRecovery,
		CategorySuccessProcedure,
		CategoryAntipattern,
		CategoryToolPattern,
	} {
		path := filepath.Join(playbookRoot, category+".md")
		body, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read category file %q: %w", path, err)
		}
		parsed, perr := ParseCategoryFile(body)
		if perr != nil {
			return nil, fmt.Errorf("parse category file %q: %w", path, perr)
		}
		rel := filepath.Join(playbookRootDirRel, category+".md")
		for _, b := range ExtractBullets(parsed) {
			categoryPool[category] = append(categoryPool[category], learningsBulletEntry{
				file: filepath.ToSlash(rel),
				body: b,
			})
		}
	}
	return categoryPool, nil
}

// findSimilarLearning は candidates の中で entry と jaccard >= threshold を満たす最初の
// bullet を返す (file path 付き)。
//
// 戻り値: (matched body, jaccard 値, file path, 一致あり)。
func findSimilarLearning(entry string, candidates []learningsBulletEntry, threshold float64) (string, float64, string, bool) {
	for _, c := range candidates {
		j := dedup.JaccardDedup(entry, c.body)
		if j >= threshold {
			return c.body, j, c.file, true
		}
	}
	return "", 0, "", false
}

// hasUnboundedPathsGlob は paths_glob のいずれかの要素が unbounded (`**` / `**/*` / `*` 単独)
// かを判定する (Req 7.4)。前後 whitespace は trim 後判定。
func hasUnboundedPathsGlob(globs []string) bool {
	for _, g := range globs {
		if _, ok := unboundedPathsGlobs[strings.TrimSpace(g)]; ok {
			return true
		}
	}
	return false
}

// validateSummary は summary が op-level validation を満たすかを判定する (Req 2.3)。
//
// 空 / 改行含 / 100 字超過のいずれかで false を返し、第二戻り値に reject の detail 文を返す。
// 文字数判定は rune 単位 (utf-8 multi-byte の summary でも 100 chars 上限と整合)。
func validateSummary(s string) (string, bool) {
	if s == "" {
		return "summary が空 (Req 2.3)", false
	}
	if strings.ContainsAny(s, "\n\r") {
		return "summary に改行が含まれる (Req 2.3)", false
	}
	if n := lenRunes(s); n > summaryMaxLength {
		return fmt.Sprintf("summary が %d chars (上限 %d / Req 2.3)", n, summaryMaxLength), false
	}
	return "", true
}

// validateAppliesTo は applies_to が op-level validation を満たすかを判定する (Req 2.3)。
//
// 改行含 / 100 字超過のいずれかで false を返し detail を返す。空文字列は accept (後段で
// プレースホルダ `(なし)` に置換される / Req 3.7)。
func validateAppliesTo(s string) (string, bool) {
	if s == "" {
		// 空文字列は reject せずプレースホルダに後段で置換する。
		return "", true
	}
	if strings.ContainsAny(s, "\n\r") {
		return "applies_to に改行が含まれる (Req 2.3)", false
	}
	if n := lenRunes(s); n > appliesToMaxLength {
		return fmt.Sprintf("applies_to が %d chars (上限 %d / Req 2.3)", n, appliesToMaxLength), false
	}
	return "", true
}

// lenRunes は s の rune 数を返す (multi-byte 安全な文字数カウント)。
// strings.Count(s, "") - 1 で同等の挙動が得られるが、利用意図 (文字数) を明示するために
// 独立関数として切り出す。
func lenRunes(s string) int {
	return len([]rune(s))
}

// decideApplyVerdict は ApplyResult 結果から global Verdict を決定する純関数。
//
// 決定ロジック (design.md §State Management §Verdict closed set):
//   - opCount==0 → applied-no-op (curator 出力 0 件)
//   - len(appended) >= 1 → applied
//   - len(appended) == 0 かつ rejected が空でなく全 reject reason が同一 → その reject verdict
//   - それ以外 (混在 / 全 deduped 等) → applied-no-op
func decideApplyVerdict(appended []AppendedEntry, rejected []RejectedOperation, _ []DedupSkippedOperation, opCount int) string {
	if opCount == 0 {
		return VerdictAppliedNoOp
	}
	if len(appended) > 0 {
		return VerdictApplied
	}
	// 0 accepted: 全 reject reason が同一なら reject verdict、それ以外は applied-no-op
	if len(rejected) > 0 {
		first := rejected[0].Reason
		allSame := true
		for _, r := range rejected[1:] {
			if r.Reason != first {
				allSame = false
				break
			}
		}
		if allSame && IsValidVerdict(first) {
			return first
		}
	}
	return VerdictAppliedNoOp
}

// writeApplyResult は ApplyResult を `<runDir>/apply-result.json` に atomic write する。
func writeApplyResult(runDir string, result ApplyResult) (string, error) {
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir runDir %q: %w", runDir, err)
	}
	path := filepath.Join(runDir, applyResultFilename)
	if err := writeJSONAtomic(path, result); err != nil {
		return "", err
	}
	return path, nil
}
