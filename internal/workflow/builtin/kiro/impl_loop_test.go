package kiro

import (
	"context"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"os"
	"path/filepath"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
)

// TestKiroImplLoopAbortsWhenNoEligibleFeature は、listImplementableFeatures が
// 候補 0 件を返したときに kiro_impl_loop が:
//   - feature 選択ダイアログ (ask.Select) を呼ばない
//   - workflow.WorkflowError を発火し、ユーザーに「実施可能な feature が無い」旨を伝える
//
// ことを assert する。新フィルタ導入の挙動契約を保護するためのテスト。
func TestKiroImplLoopAbortsWhenNoEligibleFeature(t *testing.T) {
	recorder := &workflowRecorder{}
	agent := &fakeAgent{
		resp:     provider.AgentResponse{Content: "ok", Success: true},
		recorder: recorder,
	}
	cwd := t.TempDir()

	// tasks.md は全 [x]（= 未チェック項目なし）にして、フィルタ条件を意図的に満たさない feature を 1 つ置く。
	// spec.json は承認済み形式にしておき、「フィルタ NG の主因が tasks.md である」状況を作る。
	feature := "all-done-feature"
	specDir := filepath.Join(cwd, ".kiro", "specs", feature)
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", specDir, err)
	}
	specJSON := `{
  "ready_for_implementation": true,
  "approvals": {
    "requirements": {"approved": true},
    "design": {"approved": true},
    "tasks": {"approved": true}
  }
}`
	if err := os.WriteFile(filepath.Join(specDir, "spec.json"), []byte(specJSON), 0o644); err != nil {
		t.Fatalf("WriteFile spec.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(specDir, "tasks.md"), []byte("- [x] 1. 完了済み\n"), 0o644); err != nil {
		t.Fatalf("WriteFile tasks.md: %v", err)
	}

	ask := &fakeAsk{Selected: feature}
	onEvent, eventsPtr := collectEvents()
	if err := KiroImplLoop(agent).Run(context.Background(), cwd, onEvent, ask); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// (1) feature 選択ダイアログは呼び出されていない。
	if ask.LastPrompt != "" || ask.LastItems != nil {
		t.Fatalf("ask.Select was unexpectedly called: prompt=%q items=%v", ask.LastPrompt, ask.LastItems)
	}

	// (2) workflow.WorkflowError が発火し、フィルタ要件を伝える文言を含む。
	if !hasErrorEvent(*eventsPtr, "実施可能な feature が見つかりません") {
		t.Fatalf("expected workflow.WorkflowError with eligibility message, got events=%#v", *eventsPtr)
	}

	// (3) 候補 0 件で abort したので 00-context.json は登録されない。
	if len(recorder.runArtifacts) != 0 {
		t.Fatalf("expected no RunArtifact recorded on abort, got %d: %#v", len(recorder.runArtifacts), recorder.runArtifacts)
	}
}

// TestKiroImplLoopRecordsContextArtifact は akashic-artifacts spec Task 5.3 の
// 観測可能な完了条件を assert する: kiro_impl_loop ワークフローが
// 00-context.json の WriteFile 直後に workflow.RecordRunArtifact を
// context kind で 1 度呼び、recorder が 1 件の RunArtifact を受信する (Req 2.5)。
//
// 既存の WriteFile 側面 (ファイル名・タイミング・呼び出し順) は本 spec で
// 変更しない (Req 2.7) ため、本テストは "登録呼び出しの side effect" のみに
// 焦点を絞る。pendingTasks を空にしてタスクループをスキップさせ、final review
// (`/kiro-validate-impl` 1 回) のみで完走させることでテストを安定化させる。
func TestKiroImplLoopRecordsContextArtifact(t *testing.T) {
	recorder := &workflowRecorder{}
	agent := &fakeAgent{
		resp:     provider.AgentResponse{Content: "ok", Success: true},
		recorder: recorder,
	}
	cwd := t.TempDir()

	// kiro_impl_loop が読み取る spec.json / tasks.md を準備する。
	// 新フィルタ（listImplementableFeatures）は「未チェック項目あり + spec.json 承認済み」の
	// feature だけを候補化するため、tasks.md は未チェック 1 件を含めておく必要がある。
	// fakeAgent は impl / commit / final-review のいずれにも Success:true を返すので、
	// impl ステップを 1 回経由しても 00-context.json は変わらず 1 件しか登録されない。
	feature := "demo-feature"
	specDir := filepath.Join(cwd, ".kiro", "specs", feature)
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", specDir, err)
	}
	specJSON := `{
  "ready_for_implementation": true,
  "approvals": {
    "requirements": {"approved": true},
    "design": {"approved": true},
    "tasks": {"approved": true}
  }
}`
	if err := os.WriteFile(filepath.Join(specDir, "spec.json"), []byte(specJSON), 0o644); err != nil {
		t.Fatalf("WriteFile spec.json: %v", err)
	}
	tasksMD := "- [ ] 1. ダミータスク\n"
	if err := os.WriteFile(filepath.Join(specDir, "tasks.md"), []byte(tasksMD), 0o644); err != nil {
		t.Fatalf("WriteFile tasks.md: %v", err)
	}

	// fakeAsk は kiro_spec_loop_run_test.go で定義済みのものを再利用する。
	// Selected に feature を設定すれば、Cancelled=false / Err=nil の通常経路で
	// feature を返す挙動になる。
	ask := &fakeAsk{Selected: feature}
	if err := KiroImplLoop(agent).Run(context.Background(), cwd, func(workflow.WorkflowEvent) {}, ask); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(recorder.runArtifacts) != 1 {
		t.Fatalf("expected 1 RunArtifact recorded, got %d: %#v", len(recorder.runArtifacts), recorder.runArtifacts)
	}
	got := recorder.runArtifacts[0]
	if got.Kind != akashic.ArtifactKindContext {
		t.Errorf("RunArtifact.Kind = %q, want %q", got.Kind, akashic.ArtifactKindContext)
	}
	if len(recorder.starts) != 1 {
		t.Fatalf("expected workflow start recorded, got %#v", recorder.starts)
	}
	runID := recorder.starts[0].ID
	expectedPath := filepath.Join(cwd, ".rilll", "kiro-impl-loop", runID, "00-context.json")
	// NormalizeArtifactPath 経由で EvalSymlinks 済み絶対 clean path に揃えるため、
	// 比較側も同じ正規化を適用する (macOS の /private/var → /var 解消等への対策)。
	canonical, err := filepath.EvalSymlinks(expectedPath)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", expectedPath, err)
	}
	if got.Path != filepath.Clean(canonical) {
		t.Errorf("RunArtifact.Path = %q, want %q", got.Path, filepath.Clean(canonical))
	}
	if got.WorkflowRunID != runID {
		t.Errorf("RunArtifact.WorkflowRunID = %q, want %q", got.WorkflowRunID, runID)
	}
}
