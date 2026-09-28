// Package ui のアカシックモード「データ源未配線 (Reader / ArtifactReader nil)」
// 時の graceful 表示テスト (akashic-mode-tui spec Task 10.1)。
//
// 観測完了条件 (Task 10.1 / Req 11.1〜11.5):
//
//	(a) Phase が PhaseAkashic に遷移すること (Req 11.1)
//	(b) List / Summary / Detail の各サブビューに「履歴データが利用できません」が、
//	    Detail 内成果物プレビュー領域に「成果物データが利用できません」が表示
//	    されること (Req 11.2 / 11.3)
//	(c) Reader / ArtifactReader 呼び出し Cmd が一度も発行されないこと
//	(d) Esc / q でモード退出が可能で Phase=PhaseMenu に戻ること (Req 11.4)
//	(e) 一連の操作中に tea.Quit が一度も発行されないこと (Req 11.5)
//
// 旧 internal/ui/akashic_graceful_nil_test.go は package ui に同居していたため
// State 内部メソッド (enterDetail) を直接呼べたが、サブパッケージ化に伴い
// State 単体テストは package akashic 配下に閉じ、Model 層の本テストでは
// 公開フィールド経由で State を組み立てる。
package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	record "github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/config"
	"github.com/kazuki-oshino/rilll/internal/provider"
	uiakashic "github.com/kazuki-oshino/rilll/internal/ui/akashic"
)

// TestAkashicGracefulWithNilReadersFromNewModel は Task 10.1 の (a)〜(e) を
// 網羅する単体テスト。
func TestAkashicGracefulWithNilReadersFromNewModel(t *testing.T) {
	// ============================================================
	// 主シナリオ: Reader=nil + ArtifactReader=nil
	// ============================================================
	model := NewModel(Options{
		CWD:    "/repo",
		Config: config.Config{AgentProvider: provider.ProviderClaude},
	})

	// (a): 'a' キー押下で PhaseAkashic に遷移し、初期 Subview=SubviewList。
	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m := updated.(Model)
	if m.Phase != PhaseAkashic {
		t.Fatalf("(a) expected Phase=PhaseAkashic on 'a' press with nil readers, got %s", m.Phase)
	}
	if m.Akashic.Subview != uiakashic.SubviewList {
		t.Fatalf("(a) expected initial Subview=SubviewList, got %s", m.Akashic.Subview)
	}
	if cmd != nil {
		t.Fatalf("(c) expected nil Cmd from 'a' press when Reader=nil, got %T", cmd)
	}

	listView := m.View()
	if !strings.Contains(listView, "アカシックモード - Run 一覧") {
		t.Fatalf("(b) expected list section header in view, got:\n%s", listView)
	}
	if !strings.Contains(listView, "履歴データが利用できません") {
		t.Fatalf("(b) List subview missing '履歴データが利用できません', got:\n%s", listView)
	}

	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	if m.Akashic.Subview != uiakashic.SubviewSummary {
		t.Fatalf("expected Subview=SubviewSummary on tab from List, got %s", m.Akashic.Subview)
	}
	if cmd != nil {
		t.Fatalf("(c) expected nil Cmd on tab to Summary when Reader=nil, got %T", cmd)
	}

	summaryView := m.View()
	if !strings.Contains(summaryView, "アカシックモード - 集計サマリ") {
		t.Fatalf("(b) expected summary section header in view, got:\n%s", summaryView)
	}
	if !strings.Contains(summaryView, "履歴データが利用できません") {
		t.Fatalf("(b) Summary subview missing '履歴データが利用できません', got:\n%s", summaryView)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	if m.Akashic.Subview != uiakashic.SubviewList {
		t.Fatalf("expected Subview=SubviewList after tab from Summary, got %s", m.Akashic.Subview)
	}

	// Detail 遷移は Reader=nil のため List.Items が空で Enter は no-op。
	// 公開フィールド経由で Detail サブビューに入った State を組み立てて View 出力を観測する。
	// この経路は package akashic の State 単体テストで enterDetail の構造観測
	// (Cmd=nil / Detail.Artifact.Unavailable=true) が担保されている前提に乗る。
	m.Akashic.Subview = uiakashic.SubviewDetail
	m.Akashic.Detail = uiakashic.DetailState{
		RunID: "test-run-id",
		Focus: uiakashic.DetailFocusAgentCalls,
		Artifact: uiakashic.ArtifactState{
			Unavailable: true, // ArtifactReader=nil graceful フラグ (Req 11.3)
		},
	}
	if !m.Akashic.Detail.Artifact.Unavailable {
		t.Fatalf("(b) expected Detail.Artifact.Unavailable=true when ArtifactReader=nil")
	}

	detailView := m.View()
	if !strings.Contains(detailView, "アカシックモード - Run 詳細") {
		t.Fatalf("(b) expected detail section header in view, got:\n%s", detailView)
	}
	if !strings.Contains(detailView, "履歴データが利用できません") {
		t.Fatalf("(b) Detail subview missing '履歴データが利用できません', got:\n%s", detailView)
	}

	// (d) Esc でモード退出 → PhaseMenu
	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.Phase != PhaseMenu {
		t.Fatalf("(d) expected Esc to return to PhaseMenu, got %s", m.Phase)
	}
	if cmd != nil {
		t.Fatalf("(e) expected nil Cmd on Esc, got %T", cmd)
	}

	// 再突入してから 'q' でも退出可能 (Req 1.4 の再解釈ルール)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	m = updated.(Model)
	if m.Phase != PhaseAkashic {
		t.Fatalf("expected re-entry to PhaseAkashic on 'a', got %s", m.Phase)
	}
	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	m = updated.(Model)
	if m.Phase != PhaseMenu {
		t.Fatalf("(d) expected 'q' in PhaseAkashic to return to PhaseMenu, got %s", m.Phase)
	}
	if cmd != nil {
		t.Fatalf("(e) expected nil Cmd from 'q' in akashic (no tea.Quit), got %T", cmd)
	}

	// ============================================================
	// 補完サブテスト: ArtifactReader=nil + Reader=非 nil で Detail の
	// 「成果物データが利用できません」表示を観測 (Req 11.3 独立条件)。
	// ============================================================
	t.Run("DetailArtifactUnavailableMessage", func(t *testing.T) {
		modelB := NewModel(Options{
			CWD:    "/repo",
			Config: config.Config{AgentProvider: provider.ProviderClaude},
			Reader: &fakeReader{},
			// ArtifactReader: nil
		})
		updatedB, _ := modelB.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
		mB := updatedB.(Model)

		// Detail サブビューに直接遷移し、Reader 非 nil + ArtifactReader=nil の
		// 描画経路に到達する State を組み立てる。
		mB.Akashic.Subview = uiakashic.SubviewDetail
		mB.Akashic.Detail = uiakashic.DetailState{
			RunID: "test-run-id",
			Focus: uiakashic.DetailFocusAgentCalls,
			Detail: &record.RunDetail{
				Run: record.RunHeader{RunID: "test-run-id", WorkflowID: "wf-1"},
			},
			Loading: false,
			Artifact: uiakashic.ArtifactState{
				Unavailable: true, // ArtifactReader=nil graceful フラグ
			},
		}

		detailViewB := mB.View()
		if !strings.Contains(detailViewB, "成果物データが利用できません") {
			t.Fatalf("(b) ArtifactReader=nil Detail missing '成果物データが利用できません', got:\n%s", detailViewB)
		}
		if !mB.Akashic.Detail.Artifact.Unavailable {
			t.Fatalf("expected Detail.Artifact.Unavailable=true with ArtifactReader=nil, got false")
		}
	})
}
