package facet

import (
	"bytes"
	"io/fs"
	"os"
	"testing"
)

func TestSessionBPersonaFacetAssetsMatchLegacyFixtures(t *testing.T) {
	tests := []struct {
		name        string
		facetPath   string
		fixturePath string
	}{
		{
			name:        "architecture-quality",
			facetPath:   "facets/personas/architecture-quality.md",
			fixturePath: "testdata/legacy-personas/architecture-quality.md",
		},
		{
			name:        "impl-direction",
			facetPath:   "facets/personas/impl-direction-answerer.md",
			fixturePath: "testdata/legacy-personas/impl-direction.md",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facetContent, err := fs.ReadFile(embeddedFS, tt.facetPath)
			if err != nil {
				t.Fatalf("read facet asset %s: %v", tt.facetPath, err)
			}
			if len(facetContent) == 0 {
				t.Fatalf("facet asset %s is empty", tt.facetPath)
			}

			fixtureContent, err := os.ReadFile(tt.fixturePath)
			if err != nil {
				t.Fatalf("read legacy fixture %s: %v", tt.fixturePath, err)
			}
			if len(fixtureContent) == 0 {
				t.Fatalf("legacy fixture %s is empty", tt.fixturePath)
			}

			if !bytes.Equal(facetContent, fixtureContent) {
				t.Fatalf("facet asset %s does not match legacy fixture %s byte-for-byte", tt.facetPath, tt.fixturePath)
			}
		})
	}
}
