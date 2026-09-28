package learnings

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflow"
)

const (
	earlyReflectionArtifactFilename      = "early-reflection.json"
	earlyReflectionArtifactSchemaVersion = 1
)

type EarlyReflectionTargetStatus string

const (
	EarlyReflectionTargetStatusSucceeded        EarlyReflectionTargetStatus = "succeeded"
	EarlyReflectionTargetStatusWaitCompleted    EarlyReflectionTargetStatus = "wait-completed"
	EarlyReflectionTargetStatusSkipped          EarlyReflectionTargetStatus = "skipped"
	EarlyReflectionTargetStatusFailed           EarlyReflectionTargetStatus = "failed"
	EarlyReflectionTargetStatusStale            EarlyReflectionTargetStatus = "stale"
	EarlyReflectionTargetStatusMissingJob       EarlyReflectionTargetStatus = "missing-job"
	EarlyReflectionTargetStatusPendingAtTimeout EarlyReflectionTargetStatus = "pending-at-timeout"
)

type EarlyReflectionSummary struct {
	TotalTargets     int    `json:"total_targets"`
	Included         int    `json:"included"`
	WaitCompleted    int    `json:"wait_completed"`
	Skipped          int    `json:"skipped"`
	Failed           int    `json:"failed"`
	Stale            int    `json:"stale"`
	MissingJob       int    `json:"missing_job"`
	PendingAtTimeout int    `json:"pending_at_timeout"`
	FallbackReason   string `json:"fallback_reason"`
}

type EarlyReflectionTargetSummary struct {
	AgentCallID       string                      `json:"agent_call_id"`
	WorkflowRunID     string                      `json:"workflow_run_id"`
	SessionID         string                      `json:"session_id"`
	ProviderID        string                      `json:"provider_id"`
	StepID            string                      `json:"step_id"`
	Status            EarlyReflectionTargetStatus `json:"status"`
	Reason            string                      `json:"reason"`
	IncludedInCurator bool                        `json:"included_in_curator"`
}

type EarlyReflectionArtifact struct {
	SchemaVersion int                            `json:"schema_version"`
	ParentRunID   string                         `json:"parent_run_id"`
	TailRunID     string                         `json:"tail_run_id"`
	GeneratedAt   time.Time                      `json:"generated_at"`
	Summary       EarlyReflectionSummary         `json:"summary"`
	Targets       []EarlyReflectionTargetSummary `json:"targets"`
}

func BuildEarlyReflectionArtifact(parentRunID, tailRunID, fallbackReason string, targets []EarlyReflectionTargetSummary) EarlyReflectionArtifact {
	if targets == nil {
		targets = []EarlyReflectionTargetSummary{}
	}
	artifact := EarlyReflectionArtifact{
		SchemaVersion: earlyReflectionArtifactSchemaVersion,
		ParentRunID:   parentRunID,
		TailRunID:     tailRunID,
		GeneratedAt:   time.Now().UTC(),
		Targets:       append([]EarlyReflectionTargetSummary(nil), targets...),
	}
	artifact.RefreshSummary(fallbackReason)
	return artifact
}

func (a *EarlyReflectionArtifact) RefreshSummary(fallbackReason string) {
	if a == nil {
		return
	}
	summary := EarlyReflectionSummary{
		TotalTargets:   len(a.Targets),
		FallbackReason: fallbackReason,
	}
	for _, target := range a.Targets {
		if target.IncludedInCurator {
			summary.Included++
		}
		switch target.Status {
		case EarlyReflectionTargetStatusWaitCompleted:
			summary.WaitCompleted++
		case EarlyReflectionTargetStatusSkipped:
			summary.Skipped++
		case EarlyReflectionTargetStatusFailed:
			summary.Failed++
		case EarlyReflectionTargetStatusStale:
			summary.Stale++
		case EarlyReflectionTargetStatusMissingJob:
			summary.MissingJob++
		case EarlyReflectionTargetStatusPendingAtTimeout:
			summary.PendingAtTimeout++
		}
	}
	a.Summary = summary
	if a.Targets == nil {
		a.Targets = []EarlyReflectionTargetSummary{}
	}
}

func WriteEarlyReflectionArtifact(ctx context.Context, runDir string, artifact EarlyReflectionArtifact) (string, error) {
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir runDir %q: %w", runDir, err)
	}
	if artifact.Targets == nil {
		artifact.Targets = []EarlyReflectionTargetSummary{}
	}
	path := filepath.Join(runDir, earlyReflectionArtifactFilename)
	if err := writeJSONAtomic(path, artifact); err != nil {
		return "", err
	}
	if ctx != nil {
		_ = workflow.RecordRunArtifact(ctx, akashic.ArtifactKindContext, path)
	}
	return path, nil
}
