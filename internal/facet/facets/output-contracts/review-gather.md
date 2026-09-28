# Review Gather

このセクションは、エージェントが直前の review 作業 (rule / spec / 設計レビュー等) で収集した指摘を、後段の判定 / 集約 / 通知ステップが機械的に処理しやすい形に整形して書き出すための構造化テンプレートである。

以下のルールを守ってこのテンプレートを埋めること:

- 見出し (`## Summary` / `## Findings` / `## Citations` / `## Open Questions`) と順序は改変しない。
- 各見出し直下では、本テンプレートの説明文 (この `<...>` を含む行) を削除し、実際の内容で置き換える。
- `## Findings` 表の列 (`#` / `Severity` / `Location` / `Finding` / `Citation`) と列順は改変しない。1 行 1 件で書き、`Finding` 列は 1-2 文の事実記述に留める。
- `Severity` は次の closed enum から選ぶ: `critical` / `major` / `minor` / `info`。該当しない値は使わない。
- `Citation` 列には `## Citations` セクションの `[C1]` / `[C2]` 形式 ID を 1 個以上書き、根拠を必ず外部参照に紐付ける。複数あれば `[C1], [C3]` のようにカンマ区切り。
- `## Citations` には Finding 表で参照した ID をすべて記載する。`[Cn]: <URL またはローカルパス + 行番号 等>` 形式で、`Finding` 表で言及していない citation は載せない。
- 推測ではなく Phase 1 で観測した事実のみを書く。確認できていない事項は `## Open Questions` に逃がし、`## Findings` には含めない。
- Finding が 1 件も無いときは、表ヘッダだけを残し、表の下に `_該当なし_` の 1 行を書く (空表は許容しない)。

## Summary

レビュー全体の結論を 1-3 文で書く。最重要の Finding (Severity = `critical` / `major` のもの) があるならその件数と影響範囲を含める。

- <レビュー対象とスコープ: 何を / どの範囲で見たか>
- <総合判定: pass / needs-fix / blocker の別と理由>

## Findings

レビューで観測した指摘を、Severity の高い順 (`critical` → `major` → `minor` → `info`) に並べる。同 Severity 内では発生箇所の昇順で並べる。

| # | Severity | Location | Finding | Citation |
|---|----------|----------|---------|----------|
| 1 | <critical \| major \| minor \| info> | <file:line または module / 章番号> | <1-2 文で事実を記述> | <[C1] など> |
| 2 | <...> | <...> | <...> | <...> |

## Citations

Finding 表で参照した根拠の出典を一覧化する。

- [C1]: <URL またはローカルパス + 行番号 / 行範囲。例: `internal/agent/agent.go:129-227` / `https://example.com/spec#section-3`>
- [C2]: <...>

## Open Questions

レビュー中に判断できず、Finding 表に載せられなかった事項を列挙する。無ければ `- なし` の 1 行だけを書く。

- <疑問 1: 確認したい点 / 想定される選択肢 / 影響範囲>
