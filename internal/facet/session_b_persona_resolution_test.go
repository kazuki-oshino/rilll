package facet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveFacetSessionBPersonaBuiltins(t *testing.T) {
	tests := []struct {
		name       string
		wantSource string
		wantMarker string
	}{
		{
			name:       "architecture-quality",
			wantSource: "embed:facets/personas/architecture-quality.md",
			wantMarker: "# Persona: Architecture Quality Developer",
		},
		{
			name:       "impl-direction-answerer",
			wantSource: "embed:facets/personas/impl-direction-answerer.md",
			wantMarker: "# Persona: Implementation Direction Setter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cwd := t.TempDir()

			got, err := ResolveFacet(cwd, Persona, tt.name)
			if err != nil {
				t.Fatalf("ResolveFacet(Persona, %q) error = %v, want nil", tt.name, err)
			}
			if got.Content == "" {
				t.Fatalf("ResolveFacet(Persona, %q): Content is empty, want non-empty embed content", tt.name)
			}
			if got.SourcePath != tt.wantSource {
				t.Fatalf("SourcePath = %q, want %q", got.SourcePath, tt.wantSource)
			}
			if got.Kind != Persona {
				t.Fatalf("Kind = %v, want %v", got.Kind, Persona)
			}
			if got.Name != tt.name {
				t.Fatalf("Name = %q, want %q", got.Name, tt.name)
			}
			if !strings.Contains(got.Content, tt.wantMarker) {
				t.Fatalf("Content does not contain marker %q; session B persona builtin may be missing or swapped:\n%s", tt.wantMarker, got.Content)
			}
		})
	}
}

func TestResolveFacetSessionBPersonaOverridePrecedence(t *testing.T) {
	tests := []struct {
		name            string
		embedOnlyMarker string
		overrideBody    string
	}{
		{
			name:            "architecture-quality",
			embedOnlyMarker: "# Persona: Architecture Quality Developer",
			overrideBody:    "# project override for personas/architecture-quality\nthis content must win over the embedded session B persona.\n",
		},
		{
			name:            "impl-direction-answerer",
			embedOnlyMarker: "# Persona: Implementation Direction Setter",
			overrideBody:    "# project override for personas/impl-direction-answerer\nthis content must win over the embedded session B persona.\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cwd := t.TempDir()
			overrideDir := filepath.Join(cwd, ".rilll-config", "facets", "personas")
			if err := os.MkdirAll(overrideDir, 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			overridePath := filepath.Join(overrideDir, tt.name+".md")
			if err := os.WriteFile(overridePath, []byte(tt.overrideBody), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			got, err := ResolveFacet(cwd, Persona, tt.name)
			if err != nil {
				t.Fatalf("ResolveFacet(Persona, %q) error = %v, want nil", tt.name, err)
			}
			if got.Content != tt.overrideBody {
				t.Fatalf("Content = %q, want %q (project override should win over embedded session B persona)", got.Content, tt.overrideBody)
			}
			if got.SourcePath != overridePath {
				t.Fatalf("SourcePath = %q, want %q (override resolution should report the concrete project file)", got.SourcePath, overridePath)
			}
			if !filepath.IsAbs(got.SourcePath) {
				t.Fatalf("SourcePath %q is not absolute", got.SourcePath)
			}
			if got.Kind != Persona {
				t.Fatalf("Kind = %v, want %v", got.Kind, Persona)
			}
			if got.Name != tt.name {
				t.Fatalf("Name = %q, want %q", got.Name, tt.name)
			}
			if strings.Contains(got.Content, tt.embedOnlyMarker) {
				t.Fatalf("Content contains embedded-only marker %q; override content should not be mixed with embedded content:\n%s", tt.embedOnlyMarker, got.Content)
			}
		})
	}
}
