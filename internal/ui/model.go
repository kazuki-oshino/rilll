package ui

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/bridge"
	"github.com/kazuki-oshino/rilll/internal/config"
	"github.com/kazuki-oshino/rilll/internal/notify"
	"github.com/kazuki-oshino/rilll/internal/provider"
	uiakashic "github.com/kazuki-oshino/rilll/internal/ui/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflow"
)

// heartbeat tick の間隔。スピナーアニメと「最後にイベントを受信してからの経過秒」表示用の再描画。
const heartbeatInterval = 200 * time.Millisecond

// 生存表示しきい値。idle / stale 判定のカットオフ。
const (
	heartbeatActiveThreshold = 1 * time.Second
	heartbeatStaleThreshold  = 5 * time.Second
)

// 直近のストリーム蓄積上限 (行数ベースの軽い切り詰め。表示は bodyHeight に応じた末尾のみ)。
const maxStreamLines = 4000

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// playCompletionSound はワークフロー全体完了時に呼ぶ通知音再生フック。
// テストから差し替え可能にするためパッケージ変数として保持する
// (TUI 経路で afplay が 1 回だけ鳴ることを spy で検証する用途)。
var playCompletionSound = notify.PlayCompletionSound

type Phase string

const (
	PhaseMenu                   Phase = "menu"
	PhaseWorkflow               Phase = "workflow"
	PhaseProviderSettings       Phase = "provider-settings"
	PhaseCodexModelSettings     Phase = "codex-model-settings"
	PhaseCursorSDKModelSettings Phase = "cursor-sdk-model-settings"
	PhasePrompt                 Phase = "prompt"
	PhaseDone                   Phase = "done"
	// PhaseAkashic はアカシックモード (Run 一覧 / 集計サマリ / Run 詳細)。
	// 本タスク (2.1) では Phase 列挙への追加のみで、突入経路の実装は後続タスク (8.1) で行う。
	PhaseAkashic Phase = "akashic"
)

type Options struct {
	CWD          string
	BuildVersion string
	Config       config.Config
	Warning      string
	Workflows    []workflow.Definition
	SaveConfig   func(config.Config) error
	EventBuffer  chan tea.Msg
	// Reader はアカシックレコード読み取り用 (akashic-mode-tui Task 2.1 で導入)。
	// nil 許容: NoopRecorder fallback 時は nil で渡され、TUI 側が graceful 表示にフォールバックする (Req 11.1〜11.5)。
	Reader akashic.Reader
	// ArtifactReader は成果物読み取り用 (akashic-mode-tui Task 2.1 で導入)。nil 許容。
	ArtifactReader akashic.ArtifactReader
	// ActiveCancelOrigin は foreground にいる agent_call の cancel-origin 用 *atomic.Value を
	// 保持するための Pointer (memory-distiller-trigger-policy task 4.1, Req 3.1 / 3.2)。
	//
	// UI Model 寿命と等価な単一 Pointer を cmd/rilll/main.go から渡す。指す先 (call-scoped
	// *atomic.Value) は agent.CallAgent 入口/return で register/unregister されることを想定し、
	// UI ctrl+c handler は `Load()` で取得した ref に bridge.CancelOriginUser を Store する。
	//
	// nil 許容: 旧経路 / テストでは未注入のまま、ctrl+c handler 側で no-op 分岐する
	// (= 既存挙動保全 / Req 6)。
	ActiveCancelOrigin *atomic.Pointer[atomic.Value]
}

type Model struct {
	CWD               string
	BuildVersion      string
	Config            config.Config
	Warning           string
	Workflows         []workflow.Definition
	SaveConfig        func(config.Config) error
	Phase             Phase
	Cursor            int
	ProviderPos       int
	LevelPos          int
	SettingsCursor    int
	CodexModelPos     int
	CursorSDKModelPos int
	Width             int
	Height            int

	StepText             string
	ReasoningText        string
	StreamText           string
	CurrentTool          string
	Results              []resultEntry
	CurrentWorkflowLabel string

	// WorkflowStartedAt はワークフロー実行開始時刻 (PhaseWorkflow 突入時にセット)。
	WorkflowStartedAt time.Time
	// WorkflowDuration はワークフロー完了時に確定する累計実行時間。PhaseDone での total 表示に使う。
	WorkflowDuration time.Duration
	// LastEventAt は agent 由来の event を最後に受信した時刻。生存表示の根拠。
	LastEventAt time.Time
	// SpinnerFrame は heartbeat tick ごとに進む 0..len(spinnerFrames)-1 のインデックス。
	SpinnerFrame int

	Prompt             string
	PromptItems        []workflow.SelectItem
	promptCursor       int
	promptReply        chan promptReply
	promptOrdered      bool
	promptSelectedIDs  []string
	orderedPromptReply chan orderedPromptReply

	events chan tea.Msg

	// Reader / ArtifactReader / Akashic はアカシックモード関連の依存と状態
	// (akashic-mode-tui Task 2.1 で導入)。本タスクでは Phase=PhaseAkashic 経路が
	// 未実装なので、これらのフィールドは既存 Update/View/handleKey からは参照しない。
	Reader         akashic.Reader
	ArtifactReader akashic.ArtifactReader
	Akashic        uiakashic.State

	// activeCancelOrigin は ctrl+c 時に foreground の agent_call へ cancel origin を
	// "user" として伝播するための Pointer (memory-distiller-trigger-policy task 4.1)。
	// Options.ActiveCancelOrigin から取り込まれ、`agent.CallAgent` 入口で
	// register、return 時に unregister されることを想定する。
	// nil 許容: 未注入時は ctrl+c handler が no-op 分岐し、既存挙動を保全する。
	activeCancelOrigin *atomic.Pointer[atomic.Value]
}

// resultEntry は完了画面で 1 ボックスとして描画する単一 child workflow の結果。
// SequentialWithLearningsTail などで複数 child の WorkflowDone が順に届くとき、
// それぞれを (child Label, 結果本文) の組として保持し縦スタック表示する。
type resultEntry struct {
	Label string
	Body  string
}

// appendResultEntry は dst の末尾に entry を追加する。
//   - entry.Body が空 (TrimSpace 後) のときは追加しない (無意味なボックスを描画しない)。
//   - 直前 entry と Label/Body が完全一致する場合は追加しない (Sequential wrapper 経由で
//     同一 WorkflowDone が二重に届いた場合の防御)。
func appendResultEntry(dst []resultEntry, entry resultEntry) []resultEntry {
	if strings.TrimSpace(entry.Body) == "" {
		return dst
	}
	if n := len(dst); n > 0 && dst[n-1].Label == entry.Label && dst[n-1].Body == entry.Body {
		return dst
	}
	return append(dst, entry)
}

type workflowEventMsg struct{ event workflow.WorkflowEvent }
type workflowDoneMsg struct{ err error }
type promptMsg struct {
	params workflow.SelectParams
	reply  chan promptReply
}
type orderedPromptMsg struct {
	params workflow.OrderedSelectParams
	reply  chan orderedPromptReply
}
type promptReply struct {
	id string
	ok bool
}
type orderedPromptReply struct {
	ids []string
	ok  bool
}

// heartbeatMsg は heartbeat tick 通知 (スピナー再描画と生存表示更新のトリガ)。
type heartbeatMsg time.Time

func heartbeat() tea.Cmd {
	return tea.Tick(heartbeatInterval, func(t time.Time) tea.Msg {
		return heartbeatMsg(t)
	})
}

var (
	appStyle = lipgloss.NewStyle().Padding(1, 2)

	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#7DD3FC"))
	metaStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#94A3B8"))
	providerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#A7F3D0")).
			Background(lipgloss.Color("#064E3B")).
			Padding(0, 1)
	sectionStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#F8FAFC")).
			Border(lipgloss.NormalBorder(), false, false, true, false).
			BorderForeground(lipgloss.Color("#334155"))
	// selectedStyle は一覧の選択行ハイライトに使う。Bold + amber foreground だけでは
	// 「選択中の 1 行」がどれかを把握しづらかったため、暗いスレート背景を足して
	// ハイライトバー風に視認できるようにしている (アカシックモード Detail 内の
	// agent_calls / raw_events / artifacts や Menu / Settings 共通)。
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
	resultStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#334155")).
			Padding(1, 2)

	heartbeatActiveStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#A7F3D0"))
	heartbeatIdleStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#FBBF24"))
	heartbeatStaleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#FCA5A5")).Bold(true)
)

func NewModel(options Options) Model {
	events := options.EventBuffer
	if events == nil {
		events = make(chan tea.Msg, 64)
	}
	cfg := options.Config
	defaultConfig := config.Default()
	if cfg.AgentProvider == "" {
		cfg.AgentProvider = defaultConfig.AgentProvider
	}
	if cfg.AkashicLevel == "" {
		cfg.AkashicLevel = defaultConfig.AkashicLevel
	}
	if cfg.Codex.ModelPreset == "" {
		cfg.Codex = defaultConfig.Codex
	}
	if cfg.CursorSDK.ModelPreset == "" {
		cfg.CursorSDK = defaultConfig.CursorSDK
	}
	model := Model{
		CWD:          options.CWD,
		BuildVersion: options.BuildVersion,
		Config:       cfg,
		Warning:      options.Warning,
		Workflows:    options.Workflows,
		SaveConfig:   options.SaveConfig,
		Phase:        PhaseMenu,
		events:       events,
		// アカシックモード関連の依存性 (akashic-mode-tui Task 2.1)。
		// 本タスクでは Phase=PhaseAkashic 経路が未実装のため、既存の Update/View/handleKey からは参照されない。
		Reader:         options.Reader,
		ArtifactReader: options.ArtifactReader,
		// ctrl+c 時に foreground call の cancel origin を "user" に倒す経路
		// (memory-distiller-trigger-policy task 4.1, Req 3.1 / 3.2)。
		// 未注入 (nil) の場合、handleKey の ctrl+c 分岐が no-op し既存挙動を保つ。
		activeCancelOrigin: options.ActiveCancelOrigin,
	}
	model.ProviderPos = providerIndex(cfg.AgentProvider)
	model.LevelPos = akashicLevelIndex(cfg.AkashicLevel)
	model.SettingsCursor = model.ProviderPos
	model.CodexModelPos = codexModelIndex(cfg.Codex.ModelPreset)
	model.CursorSDKModelPos = cursorSDKModelIndex(cfg.CursorSDK.ModelPreset)
	return model
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(waitForEvent(m.events), heartbeat())
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.Width = msg.Width
		m.Height = msg.Height
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	case heartbeatMsg:
		m.SpinnerFrame = (m.SpinnerFrame + 1) % len(spinnerFrames)
		return m, heartbeat()
	case workflowEventMsg:
		m.applyWorkflowEvent(msg.event)
		// 子の WorkflowDone / WorkflowError でも events ポンプは継続する
		// (Sequential 実行中、最初の子の終了イベント以降に届く後続イベントを
		// UI に反映させるため)。終端は workflowDoneMsg 側で行う。
		return m, waitForEvent(m.events)
	case workflowDoneMsg:
		// ワークフロー全体 (= トップレベル Definition.Run) が戻った瞬間の
		// 唯一のシグナル。Phase 遷移・afplay・Duration 確定はここに集約する
		// (子の WorkflowDone / WorkflowError event では実施しない)。
		if msg.err != nil {
			m.Results = appendResultEntry(m.Results, resultEntry{
				Label: m.CurrentWorkflowLabel,
				Body:  msg.err.Error(),
			})
		}
		if m.WorkflowDuration == 0 {
			m.WorkflowDuration = m.elapsedSinceStart()
		}
		m.Phase = PhaseDone
		playCompletionSound()
		return m, nil
	case promptMsg:
		m.Phase = PhasePrompt
		m.Prompt = msg.params.Prompt
		m.PromptItems = msg.params.Items
		m.promptCursor = 0
		m.promptReply = msg.reply
		m.promptOrdered = false
		m.promptSelectedIDs = nil
		m.orderedPromptReply = nil
		return m, nil
	case orderedPromptMsg:
		m.Phase = PhasePrompt
		m.Prompt = msg.params.Prompt
		m.PromptItems = msg.params.Items
		m.promptCursor = 0
		m.promptReply = nil
		m.promptOrdered = true
		m.promptSelectedIDs = nil
		m.orderedPromptReply = msg.reply
		return m, nil
	case uiakashic.HandoffStartedMsg:
		// Task 7.1, Req 7.8: hand-off 起動成功時のみ PhaseAkashic →
		// PhaseWorkflow に遷移する。akashic.State 側にも委譲し、
		// 内部状態 (Detail.HandoffErr) の整合を取る。
		// PhaseAkashic 以外で本 msg が届いた場合 (stale msg) は Phase を
		// 維持して受け流す (design.md Implementation Notes: Phase ガード)。
		if m.Phase != PhaseAkashic {
			return m, nil
		}
		m.Akashic, _ = m.Akashic.Update(msg)
		m.Phase = PhaseWorkflow
		return m, nil
	case uiakashic.HandoffFailedMsg:
		// Task 7.1, Req 7.7: hand-off 起動失敗時はモードを抜けず Detail
		// サブビューに留まり、akashic.State.Update に委譲して
		// Detail.HandoffErr に msg.Err を格納する。tea.Quit は発行しない。
		// PhaseAkashic 以外で届いた stale msg は受け流す。
		if m.Phase != PhaseAkashic {
			return m, nil
		}
		var cmd tea.Cmd
		m.Akashic, cmd = m.Akashic.Update(msg)
		return m, cmd
	case uiakashic.ListRunsMsg, uiakashic.AggregateByWorkflowMsg, uiakashic.AggregateByProviderMsg,
		uiakashic.RunDetailMsg, uiakashic.GetArtifactsMsg, uiakashic.ReadArtifactMsg:
		// Task 8.2 / Req 9.4 / 10.1 / 10.3: アカシック関連 msg は
		// Phase=PhaseAkashic のときのみ akashic.State.Update に委譲する。
		// PhaseAkashic 以外で届いた stale msg (退出後に async Cmd が完走して
		// 戻ってきたケース) は state を変更せず受け流す。
		// 再突入時は handleKey 'a' branch が akashic.NewState で値型再代入する
		// ため、stale msg は次セッションには影響しない構造的保証となる。
		if m.Phase != PhaseAkashic {
			return m, nil
		}
		var cmd tea.Cmd
		m.Akashic, cmd = m.Akashic.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m Model) View() string {
	header := m.renderHeader()
	if m.Warning != "" {
		header = lipgloss.JoinVertical(lipgloss.Left, header, warningStyle.Render(m.Warning))
	}
	footer := m.renderFooter()

	var body string
	switch m.Phase {
	case PhaseMenu:
		body = m.renderMenu()
	case PhaseProviderSettings:
		body = m.renderProviderSettings()
	case PhaseCodexModelSettings:
		body = m.renderCodexModelSettings()
	case PhaseCursorSDKModelSettings:
		body = m.renderCursorSDKModelSettings()
	case PhaseWorkflow:
		body = m.renderWorkflow()
	case PhasePrompt:
		body = m.renderPrompt()
	case PhaseDone:
		body = m.renderDone()
	case PhaseAkashic:
		// Task 8.1: PhaseAkashic では akashic.State.View にディスパッチする。
		// 既存 Phase のレンダリング経路は変更しない (Req 9.3 / 9.4)。
		body = m.Akashic.View(m.contentWidth())
	}

	// altscreen 下では View() の総行数がターミナル行数を超えるとスクロールが起き、
	// ヘッダーが上に押し出される。レンダリング済みの body を末尾優先で切り詰めて
	// header/footer を画面に固定する。
	if budget := m.bodyHeightBudget(header, footer); budget > 0 {
		body = clipFromTop(body, budget)
	}

	parts := []string{header, body}
	if footer != "" {
		parts = append(parts, footer)
	}
	return appStyle.Render(lipgloss.JoinVertical(lipgloss.Left, parts...))
}

// bodyHeightBudget は header/footer/枠 padding を差し引いた、body に許される最大行数を返す。
// Height 未取得 (起動直後) や狭すぎる場合は 0 を返し、呼び出し側は「無制限 (従来挙動)」として扱う。
func (m Model) bodyHeightBudget(header, footer string) int {
	if m.Height <= 0 {
		return 0
	}
	used := lipgloss.Height(header) + appStyle.GetVerticalFrameSize()
	if footer != "" {
		used += lipgloss.Height(footer)
	}
	budget := m.Height - used
	if budget < 1 {
		return 0
	}
	return budget
}

// clipFromTop はレンダリング済みの本体から末尾優先で budget 行のみ残す。
// lipgloss が width に応じて折り返した結果を含めて行数を数えるので、wrap 込みで正確に切り詰まる。
func clipFromTop(body string, budget int) string {
	lines := strings.Split(body, "\n")
	if len(lines) <= budget {
		return body
	}
	return strings.Join(lines[len(lines)-budget:], "\n")
}

func (m Model) renderHeader() string {
	title := "rilll"
	if m.BuildVersion != "" {
		title += " v" + m.BuildVersion
	}

	heading := lipgloss.JoinHorizontal(
		lipgloss.Center,
		titleStyle.Render(title),
		"  ",
		providerStyle.Render("provider: "+string(m.Config.AgentProvider)),
	)

	width := m.contentWidth()
	var lines []string
	if width > 0 {
		lines = append(lines, heading, metaStyle.Width(width).Render(m.CWD))
	} else {
		lines = append(lines, lipgloss.JoinHorizontal(
			lipgloss.Center,
			heading,
			"  ",
			metaStyle.Render(m.CWD),
		))
	}

	// PhaseWorkflow 中は "Running workflow: ..." セクション行と下ボーダーをヘッダーに含めて
	// 画面上部に固定する (本体ストリームが長くなって clip されても消えないようにする)。
	// StepText も同じ理由でヘッダー側に置く。これがないと StreamText overflow 時に
	// 「ステップ N/M: ...」が body 先頭から押し出されて見えなくなる (issue #10)。
	if m.Phase == PhaseWorkflow {
		label := "Running workflow"
		if m.CurrentWorkflowLabel != "" {
			label = "Running workflow: " + m.CurrentWorkflowLabel
		}
		lines = append(lines, sectionStyle.Render(label))
		if m.StepText != "" {
			lines = append(lines, withWidth(selectedStyle, width).Render(m.StepText))
		}
	}
	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}

func (m Model) renderMenu() string {
	var b strings.Builder
	b.WriteString(sectionStyle.Render("Workflows"))
	b.WriteString("\n")
	for i, item := range m.menuItems() {
		b.WriteString(renderChoice(i == m.Cursor, item.label, ""))
		b.WriteString("\n")
	}
	if len(m.menuItems()) == 0 {
		b.WriteString(mutedStyle.Render("  利用できるワークフローがありません"))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(mutedStyle.Render("↑/↓ 選択 · Enter 実行 · s 設定 · q 終了"))
	return b.String()
}

func (m Model) renderProviderSettings() string {
	var b strings.Builder
	b.WriteString(sectionStyle.Render("Settings"))
	b.WriteString("\n")
	b.WriteString(metaStyle.Render("Agent provider"))
	b.WriteString("\n")
	for i, option := range providerOptions() {
		current := ""
		if option.id == m.Config.AgentProvider {
			current = "現在"
		}
		b.WriteString(renderChoice(m.SettingsCursor == i, option.label, current))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(metaStyle.Render("アカシックレコード記録レベル"))
	b.WriteString("\n")
	providerCount := len(providerOptions())
	for i, option := range akashicLevelOptions() {
		suffixes := []string{}
		if option.id == m.Config.AkashicLevel {
			suffixes = append(suffixes, "現在")
		}
		if option.recommended {
			suffixes = append(suffixes, "おすすめ")
		}
		b.WriteString(renderChoice(m.SettingsCursor == providerCount+i, option.label, strings.Join(suffixes, " · ")))
		b.WriteString("\n")
	}
	// agent-learnings 学習対象 (opt-in) セクション。
	// 各行は `[✓]` / `[ ]` チェックボックス + workflow Label + (id) suffix。
	// toggle は space キー経路 (handleKey) に集約し、Enter は provider/level/otel 専用に維持する。
	b.WriteString("\n")
	b.WriteString(metaStyle.Render("OpenTelemetry"))
	b.WriteString("\n")
	otelOffset := settingsOtelOffset()
	for i, option := range otelOptions() {
		current := ""
		if option.enabled == m.Config.OtelEnabled {
			current = "現在"
		}
		b.WriteString(renderChoice(m.SettingsCursor == otelOffset+i, option.label, current))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(metaStyle.Render("agent-learnings 学習対象"))
	b.WriteString("\n")
	rows := m.optInRows()
	if len(rows) == 0 {
		b.WriteString(mutedStyle.Render("  対象 workflow がありません"))
		b.WriteString("\n")
	} else {
		optInOffset := settingsOptInOffset()
		for i, row := range rows {
			checkbox := "[ ]"
			if m.Config.Workflows[row.id].DistillLearningsAfter {
				checkbox = "[✓]"
			}
			label := checkbox + " " + row.baseLabel
			b.WriteString(renderChoice(m.SettingsCursor == optInOffset+i, label, "("+row.id+")"))
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(mutedStyle.Render("↑/↓ 選択 · space toggle · Enter 保存 · esc 戻る · q 終了"))
	b.WriteString("\n")
	b.WriteString(mutedStyle.Render("OpenTelemetry と agent-learnings 学習対象の変更は次回起動時から反映されます"))
	return b.String()
}

func (m Model) renderCodexModelSettings() string {
	var b strings.Builder
	b.WriteString(sectionStyle.Render("Settings"))
	b.WriteString("\n")
	b.WriteString(metaStyle.Render("Codex model"))
	b.WriteString("\n")
	for i, option := range codexModelOptions() {
		current := ""
		if option.id == m.Config.Codex.ModelPreset {
			current = "現在"
		}
		b.WriteString(renderChoice(m.CodexModelPos == i, option.label, current))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(mutedStyle.Render("↑/↓ 選択 · Enter 保存 · esc 戻る · q 終了"))
	return b.String()
}

func (m Model) renderCursorSDKModelSettings() string {
	var b strings.Builder
	b.WriteString(sectionStyle.Render("Settings"))
	b.WriteString("\n")
	b.WriteString(metaStyle.Render("Cursor SDK model"))
	b.WriteString("\n")
	for i, option := range cursorSDKModelOptions() {
		current := ""
		if option.id == m.Config.CursorSDK.ModelPreset {
			current = "現在"
		}
		b.WriteString(renderChoice(m.CursorSDKModelPos == i, option.label, current))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(mutedStyle.Render("↑/↓ 選択 · Enter 保存 · esc 戻る · q 終了"))
	return b.String()
}

func (m Model) renderWorkflow() string {
	var b strings.Builder
	width := m.contentWidth()
	// "Running workflow: ..." セクション行と "ステップ N/M: ..." は renderHeader 側に
	// 移動済み (画面固定のため)。ここから下は body (overflow 時に上から clip される領域)。
	if m.CurrentTool != "" {
		b.WriteString(withWidth(metaStyle, width).Render("tool: " + m.CurrentTool))
		b.WriteString("\n")
	}
	if m.ReasoningText != "" {
		b.WriteString(withWidth(metaStyle, width).Render("thinking: " + m.ReasoningText))
		b.WriteString("\n")
	}
	if m.StreamText != "" {
		b.WriteString(withWidth(itemStyle, width).Render(m.StreamText))
		b.WriteString("\n")
	} else if m.CurrentTool == "" && m.ReasoningText == "" {
		b.WriteString(withWidth(mutedStyle, width).Render("考え中..."))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(withWidth(mutedStyle, width).Render("ctrl+c 中断"))
	return b.String()
}

func (m Model) renderPrompt() string {
	var b strings.Builder
	width := m.contentWidth()
	b.WriteString(sectionStyle.Render("Workflow input"))
	b.WriteString("\n")
	if m.Prompt != "" {
		b.WriteString(withWidth(itemStyle, width).Render(m.Prompt))
		b.WriteString("\n")
	}
	for i, item := range m.PromptItems {
		b.WriteString(renderChoiceWithWidth(i == m.promptCursor, item.Label, m.promptSelectionSuffix(item.ID), width))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	help := "↑/↓ 選択 · Enter 決定 · esc キャンセル · q 終了"
	if m.promptOrdered {
		help = "↑/↓ 移動 · space 選択/解除 · Enter 決定 · esc キャンセル · q 終了"
	}
	b.WriteString(withWidth(mutedStyle, width).Render(help))
	return b.String()
}

func (m Model) promptSelectionSuffix(id string) string {
	if !m.promptOrdered {
		return ""
	}
	for i, selectedID := range m.promptSelectedIDs {
		if selectedID == id {
			return fmt.Sprintf("選択 #%d", i+1)
		}
	}
	return ""
}

func (m Model) renderDone() string {
	title := sectionStyle.Render("Result")
	style := resultStyle
	if width := m.contentWidth(); width > resultStyle.GetHorizontalBorderSize() {
		// Lipgloss width includes padding but borders are applied outside it.
		style = style.Width(width - resultStyle.GetHorizontalBorderSize())
	}
	footerStyle := mutedStyle
	if width := m.contentWidth(); width > 0 {
		footerStyle = footerStyle.Width(width)
	}
	parts := []string{title}
	if len(m.Results) == 0 {
		parts = append(parts, style.Render("結果はありません"))
	} else {
		// SequentialWithLearningsTail 経由で base / tail それぞれの結果が届いたとき、
		// それぞれ独立したボックスとして縦に並べる (tail opt-in でもとの workflow の
		// 結果が agent-learnings の結果に上書きされて消えないようにする)。
		for _, entry := range m.Results {
			block := strings.TrimSpace(entry.Body)
			if entry.Label != "" {
				// ボックス内 1 行目に既存の child Label を出すことで、どの child の
				// 結果かを識別可能にする (label の追加装飾は最小着地のため未付与)。
				block = entry.Label + "\n\n" + block
			}
			parts = append(parts, style.Render(block))
		}
	}
	if m.WorkflowDuration > 0 {
		parts = append(parts, footerStyle.Render("total "+formatTotal(m.WorkflowDuration)))
	}
	parts = append(parts, footerStyle.Render("Enter メニューに戻る · q 終了"))
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// renderFooter は画面下部に固定するフッターを描画する。
// PhaseWorkflow のみ生存表示 (スピナー / 直近イベント経過 / 累計時間) を出し、それ以外は空文字。
func (m Model) renderFooter() string {
	if m.Phase != PhaseWorkflow {
		return ""
	}
	width := m.contentWidth()
	frame := spinnerFrames[m.SpinnerFrame%len(spinnerFrames)]

	// 直近イベントからの経過時間で色とラベルを切り替える。
	silence := time.Duration(0)
	statusLabel := "active"
	style := heartbeatActiveStyle
	if !m.LastEventAt.IsZero() {
		silence = time.Since(m.LastEventAt)
		switch {
		case silence < heartbeatActiveThreshold:
			statusLabel = "active"
			style = heartbeatActiveStyle
		case silence < heartbeatStaleThreshold:
			statusLabel = fmt.Sprintf("idle %s", formatSilence(silence))
			style = heartbeatIdleStyle
		default:
			statusLabel = fmt.Sprintf("no event for %s", formatSilence(silence))
			style = heartbeatStaleStyle
		}
	}

	parts := []string{frame, statusLabel}
	if silence < heartbeatActiveThreshold && !m.LastEventAt.IsZero() {
		parts = append(parts, fmt.Sprintf("last event %s ago", formatSilence(silence)))
	}
	if !m.WorkflowStartedAt.IsZero() {
		parts = append(parts, "total "+formatTotal(time.Since(m.WorkflowStartedAt)))
	}
	line := style.Render(strings.Join(parts, " · "))
	return withWidth(mutedStyle, width).Render(line)
}

func formatSilence(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

func formatTotal(d time.Duration) string {
	total := int(d.Seconds())
	if total < 0 {
		total = 0
	}
	return fmt.Sprintf("%02d:%02d", total/60, total%60)
}

// elapsedSinceStart はワークフロー開始からの経過時間を返す。
// 開始時刻が未設定 (テスト経由で直接 PhaseDone に入った場合など) は 0 を返す。
func (m Model) elapsedSinceStart() time.Duration {
	if m.WorkflowStartedAt.IsZero() {
		return 0
	}
	return time.Since(m.WorkflowStartedAt)
}

func (m Model) contentWidth() int {
	width := m.Width - appStyle.GetHorizontalFrameSize()
	if width <= 0 {
		return 0
	}
	return width
}

func withWidth(style lipgloss.Style, width int) lipgloss.Style {
	if width <= 0 {
		return style
	}
	return style.Width(width)
}

func renderChoice(selected bool, label string, suffix string) string {
	return renderChoiceWithWidth(selected, label, suffix, 0)
}

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

// handleKey は Phase 依存のキー入力ディスパッチ (Task 8.1, Task 8.2)。
//
// キー写像 (Req 1.1〜1.8 / 2.3〜2.7 / 9.3 / 9.4 / 10.1 / 10.3):
//   - ctrl+c: 全 Phase で tea.Quit (Req 1.8)
//   - q     : PhaseMenu → tea.Quit (Req 1.5) / PhaseAkashic → PhaseMenu に戻し
//     tea.Quit を発行しない (Req 1.4) / それ以外は既存挙動 (tea.Quit)
//   - esc   : PhaseAkashic ではサブビューに関わらず PhaseMenu に戻す (Req 2.7)、
//     PhasePrompt のみ既存の prompt キャンセル経路、それ以外は既存挙動
//   - s / S : PhaseMenu のみ PhaseProviderSettings に遷移 (Req 1.6 / 1.7)。
//     PhaseAkashic では消費しない (Phase 不変、Req 1.7)
//   - a / A : PhaseMenu のみ PhaseAkashic に遷移し akashic.NewState を初期化、
//     初回 akashic.ListRunsCmd を Cmd として返す (Reader=nil なら Cmd=nil、
//     Req 1.1 / 1.2 / 11.1 / 11.2)
//   - その他: PhaseAkashic では akashic.State.HandleKey に委譲する
//     (Task 8.2: tab / enter / backspace でサブビュー間遷移、Detail 内 Focus 循環など)。
//     それ以外の Phase では既存挙動 (up/down/j/k → m.move、enter → m.handleEnter) を維持
//
// 既存挙動の非破壊性は model_test.go の既存テスト群 (TestMenuSelectsWorkflow /
// TestMenuShortcutOpensProviderSettings ほか) を変更なしで通すことで担保する
// (Req 9.3 / 9.4 の機械的検査)。
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// モードレベルキー (Phase に依存しない / Phase 越え退出): 早期 return で
	// akashic.State への委譲経路に流さない (q / esc は PhaseAkashic でも
	// モード退出として Model 側で消費する不変条件、Req 1.4 / 2.7)。
	switch msg.String() {
	case "ctrl+c":
		// memory-distiller-trigger-policy task 4.1 (Req 3.1 / 3.2):
		// ctrl+c は user 起源の cancel signal なので、foreground の agent_call が
		// register 済みであればその call スコープ atomic に "user" を Store する。
		// `Load() == nil` (call register 前 / call return 直後) や Pointer 自体が
		// nil (旧 Options 経路 / テスト) のときは no-op で既存挙動を保つ (Req 6)。
		// cancel の wire emission は launcher の `<-ctx.Done()` 経路が担うため、
		// UI 層は origin の意味付けだけを行う (関心の分離)。
		if m.activeCancelOrigin != nil {
			if ref := m.activeCancelOrigin.Load(); ref != nil {
				ref.Store(bridge.CancelOriginUser)
			}
		}
		// Req 1.8: ctrl+c は全 Phase で常に tea.Quit。
		return m, tea.Quit
	case "q":
		if m.Phase == PhaseAkashic {
			// Req 1.4: PhaseAkashic 中の q はモード退出として再解釈し、
			// アプリ自体を終了させない。tea.Quit は発行しない。
			m.Phase = PhaseMenu
			return m, nil
		}
		// Req 1.5: PhaseMenu / その他既存 Phase では従来通り tea.Quit。
		return m, tea.Quit
	case "esc":
		if m.Phase == PhasePrompt && (m.promptReply != nil || m.orderedPromptReply != nil) {
			m.cancelPrompt()
			return m, nil
		}
		if m.Phase == PhaseCodexModelSettings || m.Phase == PhaseCursorSDKModelSettings {
			m.Phase = PhaseProviderSettings
			return m, nil
		}
		if m.Phase != PhaseMenu {
			// Req 2.7: PhaseAkashic を含む全非 PhaseMenu Phase で esc は PhaseMenu に戻す。
			m.Phase = PhaseMenu
		}
		return m, nil
	case "s", "S":
		if m.Phase == PhaseMenu {
			m.Phase = PhaseProviderSettings
			return m, nil
		}
		// Req 1.7: PhaseAkashic / その他 Phase では s を消費しない (Phase 不変)。
		return m, nil
	case " ", "space":
		if m.Phase == PhasePrompt && m.promptOrdered {
			m.togglePromptSelection()
			return m, nil
		}
		// agent-learnings opt-in 行の toggle (PhaseProviderSettings 限定)。
		// provider/level 行 (= cursor が前段) や、その他 Phase では space は no-op。
		// PhasePrompt では ordered prompt のときだけ選択/解除に使う。
		if m.Phase != PhaseProviderSettings {
			return m, nil
		}
		offset := settingsOptInOffset()
		idx := m.SettingsCursor - offset
		rows := m.optInRows()
		if idx < 0 || idx >= len(rows) {
			return m, nil
		}
		id := rows[idx].id
		if m.Config.Workflows == nil {
			m.Config.Workflows = make(map[string]config.WorkflowConfig)
		}
		wf := m.Config.Workflows[id]
		wf.DistillLearningsAfter = !wf.DistillLearningsAfter
		m.Config.Workflows[id] = wf
		if m.SaveConfig != nil {
			_ = m.SaveConfig(m.Config)
		}
		return m, nil
	case "a", "A":
		if m.Phase == PhaseMenu {
			// Req 1.1 / 1.2: PhaseMenu からのみ PhaseAkashic に遷移。
			// uiakashic.NewState は Reader != nil のとき初回 ListRunsCmd を返す。
			// Reader=nil のとき Cmd=nil で graceful (Req 11.1 / 11.2)。
			//
			// ForwardEvent: TUI シェルが消費する workflowEventMsg 経由で
			// hand-off 中の WorkflowEvent を Update に届けるためのコールバック。
			// Ask: workflow.WorkflowAsk 実装は TUI シェルが promptMsg 経路で
			// 提供する (akashic サブパッケージは TUI 内部型を直接知らない)。
			events := m.events
			var cmd tea.Cmd
			m.Akashic, cmd = uiakashic.NewState(uiakashic.Deps{
				Reader:         m.Reader,
				ArtifactReader: m.ArtifactReader,
				Workflows:      m.Workflows,
				CWD:            m.CWD,
				ForwardEvent: func(ev workflow.WorkflowEvent) {
					events <- workflowEventMsg{event: ev}
				},
				Ask: workflowAsk{events: events},
			})
			m.Phase = PhaseAkashic
			return m, cmd
		}
		// Req 1.2: それ以外の Phase では a を消費しない。
		return m, nil
	}

	// Task 8.2: PhaseAkashic ではモードレベルキー以外を akashic.State.HandleKey に
	// 委譲する。サブビュー間遷移 (List ↔ Summary tab、List → Detail enter、
	// Detail → List backspace) と Detail 内 Focus 循環 / カーソル / ページ送り /
	// hand-off (i) など、Akashic ローカルなキー写像はすべて akashic.State 側に集約する。
	if m.Phase == PhaseAkashic {
		var cmd tea.Cmd
		m.Akashic, cmd = m.Akashic.HandleKey(msg)
		return m, cmd
	}

	// PhaseAkashic 以外の既存挙動を維持 (Req 9.3 / 9.4 機械的検査)。
	switch msg.String() {
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "enter":
		return m.handleEnter()
	}
	return m, nil
}

func (m *Model) move(delta int) {
	switch m.Phase {
	case PhaseMenu:
		max := len(m.menuItems())
		if max == 0 {
			return
		}
		m.Cursor = clamp(m.Cursor+delta, 0, max-1)
	case PhaseProviderSettings:
		max := settingsOptInOffset() + len(m.optInRows())
		m.SettingsCursor = clamp(m.SettingsCursor+delta, 0, max-1)
	case PhaseCodexModelSettings:
		m.CodexModelPos = clamp(m.CodexModelPos+delta, 0, len(codexModelOptions())-1)
	case PhaseCursorSDKModelSettings:
		m.CursorSDKModelPos = clamp(m.CursorSDKModelPos+delta, 0, len(cursorSDKModelOptions())-1)
	case PhasePrompt:
		m.promptCursor = clamp(m.promptCursor+delta, 0, len(m.PromptItems)-1)
	}
}

func (m Model) handleEnter() (tea.Model, tea.Cmd) {
	switch m.Phase {
	case PhaseMenu:
		items := m.menuItems()
		if len(items) == 0 {
			return m, nil
		}
		item := items[m.Cursor]
		m.Phase = PhaseWorkflow
		m.StreamText = ""
		m.ReasoningText = ""
		m.CurrentTool = ""
		m.Results = nil
		m.CurrentWorkflowLabel = item.workflow.Label
		now := time.Now()
		m.WorkflowStartedAt = now
		m.LastEventAt = now
		return m, tea.Batch(startWorkflow(m.events, item.workflow, m.CWD), waitForEvent(m.events))
	case PhaseProviderSettings:
		providerCount := len(providerOptions())
		levelCount := len(akashicLevelOptions())
		otelCount := len(otelOptions())
		switch {
		case m.SettingsCursor < providerCount:
			m.ProviderPos = m.SettingsCursor
			selectedProvider := providerOptions()[m.ProviderPos].id
			if selectedProvider == provider.ProviderCodex {
				m.CodexModelPos = codexModelIndex(m.Config.Codex.ModelPreset)
				m.Phase = PhaseCodexModelSettings
				return m, nil
			}
			if selectedProvider == provider.ProviderCursorSDK {
				m.CursorSDKModelPos = cursorSDKModelIndex(m.Config.CursorSDK.ModelPreset)
				m.Phase = PhaseCursorSDKModelSettings
				return m, nil
			}
			m.Config.AgentProvider = selectedProvider
		case m.SettingsCursor < providerCount+levelCount:
			m.LevelPos = m.SettingsCursor - providerCount
			m.Config.AkashicLevel = akashicLevelOptions()[m.LevelPos].id
		case m.SettingsCursor < providerCount+levelCount+otelCount:
			otelIdx := m.SettingsCursor - providerCount - levelCount
			m.Config.OtelEnabled = otelOptions()[otelIdx].enabled
		default:
			// agent-learnings opt-in 行で Enter は no-op (toggle は space 経路に分離)。
			// Phase / Cursor を変えず、SaveConfig も呼ばない (provider/level 行と挙動を差別化)。
			return m, nil
		}
		if m.SaveConfig != nil {
			_ = m.SaveConfig(m.Config)
		}
		m.Phase = PhaseMenu
	case PhaseCodexModelSettings:
		m.Config.AgentProvider = provider.ProviderCodex
		m.Config.Codex.ModelPreset = codexModelOptions()[m.CodexModelPos].id
		if m.SaveConfig != nil {
			_ = m.SaveConfig(m.Config)
		}
		m.Phase = PhaseMenu
	case PhaseCursorSDKModelSettings:
		m.Config.AgentProvider = provider.ProviderCursorSDK
		m.Config.CursorSDK.ModelPreset = cursorSDKModelOptions()[m.CursorSDKModelPos].id
		if m.SaveConfig != nil {
			_ = m.SaveConfig(m.Config)
		}
		m.Phase = PhaseMenu
	case PhasePrompt:
		if m.promptOrdered {
			if m.orderedPromptReply != nil {
				ids := append([]string(nil), m.promptSelectedIDs...)
				m.orderedPromptReply <- orderedPromptReply{ids: ids, ok: true}
			}
			m.finishPrompt()
			return m, nil
		}
		if m.promptReply != nil && len(m.PromptItems) > 0 {
			m.promptReply <- promptReply{id: m.PromptItems[m.promptCursor].ID, ok: true}
		}
		m.finishPrompt()
	case PhaseDone:
		m.Phase = PhaseMenu
	}
	return m, nil
}

func (m *Model) togglePromptSelection() {
	if len(m.PromptItems) == 0 || m.promptCursor < 0 || m.promptCursor >= len(m.PromptItems) {
		return
	}
	id := m.PromptItems[m.promptCursor].ID
	for i, selectedID := range m.promptSelectedIDs {
		if selectedID == id {
			m.promptSelectedIDs = append(m.promptSelectedIDs[:i], m.promptSelectedIDs[i+1:]...)
			return
		}
	}
	m.promptSelectedIDs = append(m.promptSelectedIDs, id)
}

func (m *Model) cancelPrompt() {
	if m.promptOrdered {
		if m.orderedPromptReply != nil {
			m.orderedPromptReply <- orderedPromptReply{ok: false}
		}
	} else if m.promptReply != nil {
		m.promptReply <- promptReply{ok: false}
	}
	m.finishPrompt()
}

func (m *Model) finishPrompt() {
	m.Phase = PhaseWorkflow
	m.promptReply = nil
	m.orderedPromptReply = nil
	m.promptOrdered = false
	m.promptSelectedIDs = nil
}

func (m *Model) applyWorkflowEvent(event workflow.WorkflowEvent) {
	switch event.Type {
	case workflow.WorkflowStep:
		m.StepText = fmt.Sprintf("ステップ %d/%d: %s", event.Step, event.TotalSteps, event.Label)
		m.StreamText = ""
		m.ReasoningText = ""
		m.CurrentTool = ""
	case workflow.WorkflowActive:
		m.CurrentWorkflowLabel = event.Label
		m.StepText = ""
		m.StreamText = ""
		m.ReasoningText = ""
		m.CurrentTool = ""
	case workflow.WorkflowReasoning:
		// Codex bridge の reasoning text は delta ではなく最新スナップショット。
		// lifecycle 更新を連結せず置換し、同じ内容の重複表示を避ける。
		m.ReasoningText = event.Text
		m.LastEventAt = time.Now()
	case workflow.WorkflowText:
		m.StreamText = appendStream(m.StreamText, event.Text)
		m.LastEventAt = time.Now()
	case workflow.WorkflowToolUse:
		m.CurrentTool = event.Tool
		m.LastEventAt = time.Now()
	case workflow.WorkflowDone:
		// Sequential などで子の完了時にも届くため、Phase 遷移と afplay は
		// ここでは行わず Results への追加のみに留める (workflowDoneMsg 側に集約)。
		// SequentialWithLearningsTail 経由で base / tail それぞれの結果が
		// 順に届くケースでは、両方を縦スタック表示するために append する。
		m.Results = appendResultEntry(m.Results, resultEntry{
			Label: m.CurrentWorkflowLabel,
			Body:  event.Result,
		})
	case workflow.WorkflowError:
		// 子の WorkflowError でも届く可能性があるため、Phase 遷移と afplay は
		// workflowDoneMsg 側に集約する。
		m.Results = appendResultEntry(m.Results, resultEntry{
			Label: m.CurrentWorkflowLabel,
			Body:  event.Message,
		})
	}
}

// appendStream は新しい text を current に追記しつつ、長期実行で青天井に伸びないよう
// maxStreamLines を超えた場合は先頭から切り詰める (表示は別途 tail-only で絞る)。
func appendStream(current, addition string) string {
	combined := current + addition
	lines := strings.Split(combined, "\n")
	if len(lines) <= maxStreamLines {
		return combined
	}
	return strings.Join(lines[len(lines)-maxStreamLines:], "\n")
}

type menuItem struct {
	label    string
	workflow workflow.Definition
}

func (m Model) menuItems() []menuItem {
	items := make([]menuItem, 0, len(m.Workflows))
	for _, wf := range m.Workflows {
		// Hidden=true の Definition は hand-off 専用 (例: improve-workflow)。
		// トップメニューからの引数なし起動は precondition_failed に落ちるだけなので、
		// メニュー項目としては露出させない。akashic Run 詳細の `i` キーなど
		// 既知の起動経路は m.Workflows から直接見つけるためここでは除外しても影響しない。
		if wf.Hidden {
			continue
		}
		items = append(items, menuItem{label: wf.Label, workflow: wf})
	}
	return items
}

type providerOption struct {
	id    provider.ProviderID
	label string
}

func providerOptions() []providerOption {
	return []providerOption{
		{id: provider.ProviderClaude, label: "Claude Agent SDK"},
		{id: provider.ProviderCursor, label: "Cursor Agent CLI"},
		{id: provider.ProviderCodex, label: "Codex SDK"},
		{id: provider.ProviderCursorSDK, label: "Cursor SDK"},
	}
}

func providerIndex(id provider.ProviderID) int {
	for i, option := range providerOptions() {
		if option.id == id {
			return i
		}
	}
	return 0
}

type akashicLevelOption struct {
	id          config.AkashicLevel
	label       string
	recommended bool
}

func akashicLevelOptions() []akashicLevelOption {
	return []akashicLevelOption{
		{id: config.AkashicSummary, label: "summary", recommended: true},
		{id: config.AkashicFull, label: "full"},
		{id: config.AkashicDebug, label: "debug"},
	}
}

func akashicLevelIndex(id config.AkashicLevel) int {
	for i, option := range akashicLevelOptions() {
		if option.id == id {
			return i
		}
	}
	return 0
}

type otelOption struct {
	enabled bool
	label   string
}

func otelOptions() []otelOption {
	return []otelOption{
		{enabled: false, label: "OFF"},
		{enabled: true, label: "ON"},
	}
}

func settingsOtelOffset() int {
	return len(providerOptions()) + len(akashicLevelOptions())
}

func settingsOptInOffset() int {
	return settingsOtelOffset() + len(otelOptions())
}

type codexModelOption struct {
	id    config.CodexModelPreset
	label string
}

func codexModelOptions() []codexModelOption {
	return []codexModelOption{
		{id: config.CodexModelPresetGPT55XHigh, label: "gpt-5.5 / xhigh"},
		{id: config.CodexModelPresetGPT6LunaMax, label: "gpt-6-luna / max"},
		{id: config.CodexModelPresetGPT56SolHigh, label: "gpt-5.6-sol / high"},
		{id: config.CodexModelPresetGPT56SolXHigh, label: "gpt-5.6-sol / xhigh"},
	}
}

func codexModelIndex(id config.CodexModelPreset) int {
	for i, option := range codexModelOptions() {
		if option.id == id {
			return i
		}
	}
	return 0
}

type cursorSDKModelOption struct {
	id    config.CursorSDKModelPreset
	label string
}

func cursorSDKModelOptions() []cursorSDKModelOption {
	return []cursorSDKModelOption{
		{id: config.CursorSDKModelPresetComposer25, label: "composer-2.5"},
		{id: config.CursorSDKModelPresetComposer25Fast, label: "composer-2.5 / fast"},
		{id: config.CursorSDKModelPresetGPT55XHigh, label: "gpt-5.5 / xhigh"},
		{id: config.CursorSDKModelPresetGrok45Fast, label: "grok-4.5 / fast / high"},
		{id: config.CursorSDKModelPresetGPT56LunaMax, label: "gpt-5.6-luna-max"},
		{id: config.CursorSDKModelPresetGPT56SolHigh, label: "gpt-5.6-sol / high"},
	}
}

func cursorSDKModelIndex(id config.CursorSDKModelPreset) int {
	for i, option := range cursorSDKModelOptions() {
		if option.id == id {
			return i
		}
	}
	return 0
}

// optInRow は agent-learnings opt-in 行の表示データ。
// id は workflow.Definition.ID (= config.Workflows のキー) と一致する。
type optInRow struct {
	id        string
	baseLabel string
}

// optInRows は m.Workflows から opt-in toggle 対象の行を抽出する。
//   - Hidden=true の Definition は内部経路用 (例: hand-off 専用) なので除外する
//   - Label が既に learnings tail wrap の marker (` 📚`) を suffix に持つ場合
//     (= 起動時に opt-in が有効で wrap 済み)、表示用には marker を strip した base Label を返す
//
// 並び順は m.Workflows の登録順をそのまま尊重する (Registry slice 順 = メニュー表示順と一致)。
func (m Model) optInRows() []optInRow {
	const learningsTailMarker = " 📚"
	rows := make([]optInRow, 0, len(m.Workflows))
	for _, wf := range m.Workflows {
		if wf.Hidden {
			continue
		}
		rows = append(rows, optInRow{
			id:        wf.ID,
			baseLabel: strings.TrimSuffix(wf.Label, learningsTailMarker),
		})
	}
	return rows
}

func waitForEvent(events <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		return <-events
	}
}

func startWorkflow(events chan<- tea.Msg, definition workflow.Definition, cwd string) tea.Cmd {
	return func() tea.Msg {
		go func() {
			ask := workflowAsk{events: events}
			err := definition.Run(context.Background(), cwd, func(event workflow.WorkflowEvent) {
				events <- workflowEventMsg{event: event}
			}, ask)
			// CLI 経路の `defer notify.PlayCompletionSound()` と対称な構造。
			// 成功時 (err==nil) も含め、トップレベル Run の完了を Bubble Tea へ
			// 通知する唯一のシグナルとして必ず workflowDoneMsg を送る。
			events <- workflowDoneMsg{err: err}
		}()
		return nil
	}
}

type workflowAsk struct {
	events chan<- tea.Msg
}

func (a workflowAsk) Select(_ context.Context, params workflow.SelectParams) (string, bool, error) {
	reply := make(chan promptReply, 1)
	a.events <- promptMsg{params: params, reply: reply}
	result := <-reply
	return result.id, result.ok, nil
}

func (a workflowAsk) SelectOrdered(_ context.Context, params workflow.OrderedSelectParams) ([]string, bool, error) {
	reply := make(chan orderedPromptReply, 1)
	a.events <- orderedPromptMsg{params: params, reply: reply}
	result := <-reply
	return result.ids, result.ok, nil
}

func clamp(value int, min int, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}
