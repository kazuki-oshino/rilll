# review-workflow YAML Schema Reference

## 改訂運用責務

> 上流 SSoT: `internal/workflow/builtin/review/rules/types.go` §`Rule` / §`ReservedRuleNames` および `internal/provider/types.go` §`ProviderID` 列挙
>
> review-workflow YAML schema を改訂した場合は、本ドキュメント (`references/schema.md`) と `templates/*.yaml` を**同時に**追従更新すること。本 skill は SSoT を所有しない参照側であり、改訂が伝播しなければ skill 出力の rule が後段の review-workflow loader に reject される。
>
> 注: 元々の上流 SSoT は `.kiro/specs/review-workflow/design.md` だったが、当該 spec の実装 main 取り込み完了に伴い (commit 1e58d08) spec ディレクトリは削除された。現在は runtime 実体である Go コードそのものが SSoT である。

本ドキュメントは上流 Go コード (Rule struct / yaml tag / 検証規則 / ProviderID 列挙) を YAML schema 視点で抜粋した参照資料である。runtime には参照されない (skill 利用者向けの reference)。

## YAML Schema (6 必須フィールド)

`.rilll-config/review-rules/<rule>.yaml` の各 rule は以下 6 フィールドを必須とする。

| フィールド | 型 | 必須 | 説明 | 制約 |
|---|---|---|---|---|
| `name` | string | ✓ | rule 識別子。loader が dedup key として用いる | kebab-case `^[a-z0-9]+(-[a-z0-9]+)*$` に一致すること / 予約語 `result` / `context` は禁止 |
| `description` | string | ✓ | rule の 1 行要約 | — |
| `provider` | string | ✓ | review 実行 provider | `claude` / `cursor` / `codex` / `cursor-sdk` のいずれか |
| `model` | string | ✓ | provider に応じた model 識別子 | provider 側で reject されない値 (loader では文字列値の妥当性検査をせず、`agent.CallAgent` 経由で provider 未登録時に fail) |
| `applies_when` | string | ✓ | この rule を適用する条件の自然文 | string のみ (list / map は decode 失敗 → Warning) |
| `prompt` | string | ✓ | レビュー観点の指示文 (review 実行時に provider へ渡す) | string |

予約語の根拠: `result.json` (集約 artifact) との衝突回避が目的で、将来の `context.json` (PR snapshot) も先行予約されている。

## 検証規則

review-workflow loader (`LoadRules(cwd string) ([]Rule, []Warning, error)`) は以下の規則で YAML を取り込む。本 skill が出力する YAML は本規則を満たすこと。

- **当該 rule のみ skip + Warning** となるケース (他の rule の取り込みは継続):
  - 必須フィールド (上記 6 つのいずれか) の欠落
  - `name` が空 / kebab-case 正規表現 `^[a-z0-9]+(-[a-z0-9]+)*$` 違反
  - `name` が予約語 (`result` / `context`) に一致
  - YAML decode 時の unknown フィールド (loader は `KnownFields(true)` で未知フィールドを skip 理由化する)
- **loader fatal error** となるケース (review 全体が中断される):
  - 同一 `name` の YAML が複数ファイルに存在 (`ErrDuplicateRuleName`)。重複検出の前段で Warning となる rule (パース失敗 / 予約名等) は重複検出から除外される
  - rules dir 不在 (`ErrRulesDirMissing`)
  - rules dir は存在するが rule が 0 件 (`ErrNoRulesFound`)
- **loader 対象範囲**:
  - `.rilll-config/review-rules/` 直下の `*.yaml` のみが loader 対象
  - サブディレクトリ配下の YAML は loader が読み込まない (本 skill もサブディレクトリには出力しないこと)

## provider / model 既定値の選定理由

- 本 skill 同梱の `templates/*.yaml` は **`provider: claude` / `model: claude-opus-4-7` を既定値**とする。
  - 理由: rilll 本体が Claude bridge (`@anthropic-ai/claude-agent-sdk`) を first-class で扱っており、利用者の初期セットアップ負荷が最も小さい。
- 利用環境に応じて `codex` / `cursor` / `cursor-sdk` への変更が可能。各テンプレート冒頭のコメントで他 provider への切り替え手順を案内する。
- `provider` 未認証 / 未セットアップ環境 (例: `ANTHROPIC_API_KEY` 未設定 / `cursor-agent` CLI 未インストール) では review-workflow precheck で fail する。**provider 認証状態の検証は本 skill の責務外**であり、skill は schema 整合のみを保証する。
