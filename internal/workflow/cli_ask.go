package workflow

import (
	"context"
	"fmt"
	"strings"
)

// CLIAsk は非対話 CLI 経路 (`rilll run`) で利用される WorkflowAsk 実装。
//
// ワークフローが ask.Select(SelectParams{Key: "feature", ...}) を呼ぶと、
// 事前に格納された Values[Key] を返す。値が無い / Items に含まれない場合は
// エラーを返し、利用可能な選択肢を併記して呼び出し側に伝える。
//
// 返すエラーは CLIAskError でラップしてあり、CLI 層が errors.As で検出して
// exit code 2 (CLI 誤用) に振り分ける。
type CLIAsk struct {
	Values map[string]string
	Counts map[string]int
}

// CLIAskError は CLI 引数の不足/不正を示すセンチネルエラー。
// CLI 層は errors.As でこの型を検出し、exit code 2 を返す。
type CLIAskError struct {
	Key     string
	Message string
}

func (e *CLIAskError) Error() string { return e.Message }

// Select は SelectParams.Key に対応する事前指定値を返す。
// 値が無い場合は不足エラー、値が Items に無い場合は無効値エラーを返す。
// どちらの場合も exit code 2 (CLI 誤用) として CLI 層が扱う。
func (c CLIAsk) Select(_ context.Context, params SelectParams) (string, bool, error) {
	if params.Key == "" {
		return "", false, fmt.Errorf("ワークフローが Key の無い Select を要求しました（CLI 経路では実行不能）")
	}
	value, present := c.Values[params.Key]
	if !present || value == "" {
		return "", false, &CLIAskError{
			Key:     params.Key,
			Message: fmt.Sprintf("--%s が必要です。利用可能な選択肢: %s", params.Key, joinSelectItems(params.Items)),
		}
	}
	if !containsSelectID(params.Items, value) {
		return "", false, &CLIAskError{
			Key:     params.Key,
			Message: fmt.Sprintf("--%s=%q は無効です。利用可能な選択肢: %s", params.Key, value, joinSelectItems(params.Items)),
		}
	}
	return value, true, nil
}

// SelectOrdered は OrderedSelectParams.Key に対応する comma-separated CLI 値を
// 入力順のまま ordered selection として返す。
func (c CLIAsk) SelectOrdered(_ context.Context, params OrderedSelectParams) ([]string, bool, error) {
	if params.Key == "" {
		return nil, false, fmt.Errorf("ワークフローが Key の無い SelectOrdered を要求しました（CLI 経路では実行不能）")
	}
	if c.Counts != nil && c.Counts[params.Key] > 1 {
		return nil, false, &CLIAskError{
			Key:     params.Key,
			Message: fmt.Sprintf("--%s は複数回指定できません。comma-separated 形式で指定してください。利用可能な選択肢: %s", params.Key, joinSelectItems(params.Items)),
		}
	}
	value, present := c.Values[params.Key]
	if !present || value == "" {
		return nil, false, &CLIAskError{
			Key:     params.Key,
			Message: fmt.Sprintf("--%s が必要です。comma-separated 形式で指定してください。利用可能な選択肢: %s", params.Key, joinSelectItems(params.Items)),
		}
	}
	ids, err := ParseOrderedCSV(value)
	if err != nil {
		return nil, false, &CLIAskError{
			Key:     params.Key,
			Message: fmt.Sprintf("--%s=%q は不正です: %v。利用可能な選択肢: %s", params.Key, value, err, joinSelectItems(params.Items)),
		}
	}
	if err := ValidateOrderedSelection(params, ids); err != nil {
		return nil, false, &CLIAskError{
			Key:     params.Key,
			Message: err.Error(),
		}
	}
	return ids, true, nil
}

func containsSelectID(items []SelectItem, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}

func joinSelectItems(items []SelectItem) string {
	if len(items) == 0 {
		return "(なし)"
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return strings.Join(ids, ", ")
}
