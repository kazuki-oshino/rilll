# config-exposure-review 観点合成ガイド

> 本ガイドは review-rules-skill Phase 4 が `applies_when` / `prompt` を合成するための観点別手順書である。テンプレ (種) `templates/config-exposure-review.yaml` と本ガイドを Read し、Phase 1 のリポ調査結果を織り込んで合成する。

## 1. 観点の目的

設定露出観点では以下クラスの問題を検出する:

- secrets / credentials / token / API key の `.env` / config / IaC へのコミット混入
- ハードコードされた接続情報 (DB URL / OAuth client secret / S3 access key) の流出
- 既定値が本番危険値になっている設定 (debug=true / 認証 off / CORS=*) の出荷
- 設定の暗号化 / sealed secrets / KMS / vault 連携の経路を迂回する平文導入
- 設定境界の取り違え (dev / staging / prod の設定混在、env-specific overlay の壊れ、role 別設定の漏れ)

これらを repo の設定ファイルレイアウトに紐付けて指摘し、本番事故を merge 前に塞ぐことが目的。

## 2. Phase 1 で読むべき入力

参照する Phase 1 出力 key:
- `convention_docs`: 設定運用方針 (`docs/configuration.md` / `docs/secrets.md` / steering の `config.md`) を引用
- `tech_stack.languages`: 言語別 config ライブラリ (viper / dotenv / pydantic-settings / config-rs) を観点に反映
- `dependencies`: secrets / config 系依存 (sops / vault client / aws-sdk-secrets-manager / sealed-secrets) の存在を確認
- `sensitive_areas`: 既存 secrets 関連パスがあれば優先対象に挿入

追加で行う Read / Glob / Grep:
- Glob: `.env*` / `*.env` / `**/*.config.{ts,js,yaml,yml,toml,json}` / `config/**` / `configs/**` / `etc/**` — 設定ファイル所在
- Glob: `**/Dockerfile*` / `**/docker-compose*.{yml,yaml}` / `k8s/**` / `helm/**` / `terraform/**` / `**/*.tf` / `ansible/**` — IaC 経路
- Glob: `**/secrets*.{yaml,yml,json}` / `sealed-secrets/**` / `.sops.yaml` — secrets 管理経路
- Read: `.env.example` / `.env.sample` — 「公開して良い既定値の集合」
- Read: `.gitignore` — どの設定ファイルが ignore されているかの確認
- Grep: `(?i)(api[_-]?key|secret|token|password|access[_-]?key)\s*[:=]\s*["'][^"'$]+["']` を設定ファイル全般に対して — 平文 secret 検出
- Grep: `debug\s*[:=]\s*true|DEBUG\s*=\s*True|allow_origin.*\*` — 危険既定値の検出

## 3. `applies_when` の作り方

判定軸:
- `.env*` / `*.env` / `**/*.config.*` / `config/` / `configs/` / `etc/` 配下の追加・変更
- IaC ファイル (Dockerfile / docker-compose / k8s manifest / Helm chart / Terraform / Ansible) の変更
- secrets 管理ファイル (sealed-secrets / sops / vault policy) の変更
- 設定読み込みコード (viper / dotenv / pydantic-settings / config-rs 利用箇所) の変更

作例 (2 件以上):

**作例 A (最小)**: Phase 1 が `tech_stack.languages=[Go]` / 設定領域 `[.env.example, configs/, Dockerfile]` を返した場合
```
この rule は Go repo の .env* / configs/ 配下 / Dockerfile / docker-compose*.yml の追加・変更、viper 等 config ローダ利用箇所の変更、.gitignore からの設定ファイル除外漏れを含む変更に対して適用する。
```

**作例 B (充実)**: Phase 1 が `tech_stack.languages=[TypeScript, Python]` / 設定領域 `[.env, .env.example, apps/web/next.config.ts, services/api/config/, k8s/, helm/]` / `dependencies=[dotenv, pydantic-settings, sealed-secrets]` を返した場合
```
この rule は TypeScript / Python repo の .env* / *.env / apps/web/next.config.ts / services/api/config/ 配下 / k8s/ / helm/ 配下 / Dockerfile / docker-compose*.yml / sealed-secrets 関連ファイルの追加・変更、dotenv / pydantic-settings 利用箇所の変更、.gitignore の設定ファイル除外漏れを含む変更に対して適用する。
```

## 4. `prompt` の作り方

骨格 (重要観点 5 項):
1. secrets / credentials / token / API key の平文混入 (.env / config / IaC / Dockerfile / k8s manifest / source code)
2. 既定値の安全性 (debug=true / 認証 off / CORS=* / TLS 検証 skip / log-level=debug 等の本番危険値が出荷経路に乗っていないか)
3. 設定の暗号化 / 取り扱い経路 (sealed-secrets / sops / vault / KMS / external-secrets を迂回した平文導入がないか)
4. env 別設定境界 (dev / staging / prod の overlay / overrides が混線していないか、prod 値が dev に漏れていないか)
5. 設定追加時の運用整合 (`.env.example` / `docs/configuration.md` / リリースノートに新設定キーが反映されているか、必須 vs optional の扱い)

Phase 1 所見をどう織り込むか:
- 具体パス: 設定領域 (`configs/` / `k8s/` / `helm/` / `apps/*/config/`) を観点 1〜4 の参照先に挿入
- 言語別 config ライブラリ: Go なら viper / envconfig、TS なら dotenv / Next.js `process.env.NEXT_PUBLIC_*` の公開境界、Python なら pydantic-settings / `os.environ`、Rust なら config-rs / `dotenvy` を観点 1, 2 に明示
- IaC 経路: Dockerfile の `ENV` / k8s `env` / helm `values.yaml` / terraform `variable` / ansible `vars` を観点 1, 3 に明示
- secrets 管理: sealed-secrets / sops / vault / external-secrets / aws-secrets-manager 等 dependencies に応じて観点 3 で固有名を呼ぶ

作例 (最小例): 作例 A に対応
```
以下の観点で .env* / configs/ / Dockerfile の差分をレビューしてください。
1. API key / token / DB password / OAuth client secret 等の機微情報が `.env` / configs/ / Dockerfile / docker-compose*.yml / source code に平文で混入していないかを確認する。.env.example / .env.sample との差で「公開して良い値」と「秘密にすべき値」が区別されているか。
2. 既定値が安全か (debug=true / log-level=debug / TLS InsecureSkipVerify / CORS=* / 認証 disable 等の本番危険値が新規導入されていないか) を確認する。
3. secrets が暗号化経路 (sealed-secrets / sops / vault) を迂回して平文化されていないかを確認する。
4. dev / staging / prod の設定境界が overlay / config 切り替えで混線していないかを確認する。
5. 新規設定キー追加時に `.env.example` / `docs/configuration.md` / CHANGELOG への反映、必須 / optional の扱い、起動時バリデーションが整合しているかを確認する。
指摘は重大度 (critical / high / medium / low) を付け、修正方針を 1 行で添える。
```

作例 (充実例): 作例 B に対応
```
以下の観点で .env* / apps/web/next.config.ts / services/api/config/ / k8s/ / helm/ / sealed-secrets の差分をレビューしてください。
1. API key / OAuth client secret / DB password / Redis 接続文字列 / JWT signing key 等の機微情報が `.env` / Next.js config / pydantic Settings / k8s manifest / helm values / Dockerfile に平文で混入していないかを確認する。Next.js では `NEXT_PUBLIC_*` プレフィックスにより client bundle へ漏出する境界に特に注意する。
2. 既定値の安全性 (debug=true / log-level=debug / TLS verify=false / CORS allow_origin=* / 認証 disable / FastAPI `docs_url` 公開) が本番経路に乗っていないかを確認する。
3. secrets が sealed-secrets / sops / vault / external-secrets / aws-secrets-manager 等の暗号化経路を迂回して平文導入されていないかを確認する。
4. dev / staging / prod の helm values / k8s overlay / pydantic Settings env 切り替えが混線せず、prod 値が dev / CI に漏れていないかを確認する。
5. 新規設定キー追加時に `.env.example` / `docs/configuration.md` / CHANGELOG / helm values.schema.json への反映、必須 / optional の扱い、Settings バリデーション (pydantic) が整合しているかを確認する。
指摘は重大度 (critical / high / medium / low) を付け、修正方針を 1 行で添える。
```

## 5. 合成後の自己点検項目

合成した `applies_when` / `prompt` が以下を満たすか機械的にチェックする:

- [ ] 観点リスト (prompt) に Phase 1 から得た具体設定パス (`.env*` / `configs/` / `k8s/` / `helm/` 等) が **少なくとも 1 つ** 含まれている (汎用文章で済んでいない)
- [ ] 重大度の付け方 (critical / high / medium / low 等) が prompt 内に明示されている
- [ ] applies_when に「いつ発火するか」の判定軸が **2 つ以上** 含まれている (例: `.env*` 変更 + IaC 変更 + config ローダ変更)
- [ ] applies_when / prompt に未置換の `<UPPER_SNAKE>` token が残存していない
- [ ] prompt が「(観点が思いつかなければ何でも報告して)」のような責務放棄になっていない
- [ ] 5 観点 (平文混入 / 危険既定値 / 暗号化経路 / env 境界 / 運用整合) のうち少なくとも 4 種が prompt に登場している
- [ ] tech_stack に対応する config ライブラリ (viper / dotenv / pydantic-settings / config-rs 等) が観点に登場している
