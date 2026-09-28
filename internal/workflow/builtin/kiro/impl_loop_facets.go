package kiro

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// previousSummaryForTaskIndex は kiro-impl-loop per-task ループの「直前タスク
// (index i-1) summary」を二段フォールバックで解決する純関数。
// kiro-spec-loop-facets の previousSummaryForStep と同形のパターンを
// per-task index ベースに焼き直したもの (Design §Decisions 2)。
//
// 解決優先順位 (Design §Components previousSummaryForTaskIndex):
//
//	(1) i == 0                                            → "" (前段なし、Req 1.2)
//	(2) i >= 1 かつ len(taskSummaries) >= i                → taskSummaries[i-1]
//	                                                          (in-memory hit、Req 1.1 / 1.3)
//	(3) i >= 1 かつ taskSummaries 不足 (restart 復元時)    → runDir/<i:02d>-impl-<prevTaskID>/result.json
//	                                                          の Summary (on-disk fallback)
//	(4) (3) の I/O / decode 失敗 / Summary 空              → "" (graceful degradation、Req 1.4)
//
// graceful degradation: on-disk fallback の ENOENT / 権限不足 / JSON 不正 /
// Summary 空 はすべて呼び出し側に error として伝播せず "" に倒し、Req 1.4 経路
// (前置なしで後段起動継続) に合流する。
//
// previous task の order と phase 解決:
//   - current task の index が i のとき、current order = i+1 (1-indexed)
//   - 直前 task の index は i-1、order は i (= 現 order - 1)
//   - 直前 task の phase は "impl-" + sanitizePhaseSegment(pendingTasks[i-1])
//   - on-disk path は stepDir(runDir, i, prevPhase) + "/result.json"
//
// 不変性:
//   - in-memory hit ケース (2) では disk I/O を一切行わない (構造上、(2) が真なら
//     branch から早期 return するため fs アクセスが発生しない)
//   - taskSummaries は直前 1 段 (index i-1) のみ参照 (累積禁止、Req 1.3)
//   - I/O write なし / global state なし / provider 分岐なし (Req 6.2 / 6.3 構造的担保)
//
// 用途: runKiroImplLoop の per-task ループ内で agentCallerAdapter.previousSummary
// field の初期化に呼ばれる (task 3.2 で配線)。
//
// Pre-condition: pendingTasks は run 開始時の snapshot (mutation 禁止)、
// taskSummaries は append-only。i >= 1 のとき pendingTasks には少なくとも i+1 個
// (= 現 task index までの) 要素があることを呼び出し側が保証する。
//
// 要件: kiro-impl-loop-facets Req 1.1, 1.2, 1.3, 1.4。
func previousSummaryForTaskIndex(i int, taskSummaries []string, runDir string, pendingTasks []string) string {
	if i == 0 {
		return ""
	}
	if len(taskSummaries) >= i {
		return taskSummaries[i-1]
	}
	// restart 復元時: in-memory が空のため on-disk fallback に降りる。
	prevPhase := "impl-" + sanitizePhaseSegment(pendingTasks[i-1])
	resultPath := filepath.Join(stepDir(runDir, i, prevPhase), "result.json")
	data, err := os.ReadFile(resultPath)
	if err != nil {
		return ""
	}
	var payload RunResultStep
	if err := json.Unmarshal(data, &payload); err != nil {
		return ""
	}
	return payload.Summary
}

// persistImplTaskResult は kiro-impl-loop per-task ループの inquiry loop 完走
// 直後に呼ばれ、当該 task の summary / lastSession を
// runDir/<NN>-impl-<sanitizedTaskID>/result.json として書き出す I/O helper。
//
// 用途 (Design §Components persistImplTaskResult):
//   - 二段フォールバック (previousSummaryForTaskIndex (3) 経路) の on-disk source
//     として機能し、restart 復元時に in-memory taskSummaries が空でも前段 summary を
//     ディスクから再生できるようにする (Req 1.4 graceful degradation の source)
//
// 書き出し形式:
//   - JSON フォーマットは spec_loop.go で定義済の公開型 RunResultStep をそのまま流用
//   - 必須フィールド: order / phase / summary / lastSession (本 spec で値が入る)
//   - 非対応フィールド: rounds / commitOK / commitOutput / fix
//     (impl_loop の文脈で意味を持たない。zero value または omitempty で省略される)
//
// 失敗時の扱い:
//   - error を返すが、呼び出し側 (task 3.2 で配線する runKiroImplLoop) は
//     `_ = persistImplTaskResult(...)` の形でベストエフォート扱いする
//     (`persistInquiry` / `persistAnswer` と同方針、Design §Error Strategy)
//   - これにより per-task の commit step 進行を妨げない
//
// 不変性:
//   - 書き出し対象は runDir/<NN>-impl-<taskID>/result.json のみ (他経路に書かない)
//   - 既存 stepDir(runDir, order, phase) 規約に従い path traversal を構造的に防ぐ
//   - provider 分岐なし (Req 6.3 構造的担保)
//
// Pre-condition: phase は "impl-<sanitized-taskID>" 形式 (呼び出し側で
// sanitizePhaseSegment 適用済)。
//
// 要件: kiro-impl-loop-facets Req 1.4。
func persistImplTaskResult(runDir string, order int, phase string, summary string, lastSession string) error {
	dir := stepDir(runDir, order, phase)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("persistImplTaskResult: mkdir %s: %w", dir, err)
	}
	out := filepath.Join(dir, "result.json")
	payload := RunResultStep{
		Order:       order,
		Phase:       phase,
		Summary:     summary,
		LastSession: lastSession,
		// Rounds / CommitOK / CommitOutput は impl_loop の文脈で意味を持たないため
		// zero value で出力する (struct タグに omitempty が無いので JSON 上は present)。
		// Fix は omitempty タグのため nil なら省略される。
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("persistImplTaskResult: marshal: %w", err)
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return fmt.Errorf("persistImplTaskResult: write %s: %w", out, err)
	}
	return nil
}
