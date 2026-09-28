package learnings

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/kazuki-oshino/rilll/internal/akashic"
	"github.com/kazuki-oshino/rilll/internal/workflow"
	"github.com/kazuki-oshino/rilll/internal/workflowctx"
)

const (
	defaultEarlyReflectorPollInterval = 1 * time.Second
	defaultEarlyReflectorShutdownWait = 2 * time.Second
)

// EarlyReflectorRuntimeConfig は opt-in workflow 実行中だけ動く early reflector worker の設定。
type EarlyReflectorRuntimeConfig struct {
	ProjectID    string
	WorkerID     string
	MaxParallel  int
	PollInterval time.Duration
}

// EarlyReflectorRuntime は durable reflection job を process-local worker で drain する。
// source of truth は Akashic job store であり、runtime は wake / polling / shutdown を担う。
type EarlyReflectorRuntime struct {
	cwd    string
	store  akashic.LearningReflectionJobStore
	agent  workflowctx.AgentCaller
	config EarlyReflectorRuntimeConfig

	ctx    context.Context
	cancel context.CancelFunc

	limiter *ParallelLimiter
	wakeCh  chan struct{}
	done    chan struct{}
	wg      sync.WaitGroup
	once    sync.Once
	now     func() time.Time
}

// StartWorkflowScopedEarlyReflectorRuntime extracts the optional job store from agent and
// starts the runtime only when this wrapper run has a usable project scope.
func StartWorkflowScopedEarlyReflectorRuntime(
	ctx context.Context,
	cwd string,
	agent workflowctx.AgentCaller,
	config EarlyReflectorRuntimeConfig,
) (*EarlyReflectorRuntime, bool, error) {
	recorder := workflow.RecorderFromAgent(agent)
	store, ok := recorder.(akashic.LearningReflectionJobStore)
	if !ok || store == nil {
		return nil, false, nil
	}
	if config.ProjectID == "" {
		config.ProjectID = akashic.ProjectIDFromCWD(cwd)
	}
	if config.ProjectID == "" || config.ProjectID == akashic.UnknownProjectID {
		return nil, false, nil
	}
	runtime, err := StartEarlyReflectorRuntime(ctx, cwd, store, agent, config)
	if err != nil {
		return nil, false, err
	}
	return runtime, true, nil
}

// StartEarlyReflectorRuntime starts a project-scoped worker pool. It is intentionally not
// invoked from package init or CLI startup; callers own the workflow-scoped lifecycle.
func StartEarlyReflectorRuntime(
	ctx context.Context,
	cwd string,
	store akashic.LearningReflectionJobStore,
	agent workflowctx.AgentCaller,
	config EarlyReflectorRuntimeConfig,
) (*EarlyReflectorRuntime, error) {
	if store == nil {
		return nil, fmt.Errorf("early reflector runtime: job store is required")
	}
	if config.ProjectID == "" {
		config.ProjectID = akashic.ProjectIDFromCWD(cwd)
	}
	if config.ProjectID == "" || config.ProjectID == akashic.UnknownProjectID {
		return nil, fmt.Errorf("early reflector runtime: usable project_id is required")
	}
	if config.WorkerID == "" {
		config.WorkerID = fmt.Sprintf("early-reflector-%d", os.Getpid())
	}
	if config.MaxParallel < 1 {
		config.MaxParallel = NewLimiter(envRillLearningsMaxParallel, defaultMaxParallel).Limit()
	}
	if config.PollInterval <= 0 {
		config.PollInterval = defaultEarlyReflectorPollInterval
	}

	// Worker calls must not inherit the front-run early reflection marker; otherwise
	// reflector resume calls could recursively enqueue themselves. Parent cancellation
	// is still bridged into the dedicated runtime context below.
	runtimeCtx, cancel := context.WithCancel(context.Background())
	r := &EarlyReflectorRuntime{
		cwd:     cwd,
		store:   store,
		agent:   agent,
		config:  config,
		ctx:     runtimeCtx,
		cancel:  cancel,
		limiter: NewLimiter("", config.MaxParallel),
		wakeCh:  make(chan struct{}, 1),
		done:    make(chan struct{}),
		now:     func() time.Time { return time.Now().UTC() },
	}
	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				cancel()
			case <-runtimeCtx.Done():
			}
		}()
	}
	r.start()
	return r, nil
}

func (r *EarlyReflectorRuntime) start() {
	for i := 0; i < r.config.MaxParallel; i++ {
		r.wg.Add(1)
		go r.workerLoop()
	}
	go func() {
		r.wg.Wait()
		close(r.done)
	}()
}

// MaxParallel returns the resolved worker concurrency for tests and observability seams.
func (r *EarlyReflectorRuntime) MaxParallel() int {
	if r == nil {
		return 0
	}
	return r.config.MaxParallel
}

// Notify wakes the runtime without blocking the caller.
func (r *EarlyReflectorRuntime) Notify() {
	if r == nil {
		return
	}
	select {
	case r.wakeCh <- struct{}{}:
	default:
	}
}

// Shutdown cancels workers and waits briefly for in-flight calls to observe cancellation.
func (r *EarlyReflectorRuntime) Shutdown() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.cancel()
		select {
		case <-r.done:
		case <-time.After(defaultEarlyReflectorShutdownWait):
		}
	})
}
