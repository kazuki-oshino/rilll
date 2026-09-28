# セットアップ

## ビルドとローカル検証

Go 1.26.6以上、Node.js 24.x、npmが必要です。
macOS / Linuxのローカル開発環境を想定しています。

```sh
npm ci --ignore-scripts
npm run build:bridge
go build -trimpath -ldflags "-X github.com/kazuki-oshino/rilll/internal/version.BuildVersion=$(cat VERSION)" -o bin/rilll ./cmd/rilll
./bin/rilll --version
./bin/rilll list

go test ./...
go vet ./...
npm run test:bridge
```

`just` を使う場合は `just build` / `just test` が同じ用途です。
ビルドは `bin/rilll` と各bridgeの `dist/` に出力し、VERSIONやグローバルの実行ファイルを変更しません。
依存パッケージはlockfileからインストールします。
インストール時のlifecycle scriptは無効にし、bridgeは明示的にコンパイルします。

`list` は登録済みワークフローを表示し、AIを呼び出しません。
CLIの初期化で空の実行記録DBが作られる場合があります。
引数なしの `./bin/rilll` でTUIを開き、`s` でProvider設定を選択できます。

## Provider認証

実際のワークフロー実行には、利用するProviderのアカウントと認証が必要です。
API利用料やモデルへのアクセス権は各Providerの契約に依存します。

| Provider | 必要な設定 |
| --- | --- |
| `claude` | Claude Agent SDKが利用できる認証。APIキーを使う場合は `ANTHROPIC_API_KEY` |
| `codex` | Codexのログイン済み認証、または `RILLL_CODEX_API_KEY` / `OPENAI_API_KEY` |
| `cursor-sdk` | `CURSOR_API_KEY` |
| `cursor` | インストール済みの `cursor-agent` CLIとその認証 |

rilllは `.env` を自動で読み込みません。
認証情報は利用者自身の環境に設定し、リポジトリへ保存しないでください。
モデルのプリセットは公開スナップショット時点の値です。
モデルの廃止、SDKの仕様変更、アカウント権限により、そのまま実行できなくなる場合があります。
現在の定義は [config.go](../internal/config/config.go) と各bridgeにあります。

## 別の作業リポジトリで使う

`-trimpath` を使ったビルドは、開発者のソース配置先をバイナリに埋め込みません。
別ディレクトリから実行するときは、ビルドしたbridgeの場所を明示してください。
以下はrilllのリポジトリルートで実行します。

```sh
export RILLL_CLAUDE_BRIDGE="$PWD/bridge/claude-agent-sdk/dist/src/index.js"
export RILLL_CODEX_BRIDGE="$PWD/bridge/codex-sdk/dist/src/index.js"
export RILLL_CURSOR_SDK_BRIDGE="$PWD/bridge/cursor-sdk/dist/src/index.js"
export PATH="$PWD/bin:$PATH"
```

その後、対象リポジトリへ移動して `rilll` を起動します。
ワークフローが編集する対象はカレントディレクトリです。
Kiro系ワークフローには、利用先での [cc-sddのSkillsモード導入](skills.md#cc-sddを利用先に導入する) が必要です。
学習処理などに使うRill独自のスキルは、[同梱スキルの配置](skills.md#同梱する独自スキル) に従って別途配置してください。
コミットを含むワークフローでは、同梱の `git-commit` skillも配置してください。
これは公開版用の最小手順で、対象を指定したローカルコミットまでを扱います。

## 設定とデータの保存先

| 内容 | 既定の保存先 |
| --- | --- |
| 設定 | `$XDG_CONFIG_HOME/rilll/config.json` または `~/.config/rilll/config.json` |
| 実行記録 | `$XDG_DATA_HOME/rilll/akashic.sqlite` または `~/.local/share/rilll/akashic.sqlite` |
| 実行成果物 | 作業リポジトリの `.rilll/` |
| レビュー規則、facets | 作業リポジトリの `.rilll-config/` |
| ユーザーhook | `$XDG_CONFIG_HOME/rilll/hooks/` または `~/.config/rilll/hooks/` |
| 学習出力 | 作業リポジトリの `docs_dev/playbook/`（有効化した場合） |

private版Rillの設定やDBは自動で読み込まず、移行もしません。
`RILL_*` 環境変数は参照せず、公開版は `RILLL_*` を使います。
Provider自身の認証とセッション保存先（例：`~/.codex/`）は各SDKが管理しており、rilll専用に隔離されません。
同じ作業リポジトリでのファイル編集や学習出力も共有されるため、試すときは別の使い捨てcloneを用意してください。

最小設定の例です。

```json
{
  "agentProvider": "claude",
  "akashicLevel": "summary",
  "otelEnabled": false,
  "codex": {
    "modelPreset": "gpt-5.5-xhigh",
    "reasoningSummary": "concise"
  },
  "cursorSDK": { "modelPreset": "composer-2.5" }
}
```

設定はTUIから変更できます。
詳細な保存形式は [config.go](../internal/config/config.go)、記録レベルは [Akashic](akashic.md) を参照してください。

レビュー系ワークフローには、利用先の `.rilll-config/review-rules/` に有効なYAML規則が必要です。
公開版はプロジェクト固有の設定を同梱していないため、[review-rules-skill](skills.md#同梱する独自スキル) などで用意してください。
facetsの上書きは任意で、未配置の場合はアプリ内の既定値を使います。

## 実行権限とログ

ワークフローはファイル編集、shellコマンド、Git操作、ネットワークアクセスを行えます。
Claude bridgeの既定は `acceptEdits`、Codex bridgeの既定は `danger-full-access` / `approvalPolicy: never` です。
Cursor SDKもローカルの作業環境にアクセスします。
bridgeには対話的な承認UIがないため、実験は機密情報を置いていない作業環境で行ってください。
特にCodexの既定はOSレベルで作業ディレクトリ内に隔離する設定ではありません。

一部bridgeのdeny gateやツール制限は多層防御の一部であり、完全な隔離を保証しません。
対象は [共有denyルール](../bridge/shared/deny-patterns.ts) と各Providerの実装で確認できます。

生のbridge出力を一時ファイルへ保存する機能は、公開版では既定で無効です。
デバッグ目的で `RILLL_BRIDGE_STDOUT_DUMP=enable` を指定したときだけ、OSの一時ディレクトリへ所有者のみ読み書きできるファイルを作成します。
この出力やSQLite、ワークフロー成果物にはプロンプトや対象コードが含まれ得るため、公開しないでください。
OpenTelemetryも既定で無効です。
外部backendへ送る内容は [telemetry.md](telemetry.md) を参照してください。
