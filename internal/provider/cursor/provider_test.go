package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

func TestBuildArgsIncludesWorkspaceTrustJSONAndPrompt(t *testing.T) {
	req := provider.AgentRequest{Prompt: "hello", CWD: "/repo"}

	got := BuildArgs(req)
	want := []string{"-p", "--trust", "--output-format", "json", "--workspace", "/repo", "--force", "--", "hello"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args mismatch\nwant: %#v\n got: %#v", want, got)
	}
}

// TestBuildArgsForwardsModelFlag は AgentRequest.Model が指定されたとき cursor-agent CLI に
// `--model <name>` を素通しすることを確認する (review-workflow Task 1.3)。zero-value のときは
// flag を追加せず、cursor-agent の既定モデルを維持する。
func TestBuildArgsForwardsModelFlag(t *testing.T) {
	req := provider.AgentRequest{Prompt: "hello", CWD: "/repo", Model: "gpt-5-codex"}
	got := BuildArgs(req)
	hasFlag := false
	for i, arg := range got {
		if arg == "--model" && i+1 < len(got) && got[i+1] == "gpt-5-codex" {
			hasFlag = true
			break
		}
	}
	if !hasFlag {
		t.Fatalf("expected --model flag with value, got %#v", got)
	}
}

// TestBuildArgsOmitsModelFlagWhenEmpty は AgentRequest.Model が空文字列のとき --model flag を
// 追加しないことを確認する。
func TestBuildArgsOmitsModelFlagWhenEmpty(t *testing.T) {
	req := provider.AgentRequest{Prompt: "hello", CWD: "/repo"}
	got := BuildArgs(req)
	for _, arg := range got {
		if arg == "--model" {
			t.Fatalf("--model flag should not appear when Model is empty: %#v", got)
		}
	}
}

func TestBuildArgsAddsResumeAndStructuredPrompt(t *testing.T) {
	req := provider.AgentRequest{
		Prompt:          "make json",
		CWD:             "/repo",
		ResumeSessionID: "session-1",
		OutputFormat: &provider.OutputFormat{
			Type:   "json_schema",
			Schema: map[string]any{"type": "object"},
		},
	}

	got := BuildArgs(req)
	if got[6] != "--resume" || got[7] != "session-1" {
		t.Fatalf("resume args missing: %#v", got)
	}
	if got[len(got)-1] == "make json" {
		t.Fatal("structured output prompt was not expanded")
	}
	prompt := got[len(got)-1]
	if !strings.Contains(prompt, "Reply with **only** a single JSON value") {
		t.Fatalf("structured output prompt should ask for a JSON value only:\n%s", prompt)
	}
	if strings.Contains(prompt, "Return exactly one fenced JSON block") {
		t.Fatalf("structured output prompt should not require fenced JSON:\n%s", prompt)
	}
}

func TestParseCursorOutputExtractsContentAndSessionID(t *testing.T) {
	parsed, err := ParseOutput(`{"response":{"content":"hello","session_id":"s1"}}`)
	if err != nil {
		t.Fatalf("ParseOutput returned error: %v", err)
	}
	if parsed.Content != "hello" || parsed.SessionID != "s1" {
		t.Fatalf("unexpected parsed output: %#v", parsed)
	}
}

func TestParseStructuredOutputAcceptsFencedJSONValue(t *testing.T) {
	got, err := ParseStructuredOutput("```json\n{\"ok\":true}\n```")
	if err != nil {
		t.Fatalf("ParseStructuredOutput returned error: %v", err)
	}
	record, ok := any(got).(map[string]any)
	if !ok {
		t.Fatalf("expected object structured output, got %#v", got)
	}
	if value, _ := record["ok"].(bool); !value {
		t.Fatalf("unexpected structured output: %#v", got)
	}
}

func TestParseStructuredOutputUsesLastValidFencedJSONBlock(t *testing.T) {
	got, err := ParseStructuredOutput("example\n```json\n{\"old\":true}\n```\nfinal\n```json\n{\"ok\":true}\n```")
	if err != nil {
		t.Fatalf("ParseStructuredOutput returned error: %v", err)
	}
	record, ok := any(got).(map[string]any)
	if !ok {
		t.Fatalf("expected object structured output, got %#v", got)
	}
	if value, _ := record["ok"].(bool); !value {
		t.Fatalf("unexpected structured output: %#v", got)
	}
}

func TestParseStructuredOutputPrefersFencedResultOverEarlierExample(t *testing.T) {
	got, err := ParseStructuredOutput("Example: {\"old\":true}\nFinal:\n```json\n{\"ok\":true}\n```")
	if err != nil {
		t.Fatalf("ParseStructuredOutput returned error: %v", err)
	}
	record, ok := any(got).(map[string]any)
	if !ok {
		t.Fatalf("expected object structured output, got %#v", got)
	}
	if value, _ := record["ok"].(bool); !value {
		t.Fatalf("unexpected structured output: %#v", got)
	}
}

func TestParseStructuredOutputAcceptsDirectJSONValue(t *testing.T) {
	got, err := ParseStructuredOutput(`{"ok":true}`)
	if err != nil {
		t.Fatalf("ParseStructuredOutput returned error: %v", err)
	}
	record, ok := any(got).(map[string]any)
	if !ok {
		t.Fatalf("expected object structured output, got %#v", got)
	}
	if value, _ := record["ok"].(bool); !value {
		t.Fatalf("unexpected structured output: %#v", got)
	}
}

func TestParseStructuredOutputAcceptsArrayJSONValue(t *testing.T) {
	got, err := ParseStructuredOutput(`[{"ok":true}]`)
	if err != nil {
		t.Fatalf("ParseStructuredOutput returned error: %v", err)
	}
	items, ok := any(got).([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("expected array structured output, got %#v", got)
	}
}

func TestParseStructuredOutputExtractsJSONValueFromProse(t *testing.T) {
	got, err := ParseStructuredOutput("Here is the JSON:\n{\"ok\":true}\nThanks.")
	if err != nil {
		t.Fatalf("ParseStructuredOutput returned error: %v", err)
	}
	record, ok := any(got).(map[string]any)
	if !ok {
		t.Fatalf("expected object structured output, got %#v", got)
	}
	if value, _ := record["ok"].(bool); !value {
		t.Fatalf("unexpected structured output: %#v", got)
	}
}

func TestCallEmitsSummaryResultObservation(t *testing.T) {
	p := &Provider{
		Command: "cursor-agent",
		runner: func(context.Context, string, []string, string) (string, string, error) {
			return `{"content":"hello","session_id":"session-1","model":"cursor-model","tools":["Read","Shell"]}`, "debug log", nil
		},
	}

	var events []provider.AgentEvent
	resp, err := p.Call(context.Background(), provider.AgentRequest{
		Prompt:       "hello",
		CWD:          "/repo",
		AkashicLevel: "summary",
	}, func(event provider.AgentEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if !resp.Success || resp.Content != "hello" || resp.SessionID != "session-1" {
		t.Fatalf("unexpected response: %#v", resp)
	}
	if resp.Metrics.TotalCostUSD != nil || resp.Metrics.InputTokens != nil || resp.Metrics.OutputTokens != nil {
		t.Fatalf("cursor metrics should stay empty: %#v", resp.Metrics)
	}
	if len(events) != 3 {
		t.Fatalf("expected observation/text/done events, got %#v", events)
	}
	observation := events[0]
	if observation.Type != provider.EventObservation || observation.Source != "cursor-cli" || observation.ObservationType != "result" || observation.Sequence != 0 {
		t.Fatalf("unexpected observation event: %#v", observation)
	}
	payload := decodePayload(t, observation.Payload)
	if payload["success"] != true || payload["sessionId"] != "session-1" || payload["cwd"] != "/repo" {
		t.Fatalf("summary payload missing major result fields: %#v", payload)
	}
	if payload["exitCode"] != float64(0) {
		t.Fatalf("summary payload should include successful exit code: %#v", payload)
	}
	if payload["stdoutBytes"] != float64(len(`{"content":"hello","session_id":"session-1","model":"cursor-model","tools":["Read","Shell"]}`)) || payload["stderrBytes"] != float64(len("debug log")) {
		t.Fatalf("summary payload should include output byte counts: %#v", payload)
	}
	if payload["model"] != "cursor-model" || payload["toolsCount"] != float64(2) {
		t.Fatalf("summary payload should include stable system info when available: %#v", payload)
	}
	if _, ok := payload["stdout"]; ok {
		t.Fatalf("summary payload should not include stdout body: %#v", payload)
	}
	if _, ok := payload["stderr"]; ok {
		t.Fatalf("summary payload should not include stderr body: %#v", payload)
	}
	if events[1].Type != provider.EventText || events[1].Text != "hello" {
		t.Fatalf("text event was not preserved: %#v", events[1])
	}
	if events[2].Type != provider.EventDone || events[2].Content != "hello" || events[2].SessionID != "session-1" {
		t.Fatalf("done event was not preserved: %#v", events[2])
	}
}

func TestCallEmitsFullStdoutAndStderrObservations(t *testing.T) {
	stdout := `{"content":"hello","session_id":"session-1"}`
	stderr := "warning: detail"
	p := &Provider{
		runner: func(context.Context, string, []string, string) (string, string, error) {
			return stdout, stderr, nil
		},
	}

	var events []provider.AgentEvent
	_, err := p.Call(context.Background(), provider.AgentRequest{
		Prompt:       "hello",
		CWD:          "/repo",
		AkashicLevel: "full",
	}, func(event provider.AgentEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}

	observations := observationEvents(events)
	if got := observationTypes(observations); !reflect.DeepEqual(got, []string{"stdout", "stderr", "result"}) {
		t.Fatalf("unexpected observation types: %#v", got)
	}
	if observations[0].Sequence != 0 || observations[1].Sequence != 1 || observations[2].Sequence != 2 {
		t.Fatalf("observation sequences should start at zero: %#v", observations)
	}
	stdoutPayload := decodePayload(t, observations[0].Payload)
	if stdoutPayload["text"] != stdout || stdoutPayload["bytes"] != float64(len(stdout)) {
		t.Fatalf("stdout payload mismatch: %#v", stdoutPayload)
	}
	stderrPayload := decodePayload(t, observations[1].Payload)
	if stderrPayload["text"] != stderr || stderrPayload["bytes"] != float64(len(stderr)) {
		t.Fatalf("stderr payload mismatch: %#v", stderrPayload)
	}
}

func TestCallEmitsDebugExecutionObservation(t *testing.T) {
	p := &Provider{
		Command: "cursor-agent",
		runner: func(context.Context, string, []string, string) (string, string, error) {
			return `{"content":"hello","session_id":"session-1"}`, "", nil
		},
	}

	var events []provider.AgentEvent
	_, err := p.Call(context.Background(), provider.AgentRequest{
		Prompt:          "secret prompt",
		CWD:             "/repo",
		ResumeSessionID: "resume-1",
		AkashicLevel:    "debug",
	}, func(event provider.AgentEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}

	observations := observationEvents(events)
	if len(observations) < 1 || observations[0].ObservationType != "execution" {
		t.Fatalf("debug execution observation was not emitted first: %#v", observations)
	}
	payload := decodePayload(t, observations[0].Payload)
	if payload["command"] != "cursor-agent" || payload["cwd"] != "/repo" || payload["resumeSessionId"] != "resume-1" || payload["akashicLevel"] != "debug" {
		t.Fatalf("execution payload missing command context: %#v", payload)
	}
	args, ok := payload["argsWithoutPrompt"].([]any)
	if !ok || len(args) == 0 {
		t.Fatalf("execution payload missing argsWithoutPrompt: %#v", payload)
	}
	argsBody, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal argsWithoutPrompt: %v", err)
	}
	if strings.Contains(string(argsBody), "secret prompt") {
		t.Fatalf("execution payload should not include the prompt: %#v", payload)
	}
}

func TestCallEmitsErrorObservationForExecutionFailure(t *testing.T) {
	p := &Provider{
		runner: func(context.Context, string, []string, string) (string, string, error) {
			return "", "fatal error", errors.New("exit status 2")
		},
	}

	var events []provider.AgentEvent
	resp, err := p.Call(context.Background(), provider.AgentRequest{
		Prompt:       "hello",
		CWD:          "/repo",
		AkashicLevel: "summary",
	}, func(event provider.AgentEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if resp.Success {
		t.Fatalf("execution failure should return unsuccessful response: %#v", resp)
	}
	observations := observationEvents(events)
	if len(observations) != 1 || observations[0].ObservationType != "error" {
		t.Fatalf("expected one error observation, got %#v", observations)
	}
	payload := decodePayload(t, observations[0].Payload)
	if payload["success"] != false || payload["stderrBytes"] != float64(len("fatal error")) {
		t.Fatalf("error payload missing failure details: %#v", payload)
	}
	if _, ok := payload["stderr"]; ok {
		t.Fatalf("summary error payload should not include stderr body: %#v", payload)
	}
}

func TestCallEmitsErrorObservationForParseFailures(t *testing.T) {
	tests := []struct {
		name          string
		request       provider.AgentRequest
		stdout        string
		wantErrorPart string
	}{
		{
			name:          "invalid cursor json",
			request:       provider.AgentRequest{Prompt: "hello", CWD: "/repo", AkashicLevel: "summary"},
			stdout:        "not json",
			wantErrorPart: "Failed to parse cursor-agent JSON output",
		},
		{
			name: "invalid structured output",
			request: provider.AgentRequest{
				Prompt:       "hello",
				CWD:          "/repo",
				AkashicLevel: "summary",
				OutputFormat: &provider.OutputFormat{Type: "json_schema", Schema: map[string]any{"type": "object"}},
			},
			stdout:        `{"content":"no json block","session_id":"session-1"}`,
			wantErrorPart: "Failed to parse structured output",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Provider{
				runner: func(context.Context, string, []string, string) (string, string, error) {
					return tt.stdout, "", nil
				},
			}
			var events []provider.AgentEvent
			resp, err := p.Call(context.Background(), tt.request, func(event provider.AgentEvent) {
				events = append(events, event)
			})
			if err != nil {
				t.Fatalf("Call returned error: %v", err)
			}
			if resp.Success {
				t.Fatalf("parse failure should return unsuccessful response: %#v", resp)
			}
			observations := observationEvents(events)
			if len(observations) != 1 || observations[0].ObservationType != "error" {
				t.Fatalf("expected one error observation, got %#v", observations)
			}
			payload := decodePayload(t, observations[0].Payload)
			message, _ := payload["message"].(string)
			if !strings.Contains(message, tt.wantErrorPart) {
				t.Fatalf("error payload message mismatch: %#v", payload)
			}
		})
	}
}

func TestCallEmitsStructuredOutputObservationWhenRecovered(t *testing.T) {
	p := &Provider{
		runner: func(context.Context, string, []string, string) (string, string, error) {
			return `{"content":"{\"ok\":true}","session_id":"session-1"}`, "", nil
		},
	}

	var events []provider.AgentEvent
	resp, err := p.Call(context.Background(), provider.AgentRequest{
		Prompt:       "hello",
		CWD:          "/repo",
		AkashicLevel: "debug",
		OutputFormat: &provider.OutputFormat{Type: "json_schema", Schema: map[string]any{"type": "object"}},
	}, func(event provider.AgentEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if !resp.Success || resp.StructuredOutput == nil {
		t.Fatalf("expected successful structured response: %#v", resp)
	}

	observations := observationEvents(events)
	for _, observation := range observations {
		if observation.ObservationType != "structured_output" {
			continue
		}
		payload := decodePayload(t, observation.Payload)
		if payload["strategy"] != "direct" {
			t.Fatalf("structured output payload should include recovery strategy: %#v", payload)
		}
		return
	}
	t.Fatalf("expected structured_output observation, got %#v", observations)
}

func observationEvents(events []provider.AgentEvent) []provider.AgentEvent {
	var observations []provider.AgentEvent
	for _, event := range events {
		if event.Type == provider.EventObservation {
			observations = append(observations, event)
		}
	}
	return observations
}

func observationTypes(events []provider.AgentEvent) []string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.ObservationType)
	}
	return types
}

func decodePayload(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("decode payload: %v\npayload: %s", err, payload)
	}
	return out
}
