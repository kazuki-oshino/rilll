package learnings

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile はテスト用 helper。filepath.Join で path を構築し 0o644 で書き込む。
// テスト直書きの os.WriteFile + 0o644 boilerplate を吸収する。
func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", p, err)
	}
	return p
}

// readIndex はテスト用 helper。index.md を読み出して string で返す。
func readIndex(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "index.md"))
	if err != nil {
		t.Fatalf("ReadFile(index.md) error = %v", err)
	}
	return string(b)
}

// allCategoriesFixtureBody は 4 category file の標準的な内容を返す helper。
// 各 file は heading + intro + 1 つの section (topic + applies_to + summary + bullet) を持つ。
// この同じ body を複数 test で使い回す。
func categoryBody(cat, topic, summary string) string {
	tmpl, _ := CategoryHeaderTemplateFor(cat)
	return strings.Join([]string{
		tmpl.Heading,
		"",
		tmpl.Intro,
		"",
		"## " + topic,
		"**適用条件:** at-" + topic,
		"**summary:** " + summary,
		"",
		"- bullet of " + topic,
		"",
	}, "\n")
}

// TestRebuildIndex_AllCategoriesPopulated は 4 category 全てに section があるとき
// index.md に 4 category section と各 topic 行が出力されることを確認する (Req 4.1 / 4.2 / 4.4)。
func TestRebuildIndex_AllCategoriesPopulated(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "failure-recovery.md", categoryBody(CategoryFailureRecovery, "edit-tool", "Edit failure ノウハウ"))
	writeFile(t, root, "antipattern.md", categoryBody(CategoryAntipattern, "hook-gate", "hook gate antipattern"))
	writeFile(t, root, "success-procedure.md", categoryBody(CategorySuccessProcedure, "error-wrap", "error wrap procedure"))
	writeFile(t, root, "tool-pattern.md", categoryBody(CategoryToolPattern, "go-test", "go test cache pattern"))

	warnings, err := RebuildIndex(RebuildIndexInput{PlaybookRoot: root})
	if err != nil {
		t.Fatalf("RebuildIndex error = %v, want nil", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want []", warnings)
	}

	body := readIndex(t, root)

	// 4 category section heading が存在
	wantHeadings := []string{
		"## " + CategoryFailureRecovery,
		"## " + CategoryAntipattern,
		"## " + CategorySuccessProcedure,
		"## " + CategoryToolPattern,
	}
	for _, h := range wantHeadings {
		if !strings.Contains(body, h) {
			t.Fatalf("body は heading %q を含むべき; got=%q", h, body)
		}
	}

	// 各 topic link が想定 format で含まれる
	wantLinks := []string{
		"- [edit-tool](./failure-recovery.md#edit-tool) — Edit failure ノウハウ",
		"- [hook-gate](./antipattern.md#hook-gate) — hook gate antipattern",
		"- [error-wrap](./success-procedure.md#error-wrap) — error wrap procedure",
		"- [go-test](./tool-pattern.md#go-test) — go test cache pattern",
	}
	for _, link := range wantLinks {
		if !strings.Contains(body, link) {
			t.Fatalf("body は link %q を含むべき; got=%q", link, body)
		}
	}
}

// TestRebuildIndex_PartiallyEmptyCategories は一部 category file 不在 / 空 / section 無しのとき、
// 該当 category section は heading + intro のみで topic 行を持たず、warnings も発生しないことを確認する (Req 4.2)。
func TestRebuildIndex_PartiallyEmptyCategories(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// failure-recovery: 通常
	writeFile(t, root, "failure-recovery.md", categoryBody(CategoryFailureRecovery, "edit-tool", "Edit failure"))
	// antipattern: 不在 (ファイルを置かない)
	// success-procedure: 空 file
	writeFile(t, root, "success-procedure.md", "")
	// tool-pattern: heading のみ (section 無し)
	tmpl, _ := CategoryHeaderTemplateFor(CategoryToolPattern)
	writeFile(t, root, "tool-pattern.md", tmpl.Heading+"\n\n"+tmpl.Intro+"\n")

	warnings, err := RebuildIndex(RebuildIndexInput{PlaybookRoot: root})
	if err != nil {
		t.Fatalf("RebuildIndex error = %v, want nil", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want [] (不在/空は warning 対象外)", warnings)
	}

	body := readIndex(t, root)

	// 4 category 全 heading が存在
	for _, cat := range []string{CategoryFailureRecovery, CategoryAntipattern, CategorySuccessProcedure, CategoryToolPattern} {
		h := "## " + cat
		if !strings.Contains(body, h) {
			t.Fatalf("body は heading %q を含むべき; got=%q", h, body)
		}
	}
	// failure-recovery には topic link がある
	if !strings.Contains(body, "- [edit-tool](./failure-recovery.md#edit-tool)") {
		t.Fatalf("failure-recovery section に link が無い; got=%q", body)
	}
	// 他 category には topic link は出てこない
	for _, link := range []string{
		"](./antipattern.md#",
		"](./success-procedure.md#",
		"](./tool-pattern.md#",
	} {
		if strings.Contains(body, link) {
			t.Fatalf("空 category に link が出てしまった: %q; got=%q", link, body)
		}
	}
	// プレースホルダ `_(unreadable, skipped)_` は出ない (空・不在は warning 対象外)
	if strings.Contains(body, "_(unreadable, skipped)_") {
		t.Fatalf("空/不在 category にプレースホルダが出てはいけない; got=%q", body)
	}
}

// TestRebuildIndex_FaultTolerantParseFailed は 1 category file が ParseCategoryFile error を返す状況で
// warnings + `_(unreadable, skipped)_` プレースホルダが書き出され、他 category は正常に index 反映されることを確認する (design.md L367)。
// ParseCategoryFile は nil body 以外で error を返さないため、本テストでは直接 parse error を誘発できない。
// 代わりに scanner buffer overflow を狙う 2MB 1 行の category file で同様の経路を踏ませる。
func TestRebuildIndex_FaultTolerantParseFailed(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// failure-recovery: 通常 section
	writeFile(t, root, "failure-recovery.md", categoryBody(CategoryFailureRecovery, "edit-tool", "Edit failure"))
	// antipattern: 2MB 1 行で scanner buffer overflow → parse error
	// 改行を含まないため bufio.Scanner が token too long で err。
	bigLine := strings.Repeat("a", 2*1024*1024) // 2MB
	writeFile(t, root, "antipattern.md", bigLine)
	// success-procedure / tool-pattern: 通常
	writeFile(t, root, "success-procedure.md", categoryBody(CategorySuccessProcedure, "error-wrap", "error wrap"))
	writeFile(t, root, "tool-pattern.md", categoryBody(CategoryToolPattern, "go-test", "go test"))

	warnings, err := RebuildIndex(RebuildIndexInput{PlaybookRoot: root})
	if err != nil {
		t.Fatalf("RebuildIndex error = %v, want nil (per-category fault tolerance)", err)
	}

	// 1 warning が antipattern / parse-failed として発生
	if len(warnings) != 1 {
		t.Fatalf("len(warnings) = %d, want 1; got=%+v", len(warnings), warnings)
	}
	w := warnings[0]
	if w.Category != CategoryAntipattern {
		t.Fatalf("warning.Category = %q, want %q", w.Category, CategoryAntipattern)
	}
	if w.Reason != "parse-failed" {
		t.Fatalf("warning.Reason = %q, want %q", w.Reason, "parse-failed")
	}
	if w.Detail == "" {
		t.Fatalf("warning.Detail = empty, want non-empty error string")
	}

	body := readIndex(t, root)

	// antipattern section にプレースホルダ
	if !strings.Contains(body, "## "+CategoryAntipattern) {
		t.Fatalf("antipattern heading が無い; got=%q", body)
	}
	if !strings.Contains(body, "_(unreadable, skipped)_") {
		t.Fatalf("antipattern section にプレースホルダが無い; got=%q", body)
	}

	// 他 3 category の topic link は正常
	for _, link := range []string{
		"- [edit-tool](./failure-recovery.md#edit-tool) — Edit failure",
		"- [error-wrap](./success-procedure.md#error-wrap) — error wrap",
		"- [go-test](./tool-pattern.md#go-test) — go test",
	} {
		if !strings.Contains(body, link) {
			t.Fatalf("link %q が無い (他 category は維持されるべき); got=%q", link, body)
		}
	}
}

// TestRebuildIndex_FaultTolerantReadFailed は 1 category file が permission denied で read 不能のとき、
// warnings に `read-failed` + プレースホルダ書き出し、他 category は正常 (Req 4.x per-category fault tolerance)。
func TestRebuildIndex_FaultTolerantReadFailed(t *testing.T) {
	t.Parallel()

	// root が permission-denied になっていると本 test の前提が崩れる
	if os.Geteuid() == 0 {
		t.Skip("root は file mode を bypass するため skip")
	}

	root := t.TempDir()
	writeFile(t, root, "failure-recovery.md", categoryBody(CategoryFailureRecovery, "edit-tool", "Edit failure"))
	apPath := writeFile(t, root, "antipattern.md", categoryBody(CategoryAntipattern, "hook-gate", "hook"))
	writeFile(t, root, "success-procedure.md", categoryBody(CategorySuccessProcedure, "error-wrap", "wrap"))
	writeFile(t, root, "tool-pattern.md", categoryBody(CategoryToolPattern, "go-test", "go test"))

	// antipattern.md を read 不能に
	if err := os.Chmod(apPath, 0o000); err != nil {
		t.Fatalf("Chmod(0o000) error = %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(apPath, 0o644)
	})

	warnings, err := RebuildIndex(RebuildIndexInput{PlaybookRoot: root})
	if err != nil {
		t.Fatalf("RebuildIndex error = %v, want nil (per-category fault tolerance)", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("len(warnings) = %d, want 1; got=%+v", len(warnings), warnings)
	}
	w := warnings[0]
	if w.Category != CategoryAntipattern {
		t.Fatalf("warning.Category = %q, want %q", w.Category, CategoryAntipattern)
	}
	if w.Reason != "read-failed" {
		t.Fatalf("warning.Reason = %q, want %q", w.Reason, "read-failed")
	}
	if w.Detail == "" {
		t.Fatalf("warning.Detail = empty, want non-empty")
	}

	body := readIndex(t, root)
	if !strings.Contains(body, "## "+CategoryAntipattern) {
		t.Fatalf("antipattern heading が無い; got=%q", body)
	}
	if !strings.Contains(body, "_(unreadable, skipped)_") {
		t.Fatalf("antipattern section にプレースホルダが無い; got=%q", body)
	}
	// 他 3 category は正常
	if !strings.Contains(body, "- [edit-tool](./failure-recovery.md#edit-tool)") {
		t.Fatalf("failure-recovery link が無い; got=%q", body)
	}
	if !strings.Contains(body, "- [error-wrap](./success-procedure.md#error-wrap)") {
		t.Fatalf("success-procedure link が無い; got=%q", body)
	}
	if !strings.Contains(body, "- [go-test](./tool-pattern.md#go-test)") {
		t.Fatalf("tool-pattern link が無い; got=%q", body)
	}
}

// TestRebuildIndex_TopicOrderPreserved は 1 category 内の topic 順序が
// file 出現順を踏襲することを確認する (design.md L366)。
func TestRebuildIndex_TopicOrderPreserved(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	tmpl, _ := CategoryHeaderTemplateFor(CategoryAntipattern)
	body := strings.Join([]string{
		tmpl.Heading,
		"",
		tmpl.Intro,
		"",
		"## charlie",
		"**適用条件:** ac",
		"**summary:** sum-charlie",
		"",
		"- bc",
		"",
		"## alpha",
		"**適用条件:** aa",
		"**summary:** sum-alpha",
		"",
		"- ba",
		"",
		"## bravo",
		"**適用条件:** ab",
		"**summary:** sum-bravo",
		"",
		"- bb",
		"",
	}, "\n")
	writeFile(t, root, "antipattern.md", body)
	// 他 3 category は存在しない (空 section) でも OK

	warnings, err := RebuildIndex(RebuildIndexInput{PlaybookRoot: root})
	if err != nil {
		t.Fatalf("RebuildIndex error = %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want []", warnings)
	}

	got := readIndex(t, root)

	// charlie → alpha → bravo の順で出現する
	idxCharlie := strings.Index(got, "- [charlie](./antipattern.md#charlie)")
	idxAlpha := strings.Index(got, "- [alpha](./antipattern.md#alpha)")
	idxBravo := strings.Index(got, "- [bravo](./antipattern.md#bravo)")
	if idxCharlie < 0 || idxAlpha < 0 || idxBravo < 0 {
		t.Fatalf("topic link が見つからない: charlie=%d alpha=%d bravo=%d", idxCharlie, idxAlpha, idxBravo)
	}
	if !(idxCharlie < idxAlpha && idxAlpha < idxBravo) {
		t.Fatalf("topic 順序が file 出現順を踏襲していない: charlie=%d alpha=%d bravo=%d", idxCharlie, idxAlpha, idxBravo)
	}
}

// TestRebuildIndex_NoFrontmatter は出力に YAML frontmatter が含まれないことを確認する (Req 4.6)。
func TestRebuildIndex_NoFrontmatter(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "failure-recovery.md", categoryBody(CategoryFailureRecovery, "edit-tool", "Edit"))

	if _, err := RebuildIndex(RebuildIndexInput{PlaybookRoot: root}); err != nil {
		t.Fatalf("RebuildIndex error = %v", err)
	}

	body := readIndex(t, root)
	// 冒頭が `---\n` で始まらない
	if strings.HasPrefix(body, "---\n") {
		head := body
		if len(head) > 32 {
			head = head[:32]
		}
		t.Fatalf("index に YAML frontmatter が含まれている; got prefix=%q", head)
	}
	// 本文中にも `---\n` で挟まれる block が無い (簡易チェック)
	if strings.Contains(body, "\n---\n") {
		t.Fatalf("index に frontmatter delimiter `---` が混入している; got=%q", body)
	}
}

// TestRebuildIndex_NavigationLineIncluded は冒頭 2-3 行に navigation 説明が含まれることを確認する (Req 4.7)。
func TestRebuildIndex_NavigationLineIncluded(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "failure-recovery.md", categoryBody(CategoryFailureRecovery, "edit-tool", "Edit"))

	if _, err := RebuildIndex(RebuildIndexInput{PlaybookRoot: root}); err != nil {
		t.Fatalf("RebuildIndex error = %v", err)
	}

	body := readIndex(t, root)
	// 「ファイル編集前にこの index を読み」相当の navigation 文言が含まれる
	if !strings.Contains(body, "ファイル編集前に") {
		t.Fatalf("navigation 文言が無い; got=%q", body)
	}
	if !strings.Contains(body, "**適用条件:**") {
		t.Fatalf("navigation 説明に `**適用条件:**` 参照が無い; got=%q", body)
	}
	// navigation 説明は 1 つ目の category heading よりも前に置かれる
	idxNav := strings.Index(body, "ファイル編集前に")
	idxFirstCat := strings.Index(body, "## "+CategoryFailureRecovery)
	if idxNav < 0 || idxFirstCat < 0 || idxNav > idxFirstCat {
		t.Fatalf("navigation 文言は冒頭に置かれるべき; idxNav=%d idxFirstCat=%d", idxNav, idxFirstCat)
	}
}

// TestRebuildIndex_CategoryOrderFixed は 4 category 順序が
// failure-recovery → antipattern → success-procedure → tool-pattern であることを確認する (Req 4.2)。
func TestRebuildIndex_CategoryOrderFixed(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for _, cat := range []string{
		CategoryFailureRecovery,
		CategoryAntipattern,
		CategorySuccessProcedure,
		CategoryToolPattern,
	} {
		writeFile(t, root, cat+".md", categoryBody(cat, "x", "s"))
	}

	if _, err := RebuildIndex(RebuildIndexInput{PlaybookRoot: root}); err != nil {
		t.Fatalf("RebuildIndex error = %v", err)
	}

	body := readIndex(t, root)
	idxFR := strings.Index(body, "## "+CategoryFailureRecovery)
	idxAP := strings.Index(body, "## "+CategoryAntipattern)
	idxSP := strings.Index(body, "## "+CategorySuccessProcedure)
	idxTP := strings.Index(body, "## "+CategoryToolPattern)
	if !(idxFR >= 0 && idxAP > idxFR && idxSP > idxAP && idxTP > idxSP) {
		t.Fatalf("category 順序が想定外: fr=%d ap=%d sp=%d tp=%d", idxFR, idxAP, idxSP, idxTP)
	}
}

// TestRebuildIndex_AtomicWrite は writeFileAtomic 経由で書かれていることを確認する (rename 後の tmp 残骸 0)。
func TestRebuildIndex_AtomicWrite(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "failure-recovery.md", categoryBody(CategoryFailureRecovery, "edit-tool", "Edit"))

	if _, err := RebuildIndex(RebuildIndexInput{PlaybookRoot: root}); err != nil {
		t.Fatalf("RebuildIndex error = %v", err)
	}

	tmpPath := filepath.Join(root, "index.md.tmp")
	if _, err := os.Stat(tmpPath); err == nil {
		t.Fatalf("tmp file が残っている: %q", tmpPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(%q) error = %v, want os.ErrNotExist", tmpPath, err)
	}

	st, err := os.Stat(filepath.Join(root, "index.md"))
	if err != nil {
		t.Fatalf("Stat(index.md) error = %v", err)
	}
	if got, want := st.Mode().Perm(), os.FileMode(0o644); got != want {
		t.Fatalf("perm = %v, want %v", got, want)
	}
}

// TestRebuildIndex_IndexWriteFailure_ErrorReturn は index.md 自体の write 失敗で
// error が返ることを確認する (design.md L367-368)。
// PlaybookRoot を存在しない directory に指定して、tmp 書き込みを失敗させる。
func TestRebuildIndex_IndexWriteFailure_ErrorReturn(t *testing.T) {
	t.Parallel()

	// 存在しない directory
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := RebuildIndex(RebuildIndexInput{PlaybookRoot: missing})
	if err == nil {
		t.Fatalf("RebuildIndex error = nil, want non-nil for missing PlaybookRoot")
	}
}
