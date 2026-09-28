package telemetry

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/kazuki-oshino/rilll/internal/config"
	"github.com/kazuki-oshino/rilll/internal/provider"
)

var exportEnabled atomic.Bool

const (
	metricWorkflowRunDuration = "rilll.workflow.run.duration"
	metricWorkflowRunCount    = "rilll.workflow.run.count"
	metricAgentCallDuration   = "rilll.agent.call.duration"
	metricAgentCallCount      = "rilll.agent.call.count"
	metricAgentCallCost       = "rilll.agent.call.cost"
	metricAgentCallTokens     = "rilll.agent.call.tokens"
	metricNodeBridgeDuration  = "rilll.nodebridge.run.duration"
	metricNodeBridgeCount     = "rilll.nodebridge.run.count"
	metricDBDuration          = "rilll.akashic.db.duration"
	metricDBCount             = "rilll.akashic.db.count"
)

func init() {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
}

// ExportEnabled は Init 成功後に bridge 子プロセスへ OTel 有効化を伝えるためのフラグ。
func ExportEnabled() bool {
	return exportEnabled.Load()
}

// BridgeProcessEnv は bridge 子プロセス用の環境変数を組み立て、RILLL_OTEL_ENABLED を上書きする。
func BridgeProcessEnv(base []string) []string {
	value := "false"
	if ExportEnabled() {
		value = "true"
	}
	return withEnvOverride(base, "RILLL_OTEL_ENABLED", value)
}

func Init(ctx context.Context, cfg config.Config, serviceVersion string) (func(context.Context) error, error) {
	exportEnabled.Store(false)
	if !Enabled(cfg, os.Getenv) {
		return func(context.Context) error { return nil }, nil
	}

	res := resource.NewWithAttributes(
		"",
		attribute.String("service.name", serviceName),
		attribute.String("service.version", serviceVersion),
	)

	traceExporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	metricExporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, err
	}

	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
	)
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
		sdkmetric.WithResource(res),
	)
	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	exportEnabled.Store(true)

	return func(ctx context.Context) error {
		exportEnabled.Store(false)
		return errors.Join(
			tracerProvider.Shutdown(ctx),
			meterProvider.Shutdown(ctx),
		)
	}, nil
}

func StartWorkflowRun(ctx context.Context, id WorkflowIdentity) (context.Context, trace.Span) {
	return otel.Tracer(instrumentationName).Start(ctx, "workflow.run", trace.WithAttributes(otelAttrs(WorkflowSpanAttributes(id))...))
}

func EndWorkflowRun(ctx context.Context, span trace.Span, id WorkflowIdentity, startedAt time.Time, err error) {
	status := statusFromError(err)
	RecordWorkflowRun(ctx, id, status, time.Since(startedAt))
	endSpan(span, err, "workflow_failed")
}

func StartAgentCall(ctx context.Context, id AgentIdentity) (context.Context, trace.Span) {
	return otel.Tracer(instrumentationName).Start(ctx, "agent.call", trace.WithAttributes(otelAttrs(AgentSpanAttributes(id))...))
}

func EndAgentCall(ctx context.Context, span trace.Span, id AgentIdentity, failureCategory string, duration time.Duration, metrics AgentCallMetrics, err error) {
	RecordAgentCall(ctx, id, failureCategory, duration, metrics)
	if failureCategory != "" {
		SetSpanAttributes(span, stringAttr("rilll.agent.failure_category", failureCategory))
		if err == nil {
			err = errors.New(failureCategory)
		}
		endSpan(span, err, failureCategory)
		return
	}
	endSpan(span, err, "agent_call_failed")
}

func StartNodeBridgeRun(ctx context.Context, id NodeBridgeIdentity) (context.Context, trace.Span) {
	return otel.Tracer(instrumentationName).Start(ctx, "nodebridge.run", trace.WithAttributes(otelAttrs(NodeBridgeSpanAttributes(id))...))
}

func EndNodeBridgeRun(ctx context.Context, span trace.Span, id NodeBridgeIdentity, startedAt time.Time, err error) {
	RecordNodeBridgeRun(ctx, id, statusFromError(err), time.Since(startedAt))
	endSpan(span, err, "nodebridge_failed")
}

func StartDBOperation(ctx context.Context, operation string) (context.Context, trace.Span, func(error)) {
	startedAt := time.Now()
	ctx, span := otel.Tracer(instrumentationName).Start(ctx, "akashic.db", trace.WithAttributes(otelAttrs(DBAttributes(operation))...))
	return ctx, span, func(err error) {
		RecordDBOperation(ctx, operation, statusFromError(err), time.Since(startedAt))
		endSpan(span, err, "db_error")
	}
}

func AddSpanEvent(span trace.Span, name string, attrs ...KeyValue) {
	if span == nil {
		return
	}
	span.AddEvent(name, trace.WithAttributes(otelAttrs(attrs)...))
}

func SetSpanAttributes(span trace.Span, attrs ...KeyValue) {
	if span == nil {
		return
	}
	span.SetAttributes(otelAttrs(attrs)...)
}

func InjectTraceContext(ctx context.Context) provider.TraceContext {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return provider.TraceContext{
		TraceParent: carrier.Get("traceparent"),
		TraceState:  carrier.Get("tracestate"),
		Baggage:     carrier.Get("baggage"),
	}
}

type AgentCallMetrics struct {
	TotalCostUSD *float64
	InputTokens  *int64
	OutputTokens *int64
}

func RecordWorkflowRun(ctx context.Context, id WorkflowIdentity, status string, duration time.Duration) {
	inst := getInstruments()
	attrs := metric.WithAttributes(otelAttrs(WorkflowMetricAttributes(id, status))...)
	inst.workflowRunDuration.Record(ctx, duration.Seconds()*1000, attrs)
	inst.workflowRunCount.Add(ctx, 1, attrs)
}

func RecordAgentCall(ctx context.Context, id AgentIdentity, failureCategory string, duration time.Duration, metrics AgentCallMetrics) {
	inst := getInstruments()
	attrs := metric.WithAttributes(otelAttrs(AgentMetricAttributes(id, failureCategory))...)
	inst.agentCallDuration.Record(ctx, duration.Seconds()*1000, attrs)
	inst.agentCallCount.Add(ctx, 1, attrs)
	if metrics.TotalCostUSD != nil {
		inst.agentCallCost.Record(ctx, *metrics.TotalCostUSD, attrs)
	}
	if metrics.InputTokens != nil {
		inst.agentCallTokens.Add(ctx, *metrics.InputTokens, metric.WithAttributes(append(otelAttrs(AgentMetricAttributes(id, failureCategory)), attribute.String("rilll.agent.token_type", "input"))...))
	}
	if metrics.OutputTokens != nil {
		inst.agentCallTokens.Add(ctx, *metrics.OutputTokens, metric.WithAttributes(append(otelAttrs(AgentMetricAttributes(id, failureCategory)), attribute.String("rilll.agent.token_type", "output"))...))
	}
}

func RecordNodeBridgeRun(ctx context.Context, id NodeBridgeIdentity, status string, duration time.Duration) {
	inst := getInstruments()
	attrs := metric.WithAttributes(otelAttrs(NodeBridgeMetricAttributes(id, status))...)
	inst.nodeBridgeDuration.Record(ctx, duration.Seconds()*1000, attrs)
	inst.nodeBridgeCount.Add(ctx, 1, attrs)
}

func RecordDBOperation(ctx context.Context, operation, status string, duration time.Duration) {
	inst := getInstruments()
	attrs := metric.WithAttributes(append(otelAttrs(DBAttributes(operation)), attribute.String("rilll.status", status))...)
	inst.dbDuration.Record(ctx, duration.Seconds()*1000, attrs)
	inst.dbCount.Add(ctx, 1, attrs)
}

type instruments struct {
	workflowRunDuration metric.Float64Histogram
	workflowRunCount    metric.Int64Counter
	agentCallDuration   metric.Float64Histogram
	agentCallCount      metric.Int64Counter
	agentCallCost       metric.Float64Histogram
	agentCallTokens     metric.Int64Counter
	nodeBridgeDuration  metric.Float64Histogram
	nodeBridgeCount     metric.Int64Counter
	dbDuration          metric.Float64Histogram
	dbCount             metric.Int64Counter
}

var (
	instrumentsOnce sync.Once
	cached          instruments
)

func getInstruments() instruments {
	instrumentsOnce.Do(func() {
		meter := otel.Meter(instrumentationName)
		cached.workflowRunDuration, _ = meter.Float64Histogram(metricWorkflowRunDuration, metric.WithUnit("ms"))
		cached.workflowRunCount, _ = meter.Int64Counter(metricWorkflowRunCount)
		cached.agentCallDuration, _ = meter.Float64Histogram(metricAgentCallDuration, metric.WithUnit("ms"))
		cached.agentCallCount, _ = meter.Int64Counter(metricAgentCallCount)
		cached.agentCallCost, _ = meter.Float64Histogram(metricAgentCallCost, metric.WithUnit("USD"))
		cached.agentCallTokens, _ = meter.Int64Counter(metricAgentCallTokens, metric.WithUnit("{token}"))
		cached.nodeBridgeDuration, _ = meter.Float64Histogram(metricNodeBridgeDuration, metric.WithUnit("ms"))
		cached.nodeBridgeCount, _ = meter.Int64Counter(metricNodeBridgeCount)
		cached.dbDuration, _ = meter.Float64Histogram(metricDBDuration, metric.WithUnit("ms"))
		cached.dbCount, _ = meter.Int64Counter(metricDBCount)
	})
	return cached
}

func endSpan(span trace.Span, err error, sanitizedMessage string) {
	if span == nil {
		return
	}
	if err != nil {
		if sanitizedMessage == "" {
			sanitizedMessage = "operation_failed"
		}
		span.RecordError(errors.New(sanitizedMessage))
		span.SetStatus(codes.Error, sanitizedMessage)
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}

func statusFromError(err error) string {
	if err != nil {
		return "failure"
	}
	return "success"
}

func withEnvOverride(base []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(base)+1)
	for _, entry := range base {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		out = append(out, entry)
	}
	return append(out, prefix+value)
}

func otelAttrs(attrs []KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, attr := range attrs {
		switch value := attr.Value.(type) {
		case string:
			out = append(out, attribute.String(attr.Key, value))
		case bool:
			out = append(out, attribute.Bool(attr.Key, value))
		case int:
			out = append(out, attribute.Int(attr.Key, value))
		case int64:
			out = append(out, attribute.Int64(attr.Key, value))
		case float64:
			out = append(out, attribute.Float64(attr.Key, value))
		}
	}
	return out
}
