// Package openseal provides the stable public API for embedding OpenSeal
// as a library. Downstream consumers should import this package
// rather than reaching into individual pkg/ subpackages.
package openseal

import (
	"context"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/executor"
	"github.com/axiom-studio/openseal/pkg/runtime"
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

	AgentNodeDefinition = types.AgentNodeDefinition
	AgentConnection     = types.AgentConnection
	AgentLibraryBean    = types.AgentLibraryBean
	AgentInstanceBean   = types.AgentInstanceBean
	AgentWorkflow       = types.AgentWorkflow
	AgentWorkflowBean   = types.AgentWorkflowBean

	RunRecord              = runtime.RunRecord
	RetryPolicy            = runtime.RetryPolicy
	ExecutionStore         = runtime.ExecutionStore
	PortfolioStore         = runtime.PortfolioStore
	KernelStore            = runtime.KernelStore
	Scope                  = runtime.Scope
	ObjectiveOwner         = runtime.ObjectiveOwner
	Objective              = runtime.Objective
	ObjectiveStatus        = runtime.ObjectiveStatus
	ObjectiveFilter        = runtime.ObjectiveFilter
	AgentRun               = runtime.AgentRun
	AgentRunStatus         = runtime.AgentRunStatus
	AgentRunFilter         = runtime.AgentRunFilter
	RunSource              = runtime.RunSource
	WakeCondition          = runtime.WakeCondition
	CreateObjectiveRequest = runtime.CreateObjectiveRequest
	UpdateObjectiveRequest = runtime.UpdateObjectiveRequest
	CreateAgentRunRequest  = runtime.CreateAgentRunRequest
	RunActivityStore       = runtime.RunActivityStore
	ActivityEvent          = runtime.ActivityEvent
	ActivityActor          = runtime.ActivityActor
	ActivityFilter         = runtime.ActivityFilter
	ActivitySeverity       = runtime.ActivitySeverity
	ActivityVisibility     = runtime.ActivityVisibility
	RunTransitionRequest   = runtime.RunTransitionRequest
	AgentTurnStore         = runtime.AgentTurnStore
	AgentTurn              = runtime.AgentTurn
	AgentTurnStatus        = runtime.AgentTurnStatus
	AgentTurnFilter        = runtime.AgentTurnFilter
	TurnDecision           = runtime.TurnDecision
	TurnAction             = runtime.TurnAction
	TurnUsage              = runtime.TurnUsage
	BeginAgentTurnRequest  = runtime.BeginAgentTurnRequest
	FinishAgentTurnRequest = runtime.FinishAgentTurnRequest
)

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
)

// Engine is the primary entry point for OpenSeal.
// It wires together the registry, execution store, worker pool, and scheduler.
type Engine struct {
	registry  *executor.Registry
	store     runtime.KernelStore
	pool      *runtime.WorkerPool
	scheduler *runtime.Scheduler
	portfolio *runtime.PortfolioService
	activity  *runtime.RunActivityService
	turns     *runtime.AgentTurnService
	logger    *zap.SugaredLogger
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
		registry:  reg,
		store:     store,
		pool:      pool,
		scheduler: runtime.NewScheduler(pool, store),
		portfolio: runtime.NewPortfolioService(store),
		activity:  runtime.NewRunActivityService(store, store),
		turns:     runtime.NewAgentTurnService(store, store),
		logger:    sugar,
	}

	for _, opt := range opts {
		if err := opt(e); err != nil {
			return nil, fmt.Errorf("engine option: %w", err)
		}
	}

	return e, nil
}

// Start begins background goroutines (worker pool).
func (e *Engine) Start(ctx context.Context) {
	e.pool.Start(ctx)
}

// Stop gracefully shuts down background goroutines.
func (e *Engine) Stop() {
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
		return nil
	}
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
