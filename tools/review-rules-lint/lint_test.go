// Package reviewruleslint は review-rules-skill 配布物の templates/*.yaml が
// review-workflow YAML schema (v2: 4 必須フィールド + kebab-case + 予約語禁止 +
// provider 有効値 + name ≡ filename + unknown フィールド非含有) を満たすことを
// authoring-time に検証するための Go test パッケージである。
//
// v2 では templates/*.yaml は「種 (枠)」として `name` / `description` /
// `provider` / `model` の 4 フィールドのみを持ち、`applies_when` / `prompt` は
// 出荷状態では存在せず Phase 4 で合成される (Req 1.1 / Req 1.2 / Req 1.4)。
// したがって本 lint の必須フィールド検査は 4 項目に縮退し、`applies_when` /
// `prompt` は欠落 / 空でも OK とする (Req 4.1 / design § Verification Contract 1)。
// 一方 unknown フィールド検査 (f) は v2 で重要な drift gate として機能し、
// templates に `applies_when` / `prompt` が残存した場合に unknown として検出する
// (= テンプレ「種」化の契約を CI で守るための gate)。
//
// 本パッケージは rilll repo 内 (tools/review-rules-lint/) にのみ存在し、
// skill 配布物 (.agents/skills/review-rules-skill/) には一切同梱しない
// (Layer Separation 不変式 / Req 5.1 / Req 5.2)。
package reviewruleslint

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// templatesDir は本テストパッケージから review-rules-skill の templates/ への
// 相対パス。go test 実行時の作業ディレクトリは tools/review-rules-lint/ である
// ため、二段上に遡ってから .agents/skills/... を辿る。
const templatesDir = "../../.agents/skills/review-rules-skill/templates"

// requiredFields は v2 templates/*.yaml が保持すべき 4 必須フィールド。
// v1 で必須としていた `applies_when` / `prompt` は v2 で削除され、Phase 4 で
// 合成される (Req 1.1 / Req 1.2)。本 lint では 4 項目のみ非空を要求し、
// `applies_when` / `prompt` は欠落 / 空でも違反としない (Req 4.1)。
// 並び順は references/schema.md および design.md と一致。
var requiredFields = []string{
	"name",
	"description",
	"provider",
	"model",
}

// reservedNames は loader が rule 名として禁じる予約語集合。
// 値は review-workflow design.md §`ReservedRuleNames` と同一でなければならない。
var reservedNames = map[string]struct{}{
	"result":  {},
	"context": {},
}

// validProviders は review-workflow loader が許容する provider 有効値集合。
// 値は rilll steering tech.md および references/schema.md と同一。
var validProviders = map[string]struct{}{
	"claude":     {},
	"cursor":     {},
	"codex":      {},
	"cursor-sdk": {},
}

// kebabCaseRe は rule 名 (および後続の auto-rename 候補) が満たすべき kebab-case
// 正規表現。design.md / references/schema.md と同一。
var kebabCaseRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// listTemplateFiles は templates/ 配下の *.yaml を列挙する。
// サブディレクトリは loader 対象外であるため、本テストでも直下のみ走査する
// (review-workflow loader 規約と整合)。
func listTemplateFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(templatesDir)
	if err != nil {
		t.Fatalf("templates ディレクトリを読めなかった: %s: %v", templatesDir, err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		files = append(files, filepath.Join(templatesDir, name))
	}
	if len(files) == 0 {
		t.Fatalf("templates/ 配下に *.yaml が 1 件も見つからなかった: %s", templatesDir)
	}
	return files
}

// stringValue は YAML root の任意キー (string 型必須) を取り出す。
// nil / 非 string / 空文字列はそれぞれ error 区別ができるよう ok / kind を返す。
func stringValue(m map[string]any, key string) (value string, present bool, isString bool) {
	v, ok := m[key]
	if !ok {
		return "", false, false
	}
	if v == nil {
		return "", true, false
	}
	s, ok := v.(string)
	if !ok {
		return "", true, false
	}
	return s, true, true
}

// schemaViolation は 1 件のスキーマ違反 (検証項目記号 + メッセージ) を表す。
// 単体テストで「lint ロジックが本当に違反を検出するか」を主張するために用いる
// (= 本テストファイルが scaffolding ではなく実検査ロジックを持つことの証拠)。
type schemaViolation struct {
	code    rune   // 'a' 〜 'f' の検証項目記号
	message string // 違反メッセージ (filename + 詳細を含む)
}

// lintRoot は decode 済みの YAML root に対して 6 検証項目 (a)〜(f) を適用し、
// 違反を返す。v2 では (a) の対象が 4 必須フィールドに縮退している点が v1 から
// の差分であり、`applies_when` / `prompt` が root に存在した場合は (a) ではなく
// (f) unknown フィールドとして検出される (= 「種」契約の drift gate)。
// TestTemplatesSchemaLint と TestLintRoot_DetectsViolations の両方から呼ばれる
// 共通の lint コア。filename は (d) name ≡ filename 検査と error メッセージ用。
func lintRoot(filename string, root map[string]any) []schemaViolation {
	var violations []schemaViolation

	// (f) unknown フィールド非含有
	// v2 では requiredFields が 4 項目に縮退しているため、`applies_when` /
	// `prompt` がもし templates に残っていればここで unknown として検出される。
	knownSet := make(map[string]struct{}, len(requiredFields))
	for _, k := range requiredFields {
		knownSet[k] = struct{}{}
	}
	for k := range root {
		if _, ok := knownSet[k]; !ok {
			violations = append(violations, schemaViolation{
				code:    'f',
				message: filename + ": unknown フィールド検出 (検証項目 f): " + k,
			})
		}
	}

	// (a) 4 必須フィールド非空
	for _, field := range requiredFields {
		v, present, isString := stringValue(root, field)
		if !present {
			violations = append(violations, schemaViolation{
				code:    'a',
				message: filename + ": 必須フィールド欠落 (検証項目 a): " + field,
			})
			continue
		}
		if !isString {
			violations = append(violations, schemaViolation{
				code:    'a',
				message: filename + ": 必須フィールド型不正 (検証項目 a): " + field,
			})
			continue
		}
		if strings.TrimSpace(v) == "" {
			violations = append(violations, schemaViolation{
				code:    'a',
				message: filename + ": 必須フィールド空 (検証項目 a): " + field,
			})
		}
	}

	// (b)(c)(d) name 関連検査
	if nameVal, present, isString := stringValue(root, "name"); present && isString {
		if !kebabCaseRe.MatchString(nameVal) {
			violations = append(violations, schemaViolation{
				code:    'b',
				message: filename + ": name が kebab-case 違反 (検証項目 b): " + nameVal,
			})
		}
		if _, isReserved := reservedNames[nameVal]; isReserved {
			violations = append(violations, schemaViolation{
				code:    'c',
				message: filename + ": name が予約語 (検証項目 c): " + nameVal,
			})
		}
		expected := strings.TrimSuffix(filename, ".yaml")
		if nameVal != expected {
			violations = append(violations, schemaViolation{
				code:    'd',
				message: filename + ": name ≠ filename (検証項目 d): name=" + nameVal + ", filename(no ext)=" + expected,
			})
		}
	}

	// (e) provider 有効値集合
	if providerVal, present, isString := stringValue(root, "provider"); present && isString {
		if _, ok := validProviders[providerVal]; !ok {
			violations = append(violations, schemaViolation{
				code:    'e',
				message: filename + ": provider 不正 (検証項目 e): " + providerVal,
			})
		}
	}

	return violations
}

// TestLintRoot_DetectsViolations は lint コアロジックが (a)〜(f) 全 6 検証項目に
// ついて違反を実際に検出することを、合成 YAML root を直接構築して証明する。
// 出荷状態の templates/*.yaml が違反を起こさないため、TestTemplatesSchemaLint
// だけでは「ロジックが scaffolding か否か」が判別できない。本テストはその
// 不足を補い、lint が実検査として機能していることを保証する。
//
// v2 縮退に伴い、各ケースの root は v2 の「種」形 (4 フィールド)
// `name` / `description` / `provider` / `model` を base にする (Req 4.1)。
// `applies_when` / `prompt` を base から外しているため、v1 では (a) 必須欠落
// として検出されていた状態は v2 では正常状態となる。代わりに (f) unknown 検査の
// drift gate として `applies_when` 残存ケースを追加し、種化契約の保護を明示する。
func TestLintRoot_DetectsViolations(t *testing.T) {
	type tc struct {
		desc       string
		filename   string
		root       map[string]any
		wantCode   rune // 期待する違反コード ('a' 〜 'f')
		wantSubstr string
	}
	cases := []tc{
		{
			desc:     "(a) 必須フィールド欠落 (description) - v2 4 項目 base から description のみ削った状態",
			filename: "sample-rule.yaml",
			// description を欠落させ、残り 3 項目のみ。`applies_when` / `prompt` は
			// v2 では必須でないため base に含めない。
			root:       map[string]any{"name": "sample-rule", "provider": "claude", "model": "claude-opus-4-7"},
			wantCode:   'a',
			wantSubstr: "description",
		},
		{
			desc:       "(a) 必須フィールド空 (description が whitespace のみ) - v2 4 項目 base",
			filename:   "sample-rule.yaml",
			root:       map[string]any{"name": "sample-rule", "description": "   ", "provider": "claude", "model": "claude-opus-4-7"},
			wantCode:   'a',
			wantSubstr: "description",
		},
		{
			desc:       "(b) name が kebab-case 違反 (大文字)",
			filename:   "BadName.yaml",
			root:       map[string]any{"name": "BadName", "description": "d", "provider": "claude", "model": "claude-opus-4-7"},
			wantCode:   'b',
			wantSubstr: "kebab-case",
		},
		{
			desc:       "(c) name が予約語 (result)",
			filename:   "result.yaml",
			root:       map[string]any{"name": "result", "description": "d", "provider": "claude", "model": "claude-opus-4-7"},
			wantCode:   'c',
			wantSubstr: "予約語",
		},
		{
			desc:       "(d) name ≠ filename",
			filename:   "actual-filename.yaml",
			root:       map[string]any{"name": "different-name", "description": "d", "provider": "claude", "model": "claude-opus-4-7"},
			wantCode:   'd',
			wantSubstr: "name ≠ filename",
		},
		{
			desc:       "(e) provider 不正",
			filename:   "sample-rule.yaml",
			root:       map[string]any{"name": "sample-rule", "description": "d", "provider": "openai", "model": "gpt-4"},
			wantCode:   'e',
			wantSubstr: "provider 不正",
		},
		{
			desc:       "(f) unknown フィールド (extra_key)",
			filename:   "sample-rule.yaml",
			root:       map[string]any{"name": "sample-rule", "description": "d", "provider": "claude", "model": "claude-opus-4-7", "extra_key": "z"},
			wantCode:   'f',
			wantSubstr: "extra_key",
		},
		{
			// v2 drift gate: テンプレ「種」化 (Req 1.1) の契約を CI で守るため、
			// `applies_when` が root に残った状態は (f) unknown として検出される
			// ことを明示する。template_v2_shape_test.go (task 3.2) と並んで二重に
			// 種化を保護する位置づけ。
			desc:       "(f) v2 drift gate: applies_when 残存は unknown として検出される",
			filename:   "sample-rule.yaml",
			root:       map[string]any{"name": "sample-rule", "description": "d", "provider": "claude", "model": "claude-opus-4-7", "applies_when": "x"},
			wantCode:   'f',
			wantSubstr: "applies_when",
		},
		{
			// 同上、`prompt` 残存も (f) unknown として検出されることを明示する。
			desc:       "(f) v2 drift gate: prompt 残存は unknown として検出される",
			filename:   "sample-rule.yaml",
			root:       map[string]any{"name": "sample-rule", "description": "d", "provider": "claude", "model": "claude-opus-4-7", "prompt": "y"},
			wantCode:   'f',
			wantSubstr: "prompt",
		},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			vs := lintRoot(c.filename, c.root)
			found := false
			for _, v := range vs {
				if v.code == c.wantCode && strings.Contains(v.message, c.wantSubstr) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("検証項目 %c が検出されなかった (期待 substr=%q): violations=%+v", c.wantCode, c.wantSubstr, vs)
			}
		})
	}
}

// TestTemplatesSchemaLint は templates/*.yaml 全件について review-workflow YAML
// schema 整合を v2 検証項目 (a)〜(f) で確認する。違反時は "<filename>: <違反項目>"
// を含む t.Errorf を発行し、ユーザが原因特定できるようにする
// (tasks.md task 3.1 観測可能完了状態 / design.md § Verification Contract 1).
//
// v2 では (a) は 4 項目に縮退し、`applies_when` / `prompt` は欠落 / 空でも OK。
// templates が `applies_when` / `prompt` を保持していた場合は (f) unknown として
// 検出される (= 種化契約の drift gate として機能する)。
func TestTemplatesSchemaLint(t *testing.T) {
	files := listTemplateFiles(t)
	for _, path := range files {
		filename := filepath.Base(path)
		t.Run(filename, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s: 読み込み失敗: %v", filename, err)
			}

			// (f) unknown フィールド非含有を検出するために
			// map[string]any へデコードし、既知 4 フィールド以外の
			// キーが root に存在しないことを確認する。yaml.v3 の
			// KnownFields(true) は struct 必要だが、本検査は SSoT として
			// references/schema.md の 4 フィールドを直接参照したいため
			// map ベースで実装する。
			var root map[string]any
			if err := yaml.Unmarshal(data, &root); err != nil {
				t.Fatalf("%s: YAML parse 失敗: %v", filename, err)
			}
			if root == nil {
				t.Fatalf("%s: YAML root が空 / null", filename)
			}

			// 6 検証項目 (a)〜(f) を共通 lint コアに委譲。違反は
			// "<filename>: <違反項目>" 形式のメッセージとして t.Errorf に転送し、
			// 違反が複数同時発火した場合も全件報告する (= 1 ファイル / 1 run で
			// 全違反を可視化し、修正サイクルを 1 回で済ませるため)。
			for _, v := range lintRoot(filename, root) {
				t.Errorf("%s", v.message)
			}
		})
	}
}
