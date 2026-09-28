// Package workflow の本テストファイルは kiro-spec-loop ワークフローの
// run() オーケストレーション本体（タスク 4.2 で実装する workflow.Definition.Fn の本体）に対する
// smoke テストを集約する。
//
// 4.2 段階での Observable は「6.1 / 6.2 の統合テストで 8 シナリオが想定通り動作する」
// ことだが、4.2 の TDD として最低限「各 abort 即時 return 分岐」と「最小成功パス」を
// 駆動するテストを置く。網羅版（success path の 12 件呼び出し順序の完全一致 / 7 abort
// シナリオ）は 6.1 / 6.2 で別途追加されるため、本ファイルではそれと衝突しないよう
// テスト名を Run_* で揃える。
package kiro

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/facet"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// fakeAsk は workflow.WorkflowAsk のスタブ。
//
// テストでは feature 選択ダイアログの応答を 3 状態で制御したい:
//   - 通常選択 (Selected != "" / Cancelled = false)
//   - ESC キャンセル (Cancelled = true)
//   - 選択処理エラー (Err != nil) ※今回は使わないが将来拡張余地として残す
type fakeAsk struct {
	Selected   string
	Cancelled  bool
	Err        error
	LastPrompt string
	LastItems  []workflow.SelectItem
}

func (f *fakeAsk) Select(_ context.Context, params workflow.SelectParams) (string, bool, error) {
	f.LastPrompt = params.Prompt
	f.LastItems = params.Items
	if f.Err != nil {
		return "", false, f.Err
	}
	if f.Cancelled {
		return "", false, nil
	}
	return f.Selected, true, nil
}

// recordedAgent は workflow.AgentCaller を満たす simple recorder。
//
// 系統別 (skill / commit) に固定応答を返し、Calls にすべての呼び出しを時系列で記録する。
// 6.1 / 6.2 の本格テストではより細かい Script を持つ fakeAgent が書かれる予定だが、
// 4.2 の smoke では「skill か commit かの 2 系統で十分」なため最小実装に留める。
type recordedAgent struct {
	SkillResponse  provider.AgentResponse
	SkillErr       error
	CommitResponse provider.AgentResponse
	CommitErr      error
	Calls          []recordedAgentCall
}

type recordedAgentCall struct {
	Prompt          string
	CWD             string
	ResumeSessionID string
	Options         *workflowctx.CallAgentOptions
	OnEvent         provider.AgentEventCallback
	Kind            string // "skill" または "commit"
}

func (r *recordedAgent) CallAgent(
	_ context.Context,
	prompt string,
	cwd string,
	onEvent provider.AgentEventCallback,
	resume string,
	options *workflowctx.CallAgentOptions,
) (provider.AgentResponse, error) {
	// kind 分類は scriptedAgent と同じ classifyAgentCall に揃える。
	// recordedAgent はラウンド回らない smoke 用途のため、fix / answerer kind が来ることはほぼ無いが、
	// 来た場合の fallback として SkillResponse / SkillErr を返す。
	kind := classifyAgentCall(prompt)
	call := recordedAgentCall{
		Prompt:          prompt,
		CWD:             cwd,
		ResumeSessionID: resume,
		Options:         options,
		OnEvent:         onEvent,
		Kind:            kind,
	}
	r.Calls = append(r.Calls, call)
	if kind == "commit" {
		return r.CommitResponse, r.CommitErr
	}
	resp := ensureValidationVerdictForTest(r.SkillResponse, options)
	maybeWriteSpecDesignArtifacts(false, call, resp)
	return resp, r.SkillErr
}

// collectEvents は workflow.WorkflowEvent を蓄積する callback と参照を返すテストヘルパ。
func collectEvents() (func(workflow.WorkflowEvent), *[]workflow.WorkflowEvent) {
	var events []workflow.WorkflowEvent
	return func(e workflow.WorkflowEvent) {
		events = append(events, e)
	}, &events
}

// setupFeatureCWD は <cwd>/.kiro/specs/<feature>/ に brief.md / spec.json を整える。
// brief / specJSON が空文字列のときは該当ファイルを作らない（不在を表現）。
func setupFeatureCWD(t *testing.T, feature, brief, specJSON string) string {
	t.Helper()
	cwd := t.TempDir()
	dir := filepath.Join(cwd, ".kiro", "specs", feature)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir feature dir: %v", err)
	}
	if brief != "" {
		if err := os.WriteFile(filepath.Join(dir, "brief.md"), []byte(brief), 0o644); err != nil {
			t.Fatalf("write brief: %v", err)
		}
	}
	if specJSON != "" {
		if err := os.WriteFile(filepath.Join(dir, "spec.json"), []byte(specJSON), 0o644); err != nil {
			t.Fatalf("write spec.json: %v", err)
		}
		var meta struct {
			Approvals struct {
				Design struct {
					Generated bool `json:"generated"`
				} `json:"design"`
			} `json:"approvals"`
		}
		if err := json.Unmarshal([]byte(specJSON), &meta); err == nil && meta.Approvals.Design.Generated {
			if err := os.WriteFile(filepath.Join(dir, "design.md"), []byte("# Design Document\n"), 0o644); err != nil {
				t.Fatalf("write design.md fixture: %v", err)
			}
		}
	}
	return cwd
}

// hasErrorEvent は workflow.WorkflowError 種別 event の Message に sub が含まれているかを判定する。
func hasErrorEvent(events []workflow.WorkflowEvent, sub string) bool {
	for _, e := range events {
		if e.Type == workflow.WorkflowError && strings.Contains(e.Message, sub) {
			return true
		}
	}
	return false
}

// hasDoneEvent は workflow.WorkflowDone 種別 event の Result に sub が含まれているかを判定する。
func hasDoneEvent(events []workflow.WorkflowEvent, sub string) bool {
	for _, e := range events {
		if e.Type == workflow.WorkflowDone && strings.Contains(e.Result, sub) {
			return true
		}
	}
	return false
}

// mustReadResultJSON は <cwd>/.rilll/kiro-spec-loop/<run-id>/result.json を読み出す。
// テスト 1 回につき run-id ディレクトリは 1 件のみ存在する前提。
func mustReadResultJSON(t *testing.T, cwd string) RunResult {
	t.Helper()
	runDir := filepath.Join(cwd, ".rilll", "kiro-spec-loop")
	entries, err := os.ReadDir(runDir)
	if err != nil {
		t.Fatalf("runDir not found: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("runDir に唯一の run-id ディレクトリを期待: got=%d entries=%v", len(entries), entries)
	}
	raw, err := os.ReadFile(filepath.Join(runDir, entries[0].Name(), "result.json"))
	if err != nil {
		t.Fatalf("result.json not written: %v", err)
	}
	var rr RunResult
	if err := json.Unmarshal(raw, &rr); err != nil {
		t.Fatalf("result.json must be valid JSON: %v\ncontent=%s", err, string(raw))
	}
	return rr
}

// TestKiroSpecLoopRun_MissingFeature: eligible feature が 0 件のとき
// agent 呼び出し 0 件で missing-feature 終端理由になることを検証する（要件 1.3）。
func TestKiroSpecLoopRun_MissingFeature(t *testing.T) {
	cwd := t.TempDir() // .kiro/specs/ 自体が無い → eligible 0 件
	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Selected: "shouldnt-be-asked"}
	agent := &recordedAgent{}

	if err := runKiroSpecLoop(
		context.Background(),
		cwd,
		onEvent,
		ask,
		agent,
	); err != nil {
		t.Fatalf("runKiroSpecLoop returned error: %v", err)
	}

	if len(agent.Calls) != 0 {
		t.Errorf("eligible が 0 件のとき agent.CallAgent は呼ばれてはならない: got=%d calls", len(agent.Calls))
	}
	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonMissingFeature {
		t.Errorf("terminationReason mismatch: got=%q want=%q", rr.TerminationReason, RunReasonMissingFeature)
	}
	// 案内文に /kiro-discovery または brief.md 配置を含むこと
	if !hasErrorEvent(*eventsPtr, "/kiro-discovery") && !hasErrorEvent(*eventsPtr, "brief.md") {
		t.Errorf("missing-feature event は /kiro-discovery または brief.md 配置の案内を含むべき: %+v", *eventsPtr)
	}
}

// TestKiroSpecLoopRun_MissingPersona: session B Persona facet override が空ファイルのとき、
// agent 呼び出し 0 件で missing-persona 終端理由になることを検証する（要件 5.4）。
func TestKiroSpecLoopRun_MissingPersona(t *testing.T) {
	cwd := setupFeatureCWD(t, "demo", "Brief 本文", "")
	// facet override 空ファイルで errEmptySessionBPersona を再現（embed フォールバックを抑止する）
	personaDir := filepath.Join(cwd, ".rilll-config", "facets", "personas")
	if err := os.MkdirAll(personaDir, 0o755); err != nil {
		t.Fatalf("mkdir personas: %v", err)
	}
	if err := os.WriteFile(filepath.Join(personaDir, "architecture-quality.md"), []byte(""), 0o644); err != nil {
		t.Fatalf("write empty persona override: %v", err)
	}

	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Selected: "demo"}
	agent := &recordedAgent{}

	if err := runKiroSpecLoop(
		context.Background(),
		cwd,
		onEvent,
		ask,
		agent,
	); err != nil {
		t.Fatalf("runKiroSpecLoop returned error: %v", err)
	}

	if len(agent.Calls) != 0 {
		t.Errorf("persona 解決失敗時 agent.CallAgent は呼ばれてはならない: got=%d calls", len(agent.Calls))
	}
	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonMissingPersona {
		t.Errorf("terminationReason mismatch: got=%q want=%q", rr.TerminationReason, RunReasonMissingPersona)
	}
	if !hasErrorEvent(*eventsPtr, "architecture-quality") {
		t.Errorf("missing-persona event は期待ペルソナ名 'architecture-quality' を含むべき: %+v", *eventsPtr)
	}
}

// TestKiroSpecLoopRun_Cancelled: feature 選択を ESC キャンセルしたとき、
// agent 呼び出し 0 件で cancelled-by-user 終端理由になることを検証する（要件 1.4）。
func TestKiroSpecLoopRun_Cancelled(t *testing.T) {
	cwd := setupFeatureCWD(t, "demo", "Brief 本文", "")
	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Cancelled: true}
	agent := &recordedAgent{}

	if err := runKiroSpecLoop(
		context.Background(),
		cwd,
		onEvent,
		ask,
		agent,
	); err != nil {
		t.Fatalf("runKiroSpecLoop returned error: %v", err)
	}

	if len(agent.Calls) != 0 {
		t.Errorf("キャンセル時 agent.CallAgent は呼ばれてはならない: got=%d calls", len(agent.Calls))
	}
	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonCancelledByUser {
		t.Errorf("terminationReason mismatch: got=%q want=%q", rr.TerminationReason, RunReasonCancelledByUser)
	}
	if !hasErrorEvent(*eventsPtr, "キャンセル") {
		t.Errorf("cancelled event は『キャンセル』文言を含むべき: %+v", *eventsPtr)
	}
}

// TestKiroSpecLoopRun_SmokeSuccess: 6 段すべて has_inquiry=false 即完走 +
// /git-commit 成功で完走することを検証する。
//
// 検証内容（要件 2.1 / 2.5 / 6.1 / 6.4 / 7.1 / 7.4 のコア）:
//   - agent.CallAgent が 12 件: skill, commit, skill, commit, ... の順
//   - すべての呼び出しが fresh session（resume="")
//   - Step 1 prompt のみ brief.md 絶対パスを含む
//   - result.json.terminationReason="completed"、6 件の steps[].commitOK=true
//   - workflow.WorkflowDone event が feature 名を含む
func TestKiroSpecLoopRun_SmokeSuccess(t *testing.T) {
	cwd := setupFeatureCWD(t, "demo", "Brief 本文", "")
	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Selected: "demo"}
	agent := &recordedAgent{
		SkillResponse: provider.AgentResponse{
			Success: true,
			StructuredOutput: map[string]any{
				"has_inquiry": false,
				"summary":     "ok",
			},
			SessionID: "session-skill",
		},
		CommitResponse: provider.AgentResponse{
			Success:   true,
			Content:   "commit ok",
			SessionID: "session-commit",
		},
	}

	if err := runKiroSpecLoop(
		context.Background(),
		cwd,
		onEvent,
		ask,
		agent,
	); err != nil {
		t.Fatalf("runKiroSpecLoop returned error: %v", err)
	}

	if len(agent.Calls) != 12 {
		t.Fatalf("成功パスでは 6 skill + 6 commit = 12 件の agent.CallAgent を期待: got=%d", len(agent.Calls))
	}
	for i, c := range agent.Calls {
		wantKind := "skill"
		if i%2 == 1 {
			wantKind = "commit"
		}
		if c.Kind != wantKind {
			t.Errorf("call[%d].Kind mismatch: got=%q want=%q (prompt=%q)", i, c.Kind, wantKind, c.Prompt)
		}
		if c.ResumeSessionID != "" {
			t.Errorf("call[%d] should be fresh session (resume=\"\"): got=%q", i, c.ResumeSessionID)
		}
	}
	// kiro-spec-loop-facets task 5.1 で session A skill prompt の先頭に persona prefix
	// (`# Persona\n\n<facet content>\n\n---\n\n`) が prepend されるようになった。spec-analyst
	// facet の content には「brief.md / design.md の領分」等の単語が含まれるため、単純な
	// `Contains(prompt, "brief.md")` では Step 2 の偽陽性を踏む。ここでは「マークダウン参照
	// ヘッダ」(`以下のマークダウンファイルを参照してください`) の有無で識別する。
	// これは step.Command が referenceFiles 経由でのみ生成する固定文字列であり、
	// persona facet 内容には現れない (markdownPaths を反映するのは Step 1 のみ)。
	const refHeader = "以下のマークダウンファイルを参照してください"
	// Step 1 (i=0) の prompt は brief.md 参照ヘッダを含む
	if !strings.Contains(agent.Calls[0].Prompt, refHeader) {
		t.Errorf("Step 1 prompt は brief.md 参照ヘッダを含むべき: %q", agent.Calls[0].Prompt)
	}
	if !strings.Contains(agent.Calls[0].Prompt, "brief.md") {
		t.Errorf("Step 1 prompt は brief.md パスを含むべき: %q", agent.Calls[0].Prompt)
	}
	// Step 2 (i=2) の prompt は brief.md 参照ヘッダを含まない
	if strings.Contains(agent.Calls[2].Prompt, refHeader) {
		t.Errorf("Step 2 prompt は brief.md 参照ヘッダを含むべきではない: %q", agent.Calls[2].Prompt)
	}
	// /git-commit の prompt 確認
	if !strings.HasPrefix(agent.Calls[1].Prompt, "/git-commit") {
		t.Errorf("call[1] should start with /git-commit: %q", agent.Calls[1].Prompt)
	}

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonCompleted {
		t.Errorf("terminationReason mismatch: got=%q want=%q", rr.TerminationReason, RunReasonCompleted)
	}
	if len(rr.Steps) != 6 {
		t.Errorf("6 件の step を期待: got=%d", len(rr.Steps))
	}
	for i, s := range rr.Steps {
		if !s.CommitOK {
			t.Errorf("steps[%d].CommitOK が false (phase=%q)", i, s.Phase)
		}
		if s.Rounds != 0 {
			t.Errorf("steps[%d].Rounds=0 expected (has_inquiry=false 即完走): got=%d", i, s.Rounds)
		}
		// rounds=0 で完走した段は fix phase が発動しないため Fix=nil であるべき。
		// omitempty なので JSON 上もキー自体が出ない契約。
		if s.Fix != nil {
			t.Errorf("steps[%d].Fix は rounds=0 完走では nil であるべき: got=%+v", i, s.Fix)
		}
	}
	if rr.CompletedAt == nil {
		t.Errorf("completed パスでは CompletedAt が non-nil であるべき")
	}
	if rr.AbortStep != nil {
		t.Errorf("completed パスでは AbortStep は nil であるべき: got=%+v", rr.AbortStep)
	}

	if !hasDoneEvent(*eventsPtr, "demo") {
		t.Errorf("Done event に feature 名 'demo' が含まれるべき: %+v", *eventsPtr)
	}
	if !hasDoneEvent(*eventsPtr, "6") {
		t.Errorf("Done event に完了プロンプト数 '6' が含まれるべき: %+v", *eventsPtr)
	}

	// =========================================================================
	// (8) kiro-spec-loop-facets task 6.3: end-to-end goldens を Previous Response +
	//     Persona 込み形式に厳密化する (Req 3.1 / 3.2 / 3.3 / 3.5)。
	// =========================================================================
	//
	// 本テストは runKiroSpecLoop() の run() オーケストレーション本体の end-to-end smoke
	// success path であり、task 6.1 が TestKiroSpecLoopSuccess (scriptedAgent 駆動の
	// inquiryloop 経路) に golden-lock したのと同じ観察対象を、recordedAgent 駆動の
	// 即完走 path (has_inquiry=false 12 件呼び出し) でも regression-lock する。
	//
	// 観察対象:
	//   (a) session A 初回 skill 呼び出しの recordedAgentCall.Prompt 先頭が
	//       `# Persona\n\n<facet content>\n\n---\n\n` (= personaPrefix の出力) に揃う。
	//       foundation Spec C 経路の `# Previous Response` ブロックは AgentCaller の
	//       seam (Options.IncludePreviousResponse / PreviousResponseContent) で観察する
	//       (recordedAgent は workflowctx.AgentCaller レイヤで intercept しており、
	//       internal/agent.Agent.CallAgent の prepend が走る前に prompt を観察するため。
	//       Req 5.1 の検証 seam 規定)。
	//   (b) Options.StepName が `<NN>-<phase>` 規約 (Req 3.3: akashic StepID 不変保証)
	//       Options.StepID が非空 (akashic agent_calls.step_id 記録の seam)
	//   (c) Previous Response wiring の chain:
	//         1 段目 spec-init     : IncludePreviousResponse=false / Content=""    (Req 1.2 前段なし)
	//         2-6 段目             : IncludePreviousResponse=true  / Content="ok"  (Req 1.1 前置あり)
	//         (recordedAgent.SkillResponse.StructuredOutput.summary="ok" が in-memory hit で伝播する)
	//   (d) commit prompt には persona prefix も Previous Response wiring も付かない
	//       (Design §Decisions 8: fix / commit phase の CallAgentOptions は touch しない)
	//   (e) runDir/<NN>-<phase>/result.json が 6 件揃う (Req 3.2: 永続化レイアウト不変)

	phaseOrder := []string{"spec-init", "spec-requirements", "validate-gap", "spec-design", "validate-design", "spec-tasks"}
	phaseToFacet := map[string]string{
		"spec-init":         "spec-analyst",
		"spec-requirements": "spec-analyst",
		"validate-gap":      "validator",
		"spec-design":       "spec-architect",
		"validate-design":   "validator",
		"spec-tasks":        "spec-architect",
	}
	// embed 経路で persona facet content を解決し、prompt 観察の期待値として使う。
	// 同 cwd には override (`<cwd>/.rilll-config/facets/personas/...`) を配置していないため、
	// 解決経路は embed (`internal/facet/facets/personas/<name>.md`) に固定される。
	facetContents := map[string]string{}
	for _, name := range []string{"spec-analyst", "spec-architect", "validator"} {
		rf, err := facet.ResolveFacet(cwd, facet.Persona, name)
		if err != nil {
			t.Fatalf("facet.ResolveFacet(%s): %v", name, err)
		}
		if rf.Content == "" {
			t.Fatalf("facet.ResolveFacet(%s): Content 非空であるべき", name)
		}
		facetContents[name] = rf.Content
	}

	// skill / commit は agent.Calls 上で交互に並ぶ (0,2,4,6,8,10 = skill / 1,3,5,7,9,11 = commit)。
	skillIndexes := []int{0, 2, 4, 6, 8, 10}
	for i, idx := range skillIndexes {
		phase := phaseOrder[i]
		wantPersona := facetContents[phaseToFacet[phase]]
		wantPersonaBlock := "\n\n---\n\n# Persona\n\n" + wantPersona
		wantSlashHead := fmt.Sprintf("/kiro-%s demo", phase)
		wantStepName := fmt.Sprintf("%02d-%s", i+1, phase)
		got := agent.Calls[idx]

		// (a) slash-first 順: prompt 先頭が slash command
		if !strings.HasPrefix(got.Prompt, wantSlashHead) {
			head := got.Prompt
			if len(head) > 80 {
				head = head[:80]
			}
			t.Errorf("call[%d] (%d 段目 %s): prompt 先頭が slash command と一致しない (slash-first 順)\n got_head=%q\nwant_head_prefix=%q",
				idx, i+1, phase, head, wantSlashHead)
		}
		// slash command の後ろに persona block が連結されている
		if !strings.Contains(got.Prompt, wantPersonaBlock) {
			t.Errorf("call[%d] (%d 段目 %s): prompt に persona block が含まれない\n want_block=%q\ngot_head=%q",
				idx, i+1, phase, wantPersonaBlock, got.Prompt[:min(len(got.Prompt), 512)])
		}

		// (b) StepName / StepID の seam 規約
		if got.Options == nil {
			t.Errorf("call[%d] (%d 段目 %s): Options=nil; StepName / Previous Response 検証不可", idx, i+1, phase)
			continue
		}
		if got.Options.StepName != wantStepName {
			t.Errorf("call[%d] (%d 段目 %s): Options.StepName mismatch (StepID 規約)\n got=%q\nwant=%q",
				idx, i+1, phase, got.Options.StepName, wantStepName)
		}
		if got.Options.StepID == "" {
			t.Errorf("call[%d] (%d 段目 %s): Options.StepID は非空であるべき (akashic agent_calls.step_id seam): got=%q",
				idx, i+1, phase, got.Options.StepID)
		}

		// (c) Previous Response 埋め込みは prompt body 内 (slash-first 順)。
		// spec-loop は foundation 側 IncludePreviousResponse 経路を使わない。
		if got.Options.IncludePreviousResponse {
			t.Errorf("call[%d] (%d 段目 %s): IncludePreviousResponse=false を期待 (spec-loop は foundation 前置経路を使わない): got true",
				idx, i+1, phase)
		}
		if got.Options.PreviousResponseContent != "" {
			t.Errorf("call[%d] (%d 段目 %s): PreviousResponseContent=\"\" を期待: got %q",
				idx, i+1, phase, got.Options.PreviousResponseContent)
		}
		if i == 0 {
			if strings.Contains(got.Prompt, "# Previous Response") {
				t.Errorf("call[%d] (1 段目 spec-init): prompt body に `# Previous Response` を含めてはならない (前段なし): got=%q",
					idx, got.Prompt[:min(len(got.Prompt), 512)])
			}
		} else {
			wantPrevBlock := "\n\n---\n\n# Previous Response\n\nok"
			if !strings.Contains(got.Prompt, wantPrevBlock) {
				t.Errorf("call[%d] (%d 段目 %s): prompt に Previous Response block が含まれない (in-memory hit summary=\"ok\")\n want_block=%q\ngot=%q",
					idx, i+1, phase, wantPrevBlock, got.Prompt[:min(len(got.Prompt), 1024)])
			}
		}
	}

	// (d) commit prompt には persona block も Previous Response block も付かない
	commitIndexes := []int{1, 3, 5, 7, 9, 11}
	for _, idx := range commitIndexes {
		got := agent.Calls[idx]
		if strings.Contains(got.Prompt, "# Persona") {
			t.Errorf("call[%d] (commit): commit prompt に persona block を含めてはならない: got=%q",
				idx, got.Prompt)
		}
		if strings.Contains(got.Prompt, "# Previous Response") {
			t.Errorf("call[%d] (commit): commit prompt に Previous Response block を含めてはならない: got=%q",
				idx, got.Prompt)
		}
		if got.Options != nil && got.Options.IncludePreviousResponse {
			t.Errorf("call[%d] (commit): commit に Previous Response wiring を付けてはならない: got Include=%v Content=%q",
				idx, got.Options.IncludePreviousResponse, got.Options.PreviousResponseContent)
		}
	}

	// (e) runDir レイアウト不変保証 (Req 3.2): <NN>-<phase>/result.json が 6 件揃う
	runRoot := filepath.Join(cwd, ".rilll", "kiro-spec-loop")
	runEntries, err := os.ReadDir(runRoot)
	if err != nil {
		t.Fatalf("read runRoot: %v", err)
	}
	if len(runEntries) != 1 {
		t.Fatalf("runRoot に唯一の run-id ディレクトリを期待: got=%d", len(runEntries))
	}
	runDir := filepath.Join(runRoot, runEntries[0].Name())
	for i, phase := range phaseOrder {
		stepDirRel := fmt.Sprintf("%02d-%s", i+1, phase)
		resultPath := filepath.Join(runDir, stepDirRel, "result.json")
		if _, statErr := os.Stat(resultPath); statErr != nil {
			t.Errorf("runDir レイアウト (Req 3.2): %s/result.json が存在するべき: %v", stepDirRel, statErr)
		}
	}
}
