package cursor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/kazuki-oshino/rilll/internal/provider"
)

const (
	commandName                = "cursor-agent"
	maxBufferBytes             = 10 * 1024 * 1024
	maxObservationPayloadBytes = 8 * 1024 * 1024
	errorDetailMaxLength       = 400
	cursorObservationSource    = "cursor-cli"
)

type Provider struct {
	Command string
	runner  runnerFunc
}

type runnerFunc func(ctx context.Context, command string, args []string, cwd string) (string, string, error)

func New() *Provider {
	return &Provider{Command: commandName, runner: runCursorAgent}
}

func (p *Provider) ID() provider.ProviderID {
	return provider.ProviderCursor
}

func (p *Provider) Call(ctx context.Context, request provider.AgentRequest, onEvent provider.AgentEventCallback) (provider.AgentResponse, error) {
	command := p.Command
	if command == "" {
		command = commandName
	}
	runner := p.runner
	if runner == nil {
		runner = runCursorAgent
	}
	args := BuildArgs(request)
	observations := newCursorObservationEmitter(request.AkashicLevel, onEvent)
	observations.emit("execution", "", map[string]any{
		"command":           command,
		"cwd":               request.CWD,
		"resumeSessionId":   request.ResumeSessionID,
		"akashicLevel":      observations.level,
		"argsWithoutPrompt": argsWithoutPrompt(args),
	})

	startedAt := time.Now()
	stdout, stderr, err := runner(ctx, command, args, request.CWD)
	duration := time.Since(startedAt)
	observations.emitOutput(stdout, stderr)
	if err != nil {
		message := classifyExecutionError(err, stdout, stderr)
		observations.emit("error", "execution", resultObservationPayload(false, request.ResumeSessionID, request.CWD, stdout, stderr, duration, exitCodeValue(err), message))
		return failureResponse(message, request.ResumeSessionID, request.CWD), nil
	}

	parsed, err := ParseOutput(stdout)
	if err != nil {
		observations.emit("error", "parse", resultObservationPayload(false, request.ResumeSessionID, request.CWD, stdout, stderr, duration, exitCodeValue(nil), err.Error()))
		return failureResponse(err.Error(), request.ResumeSessionID, request.CWD), nil
	}

	sessionID := parsed.SessionID
	if sessionID == "" {
		sessionID = request.ResumeSessionID
	}
	systemInfo := extractSystemInfo(parsed.Payload, request, sessionID)

	var structuredOutput any
	if request.OutputFormat != nil && request.OutputFormat.Type == "json_schema" {
		structured, strategy, err := parseStructuredOutput(parsed.Content)
		if err != nil {
			observations.emit("error", "structured_output", resultObservationPayload(false, sessionID, request.CWD, stdout, stderr, duration, exitCodeValue(nil), err.Error(), systemInfo))
			return provider.AgentResponse{
				Content:    err.Error(),
				SessionID:  sessionID,
				Success:    false,
				SystemInfo: systemInfo,
			}, nil
		}
		structuredOutput = structured
		observations.emit("structured_output", strategy, map[string]any{
			"strategy": strategy,
			"bytes":    len(parsed.Content),
		})
	}

	observations.emit("result", "success", resultObservationPayload(true, sessionID, request.CWD, stdout, stderr, duration, exitCodeValue(nil), "", systemInfo))
	if onEvent != nil {
		onEvent(provider.AgentEvent{Type: provider.EventText, Text: parsed.Content})
		onEvent(provider.AgentEvent{Type: provider.EventDone, Content: parsed.Content, SessionID: sessionID})
	}

	return provider.AgentResponse{
		Content:          parsed.Content,
		SessionID:        sessionID,
		Success:          true,
		StructuredOutput: structuredOutput,
		SystemInfo:       systemInfo,
	}, nil
}

func runCursorAgent(ctx context.Context, command string, args []string, cwd string) (string, string, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()

	var stdout limitedBuffer
	var stderr limitedBuffer
	stdout.limit = maxBufferBytes
	stderr.limit = maxBufferBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

type cursorObservationEmitter struct {
	level    string
	sequence int64
	onEvent  provider.AgentEventCallback
}

func newCursorObservationEmitter(level string, onEvent provider.AgentEventCallback) *cursorObservationEmitter {
	return &cursorObservationEmitter{
		level:   normalizeAkashicLevel(level),
		onEvent: onEvent,
	}
}

func normalizeAkashicLevel(level string) string {
	switch level {
	case "summary", "full", "debug":
		return level
	default:
		return "summary"
	}
}

func (e *cursorObservationEmitter) emitOutput(stdout, stderr string) {
	if stdout != "" {
		e.emit("stdout", "", map[string]any{
			"text":  stdout,
			"bytes": len(stdout),
		})
	}
	if stderr != "" {
		e.emit("stderr", "", map[string]any{
			"text":  stderr,
			"bytes": len(stderr),
		})
	}
}

func (e *cursorObservationEmitter) emit(observationType string, observationSubtype string, payload map[string]any) {
	if e == nil || e.onEvent == nil || !e.shouldEmit(observationType) {
		return
	}
	body, truncated := marshalObservationPayload(payload)
	e.onEvent(provider.AgentEvent{
		Type:               provider.EventObservation,
		Source:             cursorObservationSource,
		ObservationType:    observationType,
		ObservationSubtype: observationSubtype,
		Sequence:           e.sequence,
		Payload:            body,
		PayloadTruncated:   truncated,
	})
	e.sequence++
}

func (e *cursorObservationEmitter) shouldEmit(observationType string) bool {
	switch e.level {
	case "debug":
		return true
	case "full":
		return observationType == "result" || observationType == "error" || observationType == "stdout" || observationType == "stderr" || observationType == "structured_output"
	default:
		return observationType == "result" || observationType == "error"
	}
}

func marshalObservationPayload(payload map[string]any) ([]byte, bool) {
	body, err := json.Marshal(payload)
	if err != nil {
		body, _ = json.Marshal(map[string]any{"message": err.Error()})
		return body, false
	}
	if len(body) <= maxObservationPayloadBytes {
		return body, false
	}
	body, _ = json.Marshal(map[string]any{
		"payloadTruncated": true,
		"originalBytes":    len(body),
	})
	return body, true
}

func resultObservationPayload(success bool, sessionID string, cwd string, stdout string, stderr string, duration time.Duration, exitCode *int, message string, systemInfo ...*provider.AgentSystemInfo) map[string]any {
	payload := map[string]any{
		"success":     success,
		"sessionId":   sessionID,
		"cwd":         cwd,
		"durationMs":  duration.Milliseconds(),
		"stdoutBytes": len(stdout),
		"stderrBytes": len(stderr),
	}
	if exitCode != nil {
		payload["exitCode"] = *exitCode
	}
	if message != "" {
		payload["message"] = message
	}
	addSystemInfoToPayload(payload, systemInfo...)
	return payload
}

func addSystemInfoToPayload(payload map[string]any, systemInfo ...*provider.AgentSystemInfo) {
	if len(systemInfo) == 0 || systemInfo[0] == nil {
		return
	}
	info := systemInfo[0]
	if info.Model != "" {
		payload["model"] = info.Model
	}
	if len(info.Tools) > 0 {
		payload["toolsCount"] = len(info.Tools)
	}
}

func exitCodeValue(err error) *int {
	if err == nil {
		code := 0
		return &code
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		return &code
	}
	return nil
}

func argsWithoutPrompt(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	withoutPrompt := args[:len(args)-1]
	out := make([]string, len(withoutPrompt))
	copy(out, withoutPrompt)
	return out
}

func BuildArgs(request provider.AgentRequest) []string {
	args := []string{
		"-p",
		"--trust",
		"--output-format",
		"json",
		"--workspace",
		request.CWD,
	}
	if request.ResumeSessionID != "" {
		args = append(args, "--resume", request.ResumeSessionID)
	}
	// review-workflow Task 1.3: AgentRequest.Model 非 zero-value のときのみ `--model` を追加し、
	// 既定挙動 (cursor-agent CLI の既定モデル) を維持する。
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	args = append(args, "--force", "--", buildPrompt(request))
	return args
}

func buildPrompt(request provider.AgentRequest) string {
	if request.OutputFormat == nil || request.OutputFormat.Type != "json_schema" {
		return request.Prompt
	}
	schema, _ := json.MarshalIndent(request.OutputFormat.Schema, "", "  ")
	return strings.Join([]string{
		request.Prompt,
		"",
		"---",
		"",
		"IMPORTANT: Reply with **only** a single JSON value that strictly conforms to the following JSON schema. Do not include any prose, markdown fences, or commentary outside the JSON value.",
		"",
		"JSON schema:",
		"```json",
		string(schema),
		"```",
	}, "\n")
}

type ParsedOutput struct {
	Content   string
	SessionID string
	Payload   any
}

func ParseOutput(stdout string) (ParsedOutput, error) {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return ParsedOutput{}, errors.New("cursor-agent returned empty output")
	}

	var payload any
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return ParsedOutput{}, fmt.Errorf("Failed to parse cursor-agent JSON output: %s", trimDetail(trimmed, "<empty>"))
	}

	content := extractContent(payload)
	if content == "" {
		return ParsedOutput{}, fmt.Errorf("Failed to extract assistant content from cursor-agent JSON output: %s", trimDetail(trimmed, "<empty>"))
	}

	return ParsedOutput{
		Content:   content,
		SessionID: extractSessionID(payload),
		Payload:   payload,
	}, nil
}

func ParseStructuredOutput(content string) (any, error) {
	out, _, err := parseStructuredOutput(content)
	return out, err
}

func parseStructuredOutput(content string) (any, string, error) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return nil, "", errors.New("Failed to parse structured output from cursor-agent response: response is empty")
	}
	if out, err := parseJSONValue(trimmed); err == nil {
		return out, "direct", nil
	}
	if fenced, ok := fencedJSONValue(trimmed); ok {
		if out, err := parseJSONValue(fenced); err == nil {
			return out, "fenced", nil
		}
	}
	if out, ok := lastValidFencedJSONValue(trimmed); ok {
		return out, "fenced", nil
	}
	if candidate, ok := extractJSONValueCandidate(trimmed); ok {
		if out, err := parseJSONValue(candidate); err == nil {
			return out, "extract", nil
		}
	}
	return nil, "", errors.New("Failed to parse structured output from cursor-agent response: expected a single JSON value")
}

func parseJSONValue(value string) (any, error) {
	var out any
	if err := json.Unmarshal([]byte(value), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func fencedJSONValue(trimmed string) (string, bool) {
	if !strings.HasPrefix(trimmed, "```") || !strings.HasSuffix(trimmed, "```") || len(trimmed) < 6 {
		return "", false
	}
	inner := strings.TrimSpace(trimmed[3 : len(trimmed)-3])
	return normalizeFencedJSONBody(inner)
}

func lastValidFencedJSONValue(text string) (any, bool) {
	remaining := text
	var last any
	found := false
	for {
		start := strings.Index(remaining, "```")
		if start < 0 {
			break
		}
		afterStart := remaining[start+3:]
		end := strings.Index(afterStart, "```")
		if end < 0 {
			break
		}
		if body, ok := normalizeFencedJSONBody(afterStart[:end]); ok {
			if out, err := parseJSONValue(body); err == nil {
				last = out
				found = true
			}
		}
		remaining = afterStart[end+3:]
	}
	return last, found
}

func normalizeFencedJSONBody(body string) (string, bool) {
	inner := strings.TrimSpace(body)
	if strings.HasPrefix(strings.ToLower(inner), "json") {
		inner = strings.TrimSpace(inner[len("json"):])
	}
	return inner, inner != ""
}

func extractJSONValueCandidate(text string) (string, bool) {
	for index, char := range text {
		if char != '{' && char != '[' {
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(text[index:]))
		var out any
		if err := decoder.Decode(&out); err == nil {
			end := index + int(decoder.InputOffset())
			return strings.TrimSpace(text[index:end]), true
		}
	}
	return "", false
}

func extractContent(payload any) string {
	switch v := payload.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if part := extractContent(item); part != "" {
				parts = append(parts, part)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		for _, key := range []string{"content", "text", "output", "result", "message"} {
			if text, ok := v[key].(string); ok && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
		for _, key := range []string{"data", "response", "payload"} {
			if text := extractContent(v[key]); text != "" {
				return text
			}
		}
	}
	return ""
}

func extractSessionID(payload any) string {
	root, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	records := []map[string]any{root}
	for _, key := range []string{"data", "payload", "response"} {
		if nested, ok := root[key].(map[string]any); ok {
			records = append(records, nested)
		}
	}
	for _, record := range records {
		for _, key := range []string{"sessionId", "session_id", "chatId", "chat_id"} {
			if value, ok := record[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func extractSystemInfo(payload any, request provider.AgentRequest, sessionID string) *provider.AgentSystemInfo {
	info := &provider.AgentSystemInfo{
		ProviderID: provider.ProviderCursor,
		SessionID:  sessionID,
		CWD:        request.CWD,
	}
	records := collectRecords(payload)
	if cwd := firstString(records, "cwd", "workspace", "workspacePath", "workspace_path"); cwd != "" {
		info.CWD = cwd
	}
	info.Tools = firstStringArray(records, "tools", "availableTools", "allowedTools", "tool_names")
	info.Model = firstString(records, "model", "modelName", "model_name")
	info.PermissionMode = firstString(records, "permissionMode", "permission_mode")
	info.SlashCommands = firstStringArray(records, "slash_commands", "slashCommands")
	info.OutputStyle = firstString(records, "output_style", "outputStyle")
	return info
}

func collectRecords(payload any) []map[string]any {
	root, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	records := []map[string]any{root}
	for _, key := range []string{"data", "payload", "response"} {
		if nested, ok := root[key].(map[string]any); ok {
			records = append(records, nested)
		}
	}
	return records
}

func firstString(records []map[string]any, keys ...string) string {
	for _, record := range records {
		for _, key := range keys {
			if value, ok := record[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func firstStringArray(records []map[string]any, keys ...string) []string {
	for _, record := range records {
		for _, key := range keys {
			values, ok := record[key].([]any)
			if !ok {
				continue
			}
			var out []string
			for _, value := range values {
				if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
					out = append(out, strings.TrimSpace(text))
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	return nil
}

func classifyExecutionError(err error, stdout, stderr string) string {
	if errors.Is(err, exec.ErrNotFound) {
		return "cursor-agent binary not found. Install Cursor Agent CLI and ensure `cursor-agent` is in PATH."
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		detail := trimDetail(stderr, trimDetail(stdout, err.Error()))
		return fmt.Sprintf("Cursor Agent CLI exited with code %d: %s", exitErr.ExitCode(), detail)
	}
	return err.Error()
}

func failureResponse(message, sessionID, cwd string) provider.AgentResponse {
	return provider.AgentResponse{
		Content:   message,
		SessionID: sessionID,
		Success:   false,
		SystemInfo: &provider.AgentSystemInfo{
			ProviderID: provider.ProviderCursor,
			SessionID:  sessionID,
			CWD:        cwd,
		},
	}
}

func trimDetail(value string, fallback string) string {
	normalized := strings.TrimSpace(value)
	if normalized == "" {
		normalized = fallback
	}
	if len(normalized) > errorDetailMaxLength {
		return normalized[:errorDetailMaxLength] + "..."
	}
	return normalized
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit > 0 && b.Len()+len(p) > b.limit {
		return 0, fmt.Errorf("cursor-agent output exceeded buffer limit")
	}
	return b.Buffer.Write(p)
}
