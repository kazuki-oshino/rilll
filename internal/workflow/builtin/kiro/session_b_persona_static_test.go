package kiro

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestProductionCodeDoesNotReferenceLegacyPersonaPaths(t *testing.T) {
	repoRoot := filepath.Clean(filepath.Join("..", "..", "..", ".."))
	internalRoot := filepath.Join(repoRoot, "internal")

	fset := token.NewFileSet()
	var violations []string

	err := filepath.WalkDir(internalRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}

		for _, imp := range file.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if importPath == "github.com/kazuki-oshino/rilll/internal/persona" {
				violations = append(violations, formatLegacyPersonaViolation(fset, imp.Pos(), "legacy persona import"))
			}
		}

		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.SelectorExpr:
				if ident, ok := n.X.(*ast.Ident); ok && ident.Name == "persona" && n.Sel.Name == "NewLoader" {
					violations = append(violations, formatLegacyPersonaViolation(fset, n.Pos(), "legacy persona loader selector"))
				}
			case *ast.BasicLit:
				if n.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(n.Value)
				if err != nil {
					return true
				}
				switch {
				case strings.Contains(value, "github.com/kazuki-oshino/rilll/internal/persona"):
					violations = append(violations, formatLegacyPersonaViolation(fset, n.Pos(), "legacy persona import path literal"))
				case strings.Contains(value, ".kiro/personas"):
					violations = append(violations, formatLegacyPersonaViolation(fset, n.Pos(), "legacy persona override path literal"))
				case strings.Contains(value, "embed:personas/"):
					violations = append(violations, formatLegacyPersonaViolation(fset, n.Pos(), "legacy persona embed path literal"))
				}
			}
			return true
		})

		return nil
	})
	if err != nil {
		t.Fatalf("scan production Go files for legacy session B persona references: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("production Go AST still references legacy session B persona paths:\n%s", strings.Join(violations, "\n"))
	}
}

func formatLegacyPersonaViolation(fset *token.FileSet, pos token.Pos, reason string) string {
	return fset.Position(pos).String() + ": " + reason
}
