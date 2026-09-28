// Package workflow の本テストファイルは kiro-impl-loop の per-iteration skip 機能を
// 駆動するテストを集約する。
//
// 検証対象:
//   - run 開始時に捕捉した pendingTasks のうち、iteration 開始時点で既に [x] と
//     なっているタスクをスキップし、/kiro-impl と /git-commit を発火しないこと
//   - skip 通知が WorkflowText event として onEvent に流れること
//
// 背景: kiro-impl-loop ではタスクごとに inquiryloop.Engine 駆動で /kiro-impl を
// 1 回呼ぶが、回答ペルソナが scope 拡張を許してしまったり実装エージェントが
// tasks.md の他タスクを暗黙的に [x] 化したりすると、後続 iteration が "もう完了済"
// inquiry で 5 ラウンド消費する症状が出る。本 skip により症状の再発呼び出しを断つ。
package kiro

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
)

// TestKiroImplLoopSkipsAlreadyCheckedTasks は spillover シナリオを再現する:
//   - 開始時 pendingTasks = [1.1, 1.2]
//   - 1.1 の impl skill 呼び出し中に tasks.md の 1.2 も [x] に書き替えられる
//     (実装エージェントが scope 越えして勝手にマークしたケースを模擬)
//   - 続く 1.2 の iteration はスキップされ、/kiro-impl と /git-commit は発火しない
//   - 最終検証 (/kiro-validate-impl) は通常通り 1 回発火する
//
// 期待 agent.Calls は 3 件:
//  1. /kiro-impl <feature> 1.1
//  2. /git-commit  (1.1 の commit)
//  3. /kiro-validate-impl
func TestKiroImplLoopSkipsAlreadyCheckedTasks(t *testing.T) {
	feature := "demo"
	cwd := setupImplFeatureCWD(t, feature, "- [ ] 1.1 First\n- [ ] 1.2 Second\n")
	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Selected: feature}

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch {
		case call.Kind == "commit":
			return commitOKResp(), nil
		case strings.HasPrefix(call.Prompt, "/kiro-validate-impl"):
			return implFinalReviewOKResp(), nil
		case strings.HasPrefix(call.Prompt, "/kiro-impl "+feature+" 1.1"):
			// spillover を模擬: implementer が他タスクも進めて tasks.md の 1.2 を [x] にしてしまう。
			// 本来 kiro-impl skill は "Do NOT update tasks.md" 制約を持つが、persona 側で
			// scope 拡張を許可された場合に観測されている挙動 (手動 [x] 化) を再現する。
			tasksPath := filepath.Join(call.CWD, ".kiro", "specs", feature, "tasks.md")
			if err := os.WriteFile(tasksPath, []byte("- [ ] 1.1 First\n- [x] 1.2 Second\n"), 0o644); err != nil {
				t.Fatalf("simulate spillover: WriteFile tasks.md: %v", err)
			}
			return skillNoInquiryResp(), nil
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

	// 1.2 が skip された結果 agent.CallAgent は 3 件のみ:
	// 1.1 の impl + 1.1 の commit + 最終検証。1.2 用の impl/commit は発火しないはず。
	if len(agent.Calls) != 3 {
		t.Fatalf("skip シナリオでは 3 件の CallAgent を期待: got=%d (%+v)", len(agent.Calls), agent.Calls)
	}
	// skip シナリオでは 1.1 のみが実 impl 駆動の対象であり N=1 段目に該当するため
	// `# Previous Response` ブロックは含まれない。
	if !strings.HasPrefix(agent.Calls[0].Prompt, "/kiro-impl "+feature+" 1.1") {
		t.Errorf("call[0] は `/kiro-impl %s 1.1` で始まるべき: got head=%q",
			feature, agent.Calls[0].Prompt[:min(len(agent.Calls[0].Prompt), 64)])
	}
	if strings.Contains(agent.Calls[0].Prompt, "# Previous Response") {
		t.Errorf("call[0] (skip シナリオの 1.1 = N=1 段目) は `# Previous Response` ブロックを含まないべき (前段なし): got=%q",
			agent.Calls[0].Prompt)
	}
	if !strings.HasPrefix(agent.Calls[1].Prompt, "/git-commit") {
		t.Errorf("call[1] は /git-commit: got=%q", agent.Calls[1].Prompt)
	}
	if !strings.HasPrefix(agent.Calls[2].Prompt, "/kiro-validate-impl") {
		t.Errorf("call[2] は /kiro-validate-impl: got=%q", agent.Calls[2].Prompt)
	}

	// 1.2 は impl skill が呼ばれず、prompt に "1.2" が現れないこと。
	for i, c := range agent.Calls {
		if strings.Contains(c.Prompt, "/kiro-impl "+feature+" 1.2") {
			t.Errorf("call[%d] が 1.2 用の /kiro-impl を発火: %q", i, c.Prompt)
		}
	}

	// skip 通知 event が WorkflowText で出ていること (operator 視認用)。
	if !hasSkipTextEvent(*eventsPtr, "1.2") {
		t.Errorf("1.2 の skip 通知 event (WorkflowText) が見当たらない: %+v", *eventsPtr)
	}

	// tasks.md は両方 [x] のまま (workflow が既存マークを破壊していないこと)。
	tasksRaw, err := os.ReadFile(filepath.Join(cwd, ".kiro", "specs", feature, "tasks.md"))
	if err != nil {
		t.Fatalf("ReadFile tasks.md: %v", err)
	}
	got := string(tasksRaw)
	if !strings.Contains(got, "- [x] 1.1") || !strings.Contains(got, "- [x] 1.2") {
		t.Errorf("両タスクが [x] であるべき: %s", got)
	}
}

// hasSkipTextEvent は skip 通知用の WorkflowText event に taskID 文字列が
// 含まれているかを判定する補助関数。
func hasSkipTextEvent(events []workflow.WorkflowEvent, taskID string) bool {
	for _, e := range events {
		if e.Type == workflow.WorkflowText && strings.Contains(e.Text, taskID) && strings.Contains(e.Text, "スキップ") {
			return true
		}
	}
	return false
}
