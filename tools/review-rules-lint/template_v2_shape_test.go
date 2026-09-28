// Package reviewruleslint - template_v2_shape_test.go は v2 仕様の核心契約である
// 「templates/*.yaml は『種 (枠)』であり、`applies_when` / `prompt` を出荷状態で
// 持たない」ことを authoring-time に専用 drift gate として検査する。
//
// 本ファイルの責務 (Layer Separation 不変式 / Req 4.2 への参照):
//   - Req 4.2 (.kiro/specs/review-rules-skill-v2/requirements.md): 「templates/*.yaml が
//     `applies_when` / `prompt` を **持たない** こと (出荷状態で固定 prompt を持たない契約)
//     を drift gate として検査する」を Go test として実装する。
//   - design.md § Verification Contract (v2) 2: template_v2_shape_test.go (新設) を
//     yaml.v3 で root map をデコードし、`applies_when` / `prompt` が key set に含まれない
//     ことを単独で表明する。
//
// なお lint_test.go の TestTemplatesSchemaLint も (f) unknown フィールド検査を介して
// 同じ drift を間接的に捕捉できるが、本ファイルは v2 の核心契約 (= 「種」化) を
// 単独テストとして可視化することで、契約違反時のエラーメッセージを v2 仕様に直接
// 紐付けて発火させ、修正者が「v2 仕様違反」と即時に認識できるようにする
// (= drift gate として明示的に独立した位置づけ)。
//
// Layer Separation 不変式 (design.md § Layer Separation 不変式):
//   - 本 Go テストは rilll repo 内 (tools/review-rules-lint/) にのみ存在し、
//     skill 配布物 (.agents/skills/review-rules-skill/) には一切同梱しない。
//   - skill 側に Go / lint バイナリ依存を持ち込まない (Req 5.1 / Req 5.2)。
package reviewruleslint

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// forbiddenV2Keys は templates/*.yaml が出荷状態で持っては **ならない** key 集合。
// v2 仕様 (Req 1.1 / Req 4.2) によりこれらは Phase 4 で `references/aspects/<aspect>.md`
// の合成ガイドに従って動的に合成される。templates/*.yaml に残存している場合は
// 「種化」契約への drift であり、本テストが FAIL することで CI で検出する。
var forbiddenV2Keys = []string{
	"applies_when",
	"prompt",
}

// TestTemplateV2Shape は templates/*.yaml 全件 (8 ファイル) について、
// yaml.v3 で root map にデコードした結果の key set に `applies_when` / `prompt` が
// 含まれないことを検査する。
//
// 検査ロジック:
//  1. listTemplateFiles (lint_test.go 共通ヘルパ) で *.yaml を列挙。
//  2. 各ファイルを os.ReadFile + yaml.Unmarshal で map[string]any にデコード。
//  3. `applies_when` / `prompt` のいずれかが root key set に含まれていれば
//     「v2 仕様違反: <field> フィールドが残存している (Phase 4 で合成されるべき)」を
//     t.Errorf で報告する (= drift gate)。
//
// 期待動作:
//   - 出荷状態の 8 ファイル全てで PASS。
//   - 誤って template に `applies_when:` または `prompt:` を追加した場合に FAIL。
func TestTemplateV2Shape(t *testing.T) {
	files := listTemplateFiles(t)
	for _, path := range files {
		filename := filepath.Base(path)
		t.Run(filename, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s: 読み込み失敗: %v", filename, err)
			}

			// root を map[string]any にデコードし、key の存在を直接検査する。
			// struct + KnownFields(true) でなく map ベースにする理由は、
			// 「禁止 key の集合」を SSoT (Req 4.2) として直接表現するため
			// (= 後から禁止 key を増減させる際の追従ポイントを 1 箇所に固定)。
			var root map[string]any
			if err := yaml.Unmarshal(data, &root); err != nil {
				t.Fatalf("%s: YAML parse 失敗: %v", filename, err)
			}
			if root == nil {
				t.Fatalf("%s: YAML root が空 / null", filename)
			}

			// 禁止 key 検査: 1 ファイルにつき複数違反を全件報告し、
			// 修正サイクルを 1 回で済ませられるようにする。
			for _, forbidden := range forbiddenV2Keys {
				if _, exists := root[forbidden]; exists {
					t.Errorf("%s: v2 仕様違反: `%s` フィールドが残存している (Phase 4 で合成されるべき)", filename, forbidden)
				}
			}
		})
	}
}
