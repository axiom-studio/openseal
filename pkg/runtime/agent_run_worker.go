package runtime

import (
	"context"
	"errors"
	"fmt"
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
	// ModelCredential is the deployment's model binding
	// (deploymentModelCredential); kernel-issued model calls outside the
	// step, such as the evidence grounding review, send it. nil on desktop.
	ModelCredential *capability.CredentialReference

	// RunLoop seams (SPEC §2.4).
	Producer    StepProducer
	Session     *AgentSession        // nil only for producers with no tool calls (routing)
	Preparers   []StepPreparer       // run-start, then conversation
	Tools       LoopToolExecutor     // disp-1
	Compactor   StepCompactor        // G3
	Final       FinalAnswerHandler   // CONV-08 for chats, runOutputFinalHandler otherwise
	FinalOutput FinalOutputPublisher // nil = none
	Host        StepHost
	Authority   ControlAuthority   // section 04 §4, incl. the conversation flags of section 05 §7
	Prompt      PromptContribution // kernel-authored system parts from the owning section
	ToolCheck   ToolCallCheck      // optional (conversation CheckToolCall)
	AfterTool   AfterToolEntryHook // optional (conversation artifact receipts)
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
	loop                 *RunLoop
	wakeService          *AgentRunWakeService
	activity             *RunActivityService
	actions              *ActionCoordinator
	runFinalizer         RunTerminalFinalizer
	runCommands          *RunCommandService
	lastFinalizationScan time.Time
	dependencyCursor     string
	forks                *RunForkCoordinator
	collaboration        *CollaborationService
	conversationTasks    *ConversationTaskService
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
	signals              atomic.Pointer[WorkSignalStatus]
	// deadlines lets a queue-driven pool schedule its next due Run or lease
	// expiry instead of polling. Stores without it always poll.
	deadlines AgentRunDeadlineStore
	usage     poolActivity
}

// SetActionCoordinator opens each step's nested-action window (a running
// command's governed calls) on the pool's ActionCoordinator.
func (p *AgentRunWorkerPool) SetActionCoordinator(actions *ActionCoordinator) {
	p.actions = actions
	p.loop.actions = actions
}

func (p *AgentRunWorkerPool) SetRunTerminalFinalizer(finalizer RunTerminalFinalizer) {
	p.runFinalizer = finalizer
}

func (p *AgentRunWorkerPool) SetWorkerLimiter(limiter *WorkerLimiter) {
	p.limiter.Store(limiter)
}

// SetWorkSignals lets the pool schedule its next durable deadline into the
// work queue and sleep until woken while status is healthy. Without it the
// pool polls.
func (p *AgentRunWorkerPool) SetWorkSignals(status *WorkSignalStatus) {
	p.signals.Store(status)
}

// idleFor reports how long the pool has neither run nor been woken.
func (p *AgentRunWorkerPool) idleFor(now time.Time) time.Duration {
	return p.usage.idleFor(now)
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
	loop, err := NewRunLoop(store, nil, logger, RunLoopConfig{
		StepLeaseDuration: config.TurnLeaseDuration,
		PlanToolEntries:   workerPlanToolEntries(store), PlainTextFinal: workerPlainTextFinal,
	})
	if err != nil {
		return nil, err
	}
	pool := &AgentRunWorkerPool{
		config: config, scheduler: NewAgentRunScheduler(store), portfolio: store, loop: loop,
		wakeService: NewAgentRunWakeService(store, store), activity: NewRunActivityService(store, store),
		runCommands: NewRunCommandService(store),
		resolver:    resolver, logger: logger, wake: make(chan struct{}, 1), maintenanceWake: newWorkerPollSignal(),
		poolID: strings.TrimSpace(config.WorkerIDPrefix) + "-" + uuid.NewString(),
	}
	pool.deadlines, _ = store.(AgentRunDeadlineStore)
	pool.usage.touch()
	if forkStore, ok := store.(RunForkStore); ok {
		pool.forks = NewRunForkCoordinator(forkStore)
	}
	if collaborationStore, ok := store.(CollaborationKernelStore); ok {
		pool.collaboration = NewCollaborationService(collaborationStore)
	}
	if taskStore, ok := store.(ConversationTaskKernelStore); ok {
		pool.conversationTasks = NewConversationTaskService(taskStore)
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
	p.usage.touch()
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
		p.usage.begin()
		release, acquireErr := p.limiter.Load().acquire(ctx)
		if acquireErr != nil {
			p.usage.end()
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
			if signals := p.signals.Load(); signals.Healthy() {
				// No poll follows a failed claim while queue-driven.
				signals.scheduleIn(WorkFamilyRunDue, p.config.Scope, workerPollDelay(p.config.PollInterval, 1, workerID))
			}
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
		p.usage.end()
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
	_, _ = p.activity.AppendActivity(ctx, &ActivityEvent{
		Scope: run.Scope, RunID: run.ID, AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, TeamID: teamIDForRun(run),
		EventType: "run.claimed", Summary: "Run claimed by autonomous worker",
		Actor: ActivityActor{Type: "worker", ID: workerID}, Visibility: ActivityVisibilityScope,
	})
	// Resolution can itself involve slow hosted setup. Keep the existing claim
	// alive across that work so another worker cannot resolve or invoke a second
	// copy while the first one is still in flight.
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
	if err != nil || binding == nil || binding.Producer == nil {
		if p.parkAcceptedRunRecovery(ctx, workerID, run, nil, err) {
			return
		}
		if p.parkSkillRuntimeMaintenance(ctx, workerID, run, nil, err) {
			return
		}
		if err == nil {
			err = errors.New("turn runner resolver returned no step producer")
		}
		failed, _, transitionErr := p.activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusFailed, Error: err.Error(), LeaseOwner: workerID,
			Summary: "Agent run failed during runner resolution", Actor: ActivityActor{Type: "worker", ID: workerID},
		})
		if transitionErr != nil {
			p.logger.Errorw("failed to persist runner resolution failure", "runId", run.ID, "error", transitionErr)
		} else {
			p.finishTerminalRun(ctx, failed)
		}
		return
	}
	resolvedBinding := *binding
	resolvedBinding.InputContextRefs = append([]string(nil), binding.InputContextRefs...)
	binding = &resolvedBinding
	if ref := evidenceSnapshotInputContextRef(run.Context); ref != "" && !containsString(binding.InputContextRefs, ref) {
		binding.InputContextRefs = append(binding.InputContextRefs, ref)
	}

	claimCtx, cancelClaim := context.WithCancel(ctx)
	heartbeatDone := make(chan error, 1)
	go p.heartbeatRunLease(claimCtx, cancelClaim, workerID, run, heartbeatDone)
	exit := p.loop.Drive(claimCtx, RunLoopClaim{Run: run, WorkerID: workerID, Binding: binding})
	cancelClaim()
	if heartbeatErr := <-heartbeatDone; heartbeatErr != nil && exit.Reason != RunLoopExitLeaseLost {
		p.logger.Warnw("agent Run lease was lost during its claim", "runId", run.ID, "exit", exit.Reason, "error", heartbeatErr)
		exit.Reason = RunLoopExitLeaseLost
	}
	if exit.Err != nil {
		p.logger.Warnw("agent run claim ended with an error", "runId", run.ID, "exit", exit.Reason, "steps", exit.Steps, "error", exit.Err)
	}
	switch {
	case exit.Run != nil && isTerminalAgentRunStatus(exit.Run.Status):
		p.finishTerminalRun(ctx, exit.Run)
		p.Wake()
	case exit.Reason == RunLoopExitYielded || exit.Reason == RunLoopExitRequeued || exit.Reason == RunLoopExitParked:
		p.Wake()
	}
}

// finishTerminalRun runs the worker's terminal follow-ups: resource cleanup,
// the reporting fast path and the parent fork group or AgentRequest the Run
// answers.
func (p *AgentRunWorkerPool) finishTerminalRun(ctx context.Context, run *AgentRun) {
	p.finalizeTerminalRun(ctx, run)
	p.projectTerminalReporting(ctx, run)
	p.resolveForkChild(ctx, run)
	p.resolveCollaborationChild(ctx, run)
}

// workerPlanToolEntries is the pool's RunLoopConfig.PlanToolEntries: the
// dispatcher's planners over the binding's claim executor.
func workerPlanToolEntries(store PortfolioStore) StepToolEntryPlanner {
	return func(binding *TurnRunnerBinding, session *AgentSession, step *AgentTurn, assistant *SessionEntry, calls []LoopToolCall) ([]*SessionEntry, error) {
		if assistant != nil {
			return PlanToolEntries(binding, assistant)
		}
		if session == nil || step == nil {
			return nil, errors.New("deterministic tool entries need a session and a step")
		}
		run, err := store.GetAgentRun(context.Background(), step.Scope, step.RunID)
		if err != nil {
			return nil, err
		}
		if run == nil {
			return nil, ErrRunNotFound
		}
		return PlanToolCallEntries(binding, run, step, session.ID, calls)
	}
}

// workerPlainTextFinal is the pool's RunLoopConfig.PlainTextFinal: the
// control executor of the binding's claim executor judges the text.
func workerPlainTextFinal(ctx context.Context, cc ControlContext, text string) (ControlResult, error) {
	var control *ControlExecutor
	if cc.Binding != nil {
		if tools, ok := cc.Binding.Tools.(*claimToolExecutor); ok {
			control = tools.cfg.Control
		}
	}
	if control == nil {
		return ControlResult{}, errors.New("plain-text final needs a binding whose tool executor has a control executor")
	}
	return control.PlainTextFinal(ctx, cc, text)
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

// preparedRuntimeForAction is the activation-time runtime prepared for the
// selected action's exact binding, if any.
func preparedRuntimeForAction(binding *TurnRunnerBinding, selected *capability.ModelAction) *skill.PreparedRuntime {
	for runtimeIndex := range binding.PreparedRuntimes {
		prepared := &binding.PreparedRuntimes[runtimeIndex]
		if prepared.DeploymentID == selected.DeploymentID && prepared.BindingID == selected.BindingID && prepared.BindingRevision == selected.BindingRevision &&
			prepared.SkillID == selected.SkillID && prepared.SkillVersion == selected.Version {
			copy := prepared.Runtime
			copy.Executables = append([]string(nil), prepared.Runtime.Executables...)
			return &copy
		}
	}
	return nil
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

// selectTurnModelAction resolves a proposal exactly by name and, when the
// proposal carries one, binding. A name exposed twice is refused even when the
// proposal names a binding: a catalog with duplicate names is itself invalid.
func selectTurnModelAction(actions []capability.ModelAction, request TurnAction) (*capability.ModelAction, error) {
	var selected *capability.ModelAction
	matches := 0
	for index := range actions {
		candidate := &actions[index]
		if candidate.Name != request.Capability {
			continue
		}
		if matches++; matches > 1 {
			return nil, fmt.Errorf("requested capability %q is ambiguous: more than one binding exposes it", request.Capability)
		}
		if request.BindingID != "" && (candidate.BindingID != request.BindingID || candidate.BindingRevision != request.BindingRevision) {
			continue
		}
		selected = candidate
	}
	if request.ExpectedSkillVersion != "" && (selected == nil || selected.Version != request.ExpectedSkillVersion) {
		return nil, fmt.Errorf("%w: requested capability %q version %q is not authorized by the current binding", ErrRunSkillDependencyUnavailable, request.Capability, request.ExpectedSkillVersion)
	}
	return selected, nil
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

func (p *AgentRunWorkerPool) timerWakeLoop(ctx context.Context) {
	defer p.wg.Done()
	consecutiveFailures, maintenanceFailures, deadlineFailures := 0, 0, 0
	seed := p.poolID + "-timer-wake"
	for {
		// Capture before reconciliation so a committed enqueue during the scan
		// remains visible when this loop subsequently waits.
		_, observedWake := p.maintenanceWake.snapshot()
		signals := p.signals.Load()
		healthy, healthChanged := signals.watch()
		p.usage.begin()
		release, acquireErr := p.limiter.Load().acquire(ctx)
		if acquireErr != nil {
			p.usage.end()
			return
		}
		result, err := p.wakeService.WakeDueTimers(ctx, p.config.Scope, time.Now())
		var maintenanceDue *time.Time
		maintenanceFailed := false
		if err != nil {
			consecutiveFailures++
			release()
			p.logger.Warnw("failed to wake due agent runs", "error", err)
		} else {
			consecutiveFailures = 0
			// One timer per scope probes for work created by another process.
			// Internal claim hints do not retrigger the maintenance scan.
			p.requestClaim()
			inboxDue, inboxFailed := p.reconcileAgentRequestInbox(ctx)
			forkDue, forkFailed := p.reconcileForkChildren(ctx)
			finalizerDue := p.reconcileTerminalRunFinalizers(ctx)
			release()
			maintenanceDue = earliestDeadline(inboxDue, forkDue, finalizerDue)
			maintenanceFailed = inboxFailed || forkFailed
		}
		p.usage.end()
		var wait idleWait
		if !healthy || p.deadlines == nil || err != nil {
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
				if healthy {
					// A retired pool would miss its local retry; keep it queued.
					signals.scheduleIn(WorkFamilyRunDue, p.config.Scope, delay)
				}
			}
			wait = pollWait(delay)
		} else {
			// Queue-driven: schedule the earliest durable deadline and sleep
			// until woken. Claim and maintenance state only changes through
			// committed writes, which the outbox publishes, or through time,
			// which these deadlines cover.
			now := time.Now()
			p.usage.begin()
			deadline, deadlineErr := p.deadlines.NextAgentRunDeadline(ctx, p.config.Scope, p.config.Kind, p.config.AssignedAgentID, now.UTC())
			p.usage.end()
			var next *time.Time
			if deadlineErr != nil {
				if ctx.Err() != nil {
					return
				}
				deadlineFailures++
				p.logger.Warnw("agent run deadline lookup failed", "scopeKind", p.config.Scope.Kind, "scopeId", p.config.Scope.ID, "error", deadlineErr)
				next = new(now.Add(workerPollDelay(p.config.PollInterval, deadlineFailures, seed)))
			} else {
				deadlineFailures = 0
				next = earliestDeadline(deadline, result.NextWakeAt, maintenanceDue)
			}
			if maintenanceFailed {
				maintenanceFailures++
				next = earliestDeadline(next, new(now.Add(workerPollDelay(p.config.PollInterval, maintenanceFailures, seed))))
			} else {
				maintenanceFailures = 0
			}
			signals.schedule(WorkFamilyRunDue, p.config.Scope, next)
			wait = waitForever()
		}
		if !waitIdle(ctx, observedWake, healthChanged, wait) {
			return
		}
	}
}

// reconcileTerminalRunFinalizers returns when it next needs to run: after a
// throttled pass, or while a historical sweep is still in progress. Nil means
// it is caught up until the next signal.
func (p *AgentRunWorkerPool) reconcileTerminalRunFinalizers(ctx context.Context) *time.Time {
	if p.runCommands == nil && p.runFinalizer == nil && p.collaboration == nil && p.requestInbox == nil {
		return nil
	}
	const interval = 10 * time.Second
	now := time.Now().UTC()
	firstScan := p.lastFinalizationScan.IsZero()
	if !firstScan && now.Sub(p.lastFinalizationScan) < interval {
		return new(p.lastFinalizationScan.Add(interval))
	}
	p.lastFinalizationScan = now
	next := new(now.Add(interval))
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
		return next
	}
	if firstScan {
		p.logger.Debugw("started terminal Run resource reconciliation",
			"scopeKind", p.config.Scope.Kind, "scopeId", p.config.Scope.ID,
			"runKind", p.config.Kind, "candidates", len(newest))
	}
	for _, run := range newest {
		p.resolveForkChild(ctx, run)
		p.resolveCollaborationChild(ctx, run)
		p.finalizeTerminalRun(ctx, run)
	}
	key := p.finalizationKey()
	if len(newest) < pageSize {
		terminalRunSweeps.finish(key, now)
		return nil
	}
	offset, due := terminalRunSweeps.next(key, now)
	if !due {
		return nil
	}
	if offset == 0 {
		terminalRunSweeps.advance(key, pageSize)
		return next
	}
	historical, err := p.portfolio.ListAgentRuns(ctx, AgentRunFilter{
		Scope: p.config.Scope, Kind: p.config.Kind, Statuses: []AgentRunStatus{
			AgentRunStatusCompleted, AgentRunStatusFailed, AgentRunStatusCanceled,
		}, Order: AgentRunOrderCreatedDesc, Limit: pageSize, Offset: offset,
	})
	if err != nil {
		p.logger.Warnw("failed to list historical terminal Runs for resource finalization", "error", err)
		return next
	}
	for _, run := range historical {
		p.resolveForkChild(ctx, run)
		p.resolveCollaborationChild(ctx, run)
		p.finalizeTerminalRun(ctx, run)
	}
	if len(historical) < pageSize {
		terminalRunSweeps.finish(key, now)
		return nil
	}
	terminalRunSweeps.advance(key, offset+pageSize)
	return next
}

func (p *AgentRunWorkerPool) finalizationKey() string {
	return p.config.Scope.Kind + "\x00" + p.config.Scope.ID + "\x00" + string(p.config.Kind) + "\x00" + p.config.AssignedAgentID
}

// terminalRunSweeps remembers, per process, how far each pool scope's sweep
// over historical terminal Runs has got and when it last completed. Pools
// come and go with their scope's work, so the cursor must outlive them, and
// a completed sweep is not repeated on every wake: newly terminal Runs are
// covered by the newest page, which every pass checks.
var terminalRunSweeps = newTerminalRunSweepProgress()

const (
	terminalRunSweepRepeat = 6 * time.Hour
	terminalRunSweepLimit  = 100000
)

type terminalRunSweepProgress struct {
	mu      sync.Mutex
	entries map[string]terminalRunSweep
}

type terminalRunSweep struct {
	offset      int
	completedAt time.Time
}

func newTerminalRunSweepProgress() *terminalRunSweepProgress {
	return &terminalRunSweepProgress{entries: make(map[string]terminalRunSweep)}
}

// next returns the sweep offset, zero to start one, and whether a page is
// due: a sweep completed within terminalRunSweepRepeat is not restarted.
func (t *terminalRunSweepProgress) next(key string, now time.Time) (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[key]
	if entry.offset == 0 && !entry.completedAt.IsZero() && now.Sub(entry.completedAt) < terminalRunSweepRepeat {
		return 0, false
	}
	return entry.offset, true
}

func (t *terminalRunSweepProgress) advance(key string, offset int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.put(key, terminalRunSweep{offset: offset})
}

func (t *terminalRunSweepProgress) finish(key string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.put(key, terminalRunSweep{completedAt: now})
}

func (t *terminalRunSweepProgress) put(key string, entry terminalRunSweep) {
	if _, exists := t.entries[key]; !exists && len(t.entries) >= terminalRunSweepLimit {
		clear(t.entries)
	}
	t.entries[key] = entry
}

// reconcileAgentRequestInbox returns when a throttled conversation projection
// is next due, and whether the pass failed and must be retried.
func (p *AgentRunWorkerPool) reconcileAgentRequestInbox(ctx context.Context) (*time.Time, bool) {
	if p.requestInbox == nil || p.config.Kind != "" && p.config.Kind != RunKindAgentWork {
		return nil, false
	}
	result, err := p.requestInbox.ReconcileWorker(ctx, p.config.Scope, p.config.AssignedAgentID)
	if err != nil {
		p.logger.Warnw("AgentRequest inbox reconciliation completed with failures", "error", err)
	}
	if result != nil && (result.RequestsAccepted > 0 || result.DecisionRunsCreated > 0 || result.DecisionsApplied > 0 ||
		result.CompletionReviewsCreated > 0 || result.CompletionReviewsApplied > 0) {
		p.Wake()
	}
	return p.requestInbox.deferredProjectionAt(), err != nil
}

// reconcileForkChildren returns when another dependency page is due, and
// whether the pass failed and must be retried.
func (p *AgentRunWorkerPool) reconcileForkChildren(ctx context.Context) (*time.Time, bool) {
	if p.forks == nil {
		return nil, false
	}
	failed := false
	if _, ok := p.forks.store.(RunDependencyReconciliationStore); ok {
		const pageSize = 32
		work, err := NewDependencyCoordinator(p.forks.store).ListWaitingRunDependencyGroups(ctx, p.config.Scope, p.dependencyCursor, pageSize)
		if err != nil {
			p.logger.Warnw("failed to list waiting dependency groups", "error", err)
			return nil, true
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
				failed = true
			} else if result != nil && !result.Replayed && result.Evaluation.Wake {
				p.Wake()
			}
		}
		if len(work) < pageSize {
			p.dependencyCursor = ""
			return nil, failed
		}
		// More waiting groups remain beyond this page.
		return new(time.Now()), failed
	}
	// Custom stores without the optional indexed contract retain their existing
	// terminal-child recovery behavior.
	runs, err := p.portfolio.ListAgentRuns(ctx, AgentRunFilter{
		Scope: p.config.Scope, Statuses: []AgentRunStatus{AgentRunStatusCompleted, AgentRunStatusFailed, AgentRunStatusCanceled}, Limit: 100,
	})
	if err != nil {
		p.logger.Warnw("failed to list terminal fork children", "error", err)
		return nil, true
	}
	for _, run := range runs {
		p.resolveForkChild(ctx, run)
	}
	return nil, false
}
