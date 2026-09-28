package workflow

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"strings"
	"testing"
)

func TestSequentialIOGeneratedDefinitionUsesNonGenericRuntimeSurface(t *testing.T) {
	var runtimeDefinitions []Definition
	var activeLabels []string

	generated := BuildSequentialIO(DefinitionConfig{
		ID:     "runtime-surface-sequential-io",
		Label:  "Runtime Surface Sequential IO",
		Args:   []ArgSpec{{Key: "feature", Description: "feature name", Required: true}},
		Hidden: true,
	}, Start(Step[NoInput, NoOutput]{
		Definition: Definition{
			ID:    "runtime-surface-child",
			Label: "Runtime Surface Child",
			Fn: func(context.Context, string, func(WorkflowEvent), WorkflowAsk) error {
				return nil
			},
		},
	}))
	runtimeDefinitions = append(runtimeDefinitions, generated)

	got := runtimeDefinitions[0]
	if got.ID != "runtime-surface-sequential-io" {
		t.Fatalf("generated Definition ID = %q, want runtime-surface-sequential-io", got.ID)
	}
	if got.Label != "Runtime Surface Sequential IO" {
		t.Fatalf("generated Definition Label = %q, want Runtime Surface Sequential IO", got.Label)
	}
	if len(got.Args) != 1 || got.Args[0].Key != "feature" || !got.Args[0].Required {
		t.Fatalf("generated Definition Args = %+v, want feature arg preserved", got.Args)
	}
	if !got.Hidden {
		t.Fatalf("generated Definition Hidden = false, want true")
	}
	if got.Fn == nil {
		t.Fatalf("generated Definition Fn is nil")
	}

	err := got.Run(context.Background(), t.TempDir(), func(event WorkflowEvent) {
		if event.Type == WorkflowActive {
			activeLabels = append(activeLabels, event.Label)
		}
	}, nil)
	if err != nil {
		t.Fatalf("generated Definition Run error = %v, want nil", err)
	}

	wantActiveLabels := []string{"Runtime Surface Child"}
	if strings.Join(activeLabels, "\n") != strings.Join(wantActiveLabels, "\n") {
		t.Fatalf("active labels = %v, want %v", activeLabels, wantActiveLabels)
	}
}

func TestSequentialIOPackageGraphExcludesDomainAndRuntimeSurfaces(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "github.com/kazuki-oshino/rilll/internal/workflow")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps internal/workflow: %v\n%s", err, string(output))
	}

	forbiddenPrefixes := []string{
		"github.com/kazuki-oshino/rilll/internal/workflow/builtin",
		"github.com/kazuki-oshino/rilll/internal/bridge",
		"github.com/kazuki-oshino/rilll/internal/cli",
		"github.com/kazuki-oshino/rilll/internal/ui",
	}
	for _, pkg := range strings.Fields(string(output)) {
		for _, forbiddenPrefix := range forbiddenPrefixes {
			if pkg == forbiddenPrefix || strings.HasPrefix(pkg, forbiddenPrefix+"/") {
				t.Fatalf("internal/workflow import graph includes forbidden runtime surface package %q", pkg)
			}
		}
	}
}

func TestSequentialIOImplementationDoesNotReuseHandoffParams(t *testing.T) {
	sourceFiles := []string{
		"definition_run.go",
		"child_result.go",
		"child_runner.go",
		"sequential_io.go",
	}
	forbiddenIdentifiers := map[string]struct{}{
		"HandoffParams":      {},
		"ContextWithHandoff": {},
		"HandoffFromContext": {},
	}

	for _, sourceFile := range sourceFiles {
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, sourceFile, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", sourceFile, err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			ident, ok := node.(*ast.Ident)
			if !ok {
				return true
			}
			if _, forbidden := forbiddenIdentifiers[ident.Name]; forbidden {
				position := fileSet.Position(ident.Pos())
				t.Fatalf("%s uses %s; sequential child hand-off must not reuse workflow.HandoffParams", position, ident.Name)
			}
			return true
		})
	}
}
