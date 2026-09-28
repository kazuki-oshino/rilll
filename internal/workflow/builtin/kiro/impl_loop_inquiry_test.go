// Package workflow の本テストファイルは kiro-impl-loop の問い合わせ⇄回答ループ
// 機能 (issue #14) を駆動するテストを集約する。
//
// 検証対象:
//   - inquiryloop.Engine.RunPrompt 駆動への置き換え
//   - impl 専用 persona (impl-direction) の解決失敗時の中断
//   - 0 ラウンド / 1 ラウンド完走の正常パス
//   - schema-violation / prompt-failure / loop-limit の各 abort パス
//   - round-N-{inquiry,answer}.md の永続化
//
// scriptedAgent / classifyAgentCall / skillNoInquiryResp / commitOKResp 等のヘルパは
// kiro_spec_loop_test.go で既に同パッケージに定義されており、本ファイルからはそのまま流用する。
package kiro

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/inquiryloop"
	"github.com/kazuki-oshino/rilll/internal/provider"
)

// finalResultWithReason は formatImplAbortMessage のテスト用に inquiryloop.FinalResult を
// 最小構成で組み立てるヘルパ。Status は Aborted 固定で、reason / message のみ可変。
func finalResultWithReason(reason, message string) inquiryloop.FinalResult {
	return inquiryloop.FinalResult{
		Status:      inquiryloop.StatusAborted,
		Termination: inquiryloop.TerminationReason(reason),
		Failure: &inquiryloop.FailureDetail{
			Reason:  inquiryloop.TerminationReason(reason),
			Message: message,
		},
	}
}

// implFinalReviewOKResp は /kiro-validate-impl の成功応答。
// kiro-impl-loop の最終検証 (engine 駆動しない) で使う。
func implFinalReviewOKResp() provider.AgentResponse {
	return provider.AgentResponse{
		Success: true,
		StructuredOutput: map[string]any{
			"approved":          true,
			"feature":           "demo",
			"summary":           "all tasks ok",
			"all_tasks_checked": true,
			"issues":            []any{},
		},
		SessionID: "session-final",
	}
}

// setupImplFeatureCWD は kiro-impl-loop が要求する spec.json / tasks.md を整える。
// validImplSpecJSON は kiro_impl_loop_filter_test.go で定義済みのものを再利用する。
func setupImplFeatureCWD(t *testing.T, feature, tasksMD string) string {
	t.Helper()
	cwd := t.TempDir()
	specDir := filepath.Join(cwd, ".kiro", "specs", feature)
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", specDir, err)
	}
	if err := os.WriteFile(filepath.Join(specDir, "spec.json"), []byte(validImplSpecJSON), 0o644); err != nil {
		t.Fatalf("WriteFile spec.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(specDir, "tasks.md"), []byte(tasksMD), 0o644); err != nil {
		t.Fatalf("WriteFile tasks.md: %v", err)
	}
	return cwd
}

// TestKiroImplLoopInquiry_PersonaResolutionFailure: impl 専用 session B Persona facet の
// override が空ファイルで errEmptySessionBPersona を返すとき、impl ループは開始されず
// workflow.WorkflowError で止まる。
//
// 期待:
//   - agent.CallAgent は呼ばれない (impl 段に入る前に persona 解決でストップ)
//   - workflow.WorkflowError event の Message に impl 専用 persona 名が含まれる
func TestKiroImplLoopInquiry_PersonaResolutionFailure(t *testing.T) {
	feature := "demo"
	cwd := setupImplFeatureCWD(t, feature, "- [ ] 1. 未完\n")

	// session B の impl-direction logical name は impl-direction-answerer facet asset へ写像される。
	// 空 override は agent 起動前の expected abort として扱う。
	writeFile(t, filepath.Join(cwd, ".rilll-config", "facets", "personas", "impl-direction-answerer.md"), "")

	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Selected: feature}
	agent := &scriptedAgent{}

	if err := runKiroImplLoop(
		context.Background(),
		cwd,
		onEvent,
		ask,
		agent,
	); err != nil {
		t.Fatalf("runKiroImplLoop returned error: %v", err)
	}

	if len(agent.Calls) != 0 {
		t.Fatalf("persona 解決失敗時 agent.CallAgent は呼ばれてはならない: got=%d calls", len(agent.Calls))
	}
	if !hasErrorEvent(*eventsPtr, implPersonaName) {
		t.Fatalf("workflow.WorkflowError event は impl persona 名 %q を含むべき: %+v", implPersonaName, *eventsPtr)
	}
	message := workflowErrorMessage(t, *eventsPtr)
	for _, want := range []string{
		"impl-direction-answerer",
		".rilll-config/facets/personas/impl-direction-answerer.md",
		"embed:facets/personas/impl-direction-answerer.md",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("WorkflowError message %q does not contain %q", message, want)
		}
	}
	if strings.Contains(message, ".kiro/personas") {
		t.Fatalf("WorkflowError message must not guide to old persona path: %q", message)
	}
}

// TestKiroImplLoopInquiry_NoInquirySingleTask: 1 タスクで rounds=0 完走 → commit → 最終検証 完走。
//
// 期待 (agent.Calls 順序):
//  1. impl skill 呼び出し (kind=skill, prompt=/kiro-impl <feature> <task>) → has_inquiry=false
//  2. /git-commit (kind=commit) → success
//  3. /kiro-validate-impl (kind=skill) → success
func TestKiroImplLoopInquiry_NoInquirySingleTask(t *testing.T) {
	feature := "demo"
	cwd := setupImplFeatureCWD(t, feature, "- [ ] 1. 単独タスク\n")
	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Selected: feature}

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch {
		case call.Kind == "commit":
			return commitOKResp(), nil
		case strings.HasPrefix(call.Prompt, "/kiro-validate-impl"):
			return implFinalReviewOKResp(), nil
		default:
			return skillNoInquiryResp(), nil
		}
	}

	if err := runKiroImplLoop(
		context.Background(),
		cwd,
		onEvent,
		ask,
		agent,
	); err != nil {
		t.Fatalf("runKiroImplLoop returned error: %v", err)
	}

	if len(agent.Calls) != 3 {
		t.Fatalf("成功パスでは impl skill + commit + final-review = 3 件の CallAgent を期待: got=%d (%+v)", len(agent.Calls), agent.Calls)
	}
	// N=1 段目 (前段なし) では `# Previous Response` ブロックは含まれない。
	if !strings.HasPrefix(agent.Calls[0].Prompt, "/kiro-impl "+feature) {
		t.Errorf("call[0] は `/kiro-impl %s` で始まるべき: got head=%q",
			feature, agent.Calls[0].Prompt[:min(len(agent.Calls[0].Prompt), 64)])
	}
	if strings.Contains(agent.Calls[0].Prompt, "# Previous Response") {
		t.Errorf("call[0] (N=1 段目) は `# Previous Response` ブロックを含まないべき (前段なし): got=%q",
			agent.Calls[0].Prompt)
	}
	if !strings.HasPrefix(agent.Calls[1].Prompt, "/git-commit") {
		t.Errorf("call[1] は /git-commit であるべき: got=%q", agent.Calls[1].Prompt)
	}
	assertGitCommitProviderOverrides(t, agent.Calls[1].Options)
	if !strings.HasPrefix(agent.Calls[2].Prompt, "/kiro-validate-impl") {
		t.Errorf("call[2] は /kiro-validate-impl であるべき: got=%q", agent.Calls[2].Prompt)
	}

	// engine.RunPrompt は初回 prompt の末尾に runner-directive を付加する。
	// "[runner-directive]" マーカーを含むことで json_schema 強制経路を通っていることを確認する。
	if !strings.Contains(agent.Calls[0].Prompt, "[runner-directive]") {
		t.Errorf("impl skill 呼び出し prompt は runner-directive を含むべき: %q", agent.Calls[0].Prompt)
	}
	// engine.RunPrompt は impl skill 呼び出し時に inquiryloop.Schema() を OutputFormat として強制する。
	if agent.Calls[0].Options == nil || agent.Calls[0].Options.OutputFormat == nil {
		t.Errorf("impl skill 呼び出しでは OutputFormat (json_schema) が強制されるべき: options=%+v", agent.Calls[0].Options)
	}

	// rounds=0 完走では round-* 永続化ファイルは作られない (onRound コールバック未発火)。
	// kiro-impl-loop-facets task 3.2 で `persistImplTaskResult` が per-task `result.json`
	// を `runDir/<NN>-impl-<sanitizedTaskID>/` に書き出すようになったため、`01-impl-1/`
	// ディレクトリ自体は engine 完走時に作られる契約に変わった (Req 1.4 on-disk fallback source)。
	// したがって本テストでは directory 存在の有無ではなく、round-* ファイルが作られないことを
	// 直接検査する形に切り替える。
	stepDirRel := filepath.Join(".rilll", "kiro-impl-loop")
	if entries, err := os.ReadDir(filepath.Join(cwd, stepDirRel)); err == nil && len(entries) == 1 {
		runDir := filepath.Join(cwd, stepDirRel, entries[0].Name())
		stepDir := filepath.Join(runDir, "01-impl-1")
		for _, suffix := range []string{"round-1-inquiry.md", "round-1-answer.md"} {
			if _, err := os.Stat(filepath.Join(stepDir, suffix)); err == nil {
				t.Errorf("rounds=0 完走では round-* 永続化ファイル %s は作られないべき (onRound 未発火)", suffix)
			}
		}
	}

	// tasks.md がチェック済みになっていること。
	tasksRaw, err := os.ReadFile(filepath.Join(cwd, ".kiro", "specs", feature, "tasks.md"))
	if err != nil {
		t.Fatalf("ReadFile tasks.md: %v", err)
	}
	if !strings.Contains(string(tasksRaw), "- [x] 1.") {
		t.Errorf("タスク 1 が完了マークされていない: %s", string(tasksRaw))
	}

	if !hasDoneEvent(*eventsPtr, feature) {
		t.Errorf("workflow.WorkflowDone event に feature 名 %q が含まれるべき: %+v", feature, *eventsPtr)
	}
}

// TestKiroImplLoopInquiry_OneRoundSingleTask: 1 タスクで rounds=1 完走。
//
// シナリオ:
//   - 1st impl skill 呼び出し → has_inquiry=true
//   - answerer (persona 回答) → has_inquiry=false (summary が回答テキスト)
//   - 2nd impl skill 呼び出し (resume) → has_inquiry=false
//   - /git-commit → success
//   - /kiro-validate-impl → success
//
// 期待:
//   - agent.Calls = 5 件 (impl-skill, answerer, impl-skill-resume, commit, final-review)
//   - <run-dir>/01-impl-1/round-1-inquiry.md と round-1-answer.md が永続化される
func TestKiroImplLoopInquiry_OneRoundSingleTask(t *testing.T) {
	feature := "demo"
	cwd := setupImplFeatureCWD(t, feature, "- [ ] 1. 単独タスク\n")
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
			if strings.HasPrefix(call.Prompt, "/kiro-validate-impl") {
				return implFinalReviewOKResp(), nil
			}
			// 1st impl skill 呼び出し: resume="" → has_inquiry=true
			// 2nd impl skill 呼び出し: resume!="" → has_inquiry=false
			if call.ResumeSessionID == "" {
				return skillInquiryResp(), nil
			}
			return skillNoInquiryResp(), nil
		}
		return provider.AgentResponse{}, nil
	}

	if err := runKiroImplLoop(
		context.Background(),
		cwd,
		onEvent,
		ask,
		agent,
	); err != nil {
		t.Fatalf("runKiroImplLoop returned error: %v", err)
	}

	if len(agent.Calls) != 5 {
		t.Fatalf("rounds=1 完走では impl-skill + answerer + impl-skill-resume + commit + final-review = 5 件を期待: got=%d (%+v)", len(agent.Calls), agent.Calls)
	}
	wantKinds := []string{"skill", "answerer", "skill", "commit", "skill"}
	for i, want := range wantKinds {
		if agent.Calls[i].Kind != want {
			t.Errorf("call[%d].Kind=%q want=%q (prompt=%q)", i, agent.Calls[i].Kind, want, agent.Calls[i].Prompt)
		}
	}
	// 2nd impl skill (call[2]) は session A を resume している
	if agent.Calls[2].ResumeSessionID == "" {
		t.Errorf("2nd impl skill 呼び出しは resume session ID が non-empty であるべき: got=%q", agent.Calls[2].ResumeSessionID)
	}

	// 初回 skill (call[0], `ResumeSessionID == ""`) は `/kiro-impl <feature> <taskID>` で始まる。
	initialPrompt := agent.Calls[0].Prompt
	if !strings.HasPrefix(initialPrompt, "/kiro-impl "+feature) {
		t.Errorf("初回 skill prompt は `/kiro-impl %s` で始まるべき: got head=%q",
			feature, initialPrompt[:min(len(initialPrompt), 64)])
	}

	// 永続化ファイルの確認: <run-id>/01-impl-1/round-1-{inquiry,answer}.md
	runRoot := filepath.Join(cwd, ".rilll", "kiro-impl-loop")
	entries, err := os.ReadDir(runRoot)
	if err != nil {
		t.Fatalf("ReadDir runRoot: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("runRoot に唯一の run-id ディレクトリを期待: got=%d", len(entries))
	}
	runDir := filepath.Join(runRoot, entries[0].Name())
	stepDir := filepath.Join(runDir, "01-impl-1")
	for _, suffix := range []string{"round-1-inquiry.md", "round-1-answer.md"} {
		path := filepath.Join(stepDir, suffix)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("永続化ファイル %s が見つからない: %v", path, err)
		}
	}

	// resume 呼び出しの prompt 本文に
	// "Source: <runDir>/01-impl-1/round-1-answer.md" 行が含まれることを確認する。
	// AnswerSourcePathFunc が Engine に供給されている seam の存在検証であり、
	// inquiryloop.Engine 側の formatResumeBody が "Source: " プレフィックスを付ける契約に依存する。
	expectedAnswerPath := filepath.Join(stepDir, "round-1-answer.md")
	resumePrompt := agent.Calls[2].Prompt
	if !strings.Contains(resumePrompt, "Source: "+expectedAnswerPath) {
		t.Errorf("resume prompt が Source: <answer path> を含まない (Phase A 未適用の疑い): want substring=%q\nprompt=%q",
			"Source: "+expectedAnswerPath, resumePrompt)
	}
}

// TestKiroImplLoopInquiry_AnswerContextWiring: 1 round シナリオで答え側 (answerer) 呼び出しの
// prompt に AnswerContext (元 PromptCommand + spec ファイル絶対パス) が `# Context` ブロックとして
// 注入されていることを検証する。
//
// 配線契約:
//   - answerer prompt に `# Context` セクションが現れること
//   - 元 PromptCommand `/kiro-impl <feature> <taskID>` が含まれること
//   - .kiro/specs/<feature>/{requirements,design,tasks}.md の絶対パスが含まれること
//   - end-of-prompt 末尾に runner-directive が付加されていること (session A と対称化)
func TestKiroImplLoopInquiry_AnswerContextWiring(t *testing.T) {
	feature := "demo"
	cwd := setupImplFeatureCWD(t, feature, "- [ ] 1. 単独タスク\n")
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
			if strings.HasPrefix(call.Prompt, "/kiro-validate-impl") {
				return implFinalReviewOKResp(), nil
			}
			if call.ResumeSessionID == "" {
				return skillInquiryResp(), nil
			}
			return skillNoInquiryResp(), nil
		}
		return provider.AgentResponse{}, nil
	}

	if err := runKiroImplLoop(
		context.Background(),
		cwd,
		onEvent,
		ask,
		agent,
	); err != nil {
		t.Fatalf("runKiroImplLoop returned error: %v", err)
	}

	// answerer 呼び出しは call[1] (skill, answerer, skill, commit, final) の順
	if len(agent.Calls) < 2 || agent.Calls[1].Kind != "answerer" {
		t.Fatalf("call[1] が answerer であることを期待: got=%+v", agent.Calls)
	}
	answerPrompt := agent.Calls[1].Prompt

	if !strings.Contains(answerPrompt, "# Context") {
		t.Errorf("answerer prompt は `# Context` ブロックを含むべき:\n%s", answerPrompt)
	}
	wantCmd := "/kiro-impl " + feature + " 1."
	if !strings.Contains(answerPrompt, wantCmd) {
		t.Errorf("answerer prompt は元 PromptCommand %q を含むべき:\n%s", wantCmd, answerPrompt)
	}
	specDir := filepath.Join(cwd, ".kiro", "specs", feature)
	for _, want := range []string{
		filepath.Join(specDir, "requirements.md"),
		filepath.Join(specDir, "design.md"),
		filepath.Join(specDir, "tasks.md"),
	} {
		if !strings.Contains(answerPrompt, want) {
			t.Errorf("answerer prompt は spec パス %q を含むべき:\n%s", want, answerPrompt)
		}
	}
	if !strings.Contains(answerPrompt, "[runner-directive]") {
		t.Errorf("answerer prompt 末尾は runner-directive を含むべき (session A と対称化):\n%s", answerPrompt)
	}
	for _, want := range []string{
		"# Policy",
		"[policy:ai-antipattern-guard]",
		"配線忘れ",
		"スコープクリープ",
	} {
		if !strings.Contains(answerPrompt, want) {
			t.Errorf("answerer prompt は ai-antipattern guard policy marker %q を含むべき:\n%s", want, answerPrompt)
		}
	}
	if strings.Contains(answerPrompt, "embed:facets/policies/ai-antipattern-guard.md") {
		t.Errorf("answerer prompt は policy SourcePath を LLM-visible に漏らしてはならない:\n%s", answerPrompt)
	}
}

// TestKiroImplLoopInquiry_InquiryDirectiveWiring: kiro-impl-loop が inquiry-skill 側
// (session A = /kiro-impl) に対し implLoopInquiryDirective を配線していることを、
// 1 round シナリオの初回 skill call と resume skill call 双方の prompt で検証する。
//
// 配線契約 (Engine.InquiryDirective を通じた第 3 層防御):
//   - 初回 skill call の prompt に "[loop-policy]" マーカーが含まれる
//   - resume skill call の prompt にも "[loop-policy]" マーカーが含まれる (long-context drift 対策)
//   - directive 本文に「scope 拡張」言及が含まれる (= scope 越え inquiry を抑止する旨)
func TestKiroImplLoopInquiry_InquiryDirectiveWiring(t *testing.T) {
	feature := "demo"
	cwd := setupImplFeatureCWD(t, feature, "- [ ] 1. 単独タスク\n")
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
			if strings.HasPrefix(call.Prompt, "/kiro-validate-impl") {
				return implFinalReviewOKResp(), nil
			}
			if call.ResumeSessionID == "" {
				return skillInquiryResp(), nil
			}
			return skillNoInquiryResp(), nil
		}
		return provider.AgentResponse{}, nil
	}

	if err := runKiroImplLoop(
		context.Background(),
		cwd,
		onEvent,
		ask,
		agent,
	); err != nil {
		t.Fatalf("runKiroImplLoop returned error: %v", err)
	}

	// 1 round シナリオの呼び出し列: skill (initial) / answerer / skill (resume) / commit / final
	if len(agent.Calls) < 3 {
		t.Fatalf("少なくとも skill+answerer+skill の 3 件を期待: got=%+v", agent.Calls)
	}
	initialPrompt := agent.Calls[0].Prompt
	resumePrompt := agent.Calls[2].Prompt

	if !strings.Contains(initialPrompt, "[loop-policy]") {
		t.Errorf("初回 skill prompt は [loop-policy] マーカーを含むべき:\n%s", initialPrompt)
	}
	if !strings.Contains(initialPrompt, "scope 拡張") {
		t.Errorf("初回 skill prompt は loop-policy 本文 (scope 拡張禁止) を含むべき:\n%s", initialPrompt)
	}
	if !strings.Contains(resumePrompt, "[loop-policy]") {
		t.Errorf("resume skill prompt は [loop-policy] マーカーを含むべき (drift 対策):\n%s", resumePrompt)
	}

	// answerer 側 (call[1]) には loop-policy は付加しない (answer 側は AnswerContext +
	// answerPolicySection + persona でガード済み)。
	answerPrompt := agent.Calls[1].Prompt
	if strings.Contains(answerPrompt, "[loop-policy]") {
		t.Errorf("answerer prompt は [loop-policy] を含まないべき (別レイヤでガード済み):\n%s", answerPrompt)
	}

	// 初回 skill prompt は `/kiro-impl <feature> <taskID>` で始まる。
	if !strings.HasPrefix(initialPrompt, "/kiro-impl "+feature) {
		t.Errorf("初回 skill prompt は `/kiro-impl %s` で始まるべき: got head=%q",
			feature, initialPrompt[:min(len(initialPrompt), 64)])
	}
}

// TestKiroImplLoopInquiry_SchemaViolationAbort: impl skill が StructuredOutput=nil かつ
// Content も JSON として decode できない応答を返したときに、schema-violation で abort し
// workflow.WorkflowError 発火 + commit 呼び出し抑止される。
//
// 初回 schema-violation でも inquiryloop.Engine は schemaRepairDirective() で 1 回 retry するため、
// repair も schema-violation のケース (= 最終的に abort) を再現する。skill + skill-repair の
// 2 件で停止し、commit には進まないことを検証する。
func TestKiroImplLoopInquiry_SchemaViolationAbort(t *testing.T) {
	feature := "demo"
	cwd := setupImplFeatureCWD(t, feature, "- [ ] 1. 未完\n")
	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Selected: feature}

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		// success=true だが StructuredOutput=nil + Content も JSON decode 不能。
		// inquiryloop.ParseResponse が ErrSchemaViolation を返す。
		// repair retry も同じ応答を返すことで最終的に schema-violation abort させる。
		return provider.AgentResponse{
			Success: true,
			Content: "plain text without any JSON",
		}, nil
	}

	if err := runKiroImplLoop(
		context.Background(),
		cwd,
		onEvent,
		ask,
		agent,
	); err != nil {
		t.Fatalf("runKiroImplLoop returned error: %v", err)
	}

	// 初回 skill + 1 回の skill-repair = 2 件。commit には進まないこと。
	if len(agent.Calls) != 2 {
		t.Fatalf("schema-violation で abort 後は impl skill + repair = 2 件で停止すべき: got=%d (%+v)", len(agent.Calls), agent.Calls)
	}
	if agent.Calls[0].Kind != "skill" {
		t.Errorf("1 件目は impl skill であるべき: got kind=%q", agent.Calls[0].Kind)
	}
	if agent.Calls[1].Kind != "skill" {
		t.Errorf("2 件目 (repair retry) も skill 系統であるべき: got kind=%q", agent.Calls[1].Kind)
	}
	if !hasErrorEventAll(*eventsPtr, "1", "schema-violation") {
		t.Errorf("workflow.WorkflowError event は task ID と schema-violation を同時包含すべき: %+v", *eventsPtr)
	}
}

// TestKiroImplLoopInquiry_PromptFailureAbort: impl skill が success=false を返したときに
// prompt-failure で abort し commit 呼び出し抑止される。
func TestKiroImplLoopInquiry_PromptFailureAbort(t *testing.T) {
	feature := "demo"
	cwd := setupImplFeatureCWD(t, feature, "- [ ] 1. 未完\n")
	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Selected: feature}

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		return provider.AgentResponse{
			Success: false,
			Content: "agent rejected",
		}, nil
	}

	if err := runKiroImplLoop(
		context.Background(),
		cwd,
		onEvent,
		ask,
		agent,
	); err != nil {
		t.Fatalf("runKiroImplLoop returned error: %v", err)
	}

	if len(agent.Calls) != 1 {
		t.Fatalf("prompt-failure で abort 後は impl skill のみ 1 件で停止すべき: got=%d", len(agent.Calls))
	}
	if !hasErrorEventAll(*eventsPtr, "1", "prompt-failure") {
		t.Errorf("workflow.WorkflowError event は task ID と prompt-failure を同時包含すべき: %+v", *eventsPtr)
	}
}

// TestKiroImplLoopInquiry_LoopLimitAbort: impl skill が常に has_inquiry=true を返し続け、
// MaxRounds 既定 (5) を超過するときに loop-limit で abort する。
//
// scriptedAgent.Respond は impl skill 呼び出しのたびに has_inquiry=true を返し、
// answerer は has_inquiry=false (回答テキスト)。これで初回 + 5 ラウンド = 6 回の
// impl skill 呼び出しが起き、5 ラウンド経過時点でループ上限超過。
func TestKiroImplLoopInquiry_LoopLimitAbort(t *testing.T) {
	feature := "demo"
	cwd := setupImplFeatureCWD(t, feature, "- [ ] 1. 未完\n")
	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Selected: feature}

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "answerer":
			return answerOKResp(), nil
		case "skill":
			// 常に has_inquiry=true を返し続ける → 5 ラウンド経過で loop-limit
			return skillInquiryResp(), nil
		}
		return provider.AgentResponse{}, nil
	}

	if err := runKiroImplLoop(
		context.Background(),
		cwd,
		onEvent,
		ask,
		agent,
	); err != nil {
		t.Fatalf("runKiroImplLoop returned error: %v", err)
	}

	// 初回 skill + 5 ラウンド × (answerer + skill) = 1 + 10 = 11 件
	if len(agent.Calls) != 11 {
		t.Fatalf("loop-limit 経路では 1 + 5×2 = 11 件の呼び出しを期待: got=%d", len(agent.Calls))
	}
	commitCount := 0
	for _, c := range agent.Calls {
		if c.Kind == "commit" {
			commitCount++
		}
	}
	if commitCount != 0 {
		t.Errorf("loop-limit abort 後は /git-commit が呼ばれてはならない: got=%d", commitCount)
	}
	if !hasErrorEventAll(*eventsPtr, "1", "loop-limit") {
		t.Errorf("workflow.WorkflowError event は task ID と loop-limit を同時包含すべき: %+v", *eventsPtr)
	}
}

// TestFormatImplAbortMessage は formatImplAbortMessage の文字列フォーマットを assert する。
// FailureDetail.Message が空のときと non-empty のときの 2 ケース。
func TestFormatImplAbortMessage(t *testing.T) {
	t.Run("with failure message", func(t *testing.T) {
		got := formatImplAbortMessage("3.2", finalResultWithReason("loop-limit", "did not converge"))
		want := "中断: [3.2] [loop-limit] 実装エージェントが完了しませんでした（did not converge）"
		if got != want {
			t.Errorf("formatImplAbortMessage mismatch:\ngot=%q\nwant=%q", got, want)
		}
	})
	t.Run("without failure message", func(t *testing.T) {
		got := formatImplAbortMessage("1", finalResultWithReason("schema-violation", ""))
		want := "中断: [1] [schema-violation] 実装エージェントが完了しませんでした"
		if got != want {
			t.Errorf("formatImplAbortMessage mismatch:\ngot=%q\nwant=%q", got, want)
		}
	})
}

// TestSanitizePhaseSegment は sanitizePhaseSegment が phase 識別子のドットを '_' に
// 置換することを assert する (永続化先パスの可読性確保)。
func TestSanitizePhaseSegment(t *testing.T) {
	cases := map[string]string{
		"1":      "1",
		"1.2":    "1_2",
		"1.2.3":  "1_2_3",
		"foo":    "foo",
		"a.b":    "a_b",
		".":      "_",
		"":       "",
		"a..b":   "a__b",
		"a.b.c.": "a_b_c_",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if got := sanitizePhaseSegment(in); got != want {
				t.Errorf("sanitizePhaseSegment(%q) = %q, want %q", in, got, want)
			}
		})
	}
}
