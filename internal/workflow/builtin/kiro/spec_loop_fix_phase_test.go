// Package workflow の本テストファイルは kiro-spec-loop の以下 2 機能に対する統合テストを集約する:
//
//  1. 段別 MaxRounds: validate-gap / validate-design に 15、それ以外 5（engine 既定）
//  2. 最終修正フェーズ (fix phase): rounds > 0 で完走した段の直後に session A を resume して
//     「ここまでの方針で実ファイルを修正してください」プロンプトを 1 回投げる
//
// scriptedAgent / classifyAgentCall / 各種 *Resp() ヘルパは kiro_spec_loop_test.go で定義済み。
// classifyAgentCall は "[fix-phase]" プレフィックスを認識して "fix" kind を返す拡張済み。
package kiro

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

// fixEditOKResp は Turn 1 (fix-edit) が成功した場合の典型応答。
// OutputFormat=nil なので Content (markdown) ベースで返す。SessionID は監査用ダミー値。
func fixEditOKResp() provider.AgentResponse {
	return provider.AgentResponse{
		Success: true,
		Content: "design.md の 6 節を更新しました。\n\n## Summary\n- 修正したファイル: .kiro/specs/demo/design.md\n- 反映内容: 6 節の判断結論をラウンドの合意に揃えた。",
		// 構造化出力は付けない (Turn 1 は schema 強制なし)
		SessionID: "session-fix-edit",
	}
}

// fixJudgeOKResp は Turn 2 (fix-judge) が成功した場合の典型応答。
// json_schema 強制下で has_inquiry=false / summary を構造化出力で返す。
func fixJudgeOKResp() provider.AgentResponse {
	return provider.AgentResponse{
		Success: true,
		StructuredOutput: map[string]any{
			"has_inquiry": false,
			"summary":     "修正完了: design.md の 6 節を更新しました",
		},
		SessionID: "session-fix-judge",
	}
}

// countCallsByKind は agent.Calls 内の指定 kind の呼び出し件数を返す。
// テストで「fix kind が N 件呼ばれたか」「commit が N 件呼ばれたか」等のアサートに使う。
func countCallsByKind(calls []recordedAgentCall, kind string) int {
	n := 0
	for _, c := range calls {
		if c.Kind == kind {
			n++
		}
	}
	return n
}

// =============================================================================
// 1. 段別 MaxRounds テスト
// =============================================================================

// TestKiroSpecLoopRun_PerStepMaxRounds_ValidateGapHas15 は validate-gap (Order 3) が
// engine 既定 5 ラウンドを超えてループできること（MaxRounds=15）を検証する。
//
// シナリオ: validate-gap で 6 ラウンド分 has_inquiry=true を返し、7 ラウンド目の skill resume で
// has_inquiry=false を返して完走させる。MaxRounds=5 のままなら 5 ラウンド目で loop-limit
// abort するため、completed で終わるならば MaxRounds が 5 を超えて設定されていることが分かる。
//
// 他段 (1, 2, 4, 5, 6) は has_inquiry=false で即完走させる。
func TestKiroSpecLoopRun_PerStepMaxRounds_ValidateGapHas15(t *testing.T) {
	feature := "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "commit":
			return commitOKResp(), nil
		case "answerer":
			return answerOKResp(), nil
		case "fix-edit":
			// Turn 1: text モード成功
			return fixEditOKResp(), nil
		case "fix-judge":
			// Turn 2: 構造化出力で要約抽出
			return fixJudgeOKResp(), nil
		case "skill":
			// validate-gap (3 段目) のみラウンドを回す。他の skill 呼び出しは即完走。
			// kiro-spec-loop-facets task 5.1 で session A 初回 skill prompt の先頭に
			// persona prefix が prepend されたため HasPrefix では識別できない。Contains で判定する。
			if !strings.Contains(call.Prompt, "/kiro-validate-gap") &&
				!isResumeForValidateGap(agent.Calls) {
				return skillNoInquiryResp(), nil
			}
			// validate-gap 系の skill 呼び出し（初回 + resume）に対して、6 ラウンド分は inquiry を返し、
			// 7 回目（=ラウンド 7 の skill resume）で has_inquiry=false を返して完走させる。
			//   呼び出し回数:
			//     1: 初回 /kiro-validate-gap → inquiry
			//     2: round 1 resume          → inquiry
			//     3: round 2 resume          → inquiry
			//     4: round 3 resume          → inquiry
			//     5: round 4 resume          → inquiry
			//     6: round 5 resume          → inquiry  ← 既定 MaxRounds=5 ならここで loop-limit
			//     7: round 6 resume          → inquiry
			//     8: round 7 resume          → no_inquiry → completed (rounds=7)
			validateGapSkillCount := 0
			for _, c := range agent.Calls {
				if c.Kind == "skill" && (strings.Contains(c.Prompt, "/kiro-validate-gap") ||
					(c.ResumeSessionID == "session-skill" && isAfterValidateGapStart(agent.Calls, c))) {
					validateGapSkillCount++
				}
			}
			if validateGapSkillCount >= 8 {
				return skillNoInquiryResp(), nil
			}
			return skillInquiryResp(), nil
		}
		return provider.AgentResponse{}, nil
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

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonCompleted {
		t.Fatalf("MaxRounds=15 が validate-gap に効いていれば完走するはず: terminationReason=%q",
			rr.TerminationReason)
	}
	if len(rr.Steps) != 6 {
		t.Fatalf("6 段すべて完走を期待: got=%d steps", len(rr.Steps))
	}

	// validate-gap は rounds=7 で完走したはず（初期値 5 を超えていることが MaxRounds=15 の効力証明）。
	gap := rr.Steps[2]
	if gap.Phase != "validate-gap" {
		t.Fatalf("steps[2].Phase=validate-gap を期待: got=%q", gap.Phase)
	}
	if gap.Rounds != 7 {
		t.Errorf("validate-gap rounds=7 を期待 (6 ラウンド inquiry + 1 ラウンド no_inquiry で完走): got=%d",
			gap.Rounds)
	}
	if !gap.CommitOK {
		t.Errorf("validate-gap.CommitOK=true を期待: got false")
	}

	// fix phase が validate-gap で発動したことの確認（rounds>0 のため）。
	if gap.Fix == nil {
		t.Fatalf("rounds>0 で完走した段は Fix が non-nil であるべき (validate-gap)")
	}
	if !gap.Fix.Ran || !gap.Fix.OK {
		t.Errorf("validate-gap.Fix.Ran=true, OK=true を期待: got=%+v", gap.Fix)
	}
}

// isResumeForValidateGap / isAfterValidateGapStart は、scriptedAgent の Respond クロージャ内で
// 「現在の skill 呼び出しが validate-gap 段の resume 連鎖に属するか」を判定する補助関数。
// 直近の skill 系初回呼び出し (= ResumeSessionID="" の skill kind) が /kiro-validate-gap だったかを
// 過去履歴から確認する。
//
// kiro-spec-loop-facets task 5.1 で session A 初回 skill prompt の先頭に
// `# Persona\n\n<facet content>\n\n---\n\n` が prepend されるようになったため、旧 `HasPrefix(prompt, "/")`
// では初回呼び出しを識別できない。代わりに `c.ResumeSessionID == ""` を「初回呼び出し」の seam とする。
// slash command 本体は依然 prompt に含まれるため Contains で識別する。
func isResumeForValidateGap(calls []recordedAgentCall) bool {
	for i := len(calls) - 1; i >= 0; i-- {
		c := calls[i]
		if c.Kind != "skill" {
			continue
		}
		if c.ResumeSessionID == "" {
			return strings.Contains(c.Prompt, "/kiro-validate-gap")
		}
	}
	return false
}

func isAfterValidateGapStart(calls []recordedAgentCall, current recordedAgentCall) bool {
	// current が同 slice に既に append 済みである前提で、current より前で最後に出た
	// 「初回 skill 呼び出し」(= ResumeSessionID="" の skill kind) が /kiro-validate-gap であれば true。
	idx := -1
	for i := range calls {
		if calls[i].Prompt == current.Prompt && calls[i].ResumeSessionID == current.ResumeSessionID {
			idx = i
		}
	}
	if idx < 0 {
		return false
	}
	for i := idx - 1; i >= 0; i-- {
		c := calls[i]
		if c.Kind != "skill" {
			continue
		}
		if c.ResumeSessionID == "" {
			return strings.Contains(c.Prompt, "/kiro-validate-gap")
		}
	}
	return false
}

// TestKiroSpecLoopRun_PerStepMaxRounds_SpecRequirementsKeeps5 は spec-requirements (Order 2) の
// MaxRounds が engine 既定 5 のままであり、validate-* の 15 が誤って他段に波及していないことを
// 検証する。
//
// シナリオ: 1 段目 spec-init は即完走、2 段目 spec-requirements は無限に inquiry を返す。
// MaxRounds=5 が効いていれば 5 ラウンドで loop-limit abort するはず。
func TestKiroSpecLoopRun_PerStepMaxRounds_SpecRequirementsKeeps5(t *testing.T) {
	feature := "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "commit":
			return commitOKResp(), nil
		case "answerer":
			return answerOKResp(), nil
		case "skill":
			// spec-init は has_inquiry=false で即完走させる。
			// kiro-spec-loop-facets task 5.1 で session A 初回 skill prompt に persona prefix が
			// prepend されたため HasPrefix から Contains に切り替える。
			if strings.Contains(call.Prompt, "/kiro-spec-init") {
				return skillNoInquiryResp(), nil
			}
			// spec-requirements とその resume 連鎖はずっと inquiry を返す。
			return skillInquiryResp(), nil
		}
		return provider.AgentResponse{}, nil
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

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonLoopLimit {
		t.Fatalf("spec-requirements は MaxRounds=5（engine 既定）のはず → loop-limit を期待: got=%q",
			rr.TerminationReason)
	}
	if rr.AbortStep == nil || rr.AbortStep.Order != 2 || rr.AbortStep.Phase != "spec-requirements" {
		t.Errorf("AbortStep mismatch: got=%+v want=Order=2 Phase=spec-requirements", rr.AbortStep)
	}
	if rr.Failure == nil || rr.Failure.Round != 5 {
		t.Errorf("Failure.Round=5 を期待 (engine 既定 MaxRounds): got=%+v", rr.Failure)
	}
}

// =============================================================================
// 2. fix phase テスト
// =============================================================================

// TestKiroSpecLoopRun_FixPhase_TriggeredWhenRoundsPositive は、step が rounds>0 で完走したとき
// fix phase が 2 ターン (fix-edit → fix-judge) で呼ばれ、result.json.steps[].fix が記録され、
// fix-result.md が永続化されることを検証する。
//
// 2 ターン化 (Phase B): Turn 1 で OutputFormat=nil の text モードで実ファイル編集、
// Turn 2 で resume + json_schema 強制で要約を抽出する。stream_idle_timeout の主因 (Edit 直後の
// 構造化生成) を避けるため、編集と要約を分離する。
//
// シナリオ: step 1 (spec-init) のみ 1 ラウンド回す（初回 inquiry → 回答 → resume で no_inquiry）。
// 他段 (2-6) は has_inquiry=false で即完走。
func TestKiroSpecLoopRun_FixPhase_TriggeredWhenRoundsPositive(t *testing.T) {
	feature := "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "commit":
			return commitOKResp(), nil
		case "answerer":
			return answerOKResp(), nil
		case "fix-edit":
			return fixEditOKResp(), nil
		case "fix-judge":
			return fixJudgeOKResp(), nil
		case "skill":
			// spec-init: 初回 = inquiry、resume = no_inquiry で 1 ラウンドで完走させる。
			// kiro-spec-loop-facets task 5.1 で session A 初回 skill prompt に persona prefix が
			// prepend されたため HasPrefix から Contains に切り替える。
			if strings.Contains(call.Prompt, "/kiro-spec-init") {
				return skillInquiryResp(), nil
			}
			// spec-init の resume（"# Persona" でない / "/" 始まりでない skill）は no_inquiry。
			// それ以外の段の初回 skill も no_inquiry。
			return skillNoInquiryResp(), nil
		}
		return provider.AgentResponse{}, nil
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

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonCompleted {
		t.Fatalf("全段完走を期待: terminationReason=%q", rr.TerminationReason)
	}

	// step 1 (spec-init) のみ rounds>0 / Fix 発動を期待。他段は Fix=nil。
	specInit := rr.Steps[0]
	if specInit.Rounds != 1 {
		t.Errorf("spec-init.Rounds=1 を期待 (1 ラウンドで完走): got=%d", specInit.Rounds)
	}
	if specInit.Fix == nil {
		t.Fatalf("spec-init.Fix は rounds>0 完走で non-nil であるべき")
	}
	if !specInit.Fix.Ran {
		t.Errorf("spec-init.Fix.Ran=true を期待: got=%+v", specInit.Fix)
	}
	if !specInit.Fix.OK {
		t.Errorf("spec-init.Fix.OK=true を期待 (fixEditOKResp + fixJudgeOKResp で返している): got=%+v", specInit.Fix)
	}
	// 2 ターン化により SessionID は judge 側のものが入る
	if specInit.Fix.SessionID != "session-fix-judge" {
		t.Errorf("spec-init.Fix.SessionID=session-fix-judge を期待 (Turn 2 の sessionId が表に出る): got=%q", specInit.Fix.SessionID)
	}
	if specInit.Fix.EditSession != "session-fix-edit" {
		t.Errorf("spec-init.Fix.EditSession=session-fix-edit を期待: got=%q", specInit.Fix.EditSession)
	}
	if specInit.Fix.JudgeSession != "session-fix-judge" {
		t.Errorf("spec-init.Fix.JudgeSession=session-fix-judge を期待: got=%q", specInit.Fix.JudgeSession)
	}
	if specInit.Fix.Summary == "" {
		t.Errorf("spec-init.Fix.Summary は非空を期待 (fixJudgeOKResp で summary をセット): got empty")
	}

	// 他段 (2..6) は rounds=0 完走で Fix=nil
	for i := 1; i < len(rr.Steps); i++ {
		if rr.Steps[i].Fix != nil {
			t.Errorf("steps[%d] (phase=%s) は rounds=0 完走で Fix=nil を期待: got=%+v",
				i, rr.Steps[i].Phase, rr.Steps[i].Fix)
		}
	}

	// fix-result.md が 01-spec-init/ に書き出されていることを検証
	runDirParent := filepath.Join(cwd, ".rilll", "kiro-spec-loop")
	entries, err := os.ReadDir(runDirParent)
	if err != nil || len(entries) != 1 {
		t.Fatalf("run dir が見つからない: err=%v entries=%v", err, entries)
	}
	fixResultPath := filepath.Join(runDirParent, entries[0].Name(), "01-spec-init", "fix-result.md")
	body, err := os.ReadFile(fixResultPath)
	if err != nil {
		t.Fatalf("fix-result.md が書き出されていない: %v", err)
	}
	if !strings.Contains(string(body), "ok: true") {
		t.Errorf("fix-result.md に 'ok: true' を期待: %q", string(body))
	}
	if !strings.Contains(string(body), "session-fix-judge") {
		t.Errorf("fix-result.md に sessionId 'session-fix-judge' を期待: %q", string(body))
	}
	if !strings.Contains(string(body), "## Edit Phase Prompt") {
		t.Errorf("fix-result.md に '## Edit Phase Prompt' セクションを期待: %q", string(body))
	}
	if !strings.Contains(string(body), "## Judge Phase Prompt") {
		t.Errorf("fix-result.md に '## Judge Phase Prompt' セクションを期待: %q", string(body))
	}

	// 2 ターン化により fix-edit + fix-judge の 2 件が同 step で呼ばれる
	editCalls := 0
	judgeCalls := 0
	for _, c := range agent.Calls {
		switch c.Kind {
		case "fix-edit":
			editCalls++
			// Turn 1 は session A の最後の skill SessionID を resume する
			if c.ResumeSessionID != "session-skill" {
				t.Errorf("fix-edit (Turn 1) は session-skill を resume すべき: got=%q", c.ResumeSessionID)
			}
			// Turn 1 は OutputFormat=nil (text モード)
			if c.Options != nil && c.Options.OutputFormat != nil {
				t.Errorf("fix-edit は OutputFormat=nil で呼ばれるべき: got=%+v", c.Options.OutputFormat)
			}
			if c.Options != nil && !strings.HasPrefix(c.Options.StepID, "fix-01-spec-init-edit") {
				t.Errorf("fix-edit StepID は 'fix-01-spec-init-edit' で始まるべき: got=%q", c.Options.StepID)
			}
		case "fix-judge":
			judgeCalls++
			// Turn 2 は Turn 1 の応答 sessionId を resume する
			if c.ResumeSessionID != "session-fix-edit" {
				t.Errorf("fix-judge (Turn 2) は session-fix-edit を resume すべき: got=%q", c.ResumeSessionID)
			}
			// Turn 2 は json_schema 強制
			if c.Options == nil || c.Options.OutputFormat == nil {
				t.Errorf("fix-judge は OutputFormat (json_schema) 強制で呼ばれるべき: options=%+v", c.Options)
			}
			if c.Options != nil && !strings.HasPrefix(c.Options.StepID, "fix-01-spec-init-judge") {
				t.Errorf("fix-judge StepID は 'fix-01-spec-init-judge' で始まるべき: got=%q", c.Options.StepID)
			}
		}
	}
	if editCalls != 1 {
		t.Errorf("fix-edit 呼び出しは 1 件を期待 (rounds>0 の段=spec-init のみ): got=%d", editCalls)
	}
	if judgeCalls != 1 {
		t.Errorf("fix-judge 呼び出しは 1 件を期待: got=%d", judgeCalls)
	}

	// AllowedTools の検証 (Phase C):
	//   - fix-edit (Turn 1): AllowedTools=nil (= SDK 既定 = 全ツール許可) → Edit/Write 可能
	//   - fix-judge (Turn 2): AllowedTools=["Read","Glob","Grep"] (読み取りのみ許可)
	for _, c := range agent.Calls {
		switch c.Kind {
		case "fix-edit":
			if c.Options != nil && c.Options.AllowedTools != nil {
				t.Errorf("fix-edit (Turn 1) は AllowedTools=nil で呼ばれるべき (SDK 既定維持): got=%v", c.Options.AllowedTools)
			}
		case "fix-judge":
			if c.Options == nil {
				t.Fatalf("fix-judge は CallAgentOptions 必須")
			}
			want := []string{"Read", "Glob", "Grep"}
			if len(c.Options.AllowedTools) != len(want) {
				t.Fatalf("fix-judge AllowedTools 長さ違い: got=%v want=%v", c.Options.AllowedTools, want)
			}
			for i, w := range want {
				if c.Options.AllowedTools[i] != w {
					t.Errorf("fix-judge AllowedTools[%d]=%q want %q", i, c.Options.AllowedTools[i], w)
				}
			}
		}
	}
}

// TestKiroSpecLoopRun_FixPhase_NotTriggeredWhenRoundsZero は、全段 rounds=0 完走では fix phase が
// 1 度も呼ばれず、fix-result.md も書き出されず、result.json.steps[].fix も nil であることを検証する。
func TestKiroSpecLoopRun_FixPhase_NotTriggeredWhenRoundsZero(t *testing.T) {
	feature := "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "commit":
			return commitOKResp(), nil
		case "fix-edit", "fix-judge":
			t.Errorf("rounds=0 完走では fix phase を呼んではならない: kind=%q prompt=%q", call.Kind, call.Prompt)
			return fixJudgeOKResp(), nil
		case "skill":
			return skillNoInquiryResp(), nil
		}
		return provider.AgentResponse{}, nil
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

	if got := countCallsByKind(agent.Calls, "fix-edit"); got != 0 {
		t.Errorf("fix-edit 呼び出し件数 0 を期待: got=%d", got)
	}
	if got := countCallsByKind(agent.Calls, "fix-judge"); got != 0 {
		t.Errorf("fix-judge 呼び出し件数 0 を期待: got=%d", got)
	}

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonCompleted {
		t.Fatalf("完走を期待: terminationReason=%q", rr.TerminationReason)
	}
	for i, s := range rr.Steps {
		if s.Fix != nil {
			t.Errorf("steps[%d].Fix は rounds=0 完走で nil を期待: got=%+v", i, s.Fix)
		}
	}

	// fix-result.md が 6 段いずれにも存在しないことを検証
	runDirParent := filepath.Join(cwd, ".rilll", "kiro-spec-loop")
	entries, _ := os.ReadDir(runDirParent)
	if len(entries) != 1 {
		t.Fatalf("run dir 1 件を期待: got=%d", len(entries))
	}
	runDir := filepath.Join(runDirParent, entries[0].Name())
	for _, step := range steps {
		fixPath := filepath.Join(runDir, stepDirName(step.Order, step.Phase), "fix-result.md")
		if _, err := os.Stat(fixPath); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("fix-result.md は存在してはならない: %s (err=%v)", fixPath, err)
		}
	}
}

// stepDirName は <NN>-<phase> ディレクトリ名を組み立てる。stepDir は runDir 込みのフルパスを返すが、
// テストでは relative 名だけ欲しいケースがあるためローカル定義する。
func stepDirName(order int, phase string) string {
	return strings.TrimPrefix(stepDir("", order, phase), string(filepath.Separator))
}

// TestKiroSpecLoopRun_FixPhase_BestEffortOnFailure は、fix phase Turn 1 (fix-edit) の agent 呼び出しが
// 失敗 (error) した場合でも Turn 2 はスキップされ、/git-commit に進み、ワークフロー全体は completed で
// 終わることを検証する（best-effort 設計）。fix.OK=false / fix.Message に error 概要が記録されることも確認する。
func TestKiroSpecLoopRun_FixPhase_BestEffortOnFailure(t *testing.T) {
	feature := "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "commit":
			return commitOKResp(), nil
		case "answerer":
			return answerOKResp(), nil
		case "fix-edit":
			// Turn 1 で agent error を返す → Turn 2 はスキップされ、best-effort で /git-commit に進むはず。
			return provider.AgentResponse{}, errors.New("simulated fix-phase agent failure")
		case "fix-judge":
			t.Errorf("Turn 1 が agent error の場合 Turn 2 は呼ばれないはず: kind=%q", call.Kind)
			return provider.AgentResponse{}, nil
		case "skill":
			// step 1 (spec-init) で 1 ラウンド回して fix phase を発動させる。他段は即完走。
			// kiro-spec-loop-facets task 5.1 で session A 初回 skill prompt に persona prefix が
			// prepend されたため HasPrefix から Contains に切り替える。
			if strings.Contains(call.Prompt, "/kiro-spec-init") {
				return skillInquiryResp(), nil
			}
			return skillNoInquiryResp(), nil
		}
		return provider.AgentResponse{}, nil
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

	rr := mustReadResultJSON(t, cwd)
	// best-effort: fix 失敗でも /git-commit を実行 → completed で終わる
	if rr.TerminationReason != RunReasonCompleted {
		t.Fatalf("fix phase 失敗は best-effort で completed になるべき: got=%q msg=%q",
			rr.TerminationReason, rr.AbortMessage)
	}
	if len(rr.Steps) != 6 {
		t.Fatalf("6 段すべて完走を期待 (fix 失敗は中断要因にならない): got=%d", len(rr.Steps))
	}

	// step 1 (spec-init) は rounds>0 で完走、fix は OK=false / Message にエラー概要を含む
	specInit := rr.Steps[0]
	if specInit.Fix == nil {
		t.Fatalf("spec-init.Fix が nil（fix phase が呼ばれていない可能性）")
	}
	if !specInit.Fix.Ran {
		t.Errorf("spec-init.Fix.Ran=true を期待: got=%+v", specInit.Fix)
	}
	if specInit.Fix.OK {
		t.Errorf("spec-init.Fix.OK=false を期待 (agent error): got=%+v", specInit.Fix)
	}
	if !strings.Contains(specInit.Fix.Message, "agent error") {
		t.Errorf("spec-init.Fix.Message に 'agent error' を期待: got=%q", specInit.Fix.Message)
	}
	// Turn 1 失敗時は Turn 2 は呼ばれないため judgeSession は空のまま
	if specInit.Fix.JudgeSession != "" {
		t.Errorf("Turn 1 失敗時 JudgeSession は空であるべき: got=%q", specInit.Fix.JudgeSession)
	}

	// commit が 6 件すべて呼ばれていること（fix 失敗で commit が止まっていない証明）
	if got := countCallsByKind(agent.Calls, "commit"); got != 6 {
		t.Errorf("fix 失敗は commit を阻害しない（6 commit を期待）: got=%d", got)
	}
	if !specInit.CommitOK {
		t.Errorf("spec-init.CommitOK=true を期待 (fix 失敗後も commit は実行された): got false")
	}

	// Turn 1 (fix-edit) は呼ばれたが Turn 2 (fix-judge) は呼ばれていないこと
	if got := countCallsByKind(agent.Calls, "fix-edit"); got != 1 {
		t.Errorf("fix-edit (Turn 1) は 1 件呼ばれるべき: got=%d", got)
	}
	if got := countCallsByKind(agent.Calls, "fix-judge"); got != 0 {
		t.Errorf("Turn 1 失敗時 fix-judge (Turn 2) は呼ばれないべき: got=%d", got)
	}
}

// TestKiroSpecLoopRun_FixPhase_JudgeFailureRecoversTurn1Summary は Turn 2 (fix-judge) が
// agent error で失敗したときに、Turn 1 (fix-edit) の Content が summary に救出されることを検証する。
//
// ベスト・エフォート設計: Turn 2 失敗でも Turn 1 で実ファイル編集は完了している (success=true) ため、
// Turn 1 の markdown 本文を summary に詰めて /git-commit に進む。
func TestKiroSpecLoopRun_FixPhase_JudgeFailureRecoversTurn1Summary(t *testing.T) {
	feature := "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}

	const editContent = "TURN1_MARKER: design.md を更新しました"
	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "commit":
			return commitOKResp(), nil
		case "answerer":
			return answerOKResp(), nil
		case "fix-edit":
			return provider.AgentResponse{
				Success:   true,
				Content:   editContent,
				SessionID: "session-fix-edit",
			}, nil
		case "fix-judge":
			// Turn 2 で agent error → Turn 1 の markdown を救出するパスに入る
			return provider.AgentResponse{}, errors.New("simulated judge agent failure")
		case "skill":
			// kiro-spec-loop-facets task 5.1 で session A 初回 skill prompt に persona prefix が
			// prepend されたため HasPrefix から Contains に切り替える。
			if strings.Contains(call.Prompt, "/kiro-spec-init") {
				return skillInquiryResp(), nil
			}
			return skillNoInquiryResp(), nil
		}
		return provider.AgentResponse{}, nil
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

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonCompleted {
		t.Fatalf("Turn 2 失敗は best-effort で completed になるべき: got=%q", rr.TerminationReason)
	}

	specInit := rr.Steps[0]
	if specInit.Fix == nil {
		t.Fatalf("spec-init.Fix が nil")
	}
	if specInit.Fix.OK {
		t.Errorf("Turn 2 失敗で OK=false を期待: got=%+v", specInit.Fix)
	}
	if specInit.Fix.EditSession != "session-fix-edit" {
		t.Errorf("EditSession=session-fix-edit を期待: got=%q", specInit.Fix.EditSession)
	}
	// Turn 2 が agent error なので JudgeSession は空のまま
	if specInit.Fix.JudgeSession != "" {
		t.Errorf("Turn 2 agent error 時 JudgeSession は空であるべき: got=%q", specInit.Fix.JudgeSession)
	}
	// Summary には Turn 1 の Content を救出
	if !strings.Contains(specInit.Fix.Summary, "TURN1_MARKER") {
		t.Errorf("Turn 2 失敗時 Summary に Turn 1 の Content を救出するべき: got=%q", specInit.Fix.Summary)
	}
	if !strings.Contains(specInit.Fix.Message, "judge agent error") {
		t.Errorf("Message に judge agent error を期待: got=%q", specInit.Fix.Message)
	}
}
