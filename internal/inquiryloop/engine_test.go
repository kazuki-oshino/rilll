package inquiryloop

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

// =============================================================================
// fakeAgent: Engine.RunPrompt のシナリオテスト用ヘルパ。
//
// 役割:
//   - kiro skill 呼び出し (session A) と回答生成エージェント呼び出し (session B) を
//     別系統のスクリプトで返し分け、各系統で順番にレスポンスを払い出す。
//   - すべての Call 入力 (prompt, resumeSessionID, stepID, stepName, outputFormat)
//     を recordedCall として記録し、テスト側から「呼び出し順序 / resume 連鎖 /
//     directive 含有 / 回答エージェントの fresh session」などを検証可能にする。
//
// skill / answer の振り分けは Engine.RunPrompt 実装の以下の規約に依存する:
//   - skill 呼び出し: stepName = in.PromptLabel             / in.PromptLabel + "-repair"
//   - answer 呼び出し: stepName = in.PromptLabel + "-answer" / in.PromptLabel + "-answer-repair"
// この差分から fakeAgent.Call 内で系統を判別する。
// =============================================================================

type recordedCall struct {
	Prompt          string
	CWD             string
	ResumeSessionID string
	OutputFormat    *provider.OutputFormat
	StepID          string
	StepName        string
}

type fakeAgent struct {
	// skillResponses は kiro skill 系統で順番に払い出すレスポンス列。
	skillResponses []provider.AgentResponse
	// skillErrors[i] が非 nil なら、skill 系統 i 回目の呼び出しで err を返す。
	// nil の要素は「エラーなし」を意味し、対応する skillResponses[i] が払い出される。
	skillErrors []error

	// answerResponses は回答エージェント系統で順番に払い出すレスポンス列。
	answerResponses []provider.AgentResponse
	// answerErrors[i] が非 nil なら、answer 系統 i 回目の呼び出しで err を返す。
	answerErrors []error

	skillIdx  int
	answerIdx int

	// calls には skill / answer 区別なくすべての Call が時系列順に積まれる。
	calls []recordedCall
}

func (f *fakeAgent) Call(
	_ context.Context,
	prompt string,
	cwd string,
	_ provider.AgentEventCallback,
	resumeSessionID string,
	outputFormat *provider.OutputFormat,
	stepID string,
	stepName string,
) (provider.AgentResponse, error) {
	f.calls = append(f.calls, recordedCall{
		Prompt:          prompt,
		CWD:             cwd,
		ResumeSessionID: resumeSessionID,
		OutputFormat:    outputFormat,
		StepID:          stepID,
		StepName:        stepName,
	})
	// answer 系統: 末尾が "-answer" (通常呼び出し) または "-answer-repair" (schema-violation 後の
	// 同 session repair 呼び出し)。後者を skill 側に取りこぼさないよう両方を明示する。
	if strings.HasSuffix(stepName, "-answer") || strings.HasSuffix(stepName, "-answer-repair") {
		return f.popAnswer()
	}
	return f.popSkill()
}

func (f *fakeAgent) popSkill() (provider.AgentResponse, error) {
	idx := f.skillIdx
	f.skillIdx++
	if idx < len(f.skillErrors) && f.skillErrors[idx] != nil {
		return provider.AgentResponse{}, f.skillErrors[idx]
	}
	if idx >= len(f.skillResponses) {
		return provider.AgentResponse{}, errors.New("fakeAgent: skill script exhausted")
	}
	return f.skillResponses[idx], nil
}

func (f *fakeAgent) popAnswer() (provider.AgentResponse, error) {
	idx := f.answerIdx
	f.answerIdx++
	if idx < len(f.answerErrors) && f.answerErrors[idx] != nil {
		return provider.AgentResponse{}, f.answerErrors[idx]
	}
	if idx >= len(f.answerResponses) {
		return provider.AgentResponse{}, errors.New("fakeAgent: answer script exhausted")
	}
	return f.answerResponses[idx], nil
}

// =============================================================================
// テスト用ヘルパ: 共通の Engine / RunInput / StructuredOutput を組み立てる。
// =============================================================================

func newTestPersona() AnswerPersona {
	return AnswerPersona{
		Name:       "test-persona",
		SourcePath: "embed:facets/personas/test.md",
		Content:    "persona body for engine tests",
	}
}

func newTestEngine() Engine {
	return Engine{
		Persona:   newTestPersona(),
		MaxRounds: 5,
	}
}

func newRunInput() RunInput {
	return RunInput{
		PromptCommand: "/kiro-spec-init demo",
		PromptLabel:   "01-spec-init",
	}
}

// inquiryStructured は has_inquiry=true の skill 応答 StructuredOutput を組み立てる。
func inquiryStructured(question string) map[string]any {
	return map[string]any{
		"has_inquiry": true,
		"summary":     "needs decision",
		"inquiry": map[string]any{
			"title":      "T",
			"background": "B",
			"question":   question,
			"options":    []any{},
		},
	}
}

// completionStructured は has_inquiry=false の skill / answer 応答 StructuredOutput を組み立てる。
// 回答エージェント応答も同じ構造を取り、summary フィールドが回答テキストとして採用される。
func completionStructured(summary string) map[string]any {
	return map[string]any{
		"has_inquiry": false,
		"summary":     summary,
	}
}

func schemaRequires(out *provider.OutputFormat, name string) bool {
	if out == nil {
		return false
	}
	root, ok := out.Schema.(map[string]any)
	if !ok {
		return false
	}
	requiredAny, ok := root["required"].([]any)
	if !ok {
		return false
	}
	for _, v := range requiredAny {
		if s, ok := v.(string); ok && s == name {
			return true
		}
	}
	return false
}

// =============================================================================
// parseResponse 直叩き 3 ケース (tasks.md 2.3 a/b/c)
// =============================================================================

// (a) StructuredOutput={"has_inquiry":false,"summary":"ok"} → hasInquiry=false, summary="ok"
func TestEngineParseResponseCompletedShape(t *testing.T) {
	resp := provider.AgentResponse{
		StructuredOutput: map[string]any{
			"has_inquiry": false,
			"summary":     "ok",
		},
	}
	inq, has, summary, err := ParseResponse(resp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if has {
		t.Fatalf("hasInquiry=true, want false")
	}
	if summary != "ok" {
		t.Fatalf("summary=%q, want %q", summary, "ok")
	}
	// Inquiry は []InquiryOption を持つため "==" 比較できない。
	// 個別フィールドで「has_inquiry=false 時は zero-value のままであること」を検証する。
	if inq.Title != "" || inq.Background != "" || inq.Question != "" || len(inq.Options) != 0 {
		t.Fatalf("inquiry should be zero-value when has_inquiry=false: %+v", inq)
	}
}

// (b) has_inquiry=true だが inquiry が nil → ErrSchemaViolation
func TestEngineParseResponseInquiryNilWithFlagTrue(t *testing.T) {
	resp := provider.AgentResponse{
		StructuredOutput: map[string]any{
			"has_inquiry": true,
			"summary":     "needs",
			"inquiry":     nil,
		},
	}
	_, _, _, err := ParseResponse(resp)
	if err == nil {
		t.Fatalf("expected ErrSchemaViolation, got nil")
	}
	if !errors.Is(err, ErrSchemaViolation) {
		t.Fatalf("expected ErrSchemaViolation chain, got %v", err)
	}
}

// (c) StructuredOutput=nil かつ Content に JSON 抽出不能 → ErrSchemaViolation
func TestEngineParseResponseStructuredAndContentMissing(t *testing.T) {
	resp := provider.AgentResponse{
		StructuredOutput: nil,
		// 中括弧を含まないので extractJSONFromContent の全 3 経路で失敗する
		Content: "this is plain text without any json blob",
	}
	_, _, _, err := ParseResponse(resp)
	if err == nil {
		t.Fatalf("expected ErrSchemaViolation, got nil")
	}
	if !errors.Is(err, ErrSchemaViolation) {
		t.Fatalf("expected ErrSchemaViolation chain, got %v", err)
	}
}

// =============================================================================
// Engine シナリオ 7 件 (tasks.md 2.3 i〜vii)
// =============================================================================

// (i) 初回 has_inquiry=false で 0 ラウンド完走。
//
// Task 2.2 のスモーク契約をフラットな fakeAgent で再表現する。
func TestEngineRunPromptZeroRoundCompletes(t *testing.T) {
	fa := &fakeAgent{
		skillResponses: []provider.AgentResponse{
			{
				Success:          true,
				SessionID:        "s1",
				StructuredOutput: completionStructured("ok"),
			},
		},
	}
	eng := newTestEngine()

	final, err := eng.RunPrompt(context.Background(), fa, "/tmp", newRunInput(), nil)
	if err != nil {
		t.Fatalf("RunPrompt returned unexpected error: %v", err)
	}
	if final.Status != StatusCompleted {
		t.Fatalf("Status=%q, want %q", final.Status, StatusCompleted)
	}
	if final.Termination != ReasonCompleted {
		t.Fatalf("Termination=%q, want %q", final.Termination, ReasonCompleted)
	}
	if final.Rounds != 0 {
		t.Fatalf("Rounds=%d, want 0", final.Rounds)
	}
	if final.Summary != "ok" {
		t.Fatalf("Summary=%q, want %q", final.Summary, "ok")
	}
	if final.Failure != nil {
		t.Fatalf("Failure should be nil for completed case, got %+v", final.Failure)
	}
	// 0 ラウンド完走 = skill 1 回のみ呼ばれ answer は呼ばれない
	if len(fa.calls) != 1 {
		t.Fatalf("expected 1 call (skill only), got %d: %+v", len(fa.calls), fa.calls)
	}
	if fa.calls[0].ResumeSessionID != "" {
		t.Fatalf("first skill call should have empty resume, got %q", fa.calls[0].ResumeSessionID)
	}
}

func TestEngineRunPromptCapturesValidationVerdict(t *testing.T) {
	fa := &fakeAgent{
		skillResponses: []provider.AgentResponse{
			{
				Success:   true,
				SessionID: "s1",
				StructuredOutput: map[string]any{
					"has_inquiry": false,
					"summary":     "design has critical issue",
					"verdict":     "no_go",
				},
			},
		},
	}
	eng := newTestEngine()
	eng.SkillSchema = ValidationSchema()

	final, err := eng.RunPrompt(context.Background(), fa, "/tmp", newRunInput(), nil)
	if err != nil {
		t.Fatalf("RunPrompt returned unexpected error: %v", err)
	}
	if final.Status != StatusCompleted {
		t.Fatalf("Status=%q, want %q", final.Status, StatusCompleted)
	}
	if final.Verdict != VerdictNoGo {
		t.Fatalf("Verdict=%q, want %q", final.Verdict, VerdictNoGo)
	}
	if len(fa.calls) != 1 {
		t.Fatalf("expected 1 skill call, got %d", len(fa.calls))
	}
	if !schemaRequires(fa.calls[0].OutputFormat, "verdict") {
		t.Fatalf("skill call should use validation schema requiring verdict: %+v", fa.calls[0].OutputFormat)
	}
}

// TestEngineRunPromptPropagatesAdvisory は validate-design が verdict=go + has_advisory=true を返したとき、
// engine が FinalResult に has_advisory / advisory 本文を載せて返すことを確認する。
// workflow 層の retry ループはこの情報を使って architect に直接フィードバックする。
func TestEngineRunPromptPropagatesAdvisory(t *testing.T) {
	advisoryText := "FTS5 fallback のテスト観点を tasks 側で明確にしてください"
	fa := &fakeAgent{
		skillResponses: []provider.AgentResponse{
			{
				Success:   true,
				SessionID: "s1",
				StructuredOutput: map[string]any{
					"has_inquiry":  false,
					"summary":      "概ね整理されています",
					"verdict":      "go",
					"has_advisory": true,
					"advisory":     advisoryText,
				},
			},
		},
	}
	eng := newTestEngine()
	eng.SkillSchema = ValidationSchema()

	final, err := eng.RunPrompt(context.Background(), fa, "/tmp", newRunInput(), nil)
	if err != nil {
		t.Fatalf("RunPrompt returned unexpected error: %v", err)
	}
	if final.Verdict != VerdictGo {
		t.Fatalf("Verdict=%q, want %q", final.Verdict, VerdictGo)
	}
	if !final.HasAdvisory {
		t.Fatalf("HasAdvisory=false, want true (final=%+v)", final)
	}
	if final.Advisory != advisoryText {
		t.Fatalf("Advisory=%q, want %q", final.Advisory, advisoryText)
	}
}

func TestEngineRunPromptPreservesExtensions(t *testing.T) {
	fa := &fakeAgent{
		skillResponses: []provider.AgentResponse{
			{
				Success:   true,
				SessionID: "s1",
				StructuredOutput: map[string]any{
					"has_inquiry":         false,
					"summary":             "design has critical issue",
					"verdict":             "no_go",
					"has_advisory":        false,
					"blocker_category":    "requirements_traceability",
					"blocker_fingerprint": "missing-ac-1-1-trace",
					"affected_artifacts":  []any{"requirements.md", "design.md"},
					"recommended_action":  "revise_current_spec",
					"owner_boundary":      "current_spec",
				},
			},
		},
	}
	eng := newTestEngine()
	eng.SkillSchema = ValidationSchema()

	final, err := eng.RunPrompt(context.Background(), fa, "/tmp", newRunInput(), nil)
	if err != nil {
		t.Fatalf("RunPrompt returned unexpected error: %v", err)
	}
	if final.Status != StatusCompleted {
		t.Fatalf("Status=%q, want %q", final.Status, StatusCompleted)
	}
	if final.Verdict != VerdictNoGo {
		t.Fatalf("Verdict=%q, want %q", final.Verdict, VerdictNoGo)
	}

	extensions := requireExtensions(t, final)
	if got := extensions["blocker_category"]; got != "requirements_traceability" {
		t.Fatalf("Extensions[blocker_category]=%v, want requirements_traceability", got)
	}
	if got := extensions["blocker_fingerprint"]; got != "missing-ac-1-1-trace" {
		t.Fatalf("Extensions[blocker_fingerprint]=%v, want missing-ac-1-1-trace", got)
	}
	if got := extensions["recommended_action"]; got != "revise_current_spec" {
		t.Fatalf("Extensions[recommended_action]=%v, want revise_current_spec", got)
	}
	if got := extensions["owner_boundary"]; got != "current_spec" {
		t.Fatalf("Extensions[owner_boundary]=%v, want current_spec", got)
	}
	if _, ok := extensions["summary"]; ok {
		t.Fatalf("Extensions must not include known summary field: %v", extensions)
	}
}

// (ii) 1 ラウンドで完走 + resume 連鎖検証。
//
// 期待呼び出し順:
//  1. skill (resume="", initial directive 付き)
//  2. answer (resume="")
//  3. skill (resume="s1", directive なし)
func TestEngineOneRoundCompletesWithResumeChain(t *testing.T) {
	fa := &fakeAgent{
		skillResponses: []provider.AgentResponse{
			{Success: true, SessionID: "s1", StructuredOutput: inquiryStructured("Q1")},
			{Success: true, SessionID: "s2", StructuredOutput: completionStructured("done")},
		},
		answerResponses: []provider.AgentResponse{
			{Success: true, SessionID: "b1", StructuredOutput: completionStructured("answer-1")},
		},
	}
	eng := newTestEngine()

	final, err := eng.RunPrompt(context.Background(), fa, "/tmp", newRunInput(), nil)
	if err != nil {
		t.Fatalf("RunPrompt error: %v", err)
	}
	if final.Status != StatusCompleted {
		t.Fatalf("Status=%q, want %q (final=%+v)", final.Status, StatusCompleted, final)
	}
	if final.Rounds != 1 {
		t.Fatalf("Rounds=%d, want 1", final.Rounds)
	}
	if final.Summary != "done" {
		t.Fatalf("Summary=%q, want %q", final.Summary, "done")
	}
	if final.LastSession != "s2" {
		t.Fatalf("LastSession=%q, want %q", final.LastSession, "s2")
	}
	if len(final.Trail) != 1 {
		t.Fatalf("Trail length=%d, want 1", len(final.Trail))
	}
	if final.Trail[0].AnswerText != "answer-1" {
		t.Fatalf("Trail[0].AnswerText=%q, want %q", final.Trail[0].AnswerText, "answer-1")
	}

	// 呼び出し順序の確認
	if len(fa.calls) != 3 {
		t.Fatalf("expected 3 calls, got %d: %+v", len(fa.calls), fa.calls)
	}
	if fa.calls[0].ResumeSessionID != "" {
		t.Fatalf("call#0 (initial skill) resume=%q, want empty", fa.calls[0].ResumeSessionID)
	}
	if !strings.HasSuffix(fa.calls[1].StepName, "-answer") {
		t.Fatalf("call#1 should be answerer (StepName ends with -answer), got %q", fa.calls[1].StepName)
	}
	if fa.calls[1].ResumeSessionID != "" {
		t.Fatalf("call#1 (answerer) resume=%q, want empty", fa.calls[1].ResumeSessionID)
	}
	if fa.calls[2].ResumeSessionID != "s1" {
		t.Fatalf("call#2 (resumed skill) resume=%q, want %q", fa.calls[2].ResumeSessionID, "s1")
	}
}

// (iii) 常に has_inquiry=true → MaxRounds (5) 超過で loop-limit abort。
func TestEngineLoopLimitAbort(t *testing.T) {
	// skill: 初回 + 5 round 後の呼び出し = 6 回 (5 round 目の re-call も has_inquiry=true 想定)
	skillResps := make([]provider.AgentResponse, 0, 6)
	for range 6 {
		skillResps = append(skillResps, provider.AgentResponse{
			Success:          true,
			SessionID:        "s-loop",
			StructuredOutput: inquiryStructured("Q-still-pending"),
		})
	}
	// answer: 5 ラウンド分
	answerResps := make([]provider.AgentResponse, 0, 5)
	for range 5 {
		answerResps = append(answerResps, provider.AgentResponse{
			Success:          true,
			SessionID:        "b-loop",
			StructuredOutput: completionStructured("a"),
		})
	}
	fa := &fakeAgent{skillResponses: skillResps, answerResponses: answerResps}
	eng := newTestEngine()

	final, err := eng.RunPrompt(context.Background(), fa, "/tmp", newRunInput(), nil)
	if err != nil {
		t.Fatalf("RunPrompt error: %v", err)
	}
	if final.Status != StatusAborted {
		t.Fatalf("Status=%q, want %q", final.Status, StatusAborted)
	}
	if final.Termination != ReasonLoopLimit {
		t.Fatalf("Termination=%q, want %q", final.Termination, ReasonLoopLimit)
	}
	if final.Rounds != 5 {
		t.Fatalf("Rounds=%d, want 5", final.Rounds)
	}
	if final.Failure == nil || final.Failure.Reason != ReasonLoopLimit {
		t.Fatalf("Failure should be ReasonLoopLimit, got %+v", final.Failure)
	}
	if final.Failure.Round != 5 {
		t.Fatalf("Failure.Round=%d, want 5", final.Failure.Round)
	}
}

// (iv) parseResponse 失敗 → schema-violation abort。
//
// StructuredOutput=nil + Content も JSON 抽出不能なケースを skill 応答に流す。
// 初回 schema-violation でも schemaRepairDirective() で 1 回 retry するため、
// repair も schema-violation のケース (= 最終的に abort する経路) を再現する。
func TestEngineSchemaViolationAbort(t *testing.T) {
	fa := &fakeAgent{
		skillResponses: []provider.AgentResponse{
			{Success: true, SessionID: "s-bad", Content: "free text without json"},
			// skill-repair-r0: 同じく markdown で返し、最終的に schema-violation で abort させる
			{Success: true, SessionID: "s-bad-repair", Content: "still no json"},
		},
	}
	eng := newTestEngine()

	final, err := eng.RunPrompt(context.Background(), fa, "/tmp", newRunInput(), nil)
	if err != nil {
		t.Fatalf("RunPrompt error: %v", err)
	}
	if final.Status != StatusAborted {
		t.Fatalf("Status=%q, want %q", final.Status, StatusAborted)
	}
	if final.Termination != ReasonSchemaViolation {
		t.Fatalf("Termination=%q, want %q", final.Termination, ReasonSchemaViolation)
	}
	if final.Failure == nil || final.Failure.Reason != ReasonSchemaViolation {
		t.Fatalf("Failure should be ReasonSchemaViolation, got %+v", final.Failure)
	}
	// 診断用に Content を rawContent に残す契約 (engine.go schemaViolationResult)。
	// 最終的に violate したのは repair 応答のため、RawContent は repair 応答の Content。
	if final.Failure.RawContent != "still no json" {
		t.Fatalf("Failure.RawContent=%q, want %q", final.Failure.RawContent, "still no json")
	}
}

// (iv-init-repair-ok) 初回 skill 応答が schema-violation でも、schemaRepairDirective() で
// 同 session を resume して 1 回追プロンプトし、JSON envelope を再要求する。リペアが成功したら
// 0 ラウンドで完走できる。
//
// この対称化により、cursor-sdk のように output schema を native 強制できない provider が
// 初回呼び出しで markdown drift しても、resume 経路と同じ救済が効く。
func TestEngineInitialSchemaRepairRetrySucceeds(t *testing.T) {
	fa := &fakeAgent{
		skillResponses: []provider.AgentResponse{
			// 初回 skill: markdown 単体で schema-violation
			{Success: true, SessionID: "s-bad", Content: "markdown only without json"},
			// skill-repair-r0: 正しい JSON で完走
			{Success: true, SessionID: "s-fixed", StructuredOutput: completionStructured("done")},
		},
	}
	eng := newTestEngine()

	final, err := eng.RunPrompt(context.Background(), fa, "/tmp", newRunInput(), nil)
	if err != nil {
		t.Fatalf("RunPrompt error: %v", err)
	}
	if final.Status != StatusCompleted {
		t.Fatalf("Status=%q, want %q (final=%+v)", final.Status, StatusCompleted, final)
	}
	if final.Termination != ReasonCompleted {
		t.Fatalf("Termination=%q, want %q", final.Termination, ReasonCompleted)
	}
	if final.Rounds != 0 {
		t.Fatalf("Rounds=%d, want 0", final.Rounds)
	}
	if final.Summary != "done" {
		t.Fatalf("Summary=%q, want %q", final.Summary, "done")
	}
	if final.LastSession != "s-fixed" {
		t.Fatalf("LastSession=%q, want %q", final.LastSession, "s-fixed")
	}
	// 呼び出し列: skill(initial), skill-repair-r0 = 2 件
	if len(fa.calls) != 2 {
		t.Fatalf("expected 2 calls, got %d: %+v", len(fa.calls), fa.calls)
	}
	if fa.calls[1].StepID != "skill-repair-r0" {
		t.Fatalf("calls[1].StepID=%q, want %q", fa.calls[1].StepID, "skill-repair-r0")
	}
	if fa.calls[1].StepName != "01-spec-init-repair" {
		t.Fatalf("calls[1].StepName=%q, want %q", fa.calls[1].StepName, "01-spec-init-repair")
	}
	if fa.calls[1].ResumeSessionID != "s-bad" {
		t.Fatalf("repair should resume failed initial session id; got %q want %q", fa.calls[1].ResumeSessionID, "s-bad")
	}
	if !strings.Contains(fa.calls[1].Prompt, "[runner-repair-directive]") {
		t.Fatalf("repair prompt should include schemaRepairDirective marker; got %q", fa.calls[1].Prompt)
	}
}

// (iv-init-repair-fail) 初回 skill が schema-violation で repair も schema-violation のとき、
// ReasonSchemaViolation で abort する。リトライは 1 回限り。
func TestEngineInitialSchemaRepairRetryStillViolatesAborts(t *testing.T) {
	fa := &fakeAgent{
		skillResponses: []provider.AgentResponse{
			{Success: true, SessionID: "s-bad-1", Content: "free text without json"},
			{Success: true, SessionID: "s-bad-2", Content: "still markdown only"},
		},
	}
	eng := newTestEngine()

	final, err := eng.RunPrompt(context.Background(), fa, "/tmp", newRunInput(), nil)
	if err != nil {
		t.Fatalf("RunPrompt error: %v", err)
	}
	if final.Status != StatusAborted {
		t.Fatalf("Status=%q, want %q (final=%+v)", final.Status, StatusAborted, final)
	}
	if final.Termination != ReasonSchemaViolation {
		t.Fatalf("Termination=%q, want %q", final.Termination, ReasonSchemaViolation)
	}
	if final.Failure == nil || final.Failure.Reason != ReasonSchemaViolation {
		t.Fatalf("Failure should be ReasonSchemaViolation, got %+v", final.Failure)
	}
	if final.Failure.Round != 0 {
		t.Fatalf("Failure.Round=%d, want 0", final.Failure.Round)
	}
	if final.Failure.RawContent != "still markdown only" {
		t.Fatalf("Failure.RawContent=%q, want %q", final.Failure.RawContent, "still markdown only")
	}
	// 3 回目以降の skill 呼び出しは行わない (リトライは 1 回限り)
	repairCalls := 0
	for _, c := range fa.calls {
		if c.StepID == "skill-repair-r0" {
			repairCalls++
		}
	}
	if repairCalls != 1 {
		t.Fatalf("expected exactly 1 skill-repair-r0 call, got %d", repairCalls)
	}
}

// (iv-answer-repair-ok) answer (session B) 応答が schema-violation でも、同 session を resume して
// schemaRepairDirective() で 1 回再要求し、JSON で復旧できれば完走を継続する。
//
// answer 応答も markdown drift を起こす可能性があるため (skill 側との対称性)、
// 同じ repair retry 契約を適用する。
func TestEngineAnswerSchemaRepairRetrySucceeds(t *testing.T) {
	fa := &fakeAgent{
		skillResponses: []provider.AgentResponse{
			{Success: true, SessionID: "s1", StructuredOutput: inquiryStructured("Q1")},
			{Success: true, SessionID: "s2", StructuredOutput: completionStructured("done")},
		},
		answerResponses: []provider.AgentResponse{
			// 1 回目 answer: markdown のみで schema-violation
			{Success: true, SessionID: "b1-bad", Content: "markdown only"},
			// 2 回目 answer-repair: 正しい JSON で復旧
			{Success: true, SessionID: "b1-fixed", StructuredOutput: completionStructured("answer-1")},
		},
	}
	eng := newTestEngine()

	final, err := eng.RunPrompt(context.Background(), fa, "/tmp", newRunInput(), nil)
	if err != nil {
		t.Fatalf("RunPrompt error: %v", err)
	}
	if final.Status != StatusCompleted {
		t.Fatalf("Status=%q, want %q (final=%+v)", final.Status, StatusCompleted, final)
	}
	if final.Rounds != 1 {
		t.Fatalf("Rounds=%d, want 1", final.Rounds)
	}
	if final.Summary != "done" {
		t.Fatalf("Summary=%q, want %q", final.Summary, "done")
	}
	// 呼び出し列: skill(initial), answer(bad), answer-repair, skill(resume) = 4 件
	if len(fa.calls) != 4 {
		t.Fatalf("expected 4 calls, got %d: %+v", len(fa.calls), fa.calls)
	}
	if fa.calls[2].StepID != "answer-repair-r1" {
		t.Fatalf("calls[2].StepID=%q, want %q", fa.calls[2].StepID, "answer-repair-r1")
	}
	if fa.calls[2].StepName != "01-spec-init-answer-repair" {
		t.Fatalf("calls[2].StepName=%q, want %q", fa.calls[2].StepName, "01-spec-init-answer-repair")
	}
	if fa.calls[2].ResumeSessionID != "b1-bad" {
		t.Fatalf("answer-repair should resume failed answer session id; got %q want %q", fa.calls[2].ResumeSessionID, "b1-bad")
	}
	if !strings.Contains(fa.calls[2].Prompt, "[runner-repair-directive]") {
		t.Fatalf("answer-repair prompt should include schemaRepairDirective marker; got %q", fa.calls[2].Prompt)
	}
	// trail に乗る AnswerSession は復旧後の b1-fixed
	if len(final.Trail) != 1 {
		t.Fatalf("Trail size=%d, want 1", len(final.Trail))
	}
	if final.Trail[0].AnswerSession != "b1-fixed" {
		t.Fatalf("Trail[0].AnswerSession=%q, want %q", final.Trail[0].AnswerSession, "b1-fixed")
	}
	if final.Trail[0].AnswerText != "answer-1" {
		t.Fatalf("Trail[0].AnswerText=%q, want %q", final.Trail[0].AnswerText, "answer-1")
	}
}

// (iv-answer-repair-fail) answer も answer-repair も schema-violation の場合は
// ReasonSchemaViolation で abort する (リトライは 1 回限り)。
func TestEngineAnswerSchemaRepairRetryStillViolatesAborts(t *testing.T) {
	fa := &fakeAgent{
		skillResponses: []provider.AgentResponse{
			{Success: true, SessionID: "s1", StructuredOutput: inquiryStructured("Q1")},
		},
		answerResponses: []provider.AgentResponse{
			{Success: true, SessionID: "b1-bad-1", Content: "markdown only"},
			{Success: true, SessionID: "b1-bad-2", Content: "still markdown"},
		},
	}
	eng := newTestEngine()

	final, err := eng.RunPrompt(context.Background(), fa, "/tmp", newRunInput(), nil)
	if err != nil {
		t.Fatalf("RunPrompt error: %v", err)
	}
	if final.Status != StatusAborted {
		t.Fatalf("Status=%q, want %q (final=%+v)", final.Status, StatusAborted, final)
	}
	if final.Termination != ReasonSchemaViolation {
		t.Fatalf("Termination=%q, want %q", final.Termination, ReasonSchemaViolation)
	}
	if final.Failure == nil || final.Failure.Round != 1 {
		t.Fatalf("Failure.Round mismatch: %+v", final.Failure)
	}
	if final.Failure.RawContent != "still markdown" {
		t.Fatalf("Failure.RawContent=%q, want %q", final.Failure.RawContent, "still markdown")
	}
	// 2 回目以降の answer 呼び出しは行わない (リトライは 1 回限り)
	repairCalls := 0
	for _, c := range fa.calls {
		if c.StepID == "answer-repair-r1" {
			repairCalls++
		}
	}
	if repairCalls != 1 {
		t.Fatalf("expected exactly 1 answer-repair-r1 call, got %d", repairCalls)
	}
}

// (v) agent error → prompt-failure abort。
func TestEnginePromptFailureOnAgentError(t *testing.T) {
	fa := &fakeAgent{
		skillErrors: []error{errors.New("provider exploded")},
	}
	eng := newTestEngine()

	final, err := eng.RunPrompt(context.Background(), fa, "/tmp", newRunInput(), nil)
	if err != nil {
		t.Fatalf("RunPrompt should not surface error directly; got %v", err)
	}
	if final.Status != StatusAborted {
		t.Fatalf("Status=%q, want %q", final.Status, StatusAborted)
	}
	if final.Termination != ReasonPromptFailure {
		t.Fatalf("Termination=%q, want %q", final.Termination, ReasonPromptFailure)
	}
	if final.Failure == nil {
		t.Fatalf("Failure should be non-nil")
	}
	if !strings.Contains(final.Failure.Message, "provider exploded") {
		t.Fatalf("Failure.Message=%q, want it to mention agent error", final.Failure.Message)
	}
}

// (vi) runnerDirective() は全エージェント呼び出し (初回 skill / resume skill / answer) に含まれる。
//
// 当初は skill 側初回のみ付加だったが、long-context resume 後にエージェントが drift して
// markdown を生で返し schema-violation で abort する事例が観測されたため、
// resume にも毎ラウンド再付加する仕様に更新された (prompt.go の runnerDirectiveText 注釈を参照)。
//
// さらに answer 側も毎ラウンド fresh session で呼ばれるため、回答エージェントが drift して
// 再質問形 / markdown 単体で返す事故を予防する目的で directive を末尾に付加する仕様に統一した
// (session A 側との対称化)。
func TestEngineRunnerDirectiveIncludedOnAllCalls(t *testing.T) {
	fa := &fakeAgent{
		skillResponses: []provider.AgentResponse{
			{Success: true, SessionID: "s1", StructuredOutput: inquiryStructured("Q")},
			{Success: true, SessionID: "s2", StructuredOutput: completionStructured("done")},
		},
		answerResponses: []provider.AgentResponse{
			{Success: true, SessionID: "b1", StructuredOutput: completionStructured("ans")},
		},
	}
	eng := newTestEngine()

	if _, err := eng.RunPrompt(context.Background(), fa, "/tmp", newRunInput(), nil); err != nil {
		t.Fatalf("RunPrompt error: %v", err)
	}

	if len(fa.calls) != 3 {
		t.Fatalf("expected 3 calls (skill, answer, skill), got %d", len(fa.calls))
	}
	directive := runnerDirective()
	if !strings.Contains(fa.calls[0].Prompt, directive) {
		t.Fatalf("first skill call should include runnerDirective; got prompt=%q", fa.calls[0].Prompt)
	}
	if !strings.Contains(fa.calls[1].Prompt, directive) {
		t.Fatalf("answer call should include runnerDirective; got prompt=%q", fa.calls[1].Prompt)
	}
	if !strings.Contains(fa.calls[2].Prompt, directive) {
		t.Fatalf("resumed skill call should include runnerDirective; got prompt=%q", fa.calls[2].Prompt)
	}
}

// (vi-b) Engine.InquiryDirective が非空のとき、初回 skill 呼び出しと resume skill 呼び出しの
// 両方の prompt に directive 文字列が含まれ、answer 呼び出しには含まれないことを検証する。
//
// 設計契約:
//   - directive は workflow 固有の問い合わせポリシー (例: scope 拡張禁止) を inquiry-skill 側
//     (session A) に毎ラウンド再注入するための seam
//   - long-context resume 後の drift 対策として initial / resume 両方に再付加する
//   - answer 側 (session B) は別のレイヤ (AnswerContext + answerPolicySection + persona) で
//     ガードしているため、本 directive は付加しない
func TestEngineInquiryDirectiveIncludedOnSkillCallsOnly(t *testing.T) {
	const marker = "[loop-policy-marker-7E2A]"
	fa := &fakeAgent{
		skillResponses: []provider.AgentResponse{
			{Success: true, SessionID: "s1", StructuredOutput: inquiryStructured("Q")},
			{Success: true, SessionID: "s2", StructuredOutput: completionStructured("done")},
		},
		answerResponses: []provider.AgentResponse{
			{Success: true, SessionID: "b1", StructuredOutput: completionStructured("ans")},
		},
	}
	eng := newTestEngine()
	eng.InquiryDirective = marker

	if _, err := eng.RunPrompt(context.Background(), fa, "/tmp", newRunInput(), nil); err != nil {
		t.Fatalf("RunPrompt error: %v", err)
	}

	if len(fa.calls) != 3 {
		t.Fatalf("expected 3 calls (skill, answer, skill), got %d", len(fa.calls))
	}
	if !strings.Contains(fa.calls[0].Prompt, marker) {
		t.Errorf("first skill call should include InquiryDirective marker; got prompt=%q", fa.calls[0].Prompt)
	}
	if strings.Contains(fa.calls[1].Prompt, marker) {
		t.Errorf("answer call must NOT include InquiryDirective marker; got prompt=%q", fa.calls[1].Prompt)
	}
	if !strings.Contains(fa.calls[2].Prompt, marker) {
		t.Errorf("resumed skill call should include InquiryDirective marker; got prompt=%q", fa.calls[2].Prompt)
	}
}

// (vi-c) Engine.InquiryDirective が空のときは、どの呼び出しの prompt にも
// 余計なポリシー文字列が乗らない (= 従来挙動互換)。
func TestEngineInquiryDirectiveEmptyKeepsLegacyPrompts(t *testing.T) {
	fa := &fakeAgent{
		skillResponses: []provider.AgentResponse{
			{Success: true, SessionID: "s1", StructuredOutput: inquiryStructured("Q")},
			{Success: true, SessionID: "s2", StructuredOutput: completionStructured("done")},
		},
		answerResponses: []provider.AgentResponse{
			{Success: true, SessionID: "b1", StructuredOutput: completionStructured("ans")},
		},
	}
	eng := newTestEngine() // InquiryDirective を設定しない (zero value = 空)

	if _, err := eng.RunPrompt(context.Background(), fa, "/tmp", newRunInput(), nil); err != nil {
		t.Fatalf("RunPrompt error: %v", err)
	}

	for i, c := range fa.calls {
		if strings.Contains(c.Prompt, "[loop-policy") {
			t.Errorf("call[%d] (kind=%s) should not include any loop-policy marker when InquiryDirective is empty; got prompt=%q",
				i, c.StepName, c.Prompt)
		}
	}
}

// (vii) 回答エージェント呼び出しが毎回 resumeSessionID=="" であること。
//
// 2 ラウンド回し、skill 連鎖は (resume="") → (resume="sA") → (resume="sB")、
// answer 呼び出しは常に resume="" であることを検証する。
func TestEngineAnswererAlwaysFreshSession(t *testing.T) {
	fa := &fakeAgent{
		skillResponses: []provider.AgentResponse{
			{Success: true, SessionID: "sA", StructuredOutput: inquiryStructured("Q1")},
			{Success: true, SessionID: "sB", StructuredOutput: inquiryStructured("Q2")},
			{Success: true, SessionID: "sC", StructuredOutput: completionStructured("done")},
		},
		answerResponses: []provider.AgentResponse{
			{Success: true, SessionID: "bA", StructuredOutput: completionStructured("a1")},
			{Success: true, SessionID: "bB", StructuredOutput: completionStructured("a2")},
		},
	}
	eng := newTestEngine()
	final, err := eng.RunPrompt(context.Background(), fa, "/tmp", newRunInput(), nil)
	if err != nil {
		t.Fatalf("RunPrompt error: %v", err)
	}
	if final.Rounds != 2 {
		t.Fatalf("Rounds=%d, want 2", final.Rounds)
	}

	// answer 呼び出しすべての resume が空であること
	answerSeen := 0
	for i, c := range fa.calls {
		if !strings.HasSuffix(c.StepName, "-answer") {
			continue
		}
		answerSeen++
		if c.ResumeSessionID != "" {
			t.Fatalf("answer call (calls[%d]) should have empty resume; got %q", i, c.ResumeSessionID)
		}
	}
	if answerSeen != 2 {
		t.Fatalf("expected 2 answerer calls, saw %d (calls=%+v)", answerSeen, fa.calls)
	}

	// skill 呼び出しの resume チェーンが ["", "sA", "sB"] であること
	expectedResumes := []string{"", "sA", "sB"}
	skillSeen := 0
	for i, c := range fa.calls {
		if strings.HasSuffix(c.StepName, "-answer") {
			continue
		}
		if skillSeen >= len(expectedResumes) {
			t.Fatalf("more skill calls than expected at calls[%d]: %+v", i, fa.calls)
		}
		if c.ResumeSessionID != expectedResumes[skillSeen] {
			t.Fatalf("skill call #%d (calls[%d]) resume=%q, want %q",
				skillSeen, i, c.ResumeSessionID, expectedResumes[skillSeen])
		}
		skillSeen++
	}
	if skillSeen != 3 {
		t.Fatalf("expected 3 skill calls, saw %d", skillSeen)
	}
}

// (viii) resume 応答が schema-violation の場合、schemaRepairDirective() で 1 回追プロンプトして
// JSON envelope を再要求する。リペアが成功したらラウンドを完走できる。
//
// シナリオ:
//  1. skill (initial)               : has_inquiry=true
//  2. answer (round 1)              : completion summary "answer-1"
//  3. skill (resume r1)             : Content="markdown only" / StructuredOutput=nil  ← schema-violation
//  4. skill (skill-repair-r1, resume): completion StructuredOutput → 完走
func TestEngineSchemaRepairRetrySucceeds(t *testing.T) {
	fa := &fakeAgent{
		skillResponses: []provider.AgentResponse{
			{Success: true, SessionID: "s1", StructuredOutput: inquiryStructured("Q1")},
			// 2 つ目の skill 応答 = resume r1 の応答。schema-violation を仕込む。
			{Success: true, SessionID: "s2-bad", Content: "markdown only without json"},
			// 3 つ目 = skill-repair-r1。正しい JSON で完走させる。
			{Success: true, SessionID: "s2-fixed", StructuredOutput: completionStructured("done")},
		},
		answerResponses: []provider.AgentResponse{
			{Success: true, SessionID: "b1", StructuredOutput: completionStructured("answer-1")},
		},
	}
	eng := newTestEngine()

	final, err := eng.RunPrompt(context.Background(), fa, "/tmp", newRunInput(), nil)
	if err != nil {
		t.Fatalf("RunPrompt error: %v", err)
	}
	if final.Status != StatusCompleted {
		t.Fatalf("Status=%q, want %q (final=%+v)", final.Status, StatusCompleted, final)
	}
	if final.Termination != ReasonCompleted {
		t.Fatalf("Termination=%q, want %q", final.Termination, ReasonCompleted)
	}
	if final.Rounds != 1 {
		t.Fatalf("Rounds=%d, want 1", final.Rounds)
	}
	if final.Summary != "done" {
		t.Fatalf("Summary=%q, want %q", final.Summary, "done")
	}
	if final.LastSession != "s2-fixed" {
		t.Fatalf("LastSession=%q, want %q", final.LastSession, "s2-fixed")
	}

	// 呼び出し列: skill(initial), answer, skill(resume), skill-repair = 4 件
	if len(fa.calls) != 4 {
		t.Fatalf("expected 4 calls, got %d: %+v", len(fa.calls), fa.calls)
	}
	if fa.calls[3].StepID != "skill-repair-r1" {
		t.Fatalf("calls[3].StepID=%q, want %q", fa.calls[3].StepID, "skill-repair-r1")
	}
	if fa.calls[3].StepName != "01-spec-init-repair" {
		t.Fatalf("calls[3].StepName=%q, want %q", fa.calls[3].StepName, "01-spec-init-repair")
	}
	// repair 呼び出しは failed resume 応答の SessionID を resume する
	if fa.calls[3].ResumeSessionID != "s2-bad" {
		t.Fatalf("repair call should resume failed session id; got %q want %q", fa.calls[3].ResumeSessionID, "s2-bad")
	}
	if !strings.Contains(fa.calls[3].Prompt, "[runner-repair-directive]") {
		t.Fatalf("repair call prompt should include schemaRepairDirective marker; got %q", fa.calls[3].Prompt)
	}
}

// (x) AnswerSourcePathFunc が供給されたとき、resume 呼び出しの prompt 先頭に
// "Source: <path>" 行が付与されることを確認する。
//
// engine.AnswerSourcePathFunc が resume 時に回答ファイルのパスを供給する。
// answer エージェント呼び出しと skill (initial) 呼び出しの prompt には Source 行が出現しないこと、
// resume 呼び出し (round 1) の prompt にだけ供給されることを縛る。
func TestEngineResumeIncludesSourcePathFromFunc(t *testing.T) {
	fa := &fakeAgent{
		skillResponses: []provider.AgentResponse{
			{Success: true, SessionID: "s1", StructuredOutput: inquiryStructured("Q1")},
			{Success: true, SessionID: "s2", StructuredOutput: completionStructured("done")},
		},
		answerResponses: []provider.AgentResponse{
			{Success: true, SessionID: "b1", StructuredOutput: completionStructured("answer-1")},
		},
	}
	eng := newTestEngine()
	const sourcePath = "/abs/audit/round-1-answer.md"
	eng.AnswerSourcePathFunc = func(round int) string {
		if round != 1 {
			t.Fatalf("AnswerSourcePathFunc called with round=%d, want 1", round)
		}
		return sourcePath
	}

	if _, err := eng.RunPrompt(context.Background(), fa, "/tmp", newRunInput(), nil); err != nil {
		t.Fatalf("RunPrompt error: %v", err)
	}

	if len(fa.calls) != 3 {
		t.Fatalf("expected 3 calls (skill, answer, resumed-skill), got %d", len(fa.calls))
	}
	// initial skill / answer 側には Source 行を出してはいけない
	if strings.Contains(fa.calls[0].Prompt, "Source: ") {
		t.Fatalf("initial skill prompt must NOT contain Source line; got prompt=%q", fa.calls[0].Prompt)
	}
	if strings.Contains(fa.calls[1].Prompt, "Source: ") {
		t.Fatalf("answer call prompt must NOT contain Source line; got prompt=%q", fa.calls[1].Prompt)
	}
	// resume 側には Source 行が必ず先頭に乗る
	if !strings.Contains(fa.calls[2].Prompt, "Source: "+sourcePath) {
		t.Fatalf("resumed skill prompt must include Source path %q; got prompt=%q", sourcePath, fa.calls[2].Prompt)
	}
}

// (xi) AnswerSourcePathFunc=nil (= 従来挙動) では resume 呼び出しの prompt に
// "Source: " 行が含まれないことを確認する。後方互換ガード。
func TestEngineResumeNoSourcePathWhenFuncNil(t *testing.T) {
	fa := &fakeAgent{
		skillResponses: []provider.AgentResponse{
			{Success: true, SessionID: "s1", StructuredOutput: inquiryStructured("Q1")},
			{Success: true, SessionID: "s2", StructuredOutput: completionStructured("done")},
		},
		answerResponses: []provider.AgentResponse{
			{Success: true, SessionID: "b1", StructuredOutput: completionStructured("answer-1")},
		},
	}
	eng := newTestEngine()
	// AnswerSourcePathFunc は意図的に未設定

	if _, err := eng.RunPrompt(context.Background(), fa, "/tmp", newRunInput(), nil); err != nil {
		t.Fatalf("RunPrompt error: %v", err)
	}
	if len(fa.calls) < 3 {
		t.Fatalf("expected at least 3 calls, got %d", len(fa.calls))
	}
	if strings.Contains(fa.calls[2].Prompt, "Source: ") {
		t.Fatalf("resumed skill prompt must NOT contain Source line when func is nil; got prompt=%q", fa.calls[2].Prompt)
	}
}

// (xii) AnswerSourcePathFunc が空文字列を返すケース (path 不明時のフォールバック) でも
// "Source: " 行は出力されない。
func TestEngineResumeNoSourceWhenFuncReturnsEmpty(t *testing.T) {
	fa := &fakeAgent{
		skillResponses: []provider.AgentResponse{
			{Success: true, SessionID: "s1", StructuredOutput: inquiryStructured("Q1")},
			{Success: true, SessionID: "s2", StructuredOutput: completionStructured("done")},
		},
		answerResponses: []provider.AgentResponse{
			{Success: true, SessionID: "b1", StructuredOutput: completionStructured("answer-1")},
		},
	}
	eng := newTestEngine()
	eng.AnswerSourcePathFunc = func(round int) string { return "" }

	if _, err := eng.RunPrompt(context.Background(), fa, "/tmp", newRunInput(), nil); err != nil {
		t.Fatalf("RunPrompt error: %v", err)
	}
	if strings.Contains(fa.calls[2].Prompt, "Source: ") {
		t.Fatalf("resumed skill prompt must NOT contain Source line when func returns empty; got prompt=%q", fa.calls[2].Prompt)
	}
}

// (ix) resume schema-violation → repair retry でも schema-violation の場合は
// 従来通り ReasonSchemaViolation で abort する (リトライは 1 回限り)。
func TestEngineSchemaRepairRetryStillViolatesAborts(t *testing.T) {
	fa := &fakeAgent{
		skillResponses: []provider.AgentResponse{
			{Success: true, SessionID: "s1", StructuredOutput: inquiryStructured("Q1")},
			// 1 回目 resume: schema-violation
			{Success: true, SessionID: "s2-bad-1", Content: "markdown only"},
			// 2 回目 (skill-repair): また schema-violation
			{Success: true, SessionID: "s2-bad-2", Content: "still markdown only"},
		},
		answerResponses: []provider.AgentResponse{
			{Success: true, SessionID: "b1", StructuredOutput: completionStructured("answer-1")},
		},
	}
	eng := newTestEngine()

	final, err := eng.RunPrompt(context.Background(), fa, "/tmp", newRunInput(), nil)
	if err != nil {
		t.Fatalf("RunPrompt error: %v", err)
	}
	if final.Status != StatusAborted {
		t.Fatalf("Status=%q, want %q (final=%+v)", final.Status, StatusAborted, final)
	}
	if final.Termination != ReasonSchemaViolation {
		t.Fatalf("Termination=%q, want %q", final.Termination, ReasonSchemaViolation)
	}
	if final.Failure == nil || final.Failure.Reason != ReasonSchemaViolation {
		t.Fatalf("Failure should be ReasonSchemaViolation, got %+v", final.Failure)
	}
	if final.Failure.Round != 1 {
		t.Fatalf("Failure.Round=%d, want 1", final.Failure.Round)
	}
	// 診断用 RawContent には repair 応答の生本文が残るはず (最終的に violate したのは repair 応答)
	if final.Failure.RawContent != "still markdown only" {
		t.Fatalf("Failure.RawContent=%q, want %q", final.Failure.RawContent, "still markdown only")
	}
	// 3 回目以降の skill 呼び出しは行わない (リトライは 1 回限り)
	repairCalls := 0
	for _, c := range fa.calls {
		if c.StepID == "skill-repair-r1" {
			repairCalls++
		}
	}
	if repairCalls != 1 {
		t.Fatalf("expected exactly 1 skill-repair-r1 call, got %d", repairCalls)
	}
}
