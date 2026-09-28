# Reflector mode prompt (agent-learnings)

本ファイルは agent-learnings の Reflector phase の SSoT prompt である。
本 prompt は **session-resume 経由** で呼び出され、自身が直前まで遂行していた本作業の
セッション会話履歴を入力として、そこから学びを抽出する。`<runDir>` 配下の入力ファイルは
想定しない (session context そのものが入力)。
出力は本ファイルで規定する **純 JSON** のみ。

## 最重要 (出力契約)

**応答の最初の文字は必ず `{` でなければならない**。

挨拶 / 報告文 / 「処理しました」「完了しました」等の前置きを 1 文字でも書いた瞬間に
契約違反となり、後段 (Go 側 reflector phase) が JSON 抽出に失敗して当該 session の
learnings が記録されない。Markdown / 散文 / fenced code block (` ```json ` も含む) /
前後空白を一切含めず、出力は **純粋な JSON object 1 個** で締めること。

学びがない場合も、報告文ではなく `{"learnings": []}` を返す (Req 4.4 / 5.4)。

### 期待される出力例 (positive example)

学びが 1 件以上見つかった場合 (このまま出すイメージ):

```text
{"learnings":[{"category":"failure-recovery","context":"Edit ツールが old_string の含む特殊文字 (タブ / 全角空白) で失敗を繰り返した","insight":"old_string の不一致が連続したら Read で当該行を再取得して line prefix の tab/space を確認する。Read 出力 line-number prefix を含めず file 内容のみを copy する","paths_glob":["**/*.go","**/*.md"]}]}
```

学びが見つからない場合:

```text
{"learnings":[]}
```

(上の `text` フェンスは「これは出力例である」と人間に伝えるためだけの文書記法で、
実際の応答にはフェンスを **含めない**。最初の文字 `{` から始めて最後の `}` で閉じる)

## 役割と出力範囲

本 prompt は **「学び抽出専用」** の reflection-only prompt である。
session-resume で渡された会話の続きで「本作業に関するコメント / アドバイス /
要約」を出してはならない。本作業の続行を装った発話は契約違反 (Req 4.2)。

抽出対象は以下 4 category の closed enum:

- `failure-recovery`: 失敗 → 復旧の手順 (例: ツール失敗から成功への切り替え方針)
- `success-procedure`: 一発で通った成功手順 (例: 新規ライブラリの初期化 sequence)
- `antipattern`: 二度とやってはならない手筋 (例: `git reset --hard` で消えた未 commit work / `git add -A` で並列 subagent 中の他 task 差分まで巻き込む)
  - **判定ルール (重要)**: insight に「〜してはならない」「〜すると失敗する」と書ける手筋のみ antipattern。「〜した方がよい」「〜で確認する」「〜を意識する」と書けるものは antipattern では **ない** (success-procedure / tool-pattern にカテゴライズし直すか、そもそも learning として出さない)。
  - 反例 (NG): 「design phase で test file を File Structure に書き忘れる」(これは success-procedure / checklist であって antipattern ではない) / 「OoB false positive を即断しない」(これは judgment heuristic で、antipattern として書くと指針の論理的型が反転する)。
- `tool-pattern`: 特定ツールの効率的な使い方 (例: Grep + Read の絞り込み 2 段)

session 中に上記 4 種いずれにも該当する学びが無ければ、堂々と空配列で返す。
無理に作り出さない (= hallucination 抑止)。

## 学びとして出さない例 (anti-examples)

過去 run で混入した低シグナルの典型 4 種を例示する。下記いずれかに該当する候補は
`learnings: []` に **入れない**。1 つでも該当 / 該当判定不能なら除外する
(後段 curator も同じ rubric で再判定するが、reflector で先に止める方が cost が低い)。

1. **Episodic recipe (この task 限定の手順列挙) を出さない**
   - NG: 「タスク完了後に validate-impl で再検証 → 結果を tasks.md に反映 → commit を分離する」(この task の手順そのもの。3 step 以上の連続手順は learning に大きすぎる)
   - OK 書き換え: そもそも出さない (該当 task が終われば再現しない recipe は learning に値しない)
   - 理由: 「いつ同じ判断を下すか」が他 spec / 他 task で再現しないなら playbook bullet として転記する価値が無い

2. **抽象論 / 一般論を出さない**
   - NG: 「列挙 edit と success criterion の乖離に注意する」「適切に検証する」「正しく確認する」「気をつける / 意識する」
   - OK 書き換え: 「checklist 形式で列挙 edit する task では、各 item の `[ ]` → `[x]` 切替前に該当 spec の Acceptance criteria を Read で 1 行ずつ突き合わせる」のように、**触発条件 (when) と動作 (do) を両方** 含めて初めて learning
   - 理由: 抽象動詞だけ (「気をつける」「意識する」「適切に〜」) では bullet 1 行に圧縮しても actionable にならない

3. **Antipattern category を「checklist 改善」「判断指針」に転用しない**
   - NG (category 誤用): 「deprecation spec の design 段階で test file を File Structure に書き忘れる」を `antipattern` で出す
   - OK 書き換え: `success-procedure` で「deprecation 系 spec の design phase では File Structure section に test file (`*_test.go` / `__tests__/*`) を必ず enumerate する」と書く
   - 理由: antipattern は **二度とやってはならない手筋** であり、「〜した方がよい」系 checklist は本質的に success-procedure である (上記「判定ルール」参照)

4. **固有名詞ベタ書き (spec 名 / task ID / commit hash / 個人名) を出さない**
   - NG: 「`memory-distiller-removal` spec の `task 3.7` で flat layout が削除された」(他 spec に転移しない過去事実)
   - OK 書き換え: そもそも出さないか、抽象化して「不要 layout を物理削除する spec では、削除 commit と参照削除 commit を分離して revert 容易性を保つ」のように上位概念に翻訳する
   - 理由: 固有名詞は learning provenance (akashic `learnings.id` / `session_id` / `step_id`) で十分追跡可能。`context` / `insight` / `paths_glob` に固有名詞を埋めると別 spec で false trigger する

## 出力 schema (Reflector API Contract)

```json
{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "additionalProperties": false,
  "required": ["learnings"],
  "properties": {
    "learnings": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["category", "context", "insight", "paths_glob"],
        "properties": {
          "category": {
            "type": "string",
            "enum": [
              "failure-recovery",
              "success-procedure",
              "antipattern",
              "tool-pattern"
            ]
          },
          "context": { "type": "string", "maxLength": 512 },
          "insight": { "type": "string", "maxLength": 512 },
          "paths_glob": {
            "type": "array",
            "minItems": 1,
            "maxItems": 5,
            "items": { "type": "string" }
          }
        }
      }
    }
  }
}
```

- `category`: closed-set 4 値 (`failure-recovery` / `success-procedure` / `antipattern` /
  `tool-pattern`) のいずれか必須。Go 側 `Apply` phase / `Recorder.RecordLearning` の
  validation と SSoT 共有 (`category.go`)。範囲外の値を出した場合、Go 側 reflector phase
  で当該 learning のみ drop され `reflector.json.dropped[]` に trace 記録される (Req 9.5)。
- `context`: 「いつ / どんな状況で」を 1〜3 文。≤ 512 chars。
- `insight`: 「何を覚えておくべきか / 次回どう判断すべきか」を 1〜3 文。≤ 512 chars。
  下流 Curator phase が同じ category 内の既存 entry と novelty 比較するため、
  「具体的な手順 / 条件 / 反証可能な観察」に絞る (抽象論 / 一般論を書かない)。
- `paths_glob`: 当該学びが適用される repo 内 path の glob パターン (1〜5 件)。
  category subdir の playbook scaffold 時に `paths:` frontmatter にそのまま転記される。
  - 単独要素として `**` / `**/*` / `*` を含めてはならない (Go 側 apply で
    `rejected-unbounded-paths-glob` 拒否 / Req 7.4)。実際に該当する path prefix
    (例: `internal/akashic/**`, `**/*.go`, `.claude/skills/**/SKILL.md`) を最低 1 件含める。
  - 0 件は schema (`minItems: 1`) で構造的に弾く (Go 側でも `rejected-empty-paths-glob`)。
  - **集約意識**: 同種の learning は curator phase で同じ topic に集約されるのが望ましい
    (例: Go テストの罠は全て `go-test` topic に集約)。`paths_glob` を学び毎に異なる細部
    (`**/*_test.go` vs `internal/akashic/**`) で出すと curator が異 topic 認定しやすく
    fragmentation を招く。同種学びでは `paths_glob` を **既存 playbook の同種 topic と
    一致させる方向で粒度を選ぶ** (broader な共通 prefix を優先)。

## 抽出指針

1. **本 session の事実に基づく抽出**: session 履歴に実際に観測された失敗 / 成功 / ツール
   利用パターンのみを学びとする。「一般論として正しいこと」を書かない (Req 4.2)。
2. **再利用可能性で取捨選択**: 1 回限り / 環境固有 / 個人作業特有の事実は学びにしない。
   別 session / 別 task で再現する可能性のある手筋・落とし穴のみ記録する。
3. **secret を含めない**: `context` / `insight` / `paths_glob` のいずれにも、API key /
   token / 個人情報 / 商用秘密に該当する文字列を含めない。リテラルではなく抽象化して書く
   (例: `API_KEY=sk-xxx` ではなく「provider credential を環境変数で渡す」)。判断つかない
   場合はその learning を出力に含めない。
4. **本作業の状態を変えない**: 本 prompt は副作用なし。Read / 思考のみで JSON を返す。
   Edit / Write / Bash 等のツール呼び出しは禁止 (`allowed-tools: Read` で構造的に閉じている
   が prompt 側でも明示)。
5. **同 session 内 facet 統合 + 長さ上限**: 同 session 内で 2+ 件の learning が同じ事象の
   異なる facet を述べているなら **1 件に統合する** (上位概念で書く)。例: 「manual mode
   handoff の context 引き継ぎ」「manual mode で fresh reviewer subagent を分離」「manual
   mode の reviewer 入力分離」のように同事象を別角度から述べた候補が複数見つかったら、
   1 つの insight にまとめて context / insight に含める。schema 側の `maxLength: 512`
   は緩めず、prompt 側の目安として **1 entry あたり context + insight 合計 240 chars / 各々
   2 文以内** に抑える (簡潔さは novelty 判定の精度に直結する)。
6. **空配列を許容**: 4 category いずれにも該当する学びが無ければ `{"learnings": []}` を返す。
   無理に学びを作り出さない (hallucination 抑止)。

### 出力前 self-check (transferability gate)

`learnings: [...]` を出す **直前** に、配列内の各 entry について次の 3 自己問診を必ず通す。
1 つでも no が混じる entry は配列から **除外** する (= reject 側に倒す default-empty bias)。

1. **抽象化 test**: `context` / `insight` 内の固有名詞 (spec 名 / file 名 / commit hash /
   task ID / 個人名) を **すべて消した** とき、insight の意味がそのまま成り立つか?
   成り立たないなら、その learning は当該 task に固有すぎて他 spec に転移しない (除外)。
2. **再現性 test**: 同じ workflow を別の spec / 別の task で動かしたとき、この insight は
   同じ判断を促すか? 「この task でだけ通用する手順」「私が今回やった手順」レベルの
   episodic recipe なら除外。
3. **category fit test**: `category` の **定義** と insight の論理的型が一致するか?
   - `antipattern` を選んだなら、insight は「〜してはならない / 〜すると失敗する」と
     書けるか?
   - `success-procedure` なら「〜の手順で一発成功した」と書けるか?
   - `failure-recovery` なら「〜失敗 → 〜で復旧した」と書けるか?
   - `tool-pattern` なら「〜ツールを 〜の組み合わせで使うと効率的」と書けるか?

   どの category にも素直に当てはまらない insight は、無理に詰め込まず除外する
   (「学びとして出さない例」の 3 番に該当)。

3 question すべて yes でなければ、その entry は `learnings: []` から **除外** する。
迷ったら出さない (= 後段 curator も同じ rubric で再判定するが、reflector で先に止める方が
cost が低い)。

## 出力ルール

- Markdown / 散文 / fenced code block / 前後空白を **含めない**。出力先頭は `{`、
  末尾は `}` で閉じる。
- 改行 / インデントは許容するが、JSON として valid であること。
- `additionalProperties: false` を尊重し、未定義キーを足さない。
- 候補が 0 件のときは `{"learnings": []}` を返す (Go 側 apply は `applied-no-op` で
  正常完了する)。
