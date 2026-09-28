---
name: review-rules-skill
description: "Bootstrap .rilll-config/review-rules/ by analyzing the target repo and proposing review rule YAMLs from a built-in template catalog. Use when initializing review rules in any Claude Code-using repo (rilll installation not required)."
disable-model-invocation: true
---

# review-rules-skill

## Overview

本 skill は markdown と yaml のみで構成され、Go コード・外部バイナリ・rilll ランタイム依存を持たない。任意の Claude Code 利用 repo (rilll 未インストール環境を含む) において、`.rilll-config/review-rules/<rule-name>.yaml` 形式のレビュー rule 群を初期生成するための pure Claude Code skill である。同梱された 8 種のテンプレートカタログ (`templates/*.yaml`) と review-workflow YAML schema 抜粋 (`references/schema.md`) を参照し、対象 repo に適合した観点候補を提案する。

skill 起動後は (1) Phase 1: 対象 repo 分析、(2) Phase 2: テンプレートカタログからの観点提案、(3) Phase 3: ユーザ確認、(4) Phase 4: YAML 書き出し、の 4 phase を 1 セッション内で順に実行する。各 Phase は前 Phase の完了を前提に進行し、Phase 3 の明示承認なしには Phase 4 の書き出しに進まない。

## Execution Modes (Interactive vs. Non-interactive)

本 skill は既定で **interactive モード** (人間ユーザとの対話あり) として動作する。Phase 1 の分析サマリ提示 (Req 2.5)、Phase 3 の `AskUserQuestion` による `adopt` / `reject` / `rename` 取得 (Req 3.5 / Req 4.2)、Phase 4 の衝突時 `AskUserQuestion` (Req 5.4) は interactive モード時の既定挙動である。

**呼び出し側 (caller) が non-interactive モードを宣言した場合** (例: 別の subagent / CC agent から dispatch されており人間ユーザに到達できない、CI バッチ実行、上位スキルが `--auto` 相当のフラグを伝播してきた、scenario prompt が「ユーザ対話不可」と明記している、等)、CC agent は以下の決定論的フォールバックに従い `AskUserQuestion` を呼ばずに進行する:

- **Phase 1 サマリ提示の代替**: 分析結果サマリを出力 (テキスト) として書き出すのみとし、ユーザの追加質問・訂正は受け付けず、即座に Phase 2 へ進む。Req 2.5 の「サマリ提示」要件は出力で満たされる。
- **Phase 3 個別選択の代替**: 全 `proposed_rules` について `action="adopt"` を既定し、`rename` / `reject` は発生させない。`confirmed_rules` には全候補を `adopt` として積む。本 phase の出力で「non-interactive モードで進行したこと」「全候補を `adopt` 既定としたこと」を明記する。
- **Phase 3 プレビュー先取り合成の代替**: 先取り合成 (`prompt_preview` / `applies_when_preview` 生成) はユーザ表示目的なので non-interactive モードでは省略する。代わりに Phase 4 ステップ 2 の本体合成を 1 回だけ実行する (= 「重複合成しない」契約は維持されるが、合成回数は 1 に減る)。`confirmed_rules.synthesized` は Phase 4 の合成結果で埋める (Phase 3 では空のまま積み、Phase 4 で確定)。
- **Phase 4 衝突時の代替**: 書き出し対象パスに既存ファイルが存在する場合、`AskUserQuestion` を呼ばずに `skip` を既定する (= 既存ファイルは変更しない、Auto-Rename Cascade Rule も発火しない)。`write_summary[i].result = "skipped"` とし、衝突理由 (= 既存ファイルあり + non-interactive 進行のため skip) を明記する。`overwrite` および `rename` は non-interactive モードでは発火しない。

呼び出し側が interactive / non-interactive のいずれを採るかを宣言していない場合、CC agent は **interactive を既定**とする (= ユーザ対話が可能な前提で `AskUserQuestion` を発火する)。non-interactive モードの宣言は scenario prompt / caller prompt / 上位スキルからの委譲指示に明記される想定で、本 skill 内の自動検出は行わない (= 「対話できなかったから自動で adopt した」という事後判断はせず、事前に呼び出し側が宣言した場合に限り上記フォールバックを適用する)。

## Execution Steps

### Phase 1: Repo Analysis

本 phase の目的は、対象 repo の前提情報 (主要言語 / framework / package manager、規約ドキュメント、test 配置、docs 配置、依存定義、sensitive area) を機械的に把握し、Phase 2 のテンプレート選別ロジックが参照する構造化サマリを構築することにある。Phase 1 の出力 key 名は本文で固定されているとおり一字一句変更してはならない。後続 Phase 2 / Phase 3 / Phase 4 はこの key 名を参照する。

#### 入力

- `target_repo_root`: CC agent の current working directory。Phase 1 は本ディレクトリを起点に `Read` / `Glob` / `Grep` を実行する。

#### 検出手順

Phase 1 は以下の検出を順に実施する。各手順は CC ランタイムツール (`Read` / `Glob` / `Grep`) のみで完結し、外部バイナリや network 通信を行わない。

1. **主要言語 / framework / package manager の検出 (Req 2.1)**
   - `Glob` で `package.json` / `go.mod` / `requirements.txt` / `pyproject.toml` / `Cargo.toml` / `Gemfile` / `pom.xml` / `build.gradle` / `composer.json` 等の依存定義ファイル存在を確認する。
   - 該当ファイルを `Read` で読み、依存名 / module path / framework 名 (例: `package.json` の `dependencies` から `react` / `next` / `express`、`go.mod` の `require` から `gin` / `echo` 等) を抽出する。
   - 言語の同定はファイル拡張子の集計ではなく、依存定義ファイルの存在に基づき軽量に判定する。複数言語が共存する repo (例: Go + TypeScript) の場合は両方を `tech_stack.languages` に列挙する。

2. **規約ドキュメントの読込 (Req 2.2)**
   - `Glob` で repo root 直下の `CLAUDE.md` / `AGENTS.md` の存在を確認し、存在すれば `Read` で読み込む。
   - `.kiro/steering/` ディレクトリが存在する場合は `Glob` で `.kiro/steering/*.md` を列挙し、既定 3 ファイル (`product.md` / `tech.md` / `structure.md`) と任意のカスタム steering を `Read` で読み込む。
   - 各ドキュメントから (a) コーディング標準・規約、(b) sensitive area の手掛かり (例: `internal/auth/`、`bridge/secrets/`)、(c) 利用 framework / provider の前提を抽出する。
   - 各ドキュメントは Phase 2 の rationale 強化材料として `convention_docs[]` に格納する。

3. **test / docs ディレクトリの検出 (Req 2.3)**
   - `Glob` で `test/` / `tests/` / `__tests__/` / `*_test.go` / `spec/` / `e2e/` 等の test 配置パターンを確認する。
   - `Glob` で `docs/` / `doc/` / `documentation/` / `website/` 等の docs 配置パターンを確認する。
   - 検出された配置が複数ある場合は最初に発見されたパスを代表として保持し、残りを `paths[]` に列挙する。命名規約 (例: `*_test.go` / `*.test.ts`) を `test_layout.conventions` に文字列として記録する。

4. **依存定義ファイルの検出 (Req 2.4)**
   - 手順 1 で確認済みの依存定義ファイルパスを `dependencies.files[]` に格納する。
   - 各ファイルから notable な依存 (security / docs / test / performance / dependency 観点に直結する依存。例: `bcrypt` / `jsonwebtoken` / `lodash` / `gorm` / `pytest` / `jest`) を `dependencies.notable[]` に文字列で抽出する。

5. **sensitive area のヒューリスティック検出**
   - `Glob` で `auth*` / `secrets*` / `credentials*` / `token*` / `oauth*` / `password*` のディレクトリ / ファイルパターンを repo 全域で確認する。
   - マッチしたパスを `sensitive_areas[]` に格納する。0 件の場合は空配列とする (検出失敗を意味しない)。

#### Performance 契約 (context overflow 対策)

大規模 repo において Phase 1 は **全ファイル走査をしない**。具体的には以下の制約を守る (design.md § Performance & Scalability):

- 依存定義ファイル (`package.json` / `go.mod` / `requirements.txt` / `Cargo.toml` 等) と主要ディレクトリ (test / docs / sensitive area 候補) の **存在確認** を `Glob` で行い、ファイル本体の `Read` は依存定義ファイルおよび規約ドキュメントに限定する。
- ソースコード本体や `node_modules/` / `vendor/` / `.git/` 等のビルド成果物・依存キャッシュは Phase 1 では一切読み込まない。
- 検出対象が多数ある場合 (例: `*_test.go` が数千件) は代表パス + 件数概要のみを保持し、全パス列挙による context overflow を避ける。

この制約は Phase 1 の context overflow 対策 (大規模 repo における CC agent の context window 上限超過の回避) であり、Phase 2 以降のテンプレート選別ロジックは Phase 1 出力 (依存定義 + 主要ディレクトリ存在情報) のみで判定可能なように設計されている。

#### 出力契約

Phase 1 の出力は以下の構造化サマリ (key 名固定) を満たす。後続 Phase 2 はこの key を直接参照する。

```yaml
phase_1_output:
  tech_stack:
    languages: [string]            # 例: ["Go", "TypeScript"]
    frameworks: [string]           # 例: ["gin", "react"]
    package_managers: [string]     # 例: ["go modules", "npm"]
  convention_docs:                 # CLAUDE.md / AGENTS.md / .kiro/steering/* 等
    - path: string                 # 例: ".kiro/steering/structure.md"
      summary: string              # 該当ドキュメントから抽出した規約 / 標準 / sensitive area の要約
  test_layout:
    exists: bool
    paths: [string]                # 例: ["internal/", "bridge/<name>/test/"]
    conventions: string            # 例: "*_test.go (Go 標準)"
  docs_layout:
    exists: bool
    paths: [string]                # 例: ["docs/"]
  dependencies:
    files: [string]                # 例: ["go.mod", "package.json"]
    notable: [string]              # 観点候補生成材料となる依存名 (例: ["jsonwebtoken", "bcrypt"])
  sensitive_areas: [string]        # ヒューリスティック検出結果 (例: ["internal/auth/", "bridge/credentials/"])
```

#### 終端条件 (ユーザ提示と Phase 2 への遷移、Req 2.5)

Phase 1 の最後に、CC agent は以下の **分析結果サマリ** をユーザに提示してから Phase 2 に進む:

- 検出された **tech stack** (`tech_stack.languages` / `frameworks` / `package_managers`)
- 参照した **規約ドキュメント** の一覧 (`convention_docs[].path`)
- **sensitive area の有無** (`sensitive_areas` の件数 + 代表パスの列挙、または「未検出」)

サマリ提示後、ユーザの追加質問や訂正を受け付けつつ Phase 2 へ進行する。Phase 2 はこのサマリ (= `phase_1_output` 全体) を入力として受け取る。

#### 事後条件

- すべての検出結果は Phase 2 のテンプレート選別ロジック (stack 別選別 + 共通選別) の入力として利用可能である。
- key 名 (`tech_stack` / `convention_docs` / `test_layout` / `docs_layout` / `dependencies` / `sensitive_areas`) は Phase 2 / Phase 3 / Phase 4 で参照されるため一字一句変更しない。

### Phase 2: Catalog-based Proposal

本 phase の目的は、Phase 1 で構築した `phase_1_output` を入力として、同梱テンプレートカタログ (8 種) から対象 repo に適合する観点を選別し、各候補に選別根拠 (rationale) を付与した `proposed_rules` を構築することにある。本 phase はテンプレート本文の合成や `applies_when` / `prompt` の生成を行わない。観点合成は Phase 4 (合成本体) と Phase 3 (プレビュー用先取り合成) の責務である。

#### 入力

- `phase_1_output` (Phase 1 で固定した key 名: `tech_stack` / `convention_docs` / `test_layout` / `docs_layout` / `dependencies` / `sensitive_areas`)
- `templates/*.yaml` 配下の 8 種テンプレ「種」 (`name` / `description` / `provider` / `model` の 4 フィールドのみ)
- `references/aspects/*.md` 配下の 8 種観点合成ガイド

#### テンプレ「種」と観点合成ガイドの存在確認 (Req 3.1)

CC agent は本 phase 冒頭で以下 8 組の対応関係を `Glob` で確認する。テンプレ本体および観点合成ガイドの本文 Read は本 phase では行わず、Phase 4 (合成本体) または Phase 3 (プレビュー用先取り合成) のタイミングで Read する。

| テンプレ「種」 | 観点合成ガイド |
|---|---|
| `templates/security-review.yaml` | `references/aspects/security.md` |
| `templates/docs-review.yaml` | `references/aspects/docs.md` |
| `templates/dependency-review.yaml` | `references/aspects/dependency.md` |
| `templates/test-review.yaml` | `references/aspects/test.md` |
| `templates/performance-review.yaml` | `references/aspects/performance.md` |
| `templates/api-compatibility-review.yaml` | `references/aspects/api-compatibility.md` |
| `templates/config-exposure-review.yaml` | `references/aspects/config-exposure.md` |
| `templates/todo-tracking-review.yaml` | `references/aspects/todo-tracking.md` |

テンプレ「種」は v2 仕様により `applies_when` / `prompt` フィールドを保持しない。`applies_when` / `prompt` の合成は Phase 4 で観点合成ガイドに従って行われる (詳細は Phase 4 セクション参照)。

#### 8 テンプレートの観点割当 + 推奨化シグナル (SSoT)

下表「推奨化シグナル」列は Phase 2 のスタック別選別の判定契約 (design.md § 8 テンプレートの観点割当 + 推奨化シグナル の SSoT 転記)。`phase_1_output` のいずれかにシグナルが立った場合、当該テンプレートを推奨候補として `proposed_rules` に含める。fallback 列 ✓ のテンプレートは Req 3.4 (提案を空にしない最低保証) のための fallback セット。

| ファイル名 | name | 主観点 | 推奨化シグナル | fallback |
|---|---|---|---|---|
| `security-review.yaml` | `security-review` | 脆弱性 / credential ハードコード / 入力サニタイズ | 常時推奨 (fallback 内)。sensitive area (`auth*` / `secrets*` / `crypto*` / `keys*` ディレクトリ存在) 検出時は rationale 強化 | ✓ |
| `docs-review.yaml` | `docs-review` | README / CHANGELOG / 新規 API doc 整合 | 常時推奨 (fallback 内)。`docs/` / `documentation/` / `README.*` / `CHANGELOG.*` 検出時は rationale 強化 | ✓ |
| `test-review.yaml` | `test-review` | テスト網羅 / 新規 API のテスト有無 | テスト配置検出時 (`*_test.go` / `__tests__/` / `test/` / `tests/` / `spec/` / `*.test.*` / `*.spec.*`) | — |
| `performance-review.yaml` | `performance-review` | hot path / 計算量 / リソース使用 | コンパイル系/システム系言語検出時 (Go / Rust / C / C++ / Java / Kotlin / Swift) OR `bench*` / `benchmark*` ディレクトリ存在 | — |
| `dependency-review.yaml` | `dependency-review` | 依存追加の必要性 / 古い依存 / licensing | 常時推奨 (fallback 内)。依存定義ファイル (`package.json` / `go.mod` / `requirements.txt` / `Cargo.toml` / `Gemfile` / `pyproject.toml` 等) 検出時は rationale 強化 | ✓ |
| `api-compatibility-review.yaml` | `api-compatibility-review` | backward compatibility 違反 | 公開 API 領域検出時 (`api/` / `pkg/` / `cmd/` / `lib/` / `*.proto` / OpenAPI 系 `*.openapi.*` / `*.swagger.*`) OR semver 管理 (`VERSION` / `version.txt` / `package.json` の `"version"`) 検出 | — |
| `config-exposure-review.yaml` | `config-exposure-review` | env / config / secrets 漏洩 | 言語非依存。設定ファイル検出時 (`.env*` / `*.config.*` / `config/` / `settings/`) は必須推奨、検出無しでも任意候補として追加 | — |
| `todo-tracking-review.yaml` | `todo-tracking-review` | TODO / FIXME / XXX のトラッキング | 言語非依存。stack 判定の成否にかかわらず任意候補として追加 (Flow 2 の「共通選別」に該当) | — |

**判定契約の不変式**:

- Phase 2 出力 `proposed_rules` は最低 3 件 (fallback ✓ の 3 種 = `security-review` / `docs-review` / `dependency-review`) を必ず含む (Req 3.4 「提案を空にしない」を担保)。
- 「推奨化シグナル」列に複数条件 (OR) がある場合、いずれか 1 つでも立てば推奨化する。
- 共通選別 (`config-exposure-review` / `todo-tracking-review`) は言語非依存のため、stack 判定の成否にかかわらず候補として提示する (Flow 2 の「共通選別」分岐に対応)。
- 各 `proposed_rules[i].rationale` は本表「推奨化シグナル」のうち実際にマッチしたシグナルを文章化する (例: 「`go.mod` を検出したため `dependency-review` を推奨。`bench/` ディレクトリも検出したため `performance-review` を併せて推奨」)。

#### stack 判定不能時の fallback (Req 3.4)

`phase_1_output.tech_stack` から `languages` / `frameworks` / `package_managers` のいずれも有意に判定できない場合 (例: 依存定義ファイル不在で言語が確定しない場合)、以下の最低保証セットを提案して `proposed_rules` を空にしない:

- `security-review` / `docs-review` / `dependency-review` の **fallback 3 種**を `proposed_rules` に必ず含める。
- 共通選別 (`config-exposure-review` / `todo-tracking-review`) は **stack 判定の成否にかかわらず**候補として提示する。`config-exposure-review` は `.env*` / `*.config.*` / `config/` / `settings/` の検出有無に応じて rationale を強化し、検出無しの場合も任意候補として追加する。`todo-tracking-review` は言語非依存なので任意候補として追加する。

stack 判定が成功した場合は、上表「推奨化シグナル」列を順に評価して該当する候補を `proposed_rules` に追加する。fallback 3 種は stack 判定の成否にかかわらず常時候補に含まれる (= 最低保証セット)。

#### 出力契約 (`proposed_rules`)

Phase 2 の出力 `proposed_rules` は以下の構造を満たす配列とする (design.md § Service Interface phase_2 と同形)。

```yaml
proposed_rules:
  - rule_name: string                # kebab-case (^[a-z0-9]+(-[a-z0-9]+)*$ に合致、予約語 result / context を踏まない)
    source_template: string          # 元になった templates/*.yaml のファイル名 (例: "security-review.yaml")
    aspect_guide: string             # 対応する観点合成ガイドのパス (例: "references/aspects/security.md")
    rationale: string                # 上表「推奨化シグナル」のうち実際にマッチしたシグナルを文章化した選別根拠
```

各候補の `rationale` には、**実際にマッチした上表「推奨化シグナル」列のシグナルを文章化**して記述する。例:

- 「`go.mod` を検出したため `dependency-review` を推奨。`bench/` ディレクトリも検出したため `performance-review` を併せて推奨」
- 「`auth*` ディレクトリを検出したため `security-review` の rationale を強化 (sensitive area 重点)」
- 「`tech_stack.languages` 判定不能のため fallback 3 種 (`security-review` / `docs-review` / `dependency-review`) を最低保証として提案」

#### Phase 4 / Phase 3 へのバトン

- **Phase 2 の責務範囲**: 観点候補の選別 (rationale の組み立て含む) と、各候補に対応するテンプレ「種」 + 観点合成ガイドの存在確認のみ。`applies_when` / `prompt` の合成、placeholder 操作、本文書き換えはいずれも行わない (v2 ではテンプレに placeholder token は存在しない)。
- **Phase 3 の利用方法**: Phase 3 は `proposed_rules` の各候補について `adopt` / `reject` / `rename` のユーザ選択を取得する。プレビュー (`prompt_preview` / `applies_when_preview`) は Phase 3 で「先取り合成」を実行して合成後の冒頭 3〜5 行抜粋として表示する (合成手順は Phase 4 セクション参照)。
- **Phase 4 の利用方法**: Phase 4 は確定した rule (`adopt` または `rename` 後) について、テンプレ「種」 + 観点合成ガイド + Phase 1 所見を入力に `applies_when` / `prompt` を合成し、`.rilll-config/review-rules/<rule-name>.yaml` に書き出す。Phase 3 で先取り合成済みの結果がある場合は再利用し、合成を重複実行しない。

### Phase 3: User Confirmation

本 phase の目的は、Phase 2 が構築した `proposed_rules` をユーザに提示し、各候補について `adopt` / `reject` / `rename` の明示選択を取得した上で、Phase 4 へ引き継ぐ `confirmed_rules` を確定することにある。本 phase は YAML の書き出しを一切行わず、書き出しは Phase 4 の責務である。Phase 3 の明示承認 (= 少なくとも 1 件の `adopt` または `rename`) を経ない限り、CC agent は Phase 4 に進んではならない (Req 4.2)。

#### 入力

- `proposed_rules` (Phase 2 の出力)。各要素は以下を保持する:
  - `rule_name`: kebab-case の rule 名
  - `source_template`: 元になった `templates/*.yaml` のファイル名
  - `aspect_guide`: 対応する観点合成ガイドのパス (例: `references/aspects/security.md`)
  - `rationale`: Phase 2 の選別根拠 (実際にマッチした推奨化シグナルの文章化)

#### 候補一覧の提示要件 (Req 4.1)

CC agent は候補ごとに以下の情報を **必ず併記** してユーザに提示する (design.md § phase_3 presented_to_user)。

- `rule_name`: 候補の rule 名
- `output_path`: `.rilll-config/review-rules/<rule-name>.yaml` の予定書き出しパス
- `rationale`: Phase 2 の選別根拠
- `prompt_preview`: 合成後の `prompt` 冒頭抜粋 (3〜5 行目安)
- `applies_when_preview`: 合成後の `applies_when` 冒頭抜粋 (3〜5 行目安)

`prompt_preview` / `applies_when_preview` は **Phase 3 のプレビュー表示専用** である。プレビュー生成は **先取り合成** (Phase 4 の合成手順をプレビュー目的で先行実行) によって行う。具体的には、各候補について (a) テンプレ「種」 (`templates/<source_template>`) と (b) 対応する観点合成ガイド (`references/aspects/<aspect>.md`) と (c) Phase 1 所見 (`phase_1_output` 全体) を入力として、**Phase 4 ステップ 2 (合成) と同一の手順** (観点合成ガイド第 3 章「`applies_when` の作り方」 / 第 4 章「`prompt` の作り方」に従い Phase 1 所見を織り込む) で `applies_when` / `prompt` 文字列を合成し、その結果から冒頭 3〜5 行を抜粋して `prompt_preview` / `applies_when_preview` とする。本 phase ではテンプレ原本を変更せず、YAML 書き出しは行わない (書き出しは Phase 4 の責務)。**Phase 4 では Phase 3 で先取り合成した結果を再利用する (合成は重複させない)**。プレビュー併記の目的は、Phase 1 所見が観点合成に十分織り込まれているかをユーザがサニティチェックする機会を Phase 3 で必ず提供することにある (フル本文の確認はユーザ任意)。

#### 個別選択操作 (Req 3.5 / Req 4.2)

CC agent は各候補について `AskUserQuestion` を用いて以下の 3 択を取得する:

- `adopt`: 候補をそのまま採用し Phase 4 へ引き継ぐ
- `reject`: 候補を却下し Phase 4 へ引き継がない
- `rename`: rule 名を変更して Phase 4 へ引き継ぐ。新しい rule 名 (`renamed_to`) は `AskUserQuestion` で続けてユーザから取得する

選択は候補単位で個別に行う。複数候補の一括採用 / 一括却下 UI を採用する場合も、最終的な `action` は候補単位で確定すること。

#### rename_validation (Req 3.5)

`action="rename"` を受領した直後 (= ユーザが `renamed_to` を入力した直後) に、以下 3 規則を CC agent が機械的に検査する (design.md § phase_3 rename_validation)。本検査は Phase 4 の schema 検査より前に実施し、違反はその場で再質問させ Phase 4 にエスカレートしないこと。

- **kebab-case**: `renamed_to` が正規表現 `^[a-z0-9]+(-[a-z0-9]+)*$` に合致すること
- **reserved-name**: `renamed_to` が予約語 (`result` / `context`) のいずれでもないこと
- **duplicate-name**: `renamed_to` が同セッション内の他の `confirmed_rules.rule_name` (= 既に確定済みの他候補の rule_name。ここでは `action="adopt"` の元 `rule_name` および `action="rename"` の `renamed_to` を含む) と重複しないこと

**on_failure**: いずれかの規則に違反した場合、CC agent は `AskUserQuestion` で**違反項目** (どの規約 / kebab-case・reserved-name・duplicate-name のどれに違反したか) を提示し、当該候補について `再リネーム` (別の `renamed_to` を入力) / `adopt` (元の `rule_name` で採用) / `reject` (却下) のいずれかを再選択させる。再リネームが入力された場合は本検査を再実行する。**Phase 4 へエスカレートしないこと** (= 違反のまま `confirmed_rules` に積んで Phase 4 の schema 検査に委ねてはならない)。

**重要 (Phase 4 との同一規則集合)**: ここで定義した rename_validation の 3 規則 (kebab-case 正規表現 / 予約語 `result` / `context` 禁止 / 同セッション内重複禁止) は、Phase 4 の **Auto-Rename Cascade Rule (`<name>-v2` 〜 `-v10` の自動採番および手入力 rename) が参照する検証ルール集合と完全に同一**である (design.md § Verification Contract / § Auto-Rename Cascade Rule line 668)。Phase 3 と Phase 4 で検証規則が drift しないことが本 skill の検証契約 (Verification Contract) として固定されているため、Phase 3 セクションの規則と Phase 4 セクションの規則を改訂する場合は両者を同時更新すること。

#### 全 reject / 中断時の終了処理 (Req 4.3)

以下のいずれかに該当する場合、CC agent は YAML を一切書き出さずにセッションを終了する。

- すべての候補について `action="reject"` が選択された
- ユーザが対話を中断 (cancel) した

終了時は、書き出し対象が 0 件であった旨 (= `.rilll-config/review-rules/` 配下に YAML を書き出していないこと) を**明示的にユーザに提示**する。Phase 4 は呼び出さない (= Phase 4 の手順には進まない)。

#### Phase 3 → Phase 4 ゲート (Req 4.2)

少なくとも 1 件の `action="adopt"` または `action="rename"` が確定した場合に限り、CC agent は Phase 4 に進む。引き継ぎ時の規約は以下のとおり (design.md § phase_3 postconditions)。

- `action="reject"` の rule は Phase 4 に引き継がない (= `confirmed_rules` から除外、または Phase 4 入力時点で skip)
- `action="adopt"` または `action="rename"` の rule のみが Phase 4 入力となる
- 引き継ぎ時に **Phase 3 で先取り合成した結果** (`applies_when_preview` / `prompt_preview` の元になった完全な合成結果 = `applies_when` / `prompt` の本文全体) を rule_name 単位で同梱する。Phase 4 はこの先取り合成結果を再利用し、合成を重複実行しない (Phase 4 ステップ 1〜3 は Phase 3 で済んでいる場合 skip 可能)

#### 出力契約 (`confirmed_rules`)

Phase 3 の出力 `confirmed_rules` は以下の構造を満たす配列とする (design.md § Service Interface phase_3 と同形)。

```yaml
confirmed_rules:
  - rule_name: string                # 元の名前 (Phase 2 の proposed_rules[i].rule_name)
    source_template: string          # 元になった templates/*.yaml のファイル名
    aspect_guide: string             # 対応する観点合成ガイドのパス (例: "references/aspects/security.md")
    action: "adopt" | "reject" | "rename"
    renamed_to: string | null        # action="rename" 時のみ実値、それ以外は null
    synthesized:                     # Phase 3 の先取り合成結果。Phase 4 はこれを再利用する (合成は重複させない)
      applies_when: string           # 合成済み applies_when 本文 (block scalar 想定)
      prompt: string                 # 合成済み prompt 本文 (block scalar 想定)
```

Phase 4 へは `action="adopt"` または `action="rename"` の要素のみを引き継ぐ。`action="rename"` の場合、Phase 4 内では `renamed_to` を rule 名 (= 書き出しファイル名 `<renamed_to>.yaml` および YAML 内 `name` フィールド) として用いる。

#### 終端条件 (termination_conditions)

Phase 3 は以下を満たした時点で終端する (design.md § phase_3 termination_conditions)。

- すべての候補についてユーザの明示選択 (`adopt` / `reject` / `rename` のいずれか) を取得済
- すべての `action="rename"` 候補について本 phase の rename_validation を通過済 (= 違反のまま終端しない)
- 全候補が `action="reject"` の場合、または中断 (cancel) の場合は Phase 4 をスキップしセッション終了 (Req 4.3)

#### 事後条件

- Phase 4 入力 `confirmed_rules` (= `action="adopt"` または `action="rename"` の要素のみ) が確定している
- 各 rule に対応する **Phase 3 で先取り合成した完全な合成結果** (`applies_when` / `prompt` の本文全体) が rule_name 単位で同梱されており、Phase 4 はこれを再利用する (合成は重複させない)
- rename_validation の 3 規則 (kebab-case / 予約語 / 重複) は Phase 4 Auto-Rename Cascade と同一規則集合を参照しており、Phase 3 / Phase 4 間で検証ルールが drift していない

### Phase 4: Write

本 phase の目的は、Phase 3 の明示承認 (`adopt` / `rename`) を経た rule のみを対象に、テンプレ「種」と観点合成ガイドおよび Phase 1 所見から `applies_when` / `prompt` を合成し、自己点検と事前検査を経て `.rilll-config/review-rules/<rule-name>.yaml` への YAML 書き出しを実行し、書き出し結果のサマリをユーザに提示することにある。本 phase は Phase 3 の明示承認なしには起動せず (Req 4.2)、自己点検 / 事前検査に違反した rule は skip され、衝突時はユーザの明示選択を経るまで既存ファイルを変更しない (Req 5.4)。Phase 3 と Phase 4 で参照する検証ルール集合は完全に同一であることが本 skill の検証契約 (Verification Contract) として固定されている (design.md § Verification Contract)。

v2 では Phase 4 は機械置換ではなく観点合成として動作する。テンプレは「観点カテゴリの枠 (種)」として `name` / `description` / `provider` / `model` の 4 フィールドのみを保持し、`applies_when` / `prompt` は本 phase で `references/aspects/<aspect>.md` の合成ガイドに従い AI が動的に合成する。

#### 入力

- `confirmed_rules` (Phase 3 の出力のうち `action="adopt"` または `action="rename"` の要素のみ。`action="reject"` の rule は本 phase の入力に含まれない。各要素は `rule_name` / `source_template` / `aspect_guide` / `action` / `renamed_to` を含む)
- `aspect_guides` (各 rule の `source_template` に対応する `references/aspects/<aspect>.md` の本文。Phase 2 で対応関係を確認済、本 phase で Read する)
- `phase_1_findings` (Phase 1 出力 = `tech_stack` / `convention_docs` / `test_layout` / `docs_layout` / `dependencies` / `sensitive_areas`)

#### 書き出し先規約 (Req 5.1)

各 rule は `.rilll-config/review-rules/<rule-name>.yaml` の形式で書き出す。ファイル名と YAML 内 `name` フィールドの規約は以下のとおり:

- `action="adopt"` の場合: ファイル名および YAML 内 `name` フィールドは `confirmed_rules[i].rule_name` とする
- `action="rename"` の場合: ファイル名は `<renamed_to>.yaml`、YAML 内 `name` フィールドも `renamed_to` とする (= 書き出しファイル名と YAML 内 `name` を一致させる)

#### ディレクトリ作成 (Req 5.2)

書き出し開始時に `.rilll-config/review-rules/` ディレクトリの存在を確認し、不在の場合は作成してから書き出す。

- ディレクトリ作成に失敗した場合 (権限等)、CC agent は error をユーザに明示しセッションを終了する (design.md § Error Categories: ディレクトリ作成失敗)。本ケースでは個別 rule の skip にエスカレートせず、本 phase 全体を中断する。

#### 書き出しステップ (write_steps v2, design.md § Service Interface phase_4 (Write) write_steps)

各 rule に対する書き出し処理は以下の 5 ステップで実施する。Phase 3 で先取り合成済みの結果がある場合、ステップ 1〜3 はその結果を再利用してよい (合成は重複実行しない)。

##### ステップ 1: 読み込み

CC agent は当該 rule について以下を Read で取得する:

- **テンプレ「種」**: `templates/<source_template>` (例: `templates/security-review.yaml`)。`name` / `description` / `provider` / `model` の 4 フィールドのみを保持し、`applies_when` / `prompt` フィールドは存在しない
- **観点合成ガイド**: `references/aspects/<aspect>.md` (例: `references/aspects/security.md`)。必須 5 セクション (観点の目的 / Phase 1 で読むべき入力 / `applies_when` の作り方 / `prompt` の作り方 / 自己点検項目) を含む
- **Phase 1 所見**: `phase_1_findings` (Phase 1 出力全体) を持ち越し、観点合成ガイド第 2 章「Phase 1 で読むべき入力」で指定された key を入力として用いる

##### ステップ 2: 合成

観点合成ガイドの第 3 章「`applies_when` の作り方」と第 4 章「`prompt` の作り方」セクションに従い、Phase 1 所見を織り込んで `applies_when` および `prompt` 文字列を生成する:

- **`applies_when` の合成**: 観点合成ガイド第 3 章の判定軸 (変更対象パス / 変更内容 / 関連ファイル種別 / 言語別キーワード等、観点ごとに異なる) と作例 (2 件以上掲載) を参照し、Phase 1 所見の具体値 (`sensitive_areas` / `tech_stack.languages` / `dependencies` 等) を織り込んで自然文で記述する
- **`prompt` の合成**: 観点合成ガイド第 4 章の骨格 (重要観点 5 項程度) に従い、Phase 1 所見からの具体パス / シンボル名 / 依存ライブラリ名を観点に挿入する。重大度の付け方 (critical / high / medium / low 等) を明示する
- **観点合成ガイド第 2 章で指定された追加 Read / Glob / Grep**: 必要に応じて実行し、合成材料に追加する (例: security 観点なら `**/auth/**` の Glob、`(?i)(api[_-]?key|secret|token|password)` の Grep)

合成結果は YAML の block scalar (`|`) として `applies_when` / `prompt` フィールドに格納する想定で、自然文として整形する。テンプレ「種」由来の `name` / `description` / `provider` / `model` 4 フィールドと合わせて、6 フィールドからなる rule YAML を組み立てる。

##### ステップ 3: 自己点検

観点合成ガイド第 5 章「合成後の自己点検項目」をチェックリスト化し、合成結果が満たすかを self-evaluate する。チェックリスト項目は観点ごとに異なるが、共通的には以下の類型を含む:

- 観点リスト (prompt) に Phase 1 から得た具体パス / シンボル名が少なくとも 1 つ含まれているか (汎用文章で済んでいないか)
- 重大度の付け方が prompt 内に明示されているか
- applies_when に「いつ発火するか」の判定軸が複数含まれているか
- applies_when / prompt に未置換の `<UPPER_SNAKE>` token が残存していないか
- prompt が責務放棄 (例: 「観点が思いつかなければ何でも報告して」) になっていないか
- 観点ごとの固有チェック (security なら OWASP Top 10 のうち少なくとも 3 種に対応している、tech_stack に対応する言語固有の脆弱関数名が登場している、等)

満たさない項目があった場合、CC agent は (a) ステップ 2 に戻り合成し直す、または (b) 当該 rule を skip して書き出しサマリで報告する のいずれかを選択する。再合成しても改善しない場合は skip を選択する。

##### ステップ 4: 事前検査 (preconditions, Req 6.3)

合成結果に対して以下 3 検査を機械的に実施する (design.md § phase_4 preconditions):

- **(a) 6 必須フィールドの非空**: `name` / `description` / `provider` / `model` / `applies_when` / `prompt` の 6 フィールドが全て埋まっていること (空文字列・null・未定義は不可)。`name` / `description` / `provider` / `model` はテンプレ「種」由来、`applies_when` / `prompt` はステップ 2 の合成結果由来
- **(b) `name` の kebab-case + 予約語チェック**: `name` が正規表現 `^[a-z0-9]+(-[a-z0-9]+)*$` に合致し、かつ予約語 `result` / `context` のいずれでもないこと
- **(c) 未置換 token 残存検査**: `applies_when` / `prompt` に正規表現 `<[A-Z][A-Z0-9_]*>` に合致する未置換 token が残存しないこと。v2 ではテンプレ「種」に placeholder token は存在せず、合成手順 (ステップ 2) でも `<UPPER_SNAKE>` 形式の token を生成しないことを契約とするため、本検査は drift gate として機能する (= 違反時は合成手順または観点合成ガイドの不備が疑われる)

**違反時の処理 (Req 6.3)**: 上記いずれかの検査に違反した場合、CC agent は当該 rule を skip し、**不足理由 (どの検査項目に違反したか) と rule 名**をユーザに明示する。書き出しサマリの `result` は `"skipped"` とする。schema 違反 rule は本 phase で書き出されず、書き出しサマリで明示される。

##### ステップ 5: 書き出し

事前検査を通過した rule について、`Write` ツールで `.rilll-config/review-rules/<rule-name>.yaml` に書き出す。書き出し対象パスは `action="adopt"` の場合は `.rilll-config/review-rules/<rule_name>.yaml`、`action="rename"` の場合は `.rilll-config/review-rules/<renamed_to>.yaml`、衝突時の Auto-Rename Cascade で別名が確定した場合はその別名のパスとする。

書き出し対象パスに既存ファイルが存在しない場合は新規ファイルとして書き出し (`result="created"`、Req 5.3)、既存ファイルが存在する場合は次節「衝突時の処理」に従う。

#### 衝突時の処理 (Req 5.4) と Auto-Rename Cascade Rule

書き出し対象パスに既存ファイルが存在する場合、CC agent は `AskUserQuestion` で `overwrite` / `skip` / `rename` を取得する。デフォルトは `skip` とし、明示的な選択を得るまで当該既存ファイルを変更しない。

- `overwrite` 選択時: 既存ファイルを上書きして書き出す。書き出しサマリの `result` は `"overwritten"` とする
- `skip` 選択時: 当該 rule を書き出さない。書き出しサマリの `result` は `"skipped"` とする
- `rename` 選択時: 以下の **Auto-Rename Cascade Rule** に従い、衝突しない別名を自動採番または手入力で確定したうえで書き出す。書き出しサマリの `result` は `"renamed_saved"` とする

##### Auto-Rename Cascade Rule (design.md § Auto-Rename Cascade Rule)

Phase 4 で `rename` が選択された場合の自動サフィックス規則 (4 ステップ):

1. **サフィックス採番**: 候補名は `<name>-v2` から始め、衝突継続時に `-v3`, `-v4`, ..., `-v10` まで線形試行する (上限 N=10)。最大値 10 はスペック範囲内 (rule 数想定 5〜10) を 1 オーダー上回る安全マージン。
2. **検証通過必須**: 採番された各候補は Phase 3 `rename_validation` と**同一規則** (kebab-case 正規表現 `^[a-z0-9]+(-[a-z0-9]+)*$` / 予約語 `result` / `context` 禁止 / 同セッション内 `confirmed_rules.rule_name` との重複禁止) を通過すること。`-vN` サフィックスは kebab-case を構造的に維持するため、本 skill 同梱テンプレート 8 種の `name` 値からは検証を破る組み合わせは生まれない (validation は冗長化として実施)。
3. **上限超過時**: `-v10` まで全て衝突または検証失敗した場合、auto-rename を中断し `AskUserQuestion` で**手入力 rename** を再提示する。手入力 rename も Phase 3 と同一の `rename_validation` を通過必須とし、違反時はその場で再質問する (Phase 4 にエスカレートしない)。
4. **手入力 rename 後の再衝突**: 手入力された名前も既存ファイルと衝突する場合、ステップ 1 から再帰せず、`overwrite` / `skip` / 別の手入力 rename の 3 択を再提示する (無限ループを回避)。

**重要 (Phase 3 ↔ Phase 4 同一規則集合)**: 本 Auto-Rename Cascade Rule の検証規則 (kebab-case / 予約語 / 重複) は Phase 3 セクション「rename_validation」で定義した 3 規則と完全に同一の規則集合を参照する (design.md § Verification Contract)。本 skill の検証契約として Phase 3 と Phase 4 の検証規則が drift しないことが固定されているため、本 phase の検証規則を改訂する場合は Phase 3 セクションの `rename_validation` と同時更新すること。

#### 出力契約 (`write_summary`, Req 5.5)

書き出し完了後、CC agent は以下の構造を満たす `write_summary` をユーザに提示する (design.md § phase_4 output)。

```yaml
write_summary:
  - rule_name: string                # 書き出し対象 rule の名前 (rename 確定後の名前を含む)
    path: string                     # ".rilll-config/review-rules/<name>.yaml" 形式
    result: "created" | "overwritten" | "skipped" | "renamed_saved"
```

`result` の 4 値の意味は以下のとおり:

- `"created"`: 書き出し対象パスに既存ファイルが無く、新規ファイルとして書き出した (Req 5.3)
- `"overwritten"`: 既存ファイルが存在し、ユーザが `overwrite` を選択して上書き書き出しした (Req 5.4)
- `"skipped"`: 既存ファイル衝突時にユーザが `skip` を選択した、自己点検 (ステップ 3) で再合成しても改善せず skip 判断した、または事前検査 (ステップ 4 / Req 6.3) 違反で書き出しを行わなかった
- `"renamed_saved"`: 既存ファイル衝突時にユーザが `rename` を選択し、Auto-Rename Cascade Rule または手入力 rename で確定した別名で書き出した

#### 終端条件 (termination_conditions)

Phase 4 は以下を満たした時点で終端する (design.md § phase_4 termination_conditions)。

- 全 rule の処理 (`created` / `overwritten` / `skipped` / `renamed_saved` のいずれか) が完了している

ディレクトリ作成失敗 (権限等) によりセッション中断となった場合は、本終端条件を満たさず本 phase 全体が中断する (design.md § Error Categories: ディレクトリ作成失敗)。

#### 事後条件 (postconditions)

- 書き出しサマリ (`write_summary`) をユーザに提示している (Req 5.5)
- 自己点検 (ステップ 3) で改善不能と判断された rule、および事前検査 (ステップ 4 の (a) / (b) / (c) のいずれか) に違反した rule は書き出されておらず、不足内容と rule 名がユーザに明示されている (Req 6.3)
- Phase 3 で `action="adopt"` または `action="rename"` だった rule のうち、衝突時の `skip` 選択 / 自己点検 skip / 事前検査違反 skip 以外は `.rilll-config/review-rules/` 配下に YAML として書き出されている
