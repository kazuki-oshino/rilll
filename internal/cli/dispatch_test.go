package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/notify"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflow/builtin"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

// TestMain は cli パッケージのテスト中だけ RILLL_NOTIFY_SOUND を un-set にして
// 実 afplay の起動を抑止する。dispatch.go は spy を持たない直呼び出しのため、
// このパッケージのテストは ENV が立っていると毎回 afplay を起動する。
// go test ./... で並列に走ると複数の afplay が重なって体感音量が大きくなり
// 開発者の作業環境を妨げるため、env を un-set してテスト中だけ完全に無音にする
// (本体経路の -v 0.5 は変更しない)。
func TestMain(m *testing.M) {
	os.Unsetenv(notify.SoundEnvName)
	os.Exit(m.Run())
}

// fakeRun は呼び出された ask を観測しつつ Definition.Fn として動く。
type capture struct {
	called   bool
	cwd      string
	askValue string
	askOK    bool
	askErr   error
}

type orderedCapture struct {
	called    bool
	ids       []string
	askOK     bool
	askErr    error
	continued bool
}

func makeDef(id, label string, args []workflow.ArgSpec, cap *capture, askKey string, items []workflow.SelectItem, terminal func(onEvent func(workflow.WorkflowEvent))) workflow.Definition {
	return workflow.Definition{
		ID:    id,
		Label: label,
		Args:  args,
		Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			cap.called = true
			cap.cwd = cwd
			if askKey != "" && ask != nil {
				value, ok, err := ask.Select(ctx, workflow.SelectParams{Key: askKey, Items: items})
				cap.askValue = value
				cap.askOK = ok
				cap.askErr = err
				if err != nil {
					return err
				}
				if !ok {
					return nil
				}
			}
			if terminal != nil {
				terminal(onEvent)
			}
			return nil
		},
	}
}

func makeOrderedDef(id string, cap *orderedCapture, items []workflow.SelectItem) workflow.Definition {
	return workflow.Definition{
		ID:    id,
		Label: "Ordered",
		Args:  []workflow.ArgSpec{{Key: "features", Description: "ordered features", Required: true}},
		Fn: func(ctx context.Context, _ string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
			cap.called = true
			ids, ok, err := workflow.SelectOrdered(ctx, ask, workflow.OrderedSelectParams{
				Key:             "features",
				Items:           items,
				MinItems:        1,
				DuplicatePolicy: workflow.DuplicatePolicyReject,
			})
			cap.ids = ids
			cap.askOK = ok
			cap.askErr = err
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			cap.continued = true
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowDone, Result: strings.Join(ids, ",")})
			return nil
		},
	}
}

func defaultDeps(defs []workflow.Definition) (Deps, *bytes.Buffer, *bytes.Buffer) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	return Deps{Stdout: stdout, Stderr: stderr, Workflows: defs, CWD: "/tmp/rilll-test"}, stdout, stderr
}

func seedEligibleSpecFeature(t *testing.T, cwd, feature string) {
	t.Helper()
	dir := filepath.Join(cwd, ".kiro", "specs", feature)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "brief.md"), []byte("feature brief\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(brief.md): %v", err)
	}
	specJSON := `{"phase":"initialized"}`
	if err := os.WriteFile(filepath.Join(dir, "spec.json"), []byte(specJSON), 0o644); err != nil {
		t.Fatalf("WriteFile(spec.json): %v", err)
	}
}

func listOutputRow(out, id string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == id {
			return line, true
		}
	}
	return "", false
}

type dispatchRegistryAgent struct {
	calls int
}

func (a *dispatchRegistryAgent) CallAgent(context.Context, string, string, provider.AgentEventCallback, string, *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
	a.calls++
	return provider.AgentResponse{}, nil
}

func (*dispatchRegistryAgent) AkashicRecorder() akashic.Recorder { return nil }

type dispatchSequenceCall struct {
	Prompt string
	Kind   string
}

type dispatchSequenceAgent struct {
	calls []dispatchSequenceCall
}

func (a *dispatchSequenceAgent) CallAgent(_ context.Context, prompt string, cwd string, _ provider.AgentEventCallback, _ string, options *workflowctx.CallAgentOptions) (provider.AgentResponse, error) {
	kind := dispatchSequenceCallKind(prompt)
	a.calls = append(a.calls, dispatchSequenceCall{Prompt: prompt, Kind: kind})
	sessionID := fmt.Sprintf("dispatch-sequence-session-%02d", len(a.calls))

	if kind == "commit" {
		return provider.AgentResponse{Success: true, Content: "commit ok", SessionID: sessionID}, nil
	}
	switch {
	case strings.HasPrefix(prompt, "/kiro-spec-requirements "):
		feature := dispatchPromptFeature(prompt)
		if feature == "" {
			return provider.AgentResponse{}, errors.New("dispatch sequence test could not parse spec-requirements feature")
		}
		if err := writeDispatchGeneratedSpecArtifacts(cwd, feature, "requirements"); err != nil {
			return provider.AgentResponse{}, err
		}
	case strings.HasPrefix(prompt, "/kiro-spec-design "):
		feature := dispatchPromptFeature(prompt)
		if feature == "" {
			return provider.AgentResponse{}, errors.New("dispatch sequence test could not parse spec-design feature")
		}
		if err := writeDispatchGeneratedSpecArtifacts(cwd, feature, "design"); err != nil {
			return provider.AgentResponse{}, err
		}
	case strings.HasPrefix(prompt, "/kiro-spec-tasks "):
		feature := dispatchPromptFeature(prompt)
		if feature == "" {
			return provider.AgentResponse{}, errors.New("dispatch sequence test could not parse spec-tasks feature")
		}
		if err := writeDispatchGeneratedSpecArtifacts(cwd, feature, "tasks"); err != nil {
			return provider.AgentResponse{}, err
		}
	}

	structured := map[string]any{
		"has_inquiry": false,
		"summary":     "ok",
	}
	if dispatchOutputSchemaRequires(options, "verdict") {
		structured["verdict"] = "go"
		structured["has_advisory"] = false
	}
	return provider.AgentResponse{Success: true, StructuredOutput: structured, SessionID: sessionID}, nil
}

func (*dispatchSequenceAgent) AkashicRecorder() akashic.Recorder { return nil }

func dispatchSequenceCallKind(prompt string) string {
	if strings.HasPrefix(prompt, "/git-commit") {
		return "commit"
	}
	return "skill"
}

func dispatchPromptFeature(prompt string) string {
	fields := strings.Fields(prompt)
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
}

func writeDispatchGeneratedSpecArtifacts(cwd, feature, phase string) error {
	specDir := filepath.Join(cwd, ".kiro", "specs", feature)
	files := map[string]string{}
	spec := map[string]any{
		"phase":                    "initialized",
		"ready_for_implementation": false,
		"approvals": map[string]any{
			"requirements": map[string]any{"generated": false, "approved": false},
			"design":       map[string]any{"generated": false, "approved": false},
			"tasks":        map[string]any{"generated": false, "approved": false},
		},
	}
	approvals := spec["approvals"].(map[string]any)
	requirements := approvals["requirements"].(map[string]any)
	design := approvals["design"].(map[string]any)
	tasks := approvals["tasks"].(map[string]any)

	switch phase {
	case "requirements":
		files["requirements.md"] = "# Requirements\n"
		spec["phase"] = "requirements-generated"
		requirements["generated"] = true
	case "design":
		files["requirements.md"] = "# Requirements\n"
		files["design.md"] = "# Design\n"
		spec["phase"] = "design-generated"
		requirements["generated"] = true
		requirements["approved"] = true
		design["generated"] = true
	case "tasks":
		files["requirements.md"] = "# Requirements\n"
		files["design.md"] = "# Design\n"
		files["tasks.md"] = "- [ ] 1. pending\n"
		spec["phase"] = "tasks-generated"
		requirements["generated"] = true
		requirements["approved"] = true
		design["generated"] = true
		design["approved"] = true
		tasks["generated"] = true
	default:
		return fmt.Errorf("unknown generated spec phase %q for %s", phase, feature)
	}

	for name, body := range files {
		if err := os.WriteFile(filepath.Join(specDir, name), []byte(body), 0o644); err != nil {
			return fmt.Errorf("write generated %s for %s: %w", name, feature, err)
		}
	}
	specRaw, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal generated spec.json for %s: %w", feature, err)
	}
	specRaw = append(specRaw, '\n')
	if err := os.WriteFile(filepath.Join(specDir, "spec.json"), specRaw, 0o644); err != nil {
		return fmt.Errorf("write generated spec.json for %s: %w", feature, err)
	}
	return nil
}

func dispatchOutputSchemaRequires(options *workflowctx.CallAgentOptions, name string) bool {
	if options == nil || options.OutputFormat == nil {
		return false
	}
	root, ok := options.OutputFormat.Schema.(map[string]any)
	if !ok {
		return false
	}
	required, ok := root["required"].([]any)
	if !ok {
		return false
	}
	for _, value := range required {
		if s, ok := value.(string); ok && s == name {
			return true
		}
	}
	return false
}

func dispatchFeaturesForCommand(calls []dispatchSequenceCall, command string) []string {
	var features []string
	for _, call := range calls {
		if strings.HasPrefix(call.Prompt, command+" ") {
			features = append(features, dispatchPromptFeature(call.Prompt))
		}
	}
	return features
}

type dispatchSequenceArtifact struct {
	Status           string   `json:"status"`
	SelectedFeatures []string `json:"selectedFeatures"`
}

func readDispatchSequenceArtifact(t *testing.T, cwd string) dispatchSequenceArtifact {
	t.Helper()

	root := filepath.Join(cwd, ".rilll", "kiro-spec-impl-loop-sequence")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", root, err)
	}
	if len(entries) != 1 {
		t.Fatalf("sequence artifact root entries = %d, want 1: %#v", len(entries), entries)
	}
	raw, err := os.ReadFile(filepath.Join(root, entries[0].Name(), "sequence.json"))
	if err != nil {
		t.Fatalf("ReadFile(sequence.json): %v", err)
	}
	var artifact dispatchSequenceArtifact
	if err := json.Unmarshal(raw, &artifact); err != nil {
		t.Fatalf("Unmarshal(sequence.json): %v\n%s", err, string(raw))
	}
	return artifact
}

func assertStringSliceEqual(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s length = %d, want %d: %#v", label, len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s[%d] = %q, want %q; full result %#v", label, i, got[i], want[i], got)
		}
	}
}

func TestIsSubcommand(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"rilll"}, false},
		{[]string{"rilll", "--version"}, false},
		{[]string{"rilll", "list"}, true},
		{[]string{"rilll", "run", "x"}, true},
		{[]string{"rilll", "help"}, true},
		{[]string{"rilll", "-h"}, true},
		{[]string{"rilll", "--help"}, true},
		{[]string{"rilll", "weird"}, false},
		// task 4.7: `rilll akashic` 系も IsSubcommand に追加された (Req 1.6, 6.1, 6.11)。
		// TUI 起動経路 (`rilll` 単独 / `--version`) の非破壊性を上のケースで担保しつつ、
		// 新規 akashic 経路が CLI 経路に流れることをここで確認する。
		{[]string{"rilll", "akashic"}, true},
		{[]string{"rilll", "akashic", "export"}, true},
	}
	for _, c := range cases {
		if got := IsSubcommand(c.args); got != c.want {
			t.Errorf("IsSubcommand(%v) = %v, want %v", c.args, got, c.want)
		}
	}
}

func TestDispatchListPrintsRegistry(t *testing.T) {
	cap := &capture{}
	defs := []workflow.Definition{
		makeDef("git-commit", "git commit", nil, cap, "", nil, nil),
		makeDef("kiro-impl-loop", "Kiro 実装ループ",
			[]workflow.ArgSpec{{Key: "feature", Description: "対象 feature", Required: true}},
			cap, "", nil, nil),
	}
	deps, stdout, _ := defaultDeps(defs)
	if got := Dispatch(context.Background(), []string{"rilll", "list"}, deps); got != ExitOK {
		t.Fatalf("expected exit 0, got %d", got)
	}
	out := stdout.String()
	if !strings.Contains(out, "git-commit") || !strings.Contains(out, "kiro-impl-loop") {
		t.Errorf("expected list to contain workflow ids, got:\n%s", out)
	}
	if !strings.Contains(out, "--feature (必須)") {
		t.Errorf("expected list to expose required feature flag, got:\n%s", out)
	}
	if !strings.Contains(out, "(なし)") {
		t.Errorf("expected (なし) marker for argless workflow, got:\n%s", out)
	}
}

func TestDispatchListIncludesKiroSpecImplLoopFromSharedRegistry(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	agent := &dispatchRegistryAgent{}
	deps, stdout, stderr := defaultDeps(builtin.Registry(agent))
	exit := Dispatch(context.Background(), []string{"rilll", "list"}, deps)
	if exit != ExitOK {
		t.Fatalf("expected exit 0, got %d. stderr=%s", exit, stderr.String())
	}

	row, ok := listOutputRow(stdout.String(), "kiro-spec-impl-loop")
	if !ok {
		t.Fatalf("rilll list output does not include kiro-spec-impl-loop:\n%s", stdout.String())
	}
	if !strings.Contains(row, "Kiro 仕様作成・実装ループ") {
		t.Fatalf("kiro-spec-impl-loop row does not include label: %q", row)
	}
	if !strings.Contains(row, "--feature (必須)") {
		t.Fatalf("kiro-spec-impl-loop row does not expose required feature arg: %q", row)
	}
}

func TestDispatchListIncludesKiroSpecImplLoopSequenceFromSharedRegistry(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	agent := &dispatchRegistryAgent{}
	deps, stdout, stderr := defaultDeps(builtin.Registry(agent))
	exit := Dispatch(context.Background(), []string{"rilll", "list"}, deps)
	if exit != ExitOK {
		t.Fatalf("expected exit 0, got %d. stderr=%s", exit, stderr.String())
	}

	row, ok := listOutputRow(stdout.String(), "kiro-spec-impl-loop-sequence")
	if !ok {
		t.Fatalf("rilll list output does not include kiro-spec-impl-loop-sequence:\n%s", stdout.String())
	}
	if !strings.Contains(row, "Kiro 仕様作成・実装シーケンス") {
		t.Fatalf("sequence row does not include label: %q", row)
	}
	if !strings.Contains(row, "--features (必須)") {
		t.Fatalf("sequence row does not expose required features arg: %q", row)
	}
}

func TestDispatchRunKiroSpecImplLoopSequenceHelpShowsOrderedFeatures(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	agent := &dispatchRegistryAgent{}
	deps, stdout, stderr := defaultDeps(builtin.Registry(agent))
	exit := Dispatch(context.Background(), []string{"rilll", "run", "kiro-spec-impl-loop-sequence", "--help"}, deps)
	if exit != ExitOK {
		t.Fatalf("expected exit 0, got %d. stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}

	help := stdout.String() + stderr.String()
	for _, want := range []string{
		"rilll run kiro-spec-impl-loop-sequence",
		"--features [必須]",
		"comma-separated",
		"順序",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("help output missing %q:\nstdout=%s\nstderr=%s", want, stdout.String(), stderr.String())
		}
	}
}

func TestDispatchRunKiroSpecImplLoopMissingFeatureUsesSharedCLIAskValidation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cwd := t.TempDir()
	seedEligibleSpecFeature(t, cwd, "cli-shared-feature")

	agent := &dispatchRegistryAgent{}
	deps, stdout, stderr := defaultDeps(builtin.Registry(agent))
	deps.CWD = cwd

	exit := Dispatch(context.Background(), []string{"rilll", "run", "kiro-spec-impl-loop"}, deps)
	if exit != ExitUsageError {
		t.Fatalf("expected exit %d (CLIAsk missing feature), got %d. stdout=%s stderr=%s", ExitUsageError, exit, stdout.String(), stderr.String())
	}
	if agent.calls != 0 {
		t.Fatalf("CallAgent calls = %d, want 0 (missing --feature must stop in CLIAsk validation before agent execution)", agent.calls)
	}

	errOut := stderr.String()
	if !strings.Contains(errOut, "--feature が必要です") {
		t.Fatalf("stderr does not include missing --feature CLIAsk error:\n%s", errOut)
	}
	if !strings.Contains(errOut, "cli-shared-feature") {
		t.Fatalf("stderr does not include eligible feature choice:\n%s", errOut)
	}
	if !strings.Contains(errOut, "Hint: rilll run kiro-spec-impl-loop --help") {
		t.Fatalf("stderr does not include workflow-specific help hint:\n%s", errOut)
	}
	if got := strings.TrimSpace(stdout.String()); got != "" {
		t.Fatalf("stdout = %q, want empty on CLI usage error", got)
	}
}

func TestDispatchRunKiroSpecImplLoopSequencePassesOrderedFeaturesThroughSharedRegistry(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cwd := t.TempDir()
	seedEligibleSpecFeature(t, cwd, "alpha")
	seedEligibleSpecFeature(t, cwd, "beta")

	agent := &dispatchSequenceAgent{}
	deps, stdout, stderr := defaultDeps(builtin.Registry(agent))
	deps.CWD = cwd

	exit := Dispatch(context.Background(), []string{"rilll", "run", "kiro-spec-impl-loop-sequence", "--features", "beta,alpha", "--quiet"}, deps)
	if exit != ExitOK {
		t.Fatalf("expected exit 0, got %d. stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "kiro-spec-impl-loop-sequence completed: beta, alpha" {
		t.Fatalf("stdout = %q, want sequence completion result", got)
	}

	assertStringSliceEqual(t, "spec init order", dispatchFeaturesForCommand(agent.calls, "/kiro-spec-init"), []string{"beta", "alpha"})
	assertStringSliceEqual(t, "impl order", dispatchFeaturesForCommand(agent.calls, "/kiro-impl"), []string{"beta", "alpha"})

	artifact := readDispatchSequenceArtifact(t, cwd)
	if artifact.Status != "completed" {
		t.Fatalf("sequence artifact status = %q, want completed", artifact.Status)
	}
	assertStringSliceEqual(t, "artifact selectedFeatures", artifact.SelectedFeatures, []string{"beta", "alpha"})
}

func TestDispatchRunKiroSpecImplLoopSequenceMisuseDoesNotStartAgent(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "missing features",
			args: []string{"rilll", "run", "kiro-spec-impl-loop-sequence"},
			want: []string{"--features", "alpha, beta", "Hint: rilll run kiro-spec-impl-loop-sequence --help"},
		},
		{
			name: "invalid feature",
			args: []string{"rilll", "run", "kiro-spec-impl-loop-sequence", "--features", "alpha,gamma"},
			want: []string{"unknown feature", "gamma", "alpha, beta"},
		},
		{
			name: "duplicate feature",
			args: []string{"rilll", "run", "kiro-spec-impl-loop-sequence", "--features", "alpha,beta,alpha"},
			want: []string{"duplicate feature", "alpha", "alpha, beta"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			cwd := t.TempDir()
			seedEligibleSpecFeature(t, cwd, "alpha")
			seedEligibleSpecFeature(t, cwd, "beta")

			agent := &dispatchSequenceAgent{}
			deps, stdout, stderr := defaultDeps(builtin.Registry(agent))
			deps.CWD = cwd

			exit := Dispatch(context.Background(), tt.args, deps)
			if exit != ExitUsageError {
				t.Fatalf("expected exit %d, got %d. stdout=%s stderr=%s", ExitUsageError, exit, stdout.String(), stderr.String())
			}
			if len(agent.calls) != 0 {
				t.Fatalf("agent calls = %d, want 0 on CLI misuse: %#v", len(agent.calls), agent.calls)
			}
			for _, want := range tt.want {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
				}
			}
			if got := strings.TrimSpace(stdout.String()); got != "" {
				t.Fatalf("stdout = %q, want empty on CLI usage error", got)
			}
			if _, err := os.Stat(filepath.Join(cwd, ".rilll", "kiro-spec-impl-loop-sequence")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("sequence artifact dir should not exist after CLI misuse, stat err=%v", err)
			}
		})
	}
}

func TestDispatchRunPassesFeatureToCLIAsk(t *testing.T) {
	cap := &capture{}
	defs := []workflow.Definition{
		makeDef("kiro-impl-loop", "Kiro 実装ループ",
			[]workflow.ArgSpec{{Key: "feature", Description: "対象 feature", Required: true}},
			cap, "feature",
			[]workflow.SelectItem{{ID: "foo", Label: "foo"}, {ID: "bar", Label: "bar"}},
			func(onEvent func(workflow.WorkflowEvent)) {
				onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowDone, Result: "ok-foo"})
			}),
	}
	deps, stdout, stderr := defaultDeps(defs)
	exit := Dispatch(context.Background(), []string{"rilll", "run", "kiro-impl-loop", "--feature", "foo"}, deps)
	if exit != ExitOK {
		t.Fatalf("expected exit 0, got %d. stderr=%s", exit, stderr.String())
	}
	if !cap.called {
		t.Fatalf("workflow Fn was not called")
	}
	if cap.askValue != "foo" || !cap.askOK {
		t.Fatalf("expected ask value foo/ok=true, got %q ok=%v err=%v", cap.askValue, cap.askOK, cap.askErr)
	}
	if got := strings.TrimSpace(stdout.String()); got != "ok-foo" {
		t.Errorf("expected stdout final result, got %q", got)
	}
}

func TestDispatchRunPassesOrderedFeaturesToCLIAsk(t *testing.T) {
	cap := &orderedCapture{}
	defs := []workflow.Definition{
		makeOrderedDef("sequence", cap, []workflow.SelectItem{{ID: "alpha", Label: "alpha"}, {ID: "beta", Label: "beta"}}),
	}
	deps, stdout, stderr := defaultDeps(defs)
	exit := Dispatch(context.Background(), []string{"rilll", "run", "sequence", "--features", "beta,alpha"}, deps)
	if exit != ExitOK {
		t.Fatalf("expected exit 0, got %d. stderr=%s", exit, stderr.String())
	}
	if !cap.called || !cap.continued {
		t.Fatalf("workflow did not continue after ordered ask: called=%v continued=%v err=%v", cap.called, cap.continued, cap.askErr)
	}
	want := []string{"beta", "alpha"}
	if len(cap.ids) != len(want) {
		t.Fatalf("ordered ids length = %d, want %d: %#v", len(cap.ids), len(want), cap.ids)
	}
	for i := range want {
		if cap.ids[i] != want[i] {
			t.Fatalf("ordered ids[%d] = %q, want %q; full result %#v", i, cap.ids[i], want[i], cap.ids)
		}
	}
	if got := strings.TrimSpace(stdout.String()); got != "beta,alpha" {
		t.Fatalf("stdout = %q, want beta,alpha", got)
	}
}

func TestDispatchRunRejectsRepeatedOrderedFeaturesFlag(t *testing.T) {
	cap := &orderedCapture{}
	defs := []workflow.Definition{
		makeOrderedDef("sequence", cap, []workflow.SelectItem{{ID: "alpha", Label: "alpha"}, {ID: "beta", Label: "beta"}}),
	}
	deps, stdout, stderr := defaultDeps(defs)
	exit := Dispatch(context.Background(), []string{"rilll", "run", "sequence", "--features", "beta", "--features", "alpha"}, deps)
	if exit != ExitUsageError {
		t.Fatalf("expected exit %d, got %d. stderr=%s", ExitUsageError, exit, stderr.String())
	}
	if !cap.called {
		t.Fatal("workflow Fn was not called")
	}
	if cap.continued {
		t.Fatal("workflow continued after repeated --features misuse")
	}
	if cap.askErr == nil {
		t.Fatal("ordered ask error is nil")
	}
	if !strings.Contains(stderr.String(), "--features") || !strings.Contains(stderr.String(), "複数回") {
		t.Fatalf("stderr does not include repeated --features CLIAsk error:\n%s", stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "" {
		t.Fatalf("stdout = %q, want empty on CLI usage error", got)
	}
}

func TestDispatchRunMissingRequiredFlagExitsTwo(t *testing.T) {
	cap := &capture{}
	defs := []workflow.Definition{
		makeDef("kiro-impl-loop", "Kiro 実装ループ",
			[]workflow.ArgSpec{{Key: "feature", Description: "対象 feature", Required: true}},
			cap, "feature",
			[]workflow.SelectItem{{ID: "foo", Label: "foo"}, {ID: "bar", Label: "bar"}},
			nil),
	}
	deps, _, stderr := defaultDeps(defs)
	exit := Dispatch(context.Background(), []string{"rilll", "run", "kiro-impl-loop"}, deps)
	if exit != ExitUsageError {
		t.Fatalf("expected exit %d (CLI 誤用), got %d. stderr=%s", ExitUsageError, exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--feature") {
		t.Errorf("expected stderr to mention --feature, got %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "foo") || !strings.Contains(stderr.String(), "bar") {
		t.Errorf("expected stderr to list available choices, got %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Hint:") {
		t.Errorf("expected stderr to include Hint pointing to --help, got %s", stderr.String())
	}
}

func TestDispatchRunInvalidValueExitsTwo(t *testing.T) {
	cap := &capture{}
	defs := []workflow.Definition{
		makeDef("kiro-impl-loop", "Kiro 実装ループ",
			[]workflow.ArgSpec{{Key: "feature", Description: "対象 feature", Required: true}},
			cap, "feature",
			[]workflow.SelectItem{{ID: "foo", Label: "foo"}, {ID: "bar", Label: "bar"}},
			nil),
	}
	deps, _, stderr := defaultDeps(defs)
	exit := Dispatch(context.Background(), []string{"rilll", "run", "kiro-impl-loop", "--feature", "ghost"}, deps)
	if exit != ExitUsageError {
		t.Fatalf("expected exit 2 for invalid feature value, got %d. stderr=%s", exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "ghost") {
		t.Errorf("expected stderr to echo invalid value, got %s", stderr.String())
	}
}

func TestDispatchRunUnknownIDExitsTwo(t *testing.T) {
	defs := []workflow.Definition{
		makeDef("git-commit", "git commit", nil, &capture{}, "", nil, nil),
	}
	deps, _, stderr := defaultDeps(defs)
	exit := Dispatch(context.Background(), []string{"rilll", "run", "does-not-exist"}, deps)
	if exit != ExitUsageError {
		t.Fatalf("expected exit 2, got %d", exit)
	}
	if !strings.Contains(stderr.String(), "does-not-exist") {
		t.Errorf("expected stderr to mention unknown id, got %s", stderr.String())
	}
}

func TestDispatchRunArglessWorkflow(t *testing.T) {
	cap := &capture{}
	defs := []workflow.Definition{
		makeDef("git-commit", "git commit", nil, cap, "", nil, func(onEvent func(workflow.WorkflowEvent)) {
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowDone, Result: "committed"})
		}),
	}
	deps, stdout, _ := defaultDeps(defs)
	if exit := Dispatch(context.Background(), []string{"rilll", "run", "git-commit"}, deps); exit != ExitOK {
		t.Fatalf("expected exit 0, got %d", exit)
	}
	if !cap.called {
		t.Fatalf("workflow Fn was not called")
	}
	if got := strings.TrimSpace(stdout.String()); got != "committed" {
		t.Errorf("expected stdout=committed, got %q", got)
	}
}

func TestDispatchRunCWDFlag(t *testing.T) {
	cap := &capture{}
	defs := []workflow.Definition{
		makeDef("git-commit", "git commit", nil, cap, "", nil, func(onEvent func(workflow.WorkflowEvent)) {
			onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowDone, Result: "ok"})
		}),
	}
	deps, _, _ := defaultDeps(defs)
	if exit := Dispatch(context.Background(), []string{"rilll", "run", "git-commit", "--cwd", "/some/where"}, deps); exit != ExitOK {
		t.Fatalf("expected exit 0, got %d", exit)
	}
	if cap.cwd != "/some/where" {
		t.Errorf("expected cwd=/some/where, got %q", cap.cwd)
	}
}

func TestDispatchRunSurfacesWorkflowError(t *testing.T) {
	defs := []workflow.Definition{
		{
			ID:    "boom",
			Label: "Boom",
			Fn: func(ctx context.Context, cwd string, onEvent func(workflow.WorkflowEvent), ask workflow.WorkflowAsk) error {
				return errors.New("kaboom")
			},
		},
	}
	deps, _, stderr := defaultDeps(defs)
	if exit := Dispatch(context.Background(), []string{"rilll", "run", "boom"}, deps); exit != ExitRunError {
		t.Fatalf("expected exit 1, got %d", exit)
	}
	if !strings.Contains(stderr.String(), "kaboom") {
		t.Errorf("expected stderr to contain kaboom, got %s", stderr.String())
	}
}

func TestDispatchRunSurfacesWorkflowErrorEvent(t *testing.T) {
	defs := []workflow.Definition{
		{
			ID:    "logical",
			Label: "Logical",
			Fn: func(_ context.Context, _ string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
				onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowError, Message: "logical failure"})
				return nil
			},
		},
	}
	deps, _, stderr := defaultDeps(defs)
	if exit := Dispatch(context.Background(), []string{"rilll", "run", "logical"}, deps); exit != ExitRunError {
		t.Fatalf("expected exit 1, got %d", exit)
	}
	if !strings.Contains(stderr.String(), "logical failure") {
		t.Errorf("expected stderr to contain workflow error message, got %s", stderr.String())
	}
}

func TestDispatchRunQuietSuppressesProgressButKeepsResult(t *testing.T) {
	defs := []workflow.Definition{
		{
			ID:    "noisy",
			Label: "Noisy",
			Fn: func(_ context.Context, _ string, onEvent func(workflow.WorkflowEvent), _ workflow.WorkflowAsk) error {
				onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowText, Text: "progress"})
				onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowDone, Result: "result"})
				return nil
			},
		},
	}
	deps, stdout, stderr := defaultDeps(defs)
	if exit := Dispatch(context.Background(), []string{"rilll", "run", "noisy", "--quiet"}, deps); exit != ExitOK {
		t.Fatalf("expected exit 0, got %d", exit)
	}
	if strings.Contains(stderr.String(), "progress") {
		t.Errorf("expected --quiet to suppress progress in stderr, got %q", stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "result" {
		t.Errorf("expected final result on stdout, got %q", got)
	}
}

func TestDispatchUnknownSubcommandExitsTwo(t *testing.T) {
	deps, _, stderr := defaultDeps(nil)
	if exit := Dispatch(context.Background(), []string{"rilll", "weird"}, deps); exit != ExitUsageError {
		t.Fatalf("expected exit 2, got %d", exit)
	}
	if !strings.Contains(stderr.String(), "未知のサブコマンド") {
		t.Errorf("expected stderr message, got %s", stderr.String())
	}
}

func TestDispatchHelpForWorkflow(t *testing.T) {
	defs := []workflow.Definition{
		makeDef("kiro-impl-loop", "Kiro 実装ループ",
			[]workflow.ArgSpec{{Key: "feature", Description: "対象 feature", Required: true}},
			&capture{}, "", nil, nil),
	}
	deps, stdout, _ := defaultDeps(defs)
	if exit := Dispatch(context.Background(), []string{"rilll", "help", "run", "kiro-impl-loop"}, deps); exit != ExitOK {
		t.Fatalf("expected exit 0, got %d", exit)
	}
	out := stdout.String()
	if !strings.Contains(out, "--feature") || !strings.Contains(out, "対象 feature") {
		t.Errorf("expected workflow help to expose --feature description, got:\n%s", out)
	}
}

func TestDispatchTopLevelHelp(t *testing.T) {
	defs := []workflow.Definition{
		makeDef("git-commit", "git commit", nil, &capture{}, "", nil, nil),
	}
	deps, stdout, _ := defaultDeps(defs)
	if exit := Dispatch(context.Background(), []string{"rilll", "--help"}, deps); exit != ExitOK {
		t.Fatalf("expected exit 0, got %d", exit)
	}
	if !strings.Contains(stdout.String(), "rilll - ワークフローランナー") {
		t.Errorf("expected top-level help, got:\n%s", stdout.String())
	}
}

// TestDispatchListExcludesHiddenWorkflows は Definition.Hidden=true の
// ワークフローが `rilll list` の出力に含まれないことを観測する。
//
// 動機: improve-workflow は handoff 専用 (akashic Run 詳細から `i` で起動)
// で、CLI 一覧表示 / トップレベルヘルプ / `rilll run` Usage hint からは
// 表示されないようにする契約を導入した。
//
// 同時に、非 Hidden の git-commit はそのまま表示されることも観測する。
func TestDispatchListExcludesHiddenWorkflows(t *testing.T) {
	defs := []workflow.Definition{
		makeDef("git-commit", "git commit", nil, &capture{}, "", nil, nil),
		func() workflow.Definition {
			d := makeDef("improve-workflow", "improve workflow", nil, &capture{}, "", nil, nil)
			d.Hidden = true
			return d
		}(),
	}
	deps, stdout, _ := defaultDeps(defs)
	if exit := Dispatch(context.Background(), []string{"rilll", "list"}, deps); exit != ExitOK {
		t.Fatalf("expected exit 0, got %d", exit)
	}
	out := stdout.String()
	if !strings.Contains(out, "git-commit") {
		t.Errorf("非 Hidden の git-commit が list 出力に含まれていない:\n%s", out)
	}
	if strings.Contains(out, "improve-workflow") {
		t.Errorf("Hidden の improve-workflow が list 出力に出ている:\n%s", out)
	}
}

// TestDispatchTopLevelHelpExcludesHiddenWorkflows は printTopLevelHelp の
// 「登録ワークフロー:」セクションが Hidden=true を除外することを観測する。
func TestDispatchTopLevelHelpExcludesHiddenWorkflows(t *testing.T) {
	defs := []workflow.Definition{
		makeDef("git-commit", "git commit", nil, &capture{}, "", nil, nil),
		func() workflow.Definition {
			d := makeDef("improve-workflow", "improve workflow", nil, &capture{}, "", nil, nil)
			d.Hidden = true
			return d
		}(),
	}
	deps, stdout, _ := defaultDeps(defs)
	if exit := Dispatch(context.Background(), []string{"rilll", "--help"}, deps); exit != ExitOK {
		t.Fatalf("expected exit 0, got %d", exit)
	}
	out := stdout.String()
	if !strings.Contains(out, "git-commit") {
		t.Errorf("非 Hidden の git-commit が top-level help に出ていない:\n%s", out)
	}
	if strings.Contains(out, "improve-workflow") {
		t.Errorf("Hidden の improve-workflow が top-level help に出ている:\n%s", out)
	}
}

// TestDispatchRunHiddenWorkflowStillFindable は Hidden=true でも
// `rilll run <id>` の名指し起動は通る (findDefinition は弾かない) ことを観測する。
//
// 動機: Hidden は表示制御 (`rilll list` / `rilll help` / TUI トップメニュー)
// だけに作用する。CLI から名指し起動する経路は閉じない。
// 起動後に precondition_failed 等で fail するのは本ワークフロー本体の責務で、
// Hidden の責務ではないため、Definition.Fn が呼ばれること自体は許容する。
func TestDispatchRunHiddenWorkflowStillFindable(t *testing.T) {
	cap := &capture{}
	defs := []workflow.Definition{
		func() workflow.Definition {
			d := makeDef("improve-workflow", "improve workflow", nil, cap, "", nil,
				func(onEvent func(workflow.WorkflowEvent)) {
					onEvent(workflow.WorkflowEvent{Type: workflow.WorkflowDone, Result: "ok"})
				})
			d.Hidden = true
			return d
		}(),
	}
	deps, _, stderr := defaultDeps(defs)
	exit := Dispatch(context.Background(), []string{"rilll", "run", "improve-workflow"}, deps)
	if exit != ExitOK {
		t.Fatalf("expected exit 0 (Hidden でも findDefinition は通る), got %d. stderr=%s", exit, stderr.String())
	}
	if !cap.called {
		t.Fatal("expected Hidden workflow Fn を name 指定で起動できること")
	}
}
