package learnings

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseCategoryFile_NilBody は nil 入力時に error を返すことを確認する。
// design.md "Preconditions: body != nil" 不変式のカバレッジ。
func TestParseCategoryFile_NilBody(t *testing.T) {
	t.Parallel()

	_, err := ParseCategoryFile(nil)
	if err == nil {
		t.Fatalf("ParseCategoryFile(nil) = nil error, want non-nil")
	}
}

// TestParseCategoryFile_Empty は空 body (長さ 0) を渡したときに、
// Header が空文字、Sections が空 slice として返ることを確認する。
// migration 開始前の空 file を accept するための挙動。
func TestParseCategoryFile_Empty(t *testing.T) {
	t.Parallel()

	got, err := ParseCategoryFile([]byte(""))
	if err != nil {
		t.Fatalf("ParseCategoryFile([]byte{}) error = %v, want nil", err)
	}
	if got.Header != "" {
		t.Fatalf("Header = %q, want empty", got.Header)
	}
	if len(got.Sections) != 0 {
		t.Fatalf("len(Sections) = %d, want 0", len(got.Sections))
	}
}

// TestParseCategoryFile_HeaderOnly は category file の先頭 `# <category>` heading
// と概要のみで section が無い状態を parse できることを確認する。
// 新規作成直後 (template 書き込み直後 / migration 前) の有効状態。
func TestParseCategoryFile_HeaderOnly(t *testing.T) {
	t.Parallel()

	body := "# antipattern\n\n二度とやってはならない手筋。\n"
	got, err := ParseCategoryFile([]byte(body))
	if err != nil {
		t.Fatalf("ParseCategoryFile error = %v, want nil", err)
	}
	if got.Header != body {
		t.Fatalf("Header = %q, want %q", got.Header, body)
	}
	if len(got.Sections) != 0 {
		t.Fatalf("len(Sections) = %d, want 0", len(got.Sections))
	}
}

// TestParseCategoryFile_SingleSection は heading + applies_to + summary + bullets
// を持つ標準 section が正しく分解されることを確認する。
// design.md L375-381 の section 構造仕様 (## <topic> / **適用条件:** / **summary:** / 空行 / bullets) に対応。
func TestParseCategoryFile_SingleSection(t *testing.T) {
	t.Parallel()

	body := strings.Join([]string{
		"# antipattern",
		"",
		"二度とやってはならない手筋。",
		"",
		"## hook-gate",
		"**適用条件:** PreToolUse hook で Edit/MultiEdit/Write を gate する shell script を書くとき",
		"**summary:** hook gate は全 payload shape と全マッチ行を AND 判定する",
		"",
		"- shell hook で複数 item を一括更新する場合 ...",
		"- PreToolUse hook で Edit/MultiEdit/Write を gate するとき ...",
		"",
	}, "\n")

	got, err := ParseCategoryFile([]byte(body))
	if err != nil {
		t.Fatalf("ParseCategoryFile error = %v, want nil", err)
	}
	if got.Header == "" {
		t.Fatalf("Header is empty, want non-empty preamble")
	}
	if strings.Contains(got.Header, "## hook-gate") {
		t.Fatalf("Header should not include ## section line, got %q", got.Header)
	}
	if len(got.Sections) != 1 {
		t.Fatalf("len(Sections) = %d, want 1", len(got.Sections))
	}
	s := got.Sections[0]
	if s.Topic != "hook-gate" {
		t.Fatalf("Topic = %q, want %q", s.Topic, "hook-gate")
	}
	if !strings.Contains(s.AppliesTo, "PreToolUse hook") {
		t.Fatalf("AppliesTo = %q, want substring %q", s.AppliesTo, "PreToolUse hook")
	}
	if !strings.Contains(s.Summary, "hook gate") {
		t.Fatalf("Summary = %q, want substring %q", s.Summary, "hook gate")
	}
	if len(s.Bullets) != 2 {
		t.Fatalf("len(Bullets) = %d, want 2; got %v", len(s.Bullets), s.Bullets)
	}
	if !strings.HasPrefix(s.Bullets[0], "shell hook") {
		t.Fatalf("Bullets[0] = %q, want prefix %q (without '- ')", s.Bullets[0], "shell hook")
	}
	if !strings.HasPrefix(s.Bullets[1], "PreToolUse hook") {
		t.Fatalf("Bullets[1] = %q, want prefix %q (without '- ')", s.Bullets[1], "PreToolUse hook")
	}
}

// TestParseCategoryFile_MultipleSections は複数 section が出現順を保って分解されることを確認する。
// design.md L295 "戻り値 ParsedCategory.Sections は file 出現順を保持する" の Postcondition カバレッジ。
func TestParseCategoryFile_MultipleSections(t *testing.T) {
	t.Parallel()

	body := strings.Join([]string{
		"# tool-pattern",
		"",
		"特定ツールの効率的な使い方。",
		"",
		"## alpha",
		"**適用条件:** 条件α",
		"**summary:** 要約α",
		"",
		"- bullet a1",
		"- bullet a2",
		"",
		"## beta",
		"**適用条件:** 条件β",
		"**summary:** 要約β",
		"",
		"- bullet b1",
		"",
		"## gamma",
		"**適用条件:** 条件γ",
		"**summary:** 要約γ",
		"",
		"- bullet c1",
		"- bullet c2",
		"- bullet c3",
		"",
	}, "\n")

	got, err := ParseCategoryFile([]byte(body))
	if err != nil {
		t.Fatalf("ParseCategoryFile error = %v, want nil", err)
	}
	if len(got.Sections) != 3 {
		t.Fatalf("len(Sections) = %d, want 3", len(got.Sections))
	}
	wantTopics := []string{"alpha", "beta", "gamma"}
	wantBulletCount := []int{2, 1, 3}
	for i, want := range wantTopics {
		if got.Sections[i].Topic != want {
			t.Fatalf("Sections[%d].Topic = %q, want %q", i, got.Sections[i].Topic, want)
		}
		if len(got.Sections[i].Bullets) != wantBulletCount[i] {
			t.Fatalf("len(Sections[%d].Bullets) = %d, want %d", i, len(got.Sections[i].Bullets), wantBulletCount[i])
		}
	}
	// 1 件目の bullet 内容を spot-check
	if got.Sections[0].Bullets[0] != "bullet a1" {
		t.Fatalf("Sections[0].Bullets[0] = %q, want %q", got.Sections[0].Bullets[0], "bullet a1")
	}
}

// TestParseCategoryFile_MalformedNoAppliesTo は適用条件行 / summary 行が無い malformed section が
// parse error にならず、AppliesTo="" / Summary="" の section として返されることを確認する。
// design.md L260 "不正な構造 (適用条件行が無い section) は parse error にせず、
// 適用条件行を空文字として section を返す (migration 中の中間状態許容)" のカバレッジ。
func TestParseCategoryFile_MalformedNoAppliesTo(t *testing.T) {
	t.Parallel()

	body := strings.Join([]string{
		"# antipattern",
		"",
		"## malformed",
		"",
		"- bullet only",
		"",
	}, "\n")

	got, err := ParseCategoryFile([]byte(body))
	if err != nil {
		t.Fatalf("ParseCategoryFile error = %v, want nil", err)
	}
	if len(got.Sections) != 1 {
		t.Fatalf("len(Sections) = %d, want 1", len(got.Sections))
	}
	s := got.Sections[0]
	if s.Topic != "malformed" {
		t.Fatalf("Topic = %q, want %q", s.Topic, "malformed")
	}
	if s.AppliesTo != "" {
		t.Fatalf("AppliesTo = %q, want empty (malformed)", s.AppliesTo)
	}
	if s.Summary != "" {
		t.Fatalf("Summary = %q, want empty (malformed)", s.Summary)
	}
	if len(s.Bullets) != 1 || s.Bullets[0] != "bullet only" {
		t.Fatalf("Bullets = %v, want [\"bullet only\"]", s.Bullets)
	}
}

// TestExtractBullets_FlattenSections は複数 section の bullets が出現順を保って平坦化されることを確認する。
// Requirement 5.1 (dedup pool は全 section の全 bullet) のカバレッジ。
func TestExtractBullets_FlattenSections(t *testing.T) {
	t.Parallel()

	p := ParsedCategory{
		Sections: []Section{
			{Topic: "a", Bullets: []string{"a-1", "a-2"}},
			{Topic: "b", Bullets: []string{"b-1"}},
			{Topic: "c", Bullets: []string{"c-1", "c-2", "c-3"}},
		},
	}
	got := ExtractBullets(p)
	want := []string{"a-1", "a-2", "b-1", "c-1", "c-2", "c-3"}
	if len(got) != len(want) {
		t.Fatalf("len(ExtractBullets) = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ExtractBullets[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestExtractBullets_Empty は section ゼロの ParsedCategory に対して非 nil の empty slice を返すことを確認する。
// 呼び出し側 (Apply phase の dedup pool 構築) が nil チェック不要で len()/range できるよう [] string{} を返す契約。
func TestExtractBullets_Empty(t *testing.T) {
	t.Parallel()

	got := ExtractBullets(ParsedCategory{})
	if got == nil {
		t.Fatalf("ExtractBullets(empty) = nil, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
}

// TestExtractBullets_ExcludesHeadingAndMetaLines は ParseCategoryFile → ExtractBullets の経路で
// `## heading` / `**適用条件:**` / `**summary:**` / 空行が bullet として抽出されないことを確認する。
// Requirement 5.2 (heading + applies_to 除外) + design L258 (summary 行も除外) のカバレッジ。
func TestExtractBullets_ExcludesHeadingAndMetaLines(t *testing.T) {
	t.Parallel()

	body := strings.Join([]string{
		"# antipattern",
		"",
		"概要文",
		"",
		"## hook-gate",
		"**適用条件:** AAA",
		"**summary:** BBB",
		"",
		"- bullet one",
		"- bullet two",
		"",
		"## other",
		"**適用条件:** CCC",
		"**summary:** DDD",
		"",
		"- bullet three",
		"",
	}, "\n")

	p, err := ParseCategoryFile([]byte(body))
	if err != nil {
		t.Fatalf("ParseCategoryFile error = %v", err)
	}
	bullets := ExtractBullets(p)

	want := []string{"bullet one", "bullet two", "bullet three"}
	if len(bullets) != len(want) {
		t.Fatalf("len(bullets) = %d, want %d; got=%v", len(bullets), len(want), bullets)
	}
	for i, w := range want {
		if bullets[i] != w {
			t.Fatalf("bullets[%d] = %q, want %q", i, bullets[i], w)
		}
	}

	// 除外すべき文字列が混入していないかを念のため明示 assert
	for _, b := range bullets {
		if strings.HasPrefix(b, "## ") {
			t.Fatalf("bullet contains heading line: %q", b)
		}
		if strings.HasPrefix(b, "**適用条件:**") {
			t.Fatalf("bullet contains applies-to line: %q", b)
		}
		if strings.HasPrefix(b, "**summary:**") {
			t.Fatalf("bullet contains summary line: %q", b)
		}
		if b == "" {
			t.Fatalf("bullet is empty string")
		}
	}
}

// TestCategoryHeaderTemplates_ContainsHeadingAndIntro は 4 category 全てに対して
// `# <category>` heading と 1-3 行の非空 intro が定義されていることを確認する。
// Requirement 1.3 (各 category file の `# <category-name>` heading と 1-3 行概要) + design.md L323-328 のカバレッジ。
func TestCategoryHeaderTemplates_ContainsHeadingAndIntro(t *testing.T) {
	t.Parallel()

	cats := []string{
		CategoryFailureRecovery,
		CategorySuccessProcedure,
		CategoryAntipattern,
		CategoryToolPattern,
	}

	for _, cat := range cats {
		t.Run(cat, func(t *testing.T) {
			t.Parallel()
			tmpl, ok := CategoryHeaderTemplateFor(cat)
			if !ok {
				t.Fatalf("CategoryHeaderTemplateFor(%q) = (_, false), want true", cat)
			}
			wantHeading := "# " + cat
			if tmpl.Heading != wantHeading {
				t.Fatalf("Heading = %q, want %q", tmpl.Heading, wantHeading)
			}
			if strings.TrimSpace(tmpl.Intro) == "" {
				t.Fatalf("Intro is empty for category %q", cat)
			}
			// 1-3 行制約
			intro := strings.TrimRight(tmpl.Intro, "\n")
			lines := strings.Split(intro, "\n")
			if n := len(lines); n < 1 || n > 3 {
				t.Fatalf("Intro line count = %d, want 1..3 (intro=%q)", n, tmpl.Intro)
			}
		})
	}
}

// TestCategoryHeaderTemplateFor_UnknownReturnsFalse は 4 値 closed enum 外の category 名に対して
// `(zero, false)` を返すことを確認する。Apply phase が新規 file 作成時に template lookup と enum 検証を
// 同じ呼び出しで行えるようにするための契約。
func TestCategoryHeaderTemplateFor_UnknownReturnsFalse(t *testing.T) {
	t.Parallel()

	_, ok := CategoryHeaderTemplateFor("unknown")
	if ok {
		t.Fatalf("CategoryHeaderTemplateFor(unknown) ok = true, want false")
	}
	_, ok = CategoryHeaderTemplateFor("")
	if ok {
		t.Fatalf("CategoryHeaderTemplateFor(empty) ok = true, want false")
	}
}

// TestParseCategoryFile_DoesNotMutateInput は入力 body slice を破壊的に変更しないことを確認する。
// design.md L296 "section parser は file 内容を破壊的に変更しない (read-only)" の Invariant カバレッジ。
func TestParseCategoryFile_DoesNotMutateInput(t *testing.T) {
	t.Parallel()

	body := []byte("# antipattern\n\n概要\n\n## topic\n**適用条件:** AT\n**summary:** SM\n\n- b1\n")
	original := make([]byte, len(body))
	copy(original, body)

	_, err := ParseCategoryFile(body)
	if err != nil {
		t.Fatalf("ParseCategoryFile error = %v", err)
	}
	if !bytesEqual(body, original) {
		t.Fatalf("ParseCategoryFile mutated input body")
	}
}

// TestParseCategoryFile_NilBody_ErrorMessageMentionsNil は nil body error が呼び出し側に分かるよう
// "nil" 文字列を error message に含むことを確認する。
// 後段 Apply phase の error wrapping が原因型を判別しやすくするための diagnostic 契約。
func TestParseCategoryFile_NilBody_ErrorMessageMentionsNil(t *testing.T) {
	t.Parallel()

	_, err := ParseCategoryFile(nil)
	if err == nil {
		t.Fatalf("ParseCategoryFile(nil) error = nil, want non-nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "nil") {
		t.Fatalf("error message = %q, want substring %q", err.Error(), "nil")
	}
	// errors.Is / errors.As を将来 wrap で使えるよう、最低 sentinel 経路を経由していること
	// (現時点では Unwrap の有無は気にせず error が非 nil で nil 文字列を含むだけで OK)
	if errors.Is(err, errors.New("")) {
		t.Fatalf("unexpected sentinel equality")
	}
}

// bytesEqual は []byte の中身を順番比較するローカル helper。
// 標準 bytes.Equal を import せずに同梱 (テスト helper の依存最小化方針)。
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// readCategoryFile はテスト用の小さな file 読み出し helper。
// AppendBullet 呼び出し後の file 全文を string で返す (改行コードは保持)。
func readCategoryFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	return string(b)
}

// TestAppendBullet_NewCategoryFile_CreatesTemplate は category file 不在のとき、
// CategoryHeaderTemplateFor の Heading + Intro でファイルが初期化された上に新規 section + bullet が
// 追記されることを確認する (Req 3.3 / 3.2)。
func TestAppendBullet_NewCategoryFile_CreatesTemplate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "antipattern.md")

	in := AppendBulletInput{
		CategoryFilePath: path,
		Category:         CategoryAntipattern,
		Topic:            "hook-gate",
		AppliesTo:        "PreToolUse hook で gate するとき",
		Summary:          "hook gate は全 payload shape と全マッチ行を AND 判定する",
		Entry:            "shell hook で複数 item を一括更新する場合 ...",
	}
	created, err := AppendBullet(in)
	if err != nil {
		t.Fatalf("AppendBullet error = %v, want nil", err)
	}
	if !created {
		t.Fatalf("createdNewSection = false, want true (file は不在から新規作成された)")
	}

	body := readCategoryFile(t, path)
	tmpl, _ := CategoryHeaderTemplateFor(CategoryAntipattern)
	if !strings.HasPrefix(body, tmpl.Heading+"\n") {
		t.Fatalf("body は %q で始まるべき; got=%q", tmpl.Heading, body)
	}
	if !strings.Contains(body, tmpl.Intro) {
		t.Fatalf("body は intro %q を含むべき; got=%q", tmpl.Intro, body)
	}
	if !strings.Contains(body, "## hook-gate") {
		t.Fatalf("body は section heading `## hook-gate` を含むべき; got=%q", body)
	}
	if !strings.Contains(body, "**適用条件:** PreToolUse hook で gate するとき") {
		t.Fatalf("body は applies_to 行を含むべき; got=%q", body)
	}
	if !strings.Contains(body, "**summary:** hook gate は全 payload shape と全マッチ行を AND 判定する") {
		t.Fatalf("body は summary 行を含むべき; got=%q", body)
	}
	if !strings.Contains(body, "- shell hook で複数 item を一括更新する場合 ...") {
		t.Fatalf("body は bullet を含むべき; got=%q", body)
	}
}

// TestAppendBullet_NewSection_AppendsAtEnd は既存 category file (template のみ) に新 topic の
// section が末尾 append されることを確認する (Req 3.2)。
func TestAppendBullet_NewSection_AppendsAtEnd(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "tool-pattern.md")

	// 既存 file: template のみ + 1 つ既存 section 持ち
	initial := strings.Join([]string{
		"# " + CategoryToolPattern,
		"",
		"特定ツールの効率的な使い方。",
		"",
		"## alpha",
		"**適用条件:** 条件α",
		"**summary:** 要約α",
		"",
		"- bullet a1",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatalf("os.WriteFile error = %v", err)
	}

	in := AppendBulletInput{
		CategoryFilePath: path,
		Category:         CategoryToolPattern,
		Topic:            "beta",
		AppliesTo:        "条件β",
		Summary:          "要約β",
		Entry:            "bullet b1",
	}
	created, err := AppendBullet(in)
	if err != nil {
		t.Fatalf("AppendBullet error = %v, want nil", err)
	}
	if !created {
		t.Fatalf("createdNewSection = false, want true (新規 topic)")
	}

	body := readCategoryFile(t, path)
	// alpha が beta より先に出現
	idxAlpha := strings.Index(body, "## alpha")
	idxBeta := strings.Index(body, "## beta")
	if idxAlpha < 0 || idxBeta < 0 {
		t.Fatalf("section が見つからない; alpha=%d beta=%d body=%q", idxAlpha, idxBeta, body)
	}
	if idxAlpha > idxBeta {
		t.Fatalf("section 順序が崩れた: alpha=%d beta=%d (alpha が先のはず)", idxAlpha, idxBeta)
	}
	if !strings.Contains(body, "**適用条件:** 条件β") {
		t.Fatalf("beta section に applies_to が無い; got=%q", body)
	}
	if !strings.Contains(body, "**summary:** 要約β") {
		t.Fatalf("beta section に summary が無い; got=%q", body)
	}
	if !strings.Contains(body, "- bullet b1") {
		t.Fatalf("beta section に bullet が無い; got=%q", body)
	}
}

// TestAppendBullet_ExistingSection_AppendsBulletInsideSection は既存 section へ
// bullet を追加した際に section 内の bullet 末尾 (次 section heading 直前 or EOF 直前) に追加され、
// applies_to / summary 行は不変であることを確認する (Req 3.1 + first-write-wins)。
func TestAppendBullet_ExistingSection_AppendsBulletInsideSection(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "antipattern.md")

	initial := strings.Join([]string{
		"# " + CategoryAntipattern,
		"",
		"二度とやってはならない手筋。",
		"",
		"## hook-gate",
		"**適用条件:** 初出 applies",
		"**summary:** 初出 summary",
		"",
		"- 既存 bullet 1",
		"",
		"## later-topic",
		"**適用条件:** 後続 applies",
		"**summary:** 後続 summary",
		"",
		"- 後続 bullet 1",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatalf("os.WriteFile error = %v", err)
	}

	in := AppendBulletInput{
		CategoryFilePath: path,
		Category:         CategoryAntipattern,
		Topic:            "hook-gate",
		AppliesTo:        "上書きされるべきでない applies",
		Summary:          "上書きされるべきでない summary",
		Entry:            "新規 bullet 2",
	}
	created, err := AppendBullet(in)
	if err != nil {
		t.Fatalf("AppendBullet error = %v, want nil", err)
	}
	if created {
		t.Fatalf("createdNewSection = true, want false (既存 section)")
	}

	body := readCategoryFile(t, path)

	// applies_to / summary は初出値を保持
	if !strings.Contains(body, "**適用条件:** 初出 applies") {
		t.Fatalf("applies_to が初出値で残っていない; got=%q", body)
	}
	if strings.Contains(body, "**適用条件:** 上書きされるべきでない applies") {
		t.Fatalf("applies_to が上書きされてしまった; got=%q", body)
	}
	if !strings.Contains(body, "**summary:** 初出 summary") {
		t.Fatalf("summary が初出値で残っていない; got=%q", body)
	}
	if strings.Contains(body, "**summary:** 上書きされるべきでない summary") {
		t.Fatalf("summary が上書きされてしまった; got=%q", body)
	}

	// 新 bullet が hook-gate section 内に挿入されている (later-topic の前)
	idxNew := strings.Index(body, "- 新規 bullet 2")
	idxOld := strings.Index(body, "- 既存 bullet 1")
	idxLater := strings.Index(body, "## later-topic")
	if idxNew < 0 {
		t.Fatalf("新 bullet が見つからない; got=%q", body)
	}
	if !(idxOld < idxNew && idxNew < idxLater) {
		t.Fatalf("新 bullet の位置が不正: idxOld=%d idxNew=%d idxLater=%d body=%q", idxOld, idxNew, idxLater, body)
	}
}

// TestAppendBullet_PreservesOrderOfPriorSections は前方の section に bullet 追記しても
// 後続 section の順序が変わらないことを確認する (Req 3.5)。
func TestAppendBullet_PreservesOrderOfPriorSections(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "tool-pattern.md")

	initial := strings.Join([]string{
		"# " + CategoryToolPattern,
		"",
		"概要",
		"",
		"## alpha",
		"**適用条件:** Aα",
		"**summary:** Sα",
		"",
		"- a1",
		"",
		"## beta",
		"**適用条件:** Aβ",
		"**summary:** Sβ",
		"",
		"- b1",
		"",
		"## gamma",
		"**適用条件:** Aγ",
		"**summary:** Sγ",
		"",
		"- c1",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatalf("os.WriteFile error = %v", err)
	}

	in := AppendBulletInput{
		CategoryFilePath: path,
		Category:         CategoryToolPattern,
		Topic:            "alpha",
		AppliesTo:        "(ignored)",
		Summary:          "(ignored)",
		Entry:            "a2",
	}
	if _, err := AppendBullet(in); err != nil {
		t.Fatalf("AppendBullet error = %v", err)
	}

	body := readCategoryFile(t, path)
	idxAlpha := strings.Index(body, "## alpha")
	idxBeta := strings.Index(body, "## beta")
	idxGamma := strings.Index(body, "## gamma")
	if !(idxAlpha < idxBeta && idxBeta < idxGamma) {
		t.Fatalf("section 順序が壊れた: alpha=%d beta=%d gamma=%d", idxAlpha, idxBeta, idxGamma)
	}

	parsed, err := ParseCategoryFile([]byte(body))
	if err != nil {
		t.Fatalf("ParseCategoryFile error = %v", err)
	}
	if len(parsed.Sections) != 3 {
		t.Fatalf("len(Sections) = %d, want 3", len(parsed.Sections))
	}
	// alpha section の bullets は [a1, a2] の順
	if len(parsed.Sections[0].Bullets) != 2 || parsed.Sections[0].Bullets[0] != "a1" || parsed.Sections[0].Bullets[1] != "a2" {
		t.Fatalf("alpha section bullets = %v, want [a1, a2]", parsed.Sections[0].Bullets)
	}
	// beta / gamma の bullets は変更なし
	if len(parsed.Sections[1].Bullets) != 1 || parsed.Sections[1].Bullets[0] != "b1" {
		t.Fatalf("beta bullets = %v, want [b1]", parsed.Sections[1].Bullets)
	}
	if len(parsed.Sections[2].Bullets) != 1 || parsed.Sections[2].Bullets[0] != "c1" {
		t.Fatalf("gamma bullets = %v, want [c1]", parsed.Sections[2].Bullets)
	}
}

// TestAppendBullet_PreservesPriorBulletOrder は同一 section 内に複数 bullet を追記したとき、
// 受領順に末尾へ append されることを確認する (Req 3.4 + 3.6)。
func TestAppendBullet_PreservesPriorBulletOrder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "antipattern.md")

	initial := strings.Join([]string{
		"# " + CategoryAntipattern,
		"",
		"概要",
		"",
		"## topic",
		"**適用条件:** A",
		"**summary:** S",
		"",
		"- original-1",
		"- original-2",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatalf("os.WriteFile error = %v", err)
	}

	for _, entry := range []string{"new-1", "new-2", "new-3"} {
		in := AppendBulletInput{
			CategoryFilePath: path,
			Category:         CategoryAntipattern,
			Topic:            "topic",
			AppliesTo:        "(ignored)",
			Summary:          "(ignored)",
			Entry:            entry,
		}
		if _, err := AppendBullet(in); err != nil {
			t.Fatalf("AppendBullet(%q) error = %v", entry, err)
		}
	}

	body := readCategoryFile(t, path)
	parsed, err := ParseCategoryFile([]byte(body))
	if err != nil {
		t.Fatalf("ParseCategoryFile error = %v", err)
	}
	if len(parsed.Sections) != 1 {
		t.Fatalf("len(Sections) = %d, want 1", len(parsed.Sections))
	}
	want := []string{"original-1", "original-2", "new-1", "new-2", "new-3"}
	got := parsed.Sections[0].Bullets
	if len(got) != len(want) {
		t.Fatalf("bullets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bullets[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestAppendBullet_FirstWriteWins_IgnoresLaterSummary は既存 section に異なる summary / applies_to を持つ
// input を渡しても section 内の applies_to / summary 行が初出値のまま固定であることを確認する。
func TestAppendBullet_FirstWriteWins_IgnoresLaterSummary(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "tool-pattern.md")

	// 1 回目: section を新規作成
	first := AppendBulletInput{
		CategoryFilePath: path,
		Category:         CategoryToolPattern,
		Topic:            "topic",
		AppliesTo:        "first-applies",
		Summary:          "first-summary",
		Entry:            "first-entry",
	}
	if _, err := AppendBullet(first); err != nil {
		t.Fatalf("first AppendBullet error = %v", err)
	}

	// 2 回目: 同 topic に異なる applies_to / summary を渡す → 無視されるべき
	second := AppendBulletInput{
		CategoryFilePath: path,
		Category:         CategoryToolPattern,
		Topic:            "topic",
		AppliesTo:        "second-applies",
		Summary:          "second-summary",
		Entry:            "second-entry",
	}
	created, err := AppendBullet(second)
	if err != nil {
		t.Fatalf("second AppendBullet error = %v", err)
	}
	if created {
		t.Fatalf("createdNewSection = true, want false (既存 section)")
	}

	body := readCategoryFile(t, path)
	if !strings.Contains(body, "**適用条件:** first-applies") {
		t.Fatalf("applies_to が初出値で固定されていない; got=%q", body)
	}
	if strings.Contains(body, "second-applies") {
		t.Fatalf("applies_to が後発値で上書きされている; got=%q", body)
	}
	if !strings.Contains(body, "**summary:** first-summary") {
		t.Fatalf("summary が初出値で固定されていない; got=%q", body)
	}
	if strings.Contains(body, "second-summary") {
		t.Fatalf("summary が後発値で上書きされている; got=%q", body)
	}
	// 両 bullet が共に存在
	if !strings.Contains(body, "- first-entry") {
		t.Fatalf("first entry が無い; got=%q", body)
	}
	if !strings.Contains(body, "- second-entry") {
		t.Fatalf("second entry が無い; got=%q", body)
	}
}

// TestAppendBullet_InvalidCategory_Error は 4 値 enum 外の category を渡したときに error を返すことを確認する。
func TestAppendBullet_InvalidCategory_Error(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "unknown.md")

	in := AppendBulletInput{
		CategoryFilePath: path,
		Category:         "unknown-category",
		Topic:            "topic",
		AppliesTo:        "A",
		Summary:          "S",
		Entry:            "E",
	}
	_, err := AppendBullet(in)
	if err == nil {
		t.Fatalf("AppendBullet error = nil, want non-nil for invalid category")
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("file が作成されてしまった; want not-exist, got err=%v", statErr)
	}
}

// TestAppendBullet_InvalidTopic_Error は kebab-case 規約に違反する topic 名で error を返すことを確認する。
func TestAppendBullet_InvalidTopic_Error(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "antipattern.md")

	cases := []string{
		"",
		"Topic",           // 大文字
		"-leading-hyphen", // hyphen 始まり
		"1leading-digit",  // 数字始まり
		"snake_case",      // underscore
		"日本語",
	}
	for _, topic := range cases {
		t.Run(topic, func(t *testing.T) {
			in := AppendBulletInput{
				CategoryFilePath: path,
				Category:         CategoryAntipattern,
				Topic:            topic,
				AppliesTo:        "A",
				Summary:          "S",
				Entry:            "E",
			}
			if _, err := AppendBullet(in); err == nil {
				t.Fatalf("AppendBullet(%q) error = nil, want non-nil", topic)
			}
		})
	}
}

// TestAppendBullet_AtomicWrite は AppendBullet 後に tmp file (`<path>.tmp`) が残らないことを確認する。
// writeFileAtomic 経由で書かれていれば rename 後に tmp は消えるはず。
func TestAppendBullet_AtomicWrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "antipattern.md")

	in := AppendBulletInput{
		CategoryFilePath: path,
		Category:         CategoryAntipattern,
		Topic:            "topic",
		AppliesTo:        "A",
		Summary:          "S",
		Entry:            "E",
	}
	if _, err := AppendBullet(in); err != nil {
		t.Fatalf("AppendBullet error = %v", err)
	}

	tmpPath := path + ".tmp"
	if _, err := os.Stat(tmpPath); err == nil {
		t.Fatalf("tmp file が残っている: %q", tmpPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(%q) error = %v, want os.ErrNotExist", tmpPath, err)
	}

	// perm が 0o644 であること
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	if got, want := st.Mode().Perm(), os.FileMode(0o644); got != want {
		t.Fatalf("perm = %v, want %v", got, want)
	}
}
