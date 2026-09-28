package inquiryloop

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionInquiryloopDoesNotImportLegacyPersonaPackage(t *testing.T) {
	const legacyImport = "github.com/kazuki-oshino/rilll/internal/persona"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir internal/inquiryloop: %v", err)
	}

	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("ParseFile %s: %v", name, err)
		}
		for _, imp := range file.Imports {
			if strings.Trim(imp.Path.Value, `"`) == legacyImport {
				pos := fset.Position(imp.Pos())
				t.Fatalf("production inquiryloop must not import legacy persona package: %s", pos)
			}
		}
	}
}

func TestInquiryloopAnswerPersonaFixturesUseFacetSourcePaths(t *testing.T) {
	legacySourcePrefix := "embed:" + "personas/"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir internal/inquiryloop: %v", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}

		content, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		if strings.Contains(string(content), legacySourcePrefix) {
			t.Fatalf("inquiryloop test fixture %s still uses legacy persona source prefix %q; use embed:facets/personas/...", name, legacySourcePrefix)
		}
	}
}
