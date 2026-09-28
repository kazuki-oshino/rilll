package telemetry

import (
	"slices"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/config"
)

func TestEnabledRequiresConfigOptIn(t *testing.T) {
	endpointEnv := map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318"}
	cases := []struct {
		name string
		cfg  config.Config
		env  map[string]string
		want bool
	}{
		{name: "empty", cfg: config.Config{}, env: map[string]string{}, want: false},
		{name: "config off ignores endpoint", cfg: config.Config{}, env: endpointEnv, want: false},
		{name: "config on without endpoint", cfg: config.Config{OtelEnabled: true}, env: map[string]string{}, want: false},
		{name: "config on with endpoint", cfg: config.Config{OtelEnabled: true}, env: endpointEnv, want: true},
		{name: "trace endpoint", cfg: config.Config{OtelEnabled: true}, env: map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://localhost:4318/v1/traces"}, want: true},
		{name: "metric endpoint", cfg: config.Config{OtelEnabled: true}, env: map[string]string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://localhost:4318/v1/metrics"}, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Enabled(tc.cfg, func(key string) string { return tc.env[key] })
			if got != tc.want {
				t.Fatalf("Enabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAgentSpanAttributesCarryCorrelationButNoSecrets(t *testing.T) {
	attrs := AgentSpanAttributes(AgentIdentity{
		WorkflowRunID:   "workflow-run-1",
		WorkflowID:      "review-workflow",
		StepID:          "rule-security",
		AgentCallID:     "agent-call-1",
		ProviderID:      "codex",
		Model:           "gpt-5.5",
		Effort:          "high",
		ResumeSessionID: "session-secret",
		OutputFormat:    "json_schema",
	})
	values := attrValues(attrs)

	for _, want := range []string{
		"workflow-run-1",
		"review-workflow",
		"rule-security",
		"agent-call-1",
		"codex",
		"gpt-5.5",
		"high",
		"json_schema",
	} {
		if !slices.Contains(values, want) {
			t.Fatalf("span attributes missing %q: %#v", want, attrs)
		}
	}
	if slices.Contains(values, "session-secret") {
		t.Fatalf("span attributes must not contain raw session IDs: %#v", attrs)
	}
	if !attrBool(attrs, "rilll.agent.resume_session_present") {
		t.Fatalf("span attributes should expose resume session presence without the raw ID: %#v", attrs)
	}
}

func TestAgentMetricAttributesExcludeHighCardinalityIDs(t *testing.T) {
	attrs := AgentMetricAttributes(AgentIdentity{
		WorkflowRunID: "workflow-run-1",
		WorkflowID:    "review-workflow",
		StepID:        "rule-security",
		AgentCallID:   "agent-call-1",
		ProviderID:    "cursor-sdk",
		Model:         "composer-2.5",
		Effort:        "medium",
	}, "provider_error")
	values := attrValues(attrs)

	for _, forbidden := range []string{"workflow-run-1", "agent-call-1"} {
		if slices.Contains(values, forbidden) {
			t.Fatalf("metric attributes must not contain high-cardinality ID %q: %#v", forbidden, attrs)
		}
	}
	for _, want := range []string{"review-workflow", "rule-security", "cursor-sdk", "composer-2.5", "medium", "provider_error"} {
		if !slices.Contains(values, want) {
			t.Fatalf("metric attributes missing %q: %#v", want, attrs)
		}
	}
}

func attrValues(attrs []KeyValue) []string {
	values := make([]string, 0, len(attrs))
	for _, attr := range attrs {
		switch value := attr.Value.(type) {
		case string:
			values = append(values, value)
		}
	}
	return values
}

func attrBool(attrs []KeyValue, key string) bool {
	for _, attr := range attrs {
		if attr.Key == key {
			value, _ := attr.Value.(bool)
			return value
		}
	}
	return false
}
