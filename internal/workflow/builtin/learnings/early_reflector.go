package learnings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

const learningReflectionResultSchemaVersion = 1

// ReflectAgentCallSnapshotInput は early worker が 1 job を reflection するための入力。
// Job は enqueue 時点の allowlist policy で許可済みの provider/session snapshot として扱う。
type ReflectAgentCallSnapshotInput struct {
	Job akashic.LearningReflectionJob
	CWD string
}

// LearningReflectionResultPayload は 1 session の reflection 結果として
// learning_reflection_jobs.result_json に保存する JSON payload。
type LearningReflectionResultPayload struct {
	SchemaVersion int                                  `json:"schema_version"`
	AgentCallID   string                               `json:"agent_call_id"`
	SessionID     string                               `json:"session_id"`
	ProviderID    string                               `json:"provider_id"`
	StepID        string                               `json:"step_id,omitempty"`
	Accepted      []LearningReflectionAcceptedLearning `json:"accepted"`
	Dropped       []DroppedLearning                    `json:"dropped"`
	Failures      []SessionFailure                     `json:"failures"`
}

// LearningReflectionAcceptedLearning は category validation を通過した reflector 候補。
// worker は learnings table に直接書かず、tail aggregation が LearningRow に変換して
// RecordLearningOnce を呼ぶ。
type LearningReflectionAcceptedLearning struct {
	ID        string   `json:"id"`
	Category  string   `json:"category"`
	Context   string   `json:"context"`
	Insight   string   `json:"insight"`
	PathsGlob []string `json:"paths_glob"`
}

// ReflectAgentCallSnapshotError は worker が job store に保存できる failure payload を保持する。
// 失敗時も result JSON は返すため、test / diagnostic は failures[] を確認できる。
type ReflectAgentCallSnapshotError struct {
	Failure akashic.LearningReflectionFailure
}

func (e *ReflectAgentCallSnapshotError) Error() string {
	if e == nil {
		return ""
	}
	if e.Failure.Detail == "" {
		return e.Failure.Reason
	}
	return e.Failure.Reason + ": " + e.Failure.Detail
}

// ReflectAgentCallSnapshot は enqueue 済み agent call snapshot 1 件に対し、
// 既存 Reflector prompt と schema を再利用して learning candidate JSON を作る。
// provider allowlist の再読込、Curator/Apply 起動、playbook 書き込み、
// RecordLearning 呼び出しは行わない。
func ReflectAgentCallSnapshot(
	ctx context.Context,
	in ReflectAgentCallSnapshotInput,
	agent workflowctx.AgentCaller,
) (akashic.LearningReflectionResult, error) {
	job := in.Job
	payload := LearningReflectionResultPayload{
		SchemaVersion: learningReflectionResultSchemaVersion,
		AgentCallID:   job.AgentCallID,
		SessionID:     job.SessionID,
		ProviderID:    job.ProviderID,
		StepID:        job.StepID,
		Accepted:      []LearningReflectionAcceptedLearning{},
		Dropped:       []DroppedLearning{},
		Failures:      []SessionFailure{},
	}

	if agent == nil {
		failure := akashic.LearningReflectionFailure{
			Reason:    failureReasonCallAgent,
			Detail:    "agent caller is nil",
			Retryable: true,
		}
		payload.Failures = append(payload.Failures, sessionFailureFromJob(job, failure))
		return marshalReflectionResult(payload), &ReflectAgentCallSnapshotError{Failure: failure}
	}
	if job.SessionID == "" {
		failure := akashic.LearningReflectionFailure{
			Reason:    string(akashic.LearningReflectionSkipEmptySessionID),
			Detail:    "session_id is required",
			Retryable: false,
		}
		payload.Failures = append(payload.Failures, sessionFailureFromJob(job, failure))
		return marshalReflectionResult(payload), &ReflectAgentCallSnapshotError{Failure: failure}
	}
	if job.ProviderID == "" {
		failure := akashic.LearningReflectionFailure{
			Reason:    "provider-id-empty",
			Detail:    "provider_id is required",
			Retryable: false,
		}
		payload.Failures = append(payload.Failures, sessionFailureFromJob(job, failure))
		return marshalReflectionResult(payload), &ReflectAgentCallSnapshotError{Failure: failure}
	}

	cwd := in.CWD
	if cwd == "" {
		cwd = job.CWD
	}
	prompt := buildReflectorPrompt("", singleSessionReflectorRunDir(cwd, job.WorkflowRunID))
	opts := workflow.WithStep(&workflowctx.CallAgentOptions{
		OutputFormat: JSONSchemaForReflector(),
	}, stepIDReflector, stepNameReflector)

	resp, err := agent.CallAgent(ctx, prompt, cwd, nil, job.SessionID, opts)
	if err != nil {
		failure := akashic.LearningReflectionFailure{
			Reason:    failureReasonCallAgent,
			Detail:    err.Error(),
			Retryable: true,
		}
		if isErrNoCredentials(err) {
			failure.Reason = failureReasonNoCredentials
			failure.Retryable = false
		}
		payload.Failures = append(payload.Failures, sessionFailureFromJob(job, failure))
		return marshalReflectionResult(payload), &ReflectAgentCallSnapshotError{Failure: failure}
	}

	var parsed reflectorResponse
	if _, ok := extractAgentResponseJSON(resp, &parsed); !ok {
		failure := akashic.LearningReflectionFailure{
			Reason:    failureReasonParseJSON,
			Detail:    truncateForArtifact(resp.Content, 1024),
			Retryable: true,
		}
		payload.Failures = append(payload.Failures, sessionFailureFromJob(job, failure))
		return marshalReflectionResult(payload), &ReflectAgentCallSnapshotError{Failure: failure}
	}

	for i, learning := range parsed.Learnings {
		if !ValidCategory(learning.Category) {
			payload.Dropped = append(payload.Dropped, DroppedLearning{
				SessionID: job.SessionID,
				Reason:    dropReasonInvalidCategory,
				Learning:  learning,
			})
			continue
		}
		learning.PathsGlob = normalizedPathsGlob(learning.PathsGlob)
		payload.Accepted = append(payload.Accepted, LearningReflectionAcceptedLearning{
			ID:        deterministicLearningID(job.AgentCallID, i, learning),
			Category:  learning.Category,
			Context:   learning.Context,
			Insight:   learning.Insight,
			PathsGlob: append([]string(nil), learning.PathsGlob...),
		})
	}

	return marshalReflectionResult(payload), nil
}

func sessionFailureFromJob(job akashic.LearningReflectionJob, failure akashic.LearningReflectionFailure) SessionFailure {
	return SessionFailure{
		SessionID:  job.SessionID,
		ProviderID: job.ProviderID,
		Reason:     failure.Reason,
		Detail:     failure.Detail,
	}
}

func marshalReflectionResult(payload LearningReflectionResultPayload) akashic.LearningReflectionResult {
	payload.Accepted = nonNilAccepted(payload.Accepted)
	if payload.Dropped == nil {
		payload.Dropped = []DroppedLearning{}
	}
	if payload.Failures == nil {
		payload.Failures = []SessionFailure{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte(`{"schema_version":1,"accepted":[],"dropped":[],"failures":[{"reason":"marshal-result-failed"}]}`)
	}
	return akashic.LearningReflectionResult{JSON: json.RawMessage(raw)}
}

func nonNilAccepted(in []LearningReflectionAcceptedLearning) []LearningReflectionAcceptedLearning {
	if in == nil {
		return []LearningReflectionAcceptedLearning{}
	}
	return in
}

func deterministicLearningID(agentCallID string, index int, learning reflectorLearning) string {
	pathsGlobJSON, err := json.Marshal(normalizedPathsGlob(learning.PathsGlob))
	if err != nil {
		pathsGlobJSON = []byte("[]")
	}
	input := fmt.Sprintf("%s\n%d\n%s\n%s\n%s\n%s", agentCallID, index, learning.Category, learning.Context, learning.Insight, string(pathsGlobJSON))
	sum := sha256.Sum256([]byte(input))
	return "learning-" + hex.EncodeToString(sum[:])[:32]
}

func normalizedPathsGlob(paths []string) []string {
	if paths == nil {
		return []string{}
	}
	return append([]string(nil), paths...)
}

func singleSessionReflectorRunDir(cwd, workflowRunID string) string {
	if workflowRunID == "" {
		workflowRunID = "early-reflection"
	}
	if cwd == "" {
		return filepath.Join(".rilll", "agent-learnings", workflowRunID)
	}
	return filepath.Join(cwd, ".rilll", "agent-learnings", workflowRunID)
}
