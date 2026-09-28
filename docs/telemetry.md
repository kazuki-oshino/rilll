# OpenTelemetry

rilll の OpenTelemetry は、アカシックレコードを置き換えず、実行中の横断トレースと低カーディナリティなメトリクスを補うための opt-in 計測です。

## 責務分担

- アカシックレコード: workflow / agent call / raw event / context usage / artifact を SQLite に永続化する監査ログ。
- OpenTelemetry: workflow、agent call、Node bridge、SQLite 操作の遅延・失敗分類・provider/model 差分を外部 backend で見るための trace / metric。

## 有効化

未設定時は no-op です。`config.json` の `otelEnabled: true` と OTLP endpoint 環境変数の両方が揃ったときだけ OTLP HTTP exporter を初期化します。

- `otelEnabled` (`~/.config/rilll/config.json` または TUI 設定画面) — 既定は `false`
- `OTEL_EXPORTER_OTLP_ENDPOINT`
- `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`
- `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`

`otelEnabled: false` のときは OTEL_* 環境変数があっても接続しません。`otelEnabled: true` かつ endpoint 未設定のときも no-op です。

ローカルで確認する最小構成は、OTLP/HTTP を受ける backend を立て、設定で OTel を ON にしてから rilll を起動します。

```bash
docker compose -f docker/otel/docker-compose.yml up -d

# TUI の s → OpenTelemetry → ON を選ぶか、config.json に "otelEnabled": true を書く
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318

rilll run <workflow-id>
```

Grafana は <http://localhost:3000> で開きます。`grafana/otel-lgtm` の既定ユーザーは `admin` / `admin` です。

OpenTelemetry の ON/OFF は TUI 設定画面 (`s` → OpenTelemetry) または `config.json` の `otelEnabled` で切り替えます。変更は次回起動時から反映されます。

## ローカル backend / collector

現在の rilll には trace と metric の送信があります。prompt、結果本文、tool args、raw observation payload は OTel に送らないため、詳細本文を追う用途は引き続きアカシックレコードを使います。

### 推奨: Grafana LGTM single container

ローカル開発・デモ・手元の動作確認では [`grafana/otel-lgtm`](https://grafana.com/docs/opentelemetry/docker-lgtm/) を推奨します。

- OpenTelemetry Collector、Grafana、Tempo、Prometheus、Loki、Pyroscope が 1 コンテナで起動する。
- OTLP/HTTP (`4318`) と OTLP/gRPC (`4317`) をそのまま受けられる。
- rilll 側は `otelEnabled: true` と `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318` で接続できる。
- 本番向けではなく、開発・デモ・テスト用の backend として扱う。

この構成では独自 Dockerfile は不要です。公式イメージをそのまま使うため、`docker/otel/docker-compose.yml` だけを置いています。

#### Trace を見る

1. Grafana の左メニューから Explore を開く。
2. 画面上部の datasource を `Tempo` にする。
3. Query type を `Search` または `TraceQL` にする。
4. 時間範囲を `Last 15 minutes` など、rilll を実行した時間が含まれる範囲にする。
5. `Search` の場合は Service Name に `rilll` を選ぶ。
6. `Run query` を押す。

TraceQL が使える場合は、次の query で rilll の trace を探せます。

```traceql
{ resource.service.name = "rilll" }
```

### 代替: Jaeger all-in-one

trace だけを軽く見たい場合は [Jaeger v2 all-in-one](https://www.jaegertracing.io/docs/2.17/getting-started/) が最短です。

```bash
docker run --rm --name jaeger \
  -p 16686:16686 \
  -p 4317:4317 \
  -p 4318:4318 \
  cr.jaegertracing.io/jaegertracing/jaeger:2.17.0

export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
# あわせて TUI または config.json で otelEnabled: true にする
```

UI は <http://localhost:16686> です。Jaeger は trace の確認に強い一方、rilll が出す OTel metrics の確認には別途 Prometheus などが必要です。

### 代替: OpenTelemetry Collector + 個別 backend

本番に近い routing、batch、retry、filtering、複数 backend への fan-out を検証したい場合は、[OpenTelemetry Collector](https://opentelemetry.io/docs/collector/) を front に置き、Tempo / Prometheus / Loki などへ送ります。

- traces: Collector → Tempo
- metrics: Collector → Prometheus または Mimir
- logs: rilll は現状 OTel logs を出さないが、他アプリと合わせるなら Collector → Loki

この方式は構成ファイルが増える代わりに、本番と同じ「アプリは Collector にだけ送る」形を再現できます。rilll 単体の手元確認では `grafana/otel-lgtm` の方が簡単です。

### 代替: Grafana Alloy

Grafana stack 前提で Collector 相当の pipeline を細かく組みたい場合は [Grafana Alloy](https://grafana.com/oss/alloy-opentelemetry-collector/) も候補です。Alloy は Grafana Labs の OpenTelemetry Collector distribution で、OTLP receiver、Prometheus pipeline、Tempo/Loki/Mimir 連携をまとめて扱えます。

rilll の現状用途では Alloy 固有機能までは不要なので、まずは LGTM single container、次に必要が出たら Collector / Alloy へ進むのが扱いやすいです。

## 属性ルール

- span には `workflow_run_id` と `agent_call_id` を相関 ID として載せる。
- metrics label には `workflow_run_id`、`agent_call_id`、`session_id`、`cwd`、prompt、raw payload を載せない。
- session は raw ID ではなく `resume_session_present` の boolean だけを載せる。
- prompt、結果本文、tool args、raw observation payload は OTel に送らず、必要な場合はアカシックレコードで確認する。

## 調査導線

- stream idle / provider error: `workflow.run` → `agent.call` → `nodebridge.run` の status と `rilll.agent.failure_category` で失敗箇所を絞り、詳細本文はアカシックレコードの `agent_calls` / `raw_events` で確認する。
- resume fallback: bridge 側 `bridge.run` span event と、既存の `resume_fallback` observation を `workflow_run_id` / `agent_call_id` で突き合わせる。
- context usage 高騰: bridge の `context_usage` observation と `agent.call` token/cost metrics を併用し、必要に応じてアカシックレコードの `agent_context_usage` を読む。
- SQLite 遅延: `akashic.db` span と `rilll.akashic.db.duration` metric で保存処理の詰まりを確認する。
