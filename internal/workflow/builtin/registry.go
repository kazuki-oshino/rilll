// Package builtin は rilll の組み込みワークフロー定義を集約し、
// CLI / TUI が一括で取得できる Registry を提供する。
//
// 個別のワークフロー実装はサブパッケージ (`builtin/git`, `builtin/kiro`,
// `builtin/improve`) に分かれており、本パッケージはそれらを束ねる薄い層。
// 契約 (Definition / AgentCaller 等) は親パッケージ `internal/workflow` に
// 残しているため、本パッケージは「契約 + 各実装」を import する側となる
// (循環参照回避のためのレイヤ分離)。
package builtin

import (
	"log"

	"github.com/kazuki-oshino/rilll/internal/config"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/git"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/improve"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/kiro"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/learnings"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/apply"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// configLoader は config.Load を test から差し替えるための seam。
// 既定値は production 経路で `~/.config/rilll/config.json` を読み込む `config.Load`。
// テストでは `configLoader = func()(...)` で stub に置き換える (registry_test.go の
// `withConfigLoader` ヘルパーが Cleanup 込みで使用)。
//
// agent-learnings spec の opt-in 経路 (Workflows[*].DistillLearningsAfter) を解決する
// ために本 seam を経由して 1 度だけ Load を呼ぶ。
var configLoader = config.Load

// registryWarnLog は不明な base ID に対する warning ログ出力 seam。
// 既定値は標準 `log.Printf`。テストでは captured slice に流し込む stub に置き換えて
// メッセージ内容を検証する (agent-learnings spec / 不明 base ID warning 検証)。
var registryWarnLog = func(format string, args ...any) {
	log.Printf(format, args...)
}

// Registry は agent に対して組み込みワークフロー Definition のリストを返す。
// CLI (`rilll list` / `rilll run`) と TUI の双方が本関数の戻り値を共有する。
//
// improve-workflow は precheck で「未知 workflow ID 拒否」検証 (Req 1.4) のために
// 自分自身を含む Registry の参照を必要とするが、improve → builtin の循環参照を避けるため、
// 構築済み defs の slice を closure 経由で遅延参照する形で渡す。
//
// agent-learnings spec の opt-in 経路:
//   - `applyLearningsTailWraps` を呼び、`Workflows[<id>].DistillLearningsAfter == true` の
//     base def を `learnings.SequentialWithLearningsTail` で wrap した Definition で
//     in-place 置換する (同じ ID / Args / Hidden を維持し、Label に ` 📚` marker を suffix)。
//
// orchestration ロジックは `internal/workflow/builtin/learnings/` 側に閉じ、本関数は
// Definition を slice に並べる薄い層に留める (design.md §Registry)。
func Registry(agent workflowctx.AgentCaller) []workflow.Definition {
	var defs []workflow.Definition
	registryFn := func() []workflow.Definition { return defs }
	defs = []workflow.Definition{
		git.UpdateReadme(agent),
		git.Commit(agent),
		kiro.KiroSpecLoop(agent),
		kiro.KiroSpecImplLoop(agent),
		kiro.KiroSpecImplLoopSequence(agent),
		kiro.KiroImplLoop(agent),
		git.UpdateReadmeAndCommit(agent),
		// workflow-self-improve spec / tasks.md 12.1 (Req 6.1):
		// improve-workflow を Registry に登録する。registryFn は自分自身を含む
		// 完成版 defs の遅延参照で、precheck の workflow ID 検証で利用される。
		improve.Workflow(agent, registryFn),
		// review-workflow spec / tasks.md 3.2 (Req 7.1):
		// review-workflow を builtin として登録する。Hidden=false のため
		// `rilll list` / TUI トップに既存ワークフローと同形式で表示される。
		review.Workflow(agent),
		// review-apply-workflow spec / tasks.md 7.1 (Req 8.1):
		// review-apply-workflow を builtin として登録する。Hidden=false のため
		// `rilll list` / TUI トップから `--review-run-id <id>` 単独起動が可能。
		apply.Workflow(agent),
		// review-apply-workflow spec / tasks.md 7.1 (Req 8.3):
		// review-and-apply Sequential 合成を builtin として登録する。
		// `--pr <num>` 1 つで前段 review-workflow → 後段 review-apply-workflow を
		// 一気通貫起動できる (Args は前段 review.Workflow.Args を継承)。
		apply.ReviewAndApply(agent),
		// agent-learnings spec (Req 10.1):
		// agent-learnings tail wrap は `Workflows[<id>].DistillLearningsAfter=true` で
		// opt-in する経路のみ提供する。agent-learnings 自身は Hidden=true のため Registry
		// に直接列挙しない (tail wrap 経由でのみ起動可能)。
	}
	defs = applyLearningsTailWraps(defs, agent)
	return defs
}

// applyLearningsTailWraps は config.Workflows の `DistillLearningsAfter == true` エントリを
// 評価し、対応する base def を `learnings.SequentialWithLearningsTail` で wrap した
// Definition で in-place 置換して返す (agent-learnings spec / Req 10.1)。
//
// 不変式:
//   - 同じ ID / Args / Hidden を維持する。Fn のみが「base → tail (agent-learnings)」の
//     Sequential 等価実装に差し替わる (Args は base.Args 継承)。
//   - Label は `base.Label + ` 📚“ (learnings.learningsTailLabelMarker) に変更する。
//   - 不明な base ID (= `Workflows` に書かれているが defs に存在しない ID) は warning のみ。
//
// 本関数は registry.go の薄さを保つため、orchestration ロジックは
// `internal/workflow/builtin/learnings/sequential.go` 側に閉じる。本関数は config 読み込み +
// 対象 ID 集約 + 委譲のみを担当する。
func applyLearningsTailWraps(defs []workflow.Definition, agent workflowctx.AgentCaller) []workflow.Definition {
	loaded, err := configLoader()
	if err != nil {
		registryWarnLog("agent-learnings: config 読み込みに失敗したため tail wrap を skip します: %v", err)
		return defs
	}
	if len(loaded.Config.Workflows) == 0 {
		return defs
	}

	// 既存 ID set (validation 用)。
	indexByID := make(map[string]struct{}, len(defs))
	for _, d := range defs {
		indexByID[d.ID] = struct{}{}
	}

	// (1) DistillLearningsAfter=true の base ID を集約。
	targetIDs := make(map[string]struct{})
	for id, wfCfg := range loaded.Config.Workflows {
		if !wfCfg.DistillLearningsAfter {
			continue
		}
		if _, ok := indexByID[id]; !ok {
			registryWarnLog("agent-learnings: Workflows[%q].DistillLearningsAfter=true ですが対応する base workflow が registry に存在しないため tail wrap を skip します", id)
			continue
		}
		targetIDs[id] = struct{}{}
	}

	// (2) learnings package 側の helper に委譲。
	return learnings.ApplyLearningsTailWraps(defs, agent, targetIDs)
}
