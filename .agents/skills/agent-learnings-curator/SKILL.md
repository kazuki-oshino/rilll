---
name: agent-learnings-curator
description: Reflector and Curator prompts for agent-learnings workflow. Mode-driven (reflector | curator). Reads <runDir> artifacts and emits pure JSON conforming to the Reflector / Curator API Contracts.
disable-model-invocation: true
allowed-tools: Read
argument-hint: <mode> <runDir>
---

# agent-learnings-curator skill

本 skill は agent-learnings ワークフローの Reflector / Curator phase を担う 2 mode prompt SSoT である。
Go 側 (`internal/workflow/builtin/learnings/`) には prompt リテラルを一切持たず、本 skill が
prompt の唯一の所在となる (Req 4.2 / 6.1)。

## Inputs

引数: `<mode> <runDir>`

- `<mode>`: `reflector` または `curator` のいずれか。
- `<runDir>`: `.rilll/agent-learnings/<runID>/` の絶対パス。本 skill が Read tool で読む対象は
  すべて `<runDir>` 直下に配置される (`docs_dev/playbook/**/*.md` を直接 Read しない)。

mode ごとの prompt 本体は以下のファイルから採用する。

- `reflector` → `rules/reflector.md`
- `curator` → `rules/curator.md`

## Mode dispatch

- `<mode> == "reflector"`: `rules/reflector.md` を Read で読み、その指示に従って
  **session-resume 経由で渡された自身の会話履歴** から学びを抽出した **純 JSON** を出力する。
  Reflector mode では `<runDir>` 配下の入力ファイルは想定しない (session context が入力)。
  出力は Go 側 reflector phase が `<runDir>/reflector.json` に集約する。
- `<mode> == "curator"`: `rules/curator.md` を Read で読み、その指示に従って
  `<runDir>/{learnings.json, existing-rules.txt}` を入力に
  `operations[]` を出力する **純 JSON** を生成する。各 operation は
  `{op, category, topic, paths_glob, entry, summary, applies_to}` を required field として
  含む (各 field 詳細は `rules/curator.md` の出力 schema / 生成指示節を参照)。
  - `<runDir>/learnings.json` は Reflector phase が akashic に永続化した本 run の learning を
    provenance 付き (`id` = akashic learnings.id / `session_id` / `step_id` / `provider_id` /
    `recorded_at`) で集約した artifact (Go 側 SoT: `learnings.JSONSchemaForLearningsArtifact()`)。
  - `<runDir>/reflector.json` も同 dir に存在するが、こちらは per-session debug trace
    (counts / failures / dropped / skipped_provider) のみで learning 本文を含まないため、
    curator は読まない。
  - `<runDir>/existing-rules.txt` は `docs_dev/playbook/<category>.md` (本 spec の wiki layout
    4 file) を concat した plain text (Req 9.3)。
- それ以外の `<mode>`: 何もせず、エラーメッセージを stderr 等価出力に書き出して停止する
  (skill 実行は失敗扱いとする)。

## 出力契約 (両 mode 共通)

- 出力は **純粋な JSON のみ** (Markdown / 散文 / fenced code block 禁止 / 前後空白も禁止)。
- 1 オブジェクト 1 行 / 整形改行 (pretty print) は許容する。最終文字は `}` / `]` / 数値リテラル
  終端で締める。
- JSON Schema は本 skill の `rules/reflector.md` / `rules/curator.md` 内に SSoT として併記する。
  Go 側の `JSONSchemaForReflector` / `JSONSchemaForCurator` (`schema.go`) と完全一致させる
  (両端契約テストで pin される / Req 4.4, 6.2)。
- `additionalProperties: false` を尊重し、未定義キーを追加しない。

## 入力 read 範囲の制約

- skill 内では `docs_dev/playbook/**/*.md` を直接 Read **しない**。Curator が必要な既存
  playbook は Go 側が事前に concat した `<runDir>/existing-rules.txt` を介して受領する
  (Req 6.1 / 10.5 の novelty 入力契約 + 決定論性確保)。
- `<runDir>` 外のパスを Read してはならない (path traversal 防止)。

## Sensitive data の取扱い

- API key / 認証情報 / OAuth token / SSH 秘密鍵 / 環境変数の機密値 / 個人情報 / 商用秘密に
  該当する可能性のある文字列は、いかなる field にも **含めない**。`context` / `insight` /
  `entry` / `paths_glob` のすべてで同じ。
- 学びは「再現可能な作業手順 / 失敗パターン / 設計観察」に限定し、その時の secret リテラル
  そのものを記録しない (例: 「`API_KEY=...` を export」と書かず、「provider 用 credential
  を環境変数で渡す」と抽象化する)。
- 判断不能な場合は当該 learning / operation を出力配列に含めない。

## 失敗時の扱い

- 入力 JSON が破損 / 想定スキーマと不一致のとき、null や `{}` を返さず、可能な限り部分的でも
  schema に合致する JSON を返すこと (例: `{"learnings": []}` / `{"operations": []}`)。
- 候補が 1 件も見つからない場合も「報告文」を書かず、空配列の純 JSON
  (`{"learnings": []}` / `{"operations": []}`) を返す。
- 散文 / 報告文 / Markdown を絶対に出力しない。応答の **最初の文字は必ず `{`** で、
  最後は `}` で閉じる。これに違反すると Go 側の JSON 抽出 fallback (`extractAgentResponseJSON`)
  も救えない経路 (純散文 / 空応答) に落ちて apply phase ごと skip され、playbook が
  更新されない。
