package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

type TurnRunnerBinding struct {
	Runner            TurnRunner
	DefinitionID      string
	DefinitionVersion string
	ModelProvider     string
	Model             string
	InputContextRefs  []string
}

type TurnRunnerResolver interface {
	ResolveTurnRunner(ctx context.Context, run *AgentRun) (*TurnRunnerBinding, error)
}

type TurnRunnerResolverFunc func(context.Context, *AgentRun) (*TurnRunnerBinding, error)

func (f TurnRunnerResolverFunc) ResolveTurnRunner(ctx context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
	return f(ctx, run)
}

type AgentRunWorkerConfig struct {
	Scope             Scope
	Kind              RunKind
	AssignedAgentID   string
	Concurrency       int
	MaxActiveForAgent int
	MaxTurnsPerClaim  int
	LeaseDuration     time.Duration
	TurnLeaseDuration time.Duration
	AgingInterval     time.Duration
	PollInterval      time.Duration
}

func (c *AgentRunWorkerConfig) applyDefaults() error {
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	if c.Kind != "" && !validRunKind(c.Kind) {
		return fmt.Errorf("unsupported run kind %q", c.Kind)
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 1
	}
	if c.MaxActiveForAgent <= 0 {
		c.MaxActiveForAgent = c.Concurrency
	}
	if c.MaxTurnsPerClaim <= 0 {
		c.MaxTurnsPerClaim = 1
	}
	if c.LeaseDuration <= 0 {
		c.LeaseDuration = 15 * time.Minute
	}
	if c.TurnLeaseDuration <= 0 {
		c.TurnLeaseDuration = c.LeaseDuration
	}
	if c.AgingInterval <= 0 {
		c.AgingInterval = time.Minute
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 500 * time.Millisecond
	}
	return nil
}

// AgentRunWorkerPool autonomously claims and advances canonical Runs. The
// durable store is authoritative; the wake channel is only a latency hint.
type AgentRunWorkerPool struct {
	config      AgentRunWorkerConfig
	scheduler   *AgentRunScheduler
	coordinator *TurnCoordinator
	wakeService *AgentRunWakeService
	activity    *RunActivityService
	resolver    TurnRunnerResolver
	logger      *zap.SugaredLogger
	wake        chan struct{}
	poolID      string
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	startOnce   sync.Once
	stopOnce    sync.Once
}

func NewAgentRunWorkerPool(store KernelStore, resolver TurnRunnerResolver, logger *zap.SugaredLogger, config AgentRunWorkerConfig) (*AgentRunWorkerPool, error) {
	if store == nil || resolver == nil {
		return nil, errors.New("kernel store and turn runner resolver are required")
	}
	if err := config.applyDefaults(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &AgentRunWorkerPool{
		config: config, scheduler: NewAgentRunScheduler(store), coordinator: NewTurnCoordinator(store, store, store),
		wakeService: NewAgentRunWakeService(store, store), activity: NewRunActivityService(store, store),
		resolver: resolver, logger: logger, wake: make(chan struct{}, 1), poolID: uuid.NewString(),
	}, nil
}

func (p *AgentRunWorkerPool) Start(ctx context.Context) {
	p.startOnce.Do(func() {
		workerCtx, cancel := context.WithCancel(ctx)
		p.cancel = cancel
		for i := 0; i < p.config.Concurrency; i++ {
			p.wg.Add(1)
			go p.worker(workerCtx, fmt.Sprintf("%s-%d", p.poolID, i))
		}
		p.wg.Add(1)
		go p.timerWakeLoop(workerCtx)
		p.Wake()
	})
}

func (p *AgentRunWorkerPool) Stop() {
	p.stopOnce.Do(func() {
		if p.cancel != nil {
			p.cancel()
		}
		p.wg.Wait()
	})
}

func (p *AgentRunWorkerPool) Wake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *AgentRunWorkerPool) worker(ctx context.Context, workerID string) {
	defer p.wg.Done()
	ticker := time.NewTicker(p.config.PollInterval)
	defer ticker.Stop()
	for {
		run, err := p.scheduler.ClaimNext(ctx, AgentRunClaimRequest{
			Scope: p.config.Scope, Kind: p.config.Kind, WorkerID: workerID, AssignedAgentID: p.config.AssignedAgentID,
			LeaseDuration: p.config.LeaseDuration, AgingInterval: p.config.AgingInterval,
			MaxActiveForAgent: p.config.MaxActiveForAgent,
		})
		if err != nil && ctx.Err() == nil {
			p.logger.Errorw("failed to claim agent run", "workerId", workerID, "error", err)
		}
		if run != nil {
			p.executeClaim(ctx, workerID, run)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-ticker.C:
		}
	}
}

func (p *AgentRunWorkerPool) executeClaim(ctx context.Context, workerID string, run *AgentRun) {
	_, _ = p.activity.AppendActivity(ctx, &ActivityEvent{
		Scope: run.Scope, RunID: run.ID, AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, TeamID: teamIDForRun(run),
		EventType: "run.claimed", Summary: "Run claimed by autonomous worker",
		Actor: ActivityActor{Type: "worker", ID: workerID}, Visibility: ActivityVisibilityScope,
	})
	binding, err := p.resolver.ResolveTurnRunner(ctx, cloneAgentRun(run))
	if err != nil || binding == nil || binding.Runner == nil {
		if err == nil {
			err = errors.New("turn runner resolver returned no runner")
		}
		_, _, transitionErr := p.activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusFailed, Error: err.Error(), LeaseOwner: workerID,
			Summary: "Agent run failed during runner resolution", Actor: ActivityActor{Type: "worker", ID: workerID},
		})
		if transitionErr != nil {
			p.logger.Errorw("failed to persist runner resolution failure", "runId", run.ID, "error", transitionErr)
		}
		return
	}
	current := run
	for turnIndex := 0; turnIndex < p.config.MaxTurnsPerClaim; turnIndex++ {
		result, advanceErr := p.coordinator.Advance(ctx, AdvanceAgentRunRequest{
			Scope: current.Scope, RunID: current.ID, WorkerID: workerID, LeaseDuration: p.config.TurnLeaseDuration,
			DefinitionID: binding.DefinitionID, DefinitionVersion: binding.DefinitionVersion,
			ModelProvider: binding.ModelProvider, Model: binding.Model, InputContextRefs: binding.InputContextRefs,
		}, binding.Runner)
		if result != nil && result.Run != nil {
			current = result.Run
		}
		if advanceErr != nil {
			p.logger.Warnw("agent turn returned an error", "runId", run.ID, "error", advanceErr)
		}
		if result == nil || current.Status != AgentRunStatusRunning {
			return
		}
	}
	_, _, err = p.activity.TransitionRun(ctx, current.Scope, current.ID, RunTransitionRequest{
		ExpectedRevision: current.Revision, Status: AgentRunStatusQueued, LeaseOwner: workerID,
		Summary: "Run yielded after its bounded turn slice", EventType: "run.yielded",
		Actor: ActivityActor{Type: "worker", ID: workerID},
	})
	if err != nil {
		p.logger.Warnw("failed to yield agent run", "runId", current.ID, "error", err)
	}
	p.Wake()
}

func (p *AgentRunWorkerPool) timerWakeLoop(ctx context.Context) {
	defer p.wg.Done()
	ticker := time.NewTicker(p.config.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			result, err := p.wakeService.WakeDueTimers(ctx, p.config.Scope, now)
			if err != nil {
				p.logger.Warnw("failed to wake due agent runs", "error", err)
				continue
			}
			if len(result.Runs) > 0 {
				p.Wake()
			}
		}
	}
}
