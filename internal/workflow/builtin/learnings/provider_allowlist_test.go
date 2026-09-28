package learnings

import (
	"os"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

func TestResolveProviderAllowlistUsesExistingEnvSemantics(t *testing.T) {
	tests := []struct {
		name       string
		env        string
		defaultCSV string
		want       []provider.ProviderID
	}{
		{
			name:       "env empty uses default",
			env:        "",
			defaultCSV: "codex",
			want:       []provider.ProviderID{provider.ProviderCodex},
		},
		{
			name:       "env csv trims and dedups",
			env:        " codex , codex , claude , cursor-sdk ",
			defaultCSV: "claude",
			want:       []provider.ProviderID{provider.ProviderCodex, provider.ProviderClaude, provider.ProviderCursorSDK},
		},
		{
			name:       "env all empty falls back to parsed default",
			env:        ", , ",
			defaultCSV: " claude , claude , codex ",
			want:       []provider.ProviderID{provider.ProviderClaude, provider.ProviderCodex},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withProviderEnv(t, tt.env)

			got := resolveProviderAllowlist(envRillLearningsProviders, tt.defaultCSV)
			if !equalProviderIDSlice(got, tt.want) {
				t.Fatalf("resolveProviderAllowlist() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestEarlyReflectionPolicySnapshotUsesResolvedAllowlist(t *testing.T) {
	withProviderEnv(t, " codex , claude , codex ")
	notified := false

	policy := newEarlyReflectionPolicySnapshot(func() {
		notified = true
	})

	wantProviders := []provider.ProviderID{provider.ProviderCodex, provider.ProviderClaude}
	legacyProviders := resolveProviderAllowlist(envRillLearningsProviders, defaultProviderAllowlist)
	if !equalProviderIDSlice(legacyProviders, wantProviders) {
		t.Fatalf("legacy resolver providers = %#v, want %#v", legacyProviders, wantProviders)
	}
	if !equalProviderIDSlice(policy.AllowedProviders, wantProviders) {
		t.Fatalf("policy.AllowedProviders = %#v, want %#v", policy.AllowedProviders, wantProviders)
	}
	if !equalProviderIDSlice(policy.AllowedProviders, legacyProviders) {
		t.Fatalf("policy providers = %#v, want same set as legacy resolver %#v", policy.AllowedProviders, legacyProviders)
	}
	if !policy.AllowsProvider(provider.ProviderCodex) {
		t.Fatalf("expected policy to allow codex")
	}
	if !policy.AllowsProvider(provider.ProviderClaude) {
		t.Fatalf("expected policy to allow claude")
	}
	if policy.AllowsProvider(provider.ProviderCursor) {
		t.Fatalf("expected denied provider to use skip reason %q", SkipReasonProviderNotAllowed)
	}
	if SkipReasonProviderNotAllowed != "provider-not-in-allowlist" {
		t.Fatalf("SkipReasonProviderNotAllowed = %q, want provider-not-in-allowlist", SkipReasonProviderNotAllowed)
	}
	if policy.Notify == nil {
		t.Fatalf("expected notify callback to be preserved")
	}
	policy.Notify()
	if !notified {
		t.Fatalf("expected notify callback to run")
	}
}

func TestEarlyReflectionPolicySnapshotDoesNotReadEnvAfterCreation(t *testing.T) {
	withProviderEnv(t, "codex")

	policy := newEarlyReflectionPolicySnapshot(nil)
	if !policy.AllowsProvider(provider.ProviderCodex) {
		t.Fatalf("expected initial snapshot to allow codex")
	}

	if err := os.Setenv(envRillLearningsProviders, "claude"); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	if !policy.AllowsProvider(provider.ProviderCodex) {
		t.Fatalf("expected existing snapshot to keep allowing codex after env mutation")
	}
	if policy.AllowsProvider(provider.ProviderClaude) {
		t.Fatalf("expected existing snapshot to keep denying claude after env mutation")
	}
}

func equalProviderIDSlice(a, b []provider.ProviderID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
