package kiro

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

const (
	kiroSpecLoopID        = "kiro-spec-loop"
	kiroSpecImplLoopID    = "kiro-spec-impl-loop"
	kiroSpecImplLoopLabel = "Kiro 仕様作成・実装ループ"
)

func KiroSpecImplLoop(agent workflowctx.AgentCaller) workflow.Definition {
	specDef := KiroSpecLoop(agent)
	implDef := KiroImplLoop(agent)
	return buildKiroSpecImplLoop(agent, specDef, implDef)
}

func buildKiroSpecImplLoop(agent workflowctx.AgentCaller, specDef workflow.Definition, implDef workflow.Definition) workflow.Definition {
	runner := specImplLoopRunner{
		specDef: specDef,
		implDef: implDef,
	}
	return workflow.Definition{
		ID:    kiroSpecImplLoopID,
		Label: kiroSpecImplLoopLabel,
		Args: []workflow.ArgSpec{
			{Key: "feature", Description: ".kiro/specs/<feature>/ 配下の対象 feature 名（spec 作成対象または実装対象）", Required: true},
		},
		Hidden:   false,
		Recorder: workflow.RecorderFromAgent(agent),
		Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			return runner.run(ctx, cwd, onEvent, ask)
		},
	}
}

type specImplLoopTargetMode string

const (
	specImplLoopTargetModeSpecThenImpl specImplLoopTargetMode = "spec-then-impl"
	specImplLoopTargetModeImplOnly     specImplLoopTargetMode = "impl-only"
)

type specImplLoopTarget struct {
	Feature string
	Mode    specImplLoopTargetMode
}

type specImplLoopRunner struct {
	specDef workflow.Definition
	implDef workflow.Definition
}

func (r specImplLoopRunner) run(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
	target, ok, err := r.selectTarget(ctx, cwd, onEvent, ask)
	if err != nil || !ok {
		return err
	}

	switch target.Mode {
	case specImplLoopTargetModeImplOnly:
		return r.runImplOnly(ctx, cwd, onEvent, ask, target)
	case specImplLoopTargetModeSpecThenImpl:
		return r.runSpecThenImpl(ctx, cwd, onEvent, ask, target)
	default:
		return emitSpecImplLoopWorkflowError(onEvent, fmt.Sprintf("kiro-spec-impl-loop target mode %q is unsupported for feature %q", target.Mode, target.Feature))
	}
}

func (r specImplLoopRunner) selectTarget(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) (specImplLoopTarget, bool, error) {
	targets, err := listSpecImplLoopTargets(cwd)
	if err != nil {
		return specImplLoopTarget{}, false, err
	}
	if len(targets) == 0 {
		msg := "対象 feature が見つかりません: spec 作成対象（brief.md あり・phase < tasks-generated）または実装対象（tasks.md 未完了あり・spec.json 承認済み）の feature を準備してください"
		_ = emitSpecImplLoopWorkflowError(onEvent, msg)
		return specImplLoopTarget{}, false, nil
	}
	if ask == nil {
		_ = emitSpecImplLoopWorkflowError(onEvent, "対象 feature を選択する手段が利用できません（workflow.WorkflowAsk が nil）")
		return specImplLoopTarget{}, false, nil
	}

	feature, ok, err := ask.Select(ctx, workflow.SelectParams{
		Key:    "feature",
		Prompt: "対象のfeatureを選択してください（ESCでキャンセル）:",
		Items:  specImplLoopTargetSelectItems(targets),
	})
	if err != nil {
		return specImplLoopTarget{}, false, err
	}
	if !ok {
		_ = emitSpecImplLoopWorkflowError(onEvent, "feature 選択がキャンセルされました")
		return specImplLoopTarget{}, false, nil
	}

	target, found := findSpecImplLoopTarget(targets, feature)
	if !found {
		return specImplLoopTarget{}, false, fmt.Errorf("kiro-spec-impl-loop selected unknown feature %q", feature)
	}
	return target, true, nil
}

func (r specImplLoopRunner) runSpecThenImpl(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk, target specImplLoopTarget) error {
	specAsk := fixedFeatureAsk{feature: target.Feature, delegate: ask, selectionLabel: "spec"}
	specResult, err := workflow.RunChild(ctx, cwd, r.specDef, onEvent, specAsk, workflow.ChildRunOptions{})
	if err != nil {
		return err
	}

	output, err := extractKiroSpecLoopOutput(ctx, cwd, specResult)
	if err != nil {
		return emitSpecImplLoopStageFailure(onEvent, r.specDef.Label, "output extraction", err)
	}
	input, err := toImplLoopInput(output)
	if err != nil {
		return emitSpecImplLoopStageFailure(onEvent, r.implDef.Label, "typed transform", err)
	}
	return r.runImplChild(ctx, cwd, onEvent, ask, input)
}

func (r specImplLoopRunner) runImplOnly(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk, target specImplLoopTarget) error {
	return r.runImplChild(ctx, cwd, onEvent, ask, implLoopInput{Feature: target.Feature})
}

func (r specImplLoopRunner) runImplChild(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk, input implLoopInput) error {
	childCtx, childAsk, err := deliverKiroImplFeature(ctx, cwd, ask, input)
	if err != nil {
		return emitSpecImplLoopStageFailure(onEvent, r.implDef.Label, "input delivery", err)
	}
	_, err = workflow.RunChild(childCtx, cwd, r.implDef, onEvent, childAsk, workflow.ChildRunOptions{})
	return err
}

func listSpecImplLoopTargets(cwd string) ([]specImplLoopTarget, error) {
	modes := map[string]specImplLoopTargetMode{}

	specEligible, err := listEligibleFeatures(cwd)
	if err != nil {
		return nil, err
	}
	for _, feature := range specEligible {
		modes[feature] = specImplLoopTargetModeSpecThenImpl
	}

	implementable, err := listImplementableFeatures(cwd)
	if err != nil {
		return nil, err
	}
	for _, feature := range implementable {
		modes[feature] = specImplLoopTargetModeImplOnly
	}

	features := make([]string, 0, len(modes))
	for feature := range modes {
		features = append(features, feature)
	}
	sort.Strings(features)

	targets := make([]specImplLoopTarget, 0, len(features))
	for _, feature := range features {
		targets = append(targets, specImplLoopTarget{
			Feature: feature,
			Mode:    modes[feature],
		})
	}
	return targets, nil
}

func specImplLoopTargetSelectItems(targets []specImplLoopTarget) []workflow.SelectItem {
	items := make([]workflow.SelectItem, 0, len(targets))
	for _, target := range targets {
		items = append(items, workflow.SelectItem{
			ID:    target.Feature,
			Label: specImplLoopTargetLabel(target),
		})
	}
	return items
}

func specImplLoopTargetLabel(target specImplLoopTarget) string {
	switch target.Mode {
	case specImplLoopTargetModeImplOnly:
		return target.Feature + "（実装のみ）"
	case specImplLoopTargetModeSpecThenImpl:
		return target.Feature + "（仕様作成→実装）"
	default:
		return target.Feature
	}
}

func findSpecImplLoopTarget(targets []specImplLoopTarget, feature string) (specImplLoopTarget, bool) {
	for _, target := range targets {
		if target.Feature == feature {
			return target, true
		}
	}
	return specImplLoopTarget{}, false
}

func specImplLoopTargetFeatures(targets []specImplLoopTarget) []string {
	features := make([]string, 0, len(targets))
	for _, target := range targets {
		features = append(features, target.Feature)
	}
	return features
}

func emitSpecImplLoopWorkflowError(onEvent func(workflow.WorkflowEvent), message string) error {
	if onEvent != nil {
		onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: message})
	}
	return fmt.Errorf("%s", message)
}

func emitSpecImplLoopStageFailure(onEvent func(workflow.WorkflowEvent), stageLabel string, failureKind string, cause error) error {
	err := fmt.Errorf("kiro-spec-impl-loop stage %q %s failed: %w", stageLabel, failureKind, cause)
	if onEvent != nil {
		onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: err.Error()})
	}
	return err
}

type specLoopOutput struct {
	Feature    string
	SpecRunID  string
	ResultPath string
}

type implLoopInput struct {
	Feature    string
	SpecRunID  string
	ResultPath string
}

func extractKiroSpecLoopOutput(_ context.Context, cwd string, result workflow.ChildResult) (specLoopOutput, error) {
	runID := strings.TrimSpace(result.RunID)
	resultPath := filepath.Join(cwd, ".rilll", kiroSpecLoopID, runID, "result.json")
	if runID == "" {
		return specLoopOutput{}, formatSpecLoopResultGateError(resultPath, runID, RunResult{}, "child runID is empty")
	}

	raw, err := os.ReadFile(resultPath)
	if err != nil {
		return specLoopOutput{}, formatSpecLoopResultGateError(resultPath, runID, RunResult{}, fmt.Sprintf("read result.json: %v", err))
	}

	var runResult RunResult
	if err := json.Unmarshal(raw, &runResult); err != nil {
		return specLoopOutput{}, formatSpecLoopResultGateError(resultPath, runID, runResult, fmt.Sprintf("decode result.json: %v", err))
	}
	if runResult.WorkflowID != kiroSpecLoopID {
		return specLoopOutput{}, formatSpecLoopResultGateError(resultPath, runID, runResult, fmt.Sprintf("workflowId=%q, want %q", runResult.WorkflowID, kiroSpecLoopID))
	}
	if runResult.RunID != runID {
		return specLoopOutput{}, formatSpecLoopResultGateError(resultPath, runID, runResult, fmt.Sprintf("runId=%q, want child runID %q", runResult.RunID, runID))
	}
	feature := strings.TrimSpace(runResult.Feature)
	if feature == "" {
		return specLoopOutput{}, formatSpecLoopResultGateError(resultPath, runID, runResult, "feature is empty")
	}
	if runResult.TerminationReason != RunReasonCompleted {
		return specLoopOutput{}, formatSpecLoopResultGateError(resultPath, runID, runResult, fmt.Sprintf("terminationReason=%q, want %q", runResult.TerminationReason, RunReasonCompleted))
	}

	return specLoopOutput{
		Feature:    feature,
		SpecRunID:  runID,
		ResultPath: resultPath,
	}, nil
}

func toImplLoopInput(output specLoopOutput) (implLoopInput, error) {
	feature := strings.TrimSpace(output.Feature)
	if feature == "" {
		return implLoopInput{}, fmt.Errorf("kiro-spec-impl-loop hand-off transform failed: feature is empty (specRunID=%s resultPath=%s)", output.SpecRunID, output.ResultPath)
	}
	return implLoopInput{
		Feature:    feature,
		SpecRunID:  output.SpecRunID,
		ResultPath: output.ResultPath,
	}, nil
}

func deliverKiroImplFeature(ctx context.Context, cwd string, ask workflow.WorkflowAsk, input implLoopInput) (context.Context, workflow.WorkflowAsk, error) {
	feature := strings.TrimSpace(input.Feature)
	if feature == "" {
		return ctx, ask, fmt.Errorf("kiro-spec-impl-loop impl delivery failed: feature is empty (specRunID=%s resultPath=%s)", input.SpecRunID, input.ResultPath)
	}

	features, err := listImplementableFeatures(cwd)
	if err != nil {
		return ctx, ask, fmt.Errorf("kiro-spec-impl-loop impl delivery failed: feature=%q specRunID=%s resultPath=%s: %w", feature, input.SpecRunID, input.ResultPath, err)
	}
	if !featureListContains(features, feature) {
		return ctx, ask, fmt.Errorf("kiro-spec-impl-loop impl delivery failed: feature=%q from %s is not implementable (available: %s); no fallback to another feature", feature, formatImplLoopInputSource(input), strings.Join(features, ", "))
	}

	return ctx, fixedFeatureAsk{feature: feature, delegate: ask, selectionLabel: "impl"}, nil
}

type fixedFeatureAsk struct {
	feature        string
	delegate       workflow.WorkflowAsk
	selectionLabel string
}

func (a fixedFeatureAsk) Select(ctx context.Context, params workflow.SelectParams) (string, bool, error) {
	if params.Key == "feature" {
		if !selectParamsContainFeature(params.Items, a.feature) {
			return "", false, fmt.Errorf("kiro-spec-impl-loop fixed feature ask failed: feature=%q is not present in %s selection items", a.feature, a.selectionLabelOrDefault())
		}
		return a.feature, true, nil
	}
	if a.delegate == nil {
		return "", false, fmt.Errorf("kiro-spec-impl-loop fixed feature ask cannot delegate selection key %q", params.Key)
	}
	return a.delegate.Select(ctx, params)
}

func (a fixedFeatureAsk) selectionLabelOrDefault() string {
	if a.selectionLabel != "" {
		return a.selectionLabel
	}
	return "impl"
}

func formatImplLoopInputSource(input implLoopInput) string {
	if strings.TrimSpace(input.SpecRunID) == "" && strings.TrimSpace(input.ResultPath) == "" {
		return "direct impl target"
	}
	return fmt.Sprintf("specRunID=%s resultPath=%s", input.SpecRunID, input.ResultPath)
}

func formatSpecLoopResultGateError(resultPath string, runID string, result RunResult, reason string) error {
	return fmt.Errorf(
		"kiro-spec-impl-loop spec result gate failed: path=%s specRunID=%s workflowId=%q runId=%q feature=%q terminationReason=%q: %s",
		resultPath,
		runID,
		result.WorkflowID,
		result.RunID,
		result.Feature,
		result.TerminationReason,
		reason,
	)
}

func featureListContains(features []string, feature string) bool {
	for _, candidate := range features {
		if candidate == feature {
			return true
		}
	}
	return false
}

func selectParamsContainFeature(items []workflow.SelectItem, feature string) bool {
	for _, item := range items {
		if item.ID == feature {
			return true
		}
	}
	return false
}
