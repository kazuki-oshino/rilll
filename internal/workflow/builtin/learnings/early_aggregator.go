package learnings

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

const (
	earlyAggregationMissingJobReason  = "job-row-absent"
	earlyAggregationWaitTimeoutReason = "wait-budget-exhausted"
	earlyAggregationFallbackTimeout   = "bounded-wait-timeout"
)

type EarlyAggregationInput struct {
	CWD         string
	ParentRunID string
	TailRunID   string
	RunDir      string
	WaitBudget  time.Duration
}

type EarlyAggregationResult struct {
	LearningsJSONPath       string
	ReflectorJSONPath       string
	EarlyReflectionJSONPath string
	IncludedAgentCallIDs    []string
	MissingTargets          []EarlyReflectionTargetSummary
}

func AggregateEarlyReflection(
	ctx context.Context,
	in EarlyAggregationInput,
	reader akashic.Reader,
	store akashic.LearningReflectionJobStore,
) (EarlyAggregationResult, error) {
	if store == nil {
		return EarlyAggregationResult{}, fmt.Errorf("learnings.AggregateEarlyReflection: job store is required")
	}
	if in.RunDir == "" {
		in.RunDir = filepath.Join(in.CWD, ".rilll", learningsWorkflowID, in.TailRunID)
	}

	targets, err := CollectReflectionTargets(ctx, PreFetchInput{
		CWD:         in.CWD,
		ParentRunID: in.ParentRunID,
	}, reader)
	if err != nil {
		return EarlyAggregationResult{}, fmt.Errorf("learnings.AggregateEarlyReflection: collect targets: %w", err)
	}

	targetIDs := earlyAggregationReflectionTargetIDs(targets)
	initialJobs, err := store.JobsByAgentCallIDs(ctx, targetIDs)
	if err != nil {
		return EarlyAggregationResult{}, fmt.Errorf("learnings.AggregateEarlyReflection: load jobs: %w", err)
	}
	initialUnfinished := unfinishedJobIDSet(initialJobs)

	jobs, err := waitForEarlyReflectionJobs(ctx, store, targetIDs, in.WaitBudget)
	if err != nil {
		return EarlyAggregationResult{}, err
	}
	staleIDs := unfinishedJobIDs(jobs)
	fallbackReason := ""
	if len(staleIDs) > 0 {
		fallbackReason = earlyAggregationFallbackTimeout
		if err := store.MarkLearningReflectionStale(ctx, staleIDs, earlyAggregationWaitTimeoutReason, time.Now().UTC()); err != nil {
			return EarlyAggregationResult{}, fmt.Errorf("learnings.AggregateEarlyReflection: mark stale: %w", err)
		}
		jobs, err = store.JobsByAgentCallIDs(ctx, targetIDs)
		if err != nil {
			return EarlyAggregationResult{}, fmt.Errorf("learnings.AggregateEarlyReflection: reload stale jobs: %w", err)
		}
	}

	build := newEarlyAggregationBuild(in.ParentRunID, initialUnfinished)
	if err := build.consume(ctx, targets, jobs, store); err != nil {
		return EarlyAggregationResult{}, err
	}

	reflectorPath, err := writeReflectorArtifact(in.RunDir, build.reflectorArtifact)
	if err != nil {
		return EarlyAggregationResult{}, fmt.Errorf("learnings.AggregateEarlyReflection: write reflector.json: %w", err)
	}
	learningsPath, err := writeLearningsArtifact(in.RunDir, build.learningsArtifact)
	if err != nil {
		return EarlyAggregationResult{}, fmt.Errorf("learnings.AggregateEarlyReflection: write learnings.json: %w", err)
	}
	earlyPath, err := WriteEarlyReflectionArtifact(ctx, in.RunDir, BuildEarlyReflectionArtifact(
		in.ParentRunID,
		in.TailRunID,
		fallbackReason,
		build.targetSummaries,
	))
	if err != nil {
		return EarlyAggregationResult{}, fmt.Errorf("learnings.AggregateEarlyReflection: write early-reflection.json: %w", err)
	}

	if len(build.aggregatedIDs) > 0 {
		if err := store.MarkLearningReflectionAggregated(ctx, build.aggregatedIDs, in.TailRunID, time.Now().UTC()); err != nil {
			return EarlyAggregationResult{}, fmt.Errorf("learnings.AggregateEarlyReflection: mark aggregated: %w", err)
		}
	}

	return EarlyAggregationResult{
		LearningsJSONPath:       learningsPath,
		ReflectorJSONPath:       reflectorPath,
		EarlyReflectionJSONPath: earlyPath,
		IncludedAgentCallIDs:    build.includedAgentCallIDs,
		MissingTargets:          build.missingTargets,
	}, nil
}

type earlyAggregationBuild struct {
	parentRunID          string
	initialUnfinished    map[string]struct{}
	seenLearningIDs      map[string]struct{}
	reflectorArtifact    ReflectorArtifact
	learningsArtifact    LearningsArtifact
	targetSummaries      []EarlyReflectionTargetSummary
	includedAgentCallIDs []string
	missingTargets       []EarlyReflectionTargetSummary
	aggregatedIDs        []string
}

func newEarlyAggregationBuild(parentRunID string, initialUnfinished map[string]struct{}) *earlyAggregationBuild {
	now := time.Now().UTC()
	return &earlyAggregationBuild{
		parentRunID:       parentRunID,
		initialUnfinished: initialUnfinished,
		seenLearningIDs:   map[string]struct{}{},
		reflectorArtifact: ReflectorArtifact{
			SchemaVersion:      reflectorSchemaVersion,
			ParentRunID:        parentRunID,
			GeneratedAt:        now,
			AllowedProviders:   []string{},
			PerSession:         []SessionResult{},
			PerSessionFailures: []SessionFailure{},
			Dropped:            []DroppedLearning{},
			SkippedProvider:    []SkippedProvider{},
		},
		learningsArtifact: LearningsArtifact{
			SchemaVersion: learningsArtifactSchemaVersion,
			ParentRunID:   parentRunID,
			GeneratedAt:   now,
			Learnings:     []LearningEntry{},
		},
		targetSummaries:      []EarlyReflectionTargetSummary{},
		includedAgentCallIDs: []string{},
		missingTargets:       []EarlyReflectionTargetSummary{},
		aggregatedIDs:        []string{},
	}
}

func (b *earlyAggregationBuild) consume(
	ctx context.Context,
	targets []ReflectionTarget,
	jobs []akashic.LearningReflectionJob,
	store akashic.LearningReflectionJobStore,
) error {
	jobsByID := learningReflectionJobsByAgentCallID(jobs)
	for _, target := range targets {
		job, ok := jobsByID[target.AgentCallID]
		if !ok {
			summary := earlyAggregationTargetSummary(target, EarlyReflectionTargetStatusMissingJob, earlyAggregationMissingJobReason, false)
			b.targetSummaries = append(b.targetSummaries, summary)
			b.missingTargets = append(b.missingTargets, summary)
			continue
		}
		if err := b.consumeJob(ctx, target, job, store); err != nil {
			return err
		}
	}
	return nil
}

func (b *earlyAggregationBuild) consumeJob(
	ctx context.Context,
	target ReflectionTarget,
	job akashic.LearningReflectionJob,
	store akashic.LearningReflectionJobStore,
) error {
	switch job.Status {
	case akashic.LearningReflectionStatusSucceeded:
		return b.consumeSucceededJob(ctx, target, job, store)
	case akashic.LearningReflectionStatusSkipped:
		b.targetSummaries = append(b.targetSummaries, earlyAggregationTargetSummary(
			target,
			EarlyReflectionTargetStatusSkipped,
			string(job.SkipReason),
			false,
		))
		b.aggregatedIDs = append(b.aggregatedIDs, job.AgentCallID)
	case akashic.LearningReflectionStatusFailed:
		b.consumeFailedJob(target, job)
		b.aggregatedIDs = append(b.aggregatedIDs, job.AgentCallID)
	case akashic.LearningReflectionStatusStale:
		b.targetSummaries = append(b.targetSummaries, earlyAggregationTargetSummary(
			target,
			EarlyReflectionTargetStatusStale,
			job.FailureReason,
			false,
		))
	case akashic.LearningReflectionStatusPending, akashic.LearningReflectionStatusRunning:
		b.targetSummaries = append(b.targetSummaries, earlyAggregationTargetSummary(
			target,
			EarlyReflectionTargetStatusPendingAtTimeout,
			earlyAggregationWaitTimeoutReason,
			false,
		))
	default:
		b.targetSummaries = append(b.targetSummaries, earlyAggregationTargetSummary(
			target,
			EarlyReflectionTargetStatus(job.Status),
			"",
			false,
		))
	}
	return nil
}

func (b *earlyAggregationBuild) consumeSucceededJob(
	ctx context.Context,
	target ReflectionTarget,
	job akashic.LearningReflectionJob,
	store akashic.LearningReflectionJobStore,
) error {
	payload, err := decodeLearningReflectionResultPayload(job)
	if err != nil {
		return err
	}
	status := EarlyReflectionTargetStatusSucceeded
	if _, ok := b.initialUnfinished[job.AgentCallID]; ok {
		status = EarlyReflectionTargetStatusWaitCompleted
	}
	b.targetSummaries = append(b.targetSummaries, earlyAggregationTargetSummary(target, status, "", true))
	b.includedAgentCallIDs = append(b.includedAgentCallIDs, job.AgentCallID)
	b.aggregatedIDs = append(b.aggregatedIDs, job.AgentCallID)

	b.reflectorArtifact.PerSession = append(b.reflectorArtifact.PerSession, SessionResult{
		SessionID:     firstNonEmpty(payload.SessionID, job.SessionID, target.SessionID),
		ProviderID:    firstNonEmpty(payload.ProviderID, job.ProviderID, target.ProviderID),
		StepID:        firstNonEmpty(payload.StepID, job.StepID, target.StepID),
		AcceptedCount: len(payload.Accepted),
		DroppedCount:  len(payload.Dropped),
	})
	b.reflectorArtifact.Dropped = append(b.reflectorArtifact.Dropped, payload.Dropped...)
	b.reflectorArtifact.PerSessionFailures = append(b.reflectorArtifact.PerSessionFailures, payload.Failures...)

	recordedAt := time.Now().UTC()
	for _, accepted := range payload.Accepted {
		row := akashic.LearningRow{
			ID:            accepted.ID,
			AgentCallID:   job.AgentCallID,
			WorkflowRunID: b.parentRunID,
			WorkflowID:    job.WorkflowID,
			StepID:        firstNonEmpty(payload.StepID, job.StepID, target.StepID),
			Category:      accepted.Category,
			Context:       accepted.Context,
			Insight:       accepted.Insight,
			PathsGlob:     append([]string(nil), accepted.PathsGlob...),
			Source:        akashic.SourceAgentSelfReport,
			RecordedAt:    recordedAt,
		}
		inserted, err := store.RecordLearningOnce(ctx, row)
		if err != nil {
			return fmt.Errorf("learnings.AggregateEarlyReflection: record learning once %s: %w", row.ID, err)
		}
		if !inserted {
			continue
		}
		if _, seen := b.seenLearningIDs[row.ID]; seen {
			continue
		}
		b.seenLearningIDs[row.ID] = struct{}{}
		b.learningsArtifact.Learnings = append(b.learningsArtifact.Learnings, LearningEntry{
			ID:         row.ID,
			SessionID:  firstNonEmpty(payload.SessionID, job.SessionID, target.SessionID),
			StepID:     row.StepID,
			ProviderID: firstNonEmpty(payload.ProviderID, job.ProviderID, target.ProviderID),
			Category:   row.Category,
			Context:    row.Context,
			Insight:    row.Insight,
			PathsGlob:  append([]string(nil), row.PathsGlob...),
			RecordedAt: recordedAt,
		})
	}
	return nil
}

func (b *earlyAggregationBuild) consumeFailedJob(target ReflectionTarget, job akashic.LearningReflectionJob) {
	reason := job.FailureReason
	b.targetSummaries = append(b.targetSummaries, earlyAggregationTargetSummary(
		target,
		EarlyReflectionTargetStatusFailed,
		reason,
		false,
	))
	if len(job.ResultJSON) > 0 {
		payload, err := decodeLearningReflectionResultPayload(job)
		if err == nil {
			b.reflectorArtifact.PerSessionFailures = append(b.reflectorArtifact.PerSessionFailures, payload.Failures...)
			b.reflectorArtifact.Dropped = append(b.reflectorArtifact.Dropped, payload.Dropped...)
			return
		}
	}
	if reason != "" {
		b.reflectorArtifact.PerSessionFailures = append(b.reflectorArtifact.PerSessionFailures, SessionFailure{
			SessionID:  firstNonEmpty(job.SessionID, target.SessionID),
			ProviderID: firstNonEmpty(job.ProviderID, target.ProviderID),
			Reason:     reason,
			Detail:     job.FailureDetail,
		})
	}
}

func waitForEarlyReflectionJobs(
	ctx context.Context,
	store akashic.LearningReflectionJobStore,
	agentCallIDs []string,
	waitBudget time.Duration,
) ([]akashic.LearningReflectionJob, error) {
	jobs, err := store.JobsByAgentCallIDs(ctx, agentCallIDs)
	if err != nil {
		return nil, fmt.Errorf("learnings.AggregateEarlyReflection: poll jobs: %w", err)
	}
	if len(unfinishedJobIDs(jobs)) == 0 || waitBudget <= 0 {
		return jobs, nil
	}

	pollInterval := 10 * time.Millisecond
	if waitBudget < pollInterval {
		pollInterval = waitBudget
	}
	timer := time.NewTimer(waitBudget)
	defer timer.Stop()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return jobs, ctx.Err()
		case <-timer.C:
			return jobs, nil
		case <-ticker.C:
			jobs, err = store.JobsByAgentCallIDs(ctx, agentCallIDs)
			if err != nil {
				return nil, fmt.Errorf("learnings.AggregateEarlyReflection: poll jobs: %w", err)
			}
			if len(unfinishedJobIDs(jobs)) == 0 {
				return jobs, nil
			}
		}
	}
}

func decodeLearningReflectionResultPayload(job akashic.LearningReflectionJob) (LearningReflectionResultPayload, error) {
	var payload LearningReflectionResultPayload
	if len(job.ResultJSON) == 0 {
		return LearningReflectionResultPayload{}, fmt.Errorf("learnings.AggregateEarlyReflection: empty result_json for job %s", job.AgentCallID)
	}
	if err := json.Unmarshal(job.ResultJSON, &payload); err != nil {
		return LearningReflectionResultPayload{}, fmt.Errorf("learnings.AggregateEarlyReflection: decode result_json for job %s: %w", job.AgentCallID, err)
	}
	if payload.Accepted == nil {
		payload.Accepted = []LearningReflectionAcceptedLearning{}
	}
	if payload.Dropped == nil {
		payload.Dropped = []DroppedLearning{}
	}
	if payload.Failures == nil {
		payload.Failures = []SessionFailure{}
	}
	return payload, nil
}

func earlyAggregationReflectionTargetIDs(targets []ReflectionTarget) []string {
	ids := make([]string, 0, len(targets))
	for _, target := range targets {
		ids = append(ids, target.AgentCallID)
	}
	return ids
}

func learningReflectionJobsByAgentCallID(jobs []akashic.LearningReflectionJob) map[string]akashic.LearningReflectionJob {
	out := make(map[string]akashic.LearningReflectionJob, len(jobs))
	for _, job := range jobs {
		out[job.AgentCallID] = job
	}
	return out
}

func unfinishedJobIDSet(jobs []akashic.LearningReflectionJob) map[string]struct{} {
	out := map[string]struct{}{}
	for _, id := range unfinishedJobIDs(jobs) {
		out[id] = struct{}{}
	}
	return out
}

func unfinishedJobIDs(jobs []akashic.LearningReflectionJob) []string {
	ids := []string{}
	for _, job := range jobs {
		switch job.Status {
		case akashic.LearningReflectionStatusPending, akashic.LearningReflectionStatusRunning:
			ids = append(ids, job.AgentCallID)
		}
	}
	return ids
}

func earlyAggregationTargetSummary(
	target ReflectionTarget,
	status EarlyReflectionTargetStatus,
	reason string,
	included bool,
) EarlyReflectionTargetSummary {
	return EarlyReflectionTargetSummary{
		AgentCallID:       target.AgentCallID,
		WorkflowRunID:     target.WorkflowRunID,
		SessionID:         target.SessionID,
		ProviderID:        target.ProviderID,
		StepID:            target.StepID,
		Status:            status,
		Reason:            reason,
		IncludedInCurator: included,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
