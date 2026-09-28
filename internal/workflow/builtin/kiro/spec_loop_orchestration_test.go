// Package workflow の本テストファイルは kiro-spec-loop ワークフローの
// orchestration 周りの薄い構造（KiroSpecLoop / steps[] / referenceFiles /
// agentCallerAdapter）に対する単体テストを集約する。
//
// 監査ヘルパのテストは kiro_spec_loop_audit_test.go、
// listEligibleFeatures など feature 選定ヘルパのテストは kiro_spec_loop_test.go に分離している。
//
// 本ファイルが検証するのはタスク 4.1 の Observable に対応する次の点:
//   - KiroSpecLoop(agent) の workflow.Definition.ID / workflow.Definition.Label
//   - steps[] が 6 件で固定 Phase 順
//   - Step 1 のみ markdownPaths を反映する Command 関数
//   - referenceFiles(paths) のフォーマット
//   - agentCallerAdapter が inquiryloop.AgentCaller を満たし、内部 workflowctx.AgentCaller に
//     正しく橋渡しすること
package kiro

import (
	"context"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/inquiryloop"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// TestKiroSpecLoopDefinition は KiroSpecLoop が ID / Label を期待値で返し、Fn が non-nil で
// あることを検証する（タスク 4.1 Observable）。
func TestKiroSpecLoopDefinition(t *testing.T) {
	def := KiroSpecLoop(nil)
	if def.ID != "kiro-spec-loop" {
		t.Errorf("ID mismatch: got=%q want=%q", def.ID, "kiro-spec-loop")
	}
	if def.Label != "Kiro 仕様作成ループ" {
		t.Errorf("Label mismatch: got=%q want=%q", def.Label, "Kiro 仕様作成ループ")
	}
	if def.Fn == nil {
		t.Errorf("workflow.Definition.Fn が nil（4.2 で本体実装するためのスタブが必要）")
	}
}

// TestKiroSpecLoopSteps は steps 配列が 6 件で、Order と Phase が固定順であることを検証する。
// 4.1 Observable で要求される「6 段固定順」契約のコアテスト。
func TestKiroSpecLoopSteps(t *testing.T) {
	if len(steps) != 6 {
		t.Fatalf("steps の件数は 6 であるべき: got=%d", len(steps))
	}
	wantPhases := []string{
		"spec-init",
		"spec-requirements",
		"validate-gap",
		"spec-design",
		"validate-design",
		"spec-tasks",
	}
	for i, want := range wantPhases {
		if steps[i].Order != i+1 {
			t.Errorf("steps[%d].Order mismatch: got=%d want=%d", i, steps[i].Order, i+1)
		}
		if steps[i].Phase != want {
			t.Errorf("steps[%d].Phase mismatch: got=%q want=%q", i, steps[i].Phase, want)
		}
		if steps[i].Command == nil {
			t.Errorf("steps[%d].Command が nil", i)
		}
	}
}

// TestPromptStepCommand は Step 1 だけが markdownPaths を反映し、他 step は無視することを検証する。
// design.md「Step 1 (spec-init) のみ … markdownPaths に積めば対応可能」契約に従う。
func TestPromptStepCommand(t *testing.T) {
	feature := "demo-feature"
	briefPath := "/abs/path/.kiro/specs/demo-feature/brief.md"
	paths := []string{briefPath}

	// --- Step 1 (spec-init): markdownPaths を反映する ---
	cmd1 := steps[0].Command(feature, paths)
	if !strings.HasPrefix(cmd1, "/kiro-spec-init "+feature) {
		t.Errorf("Step1 が想定 prefix を持たない: %q", cmd1)
	}
	if !strings.Contains(cmd1, "以下のマークダウンファイルを参照してください") {
		t.Errorf("Step1 に参照ヘッダが欠けている: %q", cmd1)
	}
	if !strings.Contains(cmd1, briefPath) {
		t.Errorf("Step1 に brief.md パスが含まれない: %q", cmd1)
	}

	// Step 1 でも paths が空 / nil の場合は参照ブロックを付加しないこと
	cmd1NoPaths := steps[0].Command(feature, nil)
	if strings.Contains(cmd1NoPaths, "以下のマークダウンファイルを参照してください") {
		t.Errorf("Step1 (paths=nil) では参照ブロックを付加してはならない: %q", cmd1NoPaths)
	}
	if strings.TrimSpace(cmd1NoPaths) != "/kiro-spec-init "+feature {
		t.Errorf("Step1 (paths=nil) の出力が期待値と一致しない: got=%q", cmd1NoPaths)
	}

	// --- Step 2..6: markdownPaths を渡しても反映しない ---
	expectedPrefixes := []string{
		"", // index 0 は Step 1 で別検証済み
		"/kiro-spec-requirements " + feature,
		"/kiro-validate-gap " + feature,
		"/kiro-spec-design " + feature,
		"/kiro-validate-design " + feature,
		"/kiro-spec-tasks " + feature,
	}
	for i := 1; i < 6; i++ {
		// markdownPaths を渡しても無視されるべき
		cmd := steps[i].Command(feature, paths)
		if strings.Contains(cmd, "以下のマークダウンファイルを参照してください") {
			t.Errorf("steps[%d] (Phase=%s) は markdownPaths を反映してはならない: %q",
				i, steps[i].Phase, cmd)
		}
		if strings.TrimSpace(cmd) != expectedPrefixes[i] {
			t.Errorf("steps[%d] の出力が想定値と一致しない: got=%q want=%q",
				i, cmd, expectedPrefixes[i])
		}
	}
}

// TestReferenceFiles は referenceFiles(paths) の整形仕様を検証する。
// design.md 642-645 行参照、tasks.md 4.1 で referenceFiles の挙動として明示。
func TestReferenceFiles(t *testing.T) {
	if got := referenceFiles(nil); got != "" {
		t.Errorf("nil paths は空文字列を返すべき: got=%q", got)
	}
	if got := referenceFiles([]string{}); got != "" {
		t.Errorf("空 slice は空文字列を返すべき: got=%q", got)
	}

	got := referenceFiles([]string{"/p/a.md", "/p/b.md"})
	if !strings.HasPrefix(got, "\n\n") {
		t.Errorf("先頭が空行 2 つで始まるべき（直前のコマンド本文と空行 1 行で区切るため）: %q", got)
	}
	if !strings.Contains(got, "以下のマークダウンファイルを参照してください") {
		t.Errorf("固定ヘッダ文を含むべき: %q", got)
	}
	if !strings.Contains(got, "- /p/a.md") {
		t.Errorf("path a が箇条書きで含まれるべき: %q", got)
	}
	if !strings.Contains(got, "- /p/b.md") {
		t.Errorf("path b が箇条書きで含まれるべき: %q", got)
	}
}

// TestAgentCallerAdapterImplementsInquiryloopAgentCaller は agentCallerAdapter が
// inquiryloop.AgentCaller インターフェースを満たすことをコンパイル時にアサートする。
//
// 4.1 では adapter のサブミット先である inquiryloop.Engine.RunPrompt はまだ呼ばれないが、
// 4.2 でアダプタを Engine に渡せるようにするために型の一致を 4.1 段階で検証する。
func TestAgentCallerAdapterImplementsInquiryloopAgentCaller(t *testing.T) {
	var _ inquiryloop.AgentCaller = agentCallerAdapter{}
}

// TestAgentCallerAdapterCall は workflowctx.AgentCaller を inquiryloop.AgentCaller に橋渡しする
// agentCallerAdapter が、引数を漏れなく内部 CallAgent に転送することを検証する。
func TestAgentCallerAdapterCall(t *testing.T) {
	inner := &fakeWorkflowAgent{
		response: provider.AgentResponse{
			Success:   true,
			Content:   "ok",
			SessionID: "session-A",
		},
	}
	adapter := agentCallerAdapter{inner: inner}
	schema := inquiryloop.Schema()

	resp, err := adapter.Call(
		context.Background(),
		"prompt-text",
		"/tmp",
		nil,
		"resume-x",
		schema,
		"step-1",
		"01-spec-init",
	)
	if err != nil {
		t.Fatalf("adapter.Call returned error: %v", err)
	}
	if resp.Content != "ok" || resp.SessionID != "session-A" {
		t.Errorf("response が内部応答と一致しない: %+v", resp)
	}
	if inner.lastPrompt != "prompt-text" {
		t.Errorf("inner.CallAgent prompt mismatch: got=%q", inner.lastPrompt)
	}
	if inner.lastCwd != "/tmp" {
		t.Errorf("inner.CallAgent cwd mismatch: got=%q", inner.lastCwd)
	}
	if inner.lastResume != "resume-x" {
		t.Errorf("inner.CallAgent resume mismatch: got=%q", inner.lastResume)
	}
	if inner.lastOptions == nil {
		t.Fatalf("inner.CallAgent options が nil（OutputFormat / StepID / StepName を運ぶために必須）")
	}
	if inner.lastOptions.StepID != "step-1" {
		t.Errorf("StepID mismatch: got=%q", inner.lastOptions.StepID)
	}
	if inner.lastOptions.StepName != "01-spec-init" {
		t.Errorf("StepName mismatch: got=%q", inner.lastOptions.StepName)
	}
	if inner.lastOptions.OutputFormat != schema {
		t.Errorf("OutputFormat はそのまま橋渡しすべき（pointer 同一）")
	}
}

// fakeWorkflowAgent は agentCallerAdapter のテスト用に
// workflowctx.AgentCaller インターフェースだけを満たす最小スタブ。
type fakeWorkflowAgent struct {
	response    provider.AgentResponse
	err         error
	lastPrompt  string
	lastCwd     string
	lastResume  string
	lastOptions *workflowctx.CallAgentOptions
}

func (f *fakeWorkflowAgent) CallAgent(
	ctx context.Context,
	prompt string,
	cwd string,
	onEvent provider.AgentEventCallback,
	resumeSessionID string,
	options *workflowctx.CallAgentOptions,
) (provider.AgentResponse, error) {
	f.lastPrompt = prompt
	f.lastCwd = cwd
	f.lastResume = resumeSessionID
	f.lastOptions = options
	return f.response, f.err
}
