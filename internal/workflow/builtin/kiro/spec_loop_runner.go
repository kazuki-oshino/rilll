// Package kiro の本ファイルは kiro-spec-loop ワークフローの orchestration 本体を
// フェーズ別ハンドラに分割した specLoopRunner 構造体を提供する。
//
// 役割分担:
//   - specLoopRunner: 1 走行分のローカル状態（cwd / runDir / state / persona / feature 等）と
//     依存（agent / ask / onEvent）を保持し、フェーズ別ハンドラを公開する
//   - setup(): feature 選択 → persona 解決 → 00-context.json 永続化までの前段
//   - runStep(): 1 段の inquiryloop 駆動 → fix phase → /git-commit までを所有
//   - runInquiryLoop(): inquiryloop.Engine.RunPrompt の駆動のみ
//   - runStepFixPhase(): fix phase 実行（rounds > 0 のときのみ runFixPhase に委譲）
//   - runCommit(): /git-commit 呼び出し
//   - abort() / complete(): 終端時の result.json 書き出し + workflow event 発火
//   - Run(): 上記ハンドラをフェーズ遷移 orchestrator として束ねる
//
// 既存の runKiroSpecLoop はこの specLoopRunner を組み立てて Run() を呼ぶ薄いアダプタになる
// （後方互換のため公開シグネチャは維持）。
package kiro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kazuki-oshino/rilll/internal/facet"
	"github.com/kazuki-oshino/rilll/internal/inquiryloop"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	gitworkflow "github.com/kazuki-oshino/rilll/internal/workflow/builtin/git"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// specLoopRunner は 1 走行分の kiro-spec-loop 実行状態をまとめ、
// フェーズ別ハンドラ（setup / runStep / runInquiryLoop / runStepFixPhase / runCommit /
// abort / complete）を公開する。
//
// 並行実行は想定しない（runKiroSpecLoop ごとに 1 インスタンス）。
type specLoopRunner struct {
	// 依存（コンストラクタで注入）
	ctx     context.Context
	cwd     string
	onEvent func(workflow.WorkflowEvent)
	ask     workflow.WorkflowAsk
	agent   workflowctx.AgentCaller

	// setup 内で確定する派生状態
	state        *runState
	runDir       string
	persona      inquiryloop.AnswerPersona
	briefAbsPath string
	specJSONPath string
	startOrder   int
}

// newSpecLoopRunner は specLoopRunner を構築する。
// 状態は空（feature 未確定）の runState で初期化し、setup() 完了後に各派生状態が埋まる。
func newSpecLoopRunner(
	ctx context.Context,
	cwd string,
	onEvent func(workflow.WorkflowEvent),
	ask workflow.WorkflowAsk,
	agent workflowctx.AgentCaller,
) *specLoopRunner {
	state := &runState{
		RunID:     workflow.RunIDFromContext(ctx),
		StartedAt: time.Now().UTC(),
	}
	return &specLoopRunner{
		ctx:     ctx,
		cwd:     cwd,
		onEvent: onEvent,
		ask:     ask,
		agent:   agent,
		state:   state,
		runDir:  filepath.Join(cwd, ".rilll", "kiro-spec-loop", state.RunID),
	}
}

// stepOutcome は runStep の終端状態を表す。
//
// 3 状態:
//   - Aborted=false, RetryFromOrder=0: 完走、次段へ進める
//   - Aborted=true: abort 確定、Run はそのまま nil を返してよい
//   - RetryFromOrder>0: validate-design の retry 起動。Run は当該 order の step に巻き戻して
//     再実行する。Aborted=true と同時には立たない (排他)。
type stepOutcome struct {
	Aborted        bool
	RetryFromOrder int
}

// Run は kiro-spec-loop の phase 遷移 orchestrator。
//
// フロー:
//  1. setup(): feature 選択 → persona 解決 → 00-context.json 書き出し
//  2. 各 step を runStep() で順に処理（abort 発生時は abort 経由で result.json + Error event 確定）
//  3. validate-design の retry トリガー時は indicated order に巻き戻して再実行
//  4. 6 段完走 → complete() で result.json + Done event を発火
//
// 戻り値:
//   - 想定済み abort（missing-* / cancelled-by-user / engine 由来 / commit-failure / validation-no-go 上限）→ nil
//   - I/O 致命的エラー（result.json 書き出し失敗 等）→ error を propagate
func (r *specLoopRunner) Run() error {
	done, err := r.setup()
	if err != nil || done {
		return err
	}
	// インデックス駆動ループ: retry 時に index を巻き戻して同じ step / 後続を再実行する。
	i := 0
	for i < len(steps) && steps[i].Order < r.startOrder {
		i++
	}
	for i < len(steps) {
		outcome, err := r.runStep(steps[i])
		if err != nil {
			return err
		}
		if outcome.Aborted {
			return nil
		}
		if outcome.RetryFromOrder > 0 {
			jump := stepIndexForOrder(outcome.RetryFromOrder)
			if jump < 0 {
				// 配線エラー (steps テーブルに存在しない order を指定された) は致命扱い。
				return fmt.Errorf("specLoopRunner.Run: retryFromOrder=%d not found in steps", outcome.RetryFromOrder)
			}
			i = jump
			continue
		}
		i++
	}
	return r.complete()
}

// stepIndexForOrder は steps テーブルから指定 order の index を返す純関数。
// 見つからなければ -1。
func stepIndexForOrder(order int) int {
	for idx, s := range steps {
		if s.Order == order {
			return idx
		}
	}
	return -1
}

// setup は eligible feature 列挙 → 選択 → persona 解決 → 00-context.json 書き出しまでを担う。
//
// 戻り値:
//   - done=true: 想定済み abort（missing-feature / cancelled-by-user / missing-persona）が確定し、
//     result.json と Error event の発火まで完了している。Run はそのまま nil を返してよい。
//   - done=false: 後続 step ループに進める準備（specJSONPath / startOrder 等）が整った状態。
//   - err: I/O 致命的エラー（ask の自身のエラー / persistContext の書き込み失敗 等）
func (r *specLoopRunner) setup() (bool, error) {
	// (1) eligible feature の列挙
	eligible, err := listEligibleFeatures(r.cwd)
	if err != nil {
		return false, fmt.Errorf("specLoopRunner.setup: listEligibleFeatures: %w", err)
	}
	if len(eligible) == 0 {
		msg := "対象 feature が見つかりません: .kiro/specs/<feature>/brief.md を持ち、spec.json.phase が tasks-generated 未満の feature を準備してください（例: /kiro-discovery を先に実行する、または手動で brief.md を配置する）"
		return true, r.abort(RunReasonMissingFeature, nil, msg)
	}
	if r.ask == nil {
		return true, r.abort(RunReasonMissingFeature, nil, "対象 feature を選択する手段が利用できません（workflow.WorkflowAsk が nil）")
	}

	// (2) feature 選択ダイアログ
	items := make([]workflow.SelectItem, 0, len(eligible))
	for _, f := range eligible {
		items = append(items, workflow.SelectItem{ID: f, Label: f})
	}
	feature, ok, err := r.ask.Select(r.ctx, workflow.SelectParams{
		Key:    "feature",
		Prompt: "対象のfeatureを選択してください（ESCでキャンセル）:",
		Items:  items,
	})
	if err != nil {
		return false, fmt.Errorf("specLoopRunner.setup: ask.Select: %w", err)
	}
	if !ok {
		return true, r.abort(RunReasonCancelledByUser, nil, "feature 選択がキャンセルされました")
	}
	r.state.Feature = feature

	// (3) session B Persona 解決
	p, err := resolveSessionBPersona(r.cwd, "architecture-quality")
	if err != nil {
		if errors.Is(err, facet.ErrFacetNotFound) || errors.Is(err, errEmptySessionBPersona) {
			return true, r.abort(RunReasonMissingPersona, nil, formatSessionBPersonaError("architecture-quality", err))
		}
		return false, fmt.Errorf("specLoopRunner.setup: resolve session B persona: %w", err)
	}
	r.persona = p
	r.state.PersonaName = p.Name
	r.state.PersonaPath = p.SourcePath

	// (4) 00-context.json 書き出し
	briefAbsPath := filepath.Join(r.cwd, ".kiro", "specs", feature, "brief.md")
	if abs, absErr := filepath.Abs(briefAbsPath); absErr == nil {
		briefAbsPath = abs
	}
	r.briefAbsPath = briefAbsPath
	if err := persistContext(r.runDir, runContext{
		Feature:     feature,
		BriefPath:   briefAbsPath,
		PersonaName: p.Name,
		PersonaPath: p.SourcePath,
		StartedAt:   r.state.StartedAt,
	}); err != nil {
		return false, fmt.Errorf("specLoopRunner.setup: persistContext: %w", err)
	}

	// step ループ用の派生状態
	r.specJSONPath = filepath.Join(r.cwd, ".kiro", "specs", feature, "spec.json")
	r.startOrder = determineStartStep(r.specJSONPath)
	return false, nil
}

// runStep は 1 段の処理（inquiryloop 駆動 → fix phase → 承認フラグ → /git-commit）を所有する。
//
// 戻り値:
//   - outcome.Aborted=true: 想定済み abort が確定し、abort() で result.json + Error event の発火まで完了。
//     Run はそのまま nil を返してよい。
//   - outcome.RetryFromOrder>0: validate-design の retry トリガー。Run が当該 order に巻き戻す。
//   - outcome 全フィールド zero: 完走（commit 成功）し、次段に進める。
//   - err: I/O 致命的エラー（persistStepResult 失敗 等）
func (r *specLoopRunner) runStep(step promptStep) (stepOutcome, error) {
	promptLabel := fmt.Sprintf("%02d-%s", step.Order, step.Phase)

	if step.Phase == "validate-design" && step.RequiresVerdict {
		outcome, handled, err := r.handleValidateDesignPreflight(step, promptLabel)
		if err != nil || handled {
			return outcome, err
		}
	}

	if r.onEvent != nil {
		r.onEvent(workflow.WorkflowEvent{
			Type:       workflow.WorkflowStep,
			Step:       step.Order,
			TotalSteps: len(steps),
			Label:      promptLabel,
		})
	}

	// (a) inquiryloop 駆動
	result, err := r.runInquiryLoop(step, promptLabel)
	if err != nil {
		// kiro-spec-loop-facets task 5.2 / Design §Components runStep:
		// runInquiryLoop が Persona facet 解決失敗 (facet.ErrFacetNotFound) を
		// `%w` チェーンで wrap した error を返してきた場合、これは I/O 致命的エラーでは
		// なく「fixture 整備不足 / 配線漏れ」由来の構成エラーである。
		// RunReasonMissingPhasePersona に合流させ、既存 abort 経路と同形式の
		// terminationReason / WorkflowError event で `result.json` を確定する。
		//
		// その他 I/O error (権限不足等) は wrap せずそのまま `Run()` まで propagate し、
		// kiro-spec-loop の呼び出し側 (CLI / TUI) に致命エラーとして晒す。
		if errors.Is(err, facet.ErrFacetNotFound) {
			// WorkflowError message から runInquiryLoop wrapper の重複を避けるため、
			// 1 段 unwrap した underlying error 情報 (= 多くの場合 facet.ErrFacetNotFound 自身) を採用する。
			// unwrap 失敗時は元 err をそのまま露出させる defensive fallback で
			// 「error 内容が見えない」を回避する (Req 2.3 / 観察可能完了 (b))。
			inner := errors.Unwrap(err)
			if inner == nil {
				inner = err
			}
			msg := fmt.Sprintf("中断: %s で missing-phase-persona (%s: %v)", promptLabel, step.Persona, inner)
			return stepOutcome{Aborted: true}, r.abort(
				RunReasonMissingPhasePersona,
				&RunResultAbortStep{Order: step.Order, Phase: step.Phase},
				msg,
			)
		}
		return stepOutcome{}, err
	}

	// (b) abort パス: engine 4 値を workflow 8 値同名値に昇格
	if result.Status == inquiryloop.StatusAborted {
		return stepOutcome{Aborted: true}, r.handleStepAbort(step, promptLabel, result)
	}

	// (c) 完走パス: stepResult 組み立て → fix phase → 承認フラグ → /git-commit
	attempt := r.currentValidateDesignAttempt()
	sr := stepResult{
		Order:       step.Order,
		Phase:       step.Phase,
		Rounds:      result.Rounds,
		LastSession: result.LastSession,
		Summary:     result.Summary,
		Verdict:     result.Verdict,
		Attempt:     attempt,
	}
	if step.RequiresVerdict {
		outcome, err := r.handleValidationVerdict(step, promptLabel, sr, result)
		if err != nil {
			return outcome, err
		}
		if outcome.Aborted || outcome.RetryFromOrder > 0 {
			return outcome, nil
		}
	}
	if result.Rounds > 0 {
		fix := r.runStepFixPhase(step, result.LastSession, promptLabel)
		sr.Fix = &fix
	}
	r.applyApprovalForPhase(step.Phase)

	commitResp, commitErr := r.runCommit(step, promptLabel)
	if commitErr != nil || !commitResp.Success {
		sr.CommitOK = false
		switch {
		case commitErr != nil:
			sr.CommitOutput = commitErr.Error()
		default:
			sr.CommitOutput = commitResp.Content
		}
		r.state.StepResults = append(r.state.StepResults, sr)
		if perr := persistStepResult(r.runDir, sr); perr != nil {
			return stepOutcome{}, fmt.Errorf("specLoopRunner.runStep: persistStepResult commit abort: %w", perr)
		}
		msg := fmt.Sprintf("中断: %s 後の /git-commit が失敗しました", promptLabel)
		if commitErr != nil {
			msg = fmt.Sprintf("中断: %s 後の /git-commit が失敗しました（%v）", promptLabel, commitErr)
		}
		return stepOutcome{Aborted: true}, r.abort(RunReasonCommitFailure, &RunResultAbortStep{Order: step.Order, Phase: step.Phase}, msg)
	}
	sr.CommitOK = true
	sr.CommitOutput = commitResp.Content
	r.state.StepResults = append(r.state.StepResults, sr)
	if perr := persistStepResult(r.runDir, sr); perr != nil {
		return stepOutcome{}, fmt.Errorf("specLoopRunner.runStep: persistStepResult success: %w", perr)
	}
	return stepOutcome{}, nil
}

// runInquiryLoop は 1 段の inquiryloop.Engine.RunPrompt を駆動する。
//
// step.MaxRounds=0 のときは inquiryloop.Engine 既定（defaultMaxRounds=5）が適用される。
// validate-* 系は steps[] 定義側で 15 を指定する。
//
// session A prompt の組み立て (slash-first 順):
//
//   - **Persona facet 解決**: inquiry round 開始前に
//     `facet.ResolveFacet(r.cwd, facet.Persona, step.Persona)` を 1 回だけ呼ぶ。
//   - **session A prompt 構造**: `step.Command(...) + personaBlock(resolved) + previousResponseBlock(prev)`
//     を `inquiryloop.RunInput.PromptCommand` に渡す。slash command (`/kiro-spec-xxx <feature>`)
//     を必ず先頭に保つため、Persona / Previous Response ブロックは **後置**する
//     (Claude Code の skill 起動は先頭トークンが `/<skill>` でないと走らないため)。
//     foundation 側の `IncludePreviousResponse` 前置経路は spec-loop では使わない
//     (前置すると skill command が先頭でなくなる)。
//   - **session B (`inquiryloop.AnswerContext.PromptCommand`)** には persona block を
//     含まない素の `step.Command(...)` を渡し、session B 側 `buildAnswerPrompt` の
//     固定構造 (`# Persona (architecture-quality) → # Context → ...`) を破壊しない。
//
// Persona 解決失敗時:
//   - `step.Persona == ""` (defensive: 配線漏れ) → `facet.ErrFacetNotFound` を wrap した error を返す
//   - `facet.ErrFacetNotFound` → 同様に wrap して返し、`runStep` 側で
//     `errors.Is(err, facet.ErrFacetNotFound)` 経由で `RunReasonMissingPhasePersona` に
//     合流できる形 (`%w` チェーン) を保つ (task 5.2 で配線)
//   - その他 I/O error → `fmt.Errorf("specLoopRunner.runInquiryLoop(%s): persona resolve: %w", phase, err)`
//     で wrap して致命的 I/O として上位に伝播
//
// provider 名で分岐するロジックは持たない (Req 4.2)。
//
// 永続化（persistInquiry / persistAnswer）は onRound コールバック内で行う（best-effort）。
func (r *specLoopRunner) runInquiryLoop(step promptStep, promptLabel string) (inquiryloop.FinalResult, error) {
	stepDirAbs := stepDir(r.runDir, step.Order, step.Phase)

	var mds []string
	if step.Order == 1 {
		// Step 1 (spec-init) のみ brief.md 絶対パスを参照ブロックに乗せる。
		mds = []string{r.briefAbsPath}
	}
	promptCmd := step.Command(r.state.Feature, mds)

	// Persona facet 解決 (kiro-spec-loop-facets task 5.1 / Design §Components runInquiryLoop)。
	// session A 初回 skill 呼び出しの prompt 先頭に `# Persona\n\n<content>\n\n---\n\n` を
	// prepend するための content を取得する。
	if step.Persona == "" {
		// 配線漏れ防御: steps テーブルで Persona が空のままだと defensive に
		// ErrFacetNotFound 経路に合流させ、runStep 側の RunReasonMissingPhasePersona に流す。
		return inquiryloop.FinalResult{}, fmt.Errorf(
			"specLoopRunner.runInquiryLoop(%s): persona resolve: %w",
			step.Phase, facet.ErrFacetNotFound,
		)
	}
	resolved, ferr := facet.ResolveFacet(r.cwd, facet.Persona, step.Persona)
	if ferr != nil {
		return inquiryloop.FinalResult{}, fmt.Errorf(
			"specLoopRunner.runInquiryLoop(%s): persona resolve: %w",
			step.Phase, ferr,
		)
	}
	// session A 用 prompt (slash-first 順):
	//   `step.Command(...) + personaBlock(resolved) + previousResponseBlock(prev)`
	// slash command を必ず先頭に保つため、Persona / Previous Response ブロックは
	// skill コマンドの **後ろ**に連結する (Claude Code の skill 起動は先頭トークンが
	// `/<skill>` でないと走らないため)。
	//
	// previousResponseBlock の中で summary context 用の切り詰め policy を適用し、
	// 空文字 (1 段目 spec-init / on-disk fallback 失敗時) はブロックを省略する
	// (graceful degradation: 前段 summary 取得不能でも skill 呼び出しは継続)。
	prevSummary := previousSummaryForStep(r.state, step, r.runDir)
	// retry 経路の Previous Response は no_go 文 / advisory を欠落させないため
	// PromptContextDiagnostic (切り詰めなし) を選び、通常 forward は PromptContextSummary (2000 字) を使う。
	// PrevOverride[step.Order] が non-empty なら retry 注入とみなし lossless 経路を選ぶ。
	prevKind := facet.PromptContextSummary
	if r.state != nil && r.state.PrevOverride != nil {
		if v, ok := r.state.PrevOverride[step.Order]; ok && v != "" {
			prevKind = facet.PromptContextDiagnostic
		}
	}
	// validate-* 系で has_advisory / advisory schema を使う場合、SKILL 側に契約を伝える
	// workflow 層 prompt 注入。SKILL.md は無改変で、retry ループ駆動に必要な
	// verdict / has_advisory / advisory の使い分けを Output Contract block として伝える。
	// 通常 step では空文字を返すので prompt 構造に影響しない。
	skillPromptCmd := promptCmd + validationContractBlockForStep(step.RequiresVerdict, step.Phase) + personaBlock(resolved) + previousResponseBlock(prevSummary, prevKind)

	// 回答エージェント (session B) は毎ラウンド fresh session で呼ばれるため、
	// 元 PromptCommand と phase ごとに参照すべき spec ファイルパスを AnswerContext として
	// 明示的に渡す。kiro skill 側 (session A) は同 session を resume するため不要。
	//
	// session B には Persona / Previous Response ブロックを含まない素の promptCmd を渡す:
	//   (i)  session B 既存 `# Persona (architecture-quality)` 構造との二重 persona を回避
	//   (ii) `formatAnswerContext` が PromptCommand を `- 元コマンド:` bullet の単一行値として
	//        展開するため、改行混じり `\n\n---\n\n# Persona\n\n...` の埋め込みで
	//        markdown bullet が崩れるのを回避
	specDir := filepath.Join(r.cwd, ".kiro", "specs", r.state.Feature)
	answerSpecPaths := specPathsForPhase(specDir, r.briefAbsPath, step.Phase)

	engine := inquiryloop.Engine{
		Persona:     r.persona,
		MaxRounds:   step.MaxRounds,
		Schema:      inquiryloop.Schema(),
		SkillSchema: validationSchemaForStep(step),
		AnswerContext: inquiryloop.AnswerContext{
			PromptCommand: promptCmd,
			SpecPaths:     answerSpecPaths,
		},
		// resume 呼び出しの prompt に "Source: <path>" 行を載せるための seam。
		// persistAnswer が <runDir>/<NN>-<phase>/round-<N>-answer.md に full text を
		// 書き出している (= 監査トレース) ので、resume プロンプト本文では切り詰め + パス参照だけにし、
		// stream_idle_timeout の主因 (long structured 出力) を回避する。
		AnswerSourcePathFunc: func(round int) string {
			return filepath.Join(stepDirAbs, fmt.Sprintf("round-%d-answer.md", round))
		},
	}
	// adapter は単純な forwarder (workflowctx.AgentCaller → inquiryloop.AgentCaller)。
	// Previous Response の前置は本 runInquiryLoop 側で skillPromptCmd に埋め込み済みのため、
	// adapter には IncludePreviousResponse 経路を持たせない (slash-first 順を保つため
	// foundation 側前置経路は spec-loop からは使わない)。
	adapter := agentCallerAdapter{
		inner:   r.agent,
		onEvent: r.onEvent,
	}

	result, err := engine.RunPrompt(
		r.ctx,
		adapter,
		r.cwd,
		inquiryloop.RunInput{
			PromptCommand: skillPromptCmd,
			PromptLabel:   promptLabel,
		},
		func(round inquiryloop.Round) {
			_ = persistInquiry(r.runDir, step.Order, step.Phase, round.Index, round.Inquiry)
			_ = persistAnswer(r.runDir, step.Order, step.Phase, round.Index, r.persona.Name, round.AnswerText)
		},
	)
	if err != nil {
		return inquiryloop.FinalResult{}, fmt.Errorf("specLoopRunner.runInquiryLoop(%s): %w", step.Phase, err)
	}
	return result, nil
}

// prevPhaseForOrder は order - 1 に対応する phase 識別子を steps テーブルから引く純関数。
//
// 戻り値:
//   - order <= 1            → "" (前段なし)
//   - 2 <= order <= len(steps) → steps[order-2].Phase
//   - order > len(steps)    → "" (範囲外)
//
// 例: prevPhaseForOrder(2) → "spec-init"、prevPhaseForOrder(4) → "validate-gap"、
//
//	prevPhaseForOrder(6) → "validate-design"
//
// 用途: previousSummaryForStep の on-disk fallback パス組み立て
// (`runDir/<order-1:02d>-<prevPhase>/result.json`) で利用される (kiro-spec-loop-facets Req 1.1)。
//
// 不変性: `steps` テーブル不変 (Req 3.1) を前提とする純関数。同 input で同 output を返し、
// runtime mutation / I/O / global state への依存を持たない。
func prevPhaseForOrder(order int) string {
	if order <= 1 || order > len(steps) {
		return ""
	}
	return steps[order-2].Phase
}

// previousSummaryForStep は前段 phase の Summary を二段フォールバックで解決する純関数 helper。
//
// 優先順位:
//
//	(1) step.Order == 1                                  → "" (前段なし)
//	(2) len(state.StepResults) > 0                       → state.StepResults[len-1].Summary (in-memory hit)
//	(3) step.Order > 1 かつ state.StepResults が空        → runDir/<order-1:02d>-<prevPhase>/result.json の summary (on-disk fallback)
//	(4) (3) の読み取り失敗 (ENOENT / 権限不足 / JSON 不正 / summary 空) → "" (graceful degradation)
//
// graceful degradation: on-disk fallback は読み取り専用で、I/O / decode 失敗を呼び出し側に
// error として伝播せず "" に倒す。これにより Req 1.4 経路 (前置なしで後段起動継続) に合流する。
//
// 不変性:
//   - in-memory hit ケース (2) では disk I/O を一切行わない (Design §Components Invariants)。
//   - prevPhase 解決は prevPhaseForOrder に委譲し、`steps` テーブル不変性に依存する。
//   - `os.ReadFile` + `json.Unmarshal` の薄いラッパで、本ワークフロー側に切り詰め / provider 分岐は持たない。
//
// 用途: runInquiryLoop で skillPromptCmd の後置ブロック (previousResponseBlock) に
// 渡す前段 summary を解決する。空文字を返したときは previousResponseBlock が
// `# Previous Response` ブロック自体を省略する graceful degradation 経路に合流する。
func previousSummaryForStep(state *runState, step promptStep, runDir string) string {
	if step.Order == 1 {
		return ""
	}
	// PrevOverride は retry 注入経路 (validator NO-GO / advisory を architect step 4 に渡す) の最優先 seam。
	// 空文字値は「override 不在」として扱い、既存ロジックに倒れる (graceful)。
	if state != nil && state.PrevOverride != nil {
		if v, ok := state.PrevOverride[step.Order]; ok && v != "" {
			return v
		}
	}
	if state != nil && len(state.StepResults) > 0 {
		return state.StepResults[len(state.StepResults)-1].Summary
	}
	prevPhase := prevPhaseForOrder(step.Order)
	if prevPhase == "" {
		return ""
	}
	resultPath := filepath.Join(stepDir(runDir, step.Order-1, prevPhase), "result.json")
	data, err := os.ReadFile(resultPath)
	if err != nil {
		return ""
	}
	var payload RunResultStep
	if err := json.Unmarshal(data, &payload); err != nil {
		return ""
	}
	return payload.Summary
}

// personaBlock は ResolvedFacet を skill command の **後ろ**に連結するための
// `\n\n---\n\n# Persona\n\n<Content>` 形式 Markdown ブロックに整形する純関数。
//
// slash command (`/kiro-spec-xxx <feature>`) を prompt の **先頭**に保つため、
// Persona ブロックは前置 (prefix) ではなく後置 (append) する。Claude Code の
// skill 起動は先頭トークンが `/<skill>` でないと走らない仕様への対応。
//
// 戻り値は session A (`inquiryloop.RunInput.PromptCommand`) で skill コマンドの
// 後ろに連結される。session B (`inquiryloop.AnswerContext.PromptCommand`) には
// 適用しない (session B 既存 `# Persona (architecture-quality)` 構造への
// 二重 persona 注入 / markdown bullet 崩れを回避するため)。
//
// 不変性:
//   - 戻り値は常に `\n\n---\n\n# Persona\n\n` で始まる (skill コマンドからの区切り)
//   - resolved.Content が空文字でも形式を維持する (defensive)
//   - resolved.Content の前後 trim や改行正規化は行わない (同 input で同 output)
func personaBlock(resolved facet.ResolvedFacet) string {
	return "\n\n---\n\n# Persona\n\n" + resolved.Content
}

// validationContractBlockText は RequiresVerdict=true の skill 呼び出しの prompt に注入する
// Output Contract ブロックの本文。SKILL.md を変更せずに workflow 層から
// 「verdict / has_advisory / advisory の使い分け + retry セマンティクス」を validator に伝える。
//
// 重要:
//   - has_advisory=true のとき advisory フィールド (非空文字列) は **必須** (parser で強制)
//   - go && !has_advisory:    workflow は次段 (spec-tasks) へ進む
//   - go && has_advisory=true: workflow は spec-design に戻り advisory を反映させて再評価する
//     (go+advisory budget で 3 回まで。3 回目なら advisory 記録のうえ通過)
//   - no_go:                  workflow は spec-design に戻り summary + advisory を反映させて再評価する
//     (no_go budget で 3 回まで。3 回目なら hard abort)
//
// 「軽い指摘」をしたいケースは has_advisory=true + verdict=go で表現し、本当に通せない判定だけ no_go にする。
const validationContractBlockText = `# Output Contract (workflow gate)

このフェーズは workflow gate です。通常の has_inquiry / summary / verdict に加えて、次の advisory 契約に従ってください:

- has_advisory (boolean): 修正アドバイスや軽微な指摘がある場合 true、なければ false。
- advisory (string): has_advisory=true のとき必須。空文字は不可。具体的な修正方針を 1〜数文で記述する。

判定の使い分け:
- verdict=go && has_advisory=false: 設計は実装可能で指摘なし。workflow は次段に進む。
- verdict=go && has_advisory=true:  概ね妥当だが軽微な指摘あり。workflow は spec-design に戻り advisory を architect に渡して再評価する。
- verdict=no_go:                    critical な不整合があり次段に進められない。summary に問題、advisory (任意) に修正方針を書く。workflow は spec-design に戻る。

リトライ上限は種別ごとに 3 試行:
- no_go は go+advisory とは別 budget で数える。no_go が 3 回: workflow は abort する。
- go+advisory は no_go とは別 budget で数える。go+advisory が 3 回: workflow は advisory を記録のうえ完走する (degradation)。

advisory 本文の指針:
- 具体的に書く。「ここを直して」だけでは architect が動けない。
- 該当する requirement ID (Rx.y) と spec / design / research の該当箇所を引用する。
- 「なぜ」を 1 行で添える (architect が修正の優先度を判断できるように)。
`

// validationContractBlock は RequiresVerdict が true のとき Output Contract ブロックを返し、
// false のとき空文字を返す純関数。slash command と persona block の間に挟むことで、
// SKILL の所有する判定ロジックを変えずに workflow 側の契約を validator に追加で伝える。
func validationContractBlock(requiresVerdict bool) string {
	if !requiresVerdict {
		return ""
	}
	return "\n\n---\n\n" + validationContractBlockText
}

// previousResponseBlock は前段 phase の summary を skill command の **後ろ**に
// 連結するための `\n\n---\n\n# Previous Response\n\n<content>` 形式 Markdown
// ブロックに整形する純関数。
//
// 空文字 (前段なし / 取得不能) のときはブロック自体を省略し空文字を返す。
// graceful degradation: 1 段目 spec-init や on-disk fallback 失敗時に
// 自動的に前置 no-op に倒れる。
//
// 切り詰め: kind により切り詰め policy を切替える。
//   - facet.PromptContextSummary: 既定値。前段 summary を 2000 rune で切り詰める
//   - facet.PromptContextDiagnostic: 切り詰めなし。validator NO-GO / advisory 本文を
//     architect に欠落させずフィードバックするための retry 注入経路で使う
//
// 用途: spec-loop は foundation 側の `IncludePreviousResponse` 前置経路を
// 使わず (前置すると skill command が先頭でなくなる)、本関数で skill コマンドの
// 後ろに自前で連結することで slash-first 順を保つ。
func previousResponseBlock(prev string, kind facet.PromptContextKind) string {
	if prev == "" {
		return ""
	}
	trimmed := facet.TrimPromptContext(kind, prev, "")
	return "\n\n---\n\n# Previous Response\n\n" + trimmed
}

// specPathsForPhase は kiro-spec-loop の各フェーズで回答エージェントに参照させたい
// spec ファイルの絶対パスを返す。フェーズごとに既に生成済みのファイルだけを列挙する
// (例: spec-init の段では requirements.md / design.md / tasks.md がまだ存在しないので含めない)。
//
// 未知の phase に対しては空スライスを返し、AnswerContext.SpecPaths が空でも
// `# Context` ブロック側のサブセクションが省かれるだけで挙動の安全側に振る。
//
// `specDir` は `<cwd>/.kiro/specs/<feature>/` の絶対パス。`briefAbsPath` は
// `<specDir>/brief.md` を抽象化した呼び出し側の保持値で、setup() 段階で確定済み。
func specPathsForPhase(specDir, briefAbsPath, phase string) []string {
	requirements := filepath.Join(specDir, "requirements.md")
	design := filepath.Join(specDir, "design.md")
	tasks := filepath.Join(specDir, "tasks.md")
	switch phase {
	case "spec-init":
		return []string{briefAbsPath}
	case "spec-requirements":
		return []string{briefAbsPath, requirements}
	case "validate-gap":
		return []string{briefAbsPath, requirements}
	case "spec-design":
		return []string{briefAbsPath, requirements, design}
	case "validate-design":
		return []string{requirements, design}
	case "spec-tasks":
		return []string{requirements, design, tasks}
	default:
		return nil
	}
}

// runStepFixPhase は rounds > 0 で完走した step に対して fix phase を実行する。
//
// 既存の runFixPhase に委譲する薄いラッパー。fix phase 自体は best-effort 設計で、
// 失敗（agent error / success=false / schema 違反 / has_inquiry=true）でも /git-commit に進む。
func (r *specLoopRunner) runStepFixPhase(step promptStep, lastSession, promptLabel string) RunResultStepFix {
	return runFixPhase(r.ctx, r.agent, r.cwd, r.state.Feature, r.runDir, step, lastSession, r.persona.Name, promptLabel, r.onEvent)
}

// applyApprovalForPhase は各段完走後に対応する approval / 完成フラグを spec.json に書き込む。
//
//   - validate-gap     → approvals.requirements.approved=true
//   - validate-design  → approvals.design.approved=true
//   - spec-tasks       → approvals.tasks.approved=true
//     phase="tasks-approved" / ready_for_implementation=true
//
// best-effort: 書き込み失敗は次段へ進む（次回 listEligibleFeatures で再選択して再実行可能）。
// 上記以外の phase（spec-init / spec-requirements / spec-design）は no-op。
//
// spec-tasks 完走時の追加処理は、step 6 が走り切ること自体が「全段 abort なしで完走した」
// 証 (step 6 は最終段、validate-design retry も step 4 までしか巻き戻らない) であるため、
// この時点で `phase="tasks-approved"` / `ready_for_implementation=true` まで進めて
// 「`/kiro-impl` 着手可能」状態にしておく。`setApprovalApproved` / `markSpecReady` は
// 共に冪等なので、既に `/kiro-spec-tasks` skill 側で同状態まで進めていた場合は no-op。
//
// spec-tasks 分岐は step 6 commit の直前に走るため、書き込み結果が同 step の
// `/git-commit` で自動的にコミットへ含まれる (uncommitted 差分にならない)。
func (r *specLoopRunner) applyApprovalForPhase(phase string) {
	switch phase {
	case "validate-gap":
		_ = setApprovalApproved(r.specJSONPath, "requirements")
	case "validate-design":
		_ = setApprovalApproved(r.specJSONPath, "design")
	case "spec-tasks":
		_ = setApprovalApproved(r.specJSONPath, "tasks")
		_ = markSpecReady(r.specJSONPath)
	}
}

// runCommit は /git-commit を fresh session で呼ぶ（要件 6.4 / 7.1）。
func (r *specLoopRunner) runCommit(step promptStep, promptLabel string) (provider.AgentResponse, error) {
	commitStepID := fmt.Sprintf("commit-%02d-%s", step.Order, step.Phase)
	return gitworkflow.CallCommit(r.ctx, r.agent, r.cwd, r.onEvent, commitStepID, promptLabel+"-commit")
}

// handleStepAbort は engine 由来 abort（loop-limit / prompt-failure / schema-violation）を
// stepResult / state.Failure に確定し、abort() を呼んで result.json + Error event を発火する。
func (r *specLoopRunner) handleStepAbort(step promptStep, promptLabel string, result inquiryloop.FinalResult) error {
	reason := RunTerminationReason(result.Termination)
	sr := stepResult{
		Order:       step.Order,
		Phase:       step.Phase,
		Rounds:      result.Rounds,
		LastSession: result.LastSession,
		Summary:     result.Summary,
		Verdict:     result.Verdict,
		CommitOK:    false,
	}
	if result.Failure != nil {
		r.state.Failure = &RunResultFailure{
			Round:      result.Failure.Round,
			Reason:     string(result.Failure.Reason),
			Message:    result.Failure.Message,
			RawContent: result.Failure.RawContent,
		}
	}
	r.state.StepResults = append(r.state.StepResults, sr)
	if perr := persistStepResult(r.runDir, sr); perr != nil {
		return fmt.Errorf("specLoopRunner.handleStepAbort: persistStepResult abort: %w", perr)
	}
	msg := fmt.Sprintf("中断: %s で %s", promptLabel, reason)
	if result.Failure != nil && result.Failure.Message != "" {
		msg = fmt.Sprintf("中断: %s で %s（%s）", promptLabel, reason, result.Failure.Message)
	}
	return r.abort(reason, &RunResultAbortStep{Order: step.Order, Phase: step.Phase}, msg)
}

// maxValidateDesignNoGoAttempts は validate-design の critical NO-GO retry 上限。
//
// 初回 + 再試行 2 回 = 3 attempts。試行を超えても収束しない場合:
// verdict=no_go は RunReasonValidationNoGo で hard abort する。
const maxValidateDesignNoGoAttempts = 3

// maxValidateDesignAdvisoryAttempts は validate-design の軽微修正 retry 上限。
//
// 初回 + 再試行 2 回 = 3 attempts。試行を超えても verdict=go && has_advisory=true
// が続く場合は degradation completed として advisory を rr.Advisories に残して step 6 へ進む。
const maxValidateDesignAdvisoryAttempts = 3

// maxValidateDesignPreflightAttempts は 05-validate-design 前の deterministic artifact
// preflight retry 上限。
//
// LLM validator の no_go ではないため no_go budget とは分離しつつ、04 が design artifact を
// 生成できない状態で無限に巻き戻ることは防ぐ。
const maxValidateDesignPreflightAttempts = 3

// retryFromOrderForValidateDesign は validate-design (step 5) の retry 時に巻き戻す step.Order。
// architect (spec-design = step 4) に validator フィードバックを Previous Response として注入し再生成させる。
const retryFromOrderForValidateDesign = 4

// handleValidateDesignPreflight は 05-validate-design を LLM validator に渡す前に、
// 04-spec-design が最低限の設計 artifact を生成済みかを機械的に確認する。
//
// 不足がある場合は 05 を呼ばずに step 4 へ差し戻し、validator の no_go budget を消費しない。
func (r *specLoopRunner) handleValidateDesignPreflight(step promptStep, promptLabel string) (stepOutcome, bool, error) {
	feedback := validateDesignPreflightFeedback(r.cwd, r.state.Feature, r.specJSONPath)
	if feedback == "" {
		return stepOutcome{}, false, nil
	}

	if r.state.ValidateDesignAttempts == 0 {
		r.state.ValidateDesignAttempts = 1
	}
	attempt := r.state.ValidateDesignAttempts
	r.state.ValidateDesignPreflightAttempts++
	preflightAttempt := r.state.ValidateDesignPreflightAttempts
	if r.state.ValidateDesignPreflightAttempts < maxValidateDesignPreflightAttempts {
		r.state.recordPreflightValidationFeedback(
			step.Order,
			step.Phase,
			preflightAttempt,
			feedback,
			ValidationRetryActionRetrySpecDesign,
			false,
		)
		return r.triggerValidationRetry(feedback), true, nil
	}

	r.state.recordPreflightValidationFeedback(
		step.Order,
		step.Phase,
		preflightAttempt,
		feedback,
		ValidationRetryActionStopForHuman,
		true,
	)
	sr := stepResult{
		Order:    step.Order,
		Phase:    step.Phase,
		Summary:  feedback,
		Verdict:  inquiryloop.VerdictNoGo,
		Attempt:  attempt,
		CommitOK: false,
	}
	r.state.StepResults = append(r.state.StepResults, sr)
	if perr := persistStepResult(r.runDir, sr); perr != nil {
		return stepOutcome{}, true, fmt.Errorf("specLoopRunner.handleValidateDesignPreflight: persistStepResult: %w", perr)
	}
	msg := fmt.Sprintf("中断: %s preflight で %s（%s）", promptLabel, RunReasonValidationNoGo, feedback)
	return stepOutcome{Aborted: true}, true, r.abort(RunReasonValidationNoGo, &RunResultAbortStep{Order: step.Order, Phase: step.Phase}, msg)
}

// validateDesignPreflightFeedback は validate-design に進むための deterministic artifact 条件を検査し、
// 問題があれば step 4 へ戻すための feedback 本文を返す。問題がなければ空文字を返す。
func validateDesignPreflightFeedback(cwd, feature, specJSONPath string) string {
	specDir := filepath.Join(cwd, ".kiro", "specs", feature)
	var issues []string

	raw, err := os.ReadFile(specJSONPath)
	if err != nil {
		issues = append(issues, fmt.Sprintf("spec.json を読み込めません: %v", err))
	} else {
		var spec struct {
			Approvals struct {
				Design struct {
					Generated bool `json:"generated"`
				} `json:"design"`
			} `json:"approvals"`
		}
		if err := json.Unmarshal(raw, &spec); err != nil {
			issues = append(issues, fmt.Sprintf("spec.json を JSON として解釈できません: %v", err))
		} else if !spec.Approvals.Design.Generated {
			issues = append(issues, "spec.json の approvals.design.generated が true ではありません")
		}
	}

	designPath := filepath.Join(specDir, "design.md")
	designRaw, err := os.ReadFile(designPath)
	switch {
	case err != nil:
		issues = append(issues, fmt.Sprintf("%s が存在しないか読み込めません: %v", designPath, err))
	case strings.TrimSpace(string(designRaw)) == "":
		issues = append(issues, fmt.Sprintf("%s が空です", designPath))
	}

	if len(issues) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("validate-design preflight failed: 04-spec-design の成果物が validate-design に進める状態ではありません。\n")
	b.WriteString("次の点を 04-spec-design で解消してから再実行してください:\n")
	for _, issue := range issues {
		b.WriteString("- ")
		b.WriteString(issue)
		b.WriteString("\n")
	}
	b.WriteString("この deterministic preflight は 05-validate-design を呼び出す前に失敗しているため、validate-design の no_go budget には数えません。")
	return b.String()
}

// handleValidationVerdict は validate-design の GO/NO-GO gate を remediation packet 化し、
// RemediationRouter で retry / abort / degradation / continue を決定する。
//
// 4 経路:
//  1. verdict=go && !has_advisory          → 完走、次段へ (outcome zero value)
//  2. verdict=go && has_advisory / no_go   → packet 化 → route → retry / degrade / abort
//  3. remediation required fields 欠落     → schema-violation abort
//  4. verdict 不正 / 欠落                    → schema-violation abort
//
// retry 経路では sr を StepResults に append せず、step 4 の旧 stepResult もトリムする
// (= 試行ごとに「最終 attempt のみ rr.Steps に残す」契約)。
// PrevOverride[4] には formatValidationRemediationPreviousResponse の Markdown を入れる。
// Advisories[] と validationFeedback[] の双方に試行履歴を残す。
func (r *specLoopRunner) handleValidationVerdict(step promptStep, promptLabel string, sr stepResult, fr inquiryloop.FinalResult) (stepOutcome, error) {
	if r.state.ValidateDesignAttempts == 0 {
		r.state.ValidateDesignAttempts = 1
	}
	attempt := r.state.ValidateDesignAttempts

	if fr.Verdict == inquiryloop.VerdictGo && !fr.HasAdvisory {
		return stepOutcome{}, nil
	}

	packet, err := parseValidationRemediationPacket(fr, attempt)
	if err != nil {
		return r.abortValidationRemediationSchemaViolation(step, promptLabel, sr, fr, err)
	}
	if packet == nil {
		return r.abortValidationRemediationSchemaViolation(step, promptLabel, sr, fr,
			remediationSchemaViolation("remediation packet required for verdict=%q has_advisory=%v", fr.Verdict, fr.HasAdvisory))
	}

	validatedPacket, signals, err := validateRemediationOwnership(r.cwd, r.state.Feature, *packet)
	if err != nil {
		return r.abortValidationRemediationSchemaViolation(step, promptLabel, sr, fr, err)
	}

	verdict := string(fr.Verdict)
	decision := r.state.routeValidationRemediation(step.Order, step.Phase, validatedPacket, signals, verdict, "")
	r.recordRemediationAdvisory(step, attempt, verdict, validatedPacket, signals)

	switch decision.RetryAction {
	case ValidationRetryActionContinue:
		return stepOutcome{}, nil
	case ValidationRetryActionRetrySpecDesign:
		feedback := formatValidationRemediationPreviousResponse(validatedPacket, signals)
		return r.triggerValidationRetry(feedback), nil
	case ValidationRetryActionDegradeAndContinue:
		return stepOutcome{}, nil
	case ValidationRetryActionStopForHuman:
		r.state.StepResults = append(r.state.StepResults, sr)
		if perr := persistStepResult(r.runDir, sr); perr != nil {
			return stepOutcome{}, fmt.Errorf("specLoopRunner.handleValidationVerdict: persistStepResult abort: %w", perr)
		}
		detail := firstNonEmpty(decision.Message, validatedPacket.Summary)
		msg := fmt.Sprintf("中断: %s で %s（%s）", promptLabel, RunReasonValidationNoGo, detail)
		return stepOutcome{Aborted: true}, r.abort(
			RunReasonValidationNoGo,
			&RunResultAbortStep{Order: step.Order, Phase: step.Phase},
			msg,
		)
	default:
		return stepOutcome{}, nil
	}
}

func (r *specLoopRunner) abortValidationRemediationSchemaViolation(
	step promptStep,
	promptLabel string,
	sr stepResult,
	fr inquiryloop.FinalResult,
	err error,
) (stepOutcome, error) {
	r.state.Failure = &RunResultFailure{
		Round:      sr.Rounds,
		Reason:     string(RunReasonSchemaViolation),
		Message:    err.Error(),
		RawContent: serializeValidationRemediationFailureRawContent(fr),
	}
	r.state.StepResults = append(r.state.StepResults, sr)
	if perr := persistStepResult(r.runDir, sr); perr != nil {
		return stepOutcome{}, fmt.Errorf("specLoopRunner.handleValidationVerdict: persistStepResult schema violation: %w", perr)
	}
	msg := fmt.Sprintf("中断: %s で %s（validation remediation schema violation）", promptLabel, RunReasonSchemaViolation)
	return stepOutcome{Aborted: true}, r.abort(
		RunReasonSchemaViolation,
		&RunResultAbortStep{Order: step.Order, Phase: step.Phase},
		msg,
	)
}

func serializeValidationRemediationFailureRawContent(fr inquiryloop.FinalResult) string {
	payload := map[string]any{
		"summary":      fr.Summary,
		"verdict":      fr.Verdict,
		"has_advisory": fr.HasAdvisory,
		"advisory":     fr.Advisory,
	}
	if len(fr.Extensions) > 0 {
		payload["extensions"] = fr.Extensions
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fr.Summary
	}
	return string(raw)
}

func (r *specLoopRunner) recordRemediationAdvisory(
	step promptStep,
	attempt int,
	verdict string,
	packet ValidationRemediationPacket,
	signals []ValidationDeterministicSignal,
) {
	r.recordAdvisory(step, attempt, verdict, formatValidationRemediationPreviousResponse(packet, signals))
}

// triggerValidationRetry は validate-design の retry セマンティクスを構成する:
//   - PrevOverride[step4] = feedback (validator NO-GO 文 or advisory)
//   - 旧 step 4 / step 5 attempt の stepResult を StepResults から除去
//   - ValidateDesignAttempts++ (次の試行番号に進める)
//   - stepOutcome{RetryFromOrder: 4} を返して Run() に巻き戻しを促す
//
// 戻り値の error 経路はない (純粋に状態遷移と stepOutcome 構築のみ)。
func (r *specLoopRunner) triggerValidationRetry(feedback string) stepOutcome {
	r.state.ValidateDesignAttempts++
	if r.state.PrevOverride == nil {
		r.state.PrevOverride = map[int]string{}
	}
	r.state.PrevOverride[retryFromOrderForValidateDesign] = feedback
	r.state.StepResults = trimStepResultsBeforeOrder(r.state.StepResults, retryFromOrderForValidateDesign)
	return stepOutcome{RetryFromOrder: retryFromOrderForValidateDesign}
}

// recordAdvisory は試行 1 件分のフィードバックを state.Advisories に追記する。
func (r *specLoopRunner) recordAdvisory(step promptStep, attempt int, verdict, advisory string) {
	r.state.Advisories = append(r.state.Advisories, RunResultAdvisory{
		Order:    step.Order,
		Phase:    step.Phase,
		Attempt:  attempt,
		Verdict:  verdict,
		Advisory: advisory,
	})
}

func (r *specLoopRunner) currentValidateDesignAttempt() int {
	if r.state == nil || r.state.ValidateDesignAttempts == 0 {
		return 1
	}
	return r.state.ValidateDesignAttempts
}

// composeNoGoFeedback は no_go フィードバックの本文を組み立てる純関数。
// has_advisory=true なら advisory 本文を summary より前に置き、architect の入力で
// 「修正方針 → 失敗理由」の順に読めるようにする。advisory 不在なら summary をそのまま返す。
func composeNoGoFeedback(summary string, hasAdvisory bool, advisory string) string {
	if !hasAdvisory || advisory == "" {
		return summary
	}
	if summary == "" {
		return advisory
	}
	return advisory + "\n\n---\n\n" + summary
}

// trimStepResultsBeforeOrder は srs の末尾から「Order >= cutOrder」の連続要素を削除した新スライスを返す。
//
// 例: [step1, step2, step3, step4] で cutOrder=4 → [step1, step2, step3]
//
// 用途: validate-design retry 時に step4 attempt N の stepResult を除去し、attempt N+1 で
// 再 append できるようにする。中央の要素 (step3 など) は触らない (defensive: 末尾だけ見る)。
func trimStepResultsBeforeOrder(srs []stepResult, cutOrder int) []stepResult {
	cut := len(srs)
	for i := len(srs) - 1; i >= 0; i-- {
		if srs[i].Order >= cutOrder {
			cut = i
			continue
		}
		break
	}
	if cut == len(srs) {
		return srs
	}
	return srs[:cut]
}

// abort は中断パス共通の終端処理:
//   - state.Termination / state.AbortedAt を確定
//   - result.json を書き出す
//   - workflow.WorkflowError event を発火する
//
// 戻り値の error は persistRunResult の I/O 失敗のみ。
func (r *specLoopRunner) abort(reason RunTerminationReason, abortStep *RunResultAbortStep, message string) error {
	r.state.Termination = reason
	abortedAt := time.Now().UTC()
	r.state.AbortedAt = &abortedAt
	rr := buildRunResult(r.state, time.Time{}, abortStep, message)
	if err := persistRunResult(r.runDir, rr); err != nil {
		return fmt.Errorf("specLoopRunner.abort: persistRunResult: %w", err)
	}
	if r.onEvent != nil {
		r.onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: message})
	}
	return nil
}

// complete は 6 段完走時の終端処理:
//   - state.Termination=completed を確定
//   - result.json を書き出す
//   - workflow.WorkflowDone event を発火する
func (r *specLoopRunner) complete() error {
	r.state.Termination = RunReasonCompleted
	completedAt := time.Now().UTC()
	rr := buildRunResult(r.state, completedAt, nil, "")
	if err := persistRunResult(r.runDir, rr); err != nil {
		return fmt.Errorf("specLoopRunner.complete: persistRunResult: %w", err)
	}
	if r.onEvent != nil {
		r.onEvent(workflow.WorkflowEvent{
			Type:   workflow.WorkflowDone,
			Result: fmt.Sprintf("Kiro 仕様作成ループ完了: %s（%d プロンプト完了）", r.state.Feature, len(steps)),
		})
	}
	return nil
}
