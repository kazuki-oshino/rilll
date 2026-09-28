package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

func TestBuildRunRequestMapsProviderRequest(t *testing.T) {
	req := provider.AgentRequest{
		Prompt:          "hello",
		CWD:             "/repo",
		ResumeSessionID: "session-1",
		OutputFormat:    &provider.OutputFormat{Type: "text"},
		AkashicLevel:    "debug",
		Metadata: provider.AgentMetadata{
			WorkflowRunID: "workflow-run-1",
			WorkflowID:    "update-readme",
			WorkflowName:  "README更新",
			StepID:        "plan",
			StepName:      "提案作成",
			AgentCallID:   "agent-call-1",
		},
	}

	got := BuildRunRequest("req-1", req)
	if got.Type != "run" || got.ID != "req-1" || got.Prompt != "hello" || got.CWD != "/repo" {
		t.Fatalf("unexpected bridge request: %#v", got)
	}
	if got.ResumeSessionID != "session-1" || got.OutputFormat == nil || got.OutputFormat.Type != "text" {
		t.Fatalf("request options were not preserved: %#v", got)
	}
	if got.AkashicLevel != "debug" {
		t.Fatalf("akashic level was not preserved: %#v", got)
	}
	if got.Metadata.AgentCallID != "agent-call-1" || got.Metadata.WorkflowRunID != "workflow-run-1" {
		t.Fatalf("metadata was not preserved: %#v", got.Metadata)
	}
}

// TestBuildRunRequestForwardsPermissionModeAndMaxTurns は
// AgentRequest.PermissionMode / MaxTurns が codex provider 経由で
// bridge.Request 同名 field に素通しコピーされることを検証する
// (output-contract-2phase task 3.2)。Codex bridge では permissionMode は
// documented no-op だが、wire schema 整合のため素通しは行う。
func TestBuildRunRequestForwardsPermissionModeAndMaxTurns(t *testing.T) {
	req := provider.AgentRequest{
		Prompt:         "hi",
		CWD:            "/repo",
		PermissionMode: "plan",
		MaxTurns:       2,
	}
	got := BuildRunRequest("req-1", req)
	if got.PermissionMode != "plan" {
		t.Fatalf("PermissionMode mismatch: got=%q want=%q", got.PermissionMode, "plan")
	}
	if got.MaxTurns != 2 {
		t.Fatalf("MaxTurns mismatch: got=%d want=%d", got.MaxTurns, 2)
	}
}

// TestBuildRunRequestOmitsZeroValuePermissionModeAndMaxTurns は
// zero-value (空文字 / 0) のとき bridge.Request 側も空文字 / 0 として
// 渡り、JSON marshal 時に両 key が wire 上から omit されることを検証する
// 後方互換ガード (output-contract-2phase task 3.2)。
func TestBuildRunRequestOmitsZeroValuePermissionModeAndMaxTurns(t *testing.T) {
	req := provider.AgentRequest{Prompt: "hi", CWD: "/repo"}
	got := BuildRunRequest("req-1", req)
	if got.PermissionMode != "" {
		t.Fatalf("zero-value PermissionMode should remain empty, got %q", got.PermissionMode)
	}
	if got.MaxTurns != 0 {
		t.Fatalf("zero-value MaxTurns should remain 0, got %d", got.MaxTurns)
	}
	// wire 上で omitempty が効いて 2 key とも消えていることを確認する。
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if bytes.Contains(encoded, []byte(`"permissionMode"`)) {
		t.Fatalf("zero-value PermissionMode should be omitted on the wire, got %s", encoded)
	}
	if bytes.Contains(encoded, []byte(`"maxTurns"`)) {
		t.Fatalf("zero-value MaxTurns should be omitted on the wire, got %s", encoded)
	}
}

func TestNewUsesBridgePathFromEnvironment(t *testing.T) {
	t.Setenv("RILLL_CODEX_BRIDGE", "/tmp/custom-codex-bridge.js")

	got := New()
	if got.BridgeScript != "/tmp/custom-codex-bridge.js" {
		t.Fatalf("environment bridge path was not used: %#v", got)
	}
}

func TestConsumeEventsForwardsStreamingAndReturnsResult(t *testing.T) {
	lines := strings.Join([]string{
		`{"type":"ready"}`,
		`{"type":"system_init","id":"req-1","systemInfo":{"sessionId":"thread-1","permissionMode":"never","cwd":"/repo","model":"gpt-5.3-codex-spark","modelReasoningEffort":"high"}}`,
		`{"type":"text","id":"req-1","text":"hello"}`,
		`{"type":"tool_use","id":"req-1","tool":"command_execution"}`,
		`{"type":"result","id":"req-1","result":"hello","sessionId":"thread-1","structuredOutput":{"ok":true}}`,
		`{"type":"done","id":"req-1","result":"hello","sessionId":"thread-1"}`,
		``,
	}, "\n")

	var events []provider.AgentEvent
	resp, err := ConsumeEvents(context.Background(), strings.NewReader(lines), provider.AgentRequest{}, func(event provider.AgentEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("ConsumeEvents returned error: %v", err)
	}
	if !resp.Success || resp.Content != "hello" || resp.SessionID != "thread-1" {
		t.Fatalf("unexpected response: %#v", resp)
	}
	if resp.SystemInfo == nil || resp.SystemInfo.ProviderID != provider.ProviderCodex {
		t.Fatalf("system info should expose codex provider id: %#v", resp.SystemInfo)
	}
	if resp.SystemInfo.Model != "gpt-5.3-codex-spark" || resp.SystemInfo.ModelReasoningEffort != "high" {
		t.Fatalf("system info should preserve model/effort: %#v", resp.SystemInfo)
	}
	if len(events) != 3 {
		t.Fatalf("expected text/tool/done events, got %#v", events)
	}
	if events[0].Type != provider.EventText || events[1].Type != provider.EventToolUse || events[2].Type != provider.EventDone {
		t.Fatalf("unexpected forwarded events: %#v", events)
	}
}

func TestConsumeEventsForwardsObservationEventsAndExtractsMetrics(t *testing.T) {
	lines := strings.Join([]string{
		`{"type":"ready"}`,
		`{"type":"observation","id":"req-1","source":"codex-sdk","observationType":"usage","observationSubtype":"turn","sequence":1,"payload":{"input_tokens":42,"output_tokens":13,"cached_input_tokens":1,"reasoning_output_tokens":2}}`,
		`{"type":"result","id":"req-1","result":"hello","sessionId":"thread-1"}`,
		`{"type":"done","id":"req-1","result":"hello","sessionId":"thread-1"}`,
		``,
	}, "\n")

	var events []provider.AgentEvent
	resp, err := ConsumeEvents(context.Background(), strings.NewReader(lines), provider.AgentRequest{}, func(event provider.AgentEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("ConsumeEvents returned error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected observation and done events, got %#v", events)
	}
	if events[0].Type != provider.EventObservation || events[0].Source != "codex-sdk" || events[0].ObservationType != "usage" {
		t.Fatalf("observation event was not forwarded: %#v", events[0])
	}
	if resp.Metrics.InputTokens == nil || *resp.Metrics.InputTokens != 42 {
		t.Fatalf("input tokens were not extracted: %#v", resp.Metrics)
	}
	if resp.Metrics.OutputTokens == nil || *resp.Metrics.OutputTokens != 13 {
		t.Fatalf("output tokens were not extracted: %#v", resp.Metrics)
	}
	if resp.Metrics.ResultPayload == nil {
		t.Fatalf("result payload was not preserved: %#v", resp.Metrics)
	}
	if resp.Metrics.TotalCostUSD != nil {
		t.Fatalf("codex provides no cost; expected nil, got: %#v", *resp.Metrics.TotalCostUSD)
	}
}
