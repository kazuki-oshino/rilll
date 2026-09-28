---
name: git-commit
description: Commit the changes authorized by the current task. Use when the user requests a commit or a rilll workflow reaches its authorized commit step.
---

# Git commit

現在のタスクで許可された変更を、現在のブランチにコミットする。
このskillを読むだけではコミットの許可にならない。
ユーザーがコミットを依頼した場合、またはコミット工程を含むワークフローの実行を依頼した場合に実行する。

## 対象の確認

`git status --short`、`git diff`、`git diff --cached` で作業ツリーとステージ済み差分を確認する。
未追跡ファイルも対象なら内容を確認する。
タスクの目的や前段から渡された成果物に対応する差分を選び、無関係な変更を混ぜない。
別の作業のステージ済み変更があり安全に分離できない場合や、対象範囲が特定できない場合は、コミットせず理由を報告する。

認証情報、実行ログ、ローカル設定、生成されたビルド成果物を含めない。
変更がなければ「変更なし」と報告して終了する。

## コミットと確認

対象ファイルを明示してステージし、`git diff --cached` で最終差分を確認する。
変更内容と目的が伝わる短いコミットメッセージを作成する。
必要な補足は本文に記述し、複数行のメッセージは一時ファイルを作って `git commit -F` に渡す。
目的が独立した変更は別コミットに分ける。

コミット後に `git log -1 --oneline` と `git status --short` を確認し、コミットID、要旨、残った差分を報告する。
失敗した場合は原因を報告し、hookを無効化したり、成功したと見なして後段へ進んだりしない。
この工程にはpush、PR作成、amend、resetなどの履歴変更を含めない。
