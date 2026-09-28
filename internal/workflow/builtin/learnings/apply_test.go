// Package learnings の本 file は agent-learnings apply phase (wiki layout) の単体テスト群を保持する
// (tasks.md 3.1-3.3 / Req 1.1, 1.2, 1.5, 1.6, 1.7, 2.3, 3.1-3.7, 4.1, 5.1, 5.2, 5.3, 5.4, 5.5,
// 6.6, 7.6, 8.1, 8.2, 8.3, 8.5, 8.6)。
//
// 検証観点 (wiki layout):
//
//   - (a) operations 0 件 → Verdict=applied-no-op で副作用なし。
//   - (b) op != "add" → reject reason=op-not-add (close verdict 集合外 → applied-no-op)。
//   - (c) ValidCategory 違反 → Verdict=rejected-invalid-category (全件同 reason)。
//   - (d) ValidTopicName 違反 → Verdict=rejected-invalid-topic-name (全件同 reason)。
//   - (e) paths_glob 空 → Verdict=rejected-empty-paths-glob。
//   - (f) paths_glob 単独 unbounded (`**` / `**/*` / `*`) → Verdict=rejected-unbounded-paths-glob。
//   - (g) 1 件 accept → Verdict=applied + new category file + new section + 1 bullet。
//   - (h) 既存 section に追記 → bullet 順序保持 + applies_to/summary 行不変 (first-write-wins)。
//   - (i) category-scoped jaccard dedup → 既存 section の bullet と類似 → skip (Req 5.1)。
//   - (l) NewTopicsCreated は実際に新規 section 作成された (cat,topic) のみ記録される。
//   - (m) summary 検証: 空 / 改行 / 100 文字超 → reject reason=summary-invalid。
//   - (n) applies_to 検証: 改行 / 100 文字超 → reject reason=applies_to-invalid (空文字は accept してプレースホルダ)。
//   - (o) RebuildIndex が apply 後 1 回呼ばれ index.md が生成される。
//   - (p) IndexWarnings は index rebuild の per-category warning を伝播する。
//   - (q) decideApplyVerdict の境界条件 (混在 reject / 全 dedup / 全 accept)。
package learnings

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readApplyResultT は test 専用 helper。apply-result.json を decode して返す。
func readApplyResultT(t *testing.T, path string) ApplyResult {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read apply-result.json: %v", err)
	}
	var r ApplyResult
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("unmarshal apply-result.json: %v\nbody=%s", err, body)
	}
	return r
}

// writeCategoryFile は wiki layout の category fixture を簡潔に書き出す test helper。
// (新 path `docs_dev/playbook/<category>.md` を直接書く)
func writeCategoryFile(t *testing.T, root, category, body string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", root, err)
	}
	path := filepath.Join(root, category+".md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

// playbookRootFromCWD は test 内で `<cwd>/docs_dev/playbook` を構築する shortcut。
func playbookRootFromCWD(cwd string) string {
	return filepath.Join(cwd, "docs_dev", "playbook")
}

// validOp は 6 field を埋めた最低限有効な op fixture を返す (test 用)。
// 値は caller が必要な部分のみ override する。
func validOp() CuratorOperation {
	return CuratorOperation{
		Op:        "add",
		Category:  "failure-recovery",
		Topic:     "sample-topic",
		PathsGlob: []string{"src/**"},
		Entry:     "test entry bullet",
		Summary:   "test summary",
		AppliesTo: "test applies condition",
	}
}

// TestApplyEmptyOperations: 観点 (a) 0 件 → applied-no-op 副作用なし。
func TestApplyEmptyOperations(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	path, res, err := Apply(context.Background(), runDir, ApplyInput{
		CWD: cwd, Operations: nil,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Verdict != VerdictAppliedNoOp {
		t.Errorf("Verdict=%q, want %q", res.Verdict, VerdictAppliedNoOp)
	}
	if len(res.Appended) != 0 || len(res.Rejected) != 0 || len(res.Deduped) != 0 {
		t.Errorf("non-zero accumulators: appended=%d rejected=%d deduped=%d",
			len(res.Appended), len(res.Rejected), len(res.Deduped))
	}
	if path == "" {
		t.Error("path empty")
	}
	// playbook ディレクトリは作られない (副作用なし)
	if _, err := os.Stat(playbookRootFromCWD(cwd)); !os.IsNotExist(err) {
		t.Errorf("playbook dir should not exist: %v", err)
	}
}

func TestApplyCancelledContextDoesNotWritePlaybook(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	path, res, err := Apply(ctx, runDir, ApplyInput{
		CWD:        cwd,
		Operations: []CuratorOperation{validOp()},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply err=%v, want context.Canceled", err)
	}
	if path != "" {
		t.Fatalf("path=%q, want empty because apply-result is owned by orchestration defer on cancel", path)
	}
	if res.Verdict != VerdictAppliedNoOp || len(res.Appended) != 0 {
		t.Fatalf("Apply result=%+v, want applied-no-op without appended entries", res)
	}
	if _, err := os.Stat(playbookRootFromCWD(cwd)); !os.IsNotExist(err) {
		t.Fatalf("playbook dir should not exist after canceled Apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, applyResultFilename)); !os.IsNotExist(err) {
		t.Fatalf("apply-result.json should not be written by canceled direct Apply: %v", err)
	}
}

// TestApplyOpNotAddReject: 観点 (b) op != "add" → reject。
func TestApplyOpNotAddReject(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	op := validOp()
	op.Op = "delete"
	_, res, err := Apply(context.Background(), runDir, ApplyInput{
		CWD:        cwd,
		Operations: []CuratorOperation{op},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Rejected) != 1 || res.Rejected[0].Reason != rejectReasonOpNotAdd {
		t.Fatalf("Rejected = %+v", res.Rejected)
	}
	// 全 reject かつ reason が closed verdict 集合外なので applied-no-op に降格
	if res.Verdict != VerdictAppliedNoOp {
		t.Errorf("Verdict = %q, want applied-no-op", res.Verdict)
	}
}

// TestApplyInvalidCategoryReject: 観点 (c) category 不一致 → rejected-invalid-category。
func TestApplyInvalidCategoryReject(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	op := validOp()
	op.Category = "anti-pattern" // 不正な category
	_, res, err := Apply(context.Background(), runDir, ApplyInput{
		CWD:        cwd,
		Operations: []CuratorOperation{op},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Rejected) != 1 || res.Rejected[0].Reason != VerdictRejectedInvalidCategory {
		t.Fatalf("Rejected = %+v", res.Rejected)
	}
	if res.Verdict != VerdictRejectedInvalidCategory {
		t.Errorf("Verdict = %q, want %q", res.Verdict, VerdictRejectedInvalidCategory)
	}
}

// TestApplyInvalidTopicNameReject: 観点 (d) topic name 不一致 → rejected-invalid-topic-name。
func TestApplyInvalidTopicNameReject(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	cases := []string{"BadTopic", "1foo", "-foo", "foo_bar", "foo bar", ""}
	for _, topic := range cases {
		op := validOp()
		op.Topic = topic
		_, res, err := Apply(context.Background(), runDir, ApplyInput{
			CWD:        cwd,
			Operations: []CuratorOperation{op},
		})
		if err != nil {
			t.Fatalf("Apply (%q): %v", topic, err)
		}
		if res.Verdict != VerdictRejectedInvalidTopicName {
			t.Errorf("topic=%q: Verdict = %q, want %q", topic, res.Verdict, VerdictRejectedInvalidTopicName)
		}
	}
}

// TestApplyEmptyPathsGlobReject: 観点 (e) paths_glob 空 → rejected-empty-paths-glob。
func TestApplyEmptyPathsGlobReject(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	op := validOp()
	op.PathsGlob = nil
	_, res, err := Apply(context.Background(), runDir, ApplyInput{
		CWD:        cwd,
		Operations: []CuratorOperation{op},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Verdict != VerdictRejectedEmptyPathsGlob {
		t.Errorf("Verdict = %q, want %q", res.Verdict, VerdictRejectedEmptyPathsGlob)
	}
}

// TestApplyUnboundedPathsGlobReject: 観点 (f) `**` / `**/*` / `*` 単独は reject。
func TestApplyUnboundedPathsGlobReject(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	for _, glob := range []string{"**", "**/*", "*"} {
		op := validOp()
		op.PathsGlob = []string{glob}
		_, res, err := Apply(context.Background(), runDir, ApplyInput{
			CWD:        cwd,
			Operations: []CuratorOperation{op},
		})
		if err != nil {
			t.Fatalf("Apply (%q): %v", glob, err)
		}
		if res.Verdict != VerdictRejectedUnboundedPathsGlob {
			t.Errorf("glob=%q: Verdict = %q, want %q", glob, res.Verdict, VerdictRejectedUnboundedPathsGlob)
		}
	}
}

// TestApplyAcceptCreatesNewTopic: 観点 (g) 1 件 accept で wiki layout の new category file + section + bullet。
func TestApplyAcceptCreatesNewTopic(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	op := validOp()
	op.Topic = "broken-link"
	op.Entry = "fix broken link by reverting commit"
	op.Summary = "broken link recovery"
	op.AppliesTo = "git operations"
	_, res, err := Apply(context.Background(), runDir, ApplyInput{
		CWD:        cwd,
		Operations: []CuratorOperation{op},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Verdict != VerdictApplied {
		t.Errorf("Verdict = %q, want applied", res.Verdict)
	}
	if len(res.Appended) != 1 {
		t.Fatalf("Appended len = %d, want 1", len(res.Appended))
	}
	if res.Appended[0].Category != "failure-recovery" || res.Appended[0].Topic != "broken-link" {
		t.Errorf("Appended[0] = %+v", res.Appended[0])
	}
	wantNew := "failure-recovery/broken-link"
	if len(res.NewTopicsCreated) != 1 || res.NewTopicsCreated[0] != wantNew {
		t.Errorf("NewTopicsCreated = %v, want [%q]", res.NewTopicsCreated, wantNew)
	}
	// file 確認: 新形式 wiki layout (`docs_dev/playbook/<category>.md`)
	path := filepath.Join(playbookRootFromCWD(cwd), "failure-recovery.md")
	body, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read category file: %v", rerr)
	}
	content := string(body)
	// frontmatter は書かれない (Req 1.5)
	if strings.Contains(content, "paths:") {
		t.Errorf("frontmatter paths: leaked into wiki file: %s", content)
	}
	if strings.Contains(content, "---\n") {
		t.Errorf("frontmatter delimiter `---` leaked into wiki file: %s", content)
	}
	// category heading (Req 1.3)
	if !strings.Contains(content, "# failure-recovery\n") {
		t.Errorf("category heading missing: %s", content)
	}
	// section heading (Req 1.4)
	if !strings.Contains(content, "## broken-link\n") {
		t.Errorf("section heading missing: %s", content)
	}
	// applies_to / summary 行 (Req 1.6 / 3.7)
	if !strings.Contains(content, "**適用条件:** git operations") {
		t.Errorf("applies_to line missing: %s", content)
	}
	if !strings.Contains(content, "**summary:** broken link recovery") {
		t.Errorf("summary line missing: %s", content)
	}
	// bullet
	if !strings.Contains(content, "- fix broken link by reverting commit\n") {
		t.Errorf("bullet missing: %s", content)
	}
}

// TestApplySectionAwareAppendExistingSectionFirstWriteWins: 観点 (h) 既存 section に追記、
// applies_to / summary 行は上書きされない (first-write-wins / Req 3.1, 3.4)。
func TestApplySectionAwareAppendExistingSectionFirstWriteWins(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	root := playbookRootFromCWD(cwd)
	// 既存 wiki category file (section / applies_to / summary / bullet を持つ)
	existing := "# tool-pattern\n\ntool 利用パターン intro\n\n## git-stash\n**適用条件:** 既存条件オリジナル\n**summary:** 既存サマリオリジナル\n\n- existing bullet 1\n- existing bullet 2\n"
	writeCategoryFile(t, root, "tool-pattern", existing)

	op := validOp()
	op.Category = "tool-pattern"
	op.Topic = "git-stash"
	op.Entry = "stash before rebase"
	op.Summary = "新規 summary (ignored)"
	op.AppliesTo = "新規 applies (ignored)"
	op.PathsGlob = []string{"new/**"}
	_, res, err := Apply(context.Background(), runDir, ApplyInput{
		CWD:        cwd,
		Operations: []CuratorOperation{op},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Verdict != VerdictApplied {
		t.Errorf("Verdict = %q, want applied", res.Verdict)
	}
	if len(res.NewTopicsCreated) != 0 {
		t.Errorf("NewTopicsCreated = %v, want empty (section existed)", res.NewTopicsCreated)
	}
	body, _ := os.ReadFile(filepath.Join(root, "tool-pattern.md"))
	content := string(body)
	// 既存 applies_to / summary が不変 (first-write-wins)
	if !strings.Contains(content, "**適用条件:** 既存条件オリジナル") {
		t.Errorf("original applies_to changed: %s", content)
	}
	if !strings.Contains(content, "**summary:** 既存サマリオリジナル") {
		t.Errorf("original summary changed: %s", content)
	}
	if strings.Contains(content, "新規 applies") || strings.Contains(content, "新規 summary") {
		t.Errorf("new applies_to / summary leaked into existing section: %s", content)
	}
	// 既存 bullet 順序保持 + 末尾追記
	idx1 := strings.Index(content, "existing bullet 1")
	idx2 := strings.Index(content, "existing bullet 2")
	idx3 := strings.Index(content, "stash before rebase")
	if !(idx1 < idx2 && idx2 < idx3) {
		t.Errorf("order broken: 1=%d 2=%d 3=%d body=%s", idx1, idx2, idx3, content)
	}
}

// TestApplyCategoryScopedDedupSkip: 観点 (i) 同 category 配下に類似 bullet → dedup skip (Req 5.1)。
func TestApplyCategoryScopedDedupSkip(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	root := playbookRootFromCWD(cwd)
	// 既存 wiki category file の他 topic に類似 bullet を埋め込む
	existing := "# antipattern\n\nantipattern intro\n\n## test-mock\n**適用条件:** 既存条件\n**summary:** 既存サマリ\n\n- avoid using mock database in integration tests\n"
	writeCategoryFile(t, root, "antipattern", existing)

	// 同 category の別 topic に類似 bullet を投入 → dedup skip すべき
	op := validOp()
	op.Category = "antipattern"
	op.Topic = "different-topic"
	op.Entry = "avoid using mock database in integration tests"
	op.PathsGlob = []string{"y/**"}
	_, res, err := Apply(context.Background(), runDir, ApplyInput{
		CWD:        cwd,
		Operations: []CuratorOperation{op},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Deduped) != 1 {
		t.Fatalf("Deduped = %+v, want 1 entry", res.Deduped)
	}
	if res.Deduped[0].Jaccard < 0.8 {
		t.Errorf("Jaccard = %f, want >= 0.8", res.Deduped[0].Jaccard)
	}
	if len(res.Appended) != 0 {
		t.Errorf("Appended = %+v, want empty", res.Appended)
	}
	if res.Verdict != VerdictAppliedNoOp {
		t.Errorf("Verdict = %q, want applied-no-op (全 dedup)", res.Verdict)
	}
}

// TestApplyMixedAcceptAndReject: 観点 「混在は global verdict=applied (1+ accepted)」。
func TestApplyMixedAcceptAndReject(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	ok := validOp()
	ok.Category = "tool-pattern"
	ok.Topic = "ok-topic"
	ok.PathsGlob = []string{"a/**"}
	ok.Entry = "valid bullet"

	badCat := validOp()
	badCat.Category = "BAD"
	badCat.Topic = "x"
	badCat.PathsGlob = []string{"b/**"}
	badCat.Entry = "e"

	emptyGlob := validOp()
	emptyGlob.Category = "antipattern"
	emptyGlob.Topic = "y"
	emptyGlob.PathsGlob = []string{}
	emptyGlob.Entry = "e2"

	_, res, err := Apply(context.Background(), runDir, ApplyInput{
		CWD:        cwd,
		Operations: []CuratorOperation{ok, badCat, emptyGlob},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Verdict != VerdictApplied {
		t.Errorf("Verdict = %q, want applied (1+ accepted)", res.Verdict)
	}
	if len(res.Appended) != 1 {
		t.Errorf("Appended = %d, want 1", len(res.Appended))
	}
	if len(res.Rejected) != 2 {
		t.Errorf("Rejected = %d, want 2", len(res.Rejected))
	}
}

// TestApplyAllRejectMixedReasonsApplyNoOp: 全 reject かつ reason 混在 → applied-no-op。
func TestApplyAllRejectMixedReasonsApplyNoOp(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	badCat := validOp()
	badCat.Category = "BAD"
	emptyGlob := validOp()
	emptyGlob.PathsGlob = []string{}
	_, res, err := Apply(context.Background(), runDir, ApplyInput{
		CWD:        cwd,
		Operations: []CuratorOperation{badCat, emptyGlob},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Verdict != VerdictAppliedNoOp {
		t.Errorf("Verdict = %q, want applied-no-op (混在)", res.Verdict)
	}
}

// TestDecideApplyVerdict: decideApplyVerdict の境界条件。
func TestDecideApplyVerdict(t *testing.T) {
	cases := []struct {
		name     string
		appended []AppendedEntry
		rejected []RejectedOperation
		deduped  []DedupSkippedOperation
		opCount  int
		want     string
	}{
		{"0 ops", nil, nil, nil, 0, VerdictAppliedNoOp},
		{"全 accept", []AppendedEntry{{}}, nil, nil, 1, VerdictApplied},
		{"1+ accept (混在)", []AppendedEntry{{}}, []RejectedOperation{{Reason: VerdictRejectedInvalidCategory}}, nil, 2, VerdictApplied},
		{"全 reject 同一 reason", nil, []RejectedOperation{{Reason: VerdictRejectedInvalidCategory}, {Reason: VerdictRejectedInvalidCategory}}, nil, 2, VerdictRejectedInvalidCategory},
		{"全 reject 混在", nil, []RejectedOperation{{Reason: VerdictRejectedInvalidCategory}, {Reason: VerdictRejectedEmptyPathsGlob}}, nil, 2, VerdictAppliedNoOp},
		{"全 dedup", nil, nil, []DedupSkippedOperation{{}}, 1, VerdictAppliedNoOp},
		{"全 reject (closed verdict 集合外)", nil, []RejectedOperation{{Reason: rejectReasonOpNotAdd}}, nil, 1, VerdictAppliedNoOp},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideApplyVerdict(tc.appended, tc.rejected, tc.deduped, tc.opCount)
			if got != tc.want {
				t.Errorf("got=%q want=%q", got, tc.want)
			}
		})
	}
}

// TestScanReadsCategoryAsDedupPool: scanLearningsPlaybook が wiki layout (4 category file) を
// 読み込み per-category dedup pool を構築することを確認 (Req 5.1)。
func TestScanReadsCategoryAsDedupPool(t *testing.T) {
	cwd := t.TempDir()
	root := playbookRootFromCWD(cwd)
	// tool-pattern.md に 2 section、各 1 bullet を配置
	body := "# tool-pattern\n\nintro\n\n## topic-a\n**適用条件:** 条件A\n**summary:** サマA\n\n- bullet content alpha\n\n## topic-b\n**適用条件:** 条件B\n**summary:** サマB\n\n- bullet content beta\n"
	writeCategoryFile(t, root, "tool-pattern", body)
	// 他 category 不在は空 pool として扱われる
	pool, err := scanLearningsPlaybook(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(pool["tool-pattern"]) != 2 {
		t.Fatalf("pool[tool-pattern] = %d, want 2", len(pool["tool-pattern"]))
	}
	bodies := []string{pool["tool-pattern"][0].body, pool["tool-pattern"][1].body}
	if bodies[0] != "bullet content alpha" || bodies[1] != "bullet content beta" {
		t.Errorf("pool bullets = %v, want [bullet content alpha, bullet content beta]", bodies)
	}
}

// TestScanReadsAbsentRoot: playbook root 自体が不在 → 空 pool で副作用なし継続。
func TestScanReadsAbsentRoot(t *testing.T) {
	cwd := t.TempDir()
	pool, err := scanLearningsPlaybook(playbookRootFromCWD(cwd))
	if err != nil {
		t.Fatalf("scan absent root: %v", err)
	}
	if len(pool) != 0 {
		t.Errorf("pool = %+v, want empty", pool)
	}
}

// TestApplyArtifactSchema: apply-result.json が closed-set 9 値の verdict 付きで書き出される。
func TestApplyArtifactSchema(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	op := validOp()
	op.Category = "success-procedure"
	op.Topic = "deploy"
	op.PathsGlob = []string{"deploy/**"}
	op.Entry = "run smoke after canary"
	path, _, err := Apply(context.Background(), runDir, ApplyInput{
		CWD:        cwd,
		Operations: []CuratorOperation{op},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	a := readApplyResultT(t, path)
	if !IsValidVerdict(a.Verdict) {
		t.Errorf("Verdict %q is not in closed set 9", a.Verdict)
	}
	if a.StartedAt == "" || a.FinishedAt == "" {
		t.Errorf("StartedAt/FinishedAt empty: %+v", a)
	}
}

// TestApplyMultipleOpsToSameTopic: 同 (cat,topic) に複数 op → 同 section に複数 bullet 追記。
func TestApplyMultipleOpsToSameTopic(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	op1 := validOp()
	op1.Topic = "shared-topic"
	op1.PathsGlob = []string{"a/**"}
	op1.Entry = "first bullet about X"

	op2 := validOp()
	op2.Topic = "shared-topic"
	op2.PathsGlob = []string{"b/**"}
	op2.Entry = "totally different second bullet about Y"

	_, res, err := Apply(context.Background(), runDir, ApplyInput{
		CWD:        cwd,
		Operations: []CuratorOperation{op1, op2},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Verdict != VerdictApplied {
		t.Errorf("Verdict = %q, want applied", res.Verdict)
	}
	if len(res.Appended) != 2 {
		t.Errorf("Appended = %d, want 2", len(res.Appended))
	}
	// 1 category file に同 section の bullets 2 個
	body, _ := os.ReadFile(filepath.Join(playbookRootFromCWD(cwd), "failure-recovery.md"))
	if !strings.Contains(string(body), "first bullet about X") || !strings.Contains(string(body), "totally different second bullet about Y") {
		t.Errorf("missing bullets: %s", body)
	}
	// section が 1 つだけ (`## shared-topic`)
	if got := strings.Count(string(body), "## shared-topic\n"); got != 1 {
		t.Errorf("section count = %d, want 1, body=%s", got, body)
	}
}

// TestApplyDedupSelfWithinSameRun: 同 run 内で前 op と類似 bullet → 後続 op は dedup skip (Req 5.4)。
func TestApplyDedupSelfWithinSameRun(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	op1 := validOp()
	op1.Category = "antipattern"
	op1.Topic = "topic-a"
	op1.Entry = "avoid mocking database in integration tests"
	op2 := op1
	op2.Entry = "avoid mocking database in integration tests"
	_, res, err := Apply(context.Background(), runDir, ApplyInput{
		CWD:        cwd,
		Operations: []CuratorOperation{op1, op2},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Appended) != 1 {
		t.Errorf("Appended = %d, want 1 (2nd op is dedup'd)", len(res.Appended))
	}
	if len(res.Deduped) != 1 {
		t.Errorf("Deduped = %d, want 1", len(res.Deduped))
	}
}

// TestApplyRejectsEmptySummary: 観点 (m) summary 空 → reject reason=summary-invalid。
func TestApplyRejectsEmptySummary(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	op := validOp()
	op.Summary = ""
	_, res, err := Apply(context.Background(), runDir, ApplyInput{
		CWD:        cwd,
		Operations: []CuratorOperation{op},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Rejected) != 1 || res.Rejected[0].Reason != rejectReasonSummaryInvalid {
		t.Fatalf("Rejected = %+v, want 1 entry with reason=%q", res.Rejected, rejectReasonSummaryInvalid)
	}
}

// TestApplyRejectsNewlineSummary: 観点 (m) summary に改行 → reject reason=summary-invalid。
func TestApplyRejectsNewlineSummary(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	op := validOp()
	op.Summary = "abc\ndef"
	_, res, err := Apply(context.Background(), runDir, ApplyInput{
		CWD:        cwd,
		Operations: []CuratorOperation{op},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Rejected) != 1 || res.Rejected[0].Reason != rejectReasonSummaryInvalid {
		t.Fatalf("Rejected = %+v", res.Rejected)
	}
}

// TestApplyRejectsOver100CharsSummary: 観点 (m) summary 101 文字 → reject reason=summary-invalid。
func TestApplyRejectsOver100CharsSummary(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	op := validOp()
	op.Summary = strings.Repeat("a", 101)
	_, res, err := Apply(context.Background(), runDir, ApplyInput{
		CWD:        cwd,
		Operations: []CuratorOperation{op},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Rejected) != 1 || res.Rejected[0].Reason != rejectReasonSummaryInvalid {
		t.Fatalf("Rejected = %+v", res.Rejected)
	}
}

// TestApplyRejectsNewlineAppliesTo: 観点 (n) applies_to に改行 → reject reason=applies_to-invalid。
func TestApplyRejectsNewlineAppliesTo(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	op := validOp()
	op.AppliesTo = "a\nb"
	_, res, err := Apply(context.Background(), runDir, ApplyInput{
		CWD:        cwd,
		Operations: []CuratorOperation{op},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Rejected) != 1 || res.Rejected[0].Reason != rejectReasonAppliesToInvalid {
		t.Fatalf("Rejected = %+v, want 1 entry with reason=%q", res.Rejected, rejectReasonAppliesToInvalid)
	}
}

// TestApplyRejectsOver100CharsAppliesTo: 観点 (n) applies_to 101 文字 → reject reason=applies_to-invalid。
func TestApplyRejectsOver100CharsAppliesTo(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	op := validOp()
	op.AppliesTo = strings.Repeat("a", 101)
	_, res, err := Apply(context.Background(), runDir, ApplyInput{
		CWD:        cwd,
		Operations: []CuratorOperation{op},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Rejected) != 1 || res.Rejected[0].Reason != rejectReasonAppliesToInvalid {
		t.Fatalf("Rejected = %+v", res.Rejected)
	}
}

// TestApplyAcceptsEmptyAppliesToWithPlaceholder: 観点 (n) applies_to 空文字列 → accept、section の
// `**適用条件:**` 行に `(なし)` プレースホルダで書き出される。
func TestApplyAcceptsEmptyAppliesToWithPlaceholder(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	op := validOp()
	op.Topic = "empty-applies"
	op.AppliesTo = ""
	op.Entry = "bullet for empty applies"
	_, res, err := Apply(context.Background(), runDir, ApplyInput{
		CWD:        cwd,
		Operations: []CuratorOperation{op},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Verdict != VerdictApplied {
		t.Fatalf("Verdict = %q, want applied; rejected=%+v", res.Verdict, res.Rejected)
	}
	body, _ := os.ReadFile(filepath.Join(playbookRootFromCWD(cwd), "failure-recovery.md"))
	if !strings.Contains(string(body), "**適用条件:** (なし)") {
		t.Errorf("placeholder missing in section: %s", body)
	}
}

// TestApplySectionAwareAppendCreatesNewSection: 観点 (g) wiki layout の不在 file から
// category file 初期化 + section 作成 + bullet 1 個 (Req 3.2, 3.3)。
func TestApplySectionAwareAppendCreatesNewSection(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	op := validOp()
	op.Topic = "new-topic"
	op.Entry = "first bullet of new topic"
	op.Summary = "test summary line"
	op.AppliesTo = "test applies condition"
	_, res, err := Apply(context.Background(), runDir, ApplyInput{
		CWD:        cwd,
		Operations: []CuratorOperation{op},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Verdict != VerdictApplied {
		t.Fatalf("Verdict = %q, want applied", res.Verdict)
	}
	want := "failure-recovery/new-topic"
	if len(res.NewTopicsCreated) != 1 || res.NewTopicsCreated[0] != want {
		t.Errorf("NewTopicsCreated = %v, want [%q]", res.NewTopicsCreated, want)
	}
	body, _ := os.ReadFile(filepath.Join(playbookRootFromCWD(cwd), "failure-recovery.md"))
	c := string(body)
	if !strings.Contains(c, "# failure-recovery\n") {
		t.Errorf("category heading missing: %s", c)
	}
	if !strings.Contains(c, "## new-topic\n") {
		t.Errorf("section heading missing: %s", c)
	}
	if !strings.Contains(c, "**適用条件:** test applies condition") {
		t.Errorf("applies_to missing: %s", c)
	}
	if !strings.Contains(c, "**summary:** test summary line") {
		t.Errorf("summary missing: %s", c)
	}
	if !strings.Contains(c, "- first bullet of new topic\n") {
		t.Errorf("bullet missing: %s", c)
	}
}

// TestApplyRebuildsIndexAfterOperations: 観点 (o) Apply 後に index.md が生成され、
// navigation 説明 + 4 category section + 該当 topic 行を含む (Req 4.1, 4.2, 4.4)。
func TestApplyRebuildsIndexAfterOperations(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	op := validOp()
	op.Category = "tool-pattern"
	op.Topic = "indexed-topic"
	op.Entry = "indexed topic bullet"
	op.Summary = "indexed topic short summary"
	_, res, err := Apply(context.Background(), runDir, ApplyInput{
		CWD:        cwd,
		Operations: []CuratorOperation{op},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Verdict != VerdictApplied {
		t.Fatalf("Verdict = %q, want applied", res.Verdict)
	}
	indexBody, ierr := os.ReadFile(filepath.Join(playbookRootFromCWD(cwd), "index.md"))
	if ierr != nil {
		t.Fatalf("read index.md: %v", ierr)
	}
	c := string(indexBody)
	if !strings.Contains(c, "# Playbook Index") {
		t.Errorf("index header missing: %s", c)
	}
	// 4 category section 全て存在
	for _, cat := range []string{"failure-recovery", "antipattern", "success-procedure", "tool-pattern"} {
		if !strings.Contains(c, "## "+cat+"\n") {
			t.Errorf("index section %q missing: %s", cat, c)
		}
	}
	// 該当 topic 行 (summary 付き) が tool-pattern section に書かれる
	if !strings.Contains(c, "- [indexed-topic](./tool-pattern.md#indexed-topic) — indexed topic short summary") {
		t.Errorf("indexed-topic line missing: %s", c)
	}
}

// TestApplyIndexWarningsEmptyOnHappyPath: 観点 (p) 正常 path では IndexWarnings は空 / nil で
// 副作用なし。RebuildIndex の per-category warning 伝搬経路 (parse-failed / read-failed) は
// wiki_index_test.go で網羅されているため、apply_test.go 側では「正常 path で warning が
// 伝搬しないこと」を不変式として固定する。
func TestApplyIndexWarningsEmptyOnHappyPath(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	op := validOp()
	op.Entry = "happy path bullet"
	_, res, err := Apply(context.Background(), runDir, ApplyInput{
		CWD:        cwd,
		Operations: []CuratorOperation{op},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Verdict != VerdictApplied {
		t.Fatalf("Verdict = %q, want applied", res.Verdict)
	}
	if len(res.IndexWarnings) != 0 {
		t.Errorf("IndexWarnings = %+v, want empty on happy path", res.IndexWarnings)
	}
}

// TestApplyDedupSkipUsesSectionBullets: 既存 wiki section の bullet と類似 entry → dedup skip。
func TestApplyDedupSkipUsesSectionBullets(t *testing.T) {
	cwd := t.TempDir()
	runDir := t.TempDir()
	root := playbookRootFromCWD(cwd)
	// 既存 wiki section に bullet を仕込む
	body := "# failure-recovery\n\nintro\n\n## existing-topic\n**適用条件:** 既存条件\n**summary:** 既存サマリ\n\n- this is a very specific dedup target bullet\n"
	writeCategoryFile(t, root, "failure-recovery", body)

	op := validOp()
	op.Topic = "another-topic"
	op.Entry = "this is a very specific dedup target bullet"
	_, res, err := Apply(context.Background(), runDir, ApplyInput{
		CWD:        cwd,
		Operations: []CuratorOperation{op},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.Deduped) != 1 {
		t.Fatalf("Deduped = %+v, want 1 entry", res.Deduped)
	}
	if len(res.Appended) != 0 {
		t.Errorf("Appended = %+v, want empty", res.Appended)
	}
}
