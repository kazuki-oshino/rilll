// Package workflow の本テストファイルは restart 経路の on-disk fallback
// hit / miss / graceful degradation を観察する統合テストを集約する。
//
// 目的:
//
//   - restart 復元 (= on-disk fallback hit): runDir/<NN>-impl-<prevTaskID>/result.json が
//     summary 非空で存在するとき、previousSummaryForTaskIndex の二段フォールバック (c) 経路が
//     その summary を返す
//   - graceful 退避 (= 二段フォールバック (d) 経路): result.json 不在 / JSON 不正 / Summary 空
//     のいずれでも、呼び出し側に error 伝播せず "" に倒れ、後段起動が継続する
//
// 観察戦略 (2 層構成):
//
// 本テストの orchestration (runKiroImplLoop) は per-iteration ループの先頭で
// `previousSummaryForTaskIndex(i, taskSummaries, runDir, pendingTasks)` を呼ぶが、i==0
// の短絡 (case (a) 「前段なし」) のため、`tasks.md = task1 [x] / task2 [ ]` の
// 「task1 は前ランで完了済」シナリオでは pendingTasks=["1.2"] / iteration i=0 となり、
// 構造上 helper の on-disk fallback path (c) を踏まずに `""` 短絡で抜ける。これは
// 「N=1 タスクは後置 block なしで起動する」契約を構造的に担保する正しい挙動である。
//
// したがって本テストは「runKiroImplLoop 単体で on-disk fallback hit が観察できる」とは
// 主張しない。代わりに以下 2 層で「restart 復元」と「graceful 退避」を同時に observable に
// する設計を取る:
//
//   - Layer A (helper-direct, integration-style fixtures): integration が書き出すレイアウト
//     (`runDir/<NN>-impl-<sanitizedTaskID>/result.json`) で fixture を配置し、helper を
//     pendingTasks=["1.1", "1.2"] / i=1 / taskSummaries=nil の **restart 状態** (前段 in-memory
//     が消えた状態) で直接呼び出す。これにより 4 fixture (hit / miss / broken-json / empty-summary)
//     ごとの戻り値を assert し、restart 復元 (hit) と graceful 退避 ((d) 3 ケース) を同一
//     fixture セットで観察する。
//   - Layer B (runKiroImplLoop drive, true integration): 同じ fixture を保持したまま
//     runKiroImplLoop を駆動し、(i) task 1.2 起動 prompt が i==0 短絡により後置 block を
//     一切含まないこと (= 整備済 on-disk content が task 1.2 の初回 prompt に漏れないこと)、
//     (ii) ワークフローが abort せず graceful に完走すること (= 後段起動継続、graceful 経路)、
//     (iii) task 1.2 が [x] にマークされ `commit-1.2` が success を返すこと、を観察する。
//     これは "事前 seed が存在しても integration orchestration は graceful に短絡し、
//     N=1 task の後置 block 省略契約を逸脱しない" 構造的保証であり、
//     restart 経路で予期せぬ前段 summary 注入が起きないことを 4 fixture すべてに対して
//     横断検査する regression net として機能する。
//
// 両 Layer を合わせて 「restart 復元と graceful 退避が同時に observable」を満たす。
//
// runDir 固定化 (kiro-spec-loop sister test と同方針):
//   - 通常 runDir は `workflow.RunIDFromContext(ctx)` 経由で生成 ID を割り当てるため、test 側からは
//     事前に予測できない。本 test は seed (= prev task result.json) を runDir 配下に置きたいため、
//     `workflowctx.ContextWithRunMetadata` で WorkflowRunID を fixed ID に注入する。これにより
//     runDir = `<cwd>/.rilll/kiro-impl-loop/<fixedRunID>` が確定し、事前 seed が可能になる。
package kiro

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// TestKiroImplLoop_Restart_OnDiskFallback は kiro-impl-loop-facets task 4.6 の 4 fixture を
// table-driven で網羅する統合テスト。Layer A (helper-direct) と Layer B (runKiroImplLoop drive)
// の 2 層で「restart 復元」と「graceful 退避」を同時に observable にする。
//
// シナリオ前提:
//
//	tasks.md = "- [x] 1.1 First\n- [ ] 1.2 Second\n"
//	          (= task 1.1 は前ランで完了済、task 1.2 が次のターゲット)
//	pendingTasks = ["1.2"] (runKiroImplLoop が ExtractPendingLeaves で抽出)
//
//	fixedRunID = "restart-runid-<fixture name>"
//	runDir = <cwd>/.rilll/kiro-impl-loop/<fixedRunID>
//
//	seed 配置 (fixture ごと):
//	  hit          : runDir/01-impl-1_1/result.json (summary="restart-fallback-summary")
//	  miss         : 配置しない (= ENOENT graceful)
//	  broken-json  : runDir/01-impl-1_1/result.json (壊れた JSON、graceful)
//	  empty-summary: runDir/01-impl-1_1/result.json (summary="", graceful)
//
//	agent 応答 (Layer B 共通):
//	  /kiro-impl demo 1.2 → has_inquiry=false / summary=taskSummaryFor("1.2") で即完走
//	  /git-commit         → success=true
//	  /kiro-validate-impl → final-review OK
//
//	期待 agent.Calls (Layer B、全 fixture 共通):
//	  [0] skill /kiro-impl demo 1.2 (i==0 短絡 / 後置 block なし / 前置経路は kiro-impl-loop から未使用)
//	  [1] commit                     (StepID=commit-1.2)
//	  [2] skill /kiro-validate-impl  (final-review)
func TestKiroImplLoop_Restart_OnDiskFallback(t *testing.T) {
	const (
		// seed JSON 本文 (impl_loop が persistImplTaskResult で書き出すスキーマと同形)。
		// hit fixture の Summary は task 4.6 spec 文中の `"restart-fallback-summary"` を採用する。
		seedHit          = `{"order":1,"phase":"impl-1_1","summary":"restart-fallback-summary","lastSession":"sess-restart-1"}`
		seedEmptySummary = `{"order":1,"phase":"impl-1_1","summary":"","lastSession":"sess-restart-1"}`
		seedBrokenJSON   = `{not valid json[`
	)

	cases := []struct {
		name string
		// fixture: seed (= prev task result.json) 本文。"" のとき seed しない (miss シナリオ)。
		seedResultJSON string
		// Layer A (helper-direct) expectations:
		//   previousSummaryForTaskIndex(1, nil, runDir, ["1.1", "1.2"]) の戻り値。
		//   hit fixture では二段フォールバック (c) on-disk hit を観察し restart 復元 summary を返す。
		//   miss / broken-json / empty-summary では二段フォールバック (d) graceful 経路に倒れて "" を返す。
		wantHelperReturn string
	}{
		{
			// 二段フォールバック (c) on-disk hit: result.json 存在 & Summary 非空 → on-disk Summary を返す
			// (Req 1.1)。Layer A で restart 復元観察。
			name:             "hit",
			seedResultJSON:   seedHit,
			wantHelperReturn: "restart-fallback-summary",
		},
		{
			// 二段フォールバック (d-ENOENT): result.json 不在 → "" graceful (Req 1.4)。
			// Layer A で graceful 退避観察。
			name:             "miss",
			seedResultJSON:   "",
			wantHelperReturn: "",
		},
		{
			// 二段フォールバック (d-malformed): result.json 存在するが JSON 不正
			// → json.Unmarshal が失敗、graceful に "" (Req 1.4)。
			name:             "broken-json",
			seedResultJSON:   seedBrokenJSON,
			wantHelperReturn: "",
		},
		{
			// 二段フォールバック (d-empty): result.json 存在 & valid JSON だが summary == ""
			// → payload.Summary が空文字でそのまま返り、graceful 経路に合流 (Req 1.4)。
			name:             "empty-summary",
			seedResultJSON:   seedEmptySummary,
			wantHelperReturn: "",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			feature := "demo"
			// tasks.md は task 1.1 が [x] (前ランで完了済) / task 1.2 が [ ] (次のターゲット)。
			// runKiroImplLoop は ExtractPendingLeaves で task 1.2 のみを pendingTasks に拾う。
			cwd := setupImplFeatureCWD(t, feature, "- [x] 1.1 First\n- [ ] 1.2 Second\n")

			// runDir を予測可能にするため、ContextWithRunMetadata で WorkflowRunID を fixed 値に固定する
			// (kiro-spec-loop sister test TestKiroSpecLoopRestartOnDiskFallback と同方針)。
			// fixture 名を runID に埋め込むことで t.Run の並列実行に対しても干渉しない。
			fixedRunID := "restart-runid-" + tc.name
			ctx := workflowctx.ContextWithRunMetadata(context.Background(),
				workflowctx.RunMetadata{WorkflowRunID: fixedRunID})
			runDir := filepath.Join(cwd, ".rilll", "kiro-impl-loop", fixedRunID)

			// seed: prev task の result.json を事前配置する (miss シナリオは seedResultJSON=="" でスキップ)。
			// path は integration (persistImplTaskResult) が書き出す経路と同形:
			//   runDir/<NN>-impl-<sanitizedTaskID>/result.json
			//   task 1.1 → sanitized phase "impl-1_1"、order=1 → 01-impl-1_1
			if tc.seedResultJSON != "" {
				prevDir := filepath.Join(runDir, "01-impl-1_1")
				if err := os.MkdirAll(prevDir, 0o755); err != nil {
					t.Fatalf("seed mkdir %s: %v", prevDir, err)
				}
				seedPath := filepath.Join(prevDir, "result.json")
				if err := os.WriteFile(seedPath, []byte(tc.seedResultJSON), 0o644); err != nil {
					t.Fatalf("seed write %s: %v", seedPath, err)
				}
			}

			// =================================================================
			// Layer A: helper-direct observation (= Design §Testing Strategy Unit 3-6
			//                                       at integration-style fixtures)
			// =================================================================
			//
			// 観察対象: previousSummaryForTaskIndex の二段フォールバック (c) / (d) 経路を、
			// integration が書き出す runDir レイアウトを fixture として直接観察する。
			// integration orchestration (runKiroImplLoop) は i==0 短絡で踏まないが、helper 自体は
			// restart 状態 (taskSummaries=nil, pendingTasks がすべての leaf を含む) で呼ばれた場合に
			// fixture 通りに振る舞うことを assert する。
			//
			// i=1 / taskSummaries=nil / pendingTasks=["1.1", "1.2"] の組合せが「restart 状態で
			// 2 段目以降に到達した瞬間」の helper への入力を表す。これは TestPreviousSummaryForTaskIndex
			// の (c)/(d) ケースと同形だが、本テストでは runDir / cwd が完全に integration と同じ
			// レイアウトで構築されている点が異なる (Req 1.1 / 1.4 を integration fixture 上で再確認)。
			gotHelperReturn := previousSummaryForTaskIndex(
				1, nil, runDir, []string{"1.1", "1.2"},
			)
			if gotHelperReturn != tc.wantHelperReturn {
				t.Errorf("Layer A (restart 復元 / graceful 退避): previousSummaryForTaskIndex(1, nil, runDir, [1.1, 1.2]) = %q, want %q (fixture %s, runDir=%s)",
					gotHelperReturn, tc.wantHelperReturn, tc.name, runDir)
			}

			// =================================================================
			// Layer B: runKiroImplLoop drive (= true integration / graceful 退避 構造的担保)
			// =================================================================
			//
			// 観察対象: 整備済の on-disk fixture が存在しても integration orchestration は
			// i==0 短絡により task 1.2 の初回 prompt に前段 summary を漏らさないこと、
			// および 4 fixture すべてで workflow が graceful に完走することを assert する。
			//
			// この observation は「restart 経路で seed 内容が意図せず初回 prompt に注入される」
			// regression を 4 fixture すべてで横断防止する regression net として機能する。
			// kiro-impl-loop は foundation 前置経路 (IncludePreviousResponse) を使わず、
			// 前段 summary は `previousResponseBlock(prev)` で skill コマンドの後ろに連結する
			// (slash-first 順) ため、i==0 短絡 (prevSummary == "") のとき後置 block 自体が
			// 省略され、prompt 末尾にも `# Previous Response` が現れない。
			onEvent, eventsPtr := collectEvents()
			ask := &fakeAsk{Selected: feature}

			agent := &scriptedAgent{}
			agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
				switch {
				case call.Kind == "commit":
					return commitOKResp(), nil
				case strings.Contains(call.Prompt, "/kiro-validate-impl"):
					return implFinalReviewOKResp(), nil
				case strings.Contains(call.Prompt, "/kiro-impl "+feature+" 1.2"):
					return taskSummaryResp("1.2"), nil
				default:
					return skillNoInquiryResp(), nil
				}
			}

			if err := runKiroImplLoop(
				ctx,
				cwd,
				onEvent,
				ask,
				agent,
			); err != nil {
				t.Fatalf("Layer B: runKiroImplLoop returned error: %v", err)
			}

			// pendingTasks = ["1.2"] (task 1.1 は既 [x] のため除外) のため、graceful 経路では
			// 3 件の CallAgent を期待: [skill /kiro-impl 1.2, commit, /kiro-validate-impl]。
			// 4 fixture すべてで同じ call 列が観察されることが graceful 退避の構造的担保。
			if len(agent.Calls) != 3 {
				t.Fatalf("Layer B: graceful 経路では 3 件の CallAgent を期待 (fixture %s): got=%d\ncalls=%v",
					tc.name, len(agent.Calls), summarizeCalls(agent.Calls))
			}
			wantKinds := []string{"skill", "commit", "skill"}
			for i, want := range wantKinds {
				if agent.Calls[i].Kind != want {
					t.Errorf("Layer B (fixture %s): call[%d].Kind=%q want=%q", tc.name, i, agent.Calls[i].Kind, want)
				}
			}

			// task 1.2 の skill call (call[0]) を観察する。
			task12Skill := agent.Calls[0]
			if !strings.Contains(task12Skill.Prompt, "/kiro-impl "+feature+" 1.2") {
				t.Errorf("Layer B (fixture %s): call[0] は task 1.2 の skill 呼び出しであるべき: prompt head=%q",
					tc.name, task12Skill.Prompt[:min(len(task12Skill.Prompt), 96)])
			}
			if task12Skill.Options == nil {
				t.Fatalf("Layer B (fixture %s): call[0] Options が nil", tc.name)
			}

			// (i) foundation 前置経路 (IncludePreviousResponse / PreviousResponseContent) は
			//     kiro-impl-loop から未使用のため、4 fixture すべて / 全 call で false / 空文字を維持する。
			if task12Skill.Options.IncludePreviousResponse {
				t.Errorf("Layer B (fixture %s): kiro-impl-loop は foundation 前置経路を使わないので IncludePreviousResponse=false 期待: got=true (seed=%q が誤って task 1.2 prompt に漏れたか?)",
					tc.name, tc.seedResultJSON)
			}
			if task12Skill.Options.PreviousResponseContent != "" {
				t.Errorf("Layer B (fixture %s): kiro-impl-loop は foundation 前置経路を使わないので PreviousResponseContent=\"\" 期待: got=%q (seed が task 1.2 prompt に漏れたか?)",
					tc.name, task12Skill.Options.PreviousResponseContent)
			}

			// (ii) task 1.2 prompt 本文に `# Previous Response` ブロックが現れないこと。
			//      runKiroImplLoop は `previousResponseBlock(prev)` を skill コマンドの後ろに連結する
			//      実装だが、i==0 のとき prevSummary == "" となり previousResponseBlock("") が
			//      空文字を返して block 自体が省略される (graceful degradation)。
			//      4 fixture すべてで seed 内容が prompt に漏れないことを substring レベルで担保。
			if strings.Contains(task12Skill.Prompt, "# Previous Response") {
				t.Errorf("Layer B (fixture %s): task 1.2 prompt は `# Previous Response` ブロックを含まないべき (i==0 短絡 / 後置 block 省略): got=%q",
					tc.name, task12Skill.Prompt)
			}

			// (iii) seed 内容 (hit fixture の "restart-fallback-summary") が task 1.2 prompt の
			//       いずれかに混入していないこと: seed string を substring 検査することで構造的に担保する。
			//       hit fixture でのみ非空 seed があるので、その内容が prompt のいずれにも現れないことを
			//       確認する (i==0 短絡が seed を完全に遮断していることの 2 重ネット)。
			if tc.name == "hit" {
				if strings.Contains(task12Skill.Prompt, "restart-fallback-summary") {
					t.Errorf("Layer B (fixture %s): seed の restart-fallback-summary が task 1.2 prompt に漏洩 (i==0 短絡が seed を遮断していない疑い):\nprompt=%q",
						tc.name, task12Skill.Prompt)
				}
			}

			// (iv) workflow が graceful に完走: WorkflowDone event の発火を確認する。
			//      これは「後段起動継続」(Req 1.4 graceful 経路) と「abort 不発火」(Req 5.3 不抵触) の
			//      構造的担保: 4 fixture すべて (整備済 seed の有無 / 破損 / 空に関わらず) で workflow が
			//      最後まで進むこと。
			if !hasDoneEvent(*eventsPtr, feature) {
				t.Errorf("Layer B (fixture %s): graceful 退避 / 後段継続のため WorkflowDone event 発火を期待: got events=%+v",
					tc.name, *eventsPtr)
			}

			// (v) tasks.md が更新され task 1.2 が [x] にマークされること
			//     (= per-task commit-1.2 が成功し ApplyChildCheck が走った構造的担保、Req 5.1 順次駆動維持)。
			tasksRaw, err := os.ReadFile(filepath.Join(cwd, ".kiro", "specs", feature, "tasks.md"))
			if err != nil {
				t.Fatalf("Layer B (fixture %s): ReadFile tasks.md: %v", tc.name, err)
			}
			if !strings.Contains(string(tasksRaw), "- [x] 1.2") {
				t.Errorf("Layer B (fixture %s): graceful 退避完了後 tasks.md に `- [x] 1.2` が含まれるべき: got=\n%s",
					tc.name, string(tasksRaw))
			}
		})
	}
}
