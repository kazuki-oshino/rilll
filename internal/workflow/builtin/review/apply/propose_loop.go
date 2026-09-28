package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// MaxRegenerateAttempts は self-review が regenerate を返したときの最大再生成回数。
// design.md §Single-Run / tasks.md 5.3 に従い、review-apply では固定値 5 とする。
const MaxRegenerateAttempts = 5

// ProposeApplyHook は propose loop が self-review accept に到達した直後に apply phase へ
// 制御を渡す seam。task 5.3 では default hook が task 5.4 deferred として failed verdict を返す。
type ProposeApplyHook func(
	ctx context.Context,
	cwd string,
	applyRunID string,
	runDir string,
	reviewRunID string,
	onEvent func(workflow.WorkflowEvent),
	record *ResultRecord,
	loaded *LoadResult,
	patch Patch,
) error

func defaultProposeApplyHook(
	_ context.Context,
	_ string,
	_ string,
	_ string,
	_ string,
	_ func(workflow.WorkflowEvent),
	record *ResultRecord,
	_ *LoadResult,
	_ Patch,
) error {
	return setRecordVerdict(record, VerdictFailed,
		"apply phase は task 5.4 で実装予定のため、この run では適用を行いません。")
}

func runProposeLoop(
	ctx context.Context,
	cwd string,
	applyRunID string,
	runDir string,
	reviewRunID string,
	onEvent func(workflow.WorkflowEvent),
	record *ResultRecord,
	loaded *LoadResult,
	agent workflowctx.AgentCaller,
	applyHook ProposeApplyHook,
) error {
	if loaded == nil {
		return setRecordVerdict(record, VerdictFailed, "Loader 結果が nil のため propose loop を開始できません。")
	}
	if agent == nil {
		return setRecordVerdict(record, VerdictFailed, "agent が未設定のため propose loop を開始できません。")
	}
	if applyHook == nil {
		applyHook = defaultProposeApplyHook
	}

	findingsFiles := loaded.FindingsFiles()
	var previousReview *SelfReviewRecord
	for attempt := 1; attempt <= MaxRegenerateAttempts; attempt++ {
		cycleStartedAt := time.Now()
		emitWorkflowText(onEvent, fmt.Sprintf("cycle %d starting (attempt=%d/%d)", attempt, attempt, MaxRegenerateAttempts))

		// Issue #46 input thinning: 直前 attempt の self-review が永続化済みの
		// `cycle-<attempt-1>-review.json` 絶対パスを RenderPlan に渡し、Source 行と
		// reason 切り詰めで prompt 入力長を抑える。attempt=1 では previousReview=nil の
		// ためパスは Render 側で参照されず、attempt=2 以降のみで効く。
		var previousReviewPath string
		if previousReview != nil && attempt >= 2 {
			previousReviewPath = filepath.Join(runDir, fmt.Sprintf("cycle-%d-review.json", attempt-1))
		}
		planPrompt := RenderPlan(PlanInput{
			ReviewRunID:        reviewRunID,
			Rules:              loaded.TargetRules,
			Attempt:            attempt,
			PreviousReview:     previousReview,
			PreviousReviewPath: previousReviewPath,
		})
		// previous-response-passing PoC 配線 (task 3.2 / Req 7.1, 7.2):
		//   - loaded.ReviewOverallSummary を全 attempt で同一の PreviousResponseContent として渡す
		//     (Decision 2 / 局所化方針: plan 呼び出しにのみ前置し diff / self-review / commit-message は触らない)。
		//   - 空 summary のときは agent.CallAgent 側の no-op 分岐 (Req 1.4) に倒れ、既存挙動を保持する。
		//   - 同一 summary を attempt 間で固定することで provider 横断・regenerate 横断の決定性 (Req 2.5 / 6.1) を満たす。
		planResp, planErr := agent.CallAgent(ctx, planPrompt, cwd, nil, "", &workflowctx.CallAgentOptions{
			StepID:                  fmt.Sprintf("review-apply-plan-%d", attempt),
			StepName:                fmt.Sprintf("review apply plan cycle %d", attempt),
			IncludePreviousResponse: true,
			PreviousResponseContent: loaded.ReviewOverallSummary,
		})
		plan := agentResponseContent(planResp)
		planName := fmt.Sprintf("cycle-%d-plan.md", attempt)
		writeCycleArtifact(ctx, runDir, planName, akashic.ArtifactKindContext, []byte(plan))

		cycle := CycleRecord{
			Cycle:        attempt,
			PlanArtifact: planName,
			Verdict:      VerdictUnset,
		}
		if planErr != nil {
			cycle.Verdict = VerdictFailed
			cycle.DurationMs = millisSince(cycleStartedAt)
			record.Cycles = append(record.Cycles, cycle)
			return setRecordVerdict(record, VerdictFailed,
				fmt.Sprintf("cycle %d の plan 生成に失敗しました: %v", attempt, planErr))
		}

		diffPrompt, diffSchema := RenderDiff(DiffInput{
			ReviewRunID: reviewRunID,
			Rules:       loaded.TargetRules,
			Plan:        plan,
			Attempt:     attempt,
		})
		diffResp, diffErr := agent.CallAgent(ctx, diffPrompt, cwd, nil, "", &workflowctx.CallAgentOptions{
			OutputFormat: diffSchema,
			StepID:       fmt.Sprintf("review-apply-diff-%d", attempt),
			StepName:     fmt.Sprintf("review apply diff cycle %d", attempt),
		})
		rawDiff, rawDiffErr := diffResponseJSON(diffResp)
		diffName := fmt.Sprintf("cycle-%d-diff.patch", attempt)
		cycle.DiffArtifact = diffName

		if diffErr != nil {
			writeCycleArtifact(ctx, runDir, diffName, akashic.ArtifactKindProposal, []byte(rawDiff))
			cycle.Verdict = VerdictProposalUnparseable
			cycle.DurationMs = millisSince(cycleStartedAt)
			record.Cycles = append(record.Cycles, cycle)
			return setRecordVerdict(record, VerdictProposalUnparseable,
				fmt.Sprintf("cycle %d の diff 生成に失敗しました: %v", attempt, diffErr))
		}
		if rawDiffErr != nil {
			writeCycleArtifact(ctx, runDir, diffName, akashic.ArtifactKindProposal, []byte(rawDiff))
			cycle.Verdict = VerdictProposalUnparseable
			cycle.DurationMs = millisSince(cycleStartedAt)
			record.Cycles = append(record.Cycles, cycle)
			return setRecordVerdict(record, VerdictProposalUnparseable,
				fmt.Sprintf("cycle %d の diff JSON を復元できませんでした: %v", attempt, rawDiffErr))
		}

		declaredFiles, decodeErr := decodeAppliedFiles(rawDiff)
		if decodeErr != nil {
			writeCycleArtifact(ctx, runDir, diffName, akashic.ArtifactKindProposal, []byte(rawDiff))
			cycle.Verdict = VerdictProposalUnparseable
			cycle.DurationMs = millisSince(cycleStartedAt)
			record.Cycles = append(record.Cycles, cycle)
			return setRecordVerdict(record, VerdictProposalUnparseable,
				fmt.Sprintf("cycle %d の diff JSON を decode できませんでした: %v", attempt, decodeErr))
		}

		patch, extractErr := ExtractAndGuard(rawDiff, declaredFiles, findingsFiles, cwd)
		diffArtifactBody := []byte(rawDiff)
		if extractErr == nil {
			diffArtifactBody = patch.DiffBytes
		}
		writeCycleArtifact(ctx, runDir, diffName, akashic.ArtifactKindProposal, diffArtifactBody)

		switch {
		case errors.Is(extractErr, ErrUnparseable):
			cycle.Verdict = VerdictProposalUnparseable
			cycle.DurationMs = millisSince(cycleStartedAt)
			record.Cycles = append(record.Cycles, cycle)
			return setRecordVerdict(record, VerdictProposalUnparseable,
				fmt.Sprintf("cycle %d の diff を unified diff として解釈できませんでした。", attempt))
		case errors.Is(extractErr, ErrOutOfScope):
			violating := scopeViolationPaths(rawDiff, declaredFiles, findingsFiles, cwd)
			emitWorkflowText(onEvent, fmt.Sprintf("scope violation paths: %s", strings.Join(violating, ", ")))
			cycle.Verdict = VerdictOutOfScopeDiff
			cycle.DurationMs = millisSince(cycleStartedAt)
			record.Cycles = append(record.Cycles, cycle)
			return setRecordVerdict(record, VerdictOutOfScopeDiff,
				fmt.Sprintf("cycle %d の diff が許可範囲外の path を変更しようとしました: %s", attempt, strings.Join(violating, ", ")))
		case extractErr != nil:
			cycle.Verdict = VerdictFailed
			cycle.DurationMs = millisSince(cycleStartedAt)
			record.Cycles = append(record.Cycles, cycle)
			return setRecordVerdict(record, VerdictFailed,
				fmt.Sprintf("cycle %d の diff 検証で予期しないエラーが発生しました: %v", attempt, extractErr))
		}
		cycle.AppliedFiles = patch.TargetFiles

		selfReviewPrompt, selfReviewSchema := RenderSelfReview(SelfReviewInput{
			ReviewRunID:  reviewRunID,
			Rules:        loaded.TargetRules,
			Plan:         plan,
			DiffBytes:    patch.DiffBytes,
			AppliedFiles: patch.TargetFiles,
			Attempt:      attempt,
		})
		reviewStartedAt := time.Now()
		reviewResp, reviewErr := agent.CallAgent(ctx, selfReviewPrompt, cwd, nil, "", &workflowctx.CallAgentOptions{
			OutputFormat: selfReviewSchema,
			StepID:       fmt.Sprintf("review-apply-self-review-%d", attempt),
			StepName:     fmt.Sprintf("review apply self-review cycle %d", attempt),
		})
		if reviewErr != nil {
			cycle.Verdict = VerdictFailed
			cycle.DurationMs = millisSince(cycleStartedAt)
			record.Cycles = append(record.Cycles, cycle)
			return setRecordVerdict(record, VerdictFailed,
				fmt.Sprintf("cycle %d の self-review 呼び出しに失敗しました: %v", attempt, reviewErr))
		}

		selfReview, parseErr := ParseSelfReview(reviewResp, attempt)
		if errors.Is(parseErr, ErrSelfReviewUnparseable) || errors.Is(parseErr, ErrSelfReviewSchemaViolation) {
			cycle.Verdict = VerdictFailed
			cycle.DurationMs = millisSince(cycleStartedAt)
			record.Cycles = append(record.Cycles, cycle)
			return setRecordVerdict(record, VerdictFailed,
				fmt.Sprintf("cycle %d の self-review 応答を解釈できませんでした: %v", attempt, parseErr))
		}
		if parseErr != nil {
			cycle.Verdict = VerdictFailed
			cycle.DurationMs = millisSince(cycleStartedAt)
			record.Cycles = append(record.Cycles, cycle)
			return setRecordVerdict(record, VerdictFailed,
				fmt.Sprintf("cycle %d の self-review 解析で予期しないエラーが発生しました: %v", attempt, parseErr))
		}

		selfReview.DurationMs = millisSince(reviewStartedAt)
		cycle.SelfReview = &selfReview
		reviewName := fmt.Sprintf("cycle-%d-review.json", attempt)
		reviewBody, marshalErr := json.MarshalIndent(selfReview, "", "  ")
		if marshalErr != nil {
			cycle.Verdict = VerdictFailed
			cycle.DurationMs = millisSince(cycleStartedAt)
			record.Cycles = append(record.Cycles, cycle)
			return setRecordVerdict(record, VerdictFailed,
				fmt.Sprintf("cycle %d の self-review artifact を JSON 化できませんでした: %v", attempt, marshalErr))
		}
		// cycle-N-review.json は ParseSelfReview 成功後のみ書き出す。
		// schema 違反や unparseable 応答を review artifact として残すと「有効な自己レビュー」と誤読されるため。
		writeCycleArtifact(ctx, runDir, reviewName, akashic.ArtifactKindReview, reviewBody)
		cycle.ReviewArtifact = reviewName
		cycle.DurationMs = millisSince(cycleStartedAt)

		switch selfReview.Verdict {
		case SelfReviewRegenerate:
			cycle.Verdict = VerdictUnset
			record.Cycles = append(record.Cycles, cycle)
			previousReview = &selfReview
			emitWorkflowText(onEvent, fmt.Sprintf("cycle %d regenerate: 次の cycle を開始します。reason=%s", attempt, selfReview.Reason))
			continue
		case SelfReviewAccept:
			record.Cycles = append(record.Cycles, cycle)
			cycleIndex := len(record.Cycles) - 1
			emitWorkflowText(onEvent, fmt.Sprintf("cycle %d accept: apply phase に進みます。", attempt))
			err := applyHook(ctx, cwd, applyRunID, runDir, reviewRunID, onEvent, record, loaded, patch)
			if record.FinalVerdict == VerdictUnset {
				fallbackErr := setRecordVerdict(record, VerdictFailed,
					"applyHook が final verdict を設定せずに終了しました。")
				if err == nil {
					err = fallbackErr
				}
			}
			record.Cycles[cycleIndex].Verdict = record.FinalVerdict
			return err
		default:
			cycle.Verdict = VerdictFailed
			record.Cycles = append(record.Cycles, cycle)
			return setRecordVerdict(record, VerdictFailed,
				fmt.Sprintf("cycle %d の self-review verdict が不正です: %s", attempt, selfReview.Verdict))
		}
	}

	emitWorkflowText(onEvent, "cycle_exhausted: 最大再生成回数に到達しました。")
	return setRecordVerdict(record, VerdictCycleExhausted,
		fmt.Sprintf("self-review が %d 回連続で regenerate を返したため終了しました。", MaxRegenerateAttempts))
}

func writeCycleArtifact(ctx context.Context, runDir, name, kind string, body []byte) {
	path := filepath.Join(runDir, name)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return
	}
	_ = workflow.RecordRunArtifact(ctx, kind, path)
}

func emitWorkflowText(onEvent func(workflow.WorkflowEvent), text string) {
	if onEvent != nil {
		onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: text})
	}
}

func millisSince(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}

func agentResponseContent(resp provider.AgentResponse) string {
	if resp.Content != "" {
		return resp.Content
	}
	if resp.StructuredOutput == nil {
		return ""
	}
	body, err := json.Marshal(resp.StructuredOutput)
	if err != nil {
		return ""
	}
	return string(body)
}

func diffResponseJSON(resp provider.AgentResponse) (string, error) {
	if resp.StructuredOutput != nil {
		body, err := json.Marshal(resp.StructuredOutput)
		if err == nil {
			return string(body), nil
		}
	}
	if strings.TrimSpace(resp.Content) != "" {
		return resp.Content, nil
	}
	return "", nil
}

func decodeAppliedFiles(rawDiff string) ([]string, error) {
	var payload patchAgentPayload
	if err := json.Unmarshal([]byte(strings.TrimSpace(rawDiff)), &payload); err != nil {
		return nil, err
	}
	return payload.AppliedFiles, nil
}

func scopeViolationPaths(rawDiff string, declaredFiles []string, findingsFiles []string, cwd string) []string {
	findings := make(map[string]struct{}, len(findingsFiles))
	for _, f := range findingsFiles {
		findings[filepath.Clean(f)] = struct{}{}
	}
	seen := make(map[string]struct{})
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	check := func(raw string) {
		cleaned, ok := validateRelativePath(raw, cwd)
		if !ok {
			add(raw)
			return
		}
		if _, ok := findings[cleaned]; !ok {
			add(cleaned)
		}
	}
	for _, f := range declaredFiles {
		check(f)
	}
	var payload patchAgentPayload
	if err := json.Unmarshal([]byte(strings.TrimSpace(rawDiff)), &payload); err == nil {
		for _, m := range plusPathRegex.FindAllStringSubmatch(payload.Diff, -1) {
			if len(m) > 1 {
				check(strings.TrimRight(m[1], " \t\r"))
			}
		}
	}
	if len(out) == 0 {
		add("(path を特定できませんでした)")
	}
	return out
}
