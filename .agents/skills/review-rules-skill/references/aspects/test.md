# test-review 観点合成ガイド

> 本ガイドは review-rules-skill Phase 4 が `applies_when` / `prompt` を合成するための観点別手順書である。テンプレ (種) `templates/test-review.yaml` と本ガイドを Read し、Phase 1 のリポ調査結果を織り込んで合成する。

## 1. 観点の目的

テスト観点では以下クラスの問題を検出する:

- 公開関数 / 公開 API / コマンドの追加・変更にテストが添えられていない
- 境界条件 / エラーパス / nil・empty・極大値・並行アクセスのケース欠落
- フィクスチャ / モック方針が repo の既存規約と整合しない (グローバル fixture 共有 / mock 過多 / spy で実装結合)
- テストレイヤ (unit / integration / e2e) の混在・取り違え (unit に外部 I/O が紛れる、e2e でロジック単体を試す)
- 性能・耐障害テスト (race / load / fuzz / property-based) が必要な変更で省略されている

これらを repo の test レイアウトに即して指摘し、merge 前にカバレッジ・テスト品質を担保することが目的。

## 2. Phase 1 で読むべき入力

参照する Phase 1 出力 key:
- `test_layout`: テスト配置 (`*_test.go` / `__tests__/` / `tests/` / `spec/` / `e2e/`) と命名規約を観点に挿入
- `tech_stack.languages`: 言語別テストフレームワーク (`go test` / Jest / Vitest / pytest / unittest / RSpec / cargo test) を反映
- `convention_docs`: テスト方針 (`CONTRIBUTING.md` のテスト節 / `docs/testing.md` / steering の `testing.md`) からモック方針・カバレッジ目標を引用

追加で行う Read / Glob / Grep:
- Glob: `**/*_test.go` / `**/test_*.py` / `**/*.test.ts` / `**/*.spec.ts` / `**/__tests__/**` / `tests/**` / `e2e/**` — テストファイル所在
- Glob: `Makefile` / `justfile` / `package.json` (`scripts.test`) / `pyproject.toml` (`[tool.pytest.ini_options]`) — テスト実行 entrypoint
- Read: `docs/testing.md` / `CONTRIBUTING.md` のテスト節 — 既存方針の抽出
- Grep: `t\.Parallel\(\)` / `testing/synctest` / `goleak` (Go) / `vi\.mock|jest\.mock` (TS) / `monkeypatch|fixture` (pytest) — モック・並行テスト規約の検出
- Grep: `func Example` / `BenchmarkX` / `FuzzX` / `Property` — 補助テストの存在確認

## 3. `applies_when` の作り方

判定軸:
- 公開関数 / 公開 API / CLI / 環境変数の追加・変更を伴う diff
- テストファイル自体の変更 (`*_test.go` / `*.test.ts` / `tests/` / `__tests__/` 配下)
- 性能 / 並行 / 耐障害が関係する領域 (goroutine / async / Worker / DB) の変更
- フィクスチャ / モック / テストヘルパ (`testdata/` / `__mocks__/` / `conftest.py`) の変更

作例 (2 件以上):

**作例 A (最小)**: Phase 1 が `tech_stack.languages=[Go]` / `test_layout=[内部 *_test.go]` を返した場合
```
この rule は Go repo の公開関数 / 公開コマンドの追加・変更を含む変更、または `*_test.go` / `testdata/` 配下の変更に対して適用する。goroutine / channel / race を伴う領域の変更も対象とする。
```

**作例 B (充実)**: Phase 1 が `tech_stack.languages=[TypeScript, Python]` / `test_layout=[apps/web/__tests__, services/api/tests, e2e/]` / `convention_docs=[docs/testing.md]` を返した場合
```
この rule は TypeScript / Python repo の公開モジュール / API / CLI の追加・変更、apps/web/__tests__ / services/api/tests / e2e/ 配下のテストファイル変更、conftest.py / __mocks__ / fixtures の変更、async / Worker / DB アクセス領域の変更に対して適用する。docs/testing.md の方針 (mock 範囲 / カバレッジ目標) からの逸脱検出も含む。
```

## 4. `prompt` の作り方

骨格 (重要観点 5 項):
1. 追加・変更された公開関数 / API に unit テストが添えられているか、戻り値・エラー条件・境界値が網羅されているか
2. テストレイヤ (unit / integration / e2e) の分離が守られているか (unit に外部 I/O が紛れていない、e2e がロジック細部に踏み込んでいない)
3. フィクスチャ / モック方針が repo 規約と整合しているか (過剰モックによる実装結合、グローバル状態共有)
4. 並行 / race / fuzz / property-based / benchmark 等、対象領域に必要な補助テストが省略されていないか
5. テストの可読性・保守性 (Arrange/Act/Assert / Given-When-Then の構造、命名、test fixture の命名規則)

Phase 1 所見をどう織り込むか:
- 具体パス: `test_layout` の値を観点 1, 2 の参照先に挿入する (例: 「`internal/auth/*_test.go` / `e2e/` 配下」)
- フレームワーク: tech_stack に応じて Go なら `t.Run` / `t.Parallel` / `testing/synctest`、TypeScript なら Jest / Vitest の `describe` / `it` / `vi.mock`、Python なら pytest の `parametrize` / `fixture` / `monkeypatch`、Rust なら `#[test]` / `proptest` を観点に挿入
- 補助テスト: goroutine 多用箇所なら race / `goleak`、ホットパスなら benchmark、入力多様性が必要なら fuzz / property を観点 4 に明示
- convention_docs から抽出した方針 (例: 「カバレッジ 80% 以上」「mock は外部 I/O のみ」) を観点 3 に引用

作例 (最小例): 作例 A に対応
```
以下の観点で Go テストの追加・変更をレビューしてください。
1. 追加・変更された公開関数 / コマンドに `*_test.go` が添えられ、戻り値・エラー条件・境界値 (空入力 / nil / 巨大入力) が網羅されているか確認する。
2. unit テストに外部 I/O (DB / network / filesystem) が紛れていないか、integration / e2e と責務が分離されているかを確認する。
3. testdata / mock 利用が repo の既存パターンに沿っているか、グローバル状態共有による flaky の温床になっていないかを確認する。
4. goroutine / channel を扱う領域に対して `-race` 前提のテスト、`testing/synctest` または `goleak` 等の漏れ検出が組まれているかを確認する。Benchmark / Fuzz / Example が必要な場合、追加されているかを確認する。
5. テストの命名 (`TestX_When_Then`) と Arrange/Act/Assert 構造、`t.Run` のサブテスト粒度が読み手に優しいかを確認する。
指摘は重大度 (high / medium / low) を付け、修正方針を 1 行で添える。
```

作例 (充実例): 作例 B に対応
```
以下の観点で TypeScript / Python のテスト追加・変更をレビューしてください。
1. 追加・変更された公開モジュール / API / CLI に対して apps/web/__tests__ / services/api/tests のテストが添えられ、戻り値・throw / raise 条件・境界値 (空 / null / undefined / 極大) が網羅されているか確認する。
2. unit テスト (Vitest / pytest) に DB / HTTP / filesystem が紛れていないか、e2e/ と責務が分離されているかを確認する。e2e がロジック内部のエッジケースまで踏み込んでいないこと。
3. `vi.mock` / `jest.mock` / `monkeypatch` / conftest.py fixture が repo 既存パターンと整合しているか、過剰 mock で実装と結合しすぎていないかを docs/testing.md の方針と突き合わせて確認する。
4. async / Worker / DB アクセス領域に対し、Promise の待ち合わせ漏れ・event loop block・並行アクセス race を検出するテストが組まれているか、必要なら property-based (fast-check / hypothesis) や benchmark を追加すべきかを確認する。
5. describe / it (Vitest) / class TestX (pytest) の命名と Arrange/Act/Assert 構造、parametrize / table-driven の活用が読み手に優しいかを確認する。
指摘は重大度 (high / medium / low) を付け、修正方針を 1 行で添える。
```

## 5. 合成後の自己点検項目

合成した `applies_when` / `prompt` が以下を満たすか機械的にチェックする:

- [ ] 観点リスト (prompt) に Phase 1 から得た具体パス (`*_test.go` / `__tests__/` / `tests/` 等) が **少なくとも 1 つ** 含まれている (汎用文章で済んでいない)
- [ ] 重大度の付け方 (high / medium / low 等) が prompt 内に明示されている
- [ ] applies_when に「いつ発火するか」の判定軸が **2 つ以上** 含まれている (例: 公開 API 変更 + テストファイル変更)
- [ ] applies_when / prompt に未置換の `<UPPER_SNAKE>` token が残存していない
- [ ] prompt が「(観点が思いつかなければ何でも報告して)」のような責務放棄になっていない
- [ ] テストレイヤ (unit / integration / e2e) のいずれかが prompt 内で明示されている
- [ ] tech_stack に対応するテストフレームワーク名 (Go test / Jest / Vitest / pytest / RSpec / cargo test 等) が観点に登場している
