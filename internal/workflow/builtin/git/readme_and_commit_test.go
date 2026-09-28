package git

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
)

func TestUpdateReadmeAndCommitRunsReadmeThenGitCommit(t *testing.T) {
	agent := &fakeAgent{
		resp: provider.AgentResponse{Content: "ok", Success: true, SessionID: "session-1"},
	}
	var events []workflow.WorkflowEvent

	err := UpdateReadmeAndCommit(agent).Run(context.Background(), t.TempDir(), func(e workflow.WorkflowEvent) {
		events = append(events, e)
	}, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// UpdateReadme は3回 (plan / review / final-update)、Commit は1回 (/git-commit) で計4回呼ばれる
	if len(agent.prompts) != 4 {
		t.Fatalf("expected 4 agent calls (3 readme + 1 git-commit), got %d: %v", len(agent.prompts), agent.prompts)
	}
	if agent.prompts[3] != "/git-commit" {
		t.Fatalf("expected last prompt to be /git-commit, got %q", agent.prompts[3])
	}

	// Step メタデータが UpdateReadme の3ステップ + Commit の1ステップで順番通り
	wantStepIDs := []string{"plan", "review", "final-update", "git-commit"}
	for i, want := range wantStepIDs {
		if agent.options[i] == nil || agent.options[i].StepID != want {
			t.Fatalf("agent.options[%d].StepID = %v, want %q", i, agent.options[i], want)
		}
	}
}

// TestUpdateReadmeRecordsThreeArtifacts は akashic-artifacts spec Task 5.2 の
// 観測可能な完了条件を assert する: UpdateReadme ワークフローが
// 01-proposal.md / 02-review.md / 03-result.md の WriteFile 直後に
// workflow.RecordRunArtifact をそれぞれ proposal / review / result kind で
// 順番通りに呼び、recorder が 3 件の RunArtifact を `proposal → review → result`
// の順序で受信する (Req 2.2, 2.3, 2.4)。
//
// 既存の WriteFile に関する側面 (ファイル名・本文・呼び出し順) は本 spec で
// 変更しない (Req 2.7) ため、本テストは "登録呼び出しの side effect" と
// "順序" のみに焦点を絞る。
func TestUpdateReadmeRecordsThreeArtifacts(t *testing.T) {
	recorder := &workflowRecorder{}
	agent := &fakeAgent{
		resp:     provider.AgentResponse{Content: "ok", Success: true, SessionID: "session-1"},
		recorder: recorder,
	}
	cwd := t.TempDir()

	if err := UpdateReadme(agent).Run(context.Background(), cwd, func(workflow.WorkflowEvent) {}, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if len(recorder.runArtifacts) != 3 {
		t.Fatalf("expected 3 RunArtifacts recorded, got %d: %#v", len(recorder.runArtifacts), recorder.runArtifacts)
	}
	if len(recorder.starts) != 1 {
		t.Fatalf("expected workflow start recorded, got %#v", recorder.starts)
	}
	runID := recorder.starts[0].ID
	runDir := filepath.Join(cwd, ".rilll", "update-readme", runID)

	wantKinds := []string{
		akashic.ArtifactKindProposal,
		akashic.ArtifactKindReview,
		akashic.ArtifactKindResult,
	}
	wantFiles := []string{"01-proposal.md", "02-review.md", "03-result.md"}

	for i := range wantKinds {
		got := recorder.runArtifacts[i]
		if got.Kind != wantKinds[i] {
			t.Errorf("runArtifacts[%d].Kind = %q, want %q", i, got.Kind, wantKinds[i])
		}
		expected := filepath.Join(runDir, wantFiles[i])
		// NormalizeArtifactPath 経由で EvalSymlinks 済み絶対 clean path に揃えるため、
		// 比較側も同じ正規化を適用する (macOS の /private/var → /var 解消等への対策)。
		canonical, err := filepath.EvalSymlinks(expected)
		if err != nil {
			t.Fatalf("EvalSymlinks(%s): %v", expected, err)
		}
		if got.Path != filepath.Clean(canonical) {
			t.Errorf("runArtifacts[%d].Path = %q, want %q", i, got.Path, filepath.Clean(canonical))
		}
		if got.WorkflowRunID != runID {
			t.Errorf("runArtifacts[%d].WorkflowRunID = %q, want %q", i, got.WorkflowRunID, runID)
		}
	}
}

func TestUpdateReadmeAndCommitDefinitionMetadata(t *testing.T) {
	agent := &fakeAgent{resp: provider.AgentResponse{Content: "ok", Success: true, SessionID: "s"}}
	def := UpdateReadmeAndCommit(agent)
	if def.ID != "update-readme-and-commit" {
		t.Fatalf("unexpected ID: %q", def.ID)
	}
	if def.Label == "" {
		t.Fatalf("Label should not be empty")
	}
}
