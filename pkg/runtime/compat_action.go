// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/runtime/action. Remove once in-flight branches have landed.
//
// Exported declarations of pkg/runtime/action, kept so that importers of
// pkg/runtime continue to compile.

package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue"
	"github.com/axiom-studio/openseal/pkg/skill"
	"go.uber.org/zap"
)

// ApprovalModeAllowsAction forwards to action.ApprovalModeAllowsAction.
func ApprovalModeAllowsAction(mode models.ConversationApprovalMode, review skill.ActionReview, risk skill.RiskLevel, sideEffect skill.SideEffect) bool {
	return action.ApprovalModeAllowsAction(mode, review, risk, sideEffect)
}

// RunConversationID forwards to action.RunConversationID.
func RunConversationID(ctx context.Context, runs models.AgentRunReader, run *models.AgentRun) (string, error) {
	return action.RunConversationID(ctx, runs, run)
}

// RunConversationApprovalMode forwards to action.RunConversationApprovalMode.
func RunConversationApprovalMode(ctx context.Context, runs models.AgentRunReader, conversations action.ConversationReader, run *models.AgentRun) (string, models.ConversationApprovalMode, error) {
	return action.RunConversationApprovalMode(ctx, runs, conversations, run)
}

type ActionDisposition = action.ActionDisposition

const ActionDispositionAllow = action.ActionDispositionAllow

const ActionDispositionDeny = action.ActionDispositionDeny

const ActionDispositionRequireApproval = action.ActionDispositionRequireApproval

type ActionPolicyInput = action.ActionPolicyInput

type ActionPolicyDecision = action.ActionPolicyDecision

type ActionPolicyEvaluator = action.ActionPolicyEvaluator

type ActionPolicyEvaluatorFunc = action.ActionPolicyEvaluatorFunc

type ActionCatalog = action.ActionCatalog

type ActionProposalValidator = action.ActionProposalValidator

type ActionProposalArgumentResolver = action.ActionProposalArgumentResolver

type ProposeActionRequest = action.ProposeActionRequest

type ActionCoordinator = action.ActionCoordinator

// NewActionCoordinator forwards to action.NewActionCoordinator.
func NewActionCoordinator(portfolio models.PortfolioStore, actions models.ActionStore, catalog action.ActionCatalog, policy action.ActionPolicyEvaluator, validators []action.ActionProposalValidator, opts ...models.ServiceOption) *action.ActionCoordinator {
	return action.NewActionCoordinator(portfolio, actions, catalog, policy, validators, opts...)
}

type ActionCredentialLeaseSigner = action.ActionCredentialLeaseSigner

type ActionCredentialLeaseSignatureVerifier = action.ActionCredentialLeaseSignatureVerifier

// SignActionCredentialLease forwards to action.SignActionCredentialLease.
func SignActionCredentialLease(ctx context.Context, lease models.ActionCredentialLease, signer action.ActionCredentialLeaseSigner) (*models.SignedActionCredentialLease, error) {
	return action.SignActionCredentialLease(ctx, lease, signer)
}

type ActionCredentialLeaseValidationRequest = action.ActionCredentialLeaseValidationRequest

type ActionCredentialLeaseValidator = action.ActionCredentialLeaseValidator

// NewActionCredentialLeaseValidator forwards to action.NewActionCredentialLeaseValidator.
func NewActionCredentialLeaseValidator(verifier action.ActionCredentialLeaseSignatureVerifier, redeemer models.ActionCredentialLeaseRedeemer) (*action.ActionCredentialLeaseValidator, error) {
	return action.NewActionCredentialLeaseValidator(verifier, redeemer)
}

// MatchActionCredentialLeaseReferences forwards to action.MatchActionCredentialLeaseReferences.
func MatchActionCredentialLeaseReferences(envelope *models.SignedActionCredentialLease, call *models.ActionCall, run *models.AgentRun, transport string) error {
	return action.MatchActionCredentialLeaseReferences(envelope, call, run, transport)
}

type RequiredActionEvidenceValidator = action.RequiredActionEvidenceValidator

const RefusedLoopCallRepeat = action.RefusedLoopCallRepeat

const RefusedLoopCallDuplicateSuppressed = action.RefusedLoopCallDuplicateSuppressed

const RefusedLoopCallRepeatLimit = action.RefusedLoopCallRepeatLimit

const ActivityEventActionRefused = action.ActivityEventActionRefused

type RefusedLoopCallRequest = action.RefusedLoopCallRequest

const RunFailureCodeRepeatLimit = action.RunFailureCodeRepeatLimit

type DefaultActionPolicy = action.DefaultActionPolicy

// NewDefaultActionPolicy forwards to action.NewDefaultActionPolicy.
func NewDefaultActionPolicy() *action.DefaultActionPolicy {
	return action.NewDefaultActionPolicy()
}

type EligibleApprovalAuthorizer = action.EligibleApprovalAuthorizer

const ProposalRejectionToolArgumentsInvalid = action.ProposalRejectionToolArgumentsInvalid

const ProposalRejectionReviewInvalid = action.ProposalRejectionReviewInvalid

const ProposalRejectionExternalIdentityInvalid = action.ProposalRejectionExternalIdentityInvalid

const ProposalRejectionReviewSummaryRequired = action.ProposalRejectionReviewSummaryRequired

const ProposalRejectionNotAdmitted = action.ProposalRejectionNotAdmitted

const ProposalRejectionCapabilityUnavailable = action.ProposalRejectionCapabilityUnavailable

const ProposalRejectionUnknownTool = action.ProposalRejectionUnknownTool

type ActionProposalRejection = action.ActionProposalRejection

type CredentialResolver = action.CredentialResolver

type CredentialResolutionRequest = action.CredentialResolutionRequest

type CredentialAvailabilityRequest = action.CredentialAvailabilityRequest

type CredentialGap = action.CredentialGap

const CredentialGapPersonSignIn = action.CredentialGapPersonSignIn

const CredentialGapSharedSignIn = action.CredentialGapSharedSignIn

const CredentialGapNoPerson = action.CredentialGapNoPerson

type CredentialAvailability = action.CredentialAvailability

type CredentialAvailabilityChecker = action.CredentialAvailabilityChecker

type CredentialResolverFunc = action.CredentialResolverFunc

type ActionCredentialLeaseIssuer = action.ActionCredentialLeaseIssuer

type ActionCredentialLeaseIssuerFunc = action.ActionCredentialLeaseIssuerFunc

type ActionCredentialLeaseIssueRequest = action.ActionCredentialLeaseIssueRequest

type ActionDispatcherFunc = action.ActionDispatcherFunc

type ActionWorker = action.ActionWorker

// NewActionWorker forwards to action.NewActionWorker.
func NewActionWorker(store models.KernelStore, catalog agentrun.ActionExecutionCatalog, credentials action.CredentialResolver, dispatcher agentrun.ActionDispatcher, opts ...models.ServiceOption) *action.ActionWorker {
	return action.NewActionWorker(store, catalog, credentials, dispatcher, opts...)
}

// NewActionWorkerWithCredentialLeaseIssuer forwards to action.NewActionWorkerWithCredentialLeaseIssuer.
func NewActionWorkerWithCredentialLeaseIssuer(store models.KernelStore, catalog agentrun.ActionExecutionCatalog, issuer action.ActionCredentialLeaseIssuer, dispatcher agentrun.ActionDispatcher, opts ...models.ServiceOption) *action.ActionWorker {
	return action.NewActionWorkerWithCredentialLeaseIssuer(store, catalog, issuer, dispatcher, opts...)
}

var ErrLoopActionNotClaimable = action.ErrLoopActionNotClaimable

type LoopActionMaintenanceError = action.LoopActionMaintenanceError

type ActionWorkerConfig = action.ActionWorkerConfig

type ActionWorkerPool = action.ActionWorkerPool

// NewActionWorkerPool forwards to action.NewActionWorkerPool.
func NewActionWorkerPool(store models.KernelStore, catalog agentrun.ActionExecutionCatalog, credentials action.CredentialResolver, dispatcher agentrun.ActionDispatcher, logger *zap.SugaredLogger, config action.ActionWorkerConfig, opts ...models.ServiceOption) (*action.ActionWorkerPool, error) {
	return action.NewActionWorkerPool(store, catalog, credentials, dispatcher, logger, config, opts...)
}

// NewActionCredentialLeaseWorkerPool forwards to action.NewActionCredentialLeaseWorkerPool.
func NewActionCredentialLeaseWorkerPool(store models.KernelStore, catalog agentrun.ActionExecutionCatalog, issuer action.ActionCredentialLeaseIssuer, dispatcher agentrun.ActionDispatcher, logger *zap.SugaredLogger, config action.ActionWorkerConfig, opts ...models.ServiceOption) (*action.ActionWorkerPool, error) {
	return action.NewActionCredentialLeaseWorkerPool(store, catalog, issuer, dispatcher, logger, config, opts...)
}

type DynamicActionWorkerConfig = action.DynamicActionWorkerConfig

type ActionWorkerSupervisor = action.ActionWorkerSupervisor

// NewActionWorkerSupervisor forwards to action.NewActionWorkerSupervisor.
func NewActionWorkerSupervisor(store models.KernelStore, catalog agentrun.ActionExecutionCatalog, credentials action.CredentialResolver, dispatcher agentrun.ActionDispatcher, source workqueue.WorkerScopeSource, logger *zap.SugaredLogger, config action.DynamicActionWorkerConfig) (*action.ActionWorkerSupervisor, error) {
	return action.NewActionWorkerSupervisor(store, catalog, credentials, dispatcher, source, logger, config)
}

// NewActionCredentialLeaseWorkerSupervisor forwards to action.NewActionCredentialLeaseWorkerSupervisor.
func NewActionCredentialLeaseWorkerSupervisor(store models.KernelStore, catalog agentrun.ActionExecutionCatalog, issuer action.ActionCredentialLeaseIssuer, dispatcher agentrun.ActionDispatcher, source workqueue.WorkerScopeSource, logger *zap.SugaredLogger, config action.DynamicActionWorkerConfig) (*action.ActionWorkerSupervisor, error) {
	return action.NewActionCredentialLeaseWorkerSupervisor(store, catalog, issuer, dispatcher, source, logger, config)
}

type ApprovalAuthorizer = action.ApprovalAuthorizer

type AgentBuilderApprovalAuthorizer = action.AgentBuilderApprovalAuthorizer

var ErrAgentBuilderApprovalForbidden = action.ErrAgentBuilderApprovalForbidden

type ApprovalAuthorizerFunc = action.ApprovalAuthorizerFunc

type ResolveApprovalRequest = action.ResolveApprovalRequest

type ApprovalCoordinator = action.ApprovalCoordinator

// NewApprovalCoordinator forwards to action.NewApprovalCoordinator.
func NewApprovalCoordinator(portfolio models.PortfolioStore, actions models.ActionStore, authorizer action.ApprovalAuthorizer, opts ...models.ServiceOption) *action.ApprovalCoordinator {
	return action.NewApprovalCoordinator(portfolio, actions, authorizer, opts...)
}

var ApprovalSupersededPrincipal = action.ApprovalSupersededPrincipal

const ApprovalSupersededReason = action.ApprovalSupersededReason

var ApprovalModePrincipal = action.ApprovalModePrincipal

var ErrApprovalExpired = action.ErrApprovalExpired

type ApprovalPresentation = action.ApprovalPresentation

// PresentApproval forwards to action.PresentApproval.
func PresentApproval(approval *models.ApprovalCheckpoint) action.ApprovalPresentation {
	return action.PresentApproval(approval)
}

var ErrInvalidApprovalReviewContext = action.ErrInvalidApprovalReviewContext

type KernelResolvedActionArgumentResolver = action.KernelResolvedActionArgumentResolver

const RunbookActionCreateTask = action.RunbookActionCreateTask

const RunbookActionList = action.RunbookActionList

const RunbookActionSetStatus = action.RunbookActionSetStatus

const RunbookActionSetScript = action.RunbookActionSetScript

const TeamManagementSkillVersion = action.TeamManagementSkillVersion

const TeamActionUpdateMember = action.TeamActionUpdateMember

const TeamActionUpdateProfile = action.TeamActionUpdateProfile

const TeamActionCreateTeam = action.TeamActionCreateTeam

const TeamManagementEndpoint = action.TeamManagementEndpoint

const MaxNestedActionCallsPerTurn = action.MaxNestedActionCallsPerTurn

const MaxConcurrentNestedActionCalls = action.MaxConcurrentNestedActionCalls

var ErrNestedActionNotAuthorized = action.ErrNestedActionNotAuthorized

type NestedActionRequest = action.NestedActionRequest

type NestedActionResult = action.NestedActionResult

const NestedHostActionLease = action.NestedHostActionLease

var ErrNestedHostActionNotReady = action.ErrNestedHostActionNotReady

var ErrNestedHostActionClosed = action.ErrNestedHostActionClosed

type NestedHostActionRequest = action.NestedHostActionRequest

type NestedHostActionGrant = action.NestedHostActionGrant

type NestedHostActionOutcome = action.NestedHostActionOutcome

const ObjectiveManagementSkillVersion = action.ObjectiveManagementSkillVersion

const ObjectiveActionCreate = action.ObjectiveActionCreate

const ObjectiveActionUpdate = action.ObjectiveActionUpdate

const ObjectiveActionPause = action.ObjectiveActionPause

const ObjectiveManagementEndpoint = action.ObjectiveManagementEndpoint

// ObjectiveManagementSkill forwards to action.ObjectiveManagementSkill.
func ObjectiveManagementSkill() *skill.Definition {
	return action.ObjectiveManagementSkill()
}

type ObjectiveActionValidator = action.ObjectiveActionValidator

// NewObjectiveActionValidator forwards to action.NewObjectiveActionValidator.
func NewObjectiveActionValidator(store models.PortfolioStore) (*action.ObjectiveActionValidator, error) {
	return action.NewObjectiveActionValidator(store)
}

type ObjectiveActionDispatcher = action.ObjectiveActionDispatcher

// NewObjectiveActionDispatcher forwards to action.NewObjectiveActionDispatcher.
func NewObjectiveActionDispatcher(store models.KernelStore, fallback agentrun.ActionDispatcher) (*action.ObjectiveActionDispatcher, error) {
	return action.NewObjectiveActionDispatcher(store, fallback)
}

type ObservationRefActionProposalValidator = action.ObservationRefActionProposalValidator
