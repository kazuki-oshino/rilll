// Package workflowctx は internal/agent と internal/workflow が共有する契約型を集約する
// レイヤ中立パッケージ。
//
// 役割:
//   - workflow run のメタデータ (RunMetadata) と、その ctx 注入/取り出しヘルパ
//   - agent.CallAgent に渡すオプション (CallAgentOptions)
//   - workflow 側が agent を扱うための interface (AgentCaller / 各 optional Provider)
//
// 設計上の不変条件:
//   - 本パッケージは internal/agent / internal/workflow いずれにも依存しない leaf。
//     これにより agent → workflow の逆向き依存を断ち、レイヤを
//     `provider → agent → workflow` に揃える (issue #16)。
package workflowctx

import (
	"context"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
)

// runMetadataKey は ctx 上で RunMetadata を運ぶための private 型。
// 別パッケージから ctx を直接覗けないようにする。
type runMetadataKey struct{}

// earlyReflectionPolicyKey は ctx 上で EarlyReflectionPolicy を運ぶための private 型。
// early reflection の opt-in marker は RunMetadata と別 key にして、runtime-only signal
// として扱う。
type earlyReflectionPolicyKey struct{}

// RunMetadata は workflow run 単位で agent 呼び出しに伝播するメタデータ。
// CWD は Definition.Run の cwd 引数で、process-global な os.Getwd() に頼らず
// ctx 経由で run-scoped な作業ディレクトリを伝搬するためのフィールド。
type RunMetadata struct {
	WorkflowRunID string
	WorkflowID    string
	WorkflowName  string
	CWD           string
}

// EarlyReflectionPolicy は agent-learnings early reflection の enqueue eligibility を
// opt-in front run から agent layer へ伝える runtime-only contract。
//
// AllowedProviders は learnings package 側で解決済みの provider allowlist snapshot。
// Notify は workflow-scoped worker runtime を起こすための best-effort callback。
type EarlyReflectionPolicy struct {
	AllowedProviders []provider.ProviderID
	Notify           func()
}

// AllowsProvider は解決済み allowlist に providerID が含まれるかを返す。
func (p EarlyReflectionPolicy) AllowsProvider(providerID provider.ProviderID) bool {
	for _, allowed := range p.AllowedProviders {
		if allowed == providerID {
			return true
		}
	}
	return false
}

// CallAgentOptions は agent.CallAgent に渡す呼び出し単位のオプション。
//
// ProviderOverride / ModelOverride は review-workflow Task 1.3 で追加された per-call seam:
//   - zero-value (空文字列) のときは agent 既定の provider / provider 既定モデルを維持し、
//     既存 4 builtin workflow の挙動を変えない後方互換ガード。
//   - 非 zero-value のときは a.providers[ProviderOverride] を選択し、provider.AgentRequest.Model
//     に ModelOverride を素通しする。レビュー rule のように rule ごとに provider/model を切り替える
//     用途で使い、akashic agent_calls.provider_id にも override 後の値が記録される。
//   - ProviderModelOverrides は解決後 provider ID ごとの model 上書き。非空値のみ採用し、
//     ProviderModelOverrides[resolvedProvider] > ModelOverride > provider 既定の順で優先する。
//   - EffortOverride / ProviderEffortOverrides も同じ優先順位で reasoning effort を上書きする。
type CallAgentOptions struct {
	OutputFormat            *provider.OutputFormat
	StepID                  string
	StepName                string
	ProviderOverride        provider.ProviderID
	ModelOverride           string
	ProviderModelOverrides       map[provider.ProviderID]string
	ProviderModelParamsOverrides map[provider.ProviderID][]provider.ModelParam
	EffortOverride               string
	ProviderEffortOverrides      map[provider.ProviderID]string
	// AllowedTools は SDK / CLI に渡す許可ツールホワイトリスト。
	// nil / 空のときは provider 既定 (= 全ツール許可) を維持する後方互換ガード。
	// kiro-spec-loop の fix-judge ターンが ["Read","Glob","Grep"] を渡して Edit/Write を禁じる
	// 用途で使用する。AgentRequest.AllowedTools に伝搬される。
	AllowedTools []string

	// IncludePreviousResponse は前段応答前置 (previous-response-passing) を有効化するオプトインフラグ。
	//
	// 契約:
	//   - zero-value (false) のときは PreviousResponseContent の値に依らず前置を行わず、
	//     本機能追加前と完全に同一の prompt を provider に流す (Req 1.3, 1.5)。
	//   - true かつ PreviousResponseContent が非空文字列のときに限り、agent.CallAgent 入口で
	//     "# Previous Response" Markdown ブロックを prompt 先頭に前置する (Req 1.2)。
	//   - true でも PreviousResponseContent が空文字列のときは前置を行わず、
	//     本機能追加前と完全に同一の prompt を provider に流す no-op 経路に倒れる (Req 1.4)。
	//
	// 既存フィールド (OutputFormat / StepID / StepName / ProviderOverride / ModelOverride /
	// ProviderModelOverrides / AllowedTools) および ctx 上の RunMetadata 伝搬挙動は本フィールドの
	// 値に依らず一切変更されない (Req 1.5, 4.3)。
	IncludePreviousResponse bool

	// PreviousResponseContent は前置する前段応答テキスト (plain text / Markdown 区別なし)。
	//
	// 契約:
	//   - IncludePreviousResponse=true かつ本フィールドが非空のときに限り、agent.CallAgent 入口で
	//     "# Previous Response" ブロックに埋め込まれた上で prompt 先頭に前置される (Req 1.2, 2.1)。
	//   - 空文字列のときは IncludePreviousResponse の値に依らず前置を行わない no-op 経路に倒れる (Req 1.4)。
	//   - content の escape / sanitize は行わない。文字列はそのまま埋め込まれる (Req 2.4)。
	//
	// 切り詰め:
	//   - 本フィールドは前段応答の summary context として扱われる。summary は loss-tolerant
	//     なため internal/facet.CONTEXT_MAX_CHARS = 2000 rune を超える場合に
	//     facet.PromptContextSummary の policy で切り詰められる。
	PreviousResponseContent string

	// OutputContracts は output-contract-2phase spec で導入された Phase 2 出力契約の宣言群。
	//
	// 契約:
	//   - zero-value (nil または len==0 の空 slice) のときは本機能追加前と完全に同一挙動になる
	//     後方互換ガード (R1.3)。CallAgent は legacy path を辿り precheck (validateContracts) も
	//     Phase 2 (runReportPhase) も実行しない。akashic 記録 / provider 呼び出しの副作用も
	//     1 文字も変わらない。
	//   - 1 件以上指定されたときに限り、CallAgent 入口 precheck (Name 重複 / FormatFacet 空文字
	//     検査) を通過した後、Phase 1 (従来挙動) → Phase 2 (同一 session resume + AllowedTools=
	//     ['Write'] + MaxTurns=2 + Markdown テンプレート埋め込み指示) を同一呼び出し内で
	//     宣言順に逐次実行する (R2.1, R2.2, R2.4)。
	//   - 各 OutputContract は Name (= `.rilll/<workflow>/<runID>/<Name>.md` の report ファイル名)
	//     と FormatFacet (= Phase 2 指示文に埋め込む facet 名) の 2 フィールドを必須とする (R1.2)。
	//   - Phase 1 の応答テキスト / SessionID / Metrics 等は本フィールドの値に依らず一切変更されない。
	//     Phase 2 結果は AgentResponse.OutputContractResults に Name 単位で集約されて返る (R2.5)。
	//
	// 既存フィールド (OutputFormat / StepID / StepName / ProviderOverride / ModelOverride /
	// ProviderModelOverrides / AllowedTools / IncludePreviousResponse / PreviousResponseContent)
	// および ctx 上の RunMetadata 伝搬挙動は本フィールドの値に依らず一切変更されない。
	OutputContracts []OutputContract
}

// OutputContract は output-contract-2phase spec で導入された Phase 2 出力契約の宣言型。
//
// CallAgentOptions.OutputContracts に 1 件以上指定すると、CallAgent は Phase 1 完了後に
// 同一 session を resume したうえで Phase 2 (テンプレート充填 + report 書き出し) を実行し、
// Name 単位で人間可読 report を `.rilll/<workflow>/<runID>/<Name>.md` に書き出す。
//
// フィールドのセマンティクス:
//   - Name: report 書き出し先ファイル名の元となる識別子 (kebab-case 推奨)。
//     `.rilll/<workflow>/<runID>/<Name>.md` として書き出される。同一 CallAgent 呼び出し内で
//     Name が重複している場合、CallAgent 入口 precheck (validateContracts) で
//     ErrDuplicateContractName として弾かれ Phase 1 / Phase 2 のいずれも実行されない (R1.4)。
//   - FormatFacet: Phase 2 指示文に埋め込む facet 名。
//     facet.ResolveFacet(cwd, FacetKindOutputContract, FormatFacet) で Markdown テンプレートを
//     解決し、Phase 2 指示プロンプトに埋め込む。空文字列の場合は CallAgent 入口 precheck で
//     ErrEmptyFormatFacet として弾かれ Phase 1 / Phase 2 のいずれも実行されない (R1.5)。
//
// 本型は純データであり、検証ロジックは内部に持たない (関心の分離)。Name 重複 / FormatFacet 空文字
// 検査は agent 層の validateContracts (純関数) が CallAgent 入口で実施する。
type OutputContract struct {
	Name        string
	FormatFacet string
}

// AgentCaller は workflow 側が agent を呼び出すための contract。
// 実体は internal/agent.Agent が duck typing で満たす。
type AgentCaller interface {
	CallAgent(ctx context.Context, prompt string, cwd string, onEvent provider.AgentEventCallback, resumeSessionID string, options *CallAgentOptions) (provider.AgentResponse, error)
}

// AkashicRecorderProvider は AgentCaller がアカシックレコードの Recorder を
// 公開できることを示すためのオプショナルインターフェース。
type AkashicRecorderProvider interface {
	AkashicRecorder() akashic.Recorder
}

// AkashicReaderProvider は AgentCaller がアカシックレコードの Reader を
// 公開できることを示すためのオプショナルインターフェース。
// improve-workflow など、過去 run の context を構築する責務を持つワークフローが
// agent から Reader を取り出すための seam (design.md §workflow/optional iface)。
type AkashicReaderProvider interface {
	AkashicReader() akashic.Reader
}

// AkashicArtifactReaderProvider は AgentCaller がアカシック成果物の ArtifactReader を
// 公開できることを示すためのオプショナルインターフェース。
// improve-workflow が過去 run の中間成果物 (proposal.diff / context.json 等) を
// 取り出すための seam (design.md §workflow/optional iface)。
type AkashicArtifactReaderProvider interface {
	AkashicArtifactReader() akashic.ArtifactReader
}

// ContextWithRunMetadata は RunMetadata を ctx に注入する。
func ContextWithRunMetadata(ctx context.Context, metadata RunMetadata) context.Context {
	return context.WithValue(ctx, runMetadataKey{}, metadata)
}

// RunMetadataFromContext は ctx に注入された RunMetadata を取り出す。
func RunMetadataFromContext(ctx context.Context) (RunMetadata, bool) {
	metadata, ok := ctx.Value(runMetadataKey{}).(RunMetadata)
	return metadata, ok
}

// ContextWithEarlyReflectionEnabled は early reflection marker と resolved provider policy を
// ctx に注入する。RunMetadata は変更しない。
func ContextWithEarlyReflectionEnabled(ctx context.Context, policy EarlyReflectionPolicy) context.Context {
	policy.AllowedProviders = append([]provider.ProviderID(nil), policy.AllowedProviders...)
	return context.WithValue(ctx, earlyReflectionPolicyKey{}, policy)
}

// EarlyReflectionPolicyFromContext は ctx に注入された early reflection policy を取り出す。
func EarlyReflectionPolicyFromContext(ctx context.Context) (EarlyReflectionPolicy, bool) {
	policy, ok := ctx.Value(earlyReflectionPolicyKey{}).(EarlyReflectionPolicy)
	if !ok {
		return EarlyReflectionPolicy{}, false
	}
	policy.AllowedProviders = append([]provider.ProviderID(nil), policy.AllowedProviders...)
	return policy, true
}
