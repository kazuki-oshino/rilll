# ワークフロー

現在のワークフローは`internal/workflow/builtin/registry.go`で登録されています。

- **git commit** — `/git-commit`スキルを実行し、結果を`.rilll/git-commit/`にJSON形式で記録
- **kiro spec loop** — `.kiro/specs/`配下のbriefからspec対象を選び、requirements / design / tasksを生成
- **kiro impl loop** — `.kiro/specs/`配下のspec定義と`tasks.md`から未完了タスクを読み込み、対話的に実装対象を選択して実装ループを実行
- **kiro spec impl loop** — spec作成対象は`kiro-spec-loop`完了後に同一featureを`kiro-impl-loop`へ渡し、実装可能な既存specは`kiro-spec-loop`をスキップして`kiro-impl-loop`から開始する
- **kiro spec impl loop sequence** — TUIまたは`--features a,b,c`で指定した順に既存`kiro-spec-impl-loop`を1件ずつ実行し、途中失敗時は後続featureを起動しない
- **README更新 → git commit** — 提案作成→レビュー→反映の3ステップでREADMEを更新したあと、続けて git commit ワークフローを実行する複合ワークフロー

ワークフローの実行ログや中間成果物は`.rilll/`配下に保存されます。

`kiro-spec-impl-loop-sequence` は親runの状態を `.rilll/kiro-spec-impl-loop-sequence/<runID>/sequence.json` に保存します。`selectedFeatures` はユーザー指定順のまま保持され、`currentIndex` / `currentFeature` / 各child runのoutcome / fail-fast時の未実行tailを後追いできます。

## ワークフローの追加

`internal/workflow`に新しいGoファイルを追加し、`Definition`を返す関数を定義します。

```go
package workflow

import "context"

func MyWorkflow(agent AgentCaller) Definition {
	return Definition{
		ID:    "my-workflow",
		Label: "マイワークフロー",
		Fn: func(ctx context.Context, cwd string, onEvent func(WorkflowEvent), ask WorkflowAsk) error {
			onEvent(WorkflowEvent{Type: WorkflowStep, Step: 1, TotalSteps: 1, Label: "タスク実行"})

			result, err := agent.CallAgent(ctx, "プロンプト", cwd, forwardEvents(onEvent), "", nil)
			if err != nil {
				return err
			}
			if !result.Success {
				onEvent(WorkflowEvent{Type: WorkflowError, Message: "タスクに失敗しました"})
				return nil
			}

			onEvent(WorkflowEvent{Type: WorkflowDone, Result: result.Content})
			return nil
		},
	}
}
```

作成したワークフローは`internal/workflow/builtin/registry.go`の`Registry`に追加します。

```go
func Registry(agent AgentCaller) []Definition {
	return []Definition{
		UpdateReadme(agent),
		GitCommit(agent),
		KiroImplLoop(agent),
		UpdateReadmeAndCommit(agent),
		MyWorkflow(agent), // 自作ワークフローはここに追加
	}
}
```

## 複合ワークフロー

複数のワークフローを単に順番に実行したい場合は、`internal/workflow/sequential.go` の `Sequential` ヘルパーで既存 `Definition` を連鎖できます。子のいずれかが `error` を返すか `WorkflowError` イベントを発した時点で後続はスキップされます。

```go
package workflow

func MyComposite(agent AgentCaller) Definition {
	return Sequential(
		"my-composite",
		"ワークフローA → ワークフローB",
		agent,
		WorkflowA(agent),
		WorkflowB(agent),
	)
}
```

`internal/workflow/update_readme_and_commit.go` で実際に使われています。

段間で typed payload を渡す場合は、`internal/workflow/sequential_io.go` の `BuildSequentialIO` を使います。`Start` / `Then` / `Step[I, O]` で compile-time に payload 型を接続し、最後は通常の `Definition` として返すため、registry / CLI / TUI の非 generic surface は変わりません。child の runID や first `WorkflowError`、bounded event capture は `ChildResult` から取得し、domain-specific な抽出・注入は consumer package 側の `Extract` / `Deliver` callback に閉じます。

```go
func MyTypedComposite(agent workflowctx.AgentCaller) workflow.Definition {
	pipeline := workflow.Then(
		workflow.Start(workflow.Step[workflow.NoInput, firstOutput]{
			Definition: FirstWorkflow(agent),
			Extract: func(ctx context.Context, cwd string, result workflow.ChildResult) (firstOutput, error) {
				return firstOutput{RunID: result.RunID}, nil
			},
		}),
		func(output firstOutput) (secondInput, error) {
			return secondInput{ParentRunID: output.RunID}, nil
		},
		workflow.Step[secondInput, workflow.NoOutput]{
			Definition: SecondWorkflow(agent),
			Deliver: func(ctx context.Context, cwd string, ask workflow.WorkflowAsk, input secondInput) (context.Context, workflow.WorkflowAsk, error) {
				return ContextWithParentRunID(ctx, input.ParentRunID), ask, nil
			},
		},
	)

	return workflow.BuildSequentialIO(workflow.DefinitionConfig{
		ID:       "my-typed-composite",
		Label:    "型付き複合ワークフロー",
		Recorder: workflow.RecorderFromAgent(agent),
	}, pipeline)
}
```

`internal/workflow/builtin/kiro/spec_impl_loop.go` は target の状態で分岐します。spec作成対象は `kiro-spec-loop` の completed result artifact を gate して同一 feature を `kiro-impl-loop` へ渡し、実装可能な既存specは `kiro-spec-loop` を起動せず `kiro-impl-loop` の fixed feature selection へ直接渡します。

動的な件数のchild workflowをユーザー指定順に実行する場合は、staticな`BuildSequentialIO` pipelineではなく親workflow側で`workflow.RunChild`を1件ずつ呼びます。`internal/workflow/builtin/kiro/spec_impl_loop_sequence.go` はこの形で、受理済みのfeature順序をsortせず、childの`feature`選択だけを固定するask wrapperを差し込んでいます。
