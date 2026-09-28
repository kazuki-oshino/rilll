// Package builtin の本 file は agent-learnings の tail-wrap in-place 置換契約を
// integration テストする。registry が `Workflows[<id>].DistillLearningsAfter=true`
// を検出した際に、対応する base def が新 ID を生やさず in-place で
// `learnings.SequentialWithLearningsTail` 等価実装に差し替わることを担保する。
//
// 検証観点:
//
//   - DistillLearningsAfter=true で base def が in-place 置換され、`<base-id>` のまま
//     `📚` marker が Label に suffix される (Args / Hidden は base から継承)
//   - DistillLearningsAfter=true の base ID が registry に存在しない場合、
//     warning を 1 件 emit しつつ他 def に副作用を与えない
//
// 関連 Requirement: agent-learnings spec の registry 経路 (`applyLearningsTailWraps`)
// 不変式 (memory-distiller-removal spec で v1 関連 case は分離削除済)。
package builtin

import (
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/config"
	"github.com/kazuki-oshino/rilll/internal/workflow"
)

// TestRegistryInPlaceReplace_DistillLearningsAfter は
// 新経路 `DistillLearningsAfter=true` が in-place 置換を駆動することを検証する。
//
// 検証ポイント:
//   - 同じ ID `git-commit` がちょうど 1 件残る (`<base-id>-with-learnings` 別 ID は生成されない / in-place)
//   - Label に ` 📚` (learnings marker) が suffix される
//   - Args / Hidden は base から継承される
func TestRegistryInPlaceReplace_DistillLearningsAfter(t *testing.T) {
	// baseline (opt-in OFF) で base def をスナップショット。
	withConfigLoader(t, func() (config.LoadResult, error) {
		return config.LoadResult{Config: config.Config{}}, nil
	})
	baselineDefs := Registry(fakeAgent{})
	var origLabel string
	var origHidden bool
	for _, d := range baselineDefs {
		if d.ID == "git-commit" {
			origLabel = d.Label
			origHidden = d.Hidden
			break
		}
	}
	if origLabel == "" {
		t.Fatalf("baseline registry に git-commit が見つからない: ids=%v", definitionIDs(baselineDefs))
	}

	// opt-in ON で再取得し、in-place 置換が起きることを確認。
	withConfigLoader(t, func() (config.LoadResult, error) {
		return config.LoadResult{
			Config: config.Config{
				Workflows: map[string]config.WorkflowConfig{
					"git-commit": {DistillLearningsAfter: true},
				},
			},
		}, nil
	})
	defs := Registry(fakeAgent{})

	// 同じ ID `git-commit` がちょうど 1 件存在 (重複 / 別 ID 追加なし)。
	occurrences := 0
	var replaced *workflow.Definition
	for i := range defs {
		if defs[i].ID == "git-commit" {
			replaced = &defs[i]
			occurrences++
		}
	}
	if occurrences != 1 {
		t.Fatalf("git-commit ID の出現回数が 1 ではない: got=%d ids=%v", occurrences, definitionIDs(defs))
	}
	if replaced == nil || replaced.Fn == nil {
		t.Fatalf("git-commit Definition が in-place 置換されていない (Fn=nil)")
	}

	// `<id>-with-learnings` 別 ID は生成されない (in-place 置換契約)。
	for _, d := range defs {
		if d.ID == "git-commit-with-learnings" {
			t.Errorf("`-with-learnings` 別 ID が生成されている (in-place 置換契約違反): id=%q", d.ID)
		}
	}

	// Label は `<元 Label> + ` 📚`` 形式 (learnings marker)。
	wantLabel := origLabel + " 📚"
	if replaced.Label != wantLabel {
		t.Errorf("置換後 Label が期待と一致しない: got=%q want=%q", replaced.Label, wantLabel)
	}

	// Hidden は base から継承。
	if replaced.Hidden != origHidden {
		t.Errorf("置換後 Hidden が base から継承されていない: got=%v want=%v", replaced.Hidden, origHidden)
	}
}

// TestRegistryInPlaceReplace_UnknownBaseLogsWarning は `DistillLearningsAfter=true` の base ID が
// registry に存在しないとき warning が出ること & 他 def に副作用が無いことを検証する。
func TestRegistryInPlaceReplace_UnknownBaseLogsWarning(t *testing.T) {
	withConfigLoader(t, func() (config.LoadResult, error) {
		return config.LoadResult{
			Config: config.Config{
				Workflows: map[string]config.WorkflowConfig{
					"nonexistent-workflow": {DistillLearningsAfter: true},
				},
			},
		}, nil
	})
	captured := withRegistryWarnLog(t)

	defs := Registry(fakeAgent{})

	// 不明 base ID は何の def も追加しない。
	for _, d := range defs {
		if d.ID == "nonexistent-workflow" || d.ID == "nonexistent-workflow-with-learnings" {
			t.Errorf("不明な base ID に対して何らかの Definition が誤って追加されている: id=%q", d.ID)
		}
	}

	// warning メッセージに base ID 名を含むことを確認。
	var matched bool
	for _, msg := range *captured {
		if strings.Contains(msg, "nonexistent-workflow") {
			matched = true
			break
		}
	}
	if !matched {
		t.Errorf("warning ログに不明 base ID 名が含まれていない: msgs=%v", *captured)
	}
}
