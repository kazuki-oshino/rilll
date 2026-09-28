package kiro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/facet"
	"github.com/kazuki-oshino/rilll/internal/inquiryloop"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/provider/middleware/streamidle"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	gitworkflow "github.com/kazuki-oshino/rilll/internal/workflow/builtin/git"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// implPersonaName は kiro-impl-loop の問い合わせ⇄回答ループで回答エージェントに与える
// 論理 Persona 名。実際の session B Persona facet asset は impl-direction-answerer.md に
// 分離され、resolveSessionBPersona が facet provenance 付きの AnswerPersona へ変換する。
//
// kiro-spec-loop の architecture-quality と別立てにしている理由は、impl の問いかけに対する
// 回答方針として「再質問せず、方針指示型 1 round で収束させる」ことを前面に出すため。
const implPersonaName = "impl-direction"

// implAnswerPolicyName は impl-direction answerer に注入する共有判断基準。
// 実装方針の問い合わせで AI 特有の過剰実装・配線忘れ・黙る fallback を抑止する。
const implAnswerPolicyName = "ai-antipattern-guard"

// implLoopInquiryDirective は kiro-impl-loop が inquiry-skill 側 (session A = kiro-impl) に
// 毎ラウンド注入する loop policy 文字列。Engine.InquiryDirective として配線され、
// initial / resume の両方で runnerDirective() の直前に再付加される。
//
// 役割: kiro-impl-loop は 1 iteration = 1 task が契約。だが akashic ログ
// (2026-05-08T07-21-33-... の 04-impl-2_3 / 06-impl-2_5 等) で、kiro-impl skill が
// inquiry の options[] に「次の task X を実行」「複数 task をまとめる」「Phase Y 全体を
// autonomous で進める」のような scope 拡張選択肢を並べる挙動が観測された。
// workflow 層 (per-iteration skip) と answer ペルソナ層 (scope 拡張 option を不採択) に
// 続く第 3 層の防御として、源流 (kiro-impl skill 自身) で scope 越え inquiry を
// そもそも立てさせないよう毎ラウンド再強調する。
const implLoopInquiryDirective = `[loop-policy]
このループは 1 iteration = 1 task で動作する。inquiry を立てる場合、options[] や
question 本文に以下を含めないこと:
- assign された task ID の境界を越える scope 拡張
  (例: 「次の task X を実行」「複数 task をまとめる」「Phase Y 全体を autonomous で進める」)
判断が assign された task の範囲に収まる場合は inquiry を立てず、
has_inquiry=false で完了すること。`

func KiroImplLoop(agent workflowctx.AgentCaller) workflow.Definition {
	return workflow.Definition{
		ID:    "kiro-impl-loop",
		Label: "Kiro 実装ループ",
		Args: []workflow.ArgSpec{
			{Key: "feature", Description: ".kiro/specs/<feature>/ 配下の対象 feature 名", Required: true},
		},
		Recorder: workflow.RecorderFromAgent(agent),
		Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			return runKiroImplLoop(ctx, cwd, onEvent, ask, agent)
		},
	}
}

// runKiroImplLoop は KiroImplLoop の workflow.Definition.Fn 本体実装。
//
// フロー:
//  1. listImplementableFeatures で候補 feature を収集 → ask.Select で選択
//  2. spec.json / tasks.md を検証し pendingTasks を抽出
//  3. impl 専用 session B Persona を facet adapter で解決 (失敗で abort)
//  4. .rilll/kiro-impl-loop/<run-id>/00-context.json に Persona provenance を含めて書き出し artifact 登録
//  5. 各 pendingTask に対し inquiryloop.Engine.RunPrompt で問い合わせ⇄回答ループを駆動
//     - 完走 (StatusCompleted) → tasks.md 更新 → /git-commit
//     - 中断 (StatusAborted)   → workflow.WorkflowError 発火し本 run を停止
//  6. 全タスク完走後に final-review を実行
func runKiroImplLoop(
	ctx context.Context,
	cwd string,
	onEvent func(workflow.WorkflowEvent),
	ask workflow.WorkflowAsk,
	agent workflowctx.AgentCaller,
) error {
	runDir := filepath.Join(cwd, ".rilll", "kiro-impl-loop", workflow.RunIDFromContext(ctx))
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return err
	}

	folders, err := listImplementableFeatures(cwd)
	if err != nil {
		onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: "実施可能な feature の収集に失敗しました: " + err.Error()})
		return nil
	}
	if len(folders) == 0 {
		onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: "実施可能な feature が見つかりません: tasks.md があり未チェック項目が残っており、spec.json が承認済み（ready_for_implementation: true / approvals.* が approved）の feature を準備してください"})
		return nil
	}
	if ask == nil {
		onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: "対象featureを選択できません"})
		return nil
	}
	items := make([]workflow.SelectItem, 0, len(folders))
	for _, folder := range folders {
		items = append(items, workflow.SelectItem{ID: folder, Label: folder})
	}
	feature, ok, err := ask.Select(ctx, workflow.SelectParams{Key: "feature", Prompt: "対象のfeatureを選択してください（ESCでキャンセル）:", Items: items})
	if err != nil {
		return err
	}
	if !ok {
		onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: "キャンセルされました"})
		return nil
	}

	onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "\nfeature: " + feature + "\nspec.jsonを検証中...\n"})
	specPath := filepath.Join(cwd, ".kiro", "specs", feature, "spec.json")
	specRaw, err := os.ReadFile(specPath)
	if err != nil {
		onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: "spec.jsonが見つかりません: " + specPath})
		return nil
	}
	var specJSON any
	if err := json.Unmarshal(specRaw, &specJSON); err != nil {
		onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: "spec.jsonの読み込みに失敗しました: " + err.Error()})
		return nil
	}
	validation := ValidateSpec(specJSON)
	if !validation.OK {
		onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: "spec.json検証NG: " + validation.Reason})
		return nil
	}

	tasksPath := filepath.Join(cwd, ".kiro", "specs", feature, "tasks.md")
	tasksRaw, err := os.ReadFile(tasksPath)
	if err != nil {
		onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: "tasks.mdが見つかりません: " + tasksPath})
		return nil
	}
	pendingTasks := ExtractPendingLeaves(ParseTasksMD(string(tasksRaw)))

	// impl 専用 session B Persona の解決。logical name は impl-direction のまま維持し、
	// SourcePath は impl-direction-answerer facet の provenance として 00-context.json に残す。
	// 失敗時は impl ループ自体を開始せず workflow.WorkflowError で停止する。
	implPersona, err := resolveSessionBPersona(cwd, implPersonaName)
	if err != nil {
		if errors.Is(err, facet.ErrFacetNotFound) || errors.Is(err, errEmptySessionBPersona) {
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: formatSessionBPersonaError(implPersonaName, err)})
			return nil
		}
		return fmt.Errorf("runKiroImplLoop: resolveSessionBPersona(%s): %w", implPersonaName, err)
	}
	implPolicies, err := resolveAnswerPolicies(cwd, implAnswerPolicyName)
	if err != nil {
		if errors.Is(err, facet.ErrFacetNotFound) || errors.Is(err, errEmptyAnswerPolicy) {
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: fmt.Sprintf("answer policy %q is unavailable: %v", implAnswerPolicyName, err)})
			return nil
		}
		return fmt.Errorf("runKiroImplLoop: resolveAnswerPolicies(%s): %w", implAnswerPolicyName, err)
	}
	policyNames := make([]string, 0, len(implPolicies))
	policyPaths := make([]string, 0, len(implPolicies))
	for _, policy := range implPolicies {
		policyNames = append(policyNames, policy.Name)
		policyPaths = append(policyPaths, policy.SourcePath)
	}

	contextJSON, _ := json.MarshalIndent(map[string]any{
		"feature":      feature,
		"pendingTasks": pendingTasks,
		"personaName":  implPersona.Name,
		"personaPath":  implPersona.SourcePath,
		"policyNames":  policyNames,
		"policyPaths":  policyPaths,
	}, "", "  ")
	contextPath := filepath.Join(runDir, "00-context.json")
	_ = os.WriteFile(contextPath, contextJSON, 0o644)
	// 00-context.json を context kind として akashic レコードへ登録
	// (akashic-artifacts spec Req 2.5)。登録失敗はワークフロー本体の戻り値・
	// success/failure 判定に伝播させない (Req 2.6)。ファイル名と artifact 登録の
	// best-effort 方針は維持しつつ、session B Persona provenance を同じ context に含める。
	_ = workflow.RecordRunArtifact(ctx, akashic.ArtifactKindContext, contextPath)

	// taskSummaries は per-task ループ内で前段タスクの最終 summary を順次蓄積する
	// in-memory バッファ (Design §Components previousSummaryForTaskIndex)。
	//
	// 契約:
	//   - 各 iteration の冒頭で `previousSummaryForTaskIndex(i, taskSummaries, runDir, pendingTasks)`
	//     に渡して直前 1 段 summary を解決し、`previousResponseBlock(prev)` で skill
	//     コマンドの **後ろ**に連結した文字列を `inquiryloop.RunInput.PromptCommand` に渡す。
	//     slash command (`/kiro-impl <feature> <taskID>`) を prompt 先頭に保つため、
	//     前段 summary は **後置 block** として連結する (Claude Code の skill 起動は
	//     先頭トークンが `/<skill>` でないと走らない仕様への対応)。foundation 側
	//     `IncludePreviousResponse` の **前置 (prepend)** 経路は本ループからは使わない。
	//   - 末尾要素 (taskSummaries[i-1]) が「直前 1 段」を表すよう、各 iteration の
	//     末尾で必ず 1 件 append する (累積禁止 = N-2 以前は参照しない)。
	//   - per-iteration skip ケース (既 [x] でない既完了タスク) でも空文字列を append し、
	//     index ↔ taskID 対応を維持する。これにより skip 直後の未完了タスクは
	//     「直前タスクが skip 済 = 引き継ぐべき前段 summary を持たない」ため
	//     後置 block なし (previousResponseBlock が空文字を返し block 自体を省略) で
	//     起動する (graceful degradation 経路に合流)。
	taskSummaries := make([]string, 0, len(pendingTasks))

	totalSteps := len(pendingTasks) + 1
	for i, taskID := range pendingTasks {
		onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowStep, Step: i + 1, TotalSteps: totalSteps, Label: "タスク " + taskID})

		// per-iteration skip:
		// pendingTasks は run 開始時の snapshot なので、ループが進む間に直前 iteration の
		// spillover (実装エージェントが scope 越えして他タスクを暗黙的に [x] 化したケース)
		// で既完了になったタスクが含まれる可能性がある。再呼び出しすると "もう完了済" 系
		// inquiry を踏み無駄なラウンドを消費し、回答ペルソナが scope 拡張 option を採択する
		// 連鎖の起点になるため、tasks.md を再読して既 [x] なら skip する。
		// 変数名を preIterTasksRaw とすることで、後段 engine 完走後の post-engine read
		// (currentTasksRaw / ApplyChildCheck 入力) と混同しないようにする。
		preIterTasksRaw, err := os.ReadFile(tasksPath)
		if err != nil {
			return err
		}
		if IsTaskChecked(string(preIterTasksRaw), taskID) {
			onEvent(workflow.WorkflowEvent{
				Type: workflow.WorkflowText,
				Text: fmt.Sprintf("タスク %s は既に完了済みのためスキップします\n", taskID),
			})
			// per-iteration skip でも index ↔ taskID 対応を維持するため taskSummaries に
			// 空文字列を append する (kiro-impl-loop-facets task 3.2 / Implementation Notes 3.2)。
			// skip 直後の未完了タスクは「直前が skip 済」のため前置なしで起動するのが
			// 望ましい (skip タスクは過去ランで完了済 = 引き継ぐべき前段 summary を持たない)。
			taskSummaries = append(taskSummaries, "")
			continue
		}

		// 子なし親タスク（ID にドットなし）は tasks.md の慣習に合わせて
		// 末尾ドット付きで agent に渡す（例: "3" → "3."）
		promptID := taskID
		if !strings.Contains(taskID, ".") {
			promptID = taskID + "."
		}
		order := i + 1
		// task ID は "1.2" のようにドットを含むため、ディレクトリ名に取り込みやすいよう
		// '_' に置換する。kiro_spec_loop の <NN>-<phase>/ 形式と同形式の永続化を採用。
		phase := "impl-" + sanitizePhaseSegment(taskID)
		stepDirAbs := stepDir(runDir, order, phase)
		promptCmd := "/kiro-impl " + feature + " " + promptID
		promptLabel := fmt.Sprintf("タスク %s", taskID)

		// 回答エージェント (session B) に「どの spec / どのタスクの問い合わせか」を
		// 明示するため、関連 spec ファイルの絶対パスと元 PromptCommand を AnswerContext
		// として渡す。session B は毎ラウンド fresh session で呼ばれるため、ここで
		// 注入しないと判別不能。実在チェックはせず pass-through する (回答側が Read で
		// NotFound を見れば判別できる)。
		specDir := filepath.Join(cwd, ".kiro", "specs", feature)
		specPaths := []string{
			filepath.Join(specDir, "requirements.md"),
			filepath.Join(specDir, "design.md"),
			filepath.Join(specDir, "tasks.md"),
		}

		// 直前タスクの summary を二段フォールバックで解決する。
		// previousSummaryForTaskIndex は (in-memory hit / on-disk fallback / graceful "")
		// を内包し、i==0 の先頭タスクでは "" を返す。
		prevSummary := previousSummaryForTaskIndex(i, taskSummaries, runDir, pendingTasks)

		// adapter は単純な forwarder (workflowctx.AgentCaller → inquiryloop.AgentCaller)。
		// Previous Response の連結は本ループ側で skillPromptCmd に後置する形に集約しているため、
		// adapter には foundation 前置経路 (IncludePreviousResponse) を持たせない
		// (slash command を prompt 先頭に保つ slash-first 順を破壊しないため)。
		adapter := agentCallerAdapter{
			inner:   agent,
			onEvent: onEvent,
		}

		// MaxRounds=0 のとき inquiryloop.Engine 既定 (defaultMaxRounds=5) が適用される。
		// issue #14 最終方針「spec-loop と同じ既定 5。validate-* のように 15 まで上げる必要は当面無い」。
		engine := inquiryloop.Engine{
			Persona: implPersona,
			Schema:  inquiryloop.Schema(),
			AnswerContext: inquiryloop.AnswerContext{
				Policies:      implPolicies,
				PromptCommand: promptCmd,
				SpecPaths:     specPaths,
			},
			InquiryDirective: implLoopInquiryDirective,
			StepIDFunc: func(round int, kind string) string {
				return fmt.Sprintf("%s-%s-r%d", phase, kind, round)
			},
			// resume 呼び出しの prompt 本文に
			// "Source: <path>" 行を載せ、直前ラウンドの answer 全文を切り詰めて
			// パス参照に逃がす seam。persistAnswer が同形のパス
			// (<runDir>/<NN>-<phase>/round-<N>-answer.md) に full text を書き出して
			// いるため、resume 本文では path 参照だけで監査トレースを失わずに
			// stream_idle_timeout の主因 (long answer text の resume 流入) を回避する。
			AnswerSourcePathFunc: func(round int) string {
				return filepath.Join(stepDirAbs, fmt.Sprintf("round-%d-answer.md", round))
			},
		}

		// session A 用 prompt (slash-first 順):
		//   `promptCmd + previousResponseBlock(prevSummary)`
		// slash command (`/kiro-impl <feature> <taskID>`) を必ず prompt 先頭に保ち、
		// 前段タスク summary は skill コマンドの **後ろ**に連結する。
		// previousResponseBlock 内で summary context 用の切り詰め policy を適用し、
		// prevSummary が空文字 (i==0 / skip 直後 / on-disk fallback 失敗) のときは
		// ブロックを省略する (graceful degradation: 前段 summary 取得不能でも skill 呼び出しは継続)。
		skillPromptCmd := promptCmd + previousResponseBlock(prevSummary, facet.PromptContextSummary)

		result, runErr := engine.RunPrompt(
			ctx,
			adapter,
			cwd,
			inquiryloop.RunInput{
				PromptCommand: skillPromptCmd,
				PromptLabel:   promptLabel,
			},
			func(round inquiryloop.Round) {
				// 永続化失敗 (disk full 等) は engine に伝播させずベストエフォート扱い。
				_ = persistInquiry(runDir, order, phase, round.Index, round.Inquiry)
				_ = persistAnswer(runDir, order, phase, round.Index, implPersona.Name, round.AnswerText)
			},
		)
		if runErr != nil {
			// engine.RunPrompt は基本 err=nil で FinalResult に詰めて返すが、
			// 防御的に propagate する (kiro_spec_loop.go と同じ扱い)。
			return fmt.Errorf("runKiroImplLoop: engine.RunPrompt(%s): %w", phase, runErr)
		}
		if result.Status == inquiryloop.StatusAborted {
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: formatImplAbortMessage(taskID, result)})
			return nil
		}

		// per-task `result.json` を best-effort で書き出す
		// (kiro-impl-loop-facets task 3.2 / Design §Components persistImplTaskResult /
		// §Error Strategy ベストエフォート)。
		//
		// 用途: restart 復元時に in-memory `taskSummaries` が空でも、ディスク上の
		// `result.json` から前段 summary を再生できるようにする
		// (`previousSummaryForTaskIndex` の二段フォールバック (3) 経路の on-disk source)。
		// I/O 失敗は `persistInquiry` / `persistAnswer` と同方針で握りつぶし、commit step
		// の進行を妨げない (Req 1.4 graceful degradation の source、Req 5.x 既存挙動 retain)。
		_ = persistImplTaskResult(runDir, order, phase, result.Summary, result.LastSession)

		// in-memory taskSummaries に当該タスクの最終 summary を append する
		// (kiro-impl-loop-facets task 3.2)。Summary が空文字でも append し、index ↔ taskID
		// 対応を維持する (次イテレーションの `previousSummaryForTaskIndex` が
		// `taskSummaries[i-1]` を直接参照するため、空でも要素位置を保つ必要がある)。
		taskSummaries = append(taskSummaries, result.Summary)

		currentTasksRaw, err := os.ReadFile(tasksPath)
		if err != nil {
			return err
		}
		checked := ApplyChildCheck(string(currentTasksRaw), taskID)
		rolled := ApplyParentRollup(checked)
		if err := os.WriteFile(tasksPath, []byte(rolled), 0o644); err != nil {
			return err
		}

		commitResult, err := gitworkflow.CallCommit(ctx, agent, cwd, onEvent, "commit-"+taskID, "タスク "+taskID+" commit")
		if err != nil || !commitResult.Success {
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: formatStepError(taskID, "/git-commit", err)})
			return nil
		}
	}

	onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowStep, Step: totalSteps, TotalSteps: totalSteps, Label: "最終検証"})
	finalTasksRaw, err := os.ReadFile(tasksPath)
	if err != nil {
		return err
	}
	if HasUncheckedItems(string(finalTasksRaw)) {
		onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: "中断: タスクループ完了後もtasks.mdに未チェック項目が残っています"})
		return nil
	}

	// final-review (Phase 1-5) 全体を runFinalReview に委譲する。
	//
	// 旧実装は `/kiro-validate-impl` を 1 shot で agent.CallAgent し、その直後に WorkflowDone を
	// 発火する単純構造だったが、本 spec (kiro-validate-impl-loop) で次の 5 phase 構造に置換した:
	//   - Phase 1: inquiryloop.Engine.RunPrompt 駆動 (MaxRounds=15)
	//   - Phase 2-3: fix-edit + fix-judge (resume A、Schema 強制下の Edit/Write 回避のため 2-turn)
	//   - Phase 4: test-edit + test-judge (resume Phase 2 fix-edit、同 2-turn)
	//   - Phase 5: /git-commit (fresh session)
	//
	// runFinalReview は完走時 / abort 時とも自身で WorkflowDone / WorkflowError を 1 回だけ発火する
	// 契約 (Implementation Notes 3.1 参照) のため、本関数側で追加の WorkflowDone / WorkflowError を
	// 発火させないこと。I/O 致命的エラーのみ runFinalReview が err として propagate する。
	if err := runFinalReview(ctx, agent, cwd, runDir, feature, totalSteps, onEvent, implPersona); err != nil {
		return err
	}
	return nil
}

// listImplementableFeatures は kiro-impl-loop の対象として「いま実施可能」な
// feature 名一覧を辞書順で返す。
//
// 含む条件（すべて AND）:
//
//	(a) <cwd>/.kiro/specs/<feature>/tasks.md が存在し読み込める
//	(b) tasks.md に未チェック項目（- [ ] 行）が 1 件以上ある
//	(c) <cwd>/.kiro/specs/<feature>/spec.json が読み込め、ValidateSpec を通る
//	    （= ready_for_implementation:true かつ approvals.{requirements,design,tasks}.approved=true）
//
// (b) は HasUncheckedItems、(c) は ValidateSpec をそれぞれ再利用し、判定ロジックの
// 二重化を避ける。kiro-spec-loop の listEligibleFeatures と同様、各 feature 単位の
// 検証 NG / ファイル不在は "その feature だけスキップ" し、他 feature の候補化を
// 妨げない。spec.json のパース失敗等も安全側で「対象外」として握りつぶす。
//
// .kiro/specs/ ディレクトリ自体が存在しない場合はエラーを返さず空スライスを返す
// （listEligibleFeatures と同じ I/O インターフェース方針）。それ以外の I/O エラー
// （権限不足等）は呼び出し側に伝播する。
func listImplementableFeatures(cwd string) ([]string, error) {
	specsDir := filepath.Join(cwd, ".kiro", "specs")
	entries, err := os.ReadDir(specsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("listImplementableFeatures: read %s: %w", specsDir, err)
	}

	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		feature := entry.Name()
		featureDir := filepath.Join(specsDir, feature)

		// (a)(b) tasks.md が存在し未チェック項目が残っていること。
		tasksRaw, err := os.ReadFile(filepath.Join(featureDir, "tasks.md"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("listImplementableFeatures: read %s/tasks.md: %w", featureDir, err)
		}
		if !HasUncheckedItems(string(tasksRaw)) {
			continue
		}

		// (c) spec.json が ValidateSpec を通ること。
		specRaw, err := os.ReadFile(filepath.Join(featureDir, "spec.json"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("listImplementableFeatures: read %s/spec.json: %w", featureDir, err)
		}
		var specJSON any
		if err := json.Unmarshal(specRaw, &specJSON); err != nil {
			continue
		}
		if !ValidateSpec(specJSON).OK {
			continue
		}

		out = append(out, feature)
	}

	sort.Strings(out)
	return out, nil
}

// formatStepError は failure_category をプレフィックスに含めた中断メッセージを生成する。
// 例: `中断: [3.2] [stream_idle_timeout] 実装エージェントが失敗しました`
// agent パッケージへの import 循環を避けるため、ここでは streamidle のセンチネル
// エラーから category を直接導出する (他カテゴリは workflow から検出不能なので無印)。
func formatStepError(stepID, agentLabel string, err error) string {
	prefix := fmt.Sprintf("中断: [%s]", stepID)
	if errors.Is(err, streamidle.ErrStreamIdleTimeout) {
		prefix = fmt.Sprintf("中断: [%s] [stream_idle_timeout]", stepID)
	}
	return fmt.Sprintf("%s %s が失敗しました", prefix, agentLabel)
}

// formatImplAbortMessage は inquiryloop.Engine.RunPrompt が aborted で返したときに
// ユーザー向けに表示する中断メッセージを組み立てる。
//
// 形式:
//
//	中断: [<taskID>] [<termination>] 実装エージェントが完了しませんでした
//	中断: [<taskID>] [<termination>] 実装エージェントが完了しませんでした（<failure.message>）
//
// termination は inquiryloop.TerminationReason の文字列値 (loop-limit / prompt-failure / schema-violation)。
// FailureDetail.Message が空文字でない場合は丸括弧でメッセージを補足する。
func formatImplAbortMessage(taskID string, result inquiryloop.FinalResult) string {
	reason := string(result.Termination)
	base := fmt.Sprintf("中断: [%s] [%s] 実装エージェントが完了しませんでした", taskID, reason)
	if result.Failure != nil && strings.TrimSpace(result.Failure.Message) != "" {
		return fmt.Sprintf("%s（%s）", base, result.Failure.Message)
	}
	return base
}

// sanitizePhaseSegment は phase 識別子をディレクトリ / ファイル名安全な形に正規化する。
// task ID は "1.2" のようにドットを含むため、そのままでは "<NN>-<phase>/" 形式の
// 永続化先パスとして読みづらい。ドットを '_' に置換して "impl-1_2" のような phase に変換する。
//
// kiro_spec_loop の "spec-init" / "validate-gap" のような英数字ハイフン phase と区別するため、
// kiro_impl_loop 側ではプレフィックス "impl-" を付けたうえで本関数を通す運用を取る。
func sanitizePhaseSegment(s string) string {
	return strings.ReplaceAll(s, ".", "_")
}

func simpleObjectSchema(required ...string) *provider.OutputFormat {
	properties := map[string]any{}
	for _, key := range required {
		properties[key] = map[string]any{}
	}
	return &provider.OutputFormat{
		Type: "json_schema",
		Schema: map[string]any{
			"type":       "object",
			"required":   required,
			"properties": properties,
		},
	}
}
