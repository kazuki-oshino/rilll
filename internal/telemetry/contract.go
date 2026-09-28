package telemetry

import (
	"strings"

	"github.com/kazuki-oshino/rilll/internal/config"
)

const (
	instrumentationName = "github.com/kazuki-oshino/rilll"
	serviceName         = "rilll"
)

// KeyValue is rilll's small telemetry contract type. Keeping this separate from
// the OpenTelemetry attribute package lets tests assert the privacy contract
// without depending on exporter-specific formatting.
type KeyValue struct {
	Key   string
	Value any
}

type WorkflowIdentity struct {
	WorkflowRunID string
	WorkflowID    string
	WorkflowName  string
	ProjectID     string
	CWD           string
}

type AgentIdentity struct {
	WorkflowRunID   string
	WorkflowID      string
	WorkflowName    string
	StepID          string
	StepName        string
	AgentCallID     string
	ProviderID      string
	Model           string
	Effort          string
	ResumeSessionID string
	OutputFormat    string
}

type NodeBridgeIdentity struct {
	ProviderID    string
	WorkflowRunID string
	WorkflowID    string
	StepID        string
	AgentCallID   string
	Model         string
	Effort        string
}

// Enabled は config.OtelEnabled が true かつ OTLP endpoint 環境変数が設定されているときのみ
// OpenTelemetry を有効化する。config が最優先であり、OTEL_* だけでは有効化しない。
func Enabled(cfg config.Config, lookup func(string) string) bool {
	if !cfg.OtelEnabled || lookup == nil {
		return false
	}
	return strings.TrimSpace(lookup("OTEL_EXPORTER_OTLP_ENDPOINT")) != "" ||
		strings.TrimSpace(lookup("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")) != "" ||
		strings.TrimSpace(lookup("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT")) != ""
}

func WorkflowSpanAttributes(id WorkflowIdentity) []KeyValue {
	return compactAttrs([]KeyValue{
		stringAttr("rilll.workflow.run_id", id.WorkflowRunID),
		stringAttr("rilll.workflow.id", id.WorkflowID),
		stringAttr("rilll.workflow.name", id.WorkflowName),
		stringAttr("rilll.project.id", id.ProjectID),
	})
}

func WorkflowMetricAttributes(id WorkflowIdentity, status string) []KeyValue {
	return compactAttrs([]KeyValue{
		stringAttr("rilll.workflow.id", id.WorkflowID),
		stringAttr("rilll.project.id", id.ProjectID),
		stringAttr("rilll.status", status),
	})
}

func AgentSpanAttributes(id AgentIdentity) []KeyValue {
	return compactAttrs([]KeyValue{
		stringAttr("rilll.workflow.run_id", id.WorkflowRunID),
		stringAttr("rilll.workflow.id", id.WorkflowID),
		stringAttr("rilll.workflow.name", id.WorkflowName),
		stringAttr("rilll.workflow.step_id", id.StepID),
		stringAttr("rilll.workflow.step_name", id.StepName),
		stringAttr("rilll.agent.call_id", id.AgentCallID),
		stringAttr("rilll.agent.provider_id", id.ProviderID),
		stringAttr("rilll.agent.model", id.Model),
		stringAttr("rilll.agent.effort", id.Effort),
		boolAttr("rilll.agent.resume_session_present", id.ResumeSessionID != ""),
		stringAttr("rilll.agent.output_format", id.OutputFormat),
	})
}

func AgentMetricAttributes(id AgentIdentity, failureCategory string) []KeyValue {
	status := "success"
	if failureCategory != "" {
		status = "failure"
	}
	return compactAttrs([]KeyValue{
		stringAttr("rilll.workflow.id", id.WorkflowID),
		stringAttr("rilll.workflow.step_id", id.StepID),
		stringAttr("rilll.agent.provider_id", id.ProviderID),
		stringAttr("rilll.agent.model", id.Model),
		stringAttr("rilll.agent.effort", id.Effort),
		stringAttr("rilll.status", status),
		stringAttr("rilll.agent.failure_category", failureCategory),
	})
}

func NodeBridgeSpanAttributes(id NodeBridgeIdentity) []KeyValue {
	return compactAttrs([]KeyValue{
		stringAttr("rilll.agent.provider_id", id.ProviderID),
		stringAttr("rilll.workflow.run_id", id.WorkflowRunID),
		stringAttr("rilll.workflow.id", id.WorkflowID),
		stringAttr("rilll.workflow.step_id", id.StepID),
		stringAttr("rilll.agent.call_id", id.AgentCallID),
		stringAttr("rilll.agent.model", id.Model),
		stringAttr("rilll.agent.effort", id.Effort),
	})
}

func NodeBridgeMetricAttributes(id NodeBridgeIdentity, status string) []KeyValue {
	return compactAttrs([]KeyValue{
		stringAttr("rilll.agent.provider_id", id.ProviderID),
		stringAttr("rilll.workflow.id", id.WorkflowID),
		stringAttr("rilll.workflow.step_id", id.StepID),
		stringAttr("rilll.agent.model", id.Model),
		stringAttr("rilll.agent.effort", id.Effort),
		stringAttr("rilll.status", status),
	})
}

func DBAttributes(operation string) []KeyValue {
	return compactAttrs([]KeyValue{
		stringAttr("db.system", "sqlite"),
		stringAttr("db.operation.name", operation),
	})
}

func stringAttr(key, value string) KeyValue {
	if value == "" {
		return KeyValue{}
	}
	return KeyValue{Key: key, Value: value}
}

func boolAttr(key string, value bool) KeyValue {
	return KeyValue{Key: key, Value: value}
}

func compactAttrs(attrs []KeyValue) []KeyValue {
	out := attrs[:0]
	for _, attr := range attrs {
		if attr.Key != "" {
			out = append(out, attr)
		}
	}
	return out
}
