// Package apply の本ファイルは task 3.1 / design.md §Prompt の Service Interface を実装する
// (Req 3.1, 3.2, 3.3, 4.1, 5.4)。
//
// 役割: review-apply-workflow の cycle (plan → diff → self-review) と最終 commit message を
// 構築するための 4 種 Render 関数 + 2 種 JSON Schema (`JSONSchemaForDiff` /
// `JSONSchemaForSelfReview`) を提供する。本ファイルは純関数のみで構成され、agent 呼び出しや
// I/O は行わない (副作用ゼロ契約 / Req 2.3)。
//
// 設計契約 (design.md §Prompt):
//   - 各 Render は preamble に **reviewRunID** と **対象 rule 名一覧** を必ず含める
//     (Req 3.3 / Req 5.4 「コミットメッセージに review run id および対象 rule 名」)。
//   - RenderPlan / RenderDiff / RenderSelfReview は加えて **attempt 番号** を preamble に
//     含める (regenerate ループでの試行追跡 / Req 4.1)。
//   - RenderDiff / RenderSelfReview は構造化出力 schema を package-level の
//     `JSONSchemaForDiff` / `JSONSchemaForSelfReview` から返す (両端契約: 同一インスタンスを返却)。
//   - cursor / cursor-sdk 経路では `provider.OutputFormat` が no-op (CLI が JSON schema を
//     解釈しない) ため、prompt 内テキスト指示にも JSON 期待形式 / enum 値を併記する
//     (best-effort 経路 / improve/patch.prompt.go と同方針)。
//
// 後方互換ガード:
//   - `PlanInput` / `DiffInput` / `SelfReviewInput` / `CommitMessageInput` のフィールド名は
//     後段 Workflow 層 (task 5.3, 5.4) と両端契約。フィールド追加は許容、削除 / rename は破壊的。
//   - `JSONSchemaForDiff` / `JSONSchemaForSelfReview` の properties / enum 値は固定。
//     値追加 / rename は spec 改訂を伴う破壊的変更扱い (Req 7.5 と整合)。
//   - prompt 文字列の細部 (改行・セクションヘッダ文言) はテストで substring match を要求する箇所のみが契約。
//     並べ替え / 補足追加は本テストでは検出されない設計 (review.BuildRulePrompt と同方針)。
package apply

import (
	"fmt"
	"strings"

	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/result"
)

// PlanInput は RenderPlan の入力 (design.md §Prompt Service Interface)。
//
// Attempt は 1 始まりの cycle 番号 (regenerate 時に増加、CycleRecord.Cycle と一致)。
// PreviousReview は attempt>=2 の regenerate 経路でのみ非 nil となり、直前 self-review が
// 返した reason を Render に注入することで「異なる方向性で再生成」を AI に要求する
// (Req 4.1 / Req 4.5 と整合)。attempt=1 では nil を渡す契約。
type PlanInput struct {
	// ReviewRunID は前段 review-workflow の run id (preamble 必須 token)。
	ReviewRunID string

	// Rules は `findings_reported` のみフィルタ済の修正対象 rule 集合 (Loader 出力)。
	// 順序は Loader が deterministic に並べた順 (ファイル名昇順) を保つ。
	Rules []result.RuleResult

	// Attempt は 1 始まりの cycle 番号。
	Attempt int

	// PreviousReview は直前 attempt の self-review 結果 (regenerate 時のみ非 nil)。
	// attempt=1 / 直前が無い経路では nil。
	PreviousReview *SelfReviewRecord

	// PreviousReviewPath は直前 attempt の self-review 結果が永続化された
	// `cycle-N-review.json` の絶対パス (Issue #46 input thinning)。
	// 非空のとき RenderPlan は本文に "- previous_reason_source: <path>" 行を
	// 追加し、長文 Reason は previousReviewReasonMaxChars で切り詰めて Source
	// パス参照に逃がす。空文字列のときは従来挙動 (Source 行省略 + Reason 全文埋め込み)。
	PreviousReviewPath string
}

// DiffInput は RenderDiff の入力 (design.md §Prompt Service Interface)。
type DiffInput struct {
	// ReviewRunID は前段 review-workflow の run id (preamble 必須 token)。
	ReviewRunID string

	// Rules は修正対象 rule 集合 (PlanInput と同一形式)。
	Rules []result.RuleResult

	// Plan は直前に生成した cycle-N-plan.md の内容 (markdown 全文)。
	// 空文字列の場合は AI に「plan が空であること」を明示しないため、Workflow 層が plan 生成成功を
	// 確認した上で渡す責務を持つ。
	Plan string

	// Attempt は 1 始まりの cycle 番号 (PlanInput.Attempt と一致する契約)。
	Attempt int
}

// SelfReviewInput は RenderSelfReview の入力 (design.md §Prompt Service Interface)。
//
// design.md L568-573 のフィールド集合に **加えて** Attempt を保持する。
// 自己レビューもまた cycle 番号を preamble に含める Req 4.1 を満たすために必要であり、
// design.md の最小集合からの拡張は許容範囲 (本ファイル冒頭の後方互換ガード参照)。
type SelfReviewInput struct {
	// ReviewRunID は前段 review-workflow の run id (preamble 必須 token)。
	ReviewRunID string

	// Rules は修正対象 rule 集合。
	Rules []result.RuleResult

	// Plan は直前に生成した plan markdown (DiffInput.Plan と同値)。
	Plan string

	// DiffBytes は LLM が直前に生成した unified diff の生バイト列。
	// Patch.ExtractAndGuard (task 4.1) の入力でもあり、ここでは self-review の判定材料として渡す。
	DiffBytes []byte

	// AppliedFiles は LLM が宣言した applied_files (Patch.ExtractAndGuard の declaredFiles)。
	// self-review preamble で「どのファイルを書き換えた diff か」を AI に明示するために含める。
	AppliedFiles []string

	// Attempt は 1 始まりの cycle 番号 (DiffInput.Attempt と一致する契約)。
	Attempt int
}

// CommitMessageInput は RenderCommitMessage の入力 (design.md §Prompt Service Interface)。
// commit は 1 review run = 1 commit (Req 5.7) のため attempt の概念を持たず、
// 該当フィールドは本構造体には含めない。
type CommitMessageInput struct {
	// ReviewRunID は前段 review-workflow の run id (commit メッセージ preamble 必須 / Req 5.4)。
	ReviewRunID string

	// Rules は修正対象 rule 集合 (commit メッセージに rule 名一覧を含めるため / Req 5.4)。
	Rules []result.RuleResult

	// AppliedFiles は実際に commit する変更ファイル集合 (Patch.TargetFiles 由来)。
	AppliedFiles []string
}

// JSONSchemaForDiff は plan を踏まえた multi-file 修正 diff を構造化出力させる schema。
// `{diff: string, applied_files: []string}` を必須とし、Patch.ExtractAndGuard (task 4.1)
// の self-consistency check (declaredFiles と diff 内 path 集合の完全一致) で利用する
// (Req 3.5 / design.md §Patch)。
//
// 設計判断:
//   - improve の `JSONSchemaForDiff` と同形に **加えて** `applied_files []string` を必須化する
//     (multi-file scope guard 用、design.md §Prompt L544)。
//   - additionalProperties: false で未定義フィールドを拒否し、両端契約の輪郭を保つ。
//   - cursor 経路では本値は使われない (CLI が OutputFormat を解釈しない)。
//     prompt 内のテキスト指示で best-effort 誘導する設計 (improve/patch.prompt.go と同方針)。
//
// 公開理由 (var):
//   - Workflow 層 (task 5.3) が `provider.AgentRequest.OutputFormat = JSONSchemaForDiff` として
//     直接参照できるよう exported var として公開する。
//   - RenderDiff の戻り値としても **同一インスタンス** を返すことで、test (3.1) で `==` 同一性検証が
//     可能になる (両端契約のシングルトン化)。
var JSONSchemaForDiff = &provider.OutputFormat{
	Type: "json_schema",
	Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"diff": map[string]any{
				"type":        "string",
				"description": "修正対象 file 集合に対する unified diff (git apply --check で適用可能な形式)。`diff --git a/<path> b/<path>` 行で始まる標準的な git unified diff を返す。",
			},
			"applied_files": map[string]any{
				"type":        "array",
				"description": "diff が変更する file の相対 path 集合 (cwd 起点)。Patch self-consistency check で diff 内 `+++ b/<path>` 集合と完全一致が要求される (Req 3.5)。",
				"items": map[string]any{
					"type": "string",
				},
			},
		},
		"required":             []string{"diff", "applied_files"},
		"additionalProperties": false,
	},
}

// JSONSchemaForSelfReview は self-review LLM の二値判定 + 理由を構造化出力させる schema。
// `{verdict: "accept"|"regenerate", reason: string}` を必須とし、Parse (task 3.2) で
// `SelfReviewRecord` に正規化する (Req 4.1 / 4.5)。
//
// 設計判断:
//   - verdict は closed set 二値 (accept / regenerate) を JSON Schema enum で強制する。
//     Parse 側で範囲外値を `ErrSelfReviewSchemaViolation` で reject する二重防御契約。
//   - reason は LLM の判定理由 (改行を含む自由記述可)、必須化することで「無理由 regenerate」の
//     状態を構造的に防ぐ。
//   - additionalProperties: false で未定義フィールドを拒否する。
var JSONSchemaForSelfReview = &provider.OutputFormat{
	Type: "json_schema",
	Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"verdict": map[string]any{
				"type":        "string",
				"enum":        []string{"accept", "regenerate"},
				"description": "self-review 判定: accept (apply に進む) / regenerate (再生成を要求)。closed set 二値。",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "判定理由 (改行を含む自由記述可)。utility / coverage / scope などの観点を簡潔に述べる。",
			},
		},
		"required":             []string{"verdict", "reason"},
		"additionalProperties": false,
	},
}

// RenderPlan は修正計画 markdown を生成するための prompt を組み立てる純関数 (Req 3.1, 3.3)。
//
// 出力構造:
//  1. preamble: review_run_id / target rule 名一覧 / attempt 番号
//  2. 各 rule の指摘 (Severity / File / Location / Message) を列挙
//  3. (regenerate 経路のみ) 直前 self-review の reason を inject
//  4. 出力形式指示 (markdown 形式で plan を返すよう指示)
//
// 副作用なし、決定的純関数 (review.BuildRulePrompt / improve.DefaultPromptTemplate.Render と同方針)。
func RenderPlan(in PlanInput) string {
	var b strings.Builder

	// (1) preamble
	writePreamble(&b, "修正計画 (plan) 生成", in.ReviewRunID, in.Rules, &in.Attempt)

	// (2) rule 別指摘の列挙
	writeRulesWithFindings(&b, in.Rules)

	// (3) regenerate context (attempt >= 2 で PreviousReview が非 nil のときのみ)
	if in.Attempt >= 2 && in.PreviousReview != nil {
		b.WriteString("# 直前試行の self-review (regenerate context)\n")
		b.WriteString(fmt.Sprintf("- previous_attempt: %d\n", in.PreviousReview.Attempt))
		b.WriteString(fmt.Sprintf("- previous_verdict: %s\n", string(in.PreviousReview.Verdict)))
		// Issue #46 input thinning: PreviousReviewPath が供給されているとき
		// Source パス参照行を追加。長文 reason は永続化済みファイルにあるため、
		// 必要なら agent が Read ツールで読みに行ける。
		if in.PreviousReviewPath != "" {
			b.WriteString(fmt.Sprintf("- previous_reason_source: %s\n", in.PreviousReviewPath))
		}
		b.WriteString("- previous_reason:\n")
		writeBlockEnsureNewline(&b, truncatePreviousReason(in.PreviousReview.Reason))
		b.WriteString("\n")
		b.WriteString("以上の reason を踏まえ、直前 attempt とは異なる方向性で修正計画を再構築してください。\n\n")
	}

	// (4) 出力形式指示
	b.WriteString("# 出力形式 (Req 3.1)\n")
	b.WriteString("修正計画を markdown 形式で返してください。コードブロックや前後の説明文は不要で、plan 本文のみを返却してください。\n")
	b.WriteString("plan には少なくとも以下の項目を含めてください:\n")
	b.WriteString("- 各指摘に対応する修正方針 (どの file のどの箇所をどう変えるか)\n")
	b.WriteString("- 影響範囲 (修正対象 file 一覧、副次的影響の有無)\n")
	b.WriteString("- 検証方法 (test 追加の有無、既存 test への影響)\n")

	return b.String()
}

// RenderDiff は修正計画 (plan) を踏まえて修正 diff (unified diff) を生成するための prompt と
// 構造化出力 schema を返す (Req 3.2, 3.3)。
//
// 戻り値:
//   - prompt 文字列: agent.CallAgent の prompt 引数にそのまま渡せる完成 prompt。
//   - schema: package-level の `JSONSchemaForDiff` (claude / codex 経路で
//     `provider.AgentRequest.OutputFormat` に渡す)。**同一インスタンス** を返す契約。
//
// 出力構造:
//  1. preamble: review_run_id / target rule 名一覧 / attempt 番号
//  2. 修正計画 (Plan markdown) の本文
//  3. 各 rule の指摘サマリ (file allowlist の根拠として明示)
//  4. 出力形式指示 (JSON `{diff, applied_files}`、cursor 経路向け fallback も併記)
func RenderDiff(in DiffInput) (string, *provider.OutputFormat) {
	var b strings.Builder

	// (1) preamble
	writePreamble(&b, "修正 diff 生成", in.ReviewRunID, in.Rules, &in.Attempt)

	// (2) 修正計画 (plan) 本文
	b.WriteString("# 修正計画 (plan)\n")
	if in.Plan == "" {
		b.WriteString("(plan が空です。Workflow 層の plan 生成成功を確認の上で再実行してください。)\n\n")
	} else {
		writeBlockEnsureNewline(&b, in.Plan)
		b.WriteString("\n")
	}

	// (3) rule 別指摘の列挙 (allowlist 根拠の再提示)
	writeRulesWithFindings(&b, in.Rules)

	// (4) 出力形式指示 (JSON schema 強制 + cursor 経路向け fallback)
	b.WriteString("# 出力形式 (Req 3.2 / 3.5 multi-file scope guard)\n")
	b.WriteString("応答は次の JSON 1 オブジェクトのみで返してください (説明文を前後に付けない):\n")
	b.WriteString("```json\n")
	b.WriteString(`{"diff": "<unified diff 本文>", "applied_files": ["<相対 path>", ...]}` + "\n")
	b.WriteString("```\n")
	b.WriteString("\n")
	b.WriteString("制約:\n")
	b.WriteString("- diff は `diff --git a/<path> b/<path>` 行で始まる標準的な git unified diff (`git apply --check` で適用可能な形式) を返してください。\n")
	b.WriteString("- applied_files は diff が実際に変更する file の相対 path 集合 (cwd 起点) を **全件** 列挙してください。diff 内 `+++ b/<path>` の集合と完全一致 (self-consistency) が後段 scope guard で要求されます。\n")
	b.WriteString("- 上記の指摘で `file` として現れた path のみが許容範囲です (allowlist)。allowlist 外の file を変更する diff は scope 違反として reject されます。\n")
	b.WriteString("- 絶対 path / `..` を含む path は禁止です (cwd 配下の relative path に限定)。\n")

	return b.String(), JSONSchemaForDiff
}

// RenderSelfReview は LLM 自身が直前 diff を accept / regenerate のいずれかで判定するための
// prompt と構造化出力 schema を返す (Req 4.1)。
//
// 戻り値:
//   - prompt 文字列: agent.CallAgent の prompt 引数にそのまま渡せる完成 prompt。
//   - schema: package-level の `JSONSchemaForSelfReview` (closed set 二値 enum + reason)。
//     **同一インスタンス** を返す契約。
//
// 出力構造:
//  1. preamble: review_run_id / target rule 名一覧 / attempt 番号
//  2. 修正計画 (plan) 本文
//  3. 修正 diff (unified diff fenced code block)
//  4. AppliedFiles 一覧 (review 観点で「変更範囲が妥当か」を判定する材料)
//  5. 出力形式指示 (JSON `{verdict, reason}`、enum {accept, regenerate} を明示)
func RenderSelfReview(in SelfReviewInput) (string, *provider.OutputFormat) {
	var b strings.Builder

	// (1) preamble
	writePreamble(&b, "自己レビュー (self-review) 判定", in.ReviewRunID, in.Rules, &in.Attempt)

	// (2) 修正計画 (plan)
	b.WriteString("# 修正計画 (plan)\n")
	if in.Plan == "" {
		b.WriteString("(plan が空です。)\n\n")
	} else {
		writeBlockEnsureNewline(&b, in.Plan)
		b.WriteString("\n")
	}

	// (3) 修正 diff
	b.WriteString("# 直前試行で生成された修正 diff\n")
	b.WriteString("```diff\n")
	if len(in.DiffBytes) == 0 {
		b.WriteString("(diff が空です。)\n")
	} else {
		b.WriteString(string(in.DiffBytes))
		if !strings.HasSuffix(string(in.DiffBytes), "\n") {
			b.WriteString("\n")
		}
	}
	b.WriteString("```\n\n")

	// (4) applied_files の列挙
	b.WriteString("# 変更対象 file 一覧 (applied_files)\n")
	if len(in.AppliedFiles) == 0 {
		b.WriteString("(applied_files が空です。scope guard 違反の可能性があります。)\n")
	} else {
		for _, f := range in.AppliedFiles {
			b.WriteString(fmt.Sprintf("- %s\n", f))
		}
	}
	b.WriteString("\n")

	// (5) 出力形式指示
	b.WriteString("# 出力形式 (Req 4.1 二値判定 closed set)\n")
	b.WriteString("応答は次の JSON 1 オブジェクトのみで返してください (説明文を前後に付けない):\n")
	b.WriteString("```json\n")
	b.WriteString(`{"verdict": "accept|regenerate", "reason": "<判定理由>"}` + "\n")
	b.WriteString("```\n")
	b.WriteString("\n")
	b.WriteString("制約:\n")
	b.WriteString("- verdict は closed set: \"accept\" (apply に進む) / \"regenerate\" (再生成を要求) の 2 値のみ許容。\n")
	b.WriteString("- reason は判定根拠 (utility / coverage / scope 違反の有無 等) を簡潔に記述してください。\n")
	b.WriteString("- regenerate を返す場合は、次 attempt で改善すべき点を reason に明示してください。\n")

	return b.String(), JSONSchemaForSelfReview
}

// RenderCommitMessage は accept された diff を `git commit` する際の commit メッセージを
// 生成するための prompt を返す (Req 5.4)。
//
// 出力構造:
//  1. preamble: review_run_id / target rule 名一覧 (attempt は不要 / 1 review run = 1 commit / Req 5.7)
//  2. 変更対象 file 一覧 (applied_files)
//  3. 各 rule の指摘サマリ (commit body の根拠として渡す)
//  4. 出力形式指示 (Conventional Commits 風 1 行 subject + body の形式を best-effort 誘導)
func RenderCommitMessage(in CommitMessageInput) string {
	var b strings.Builder

	// (1) preamble (attempt なし: 1 review run = 1 commit / Req 5.7)
	writePreamble(&b, "commit メッセージ生成", in.ReviewRunID, in.Rules, nil)

	// (2) 変更対象 file 一覧
	b.WriteString("# 変更対象 file (applied_files)\n")
	if len(in.AppliedFiles) == 0 {
		b.WriteString("(applied_files が空です。)\n")
	} else {
		for _, f := range in.AppliedFiles {
			b.WriteString(fmt.Sprintf("- %s\n", f))
		}
	}
	b.WriteString("\n")

	// (3) rule 別指摘サマリ
	writeRulesWithFindings(&b, in.Rules)

	// (4) 出力形式指示
	b.WriteString("# 出力形式 (Req 5.4)\n")
	b.WriteString("commit メッセージのみを返してください (前後の説明文不要)。書式:\n")
	b.WriteString("- 1 行目: subject (50 文字程度の簡潔な要約、conventional commits 形式が望ましい例: `fix(auth): ...`)\n")
	b.WriteString("- 2 行目: 空行\n")
	b.WriteString("- 3 行目以降: body (修正対象 rule 名一覧と review_run_id への言及を含めること)\n")
	b.WriteString(fmt.Sprintf("- body 内に `review_run_id: %s` の行を必ず含めてください (追跡用 / Req 5.4)。\n", in.ReviewRunID))

	return b.String()
}

// writePreamble は 4 種 Render 共通の preamble を b に書き出す internal helper。
//
// 出力構造 (Req 3.3 / 5.4):
//   - title 行 (例: "# 修正計画 (plan) 生成")
//   - review_run_id (preamble 必須 token)
//   - target_rules (rule 名一覧 / preamble 必須 token)
//   - attempt (Render 種別が attempt 概念を持つときのみ / `attempt: <N>` 形式)
//
// attempt が nil の場合 (RenderCommitMessage) は attempt 行を省略する。
func writePreamble(b *strings.Builder, title, reviewRunID string, rules []result.RuleResult, attempt *int) {
	b.WriteString("# ")
	b.WriteString(title)
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("- review_run_id: %s\n", reviewRunID))
	b.WriteString("- target_rules:\n")
	if len(rules) == 0 {
		b.WriteString("  (なし)\n")
	} else {
		for _, r := range rules {
			b.WriteString(fmt.Sprintf("  - %s\n", r.Name))
		}
	}
	if attempt != nil {
		// テストは `attempt: <N>` 形式の substring を要求する (prompt_test.go: attemptToken)。
		b.WriteString(fmt.Sprintf("- attempt: %d\n", *attempt))
	}
	b.WriteString("\n")
}

// writeRulesWithFindings は rule 別指摘サマリを b に書き出す internal helper。
// rule 名 / description / summary / 各 finding (severity / file / location / message) を
// markdown 形式で列挙する。RenderPlan / RenderDiff / RenderCommitMessage の 3 種で共有する。
func writeRulesWithFindings(b *strings.Builder, rules []result.RuleResult) {
	if len(rules) == 0 {
		b.WriteString("# 指摘 (なし)\n\n")
		return
	}
	b.WriteString("# 指摘 (各 rule の findings)\n")
	for _, r := range rules {
		b.WriteString(fmt.Sprintf("## %s\n", r.Name))
		if r.Description != "" {
			b.WriteString(fmt.Sprintf("- description: %s\n", r.Description))
		}
		if r.Summary != "" {
			b.WriteString(fmt.Sprintf("- summary: %s\n", r.Summary))
		}
		if len(r.Findings) == 0 {
			b.WriteString("- findings: (なし)\n\n")
			continue
		}
		b.WriteString("- findings:\n")
		for _, f := range r.Findings {
			b.WriteString(fmt.Sprintf("  - severity=%s file=%s", string(f.Severity), f.File))
			if f.Location != "" {
				b.WriteString(fmt.Sprintf(" location=%s", f.Location))
			}
			b.WriteString("\n")
			if f.Message != "" {
				b.WriteString("    message:\n")
				// message を 4 空白インデントの blockquote 風に書き出す (改行込み message も読める)。
				for _, line := range strings.Split(strings.TrimRight(f.Message, "\n"), "\n") {
					b.WriteString("      ")
					b.WriteString(line)
					b.WriteString("\n")
				}
			}
		}
		b.WriteString("\n")
	}
}

// writeBlockEnsureNewline は s を b に書き出し、末尾改行が無ければ補う internal helper。
// 自由文ブロック (plan / reason 等) を後続セクションと視覚的に分離するために使用する
// (review.writeBlockEnsureNewline と同形)。
func writeBlockEnsureNewline(b *strings.Builder, s string) {
	b.WriteString(s)
	if !strings.HasSuffix(s, "\n") {
		b.WriteString("\n")
	}
}

// previousReviewReasonMaxChars は RenderPlan が直前 self-review の Reason 本文を
// 切り詰めるときの上限文字数 (rune 単位)。
//
// `inquiryloop.previousAnswerMaxChars=2000` と同様に、プロンプトへ含める本文量を制限する。
// 永続化済み `cycle-N-review.json` への Source パス参照を併記することで、
// 長文 reason 全文を prompt に流し込まず stream_idle_timeout の構造的リスクを下げる。
// 値が 0 以下のときは切り詰めを無効化する (truncatePreviousReason 側で素通し)。
const previousReviewReasonMaxChars = 2000

// truncatePreviousReason は long reason を previousReviewReasonMaxChars (rune 単位) に
// 切り詰める。rune 境界で切るためマルチバイト文字 (日本語) でも文字途中で切れない。
//
// 上限以下ならそのまま返す。超えるときは先頭 N rune + 改行 +
// "...(truncated; full content at Source path)" マーカを末尾に付ける
// (`inquiryloop.truncateForResume` と同形)。
func truncatePreviousReason(s string) string {
	if previousReviewReasonMaxChars <= 0 || s == "" {
		return s
	}
	runes := []rune(s)
	if len(runes) <= previousReviewReasonMaxChars {
		return s
	}
	return string(runes[:previousReviewReasonMaxChars]) + "\n\n...(truncated; full content at Source path)"
}
