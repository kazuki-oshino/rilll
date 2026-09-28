// Package learnings の本 file は agent-learnings curator phase の単体テスト群を保持する
// (tasks.md 7.3 / Req 6.1, 6.2, 6.5, 8.2, 8.4, 9.3, 9.5)。
//
// 検証観点:
//
//   - (a) operations 0 件 → Verdict=applied-no-op で apply skip。
//   - (b) JSON parse 失敗 → Verdict=rejected-invalid-json + curator.raw.txt 保存。
//   - (c) call 失敗 → Verdict=rejected-invalid-json + raw 保存。
//   - (d) 1 件以上の operation → Verdict="" で apply phase に進む + curator.json 書き出し。
//   - (e) existing-rules.txt は `docs_dev/playbook/<category>.md` (4 file flat) を
//     scan して concat する (Req 8.2)。旧 `.claude/rules/playbook/` は scan しない。
//   - (f) 256 KiB 超過 → head-tail-clip 戦略で truncated=true。
//   - (g) buildCuratorPrompt の slash command 組立 / override 経路。
//   - (h) Topic Index header は各 category file の `## <topic>` section から構築する。
package learnings

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// fakeCuratorAgent は workflowctx.AgentCaller を満たす test stub (curator 用)。
// CallAgent は固定 response / error を返し、最後の prompt と OutputFormat 種別を観測する。
type fakeCuratorAgent struct {
	mu       sync.Mutex
	resp     provider.AgentResponse
	err      error
	gotOpts  *workflowctx.CallAgentOptions
	gotPromt string
	gotCWD   string
}

func (f *fakeCuratorAgent) CallAgent(_ context.Context, prompt, cwd string, _ provider.AgentEventCallback, _ string, opts *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotOpts = opts
	f.gotPromt = prompt
	f.gotCWD = cwd
	return f.resp, f.err
}

// readCuratorArtifactT は test 専用 helper。curator.json を decode して返す。
func readCuratorArtifactT(t *testing.T, path string) CuratorArtifact {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read curator.json: %v", err)
	}
	var a CuratorArtifact
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatalf("unmarshal curator.json: %v\nbody = %s", err, body)
	}
	return a
}

// writeWikiCategoryFile は test 用 helper。`docs_dev/playbook/<category>.md` に
// `# <category>` + 概要 + 1 つ以上の `## <topic>` section を含む wiki 形式 file を書き出す。
//
// 入力 sections は (topic-slug, bullet-text) の対の列。順序は配列順に追加される。
// 各 section は applies_to / summary 行を必ず持つ wiki 構造規約に準拠した形式で書く。
func writeWikiCategoryFile(t *testing.T, cwd, category string, sections [][2]string) {
	t.Helper()
	playbookDir := filepath.Join(cwd, "docs_dev", "playbook")
	if err := os.MkdirAll(playbookDir, 0o755); err != nil {
		t.Fatalf("mkdir docs_dev/playbook: %v", err)
	}
	var b strings.Builder
	// category heading + 概要 (1-3 行)。
	b.WriteString("# " + category + "\n\n")
	b.WriteString("category 概要 (test fixture)。\n")
	for _, s := range sections {
		topic, bullet := s[0], s[1]
		b.WriteString("\n## " + topic + "\n")
		b.WriteString("**適用条件:** test fixture 適用条件\n")
		b.WriteString("**summary:** test fixture summary\n\n")
		b.WriteString("- " + bullet + "\n")
	}
	path := filepath.Join(playbookDir, category+".md")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestCuratorEmptyOperationsAppliedNoOp: 観点 (a) 空 operations 配列 →
// Verdict=applied-no-op で apply phase 不発火。
func TestCuratorEmptyOperationsAppliedNoOp(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &fakeCuratorAgent{
		resp: provider.AgentResponse{Content: `{"operations":[]}`},
	}
	got, err := Curator(context.Background(), runDir, CuratorInput{
		CWD:         cwd,
		ParentRunID: "run-1",
	}, agent)
	if err != nil {
		t.Fatalf("Curator: %v", err)
	}
	if got.Verdict != VerdictAppliedNoOp {
		t.Errorf("Verdict = %q, want %q", got.Verdict, VerdictAppliedNoOp)
	}
	if len(got.Operations) != 0 {
		t.Errorf("Operations len = %d, want 0", len(got.Operations))
	}
	if got.CuratorJSONPath == "" {
		t.Errorf("CuratorJSONPath empty")
	}
	a := readCuratorArtifactT(t, got.CuratorJSONPath)
	if a.SchemaVersion != curatorSchemaVersion {
		t.Errorf("schema_version = %d, want %d", a.SchemaVersion, curatorSchemaVersion)
	}
	if len(a.Operations) != 0 {
		t.Errorf("artifact.Operations len = %d, want 0", len(a.Operations))
	}
}

// TestCuratorRejectsInvalidJSON: 観点 (b) JSON 抽出不能 → Verdict=rejected-invalid-json
// + curator.raw.txt に raw 応答が保存される。
func TestCuratorRejectsInvalidJSON(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &fakeCuratorAgent{
		resp: provider.AgentResponse{Content: `not a json at all`},
	}
	got, err := Curator(context.Background(), runDir, CuratorInput{
		CWD:         cwd,
		ParentRunID: "run-1",
	}, agent)
	if err != nil {
		t.Fatalf("Curator: %v", err)
	}
	if got.Verdict != VerdictRejectedInvalidJSON {
		t.Errorf("Verdict = %q, want %q", got.Verdict, VerdictRejectedInvalidJSON)
	}
	if got.CuratorJSONPath == "" {
		t.Fatalf("CuratorJSONPath empty")
	}
	rawPath := filepath.Join(runDir, curatorRawFilename)
	body, rerr := os.ReadFile(rawPath)
	if rerr != nil {
		t.Fatalf("read curator.raw.txt: %v", rerr)
	}
	if string(body) != `not a json at all` {
		t.Errorf("raw body = %q, want %q", string(body), `not a json at all`)
	}
}

// TestCuratorCallFailureRejectsInvalidJSON: 観点 (c) CallAgent error →
// Verdict=rejected-invalid-json (call 失敗系を JSON 不正系で吸収)。
func TestCuratorCallFailureRejectsInvalidJSON(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &fakeCuratorAgent{
		err: errors.New("network failure"),
	}
	got, err := Curator(context.Background(), runDir, CuratorInput{
		CWD:         cwd,
		ParentRunID: "run-1",
	}, agent)
	if err != nil {
		t.Fatalf("Curator: %v", err)
	}
	if got.Verdict != VerdictRejectedInvalidJSON {
		t.Errorf("Verdict = %q, want %q", got.Verdict, VerdictRejectedInvalidJSON)
	}
	rawPath := filepath.Join(runDir, curatorRawFilename)
	if _, statErr := os.Stat(rawPath); statErr != nil {
		t.Errorf("curator.raw.txt should exist: %v", statErr)
	}
}

// TestCuratorAcceptsOperations: 観点 (d) 1 件以上の operation → Verdict="" + curator.json
// に operations が書き出される。
func TestCuratorAcceptsOperations(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()

	body := `{"operations":[{"op":"add","category":"failure-recovery","topic":"foo-bar","paths_glob":["a/**"],"entry":"avoid X when Y","summary":"avoid X when Y","applies_to":"Y 状況の時"}]}`
	agent := &fakeCuratorAgent{
		resp: provider.AgentResponse{Content: body},
	}
	got, err := Curator(context.Background(), runDir, CuratorInput{
		CWD:         cwd,
		ParentRunID: "run-1",
	}, agent)
	if err != nil {
		t.Fatalf("Curator: %v", err)
	}
	if got.Verdict != "" {
		t.Errorf("Verdict = %q, want \"\" (apply phase に進む)", got.Verdict)
	}
	if len(got.Operations) != 1 {
		t.Fatalf("Operations len = %d, want 1", len(got.Operations))
	}
	op := got.Operations[0]
	if op.Op != "add" || op.Category != "failure-recovery" || op.Topic != "foo-bar" {
		t.Errorf("operation = %+v, want add/failure-recovery/foo-bar", op)
	}
	a := readCuratorArtifactT(t, got.CuratorJSONPath)
	if len(a.Operations) != 1 {
		t.Fatalf("artifact.Operations len = %d, want 1", len(a.Operations))
	}
	// JSONSchemaForCurator が opts.OutputFormat に渡されている事を観測する。
	if agent.gotOpts == nil || agent.gotOpts.OutputFormat == nil {
		t.Errorf("OutputFormat should be set")
	} else if agent.gotOpts.OutputFormat.Type != "json_schema" {
		t.Errorf("OutputFormat.Type = %q, want json_schema", agent.gotOpts.OutputFormat.Type)
	}
}

// TestCuratorStructuredOutputPath: claude bridge 経路 (resp.Content 空 / StructuredOutput 経由)。
func TestCuratorStructuredOutputPath(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()

	agent := &fakeCuratorAgent{
		resp: provider.AgentResponse{
			StructuredOutput: map[string]any{
				"operations": []any{
					map[string]any{
						"op":         "add",
						"category":   "tool-pattern",
						"topic":      "git-stash",
						"paths_glob": []any{"x/**"},
						"entry":      "stash before rebase",
						"summary":    "stash before rebase",
						"applies_to": "rebase 前",
					},
				},
			},
		},
	}
	got, err := Curator(context.Background(), runDir, CuratorInput{
		CWD:         cwd,
		ParentRunID: "run-1",
	}, agent)
	if err != nil {
		t.Fatalf("Curator: %v", err)
	}
	if got.Verdict != "" {
		t.Errorf("Verdict = %q, want \"\"", got.Verdict)
	}
	if len(got.Operations) != 1 {
		t.Fatalf("Operations len = %d, want 1", len(got.Operations))
	}
}

// TestCuratorExistingRulesScansWikiLayout: 観点 (e / Req 8.2) existing-rules.txt が
// `docs_dev/playbook/<category>.md` の 4 category file を flat scan することを観測する。
// 旧 `.claude/rules/playbook/` 配下の file は scan されないことも合わせて観測する。
func TestCuratorExistingRulesScansWikiLayout(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()

	// 新 layout: docs_dev/playbook/<category>.md を 4 category 全て作成。
	writeWikiCategoryFile(t, cwd, CategoryFailureRecovery, [][2]string{
		{"fr-topic", "failure-recovery bullet"},
	})
	writeWikiCategoryFile(t, cwd, CategoryAntipattern, [][2]string{
		{"ap-topic", "antipattern bullet"},
	})
	writeWikiCategoryFile(t, cwd, CategorySuccessProcedure, [][2]string{
		{"sp-topic", "success-procedure bullet"},
	})
	writeWikiCategoryFile(t, cwd, CategoryToolPattern, [][2]string{
		{"tp-topic", "tool-pattern bullet"},
	})

	// 旧 layout: `.claude/rules/playbook/...` に file を仕込むが、これは scan されない (Req 8.2)。
	legacyDir := filepath.Join(cwd, ".claude", "rules", "playbook", "failure-recovery")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "legacy.md"), []byte("---\npaths:\n  - x/**\n---\n- legacy bullet must not appear\n"), 0o644); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	agent := &fakeCuratorAgent{
		resp: provider.AgentResponse{Content: `{"operations":[]}`},
	}
	_, err := Curator(context.Background(), runDir, CuratorInput{
		CWD:         cwd,
		ParentRunID: "run-1",
	}, agent)
	if err != nil {
		t.Fatalf("Curator: %v", err)
	}

	existingPath := filepath.Join(runDir, existingRulesFilename)
	body, rerr := os.ReadFile(existingPath)
	if rerr != nil {
		t.Fatalf("read existing-rules.txt: %v", rerr)
	}
	content := string(body)

	// 4 category file それぞれの separator が含まれること。
	for _, cat := range []string{
		CategoryFailureRecovery,
		CategoryAntipattern,
		CategorySuccessProcedure,
		CategoryToolPattern,
	} {
		sep := "===== docs_dev/playbook/" + cat + ".md ====="
		if !strings.Contains(content, sep) {
			t.Errorf("existing-rules.txt missing separator for %s: not found %q", cat, sep)
		}
	}
	// 各 category の bullet 本文が含まれること。
	for _, want := range []string{
		"failure-recovery bullet",
		"antipattern bullet",
		"success-procedure bullet",
		"tool-pattern bullet",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("existing-rules.txt missing bullet %q", want)
		}
	}
	// 旧 layout の bullet は含まれないこと (Req 8.2)。
	if strings.Contains(content, "legacy bullet must not appear") {
		t.Errorf("existing-rules.txt must not include legacy .claude/rules/playbook content")
	}
	// 旧 layout の path も含まれないこと。
	if strings.Contains(content, ".claude/rules/playbook") {
		t.Errorf("existing-rules.txt must not reference .claude/rules/playbook path")
	}
}

// TestCuratorExistingRulesPartialCategoryFiles: 一部 category file のみ存在するとき、
// 存在する分のみ separator + 本文が含まれ、不在 file は warning も error も無く skip される。
func TestCuratorExistingRulesPartialCategoryFiles(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()

	// 2 file のみ作成 (failure-recovery / tool-pattern)。
	writeWikiCategoryFile(t, cwd, CategoryFailureRecovery, [][2]string{
		{"fr-topic", "fr bullet"},
	})
	writeWikiCategoryFile(t, cwd, CategoryToolPattern, [][2]string{
		{"tp-topic", "tp bullet"},
	})

	agent := &fakeCuratorAgent{
		resp: provider.AgentResponse{Content: `{"operations":[]}`},
	}
	_, err := Curator(context.Background(), runDir, CuratorInput{
		CWD:         cwd,
		ParentRunID: "run-1",
	}, agent)
	if err != nil {
		t.Fatalf("Curator: %v", err)
	}

	existingPath := filepath.Join(runDir, existingRulesFilename)
	body, rerr := os.ReadFile(existingPath)
	if rerr != nil {
		t.Fatalf("read existing-rules.txt: %v", rerr)
	}
	content := string(body)
	if !strings.Contains(content, "===== docs_dev/playbook/failure-recovery.md =====") {
		t.Errorf("missing failure-recovery separator")
	}
	if !strings.Contains(content, "===== docs_dev/playbook/tool-pattern.md =====") {
		t.Errorf("missing tool-pattern separator")
	}
	// 不在 file の separator は出力されない。
	if strings.Contains(content, "===== docs_dev/playbook/antipattern.md =====") {
		t.Errorf("antipattern file is absent; separator should not appear")
	}
	if strings.Contains(content, "===== docs_dev/playbook/success-procedure.md =====") {
		t.Errorf("success-procedure file is absent; separator should not appear")
	}
}

// TestBuildLearningsExistingRulesArtifactSizeGuard: 観点 (f) 256 KiB 超過 →
// head-tail-clip 戦略で truncated=true / strategy=playbook-only+head-tail-clip。
//
// 新 layout: docs_dev/playbook/<category>.md を 1 file 巨大化させて size guard を発火させる。
func TestBuildLearningsExistingRulesArtifactSizeGuard(t *testing.T) {
	cwd := t.TempDir()

	// 3 category file をそれぞれ ~5KB 程度に作成 (合計 ~15KB)。limit=4KB で degrade 経路。
	// 各 file は `# <category>` heading + 概要 + 1 つの section + 大量 bullet で構成。
	bigSectionBody := strings.Repeat("- bullet line repeated for size guard test\n", 200)
	bigFileBody := func(cat string) string {
		return "# " + cat + "\n\n概要\n\n## big-topic\n**適用条件:** any\n**summary:** big section\n\n" + bigSectionBody
	}
	playbookDir := filepath.Join(cwd, "docs_dev", "playbook")
	if err := os.MkdirAll(playbookDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, cat := range []string{
		CategoryFailureRecovery,
		CategoryAntipattern,
		CategoryToolPattern,
	} {
		if err := os.WriteFile(filepath.Join(playbookDir, cat+".md"), []byte(bigFileBody(cat)), 0o644); err != nil {
			t.Fatalf("write %s: %v", cat, err)
		}
	}

	outPath := filepath.Join(t.TempDir(), "existing-rules.txt")
	bytes, truncated, strategy, err := buildLearningsExistingRulesArtifact(cwd, outPath, 4*1024)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !truncated {
		t.Errorf("truncated = false, want true")
	}
	if strategy != existingRulesTruncationStrategyHeadTailClip {
		t.Errorf("strategy = %q, want %q", strategy, existingRulesTruncationStrategyHeadTailClip)
	}
	if bytes <= 0 {
		t.Errorf("bytes = %d, want > 0", bytes)
	}
	// 切り詰め後の本文に少なくとも 1 つの category heading (`# `) が残ること。
	out, rerr := os.ReadFile(outPath)
	if rerr != nil {
		t.Fatalf("read out: %v", rerr)
	}
	if !strings.Contains(string(out), "# ") {
		t.Errorf("at least one category heading should remain in clipped body")
	}
}

// TestBuildLearningsExistingRulesArtifactNoPlaybookDir: playbook root が無いとき
// 空 artifact が書き出される (本 spec 初回実行経路)。
func TestBuildLearningsExistingRulesArtifactNoPlaybookDir(t *testing.T) {
	cwd := t.TempDir() // docs_dev/playbook ディレクトリは作らない
	outPath := filepath.Join(t.TempDir(), "existing-rules.txt")
	bytes, truncated, strategy, err := buildLearningsExistingRulesArtifact(cwd, outPath, MaxExistingRulesBytes)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if bytes != 0 {
		t.Errorf("bytes = %d, want 0", bytes)
	}
	if truncated {
		t.Errorf("truncated = true, want false")
	}
	if strategy != "" {
		t.Errorf("strategy = %q, want empty", strategy)
	}
	body, rerr := os.ReadFile(outPath)
	if rerr != nil {
		t.Fatalf("read out: %v", rerr)
	}
	if len(body) != 0 {
		t.Errorf("body len = %d, want 0", len(body))
	}
}

// TestBuildCuratorPromptDefault: skill 空 → slash command 組立。
func TestBuildCuratorPromptDefault(t *testing.T) {
	got := buildCuratorPrompt("", "/tmp/run-dir-1")
	want := "/agent-learnings-curator curator /tmp/run-dir-1"
	if got != want {
		t.Errorf("got=%q want=%q", got, want)
	}
}

// TestBuildCuratorPromptOverride: skill 明示 → そのまま採用。
func TestBuildCuratorPromptOverride(t *testing.T) {
	got := buildCuratorPrompt("custom-skill-cmd", "/tmp/x")
	if got != "custom-skill-cmd" {
		t.Errorf("got=%q want=custom-skill-cmd", got)
	}
}

// TestBuildLearningsExistingRulesArtifactTopicIndexPrepended: 4 category 全て + tool-pattern
// 内 2 topic を作って、existing-rules.txt 先頭に Topic Index header が
// `category SSoT 順 → 各 category 内 alphabetical` で出力されることを観測する (A-1)。
//
// 新 layout: 各 category file 内の `## <topic>` section から topic 一覧を抽出する。
func TestBuildLearningsExistingRulesArtifactTopicIndexPrepended(t *testing.T) {
	cwd := t.TempDir()

	writeWikiCategoryFile(t, cwd, CategoryFailureRecovery, [][2]string{
		{"alpha", "fr bullet"},
	})
	writeWikiCategoryFile(t, cwd, CategorySuccessProcedure, [][2]string{
		{"sigma", "sp bullet"},
	})
	writeWikiCategoryFile(t, cwd, CategoryAntipattern, [][2]string{
		{"omega", "ap bullet"},
	})
	writeWikiCategoryFile(t, cwd, CategoryToolPattern, [][2]string{
		{"zulu", "zulu bullet"},
		{"kilo", "kilo bullet"},
	})

	outPath := filepath.Join(t.TempDir(), "existing-rules.txt")
	bytes, truncated, strategy, err := buildLearningsExistingRulesArtifact(cwd, outPath, MaxExistingRulesBytes)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if truncated || strategy != "" {
		t.Errorf("truncated=%v strategy=%q, want false / empty", truncated, strategy)
	}
	if bytes <= 0 {
		t.Fatalf("bytes = %d, want > 0", bytes)
	}

	body, rerr := os.ReadFile(outPath)
	if rerr != nil {
		t.Fatalf("read out: %v", rerr)
	}
	content := string(body)

	// 先頭が Topic Index で始まる
	if !strings.HasPrefix(content, "## Topic Index") {
		t.Fatalf("artifact must start with `## Topic Index`, got first 80 bytes = %q", firstN(content, 80))
	}

	// category SSoT 順 (failure-recovery → success-procedure → antipattern → tool-pattern)
	wantOrder := []string{
		"- failure-recovery: alpha",
		"- success-procedure: sigma",
		"- antipattern: omega",
		"- tool-pattern: kilo, zulu", // alphabetical
	}
	prev := -1
	for _, want := range wantOrder {
		idx := strings.Index(content, want)
		if idx < 0 {
			t.Errorf("Topic Index missing line %q in content:\n%s", want, content)
			continue
		}
		if idx < prev {
			t.Errorf("Topic Index order broken: %q at %d came before previous (prev=%d)", want, idx, prev)
		}
		prev = idx
	}

	// header の後に separator + 本文が続く
	if !strings.Contains(content, "===== ") {
		t.Errorf("entry separator missing: %s", content)
	}
}

// TestBuildLearningsExistingRulesArtifactTopicIndexSurvivesClipping: head-tail-clip 経路でも
// Topic Index header が verbatim 残ることを観測する (A-1 budget 加算)。
func TestBuildLearningsExistingRulesArtifactTopicIndexSurvivesClipping(t *testing.T) {
	cwd := t.TempDir()

	// tool-pattern category file 1 つに 3 section を含めて巨大化、clip 経路へ。
	bigSectionBody := strings.Repeat("- bullet line\n", 200)
	var b strings.Builder
	b.WriteString("# " + CategoryToolPattern + "\n\n概要\n")
	for _, topic := range []string{"first", "second", "third"} {
		b.WriteString("\n## " + topic + "\n")
		b.WriteString("**適用条件:** any\n")
		b.WriteString("**summary:** big section\n\n")
		b.WriteString(bigSectionBody)
	}
	playbookDir := filepath.Join(cwd, "docs_dev", "playbook")
	if err := os.MkdirAll(playbookDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(playbookDir, CategoryToolPattern+".md"), []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	outPath := filepath.Join(t.TempDir(), "existing-rules.txt")
	_, truncated, strategy, err := buildLearningsExistingRulesArtifact(cwd, outPath, 4*1024)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !truncated {
		t.Errorf("truncated = false, want true (clip 経路)")
	}
	if strategy != existingRulesTruncationStrategyHeadTailClip {
		t.Errorf("strategy = %q, want %q", strategy, existingRulesTruncationStrategyHeadTailClip)
	}

	body, rerr := os.ReadFile(outPath)
	if rerr != nil {
		t.Fatalf("read out: %v", rerr)
	}
	content := string(body)

	// clip された artifact でも Topic Index header が verbatim 残ること
	if !strings.HasPrefix(content, "## Topic Index") {
		t.Fatalf("clipped artifact must start with `## Topic Index`, got first 80 bytes = %q", firstN(content, 80))
	}
	// 3 topic alphabetical 順 (first, second, third)
	if !strings.Contains(content, "- tool-pattern: first, second, third") {
		t.Errorf("Topic Index line missing (alphabetical 3 topics): %s", firstN(content, 200))
	}
}

// firstN は string の先頭 N bytes を debug 出力用に切り出す helper。
func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// TestConcatLearningsExistingRulesEntries: separator + body の concat 順序を観測する。
// 新 layout では entries.rel は `docs_dev/playbook/<category>.md` 形式となる。
func TestConcatLearningsExistingRulesEntries(t *testing.T) {
	entries := []learningsExistingRulesEntry{
		{rel: "docs_dev/playbook/failure-recovery.md", body: []byte("body-FR\n"), category: CategoryFailureRecovery, topics: []string{"alpha"}},
		{rel: "docs_dev/playbook/tool-pattern.md", body: []byte("body-TP\n"), category: CategoryToolPattern, topics: []string{"git"}},
	}
	got := concatLearningsExistingRulesEntries(entries)
	if !strings.Contains(got, "===== docs_dev/playbook/failure-recovery.md =====\nbody-FR") {
		t.Errorf("missing entry FR: %q", got)
	}
	if !strings.Contains(got, "===== docs_dev/playbook/tool-pattern.md =====\nbody-TP") {
		t.Errorf("missing entry TP: %q", got)
	}
	// 順序は slice 順序が保たれる
	idxFR := strings.Index(got, "failure-recovery.md")
	idxTP := strings.Index(got, "tool-pattern.md")
	if idxFR < 0 || idxTP < 0 || idxFR > idxTP {
		t.Errorf("order wrong: idxFR=%d idxTP=%d", idxFR, idxTP)
	}
}

// TestFormatTopicIndex_FlatLayoutOnly: 新 layout では category file 1 entry あたり
// 複数 topic を保持する。category が closed enum でない entry は skip される。
func TestFormatTopicIndex_UnknownCategoryDropped(t *testing.T) {
	entries := []learningsExistingRulesEntry{
		{rel: "docs_dev/playbook/bogus-category.md", body: []byte("x\n"), category: "bogus-category", topics: []string{"foo"}},
		{rel: "docs_dev/playbook/tool-pattern.md", body: []byte("y\n"), category: CategoryToolPattern, topics: []string{"bar"}},
	}
	got := formatTopicIndex(entries)
	if strings.Contains(got, "bogus-category") {
		t.Errorf("unknown category should be dropped: %s", got)
	}
	if !strings.Contains(got, "- tool-pattern: bar") {
		t.Errorf("valid category should appear: %s", got)
	}
}
