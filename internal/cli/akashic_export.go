// Package cli の akashic_export.go は `rilll akashic export` の実装 (タスク 3.1) である。
//
// 役割:
//   - フラグ解析 (--workflow / --since / --until / --provider / --cwd / --success /
//     --limit / --offset / --pretty)
//   - 不正フラグ値 (RFC3339 パース失敗 / --success の未許容値 / Reader が返す
//     ErrInvalidLimit・ErrInvalidOffset) を CLI 不正入力扱い (exit 2 + stderr) に
//     マップする (Req 6.9)
//   - akashic.Reader.List を呼び、ListResult を ExportPayload に詰め替えて
//     encoding/json で stdout に出力する (Req 6.6, 6.10)
//   - --pretty 有無で MarshalIndent / Encoder を切り替える (Req 6.7)
//
// JSON 出力契約:
//   - フィールド名は snake_case (struct タグで指定、Req 6.10)
//   - フィールド順序は struct 定義順を維持 (encoding/json は struct 定義順を保証)
//   - 後方互換: フィールド追加のみ許容、既存フィールドの削除・型変更は破壊的変更
//     (Revalidation Triggers / 下流 spec のレビューが必要)
//   - 時刻は RFC3339Nano (time.Time の zero ではなく完了時刻のみフォーマット)
//   - nil slice (items / agent_calls) は空配列 [] として出力する
//     (encoding/json のデフォルトは null だが、機械処理側が常に配列を期待できる
//     ように buildExportPayload 内で空 slice を割り当てて null 出力を回避する)
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

// ExportPayload は `rilll akashic export` の JSON 出力ルート (Req 6.10)。
// 後方互換のためフィールド追加のみ許容する。
type ExportPayload struct {
	Items      []ExportItem `json:"items"`
	Limit      int          `json:"limit"`
	Offset     int          `json:"offset"`
	NextOffset *int         `json:"next_offset"` // null なら次ページなし
}

// ExportItem は workflow_run 1 件の JSON 表現。
// 時刻は RFC3339Nano 文字列、未完了 (FinishedAt nil) / 未確定 (Success nil) は null。
type ExportItem struct {
	WorkflowRunID string            `json:"workflow_run_id"`
	WorkflowID    string            `json:"workflow_id"`
	WorkflowName  string            `json:"workflow_name"`
	CWD           string            `json:"cwd"`
	StartedAt     string            `json:"started_at"`
	FinishedAt    *string           `json:"finished_at"`
	Success       *bool             `json:"success"`
	Error         string            `json:"error"`
	AgentCalls    []ExportAgentCall `json:"agent_calls"` // 空配列で出力 (null にしない)
}

// ExportAgentCall は agent_call 1 件の JSON 表現。
type ExportAgentCall struct {
	ID              string   `json:"id"`
	ProviderID      string   `json:"provider_id"`
	StepID          string   `json:"step_id"`
	StepName        string   `json:"step_name"`
	Success         *bool    `json:"success"`
	DurationMS      *int64   `json:"duration_ms"`
	TotalCostUSD    *float64 `json:"total_cost_usd"`
	InputTokens     *int64   `json:"input_tokens"`
	OutputTokens    *int64   `json:"output_tokens"`
	FailureCategory string   `json:"failure_category"`
	Error           string   `json:"error"`
	StartedAt       string   `json:"started_at"`
	FinishedAt      *string  `json:"finished_at"`
}

// runAkashicExport は `rilll akashic export` の本実装 (タスク 3.1, Req 6.2〜6.10, 7.6)。
//
// 動作概要:
//  1. flag.NewFlagSet で本サブコマンドのフラグ群を定義
//  2. fs.Parse 失敗時は ErrHelp なら exit 0、それ以外は exit 2 (flag 自身が stderr に
//     usage を出力済み)
//  3. --success の値を *bool に変換 ("true" / "false" / "" のみ許容)
//  4. --since / --until を RFC3339 でパース (zero time → フィルタなし)
//  5. ListOptions を組み立てて Reader.List を呼ぶ
//  6. Reader が ErrInvalidLimit / ErrInvalidOffset を返した場合は CLI 不正入力扱いで
//     exit 2、それ以外の DB / scan エラーは exit 1
//  7. ListResult を ExportPayload に詰め替え (時刻 RFC3339Nano / nil slice → [])
//  8. --pretty 有無で MarshalIndent (LF 1 つ追加) / Encoder (Encode が LF 1 つで終端) を
//     切り替えて stdout に書き込む
//
// 出力規約: 結果は stdout、エラーは stderr、exit code は 0 (成功) / 1 (実行時エラー)
// / 2 (CLI 誤用)。これは internal/cli パッケージの既存規約 (dispatch.go の package
// コメント) に揃える。
func runAkashicExport(ctx context.Context, args []string, deps Deps) int {
	fs := flag.NewFlagSet("rilll akashic export", flag.ContinueOnError)
	fs.SetOutput(deps.Stderr)
	workflowID := fs.String("workflow", "", "workflow_id でフィルタ (空 → 全 workflow)")
	since := fs.String("since", "", "started_at >= この時刻 (RFC3339)")
	until := fs.String("until", "", "started_at <= この時刻 (RFC3339)")
	providerID := fs.String("provider", "", "provider_id でフィルタ")
	cwd := fs.String("cwd", "", "cwd でフィルタ")
	successFlag := fs.String("success", "", `成功フラグでフィルタ ("true" / "false" / 空)`)
	limit := fs.Int("limit", 0, "1 ページあたりの最大件数 (0 → 既定 limit を適用)")
	offset := fs.Int("offset", 0, "オフセット")
	pretty := fs.Bool("pretty", false, "インデント付き JSON で出力")

	if err := fs.Parse(args); err != nil {
		// flag が ErrHelp を返した場合は exit 0 (--help / -h の慣例)。
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		// flag 自身が usage を stderr に出しているため、CLI 側は追加メッセージを出さない。
		return ExitUsageError
	}

	successPtr, err := parseSuccessFlag(*successFlag)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "Error: --success は \"true\" / \"false\" / 空のみ許容: %v\n", err)
		return ExitUsageError
	}

	sinceTime, err := parseRFC3339Flag("--since", *since)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "Error: %v\n", err)
		return ExitUsageError
	}
	untilTime, err := parseRFC3339Flag("--until", *until)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "Error: %v\n", err)
		return ExitUsageError
	}

	opts := akashic.ListOptions{
		WorkflowID: *workflowID,
		Range:      akashic.TimeRange{Since: sinceTime, Until: untilTime},
		Success:    successPtr,
		ProviderID: *providerID,
		CWD:        *cwd,
		Limit:      *limit,
		Offset:     *offset,
	}

	result, err := deps.Reader.List(ctx, opts)
	if err != nil {
		// Validate 系 sentinel は CLI 不正入力扱いで exit 2 にマップする (Req 6.9 / 7.4)。
		if errors.Is(err, akashic.ErrInvalidLimit) || errors.Is(err, akashic.ErrInvalidOffset) {
			fmt.Fprintf(deps.Stderr, "Error: %v\n", err)
			return ExitUsageError
		}
		fmt.Fprintf(deps.Stderr, "Error: %v\n", err)
		return ExitRunError
	}

	payload := buildExportPayload(result)
	if *pretty {
		buf, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			fmt.Fprintf(deps.Stderr, "Error: JSON エンコード失敗: %v\n", err)
			return ExitRunError
		}
		if _, err := deps.Stdout.Write(buf); err != nil {
			fmt.Fprintf(deps.Stderr, "Error: 出力書き込み失敗: %v\n", err)
			return ExitRunError
		}
		fmt.Fprintln(deps.Stdout)
	} else {
		// json.Encoder.Encode は出力末尾に LF 1 つを付与するため、追加改行は不要。
		enc := json.NewEncoder(deps.Stdout)
		if err := enc.Encode(payload); err != nil {
			fmt.Fprintf(deps.Stderr, "Error: JSON エンコード失敗: %v\n", err)
			return ExitRunError
		}
	}
	return ExitOK
}

// parseSuccessFlag は --success フラグ値を ListOptions.Success (*bool) に変換する。
// "" → nil (両方) / "true" → &true (成功のみ) / "false" → &false (失敗のみ) (Req 6.4)。
// 上記以外は invalid value を含むエラーを返し、呼び出し側で exit 2 にマップする (Req 6.9)。
func parseSuccessFlag(s string) (*bool, error) {
	switch s {
	case "":
		return nil, nil
	case "true":
		t := true
		return &t, nil
	case "false":
		f := false
		return &f, nil
	default:
		return nil, fmt.Errorf("invalid value %q", s)
	}
}

// parseRFC3339Flag は時刻フラグ (--since / --until) を time.Time にパースする。
// 空文字は zero time (= ListOptions の TimeRange でフィルタなし) を返す (Req 6.3)。
// パース失敗時はフラグ名込みのエラーを返し、呼び出し側で exit 2 にマップする (Req 6.9)。
func parseRFC3339Flag(name, s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s は RFC3339 でパースできません: %w", name, err)
	}
	return t, nil
}

// buildExportPayload は akashic.ListResult を ExportPayload に詰め替える。
//
// 重要な変換:
//   - Items / AgentCalls の nil slice → 空 slice ([]) を割り当てる
//     (encoding/json の nil slice → null 出力を回避し、機械処理側が常に
//     配列を期待できるようにするため、Req 6.10 の出力契約)
//   - 時刻フィールドは time.Time → RFC3339Nano 文字列に変換
//     (StartedAt は必須、FinishedAt は nil → null を出力)
//   - Success / DurationMS / TotalCostUSD / Input/OutputTokens は *T のまま
//     渡し、encoding/json が nil → null を出力する
func buildExportPayload(result akashic.ListResult) ExportPayload {
	items := make([]ExportItem, 0, len(result.Items))
	for _, it := range result.Items {
		items = append(items, toExportItem(it))
	}
	return ExportPayload{
		Items:      items,
		Limit:      result.Limit,
		Offset:     result.Offset,
		NextOffset: result.NextOffset,
	}
}

func toExportItem(item akashic.WorkflowRunListItem) ExportItem {
	calls := make([]ExportAgentCall, 0, len(item.AgentCalls))
	for _, c := range item.AgentCalls {
		calls = append(calls, toExportAgentCall(c))
	}
	return ExportItem{
		WorkflowRunID: item.RunID,
		WorkflowID:    item.WorkflowID,
		WorkflowName:  item.WorkflowName,
		CWD:           item.CWD,
		StartedAt:     item.StartedAt.Format(time.RFC3339Nano),
		FinishedAt:    formatOptionalTime(item.FinishedAt),
		Success:       item.Success,
		Error:         item.Error,
		AgentCalls:    calls,
	}
}

func toExportAgentCall(c akashic.AgentCallSummary) ExportAgentCall {
	return ExportAgentCall{
		ID:              c.ID,
		ProviderID:      c.ProviderID,
		StepID:          c.StepID,
		StepName:        c.StepName,
		Success:         c.Success,
		DurationMS:      c.DurationMS,
		TotalCostUSD:    c.TotalCostUSD,
		InputTokens:     c.InputTokens,
		OutputTokens:    c.OutputTokens,
		FailureCategory: c.FailureCategory,
		Error:           c.Error,
		StartedAt:       c.StartedAt.Format(time.RFC3339Nano),
		FinishedAt:      formatOptionalTime(c.FinishedAt),
	}
}

// formatOptionalTime は *time.Time → *string (RFC3339Nano) に変換する。
// nil → nil を維持することで JSON 出力では null になる。
func formatOptionalTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.RFC3339Nano)
	return &s
}
