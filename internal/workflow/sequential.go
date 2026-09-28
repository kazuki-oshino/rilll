package workflow

import (
	"context"
	"errors"

	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// Sequential は渡された Definition を順番に実行する複合 Definition を返す。
//
// 子の失敗判定は二段構え:
//  1. 子の Run() が non-nil な error を返した場合
//  2. 子が onEvent で WorkflowError イベントを発した場合
//     （既存ワークフローは失敗時に WorkflowError を出して return nil する設計のため）
//
// いずれかの条件で失敗を検知した時点で、後続の子は実行せず error を返す。
// 子のイベントは親の onEvent にそのまま転送される。
func Sequential(id, label string, agent workflowctx.AgentCaller, defs ...Definition) Definition {
	return Definition{
		ID:       id,
		Label:    label,
		Recorder: RecorderFromAgent(agent),
		Fn: func(ctx context.Context, cwd string, onEvent func(WorkflowEvent), ask WorkflowAsk) error {
			for _, def := range defs {
				if onEvent != nil {
					onEvent(WorkflowEvent{Type: WorkflowActive, Label: def.Label})
				}
				if err := runChildSequentially(ctx, cwd, def, onEvent, ask); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// runChildSequentially は子の Definition を Run で実行し、エラー（戻り値 / WorkflowError event）を検知する。
func runChildSequentially(ctx context.Context, cwd string, def Definition, onEvent func(WorkflowEvent), ask WorkflowAsk) error {
	var firstError string
	wrapped := func(event WorkflowEvent) {
		if event.Type == WorkflowError && firstError == "" {
			firstError = event.Message
		}
		if onEvent != nil {
			onEvent(event)
		}
	}
	if err := def.Run(ctx, cwd, wrapped, ask); err != nil {
		return err
	}
	if firstError != "" {
		return errors.New(firstError)
	}
	return nil
}
