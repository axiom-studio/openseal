// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/models. Remove once in-flight branches have landed.
//
// Exported sentinel errors and functions of pkg/models, kept so that
// importers of pkg/runtime continue to compile.

package runtime

import (
	"context"
	"hash"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
)

var (
	ErrAcceptedRunExecution                 = models.ErrAcceptedRunExecution
	ErrActionNotFound                       = models.ErrActionNotFound
	ErrApprovalNotFound                     = models.ErrApprovalNotFound
	ErrIdempotencyConflict                  = models.ErrIdempotencyConflict
	ErrApprovalResolved                     = models.ErrApprovalResolved
	ErrActionCredentialLeaseInvalid         = models.ErrActionCredentialLeaseInvalid
	ErrActionCredentialLeaseExpired         = models.ErrActionCredentialLeaseExpired
	ErrActionCredentialLeaseMismatch        = models.ErrActionCredentialLeaseMismatch
	ErrInvalidRunTransition                 = models.ErrInvalidRunTransition
	ErrSteeringArrived                      = models.ErrSteeringArrived
	ErrInvalidAgentSession                  = models.ErrInvalidAgentSession
	ErrAgentSessionNotFound                 = models.ErrAgentSessionNotFound
	ErrAgentSessionConflict                 = models.ErrAgentSessionConflict
	ErrAgentSessionForkTooDeep              = models.ErrAgentSessionForkTooDeep
	ErrInvalidSessionEntry                  = models.ErrInvalidSessionEntry
	ErrSessionEntryTooLarge                 = models.ErrSessionEntryTooLarge
	ErrSessionEntryNotFound                 = models.ErrSessionEntryNotFound
	ErrInvalidToolEntryTransition           = models.ErrInvalidToolEntryTransition
	ErrToolEntryStateConflict               = models.ErrToolEntryStateConflict
	ErrToolEntryNotOwned                    = models.ErrToolEntryNotOwned
	ErrAgentSessionPathTooLarge             = models.ErrAgentSessionPathTooLarge
	ErrSessionMoved                         = models.ErrSessionMoved
	ErrChannelWatermarkMoved                = models.ErrChannelWatermarkMoved
	ErrArtifactImmutable                    = models.ErrArtifactImmutable
	ErrArtifactVersionConflict              = models.ErrArtifactVersionConflict
	ErrInvalidArtifactRecord                = models.ErrInvalidArtifactRecord
	ErrBudgetExhausted                      = models.ErrBudgetExhausted
	ErrCallbackRegistrationNotFound         = models.ErrCallbackRegistrationNotFound
	ErrInvalidCallbackRegistration          = models.ErrInvalidCallbackRegistration
	ErrCallbackRegistrationConflict         = models.ErrCallbackRegistrationConflict
	ErrAgentRequestNotFound                 = models.ErrAgentRequestNotFound
	ErrInvalidAgentRequestState             = models.ErrInvalidAgentRequestState
	ErrAgentRequestAssignment               = models.ErrAgentRequestAssignment
	ErrAgentRequestIdempotency              = models.ErrAgentRequestIdempotency
	ErrUnsafeSharedContext                  = models.ErrUnsafeSharedContext
	ErrInvalidArtifact                      = models.ErrInvalidArtifact
	ErrConversationNotFound                 = models.ErrConversationNotFound
	ErrChannelMessageNotFound               = models.ErrChannelMessageNotFound
	ErrParticipationRoundNotFound           = models.ErrParticipationRoundNotFound
	ErrInvalidConversation                  = models.ErrInvalidConversation
	ErrMessageConflict                      = models.ErrMessageConflict
	ErrConversationCursorConflict           = models.ErrConversationCursorConflict
	ErrConversationPresenceConflict         = models.ErrConversationPresenceConflict
	ErrInvalidConversationTask              = models.ErrInvalidConversationTask
	ErrConversationTaskConflict             = models.ErrConversationTaskConflict
	ErrCredentialRequestConflict            = models.ErrCredentialRequestConflict
	ErrInvalidCredentialRequest             = models.ErrInvalidCredentialRequest
	ErrRunDependencyNotFound                = models.ErrRunDependencyNotFound
	ErrDependencyGroupNotFound              = models.ErrDependencyGroupNotFound
	ErrInvalidRunDependency                 = models.ErrInvalidRunDependency
	ErrDependencyConflict                   = models.ErrDependencyConflict
	ErrEmbedInstallationNotFound            = models.ErrEmbedInstallationNotFound
	ErrEmbedSessionNotFound                 = models.ErrEmbedSessionNotFound
	ErrEmbedRevisionConflict                = models.ErrEmbedRevisionConflict
	ErrEmbedRouteConflict                   = models.ErrEmbedRouteConflict
	ErrEmbedSessionUnauthorized             = models.ErrEmbedSessionUnauthorized
	ErrEmbedLimitExceeded                   = models.ErrEmbedLimitExceeded
	ErrExternalConversationEndpointNotFound = models.ErrExternalConversationEndpointNotFound
	ErrExternalConversationConflict         = models.ErrExternalConversationConflict
	ErrExternalConversationInboxNotFound    = models.ErrExternalConversationInboxNotFound
	ErrExternalConversationDeliveryNotFound = models.ErrExternalConversationDeliveryNotFound
	ErrInvalidExternalConversation          = models.ErrInvalidExternalConversation
	ErrExternalConversationLeaseLost        = models.ErrExternalConversationLeaseLost
	ErrExternalConversationGatewayNotFound  = models.ErrExternalConversationGatewayNotFound
	ErrExternalOperationClaimed             = models.ErrExternalOperationClaimed
	ErrLeaseLost                            = models.ErrLeaseLost
	ErrNestedTurnNotActive                  = models.ErrNestedTurnNotActive
	ErrNestedActionLimit                    = models.ErrNestedActionLimit
	ErrInvalidScope                         = models.ErrInvalidScope
	ErrInvalidOwner                         = models.ErrInvalidOwner
	ErrObjectiveNotFound                    = models.ErrObjectiveNotFound
	ErrObjectiveIdempotency                 = models.ErrObjectiveIdempotency
	ErrRunNotFound                          = models.ErrRunNotFound
	ErrRevisionConflict                     = models.ErrRevisionConflict
	ErrRunIdempotency                       = models.ErrRunIdempotency
	ErrInvalidAgentRun                      = models.ErrInvalidAgentRun
	ErrInvalidRoutineEvent                  = models.ErrInvalidRoutineEvent
	ErrInvalidRoutineEventTrigger           = models.ErrInvalidRoutineEventTrigger
	ErrRoutineEventBatchNotFound            = models.ErrRoutineEventBatchNotFound
	ErrRoutineEventBatchStarted             = models.ErrRoutineEventBatchStarted
	ErrHookNotFound                         = models.ErrHookNotFound
	ErrHookRevision                         = models.ErrHookRevision
	ErrHookConflict                         = models.ErrHookConflict
	ErrRunSkillDependencyUnavailable        = models.ErrRunSkillDependencyUnavailable
	ErrInvalidRunTerminalReport             = models.ErrInvalidRunTerminalReport
	ErrRunTerminalReportNotFound            = models.ErrRunTerminalReportNotFound
	ErrRunbookActivationNotFound            = models.ErrRunbookActivationNotFound
	ErrRunbookActivationRevision            = models.ErrRunbookActivationRevision
	ErrRunbookActivationIdempotency         = models.ErrRunbookActivationIdempotency
	ErrSkillReferenceUpgradeInvalid         = models.ErrSkillReferenceUpgradeInvalid
	ErrSkillReferenceUpgradeConflict        = models.ErrSkillReferenceUpgradeConflict
	ErrSkillReferenceUpgradeBusy            = models.ErrSkillReferenceUpgradeBusy
	ErrSkillRuntimeMaintenance              = models.ErrSkillRuntimeMaintenance
	ErrSkillRuntimeMaintenanceNotFound      = models.ErrSkillRuntimeMaintenanceNotFound
	ErrSkillRuntimeMaintenanceConflict      = models.ErrSkillRuntimeMaintenanceConflict
	ErrSkillSetupConflict                   = models.ErrSkillSetupConflict
	ErrInvalidSkillSetup                    = models.ErrInvalidSkillSetup
	ErrSpaceNotFound                        = models.ErrSpaceNotFound
	ErrInvalidSpace                         = models.ErrInvalidSpace
	ErrDefaultSpaceProtected                = models.ErrDefaultSpaceProtected
	ErrInvalidSurfaceInteraction            = models.ErrInvalidSurfaceInteraction
	ErrSurfaceInteractionConflict           = models.ErrSurfaceInteractionConflict
	ErrTurnNotFound                         = models.ErrTurnNotFound
	ErrActiveTurnExists                     = models.ErrActiveTurnExists
	ErrTurnLeaseHeld                        = models.ErrTurnLeaseHeld
)

// AcceptedRunExecutionForRun forwards to models.AcceptedRunExecutionForRun.
func AcceptedRunExecutionForRun(run *AgentRun) (*AcceptedRunExecution, error) {
	return models.AcceptedRunExecutionForRun(run)
}

// NewAcceptedRunExecution forwards to models.NewAcceptedRunExecution.
func NewAcceptedRunExecution(scope Scope, deployment *kernelagent.AgentDeployment, definition *kernelagent.AgentDefinition, entrypoint string) (*AcceptedRunExecution, error) {
	return models.NewAcceptedRunExecution(scope, deployment, definition, entrypoint)
}

// MessageActingUser forwards to models.MessageActingUser.
func MessageActingUser(conversation *Conversation, message *ChannelMessage) string {
	return models.MessageActingUser(conversation, message)
}

// ComputeActionInvocationDigest forwards to models.ComputeActionInvocationDigest.
func ComputeActionInvocationDigest(call *ActionCall) string {
	return models.ComputeActionInvocationDigest(call)
}

// ComputeActionSemanticDigest forwards to models.ComputeActionSemanticDigest.
func ComputeActionSemanticDigest(call *ActionCall) string {
	return models.ComputeActionSemanticDigest(call)
}

// ConversationAgentSessionID forwards to models.ConversationAgentSessionID.
func ConversationAgentSessionID(scope Scope, laneKey string) string {
	return models.ConversationAgentSessionID(scope, laneKey)
}

// RunAgentSessionID forwards to models.RunAgentSessionID.
func RunAgentSessionID(scope Scope, ownerRunID string) string {
	return models.RunAgentSessionID(scope, ownerRunID)
}

// SessionEntryID forwards to models.SessionEntryID.
func SessionEntryID(sessionID, logicalKey string) string {
	return models.SessionEntryID(sessionID, logicalKey)
}

// AssistantEntryKey forwards to models.AssistantEntryKey.
func AssistantEntryKey(stepID string) string {
	return models.AssistantEntryKey(stepID)
}

// ToolEntryKey forwards to models.ToolEntryKey.
func ToolEntryKey(stepID, toolCallID string) string {
	return models.ToolEntryKey(stepID, toolCallID)
}

// ChannelMessageSourceKey forwards to models.ChannelMessageSourceKey.
func ChannelMessageSourceKey(messageID string) string {
	return models.ChannelMessageSourceKey(messageID)
}

// InterventionSourceKey forwards to models.InterventionSourceKey.
func InterventionSourceKey(interventionID string) string {
	return models.InterventionSourceKey(interventionID)
}

// RunContextSourceKey forwards to models.RunContextSourceKey.
func RunContextSourceKey(runID string) string {
	return models.RunContextSourceKey(runID)
}

// GoalSourceKey forwards to models.GoalSourceKey.
func GoalSourceKey(ownerRunID string) string {
	return models.GoalSourceKey(ownerRunID)
}

// FailureNoteSourceKey forwards to models.FailureNoteSourceKey.
func FailureNoteSourceKey(runID, stepID string) string {
	return models.FailureNoteSourceKey(runID, stepID)
}

// FinalRejectionEntryKey forwards to models.FinalRejectionEntryKey.
func FinalRejectionEntryKey(stepID string) string {
	return models.FinalRejectionEntryKey(stepID)
}

// TaskPromotedSourceKey forwards to models.TaskPromotedSourceKey.
func TaskPromotedSourceKey(runID string) string {
	return models.TaskPromotedSourceKey(runID)
}

// RoutineEventEntrySourceKey forwards to models.RoutineEventEntrySourceKey.
func RoutineEventEntrySourceKey(runID string) string {
	return models.RoutineEventEntrySourceKey(runID)
}

// ValidToolEntryTransition forwards to models.ValidToolEntryTransition.
func ValidToolEntryTransition(from, to ToolEntryState) bool {
	return models.ValidToolEntryTransition(from, to)
}

// EstimateSessionEntryTokens forwards to models.EstimateSessionEntryTokens.
func EstimateSessionEntryTokens(e *SessionEntry) int64 {
	return models.EstimateSessionEntryTokens(e)
}

// EvaluateBudget forwards to models.EvaluateBudget.
func EvaluateBudget(policy BudgetPolicy, usage BudgetUsage) (BudgetState, []string, error) {
	return models.EvaluateBudget(policy, usage)
}

// EffectiveBudgetUsage forwards to models.EffectiveBudgetUsage.
func EffectiveBudgetUsage(committed BudgetUsage, reservations map[string]BudgetReservation) (BudgetUsage, error) {
	return models.EffectiveBudgetUsage(committed, reservations)
}

// BudgetWouldExceed forwards to models.BudgetWouldExceed.
func BudgetWouldExceed(policy BudgetPolicy, usage BudgetUsage) (bool, []string, error) {
	return models.BudgetWouldExceed(policy, usage)
}

// ValidateCredentialFreeContext forwards to models.ValidateCredentialFreeContext.
func ValidateCredentialFreeContext(value interface{}) error {
	return models.ValidateCredentialFreeContext(value)
}

// NormalizeAppAddress forwards to models.NormalizeAppAddress.
func NormalizeAppAddress(value string) (string, error) {
	return models.NormalizeAppAddress(value)
}

// AgentApprovalsConversationReferenceID forwards to models.AgentApprovalsConversationReferenceID.
func AgentApprovalsConversationReferenceID(owner ObjectiveOwner) string {
	return models.AgentApprovalsConversationReferenceID(owner)
}

// CanViewChannelMessage forwards to models.CanViewChannelMessage.
func CanViewChannelMessage(message *ChannelMessage, viewer ConversationViewer) bool {
	return models.CanViewChannelMessage(message, viewer)
}

// ValidateReactionEmoji forwards to models.ValidateReactionEmoji.
func ValidateReactionEmoji(emoji string) error {
	return models.ValidateReactionEmoji(emoji)
}

// ConversationTaskMatchesWorkRun forwards to models.ConversationTaskMatchesWorkRun.
func ConversationTaskMatchesWorkRun(task *ConversationTask, run *AgentRun) bool {
	return models.ConversationTaskMatchesWorkRun(task, run)
}

// ConversationTaskAcknowledgmentMessageID forwards to models.ConversationTaskAcknowledgmentMessageID.
func ConversationTaskAcknowledgmentMessageID(task *ConversationTask) string {
	return models.ConversationTaskAcknowledgmentMessageID(task)
}

// ConversationTaskStartedToolResult forwards to models.ConversationTaskStartedToolResult.
func ConversationTaskStartedToolResult(task *ConversationTask) *ToolResult {
	return models.ConversationTaskStartedToolResult(task)
}

// ConversationRunLane forwards to models.ConversationRunLane.
func ConversationRunLane(conversationID, threadRoot, assignedAgentID string) string {
	return models.ConversationRunLane(conversationID, threadRoot, assignedAgentID)
}

// NormalizeCredentialRequestWebsite forwards to models.NormalizeCredentialRequestWebsite.
func NormalizeCredentialRequestWebsite(value string) (string, error) {
	return models.NormalizeCredentialRequestWebsite(value)
}

// EvaluateRunDependencies forwards to models.EvaluateRunDependencies.
func EvaluateRunDependencies(group *RunDependencyGroup, edges []*RunDependency) (RunDependencyEvaluation, error) {
	return models.EvaluateRunDependencies(group, edges)
}

// ValidRoutineEventID forwards to models.ValidRoutineEventID.
func ValidRoutineEventID(id string) bool {
	return models.ValidRoutineEventID(id)
}

// RoutineConversationSourceRef forwards to models.RoutineConversationSourceRef.
func RoutineConversationSourceRef(deploymentID, bindingID, adapterID, installationID string) string {
	return models.RoutineConversationSourceRef(deploymentID, bindingID, adapterID, installationID)
}

// RoutineCallbackSourceRef forwards to models.RoutineCallbackSourceRef.
func RoutineCallbackSourceRef(registrationID string) string {
	return models.RoutineCallbackSourceRef(registrationID)
}

// RoutineGoogleSourceRef forwards to models.RoutineGoogleSourceRef.
func RoutineGoogleSourceRef(userID, googleSubject string) string {
	return models.RoutineGoogleSourceRef(userID, googleSubject)
}

// HookSourceRef forwards to models.HookSourceRef.
func HookSourceRef(hookID string) string {
	return models.HookSourceRef(hookID)
}

// HookID forwards to models.HookID.
func HookID(scope Scope, activationID string) string {
	return models.HookID(scope, activationID)
}

// ValidHookID forwards to models.ValidHookID.
func ValidHookID(id string) bool {
	return models.ValidHookID(id)
}

// DescribeRoutineEventTrigger forwards to models.DescribeRoutineEventTrigger.
func DescribeRoutineEventTrigger(t *RoutineEventTrigger) string {
	return models.DescribeRoutineEventTrigger(t)
}

// DescribeRoutineEventTriggerAudience forwards to models.DescribeRoutineEventTriggerAudience.
func DescribeRoutineEventTriggerAudience(t *RoutineEventTrigger) string {
	return models.DescribeRoutineEventTriggerAudience(t)
}

// DescribeRoutineEventTriggerLimit forwards to models.DescribeRoutineEventTriggerLimit.
func DescribeRoutineEventTriggerLimit(t *RoutineEventTrigger) string {
	return models.DescribeRoutineEventTriggerLimit(t)
}

// RoutineEventFilterFields forwards to models.RoutineEventFilterFields.
func RoutineEventFilterFields(kind RoutineEventKind) []string {
	return models.RoutineEventFilterFields(kind)
}

// RunSkillActor forwards to models.RunSkillActor.
func RunSkillActor(run *AgentRun) skill.RunActor {
	return models.RunSkillActor(run)
}

// SkillUmbrella forwards to models.SkillUmbrella.
func SkillUmbrella(owner ObjectiveOwner) skill.Umbrella {
	return models.SkillUmbrella(owner)
}

// WithSkillRuntimeMaintenance forwards to models.WithSkillRuntimeMaintenance.
func WithSkillRuntimeMaintenance(ctx context.Context, gate *SkillRuntimeMaintenance) context.Context {
	return models.WithSkillRuntimeMaintenance(ctx, gate)
}

// SkillRuntimeMaintenanceFromContext forwards to models.SkillRuntimeMaintenanceFromContext.
func SkillRuntimeMaintenanceFromContext(ctx context.Context) (*SkillRuntimeMaintenance, bool) {
	return models.SkillRuntimeMaintenanceFromContext(ctx)
}

// ValidateSkillRuntimeMaintenanceContext forwards to models.ValidateSkillRuntimeMaintenanceContext.
func ValidateSkillRuntimeMaintenanceContext(ctx context.Context, gate *SkillRuntimeMaintenance, now time.Time) error {
	return models.ValidateSkillRuntimeMaintenanceContext(ctx, gate, now)
}

// AddSkillRuntimeMaintenanceBindingDigest forwards to models.AddSkillRuntimeMaintenanceBindingDigest.
func AddSkillRuntimeMaintenanceBindingDigest(h hash.Hash, b SkillRuntimeMaintenanceBindingRevision) error {
	return models.AddSkillRuntimeMaintenanceBindingDigest(h, b)
}
