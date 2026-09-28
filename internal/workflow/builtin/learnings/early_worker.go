package learnings

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
)

func (r *EarlyReflectorRuntime) workerLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(r.config.PollInterval)
	defer ticker.Stop()

	r.drainAvailable()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-r.wakeCh:
			r.drainAvailable()
		case <-ticker.C:
			r.drainAvailable()
		}
	}
}

func (r *EarlyReflectorRuntime) drainAvailable() {
	for {
		if r.ctx.Err() != nil {
			return
		}
		claimed := r.processOne()
		if !claimed {
			return
		}
	}
}

func (r *EarlyReflectorRuntime) processOne() bool {
	if err := r.limiter.Acquire(r.ctx); err != nil {
		return false
	}
	defer r.limiter.Release()

	now := r.now()
	job, ok, err := r.store.ClaimLearningReflectionJob(r.ctx, r.config.ProjectID, r.config.WorkerID, now)
	if err != nil || !ok {
		return false
	}
	if job.ProjectID != "" && job.ProjectID != r.config.ProjectID {
		failure := akashic.LearningReflectionFailure{
			Reason:    "project-id-mismatch",
			Detail:    fmt.Sprintf("claimed project_id=%q runtime project_id=%q", job.ProjectID, r.config.ProjectID),
			Retryable: false,
		}
		_ = r.store.MarkLearningReflectionFailed(context.WithoutCancel(r.ctx), job.AgentCallID, failure, r.now())
		return true
	}

	result, reflectErr := ReflectAgentCallSnapshot(r.ctx, ReflectAgentCallSnapshotInput{
		Job: job,
		CWD: r.cwd,
	}, r.agent)
	if reflectErr != nil {
		failure := learningReflectionFailureFromError(reflectErr)
		failure.ResultJSON = result.JSON
		_ = r.store.MarkLearningReflectionFailed(context.WithoutCancel(r.ctx), job.AgentCallID, failure, r.now())
		return true
	}
	_ = r.store.MarkLearningReflectionSucceeded(context.WithoutCancel(r.ctx), job.AgentCallID, result, r.now())
	return true
}

func learningReflectionFailureFromError(err error) akashic.LearningReflectionFailure {
	var execErr *ReflectAgentCallSnapshotError
	if errors.As(err, &execErr) && execErr.Failure.Reason != "" {
		return execErr.Failure
	}
	return akashic.LearningReflectionFailure{
		Reason:    failureReasonCallAgent,
		Detail:    err.Error(),
		Retryable: true,
	}
}
