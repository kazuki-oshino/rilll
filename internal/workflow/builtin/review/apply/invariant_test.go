// Package apply のテスト本ファイルは task 5.6 / design.md §Workflow §Risk-1 §Verdict
// Invariants §Result / Req 6.4, 7.1, 7.4 で要求される invariant test 群を提供する。
//
// 検証する不変式 (3 件):
//
//  1. **runOrCancel ガード不変式 (Req 6.4 / Risk-1)**:
//     `GitOpsExt.Apply` / `Add` / `Commit` / `Revert` の各副作用呼び出しは、必ず
//     `runOrCancel(ctx, fn)` または等価な `select { case <-ctx.Done(): ...; default: ... }`
//     ブロック経由で発火する。design.md §Open Questions Risk-1 (確定) で AST ベースの
//     scanner を `tasks.md` の必須 task として降ろす方針が確定しており、本ファイルが
//     その実体を提供する。新規副作用追加時に helper 経由を機械的に強制し、
//     cancel 注入漏れによる中断時 revert 失敗を構造的に塞ぐ。
//
//  2. **VerdictUnset 永続化禁止不変式 (Req 7.4 / design.md §Verdict Invariants)**:
//     `result.json.final_verdict` に `VerdictUnset` (空文字) を書き出さない。
//     production コード上で `record.FinalVerdict = VerdictUnset` への直接代入が
//     存在しないこと (静的検査) と、`WriteResultJSON` が VerdictUnset を渡された場合に
//     `ErrFinalVerdictUnset` を返してファイル書き出しを行わないこと (動的検査) を併せて
//     固定する。
//
//  3. **cycle 番号連番不変式 (Req 7.1)**:
//     `CycleRecord.Cycle` は 1 始まり連番 (= attempt 番号と同値) で永続化される。
//     `SelfReview` が記録された cycle では `SelfReview.Attempt` も同値であること。
//     `MaxRegenerateAttempts` 連続 regenerate (cycle_exhausted) シナリオで Cycles[i].Cycle
//     == i+1 を機械的に固定する。
//
// 設計契約:
//   - 本ファイルの AST scanner は production コード (`*.go` で `_test.go` を除く) を
//     パッケージディレクトリ直下から読み込み、go/ast で AST を構築する。
//   - scanner の helper 名・引数順契約 (`runOrCancel(ctx, fn)`) は cancel_guard.go の
//     コメントと両端契約として同期する。改名する場合は本 invariant test も同時改訂する。
//   - 受信子名 (`gitOps`) は apply_phase.go / propose_loop.go 等の production コードで
//     一貫して用いる規約で、新規副作用呼び出しを別の receiver 名で導入する場合は
//     本 scanner の receiverAllowList も同時改訂する (production コード側の SSoT 化)。
//
// negative test (skip タグ付き):
//   - 副作用呼び出しを runOrCancel 経由でない形で意図的に追加した「悪い」Go ソース文字列を
//     `parser.ParseFile` で AST 化し、scanner が違反を検出することを示す。
//   - `t.Skip(...)` で CI 経路では実行しない (positive test が production コード上で
//     常時 green であることが本 invariant の本筋)。手動で skip を外して実行することで
//     「scanner が壊れていない」「未来の違反追加で fail する」ことを確認できる。
package apply

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

// runOrCancelHelperName は AST scanner が「ガード経由」と判定する helper 名。
// cancel_guard.go の `runOrCancel(ctx, fn)` と両端契約で同期する。
const runOrCancelHelperName = "runOrCancel"

// guardedSideEffectMethods は runOrCancel 経由を強制する `GitOpsExt` 上のメソッド名集合。
// design.md §Risk-1 (確定) で AST scanner の対象として降ろした 4 メソッド。
//
// 不変式契約: GitOpsExt に新たな副作用メソッド (例: 将来の Push / Tag) を追加する際は
// 本集合と cancel_guard.go の helper 経由規約を同時改訂する (両端契約 SSoT)。
var guardedSideEffectMethods = map[string]bool{
	"Apply":  true,
	"Add":    true,
	"Commit": true,
	"Revert": true,
}

// gitOpsReceiverAllowList は AST scanner が「GitOpsExt メソッド呼び出し」と判定する
// receiver 名集合。production コード (apply_phase.go) で一貫して `gitOps` を用いる規約。
//
// 拡張時の規約: 別の receiver 名 (例: `g` / `client`) を導入する場合は本集合と
// production コードの命名規約を同時改訂する。`g.runGit` のような無関係な field 呼び出しは
// メソッド名フィルタ (guardedSideEffectMethods) で別途除外される。
var gitOpsReceiverAllowList = map[string]bool{
	"gitOps": true,
}

// productionGoFiles はパッケージディレクトリ直下の `_test.go` を除く `*.go` ファイル一覧を返す。
// AST scanner / 静的検査 helper の共通入口として利用する。
//
// テストは package directory で実行されるため、`os.ReadDir(".")` で十分。
// ファイル名でソートして deterministic にする (差分検査 / 再現性のため)。
func productionGoFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("os.ReadDir(.): %v", err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, name)
	}
	sort.Strings(files)
	return files
}

// scanRunOrCancelInvariant は与えられた Go ソース (filename, src) を AST 化し、
// 「`gitOps.{Apply|Add|Commit|Revert}` 呼び出しが `runOrCancel(ctx, fn)` の引数 FuncLit
// 配下に存在するか」を機械検証する。
//
// 戻り値:
//   - violations: 違反 1 件につき `<file>:<line>: <recv>.<method>() not guarded by runOrCancel` 形の
//     人間可読メッセージ。空 slice なら不変式を満たす。
//
// アルゴリズム:
//  1. AST を walk し、すべての `runOrCancel(ctx, FuncLit)` 呼び出しを抽出する。
//     その第 2 引数 (FuncLit) の Body 内に出現する `*ast.CallExpr` のソース位置 (`token.Pos`) を
//     `guarded` set に登録する。
//  2. AST を再 walk し、`*ast.SelectorExpr` を持つ `*ast.CallExpr` のうち、
//     - メソッド名が `guardedSideEffectMethods` のいずれか
//     - receiver が `gitOpsReceiverAllowList` のいずれか
//     を満たす呼び出しを抽出する。各呼び出しのソース位置が `guarded` set に含まれるか確認し、
//     未含有なら違反として記録する。
//
// 限界 (False Negative):
//   - 受信子名が allow list 外 (例: 別変数経由で間接呼び出し) の経路は検出できない。
//     production コードの命名規約 (gitOps) を SSoT として固定する代わりに、scanner は
//     名前ベース検査に留める。
func scanRunOrCancelInvariant(filename string, src []byte) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parser.ParseFile(%s): %w", filename, err)
	}

	// (1) runOrCancel 経由の CallExpr 位置を guarded set に登録する。
	guarded := map[token.Pos]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name != runOrCancelHelperName {
			return true
		}
		if len(call.Args) < 2 {
			return true
		}
		fnLit, ok := call.Args[1].(*ast.FuncLit)
		if !ok || fnLit.Body == nil {
			return true
		}
		ast.Inspect(fnLit.Body, func(inner ast.Node) bool {
			if c, ok := inner.(*ast.CallExpr); ok {
				guarded[c.Pos()] = true
			}
			return true
		})
		return true
	})

	// (2) gitOps.{Apply|Add|Commit|Revert} 呼び出しのうち、guarded に未含有のものを違反として記録する。
	var violations []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if !guardedSideEffectMethods[sel.Sel.Name] {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if !gitOpsReceiverAllowList[recv.Name] {
			return true
		}
		if guarded[call.Pos()] {
			return true
		}
		pos := fset.Position(call.Pos())
		violations = append(violations, fmt.Sprintf(
			"%s:%d: %s.%s() not guarded by %s",
			filepath.Base(pos.Filename), pos.Line, recv.Name, sel.Sel.Name, runOrCancelHelperName,
		))
		return true
	})

	return violations, nil
}

// TestInvariant_GitOpsExtCallsGuardedByRunOrCancel は production コード全体で
// `gitOps.{Apply|Add|Commit|Revert}` の呼び出しが必ず `runOrCancel(ctx, fn)` 経由で
// 発火していることを AST scanner で機械検証する (Req 6.4 / design.md §Risk-1)。
//
// 期待:
//   - 全 production .go ファイル (テスト除外) で違反 0 件 (= scanner の戻り値が空 slice)。
//   - 違反検出時はファイル名 + 行番号 + 受信子名 + メソッド名で fail する。
func TestInvariant_GitOpsExtCallsGuardedByRunOrCancel(t *testing.T) {
	files := productionGoFiles(t)
	if len(files) == 0 {
		t.Fatalf("production .go ファイルが見つからない (productionGoFiles の起点ディレクトリを確認)")
	}

	var allViolations []string
	for _, fname := range files {
		src, err := os.ReadFile(fname)
		if err != nil {
			t.Fatalf("os.ReadFile(%s): %v", fname, err)
		}
		violations, err := scanRunOrCancelInvariant(fname, src)
		if err != nil {
			t.Fatalf("scanRunOrCancelInvariant(%s): %v", fname, err)
		}
		allViolations = append(allViolations, violations...)
	}

	if len(allViolations) > 0 {
		t.Fatalf(
			"runOrCancel ガード不変式違反 (%d 件):\n  %s\n\n"+
				"対象メソッド: gitOps.{Apply,Add,Commit,Revert} はすべて runOrCancel(ctx, fn) 経由で呼び出してください。\n"+
				"design.md §Risk-1 (確定) / cancel_guard.go の helper 名契約と同期します。",
			len(allViolations), strings.Join(allViolations, "\n  "),
		)
	}
}

// TestInvariantNegative_DetectsUngardedGitOpsCall は AST scanner が「悪い」入力に対して
// 違反を正しく検出できることを inline fixture で示す。
//
// 本テストは `t.Skip(...)` で CI 経路では実行しない (positive test が production コードで
// 常時 green であることが本 invariant の本筋)。手動で skip を外して実行することで
// scanner の検出力を再確認できる (例: scanner 改修時の回帰検証 / 命名規約変更時の確認)。
//
// 観察可能完了条件 (tasks.md 5.6): 「helper 未経由の副作用呼び出しを試験的に追加すると
// AST scanner が fail する (negative test 1 ケースを skip タグ付きで併置)」を満たす。
func TestInvariantNegative_DetectsUngardedGitOpsCall(t *testing.T) {
	t.Skip("scanner の検出力を手動で確認する場合のみ -run TestInvariantNegative で実行する。")

	// inline fixture: gitOps.Apply / Add / Commit / Revert を runOrCancel 経由でなく直接呼ぶ「悪い」コード。
	// 各メソッドにつき 1 件ずつ違反を仕込み、scanner が 4 件すべてを検出することを assert する。
	const ungardedSrc = `package apply

import "context"

func sample(ctx context.Context, cwd string, gitOps GitOpsExt, diff []byte, files []string) {
	_ = gitOps.Apply(ctx, cwd, diff)        // 違反 1: Apply 直接呼び出し
	_ = gitOps.Add(ctx, cwd, files)         // 違反 2: Add 直接呼び出し
	_ = gitOps.Commit(ctx, cwd, "m", files) // 違反 3: Commit 直接呼び出し
	_ = gitOps.Revert(ctx, cwd, BaselineRef{}, files) // 違反 4: Revert 直接呼び出し
}
`
	violations, err := scanRunOrCancelInvariant("ungarded_fixture.go", []byte(ungardedSrc))
	if err != nil {
		t.Fatalf("scanRunOrCancelInvariant: %v", err)
	}
	if len(violations) != 4 {
		t.Fatalf("scanner が違反を検出できない: violations=%v, want 4 件", violations)
	}
	for _, want := range []string{".Apply()", ".Add()", ".Commit()", ".Revert()"} {
		var found bool
		for _, v := range violations {
			if strings.Contains(v, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("scanner 出力に %q が含まれない: %v", want, violations)
		}
	}
}

// scanFinalVerdictUnsetAssignments は与えられた Go ソース (filename, src) を AST 化し、
// `record.FinalVerdict = VerdictUnset` 形の代入文の出現位置を抽出する (Req 7.4 静的検査)。
//
// 戻り値:
//   - hits: ファイル名 + 行番号 + assignee 名の文字列 slice。空 slice なら不変式を満たす。
//
// アルゴリズム:
//   - `*ast.AssignStmt` のうち、LHS が `<x>.FinalVerdict` 形 (`*ast.SelectorExpr.Sel.Name == "FinalVerdict"`)
//     かつ RHS が `VerdictUnset` 識別子の代入を抽出する。
//   - 比較演算 (`==`) は AssignStmt ではないため自然に除外される (workflow.go の if 文等は false-positive にならない)。
func scanFinalVerdictUnsetAssignments(filename string, src []byte) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parser.ParseFile(%s): %w", filename, err)
	}

	var hits []string
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		if len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		sel, ok := assign.Lhs[0].(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "FinalVerdict" {
			return true
		}
		rhs, ok := assign.Rhs[0].(*ast.Ident)
		if !ok || rhs.Name != "VerdictUnset" {
			return true
		}
		pos := fset.Position(assign.Pos())
		hits = append(hits, fmt.Sprintf("%s:%d: %s.FinalVerdict = VerdictUnset",
			filepath.Base(pos.Filename), pos.Line, exprString(sel.X)))
		return true
	})
	return hits, nil
}

// exprString は AST ノード (主に SelectorExpr の receiver) を簡易的に文字列化する。
// 完全な go/printer よりは粗いが、エラーメッセージ用途では十分。
func exprString(expr ast.Expr) string {
	switch v := expr.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	default:
		return fmt.Sprintf("%T", expr)
	}
}

// TestInvariant_NoVerdictUnsetAssignmentInProductionCode は production コード上で
// `<x>.FinalVerdict = VerdictUnset` への直接代入が存在しないことを静的検査する
// (Req 7.4 / design.md §Verdict Invariants)。
//
// この不変式は orchestration の defer 経路 (workflow.go) で「`VerdictUnset` のまま return した経路は
// `VerdictFailed` に fallback する」契約と組み合わさることで、`result.json.final_verdict` への
// VerdictUnset 永続化を構造的に防止する。
//
// production コードでの正しい代入経路は `setRecordVerdict(record, v, summary)` のみ。
// 本 helper は `v Verdict` を受け取り、record.FinalVerdict に格納するが、
// 呼び出し側が VerdictUnset を渡さない契約 (workflow.go の callsite 列挙で明示) で守られる。
func TestInvariant_NoVerdictUnsetAssignmentInProductionCode(t *testing.T) {
	files := productionGoFiles(t)
	if len(files) == 0 {
		t.Fatalf("production .go ファイルが見つからない")
	}

	var hits []string
	for _, fname := range files {
		src, err := os.ReadFile(fname)
		if err != nil {
			t.Fatalf("os.ReadFile(%s): %v", fname, err)
		}
		fileHits, err := scanFinalVerdictUnsetAssignments(fname, src)
		if err != nil {
			t.Fatalf("scanFinalVerdictUnsetAssignments(%s): %v", fname, err)
		}
		hits = append(hits, fileHits...)
	}

	if len(hits) > 0 {
		t.Fatalf(
			"VerdictUnset 永続化禁止不変式違反 (%d 件):\n  %s\n\n"+
				"production コードで `<x>.FinalVerdict = VerdictUnset` 形の直接代入は禁止です。\n"+
				"verdict の確定は `setRecordVerdict(record, v, summary)` 経由で行ってください。",
			len(hits), strings.Join(hits, "\n  "),
		)
	}
}

// TestInvariant_WriteResultJSONRejectsVerdictUnset は WriteResultJSON の動的契約として
// `FinalVerdict == VerdictUnset` の record を渡された場合に `ErrFinalVerdictUnset` を返し、
// ファイル書き出しと artifact 登録のいずれも実行しないことを再確認する
// (Req 7.4 / design.md §Verdict Invariants)。
//
// result_test.go の TestWriteResultJSONRejectsVerdictUnset と重なるが、本 invariant test では
// 「invariant_test.go から見ても WriteResultJSON が VerdictUnset を弾く」ことを独立に anchor する。
// result.go を refactor した際に invariant 不変式が破綻していないかを 1 ファイルで検証可能にする。
func TestInvariant_WriteResultJSONRejectsVerdictUnset(t *testing.T) {
	runDir := t.TempDir()
	record := &ResultRecord{
		WorkflowRunID: "invariant-run",
		ReviewRunID:   "invariant-review",
		FinalVerdict:  VerdictUnset,
		FinalSummary:  "should not be persisted",
	}

	err := WriteResultJSON(context.Background(), runDir, record)
	if err == nil {
		t.Fatal("WriteResultJSON: error が返らない (VerdictUnset 拒否の不変式違反)")
	}
	if !errors.Is(err, ErrFinalVerdictUnset) {
		t.Errorf("err=%v, want errors.Is(err, ErrFinalVerdictUnset)", err)
	}

	// ファイルが書き出されていないこと (痕跡を残さない不変式)。
	path := filepath.Join(runDir, "result.json")
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("VerdictUnset 拒否経路で result.json が書き出された (path=%s, statErr=%v)", path, statErr)
	}
}

// TestInvariant_CycleNumbersAreSequential は MaxRegenerateAttempts 連続 regenerate 経路
// (cycle_exhausted) で `CycleRecord.Cycle` が 1 始まり連番 (= attempt 番号と同値) であり、
// `SelfReview.Attempt` が記録された cycle では `Cycle` と一致することを機械検証する
// (Req 7.1 / design.md §Result §State Management)。
//
// scenario: 5 cycle 連続 regenerate → cycle_exhausted 経路。
// 本テストは propose_loop_test.go の TestProposeLoop_CycleExhausted と検証観点が
// 一部重なるが、invariant_test.go では cycle 連番性のみに焦点を絞り、artifact 件数や
// agent 呼び出し回数等は別 test の責務とする (関心の分離)。
func TestInvariant_CycleNumbersAreSequential(t *testing.T) {
	validDiffJSON := makePatchAgentJSON(t, proposeLoopAuthLoginDiff, []string{"auth/login.go"})
	responses := make([]provider.AgentResponse, 0, MaxRegenerateAttempts*3)
	for i := 1; i <= MaxRegenerateAttempts; i++ {
		responses = append(responses,
			planResp("invariant cycle plan"),
			diffResp(validDiffJSON),
			selfReviewResp(t, SelfReviewRegenerate, "規約上 attempt N は再生成"),
		)
	}
	agent := &scriptedProposeAgent{responses: responses}

	_, rec, _, err := runProposeLoopWorkflowForTest(t, "invariant-cycle-seq", agent, nil)
	if err == nil {
		t.Fatalf("cycle_exhausted 経路で error が nil")
	}
	if rec.FinalVerdict != VerdictCycleExhausted {
		t.Fatalf("final_verdict=%q, want %q", rec.FinalVerdict, VerdictCycleExhausted)
	}

	// 不変式: Cycles は MaxRegenerateAttempts 件、Cycle 値は 1 始まり連番。
	if got, want := len(rec.Cycles), MaxRegenerateAttempts; got != want {
		t.Fatalf("len(Cycles)=%d, want %d (1 始まり連番不変式)", got, want)
	}
	for i, c := range rec.Cycles {
		want := i + 1
		if c.Cycle != want {
			t.Errorf("Cycles[%d].Cycle=%d, want %d (1 始まり連番不変式)", i, c.Cycle, want)
		}
		// SelfReview が記録された cycle では Attempt も Cycle と一致する不変式
		// (CycleRecord.Cycle と SelfReviewRecord.Attempt の整合)。
		if c.SelfReview != nil && c.SelfReview.Attempt != c.Cycle {
			t.Errorf("Cycles[%d].SelfReview.Attempt=%d, want %d (CycleRecord.Cycle と一致)",
				i, c.SelfReview.Attempt, c.Cycle)
		}
	}

	// expected slice 全体を deep-equal で再確認する (回帰検出 anchor)。
	expectedCycleNumbers := make([]int, MaxRegenerateAttempts)
	for i := range expectedCycleNumbers {
		expectedCycleNumbers[i] = i + 1
	}
	gotCycleNumbers := make([]int, len(rec.Cycles))
	for i, c := range rec.Cycles {
		gotCycleNumbers[i] = c.Cycle
	}
	if !reflect.DeepEqual(gotCycleNumbers, expectedCycleNumbers) {
		t.Errorf("Cycle 番号列=%v, want %v", gotCycleNumbers, expectedCycleNumbers)
	}
}
