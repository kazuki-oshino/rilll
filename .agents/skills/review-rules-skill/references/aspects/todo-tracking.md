# todo-tracking-review 観点合成ガイド

> 本ガイドは review-rules-skill Phase 4 が `applies_when` / `prompt` を合成するための観点別手順書である。テンプレ (種) `templates/todo-tracking-review.yaml` と本ガイドを Read し、Phase 1 のリポ調査結果を織り込んで合成する。

## 1. 観点の目的

TODO トラッキング観点では以下クラスの問題を検出する:

- 新規追加された `TODO` / `FIXME` / `XXX` / `HACK` / `NOTE` マーカーの管理状況不足 (担当 / 期限 / issue 番号 / 重大度の不在)
- 既存マーカーの取り扱い変化 (放置されたまま当該コードを書き換えた、解消されていないのに削除した、内容を変更したのに ID 維持)
- 重大度の使い分け不足 (critical な抜けが `NOTE` でマスクされている、軽微なメモが `FIXME` で煽っている)
- 関連 issue / ticket / 仕様への trace 不足 (`TODO(#123)` / `// TODO: kazuki - 2026-01-31` の体裁が repo 規約と合っていない)
- 期限超過 / 長期放置 (`TODO(due: 2025-Q1)` のような期限が過ぎた、git blame で 1 年超のマーカーが温存されている)

これらを言語非依存に検出し、技術的負債が暗黙化する経路を merge 前に塞ぐことが目的。

## 2. Phase 1 で読むべき入力

参照する Phase 1 出力 key (本観点は **言語非依存** で全般を弱く参照):
- `tech_stack.languages`: 言語別コメント記法 (`//` / `#` / `--` / `<!-- -->` / `;`) を観点に反映
- `convention_docs`: TODO 運用規約 (`CONTRIBUTING.md` の TODO 節 / `docs/contributing.md` / steering の `code-style.md`) があれば引用
- `sensitive_areas`: 既存マーカーの放置が許されない領域 (security / billing / 公開 API) を優先対象に挿入

追加で行う Read / Glob / Grep:
- Glob: 全コードファイル (`**/*.{go,ts,tsx,js,jsx,py,rs,rb,java,kt,swift,sh,md}`) — マーカー検出対象
- Grep: `(?i)\b(TODO|FIXME|XXX|HACK|NOTE)\b[(:\s]` — マーカー出現箇所
- Grep: `TODO\([^)]+\)` — 担当者 / issue 番号付き TODO 体裁の検出 (規約把握)
- Grep: `TODO.*\d{4}-\d{2}-\d{2}` / `TODO.*Q[1-4]` — 期限付き TODO の検出
- Read: `CONTRIBUTING.md` / `docs/contributing.md` / `docs/code-style.md` の TODO 節 — 既存規約抽出

## 3. `applies_when` の作り方

判定軸:
- diff 内のマーカー (`TODO` / `FIXME` / `XXX` / `HACK` / `NOTE`) の追加・変更・削除
- 新規追加コード全般 (新規ファイルにマーカーが紛れていないか cross check)
- マーカー周辺コードの変更 (既存 TODO のあるブロックを書き換えた場合の整合チェック)
- TODO 運用規約ファイルの変更 (規約自体が変わった場合の波及)

作例 (2 件以上):

**作例 A (最小)**: Phase 1 が `tech_stack.languages=[Go]` / `convention_docs=[CONTRIBUTING.md]` を返した場合
```
この rule は Go repo の全変更ファイル (`.go` / `.md` / `.sh` / その他コメント記法を持つ言語) で TODO / FIXME / XXX / HACK / NOTE マーカーの追加・変更・削除を含む変更、または既存マーカーがあるブロックの書き換えを含む変更に対して適用する。CONTRIBUTING.md の TODO 節の変更も対象とする。
```

**作例 B (充実)**: Phase 1 が `tech_stack.languages=[TypeScript, Python, Shell, Markdown]` / `convention_docs=[CONTRIBUTING.md, docs/code-style.md]` / `sensitive_areas=[apps/api/auth, services/billing]` を返した場合
```
この rule は TypeScript / Python / Shell / Markdown を含む全コードファイルで TODO / FIXME / XXX / HACK / NOTE マーカーの追加・変更・削除を含む変更、apps/api/auth および services/billing 配下の変更 (機微領域では既存マーカーの放置を厳しめに見る)、新規ファイル追加、CONTRIBUTING.md / docs/code-style.md の TODO 節の変更に対して適用する。
```

## 4. `prompt` の作り方

骨格 (重要観点 5 項):
1. 新規追加マーカーが repo 規約の体裁 (担当 / issue 番号 / 期限 / 重大度) を満たしているか
2. 既存マーカー周辺の変更で、解消済みなのに残存・未解消なのに削除・内容ズレ等の整合崩れがないか
3. マーカー種別の使い分け (TODO / FIXME / XXX / HACK / NOTE) が repo 規約と一致しているか (critical な内容が NOTE で隠れていない、軽微メモが FIXME で煽っていない)
4. 関連 issue / ticket / 仕様 / RFC への trace (`TODO(#123)` / `TODO(@user)` / `TODO(spec: ...)` 等) が確保されているか
5. 期限超過 / 長期放置 (期限付き TODO の超過、git blame で 1 年超のマーカー、機微領域での未解消) の確認

Phase 1 所見をどう織り込むか:
- 具体パス: `sensitive_areas` の値を観点 5 で「機微領域での未解消は critical」のように引用
- 言語別コメント記法: `tech_stack.languages` から `//` (Go/TS/Rust) / `#` (Python/Shell/Ruby) / `--` (SQL/Lua) / `<!-- -->` (Markdown/HTML) を観点 1, 2 に列挙
- 既存規約: `convention_docs` で発見した体裁 (`TODO(#issue)` / `TODO(@user, YYYY-MM-DD)` 等) を観点 1, 4 に引用
- 既存マーカー数: Phase 1 で grep して既存マーカー総数が多い repo なら「全体での増減」を観点 5 にも追加

作例 (最小例): 作例 A に対応
```
以下の観点で TODO / FIXME / XXX / HACK / NOTE マーカーの追加・変更・削除をレビューしてください。対象は `//` コメント (Go / その他) と `#` コメント (Shell / Markdown 内コードブロック) の両方です。
1. 新規追加マーカーが CONTRIBUTING.md の体裁 (担当者 / issue 番号 / 重大度) を満たしているかを確認する。
2. 既存マーカーがあるブロックを書き換えた変更で、内容が解消済みなのに残存している、または未解消なのに削除されている、内容ズレがある等の整合崩れがないかを確認する。
3. マーカー種別の使い分け (TODO=未着手 / FIXME=既存バグ / XXX=要注意 / HACK=暫定回避 / NOTE=参考情報) が repo 規約と一致しているかを確認する。
4. 新規 TODO に関連 issue / ticket / 仕様への trace (`TODO(#123)` / `TODO(@user)`) が付与されているかを確認する。
5. 期限付き TODO の超過、または git blame で 1 年超のマーカーが温存されている箇所を指摘する。
指摘は重大度 (high / medium / low) を付け、修正方針 (issue 化 / 削除 / 担当付与等) を 1 行で添える。
```

作例 (充実例): 作例 B に対応
```
以下の観点で TODO / FIXME / XXX / HACK / NOTE マーカーの追加・変更・削除をレビューしてください。対象は `//` (TypeScript) / `#` (Python / Shell) / `<!-- -->` (Markdown) の各コメント記法を含みます。
1. 新規追加マーカーが CONTRIBUTING.md / docs/code-style.md の体裁 (担当 / issue 番号 / 期限 / 重大度) を満たしているかを確認する。
2. 既存マーカーがあるブロックを書き換えた変更で、解消済みなのに残存・未解消なのに削除・内容ズレ等の整合崩れがないかを確認する。
3. マーカー種別の使い分け (TODO=未着手 / FIXME=既存バグ / XXX=要注意 / HACK=暫定回避 / NOTE=参考情報) が docs/code-style.md と一致しているかを確認する。critical な事項が NOTE で隠れている、軽微なメモが FIXME で煽っているケースを指摘する。
4. 新規 TODO に関連 issue / ticket / 仕様 / RFC への trace (`TODO(#123)` / `TODO(@user, 2026-Q2)` 等) が付与されているかを確認する。
5. apps/api/auth および services/billing 配下では既存 / 新規マーカーが機微領域に放置されていないか厳しめに点検する。期限付き TODO の超過、git blame で 1 年超のマーカーは critical / high として扱う。
指摘は重大度 (critical / high / medium / low) を付け、修正方針 (issue 化 / 削除 / 担当付与 / 期限明示) を 1 行で添える。
```

## 5. 合成後の自己点検項目

合成した `applies_when` / `prompt` が以下を満たすか機械的にチェックする:

- [ ] 観点リスト (prompt) に Phase 1 から得た具体パス / 規約ファイル名 / 言語別コメント記法のいずれかが **少なくとも 1 つ** 含まれている (汎用文章で済んでいない)
- [ ] 重大度の付け方 (critical / high / medium / low 等) が prompt 内に明示されている
- [ ] applies_when に「いつ発火するか」の判定軸が **2 つ以上** 含まれている (例: マーカー追加・変更条件 + 機微領域条件 + 規約変更条件)
- [ ] applies_when / prompt に未置換の `<UPPER_SNAKE>` token が残存していない
- [ ] prompt が「(観点が思いつかなければ何でも報告して)」のような責務放棄になっていない
- [ ] 5 種マーカー (TODO / FIXME / XXX / HACK / NOTE) のうち少なくとも 4 種が prompt に登場している
- [ ] 種別の使い分け基準 (TODO=未着手 / FIXME=既存バグ 等) が prompt 内に明示されている
