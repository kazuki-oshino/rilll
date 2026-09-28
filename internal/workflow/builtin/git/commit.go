package git

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

type gitCommitAuditResult struct {
	Workflow    string                    `json:"workflow"`
	RunID       string                    `json:"runId"`
	StartedAt   string                    `json:"startedAt"`
	CompletedAt string                    `json:"completedAt"`
	Success     bool                      `json:"success"`
	Content     string                    `json:"content"`
	SessionID   string                    `json:"sessionId,omitempty"`
	SystemInfo  *provider.AgentSystemInfo `json:"systemInfo,omitempty"`
}

func Commit(agent workflowctx.AgentCaller) workflow.Definition {
	return workflow.Definition{
		ID:       "git-commit",
		Label:    "git commit",
		Recorder: workflow.RecorderFromAgent(agent),
		Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			runID := workflow.RunIDFromContext(ctx)
			startedAt := time.Now().Format(time.RFC3339Nano)
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowStep, Step: 1, TotalSteps: 1, Label: "git commit"})

			result, err := CallCommit(ctx, agent, cwd, onEvent, "git-commit", "git commit")
			if err != nil {
				_ = saveGitCommitAudit(ctx, cwd, gitCommitAuditResult{
					Workflow:    "git-commit",
					RunID:       runID,
					StartedAt:   startedAt,
					CompletedAt: time.Now().Format(time.RFC3339Nano),
					Success:     false,
					Content:     err.Error(),
				})
				onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: "エラーが発生しました"})
				return nil
			}

			_ = saveGitCommitAudit(ctx, cwd, gitCommitAuditResult{
				Workflow:    "git-commit",
				RunID:       runID,
				StartedAt:   startedAt,
				CompletedAt: time.Now().Format(time.RFC3339Nano),
				Success:     result.Success,
				Content:     result.Content,
				SessionID:   result.SessionID,
				SystemInfo:  result.SystemInfo,
			})

			if !result.Success {
				onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: "git commitに失敗しました"})
				return nil
			}
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowDone, Result: result.Content})
			return nil
		},
	}
}

// saveGitCommitAudit は result.json を `.rilll/git-commit/<runID>/` に書き出し、
// その直後にアカシックレコードへ result kind の成果物として登録する
// (akashic-artifacts spec Req 2.1)。
//
// 登録呼び出しは `_ =` で error を握りつぶし、ワークフロー本体の戻り値・
// success/failure 判定には影響させない (Req 2.6)。ファイル名・書き出しタイミング・
// 呼び出し順は本 spec で変更しない (Req 2.7)。
func saveGitCommitAudit(ctx context.Context, cwd string, audit gitCommitAuditResult) error {
	runDir := filepath.Join(cwd, ".rilll", "git-commit", audit.RunID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(audit, "", "  ")
	if err != nil {
		return err
	}
	resultPath := filepath.Join(runDir, "result.json")
	if err := os.WriteFile(resultPath, body, 0o644); err != nil {
		return err
	}
	// 登録失敗はワークフロー結果に伝播させない (Req 2.6)。失敗時は workflow.RecordRunArtifact
	// 内部で raw_events に warning が best-effort で記録される (recorder 解決済み経路の場合)。
	_ = workflow.RecordRunArtifact(ctx, akashic.ArtifactKindResult, resultPath)
	return nil
}
