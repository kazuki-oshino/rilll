# 使い方

公開版のバイナリをローカルにビルドして起動します。

```bash
just build
./bin/rilll
```

以下の例は `bin/` をPATHに追加した状態を想定しています。
別の作業リポジトリで使う場合は、[セットアップ](setup.md)の手順でbridgeの場所も明示してください。
コミットを伴うワークフローは、利用先での `git-commit` skill配置が必要です。

起動するとワークフロー選択メニューが表示されます。矢印キーでワークフローを選択し、Enterで実行します。`s`キーでprovider設定を開き、`claude` / `cursor` / `codex` / `cursor-sdk` providerを切り替えられます。ワークフロー完了後は結果画面が表示され、Enterでメニューに戻れます。

## 非対話 CLI（CI / スクリプト統合）

TUI を介さずにワークフローを直接実行できます。CI、エディタ統合、シェルスクリプト経由の呼び出しを想定しています。

```bash
# 登録ワークフロー一覧（ID / Label / 必要引数）を表示
rilll list

# 引数不要なワークフローを実行
rilll run git-commit
rilll run update-readme

# 引数必須のワークフローを実行（--feature を渡す）
rilll run kiro-impl-loop --feature my-feature
rilll run kiro-spec-loop --feature my-feature
# spec作成対象なら spec→impl、実装可能な既存specなら impl から開始
rilll run kiro-spec-impl-loop --feature my-feature

# 複数 feature を指定順に実行（途中失敗で後続は未起動）
rilll run kiro-spec-impl-loop-sequence --features foundation,feature-a,feature-b

# ワークフローごとのヘルプ
rilll run kiro-impl-loop --help
rilll help run kiro-impl-loop
```

### 出力規約

- 進捗イベント（テキスト / ツール呼び出し / ステップ）は **stderr** に流れます。`--quiet` (`-q`) で抑制できます。
- 最終結果（`WorkflowDone.Result`）は **stdout** に出力されます。スクリプトから `$(rilll run ...)` で捕捉してください。
- エラーメッセージ（`WorkflowError`）は stderr に出ます。

### Exit code

| code | 意味 |
|---|---|
| 0 | 成功 |
| 1 | ワークフロー実行時エラー |
| 2 | CLI 誤用（未知サブコマンド / 未知 ID / 必要引数不足 / 引数不正） |

### 共通フラグ（`run` 配下）

| フラグ | 説明 |
|---|---|
| `--cwd <dir>` | 作業ディレクトリ（未指定時はプロセス CWD） |
| `--quiet`, `-q` | 進捗イベントを stderr に出さない |

引数なしで `rilll` を起動した場合は、これまで通り TUI が立ち上がります（後方互換）。`rilll --version` も従来どおり動作します。
