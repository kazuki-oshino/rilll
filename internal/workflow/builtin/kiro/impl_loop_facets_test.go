package kiro

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPreviousSummaryForTaskIndex は previousSummaryForTaskIndex 二段フォールバック
// helper の 6 ケース (Design §Testing Strategy Unit 3-6 + 補助 2 ケース) を表駆動で固定する。
//
// 設計契約 (kiro-impl-loop-facets task 2.2 / Design §Components previousSummaryForTaskIndex /
// §Decisions 2):
//
//	(a) i == 0                                                  → "" (前段なし、Req 1.2)
//	(b) i >= 1 かつ len(taskSummaries) >= i                     → taskSummaries[i-1]
//	                                                              (in-memory hit、Req 1.1 / 1.3)
//	(c) i >= 1 かつ taskSummaries 不足 (restart 復元時) で
//	    runDir/<i:02d>-impl-<prevTaskID>/result.json 存在 &
//	    Summary 非空                                             → on-disk Summary (Req 1.1 fallback)
//	(d-ENOENT) (c) 条件で result.json 不在                       → "" (graceful、Req 1.4)
//	(d-malformed) (c) 条件で JSON 不正                          → "" (graceful、Req 1.4)
//	(d-empty)   (c) 条件で Summary == ""                         → "" (graceful、Req 1.4)
//
// graceful degradation: ENOENT / 権限不足 / JSON 不正 / Summary 空 はすべて呼び出し側に
// error として伝播せず "" に倒れて Req 1.4 経路 (前置なしで後段起動継続) に合流する。
//
// 構造的不変性: in-memory hit ケース (b) では disk I/O が一切発生しないことを、
// disk fixture に *異なる* summary を配置して in-memory が勝つことを観測することで担保する
// (実装が誤って disk read を先行させると "disk-summary" が観測されて test が失敗する)。
//
// 要件: kiro-impl-loop-facets Req 1.1, 1.2, 1.3, 1.4。
func TestPreviousSummaryForTaskIndex(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(t *testing.T, runDir string) (i int, taskSummaries []string, pendingTasks []string)
		wantSummary string
	}{
		{
			name: "(a) i==0 returns empty (no previous task)",
			setup: func(t *testing.T, _ string) (int, []string, []string) {
				return 0, nil, []string{"1.1", "1.2"}
			},
			wantSummary: "",
		},
		{
			name: "(b) in-memory hit wins over disk fixture (proves disk I/O is bypassed)",
			setup: func(t *testing.T, runDir string) (int, []string, []string) {
				// Disk fixture has a *different* summary; if the implementation
				// erroneously reads disk before returning in-memory, the test would
				// observe "disk-summary-should-not-be-used" instead of "in-memory-summary"
				// and fail. これにより Design §Components Invariants 「in-memory hit
				// ケースでは disk I/O を一切行わない」を構造的に担保する。
				writeStepResultRaw(t, runDir, 1, "impl-1_1",
					`{"order":1,"phase":"impl-1_1","summary":"disk-summary-should-not-be-used","lastSession":""}`)
				return 1, []string{"in-memory-summary"}, []string{"1.1", "1.2"}
			},
			wantSummary: "in-memory-summary",
		},
		{
			name: "(c) on-disk fallback hit when taskSummaries empty (restart path)",
			setup: func(t *testing.T, runDir string) (int, []string, []string) {
				// previous task at index 0 is "1.1" → sanitized phase "impl-1_1", order = 1
				// (current task at index 1 has order 2, so previous order = i = 1).
				writeStepResultRaw(t, runDir, 1, "impl-1_1",
					`{"order":1,"phase":"impl-1_1","summary":"on-disk-fallback-summary","lastSession":"sess-1"}`)
				return 1, nil, []string{"1.1", "1.2"}
			},
			wantSummary: "on-disk-fallback-summary",
		},
		{
			name: "(d-ENOENT) on-disk fallback miss (result.json absent) returns empty (graceful)",
			setup: func(t *testing.T, _ string) (int, []string, []string) {
				// result.json was never written → ENOENT で graceful degradation
				return 1, nil, []string{"1.1", "1.2"}
			},
			wantSummary: "",
		},
		{
			name: "(d-malformed) on-disk fallback malformed JSON returns empty (graceful)",
			setup: func(t *testing.T, runDir string) (int, []string, []string) {
				writeStepResultRaw(t, runDir, 1, "impl-1_1", "{not valid json[")
				return 1, nil, []string{"1.1", "1.2"}
			},
			wantSummary: "",
		},
		{
			name: "(d-empty) on-disk fallback empty Summary returns empty (graceful)",
			setup: func(t *testing.T, runDir string) (int, []string, []string) {
				writeStepResultRaw(t, runDir, 1, "impl-1_1",
					`{"order":1,"phase":"impl-1_1","summary":"","lastSession":""}`)
				return 1, nil, []string{"1.1", "1.2"}
			},
			wantSummary: "",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			runDir := t.TempDir()
			i, taskSummaries, pendingTasks := tc.setup(t, runDir)
			got := previousSummaryForTaskIndex(i, taskSummaries, runDir, pendingTasks)
			if got != tc.wantSummary {
				t.Errorf("previousSummaryForTaskIndex(%d, %v, runDir, %v) = %q, want %q",
					i, taskSummaries, pendingTasks, got, tc.wantSummary)
			}
		})
	}
}

// TestPersistImplTaskResult は persistImplTaskResult I/O helper の挙動を
// 表駆動で固定する。
//
// 設計契約 (kiro-impl-loop-facets task 2.3 / Design §Components persistImplTaskResult):
//
//   - 書き出し先パスは stepDir(runDir, order, phase) + "/result.json"
//     (例: runDir/02-impl-1_1/result.json)
//   - 書き出し内容は RunResultStep の JSON シリアライズ (json.MarshalIndent)
//   - 必須フィールド: order / phase / summary / lastSession
//   - 非対応フィールド (rounds / commitOK / commitOutput) は zero value で出力される
//     (構造体タグに omitempty が無いため JSON 上は present、値は 0 / false / "")
//   - Fix フィールドは json タグ omitempty で常に省略される (本 spec で値が入らない)
//   - 親ディレクトリが未作成のケースでも os.MkdirAll で自動作成され I/O 失敗しない
//   - 失敗時 (write error 等) は error を返す (呼び出し側 task 3.2 で `_ =` ベストエフォート扱い)
//
// 要件: kiro-impl-loop-facets Req 1.4 (graceful degradation の source として on-disk
// 状態を提供)。
func TestPersistImplTaskResult(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		setup           func(t *testing.T) (runDir string, order int, phase, summary, lastSession string)
		wantOrder       int
		wantPhase       string
		wantSummary     string
		wantLastSession string
	}{
		{
			name: "writes result.json with RunResultStep schema (normal case)",
			setup: func(t *testing.T) (string, int, string, string, string) {
				return t.TempDir(), 2, "impl-1_1", "impl summary for task 1.1", "sess-abc"
			},
			wantOrder:       2,
			wantPhase:       "impl-1_1",
			wantSummary:     "impl summary for task 1.1",
			wantLastSession: "sess-abc",
		},
		{
			name: "creates parent step directory when it does not exist",
			setup: func(t *testing.T) (string, int, string, string, string) {
				// runDir 直下の <NN>-<phase> ディレクトリが事前に未作成な状態を構築する。
				// persistImplTaskResult が内部で os.MkdirAll を呼ぶことで I/O 失敗せず作成されること
				// を構造的に担保する (既存 persistStepResult と同方針)。
				runDir := t.TempDir()
				return runDir, 3, "impl-2_1", "second task summary", "sess-xyz"
			},
			wantOrder:       3,
			wantPhase:       "impl-2_1",
			wantSummary:     "second task summary",
			wantLastSession: "sess-xyz",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runDir, order, phase, summary, lastSession := tc.setup(t)

			if err := persistImplTaskResult(runDir, order, phase, summary, lastSession); err != nil {
				t.Fatalf("persistImplTaskResult(...) error = %v, want nil", err)
			}

			// 書き出し先パスは既存 stepDir 規約に従う (Design §Components 不変性)。
			resultPath := filepath.Join(stepDir(runDir, order, phase), "result.json")
			data, err := os.ReadFile(resultPath)
			if err != nil {
				t.Fatalf("read %s: %v", resultPath, err)
			}

			// RunResultStep に decode 可能であること (= JSON フォーマットが
			// 既存スキーマと互換) を構造的に担保する。
			var got RunResultStep
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("unmarshal result.json into RunResultStep: %v\nraw=%s", err, string(data))
			}

			if got.Order != tc.wantOrder {
				t.Errorf("Order = %d, want %d", got.Order, tc.wantOrder)
			}
			if got.Phase != tc.wantPhase {
				t.Errorf("Phase = %q, want %q", got.Phase, tc.wantPhase)
			}
			if got.Summary != tc.wantSummary {
				t.Errorf("Summary = %q, want %q", got.Summary, tc.wantSummary)
			}
			if got.LastSession != tc.wantLastSession {
				t.Errorf("LastSession = %q, want %q", got.LastSession, tc.wantLastSession)
			}

			// 本 spec で意味を持たない field は zero value で出力される (Design §Data Models)。
			if got.Rounds != 0 {
				t.Errorf("Rounds = %d, want 0 (impl_loop context has no rounds)", got.Rounds)
			}
			if got.CommitOK {
				t.Errorf("CommitOK = true, want false (commit 結果は別経路で記録)")
			}
			if got.CommitOutput != "" {
				t.Errorf("CommitOutput = %q, want \"\" (commit 結果は別経路で記録)", got.CommitOutput)
			}

			// Fix は json tag omitempty のため、nil のときは JSON に出力されない。
			// 生 JSON 上に "fix" キーが含まれないことを文字列レベルでも確認する。
			if got.Fix != nil {
				t.Errorf("Fix = %+v, want nil (impl_loop has no fix phase)", got.Fix)
			}
			if strings.Contains(string(data), "\"fix\"") {
				t.Errorf("result.json contains \"fix\" key, want omitempty omitted\nraw=%s", string(data))
			}
		})
	}
}
