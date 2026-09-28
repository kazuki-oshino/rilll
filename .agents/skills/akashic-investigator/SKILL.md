---
name: akashic-investigator
description: rilll (このリポジトリで開発中のワークフローオーケストレータ) の実行ログ「アカシックレコード」を SQLite から直接読み出して調査するスキル。「アカシックレコードを見て」「rilll の実行履歴を調べて」「ワークフローが失敗した理由を調査」「直近の失敗原因」「ステップが落ちた理由」「コスト/トークン使用量」「context window の使用率」「session を resume したい」「provider 別の比較」など、rilll のラン/エージェント呼び出し履歴を確認・診断したい場面では必ず最初にこのスキルを使うこと。スキルなしで取りかかるとアカシックレコードの場所やスキーマの再発見から始まり著しく非効率になる。
---

# Akashic Investigator

## アカシックレコードとは

rilll (Go 製のワークフローオーケストレータ。本リポジトリのプロダクト) が、ワークフロー実行のメトリクス・観測イベント・成果物索引を SQLite に永続化したもの。リポジトリ内では **アカシックレコード** と呼ぶ。仕様は `docs/akashic.md`、Go 実装は `internal/akashic/` を参照。

## DB の場所

優先順 (既存のものを採用):

1. `$RILLL_AKASHIC_DB`
2. `${XDG_DATA_HOME:-$HOME/.local/share}/rilll/akashic.sqlite`
3. `${XDG_DATA_HOME:-$HOME/.local/share}/rilll/observability.sqlite` (旧名 fallback)

シェルで毎回解決する one-liner:

```sh
DB="${RILLL_AKASHIC_DB:-${XDG_DATA_HOME:-$HOME/.local/share}/rilll/akashic.sqlite}"
[ -f "$DB" ] || DB="${XDG_DATA_HOME:-$HOME/.local/share}/rilll/observability.sqlite"
```

実行は基本的に `sqlite3 -readonly "$DB"` で開く (rilll 本体が同時に書き込み中でも安全)。**WAL (`*.sqlite-wal` / `*.sqlite-shm`) があるので DB ファイル単体のコピー・別ホストへの送付は禁止**。

## スキーマ早見表

| テーブル | 役割 | 重要列 |
|---|---|---|
| `workflow_runs` | ワークフロー単位の開始/終了/エラー | `id`, `workflow_id`, `workflow_name`, `cwd`, `started_at`, `finished_at`, `error`, `project_id` |
| `agent_calls` | step 単位のエージェント呼び出し (Claude / Codex / Cursor) | `id`, `workflow_run_id`, `step_id`, `step_name`, `provider_id`, `success`, `duration_ms`, `total_cost_usd`, `input_tokens`, `output_tokens`, `error`, `failure_category`, `result_summary`, `result_payload_json`, `session_id`, `resume_session_id`, `user_corrected` |
| `raw_events` | 観測イベントの生ログ | `id`, `agent_call_id`, `workflow_run_id`, `sequence`, `source`, `type`, `subtype`, `observed_at`, `payload_json`, `payload_truncated` |
| `agent_context_usage` | context window 使用率スナップショット | `agent_call_id`, `total_tokens`, `max_tokens`, `percentage`, `categories_json`, `mcp_tools_json`, `message_breakdown_json` |
| `run_artifacts` | 成果物の索引 (中身ではなく path/size/mime) | `workflow_run_id`, `path`, `kind` (`proposal` / `review` / `result` / `context`), `size_bytes`, `mtime`, `mime` |

`raw_events.payload_json` は数 KB〜数百 KB に達するため、**素朴な `SELECT *` や生 `payload_json` 取得はしないこと**。先に `substr(payload_json, 1, N)` で頭出しし、対象を絞ってから全文を取る。

`raw_events.type` の代表値 (本リポジトリの実 DB 分布):

- `hook` — `subtype` が `PreToolUse` / `PostToolUse` / `PostToolUseFailure`
- `system` — `subtype = init` (モデル / cwd / 利用可能ツール)
- `result` — `subtype` が `success` / `error` (エージェントの最終結果)
- `context_usage` / `context_usage_error` — context 使用率の観測

記録レベル (`akashicLevel`) は `summary` / `full` / `debug`。`summary` だとストリームイベントが落ちるため、`payload_truncated = 1` の行が増える。

## 失敗原因の標準調査フロー

「直近のワークフロー失敗の原因を探して」と頼まれたらこの順で掘る。途中で結論が出ても下層は確認用に控えておく。

### 1. 失敗 run の一覧を取る

```sql
SELECT id, workflow_name, started_at, finished_at,
       substr(error, 1, 200) AS error_head
FROM workflow_runs
WHERE error IS NOT NULL AND error != ''
ORDER BY started_at DESC
LIMIT 5;
```

`workflow_runs.error` は workflow レイヤの中断理由 (例: `中断: 05-validate-design で prompt-failure（[stream_idle_timeout] ...`)。この時点で「どの step」「どの failure category」かが多くの場合読める。

### 2. その run の agent_calls を時系列に並べる

```sql
SELECT id, step_name, provider_id, success, duration_ms,
       failure_category, substr(error, 1, 200) AS error_head, started_at
FROM agent_calls
WHERE workflow_run_id = '<RUN_ID>'
ORDER BY started_at ASC;
```

`success` は **三値**:

- `1` = 成功
- `0` = 失敗
- `NULL` = 未完了 / 中断 / in-progress

`success != 1` で書くと NULL を取りこぼすので、目的に合わせて `success = 0` か `success IS NOT 1` を選ぶ。

### 3. 失敗 agent_call の終端イベントを覗く

```sql
SELECT sequence, type, subtype, observed_at,
       substr(payload_json, 1, 400) AS payload_head, payload_truncated
FROM raw_events
WHERE agent_call_id = '<AGENT_CALL_ID>'
ORDER BY sequence DESC
LIMIT 20;
```

注目すべき subtype:

- `result` (`success` / `error`) — エージェント最終結果。`payload_json` 内の `result` / `is_error` / `api_error_status` / `duration_api_ms` / `num_turns` を読む。
- `PostToolUseFailure` — ツール実行失敗。`payload_json.input.tool_name` と `tool_response` を読む。
- `system init` — 起動時のモデル / cwd / 有効ツール一覧。再現条件特定に使える。
- `context_usage` / `context_usage_error` — コンテキスト枯渇の有無。

### 4. 必要なら type / subtype で絞って全件読む

```sql
SELECT sequence, observed_at, payload_json
FROM raw_events
WHERE workflow_run_id = '<RUN_ID>'
  AND type = 'result'
ORDER BY sequence ASC;
```

`payload_truncated = 1` の行は記録レベルが `summary` で詳細が落ちている。`debug` で再実行しないと中身は復元できない。

### 5. 成果物 (artifact) を確認する

```sql
SELECT path, kind, size_bytes, mtime, mime
FROM run_artifacts
WHERE workflow_run_id = '<RUN_ID>'
ORDER BY mtime ASC;
```

成果物の **本体はリポジトリ内** (例: `.rilll/<workflow>/<RUN_ID>/...`) に置かれているので、`path` を `Read` ツールで直接開く。`kind` は `proposal` / `review` / `result` / `context`。memory-distiller の中間ファイルは `.rilll/memory-distiller/<RUN_ID>/` 配下。

## よく使う集計・横断クエリ

### failure_category の分布

```sql
SELECT failure_category, COUNT(*) AS n
FROM agent_calls
WHERE success = 0
GROUP BY failure_category
ORDER BY n DESC;
```

### workflow ごとの成功率

```sql
SELECT workflow_id,
       COUNT(*) AS runs,
       SUM(CASE WHEN error IS NULL AND finished_at IS NOT NULL THEN 1 ELSE 0 END) AS ok,
       printf('%.1f%%',
              100.0 * SUM(CASE WHEN error IS NULL AND finished_at IS NOT NULL THEN 1 ELSE 0 END) / COUNT(*)) AS rate
FROM workflow_runs
GROUP BY workflow_id
ORDER BY runs DESC;
```

### context window 使用率が高い run

```sql
SELECT agent_call_id, model, total_tokens, max_tokens, percentage, observed_at
FROM agent_context_usage
WHERE percentage >= 80
ORDER BY observed_at DESC
LIMIT 20;
```

### provider 別のコスト / レイテンシ

```sql
SELECT provider_id,
       COUNT(*)              AS calls,
       AVG(total_cost_usd)   AS avg_cost_usd,
       AVG(duration_ms)      AS avg_duration_ms,
       SUM(input_tokens)     AS in_tok,
       SUM(output_tokens)    AS out_tok
FROM agent_calls
WHERE provider_id IS NOT NULL
GROUP BY provider_id
ORDER BY calls DESC;
```

### session resume の系譜

`resume_session_id` が紐付いた agent_call は、別の agent_call の `session_id` を継いだもの。コンテキスト保持の流れを辿りたいときに使う。

```sql
SELECT id, step_name, provider_id, session_id, resume_session_id, started_at
FROM agent_calls
WHERE workflow_run_id = '<RUN_ID>'
ORDER BY started_at ASC;
```

### 期間で絞る

`started_at` / `finished_at` / `observed_at` は **RFC3339Nano (UTC) の TEXT** なので文字列比較で範囲指定できる:

```sql
WHERE started_at >= '2026-05-01T00:00:00Z'
  AND started_at <  '2026-05-08T00:00:00Z'
```

## 失敗カテゴリの読み方

`agent_calls.failure_category` で観測される代表値と典型的な意味:

- `stream_idle_timeout` — provider 側ストリームのアイドルタイムアウト。長尺生成や ToolUse 待ち中に出やすい。一過性のことが多く再実行可能。
- `schema-violation` — bridge (TS) が NDJSON を zod 検証で弾いた。bridge 側ログ (`.rilll/<workflow>/<RUN_ID>/...`) と併読。
- `loop-limit` — inquiry loop が指定回数で収束しなかった。プロンプトやループ条件の見直しが必要。

`workflow_runs.error` は `中断: <step> で <category>(<message>)` 形式で書かれることが多く、先頭 200 文字で大半が読める。長文のときは `SELECT length(error)` で確認してから全文取得する。

## アンチパターン

- **`SELECT *` で `raw_events` を全列取得しない**。`payload_json` が巨大。`substr(payload_json, 1, N)` か、対象 sequence を絞ってから全文を取る。
- **DB ファイルをコピーしない**。WAL と整合が取れていないと壊れる。読むだけなら `sqlite3 -readonly "$DB"`。
- **id を推測で打たない**。`workflow_runs.id` は `2026-05-06T13-41-27-1778074887195936000` のように **時刻 + nanos でハイフン区切り** (`:` ではない)。先に `LIMIT` で一覧してから貼り付ける。
- **`success = 1` の否定で NULL を巻き込む**。`success != 1` は NULL を弾くので意図に応じて `success = 0` と書き分ける。
- **`observability.sqlite` を旧名だからと無視しない**。マシン状態によっては fallback でこちらが現行の場合がある。
- **記録レベルを確認せず詳細追跡しない**。`summary` だとストリームイベントが落ちるので、欲しい情報がそもそも書かれていない可能性がある (`payload_truncated = 1` の頻度で判別)。

## 結論を書くときに必ず添える項目

調査の出力には少なくとも次を揃える:

- run id / workflow_name / started_at
- 失敗 step (`agent_calls.step_name`) と provider
- failure_category
- 終端 raw_event の subtype と要点 (`substr(payload_json, ...)`)
- 関連する run_artifacts の path

run id は省略・ハッシュ化せずそのまま提示する。後で再検索するときに必要。
