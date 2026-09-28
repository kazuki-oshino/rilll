// Package kiro の本テストファイルは specLoopRunner のフェーズ別ハンドラに対する
// ユニットテストを集約する。
//
// 既存の spec_loop_run_test.go / spec_loop_test.go は runKiroSpecLoop を 1 回駆動して
// end-to-end 挙動を検証する統合テスト群。本ファイルは specLoopRunner の各メソッドを
// 直接呼び、フェーズ単位の挙動を孤立して検証する（issue #24「フェーズ単位でユニットテスト可能に」）。
package kiro

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/facet"
	"github.com/kazuki-oshino/rilll/internal/provider"
)

// TestSpecLoopRunner_NewInitializesState は newSpecLoopRunner が
// state.RunID / state.StartedAt / runDir を正しく初期化することを検証する。
func TestSpecLoopRunner_NewInitializesState(t *testing.T) {
	cwd := t.TempDir()
	r := newSpecLoopRunner(
		context.Background(),
		cwd,
		nil,
		nil,
		nil,
	)
	if r.state == nil {
		t.Fatalf("state は non-nil で初期化されるべき")
	}
	if r.state.StartedAt.IsZero() {
		t.Errorf("state.StartedAt は現在時刻で初期化されるべき: got=%v", r.state.StartedAt)
	}
	wantRunDir := filepath.Join(cwd, ".rilll", "kiro-spec-loop", r.state.RunID)
	if r.runDir != wantRunDir {
		t.Errorf("runDir mismatch:\n got=%q\nwant=%q", r.runDir, wantRunDir)
	}
}

// TestSpecLoopRunner_Setup_MissingFeature は setup() で eligible feature 0 件のとき
// done=true / err=nil で missing-feature abort が確定することを検証する。
func TestSpecLoopRunner_Setup_MissingFeature(t *testing.T) {
	cwd := t.TempDir() // .kiro/specs/ 自体が無い → eligible 0 件
	onEvent, eventsPtr := collectEvents()
	agent := &recordedAgent{}
	r := newSpecLoopRunner(
		context.Background(),
		cwd,
		onEvent,
		&fakeAsk{Selected: "shouldnt-be-asked"},
		agent,
	)
	done, err := r.setup()
	if err != nil {
		t.Fatalf("err は nil を期待: got=%v", err)
	}
	if !done {
		t.Errorf("missing-feature abort 確定で done=true を期待")
	}
	if r.state.Termination != RunReasonMissingFeature {
		t.Errorf("state.Termination=missing-feature を期待: got=%q", r.state.Termination)
	}
	if len(agent.Calls) != 0 {
		t.Errorf("setup 段階では agent.CallAgent は呼ばれない: got=%d calls", len(agent.Calls))
	}
	if !hasErrorEvent(*eventsPtr, "brief.md") && !hasErrorEvent(*eventsPtr, "/kiro-discovery") {
		t.Errorf("missing-feature event は案内文言を含むべき: %+v", *eventsPtr)
	}
}

// TestSpecLoopRunner_Setup_Cancelled は setup() で feature 選択 ESC キャンセル時に
// done=true / err=nil で cancelled-by-user abort が確定することを検証する。
func TestSpecLoopRunner_Setup_Cancelled(t *testing.T) {
	cwd := setupFeatureCWD(t, "demo", "Brief 本文", "")
	onEvent, _ := collectEvents()
	r := newSpecLoopRunner(
		context.Background(),
		cwd,
		onEvent,
		&fakeAsk{Cancelled: true},
		&recordedAgent{},
	)
	done, err := r.setup()
	if err != nil {
		t.Fatalf("err は nil を期待: got=%v", err)
	}
	if !done {
		t.Errorf("cancelled abort 確定で done=true を期待")
	}
	if r.state.Termination != RunReasonCancelledByUser {
		t.Errorf("state.Termination=cancelled-by-user を期待: got=%q", r.state.Termination)
	}
}

// TestSpecLoopRunner_Setup_Success は setup() の正常系で派生状態（feature / persona /
// briefAbsPath / specJSONPath / startOrder）が確定することを検証する。
func TestSpecLoopRunner_Setup_Success(t *testing.T) {
	feature := "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	r := newSpecLoopRunner(
		context.Background(),
		cwd,
		nil,
		&fakeAsk{Selected: feature},
		&recordedAgent{},
	)
	done, err := r.setup()
	if err != nil {
		t.Fatalf("setup error: %v", err)
	}
	if done {
		t.Errorf("正常系では done=false を期待")
	}
	if r.state.Feature != feature {
		t.Errorf("state.Feature mismatch: got=%q want=%q", r.state.Feature, feature)
	}
	if r.persona.Name == "" {
		t.Errorf("persona は解決済みであるべき: got=%+v", r.persona)
	}
	if !strings.HasSuffix(r.briefAbsPath, filepath.Join(feature, "brief.md")) {
		t.Errorf("briefAbsPath が期待 suffix を持たない: %q", r.briefAbsPath)
	}
	if r.startOrder != 1 {
		t.Errorf("spec.json 不在では startOrder=1 を期待: got=%d", r.startOrder)
	}
	// 00-context.json が書き出されていること
	ctxPath := filepath.Join(r.runDir, "00-context.json")
	if _, err := os.Stat(ctxPath); err != nil {
		t.Errorf("00-context.json が書き出されていない: %v", err)
	}
}

// TestSpecLoopRunner_Setup_DesignApprovedSetsStartOrder6 は
// approvals.design.approved=true の spec.json を置くと startOrder=6 になることを検証する。
func TestSpecLoopRunner_Setup_DesignApprovedSetsStartOrder6(t *testing.T) {
	feature := "resume-tasks"
	// listEligibleFeatures は phase < tasks-generated を要求するため、design-generated を指定する。
	specJSON := `{"phase":"design-generated","approvals":{"design":{"approved":true}}}`
	cwd := setupFeatureCWD(t, feature, "Brief 本文", specJSON)
	r := newSpecLoopRunner(
		context.Background(),
		cwd,
		nil,
		&fakeAsk{Selected: feature},
		&recordedAgent{},
	)
	done, err := r.setup()
	if err != nil || done {
		t.Fatalf("setup unexpected outcome: done=%v err=%v", done, err)
	}
	if r.startOrder != 6 {
		t.Errorf("design.approved=true では startOrder=6 を期待: got=%d", r.startOrder)
	}
}

// TestSpecLoopRunner_ApplyApprovalForPhase は applyApprovalForPhase が
// validate-gap / validate-design / spec-tasks で spec.json に承認フラグを書き、
// それ以外 (spec-init / spec-requirements / spec-design) は no-op であることを検証する。
func TestSpecLoopRunner_ApplyApprovalForPhase(t *testing.T) {
	tmp := t.TempDir()
	specJSONPath := filepath.Join(tmp, "spec.json")
	body := `{
  "approvals": {
    "requirements": {"approved": false},
    "design": {"approved": false},
    "tasks": {"approved": false}
  }
}`
	if err := os.WriteFile(specJSONPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write spec.json: %v", err)
	}
	r := &specLoopRunner{specJSONPath: specJSONPath}

	// validate-gap → requirements 承認
	r.applyApprovalForPhase("validate-gap")
	if got := mustReadApproved(t, specJSONPath, "requirements"); !got {
		t.Errorf("validate-gap 後は requirements.approved=true を期待")
	}
	if got := mustReadApproved(t, specJSONPath, "design"); got {
		t.Errorf("validate-gap 後は design.approved=false のままを期待")
	}
	if got := mustReadApproved(t, specJSONPath, "tasks"); got {
		t.Errorf("validate-gap 後は tasks.approved=false のままを期待")
	}

	// validate-design → design 承認
	r.applyApprovalForPhase("validate-design")
	if got := mustReadApproved(t, specJSONPath, "design"); !got {
		t.Errorf("validate-design 後は design.approved=true を期待")
	}
	if got := mustReadApproved(t, specJSONPath, "tasks"); got {
		t.Errorf("validate-design 後は tasks.approved=false のままを期待")
	}

	// spec-tasks → tasks 承認
	r.applyApprovalForPhase("spec-tasks")
	if got := mustReadApproved(t, specJSONPath, "tasks"); !got {
		t.Errorf("spec-tasks 後は tasks.approved=true を期待")
	}

	// 承認対象外 phase は no-op（既存承認状態を変えない）
	r.applyApprovalForPhase("spec-init")
	r.applyApprovalForPhase("spec-requirements")
	r.applyApprovalForPhase("spec-design")
	if !mustReadApproved(t, specJSONPath, "requirements") ||
		!mustReadApproved(t, specJSONPath, "design") ||
		!mustReadApproved(t, specJSONPath, "tasks") {
		t.Errorf("承認対象外 phase は承認フラグを変えてはならない")
	}
}

// mustReadApproved は spec.json の approvals.<key>.approved を読み出すテストヘルパ。
func mustReadApproved(t *testing.T, specJSONPath, key string) bool {
	t.Helper()
	raw, err := os.ReadFile(specJSONPath)
	if err != nil {
		t.Fatalf("read spec.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse spec.json: %v", err)
	}
	approvals, _ := m["approvals"].(map[string]any)
	entry, _ := approvals[key].(map[string]any)
	approved, _ := entry["approved"].(bool)
	return approved
}

// TestSpecLoopRunner_RunCommit は runCommit が
//   - prompt="/git-commit"
//   - resume="" (fresh session)
//   - StepID="commit-NN-<phase>" / StepName="NN-<phase>-commit"
//
// で agent.CallAgent を呼ぶことを検証する。
func TestSpecLoopRunner_RunCommit(t *testing.T) {
	cwd := t.TempDir()
	agent := &recordedAgent{
		CommitResponse: provider.AgentResponse{
			Success:   true,
			Content:   "commit ok",
			SessionID: "session-commit",
		},
	}
	r := newSpecLoopRunner(
		context.Background(),
		cwd,
		nil,
		nil,
		agent,
	)
	step := promptStep{Order: 3, Phase: "validate-gap"}
	resp, err := r.runCommit(step, "03-validate-gap")
	if err != nil {
		t.Fatalf("runCommit error: %v", err)
	}
	if !resp.Success || resp.Content != "commit ok" {
		t.Errorf("response unexpected: %+v", resp)
	}
	if len(agent.Calls) != 1 {
		t.Fatalf("agent.CallAgent 呼び出し件数 1 を期待: got=%d", len(agent.Calls))
	}
	c := agent.Calls[0]
	if c.Prompt != "/git-commit" {
		t.Errorf("prompt mismatch: got=%q want=%q", c.Prompt, "/git-commit")
	}
	if c.ResumeSessionID != "" {
		t.Errorf("commit は fresh session であるべき: resume=%q", c.ResumeSessionID)
	}
	if c.Options == nil {
		t.Fatalf("options が nil")
	}
	if c.Options.StepID != "commit-03-validate-gap" {
		t.Errorf("StepID mismatch: got=%q", c.Options.StepID)
	}
	if c.Options.StepName != "03-validate-gap-commit" {
		t.Errorf("StepName mismatch: got=%q", c.Options.StepName)
	}
	assertGitCommitProviderOverrides(t, c.Options)
}

// TestSpecLoopRunner_Abort は abort() が state を確定し result.json + Error event を発火することを検証する。
func TestSpecLoopRunner_Abort(t *testing.T) {
	cwd := t.TempDir()
	onEvent, eventsPtr := collectEvents()
	r := newSpecLoopRunner(
		context.Background(),
		cwd,
		onEvent,
		nil,
		nil,
	)
	abortStep := &RunResultAbortStep{Order: 2, Phase: "spec-requirements"}
	if err := r.abort(RunReasonLoopLimit, abortStep, "test abort"); err != nil {
		t.Fatalf("abort error: %v", err)
	}
	if r.state.Termination != RunReasonLoopLimit {
		t.Errorf("Termination mismatch: got=%q", r.state.Termination)
	}
	if r.state.AbortedAt == nil {
		t.Errorf("AbortedAt が nil")
	}
	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonLoopLimit {
		t.Errorf("result.json terminationReason mismatch: got=%q", rr.TerminationReason)
	}
	if rr.AbortStep == nil || rr.AbortStep.Order != 2 || rr.AbortStep.Phase != "spec-requirements" {
		t.Errorf("AbortStep mismatch: got=%+v", rr.AbortStep)
	}
	if !hasErrorEvent(*eventsPtr, "test abort") {
		t.Errorf("Error event に message が含まれるべき: %+v", *eventsPtr)
	}
}

// TestSpecLoopRunner_Complete は complete() が state を確定し result.json + Done event を発火することを検証する。
func TestSpecLoopRunner_Complete(t *testing.T) {
	cwd := t.TempDir()
	onEvent, eventsPtr := collectEvents()
	r := newSpecLoopRunner(
		context.Background(),
		cwd,
		onEvent,
		nil,
		nil,
	)
	r.state.Feature = "demo"
	if err := r.complete(); err != nil {
		t.Fatalf("complete error: %v", err)
	}
	if r.state.Termination != RunReasonCompleted {
		t.Errorf("Termination mismatch: got=%q", r.state.Termination)
	}
	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonCompleted {
		t.Errorf("result.json terminationReason mismatch: got=%q", rr.TerminationReason)
	}
	if rr.CompletedAt == nil {
		t.Errorf("CompletedAt が nil")
	}
	if !hasDoneEvent(*eventsPtr, "demo") {
		t.Errorf("Done event に feature 名が含まれるべき: %+v", *eventsPtr)
	}
}

// TestSpecLoopRunner_RunStepHandlesCommitFailure は runStep() が commit 失敗を検知して
// commit-failure abort が確定することを、setup を通したフルパスで検証する。
//
// 直接 runStep() を呼んで abort 経路を駆動する。setup() は事前に呼んで派生状態を整える。
func TestSpecLoopRunner_RunStepHandlesCommitFailure(t *testing.T) {
	feature := "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	onEvent, eventsPtr := collectEvents()
	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "commit":
			return commitFailResp(), nil
		case "skill":
			return skillNoInquiryResp(), nil
		}
		return provider.AgentResponse{}, nil
	}
	r := newSpecLoopRunner(
		context.Background(),
		cwd,
		onEvent,
		&fakeAsk{Selected: feature},
		agent,
	)
	if done, err := r.setup(); err != nil || done {
		t.Fatalf("setup unexpected outcome: done=%v err=%v", done, err)
	}
	aborted, err := r.runStep(steps[0])
	if err != nil {
		t.Fatalf("runStep error: %v", err)
	}
	if !aborted.Aborted {
		t.Errorf("commit 失敗で aborted=true を期待")
	}
	if r.state.Termination != RunReasonCommitFailure {
		t.Errorf("Termination=commit-failure を期待: got=%q", r.state.Termination)
	}
	if !hasErrorEventAll(*eventsPtr, "/git-commit", "spec-init") {
		t.Errorf("Error event は '/git-commit' と 'spec-init' を含むべき: %+v", *eventsPtr)
	}
}

// TestSpecLoopRunner_RunStepHandlesEngineAbort は runStep() が engine 由来 abort
// (schema-violation) を検知して state.Failure を確定することを検証する。
func TestSpecLoopRunner_RunStepHandlesEngineAbort(t *testing.T) {
	feature := "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	onEvent, _ := collectEvents()
	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		// step 1 初回 skill 応答: StructuredOutput=nil + JSON 抽出不能 Content → schema-violation
		return provider.AgentResponse{
			Success:   true,
			Content:   "プレーンテキスト応答（JSON ブロックなし）",
			SessionID: "session-skill",
		}, nil
	}
	r := newSpecLoopRunner(
		context.Background(),
		cwd,
		onEvent,
		&fakeAsk{Selected: feature},
		agent,
	)
	if done, err := r.setup(); err != nil || done {
		t.Fatalf("setup unexpected outcome: done=%v err=%v", done, err)
	}
	aborted, err := r.runStep(steps[0])
	if err != nil {
		t.Fatalf("runStep error: %v", err)
	}
	if !aborted.Aborted {
		t.Errorf("schema-violation で aborted=true を期待")
	}
	if r.state.Termination != RunReasonSchemaViolation {
		t.Errorf("Termination=schema-violation を期待: got=%q", r.state.Termination)
	}
	if r.state.Failure == nil {
		t.Errorf("engine 由来 abort では state.Failure が non-nil を期待")
	}
}

// TestSpecLoopRunner_RunStepMissingPhasePersona は task 5.2 の観察可能完了を駆動する:
//
//	(a) runStep が runInquiryLoop から伝搬した Persona 解決失敗
//	    (facet.ErrFacetNotFound) を識別し、RunReasonMissingPhasePersona で abort する
//	    こと (state.Termination / result.json.terminationReason / abortStep)
//	(b) WorkflowError event の message に対象 facet 名と error 内容が含まれること
//	(c) inquiry round に入らず即時 abort されるため
//	    runDir/<NN>-<phase>/round-*-inquiry.md / round-*-answer.md が
//	    一切書かれず、agent.CallAgent も呼ばれないこと
//
// fixture 戦略: embed (`internal/facet/facets/personas/`) にも override
// (`.rilll-config/facets/personas/`) にも存在しない名前を Persona に詰めた
// `promptStep` を直接 runStep に渡す。`.rilll-config/facets/personas/spec-analyst.md`
// を意図的に欠落させた上で steps テーブルの persona 名を差し替える tasks.md
// 5.2 の指示と同等の経路を、テストヘルパを増やさずに `runStep` 引数経由で再現する。
//
// 設計参照: Design §Components runStep / §Decisions 4
// (RunReasonMissingPersona と RunReasonMissingPhasePersona の semantics 分離)。
func TestSpecLoopRunner_RunStepMissingPhasePersona(t *testing.T) {
	feature := "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	onEvent, eventsPtr := collectEvents()
	agent := &scriptedAgent{} // Respond 未設定。万一呼ばれれば skillNoInquiryResp() を返すが、
	// 本テストでは agent.CallAgent が一度も呼ばれないこと自体を assertion する。
	r := newSpecLoopRunner(
		context.Background(),
		cwd,
		onEvent,
		&fakeAsk{Selected: feature},
		agent,
	)
	if done, err := r.setup(); err != nil || done {
		t.Fatalf("setup unexpected outcome: done=%v err=%v", done, err)
	}

	// embed / override どちらにも存在しない persona 名を組み込んだ step を駆動する。
	// `.rilll-config/facets/personas/` ディレクトリ自体を作成していないため
	// 2 層解決 (override → embed) は両方 miss し facet.ErrFacetNotFound が返る。
	missingPersona := "definitely-not-a-real-persona-xyz"
	badStep := promptStep{
		Order:   1,
		Phase:   "spec-init",
		Persona: missingPersona,
		Command: func(feature string, _ []string) string {
			return "/kiro-spec-init " + feature
		},
	}

	aborted, err := r.runStep(badStep)
	if err != nil {
		t.Fatalf("runStep error: %v", err)
	}
	if !aborted.Aborted {
		t.Fatalf("missing-phase-persona で aborted=true を期待")
	}

	// (a) state.Termination / result.json.terminationReason / abortStep
	if r.state.Termination != RunReasonMissingPhasePersona {
		t.Errorf("state.Termination=missing-phase-persona を期待: got=%q", r.state.Termination)
	}
	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonMissingPhasePersona {
		t.Errorf("result.json.terminationReason=missing-phase-persona を期待: got=%q", rr.TerminationReason)
	}
	if rr.AbortStep == nil || rr.AbortStep.Order != 1 || rr.AbortStep.Phase != "spec-init" {
		t.Errorf("abortStep=(1, spec-init) を期待: got=%+v", rr.AbortStep)
	}

	// (b) WorkflowError event の message に "missing-phase-persona" マーカ
	//     と対象 facet 名と underlying error 情報が含まれること
	if !hasErrorEventAll(*eventsPtr, "missing-phase-persona", missingPersona) {
		t.Errorf("Error event は 'missing-phase-persona' と %q を含むべき: %+v", missingPersona, *eventsPtr)
	}

	// (c) inquiry round に入らず即時 abort される:
	//     - <runDir>/01-spec-init/ 配下に round-* 永続化ファイルが存在しない
	//       (ディレクトリ自体が未作成でも ReadDir エラー → entries=nil で OK)
	//     - agent.CallAgent が一度も呼ばれていない (persona 解決失敗は inquiry エンジン起動前)
	stepDirPath := stepDir(r.runDir, 1, "spec-init")
	if entries, derr := os.ReadDir(stepDirPath); derr == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "round-") {
				t.Errorf("inquiry round に入らずに abort のはずなのに %q が生成された", e.Name())
			}
		}
	}
	if len(agent.Calls) != 0 {
		t.Errorf("persona 解決失敗時は agent 呼び出しが起きてはならない: got=%d calls", len(agent.Calls))
	}
}

// TestSpecLoopRunner_Setup_AskNilFallback は ask=nil のとき missing-feature 同等の
// abort が確定することを検証する（TUI なし起動経路はサポートしないという契約）。
func TestSpecLoopRunner_Setup_AskNilFallback(t *testing.T) {
	cwd := setupFeatureCWD(t, "demo", "Brief 本文", "")
	onEvent, _ := collectEvents()
	r := newSpecLoopRunner(
		context.Background(),
		cwd,
		onEvent,
		nil, // ask=nil
		&recordedAgent{},
	)
	done, err := r.setup()
	if err != nil {
		t.Fatalf("err は nil を期待: got=%v", err)
	}
	if !done {
		t.Errorf("ask=nil で done=true を期待")
	}
	if r.state.Termination != RunReasonMissingFeature {
		t.Errorf("Termination=missing-feature を期待: got=%q", r.state.Termination)
	}
}

// TestSpecLoopRunner_Setup_AskErrorPropagates は ask が error を返した場合に
// done=false / err=non-nil で propagate されることを検証する（中断ではなく異常）。
func TestSpecLoopRunner_Setup_AskErrorPropagates(t *testing.T) {
	cwd := setupFeatureCWD(t, "demo", "Brief 本文", "")
	wantErr := errors.New("simulated ask error")
	r := newSpecLoopRunner(
		context.Background(),
		cwd,
		nil,
		&fakeAsk{Err: wantErr},
		&recordedAgent{},
	)
	done, err := r.setup()
	if done {
		t.Errorf("ask error は abort ではなく異常として propagate すべき: got done=true")
	}
	if err == nil || !errors.Is(err, wantErr) {
		t.Errorf("ask error は wrap されて propagate されるべき: got=%v", err)
	}
}

// TestSpecLoopRunner_RunIntegratesPhases は Run() がフェーズ orchestrator として
// setup → step ループ → complete の順序で動作することを 1 段だけ実行可能な spec.json で検証する。
//
// approvals.design.approved=true の spec.json を置くと startOrder=6 になり、step は spec-tasks の
// 1 段だけになる。Run() 完了後に Done event が発火していれば orchestrator として動作している。
func TestSpecLoopRunner_RunIntegratesPhases(t *testing.T) {
	feature := "demo"
	// listEligibleFeatures は phase < tasks-generated を要求するため、design-generated を指定する。
	specJSON := `{"phase":"design-generated","approvals":{"design":{"approved":true}}}`
	cwd := setupFeatureCWD(t, feature, "Brief 本文", specJSON)
	onEvent, eventsPtr := collectEvents()
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
	r := newSpecLoopRunner(
		context.Background(),
		cwd,
		onEvent,
		&fakeAsk{Selected: feature},
		agent,
	)
	if err := r.Run(); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	// step 6 (spec-tasks) のみ実行 → skill 1 + commit 1 = 2 件
	if len(agent.Calls) != 2 {
		t.Errorf("startOrder=6 では skill 1 + commit 1 = 2 件を期待: got=%d", len(agent.Calls))
	}
	if !hasDoneEvent(*eventsPtr, feature) {
		t.Errorf("Done event に feature 名が含まれるべき: %+v", *eventsPtr)
	}
}

// TestPrevPhaseForOrder は prevPhaseForOrder 純関数が `steps` テーブルから
// order-1 に対応する phase 識別子を引く挙動を表駆動で固定する。
//
// 設計契約 (kiro-spec-loop-facets task 3.1 / Design §Components prevPhaseForOrder):
//   - order <= 1: 前段なし → ""
//   - 2 <= order <= 6: steps[order-2].Phase
//   - order > len(steps): 範囲外 → ""
//
// previousSummaryForStep の on-disk fallback パス組み立て
// (`runDir/<order-1:02d>-<prevPhase>/result.json`) で利用される (Req 1.1)。
// `steps` テーブル不変性に依存する純関数なので、同 input で同 output を返すことを担保する。
func TestPrevPhaseForOrder(t *testing.T) {
	cases := []struct {
		name  string
		order int
		want  string
	}{
		{name: "negative order", order: -1, want: ""},
		{name: "zero order", order: 0, want: ""},
		{name: "first phase has no previous", order: 1, want: ""},
		{name: "order 2 -> spec-init", order: 2, want: "spec-init"},
		{name: "order 3 -> spec-requirements", order: 3, want: "spec-requirements"},
		{name: "order 4 -> validate-gap", order: 4, want: "validate-gap"},
		{name: "order 5 -> spec-design", order: 5, want: "spec-design"},
		{name: "order 6 -> validate-design", order: 6, want: "validate-design"},
		{name: "out of range", order: 7, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := prevPhaseForOrder(tc.order)
			if got != tc.want {
				t.Errorf("prevPhaseForOrder(%d) = %q, want %q", tc.order, got, tc.want)
			}
		})
	}
}

// TestPersonaBlock は personaBlock 純関数が ResolvedFacet を
// `\n\n---\n\n# Persona\n\n<Content>` 形式 (append 用) に整形することを表駆動で固定する。
//
// 設計契約:
//   - 戻り値は常に `\n\n---\n\n# Persona\n\n` で始まる (skill コマンドからの区切り)
//   - resolved.Content が空文字でも形式は維持する (defensive)
//   - resolved.Content の前後 trim や改行正規化は行わず原文を埋め込む (純関数)
//
// slash-first 順を保つため skill コマンドの**後ろ**に連結する用途で使う。
// session A (RunInput.PromptCommand) でのみ使用し、session B (AnswerContext.PromptCommand)
// には適用しない (session B `buildAnswerPrompt` への persona 二重注入 / markdown bullet 崩れを回避)。
func TestPersonaBlock(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "short content",
			content: "spec-analyst persona",
			want:    "\n\n---\n\n# Persona\n\nspec-analyst persona",
		},
		{
			name:    "long content",
			content: strings.Repeat("a", 1500),
			want:    "\n\n---\n\n# Persona\n\n" + strings.Repeat("a", 1500),
		},
		{
			name:    "empty content",
			content: "",
			want:    "\n\n---\n\n# Persona\n\n",
		},
		{
			name:    "content with newlines",
			content: "line1\nline2\n\nline3",
			want:    "\n\n---\n\n# Persona\n\nline1\nline2\n\nline3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := personaBlock(facet.ResolvedFacet{Content: tc.content})
			if got != tc.want {
				t.Errorf("personaBlock(content=%q) = %q, want %q", tc.content, got, tc.want)
			}
		})
	}
}

// TestPreviousResponseBlock は previousResponseBlock 純関数が前段 summary を
// `\n\n---\n\n# Previous Response\n\n<content>` 形式 (append 用) に整形することを固定する。
//
// 設計契約:
//   - 空文字列 → 空文字列 (ブロック自体を省略する graceful degradation)
//   - 非空 (短文) → `\n\n---\n\n# Previous Response\n\n<content>`
//   - 非空 (長文 > 2000 rune) → PromptContextSummary kind では末尾切り詰め + truncated マーカ
//   - PromptContextDiagnostic kind では切り詰めなし (validator フィードバック注入用)
func TestPreviousResponseBlock(t *testing.T) {
	longContent := strings.Repeat("a", facet.CONTEXT_MAX_CHARS+50)
	cases := []struct {
		name string
		prev string
		kind facet.PromptContextKind
		want string
	}{
		{
			name: "empty -> empty (no block)",
			prev: "",
			kind: facet.PromptContextSummary,
			want: "",
		},
		{
			name: "short content (summary kind)",
			prev: "前段の summary",
			kind: facet.PromptContextSummary,
			want: "\n\n---\n\n# Previous Response\n\n前段の summary",
		},
		{
			name: "content with newlines preserved (summary kind)",
			prev: "line1\nline2",
			kind: facet.PromptContextSummary,
			want: "\n\n---\n\n# Previous Response\n\nline1\nline2",
		},
		{
			name: "long content truncated at CONTEXT_MAX_CHARS (summary kind)",
			prev: longContent,
			kind: facet.PromptContextSummary,
			want: "\n\n---\n\n# Previous Response\n\n" +
				strings.Repeat("a", facet.CONTEXT_MAX_CHARS) + "...(truncated)",
		},
		{
			name: "long content NOT truncated for diagnostic kind (retry feedback path)",
			prev: longContent,
			kind: facet.PromptContextDiagnostic,
			want: "\n\n---\n\n# Previous Response\n\n" + longContent,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := previousResponseBlock(tc.prev, tc.kind)
			if got != tc.want {
				t.Errorf("previousResponseBlock(prev_len=%d, kind=%v) = %q, want %q",
					len(tc.prev), tc.kind, got, tc.want)
			}
		})
	}
}

// TestPreviousSummaryForStep は previousSummaryForStep 二段フォールバック helper の
// 6 ケースを表駆動で固定する。
//
// 設計契約 (kiro-spec-loop-facets task 3.3 / Design §Testing Strategy Unit 1):
//
//	(a) step.Order == 1                                            → "" (前段なし)
//	(b) len(state.StepResults) > 0 & Order > 1                      → 末尾 Summary (in-memory hit、disk I/O 不要)
//	(c) StepResults 空 & Order > 1 & runDir/<order-1:02d>-<prevPhase>/result.json
//	    存在 & summary 非空                                          → on-disk summary (fallback hit)
//	(d) (c) 条件で result.json 不在                                    → "" (graceful)
//	(e) (c) 条件で JSON 不正                                          → "" (graceful)
//	(f) (c) 条件で result.json.summary == ""                          → ""
//
// graceful degradation: I/O 失敗 (ENOENT / 権限不足 / JSON 不正 / summary 空) は
// 呼び出し側に error として伝播せず "" に倒れて Req 1.4 経路 (前置なしで後段起動継続) に合流する。
//
// 要件: kiro-spec-loop-facets Req 1.1 / 1.2 / 1.4 / 5.3 / 5.4。
func TestPreviousSummaryForStep(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(t *testing.T, runDir string) (*runState, promptStep)
		wantSummary string
	}{
		{
			name: "a) Order==1 returns empty (no previous phase)",
			setup: func(t *testing.T, _ string) (*runState, promptStep) {
				return &runState{}, promptStep{Order: 1, Phase: "spec-init"}
			},
			wantSummary: "",
		},
		{
			name: "b) in-memory StepResults tail wins (no disk fixture)",
			setup: func(t *testing.T, _ string) (*runState, promptStep) {
				// runDir 配下に result.json を書かないことで、on-disk fallback 経路に降りないことを担保する。
				// in-memory hit パスは disk I/O 不要 (Design §Components previousSummaryForStep Invariants)。
				state := &runState{
					StepResults: []stepResult{
						{Order: 1, Phase: "spec-init", Summary: "in-memory-summary"},
					},
				}
				return state, promptStep{Order: 2, Phase: "spec-requirements"}
			},
			wantSummary: "in-memory-summary",
		},
		{
			name: "c) on-disk fallback hit when StepResults empty (restart path)",
			setup: func(t *testing.T, runDir string) (*runState, promptStep) {
				writeStepResultRaw(t, runDir, 3, "validate-gap",
					`{"order":3,"phase":"validate-gap","rounds":0,"lastSession":"","summary":"on-disk-fallback-summary","commitOK":true,"commitOutput":""}`)
				return &runState{}, promptStep{Order: 4, Phase: "spec-design"}
			},
			wantSummary: "on-disk-fallback-summary",
		},
		{
			name: "d) on-disk fallback miss (result.json absent) returns empty",
			setup: func(t *testing.T, _ string) (*runState, promptStep) {
				// result.json を書かない → ENOENT で graceful degradation
				return &runState{}, promptStep{Order: 4, Phase: "spec-design"}
			},
			wantSummary: "",
		},
		{
			name: "e) on-disk fallback malformed JSON returns empty (graceful)",
			setup: func(t *testing.T, runDir string) (*runState, promptStep) {
				writeStepResultRaw(t, runDir, 3, "validate-gap", "{not valid json[")
				return &runState{}, promptStep{Order: 4, Phase: "spec-design"}
			},
			wantSummary: "",
		},
		{
			name: "f) on-disk fallback empty summary returns empty",
			setup: func(t *testing.T, runDir string) (*runState, promptStep) {
				writeStepResultRaw(t, runDir, 3, "validate-gap",
					`{"order":3,"phase":"validate-gap","rounds":0,"lastSession":"","summary":"","commitOK":true,"commitOutput":""}`)
				return &runState{}, promptStep{Order: 4, Phase: "spec-design"}
			},
			wantSummary: "",
		},
		{
			// (g) PrevOverride[step.Order] が non-empty なら他経路を全てバイパスする。
			// validate-design retry が architect (step 4) に validator フィードバックを注入する経路。
			name: "g) PrevOverride preempts in-memory and on-disk paths",
			setup: func(t *testing.T, runDir string) (*runState, promptStep) {
				// in-memory にも on-disk にも別の summary を仕込み、PrevOverride が勝つことを確認
				writeStepResultRaw(t, runDir, 3, "validate-gap",
					`{"order":3,"phase":"validate-gap","rounds":0,"lastSession":"","summary":"on-disk-summary","commitOK":true,"commitOutput":""}`)
				state := &runState{
					StepResults: []stepResult{
						{Order: 3, Phase: "validate-gap", Summary: "in-memory-summary"},
					},
					PrevOverride: map[int]string{4: "validator-feedback-injected"},
				}
				return state, promptStep{Order: 4, Phase: "spec-design"}
			},
			wantSummary: "validator-feedback-injected",
		},
		{
			// (h) PrevOverride に対象 Order のキーが無い場合は既存ロジック (in-memory hit) に倒れる。
			name: "h) PrevOverride miss falls back to in-memory hit",
			setup: func(t *testing.T, _ string) (*runState, promptStep) {
				state := &runState{
					StepResults: []stepResult{
						{Order: 3, Phase: "validate-gap", Summary: "in-memory-summary"},
					},
					PrevOverride: map[int]string{5: "for-step-5-only"}, // 注: step 4 のキー無し
				}
				return state, promptStep{Order: 4, Phase: "spec-design"}
			},
			wantSummary: "in-memory-summary",
		},
		{
			// (i) PrevOverride[step.Order] が空文字なら override なしと同じ扱い (graceful)。
			name: "i) PrevOverride empty value behaves as miss",
			setup: func(t *testing.T, _ string) (*runState, promptStep) {
				state := &runState{
					StepResults: []stepResult{
						{Order: 3, Phase: "validate-gap", Summary: "in-memory-summary"},
					},
					PrevOverride: map[int]string{4: ""},
				}
				return state, promptStep{Order: 4, Phase: "spec-design"}
			},
			wantSummary: "in-memory-summary",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runDir := t.TempDir()
			state, step := tc.setup(t, runDir)
			got := previousSummaryForStep(state, step, runDir)
			if got != tc.wantSummary {
				t.Errorf("previousSummaryForStep(...) = %q, want %q", got, tc.wantSummary)
			}
		})
	}
}

// writeStepResultRaw は runDir/<order:02d>-<phase>/result.json に任意のバイト列を書き出すテストヘルパ。
// 正常な JSON でも JSON 不正 fixture でも使えるよう生バイト書き込みに統一している。
// パス組み立ては本体と一致させるため stepDir() を再利用する (= 既存永続化規約 Req 3.2 と一致)。
func writeStepResultRaw(t *testing.T, runDir string, order int, phase, body string) {
	t.Helper()
	dir := stepDir(runDir, order, phase)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write result.json: %v", err)
	}
}

// TestSpecPathsForPhase は phase ごとに参照させたい spec ファイルパスのマッピングを固定する。
//
// 設計契約 (回答エージェントが phase ごとに必要十分な情報を取れるよう絞った):
//   - spec-init        : brief.md
//   - spec-requirements: brief.md, requirements.md
//   - validate-gap     : brief.md, requirements.md
//   - spec-design      : brief.md, requirements.md, design.md
//   - validate-design  : requirements.md, design.md
//   - spec-tasks       : requirements.md, design.md, tasks.md
//   - 未知 phase       : nil (空)
//
// design 段以降で brief.md が落ちるのは、要求は requirements.md に圧縮済みで brief.md に
// 戻る判断は本来 spec-loop の手前 (discovery) でしか起きないため。
func TestSpecPathsForPhase(t *testing.T) {
	specDir := "/abs/specdir"
	briefPath := "/abs/specdir/brief.md"
	cases := []struct {
		phase string
		want  []string
	}{
		{"spec-init", []string{briefPath}},
		{"spec-requirements", []string{briefPath, "/abs/specdir/requirements.md"}},
		{"validate-gap", []string{briefPath, "/abs/specdir/requirements.md"}},
		{"spec-design", []string{briefPath, "/abs/specdir/requirements.md", "/abs/specdir/design.md"}},
		{"validate-design", []string{"/abs/specdir/requirements.md", "/abs/specdir/design.md"}},
		{"spec-tasks", []string{"/abs/specdir/requirements.md", "/abs/specdir/design.md", "/abs/specdir/tasks.md"}},
		{"unknown-phase", nil},
	}
	for _, tc := range cases {
		t.Run(tc.phase, func(t *testing.T) {
			got := specPathsForPhase(specDir, briefPath, tc.phase)
			if len(got) != len(tc.want) {
				t.Fatalf("specPathsForPhase(%q) len=%d want=%d (got=%v)", tc.phase, len(got), len(tc.want), got)
			}
			for i, w := range tc.want {
				if got[i] != w {
					t.Errorf("specPathsForPhase(%q)[%d]=%q want=%q", tc.phase, i, got[i], w)
				}
			}
		})
	}
}

// TestSpecLoopRunner_AnswerContextWiring は specLoopRunner.runInquiryLoop が
// inquiryloop.Engine.AnswerContext に元 PromptCommand と phase ごとの spec パスを
// 配線できていることを、spec-init で 1 ラウンド回した answerer 呼び出しの prompt 内容で検証する。
//
// 配線契約:
//   - answerer prompt に `# Context` ブロックが現れる
//   - 元 PromptCommand `/kiro-spec-init <feature>` が含まれる
//   - spec-init phase に対応する brief.md 絶対パスが含まれる
//   - 末尾 runner-directive が付加されている (session A と対称化)
func TestSpecLoopRunner_AnswerContextWiring(t *testing.T) {
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
			// kiro-spec-loop-facets task 5.1 で session A skill prompt の先頭に
			// `# Persona\n\n...---\n\n` が prepend されたため、HasPrefix では識別できない。
			// 素の slash command を Contains で識別する。
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

	// answerer 呼び出しを抽出 (spec-init の 1 round 中に 1 回出現する)
	var answerCalls []recordedAgentCall
	for _, c := range agent.Calls {
		if c.Kind == "answerer" {
			answerCalls = append(answerCalls, c)
		}
	}
	if len(answerCalls) == 0 {
		t.Fatalf("answerer 呼び出しが少なくとも 1 回発生することを期待: calls=%+v", agent.Calls)
	}
	answerPrompt := answerCalls[0].Prompt

	if !strings.Contains(answerPrompt, "# Context") {
		t.Errorf("answerer prompt は `# Context` ブロックを含むべき:\n%s", answerPrompt)
	}
	wantCmd := "/kiro-spec-init " + feature
	if !strings.Contains(answerPrompt, wantCmd) {
		t.Errorf("answerer prompt は元 PromptCommand %q を含むべき:\n%s", wantCmd, answerPrompt)
	}
	wantBriefPath := filepath.Join(cwd, ".kiro", "specs", feature, "brief.md")
	if absBrief, err := filepath.Abs(wantBriefPath); err == nil {
		wantBriefPath = absBrief
	}
	if !strings.Contains(answerPrompt, wantBriefPath) {
		t.Errorf("answerer prompt は brief.md パス %q を含むべき:\n%s", wantBriefPath, answerPrompt)
	}
	if !strings.Contains(answerPrompt, "[runner-directive]") {
		t.Errorf("answerer prompt 末尾は runner-directive を含むべき:\n%s", answerPrompt)
	}
}

// TestSpecLoopRunner_PersonaAndPreviousResponseWiring は slash-first 順での
// Persona / Previous Response 配線を固定する:
//
//	(i)  6 phase いずれの起動でも session A 初回 skill prompt が
//	     `/kiro-<phase> <feature>` で始まり、その後に `\n\n---\n\n# Persona\n\n<facet content>` を含む。
//	(ii) 2-6 段目では prompt body 内に `\n\n---\n\n# Previous Response\n\n<N-1 段目 Summary>` が
//	     埋め込まれ、1 段目では `# Previous Response` 文字列を含まない。
//	     spec-loop は foundation 側 IncludePreviousResponse 経路を使わないため、
//	     Options.IncludePreviousResponse は常に false / PreviousResponseContent は常に空文字。
//	(iii) session B (`AnswerContext.PromptCommand`) には persona facet 内容が現れない
//	     (別 test TestSpecLoopRunner_AnswererPromptHasNoPersonaFacet で観察)。
func TestSpecLoopRunner_PersonaAndPreviousResponseWiring(t *testing.T) {
	feature := "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}

	// embed 経路で facet 内容を取得し、prompt 観察の期待値として使う。
	wantPersonaByPhase := map[string]string{}
	for _, name := range []string{"spec-analyst", "spec-architect", "validator"} {
		rf, err := facet.ResolveFacet(cwd, facet.Persona, name)
		if err != nil {
			t.Fatalf("facet.ResolveFacet(%s) error: %v", name, err)
		}
		wantPersonaByPhase[name] = rf.Content
	}
	personaByPhase := map[string]string{
		"spec-init":         "spec-analyst",
		"spec-requirements": "spec-analyst",
		"validate-gap":      "validator",
		"spec-design":       "spec-architect",
		"validate-design":   "validator",
		"spec-tasks":        "spec-architect",
	}

	agent := &scriptedAgent{}
	// summary の値で N-1 段目 → N 段目の伝播を観察する。
	summaryByPhase := map[string]string{
		"spec-init":         "summary-step-1-spec-init",
		"spec-requirements": "summary-step-2-spec-requirements",
		"validate-gap":      "summary-step-3-validate-gap",
		"spec-design":       "summary-step-4-spec-design",
		"validate-design":   "summary-step-5-validate-design",
		"spec-tasks":        "summary-step-6-spec-tasks",
	}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "commit":
			return commitOKResp(), nil
		case "skill":
			// どの phase の skill 呼び出しか option.StepName ("<NN>-<phase>") から識別する。
			phase := ""
			if call.Options != nil {
				for _, p := range []string{"spec-init", "spec-requirements", "validate-gap", "spec-design", "validate-design", "spec-tasks"} {
					if strings.HasSuffix(call.Options.StepName, "-"+p) {
						phase = p
						break
					}
				}
			}
			return provider.AgentResponse{
				Success: true,
				StructuredOutput: map[string]any{
					"has_inquiry": false,
					"summary":     summaryByPhase[phase],
				},
				SessionID: "session-" + phase,
			}, nil
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

	// session A 初回 skill 呼び出しを phase ごとに 1 件抽出する (ResumeSessionID="" かつ
	// StepName が answerer suffix を持たないものが session A 初回; 本シナリオでは
	// 各 phase 1 件のみ発火する)。
	skillCallByPhase := map[string]recordedAgentCall{}
	orderedPhases := []string{"spec-init", "spec-requirements", "validate-gap", "spec-design", "validate-design", "spec-tasks"}
	for _, c := range agent.Calls {
		if c.Kind != "skill" || c.ResumeSessionID != "" || c.Options == nil {
			continue
		}
		if strings.HasSuffix(c.Options.StepName, "-answer") {
			continue
		}
		for _, p := range orderedPhases {
			if strings.HasSuffix(c.Options.StepName, "-"+p) {
				skillCallByPhase[p] = c
				break
			}
		}
	}
	if len(skillCallByPhase) != 6 {
		t.Fatalf("session A 初回 skill 呼び出しは 6 phase 分発火することを期待: got=%d (%+v)",
			len(skillCallByPhase), agent.Calls)
	}

	// (i) 各 phase の session A 初回 skill prompt が slash command で始まり、
	// その後に persona block が連結されていること (slash-first 順)。
	for _, phase := range orderedPhases {
		c, ok := skillCallByPhase[phase]
		if !ok {
			t.Fatalf("phase %q の session A skill 呼び出しが未発火", phase)
		}
		facetName := personaByPhase[phase]
		wantPersona := wantPersonaByPhase[facetName]
		wantSlashHead := "/kiro-" + phase + " " + feature
		wantPersonaBlock := "\n\n---\n\n# Persona\n\n" + wantPersona
		if !strings.HasPrefix(c.Prompt, wantSlashHead) {
			head := c.Prompt
			if len(head) > 80 {
				head = head[:80]
			}
			t.Errorf("phase %q: prompt 先頭が slash command と一致しない (slash-first 順)\n got_head=%q\nwant_head_prefix=%q",
				phase, head, wantSlashHead)
		}
		if !strings.Contains(c.Prompt, wantPersonaBlock) {
			t.Errorf("phase %q: prompt に persona block が含まれない\n want_block=%q\ngot_head=%q",
				phase, wantPersonaBlock, c.Prompt[:min(len(c.Prompt), 512)])
		}
	}

	// (ii) Previous Response の埋め込みは prompt body 内 (slash-first 順)。
	// spec-loop は foundation 側 IncludePreviousResponse 経路を使わない。
	for i, phase := range orderedPhases {
		c := skillCallByPhase[phase]
		if c.Options.IncludePreviousResponse {
			t.Errorf("phase %q (%d 段目): IncludePreviousResponse=false を期待 (spec-loop は foundation 前置経路を使わない): got true",
				phase, i+1)
		}
		if c.Options.PreviousResponseContent != "" {
			t.Errorf("phase %q (%d 段目): PreviousResponseContent=\"\" を期待: got %q",
				phase, i+1, c.Options.PreviousResponseContent)
		}
		if i == 0 {
			// 1 段目: 前段なし → prompt に `# Previous Response` を含まない
			if strings.Contains(c.Prompt, "# Previous Response") {
				t.Errorf("phase %q (1 段目): prompt に `# Previous Response` を含めてはならない (前段なし):\n%s",
					phase, c.Prompt[:min(len(c.Prompt), 512)])
			}
			continue
		}
		// 2 段目以降: 直前 phase の Summary が prompt body に埋め込まれている
		wantPrev := summaryByPhase[orderedPhases[i-1]]
		wantPrevBlock := "\n\n---\n\n# Previous Response\n\n" + wantPrev
		if !strings.Contains(c.Prompt, wantPrevBlock) {
			t.Errorf("phase %q (%d 段目): prompt に Previous Response block が含まれない\n want_block=%q\ngot=%q",
				phase, i+1, wantPrevBlock, c.Prompt[:min(len(c.Prompt), 1024)])
		}
	}

	// (ii) session B AnswerContext.PromptCommand に persona facet 内容が現れないこと。
	// 本シナリオでは all phase が no_inquiry で完走しており answerer は発火しないので、
	// 別途 1 round 回す inquiry シナリオで観察する。本テストは別関数
	// TestSpecLoopRunner_AnswererPromptHasNoPersonaFacet で観察する。
}

// TestSpecLoopRunner_AnswererPromptHasNoPersonaFacet は task 5.1 観察 (ii) を駆動する:
// session B (`AnswerContext.PromptCommand`) に persona facet 内容が現れないこと。
// 1 round 回す inquiry シナリオで answerer 呼び出しの prompt に facet 内容が含まれないことを assert する。
//
// 要件: kiro-spec-loop-facets Req 2.2 余事象 / 3.4。
func TestSpecLoopRunner_AnswererPromptHasNoPersonaFacet(t *testing.T) {
	feature := "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}

	wantAnalyst, err := facet.ResolveFacet(cwd, facet.Persona, "spec-analyst")
	if err != nil {
		t.Fatalf("facet.ResolveFacet(spec-analyst) error: %v", err)
	}

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "commit":
			return commitOKResp(), nil
		case "answerer":
			return answerOKResp(), nil
		case "skill":
			// spec-init: 初回 = inquiry、resume = no_inquiry で 1 ラウンドで完走させる。
			if call.ResumeSessionID == "" && strings.Contains(call.Prompt, "/kiro-spec-init") {
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

	var answererCount int
	for _, c := range agent.Calls {
		if c.Kind != "answerer" {
			continue
		}
		answererCount++
		// session B prompt に session A 用 facet 内容が混入していないこと。
		if strings.Contains(c.Prompt, wantAnalyst.Content) {
			t.Errorf("answerer prompt に session A の persona facet 内容が混入してはならない:\n%s", c.Prompt)
		}
		// session B prompt は AnswerContext.PromptCommand を `# Context` ブロックの
		// 元コマンド bullet として展開するため、素の slash コマンドが含まれる
		// (= persona prefix を含まない素の step.Command が渡っていることの間接観察)。
		if !strings.Contains(c.Prompt, "/kiro-spec-init "+feature) {
			t.Errorf("answerer prompt は素の slash コマンドを `# Context` bullet として含むべき:\n%s", c.Prompt)
		}
		// session B prompt の `# Context` bullet 内に persona prefix の区切り (`---`) が
		// markdown bullet を崩す形で混入していないこと (Design §Decisions 3 の崩れ回避)。
		bulletLine := "- 元コマンド: /kiro-spec-init " + feature
		if !strings.Contains(c.Prompt, bulletLine) {
			t.Errorf("answerer prompt の `# Context` bullet が素の単一行形式で含まれていない (期待: %q):\n%s",
				bulletLine, c.Prompt)
		}
	}
	if answererCount == 0 {
		t.Fatalf("answerer 呼び出しが少なくとも 1 回発火することを期待: calls=%+v", agent.Calls)
	}
}

// min は Go 1.21 以降組み込みだが、本パッケージは古い Go との互換のためローカル定義しておく。
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
