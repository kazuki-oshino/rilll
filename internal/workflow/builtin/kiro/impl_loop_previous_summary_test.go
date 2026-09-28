// Package workflow の本テストファイルは kiro-impl-loop の per-task ループにおける
// 前タスク Summary 引き渡しを統合レベルで観察する。
//
// 観察対象 (slash-first 後置仕様、kiro-spec-loop 側 dc6aa54 と同形):
//   - N=1 (先頭タスク): session A 初回 skill 呼び出しの Prompt 末尾に
//     `# Previous Response` ブロックが現れない (前段なし → previousResponseBlock no-op)
//   - N>=2 (後続タスク): session A 初回 skill 呼び出しの Prompt 末尾に
//     `\n\n---\n\n# Previous Response\n\n<summary>` ブロックが追記される
//   - N=3 タスクの後置 block には N=2 タスクの summary のみが含まれ、
//     N=1 + N=2 の累積連結ではない (直前 1 段のみ参照、累積禁止)
//   - per-iteration skip (既 [x] taskID) を含むケース: taskSummaries の index ↔ taskID
//     対応が維持され、skip 直後の未完了タスクは「直前タスクが skip 済 = 引き継ぐべき
//     前段 summary を持たない」ため後置 block なしで起動する
//   - foundation 前置経路 (Options.IncludePreviousResponse) は kiro-impl-loop からは
//     使わない: adapter を素の forwarder として通り、前段 summary は
//     runKiroImplLoop 内で `previousResponseBlock(prev)` を skill コマンドの後ろに
//     連結することで slash-first 順 (`/kiro-impl ...` が prompt 先頭) を維持する
//
// 観察方法:
//   - `scriptedAgent.Calls[i].Prompt` の prefix (`/kiro-impl <feature> <taskID>`) を
//     `strings.HasPrefix` で検査し、prompt 先頭が必ず slash command であることを担保
//   - 末尾の後置 block 文字列 (`\n\n---\n\n# Previous Response\n\n<summary>`) を
//     `strings.Contains` で検査
//   - Options.IncludePreviousResponse は常に false、Options.PreviousResponseContent は
//     常に "" であることを併せて検査する (adapter から 3 条件ゲート / previousSummary
//     フィールドを撤去済みで、foundation 前置経路は kiro-impl-loop からは未使用)
//   - round 駆動・persist 挙動を不変 とする観察可能完了条件は no-inquiry シナリオで
//     `round-*-{inquiry,answer}.md` が作られないこと で構造的に担保する
package kiro

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

// taskSummaryFor は scriptedAgent.Respond クロージャ内で「どの task 用 skill 呼び出しか」を
// prompt 内 substring `/kiro-impl <feature> <taskID>` から識別し、task ごとに distinguishable な
// summary を返す。task ごとに固有値を返すことで、後続 task 観察時に
// 後置 block 内の文字列が「どの task の summary を引いてきたか」を一意に判定できる。
func taskSummaryFor(taskID string) string {
	return "summary-for-task-" + taskID
}

// taskSummaryResp は task 固有 summary を持つ skill 応答 (has_inquiry=false 完走) を組み立てる。
// `inquiryloop.Engine.RunPrompt` の `FinalResult.Summary` field は構造化応答 StructuredOutput.summary
// に由来するため、ここで task ごとに distinguishable な値を埋め込むことで後続 task の
// 後置 block 内容観察に利用する。
func taskSummaryResp(taskID string) provider.AgentResponse {
	return provider.AgentResponse{
		Success: true,
		StructuredOutput: map[string]any{
			"has_inquiry": false,
			"summary":     taskSummaryFor(taskID),
		},
		SessionID: "session-skill-" + taskID,
	}
}

// previousResponseBlockForSummary はテスト assert 用に
// `runKiroImplLoop` が prompt 末尾に連結する後置 block の期待文字列を組み立てる helper。
// production 側 `previousResponseBlock(prev)` の戻り値そのものと一致する形式で、
// 本テストの summary はいずれも `facet.CONTEXT_MAX_CHARS` (2000 rune) 未満のため
// 切り詰めは発生しない前提。
func previousResponseBlockForSummary(summary string) string {
	return "\n\n---\n\n# Previous Response\n\n" + summary
}

// TestKiroImplLoop_MultiTask_PreviousSummaryWiring は kiro-impl-loop の per-task ループにおける
// 前タスク Summary 引き渡しを 3 task 連鎖の no-inquiry / 0-round シナリオで網羅する統合テスト:
//
//	(1) N=1 (先頭) skill 呼び出しは Prompt 末尾に `# Previous Response` ブロックを含まない
//	(2) N=2 skill 呼び出しは Prompt 末尾に previousResponseBlock(task 1.1 summary) が連結される
//	(3) N=3 skill 呼び出しは Prompt 末尾に previousResponseBlock(task 1.2 summary) "のみ" が連結され、
//	    task 1.1 + task 1.2 の累積連結ではない (直前 1 段のみ参照、累積禁止)
//	(4) commit / final-review 呼び出しでは Prompt は対応する skill コマンドのみで、
//	    後置 block は連結されない (commit は agentCallerAdapter 非経由、final-review は
//	    per-task ループ外の別経路)
//
// 共通の不変条件:
//   - すべての agent.CallAgent 呼び出しで Options.IncludePreviousResponse=false /
//     Options.PreviousResponseContent="" を維持する (foundation 前置経路は未使用)
//   - skill 呼び出しの Prompt は必ず `/kiro-impl <feature> <taskID>` で始まる (slash-first)
//
// 観察対象:
//
//	pendingTasks = ["1.1", "1.2", "1.3"] (全 [ ])
//	agent.Calls = [
//	  [0] skill /kiro-impl demo 1.1   (ResumeSessionID="")
//	  [1] commit                       (StepID="commit-1.1")
//	  [2] skill /kiro-impl demo 1.2   (ResumeSessionID="")
//	  [3] commit                       (StepID="commit-1.2")
//	  [4] skill /kiro-impl demo 1.3   (ResumeSessionID="")
//	  [5] commit                       (StepID="commit-1.3")
//	  [6] skill /kiro-validate-impl    (ResumeSessionID="")  ← final-review (touch 外)
//	]
func TestKiroImplLoop_MultiTask_PreviousSummaryWiring(t *testing.T) {
	feature := "demo"
	cwd := setupImplFeatureCWD(t, feature, "- [ ] 1.1 First\n- [ ] 1.2 Second\n- [ ] 1.3 Third\n")
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch {
		case call.Kind == "commit":
			return commitOKResp(), nil
		case strings.Contains(call.Prompt, "/kiro-validate-impl"):
			return implFinalReviewOKResp(), nil
		case strings.Contains(call.Prompt, "/kiro-impl "+feature+" 1.1"):
			return taskSummaryResp("1.1"), nil
		case strings.Contains(call.Prompt, "/kiro-impl "+feature+" 1.2"):
			return taskSummaryResp("1.2"), nil
		case strings.Contains(call.Prompt, "/kiro-impl "+feature+" 1.3"):
			return taskSummaryResp("1.3"), nil
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

	// 3 task × (skill + commit) + final-review = 7 件
	if len(agent.Calls) != 7 {
		t.Fatalf("multi-task 連鎖 (3 task no-inquiry + final) では 7 件の CallAgent を期待: got=%d (%+v)",
			len(agent.Calls), agent.Calls)
	}

	// 期待 kind 列 (commit step 後置不採用 + final-review 別経路の構造的検証ベース)
	wantKinds := []string{"skill", "commit", "skill", "commit", "skill", "commit", "skill"}
	for i, want := range wantKinds {
		if agent.Calls[i].Kind != want {
			t.Errorf("call[%d].Kind=%q want=%q (prompt head=%q)",
				i, agent.Calls[i].Kind, want, agent.Calls[i].Prompt[:min(len(agent.Calls[i].Prompt), 64)])
		}
	}

	// table-driven assertion: 各 skill / commit call の Prompt 構造を観察する。
	//
	//   - wantPromptPrefix: Prompt 先頭一致 (slash-first 不変条件)。空文字なら検査スキップ
	//     (commit の prompt は `/git-commit` 等で固定でないため)
	//   - wantPromptContains: Prompt の末尾後置 block 期待値。空文字なら「prompt に
	//     `# Previous Response` 文字列が現れない」ことを assert
	//
	// 全 call 共通で Options.IncludePreviousResponse=false / Options.PreviousResponseContent=""
	// を assert する (foundation 前置経路は kiro-impl-loop から未使用)。
	type wantPrev struct {
		callIdx            int    // agent.Calls の index
		label              string // 失敗時に「どの call か」を識別するラベル
		wantPromptPrefix   string // Prompt 先頭が一致すべき文字列 (空文字なら検査スキップ)
		wantPromptContains string // Prompt 末尾後置 block の期待値 (空文字なら「`# Previous Response` 不在」を要求)
		wantResumeEmpty    bool   // ResumeSessionID == "" (skill 初回呼び出しのみ)
	}
	wants := []wantPrev{
		{
			// (1) N=1 (先頭タスク 1.1) skill 呼び出し:
			//   - i==0 → previousSummaryForTaskIndex(0, ...) は "" を返す
			//   - previousResponseBlock("") は空文字を返し block が省略される
			//   - prompt 本文に `# Previous Response` 文字列が現れない
			callIdx:            0,
			label:              "N=1 task 1.1 skill (先頭タスク、後置 block なし)",
			wantPromptPrefix:   "/kiro-impl " + feature + " 1.1",
			wantPromptContains: "",
			wantResumeEmpty:    true,
		},
		{
			// commit step は `agent.CallAgent` 直接呼び出しで agentCallerAdapter を経由しない。
			// したがって runKiroImplLoop 側の previousResponseBlock 後置経路にも乗らず、
			// prompt は `/git-commit` のみとなる。
			callIdx:            1,
			label:              "commit-1.1 (agentCallerAdapter 非経由、後置 block なし)",
			wantPromptPrefix:   "",
			wantPromptContains: "",
		},
		{
			// (2) N=2 (task 1.2) skill 呼び出し:
			//   - i==1 → previousSummaryForTaskIndex(1, [s11], ...) は taskSummaries[0] を返す
			//     (in-memory hit)
			//   - prompt 末尾に previousResponseBlock(summary-1.1) が連結される
			callIdx:            2,
			label:              "N=2 task 1.2 skill (後置 block: summary-1.1)",
			wantPromptPrefix:   "/kiro-impl " + feature + " 1.2",
			wantPromptContains: previousResponseBlockForSummary(taskSummaryFor("1.1")),
			wantResumeEmpty:    true,
		},
		{
			callIdx:            3,
			label:              "commit-1.2 (agentCallerAdapter 非経由)",
			wantPromptPrefix:   "",
			wantPromptContains: "",
		},
		{
			// (3) N=3 (task 1.3) skill 呼び出し:
			//   - i==2 → previousSummaryForTaskIndex(2, [s11, s12], ...) は taskSummaries[1] を返す
			//     (直前 1 段のみ参照、累積禁止)
			//   - prompt 末尾の後置 block は summary-1.2 "のみ" を含み、summary-1.1 を含まない
			callIdx:            4,
			label:              "N=3 task 1.3 skill (後置 block: 直前 1 段 summary-1.2 のみ)",
			wantPromptPrefix:   "/kiro-impl " + feature + " 1.3",
			wantPromptContains: previousResponseBlockForSummary(taskSummaryFor("1.2")),
			wantResumeEmpty:    true,
		},
		{
			callIdx:            5,
			label:              "commit-1.3 (agentCallerAdapter 非経由)",
			wantPromptPrefix:   "",
			wantPromptContains: "",
		},
		{
			// final-review /kiro-validate-impl は impl_loop の per-task ループ外で
			// `runFinalReview` 内に別経路で呼ばれる。per-task の previousResponseBlock 連結経路
			// には乗らないため prompt は素の skill コマンドのみ。
			callIdx:            6,
			label:              "final-review /kiro-validate-impl (per-task ループ外、後置 block なし)",
			wantPromptPrefix:   "",
			wantPromptContains: "",
		},
	}

	for _, w := range wants {
		w := w
		t.Run(w.label, func(t *testing.T) {
			call := agent.Calls[w.callIdx]
			if call.Options == nil {
				t.Fatalf("call[%d] (%s) Options が nil: 期待 IncludePreviousResponse=false", w.callIdx, w.label)
			}
			// foundation 前置経路は kiro-impl-loop から未使用 (常に false / 空文字)
			if call.Options.IncludePreviousResponse {
				t.Errorf("call[%d] (%s) Options.IncludePreviousResponse=true: kiro-impl-loop は foundation 前置経路を使わないべき",
					w.callIdx, w.label)
			}
			if call.Options.PreviousResponseContent != "" {
				t.Errorf("call[%d] (%s) Options.PreviousResponseContent=%q want=\"\" (adapter から previousSummary フィールド撤去済み)",
					w.callIdx, w.label, call.Options.PreviousResponseContent)
			}
			if w.wantResumeEmpty && call.ResumeSessionID != "" {
				t.Errorf("call[%d] (%s) ResumeSessionID=%q want=\"\" (skill 初回呼び出し)",
					w.callIdx, w.label, call.ResumeSessionID)
			}
			// Prompt は必ず `/kiro-impl <feature> <taskID>` で始まる (slash-first 不変条件)。
			// 末尾に Previous Response block が後置される場合でも prefix は不変。
			if w.wantPromptPrefix != "" && !strings.HasPrefix(call.Prompt, w.wantPromptPrefix) {
				t.Errorf("call[%d] (%s) Prompt は %q で始まるべき (slash-first 不変条件): got=%q",
					w.callIdx, w.label, w.wantPromptPrefix, call.Prompt)
			}
			// 後置 block: 期待値が非空なら Prompt は当該文字列を含む。
			// 期待値が空文字なら Prompt 内に `# Previous Response` が現れてはならない。
			if w.wantPromptContains != "" {
				if !strings.Contains(call.Prompt, w.wantPromptContains) {
					t.Errorf("call[%d] (%s) Prompt 末尾に後置 block %q を含むべき: got=%q",
						w.callIdx, w.label, w.wantPromptContains, call.Prompt)
				}
			} else {
				if strings.Contains(call.Prompt, "# Previous Response") {
					t.Errorf("call[%d] (%s) Prompt に `# Previous Response` が含まれてはならない (前段なし): got=%q",
						w.callIdx, w.label, call.Prompt)
				}
			}
		})
	}

	// (3) 累積禁止の追加検証: N=3 タスクの Prompt が N=1 の summary を一切含まないことを
	// substring レベルで構造的に担保する。実装が誤って累積連結
	// (例: previousResponseBlock の中身に summary-1.1 + summary-1.2 の両方を詰める) していたら、
	// このアサーションが落ちて累積禁止リグレッションを検出する。
	task13Prompt := agent.Calls[4].Prompt
	if strings.Contains(task13Prompt, taskSummaryFor("1.1")) {
		t.Errorf("N=3 (task 1.3) の Prompt は task 1.1 summary を含まないべき (直前 1 段のみ参照、累積禁止):\n got=%q\n must not contain=%q",
			task13Prompt, taskSummaryFor("1.1"))
	}

	// round 駆動・persist 挙動を不変 とする観察可能完了条件: no-inquiry シナリオでは
	// inquiry round が発生せず、既存の round 駆動・inquiry 永続化・answer 永続化挙動
	// (round-*-{inquiry,answer}.md) が発火しないことを観察する。
	runRoot := filepath.Join(cwd, ".rilll", "kiro-impl-loop")
	if entries, err := os.ReadDir(runRoot); err == nil && len(entries) == 1 {
		runDir := filepath.Join(runRoot, entries[0].Name())
		for _, taskID := range []string{"1_1", "1_2", "1_3"} {
			// stepDir は <order:02d>-impl-<sanitizedTaskID> 形式 (sanitizePhaseSegment で
			// '.' → '_' 置換済)。order は 1-indexed、task 1.1=01, 1.2=02, 1.3=03。
			for _, suffix := range []string{"round-1-inquiry.md", "round-1-answer.md"} {
				for _, order := range []string{"01", "02", "03"} {
					path := filepath.Join(runDir, order+"-impl-"+taskID, suffix)
					if _, err := os.Stat(path); err == nil {
						t.Errorf("no-inquiry シナリオでは round-* 永続化ファイル %s は作られないべき (round 駆動不変)", path)
					}
				}
			}
		}
	}
}

// TestKiroImplLoop_MultiTask_PreviousSummary_SkipMaintainsIndex は per-iteration skip を
// 含むケースで `taskSummaries` の index ↔ taskID 対応が維持され、次の未完了タスクで
// 「直前 1 段」(= 空文字) が正しく引き渡される (= 後置 block が省略される) ことを
// 統合レベルで担保する。
//
// シナリオ:
//   - 開始時 pendingTasks = [1.1, 1.2, 1.3] (全 [ ])
//   - task 1.1 の impl skill 呼び出し中に tasks.md の 1.2 を [x] に書き替える
//     (spillover 模擬、impl_loop_skip_test.go と同方針)
//   - i=1 (task 1.2) は per-iteration skip → `taskSummaries = [summary-1.1, ""]`
//     (index ↔ taskID 対応のため空文字列を append)
//   - i=2 (task 1.3) では previousSummaryForTaskIndex(2, ["s11", ""], ...) が
//     taskSummaries[1] == "" を返す (直前 1 段のみ参照)
//   - previousResponseBlock("") は空文字を返し block が省略される
//     → 1.3 の skill Prompt 末尾に `# Previous Response` が現れない
//
// 観察:
//
//	agent.Calls = [
//	  [0] skill /kiro-impl demo 1.1   (ResumeSessionID="", 後置 block なし)
//	  [1] commit                       (StepID="commit-1.1")
//	  [2] skill /kiro-impl demo 1.3   (ResumeSessionID="", 後置 block なし: 直前 1.2 が skip)
//	  [3] commit                       (StepID="commit-1.3")
//	  [4] skill /kiro-validate-impl    (final-review)
//	]
//
//	(task 1.2 は skip されるため `/kiro-impl demo 1.2` も `commit-1.2` も呼ばれない)
func TestKiroImplLoop_MultiTask_PreviousSummary_SkipMaintainsIndex(t *testing.T) {
	feature := "demo"
	cwd := setupImplFeatureCWD(t, feature, "- [ ] 1.1 First\n- [ ] 1.2 Second\n- [ ] 1.3 Third\n")
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch {
		case call.Kind == "commit":
			return commitOKResp(), nil
		case strings.Contains(call.Prompt, "/kiro-validate-impl"):
			return implFinalReviewOKResp(), nil
		case strings.Contains(call.Prompt, "/kiro-impl "+feature+" 1.1"):
			// spillover 模擬: task 1.1 の impl skill 呼び出し時に tasks.md の 1.2 を [x] にマーク
			// (impl_loop_skip_test.go と同方針)。1.2 は次イテレーションで per-iteration skip され、
			// taskSummaries に空文字列が append されることで index ↔ taskID 対応が維持される。
			tasksPath := filepath.Join(call.CWD, ".kiro", "specs", feature, "tasks.md")
			if err := os.WriteFile(tasksPath, []byte("- [ ] 1.1 First\n- [x] 1.2 Second\n- [ ] 1.3 Third\n"), 0o644); err != nil {
				t.Fatalf("simulate spillover: WriteFile tasks.md: %v", err)
			}
			return taskSummaryResp("1.1"), nil
		case strings.Contains(call.Prompt, "/kiro-impl "+feature+" 1.3"):
			return taskSummaryResp("1.3"), nil
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

	// task 1.2 が skip された結果、agent.CallAgent は 5 件:
	// 1.1 skill + 1.1 commit + 1.3 skill + 1.3 commit + final-review
	if len(agent.Calls) != 5 {
		t.Fatalf("skip 含み multi-task 連鎖では 5 件の CallAgent を期待: got=%d (%+v)", len(agent.Calls), agent.Calls)
	}

	// 1.2 用の impl skill / commit が発火していないこと (per-iteration skip 維持の前提)
	for i, c := range agent.Calls {
		if strings.Contains(c.Prompt, "/kiro-impl "+feature+" 1.2") {
			t.Errorf("call[%d] が 1.2 用の /kiro-impl を発火: %q (skip 失敗の疑い)", i, c.Prompt)
		}
		if c.Kind == "commit" && c.Options != nil && c.Options.StepID == "commit-1.2" {
			t.Errorf("call[%d] が commit-1.2 を発火: %+v (skip 失敗の疑い)", i, c.Options)
		}
	}

	// table-driven assertion: skip シナリオで「直前 1 段が空文字列」となる挙動を観察する。
	// 全 call 共通で Options.IncludePreviousResponse=false / Options.PreviousResponseContent=""
	// + 後置 block が省略されることを担保する。
	type wantPrev struct {
		callIdx          int
		label            string
		wantPromptPrefix string
	}
	wants := []wantPrev{
		{
			// 先頭タスク 1.1: i==0 → previousSummary=="" → 後置 block 省略
			callIdx:          0,
			label:            "N=1 task 1.1 skill (先頭、後置 block なし)",
			wantPromptPrefix: "/kiro-impl " + feature + " 1.1",
		},
		{
			callIdx:          1,
			label:            "commit-1.1 (agentCallerAdapter 非経由)",
			wantPromptPrefix: "",
		},
		{
			// task 1.3 skill: i==2 だが taskSummaries==["summary-1.1", ""] のため
			// previousSummaryForTaskIndex(2, ...) は taskSummaries[1]=="" を返し、
			// previousResponseBlock("") は空文字を返して block が省略される。
			// これにより「skip 直後の未完了タスクは後置 block なしで起動する」が
			// index ↔ taskID 対応を維持したまま観察できる。
			//
			// 構造的検証ポイント:
			//   - 実装が誤って taskSummaries に空文字列を append 漏れすると i==2 で
			//     taskSummaries[1] が task 1.1 の summary になり、後置 block に
			//     summary-1.1 が現れて本アサーションが落ちる (= 累積禁止 / index ↔ taskID
			//     対応のリグレッション検出)
			callIdx:          2,
			label:            "task 1.3 skill (直前 1.2 が skip、後置 block なし)",
			wantPromptPrefix: "/kiro-impl " + feature + " 1.3",
		},
		{
			callIdx:          3,
			label:            "commit-1.3 (agentCallerAdapter 非経由)",
			wantPromptPrefix: "",
		},
		{
			callIdx:          4,
			label:            "final-review /kiro-validate-impl (per-task ループ外)",
			wantPromptPrefix: "",
		},
	}

	for _, w := range wants {
		w := w
		t.Run(w.label, func(t *testing.T) {
			call := agent.Calls[w.callIdx]
			if call.Options == nil {
				t.Fatalf("call[%d] (%s) Options が nil: 期待 IncludePreviousResponse=false", w.callIdx, w.label)
			}
			if call.Options.IncludePreviousResponse {
				t.Errorf("call[%d] (%s) Options.IncludePreviousResponse=true: kiro-impl-loop は foundation 前置経路を使わないべき",
					w.callIdx, w.label)
			}
			if call.Options.PreviousResponseContent != "" {
				t.Errorf("call[%d] (%s) Options.PreviousResponseContent=%q want=\"\"",
					w.callIdx, w.label, call.Options.PreviousResponseContent)
			}
			if w.wantPromptPrefix != "" && !strings.HasPrefix(call.Prompt, w.wantPromptPrefix) {
				t.Errorf("call[%d] (%s) Prompt は %q で始まるべき (slash-first 不変条件): got=%q",
					w.callIdx, w.label, w.wantPromptPrefix, call.Prompt)
			}
			// skip シナリオでは全 call で後置 block が省略される (直前 1 段 == "" のため)。
			if strings.Contains(call.Prompt, "# Previous Response") {
				t.Errorf("call[%d] (%s) Prompt に `# Previous Response` が含まれてはならない (前段なし / skip 直後): got=%q",
					w.callIdx, w.label, call.Prompt)
			}
		})
	}

	// 1.3 の skill 呼び出し prompt 本文には task 1.1 の summary が含まれないことを substring レベルで担保する。
	// 万一 taskSummaries の append 漏れが発生して 1.1 summary が誤って引き渡される実装ミスがあれば、
	// 本アサーションが落ちて検出する (kiro 層が累積連結を独自で行わない構造的担保にも資する)。
	task13SkillPrompt := agent.Calls[2].Prompt
	if strings.Contains(task13SkillPrompt, taskSummaryFor("1.1")) {
		t.Errorf("task 1.3 skill prompt は task 1.1 summary %q を含まないべき (skip シナリオで累積/再注入禁止): got prompt=%q",
			taskSummaryFor("1.1"), task13SkillPrompt)
	}

	// tasks.md は最終的に 3 件すべて [x] になっていること (workflow が既存マークを破壊していないこと)
	tasksRaw, err := os.ReadFile(filepath.Join(cwd, ".kiro", "specs", feature, "tasks.md"))
	if err != nil {
		t.Fatalf("ReadFile tasks.md: %v", err)
	}
	got := string(tasksRaw)
	for _, want := range []string{"- [x] 1.1", "- [x] 1.2", "- [x] 1.3"} {
		if !strings.Contains(got, want) {
			t.Errorf("tasks.md は %q を含むべき (skip 後も最終的に全 [x]): got=\n%s", want, got)
		}
	}
}
