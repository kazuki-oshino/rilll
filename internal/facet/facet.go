package facet

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// embeddedFS にはプロンプトファセットの placeholder Markdown 群を焼き込む。
// 後続 spec が実コンテンツを追加する場合は internal/facet/facets/<kind-dir>/<name>.md を
// 追加するだけで ResolveFacet から解決可能になるよう、glob パターンで宣言する。
//
//go:embed facets/*/*.md
var embeddedFS embed.FS

// FacetKind は 4 種のファセット種別を表す列挙型。
// 外部から FacetKind(任意 int) を生成しないこと (不正値混入防止)。
type FacetKind int

const (
	Persona        FacetKind = iota // → "personas"
	Policy                          // → "policies"
	Knowledge                       // → "knowledge"
	OutputContract                  // → "output-contracts"
)

// CONTEXT_MAX_CHARS は TrimContext のデフォルト切り詰め上限 (rune 単位)。
// 下流 spec (previous-response-passing 等) は本定数を import で参照すること。
const CONTEXT_MAX_CHARS = 2000

// ErrFacetNotFound は (kind, name) で示されるファセットが
// project override / embed のいずれにも存在しないことを示す sentinel error。
// 不正な name (path traversal 防御に引っかかる値) もこの error に丸める。
// I/O エラー (権限不足等) は wrap した別系統 error として伝播し、本 sentinel とは
// errors.Is で区別可能。
var ErrFacetNotFound = errors.New("facet: not found")

// ResolvedFacet は ResolveFacet が返す解決済みファセット。
//
// SourcePath の文字列規約:
//   - project override 時: 解決した実ファイルの絶対パス
//     (例: "/workspace/example/.rilll-config/facets/policies/coding.md")
//   - embed 時: "embed:facets/<kind-dir>/<name>.md"
//     (例: "embed:facets/policies/coding.md")
type ResolvedFacet struct {
	Content    string
	SourcePath string
	Kind       FacetKind
	Name       string
}

// String は fmt.Stringer 実装。log / error メッセージで識別可能にする。
//
// 戻り値は以下に固定 (Revalidation Triggers 対象):
//
//	Persona        → "Persona"
//	Policy         → "Policy"
//	Knowledge      → "Knowledge"
//	OutputContract → "OutputContract"
//	未知値         → "facet:unknown(<int>)" (panic しない / 防御的)
//
// 注: 上記は log / error メッセージ用の人間可読 PascalCase 表記。
// kebab-case ディレクトリ名 ("personas" / "policies" / "knowledge" /
// "output-contracts") はパス解決用の別 helper (k FacetKind) dir() が担当し、
// String() の責務 (識別子の文字列化) とは分離する。
func (k FacetKind) String() string {
	switch k {
	case Persona:
		return "Persona"
	case Policy:
		return "Policy"
	case Knowledge:
		return "Knowledge"
	case OutputContract:
		return "OutputContract"
	default:
		return fmt.Sprintf("facet:unknown(%d)", int(k))
	}
}

func (k FacetKind) dir() (string, bool) {
	switch k {
	case Persona:
		return "personas", true
	case Policy:
		return "policies", true
	case Knowledge:
		return "knowledge", true
	case OutputContract:
		return "output-contracts", true
	default:
		return "", false
	}
}

// isInvalidName は ResolveFacet の name バリデーション条件を集約した内部 helper。
// 新規 sentinel を増やさない方針のため、ここで真を返した name は呼び出し側で
// ErrFacetNotFound に丸める。条件は design.md の Revalidation Triggers 対象:
//
//   - 空文字列
//   - "/" または "\" を含む (cross-platform path traversal 防御)
//   - filepath.Clean(name) != name (例: "../foo" / "./foo" / 末尾区切り 等)
func isInvalidName(name string) bool {
	if name == "" {
		return true
	}
	if strings.ContainsAny(name, `/\`) {
		return true
	}
	if filepath.Clean(name) != name {
		return true
	}
	return false
}

// ResolveFacet は (kind, name) を 2 層解決し、ResolvedFacet を返す。
//
// 解決順:
//  1. cwd が空でない かつ <cwd>/.rilll-config/facets/<kind-dir>/<name>.md が存在 →
//     その内容を返却 (SourcePath = 解決した実ファイルのパス / cwd が絶対なら絶対パス)
//  2. embed FS に facets/<kind-dir>/<name>.md が同梱 →
//     embed 内容を返却 (SourcePath = "embed:facets/<kind-dir>/<name>.md")
//  3. いずれにも存在しない → ErrFacetNotFound
//
// name バリデーション: 空文字列 / "/" or "\" を含む / filepath.Clean(name) != name の
// いずれかに該当する name は ErrFacetNotFound として扱う (path traversal 防止 /
// 新規 sentinel を増やさない)。
//
// override 読込での fs.ErrNotExist 以外の I/O エラー (権限不足等) は embed に
// フォールスルーせず、`fmt.Errorf("facet: read override %s: %w", path, err)` の形で
// wrap して伝播する (権限不足を NotFound で隠さない)。
//
// 本関数はパッケージ内可変グローバル状態に依存しない (戻り値は cwd / kind / name /
// 環境のファイルシステム状態のみから決定される / R2.5)。
func ResolveFacet(cwd string, kind FacetKind, name string) (ResolvedFacet, error) {
	// name バリデーション (新規 sentinel を増やさず ErrFacetNotFound に丸める)
	if isInvalidName(name) {
		return ResolvedFacet{}, ErrFacetNotFound
	}

	// kind → kebab-case ディレクトリ名
	kindDir, ok := kind.dir()
	if !ok {
		return ResolvedFacet{}, ErrFacetNotFound
	}

	// 1. project override 経路 (cwd != "" のときのみ試行)
	if cwd != "" {
		overridePath := filepath.Join(cwd, ".rilll-config", "facets", kindDir, name+".md")
		content, err := os.ReadFile(overridePath)
		switch {
		case err == nil:
			return ResolvedFacet{
				Content:    string(content),
				SourcePath: overridePath,
				Kind:       kind,
				Name:       name,
			}, nil
		case errors.Is(err, fs.ErrNotExist):
			// embed フォールスルー
		default:
			// 権限不足など fs.ErrNotExist 以外の I/O エラーは embed で隠さず wrap して返す
			return ResolvedFacet{}, fmt.Errorf("facet: read override %s: %w", overridePath, err)
		}
	}

	// 2. embed 経路
	// path.Join (slash 区切り) を使う: embed.FS の path は OS 非依存の slash 表現
	embedPath := path.Join("facets", kindDir, name+".md")
	content, err := embeddedFS.ReadFile(embedPath)
	switch {
	case err == nil:
		return ResolvedFacet{
			Content:    string(content),
			SourcePath: "embed:" + embedPath,
			Kind:       kind,
			Name:       name,
		}, nil
	case errors.Is(err, fs.ErrNotExist):
		return ResolvedFacet{}, ErrFacetNotFound
	default:
		// embed.FS は in-memory のため通常 fs.ErrNotExist 以外は発生しないが防御的に wrap
		return ResolvedFacet{}, fmt.Errorf("facet: read embed %s: %w", embedPath, err)
	}
}

// TrimContext は content を rune 境界で maxChars に切り詰める低水準の純関数。
// prompt に埋め込む context の種類が分かっている場合は、損失可否を明示できる
// TrimPromptContext を優先する。
//
//   - utf8.RuneCountInString(content) <= maxChars または maxChars <= 0:
//     content をそのまま返す (短文 / 防御分岐)
//   - 長文かつ sourcePath != "":
//     先頭 maxChars rune + "...(truncated, full at <sourcePath>)"
//   - 長文かつ sourcePath == "":
//     先頭 maxChars rune + "...(truncated)"
//     (sourcePath が無いとき末尾の "full at " が裸で残るのを避ける graceful degradation)
//
// マーカ 2 形式は Revalidation Triggers 対象。下流 spec
// (previous-response-passing 等) で sourcePath="" を渡す呼出しが想定される。
//
// 本関数はファイル I/O / グローバル可変状態に依存せず、同一引数で同一結果を返す
// (R5.3 純関数性)。
func TrimContext(content string, maxChars int, sourcePath string) string {
	// 短文 / 防御分岐: rune 数 ≤ maxChars または maxChars <= 0 はそのまま返す
	if maxChars <= 0 {
		return content
	}
	if utf8.RuneCountInString(content) <= maxChars {
		return content
	}

	// 長文: 先頭 maxChars rune を切り出して string に戻す
	head := string([]rune(content)[:maxChars])

	// マーカ 2 形式 (Revalidation Triggers 対象)
	if sourcePath != "" {
		return head + fmt.Sprintf("...(truncated, full at %s)", sourcePath)
	}
	return head + "...(truncated)"
}
