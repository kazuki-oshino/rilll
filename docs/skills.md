# ワークフローが利用するスキル

rilllは、利用先プロジェクトのエージェント環境に配置されたスキルを名前で呼び出します。
Kiro系の手順は外部の [cc-sdd](https://github.com/gotalab/cc-sdd)、Rill独自の手順は本リポジトリの [.agents/skills/](../.agents/skills/) が提供します。
バイナリのビルドや `npm ci` だけでは、利用先へのスキル配置は行われません。
ソースの閲覧、ビルド、模擬Providerを使うテストにはcc-sddの導入は不要です。

## cc-sddを利用先に導入する

Kiro系ワークフローを実行する場合は、対象の作業リポジトリでcc-sddの **Skillsモード** を導入する前提です。
使用するエージェントに対応するコマンドを選んでください。
以下は [公式README](https://github.com/gotalab/cc-sdd#quick-start) に基づく導入例です。

```sh
# 対象の作業リポジトリで、使用するエージェントのものを選択
npx cc-sdd@latest --claude-skills --lang ja
npx cc-sdd@latest --codex-skills --lang ja
npx cc-sdd@latest --cursor-skills --lang ja
```

導入先にはスキルや `.kiro/settings/` などの設定が生成されます。
変更対象を先に確認する場合は、公式の `--dry-run` オプションを利用してください。
rilll自身の公開リポジトリには `kiro-*` のスキル本文やcc-sddの設定テンプレートを同梱していません。
旧形式の `/kiro:*` コマンドではなく、`kiro-*` のスキル名を使います。
呼び出し形式と対応環境は [公式スキルリファレンス](https://github.com/gotalab/cc-sdd/blob/main/docs/guides/ja/skill-reference.md) を参照してください。

| rilllの機能 | 参照する外部スキル |
| --- | --- |
| `kiro-spec-loop` | `kiro-spec-init`、`kiro-spec-requirements`、`kiro-validate-gap`、`kiro-spec-design`、`kiro-validate-design`、`kiro-spec-tasks` |
| `kiro-impl-loop` | `kiro-impl`、`kiro-validate-impl` |
| `kiro-spec-impl-loop` / `kiro-spec-impl-loop-sequence` | 上記の仕様作成と実装スキル |

rilllはGoからSDK経由で `/kiro-*` を含むプロンプトを送り、エージェントが配置済みの手順を参照します。
Codex bridgeでは、先頭の `/skill-name` を `$skill-name` に変換します。
cc-sddをGoからimportしたり、npmライブラリとして直接呼び出したりする構成ではありません。

## rilll側の連携契約

rilllはスキル呼び出しに [runner directive](../internal/inquiryloop/prompt.go) と [出力スキーマ](../internal/inquiryloop/schema.go) を追加します。
`has_inquiry`、`summary`、必要に応じた `inquiry` や `verdict` を受け取り、確認、再開、検証、修復の工程を制御します。
Kiroの作業手順はcc-sdd、Provider間の接続と工程管理はrilll、という責務分担です。

スキル名の対応だけで、最新cc-sddとすべてのProviderの実行互換性が確認できるわけではありません。
この公開版ではcc-sdd導入後のライブE2Eは実施していません。
特にcc-sddのCursor向け導入はCursor IDE用であり、rilllのCursor SDK / CLI経路でのスキル探索まで確認したものではありません。
再現実行する場合は、利用先でのスキル解決とrilllの構造化応答契約を確認してください。

## 同梱する独自スキル

Kiro系以外の5スキルは、本リポジトリに保持しています。
必要なものを、利用先のエージェントが参照できる場所へディレクトリごと配置してください。
`rules/`、`templates/`、`references/` も手順の一部です。

| スキル | 用途 |
| --- | --- |
| `agent-learnings-curator` | 学習ワークフローがReflector / Curatorモードで呼び出す手順 |
| `agent-learnings-consolidator` | 同じカテゴリ内のトピックを手動で集約し、playbookの索引を更新する手順 |
| `review-rules-skill` | 利用先の `.rilll-config/review-rules/` を作成する手順 |
| `akashic-investigator` | rilllの実行記録を調査する手順 |
| `git-commit` | コミット工程が呼び出す公開版用の最小手順 |

Claude向けには利用先の `.claude/skills/`、Codex向けには `.agents/skills/` へ配置する構成です。
Cursor側は利用するSDK / CLIのスキル探索設定に従ってください。
公開版の `.claude` symlinkや開発用 `AGENTS.md` / `CLAUDE.md` は同梱していません。
独自スキル内で参照する規約や仕様は、利用先プロジェクトのものです。

`git-commit`、`update-readme-and-commit`、コミット工程を含むKiro系ワークフローには、独自のコミットスキルも必要です。
cc-sddの導入だけでは、これらの独自スキルは配置されません。
レビューを実行する場合は、`review-rules-skill` などで利用先の `.rilll-config/review-rules/` に有効な規則を用意してください。
この公開版にはrilll自身を対象としたプロジェクト固有の `.rilll-config/` は同梱していません。
`update-readme` 自体のプロンプトはGoの実装に含まれています。

[review-rules-lint](../tools/review-rules-lint/) と学習スキーマのテストは、同梱する独自スキルを直接読み、契約の整合を検査します。
cc-sdd側のスキル本文は、このリポジトリのテスト対象には含めていません。

## ライセンスと出典

rilllのコードと同梱する独自スキルのライセンスは [MIT](../LICENSE) です。
cc-sddはgotalabが公開する別プロジェクトで、[MIT License](https://github.com/gotalab/cc-sdd/blob/main/LICENSE) です。
この公開版では、cc-sddを外部スキルの提供元として案内し、そのスキル本文や設定テンプレートを再配布する構成にはしていません。
利用先に導入したcc-sddのファイルは、上流のライセンスと著作権表示に従って扱ってください。
