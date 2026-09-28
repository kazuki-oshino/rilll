package git

import (
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// UpdateReadmeAndCommit は README更新ワークフローを実行した後に git-commit ワークフローを実行する複合ワークフロー。
// 既存の UpdateReadme / Commit ワークフロー定義をそのまま再利用し、Sequential で順次連鎖させる。
// 子のいずれかが失敗（戻り値 error または WorkflowError イベント）した場合は、後続を実行せず中断する。
func UpdateReadmeAndCommit(agent workflowctx.AgentCaller) workflow.Definition {
	return workflow.Sequential(
		"update-readme-and-commit",
		"README更新 → git commit",
		agent,
		UpdateReadme(agent),
		Commit(agent),
	)
}
