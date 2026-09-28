// Package claude は Claude Agent SDK 用の Node bridge を起動する provider 実装。
// 起動・通信ロジックは internal/provider/nodebridge に集約されており、本パッケージは
// provider 固有の差分 (env var / default bridge path / observation の metrics 抽出) のみを保持する。
package claude

import (
	"context"
	"io"

	"github.com/kazuki-oshino/rilll/internal/bridge"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/provider/nodebridge"
)

const (
	defaultBridgePath = "bridge/claude-agent-sdk/dist/src/index.js"
	bridgeEnvVar      = "RILLL_CLAUDE_BRIDGE"
)

// config は ConsumeEvents / Run に渡す provider 固有メタデータを返す。
func config() nodebridge.Config {
	return nodebridge.Config{
		ProviderID:        provider.ProviderClaude,
		BridgeEnvVar:      bridgeEnvVar,
		DefaultBridgePath: defaultBridgePath,
		BuildHint:         "npm run build:claude-bridge",
		ExtractMetrics:    extractMetrics,
	}
}

// Provider は Claude Agent SDK bridge 用の AgentProvider 実装。
// NodeCommand / BridgeScript はテストおよび実行時に上書き可能。
type Provider struct {
	NodeCommand  string
	BridgeScript string
}

// New はデフォルト設定の Provider を構築する。BridgeScript は
// RILLL_CLAUDE_BRIDGE / cwd 探索 / repo root 推定の優先順で解決する。
func New() *Provider {
	return &Provider{
		NodeCommand:  nodebridge.DefaultNodeCommand,
		BridgeScript: nodebridge.ResolveDefaultBridgePath(bridgeEnvVar, defaultBridgePath),
	}
}

// ID は provider 識別子を返す。
func (p *Provider) ID() provider.ProviderID {
	return provider.ProviderClaude
}

// Call は bridge プロセスを起動して 1 リクエストを実行する。
func (p *Provider) Call(ctx context.Context, request provider.AgentRequest, onEvent provider.AgentEventCallback) (provider.AgentResponse, error) {
	return nodebridge.Run(ctx, nodebridge.RunOptions{
		Config:       config(),
		NodeCommand:  p.NodeCommand,
		BridgeScript: p.BridgeScript,
	}, request, onEvent)
}

// BuildRunRequest は AgentRequest を bridge.Request にマップする (テスト用に公開)。
func BuildRunRequest(id string, request provider.AgentRequest) bridge.Request {
	return nodebridge.BuildRunRequest(id, request)
}

// ConsumeEvents は bridge stdout を解釈して AgentResponse を返す (テスト用に公開)。
func ConsumeEvents(ctx context.Context, r io.Reader, request provider.AgentRequest, onEvent provider.AgentEventCallback) (provider.AgentResponse, error) {
	return nodebridge.ConsumeEvents(ctx, r, request, onEvent, config())
}

// extractMetrics は Claude Agent SDK の result observation から
// total_cost_usd / usage.{input,output}_tokens を読み取る。
func extractMetrics(response *provider.AgentResponse, event bridge.Event) {
	if event.ObservationType != "result" {
		return
	}
	payload, ok := nodebridge.PayloadAsMap(event)
	if !ok {
		return
	}
	response.Metrics.ResultPayload = payload
	if cost, ok := nodebridge.Number(payload["total_cost_usd"]); ok {
		response.Metrics.TotalCostUSD = &cost
	}
	if usage, ok := payload["usage"].(map[string]any); ok {
		if input, ok := nodebridge.IntNumber(usage["input_tokens"]); ok {
			response.Metrics.InputTokens = &input
		}
		if output, ok := nodebridge.IntNumber(usage["output_tokens"]); ok {
			response.Metrics.OutputTokens = &output
		}
	}
}
