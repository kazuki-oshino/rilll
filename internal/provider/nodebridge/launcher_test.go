package nodebridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kazuki-oshino/rilll/internal/bridge"
	"github.com/kazuki-oshino/rilll/internal/provider"
)

func Test_resolveStdoutDumpPath(t *testing.T) {
	got := resolveStdoutDumpPath("cursor-sdk", "req-a", "enable", 12345)
	want := filepath.Join(os.TempDir(), "rilll-bridge-cursor-sdk-12345-req-a.log")
	if got != want {
		t.Fatalf("default dump path = %q, want %q", got, want)
	}

	for _, value := range []string{"", "disable", "true", "typo"} {
		if got := resolveStdoutDumpPath("cursor-sdk", "req-a", value, 12345); got != "" {
			t.Fatalf("dump must require explicit opt-in: value=%q path=%q", value, got)
		}
	}

	providerPath := resolveStdoutDumpPath("claude", "req-a", "enable", 12345)
	pidPath := resolveStdoutDumpPath("cursor-sdk", "req-a", "enable", 54321)
	runPath := resolveStdoutDumpPath("cursor-sdk", "req-b", "enable", 12345)
	if providerPath == got || pidPath == got || runPath == got || providerPath == pidPath || providerPath == runPath || pidPath == runPath {
		t.Fatalf("dump paths should differ by provider, pid, and run id: got=%q providerPath=%q pidPath=%q runPath=%q", got, providerPath, pidPath, runPath)
	}

	// Issue #64 D1: claude / codex / cursor の 3 provider が同一 pid / run-id の
	// dump path を要求しても衝突しないことを確認する (per-provider 命名の最小契約)。
	codexPath := resolveStdoutDumpPath("codex", "req-a", "enable", 12345)
	cursorPath := resolveStdoutDumpPath("cursor-sdk", "req-a", "enable", 12345)
	claudePath := resolveStdoutDumpPath("claude", "req-a", "enable", 12345)
	if codexPath == cursorPath || codexPath == claudePath || cursorPath == claudePath {
		t.Fatalf("3-provider dump paths should be distinct: codex=%q cursor=%q claude=%q", codexPath, cursorPath, claudePath)
	}
}

func Test_Run_StdoutDumpDisabled(t *testing.T) {
	t.Setenv("RILLL_BRIDGE_STDOUT_DUMP", "")

	cleanProviderDumpFiles(t, provider.ProviderCursorSDK)
	legacyDumpPath := "/tmp/rilll-bridge-stdout-dump.log"
	_ = os.Remove(legacyDumpPath)

	bridgeScript := writeTestBridgeScript(t, "disabled-ok")
	resp, err := runTestBridge(t, provider.ProviderCursorSDK, bridgeScript)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !resp.Success || resp.Content != "disabled-ok" {
		t.Fatalf("Run should continue normally with dump disabled, got %#v", resp)
	}
	if files := dumpFilesForProvider(t, provider.ProviderCursorSDK); len(files) != 0 {
		t.Fatalf("disabled dump path should not exist, got files=%v", files)
	}
	if _, err := os.Stat(legacyDumpPath); !os.IsNotExist(err) {
		t.Fatalf("legacy fixed dump path should not be created when disabled: path=%q err=%v", legacyDumpPath, err)
	}
}

func Test_Run_StdoutDumpParallel(t *testing.T) {
	t.Setenv("RILLL_BRIDGE_STDOUT_DUMP", "enable")

	// Issue #64 D1: cursor / claude / codex の 3 provider 並走時に
	// dump ファイル名が衝突しないことを確認する (per-provider/pid/run-id 命名)。
	cases := []struct {
		providerID provider.ProviderID
		result     string
		script     string
	}{
		{providerID: provider.ProviderCursorSDK, result: "cursor-stdout"},
		{providerID: provider.ProviderClaude, result: "claude-stdout"},
		{providerID: provider.ProviderCodex, result: "codex-stdout"},
	}

	for i, tc := range cases {
		cases[i].script = writeTestBridgeScript(t, tc.result)
		cleanProviderDumpFiles(t, tc.providerID)
	}
	_ = os.Remove("/tmp/rilll-bridge-stdout-dump.log")

	var wg sync.WaitGroup
	errs := make(chan error, len(cases))
	for _, tc := range cases {
		tc := tc
		wg.Add(1)
		go func() {
			defer wg.Done()

			resp, err := runTestBridge(t, tc.providerID, tc.script)
			if err != nil {
				errs <- err
				return
			}
			if !resp.Success || resp.Content != tc.result {
				errs <- fmt.Errorf("unexpected bridge response for %s: %#v", tc.providerID, resp)
				return
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range cases {
		files := dumpFilesForProvider(t, tc.providerID)
		if len(files) != 1 {
			t.Fatalf("expected one dump file for %s, got %v", tc.providerID, files)
		}
		dumpPath := files[0]
		info, err := os.Stat(dumpPath)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("dump must be owner-only: info=%v err=%v", info, err)
		}
		contents, err := os.ReadFile(dumpPath)
		if err != nil {
			t.Fatalf("dump file should exist for %s at %q: %v", tc.providerID, dumpPath, err)
		}
		if !strings.Contains(string(contents), tc.result) {
			t.Fatalf("dump file %q should contain %q, got %q", dumpPath, tc.result, string(contents))
		}
	}
}

func Test_Run_StdoutDumpParallelSameProvider(t *testing.T) {
	t.Setenv("RILLL_BRIDGE_STDOUT_DUMP", "enable")
	cleanProviderDumpFiles(t, provider.ProviderCursorSDK)

	results := []string{"cursor-stdout-a", "cursor-stdout-b"}
	scripts := make([]string, 0, len(results))
	for _, result := range results {
		scripts = append(scripts, writeTestBridgeScript(t, result))
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(scripts))
	for i, script := range scripts {
		i, script := i, script
		wg.Add(1)
		go func() {
			defer wg.Done()

			resp, err := runTestBridge(t, provider.ProviderCursorSDK, script)
			if err != nil {
				errs <- err
				return
			}
			if !resp.Success || resp.Content != results[i] {
				errs <- fmt.Errorf("unexpected bridge response for run %d: %#v", i, resp)
				return
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	files := dumpFilesForProvider(t, provider.ProviderCursorSDK)
	if len(files) != len(results) {
		t.Fatalf("expected one dump file per same-provider run, got files=%v", files)
	}

	seen := map[string]bool{}
	for _, dumpPath := range files {
		contents, err := os.ReadFile(dumpPath)
		if err != nil {
			t.Fatalf("dump file should be readable at %q: %v", dumpPath, err)
		}
		for _, result := range results {
			if strings.Contains(string(contents), result) {
				seen[result] = true
			}
		}
	}
	for _, result := range results {
		if !seen[result] {
			t.Fatalf("expected dump files to contain %q, got seen=%v files=%v", result, seen, files)
		}
	}
}

func writeTestBridgeScript(t *testing.T, result string) string {
	t.Helper()

	scriptPath := filepath.Join(t.TempDir(), "bridge.mjs")
	source := `
let emitted = false;
process.stdin.on('data', () => {
  if (emitted) {
    return;
  }
  emitted = true;
  const result = ` + strconv.Quote(result) + `;
  process.stdout.write('{"type":"ready"}\n');
  process.stdout.write(JSON.stringify({ type: "done", id: "req-test", result, sessionId: "session-test" }) + '\n');
});
`
	if err := os.WriteFile(scriptPath, []byte(source), 0o600); err != nil {
		t.Fatalf("write test bridge script: %v", err)
	}
	return scriptPath
}

func runTestBridge(t *testing.T, providerID provider.ProviderID, bridgeScript string) (provider.AgentResponse, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return Run(ctx, RunOptions{
		Config: Config{
			ProviderID:        providerID,
			DefaultBridgePath: bridgeScript,
		},
		BridgeScript: bridgeScript,
	}, provider.AgentRequest{CWD: t.TempDir()}, nil)
}

func TestBuildRunRequestMapsTraceContext(t *testing.T) {
	req := BuildRunRequest("req-1", provider.AgentRequest{
		Prompt: "hello",
		CWD:    "/repo",
		TraceContext: provider.TraceContext{
			TraceParent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			TraceState:  "vendor=value",
			Baggage:     "project=rilll",
		},
	})

	if req.TraceContext.TraceParent == "" {
		t.Fatalf("trace context was not mapped: %#v", req.TraceContext)
	}
	if req.TraceContext.TraceState != "vendor=value" || req.TraceContext.Baggage != "project=rilll" {
		t.Fatalf("trace context mismatch: %#v", req.TraceContext)
	}
}

func TestBuildRunRequestMapsReasoningSummary(t *testing.T) {
	req := BuildRunRequest("req-1", provider.AgentRequest{
		Prompt:           "hello",
		CWD:              "/repo",
		ReasoningSummary: "detailed",
	})

	if req.ReasoningSummary != "detailed" {
		t.Fatalf("reasoning summary was not mapped: %#v", req)
	}
}

func dumpFilesForProvider(t *testing.T, providerID provider.ProviderID) []string {
	t.Helper()

	pattern := filepath.Join(os.TempDir(), fmt.Sprintf("rilll-bridge-%s-%d-*.log", providerID, os.Getpid()))
	files, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("invalid dump file glob pattern %q: %v", pattern, err)
	}
	return files
}

func cleanProviderDumpFiles(t *testing.T, providerID provider.ProviderID) {
	t.Helper()

	for _, path := range dumpFilesForProvider(t, providerID) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove stale dump file %q: %v", path, err)
		}
	}
	t.Cleanup(func() {
		for _, path := range dumpFilesForProvider(t, providerID) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatalf("remove dump file %q: %v", path, err)
			}
		}
	})
}

func TestBuildRunRequestMapsAllFields(t *testing.T) {
	req := provider.AgentRequest{
		Prompt:          "hello",
		CWD:             "/repo",
		ResumeSessionID: "session-1",
		OutputFormat:    &provider.OutputFormat{Type: "text", Schema: map[string]any{"foo": "bar"}},
		AkashicLevel:    "debug",
		Metadata: provider.AgentMetadata{
			WorkflowRunID: "workflow-run-1",
			WorkflowID:    "wf",
			WorkflowName:  "name",
			StepID:        "step",
			StepName:      "step-name",
			AgentCallID:   "agent-call-1",
		},
		Model: "claude-opus-4-1-20250805",
	}

	got := BuildRunRequest("req-1", req)
	if got.Type != bridge.RequestRun || got.ID != "req-1" {
		t.Fatalf("type/id mismatch: %#v", got)
	}
	if got.Prompt != "hello" || got.CWD != "/repo" || got.ResumeSessionID != "session-1" || got.AkashicLevel != "debug" {
		t.Fatalf("scalar fields mismatch: %#v", got)
	}
	if got.OutputFormat == nil || got.OutputFormat.Type != "text" || got.OutputFormat.Schema == nil {
		t.Fatalf("output format not preserved: %#v", got.OutputFormat)
	}
	if got.Metadata.WorkflowRunID != "workflow-run-1" || got.Metadata.AgentCallID != "agent-call-1" {
		t.Fatalf("metadata not preserved: %#v", got.Metadata)
	}
	if got.Model != "claude-opus-4-1-20250805" {
		t.Fatalf("model field not preserved: %#v", got)
	}
}

func TestBuildRunRequestForwardsModelParams(t *testing.T) {
	got := BuildRunRequest("req-1", provider.AgentRequest{
		Prompt: "hi",
		CWD:    "/repo",
		ModelParams: []provider.ModelParam{
			{ID: "reasoning", Value: "max"},
		},
	})

	if len(got.ModelParams) != 1 || got.ModelParams[0] != (bridge.ModelParam{ID: "reasoning", Value: "max"}) {
		t.Fatalf("model params were not forwarded: %#v", got)
	}
	payload, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if !bytes.Contains(payload, []byte(`"modelParams":[{"id":"reasoning","value":"max"}]`)) {
		t.Fatalf("model params were not preserved on the wire: %s", payload)
	}
}

// TestBuildRunRequestOmitsZeroValueModel は AgentRequest.Model が空文字列のとき bridge.Request.Model も
// 空のまま (provider 既定モデルを維持) であることを確認する (review-workflow Task 1.3)。
func TestBuildRunRequestOmitsZeroValueModel(t *testing.T) {
	req := provider.AgentRequest{Prompt: "hi", CWD: "/repo"}
	got := BuildRunRequest("req-1", req)
	if got.Model != "" {
		t.Fatalf("zero-value Model should remain empty in bridge.Request, got %q", got.Model)
	}
}

// TestBuildRunRequestForwardsAllowedTools は AgentRequest.AllowedTools が
// bridge.Request.AllowedTools に素通しされることを検証する (Phase C)。
func TestBuildRunRequestForwardsAllowedTools(t *testing.T) {
	req := provider.AgentRequest{
		Prompt:       "hi",
		CWD:          "/repo",
		AllowedTools: []string{"Read", "Glob", "Grep"},
	}
	got := BuildRunRequest("req-1", req)
	if len(got.AllowedTools) != 3 {
		t.Fatalf("AllowedTools length: got=%d want=3 (req=%#v)", len(got.AllowedTools), got)
	}
	want := []string{"Read", "Glob", "Grep"}
	for i, w := range want {
		if got.AllowedTools[i] != w {
			t.Fatalf("AllowedTools[%d]=%q want %q", i, got.AllowedTools[i], w)
		}
	}
}

// TestBuildRunRequestOmitsNilAllowedTools は AllowedTools が nil のときに bridge.Request 側も nil で渡され、
// SDK 既定 (= 全ツール許可) を維持できることを検証する後方互換ガード。
func TestBuildRunRequestOmitsNilAllowedTools(t *testing.T) {
	req := provider.AgentRequest{Prompt: "hi", CWD: "/repo"}
	got := BuildRunRequest("req-1", req)
	if got.AllowedTools != nil {
		t.Fatalf("AllowedTools should remain nil when not specified, got %#v", got.AllowedTools)
	}
}

// TestBuildRunRequestForwardsUserHooks は AgentRequest.UserHooks が
// bridge.Request.UserHooks に Matcher / Command まで保たれて素通しされることを検証する (issue #47)。
func TestBuildRunRequestForwardsUserHooks(t *testing.T) {
	req := provider.AgentRequest{
		Prompt: "hi",
		CWD:    "/repo",
		UserHooks: []provider.UserHook{
			{Matcher: "Edit", Command: []string{"bash", "-lc", "/tmp/verify-gate.sh"}},
			{Matcher: "Read", Command: []string{"bash", "-lc", "/tmp/track-read.sh"}},
		},
	}
	got := BuildRunRequest("req-1", req)
	if len(got.UserHooks) != 2 {
		t.Fatalf("UserHooks length: got=%d want=2", len(got.UserHooks))
	}
	if got.UserHooks[0].Matcher != "Edit" || got.UserHooks[1].Matcher != "Read" {
		t.Fatalf("UserHooks Matcher mismatch: %#v", got.UserHooks)
	}
	wantCmds := [][]string{
		{"bash", "-lc", "/tmp/verify-gate.sh"},
		{"bash", "-lc", "/tmp/track-read.sh"},
	}
	for i, want := range wantCmds {
		if len(got.UserHooks[i].Command) != len(want) {
			t.Fatalf("UserHooks[%d].Command length mismatch: %#v", i, got.UserHooks[i].Command)
		}
		for j, w := range want {
			if got.UserHooks[i].Command[j] != w {
				t.Fatalf("UserHooks[%d].Command[%d]=%q want %q", i, j, got.UserHooks[i].Command[j], w)
			}
		}
	}
}

// TestBuildRunRequestOmitsNilUserHooks は UserHooks が nil のときに bridge.Request 側も nil で渡され、
// bridge 側で hook chain が変更されない (= 既存 observationHook のみが走る) ことを保証する後方互換ガード。
func TestBuildRunRequestOmitsNilUserHooks(t *testing.T) {
	req := provider.AgentRequest{Prompt: "hi", CWD: "/repo"}
	got := BuildRunRequest("req-1", req)
	if got.UserHooks != nil {
		t.Fatalf("UserHooks should remain nil when not specified, got %#v", got.UserHooks)
	}
}

// TestBuildRunRequestCopiesUserHookCommandSlice は UserHook.Command が呼び出し元と
// 共有されない (浅いコピーで分離されている) ことを検証する。後段で worker が複数 request を
// 並列で扱う場合に slice mutation で副作用が出ないことを担保する。
func TestBuildRunRequestCopiesUserHookCommandSlice(t *testing.T) {
	original := []string{"bash", "-lc", "/tmp/verify-gate.sh"}
	req := provider.AgentRequest{
		Prompt: "hi",
		CWD:    "/repo",
		UserHooks: []provider.UserHook{
			{Matcher: "Edit", Command: original},
		},
	}
	got := BuildRunRequest("req-1", req)
	// 元の slice を後から書き換えても bridge.Request 側に影響が出ないことを確認する。
	original[2] = "/tmp/HACKED.sh"
	if got.UserHooks[0].Command[2] != "/tmp/verify-gate.sh" {
		t.Fatalf("UserHook.Command should be copied, got %q", got.UserHooks[0].Command[2])
	}
}

// TestBuildRunRequestForwardsAdditionalDirectories は AgentRequest.AdditionalDirectories が
// bridge.Request.AdditionalDirectories に素通しされることを検証する (issue #66 A7)。
func TestBuildRunRequestForwardsAdditionalDirectories(t *testing.T) {
	req := provider.AgentRequest{
		Prompt:                "hi",
		CWD:                   "/repo",
		AdditionalDirectories: []string{"/usr/local/lib", "/opt/data"},
	}
	got := BuildRunRequest("req-1", req)
	if len(got.AdditionalDirectories) != 2 {
		t.Fatalf("AdditionalDirectories length: got=%d want=2", len(got.AdditionalDirectories))
	}
	if got.AdditionalDirectories[0] != "/usr/local/lib" || got.AdditionalDirectories[1] != "/opt/data" {
		t.Fatalf("AdditionalDirectories not preserved: %#v", got.AdditionalDirectories)
	}
}

// TestBuildRunRequestOmitsNilAdditionalDirectories は AdditionalDirectories が nil のとき
// bridge.Request 側も nil で渡され、bridge 側で `['/']` のデフォルト許可が適用される後方互換ガード。
func TestBuildRunRequestOmitsNilAdditionalDirectories(t *testing.T) {
	req := provider.AgentRequest{Prompt: "hi", CWD: "/repo"}
	got := BuildRunRequest("req-1", req)
	if got.AdditionalDirectories != nil {
		t.Fatalf("AdditionalDirectories should remain nil when not specified, got %#v", got.AdditionalDirectories)
	}
}

// TestBuildRunRequestPreservesExplicitEmptyAdditionalDirectories は明示的に空配列を
// 渡したときに、bridge.Request 側も空配列のまま (= restrict 意図) になることを検証する。
// この区別が壊れると「制限したいのにデフォルト許可になる」という重大な seam 破壊が起きる。
func TestBuildRunRequestPreservesExplicitEmptyAdditionalDirectories(t *testing.T) {
	req := provider.AgentRequest{
		Prompt:                "hi",
		CWD:                   "/repo",
		AdditionalDirectories: []string{},
	}
	got := BuildRunRequest("req-1", req)
	if got.AdditionalDirectories == nil {
		t.Fatalf("explicit empty AdditionalDirectories must be passed as empty slice, got nil")
	}
	if len(got.AdditionalDirectories) != 0 {
		t.Fatalf("explicit empty AdditionalDirectories must remain empty, got %#v", got.AdditionalDirectories)
	}
}

// TestBuildRunRequestAdditionalDirectoriesWireFormat は struct field レベルだけでなく
// JSON wire 上でも nil / 空配列 / 非空が正しく区別されることを検証する (issue #66 A7)。
//
// この test は `omitempty` バグへの直接的な回帰防止: `omitempty` は空スライスも omit するため、
// 「明示的空配列 = restrict 意図」という seam が JSON marshal で消える。`omitzero` (Go 1.24+) に
// 切り替えた前提で、nil → omit / `[]` → `[]` / 非空 → 値、の 3 状態を wire-level で固定する。
func TestBuildRunRequestAdditionalDirectoriesWireFormat(t *testing.T) {
	t.Run("nil omits the field on the wire (default-allow)", func(t *testing.T) {
		got := BuildRunRequest("req-1", provider.AgentRequest{Prompt: "hi", CWD: "/repo"})
		b, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("marshal failed: %v", err)
		}
		if bytes.Contains(b, []byte(`"additionalDirectories"`)) {
			t.Fatalf("nil should be omitted on the wire, got %s", b)
		}
	})

	t.Run("explicit empty slice survives marshal as [] (restrict intent)", func(t *testing.T) {
		got := BuildRunRequest("req-1", provider.AgentRequest{
			Prompt:                "hi",
			CWD:                   "/repo",
			AdditionalDirectories: []string{},
		})
		b, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("marshal failed: %v", err)
		}
		if !bytes.Contains(b, []byte(`"additionalDirectories":[]`)) {
			t.Fatalf("explicit empty must survive as []; got %s", b)
		}
	})

	t.Run("non-empty slice is emitted verbatim", func(t *testing.T) {
		got := BuildRunRequest("req-1", provider.AgentRequest{
			Prompt:                "hi",
			CWD:                   "/repo",
			AdditionalDirectories: []string{"/usr/local/lib", "/opt/data"},
		})
		b, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("marshal failed: %v", err)
		}
		if !bytes.Contains(b, []byte(`"additionalDirectories":["/usr/local/lib","/opt/data"]`)) {
			t.Fatalf("non-empty must be emitted verbatim; got %s", b)
		}
	})
}

// TestConsumeEventsEmitsReasoningEvent は bridge から流れてきた reasoning event が
// provider.EventReasoning として onEvent に伝搬されることを検証する (issue #66 A15+D7)。
// id (run id) / itemId (item-level identity) / text の 3 値が AgentEvent.ItemID / Text 経由で
// 上位に届くこと、reasoning text が response.Content に混入しないことを併せて固定する。
func TestConsumeEventsEmitsReasoningEvent(t *testing.T) {
	lines := strings.Join([]string{
		`{"type":"ready"}`,
		`{"type":"reasoning","id":"req-1","itemId":"reason-1","text":"thinking carefully"}`,
		`{"type":"text","id":"req-1","text":"final answer"}`,
		`{"type":"done","id":"req-1","result":"final answer","sessionId":"s1"}`,
		``,
	}, "\n")

	var captured []provider.AgentEvent
	resp, err := ConsumeEvents(
		context.Background(),
		strings.NewReader(lines),
		provider.AgentRequest{CWD: "/work"},
		func(event provider.AgentEvent) { captured = append(captured, event) },
		Config{ProviderID: provider.ProviderCodex},
	)
	if err != nil {
		t.Fatalf("ConsumeEvents returned error: %v", err)
	}
	if !resp.Success {
		t.Fatalf("reasoning event must not block success: %#v", resp)
	}
	if resp.Content != "final answer" {
		t.Fatalf("reasoning text must not leak into response.Content, got %q", resp.Content)
	}
	var reasoningCount int
	for _, e := range captured {
		if e.Type == provider.EventReasoning {
			reasoningCount++
			if e.Text != "thinking carefully" {
				t.Fatalf("EventReasoning.Text mismatch: %q", e.Text)
			}
			if e.ItemID != "reason-1" {
				t.Fatalf("EventReasoning.ItemID must carry bridge ItemID, got %q", e.ItemID)
			}
		}
	}
	if reasoningCount != 1 {
		t.Fatalf("expected exactly 1 EventReasoning, got %d (captured=%#v)", reasoningCount, captured)
	}
}

func TestConsumeEventsRoutesByProviderID(t *testing.T) {
	// system_init を通さない経路でも cfg.ProviderID が SystemInfo に伝播することを確認。
	lines := strings.Join([]string{
		`{"type":"ready"}`,
		`{"type":"text","id":"req-1","text":"hi"}`,
		`{"type":"done","id":"req-1","result":"hi","sessionId":"s1"}`,
		``,
	}, "\n")

	resp, err := ConsumeEvents(context.Background(), strings.NewReader(lines), provider.AgentRequest{CWD: "/work"}, nil, Config{
		ProviderID: provider.ProviderClaude,
	})
	if err != nil {
		t.Fatalf("ConsumeEvents returned error: %v", err)
	}
	if resp.SystemInfo == nil || resp.SystemInfo.ProviderID != provider.ProviderClaude {
		t.Fatalf("provider id was not propagated: %#v", resp.SystemInfo)
	}
	if resp.SystemInfo.SessionID != "s1" {
		t.Fatalf("session id from done event was not applied: %#v", resp.SystemInfo)
	}
}

func TestConsumeEventsCallsExtractMetricsForObservations(t *testing.T) {
	calls := 0
	cfg := Config{
		ProviderID: provider.ProviderCodex,
		ExtractMetrics: func(resp *provider.AgentResponse, event bridge.Event) {
			calls++
			// 渡される event がそのまま observation を保持していることを確認。
			if event.Type != bridge.EventObservation || event.ObservationType != "usage" {
				t.Fatalf("unexpected event passed to extractor: %#v", event)
			}
			value := int64(7)
			resp.Metrics.InputTokens = &value
		},
	}
	lines := strings.Join([]string{
		`{"type":"ready"}`,
		`{"type":"observation","id":"req-1","observationType":"usage","payload":{"input_tokens":7}}`,
		`{"type":"done","id":"req-1","result":"ok","sessionId":"s1"}`,
		``,
	}, "\n")

	resp, err := ConsumeEvents(context.Background(), strings.NewReader(lines), provider.AgentRequest{}, nil, cfg)
	if err != nil {
		t.Fatalf("ConsumeEvents returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("ExtractMetrics should be invoked exactly once, got %d", calls)
	}
	if resp.Metrics.InputTokens == nil || *resp.Metrics.InputTokens != 7 {
		t.Fatalf("metrics from extractor were not retained: %#v", resp.Metrics)
	}
}

// TestConsumeEventsStoresResultOriginIntoCtxAtomic は task 2.6 の観測可能な完了:
// 「synthetic result event(origin='user') 受信後、bridge.CancelOriginFromContext(ctx) == "user"」
// を verify する (Req 3.2)。bridge protocol で cancelled な result event が origin を
// 伴うとき、launcher が ctx に attach 済みの atomic ref に Store して agent 層へ伝搬する
// 単方向経路の RED gate。
func TestConsumeEventsStoresResultOriginIntoCtxAtomic(t *testing.T) {
	ref := &atomic.Value{}
	ctx := bridge.WithCancelOrigin(context.Background(), ref)

	lines := strings.Join([]string{
		`{"type":"ready"}`,
		`{"type":"result","id":"req-1","result":"cancelled","sessionId":"s1","origin":"user"}`,
		`{"type":"done","id":"req-1","result":"cancelled","sessionId":"s1"}`,
		``,
	}, "\n")

	_, err := ConsumeEvents(ctx, strings.NewReader(lines), provider.AgentRequest{}, nil, Config{
		ProviderID: provider.ProviderClaude,
	})
	if err != nil {
		t.Fatalf("ConsumeEvents returned error: %v", err)
	}
	if got := bridge.CancelOriginFromContext(ctx); got != bridge.CancelOriginUser {
		t.Fatalf("CancelOriginFromContext after result(origin=user) = %q, want %q", got, bridge.CancelOriginUser)
	}
}

// TestConsumeEventsLeavesCtxAtomicEmptyWhenResultOriginAbsent は cancel が来ていない
// 正常終了 (origin 未指定) で ctx atomic が Store されないこと (空文字のまま) を verify する。
// task 2.6 の「cancel が来ていない正常終了 (origin 未指定) では Store しない」契約の
// negative case test。
func TestConsumeEventsLeavesCtxAtomicEmptyWhenResultOriginAbsent(t *testing.T) {
	ref := &atomic.Value{}
	ctx := bridge.WithCancelOrigin(context.Background(), ref)

	lines := strings.Join([]string{
		`{"type":"ready"}`,
		`{"type":"result","id":"req-1","result":"ok","sessionId":"s1"}`,
		`{"type":"done","id":"req-1","result":"ok","sessionId":"s1"}`,
		``,
	}, "\n")

	_, err := ConsumeEvents(ctx, strings.NewReader(lines), provider.AgentRequest{}, nil, Config{
		ProviderID: provider.ProviderClaude,
	})
	if err != nil {
		t.Fatalf("ConsumeEvents returned error: %v", err)
	}
	if got := bridge.CancelOriginFromContext(ctx); got != "" {
		t.Fatalf("CancelOriginFromContext after non-cancel result = %q, want empty string", got)
	}
}

// TestConsumeEventsHandlesResultOriginWithoutCtxAtomic は ctx に atomic ref が
// attach されていない既存呼び出し経路で、result event に origin が乗っていても
// panic / 副作用なく完了することを verify する (defense-in-depth)。
func TestConsumeEventsHandlesResultOriginWithoutCtxAtomic(t *testing.T) {
	lines := strings.Join([]string{
		`{"type":"ready"}`,
		`{"type":"result","id":"req-1","result":"cancelled","sessionId":"s1","origin":"user"}`,
		`{"type":"done","id":"req-1","result":"cancelled","sessionId":"s1"}`,
		``,
	}, "\n")

	_, err := ConsumeEvents(context.Background(), strings.NewReader(lines), provider.AgentRequest{}, nil, Config{
		ProviderID: provider.ProviderClaude,
	})
	if err != nil {
		t.Fatalf("ConsumeEvents returned error: %v", err)
	}
}

func TestConsumeEventsErrorEventReturnsFailureWithProviderID(t *testing.T) {
	lines := strings.Join([]string{
		`{"type":"ready"}`,
		`{"type":"error","id":"req-1","message":"boom"}`,
		``,
	}, "\n")

	resp, err := ConsumeEvents(
		context.Background(),
		strings.NewReader(lines),
		provider.AgentRequest{ResumeSessionID: "prev", CWD: "/work"},
		nil,
		Config{ProviderID: provider.ProviderCursorSDK},
	)
	if err != nil {
		t.Fatalf("ConsumeEvents returned error: %v", err)
	}
	if resp.Success {
		t.Fatalf("expected failure response, got %#v", resp)
	}
	if resp.Content != "boom" {
		t.Fatalf("error message was not propagated: %#v", resp)
	}
	if resp.SystemInfo == nil || resp.SystemInfo.ProviderID != provider.ProviderCursorSDK {
		t.Fatalf("provider id should be preserved on error path: %#v", resp.SystemInfo)
	}
	if resp.SessionID != "prev" {
		t.Fatalf("resume session id should be carried over on error: %#v", resp)
	}
}

// TestConsumeEventsHookBlockErrorContinuesRun は issue #47 の中核挙動の検証:
// PreToolUse user hook が block を返した bridge の error event (code='hook_block') を受け取っても
// run を中断せず、後続 done event まで通常通り進むことを確認する。
// 同時に AgentEventCallback で provider.EventError が発火し、Message/Code/Retryable が
// そのまま乗っていることも検証する。
func TestConsumeEventsHookBlockErrorContinuesRun(t *testing.T) {
	lines := strings.Join([]string{
		`{"type":"ready"}`,
		`{"type":"error","id":"req-1","message":"hook_block: verify ログ未読","code":"hook_block","retryable":false}`,
		`{"type":"text","id":"req-1","text":"trying alternative"}`,
		`{"type":"done","id":"req-1","result":"completed","sessionId":"s1"}`,
		``,
	}, "\n")

	var captured []provider.AgentEvent
	resp, err := ConsumeEvents(
		context.Background(),
		strings.NewReader(lines),
		provider.AgentRequest{CWD: "/work"},
		func(event provider.AgentEvent) { captured = append(captured, event) },
		Config{ProviderID: provider.ProviderClaude},
	)
	if err != nil {
		t.Fatalf("ConsumeEvents returned error: %v", err)
	}
	if !resp.Success {
		t.Fatalf("hook_block error should not fail the run, got resp=%#v", resp)
	}
	if resp.Content != "completed" {
		t.Fatalf("done event content should be applied even after hook_block error: %#v", resp)
	}
	// EventError, EventText, EventDone の 3 つが発火している想定。
	var sawError bool
	for _, e := range captured {
		if e.Type == provider.EventError {
			sawError = true
			if e.Code != "hook_block" {
				t.Fatalf("EventError.Code should be hook_block, got %q", e.Code)
			}
			if e.Retryable {
				t.Fatalf("EventError.Retryable should be false for hook_block, got true")
			}
			if !strings.Contains(e.Message, "verify ログ未読") {
				t.Fatalf("EventError.Message should contain hook reason, got %q", e.Message)
			}
		}
	}
	if !sawError {
		t.Fatalf("expected an EventError to be emitted via onEvent, captured=%#v", captured)
	}
}

// TestConsumeEventsNonHookBlockErrorStillTerminates は issue #47 の回帰防止:
// hook_block 以外の code (空文字 / "fatal" 等) を持つ error event は、従来通り
// FailureResponse で run を終了することを保証する。後方互換ガード。
func TestConsumeEventsNonHookBlockErrorStillTerminates(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{name: "no_code", json: `{"type":"error","id":"req-1","message":"boom"}`},
		{name: "other_code", json: `{"type":"error","id":"req-1","message":"boom","code":"fatal","retryable":false}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines := strings.Join([]string{
				`{"type":"ready"}`,
				c.json,
				``,
			}, "\n")
			resp, err := ConsumeEvents(
				context.Background(),
				strings.NewReader(lines),
				provider.AgentRequest{CWD: "/work"},
				nil,
				Config{ProviderID: provider.ProviderClaude},
			)
			if err != nil {
				t.Fatalf("ConsumeEvents returned error: %v", err)
			}
			if resp.Success {
				t.Fatalf("non-hook_block error should still terminate the run as failure, got %#v", resp)
			}
			if resp.Content != "boom" {
				t.Fatalf("error message should be propagated: %#v", resp)
			}
		})
	}
}

func TestResolveDefaultBridgePathHonoursEnv(t *testing.T) {
	t.Setenv("RILLL_TEST_BRIDGE_PATH", "/tmp/explicit-bridge.js")
	got := ResolveDefaultBridgePath("RILLL_TEST_BRIDGE_PATH", "bridge/missing.js")
	if got != "/tmp/explicit-bridge.js" {
		t.Fatalf("env override was not honoured: %q", got)
	}
}

func TestNumberAndIntNumber(t *testing.T) {
	if v, ok := Number(float64(1.5)); !ok || v != 1.5 {
		t.Fatalf("Number(float64) failed: %v %v", v, ok)
	}
	if v, ok := Number(int(2)); !ok || v != 2 {
		t.Fatalf("Number(int) failed: %v %v", v, ok)
	}
	if v, ok := Number(int64(3)); !ok || v != 3 {
		t.Fatalf("Number(int64) failed: %v %v", v, ok)
	}
	if _, ok := Number("nope"); ok {
		t.Fatalf("Number(string) should fail")
	}
	if v, ok := IntNumber(float64(4.9)); !ok || v != 4 {
		t.Fatalf("IntNumber should truncate to 4, got %d (%v)", v, ok)
	}
}

func TestPayloadAsMap(t *testing.T) {
	event := bridge.Event{Payload: []byte(`{"a":1,"b":"x"}`)}
	got, ok := PayloadAsMap(event)
	if !ok || got["a"].(float64) != 1 || got["b"].(string) != "x" {
		t.Fatalf("PayloadAsMap failed: %#v ok=%v", got, ok)
	}
	if _, ok := PayloadAsMap(bridge.Event{}); ok {
		t.Fatalf("PayloadAsMap should return false on empty payload")
	}
	if _, ok := PayloadAsMap(bridge.Event{Payload: []byte("not json")}); ok {
		t.Fatalf("PayloadAsMap should return false on invalid json")
	}
}

func TestFailureResponseShape(t *testing.T) {
	resp := FailureResponse("oops", provider.AgentRequest{ResumeSessionID: "prev", CWD: "/cwd"}, provider.ProviderClaude)
	if resp.Success {
		t.Fatalf("FailureResponse should be unsuccessful")
	}
	if resp.Content != "oops" || resp.SessionID != "prev" {
		t.Fatalf("FailureResponse fields not set: %#v", resp)
	}
	if resp.SystemInfo == nil || resp.SystemInfo.ProviderID != provider.ProviderClaude || resp.SystemInfo.CWD != "/cwd" {
		t.Fatalf("FailureResponse system info: %#v", resp.SystemInfo)
	}
}

func TestToProviderSystemInfoNilInfo(t *testing.T) {
	got := ToProviderSystemInfo(nil, provider.ProviderCodex)
	if got == nil || got.ProviderID != provider.ProviderCodex {
		t.Fatalf("nil info should produce stub with provider id: %#v", got)
	}
}

func TestRunReturnsHintedFailureWhenBridgeMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "definitely-missing-bridge.js")
	resp, err := Run(context.Background(), RunOptions{
		Config: Config{
			ProviderID:        provider.ProviderClaude,
			DefaultBridgePath: missing,
			BuildHint:         "npm run build:claude-bridge",
		},
		BridgeScript: missing,
	}, provider.AgentRequest{ResumeSessionID: "prev", CWD: t.TempDir()}, nil)
	if err != nil {
		t.Fatalf("Run should swallow error and return failure response, got err=%v", err)
	}
	if resp.Success {
		t.Fatalf("response should be unsuccessful: %#v", resp)
	}
	if !strings.Contains(resp.Content, "bridge script not found") {
		t.Fatalf("response content should mention missing bridge script, got %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "npm run build:claude-bridge") {
		t.Fatalf("response content should include provider build hint, got %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "just build-bridge") {
		t.Fatalf("response content should also mention just build-bridge, got %q", resp.Content)
	}
	if resp.SystemInfo == nil || resp.SystemInfo.ProviderID != provider.ProviderClaude {
		t.Fatalf("provider id should be preserved on missing-bridge failure: %#v", resp.SystemInfo)
	}
}

func TestRunFallsBackToJustBuildHintWhenBuildHintEmpty(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.js")
	resp, err := Run(context.Background(), RunOptions{
		Config:       Config{ProviderID: provider.ProviderCodex, DefaultBridgePath: missing},
		BridgeScript: missing,
	}, provider.AgentRequest{}, nil)
	if err != nil {
		t.Fatalf("Run should swallow error, got %v", err)
	}
	if resp.Success {
		t.Fatalf("response should be unsuccessful: %#v", resp)
	}
	if strings.Contains(resp.Content, "npm run build:") {
		t.Fatalf("npm hint should be omitted when BuildHint is empty, got %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "just build-bridge") {
		t.Fatalf("default hint should mention just build-bridge, got %q", resp.Content)
	}
}

func TestRunReturnsErrorWhenBridgePathIsDirectory(t *testing.T) {
	dir := t.TempDir()
	resp, err := Run(context.Background(), RunOptions{
		Config:       Config{ProviderID: provider.ProviderCursorSDK, DefaultBridgePath: dir},
		BridgeScript: dir,
	}, provider.AgentRequest{}, nil)
	if err != nil {
		t.Fatalf("Run should swallow error, got %v", err)
	}
	if resp.Success {
		t.Fatalf("response should be unsuccessful when path is a directory: %#v", resp)
	}
	if !strings.Contains(resp.Content, "ディレクトリ") {
		t.Fatalf("response should explain that path resolves to a directory, got %q", resp.Content)
	}
}

func TestToProviderSystemInfoCopiesMCPServers(t *testing.T) {
	info := &bridge.SystemInfo{
		UUID:       "uuid",
		SessionID:  "sess",
		Tools:      []string{"Read"},
		MCPServers: []bridge.MCPServerInfo{{Name: "fs", Status: "ready"}},
		Model:      "claude-x",
	}
	got := ToProviderSystemInfo(info, provider.ProviderClaude)
	if got.ProviderID != provider.ProviderClaude || got.UUID != "uuid" || got.Model != "claude-x" {
		t.Fatalf("scalar fields not copied: %#v", got)
	}
	if len(got.MCPServers) != 1 || got.MCPServers[0].Name != "fs" || got.MCPServers[0].Status != "ready" {
		t.Fatalf("MCP servers not copied: %#v", got.MCPServers)
	}
}
