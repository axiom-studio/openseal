// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/runtime/runloop. Remove once in-flight branches have landed.
//
// Exported declarations of pkg/runtime/runloop, kept so that importers of
// pkg/runtime continue to compile.

package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
	"go.uber.org/zap"
)

type GovernedToolCall = runloop.GovernedToolCall

type GovernedToolExecutorConfig = runloop.GovernedToolExecutorConfig

type GovernedToolExecutor = runloop.GovernedToolExecutor

// NewGovernedToolExecutor forwards to runloop.NewGovernedToolExecutor.
func NewGovernedToolExecutor(config runloop.GovernedToolExecutorConfig) (*runloop.GovernedToolExecutor, error) {
	return runloop.NewGovernedToolExecutor(config)
}

// GovernedBudgetExhaustion forwards to runloop.GovernedBudgetExhaustion.
func GovernedBudgetExhaustion(outcome runloop.GovernedToolOutcome) *agentrun.BudgetExhaustion {
	return runloop.GovernedBudgetExhaustion(outcome)
}

type ActionProposalObserver = runloop.ActionProposalObserver

type GovernedParkPlan = runloop.GovernedParkPlan

type GovernedToolOutcome = runloop.GovernedToolOutcome

type GovernedPark = runloop.GovernedPark

// GovernedEvidenceRef forwards to runloop.GovernedEvidenceRef.
func GovernedEvidenceRef(actionCallID string) string {
	return runloop.GovernedEvidenceRef(actionCallID)
}

const ActionErrorCodeImageProviderRejected = runloop.ActionErrorCodeImageProviderRejected

const ToolGuardImageProviderRejected = runloop.ToolGuardImageProviderRejected

type RepeatEffect = runloop.RepeatEffect

const RepeatEffectRead = runloop.RepeatEffectRead

const RepeatEffectWrite = runloop.RepeatEffectWrite

const RepeatEffectStateful = runloop.RepeatEffectStateful

const RepeatEffectControl = runloop.RepeatEffectControl

type LoopToolKind = runloop.LoopToolKind

const LoopToolGoverned = runloop.LoopToolGoverned

const LoopToolHost = runloop.LoopToolHost

const LoopToolKernel = runloop.LoopToolKernel

type RepeatSubject = runloop.RepeatSubject

// RepeatNoteSourceKey forwards to runloop.RepeatNoteSourceKey.
func RepeatNoteSourceKey(toolEntryID string) string {
	return runloop.RepeatNoteSourceKey(toolEntryID)
}

const ToolGuardSameStepDuplicate = runloop.ToolGuardSameStepDuplicate

const ToolGuardRunStopping = runloop.ToolGuardRunStopping

type StepToolDispatcherConfig = runloop.StepToolDispatcherConfig

type StepToolDispatcher = runloop.StepToolDispatcher

// NewStepToolDispatcher forwards to runloop.NewStepToolDispatcher.
func NewStepToolDispatcher(cfg runloop.StepToolDispatcherConfig) (*runloop.StepToolDispatcher, error) {
	return runloop.NewStepToolDispatcher(cfg)
}

type LoopToolClaim = runloop.LoopToolClaim

// LoopCapabilityCatalog forwards to runloop.LoopCapabilityCatalog.
func LoopCapabilityCatalog(run *models.AgentRun, binding *agentrun.TurnRunnerBinding, claim runloop.LoopToolClaim) modelstep.CapabilityCatalog {
	return runloop.LoopCapabilityCatalog(run, binding, claim)
}

// PlanToolEntries forwards to runloop.PlanToolEntries.
func PlanToolEntries(binding *agentrun.TurnRunnerBinding, assistant *models.SessionEntry) ([]*models.SessionEntry, error) {
	return runloop.PlanToolEntries(binding, assistant)
}

// PlanToolCallEntries forwards to runloop.PlanToolCallEntries.
func PlanToolCallEntries(binding *agentrun.TurnRunnerBinding, run *models.AgentRun, step *models.AgentTurn, sessionID string, calls []agentrun.LoopToolCall) ([]*models.SessionEntry, error) {
	return runloop.PlanToolCallEntries(binding, run, step, sessionID, calls)
}

const RunLoopMaxStepsPerClaim = runloop.RunLoopMaxStepsPerClaim

const RunLoopMaxClaimDuration = runloop.RunLoopMaxClaimDuration

const StepFailureHostUnavailable = runloop.StepFailureHostUnavailable

const StepFailureHostConfiguration = runloop.StepFailureHostConfiguration

const StepFailureContextExceeded = runloop.StepFailureContextExceeded

const StepFailureProducerFailed = runloop.StepFailureProducerFailed

const StepFailurePreparationFailed = runloop.StepFailurePreparationFailed

type StepToolEntryPlanner = runloop.StepToolEntryPlanner

type PlainTextFinalizer = runloop.PlainTextFinalizer

type RunLoopConfig = runloop.RunLoopConfig

type RunLoop = runloop.RunLoop

// NewRunLoop forwards to runloop.NewRunLoop.
func NewRunLoop(store models.KernelStore, actions *action.ActionCoordinator, logger *zap.SugaredLogger, config runloop.RunLoopConfig) (*runloop.RunLoop, error) {
	return runloop.NewRunLoop(store, actions, logger, config)
}

type RunLoopClaim = runloop.RunLoopClaim

type RunLoopExitReason = runloop.RunLoopExitReason

const RunLoopExitFinal = runloop.RunLoopExitFinal

const RunLoopExitParked = runloop.RunLoopExitParked

const RunLoopExitYielded = runloop.RunLoopExitYielded

const RunLoopExitRequeued = runloop.RunLoopExitRequeued

const RunLoopExitPaused = runloop.RunLoopExitPaused

const RunLoopExitFailed = runloop.RunLoopExitFailed

const RunLoopExitLeaseLost = runloop.RunLoopExitLeaseLost

const RunLoopExitEnded = runloop.RunLoopExitEnded

type RunLoopExit = runloop.RunLoopExit

const StepFailureBudgetExhausted = runloop.StepFailureBudgetExhausted

const StepFailureDurationBudget = runloop.StepFailureDurationBudget

const RunLoopCutoverCode = runloop.RunLoopCutoverCode

const RunLoopCutoverMessage = runloop.RunLoopCutoverMessage

type RunLoopCutoverConversations = runloop.RunLoopCutoverConversations

type RunLoopCutoverReport = runloop.RunLoopCutoverReport

// RunLoopCutover forwards to runloop.RunLoopCutover.
func RunLoopCutover(ctx context.Context, store models.RunLoopCutoverStore, conversations runloop.RunLoopCutoverConversations) (runloop.RunLoopCutoverReport, error) {
	return runloop.RunLoopCutover(ctx, store, conversations)
}

// ControlToolSpecs forwards to runloop.ControlToolSpecs.
func ControlToolSpecs(a agentrun.ControlAuthority) []control.ControlToolSpec {
	return runloop.ControlToolSpecs(a)
}

// ControlModelStepTools forwards to runloop.ControlModelStepTools.
func ControlModelStepTools(a agentrun.ControlAuthority) []models.ModelStepTool {
	return runloop.ControlModelStepTools(a)
}

const RoutineScriptRulesText = runloop.RoutineScriptRulesText

const RunFailureCodeRoutineScript = runloop.RunFailureCodeRoutineScript

// RateLimitedSourceKey forwards to runloop.RateLimitedSourceKey.
func RateLimitedSourceKey(runID, actionCallID string) string {
	return runloop.RateLimitedSourceKey(runID, actionCallID)
}
