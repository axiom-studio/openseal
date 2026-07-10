// Package openseal provides the stable public API for embedding OpenSeal
// as a library. Downstream consumers should import this package
// rather than reaching into individual pkg/ subpackages.
package openseal

import (
	"context"
	"fmt"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/executor"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/skill/clawhub"
	skillopenclaw "github.com/axiom-studio/openseal/pkg/skill/openclaw"
	skillsource "github.com/axiom-studio/openseal/pkg/skill/source"
	"github.com/axiom-studio/openseal/pkg/types"
	"go.uber.org/zap"
)

// Re-export key types so consumers only import this package.
type (
	NodeDefinition       = executor.NodeDefinition
	ConnectionDefinition = executor.ConnectionDefinition
	ExecutionResult      = executor.ExecutionResult
	NodeResult           = executor.NodeResult
	Registry             = executor.Registry
	StepExecutor         = executor.StepExecutor
	ExecutionGraph       = executor.ExecutionGraph

	AgentNodeDefinition                   = types.AgentNodeDefinition
	AgentConnection                       = types.AgentConnection
	AgentLibraryBean                      = types.AgentLibraryBean
	AgentInstanceBean                     = types.AgentInstanceBean
	AgentWorkflow                         = types.AgentWorkflow
	AgentWorkflowBean                     = types.AgentWorkflowBean
	AgentDefinition                       = kernelagent.AgentDefinition
	AgentSkillRequirement                 = kernelagent.SkillRequirement
	AgentAuthorityPolicy                  = kernelagent.AuthorityPolicy
	AgentMemoryPolicy                     = kernelagent.MemoryPolicy
	AgentEscalationPolicy                 = kernelagent.EscalationPolicy
	AgentObjectiveTemplate                = kernelagent.ObjectiveTemplate
	AgentEvaluationCriterion              = kernelagent.EvaluationCriterion
	AgentAmendmentPolicy                  = kernelagent.AmendmentPolicy
	AgentDefinitionProvenance             = kernelagent.DefinitionProvenance
	AgentDeployment                       = kernelagent.AgentDeployment
	AgentDeploymentRestrictions           = kernelagent.DeploymentRestrictions
	AgentDeploymentCapacity               = kernelagent.DeploymentCapacity
	AgentDeploymentHealth                 = kernelagent.DeploymentHealth
	AgentDefinitionActivation             = kernelagent.DefinitionActivation
	AgentRolloutStatus                    = kernelagent.RolloutStatus
	AgentDefinitionAmendment              = kernelagent.DefinitionAmendment
	AgentAmendmentStatus                  = kernelagent.AmendmentStatus
	AgentDefinitionFieldChange            = kernelagent.DefinitionFieldChange
	AgentAmendmentEvaluation              = kernelagent.AmendmentEvaluation
	AgentAmendmentDecision                = kernelagent.AmendmentDecision
	ProposeAgentAmendmentRequest          = kernelagent.ProposeAmendmentRequest
	SubmitAgentAmendmentEvaluationRequest = kernelagent.SubmitAmendmentEvaluationRequest
	ResolveAgentAmendmentRequest          = kernelagent.ResolveAmendmentRequest
	AgentRegistryStore                    = kernelagent.Store

	RunRecord                   = runtime.RunRecord
	RetryPolicy                 = runtime.RetryPolicy
	ExecutionStore              = runtime.ExecutionStore
	PortfolioStore              = runtime.PortfolioStore
	KernelStore                 = runtime.KernelStore
	PostgresStore               = runtime.PostgresStore
	PostgresStoreOption         = runtime.PostgresStoreOption
	Scope                       = runtime.Scope
	ObjectiveOwner              = runtime.ObjectiveOwner
	Objective                   = runtime.Objective
	ObjectiveStatus             = runtime.ObjectiveStatus
	ObjectiveFilter             = runtime.ObjectiveFilter
	AgentRun                    = runtime.AgentRun
	AgentRunStatus              = runtime.AgentRunStatus
	AgentRunFilter              = runtime.AgentRunFilter
	RunSource                   = runtime.RunSource
	WakeCondition               = runtime.WakeCondition
	CreateObjectiveRequest      = runtime.CreateObjectiveRequest
	UpdateObjectiveRequest      = runtime.UpdateObjectiveRequest
	CreateAgentRunRequest       = runtime.CreateAgentRunRequest
	RunActivityStore            = runtime.RunActivityStore
	ActivityEvent               = runtime.ActivityEvent
	ActivityActor               = runtime.ActivityActor
	ActivityFilter              = runtime.ActivityFilter
	ActivitySeverity            = runtime.ActivitySeverity
	ActivityVisibility          = runtime.ActivityVisibility
	RunTransitionRequest        = runtime.RunTransitionRequest
	AgentTurnStore              = runtime.AgentTurnStore
	AgentTurn                   = runtime.AgentTurn
	AgentTurnStatus             = runtime.AgentTurnStatus
	AgentTurnFilter             = runtime.AgentTurnFilter
	TurnDecision                = runtime.TurnDecision
	TurnAction                  = runtime.TurnAction
	TurnUsage                   = runtime.TurnUsage
	BeginAgentTurnRequest       = runtime.BeginAgentTurnRequest
	FinishAgentTurnRequest      = runtime.FinishAgentTurnRequest
	TurnExecutionContext        = runtime.TurnExecutionContext
	TurnRunner                  = runtime.TurnRunner
	TurnRunnerFunc              = runtime.TurnRunnerFunc
	TurnOutcome                 = runtime.TurnOutcome
	AdvanceAgentRunRequest      = runtime.AdvanceAgentRunRequest
	AdvanceAgentRunResult       = runtime.AdvanceAgentRunResult
	AgentRunScheduleStore       = runtime.AgentRunScheduleStore
	AgentRunClaimRequest        = runtime.AgentRunClaimRequest
	AgentRunWorkerConfig        = runtime.AgentRunWorkerConfig
	TurnRunnerBinding           = runtime.TurnRunnerBinding
	TurnRunnerResolver          = runtime.TurnRunnerResolver
	TurnRunnerResolverFunc      = runtime.TurnRunnerResolverFunc
	WakeSignal                  = runtime.WakeSignal
	WokenRun                    = runtime.WokenRun
	WakeResult                  = runtime.WakeResult
	SkillCatalog                = skill.Catalog
	SkillDefinition             = skill.Definition
	SkillAction                 = skill.Action
	SkillBinding                = skill.Binding
	SkillScope                  = skill.ScopeReference
	SkillRiskLevel              = skill.RiskLevel
	SkillSideEffect             = skill.SideEffect
	SkillIdempotencyMode        = skill.IdempotencyMode
	SkillCredentialRequirement  = skill.CredentialRequirement
	SkillCredentialReference    = skill.CredentialReference
	SkillTransportReference     = skill.TransportReference
	SkillTransportArgument      = skill.TransportArgument
	SkillArgumentRule           = skill.ArgumentRule
	SkillActionRetryPolicy      = skill.ActionRetryPolicy
	ModelSkillAction            = skill.ModelAction
	ModelSkillPrompt            = skill.ModelPrompt
	BoundSkillAction            = skill.BoundAction
	SkillPromptModule           = skill.PromptModule
	SkillHostCapabilityState    = skill.HostCapabilityState
	SkillAvailabilityReason     = skill.AvailabilityReason
	ActivatedSkill              = skill.ActivatedSkill
	UnavailableSkill            = skill.UnavailableSkill
	SkillActivationSnapshot     = skill.ActivationSnapshot
	SkillCatalogStore           = skill.CatalogStore
	ActionStore                 = runtime.ActionStore
	ActionCall                  = runtime.ActionCall
	ActionCallStatus            = runtime.ActionCallStatus
	ActionFilter                = runtime.ActionFilter
	ActionDisposition           = runtime.ActionDisposition
	ActionPolicyInput           = runtime.ActionPolicyInput
	ActionPolicyDecision        = runtime.ActionPolicyDecision
	ActionPolicyEvaluator       = runtime.ActionPolicyEvaluator
	ActionPolicyEvaluatorFunc   = runtime.ActionPolicyEvaluatorFunc
	ProposeActionRequest        = runtime.ProposeActionRequest
	ActionProposalResult        = runtime.ActionProposalResult
	ApprovalCheckpoint          = runtime.ApprovalCheckpoint
	ApprovalStatus              = runtime.ApprovalStatus
	ApprovalPrincipal           = runtime.ApprovalPrincipal
	ApprovalFilter              = runtime.ApprovalFilter
	ApprovalAuthorizer          = runtime.ApprovalAuthorizer
	ApprovalAuthorizerFunc      = runtime.ApprovalAuthorizerFunc
	ResolveApprovalRequest      = runtime.ResolveApprovalRequest
	ApprovalResolutionResult    = runtime.ApprovalResolutionResult
	ActionWorkerConfig          = runtime.ActionWorkerConfig
	DynamicActionWorkerConfig   = runtime.DynamicActionWorkerConfig
	ActionWorkerScopeSource     = runtime.ActionWorkerScopeSource
	ActionWorkerScopeSourceFunc = runtime.ActionWorkerScopeSourceFunc
	CredentialResolver          = runtime.CredentialResolver
	CredentialResolverFunc      = runtime.CredentialResolverFunc
	CredentialResolutionRequest = runtime.CredentialResolutionRequest
	ActionDispatchInput         = runtime.ActionDispatchInput
	ActionDispatcher            = runtime.ActionDispatcher
	ActionDispatcherFunc        = runtime.ActionDispatcherFunc
	ToolInvoker                 = runtime.ToolInvoker
	ToolInvokerFunc             = runtime.ToolInvokerFunc
	ToolInvocation              = runtime.ToolInvocation
	ToolActionDispatcher        = runtime.ToolActionDispatcher
	OpenClawSkillSource         = skillopenclaw.Source
	OpenClawSkillFile           = skillopenclaw.File
	OpenClawSkillBundle         = skillopenclaw.Bundle
	OpenClawSkillDiagnostic     = skillopenclaw.Diagnostic
	OpenClawSkillCompilation    = skillopenclaw.Compilation
	SkillSourceRootKind         = skillsource.RootKind
	SkillSourceRoot             = skillsource.Root
	SkillSourceCandidate        = skillsource.Candidate
	SkillSourceShadowed         = skillsource.ShadowedCandidate
	SkillSourceDiagnostic       = skillsource.Diagnostic
	SkillSourceSnapshot         = skillsource.Snapshot
	SkillSourceCatalog          = skillsource.Catalog
	SkillSourceChange           = skillsource.Change
	SkillSourceWatcher          = skillsource.Watcher
	ClawHubRegistry             = clawhub.Registry
	ClawHubClient               = clawhub.ClawHubClient
	ClawHubSkillReference       = clawhub.SkillReference
	ClawHubSearchRequest        = clawhub.SearchRequest
	ClawHubExploreRequest       = clawhub.ExploreRequest
	ClawHubSkillPage            = clawhub.SkillPage
	ClawHubSkillSummary         = clawhub.SkillSummary
	ClawHubSkillDetail          = clawhub.SkillDetail
	ClawHubInstallRequest       = clawhub.InstallRequest
	ClawHubInstalledSkill       = clawhub.InstalledSkill
	ClawHubVerification         = clawhub.Verification
	ClawHubVersionPage          = clawhub.VersionPage
	ClawHubVersionDetail        = clawhub.VersionDetail
	ClawHubDownloadedArchive    = clawhub.DownloadedArchive
)

// PersistentKernelStore is the complete durable control-plane contract for an
// embedded OpenSeal engine. Enterprise adapters implement this interface so
// runtime state, agent definitions/deployments, and skill bindings share one
// authoritative persistence boundary.
type PersistentKernelStore interface {
	runtime.KernelStore
	kernelagent.Store
	skill.CatalogStore
}

var _ PersistentKernelStore = (*runtime.PostgresStore)(nil)

func NewToolActionDispatcher(invoker runtime.ToolInvoker) (*runtime.ToolActionDispatcher, error) {
	return runtime.NewToolActionDispatcher(invoker)
}

// NewPostgresStore opens the production-grade shared persistence adapter and
// applies OpenSeal's versioned schema migrations.
func NewPostgresStore(ctx context.Context, dsn string, options ...runtime.PostgresStoreOption) (*runtime.PostgresStore, error) {
	return runtime.NewPostgresStore(ctx, dsn, options...)
}

func WithPostgresSchema(schema string) runtime.PostgresStoreOption {
	return runtime.WithPostgresSchema(schema)
}

func CompileOpenClawSkill(bundle skillopenclaw.Bundle) (*skillopenclaw.Compilation, error) {
	return skillopenclaw.Compile(bundle)
}

func ExportOpenClawSkill(compilation *skillopenclaw.Compilation) (skillopenclaw.Bundle, error) {
	return skillopenclaw.ExportBundle(compilation)
}

func NewSkillSourceCatalog() *skillsource.Catalog {
	return skillsource.NewCatalog()
}

func NewSkillSourceWatcher(catalog *skillsource.Catalog, roots []skillsource.Root, interval time.Duration) (*skillsource.Watcher, error) {
	return skillsource.NewWatcher(catalog, roots, interval)
}

const (
	OwnerTypeAgent = runtime.OwnerTypeAgent
	OwnerTypeTeam  = runtime.OwnerTypeTeam

	ObjectiveStatusDraft     = runtime.ObjectiveStatusDraft
	ObjectiveStatusActive    = runtime.ObjectiveStatusActive
	ObjectiveStatusPaused    = runtime.ObjectiveStatusPaused
	ObjectiveStatusSatisfied = runtime.ObjectiveStatusSatisfied
	ObjectiveStatusFailed    = runtime.ObjectiveStatusFailed
	ObjectiveStatusRetired   = runtime.ObjectiveStatusRetired

	RunSourceManual    = runtime.RunSourceManual
	RunSourceChat      = runtime.RunSourceChat
	RunSourceSchedule  = runtime.RunSourceSchedule
	RunSourceEvent     = runtime.RunSourceEvent
	RunSourceWebhook   = runtime.RunSourceWebhook
	RunSourceHandoff   = runtime.RunSourceHandoff
	RunSourceObjective = runtime.RunSourceObjective

	AgentRunStatusQueued               = runtime.AgentRunStatusQueued
	AgentRunStatusPlanning             = runtime.AgentRunStatusPlanning
	AgentRunStatusRunning              = runtime.AgentRunStatusRunning
	AgentRunStatusSleeping             = runtime.AgentRunStatusSleeping
	AgentRunStatusWaitingForDependency = runtime.AgentRunStatusWaitingForDependency
	AgentRunStatusWaitingForAgent      = runtime.AgentRunStatusWaitingForAgent
	AgentRunStatusWaitingForApproval   = runtime.AgentRunStatusWaitingForApproval
	AgentRunStatusWaitingForEvent      = runtime.AgentRunStatusWaitingForEvent
	AgentRunStatusCompleted            = runtime.AgentRunStatusCompleted
	AgentRunStatusFailed               = runtime.AgentRunStatusFailed
	AgentRunStatusCanceled             = runtime.AgentRunStatusCanceled

	ActivitySeverityDebug   = runtime.ActivitySeverityDebug
	ActivitySeverityInfo    = runtime.ActivitySeverityInfo
	ActivitySeverityWarning = runtime.ActivitySeverityWarning
	ActivitySeverityError   = runtime.ActivitySeverityError

	ActivityVisibilityPrivate = runtime.ActivityVisibilityPrivate
	ActivityVisibilityTeam    = runtime.ActivityVisibilityTeam
	ActivityVisibilityScope   = runtime.ActivityVisibilityScope

	AgentTurnStatusRunning   = runtime.AgentTurnStatusRunning
	AgentTurnStatusCompleted = runtime.AgentTurnStatusCompleted
	AgentTurnStatusFailed    = runtime.AgentTurnStatusFailed
	AgentTurnStatusCanceled  = runtime.AgentTurnStatusCanceled

	SkillRiskRead        = skill.RiskLevelRead
	SkillRiskWrite       = skill.RiskLevelWrite
	SkillRiskExternal    = skill.RiskLevelExternal
	SkillRiskProduction  = skill.RiskLevelProduction
	SkillRiskDestructive = skill.RiskLevelDestructive

	SkillSideEffectNone        = skill.SideEffectNone
	SkillSideEffectRead        = skill.SideEffectRead
	SkillSideEffectWrite       = skill.SideEffectWrite
	SkillSideEffectExternal    = skill.SideEffectExternal
	SkillSideEffectDestructive = skill.SideEffectDestructive

	SkillIdempotencyNone      = skill.IdempotencyNone
	SkillIdempotencySupported = skill.IdempotencySupported
	SkillIdempotencyRequired  = skill.IdempotencyRequired

	ActionDispositionAllow           = runtime.ActionDispositionAllow
	ActionDispositionDeny            = runtime.ActionDispositionDeny
	ActionDispositionRequireApproval = runtime.ActionDispositionRequireApproval

	ActionCallStatusReady           = runtime.ActionCallStatusReady
	ActionCallStatusWaitingApproval = runtime.ActionCallStatusWaitingApproval
	ActionCallStatusDenied          = runtime.ActionCallStatusDenied
	ActionCallStatusRunning         = runtime.ActionCallStatusRunning
	ActionCallStatusSucceeded       = runtime.ActionCallStatusSucceeded
	ActionCallStatusFailed          = runtime.ActionCallStatusFailed

	ApprovalStatusPending  = runtime.ApprovalStatusPending
	ApprovalStatusApproved = runtime.ApprovalStatusApproved
	ApprovalStatusRejected = runtime.ApprovalStatusRejected
	ApprovalStatusExpired  = runtime.ApprovalStatusExpired

	SkillSourceWorkspace    = skillsource.RootWorkspace
	SkillSourceProjectAgent = skillsource.RootProjectAgent
	SkillSourcePersonal     = skillsource.RootPersonal
	SkillSourceManaged      = skillsource.RootManaged
	SkillSourceBundled      = skillsource.RootBundled
	SkillSourcePlugin       = skillsource.RootPlugin
	SkillSourceExtra        = skillsource.RootExtra

	AgentRolloutPending  = kernelagent.RolloutPending
	AgentRolloutActive   = kernelagent.RolloutActive
	AgentRolloutDegraded = kernelagent.RolloutDegraded
	AgentRolloutPaused   = kernelagent.RolloutPaused
	AgentRolloutRetired  = kernelagent.RolloutRetired

	AgentAmendmentEvaluating       = kernelagent.AmendmentEvaluating
	AgentAmendmentAwaitingApproval = kernelagent.AmendmentAwaitingApproval
	AgentAmendmentReady            = kernelagent.AmendmentReady
	AgentAmendmentApproved         = kernelagent.AmendmentApproved
	AgentAmendmentRejected         = kernelagent.AmendmentRejected
	AgentAmendmentEvaluationFailed = kernelagent.AmendmentEvaluationFailed
	AgentAmendmentActivated        = kernelagent.AmendmentActivated
)

// Engine is the primary entry point for OpenSeal.
// It wires together the registry, execution store, worker pool, and scheduler.
type Engine struct {
	registry              *executor.Registry
	store                 runtime.KernelStore
	pool                  *runtime.WorkerPool
	scheduler             *runtime.Scheduler
	portfolio             *runtime.PortfolioService
	activity              *runtime.RunActivityService
	turns                 *runtime.AgentTurnService
	turnsRun              *runtime.TurnCoordinator
	runQueue              *runtime.AgentRunScheduler
	wake                  *runtime.AgentRunWakeService
	actions               *runtime.ActionCoordinator
	approvals             *runtime.ApprovalCoordinator
	actionPolicy          runtime.ActionPolicyEvaluator
	approvalAuth          runtime.ApprovalAuthorizer
	clawHub               *clawhub.InstallManager
	clawHubRegistry       clawhub.Registry
	agentPoolSpecs        []agentRunWorkerSpec
	agentPools            []*runtime.AgentRunWorkerPool
	actionPoolSpecs       []actionWorkerSpec
	actionPools           []*runtime.ActionWorkerPool
	actionSupervisorSpecs []actionWorkerSupervisorSpec
	actionSupervisors     []*runtime.ActionWorkerSupervisor
	skills                *skill.Catalog
	agents                *kernelagent.Registry
	logger                *zap.SugaredLogger
}

type agentRunWorkerSpec struct {
	config   runtime.AgentRunWorkerConfig
	resolver runtime.TurnRunnerResolver
}

type actionWorkerSpec struct {
	config      runtime.ActionWorkerConfig
	credentials runtime.CredentialResolver
	dispatcher  runtime.ActionDispatcher
}

type actionWorkerSupervisorSpec struct {
	config      runtime.DynamicActionWorkerConfig
	source      runtime.ActionWorkerScopeSource
	credentials runtime.CredentialResolver
	dispatcher  runtime.ActionDispatcher
}

// Option configures an Engine.
type Option func(*Engine) error

// New creates an Engine with the given options.
// Defaults: MemoryStore (100 runs), 4 workers, default retry policy.
func New(opts ...Option) (*Engine, error) {
	logger, _ := zap.NewProduction()
	sugar := logger.Sugar()

	reg := executor.NewRegistry(nil)
	store := runtime.NewMemoryStore(100)
	pool := runtime.NewWorkerPool(
		executor.NewPipelineExecutor(reg, sugar),
		store,
		sugar,
		4,
		runtime.DefaultRetryPolicy(),
	)

	e := &Engine{
		registry:     reg,
		store:        store,
		pool:         pool,
		scheduler:    runtime.NewScheduler(pool, store),
		portfolio:    runtime.NewPortfolioService(store),
		activity:     runtime.NewRunActivityService(store, store),
		turns:        runtime.NewAgentTurnService(store, store),
		turnsRun:     runtime.NewTurnCoordinator(store, store, store),
		runQueue:     runtime.NewAgentRunScheduler(store),
		wake:         runtime.NewAgentRunWakeService(store, store),
		skills:       skill.NewCatalog(),
		agents:       kernelagent.NewRegistry(),
		actionPolicy: runtime.NewDefaultActionPolicy(),
		approvalAuth: runtime.EligibleApprovalAuthorizer{},
		logger:       sugar,
	}

	for _, opt := range opts {
		if err := opt(e); err != nil {
			return nil, fmt.Errorf("engine option: %w", err)
		}
	}
	if err := e.restoreClawHubSkills(); err != nil {
		return nil, fmt.Errorf("restore ClawHub skills: %w", err)
	}
	e.rebuildGovernance()
	if err := e.rebuildActionWorkerPools(); err != nil {
		return nil, fmt.Errorf("action worker configuration: %w", err)
	}
	if err := e.rebuildActionWorkerSupervisors(); err != nil {
		return nil, fmt.Errorf("dynamic action worker configuration: %w", err)
	}
	if err := e.rebuildAgentWorkerPools(); err != nil {
		return nil, fmt.Errorf("agent worker configuration: %w", err)
	}

	return e, nil
}

// Start begins background goroutines (worker pool).
func (e *Engine) Start(ctx context.Context) {
	e.pool.Start(ctx)
	for _, pool := range e.agentPools {
		pool.Start(ctx)
	}
	for _, pool := range e.actionPools {
		pool.Start(ctx)
	}
	for _, supervisor := range e.actionSupervisors {
		supervisor.Start(ctx)
	}
}

// Stop gracefully shuts down background goroutines.
func (e *Engine) Stop() {
	for _, supervisor := range e.actionSupervisors {
		supervisor.Stop()
	}
	for _, pool := range e.actionPools {
		pool.Stop()
	}
	for _, pool := range e.agentPools {
		pool.Stop()
	}
	e.pool.Stop()
}

// ExecuteWorkflow runs a workflow synchronously and returns the result.
// For async execution, use ScheduleWorkflow.
func (e *Engine) ExecuteWorkflow(
	ctx context.Context,
	nodes []*executor.NodeDefinition,
	connections []*executor.ConnectionDefinition,
	startNodeID string,
	triggerData map[string]interface{},
) (*executor.ExecutionResult, error) {
	pe := executor.NewPipelineExecutor(e.registry, e.logger)
	return pe.Execute(ctx, 0, nodes, connections, startNodeID, triggerData, nil)
}

// ScheduleWorkflow enqueues a workflow for async execution.
// Returns the run ID immediately. Check the store for completion status.
func (e *Engine) ScheduleWorkflow(
	ctx context.Context,
	workflowName string,
	nodes []*executor.NodeDefinition,
	connections []*executor.ConnectionDefinition,
	startNodeID string,
	triggerData map[string]interface{},
) (int, error) {
	entry := runtime.WorkflowEntry{
		Name:        workflowName,
		Nodes:       nodes,
		Connections: connections,
		StartNodeID: startNodeID,
	}
	return e.scheduler.Schedule(ctx, entry, triggerData)
}

// Registry returns the executor registry for registering custom executors.
func (e *Engine) Registry() *executor.Registry {
	return e.registry
}

// Store returns the execution store.
func (e *Engine) Store() runtime.KernelStore {
	return e.store
}

// Logger returns the engine's logger.
func (e *Engine) Logger() *zap.SugaredLogger {
	return e.logger
}

// WithRegistry replaces the default registry.
func WithRegistry(reg *executor.Registry) Option {
	return func(e *Engine) error {
		e.registry = reg
		// Rebuild pool with new registry
		pe := executor.NewPipelineExecutor(reg, e.logger)
		e.pool = runtime.NewWorkerPool(pe, e.store, e.logger, 4, runtime.DefaultRetryPolicy())
		e.scheduler = runtime.NewScheduler(e.pool, e.store)
		return nil
	}
}

// WithStore replaces the default in-memory store.
func WithStore(store runtime.KernelStore) Option {
	return func(e *Engine) error {
		e.store = store
		e.pool.SetStore(store)
		e.scheduler = runtime.NewScheduler(e.pool, store)
		e.portfolio = runtime.NewPortfolioService(store)
		e.activity = runtime.NewRunActivityService(store, store)
		e.turns = runtime.NewAgentTurnService(store, store)
		e.turnsRun = runtime.NewTurnCoordinator(store, store, store)
		e.runQueue = runtime.NewAgentRunScheduler(store)
		e.wake = runtime.NewAgentRunWakeService(store, store)
		if agentStore, ok := store.(kernelagent.Store); ok {
			e.agents = kernelagent.NewRegistryWithStore(agentStore)
		}
		if skillStore, ok := store.(skill.CatalogStore); ok {
			e.skills = skill.NewCatalogWithStore(skillStore)
		}
		return nil
	}
}

// WithPersistentStore wires the full durable kernel contract. Use this option
// for production deployments; WithStore remains available for runtime-only
// embeddings and tests.
func WithPersistentStore(store PersistentKernelStore) Option {
	return WithStore(store)
}

// WithLogger replaces the default logger.
func WithLogger(logger *zap.SugaredLogger) Option {
	return func(e *Engine) error {
		e.logger = logger
		return nil
	}
}

// WithWorkerPool configures the async worker pool.
func WithWorkerPool(concurrency int, retry *runtime.RetryPolicy) Option {
	return func(e *Engine) error {
		pe := executor.NewPipelineExecutor(e.registry, e.logger)
		e.pool = runtime.NewWorkerPool(pe, e.store, e.logger, concurrency, retry)
		e.scheduler = runtime.NewScheduler(e.pool, e.store)
		return nil
	}
}

// WithAgentRunWorkers adds a scope-bound autonomous portfolio worker pool.
// The option may be repeated for additional scopes or agent deployments.
func WithAgentRunWorkers(config runtime.AgentRunWorkerConfig, resolver runtime.TurnRunnerResolver) Option {
	return func(e *Engine) error {
		if resolver == nil {
			return fmt.Errorf("turn runner resolver is required")
		}
		e.agentPoolSpecs = append(e.agentPoolSpecs, agentRunWorkerSpec{config: config, resolver: resolver})
		return nil
	}
}

func WithSkillCatalog(catalog *skill.Catalog) Option {
	return func(e *Engine) error {
		if catalog == nil {
			return fmt.Errorf("skill catalog is required")
		}
		e.skills = catalog
		return nil
	}
}

func WithActionPolicy(policy runtime.ActionPolicyEvaluator) Option {
	return func(e *Engine) error {
		if policy == nil {
			return fmt.Errorf("action policy is required")
		}
		e.actionPolicy = policy
		return nil
	}
}

func WithActionWorkers(config runtime.ActionWorkerConfig, credentials runtime.CredentialResolver, dispatcher runtime.ActionDispatcher) Option {
	return func(e *Engine) error {
		if dispatcher == nil {
			return fmt.Errorf("action dispatcher is required")
		}
		e.actionPoolSpecs = append(e.actionPoolSpecs, actionWorkerSpec{config: config, credentials: credentials, dispatcher: dispatcher})
		return nil
	}
}

// WithDynamicActionWorkers reconciles one isolated worker pool per active
// scope supplied by the embedding control plane. The option may be repeated
// for independent scope sources or transport hosts.
func WithDynamicActionWorkers(config runtime.DynamicActionWorkerConfig, source runtime.ActionWorkerScopeSource, credentials runtime.CredentialResolver, dispatcher runtime.ActionDispatcher) Option {
	return func(e *Engine) error {
		if source == nil {
			return fmt.Errorf("action worker scope source is required")
		}
		if dispatcher == nil {
			return fmt.Errorf("action dispatcher is required")
		}
		e.actionSupervisorSpecs = append(e.actionSupervisorSpecs, actionWorkerSupervisorSpec{
			config: config, source: source, credentials: credentials, dispatcher: dispatcher,
		})
		return nil
	}
}

func WithClawHubRegistry(registryID string, registry clawhub.Registry, workspace string) Option {
	return func(e *Engine) error {
		manager, err := clawhub.NewInstallManager(registryID, registry, workspace)
		if err != nil {
			return err
		}
		e.clawHub = manager
		e.clawHubRegistry = registry
		return nil
	}
}

func WithApprovalAuthorizer(authorizer runtime.ApprovalAuthorizer) Option {
	return func(e *Engine) error {
		if authorizer == nil {
			return fmt.Errorf("approval authorizer is required")
		}
		e.approvalAuth = authorizer
		return nil
	}
}

func (e *Engine) rebuildGovernance() {
	e.actions = runtime.NewActionCoordinator(e.store, e.store, e.skills, e.actionPolicy)
	e.approvals = runtime.NewApprovalCoordinator(e.store, e.store, e.approvalAuth)
}

func (e *Engine) restoreClawHubSkills() error {
	if e.clawHub == nil {
		return nil
	}
	installed, err := e.clawHub.LoadInstalled()
	if err != nil {
		return err
	}
	for _, item := range installed {
		if err := e.activateInstalledSkill(context.Background(), item); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) activateInstalledSkill(ctx context.Context, installed *clawhub.InstalledSkill) error {
	if installed == nil || installed.Compilation == nil || installed.Compilation.Definition == nil {
		return fmt.Errorf("installed skill compilation is required")
	}
	definition := installed.Compilation.Definition
	if err := e.validateClawHubCompilation(installed.Compilation); err != nil {
		return err
	}
	existing, err := e.skills.GetDefinition(ctx, definition.ID, definition.Version)
	if err != nil {
		return err
	}
	if existing != nil {
		if existing.Source != nil && definition.Source != nil && existing.Source.Digest == definition.Source.Digest {
			return nil
		}
		return fmt.Errorf("skill %s@%s conflicts with an active immutable definition", definition.ID, definition.Version)
	}
	return e.skills.Register(ctx, definition)
}

func (e *Engine) validateClawHubCompilation(compilation *skillopenclaw.Compilation) error {
	if compilation == nil || compilation.Definition == nil {
		return fmt.Errorf("compiled skill definition is required")
	}
	definition := compilation.Definition
	validationCatalog := skill.NewCatalog()
	if err := validationCatalog.Register(context.Background(), definition); err != nil {
		return err
	}
	existing, err := e.skills.GetDefinition(context.Background(), definition.ID, definition.Version)
	if err != nil {
		return err
	}
	if existing != nil && (existing.Source == nil || definition.Source == nil || existing.Source.Digest != definition.Source.Digest) {
		return fmt.Errorf("skill %s@%s conflicts with an active immutable definition", definition.ID, definition.Version)
	}
	return nil
}

func (e *Engine) rebuildActionWorkerPools() error {
	e.actionPools = make([]*runtime.ActionWorkerPool, 0, len(e.actionPoolSpecs))
	for _, spec := range e.actionPoolSpecs {
		pool, err := runtime.NewActionWorkerPool(e.store, e.skills, spec.credentials, spec.dispatcher, e.logger, spec.config)
		if err != nil {
			return err
		}
		e.actionPools = append(e.actionPools, pool)
	}
	return nil
}

func (e *Engine) rebuildActionWorkerSupervisors() error {
	e.actionSupervisors = make([]*runtime.ActionWorkerSupervisor, 0, len(e.actionSupervisorSpecs))
	for _, spec := range e.actionSupervisorSpecs {
		supervisor, err := runtime.NewActionWorkerSupervisor(e.store, e.skills, spec.credentials, spec.dispatcher, spec.source, e.logger, spec.config)
		if err != nil {
			return err
		}
		e.actionSupervisors = append(e.actionSupervisors, supervisor)
	}
	return nil
}

func (e *Engine) rebuildAgentWorkerPools() error {
	e.agentPools = make([]*runtime.AgentRunWorkerPool, 0, len(e.agentPoolSpecs))
	for _, spec := range e.agentPoolSpecs {
		pool, err := runtime.NewAgentRunWorkerPool(e.store, spec.resolver, e.logger, spec.config)
		if err != nil {
			return err
		}
		e.agentPools = append(e.agentPools, pool)
	}
	return nil
}

func (e *Engine) WakeAgentWorkers() {
	for _, pool := range e.agentPools {
		pool.Wake()
	}
}

func (e *Engine) WakeActionWorkers() {
	for _, pool := range e.actionPools {
		pool.Wake()
	}
	for _, supervisor := range e.actionSupervisors {
		supervisor.Wake()
	}
}

// BuildGraph is a convenience wrapper for executor.BuildGraph.
func BuildGraph(nodes []*executor.NodeDefinition, connections []*executor.ConnectionDefinition) (*executor.ExecutionGraph, error) {
	return executor.BuildGraph(nodes, connections)
}

func (e *Engine) CreateObjective(ctx context.Context, req runtime.CreateObjectiveRequest) (*runtime.Objective, error) {
	return e.portfolio.CreateObjective(ctx, req)
}

func (e *Engine) GetObjective(ctx context.Context, scope runtime.Scope, objectiveID string) (*runtime.Objective, error) {
	return e.portfolio.GetObjective(ctx, scope, objectiveID)
}

func (e *Engine) ListObjectives(ctx context.Context, filter runtime.ObjectiveFilter) ([]*runtime.Objective, error) {
	return e.portfolio.ListObjectives(ctx, filter)
}

func (e *Engine) UpdateObjective(ctx context.Context, scope runtime.Scope, objectiveID string, req runtime.UpdateObjectiveRequest) (*runtime.Objective, error) {
	return e.portfolio.UpdateObjective(ctx, scope, objectiveID, req)
}

func (e *Engine) CreateAgentRun(ctx context.Context, req runtime.CreateAgentRunRequest) (*runtime.AgentRun, error) {
	return e.portfolio.CreateAgentRun(ctx, req)
}

func (e *Engine) GetAgentRun(ctx context.Context, scope runtime.Scope, runID string) (*runtime.AgentRun, error) {
	return e.portfolio.GetAgentRun(ctx, scope, runID)
}

func (e *Engine) ListAgentRuns(ctx context.Context, filter runtime.AgentRunFilter) ([]*runtime.AgentRun, error) {
	return e.portfolio.ListAgentRuns(ctx, filter)
}

func (e *Engine) TransitionAgentRun(ctx context.Context, scope runtime.Scope, runID string, req runtime.RunTransitionRequest) (*runtime.AgentRun, *runtime.ActivityEvent, error) {
	return e.activity.TransitionRun(ctx, scope, runID, req)
}

func (e *Engine) AppendActivity(ctx context.Context, event *runtime.ActivityEvent) (*runtime.ActivityEvent, error) {
	return e.activity.AppendActivity(ctx, event)
}

func (e *Engine) ListActivity(ctx context.Context, filter runtime.ActivityFilter) ([]*runtime.ActivityEvent, error) {
	return e.activity.ListActivity(ctx, filter)
}

func (e *Engine) BeginAgentTurn(ctx context.Context, req runtime.BeginAgentTurnRequest) (*runtime.AgentTurn, error) {
	return e.turns.BeginTurn(ctx, req)
}

func (e *Engine) FinishAgentTurn(ctx context.Context, scope runtime.Scope, turnID string, req runtime.FinishAgentTurnRequest) (*runtime.AgentTurn, error) {
	return e.turns.FinishTurn(ctx, scope, turnID, req)
}

func (e *Engine) GetAgentTurn(ctx context.Context, scope runtime.Scope, turnID string) (*runtime.AgentTurn, error) {
	return e.turns.GetTurn(ctx, scope, turnID)
}

func (e *Engine) ListAgentTurns(ctx context.Context, filter runtime.AgentTurnFilter) ([]*runtime.AgentTurn, error) {
	return e.turns.ListTurns(ctx, filter)
}

// AdvanceAgentRun performs or reconciles one bounded, provider-neutral agent
// turn. The runner proposes actions; governed side effects execute separately.
func (e *Engine) AdvanceAgentRun(ctx context.Context, req runtime.AdvanceAgentRunRequest, runner runtime.TurnRunner) (*runtime.AdvanceAgentRunResult, error) {
	return e.turnsRun.Advance(ctx, req, runner)
}

func (e *Engine) ClaimNextAgentRun(ctx context.Context, req runtime.AgentRunClaimRequest) (*runtime.AgentRun, error) {
	return e.runQueue.ClaimNext(ctx, req)
}

func (e *Engine) RenewAgentRunLease(ctx context.Context, scope runtime.Scope, runID, workerID string, leaseDuration time.Duration) (*runtime.AgentRun, error) {
	return e.runQueue.RenewLease(ctx, scope, runID, workerID, leaseDuration)
}

func (e *Engine) WakeAgentRuns(ctx context.Context, signal runtime.WakeSignal) (*runtime.WakeResult, error) {
	return e.wake.Wake(ctx, signal)
}

func (e *Engine) WakeDueAgentRuns(ctx context.Context, scope runtime.Scope, at time.Time) (*runtime.WakeResult, error) {
	return e.wake.WakeDueTimers(ctx, scope, at)
}

func (e *Engine) RegisterSkill(ctx context.Context, definition *skill.Definition) error {
	return e.skills.Register(ctx, definition)
}

func (e *Engine) GetSkillDefinition(ctx context.Context, skillID, version string) (*skill.Definition, error) {
	return e.skills.GetDefinition(ctx, skillID, version)
}

func (e *Engine) BindSkill(ctx context.Context, binding *skill.Binding) error {
	return e.skills.Bind(ctx, binding)
}

func (e *Engine) ListModelSkillActions(ctx context.Context, scope skill.ScopeReference, deploymentID string) ([]skill.ModelAction, error) {
	return e.skills.ListModelActions(ctx, scope, deploymentID)
}

func (e *Engine) ListModelSkillPrompts(ctx context.Context, scope skill.ScopeReference, deploymentID string) ([]skill.ModelPrompt, error) {
	return e.skills.ListModelPrompts(ctx, scope, deploymentID)
}

func (e *Engine) ResolveSkillPrompt(ctx context.Context, scope skill.ScopeReference, deploymentID, skillID, version string) (*skill.PromptModule, error) {
	return e.skills.ResolvePrompt(ctx, scope, deploymentID, skillID, version)
}

func (e *Engine) ResolveSkillAction(ctx context.Context, scope skill.ScopeReference, deploymentID, skillID, version, action string) (*skill.BoundAction, error) {
	return e.skills.Resolve(ctx, scope, deploymentID, skillID, version, action)
}

func (e *Engine) ActivateSkills(ctx context.Context, scope skill.ScopeReference, deploymentID string, host skill.HostCapabilityState) (*skill.ActivationSnapshot, error) {
	return e.skills.Activate(ctx, scope, deploymentID, host)
}

func (e *Engine) RegisterAgentDefinition(ctx context.Context, definition *kernelagent.AgentDefinition) (*kernelagent.AgentDefinition, error) {
	return e.agents.RegisterDefinition(ctx, definition)
}

func (e *Engine) GetAgentDefinition(ctx context.Context, id, version string) (*kernelagent.AgentDefinition, error) {
	return e.agents.GetDefinition(ctx, id, version)
}

func (e *Engine) ListAgentDefinitionVersions(ctx context.Context, id string) ([]*kernelagent.AgentDefinition, error) {
	return e.agents.ListDefinitionVersions(ctx, id)
}

func (e *Engine) CreateAgentDeployment(ctx context.Context, deployment *kernelagent.AgentDeployment, actorType, actorID, reason string) (*kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, error) {
	return e.agents.CreateDeployment(ctx, deployment, actorType, actorID, reason)
}

func (e *Engine) GetAgentDeployment(ctx context.Context, scope skill.ScopeReference, deploymentID string) (*kernelagent.AgentDeployment, error) {
	return e.agents.GetDeployment(ctx, scope, deploymentID)
}

func (e *Engine) ActivateAgentDefinition(ctx context.Context, scope skill.ScopeReference, deploymentID, version string, expectedRevision int64, actorType, actorID, reason string) (*kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, error) {
	return e.agents.ActivateDefinition(ctx, scope, deploymentID, version, expectedRevision, actorType, actorID, reason)
}

func (e *Engine) RollbackAgentDefinition(ctx context.Context, scope skill.ScopeReference, deploymentID string, expectedRevision int64, actorType, actorID, reason string) (*kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, error) {
	return e.agents.RollbackDefinition(ctx, scope, deploymentID, expectedRevision, actorType, actorID, reason)
}

func (e *Engine) ListAgentDefinitionActivations(ctx context.Context, scope skill.ScopeReference, deploymentID string) ([]kernelagent.DefinitionActivation, error) {
	return e.agents.ListActivations(ctx, scope, deploymentID)
}

func (e *Engine) ProposeAgentDefinitionAmendment(ctx context.Context, request kernelagent.ProposeAmendmentRequest) (*kernelagent.DefinitionAmendment, error) {
	return e.agents.ProposeAmendment(ctx, request)
}

func (e *Engine) GetAgentDefinitionAmendment(ctx context.Context, scope skill.ScopeReference, amendmentID string) (*kernelagent.DefinitionAmendment, error) {
	return e.agents.GetAmendment(ctx, scope, amendmentID)
}

func (e *Engine) SubmitAgentDefinitionAmendmentEvaluation(ctx context.Context, request kernelagent.SubmitAmendmentEvaluationRequest) (*kernelagent.DefinitionAmendment, error) {
	return e.agents.SubmitAmendmentEvaluation(ctx, request)
}

func (e *Engine) ResolveAgentDefinitionAmendment(ctx context.Context, request kernelagent.ResolveAmendmentRequest) (*kernelagent.DefinitionAmendment, error) {
	return e.agents.ResolveAmendment(ctx, request)
}

func (e *Engine) ActivateAgentDefinitionAmendment(ctx context.Context, scope skill.ScopeReference, amendmentID string, expectedRevision int64, actorType, actorID, reason string) (*kernelagent.DefinitionAmendment, *kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, error) {
	return e.agents.ActivateAmendment(ctx, scope, amendmentID, expectedRevision, actorType, actorID, reason)
}

func (e *Engine) ValidateSkillInput(ctx context.Context, action *skill.BoundAction, input map[string]interface{}) error {
	return e.skills.ValidateInput(ctx, action, input)
}

func (e *Engine) ValidateSkillOutput(ctx context.Context, action *skill.BoundAction, output map[string]interface{}) error {
	return e.skills.ValidateOutput(ctx, action, output)
}

func (e *Engine) ProposeAction(ctx context.Context, req runtime.ProposeActionRequest) (*runtime.ActionProposalResult, error) {
	return e.actions.Propose(ctx, req)
}

func (e *Engine) ResolveApproval(ctx context.Context, req runtime.ResolveApprovalRequest) (*runtime.ApprovalResolutionResult, error) {
	return e.approvals.Resolve(ctx, req)
}

func (e *Engine) GetActionCall(ctx context.Context, scope runtime.Scope, actionID string) (*runtime.ActionCall, error) {
	return e.store.GetActionCall(ctx, scope, actionID)
}

func (e *Engine) ListActionCalls(ctx context.Context, filter runtime.ActionFilter) ([]*runtime.ActionCall, error) {
	return e.store.ListActionCalls(ctx, filter)
}

func (e *Engine) GetApproval(ctx context.Context, scope runtime.Scope, approvalID string) (*runtime.ApprovalCheckpoint, error) {
	return e.store.GetApproval(ctx, scope, approvalID)
}

func (e *Engine) ListApprovals(ctx context.Context, filter runtime.ApprovalFilter) ([]*runtime.ApprovalCheckpoint, error) {
	return e.store.ListApprovals(ctx, filter)
}

func NewClawHubClient(baseURL string) *clawhub.ClawHubClient {
	return clawhub.NewClawHubClient(baseURL)
}

func (e *Engine) SearchClawHubSkills(ctx context.Context, request clawhub.SearchRequest) (*clawhub.SkillPage, error) {
	if e.clawHubRegistry == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	return e.clawHubRegistry.SearchSkills(ctx, request)
}

func (e *Engine) ExploreClawHubSkills(ctx context.Context, request clawhub.ExploreRequest) (*clawhub.SkillPage, error) {
	if e.clawHubRegistry == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	return e.clawHubRegistry.ExploreSkills(ctx, request)
}

func (e *Engine) InspectClawHubSkill(ctx context.Context, reference clawhub.SkillReference) (*clawhub.SkillDetail, error) {
	if e.clawHubRegistry == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	return e.clawHubRegistry.InspectSkill(ctx, reference)
}

func (e *Engine) InstallClawHubSkill(ctx context.Context, request clawhub.InstallRequest) (*clawhub.InstalledSkill, error) {
	if e.clawHub == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	installed, err := e.clawHub.InstallValidated(ctx, request, e.validateClawHubCompilation)
	if err != nil {
		return nil, err
	}
	if err := e.activateInstalledSkill(ctx, installed); err != nil {
		return nil, err
	}
	return installed, nil
}

func (e *Engine) UpdateClawHubSkill(ctx context.Context, slug string) (*clawhub.InstalledSkill, error) {
	if e.clawHub == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	installed, err := e.clawHub.UpdateValidated(ctx, slug, e.validateClawHubCompilation)
	if err != nil {
		return nil, err
	}
	if err := e.activateInstalledSkill(ctx, installed); err != nil {
		return nil, err
	}
	return installed, nil
}

func (e *Engine) VerifyInstalledClawHubSkill(ctx context.Context, slug string) (*clawhub.Verification, error) {
	if e.clawHub == nil {
		return nil, fmt.Errorf("ClawHub registry is not configured")
	}
	return e.clawHub.VerifyInstalled(ctx, slug)
}

func (e *Engine) PinClawHubSkill(slug, reason string) error {
	if e.clawHub == nil {
		return fmt.Errorf("ClawHub registry is not configured")
	}
	return e.clawHub.Pin(slug, reason)
}

func (e *Engine) UnpinClawHubSkill(slug string) error {
	if e.clawHub == nil {
		return fmt.Errorf("ClawHub registry is not configured")
	}
	return e.clawHub.Unpin(slug)
}
