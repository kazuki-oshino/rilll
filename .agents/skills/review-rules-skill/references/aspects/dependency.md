# dependency-review 観点合成ガイド

> 本ガイドは review-rules-skill Phase 4 が `applies_when` / `prompt` を合成するための観点別手順書である。テンプレ (種) `templates/dependency-review.yaml` と本ガイドを Read し、Phase 1 のリポ調査結果を織り込んで合成する。

## 1. 観点の目的

依存観点では以下クラスの問題を検出する:

- 不要・過剰な依存追加 (標準ライブラリで足りる / 既存依存と機能重複 / 一機能のために巨大ライブラリ採用)
- 既知脆弱性を含むバージョンの採用 (CVE / GHSA Advisory) や、放置された古い major
- ライセンス互換性の破壊 (例: MIT/Apache repo に GPL/AGPL を混ぜる、商用利用不可ライセンスを取り込む)
- バージョン固定 / lockfile の不整合 (pin なし `^` / `~` のみ、lockfile 未更新、direct と indirect の不一致)
- supply chain 的リスク (typo squatting、メンテ停止、単独メンテナ、postinstall script、未署名パッケージ)

これらを Phase 1 で抽出した dependencies に紐付けて指摘し、merge 前に塞ぐことが目的。

## 2. Phase 1 で読むべき入力

参照する Phase 1 出力 key:
- `dependencies`: 依存マニフェスト (`go.mod` / `package.json` / `pyproject.toml` / `Cargo.toml` / `Gemfile` / `requirements*.txt` 等) と既知依存リストを観点に挿入
- `tech_stack.languages`: 言語ごとのエコシステム特性 (npm / Go modules / pip / cargo / gem) を観点に反映
- `convention_docs`: 依存追加ポリシー (例: `CONTRIBUTING.md` の依存方針 / steering の `dependencies.md`) を引用

追加で行う Read / Glob / Grep:
- Glob: `go.mod` / `go.sum` / `package.json` / `package-lock.json` / `pnpm-lock.yaml` / `yarn.lock` / `pyproject.toml` / `poetry.lock` / `requirements*.txt` / `Cargo.toml` / `Cargo.lock` / `Gemfile` / `Gemfile.lock` — マニフェスト & lockfile の所在
- Glob: `**/vendor/**` / `**/node_modules/**` (gitignore 確認用) / `.npmrc` / `.yarnrc*` / `renovate.json` / `dependabot.yml` — 取り込み境界と更新方針
- Read: `LICENSE` / `NOTICE` / `THIRD_PARTY_NOTICES*` — ライセンス境界の把握
- Grep: `(github|gitlab)\.com/[^/]+/[^/\s]+` をマニフェストに対して — 依存元 host の分布
- Grep: `"postinstall"|"preinstall"` を package.json に対して — supply chain リスク兆候

## 3. `applies_when` の作り方

判定軸:
- 依存マニフェストの追加・変更・削除 (`go.mod` / `package.json` / `pyproject.toml` / `Cargo.toml` / `Gemfile`)
- lockfile の差分 (`go.sum` / `*-lock.*` / `Cargo.lock` / `Gemfile.lock`)
- vendoring / sub-module 追加, `vendor/` 配下の更新
- ライセンス関連ファイル (`LICENSE` / `NOTICE` / `THIRD_PARTY_NOTICES*`) の変更

作例 (2 件以上):

**作例 A (最小)**: Phase 1 が `tech_stack.languages=[Go]` / `dependencies=[go.mod, go.sum]` を返した場合
```
この rule は Go repo の go.mod / go.sum の変更、vendor/ 配下の更新、LICENSE / NOTICE 系ファイルの変更を含むコミットに対して適用する。
```

**作例 B (充実)**: Phase 1 が `tech_stack.languages=[TypeScript, Python]` / `dependencies=[package.json, pnpm-lock.yaml, pyproject.toml, poetry.lock]` を返した場合
```
この rule は TypeScript / Python repo の package.json / pnpm-lock.yaml / pyproject.toml / poetry.lock のいずれかの変更、`renovate.json` / `dependabot.yml` の更新、LICENSE / NOTICE / THIRD_PARTY_NOTICES* の変更、`postinstall` / `preinstall` script の追加を含む変更に対して適用する。
```

## 4. `prompt` の作り方

骨格 (重要観点 5 項):
1. 追加された依存の必要性 (標準ライブラリ / 既存依存で代替できないか、機能重複がないか)
2. 採用バージョンの健全性 (既知 CVE / GHSA Advisory がないか、最新メジャーから極端に遅れていないか)
3. ライセンス互換性 (repo 全体ライセンスと衝突しないか、コピーレフト系の伝播が許容されるか)
4. バージョン固定 / lockfile 整合 (`^` / `~` のみのレンジ採用、lockfile の未更新、direct / indirect の zureru)
5. supply chain リスク (typo squatting、メンテ停止 / 単独メンテナ、postinstall script、未署名・未検証パッケージ)

Phase 1 所見をどう織り込むか:
- 具体パス: `dependencies` で検出されたマニフェストファイル名を観点 1, 4 に挿入
- 言語別エコシステム: Go なら `go.mod` の major / `replace` directive、npm なら `^` / `~` / `peerDependencies` / `engines` / postinstall、Python なら `pyproject.toml` の `[tool.poetry.dependencies]` / `requirements.txt` の pin、Rust なら `Cargo.toml` の `default-features = false` / `features`
- 既存依存名: `dependencies` から拾った具体パッケージ名を観点 2, 3 で名指しして既知 advisory との突合を促す
- convention_docs に依存追加ポリシーがあれば観点 1 に引用 (例: 「`CONTRIBUTING.md` の依存追加ガイドラインに従っているか」)

作例 (最小例): 作例 A に対応
```
以下の観点で go.mod / go.sum / vendor の差分をレビューしてください。
1. 追加された依存が標準ライブラリ (`net/http` / `encoding/json` / `crypto/*` 等) や既存依存で代替できないか、機能重複がないかを確認する。
2. 採用バージョンに既知の脆弱性 (CVE / GHSA Advisory) がないか、最新メジャーから著しく遅れていないかを確認する。
3. 追加依存のライセンスが repo 全体のライセンスと互換か (GPL / AGPL の混入が許容されるか) を確認する。
4. go.mod の require / replace / exclude と go.sum / vendor の整合が取れているか、indirect 依存に意図しないものが入っていないかを確認する。
5. 取り込み元リポジトリのメンテナ状況 (最終 commit / メンテナ数 / fork 元) と typo squatting (近接した module path) のリスクを確認する。
指摘は重大度 (critical / high / medium / low) を付け、修正方針を 1 行で添える。
```

作例 (充実例): 作例 B に対応
```
以下の観点で package.json / pnpm-lock.yaml / pyproject.toml / poetry.lock の差分をレビューしてください。
1. 追加された npm / Python 依存が標準ライブラリ (`fs` / `crypto` / `hashlib` / `pathlib` 等) や既存依存で代替できないか、機能重複 (lodash / underscore など) がないか確認する。
2. 採用バージョンに既知の脆弱性 (npm GHSA / PyPI Safety DB) がないか、最新メジャーから著しく遅れていないかを確認する。
3. 追加依存のライセンスが repo 全体のライセンスと互換か (GPL / AGPL / SSPL の混入が許容されるか) を確認し、必要なら THIRD_PARTY_NOTICES への追記を提案する。
4. package.json の `^` / `~` レンジ、`peerDependencies` / `engines` 整合、pnpm-lock.yaml の未更新、pyproject.toml の `[tool.poetry.dependencies]` の pin と poetry.lock の整合を確認する。
5. supply chain リスクとして、`postinstall` / `preinstall` script の追加、typo squatting (`reqeusts` / `lodahs` 等)、メンテ停止プロジェクト、単独メンテナへの依存、未署名パッケージの取り込みを確認する。
指摘は重大度 (critical / high / medium / low) を付け、修正方針を 1 行で添える。
```

## 5. 合成後の自己点検項目

合成した `applies_when` / `prompt` が以下を満たすか機械的にチェックする:

- [ ] 観点リスト (prompt) に Phase 1 から得たマニフェスト名 (go.mod / package.json 等) が **少なくとも 1 つ** 含まれている (汎用文章で済んでいない)
- [ ] 重大度の付け方 (critical / high / medium / low 等) が prompt 内に明示されている
- [ ] applies_when に「いつ発火するか」の判定軸が **2 つ以上** 含まれている (例: マニフェスト変更 + lockfile 変更 + LICENSE 変更)
- [ ] applies_when / prompt に未置換の `<UPPER_SNAKE>` token が残存していない
- [ ] prompt が「(観点が思いつかなければ何でも報告して)」のような責務放棄になっていない
- [ ] 5 観点 (必要性 / 脆弱性 / ライセンス / バージョン固定 / supply chain) が prompt に揃っている
- [ ] tech_stack に対応するエコシステム固有要素 (`peerDependencies` / `replace` directive / poetry の pin 等) が観点に登場している
