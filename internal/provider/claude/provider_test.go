package claude

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
// AgentRequest.PermissionMode / MaxTurns が claude provider 経由で
// bridge.Request 同名 field に素通しコピーされることを検証する
// (output-contract-2phase task 3.1)。
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
// 後方互換ガード (output-contract-2phase task 3.1)。
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
	t.Setenv("RILLL_CLAUDE_BRIDGE", "/tmp/custom-bridge.js")

	got := New()
	if got.BridgeScript != "/tmp/custom-bridge.js" {
		t.Fatalf("environment bridge path was not used: %#v", got)
	}
}

func TestConsumeEventsForwardsStreamingAndReturnsResult(t *testing.T) {
	lines := strings.Join([]string{
		`{"type":"ready"}`,
		`{"type":"text","id":"req-1","text":"hel"}`,
		`{"type":"tool_use","id":"req-1","tool":"Read"}`,
		`{"type":"result","id":"req-1","result":"hello","sessionId":"s1","structuredOutput":{"ok":true}}`,
		`{"type":"done","id":"req-1","result":"hello","sessionId":"s1"}`,
		``,
	}, "\n")

	var events []provider.AgentEvent
	resp, err := ConsumeEvents(context.Background(), strings.NewReader(lines), provider.AgentRequest{}, func(event provider.AgentEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("ConsumeEvents returned error: %v", err)
	}
	if !resp.Success || resp.Content != "hello" || resp.SessionID != "s1" {
		t.Fatalf("unexpected response: %#v", resp)
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
		`{"type":"observation","id":"req-1","source":"claude-sdk","observationType":"result","observationSubtype":"success","sequence":2,"payload":{"type":"result","subtype":"success","total_cost_usd":0.5,"usage":{"input_tokens":20,"output_tokens":8}}}`,
		`{"type":"result","id":"req-1","result":"hello","sessionId":"s1"}`,
		`{"type":"done","id":"req-1","result":"hello","sessionId":"s1"}`,
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
	if events[0].Type != provider.EventObservation || events[0].Source != "claude-sdk" || events[0].ObservationType != "result" {
		t.Fatalf("observation event was not forwarded: %#v", events[0])
	}
	if resp.Metrics.TotalCostUSD == nil || *resp.Metrics.TotalCostUSD != 0.5 {
		t.Fatalf("total cost metric was not extracted: %#v", resp.Metrics)
	}
	if resp.Metrics.InputTokens == nil || *resp.Metrics.InputTokens != 20 || resp.Metrics.OutputTokens == nil || *resp.Metrics.OutputTokens != 8 {
		t.Fatalf("token metrics were not extracted: %#v", resp.Metrics)
	}
	if resp.Metrics.ResultPayload == nil {
		t.Fatalf("result payload was not preserved: %#v", resp.Metrics)
	}
}
