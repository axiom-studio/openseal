package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type TurnRunnerBinding struct {
	OutputPublisher                 TurnOutputPublisher
	Runner                          TurnRunner
	ProgressAcknowledgementRenderer RunProgressAcknowledgementRenderer
	DeploymentID                    string
	// ActionDeploymentID is the resource whose Skill bindings authorized the
	// projected actions. Team conversations keep the roster Agent as the Turn
	// identity while resolving governed mutations against the owning Team.
	ActionDeploymentID string
	DefinitionID       string
	DefinitionVersion  string
	ModelProvider      string
	Model              string
	ModelActions       []capability.ModelAction
	RunbookOperations  []HostedRunbookOperation
	PreparedRuntimes   []PreparedSkillRuntime
	InputContextRefs   []string
	BudgetReservation  BudgetUsage
}

// PreparedSkillRuntime binds one activation-time immutable runtime to the
// exact Skill binding revision exposed during a Turn. It stays outside the
// model action catalog and is attached only after a proposal selects that
// authorized binding.
type PreparedSkillRuntime struct {
	DeploymentID    string
	BindingID       string
	BindingRevision int64
	SkillID         string
	SkillVersion    string
	Runtime         skill.PreparedRuntime
}

type TurnRunnerResolver interface {
	ResolveTurnRunner(ctx context.Context, run *AgentRun) (*TurnRunnerBinding, error)
}

type TurnRunnerResolverFunc func(context.Context, *AgentRun) (*TurnRunnerBinding, error)

func (f TurnRunnerResolverFunc) ResolveTurnRunner(ctx context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
	return f(ctx, run)
}

// ActionProposalObserver projects a committed governed ActionCall into other
// durable kernel resources. Projection failures never roll back or redispatch
// the authoritative action; observers must be idempotent and repairable from
// the persisted proposal.
type ActionProposalObserver interface {
	ObserveActionProposal(context.Context, *AgentRun, *AgentTurn, *ActionProposalResult) error
}

type ActionProposalObserverFunc func(context.Context, *AgentRun, *AgentTurn, *ActionProposalResult) error

func (f ActionProposalObserverFunc) ObserveActionProposal(ctx context.Context, run *AgentRun, turn *AgentTurn, proposal *ActionProposalResult) error {
	return f(ctx, run, turn, proposal)
}

type AgentRunWorkerConfig struct {
	Scope                             Scope
	Kind                              RunKind
	AssignedAgentID                   string
	WorkerIDPrefix                    string
	Concurrency                       int
	MaxActiveForAgent                 int
	MaxActiveForOwner                 int
	MaxActiveForObjective             int
	MaxActiveForConcurrencyKey        int
	ConversationTaskForegroundReserve int
	MaxTurnsPerClaim                  int
	LeaseDuration                     time.Duration
	TurnLeaseDuration                 time.Duration
	AgingInterval                     time.Duration
	PollInterval                      time.Duration
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
	if err := validateConversationTaskForegroundReserve(c.ConversationTaskForegroundReserve, c.MaxActiveForAgent); err != nil {
		return err
	}
	if c.MaxActiveForOwner < 0 || c.MaxActiveForObjective < 0 || c.MaxActiveForConcurrencyKey < 0 {
		return errors.New("agent run portfolio concurrency limits cannot be negative")
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
	config               AgentRunWorkerConfig
	scheduler            *AgentRunScheduler
	portfolio            PortfolioStore
	coordinator          *TurnCoordinator
	wakeService          *AgentRunWakeService
	activity             *RunActivityService
	actions              *ActionCoordinator
	actionObserver       ActionProposalObserver
	runFinalizer         RunTerminalFinalizer
	runCommands          *RunCommandService
	lastFinalizationScan time.Time
	finalizationOffset   int
	dependencyCursor     string
	forks                *RunForkCoordinator
	collaboration        *CollaborationService
	conversationTasks    *ConversationTaskService
	continuations        *ConversationTaskContinuationReconciler
	requestInbox         *AgentRequestInboxReconciler
	reportingStore       ConversationStore
	resolver             TurnRunnerResolver
	logger               *zap.SugaredLogger
	wake                 chan struct{}
	maintenanceWake      *workerPollSignal
	poolID               string
	cancel               context.CancelFunc
	wg                   sync.WaitGroup
	startOnce            sync.Once
	stopOnce             sync.Once
	limiter              atomic.Pointer[WorkerLimiter]
}

// SetActionCoordinator enables atomic materialization of one proposal-only
// hosted action into the governed ActionCall lifecycle after its Turn commits.
func (p *AgentRunWorkerPool) SetActionCoordinator(actions *ActionCoordinator) {
	p.actions = actions
}

func (p *AgentRunWorkerPool) SetActionProposalObserver(observer ActionProposalObserver) {
	p.actionObserver = observer
}

func (p *AgentRunWorkerPool) SetRunTerminalFinalizer(finalizer RunTerminalFinalizer) {
	p.runFinalizer = finalizer
}

func (p *AgentRunWorkerPool) SetWorkerLimiter(limiter *WorkerLimiter) {
	p.limiter.Store(limiter)
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
		runCommands: NewRunCommandService(store),
		resolver:    resolver, logger: logger, wake: make(chan struct{}, 1), maintenanceWake: newWorkerPollSignal(),
		poolID: strings.TrimSpace(config.WorkerIDPrefix) + "-" + uuid.NewString(),
	}
	if forkStore, ok := store.(RunForkStore); ok {
		pool.forks = NewRunForkCoordinator(forkStore)
	}
	if collaborationStore, ok := store.(CollaborationKernelStore); ok {
		pool.collaboration = NewCollaborationService(collaborationStore)
	}
	if taskStore, ok := store.(ConversationTaskKernelStore); ok {
		pool.conversationTasks = NewConversationTaskService(taskStore)
	}
	if continuationStore, ok := store.(ConversationTaskContinuationStore); ok && (config.Kind == "" || config.Kind == RunKindConversation) {
		pool.continuations, _ = NewConversationTaskContinuationReconciler(continuationStore)
	}
	if inboxStore, ok := store.(AgentRequestInboxStore); ok {
		pool.requestInbox, _ = NewAgentRequestInboxReconciler(inboxStore)
	}
	if reportingStore, ok := store.(ConversationStore); ok {
		pool.reportingStore = reportingStore
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
		if p.continuations != nil {
			p.wg.Add(1)
			go p.conversationTaskDeadlineLoop(workerCtx)
		}
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
	p.maintenanceWake.notify()
	p.requestClaim()
}

func (p *AgentRunWorkerPool) requestClaim() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *AgentRunWorkerPool) WakeScope(scope Scope) {
	if scope == p.config.Scope {
		p.Wake()
	}
}

func (p *AgentRunWorkerPool) worker(ctx context.Context, workerID string) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		}
		release, acquireErr := p.limiter.Load().acquire(ctx)
		if acquireErr != nil {
			return
		}
		run, err := p.scheduler.ClaimNext(ctx, AgentRunClaimRequest{
			Scope: p.config.Scope, Kind: p.config.Kind, WorkerID: workerID, AssignedAgentID: p.config.AssignedAgentID,
			LeaseDuration: p.config.LeaseDuration, AgingInterval: p.config.AgingInterval,
			MaxActiveForAgent:                 p.config.MaxActiveForAgent,
			MaxActiveForOwner:                 p.config.MaxActiveForOwner,
			MaxActiveForObjective:             p.config.MaxActiveForObjective,
			MaxActiveForConcurrencyKey:        p.config.MaxActiveForConcurrencyKey,
			ConversationTaskForegroundReserve: p.config.ConversationTaskForegroundReserve,
		})
		if err != nil && ctx.Err() == nil {
			p.logger.Errorw("failed to claim agent run", "workerId", workerID, "error", err)
		}
		if run != nil {
			// Hand the next claim opportunity to another worker before this one
			// executes. This drains runnable work up to the configured concurrency
			// without making every idle worker poll durable storage independently.
			p.requestClaim()
			p.executeClaim(ctx, workerID, run)
			p.maintenanceWake.notify()
		}
		release()
	}
}

func (p *AgentRunWorkerPool) executeClaim(ctx context.Context, workerID string, run *AgentRun) {
	if run == nil {
		return
	}
	if run.Status == AgentRunStatusPaused && runAttemptBudgetAtLimit(run) {
		_, _ = p.activity.AppendActivity(ctx, &ActivityEvent{
			Scope: run.Scope, RunID: run.ID, AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, TeamID: teamIDForRun(run),
			EventType: "budget.exhausted", Summary: "Run paused before exceeding its autonomous attempt budget",
			Actor: ActivityActor{Type: "worker", ID: workerID}, Visibility: ActivityVisibilityScope,
			Payload: map[string]interface{}{"dimension": "attempts", "used": run.BudgetUsage.Attempts, "limit": run.Budget.MaxAttempts},
		})
		return
	}
	if run.Status != AgentRunStatusRunning {
		return
	}
	stopDeadline := p.observeConversationTaskDeadline(ctx, run)
	defer stopDeadline()
	_, _ = p.activity.AppendActivity(ctx, &ActivityEvent{
		Scope: run.Scope, RunID: run.ID, AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, TeamID: teamIDForRun(run),
		EventType: "run.claimed", Summary: "Run claimed by autonomous worker",
		Actor: ActivityActor{Type: "worker", ID: workerID}, Visibility: ActivityVisibilityScope,
	})
	// Resolution can itself involve slow hosted setup. Keep the existing claim
	// alive across that work so promotion never causes another worker to resolve
	// or invoke a second copy while the first one is still in flight.
	resolutionCtx, cancelResolution := context.WithCancel(ctx)
	resolutionHeartbeatDone := make(chan error, 1)
	go p.heartbeatRunLease(resolutionCtx, cancelResolution, workerID, run, resolutionHeartbeatDone)
	binding, err := p.resolver.ResolveTurnRunner(resolutionCtx, cloneAgentRun(run))
	cancelResolution()
	if heartbeatErr := <-resolutionHeartbeatDone; heartbeatErr != nil {
		p.logger.Warnw("agent Run lease was lost during runner resolution", "runId", run.ID, "error", heartbeatErr)
		return
	}
	if ctx.Err() != nil {
		return
	}
	if latest, refreshErr := p.portfolio.GetAgentRun(ctx, run.Scope, run.ID); refreshErr != nil {
		p.logger.Warnw("failed to refresh agent Run after runner resolution", "runId", run.ID, "error", refreshErr)
		return
	} else if latest == nil || latest.Status != AgentRunStatusRunning || latest.LeaseOwner != workerID {
		return
	} else {
		run = latest
	}
	if err != nil || binding == nil || binding.Runner == nil {
		if p.parkAcceptedRunRecovery(ctx, workerID, run, nil, err) {
			return
		}
		if p.parkSkillRuntimeMaintenance(ctx, workerID, run, nil, err) {
			return
		}
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
			p.finalizeTerminalRun(ctx, failed)
			p.projectTerminalReporting(ctx, failed)
			p.resolveCollaborationChild(ctx, failed)
		}
		return
	}
	resolvedBinding := *binding
	resolvedBinding.InputContextRefs = append([]string(nil), binding.InputContextRefs...)
	binding = &resolvedBinding
	if p.reportingStore != nil {
		binding.Runner = conversationWorkTurnRunner{inner: binding.Runner, runs: p.portfolio, conversations: p.reportingStore}
	}

	if ref := evidenceSnapshotInputContextRef(run.Context); ref != "" && !containsString(binding.InputContextRefs, ref) {
		binding.InputContextRefs = append(binding.InputContextRefs, ref)
	}
	current := run
	if recoveredTurn, recoveryErr := p.pendingAppliedTaskTurn(ctx, current); recoveryErr != nil {
		p.logger.Warnw("failed to recover an applied conversation task proposal", "runId", current.ID, "error", recoveryErr)
		return
	} else if recoveredTurn != nil {
		materialized, materializeErr := p.materializeTurnTask(ctx, workerID, current, recoveredTurn, binding)
		if materializeErr != nil {
			p.failMaterialization(ctx, workerID, current, recoveredTurn, materializeErr)
			return
		}
		current = materialized
		if isTerminalAgentRunStatus(current.Status) {
			p.finalizeTerminalRun(ctx, current)
			p.projectTerminalReporting(ctx, current)
			p.resolveForkChild(ctx, current)
			p.resolveCollaborationChild(ctx, current)
			p.Wake()
			return
		}
	}
	for turnIndex := 0; turnIndex < p.config.MaxTurnsPerClaim; turnIndex++ {
		advanceCtx, cancelAdvance := context.WithCancel(ctx)
		heartbeatDone := make(chan error, 1)
		go p.heartbeatRunLease(advanceCtx, cancelAdvance, workerID, current, heartbeatDone)
		result, advanceErr := p.coordinator.Advance(advanceCtx, AdvanceAgentRunRequest{
			Scope: current.Scope, RunID: current.ID, WorkerID: workerID, LeaseDuration: p.config.TurnLeaseDuration,
			DefinitionID: binding.DefinitionID, DefinitionVersion: binding.DefinitionVersion,
			ModelProvider: binding.ModelProvider, Model: binding.Model, InputContextRefs: binding.InputContextRefs,
			BudgetReservation: binding.BudgetReservation,
			OutputPublisher:   binding.OutputPublisher,
		}, preparedRuntimeTurnRunner{binding: binding})
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
		if result != nil && result.Turn != nil && result.Turn.RequestedTask != nil {
			materialized, materializeErr := p.materializeTurnTask(ctx, workerID, current, result.Turn, binding)
			if materializeErr != nil {
				p.failMaterialization(ctx, workerID, current, result.Turn, materializeErr)
				return
			}
			current = materialized
			if isTerminalAgentRunStatus(current.Status) {
				p.finalizeTerminalRun(ctx, current)
				p.projectTerminalReporting(ctx, current)
				p.resolveForkChild(ctx, current)
				p.resolveCollaborationChild(ctx, current)
				p.Wake()
				return
			}
			// A deadline may have already promoted this same Run. Consume the
			// redundant proposal and continue its existing Turn lineage.
			continue
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
		if result != nil && result.Turn != nil && result.Turn.RequestedRunbook != nil {
			materialized, materializeErr := p.materializeTurnRunbook(ctx, workerID, current, result.Turn, binding)
			if materializeErr != nil {
				p.failMaterialization(ctx, workerID, current, result.Turn, materializeErr)
				return
			}
			current = materialized
			p.Wake()
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
			p.finalizeTerminalRun(ctx, current)
			p.projectTerminalReporting(ctx, current)
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

func (p *AgentRunWorkerPool) pendingAppliedTaskTurn(ctx context.Context, run *AgentRun) (*AgentTurn, error) {
	if p == nil || p.coordinator == nil || run == nil || run.Kind != RunKindConversation || run.Status != AgentRunStatusRunning || run.LastAppliedTurn < 1 {
		return nil, nil
	}
	turns, err := p.coordinator.turns.turns.ListAgentTurns(ctx, AgentTurnFilter{
		Scope: run.Scope, RunID: run.ID, AfterSequence: run.LastAppliedTurn - 1, Limit: 1,
	})
	if err != nil {
		return nil, err
	}
	if len(turns) != 1 || turns[0] == nil || turns[0].Sequence != run.LastAppliedTurn || turns[0].Status != AgentTurnStatusCompleted ||
		turns[0].RequestedTask == nil || !slices.Equal(turns[0].InputInterventionIDs, interventionIDs(run)) {
		return nil, nil
	}
	return turns[0], nil
}

func (p *AgentRunWorkerPool) materializeTurnTask(ctx context.Context, workerID string, run *AgentRun, turn *AgentTurn, binding *TurnRunnerBinding) (*AgentRun, error) {
	if p == nil || p.conversationTasks == nil {
		return nil, errors.New("durable conversation task admission is unavailable")
	}
	if run == nil || turn == nil || binding == nil || turn.RequestedTask == nil ||
		len(turn.RequestedActions) != 0 || turn.RequestedRunbook != nil || turn.RequestedFork != nil || turn.RequestedDelegation != nil {
		return nil, errors.New("a bounded Turn must request exactly one conversation task")
	}
	proposal := turn.RequestedTask
	if err := validateTaskProposalOutput(proposal, turn.NextRunStatus, turn.WakeCondition, turn.OutputSummary, turn.RunOutput, turn.RunError); err != nil {
		return nil, err
	}
	canonical, err := p.portfolio.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil {
		return nil, err
	}
	if canonical == nil {
		return nil, ErrRunNotFound
	}
	if task, proofErr := canonicalConversationTaskContinuation(ctx, p.portfolio, canonical); proofErr != nil {
		return nil, proofErr
	} else if task != nil {
		return canonical, nil
	}
	result, err := p.conversationTasks.Start(ctx, StartConversationTaskRequest{
		Scope: run.Scope, SourceRunID: run.ID, ExpectedSourceRevision: run.Revision,
		WorkerID: workerID, TurnID: turn.ID, AssignedAgentID: binding.DeploymentID,
		TaskKey: proposal.TaskKey, Goal: proposal.Goal, Acknowledgment: proposal.Acknowledgment,
		Budget: cloneBudgetPolicy(proposal.Budget),
	})
	if err != nil {
		return nil, err
	}
	if result != nil && result.Task != nil && result.WorkRun != nil && result.Task.Mode == ConversationTaskModeContinuation && ConversationTaskMatchesWorkRun(result.Task, result.WorkRun) {
		return result.WorkRun, nil
	}
	if result == nil || result.Task == nil || result.WorkRun == nil || result.SourceRun == nil || result.SourceRun.Status != AgentRunStatusCompleted {
		return nil, errors.New("conversation task admission returned no committed foreground completion")
	}
	return result.SourceRun, nil
}

func (p *AgentRunWorkerPool) materializeTurnRunbook(ctx context.Context, workerID string, run *AgentRun, turn *AgentTurn, binding *TurnRunnerBinding) (*AgentRun, error) {
	if p.forks == nil {
		return nil, errors.New("durable runbook materialization is unavailable")
	}
	if run == nil || turn == nil || binding == nil || turn.RequestedRunbook == nil ||
		len(turn.RequestedActions) != 0 || turn.RequestedFork != nil || turn.RequestedDelegation != nil || turn.RequestedTask != nil {
		return nil, errors.New("a bounded Turn must request exactly one runbook")
	}
	proposal := turn.RequestedRunbook
	var authorized *HostedRunbookOperation
	for _, operation := range binding.RunbookOperations {
		if operation.Entrypoint == proposal.Entrypoint {
			resolved := operation
			authorized = &resolved
			break
		}
	}
	if authorized == nil {
		return nil, fmt.Errorf("requested runbook entrypoint %q is not authorized", proposal.Entrypoint)
	}
	if err := p.rejectRecursiveRunbookInvocation(ctx, run, *authorized); err != nil {
		return nil, err
	}
	// A callable Runbook is an already reviewed operation. Its execution budget
	// is therefore owned by the kernel and the invoking Run, not selected by the
	// conversational model. An unbounded invoking Run starts an unbounded
	// operation; an explicitly bounded invoking Run contributes all remaining
	// capacity. Keeping the model-proposed slice here previously created tiny
	// child ceilings (commonly three attempts), which then stranded a valid
	// delegated step before its hosted minimum could be admitted.
	var operationBudget *BudgetPolicy
	if run.Budget != nil {
		var budgetErr error
		operationBudget, budgetErr = completeChildBudgetAllocation(run, &BudgetPolicy{})
		if budgetErr != nil {
			return nil, fmt.Errorf("resolve callable Runbook budget: %w", budgetErr)
		}
	}
	operationContext := cloneMap(proposal.Arguments)
	if operationContext == nil {
		operationContext = make(map[string]interface{})
	}
	operationContext[RunbookInvocationContextKey] = map[string]interface{}{
		"summary":   strings.TrimSpace(proposal.Summary),
		"arguments": cloneMap(proposal.Arguments),
	}
	sourceMessage, err := runbookSourceMessage(ctx, p.reportingStore, run)
	if err != nil {
		return nil, err
	}
	if sourceMessage != nil {
		operationContext[RunbookInvocationContextKey].(map[string]interface{})["sourceMessage"] = sourceMessage
	}
	if strings.TrimSpace(authorized.DefinitionID) != "" && strings.TrimSpace(authorized.DefinitionVersion) != "" {
		operationContext["runbookDefinitionId"] = strings.TrimSpace(authorized.DefinitionID)
		operationContext["runbookDefinitionVersion"] = strings.TrimSpace(authorized.DefinitionVersion)
		operationContext["runbookEntrypoint"] = strings.TrimSpace(authorized.Entrypoint)
	}
	result, err := p.forks.Create(ctx, CreateRunForkRequest{
		Scope: run.Scope, SourceRunID: run.ID, ExpectedSourceRevision: run.Revision, WorkerID: workerID,
		ForkID: "runbook-" + proposal.Entrypoint,
		Policy: RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
		Branches: []RunForkBranch{{
			ID: "operation", Goal: proposal.Summary, AssignedAgentID: run.AssignedAgentID,
			Entrypoint: proposal.Entrypoint, Context: operationContext, Checkpoint: map[string]interface{}{},
			Budget: operationBudget,
		}},
		ContinuationCheckpoint: turn.ContinuationCheckpoint,
		Actor:                  ActivityActor{Type: "worker", ID: workerID},
	})
	if err != nil {
		return nil, err
	}
	if result == nil || result.DependencyGroup == nil || result.DependencyGroup.Source == nil {
		return nil, errors.New("runbook materialization returned no durable source Run")
	}
	if len(result.Children) != 1 || result.Children[0] == nil {
		return nil, errors.New("runbook materialization returned no durable child Run")
	}
	if err := projectConversationRunbookStart(ctx, p.reportingStore, run, proposal, result.Children[0]); err != nil {
		// The durable child is already committed. A transient observation-surface
		// failure must never convert successful work creation into a failed source
		// Run or duplicate the operation on retry.
		p.logger.Warnw("failed to project Runbook start into conversation", "runId", run.ID, "childRunId", result.Children[0].ID, "error", err)
	}
	return result.DependencyGroup.Source, nil
}

func (p *AgentRunWorkerPool) rejectRecursiveRunbookInvocation(ctx context.Context, run *AgentRun, operation HostedRunbookOperation) error {
	if run == nil {
		return nil
	}
	wantedID := strings.TrimSpace(operation.DefinitionID)
	wantedVersion := strings.TrimSpace(operation.DefinitionVersion)
	wantedEntrypoint := strings.TrimSpace(operation.Entrypoint)
	visited := make(map[string]struct{})
	current := run
	for depth := 0; current != nil && depth < 256; depth++ {
		if _, duplicate := visited[current.ID]; duplicate {
			return fmt.Errorf("reject recursive Runbook invocation %q: Run ancestry contains a cycle at %s", wantedEntrypoint, current.ID)
		}
		visited[current.ID] = struct{}{}
		ancestorID, ancestorVersion, ancestorEntrypoint := runbookInvocationIdentity(current)
		exactIdentity := wantedID != "" && wantedVersion != "" &&
			ancestorID == wantedID && ancestorVersion == wantedVersion &&
			(ancestorEntrypoint == "" || ancestorEntrypoint == wantedEntrypoint)
		legacyIdentity := wantedEntrypoint != "" && ancestorID == "" &&
			strings.TrimSpace(current.Entrypoint) == wantedEntrypoint &&
			strings.TrimSpace(current.AssignedAgentID) == strings.TrimSpace(run.AssignedAgentID)
		if exactIdentity || legacyIdentity {
			return fmt.Errorf("reject recursive Runbook invocation %q: the same operation is already active in ancestor Run %s", wantedEntrypoint, current.ID)
		}
		if strings.TrimSpace(current.ParentRunID) == "" {
			return nil
		}
		parent, err := p.portfolio.GetAgentRun(ctx, current.Scope, current.ParentRunID)
		if err != nil {
			return fmt.Errorf("resolve Runbook invocation ancestry at %s: %w", current.ID, err)
		}
		current = parent
	}
	if current != nil {
		return fmt.Errorf("reject Runbook invocation %q: Run ancestry exceeds the supported depth", wantedEntrypoint)
	}
	return nil
}

func runbookInvocationIdentity(run *AgentRun) (string, string, string) {
	if run == nil {
		return "", "", ""
	}
	contextValues := run.Context
	if triggerInput, ok := contextValues["triggerInput"].(map[string]interface{}); ok {
		contextValues = triggerInput
	}
	definitionID, _ := contextValues["runbookDefinitionId"].(string)
	definitionVersion, _ := contextValues["runbookDefinitionVersion"].(string)
	entrypoint, _ := contextValues["runbookEntrypoint"].(string)
	if strings.TrimSpace(entrypoint) == "" {
		entrypoint = run.Entrypoint
	}
	return strings.TrimSpace(definitionID), strings.TrimSpace(definitionVersion), strings.TrimSpace(entrypoint)
}

func (p *AgentRunWorkerPool) resolveCollaborationChild(ctx context.Context, run *AgentRun) {
	if run == nil || !isTerminalAgentRunStatus(run.Status) {
		return
	}
	if p.requestInbox != nil {
		applied, err := p.requestInbox.ResolveDecisionRun(ctx, run)
		if err != nil && !errors.Is(err, ErrRevisionConflict) && !errors.Is(err, ErrInvalidAgentRequestState) {
			p.logger.Warnw("failed to resolve AgentRequest inbox decision", "runId", run.ID, "error", err)
		} else if applied {
			p.Wake()
		}
		applied, err = p.requestInbox.ResolveCompletionReviewRun(ctx, run)
		if err != nil && !errors.Is(err, ErrRevisionConflict) && !errors.Is(err, ErrInvalidAgentRequestState) {
			p.logger.Warnw("failed to resolve AgentRequest completion review", "runId", run.ID, "error", err)
		} else if applied {
			p.Wake()
		}
	}
	if p.collaboration == nil {
		return
	}
	_, err := p.collaboration.ResolveTerminalAgentRequestChild(ctx, run)
	if err != nil && !errors.Is(err, ErrRevisionConflict) && !errors.Is(err, ErrInvalidAgentRequestState) {
		p.logger.Warnw("failed to resolve collaboration request from terminal child", "runId", run.ID, "error", err)
	}
}

func (p *AgentRunWorkerPool) materializeTurnDelegation(ctx context.Context, _ string, run *AgentRun, turn *AgentTurn) (*AgentRun, error) {
	if p.collaboration == nil {
		return nil, errors.New("durable collaboration materialization is unavailable")
	}
	if run == nil || turn == nil || turn.RequestedDelegation == nil || len(turn.RequestedActions) != 0 || turn.RequestedFork != nil {
		return nil, errors.New("a bounded Turn must request exactly one delegation")
	}
	proposal := turn.RequestedDelegation
	if err := validateTaskDelegationSource(ctx, p.portfolio, run); err != nil {
		return nil, err
	}
	sharedContext := cloneMap(proposal.Context)
	if sharedContext == nil {
		sharedContext = make(map[string]interface{})
	}
	if proposal.Mode != "" {
		sharedContext[DelegationModeContextKey] = string(proposal.Mode)
	}
	requestedEntrypoint, err := requestedAcceptedChildEntrypoint(sharedContext)
	if err != nil {
		return nil, err
	}
	if requestedEntrypoint != "" {
		sharedContext["runbookEntrypoint"] = requestedEntrypoint
	}
	runbookOrigin := delegatedRunbookOrigin(run.Context)
	if runbookOrigin != nil {
		if entrypoint := strings.TrimSpace(run.Entrypoint); entrypoint != "" {
			runbookOrigin["runbookEntrypoint"] = entrypoint
		}
		sharedContext["triggerInput"] = runbookOrigin
	}
	if invocation, ok := run.Context[RunbookInvocationContextKey].(map[string]interface{}); ok {
		// Explicit model arguments remain shareable; an original chat message
		// is retained only when the same Agent continues its own operation.
		forwarded := forwardRunbookInvocation(invocation, proposal.AssignedAgentID == run.AssignedAgentID)
		// The caller's invocation is task lineage. Only this proposal's explicit
		// target method can select an entrypoint on the delegated Agent.
		delete(forwarded, "entrypoint")
		if requestedEntrypoint != "" {
			forwarded["entrypoint"] = requestedEntrypoint
		}
		sharedContext[RunbookInvocationContextKey] = forwarded
	}
	key := strings.Join([]string{"delegation", run.ID, proposal.StepID}, ":")
	requestID := stableCollaborationID(run.Scope, key, "request")
	existing, err := p.collaboration.GetAgentRequest(ctx, run.Scope, requestID)
	if err != nil && !errors.Is(err, ErrAgentRequestNotFound) {
		return nil, err
	}
	if existing != nil {
		if existing.Status == AgentRequestStatusClarificationRequested {
			if strings.TrimSpace(proposal.Clarification) == "" {
				return nil, errors.New("delegation recipient requested clarification, but the follow-up proposal supplied no answer")
			}
			clarified, clarifyErr := p.collaboration.RespondAgentRequest(ctx, RespondAgentRequestRequest{
				Scope: run.Scope, RequestID: existing.ID, ExpectedRevision: existing.Revision,
				Decision: AgentRequestDecisionProvideClarification, Principal: existing.Requester,
				Message: strings.TrimSpace(proposal.Clarification),
			})
			if clarifyErr != nil {
				return nil, clarifyErr
			}
			if clarified == nil || clarified.Source == nil {
				return nil, errors.New("delegation clarification did not return the durable waiting source Run")
			}
			return clarified.Source, nil
		}
		source, getErr := p.portfolio.GetAgentRun(ctx, run.Scope, run.ID)
		if getErr != nil || source == nil {
			if getErr == nil {
				getErr = ErrRunNotFound
			}
			return nil, getErr
		}
		return source, nil
	}
	budget, err := completeChildBudgetAllocation(run, proposal.Budget)
	if err != nil {
		return nil, err
	}
	acceptancePolicy := AgentRequestAcceptanceRecipientReview
	if runbookOrigin != nil {
		// The kernel-authored Runbook turn has already selected the exact
		// recipient under an immutable governed definition. A second model
		// admission turn adds no authority and can only make deterministic
		// scheduled work less reliable. Conversational/model-authored
		// delegation continues to require recipient review.
		acceptancePolicy = AgentRequestAcceptancePreauthorized
	}
	created, err := p.collaboration.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		ID: requestID, Scope: run.Scope, Kind: AgentRequestKindRequest,
		Requester:   CollaborationParty{Type: run.Owner.Type, ID: run.Owner.ID},
		Recipient:   CollaborationParty{Type: OwnerTypeAgent, ID: proposal.AssignedAgentID},
		SourceRunID: run.ID, Goal: proposal.Goal, SharedContext: sharedContext, ChildCheckpoint: proposal.Checkpoint,
		AcceptancePolicy: acceptancePolicy,
		BudgetAllocation: budget, IdempotencyKey: key,
	})
	if err != nil {
		return nil, err
	}
	if created == nil || created.Request == nil {
		return nil, errors.New("delegation materialization returned no durable Agent request")
	}
	if created.Request.Status == AgentRequestStatusPending {
		if created.Source != nil {
			return created.Source, nil
		}
		source, getErr := p.portfolio.GetAgentRun(ctx, run.Scope, run.ID)
		if getErr != nil || source == nil {
			if getErr == nil {
				getErr = ErrRunNotFound
			}
			return nil, getErr
		}
		return source, nil
	}
	if created.Request.Status != AgentRequestStatusAccepted && created.Request.Status != AgentRequestStatusCompleted &&
		created.Request.Status != AgentRequestStatusRejected && created.Request.Status != AgentRequestStatusCanceled &&
		created.Request.Status != AgentRequestStatusFailed {
		return nil, fmt.Errorf("delegation request %s is %s", created.Request.ID, created.Request.Status)
	}
	source, err := p.portfolio.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil || source == nil {
		if err == nil {
			err = ErrRunNotFound
		}
		return nil, err
	}
	return source, nil
}

func delegatedRunbookOrigin(contextValues map[string]interface{}) map[string]interface{} {
	if contextValues == nil {
		return nil
	}
	definitionID, _ := contextValues["runbookDefinitionId"].(string)
	definitionVersion, _ := contextValues["runbookDefinitionVersion"].(string)
	if strings.TrimSpace(definitionID) == "" || strings.TrimSpace(definitionVersion) == "" {
		return nil
	}
	origin := map[string]interface{}{
		"runbookDefinitionId":      strings.TrimSpace(definitionID),
		"runbookDefinitionVersion": strings.TrimSpace(definitionVersion),
	}
	if value, ok := contextValues["runbookEntrypoint"].(string); ok && strings.TrimSpace(value) != "" {
		origin["runbookEntrypoint"] = strings.TrimSpace(value)
	}
	for _, key := range []string{"runbookActivationId", "runbookTriggerId"} {
		if value, ok := contextValues[key].(string); ok && strings.TrimSpace(value) != "" {
			origin[key] = strings.TrimSpace(value)
		}
	}
	return origin
}

func (p *AgentRunWorkerPool) resolveForkChild(ctx context.Context, run *AgentRun) {
	if p.forks == nil || run == nil || run.ParentRunID == "" || run.Checkpoint["forkChild"] == nil {
		return
	}
	result, err := p.forks.CompleteChild(ctx, run, ActivityActor{Type: "worker", ID: p.poolID})
	if err != nil && !errors.Is(err, ErrDependencyConflict) {
		p.logger.Warnw("failed to resolve terminal fork child", "runId", run.ID, "error", err)
	} else if result != nil && !result.Replayed && result.Evaluation.Wake {
		p.Wake()
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
	selected, err := selectTurnModelAction(binding.ModelActions, request)
	if err != nil {
		return nil, err
	}
	if selected == nil {
		return nil, fmt.Errorf("requested capability %q is not authorized", request.Capability)
	}
	arguments, err := resolveTurnActionInput(turn.ContinuationCheckpoint, request.InputRef)
	if err != nil {
		return nil, err
	}
	idempotencyKey := turnActionIdempotencyKey(request.IdempotencyKey, selected.SideEffect, turn.ID)
	actionDeploymentID := strings.TrimSpace(binding.ActionDeploymentID)
	if strings.TrimSpace(selected.DeploymentID) != "" {
		actionDeploymentID = strings.TrimSpace(selected.DeploymentID)
	}
	if actionDeploymentID == "" {
		actionDeploymentID = binding.DeploymentID
	}
	assignedAgentID := ""
	if run.Owner.Type == OwnerTypeTeam && run.Kind == RunKindConversation {
		assignedAgentID, _ = turn.ContinuationCheckpoint[teamActionAssignedAgentCheckpointKey].(string)
		assignedAgentID = strings.TrimSpace(assignedAgentID)
		if assignedAgentID == "" {
			return nil, errors.New("Team action proposal is missing its trusted roster Agent attribution")
		}
	}
	proposalRequest := ProposeActionRequest{
		Scope: run.Scope, RunID: run.ID, TurnID: turn.ID, WorkerID: workerID, DeploymentID: actionDeploymentID,
		AssignedAgentID: assignedAgentID,
		BindingID:       selected.BindingID, BindingRevision: selected.BindingRevision,
		SkillID: selected.SkillID, SkillVersion: selected.Version, Action: selected.Action, Arguments: arguments,
		PreparedRuntime: request.PreparedRuntime,
		IdempotencyKey:  idempotencyKey, Summary: request.Summary,
		ExternalOperation: request.ExternalOperation,
		ReviewContext:     cloneApprovalReviewContext(request.ReviewContext),
		Actor:             ActivityActor{Type: "worker", ID: workerID}, EvidenceRefs: append([]string(nil), request.EvidenceRefs...),
		ContinuationCheckpoint: turn.ContinuationCheckpoint,
		CausationID:            turn.ID,
	}
	var proposal *ActionProposalResult
	for attempt := 0; attempt < 4; attempt++ {
		proposal, err = p.actions.Propose(ctx, proposalRequest)
		if !errors.Is(err, ErrRevisionConflict) || attempt == 3 {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		latest, retryErr := p.continuationActionProposalRetry(ctx, workerID, run, turn)
		if retryErr != nil {
			return nil, retryErr
		}
		if latest == nil {
			break
		}
		// Revalidate the same accepted action against the adopted canonical
		// Run. The model, Turn, arguments and idempotency key remain unchanged.
		run = latest
	}
	if err != nil {
		return nil, err
	}
	if proposal == nil || proposal.Call == nil {
		return nil, errors.New("governed action proposal returned no durable Run or ActionCall")
	}
	if !proposal.Created {
		return p.resumeReplayedTurnAction(ctx, workerID, run, turn, proposal.Call)
	}
	if proposal.Run == nil {
		return nil, errors.New("governed action proposal returned no durable Run")
	}
	if p.actionObserver != nil {
		if observeErr := p.actionObserver.ObserveActionProposal(ctx, run, turn, proposal); observeErr != nil {
			p.logger.Warnw("governed action projection will require reconciliation", "runId", run.ID, "turnId", turn.ID, "actionCallId", proposal.Call.ID, "error", observeErr)
		}
	}
	return proposal.Run, nil
}

func (p *AgentRunWorkerPool) continuationActionProposalRetry(ctx context.Context, workerID string, before *AgentRun, turn *AgentTurn) (*AgentRun, error) {
	if before == nil || turn == nil || p.coordinator == nil || before.LastAppliedTurn != turn.Sequence || turn.Status != AgentTurnStatusCompleted {
		return nil, nil
	}
	latest, err := p.portfolio.GetAgentRun(ctx, before.Scope, before.ID)
	if err != nil {
		return nil, err
	}
	if latest == nil || latest.Status != AgentRunStatusRunning || latest.LeaseOwner != workerID ||
		latest.LeaseExpiresAt == nil || !latest.LeaseExpiresAt.After(time.Now()) || latest.LastAppliedTurn != turn.Sequence {
		return nil, nil
	}
	task, err := canonicalConversationTaskContinuation(ctx, p.portfolio, latest)
	if err != nil {
		return nil, err
	}
	if task == nil || before.Context[ConversationTaskContextKey] != nil && before.Context[ConversationTaskContextKey] != task.ID {
		return nil, nil
	}
	// Only the committed handoff and a same-worker lease heartbeat may have
	// changed the source. New guidance, budgets, status and pending governance
	// remain ordinary conflicts and cannot be rebased by this retry.
	expected := cloneAgentRun(before)
	expected.ConcurrencyKey = latest.ConcurrencyKey
	if expected.Context == nil {
		expected.Context = make(map[string]interface{})
	}
	expected.Context[ConversationTaskContextKey] = task.ID
	expected.Revision, expected.UpdatedAt = latest.Revision, latest.UpdatedAt
	expected.LeaseExpiresAt = cloneAdmissionTime(latest.LeaseExpiresAt)
	expectedJSON, err := json.Marshal(expected)
	if err != nil {
		return nil, err
	}
	latestJSON, err := json.Marshal(latest)
	if err != nil {
		return nil, err
	}
	if string(expectedJSON) != string(latestJSON) {
		return nil, nil
	}
	persistedTurn, err := p.coordinator.turns.turns.GetAgentTurn(ctx, before.Scope, turn.ID)
	if err != nil {
		return nil, err
	}
	if persistedTurn == nil || persistedTurn.RunID != latest.ID || persistedTurn.Sequence != turn.Sequence || persistedTurn.Status != AgentTurnStatusCompleted {
		return nil, nil
	}
	providedJSON, err := json.Marshal(turn)
	if err != nil {
		return nil, err
	}
	persistedJSON, err := json.Marshal(persistedTurn)
	if err != nil {
		return nil, err
	}
	if string(providedJSON) != string(persistedJSON) {
		return nil, nil
	}
	return latest, nil
}

func turnActionIdempotencyKey(proposed string, sideEffect capability.SideEffect, turnID string) string {
	if sideEffect == capability.SideEffectRead || sideEffect == capability.SideEffectNone {
		// A later Turn may need a fresh observation after the resource changes.
		// The Turn ID still makes worker retries of this proposal idempotent.
		return fmt.Sprintf("turn:%s:read:0", turnID)
	}
	if key := strings.TrimSpace(proposed); key != "" {
		return key
	}
	return fmt.Sprintf("turn:%s:action:0", turnID)
}

func (p *AgentRunWorkerPool) resumeReplayedTurnAction(ctx context.Context, workerID string, run *AgentRun, turn *AgentTurn, call *ActionCall) (*AgentRun, error) {
	if run == nil || turn == nil || call == nil || call.RunID != run.ID {
		return nil, errors.New("governed action replay identity is invalid")
	}
	status := AgentRunStatusQueued
	var wake *WakeCondition
	checkpoint := preserveKernelActionHistory(run.Checkpoint, turn.ContinuationCheckpoint)
	switch call.Status {
	case ActionCallStatusSucceeded, ActionCallStatusFailed, ActionCallStatusDenied, ActionCallStatusCanceled, ActionCallStatusCompensated:
		metadata := map[string]interface{}{"idempotentReplay": true}
		if call.Status == ActionCallStatusDenied && call.ApprovalID != "" {
			getter, ok := p.portfolio.(interface {
				GetApproval(context.Context, Scope, string) (*ApprovalCheckpoint, error)
			})
			if ok {
				approval, err := getter.GetApproval(ctx, call.Scope, call.ApprovalID)
				if err != nil {
					return nil, err
				}
				if approval == nil || approval.Scope != call.Scope || approval.RunID != run.ID || approval.ActionCallID != call.ID {
					return nil, errors.New("replayed action approval provenance is invalid")
				}
				metadata["approvalStatus"] = approval.Status
				if approval.Status == ApprovalStatusChangesRequested {
					metadata["reviewerGuidance"] = approval.DecisionReason
					metadata["reviewedProposalRevision"] = approval.Revision - 1
				}
			} else if last, ok := run.Checkpoint["lastAction"].(map[string]interface{}); ok && last["actionCallId"] == call.ID && last["approvalId"] == call.ApprovalID {
				for _, key := range []string{"approvalStatus", "reviewerGuidance", "reviewedProposalRevision"} {
					if value, ok := last[key]; ok {
						metadata[key] = value
					}
				}
			}
		}
		checkpoint = checkpointTerminalAction(checkpoint, call, metadata)
	case ActionCallStatusReady, ActionCallStatusRunning, ActionCallStatusCompensating:
		status = AgentRunStatusWaitingForDependency
		wake = &WakeCondition{Type: "action", Reference: call.ID}
	case ActionCallStatusWaitingApproval:
		if strings.TrimSpace(call.ApprovalID) == "" {
			return nil, errors.New("governed action replay is waiting for an approval without an approval identity")
		}
		status = AgentRunStatusWaitingForApproval
		wake = &WakeCondition{Type: "approval", Reference: call.ApprovalID}
	default:
		return nil, fmt.Errorf("governed action replay has unsupported status %q", call.Status)
	}
	resumed, _, err := p.activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: status, WakeCondition: wake, Checkpoint: checkpoint,
		LeaseOwner: workerID, Summary: "Reused an existing governed action result",
		EventType: "action.replayed", Actor: ActivityActor{Type: "worker", ID: workerID},
		TurnID: turn.ID, CausationID: call.ID, Payload: map[string]interface{}{
			"actionCallId": call.ID, "skillId": call.SkillID, "skillVersion": call.SkillVersion,
			"action": call.Action, "status": call.Status,
		},
	})
	if err != nil {
		return nil, err
	}
	if status == AgentRunStatusQueued {
		p.Wake()
	}
	return resumed, nil
}

type preparedRuntimeTurnRunner struct{ binding *TurnRunnerBinding }

func (r preparedRuntimeTurnRunner) PlanTurnBudget(ctx context.Context, input TurnExecutionContext) (BudgetUsage, error) {
	if r.binding == nil || r.binding.Runner == nil {
		return BudgetUsage{}, errors.New("turn runner binding is unavailable")
	}
	planner, ok := r.binding.Runner.(TurnBudgetPlanner)
	if !ok {
		return BudgetUsage{}, nil
	}
	return planner.PlanTurnBudget(ctx, input)
}

func (r preparedRuntimeTurnRunner) RunTurn(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
	if r.binding == nil || r.binding.Runner == nil {
		return nil, errors.New("turn runner binding is unavailable")
	}
	if err := validatePreparedSkillRuntimes(r.binding); err != nil {
		return nil, err
	}
	outcome, err := r.binding.Runner.RunTurn(ctx, input)
	if outcome == nil {
		return nil, err
	}
	for index := range outcome.ProposedActions {
		request := &outcome.ProposedActions[index]
		// The model is never an authority for execution artifacts.
		request.PreparedRuntime = nil
		selected, selectionErr := selectTurnModelAction(r.binding.ModelActions, *request)
		if selectionErr != nil || selected == nil {
			continue
		}
		for runtimeIndex := range r.binding.PreparedRuntimes {
			prepared := &r.binding.PreparedRuntimes[runtimeIndex]
			if prepared.DeploymentID == selected.DeploymentID && prepared.BindingID == selected.BindingID && prepared.BindingRevision == selected.BindingRevision &&
				prepared.SkillID == selected.SkillID && prepared.SkillVersion == selected.Version {
				copy := prepared.Runtime
				copy.Executables = append([]string(nil), prepared.Runtime.Executables...)
				request.PreparedRuntime = &copy
				break
			}
		}
	}
	return outcome, err
}

func validatePreparedSkillRuntimes(binding *TurnRunnerBinding) error {
	seen := make(map[string]bool, len(binding.PreparedRuntimes))
	for index := range binding.PreparedRuntimes {
		prepared := &binding.PreparedRuntimes[index]
		if strings.TrimSpace(prepared.DeploymentID) == "" || strings.TrimSpace(prepared.BindingID) == "" || prepared.BindingRevision < 1 || strings.TrimSpace(prepared.SkillID) == "" || strings.TrimSpace(prepared.SkillVersion) == "" {
			return errors.New("prepared Skill runtime binding identity is invalid")
		}
		if err := skill.ValidatePreparedRuntimeReference(&prepared.Runtime); err != nil {
			return fmt.Errorf("prepared Skill runtime binding is invalid: %w", err)
		}
		key := fmt.Sprintf("%s:%s@%d:%s@%s", prepared.DeploymentID, prepared.BindingID, prepared.BindingRevision, prepared.SkillID, prepared.SkillVersion)
		if seen[key] {
			return errors.New("prepared Skill runtime binding is duplicated")
		}
		seen[key] = true
		authorized := false
		for actionIndex := range binding.ModelActions {
			action := &binding.ModelActions[actionIndex]
			if action.DeploymentID == prepared.DeploymentID && action.BindingID == prepared.BindingID && action.BindingRevision == prepared.BindingRevision && action.SkillID == prepared.SkillID && action.Version == prepared.SkillVersion {
				authorized = true
				break
			}
		}
		if !authorized {
			return errors.New("prepared Skill runtime is not bound to an authorized model action")
		}
	}
	return nil
}

func selectTurnModelAction(actions []capability.ModelAction, request TurnAction) (*capability.ModelAction, error) {
	var selected *capability.ModelAction
	for index := range actions {
		candidate := &actions[index]
		if candidate.Name != request.Capability || (request.BindingID != "" && (candidate.BindingID != request.BindingID || candidate.BindingRevision != request.BindingRevision)) {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("requested capability %q is ambiguous without an exact binding", request.Capability)
		}
		selected = candidate
	}
	if request.ExpectedSkillVersion != "" && (selected == nil || selected.Version != request.ExpectedSkillVersion) {
		return nil, fmt.Errorf("%w: requested capability %q version %q is not authorized by the current binding", ErrRunSkillDependencyUnavailable, request.Capability, request.ExpectedSkillVersion)
	}
	return selected, nil
}

func (p *AgentRunWorkerPool) failMaterialization(ctx context.Context, workerID string, run *AgentRun, turn *AgentTurn, cause error) {
	if run == nil {
		return
	}
	if p.parkAcceptedRunRecovery(ctx, workerID, run, turn, cause) {
		return
	}
	if p.parkSkillRuntimeMaintenance(ctx, workerID, run, turn, cause) {
		return
	}
	status := AgentRunStatusFailed
	runError := "governed action materialization failed"
	safeCause := sanitizeActionError(cause, nil)
	checkpoint := checkpointTerminalFailure(preserveKernelActionHistory(run.Checkpoint, run.Checkpoint), "action_admission_failed")
	failed, _, err := p.activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: status, LeaseOwner: workerID, Checkpoint: checkpoint,
		Error: runError, Summary: "Agent action proposal could not be governed",
		EventType: "action.materialization_failed", Actor: ActivityActor{Type: "worker", ID: workerID},
		TurnID: turn.ID, CausationID: turn.ID, Payload: map[string]interface{}{"reason": safeCause},
	})
	if err != nil {
		p.logger.Errorw("failed to persist action materialization failure", "runId", run.ID, "error", err)
	} else if isTerminalAgentRunStatus(failed.Status) {
		p.finalizeTerminalRun(ctx, failed)
		p.projectTerminalReporting(ctx, failed)
		p.resolveCollaborationChild(ctx, failed)
	} else {
		p.Wake()
	}
}

// Retained for deterministic validation callers; rejected proposals cannot
// grant an automatic recovery turn or retain model-authored rejected state.
func checkpointGovernedProposalFailure(run *AgentRun, turn *AgentTurn, cause error, safeCause string) (map[string]interface{}, bool) {
	return nil, false
}

func (p *AgentRunWorkerPool) projectTerminalReporting(ctx context.Context, run *AgentRun) {
	// Canonical stores already captured an immutable reporting intent with the
	// terminal commit. This fast path improves latency; the durable reporting
	// worker retries and acknowledges it independently using the same message
	// idempotency key, including crashes immediately before or after this call.
	if err := projectTerminalRunReporting(ctx, p.reportingStore, run); err != nil {
		p.logger.Warnw("failed to project terminal Run milestone", "runId", run.ID, "error", err)
	}
}

func (p *AgentRunWorkerPool) finalizeTerminalRun(ctx context.Context, run *AgentRun) {
	if run == nil || !isTerminalAgentRunStatus(run.Status) {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if p.runCommands != nil {
		if err := p.runCommands.CascadeTerminalRun(cleanupCtx, cloneAgentRun(run)); err != nil {
			p.logger.Warnw("failed to cascade terminal Run cancellation", "runId", run.ID, "status", run.Status, "error", err)
		}
	}
	if p.runFinalizer == nil {
		return
	}
	if err := p.runFinalizer.FinalizeRun(cleanupCtx, cloneAgentRun(run)); err != nil {
		p.logger.Warnw("failed to finalize terminal Run resources", "runId", run.ID, "status", run.Status, "error", err)
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

func (p *AgentRunWorkerPool) observeConversationTaskDeadline(ctx context.Context, run *AgentRun) context.CancelFunc {
	deadlineCtx, cancel := context.WithCancel(ctx)
	if p.continuations == nil || run.Kind != RunKindConversation {
		return cancel
	}
	// Start before runner resolution. Commentary, model Turns, queueing and
	// action waits all retain this same canonical creation-time deadline.
	deadline := run.CreatedAt.Add(ConversationTaskForegroundTimeout)
	go func() {
		timer := time.NewTimer(max(time.Until(deadline), 0))
		defer timer.Stop()
		select {
		case <-deadlineCtx.Done():
			return
		case <-timer.C:
		}
		result, err := p.continuations.store.PromoteConversationTask(deadlineCtx, ConversationTaskPromotionRequest{
			Scope: run.Scope, RunID: run.ID, Now: time.Now().UTC(), MinimumAge: ConversationTaskForegroundTimeout,
		})
		if err != nil && deadlineCtx.Err() == nil {
			p.logger.Warnw("failed to promote conversation Run at its foreground deadline", "runId", run.ID, "error", err)
		} else if result != nil {
			p.Wake()
		}
	}()
	return cancel
}

func (p *AgentRunWorkerPool) conversationTaskDeadlineLoop(ctx context.Context) {
	defer p.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		// This maintenance must remain independent of the worker limiter: every
		// available execution slot may be occupied by a long hosted invocation.
		results, err := p.continuations.ReconcileScope(ctx, p.config.Scope, time.Now().UTC())
		if err != nil && ctx.Err() == nil {
			p.logger.Warnw("failed to reconcile conversation task deadlines", "error", err)
		} else if len(results) > 0 {
			p.Wake()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *AgentRunWorkerPool) timerWakeLoop(ctx context.Context) {
	defer p.wg.Done()
	consecutiveFailures := 0
	seed := p.poolID + "-timer-wake"
	for {
		// Capture before reconciliation so a committed enqueue during the scan
		// remains visible when this loop subsequently waits.
		_, observedWake := p.maintenanceWake.snapshot()
		release, acquireErr := p.limiter.Load().acquire(ctx)
		if acquireErr != nil {
			return
		}
		result, err := p.wakeService.WakeDueTimers(ctx, p.config.Scope, time.Now())
		if err != nil {
			consecutiveFailures++
			release()
			p.logger.Warnw("failed to wake due agent runs", "error", err)
		} else {
			consecutiveFailures = 0
			// One timer per scope probes for work created by another process.
			// Internal claim hints do not retrigger the maintenance scan.
			p.requestClaim()
			p.reconcileAgentRequestInbox(ctx)
			p.reconcileForkChildren(ctx)
			p.reconcileTerminalRunFinalizers(ctx)
			release()
		}
		delay := workerPollDelay(p.config.PollInterval, consecutiveFailures, seed)
		if err == nil && result != nil && result.NextWakeAt != nil {
			if due := time.Until(*result.NextWakeAt); due < delay {
				delay = max(due, 0)
			}
		}
		if err != nil {
			// Timer-store failures retain their existing backoff even if another
			// worker publishes a latency hint while the store is unavailable.
			observedWake = nil
		}
		if !waitForWorkerPoll(ctx, observedWake, delay) {
			return
		}
	}
}

func (p *AgentRunWorkerPool) reconcileTerminalRunFinalizers(ctx context.Context) {
	if p.runCommands == nil && p.runFinalizer == nil && p.collaboration == nil && p.requestInbox == nil {
		return
	}
	now := time.Now().UTC()
	firstScan := p.lastFinalizationScan.IsZero()
	if !firstScan && now.Sub(p.lastFinalizationScan) < 10*time.Second {
		return
	}
	p.lastFinalizationScan = now
	// Terminal cleanup is maintenance behind timer wakes. Keep each slice small
	// so a large historical backlog cannot hold this pool's scheduler loop for
	// minutes while it probes already-finalized Runs.
	const pageSize = 10
	newest, err := p.portfolio.ListAgentRuns(ctx, AgentRunFilter{
		Scope: p.config.Scope, Kind: p.config.Kind, Statuses: []AgentRunStatus{
			AgentRunStatusCompleted, AgentRunStatusFailed, AgentRunStatusCanceled,
		}, Order: AgentRunOrderCreatedDesc, Limit: pageSize,
	})
	if err != nil {
		p.logger.Warnw("failed to list terminal Runs for resource finalization", "error", err)
		return
	}
	if firstScan {
		p.logger.Infow("started terminal Run resource reconciliation",
			"scopeKind", p.config.Scope.Kind, "scopeId", p.config.Scope.ID,
			"runKind", p.config.Kind, "candidates", len(newest))
	}
	for _, run := range newest {
		p.resolveForkChild(ctx, run)
		p.resolveCollaborationChild(ctx, run)
		p.finalizeTerminalRun(ctx, run)
	}
	if len(newest) < pageSize {
		p.finalizationOffset = 0
		return
	}
	if p.finalizationOffset == 0 {
		p.finalizationOffset = pageSize
		return
	}
	historical, err := p.portfolio.ListAgentRuns(ctx, AgentRunFilter{
		Scope: p.config.Scope, Kind: p.config.Kind, Statuses: []AgentRunStatus{
			AgentRunStatusCompleted, AgentRunStatusFailed, AgentRunStatusCanceled,
		}, Order: AgentRunOrderCreatedDesc, Limit: pageSize, Offset: p.finalizationOffset,
	})
	if err != nil {
		p.logger.Warnw("failed to list historical terminal Runs for resource finalization", "error", err)
		return
	}
	for _, run := range historical {
		p.resolveForkChild(ctx, run)
		p.resolveCollaborationChild(ctx, run)
		p.finalizeTerminalRun(ctx, run)
	}
	if len(historical) < pageSize {
		p.finalizationOffset = 0
	} else {
		p.finalizationOffset += pageSize
	}
}

func (p *AgentRunWorkerPool) reconcileAgentRequestInbox(ctx context.Context) {
	if p.requestInbox == nil || p.config.Kind != "" && p.config.Kind != RunKindAgentWork {
		return
	}
	result, err := p.requestInbox.ReconcileWorker(ctx, p.config.Scope, p.config.AssignedAgentID)
	if err != nil {
		p.logger.Warnw("AgentRequest inbox reconciliation completed with failures", "error", err)
	}
	if result != nil && (result.RequestsAccepted > 0 || result.DecisionRunsCreated > 0 || result.DecisionsApplied > 0 ||
		result.CompletionReviewsCreated > 0 || result.CompletionReviewsApplied > 0) {
		p.Wake()
	}
}

func (p *AgentRunWorkerPool) reconcileForkChildren(ctx context.Context) {
	if p.forks == nil {
		return
	}
	if _, ok := p.forks.store.(RunDependencyReconciliationStore); ok {
		const pageSize = 32
		work, err := NewDependencyCoordinator(p.forks.store).ListWaitingRunDependencyGroups(ctx, p.config.Scope, p.dependencyCursor, pageSize)
		if err != nil {
			p.logger.Warnw("failed to list waiting dependency groups", "error", err)
			return
		}
		for _, candidate := range work {
			p.dependencyCursor = candidate.GroupID
			if !candidate.Ready {
				continue
			}
			result, err := p.forks.ReconcileRunDependencyGroup(ctx, ReconcileRunDependencyGroupRequest{
				Scope: candidate.Scope, GroupID: candidate.GroupID, ExpectedGroupRevision: candidate.GroupRevision,
				ExpectedSourceRevision: candidate.SourceRevision, Actor: ActivityActor{Type: "worker", ID: p.poolID}, Visibility: ActivityVisibilityScope,
			})
			if err != nil && !errors.Is(err, ErrRevisionConflict) {
				p.logger.Warnw("failed to reconcile waiting dependency group", "groupId", candidate.GroupID, "error", err)
			} else if result != nil && !result.Replayed && result.Evaluation.Wake {
				p.Wake()
			}
		}
		if len(work) < pageSize {
			p.dependencyCursor = ""
		}
		return
	}
	// Custom stores without the optional indexed contract retain their existing
	// terminal-child recovery behavior.
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
