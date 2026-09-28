// Package workflow の本テストファイルは kiro-spec-loop ワークフローの
// orchestration 周辺ヘルパに対する単体テストおよび success path 統合テストを集約する。
// audit 永続化ヘルパのテストは kiro_spec_loop_audit_test.go に分離している。
package kiro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/facet"
	"github.com/kazuki-oshino/rilll/internal/inquiryloop"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// TestListEligibleFeatures は listEligibleFeatures が
// brief.md 持ち かつ spec.json.phase < tasks-generated の feature のみを返すことを検証する。
//
// 4 種の feature ディレクトリを t.TempDir 上に並べ、戻り値が想定の 2 件のみで、
// かつ辞書順で安定していることまで検証する（要件 1.2 / tasks.md 3.2）。
func TestListEligibleFeatures(t *testing.T) {
	cwd := t.TempDir()
	specsDir := filepath.Join(cwd, ".kiro", "specs")
	if err := os.MkdirAll(specsDir, 0o755); err != nil {
		t.Fatalf("failed to mkdir specs root: %v", err)
	}

	// (i) brief.md あり / spec.json なし → 含む
	mustWriteFeature(t, specsDir, "feat-a-no-spec", "Brief A の本文", "")
	// (ii) brief.md あり / phase=design-generated → 含む
	mustWriteFeature(t, specsDir, "feat-b-design", "Brief B の本文", `{"phase":"design-generated"}`)
	// (iii) brief.md あり / phase=tasks-generated → 含まない
	mustWriteFeature(t, specsDir, "feat-c-tasks", "Brief C の本文", `{"phase":"tasks-generated"}`)
	// (iv) brief.md なし → 含まない（spec.json があっても brief.md が無い時点で対象外）
	mustWriteFeature(t, specsDir, "feat-d-no-brief", "", `{"phase":"requirements-generated"}`)

	got, err := listEligibleFeatures(cwd)
	if err != nil {
		t.Fatalf("listEligibleFeatures returned error: %v", err)
	}

	want := []string{"feat-a-no-spec", "feat-b-design"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("eligible features mismatch:\n got=%v\nwant=%v", got, want)
	}

	// 件数を独立に検証して、(i)(ii) の 2 件のみであることをタスク観測条件に揃える。
	if len(got) != 2 {
		t.Errorf("eligible feature count mismatch: got=%d want=2", len(got))
	}

	// (iii)(iv) が結果に含まれていないことを明示的に検証
	for _, name := range got {
		if name == "feat-c-tasks" {
			t.Errorf("feat-c-tasks (phase=tasks-generated) は含めてはならない")
		}
		if name == "feat-d-no-brief" {
			t.Errorf("feat-d-no-brief (brief.md なし) は含めてはならない")
		}
	}
}

// TestListEligibleFeaturesEmptyBrief は brief.md が存在しても中身が空（空文字 / 空白のみ）の
// 場合に対象外となることを検証する。tasks.md 3.2 の「非空」条件に対応。
func TestListEligibleFeaturesEmptyBrief(t *testing.T) {
	cwd := t.TempDir()
	specsDir := filepath.Join(cwd, ".kiro", "specs")
	if err := os.MkdirAll(specsDir, 0o755); err != nil {
		t.Fatalf("failed to mkdir specs root: %v", err)
	}

	// brief.md が空 → 含まない（spec.json は無いが (a) を満たさない）
	mustWriteFeature(t, specsDir, "feat-empty-brief", "", "")

	got, err := listEligibleFeatures(cwd)
	if err != nil {
		t.Fatalf("listEligibleFeatures returned error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("空 brief.md は対象外となるべき: got=%v", got)
	}
}

// TestListEligibleFeaturesMissingSpecsDir は .kiro/specs/ ディレクトリ自体が存在しない場合に
// エラーではなく空スライスを返すことを検証する（呼び出し側が「対象なし」分岐に流せるように）。
func TestListEligibleFeaturesMissingSpecsDir(t *testing.T) {
	cwd := t.TempDir()

	got, err := listEligibleFeatures(cwd)
	if err != nil {
		t.Fatalf("listEligibleFeatures は specs ディレクトリ不在時にエラーを返さないべき: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("specs ディレクトリ不在時は空スライスを返すべき: got=%v", got)
	}
}

// TestKiroSpecLoopSuccess は kiro-spec-loop の success path を統合テストする
// （task 6.1 / 要件 2.1, 2.3, 2.5, 3.4, 6.1, 6.4, 7.1, 7.4, 8.4）。
//
// 振る舞い前提: fakeAgent (recordedAgent) は 6 段すべて has_inquiry=false で即完走させ、
// /git-commit も常に成功で返す。ループ・回答エージェント呼び出しは発生しない（0 ラウンド完走）。
//
// 検証ポイント (kiro_spec_loop_run_test.go の Smoke 版より厳密):
//  1. agent.CallAgent が 12 件: skill / commit が交互に並ぶ
//  2. 順序が完全一致: [/kiro-spec-init, /git-commit, /kiro-spec-requirements, /git-commit,
//     /kiro-validate-gap, /git-commit, /kiro-spec-design, /git-commit,
//     /kiro-validate-design, /git-commit, /kiro-spec-tasks, /git-commit]
//  3. /kiro-spec-init の query にのみ brief.md 絶対パス + 参照ブロックヘッダが含まれる
//  4. 6 段すべての kiro skill 初回呼び出しに runnerDirective() マーカ ("[runner-directive]") を含む
//  5. すべての呼び出しが resume="" (fresh session: 要件 6.1 / 6.4)
//  6. result.json.terminationReason == "completed"、6 件の steps[].commitOK==true、rounds==0
//  7. workflow.WorkflowDone event が 1 度だけ発火し、Result に feature 名と "6" を含む
func TestKiroSpecLoopSuccess(t *testing.T) {
	feature := "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Selected: feature}
	agent := &recordedAgent{
		// 6 段すべての kiro skill が has_inquiry=false で即完走する応答 (0 ラウンド完走)
		SkillResponse: provider.AgentResponse{
			Success: true,
			StructuredOutput: map[string]any{
				"has_inquiry": false,
				"summary":     "ok",
			},
			SessionID: "session-skill",
		},
		// /git-commit は常に成功
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

	// (1) agent.Call 履歴は 12 件
	if len(agent.Calls) != 12 {
		t.Fatalf("成功パスは skill 6 件 + commit 6 件 = 12 件の agent.CallAgent を期待: got=%d", len(agent.Calls))
	}

	// (2) 順序完全一致: 各呼び出しの prompt prefix と kind を 1 件ずつ検証する。
	// 期待値は steps[] と完全に同じ順序であり、設計仕様 (要件 2.1) のロックイン。
	expectedCalls := []struct {
		prefix string
		kind   string
	}{
		{prefix: "/kiro-spec-init " + feature, kind: "skill"},
		{prefix: "/git-commit", kind: "commit"},
		{prefix: "/kiro-spec-requirements " + feature, kind: "skill"},
		{prefix: "/git-commit", kind: "commit"},
		{prefix: "/kiro-validate-gap " + feature, kind: "skill"},
		{prefix: "/git-commit", kind: "commit"},
		{prefix: "/kiro-spec-design " + feature, kind: "skill"},
		{prefix: "/git-commit", kind: "commit"},
		{prefix: "/kiro-validate-design " + feature, kind: "skill"},
		{prefix: "/git-commit", kind: "commit"},
		{prefix: "/kiro-spec-tasks " + feature, kind: "skill"},
		{prefix: "/git-commit", kind: "commit"},
	}
	// kiro-spec-loop-facets task 5.1 で session A 初回 skill prompt の先頭に
	// `# Persona\n\n<facet content>\n\n---\n\n` が prepend されるようになったため、prompt prefix
	// の完全一致では検証できない。phase 識別子 (slash command + feature 名) を Contains で
	// チェックする (commit prompt は `/git-commit` のみで persona prefix が付かないため
	// 引き続き HasPrefix で検証可能)。
	for i, want := range expectedCalls {
		got := agent.Calls[i]
		if want.kind == "commit" {
			if !strings.HasPrefix(got.Prompt, want.prefix) {
				t.Errorf("call[%d] (commit) prompt prefix mismatch:\n got=%q\nwant prefix=%q", i, got.Prompt, want.prefix)
			}
		} else {
			if !strings.Contains(got.Prompt, want.prefix) {
				t.Errorf("call[%d] (skill) prompt は slash コマンド %q を含むべき:\n got=%q", i, want.prefix, got.Prompt)
			}
		}
		if got.Kind != want.kind {
			t.Errorf("call[%d] kind mismatch: got=%q want=%q", i, got.Kind, want.kind)
		}
		// (5) 全呼び出しが fresh session であること（要件 6.1 / 6.4）
		if got.ResumeSessionID != "" {
			t.Errorf("call[%d] should be fresh session (resume=\"\"): got=%q", i, got.ResumeSessionID)
		}
	}

	// (3) Step 1 (i=0) の query にのみ brief.md 参照ブロックがあり、絶対パスが含まれる
	const refBlockMarker = "以下のマークダウンファイルを参照してください"
	if !strings.Contains(agent.Calls[0].Prompt, refBlockMarker) {
		t.Errorf("Step 1 (/kiro-spec-init) の prompt は参照ブロックヘッダ %q を含むべき: %q",
			refBlockMarker, agent.Calls[0].Prompt)
	}
	briefAbs := filepath.Join(cwd, ".kiro", "specs", feature, "brief.md")
	if abs, absErr := filepath.Abs(briefAbs); absErr == nil {
		briefAbs = abs
	}
	if !strings.Contains(agent.Calls[0].Prompt, briefAbs) {
		t.Errorf("Step 1 prompt は brief.md 絶対パス %q を含むべき: %q", briefAbs, agent.Calls[0].Prompt)
	}
	// 他の kiro skill (i=2,4,6,8,10) では参照ブロックを含まないこと
	for _, idx := range []int{2, 4, 6, 8, 10} {
		if strings.Contains(agent.Calls[idx].Prompt, refBlockMarker) {
			t.Errorf("call[%d] (Step %d 以外) は参照ブロックを含むべきではない: %q",
				idx, idx/2+1, agent.Calls[idx].Prompt)
		}
	}

	// (4) 各 kiro skill の初回呼び出しに runnerDirective() の固有マーカが含まれること。
	// runnerDirective は inquiryloop 内 unexported のため、文字列の特徴的な先頭 "[runner-directive]" で判定する。
	// 0 ラウンド完走では skill 呼び出しはすべて「初回呼び出し」のため、6 件すべてを検証する。
	const directiveMarker = "[runner-directive]"
	for _, idx := range []int{0, 2, 4, 6, 8, 10} {
		if !strings.Contains(agent.Calls[idx].Prompt, directiveMarker) {
			t.Errorf("call[%d] は kiro skill 初回呼び出しなので runnerDirective() マーカ %q を含むべき: %q",
				idx, directiveMarker, agent.Calls[idx].Prompt)
		}
	}
	// /git-commit には directive が付かないこと（要件 6.4 のセッション分離）
	for _, idx := range []int{1, 3, 5, 7, 9, 11} {
		if strings.Contains(agent.Calls[idx].Prompt, directiveMarker) {
			t.Errorf("call[%d] (/git-commit) には runnerDirective() を付加すべきではない: %q",
				idx, agent.Calls[idx].Prompt)
		}
	}

	// (6) result.json の検証
	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonCompleted {
		t.Errorf("terminationReason mismatch: got=%q want=%q", rr.TerminationReason, RunReasonCompleted)
	}
	if rr.Feature != feature {
		t.Errorf("feature mismatch: got=%q want=%q", rr.Feature, feature)
	}
	if rr.WorkflowID != "kiro-spec-loop" {
		t.Errorf("workflowId mismatch: got=%q want=%q", rr.WorkflowID, "kiro-spec-loop")
	}
	if len(rr.Steps) != 6 {
		t.Fatalf("6 件の step を期待: got=%d", len(rr.Steps))
	}
	wantPhases := []string{
		"spec-init",
		"spec-requirements",
		"validate-gap",
		"spec-design",
		"validate-design",
		"spec-tasks",
	}
	for i, s := range rr.Steps {
		if s.Order != i+1 {
			t.Errorf("steps[%d].Order mismatch: got=%d want=%d", i, s.Order, i+1)
		}
		if s.Phase != wantPhases[i] {
			t.Errorf("steps[%d].Phase mismatch: got=%q want=%q", i, s.Phase, wantPhases[i])
		}
		if !s.CommitOK {
			t.Errorf("steps[%d].CommitOK=true を期待 (phase=%q)", i, s.Phase)
		}
		if s.Rounds != 0 {
			t.Errorf("steps[%d].Rounds=0 を期待 (has_inquiry=false 即完走): got=%d", i, s.Rounds)
		}
	}
	if rr.CompletedAt == nil {
		t.Errorf("completed パスでは CompletedAt は non-nil であるべき")
	}
	if rr.AbortStep != nil {
		t.Errorf("completed パスでは AbortStep は nil であるべき: got=%+v", rr.AbortStep)
	}

	// (7) workflow.WorkflowDone event が 1 度だけ発火し、message に feature 名と "6" を含む
	doneCount := 0
	for _, e := range *eventsPtr {
		if e.Type == workflow.WorkflowDone {
			doneCount++
		}
	}
	if doneCount != 1 {
		t.Errorf("workflow.WorkflowDone event は 1 度だけ発火すべき: got=%d", doneCount)
	}
	if !hasDoneEvent(*eventsPtr, feature) {
		t.Errorf("Done event に feature 名 %q が含まれるべき: %+v", feature, *eventsPtr)
	}
	if !hasDoneEvent(*eventsPtr, "6") {
		t.Errorf("Done event に '6' (完了プロンプト数) が含まれるべき: %+v", *eventsPtr)
	}

	// =========================================================================
	// (8) prompt golden: slash-first 順 (slash command 先頭 → persona block 後置 → previous response block 後置)
	// =========================================================================
	//
	// 観察対象:
	//   (a) session A 初回 skill 呼び出しの recordedAgentCall.Prompt が
	//       `/kiro-{phase} {feature}` で始まり (Claude Code の skill 起動が
	//       先頭トークンに依存するため slash-first 順を採用)、その後に
	//       `\n\n---\n\n# Persona\n\n<facet content>` を含む。
	//   (b) Options.StepName が `<NN>-<phase>` 規約 (akashic StepID 不変保証)
	//       Options.StepID が非空 (akashic agent_calls.step_id 記録の seam)
	//   (c) Previous Response の埋め込み (slash-first 順では prompt body に直接埋め込まれる):
	//         1 段目 spec-init : prompt body に `# Previous Response` を含まない (前段なし)
	//         2-6 段目        : prompt body に `\n\n---\n\n# Previous Response\n\nok` を含む
	//                           (recordedAgent の StructuredOutput.summary="ok" が in-memory hit で伝播)
	//       いずれの段でも spec-loop は foundation 側 `IncludePreviousResponse` 経路を使わないため
	//       Options.IncludePreviousResponse=false / PreviousResponseContent="" となる。
	//   (d) commit prompt には persona block も Previous Response block も付かない
	//       (fix / commit phase の CallAgentOptions は touch しない)
	//   (e) runDir/<NN>-<phase>/result.json が 6 件揃う (永続化レイアウト不変)

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
		wantSlashHead := fmt.Sprintf("/kiro-%s %s", phase, feature)
		wantStepName := fmt.Sprintf("%02d-%s", i+1, phase)
		got := agent.Calls[idx]

		// (a) slash-first 順: prompt 先頭が slash command (`/kiro-{phase} {feature}`)
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

// =============================================================================
// task 6.2: abort scenarios の統合テスト 7 件
// =============================================================================
//
// 構成:
//   - scriptedAgent: workflowctx.AgentCaller を満たす柔軟な test agent。各呼び出しの
//     Kind / Prompt / 履歴に応じて応答を動的に組み立てたいシナリオ用に、
//     Respond クロージャを取る形で recordedAgent と差別化する。
//   - classifyAgentCall: prompt から Kind ("commit"/"answerer"/"skill") を推定する
//     共通ヘルパ。recordedAgent は kind="commit" / "skill" の 2 値のみだが、
//     loop-limit シナリオで answerer 呼び出しを区別する必要があるため拡張する。
//   - skill / commit / answerer の典型応答を組み立てる factory 関数群（テストで
//     重複コードを避ける）。
//   - 7 シナリオの TestKiroSpecLoop* テスト本体。
//
// 各シナリオで共通に検証する項目:
//   1. agent.Calls の件数および順序が abort 直前で停止していること（後続 step / commit が呼ばれない）
//   2. result.json.terminationReason が想定 enum 値であること
//   3. result.json.abortStep / result.json.failure が想定どおり書かれること
//   4. workflow.WorkflowEvent{Error}.Message に「どの kiro プロンプト（または /git-commit）で」
//      「どの abort 理由か」を識別できる文言が含まれること（要件 9.3）

// scriptedAgent は workflowctx.AgentCaller を満たす柔軟なテストエージェント。
//
// recordedAgent は SkillResponse / CommitResponse の 2 系統固定値しか返せないため、
// 「3 回目の commit だけ失敗」「step ごとに has_inquiry を切り替える」等の
// 多段シナリオを動かせない。scriptedAgent は Respond クロージャを取り、
// 各呼び出しの prompt / kind / 過去履歴に基づいて動的な応答を返せるようにする。
//
// Respond は Calls の append 後に呼ばれるため、Respond 内から len(s.Calls) を
// 参照すれば「これは何回目の呼び出しか」をシンプルに判定できる。
type scriptedAgent struct {
	Respond                        func(call recordedAgentCall) (provider.AgentResponse, error)
	DisableAutoSpecDesignArtifacts bool
	Calls                          []recordedAgentCall
}

func (s *scriptedAgent) CallAgent(
	_ context.Context,
	prompt string,
	cwd string,
	onEvent provider.AgentEventCallback,
	resume string,
	options *workflowctx.CallAgentOptions,
) (provider.AgentResponse, error) {
	call := recordedAgentCall{
		Prompt:          prompt,
		CWD:             cwd,
		ResumeSessionID: resume,
		Options:         options,
		OnEvent:         onEvent,
		Kind:            classifyAgentCall(prompt),
	}
	s.Calls = append(s.Calls, call)
	if s.Respond == nil {
		// Respond 未設定時は success=true / has_inquiry=false で fallback する。
		// 失敗を再現したい場合は明示的に Respond をセットすること。
		resp := ensureValidationVerdictForTest(skillNoInquiryResp(), options)
		maybeWriteSpecDesignArtifacts(s.DisableAutoSpecDesignArtifacts, call, resp)
		return resp, nil
	}
	resp, err := s.Respond(call)
	resp = ensureValidationVerdictForTest(resp, options)
	maybeWriteSpecDesignArtifacts(s.DisableAutoSpecDesignArtifacts, call, resp)
	return resp, err
}

func maybeWriteSpecDesignArtifacts(disabled bool, call recordedAgentCall, resp provider.AgentResponse) {
	if disabled || !resp.Success {
		return
	}
	feature, ok := featureFromSlashPrompt(call.Prompt, "/kiro-spec-design")
	if !ok {
		if call.Kind != "fix-edit" {
			return
		}
		feature, ok = featureFromFixPrompt(call.Prompt)
		if !ok || call.Options == nil || call.Options.StepName != "04-spec-design-fix-edit" {
			return
		}
	}
	if call.Kind == "skill" && hasInquiry(resp) {
		return
	}
	specDir := filepath.Join(call.CWD, ".kiro", "specs", feature)
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		return
	}
	designPath := filepath.Join(specDir, "design.md")
	if _, err := os.Stat(designPath); errors.Is(err, os.ErrNotExist) {
		_ = os.WriteFile(designPath, []byte("# Design Document\n"), 0o644)
	}
	specPath := filepath.Join(specDir, "spec.json")
	raw, err := os.ReadFile(specPath)
	var spec map[string]any
	if err == nil {
		_ = json.Unmarshal(raw, &spec)
	}
	if spec == nil {
		spec = map[string]any{"feature_name": feature}
	}
	approvals, _ := spec["approvals"].(map[string]any)
	if approvals == nil {
		approvals = map[string]any{}
		spec["approvals"] = approvals
	}
	req, _ := approvals["requirements"].(map[string]any)
	if req == nil {
		req = map[string]any{}
		approvals["requirements"] = req
	}
	req["generated"] = true
	req["approved"] = true
	design, _ := approvals["design"].(map[string]any)
	if design == nil {
		design = map[string]any{}
		approvals["design"] = design
	}
	design["generated"] = true
	if _, ok := design["approved"].(bool); !ok {
		design["approved"] = false
	}
	tasks, _ := approvals["tasks"].(map[string]any)
	if tasks == nil {
		tasks = map[string]any{"generated": false, "approved": false}
		approvals["tasks"] = tasks
	}
	spec["phase"] = "design-generated"
	out, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return
	}
	out = append(out, '\n')
	_ = os.WriteFile(specPath, out, 0o644)
}

func featureFromSlashPrompt(prompt, command string) (string, bool) {
	fields := strings.Fields(prompt)
	if len(fields) < 2 || fields[0] != command {
		return "", false
	}
	return fields[1], true
}

func featureFromFixPrompt(prompt string) (string, bool) {
	const marker = ".kiro/specs/"
	idx := strings.Index(prompt, marker)
	if idx < 0 {
		return "", false
	}
	rest := prompt[idx+len(marker):]
	end := strings.Index(rest, "/")
	if end <= 0 {
		return "", false
	}
	return rest[:end], true
}

func hasInquiry(resp provider.AgentResponse) bool {
	raw, ok := resp.StructuredOutput.(map[string]any)
	if !ok {
		return false
	}
	v, _ := raw["has_inquiry"].(bool)
	return v
}

// classifyAgentCall は prompt のプレフィックスから kind を推定する。
//
// 規約:
//   - "/git-commit"                  始まり → "commit"
//   - "[fix-phase-edit]"             始まり → "fix-edit"  (spec_loop: Turn 1 OutputFormat=nil)
//   - "[fix-phase-judge]"            始まり → "fix-judge" (spec_loop: Turn 2 schema 強制)
//   - "[fix-phase]"                  始まり → "fix"       (legacy fallback、現コード未使用)
//   - "[final-review-fix-edit]"      始まり → "fix-edit"  (impl_loop final-review Phase 2)
//   - "[final-review-fix-judge]"     始まり → "fix-judge" (impl_loop final-review Phase 3)
//   - "[final-review-test-edit]"     始まり → "test-edit"  (impl_loop final-review Phase 4 Turn 1)
//   - "[final-review-test-judge]"    始まり → "test-judge" (impl_loop final-review Phase 4 Turn 2)
//   - prompt が "# 回答方針" を含む                → "answerer"  (buildAnswerPrompt 末尾固定セクション)
//   - それ以外                              → "skill"     (kiro skill 初回 query または resume 後の query)
//
// 順序の注意: "[fix-phase-edit]" / "[fix-phase-judge]" は "[fix-phase]" を真の prefix として持つため、
// より具体的な edit / judge 判定を先に置く必要がある。
//
// kiro-spec-loop-facets task 5.1 で session A 初回 skill prompt の先頭にも
// `# Persona\n\n<facet content>\n\n---\n\n` が prepend されるようになったため、
// 旧 `strings.HasPrefix(prompt, "# Persona")` ベースの answerer 判定では
// session A skill と session B answerer を区別できなくなった。session B answerer prompt の
// 末尾には `# 回答方針` (answerPolicySection 固定文) が必ず含まれる一方、
// session A skill prompt にはこのセクションが現れないため、本マーカで両者を分離する。
//
// final-review (impl_loop) の "[final-review-fix-edit]" / "[final-review-fix-judge]" は spec_loop の
// "[fix-phase-edit]" / "[fix-phase-judge]" と marker こそ違うが、test scriptedAgent の `Respond`
// クロージャにとっては「Turn 1 OutputFormat=nil 系か、Turn 2 schema 系か」を区別できれば十分なので、
// 同じ kind ("fix-edit" / "fix-judge") にマップする。1 シナリオ内で spec_loop fix と final-review fix が
// 混在するテストは存在しない (per-task ループと final-review は別関数で分離済) ため、この共有で
// 衝突は起きない。
//
// final-review Phase 4 (test-edit / test-judge) は同じく Turn 1 OutputFormat=nil → Turn 2 schema 強制の
// 2-turn 構成だが、Phase 2-3 (fix-edit / fix-judge) と同一シナリオ内で混在し得る (Phase 2-3 → Phase 4
// と進む E2E テスト) ため、別 kind ("test-edit" / "test-judge") に分けて Respond クロージャが
// それぞれの応答を区別できるようにする。
//
// resume 後の skill query は inquiry に対する回答テキストそのもの（# Persona ヘッダなし）であり、
// `/`-prefix も持たないため、判別は prompt の文面に依存する。テストで answerer 応答の
// summary には "# Persona" を含めない取り決めにすれば classifyAgentCall は安定する。
func classifyAgentCall(prompt string) string {
	switch {
	case strings.HasPrefix(prompt, "/git-commit"):
		return "commit"
	case strings.HasPrefix(prompt, "[fix-phase-edit]"):
		return "fix-edit"
	case strings.HasPrefix(prompt, "[fix-phase-judge]"):
		return "fix-judge"
	case strings.HasPrefix(prompt, "[fix-phase]"):
		return "fix"
	case strings.HasPrefix(prompt, "[final-review-fix-edit]"):
		return "fix-edit"
	case strings.HasPrefix(prompt, "[final-review-fix-judge]"):
		return "fix-judge"
	case strings.HasPrefix(prompt, "[final-review-test-edit]"):
		return "test-edit"
	case strings.HasPrefix(prompt, "[final-review-test-judge]"):
		return "test-judge"
	case strings.Contains(prompt, "# 回答方針"):
		return "answerer"
	default:
		return "skill"
	}
}

// skillNoInquiryResp は kiro skill が has_inquiry=false で即完走する典型応答。
// 構造化出力で has_inquiry=false / summary を返す。SessionID は監査用ダミー値。
func skillNoInquiryResp() provider.AgentResponse {
	return provider.AgentResponse{
		Success: true,
		StructuredOutput: map[string]any{
			"has_inquiry": false,
			"summary":     "ok",
		},
		SessionID: "session-skill",
	}
}

// skillInquiryResp は kiro skill が has_inquiry=true で問い合わせを返す典型応答。
// title / question を満たす最小 inquiry オブジェクトを乗せ、parseResponse のスキーマ要件
// （has_inquiry=true なら inquiry オブジェクト必須 / 内 required は title・question）を満たす。
func skillInquiryResp() provider.AgentResponse {
	return provider.AgentResponse{
		Success: true,
		StructuredOutput: map[string]any{
			"has_inquiry": true,
			"summary":     "判断保留中",
			"inquiry": map[string]any{
				"title":      "判断要求",
				"background": "X が必要",
				"question":   "Y にするか Z にするか？",
			},
		},
		SessionID: "session-skill",
	}
}

// answerOKResp は回答エージェントが返す「応答テキスト」の典型値。
// 回答エージェントもスキーマで json_schema を強制されるため、has_inquiry=false /
// summary（= 回答テキスト本体）を返す形にする。inquiry オブジェクトは含まない。
//
// 注意: summary には "# Persona" を含めない（含めると classifyAgentCall が
// 後続 resume 呼び出しを誤って "answerer" に分類してしまうため）。
func answerOKResp() provider.AgentResponse {
	return provider.AgentResponse{
		Success: true,
		StructuredOutput: map[string]any{
			"has_inquiry": false,
			"summary":     "回答テキスト本体（人間レビュー用の判断と理由）",
		},
		SessionID: "session-answer",
	}
}

// commitOKResp は /git-commit が成功で完了した応答。
func commitOKResp() provider.AgentResponse {
	return provider.AgentResponse{
		Success:   true,
		Content:   "commit ok",
		SessionID: "session-commit",
	}
}

// commitFailResp は /git-commit が success=false で失敗した応答。
// Content は失敗理由のダミー文字列（result.json.steps[].commitOutput に保存される）。
func commitFailResp() provider.AgentResponse {
	return provider.AgentResponse{
		Success:   false,
		Content:   "no changes to commit (test-induced failure)",
		SessionID: "session-commit-fail",
	}
}

// hasErrorEventAll は workflow.WorkflowError 種別 event の Message にすべての sub 文字列が含まれているかを判定する。
// すべての文字列が同一 event に含まれる必要があるため、要件 9.3「どのプロンプトで」「どの理由で」
// の同時包含検証で使う。
func hasErrorEventAll(events []workflow.WorkflowEvent, subs ...string) bool {
	for _, e := range events {
		if e.Type != workflow.WorkflowError {
			continue
		}
		all := true
		for _, sub := range subs {
			if !strings.Contains(e.Message, sub) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func outputSchemaRequires(out *provider.OutputFormat, name string) bool {
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

func ensureValidationVerdictForTest(resp provider.AgentResponse, options *workflowctx.CallAgentOptions) provider.AgentResponse {
	if options == nil || !outputSchemaRequires(options.OutputFormat, "verdict") {
		return resp
	}
	root, ok := resp.StructuredOutput.(map[string]any)
	if !ok || root == nil {
		return resp
	}
	// verdict / has_advisory のうち、テスト側が明示していないものを安全側 (verdict=go, has_advisory=false) で補完する。
	// 既存テスト (advisory 未対応のシナリオ群) を改修せずに ValidationSchema 拡張に追随させるためのシム。
	_, hasVerdict := root["verdict"]
	_, hasAdvisory := root["has_advisory"]
	if hasVerdict && hasAdvisory {
		return resp
	}
	copied := make(map[string]any, len(root)+2)
	for k, v := range root {
		copied[k] = v
	}
	if !hasVerdict {
		copied["verdict"] = string(inquiryloop.VerdictGo)
	}
	if !hasAdvisory {
		copied["has_advisory"] = false
	}
	resp.StructuredOutput = copied
	return resp
}

// TestKiroSpecLoopCommitFailure は task 6.2 シナリオ #1。
//
// 振る舞い前提: 6 段の skill はすべて has_inquiry=false で即完走するが、
// 3 段目 (`validate-gap`) の `/git-commit` のみ success=false を返す。
//
// 期待:
//   - 4 段目以降の skill / commit は呼ばれない（agent.Calls = 6 件で停止）
//   - result.json.terminationReason == "commit-failure"
//   - result.json.abortStep = {Order:3, Phase:"validate-gap"}
//   - result.json.steps の 3 件目は CommitOK=false、先頭 2 件は CommitOK=true
//   - workflow.WorkflowEvent{Error}.Message は "/git-commit" と "validate-gap" を同時包含する
func TestKiroSpecLoopCommitFailure(t *testing.T) {
	feature := "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Selected: feature}

	// 自身の Calls 履歴を見て commit 系の呼び出し回数をカウントし、3 回目だけ失敗させる。
	// scriptedAgent.Calls には append 後に Respond が呼ばれる契約なので、
	// 「現在の commit が何回目か」は履歴を走査して数える。
	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "commit":
			commitN := 0
			for _, c := range agent.Calls {
				if c.Kind == "commit" {
					commitN++
				}
			}
			if commitN == 3 {
				return commitFailResp(), nil
			}
			return commitOKResp(), nil
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

	// 期待呼び出し: skill1, commit1, skill2, commit2, skill3, commit3 = 6 件で停止
	if len(agent.Calls) != 6 {
		t.Fatalf("3 段目の commit 失敗で 6 件 (skill+commit x3) を期待: got=%d calls=%v",
			len(agent.Calls), summarizeCalls(agent.Calls))
	}
	expected := []struct{ prefix, kind string }{
		{"/kiro-spec-init " + feature, "skill"},
		{"/git-commit", "commit"},
		{"/kiro-spec-requirements " + feature, "skill"},
		{"/git-commit", "commit"},
		{"/kiro-validate-gap " + feature, "skill"},
		{"/git-commit", "commit"},
	}
	// kiro-spec-loop-facets task 5.1 で session A skill prompt 先頭に persona prefix が
	// 入るようになったため、skill 系は Contains で判定する (commit は無変更で HasPrefix のまま)。
	for i, want := range expected {
		got := agent.Calls[i]
		if want.kind == "commit" {
			if !strings.HasPrefix(got.Prompt, want.prefix) {
				t.Errorf("call[%d] (commit) prompt prefix mismatch: got=%q want=%q", i, got.Prompt, want.prefix)
			}
		} else {
			if !strings.Contains(got.Prompt, want.prefix) {
				t.Errorf("call[%d] (skill) prompt は slash コマンド %q を含むべき: got=%q", i, want.prefix, got.Prompt)
			}
		}
		if got.Kind != want.kind {
			t.Errorf("call[%d] kind mismatch: got=%q want=%q", i, got.Kind, want.kind)
		}
	}

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonCommitFailure {
		t.Errorf("terminationReason mismatch: got=%q want=%q", rr.TerminationReason, RunReasonCommitFailure)
	}
	if rr.AbortStep == nil || rr.AbortStep.Order != 3 || rr.AbortStep.Phase != "validate-gap" {
		t.Errorf("AbortStep mismatch: got=%+v want=Order=3 Phase=validate-gap", rr.AbortStep)
	}
	if len(rr.Steps) != 3 {
		t.Fatalf("3 件の step を期待: got=%d steps=%+v", len(rr.Steps), rr.Steps)
	}
	if !rr.Steps[0].CommitOK || !rr.Steps[1].CommitOK {
		t.Errorf("先頭 2 段は CommitOK=true: got=%+v", rr.Steps)
	}
	if rr.Steps[2].CommitOK {
		t.Errorf("3 段目は CommitOK=false（commit 失敗): got=%+v", rr.Steps[2])
	}
	// commit-failure では engine.FailureDetail を介さないので Failure フィールドは nil
	if rr.Failure != nil {
		t.Errorf("commit-failure では Failure は nil（engine 由来 abort ではないため）: got=%+v", rr.Failure)
	}

	if !hasErrorEventAll(*eventsPtr, "/git-commit", "validate-gap") {
		t.Errorf("Error event は '/git-commit' と 'validate-gap' を同時包含すべき: %+v", *eventsPtr)
	}
}

// TestKiroSpecLoopLoopLimit は task 6.2 シナリオ #2。
//
// 振る舞い前提: 1 段目は has_inquiry=false で即完走し commit 1 も成功する。
// 2 段目 (`spec-requirements`) は初回応答 + 5 ラウンドすべて has_inquiry=true を返し、
// 回答エージェントは毎回 has_inquiry=false / summary="回答テキスト本体" を返す。
//
// 期待:
//   - 2 段目で MaxRounds=5 を超過して loop-limit abort
//   - 2 段目 commit / 3 段目以降 skill は呼ばれない
//   - result.json.terminationReason == "loop-limit"
//   - result.json.failure.round == 5（engine.FailureDetail.Round が 5）
//   - result.json.steps[1].rounds == 5
//   - workflow.WorkflowEvent{Error}.Message は "spec-requirements" と "loop-limit" を同時包含する
func TestKiroSpecLoopLoopLimit(t *testing.T) {
	feature := "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Selected: feature}

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "commit":
			return commitOKResp(), nil
		case "answerer":
			return answerOKResp(), nil
		case "skill":
			// step 1: /kiro-spec-init は has_inquiry=false で即完走
			// kiro-spec-loop-facets task 5.1 で session A skill prompt の先頭に persona prefix が
			// 入るようになったため HasPrefix では識別できない。素の slash command を Contains で識別する。
			if strings.Contains(call.Prompt, "/kiro-spec-init") {
				return skillNoInquiryResp(), nil
			}
			// step 2: /kiro-spec-requirements 初回応答 と その resume すべて has_inquiry=true を返す
			// （resume の prompt は answerOKResp().summary なので classifyAgentCall は "skill" を返す）
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

	// 期待呼び出し:
	//   step 1: skill1 (initial /kiro-spec-init), commit1 = 2 件
	//   step 2: skill2 (initial /kiro-spec-requirements) + 5 rounds × 2 calls (answerer + skill resume) = 11 件
	//   合計: 2 + 11 = 13 件
	if len(agent.Calls) != 13 {
		t.Fatalf("loop-limit シナリオでは skill1+commit1+11 = 13 件を期待: got=%d calls=%v",
			len(agent.Calls), summarizeCalls(agent.Calls))
	}

	// 3 段目以降の skill は呼ばれてはならない（2 段目 abort で停止）
	forbiddenPrefixes := []string{
		"/kiro-validate-gap",
		"/kiro-spec-design",
		"/kiro-validate-design",
		"/kiro-spec-tasks",
	}
	for _, c := range agent.Calls {
		for _, fp := range forbiddenPrefixes {
			if strings.HasPrefix(c.Prompt, fp) {
				t.Errorf("3 段目以降の skill は呼ばれてはならない: prompt=%q", c.Prompt)
			}
		}
	}

	// commit は step 1 完了後の 1 件のみ
	commitCount := 0
	answererCount := 0
	for _, c := range agent.Calls {
		switch c.Kind {
		case "commit":
			commitCount++
		case "answerer":
			answererCount++
		}
	}
	if commitCount != 1 {
		t.Errorf("step 2 abort では commit は step 1 完了分の 1 件のみを期待: got=%d", commitCount)
	}
	if answererCount != 5 {
		t.Errorf("loop-limit では 5 ラウンド分の answerer 呼び出しを期待: got=%d", answererCount)
	}

	// step 2 の resume 連鎖検証: 回答後の skill 呼び出しは resumeSessionID="session-skill" を使う
	for i, c := range agent.Calls {
		if c.Kind != "skill" {
			continue
		}
		// step 1 初回 (i=0) と step 2 初回 (i=2) は resume="" の fresh session
		if i == 0 || i == 2 {
			if c.ResumeSessionID != "" {
				t.Errorf("call[%d] (skill 初回) は resume=\"\" であるべき: got=%q", i, c.ResumeSessionID)
			}
			continue
		}
		// それ以外の skill 呼び出し（step 2 の resume 連鎖）は session-skill を resume に渡している
		if c.ResumeSessionID != "session-skill" {
			t.Errorf("call[%d] (step 2 resume skill) は resume=\"session-skill\" を期待: got=%q",
				i, c.ResumeSessionID)
		}
	}
	// 回答エージェント呼び出しは毎回 fresh session
	for i, c := range agent.Calls {
		if c.Kind != "answerer" {
			continue
		}
		if c.ResumeSessionID != "" {
			t.Errorf("call[%d] (answerer) は毎回 resume=\"\" であるべき: got=%q", i, c.ResumeSessionID)
		}
	}

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonLoopLimit {
		t.Errorf("terminationReason mismatch: got=%q want=%q", rr.TerminationReason, RunReasonLoopLimit)
	}
	if rr.AbortStep == nil || rr.AbortStep.Order != 2 || rr.AbortStep.Phase != "spec-requirements" {
		t.Errorf("AbortStep mismatch: got=%+v want=Order=2 Phase=spec-requirements", rr.AbortStep)
	}
	if rr.Failure == nil {
		t.Fatalf("loop-limit では Failure 非 nil を期待: got=nil")
	}
	if rr.Failure.Round != 5 {
		t.Errorf("Failure.Round=5 を期待 (MaxRounds 超過時の最終ラウンド番号): got=%d", rr.Failure.Round)
	}
	if rr.Failure.Reason != string(RunReasonLoopLimit) {
		t.Errorf("Failure.Reason=%q を期待: got=%q", RunReasonLoopLimit, rr.Failure.Reason)
	}
	if len(rr.Steps) != 2 {
		t.Fatalf("step result 2 件 (step1 完走 + step2 abort) を期待: got=%d", len(rr.Steps))
	}
	if rr.Steps[1].Rounds != 5 {
		t.Errorf("step[1].Rounds=5 を期待: got=%d", rr.Steps[1].Rounds)
	}
	if rr.Steps[1].CommitOK {
		t.Errorf("step[1].CommitOK=false を期待 (loop-limit で commit 未到達)")
	}

	if !hasErrorEventAll(*eventsPtr, "spec-requirements", string(RunReasonLoopLimit)) {
		t.Errorf("Error event は 'spec-requirements' と 'loop-limit' を同時包含すべき: %+v", *eventsPtr)
	}
}

// TestKiroSpecLoopSchemaViolation は task 6.2 シナリオ #3。
//
// 振る舞い前提: 1 段目で StructuredOutput=nil + Content に JSON ブロックなしの応答を返す。
// inquiryloop.Engine は初回 schema-violation でも schemaRepairDirective() で 1 回 retry するが、
// repair も同じ malformed 応答を返すため、最終的に schema-violation で abort する。
//
// 期待:
//   - agent.Calls は 2 件（初回 skill + skill-repair）で停止
//   - result.json.terminationReason == "schema-violation"
//   - result.json.abortStep = {Order:1, Phase:"spec-init"}
//   - result.json.failure.rawContent に応答 Content が記録される
//   - workflow.WorkflowEvent{Error}.Message は "spec-init" と "schema-violation" を同時包含する
func TestKiroSpecLoopSchemaViolation(t *testing.T) {
	feature := "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Selected: feature}

	const malformedContent = "プレーンテキスト応答（JSON ブロックなし）"
	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		// step 1 初回 skill 応答: StructuredOutput=nil + JSON 抽出不能 Content
		return provider.AgentResponse{
			Success:   true,
			Content:   malformedContent,
			SessionID: "session-skill",
		}, nil
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

	// 初回 skill + 1 回の skill-repair = 2 件で停止する (commit には進まない)。
	if len(agent.Calls) != 2 {
		t.Fatalf("schema violation では skill + repair = 2 件で停止すべき: got=%d calls=%v",
			len(agent.Calls), summarizeCalls(agent.Calls))
	}
	// kiro-spec-loop-facets task 5.1 で session A skill prompt 先頭に persona prefix が
	// 入るようになったため HasPrefix から Contains に変更する (前置は foundation Spec C 経路で
	// 入る `# Previous Response` ブロックの後段にも入りうるため、prompt 全体に対する Contains が安定)。
	if !strings.Contains(agent.Calls[0].Prompt, "/kiro-spec-init "+feature) {
		t.Errorf("call[0] は /kiro-spec-init を含むべき: got=%q", agent.Calls[0].Prompt)
	}
	// call[1] は schemaRepairDirective() で同 session を resume する。
	if !strings.Contains(agent.Calls[1].Prompt, "[runner-repair-directive]") {
		t.Errorf("call[1] は repair directive を含むべき: got=%q", agent.Calls[1].Prompt)
	}
	if agent.Calls[1].ResumeSessionID != "session-skill" {
		t.Errorf("call[1] は 1 件目応答の session を resume すべき: got=%q want=%q",
			agent.Calls[1].ResumeSessionID, "session-skill")
	}

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonSchemaViolation {
		t.Errorf("terminationReason mismatch: got=%q want=%q", rr.TerminationReason, RunReasonSchemaViolation)
	}
	if rr.AbortStep == nil || rr.AbortStep.Order != 1 || rr.AbortStep.Phase != "spec-init" {
		t.Errorf("AbortStep mismatch: got=%+v want=Order=1 Phase=spec-init", rr.AbortStep)
	}
	if rr.Failure == nil {
		t.Fatalf("schema-violation では Failure 非 nil を期待: got=nil")
	}
	if rr.Failure.Reason != string(RunReasonSchemaViolation) {
		t.Errorf("Failure.Reason=%q を期待: got=%q", RunReasonSchemaViolation, rr.Failure.Reason)
	}
	if rr.Failure.RawContent != malformedContent {
		t.Errorf("Failure.RawContent は応答 Content を保存すべき:\n got=%q\nwant=%q",
			rr.Failure.RawContent, malformedContent)
	}
	if len(rr.Steps) != 1 {
		t.Fatalf("step result 1 件を期待: got=%d", len(rr.Steps))
	}
	if rr.Steps[0].CommitOK {
		t.Errorf("step[0].CommitOK=false を期待 (schema violation で commit 未到達)")
	}

	if !hasErrorEventAll(*eventsPtr, "spec-init", string(RunReasonSchemaViolation)) {
		t.Errorf("Error event は 'spec-init' と 'schema-violation' を同時包含すべき: %+v", *eventsPtr)
	}
}

// TestKiroSpecLoopPromptFailure は task 6.2 シナリオ #4。
//
// 振る舞い前提: 1 段目の agent.Call が err を返す（provider レベルのエラー）。
// engine が ReasonPromptFailure で即時 abort する。
//
// 期待:
//   - agent.Calls は 1 件のみで停止
//   - result.json.terminationReason == "prompt-failure"
//   - result.json.abortStep = {Order:1, Phase:"spec-init"}
//   - result.json.failure.message に agent error 文言が含まれる
//   - workflow.WorkflowEvent{Error}.Message は "spec-init" と "prompt-failure" を同時包含する
func TestKiroSpecLoopPromptFailure(t *testing.T) {
	feature := "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Selected: feature}

	const agentErrMsg = "test-induced agent error"
	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		// step 1 初回で provider error を返し、engine が ReasonPromptFailure で abort する
		return provider.AgentResponse{}, errors.New(agentErrMsg)
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

	if len(agent.Calls) != 1 {
		t.Fatalf("prompt failure では 1 件目 skill のみで停止すべき: got=%d calls=%v",
			len(agent.Calls), summarizeCalls(agent.Calls))
	}

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonPromptFailure {
		t.Errorf("terminationReason mismatch: got=%q want=%q", rr.TerminationReason, RunReasonPromptFailure)
	}
	if rr.AbortStep == nil || rr.AbortStep.Order != 1 || rr.AbortStep.Phase != "spec-init" {
		t.Errorf("AbortStep mismatch: got=%+v want=Order=1 Phase=spec-init", rr.AbortStep)
	}
	if rr.Failure == nil {
		t.Fatalf("prompt-failure では Failure 非 nil を期待: got=nil")
	}
	if rr.Failure.Reason != string(RunReasonPromptFailure) {
		t.Errorf("Failure.Reason=%q を期待: got=%q", RunReasonPromptFailure, rr.Failure.Reason)
	}
	if !strings.Contains(rr.Failure.Message, agentErrMsg) {
		t.Errorf("Failure.Message は agent error 文言 %q を含むべき: got=%q", agentErrMsg, rr.Failure.Message)
	}

	if !hasErrorEventAll(*eventsPtr, "spec-init", string(RunReasonPromptFailure)) {
		t.Errorf("Error event は 'spec-init' と 'prompt-failure' を同時包含すべき: %+v", *eventsPtr)
	}
}

// TestKiroSpecLoopMissingPersona は task 6.2 シナリオ #5。
//
// 振る舞い前提: <cwd>/.rilll-config/facets/personas/architecture-quality.md を
// 空ファイルとして配置すると session B Persona facet adapter が
// errEmptySessionBPersona を返し、runKiroSpecLoop は agent 呼び出し前に
// missing-persona で abort する。
//
// 期待:
//   - agent.Calls = 0 件
//   - result.json.terminationReason == "missing-persona"
//   - workflow.WorkflowEvent{Error}.Message は "architecture-quality" を含む
func TestKiroSpecLoopMissingPersona(t *testing.T) {
	feature := "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")

	// facet override 空ファイルで errEmptySessionBPersona を再現する。
	personaDir := filepath.Join(cwd, ".rilll-config", "facets", "personas")
	if err := os.MkdirAll(personaDir, 0o755); err != nil {
		t.Fatalf("mkdir personas: %v", err)
	}
	if err := os.WriteFile(filepath.Join(personaDir, "architecture-quality.md"), []byte(""), 0o644); err != nil {
		t.Fatalf("write empty persona override: %v", err)
	}

	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Selected: feature}
	agent := &scriptedAgent{}

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
		t.Errorf("missing-persona では agent.CallAgent は呼ばれてはならない: got=%d calls", len(agent.Calls))
	}

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonMissingPersona {
		t.Errorf("terminationReason mismatch: got=%q want=%q", rr.TerminationReason, RunReasonMissingPersona)
	}
	if rr.Failure != nil {
		t.Errorf("missing-persona は engine 由来 abort ではないので Failure は nil: got=%+v", rr.Failure)
	}

	if !hasErrorEvent(*eventsPtr, "architecture-quality") {
		t.Errorf("Error event は期待ペルソナ名 'architecture-quality' を含むべき: %+v", *eventsPtr)
	}
}

// TestKiroSpecLoopCancelled は task 6.2 シナリオ #6。
//
// 振る舞い前提: feature 選択ダイアログで ESC キャンセル（fakeAsk.Cancelled=true）。
// runKiroSpecLoop は agent 呼び出し前に cancelled-by-user で abort する。
//
// 期待:
//   - agent.Calls = 0 件
//   - result.json.terminationReason == "cancelled-by-user"
//   - workflow.WorkflowEvent{Error}.Message は "キャンセル" を含む
func TestKiroSpecLoopCancelled(t *testing.T) {
	feature := "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Cancelled: true}
	agent := &scriptedAgent{}

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
		t.Errorf("キャンセル時は agent.CallAgent は呼ばれてはならない: got=%d calls", len(agent.Calls))
	}

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonCancelledByUser {
		t.Errorf("terminationReason mismatch: got=%q want=%q", rr.TerminationReason, RunReasonCancelledByUser)
	}
	if rr.Failure != nil {
		t.Errorf("cancelled-by-user は engine 由来 abort ではないので Failure は nil: got=%+v", rr.Failure)
	}

	if !hasErrorEvent(*eventsPtr, "キャンセル") {
		t.Errorf("Error event は『キャンセル』文言を含むべき: %+v", *eventsPtr)
	}
}

// TestKiroSpecLoopMissingFeature は task 6.2 シナリオ #7。
//
// 振る舞い前提: <cwd>/.kiro/specs/ 自体を作らず eligible feature 0 件にする。
// runKiroSpecLoop は agent 呼び出し前に missing-feature で abort する。
//
// 期待:
//   - agent.Calls = 0 件
//   - result.json.terminationReason == "missing-feature"
//   - workflow.WorkflowEvent{Error}.Message は "/kiro-discovery" または "brief.md" の案内を含む
//     （要件 1.3 / design.md「対象なしのエラー」契約）
func TestKiroSpecLoopMissingFeature(t *testing.T) {
	cwd := t.TempDir() // .kiro/specs/ 自体を作らない → eligible 0 件
	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Selected: "shouldnt-be-asked"}
	agent := &scriptedAgent{}

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
		t.Errorf("missing-feature では agent.CallAgent は呼ばれてはならない: got=%d calls", len(agent.Calls))
	}

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonMissingFeature {
		t.Errorf("terminationReason mismatch: got=%q want=%q", rr.TerminationReason, RunReasonMissingFeature)
	}
	if rr.Failure != nil {
		t.Errorf("missing-feature は engine 由来 abort ではないので Failure は nil: got=%+v", rr.Failure)
	}

	// /kiro-discovery または brief.md 配置の案内（どちらか）を含むこと
	if !hasErrorEvent(*eventsPtr, "/kiro-discovery") && !hasErrorEvent(*eventsPtr, "brief.md") {
		t.Errorf("missing-feature event は /kiro-discovery または brief.md 配置の案内を含むべき: %+v", *eventsPtr)
	}
}

// summarizeCalls はテスト失敗時のデバッグ出力で使う。Calls の prompt 先頭 60 字 + Kind を 1 行ずつ並べる。
func summarizeCalls(calls []recordedAgentCall) []string {
	out := make([]string, 0, len(calls))
	for i, c := range calls {
		head := c.Prompt
		if len(head) > 60 {
			head = head[:60] + "..."
		}
		out = append(out, fmt.Sprintf("[%d] kind=%s prompt=%q resume=%q", i, c.Kind, head, c.ResumeSessionID))
	}
	return out
}

// mustWriteFeature は <specsDir>/<feature>/ に brief.md（briefContent が非空のとき）と
// spec.json（specJSON が非空のとき）を書き出すテストヘルパー。
//
// briefContent="" のとき brief.md は作成しない（= 「brief.md なし」を表現）。
// specJSON="" のとき spec.json は作成しない（= 「spec.json なし」を表現）。
func mustWriteFeature(t *testing.T, specsDir, feature, briefContent, specJSON string) {
	t.Helper()
	dir := filepath.Join(specsDir, feature)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if briefContent != "" {
		if err := os.WriteFile(filepath.Join(dir, "brief.md"), []byte(briefContent), 0o644); err != nil {
			t.Fatalf("write brief.md: %v", err)
		}
	}
	if specJSON != "" {
		if err := os.WriteFile(filepath.Join(dir, "spec.json"), []byte(specJSON), 0o644); err != nil {
			t.Fatalf("write spec.json: %v", err)
		}
	}
}

// =============================================================================
// determineStartStep / setApprovalApproved の単体テスト
// =============================================================================

// TestDetermineStartStep_NoSpecFile: spec.json 不在は 1 (spec-init から)。
func TestDetermineStartStep_NoSpecFile(t *testing.T) {
	tmp := t.TempDir()
	got := determineStartStep(filepath.Join(tmp, "spec.json"))
	if got != 1 {
		t.Errorf("spec.json 不在時は 1 を返すべき: got=%d", got)
	}
}

// TestDetermineStartStep_PlainSpec: approvals 無し → 1。
func TestDetermineStartStep_PlainSpec(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "spec.json")
	if err := os.WriteFile(path, []byte(`{"phase":"initialized"}`), 0o644); err != nil {
		t.Fatalf("write spec.json: %v", err)
	}
	got := determineStartStep(path)
	if got != 1 {
		t.Errorf("approvals 無しは 1 を返すべき: got=%d", got)
	}
}

// TestDetermineStartStep_RequirementsApproved: requirements approved → 4 (spec-design)。
func TestDetermineStartStep_RequirementsApproved(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "spec.json")
	body := `{"approvals":{"requirements":{"approved":true},"design":{"approved":false}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write spec.json: %v", err)
	}
	got := determineStartStep(path)
	if got != 4 {
		t.Errorf("requirements approved 時は 4 を返すべき: got=%d", got)
	}
}

// TestDetermineStartStep_DesignApproved: design approved → 6 (spec-tasks)。
// requirements が false でも design が true なら 6 を優先する。
func TestDetermineStartStep_DesignApproved(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "spec.json")
	body := `{"approvals":{"requirements":{"approved":false},"design":{"approved":true}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write spec.json: %v", err)
	}
	got := determineStartStep(path)
	if got != 6 {
		t.Errorf("design approved 時は 6 を返すべき (requirements は false でも): got=%d", got)
	}
}

// TestDetermineStartStep_BrokenJSON: JSON パース失敗 → 1 (安全側)。
func TestDetermineStartStep_BrokenJSON(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "spec.json")
	if err := os.WriteFile(path, []byte(`{not json`), 0o644); err != nil {
		t.Fatalf("write spec.json: %v", err)
	}
	got := determineStartStep(path)
	if got != 1 {
		t.Errorf("壊れた spec.json は安全側 1 を返すべき: got=%d", got)
	}
}

// TestSetApprovalApproved_Requirements: approvals.requirements.approved=false → true 化。
func TestSetApprovalApproved_Requirements(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "spec.json")
	body := `{
  "feature_name": "demo",
  "phase": "requirements-generated",
  "approvals": {
    "requirements": {"generated": true, "approved": false},
    "design": {"generated": false, "approved": false}
  }
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write spec.json: %v", err)
	}
	if err := setApprovalApproved(path, "requirements"); err != nil {
		t.Fatalf("setApprovalApproved: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read spec.json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	approvals, _ := got["approvals"].(map[string]any)
	req, _ := approvals["requirements"].(map[string]any)
	if approved, _ := req["approved"].(bool); !approved {
		t.Errorf("approvals.requirements.approved は true になるべき: got=%v", req)
	}
	// 既存フィールドが保持されていること
	if got["feature_name"] != "demo" {
		t.Errorf("feature_name が保持されていない: got=%v", got["feature_name"])
	}
	if got["phase"] != "requirements-generated" {
		t.Errorf("phase が保持されていない: got=%v", got["phase"])
	}
	// updated_at が更新されていること
	if got["updated_at"] == nil {
		t.Errorf("updated_at がセットされるべき: got=%v", got)
	}
}

// TestSetApprovalApproved_Design: approvals.design.approved=false → true 化。
func TestSetApprovalApproved_Design(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "spec.json")
	body := `{"approvals":{"design":{"approved":false}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write spec.json: %v", err)
	}
	if err := setApprovalApproved(path, "design"); err != nil {
		t.Fatalf("setApprovalApproved: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read spec.json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	approvals, _ := got["approvals"].(map[string]any)
	d, _ := approvals["design"].(map[string]any)
	if approved, _ := d["approved"].(bool); !approved {
		t.Errorf("approvals.design.approved は true になるべき: got=%v", d)
	}
}

// TestSetApprovalApproved_Idempotent: 既に approved=true のとき書き込みをスキップする。
// updated_at が変わらないことで idempotent を確認する。
func TestSetApprovalApproved_Idempotent(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "spec.json")
	body := `{
  "approvals": {"requirements": {"approved": true}},
  "updated_at": "2020-01-01T00:00:00Z"
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write spec.json: %v", err)
	}
	stat0, _ := os.Stat(path)
	if err := setApprovalApproved(path, "requirements"); err != nil {
		t.Fatalf("setApprovalApproved: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["updated_at"] != "2020-01-01T00:00:00Z" {
		t.Errorf("idempotent でない: updated_at が変更された: got=%v", got["updated_at"])
	}
	stat1, _ := os.Stat(path)
	if !stat0.ModTime().Equal(stat1.ModTime()) {
		t.Errorf("idempotent でない: ファイルの ModTime が変わった")
	}
}

// TestSetApprovalApproved_NoApprovalsField: approvals フィールド自体が無い spec.json でも動作する。
func TestSetApprovalApproved_NoApprovalsField(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "spec.json")
	if err := os.WriteFile(path, []byte(`{"feature_name":"x"}`), 0o644); err != nil {
		t.Fatalf("write spec.json: %v", err)
	}
	if err := setApprovalApproved(path, "requirements"); err != nil {
		t.Fatalf("setApprovalApproved: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	approvals, _ := got["approvals"].(map[string]any)
	req, _ := approvals["requirements"].(map[string]any)
	if approved, _ := req["approved"].(bool); !approved {
		t.Errorf("approvals.requirements.approved を新設して true にすべき: got=%v", got)
	}
}

// =============================================================================
// markSpecReady の単体テスト
// =============================================================================
//
// markSpecReady は kiro-spec-loop の最終段 (step 6 / spec-tasks) 完走時に呼ばれ、
// spec.json を「実装着手可能」状態に進める純粋ヘルパ:
//   - phase: "tasks-approved"
//   - ready_for_implementation: true
//   - updated_at: 現在時刻 (変更発生時のみ)
//
// 既に該当状態なら no-op (idempotent) で書き込み自体を行わない。

// TestMarkSpecReady_SetsPhaseAndReadyFlag: phase=tasks-generated / ready_for_implementation=false の
// 一般ケースから phase=tasks-approved / ready_for_implementation=true へ進める。
func TestMarkSpecReady_SetsPhaseAndReadyFlag(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "spec.json")
	body := `{
  "feature_name": "demo",
  "phase": "tasks-generated",
  "ready_for_implementation": false,
  "approvals": {"tasks": {"generated": true, "approved": true}}
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write spec.json: %v", err)
	}
	if err := markSpecReady(path); err != nil {
		t.Fatalf("markSpecReady: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read spec.json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["phase"] != "tasks-approved" {
		t.Errorf("phase は \"tasks-approved\" になるべき: got=%v", got["phase"])
	}
	if ready, _ := got["ready_for_implementation"].(bool); !ready {
		t.Errorf("ready_for_implementation は true になるべき: got=%v", got["ready_for_implementation"])
	}
	if got["feature_name"] != "demo" {
		t.Errorf("feature_name が保持されていない: got=%v", got["feature_name"])
	}
	if got["updated_at"] == nil {
		t.Errorf("updated_at がセットされるべき: got=%v", got)
	}
	// approvals は触らない (既存値の保持)
	approvals, _ := got["approvals"].(map[string]any)
	tasks, _ := approvals["tasks"].(map[string]any)
	if approved, _ := tasks["approved"].(bool); !approved {
		t.Errorf("approvals.tasks.approved を破壊している: got=%v", tasks)
	}
}

// TestMarkSpecReady_Idempotent: 既に phase=tasks-approved && ready_for_implementation=true
// なら書き込みを完全に skip する (updated_at / mtime とも不変)。
func TestMarkSpecReady_Idempotent(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "spec.json")
	body := `{
  "phase": "tasks-approved",
  "ready_for_implementation": true,
  "updated_at": "2020-01-01T00:00:00Z"
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write spec.json: %v", err)
	}
	stat0, _ := os.Stat(path)
	if err := markSpecReady(path); err != nil {
		t.Fatalf("markSpecReady: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["updated_at"] != "2020-01-01T00:00:00Z" {
		t.Errorf("idempotent でない: updated_at が変更された: got=%v", got["updated_at"])
	}
	stat1, _ := os.Stat(path)
	if !stat0.ModTime().Equal(stat1.ModTime()) {
		t.Errorf("idempotent でない: ファイルの ModTime が変わった")
	}
}

// TestMarkSpecReady_PartialState_PhaseOnly: phase だけ "tasks-approved" だが
// ready_for_implementation が false の場合、ready_for_implementation=true まで進める。
func TestMarkSpecReady_PartialState_PhaseOnly(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "spec.json")
	body := `{"phase":"tasks-approved","ready_for_implementation":false}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write spec.json: %v", err)
	}
	if err := markSpecReady(path); err != nil {
		t.Fatalf("markSpecReady: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ready, _ := got["ready_for_implementation"].(bool); !ready {
		t.Errorf("ready_for_implementation は true になるべき: got=%v", got)
	}
	if got["phase"] != "tasks-approved" {
		t.Errorf("phase は \"tasks-approved\" のままであるべき: got=%v", got["phase"])
	}
}

// TestMarkSpecReady_MissingFields: phase / ready_for_implementation フィールド自体が無い
// spec.json でも両フィールドを新設して進める。
func TestMarkSpecReady_MissingFields(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "spec.json")
	if err := os.WriteFile(path, []byte(`{"feature_name":"x"}`), 0o644); err != nil {
		t.Fatalf("write spec.json: %v", err)
	}
	if err := markSpecReady(path); err != nil {
		t.Fatalf("markSpecReady: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["phase"] != "tasks-approved" {
		t.Errorf("phase を新設して \"tasks-approved\" にすべき: got=%v", got["phase"])
	}
	if ready, _ := got["ready_for_implementation"].(bool); !ready {
		t.Errorf("ready_for_implementation を新設して true にすべき: got=%v", got)
	}
}

// =============================================================================
// applyApprovalForPhase 統合テスト (spec-tasks 完走時の最終状態)
// =============================================================================

// TestApplyApprovalForPhase_SpecTasks_MarksSpecReady:
// step 6 (spec-tasks) 完走相当の applyApprovalForPhase("spec-tasks") 呼び出しで、
// approvals.tasks.approved=true に加えて phase=tasks-approved と
// ready_for_implementation=true まで一気に進めることを確認する。
//
// ユーザ要件: 「全タスクがエラーにならずに終わったら rilll 側で
// phase=tasks-approved と ready_for_implementation=true を立てる」。
func TestApplyApprovalForPhase_SpecTasks_MarksSpecReady(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "spec.json")
	body := `{
  "feature_name": "demo",
  "phase": "tasks-generated",
  "ready_for_implementation": false,
  "approvals": {
    "requirements": {"generated": true, "approved": true},
    "design":       {"generated": true, "approved": true},
    "tasks":        {"generated": true, "approved": false}
  }
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write spec.json: %v", err)
	}
	r := &specLoopRunner{specJSONPath: path}
	r.applyApprovalForPhase("spec-tasks")

	raw, _ := os.ReadFile(path)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	approvals, _ := got["approvals"].(map[string]any)
	tasks, _ := approvals["tasks"].(map[string]any)
	if approved, _ := tasks["approved"].(bool); !approved {
		t.Errorf("approvals.tasks.approved=true になるべき: got=%v", tasks)
	}
	if got["phase"] != "tasks-approved" {
		t.Errorf("phase=\"tasks-approved\" になるべき: got=%v", got["phase"])
	}
	if ready, _ := got["ready_for_implementation"].(bool); !ready {
		t.Errorf("ready_for_implementation=true になるべき: got=%v", got)
	}
}

// =============================================================================
// resume シナリオの統合テスト
// =============================================================================

// TestKiroSpecLoopResumeFromDesign: approvals.requirements.approved=true の状態で kiro-spec-loop を流すと
// step 1〜3 がスキップされ、step 4 (spec-design) から開始することを agent.Calls の履歴と result.json で確認する。
//
// 検証ポイント:
//  1. agent.CallAgent が 6 件: skill 3 件 + commit 3 件（spec-design / validate-design / spec-tasks）
//  2. 順序が完全一致: [/kiro-spec-design, /git-commit, /kiro-validate-design, /git-commit, /kiro-spec-tasks, /git-commit]
//  3. result.json.steps が 3 件のみ（order 4, 5, 6）
//  4. validate-design 完走で approvals.design.approved=true になっていること
//  5. validate-design 終了時点でも approvals.requirements.approved は維持される
func TestKiroSpecLoopResumeFromDesign(t *testing.T) {
	feature := "resume-design"
	specJSON := `{
  "feature_name": "resume-design",
  "phase": "requirements-generated",
  "approvals": {
    "requirements": {"generated": true, "approved": true},
    "design": {"generated": false, "approved": false},
    "tasks": {"generated": false, "approved": false}
  }
}`
	cwd := setupFeatureCWD(t, feature, "Brief 本文", specJSON)
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}
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

	// (1) agent.Call 履歴は 6 件 (skill 3 + commit 3)
	if len(agent.Calls) != 6 {
		t.Fatalf("resume-from-design では skill 3 件 + commit 3 件 = 6 件を期待: got=%d\n%v",
			len(agent.Calls), summarizeCalls(agent.Calls))
	}

	// (2) 順序完全一致
	expected := []struct {
		prefix string
		kind   string
	}{
		{prefix: "/kiro-spec-design " + feature, kind: "skill"},
		{prefix: "/git-commit", kind: "commit"},
		{prefix: "/kiro-validate-design " + feature, kind: "skill"},
		{prefix: "/git-commit", kind: "commit"},
		{prefix: "/kiro-spec-tasks " + feature, kind: "skill"},
		{prefix: "/git-commit", kind: "commit"},
	}
	// kiro-spec-loop-facets task 5.1 で session A skill prompt 先頭に persona prefix が
	// 入るようになったため、skill 系は Contains で判定する (commit は HasPrefix のまま)。
	for i, want := range expected {
		got := agent.Calls[i]
		if want.kind == "commit" {
			if !strings.HasPrefix(got.Prompt, want.prefix) {
				t.Errorf("call[%d] (commit) prompt prefix mismatch:\n got=%q\nwant prefix=%q", i, got.Prompt, want.prefix)
			}
		} else {
			if !strings.Contains(got.Prompt, want.prefix) {
				t.Errorf("call[%d] (skill) prompt は slash コマンド %q を含むべき:\n got=%q", i, want.prefix, got.Prompt)
			}
		}
		if got.Kind != want.kind {
			t.Errorf("call[%d] kind mismatch: got=%q want=%q", i, got.Kind, want.kind)
		}
	}

	// 参照ブロックヘッダ（spec-init で使う brief.md 参照）が一切現れないこと
	const refBlockMarker = "以下のマークダウンファイルを参照してください"
	for i, c := range agent.Calls {
		if strings.Contains(c.Prompt, refBlockMarker) {
			t.Errorf("resume-from-design では brief.md 参照ブロックは現れない: call[%d]=%q", i, c.Prompt)
		}
	}

	// (3) result.json.steps は 3 件 (order 4, 5, 6)
	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonCompleted {
		t.Errorf("terminationReason mismatch: got=%q want=%q", rr.TerminationReason, RunReasonCompleted)
	}
	if len(rr.Steps) != 3 {
		t.Fatalf("steps は 3 件を期待: got=%d", len(rr.Steps))
	}
	wantOrders := []int{4, 5, 6}
	wantPhases := []string{"spec-design", "validate-design", "spec-tasks"}
	for i, s := range rr.Steps {
		if s.Order != wantOrders[i] {
			t.Errorf("steps[%d].Order mismatch: got=%d want=%d", i, s.Order, wantOrders[i])
		}
		if s.Phase != wantPhases[i] {
			t.Errorf("steps[%d].Phase mismatch: got=%q want=%q", i, s.Phase, wantPhases[i])
		}
	}

	// (4) validate-design 完走で approvals.design.approved=true になっていること
	specJSONPath := filepath.Join(cwd, ".kiro", "specs", feature, "spec.json")
	rawAfter, err := os.ReadFile(specJSONPath)
	if err != nil {
		t.Fatalf("read spec.json after run: %v", err)
	}
	var afterSpec map[string]any
	if err := json.Unmarshal(rawAfter, &afterSpec); err != nil {
		t.Fatalf("parse spec.json after run: %v", err)
	}
	approvals, _ := afterSpec["approvals"].(map[string]any)
	d, _ := approvals["design"].(map[string]any)
	if approved, _ := d["approved"].(bool); !approved {
		t.Errorf("validate-design 後は approvals.design.approved=true を期待: got=%v", d)
	}
	// (5) requirements の承認も維持されていること
	req, _ := approvals["requirements"].(map[string]any)
	if approved, _ := req["approved"].(bool); !approved {
		t.Errorf("approvals.requirements.approved は維持されるべき: got=%v", req)
	}
}

// TestKiroSpecLoopResumeFromTasks: approvals.design.approved=true の状態で kiro-spec-loop を流すと
// step 6 (spec-tasks) のみ実行されることを確認する。
func TestKiroSpecLoopResumeFromTasks(t *testing.T) {
	feature := "resume-tasks"
	specJSON := `{
  "feature_name": "resume-tasks",
  "phase": "design-generated",
  "approvals": {
    "requirements": {"generated": true, "approved": true},
    "design": {"generated": true, "approved": true},
    "tasks": {"generated": false, "approved": false}
  }
}`
	cwd := setupFeatureCWD(t, feature, "Brief 本文", specJSON)
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}
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

	// agent.Call 履歴は 2 件のみ（spec-tasks + commit）
	if len(agent.Calls) != 2 {
		t.Fatalf("resume-from-tasks では skill 1 件 + commit 1 件 = 2 件を期待: got=%d\n%v",
			len(agent.Calls), summarizeCalls(agent.Calls))
	}
	// kiro-spec-loop-facets task 5.1 で session A skill prompt 先頭に persona prefix が入るため
	// HasPrefix から Contains に変更する。
	if !strings.Contains(agent.Calls[0].Prompt, "/kiro-spec-tasks "+feature) {
		t.Errorf("最初の呼び出しは /kiro-spec-tasks を含むべき: got=%q", agent.Calls[0].Prompt)
	}
	if agent.Calls[1].Kind != "commit" {
		t.Errorf("2 件目は /git-commit であるべき: got kind=%q", agent.Calls[1].Kind)
	}

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonCompleted {
		t.Errorf("terminationReason mismatch: got=%q want=%q", rr.TerminationReason, RunReasonCompleted)
	}
	if len(rr.Steps) != 1 {
		t.Fatalf("steps は 1 件を期待: got=%d", len(rr.Steps))
	}
	if rr.Steps[0].Order != 6 || rr.Steps[0].Phase != "spec-tasks" {
		t.Errorf("唯一の step は order=6 / phase=spec-tasks であるべき: got=%+v", rr.Steps[0])
	}
}

// TestKiroSpecLoopApprovesRequirementsAndDesign: success path（spec.json 不在から完走）で
// validate-gap / validate-design 完了時に approvals.{requirements,design}.approved=true が
// 立っていることを spec.json の最終状態で検証する。
//
// 注意: TestKiroSpecLoopSuccess の recordedAgent は SkillResponse を fake で返すため、
// 既存 kiro-spec-* skill が spec.json を書き換える効果は再現されない（=spec.json は本テストでは
// 工程中作成されない）。validate-gap 完了時に setApprovalApproved が呼ばれても spec.json が無ければ
// no-op となるため、本テストではあらかじめ「requirements 生成済み / 未承認」の spec.json を配置して
// validate-gap 完走で requirements.approved=true、validate-design 完走で design.approved=true に
// 遷移することを確認する。
func TestKiroSpecLoopApprovesRequirementsAndDesign(t *testing.T) {
	feature := "approve-flow"
	specJSON := `{
  "feature_name": "approve-flow",
  "phase": "initialized",
  "approvals": {
    "requirements": {"generated": true, "approved": false},
    "design": {"generated": false, "approved": false},
    "tasks": {"generated": false, "approved": false}
  }
}`
	cwd := setupFeatureCWD(t, feature, "Brief 本文", specJSON)
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}
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

	specJSONPath := filepath.Join(cwd, ".kiro", "specs", feature, "spec.json")
	raw, err := os.ReadFile(specJSONPath)
	if err != nil {
		t.Fatalf("read spec.json after run: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse spec.json: %v", err)
	}
	approvals, _ := got["approvals"].(map[string]any)
	req, _ := approvals["requirements"].(map[string]any)
	if approved, _ := req["approved"].(bool); !approved {
		t.Errorf("validate-gap 完走で approvals.requirements.approved=true を期待: got=%v", req)
	}
	d, _ := approvals["design"].(map[string]any)
	if approved, _ := d["approved"].(bool); !approved {
		t.Errorf("validate-design 完走で approvals.design.approved=true を期待: got=%v", d)
	}
}

// TestKiroSpecLoopValidateDesignAbortsAfterMaxNoGoAttempts は validate-design が同一 critical blocker を
// 3 回連続で返すケースで、 maxSameCriticalBlockerAttempts (= 3) 回試行した後 abort する契約を固定する。
//
// 観察ポイント:
//   - step 4 (spec-design) → step 5 (validate-design) のペアが 3 回実行される
//   - 3 回目の no_go で初めて RunReasonValidationNoGo へ落ちる
//   - rr.Steps は最終試行 (3rd attempt) の 2 件のみ保持し、過去 attempt は Advisories[] / validationFeedback[] に履歴化される
//   - 各 attempt の remediation feedback が validationFeedback[] に source=validate-design として 3 件積まれる
func TestKiroSpecLoopValidateDesignAbortsAfterMaxNoGoAttempts(t *testing.T) {
	feature := "validate-design-no-go"
	const fingerprint = "same-critical-blocker"
	specJSON := `{
  "feature_name": "validate-design-no-go",
  "phase": "requirements-generated",
  "approvals": {
    "requirements": {"generated": true, "approved": true},
    "design": {"generated": true, "approved": false},
    "tasks": {"generated": false, "approved": false}
  }
}`
	cwd := setupFeatureCWD(t, feature, "Brief 本文", specJSON)
	onEvent, eventsPtr := collectEvents()
	ask := &fakeAsk{Selected: feature}
	validateCount := 0
	specDesignCount := 0
	agent := &scriptedAgent{
		Respond: func(call recordedAgentCall) (provider.AgentResponse, error) {
			switch {
			case call.Kind == "commit":
				return commitOKResp(), nil
			case strings.Contains(call.Prompt, "/kiro-validate-design "+feature):
				validateCount++
				resp := validateDesignRemediationResp(
					"no_go",
					false,
					fmt.Sprintf("NO-GO attempt %d", validateCount),
					"",
					feature,
					fingerprint,
				)
				resp.SessionID = fmt.Sprintf("vd-session-%d", validateCount)
				return resp, nil
			case strings.Contains(call.Prompt, "/kiro-spec-design "+feature):
				specDesignCount++
				return skillNoInquiryResp(), nil
			default:
				return skillNoInquiryResp(), nil
			}
		},
	}

	if err := runKiroSpecLoop(context.Background(), cwd, onEvent, ask, agent); err != nil {
		t.Fatalf("runKiroSpecLoop returned error: %v", err)
	}

	if specDesignCount != 3 {
		t.Fatalf("spec-design skill は 3 回呼ばれるべき (1 初回 + 2 retry): got=%d", specDesignCount)
	}
	if validateCount != 3 {
		t.Fatalf("validate-design skill は 3 回呼ばれるべき: got=%d", validateCount)
	}
	for i, call := range agent.Calls {
		if strings.Contains(call.Prompt, "/kiro-spec-tasks "+feature) {
			t.Fatalf("call[%d] unexpectedly invoked spec-tasks after 3 no-go retries: %q", i, call.Prompt)
		}
	}

	specJSONPath := filepath.Join(cwd, ".kiro", "specs", feature, "spec.json")
	if got := mustReadApproved(t, specJSONPath, "design"); got {
		t.Fatalf("validate-design NO-GO abort must not set approvals.design.approved=true")
	}

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonValidationNoGo {
		t.Fatalf("terminationReason=%q, want %q", rr.TerminationReason, RunReasonValidationNoGo)
	}
	if rr.AbortStep == nil || rr.AbortStep.Order != 5 || rr.AbortStep.Phase != "validate-design" {
		t.Fatalf("abortStep should point to validate-design: %+v", rr.AbortStep)
	}
	if len(rr.Steps) != 2 {
		t.Fatalf("result should include only last attempt (spec-design + validate-design): got %d steps", len(rr.Steps))
	}
	if rr.Steps[1].Verdict != "no_go" {
		t.Fatalf("validate-design step verdict=%q, want no_go", rr.Steps[1].Verdict)
	}
	if rr.Steps[1].Attempt != 3 {
		t.Fatalf("last validate-design step.Attempt=%d, want 3", rr.Steps[1].Attempt)
	}
	if len(rr.Advisories) != 3 {
		t.Fatalf("Advisories[] は 3 件 (各 no_go attempt): got=%d", len(rr.Advisories))
	}
	for i, a := range rr.Advisories {
		if a.Verdict != "no_go" {
			t.Errorf("Advisories[%d].Verdict=%q, want no_go", i, a.Verdict)
		}
		if a.Attempt != i+1 {
			t.Errorf("Advisories[%d].Attempt=%d, want %d", i, a.Attempt, i+1)
		}
		if !strings.Contains(a.Advisory, "# Validation Remediation") {
			t.Errorf("Advisories[%d].Advisory should contain remediation marker: %q", i, a.Advisory)
		}
	}
	if len(rr.ValidationFeedback) != 3 {
		t.Fatalf("validationFeedback len = %d, want 3: %#v", len(rr.ValidationFeedback), rr.ValidationFeedback)
	}
	for i, fb := range rr.ValidationFeedback {
		if fb.Source != string(ValidationFeedbackSourceValidateDesign) {
			t.Fatalf("validationFeedback[%d].source = %q, want validate-design", i, fb.Source)
		}
		if fb.SameBlockerCount != i+1 {
			t.Fatalf("validationFeedback[%d].sameBlockerCount = %d, want %d", i, fb.SameBlockerCount, i+1)
		}
	}
	if !rr.ValidationFeedback[2].Terminal {
		t.Fatalf("last validationFeedback must be terminal: %#v", rr.ValidationFeedback[2])
	}
	if rr.ValidationFeedback[2].RetryAction != string(ValidationRetryActionStopForHuman) {
		t.Fatalf("last validationFeedback retryAction = %q, want stop_for_human", rr.ValidationFeedback[2].RetryAction)
	}
	if !hasErrorEvent(*eventsPtr, string(RunReasonValidationNoGo)) {
		t.Fatalf("WorkflowError should mention validation-no-go: %+v", *eventsPtr)
	}
}

// TestKiroSpecLoopValidateDesignRetriesOnNoGoThenSucceeds は 1 回目 no_go → 2 回目 go で
// retry ループが収束し、step 6 (spec-tasks) まで進んで completed することを確認する。
//
// 同時に検証: 2 回目の spec-design 呼び出しに validator NO-GO summary が Previous Response として
// 流入していること (architect が指摘内容を反映できる経路の観察)。
func TestKiroSpecLoopValidateDesignRetriesOnNoGoThenSucceeds(t *testing.T) {
	feature := "no-go-then-go"
	const fingerprint = "facet-boundary-mismatch"
	specJSON := `{
  "feature_name": "no-go-then-go",
  "phase": "requirements-generated",
  "approvals": {
    "requirements": {"generated": true, "approved": true},
    "design": {"generated": true, "approved": false},
    "tasks": {"generated": false, "approved": false}
  }
}`
	cwd := setupFeatureCWD(t, feature, "Brief 本文", specJSON)
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}
	validateCount := 0
	const noGoSummary = "NO-GO: ファセット境界が requirements R1.2 と矛盾"
	agent := &scriptedAgent{
		Respond: func(call recordedAgentCall) (provider.AgentResponse, error) {
			switch {
			case call.Kind == "commit":
				return commitOKResp(), nil
			case strings.Contains(call.Prompt, "/kiro-validate-design "+feature):
				validateCount++
				if validateCount == 1 {
					resp := validateDesignRemediationResp("no_go", false, noGoSummary, "", feature, fingerprint)
					resp.SessionID = "vd-1"
					return resp, nil
				}
				return provider.AgentResponse{
					Success:   true,
					SessionID: "vd-2",
					StructuredOutput: map[string]any{
						"has_inquiry":  false,
						"summary":      "GO",
						"verdict":      "go",
						"has_advisory": false,
					},
				}, nil
			default:
				return skillNoInquiryResp(), nil
			}
		},
	}

	if err := runKiroSpecLoop(context.Background(), cwd, onEvent, ask, agent); err != nil {
		t.Fatalf("runKiroSpecLoop returned error: %v", err)
	}

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonCompleted {
		t.Fatalf("terminationReason=%q, want completed", rr.TerminationReason)
	}
	if validateCount != 2 {
		t.Fatalf("validate-design は 2 回呼ばれるべき (1 no_go + 1 go): got=%d", validateCount)
	}
	// rr.Steps は phase=requirements-generated 開始のため step 4/5/6 の 3 件
	if len(rr.Steps) != 3 {
		t.Fatalf("rr.Steps は 3 件 (spec-design + validate-design + spec-tasks): got=%d", len(rr.Steps))
	}
	// 最終 attempt はいずれも 2
	if rr.Steps[0].Attempt != 2 {
		t.Errorf("spec-design 最終 Attempt=%d, want 2", rr.Steps[0].Attempt)
	}
	if rr.Steps[1].Attempt != 2 {
		t.Errorf("validate-design 最終 Attempt=%d, want 2", rr.Steps[1].Attempt)
	}
	if rr.Steps[1].Verdict != "go" {
		t.Errorf("validate-design 最終 verdict=%q, want go", rr.Steps[1].Verdict)
	}
	if len(rr.Advisories) != 1 {
		t.Fatalf("Advisories は no_go attempt 1 のみで 1 件: got=%d", len(rr.Advisories))
	}
	if rr.Advisories[0].Verdict != "no_go" || rr.Advisories[0].Attempt != 1 {
		t.Errorf("Advisories[0]={verdict=%q, attempt=%d}, want {no_go, 1}", rr.Advisories[0].Verdict, rr.Advisories[0].Attempt)
	}

	// validator NO-GO summary が 2 回目 spec-design prompt に Previous Response として流入していること
	// (架構上、agent.Calls の中で /kiro-spec-design feature を含む 2 回目 skill 呼び出しを探す)
	var specDesignPrompts []string
	for _, c := range agent.Calls {
		if c.Kind == "skill" && strings.Contains(c.Prompt, "/kiro-spec-design "+feature) {
			specDesignPrompts = append(specDesignPrompts, c.Prompt)
		}
	}
	if len(specDesignPrompts) != 2 {
		t.Fatalf("spec-design skill は 2 回呼ばれるべき: got=%d", len(specDesignPrompts))
	}
	if !strings.Contains(specDesignPrompts[1], "# Validation Remediation") {
		t.Errorf("2 回目 spec-design prompt に # Validation Remediation が含まれていない")
	}
	if !strings.Contains(specDesignPrompts[1], fingerprint) {
		t.Errorf("2 回目 spec-design prompt に fingerprint %q が含まれていない", fingerprint)
	}
	if !strings.Contains(specDesignPrompts[1], noGoSummary) {
		t.Errorf("2 回目 spec-design prompt に NO-GO summary が含まれていない (Previous Response 注入経路の確認):\n%s",
			specDesignPrompts[1])
	}
}

// TestKiroSpecLoopValidateDesignPreflightRetriesMissingDesign は
// 04-spec-design が design artifact を生成しないまま完了した場合、05-validate-design を
// 呼び出す前に deterministic preflight で 04 に差し戻すことを固定する。
//
// 観察ポイント:
//   - 1 回目の 04 後は design.md / approvals.design.generated が不足しているため 05 を呼ばない
//   - 2 回目の 04 prompt には preflight feedback が Previous Response として流入する
//   - 2 回目の 04 が design.md と spec.json を整えた後、05 は 1 回だけ実行される
//   - preflight retry は validate-design no_go budget を消費しない
func TestKiroSpecLoopValidateDesignPreflightRetriesMissingDesign(t *testing.T) {
	feature := "preflight-missing-design"
	specJSON := `{
  "feature_name": "preflight-missing-design",
  "phase": "requirements-generated",
  "approvals": {
    "requirements": {"generated": true, "approved": true},
    "design": {"generated": false, "approved": false},
    "tasks": {"generated": false, "approved": false}
  }
}`
	cwd := setupFeatureCWD(t, feature, "Brief 本文", specJSON)
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}
	specDesignCount := 0
	validateCount := 0
	agent := &scriptedAgent{
		DisableAutoSpecDesignArtifacts: true,
		Respond: func(call recordedAgentCall) (provider.AgentResponse, error) {
			switch {
			case call.Kind == "commit":
				return commitOKResp(), nil
			case strings.Contains(call.Prompt, "/kiro-spec-design "+feature):
				specDesignCount++
				if specDesignCount == 2 {
					specDir := filepath.Join(cwd, ".kiro", "specs", feature)
					if err := os.WriteFile(filepath.Join(specDir, "design.md"), []byte("# Design Document\n"), 0o644); err != nil {
						t.Fatalf("write design.md: %v", err)
					}
					updated := `{
  "feature_name": "preflight-missing-design",
  "phase": "design-generated",
  "approvals": {
    "requirements": {"generated": true, "approved": true},
    "design": {"generated": true, "approved": false},
    "tasks": {"generated": false, "approved": false}
  }
}`
					if err := os.WriteFile(filepath.Join(specDir, "spec.json"), []byte(updated), 0o644); err != nil {
						t.Fatalf("write spec.json: %v", err)
					}
				}
				return skillNoInquiryResp(), nil
			case strings.Contains(call.Prompt, "/kiro-validate-design "+feature):
				validateCount++
				return provider.AgentResponse{
					Success:   true,
					SessionID: "vd-clean",
					StructuredOutput: map[string]any{
						"has_inquiry":  false,
						"summary":      "GO",
						"verdict":      "go",
						"has_advisory": false,
					},
				}, nil
			default:
				return skillNoInquiryResp(), nil
			}
		},
	}

	if err := runKiroSpecLoop(context.Background(), cwd, onEvent, ask, agent); err != nil {
		t.Fatalf("runKiroSpecLoop returned error: %v", err)
	}
	if specDesignCount != 2 {
		t.Fatalf("spec-design should retry once after preflight failure: got=%d", specDesignCount)
	}
	if validateCount != 1 {
		t.Fatalf("validate-design should run only after preflight passes: got=%d", validateCount)
	}

	var specDesignPrompts []string
	for _, c := range agent.Calls {
		if c.Kind == "skill" && strings.Contains(c.Prompt, "/kiro-spec-design "+feature) {
			specDesignPrompts = append(specDesignPrompts, c.Prompt)
		}
	}
	if len(specDesignPrompts) != 2 {
		t.Fatalf("spec-design prompts=%d, want 2", len(specDesignPrompts))
	}
	if !strings.Contains(specDesignPrompts[1], "validate-design preflight failed") ||
		!strings.Contains(specDesignPrompts[1], "approvals.design.generated") ||
		!strings.Contains(specDesignPrompts[1], "design.md") {
		t.Fatalf("2 回目 spec-design prompt に preflight feedback が含まれていない:\n%s", specDesignPrompts[1])
	}

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonCompleted {
		t.Fatalf("terminationReason=%q, want completed", rr.TerminationReason)
	}
	if len(rr.Advisories) != 0 {
		t.Fatalf("preflight retry must not consume validate-design advisory/no_go history: got=%d", len(rr.Advisories))
	}
	assertPreflightValidationFeedbackOnly(t, rr.ValidationFeedback, 1, false)
}

// TestKiroSpecLoopValidateDesignPreflightAbortsAfterMaxMissingDesign は
// 04-spec-design が artifact を作れない状態が続く場合、05 を呼ばずに deterministic
// preflight の上限で hard abort することを固定する。
func TestKiroSpecLoopValidateDesignPreflightAbortsAfterMaxMissingDesign(t *testing.T) {
	feature := "preflight-abort-missing-design"
	specJSON := `{
  "feature_name": "preflight-abort-missing-design",
  "phase": "requirements-generated",
  "approvals": {
    "requirements": {"generated": true, "approved": true},
    "design": {"generated": false, "approved": false},
    "tasks": {"generated": false, "approved": false}
  }
}`
	cwd := setupFeatureCWD(t, feature, "Brief 本文", specJSON)
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}
	specDesignCount := 0
	validateCount := 0
	agent := &scriptedAgent{
		DisableAutoSpecDesignArtifacts: true,
		Respond: func(call recordedAgentCall) (provider.AgentResponse, error) {
			switch {
			case call.Kind == "commit":
				return commitOKResp(), nil
			case strings.Contains(call.Prompt, "/kiro-spec-design "+feature):
				specDesignCount++
				return skillNoInquiryResp(), nil
			case strings.Contains(call.Prompt, "/kiro-validate-design "+feature):
				validateCount++
				return skillNoInquiryResp(), nil
			default:
				return skillNoInquiryResp(), nil
			}
		},
	}

	if err := runKiroSpecLoop(context.Background(), cwd, onEvent, ask, agent); err != nil {
		t.Fatalf("runKiroSpecLoop returned error: %v", err)
	}
	if specDesignCount != maxValidateDesignPreflightAttempts {
		t.Fatalf("spec-design should retry until preflight max: got=%d want=%d", specDesignCount, maxValidateDesignPreflightAttempts)
	}
	if validateCount != 0 {
		t.Fatalf("validate-design must not run while preflight is failing: got=%d", validateCount)
	}

	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonValidationNoGo {
		t.Fatalf("terminationReason=%q, want %q", rr.TerminationReason, RunReasonValidationNoGo)
	}
	if rr.AbortStep == nil || rr.AbortStep.Order != 5 || rr.AbortStep.Phase != "validate-design" {
		t.Fatalf("abortStep should point to validate-design: %+v", rr.AbortStep)
	}
	if len(rr.Steps) == 0 {
		t.Fatalf("result should include preflight abort step")
	}
	last := rr.Steps[len(rr.Steps)-1]
	if last.Phase != "validate-design" || last.Verdict != "no_go" {
		t.Fatalf("last step should be validate-design preflight no_go: %+v", last)
	}
	if last.Attempt != maxValidateDesignPreflightAttempts {
		t.Fatalf("preflight abort attempt=%d, want %d", last.Attempt, maxValidateDesignPreflightAttempts)
	}
	if len(rr.Advisories) != 0 {
		t.Fatalf("preflight abort must not record validate-design advisories: got=%d", len(rr.Advisories))
	}
	assertPreflightValidationFeedbackOnly(t, rr.ValidationFeedback, maxValidateDesignPreflightAttempts, true)
}

func assertPreflightValidationFeedbackOnly(t *testing.T, feedback []RunResultValidationFeedback, wantLen int, wantTerminalLast bool) {
	t.Helper()
	if len(feedback) != wantLen {
		t.Fatalf("validationFeedback len = %d, want %d: %#v", len(feedback), wantLen, feedback)
	}
	for i, got := range feedback {
		if got.Source != string(ValidationFeedbackSourcePreflight) {
			t.Fatalf("validationFeedback[%d].source = %q, want preflight", i, got.Source)
		}
		if got.Source == string(ValidationFeedbackSourceValidateDesign) {
			t.Fatalf("validationFeedback[%d] must not record validate-design blocker history", i)
		}
		if got.Blocker != nil {
			t.Fatalf("validationFeedback[%d].blocker must be nil for preflight: %#v", i, got.Blocker)
		}
		if got.VerdictClass != "" {
			t.Fatalf("validationFeedback[%d].verdictClass = %q, want empty", i, got.VerdictClass)
		}
		if !strings.Contains(got.Message, "validate-design preflight failed") {
			t.Fatalf("validationFeedback[%d].message must preserve preflight marker: %q", i, got.Message)
		}
	}
	if wantTerminalLast {
		last := feedback[len(feedback)-1]
		if !last.Terminal {
			t.Fatalf("last preflight feedback must be terminal=true: %#v", last)
		}
		if last.RetryAction != string(ValidationRetryActionStopForHuman) {
			t.Fatalf("last preflight retryAction = %q, want stop_for_human", last.RetryAction)
		}
		for _, got := range feedback[:len(feedback)-1] {
			if got.Terminal {
				t.Fatalf("non-terminal preflight feedback must not be terminal: %#v", got)
			}
			if got.RetryAction != string(ValidationRetryActionRetrySpecDesign) {
				t.Fatalf("retry preflight retryAction = %q, want retry_spec_design", got.RetryAction)
			}
		}
	}
}

// TestKiroSpecLoopValidateDesignRetriesOnAdvisoryGoThenCleanGoSucceeds は verdict=go + has_advisory=true
// (= 軽い条件付き go) で retry が走り、2 回目 clean go で完走することを確認する。
func TestKiroSpecLoopValidateDesignRetriesOnAdvisoryGoThenCleanGoSucceeds(t *testing.T) {
	feature := "advisory-then-clean"
	const fingerprint = "fts5-fallback-test-gap"
	specJSON := `{
  "feature_name": "advisory-then-clean",
  "phase": "requirements-generated",
  "approvals": {
    "requirements": {"generated": true, "approved": true},
    "design": {"generated": true, "approved": false},
    "tasks": {"generated": false, "approved": false}
  }
}`
	cwd := setupFeatureCWD(t, feature, "Brief 本文", specJSON)
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}
	validateCount := 0
	const advisoryText = "FTS5 fallback のテスト観点を tasks 側で明文化してください"
	agent := &scriptedAgent{
		Respond: func(call recordedAgentCall) (provider.AgentResponse, error) {
			switch {
			case call.Kind == "commit":
				return commitOKResp(), nil
			case strings.Contains(call.Prompt, "/kiro-validate-design "+feature):
				validateCount++
				if validateCount == 1 {
					resp := validateDesignRemediationResp("go", true, "概ね整理されています", advisoryText, feature, fingerprint)
					resp.SessionID = "vd-1"
					return resp, nil
				}
				return provider.AgentResponse{
					Success:   true,
					SessionID: "vd-2",
					StructuredOutput: map[string]any{
						"has_inquiry":  false,
						"summary":      "GO",
						"verdict":      "go",
						"has_advisory": false,
					},
				}, nil
			default:
				return skillNoInquiryResp(), nil
			}
		},
	}

	if err := runKiroSpecLoop(context.Background(), cwd, onEvent, ask, agent); err != nil {
		t.Fatalf("runKiroSpecLoop returned error: %v", err)
	}
	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonCompleted {
		t.Fatalf("advisory retry が clean go で収束した場合 terminationReason=completed: got=%q", rr.TerminationReason)
	}
	if validateCount != 2 {
		t.Fatalf("validate-design は 2 回呼ばれるべき: got=%d", validateCount)
	}
	if len(rr.Advisories) != 1 {
		t.Fatalf("Advisories は advisory attempt 1 のみで 1 件: got=%d", len(rr.Advisories))
	}
	if rr.Advisories[0].Verdict != "go" {
		t.Errorf("Advisories[0].Verdict=%q, want go", rr.Advisories[0].Verdict)
	}
	if !strings.Contains(rr.Advisories[0].Advisory, advisoryText) {
		t.Errorf("Advisories[0].Advisory should contain advisory text: %q", rr.Advisories[0].Advisory)
	}
	// architect 2 回目 prompt に advisory が流入していること
	var sdPrompts []string
	for _, c := range agent.Calls {
		if c.Kind == "skill" && strings.Contains(c.Prompt, "/kiro-spec-design "+feature) {
			sdPrompts = append(sdPrompts, c.Prompt)
		}
	}
	if len(sdPrompts) < 2 || !strings.Contains(sdPrompts[1], "# Validation Remediation") {
		t.Errorf("2 回目 spec-design prompt に remediation marker が含まれていない: prompts=%d", len(sdPrompts))
	}
	if len(sdPrompts) < 2 || !strings.Contains(sdPrompts[1], advisoryText) {
		t.Errorf("2 回目 spec-design prompt に advisory が含まれていない: prompts=%d", len(sdPrompts))
	}
}

// TestKiroSpecLoopValidateDesignAdvisoryAttemptsDoNotConsumeNoGoBudget は
// go+advisory retry が先に複数回発生しても、その後に初めて返った no_go は
// no_go retry budget の 1 回目として扱われ、spec-design に差し戻されることを固定する。
func TestKiroSpecLoopValidateDesignAdvisoryAttemptsDoNotConsumeNoGoBudget(t *testing.T) {
	feature := "advisory-then-no-go"
	const advisoryFingerprint = "advisory-gap"
	const noGoFingerprint = "token-input-contract-gap"
	specJSON := `{
  "feature_name": "advisory-then-no-go",
  "phase": "requirements-generated",
  "approvals": {
    "requirements": {"generated": true, "approved": true},
    "design": {"generated": true, "approved": false},
    "tasks": {"generated": false, "approved": false}
  }
}`
	cwd := setupFeatureCWD(t, feature, "Brief 本文", specJSON)
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}
	validateCount := 0
	const noGoSummary = "NO-GO: 削除前 Token の入力契約が未確定"
	const noGoAdvisory = "Boundary Adapter に削除前 Token snapshot を渡す契約を明記してください"
	agent := &scriptedAgent{
		Respond: func(call recordedAgentCall) (provider.AgentResponse, error) {
			switch {
			case call.Kind == "commit":
				return commitOKResp(), nil
			case strings.Contains(call.Prompt, "/kiro-validate-design "+feature):
				validateCount++
				switch validateCount {
				case 1, 2:
					resp := validateDesignRemediationResp(
						"go",
						true,
						"概ね整理されています",
						fmt.Sprintf("advisory %d", validateCount),
						feature,
						fmt.Sprintf("%s-%d", advisoryFingerprint, validateCount),
					)
					resp.SessionID = fmt.Sprintf("vd-%d", validateCount)
					return resp, nil
				case 3:
					resp := validateDesignRemediationResp(
						"no_go",
						true,
						noGoSummary,
						noGoAdvisory,
						feature,
						noGoFingerprint,
					)
					resp.SessionID = "vd-3"
					return resp, nil
				default:
					return provider.AgentResponse{
						Success:   true,
						SessionID: "vd-4",
						StructuredOutput: map[string]any{
							"has_inquiry":  false,
							"summary":      "GO",
							"verdict":      "go",
							"has_advisory": false,
						},
					}, nil
				}
			default:
				return skillNoInquiryResp(), nil
			}
		},
	}

	if err := runKiroSpecLoop(context.Background(), cwd, onEvent, ask, agent); err != nil {
		t.Fatalf("runKiroSpecLoop returned error: %v", err)
	}
	if validateCount != 4 {
		t.Fatalf("validate-design は 4 回呼ばれるべき (2 advisory + 1 no_go + 1 clean go): got=%d", validateCount)
	}
	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonCompleted {
		t.Fatalf("terminationReason=%q, want completed", rr.TerminationReason)
	}
	if len(rr.Advisories) != 3 {
		t.Fatalf("Advisories は 3 件 (2 advisory + 1 no_go): got=%d", len(rr.Advisories))
	}
	if rr.Advisories[2].Verdict != "no_go" {
		t.Fatalf("3 件目の advisory verdict=%q, want no_go", rr.Advisories[2].Verdict)
	}
	var specDesignPrompts []string
	for _, c := range agent.Calls {
		if c.Kind == "skill" && strings.Contains(c.Prompt, "/kiro-spec-design "+feature) {
			specDesignPrompts = append(specDesignPrompts, c.Prompt)
		}
	}
	if len(specDesignPrompts) != 4 {
		t.Fatalf("spec-design skill は 4 回呼ばれるべき: got=%d", len(specDesignPrompts))
	}
	if !strings.Contains(specDesignPrompts[3], "# Validation Remediation") ||
		!strings.Contains(specDesignPrompts[3], noGoFingerprint) ||
		!strings.Contains(specDesignPrompts[3], noGoAdvisory) {
		t.Errorf("no_go 後の spec-design prompt に remediation feedback が含まれていない:\n%s", specDesignPrompts[3])
	}
}

// TestKiroSpecLoopValidateDesignDegradesOnMaxAdvisoryAttempts は 3 回連続で verdict=go + has_advisory=true
// が返ったとき、最後の attempt で degradation completed として step 6 に進むことを確認する。
//
// 設計判断: advisory は no_go と異なり「軽微な指摘」のため、上限到達時は workflow を止めず
// 通過させ、advisory 履歴を rr.Advisories に残して後段 (人間 / 後続 spec) で対処させる。
func TestKiroSpecLoopValidateDesignDegradesOnMaxAdvisoryAttempts(t *testing.T) {
	feature := "advisory-degradation"
	const fingerprint = "same-advisory-blocker"
	specJSON := `{
  "feature_name": "advisory-degradation",
  "phase": "requirements-generated",
  "approvals": {
    "requirements": {"generated": true, "approved": true},
    "design": {"generated": true, "approved": false},
    "tasks": {"generated": false, "approved": false}
  }
}`
	cwd := setupFeatureCWD(t, feature, "Brief 本文", specJSON)
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}
	validateCount := 0
	agent := &scriptedAgent{
		Respond: func(call recordedAgentCall) (provider.AgentResponse, error) {
			switch {
			case call.Kind == "commit":
				return commitOKResp(), nil
			case strings.Contains(call.Prompt, "/kiro-validate-design "+feature):
				validateCount++
				resp := validateDesignRemediationResp(
					"go",
					true,
					"概ね OK",
					fmt.Sprintf("advisory %d", validateCount),
					feature,
					fingerprint,
				)
				resp.SessionID = fmt.Sprintf("vd-%d", validateCount)
				return resp, nil
			default:
				return skillNoInquiryResp(), nil
			}
		},
	}

	if err := runKiroSpecLoop(context.Background(), cwd, onEvent, ask, agent); err != nil {
		t.Fatalf("runKiroSpecLoop returned error: %v", err)
	}
	rr := mustReadResultJSON(t, cwd)
	if rr.TerminationReason != RunReasonCompleted {
		t.Fatalf("advisory degradation は completed で終端する: got=%q", rr.TerminationReason)
	}
	if validateCount != 3 {
		t.Fatalf("advisory が連続する場合 validate-design は max=3 回呼ばれる: got=%d", validateCount)
	}
	if len(rr.Advisories) != 3 {
		t.Fatalf("Advisories は 3 件: got=%d", len(rr.Advisories))
	}
	for i, a := range rr.Advisories {
		if a.Verdict != "go" {
			t.Errorf("Advisories[%d].Verdict=%q, want go", i, a.Verdict)
		}
		if a.Attempt != i+1 {
			t.Errorf("Advisories[%d].Attempt=%d, want %d", i, a.Attempt, i+1)
		}
	}
	// step 6 (spec-tasks) まで実行されている
	hasSpecTasks := false
	for _, c := range agent.Calls {
		if strings.Contains(c.Prompt, "/kiro-spec-tasks "+feature) {
			hasSpecTasks = true
			break
		}
	}
	if !hasSpecTasks {
		t.Errorf("advisory degradation 後は spec-tasks まで進むべき")
	}
}

// TestKiroSpecLoopValidateDesignInjectsContractBlock は workflow 層が validation contract block を
// validate-design (および任意の RequiresVerdict step) の prompt に注入していることを確認する。
//
// SKILL は無改変のため、workflow 層が prompt 注入経由で validator に
// 「verdict / has_advisory / advisory の使い分け + retry semantics」を伝える必要がある。
func TestKiroSpecLoopValidateDesignInjectsContractBlock(t *testing.T) {
	feature := "contract-block"
	specJSON := `{
  "feature_name": "contract-block",
  "phase": "requirements-generated",
  "approvals": {
    "requirements": {"generated": true, "approved": true},
    "design": {"generated": true, "approved": false},
    "tasks": {"generated": false, "approved": false}
  }
}`
	cwd := setupFeatureCWD(t, feature, "Brief 本文", specJSON)
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}
	agent := &scriptedAgent{
		Respond: func(call recordedAgentCall) (provider.AgentResponse, error) {
			switch {
			case call.Kind == "commit":
				return commitOKResp(), nil
			case strings.Contains(call.Prompt, "/kiro-validate-design "+feature):
				return provider.AgentResponse{
					Success:   true,
					SessionID: "vd",
					StructuredOutput: map[string]any{
						"has_inquiry":  false,
						"summary":      "GO",
						"verdict":      "go",
						"has_advisory": false,
					},
				}, nil
			default:
				return skillNoInquiryResp(), nil
			}
		},
	}
	if err := runKiroSpecLoop(context.Background(), cwd, onEvent, ask, agent); err != nil {
		t.Fatalf("runKiroSpecLoop returned error: %v", err)
	}

	// validate-design の skill 呼び出しを見つけ、contract block マーカが含まれていることを確認
	var vdPrompt string
	var vdOptions *workflowctx.CallAgentOptions
	for _, c := range agent.Calls {
		if c.Kind == "skill" && strings.Contains(c.Prompt, "/kiro-validate-design "+feature) {
			vdPrompt = c.Prompt
			vdOptions = c.Options
			break
		}
	}
	if vdPrompt == "" {
		t.Fatalf("validate-design skill prompt が見つからない")
	}
	if !strings.Contains(vdPrompt, "# Output Contract") {
		t.Errorf("validate-design prompt に '# Output Contract' marker が無い:\n%s", vdPrompt)
	}
	if !strings.Contains(vdPrompt, "has_advisory") {
		t.Errorf("validate-design prompt に 'has_advisory' contract 説明が無い:\n%s", vdPrompt)
	}
	for _, field := range []string{
		"blocker_category",
		"blocker_fingerprint",
		"affected_artifacts",
		"recommended_action",
		"owner_boundary",
		"owner_spec",
	} {
		if !strings.Contains(vdPrompt, field) {
			t.Errorf("validate-design prompt に remediation field %q の説明が無い:\n%s", field, vdPrompt)
		}
	}
	if vdOptions == nil || vdOptions.OutputFormat == nil {
		t.Fatalf("validate-design skill 呼び出しに OutputFormat が設定されていない: %+v", vdOptions)
	}
	props := remediationProperties(t, requireRemediationSchemaRoot(t, vdOptions.OutputFormat.Schema))
	for _, field := range []string{
		"blocker_category",
		"blocker_fingerprint",
		"affected_artifacts",
		"recommended_action",
		"owner_boundary",
		"owner_spec",
	} {
		if _, ok := props[field]; !ok {
			t.Fatalf("validate-design OutputFormat schema missing remediation field %q", field)
		}
	}
	// non-validate step (spec-design など) には contract block を付けない
	for _, c := range agent.Calls {
		if c.Kind != "skill" {
			continue
		}
		if strings.Contains(c.Prompt, "/kiro-validate-design ") {
			continue
		}
		if strings.Contains(c.Prompt, "# Output Contract") {
			t.Errorf("non-validate skill prompt に Output Contract が付いている (RequiresVerdict=false のはず): %q", c.Prompt)
		}
	}
}

// =============================================================================
// kiro-spec-loop-facets task 6.4: Persona 注入 + Previous Response chain の
//
//	統合テスト (Design §Testing Strategy Integration 2-4)
//
// =============================================================================
//
// 観察対象 (table-driven):
//   - I2: 1 段目 spec-init の初回 skill 呼び出し prompt に `# Previous Response` 文字列が
//     現れない (Req 1.2, 5.3) — 同時に Options.IncludePreviousResponse=false /
//     PreviousResponseContent="" であること
//   - I3: N>=2 段目の初回 skill 呼び出しで前置 wiring (Options.IncludePreviousResponse=true /
//     Options.PreviousResponseContent) が N-1 段目 scriptedAgent 経由で観察した
//     response Summary と一致すること (Req 1.1, 5.1)
//   - I4: 全 6 phase の session A 初回 skill prompt 先頭に `# Persona\n\n<facet content>\n\n---\n\n`
//     block が prepend され、内容が phase に対応する facet (`spec-analyst` /
//     `spec-architect` / `validator`) の embed content と完全一致すること (Req 2.1, 2.2)
//
// seam の注意:
//   - 本 test seam (scriptedAgent / recordedAgent) は workflowctx.AgentCaller レイヤで
//     intercept する。foundation Spec C 経路 (`internal/agent.Agent.CallAgent` 入口の
//     `prependPreviousResponse`) は本 seam の下流で走るため、`recordedAgentCall.Prompt`
//     には `# Previous Response` block が現れず、observe 対象は `Options.IncludePreviousResponse`
//     / `Options.PreviousResponseContent` のペアになる (Design §Components 注記)。
//   - 一方 `# Persona\n\n...\n\n---\n\n` prefix は本 seam の上流 (specLoopRunner.runInquiryLoop の
//     `personaPrefix(resolved) + step.Command(...)`) で prompt に prepend されるため、
//     `recordedAgentCall.Prompt` で直接 observable。
//   - これにより I4 は Prompt 観察、I2/I3 は Options 観察、で 3 アサーションを 1 つの
//     table-driven loop に揃える。
//
// TestKiroSpecLoopSuccess (task 6.1) との差異:
//   - 本 test は per-phase distinct な summary を返し、chain (N-1 → N) を曖昧さなく
//     verify する (TestKiroSpecLoopSuccess は recordedAgent の固定 summary="ok" を使う)
//   - 本 test は table-driven 単位を「6 phase × 3 アサーション」に揃え、task 6.4 の
//     Integration 2-4 のみを focused に観測する
func TestKiroSpecLoopPersonaAndPreviousResponseIntegration(t *testing.T) {
	feature := "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}

	// embed 経路で persona facet content を解決し、I4 の期待値として使う。
	// 同 cwd には override (`<cwd>/.rilll-config/facets/personas/...`) を配置していないため、
	// 解決経路は embed (`internal/facet/facets/personas/<name>.md`) に固定される。
	wantPersonaByFacet := map[string]string{}
	for _, name := range []string{"spec-analyst", "spec-architect", "validator"} {
		rf, err := facet.ResolveFacet(cwd, facet.Persona, name)
		if err != nil {
			t.Fatalf("facet.ResolveFacet(%s): %v", name, err)
		}
		if rf.Content == "" {
			t.Fatalf("facet.ResolveFacet(%s): Content 非空であるべき (embed asset 整備不足)", name)
		}
		wantPersonaByFacet[name] = rf.Content
	}
	phaseToFacet := map[string]string{
		"spec-init":         "spec-analyst",
		"spec-requirements": "spec-analyst",
		"validate-gap":      "validator",
		"spec-design":       "spec-architect",
		"validate-design":   "validator",
		"spec-tasks":        "spec-architect",
	}

	// phase ごとに distinct な summary を返し、N-1 → N の chain を曖昧さなく観察する。
	// summary 値はそれぞれ unique かつ phase 名を含むため、誤って同一値で chain が
	// 通ったように見える failure mode を防ぐ。
	summaryByPhase := map[string]string{
		"spec-init":         "S1-spec-init-summary",
		"spec-requirements": "S2-spec-requirements-summary",
		"validate-gap":      "S3-validate-gap-summary",
		"spec-design":       "S4-spec-design-summary",
		"validate-design":   "S5-validate-design-summary",
		"spec-tasks":        "S6-spec-tasks-summary",
	}
	orderedPhases := []string{"spec-init", "spec-requirements", "validate-gap", "spec-design", "validate-design", "spec-tasks"}

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "commit":
			return commitOKResp(), nil
		case "skill":
			// どの phase の skill 呼び出しか option.StepName ("<NN>-<phase>") から識別する。
			phase := ""
			if call.Options != nil {
				for _, p := range orderedPhases {
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

	// session A 初回 skill 呼び出しを phase ごとに 1 件抽出する。
	// 条件: Kind="skill" / ResumeSessionID="" (初回) / StepName が "-answer" suffix ではない。
	// 本シナリオでは has_inquiry=false 即完走のため各 phase 1 件のみ発火する。
	skillCallByPhase := map[string]recordedAgentCall{}
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
		t.Fatalf("session A 初回 skill 呼び出しは 6 phase 分発火することを期待: got=%d (calls=%+v)",
			len(skillCallByPhase), agent.Calls)
	}

	// table-driven assertion (I2 / I3 / I4 を 1 loop に集約)
	for i, phase := range orderedPhases {
		c, ok := skillCallByPhase[phase]
		if !ok {
			t.Fatalf("phase %q (%d 段目): session A 初回 skill 呼び出しが未発火", phase, i+1)
		}
		if c.Options == nil {
			t.Errorf("phase %q (%d 段目): Options=nil; Previous Response wiring を観察できない", phase, i+1)
			continue
		}

		// ----- I4: slash-first 順 + Persona block 後置 -----
		facetName := phaseToFacet[phase]
		wantPersona := wantPersonaByFacet[facetName]
		wantSlashHead := "/kiro-" + phase + " " + feature
		wantPersonaBlock := "\n\n---\n\n# Persona\n\n" + wantPersona
		if !strings.HasPrefix(c.Prompt, wantSlashHead) {
			head := c.Prompt
			if len(head) > 80 {
				head = head[:80]
			}
			t.Errorf("phase %q (%d 段目, facet=%s): I4 — prompt 先頭が slash command と一致しない (slash-first 順)\n got_head=%q\nwant_head_prefix=%q",
				phase, i+1, facetName, head, wantSlashHead)
		}
		if !strings.Contains(c.Prompt, wantPersonaBlock) {
			t.Errorf("phase %q (%d 段目, facet=%s): I4 — prompt に persona block が含まれない\n want_block=%q\ngot_head=%q",
				phase, i+1, facetName, wantPersonaBlock, c.Prompt[:min(len(c.Prompt), 512)])
		}

		// ----- I2 / I3: Previous Response 埋め込み (slash-first 順では prompt body 内に直接埋め込まれる) -----
		// spec-loop は foundation 側 IncludePreviousResponse 経路を使わない。
		if c.Options.IncludePreviousResponse {
			t.Errorf("phase %q (%d 段目): Options.IncludePreviousResponse=false を期待 (spec-loop は foundation 前置経路を使わない): got true",
				phase, i+1)
		}
		if c.Options.PreviousResponseContent != "" {
			t.Errorf("phase %q (%d 段目): Options.PreviousResponseContent=\"\" を期待: got %q",
				phase, i+1, c.Options.PreviousResponseContent)
		}
		if i == 0 {
			// I2: 1 段目 spec-init では前段なし → prompt body に `# Previous Response` を含まない
			if strings.Contains(c.Prompt, "# Previous Response") {
				t.Errorf("phase %q (1 段目): I2 — prompt に `# Previous Response` 文字列が現れてはならない (前段なし):\n%s",
					phase, c.Prompt)
			}
			continue
		}
		// I3: N>=2 段目では prompt body 内に `\n\n---\n\n# Previous Response\n\n<N-1 段目 Summary>` が埋め込まれる
		prevPhase := orderedPhases[i-1]
		wantPrev := summaryByPhase[prevPhase]
		wantPrevBlock := "\n\n---\n\n# Previous Response\n\n" + wantPrev
		if !strings.Contains(c.Prompt, wantPrevBlock) {
			t.Errorf("phase %q (%d 段目, prev=%s): I3 — prompt に Previous Response block が含まれない (N-1 段目 Summary と一致するべき)\n want_block=%q\ngot=%q",
				phase, i+1, prevPhase, wantPrevBlock, c.Prompt[:min(len(c.Prompt), 1024)])
		}
	}
}

// =============================================================================
// kiro-spec-loop-facets task 6.5: inquiryloop resume 周回 / answerer 経路で
//
//	前置・Persona facet 漏出が起きないことを観察する統合テスト
//	(Design §Testing Strategy Integration 5-6)
//
// =============================================================================
//
// 観察対象 (table-driven):
//   - I5: inquiryloop resume 周回 (round 1+ で `resumeSessionID != ""`) の skill 再呼び出し
//     prompt に `# Previous Response` 文字列が現れない (Req 3.4 / Design §Decisions 1
//     3 条件ゲートのうち (a) `resumeSessionID == ""` を破る path)。
//     同時に session A 用 facet content (spec-analyst / spec-architect / validator) も
//     resume 経路では再 prepend されないこと (resume は同一 session 内で persona は
//     既に注入済み; Design §Components personaPrefix の適用範囲は「初回 skill の prompt
//     先頭」に限定)。
//   - I6: answerer 呼び出し (Kind == "answerer" / stepName 末尾 `-answer`) の prompt に
//     (i) `# Previous Response` 文字列、(ii) `# Persona\n\n<spec-analyst|spec-architect|
//     validator content>` のいずれも現れない (Req 1.1 余事象 / 2.2 余事象 / 3.4)。
//     同時に session B 既存構造 (`# Persona (architecture-quality) → # Context →
//     # Inquiry → # 回答方針`) が `buildAnswerPrompt` の出力から保たれていること。
//
// シナリオ:
//   - 6 phase それぞれで 1 round の inquiry を発火させ round 1 で完走する
//     (initial skill = has_inquiry=true → answerer = has_inquiry=false →
//     resume skill = has_inquiry=false → /git-commit の 4 calls per phase)。
//   - 6 phase × 4 calls = 24 calls 合計。
//   - 6 phase は spec-analyst (2) / validator (2) / spec-architect (2) を全カバーするので、
//     answerer prompt に 3 種 facet いずれも漏出しないことを 1 test で機械的に否定形検証できる。
//
// seam の注意 (TestKiroSpecLoopPersonaAndPreviousResponseIntegration と同形):
//   - 本 test seam (scriptedAgent) は `workflowctx.AgentCaller` 層で intercept しているため、
//     foundation Spec C 経路 (`internal/agent.Agent.CallAgent` 入口の `prependPreviousResponse`)
//     は本 seam の下流で走る。`recordedAgentCall.Prompt` には `# Previous Response` block が
//     現れないため、I5 / I6 の `# Previous Response` 否定形は「上流で文字列も無く、下流に渡す
//     `Options.IncludePreviousResponse=false` でも前置されないこと」を二重に確認する形になる。
//   - `# Persona\n\n<facet content>\n\n---\n\n` prefix は本 seam の上流 (specLoopRunner.runInquiryLoop の
//     `personaPrefix(resolved) + step.Command(...)`) で prompt に prepend されるため、
//     初回 skill 呼び出しでは `recordedAgentCall.Prompt` で直接 observable。本 test の I5 / I6 は
//     その prepend が「resume 経路 / answerer 経路では行われない」ことを否定形で検証する。
//
// TestSpecLoopRunner_AnswererPromptHasNoPersonaFacet (task 5.1 由来 unit test) との差異:
//   - unit test は spec-init 1 phase × 1 facet (spec-analyst) のみ観察
//   - 本 integration test は 6 phase × 3 facet を全カバーし、resume skill 経路の否定形 / session B
//     既存構造 (Persona → Context → Inquiry → 回答方針 順序) も同 loop で同時 verify する。
func TestKiroSpecLoopAnswererAndResumeNoLeakage(t *testing.T) {
	feature := "demo"
	cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
	onEvent, _ := collectEvents()
	ask := &fakeAsk{Selected: feature}

	// session A 用 facet content (spec-analyst / spec-architect / validator) を embed 経路で
	// 解決し、I5 / I6 の「prompt にこの content が現れないこと」否定形アサーションに使う。
	// 同 cwd には override (`<cwd>/.rilll-config/facets/personas/...`) を配置していないため、
	// 解決経路は embed (`internal/facet/facets/personas/<name>.md`) に固定される。
	sessionAFacetContents := map[string]string{}
	for _, name := range []string{"spec-analyst", "spec-architect", "validator"} {
		rf, err := facet.ResolveFacet(cwd, facet.Persona, name)
		if err != nil {
			t.Fatalf("facet.ResolveFacet(%s): %v", name, err)
		}
		if rf.Content == "" {
			t.Fatalf("facet.ResolveFacet(%s): Content 非空 (embed asset 整備不足)", name)
		}
		sessionAFacetContents[name] = rf.Content
	}

	// session B 既存構造の verify 用に architecture-quality persona を解決する。
	// session B `buildAnswerPrompt` の `# Persona\n` ブロックには本 content が
	// 埋め込まれる (Design §Decisions 3 / inquiryloop.buildAnswerPrompt)。
	archPersona, err := resolveSessionBPersona(cwd, "architecture-quality")
	if err != nil {
		t.Fatalf("resolveSessionBPersona(architecture-quality): %v", err)
	}
	if archPersona.Content == "" {
		t.Fatalf("architecture-quality persona Content 非空 (embed asset 整備不足)")
	}

	agent := &scriptedAgent{}
	agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
		switch call.Kind {
		case "commit":
			return commitOKResp(), nil
		case "answerer":
			// 1 round で完走させるため answerer は has_inquiry=false / summary を返す。
			return answerOKResp(), nil
		case "skill":
			if call.ResumeSessionID == "" {
				// 初回 skill: has_inquiry=true で 1 round 発火させる
				return skillInquiryResp(), nil
			}
			// resume 周回 skill: has_inquiry=false で完走
			return skillNoInquiryResp(), nil
		case "fix-edit":
			// fix-edit (Turn 1) は OutputFormat=nil で markdown を返す契約。
			// 本 test の focus (resume skill / answerer の漏出否定形) ではないが、
			// 各 step が rounds>0 で完走するため runFixPhase が必ず駆動される。
			// success=false で短絡せず Turn 2 (fix-judge) まで通すため、success=true の
			// markdown を返す。fix-edit / fix-judge は agentCallerAdapter を経由せず
			// agent.CallAgent を直接呼ぶ (Persona prefix / Previous Response 配線とは
			// 無関係) ため、I5 / I6 の `Kind == "skill"` / `Kind == "answerer"` フィルタには
			// 引っかからない。
			return provider.AgentResponse{
				Success:   true,
				Content:   "fix edit summary",
				SessionID: "session-fix-edit",
			}, nil
		case "fix-judge":
			// fix-judge (Turn 2) は OutputFormat=Schema() で構造化出力を返す契約。
			// summary を返して fix.OK=true で完走させる。
			return provider.AgentResponse{
				Success: true,
				StructuredOutput: map[string]any{
					"has_inquiry": false,
					"summary":     "fix judge summary",
				},
				SessionID: "session-fix-judge",
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

	// 期待呼び出し件数: 6 phase × (initial skill + answerer + resume skill + fix-edit + fix-judge + commit) = 36 calls
	// (各 step が rounds>0 で完走するため runFixPhase の Turn 1/Turn 2 が必ず駆動される。
	// fix-edit / fix-judge は agentCallerAdapter を経由せず本 test の focus 外だが、
	// 期待件数のセンチネルとして count に含める。)
	if len(agent.Calls) != 36 {
		t.Fatalf("6 phase × 6 calls (initial skill + answerer + resume skill + fix-edit + fix-judge + commit) = 36 calls を期待: got=%d calls=%v",
			len(agent.Calls), summarizeCalls(agent.Calls))
	}

	orderedPhases := []string{"spec-init", "spec-requirements", "validate-gap", "spec-design", "validate-design", "spec-tasks"}

	// resume skill / answerer 呼び出しを kind で分類して 6 phase 分そろっていることを件数で先に verify する。
	var resumeSkillCalls []recordedAgentCall
	var answererCalls []recordedAgentCall
	for _, c := range agent.Calls {
		switch c.Kind {
		case "skill":
			if c.ResumeSessionID != "" {
				resumeSkillCalls = append(resumeSkillCalls, c)
			}
		case "answerer":
			answererCalls = append(answererCalls, c)
		}
	}
	if len(resumeSkillCalls) != 6 {
		t.Fatalf("6 phase 分の resume skill 呼び出しを期待: got=%d (calls=%+v)",
			len(resumeSkillCalls), summarizeCalls(agent.Calls))
	}
	if len(answererCalls) != 6 {
		t.Fatalf("6 phase 分の answerer 呼び出しを期待: got=%d (calls=%+v)",
			len(answererCalls), summarizeCalls(agent.Calls))
	}

	// ---------- I5: resume skill prompt assertions (Req 3.4) ----------
	// 全 6 phase の resume skill 呼び出しで:
	//   (a) Prompt 自体に `# Previous Response` 文字列が現れない (workflowctx 層 seam の上流確認)
	//   (b) Options.IncludePreviousResponse=false (Req 3.4 / 3 条件ゲートで resume を除外)
	//   (c) Options.PreviousResponseContent="" (zero value)
	//   (d) Prompt に session A 用 facet content (spec-analyst/spec-architect/validator) も
	//       prepend されない (resume 経路は personaPrefix を再付加しない設計;
	//       session A は同一 session を resume するため Persona は既に session 内にある)
	for i, c := range resumeSkillCalls {
		stepName := ""
		if c.Options != nil {
			stepName = c.Options.StepName
		}
		if strings.Contains(c.Prompt, "# Previous Response") {
			t.Errorf("resume[%d] (stepName=%q): I5 — resume skill prompt に `# Previous Response` 文字列が現れてはならない (Req 3.4):\nprompt=%q",
				i, stepName, c.Prompt)
		}
		if c.Options == nil {
			t.Errorf("resume[%d]: Options=nil; Previous Response wiring を観察できない", i)
			continue
		}
		if c.Options.IncludePreviousResponse {
			t.Errorf("resume[%d] (stepName=%q): I5 — Options.IncludePreviousResponse=false を期待 (resume / repair 周回除外; Req 3.4): got true",
				i, stepName)
		}
		if c.Options.PreviousResponseContent != "" {
			t.Errorf("resume[%d] (stepName=%q): I5 — Options.PreviousResponseContent=\"\" を期待: got %q",
				i, stepName, c.Options.PreviousResponseContent)
		}
		// session A 経路の facet content が resume prompt 先頭に再 prepend されないこと
		// (混同防御: 6 phase × 3 facet すべて否定形で確認)
		for facetName, fc := range sessionAFacetContents {
			if strings.Contains(c.Prompt, fc) {
				t.Errorf("resume[%d] (stepName=%q): I5 — resume skill prompt に session A 用 facet content (%s) が現れてはならない (resume 経路は personaPrefix 再付加なし):\nprompt head=%q",
					i, stepName, facetName, c.Prompt[:min(len(c.Prompt), 200)])
			}
		}
	}

	// ---------- I6: answerer prompt assertions (Req 1.1 余事象 / 2.2 余事象 / 3.4) ----------
	for i, c := range answererCalls {
		stepName := ""
		if c.Options != nil {
			stepName = c.Options.StepName
		}
		// (i) `# Previous Response` 文字列が現れない
		if strings.Contains(c.Prompt, "# Previous Response") {
			t.Errorf("answerer[%d] (stepName=%q): I6 — answerer prompt に `# Previous Response` 文字列が現れてはならない (Req 1.1 余事象 / 3.4):\nprompt=%q",
				i, stepName, c.Prompt)
		}
		if c.Options == nil {
			t.Errorf("answerer[%d]: Options=nil; Previous Response wiring を観察できない", i)
			continue
		}
		if c.Options.IncludePreviousResponse {
			t.Errorf("answerer[%d] (stepName=%q): I6 — Options.IncludePreviousResponse=false を期待 (answerer 除外; Req 3.4): got true",
				i, stepName)
		}
		if c.Options.PreviousResponseContent != "" {
			t.Errorf("answerer[%d] (stepName=%q): I6 — Options.PreviousResponseContent=\"\" を期待: got %q",
				i, stepName, c.Options.PreviousResponseContent)
		}
		// stepName が "-answer" suffix で終わっていること (3 条件ゲート (b) の前提 / engine 命名規約)
		if !strings.HasSuffix(stepName, "-answer") {
			t.Errorf("answerer[%d]: stepName が '-answer' suffix で終わるべき (engine 命名規約): got %q",
				i, stepName)
		}
		// (ii) session A 用 facet content が answerer prompt に現れないこと (Req 2.2 余事象)
		// 混同防御: spec-design phase の answerer に spec-analyst content が漏れていないか等を
		// 機械的に否定形で確認するため、6 phase × 3 facet すべての組合せをスキャンする。
		for facetName, fc := range sessionAFacetContents {
			if strings.Contains(c.Prompt, fc) {
				t.Errorf("answerer[%d] (stepName=%q): I6 — answerer prompt に session A 用 facet content (%s) が現れてはならない (Req 2.2 余事象):\nprompt head=%q",
					i, stepName, facetName, c.Prompt[:min(len(c.Prompt), 200)])
			}
		}
		// (iii) session B 既存構造 (`# Persona (architecture-quality) → # Context → # Inquiry → # 回答方針`)
		// が保たれていること:
		//   - prompt 先頭が `# Persona\n` (buildAnswerPrompt の section 1)
		//   - architecture-quality persona content が含まれる
		//   - section 順序: Persona → Context → Inquiry → 回答方針
		//   - 1 round 完走シナリオでは trail が空のため `# History\n` は現れない
		if !strings.HasPrefix(c.Prompt, "# Persona\n") {
			t.Errorf("answerer[%d] (stepName=%q): session B prompt は `# Persona\\n` で始まるべき (buildAnswerPrompt 構造): got head=%q",
				i, stepName, c.Prompt[:min(len(c.Prompt), 64)])
		}
		if !strings.Contains(c.Prompt, archPersona.Content) {
			t.Errorf("answerer[%d] (stepName=%q): session B prompt に architecture-quality persona content が含まれるべき (Design §Decisions 3): missing\nprompt head=%q",
				i, stepName, c.Prompt[:min(len(c.Prompt), 200)])
		}
		// section 順序 verify: Persona → Context → Inquiry → 回答方針 が単調増加で出現する
		sections := []struct {
			label  string
			marker string
		}{
			{"# Persona", "# Persona\n"},
			{"# Context", "# Context\n"},
			{"# Inquiry", "# Inquiry\n"},
			{"# 回答方針", "# 回答方針"},
		}
		prevPos := -1
		for _, s := range sections {
			pos := strings.Index(c.Prompt, s.marker)
			if pos < 0 {
				t.Errorf("answerer[%d] (stepName=%q): session B prompt に %q セクションが含まれるべき (buildAnswerPrompt 構造):\nprompt=%q",
					i, stepName, s.label, c.Prompt)
				continue
			}
			if pos <= prevPos {
				t.Errorf("answerer[%d] (stepName=%q): session B prompt の section 順序違反 (%q が前セクションより前に出現):\nprompt=%q",
					i, stepName, s.label, c.Prompt)
			}
			prevPos = pos
		}
		// 1 round 完走シナリオでは trail が空のため `# History\n` ブロックは現れない契約を明示確認
		if strings.Contains(c.Prompt, "# History\n") {
			t.Errorf("answerer[%d] (stepName=%q): round 1 完走シナリオでは buildAnswerPrompt の trail が空のため `# History` ブロックは現れないはず:\nprompt=%q",
				i, stepName, c.Prompt)
		}
	}

	// 6 phase 全カバー verify: stepName から phase を逆引きして 6 phase 分の answerer が
	// 揃っていることを集合として確認する。これにより I6 のアサーションが
	// spec-analyst (2) / validator (2) / spec-architect (2) の 3 種 facet すべてを
	// 機械的に否定形で検査済みであることを保証する (混同防御の網羅性ロック)。
	answererPhaseSet := map[string]bool{}
	for _, c := range answererCalls {
		if c.Options == nil {
			continue
		}
		for _, p := range orderedPhases {
			if strings.HasSuffix(c.Options.StepName, "-"+p+"-answer") {
				answererPhaseSet[p] = true
				break
			}
		}
	}
	for _, p := range orderedPhases {
		if !answererPhaseSet[p] {
			t.Errorf("phase %q の answerer 呼び出しが未発火 (6 phase 全カバーが I6 否定形検査の網羅性前提): set=%v",
				p, answererPhaseSet)
		}
	}
}

// =============================================================================
// kiro-spec-loop-facets task 6.6: restart 経路の on-disk fallback hit / miss /
//
//	graceful degradation を観察する統合テスト
//	(Design §Testing Strategy Integration 7-8)
//
// =============================================================================
//
// 観察対象 (table-driven, 4 fixture):
//
//   - hit            : approvals.requirements.approved=true (= startOrder=4) かつ同一 runDir 内に
//     03-validate-gap/result.json (summary="restart-fallback-summary") を seed する。
//     spec-design 起動時の session A 初回 skill 呼び出しが
//     Options.IncludePreviousResponse=true /
//     Options.PreviousResponseContent="restart-fallback-summary" となること
//     (Req 1.1, 5.3 / 二段フォールバック (3) on-disk hit 経路)。
//   - miss           : approvals.design.approved=true (= startOrder=6) かつ
//     05-validate-design/result.json **不在**。spec-tasks 起動時の prompt に
//     `# Previous Response` 前置がなく
//     (Options.IncludePreviousResponse=false / Content="")、後段が完走する
//     (Req 1.4, 5.3 / 二段フォールバック (4) graceful 経路: ENOENT)。
//   - broken-json    : 同 startOrder=6 で 05-validate-design/result.json に壊れた JSON を配置。
//     同様に前置なしで完走する (Req 1.4 / 二段フォールバック (4) graceful 経路: decode 失敗)。
//   - empty-summary  : 同 startOrder=6 で 05-validate-design/result.json に valid な JSON
//     (summary="") を配置。同様に前置なしで完走する
//     (Req 1.4 / 二段フォールバック (4) graceful 経路: summary 空)。
//
// seam の注意 (task 6.4/6.5 と同形):
//   - 本 test seam (recordedAgent) は workflowctx.AgentCaller 層で intercept する。
//   - kiro-spec-loop は foundation 前置経路 (IncludePreviousResponse) を使わず、
//     `runInquiryLoop` 内で `previousResponseBlock(prev)` を skill コマンドの後ろに連結する
//     (slash-first 順)。観察対象は `recordedAgentCall.Prompt` 本文に含まれる
//     `\n\n---\n\n# Previous Response\n\n<summary>` ブロックそのもの (= previousSummaryForStep の
//     on-disk fallback 戻り値を後置 block 経由で観察する seam)。
//   - Options.IncludePreviousResponse / Options.PreviousResponseContent は kiro-spec-loop
//     からの呼び出しでは常に false / 空文字となる (adapter は素の forwarder)。
//
// runDir 固定化:
//   - 通常 runDir は `state.RunID = workflow.RunIDFromContext(ctx)` 経由で生成 ID が割り当てられ、
//     test 側からは事前に予測できない。本 test は seed ファイルを runDir 配下に置きたいため、
//     `workflowctx.ContextWithRunMetadata` で WorkflowRunID を固定 ID に注入する。これにより
//     runDir = `<cwd>/.rilll/kiro-spec-loop/<fixed-runID>` が確定し、事前 seed が可能になる。
func TestKiroSpecLoopRestartOnDiskFallback(t *testing.T) {
	// 1 件で startOrder=4 (requirements approved) → spec-design から、
	// 残り 3 件で startOrder=6 (design approved) → spec-tasks 1 phase のみ実行、の 2 シナリオ。
	const specJSONRequirementsApproved = `{
  "feature_name": "restart-feature",
  "phase": "requirements-generated",
  "approvals": {
    "requirements": {"generated": true, "approved": true},
    "design": {"generated": false, "approved": false},
    "tasks": {"generated": false, "approved": false}
  }
}`
	const specJSONDesignApproved = `{
  "feature_name": "restart-feature",
  "phase": "design-generated",
  "approvals": {
    "requirements": {"generated": true, "approved": true},
    "design": {"generated": true, "approved": true},
    "tasks": {"generated": false, "approved": false}
  }
}`

	cases := []struct {
		name string
		// fixture
		specJSON       string // approvals state → startOrder の決定材料
		startOrder     int    // 期待される startOrder (= 観察対象 phase の order)
		startPhase     string // 観察対象 phase 名 (startOrder の phase)
		prevOrder      int    // seed 配置先の prev phase order (= startOrder - 1)
		prevPhase      string // seed 配置先の prev phase 名
		seedResultJSON string // result.json 本文。"" のときは seed しない (= miss シナリオ)。
		// expectations (slash-first 順: spec-loop は foundation 前置経路を使わず prompt body に直接埋め込む)
		wantPrevBlockBody string // "" のとき prompt body に `# Previous Response` を含まないことを期待。
		//                          非空のとき prompt body に `\n\n---\n\n# Previous Response\n\n<wantPrevBlockBody>` を含むことを期待。
		expectedCalls    int // agent.Calls 件数 (skill + commit; rounds=0 即完走前提)
		expectedRemSteps int // result.json.steps の件数 (= 7 - startOrder)
	}{
		{
			name:              "hit",
			specJSON:          specJSONRequirementsApproved,
			startOrder:        4,
			startPhase:        "spec-design",
			prevOrder:         3,
			prevPhase:         "validate-gap",
			seedResultJSON:    `{"order":3,"phase":"validate-gap","rounds":0,"lastSession":"session-skill","summary":"restart-fallback-summary","commitOK":true,"commitOutput":"commit ok"}`,
			wantPrevBlockBody: "restart-fallback-summary",
			expectedCalls:     6, // 3 phases × (skill + commit)
			expectedRemSteps:  3,
		},
		{
			name:              "miss",
			specJSON:          specJSONDesignApproved,
			startOrder:        6,
			startPhase:        "spec-tasks",
			prevOrder:         5,
			prevPhase:         "validate-design",
			seedResultJSON:    "", // 05-validate-design/result.json を作らない (= ENOENT graceful)
			wantPrevBlockBody: "",
			expectedCalls:     2, // 1 phase × (skill + commit)
			expectedRemSteps:  1,
		},
		{
			name:              "broken-json",
			specJSON:          specJSONDesignApproved,
			startOrder:        6,
			startPhase:        "spec-tasks",
			prevOrder:         5,
			prevPhase:         "validate-design",
			seedResultJSON:    "{not valid json", // JSON 不正で json.Unmarshal が失敗 → "" graceful
			wantPrevBlockBody: "",
			expectedCalls:     2,
			expectedRemSteps:  1,
		},
		{
			name:              "empty-summary",
			specJSON:          specJSONDesignApproved,
			startOrder:        6,
			startPhase:        "spec-tasks",
			prevOrder:         5,
			prevPhase:         "validate-design",
			seedResultJSON:    `{"order":5,"phase":"validate-design","rounds":0,"lastSession":"session-skill","summary":"","commitOK":true,"commitOutput":"commit ok"}`,
			wantPrevBlockBody: "",
			expectedCalls:     2,
			expectedRemSteps:  1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			feature := "restart-feature"
			cwd := setupFeatureCWD(t, feature, "Brief 本文", c.specJSON)

			// runDir を予測可能にするため、ContextWithRunMetadata で WorkflowRunID を固定する。
			// これにより runDir = <cwd>/.rilll/kiro-spec-loop/<fixedRunID> が確定し、
			// seed (= prev phase result.json) を事前配置できる。
			fixedRunID := "restart-runid-" + c.name
			ctx := workflowctx.ContextWithRunMetadata(context.Background(),
				workflowctx.RunMetadata{WorkflowRunID: fixedRunID})
			runDir := filepath.Join(cwd, ".rilll", "kiro-spec-loop", fixedRunID)

			// seed: prev phase の result.json を事前配置する (miss シナリオは seedResultJSON=="" でスキップ)。
			if c.seedResultJSON != "" {
				prevDir := filepath.Join(runDir, fmt.Sprintf("%02d-%s", c.prevOrder, c.prevPhase))
				if err := os.MkdirAll(prevDir, 0o755); err != nil {
					t.Fatalf("seed mkdir %s: %v", prevDir, err)
				}
				prevPath := filepath.Join(prevDir, "result.json")
				if err := os.WriteFile(prevPath, []byte(c.seedResultJSON), 0o644); err != nil {
					t.Fatalf("seed write %s: %v", prevPath, err)
				}
			}

			onEvent, _ := collectEvents()
			ask := &fakeAsk{Selected: feature}
			// recordedAgent は 6 phase すべて has_inquiry=false / summary="ok" で即完走させ、
			// /git-commit を success=true で返す。これにより rounds=0 / fix phase 未発火で
			// 1 phase あたり skill 1 + commit 1 = 2 calls に揃う。
			agent := &recordedAgent{
				SkillResponse: provider.AgentResponse{
					Success: true,
					StructuredOutput: map[string]any{
						"has_inquiry": false,
						"summary":     "ok",
					},
					SessionID: "session-skill",
				},
				CommitResponse: commitOKResp(),
			}

			if err := runKiroSpecLoop(
				ctx,
				cwd,
				onEvent,
				ask,
				agent,
			); err != nil {
				t.Fatalf("runKiroSpecLoop returned error: %v", err)
			}

			// (a) 観察対象 phase の session A 初回 skill 呼び出しを特定する。
			//     条件: Kind=="skill" / ResumeSessionID=="" (初回) / StepName=="<startOrder:02d>-<startPhase>"
			wantStepName := fmt.Sprintf("%02d-%s", c.startOrder, c.startPhase)
			var target *recordedAgentCall
			for i := range agent.Calls {
				cc := agent.Calls[i]
				if cc.Kind != "skill" || cc.ResumeSessionID != "" || cc.Options == nil {
					continue
				}
				if cc.Options.StepName == wantStepName {
					target = &cc
					break
				}
			}
			if target == nil {
				t.Fatalf("%s: session A 初回 skill 呼び出し (StepName=%q) が見つからない\ncalls=%v",
					c.name, wantStepName, summarizeCalls(agent.Calls))
			}

			// (b) spec-loop は foundation 側 IncludePreviousResponse 経路を使わず prompt body に
			//     直接埋め込む (slash-first 順)。したがって Options は常に zero-value。
			if target.Options.IncludePreviousResponse {
				t.Errorf("%s: Options.IncludePreviousResponse=false を期待 (spec-loop は foundation 前置経路を使わない): got true",
					c.name)
			}
			if target.Options.PreviousResponseContent != "" {
				t.Errorf("%s: Options.PreviousResponseContent=\"\" を期待: got %q",
					c.name, target.Options.PreviousResponseContent)
			}
			// prompt body 内に Previous Response block が埋め込まれていることを (または埋め込まれて
			// いないことを) 観察する。hit ケースでは on-disk fallback で取得した summary が
			// `\n\n---\n\n# Previous Response\n\n<summary>` として埋め込まれる。
			// miss / broken-json / empty-summary では graceful degradation で block 自体が省略される。
			if c.wantPrevBlockBody == "" {
				if strings.Contains(target.Prompt, "# Previous Response") {
					t.Errorf("%s: prompt body に `# Previous Response` を含めてはならない (graceful degradation):\n%s",
						c.name, target.Prompt)
				}
			} else {
				wantBlock := "\n\n---\n\n# Previous Response\n\n" + c.wantPrevBlockBody
				if !strings.Contains(target.Prompt, wantBlock) {
					t.Errorf("%s: prompt body に Previous Response block が含まれない (on-disk fallback hit)\n want_block=%q\ngot=%s",
						c.name, wantBlock, target.Prompt)
				}
			}

			// (c) 後段が abort せず完走することを result.json で verify する
			//     (miss / broken-json / empty-summary は graceful degradation で前置なし継続)。
			rr := mustReadResultJSON(t, cwd)
			if rr.TerminationReason != RunReasonCompleted {
				t.Errorf("%s: termination should be completed (graceful 経路でも後段継続): got=%q",
					c.name, rr.TerminationReason)
			}
			if len(rr.Steps) != c.expectedRemSteps {
				t.Errorf("%s: steps の件数が一致しない (startOrder=%d): got=%d want=%d",
					c.name, c.startOrder, len(rr.Steps), c.expectedRemSteps)
			}
			if len(agent.Calls) != c.expectedCalls {
				t.Errorf("%s: agent.Calls の件数が一致しない (rounds=0 即完走前提): got=%d want=%d\ncalls=%v",
					c.name, len(agent.Calls), c.expectedCalls, summarizeCalls(agent.Calls))
			}
		})
	}
}

// =============================================================================
// kiro-spec-loop-facets task 6.7: provider 横断同一観測 + adapter での
//
//	provider 分岐不在 + Persona override seam を
//	機械的に検証する統合テスト
//	(Design §Testing Strategy Integration 9 /
//	 §Provider Cross Tests)
//
// =============================================================================
//
// 本テストは 3 つの観察可能完了を 1 関数内のサブテストで担保する。
//
//   - sub: identical-prompt-across-providers (Req 1.5, 4.1, 4.2)
//     scriptedAgent の provider mock を Claude / Codex / Cursor の 3 種類で切り替え、
//     6 phase の session A 初回 skill prompt を完全一致 (byte-identical) で観測する。
//     prompt は kiro 配線層 (specLoopRunner.runInquiryLoop の personaPrefix +
//     step.Command + 前置 wiring) で組み立てられ、provider 名には依存しないことを
//     直接的に検証する。
//
//   - sub: no-provider-symbol-in-adapter-and-runinquiryloop (Req 4.3)
//     `internal/workflow/builtin/kiro/spec_loop.go` の `agentCallerAdapter.Call`
//     と `internal/workflow/builtin/kiro/spec_loop_runner.go` の
//     `(*specLoopRunner).runInquiryLoop` の関数本体 (FuncDecl.Body) を go/parser +
//     go/ast で解析し、`provider.ProviderID` / `provider.ProviderClaude` /
//     `provider.ProviderCodex` / `provider.ProviderCursor` /
//     `provider.ProviderCursorSDK` の SelectorExpr が一切現れないことを機械的に
//     確認する。AST はコメント (例: spec_loop.go の `provider.ProviderID 参照を一切
//     実装しない` ドキュメント文) を expression として走査しないため、誤検出の懸念は
//     無い。本 assertion は今後誰かが adapter / runner 本体に provider 名分岐
//     (`switch provider == ProviderClaude` 等) を入れた瞬間に fail する regression
//     lock を兼ねる (Req 4.3 防衛線)。
//
//   - sub: persona-resolution-mockable-via-override (Req 5.2)
//     `<cwd>/.rilll-config/facets/personas/spec-analyst.md` に sentinel content を
//     置いた状態で runKiroSpecLoop を駆動し、spec-init / spec-requirements (両 phase
//     とも persona facet = "spec-analyst") の session A 初回 skill prompt 先頭が
//     `# Persona\n\n<override-body>\n\n---\n\n` (= override 経由で解決された content)
//     で始まることを確認する。これにより Persona 解決経路が test 側から差し替え可能な
//     seam (`facet.ResolveFacet` の override → embed 2 層解決の上位レイヤ) であること
//     が観察可能に保たれる。
//
// seam の注意:
//   - identical-prompt-across-providers では session A 初回 skill prompt
//     (Kind=="skill" / ResumeSessionID=="" / StepName が "-answer" 末尾でない) を
//     phase ごとに 1 件抽出する。rounds=0 (has_inquiry=false 即完走) 前提なので、各 phase
//     1 件のみ発火する (task 6.4 と同形)。
//   - identical-prompt-across-providers では各サブテストで `setupFeatureCWD`
//     (= `t.TempDir`) を呼ぶため、cwd は分離される。embed 経路で persona facet が解決
//     されるため、解決された content も 3 サブテストで一致し、結果 prompt も byte-identical
//     になることが本テストの観察対象。
func TestKiroSpecLoopProviderCrossInvariants(t *testing.T) {
	// ------------------------------------------------------------------
	// sub: identical-prompt-across-providers (Req 1.5, 4.1, 4.2)
	// ------------------------------------------------------------------
	t.Run("identical-prompt-across-providers", func(t *testing.T) {
		orderedPhases := []string{
			"spec-init", "spec-requirements", "validate-gap",
			"spec-design", "validate-design", "spec-tasks",
		}
		// phase ごとに distinct な summary を返し、N-1 → N の chain を曖昧さなく観察する。
		// 同一の summary 値を 3 provider すべてで返すことで、prompt の `# Previous Response`
		// wiring (Options.PreviousResponseContent) も 3 provider で一致することを担保する。
		summaryByPhase := map[string]string{
			"spec-init":         "S1-spec-init-summary",
			"spec-requirements": "S2-spec-requirements-summary",
			"validate-gap":      "S3-validate-gap-summary",
			"spec-design":       "S4-spec-design-summary",
			"validate-design":   "S5-validate-design-summary",
			"spec-tasks":        "S6-spec-tasks-summary",
		}

		// providers: SystemInfo.ProviderID を変えるだけで本 mock の振る舞い (prompt 観察) は
		// 一切変えない。これにより「kiro 配線層が provider 名で分岐していない」ことが
		// prompt の一致性から直接観察可能になる。
		providerCases := []struct {
			name string
			pid  provider.ProviderID
		}{
			{name: "claude", pid: provider.ProviderClaude},
			{name: "codex", pid: provider.ProviderCodex},
			{name: "cursor", pid: provider.ProviderCursor},
		}

		// phase ごとに provider→prompt の map を作って、最後に 3 provider 間で一致するかを
		// 1 ループで verify する。
		//
		// 各 provider サブテストは `setupFeatureCWD` (= `t.TempDir`) で独立した cwd を持つため、
		// prompt 中の brief.md 絶対パス (spec-init で参照ブロックに乗る) は provider 間で
		// 異なる文字列になる。これは provider 名による分岐とは無関係な「cwd の差」なので、
		// 観測値からは cwd 部分を sentinel (`<CWD>`) に正規化してから一致比較する。
		// 正規化後の文字列は (i) persona prefix / (ii) slash command / (iii) brief.md
		// 参照ブロック (sentinel 化後) / (iv) runnerDirective() のいずれも provider 名に
		// 依存しないため、3 provider 間で byte-identical になる契約。
		promptByProviderByPhase := map[string]map[string]string{}
		for _, p := range orderedPhases {
			promptByProviderByPhase[p] = map[string]string{}
		}

		for _, pc := range providerCases {
			pc := pc
			t.Run(pc.name, func(t *testing.T) {
				feature := "demo"
				cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
				// 後で cwd を sentinel `<CWD>` に置換するため、絶対パスにも展開しておく
				// (setupFeatureCWD は `t.TempDir` を返すが、`os.TempDir` 由来の path が
				// macOS では `/private/var/...` と evaluated されているケースに備える)。
				cwdAbs := cwd
				if abs, err := filepath.Abs(cwd); err == nil {
					cwdAbs = abs
				}
				onEvent, _ := collectEvents()
				ask := &fakeAsk{Selected: feature}

				agent := &scriptedAgent{}
				agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
					switch call.Kind {
					case "commit":
						// commit 応答にも provider id を載せる (kiro 配線層が
						// SystemInfo.ProviderID で分岐しないことの観察値担保)。
						resp := commitOKResp()
						resp.SystemInfo = &provider.AgentSystemInfo{ProviderID: pc.pid}
						return resp, nil
					case "skill":
						// どの phase の skill 呼び出しか option.StepName ("<NN>-<phase>") から識別する。
						phase := ""
						if call.Options != nil {
							for _, p := range orderedPhases {
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
							// provider mock の switch 観察点: SystemInfo.ProviderID を
							// Claude / Codex / Cursor の 3 種類に切り替えるが、
							// 本 mock が観察する prompt 自体は provider 名に依らず
							// 同一であることを上位テストで verify する。
							SystemInfo: &provider.AgentSystemInfo{ProviderID: pc.pid},
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

				// session A 初回 skill 呼び出しを phase ごとに 1 件抽出する。
				// 条件: Kind="skill" / ResumeSessionID="" (初回) / StepName が "-answer" suffix ではない。
				// rounds=0 (has_inquiry=false 即完走) 前提のため各 phase 1 件のみ発火する。
				// cwd を sentinel `<CWD>` に置換することで brief.md 絶対パス (spec-init 参照
				// ブロックに含まれる) の差を吸収する。これは「provider 名に依存しない可変要素」
				// (= cwd 由来の差) を意図的に剥がす正規化であり、provider 名による分岐が
				// 入った瞬間は normalize 後でも prompt が分裂するため検出される。
				normalize := func(s string) string {
					s = strings.ReplaceAll(s, cwdAbs, "<CWD>")
					if cwdAbs != cwd {
						s = strings.ReplaceAll(s, cwd, "<CWD>")
					}
					return s
				}
				for _, c := range agent.Calls {
					if c.Kind != "skill" || c.ResumeSessionID != "" || c.Options == nil {
						continue
					}
					if strings.HasSuffix(c.Options.StepName, "-answer") {
						continue
					}
					for _, p := range orderedPhases {
						if strings.HasSuffix(c.Options.StepName, "-"+p) {
							promptByProviderByPhase[p][pc.name] = normalize(c.Prompt)
							break
						}
					}
				}
				for _, p := range orderedPhases {
					if _, ok := promptByProviderByPhase[p][pc.name]; !ok {
						t.Fatalf("provider=%s: phase %q の session A 初回 skill 呼び出しが未発火 (calls=%v)",
							pc.name, p, summarizeCalls(agent.Calls))
					}
				}
			})
		}

		// 3 provider 間で prompt が byte-identical であることを phase ごとに verify する。
		// 基準は claude (providerCases[0]) の prompt とし、codex / cursor との差分を report する。
		// Req 1.5, 4.1, 4.2 の核心観測: 「kiro 配線層は provider 名で分岐していない」。
		const base = "claude"
		others := []string{"codex", "cursor"}
		for _, phase := range orderedPhases {
			basePrompt, ok := promptByProviderByPhase[phase][base]
			if !ok {
				t.Fatalf("基準 provider %q の phase %q prompt が観測できていない", base, phase)
			}
			for _, oname := range others {
				other, ok := promptByProviderByPhase[phase][oname]
				if !ok {
					t.Fatalf("provider %q の phase %q prompt が観測できていない", oname, phase)
				}
				if other != basePrompt {
					t.Errorf("phase %q: provider %q と %q の prompt が一致しない (Req 1.5 / 4.1 / 4.2 — kiro 配線層は provider 名で分岐してはならない)\n base(%s)=%q\nother(%s)=%q",
						phase, base, oname, base, basePrompt, oname, other)
				}
			}
		}
	})

	// ------------------------------------------------------------------
	// sub: no-provider-symbol-in-adapter-and-runinquiryloop (Req 4.3)
	// ------------------------------------------------------------------
	//
	// 観察対象:
	//   - `agentCallerAdapter.Call` (spec_loop.go) と
	//     `(*specLoopRunner).runInquiryLoop` (spec_loop_runner.go) の FuncDecl.Body 内に、
	//     `provider.ProviderID` / `provider.ProviderClaude` / `provider.ProviderCodex` /
	//     `provider.ProviderCursor` / `provider.ProviderCursorSDK` の SelectorExpr が
	//     一切現れないこと。
	//
	// 検査手段: go/parser.ParseFile で AST を取得し、ast.Inspect で対象 FuncDecl.Body のみを
	// 走査して `*ast.SelectorExpr` の `<X>.<Sel>` を `<X>=ident("provider")` /
	// `<Sel>=ident("ProviderID" / "ProviderClaude" / "ProviderCodex" / "ProviderCursor" /
	// "ProviderCursorSDK")` の組合せでチェックする。
	//
	// 補足: AST はコメント (例: spec_loop.go L1097 周辺の「`provider.ProviderID 参照を一切
	// 実装しない`」というドキュメント文) を expression として持たないため、コメント中の
	// 文字列は自動で除外される (= 本 assertion は誤検出しない)。
	t.Run("no-provider-symbol-in-adapter-and-runinquiryloop", func(t *testing.T) {
		// 検査対象 (file, recv 型名, 関数名) の組合せ。
		//
		// agentCallerAdapter.Call は receiver が `agentCallerAdapter` (value receiver) なので
		// recv は型名のみ。runInquiryLoop は `*specLoopRunner` のポインタレシーバなので
		// recv 名にも `*` を含めて識別する。
		targets := []struct {
			file string
			recv string
			fn   string
		}{
			{
				file: "spec_loop.go",
				recv: "agentCallerAdapter",
				fn:   "Call",
			},
			{
				file: "spec_loop_runner.go",
				recv: "specLoopRunner",
				fn:   "runInquiryLoop",
			},
		}

		// 機械検査: 本 test ファイルが置かれているディレクトリ (= internal/workflow/builtin/kiro/)
		// は go test の cwd と一致するため、相対パスで file を読める。
		bannedSelectors := []string{
			"ProviderID",
			"ProviderClaude",
			"ProviderCodex",
			"ProviderCursor",
			"ProviderCursorSDK",
		}

		for _, tg := range targets {
			tg := tg
			t.Run(tg.recv+"."+tg.fn, func(t *testing.T) {
				fset := token.NewFileSet()
				file, err := parser.ParseFile(fset, tg.file, nil, parser.ParseComments)
				if err != nil {
					t.Fatalf("parser.ParseFile(%s): %v", tg.file, err)
				}

				var body *ast.BlockStmt
				for _, decl := range file.Decls {
					fd, ok := decl.(*ast.FuncDecl)
					if !ok {
						continue
					}
					if fd.Name == nil || fd.Name.Name != tg.fn {
						continue
					}
					// receiver 型名を取り出す (pointer / value 両対応)
					recvName := ""
					if fd.Recv != nil && len(fd.Recv.List) > 0 {
						switch te := fd.Recv.List[0].Type.(type) {
						case *ast.Ident:
							recvName = te.Name
						case *ast.StarExpr:
							if id, ok := te.X.(*ast.Ident); ok {
								recvName = id.Name
							}
						}
					}
					if recvName != tg.recv {
						continue
					}
					body = fd.Body
					break
				}
				if body == nil {
					t.Fatalf("FuncDecl が見つからない: file=%s recv=%s fn=%s",
						tg.file, tg.recv, tg.fn)
				}

				// FuncDecl.Body のみを ast.Inspect で走査し、provider パッケージの
				// 5 種の禁制シンボル (ProviderID / ProviderClaude / ProviderCodex /
				// ProviderCursor / ProviderCursorSDK) を SelectorExpr ベースで検出する。
				var violations []string
				ast.Inspect(body, func(n ast.Node) bool {
					sel, ok := n.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					xIdent, ok := sel.X.(*ast.Ident)
					if !ok {
						return true
					}
					if xIdent.Name != "provider" {
						return true
					}
					if sel.Sel == nil {
						return true
					}
					for _, banned := range bannedSelectors {
						if sel.Sel.Name == banned {
							pos := fset.Position(sel.Pos())
							violations = append(violations, fmt.Sprintf("provider.%s @ %s:%d:%d",
								banned, pos.Filename, pos.Line, pos.Column))
						}
					}
					return true
				})

				if len(violations) != 0 {
					t.Errorf("%s.%s 関数本体に provider 名分岐シンボルが現れている (Req 4.3 — provider 名で分岐したロジックを持たない):\n  %s",
						tg.recv, tg.fn, strings.Join(violations, "\n  "))
				}
			})
		}
	})

	// ------------------------------------------------------------------
	// sub: persona-resolution-mockable-via-override (Req 5.2)
	// ------------------------------------------------------------------
	//
	// 観察対象:
	//   - `<cwd>/.rilll-config/facets/personas/spec-analyst.md` に sentinel content を
	//     置いた状態で runKiroSpecLoop を駆動し、spec-init / spec-requirements
	//     (両 phase とも persona facet = "spec-analyst") の session A 初回 skill prompt 先頭が
	//     `# Persona\n\n<override-body>\n\n---\n\n` で始まる。
	//   - override 経由で解決された content が embed builtin より優先される
	//     (`facet.ResolveFacet` の 2 層解決経路は task 1.4 で verify 済) ことを、
	//     spec_loop 配線層から観察可能であることを再確認する。
	//
	// 期待値の組み立ては `facet.ResolveFacet(cwd, facet.Persona, "spec-analyst")` を test 側で
	// 再度呼び、戻り値の Content (= override body) を期待値に使う。これにより test 側と
	// production 側で同じ resolution 経路を踏み、検証経路が分裂しないようにする。
	t.Run("persona-resolution-mockable-via-override", func(t *testing.T) {
		feature := "demo"
		cwd := setupFeatureCWD(t, feature, "Brief 本文", "")
		onEvent, _ := collectEvents()
		ask := &fakeAsk{Selected: feature}

		// override fixture: spec-analyst persona を test sentinel に差し替える。
		// embed 側 marker (`# Persona: spec-analyst`) と区別がつく専用 sentinel を使い、
		// 「override が確実に勝った」ことを陰性確認できる体裁にしておく。
		const overrideBody = "# override sentinel A: spec-analyst\nproject override body for kiro-spec-loop-facets task 6.7 mock seam.\n"
		overrideDir := filepath.Join(cwd, ".rilll-config", "facets", "personas")
		if err := os.MkdirAll(overrideDir, 0o755); err != nil {
			t.Fatalf("MkdirAll override dir: %v", err)
		}
		overridePath := filepath.Join(overrideDir, "spec-analyst.md")
		if err := os.WriteFile(overridePath, []byte(overrideBody), 0o644); err != nil {
			t.Fatalf("WriteFile override: %v", err)
		}

		// 期待値の組み立ては production 側と同じ facet.ResolveFacet を経由して取り、
		// 「override 経由の解決値が production prompt prefix に流れている」ことを意味的に検証する。
		rf, err := facet.ResolveFacet(cwd, facet.Persona, "spec-analyst")
		if err != nil {
			t.Fatalf("facet.ResolveFacet(spec-analyst): %v", err)
		}
		if rf.Content != overrideBody {
			t.Fatalf("override resolution に失敗: got=%q want=%q (test 前提条件; facet.ResolveFacet の 2 層解決は task 1.4 で verify 済)",
				rf.Content, overrideBody)
		}
		// slash-first 順: skill コマンドが先頭、その後に `\n\n---\n\n# Persona\n\n<override content>` が続く。
		wantPersonaBlock := "\n\n---\n\n# Persona\n\n" + rf.Content

		// embed 側 marker が prompt prefix に漏れていないことの陰性確認用 sentinel。
		// embed 由来 spec-analyst の冒頭マーカ (`# Persona: spec-analyst`) は override body には
		// 含まれない (override body 冒頭は `# override sentinel A: spec-analyst`) ため、
		// prompt prefix に embed marker が現れたら override 解決経路が崩れたことになる。
		const embedOnlyMarker = "# Persona: spec-analyst"

		agent := &scriptedAgent{}
		agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
			switch call.Kind {
			case "commit":
				return commitOKResp(), nil
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

		// spec-init / spec-requirements (両者の persona facet は "spec-analyst") の
		// session A 初回 skill 呼び出しを抽出する。
		// 条件: Kind="skill" / ResumeSessionID="" (初回) / StepName=="<NN>-<phase>" で
		//       phase ∈ {spec-init, spec-requirements}。
		analystPhases := []string{"spec-init", "spec-requirements"}
		callByPhase := map[string]recordedAgentCall{}
		for _, c := range agent.Calls {
			if c.Kind != "skill" || c.ResumeSessionID != "" || c.Options == nil {
				continue
			}
			if strings.HasSuffix(c.Options.StepName, "-answer") {
				continue
			}
			for _, p := range analystPhases {
				if strings.HasSuffix(c.Options.StepName, "-"+p) {
					callByPhase[p] = c
					break
				}
			}
		}
		if len(callByPhase) != len(analystPhases) {
			t.Fatalf("spec-analyst persona の 2 phase 分の session A 初回 skill 呼び出しを期待: got=%d (calls=%v)",
				len(callByPhase), summarizeCalls(agent.Calls))
		}

		for _, phase := range analystPhases {
			c, ok := callByPhase[phase]
			if !ok {
				t.Fatalf("phase %q: session A 初回 skill 呼び出しが未発火", phase)
			}
			// slash-first 順: prompt 先頭は `/kiro-{phase} <feature>` から始まる必要がある。
			wantSlashHead := fmt.Sprintf("/kiro-%s ", phase)
			if !strings.HasPrefix(c.Prompt, wantSlashHead) {
				head := c.Prompt
				if len(head) > 80 {
					head = head[:80]
				}
				t.Errorf("phase %q: prompt 先頭が slash command と一致しない (slash-first 順)\n got_head=%q\nwant_head_prefix=%q",
					phase, head, wantSlashHead)
			}
			// override 経由 persona block が skill コマンドの後ろに連結されていること
			if !strings.Contains(c.Prompt, wantPersonaBlock) {
				t.Errorf("phase %q: prompt に override 経由 persona block が含まれない (override fixture が Persona seam として動作するべき)\n want_block=%q\ngot_prompt_head=%q",
					phase, wantPersonaBlock, c.Prompt[:min(len(c.Prompt), 512)])
			}
			// embed 由来 marker が prompt 全体に現れていないこと
			// (= override 解決が確実に勝った陰性確認; facet_test.go と同形)
			if strings.Contains(c.Prompt, embedOnlyMarker) {
				t.Errorf("phase %q: prompt に embed builtin marker %q が漏れている (override が勝つべき):\n%s",
					phase, embedOnlyMarker, c.Prompt[:min(len(c.Prompt), 256)])
			}
		}
	})
}
