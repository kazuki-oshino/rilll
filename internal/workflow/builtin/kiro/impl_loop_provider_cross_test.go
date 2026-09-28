// Package workflow の本テストファイルは kiro-impl-loop 配線層の provider 横断観測を
// 機械的に検証する統合テストを集約する。
//
// 観察対象:
//
//   - sub: identical-prompt-across-providers
//     `scriptedAgent.Respond` が返す `AgentResponse.SystemInfo.ProviderID` を
//     Claude / Codex / Cursor の 3 種類で切り替え、2 task no-inquiry 連鎖シナリオの
//     per-task session A 初回 skill prompt を 3 provider 間で完全一致 (byte-identical) で
//     観測する。prompt は kiro 配線層 (`runKiroImplLoop` の promptCmd 組み立て +
//     agentCallerAdapter による foundation `previous-response-passing` 経路 wiring) で
//     構築され、impl_loop.go / impl_loop_facets.go / spec_loop.go agentCallerAdapter.Call の
//     いずれにも provider 名分岐は無いため、3 provider 間で normalize 後の prompt は
//     byte-identical になる契約。任意の provider 分岐が将来混入した瞬間に本サブテストが落ちる。
//
//   - sub: no-provider-symbol-in-impl-loop-files
//     impl_loop.go / impl_loop_facets.go / spec_loop.go (agentCallerAdapter.Call のみ) を
//     `go/parser` + `go/ast` で走査し、`provider.ProviderID` / `provider.ProviderClaude` /
//     `provider.ProviderCodex` / `provider.ProviderCursor` / `provider.ProviderCursorSDK` の
//     `*ast.SelectorExpr` が一切現れないことを機械的に確認する。
package kiro

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

// TestKiroImplLoopProviderCrossInvariants は kiro-impl-loop 配線層の provider 横断
// 同一観測 (impl_loop / impl_loop_facets / spec_loop:agentCallerAdapter.Call に provider
// 名分岐が存在しないこと) を 1 関数内のサブテストで担保する統合テスト。
func TestKiroImplLoopProviderCrossInvariants(t *testing.T) {
	// ------------------------------------------------------------------
	// sub: identical-prompt-across-providers
	// ------------------------------------------------------------------
	//
	// 3 provider (Claude / Codex / Cursor) × 2 task (1.1=N=1 前置なし / 1.2=N=2 前置あり) の
	// クロス観測で、各 task の session A 初回 skill prompt が provider 名に依存しないことを
	// byte-identical (cwd 正規化後) で verify する。
	t.Run("identical-prompt-across-providers", func(t *testing.T) {
		feature := "demo"
		// 2 task 連鎖の no-inquiry シナリオで N=1 / N=2 を同時カバーする。N=2 まで走らせる
		// ことで `previousResponseBlock(prev)` の後置 block 連結経路を踏んだ provider 横断観測
		// になる (kiro-impl-loop の prompt 組み立てに provider 名分岐が混入していないことを
		// byte-identical で担保する)。
		taskIDs := []string{"1.1", "1.2"}

		providerCases := []struct {
			name string
			pid  provider.ProviderID
		}{
			{name: "claude", pid: provider.ProviderClaude},
			{name: "codex", pid: provider.ProviderCodex},
			{name: "cursor", pid: provider.ProviderCursor},
		}

		// 各 provider × 各 task 用に normalize 後の prompt を集める。最後に基準 provider
		// (claude) と他 provider との byte-identical 比較を実施する。
		promptByTaskByProvider := map[string]map[string]string{}
		for _, tid := range taskIDs {
			promptByTaskByProvider[tid] = map[string]string{}
		}

		for _, pc := range providerCases {
			pc := pc
			t.Run(pc.name, func(t *testing.T) {
				cwd := setupImplFeatureCWD(t, feature, "- [ ] 1.1 First\n- [ ] 1.2 Second\n")
				// macOS では `t.TempDir()` が `/var/folders/...` を返す一方、
				// `filepath.Abs` は `/private/var/folders/...` に展開されることがあるため
				// 両方を <CWD> sentinel に置換する。
				cwdAbs := cwd
				if abs, err := filepath.Abs(cwd); err == nil {
					cwdAbs = abs
				}
				onEvent, _ := collectEvents()
				ask := &fakeAsk{Selected: feature}

				agent := &scriptedAgent{}
				agent.Respond = func(call recordedAgentCall) (provider.AgentResponse, error) {
					// 各応答に provider mock 識別用 SystemInfo を載せる。本 mock が観察する
					// prompt 自体は provider 名に依らず同一であることを上位テストで verify する。
					sysInfo := &provider.AgentSystemInfo{ProviderID: pc.pid}
					switch {
					case call.Kind == "commit":
						resp := commitOKResp()
						resp.SystemInfo = sysInfo
						return resp, nil
					case strings.Contains(call.Prompt, "/kiro-validate-impl"):
						resp := implFinalReviewOKResp()
						resp.SystemInfo = sysInfo
						return resp, nil
					case strings.Contains(call.Prompt, "/kiro-impl "+feature+" 1.1"):
						resp := taskSummaryResp("1.1")
						resp.SystemInfo = sysInfo
						return resp, nil
					case strings.Contains(call.Prompt, "/kiro-impl "+feature+" 1.2"):
						resp := taskSummaryResp("1.2")
						resp.SystemInfo = sysInfo
						return resp, nil
					default:
						resp := skillNoInquiryResp()
						resp.SystemInfo = sysInfo
						return resp, nil
					}
				}

				if err := runKiroImplLoop(
					context.Background(),
					cwd,
					onEvent,
					ask,
					agent,
				); err != nil {
					t.Fatalf("provider=%s: runKiroImplLoop returned error: %v", pc.name, err)
				}

				// cwd 正規化: 「provider 名に依存しない可変要素」 (= cwd 由来の差) を意図的に
				// 剥がす。provider 名による分岐が入った瞬間は normalize 後でも prompt が分裂する
				// ため検出される (sister spec 6.7 と同方針)。
				normalize := func(s string) string {
					s = strings.ReplaceAll(s, cwdAbs, "<CWD>")
					if cwdAbs != cwd {
						s = strings.ReplaceAll(s, cwd, "<CWD>")
					}
					return s
				}

				// task 1.1 / 1.2 の session A 初回 skill 呼び出しを抽出する。
				// 条件: Kind="skill" / ResumeSessionID="" (初回) / StepName が "-answer" suffix
				// ではない。rounds=0 (has_inquiry=false 即完走) 前提のため各 task 1 件のみ発火。
				for _, c := range agent.Calls {
					if c.Kind != "skill" || c.ResumeSessionID != "" || c.Options == nil {
						continue
					}
					if strings.HasSuffix(c.Options.StepName, "-answer") {
						continue
					}
					for _, tid := range taskIDs {
						if strings.Contains(c.Prompt, "/kiro-impl "+feature+" "+tid) {
							promptByTaskByProvider[tid][pc.name] = normalize(c.Prompt)
							break
						}
					}
				}
				for _, tid := range taskIDs {
					if _, ok := promptByTaskByProvider[tid][pc.name]; !ok {
						t.Fatalf("provider=%s: task %q の session A 初回 skill 呼び出しが未発火 (calls=%d)",
							pc.name, tid, len(agent.Calls))
					}
				}
			})
		}

		// 3 provider 間で prompt が byte-identical (cwd 正規化後) であることを task ごとに verify する。
		// 基準は claude (providerCases[0]) の prompt とし、codex / cursor との差分を report する。
		// Req 6.1 / 6.2 / 6.3 の核心観測: 「kiro 配線層は provider 名で分岐していない」。
		const base = "claude"
		others := []string{"codex", "cursor"}
		for _, tid := range taskIDs {
			basePrompt, ok := promptByTaskByProvider[tid][base]
			if !ok {
				t.Fatalf("基準 provider %q の task %q prompt が観測できていない", base, tid)
			}
			for _, oname := range others {
				other, ok := promptByTaskByProvider[tid][oname]
				if !ok {
					t.Fatalf("provider %q の task %q prompt が観測できていない", oname, tid)
				}
				if other != basePrompt {
					t.Errorf("task %q: provider %q と %q の prompt が一致しない (Req 6.1 / 6.2 / 6.3 — kiro 配線層は provider 名で分岐してはならない)\n base(%s)=%q\nother(%s)=%q",
						tid, base, oname, base, basePrompt, oname, other)
				}
			}
		}
	})

	// ------------------------------------------------------------------
	// sub: no-provider-symbol-in-impl-loop-files (Req 6.3)
	// ------------------------------------------------------------------
	//
	// 観察対象 (tasks.md 4.7 spec 文「本 spec で改修する 3 ソースファイル」):
	//   - impl_loop.go            (全 FuncDecl.Body)
	//   - impl_loop_facets.go     (全 FuncDecl.Body)
	//   - spec_loop.go            (agentCallerAdapter.Call メソッドのみ)
	//
	// 検査手段: go/parser.ParseFile で AST を取得し、ast.Inspect で対象 FuncDecl.Body のみを
	// 走査して `*ast.SelectorExpr` の `<X>.<Sel>` を `<X>=ident("provider")` かつ
	// `<Sel>=ident("ProviderID" / "ProviderClaude" / "ProviderCodex" / "ProviderCursor" /
	// "ProviderCursorSDK")` の組合せでチェックする。
	t.Run("no-provider-symbol-in-impl-loop-files", func(t *testing.T) {
		bannedSelectors := []string{
			"ProviderID",
			"ProviderClaude",
			"ProviderCodex",
			"ProviderCursor",
			"ProviderCursorSDK",
		}

		// inspectBody は与えられた node 配下を走査して banned SelectorExpr を検出する純関数。
		// AST はコメントを expression として持たないため、コメント中の `provider.ProviderID`
		// ドキュメント文字列は自動で除外される (= 本 assertion は誤検出しない)。
		inspectBody := func(node ast.Node, fset *token.FileSet, label string) []string {
			var violations []string
			ast.Inspect(node, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				xIdent, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				if xIdent.Name != "provider" {
					return true
				}
				if sel.Sel == nil {
					return true
				}
				for _, banned := range bannedSelectors {
					if sel.Sel.Name == banned {
						pos := fset.Position(sel.Pos())
						violations = append(violations, fmt.Sprintf("%s: provider.%s @ %s:%d:%d",
							label, banned, pos.Filename, pos.Line, pos.Column))
					}
				}
				return true
			})
			return violations
		}

		// recvNameOf は FuncDecl の receiver 型名 (pointer / value 両対応) を取り出す。
		// receiver なし (関数) のときは "" を返す。
		recvNameOf := func(fd *ast.FuncDecl) string {
			if fd.Recv == nil || len(fd.Recv.List) == 0 {
				return ""
			}
			switch te := fd.Recv.List[0].Type.(type) {
			case *ast.Ident:
				return te.Name
			case *ast.StarExpr:
				if id, ok := te.X.(*ast.Ident); ok {
					return id.Name
				}
			}
			return ""
		}

		// (A) impl_loop.go と impl_loop_facets.go: 全 FuncDecl.Body を走査
		entireFileTargets := []string{
			"impl_loop.go",
			"impl_loop_facets.go",
		}
		for _, target := range entireFileTargets {
			target := target
			t.Run(target, func(t *testing.T) {
				fset := token.NewFileSet()
				file, err := parser.ParseFile(fset, target, nil, parser.ParseComments)
				if err != nil {
					t.Fatalf("parser.ParseFile(%s): %v", target, err)
				}
				var violations []string
				inspectedAny := false
				for _, decl := range file.Decls {
					fd, ok := decl.(*ast.FuncDecl)
					if !ok {
						continue
					}
					if fd.Body == nil {
						// interface method / 外部宣言 (本ファイルには無いはずだが念のため)
						continue
					}
					inspectedAny = true
					label := target + ":" + fd.Name.Name
					if rn := recvNameOf(fd); rn != "" {
						label = target + ":" + rn + "." + fd.Name.Name
					}
					violations = append(violations, inspectBody(fd.Body, fset, label)...)
				}
				if !inspectedAny {
					t.Fatalf("%s に FuncDecl が 1 つも見つからなかった (test 前提条件違反 / ファイル名 typo?)",
						target)
				}
				if len(violations) != 0 {
					t.Errorf("%s 内に provider 名分岐シンボルが現れている (Req 6.3 — provider 名で分岐したロジックを持たない):\n  %s",
						target, strings.Join(violations, "\n  "))
				}
			})
		}

		// (B) spec_loop.go の agentCallerAdapter.Call メソッドのみ
		//
		// spec_loop.go は本 spec のスコープ外の関数も多数含むため、ここでは tasks.md 4.7 spec 文の
		// 境界指定 (「`agentCallerAdapter.Call` メソッドのみ」) に従い、対象を 1 メソッドに限定する。
		// 姉妹 spec `kiro-spec-loop-facets` task 6.7 でも同形の限定走査を行っており、両 spec の
		// regression net は重複せず棲み分けされる (本サブテストは impl_loop 改修側の境界担保、
		// 姉妹 spec 側は spec_loop 改修側の境界担保)。
		t.Run("spec_loop.go:agentCallerAdapter.Call", func(t *testing.T) {
			const target = "spec_loop.go"
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, target, nil, parser.ParseComments)
			if err != nil {
				t.Fatalf("parser.ParseFile(%s): %v", target, err)
			}
			var body *ast.BlockStmt
			for _, decl := range file.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				if fd.Name == nil || fd.Name.Name != "Call" {
					continue
				}
				if recvNameOf(fd) != "agentCallerAdapter" {
					continue
				}
				body = fd.Body
				break
			}
			if body == nil {
				t.Fatalf("FuncDecl が見つからない: %s agentCallerAdapter.Call (本 spec で再利用する shared component を欠いている)",
					target)
			}
			violations := inspectBody(body, fset, target+":agentCallerAdapter.Call")
			if len(violations) != 0 {
				t.Errorf("%s agentCallerAdapter.Call 関数本体に provider 名分岐シンボルが現れている (Req 6.3 — provider 名で分岐したロジックを持たない):\n  %s",
					target, strings.Join(violations, "\n  "))
			}
		})
	})

}
