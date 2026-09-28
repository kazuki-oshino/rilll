package git

import (
	"context"
	"os"
	"path/filepath"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

const planPrompt = `現在のディレクトリの構成とソースコードを確認して、README.mdの提案を作成してください。
README.mdが既に存在する場合は更新提案を、存在しない場合は新規作成の提案を作成してください。

以下のルールに従ってください：
- ファイルには書き込まないでください
- 提案内容をMarkdown形式でそのまま出力してください
- プロジェクトの目的、技術スタック、セットアップ手順、使い方を含めてください`

func UpdateReadme(agent workflowctx.AgentCaller) workflow.Definition {
	return workflow.Definition{
		ID:    "update-readme",
		Label: "README更新",
		// 主要な起動経路は複合ワークフロー update-readme-and-commit。
		// トップメニュー / `rilll list` / `rilll help` の一覧からは除外し、
		// Sequential 経由の利用と `rilll run update-readme` 名指し起動だけを残す。
		Hidden:   true,
		Recorder: workflow.RecorderFromAgent(agent),
		Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
			runDir := filepath.Join(cwd, ".rilll", "update-readme", workflow.RunIDFromContext(ctx))
			if err := os.MkdirAll(runDir, 0o755); err != nil {
				return err
			}

			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowStep, Step: 1, TotalSteps: 3, Label: "提案作成"})
			plan, err := agent.CallAgent(ctx, planPrompt, cwd, workflow.ForwardEvents(onEvent), "", workflow.WithStep(structuredMarkdownOptions(), "plan", "提案作成"))
			if err != nil || !plan.Success || plan.SessionID == "" {
				onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: "提案の作成に失敗しました"})
				return nil
			}
			proposal := extractMarkdown(plan.StructuredOutput, plan.Content)
			proposalPath := filepath.Join(runDir, "01-proposal.md")
			if err := os.WriteFile(proposalPath, []byte(proposal), 0o644); err != nil {
				return err
			}
			// 01-proposal.md を proposal kind として akashic レコードへ登録 (akashic-artifacts spec Req 2.2)。
			// 登録失敗はワークフロー本体の戻り値・success/failure 判定に伝播させない (Req 2.6)。
			_ = workflow.RecordRunArtifact(ctx, akashic.ArtifactKindProposal, proposalPath)

			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowStep, Step: 2, TotalSteps: 3, Label: "レビュー"})
			reviewPrompt := "以下のファイルにREADME.mdの更新提案があります。ファイルを読んでレビューしてフィードバックを提供してください。\n\nファイルには書き込まないでください。改善点や修正すべき箇所をリストアップしてください。\n\n提案ファイル: " + proposalPath
			review, err := agent.CallAgent(ctx, reviewPrompt, cwd, workflow.ForwardEvents(onEvent), "", workflow.WithStep(structuredMarkdownOptions(), "review", "レビュー"))
			if err != nil || !review.Success {
				onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: "レビューに失敗しました"})
				return nil
			}
			reviewText := extractMarkdown(review.StructuredOutput, review.Content)
			reviewPath := filepath.Join(runDir, "02-review.md")
			if err := os.WriteFile(reviewPath, []byte(reviewText), 0o644); err != nil {
				return err
			}
			// 02-review.md を review kind として akashic レコードへ登録 (akashic-artifacts spec Req 2.3)。
			// 登録失敗はワークフロー本体の戻り値・success/failure 判定に伝播させない (Req 2.6)。
			_ = workflow.RecordRunArtifact(ctx, akashic.ArtifactKindReview, reviewPath)

			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowStep, Step: 3, TotalSteps: 3, Label: "最終更新"})
			finalPrompt := "以下のファイルにレビューアーからのフィードバックがあります。ファイルを読んで、このフィードバックを反映してREADME.mdを更新または作成してください。\n\nフィードバックファイル: " + reviewPath
			finalResult, err := agent.CallAgent(ctx, finalPrompt, cwd, workflow.ForwardEvents(onEvent), plan.SessionID, workflow.WithStep(nil, "final-update", "最終更新"))
			if err != nil {
				onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: "エラーが発生しました"})
				return nil
			}
			resultPath := filepath.Join(runDir, "03-result.md")
			_ = os.WriteFile(resultPath, []byte(finalResult.Content), 0o644)
			// 03-result.md を result kind として akashic レコードへ登録 (akashic-artifacts spec Req 2.4)。
			// WriteFile のエラーは既存設計通り `_ =` で握りつぶされており (Req 2.7 で本 spec は呼び出し
			// 順を変更しない)、登録呼び出しは WriteFile 直後に行う。登録失敗はワークフロー本体の
			// 戻り値・success/failure 判定に伝播させない (Req 2.6)。
			_ = workflow.RecordRunArtifact(ctx, akashic.ArtifactKindResult, resultPath)
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowDone, Result: "README.mdを更新しました"})
			return nil
		},
	}
}

func structuredMarkdownOptions() *workflowctx.CallAgentOptions {
	return &workflowctx.CallAgentOptions{OutputFormat: &provider.OutputFormat{
		Type: "json_schema",
		Schema: map[string]any{
			"type":     "object",
			"required": []any{"markdown", "summary"},
			"properties": map[string]any{
				"markdown": map[string]any{"type": "string"},
				"summary":  map[string]any{"type": "string"},
			},
		},
	}}
}

func extractMarkdown(value any, fallback string) string {
	if record, ok := value.(map[string]any); ok {
		if markdown, ok := record["markdown"].(string); ok && markdown != "" {
			return markdown
		}
	}
	return fallback
}
