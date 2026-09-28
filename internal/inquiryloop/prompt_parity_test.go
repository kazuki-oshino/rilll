package inquiryloop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/facet"
)

func TestBuildAnswerPromptSessionBPersonaFacetParity(t *testing.T) {
	tests := []struct {
		logicalName   string
		facetName     string
		legacyFixture string
	}{
		{
			logicalName:   "architecture-quality",
			facetName:     "architecture-quality",
			legacyFixture: filepath.Join("..", "facet", "testdata", "legacy-personas", "architecture-quality.md"),
		},
		{
			logicalName:   "impl-direction",
			facetName:     "impl-direction-answerer",
			legacyFixture: filepath.Join("..", "facet", "testdata", "legacy-personas", "impl-direction.md"),
		},
	}

	ctx := AnswerContext{
		PromptCommand: "/kiro-impl legacy-persona-facet-migration 2.2",
		SpecPaths: []string{
			"/repo/.kiro/specs/legacy-persona-facet-migration/requirements.md",
			"/repo/.kiro/specs/legacy-persona-facet-migration/design.md",
			"/repo/.kiro/specs/legacy-persona-facet-migration/tasks.md",
		},
		Background: "Prompt parity regression gate for session B persona migration.",
	}
	inquiry := Inquiry{
		Title:      "Persona parity decision",
		Background: "session B persona migrated from legacy path to facet path",
		Question:   "Does the answer prompt remain byte-for-byte identical?",
		Options: []InquiryOption{
			{Label: "yes", Description: "whole prompt parity is preserved"},
			{Label: "no", Description: "prompt structure or visible persona changed"},
		},
	}
	history := []Round{
		{
			Index:      1,
			Inquiry:    Inquiry{Title: "previous", Question: "previous question"},
			AnswerText: "previous answer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.logicalName, func(t *testing.T) {
			legacyContent := readLegacyPersonaFixture(t, tt.legacyFixture)
			migrated, err := facet.ResolveFacet(t.TempDir(), facet.Persona, tt.facetName)
			if err != nil {
				t.Fatalf("ResolveFacet(Persona, %q): %v", tt.facetName, err)
			}

			legacyPrompt := buildAnswerPrompt(AnswerPersona{
				Name:       tt.logicalName,
				SourcePath: tt.legacyFixture,
				Content:    legacyContent,
			}, ctx, inquiry, history)
			migratedPrompt := buildAnswerPrompt(AnswerPersona{
				Name:       "METADATA_NAME_MARKER_SHOULD_NOT_APPEAR_" + tt.logicalName,
				SourcePath: "METADATA_SOURCE_MARKER_SHOULD_NOT_APPEAR_" + migrated.SourcePath,
				Content:    migrated.Content,
			}, ctx, inquiry, history)

			if personaSection := extractPersonaSection(t, migratedPrompt); personaSection != legacyContent {
				t.Fatalf("migrated # Persona section content does not match legacy fixture exactly.\nGot:\n%s\nWant:\n%s", personaSection, legacyContent)
			}
			if migratedPrompt != legacyPrompt {
				t.Fatalf("migrated answer prompt must match legacy answer prompt byte-for-byte.\nGot:\n%s\nWant:\n%s", migratedPrompt, legacyPrompt)
			}
			for _, marker := range []string{
				"METADATA_NAME_MARKER_SHOULD_NOT_APPEAR",
				"METADATA_SOURCE_MARKER_SHOULD_NOT_APPEAR",
				migrated.SourcePath,
			} {
				if strings.Contains(migratedPrompt, marker) {
					t.Fatalf("answer prompt leaked persona metadata marker %q.\nPrompt:\n%s", marker, migratedPrompt)
				}
			}
		})
	}
}

func readLegacyPersonaFixture(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if len(content) == 0 {
		t.Fatalf("legacy persona fixture %s is empty", path)
	}
	return string(content)
}

func extractPersonaSection(t *testing.T, prompt string) string {
	t.Helper()
	const heading = "# Persona\n"
	if !strings.HasPrefix(prompt, heading) {
		t.Fatalf("answer prompt does not start with %q:\n%s", heading, prompt)
	}
	body := strings.TrimPrefix(prompt, heading)
	for _, nextHeading := range []string{"\n# Context\n", "\n# History\n", "\n# Inquiry\n"} {
		if idx := strings.Index(body, nextHeading); idx >= 0 {
			return body[:idx]
		}
	}
	t.Fatalf("answer prompt missing section after # Persona:\n%s", prompt)
	return ""
}
