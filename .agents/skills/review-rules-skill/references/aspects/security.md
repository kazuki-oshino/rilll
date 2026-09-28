# security-review 観点合成ガイド

> 本ガイドは review-rules-skill Phase 4 が `applies_when` / `prompt` を合成するための観点別手順書である。テンプレ (種) `templates/security-review.yaml` と本ガイドを Read し、Phase 1 のリポ調査結果を織り込んで合成する。

## 1. 観点の目的

セキュリティ観点では以下クラスの問題を検出する:

- 機微情報 (credential / API key / secret / token / PII) のハードコード or ログ出力経路への漏洩
- 認証 / 認可境界 (auth middleware / RBAC / session) の取り違え・抜け道
- 外部入力 (HTTP body / query / CLI 引数 / file / IPC) のサニタイズ不足に起因する injection 系脆弱性 (SQL / command / path traversal / template / SSRF)
- 暗号 / ハッシュ / 乱数のアルゴリズム選定誤り (MD5 / SHA-1 / DES / 予測可能 RNG / IV 再利用)
- エラー / ログ経路から token / PII / stack trace 内 secret が漏れる経路

これらを repo 固有の sensitive area に紐付けて指摘し、merge 前に塞ぐことが目的。

## 2. Phase 1 で読むべき入力

参照する Phase 1 出力 key:
- `tech_stack.languages`: 言語ごとの代表的脆弱性パターンを観点に織り込む
- `convention_docs`: 認証・暗号方針が steering / docs に書かれている場合に rule 文面へ引用
- `sensitive_areas`: `internal/auth/` / `cmd/server/` / `pkg/crypto/` 等の重点対象パスとして applies_when / prompt に挿入
- `dependencies`: 認証 / crypto / JWT 系ライブラリの存在を prompt の観点に反映 (例: `golang-jwt/jwt`, `bcrypt`, `crypto/rand`)

追加で行う Read / Glob / Grep:
- Glob: `**/auth/**` / `**/security/**` / `**/crypto/**` / `**/secrets/**` — 認証 / 暗号領域の網羅
- Glob: `.env*` / `**/*credential*` / `**/*secret*` — 機微ファイルの所在確認
- Grep: `(?i)(api[_-]?key|secret|token|password)\s*[:=]\s*["'][^"']+["']` — repo 内ハードコード検出
- Grep: `md5\.|sha1\.|DES|rand\.Int\(` (Go) / `hashlib\.md5|hashlib\.sha1|random\.` (Python) — 弱い暗号 / RNG
- Read: `SECURITY.md` / `docs/security/*.md` / steering の security 系ファイル — 既存運用ルール抽出

## 3. `applies_when` の作り方

判定軸:
- 変更対象パス: `sensitive_areas` の値、認証 / 暗号 / 入出力境界配下を列挙
- 変更内容: credential / token / secret / 認証 / 認可 / 暗号 / RNG / 入力検証 / SQL / command / path traversal / log にまつわる diff
- 関連ファイル種別: `.env*` / `*.config.*` / `Dockerfile` / IaC / secrets store 関連
- 言語別キーワード: 言語固有の脆弱関数 (`os.system` / `eval` / `exec.Command` / `subprocess`)

作例 (2 件以上):

**作例 A (最小)**: Phase 1 が `tech_stack.languages=[Go]` / `sensitive_areas=[internal/auth]` を返した場合
```
この rule は Go repo の internal/auth 配下を含む変更、または認証 / credential / 機微情報 / 入力境界 (HTTP handler / CLI 引数 / DB クエリ生成) に関わる変更に対して適用する。.env / Dockerfile / IaC 系ファイルの変更も対象とする。
```

**作例 B (充実)**: Phase 1 が `tech_stack.languages=[TypeScript, Python]` / `sensitive_areas=[apps/api/auth, services/billing]` / `dependencies=[jsonwebtoken, bcrypt, stripe]` を返した場合
```
この rule は TypeScript / Python repo の apps/api/auth, services/billing 配下を含む変更、または認証 (jsonwebtoken / bcrypt) / 決済データ (stripe) / API key / PII / 入力境界 (Express handler / FastAPI route / SQL クエリ生成) に関わる変更に対して適用する。.env* / *.config.ts / Dockerfile / k8s manifest / secrets store の差分も対象とする。
```

## 4. `prompt` の作り方

骨格 (重要観点 5 項):
1. 機微情報 (credential / API key / secret / token / PII) のハードコードと、ログ・エラーメッセージ・スタックトレース経由の漏洩
2. 認証 / 認可境界の整合 (middleware の適用漏れ、権限チェック取り違え、role escalation)
3. 外部入力に対するサニタイズ・バリデーション (SQL / command / path traversal / SSRF / template injection)
4. 暗号 / ハッシュ / 乱数の選定 (弱いアルゴリズム回避、IV / nonce 一意性、CSPRNG 使用)
5. secrets / config の取り扱い (env 経由の注入、コミット混入の防止、ローテーション可能性)

Phase 1 所見をどう織り込むか:
- 具体パス: `sensitive_areas[*]` を観点 1〜3 の主語に挿入する (例: 「`internal/auth/` および `cmd/server/middleware/` を重点的に」)
- シンボル名: `convention_docs` から抽出した固有 API / middleware 名 (例: `RequireAuth` / `WithRBAC`) を観点 2 に挿入
- tech_stack に応じた強調: Go なら `exec.Command` / `database/sql` / `crypto/rand`, TypeScript なら型境界・`eval` / `Function`, Python なら `eval` / `pickle` / `subprocess(shell=True)` / `yaml.load`, Rust なら `unsafe` ブロック内の境界
- 依存ライブラリ: `dependencies` に `jsonwebtoken` / `bcrypt` / `crypto-js` / `golang-jwt` 等が見えれば、その API の誤用 (アルゴリズム none / round 数不足 / verify 省略) を観点 4 に追加

作例 (最小例): 作例 A に対応
```
以下の観点で internal/auth 配下および周辺を重点的にレビューしてください。
1. credential / API key / secret 等の機微情報がコード内にハードコードされていないか、ログ・エラーメッセージへ出力されていないか確認する。
2. 認証 / 認可境界 (middleware / RBAC) が正しく敷かれており、権限昇格経路がないか確認する。
3. ユーザ入力 / 外部入力に対するサニタイズ・バリデーションが行われているか確認する (Go では SQL injection / command injection / path traversal / SSRF を観点として用い、`exec.Command` や `database/sql` の使い方を点検する)。
4. 暗号 / ハッシュ / 乱数生成において脆弱なアルゴリズム (MD5 / SHA-1 / DES / `math/rand`) が使われていないか確認し、`crypto/rand` 使用と IV / nonce 一意性を確認する。
5. .env / config / Dockerfile / IaC への secret 混入経路がないか確認する。
指摘は重大度 (critical / high / medium / low) を付け、具体的な修正方針を 1 行で添える。
```

作例 (充実例): 作例 B に対応
```
以下の観点で apps/api/auth, services/billing 配下および周辺を重点的にレビューしてください。
1. credential / API key / Stripe secret / 顧客 PII がコード内にハードコードされていないか、Express / FastAPI のレスポンスやログに含まれていないか確認する。
2. apps/api/auth の認証 (jsonwebtoken / bcrypt) と services/billing の権限境界が一貫しているか、RBAC / scope check が漏れていないか確認する。`requireAuth` / `requireScope` 系 middleware の適用範囲を点検する。
3. ユーザ入力 / 外部入力に対するサニタイズ・バリデーションが行われているか確認する。TypeScript 側は `eval` / `Function` / 動的 import / SQL builder の query 引数を、Python 側は `eval` / `pickle` / `subprocess(shell=True)` / `yaml.load` / SQLAlchemy の raw クエリを観点として用いる。
4. JWT のアルゴリズムが `none` 許容になっていないか、bcrypt の cost が適切か、乱数生成に CSPRNG (`crypto.randomBytes` / `secrets.token_bytes`) が使われているかを確認する。MD5 / SHA-1 を新規導入していないこと。
5. Stripe API key / DB 接続情報 / OAuth client secret が .env* / *.config.ts / Dockerfile / k8s manifest にコミット混入していないか、ローテーション手順が壊れていないかを確認する。
指摘は重大度 (critical / high / medium / low) を付け、具体的な修正方針を 1 行で添える。
```

## 5. 合成後の自己点検項目

合成した `applies_when` / `prompt` が以下を満たすか機械的にチェックする:

- [ ] 観点リスト (prompt) に Phase 1 から得た具体パス / シンボル名が **少なくとも 1 つ** 含まれている (汎用文章で済んでいない)
- [ ] 重大度の付け方 (critical / high / medium / low 等) が prompt 内に明示されている
- [ ] applies_when に「いつ発火するか」の判定軸が **2 つ以上** 含まれている (例: パス条件 + 内容条件 + ファイル種別条件)
- [ ] applies_when / prompt に未置換の `<UPPER_SNAKE>` token が残存していない
- [ ] prompt が「(観点が思いつかなければ何でも報告して)」のような責務放棄になっていない
- [ ] OWASP Top 10 (Broken Access Control / Cryptographic Failures / Injection / Security Misconfiguration / Vulnerable Components 等) のうち少なくとも 3 種に観点が対応している
- [ ] 言語固有の脆弱関数名 (Go なら `exec.Command` / Python なら `eval` 等) が tech_stack に対応する形で観点に登場している
