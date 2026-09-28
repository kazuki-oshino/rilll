// Package improvepatch の本ファイルは PromptTemplate interface 定義と既定実装
// (DefaultPromptTemplate: provider 別 prompt + attempt≥2 で previousProposals inject) を提供する
// (tasks.md 6.1 / design.md §PromptTemplate / Resolved Decisions §4 / §6 / Req 3.2, 3.3, 4.5)。
//
// 設計契約 (design.md §PromptTemplate / Resolved Decisions §4, §6):
//   - 純関数 (副作用なし)。`provider.AgentCaller.Call` を呼ばず、prompt 文字列のみを返す。
//   - provider 別出力契約:
//   - claude / codex: JSON Schema (`{"diff": "..."}`) を `provider.OutputFormat{Type:"json_schema"}`
//     経由で要求する prompt 指示を埋め込む。MVP の正式サポート。
//   - cursor / cursor-sdk: 外部 CLI 経由のため OutputFormat は no-op。
//     「```diff ... ``` で囲んで返せ」を prompt に明記する best-effort 経路。
//   - regenerate ループ契約 (Resolved Decisions §4):
//   - `attempt int` (1-origin) と `previousProposals []Patch` を受け取り、attempt≥2 かつ
//     previousProposals が非空のときのみ prompt 末尾に「以下は直前 N-1 試行で生成済の差分。
//     これらと異なる方向性で改善案を生成せよ」セクションを inject する。
//   - attempt=1 または previousProposals が空配列のときは section を完全に省略する
//     (既存 prompt と等価、`internal/inquiryloop/engine.go` の前ラウンド応答 inject パターンと整合)。
//
// 設計ノート (design.md naming alignment):
//   - design.md は `previousProposals []ProposedPatch` と表記しているが、本パッケージの
//     既存型は `Patch` (patch.go: `type Patch struct { TargetFiles []string; DiffBytes []byte }`)。
//     task 5.1 で `Patch` の名前で確定済 (commit b61215a) のため、本ファイルもそれに合わせて
//     `[]Patch` を引数型として採用する。design.md と実装の naming 差は本コメントで明示する。
//   - PromptContext / SampleRun / ArtifactSnippet 型は本ファイルで先行定義する。
//     ContextBuilder (task 8.1) は本型を再利用して BuildInput と並べて公開する予定。
//
// orchestration 統合 (task 11.2):
//   - regenerate ループ内 1 試行ごとに `Render(ctx, in, attempt, previousProposals, providerID)` を呼び、
//     得た prompt 文字列を `agent.CallAgent` に渡す。
//   - claude / codex 経路では `provider.AgentRequest.OutputFormat = JSONSchemaForDiff` を設定する
//     (本ファイル末尾に集約された package var を再利用)。
package patch

import (
	"context"
	"fmt"
	"strings"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
)

// PromptContext は PromptTemplate.Render に渡す入力。ContextBuilder (task 8.1) が akashic.Reader /
// akashic.ArtifactReader を集約して構築する想定の型 (design.md §ContextBuilder)。
//
// MVP では本 task 6.1 で必要な最小フィールドを定義し、task 8.1 で ContextBuilder の
// `Build(ctx, in BuildInput) (PromptContext, error)` から返される値として完成する。
type PromptContext struct {
	// TargetWorkflowID は改善対象 workflow の kebab-case ID (例: "git-commit")。
	TargetWorkflowID string

	// TargetGoFile は改善対象 Go ファイルの絶対 clean path (TargetResolver.Resolve の戻り値)。
	TargetGoFile string

	// CurrentSource は改善対象ファイルの現状全文 (Req 3.2 の (a))。
	CurrentSource string

	// SampleRuns は対象 workflow の直近 success / failure run のサンプル (Req 3.2 の (b))。
	SampleRuns []SampleRun

	// FailureClusters は失敗カテゴリ別の代表 run 集合 (Req 3.2 の (c))。
	FailureClusters []akashic.FailureCluster

	// Limited は起点 run のみで限定的なコンテキストを構築した場合 true (Req 2.5)。
	// orchestration 側で `WorkflowText` で限定通知の判定に使う。
	Limited bool
}

// SampleRun は PromptContext.SampleRuns の各要素。akashic.Reader が返す単一 run 分の集約。
//
// Req 2.4 (raw_events サイズ抑制) のため、RawEvents は MaxRawEvents 上限で切り詰められる前提。
// Artifacts は ArtifactReader 経由で取得した中間成果物のサマリ + 本体 (Req 2.3)。
type SampleRun struct {
	Run                     akashic.RunHeader
	AgentCalls              []akashic.AgentCallRow
	RawEvents               []akashic.RawEventRow
	Artifacts               []ArtifactSnippet
	IsOriginatedFromHandoff bool
}

// ArtifactSnippet は SampleRun.Artifacts の各要素。akashic.ArtifactReader.GetArtifacts +
// ReadArtifact から取得した中間成果物のスナップショットを保持する。
//
// SizeBytes が MaxArtifactBytes を超えるものは Body を空にして metadata のみ保持する契約
// (design.md §ContextBuilder Implementation Notes)。
type ArtifactSnippet struct {
	// Kind は artifact の種類 (proposal / review / result / context など)。
	Kind string
	// Path は artifact ファイルの相対パス。
	Path string
	// SizeBytes は artifact のバイト数 (truncate 判定用)。
	SizeBytes int64
	// Body は SizeBytes <= MaxArtifactBytes のときのみ非空。それ以外は空 byte slice。
	Body []byte
}

// PromptTemplate は改善案生成 prompt を構築する seam を表す。
//
// Render は副作用を持たず、入力 PromptContext / attempt / previousProposals / providerID から
// 決定的に prompt 文字列を生成する純関数契約。orchestration 側 (task 11.2) は本契約に従い、
// regenerate ループ内 1 試行ごとに Render を呼び出して agent.CallAgent に prompt を渡す。
type PromptTemplate interface {
	// Render は provider 別の prompt 文字列を返す。
	//
	// 引数:
	//   - ctx: future spec での timeout / cancel 伝播用 (現 MVP では未使用、Q4=A 確定)。
	//   - in: ContextBuilder から受け取った PromptContext。
	//   - attempt: 1-origin の試行回数 (regenerate ループ内で 1 ずつ増加)。
	//   - previousProposals: 直前までに discard された全提案 (順序保持、新しいものほど末尾)。
	//   - providerID: `provider.ProviderID` の文字列値 ("claude" / "cursor" / "codex" / "cursor-sdk")。
	//
	// 戻り値: 完成した prompt 文字列。`agent.CallAgent` の prompt 引数にそのまま渡せる形式。
	Render(ctx context.Context, in PromptContext, attempt int, previousProposals []Patch, providerID string) string
}

// DefaultPromptTemplate は PromptTemplate の MVP 既定実装。状態を持たず、
// Render は決定的な純関数として動作する (instance を使い回しても安全)。
//
// 別実装 (例: future spec での DSL prompt template) に差し替える際は、本構造体ではなく
// PromptTemplate interface を seam として利用する。
type DefaultPromptTemplate struct{}

// NewDefaultPromptTemplate は DefaultPromptTemplate のコンストラクタ。
// 戻り値 *DefaultPromptTemplate は PromptTemplate interface を満たす (`var _ PromptTemplate = ...` で検証)。
func NewDefaultPromptTemplate() *DefaultPromptTemplate {
	return &DefaultPromptTemplate{}
}

// previousProposalsSectionHeader は Resolved Decisions §4 で確定したセクションヘッダ文。
// attempt≥2 かつ previousProposals が非空のとき、prompt 末尾に inject される。
//
// 本定数の文字列を変更すると prompt_test.go の substring match テストが失敗する契約のため、
// design.md の文言と完全一致を維持する (validation: prompt_test.go の同名定数で機械検証)。
const previousProposalsSectionHeader = "以下は直前 N-1 試行で生成済の差分。これらと異なる方向性で改善案を生成せよ"

// JSONSchemaForDiff は claude / codex 経路で `provider.AgentRequest.OutputFormat` に渡す
// JSON Schema 定義 (Req 3.3 / Provider Compatibility Matrix)。
//
// `{"diff": "<unified diff>"}` 形式の JSON 出力を強制し、Patch.ExtractAndGuard の JSON 経路で
// 確実に抽出できる形を保証する。cursor 経路では本値は使われない (CLI 経由で OutputFormat が no-op)。
//
// 本 var を package level に集約する理由:
//   - tasks.md 6.1 観察可能完了条件「JSON Schema 形式 (`provider.OutputFormat{Type:"json_schema"}`) の
//     スキーマ定義も同ファイルに集約」を機械的に満たすため。
//   - orchestration (task 11.2) から `provider.AgentRequest{OutputFormat: improvepatch.JSONSchemaForDiff}`
//     として直接参照できるよう exported var として公開する。
var JSONSchemaForDiff = &provider.OutputFormat{
	Type: "json_schema",
	Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"diff": map[string]any{
				"type":        "string",
				"description": "対象 Go ファイル単一に対する unified diff (git apply --check で適用可能な形式)",
			},
		},
		"required":             []string{"diff"},
		"additionalProperties": false,
	},
}

// Render は provider 別 prompt + attempt≥2 で previousProposals inject を行う。
//
// 出力構造 (上から順):
//  1. 改善対象 metadata (workflow ID / target Go ファイル / attempt 番号)
//  2. Limited フラグの限定通知 (該当時のみ)
//  3. 現状ソース (Req 3.2 の (a))
//  4. 過去 run サンプル (Req 3.2 の (b))
//  5. 失敗クラスタ (Req 3.2 の (c))
//  6. 出力形式指示 (provider 別: claude/codex は JSON Schema、cursor は ```diff コードブロック)
//  7. 直前提案セクション (attempt≥2 かつ previousProposals 非空のときのみ、Resolved Decisions §4)
//
// ctx は現 MVP では未使用だが、interface 契約として将来の timeout 伝播のため引数に保持する。
func (t *DefaultPromptTemplate) Render(_ context.Context, in PromptContext, attempt int, previousProposals []Patch, providerID string) string {
	var b strings.Builder

	// (1) 改善対象 metadata
	b.WriteString("# 改善対象ワークフロー\n")
	b.WriteString(fmt.Sprintf("- workflow_id: %s\n", in.TargetWorkflowID))
	b.WriteString(fmt.Sprintf("- target_go_file: %s\n", in.TargetGoFile))
	b.WriteString(fmt.Sprintf("- attempt: %d\n", attempt))
	b.WriteString("\n")

	// (2) Limited 通知 (起点 run のみで限定コンテキストの場合)
	if in.Limited {
		b.WriteString("(注: 起点 run のみの限定的なコンテキストです。改善案生成の根拠が薄い可能性があります。)\n\n")
	}

	// (3) 現状ソース (Req 3.2 (a))
	b.WriteString("# 現状ソース\n")
	b.WriteString("```go\n")
	b.WriteString(in.CurrentSource)
	if !strings.HasSuffix(in.CurrentSource, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("```\n\n")

	// (4) 過去 run サンプル (Req 3.2 (b))
	if len(in.SampleRuns) > 0 {
		b.WriteString("# 過去 run のサンプル\n")
		for i, sr := range in.SampleRuns {
			b.WriteString(fmt.Sprintf("## sample_run %d\n", i+1))
			b.WriteString(fmt.Sprintf("- run_id: %s\n", sr.Run.RunID))
			if sr.Run.WorkflowName != "" {
				b.WriteString(fmt.Sprintf("- workflow_name: %s\n", sr.Run.WorkflowName))
			}
			if sr.Run.Error != "" {
				b.WriteString(fmt.Sprintf("- error: %s\n", sr.Run.Error))
			}
			if sr.IsOriginatedFromHandoff {
				b.WriteString("- origin: handoff (起点 run)\n")
			}
			b.WriteString(fmt.Sprintf("- agent_calls: %d 件 / raw_events: %d 件 / artifacts: %d 件\n",
				len(sr.AgentCalls), len(sr.RawEvents), len(sr.Artifacts)))
			b.WriteString("\n")
		}
	}

	// (5) 失敗クラスタ (Req 3.2 (c))
	if len(in.FailureClusters) > 0 {
		b.WriteString("# 失敗クラスタ\n")
		for _, fc := range in.FailureClusters {
			b.WriteString(fmt.Sprintf("- category=%q provider=%s count=%d normalized_key=%q\n",
				fc.FailureCategory, fc.ProviderID, fc.Count, fc.NormalizedErrorKey))
		}
		b.WriteString("\n")
	}

	// (6) 出力形式指示 (provider 別、Resolved Decisions §6)
	b.WriteString("# 出力形式 (Req 3.3 / Provider Compatibility Matrix)\n")
	b.WriteString(renderOutputFormatInstruction(providerID, in.TargetGoFile))
	b.WriteString("\n")

	// (7) 直前提案セクション (Resolved Decisions §4)
	// attempt≥2 かつ previousProposals が非空のときのみ inject する。
	// attempt=1 または previousProposals 空のときは section 自体を省略 (既存 prompt と等価)。
	if attempt >= 2 && len(previousProposals) > 0 {
		b.WriteString(renderPreviousProposalsSection(previousProposals))
	}

	return b.String()
}

// renderOutputFormatInstruction は providerID に応じた出力形式指示文字列を返す。
//
// claude / codex: JSON Schema (`{"diff": "..."}`) 形式で diff を返却する指示。
//
//	`provider.OutputFormat{Type:"json_schema"}` (本ファイル末尾の JSONSchemaForDiff) で
//	出力形式を強制する経路と組み合わせて使う。
//
// cursor / cursor-sdk: ```diff ... ``` のコードブロックで囲んで返却する指示。
//
//	cursor は外部 CLI 経由のため OutputFormat 強制が no-op となる現実 (Resolved Decisions §6)
//	に対応する best-effort 経路。
//
// 未知 providerID: cursor 経路と同じ生 text コードブロック指示を fallback として採用する。
//
//	Patch.ExtractAndGuard が両形式 (JSON / 生 text) を順に試行する設計と整合 (Req 3.1)。
func renderOutputFormatInstruction(providerID string, targetGoFile string) string {
	var b strings.Builder
	switch providerID {
	case string(provider.ProviderClaude), string(provider.ProviderCodex):
		// claude / codex: JSON Schema 強制経路。
		// `{"diff": "..."}` JSON 形式を指示し、provider.OutputFormat 経由で構造化出力を要求する。
		b.WriteString(`応答は JSON 形式で {"diff": "<unified diff>"} を返してください。` + "\n")
		b.WriteString(fmt.Sprintf("改善対象は %s 単一ファイルに限定し、unified diff (git apply --check で適用可能な形式) を diff フィールドに格納してください。\n", targetGoFile))
		b.WriteString("`diff --git a/<path> b/<path>` 行で始まる標準的な git unified diff 形式を使ってください。\n")
	case string(provider.ProviderCursor), string(provider.ProviderCursorSDK):
		// cursor / cursor-sdk: 生 text コードブロック誘導経路 (best-effort)。
		b.WriteString("応答は ```diff ... ``` のコードブロックで囲んだ unified diff を返してください。\n")
		b.WriteString(fmt.Sprintf("改善対象は %s 単一ファイルに限定し、`diff --git a/<path> b/<path>` 行で始まる標準的な git unified diff 形式を使ってください。\n", targetGoFile))
	default:
		// 未知 providerID: cursor 経路と同じ生 text 指示を fallback。
		// Patch.ExtractAndGuard は両形式を試行するため、安全側に倒す。
		b.WriteString("応答は ```diff ... ``` のコードブロックで囲んだ unified diff を返してください (provider 未特定 fallback)。\n")
		b.WriteString(fmt.Sprintf("改善対象は %s 単一ファイルに限定してください。\n", targetGoFile))
	}
	return b.String()
}

// renderPreviousProposalsSection は attempt≥2 で prompt 末尾に inject されるセクションを構築する。
//
// 構造:
//   - セクションヘッダ: previousProposalsSectionHeader (design.md Resolved Decisions §4 の文言に厳密一致)
//   - 各 previousProposal の DiffBytes を ```diff コードブロックで囲んで列挙
//
// orchestration (task 11.2) は本セクションを通じて「直前 N-1 試行とは異なる方向性」を AI に要求する
// (Req 4.5: regenerate で同コンテキスト再生成、試行毎に prompt 入力を変化)。
func renderPreviousProposalsSection(previousProposals []Patch) string {
	var b strings.Builder
	b.WriteString("# 直前試行の差分 (regenerate context)\n")
	b.WriteString(previousProposalsSectionHeader)
	b.WriteString("\n\n")
	for i, p := range previousProposals {
		b.WriteString(fmt.Sprintf("## attempt %d (拒否済)\n", i+1))
		// Issue #46 input thinning: AbsolutePath が供給されているとき
		// Source パス参照行を追加。長文 DiffBytes は永続化済みファイルにあるため、
		// 必要なら agent が Read ツールで読みに行ける。
		if p.AbsolutePath != "" {
			b.WriteString(fmt.Sprintf("- source: %s\n", p.AbsolutePath))
		}
		body := truncatePreviousDiff(p.DiffBytes)
		b.WriteString("```diff\n")
		b.WriteString(body)
		// 末尾改行が無いと コードフェンスがネストして見えるため安全側に補う。
		if !strings.HasSuffix(body, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("```\n\n")
	}
	return b.String()
}

// previousProposalDiffMaxChars は renderPreviousProposalsSection が直前 attempt の
// DiffBytes を切り詰めるときの上限文字数 (rune 単位)。
//
// `inquiryloop.previousAnswerMaxChars=2000` と同様に、プロンプトへ含める本文量を制限する。
// 永続化済み `proposal-N.diff` への Source パス参照を併記することで、
// 長文 diff 全文を prompt に流し込まず stream_idle_timeout の構造的リスクを下げる。
// 値が 0 以下のときは切り詰めを無効化する (truncatePreviousDiff 側で素通し)。
const previousProposalDiffMaxChars = 2000

// truncatePreviousDiff は long DiffBytes を previousProposalDiffMaxChars (rune 単位) に
// 切り詰める。rune 境界で切るためマルチバイト文字を含む diff でも文字途中で切れない。
//
// 上限以下ならそのまま string 化して返す。超えるときは先頭 N rune + 改行 +
// "...(truncated; full content at Source path)" マーカを末尾に付ける
// (`inquiryloop.truncateForResume` と同形)。
func truncatePreviousDiff(b []byte) string {
	s := string(b)
	if previousProposalDiffMaxChars <= 0 || s == "" {
		return s
	}
	runes := []rune(s)
	if len(runes) <= previousProposalDiffMaxChars {
		return s
	}
	return string(runes[:previousProposalDiffMaxChars]) + "\n\n...(truncated; full content at Source path)"
}
