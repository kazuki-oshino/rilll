// Package kiro の本ファイルは、kiro-impl-loop の終端 final-review (5 フェーズ Sequential
// Pipeline: Phase 1 inquiry loop / Phase 2-3 fix-edit + fix-judge / Phase 4 test-edit +
// test-judge / Phase 5 new commit) を駆動する helper を集約する (.kiro/specs/kiro-validate-impl-loop/
// design.md 参照)。
//
// 本ファイルは Foundation 層 (Task 1.1) として、後続フェーズで共有される下記の純粋ヘルパと
// 値型のみを定義する:
//   - 値型: finalReviewFixResult / finalReviewTestResult
//   - parser: parseChangedFilesCount / parseTestResult
//
// orchestration 本体 (runFinalReview), service helper (runFinalReviewFixPhase /
// runFinalReviewTestVerification / runFinalReviewCommit), prompt builder
// (buildFinalReview*Prompt), persistence helper (formatTestResultMarkdown / persistTestResult)
// は本 spec の後続タスク (Task 1.2 以降) で本ファイルか兄弟ファイルに追加される。
package kiro

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/kazuki-oshino/rilll/internal/facet"
	"github.com/kazuki-oshino/rilll/internal/inquiryloop"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	gitworkflow "github.com/kazuki-oshino/rilll/internal/workflow/builtin/git"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// finalReviewFixResult は Phase 2 (fix-edit) と Phase 3 (fix-judge) の合成結果を表す値型。
//
// spec_loop の RunResultStepFix と異なり、本型は impl_loop 専用に設計されており result.json
// 永続化を前提としない (永続化は formatFixResultMarkdown 経由で markdown 形式で行う)。
// design.md「runFinalReviewFixPhase」「finalReviewFixResult」契約に従う。
//
// ChangedFiles (int) は judge summary 末尾の "Files Modified: <count>" 行から
// parseChangedFilesCount で抽出した修正ファイル数を保持する。Phase 4 進行判定に使われる:
//   - ChangedFiles >= 1: Phase 4 (全件 test 再検証) へ進む
//   - ChangedFiles == 0: Phase 4-5 を skip (修正なし許容、Req 2.5 / 3.6)
//
// OK == false のとき (Turn 1/2 失敗 / schema-violation / has_inquiry=true 残留) は
// 呼び出し側 (runFinalReview) で WorkflowError 発火 + run 停止 (Req 2.6 / 6.2)。
type finalReviewFixResult struct {
	// Ran は fix phase helper が実際に呼び出されたかを表す (helper を呼んだ事実が true)。
	Ran bool
	// OK は fix phase 全体が成功したか (Turn 1 success && Turn 2 success && schema OK
	// && has_inquiry=false && Files Modified marker 抽出成功) を表す。
	OK bool
	// ChangedFiles は Turn 2 (fix-judge) summary の "Files Modified: <count>" 行から
	// parseChangedFilesCount で抽出した数値。OK==false のときは未定義 (0 にフォールバック)。
	ChangedFiles int
	// Summary は judge ターン応答の summary フィールド。Turn 2 失敗時は Turn 1 の Content
	// (markdown) を救出して入れる。
	Summary string
	// Message は失敗時の人間向け説明 (例: "agent error: ...", "Files Modified marker missing")。
	Message string
	// SessionID は fix phase 全体の最終応答 sessionId (監査用)。
	// 通常は Turn 2 (fix-judge) の sessionId、Turn 2 失敗時は Turn 1 (fix-edit) の sessionId。
	SessionID string
	// EditSession は Turn 1 (fix-edit、OutputFormat=nil でファイル編集を許可) の応答 sessionId。
	// Phase 4 Turn 1 (test-edit) の resume 起点として使われる (cross-restriction resume 回避)。
	EditSession string
	// JudgeSession は Turn 2 (fix-judge、Schema 強制で要約のみ抽出) の応答 sessionId。
	// Turn 1 失敗で Turn 2 を skip した場合は空文字列。
	JudgeSession string
}

// finalReviewTestResult は Phase 4 (test-edit + test-judge、2-turn 構造) の合成結果を表す値型。
//
// design.md「runFinalReviewTestVerification」「finalReviewTestResult」契約に従う。
//
// Turn 1 (test-edit、OutputFormat=nil) で agent が同 session 内で test → fail → fix → re-test
// を最大 5 回まで iterate し、Turn 2 (test-judge、Schema + fixJudgeAllowedTools) で
// "Test Result: pass|fail" と "Files Modified: <count>" を構造化抽出する。
//
// TestPassed == OK の関係 (Turn 2 で "Test Result: pass" を抽出できた == テスト pass)。
// 表記揺れ防止のため両 field を併記する (runFinalReview の switch 分岐で使い分ける)。
type finalReviewTestResult struct {
	// Ran は test verification helper が実際に呼び出されたかを表す。
	Ran bool
	// OK は Phase 4 全体が成功したか (Turn 1/2 success && Test Result marker 抽出成功 &&
	// pass) を表す。実質的に TestPassed と同値。
	OK bool
	// TestPassed は Turn 2 summary から parseTestResult で抽出した結果。
	// pass → true / fail → false / 未出力 (schema-violation) → false (OK=false 経由で表現)。
	TestPassed bool
	// ChangedFiles は Turn 2 summary の "Files Modified: <count>" 行から
	// parseChangedFilesCount で抽出した数値 (Turn 1 内で発生した修正ファイル数)。
	ChangedFiles int
	// Summary は Turn 2 (test-judge) 応答の summary フィールド。Turn 2 失敗時は
	// Turn 1 (test-edit) の Content (markdown) を救出して入れる。
	Summary string
	// Message は失敗時の人間向け説明。
	Message string
	// SessionID は Phase 4 全体の最終応答 sessionId (監査用)。
	// 通常は Turn 2 (test-judge) の sessionId、Turn 2 失敗時は Turn 1 (test-edit) の sessionId。
	SessionID string
	// EditSession は Turn 1 (test-edit、OutputFormat=nil で Bash + Edit/Write/Read を許可)
	// の応答 sessionId。
	EditSession string
	// JudgeSession は Turn 2 (test-judge、Schema 強制で要約のみ抽出) の応答 sessionId。
	// Turn 1 失敗で Turn 2 を skip した場合は空文字列。
	JudgeSession string
}

// changedFilesMarkerRE は judge summary に必須の "Files Modified: <count>" 行を抽出する正規表現。
// (?m) で multi-line mode を有効化し、^ と $ をそれぞれ行頭・行末に固定する。
// `\s*` で `:` と数値の間の半角空白 / タブのみを許容し、`\d+` で ASCII 半角数字のみマッチする
// (全角数字 / 数値以外 / 末尾余剰文字 はすべて非マッチ)。
var changedFilesMarkerRE = regexp.MustCompile(`(?m)^Files Modified:\s*(\d+)$`)

// parseChangedFilesCount は fix-judge (Phase 3) / test-judge (Phase 4 Turn 2) の summary から
// "Files Modified: <count>" 行を抽出して count を int で返す。
//
// 受理条件 (すべてを満たす):
//   - 行頭固定 (multi-line ^ アンカー / 先頭 whitespace 不可)
//   - 大文字小文字一致 ("Files Modified" 完全一致、case sensitive)
//   - ASCII 半角数字のみ (全角数字 / 文字列値は不可)
//   - 行末固定 (multi-line $ アンカー / 末尾余剰文字不可)
//
// マーカ未出力 / 行頭以外の出現 / 大文字小文字違い / 全角数字 / 数値以外 / 末尾余剰のいずれも
// schema-violation として error を返す。呼び出し側は OK=false に倒して WorkflowError 発火する
// (Req 2.5 / 3.6: 構造化契約に従わない応答を成功扱いにしない)。
func parseChangedFilesCount(summary string) (int, error) {
	m := changedFilesMarkerRE.FindStringSubmatch(summary)
	if m == nil {
		return 0, fmt.Errorf("Files Modified marker not found in summary")
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		// 正規表現で `\d+` まで絞り込んでいるため通常到達しない防衛的分岐。
		return 0, fmt.Errorf("Files Modified count parse failed: %w", err)
	}
	return n, nil
}

// testResultMarkerRE は test-judge summary に必須の "Test Result: pass|fail" 行を抽出する正規表現。
// changedFilesMarkerRE と同じ multi-line + 行頭/行末固定方針を採る。
// `pass` / `fail` のみを受理し、それ以外の値 (例: PASS / skipped / pass (3 suites)) は非マッチ。
var testResultMarkerRE = regexp.MustCompile(`(?m)^Test Result:\s*(pass|fail)$`)

// parseTestResult は test-judge (Phase 4 Turn 2) の summary から "Test Result: pass|fail" 行を
// 抽出し、pass → true / fail → false を返す。マーカ未出力 / それ以外の値は schema-violation
// として error を返す (Req 3.4 / 3.5)。
//
// 受理条件は parseChangedFilesCount と同じく行頭/行末固定 + case sensitive + 末尾余剰不可。
func parseTestResult(summary string) (bool, error) {
	m := testResultMarkerRE.FindStringSubmatch(summary)
	if m == nil {
		return false, fmt.Errorf("Test Result marker not found in summary")
	}
	switch m[1] {
	case "pass":
		return true, nil
	case "fail":
		return false, nil
	default:
		// 正規表現の (pass|fail) で絞り込んでいるため通常到達しない防衛的分岐。
		return false, fmt.Errorf("Test Result unexpected value: %q", m[1])
	}
}

// =============================================================================
// Final Review Prompt Builders
// =============================================================================
//
// final-review (5-Phase Sequential Pipeline) の各 Turn で agent に投げる prompt 本文を組み立てる
// 純関数群。spec_loop の `buildFixEditPrompt` / `buildFixJudgePrompt` (.kiro/specs/<feature>/ 配下の
// docs 編集前提) を、impl_loop の **実装コード編集 + 全件 test 再検証** 用に書き直したもの。
//
// 各 prompt 本文の冒頭に置かれる固定マーカ (`[final-review-fix-edit]` /
// `[final-review-fix-judge]` / `[final-review-test-edit]` / `[final-review-test-judge]`) は、
// classifyAgentCall や運用ログでの phase 識別マーカ役を兼ねる。マーカを変更すると
// 既存テストの kind 判定が壊れるため、定数として固定する。

// finalReviewEditPromptHeader は Phase 2 (fix-edit; OutputFormat=nil で実装コード修正を許可) の
// プロンプト先頭固定文字列。
const finalReviewEditPromptHeader = "[final-review-fix-edit]\n直前のラウンドで final-review (validate-impl) の方針が確定しました。"

// finalReviewEditPromptTemplate は Phase 2 (fix-edit) で投げる固定プロンプト本文。
// %s に feature 名を埋める。修正対象は当該 feature の責務範囲内の **実装コード** (.kiro/specs/
// 配下の docs ではない) に限定する旨を明示する (Req 2.3)。
const finalReviewEditPromptTemplate = finalReviewEditPromptHeader + `
これらの方針に基づいて、feature %s の責務範囲内にある **実装コード** (Go ソース / テストコード /
設定ファイルなど、当該 feature が所有するもの) を実際に Edit / Write で編集して反映してください。

修正対象は当該 feature の責務範囲内に限定し、無関係なファイルや他 feature が所有するコードへの
波及修正は行わないでください。修正対象が無い・既に整合している場合は編集を行わず、その旨を
summary に記してください。

完了したら、応答末尾に以下のセクションを付けてください (json_schema 形式は要求しません):

## Summary
- 修正したファイル: <相対パスのリスト、なければ「なし」>
- 反映内容の要約 (または「変更なし」とその理由) を簡潔に書く

このターンは Edit / Write を許可した「作業フェーズ」です。
構造化出力 (json_schema) は次のターンで別途要求するため、ここでは markdown の本文として書いてください。`

// finalReviewJudgePromptHeader は Phase 3 (fix-judge; resume + json_schema 強制で要約抽出) の
// プロンプト先頭固定文字列。
const finalReviewJudgePromptHeader = "[final-review-fix-judge]\n直前ターンで実施した修正内容を要約として返してください。"

// finalReviewJudgePromptTemplate は Phase 3 (fix-judge) で投げる固定プロンプト本文。
// %s に feature 名を埋める。Edit / Write はせず、Turn 1 の作業内容を summary に詰める責務に絞る。
//
// **summary 末尾に `Files Modified: <count>` の 1 行を必須で含めること** を明文化する (Req 2.4 / 2.5)。
// このマーカは parseChangedFilesCount で行頭固定 regex 抽出され、`<count>==0` のとき Phase 4-5
// を skip / `<count>>=1` のとき Phase 4 へ進行する判定に使われる。マーカ未出力は schema-violation
// として OK=false に倒される。
const finalReviewJudgePromptTemplate = finalReviewJudgePromptHeader + `
このターンは「判定フェーズ」です。新たな編集 (Edit / Write) は行わないでください。
直前ターンで実施した修正内容を、以下の json_schema 形式の単一オブジェクトで返してください:

{
  "has_inquiry": false,
  "summary": "修正したファイルと反映内容の要約 (または「変更なし」と理由)\n\nFiles Modified: <count>"
}

feature: %s

**重要**: summary 末尾に ` + "`Files Modified: <count>`" + ` の 1 行を **必須** で行頭固定で含めてください。
- ` + "`<count>`" + ` は今回 Edit / Write で実際に修正したファイル数 (ASCII 半角数字、修正なしは 0)。
- 例: ` + "`Files Modified: 3`" + ` / ` + "`Files Modified: 0`" + `
- このマーカは後段で構造化抽出されるため、行頭以外への記載 / 大文字小文字違い / 全角数字 /
  数値以外 / 末尾余剰文字はすべて schema-violation として失敗扱いになります。

判断は変更せず、構造化出力に詰め替えるだけのターンです。`

// finalReviewTestEditPromptHeader は Phase 4 Turn 1 (test-edit; OutputFormat=nil で全件 test 実行と
// Edit/Write の iterate を許可) のプロンプト先頭固定文字列。
const finalReviewTestEditPromptHeader = "[final-review-test-edit]\nfeature の実装コードに修正が入ったため、全件 test を再検証してください。"

// finalReviewTestEditPromptTemplate は Phase 4 Turn 1 (test-edit) で投げる固定プロンプト本文。
// %s に feature 名を埋める。
//
// 設計意図 (design.md "Test command discovery" 参照):
//   - 具体 test コマンド (just test / npm test / go test 等) を **直書きしない**。
//   - 「リポジトリの test 規約 (justfile / Makefile / package.json) を発見して全件 test を実行」
//     という指示文に留める。validate-impl skill の "Discover canonical validation commands" と整合。
//
// 同 session 内 iterate 規約 (Req 3.3):
//   - test 失敗時は同 session で Bash + Edit/Write を **最大 5 回まで** iterate。
//   - 上限到達時は試行履歴 (失敗 test 名 / 直近の修正対象ファイル / 残存 failure 概要) を markdown
//     summary に含めて Turn 1 を終える。判定 (pass/fail) は Turn 2 (test-judge) で構造化抽出する。
const finalReviewTestEditPromptTemplate = finalReviewTestEditPromptHeader + `
feature: %s

このターンの責務は「全件 test を実行して pass まで仕上げる」ことです。
具体的な手順:

1. **test 規約の発見**: リポジトリ root を Read / Glob して test 規約を発見してください。
   候補は以下の優先順で確認してください (具体的な test コマンドはここで直書きしません):
   - ` + "`justfile`" + ` (just タスクランナ規約)
   - ` + "`Makefile`" + ` (make ターゲット規約)
   - ` + "`package.json`" + ` の ` + "`scripts.test`" + ` (npm 規約)
   - 上記が見つからない場合は当該言語のプロジェクト manifest (例: ` + "`go.mod`" + ` /
     ` + "`Cargo.toml`" + ` / ` + "`pyproject.toml`" + ` 等) を Read してプロジェクト規約を判断し、
     当該言語の標準 test 規約を採用してください。

2. **全件 test を Bash で実行**: 発見した test コマンドを Bash で実行し、終了コード / 失敗件数 /
   失敗 test 名を観測してください。

3. **失敗時の iterate (上限 5 回)**: test が失敗したら、同 session 内で:
   - Read / Grep で失敗箇所を debug し、
   - Edit / Write で実装コードを修正し、
   - 再び Bash で全件 test を実行する、
   というサイクルを **最大 5 回まで iterate** してください。新規 session / 別 agent run は起動
   しないでください (Req 3.3)。

4. **上限到達時の出力**: 5 回 iterate しても全件 pass にならなかった場合は、その時点で iterate を
   打ち切り、以下を markdown summary に含めて Turn 1 を終えてください:
   - 失敗した test 名のリスト
   - 直近の修正対象ファイル (最後に Edit / Write したパス)
   - 残存 failure の概要 (エラーメッセージや想定原因)

5. **応答末尾に以下のセクションを付けてください** (json_schema 形式は要求しません):

## Summary
- 実行した test コマンド: <発見した test 規約と実際に実行したコマンド>
- iterate 回数: <0-5 のいずれか>
- 修正したファイル: <相対パスのリスト、なければ「なし」>
- test 結果: <pass / fail と概要>

このターンは Bash / Edit / Write / Read / Glob / Grep を許可した「作業フェーズ」です。
構造化出力 (json_schema) は次のターンで別途要求するため、ここでは markdown の本文として書いてください。`

// finalReviewTestJudgePromptHeader は Phase 4 Turn 2 (test-judge; resume + json_schema 強制で
// 構造化抽出) のプロンプト先頭固定文字列。
const finalReviewTestJudgePromptHeader = "[final-review-test-judge]\n直前ターンで実施した全件 test 再検証の結果を要約として返してください。"

// finalReviewTestJudgePromptTemplate は Phase 4 Turn 2 (test-judge) で投げる固定プロンプト本文。
// %s に feature 名を埋める。Edit / Write / Bash はせず、Turn 1 の test 実行結果を summary に
// 詰める責務に絞る。
//
// **summary 末尾に `Test Result: pass|fail` の 1 行と `Files Modified: <count>` の 1 行を必須で
// 含めること** を明文化する (Req 3.4)。これら 2 行は parseTestResult / parseChangedFilesCount で
// 行頭固定 regex 抽出され、Phase 5 進行 (`Test Result: pass`) / Phase 5 skip (`Test Result: fail`
// または schema-violation) の判定に使われる。
const finalReviewTestJudgePromptTemplate = finalReviewTestJudgePromptHeader + `
このターンは「判定フェーズ」です。新たな編集 (Edit / Write) や Bash 実行は行わないでください。
直前ターンで実施した全件 test 再検証の結果を、以下の json_schema 形式の単一オブジェクトで返してください:

{
  "has_inquiry": false,
  "summary": "test 実行結果と修正概要の要約\n\nTest Result: <pass|fail>\nFiles Modified: <count>"
}

feature: %s

**重要**: summary 末尾には以下の 2 行を **必須** で行頭固定で含めてください:

1. ` + "`Test Result: pass`" + ` または ` + "`Test Result: fail`" + ` (どちらかの値のみ、case sensitive)
   - Turn 1 内の最終 test 実行が全件 pass で終わったなら ` + "`pass`" + `、
     1 件でも fail が残っていたなら ` + "`fail`" + ` を選んでください。

2. ` + "`Files Modified: <count>`" + ` (ASCII 半角数字、Turn 1 内で Edit / Write したファイル数)
   - 例: ` + "`Files Modified: 3`" + ` / ` + "`Files Modified: 0`" + `

これら 2 行は後段で構造化抽出されるため、行頭以外への記載 / 大文字小文字違い / 全角数字 /
数値以外 / 末尾余剰文字 / 想定外の値 (PASS / skipped 等) はすべて schema-violation として
失敗扱いになります。

判断は変更せず、Turn 1 の作業内容を構造化出力に詰め替えるだけのターンです。`

// buildFinalReviewEditPrompt は Phase 2 (fix-edit) の prompt 本文を組み立てる純関数。
// feature 名を finalReviewEditPromptTemplate に埋め込んで返す (Req 2.1, 2.3, 7.2)。
func buildFinalReviewEditPrompt(feature string) string {
	return fmt.Sprintf(finalReviewEditPromptTemplate, feature)
}

// buildFinalReviewJudgePrompt は Phase 3 (fix-judge) の prompt 本文を組み立てる純関数。
// feature 名を finalReviewJudgePromptTemplate に埋め込んで返す (Req 2.4, 2.5, 7.2)。
func buildFinalReviewJudgePrompt(feature string) string {
	return fmt.Sprintf(finalReviewJudgePromptTemplate, feature)
}

// buildFinalReviewTestEditPrompt は Phase 4 Turn 1 (test-edit) の prompt 本文を組み立てる純関数。
// feature 名を finalReviewTestEditPromptTemplate に埋め込んで返す (Req 3.1, 3.2, 3.3, 7.2)。
//
// 具体 test コマンドは直書きせず、justfile / Makefile / package.json の発見指示に留める設計
// (design.md "Test command discovery")。
func buildFinalReviewTestEditPrompt(feature string) string {
	return fmt.Sprintf(finalReviewTestEditPromptTemplate, feature)
}

// buildFinalReviewTestJudgePrompt は Phase 4 Turn 2 (test-judge) の prompt 本文を組み立てる純関数。
// feature 名を finalReviewTestJudgePromptTemplate に埋め込んで返す (Req 3.4, 7.2)。
//
// summary 末尾に `Test Result: pass|fail` と `Files Modified: <count>` の 2 行を必須出力させる
// 指示文を含む。
func buildFinalReviewTestJudgePrompt(feature string) string {
	return fmt.Sprintf(finalReviewTestJudgePromptTemplate, feature)
}

// =============================================================================
// Phase 4 Persistence (test-result.md)
// =============================================================================
//
// Phase 4 (test-edit + test-judge) 専用の永続化 helper。spec_loop の `formatFixResultMarkdown` /
// `persistFixResult` (Phase 2-3 / fix-result.md 専用) とは独立形式で、`<NN>-final-review/test-result.md`
// に書き出す (Req 5.1 / 5.2 / design.md "Phase 4 専用永続化 helper")。
//
// 独立形式である理由:
//   - fix-result.md と test-result.md を 1 つの `<NN>-final-review/` 下に並列に置くため、
//     ヘッダ (`# Fix Result` / `# Test Result`) と metadata 行ラベルを衝突させない方が、
//     後段の人間レビューや grep ベース解析で識別しやすい
//   - Phase 4 固有の `Test Result` / `Files Modified` 必須行を独自セクションで明示することで、
//     fix-result.md と書き分けの責任を読み取りやすくする

// formatTestResultMarkdown は test-result.md の本文を組み立てる。
//
// Phase 4 (test-edit + test-judge、2-turn 構造) に対応した独立形式:
//
//	# Test Result <NN>-<phase>
//
//	- order: <N>
//	- phase: <phase>
//	- persona: <personaName>
//	- ok: <true|false>
//	- testPassed: <true|false>
//	- changedFiles: <count>
//	- sessionId: <SessionID>
//	- editSession: <EditSession>   (空なら省略)
//	- judgeSession: <JudgeSession> (空なら省略)
//
//	## Edit Phase Prompt           (editPrompt="" なら省略)
//	<editPrompt>
//
//	## Judge Phase Prompt          (judgePrompt="" なら省略 / Turn 2 skip 時)
//	<judgePrompt>
//
//	## Test Result
//	Test Result: <pass|fail>
//
//	## Files Modified
//	Files Modified: <count>
//
//	## Summary
//	<test.Summary>
//
//	## Message
//	<test.Message>
//
//	## SessionID
//	<test.SessionID>
//
//	## EditSession
//	<test.EditSession>
//
//	## JudgeSession
//	<test.JudgeSession>
//
// `Test Result` / `Files Modified` セクションには、agent の judge summary に強制した必須マーカ
// (`Test Result: pass|fail` / `Files Modified: <count>`) と同じ行ラベル形式を再現する。これにより、
// 永続化された markdown 単独でも `parseTestResult` / `parseChangedFilesCount` の正規表現で
// 値を再抽出できる (障害解析時の手動 recovery を最小化、Req 5.1)。
//
// 編集 / 判定 prompt が空文字列のときは該当セクション自体を省略する (Turn 2 を skip した
// best-effort パスでの冗長セクション抑止)。
func formatTestResultMarkdown(order int, phase, personaName, editPrompt, judgePrompt string, test finalReviewTestResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Test Result %02d-%s\n\n", order, phase)
	fmt.Fprintf(&b, "- order: %d\n", order)
	fmt.Fprintf(&b, "- phase: %s\n", phase)
	fmt.Fprintf(&b, "- persona: %s\n", personaName)
	fmt.Fprintf(&b, "- ok: %t\n", test.OK)
	fmt.Fprintf(&b, "- testPassed: %t\n", test.TestPassed)
	fmt.Fprintf(&b, "- changedFiles: %d\n", test.ChangedFiles)
	fmt.Fprintf(&b, "- sessionId: %s\n", test.SessionID)
	if test.EditSession != "" {
		fmt.Fprintf(&b, "- editSession: %s\n", test.EditSession)
	}
	if test.JudgeSession != "" {
		fmt.Fprintf(&b, "- judgeSession: %s\n", test.JudgeSession)
	}
	b.WriteString("\n")

	if editPrompt != "" {
		fmt.Fprintf(&b, "## Edit Phase Prompt\n\n%s\n\n", editPrompt)
	}
	if judgePrompt != "" {
		fmt.Fprintf(&b, "## Judge Phase Prompt\n\n%s\n\n", judgePrompt)
	}

	// Test Result セクション: 必須マーカ "Test Result: pass|fail" を本文に書く。
	// `parseTestResult` の (?m)^Test Result:\s*(pass|fail)$ で再抽出可能な行頭固定形式。
	testResultLabel := "fail"
	if test.TestPassed {
		testResultLabel = "pass"
	}
	fmt.Fprintf(&b, "## Test Result\n\nTest Result: %s\n\n", testResultLabel)

	// Files Modified セクション: 必須マーカ "Files Modified: <count>" を本文に書く。
	// `parseChangedFilesCount` の (?m)^Files Modified:\s*(\d+)$ で再抽出可能な行頭固定形式。
	fmt.Fprintf(&b, "## Files Modified\n\nFiles Modified: %d\n\n", test.ChangedFiles)

	fmt.Fprintf(&b, "## Summary\n\n%s\n\n", test.Summary)
	fmt.Fprintf(&b, "## Message\n\n%s\n\n", test.Message)
	fmt.Fprintf(&b, "## SessionID\n\n%s\n\n", test.SessionID)
	fmt.Fprintf(&b, "## EditSession\n\n%s\n\n", test.EditSession)
	fmt.Fprintf(&b, "## JudgeSession\n\n%s\n", test.JudgeSession)

	return b.String()
}

// persistTestResult は Phase 4 (test-edit + test-judge) の結果を
// `<runDir>/<NN>-<phase>/test-result.md` として書き出す。
//
// path 規約 (Req 5.2):
//   - `<NN>` は `stepDir(runDir, order, phase)` 経由で zero-padded 2 桁 (例: order=7, phase=
//     "final-review" → `07-final-review/test-result.md`)
//   - per-task ループ artifact (`<NN>-impl-<task>/`) と prefix (`final-review` / `impl-`) で
//     完全に分離されるため、path 衝突は起きない
//
// best-effort: 永続化失敗 (`MkdirAll` / `WriteFile`) は呼び出し側 (`runFinalReviewTestVerification`)
// に伝播させず error を返すのみで、上位は無視する設計 (`runFinalReview` は OK 判定で進路を決め、
// 永続化の成否で run の停止/継続を変えない)。
//
// editPrompt / judgePrompt は省略可: Turn 2 (judge) を skip した場合は judgePrompt="" で渡す。
// この場合 `formatTestResultMarkdown` 側で対応セクション自体が出力されない。
func persistTestResult(runDir string, order int, phase, personaName, editPrompt, judgePrompt string, test finalReviewTestResult) error {
	dir := stepDir(runDir, order, phase)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("persistTestResult: mkdir %s: %w", dir, err)
	}
	out := filepath.Join(dir, "test-result.md")
	body := formatTestResultMarkdown(order, phase, personaName, editPrompt, judgePrompt, test)
	if err := os.WriteFile(out, []byte(body), 0o644); err != nil {
		return fmt.Errorf("persistTestResult: write %s: %w", out, err)
	}
	return nil
}

// =============================================================================
// Phase 2-3 Service Helper (runFinalReviewFixPhase)
// =============================================================================
//
// Phase 2 (fix-edit) と Phase 3 (fix-judge) を 2-turn 構成で駆動する service helper。
// kiro-spec-loop の `runFixPhase` (spec_loop.go L792) と同一の Turn 構造 (Turn 1: OutputFormat=nil
// + Edit/Write 解放、Turn 2: Schema 強制 + fixJudgeAllowedTools 制限) を採用しつつ、impl_loop 専用
// に下記を独自化する:
//   - 戻り値型を impl_loop 専用 `finalReviewFixResult` に変える (ChangedFiles int を持つ)
//   - prompt template を `buildFinalReviewEditPrompt` / `buildFinalReviewJudgePrompt` に差し替える
//     (.kiro/specs/<feature>/ 配下 docs ではなく **実装コード** を編集対象にする)
//   - StepID を固定の `final-review-fix-edit` / `final-review-fix-judge` にする (per-task ループ
//     `fix-<NN>-<phase>-edit` / `fix-<NN>-<phase>-judge` 等とは別系統で akashic 識別する)
//   - judge summary に必須化した `Files Modified: <count>` 行を `parseChangedFilesCount` で抽出
//     (マーカ未出力 / 数値以外は schema-violation として OK=false)
//
// 永続化は `formatFixResultMarkdown` / `persistFixResult` (spec_loop.go) を流用する
// (`finalReviewFixResult` → `RunResultStepFix` への詰め替えは関数末尾の persist 直前で行う)。

// COPY-OF: spec_loop.go runFixPhase (L792 起点) — keep in sync until internal/workflow/builtin/kiro/fixphase/ extraction
//
// runFinalReviewFixPhase は kiro-impl-loop の最終検証 (Phase 1) 完走後に呼ばれ、Phase 2 (fix-edit)
// と Phase 3 (fix-judge) を 2-turn で駆動する。spec_loop の `runFixPhase` を impl_loop 専用に
// rename copy したもので、シグネチャは promptStep を取らず order/phase を直接受ける。
//
// Turn 構成 (spec_loop runFixPhase と同型):
//
//   - Turn 1 (fix-edit): resume=lastSession (Phase 1 lastSession)、OutputFormat=nil で
//     実装コード編集を許可した自由記述ターン。`buildFinalReviewEditPrompt(feature)` を投げて、
//     応答末尾の "## Summary" セクションで修正内容を markdown で書かせる。stream_idle_timeout の
//     主因 (Edit 直後の json_schema 強制生成) を回避する。
//
//   - Turn 2 (fix-judge): resume=Turn 1 sessionId、OutputFormat=inquiryloop.Schema() +
//     AllowedTools=fixJudgeAllowedTools (Read/Glob/Grep のみ) で要約抽出に絞ったターン。
//     `buildFinalReviewJudgePrompt(feature)` を投げて、Turn 1 の作業内容を summary に
//     詰め直させる。**summary 末尾に `Files Modified: <count>` の 1 行が必須** で、
//     未出力時は schema-violation として OK=false。
//
// best-effort 規約 (spec_loop runFixPhase と同型):
//   - Turn 1 失敗 (agent err / success=false) → OK=false / Turn 2 をスキップして return
//   - Turn 2 失敗 (agent err / success=false / schema 違反 / has_inquiry=true /
//     Files Modified marker 未出力) → OK=false。Turn 1 が成功していれば Turn 1 の Content
//     (markdown) を `Summary` に救出する
//
// 永続化: 本関数の中で `persistFixResult` (formatFixResultMarkdown 経由) まで実行する。
// 永続化エラーは best-effort で捨てる (fix phase 自体の OK 判定には影響しない)。
//
// 戻り値:
//   - `finalReviewFixResult` を値で返す。`Ran=true` は常に成立 (helper を呼んだ事実)。
//   - 呼び出し側 `runFinalReview` は `OK==false` で WorkflowError 発火 + run 停止、
//     `OK==true && ChangedFiles>=1` で Phase 4 へ進行、
//     `OK==true && ChangedFiles==0` で Phase 4-5 を skip + WorkflowDone (Req 2.5 / 3.6)。
func runFinalReviewFixPhase(
	ctx context.Context,
	agent workflowctx.AgentCaller,
	cwd, feature, runDir string,
	order int,
	phase string,
	lastSession, personaName, promptLabel string,
	onEvent func(workflow.WorkflowEvent),
) finalReviewFixResult {
	editPrompt := buildFinalReviewEditPrompt(feature)
	judgePrompt := ""
	fix := finalReviewFixResult{Ran: true}

	// =========================================================================
	// Turn 1: fix-edit (OutputFormat=nil; Edit/Write を許可した作業ターン)
	// =========================================================================
	editOptions := &workflowctx.CallAgentOptions{
		// OutputFormat は意図的に nil。json_schema を強制しないことで Edit/Write 直後の
		// 構造化生成由来の stream_idle_timeout を回避する。
		OutputFormat: nil,
		// StepID は固定文字列 "final-review-fix-edit"。spec_loop の "fix-<NN>-<phase>-edit" と
		// 別系統にすることで、akashic agent_calls.step_id で final-review 系を一意フィルタできる。
		StepID:   "final-review-fix-edit",
		StepName: promptLabel + "-fix-edit",
		// AllowedTools=nil = SDK 既定の全ツール許可 (Edit/Write 解放)。resume session 中の
		// allowed-tools 緩和挙動 (claude-agent-sdk) に依存して実コード編集を通す (Req 2.2 / 6.2)。
		AllowedTools: nil,
	}
	editResp, editErr := agent.CallAgent(ctx, editPrompt, cwd, workflow.ForwardEvents(onEvent), lastSession, editOptions)
	switch {
	case editErr != nil:
		fix.OK = false
		fix.Message = fmt.Sprintf("final-review fix phase agent error: %v", editErr)
		// Turn 2 はスキップ (judgePrompt は "" のまま、JudgeSession も空)。
		_ = persistFixResult(runDir, order, phase, personaName, editPrompt, judgePrompt, fixResultForPersist(fix))
		return fix
	case !editResp.Success:
		fix.OK = false
		fix.SessionID = editResp.SessionID
		fix.EditSession = editResp.SessionID
		fix.Message = "final-review fix phase edit turn returned success=false"
		_ = persistFixResult(runDir, order, phase, personaName, editPrompt, judgePrompt, fixResultForPersist(fix))
		return fix
	}
	// Turn 1 成功
	fix.EditSession = editResp.SessionID
	fix.SessionID = editResp.SessionID // Turn 2 で上書きされる予定だが、Turn 2 失敗時のフォールバック値として先に入れる
	editContent := editResp.Content    // Turn 2 が失敗したときに summary として救出する候補

	// =========================================================================
	// Turn 2: fix-judge (resume + json_schema 強制で要約だけ抽出)
	// =========================================================================
	judgePrompt = buildFinalReviewJudgePrompt(feature)
	judgeOptions := &workflowctx.CallAgentOptions{
		OutputFormat: inquiryloop.Schema(),
		// StepID は固定文字列 "final-review-fix-judge" (Turn 1 と同様、final-review 系を一意識別)。
		StepID:   "final-review-fix-judge",
		StepName: promptLabel + "-fix-judge",
		// 判定ターンは Edit / Write を禁じて Read/Glob/Grep のみに絞る (spec_loop と同型)。
		// Turn 1 で済んだ実ファイル編集を再度行わせず、構造化出力への詰め替えだけを期待する。
		AllowedTools: fixJudgeAllowedTools,
	}
	judgeResp, judgeErr := agent.CallAgent(ctx, judgePrompt, cwd, workflow.ForwardEvents(onEvent), editResp.SessionID, judgeOptions)
	switch {
	case judgeErr != nil:
		fix.OK = false
		fix.Message = fmt.Sprintf("final-review fix phase judge agent error: %v", judgeErr)
		// Turn 1 は成功していたので、Content を summary として救出する。
		fix.Summary = editContent
	case !judgeResp.Success:
		fix.OK = false
		fix.JudgeSession = judgeResp.SessionID
		fix.SessionID = judgeResp.SessionID
		fix.Message = "final-review fix phase judge turn returned success=false"
		fix.Summary = editContent
	default:
		fix.JudgeSession = judgeResp.SessionID
		fix.SessionID = judgeResp.SessionID
		_, hasInquiry, summary, parseErr := inquiryloop.ParseResponse(judgeResp)
		switch {
		case parseErr != nil:
			fix.OK = false
			fix.Message = fmt.Sprintf("final-review fix phase judge schema violation: %v", parseErr)
			// Turn 1 の markdown summary を救出
			fix.Summary = editContent
		case hasInquiry:
			fix.OK = false
			fix.Summary = summary
			fix.Message = "final-review fix phase judge returned has_inquiry=true (judge ターンが構造化応答を破った)"
		default:
			// summary 自体は取れた。次に Files Modified marker を抽出する。
			// マーカ未出力 / 数値以外は schema-violation として OK=false に倒す
			// (Req 2.5: 「修正なし」の判定は構造化マーカ経由で行い、自然言語マッチを採用しない)。
			count, markerErr := parseChangedFilesCount(summary)
			if markerErr != nil {
				fix.OK = false
				fix.Summary = summary
				fix.Message = fmt.Sprintf("final-review fix phase judge schema violation: Files Modified marker missing or invalid: %v", markerErr)
			} else {
				fix.OK = true
				fix.Summary = summary
				fix.ChangedFiles = count
			}
		}
	}

	// 永続化は best-effort（失敗しても fix phase の成否判定には影響させない）。
	_ = persistFixResult(runDir, order, phase, personaName, editPrompt, judgePrompt, fixResultForPersist(fix))
	return fix
}

// =============================================================================
// Phase 4 Service Helper (runFinalReviewTestVerification)
// =============================================================================
//
// Phase 4 (test-edit + test-judge) を 2-turn 構成で駆動する service helper。
// 骨格は `runFinalReviewFixPhase` (Phase 2-3) のコピーで、Turn 構造とエラー分岐は完全対称。
// 独自化されるのは下記 3 点のみ:
//   - prompt template: `buildFinalReviewTestEditPrompt` / `buildFinalReviewTestJudgePrompt` を使う
//     (実装コード編集ではなく **全件 test の再検証 + Turn 1 内 iterate** を agent に依頼)
//   - parser: `parseTestResult` で `Test Result: pass|fail` を抽出して TestPassed を確定する
//     (`parseChangedFilesCount` も併用して Turn 1 内 iterate での修正本数を観測)
//   - persistence: `formatTestResultMarkdown` / `persistTestResult` で `<NN>-final-review/test-result.md`
//     に書き出す (`fix-result.md` と独立形式)
//
// design.md「runFinalReviewTestVerification」「finalReviewTestResult」契約に従う。
// Turn 1 の resume 起点は **Phase 2 fix-edit の sessionId** (Phase 3 fix-judge の sessionId ではない、
// cross-restriction resume を避けるため)。これは呼び出し側 `runFinalReview` で `fixResult.EditSession`
// を `editSession` 引数に渡すことで担保される。

// runFinalReviewTestVerification は kiro-impl-loop の Phase 4 (全件 test 再検証) を駆動する。
// `runFinalReviewFixPhase` と同型の 2-turn 構造で、Phase 4 専用の prompt template / parser /
// persistence helper のみ差し替えて骨格をコピー流用する (Req 3.1, 3.2, 3.3, 3.4, 3.5)。
//
// Turn 構成:
//
//   - Turn 1 (test-edit): resume=editSession (Phase 2 fix-edit sessionId、Phase 3 fix-judge ではない)、
//     OutputFormat=nil で全件 test 実行 + Turn 内 iterate (Bash + Edit/Write、最大 5 回) を許可した
//     自由記述ターン。`buildFinalReviewTestEditPrompt(feature)` を投げて、応答末尾の "## Summary"
//     セクションで test 結果と修正内容を markdown で書かせる。stream_idle_timeout の主因 (Edit 直後の
//     json_schema 強制生成) を回避する。
//
//   - Turn 2 (test-judge): resume=Turn 1 sessionId、OutputFormat=inquiryloop.Schema() +
//     AllowedTools=fixJudgeAllowedTools (Read/Glob/Grep のみ) で要約抽出に絞ったターン。
//     `buildFinalReviewTestJudgePrompt(feature)` を投げて、Turn 1 の作業内容を summary に
//     詰め直させる。**summary 末尾に `Test Result: pass|fail` と `Files Modified: <count>` の
//     2 行が必須** で、未出力時は schema-violation として OK=false。
//
// best-effort 規約 (`runFinalReviewFixPhase` と同型):
//   - Turn 1 失敗 (agent err / success=false) → OK=false / Turn 2 をスキップして return
//   - Turn 2 失敗 (agent err / success=false / schema 違反 / has_inquiry=true /
//     Test Result marker 未出力) → OK=false。Turn 1 が成功していれば Turn 1 の Content
//     (markdown) を `Summary` に救出する
//
// 永続化: 本関数の中で `persistTestResult` (formatTestResultMarkdown 経由) まで実行する。
// 永続化エラーは best-effort で捨てる (test verification 自体の OK 判定には影響しない)。
//
// 戻り値:
//   - `finalReviewTestResult` を値で返す。`Ran=true` は常に成立 (helper を呼んだ事実)。
//   - `OK == TestPassed` (Turn 2 で `Test Result: pass` を抽出できた == テスト pass)。
//   - 呼び出し側 `runFinalReview` は `OK==false` で Phase 5 を skip + WorkflowError 発火 + run 停止、
//     `OK==true` (= TestPassed=true) で Phase 5 (commit) へ進行する (Req 3.4 / 3.5)。
func runFinalReviewTestVerification(
	ctx context.Context,
	agent workflowctx.AgentCaller,
	cwd, feature, runDir string,
	order int,
	phase string,
	editSession, personaName, promptLabel string,
	onEvent func(workflow.WorkflowEvent),
) finalReviewTestResult {
	editPrompt := buildFinalReviewTestEditPrompt(feature)
	judgePrompt := ""
	test := finalReviewTestResult{Ran: true}

	// =========================================================================
	// Turn 1: test-edit (OutputFormat=nil; Bash + Edit/Write 解放で全件 test + iterate を許可)
	// =========================================================================
	editOptions := &workflowctx.CallAgentOptions{
		// OutputFormat は意図的に nil。json_schema を強制しないことで Edit/Write/Bash 直後の
		// 構造化生成由来の stream_idle_timeout を回避する。
		OutputFormat: nil,
		// StepID は固定文字列 "final-review-test-edit"。spec_loop fix phase / final-review fix phase
		// と別系統にすることで、akashic agent_calls.step_id で final-review test 系を一意フィルタできる。
		StepID:   "final-review-test-edit",
		StepName: promptLabel + "-test-edit",
		// AllowedTools=nil = SDK 既定の全ツール許可 (Bash + Edit/Write/Read/Glob/Grep 解放)。
		// resume session 中の allowed-tools 緩和挙動 (claude-agent-sdk) に依存して全件 test 実行と
		// 失敗時の iterate 修正を通す (Req 3.3)。
		AllowedTools: nil,
	}
	editResp, editErr := agent.CallAgent(ctx, editPrompt, cwd, workflow.ForwardEvents(onEvent), editSession, editOptions)
	switch {
	case editErr != nil:
		test.OK = false
		test.Message = fmt.Sprintf("final-review test verification agent error: %v", editErr)
		// Turn 2 はスキップ (judgePrompt は "" のまま、JudgeSession も空)。
		_ = persistTestResult(runDir, order, phase, personaName, editPrompt, judgePrompt, test)
		return test
	case !editResp.Success:
		test.OK = false
		test.SessionID = editResp.SessionID
		test.EditSession = editResp.SessionID
		test.Message = "final-review test verification edit turn returned success=false"
		_ = persistTestResult(runDir, order, phase, personaName, editPrompt, judgePrompt, test)
		return test
	}
	// Turn 1 成功
	test.EditSession = editResp.SessionID
	test.SessionID = editResp.SessionID // Turn 2 で上書きされる予定だが、Turn 2 失敗時のフォールバック値として先に入れる
	editContent := editResp.Content     // Turn 2 が失敗したときに summary として救出する候補

	// =========================================================================
	// Turn 2: test-judge (resume + json_schema 強制で要約だけ抽出)
	// =========================================================================
	judgePrompt = buildFinalReviewTestJudgePrompt(feature)
	judgeOptions := &workflowctx.CallAgentOptions{
		OutputFormat: inquiryloop.Schema(),
		// StepID は固定文字列 "final-review-test-judge" (Turn 1 と同様、final-review test 系を一意識別)。
		StepID:   "final-review-test-judge",
		StepName: promptLabel + "-test-judge",
		// 判定ターンは Edit / Write / Bash を禁じて Read/Glob/Grep のみに絞る (spec_loop fix-judge / impl_loop fix-judge と同型)。
		// Turn 1 で済んだ test 実行と修正を再度行わせず、構造化出力への詰め替えだけを期待する。
		AllowedTools: fixJudgeAllowedTools,
	}
	judgeResp, judgeErr := agent.CallAgent(ctx, judgePrompt, cwd, workflow.ForwardEvents(onEvent), editResp.SessionID, judgeOptions)
	switch {
	case judgeErr != nil:
		test.OK = false
		test.Message = fmt.Sprintf("final-review test verification judge agent error: %v", judgeErr)
		// Turn 1 は成功していたので、Content を summary として救出する。
		test.Summary = editContent
	case !judgeResp.Success:
		test.OK = false
		test.JudgeSession = judgeResp.SessionID
		test.SessionID = judgeResp.SessionID
		test.Message = "final-review test verification judge turn returned success=false"
		test.Summary = editContent
	default:
		test.JudgeSession = judgeResp.SessionID
		test.SessionID = judgeResp.SessionID
		_, hasInquiry, summary, parseErr := inquiryloop.ParseResponse(judgeResp)
		switch {
		case parseErr != nil:
			test.OK = false
			test.Message = fmt.Sprintf("final-review test verification judge schema violation: %v", parseErr)
			// Turn 1 の markdown summary を救出
			test.Summary = editContent
		case hasInquiry:
			test.OK = false
			test.Summary = summary
			test.Message = "final-review test verification judge returned has_inquiry=true (judge ターンが構造化応答を破った)"
		default:
			// summary 自体は取れた。次に Test Result / Files Modified marker を抽出する。
			// マーカ未出力 / それ以外の値は schema-violation として OK=false に倒す
			// (Req 3.4 / 3.5: テスト結果の判定は構造化マーカ経由で行い、自然言語マッチを採用しない)。
			test.Summary = summary
			passed, testParseErr := parseTestResult(summary)
			if testParseErr != nil {
				test.OK = false
				test.Message = fmt.Sprintf("final-review test verification judge schema violation: Test Result marker missing or invalid: %v", testParseErr)
			} else {
				test.TestPassed = passed
				// Test Result が抽出できれば、続いて Files Modified を読む。
				// Files Modified marker が不足している場合は schema-violation として OK=false にするが、
				// TestPassed の値自体は保持する (上位の persistence や監査で参照可能にする)。
				count, countErr := parseChangedFilesCount(summary)
				if countErr != nil {
					test.OK = false
					test.Message = fmt.Sprintf("final-review test verification judge schema violation: Files Modified marker missing or invalid: %v", countErr)
				} else {
					test.ChangedFiles = count
					// schema 適合 + マーカ抽出成功。OK は TestPassed と同値にする
					// (Test Result: pass → OK=true / Test Result: fail → OK=false)。
					test.OK = passed
					if !passed {
						test.Message = "final-review test verification: Test Result: fail (test 不通解消不能)"
					}
				}
			}
		}
	}

	// 永続化は best-effort（失敗しても test verification の成否判定には影響させない）。
	_ = persistTestResult(runDir, order, phase, personaName, editPrompt, judgePrompt, test)
	return test
}

// =============================================================================
// Phase 5 Service Helper (runFinalReviewCommit)
// =============================================================================
//
// Phase 5 (commit) は per-task 経路と同じ gitworkflow.CallCommit 経由で `/git-commit` を
// fresh session 起動するだけの薄い helper。判定ロジックは持たず、`agent.CallAgent` の
// 戻り値をそのまま伝播する (success=true/false / agent error すべて素通し)。上位
// `runFinalReview` が Phase 5 の終端で `WorkflowError` 発火 / `WorkflowDone` 発火を判定する
// (Req 4.4: agent error / success=false で run 停止)。
//
// design.md「runFinalReviewCommit」契約:
//   - prompt        = "/git-commit"
//   - resume        = ""                                    (fresh session 強制、Req 4.1 / 4.2)
//   - StepID        = "commit-final-review"                 (akashic フィルタ用、Req 5.4)
//   - StepName      = "最終検証 commit"
//   - OutputFormat  = nil                                   (`/git-commit` 既存挙動に委譲)
//   - AllowedTools  = nil                                   (per-task 経路と一致)
//   - provider policy: gitworkflow.CommitCallOptions に集約 (cursor-sdk / codex+max)
//   - onEvent forward: workflow.ForwardEvents(onEvent)      (gitworkflow.CallCommit 側で適用)
//
// 設計判断:
//   - resume="" を渡すことで final-review session 群 (Phase 1-4) と分離する。Phase 1-4 は
//     validate-impl agent の session 上で動くため、Phase 5 で同 session を resume すると
//     `/git-commit` slash command の挙動が validate-impl 用 system-prompt の影響を受ける
//     懸念がある。fresh session で起動して per-task 経路と同じ helper に通すことで、
//     `/git-commit` 既存契約 (空コミット作らない / hook 失敗時 success=false) をそのまま
//     継承する (Req 4.3)。
//
//   - ステージ可能差分なしの判定は `/git-commit` slash command 既存挙動に委ねる (Req 4.3)。
//     本 helper は判定をしない: 空コミット作らない / hook 失敗時 success=false 返却の挙動は
//     `/git-commit` 内部で完結し、その結果を Phase 5 が AgentResponse.Success で受ける。

// runFinalReviewCommit は Phase 5 (新規 commit 発行) の薄いラッパ。
//
// per-task 経路と同じ `/git-commit` helper を fresh session で起動し、
// `agent.CallAgent` の戻り値をそのまま伝播する。
//
// 戻り値:
//   - 成功時: provider.AgentResponse (Success=true) / err=nil
//   - 失敗時: provider.AgentResponse (Success=false) または非 nil err
//     どちらも上位 `runFinalReview` が `WorkflowError` 発火 + run 停止対象として扱う (Req 4.4)。
func runFinalReviewCommit(
	ctx context.Context,
	agent workflowctx.AgentCaller,
	cwd string,
	onEvent func(workflow.WorkflowEvent),
) (provider.AgentResponse, error) {
	return gitworkflow.CallCommit(ctx, agent, cwd, onEvent, "commit-final-review", "最終検証 commit")
}

// =============================================================================
// 5-Phase Sequential Pipeline Orchestrator (runFinalReview)
// =============================================================================
//
// runFinalReview は kiro-impl-loop の終端 final-review を駆動する単一エントリポイント。
// impl_loop.go から呼ばれ、内部で Phase 1 (inquiry loop) → Phase 2-3 (fix-edit + fix-judge)
// → Phase 4 (test-edit + test-judge) → Phase 5 (commit) の 5 フェーズを順次駆動する。
//
// 設計契約: design.md「runFinalReview」「System Flows」「Stop Policy by Failure Region」参照。
//
// 想定済み abort (Phase 1-5 のいずれかで停止) は onEvent(WorkflowError) を 1 回だけ発火し
// err=nil で return する規約 (per-task ループ / spec-loop の handleStepAbort と同じ
// 「想定済み abort は err=nil で返す」契約)。I/O 致命的エラー (engine.RunPrompt の防御的
// 伝播等) のみ fmt.Errorf でラップして propagate する。

// finalReviewMaxRounds は Phase 1 の inquiry loop の上限ラウンド数。
//
// 設計値 15 は spec_loop の validate-gap / validate-design (steps[].MaxRounds=15) と一致させ、
// 「validate-* 系は 15 まで上げる」運用契約 (issue #14 / kiro-spec-loop の事前合意) に従う。
// 修正方針が複雑な場合に複数 round で議論を収束させる余裕を確保する目的 (Req 1.5)。
const finalReviewMaxRounds = 15

// finalReviewPhase は <runDir>/<NN>-final-review/ の phase 部分に使う固定識別子。
// per-task ループ artifact (`<NN>-impl-<task>/`) と prefix で完全分離させる (Req 5.2)。
const finalReviewPhase = "final-review"

// finalReviewPromptLabel は WorkflowEvent.Label / StepName 接頭辞などに使う共通ラベル。
const finalReviewPromptLabel = "最終検証"

// finalReviewInquiryDirective は kiro-impl-loop の最終検証 (Phase 1) で
// inquiry-skill 側 (session A = /kiro-validate-impl) に毎ラウンド注入する loop policy 文字列。
// Engine.InquiryDirective として配線され、initial / resume の両方で runnerDirective() の
// 直前に再付加される。
//
// 役割: validate-impl が build / test / spec 充足の不合格を検出しても、skill 側が
// 「質問ではなく自分で直せる」と判断して has_inquiry=false を返してしまうと、
// runFinalReview は result.Rounds==0 を「問題なし」とみなして Phase 2-5 を全 skip してしまう
// (テスト失敗を検出しても修復工程を省略してrunが完了するケース)。
//
// 不合格検出時は必ず has_inquiry=true で workflow に明示通知させ、workflow が Phase 2-3
// fix-edit/fix-judge に進入できるよう源流 (validate-impl skill 自身) で規律を再強調する。
// per-task ループ向け implLoopInquiryDirective (impl_loop.go) と同形の第 3 層防御。
const finalReviewInquiryDirective = `[final-review-policy]
これは kiro-impl-loop の最終検証フェーズである。検証 (build / test / spec 充足) で
問題を検出した場合は、自力で修正できると判断しても has_inquiry=true を返し、
inquiry.question に問題内容と修正方針を必ず記述すること。
has_inquiry=false は、検証が全て pass し追加修正が不要な場合のみ使うこと。`

// runFinalReview は kiro-impl-loop の終端 final-review 5 フェーズ orchestration を駆動する。
//
// Phase 1 (inquiry loop):
//   - inquiryloop.Engine.RunPrompt で /kiro-validate-impl を MaxRounds=15 で駆動
//   - Rounds==0 && StatusCompleted → no-issue 即完走 (Phase 2-5 skip、WorkflowDone 発火、nil return) (Req 1.3)
//   - Rounds>=1 && StatusCompleted → Phase 2 へ進行 (Req 1.4)
//   - StatusAborted (loop-limit / prompt-failure / schema-violation) → WorkflowError 発火、nil return (Req 1.6)
//
// Phase 2-3 (fix-edit + fix-judge):
//   - runFinalReviewFixPhase を呼び出し、resume 起点に Phase 1 LastSession を渡す
//   - finalReviewFixResult.OK==false → WorkflowError 発火、nil return (Req 2.6)
//   - ChangedFiles==0 → Phase 4-5 skip (修正なし許容)、WorkflowDone 発火、nil return (Req 2.5 / 3.6)
//   - ChangedFiles>=1 → Phase 4 へ進行
//
// Phase 4 (test-edit + test-judge):
//   - runFinalReviewTestVerification を呼び出し、resume 起点に **Phase 2 fix-edit sessionId**
//     (= fixResult.EditSession) を渡す。fix-judge sessionId は cross-restriction resume を
//     避けるため使わない (design.md "Resume source rationale")
//   - finalReviewTestResult.OK==false → WorkflowError 発火、nil return (Req 3.5)
//     失敗種別は Message 文字列にエンコード:
//   - test-edit (Turn 1) 失敗 (JudgeSession 空) → "test-edit"
//   - test-judge Test Result: fail → "test-failed"
//   - test-judge schema-violation → "schema-violation"
//   - OK==true (= TestPassed=true) → Phase 5 へ進行
//
// Phase 5 (commit):
//   - runFinalReviewCommit で /git-commit を fresh session で起動
//   - agent error / success=false → WorkflowError 発火、nil return (Req 4.4)
//   - 成功 → WorkflowDone 発火、nil return (Req 4.1 / 4.3)
//
// 戻り値:
//   - 想定済み abort: onEvent(WorkflowError) を 1 回発火後、err=nil で return
//   - 完走 (任意 phase で正常終了): onEvent(WorkflowDone) を 1 回発火後、err=nil で return
//   - I/O 致命的エラー (engine.RunPrompt の防御的伝播 等): fmt.Errorf でラップして propagate
func runFinalReview(
	ctx context.Context,
	agent workflowctx.AgentCaller,
	cwd, runDir, feature string,
	order int,
	onEvent func(workflow.WorkflowEvent),
	implPersona inquiryloop.AnswerPersona,
) error {
	// ===== Phase 1: inquiry loop =====
	emitText(onEvent, "最終検証 Phase 1 (inquiry loop) 開始\n")

	// stepDirAbs は inquiryloop の AnswerSourcePathFunc が round-N-answer.md の絶対パスを
	// 構築するために使う (resume 呼び出しの "Source: <path>" 行用、stream_idle_timeout 回避)。
	stepDirAbs := stepDir(runDir, order, finalReviewPhase)

	// Phase 1 で session A (validate-impl skill) に投げる初回プロンプト。spec_loop の
	// validate-gap / validate-design と同形式の "/<skill> <feature>" コマンド。
	promptCmd := "/kiro-validate-impl " + feature

	// 回答エージェント (session B) に「どの spec のレビューか」を伝えるための AnswerContext。
	// session B は毎ラウンド fresh session で呼ばれるため、初回 PromptCommand と関連 spec
	// ファイルの絶対パスを workflow 層から明示注入しないと判別不能。
	specDir := filepath.Join(cwd, ".kiro", "specs", feature)
	specPaths := []string{
		filepath.Join(specDir, "requirements.md"),
		filepath.Join(specDir, "design.md"),
		filepath.Join(specDir, "tasks.md"),
	}
	answerPolicies, err := resolveAnswerPolicies(cwd, implAnswerPolicyName)
	if err != nil {
		if errors.Is(err, facet.ErrFacetNotFound) || errors.Is(err, errEmptyAnswerPolicy) {
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: fmt.Sprintf("answer policy %q is unavailable: %v", implAnswerPolicyName, err)})
			return nil
		}
		return fmt.Errorf("runFinalReview: resolveAnswerPolicies(%s): %w", implAnswerPolicyName, err)
	}

	// inquiryloop.Engine の adapter (workflowctx.AgentCaller → inquiryloop.AgentCaller の橋渡し)。
	// agentCallerAdapter は spec_loop.go で既出 (改修なし)。
	adapter := agentCallerAdapter{inner: agent, onEvent: onEvent}

	engine := inquiryloop.Engine{
		Persona:   implPersona,
		MaxRounds: finalReviewMaxRounds,
		Schema:    inquiryloop.Schema(),
		AnswerContext: inquiryloop.AnswerContext{
			Policies:      answerPolicies,
			PromptCommand: promptCmd,
			SpecPaths:     specPaths,
		},
		// issue #61: validate-impl が不合格検出時に has_inquiry=false を返してしまい
		// Phase 2-5 が skip されてしまう事故を抑止する第 3 層防御。
		InquiryDirective: finalReviewInquiryDirective,
		StepIDFunc: func(round int, kind string) string {
			// per-task ループ ("impl-<taskID>-<kind>-r<N>") と別系統で akashic フィルタ可能にする。
			return fmt.Sprintf("final-review-%s-r%d", kind, round)
		},
		AnswerSourcePathFunc: func(round int) string {
			// resume 時は長い answerText を切り詰め、全文を Source パスで参照させる。
			// stream_idle_timeout のリスクを下げるため、persistAnswer 済みのパスを返す。
			return filepath.Join(stepDirAbs, fmt.Sprintf("round-%d-answer.md", round))
		},
	}

	result, runErr := engine.RunPrompt(
		ctx,
		adapter,
		cwd,
		inquiryloop.RunInput{
			PromptCommand: promptCmd,
			PromptLabel:   finalReviewPromptLabel,
		},
		func(round inquiryloop.Round) {
			// 永続化失敗 (disk full 等) は engine に伝播させずベストエフォート扱い。
			_ = persistInquiry(runDir, order, finalReviewPhase, round.Index, round.Inquiry)
			_ = persistAnswer(runDir, order, finalReviewPhase, round.Index, implPersona.Name, round.AnswerText)
		},
	)
	if runErr != nil {
		// engine.RunPrompt は基本 err=nil で FinalResult に詰めて返すが、防御的に propagate。
		return fmt.Errorf("runFinalReview: engine.RunPrompt: %w", runErr)
	}

	if result.Status == inquiryloop.StatusAborted {
		// Phase 1 abort: Phase 2-5 skip、WorkflowError 発火 (Req 1.6)。
		// Termination の文字列値 (loop-limit / prompt-failure / schema-violation) を Message に
		// 直接含めることで、後段テスト (シナリオ 4a 等) が substring 一致で abort 種別を観測できる。
		msg := fmt.Sprintf("中断: 最終検証 inquiry loop が %s で abort", string(result.Termination))
		if result.Failure != nil && strings.TrimSpace(result.Failure.Message) != "" {
			msg = fmt.Sprintf("%s（%s）", msg, result.Failure.Message)
		}
		emitError(onEvent, msg)
		return nil
	}

	emitText(onEvent, fmt.Sprintf("最終検証 Phase 1 完了 (rounds=%d)\n", result.Rounds))

	if result.Rounds == 0 {
		// 初回応答 has_inquiry=false で完走 → Phase 2-5 を完全 skip (Req 1.3)。
		// per-task 完了後の「問題なし」即完走経路。
		emitDone(onEvent, "Kiro実装ループ完了: "+feature+"（最終検証で問題なし）")
		return nil
	}

	// ===== Phase 2-3: fix-edit + fix-judge =====
	emitText(onEvent, "最終検証 Phase 2-3 (fix-edit + fix-judge) 開始\n")

	fixResult := runFinalReviewFixPhase(
		ctx, agent, cwd, feature, runDir,
		order, finalReviewPhase,
		result.LastSession, implPersona.Name, finalReviewPromptLabel,
		onEvent,
	)
	if !fixResult.OK {
		// Phase 2 (fix-edit、Turn 1) と Phase 3 (fix-judge、Turn 2) のどちらで失敗したかは
		// `fixResult.Message` の "judge" substring で識別する (helper 仕様: Turn 2 系の失敗は
		// すべて Message 先頭に "...fix phase judge ..." を含む)。`JudgeSession==""` ベースの
		// 判定は Turn 2 `judgeErr != nil` のとき JudgeSession 未設定のままになる helper 内部仕様
		// により Turn 1 / Turn 2 を誤分類するため避ける。Message 文字列に "fix-edit" / "fix-judge"
		// を含めることで、後段テスト (シナリオ 4b 等) が substring 一致で失敗フェーズを観測できる。
		var prefix string
		if strings.Contains(fixResult.Message, "judge") {
			prefix = "中断: 最終検証 fix-judge で失敗"
		} else {
			prefix = "中断: 最終検証 fix-edit でエージェントエラー"
		}
		msg := prefix
		if strings.TrimSpace(fixResult.Message) != "" {
			msg = fmt.Sprintf("%s（%s）", prefix, fixResult.Message)
		}
		emitError(onEvent, msg)
		return nil
	}

	emitText(onEvent, fmt.Sprintf("最終検証 Phase 2-3 完了 (Files Modified: %d)\n", fixResult.ChangedFiles))

	if fixResult.ChangedFiles == 0 {
		// 修正対象なし → 全件 test と commit を skip (Req 2.5 / 3.6)。
		emitDone(onEvent, "Kiro実装ループ完了: "+feature+"（最終検証で修正なし）")
		return nil
	}

	// ===== Phase 4: test-edit + test-judge =====
	emitText(onEvent, "最終検証 Phase 4 (test-edit + test-judge) 開始\n")

	// Turn 1 の resume 起点は **Phase 2 fix-edit sessionId** (= fixResult.EditSession)。
	// fix-judge sessionId (= fixResult.SessionID) は fixJudgeAllowedTools で制限された
	// 状態の resume となり cross-restriction resume を踏むため使わない
	// (design.md "Resume source rationale" / 2.2 の Implementation Notes 参照)。
	testResult := runFinalReviewTestVerification(
		ctx, agent, cwd, feature, runDir,
		order, finalReviewPhase,
		fixResult.EditSession, implPersona.Name, finalReviewPromptLabel,
		onEvent,
	)
	if !testResult.OK {
		// Phase 4 失敗は 4 サブカテゴリに分類して Message にエンコードする。識別はすべて
		// `testResult.Message` の substring 一致で行う (helper 仕様: 各失敗パスは固有の文言を
		// Message 先頭に含む):
		//   1. Test Result: fail (schema 適合 + テスト不通解消不能、Req 3.5):
		//      Message="...Test Result: fail (test 不通解消不能)" → "test-failed"
		//   2. schema-violation (Test Result / Files Modified marker 未出力 / parse error / has_inquiry):
		//      Message="...judge schema violation: ..." or "...returned has_inquiry=true..." → "schema-violation"
		//   3. test-judge agent error / success=false (Turn 2 で error / success=false):
		//      Message="...judge agent error: ..." or "...judge turn returned success=false" → "test-judge"
		//   4. test-edit (Turn 1) agent error / success=false (Turn 2 未到達):
		//      Message に "judge" が現れない → "test-edit"
		var prefix string
		switch {
		case strings.Contains(testResult.Message, "Test Result: fail"):
			// Turn 2 で schema 適合かつ Test Result: fail を抽出 → テスト不通解消不能 (Req 3.5)。
			prefix = "中断: 最終検証 全件 test で test-failed"
		case strings.Contains(testResult.Message, "schema violation"),
			strings.Contains(testResult.Message, "has_inquiry"):
			// Turn 2 schema-violation (helper Message 仕様: "...judge schema violation: ..." または
			// has_inquiry=true 残留)。後者も構造化応答契約違反として schema-violation 系に倒す。
			prefix = "中断: 最終検証 test-judge で schema-violation"
		case strings.Contains(testResult.Message, "judge"):
			// Turn 2 (test-judge) の agent error / success=false 等。
			// 上記の "Test Result: fail" / "schema violation" / "has_inquiry" の先行 case で
			// 拾われなかった残りの Turn 2 失敗系をここで吸収する。
			prefix = "中断: 最終検証 test-judge で失敗"
		default:
			// Turn 1 (test-edit) で agent error / success=false。Message に "judge" が含まれないため
			// 上記すべての case を素通りしてここに到達する。
			prefix = "中断: 最終検証 test-edit でエージェントエラー"
		}
		msg := prefix
		if strings.TrimSpace(testResult.Message) != "" {
			msg = fmt.Sprintf("%s（%s）", prefix, testResult.Message)
		}
		emitError(onEvent, msg)
		return nil
	}

	emitText(onEvent, "最終検証 Phase 4 完了 (Test Result: pass)\n")

	// ===== Phase 5: commit =====
	emitText(onEvent, "最終検証 Phase 5 (commit) 開始\n")

	commitResp, commitErr := runFinalReviewCommit(ctx, agent, cwd, onEvent)
	if commitErr != nil || !commitResp.Success {
		// Phase 5 失敗は WorkflowError 発火 + run 停止 (Req 4.4)。Message に "commit" を含めて
		// シナリオ 4d の substring 一致で失敗フェーズを観測可能にする。
		msg := "中断: 最終検証 commit が失敗"
		switch {
		case commitErr != nil:
			msg = fmt.Sprintf("%s（%v）", msg, commitErr)
		case strings.TrimSpace(commitResp.Content) != "":
			msg = fmt.Sprintf("%s（%s）", msg, commitResp.Content)
		}
		emitError(onEvent, msg)
		return nil
	}

	// Phase 5 成功 → final-review 全体完走。
	emitDone(onEvent, "Kiro実装ループ完了: "+feature+"（最終検証 commit 完了）")
	return nil
}

// emitText / emitError / emitDone は onEvent==nil 防御を 1 箇所に集約する内部 helper。
// runFinalReview の各 phase 進行通知 / abort 通知で使う。

func emitText(onEvent func(workflow.WorkflowEvent), text string) {
	if onEvent == nil {
		return
	}
	onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: text})
}

func emitError(onEvent func(workflow.WorkflowEvent), message string) {
	if onEvent == nil {
		return
	}
	onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: message})
}

func emitDone(onEvent func(workflow.WorkflowEvent), result string) {
	if onEvent == nil {
		return
	}
	onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowDone, Result: result})
}

// fixResultForPersist は `finalReviewFixResult` を `persistFixResult` (spec_loop.go) が
// 受け取る `RunResultStepFix` 形式に詰め替える薄いアダプタ。
//
// `RunResultStepFix` は spec_loop の result.json 永続化を主目的とした既存型で、`ChangedFiles`
// を持たない。impl_loop 側 (`finalReviewFixResult`) は `ChangedFiles` を Phase 4 進行判定の
// ために保持するが、`fix-result.md` 自体には Summary 経由で `Files Modified: <count>` 行が
// 残るため、永続化フォーマット側に新フィールドを足す必要はない (spec_loop の既存挙動を
// 壊さないことを優先する設計判断)。
func fixResultForPersist(fix finalReviewFixResult) RunResultStepFix {
	return RunResultStepFix{
		Ran:          fix.Ran,
		OK:           fix.OK,
		Summary:      fix.Summary,
		SessionID:    fix.SessionID,
		EditSession:  fix.EditSession,
		JudgeSession: fix.JudgeSession,
		Message:      fix.Message,
	}
}
