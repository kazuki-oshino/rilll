// Package workflow の本ファイルは、kiro-spec-loop ワークフロー（Kiro 仕様作成ループ）の
// 共通型と監査ログ永続化ヘルパを定義する。
//
// 本ファイルが所有するのは以下のレイヤ:
//   - workflow.RunTerminationReason: 1 走行の終端理由（8 値 enum）
//   - 内部 state 型: runState / stepResult / runContext
//   - result.json 用 JSON 構造: RunResult / RunResultStep / RunResultAbortStep
//   - audit persistence helpers: persistContext / persistInquiry / persistAnswer /
//     persistStepResult / persistRunResult
//
// オーケストレーション本体（KiroSpecLoop / steps[] / run() 本体）は本ファイルでは持たず、
// 後続タスク (4.1 / 4.2) で本ファイルか兄弟ファイルに追加される。
//
// design.md「workflow.KiroSpecLoop」「.rilll/kiro-spec-loop/<run-id>/ ファイル配置」
// および tasks.md 3.1 契約に従う。
package kiro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kazuki-oshino/rilll/internal/inquiryloop"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// RunTerminationReason は kiro-spec-loop 1 走行の終端理由を表す（10 値 enum）。
//
// engine 由来 4 値 (completed / loop-limit / prompt-failure / schema-violation) を
// 同名で受け継ぎ、workflow 固有 6 値 (cancelled-by-user / missing-feature /
// missing-persona / missing-phase-persona / commit-failure / validation-no-go) を加える。
//
// engine 側 inquiryloop.TerminationReason と本型は型としては別だが、
// 共通 4 値については「文字列値レベルでの昇格関係」を持つ。これにより
// result.json.terminationReason の値領域 = engine 4 値 ⊂ workflow 10 値 となり、
// 後続の jq 集計 / トレーサビリティを単一 enum で扱える。
//
// 文字列値は result.json の永続フォーマット契約の一部であり、変更すると
// 過去の result.json と互換が壊れる。安易に rename しないこと。
type RunTerminationReason string

const (
	// RunReasonCompleted: 6 段すべてが has_inquiry=false で完走 + 各 commit 成功。
	RunReasonCompleted RunTerminationReason = "completed"
	// RunReasonCancelledByUser: feature 選択で ESC キャンセル。agent 呼び出しは 0 件。
	RunReasonCancelledByUser RunTerminationReason = "cancelled-by-user"
	// RunReasonMissingFeature: 対象 feature (brief.md 持ち / phase 未到達) が 0 件。
	RunReasonMissingFeature RunTerminationReason = "missing-feature"
	// RunReasonMissingPersona: session B（回答エージェント）用 architecture-quality Persona facet
	// の解決失敗または空 content を表す。
	// 本 spec で導入する session A 用 phase persona facet の失敗 (RunReasonMissingPhasePersona) と
	// semantics を分離するため、両定数は別個に維持する。
	RunReasonMissingPersona RunTerminationReason = "missing-persona"
	// RunReasonMissingPhasePersona: phase ごとの session A 用 Persona facet
	// (`facet.ResolveFacet(facet.Persona, ...)`) の解決に失敗した場合の終端理由。
	// kiro-spec-loop-facets spec (Req 2.3) で新設。RunReasonMissingPersona と semantics を
	// 分離する理由は、両者が指す Persona 種別 (session B 用 / session A 用) が独立しているため。
	RunReasonMissingPhasePersona RunTerminationReason = "missing-phase-persona"
	// RunReasonPromptFailure: kiro skill 呼び出しが error / success=false（engine 由来）。
	RunReasonPromptFailure RunTerminationReason = "prompt-failure"
	// RunReasonSchemaViolation: 構造化出力がスキーマ違反（engine 由来）。
	RunReasonSchemaViolation RunTerminationReason = "schema-violation"
	// RunReasonLoopLimit: 問い合わせ⇄回答ラウンドが 5 回以内に収束せず（engine 由来）。
	RunReasonLoopLimit RunTerminationReason = "loop-limit"
	// RunReasonCommitFailure: 段の /git-commit が error / success=false。
	RunReasonCommitFailure RunTerminationReason = "commit-failure"
	// RunReasonValidationNoGo: validate-design が verdict=no_go を返したため次段生成を停止。
	RunReasonValidationNoGo RunTerminationReason = "validation-no-go"
)

// runContext は 00-context.json に書き出される初期コンテキスト。
// feature 選択直後（persona 解決成功後・最初の skill 呼び出し前）に persistContext で書き出す。
type runContext struct {
	// Feature は今回処理する feature 名（.kiro/specs/<feature>/）。
	Feature string `json:"feature"`
	// BriefPath は brief.md の絶対パス（kiro skill に渡した参照ファイル経路）。
	BriefPath string `json:"briefPath"`
	// PersonaName は解決したペルソナ名（例: "architecture-quality"）。
	PersonaName string `json:"personaName"`
	// PersonaPath は session B Persona facet の SourcePath（embed: プレフィックス or 絶対パス）。
	PersonaPath string `json:"personaPath"`
	// StartedAt は 00-context.json 書き出し時刻（UTC 推奨）。
	StartedAt time.Time `json:"startedAt"`
}

// runState は kiro_spec_loop.run の実行中ローカル状態。
// 1 回の workflow.Definition.Fn 呼び出し内でのみ生存し、並行実行は想定しない。
//
// 各段の処理が進むにつれ StepResults が増え、abort 時には Termination /
// AbortedAt が確定し、persistRunResult が runState から RunResult を組み立てて
// result.json に書き出す。
type runState struct {
	// RunID は workflow.RunMetadata から取得した実行 ID。
	RunID string
	// Feature は処理対象の feature 名。
	Feature string
	// BriefContent は brief.md の本文（必要に応じてコピーするが、現契約では skill にパス参照させるため未使用）。
	BriefContent string
	// PersonaPath は session B Persona facet の SourcePath。
	PersonaPath string
	// PersonaName は workflow-visible logical Persona 名。
	PersonaName string
	// StartedAt は run 開始時刻。
	StartedAt time.Time
	// StepResults は各段の集計（abort 時は途中まで）。
	StepResults []stepResult
	// Termination は確定した終端理由。未確定時はゼロ値（空文字列）。
	Termination RunTerminationReason
	// AbortedAt は abort 時刻（成功時は nil）。
	AbortedAt *time.Time
	// Failure は engine 由来 abort（loop-limit / prompt-failure / schema-violation）の詳細。
	// それ以外（commit-failure / cancelled-by-user / missing-feature / missing-persona / completed）
	// では nil のまま。result.json.failure に表出する（omitempty で nil は出力されない）。
	Failure *RunResultFailure
	// ValidateDesignAttempts は validate-design retry cycle の現在試行番号（1 始まり）。
	// StepResult.Attempt と Advisories[].Attempt に表出する表示用の通番で、
	// retry budget 判定には下の種別別 counters を使う。
	ValidateDesignAttempts int
	// ValidateDesignAdvisoryAttempts は verdict=go && has_advisory=true の発生回数。
	// 軽微修正の retry budget として使い、no_go budget とは独立させる。
	ValidateDesignAdvisoryAttempts int
	// ValidateDesignNoGoAttempts は verdict=no_go の発生回数。
	// critical な設計不整合の retry budget として使い、go+advisory budget とは独立させる。
	ValidateDesignNoGoAttempts int
	// ValidateDesignPreflightAttempts は 05-validate-design を呼ぶ前の deterministic artifact
	// preflight に失敗した回数。LLM validator の no_go 判定ではないため
	// ValidateDesignNoGoAttempts とは分離して数える。
	ValidateDesignPreflightAttempts int
	// PrevOverride は retry 時に previousSummaryForStep が返す summary を強制上書きするためのマップ。
	// key=step.Order, value=注入する Previous Response 本文 (validator NO-GO 文 or advisory)。
	// 空文字の値は「override 不在」と同じ扱い。retry ループ以外では nil のまま使われない。
	PrevOverride map[int]string
	// Advisories は試行履歴のうち advisory 付き完走（degradation 含む）の本文を時系列で記録する。
	// retry 試行ごとに validate-design が返した advisory がここに積まれ、最終 result.json に表出する。
	Advisories []RunResultAdvisory
	// BlockerRetry は validate-design 実行後 blocker の same/new 判定用 state。
	// ValidateDesignPreflightAttempts とは分離し、05 実行後 feedback のみを数える。
	BlockerRetry validationBlockerRetryState
	// ValidationFeedback は result.json.validationFeedback[] への projection 正本。
	ValidationFeedback []RunResultValidationFeedback
}

// stepResult は 1 つの kiro プロンプト段の集計（commit 結果込み）。
// runState.StepResults に蓄積され、その段の /git-commit 完了直後に
// persistStepResult が JSON 化して <NN>-<phase>/result.json に書き出す。
type stepResult struct {
	// Order は 1〜6 の段番号。
	Order int
	// Phase は "spec-init" / "spec-requirements" / ... のフェーズ識別子。
	Phase string
	// Rounds は inquiryloop が消費した問い合わせ⇄回答ラウンド数（0 ラウンド完走なら 0）。
	Rounds int
	// LastSession は session A（kiro skill 本体）の最終応答 sessionId。
	LastSession string
	// Summary は最終 has_inquiry=false 応答の summary フィールド。
	Summary string
	// Verdict は validation phase が返した GO/NO-GO 判定。通常 phase では空。
	Verdict inquiryloop.ValidationVerdict
	// CommitOK はその段の /git-commit が成功したか。
	CommitOK bool
	// CommitOutput は /git-commit 応答の Content（診断用）。
	CommitOutput string
	// Fix は最終修正フェーズ（fix phase）の結果。rounds > 0 で完走した段のみ非 nil。
	Fix *RunResultStepFix
	// Attempt は validate-design retry の試行番号 (1 始まり)。通常 step では 1。
	Attempt int
}

// RunResult は result.json に書き出すワークフロー全体の結果（公開構造）。
//
// JSON タグは design.md「RunResult JSON スキーマ」に従う。
// completedAt / abortStep / abortMessage / failure は abort / 完走 で出現条件が変わるため
// omitempty で表現する。
type RunResult struct {
	RunID             string               `json:"runId"`
	WorkflowID        string               `json:"workflowId"`
	Feature           string               `json:"feature"`
	PersonaName       string               `json:"personaName"`
	PersonaPath       string               `json:"personaPath"`
	StartedAt         time.Time            `json:"startedAt"`
	CompletedAt       *time.Time           `json:"completedAt,omitempty"`
	TerminationReason RunTerminationReason `json:"terminationReason"`
	AbortStep         *RunResultAbortStep  `json:"abortStep,omitempty"`
	AbortMessage      string               `json:"abortMessage,omitempty"`
	Failure           *RunResultFailure    `json:"failure,omitempty"`
	Steps             []RunResultStep      `json:"steps"`
	// Advisories は validate-design の retry 試行履歴。
	// 1 つの試行 (verdict=no_go or verdict=go+has_advisory=true) ごとに 1 件積まれる。
	// 通常完走 (retry なし) では nil/空。omitempty で出力を抑制する。
	Advisories []RunResultAdvisory `json:"advisories,omitempty"`
	// ValidationFeedback は validate-design / preflight 由来の remediation trace。
	// additive field として既存 consumer が unknown field を無視しても gate を維持できる。
	ValidationFeedback []RunResultValidationFeedback `json:"validationFeedback,omitempty"`
}

// RunResultValidationFeedback は result.json.validationFeedback[] の 1 要素。
type RunResultValidationFeedback struct {
	Source               string                       `json:"source"`
	Order                int                          `json:"order"`
	Phase                string                       `json:"phase"`
	Attempt              int                          `json:"attempt"`
	Verdict              string                       `json:"verdict"`
	VerdictClass         string                       `json:"verdictClass"`
	RetryAction          string                       `json:"retryAction"`
	SameBlockerCount     int                          `json:"sameBlockerCount,omitempty"`
	Blocker              *RunResultValidationBlocker  `json:"blocker,omitempty"`
	DeterministicSignals []RunResultValidationSignal  `json:"deterministicSignals,omitempty"`
	Message              string                       `json:"message,omitempty"`
	Terminal             bool                         `json:"terminal,omitempty"`
}

// RunResultValidationBlocker は validationFeedback[].blocker の blocker 投影。
type RunResultValidationBlocker struct {
	Category          string   `json:"category"`
	Fingerprint       string   `json:"fingerprint"`
	StableKey         string   `json:"stableKey"`
	AffectedArtifacts []string `json:"affectedArtifacts"`
	RecommendedAction string   `json:"recommendedAction"`
	OwnerBoundary     string   `json:"ownerBoundary"`
	OwnerSpec         string   `json:"ownerSpec,omitempty"`
}

// RunResultValidationSignal は validationFeedback[].deterministicSignals[] の 1 要素。
type RunResultValidationSignal struct {
	Kind                  string `json:"kind"`
	Artifact              string `json:"artifact,omitempty"`
	Message               string `json:"message,omitempty"`
	DetectedOwnerBoundary string `json:"detectedOwnerBoundary,omitempty"`
	DetectedOwnerSpec     string `json:"detectedOwnerSpec,omitempty"`
}

// RunResultAbortStep は result.json.abortStep フィールドの内訳。
// abort 時のみ非 nil。「どの段で」止まったかを order / phase で示す。
type RunResultAbortStep struct {
	Order int    `json:"order"`
	Phase string `json:"phase"`
}

// RunResultAdvisory は validate-design の retry 試行で記録された advisory 1 件分。
//
// 発生条件:
//   - verdict=go && has_advisory=true → architect に Advisory を注入して step 4 を再走（retry）
//   - go+advisory budget の 3 回目で verdict=go && has_advisory=true → degradation completed として終端し履歴に残す
//   - verdict=no_go → Verdict="no_go", Advisory には no-go の理由 (sr.Summary または validator advisory) を記録
type RunResultAdvisory struct {
	// Order / Phase は advisory が記録された段（常に 5 / "validate-design"）。
	Order int    `json:"order"`
	Phase string `json:"phase"`
	// Attempt は 1 始まりの試行番号。
	Attempt int `json:"attempt"`
	// Verdict は当該試行の verdict ("go" または "no_go")。
	Verdict string `json:"verdict"`
	// Advisory は validator が返したアドバイス本文 (advisory フィールド or no-go summary)。
	Advisory string `json:"advisory"`
}

// RunResultFailure は engine 由来 abort（loop-limit / prompt-failure / schema-violation）の
// 詳細情報を result.json に表面化するための公開構造。
//
// design.md「Error Categories」が言及する `result.json.failure.rawContent` 契約に対応する。
// inquiryloop.FailureDetail と同形だが、永続フォーマットとして JSON タグを持ち、
// internal レイアウト変更に対する境界を確保する。
//
// 出現条件:
//   - engine 由来 abort（loop-limit / prompt-failure / schema-violation）→ 非 nil
//   - workflow 由来 abort（commit-failure / cancelled-by-user / missing-feature /
//     missing-persona）→ nil（理由は AbortMessage と TerminationReason で十分）
//   - 完走（completed）→ nil
type RunResultFailure struct {
	// Round は失敗が発生したラウンド番号。0 は初回 skill 呼び出し、1+ はループ N 周目。
	Round int `json:"round"`
	// Reason は engine 由来の 4 値文字列のいずれか（completed は出現しない）。
	Reason string `json:"reason"`
	// Message は人間向けの簡潔な説明（例: "agent returned success=false"）。
	Message string `json:"message,omitempty"`
	// RawContent は診断用に保存する agent 生応答（schema-violation 時に特に有効）。
	RawContent string `json:"rawContent,omitempty"`
}

// RunResultStep は result.json.steps[] の各要素。
// 内部 stepResult と同じ情報だが、JSON タグ付き公開型として独立させて
// result.json のフォーマット契約を構造体レベルで明示する。
type RunResultStep struct {
	Order        int               `json:"order"`
	Phase        string            `json:"phase"`
	Rounds       int               `json:"rounds"`
	LastSession  string            `json:"lastSession"`
	Summary      string            `json:"summary"`
	Verdict      string            `json:"verdict,omitempty"`
	CommitOK     bool              `json:"commitOK"`
	CommitOutput string            `json:"commitOutput"`
	Fix          *RunResultStepFix `json:"fix,omitempty"`
	// Attempt は validate-design retry 試行番号 (1 始まり)。retry が発生しない通常 step では 1。
	// omitempty を付けないことで「最終的な試行回数」を常に観察可能にする。
	Attempt int `json:"attempt"`
}

// RunResultStepFix は最終修正フェーズ（fix phase）の結果を result.json に表面化するための公開構造。
//
// fix phase は「engine.RunPrompt が completed で round が 1 件以上回った段」で発動し、
// session A を resume したまま「ここまでのラウンドで決まった方針で実ファイルを修正してください」
// 旨の追加プロンプトを 1 回投げる。Claude Code は会話の流れの中で初期 allowed-tools 制約から外れた
// Edit が通る挙動を持つため、resume での修正の機会を与える設計。
//
// 出現条件:
//   - 当該 step の rounds > 0 で完走 → 非 nil（成功・失敗いずれも記録）
//   - rounds == 0 で完走 / engine 由来 abort / commit 前 → nil
//
// best-effort: 失敗（agent error / success=false / schema 違反 / has_inquiry=true で返却）でも
// /git-commit に進む。OK=false で記録される。
type RunResultStepFix struct {
	// Ran は fix phase が実際に呼び出されたかを表す（nil 識別と冗長だが明示）。
	Ran bool `json:"ran"`
	// OK は fix phase が成功したか（success=true && schema OK && has_inquiry=false）。
	OK bool `json:"ok"`
	// Summary は fix phase 応答の summary フィールド。失敗時は空文字列のことがある。
	Summary string `json:"summary,omitempty"`
	// SessionID は fix phase 全体の最終応答 sessionId（監査用）。
	// 2 ターン構成 (edit → judge) では judge ターンの sessionId が入る。
	// Turn 1 のみ実施した（Turn 1 失敗で Turn 2 をスキップした）場合は edit ターンの sessionId が入る。
	SessionID string `json:"sessionId,omitempty"`
	// EditSession は Turn 1 (fix-edit、OutputFormat=nil でファイル編集を許可) の応答 sessionId。
	// 2 ターン構成導入時に追加。Turn 1 自体が呼び出されない (= Ran=false) ときは空。
	EditSession string `json:"editSession,omitempty"`
	// JudgeSession は Turn 2 (fix-judge、json_schema 強制で要約のみ抽出) の応答 sessionId。
	// Turn 1 失敗で Turn 2 をスキップした場合は空。
	JudgeSession string `json:"judgeSession,omitempty"`
	// Message は失敗時の人間向け説明（例: "agent error: ...", "has_inquiry=true returned"）。
	Message string `json:"message,omitempty"`
}

// persistContext は feature 選択直後（persona 解決成功後）に呼び、
// runDir に 00-context.json を書き出す。
//
// runDir が未作成でも MkdirAll で作る。前段で作成済みでも問題なく冪等に動作する。
func persistContext(runDir string, ctx runContext) error {
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return fmt.Errorf("persistContext: mkdir %s: %w", runDir, err)
	}
	data, err := json.MarshalIndent(ctx, "", "  ")
	if err != nil {
		return fmt.Errorf("persistContext: marshal: %w", err)
	}
	out := filepath.Join(runDir, "00-context.json")
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return fmt.Errorf("persistContext: write %s: %w", out, err)
	}
	return nil
}

// persistInquiry は has_inquiry=true のラウンドが発生したとき、
// <NN>-<phase>/round-<N>-inquiry.md を書き出す。
//
// inquiryloop.Engine の onRound コールバック経由で workflow 層から呼ばれる想定。
// engine 自体はファイル書き出しを行わない（design.md「副作用なし」契約）。
func persistInquiry(runDir string, order int, phase string, round int, inquiry inquiryloop.Inquiry) error {
	dir := stepDir(runDir, order, phase)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("persistInquiry: mkdir %s: %w", dir, err)
	}
	out := filepath.Join(dir, fmt.Sprintf("round-%d-inquiry.md", round))
	if err := os.WriteFile(out, []byte(formatInquiryMarkdown(order, phase, round, inquiry)), 0o644); err != nil {
		return fmt.Errorf("persistInquiry: write %s: %w", out, err)
	}
	return nil
}

// persistAnswer は回答エージェントが返した回答テキストを
// <NN>-<phase>/round-<N>-answer.md として書き出す。
//
// persona 名は監査用にファイル冒頭メタへ含める。回答本文 (answerText) は
// 改行を含む長文を想定し、Markdown の本文セクションに素朴にそのまま埋め込む。
func persistAnswer(runDir string, order int, phase string, round int, personaName, answerText string) error {
	dir := stepDir(runDir, order, phase)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("persistAnswer: mkdir %s: %w", dir, err)
	}
	out := filepath.Join(dir, fmt.Sprintf("round-%d-answer.md", round))
	if err := os.WriteFile(out, []byte(formatAnswerMarkdown(order, phase, round, personaName, answerText)), 0o644); err != nil {
		return fmt.Errorf("persistAnswer: write %s: %w", out, err)
	}
	return nil
}

// persistStepResult はその段の /git-commit 完了直後に呼び、
// <NN>-<phase>/result.json として stepResult を書き出す。
//
// 内部型 stepResult から公開型 RunResultStep に変換するのは、
// stepResult が内部レイアウト変更に追従しやすい一方、result.json の JSON 形は
// 永続フォーマットとして固定したいため。
func persistStepResult(runDir string, sr stepResult) error {
	dir := stepDir(runDir, sr.Order, sr.Phase)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("persistStepResult: mkdir %s: %w", dir, err)
	}
	out := filepath.Join(dir, "result.json")
	attempt := sr.Attempt
	if attempt == 0 {
		attempt = 1
	}
	payload := RunResultStep{
		Order:        sr.Order,
		Phase:        sr.Phase,
		Rounds:       sr.Rounds,
		LastSession:  sr.LastSession,
		Summary:      sr.Summary,
		Verdict:      string(sr.Verdict),
		CommitOK:     sr.CommitOK,
		CommitOutput: sr.CommitOutput,
		Fix:          sr.Fix,
		Attempt:      attempt,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("persistStepResult: marshal: %w", err)
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return fmt.Errorf("persistStepResult: write %s: %w", out, err)
	}
	return nil
}

// persistRunResult は workflow 終端時（完走 or abort）に top-level result.json を書き出す。
//
// 終端理由は RunResult.TerminationReason に格納されており、ここでの書き出しは
// 単純な JSON シリアライズのみ。abort パスでは StepResults が途中までしか
// 揃わないが、JSON 上は steps[] が短くなるだけで構造は同じ。
func persistRunResult(runDir string, rr RunResult) error {
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return fmt.Errorf("persistRunResult: mkdir %s: %w", runDir, err)
	}
	data, err := json.MarshalIndent(rr, "", "  ")
	if err != nil {
		return fmt.Errorf("persistRunResult: marshal: %w", err)
	}
	out := filepath.Join(runDir, "result.json")
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return fmt.Errorf("persistRunResult: write %s: %w", out, err)
	}
	return nil
}

// listEligibleFeatures は kiro-spec-loop の対象となる feature 名一覧を返す。
//
// 含む条件（両方を満たす）:
//
//	(a) <cwd>/.kiro/specs/<feature>/brief.md が存在し、内容が非空である
//	(b) <cwd>/.kiro/specs/<feature>/spec.json が存在しない、または
//	    spec.json.phase が "tasks-generated" 未満（= 既に tasks まで完了した feature は除外）
//
// phase の昇順は次の 4 段階を採用する（design.md / tasks.md 3.2 に従う）:
//
//	initialized < requirements-generated < design-generated < tasks-generated
//
// 上記 4 値以外（未知 phase / spec.json パース不能）は安全側で「対象外」とする。
// これは「未知の値は tasks より進んでいる可能性がある」リスクを避けるための選択であり、
// 既知 phase 集合の追加が必要になったらここを更新する。
//
// 戻り値の順序は呼び出し側の TUI 表示で安定させたいため辞書順にソートする。
//
// .kiro/specs/ ディレクトリ自体が存在しない場合はエラーを返さず空スライスを返す
// （呼び出し側が「missing-feature」分岐に流せるように、I/O エラーと「対象 0 件」を
// 区別しないインターフェースにする）。
func listEligibleFeatures(cwd string) ([]string, error) {
	specsDir := filepath.Join(cwd, ".kiro", "specs")
	entries, err := os.ReadDir(specsDir)
	if err != nil {
		// specs/ ディレクトリ不在は「準備未着手」状態と等価とみなし、空で返す。
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("listEligibleFeatures: read %s: %w", specsDir, err)
	}

	eligible := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		feature := entry.Name()
		featureDir := filepath.Join(specsDir, feature)

		// (a) brief.md が存在し非空であること。空ファイル / ディレクトリ /
		// 読み込みエラーはすべて「準備未完了」と扱い、対象外。
		if !hasNonEmptyBrief(featureDir) {
			continue
		}

		// (b) spec.json が無ければ即対象、ある場合は phase < tasks-generated を要件とする。
		specPath := filepath.Join(featureDir, "spec.json")
		specRaw, err := os.ReadFile(specPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				eligible = append(eligible, feature)
				continue
			}
			// 想定外の I/O エラー（権限不足等）は呼び出し側に判断を委ねる。
			return nil, fmt.Errorf("listEligibleFeatures: read %s: %w", specPath, err)
		}
		var meta struct {
			Phase string `json:"phase"`
		}
		if err := json.Unmarshal(specRaw, &meta); err != nil {
			// spec.json が壊れている feature は対象外（安全側）。
			continue
		}
		if phaseBelowTasksGenerated(meta.Phase) {
			eligible = append(eligible, feature)
		}
	}

	sort.Strings(eligible)
	return eligible, nil
}

// hasNonEmptyBrief は <featureDir>/brief.md が存在し、空でないファイルかを判定する。
// ディレクトリ・存在しない・サイズ 0 はすべて false を返し、I/O エラーも false に集約する
// （listEligibleFeatures の (a) 条件「存在し非空」の最小チェックに揃える）。
func hasNonEmptyBrief(featureDir string) bool {
	briefPath := filepath.Join(featureDir, "brief.md")
	info, err := os.Stat(briefPath)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Size() > 0
}

// phaseBelowTasksGenerated は spec.json.phase が "tasks-generated" 未満か判定する。
//
// 採用順序: initialized(0) < requirements-generated(1) < design-generated(2) < tasks-generated(3)。
// 既知 4 値以外は false を返す（= 対象外）。これは未知 phase が将来追加されたとき、
// 安易に「tasks 以下」とみなして処理対象に含めるのを避けるための保守的選択である。
//
// 既知 phase 集合を増やしたい場合は本関数のみを更新すれば listEligibleFeatures 側の
// 利用面に影響しない（カプセル化）。
func phaseBelowTasksGenerated(phase string) bool {
	switch phase {
	case "initialized", "requirements-generated", "design-generated":
		return true
	default:
		return false
	}
}

// determineStartStep は spec.json の状態を見て、kiro-spec-loop が開始すべき step.Order を返す。
//
// 判定ルール:
//   - spec.json 不在 / 読み込み不能 / JSON パース失敗 → 1 (spec-init から)
//   - approvals.design.approved == true → 6 (spec-tasks から)
//   - approvals.requirements.approved == true → 4 (spec-design から)
//   - その他 → 1 (安全側で最初から)
//
// design.approved を requirements.approved より先に判定する理由:
// 通常は design.approved=true なら requirements.approved=true も成立しているが、
// 万一片方だけ true でも「より進んだ承認」を優先することで、不要な再生成を避ける。
//
// phase フィールドは判定に使わない（approvals が真理）。phase は読み物として残し、
// 判定の二系統化（phase と approvals の両方を見る複雑さ）を避ける。
func determineStartStep(specJSONPath string) int {
	raw, err := os.ReadFile(specJSONPath)
	if err != nil {
		return 1
	}
	var meta struct {
		Approvals struct {
			Requirements struct {
				Approved bool `json:"approved"`
			} `json:"requirements"`
			Design struct {
				Approved bool `json:"approved"`
			} `json:"design"`
		} `json:"approvals"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return 1
	}
	if meta.Approvals.Design.Approved {
		return 6
	}
	if meta.Approvals.Requirements.Approved {
		return 4
	}
	return 1
}

// setApprovalApproved は spec.json の approvals.<key>.approved を true に設定する。
// key は "requirements" / "design" / "tasks" を想定するが、検証はしない（呼び出し側責務）。
//
// 既に true なら書き込みをスキップする idempotent な挙動。spec.json 不在 / パース失敗 /
// 書き込み不能はエラーを返すが、呼び出し側 (runKiroSpecLoop) は best-effort 扱いで握りつぶす設計。
//
// フィールド構造を保持するため map[string]any で読み込み → 該当フィールドだけ書き換え →
// MarshalIndent で書き戻す方式を採る。spec.json は人が編集する小さい JSON で、フィールド順や
// 未知キーの保持は必須要件ではないため十分（既存 kiro-spec-* skill も全体書き換え方式）。
//
// updated_at は他 skill が更新する慣習に合わせ、書き換え発生時のみ ISO8601 (RFC3339) で更新する。
func setApprovalApproved(specJSONPath string, key string) error {
	raw, err := os.ReadFile(specJSONPath)
	if err != nil {
		return fmt.Errorf("setApprovalApproved: read %s: %w", specJSONPath, err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		return fmt.Errorf("setApprovalApproved: parse %s: %w", specJSONPath, err)
	}

	approvalsAny, _ := spec["approvals"].(map[string]any)
	if approvalsAny == nil {
		approvalsAny = map[string]any{}
		spec["approvals"] = approvalsAny
	}
	entryAny, _ := approvalsAny[key].(map[string]any)
	if entryAny == nil {
		entryAny = map[string]any{}
		approvalsAny[key] = entryAny
	}
	if approved, ok := entryAny["approved"].(bool); ok && approved {
		// 既に true なら書き込みスキップ（idempotent）
		return nil
	}
	entryAny["approved"] = true
	spec["updated_at"] = time.Now().UTC().Format(time.RFC3339)

	out, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("setApprovalApproved: marshal %s: %w", specJSONPath, err)
	}
	// 末尾改行を付ける（既存 spec.json の慣習に合わせる）。
	out = append(out, '\n')
	if err := os.WriteFile(specJSONPath, out, 0o644); err != nil {
		return fmt.Errorf("setApprovalApproved: write %s: %w", specJSONPath, err)
	}
	return nil
}

// markSpecReady は kiro-spec-loop の最終段 (step 6 / spec-tasks) 完走時に呼び、
// spec.json を「実装着手可能」状態に進める:
//   - phase: "tasks-approved"
//   - ready_for_implementation: true
//
// 両フィールドが既に目的状態なら書き込みをスキップする idempotent 動作。
// spec.json 不在 / パース失敗 / 書き込み不能はエラーを返すが、呼び出し側
// (applyApprovalForPhase) は best-effort 扱いで握りつぶす設計
// (setApprovalApproved と同じポリシー)。
//
// 設計判断: setApprovalApproved の責務 (approvals フィールドのみ) を保つため、
// phase / ready_for_implementation 更新は本ヘルパに分離する。spec-tasks 完走時に
// applyApprovalForPhase から両者を続けて呼ぶ。
//
// updated_at は変更発生時のみ ISO8601 (RFC3339) で更新する
// (setApprovalApproved と同じ慣習に揃える)。
func markSpecReady(specJSONPath string) error {
	raw, err := os.ReadFile(specJSONPath)
	if err != nil {
		return fmt.Errorf("markSpecReady: read %s: %w", specJSONPath, err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		return fmt.Errorf("markSpecReady: parse %s: %w", specJSONPath, err)
	}

	changed := false
	if v, _ := spec["phase"].(string); v != "tasks-approved" {
		spec["phase"] = "tasks-approved"
		changed = true
	}
	if v, _ := spec["ready_for_implementation"].(bool); !v {
		spec["ready_for_implementation"] = true
		changed = true
	}
	if !changed {
		return nil
	}
	spec["updated_at"] = time.Now().UTC().Format(time.RFC3339)

	out, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("markSpecReady: marshal %s: %w", specJSONPath, err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(specJSONPath, out, 0o644); err != nil {
		return fmt.Errorf("markSpecReady: write %s: %w", specJSONPath, err)
	}
	return nil
}

// stepDir は runDir 配下の <NN>-<phase> サブディレクトリパスを組み立てる。
// 例: stepDir(runDir, 3, "validate-gap") -> "<runDir>/03-validate-gap"
//
// 形式は design.md「.rilll/kiro-spec-loop/<run-id>/ ファイル配置」の表に準拠する。
func stepDir(runDir string, order int, phase string) string {
	return filepath.Join(runDir, fmt.Sprintf("%02d-%s", order, phase))
}

// formatInquiryMarkdown は round-N-inquiry.md の本文を組み立てる。
//
// 形式（人間レビュー用に Markdown に整形）:
//
//	# Inquiry <NN>-<phase> round <N>
//
//	- order: <N>
//	- phase: <phase>
//	- round: <N>
//
//	## Title
//	...
//	## Background
//	...
//	## Question
//	...
//	## Options
//	- **<label>**: <description>
//	- <label>           # description が空の場合
//
// Title / Background / Options は空なら該当セクション自体を出さない（冗長な空セクションを避ける）。
// Question は inquiryloop 上 required なので常に出す。
func formatInquiryMarkdown(order int, phase string, round int, inq inquiryloop.Inquiry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Inquiry %02d-%s round %d\n\n", order, phase, round)
	fmt.Fprintf(&b, "- order: %d\n", order)
	fmt.Fprintf(&b, "- phase: %s\n", phase)
	fmt.Fprintf(&b, "- round: %d\n\n", round)

	if strings.TrimSpace(inq.Title) != "" {
		fmt.Fprintf(&b, "## Title\n\n%s\n\n", inq.Title)
	}
	if strings.TrimSpace(inq.Background) != "" {
		fmt.Fprintf(&b, "## Background\n\n%s\n\n", inq.Background)
	}
	fmt.Fprintf(&b, "## Question\n\n%s\n", inq.Question)
	if len(inq.Options) > 0 {
		b.WriteString("\n## Options\n\n")
		for _, opt := range inq.Options {
			if strings.TrimSpace(opt.Description) != "" {
				fmt.Fprintf(&b, "- **%s**: %s\n", opt.Label, opt.Description)
			} else {
				fmt.Fprintf(&b, "- %s\n", opt.Label)
			}
		}
	}
	return b.String()
}

// formatAnswerMarkdown は round-N-answer.md の本文を組み立てる。
//
// 形式:
//
//	# Answer <NN>-<phase> round <N>
//
//	- order: <N>
//	- phase: <phase>
//	- round: <N>
//	- persona: <personaName>
//
//	## Answer
//	<answerText 改行込みでそのまま>
//
// answerText は inquiryloop が回答エージェント応答の summary フィールドを採用したものを渡される。
// 改行コードや Markdown 記法はそのまま保存し、後で人がレビューしやすくする。
func formatAnswerMarkdown(order int, phase string, round int, personaName, answerText string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Answer %02d-%s round %d\n\n", order, phase, round)
	fmt.Fprintf(&b, "- order: %d\n", order)
	fmt.Fprintf(&b, "- phase: %s\n", phase)
	fmt.Fprintf(&b, "- round: %d\n", round)
	fmt.Fprintf(&b, "- persona: %s\n\n", personaName)
	fmt.Fprintf(&b, "## Answer\n\n%s\n", answerText)
	return b.String()
}

// =============================================================================
// Fix Phase (最終修正フェーズ)
// =============================================================================
//
// fix phase は engine.RunPrompt が completed で round が 1 件以上回った段の直後に発動し、
// session A を resume したまま「ここまでのラウンドで決まった方針で実ファイルを修正してください」
// 旨の追加プロンプトを 1 回投げる。kiro skill の allowed-tools フロントマターは初回呼び出し時に
// 強く効くが、resume = session A での会話継続では Edit / Write が通る挙動が観測されている。
//
// best-effort 設計: agent error / success=false / schema 違反 / has_inquiry=true 返却のいずれも
// /git-commit を中断せず継続する。round-N-answer.md に方針が残っているため手動リカバリ可能で、
// fix phase は「うまくいけば自動反映される」ボーナス層と位置付ける。
//
// 永続化: <NN>-<phase>/fix-result.md にプロンプト本文と応答 summary（または失敗詳細）を残す。
// result.json の steps[].fix にも同等情報を表面化する（RunResultStepFix）。

// fixPhaseEditPromptHeader は Turn 1 (fix-edit; OutputFormat=nil で実ファイル修正を許可) の
// プロンプト先頭固定文字列。
//
// 本文の冒頭に置くことで、テストの classifyAgentCall や運用時のログ分類で
// 「fix phase の編集ターン」を識別できるマーカ役を兼ねる。
// 本文の意味自体（人間向けに「この続きで実ファイルを修正してください」と読める）も保つ。
//
// 2 ターン化の背景:
//   - 旧: 1 ターンで Edit/Write 直後に json_schema 強制 → stream_idle_timeout を踏みやすい
//   - 新: Turn 1 で Edit/Write をテキストモードで完了させ、Turn 2 で薄い json_schema で要約を抽出
//
// ファイル編集と構造化された判定を別ターンに分ける。
const fixPhaseEditPromptHeader = "[fix-phase-edit]\nここまでのラウンドで設計判断 / 修正方針が確定しました。"

// fixPhaseEditPromptTemplate は Turn 1 (fix-edit) で投げる固定プロンプト本文。
// %s に feature 名を埋める。json_schema 強制ではなく、要約は markdown の "## Summary" 節で受ける。
const fixPhaseEditPromptTemplate = fixPhaseEditPromptHeader + `
これらの方針に基づいて、対応する .kiro/specs/%s/ 配下のファイル
(requirements.md / design.md / research.md / spec.json など、当該フェーズが所有するもの)
を実際に編集して反映してください。

修正対象が無い・既に整合している場合は編集を行わず、その旨を summary に記してください。

完了したら、最後に以下のセクションを応答末尾に付けてください (json_schema 形式は要求しません):

## Summary
- 修正したファイル: <相対パスのリスト、なければ「なし」>
- 反映内容の要約 (または「変更なし」とその理由) を簡潔に書く

このターンは Edit / Write を許可した「作業フェーズ」です。
構造化出力 (json_schema) は次のターンで別途要求するため、ここでは markdown の本文として書いてください。`

// fixPhaseJudgePromptHeader は Turn 2 (fix-judge; resume + json_schema 強制で要約抽出) の
// プロンプト先頭固定文字列。
//
// classifyAgentCall は本マーカで Turn 2 を Turn 1 と区別し、テスト側で 2 ターン構成を縛る。
// 本マーカを変更すると既存テストの kind 判定が壊れるため、定数として固定する。
const fixPhaseJudgePromptHeader = "[fix-phase-judge]\n直前ターンで実施した修正内容を要約として返してください。"

// fixPhaseJudgePromptTemplate は Turn 2 (fix-judge) で投げる固定プロンプト本文。
// %s には feature 名を埋める。Edit / Write はせず、Turn 1 の作業内容を summary に詰めるだけの責務。
const fixPhaseJudgePromptTemplate = fixPhaseJudgePromptHeader + `
このターンは「判定フェーズ」です。新たな編集 (Edit / Write) は行わないでください。
直前ターンで実施した修正内容を、以下の json_schema 形式の単一オブジェクトで返してください:

{
  "has_inquiry": false,
  "summary": "修正したファイルと反映内容の要約 (または「変更なし」と理由)"
}

feature: %s

判断は変更せず、構造化出力に詰め替えるだけのターンです。`

// buildFixEditPrompt は Turn 1 (fix-edit) の prompt 本文を組み立てる純関数。
// feature 名を fixPhaseEditPromptTemplate に埋め込んで返す。
func buildFixEditPrompt(feature string) string {
	return fmt.Sprintf(fixPhaseEditPromptTemplate, feature)
}

// buildFixJudgePrompt は Turn 2 (fix-judge) の prompt 本文を組み立てる純関数。
// feature 名を fixPhaseJudgePromptTemplate に埋め込んで返す。
func buildFixJudgePrompt(feature string) string {
	return fmt.Sprintf(fixPhaseJudgePromptTemplate, feature)
}

// formatFixResultMarkdown は fix-result.md の本文を組み立てる。
//
// 2 ターン構成（fix-edit → fix-judge）に対応した形式:
//
//	# Fix Result <NN>-<phase>
//
//	- order: <N>
//	- phase: <phase>
//	- persona: <personaName>
//	- sessionId: <最終 sessionID>
//	- editSession: <Turn 1 sessionID>   (省略可)
//	- judgeSession: <Turn 2 sessionID>  (省略可)
//	- ok: <true|false>
//
//	## Edit Phase Prompt
//	<Turn 1 prompt>
//
//	## Judge Phase Prompt
//	<Turn 2 prompt>     (Turn 2 をスキップした場合は省略)
//
//	## Result
//	<summary or message>
//
// 失敗時 (OK=false) は Result に Message を載せ、成功時は Summary を載せる。
// editPrompt / judgePrompt は呼び出し側 (runFixPhase) が空文字列を渡せばそのセクションを省略できる。
func formatFixResultMarkdown(order int, phase, personaName, editPrompt, judgePrompt string, fix RunResultStepFix) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Fix Result %02d-%s\n\n", order, phase)
	fmt.Fprintf(&b, "- order: %d\n", order)
	fmt.Fprintf(&b, "- phase: %s\n", phase)
	fmt.Fprintf(&b, "- persona: %s\n", personaName)
	fmt.Fprintf(&b, "- sessionId: %s\n", fix.SessionID)
	if fix.EditSession != "" {
		fmt.Fprintf(&b, "- editSession: %s\n", fix.EditSession)
	}
	if fix.JudgeSession != "" {
		fmt.Fprintf(&b, "- judgeSession: %s\n", fix.JudgeSession)
	}
	fmt.Fprintf(&b, "- ok: %t\n\n", fix.OK)
	if editPrompt != "" {
		fmt.Fprintf(&b, "## Edit Phase Prompt\n\n%s\n\n", editPrompt)
	}
	if judgePrompt != "" {
		fmt.Fprintf(&b, "## Judge Phase Prompt\n\n%s\n\n", judgePrompt)
	}
	if fix.OK {
		fmt.Fprintf(&b, "## Result\n\n%s\n", fix.Summary)
	} else {
		fmt.Fprintf(&b, "## Result\n\n%s\n", fix.Message)
	}
	return b.String()
}

// persistFixResult は fix phase の結果を <NN>-<phase>/fix-result.md として書き出す。
//
// best-effort: 永続化失敗 (mkdir / write) は呼び出し側に伝播させず error を返すのみで、
// 上位 (runFixPhase) は無視する設計。
//
// editPrompt / judgePrompt は省略可（Turn 2 をスキップした場合は judgePrompt="" で渡す）。
func persistFixResult(runDir string, order int, phase, personaName, editPrompt, judgePrompt string, fix RunResultStepFix) error {
	dir := stepDir(runDir, order, phase)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("persistFixResult: mkdir %s: %w", dir, err)
	}
	out := filepath.Join(dir, "fix-result.md")
	body := formatFixResultMarkdown(order, phase, personaName, editPrompt, judgePrompt, fix)
	if err := os.WriteFile(out, []byte(body), 0o644); err != nil {
		return fmt.Errorf("persistFixResult: write %s: %w", out, err)
	}
	return nil
}

// runFixPhase は engine.RunPrompt 完走 (rounds > 0) 直後に呼ばれ、session A を resume して
// 「ここまでの方針で修正してください」プロンプトを 2 ターンに分けて投げる。
//
// 編集と判定を分けた 2 ターン構成:
//
//   - Turn 1 (fix-edit): OutputFormat=nil で呼び出し、Edit / Write を許可した自由記述ターン。
//     プロンプト末尾の "## Summary" 節に、修正したファイルと反映内容の要約を markdown で書かせる。
//     stream_idle_timeout の主因 (Edit 直後の json_schema 強制生成) を回避する。
//
//   - Turn 2 (fix-judge): Turn 1 の sessionId を resume し、json_schema を強制した
//     「要約抽出だけの薄いターン」。新たな編集は行わせない。Turn 1 の Summary 節を
//     構造化出力に詰め替える責務に絞る。
//
// best-effort:
//   - Turn 1 失敗 (agent err / success=false) → OK=false / Turn 2 はスキップ。
//   - Turn 2 失敗 (agent err / success=false / schema 違反 / has_inquiry=true) → OK=false。
//     ただし Turn 1 が成功していれば、Turn 1 応答の Content (markdown) を fix.Summary に救出する。
//
// 永続化: 本関数の中で persistFixResult まで実行する（呼び出し側の責務分担を最小化）。
// 永続化エラーはログ的扱いで RunResultStepFix.Message に追記しない（fix phase 自体の成否と区別する）。
func runFixPhase(
	ctx context.Context,
	agent workflowctx.AgentCaller,
	cwd, feature, runDir string,
	step promptStep,
	lastSession, personaName, promptLabel string,
	onEvent func(workflow.WorkflowEvent),
) RunResultStepFix {
	editPrompt := buildFixEditPrompt(feature)
	judgePrompt := ""
	fix := RunResultStepFix{Ran: true}

	// =========================================================================
	// Turn 1: fix-edit (OutputFormat=nil; Edit/Write を許可した作業ターン)
	// =========================================================================
	editOptions := &workflowctx.CallAgentOptions{
		// OutputFormat は意図的に nil。json_schema を強制しないことで Edit/Write 直後の
		// 構造化生成由来の stream_idle_timeout を回避する。
		OutputFormat: nil,
		StepID:       fmt.Sprintf("fix-%02d-%s-edit", step.Order, step.Phase),
		StepName:     promptLabel + "-fix-edit",
	}
	editResp, editErr := agent.CallAgent(ctx, editPrompt, cwd, workflow.ForwardEvents(onEvent), lastSession, editOptions)
	switch {
	case editErr != nil:
		fix.OK = false
		fix.Message = fmt.Sprintf("fix phase agent error: %v", editErr)
		// Turn 2 はスキップ (judgePrompt は "" のまま、JudgeSession も空)。
		_ = persistFixResult(runDir, step.Order, step.Phase, personaName, editPrompt, judgePrompt, fix)
		return fix
	case !editResp.Success:
		fix.OK = false
		fix.SessionID = editResp.SessionID
		fix.EditSession = editResp.SessionID
		fix.Message = "fix phase edit turn returned success=false"
		_ = persistFixResult(runDir, step.Order, step.Phase, personaName, editPrompt, judgePrompt, fix)
		return fix
	}
	// Turn 1 成功
	fix.EditSession = editResp.SessionID
	fix.SessionID = editResp.SessionID // Turn 2 で上書きされる予定だが、Turn 2 失敗時のフォールバック値として先に入れる
	editContent := editResp.Content    // Turn 2 が失敗したときに summary として救出する候補

	// =========================================================================
	// Turn 2: fix-judge (resume + json_schema 強制で要約だけ抽出)
	// =========================================================================
	judgePrompt = buildFixJudgePrompt(feature)
	judgeOptions := &workflowctx.CallAgentOptions{
		OutputFormat: inquiryloop.Schema(),
		StepID:       fmt.Sprintf("fix-%02d-%s-judge", step.Order, step.Phase),
		StepName:     promptLabel + "-fix-judge",
		// Turn 2 = 判定ターン: Edit / Write を禁じて Read/Glob/Grep のみに絞る。
		// Turn 1 で済んだ実ファイル編集を再度行わせず、構造化出力への詰め替えだけを期待する。
		AllowedTools: fixJudgeAllowedTools,
	}
	judgeResp, judgeErr := agent.CallAgent(ctx, judgePrompt, cwd, workflow.ForwardEvents(onEvent), editResp.SessionID, judgeOptions)
	switch {
	case judgeErr != nil:
		fix.OK = false
		fix.Message = fmt.Sprintf("fix phase judge agent error: %v", judgeErr)
		// Turn 1 は成功していたので、Content を summary として救出する。
		fix.Summary = editContent
	case !judgeResp.Success:
		fix.OK = false
		fix.JudgeSession = judgeResp.SessionID
		fix.SessionID = judgeResp.SessionID
		fix.Message = "fix phase judge turn returned success=false"
		fix.Summary = editContent
	default:
		fix.JudgeSession = judgeResp.SessionID
		fix.SessionID = judgeResp.SessionID
		_, hasInquiry, summary, parseErr := inquiryloop.ParseResponse(judgeResp)
		switch {
		case parseErr != nil:
			fix.OK = false
			fix.Message = fmt.Sprintf("fix phase judge schema violation: %v", parseErr)
			// Turn 1 の markdown summary を救出
			fix.Summary = editContent
		case hasInquiry:
			fix.OK = false
			fix.Summary = summary
			fix.Message = "fix phase judge returned has_inquiry=true (judge ターンが構造化応答を破った)"
		default:
			fix.OK = true
			fix.Summary = summary
		}
	}

	// 永続化は best-effort（失敗しても fix phase の成否判定には影響させない）。
	_ = persistFixResult(runDir, step.Order, step.Phase, personaName, editPrompt, judgePrompt, fix)
	return fix
}

// =============================================================================
// Orchestration: KiroSpecLoop workflow.Definition + steps[] + workflowctx.AgentCaller adapter
// =============================================================================
//
// 本セクションは tasks.md 4.1 の責務範囲（rilll ワークフロー定義のスキャフォールド）を所有する。
//
//   - KiroSpecLoop(agent): rilll ワークフロー登録用の workflow.Definition を返す。
//     現段階では workflow.Definition.Fn は「即時 nil 返却 + Done event 発火」のスタブ。
//     6 段の orchestration 本体（feature 選択 / inquiryloop 駆動 / per-step commit / per-run audit）は
//     tasks.md 4.2 で本ファイルに追加する。
//   - promptStep / steps[]: 6 段固定順の kiro プロンプト定義。Order と Phase を共有契約として固定する。
//   - referenceFiles: brief.md など長文 Markdown を skill コマンド本文に「パス参照」として
//     末尾追記するためのフォーマッタ。本文は埋め込まずパスのみを示す方針（design.md 設計ポリシー）。
//   - agentCallerAdapter: workflowctx.AgentCaller を inquiryloop.AgentCaller に橋渡しする
//     最小限のアダプタ。両者の Call シグネチャの差分（workflowctx.CallAgentOptions 構造体 vs 個別引数）を吸収する。

// promptStep は kiro プロンプト 1 段の組み立て情報を表す内部型。
//
// Command は (feature 名, skill に参照させたいマークダウン群の絶対パス) を受け取り、
// 末尾の参照セクション（"以下のマークダウンファイルを参照してください: ...".）を付加するか
// 否かは各 step の責務とする。Step 1 (spec-init) のみ markdownPaths を反映する。
type promptStep struct {
	// Order は 1〜6 の段番号。result.json / step ディレクトリ名の組立に使う。
	Order int
	// Phase は "spec-init" / "spec-requirements" / ... のフェーズ識別子。
	// ファイルパス（<NN>-<phase>/）と result.json.steps[].phase で同一値を共有する契約。
	Phase string
	// Persona は当該フェーズの session A (kiro skill 初回呼び出し) に注入する
	// Persona facet 名（例: "spec-analyst" / "spec-architect" / "validator"）。
	// kiro-spec-loop-facets spec (Req 2.1, 2.4) で導入。
	//
	// 契約:
	//   - 6 entry すべて非空必須（空文字列は配線漏れと見なされ、ErrFacetNotFound 経路に
	//     合流して RunReasonMissingPhasePersona で abort される defensive 分岐の入力となる）。
	//   - 値は `internal/facet/facets/personas/<Persona>.md` を embed 経路 / project override 経路で
	//     解決する識別子であり、`facet.ResolveFacet(cwd, facet.Persona, step.Persona)` に渡す。
	Persona string
	// Command は (feature, markdownPaths) からスラッシュコマンド本文を組み立てる関数。
	// markdownPaths が unused でも、契約として 2 引数を要求して将来拡張に備える
	// （他段でもマークダウン参照を加えたい場合に契約変更不要となる）。
	Command func(feature string, markdownPaths []string) string
	// MaxRounds は inquiryloop.Engine.MaxRounds に渡す問い合わせ⇄回答ラウンド上限。
	// 0 のときは engine 既定（defaultMaxRounds=5）が適用される。
	// validate-* 系は 15 を指定して、メタ質問が多くてもラウンドが収束する余裕を確保する。
	MaxRounds int
	// RequiresVerdict は session A skill 応答に verdict=go/no_go を必須化し、
	// no_go のとき次段に進めず workflow を停止する gate を有効にする。
	RequiresVerdict bool
}

// referenceFiles は brief.md など参照させたいマークダウン群の絶対パスを
// 「以下のマークダウンファイルを参照してください: ...」固定セクションとして整形する。
//
// 設計ポリシー（design.md「マークダウン参照の扱い」契約に従う）:
//   - brief.md など長文の本文を $ARGUMENTS に流すと runner-directive とのパース境界が
//     曖昧になるため、**絶対パスを末尾に列挙して明示参照させる**運用にする。
//   - paths が空 / nil の場合は空文字列を返す。これにより呼び出し側で
//     「常に末尾に referenceFiles を結合する」シンプルな契約で済む（条件分岐が不要）。
//
// 形式:
//
//	"\n\n以下のマークダウンファイルを参照してください:\n- <path1>\n- <path2>\n"
//
// 先頭の "\n\n" は直前のコマンド本文との「空行 1 行区切り」を担保するためのもの。
// 後続にさらに runner-directive を結合する場合も、directive 側が先頭 "\n\n" を持つため
// 区切りが二重に挟まる形になる（design.md「directive とマークダウン参照行は空行 1 行で区切る」契約）。
func referenceFiles(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n以下のマークダウンファイルを参照してください:\n")
	for _, p := range paths {
		b.WriteString("- " + p + "\n")
	}
	return b.String()
}

// steps は kiro-spec-loop が直列実行する 6 段の kiro プロンプト定義。
//
// 順序固定の理由（要件 2.1 / design.md「6 段固定順序」）:
//
//	spec-init → spec-requirements → validate-gap → spec-design → validate-design → spec-tasks
//
// この順序は kiro skill 本体の依存関係（後段が前段の生成物を読む）と一致しており、
// 入れ替えや並列化は本仕様の対象外。
//
// Step 1 (spec-init) のみ markdownPaths を反映するのは、`/kiro-spec-init` skill が
// `.kiro/specs/<feature>/brief.md` を直接読む契約に乗るための措置。他段では feature 名のみを
// 引数として渡し、既存ファイル群は skill 側が読みに行く既存仕様に従う。
//
// 将来「他段でも特定マークダウンを skill に明示参照させたい」要件が出た場合は、
// 該当 step の Command 関数で markdownPaths を受け取り referenceFiles で末尾追記するだけで
// 対応できる（promptStep の契約を変更する必要はない）。
// Persona マッピング (Design §Decisions 2 / §Components steps 表):
//
//	spec-init        / spec-requirements → "spec-analyst"   (要件分析: WHAT に絞った EARS 形式記述)
//	spec-design      / spec-tasks        → "spec-architect" (技術設計: WHAT → HOW 翻訳, 境界 / 依存)
//	validate-gap     / validate-design   → "validator"      (adversarial 査読: 覆い忘れ / 境界跨ぎ)
//
// 6 entry すべて非空必須。空文字列は配線漏れと見なされ、runInquiryLoop 側で
// ErrFacetNotFound 経路に合流して RunReasonMissingPhasePersona で abort する
// defensive 分岐の入力となる (kiro-spec-loop-facets Req 2.1, 2.3, 2.4)。
var steps = []promptStep{
	{Order: 1, Phase: "spec-init", Persona: "spec-analyst", Command: func(feature string, mds []string) string {
		return "/kiro-spec-init " + feature + referenceFiles(mds)
	}},
	{Order: 2, Phase: "spec-requirements", Persona: "spec-analyst", Command: func(feature string, _ []string) string {
		return "/kiro-spec-requirements " + feature
	}},
	{Order: 3, Phase: "validate-gap", Persona: "validator", Command: func(feature string, _ []string) string {
		return "/kiro-validate-gap " + feature
	}, MaxRounds: 15},
	{Order: 4, Phase: "spec-design", Persona: "spec-architect", Command: func(feature string, _ []string) string {
		return "/kiro-spec-design " + feature
	}},
	{Order: 5, Phase: "validate-design", Persona: "validator", Command: func(feature string, _ []string) string {
		return "/kiro-validate-design " + feature
	}, MaxRounds: 15, RequiresVerdict: true},
	{Order: 6, Phase: "spec-tasks", Persona: "spec-architect", Command: func(feature string, _ []string) string {
		return "/kiro-spec-tasks " + feature
	}},
}

// agentCallerAdapter は workflowctx.AgentCaller を inquiryloop.AgentCaller に橋渡しする
// 薄いアダプタ。両 interface の差分は次の 2 点であり、本アダプタはその吸収に徹する:
//
//  1. 引数受け渡し方式の差: workflowctx.AgentCaller は workflowctx.CallAgentOptions 構造体で
//     OutputFormat / StepID / StepName を渡す。inquiryloop.AgentCaller は同情報を
//     個別引数 (outputFormat, stepID, stepName) で渡す。
//  2. 命名差: workflow 側は CallAgent、inquiryloop 側は Call。
//
// onEvent フィールドは optional（4.2 で追加）。inquiryloop.Engine.RunPrompt は
// agent.Call(..., onEvent=nil, ...) で固定的に呼ぶ設計のため、TUI への text /
// tool_use イベント転送（要件 2.2）を満たすには adapter 側で workflow 層の onEvent
// を保持し、Call の onEvent 引数が nil のときに forwardEvents で代理する必要がある。
// onEvent 引数が non-nil の場合（呼び出し側が直接 forwarder を渡したケース）は
// その引数を優先して inner に転送し、4.1 で確立した契約を破壊しない。
// 前段 phase の summary を skill 起動 prompt の **後ろ**に連結する責務は呼び出し側
// (`spec-loop` 側: runInquiryLoop / `impl-loop` 側: runKiroImplLoop) が `previousResponseBlock(prev)`
// を skill コマンドの直後に付加する形で果たす。slash command を prompt 先頭に保つ
// (Claude Code の skill 起動は先頭トークンが `/<skill>` でないと走らない仕様への対応)
// ため、foundation 側 `IncludePreviousResponse` の **前置 (prepend)** 経路は本 adapter
// 経由では使わない。本 adapter は素の forwarder としての責務に徹し、Previous Response
// 関連の field / 分岐は持たない。
type agentCallerAdapter struct {
	inner   workflowctx.AgentCaller
	onEvent func(workflow.WorkflowEvent)
}

// Call は inquiryloop.AgentCaller を満たすための実装で、引数列を workflowctx.CallAgentOptions に詰め直して
// inner.CallAgent を呼ぶ。ctx / prompt / cwd / resumeSessionID は素のまま橋渡しする。
//
// onEvent 引数の解決ルール:
//   - 引数 onEvent が non-nil → そのまま inner に転送（4.1 既定動作）
//   - 引数 onEvent が nil かつ a.onEvent が non-nil → workflow.ForwardEvents(a.onEvent) を inner に渡す
//   - 双方 nil → nil を inner に渡す（イベント転送なし）
//
// outputFormat が nil の場合でも workflowctx.CallAgentOptions を必ず生成する。これは inquiryloop.Engine が
// stepID / stepName を毎回設定する契約のため、options 自体を nil にすると StepID / StepName が
// 失われてしまう（akashic 計測の粒度が落ちる）ことを防ぐため。
//
// AllowedTools は本 adapter 経由の呼び出し (= inquiryloop.Engine 駆動の skill / answer / repair) では
// すべて nil を渡し、SDK 既定の全ツール許可を維持する。fix-judge ターン等で
// allowedTools を制限したい呼び出しは adapter を経由せず agent.CallAgent を直接呼ぶ
// (runFixPhase 参照)。これにより adapter 側に stepID 分岐ロジックを持たない
// シンプルな責務分担を維持する。
func (a agentCallerAdapter) Call(
	ctx context.Context,
	prompt string,
	cwd string,
	onEvent provider.AgentEventCallback,
	resumeSessionID string,
	outputFormat *provider.OutputFormat,
	stepID string,
	stepName string,
) (provider.AgentResponse, error) {
	forwarded := onEvent
	if forwarded == nil && a.onEvent != nil {
		forwarded = workflow.ForwardEvents(a.onEvent)
	}
	options := &workflowctx.CallAgentOptions{
		OutputFormat: outputFormat,
		StepID:       stepID,
		StepName:     stepName,
	}
	// 前段 phase / 前段 task の summary は呼び出し側 (runInquiryLoop / runKiroImplLoop) が
	// `previousResponseBlock(prev)` を prompt 本文に直接連結する責務を負う (slash-first 順
	// 維持のため、foundation `IncludePreviousResponse` の前置経路は本 adapter からは使わない)。
	// 本 adapter は素の forwarder としての責務に徹する。
	return a.inner.CallAgent(ctx, prompt, cwd, forwarded, resumeSessionID, options)
}

// fixJudgeAllowedTools は fix-judge ターン (Turn 2) で許可するツールのホワイトリスト。
//
// 判定に必要な読み取りツールだけを許可する。
// fix-judge は「Turn 1 で実施した修正の要約だけを構造化出力に詰め直す」薄い責務であり、
// Edit / Write が動くと stream_idle_timeout / 二重編集の温床になるため明示的に禁じる。
//
// 変数として外出ししているのは、テストから期待値を参照しやすくするため。
var fixJudgeAllowedTools = []string{"Read", "Glob", "Grep"}

// KiroSpecLoop は rilll のワークフロー定義として「Kiro 仕様作成ループ」を返す。
//
// 本関数は外形シグネチャだけを所有し、workflow.Definition.Fn の本体は
// runKiroSpecLoop に委譲する。
func KiroSpecLoop(agent workflowctx.AgentCaller) workflow.Definition {
	return workflow.Definition{
		ID:    "kiro-spec-loop",
		Label: "Kiro 仕様作成ループ",
		Args: []workflow.ArgSpec{
			{Key: "feature", Description: ".kiro/specs/<feature>/ 配下の対象 feature 名（brief.md 必須・phase < tasks-generated）", Required: true},
		},
		Recorder: workflow.RecorderFromAgent(agent),
		Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			return runKiroSpecLoop(ctx, cwd, onEvent, ask, agent)
		},
	}
}

// =============================================================================
// run() orchestration entrypoint (tasks.md 4.2)
// =============================================================================
//
// runKiroSpecLoop は KiroSpecLoop workflow.Definition.Fn の本体実装エントリ。
// 実装本体は specLoopRunner（spec_loop_runner.go）に分離されており、本関数は
// runner を組み立てて Run() を呼ぶだけの薄いアダプタである。
//
// session B Persona は workflow 層の facet adapter で解決する。ask が nil の場合は
// 「対象 feature 選択不能」の構成不備として missing-feature 同等で扱う
// （TUI なしの起動経路はサポートしない）。
//
// 失敗ポリシー:
//   - 想定済み abort 理由（missing-feature / cancelled-by-user / missing-persona /
//     prompt-failure / schema-violation / loop-limit / commit-failure / completed）は
//     err=nil で return し、result.json に terminationReason を確定して残す（rollback なし、要件 9.4）。
//   - I/O 致命的エラー（result.json 書き出し失敗、context 書き出し失敗、ask 自身のエラー等）は
//     error 戻り値で propagate し、workflow.Definition.Run の error フィールド（akashic）に乗せる。
func runKiroSpecLoop(
	ctx context.Context,
	cwd string,
	onEvent func(workflow.WorkflowEvent),
	ask workflow.WorkflowAsk,
	agent workflowctx.AgentCaller,
) error {
	return newSpecLoopRunner(ctx, cwd, onEvent, ask, agent).Run()
}

// buildRunResult は runState から RunResult（result.json 公開構造）を組み立てる純関数。
//
// 契約:
//   - completedAt が zero value なら CompletedAt を nil のままにする（abort パス用途）
//   - abortStep が nil なら AbortStep / AbortMessage はセットしない（completed パス用途）
//   - state.StepResults は内部表現で abort 時には途中まで、completed 時には 6 件、と長さが変動する
//
// stepResult / RunResultStep の変換は単純なフィールド対応だが、内部型と公開型を分けることで
// 内部レイアウト変更（例: state.StepResults に診断用フィールド追加）を JSON 永続フォーマットから
// 切り離せる。
func buildRunResult(
	state *runState,
	completedAt time.Time,
	abortStep *RunResultAbortStep,
	abortMessage string,
) RunResult {
	rr := RunResult{
		RunID:             state.RunID,
		WorkflowID:        "kiro-spec-loop",
		Feature:           state.Feature,
		PersonaName:       state.PersonaName,
		PersonaPath:       state.PersonaPath,
		StartedAt:         state.StartedAt,
		TerminationReason: state.Termination,
	}
	if !completedAt.IsZero() {
		ca := completedAt
		rr.CompletedAt = &ca
	}
	if abortStep != nil {
		rr.AbortStep = abortStep
		rr.AbortMessage = abortMessage
	}
	// engine 由来 abort では state.Failure が事前にセットされており、ここで result.json に伝搬する。
	// 文字列値は engine.FailureDetail からそのまま採用するため、result.json.failure.reason の値領域は
	// engine.TerminationReason 4 値（loop-limit / prompt-failure / schema-violation）に限定される。
	if state.Failure != nil {
		rr.Failure = state.Failure
	}
	rr.Steps = make([]RunResultStep, 0, len(state.StepResults))
	for _, sr := range state.StepResults {
		attempt := sr.Attempt
		if attempt == 0 {
			attempt = 1
		}
		rr.Steps = append(rr.Steps, RunResultStep{
			Order:        sr.Order,
			Phase:        sr.Phase,
			Rounds:       sr.Rounds,
			LastSession:  sr.LastSession,
			Summary:      sr.Summary,
			Verdict:      string(sr.Verdict),
			CommitOK:     sr.CommitOK,
			CommitOutput: sr.CommitOutput,
			Fix:          sr.Fix,
			Attempt:      attempt,
		})
	}
	if len(state.Advisories) > 0 {
		rr.Advisories = append([]RunResultAdvisory(nil), state.Advisories...)
	}
	if len(state.ValidationFeedback) > 0 {
		rr.ValidationFeedback = append([]RunResultValidationFeedback(nil), state.ValidationFeedback...)
	}
	return rr
}
