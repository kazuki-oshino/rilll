package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/facet"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// TestValidateContracts は CallAgent 入口 precheck である validateContracts (純関数) が
// OutputContract slice について Name 重複 / FormatFacet 空文字を fail-fast 検出し、
// errors.Is で sentinel error を識別できることを table-driven test で検証する
// (Req 1.4 / 1.5)。
//
// 4 ケース構成:
//   - 正常通過: Name 一意 + FormatFacet 非空 → nil を返す
//   - 重複検出: 同一 Name が 2 件 → ErrDuplicateContractName を wrap した error を返す
//   - FormatFacet 空文字検出: 1 件のみ → ErrEmptyFormatFacet を wrap した error を返す
//   - fail-fast: 先頭インデックスの問題 (空文字) が後続インデックスの問題 (重複) より優先される
func TestValidateContracts(t *testing.T) {
	cases := []struct {
		name         string
		contracts    []workflowctx.OutputContract
		wantSentinel error // nil なら error 非発生を期待
	}{
		{
			name: "正常通過: Name 一意 + FormatFacet 非空",
			contracts: []workflowctx.OutputContract{
				{Name: "plan", FormatFacet: "plan"},
				{Name: "review", FormatFacet: "review-gather"},
			},
			wantSentinel: nil,
		},
		{
			name: "重複検出: 同一 Name が 2 件並ぶと ErrDuplicateContractName",
			contracts: []workflowctx.OutputContract{
				{Name: "dup", FormatFacet: "plan"},
				{Name: "dup", FormatFacet: "review-gather"},
			},
			wantSentinel: ErrDuplicateContractName,
		},
		{
			name: "FormatFacet 空文字検出: 1 件で ErrEmptyFormatFacet",
			contracts: []workflowctx.OutputContract{
				{Name: "a", FormatFacet: ""},
			},
			wantSentinel: ErrEmptyFormatFacet,
		},
		{
			// fail-fast: index 0 の空文字を検出した時点で即 return し、
			// index 2 にある Name 重複は (検査されず) ErrEmptyFormatFacet が優先される。
			// この性質により、全件列挙ではなく 1 個目で止まることが確認できる。
			name: "fail-fast: 先頭の空文字が後続の重複より優先",
			contracts: []workflowctx.OutputContract{
				{Name: "x", FormatFacet: ""},     // index 0: 空文字 → これが返るべき sentinel
				{Name: "y", FormatFacet: "plan"}, // index 1: OK
				{Name: "y", FormatFacet: "plan"}, // index 2: 重複 (到達してはならない)
			},
			wantSentinel: ErrEmptyFormatFacet,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateContracts(tc.contracts)

			if tc.wantSentinel == nil {
				if err != nil {
					t.Fatalf("error が発生してはならない経路で err=%v", err)
				}
				return
			}

			if err == nil {
				t.Fatalf("sentinel %v を期待したが nil が返った", tc.wantSentinel)
			}
			if !errors.Is(err, tc.wantSentinel) {
				t.Fatalf("errors.Is(err, %v) が true である必要がある (got err=%v)", tc.wantSentinel, err)
			}
		})
	}
}

// TestWriteReportFile は writeReportFile が以下を満たすことを table-driven で検証する
// (Req 4.1 / 4.2 / 4.3 / 4.5):
//
//   - path-escape 検出: name に "../foo" 等の reportDir 配下から外れる相対表現を渡すと
//     ErrPathEscape を返し、target / backup いずれのファイルも作成しない
//   - 新規書き出し: 同名ファイルが存在しないとき、target を mode 0644 で生成し
//     戻り値として絶対パスを返す
//   - 既存上書き + backup 生成: 同名ファイルが存在するとき、
//     `<base>.<RFC3339>.md` の形で 1 件 rename したうえで新 body を書き出す
//   - 連続 2 回上書き: 1 つの target に対して 2 度の上書きを行うと、
//     `<base>.<ts1>.md` と `<base>.<ts2>.md` の 2 件の backup と最終 body の合計 3 ファイルが
//     reportDir 配下に並ぶ (RFC3339 = 秒精度なので 2 回目までに ≥1 秒 sleep して
//     timestamp 衝突を回避する)
//
// 観察可能な検査点:
//   - 戻り値 path は target の絶対パスと一致 (filepath.Abs 経由)
//   - mode 0644 が target / backup の双方に付与される
//   - 戻り値 err は path-escape のとき errors.Is(err, ErrPathEscape) を満たす
func TestWriteReportFile(t *testing.T) {
	const (
		newBody   = "new contract body\n"
		v2Body    = "second version body\n"
		v3Body    = "third version body\n"
		writeMode = 0o644
	)

	t.Run("path-escape: name=../foo は ErrPathEscape を返し副作用なし", func(t *testing.T) {
		reportDir := t.TempDir()

		// 事前に reportDir 内の entry を採取し、後で副作用ゼロを確認する。
		beforeEntries, err := os.ReadDir(reportDir)
		if err != nil {
			t.Fatalf("ReadDir 失敗: %v", err)
		}

		got, err := writeReportFile(reportDir, "../escape", newBody)

		if err == nil {
			t.Fatalf("ErrPathEscape を期待したが err=nil / got=%q", got)
		}
		if !errors.Is(err, ErrPathEscape) {
			t.Fatalf("errors.Is(err, ErrPathEscape) が true である必要がある (got err=%v)", err)
		}
		if got != "" {
			t.Fatalf("path-escape 時の戻り値 path は空文字である必要がある (got %q)", got)
		}

		// reportDir 内の状態が呼び出し前から変化していないことを確認 (副作用なし)。
		afterEntries, err := os.ReadDir(reportDir)
		if err != nil {
			t.Fatalf("ReadDir 失敗: %v", err)
		}
		if len(afterEntries) != len(beforeEntries) {
			t.Fatalf("path-escape 時に reportDir 配下の entry 数が変化した (before=%d after=%d)", len(beforeEntries), len(afterEntries))
		}

		// reportDir 外 (../escape.md) にもファイルが作られていないことを確認。
		parentDir := filepath.Dir(reportDir)
		if _, err := os.Stat(filepath.Join(parentDir, "escape.md")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("path-escape 時に reportDir 外にファイルが作成された: err=%v", err)
		}
	})

	t.Run("新規書き出し: target 不在なら 0644 で生成し絶対パスを返す", func(t *testing.T) {
		reportDir := t.TempDir()
		name := "plan"

		got, err := writeReportFile(reportDir, name, newBody)
		if err != nil {
			t.Fatalf("新規書き出し経路で err=%v", err)
		}

		expectedTarget := filepath.Join(reportDir, name+".md")
		if !filepath.IsAbs(got) {
			t.Fatalf("戻り値は絶対パスである必要がある (got %q)", got)
		}
		// macOS の TempDir は /private/var/... と /var/... 双方の表現を取りうるので、
		// EvalSymlinks 経由で正規化して比較する。
		gotEval, err := filepath.EvalSymlinks(got)
		if err != nil {
			t.Fatalf("EvalSymlinks(got) 失敗: %v", err)
		}
		expectedEval, err := filepath.EvalSymlinks(expectedTarget)
		if err != nil {
			t.Fatalf("EvalSymlinks(expectedTarget) 失敗: %v", err)
		}
		if gotEval != expectedEval {
			t.Fatalf("戻り値 path mismatch: got=%q want=%q", gotEval, expectedEval)
		}

		body, err := os.ReadFile(expectedTarget)
		if err != nil {
			t.Fatalf("target ReadFile 失敗: %v", err)
		}
		if string(body) != newBody {
			t.Fatalf("target body mismatch: got=%q want=%q", string(body), newBody)
		}

		info, err := os.Stat(expectedTarget)
		if err != nil {
			t.Fatalf("target Stat 失敗: %v", err)
		}
		if mode := info.Mode().Perm(); mode != writeMode {
			t.Fatalf("target permission mismatch: got=%#o want=%#o", mode, writeMode)
		}

		// backup は存在しないことを確認 (新規書き出しでは rename が走らない)。
		entries, err := os.ReadDir(reportDir)
		if err != nil {
			t.Fatalf("ReadDir 失敗: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("新規書き出し後の reportDir entry 数は 1 (got %d)", len(entries))
		}
	})

	t.Run("既存上書き + backup 生成: 同名ファイルがあるとき backup 1 件 + 新 body 書き出し", func(t *testing.T) {
		reportDir := t.TempDir()
		name := "plan"
		target := filepath.Join(reportDir, name+".md")

		// 事前に旧 body を target に置く。
		if err := os.WriteFile(target, []byte("old body\n"), writeMode); err != nil {
			t.Fatalf("事前書き出し失敗: %v", err)
		}

		got, err := writeReportFile(reportDir, name, newBody)
		if err != nil {
			t.Fatalf("既存上書き経路で err=%v", err)
		}
		if got == "" {
			t.Fatalf("戻り値 path が空文字")
		}

		// 上書き後の target body は新 body。
		body, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("target ReadFile 失敗: %v", err)
		}
		if string(body) != newBody {
			t.Fatalf("target body mismatch: got=%q want=%q", string(body), newBody)
		}

		// reportDir 配下に target + backup 1 件の計 2 ファイルが並ぶ。
		entries, err := os.ReadDir(reportDir)
		if err != nil {
			t.Fatalf("ReadDir 失敗: %v", err)
		}
		if len(entries) != 2 {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("既存上書き後の entry 数は 2 (got %d entries=%v)", len(entries), names)
		}

		// backup ファイル (target 以外) の中身が旧 body であることを確認。
		var backupPath string
		for _, e := range entries {
			if e.Name() == name+".md" {
				continue
			}
			backupPath = filepath.Join(reportDir, e.Name())
		}
		if backupPath == "" {
			t.Fatalf("backup ファイルが見つからない")
		}
		// backup 名は <base>.<RFC3339>.md 形式。
		if !strings.HasPrefix(filepath.Base(backupPath), name+".") || !strings.HasSuffix(backupPath, ".md") {
			t.Fatalf("backup ファイル名が <base>.<RFC3339>.md 形式でない: got=%q", filepath.Base(backupPath))
		}
		backupBody, err := os.ReadFile(backupPath)
		if err != nil {
			t.Fatalf("backup ReadFile 失敗: %v", err)
		}
		if string(backupBody) != "old body\n" {
			t.Fatalf("backup body mismatch: got=%q want=%q", string(backupBody), "old body\n")
		}

		// backup ファイルの permission は os.Rename で保たれるはずなので 0644 を期待する。
		backupInfo, err := os.Stat(backupPath)
		if err != nil {
			t.Fatalf("backup Stat 失敗: %v", err)
		}
		if mode := backupInfo.Mode().Perm(); mode != writeMode {
			t.Fatalf("backup permission mismatch: got=%#o want=%#o", mode, writeMode)
		}
	})

	t.Run("連続 2 回上書き: backup が 2 件並ぶ", func(t *testing.T) {
		reportDir := t.TempDir()
		name := "plan"
		target := filepath.Join(reportDir, name+".md")

		// 事前に v1 body を target に置く。
		if err := os.WriteFile(target, []byte("v1 body\n"), writeMode); err != nil {
			t.Fatalf("事前書き出し失敗: %v", err)
		}

		// 1 回目の上書き (v1 → backup1, target は v2)。
		if _, err := writeReportFile(reportDir, name, v2Body); err != nil {
			t.Fatalf("1 回目の上書きで err=%v", err)
		}

		// RFC3339 は秒精度なので 2 回目の rename が衝突しないように
		// 1 秒以上 sleep してから 2 回目の上書きを行う。
		time.Sleep(1100 * time.Millisecond)

		// 2 回目の上書き (v2 → backup2, target は v3)。
		if _, err := writeReportFile(reportDir, name, v3Body); err != nil {
			t.Fatalf("2 回目の上書きで err=%v", err)
		}

		entries, err := os.ReadDir(reportDir)
		if err != nil {
			t.Fatalf("ReadDir 失敗: %v", err)
		}
		// target (v3) + backup1 (v1) + backup2 (v2) = 3 ファイル。
		if len(entries) != 3 {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("連続 2 回上書き後の entry 数は 3 (got %d entries=%v)", len(entries), names)
		}

		// target body は v3。
		body, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("target ReadFile 失敗: %v", err)
		}
		if string(body) != v3Body {
			t.Fatalf("target body mismatch: got=%q want=%q", string(body), v3Body)
		}

		// backup 2 件を mtime 順に並べて、各 backup の body が宣言順 (v1, v2) と一致することを確認する。
		var backups []string
		for _, e := range entries {
			if e.Name() == name+".md" {
				continue
			}
			backups = append(backups, filepath.Join(reportDir, e.Name()))
		}
		if len(backups) != 2 {
			t.Fatalf("backup 件数は 2 (got %d)", len(backups))
		}
		// RFC3339 timestamp の lex 順 = 時系列順なので filepath base の文字列順で安定ソートする。
		sort.Strings(backups)
		backup1Body, err := os.ReadFile(backups[0])
		if err != nil {
			t.Fatalf("backup1 ReadFile 失敗: %v", err)
		}
		backup2Body, err := os.ReadFile(backups[1])
		if err != nil {
			t.Fatalf("backup2 ReadFile 失敗: %v", err)
		}
		// backups[0] は古い backup (v1), backups[1] は新しい backup (v2)。
		if string(backup1Body) != "v1 body\n" {
			t.Fatalf("backup1 body mismatch: got=%q want=%q", string(backup1Body), "v1 body\n")
		}
		if string(backup2Body) != v2Body {
			t.Fatalf("backup2 body mismatch: got=%q want=%q", string(backup2Body), v2Body)
		}
	})
}

// TestBuildReportInstruction は buildReportInstruction が Phase 2 指示文を
// `# Output Contract\n\n<facet content>\n\n# Write To\n\n<absolute path>\n` の
// golden 形式で組み立て、facet content を escape / sanitize せず素のまま埋め込むことを
// 検証する (Req 3.4 / 8.4 / design.md "Phase 2 指示文フォーマット")。
//
// 検査観点:
//   - 全体の前後 prefix / suffix が指定の見出しと末尾改行で一致すること
//   - facet content 内の `#` (見出し) / `---` (HR) / 連続改行 / 行頭タブ /
//     trailing whitespace が 1 byte も変化しないこと
//   - reportPath を末尾改行付きで埋め込み、上書きや変換を行わないこと
func TestBuildReportInstruction(t *testing.T) {
	// facet content は実雛形に出現する記法を一通り含む合成サンプル。
	// 改変されてはならない要素:
	//   - 見出し `#` / `##`
	//   - HR `---`
	//   - 連続改行 (空行)
	//   - 行頭タブ
	//   - trailing whitespace (行末空白)
	facetContent := "# Plan\n" +
		"\n" +
		"## Goals\n" +
		"\n" +
		"- item with trailing space   \n" +
		"\t- indented with tab\n" +
		"\n" +
		"---\n" +
		"\n" +
		"## Open Questions\n" +
		"\n" +
		"<!-- placeholder -->\n"
	reportPath := "/abs/path/.rilll/wf/run-1/plan.md"

	got := buildReportInstruction(facetContent, reportPath)

	// 仕様 (task 5.3 / design.md "Phase 2 指示文フォーマット"):
	//   `# Output Contract\n\n<facet content>\n\n# Write To\n\n<absolute path>\n`
	// facet content は素のまま埋め込み、その前後それぞれに blank line (\n\n) を挟む。
	// facetContent 自体が末尾 \n を含む場合、結果として `<facet>\n\n# Write To` の
	// 直前は \n が 3 連続 = blank line が 2 行ぶん並ぶ形になるが、これは「facet content を
	// 改変しない」要件 (R8.4) を優先した観察可能な副作用として受け入れる。
	want := "# Output Contract\n\n" +
		facetContent +
		"\n\n# Write To\n\n" +
		reportPath + "\n"

	if got != want {
		t.Fatalf("buildReportInstruction golden mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	// facet content が完全一致部分文字列として出現することを冗長に確認する
	// (golden assertion が壊れたとき、原因が prefix/suffix 側か facet 改変側かを
	// 切り分けるためのセーフネット)。
	if !strings.Contains(got, facetContent) {
		t.Fatalf("facet content が改変されている (substring 不一致)")
	}

	// `# Output Contract\n\n` で始まり `# Write To\n\n<path>\n` で終わることを assert。
	if !strings.HasPrefix(got, "# Output Contract\n\n") {
		t.Fatalf("先頭 prefix が `# Output Contract\\n\\n` で始まっていない: %q", got[:min(len(got), 32)])
	}
	if !strings.HasSuffix(got, "\n\n# Write To\n\n"+reportPath+"\n") {
		t.Fatalf("末尾 suffix が `\\n\\n# Write To\\n\\n<path>\\n` で終わっていない: ...%q", got[max(0, len(got)-64):])
	}
}

// TestPhase2StepID は phase2StepID が以下を満たすことを検証する (Req 7.2 / 7.3 /
// design.md "Phase 2 step_id 規約"):
//
//   - attempt == "" のとき `<phase1StepID>:phase2:<contractName>` を返す (Phase 2 main)
//   - attempt == "retry" のとき `<phase1StepID>:phase2:<contractName>:retry` を返す
//     (Phase 2 retry)
func TestPhase2StepID(t *testing.T) {
	cases := []struct {
		name         string
		phase1StepID string
		contractName string
		attempt      string
		want         string
	}{
		{
			name:         "main (attempt 空文字)",
			phase1StepID: "review-workflow.run-one-rule",
			contractName: "no-todo-comments",
			attempt:      "",
			want:         "review-workflow.run-one-rule:phase2:no-todo-comments",
		},
		{
			name:         "retry",
			phase1StepID: "review-workflow.run-one-rule",
			contractName: "no-todo-comments",
			attempt:      "retry",
			want:         "review-workflow.run-one-rule:phase2:no-todo-comments:retry",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := phase2StepID(tc.phase1StepID, tc.contractName, tc.attempt)
			if got != tc.want {
				t.Fatalf("phase2StepID mismatch\n got=%q\nwant=%q", got, tc.want)
			}
		})
	}
}

// TestIsRetryable は isRetryable が Phase 2 retry whitelist を正しく解釈することを
// 検証する (Req 5.4 / design.md "Retry 制御"):
//
//   - errors.Is(err, ErrPathEscape) → false (retry なし)
//   - errors.Is(err, facet.ErrFacetNotFound) → false (retry なし)
//   - 一般 transient error (上記 2 sentinel 以外) → true (retry あり)
//   - sentinel を fmt.Errorf("...: %w", sentinel) で wrap した形でも errors.Is で
//     識別できることを確認する
func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "path-escape sentinel そのもの → retry 不可",
			err:  ErrPathEscape,
			want: false,
		},
		{
			name: "path-escape を wrap した error → retry 不可",
			err:  fmt.Errorf("write report failed: %w", ErrPathEscape),
			want: false,
		},
		{
			name: "facet 不在 sentinel そのもの → retry 不可",
			err:  facet.ErrFacetNotFound,
			want: false,
		},
		{
			name: "facet 不在を wrap した error → retry 不可",
			err:  fmt.Errorf("resolve facet failed: %w", facet.ErrFacetNotFound),
			want: false,
		},
		{
			name: "一般 transient error (io.ErrUnexpectedEOF) → retry 可",
			err:  io.ErrUnexpectedEOF,
			want: true,
		},
		{
			name: "errors.New 由来の任意 error → retry 可",
			err:  errors.New("provider transient failure"),
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isRetryable(tc.err)
			if got != tc.want {
				t.Fatalf("isRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// min / max は Go 1.21+ で組み込みになっているが、テスト局所での読みやすさのため
// shadow しないようパッケージ層へは追加せず、ここでは組み込み関数を使うのみ。

// scriptedProvider は runReportPhase の Phase 2 SDK 呼び出しを観測するための fake provider。
// 各 Call() ごとに事前スクリプト (scripts[]) の i 番目を返し、req は calls[] に蓄積する。
// scripts の本数を超える呼び出しは即時 error を返す (= 過剰呼び出しを構造的に検知)。
type scriptedProvider struct {
	id      provider.ProviderID
	calls   []provider.AgentRequest
	scripts []scriptedResponse
}

type scriptedResponse struct {
	resp provider.AgentResponse
	err  error
}

func (p *scriptedProvider) ID() provider.ProviderID { return p.id }

func (p *scriptedProvider) Call(_ context.Context, req provider.AgentRequest, _ provider.AgentEventCallback) (provider.AgentResponse, error) {
	idx := len(p.calls)
	p.calls = append(p.calls, req)
	if idx >= len(p.scripts) {
		return provider.AgentResponse{}, fmt.Errorf("scriptedProvider: unexpected call index=%d (scripts=%d)", idx, len(p.scripts))
	}
	s := p.scripts[idx]
	return s.resp, s.err
}

// newPhase1Metadata は test 用の Phase 1 metadata を組み立てる。
// WorkflowID / WorkflowRunID は reportDir 算出に使われ、StepID は phase2StepID 接頭辞になる。
func newPhase1Metadata(workflowID, runID, stepID string) provider.AgentMetadata {
	return provider.AgentMetadata{
		WorkflowID:    workflowID,
		WorkflowRunID: runID,
		WorkflowName:  "review-workflow",
		StepID:        stepID,
		StepName:      "rule-step",
		AgentCallID:   "phase1-call-id",
	}
}

// TestRunReportPhase は runReportPhase が Phase 1 完了後の Phase 2 を逐次実行し、
// 各 contract について OutputContractResult を集約する責務を満たすことを検証する
// (Req 2.1, 2.2, 2.4, 2.5, 3.1, 3.2, 3.3, 3.5, 3.6, 4.1, 4.4, 5.1, 5.2, 5.3, 5.4,
// 6.1, 6.2, 6.3, 6.4, 7.1, 7.2, 7.3, 7.4)。
//
// 7 シナリオ table-driven test:
//  1. happy_1_contract             — 1 contract 成功
//  2. happy_N_contracts            — N contracts 成功 / 順序対応 / Mode=native
//  3. facet_not_found_no_retry     — ResolveFacet 失敗 / retry なし / 他 contract 継続
//  4. path_escape_no_retry         — writeReportFile が ErrPathEscape / retry なし
//  5. transient_then_retry_success — Phase 2 transient 失敗 → 新規 session retry success
//  6. transient_then_retry_failure — Phase 2 transient 失敗 → retry 失敗 / Warning に集約
//  7. missing_run_metadata_guard   — phase1Metadata の WorkflowID/RunID 空 → 全 contract failed
//
// 各 sub-test で recordingRecorder に蓄積された agent_calls (Phase 2 main / retry) の
// step_id 規約 (<orig>:phase2:<name> / <orig>:phase2:<name>:retry) を確認し、Phase 1 と
// 別 agent_call として記録されていることを assert する (R7.1 / R7.2 / R7.3)。
func TestRunReportPhase(t *testing.T) {
	t.Run("happy_1_contract", func(t *testing.T) {
		cwd := t.TempDir()
		workflowID := "review-workflow"
		runID := "run-happy-1"
		phase1StepID := "review-step"
		phase1SessionID := "phase1-session-abc"

		recorder := &recordingRecorder{}
		sp := &scriptedProvider{
			id: provider.ProviderClaude,
			scripts: []scriptedResponse{
				{resp: provider.AgentResponse{Content: "phase 2 body for plan\n", SessionID: "phase2-session-1", Success: true}, err: nil},
			},
		}
		a := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
			provider.ProviderClaude: sp,
		}, recorder)

		contracts := []workflowctx.OutputContract{
			{Name: "plan", FormatFacet: "plan"},
		}
		phase1Meta := newPhase1Metadata(workflowID, runID, phase1StepID)

		results := a.runReportPhase(context.Background(), cwd, provider.ProviderClaude, phase1SessionID, phase1Meta, contracts)

		if len(results) != 1 {
			t.Fatalf("results 件数は 1 (got %d)", len(results))
		}
		r := results[0]
		if r.Name != "plan" {
			t.Fatalf("results[0].Name = %q, want %q", r.Name, "plan")
		}
		if r.Status != "success" {
			t.Fatalf("results[0].Status = %q, want %q (warning=%q)", r.Status, "success", r.Warning)
		}
		if r.Mode != "native" {
			t.Fatalf("results[0].Mode = %q, want %q (claude=native)", r.Mode, "native")
		}
		if r.ReportPath == "" || !filepath.IsAbs(r.ReportPath) {
			t.Fatalf("results[0].ReportPath は絶対パスである必要がある (got %q)", r.ReportPath)
		}
		if r.RetryUsed {
			t.Fatalf("results[0].RetryUsed = true (期待 false)")
		}
		if r.Warning != "" {
			t.Fatalf("results[0].Warning = %q (期待 \"\")", r.Warning)
		}

		// 書き出されたファイル本文を確認。
		body, err := os.ReadFile(r.ReportPath)
		if err != nil {
			t.Fatalf("report ファイル ReadFile 失敗: %v", err)
		}
		if string(body) != "phase 2 body for plan\n" {
			t.Fatalf("report body mismatch: got=%q want=%q", string(body), "phase 2 body for plan\n")
		}

		// Phase 2 SDK 呼び出しは 1 回。
		if len(sp.calls) != 1 {
			t.Fatalf("Phase 2 SDK 呼び出し回数 = %d (期待 1)", len(sp.calls))
		}
		req := sp.calls[0]
		if req.ResumeSessionID != phase1SessionID {
			t.Fatalf("Phase 2 ResumeSessionID = %q, want %q", req.ResumeSessionID, phase1SessionID)
		}
		if len(req.AllowedTools) != 1 || req.AllowedTools[0] != "Write" {
			t.Fatalf("Phase 2 AllowedTools = %#v, want []string{\"Write\"}", req.AllowedTools)
		}
		if req.MaxTurns != 2 {
			t.Fatalf("Phase 2 MaxTurns = %d, want 2", req.MaxTurns)
		}
		if req.PermissionMode != "" {
			t.Fatalf("Phase 2 PermissionMode = %q, want \"\" (bridge default acceptEdits 維持)", req.PermissionMode)
		}
		wantStepID := phase1StepID + ":phase2:plan"
		if req.Metadata.StepID != wantStepID {
			t.Fatalf("Phase 2 Metadata.StepID = %q, want %q", req.Metadata.StepID, wantStepID)
		}
		if req.Metadata.AgentCallID == "" || req.Metadata.AgentCallID == phase1Meta.AgentCallID {
			t.Fatalf("Phase 2 AgentCallID は Phase 1 と別 ID である必要がある (got %q)", req.Metadata.AgentCallID)
		}

		// akashic: Phase 2 main の start/finish 各 1 件 (Phase 1 は本 helper 範囲外なので 0 件)。
		if len(recorder.starts) != 1 {
			t.Fatalf("akashic starts = %d (期待 1)", len(recorder.starts))
		}
		if recorder.starts[0].StepID != wantStepID {
			t.Fatalf("akashic start StepID = %q, want %q", recorder.starts[0].StepID, wantStepID)
		}
		if recorder.starts[0].ResumeSessionID != phase1SessionID {
			t.Fatalf("akashic start ResumeSessionID = %q, want %q", recorder.starts[0].ResumeSessionID, phase1SessionID)
		}
		if len(recorder.finishes) != 1 {
			t.Fatalf("akashic finishes = %d (期待 1)", len(recorder.finishes))
		}
		if !recorder.finishes[0].Success {
			t.Fatalf("akashic finish Success = false (期待 true)")
		}
	})

	t.Run("happy_N_contracts_order_and_mode", func(t *testing.T) {
		cwd := t.TempDir()
		workflowID := "review-workflow"
		runID := "run-happy-n"
		phase1StepID := "review-step"
		phase1SessionID := "sess-n"

		recorder := &recordingRecorder{}
		sp := &scriptedProvider{
			id: provider.ProviderClaude,
			scripts: []scriptedResponse{
				{resp: provider.AgentResponse{Content: "plan body\n", SessionID: "p2-1", Success: true}},
				{resp: provider.AgentResponse{Content: "review body\n", SessionID: "p2-2", Success: true}},
				{resp: provider.AgentResponse{Content: "research body\n", SessionID: "p2-3", Success: true}},
			},
		}
		a := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
			provider.ProviderClaude: sp,
		}, recorder)

		contracts := []workflowctx.OutputContract{
			{Name: "plan-doc", FormatFacet: "plan"},
			{Name: "review-doc", FormatFacet: "review-gather"},
			{Name: "research-doc", FormatFacet: "research-report"},
		}
		phase1Meta := newPhase1Metadata(workflowID, runID, phase1StepID)

		results := a.runReportPhase(context.Background(), cwd, provider.ProviderClaude, phase1SessionID, phase1Meta, contracts)

		if len(results) != 3 {
			t.Fatalf("results 件数は 3 (got %d)", len(results))
		}

		// 順序対応 (R2.4): results[i].Name == contracts[i].Name
		wantNames := []string{"plan-doc", "review-doc", "research-doc"}
		wantBodies := []string{"plan body\n", "review body\n", "research body\n"}
		for i, r := range results {
			if r.Name != wantNames[i] {
				t.Fatalf("results[%d].Name = %q, want %q", i, r.Name, wantNames[i])
			}
			if r.Status != "success" {
				t.Fatalf("results[%d].Status = %q, want success (warning=%q)", i, r.Status, r.Warning)
			}
			if r.Mode != "native" {
				t.Fatalf("results[%d].Mode = %q, want native (claude=native)", i, r.Mode)
			}
			if r.ReportPath == "" {
				t.Fatalf("results[%d].ReportPath が空", i)
			}
			body, err := os.ReadFile(r.ReportPath)
			if err != nil {
				t.Fatalf("results[%d] report ReadFile 失敗: %v", i, err)
			}
			if string(body) != wantBodies[i] {
				t.Fatalf("results[%d] body mismatch: got=%q want=%q", i, string(body), wantBodies[i])
			}
		}

		if len(sp.calls) != 3 {
			t.Fatalf("Phase 2 SDK 呼び出し回数 = %d (期待 3)", len(sp.calls))
		}
		// step_id 規約: <orig>:phase2:<name>
		wantStepIDs := []string{
			phase1StepID + ":phase2:plan-doc",
			phase1StepID + ":phase2:review-doc",
			phase1StepID + ":phase2:research-doc",
		}
		for i, call := range sp.calls {
			if call.Metadata.StepID != wantStepIDs[i] {
				t.Fatalf("Phase 2 SDK calls[%d].StepID = %q, want %q", i, call.Metadata.StepID, wantStepIDs[i])
			}
		}
		if len(recorder.starts) != 3 || len(recorder.finishes) != 3 {
			t.Fatalf("akashic 行数 mismatch: starts=%d finishes=%d (期待 3/3)", len(recorder.starts), len(recorder.finishes))
		}
	})

	t.Run("cursor_provider_mode_fallback", func(t *testing.T) {
		// claude=native と cursor=fallback の static mapping を直接検証する。
		// happy_N_contracts と組み合わせるのではなく単独 sub-test として分離することで、
		// cursor provider 分岐の責任を明示する (R6.4)。
		cwd := t.TempDir()
		recorder := &recordingRecorder{}
		sp := &scriptedProvider{
			id: provider.ProviderCursor,
			scripts: []scriptedResponse{
				{resp: provider.AgentResponse{Content: "plan body\n", SessionID: "p2-c", Success: true}},
			},
		}
		a := New(provider.ProviderCursor, map[provider.ProviderID]provider.AgentProvider{
			provider.ProviderCursor: sp,
		}, recorder)

		contracts := []workflowctx.OutputContract{{Name: "plan", FormatFacet: "plan"}}
		phase1Meta := newPhase1Metadata("review-workflow", "run-cursor", "step-cursor")

		results := a.runReportPhase(context.Background(), cwd, provider.ProviderCursor, "sess-cursor", phase1Meta, contracts)

		if len(results) != 1 || results[0].Mode != "fallback" {
			t.Fatalf("cursor provider の Mode は fallback である必要がある (got results=%#v)", results)
		}
		if results[0].Status != "success" {
			t.Fatalf("cursor provider happy path で Status = %q (期待 success / warning=%q)", results[0].Status, results[0].Warning)
		}
	})

	t.Run("facet_not_found_no_retry_isolates_failures", func(t *testing.T) {
		// 1 つ目の contract で facet 不在 → 当該 contract のみ failed (retry なし) /
		// 2 つ目の contract は通常通り success を返す (R5.3 per-contract isolation)。
		cwd := t.TempDir()
		recorder := &recordingRecorder{}
		sp := &scriptedProvider{
			id: provider.ProviderClaude,
			scripts: []scriptedResponse{
				// 1 つ目の contract は ResolveFacet 段で fail するので SDK 呼び出しに到達しない。
				// 2 つ目の contract のみ SDK call が 1 回走る。
				{resp: provider.AgentResponse{Content: "ok body\n", SessionID: "p2-ok", Success: true}},
			},
		}
		a := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
			provider.ProviderClaude: sp,
		}, recorder)

		contracts := []workflowctx.OutputContract{
			{Name: "missing-doc", FormatFacet: "this-facet-does-not-exist-xyz"},
			{Name: "ok-doc", FormatFacet: "plan"},
		}
		phase1Meta := newPhase1Metadata("review-workflow", "run-facet", "review-step")

		results := a.runReportPhase(context.Background(), cwd, provider.ProviderClaude, "sess", phase1Meta, contracts)

		if len(results) != 2 {
			t.Fatalf("results 件数は 2 (got %d)", len(results))
		}
		// 1 つ目: facet 不在で failed
		if results[0].Name != "missing-doc" || results[0].Status != "failed" {
			t.Fatalf("results[0]: Name=%q Status=%q (期待 missing-doc / failed)", results[0].Name, results[0].Status)
		}
		if results[0].Warning == "" {
			t.Fatalf("results[0].Warning が空 (facet 不在の理由が必要)")
		}
		if results[0].ReportPath != "" {
			t.Fatalf("results[0].ReportPath = %q (failed では空文字)", results[0].ReportPath)
		}
		if results[0].RetryUsed {
			t.Fatalf("results[0].RetryUsed = true (期待 false / facet 不在は retry 不可)")
		}
		// 2 つ目: success
		if results[1].Name != "ok-doc" || results[1].Status != "success" {
			t.Fatalf("results[1]: Name=%q Status=%q (期待 ok-doc / success / warning=%q)", results[1].Name, results[1].Status, results[1].Warning)
		}
		// SDK 呼び出しは 2 つ目だけ (1 回)
		if len(sp.calls) != 1 {
			t.Fatalf("Phase 2 SDK 呼び出し回数 = %d (期待 1 / facet 不在の contract は呼ばれない)", len(sp.calls))
		}
		// akashic: SDK 呼ばれていない contract は akashic 行も 0 (facet 不在は SDK 起動前に弾く)
		if len(recorder.starts) != 1 || len(recorder.finishes) != 1 {
			t.Fatalf("akashic 行数 mismatch: starts=%d finishes=%d (期待 1/1)", len(recorder.starts), len(recorder.finishes))
		}
		if recorder.starts[0].StepID != "review-step:phase2:ok-doc" {
			t.Fatalf("akashic start StepID = %q (期待 review-step:phase2:ok-doc)", recorder.starts[0].StepID)
		}
	})

	t.Run("path_escape_no_retry", func(t *testing.T) {
		// contract.Name に "../escape" を渡し、writeReportFile が ErrPathEscape を返す経路。
		// retry は走らず failed として集約される (R5.4)。
		cwd := t.TempDir()
		recorder := &recordingRecorder{}
		sp := &scriptedProvider{
			id: provider.ProviderClaude,
			scripts: []scriptedResponse{
				// Phase 2 SDK 呼び出し自体は (path-escape 検出前に) 1 回走る。
				{resp: provider.AgentResponse{Content: "leaked body\n", SessionID: "p2-x", Success: true}},
			},
		}
		a := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
			provider.ProviderClaude: sp,
		}, recorder)

		contracts := []workflowctx.OutputContract{
			{Name: "../escape", FormatFacet: "plan"},
		}
		phase1Meta := newPhase1Metadata("review-workflow", "run-path", "step-path")

		results := a.runReportPhase(context.Background(), cwd, provider.ProviderClaude, "sess", phase1Meta, contracts)

		if len(results) != 1 {
			t.Fatalf("results 件数は 1 (got %d)", len(results))
		}
		r := results[0]
		if r.Name != "../escape" {
			t.Fatalf("results[0].Name = %q (期待 ../escape)", r.Name)
		}
		if r.Status != "failed" {
			t.Fatalf("results[0].Status = %q (期待 failed)", r.Status)
		}
		if r.ReportPath != "" {
			t.Fatalf("results[0].ReportPath = %q (failed では空文字)", r.ReportPath)
		}
		if r.RetryUsed {
			t.Fatalf("results[0].RetryUsed = true (期待 false / path-escape は retry 不可)")
		}
		if r.Warning == "" || !strings.Contains(r.Warning, "path") {
			t.Fatalf("results[0].Warning は path-escape を示す必要がある (got %q)", r.Warning)
		}

		// SDK 呼び出しは 1 回のみ (retry なし)
		if len(sp.calls) != 1 {
			t.Fatalf("Phase 2 SDK 呼び出し回数 = %d (期待 1 / path-escape では retry なし)", len(sp.calls))
		}
		// reportDir 外にファイルが作成されていないことを確認
		reportDirParent := filepath.Join(cwd, ".rilll", "review-workflow")
		if _, err := os.Stat(filepath.Join(reportDirParent, "escape.md")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reportDir 外に escape.md が作成された: err=%v", err)
		}
	})

	t.Run("transient_then_retry_success", func(t *testing.T) {
		// Phase 2 main で transient error → 新規 session で retry → success (R5.1)。
		// RetryUsed=true / akashic に 2 行 (main + retry) が記録される (R7.3)。
		cwd := t.TempDir()
		recorder := &recordingRecorder{}
		sp := &scriptedProvider{
			id: provider.ProviderClaude,
			scripts: []scriptedResponse{
				{resp: provider.AgentResponse{}, err: errors.New("transient sdk error")},
				{resp: provider.AgentResponse{Content: "retry success body\n", SessionID: "p2-retry", Success: true}, err: nil},
			},
		}
		a := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
			provider.ProviderClaude: sp,
		}, recorder)

		contracts := []workflowctx.OutputContract{
			{Name: "plan", FormatFacet: "plan"},
		}
		phase1Meta := newPhase1Metadata("review-workflow", "run-retry-ok", "step-retry")
		phase1SessionID := "sess-original"

		results := a.runReportPhase(context.Background(), cwd, provider.ProviderClaude, phase1SessionID, phase1Meta, contracts)

		if len(results) != 1 {
			t.Fatalf("results 件数は 1 (got %d)", len(results))
		}
		r := results[0]
		if r.Status != "success" {
			t.Fatalf("results[0].Status = %q (期待 success / warning=%q)", r.Status, r.Warning)
		}
		if !r.RetryUsed {
			t.Fatalf("results[0].RetryUsed = false (期待 true)")
		}
		if r.ReportPath == "" {
			t.Fatalf("results[0].ReportPath が空")
		}
		body, err := os.ReadFile(r.ReportPath)
		if err != nil {
			t.Fatalf("report ファイル ReadFile 失敗: %v", err)
		}
		if string(body) != "retry success body\n" {
			t.Fatalf("report body mismatch: got=%q want=%q (retry 後の body であること)", string(body), "retry success body\n")
		}

		// SDK 呼び出しは 2 回 (main + retry)
		if len(sp.calls) != 2 {
			t.Fatalf("Phase 2 SDK 呼び出し回数 = %d (期待 2)", len(sp.calls))
		}
		// main: ResumeSessionID は phase1SessionID
		if sp.calls[0].ResumeSessionID != phase1SessionID {
			t.Fatalf("main call ResumeSessionID = %q (期待 %q)", sp.calls[0].ResumeSessionID, phase1SessionID)
		}
		// retry: ResumeSessionID は空文字 (新規 session)
		if sp.calls[1].ResumeSessionID != "" {
			t.Fatalf("retry call ResumeSessionID = %q (期待 \"\")", sp.calls[1].ResumeSessionID)
		}
		// step_id 規約: main / retry
		if sp.calls[0].Metadata.StepID != "step-retry:phase2:plan" {
			t.Fatalf("main call StepID = %q (期待 step-retry:phase2:plan)", sp.calls[0].Metadata.StepID)
		}
		if sp.calls[1].Metadata.StepID != "step-retry:phase2:plan:retry" {
			t.Fatalf("retry call StepID = %q (期待 step-retry:phase2:plan:retry)", sp.calls[1].Metadata.StepID)
		}
		// main と retry の AgentCallID は別であること (akashic 行が物理的に別行になる)
		if sp.calls[0].Metadata.AgentCallID == sp.calls[1].Metadata.AgentCallID {
			t.Fatalf("main と retry の AgentCallID が同一 (Phase 2 retry は別 agent_call として記録される必要がある)")
		}

		// akashic: 2 行 (main + retry)
		if len(recorder.starts) != 2 || len(recorder.finishes) != 2 {
			t.Fatalf("akashic 行数 mismatch: starts=%d finishes=%d (期待 2/2)", len(recorder.starts), len(recorder.finishes))
		}
		// step_id の retry サフィックス
		stepIDs := []string{recorder.starts[0].StepID, recorder.starts[1].StepID}
		if stepIDs[0] != "step-retry:phase2:plan" || stepIDs[1] != "step-retry:phase2:plan:retry" {
			t.Fatalf("akashic StepIDs mismatch: got=%v want=[step-retry:phase2:plan step-retry:phase2:plan:retry]", stepIDs)
		}
		// main は failed (Success=false), retry は success
		if recorder.finishes[0].Success {
			t.Fatalf("akashic main finish Success = true (期待 false / transient error)")
		}
		if !recorder.finishes[1].Success {
			t.Fatalf("akashic retry finish Success = false (期待 true)")
		}
	})

	t.Run("transient_then_retry_failure", func(t *testing.T) {
		// Phase 2 main / retry 両方が失敗 → Status=failed + Warning, RetryUsed=true (R5.2)。
		// Phase 1 の応答は本 helper のスコープ外なので、ここでは results 側のみを assert する。
		cwd := t.TempDir()
		recorder := &recordingRecorder{}
		sp := &scriptedProvider{
			id: provider.ProviderClaude,
			scripts: []scriptedResponse{
				{resp: provider.AgentResponse{}, err: errors.New("main transient error")},
				{resp: provider.AgentResponse{}, err: errors.New("retry transient error")},
			},
		}
		a := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
			provider.ProviderClaude: sp,
		}, recorder)

		contracts := []workflowctx.OutputContract{
			{Name: "plan", FormatFacet: "plan"},
		}
		phase1Meta := newPhase1Metadata("review-workflow", "run-retry-fail", "step-rf")

		results := a.runReportPhase(context.Background(), cwd, provider.ProviderClaude, "sess", phase1Meta, contracts)

		if len(results) != 1 {
			t.Fatalf("results 件数は 1 (got %d)", len(results))
		}
		r := results[0]
		if r.Status != "failed" {
			t.Fatalf("results[0].Status = %q (期待 failed)", r.Status)
		}
		if !r.RetryUsed {
			t.Fatalf("results[0].RetryUsed = false (期待 true / main 失敗 → retry を試みたため)")
		}
		if r.Warning == "" {
			t.Fatalf("results[0].Warning が空 (retry 失敗の理由が必要)")
		}
		if r.ReportPath != "" {
			t.Fatalf("results[0].ReportPath = %q (failed では空文字)", r.ReportPath)
		}

		// SDK 呼び出しは 2 回 (main + retry)
		if len(sp.calls) != 2 {
			t.Fatalf("Phase 2 SDK 呼び出し回数 = %d (期待 2)", len(sp.calls))
		}
		// akashic 2 行 (両方 Success=false)
		if len(recorder.finishes) != 2 {
			t.Fatalf("akashic finishes = %d (期待 2)", len(recorder.finishes))
		}
		if recorder.finishes[0].Success || recorder.finishes[1].Success {
			t.Fatalf("akashic 両 finish Success が true 含む (期待 両方 false)")
		}
	})

	t.Run("missing_run_metadata_guard", func(t *testing.T) {
		// phase1Metadata.WorkflowID / WorkflowRunID が空 → reportDir 算出不能。
		// 全 contract を Status="failed" + Warning に明示して return する caller 側責務違反ガード。
		cwd := t.TempDir()
		recorder := &recordingRecorder{}
		sp := &scriptedProvider{
			id: provider.ProviderClaude,
			// scripts は空: SDK 呼び出しが 1 回でも発生したら scriptedProvider が error を返す。
		}
		a := New(provider.ProviderClaude, map[provider.ProviderID]provider.AgentProvider{
			provider.ProviderClaude: sp,
		}, recorder)

		contracts := []workflowctx.OutputContract{
			{Name: "plan", FormatFacet: "plan"},
			{Name: "review", FormatFacet: "review-gather"},
		}
		// WorkflowID / WorkflowRunID が空の phase1Metadata
		phase1Meta := provider.AgentMetadata{
			WorkflowName: "review-workflow",
			StepID:       "step-x",
			AgentCallID:  "phase1-call",
		}

		results := a.runReportPhase(context.Background(), cwd, provider.ProviderClaude, "sess", phase1Meta, contracts)

		if len(results) != 2 {
			t.Fatalf("results 件数は 2 (got %d)", len(results))
		}
		for i, r := range results {
			if r.Status != "failed" {
				t.Fatalf("results[%d].Status = %q (期待 failed / RunMetadata 不在ガード)", i, r.Status)
			}
			if r.Warning == "" {
				t.Fatalf("results[%d].Warning が空 (caller 側責務違反の理由が必要)", i)
			}
			if r.ReportPath != "" {
				t.Fatalf("results[%d].ReportPath = %q (期待 \"\" / 書き出し未発生)", i, r.ReportPath)
			}
			if r.RetryUsed {
				t.Fatalf("results[%d].RetryUsed = true (期待 false / SDK 呼び出し未発生)", i)
			}
		}
		// SDK 呼び出しゼロ件 / akashic ゼロ件 (副作用なし)
		if len(sp.calls) != 0 {
			t.Fatalf("Phase 2 SDK 呼び出し回数 = %d (期待 0 / RunMetadata 不在ガードで Phase 2 を起動しない)", len(sp.calls))
		}
		if len(recorder.starts) != 0 || len(recorder.finishes) != 0 {
			t.Fatalf("akashic 行数 mismatch: starts=%d finishes=%d (期待 0/0)", len(recorder.starts), len(recorder.finishes))
		}
	})
}
