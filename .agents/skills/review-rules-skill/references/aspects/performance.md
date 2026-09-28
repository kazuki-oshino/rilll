# performance-review 観点合成ガイド

> 本ガイドは review-rules-skill Phase 4 が `applies_when` / `prompt` を合成するための観点別手順書である。テンプレ (種) `templates/performance-review.yaml` と本ガイドを Read し、Phase 1 のリポ調査結果を織り込んで合成する。

## 1. 観点の目的

パフォーマンス観点では以下クラスの問題を検出する:

- hot path (毎リクエスト・毎ループ・毎フレーム実行される箇所) における計算量悪化 (O(N^2) / 不要なソート / 線形探索の重複)
- アロケーション過多 (毎回の `make` / `new` / 文字列連結 / boxing) によるヒープ圧と GC 負荷
- I/O / ネットワーク / DB の N+1 / 同期ブロッキング / batch 化漏れ / context cancel 未対応
- 並行性の劣化 (ロック粒度過大 / mutex を伴う長時間処理 / channel buffering 不足 / goroutine leak)
- DB クエリ計画の悪化 (インデックス未使用 / SELECT * / large IN / cross join)

これらを repo の hot path / runtime プロファイルに紐付けて指摘し、リグレッションを merge 前に検出することが目的。

## 2. Phase 1 で読むべき入力

参照する Phase 1 出力 key:
- `tech_stack.languages`: 言語別の hot path 観点 (Go なら escape analysis / pool、TS なら hot loop allocator、Python なら CPU bound vs IO bound、Rust なら `clone` / `Vec` realloc) を反映
- `convention_docs`: パフォーマンス方針 (`docs/performance.md` / steering の `performance.md`) があれば引用
- `dependencies`: パフォーマンス特性に影響する依存 (ORM / HTTP client / cache / queue) を観点に反映

追加で行う Read / Glob / Grep:
- Glob: `cmd/**` / `internal/**` / `pkg/**` / `apps/**` / `services/**` — hot path 領域候補の網羅
- Glob: `**/*.bench.go` / `**/benchmark*.{ts,js,py}` / `bench/**` — 既存ベンチマーク所在
- Grep: 大規模ファイル (1000 行以上) の検出 — hot path 候補の絞り込み (LSP / `wc -l`)
- Grep: `for\s+.*range` (Go) / `for.*of` (TS) / `for .* in` (Python) を hot path 候補配下に対して — ループ密度
- Grep: `make\(map|make\(\[\]` (Go) / `new Array|new Map` (TS) / `database/sql|gorm|sqlx|prisma|sqlalchemy` — アロケーション・DB アクセス検出
- Read: `docs/performance.md` / SLO 関連ドキュメント — 既存目標値の把握

## 3. `applies_when` の作り方

判定軸:
- hot path 領域 (Phase 1 で検出した `cmd/server/` / `internal/render/` 等) 配下の変更
- ループ / 再帰 / 大規模データ構造を扱うコードの変更
- DB クエリ / ORM 利用箇所の変更
- 並行処理 (goroutine / async / Worker / Promise.all) の変更
- ベンチマーク / プロファイル設定 (`*_bench*` / `pprof` / `profiler` 設定) の変更

作例 (2 件以上):

**作例 A (最小)**: Phase 1 が `tech_stack.languages=[Go]` / hot path 候補 `[internal/server, cmd/server]` を返した場合
```
この rule は Go repo の internal/server / cmd/server 配下を含む変更、ループ / goroutine / channel を扱うコードの変更、`database/sql` 利用箇所の変更、`*_bench*.go` / pprof 設定の変更に対して適用する。
```

**作例 B (充実)**: Phase 1 が `tech_stack.languages=[TypeScript, Python]` / hot path 候補 `[apps/web/src/render, services/api/handlers]` / `dependencies=[prisma, ioredis, fastapi, sqlalchemy]` を返した場合
```
この rule は TypeScript / Python repo の apps/web/src/render / services/api/handlers 配下を含む変更、prisma / sqlalchemy ORM クエリの追加・変更、ioredis / Redis アクセスの追加・変更、async / Promise.all / asyncio.gather など並行処理の変更、benchmark / profiler 設定の変更に対して適用する。
```

## 4. `prompt` の作り方

骨格 (重要観点 5 項):
1. hot path における計算量・データ構造選定 (O(N^2) / 不要ソート / 線形探索の重複の有無)
2. アロケーション最適化 (per-iteration 確保の回避、文字列連結 / boxing / clone の最小化、pool 利用)
3. I/O / ネットワーク / DB の N+1・同期ブロッキング・batch 化・context cancel 対応
4. 並行性の取り扱い (ロック粒度、goroutine / Worker leak、channel / queue サイズ、async cancel propagation)
5. DB クエリ計画 (インデックス利用、SELECT 列の選択、large IN / cross join 回避、prepared statement 利用)

Phase 1 所見をどう織り込むか:
- 具体パス: hot path 候補のパスを観点 1, 2 の参照先に挿入する (例: 「`internal/render/` 配下のループ」)
- 言語別強調: Go なら escape analysis / `sync.Pool` / `bytes.Buffer` / `strings.Builder`, TypeScript なら hot loop での `.map`/`.filter` 連鎖避け、Python なら `numpy` / vectorize / GIL の影響、Rust なら `Vec::with_capacity` / `clone` 削減
- DB 依存: `dependencies` から ORM / DB driver を抽出し、観点 3, 5 で固有名 (Prisma / SQLAlchemy / GORM / sqlx) と共に N+1 / preload / select_related の有無を点検
- ベンチマーク: 既存 benchmark / profile 結果があるなら観点 1, 2 の根拠データとして「リグレッション前後の比較」を要求

作例 (最小例): 作例 A に対応
```
以下の観点で internal/server / cmd/server の変更をレビューしてください。
1. hot path (リクエスト処理経路) における計算量と選択データ構造を確認する。O(N^2) ループ、不要なソート、線形探索の重複がないか、map / slice の使い分けが適切かを点検する。
2. per-iteration の `make` / `new` / 文字列連結 / boxing を最小化できるか、`sync.Pool` / `bytes.Buffer` / `strings.Builder` の活用余地がないかを確認する。
3. database/sql / HTTP client 利用箇所で N+1 / 同期ブロッキング / context cancel 未対応 / batch 化漏れがないかを確認する。
4. goroutine / channel の取り扱いで、ロック粒度過大、`select` での leak、buffer サイズ不足、`-race` 検知漏れがないかを確認する。
5. DB クエリの計画 (EXPLAIN 視点でのインデックス利用、SELECT 列、large IN / cross join 回避、prepared statement) が崩れていないかを確認する。
指摘は重大度 (high / medium / low) を付け、可能ならば benchmark での確認方針を 1 行添える。
```

作例 (充実例): 作例 B に対応
```
以下の観点で apps/web/src/render / services/api/handlers の変更をレビューしてください。
1. hot path (SSR レンダリング / API ハンドラ) における計算量と選択データ構造を確認する。`.map` / `.filter` / list comprehension の連鎖、不要なソート、線形探索の重複、巨大配列の都度作成がないか点検する。
2. per-call の `new Array` / `new Map` / `dict` 作成、文字列連結、JSON serialize / deserialize 反復を最小化できるか、Buffer / streaming / cache の活用余地がないかを確認する。
3. Prisma / SQLAlchemy のクエリで N+1 (preload / select_related / include 未指定)、同期ブロッキング (asyncio で sync 呼び出し)、ioredis / Redis MGET batch 化漏れ、context / AbortSignal 未伝播がないかを確認する。
4. `Promise.all` / `asyncio.gather` のキャンセル伝播、ロック / mutex 粒度、Worker leak、event loop block (CPU bound 同期処理) がないかを確認する。
5. Prisma / SQLAlchemy が生成する SQL のインデックス利用、SELECT 列の選択、large IN / cross join 回避、prepared statement の利用を確認する。可能なら EXPLAIN 結果を要求する。
指摘は重大度 (high / medium / low) を付け、可能ならば benchmark / profiler でのリグレッション確認方針を 1 行添える。
```

## 5. 合成後の自己点検項目

合成した `applies_when` / `prompt` が以下を満たすか機械的にチェックする:

- [ ] 観点リスト (prompt) に Phase 1 から得た hot path パスが **少なくとも 1 つ** 含まれている (汎用文章で済んでいない)
- [ ] 重大度の付け方 (high / medium / low 等) が prompt 内に明示されている
- [ ] applies_when に「いつ発火するか」の判定軸が **2 つ以上** 含まれている (例: hot path 配下条件 + 並行 / DB 領域条件)
- [ ] applies_when / prompt に未置換の `<UPPER_SNAKE>` token が残存していない
- [ ] prompt が「(観点が思いつかなければ何でも報告して)」のような責務放棄になっていない
- [ ] 5 観点 (計算量 / アロケーション / I/O / 並行 / DB) のうち少なくとも 4 種が prompt に登場している
- [ ] tech_stack に対応する言語固有要素 (`sync.Pool` / `Vec::with_capacity` / `numpy` / `Promise.all` 等) が観点に登場している
