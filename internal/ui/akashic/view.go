// Package akashic のアカシックモード描画ロジック。
//
// State.View は 3 サブビュー (List / Summary / Detail) のいずれかを描画する。
// lipgloss の既存 style (sectionStyle / itemStyle / selectedStyle / mutedStyle /
// warningStyle / metaStyle) を流用し、新規依存は追加しない。
//
// List 描画は次の優先順で排他的に 1 状態のみを描く:
//  1. Reader == nil       → 「履歴データが利用できません」(Req 11.2)
//  2. Loading == true     → 「読み込み中...」(Req 9.2 の応答性表示)
//  3. Err != nil          → エラー文 (warningStyle)
//  4. EmptyMessage != ""  → 「該当 run がありません」等 (Req 3.9)
//  5. それ以外            → Items を Cursor 反映で描画
//
// この優先順は applyListRunsMsg の状態確定ルールと整合する: 受信時に
// Loading=false にしてから Err / EmptyMessage / Items のいずれかを排他に
// セットするため、View 側で「Loading 中の Items 表示」「Err と EmptyMessage の
// 併存表示」のような中途半端な描画は発生しない。
//
// Reader=nil graceful 表示は単一メッセージで完結させ、Sort / Filter のヘッダや
// ページング情報は描画しない (ユーザに「データ源が利用不可」を最短で伝えるため)。
package akashic

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	record "github.com/kazuki-oshino/rilll/internal/akashic"
)

// アカシックモード描画で使う lipgloss style 群。
//
// TUI シェル (package ui) と同じ配色を維持するため、definitions は package ui
// から複製している。共通化すると ui ↔ akashic の双方向依存になるため、
// 単純な視覚 token はそれぞれのパッケージ内で持つ運用とする。
var (
	metaStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#94A3B8"))
	sectionStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#F8FAFC")).
			Border(lipgloss.NormalBorder(), false, false, true, false).
			BorderForeground(lipgloss.Color("#334155"))
	selectedStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FBBF24")).
			Background(lipgloss.Color("#1E293B"))
	itemStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#E2E8F0"))
	mutedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#64748B"))
	warningStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FBBF24"))
)

// withWidth は width > 0 のとき lipgloss.Style に幅を適用する。
//
// width <= 0 (= 未取得 / 狭すぎる) のときは素の style を返し、ターミナル幅
// 取得前の起動直後でも崩れずに描画できるようにする。
func withWidth(style lipgloss.Style, width int) lipgloss.Style {
	if width <= 0 {
		return style
	}
	return style.Width(width)
}

// renderChoiceWithWidth は選択肢 1 行を描画する共通ヘルパ。
//
// 出力フォーマット: "<cursor><label>  <suffix-meta>"
//   - selected=true   → cursor "› " + selectedStyle
//   - selected=false  → cursor "  " + itemStyle
//   - suffix != ""    → metaStyle で末尾に追記
//
// width > 0 のとき withWidth で右側を埋め、選択行ハイライトを行末まで伸ばす。
func renderChoiceWithWidth(selected bool, label string, suffix string, width int) string {
	cursor := "  "
	style := itemStyle
	if selected {
		cursor = "› "
		style = selectedStyle
	}
	text := cursor + label
	if suffix != "" {
		text += "  " + metaStyle.Render(suffix)
	}
	return withWidth(style, width).Render(text)
}

// View は現在のサブビューに応じた表示文字列を返す (width は contentWidth)。
//
// width <= 0 は「未取得 (起動直後) または狭すぎる」を示すサインなので、
// 各サブビューは lipgloss style に width を渡さず素のレンダリングに留める。
//
// 本体描画の末尾には、サブビュー固有の操作キーヒント (renderSubviewHint) を
// 必ず付与する。Reader=nil / Loading / Err 等の graceful 経路でも esc/q による
// 退出キーをユーザに伝えるため、ヒントは状態に関わらず常に表示する
// (既存の renderMenu / renderProviderSettings / renderWorkflow と同じ運用)。
func (s State) View(width int) string {
	switch s.Subview {
	case SubviewList:
		return s.renderList(width) + s.renderSubviewHint(SubviewList, width)
	case SubviewSummary:
		return s.renderSummary(width) + s.renderSubviewHint(SubviewSummary, width)
	case SubviewDetail:
		return s.renderDetail(width) + s.renderSubviewHint(SubviewDetail, width)
	}
	return ""
}

// renderSubviewHint はサブビュー固有の操作キー一覧を 1 行のヒント表示に整形する。
//
// 文言は 1 箇所に集約し、docs/akashic.md のキー操作リファレンス節と整合させる。
// mutedStyle で控えめに描画し、画面幅 (width) に合わせて折り返しできるよう
// withWidth でラップする。
//
// 出力末尾には改行を含めて返すことで、呼び出し側が改行管理を意識しなくて済む。
func (s State) renderSubviewHint(sub Subview, width int) string {
	var hint string
	switch sub {
	case SubviewList:
		hint = "↑/↓ 選択 · Enter 詳細 · Tab 集計 · esc/q 戻る"
	case SubviewSummary:
		hint = "Tab 一覧 · esc/q 戻る"
	case SubviewDetail:
		hint = "Tab/Shift+Tab フォーカス · ↑/↓ 移動 · n/p ページ · Enter プレビュー · i 改善 · Backspace 一覧 · esc/q 戻る"
	default:
		return ""
	}
	return withWidth(mutedStyle, width).Render(hint) + "\n"
}

// renderList は Run 一覧サブビューを描画する (Req 3.1 / 3.9 / 3.10 / 9.1 / 11.2 / 11.5)。
//
// 排他描画の優先順位はファイル冒頭コメント参照。各分岐は独立に 1 文字列を
// 組み立てて return することで、複数状態が同時に描画されないことを構造的に保証する。
func (s State) renderList(width int) string {
	var b strings.Builder
	b.WriteString(sectionStyle.Render("アカシックモード - Run 一覧"))
	b.WriteString("\n")

	if s.Reader == nil {
		b.WriteString(withWidth(mutedStyle, width).Render("履歴データが利用できません"))
		b.WriteString("\n")
		return b.String()
	}

	// Sort / Filter の現在値を 1 行で表示
	header := s.renderListHeader()
	if header != "" {
		b.WriteString(withWidth(metaStyle, width).Render(header))
		b.WriteString("\n")
	}

	if s.List.Loading {
		b.WriteString(withWidth(mutedStyle, width).Render("読み込み中..."))
		b.WriteString("\n")
		return b.String()
	}

	if s.List.Err != nil {
		b.WriteString(withWidth(warningStyle, width).Render(fmt.Sprintf("エラー: %s", s.List.Err.Error())))
		b.WriteString("\n")
		return b.String()
	}

	if s.List.EmptyMessage != "" {
		b.WriteString(withWidth(mutedStyle, width).Render(s.List.EmptyMessage))
		b.WriteString("\n")
		return b.String()
	}

	// Items は newest-first で並ぶ。全件描画すると model.View の clipFromTop で
	// 先頭 (= 新しい側) が画面外に押し出されてしまう (#11) ため、Cursor を中心
	// (端付近では端基準) とする listWindow 件の窓に絞り込んで描画する。これにより
	// 画面に映る範囲に必ず Cursor 行が含まれることを構造的に保証する。
	total := len(s.List.Items)
	start, end := clipListWindow(s.List.Cursor, total, listWindow)
	for i := start; i < end; i++ {
		b.WriteString(renderRunListItem(i == s.List.Cursor, s.List.Items[i], width))
		b.WriteString("\n")
	}

	// 窓外があるとき "showing X-Y / total" を出して全件数を告知する
	// (Detail 側 renderDetailAgentCalls 等と同じパターン)。
	if start > 0 || end < total {
		b.WriteString(withWidth(mutedStyle, width).Render(
			fmt.Sprintf("  showing %d-%d / %d", start+1, end, total)))
		b.WriteString("\n")
	}

	// 窓フッタの後にページング footer を出す: 視線移動として「現在の窓 → 全体ページ」
	// と段階的に範囲が広がる読み順を維持する。
	if footer := s.renderListPagingFooter(); footer != "" {
		b.WriteString(withWidth(mutedStyle, width).Render(footer))
		b.WriteString("\n")
	}

	return b.String()
}

// renderListHeader は Sort / Filter の現在値を 1 行のメタ表示文字列に組み立てる。
//
// Sort はゼロ値のとき "newest" (akashic-query 既定値 = SortNewest) として表示する。
// 初期化忘れの状態でも「現在の並び順」がユーザに伝わるようにするため。
func (s State) renderListHeader() string {
	sortLabel := string(s.List.Sort)
	if sortLabel == "" {
		sortLabel = string(SortNewest)
	}
	parts := []string{fmt.Sprintf("Sort: %s", sortLabel)}

	filter := s.List.Filter
	if filter.WorkflowID != "" {
		parts = append(parts, fmt.Sprintf("Workflow: %s", filter.WorkflowID))
	}
	if !filter.Range.Since.IsZero() || !filter.Range.Until.IsZero() {
		parts = append(parts, fmt.Sprintf("Period: %s - %s",
			formatTimeOrEmpty(filter.Range.Since),
			formatTimeOrEmpty(filter.Range.Until)))
	}
	if filter.Success != nil {
		parts = append(parts, fmt.Sprintf("Success: %t", *filter.Success))
	}

	return strings.Join(parts, " | ")
}

// renderListPagingFooter は現在ページの位置と次ページの有無を伝える。
//
// 例: "page offset: 0 (next: 50)" / "page offset: 100 (last)"
//
// 呼び出し側 (renderList の Items 描画分岐) で len(Items) > 0 が確定しているため、
// 空判定は不要。
func (s State) renderListPagingFooter() string {
	if s.List.NextOffset != nil {
		return fmt.Sprintf("page offset: %d (next: %d)", s.List.Offset, *s.List.NextOffset)
	}
	return fmt.Sprintf("page offset: %d (last)", s.List.Offset)
}

// formatTimeOrEmpty は zero 時刻を "-" として整形する (View ヘッダの Range 表示用)。
func formatTimeOrEmpty(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02")
}

// renderSummary は集計サマリサブビューを描画する (Req 4.1〜4.7 / 9.1 / 11.2 / 11.5)。
//
// 排他描画の優先順位 (renderList と同じパターン):
//  1. Reader == nil       → 「履歴データが利用できません」(Req 11.2)
//  2. Loading == true     → 「読み込み中...」(Req 9.1: 初回レンダリング応答性)
//  3. Err != nil          → エラー文 (warningStyle, Req 4.6)
//  4. EmptyMessage != ""  → 「集計対象 run がありません」(Req 4.5)
//  5. それ以外            → Workflow / Provider 各 bucket を 1 行 1 件で描画
//
// Req 4.7: 簡易バーは ASCII (`[####------]` 形式) のみ。グラフィカル描画は行わない。
// バー長さは barLengthForWidth で 10〜20 文字に正規化する。
func (s State) renderSummary(width int) string {
	var b strings.Builder
	b.WriteString(sectionStyle.Render("アカシックモード - 集計サマリ"))
	b.WriteString("\n")

	if s.Reader == nil {
		b.WriteString(withWidth(mutedStyle, width).Render("履歴データが利用できません"))
		b.WriteString("\n")
		return b.String()
	}

	if s.Summary.Loading {
		b.WriteString(withWidth(mutedStyle, width).Render("読み込み中..."))
		b.WriteString("\n")
		return b.String()
	}

	if s.Summary.Err != nil {
		b.WriteString(withWidth(warningStyle, width).Render(fmt.Sprintf("エラー: %s", s.Summary.Err.Error())))
		b.WriteString("\n")
		return b.String()
	}

	if s.Summary.EmptyMessage != "" {
		b.WriteString(withWidth(mutedStyle, width).Render(s.Summary.EmptyMessage))
		b.WriteString("\n")
		return b.String()
	}

	barLen := barLengthForWidth(width)

	// Workflow 集計セクション (Req 4.1)
	b.WriteString(metaStyle.Render("[Workflow 集計]"))
	b.WriteString("\n")
	if len(s.Summary.WorkflowBuckets) == 0 {
		b.WriteString(withWidth(mutedStyle, width).Render("  (workflow 集計なし)"))
		b.WriteString("\n")
	} else {
		for _, bucket := range s.Summary.WorkflowBuckets {
			b.WriteString(withWidth(itemStyle, width).Render(renderWorkflowBucket(bucket, barLen)))
			b.WriteString("\n")
		}
	}

	// Provider 集計セクション (Req 4.2)
	b.WriteString(metaStyle.Render("[Provider 集計]"))
	b.WriteString("\n")
	if len(s.Summary.ProviderBuckets) == 0 {
		b.WriteString(withWidth(mutedStyle, width).Render("  (provider 集計なし)"))
		b.WriteString("\n")
	} else {
		for _, bucket := range s.Summary.ProviderBuckets {
			b.WriteString(withWidth(itemStyle, width).Render(renderProviderBucket(bucket, barLen)))
			b.WriteString("\n")
		}
	}

	return b.String()
}

// renderWorkflowBucket は workflow 集計 1 件を 1 行に整形する (Req 4.1)。
//
// 表示要素 (1 行 / 1 bucket):
//   - WorkflowID: 識別子
//   - SuccessRate: 0〜100% 整数表示 + ASCII バー併記 (Req 4.7)
//   - RunCount: 母集団サイズ
//   - AvgCostUSD: 小数 4 桁 ($-prefix)、nil → "-"
//   - P95DurationMS: 整数 ms、nil → "-"
//   - SumInputTokens / SumOutputTokens: "in/out" 形式
func renderWorkflowBucket(bucket record.WorkflowAggregateBucket, barLen int) string {
	rate := bucket.SuccessRate
	pct := int(rate*100 + 0.5)
	bar := renderASCIIBar(rate, barLen)
	cost := formatNullableCost(bucket.AvgCostUSD)
	p95 := formatNullableDurationMS(bucket.P95DurationMS)
	return fmt.Sprintf("  %-20s %3d%% %s runs=%-5d avg=%s p95=%s tokens=%d/%d",
		bucket.WorkflowID,
		pct,
		bar,
		bucket.RunCount,
		cost,
		p95,
		bucket.SumInputTokens,
		bucket.SumOutputTokens,
	)
}

// renderProviderBucket は provider 集計 1 件を 1 行に整形する (Req 4.2)。
//
// workflow 集計と母集団単位 (RunCount vs CallCount) と識別子 (WorkflowID vs
// ProviderID) のみが異なるため、表示フォーマットは renderWorkflowBucket と
// 揃えて読み手の認知負荷を減らす。
func renderProviderBucket(bucket record.ProviderAggregateBucket, barLen int) string {
	rate := bucket.SuccessRate
	pct := int(rate*100 + 0.5)
	bar := renderASCIIBar(rate, barLen)
	cost := formatNullableCost(bucket.AvgCostUSD)
	p95 := formatNullableDurationMS(bucket.P95DurationMS)
	return fmt.Sprintf("  %-20s %3d%% %s calls=%-5d avg=%s p95=%s tokens=%d/%d",
		bucket.ProviderID,
		pct,
		bar,
		bucket.CallCount,
		cost,
		p95,
		bucket.SumInputTokens,
		bucket.SumOutputTokens,
	)
}

// renderASCIIBar は success rate に対する ASCII バー文字列を返す (Req 4.7)。
//
// フォーマット: "[" + "#" * filled + "-" * (length - filled) + "]"
//
//	filled = round(rate * length)
//
// クランプ規則:
//   - length < 10 → 10 (画面幅が極端に狭くても最低 10 文字を確保)
//   - length > 20 → 20 (グラフィカル描画になりうる長尺を抑制、Req 4.7 の
//     「ASCII 文字ベースの簡易バー (横幅 1 行内)」に収める)
//   - rate < 0    → 0 (全 "-")
//   - rate > 1    → 1 (全 "#")
//
// 丸めは int(rate * length + 0.5) の四捨五入。length と filled の不整合を
// 防ぐため、最終的な filled も [0, length] にクランプする。
func renderASCIIBar(rate float64, length int) string {
	if length < 10 {
		length = 10
	}
	if length > 20 {
		length = 20
	}
	if rate < 0 {
		rate = 0
	}
	if rate > 1 {
		rate = 1
	}
	filled := int(rate*float64(length) + 0.5)
	if filled < 0 {
		filled = 0
	}
	if filled > length {
		filled = length
	}
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", length-filled) + "]"
}

// barLengthForWidth は画面幅に応じた ASCII バー長さを返す (Req 4.7)。
//
// 区分:
//   - width >= 100 → 20 文字 (広い画面でも 1 行に収まる範囲で詳細さを増やす)
//   - width >=  70 → 15 文字 (中間幅: バランス)
//   - その他       → 10 文字 (狭い画面: 最小長さ)
//
// 区分の境界値は意図的に少なく保ち、表示挙動の予測性を優先する (テストの
// 観測安定性も担保される)。
func barLengthForWidth(width int) int {
	switch {
	case width >= 100:
		return 20
	case width >= 70:
		return 15
	default:
		return 10
	}
}

// detailListWindow は Detail サブビュー内 list セクション (agent_calls /
// agent_context_usage / raw_events / artifacts) の同時表示行数。
//
// WithAltScreen の固定キャンバスでは画面高さが有限のため、件数が多いと Cursor
// 行が画面外に押し出されて視認できなくなる UX 問題があった。各セクションに
// 本値の window を適用し、focused セクションは Cursor 中央配置、non-focused は
// 先頭基準で表示することで、画面サイズに依存せず Cursor 行が常に映るようにする。
//
// 11 件の根拠: 4 セクション × 11 行 = 44 行 + ヘッダ / メタ / 状態行 / ヒントを
// 加えても 60 行程度に収まり、一般的な terminal 高さ (24〜50 行) でも
// focused セクションの Cursor 行が必ず描画範囲に入る。
const detailListWindow = 11

// listWindow は Run 一覧サブビュー (renderList) の同時表示行数 (#11)。
//
// 一覧画面は他セクションと縦に並ばず、ヘッダ (section / sort-filter line) と
// フッタ (window / paging / hint) を合わせても overhead が 6〜7 行に収まるため、
// detailListWindow より大きく取れる。一般的な terminal 高さ (24〜50 行) で
// Cursor 行が常に映ることを優先しつつ、過剰に長くしないバランスとして 20 行を
// 採用する (overhead 7 + window 20 = 27 行で 24 行端末でも実害が小さい)。
//
// model.View の clipFromTop と組み合わせたときの安全性: 本 window で行数を
// 抑えることで、newest-first の Items が clipFromTop で先頭ごと捨てられて
// 「新しい側が見えない」状態になる #11 の症状を構造的に解消する。
const listWindow = 20

// clipListWindow は cursor を中心に items の表示範囲を window 件に絞り込む純関数。
//
// 戻り値 (start, end) は items[start:end] のインデックス範囲 (end exclusive)。
// 不変条件: 0 <= start <= end <= total かつ start <= cursor < end (total>0 のとき)。
//
// クランプ規則:
//   - total <= 0       → (0, 0)
//   - window <= 0      → (0, total)  (異常値の graceful 経路: 全件表示)
//   - total <= window  → (0, total)  (全件そのまま)
//   - cursor 中央配置: half = window/2
//   - cursor が左端寄り (cursor < half)            → (0, window)
//   - cursor が中央付近                             → (cursor-half, cursor-half+window)
//   - cursor が右端寄り (cursor+window-half > total) → (total-window, total)
//
// 「cursor が必ず映る」不変条件は呼び出し側 (View) で items[start:end] が
// 描画されたとき選択行ハイライトが画面に出ることを構造的に保証する。
func clipListWindow(cursor, total, window int) (int, int) {
	if total <= 0 {
		return 0, 0
	}
	if window <= 0 || total <= window {
		return 0, total
	}
	half := window / 2
	start := cursor - half
	end := start + window
	if start < 0 {
		start = 0
		end = window
	}
	if end > total {
		end = total
		start = total - window
	}
	return start, end
}

// formatNullableCost は *float64 のコストを "$0.0123" 形式 / nil → "-" で整形する。
//
// 小数 4 桁固定の理由: USD コスト (例: $0.0001 〜 $1.0) は agent_calls の典型値
// として小数第 3 〜 4 位に有意差が出るため、4 桁で十分な解像度を確保する。
func formatNullableCost(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("$%.4f", *v)
}

// formatNullableDurationMS は *int64 の所要 ms を "1234ms" 形式 / nil → "-" で
// 整形する。秒以上に丸めない理由は p95 を ms 単位の生数値で比較するため。
func formatNullableDurationMS(v *int64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%dms", *v)
}

// renderRunListItem は WorkflowRunListItem を 1 行の表示文字列に変換する。
//
// 出力フォーマット: "<cursor><success-mark> [<started_at>] <workflow_label>  <run_id>"
//
//	cursor:       "› " (selected) / "  " (other) -- renderChoiceWithWidth が付与
//	success-mark: "✓" (Success=true) / "✗" (Success=false) / "?" (Success=nil)
//	started_at:   "YYYY-MM-DD HH:MM:SS"
//	workflow:     WorkflowName を優先、なければ WorkflowID
//	run_id:       metaStyle で suffix 表示 (renderChoiceWithWidth に委譲)
//
// 既存 renderChoiceWithWidth (model.go) を再利用することで、選択行ハイライトと
// suffix 表示の一貫性をワークフロー選択画面と揃える。
func renderRunListItem(selected bool, item record.WorkflowRunListItem, width int) string {
	label := item.WorkflowID
	if item.WorkflowName != "" {
		label = item.WorkflowName
	}
	main := fmt.Sprintf("%s [%s] %s", successMarkOf(item.Success), item.StartedAt.Format("2006-01-02 15:04:05"), label)
	return renderChoiceWithWidth(selected, main, item.RunID, width)
}

// successMarkOf は *bool 由来の成功フラグを 1 文字シンボルに変換する。
//
//   - nil   → "?" (未確定 / 未完了)
//   - true  → "✓" (成功)
//   - false → "✗" (失敗)
//
// Run 詳細 / agent_call 行 / Run 一覧 で同じ規則を共有することで、TUI 上の
// 「成功 / 失敗 / 未完」表現を一意に揃える。
func successMarkOf(success *bool) string {
	if success == nil {
		return "?"
	}
	if *success {
		return "✓"
	}
	return "✗"
}

// renderDetail は Run 詳細サブビューを描画する (Req 5.1〜5.5 / 6.1〜6.9 / 7.7 /
// 11.2 / 11.3)。
//
// 排他描画の優先順 (renderList / renderSummary と同じパターン):
//  1. Reader == nil       → 「履歴データが利用できません」(Req 11.2) のみ表示し
//     Detail 本体 / Artifact 領域は一切描画しない (stale 排除)
//  2. Detail.Loading      → 「読み込み中...」のみ
//  3. Detail.Err is ErrWorkflowRunNotFound → runDetailNotFoundMessage (Req 5.4)
//  4. Detail.Err その他   → エラー文 (warningStyle, Req 5.5)
//  5. それ以外            → workflow_run / agent_calls / raw_events / 成果物 を縦分割で描画
//
// Detail 本体描画 (5) では:
//   - workflow_run ヘッダ (run_id / workflow / cwd / 期間 / total_cost)
//   - agent_calls 一覧 (started_at 昇順、Reader 戻り順を尊重)
//   - raw_events 一覧 (sequence 昇順)
//   - 成果物 pane (メタ一覧 + 選択中のプレビュー本体 / 警告)
//
// が縦に並び、Focus に応じて該当行 / 領域に renderChoiceWithWidth 経由の
// 「›」カーソル記号が乗る。
//
// HandoffErr (Req 7.7) は本体描画と排他にせず、本体冒頭の inline 通知として
// 上部に追加表示する (起動失敗後も Detail を確認し続けられるようにするため)。
func (s State) renderDetail(width int) string {
	var b strings.Builder
	b.WriteString(sectionStyle.Render("アカシックモード - Run 詳細"))
	b.WriteString("\n")

	if s.Reader == nil {
		b.WriteString(withWidth(mutedStyle, width).Render("履歴データが利用できません"))
		b.WriteString("\n")
		return b.String()
	}

	if s.Detail.Loading {
		b.WriteString(withWidth(mutedStyle, width).Render("読み込み中..."))
		b.WriteString("\n")
		return b.String()
	}

	if s.Detail.Err != nil {
		if errors.Is(s.Detail.Err, record.ErrWorkflowRunNotFound) {
			b.WriteString(withWidth(mutedStyle, width).Render(runDetailNotFoundMessage))
			b.WriteString("\n")
			return b.String()
		}
		b.WriteString(withWidth(warningStyle, width).Render(fmt.Sprintf("エラー: %s", s.Detail.Err.Error())))
		b.WriteString("\n")
		return b.String()
	}

	// HandoffErr の inline 通知 (Req 7.7)。本体描画と排他にせず、上部に表示する。
	if s.Detail.HandoffErr != nil {
		b.WriteString(withWidth(warningStyle, width).Render(fmt.Sprintf("hand-off エラー: %s", s.Detail.HandoffErr.Error())))
		b.WriteString("\n")
	}

	// Focus / Cursor 状態行 (Tab/j/k の効きを常時可視化する)。
	// 件数が多くて Cursor 行が画面外に押し出されている場面でも、本行の文字列が
	// キー操作で更新されることで「キーが効いているか」をユーザが判別できる。
	b.WriteString(renderDetailFocusStatus(s.Detail, width))

	// workflow_run セクション
	b.WriteString(metaStyle.Render("[workflow_run]"))
	b.WriteString("\n")
	b.WriteString(renderDetailHeader(s.Detail, s.Detail.Focus == DetailFocusHeader, width))

	// agent_calls セクション (Req 5.2)
	b.WriteString(metaStyle.Render("[agent_calls]"))
	b.WriteString("\n")
	b.WriteString(renderDetailAgentCalls(s.Detail, width))

	// agent_context_usage セクション (Req 5.1 / 5.2 / 8.1: コンテキスト使用量を可視化)
	b.WriteString(metaStyle.Render("[agent_context_usage]"))
	b.WriteString("\n")
	b.WriteString(renderDetailContextUsage(s.Detail, width))

	// raw_events セクション (Req 5.2 / 5.3)
	b.WriteString(metaStyle.Render("[raw_events]"))
	b.WriteString("\n")
	b.WriteString(renderDetailRawEvents(s.Detail, width))

	// 成果物 pane (Req 6)
	b.WriteString(metaStyle.Render("[成果物]"))
	b.WriteString("\n")
	b.WriteString(renderDetailArtifacts(s.Detail.Artifact, s.Detail.Focus == DetailFocusArtifacts, width))

	return b.String()
}

// renderDetailFocusStatus は Detail サブビューの Focus / Cursor 状態を 1 行で
// 描画する (画面ヘッダ直下に常設)。
//
// 出力フォーマット: "Focus: <name>  Cursor: <i+1>/<n>"
//
// Focus 名:
//   - DetailFocusAgentCalls → "agent_calls"
//   - DetailFocusRawEvents  → "raw_events"
//   - DetailFocusArtifacts  → "artifacts"
//   - DetailFocusHeader / 未知 → "header"
//
// Cursor 表示:
//   - 該当セクションの件数 n>0 のとき "<i+1>/<n>" (1-indexed: ユーザ視点で読みやすく)
//   - n==0 のとき "-/0" (graceful: Cursor 不在を明示)
//
// 動機: Detail 画面は agent_calls / raw_events / artifacts 全件を縦に並べるため、
// 件数が多いと WithAltScreen の固定キャンバスから Cursor 行が押し出されて視認
// できなくなる。本状態行はキー操作 (Tab/Shift+Tab/j/k) で必ず文字列が更新される
// ため、ユーザは「キーが効いているか」を画面に映る 1 行で常に確認できる。
func renderDetailFocusStatus(d DetailState, width int) string {
	var focusLabel string
	var cursor, total int
	switch d.Focus {
	case DetailFocusAgentCalls:
		focusLabel = "agent_calls"
		cursor = d.AgentCallsCursor
		if d.Detail != nil {
			total = len(d.Detail.AgentCalls)
		}
	case DetailFocusRawEvents:
		focusLabel = "raw_events"
		cursor = d.RawEventsCursor
		if d.Detail != nil {
			total = len(d.Detail.RawEvents)
		}
	case DetailFocusArtifacts:
		focusLabel = "artifacts"
		cursor = d.Artifact.SelectedIndex
		total = len(d.Artifact.Rows)
	default:
		focusLabel = "header"
	}
	var pos string
	if total > 0 {
		pos = fmt.Sprintf("%d/%d", cursor+1, total)
	} else {
		pos = "-/0"
	}
	line := fmt.Sprintf("Focus: %s  Cursor: %s", focusLabel, pos)
	return withWidth(metaStyle, width).Render(line) + "\n"
}

// renderDetailHeader は workflow_run ヘッダ領域 (run / workflow / cwd / 期間) を
// 1〜2 行で描画する。Detail.Detail が nil (= 未取得) の場合は muted で
// 「(取得待ち)」を 1 行表示する。Focus=Header のとき selectedStyle 経路の
// renderChoiceWithWidth を使ってカーソル記号「›」を乗せる。
func renderDetailHeader(d DetailState, focused bool, width int) string {
	var b strings.Builder
	if d.Detail == nil {
		b.WriteString(withWidth(mutedStyle, width).Render("  (取得待ち)"))
		b.WriteString("\n")
		return b.String()
	}
	run := d.Detail.Run
	finished := "-"
	if run.FinishedAt != nil {
		finished = run.FinishedAt.Format("2006-01-02 15:04:05")
	}
	main := fmt.Sprintf("%s %s started=%s finished=%s",
		successMarkOf(run.Success),
		runHeaderLabel(run),
		run.StartedAt.Format("2006-01-02 15:04:05"),
		finished,
	)
	suffix := run.RunID
	// WorkflowID は WorkflowName と異なるとき suffix に併記する。
	// (Name 優先表示の利便性と、ログでの workflow_id 検索性の両立)。
	if run.WorkflowID != "" && run.WorkflowID != runHeaderLabel(run) {
		suffix = run.WorkflowID + " · " + run.RunID
	}
	b.WriteString(renderChoiceWithWidth(focused, main, suffix, width))
	b.WriteString("\n")
	if run.CWD != "" {
		b.WriteString(withWidth(metaStyle, width).Render("  cwd: " + run.CWD))
		b.WriteString("\n")
	}
	if run.Error != "" {
		b.WriteString(withWidth(warningStyle, width).Render("  error: " + run.Error))
		b.WriteString("\n")
	}
	return b.String()
}

// runHeaderLabel は表示用の workflow ラベルを返す (Name 優先、なければ ID)。
func runHeaderLabel(run record.RunHeader) string {
	if run.WorkflowName != "" {
		return run.WorkflowName
	}
	return run.WorkflowID
}

// renderDetailAgentCalls は agent_calls 一覧を描画する (Req 5.2)。
// Focus=AgentCalls + AgentCallsCursor の指す行に「›」を乗せる。
// Detail 未取得 / 件数 0 の場合は muted で「(agent_calls なし)」を 1 行表示する。
//
// 件数が detailListWindow を超える場合は clipListWindow で表示範囲を絞り込む。
// focused なら Cursor 中心、non-focused なら先頭基準で window を取り、末尾に
// 「showing X-Y / total」フッタを出して窓外があることを告知する。これにより
// 件数が多くても画面圧迫せず、focused 時には Cursor 行が必ず描画される。
func renderDetailAgentCalls(d DetailState, width int) string {
	if d.Detail == nil || len(d.Detail.AgentCalls) == 0 {
		return withWidth(mutedStyle, width).Render("  (agent_calls なし)") + "\n"
	}
	calls := d.Detail.AgentCalls
	total := len(calls)
	cursor := 0
	if d.Focus == DetailFocusAgentCalls {
		cursor = d.AgentCallsCursor
	}
	start, end := clipListWindow(cursor, total, detailListWindow)
	var b strings.Builder
	for i := start; i < end; i++ {
		selected := d.Focus == DetailFocusAgentCalls && i == d.AgentCallsCursor
		b.WriteString(renderAgentCallRow(selected, calls[i], width))
		b.WriteString("\n")
	}
	if start > 0 || end < total {
		b.WriteString(withWidth(mutedStyle, width).Render(
			fmt.Sprintf("  showing %d-%d / %d", start+1, end, total)))
		b.WriteString("\n")
	}
	// AgentCallPaneOpen のとき選択行 (AgentCallsCursor) の inline 詳細 pane を描画する。
	// Focus 状態に依らず pane state のみで開閉判定するため、別 Focus 中でも
	// 過去に開かれた pane を残せる (Req 5: 失敗原因確認の継続性)。
	if d.AgentCallPaneOpen && d.AgentCallsCursor >= 0 && d.AgentCallsCursor < total {
		b.WriteString(renderAgentCallPane(calls[d.AgentCallsCursor], width))
	}
	return b.String()
}

// renderAgentCallPane は AgentCallRow の 1 行に収まらないリッチフィールド
// (Prompt / ResultSummary / Error / FailureCategory / ResultPayload) を inline pane
// として複数行に展開する (Req 5: 外部 SQL クライアント無しで失敗原因を確認可能にする)。
//
// 空フィールドは行ごとスキップする (短い run / 成功 run でも余計な空行を増やさない)。
// Error / FailureCategory は warningStyle、それ以外は metaStyle / itemStyle で描画して
// 視覚的に「何が問題か」をユーザに伝える。
func renderAgentCallPane(call record.AgentCallRow, width int) string {
	var b strings.Builder
	b.WriteString(withWidth(metaStyle, width).Render("  --- agent_call detail ---"))
	b.WriteString("\n")
	if call.Prompt != "" {
		b.WriteString(withWidth(itemStyle, width).Render("  prompt: " + call.Prompt))
		b.WriteString("\n")
	}
	if call.ResultSummary != "" {
		b.WriteString(withWidth(itemStyle, width).Render("  summary: " + call.ResultSummary))
		b.WriteString("\n")
	}
	if call.Error != "" {
		b.WriteString(withWidth(warningStyle, width).Render("  error: " + call.Error))
		b.WriteString("\n")
	}
	if call.FailureCategory != "" {
		b.WriteString(withWidth(warningStyle, width).Render("  failure_category: " + call.FailureCategory))
		b.WriteString("\n")
	}
	if len(call.ResultPayload) > 0 {
		b.WriteString(withWidth(metaStyle, width).Render("  payload:"))
		b.WriteString("\n")
		b.WriteString(withWidth(itemStyle, width).Render("  " + string(call.ResultPayload)))
		b.WriteString("\n")
	}
	return b.String()
}

// renderAgentCallRow は AgentCallRow を 1 行表示文字列に変換する。
//
// 出力フォーマット: "<cursor><success-mark> [<started_at>] <step> <provider>  <id>"
//   - 失敗時は failure_category を suffix に追加
//
// renderChoiceWithWidth を再利用してカーソル記号と幅整形を一貫させる。
func renderAgentCallRow(selected bool, call record.AgentCallRow, width int) string {
	step := call.StepID
	if call.StepName != "" {
		step = call.StepName
	}
	main := fmt.Sprintf("%s [%s] %s %s",
		successMarkOf(call.Success),
		call.StartedAt.Format("2006-01-02 15:04:05"),
		step,
		call.ProviderID,
	)
	suffix := call.ID
	if call.FailureCategory != "" {
		suffix = call.ID + " · " + call.FailureCategory
	}
	return renderChoiceWithWidth(selected, main, suffix, width)
}

// renderDetailContextUsage は agent_context_usage 一覧を描画する (Req 5.1 / 5.2 / 8.1)。
//
// 1 行表示フォーマット: "[seq=<n>] <model> tokens=<total>/<max> (<pct>%)  <observed_at>"
//
// 本セクションはフォーカス対象ではないため (DetailFocus に該当値がない)、
// カーソル「›」は付けず muted な参照情報として並べる。Detail 未取得 / 件数 0 の
// ときは muted で「(agent_context_usage なし)」を 1 行表示する (agent_calls /
// raw_events と同じ空状態パターン)。
func renderDetailContextUsage(d DetailState, width int) string {
	if d.Detail == nil || len(d.Detail.ContextUsage) == 0 {
		return withWidth(mutedStyle, width).Render("  (agent_context_usage なし)") + "\n"
	}
	usages := d.Detail.ContextUsage
	total := len(usages)
	// 本セクションは Focus 対象ではないため、常に先頭基準で window を取る
	// (画面圧迫防止が主目的)。件数告知のために窓外があれば末尾フッタを出す。
	start, end := clipListWindow(0, total, detailListWindow)
	var b strings.Builder
	for i := start; i < end; i++ {
		b.WriteString(renderContextUsageRow(usages[i], width))
		b.WriteString("\n")
	}
	if start > 0 || end < total {
		b.WriteString(withWidth(mutedStyle, width).Render(
			fmt.Sprintf("  showing %d-%d / %d", start+1, end, total)))
		b.WriteString("\n")
	}
	return b.String()
}

// renderContextUsageRow は AgentContextUsageRow を 1 行表示文字列に変換する。
//
// 出力フォーマット: "  [seq=<n>] <model> tokens=<total>/<max> (<pct>%)  <observed_at>"
//
// MaxTokens=0 のときは "tokens=<total>" で打ち切る (ゼロ除算回避を兼ねた表記)。
// renderChoiceWithWidth を使わず素の itemStyle で描画するのは、本セクションが
// フォーカス遷移の対象ではないため (Tab 巡回からは除外される)。
func renderContextUsageRow(row record.AgentContextUsageRow, width int) string {
	var tokens string
	if row.MaxTokens > 0 {
		tokens = fmt.Sprintf("tokens=%d/%d (%d%%)", row.TotalTokens, row.MaxTokens, row.Percentage)
	} else {
		tokens = fmt.Sprintf("tokens=%d", row.TotalTokens)
	}
	main := fmt.Sprintf("  [seq=%d] %s %s  %s",
		row.Sequence,
		row.Model,
		tokens,
		row.ObservedAt.Format("2006-01-02 15:04:05"),
	)
	return withWidth(itemStyle, width).Render(main)
}

// renderDetailRawEvents は raw_events 一覧を描画する (Req 5.2 / 5.3)。
// Focus=RawEvents + RawEventsCursor の指す行に「›」を乗せる。
// 末尾に現在ページ Offset / next の有無を muted で表示する (ページング操作の手掛かり)。
func renderDetailRawEvents(d DetailState, width int) string {
	if d.Detail == nil || len(d.Detail.RawEvents) == 0 {
		return withWidth(mutedStyle, width).Render("  (raw_events なし)") + "\n"
	}
	events := d.Detail.RawEvents
	total := len(events)
	cursor := 0
	if d.Focus == DetailFocusRawEvents {
		cursor = d.RawEventsCursor
	}
	start, end := clipListWindow(cursor, total, detailListWindow)
	var b strings.Builder
	for i := start; i < end; i++ {
		selected := d.Focus == DetailFocusRawEvents && i == d.RawEventsCursor
		b.WriteString(renderRawEventRow(selected, events[i], width))
		b.WriteString("\n")
	}
	if start > 0 || end < total {
		b.WriteString(withWidth(mutedStyle, width).Render(
			fmt.Sprintf("  showing %d-%d / %d", start+1, end, total)))
		b.WriteString("\n")
	}
	// ページングフッタ (現在ページ offset と next の有無、Req 5.3 操作の手掛かり)
	if d.NextRawEventsOffset != nil {
		b.WriteString(withWidth(mutedStyle, width).Render(fmt.Sprintf("  page offset: %d (next: %d)", d.RawEventsOffset, *d.NextRawEventsOffset)))
	} else {
		b.WriteString(withWidth(mutedStyle, width).Render(fmt.Sprintf("  page offset: %d (last)", d.RawEventsOffset)))
	}
	b.WriteString("\n")
	// RawEventPaneOpen のとき選択行 (RawEventsCursor) の inline 詳細 pane を描画する。
	// Focus 状態に依らず pane state のみで開閉判定する (agent_call pane と対称)。
	if d.RawEventPaneOpen && d.RawEventsCursor >= 0 && d.RawEventsCursor < total {
		b.WriteString(renderRawEventPane(events[d.RawEventsCursor], width))
	}
	return b.String()
}

// renderRawEventPane は RawEventRow の Payload を inline pane として展開する
// (Req 5: 外部 SQL クライアント無しで raw event 内容を確認可能にする)。
//
// PayloadTruncated=true のときは reader 側で本文が切り詰められたサインなので
// "payload truncated" 警告を出して、ユーザに「画面に出ているのは抜粋」と伝える。
func renderRawEventPane(ev record.RawEventRow, width int) string {
	var b strings.Builder
	b.WriteString(withWidth(metaStyle, width).Render("  --- raw_event detail ---"))
	b.WriteString("\n")
	if len(ev.Payload) > 0 {
		b.WriteString(withWidth(itemStyle, width).Render("  payload: " + string(ev.Payload)))
		b.WriteString("\n")
	}
	if ev.PayloadTruncated {
		b.WriteString(withWidth(warningStyle, width).Render("  payload truncated (raw event 本文が切り詰められています)"))
		b.WriteString("\n")
	}
	return b.String()
}

// renderRawEventRow は RawEventRow を 1 行表示文字列に変換する。
//
// 出力フォーマット: "<cursor>[seq=<n>] <type>/<subtype> <source>  <observed_at>"
func renderRawEventRow(selected bool, ev record.RawEventRow, width int) string {
	main := fmt.Sprintf("[seq=%d] %s/%s %s",
		ev.Sequence,
		ev.Type,
		ev.Subtype,
		ev.Source,
	)
	return renderChoiceWithWidth(selected, main, ev.ObservedAt.Format("2006-01-02 15:04:05"), width)
}

// renderDetailArtifacts は成果物 pane を描画する (Req 6.1〜6.9 / 11.3)。
//
// 排他描画の優先順 (Artifact pane 内で閉じる):
//  1. Unavailable=true     → 「成果物データが利用できません」(Req 11.3)
//  2. Loading=true         → 「読み込み中...」
//  3. Err != nil           → エラー文 (warningStyle, Req 5.5 と対称)
//  4. EmptyMessage != ""   → 「成果物なし」(Req 6.9)
//  5. それ以外             → メタ一覧 + 選択行のプレビュー本体 / 警告
//
// プレビュー本体 (PreviewBytes) は Markdown シンタックスハイライトを行わず raw 表示する (Req 6.4)。
// PreviewWarning (上限超過 / 削除済) はメタ一覧の下に並べて表示し、メタ一覧自体は
// 引き続き表示する (Req 6.5 後段「メタは継続表示」)。
func renderDetailArtifacts(a ArtifactState, focused bool, width int) string {
	if a.Unavailable {
		return withWidth(mutedStyle, width).Render("  成果物データが利用できません") + "\n"
	}
	if a.Loading {
		return withWidth(mutedStyle, width).Render("  読み込み中...") + "\n"
	}
	if a.Err != nil {
		return withWidth(warningStyle, width).Render(fmt.Sprintf("  エラー: %s", a.Err.Error())) + "\n"
	}
	if a.EmptyMessage != "" {
		return withWidth(mutedStyle, width).Render("  "+a.EmptyMessage) + "\n"
	}

	var b strings.Builder
	// メタ一覧 (件数が多いと画面圧迫するため window クリップ表示)
	rows := a.Rows
	total := len(rows)
	cursor := 0
	if focused {
		cursor = a.SelectedIndex
	}
	start, end := clipListWindow(cursor, total, detailListWindow)
	for i := start; i < end; i++ {
		selected := focused && i == a.SelectedIndex
		b.WriteString(renderArtifactRow(selected, rows[i], width))
		b.WriteString("\n")
	}
	if start > 0 || end < total {
		b.WriteString(withWidth(mutedStyle, width).Render(
			fmt.Sprintf("  showing %d-%d / %d", start+1, end, total)))
		b.WriteString("\n")
	}

	// プレビュー警告 (上限超過 / 削除済)。メタ一覧の下に並べる (Req 6.5 / 6.7)。
	if a.PreviewWarning != "" {
		b.WriteString(withWidth(warningStyle, width).Render("  " + a.PreviewWarning))
		b.WriteString("\n")
	}

	// プレビュー本体 (PreviewBytes) は raw 表示する (Req 6.4 - Markdown ハイライト無し)。
	if len(a.PreviewBytes) > 0 {
		if a.PreviewPath != "" {
			b.WriteString(withWidth(metaStyle, width).Render("  preview: " + a.PreviewPath))
			b.WriteString("\n")
		}
		b.WriteString(withWidth(itemStyle, width).Render(string(a.PreviewBytes)))
		b.WriteString("\n")
	}
	return b.String()
}

// renderArtifactRow は ArtifactRow を 1 行表示文字列に変換する。
//
// 出力フォーマット: "<cursor>[<kind>] <path>  <size>B <mime>"
func renderArtifactRow(selected bool, row record.ArtifactRow, width int) string {
	main := fmt.Sprintf("[%s] %s", row.Kind, row.Path)
	suffix := fmt.Sprintf("%dB %s", row.SizeBytes, row.MIME)
	return renderChoiceWithWidth(selected, main, suffix, width)
}
