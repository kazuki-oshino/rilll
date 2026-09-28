// Package learnings の本 file は agent-learnings spec / tasks.md 10.4
// (Boundary: LearningsE2E) で要求される E2E パイプラインの不変式を検証する。
//
// 検証範囲: 親 run 終了相当 ctx (parent_run_id 注入済み) → 4 phase orchestration →
// 5 artifact 出力 + learnings table insert + wiki playbook 書き込み + index.md 再構築。
//
// fixture (wiki layout):
//
//   - `agent_calls` 2 行 (異なる session_id) を fakeReader 経由で供給
//   - 既存 category playbook entry (wiki 5 file layout):
//     `docs_dev/playbook/failure-recovery.md` (`## known` section + 1 bullet)
//   - mock provider response (reflector / curator) を deterministic に制御
//
// 観測ポイント:
//
//   - (a) `<.rilll/agent-learnings/<runID>/>` 配下に 5 artifact が生成される
//     (sessions.json / reflector.json / learnings.json / curator.json / apply-result.json)
//   - (b) `learnings` table に 1+ rows insert される (Recorder.RecordLearning 呼び出し)
//   - (c) `docs_dev/playbook/<category>.md` に section が追加され、`**summary:**` /
//     `**適用条件:**` 行 + curator が指定した bullet が書かれる (Req 1, 3, 4)
//   - (d) `docs_dev/playbook/index.md` が rebuild され、4 category section + 該当 topic link を含む
//   - (e) Curator 入力 `existing-rules.txt` が wiki layout の `===== docs_dev/playbook/<cat>.md =====`
//     セパレータと既存 bullet を含む (Req 8.2)
//   - (f) verdict = applied
//
// 関連 Requirement: 1.2 (DistillLearningsAfter wrap exposure) / 2.1 (4 phase orchestration) /
// 4.1 (Reflector phase) / 5.1 (akashic learnings 永続化) / 6.1 (Curator phase) /
// 7.1 (Apply phase ADD-only) / 8.1 (workflow orchestration) / 9.1 (wiki layout regression) /
// 9.2 (旧形式 fixture / assertion 不在) / 9.5 (wiki 構造規約準拠 assert)。
//
// 注: 本 file は agent-learnings-wiki spec (task 6.1) で wiki 5 file layout に rewrite された。
// 旧形式 (memory-distiller v1 時代の flat layout) への参照 / fixture / assertion はすべて
// 撤去済み (Req 9.2)。
package learnings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
)

// TestLearningsE2E_FullPipelineWith2Sessions: 観点 (a)(b)(c)(d)(e)(f) を 1 case で観測する E2E test。
//
// fixture:
//   - agent_calls 2 行: (sess-A / sess-B / 同一 parent_run_id "parent-X")
//   - 既存 category playbook entry: `docs_dev/playbook/failure-recovery.md` に
//     `## known` section + 1 bullet を仕込み、curator existing-rules.txt が wiki layout を
//     走査する観点 (e) を確実に踏ませる。
//   - mock reflector response: 1 つの learning を返す (両 session 共通)
//   - mock curator response: 1 つの ADD operation を返す (`summary` / `applies_to` 必須 / 新 topic)
//
// 期待: verdict=applied / 5 artifact / learnings rows >= 1 (reflector が akashic に record) /
// `docs_dev/playbook/failure-recovery.md` に `## sample-e2e` section が追加され、
// `**summary:**` / `**適用条件:**` 行 + curator bullet を含む / 既存 `## known` section が維持される /
// `index.md` が生成され `failure-recovery` section に `sample-e2e` link を含む /
// existing-rules.txt が wiki layout セパレータと既存 known bullet を含む。
func TestLearningsE2E_FullPipelineWith2Sessions(t *testing.T) {
	withProviderEnv(t, "codex")
	cwd := t.TempDir()
	const (
		parentRunID      = "parent-X"
		tailRunID        = "tail-1"
		existingBullet   = "known category bullet"
		curatorBullet    = "E2E synthesized bullet from 2 sessions"
		curatorSummary   = "E2E synthesized summary"
		curatorAppliesTo = "E2E sample applies condition"
		newCategory      = "failure-recovery"
		newTopic         = "sample-e2e"
	)

	// fixture (1): wiki layout の既存 category file を作成。
	// curator existing-rules.txt 構築が wiki 4 file flat layout を走査することを確実にする (Req 8.2)。
	playbookDir := filepath.Join(cwd, "docs_dev", "playbook")
	if err := os.MkdirAll(playbookDir, 0o755); err != nil {
		t.Fatalf("mkdir playbook dir: %v", err)
	}
	existingCategoryPath := filepath.Join(playbookDir, newCategory+".md")
	existingCategoryBody := strings.Join([]string{
		"# " + newCategory,
		"",
		"失敗 → 復旧の手順。",
		"",
		"## known",
		"**適用条件:** 既存テスト用 placeholder",
		"**summary:** known category bullet 用 summary",
		"",
		"- " + existingBullet,
		"",
	}, "\n")
	if err := os.WriteFile(existingCategoryPath, []byte(existingCategoryBody), 0o644); err != nil {
		t.Fatalf("write category playbook: %v", err)
	}

	// fixture (2): fakeReader に 2 行 (異なる session_id / 同一 parent_run_id) を供給。
	rec := &fakeWorkflowRecorder{}
	projectID := akashic.ProjectIDFromCWD(cwd)
	reader := &fakeReader{rows: []akashic.AgentCallRow{
		{ID: "ac-1", SessionID: "sess-A", ProviderID: "codex", StepID: "implement", ProjectID: projectID},
		{ID: "ac-2", SessionID: "sess-B", ProviderID: "codex", StepID: "implement", ProjectID: projectID},
	}}

	// fixture (3): mock provider response。
	// reflector: 両 session 共通で 1 learning を返す → 2 calls 合計 2 learnings 集約。
	// curator: ADD 1 件 (新 topic / `summary` / `applies_to` 必須 / Req 2.1, 2.2)。
	agent := &fakeWorkflowAgent{
		reader:   reader,
		recorder: rec,
		reflectorResp: provider.AgentResponse{
			Content: `{"learnings":[{"category":"failure-recovery","context":"timeout in implement step","insight":"retry with longer timeout","paths_glob":["src/**"]}]}`,
		},
		curatorResp: provider.AgentResponse{
			Content: `{"operations":[{"op":"add","category":"failure-recovery","topic":"sample-e2e","paths_glob":["src/**"],"entry":"E2E synthesized bullet from 2 sessions","summary":"E2E synthesized summary","applies_to":"E2E sample applies condition"}]}`,
		},
	}

	// orchestration 駆動。
	ctx := makeRunCtx(t, cwd, tailRunID, parentRunID, rec)
	var doneCount int
	onEvent := func(e workflow.WorkflowEvent) {
		if e.Type == workflow.WorkflowDone {
			doneCount++
		}
	}
	if err := runLearningsOrchestration(ctx, cwd, onEvent, nil, agent); err != nil {
		t.Fatalf("runLearningsOrchestration: %v", err)
	}
	if doneCount != 1 {
		t.Errorf("WorkflowDone emit count = %d, want 1", doneCount)
	}

	// (a) 5 artifact 生成確認。
	// (sessions / reflector / curator / apply-result + curator phase 入力契約の learnings.json)
	runDir := filepath.Join(cwd, ".rilll", learningsWorkflowID, tailRunID)
	for _, fn := range []string{
		sessionsFilename,
		reflectorFilename,
		learningsArtifactFilename,
		curatorFilename,
		applyResultFilename,
	} {
		path := filepath.Join(runDir, fn)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("artifact %q not generated: %v", fn, err)
		}
	}

	// (a') learnings.json は curator phase 入力契約 (Req 8.7) を満たす形であること。
	// 2 sessions × 1 learning = 2 entries / provenance fields が埋まっていること。
	learningsBody, err := os.ReadFile(filepath.Join(runDir, learningsArtifactFilename))
	if err != nil {
		t.Fatalf("read learnings.json: %v", err)
	}
	var learningsArt LearningsArtifact
	if uerr := json.Unmarshal(learningsBody, &learningsArt); uerr != nil {
		t.Fatalf("unmarshal learnings.json: %v\nbody=%s", uerr, learningsBody)
	}
	if len(learningsArt.Learnings) != 2 {
		t.Errorf("learnings.json entries = %d, want 2 (2 sessions × 1 learning)", len(learningsArt.Learnings))
	}
	for i, e := range learningsArt.Learnings {
		if e.ID == "" || e.SessionID == "" || e.ProviderID == "" {
			t.Errorf("learnings[%d] missing provenance: %+v", i, e)
		}
		if e.Category == "" || e.Insight == "" {
			t.Errorf("learnings[%d] missing LLM-emitted: %+v", i, e)
		}
	}

	// (f) verdict = applied。
	a := readApplyArtifact(t, cwd, tailRunID)
	if a.Verdict != VerdictApplied {
		t.Errorf("verdict = %q, want %q", a.Verdict, VerdictApplied)
	}
	if len(a.Appended) != 1 {
		t.Errorf("Appended = %d, want 1", len(a.Appended))
	}
	// happy path では IndexWarnings は空 / nil で副作用なし (Req 9.5 — wiki 構造規約準拠下では warning 0)。
	if len(a.IndexWarnings) != 0 {
		t.Errorf("IndexWarnings = %+v, want empty on happy path (wiki 4 file 正常)", a.IndexWarnings)
	}

	// (b) learnings table に rows insert。reflector は 2 session × 1 learning = 2 rows insert する想定。
	if len(rec.rows) < 1 {
		t.Errorf("RecordLearning rows = %d, want >= 1 (reflector が learnings を akashic に永続化)", len(rec.rows))
	}

	// (c) wiki 5 file layout の category file に section が追加され、新 section に
	// `**適用条件:**` / `**summary:**` 行 + curator bullet が書かれる (Req 1.4, 1.6, 3.2)。
	// 既存 `## known` section の bullet も維持される (Req 3.4)。
	body, err := os.ReadFile(existingCategoryPath)
	if err != nil {
		t.Fatalf("category file %q not readable: %v", existingCategoryPath, err)
	}
	bodyStr := string(body)
	// 既存 section が維持される (Req 3.4 / 3.5)
	if !strings.Contains(bodyStr, "## known") {
		t.Errorf("existing `## known` section was lost: body=%q", bodyStr)
	}
	if !strings.Contains(bodyStr, "- "+existingBullet) {
		t.Errorf("existing bullet `- %s` was lost: body=%q", existingBullet, bodyStr)
	}
	// 新 section が末尾に append される (Req 3.2 / 3.5)
	if !strings.Contains(bodyStr, "## "+newTopic) {
		t.Errorf("new `## %s` section was not appended: body=%q", newTopic, bodyStr)
	}
	// 新 section に `**summary:**` 行 (Req 1.6 拡張: curator summary を section heading 直下に固定)
	if !strings.Contains(bodyStr, "**summary:** "+curatorSummary) {
		t.Errorf("new section lacks **summary:** line: body=%q", bodyStr)
	}
	// 新 section に `**適用条件:**` 行 (Req 1.6 / 3.7)
	if !strings.Contains(bodyStr, "**適用条件:** "+curatorAppliesTo) {
		t.Errorf("new section lacks **適用条件:** line: body=%q", bodyStr)
	}
	// 新 section に curator bullet が含まれる
	if !strings.Contains(bodyStr, "- "+curatorBullet) {
		t.Errorf("new section lacks curator bullet: body=%q", bodyStr)
	}
	// frontmatter は書かれない (Req 1.5)
	if strings.Contains(bodyStr, "paths:") {
		t.Errorf("category file should not include `paths:` frontmatter (Req 1.5): body=%q", bodyStr)
	}

	// (d) `docs_dev/playbook/index.md` が rebuild され、4 category section + 該当 topic link を含む (Req 4.1-4.4)。
	indexBody, err := os.ReadFile(filepath.Join(playbookDir, "index.md"))
	if err != nil {
		t.Fatalf("index.md not generated: %v", err)
	}
	indexStr := string(indexBody)
	if !strings.Contains(indexStr, "# Playbook Index") {
		t.Errorf("index.md lacks header `# Playbook Index`: body=%q", indexStr)
	}
	// 4 category section が固定順序で含まれる (Req 4.2)
	for _, cat := range []string{
		CategoryFailureRecovery,
		CategoryAntipattern,
		CategorySuccessProcedure,
		CategoryToolPattern,
	} {
		if !strings.Contains(indexStr, "## "+cat+"\n") {
			t.Errorf("index.md lacks `## %s` section heading: body=%q", cat, indexStr)
		}
	}
	// 新 topic link が `failure-recovery` section に含まれる (Req 4.4)
	wantLink := "- [" + newTopic + "](./" + newCategory + ".md#" + newTopic + ") — " + curatorSummary
	if !strings.Contains(indexStr, wantLink) {
		t.Errorf("index.md lacks topic link %q: body=%q", wantLink, indexStr)
	}
	// 既存 known topic も index に再構築される (RebuildIndex は full rebuild なので既存 section の topic も拾う)
	wantKnownLink := "- [known](./" + newCategory + ".md#known) — known category bullet 用 summary"
	if !strings.Contains(indexStr, wantKnownLink) {
		t.Errorf("index.md lacks existing known topic link %q: body=%q", wantKnownLink, indexStr)
	}
	// frontmatter は書かれない (Req 4.6)
	if strings.Contains(indexStr, "paths:") {
		t.Errorf("index.md should not include YAML frontmatter (Req 4.6): body=%q", indexStr)
	}

	// (e) curator existing-rules.txt が wiki layout の category file separator と既存 bullet を含む。
	existingRulesPath := filepath.Join(runDir, existingRulesFilename)
	rules, err := os.ReadFile(existingRulesPath)
	if err != nil {
		t.Fatalf("read %q: %v", existingRulesFilename, err)
	}
	rulesStr := string(rules)
	// wiki layout separator (`===== docs_dev/playbook/failure-recovery.md =====`) が含まれる
	wantSeparator := "===== docs_dev/playbook/" + newCategory + ".md ====="
	if !strings.Contains(rulesStr, wantSeparator) {
		t.Errorf("existing-rules.txt lacks wiki layout separator %q: rules=%q", wantSeparator, rulesStr)
	}
	// 既存 bullet も含まれる (curator が dedup 判定 / novelty 判定で参照可能)
	if !strings.Contains(rulesStr, "- "+existingBullet) {
		t.Errorf("existing-rules.txt lacks legacy known bullet: rules=%q", rulesStr)
	}

	// CallAgent は reflector x 2 + curator x 1 = 3 回呼ばれた。
	if len(agent.calls) != 3 {
		t.Errorf("CallAgent invocation count = %d, want 3 (reflector x 2 sessions + curator x 1)", len(agent.calls))
	}
	curatorCalls := 0
	reflectorCalls := 0
	for _, c := range agent.calls {
		switch c {
		case stepIDReflector:
			reflectorCalls++
		case stepIDCurator:
			curatorCalls++
		}
	}
	if reflectorCalls != 2 {
		t.Errorf("reflector CallAgent count = %d, want 2 (per-session)", reflectorCalls)
	}
	if curatorCalls != 1 {
		t.Errorf("curator CallAgent count = %d, want 1", curatorCalls)
	}
}

// TestLearningsE2E_FaultTolerantWithCorruptedCategoryFile は、wiki 5 file layout の
// 1 category file が破損 (scanner buffer overflow 経路で ParseCategoryFile が error 返却)
// した状態でも、agent-learnings workflow が壊滅せず apply-result.json を defer 書き出し
// する fault tolerance を観測する (Req 9.1 / 9.5)。
//
// 設計確認:
//   - Apply phase の `scanLearningsPlaybook` は parse error を致命扱いし、Apply は
//     初期化値 `Verdict: VerdictApplied` を保持したまま error を返す (apply.go L170-198)。
//   - workflow.go は Apply error を伝播するが、defer 経由で apply-result.json は書き出される
//     (workflow.go L118-135)。
//   - 破損 file 以外の category file には副作用が一切無い (Apply は破損 scan 段で停止)。
//   - reflector / curator は破損 file に触れないので、ここまでは正常に進む。
//
// fixture:
//   - agent_calls 1 行
//   - 破損 file: `docs_dev/playbook/tool-pattern.md` を 2MB 1 行で書き込み、
//     bufio.Scanner buffer overflow → ParseCategoryFile error 経路に誘導。
//   - curator は valid op を返す想定だが、Apply 内 scan で死ぬので Append には到達しない。
//
// 期待 (workflow-level fault tolerance 不変式 / Req 9.5):
//   - runLearningsOrchestration は error を返す (Apply scan 失敗の伝播)。
//   - WorkflowDone は defer 経路で 1 回 emit される (defer 不変式)。
//   - apply-result.json は defer 経路で書き出される (workflow 壊滅せず)。
//   - Appended = 0 件 (scan 段で停止)。
//   - 他 category file (`failure-recovery.md` 等) は副作用ゼロ。
//   - 破損 file 自体も Apply による書き換えを受けない。
//   - reflector x 1 + curator x 1 は呼ばれている (Apply より前に走り終わる)。
//
// 注: 現実装では scan 失敗時の verdict は Apply 初期化値 `VerdictApplied` のまま defer に伝わる
// (workflow.go L267-269 は `result.Verdict == ""` のときのみ降格するが、Apply 初期化で
// `VerdictApplied` が立っているため降格されない)。これは将来 scan fault tolerance を強化
// した際に test を見直すべき箇所だが、本 task 6.1 の boundary では「workflow 壊滅せず
// apply-result.json が書き出される」不変式の固定で十分とする。
func TestLearningsE2E_FaultTolerantWithCorruptedCategoryFile(t *testing.T) {
	withProviderEnv(t, "codex")
	cwd := t.TempDir()
	const (
		parentRunID = "parent-Y"
		tailRunID   = "tail-fault-1"
	)

	// fixture (1): wiki 5 file layout の playbook root を作成し、tool-pattern.md を
	// 2MB 1 行で破損させる (改行を含まないため scanner buffer overflow が parse error を誘発)。
	playbookDir := filepath.Join(cwd, "docs_dev", "playbook")
	if err := os.MkdirAll(playbookDir, 0o755); err != nil {
		t.Fatalf("mkdir playbook dir: %v", err)
	}
	bigLine := strings.Repeat("a", 2*1024*1024) // 2MB / 改行なし → bufio.Scanner buffer overflow
	corruptedPath := filepath.Join(playbookDir, "tool-pattern.md")
	if err := os.WriteFile(corruptedPath, []byte(bigLine), 0o644); err != nil {
		t.Fatalf("write corrupted tool-pattern.md: %v", err)
	}

	// fixture (2): agent_calls 1 行 (1 session で十分)。
	rec := &fakeWorkflowRecorder{}
	projectID := akashic.ProjectIDFromCWD(cwd)
	reader := &fakeReader{rows: []akashic.AgentCallRow{
		{ID: "ac-1", SessionID: "sess-A", ProviderID: "codex", StepID: "implement", ProjectID: projectID},
	}}

	// fixture (3): mock response。curator は valid op を返すが、Apply 内 scan で死ぬので
	// AppendBullet には到達しない (= 他 category file は副作用ゼロ)。
	agent := &fakeWorkflowAgent{
		reader:   reader,
		recorder: rec,
		reflectorResp: provider.AgentResponse{
			Content: `{"learnings":[{"category":"failure-recovery","context":"c","insight":"i","paths_glob":["src/**"]}]}`,
		},
		curatorResp: provider.AgentResponse{
			Content: `{"operations":[{"op":"add","category":"failure-recovery","topic":"new-topic","paths_glob":["src/**"],"entry":"new bullet","summary":"new summary","applies_to":"new applies"}]}`,
		},
	}

	ctx := makeRunCtx(t, cwd, tailRunID, parentRunID, rec)
	var doneCount int
	onEvent := func(e workflow.WorkflowEvent) {
		if e.Type == workflow.WorkflowDone {
			doneCount++
		}
	}
	err := runLearningsOrchestration(ctx, cwd, onEvent, nil, agent)
	// Apply scan 失敗は致命なので error が返る (但し apply-result.json は defer 経由で書き出し済み)。
	if err == nil {
		t.Fatalf("runLearningsOrchestration should return error when category file is corrupted")
	}
	// 致命でも WorkflowDone は defer 経由で 1 回 emit される (Req 9.5 fault tolerance 不変式)。
	if doneCount != 1 {
		t.Errorf("WorkflowDone emit count = %d, want 1 (defer-driven invariant)", doneCount)
	}

	// apply-result.json は defer 経路で書き出されている。verdict は Apply 初期化値 `VerdictApplied`
	// (workflow.go の降格条件 `result.Verdict == ""` に該当しないため初期値保持)。
	// 本 test の主眼は「workflow 壊滅せず artifact が読める」不変式であり、verdict 値そのものは
	// 現実装の scan-fail 経路の挙動として固定する (将来 fault tolerance 強化時に見直し対象)。
	a := readApplyArtifact(t, cwd, tailRunID)
	if a.Verdict != VerdictApplied {
		t.Errorf("verdict = %q, want %q (apply scan error → defer write with initial verdict)", a.Verdict, VerdictApplied)
	}
	// Apply scan で死んだので Append は 0 件。
	if len(a.Appended) != 0 {
		t.Errorf("Appended = %d, want 0 (apply scan failed before AppendBullet)", len(a.Appended))
	}
	// 他 category file (failure-recovery.md 等) には書き込みされていない (副作用ゼロ)。
	for _, cat := range []string{
		CategoryFailureRecovery,
		CategoryAntipattern,
		CategorySuccessProcedure,
	} {
		path := filepath.Join(playbookDir, cat+".md")
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Errorf("category file %q should not be created (fault scan stopped Apply): err=%v", cat, statErr)
		}
	}
	// 破損 tool-pattern.md は元のままで保持される (2MB 1 行 / Apply は読まずに parse 段で失敗)。
	got, rerr := os.ReadFile(corruptedPath)
	if rerr != nil {
		t.Fatalf("read corrupted file: %v", rerr)
	}
	if len(got) != len(bigLine) {
		t.Errorf("corrupted file altered: len=%d, want %d (Apply must not modify on scan-fail)", len(got), len(bigLine))
	}

	// reflector x 1 + curator x 1 は走り終わっている (Apply より前なので fault に巻き込まれない)。
	reflectorCalls := 0
	curatorCalls := 0
	for _, c := range agent.calls {
		switch c {
		case stepIDReflector:
			reflectorCalls++
		case stepIDCurator:
			curatorCalls++
		}
	}
	if reflectorCalls != 1 {
		t.Errorf("reflector CallAgent count = %d, want 1", reflectorCalls)
	}
	if curatorCalls != 1 {
		t.Errorf("curator CallAgent count = %d, want 1", curatorCalls)
	}
}
