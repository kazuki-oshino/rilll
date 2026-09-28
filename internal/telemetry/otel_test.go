package telemetry

import (
	"testing"

	"github.com/kazuki-oshino/rilll/internal/config"
)

func TestBridgeProcessEnvOverridesRILLLOtelEnabled(t *testing.T) {
	exportEnabled.Store(false)
	t.Cleanup(func() { exportEnabled.Store(false) })

	got := BridgeProcessEnv([]string{
		"RILLL_OTEL_ENABLED=true",
		"OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318",
	})
	if !containsEnv(got, "RILLL_OTEL_ENABLED=false") {
		t.Fatalf("expected bridge env to force disable, got %#v", got)
	}

	exportEnabled.Store(true)
	got = BridgeProcessEnv([]string{"RILLL_OTEL_ENABLED=false"})
	if !containsEnv(got, "RILLL_OTEL_ENABLED=true") {
		t.Fatalf("expected bridge env to force enable, got %#v", got)
	}
}

func TestInitSkipsWhenConfigDisabled(t *testing.T) {
	exportEnabled.Store(false)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")

	shutdown, err := Init(t.Context(), config.Config{}, "test")
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if ExportEnabled() {
		t.Fatal("expected ExportEnabled=false when config disables otel")
	}
	if err := shutdown(t.Context()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
}

func containsEnv(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}
