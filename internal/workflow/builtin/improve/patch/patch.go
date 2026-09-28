// Package improvepatch の本ファイルは Patch 型と ExtractAndGuard 関数 (純関数) を提供する。
// AI 応答 (生 text または `provider.OutputFormat` JSON) から unified diff を抽出し、
// 対象ファイル scope guard を行う (tasks.md 5.1 / design.md §Patch / Req 3.3, 3.4, 3.5, 5.8)。
//
// 設計契約 (design.md §Patch):
//   - 純関数 (副作用なし)。
//   - Provider ID を受け取らず、両形式 (JSON Schema / 生 text) を順に試行する (Req 3.1)。
//   - diff parser は最小限実装: `diff --git a/<path> b/<path>` 行から `<path>` を抽出し、
//     cwd 結合後の絶対 clean path が expectedTarget と一致するかだけを検証する。
//   - `@@` ハンク内容や行番号の整合性検証は本関数の責務外 (`git apply --check` に委ねる)。
//
// Provider Compatibility Matrix (design.md §Patch / Resolved Decisions §6):
//   - claude / codex: `provider.OutputFormat{Type:"json_schema"}` 経由で `{"diff": "..."}` JSON を要求。
//     JSON path 優先抽出、失敗時は生 text fallback に降下。
//   - cursor: 外部 CLI 経由のため OutputFormat は no-op。生 text コードブロック (```diff ... ```) のみ。
//
// orchestration 統合 (task 11.2):
//   - regenerate ループ内 1 試行ごとに呼び出し、ErrUnparseable / ErrOutOfScope のいずれも
//     ユーザに通知して 3 択 Select の選択肢に「再生成」を提示する。
package patch

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Patch は ExtractAndGuard が抽出 + scope guard 通過後に返す改善 patch の表現。
//
// Postconditions (ExtractAndGuard 成功時):
//   - TargetFiles は cwd 結合後の絶対 clean path で正規化済み (`filepath.Clean` 適用済み)。
//   - 現 MVP では len(TargetFiles) == 1 で、要素は expectedTarget と一致する。
//   - DiffBytes は `git apply` に渡せる unified diff の生バイト列 (JSON 経路では `diff` フィールドの
//     値、生 text 経路ではコードブロック内側の生バイト列)。コードフェンス (` ``` `) や JSON 構造は
//     除去されている。
type Patch struct {
	// TargetFiles は diff 内で変更される全ファイルの絶対 clean path 配列。
	// 現 MVP では単一ファイル限定 (len == 1)、要素は expectedTarget と一致する契約 (Req 5.8)。
	TargetFiles []string

	// DiffBytes は `git apply --check` / `git apply` に標準入力経由で渡せる unified diff の生バイト列。
	// 末尾改行の有無は元応答に依存し、本パッケージでは正規化しない (`git apply` が両形式を受容するため)。
	DiffBytes []byte

	// AbsolutePath は本 Patch に対応する `proposal-N.diff` 等の永続化済みファイル絶対パス
	// (Issue #46 input thinning)。orchestration 側 (`improve/workflow.go`) が
	// previousProposals に append する前にセットする。
	//
	// renderPreviousProposalsSection は本フィールドが非空のとき、各 attempt セクションに
	// "- source: <path>" 行を追加し、`DiffBytes` 全文の代わりに `previousProposalDiffMaxChars`
	// で切り詰めた本文 + Source パス参照で stream_idle_timeout の構造的リスクを下げる。
	// 空文字列のときは従来挙動 (Source 行省略 + DiffBytes 全文埋め込み)。
	//
	// `ExtractAndGuard` の戻り値時点では未設定 (zero value) で、orchestration 層が
	// 永続化に成功した経路でのみ書き込む契約。本フィールドの有無は `Patch` のセマンティクス
	// (= 適用可能な diff の表現) には影響しない。
	AbsolutePath string
}

// ErrUnparseable は AI 応答から unified diff を抽出できなかったことを示す sentinel error。
// orchestration 側は本 sentinel を観測したら `proposal_unparseable` verdict にマップし、
// ユーザに 3 択 (accept は無効、reject / regenerate のみ) を提示する (Req 3.4)。
//
// errors.Is を介した検出を可能にするため、本パッケージの公開 sentinel として保持する。
var ErrUnparseable = errors.New("improvepatch: AI response does not contain a parseable unified diff")

// ErrOutOfScope は抽出された diff が expectedTarget 以外のファイルを変更しようとした
// (= 対象範囲外) ことを示す sentinel error。orchestration 側は `out_of_scope_diff` verdict に
// マップし、ユーザに 3 択を提示する (Req 3.5 / 5.8)。
var ErrOutOfScope = errors.New("improvepatch: diff modifies files outside the target")

// diffGitLineRegex は unified diff の `diff --git a/<path> b/<path>` 行から
// 変更対象 path を抽出する regex。
//
// 仕様:
//   - `diff --git ` プレフィックス必須 (行頭マッチ)。
//   - `a/<path>` と `b/<path>` の path が一致する場合のみ正規 (rename diff は MVP 対象外)。
//   - path はスペースを含まない簡素なケース (POSIX-like) のみ対応。
//     これは MVP の対象範囲が `internal/workflow/<snake>.go` 形式に限定されるため十分 (design.md §Patch)。
//
// `(?m)` を付けて multi-line モードで `^` / `$` を行頭 / 行末にマッチさせる。
var diffGitLineRegex = regexp.MustCompile(`(?m)^diff --git a/(\S+) b/(\S+)`)

// codeBlockDiffRegex は生 text 内の ` ```diff ... ``` ` コードブロック (cursor 想定) を抽出する regex。
//
// 仕様:
//   - フェンスは `~~~` でも対応する (Markdown 仕様でも有効) → 本実装では ``` のみに限定 (cursor の典型出力)。
//   - 言語 hint は `diff` のみ受け入れる (大文字小文字を区別しない)。
//     MVP の prompt template (`PromptTemplate.Render` cursor 経路) で「```diff ... ``` で囲んで返せ」と
//     明示するため (design.md §Patch / Resolved Decisions §6)。
//   - フェンス内側の本文は ``` を含まない最長マッチで抽出 (lazy match)。
//   - `(?s)` を付けて `.` を改行にもマッチさせる multi-line 抽出。
var codeBlockDiffRegex = regexp.MustCompile("(?si)```diff[ \\t]*\\r?\\n(.*?)```")

// jsonDiffPayload は claude/codex 想定の JSON Schema 応答 (`{"diff": "..."}`) を受け取る型。
// 不要フィールドは無視 (`json.Unmarshal` の既定動作) し、`diff` キー有無のみを検出する。
type jsonDiffPayload struct {
	// Diff は AI が返した unified diff 本文。空文字列のときは「JSON 形式は満たすが diff 不在」として
	// raw text fallback に降下する契約 (provider 応答の形式揺れに耐える)。
	Diff string `json:"diff"`
}

// ExtractAndGuard は AI 応答 raw text から unified diff を抽出し、対象ファイル scope guard を行う。
//
// 抽出戦略 (両形式 fallback):
//  1. JSON Schema 経路: `{"diff": "..."}` 形式の JSON を `json.Unmarshal` で抽出 (claude/codex 想定)。
//  2. 生 text 経路: `(?si)\x60\x60\x60diff ... \x60\x60\x60` コードブロックを regex で抽出 (cursor 想定 + claude/codex 揺れ吸収)。
//
// scope guard:
//   - 抽出した diff 内の全 `diff --git a/<path> b/<path>` 行から path を集める。
//   - 各 path を cwd と filepath.Join で結合 → filepath.Clean → expectedTarget の clean path と比較。
//   - 1 つでも不一致があれば ErrOutOfScope。全要素が一致 (= 単一ファイルかつ expectedTarget) なら成功。
//
// 失敗時 sentinel:
//   - いずれの経路でも diff を抽出できない → ErrUnparseable
//   - 抽出はできたが対象ファイル外を変更しようとする → ErrOutOfScope
//
// Preconditions:
//   - expectedTarget は絶対 clean path を期待 (resolver_test.go と同形式)。
//   - cwd は絶対パス (orchestration から moduleRoot を渡す契約 / tasks.md 11.1)。
//
// Postconditions:
//   - 成功時 Patch.TargetFiles == [filepath.Clean(expectedTarget)] かつ Patch.DiffBytes は非空。
func ExtractAndGuard(rawAgentText string, expectedTarget string, cwd string) (Patch, error) {
	// Step 1: 両形式 fallback で diff 本文を抽出する。
	// JSON 経路を優先し、失敗 (構造不一致 or diff キー空) なら生 text 経路に降下する。
	diffBody, ok := extractDiffBody(rawAgentText)
	if !ok {
		return Patch{}, fmt.Errorf("%w: provider 応答に JSON / 生 text いずれの形式でも diff が見つからない", ErrUnparseable)
	}

	// Step 2: diff 内の `diff --git a/<path> b/<path>` 行を全件抽出する。
	// 1 件も見つからない場合は「diff 形式ではない」として ErrUnparseable に集約する。
	matches := diffGitLineRegex.FindAllStringSubmatch(diffBody, -1)
	if len(matches) == 0 {
		return Patch{}, fmt.Errorf("%w: 抽出された diff に `diff --git a/<path> b/<path>` 行が含まれない", ErrUnparseable)
	}

	// Step 3: scope guard。各 `diff --git a/<path> b/<path>` 行から path を抽出し、
	// cwd 結合後の絶対 clean path が expectedTarget と一致するかを検証する。
	expectedClean := filepath.Clean(expectedTarget)
	collected := make([]string, 0, len(matches))
	for _, m := range matches {
		// m[1] = a/ 側 path、m[2] = b/ 側 path。MVP では両者一致を必須とする (rename 非対応)。
		aPath := m[1]
		bPath := m[2]
		if aPath != bPath {
			return Patch{}, fmt.Errorf("%w: rename diff は MVP 非対応 (a=%q b=%q)", ErrOutOfScope, aPath, bPath)
		}
		// cwd と join し clean 後、expectedTarget と一致するか検証する。
		// cwd が絶対 clean path、aPath が相対 path のため、Join → Clean で正規化された絶対パスになる。
		absCandidate := filepath.Clean(filepath.Join(cwd, aPath))
		if absCandidate != expectedClean {
			return Patch{}, fmt.Errorf("%w: diff 内の path が target と一致しない (got=%q want=%q)",
				ErrOutOfScope, absCandidate, expectedClean)
		}
		collected = append(collected, absCandidate)
	}

	// 全 path が expectedTarget と一致した。MVP では len(collected) == 1 が期待されるが、
	// 同一 path が複数 hunk として現れるケース (理論上は 1 ファイルに対する 1 diff --git 行なので
	// 通常は 1 件) には冪等的に対応する。重複除去は MVP では行わず、orchestration 側の
	// `git apply --check` に委ねる。
	return Patch{
		TargetFiles: dedupStrings(collected),
		DiffBytes:   []byte(diffBody),
	}, nil
}

// extractDiffBody は両形式 fallback で diff 本文文字列を抽出する。
// 戻り値 ok=true のとき diff 本文 (string)、false のときは JSON / 生 text いずれの形式でも
// 抽出できなかったことを表す。
//
// 抽出順序:
//  1. JSON Schema 経路 (`{"diff": "..."}`) を `json.Unmarshal` で試行する。
//     成功かつ diff フィールドが非空 → diff 本文として返す。
//     JSON 構造不一致 / diff フィールド空 → 生 text 経路に降下。
//  2. 生 text 経路: `(?si)\x60\x60\x60diff ... \x60\x60\x60` コードブロックを regex で抽出する。
//     コードブロック内側 (フェンス除去後の本文) を返す。
//     見つからない → JSON / 生 text いずれにも該当せず ok=false。
func extractDiffBody(rawAgentText string) (string, bool) {
	// JSON Schema 経路 (claude/codex 想定 + claude/codex 応答が JSON 形式で返るケース)。
	// `json.Unmarshal` は前後の空白文字を許容しないため、trim してから解析する。
	trimmed := strings.TrimSpace(rawAgentText)
	if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
		var payload jsonDiffPayload
		if err := json.Unmarshal([]byte(trimmed), &payload); err == nil {
			if payload.Diff != "" {
				return payload.Diff, true
			}
			// JSON 構造は満たすが diff キー空 → 生 text 経路に降下する (provider 応答の揺れを吸収)。
		}
	}

	// 生 text 経路 (cursor 想定 + JSON 経路失敗時の fallback)。
	if m := codeBlockDiffRegex.FindStringSubmatch(rawAgentText); len(m) >= 2 {
		// m[1] = フェンス内側のコードブロック本文。前後の余分な空白行は trim せず、
		// `git apply` がそのまま受容する形で返す (末尾改行がないと `git apply` が拒否するケースを避ける)。
		body := m[1]
		// `git apply` 入力末尾改行の安全のため、改行で終わっていなければ補う。
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		return body, true
	}

	return "", false
}

// dedupStrings は string slice の重複を順序保持で除去する小ヘルパー。
// MVP では len(collected) == 1 がほぼ確定だが、将来 multi-hunk diff が同じ path を複数回
// 含むケース (極めて稀) に備えて冪等的に実装する。`bytes.Buffer` 等は使わず最小実装。
func dedupStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
