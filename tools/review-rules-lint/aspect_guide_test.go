// Package reviewruleslint - aspect_guide_test.go は v2 仕様で新設された
// 観点合成ガイド (`references/aspects/<aspect>.md`) が以下の契約を満たすことを
// authoring-time に検査する drift gate である。
//
// 本ファイルの責務 (Layer Separation 不変式 / Req 4.3 への参照):
//   - Req 4.3 (.kiro/specs/review-rules-skill-v2/requirements.md):
//     「`tools/review-rules-lint/aspect_guide_test.go` を新規作成し、
//     `references/aspects/<aspect>.md` の 8 ファイルが必須 5 セクション
//     (Req 2.2) を含むことを検査する」を Go test として実装する。
//   - Req 2.1 (8 観点ファイルの存在) / Req 2.2 (各ファイルが必須 5 セクション
//     を含むこと) を間接的に CI で守る。
//   - design.md § Verification Contract (v2) item 3:
//     「aspect_guide_test.go (新設): `references/aspects/<aspect>.md` 8
//     ファイルが必須 5 セクションを含むこと、対応テンプレと 1:1 対応する
//     こと、を検査する」を実装する。
//   - design.md § Catalog domain → 観点合成ガイドの必須 5 セクション
//     (順序固定) を SSoT として参照し、各セクション見出しを Markdown 見出し行
//     (`^## ` 始まり) として検出する。
//   - design.md § Aspect / Template 対応 SSoT 表 (8 行) を package-level
//     スライスとして 1 箇所に固定し、aspect ↔ template の 1:1 対応を
//     enumerate して assertion する。
//
// Layer Separation 不変式 (design.md § Layer Separation 不変式 / Req 5.1 / 5.2):
//   - 本 Go テストは rilll repo 内 (tools/review-rules-lint/) にのみ存在し、
//     skill 配布物 (.agents/skills/review-rules-skill/) には一切同梱しない。
//   - skill 側に Go / lint バイナリ依存を持ち込まない。
package reviewruleslint

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// aspectsDir は本テストパッケージから review-rules-skill の references/aspects/
// への相対パス。go test 実行時の作業ディレクトリは tools/review-rules-lint/ で
// あるため、二段上に遡ってから .agents/skills/... を辿る (templatesDir と
// 並行する位置づけ)。
const aspectsDir = "../../.agents/skills/review-rules-skill/references/aspects"

// expectedAspects は design.md § Aspect / Template 対応 SSoT 表 (8 行) の
// 「aspect 側 stem」を SSoT として保持する。Req 2.1 で要求される 8 観点に対応。
// templates/<aspect>-review.yaml と 1:1 対応し、aspect ファイル名は
// references/aspects/<aspect>.md となる。
var expectedAspects = []string{
	"security",
	"docs",
	"dependency",
	"test",
	"performance",
	"api-compatibility",
	"config-exposure",
	"todo-tracking",
}

// requiredAspectSection は観点合成ガイドの必須 5 セクション
// (design.md § Catalog domain → 観点合成ガイドの必須 5 セクション / Req 2.2)
// を順序固定で表現する。各エントリは:
//   - number: セクション番号 (1〜5、エラーメッセージで人が読みやすくするための識別子)
//   - title: セクション名 (人間可読、エラーメッセージ用)
//   - pattern: Markdown 見出し行 (`^## ` 始まり) にマッチする正規表現
//
// 見出しは既存 8 ファイルの実装と一致させる。section 3 / 4 は見出し内に
// バッククォート (`applies_when` / `prompt`) を含むため、リテラル正規表現で
// その揺らぎも明示的に固定する (本文中の `applies_when` 言及にマッチして
// 偽陽性となるのを避けるため、必ず `^## ` 始まりの行のみにマッチさせる)。
var requiredAspectSection = []struct {
	number  int
	title   string
	pattern *regexp.Regexp
}{
	{1, "観点の目的", regexp.MustCompile(`^##\s+1\.\s+観点の目的\s*$`)},
	{2, "Phase 1 で読むべき入力", regexp.MustCompile(`^##\s+2\.\s+Phase 1 で読むべき入力\s*$`)},
	{3, "`applies_when` の作り方", regexp.MustCompile("^##\\s+3\\.\\s+`applies_when` の作り方\\s*$")},
	{4, "`prompt` の作り方", regexp.MustCompile("^##\\s+4\\.\\s+`prompt` の作り方\\s*$")},
	{5, "合成後の自己点検項目", regexp.MustCompile(`^##\s+5\.\s+合成後の自己点検項目\s*$`)},
}

// listAspectFiles は references/aspects/ 配下の *.md を列挙する。
// サブディレクトリは設置されない前提のため、本テストでも直下のみ走査する。
func listAspectFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(aspectsDir)
	if err != nil {
		t.Fatalf("aspects ディレクトリを読めなかった: %s: %v", aspectsDir, err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		files = append(files, filepath.Join(aspectsDir, name))
	}
	if len(files) == 0 {
		t.Fatalf("references/aspects/ 配下に *.md が 1 件も見つからなかった: %s", aspectsDir)
	}
	return files
}

// TestAspectGuideRequiredSections は references/aspects/<aspect>.md の各
// ファイルが design.md § Catalog domain で定められた必須 5 セクションを
// すべて含むことを検査する (Req 2.2 / Req 4.3 / design.md § Verification
// Contract (v2) item 3)。
//
// 検査ロジック:
//  1. listAspectFiles で *.md を列挙。
//  2. 各ファイルを行単位に分割し、`^## ` 始まりの見出し行のみ走査する
//     (本文中の `applies_when` などへの偽陽性マッチを避けるため)。
//  3. requiredAspectSection を順番に検査し、対応見出しが 1 行も無い場合に
//     section 番号 / title / ファイル名を含む t.Errorf を発火させる。
//
// 期待動作:
//   - 出荷状態の 8 ファイル全てで PASS。
//   - 5 セクション中いずれかの見出しが欠落 / 改名 / コメントアウトされた場合に FAIL。
func TestAspectGuideRequiredSections(t *testing.T) {
	files := listAspectFiles(t)
	for _, path := range files {
		filename := filepath.Base(path)
		t.Run(filename, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s: 読み込み失敗: %v", filename, err)
			}
			// Markdown 見出しは行単位でしか定義されないため、行に分割して
			// `^## ` 始まりの行のみ走査する。
			lines := strings.Split(string(data), "\n")
			for _, sec := range requiredAspectSection {
				found := false
				for _, line := range lines {
					if !strings.HasPrefix(line, "## ") {
						continue
					}
					if sec.pattern.MatchString(line) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("%s: 必須セクション欠落 (Req 2.2 / design.md § Catalog domain): section %d %q の見出し行が見つからない",
						filename, sec.number, sec.title)
				}
			}
		})
	}
}

// TestAspectTemplateOneToOneMapping は design.md § Aspect / Template 対応
// SSoT 表に従い、references/aspects/*.md と templates/*-review.yaml が
// 1:1 で対応することを検査する (Req 4.3 / design.md § Verification Contract
// (v2) item 3)。
//
// 検査ロジック:
//  1. listAspectFiles と listTemplateFiles でそれぞれの実ファイル列を取得し、
//     stem 集合 (aspect: 拡張子除去, template: 拡張子と接尾辞 `-review` 除去)
//     を導出する。
//  2. expectedAspects (= SSoT 表) と完全一致することを確認する
//     (= 余計な追加・欠落・重複を一括検出)。
//  3. aspect 側と template 側で stem 集合が一致することも確認する
//     (= aspect ↔ template の 1:1 対応)。
//
// 期待動作:
//   - 出荷状態の 8 + 8 ファイルで PASS。
//   - aspect / template のいずれかに extra / missing / mismatched stem が
//     生じた場合に FAIL。
func TestAspectTemplateOneToOneMapping(t *testing.T) {
	aspectFiles := listAspectFiles(t)
	templateFiles := listTemplateFiles(t)

	aspectStems := make([]string, 0, len(aspectFiles))
	for _, p := range aspectFiles {
		base := filepath.Base(p)
		stem := strings.TrimSuffix(base, ".md")
		aspectStems = append(aspectStems, stem)
	}
	templateStems := make([]string, 0, len(templateFiles))
	for _, p := range templateFiles {
		base := filepath.Base(p)
		stem := strings.TrimSuffix(base, ".yaml")
		// design.md § Aspect / Template 対応 SSoT 表に従い、template ファイル名は
		// 必ず `<aspect>-review.yaml` 形式である前提。接尾辞 `-review` を取り除いて
		// aspect 側 stem と比較できる形に正規化する。`-review` を含まない template
		// が混入した場合は SSoT 違反として stem 不一致で検出される。
		if !strings.HasSuffix(stem, "-review") {
			t.Errorf("templates/%s.yaml: 期待されるファイル名形式 `<aspect>-review.yaml` ではない (design.md § Aspect / Template 対応 SSoT 表)", stem)
			continue
		}
		templateStems = append(templateStems, strings.TrimSuffix(stem, "-review"))
	}

	expected := append([]string(nil), expectedAspects...)
	sort.Strings(expected)

	sortedAspect := append([]string(nil), aspectStems...)
	sort.Strings(sortedAspect)
	sortedTemplate := append([]string(nil), templateStems...)
	sort.Strings(sortedTemplate)

	// (1) aspect 側 stem 集合 == SSoT 表
	if !equalStringSlices(sortedAspect, expected) {
		t.Errorf("aspects/*.md の stem 集合が SSoT (design.md § Aspect / Template 対応 SSoT 表) と一致しない:\n  got     = %v\n  expected= %v",
			sortedAspect, expected)
	}
	// (2) template 側 stem 集合 (`-review` 除去後) == SSoT 表
	if !equalStringSlices(sortedTemplate, expected) {
		t.Errorf("templates/*-review.yaml の stem 集合 (`-review` 除去後) が SSoT (design.md § Aspect / Template 対応 SSoT 表) と一致しない:\n  got     = %v\n  expected= %v",
			sortedTemplate, expected)
	}
	// (3) aspect 側 == template 側 (= 1:1 対応の対称性)
	if !equalStringSlices(sortedAspect, sortedTemplate) {
		t.Errorf("aspects と templates の 1:1 対応が崩れている (design.md § Verification Contract (v2) item 3):\n  aspects  = %v\n  templates= %v",
			sortedAspect, sortedTemplate)
	}
}

// equalStringSlices は事前にソート済みの 2 集合が同一かを判定する補助関数。
// 集合比較目的のため、長さ + 各要素の equality だけ確認すれば十分。
func equalStringSlices(a, b []string) bool {
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
