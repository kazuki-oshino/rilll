// Package apply の本ファイルは review-apply-workflow の workflow.Definition skeleton
// (task 5.1 / design.md §Workflow / Req 1.2, 2.5, 7.3, 7.4, 8.1, 8.2) と
// 5.2 の precheck → loader → no_findings fast-path 統合
// (design.md §Single-Run §precheck §Loader §findings 0 件 / Req 1.1, 1.3, 1.5, 1.6,
// 2.1, 2.2, 2.3, 2.5) を提供する。
//
// 本ファイルの責務 (5.1 + 5.2 scope):
//
//  1. Definition の構築 (`Workflow(agent)` から ID `review-apply-workflow` の
//     workflow.Definition を返す)。Args は `{Key:"review-run-id", Required:false}` のみ
//     登録する。Sequential 経路 (`review-and-apply`) では ctx 注入で reviewRunID が
//     満たされるため Required にしない (design.md §Workflow Service Interface)。
//
//  2. reviewRunID 二段階解決:
//     - 第一優先: `ReviewRunIDFromContext(ctx)` (Sequential 経路で前段 review.Workflow から
//     注入される hand-off 値)。
//     - 第二優先: `--review-run-id` arg (CLIAsk.Values["review-run-id"] を直読み)。
//     - 双方空のとき `VerdictPreconditionFailed` で副作用ゼロで終端する (Req 1.3, 8.2)。
//
//  3. result.json defer 不変式:
//     - `defer` ブロックで `WriteResultJSON` を必ず呼び出し、いかなる早期 return 経路
//     (precheck 失敗 / panic recovery 含む) でも `<runDir>/result.json` への書き出しが
//     漏れない (Req 7.3 / design.md §Workflow §defer 不変式)。
//     - panic 経路では `recover()` で捕捉し、未設定 verdict を `VerdictFailed` に **fallback**
//     してから result.json を書き出す (Req 7.4 / design.md §Verdict Invariants:
//     VerdictUnset を最終 verdict として絶対に書き出さない)。
//     - panic は再 raise せず、`errors.New(ErrorPrefixForVerdict(...))` を Fn 戻り値に
//     設定して呼び出し元に返す (improve-workflow と同形の安定終端)。
//
//  4. 5.2 で追加: precheck → loader → no_findings fast-path:
//     - reviewRunID 解決後に Precheck (clean tree + reviewRunID 安全性 + dir 存在) を実行。
//     失敗時は `VerdictPreconditionFailed` で副作用ゼロ終端 (Req 1.3, 2.1, 2.2, 2.5)。
//     - precheck 通過後に Loader.Load を実行し、TargetRules / SkippedSummary を集約する。
//     スキーマ違反は `VerdictPreconditionFailed` に集約 (Req 1.4 / Loader と Workflow の合流)。
//     - SkippedSummary を `WorkflowText` として表示し、`record.SkippedRuleSummary`
//     および `record.TargetRuleNames` を ResultRecord に転記する (Req 1.1, 1.5)。
//     - findings 0 件 (TargetRules が空) で `VerdictNoFindings` (intent) を最終 verdict
//     として確定し副作用ゼロで return する (Req 1.6)。
//     - findings >= 1 件は task 5.3+ で propose loop に進むため、現状は skeleton fallback
//     (`VerdictFailed`) で友好的に終端する (5.3 で置換予定)。
//
// 5.3+ task との接合面 (`coreFn` seam):
//   - 本ファイルは `applyWorkflowDeps.coreFn` seam を provide し、5.3+ で propose loop /
//     apply 実装を順次差し込めるように備える。default deps では 5.2 fast-path
//     (`newPrecheckLoaderFastPathCoreFn`) を注入する。
//   - テストは custom coreFn を注入して reviewRunID 解決経路 / panic 経路 / 即時 cancel
//     経路 / verdict 未設定 fallback / 5.2 fast-path 各経路を検証する。
//
// 副作用契約 (Req 2.3):
//   - precheck 段は GitOps.Status のみを呼び、Snapshot/ApplyCheck/Apply/Revert は呼ばない。
//   - reviewRunID 解決失敗 (双方空) / precheck 失敗 / no_findings fast-path 経路では
//     LLM / git apply / commit のいずれも呼ばれない。
//   - `os.MkdirAll` のみが副作用候補で、result.json 書き出しのために defer から呼ばれる。
package apply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kazuki-oshino/rilll/internal/workflow"
	improvepatch "github.com/kazuki-oshino/rilll/internal/workflow/builtin/improve/patch"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin/review/result"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// applyWorkflowID は本 workflow.Definition の workflow ID (kebab-case)。
// 既存 builtin ワークフローと同形式で命名し、akashic レコードでも
// `workflow_runs.workflow_id` にそのまま格納される (Req 8.1)。
const applyWorkflowID = "review-apply-workflow"

// applyWorkflowLabel は TUI 上で表示される人間可読ラベル。
// 既存ワークフロー (`レビュー実行` / `improve workflow`) と並ぶ表示行で利用される。
const applyWorkflowLabel = "レビュー結果適用"

// argReviewRunID は `--review-run-id` arg の semantic key。
// design.md §Workflow Service Interface の `ArgSpec{Key:"review-run-id"}` と一致させる。
// CLIAsk.Values から直接読み取る経路で本キーを参照する (Req 8.2)。
const argReviewRunID = "review-run-id"

// loadFunc は Loader.Load の関数型エイリアスで、テスト経路で fake を注入するための seam。
// 既定では `Load` (本パッケージ実装) が `defaultApplyWorkflowDeps` 経由で割り当てられる。
type loadFunc func(ctx context.Context, cwd, reviewRunID string) (LoadResult, error)

// applyWorkflowDeps は runApplyWorkflowOrchestration の internal な DI 構造体。
//
// 5.1 skeleton では `coreFn` seam のみを保持していたが、5.2 で precheck → loader 統合に
// 必要な GitOps / load 関数の seam を追加した。5.3+ task でさらに以下を追加していく:
//   - prompt / parse / patch deps
//   - GitOpsExt deps (Add / Commit / Snapshot / Apply / Revert)
//
// `coreFn` の戻り値はそのまま Fn の戻り値として伝播するが、最終的な result.json 書き出しは
// 必ず defer で実行される (Req 7.3)。`coreFn` 内部での panic も outer の defer + recover
// で捕捉され、`VerdictFailed` に fallback される (Req 7.4)。
type applyWorkflowDeps struct {
	// gitOps は precheck (および 5.4+ の Snapshot/ApplyCheck/Apply/Revert) で利用される
	// improvepatch.GitOps interface。default では `improvepatch.NewGitOps()` (実 git CLI 経由)
	// が割り当てられる。テスト経路では fakePrecheckGitOps 等を注入して副作用契約を機械検証する。
	//
	// 注: テストが custom coreFn を直接注入する場合 (`applyWorkflowDeps{coreFn: ...}`)、
	// gitOps は default coreFn からのみ参照されるため未設定でも動作する (no-op)。
	gitOps improvepatch.GitOps

	// load は Loader.Load の seam。default では `Load` (本パッケージ実装) が割り当てられる。
	// テスト経路で fake を注入し、Loader が返す各種 error 経路 (LoaderSchemaError 等) を
	// orchestration 側でどう verdict にマップするかを単独検証する用途。
	load loadFunc

	// agent は propose loop (task 5.3) 以降の LLM 呼び出し seam。
	// default では Workflow(agent) の引数をそのまま保持し、plan / diff / self-review の
	// agent.CallAgent 呼び出しに利用する。
	agent workflowctx.AgentCaller

	// applyHook は self-review accept 後に apply phase へ制御を渡す seam。
	// task 5.3 では defaultProposeApplyHook が task 5.4 deferred として VerdictFailed を返す。
	applyHook ProposeApplyHook

	// coreFn は reviewRunID 解決後に実行される「実作業」の seam。
	//
	// パラメータ:
	//   - ctx: 親 ctx (recorder / RunMetadata 注入済み)。
	//   - cwd: workflow 実行の作業ディレクトリ。
	//   - applyRunID: 本 apply run の workflow_run_id (`<cwd>/.rilll/review-apply-workflow/<id>`)。
	//   - runDir: applyRunID 配下の絶対パス (MkdirAll 済み)。
	//   - reviewRunID: 解決済みの前段 review run id (ctx 第一優先 / arg 第二優先で確定)。
	//   - onEvent: WorkflowEvent emit 用 callback (nil-safe wrapper 適用済み)。
	//   - record: ResultRecord 集約 state ポインタ。coreFn は finalVerdict / finalSummary /
	//     その他のフィールド (Cycles / CommitSHA / RecoveryGuidance 等) を必要に応じて埋める。
	//
	// 戻り値:
	//   - intent verdict (`applied` / `no_findings` / `user_aborted`) では nil。
	//   - 内部失敗系 verdict では `errors.New(ErrorPrefixForVerdict(v, summary))`。
	//   - panic は呼び出し元の defer + recover で `VerdictFailed` に fallback される。
	//   - nil でも record.FinalVerdict が VerdictUnset のままだった場合、defer が
	//     `VerdictFailed` に fallback して result.json を書き出す (Req 7.4)。
	//
	// 5.1 skeleton では coreFn=nil で `VerdictFailed` 友好的終端だったが、5.2 以降は
	// `defaultApplyWorkflowDeps()` が `newPrecheckLoaderFastPathCoreFn(...)` を注入し、
	// precheck → loader → no_findings fast-path を実行する (5.3+ は順次差し替え)。
	coreFn func(
		ctx context.Context,
		cwd string,
		applyRunID string,
		runDir string,
		reviewRunID string,
		onEvent func(workflow.WorkflowEvent),
		record *ResultRecord,
	) error
}

// defaultApplyWorkflowDeps は production 経路で利用する deps 既定値を構築する。
// 5.4 で apply phase (Snapshot → ApplyCheck → Apply → commit-message → Add → Commit) を
// `newApplyHook(gitOpsExt, agent)` で構築し、`defaultProposeApplyHook` (5.3 期の deferred
// 専用 placeholder) を production wiring から外す。`gitOpsExt` は `improvepatch.GitOps`
// を embedding しているため、`applyWorkflowDeps.gitOps` (precheck 用 GitOps seam) と
// `newProposeLoopCoreFn` の第 1 引数 (GitOps interface) の双方に同一値を渡せる。
//
// 5.2 用 factory (`newPrecheckLoaderFastPathCoreFn`) は既存テスト経路で利用するため保持する。
func defaultApplyWorkflowDeps(agent workflowctx.AgentCaller) applyWorkflowDeps {
	gitOpsExt := NewGitOpsExt()
	load := loadFunc(Load)
	applyHook := newApplyHook(gitOpsExt, agent)
	return applyWorkflowDeps{
		gitOps:    gitOpsExt,
		load:      load,
		agent:     agent,
		applyHook: applyHook,
		coreFn:    newProposeLoopCoreFn(gitOpsExt, load, agent, applyHook),
	}
}

// Workflow は review-apply-workflow の workflow.Definition を返す
// (design.md §Workflow / tasks.md 5.1 / Req 8.1, 8.2)。
//
// Definition のフィールド契約:
//   - ID: "review-apply-workflow" (Req 8.1)
//   - Label: "レビュー結果適用"
//   - Args: [{Key:"review-run-id", Required:false}] (Req 8.2、Sequential 経路では
//     ctx 注入で満たされるため Required:false / design.md §Workflow Service Interface)
//   - Hidden: false (`rilll list` / TUI トップに通常表示する)
//   - Recorder: workflow.RecorderFromAgent(agent) で取得 (NoopRecorder では no-op 記録)
//
// agent は workflowctx.AgentCaller。本 skeleton (5.1) では agent を直接呼ばないが、
// 5.2+ task で plan / diff / self-review / commit-message LLM 呼び出しのために利用する。
// agent==nil でも Definition 構築自体は成功する (Recorder 解決が nil で no-op になるだけ)
// が、Fn 内部で agent を呼ぶ経路は 5.2+ で別途 nil 防御を加える契約。
func Workflow(agent workflowctx.AgentCaller) workflow.Definition {
	deps := defaultApplyWorkflowDeps(agent)
	return workflow.Definition{
		ID:    applyWorkflowID,
		Label: applyWorkflowLabel,
		Args: []workflow.ArgSpec{
			{
				Key:         argReviewRunID,
				Description: "前段 review-workflow が出力した run ID (Sequential 経由では ctx で受領される)",
				Required:    false,
			},
		},
		Hidden:   false,
		Recorder: workflow.RecorderFromAgent(agent),
		Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			return runApplyWorkflowOrchestration(ctx, cwd, onEvent, ask, deps)
		},
	}
}

// runApplyWorkflowOrchestration は Workflow.Fn の本体実装 (skeleton)。
//
// 処理順序 (厳守):
//  1. runID / runDir / startedAt の確定 + MkdirAll (best-effort)
//  2. ResultRecord 集約 state の初期化 (defer から参照される)
//  3. defer による result.json 書き出し不変式の登録 (Req 7.3)
//     - panic は recover で捕捉し VerdictFailed に fallback してから書き出す (Req 7.4)
//     - VerdictUnset のまま return した経路も VerdictFailed に fallback する (skeleton 防御)
//  4. reviewRunID 二段階解決 (ctx 第一優先 / arg 第二優先) (Req 1.2, 8.2)
//     - 双方空なら VerdictPreconditionFailed で副作用ゼロ終端
//  5. coreFn が注入されていれば呼び出し、なければ skeleton fallback (VerdictFailed)
//
// 戻り値: intent verdict なら nil、それ以外は `[verdict:<id>] <summary>` 形 error。
// panic は recover で握り、error として呼び出し元に返す (再 raise しない)。
func runApplyWorkflowOrchestration(
	ctx context.Context,
	cwd string,
	onEvent func(workflow.WorkflowEvent),
	ask workflow.WorkflowAsk,
	deps applyWorkflowDeps,
) (retErr error) {
	// onEvent==nil 防御 helper (CLI 単独 / テスト経路で nil で渡される場合を吸収)。
	onEventSafe := func(event workflow.WorkflowEvent) {
		if onEvent != nil {
			onEvent(event)
		}
	}

	// (1) runID / runDir / startedAt の確定。
	//
	// RunIDFromContext は RunMetadata 不在のとき新規 UUID を返す契約のため、
	// CLI 単独 / テスト経路 (RunMetadata 注入無し) でも常に有効な runID が手に入る。
	startedAt := time.Now().UTC()
	applyRunID := workflow.RunIDFromContext(ctx)
	runDir := filepath.Join(cwd, ".rilll", applyWorkflowID, applyRunID)
	// MkdirAll の失敗は best-effort で握りつぶす: 後続の WriteResultJSON 段で
	// 再度判明する (失敗時はその段で wrap されて返る)。
	_ = os.MkdirAll(runDir, 0o755)

	// (2) ResultRecord 集約 state。
	//
	// defer 経由で書き出される最終 record。各 phase が必要に応じて書き換える。
	// TargetRuleNames / Cycles は空 slice で初期化することで、中途終端時の
	// JSON 出力が `null` ではなく `[]` となるようにする (Loader / cycle 未到達経路)。
	record := &ResultRecord{
		WorkflowRunID:   applyRunID,
		StartedAt:       startedAt.Format(time.RFC3339Nano),
		TargetRuleNames: []string{},
		Cycles:          []CycleRecord{},
	}

	// (3) defer 不変式 (Req 7.3 / 7.4)。
	//
	// 全 exit 経路 (panic 含む) で必ず result.json が書き出される。
	// VerdictUnset (空文字) は最終 verdict として書き出さない不変式 (design.md §Verdict Invariants)
	// を維持するため、未設定経路では VerdictFailed に fallback する。
	defer func() {
		if r := recover(); r != nil {
			// panic 経路: 未設定 verdict を VerdictFailed に強制セット。
			if record.FinalVerdict == VerdictUnset {
				record.FinalVerdict = VerdictFailed
				record.FinalSummary = fmt.Sprintf("review-apply-workflow panic: %v", r)
			}
			// panic は再 raise しない (defer 内で握りつぶし、Fn 戻り値経由で呼び出し元に伝える)。
			// retErr が既に設定済みなら上書きしない (panic 直前の error を保持)。
			if retErr == nil {
				retErr = errors.New(ErrorPrefixForVerdict(record.FinalVerdict, record.FinalSummary))
			}
		}

		// 通常 return 経路でも VerdictUnset であれば fallback (skeleton 防御 / 5.2+ 実装ミス検出)。
		if record.FinalVerdict == VerdictUnset {
			record.FinalVerdict = VerdictFailed
			if record.FinalSummary == "" {
				record.FinalSummary = "review-apply-workflow が verdict 未確定で終了 (skeleton fallback)"
			}
			if retErr == nil {
				retErr = errors.New(ErrorPrefixForVerdict(record.FinalVerdict, record.FinalSummary))
			}
		}

		// FinishedAt は defer 内で確定 (record.FinishedAt の唯一の書き込み点)。
		record.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)

		// best-effort 書き出し (akashic-artifacts spec Req 2.6 と整合)。
		// runDir 不在 / MkdirAll 失敗 / artifact 登録失敗のいずれも握りつぶす:
		// orchestration の主結果 (Fn の error 戻り値) を artifact 登録失敗で揺らさない。
		_ = WriteResultJSON(ctx, runDir, record)
	}()

	// (4) reviewRunID 二段階解決 (Req 1.2, 8.2)。
	//
	// 第一優先: ctx 注入 (Sequential 経路で前段 review.Workflow から渡される)。
	// 第二優先: `--review-run-id` arg (CLIAsk.Values から直読み)。
	// 双方空なら VerdictPreconditionFailed で副作用ゼロ終端する。
	reviewRunID, ok := ReviewRunIDFromContext(ctx)
	if !ok || reviewRunID == "" {
		reviewRunID = extractCLIArg(ask, argReviewRunID)
	}
	if reviewRunID == "" {
		return setRecordVerdict(
			record,
			VerdictPreconditionFailed,
			"review-run-id が指定されていません。--review-run-id <id> を指定するか、Sequential 経由で起動してください。",
		)
	}
	record.ReviewRunID = reviewRunID

	// 解決成功を利用者に通知 (5.2 の Loader 段サマリと併せて 2 行構成で表示される)。
	onEventSafe(workflow.WorkflowEvent{
		Type: workflow.WorkflowText,
		Text: fmt.Sprintf("review-apply-workflow start: review-run-id=%s, apply-run-id=%s", reviewRunID, applyRunID),
	})

	// (5) coreFn 経由の実作業 (5.2 で precheck → loader → no_findings fast-path、
	// 5.3+ で propose loop / apply を順次差し込む)。
	//
	// coreFn==nil の経路は `applyWorkflowDeps{coreFn: nil}` を直接渡すテスト用 fallback で、
	// `VerdictFailed` で友好的に終端する。production deps (`defaultApplyWorkflowDeps`) は
	// 常に non-nil の coreFn を割り当てる契約。
	if deps.coreFn != nil {
		return deps.coreFn(ctx, cwd, applyRunID, runDir, reviewRunID, onEventSafe, record)
	}
	return setRecordVerdict(
		record,
		VerdictFailed,
		"review-apply-workflow skeleton: coreFn が未注入 (テスト用 fallback)",
	)
}

// setRecordVerdict は ResultRecord に finalVerdict / finalSummary を確定し、IsIntent==true で
// nil、それ以外で `[verdict:<id>] <summary>` 形 error を返す package-private helper。
//
// 旧 `runApplyWorkflowOrchestration` 内の `setVerdict` クロージャを free function 化したもの。
// `newPrecheckLoaderFastPathCoreFn` で構築される coreFn からも同 helper を共有することで、
// Verdict / Summary / Fn 戻り値の整合性 (Req 6.3 / 7.3 / 7.4) を 1 箇所に集約する。
func setRecordVerdict(record *ResultRecord, v Verdict, summary string) error {
	record.FinalVerdict = v
	record.FinalSummary = summary
	if v.IsIntent() {
		return nil
	}
	return errors.New(ErrorPrefixForVerdict(v, summary))
}

// newPrecheckLoaderFastPathCoreFn は task 5.2 の coreFn (precheck → loader →
// no_findings fast-path) を構築する factory (design.md §Single-Run §precheck §Loader §findings 0 件)。
//
// 構築された coreFn は次の順序で実行する:
//
//  1. Precheck (clean tree + reviewRunID 安全性 + dir 存在) を実行。失敗時は
//     `VerdictPreconditionFailed` を確定し副作用ゼロで return (Req 1.3, 2.1, 2.2, 2.5)。
//  2. Loader.Load を実行し TargetRules / SkippedSummary を取得。LoaderSchemaError /
//     I/O 失敗は `VerdictPreconditionFailed` に集約する (Req 1.4 / Loader と Workflow の合流)。
//  3. record.TargetRuleNames / record.SkippedRuleSummary を埋め、サマリを `WorkflowText`
//     として onEvent に流す (Req 1.1, 1.5)。
//  4. TargetRules == 0 件なら `VerdictNoFindings` (intent) を確定し副作用ゼロで return (Req 1.6)。
//  5. TargetRules >= 1 件は task 5.3+ で propose loop に進むため、現状は `VerdictFailed` で
//     skeleton fallback 終端する (5.3 で本ブロックが置換される予定)。
//
// gitOps / load は seam で、テスト経路では fake を注入して副作用ゼロ契約と verdict 振り分けを
// 機械検証する。production では `defaultApplyWorkflowDeps()` が実 gitOps / Loader を割り当てる。
func newPrecheckLoaderFastPathCoreFn(gitOps improvepatch.GitOps, load loadFunc) func(
	ctx context.Context,
	cwd string,
	applyRunID string,
	runDir string,
	reviewRunID string,
	onEvent func(workflow.WorkflowEvent),
	record *ResultRecord,
) error {
	return func(
		ctx context.Context,
		cwd string,
		applyRunID string,
		runDir string,
		reviewRunID string,
		onEvent func(workflow.WorkflowEvent),
		record *ResultRecord,
	) error {
		return runPrecheckLoaderFastPath(ctx, cwd, applyRunID, runDir, reviewRunID, onEvent, record, gitOps, load)
	}
}

// newProposeLoopCoreFn は task 5.3 の coreFn factory。
// 5.2 と同じ precheck → loader → no_findings fast-path を通過した後、
// findings_reported が 1 件以上ある場合に propose loop (plan → diff → scope guard →
// self-review) を起動する。
func newProposeLoopCoreFn(
	gitOps improvepatch.GitOps,
	load loadFunc,
	agent workflowctx.AgentCaller,
	applyHook ProposeApplyHook,
) func(
	ctx context.Context,
	cwd string,
	applyRunID string,
	runDir string,
	reviewRunID string,
	onEvent func(workflow.WorkflowEvent),
	record *ResultRecord,
) error {
	return func(
		ctx context.Context,
		cwd string,
		applyRunID string,
		runDir string,
		reviewRunID string,
		onEvent func(workflow.WorkflowEvent),
		record *ResultRecord,
	) error {
		loaded, done, err := runPrecheckAndLoad(ctx, cwd, reviewRunID, onEvent, record, gitOps, load)
		if done || err != nil {
			return err
		}
		return runProposeLoop(ctx, cwd, applyRunID, runDir, reviewRunID, onEvent, record, &loaded, agent, applyHook)
	}
}

// runPrecheckLoaderFastPath は task 5.2 の coreFn 本体 (precheck → loader → no_findings
// fast-path)。`newPrecheckLoaderFastPathCoreFn` の closure からのみ呼ばれる構成だが、
// 関心の分離のため独立した free function として切り出す (テスト経路でも単独テスト可)。
//
// 副作用契約:
//   - precheck 段は gitOps.Status のみを呼ぶ (Snapshot/ApplyCheck/Apply/Revert は呼ばない)。
//   - load 段は filesystem read のみ (LLM / git apply / commit は呼ばない)。
//   - findings 0 件 fast-path / 5.3 未実装 fallback いずれも LLM / git apply / commit を呼ばない。
//
// 戻り値: intent (`no_findings`) で nil、それ以外は `[verdict:<id>] <summary>` 形 error。
func runPrecheckLoaderFastPath(
	ctx context.Context,
	cwd string,
	_ string, // applyRunID は record.WorkflowRunID で既に保持済 (5.3+ artifact 採番で利用)
	_ string, // runDir は cycle artifact 書き出し (5.3+) で利用
	reviewRunID string,
	onEvent func(workflow.WorkflowEvent),
	record *ResultRecord,
	gitOps improvepatch.GitOps,
	load loadFunc,
) error {
	loaded, done, err := runPrecheckAndLoad(ctx, cwd, reviewRunID, onEvent, record, gitOps, load)
	if done || err != nil {
		return err
	}

	// findings >= 1 件: 5.2 fast-path factory では propose loop に進まず skeleton fallback を維持する。
	// 5.3 production wiring は newProposeLoopCoreFn を使い、この地点から runProposeLoop に進む。
	_ = loaded
	return setRecordVerdict(record, VerdictFailed,
		"review-apply-workflow: propose loop は task 5.3+ で実装されます")
}

// runPrecheckAndLoad は precheck → loader → no_findings fast-path までを共有する helper。
// 戻り値 done=true は record.FinalVerdict が確定済みで caller がそのまま return すべきことを表す。
func runPrecheckAndLoad(
	ctx context.Context,
	cwd string,
	reviewRunID string,
	onEvent func(workflow.WorkflowEvent),
	record *ResultRecord,
	gitOps improvepatch.GitOps,
	load loadFunc,
) (LoadResult, bool, error) {
	// (1) Precheck: clean tree + reviewRunID 安全性 + reviewRunDir 存在 を順に検証する。
	// Precheck 内部で副作用契約 (gitOps.Status のみ呼び出し) は守られる前提。
	pre := Precheck(ctx, PrecheckInput{
		Cwd:         cwd,
		ReviewRunID: reviewRunID,
		GitOps:      gitOps,
	})
	if pre.Verdict != VerdictUnset {
		// precheck 失敗時は副作用ゼロで終端する。pre.Summary は利用者向け短文を保持済。
		return LoadResult{}, true, setRecordVerdict(record, pre.Verdict, pre.Summary)
	}

	// (2) Loader.Load: per-rule 結果を全件ロードし findings_reported のみ抽出する。
	// schema 違反 / I/O 失敗は VerdictPreconditionFailed に集約する。
	loaded, err := load(ctx, cwd, reviewRunID)
	if err != nil {
		var schemaErr *LoaderSchemaError
		switch {
		case errors.As(err, &schemaErr):
			summary := fmt.Sprintf(
				"review run の per-rule 結果スキーマが不正です: %s。`review-workflow` を再実行してください。",
				schemaErr.File,
			)
			return LoadResult{}, true, setRecordVerdict(record, VerdictPreconditionFailed, summary)
		case errors.Is(err, ErrEmptyReviewRunID), errors.Is(err, ErrReviewRunDirNotFound):
			// Precheck で同名 sentinel を捕捉済の防御層 (二重検証になるが defensive で残す)。
			return LoadResult{}, true, setRecordVerdict(record, VerdictPreconditionFailed,
				fmt.Sprintf("review run のロードに失敗しました: %v", err))
		default:
			return LoadResult{}, true, setRecordVerdict(record, VerdictPreconditionFailed,
				fmt.Sprintf("review run のロードに失敗しました: %v", err))
		}
	}

	// (3) record に転記: TargetRuleNames / SkippedRuleSummary を埋める。
	// TargetRuleNames は空集合 (no_findings fast-path 経路) でも空 slice で初期化済のため
	// 上書きで安定する。SkippedRuleSummary は空のときは nil のままにし、`omitempty` で
	// JSON 出力から省略させる (design.md §Result `omitempty` 設計と整合)。
	targetNames := make([]string, 0, len(loaded.TargetRules))
	for _, r := range loaded.TargetRules {
		targetNames = append(targetNames, r.Name)
	}
	record.TargetRuleNames = targetNames

	if len(loaded.SkippedSummary) > 0 {
		skipped := make(map[string]int, len(loaded.SkippedSummary))
		for k, v := range loaded.SkippedSummary {
			skipped[string(k)] = v
		}
		record.SkippedRuleSummary = skipped
	}

	// (4) サマリを WorkflowText で利用者に表示 (Req 1.1, 1.5)。
	// rule 件数 / 修正対象 rule 名 / 対象外 rule の verdict 別件数を 1 行に集約する。
	if onEvent != nil {
		text := fmt.Sprintf(
			"review-apply-workflow loaded: 対象 %d 件 (rules=%v)",
			len(loaded.TargetRules), targetNames,
		)
		if len(loaded.SkippedSummary) > 0 {
			text += ", 対象外 " + formatSkippedSummary(loaded.SkippedSummary)
		}
		onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: text})
	}

	// (5) findings 0 件 fast-path: VerdictNoFindings (intent) で副作用ゼロ終端 (Req 1.6)。
	if len(loaded.TargetRules) == 0 {
		return loaded, true, setRecordVerdict(record, VerdictNoFindings,
			"修正対象の指摘がありませんでした。何も変更していません。")
	}

	return loaded, false, nil
}

// formatSkippedSummary は SkippedSummary (verdict→count) を deterministic な人間可読
// 文字列に整形する。例: "{failed=1, no_findings=2}"。verdict 文字列で昇順ソートし、
// 出力が CI / 利用者表示で一定になるよう保証する (Req 1.5 利用者向け表示の安定性)。
func formatSkippedSummary(skipped map[result.Verdict]int) string {
	if len(skipped) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(skipped))
	for k := range skipped {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, skipped[result.Verdict(k)]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// extractCLIArg は WorkflowAsk が CLIAsk のとき Values[key] を取り出す helper。
//
// dispatch.go では `ask := workflow.CLIAsk{Values: askValues}` (値型) を passes する一方、
// テストや一部経路では `&workflow.CLIAsk{...}` (ポインタ型) を渡す慣習があるため、
// 値型 / ポインタ型双方を type-assertion で吸収する。いずれでもなければ空文字を返す
// (TUI 経路では `--review-run-id` arg は使われず ctx 注入経路で満たされる前提)。
func extractCLIArg(ask workflow.WorkflowAsk, key string) string {
	if ask == nil {
		return ""
	}
	if cli, ok := ask.(*workflow.CLIAsk); ok && cli != nil {
		return cli.Values[key]
	}
	if cli, ok := ask.(workflow.CLIAsk); ok {
		return cli.Values[key]
	}
	return ""
}
