package provider

import "testing"

func TestAgentEventMatchesStreamingContract(t *testing.T) {
	event := AgentEvent{Type: EventText, Text: "hello"}
	if event.Type != "text" || event.Text != "hello" {
		t.Fatalf("unexpected text event: %#v", event)
	}

	done := AgentEvent{Type: EventDone, Content: "final", SessionID: "s1"}
	if done.Type != "done" || done.Content != "final" || done.SessionID != "s1" {
		t.Fatalf("unexpected done event: %#v", done)
	}
}

func TestAgentRequestCarriesStructuredOutput(t *testing.T) {
	req := AgentRequest{
		Prompt: "prompt",
		CWD:    "/tmp/project",
		OutputFormat: &OutputFormat{
			Type:   "json_schema",
			Schema: map[string]any{"type": "object"},
		},
	}

	if req.OutputFormat == nil || req.OutputFormat.Type != "json_schema" {
		t.Fatalf("structured output was not preserved: %#v", req)
	}
}
