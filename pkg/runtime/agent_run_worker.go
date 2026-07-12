package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type TurnRunnerBinding struct {
	Runner            TurnRunner
	DeploymentID      string
	DefinitionID      string
	DefinitionVersion string
	ModelProvider     string
	Model             string
	ModelActions      []capability.ModelAction
	InputContextRefs  []string
	BudgetReservation BudgetUsage
}

type TurnRunnerResolver interface {
	ResolveTurnRunner(ctx context.Context, run *AgentRun) (*TurnRunnerBinding, error)
}

type TurnRunnerResolverFunc func(context.Context, *AgentRun) (*TurnRunnerBinding, error)

func (f TurnRunnerResolverFunc) ResolveTurnRunner(ctx context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
	return f(ctx, run)
}

type AgentRunWorkerConfig struct {
	Scope                      Scope
	Kind                       RunKind
	AssignedAgentID            string
	WorkerIDPrefix             string
	Concurrency                int
	MaxActiveForAgent          int
	MaxActiveForConcurrencyKey int
	MaxTurnsPerClaim           int
	LeaseDuration              time.Duration
	TurnLeaseDuration          time.Duration
	AgingInterval              time.Duration
	PollInterval               time.Duration
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
		c.LeaseDuration = 30 * time.Second
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
	if strings.TrimSpace(c.WorkerIDPrefix) == "" {
		c.WorkerIDPrefix = "agent-run-worker"
	}
	return nil
}

// AgentRunWorkerPool autonomously claims and advances canonical Runs. The
// durable store is authoritative; the wake channel is only a latency hint.
type AgentRunWorkerPool struct {
	config        AgentRunWorkerConfig
	scheduler     *AgentRunScheduler
	portfolio     PortfolioStore
	coordinator   *TurnCoordinator
	wakeService   *AgentRunWakeService
	activity      *RunActivityService
	actions       *ActionCoordinator
	forks         *RunForkCoordinator
	collaboration *CollaborationService
	resolver      TurnRunnerResolver
	logger        *zap.SugaredLogger
	wake          chan struct{}
	poolID        string
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	startOnce     sync.Once
	stopOnce      sync.Once
}

// SetActionCoordinator enables atomic materialization of one proposal-only
// hosted action into the governed ActionCall lifecycle after its Turn commits.
func (p *AgentRunWorkerPool) SetActionCoordinator(actions *ActionCoordinator) {
	p.actions = actions
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
	pool := &AgentRunWorkerPool{
		config: config, scheduler: NewAgentRunScheduler(store), portfolio: store, coordinator: NewTurnCoordinator(store, store, store),
		wakeService: NewAgentRunWakeService(store, store), activity: NewRunActivityService(store, store),
		resolver: resolver, logger: logger, wake: make(chan struct{}, 1),
		poolID: strings.TrimSpace(config.WorkerIDPrefix) + "-" + uuid.NewString(),
	}
	if forkStore, ok := store.(RunForkStore); ok {
		pool.forks = NewRunForkCoordinator(forkStore)
	}
	if collaborationStore, ok := store.(CollaborationKernelStore); ok {
		pool.collaboration = NewCollaborationService(collaborationStore)
	}
	return pool, nil
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
			MaxActiveForAgent:          p.config.MaxActiveForAgent,
			MaxActiveForConcurrencyKey: p.config.MaxActiveForConcurrencyKey,
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
		failed, _, transitionErr := p.activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusFailed, Error: err.Error(), LeaseOwner: workerID,
			Summary: "Agent run failed during runner resolution", Actor: ActivityActor{Type: "worker", ID: workerID},
		})
		if transitionErr != nil {
			p.logger.Errorw("failed to persist runner resolution failure", "runId", run.ID, "error", transitionErr)
		} else {
			p.resolveCollaborationChild(ctx, failed)
		}
		return
	}
	current := run
	for turnIndex := 0; turnIndex < p.config.MaxTurnsPerClaim; turnIndex++ {
		advanceCtx, cancelAdvance := context.WithCancel(ctx)
		heartbeatDone := make(chan error, 1)
		go p.heartbeatRunLease(advanceCtx, cancelAdvance, workerID, current, heartbeatDone)
		result, advanceErr := p.coordinator.Advance(advanceCtx, AdvanceAgentRunRequest{
			Scope: current.Scope, RunID: current.ID, WorkerID: workerID, LeaseDuration: p.config.TurnLeaseDuration,
			DefinitionID: binding.DefinitionID, DefinitionVersion: binding.DefinitionVersion,
			ModelProvider: binding.ModelProvider, Model: binding.Model, InputContextRefs: binding.InputContextRefs,
			BudgetReservation: binding.BudgetReservation,
		}, binding.Runner)
		cancelAdvance()
		heartbeatErr := <-heartbeatDone
		if heartbeatErr != nil && (advanceErr == nil || errors.Is(heartbeatErr, ErrLeaseLost)) {
			advanceErr = heartbeatErr
		}
		if result != nil && result.Run != nil {
			current = result.Run
		}
		if advanceErr != nil {
			p.logger.Warnw("agent turn returned an error", "runId", run.ID, "error", advanceErr)
		}
		if result != nil && result.Turn != nil && len(result.Turn.RequestedActions) > 0 {
			materialized, materializeErr := p.materializeTurnAction(ctx, workerID, current, result.Turn, binding)
			if materializeErr != nil {
				p.failMaterialization(ctx, workerID, current, result.Turn, materializeErr)
				return
			}
			current = materialized
			return
		}
		if result != nil && result.Turn != nil && result.Turn.RequestedFork != nil {
			materialized, materializeErr := p.materializeTurnFork(ctx, workerID, current, result.Turn)
			if materializeErr != nil {
				p.failMaterialization(ctx, workerID, current, result.Turn, materializeErr)
				return
			}
			current = materialized
			p.Wake()
			return
		}
		if result != nil && result.Turn != nil && result.Turn.RequestedDelegation != nil {
			materialized, materializeErr := p.materializeTurnDelegation(ctx, workerID, current, result.Turn)
			if materializeErr != nil {
				p.failMaterialization(ctx, workerID, current, result.Turn, materializeErr)
				return
			}
			current = materialized
			p.Wake()
			return
		}
		if current != nil && isTerminalAgentRunStatus(current.Status) {
			p.resolveForkChild(ctx, current)
			p.resolveCollaborationChild(ctx, current)
			return
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

func (p *AgentRunWorkerPool) resolveCollaborationChild(ctx context.Context, run *AgentRun) {
	if p.collaboration == nil || run == nil || !isTerminalAgentRunStatus(run.Status) {
		return
	}
	_, err := p.collaboration.ResolveTerminalAgentRequestChild(ctx, run)
	if err != nil && !errors.Is(err, ErrRevisionConflict) && !errors.Is(err, ErrInvalidAgentRequestState) {
		p.logger.Warnw("failed to resolve collaboration request from terminal child", "runId", run.ID, "error", err)
	}
}

func (p *AgentRunWorkerPool) materializeTurnDelegation(ctx context.Context, workerID string, run *AgentRun, turn *AgentTurn) (*AgentRun, error) {
	if p.forks == nil {
		return nil, errors.New("durable delegation materialization is unavailable")
	}
	if run == nil || turn == nil || turn.RequestedDelegation == nil || len(turn.RequestedActions) != 0 || turn.RequestedFork != nil {
		return nil, errors.New("a bounded Turn must request exactly one delegation")
	}
	proposal := turn.RequestedDelegation
	result, err := p.forks.Create(ctx, CreateRunForkRequest{
		Scope: run.Scope, SourceRunID: run.ID, ExpectedSourceRevision: run.Revision, WorkerID: workerID,
		ForkID: proposal.StepID,
		Policy: RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
		Branches: []RunForkBranch{{
			ID: "delegate", Goal: proposal.Goal, AssignedAgentID: proposal.AssignedAgentID,
			Context: proposal.Context, Checkpoint: proposal.Checkpoint, Budget: proposal.Budget, Timeout: proposal.Timeout,
			Mode: proposal.Mode,
		}},
		ContinuationCheckpoint: turn.ContinuationCheckpoint,
		Actor:                  ActivityActor{Type: "worker", ID: workerID},
	})
	if err != nil {
		return nil, err
	}
	if result == nil || result.DependencyGroup == nil || result.DependencyGroup.Source == nil {
		return nil, errors.New("delegation materialization returned no durable source Run")
	}
	return result.DependencyGroup.Source, nil
}

func (p *AgentRunWorkerPool) resolveForkChild(ctx context.Context, run *AgentRun) {
	if p.forks == nil || run == nil || run.ParentRunID == "" || run.Checkpoint["forkChild"] == nil {
		return
	}
	if _, err := p.forks.CompleteChild(ctx, run, ActivityActor{Type: "worker", ID: p.poolID}); err != nil && !errors.Is(err, ErrDependencyConflict) {
		p.logger.Warnw("failed to resolve terminal fork child", "runId", run.ID, "error", err)
	}
}

func (p *AgentRunWorkerPool) materializeTurnFork(ctx context.Context, workerID string, run *AgentRun, turn *AgentTurn) (*AgentRun, error) {
	if p.forks == nil {
		return nil, errors.New("durable fork materialization is unavailable")
	}
	if run == nil || turn == nil || turn.RequestedFork == nil || len(turn.RequestedActions) != 0 {
		return nil, errors.New("a bounded Turn must request exactly one fork without actions")
	}
	result, err := p.forks.Create(ctx, CreateRunForkRequest{
		Scope: run.Scope, SourceRunID: run.ID, ExpectedSourceRevision: run.Revision, WorkerID: workerID,
		ForkID: turn.RequestedFork.ForkID, Policy: turn.RequestedFork.Policy, Branches: turn.RequestedFork.Branches,
		ContinuationCheckpoint: turn.ContinuationCheckpoint,
		Actor:                  ActivityActor{Type: "worker", ID: workerID},
	})
	if err != nil {
		return nil, err
	}
	if result == nil || result.DependencyGroup == nil || result.DependencyGroup.Source == nil {
		return nil, errors.New("fork materialization returned no durable source Run")
	}
	return result.DependencyGroup.Source, nil
}

func (p *AgentRunWorkerPool) materializeTurnAction(ctx context.Context, workerID string, run *AgentRun, turn *AgentTurn, binding *TurnRunnerBinding) (*AgentRun, error) {
	if p.actions == nil {
		return nil, errors.New("governed action materialization is unavailable")
	}
	if run == nil || turn == nil || binding == nil || len(turn.RequestedActions) != 1 {
		return nil, errors.New("a bounded Turn must request exactly one action at a time")
	}
	request := turn.RequestedActions[0]
	if request.Type != "skill_action" || strings.TrimSpace(request.Capability) == "" || strings.TrimSpace(request.Summary) == "" {
		return nil, errors.New("requested action requires type skill_action, capability, and summary")
	}
	var selected *capability.ModelAction
	for index := range binding.ModelActions {
		if binding.ModelActions[index].Name == request.Capability {
			selected = &binding.ModelActions[index]
			break
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("requested capability %q is not authorized", request.Capability)
	}
	arguments, err := resolveTurnActionInput(turn.ContinuationCheckpoint, request.InputRef)
	if err != nil {
		return nil, err
	}
	idempotencyKey := strings.TrimSpace(request.IdempotencyKey)
	if idempotencyKey == "" {
		idempotencyKey = fmt.Sprintf("turn:%s:action:0", turn.ID)
	}
	proposal, err := p.actions.Propose(ctx, ProposeActionRequest{
		Scope: run.Scope, RunID: run.ID, TurnID: turn.ID, WorkerID: workerID, DeploymentID: binding.DeploymentID,
		SkillID: selected.SkillID, SkillVersion: selected.Version, Action: selected.Action, Arguments: arguments,
		IdempotencyKey: idempotencyKey, Summary: request.Summary,
		Actor: ActivityActor{Type: "worker", ID: workerID}, ContinuationCheckpoint: turn.ContinuationCheckpoint,
		CausationID: turn.ID,
	})
	if err != nil {
		return nil, err
	}
	if proposal == nil || proposal.Run == nil {
		return nil, errors.New("governed action proposal returned no durable Run")
	}
	return proposal.Run, nil
}

func (p *AgentRunWorkerPool) failMaterialization(ctx context.Context, workerID string, run *AgentRun, turn *AgentTurn, cause error) {
	if run == nil {
		return
	}
	failed, _, err := p.activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusFailed, LeaseOwner: workerID,
		Error: "governed action materialization failed", Summary: "Agent action proposal could not be governed",
		EventType: "action.materialization_failed", Actor: ActivityActor{Type: "worker", ID: workerID},
		TurnID: turn.ID, CausationID: turn.ID, Payload: map[string]interface{}{"reason": cause.Error()},
	})
	if err != nil {
		p.logger.Errorw("failed to persist action materialization failure", "runId", run.ID, "error", err)
	} else {
		p.resolveCollaborationChild(ctx, failed)
	}
}

func (p *AgentRunWorkerPool) heartbeatRunLease(ctx context.Context, cancel context.CancelFunc, workerID string, run *AgentRun, done chan<- error) {
	interval := p.config.LeaseDuration / 3
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			done <- nil
			return
		case <-ticker.C:
			if _, err := p.scheduler.RenewLease(ctx, run.Scope, run.ID, workerID, p.config.LeaseDuration); err != nil {
				if ctx.Err() != nil {
					done <- nil
					return
				}
				cancel()
				done <- err
				return
			}
		}
	}
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
			p.reconcileForkChildren(ctx)
		}
	}
}

func (p *AgentRunWorkerPool) reconcileForkChildren(ctx context.Context) {
	if p.forks == nil {
		return
	}
	runs, err := p.portfolio.ListAgentRuns(ctx, AgentRunFilter{
		Scope: p.config.Scope, Statuses: []AgentRunStatus{AgentRunStatusCompleted, AgentRunStatusFailed, AgentRunStatusCanceled}, Limit: 100,
	})
	if err != nil {
		p.logger.Warnw("failed to list terminal fork children", "error", err)
		return
	}
	for _, run := range runs {
		p.resolveForkChild(ctx, run)
	}
}
