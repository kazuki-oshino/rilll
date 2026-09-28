// Package agent の report_phase.go は output-contract-2phase spec で導入される
// Phase 2 (report 書き出し) 関連の純関数群と sentinel error を集約する。
//
// 本ファイルが集約する責務:
//   - validateContracts: CallAgent 入口で呼ばれる純関数 precheck (タスク 5.1 で導入)
//   - writeReportFile: path-escape 検証 + timestamp backup + 0644 書き出し (タスク 5.2 で導入)
//   - buildReportInstruction / phase2StepID / isRetryable: Phase 2 リクエスト組立と
//     retry whitelist 判定のための純関数群 (タスク 5.3 で導入)
//   - runReportPhase: Phase 1 完了後の Phase 2 逐次実行本体 (タスク 5.4 で導入)
//
// 本ファイルの関数はいずれも agent 層 (internal/agent) 内のみで呼び出される package-private
// helper であり、workflow 層からは CallAgent の戻り値経由でのみ結果が伝わる (workflow 層は
// OutputContract を「宣言するだけ」で済む / design Architecture 図参照)。
package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kazuki-oshino/rilll/internal/facet"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// ErrDuplicateContractName は CallAgentOptions.OutputContracts 内で OutputContract.Name が
// 重複して宣言されているときに validateContracts が返す sentinel error (Req 1.4)。
//
// 本 error は CallAgent 入口 precheck (validateContracts) が Phase 1 dispatch より前に
// 検出する構造的ガードであり、検出時には Phase 1 / Phase 2 のいずれも実行されない
// (provider.Call ゼロ呼び出し / akashic agent_calls 行ゼロ追加 / 課金ゼロ)。
//
// caller (CallAgent) は本 sentinel を errors.Is で識別したとき、
// CallError{Category: FailureCategoryInvalidOptions, Err: <返された err>} として
// 即 return する責務を持つ (関心の分離: 純関数検証と副作用呼び出しの分離)。
//
//	errors.Is(err, ErrDuplicateContractName) // → true
var ErrDuplicateContractName = errors.New("output contract: duplicate name")

// ErrEmptyFormatFacet は CallAgentOptions.OutputContracts 内の OutputContract.FormatFacet が
// 空文字列で宣言されているときに validateContracts が返す sentinel error (Req 1.5)。
//
// 本 error は CallAgent 入口 precheck (validateContracts) が Phase 1 dispatch より前に
// 検出する構造的ガードであり、検出時には Phase 1 / Phase 2 のいずれも実行されない
// (provider.Call ゼロ呼び出し / akashic agent_calls 行ゼロ追加 / 課金ゼロ)。
//
// caller (CallAgent) は本 sentinel を errors.Is で識別したとき、
// CallError{Category: FailureCategoryInvalidOptions, Err: <返された err>} として
// 即 return する責務を持つ。
//
//	errors.Is(err, ErrEmptyFormatFacet) // → true
var ErrEmptyFormatFacet = errors.New("output contract: empty format facet")

// validateContracts は CallAgent 入口で呼ばれる純関数 precheck であり、
// CallAgentOptions.OutputContracts の宣言が構造的に妥当であることを Phase 1 dispatch 前に
// 検証する (Req 1.4 / 1.5)。
//
// 検出する error 種別 (sentinel を wrap):
//   - OutputContract.FormatFacet が空文字列 → ErrEmptyFormatFacet (Req 1.5)
//   - OutputContract.Name が既出と一致 (重複) → ErrDuplicateContractName (Req 1.4)
//
// 不変式:
//   - I/O / グローバル状態に依存しない純関数。引数 slice のみを読む。
//   - 1 個目の問題を検出した時点で即 return する (fail-fast / 全件列挙は行わない)。
//   - 検査順は slice 宣言順 (前方インデックスから後方インデックス) であり、各要素については
//     FormatFacet 空文字検査 → Name 重複検査の順で評価する。
//     したがって、より前方インデックスの問題が後方インデックスの問題より優先される。
//   - contracts が nil / 空 slice のときは何も検査せず nil を返す。
//     本関数は CallAgent 入口の zero-value ガード (len(opts.OutputContracts) == 0 で
//     skip / R1.3) の内側で呼ばれる前提だが、防御的に zero-value 入力でも panic せず
//     nil を返す挙動を保証する。
//
// 計算量: 時間 O(N) / 空間 O(N) (N = len(contracts))。重複検出は map[string]struct{} を
// 1 回走査することで実現する。
//
// caller (CallAgent) は error 非 nil のとき以下を構造的に保証する責務を持つ:
//   - provider.Call を呼ばない (= Phase 1 SDK 呼び出しを発生させない / R1.4 R1.5)
//   - akashic agent_calls 行を積まない (= recordAgentCallStart も呼ばない)
//   - CallError{Category: FailureCategoryInvalidOptions, Err: <returned err>} を即 return する
//
// 上記により Name 重複 / FormatFacet 空文字は Phase 2 / retry 経路に到達せず、
// 後続タスクで実装される isRetryable の whitelist (path-escape / facet 不在) は
// 本 2 種の error を扱わない。
func validateContracts(contracts []workflowctx.OutputContract) error {
	seen := make(map[string]struct{}, len(contracts))
	for i, c := range contracts {
		if c.FormatFacet == "" {
			return fmt.Errorf("%w: index=%d name=%q", ErrEmptyFormatFacet, i, c.Name)
		}
		if _, dup := seen[c.Name]; dup {
			return fmt.Errorf("%w: index=%d name=%q", ErrDuplicateContractName, i, c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	return nil
}

// ErrPathEscape は writeReportFile が解決した target path が reportDir 配下から
// 外れていることを検出したときに返す sentinel error (Req 4.2 / 5.4)。
//
// 検出経路: writeReportFile が `filepath.Clean(filepath.Join(reportDir, name+".md"))` を
// 算出し、`strings.HasPrefix(cleanedTarget, cleanedReportDir+sep)` が false のとき本 sentinel を
// wrap した error を返す。代表的な発火条件:
//   - name に `../foo` 等の親ディレクトリ参照が含まれ、解決結果が reportDir の兄弟以上に
//     移動するケース
//   - name に絶対パス相当の表現 (例: `/etc/passwd`) を渡し、結果が reportDir の外を指すケース
//
// 本 sentinel は retry 不可カテゴリ (R5.4) に属し、isRetryable (タスク 5.3) の whitelist で
// false を返す対象である。検出時には backup / target いずれのファイルも作成されない構造的
// 不変式を保つ (後段の os.Stat / os.Rename / os.WriteFile を実行する前で return する)。
//
//	errors.Is(err, ErrPathEscape) // → true
var ErrPathEscape = errors.New("output contract: report path escape")

// writeReportFile は OutputContract.Name に対応する Phase 2 report ファイルを
// `<reportDir>/<name>.md` に安全に書き出す純粋関数 (副作用は filesystem のみ /
// Req 4.1 / 4.2 / 4.3 / 4.5)。
//
// 引数:
//   - reportDir: 書き出し先 directory (絶対パス / 相対パスのいずれでも可)。
//     `.rilll/<workflow>/<runID>/` を caller (runReportPhase) が組み立てて渡す前提。
//   - name: OutputContract.Name (kebab-case 推奨)。`.md` 拡張子を自動付与する。
//   - body: Phase 2 で受領した report 本文 (改変せずそのまま書き出す)。
//
// 戻り値:
//   - 成功時: 書き出した target の絶対パス (filepath.Abs 経由)。
//   - 失敗時: 空文字 + ErrPathEscape または os.* 起因 error。
//
// 処理フロー (design.md "writeReportFile 内部フロー" に対応):
//  1. `target = filepath.Clean(filepath.Join(reportDir, name+".md"))` を算出。
//  2. path-escape 検証: `cleanedTarget` が `cleanedReportDir + sep` で始まらなければ
//     ErrPathEscape を return (後続 I/O は一切行わない)。
//  3. target が既存ファイルなら `<base>.<RFC3339>.md` に rename して backup を作る。
//     既存 directory / symlink は filename 衝突として通常通り Rename を試み、失敗時は
//     上位に error 伝播する (= 後段の os.WriteFile も実行しない)。
//     - RFC3339 は秒精度なので、同一秒内の連続上書きでは backup path が衝突しうる。
//     その場合 os.Rename は既存 backup を上書きする挙動になり、古い backup が失われる。
//     この観測可能な失敗モードは「同一秒内の再書き出しは backup を 1 件しか残さない」
//     として受け入れ済み (本 spec の retry policy は ≥1 秒の sleep を内包する Phase 2 SDK
//     呼び出しを挟む / R5.1 retry 経路と同一秒で衝突する確率は実運用上無視できる)。
//  4. `os.WriteFile(target, []byte(body), 0o644)` で新 body を書き出す。
//  5. `filepath.Abs(target)` で絶対パスを取り戻し値として返す。
//
// 観測可能な失敗モード:
//   - backup 成立後 (Rename 完了) に WriteFile が失敗したとき、backup は巻き戻されない。
//     結果として reportDir 配下には `<base>.<timestamp>.md` だけが残り `<base>.md` は不在
//     となる。本ケースは retry なしで warning に集約する (R5.4) / 修復は手動で `<base>.md`
//     への rename back を行う。
//   - reportDir が存在しない場合は backup 段階に到達しないか、WriteFile が ENOENT を返す。
//     caller (runReportPhase) は呼び出し前に MkdirAll で reportDir を担保する責務を持つ。
//
// 計算量: 時間 O(1) (filesystem syscall 数定数) / 空間 O(len(body))。
//
// caller (runReportPhase / タスク 5.4) は以下を構造的に保証する責務を持つ:
//
//   - reportDir は MkdirAll 済みで実在する。
//
//   - name は OutputContract.Name のままで、明示的な sanitize は行わない (path-escape 検証で
//     不正値を弾く方針 / R4.2)。
//
//     abs, err := writeReportFile(reportDir, "plan", body)
//     // err == nil → abs に書き出した絶対パスが入る
//     // errors.Is(err, ErrPathEscape) → true なら caller は当該 contract を fail にし retry しない
func writeReportFile(reportDir, name, body string) (string, error) {
	cleanedReportDir := filepath.Clean(reportDir)
	target := filepath.Clean(filepath.Join(cleanedReportDir, name+".md"))

	// path-escape 検証: cleanedReportDir + sep を prefix に持つことを必須化する。
	// 末尾 sep を付けることで `/tmp/reportdir` と `/tmp/reportdir2` の hijack を防ぐ。
	sep := string(filepath.Separator)
	guard := cleanedReportDir + sep
	if !strings.HasPrefix(target, guard) {
		return "", fmt.Errorf("%w: reportDir=%q name=%q resolved=%q", ErrPathEscape, reportDir, name, target)
	}

	// 既存ファイルがあれば <base>.<RFC3339>.md に rename して backup する。
	// 不在 (ErrNotExist) は新規書き出しなので backup をスキップする。
	// それ以外の Stat error (permission など) は上位に伝播し、WriteFile を試みずに失敗させる。
	if _, err := os.Stat(target); err == nil {
		base := strings.TrimSuffix(target, ".md")
		ts := time.Now().UTC().Format(time.RFC3339)
		backupPath := base + "." + ts + ".md"
		if err := os.Rename(target, backupPath); err != nil {
			return "", fmt.Errorf("backup existing report failed (target=%q backup=%q): %w", target, backupPath, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat target failed (target=%q): %w", target, err)
	}

	if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("write report failed (target=%q): %w", target, err)
	}

	abs, err := filepath.Abs(target)
	if err != nil {
		// filepath.Abs はほぼ失敗しないが、failure 時は cleanedTarget をそのまま返して
		// 「書き出しは成功している」事実を caller に伝える (path 取り違えのほうが副作用が大きい)。
		return target, nil
	}
	return abs, nil
}

// buildReportInstruction は Phase 2 のエージェント指示文を組み立てる純関数
// (Req 3.4 / 8.4 / design.md "Phase 2 指示文フォーマット")。
//
// 戻り値は以下の golden 形式を 1 byte もズラさずに返す:
//
//	# Output Contract
//
//	<facetContent をそのまま挿入>
//
//	# Write To
//
//	<reportPath>
//
// 不変式:
//   - facetContent は escape / sanitize / trim を行わず、与えられたバイト列を素のまま
//     埋め込む (Req 8.4 — 雛形構造を改変しない)。`#` 見出し / `---` HR / 連続改行 /
//     行頭タブ / trailing whitespace が改変されないことを caller が前提にしてよい。
//   - reportPath も改変しない。caller (runReportPhase) は絶対 clean path を渡す
//     責務を持つ (design.md "Phase 2 指示文フォーマット" #595-597)。
//   - Phase 1 と同一 session を継続する前提のため、`# Previous Response` のような
//     前置ブロックはここで付加しない (Phase 1 の文脈は session resume が運ぶ /
//     design.md #595)。
//   - I/O / グローバル状態に依存しない純関数。
//
// 計算量: 時間 / 空間ともに O(len(facetContent) + len(reportPath))。
func buildReportInstruction(facetContent, reportPath string) string {
	// Builder で 1 回確保し、固定部分 + facet + reportPath を連結する。
	// 形式: `# Output Contract\n\n<facet>\n\n# Write To\n\n<path>\n`
	var b strings.Builder
	b.Grow(len(facetContent) + len(reportPath) + 40)
	b.WriteString("# Output Contract\n\n")
	b.WriteString(facetContent)
	b.WriteString("\n\n# Write To\n\n")
	b.WriteString(reportPath)
	b.WriteByte('\n')
	return b.String()
}

// phase2StepID は Phase 2 の akashic agent_call エントリに付与する step_id を組み立てる
// 純関数 (Req 7.2 / 7.3 / design.md "Phase 2 step_id 規約")。
//
// 規約:
//   - attempt == "" → Phase 2 main: `<phase1StepID>:phase2:<contractName>`
//   - attempt == "retry" → Phase 2 retry: `<phase1StepID>:phase2:<contractName>:retry`
//
// runReportPhase (タスク 5.4) は本関数で生成した step_id を `provider.AgentRequest.Metadata`
// に積み、recordAgentCallStart / recordAgentCallFinish 経由で akashic に記録する。Phase 1
// の step_id と prefix を共有することで「同一 workflow run に紐付く Phase 2 / retry」が
// 後から復元可能になる (R7.2 / R7.3)。
//
// 不変式:
//   - I/O / グローバル状態に依存しない純関数。
//   - attempt が空文字以外 (将来 retry 経路以外の attempt 種別が追加された場合) は
//     `<phase1StepID>:phase2:<contractName>:<attempt>` の形で suffix を付ける。本 spec
//     範囲では呼び出し元が "" / "retry" のみを渡すため "retry" のときの結果は
//     `<phase1StepID>:phase2:<contractName>:retry` と一致する。
//   - phase1StepID / contractName / attempt の各引数は escape / sanitize しない。
//     caller (runReportPhase) が precheck (validateContracts) を通過させた contract.Name
//     のみを渡す前提。
//
// 計算量: 時間 / 空間ともに O(len(phase1StepID) + len(contractName) + len(attempt))。
func phase2StepID(phase1StepID, contractName, attempt string) string {
	base := phase1StepID + ":phase2:" + contractName
	if attempt == "" {
		return base
	}
	return base + ":" + attempt
}

// isRetryable は runReportPhase の retry whitelist gate であり、与えられた error が
// Phase 2 で「新規 session 経由の retry を試す価値があるか」を判定する純関数
// (Req 5.4 / design.md "Retry 制御")。
//
// 規約:
//   - errors.Is(err, ErrPathEscape) → false (retry 不可 / R4.2)
//   - errors.Is(err, facet.ErrFacetNotFound) → false (retry 不可 / R3.5)
//   - err == nil → false (success に retry は不要 / 防御的)
//   - 上記以外 (provider / bridge / SDK 起因の transient error 等) → true (retry 可)
//
// 設計上の position:
//   - Name 重複 / FormatFacet 空文字は CallAgent 入口 precheck (validateContracts) で
//     Phase 1 前に弾かれるため、本関数の whitelist には含まれない (関心の分離 /
//     design.md #572)。
//   - whitelist (false を返す対象) は将来 Phase 2 で増える可能性があるが、その追加は
//     本関数の修正 + 対応 sentinel の export を伴うため、回帰防止の観点から都度
//     `report_phase_test.go` の TestIsRetryable に新ケースを追加する責務を caller に
//     残す。
//
// 不変式:
//   - I/O / グローバル状態に依存しない純関数。
//
// 計算量: 時間 O(深さ) (errors.Is が wrap chain を辿る深さ) / 空間 O(1)。
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrPathEscape) {
		return false
	}
	if errors.Is(err, facet.ErrFacetNotFound) {
		return false
	}
	return true
}

// phase2Mode は provider ID から OutputContractResult.Mode の静的マッピングを返す
// (Req 6.4 / design.md "Mode 判定")。
//
// マッピング規約:
//   - provider.ProviderClaude / provider.ProviderCodex → "native"
//     (AllowedTools / permissionMode が SDK レベルで強制される)
//   - 上記以外 (cursor / cursor-sdk / 未知 provider) → "fallback"
//     (Phase 2 制限は prompt-only best-effort)
//
// 本関数は実行時の provider 振る舞いを動的検証しない (= static mapping)。
// 「実際に AllowedTools が強制されたか」を runtime 観測しないのは steering tech.md
// の "信頼できるワークスペース前提" と整合する設計判断 (design.md #555)。
func phase2Mode(pid provider.ProviderID) string {
	switch pid {
	case provider.ProviderClaude, provider.ProviderCodex:
		return "native"
	default:
		return "fallback"
	}
}

// runReportPhase は Phase 1 成功後に Phase 2 を逐次実行する agent 内部 helper
// (Req 2.1, 2.2, 2.4, 2.5, 3.1, 3.2, 3.3, 3.5, 3.6, 4.1, 4.4, 5.1, 5.2, 5.3, 5.4,
//
//	6.1, 6.2, 6.3, 6.4, 7.1, 7.2, 7.3, 7.4 / design.md "runReportPhase")。
//
// 引数:
//   - ctx: 呼び出し元から伝搬する context (cancel / deadline は Phase 2 SDK 呼び出しに伝搬)
//   - cwd: workflow run の CWD (RunMetadata.CWD 経由)
//   - pid: 解決済み provider ID (CallAgent 側で ProviderOverride を反映済)
//   - phase1SessionID: Phase 1 response.SessionID。Phase 2 main の ResumeSessionID として渡す
//     (空文字のときは新規 session 同等に倒れる — provider 仕様による)
//   - phase1Metadata: Phase 1 で記録した AgentMetadata。本 helper では WorkflowID /
//     WorkflowRunID / WorkflowName / StepID を継承し、AgentCallID と StepID を Phase 2
//     ごとに上書きする
//   - contracts: precheck (validateContracts) を通過済の非空 slice
//
// 不変式 (caller 責務 + 本 helper postcondition):
//   - len(results) == len(contracts) を保証する (R2.5)
//   - results[i].Name == contracts[i].Name を保証する (R2.4 順序対応)
//   - 個別 contract の失敗は他 contract に伝播させない (R5.3)
//   - 副作用 (Phase 2 SDK 呼び出し / akashic 記録) は contracts ごとに完結する
//
// caller-violation guard:
//   - phase1Metadata.WorkflowID または WorkflowRunID が空のとき、reportDir を算出できない
//     ため、全 contract を Status="failed" + Warning に明示して return する。SDK 呼び出しも
//     akashic 記録も発生させない (caller 責務違反の defensive guard)。
//   - a.providers[pid] が未登録のときも同様に全 contract を failed + Warning にする。
//
// Mode マッピング:
//   - Mode は provider 静的マッピング (phase2Mode) で contract 横断的に固定される
//     (claude/codex=native, cursor 系=fallback)。Phase 2 の成否に依らず Mode は埋まる。
//
// Retry 制御 (R5.1 / R5.4):
//   - main attempt: ResumeSessionID = phase1SessionID で 1 回 Phase 2 SDK 呼び出し
//   - 失敗時 isRetryable(err) が true なら ResumeSessionID = "" で 1 回 retry (RetryUsed=true)
//   - isRetryable が false (ErrPathEscape / facet.ErrFacetNotFound) なら retry なしで failed
//   - retry を含めて 2 回失敗したら Status="failed" + Warning に集約する
//
// akashic 記録 (R7.1 / R7.2 / R7.3):
//   - Phase 2 main: step_id=<phase1StepID>:phase2:<contractName>
//   - Phase 2 retry: step_id=<phase1StepID>:phase2:<contractName>:retry
//   - 各 attempt について recordAgentCallStart / recordAgentCallFinish を 1 組ずつ呼ぶ
//   - AgentCallID は attempt ごとに newAgentCallID() で新規発行 (Phase 1 と物理的に別行)
//
// 計算量: 時間 O(N × Phase 2 SDK 所要時間) / 空間 O(N) (N = len(contracts))。
// 並列化はしない (R2.4 / session resume 衝突を避けるため逐次)。
func (a *Agent) runReportPhase(
	ctx context.Context,
	cwd string,
	pid provider.ProviderID,
	phase1SessionID string,
	phase1Metadata provider.AgentMetadata,
	contracts []workflowctx.OutputContract,
) []provider.OutputContractResult {
	// Mode は provider 静的マッピングで contract 横断的に固定 (R6.4)。
	mode := phase2Mode(pid)

	// results を初期化 (Name と Mode は早期に埋める / postcondition R2.4 / R6.4)。
	results := make([]provider.OutputContractResult, len(contracts))
	for i, c := range contracts {
		results[i] = provider.OutputContractResult{Name: c.Name, Mode: mode}
	}

	// caller-violation guard 1: RunMetadata 不在 (WorkflowID / WorkflowRunID 空) →
	// reportDir を算出できないので全 contract を failed に明示し SDK 呼び出しを発生させない。
	if phase1Metadata.WorkflowID == "" || phase1Metadata.WorkflowRunID == "" {
		warn := fmt.Sprintf("run metadata missing: workflowID=%q workflowRunID=%q (caller responsibility violation)", phase1Metadata.WorkflowID, phase1Metadata.WorkflowRunID)
		for i := range results {
			results[i].Status = "failed"
			results[i].Warning = warn
		}
		return results
	}

	// caller-violation guard 2: provider registry 未登録 →
	// SDK 呼び出しに到達できないので全 contract を failed に明示する。
	p, ok := a.providers[pid]
	if !ok {
		warn := fmt.Sprintf("provider %q not registered in agent providers map (caller responsibility violation)", pid)
		for i := range results {
			results[i].Status = "failed"
			results[i].Warning = warn
		}
		return results
	}

	// reportDir = <cwd>/.rilll/<workflowID>/<workflowRunID>。
	// MkdirAll で reportDir 配下を担保する (writeReportFile は MkdirAll を行わない契約)。
	reportDir := filepath.Join(cwd, ".rilll", phase1Metadata.WorkflowID, phase1Metadata.WorkflowRunID)
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		warn := fmt.Sprintf("create report directory %q failed: %v", reportDir, err)
		for i := range results {
			results[i].Status = "failed"
			results[i].Warning = warn
		}
		return results
	}

	// recordCtx は agent.CallAgent と同形: 上位 ctx が cancel されても akashic 記録は
	// 継続するため context.WithoutCancel で deadline / cancel を切り離す。
	recordCtx := context.WithoutCancel(ctx)

	// 各 contract を宣言順に逐次処理する (R2.4 並列なし)。
	for i, c := range contracts {
		a.processOneContract(ctx, recordCtx, p, pid, cwd, reportDir, phase1SessionID, phase1Metadata, c, &results[i])
	}

	return results
}

// processOneContract は 1 件の OutputContract に対して Phase 2 (main + 必要なら retry) を
// 実行し、result を in-place で埋める (Req 3.5 / 4.x / 5.1 / 5.4)。
//
// 処理フロー:
//  1. facet.ResolveFacet で Format facet を解決。ErrFacetNotFound 等は retry なしで failed。
//  2. report 絶対パスを算出し buildReportInstruction で Phase 2 指示文を組み立てる。
//  3. attemptPhase2 で Phase 2 main attempt (ResumeSessionID = phase1SessionID)。
//     成功時は writeReportFile が実行され result.Status="success" / ReportPath が埋まる。
//  4. main 失敗時、isRetryable(err) が true なら ResumeSessionID="" で retry。
//     result.RetryUsed=true を立てたうえで attemptPhase2 を再度呼ぶ。
//  5. retry も失敗、または非 retryable error の場合は result.Status="failed" / Warning。
//
// 観測可能な後置条件:
//   - 戻り時に result.Status は必ず "success" または "failed" のいずれかに確定する。
//   - result.Status="success" のとき ReportPath は絶対パス、Warning は空。
//   - result.Status="failed" のとき Warning は非空、ReportPath は空。
func (a *Agent) processOneContract(
	ctx context.Context,
	recordCtx context.Context,
	p provider.AgentProvider,
	pid provider.ProviderID,
	cwd string,
	reportDir string,
	phase1SessionID string,
	phase1Metadata provider.AgentMetadata,
	c workflowctx.OutputContract,
	result *provider.OutputContractResult,
) {
	// Step 1: Facet 解決 (retry whitelist 外)。
	facetResult, err := facet.ResolveFacet(cwd, facet.OutputContract, c.FormatFacet)
	if err != nil {
		result.Status = "failed"
		result.Warning = fmt.Sprintf("resolve facet %q failed: %v", c.FormatFacet, err)
		return
	}

	// Step 2: report 絶対パスを算出し Phase 2 指示文を組み立てる (R3.4 / R8.4)。
	targetPath := filepath.Join(reportDir, c.Name+".md")
	absReportPath, absErr := filepath.Abs(targetPath)
	if absErr != nil {
		// filepath.Abs はほぼ失敗しないが、failure 時は targetPath をそのまま埋め込む。
		absReportPath = targetPath
	}
	instruction := buildReportInstruction(facetResult.Content, absReportPath)

	// Step 3: Phase 2 main attempt (ResumeSessionID = phase1SessionID)。
	mainErr := a.attemptPhase2(ctx, recordCtx, p, pid, cwd, reportDir, phase1SessionID, phase1Metadata, c.Name, instruction, "", result)
	if mainErr == nil {
		return // success: result は attemptPhase2 内で確定済
	}
	if !isRetryable(mainErr) {
		// retry whitelist 外 (ErrPathEscape / facet.ErrFacetNotFound) → 即時 fail。
		result.Status = "failed"
		result.Warning = mainErr.Error()
		return
	}

	// Step 4: 新規 session で 1 回だけ retry (R5.1 / R7.3)。
	result.RetryUsed = true
	retryErr := a.attemptPhase2(ctx, recordCtx, p, pid, cwd, reportDir, "", phase1Metadata, c.Name, instruction, "retry", result)
	if retryErr == nil {
		return // retry success: result は attemptPhase2 内で確定済 (RetryUsed=true は既に立っている)
	}

	// Step 5: retry も失敗 → failed として集約 (R5.2)。
	result.Status = "failed"
	result.Warning = retryErr.Error()
}

// attemptPhase2 は Phase 2 1 attempt (main または retry) を実行する内部 helper。
// 副作用として akashic agent_calls 行を 1 件追記し、SDK 成功時に限り writeReportFile を呼ぶ。
//
// 戻り値:
//   - nil: Phase 2 SDK 成功 + writeReportFile 成功 (result.Status="success" / ReportPath が確定)
//   - 非 nil: SDK 失敗 (provider / context 起因の error) または writeReportFile 失敗
//     (ErrPathEscape / I/O error)。caller (processOneContract) が retry 判定に使う
//
// step_id 規約 (R7.2 / R7.3):
//   - attempt == "" → main: <phase1StepID>:phase2:<contractName>
//   - attempt == "retry" → retry: <phase1StepID>:phase2:<contractName>:retry
//
// AgentCallID は attempt ごとに newAgentCallID() で新規発行する。これにより Phase 1 /
// Phase 2 main / Phase 2 retry が物理的に別 agent_calls 行として記録される (R7.1)。
//
// AllowedTools / MaxTurns / PermissionMode 設定 (R3.2 / R3.3 / R3.6):
//   - AllowedTools = []string{"Write"}: Phase 2 のツール制限を SDK レベルで強制
//   - MaxTurns = 2: 指示 turn + Write turn で完結させる
//   - PermissionMode = "": bridge default ('acceptEdits' for Claude) を維持する
//     (steering tech.md の "Claude bridge は permissionMode: 'acceptEdits' 固定"
//     不変式に対する逸脱を行わないため / design.md #536)
//
// observation 経路: Phase 2 でも observation event は akashic raw_events に記録する
// (上位への onEvent forwarding はしない / Phase 2 は agent 内部完結のため)。
func (a *Agent) attemptPhase2(
	ctx context.Context,
	recordCtx context.Context,
	p provider.AgentProvider,
	pid provider.ProviderID,
	cwd string,
	reportDir string,
	resumeSessionID string,
	phase1Metadata provider.AgentMetadata,
	contractName string,
	instruction string,
	attempt string,
	result *provider.OutputContractResult,
) error {
	// Phase 2 用の metadata を組み立てる。Phase 1 から WorkflowID / RunID / Name を継承し、
	// StepID は phase2StepID で suffix を付け、AgentCallID は attempt ごとに新規発行する。
	metadata := phase1Metadata
	metadata.AgentCallID = newAgentCallID()
	metadata.StepID = phase2StepID(phase1Metadata.StepID, contractName, attempt)

	req := provider.AgentRequest{
		Prompt:          instruction,
		CWD:             cwd,
		ResumeSessionID: resumeSessionID,
		AllowedTools:    []string{"Write"},
		PermissionMode:  "", // bridge default ('acceptEdits' for Claude) を維持
		MaxTurns:        2,
		AkashicLevel:    a.akashicLevel,
		Metadata:        metadata,
	}

	startedAt := time.Now().UTC()
	// outputFormat は Phase 2 では指定しない (Phase 1 の json_schema は session resume で
	// 継続される構造化応答用であり、Phase 2 は Markdown report 書き出し主体のため)。
	a.recordAgentCallStart(recordCtx, pid, instruction, cwd, resumeSessionID, nil, metadata, startedAt)

	// Phase 2 内部の observation は raw_events に記録するが、上位の onEvent には forward しない。
	// Phase 2 は agent 層完結の helper であり、UI / workflow は Phase 1 イベントのみを観測する。
	wrappedOnEvent := func(event provider.AgentEvent) {
		if event.Type == provider.EventObservation {
			a.recordRawEvent(recordCtx, metadata, event)
		}
	}

	resp, callErr := p.Call(ctx, req, wrappedOnEvent)
	failureCategory := classifyFailure(ctx, resp, callErr)
	a.recordAgentCallFinish(recordCtx, metadata.AgentCallID, resp, callErr, failureCategory, time.Since(startedAt), time.Now().UTC())

	if callErr != nil {
		return callErr
	}

	// SDK 成功: response.Content を report ファイルとして書き出す (R4.1 / R4.5)。
	// writeReportFile が ErrPathEscape を返した場合、caller (processOneContract) が
	// isRetryable で false 判定するため retry なしで failed に倒れる (R5.4)。
	abs, writeErr := writeReportFile(reportDir, contractName, resp.Content)
	if writeErr != nil {
		return writeErr
	}

	result.Status = "success"
	result.ReportPath = abs
	return nil
}
