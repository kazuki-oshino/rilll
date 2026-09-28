// Package review は review-workflow の per-rule 実行で利用する prompt 構築・出力 schema
// 定義・並行 executor 等を提供する (design.md §workflow/builtin/review domain)。
//
// 本ファイルは 1-call combined applies_when + review prompt の構築 (`BuildRulePrompt`)
// と claude / codex 経路の出力構造強制用 JSON schema (`JSONSchemaForReview`) を実装する
// (tasks.md 2.1 / design.md §review.Prompt / Req 2.5, 5.1, 5.3, 5.4)。
//
// 公開契約:
//
//   - `BuildRulePrompt(rule, prMeta, diff)` は副作用なしの純関数。同一入力で常に同一文字列を
//     返し、テスト容易性と決定性を担保する (provider 依存ロジックを持たないことで cursor /
//     claude / codex の全経路で 1 つの prompt 文字列を共有する設計、design.md §review.Prompt)。
//   - 出力末尾には JSON 形式での応答指示を必ず付与する。cursor (CLI 経由で OutputFormat が
//     no-op となる現実) でも prompt 内指示だけで applied/skip 分岐を表現できる契約。
//   - claude / codex 経路ではこれに加えて `JSONSchemaForReview` を `provider.AgentRequest.OutputFormat`
//     に渡し、構造化出力を二重に強制する (executor 側の責務、本ファイルでは schema 定義のみ提供)。
//
// 後方互換ガード:
//
//   - prompt 文字列の細部 (改行 / 文言) はテストで substring match を要求する箇所のみが契約。
//     セクションヘッダ等の追加・並べ替えは互換扱い (テストが substring 検査) で行える。
//   - JSONSchemaForReview のフィールド名・型・enum 値は両端契約として固定。値追加 (severity の
//     新規値追加など) は破壊的変更扱いで、result.Severity との同時改訂を要する。
package review

import (
	"fmt"
	"strings"

	"github.com/kazuki-oshino/rilll/internal/gh"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/rules"
)

// JSONSchemaForReview は claude / codex 経路で `provider.AgentRequest.OutputFormat` に
// 渡す JSON Schema 定義 (design.md §review.Parser Implementation Notes / Req 5.4)。
//
// 構造 (両端契約 design.md §review.Parser L520-534):
//
//	{
//	  "applied":           bool,    // applies_when 自己判定 (true=review 実行 / false=skip)
//	  "skip_reason":       string,  // applied=false のとき必須
//	  "findings_present":  bool,    // applied=true のとき有効、findings 1 件以上で true
//	  "summary":           string,  // applied=true のとき必須 (1-3 文)
//	  "details": [                  // 個別指摘配列、空配列許容
//	    {
//	      "severity": "info"|"warn"|"error",  // closed set
//	      "file":     string,                  // 必須 (review-apply scope guard allowlist)
//	      "location": string,                  // 任意 (補助、行番号等)
//	      "message":  string                   // 必須
//	    }
//	  ]
//	}
//
// 設計判断:
//   - `required` は `applied` のみ。`skip_reason` / `summary` は applied 値に応じて
//     parser 側で必須性を validate する設計とし、JSON schema レベルでは optional にする
//     (片方だけ必須化が JSON schema 単体では表現できないため)。
//   - `additionalProperties: false` で未定義フィールドを拒否し、両端契約の輪郭を保つ。
//   - cursor 経路では本値は使われない (CLI が OutputFormat を解釈しない)。prompt 内の
//     指示文だけで JSON 出力を best-effort 誘導する設計。
//
// 本 var を package level に集約する理由:
//   - tasks.md 2.1 観察可能完了条件「JSONSchemaForReview 定数 (`improve` の `JSONSchemaForDiff`
//     と同じ位置付け) を定義し、…claude/codex provider 経由で `OutputFormat` に渡せる形にする」
//     を構造的に満たすため。
//   - executor (task 2.4) が `provider.AgentRequest{OutputFormat: review.JSONSchemaForReview}`
//     として直接参照できるよう exported var として公開する。
var JSONSchemaForReview = &provider.OutputFormat{
	Type: "json_schema",
	Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"applied": map[string]any{
				"type":        "boolean",
				"description": "applies_when 自己判定の結果。true=review を実行 / false=skip。",
			},
			"skip_reason": map[string]any{
				"type":        "string",
				"description": "applied=false のとき必須となる skip 理由。applied=true のときは空文字。",
			},
			"findings_present": map[string]any{
				"type":        "boolean",
				"description": "review 結果に指摘が 1 件以上存在するかどうか。applied=false のときは false。",
			},
			"summary": map[string]any{
				"type":        "string",
				"description": "applied=true のとき必須となる人間可読要約 (1-3 文が望ましい)。",
			},
			"details": map[string]any{
				"type":        "array",
				"description": "個別指摘の配列。findings_present=true でも空配列でよい (summary のみ提示する場合)。",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"severity": map[string]any{
							"type":        "string",
							"enum":        []string{"info", "warn", "error"},
							"description": "指摘の重要度。closed set: info / warn / error。",
						},
						"file": map[string]any{
							"type":        "string",
							"description": "指摘対象ファイルの相対パス (例: \"auth/login.go\")。必須。後段 review-apply の scope guard allowlist に使われるため pure な path を返すこと (行番号は location 側へ)。",
						},
						"location": map[string]any{
							"type":        "string",
							"description": "指摘箇所の自由文表現 (例: \"auth/login.go:42\")。任意 (行番号等の補助情報)。",
						},
						"message": map[string]any{
							"type":        "string",
							"description": "指摘内容の人間可読テキスト。",
						},
					},
					"required":             []string{"severity", "file", "message"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"applied"},
		"additionalProperties": false,
	},
}

// BuildRulePrompt は rule の applies_when + review prompt と PR メタデータ + diff を
// 連結した 1-call combined prompt 文字列を返す純関数 (design.md §review.Prompt /
// Req 2.5, 5.1, 5.3)。
//
// 出力構造 (上から順):
//  1. レビュー rule metadata (name / description)
//  2. applies_when 自然文 (適用判断用、agent が自己判定)
//  3. review prompt 本文 (適用と判断した場合のみ実行)
//  4. PR メタデータ (number / title / additions / deletions / files 一覧)
//  5. PR 本文 (body) — 空のときは "(空)"
//  6. PR diff — fenced code block (` ```diff ... ``` `)
//  7. 出力形式指示 — JSON schema (両端契約) と applied=true / false の例示
//
// (7) の指示は以下を満たす:
//   - cursor の自由文経路でも JSON 1 オブジェクトを返す指示を明文化する
//   - applied=true / applied=false の両分岐を例示し、agent が分岐を誤らないようにする
//   - severity の closed set (info / warn / error) を明示する
//
// 引数:
//   - rule: review-workflow が `.rilll-config/review-rules/<rule>.yaml` から loader 経由で
//     読み込んだ単一 rule。
//   - prMeta: `internal/gh.ViewPR` 由来の PR メタデータ。
//   - diff: `internal/gh.DiffPR` 由来の unified diff (空文字列も許容)。
//
// 戻り値: agent.CallAgent の prompt 引数にそのまま渡せる完成 prompt 文字列。
func BuildRulePrompt(rule rules.Rule, prMeta gh.PRMeta, diff string) string {
	var b strings.Builder

	// (1) レビュー rule metadata
	b.WriteString("# レビュー rule\n")
	b.WriteString(fmt.Sprintf("- name: %s\n", rule.Name))
	if rule.Description != "" {
		b.WriteString(fmt.Sprintf("- description: %s\n", rule.Description))
	}
	b.WriteString("\n")

	// (2) applies_when 自然文 (適用判断用)
	b.WriteString("# 適用条件 (applies_when)\n")
	b.WriteString("以下の条件に PR が該当するかをまず判定してください。該当しなければ review prompt は実行せず skip と返してください。\n\n")
	writeBlockEnsureNewline(&b, rule.AppliesWhen)
	b.WriteString("\n")

	// (3) review prompt 本文 (適用時のみ実行)
	b.WriteString("# レビュー指示 (適用と判定したときのみ実行)\n")
	writeBlockEnsureNewline(&b, rule.Prompt)
	b.WriteString("\n")

	// (4) PR メタデータ
	b.WriteString("# PR メタデータ\n")
	b.WriteString(fmt.Sprintf("- number: %d\n", prMeta.Number))
	b.WriteString(fmt.Sprintf("- title: %s\n", prMeta.Title))
	b.WriteString(fmt.Sprintf("- additions: %d\n", prMeta.Additions))
	b.WriteString(fmt.Sprintf("- deletions: %d\n", prMeta.Deletions))
	b.WriteString("- files:\n")
	if len(prMeta.Files) == 0 {
		b.WriteString("  (なし)\n")
	} else {
		for _, f := range prMeta.Files {
			b.WriteString(fmt.Sprintf("  - %s\n", f))
		}
	}
	b.WriteString("\n")

	// (5) PR 本文 (body)
	b.WriteString("# PR 本文 (body)\n")
	if prMeta.Body == "" {
		b.WriteString("(空)\n")
	} else {
		writeBlockEnsureNewline(&b, prMeta.Body)
	}
	b.WriteString("\n")

	// (6) PR diff
	b.WriteString("# PR diff\n")
	b.WriteString("```diff\n")
	b.WriteString(diff)
	if !strings.HasSuffix(diff, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("```\n\n")

	// (7) 出力形式指示 (両端契約 / Req 5.4 / cursor 経路向け明示分岐)
	b.WriteString("# 出力形式 (両端契約: applied/skip_reason/findings_present/summary/details)\n")
	b.WriteString("応答は次の JSON schema に従った 1 つの JSON オブジェクトのみで返してください (説明文を前後に付けない)。\n")
	b.WriteString("\n")
	b.WriteString("適用条件に該当しない場合は次の形を返してください:\n")
	b.WriteString("```json\n")
	b.WriteString(`{"applied": false, "skip_reason": "<簡潔な skip 理由>"}` + "\n")
	b.WriteString("```\n")
	b.WriteString("\n")
	b.WriteString("適用条件に該当する場合は次の形を返してください:\n")
	b.WriteString("```json\n")
	b.WriteString(`{"applied": true, "findings_present": <true|false>, "summary": "<1-3 文の要約>", "details": [{"severity": "info|warn|error", "file": "<相対パス>", "location": "<任意 path:line>", "message": "<指摘内容>"}]}` + "\n")
	b.WriteString("```\n")
	b.WriteString("\n")
	b.WriteString("制約:\n")
	b.WriteString("- severity は \"info\" / \"warn\" / \"error\" の closed set のみ許容。\n")
	b.WriteString("- file は **必須** (相対パス、例: \"auth/login.go\")。後段 review-apply-workflow の scope guard allowlist に使われるため、行番号などを含めず pure なパスを返してください。\n")
	b.WriteString("- location は任意 (省略可、行番号等の補助情報)。message は必須。\n")
	b.WriteString("- details は空配列でも構いません (summary のみで指摘を表現するパターンを許容)。\n")
	b.WriteString("- findings_present=false のときは details を空配列にしてください。\n")

	return b.String()
}

// writeBlockEnsureNewline は s を b に書き出し、末尾改行が無ければ補う。
// rule.AppliesWhen / rule.Prompt / PR.Body のような自由文を後続セクションと
// 視覚的に分離するための共通ヘルパ。
func writeBlockEnsureNewline(b *strings.Builder, s string) {
	b.WriteString(s)
	if !strings.HasSuffix(s, "\n") {
		b.WriteString("\n")
	}
}
