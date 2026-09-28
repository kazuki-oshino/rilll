package learnings

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/provider"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

func TestReflectAgentCallSnapshotAcceptedAndDropped(t *testing.T) {
	// early worker は enqueue 時点の provider snapshot を信頼し、env allowlist を再読込しない。
	// ここで codex を env から外しても job.ProviderID=codex は reflection される。
	t.Setenv(envRillLearningsProviders, "claude")
	agent := &fakeReflectorAgent{
		responses: map[string]provider.AgentResponse{
			"session-1": {Content: `{"learnings":[
				{"category":"failure-recovery","context":"ctx accepted","insight":"ins accepted","paths_glob":["a/**"]},
				{"category":"not-a-category","context":"ctx dropped","insight":"ins dropped","paths_glob":["b/**"]}
			]}`},
		},
		checkOpts: func(opts *workflowctx.CallAgentOptions) {
			if opts == nil || opts.OutputFormat == nil {
				t.Fatalf("OutputFormat was not set")
			}
			if opts.StepID != stepIDReflector {
				t.Fatalf("StepID = %q, want %q", opts.StepID, stepIDReflector)
			}
		},
	}

	got, err := ReflectAgentCallSnapshot(context.Background(), ReflectAgentCallSnapshotInput{
		Job: akashic.LearningReflectionJob{
			AgentCallID:   "call-1",
			WorkflowRunID: "run-1",
			WorkflowID:    "workflow-1",
			StepID:        "implement",
			ProviderID:    "codex",
			SessionID:     "session-1",
			CWD:           t.TempDir(),
		},
	}, agent)
	if err != nil {
		t.Fatalf("ReflectAgentCallSnapshot: %v", err)
	}
	payload := decodeReflectionJobResultT(t, got.JSON)
	if payload.AgentCallID != "call-1" || payload.SessionID != "session-1" || payload.ProviderID != "codex" || payload.StepID != "implement" {
		t.Fatalf("snapshot provenance mismatch: %+v", payload)
	}
	if len(payload.Accepted) != 1 {
		t.Fatalf("accepted len = %d, want 1; payload=%s", len(payload.Accepted), got.JSON)
	}
	accepted := payload.Accepted[0]
	wantID := "learning-8c4a8b929ecc7fa83205bcd124390467"
	if accepted.ID != wantID {
		t.Fatalf("accepted ID = %q, want %q", accepted.ID, wantID)
	}
	if accepted.Category != "failure-recovery" || accepted.Context != "ctx accepted" || accepted.Insight != "ins accepted" {
		t.Fatalf("accepted content mismatch: %+v", accepted)
	}
	if len(payload.Dropped) != 1 {
		t.Fatalf("dropped len = %d, want 1; payload=%s", len(payload.Dropped), got.JSON)
	}
	if payload.Dropped[0].Reason != dropReasonInvalidCategory {
		t.Fatalf("drop reason = %q, want %q", payload.Dropped[0].Reason, dropReasonInvalidCategory)
	}
	if len(payload.Failures) != 0 {
		t.Fatalf("failures len = %d, want 0; payload=%s", len(payload.Failures), got.JSON)
	}

	agent.mu.Lock()
	calls := append([]string(nil), agent.calls...)
	prompts := append([]string(nil), agent.gotPrompts...)
	agent.mu.Unlock()
	if len(calls) != 1 || calls[0] != "session-1" {
		t.Fatalf("CallAgent resume calls = %v, want [session-1]", calls)
	}
	if len(prompts) != 1 || !strings.HasPrefix(prompts[0], reflectorSkillSlash+" "+reflectorMode+" ") {
		t.Fatalf("prompt = %q, want existing reflector slash command", prompts)
	}
}

func TestReflectAgentCallSnapshotFailuresAreReturnedInResultJSON(t *testing.T) {
	cases := []struct {
		name       string
		responses  map[string]provider.AgentResponse
		errors     map[string]error
		wantReason string
	}{
		{
			name: "parse failure",
			responses: map[string]provider.AgentResponse{
				"session-1": {Content: "plain text, not json"},
			},
			wantReason: failureReasonParseJSON,
		},
		{
			name: "no credentials",
			errors: map[string]error{
				"session-1": errors.New("provider auth: no credentials configured"),
			},
			wantReason: failureReasonNoCredentials,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := &fakeReflectorAgent{responses: tc.responses, errors: tc.errors}
			got, err := ReflectAgentCallSnapshot(context.Background(), ReflectAgentCallSnapshotInput{
				Job: akashic.LearningReflectionJob{
					AgentCallID:   "call-1",
					WorkflowRunID: "run-1",
					StepID:        "implement",
					ProviderID:    "codex",
					SessionID:     "session-1",
					CWD:           t.TempDir(),
				},
			}, agent)
			if err == nil {
				t.Fatalf("ReflectAgentCallSnapshot error = nil, want failure")
			}
			var execErr *ReflectAgentCallSnapshotError
			if !errors.As(err, &execErr) {
				t.Fatalf("error type = %T, want *ReflectAgentCallSnapshotError", err)
			}
			if execErr.Failure.Reason != tc.wantReason {
				t.Fatalf("failure reason = %q, want %q", execErr.Failure.Reason, tc.wantReason)
			}
			payload := decodeReflectionJobResultT(t, got.JSON)
			if len(payload.Accepted) != 0 || len(payload.Dropped) != 0 {
				t.Fatalf("accepted/dropped should be empty on failure: %+v", payload)
			}
			if len(payload.Failures) != 1 {
				t.Fatalf("failures len = %d, want 1; payload=%s", len(payload.Failures), got.JSON)
			}
			if payload.Failures[0].Reason != tc.wantReason {
				t.Fatalf("payload failure reason = %q, want %q", payload.Failures[0].Reason, tc.wantReason)
			}
		})
	}
}

func TestReflectAgentCallSnapshotDoesNotWriteArtifactsOrLearnings(t *testing.T) {
	dir := t.TempDir()
	agent := &fakeReflectorAgent{
		responses: map[string]provider.AgentResponse{
			"session-1": {Content: `{"learnings":[{"category":"tool-pattern","context":"ctx","insight":"ins","paths_glob":["x/**"]}]}`},
		},
	}

	got, err := ReflectAgentCallSnapshot(context.Background(), ReflectAgentCallSnapshotInput{
		Job: akashic.LearningReflectionJob{
			AgentCallID:   "call-1",
			WorkflowRunID: "run-1",
			StepID:        "implement",
			ProviderID:    "codex",
			SessionID:     "session-1",
			CWD:           dir,
		},
	}, agent)
	if err != nil {
		t.Fatalf("ReflectAgentCallSnapshot: %v", err)
	}
	if len(got.JSON) == 0 {
		t.Fatalf("result JSON empty")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("single-session reflector wrote files in cwd: %v", entries)
	}
}

func decodeReflectionJobResultT(t *testing.T, raw json.RawMessage) LearningReflectionResultPayload {
	t.Helper()
	var payload LearningReflectionResultPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal result JSON: %v\nbody=%s", err, raw)
	}
	return payload
}
