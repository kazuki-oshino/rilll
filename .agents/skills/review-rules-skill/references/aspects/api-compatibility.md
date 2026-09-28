# api-compatibility-review 観点合成ガイド

> 本ガイドは review-rules-skill Phase 4 が `applies_when` / `prompt` を合成するための観点別手順書である。テンプレ (種) `templates/api-compatibility-review.yaml` と本ガイドを Read し、Phase 1 のリポ調査結果を織り込んで合成する。

## 1. 観点の目的

API 互換観点では以下クラスの問題を検出する:

- 公開 API (関数 / メソッド / 構造体 / 型 / interface) のシグネチャ変更による破壊的変更 (引数 / 戻り値 / エラー型)
- HTTP / gRPC / GraphQL のレスポンス形式変更による wire 互換破壊 (フィールド削除 / 型変更 / 列挙値追加と clients の影響)
- semver 整合性 (破壊的変更時の major bump、既存 deprecation の削除タイミング)
- deprecation 運用 (`Deprecated:` / `@deprecated` 注釈の有無、置換手段の提示、移行期間の確保)
- バージョン交渉 / negotiation (`/v1` / `Accept` / `protocol_version` 系の取り扱い)

これらを公開 API 領域に紐付けて指摘し、外部 client への破壊的変更を merge 前に塞ぐことが目的。

## 2. Phase 1 で読むべき入力

参照する Phase 1 出力 key:
- `tech_stack.languages`: 言語別の公開記法 (Go の大文字 export / TS の `export` / Python の `__all__` / Rust の `pub`) を観点に反映
- `convention_docs`: deprecation ポリシー (`docs/deprecation.md` / `CHANGELOG.md` 規約) を引用
- `dependencies`: 公開 API を生成する codegen / IDL (protobuf / OpenAPI / GraphQL schema) の存在を確認

追加で行う Read / Glob / Grep:
- Glob: `api/**` / `pkg/**` / `cmd/**` / `internal/api/**` / `proto/**` / `openapi*.{yml,yaml,json}` / `schema.graphql` — 公開 API 領域候補
- Glob: `CHANGELOG*` / `RELEASE_NOTES*` / `MIGRATION*.md` / `docs/migration/**` — semver / 移行履歴
- Glob: `go.mod` / `package.json` / `Cargo.toml` / `pyproject.toml` — 現行バージョン把握
- Grep: `Deprecated:` (Go godoc) / `@deprecated` (TS) / `warnings\.warn.*Deprecation` (Python) / `#[deprecated]` (Rust) — 既存 deprecation の所在
- Grep: `func\s+[A-Z]` (Go) / `export\s+(function|class|const|interface|type)` (TS) / `def\s+[a-z]` の変更を `__all__` と突合 — 公開シンボルの差分把握
- Read: `CHANGELOG.md` の latest section / `proto/**/*.proto` / `openapi.yaml` — 現行公開境界の理解

## 3. `applies_when` の作り方

判定軸:
- 公開 API 領域 (`api/` / `pkg/` / `proto/` / `openapi*` / `schema.graphql` / 公開 export 配下) の変更
- semver 関連ファイル (`CHANGELOG.md` / `package.json` の `version` / `Cargo.toml` の `version` / git tag 連携) の変更
- 公開シンボルのシグネチャ変更 (関数引数 / 戻り値 / 型 / interface メソッド)
- HTTP route / gRPC service method / GraphQL field の追加・変更・削除

作例 (2 件以上):

**作例 A (最小)**: Phase 1 が `tech_stack.languages=[Go]` / 公開 API 領域 `[pkg/, cmd/]` / `CHANGELOG.md` ありを返した場合
```
この rule は Go repo の pkg/ / cmd/ 配下の公開シンボル (大文字 export) の追加・変更・削除を含む変更、proto/ ファイルの変更、CHANGELOG.md / go.mod の version 関連変更に対して適用する。
```

**作例 B (充実)**: Phase 1 が `tech_stack.languages=[TypeScript]` / 公開 API 領域 `[packages/sdk/src, openapi.yaml, schema.graphql]` / `CHANGELOG.md` / `package.json` / `MIGRATION.md` ありを返した場合
```
この rule は TypeScript repo の packages/sdk/src 配下の `export` 公開シンボルの追加・変更・削除、openapi.yaml / schema.graphql のスキーマ変更、HTTP route / GraphQL resolver の変更、CHANGELOG.md / MIGRATION.md / package.json `version` の変更を含む差分に対して適用する。`@deprecated` タグの追加・削除も対象とする。
```

## 4. `prompt` の作り方

骨格 (重要観点 5 項):
1. 公開シンボル (関数 / 型 / interface / class / モジュール) のシグネチャ変更が破壊的か、引数追加が optional として収まっているか
2. wire 互換 (HTTP / gRPC / GraphQL) でのフィールド削除・型変更・required 化・列挙値削除など clients を壊す変更がないか
3. semver 整合 (破壊的変更で major bump、新機能で minor、bugfix で patch、pre-1.0 では minor 破壊許容) が CHANGELOG / version と一致しているか
4. deprecation 運用 (旧 API に `Deprecated:` / `@deprecated` 注釈 / 置換手段の提示 / 移行期間の確保) が repo ポリシー通りか
5. バージョン交渉・互換層 (`/v1` / `/v2` 並存、`Accept` ヘッダ、`protocol_version`、feature flag) の取り扱いが整合しているか

Phase 1 所見をどう織り込むか:
- 具体パス: 公開 API 領域 (`pkg/` / `packages/sdk/src` / `api/` / `proto/` / `openapi.yaml`) を観点 1, 2 に挿入
- シンボル名: diff から検出された公開関数 / type 名を名指しで観点 1 / 4 に挿入
- 言語別記法: Go なら godoc `Deprecated:` 1 行目規約 + 大文字 export、TypeScript なら `@deprecated` JSDoc + `export type` / `export const` + barrel re-export、Python なら `__all__` + `warnings.warn(DeprecationWarning)`、Rust なら `#[deprecated(since, note)]`
- IDL: protobuf なら field number 不変・`reserved` 利用、OpenAPI なら required / nullable 変更、GraphQL なら nullable 化 / required 化と client query 影響を観点 2 に明示

作例 (最小例): 作例 A に対応
```
以下の観点で pkg/ / cmd/ / proto/ の公開境界変更をレビューしてください。
1. 公開関数 / 公開構造体 / interface のシグネチャ変更が破壊的か (引数追加 / 戻り値型変更 / エラー型変更) を確認する。引数追加は optional として収まらない場合、新関数併設または major bump が必要。
2. proto / wire 互換で field number の変更・削除、`required` 化、enum 値削除など clients を壊す変更がないかを確認する。削除は `reserved` で番号予約しているか。
3. CHANGELOG.md と go.mod の version が semver 規約 (破壊的=major / 機能追加=minor / 修正=patch) で整合しているかを確認する。
4. 旧 API に `Deprecated:` 注釈 (godoc 1 行目規約) と置換手段が示され、削除タイミングが repo ポリシーの移行期間を満たしているかを確認する。
5. `/v1` / `/v2` の並存方針、`Accept` / version negotiation 経路が新変更と整合しているかを確認する。
指摘は重大度 (critical / high / medium / low) を付け、修正方針 (新関数併設 / 移行期間延長等) を 1 行で添える。
```

作例 (充実例): 作例 B に対応
```
以下の観点で packages/sdk/src / openapi.yaml / schema.graphql の公開境界変更をレビューしてください。
1. `export function` / `export class` / `export interface` / `export type` のシグネチャ変更が破壊的か (引数追加 / 戻り値型変更 / generic constraint 強化) を確認する。引数追加は optional として収まらない場合、新シンボル併設または major bump が必要。
2. openapi.yaml の required / nullable / type 変更、status code 削除、レスポンスフィールド削除、schema.graphql の field 削除 / non-null 化 / enum 値削除など、SDK clients を壊す変更がないかを確認する。
3. CHANGELOG.md と package.json `version`、MIGRATION.md の記載が semver (破壊的=major / 機能追加=minor / 修正=patch) と整合しているかを確認する。pre-1.0 の場合の minor 破壊許容ポリシーも踏まえる。
4. 旧 API に `@deprecated` JSDoc 注釈と置換手段が示され、削除タイミングが repo の移行期間ポリシーを満たしているかを確認する。barrel re-export (`index.ts`) からの export 削除も影響範囲に含める。
5. SDK clients が依存する `apiVersion` / `Accept` / GraphQL `@version` 系 directive の取り扱い、feature flag による段階的展開が整合しているかを確認する。
指摘は重大度 (critical / high / medium / low) を付け、修正方針 (新シンボル併設 / openapi の `deprecated: true` 付与 / 移行期間延長等) を 1 行で添える。
```

## 5. 合成後の自己点検項目

合成した `applies_when` / `prompt` が以下を満たすか機械的にチェックする:

- [ ] 観点リスト (prompt) に Phase 1 から得た公開 API パス (`pkg/` / `api/` / `openapi.yaml` / `schema.graphql` 等) が **少なくとも 1 つ** 含まれている (汎用文章で済んでいない)
- [ ] 重大度の付け方 (critical / high / medium / low 等) が prompt 内に明示されている
- [ ] applies_when に「いつ発火するか」の判定軸が **2 つ以上** 含まれている (例: 公開シンボル変更 + IDL 変更 + version 系ファイル変更)
- [ ] applies_when / prompt に未置換の `<UPPER_SNAKE>` token が残存していない
- [ ] prompt が「(観点が思いつかなければ何でも報告して)」のような責務放棄になっていない
- [ ] 5 観点 (シグネチャ / wire / semver / deprecation / version negotiation) のうち少なくとも 4 種が prompt に登場している
- [ ] tech_stack に対応する deprecation 記法 (`Deprecated:` / `@deprecated` / `warnings.warn` / `#[deprecated]`) が観点に登場している
