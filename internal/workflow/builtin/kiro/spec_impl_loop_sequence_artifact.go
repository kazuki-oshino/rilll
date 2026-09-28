package kiro

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflow"
)

const (
	kiroSpecImplLoopSequenceID    = "kiro-spec-impl-loop-sequence"
	sequenceArtifactFileName      = "sequence.json"
	sequenceItemOutcomeSucceeded  = "succeeded"
	sequenceArtifactSchemaVersion = 1
	sequenceFailureDeliveryFailed = "delivery_failed"
	sequenceFailureChildReturned  = "child_returned_error"
	sequenceFailureChildWorkflow  = "child_workflow_error"
)

type SequenceStatus string

const (
	SequenceStatusRunning   SequenceStatus = "running"
	SequenceStatusCompleted SequenceStatus = "completed"
	SequenceStatusFailed    SequenceStatus = "failed"
)

type SequenceItemStatus string

const (
	SequenceItemPending   SequenceItemStatus = "pending"
	SequenceItemRunning   SequenceItemStatus = "running"
	SequenceItemSucceeded SequenceItemStatus = "succeeded"
	SequenceItemFailed    SequenceItemStatus = "failed"
	SequenceItemUnstarted SequenceItemStatus = "unstarted"
)

type SequenceArtifact struct {
	SchemaVersion    int              `json:"schemaVersion"`
	WorkflowID       string           `json:"workflowId"`
	RunID            string           `json:"runId"`
	Status           SequenceStatus   `json:"status"`
	DuplicatePolicy  string           `json:"duplicatePolicy"`
	SelectedFeatures []string         `json:"selectedFeatures"`
	CurrentIndex     *int             `json:"currentIndex,omitempty"`
	CurrentFeature   string           `json:"currentFeature,omitempty"`
	Items            []SequenceItem   `json:"items"`
	Failure          *SequenceFailure `json:"failure,omitempty"`
	StartedAt        time.Time        `json:"startedAt"`
	UpdatedAt        time.Time        `json:"updatedAt"`
	FinishedAt       *time.Time       `json:"finishedAt,omitempty"`
}

type SequenceItem struct {
	Index           int                `json:"index"`
	Feature         string             `json:"feature"`
	Status          SequenceItemStatus `json:"status"`
	ChildWorkflowID string             `json:"childWorkflowId,omitempty"`
	ChildRunID      string             `json:"childRunId,omitempty"`
	Outcome         string             `json:"outcome,omitempty"`
	ErrorMessage    string             `json:"errorMessage,omitempty"`
	UnstartedReason string             `json:"unstartedReason,omitempty"`
	StartedAt       *time.Time         `json:"startedAt,omitempty"`
	FinishedAt      *time.Time         `json:"finishedAt,omitempty"`
}

type SequenceFailure struct {
	Kind              string   `json:"kind"`
	Message           string   `json:"message"`
	Index             int      `json:"index"`
	Feature           string   `json:"feature"`
	UnstartedFeatures []string `json:"unstartedFeatures"`
}

func NewKiroSpecImplLoopSequenceArtifact(runID string, selectedFeatures []string, startedAt time.Time) SequenceArtifact {
	startedAt = startedAt.UTC()
	selected := append([]string(nil), selectedFeatures...)
	items := make([]SequenceItem, 0, len(selected))
	for i, feature := range selected {
		items = append(items, SequenceItem{
			Index:   i,
			Feature: feature,
			Status:  SequenceItemPending,
		})
	}
	return SequenceArtifact{
		SchemaVersion:    sequenceArtifactSchemaVersion,
		WorkflowID:       kiroSpecImplLoopSequenceID,
		RunID:            runID,
		Status:           SequenceStatusRunning,
		DuplicatePolicy:  string(workflow.DuplicatePolicyReject),
		SelectedFeatures: selected,
		Items:            items,
		StartedAt:        startedAt,
		UpdatedAt:        startedAt,
	}
}

func (a *SequenceArtifact) MarkItemRunning(index int, childWorkflowID string, now time.Time) {
	item := a.sequenceItem(index)
	if item == nil {
		return
	}
	now = now.UTC()
	a.Status = SequenceStatusRunning
	a.CurrentIndex = intPtr(index)
	a.CurrentFeature = item.Feature
	a.UpdatedAt = now
	a.FinishedAt = nil
	item.Status = SequenceItemRunning
	item.ChildWorkflowID = childWorkflowID
	item.UnstartedReason = ""
	if item.StartedAt == nil {
		item.StartedAt = timePtr(now)
	}
}

func (a *SequenceArtifact) MarkItemSucceeded(index int, childWorkflowID, childRunID string, now time.Time) {
	item := a.sequenceItem(index)
	if item == nil {
		return
	}
	now = now.UTC()
	a.Status = SequenceStatusRunning
	a.CurrentIndex = intPtr(index)
	a.CurrentFeature = item.Feature
	a.UpdatedAt = now
	item.Status = SequenceItemSucceeded
	item.ChildWorkflowID = childWorkflowID
	item.ChildRunID = childRunID
	item.Outcome = sequenceItemOutcomeSucceeded
	item.ErrorMessage = ""
	item.UnstartedReason = ""
	if item.StartedAt == nil {
		item.StartedAt = timePtr(now)
	}
	item.FinishedAt = timePtr(now)
}

func (a *SequenceArtifact) MarkItemFailed(index int, childWorkflowID, childRunID, kind, message string, now time.Time) {
	item := a.sequenceItem(index)
	if item == nil {
		return
	}
	now = now.UTC()
	a.Status = SequenceStatusFailed
	a.CurrentIndex = intPtr(index)
	a.CurrentFeature = item.Feature
	a.UpdatedAt = now
	a.FinishedAt = timePtr(now)
	item.Status = SequenceItemFailed
	item.ChildWorkflowID = childWorkflowID
	item.ChildRunID = childRunID
	item.Outcome = kind
	item.ErrorMessage = message
	item.UnstartedReason = ""
	if item.StartedAt == nil {
		item.StartedAt = timePtr(now)
	}
	item.FinishedAt = timePtr(now)

	unstarted := a.markUnstartedTail(index+1, kind)
	a.Failure = &SequenceFailure{
		Kind:              kind,
		Message:           message,
		Index:             index,
		Feature:           item.Feature,
		UnstartedFeatures: unstarted,
	}
}

func (a *SequenceArtifact) MarkCompleted(now time.Time) {
	now = now.UTC()
	a.Status = SequenceStatusCompleted
	a.CurrentIndex = nil
	a.CurrentFeature = ""
	a.Failure = nil
	a.UpdatedAt = now
	a.FinishedAt = timePtr(now)
}

func (a *SequenceArtifact) sequenceItem(index int) *SequenceItem {
	if index < 0 || index >= len(a.Items) {
		return nil
	}
	return &a.Items[index]
}

func (a *SequenceArtifact) markUnstartedTail(start int, reason string) []string {
	if start >= len(a.Items) {
		return nil
	}
	unstarted := make([]string, 0, len(a.Items)-start)
	for i := start; i < len(a.Items); i++ {
		item := &a.Items[i]
		item.Status = SequenceItemUnstarted
		item.UnstartedReason = fmt.Sprintf("sequence stopped after %s", reason)
		item.ChildWorkflowID = ""
		item.ChildRunID = ""
		item.Outcome = ""
		item.ErrorMessage = ""
		item.StartedAt = nil
		item.FinishedAt = nil
		unstarted = append(unstarted, item.Feature)
	}
	return unstarted
}

type sequenceArtifactStore struct {
	cwd string
}

func newSequenceArtifactStore(cwd string) sequenceArtifactStore {
	return sequenceArtifactStore{cwd: cwd}
}

func (s sequenceArtifactStore) Path(runID string) string {
	return filepath.Join(s.cwd, ".rilll", kiroSpecImplLoopSequenceID, runID, sequenceArtifactFileName)
}

func (s sequenceArtifactStore) Save(ctx context.Context, artifact SequenceArtifact) (string, error) {
	path := s.Path(artifact.RunID)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("kiro-spec-impl-loop-sequence: write sequence.json: mkdir %s: %w", dir, err)
	}

	body, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return "", fmt.Errorf("kiro-spec-impl-loop-sequence: write sequence.json: marshal: %w", err)
	}
	body = append(body, '\n')
	if err := writeSequenceArtifactAtomic(path, body, 0o644); err != nil {
		return "", fmt.Errorf("kiro-spec-impl-loop-sequence: write sequence.json: %w", err)
	}

	_ = workflow.RecordRunArtifact(ctx, akashic.ArtifactKindResult, path)
	return path, nil
}

func writeSequenceArtifactAtomic(path string, body []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, perm); err != nil {
		return fmt.Errorf("write tmp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

func intPtr(v int) *int {
	return &v
}

func timePtr(t time.Time) *time.Time {
	t = t.UTC()
	return &t
}
