// Package learnings の本ファイルは Reflector / Curator phase の LLM 出力を
// `provider.OutputFormat{Type:"json_schema"}` で固定するための JSON Schema 定義
// (`JSONSchemaForReflector` / `JSONSchemaForCurator`) と、Curator → Apply phase 間で
// 共有される `CuratorOperation` 型を提供する (task 6.5 / design.md §Data Contracts &
// Integration / Req 4.4 / 6.2 / 6.4)。
//
// 設計契約:
//
//   - **`distiller.JSONSchemaForReflector` と同パターン**: `Type:"json_schema"` /
//     `Schema: map[string]any{...}` / `additionalProperties:false` を全 object 階層で適用し、
//     LLM 出力を厳密な JSON にロックする (Req 4.4 / 6.2 の純 JSON 出力契約と整合)。
//   - **API Contract の SSoT**: design.md §Data Contracts & Integration の
//     「Reflector の per-session response schema」「Curator の operations response schema」を
//     SSoT とし、必須フィールド / 型 / enum / maxLength を本ファイル内で機械的に固定する。
//     skill 側 (`.agents/skills/agent-learnings-curator/rules/{reflector,curator}.md`) も
//     同 schema を ```json fenced block で併記し、両端契約は schema_test.go の
//     `TestSkillContractIntegrity_*` (skill 配置時に assert / 未配置時 skip seam) で保つ。
//   - **closed enum 4 値** (`failure-recovery` / `success-procedure` / `antipattern` /
//     `tool-pattern`) は category.go の `Category*` 定数を SSoT として参照する
//     (重複定義禁止)。
//   - **関数 (function) として export**: tasks.md 6.5 が
//     `JSONSchemaForReflector() *provider.OutputFormat` を要求しているため関数として export
//     する (毎回新しい map を構築する純関数)。これにより呼び出し側が schema を mutate しても
//     他コードに影響を残さない安全側の API になっている。
//   - **CGO 非依存**: 標準 `map[string]any` のみで構築し、新規依存追加なし。
//
// 関連 task:
//   - task 7.2 (Reflector phase) は `OutputFormat=JSONSchemaForReflector()` を
//     `agent.CallAgent` opts に渡して per-session response を JSON 強制する。
//   - task 7.3 (Curator phase) は `OutputFormat=JSONSchemaForCurator()` で curator response を
//     強制し、抽出した operations を `[]CuratorOperation` として task 7.4 (Apply) に渡す。
package learnings

import (
	"github.com/kazuki-oshino/rilll/internal/provider"
)

// curatorEntryMaxLength は Curator schema の `operations[].entry` に課す
// 1 行 320 chars 上限 (design.md API Contract — Curator / requirements.md Req 6.2)。
//
// 320 chars は playbook の bullet 1 行分の運用上限であり、超過した entry は
// LLM 出力段階で truncate されるか rejected として扱う (apply phase 側で
// 追加検証されることは task 7.4 で分担)。本定数は schema 内 `maxLength` 値の
// SSoT として import 経由で参照可能にしておく (将来 task 7.4 から再利用)。
const curatorEntryMaxLength = 320

// reflectorTextMaxLength は Reflector schema の `learnings[].context` /
// `learnings[].insight` に課す 512 chars 上限 (design.md L855-856 / Req 4.4 形状)。
//
// design.md §Storage characteristics で「context 512 + insight 512」が 1 行あたり想定
// サイズの根拠となっているため、両 field とも同値を使う。本定数は schema 内 `maxLength`
// 値の SSoT として import 経由で参照可能にしておく。
const reflectorTextMaxLength = 512

// reflectorPathsGlobMaxItems は Reflector schema の `learnings[].paths_glob` 配列に課す
// 最大 5 entries の上限 (design.md L857 「≤ 5 entries」/ Req 5.3 paths_glob ≤ 5)。
//
// learnings table の Recorder validation も同値で reject するため (Req 5.3)、本定数を
// SSoT として両端 (schema / Recorder) で参照することが望ましい (Recorder 側の参照は
// task 7.4 / 既存 akashic Recorder の責務で別途扱う)。
const reflectorPathsGlobMaxItems = 5

// pathsGlobMinItems は Reflector / Curator schema の `paths_glob` 配列に課す最小 1 entry の
// 下限 (skill SSoT と整合 / Req 7.4 で空配列は apply phase でも reject される)。
// LLM 側で空配列を出すこと自体を schema レベルで禁じる契約。
const pathsGlobMinItems = 1

// curatorPathsGlobMaxItems は Curator schema の `operations[].paths_glob` 配列に課す
// 最大 5 entries の上限 (skill SSoT と整合)。reflector と同値だが意味的に独立な定数として
// 切り出し、将来の値変更に対して片側のみ更新可能にする (両端契約テストが SSoT 不一致を検出)。
const curatorPathsGlobMaxItems = 5

// curatorTopicRationaleMaxLength は Curator schema の `operations[].topic_rationale` に課す
// 240 chars 上限。新規 topic 作成 operation のみで使われる optional metadata。
// 既存 topic を流用する operation では省略可 (omitempty)。
const curatorTopicRationaleMaxLength = 240

// curatorSummaryMaxLength は Curator schema の `operations[].summary` に課す
// 100 chars 上限 (Req 2.1 / design.md §Data Models CuratorOperation Schema)。
//
// summary は wiki index の各 entry に付ける 1 行説明として用いられ、
// Apply phase が section heading 直下に `**summary:** <summary>` 行として書き出す。
// 100 chars 上限は index.md の topic 一覧 (`- [<topic>](./<cat>.md#<topic>) — <summary>`)
// が 1 行に収まる運用上限。改行混入は Apply phase の op-level validation で reject される
// (本 schema は maxLength のみを固定し、改行 / 空文字判定は Apply phase の責務)。
const curatorSummaryMaxLength = 100

// curatorAppliesToMaxLength は Curator schema の `operations[].applies_to` に課す
// 100 chars 上限 (Req 2.1 / design.md §Data Models CuratorOperation Schema)。
//
// applies_to は paths_glob を自然言語化した適用条件であり、Apply phase が新規 section
// 作成時に `**適用条件:** <applies_to>` 行として書き出す (Req 1.6 / 3.7)。
// 100 chars 上限は section heading 直下の 1 行表現として運用上限。改行混入 / 空文字判定は
// Apply phase の op-level validation の責務 (本 schema は maxLength のみを固定する)。
const curatorAppliesToMaxLength = 100

// jsonSchemaDraft07URI は JSON Schema draft-07 の `$schema` 属性値。
// 両 schema の top-level に置き、skill SSoT (`rules/{reflector,curator}.md`) 内に併記される
// JSON Schema と完全一致させる (両端契約 / `TestSkillContractIntegrity_*` で assert)。
const jsonSchemaDraft07URI = "http://json-schema.org/draft-07/schema#"

// topicNamePattern は Curator schema の `topic` field に課す regex 制約。
// Go 側 `ValidTopicName` (category.go) と完全に同じ正規表現を使い、skill SSoT 内 schema と
// 1:1 で対応させる (両端契約)。値の変更は category.go と本ファイル + skill 3 ファイルを
// 同 commit で更新する不変式。
const topicNamePattern = `^[a-z][a-z0-9-]*$`

// CuratorOperation は Curator phase の LLM 出力 operations[] と Apply phase 入力で
// 共有する型 (design.md API Contract — Curator / requirements.md Req 2.1 / 2.4 / 6.2)。
//
// 構造:
//
//	{
//	  "op":         "add",
//	  "category":   "failure-recovery | success-procedure | antipattern | tool-pattern",
//	  "topic":      "<kebab-case>",
//	  "paths_glob": ["..."],
//	  "entry":      "≤ 320 chars",
//	  "summary":    "≤ 100 chars",
//	  "applies_to": "≤ 100 chars"
//	}
//
// JSON tag は snake_case で skill 側 fenced JSON / akashic learnings 列名と整合する。
// 値の検証 (Op == "add" / ValidCategory / ValidTopicName / paths_glob 健全性 /
// summary / applies_to の空・改行・長さ判定) は Apply phase (task 3.x) の責務。
// 本型は単なる data carrier であり validator を持たない。
type CuratorOperation struct {
	// Op は curator が指示する操作種別。MVP では "add" のみ (Req 6.6)。
	Op string `json:"op"`
	// Category は 4 値 closed enum のいずれか。category.go の `Category*` 定数を参照する。
	Category string `json:"category"`
	// Topic は kebab-case の topic 名。`^[a-z][a-z0-9-]*$` 規則は ValidTopicName が判定。
	Topic string `json:"topic"`
	// PathsGlob は新規 topic ファイル scaffold 時に paths: frontmatter にそのまま反映される
	// glob 配列。空 / `**` / `**/*` / `*` 単独は Apply phase で reject される (Req 7.4)。
	// wiki 移行 (Req 2.4) 後は後方互換のため保持され、Apply phase が `**適用条件:**` 行への
	// 変換に利用する原データとして残る (実際の自然言語化済み文は `AppliesTo` field に入る)。
	PathsGlob []string `json:"paths_glob"`
	// Entry は playbook bullet 1 行 (改行禁止、長さ上限 320 chars / Req 6.2)。
	Entry string `json:"entry"`
	// TopicRationale は新規 topic 作成時に curator が記す「なぜ既存 topic に fit しないか」の
	// 1 文 (≤ 240 chars / optional)。既存 topic を流用する operation では空文字 (omitempty)。
	// Apply phase は本 field を validation には使わず、curator.json への trace として保持する
	// のみ (将来の効果計測用 metadata)。
	TopicRationale string `json:"topic_rationale,omitempty"`
	// Summary は wiki index 一覧で各 entry に付ける 1 行説明 (≤ 100 chars / Req 2.1)。
	// 改行禁止 / 空文字禁止 / 100 字超過禁止の op-level validation は Apply phase が行う
	// (Req 2.3)。Apply phase は新規 section 作成時に section heading 直下の
	// `**summary:** <summary>` 行として書き出す。
	Summary string `json:"summary"`
	// AppliesTo は paths_glob を自然言語化した適用条件 (≤ 100 chars / Req 2.1)。
	// Apply phase は新規 section 作成時に `**適用条件:** <applies_to>` 行として書き出す
	// (Req 1.6 / 3.7)。空文字は reject せずプレースホルダ `(なし)` に置換する経路を
	// Apply phase が持つ (task 3.2 の責務)。改行禁止 / 100 字超過禁止のみ op-level
	// validation で reject される (Req 2.3)。
	AppliesTo string `json:"applies_to"`
}

// JSONSchemaForReflector は Reflector phase の LLM 出力 (`reflector.json` per-session)
// を純 JSON に固定するための `*provider.OutputFormat` を返す
// (Req 4.4 / design.md API Contract — Reflector)。
//
// 構造:
//
//	{
//	  "type": "object",
//	  "additionalProperties": false,
//	  "required": ["learnings"],
//	  "properties": {
//	    "learnings": {
//	      "type": "array",
//	      "items": {
//	        "type": "object",
//	        "additionalProperties": false,
//	        "required": ["category", "context", "insight", "paths_glob"],
//	        "properties": {
//	          "category":   {"type":"string","enum":[<closed 4 values>]},
//	          "context":    {"type":"string","maxLength":512},
//	          "insight":    {"type":"string","maxLength":512},
//	          "paths_glob": {"type":"array","maxItems":5,"items":{"type":"string"}}
//	        }
//	      }
//	    }
//	  }
//	}
//
// 空配列 (`learnings: []`) は「このセッションには記録すべき学びがない」を意味する有効な応答
// (Req 4.4) のため `minItems` は設定しない。
//
// 呼び出しごとに新しい map を構築する (mutation 安全)。
func JSONSchemaForReflector() *provider.OutputFormat {
	return &provider.OutputFormat{
		Type: "json_schema",
		Schema: map[string]any{
			"$schema":              jsonSchemaDraft07URI,
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"learnings"},
			"properties": map[string]any{
				"learnings": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type":                 "object",
						"additionalProperties": false,
						"required": []string{
							"category",
							"context",
							"insight",
							"paths_glob",
						},
						"properties": map[string]any{
							"category": map[string]any{
								"type": "string",
								// 4 値 closed enum: category.go の Category* 定数を SSoT 参照
								"enum": []string{
									CategoryFailureRecovery,
									CategorySuccessProcedure,
									CategoryAntipattern,
									CategoryToolPattern,
								},
							},
							"context": map[string]any{
								"type":      "string",
								"maxLength": reflectorTextMaxLength,
							},
							"insight": map[string]any{
								"type":      "string",
								"maxLength": reflectorTextMaxLength,
							},
							"paths_glob": map[string]any{
								"type":     "array",
								"minItems": pathsGlobMinItems,
								"maxItems": reflectorPathsGlobMaxItems,
								"items": map[string]any{
									"type": "string",
								},
							},
						},
					},
				},
			},
		},
	}
}

// JSONSchemaForLearningsArtifact は Reflector phase が `<runDir>/learnings.json` に
// 書き出し、Curator skill (curator.md) が Read tool で読む両端契約 SSoT を返す
// (Req 8.7 / design.md §Reflector Responsibilities)。
//
// 用途は他の 2 関数 (`JSONSchemaForReflector` / `JSONSchemaForCurator`) と異なり、
// LLM の `provider.OutputFormat` 制約ではない。本 schema は Go 側 writer
// (reflector.go の learnings.json 書き出し) と skill 側 reader (curator.md の入力契約) の
// 形状一致を pin するためだけに存在する。両端契約テスト
// (`TestSkillContractIntegrity_LearningsArtifactSchema`) は本関数の戻り値と
// curator.md 内 fenced JSON block (2 つ目) を構造比較する。
//
// 構造:
//
//	{
//	  "$schema": "http://json-schema.org/draft-07/schema#",
//	  "type": "object",
//	  "additionalProperties": false,
//	  "required": ["schema_version", "parent_run_id", "generated_at", "learnings"],
//	  "properties": {
//	    "schema_version": {"type":"integer", "const": 1},
//	    "parent_run_id":  {"type":"string"},
//	    "generated_at":   {"type":"string", "format":"date-time"},
//	    "learnings": {
//	      "type": "array",
//	      "items": {
//	        "type": "object",
//	        "additionalProperties": false,
//	        "required": ["id","session_id","step_id","provider_id","category","context","insight","paths_glob","recorded_at"],
//	        "properties": {
//	          "id":          {"type":"string"},
//	          "session_id":  {"type":"string"},
//	          "step_id":     {"type":"string"},
//	          "provider_id": {"type":"string"},
//	          "category":    {"type":"string","enum":[<closed 4>]},
//	          "context":     {"type":"string","maxLength":512},
//	          "insight":     {"type":"string","maxLength":512},
//	          "paths_glob":  {"type":"array","minItems":1,"maxItems":5,"items":{"type":"string"}},
//	          "recorded_at": {"type":"string","format":"date-time"}
//	        }
//	      }
//	    }
//	  }
//	}
//
// `learnings` 配列が空 (= 本 run で accept された learning が 0 件) でも valid。
// 呼び出しごとに新しい map を構築する (mutation 安全)。
func JSONSchemaForLearningsArtifact() map[string]any {
	return map[string]any{
		"$schema":              jsonSchemaDraft07URI,
		"type":                 "object",
		"additionalProperties": false,
		"required": []string{
			"schema_version",
			"parent_run_id",
			"generated_at",
			"learnings",
		},
		"properties": map[string]any{
			"schema_version": map[string]any{
				"type":  "integer",
				"const": 1,
			},
			"parent_run_id": map[string]any{
				"type": "string",
			},
			"generated_at": map[string]any{
				"type":   "string",
				"format": "date-time",
			},
			"learnings": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required": []string{
						"id",
						"session_id",
						"step_id",
						"provider_id",
						"category",
						"context",
						"insight",
						"paths_glob",
						"recorded_at",
					},
					"properties": map[string]any{
						"id": map[string]any{
							"type": "string",
						},
						"session_id": map[string]any{
							"type": "string",
						},
						"step_id": map[string]any{
							"type": "string",
						},
						"provider_id": map[string]any{
							"type": "string",
						},
						"category": map[string]any{
							"type": "string",
							"enum": []string{
								CategoryFailureRecovery,
								CategorySuccessProcedure,
								CategoryAntipattern,
								CategoryToolPattern,
							},
						},
						"context": map[string]any{
							"type":      "string",
							"maxLength": reflectorTextMaxLength,
						},
						"insight": map[string]any{
							"type":      "string",
							"maxLength": reflectorTextMaxLength,
						},
						"paths_glob": map[string]any{
							"type":     "array",
							"minItems": pathsGlobMinItems,
							"maxItems": reflectorPathsGlobMaxItems,
							"items": map[string]any{
								"type": "string",
							},
						},
						"recorded_at": map[string]any{
							"type":   "string",
							"format": "date-time",
						},
					},
				},
			},
		},
	}
}

// JSONSchemaForCurator は Curator phase の LLM 出力 (`curator.json`) を
// 純 JSON に固定するための `*provider.OutputFormat` を返す
// (Req 2.1 / 2.4 / 6.2 / 6.4 / design.md API Contract — Curator)。
//
// 構造:
//
//	{
//	  "type": "object",
//	  "additionalProperties": false,
//	  "required": ["operations"],
//	  "properties": {
//	    "operations": {
//	      "type": "array",
//	      "items": {
//	        "type": "object",
//	        "additionalProperties": false,
//	        "required": ["op", "category", "topic", "paths_glob", "entry", "summary", "applies_to"],
//	        "properties": {
//	          "op":         {"type":"string","enum":["add"]},
//	          "category":   {"type":"string","enum":[<closed 4 values>]},
//	          "topic":      {"type":"string"},
//	          "paths_glob": {"type":"array","items":{"type":"string"}},
//	          "entry":      {"type":"string","maxLength":320},
//	          "summary":    {"type":"string","maxLength":100},
//	          "applies_to": {"type":"string","maxLength":100}
//	        }
//	      }
//	    }
//	  }
//	}
//
// 空配列 (`operations: []`) は applied-no-op verdict 経路 (Req 6.5) で使われるため
// `minItems` は設定しない。
//
// `op` は MVP では `add` のみ (Req 6.6)、`update` / `delete` は schema レベルで reject。
// `category` は 4 値 closed enum (Req 6.4)。`topic` の kebab-case 検証は ValidTopicName
// (apply phase) の責務で schema は string 型のみ固定。`paths_glob` は wiki 移行後も
// 後方互換のため required に残り (Req 2.4)、Apply phase が `**適用条件:**` 行への変換に
// 利用する原データとして残る。`summary` / `applies_to` は新規追加 (Req 2.1) で
// maxLength:100 を固定、改行 / 空文字検証は apply phase の責務 (Req 2.3)。
//
// 呼び出しごとに新しい map を構築する (mutation 安全)。
func JSONSchemaForCurator() *provider.OutputFormat {
	return &provider.OutputFormat{
		Type: "json_schema",
		Schema: map[string]any{
			"$schema":              jsonSchemaDraft07URI,
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"operations"},
			"properties": map[string]any{
				"operations": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type":                 "object",
						"additionalProperties": false,
						"required": []string{
							"op",
							"category",
							"topic",
							"paths_glob",
							"entry",
							"summary",
							"applies_to",
						},
						"properties": map[string]any{
							"op": map[string]any{
								"type": "string",
								"enum": []string{"add"},
							},
							"category": map[string]any{
								"type": "string",
								// 4 値 closed enum: category.go の Category* 定数を SSoT 参照
								"enum": []string{
									CategoryFailureRecovery,
									CategorySuccessProcedure,
									CategoryAntipattern,
									CategoryToolPattern,
								},
							},
							"topic": map[string]any{
								"type":    "string",
								"pattern": topicNamePattern,
							},
							"paths_glob": map[string]any{
								"type":     "array",
								"minItems": pathsGlobMinItems,
								"maxItems": curatorPathsGlobMaxItems,
								"items": map[string]any{
									"type": "string",
								},
							},
							"entry": map[string]any{
								"type":      "string",
								"maxLength": curatorEntryMaxLength,
							},
							// topic_rationale は新規 topic 作成 operation のみで使う optional metadata。
							// required に含めない (既存 topic 流用時は省略可)。240 chars 上限は
							// curator.md 12-13 (Topic Index 流用ガイドライン) の運用と整合。
							"topic_rationale": map[string]any{
								"type":      "string",
								"maxLength": curatorTopicRationaleMaxLength,
							},
							// summary は wiki index で各 entry を 1 行説明する文 (Req 2.1)。
							// maxLength:100 を schema で固定し、改行 / 空文字 / 過長は apply phase で reject。
							"summary": map[string]any{
								"type":      "string",
								"maxLength": curatorSummaryMaxLength,
							},
							// applies_to は paths_glob を自然言語化した適用条件 (Req 2.1)。
							// maxLength:100 を schema で固定し、改行 / 過長は apply phase で reject。
							// 空文字は apply phase でプレースホルダ `(なし)` に置換する。
							"applies_to": map[string]any{
								"type":      "string",
								"maxLength": curatorAppliesToMaxLength,
							},
						},
					},
				},
			},
		},
	}
}
