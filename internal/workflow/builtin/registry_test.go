package builtin

import (
	"context"
	"fmt"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/config"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// fakeAgent は Registry 経由で組み立てられる Definition の存在を確認するための
// 最小スタブ。CallAgent / AkashicRecorder は呼ばれない前提なので空実装で十分。
type fakeAgent struct{}

func (fakeAgent) CallAgent(context.Context, string, string, provider.AgentEventCallback, string, *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
	return provider.AgentResponse{}, nil
}

func (fakeAgent) AkashicRecorder() akashic.Recorder { return nil }

// TestRegistryIncludesKiroSpecLoop は Registry(agent) に KiroSpecLoop が
// 登録されていることを検証する（要件 1.1 / tasks.md 5.1）。
//
// hermeticity: configLoader を空 cfg で固定し、ローカル `~/.config/rilll/config.json` の
// `Workflows.kiro-spec-loop.DistillAfter=true` 設定が leak して Label assertion を破壊する
// 経路を避ける (memory-distiller in-place 置換版適用後の必須前提)。
func TestRegistryIncludesKiroSpecLoop(t *testing.T) {
	withConfigLoader(t, func() (config.LoadResult, error) {
		return config.LoadResult{Config: config.Config{}}, nil
	})

	defs := Registry(fakeAgent{})

	var found *workflow.Definition
	for i := range defs {
		if defs[i].ID == "kiro-spec-loop" {
			found = &defs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("Registry に kiro-spec-loop が登録されていない: ids=%v", definitionIDs(defs))
	}
	if got, want := found.Label, "Kiro 仕様作成ループ"; got != want {
		t.Errorf("kiro-spec-loop の Label が期待と一致しない: got=%q want=%q", got, want)
	}
	if found.Fn == nil {
		t.Error("kiro-spec-loop の Fn が nil。Definition が組み立てられていない")
	}
}

// TestRegistryIncludesKiroSpecImplLoop は Registry(agent) に kiro-spec-impl-loop が
// 通常 builtin として登録されていることを検証する
// (kiro-spec-impl-loop Req 1.1 / 7.1, tasks.md 4.1)。
//
// 検証ポイント:
//   - ID `kiro-spec-impl-loop` が defs に含まれる
//   - Fn が組み立てられている (nil でない)
//   - Hidden=false (`rilll list` / TUI トップに表示される通常 builtin)
//   - `--feature` Arg が必須として登録されている (spec-loop と同じ起動 surface)
//   - 既存ワークフロー登録が失われていない (回帰防止)
func TestRegistryIncludesKiroSpecImplLoop(t *testing.T) {
	withConfigLoader(t, func() (config.LoadResult, error) {
		return config.LoadResult{Config: config.Config{}}, nil
	})

	defs := Registry(fakeAgent{})

	var found *workflow.Definition
	for i := range defs {
		if defs[i].ID == "kiro-spec-impl-loop" {
			found = &defs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("Registry に kiro-spec-impl-loop が登録されていない: ids=%v", definitionIDs(defs))
	}
	if found.Fn == nil {
		t.Error("kiro-spec-impl-loop の Fn が nil。Definition が組み立てられていない")
	}
	if found.Hidden {
		t.Errorf("kiro-spec-impl-loop は Hidden=false である必要がある (Req 1.1: rilll list / TUI トップに表示)")
	}

	if len(found.Args) != 1 {
		t.Fatalf("kiro-spec-impl-loop の Args=%v, want required feature arg 1 件", found.Args)
	}
	featureArg := found.Args[0]
	if featureArg.Key != "feature" {
		t.Errorf("kiro-spec-impl-loop feature Arg Key=%q, want %q", featureArg.Key, "feature")
	}
	if !featureArg.Required {
		t.Errorf("kiro-spec-impl-loop feature Arg Required=false, want true")
	}
	if featureArg.Description == "" {
		t.Errorf("kiro-spec-impl-loop feature Arg Description is empty")
	}

	wantIDs := []string{
		"update-readme",
		"git-commit",
		"kiro-spec-loop",
		"kiro-impl-loop",
		"update-readme-and-commit",
		"improve-workflow",
		"review-workflow",
		"review-apply-workflow",
		"review-and-apply",
	}
	gotIDs := definitionIDs(defs)
	for _, want := range wantIDs {
		if !containsID(gotIDs, want) {
			t.Errorf("既存ワークフロー %q が Registry から失われた: ids=%v", want, gotIDs)
		}
	}
}

// TestRegistryWrapsKiroSpecImplLoopWhenLearningsOptIn は
// kiro-spec-impl-loop が registry の config opt-in tail wrap 対象に含まれることを検証する。
func TestRegistryWrapsKiroSpecImplLoopWhenLearningsOptIn(t *testing.T) {
	withConfigLoader(t, func() (config.LoadResult, error) {
		return config.LoadResult{
			Config: config.Config{
				Workflows: map[string]config.WorkflowConfig{
					"kiro-spec-impl-loop": {DistillLearningsAfter: true},
				},
			},
		}, nil
	})

	defs := Registry(fakeAgent{})

	occurrences := 0
	var found *workflow.Definition
	for i := range defs {
		if defs[i].ID == "kiro-spec-impl-loop" {
			found = &defs[i]
			occurrences++
		}
	}
	if occurrences != 1 {
		t.Fatalf("kiro-spec-impl-loop ID の出現回数が 1 ではない: got=%d ids=%v", occurrences, definitionIDs(defs))
	}
	if found == nil || found.Fn == nil {
		t.Fatalf("kiro-spec-impl-loop Definition が in-place 置換されていない (Fn=nil)")
	}

	wantLabel := "Kiro 仕様作成・実装ループ 📚"
	if found.Label != wantLabel {
		t.Errorf("kiro-spec-impl-loop 置換後 Label=%q, want %q", found.Label, wantLabel)
	}
	if found.Hidden {
		t.Errorf("kiro-spec-impl-loop 置換後 Hidden=true, want false")
	}
	if len(found.Args) != 1 || found.Args[0].Key != "feature" || !found.Args[0].Required {
		t.Errorf("kiro-spec-impl-loop 置換後 Args=%v, want required feature arg", found.Args)
	}
}

// TestRegistryIncludesKiroSpecImplLoopSequence は sequence parent workflow が
// 通常 builtin として registry に公開されることを検証する
// (kiro-spec-impl-loop-sequence Req 6.1 / 6.6, tasks.md 3.1)。
//
// 検証ポイント:
//   - ID `kiro-spec-impl-loop-sequence` が defs に含まれる
//   - Fn が組み立てられている (nil でない)
//   - Hidden=false (`rilll list` / TUI トップに表示される通常 builtin)
//   - `--features` Arg が必須として登録されている
//   - 既存 Kiro workflow 登録が失われていない (回帰防止)
func TestRegistryIncludesKiroSpecImplLoopSequence(t *testing.T) {
	withConfigLoader(t, func() (config.LoadResult, error) {
		return config.LoadResult{Config: config.Config{}}, nil
	})

	defs := Registry(fakeAgent{})

	var found *workflow.Definition
	for i := range defs {
		if defs[i].ID == "kiro-spec-impl-loop-sequence" {
			found = &defs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("Registry に kiro-spec-impl-loop-sequence が登録されていない: ids=%v", definitionIDs(defs))
	}
	if found.Fn == nil {
		t.Error("kiro-spec-impl-loop-sequence の Fn が nil。Definition が組み立てられていない")
	}
	if found.Hidden {
		t.Errorf("kiro-spec-impl-loop-sequence は Hidden=false である必要がある (Req 6.1: rilll list / TUI トップに表示)")
	}

	if len(found.Args) != 1 {
		t.Fatalf("kiro-spec-impl-loop-sequence の Args=%v, want required features arg 1 件", found.Args)
	}
	featuresArg := found.Args[0]
	if featuresArg.Key != "features" {
		t.Errorf("kiro-spec-impl-loop-sequence features Arg Key=%q, want %q", featuresArg.Key, "features")
	}
	if !featuresArg.Required {
		t.Errorf("kiro-spec-impl-loop-sequence features Arg Required=false, want true")
	}
	if featuresArg.Description == "" {
		t.Errorf("kiro-spec-impl-loop-sequence features Arg Description is empty")
	}

	wantIDs := []string{
		"kiro-spec-loop",
		"kiro-spec-impl-loop",
		"kiro-impl-loop",
		"review-workflow",
		"review-apply-workflow",
		"review-and-apply",
	}
	gotIDs := definitionIDs(defs)
	for _, want := range wantIDs {
		if !containsID(gotIDs, want) {
			t.Errorf("既存ワークフロー %q が Registry から失われた: ids=%v", want, gotIDs)
		}
	}
}

// TestRegistryIncludesImproveWorkflow は Registry(agent) に ImproveWorkflow が
// 登録されていることを検証する（workflow-self-improve Req 6.1 / tasks.md 12.1）。
func TestRegistryIncludesImproveWorkflow(t *testing.T) {
	defs := Registry(fakeAgent{})

	var found *workflow.Definition
	for i := range defs {
		if defs[i].ID == "improve-workflow" {
			found = &defs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("Registry に improve-workflow が登録されていない: ids=%v", definitionIDs(defs))
	}
	if found.Fn == nil {
		t.Error("improve-workflow の Fn が nil。Definition が組み立てられていない")
	}

	// 既存ワークフローを変更していないこと (回帰防止)
	wantIDs := []string{
		"update-readme",
		"git-commit",
		"kiro-impl-loop",
		"update-readme-and-commit",
	}
	gotIDs := definitionIDs(defs)
	for _, want := range wantIDs {
		if !containsID(gotIDs, want) {
			t.Errorf("既存ワークフロー %q が Registry から失われた: ids=%v", want, gotIDs)
		}
	}
}

// TestRegistryIncludesReviewWorkflow は Registry(agent) に review-workflow が
// 登録されていることを検証する (review-workflow Req 7.1 / tasks.md 3.2)。
//
// 検証ポイント:
//   - ID `review-workflow` が defs に含まれる
//   - Fn が組み立てられている (nil でない)
//   - Hidden=false (`rilll list` / TUI トップに既存ワークフローと同形式で表示される)
//   - 既存ワークフロー登録が失われていない (回帰防止)
func TestRegistryIncludesReviewWorkflow(t *testing.T) {
	defs := Registry(fakeAgent{})

	var found *workflow.Definition
	for i := range defs {
		if defs[i].ID == "review-workflow" {
			found = &defs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("Registry に review-workflow が登録されていない: ids=%v", definitionIDs(defs))
	}
	if found.Fn == nil {
		t.Error("review-workflow の Fn が nil。Definition が組み立てられていない")
	}
	if found.Hidden {
		t.Errorf("review-workflow は Hidden=false である必要がある (Req 7.1: rilll list / TUI トップに表示)")
	}

	// 既存ワークフローが失われていないこと (回帰防止)
	wantIDs := []string{
		"update-readme",
		"git-commit",
		"kiro-spec-loop",
		"kiro-impl-loop",
		"update-readme-and-commit",
		"improve-workflow",
	}
	gotIDs := definitionIDs(defs)
	for _, want := range wantIDs {
		if !containsID(gotIDs, want) {
			t.Errorf("既存ワークフロー %q が Registry から失われた: ids=%v", want, gotIDs)
		}
	}
}

// TestRegistryIncludesReviewApplyWorkflow は Registry(agent) に review-apply-workflow が
// 登録されていることを検証する (review-apply-workflow Req 8.1 / tasks.md 7.1)。
//
// 検証ポイント:
//   - ID `review-apply-workflow` が defs に含まれる
//   - Fn が組み立てられている (nil でない)
//   - Hidden=false (`rilll list` / TUI トップに既存ワークフローと同形式で表示される / Req 8.1)
//   - --review-run-id Arg が登録されている (Req 8.2 / 5.1 で Required:false に確定)
//   - 既存ワークフロー登録が失われていない (回帰防止)
func TestRegistryIncludesReviewApplyWorkflow(t *testing.T) {
	defs := Registry(fakeAgent{})

	var found *workflow.Definition
	for i := range defs {
		if defs[i].ID == "review-apply-workflow" {
			found = &defs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("Registry に review-apply-workflow が登録されていない: ids=%v", definitionIDs(defs))
	}
	if found.Fn == nil {
		t.Error("review-apply-workflow の Fn が nil。Definition が組み立てられていない")
	}
	if found.Hidden {
		t.Errorf("review-apply-workflow は Hidden=false である必要がある (Req 8.1: rilll list / TUI トップに表示)")
	}

	// --review-run-id Arg が登録されていることを確認 (Req 8.2)
	var hasReviewRunID bool
	for _, arg := range found.Args {
		if arg.Key == "review-run-id" {
			hasReviewRunID = true
			break
		}
	}
	if !hasReviewRunID {
		t.Errorf("review-apply-workflow に --review-run-id Arg が登録されていない: args=%v", found.Args)
	}

	// 既存ワークフローが失われていないこと (回帰防止)
	wantIDs := []string{
		"update-readme",
		"git-commit",
		"kiro-spec-loop",
		"kiro-impl-loop",
		"update-readme-and-commit",
		"improve-workflow",
		"review-workflow",
	}
	gotIDs := definitionIDs(defs)
	for _, want := range wantIDs {
		if !containsID(gotIDs, want) {
			t.Errorf("既存ワークフロー %q が Registry から失われた: ids=%v", want, gotIDs)
		}
	}
}

// TestRegistryIncludesReviewAndApply は Registry(agent) に review-and-apply Sequential 合成が
// 登録されていることを検証する (review-apply-workflow Req 8.3 / tasks.md 7.1)。
//
// 検証ポイント:
//   - ID `review-and-apply` が defs に含まれる
//   - Fn が組み立てられている (nil でない)
//   - Hidden=false (`rilll list` / TUI トップに表示)
//   - --pr Arg が前段 review.Workflow.Args 経由で継承されている (Req 8.3)
//   - 既存ワークフロー登録が失われていない (回帰防止)
func TestRegistryIncludesReviewAndApply(t *testing.T) {
	defs := Registry(fakeAgent{})

	var found *workflow.Definition
	for i := range defs {
		if defs[i].ID == "review-and-apply" {
			found = &defs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("Registry に review-and-apply が登録されていない: ids=%v", definitionIDs(defs))
	}
	if found.Fn == nil {
		t.Error("review-and-apply の Fn が nil。Definition が組み立てられていない")
	}
	if found.Hidden {
		t.Errorf("review-and-apply は Hidden=false である必要がある (Req 8.3: rilll list / TUI トップに表示)")
	}

	// --pr Arg を継承していることを確認 (Req 8.3 / sequential.go の Args=deps.reviewDef.Args 契約)
	var hasPRArg bool
	for _, arg := range found.Args {
		if arg.Key == "pr" {
			hasPRArg = true
			break
		}
	}
	if !hasPRArg {
		t.Errorf("review-and-apply に --pr Arg が継承されていない (review.Workflow.Args 直接コピー契約): args=%v", found.Args)
	}

	// 既存ワークフローが失われていないこと (回帰防止)
	wantIDs := []string{
		"update-readme",
		"git-commit",
		"kiro-spec-loop",
		"kiro-impl-loop",
		"update-readme-and-commit",
		"improve-workflow",
		"review-workflow",
	}
	gotIDs := definitionIDs(defs)
	for _, want := range wantIDs {
		if !containsID(gotIDs, want) {
			t.Errorf("既存ワークフロー %q が Registry から失われた: ids=%v", want, gotIDs)
		}
	}
}

// TestRegistryHidesUpdateReadme は update-readme ワークフローが Hidden=true として
// 登録されていることを検証する。
// 動機: 複合ワークフロー update-readme-and-commit が主要な起動経路となったため、
// トップメニュー / `rilll list` / `rilll help` での単独露出は廃止する。
// Registry には残しているので、Sequential 経由の利用と `rilll run update-readme`
// 名指し起動 (Hidden は findDefinition を弾かない) は引き続き機能する。
func TestRegistryHidesUpdateReadme(t *testing.T) {
	defs := Registry(fakeAgent{})

	var found *workflow.Definition
	for i := range defs {
		if defs[i].ID == "update-readme" {
			found = &defs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("Registry から update-readme が消えている: ids=%v", definitionIDs(defs))
	}
	if !found.Hidden {
		t.Errorf("update-readme は Hidden=true である必要がある (トップメニューから除外する設計)")
	}
}

// withConfigLoader は configLoader test seam を一時的に override するヘルパー。
// 各テスト終了時に t.Cleanup で確実に元の関数 (= config.Load) に戻す。
func withConfigLoader(t *testing.T, fn func() (config.LoadResult, error)) {
	t.Helper()
	prev := configLoader
	configLoader = fn
	t.Cleanup(func() { configLoader = prev })
}

// withRegistryWarnLog は registryWarnLog test seam を一時的に override し、
// 受信した warning メッセージを slice として収集して返すヘルパー。
// 戻り値の slice 参照は test 終了時まで有効。
func withRegistryWarnLog(t *testing.T) *[]string {
	t.Helper()
	prev := registryWarnLog
	captured := []string{}
	registryWarnLog = func(format string, args ...any) {
		captured = append(captured, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { registryWarnLog = prev })
	return &captured
}

func definitionIDs(defs []workflow.Definition) []string {
	ids := make([]string, 0, len(defs))
	for _, d := range defs {
		ids = append(ids, d.ID)
	}
	return ids
}

func containsID(ids []string, target string) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}
