# アカシックレコード

rillはワークフロー実行のメトリクスをSQLiteデータベース（アカシックレコード）に記録します。

## 保存先

`$XDG_DATA_HOME/rilll/akashic.sqlite`に保存されます。`XDG_DATA_HOME`が未設定の場合は`~/.local/share/rilll/akashic.sqlite`がデフォルトです。環境変数`RILLL_AKASHIC_DB`で任意のパスに上書きすることもできます。

## テーブル構造

| テーブル | 概要 |
|----------|------|
| `workflow_runs` | ワークフロー実行の開始・終了・エラー |
| `agent_calls` | エージェント呼び出しごとのメトリクス（コスト・トークン数・所要時間） |
| `raw_events` | 生のobservationイベント（ツール使用、フック実行等） |
| `agent_context_usage` | コンテキストウィンドウ使用状況のスナップショット |

## アカシックレコード記録レベル

レベルによって記録される`raw_events`の情報量が変わります。

| レベル | 記録内容 |
|--------|----------|
| `summary` | 結果・システム初期化・主要フック（PreToolUse, PostToolUse等）・コンテキスト使用状況のみ |
| `full` | 上記に加え、ストリームイベントを除くすべてのobservation |
| `debug` | すべてのobservation（ストリームイベントを含む） |

レベルはUI設定画面または設定ファイルの`akashicLevel`から変更できます。

## キー操作リファレンス

メインメニューから `a` / `A` でアカシックモードに入ります。サブビューは List → Summary / Detail の 3 種類で、それぞれの末尾にも同じヒントが表示されます。

### List サブビュー（Run 一覧）

| キー | 動作 |
|------|------|
| `↑` / `↓` | カーソル移動 |
| `Enter` | カーソル位置の Run の詳細サブビューへ遷移 |
| `Tab` | 集計サマリサブビューへ切替 |
| `esc` / `q` | アカシックモードを抜けてメニューへ戻る |

### Summary サブビュー（集計サマリ）

| キー | 動作 |
|------|------|
| `Tab` | Run 一覧サブビューへ戻る |
| `esc` / `q` | アカシックモードを抜けてメニューへ戻る |

### Detail サブビュー（Run 詳細）

| キー | 動作 |
|------|------|
| `Tab` / `Shift+Tab` | フォーカスを Header / agent_calls / raw_events / 成果物 の順で循環 |
| `↑` / `↓` (`j` / `k`) | 現在フォーカス中の一覧でカーソル移動 |
| `n` / `p` | raw_events の次 / 前ページ |
| `Enter` | フォーカス=成果物 のとき、選択中成果物の本体プレビューを読み込む |
| `i` | この Run を入力に `improve-workflow` を hand-off で起動（トップメニューには表示されない hand-off 専用ワークフロー） |
| `Backspace` | Run 一覧サブビューへ戻る |
| `esc` / `q` | アカシックモードを抜けてメニューへ戻る |

## 旧名からの移行ガイド

過去バージョンで `observability` 名義だった項目は次のとおり `akashic` に統一されました。既存ユーザのデータ・設定・環境変数は破壊しません。

| 区分 | 旧名 | 新名 |
|------|------|------|
| Go パッケージ | `internal/observability/` | `internal/akashic/` |
| SQLite ファイル | `observability.sqlite` (および WAL 副ファイル `observability.sqlite-wal` / `observability.sqlite-shm`) | `akashic.sqlite` |
| 環境変数 | `RILLL_OBSERVABILITY_DB` | `RILLL_AKASHIC_DB` |
| 設定ファイルのフィールド | `observabilityLevel` | `akashicLevel` |
| TS bridge ファイル | `bridge/<provider>-sdk/src/observability.ts` | `bridge/<provider>-sdk/src/akashic.ts` |
| NDJSON フィールド | `observabilityLevel` | `akashicLevel` |

### 起動時の自動移行

- rilll は起動時に旧 `observability.sqlite` (および WAL 副ファイル) を新 `akashic.sqlite` へ自動的に rename します。
- 新パスに既にファイルが存在する場合は no-op（冪等）で、旧ファイルがあっても破壊しません。
- rename に失敗した場合は警告を 1 行表示したうえで旧ファイルを使い続けるため、起動は中断されません。
- DB ファイルのアクセス権（ディレクトリ 0700 / ファイル 0600）と SQLite スキーマは移行後も無変更です。

### 環境変数 / 設定ファイルの後方互換 fallback

- 環境変数: `RILLL_AKASHIC_DB` を主、`RILLL_OBSERVABILITY_DB` を fallback として読み込みます。両方設定されている場合は新を採用し、旧は無視される旨の deprecation warning がプロセス起動ごとに 1 度だけ出力されます。空文字や空白のみの値は未設定として扱われます。
- 設定ファイル: `akashicLevel` を主、`observabilityLevel` を fallback として読み込みます。両方が存在する場合は新を採用し旧は警告対象になります。設定保存（TUI からの変更等）の際は `akashicLevel` フィールドのみが書き出されるため、次回読み込み以降は旧フィールドが自動的に消えます。
