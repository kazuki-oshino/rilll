package workflow

import (
	"context"
	"errors"
	"fmt"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

// NoInput は先頭 step や payload 不要の接続を表す marker 型。
type NoInput struct{}

// NoOutput は後段へ渡す payload が不要な step output を表す marker 型。
type NoOutput struct{}

// DefinitionConfig は typed pipeline から生成する Definition の public metadata を明示する。
type DefinitionConfig struct {
	ID       string
	Label    string
	Args     []ArgSpec
	Hidden   bool
	Recorder akashic.Recorder
}

// InputDelivery は typed input を child workflow の ctx / ask surface に注入する。
type InputDelivery[I any] func(
	ctx context.Context,
	cwd string,
	ask WorkflowAsk,
	input I,
) (context.Context, WorkflowAsk, error)

// OutputExtractor は child completion result から typed output を抽出する。
type OutputExtractor[O any] func(
	ctx context.Context,
	cwd string,
	result ChildResult,
) (O, error)

// Transform は前段 output を後段 input へ変換する。
type Transform[I, O any] func(I) (O, error)

// Step は child Definition と typed input / output callback の境界を表す。
type Step[I, O any] struct {
	Definition Definition
	Deliver    InputDelivery[I]
	Extract    OutputExtractor[O]
	Capture    EventCapturePolicy
}

// Pipeline は現在の chain output 型を保持する typed builder state。
type Pipeline[O any] struct {
	stages []sequentialIOStage
}

type sequentialIOStage struct {
	definition            Definition
	capture               EventCapturePolicy
	deliver               func(context.Context, string, WorkflowAsk, any) (context.Context, WorkflowAsk, error)
	extract               func(context.Context, string, ChildResult) (any, error)
	transformFromPrevious func(any) (any, error)
}

// Start は NoInput を受ける first step から typed pipeline を開始する。
func Start[O any](first Step[NoInput, O]) Pipeline[O] {
	return Pipeline[O]{
		stages: []sequentialIOStage{
			newSequentialIOStage(first, nil),
		},
	}
}

// Then は前段 output、transform、後段 input の型整合を compile-time に検証して step を接続する。
func Then[Prev, NextIn, NextOut any](
	pipeline Pipeline[Prev],
	transform Transform[Prev, NextIn],
	next Step[NextIn, NextOut],
) Pipeline[NextOut] {
	stages := make([]sequentialIOStage, 0, len(pipeline.stages)+1)
	stages = append(stages, pipeline.stages...)
	stages = append(stages, newSequentialIOStage(next, func(previous any) (NextIn, error) {
		var zero NextIn
		if transform == nil {
			return zero, nil
		}
		typedPrevious, ok := previous.(Prev)
		if !ok {
			return zero, fmt.Errorf("sequential IO internal type mismatch: previous output has type %T", previous)
		}
		return transform(typedPrevious)
	}))
	return Pipeline[NextOut]{stages: stages}
}

func newSequentialIOStage[I, O any](step Step[I, O], transformFromPrevious func(any) (I, error)) sequentialIOStage {
	return sequentialIOStage{
		definition:            step.Definition,
		capture:               step.Capture,
		deliver:               eraseSequentialIODeliver(step.Deliver),
		extract:               eraseSequentialIOExtract(step.Extract),
		transformFromPrevious: eraseSequentialIOTransform(transformFromPrevious),
	}
}

func eraseSequentialIODeliver[I any](deliver InputDelivery[I]) func(context.Context, string, WorkflowAsk, any) (context.Context, WorkflowAsk, error) {
	return func(ctx context.Context, cwd string, ask WorkflowAsk, input any) (context.Context, WorkflowAsk, error) {
		if deliver == nil {
			return ctx, ask, nil
		}
		typedInput, ok := input.(I)
		if !ok {
			return ctx, ask, fmt.Errorf("sequential IO internal type mismatch: input has type %T", input)
		}
		return deliver(ctx, cwd, ask, typedInput)
	}
}

func eraseSequentialIOExtract[O any](extract OutputExtractor[O]) func(context.Context, string, ChildResult) (any, error) {
	return func(ctx context.Context, cwd string, result ChildResult) (any, error) {
		if extract == nil {
			var zero O
			return zero, nil
		}
		return extract(ctx, cwd, result)
	}
}

func eraseSequentialIOTransform[I any](transform func(any) (I, error)) func(any) (any, error) {
	if transform == nil {
		return nil
	}
	return func(input any) (any, error) {
		return transform(input)
	}
}

// BuildSequentialIO は typed pipeline を registry / CLI / TUI 互換の通常 Definition として公開する。
func BuildSequentialIO[O any](config DefinitionConfig, pipeline Pipeline[O]) Definition {
	return Definition{
		ID:       config.ID,
		Label:    config.Label,
		Args:     config.Args,
		Hidden:   config.Hidden,
		Recorder: config.Recorder,
		Fn: func(ctx context.Context, cwd string, onEvent func(WorkflowEvent), ask WorkflowAsk) error {
			if err := validateSequentialIODefinitionMetadata(config, pipeline); err != nil {
				return emitSequentialIOWorkflowError(onEvent, err.Error())
			}
			return runSequentialIOPipeline(ctx, cwd, onEvent, ask, pipeline)
		},
	}
}

func runSequentialIOPipeline[O any](
	ctx context.Context,
	cwd string,
	onEvent func(WorkflowEvent),
	ask WorkflowAsk,
	pipeline Pipeline[O],
) error {
	var previousOutput any
	for i, stage := range pipeline.stages {
		input := any(NoInput{})
		if i > 0 {
			transformed, err := stage.transformFromPrevious(previousOutput)
			if err != nil {
				return emitSequentialIOPipelineFailure(onEvent, stage.definition.Label, "typed transform", err)
			}
			input = transformed
		}

		childCtx, childAsk, err := stage.deliver(ctx, cwd, ask, input)
		if err != nil {
			return emitSequentialIOPipelineFailure(onEvent, stage.definition.Label, "input delivery", err)
		}
		result, err := RunChild(childCtx, cwd, stage.definition, onEvent, childAsk, ChildRunOptions{
			Capture: stage.capture,
		})
		if err != nil {
			return err
		}
		output, err := stage.extract(childCtx, cwd, result)
		if err != nil {
			return emitSequentialIOPipelineFailure(onEvent, stage.definition.Label, "output extraction", err)
		}
		previousOutput = output
	}
	return nil
}

func validateSequentialIODefinitionMetadata[O any](config DefinitionConfig, pipeline Pipeline[O]) error {
	if config.ID == "" {
		return errors.New("sequential IO definition metadata invalid: id is empty")
	}
	if config.Label == "" {
		return errors.New("sequential IO definition metadata invalid: label is empty")
	}
	for i, stage := range pipeline.stages {
		stageNumber := i + 1
		if stage.definition.ID == "" {
			return fmt.Errorf("sequential IO child metadata invalid: stage %d id is empty", stageNumber)
		}
		if stage.definition.Label == "" {
			return fmt.Errorf("sequential IO child metadata invalid: stage %d label is empty", stageNumber)
		}
	}
	return nil
}

func emitSequentialIOWorkflowError(onEvent func(WorkflowEvent), message string) error {
	if onEvent != nil {
		onEvent(WorkflowEvent{Type: WorkflowError, Message: message})
	}
	return errors.New(message)
}

func emitSequentialIOPipelineFailure(onEvent func(WorkflowEvent), stageLabel string, failureKind string, cause error) error {
	err := fmt.Errorf("sequential IO stage %q %s failed: %w", stageLabel, failureKind, cause)
	if onEvent != nil {
		onEvent(WorkflowEvent{Type: WorkflowError, Message: err.Error()})
	}
	return err
}
