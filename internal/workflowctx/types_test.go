package workflowctx

import (
	"context"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

// ContextWithRunMetadata で注入した値が RunMetadataFromContext で取り出せることを確認する。
func TestContextWithRunMetadataRoundTrip(t *testing.T) {
	want := RunMetadata{
		WorkflowRunID: "run-1",
		WorkflowID:    "update-readme",
		WorkflowName:  "README更新",
		CWD:           "/tmp/repo",
	}
	ctx := ContextWithRunMetadata(context.Background(), want)

	got, ok := RunMetadataFromContext(ctx)
	if !ok {
		t.Fatalf("expected ok=true, got false")
	}
	if got != want {
		t.Fatalf("RunMetadata mismatch: got %#v, want %#v", got, want)
	}
}

// 未注入 ctx に対しては (zero, false) が返ることを確認する。
func TestRunMetadataFromContextReturnsFalseWhenAbsent(t *testing.T) {
	got, ok := RunMetadataFromContext(context.Background())
	if ok {
		t.Fatalf("expected ok=false for empty ctx, got %#v", got)
	}
	if (got != RunMetadata{}) {
		t.Fatalf("expected zero RunMetadata, got %#v", got)
	}
}

func TestEarlyReflectionPolicyFromContextReturnsFalseWhenAbsent(t *testing.T) {
	got, ok := EarlyReflectionPolicyFromContext(context.Background())
	if ok {
		t.Fatalf("expected ok=false for empty ctx, got %#v", got)
	}
	if got.AllowsProvider(provider.ProviderClaude) {
		t.Fatalf("expected absent policy to deny provider")
	}
}

func TestEarlyReflectionPolicyRoundTripThroughChildContext(t *testing.T) {
	notified := false
	want := EarlyReflectionPolicy{
		AllowedProviders: []provider.ProviderID{provider.ProviderClaude, provider.ProviderCodex},
		Notify: func() {
			notified = true
		},
	}

	parent := ContextWithEarlyReflectionEnabled(context.Background(), want)
	wantMetadata := RunMetadata{WorkflowRunID: "front-run"}
	child := ContextWithRunMetadata(parent, wantMetadata)

	got, ok := EarlyReflectionPolicyFromContext(child)
	if !ok {
		t.Fatalf("expected ok=true from child context")
	}
	gotMetadata, ok := RunMetadataFromContext(child)
	if !ok {
		t.Fatalf("expected RunMetadata to remain available")
	}
	if gotMetadata != wantMetadata {
		t.Fatalf("RunMetadata mismatch: got %#v, want %#v", gotMetadata, wantMetadata)
	}
	if !got.AllowsProvider(provider.ProviderClaude) {
		t.Fatalf("expected policy to allow claude")
	}
	if !got.AllowsProvider(provider.ProviderCodex) {
		t.Fatalf("expected policy to allow codex")
	}
	if got.AllowsProvider(provider.ProviderCursor) {
		t.Fatalf("expected policy to deny cursor")
	}
	if got.Notify == nil {
		t.Fatalf("expected notify callback to round-trip")
	}
	got.Notify()
	if !notified {
		t.Fatalf("expected notify callback from child policy to run")
	}
}

func TestEarlyReflectionPolicyAllowsProviderDeniesEmptyAllowlist(t *testing.T) {
	policy := EarlyReflectionPolicy{}

	if policy.AllowsProvider(provider.ProviderClaude) {
		t.Fatalf("expected empty allowlist to deny claude")
	}
	if policy.AllowsProvider("") {
		t.Fatalf("expected empty allowlist to deny empty provider")
	}
}

func TestEarlyReflectionPolicyNilNotifyIsSafeForWorkerReflectorFixture(t *testing.T) {
	ctx := ContextWithEarlyReflectionEnabled(context.Background(), EarlyReflectionPolicy{
		AllowedProviders: []provider.ProviderID{provider.ProviderCodex},
	})

	got, ok := EarlyReflectionPolicyFromContext(ctx)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if !got.AllowsProvider(provider.ProviderCodex) {
		t.Fatalf("expected policy to allow codex")
	}
	if got.Notify != nil {
		got.Notify()
	}
}

func TestEarlyReflectionPolicyFrontRunOnlyMarkerFixture(t *testing.T) {
	frontRunCtx := ContextWithEarlyReflectionEnabled(context.Background(), EarlyReflectionPolicy{
		AllowedProviders: []provider.ProviderID{provider.ProviderClaude},
	})
	tailRunCtx := context.Background()
	workerReflectorCtx := context.Background()

	if _, ok := EarlyReflectionPolicyFromContext(frontRunCtx); !ok {
		t.Fatalf("expected front run fixture to carry early reflection marker")
	}
	if _, ok := EarlyReflectionPolicyFromContext(tailRunCtx); ok {
		t.Fatalf("tail run fixture must not carry early reflection marker")
	}
	if _, ok := EarlyReflectionPolicyFromContext(workerReflectorCtx); ok {
		t.Fatalf("worker reflector fixture must not carry early reflection marker")
	}
}
