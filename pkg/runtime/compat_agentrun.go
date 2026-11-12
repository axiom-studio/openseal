// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/runtime/agentrun. Remove once in-flight branches have landed.
//
// Exported declarations of pkg/runtime/agentrun, kept so that importers of
// pkg/runtime continue to compile.

package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type AgentRunClaimRequest = agentrun.AgentRunClaimRequest

type AgentRunScheduler = agentrun.AgentRunScheduler

// NewAgentRunScheduler forwards to agentrun.NewAgentRunScheduler.
func NewAgentRunScheduler(store models.AgentRunScheduleStore, opts ...models.ServiceOption) *agentrun.AgentRunScheduler {
	return agentrun.NewAgentRunScheduler(store, opts...)
}

type FinalAnswerMode = agentrun.FinalAnswerMode

const FinalModeDefault = agentrun.FinalModeDefault

const FinalModeDecision = agentrun.FinalModeDecision

const FinalModeReview = agentrun.FinalModeReview

type ControlAuthority = agentrun.ControlAuthority

type ControlOrigin = agentrun.ControlOrigin

type ControlContext = agentrun.ControlContext

type SessionForkRef = agentrun.SessionForkRef

// ControlChildSeedText forwards to agentrun.ControlChildSeedText.
func ControlChildSeedText(run *models.AgentRun) (string, bool) {
	return agentrun.ControlChildSeedText(run)
}

// VerifyConversationTaskWorkRun forwards to agentrun.VerifyConversationTaskWorkRun.
func VerifyConversationTaskWorkRun(ctx context.Context, store models.ConversationTaskProofStore, task *models.ConversationTask, run *models.AgentRun) (bool, error) {
	return agentrun.VerifyConversationTaskWorkRun(ctx, store, task, run)
}

var ErrConversationTaskNotFound = agentrun.ErrConversationTaskNotFound

// PersistedTaskOrigin forwards to agentrun.PersistedTaskOrigin.
func PersistedTaskOrigin(ctx context.Context, store models.ConversationTaskStore, scope models.Scope, workRunID string) (*models.ConversationTask, error) {
	return agentrun.PersistedTaskOrigin(ctx, store, scope, workRunID)
}

type FinalOutputPublisher = agentrun.FinalOutputPublisher

type FinalOutputPublication = agentrun.FinalOutputPublication

type TurnRunnerBinding = agentrun.TurnRunnerBinding

type PreparedSkillRuntime = agentrun.PreparedSkillRuntime

type StepHost = agentrun.StepHost

type HostedAgentTarget = agentrun.HostedAgentTarget

type HostedRunbookOperation = agentrun.HostedRunbookOperation

type ActionExecutionCatalog = agentrun.ActionExecutionCatalog

type ActionDispatchInput = agentrun.ActionDispatchInput

type ActionDispatcher = agentrun.ActionDispatcher

type ActionProposalValidationInput = agentrun.ActionProposalValidationInput

type RunActionHistory = agentrun.RunActionHistory

type AgentRunCommandKind = agentrun.AgentRunCommandKind

const AgentRunCommandPause = agentrun.AgentRunCommandPause

const AgentRunCommandResume = agentrun.AgentRunCommandResume

const AgentRunCommandRetry = agentrun.AgentRunCommandRetry

const AgentRunCommandCancel = agentrun.AgentRunCommandCancel

const AgentRunCommandIntervene = agentrun.AgentRunCommandIntervene

const AgentRunCommandResolveHumanIntervention = agentrun.AgentRunCommandResolveHumanIntervention

type AgentRunCommandRequest = agentrun.AgentRunCommandRequest

type AgentRunCommandResult = agentrun.AgentRunCommandResult

type RunCommandService = agentrun.RunCommandService

// NewRunCommandService forwards to agentrun.NewRunCommandService.
func NewRunCommandService(store models.RunCommandStore, opts ...models.ServiceOption) *agentrun.RunCommandService {
	return agentrun.NewRunCommandService(store, opts...)
}

var ErrRunCommandNotAuthorized = agentrun.ErrRunCommandNotAuthorized

const RunContextSessionKey = agentrun.RunContextSessionKey

const AgentRequestInboxContextKey = agentrun.AgentRequestInboxContextKey

const AgentRequestCompletionReviewContextKey = agentrun.AgentRequestCompletionReviewContextKey

const RunManagementSkillVersion = agentrun.RunManagementSkillVersion

const RunManagementEndpoint = agentrun.RunManagementEndpoint

// RunManagementSkill forwards to agentrun.RunManagementSkill.
func RunManagementSkill() *skill.Definition {
	return agentrun.RunManagementSkill()
}

type RunActionValidator = agentrun.RunActionValidator

// NewRunActionValidator forwards to agentrun.NewRunActionValidator.
func NewRunActionValidator(store models.RunCommandStore) (*agentrun.RunActionValidator, error) {
	return agentrun.NewRunActionValidator(store)
}

type RunActionDispatcher = agentrun.RunActionDispatcher

// NewRunActionDispatcher forwards to agentrun.NewRunActionDispatcher.
func NewRunActionDispatcher(store models.RunCommandStore, fallback agentrun.ActionDispatcher) (*agentrun.RunActionDispatcher, error) {
	return agentrun.NewRunActionDispatcher(store, fallback)
}

const DelegationModeContextKey = agentrun.DelegationModeContextKey

const RunbookInvocationContextKey = agentrun.RunbookInvocationContextKey

type RunForkBranch = agentrun.RunForkBranch

type TurnRunbookProposal = agentrun.TurnRunbookProposal

type TurnForkProposal = agentrun.TurnForkProposal

type TurnDelegationProposal = agentrun.TurnDelegationProposal

type CreateRunForkRequest = agentrun.CreateRunForkRequest

type RunForkResult = agentrun.RunForkResult

type RunForkCoordinator = agentrun.RunForkCoordinator

// NewRunForkCoordinator forwards to agentrun.NewRunForkCoordinator.
func NewRunForkCoordinator(store models.RunForkStore, opts ...models.ServiceOption) *agentrun.RunForkCoordinator {
	return agentrun.NewRunForkCoordinator(store, opts...)
}

const TerminalRunInteractionCloser = agentrun.TerminalRunInteractionCloser

type PromptContribution = agentrun.PromptContribution

type SteeringPlan = agentrun.SteeringPlan

type StepContextAppend = agentrun.StepContextAppend

type StepPreparer = agentrun.StepPreparer

type ModelStepPreparer = agentrun.ModelStepPreparer

type LoopToolExecutor = agentrun.LoopToolExecutor

type StepToolsInput = agentrun.StepToolsInput

type StepToolsOutcome = agentrun.StepToolsOutcome

type BudgetExhaustion = agentrun.BudgetExhaustion

type FinalAnswer = agentrun.FinalAnswer

type FinalPark = agentrun.FinalPark

type FinalDelivery = agentrun.FinalDelivery

type FinalDeliveryKind = agentrun.FinalDeliveryKind

const FinalDeliveryComplete = agentrun.FinalDeliveryComplete

const FinalDeliveryFail = agentrun.FinalDeliveryFail

const FinalDeliveryRetryFinal = agentrun.FinalDeliveryRetryFinal

const FinalDeliveryContinue = agentrun.FinalDeliveryContinue

const FinalDeliveryPark = agentrun.FinalDeliveryPark

type FinalDeliveryResult = agentrun.FinalDeliveryResult

type FinalAnswerHandler = agentrun.FinalAnswerHandler

type StepCompactor = agentrun.StepCompactor

type ToolCallCheck = agentrun.ToolCallCheck

type AfterToolEntryHook = agentrun.AfterToolEntryHook

type RunProgressSnapshot = agentrun.RunProgressSnapshot

type RunProgressAcknowledgementRequest = agentrun.RunProgressAcknowledgementRequest

type RunProgressAcknowledgementRenderer = agentrun.RunProgressAcknowledgementRenderer

type RunRetryEligibility = agentrun.RunRetryEligibility

const StepFailureRunEnded = agentrun.StepFailureRunEnded

type RunTerminalFinalizer = agentrun.RunTerminalFinalizer

type RunTerminalFinalizerFunc = agentrun.RunTerminalFinalizerFunc

type ActionRunTerminalFinalizer = agentrun.ActionRunTerminalFinalizer

// NewActionRunTerminalFinalizer forwards to agentrun.NewActionRunTerminalFinalizer.
func NewActionRunTerminalFinalizer(store models.KernelStore, catalog agentrun.ActionExecutionCatalog, dispatcher agentrun.ActionDispatcher, opts ...models.ServiceOption) *agentrun.ActionRunTerminalFinalizer {
	return agentrun.NewActionRunTerminalFinalizer(store, catalog, dispatcher, opts...)
}

const SkillActionRequestConnection = agentrun.SkillActionRequestConnection

const SkillActionRequestCredential = agentrun.SkillActionRequestCredential

const SkillActionRequestSetup = agentrun.SkillActionRequestSetup

type StepProducer = agentrun.StepProducer

type StepMode = agentrun.StepMode

const StepModeNormal = agentrun.StepModeNormal

const StepModeExplainFailure = agentrun.StepModeExplainFailure

type StepInput = agentrun.StepInput

type StepOutput = agentrun.StepOutput

type LoopToolCall = agentrun.LoopToolCall

type ToolCallDirective = agentrun.ToolCallDirective

type LoopFinal = agentrun.LoopFinal

type LoopPark = agentrun.LoopPark

type LoopFailure = agentrun.LoopFailure

var ErrInvalidStepOutput = agentrun.ErrInvalidStepOutput

type WakeSignal = agentrun.WakeSignal

type WokenRun = agentrun.WokenRun

type WakeResult = agentrun.WakeResult

type AgentRunWakeService = agentrun.AgentRunWakeService

// NewAgentRunWakeService forwards to agentrun.NewAgentRunWakeService.
func NewAgentRunWakeService(portfolio models.PortfolioStore, activityStore models.RunActivityStore, opts ...models.ServiceOption) *agentrun.AgentRunWakeService {
	return agentrun.NewAgentRunWakeService(portfolio, activityStore, opts...)
}

const AgentTrialWakeType = agentrun.AgentTrialWakeType
