package git

import (
	"context"
	"strings"

	agentpkg "github.com/kazuki-oshino/rilll/internal/agent"
	"github.com/kazuki-oshino/rilll/internal/config"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// git-commit ワークフローのプロバイダ別ポリシー (single source of truth)。
// モデルや effort を変えるときはここだけを編集する。テストはこの定数を参照する。
//
// Cursor SDK は composer-2.5 (fast=false) を reasoning 未指定 (effort="") で固定する。TUI で
// gpt-5.5-xhigh を選択していても /git-commit はこの override で composer-2.5 に
// 倒し、effort="" を明示的に渡すことで provider default の Effort="xhigh" を
// 確実にクリアする (agent.go の ProviderEffortOverrides ok 判定で空文字 entry が
// 採用される semantics に依存)。ModelParams も composer-2.5 プリセットと同形に揃える。
var cursorSDKGitCommitModelSettings = config.CursorSDKModelSelection(config.CursorSDKModelPresetComposer25)

const (
	CursorSDKGitCommitModel  = "composer-2.5"
	CursorSDKGitCommitEffort = ""
	CodexGitCommitModel      = "gpt-6-luna"
	CodexGitCommitEffort     = "medium"
)

// CommitCallOptions returns the provider-specific execution policy for the
// /git-commit slash command without starting a nested git-commit workflow run.
func CommitCallOptions(stepID, stepName string) *workflowctx.CallAgentOptions {
	return workflow.WithStep(&workflowctx.CallAgentOptions{
		ProviderModelOverrides: map[provider.ProviderID]string{
			provider.ProviderCursorSDK: cursorSDKGitCommitModelSettings.Model,
			provider.ProviderCodex:     CodexGitCommitModel,
		},
		ProviderModelParamsOverrides: map[provider.ProviderID][]provider.ModelParam{
			provider.ProviderCursorSDK: append([]provider.ModelParam(nil), cursorSDKGitCommitModelSettings.ModelParams...),
		},
		ProviderEffortOverrides: map[provider.ProviderID]string{
			provider.ProviderCursorSDK: CursorSDKGitCommitEffort,
			provider.ProviderCodex:     CodexGitCommitEffort,
		},
	}, stepID, stepName)
}

// CallCommit invokes the /git-commit slash command in a fresh agent session.
// Callers keep their current workflow run and artifact boundary; only the
// git-commit provider/model policy is shared.
func CallCommit(
	ctx context.Context,
	agent workflowctx.AgentCaller,
	cwd string,
	onEvent func(workflow.WorkflowEvent),
	stepID string,
	stepName string,
) (provider.AgentResponse, error) {
	resp, err := agent.CallAgent(
		ctx,
		"/git-commit",
		cwd,
		workflow.ForwardEvents(onEvent),
		"",
		CommitCallOptions(stepID, stepName),
	)
	if !shouldFallbackGitCommitToCursorSDK(resp, err) {
		return resp, err
	}
	if onEvent != nil {
		onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "git-commit provider error; retrying with cursor-sdk"})
	}
	fallbackOptions := CommitCallOptions(stepID, stepName)
	fallbackOptions.ProviderOverride = provider.ProviderCursorSDK
	return agent.CallAgent(
		ctx,
		"/git-commit",
		cwd,
		workflow.ForwardEvents(onEvent),
		"",
		fallbackOptions,
	)
}

func shouldFallbackGitCommitToCursorSDK(resp provider.AgentResponse, err error) bool {
	if err != nil && agentpkg.CategoryFromError(err) != agentpkg.FailureCategoryProviderError {
		return false
	}
	if err == nil && (resp.Success || !isGitCommitProviderFailureResponse(resp)) {
		return false
	}
	if resp.SystemInfo == nil || resp.SystemInfo.ProviderID == "" {
		return false
	}
	return resp.SystemInfo.ProviderID != provider.ProviderCursorSDK
}

func isGitCommitProviderFailureResponse(resp provider.AgentResponse) bool {
	content := strings.ToLower(resp.Content)
	return strings.Contains(content, "selected model is at capacity") ||
		strings.Contains(content, "stream disconnected before completion") ||
		strings.Contains(content, "an error occurred while processing your request")
}
