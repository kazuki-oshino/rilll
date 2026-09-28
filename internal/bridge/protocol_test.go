package bridge

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEncoderWritesOneJSONEventPerLine(t *testing.T) {
	var out strings.Builder
	encoder := NewEncoder(&out)

	err := encoder.Encode(Event{Type: EventText, ID: "req-1", Text: "hello"})
	if err != nil {
		t.Fatalf("Encode returned error: %v", err)
	}

	got := out.String()
	want := "{\"type\":\"text\",\"id\":\"req-1\",\"text\":\"hello\"}\n"
	if got != want {
		t.Fatalf("encoded event mismatch\nwant: %q\n got: %q", want, got)
	}
}

func TestEncoderPreservesMaxEffort(t *testing.T) {
	var out strings.Builder
	if err := NewEncoder(&out).Encode(Request{
		Type: RequestRun, ID: "luna", Prompt: "hi", CWD: "/repo",
		Model: "gpt-6-luna", Effort: "max",
	}); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(out.String()), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != "gpt-6-luna" || payload["effort"] != "max" {
		t.Fatalf("wire request lost model or effort: %s", out.String())
	}
}

func TestEncoderEmitsAkashicLevelJSONField(t *testing.T) {
	var out strings.Builder
	encoder := NewEncoder(&out)

	req := Request{
		Type:         RequestRun,
		ID:           "req-1",
		Prompt:       "hello",
		CWD:          "/repo",
		AkashicLevel: "summary",
	}
	if err := encoder.Encode(req); err != nil {
		t.Fatalf("Encode returned error: %v", err)
	}
	encoded := out.String()
	if !strings.Contains(encoded, `"akashicLevel":"summary"`) {
		t.Fatalf("expected akashicLevel JSON field, got: %q", encoded)
	}
	if strings.Contains(encoded, `"observabilityLevel"`) {
		t.Fatalf("legacy observabilityLevel field must not appear, got: %q", encoded)
	}
}

func TestEncoderOmitsAkashicLevelWhenEmpty(t *testing.T) {
	var out strings.Builder
	encoder := NewEncoder(&out)

	req := Request{Type: RequestRun, ID: "req-1", Prompt: "hello", CWD: "/repo"}
	if err := encoder.Encode(req); err != nil {
		t.Fatalf("Encode returned error: %v", err)
	}
	encoded := out.String()
	if strings.Contains(encoded, "akashicLevel") {
		t.Fatalf("empty akashic level must be omitted, got: %q", encoded)
	}
}

func TestEncoderEmitsTraceContextJSONField(t *testing.T) {
	var out strings.Builder
	encoder := NewEncoder(&out)

	req := Request{
		Type:   RequestRun,
		ID:     "req-1",
		Prompt: "hello",
		CWD:    "/repo",
		TraceContext: TraceContext{
			TraceParent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			TraceState:  "vendor=value",
			Baggage:     "project=rilll",
		},
	}
	if err := encoder.Encode(req); err != nil {
		t.Fatalf("Encode returned error: %v", err)
	}
	encoded := out.String()
	for _, want := range []string{
		`"traceContext":{`,
		`"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"`,
		`"tracestate":"vendor=value"`,
		`"baggage":"project=rilll"`,
	} {
		if !strings.Contains(encoded, want) {
			t.Fatalf("encoded request missing %s: %q", want, encoded)
		}
	}
}

func TestEncoderOmitsTraceContextWhenEmpty(t *testing.T) {
	var out strings.Builder
	encoder := NewEncoder(&out)

	req := Request{Type: RequestRun, ID: "req-1", Prompt: "hello", CWD: "/repo"}
	if err := encoder.Encode(req); err != nil {
		t.Fatalf("Encode returned error: %v", err)
	}
	encoded := out.String()
	if strings.Contains(encoded, "traceContext") {
		t.Fatalf("empty trace context must be omitted, got: %q", encoded)
	}
}

func TestDecoderRejectsUnknownEventType(t *testing.T) {
	decoder := NewDecoder(strings.NewReader("{\"type\":\"bogus\",\"id\":\"req-1\"}\n"))

	_, err := decoder.Decode()
	if err == nil {
		t.Fatal("expected unknown event type error")
	}
}

func TestDecoderAcceptsResultEventWithStructuredOutput(t *testing.T) {
	decoder := NewDecoder(strings.NewReader("{\"type\":\"result\",\"id\":\"req-1\",\"result\":\"ok\",\"structuredOutput\":{\"ok\":true},\"sessionId\":\"s1\"}\n"))

	event, err := decoder.Decode()
	if err != nil {
		t.Fatalf("Decode returned error: %v", err)
	}
	if event.Type != EventResult || event.Result != "ok" || event.SessionID != "s1" {
		t.Fatalf("unexpected result event: %#v", event)
	}
	if event.StructuredOutput == nil {
		t.Fatalf("structured output was not preserved: %#v", event)
	}
}

func TestDecoderAcceptsObservationEvent(t *testing.T) {
	decoder := NewDecoder(strings.NewReader("{\"type\":\"observation\",\"id\":\"req-1\",\"source\":\"claude-sdk\",\"observationType\":\"result\",\"observationSubtype\":\"success\",\"sequence\":1,\"payload\":{\"type\":\"result\"},\"payloadTruncated\":false}\n"))

	event, err := decoder.Decode()
	if err != nil {
		t.Fatalf("Decode returned error: %v", err)
	}
	if event.Type != EventObservation || event.Source != "claude-sdk" || event.ObservationType != "result" || event.ObservationSubtype != "success" {
		t.Fatalf("unexpected observation event: %#v", event)
	}
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("payload was not raw JSON: %v", err)
	}
	if payload["type"] != "result" {
		t.Fatalf("unexpected payload: %#v", payload)
	}
}

// CancelOrigin* 定数が closed enum 2 値 ("user" / "engine") に固定されていることを確認する。
// bridge/shared/protocol.ts の cancelRequestSchema.origin と wire-level で一致することの gate。
func TestCancelOriginConstantsHaveExpectedValues(t *testing.T) {
	if CancelOriginUser != "user" {
		t.Fatalf("CancelOriginUser = %q, want %q", CancelOriginUser, "user")
	}
	if CancelOriginEngine != "engine" {
		t.Fatalf("CancelOriginEngine = %q, want %q", CancelOriginEngine, "engine")
	}
}

// CancelRequest が origin field の 3 ケース (空文字 / "user" / "engine") を正しく unmarshal することを確認する。
// origin 不在時は zero-value (空文字) になり、後続層が default 'engine' として解釈する契約に従う。
func TestCancelRequestUnmarshalsOriginThreeCases(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{name: "absent", payload: `{"type":"cancel","id":"req-1"}`, want: ""},
		{name: "user", payload: `{"type":"cancel","id":"req-1","origin":"user"}`, want: "user"},
		{name: "engine", payload: `{"type":"cancel","id":"req-1","origin":"engine"}`, want: "engine"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req CancelRequest
			if err := json.Unmarshal([]byte(tc.payload), &req); err != nil {
				t.Fatalf("Unmarshal returned error: %v", err)
			}
			if req.Type != "cancel" || req.ID != "req-1" {
				t.Fatalf("unexpected cancel request: %#v", req)
			}
			if req.Origin != tc.want {
				t.Fatalf("origin = %q, want %q", req.Origin, tc.want)
			}
		})
	}
}

// CancelRequest を Marshal したときに空 Origin が omitempty で省略されることを確認する。
// shared/protocol.ts 側の `optional()` と対称な振る舞い。
func TestCancelRequestMarshalOmitsEmptyOrigin(t *testing.T) {
	req := CancelRequest{Type: "cancel", ID: "req-1"}
	encoded, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if strings.Contains(string(encoded), "origin") {
		t.Fatalf("expected origin field to be omitted when empty, got: %q", string(encoded))
	}
}

// Event.Origin が result event の origin field 3 ケース (空文字 / "user" / "engine") を正しく decode することを確認する。
// 後続 task (2.6 launcher) で ConsumeEvents が ctx atomic に Store する経路の前提条件。
func TestDecoderAcceptsResultEventOriginThreeCases(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{name: "absent", payload: `{"type":"result","id":"req-1","result":"ok"}` + "\n", want: ""},
		{name: "user", payload: `{"type":"result","id":"req-1","result":"ok","origin":"user"}` + "\n", want: "user"},
		{name: "engine", payload: `{"type":"result","id":"req-1","result":"ok","origin":"engine"}` + "\n", want: "engine"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decoder := NewDecoder(strings.NewReader(tc.payload))
			event, err := decoder.Decode()
			if err != nil {
				t.Fatalf("Decode returned error: %v", err)
			}
			if event.Type != EventResult {
				t.Fatalf("unexpected event type: %q", event.Type)
			}
			if event.Origin != tc.want {
				t.Fatalf("origin = %q, want %q", event.Origin, tc.want)
			}
		})
	}
}

// Event を Marshal したときに空 Origin が omitempty で省略されることを確認する。
// cancel が来ていない正常終了時に origin が wire 上に出ないことの gate (Req 6 後方互換)。
func TestEventMarshalOmitsEmptyOrigin(t *testing.T) {
	var out strings.Builder
	encoder := NewEncoder(&out)

	if err := encoder.Encode(Event{Type: EventResult, ID: "req-1", Result: "ok"}); err != nil {
		t.Fatalf("Encode returned error: %v", err)
	}
	if strings.Contains(out.String(), "origin") {
		t.Fatalf("expected origin field to be omitted when empty, got: %q", out.String())
	}
}

// output-contract-2phase task 1.2: Request.PermissionMode が closed enum 値 (e.g. "plan") を
// JSON wire 上に "permissionMode":"plan" で運搬することを確認する。
// bridge/shared/protocol.ts の runRequestSchema.permissionMode と wire-level で 1:1 対応する。
func TestEncoderEmitsPermissionModeJSONField(t *testing.T) {
	var out strings.Builder
	encoder := NewEncoder(&out)

	req := Request{
		Type:           RequestRun,
		ID:             "req-1",
		Prompt:         "hello",
		CWD:            "/repo",
		PermissionMode: "plan",
	}
	if err := encoder.Encode(req); err != nil {
		t.Fatalf("Encode returned error: %v", err)
	}
	encoded := out.String()
	if !strings.Contains(encoded, `"permissionMode":"plan"`) {
		t.Fatalf("expected permissionMode JSON field, got: %q", encoded)
	}
}

// output-contract-2phase task 1.2: Request.PermissionMode が空文字のとき
// omitempty で wire 上から省略されることを確認する。
// 空文字 = SDK 既定値 ('acceptEdits' を Claude bridge が維持) を意味する zero-value semantics。
func TestEncoderOmitsPermissionModeWhenEmpty(t *testing.T) {
	var out strings.Builder
	encoder := NewEncoder(&out)

	req := Request{Type: RequestRun, ID: "req-1", Prompt: "hello", CWD: "/repo"}
	if err := encoder.Encode(req); err != nil {
		t.Fatalf("Encode returned error: %v", err)
	}
	encoded := out.String()
	if strings.Contains(encoded, "permissionMode") {
		t.Fatalf("empty permissionMode must be omitted, got: %q", encoded)
	}
}

// output-contract-2phase task 1.2: Request.MaxTurns が正の int 値を
// JSON wire 上に "maxTurns":<n> で運搬することを確認する。
// bridge/shared/protocol.ts の runRequestSchema.maxTurns と wire-level で 1:1 対応する。
func TestEncoderEmitsMaxTurnsJSONField(t *testing.T) {
	var out strings.Builder
	encoder := NewEncoder(&out)

	req := Request{
		Type:     RequestRun,
		ID:       "req-1",
		Prompt:   "hello",
		CWD:      "/repo",
		MaxTurns: 2,
	}
	if err := encoder.Encode(req); err != nil {
		t.Fatalf("Encode returned error: %v", err)
	}
	encoded := out.String()
	if !strings.Contains(encoded, `"maxTurns":2`) {
		t.Fatalf("expected maxTurns JSON field, got: %q", encoded)
	}
}

// output-contract-2phase task 1.2: Request.MaxTurns が 0 のとき
// omitempty で wire 上から省略されることを確認する。
// 0 = SDK 既定 turn budget を維持する zero-value semantics。
func TestEncoderOmitsMaxTurnsWhenZero(t *testing.T) {
	var out strings.Builder
	encoder := NewEncoder(&out)

	req := Request{Type: RequestRun, ID: "req-1", Prompt: "hello", CWD: "/repo"}
	if err := encoder.Encode(req); err != nil {
		t.Fatalf("Encode returned error: %v", err)
	}
	encoded := out.String()
	if strings.Contains(encoded, "maxTurns") {
		t.Fatalf("zero maxTurns must be omitted, got: %q", encoded)
	}
}

// output-contract-2phase task 1.2: Request の PermissionMode / MaxTurns が
// JSON marshal → unmarshal の round-trip で値を保持することを確認する。
// closed enum 4 値 ('default' / 'acceptEdits' / 'plan' / 'bypassPermissions') と
// 正の int 値が wire 上で改変されないことを担保する。
func TestRequestPermissionModeAndMaxTurnsRoundTrip(t *testing.T) {
	cases := []struct {
		name           string
		permissionMode string
		maxTurns       int
	}{
		{name: "default", permissionMode: "default", maxTurns: 1},
		{name: "acceptEdits", permissionMode: "acceptEdits", maxTurns: 2},
		{name: "plan", permissionMode: "plan", maxTurns: 5},
		{name: "bypassPermissions", permissionMode: "bypassPermissions", maxTurns: 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := Request{
				Type:           RequestRun,
				ID:             "req-1",
				Prompt:         "hello",
				CWD:            "/repo",
				PermissionMode: tc.permissionMode,
				MaxTurns:       tc.maxTurns,
			}
			encoded, err := json.Marshal(orig)
			if err != nil {
				t.Fatalf("Marshal returned error: %v", err)
			}
			var decoded Request
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("Unmarshal returned error: %v", err)
			}
			if decoded.PermissionMode != tc.permissionMode {
				t.Fatalf("permissionMode round-trip mismatch: want %q, got %q", tc.permissionMode, decoded.PermissionMode)
			}
			if decoded.MaxTurns != tc.maxTurns {
				t.Fatalf("maxTurns round-trip mismatch: want %d, got %d", tc.maxTurns, decoded.MaxTurns)
			}
		})
	}
}
