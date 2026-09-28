// Package apply の本ファイルは task 4.1 / design.md §Patch を実装する
// (Req 3.4, 3.5)。
//
// 役割: self-review が accept した cycle の LLM 出力 (`{diff, applied_files}` JSON) を解釈し、
// 多重の scope guard を経た上で `git apply --check` に渡せる Patch 構造体を返す純関数。
// 副作用は持たない (ファイル I/O / git 呼び出しなし)。
//
// 設計契約 (design.md §Patch):
//
//   - 入力: agent raw text (JSON `{"diff": "...", "applied_files": [...]}`) +
//     workflow 層が provider.StructuredOutput から抽出した declaredFiles +
//     LoadResult.FindingsFiles() で算出した findingsFiles + cwd。
//   - diff を unified diff として最低限 parse 可能か検証 (`+++ b/<path>` 行 + `@@` ハンク)。
//     失敗時 ErrUnparseable。
//   - Self-consistency: diff 内 `+++ b/<path>` 集合と declaredFiles の **完全一致** を検証
//     (Req 3.5 第一関門)。一致しなければ ErrOutOfScope。
//   - すべての path が cwd 配下の relative path であること (絶対パス / `..` を含まないこと)
//     を検証 (declaredFiles + diff path 双方)。違反は ErrOutOfScope。
//   - Findings-derived allowlist: declaredFiles ⊆ findingsFiles を検証 (Req 3.5 第二関門)。
//     違反は ErrOutOfScope。
//   - Fail-closed: findingsFiles が空集合 (= review-workflow 両端契約上ありえない契約違反)
//     のときは無条件に ErrOutOfScope を返す (LLM の自由裁量で任意 file を変更できる経路を
//     構造的に塞ぐ; design.md §Patch L640)。
//   - 成功時 Patch.TargetFiles は declaredFiles の宣言順を保ち、cleaned relative path として
//     返す (重複は最初の出現を保持)。
//
// improve/patch.ExtractAndGuard を流用しない理由 (design.md §Patch L641):
//
//   - improve は single-file 制約 (expectedTarget 1 件のみ); review-apply は multi-file 対応。
//   - improve は path を絶対 clean path で返す; review-apply は cwd 起点 relative path で返す
//     (design.md §Patch Service Interface L649)。
package apply

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Patch は ExtractAndGuard が抽出 + 全 scope guard 通過後に返す修正 patch の表現
// (design.md §Patch Service Interface)。
//
// Postconditions (ExtractAndGuard 成功時):
//   - TargetFiles は declaredFiles の宣言順を保持した cleaned relative path 列。
//     要素は cwd を抜けない (`..` を含まない / 絶対パスではない) ことが保証される。
//   - DiffBytes は `git apply --check` / `git apply` に渡せる unified diff の生バイト列
//     (JSON `diff` フィールドの値そのもの)。末尾改行の有無は元応答に依存し正規化しない。
type Patch struct {
	// TargetFiles は diff が変更する全 file の cwd 起点 cleaned relative path。
	// declaredFiles の宣言順を保ち、重複は最初の出現を保持する。
	TargetFiles []string

	// DiffBytes は agent JSON の `diff` フィールド値を []byte に変換した unified diff 本文。
	// `git apply --check` / `git apply` に標準入力経由で渡せる形式。
	DiffBytes []byte
}

// ErrUnparseable は agent 応答から unified diff を抽出できなかった (JSON 不正 / diff field
// 空 / `+++ b/` 行不在 / `@@` ハンク不在のいずれか) ことを示す sentinel error。
// orchestration 側 (task 5.3) は本 sentinel を観測したら `VerdictProposalUnparseable` に
// マップし、cycle を再生成 ループに戻す (Req 3.4)。
//
// errors.Is を介した検出を可能にするため、本パッケージの公開 sentinel として保持する。
var ErrUnparseable = errors.New("review/apply/patch: diff is not parseable as unified diff")

// ErrOutOfScope は抽出された diff および declaredFiles が許容範囲を逸脱した
// (self-consistency 違反 / allowlist 違反 / 絶対パス / `..` 含み / fail-closed) ことを
// 示す sentinel error。orchestration 側 (task 5.3) は本 sentinel を観測したら
// `VerdictOutOfScopeDiff` にマップし、違反 path 一覧を WorkflowText イベントで利用者に
// 明示する (Req 3.5)。
var ErrOutOfScope = errors.New("review/apply/patch: diff modifies files outside declared scope")

// patchAgentPayload は LLM 想定の `{"diff": "...", "applied_files": [...]}` JSON 形式を
// 受け取る型。declaredFiles は引数経由で受領するため、本構造体では `applied_files` を
// 参照しない (将来 self-validation 用途で参照する場合に備えて tag 定義は残す)。
type patchAgentPayload struct {
	// Diff は agent が返した unified diff 本文。空文字列のときは ErrUnparseable に変換する。
	Diff string `json:"diff"`

	// AppliedFiles は agent が宣言した変更 file 集合。本パッケージでは引数 declaredFiles を
	// 一次情報源として扱うため参照しないが、将来 declaredFiles と JSON の applied_files の
	// 一致検証 (cross-validation) を追加する余地として tag を残す。
	AppliedFiles []string `json:"applied_files"`
}

// plusPathRegex は unified diff の `+++ b/<path>` 行から変更後 path を抽出する regex。
//
// 仕様:
//   - `+++ b/` プレフィックス必須 (行頭マッチ)。
//   - path は改行を含まない任意文字列。前後の空白 / tab / metadata は別途 trim する。
//   - `(?m)` で multi-line モードを有効にし、`^` を行頭にマッチさせる。
//
// MVP の対象範囲は `internal/workflow/<...>.go` 形式に限定されないが、
// path の構文検証は ExtractAndGuard 側で path 単位に絶対パス / `..` reject を行うため
// regex 側では path 文字列をそのまま greedy に拾う。
var plusPathRegex = regexp.MustCompile(`(?m)^\+\+\+ b/(.+)$`)

// hunkHeaderRegex は unified diff の `@@ -<old> +<new> @@` ハンクヘッダを検出する regex。
// `@@` ハンクが diff 内に 1 つも無い場合は実際には apply 不能 (差分情報ゼロ) のため、
// ExtractAndGuard は ErrUnparseable に変換する。
var hunkHeaderRegex = regexp.MustCompile(`(?m)^@@ `)

// ExtractAndGuard は agent 出力から diff を抽出し、self-consistency / 絶対パス /
// `..` / allowlist / fail-closed の 5 段 scope guard を実行する純関数。
//
// 引数:
//   - rawAgentText: agent が返した `{"diff": "...", "applied_files": [...]}` 形式の JSON 文字列。
//   - declaredFiles: workflow 層が provider.StructuredOutput から抽出した applied_files 集合。
//   - findingsFiles: LoadResult.FindingsFiles() で算出した findings 由来の許容 file 集合
//     (Req 3.5 第二関門 allowlist)。空集合は契約違反として fail-closed (常に ErrOutOfScope)。
//   - cwd: workflow 層が解決した repo root の絶対パス。`filepath.Rel` 経由の escape 検出に利用する。
//
// 戻り値:
//   - 成功時: Patch (TargetFiles は declaredFiles の宣言順を保つ cleaned relative path、
//     DiffBytes は JSON `diff` フィールド本文の []byte)。
//   - 失敗時: ErrUnparseable (parse 不能) / ErrOutOfScope (scope 違反) のいずれか。
//     errors.Is で sentinel を判別可能。
//
// Preconditions:
//   - cwd は呼び出し側が `filepath.Clean` 済みの絶対パス (Workflow 層 task 5.x が責務)。
//
// Postconditions:
//   - 成功時 Patch.TargetFiles と Patch.DiffBytes はいずれも非空。
//   - 失敗時 Patch ゼロ値 + 非 nil error。
func ExtractAndGuard(rawAgentText string, declaredFiles []string, findingsFiles []string, cwd string) (Patch, error) {
	// (1) JSON parse: `{"diff": "...", "applied_files": [...]}` を decode する。
	trimmed := strings.TrimSpace(rawAgentText)
	if trimmed == "" {
		return Patch{}, fmt.Errorf("%w: agent text is empty", ErrUnparseable)
	}
	var payload patchAgentPayload
	if uerr := json.Unmarshal([]byte(trimmed), &payload); uerr != nil {
		return Patch{}, fmt.Errorf("%w: JSON unmarshal failed: %v", ErrUnparseable, uerr)
	}
	if payload.Diff == "" {
		return Patch{}, fmt.Errorf("%w: `diff` field is empty", ErrUnparseable)
	}

	// (2) Fail-closed: findingsFiles が空集合のとき即座に ErrOutOfScope を返す
	// (両端契約違反防御; design.md §Patch L640)。
	// declaredFiles の検査前に判定することで、LLM が空 declaredFiles を返した経路でも
	// fail-closed が優先される (allowlist 経路の意図を構造的に保つ)。
	if len(findingsFiles) == 0 {
		return Patch{}, fmt.Errorf("%w: findingsFiles is empty (fail-closed: contract violation defense)", ErrOutOfScope)
	}

	// (3) declaredFiles の path 健全性検証: 絶対パス / `..` 含みを reject する。
	// declaredSet は self-consistency 比較 (diff `+++ b/` 集合との完全一致) で利用する。
	// declaredOrder は 宣言順を保持して TargetFiles に詰める順序情報として利用する。
	declaredSet := make(map[string]struct{}, len(declaredFiles))
	declaredOrder := make([]string, 0, len(declaredFiles))
	for _, f := range declaredFiles {
		cleaned, ok := validateRelativePath(f, cwd)
		if !ok {
			return Patch{}, fmt.Errorf("%w: declared file %q is absolute or escapes cwd", ErrOutOfScope, f)
		}
		if _, dup := declaredSet[cleaned]; dup {
			continue
		}
		declaredSet[cleaned] = struct{}{}
		declaredOrder = append(declaredOrder, cleaned)
	}

	// (4) findingsFiles の正規化と allowlist set 構築。
	// findingsFiles 自体に `..` / 絶対パスが含まれていても、本パッケージは declaredFiles
	// 由来の path のみを TargetFiles に出すため、findingsFiles の path 健全性は
	// Loader (task 2.1) と review-workflow 両端契約に委ねる。ここでは set membership 用の
	// cleaned 表現のみを構築する。
	findingsSet := make(map[string]struct{}, len(findingsFiles))
	for _, f := range findingsFiles {
		findingsSet[filepath.Clean(f)] = struct{}{}
	}

	// (5) Allowlist (Req 3.5 第二関門): declaredFiles ⊆ findingsFiles を検証。
	for _, p := range declaredOrder {
		if _, ok := findingsSet[p]; !ok {
			return Patch{}, fmt.Errorf("%w: declared file %q is not in findings allowlist", ErrOutOfScope, p)
		}
	}

	// (6) Diff parse 検証: `+++ b/<path>` 行と `@@` ハンクが共に存在することを最低条件とする。
	matches := plusPathRegex.FindAllStringSubmatch(payload.Diff, -1)
	if len(matches) == 0 {
		return Patch{}, fmt.Errorf("%w: no `+++ b/<path>` line found", ErrUnparseable)
	}
	if !hunkHeaderRegex.MatchString(payload.Diff) {
		return Patch{}, fmt.Errorf("%w: no `@@` hunk header found (empty diff body)", ErrUnparseable)
	}

	// (7) diff 内 `+++ b/` path 集合を抽出し、self-consistency / 健全性検証を行う。
	// path 文字列は trailing whitespace / tab / unified diff timestamp 等が付くケースに
	// 備えて TrimRight でホワイトスペースを除去する。
	diffSet := make(map[string]struct{}, len(matches))
	diffOrder := make([]string, 0, len(matches))
	for _, m := range matches {
		raw := strings.TrimRight(m[1], " \t\r")
		if raw == "" {
			return Patch{}, fmt.Errorf("%w: empty path in `+++ b/` line", ErrUnparseable)
		}
		if raw == "/dev/null" {
			// `/dev/null` は削除 diff の出力先として現れる。MVP では削除 diff を対象外とし、
			// scope guard を構造的にすり抜ける経路を塞ぐため OutOfScope として扱う。
			return Patch{}, fmt.Errorf("%w: deletion diff (`+++ b//dev/null`) is not supported in review-apply scope", ErrOutOfScope)
		}
		cleaned, ok := validateRelativePath(raw, cwd)
		if !ok {
			return Patch{}, fmt.Errorf("%w: diff path %q is absolute or escapes cwd", ErrOutOfScope, raw)
		}
		if _, dup := diffSet[cleaned]; dup {
			continue
		}
		diffSet[cleaned] = struct{}{}
		diffOrder = append(diffOrder, cleaned)
	}

	// (8) Self-consistency (Req 3.5 第一関門): diff path 集合 == declaredFiles 集合 を検証。
	// 両方向の包含 (declared ⊆ diff && diff ⊆ declared) を確認する。
	if len(diffSet) != len(declaredSet) {
		return Patch{}, fmt.Errorf("%w: diff path set (%d) and declared files (%d) cardinality mismatch",
			ErrOutOfScope, len(diffSet), len(declaredSet))
	}
	for _, p := range diffOrder {
		if _, ok := declaredSet[p]; !ok {
			return Patch{}, fmt.Errorf("%w: diff modifies %q which is not in declared applied_files", ErrOutOfScope, p)
		}
	}
	for _, p := range declaredOrder {
		if _, ok := diffSet[p]; !ok {
			return Patch{}, fmt.Errorf("%w: declared file %q is not present in diff `+++ b/` lines", ErrOutOfScope, p)
		}
	}

	// (9) 全 guard 通過。Patch を組み立てて返す。
	// TargetFiles は declaredFiles の宣言順を保持する (workflow 層が UI 表示や git add の
	// 引数順序として利用する場合に deterministic な順序を保証するため)。
	return Patch{
		TargetFiles: declaredOrder,
		DiffBytes:   []byte(payload.Diff),
	}, nil
}

// validateRelativePath は path が cwd 配下の relative path であることを検証する。
//
// 仕様:
//   - 絶対パス (`filepath.IsAbs`) は reject する。
//   - `filepath.Clean` 後に `..` または `../*` で始まる path は cwd を抜けるため reject する。
//   - cwd join 後に `filepath.Rel(cwd, ...)` で再判定し、`..` が残っていれば reject する
//     (二重防御: shell-glob / symbolic 等の特殊 path を構造的に塞ぐ)。
//
// 戻り値: (cleaned relative path, true) または ("", false)。
func validateRelativePath(p, cwd string) (string, bool) {
	if p == "" {
		return "", false
	}
	if filepath.IsAbs(p) {
		return "", false
	}
	cleaned := filepath.Clean(p)
	if cleaned == ".." {
		return "", false
	}
	if strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", false
	}
	// 二重防御: cwd join 後の `filepath.Rel` で `..` を含まないことを再確認する。
	// cwd が relative path のときも安全 (filepath.Rel が relative result を返すため)。
	joined := filepath.Clean(filepath.Join(cwd, cleaned))
	rel, err := filepath.Rel(cwd, joined)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return cleaned, true
}
