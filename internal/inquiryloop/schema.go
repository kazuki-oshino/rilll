// Package inquiryloop は kiro-spec-loop ワークフローで再利用される
// 「問い合わせ⇄回答ループ」エンジンの公開契約を提供する。
//
// このファイル (schema.go) は、各 kiro プロンプト呼び出しに渡す json_schema を
// 1 箇所に固定するためのファクトリ Schema() を提供する。スキーマ定義を散らかさないため、
// 呼び出し側 (Engine.RunPrompt や workflow 層) は必ずこの関数経由でスキーマを取得する。
package inquiryloop

import (
	"github.com/kazuki-oshino/rilll/internal/provider"
)

// Schema は kiro プロンプト応答用の OutputFormat (json_schema) を返す。
//
// スキーマは以下の最小構造を強制する:
//   - 最上位 required: ["has_inquiry", "summary"]
//   - properties.has_inquiry: boolean
//   - properties.summary:     string
//   - properties.inquiry:     object（required: ["title", "question"]、
//     properties は title / question のみ）
//
// background / options は schema から除外する。長文の判断材料は per-round の
// markdown 成果物 (round-N-inquiry.md / round-N-answer.md) に書き出して、
// プロンプト本文では Source: パス参照を使う。構造化出力を小さく保つことで、
// Anthropic SDK の stream_idle_timeout のリスクを下げる。
//
// 呼び出しごとに新しい map / OutputFormat を生成する。これは呼び出し側が
// 取得した map を一時的に書き換える（例えばテストで mutate する）状況を
// 安全にするため。スキーマ定義を 1 箇所にまとめる責務は維持しつつ、
// 値そのものは呼び出しごとに独立させる。
func Schema() *provider.OutputFormat {
	return &provider.OutputFormat{
		Type:   "json_schema",
		Schema: buildSchemaRoot(),
	}
}

// ValidationSchema は validate-design など、skill 自身の GO/NO-GO を workflow gate に使う
// phase 向けの OutputFormat を返す。通常 schema に以下を追加必須化する:
//   - verdict: "go" / "no_go" (workflow gate の最終判定)
//   - has_advisory: boolean (修正アドバイスや指摘の有無)
//   - advisory: string (has_advisory=true のとき本文を必須とする。parser 側で強制)
//
// has_advisory + advisory は「軽い条件付き go」も retry ループで architect に戻すための情報源。
// no_go の場合は架空の go/no_go 区分とは独立に「修正方針本文」として併用できる。
func ValidationSchema() *provider.OutputFormat {
	root := buildSchemaRoot()
	required, _ := root["required"].([]any)
	root["required"] = append(append([]any(nil), required...), "verdict", "has_advisory")
	props, _ := root["properties"].(map[string]any)
	props["verdict"] = map[string]any{
		"type": "string",
		"enum": []any{string(VerdictGo), string(VerdictNoGo)},
	}
	props["has_advisory"] = map[string]any{"type": "boolean"}
	props["advisory"] = map[string]any{"type": "string"}
	return &provider.OutputFormat{
		Type:   "json_schema",
		Schema: root,
	}
}

// buildSchemaRoot は Schema() の中身を新しい map インスタンスとして生成する。
// 呼び出しごとに「同じ構造の別オブジェクト」を返したいので、リテラルから組み立てる。
func buildSchemaRoot() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []any{"has_inquiry", "summary"},
		"properties": map[string]any{
			"has_inquiry": map[string]any{"type": "boolean"},
			"summary":     map[string]any{"type": "string"},
			"inquiry": map[string]any{
				"type":     "object",
				"required": []any{"title", "question"},
				"properties": map[string]any{
					"title":    map[string]any{"type": "string"},
					"question": map[string]any{"type": "string"},
				},
			},
		},
	}
}
