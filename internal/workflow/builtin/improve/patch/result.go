// Package improvepatch の本ファイルは result.json artifact の正規型ファミリ
// (ResultRecord / AttemptRecord / TestResultRecord / BaselineRefRecord /
// DiffSummaryRecord) と DiffSummaryRecord プレビュー生成 helper を提供する
// (tasks.md 9.1 / design.md §Data Contracts & Integration §7)。
//
// 設計契約:
//   - JSON タグは snake_case で固定。フィールド追加時は本 spec の改訂を伴う
//     (design.md §7 / Req 6.2, 6.3, 6.4)。
//   - TestResult / BaselineRef / DiffSummary は apply 未到達 verdict
//     (`precondition_failed` / `context_load_failed` 等) で nil となるため
//     `omitempty` で JSON 出力から省略する。
//   - DiffSummaryRecord プレビューは「先頭 50 行 / 4 KiB」の 2 軸 cap で truncate
//     (Q3 / Resolved Decisions §3 / TUI 表示用)。
//
// orchestration (task 11.3) との関係:
//   - 全 verdict 経路で必ず `RecordRunArtifact(ResultKind, "result.json")` で
//     登録される (Req 6.5 記録欠落禁止)。
//   - `IsIntent() == true` (`applied_passed` / `user_rejected`) のとき Fn は nil 返却、
//     それ以外で `ErrorPrefixForVerdict(v, summary)` を error に乗せる (Req 6.2)。
package patch

import (
	"strings"
)

// ResultRecord は result.json の最低限の型契約。
// JSON タグは snake_case 固定、フィールド追加は本 spec (design.md §7) の改訂を要する。
type ResultRecord struct {
	// WorkflowRunID は本 improve-workflow run の workflow_run_id。
	WorkflowRunID string `json:"workflow_run_id"`

	// TargetWorkflowID は改善対象の workflow ID (例: "git-commit")。
	TargetWorkflowID string `json:"target_workflow_id"`

	// TargetGoFile は改善対象 Go ファイルの絶対 clean path。
	TargetGoFile string `json:"target_go_file"`

	// RunIDs は hand-off で受領した起点 run ID 配列 (MVP は length=1)。
	RunIDs []string `json:"run_ids"`

	// Verdict は closed set 10 値の最終 verdict (verdict.go 参照)。
	Verdict Verdict `json:"verdict"`

	// Summary は単一行 (改行を含まない) のサマリ。
	// `IsIntent() == false` のときに `workflow_runs.error` の suffix として
	// `[verdict:<id>] <summary>` 形で利用される (Req 6.2)。
	Summary string `json:"summary"`

	// Attempts は全試行 (proposal_unparseable / out_of_scope_diff で discard
	// されたものを含む) を順序保持で残す (Req 3.6 / 6.4)。
	Attempts []AttemptRecord `json:"attempts"`

	// TestResult は GoTestRunner.Run の結果サマリ。apply に至らない verdict
	// (`precondition_failed` / `context_load_failed` / `proposal_unparseable` /
	// `out_of_scope_diff` / `regenerate_loop_aborted` / `git_apply_failed` の一部)
	// では nil で省略される (omitempty)。
	TestResult *TestResultRecord `json:"test_result,omitempty"`

	// BaselineRef は Snapshot 実行経路 (apply フェーズ突入経路) でのみ非 nil。
	// Revert 時の手動復旧 (`git checkout <head_commit> -- <file>`) に使用する
	// (Req 5.9 / Resolved Decisions §7)。
	BaselineRef *BaselineRefRecord `json:"baseline_ref,omitempty"`

	// DiffSummary は TUI 表示用の diff サマリ (Q3 / Resolved Decisions §3)。
	// 全文は proposal-N.diff artifact 経由で参照する。
	// apply に至らない verdict では nil で省略される。
	DiffSummary *DiffSummaryRecord `json:"diff_summary,omitempty"`

	// StartedAt は run 開始 timestamp (RFC3339Nano)。
	StartedAt string `json:"started_at"`

	// FinishedAt は run 終了 timestamp (RFC3339Nano)。
	FinishedAt string `json:"finished_at"`
}

// AttemptRecord は ResultRecord.Attempts[] の各要素 (Req 3.6 / 6.4)。
//
// 各試行で record 必須:
//   - Attempt: 1-origin の試行回数。
//   - Provider: AI 改善案を生成した provider ID
//     ("claude" / "cursor" / "codex" / "cursor-sdk" など)。
//   - PatchArtifact: その試行で記録された proposal-N.diff の artifact 名。
//   - Outcome: 試行の解釈結果。"ok" / "proposal_unparseable" / "out_of_scope_diff"。
type AttemptRecord struct {
	// Attempt は 1-origin の試行回数。regenerate ループの 1 巡で 1 増える。
	Attempt int `json:"attempt"`

	// Provider は本試行で改善案生成に使用した provider ID。
	// 値は orchestration (task 11.2) が AgentCaller 経路から確定する文字列。
	Provider string `json:"provider"`

	// PatchArtifact は本試行で `RecordRunArtifact(ProposalKind, ...)` に登録された
	// proposal-N.diff の artifact 名。
	PatchArtifact string `json:"patch_artifact"`

	// Outcome は試行の解釈結果。
	//   - "ok": ExtractAndGuard 成功 (= レビュー段階に進む候補)。
	//   - "proposal_unparseable": ErrUnparseable で discard。
	//   - "out_of_scope_diff": ErrOutOfScope で discard。
	Outcome string `json:"outcome"`
}

// TestResultRecord は GoTestRunner.Run の出力サマリ (Req 5.5〜5.7)。
//
// `Executed == false` のとき他フィールドは zero 値であり、
// orchestration は「test 未実行」(precondition_failed 等) の経路で
// このレコード自体を nil とする (`ResultRecord.TestResult = nil`)。
// `Executed == true` のときのみ ExitCode / Duration / 出力テールが意味を持つ。
type TestResultRecord struct {
	// Executed は go test サブプロセスを実行したか否か。
	Executed bool `json:"executed"`

	// ExitCode は子プロセスの終了コード。`Executed == false` のときは 0。
	ExitCode int `json:"exit_code"`

	// DurationMS は実行時間 (ミリ秒)。`Executed == false` のときは 0。
	DurationMS int64 `json:"duration_ms"`

	// StdoutTail は stdout の末尾 (最大 32 KiB、超過時は中央切り詰め)。
	// 切り詰めの実装は GoTestRunner 側責務 (gotest.go 参照)。
	StdoutTail string `json:"stdout_tail"`

	// StderrTail は stderr の末尾 (最大 32 KiB、同上)。
	StderrTail string `json:"stderr_tail"`

	// Truncated は stdout / stderr のいずれかで切り詰めが発生したか。
	Truncated bool `json:"truncated"`
}

// BaselineRefRecord は Revert 時の手動復旧用 baseline 識別情報
// (Req 5.9 / Resolved Decisions §7)。
//
// 手動復旧手順 (Revert 失敗時に WorkflowText でユーザに案内する):
//
//	git checkout <head_commit> -- <target_files[i]>
//
// を 1 行ずつ実行可能な形に揃えるため、HeadCommit と TargetFiles を独立フィールドで持つ。
type BaselineRefRecord struct {
	// HeadCommit は Snapshot 時点の `git rev-parse HEAD` の出力 (40 文字 hex SHA-1)。
	HeadCommit string `json:"head_commit"`

	// TargetFiles は Revert 対象の絶対 clean path 配列 (Patch.TargetFiles と一致)。
	TargetFiles []string `json:"target_files"`
}

// DiffSummaryRecord は TUI 表示用の diff サマリ (Q3 / Resolved Decisions §3)。
//
// 用途:
//   - `SelectParams.Prompt` に乗せるサマリとして PreviewHead / 加減算行数を表示する。
//   - 全文は `proposal-N.diff` artifact (FullDiffArtifact 参照) 経由で取得する。
//   - `maxStreamLines = 4000` の先頭切り詰めを回避し、Run 詳細 Artifact pane で
//     全文確認可能にする (design.md §7 末尾の補足)。
type DiffSummaryRecord struct {
	// TargetFile は変更対象ファイルの絶対 clean path (Patch.TargetFiles[0] と一致)。
	TargetFile string `json:"target_file"`

	// AddedLines は unified diff 内 `+` 行 (`+++` ヘッダを除く) の総数。
	AddedLines int `json:"added_lines"`

	// DeletedLines は unified diff 内 `-` 行 (`---` ヘッダを除く) の総数。
	DeletedLines int `json:"deleted_lines"`

	// PreviewHead は diff の先頭部分のプレビュー文字列。
	// 先頭 50 行 / 4 KiB の 2 軸 cap で truncate される (Resolved Decisions §3)。
	PreviewHead string `json:"preview_head"`

	// FullDiffArtifact は全文 diff が保存された artifact 名 (proposal-N.diff)。
	FullDiffArtifact string `json:"full_diff_artifact"`
}

// previewMaxLines は DiffSummaryRecord.PreviewHead の最大行数 (Resolved Decisions §3)。
const previewMaxLines = 50

// previewMaxBytes は DiffSummaryRecord.PreviewHead の最大バイト数 (4 KiB / Resolved Decisions §3)。
const previewMaxBytes = 4096

// BuildDiffSummary は unified diff バイト列から DiffSummaryRecord を構築する。
//
// 動作:
//   - AddedLines / DeletedLines を unified diff の `+` / `-` 行からカウント。
//     `+++` (file ヘッダ) と `---` (file ヘッダ) は除外する。
//     hunk マーカ (`@@`) と `diff --git` 行はカウント対象外。
//   - PreviewHead を「先頭 50 行 / 4 KiB」の 2 軸 cap で truncate。
//     どちらか先に到達した時点で切り詰める。
//   - diff が空 ([]byte{} or nil) のときは AddedLines/DeletedLines = 0 かつ
//     PreviewHead = "" の DiffSummaryRecord を返す (nil は返さない)。
//     `apply 未到達経路で nil にする` 判定は orchestration 側責務とし、
//     本 helper は SRP に従い純粋に diff からの集計だけを行う。
//
// Preconditions:
//   - targetFile は絶対 clean path を期待する (orchestration から Patch.TargetFiles[0] を渡す契約)。
//   - fullDiffArtifact は proposal-N.diff の artifact 名 (registerArtifact 後の戻り値)。
//
// Postconditions:
//   - 戻り値は常に非 nil。AddedLines / DeletedLines は >= 0。
//   - len(PreviewHead) <= previewMaxBytes (= 4096)。
//   - PreviewHead に含まれる改行数 <= previewMaxLines (= 50)。
func BuildDiffSummary(targetFile string, diffBytes []byte, fullDiffArtifact string) *DiffSummaryRecord {
	added, deleted := countAddedDeletedLines(diffBytes)
	preview := buildPreviewHead(diffBytes)

	return &DiffSummaryRecord{
		TargetFile:       targetFile,
		AddedLines:       added,
		DeletedLines:     deleted,
		PreviewHead:      preview,
		FullDiffArtifact: fullDiffArtifact,
	}
}

// countAddedDeletedLines は unified diff バイト列から `+` 行 / `-` 行をカウントする。
//
// 規則 (unified diff 標準):
//   - `+` で始まる行のうち、`+++` で始まる行 (file ヘッダ) は除外する。
//   - `-` で始まる行のうち、`---` で始まる行 (file ヘッダ) は除外する。
//   - hunk マーカ (`@@`) や `diff --git` 行はカウント対象外。
//   - context 行 (` ` 始まり) は加減算対象外。
func countAddedDeletedLines(diffBytes []byte) (int, int) {
	if len(diffBytes) == 0 {
		return 0, 0
	}
	var added, deleted int
	// strings.SplitSeq は元 string を 1 度に分割せず iterator で 1 行ずつ供給するため
	// 大きな diff 入力でも一時 slice を持たない (Go 1.25 modernize 推奨)。
	// 入力末尾改行で空 element を 1 つ生むが、その空文字列は switch のいずれにも
	// マッチしないため zero impact。
	for line := range strings.SplitSeq(string(diffBytes), "\n") {
		switch {
		case strings.HasPrefix(line, "+++"):
			// file ヘッダ。追加行ではない。
		case strings.HasPrefix(line, "---"):
			// file ヘッダ。削除行ではない。
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			deleted++
		}
	}
	return added, deleted
}

// buildPreviewHead は unified diff バイト列の先頭部分を「50 行 / 4 KiB」の
// 2 軸 cap で truncate して文字列化する (Resolved Decisions §3)。
//
// 動作:
//  1. diff を行単位で走査し、累積バイト数 + 改行 1 バイトが previewMaxBytes
//     を超える行は採用しない (途中切れの行を残さない)。
//  2. 採用行数が previewMaxLines に達した時点で打ち切る。
//  3. 採用した各行の末尾に "\n" を付けて連結する (元の改行有無に依存しない正規化)。
//
// 戻り値:
//   - 空入力 → 空文字列。
//   - それ以外 → 採用済み行 + 末尾改行。len(戻り値) <= previewMaxBytes。
func buildPreviewHead(diffBytes []byte) string {
	if len(diffBytes) == 0 {
		return ""
	}
	// strings.SplitSeq で 1 行ずつ iterator から取り出し、cap 到達時点で early break する
	// (countAddedDeletedLines と同じく Go 1.25 modernize 推奨)。
	// 末尾の改行を split で空 element に変換しないように TrimRight しない
	// (元 diff の最終行が "\n" 終端でないとき採用判定で誤って drop されないよう
	//  各行を独立に評価する)。
	var b strings.Builder
	count := 0
	for line := range strings.SplitSeq(string(diffBytes), "\n") {
		if count >= previewMaxLines {
			break
		}
		// この行を採用した場合の追加サイズ (行本体 + "\n")。
		// builder の現在 length と合わせて previewMaxBytes を超えるなら採用しない。
		additional := len(line) + 1 // +1 は改行 1 バイト
		if b.Len()+additional > previewMaxBytes {
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
		count++
	}
	return b.String()
}
