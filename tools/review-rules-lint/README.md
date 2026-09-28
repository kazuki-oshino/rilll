# review-rules-lint

同梱する [review-rules-skill](../../.agents/skills/review-rules-skill/SKILL.md) の手順、テンプレート、参照資料を検査するGoテスト。
レビュー規則の [型定義](../../internal/workflow/builtin/review/rules/types.go) やProviderの定義と比較し、スキルが生成するYAMLと実装の不整合を検出する。

## 実行方法

リポジトリのルートから実行する。

```sh
go test ./tools/review-rules-lint
```

`go test ./...`、`just test`、`just check` にも含まれる。
AI Providerへの接続は不要で、スキルのファイルを読み取るだけで変更しない。
利用先へスキルをコピーするとき、このテストパッケージを同梱する必要はない。

## 検査する内容

| テスト | 対象 |
| --- | --- |
| [lint_test.go](lint_test.go) | YAMLの必須フィールド、命名規則、予約語、Provider名、未知フィールド |
| [schema_sync_test.go](schema_sync_test.go) | Goの型定義と、スキルのスキーマ説明やテンプレートとの整合 |
| [language_contract_test.go](language_contract_test.go) | 英語のdescriptionと、日本語の本文という表記規約 |
| [cross_validation_test.go](cross_validation_test.go) | 手動起動の指定、名称検証、テンプレートの置換トークンと注記 |
| [template_v2_shape_test.go](template_v2_shape_test.go) | テンプレートのプロンプト構成 |
| [aspect_guide_test.go](aspect_guide_test.go) | 観点別ガイドとテンプレートの対応 |

レビュー規則の形式を変更するときは、実装と併せてスキルの `references/schema.md`、`templates/`、`SKILL.md` を確認する。
これらのテストは配布ファイルの整合を検査するもので、エージェントが生成するレビューの品質を保証するものではない。
