package workflow

import "context"

// SelectItem は WorkflowAsk.Select に渡す選択肢 1 件。
// Items で配列として並べられ、選択された ID が WorkflowAsk.Select の戻り値となる。
type SelectItem struct {
	ID    string
	Label string
}

// SelectParams は WorkflowAsk.Select に渡される引数の集合。
// Key は CLI フラグ名 (--<key>) と対応する semantic key で、非対話 CLI (CLIAsk) は
// この Key を見て事前に与えられた値を返す。TUI (workflowAsk) は Key を参照しないため
// 未指定でも互換動作する。
type SelectParams struct {
	Key    string
	Prompt string
	Items  []SelectItem
}

// WorkflowAsk はワークフロー実装からユーザー選択を取り出すための seam。
// TUI 経路では BubbleTea ベースの実装、非対話 CLI 経路では CLIAsk が利用される。
type WorkflowAsk interface {
	Select(ctx context.Context, params SelectParams) (string, bool, error)
}
