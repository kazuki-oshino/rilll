// Package codex は Codex SDK 用の Node bridge を起動する provider 実装。
// 起動・通信ロジックは internal/provider/nodebridge に集約されている。
package codex

import (
	"context"
	"io"

	"github.com/kazuki-oshino/rilll/internal/bridge"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/provider/nodebridge"
)

const (
	defaultBridgePath = "bridge/codex-sdk/dist/src/index.js"
	bridgeEnvVar      = "RILLL_CODEX_BRIDGE"
)

func config() nodebridge.Config {
	return nodebridge.Config{
		ProviderID:        provider.ProviderCodex,
		BridgeEnvVar:      bridgeEnvVar,
		DefaultBridgePath: defaultBridgePath,
		BuildHint:         "npm run build:codex-bridge",
		ExtractMetrics:    extractMetrics,
	}
}

// Provider は Codex SDK bridge 用の AgentProvider 実装。
type Provider struct {
	NodeCommand  string
	BridgeScript string
}

// New はデフォルト設定の Provider を構築する。
func New() *Provider {
	return &Provider{
		NodeCommand:  nodebridge.DefaultNodeCommand,
		BridgeScript: nodebridge.ResolveDefaultBridgePath(bridgeEnvVar, defaultBridgePath),
	}
}

func (p *Provider) ID() provider.ProviderID {
	return provider.ProviderCodex
}

func (p *Provider) Call(ctx context.Context, request provider.AgentRequest, onEvent provider.AgentEventCallback) (provider.AgentResponse, error) {
	return nodebridge.Run(ctx, nodebridge.RunOptions{
		Config:       config(),
		NodeCommand:  p.NodeCommand,
		BridgeScript: p.BridgeScript,
	}, request, onEvent)
}

// BuildRunRequest はテスト互換のため公開する委譲関数。
func BuildRunRequest(id string, request provider.AgentRequest) bridge.Request {
	return nodebridge.BuildRunRequest(id, request)
}

// ConsumeEvents はテスト互換のため公開する委譲関数。
func ConsumeEvents(ctx context.Context, r io.Reader, request provider.AgentRequest, onEvent provider.AgentEventCallback) (provider.AgentResponse, error) {
	return nodebridge.ConsumeEvents(ctx, r, request, onEvent, config())
}

// extractMetrics は Codex SDK の usage observation から
// input_tokens / output_tokens を読み取る (cost は提供されないため抽出しない)。
func extractMetrics(response *provider.AgentResponse, event bridge.Event) {
	if event.ObservationType != "usage" {
		return
	}
	payload, ok := nodebridge.PayloadAsMap(event)
	if !ok {
		return
	}
	response.Metrics.ResultPayload = payload
	if input, ok := nodebridge.IntNumber(payload["input_tokens"]); ok {
		response.Metrics.InputTokens = &input
	}
	if output, ok := nodebridge.IntNumber(payload["output_tokens"]); ok {
		response.Metrics.OutputTokens = &output
	}
}
