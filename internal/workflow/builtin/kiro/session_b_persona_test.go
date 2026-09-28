package kiro

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/facet"
)

func TestSessionBPersonaFacetName(t *testing.T) {
	tests := []struct {
		name      string
		wantName  string
		wantFound bool
	}{
		{name: "architecture-quality", wantName: "architecture-quality", wantFound: true},
		{name: implPersonaName, wantName: "impl-direction-answerer", wantFound: true},
		{name: "unknown-persona", wantName: "", wantFound: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotFound := sessionBPersonaFacetName(tt.name)
			if gotName != tt.wantName || gotFound != tt.wantFound {
				t.Fatalf("sessionBPersonaFacetName(%q) = (%q, %v), want (%q, %v)", tt.name, gotName, gotFound, tt.wantName, tt.wantFound)
			}
		})
	}
}

func TestResolveSessionBPersonaBuiltInFallback(t *testing.T) {
	tests := []struct {
		logicalName string
		wantSource  string
		wantMarker  string
	}{
		{
			logicalName: "architecture-quality",
			wantSource:  "embed:facets/personas/architecture-quality.md",
			wantMarker:  "# Persona: Architecture Quality Developer",
		},
		{
			logicalName: implPersonaName,
			wantSource:  "embed:facets/personas/impl-direction-answerer.md",
			wantMarker:  "# Persona: Implementation Direction Setter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.logicalName, func(t *testing.T) {
			got, err := resolveSessionBPersona(t.TempDir(), tt.logicalName)
			if err != nil {
				t.Fatalf("resolveSessionBPersona(%q) error = %v, want nil", tt.logicalName, err)
			}
			if got.Name != tt.logicalName {
				t.Fatalf("Name = %q, want logical name %q", got.Name, tt.logicalName)
			}
			if got.SourcePath != tt.wantSource {
				t.Fatalf("SourcePath = %q, want %q", got.SourcePath, tt.wantSource)
			}
			if !strings.Contains(got.Content, tt.wantMarker) {
				t.Fatalf("Content does not contain marker %q:\n%s", tt.wantMarker, got.Content)
			}
		})
	}
}

func TestResolveSessionBPersonaProjectOverride(t *testing.T) {
	cwd := t.TempDir()
	const overrideBody = "# impl-direction-answerer override\nsession B override content must win.\n"
	const oldPathLoser = "# old impl-direction override\nthis old path must be ignored.\n"

	writeFile(t, filepath.Join(cwd, ".rilll-config", "facets", "personas", "impl-direction-answerer.md"), overrideBody)
	writeFile(t, filepath.Join(cwd, ".kiro", "personas", implPersonaName+".md"), oldPathLoser)

	got, err := resolveSessionBPersona(cwd, implPersonaName)
	if err != nil {
		t.Fatalf("resolveSessionBPersona(%q) error = %v, want nil", implPersonaName, err)
	}
	wantSource := filepath.Join(cwd, ".rilll-config", "facets", "personas", "impl-direction-answerer.md")
	if got.Name != implPersonaName {
		t.Fatalf("Name = %q, want logical name %q", got.Name, implPersonaName)
	}
	if got.SourcePath != wantSource {
		t.Fatalf("SourcePath = %q, want override path %q", got.SourcePath, wantSource)
	}
	if got.Content != overrideBody {
		t.Fatalf("Content = %q, want override body %q", got.Content, overrideBody)
	}
	if strings.Contains(got.Content, oldPathLoser) {
		t.Fatalf("Content includes old .kiro/personas loser marker: %q", got.Content)
	}
}

func TestResolveSessionBPersonaIgnoresOldKiroPersonaPath(t *testing.T) {
	cwd := t.TempDir()
	const oldPathLoser = "# old architecture-quality override\nthis old path must not affect session B persona.\n"
	writeFile(t, filepath.Join(cwd, ".kiro", "personas", "architecture-quality.md"), oldPathLoser)

	got, err := resolveSessionBPersona(cwd, "architecture-quality")
	if err != nil {
		t.Fatalf("resolveSessionBPersona(architecture-quality) error = %v, want nil", err)
	}
	if got.SourcePath != "embed:facets/personas/architecture-quality.md" {
		t.Fatalf("SourcePath = %q, want embedded facet source", got.SourcePath)
	}
	if strings.Contains(got.Content, oldPathLoser) {
		t.Fatalf("Content includes old .kiro/personas loser marker: %q", got.Content)
	}
	if !strings.Contains(got.Content, "# Persona: Architecture Quality Developer") {
		t.Fatalf("Content does not contain embedded architecture-quality marker:\n%s", got.Content)
	}
}

func TestResolveSessionBPersonaEmptyContentAbort(t *testing.T) {
	cwd := t.TempDir()
	writeFile(t, filepath.Join(cwd, ".rilll-config", "facets", "personas", "impl-direction-answerer.md"), "")

	_, err := resolveSessionBPersona(cwd, implPersonaName)
	if !errors.Is(err, errEmptySessionBPersona) {
		t.Fatalf("error = %v, want errors.Is errEmptySessionBPersona", err)
	}

	got := formatSessionBPersonaError(implPersonaName, err)
	want := "session B persona \"impl-direction\" (facet \"impl-direction-answerer\") is empty; expected non-empty content at <cwd>/.rilll-config/facets/personas/impl-direction-answerer.md or embed:facets/personas/impl-direction-answerer.md. Error: session B persona: empty content"
	if got != want {
		t.Fatalf("formatSessionBPersonaError() = %q, want %q", got, want)
	}
	assertNoOldPersonaPath(t, got)
}

func TestResolveAnswerPolicyWhitespaceOnlyOverrideAbort(t *testing.T) {
	cwd := t.TempDir()
	writeFile(t, filepath.Join(cwd, ".rilll-config", "facets", "policies", implAnswerPolicyName+".md"), " \n\t\n")

	_, err := resolveAnswerPolicy(cwd, implAnswerPolicyName)
	if !errors.Is(err, errEmptyAnswerPolicy) {
		t.Fatalf("error = %v, want errors.Is errEmptyAnswerPolicy", err)
	}
}

func TestResolveSessionBPersonaUnknownLogicalName(t *testing.T) {
	_, err := resolveSessionBPersona(t.TempDir(), "unknown-persona")
	if !errors.Is(err, facet.ErrFacetNotFound) {
		t.Fatalf("error = %v, want errors.Is facet.ErrFacetNotFound", err)
	}

	got := formatSessionBPersonaError("unknown-persona", err)
	want := "session B persona \"unknown-persona\" is not supported; expected one of: architecture-quality, impl-direction. Error: session B persona \"unknown-persona\" is not mapped to a Persona facet: facet: not found"
	if got != want {
		t.Fatalf("formatSessionBPersonaError() = %q, want %q", got, want)
	}
	assertNoOldPersonaPath(t, got)
}

func TestFormatSessionBPersonaMappedMissingGuidance(t *testing.T) {
	err := fmt.Errorf("fixture resolver miss: %w", facet.ErrFacetNotFound)

	got := formatSessionBPersonaError(implPersonaName, err)
	want := "session B persona \"impl-direction\" (facet \"impl-direction-answerer\") was not found; expected <cwd>/.rilll-config/facets/personas/impl-direction-answerer.md or embed:facets/personas/impl-direction-answerer.md. Error: fixture resolver miss: facet: not found"
	if got != want {
		t.Fatalf("formatSessionBPersonaError() = %q, want %q", got, want)
	}
	assertNoOldPersonaPath(t, got)
}

func TestResolveSessionBPersonaFatalIOPropagation(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can bypass file mode checks; fatal I/O propagation needs normal permissions")
	}
	cwd := t.TempDir()
	guardedDir := filepath.Join(cwd, ".rilll-config", "facets", "personas", "architecture-quality.md")
	if err := os.MkdirAll(guardedDir, 0o755); err != nil {
		t.Fatalf("MkdirAll guarded override dir: %v", err)
	}

	_, err := resolveSessionBPersona(cwd, "architecture-quality")
	if err == nil {
		t.Fatal("resolveSessionBPersona() error = nil, want fatal override read error")
	}
	if errors.Is(err, facet.ErrFacetNotFound) {
		t.Fatalf("fatal override read error was folded into ErrFacetNotFound: %v", err)
	}
	if errors.Is(err, errEmptySessionBPersona) {
		t.Fatalf("fatal override read error was folded into errEmptySessionBPersona: %v", err)
	}

	msg := formatSessionBPersonaError("architecture-quality", err)
	if !strings.Contains(msg, "session B persona \"architecture-quality\" (facet \"architecture-quality\") resolution failed") {
		t.Fatalf("formatted fatal error lacks resolution failed prefix: %q", msg)
	}
	if !strings.Contains(msg, ".rilll-config/facets/personas/architecture-quality.md") {
		t.Fatalf("formatted fatal error lacks facet override guidance: %q", msg)
	}
	assertNoOldPersonaPath(t, msg)
}

func writeFile(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func assertNoOldPersonaPath(t *testing.T, s string) {
	t.Helper()
	if strings.Contains(s, ".kiro/personas") {
		t.Fatalf("message contains old persona path guidance: %q", s)
	}
}
