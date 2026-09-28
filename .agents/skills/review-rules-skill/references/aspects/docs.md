# docs-review 観点合成ガイド

> 本ガイドは review-rules-skill Phase 4 が `applies_when` / `prompt` を合成するための観点別手順書である。テンプレ (種) `templates/docs-review.yaml` と本ガイドを Read し、Phase 1 のリポ調査結果を織り込んで合成する。

## 1. 観点の目的

ドキュメント観点では以下クラスの問題を検出する:

- README / docs 配下 / 公開 API リファレンスがコード変更に追従していない (関数シグネチャ・オプション・環境変数・既定値の zureru)
- CHANGELOG / リリースノート / migration guide に user-impacting な変更が記録されていない
- 命名規約 / 用語統一 / コーディング規約 (steering) からの逸脱
- コード内コメント (Doc comment / docstring) が実装と矛盾している、または公開シンボルに対して不足
- サンプル / quickstart / コードブロックが現行 API でビルドできない・動作しない

これらを repo の docs レイアウトに即して指摘し、ドキュメント腐敗を merge 前に塞ぐことが目的。

## 2. Phase 1 で読むべき入力

参照する Phase 1 出力 key:
- `docs_layout`: README / docs/ / handbook / website / 等の所在を applies_when と prompt の参照先に挿入
- `convention_docs`: steering / `CONTRIBUTING.md` / `STYLE.md` 等から命名規約・用語統一・doc comment ポリシーを引用
- `tech_stack.languages`: 言語ごとの doc comment 形式 (godoc / TSDoc / docstring / rustdoc) を観点に反映

追加で行う Read / Glob / Grep:
- Glob: `README*` / `CHANGELOG*` / `RELEASE_NOTES*` / `docs/**/*.md` / `website/**/*.md` / `handbook/**/*.md` — ドキュメントファイル網羅
- Glob: `**/*.md` direct under repo root — top-level ガイド類
- Grep: `^##\s` を docs/ 配下に対して — セクション構成把握 (どこが API 一覧 / migration / quickstart か)
- Grep: 公開シンボル名 (例: 関数 / コマンド / 環境変数) を docs/ 配下に対して — コード ↔ docs 言及の対応把握
- Read: `README.md` / `CHANGELOG.md` / `docs/index.md` / `mkdocs.yml` / `docusaurus.config.js` — docs ビルド設定と入口の把握

## 3. `applies_when` の作り方

判定軸:
- 公開 API / CLI / 環境変数 / 設定キーの追加・変更・削除を伴う diff
- 既存 doc がカバーする領域への変更 (`docs/api/` / `docs/cli/` / README の代表例セクション)
- `*.md` 自体の変更 (整合性チェックの逆方向: docs 単独変更でも cross check)
- CHANGELOG / migration guide が更新されていないコードの user-impacting 変更

作例 (2 件以上):

**作例 A (最小)**: Phase 1 が `docs_layout=[README.md, docs/]` / `tech_stack.languages=[Go]` を返した場合
```
この rule は Go repo の公開関数 / コマンド / 環境変数の追加・変更・削除を含む変更、`docs/` 配下または README.md の変更、または CHANGELOG の更新を伴う変更に対して適用する。
```

**作例 B (充実)**: Phase 1 が `docs_layout=[README.md, docs/, website/docs/]` / `tech_stack.languages=[TypeScript]` / `convention_docs=[CONTRIBUTING.md, docs/style-guide.md]` を返した場合
```
この rule は TypeScript repo の公開モジュール / API / CLI / 環境変数 / 設定キーの追加・変更・削除を含む変更、docs/ または website/docs/ または README.md の変更、CHANGELOG.md / RELEASE_NOTES.md の更新を伴う変更、TSDoc コメントを含むファイルの変更に対して適用する。CONTRIBUTING.md / docs/style-guide.md の規約逸脱検出も含む。
```

## 4. `prompt` の作り方

骨格 (重要観点 5 項):
1. 公開 API / CLI / 環境変数 / 設定キー変更が README / docs / リファレンスに反映されているか
2. CHANGELOG / リリースノート / migration guide に user-impacting 変更が記録されているか (semver 整合)
3. 命名規約 / 用語統一 / 文体ルールが steering / style guide 通りに守られているか
4. 公開シンボルの doc comment / docstring が実装と一致しているか (引数・戻り値・エラー条件)
5. サンプルコード / quickstart / 設定例が現行 API でビルド可能かつ動作するか

Phase 1 所見をどう織り込むか:
- 具体パス: `docs_layout` の値を観点 1, 5 の参照先に挿入する (例: 「`docs/api/` / `website/docs/` 配下」)
- シンボル名: diff から検出された公開関数名 / コマンド名 / 環境変数名を観点 1 / 4 で名指し
- tech_stack に応じた強調: Go なら godoc / `Example*` テスト, TypeScript なら TSDoc / `@deprecated` タグ, Python なら docstring / Sphinx, Rust なら rustdoc / `# Examples`
- convention_docs から抽出した固有規約 (用語表記 / 見出しレベル / 箇条書きスタイル) を観点 3 に引用

作例 (最小例): 作例 A に対応
```
以下の観点でドキュメント整合性をレビューしてください。
1. 変更された公開関数 / コマンド / 環境変数が README.md および docs/ 配下に反映されているか確認する。godoc コメントが実装と一致していること。
2. CHANGELOG に user-impacting な変更 (シグネチャ変更 / 既定値変更 / 削除) が記録されているか、semver の bump 方針と整合しているか確認する。
3. 命名規約 / 用語統一 / 文体が repo の既存ドキュメントと整合しているか確認する。
4. godoc / Example テストが実装と矛盾していないか、引数・戻り値・エラー条件が正しく説明されているか確認する。
5. README / docs/ のサンプルコードが現行 API でビルド可能か (古い API 名 / 廃止された引数が残っていないか) を確認する。
指摘は重大度 (high / medium / low) を付け、修正方針を 1 行で添える。
```

作例 (充実例): 作例 B に対応
```
以下の観点でドキュメント整合性をレビューしてください。
1. 変更された TypeScript モジュール / API / CLI / 環境変数 / 設定キーが README.md, docs/, website/docs/ の該当ページに反映されているか確認する。TSDoc コメント (`@param` / `@returns` / `@deprecated`) が実装と一致していること。
2. CHANGELOG.md / RELEASE_NOTES.md に user-impacting な変更 (シグネチャ変更 / 既定値変更 / 削除 / 移行手順) が記録され、semver の bump 方針と整合しているか確認する。`@deprecated` タグから removal までのリードタイムが style-guide 通りであること。
3. CONTRIBUTING.md / docs/style-guide.md に従った命名規約 / 用語統一 / 見出しレベル / 箇条書きスタイルが守られているか確認する。
4. 公開関数の TSDoc が実装と矛盾していないか、引数・戻り値・throw 条件が正しく説明されているか確認する。
5. README / docs/ / website/docs/ のサンプルコード・quickstart・設定例が現行 API でビルド可能か (古い import path / 廃止された option が残っていないか) を確認する。
指摘は重大度 (high / medium / low) を付け、修正方針を 1 行で添える。
```

## 5. 合成後の自己点検項目

合成した `applies_when` / `prompt` が以下を満たすか機械的にチェックする:

- [ ] 観点リスト (prompt) に Phase 1 から得た具体パス (`docs/` / README / website 等) が **少なくとも 1 つ** 含まれている (汎用文章で済んでいない)
- [ ] 重大度の付け方 (high / medium / low 等) が prompt 内に明示されている
- [ ] applies_when に「いつ発火するか」の判定軸が **2 つ以上** 含まれている (例: コード変更条件 + docs 自体の変更条件)
- [ ] applies_when / prompt に未置換の `<UPPER_SNAKE>` token が残存していない
- [ ] prompt が「(観点が思いつかなければ何でも報告して)」のような責務放棄になっていない
- [ ] doc comment 形式 (godoc / TSDoc / docstring / rustdoc 等) が tech_stack に対応する形で観点に登場している
- [ ] CHANGELOG / リリースノート / migration guide のいずれかが観点 2 に明示されている
