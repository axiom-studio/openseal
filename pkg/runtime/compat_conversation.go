// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/runtime/conversation. Remove once in-flight branches have landed.
//
// Exported declarations of pkg/runtime/conversation, kept so that importers
// of pkg/runtime continue to compile.

package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

const ConversationApprovalModeChangedEvent = runtimeconversation.ConversationApprovalModeChangedEvent

type SetConversationApprovalModeRequest = runtimeconversation.SetConversationApprovalModeRequest

type SetConversationApprovalModeResult = runtimeconversation.SetConversationApprovalModeResult

type ConversationApprovalModeService = runtimeconversation.ConversationApprovalModeService

// NewConversationApprovalModeService forwards to conversation.NewConversationApprovalModeService.
func NewConversationApprovalModeService(store models.ConversationApprovalModeKernelStore, opts ...models.ServiceOption) *runtimeconversation.ConversationApprovalModeService {
	return runtimeconversation.NewConversationApprovalModeService(store, opts...)
}

type ConversationArchiveSource = runtimeconversation.ConversationArchiveSource

type ConversationArchiveMessage = runtimeconversation.ConversationArchiveMessage

type ImportConversationArchiveRequest = runtimeconversation.ImportConversationArchiveRequest

type ConversationArchiveImportResult = runtimeconversation.ConversationArchiveImportResult

type ConversationChangeRequest = runtimeconversation.ConversationChangeRequest

type ConversationChangeSet = runtimeconversation.ConversationChangeSet

type ConversationChangeService = runtimeconversation.ConversationChangeService

// NewConversationChangeService forwards to conversation.NewConversationChangeService.
func NewConversationChangeService(conversationStore models.ConversationStore, portfolio models.PortfolioStore, opts ...models.ServiceOption) (*runtimeconversation.ConversationChangeService, error) {
	return runtimeconversation.NewConversationChangeService(conversationStore, portfolio, opts...)
}

type ConversationCredentialRequest = runtimeconversation.ConversationCredentialRequest

type SkillSetupRequestReader = runtimeconversation.SkillSetupRequestReader

type ConversationSkillSetupRequest = runtimeconversation.ConversationSkillSetupRequest

type ConversationRunbookStatus = runtimeconversation.ConversationRunbookStatus

const ConversationRunbookActive = runtimeconversation.ConversationRunbookActive

const ConversationRunbookPaused = runtimeconversation.ConversationRunbookPaused

const ConversationRunbookStopped = runtimeconversation.ConversationRunbookStopped

type ConversationRunbook = runtimeconversation.ConversationRunbook

type ConversationRunbookTrigger = runtimeconversation.ConversationRunbookTrigger

type ConversationRunbookScript = runtimeconversation.ConversationRunbookScript

type ConversationRunbookRun = runtimeconversation.ConversationRunbookRun

var ErrConversationCoordinationUnavailable = runtimeconversation.ErrConversationCoordinationUnavailable

var ErrNoConversationParticipants = runtimeconversation.ErrNoConversationParticipants

type ConversationParticipantBinding = runtimeconversation.ConversationParticipantBinding

type ConversationParticipantQuery = runtimeconversation.ConversationParticipantQuery

type ConversationParticipantSource = runtimeconversation.ConversationParticipantSource

type ConversationParticipantSourceFunc = runtimeconversation.ConversationParticipantSourceFunc

type ConversationRoutingRequest = runtimeconversation.ConversationRoutingRequest

type ConversationCoordinatorConfig = runtimeconversation.ConversationCoordinatorConfig

// DefaultConversationCoordinatorConfig forwards to conversation.DefaultConversationCoordinatorConfig.
func DefaultConversationCoordinatorConfig() runtimeconversation.ConversationCoordinatorConfig {
	return runtimeconversation.DefaultConversationCoordinatorConfig()
}

type ConversationCoordinator = runtimeconversation.ConversationCoordinator

// NewConversationCoordinator forwards to conversation.NewConversationCoordinator.
func NewConversationCoordinator(conversations *runtimeconversation.ConversationService, participants runtimeconversation.ConversationParticipantSource, router runtimeconversation.ConversationRouter, config runtimeconversation.ConversationCoordinatorConfig) (*runtimeconversation.ConversationCoordinator, error) {
	return runtimeconversation.NewConversationCoordinator(conversations, participants, router, config)
}

const MaximumExternalConversationContextMessages = runtimeconversation.MaximumExternalConversationContextMessages

const ExternalConversationContextComplete = runtimeconversation.ExternalConversationContextComplete

const ExternalConversationContextPartial = runtimeconversation.ExternalConversationContextPartial

const ExternalConversationContextUnavailable = runtimeconversation.ExternalConversationContextUnavailable

type ExternalConversationContextState = runtimeconversation.ExternalConversationContextState

// ExternalConversationContextAvailability forwards to conversation.ExternalConversationContextAvailability.
func ExternalConversationContextAvailability(message *models.ChannelMessage, endpointID string) *runtimeconversation.ExternalConversationContextState {
	return runtimeconversation.ExternalConversationContextAvailability(message, endpointID)
}

type ConversationHistoryReadStore = runtimeconversation.ConversationHistoryReadStore

type ConversationHistoryReadRequest = runtimeconversation.ConversationHistoryReadRequest

type ConversationHistoryExcerpt = runtimeconversation.ConversationHistoryExcerpt

type ConversationHistoryReadResult = runtimeconversation.ConversationHistoryReadResult

// ReadConversationHistory forwards to conversation.ReadConversationHistory.
func ReadConversationHistory(ctx context.Context, store runtimeconversation.ConversationHistoryReadStore, scope models.Scope, owner models.ObjectiveOwner, conversationID string, viewer models.ConversationViewer, request runtimeconversation.ConversationHistoryReadRequest, trustedThreadRoot ...string) (*runtimeconversation.ConversationHistoryReadResult, error) {
	return runtimeconversation.ReadConversationHistory(ctx, store, scope, owner, conversationID, viewer, request, trustedThreadRoot...)
}

type MessageReactionProjector = runtimeconversation.MessageReactionProjector

type MessageReactionRequest = runtimeconversation.MessageReactionRequest

type MessageReactionResult = runtimeconversation.MessageReactionResult

type TurnReaction = runtimeconversation.TurnReaction

const MaximumTurnReactions = runtimeconversation.MaximumTurnReactions

const ConversationReactInstruction = runtimeconversation.ConversationReactInstruction

type ConversationRouter = runtimeconversation.ConversationRouter

type ConversationRouterFunc = runtimeconversation.ConversationRouterFunc

type ConversationRouteRequest = runtimeconversation.ConversationRouteRequest

type RouteMember = runtimeconversation.RouteMember

type ConversationRouteDecision = runtimeconversation.ConversationRouteDecision

type ConversationPlace = runtimeconversation.ConversationPlace

const MaximumConversationPlaces = runtimeconversation.MaximumConversationPlaces

type CreateConversationRequest = runtimeconversation.CreateConversationRequest

type UpdateConversationRequest = runtimeconversation.UpdateConversationRequest

type PostChannelMessageRequest = runtimeconversation.PostChannelMessageRequest

type CommitConversationRoutingRequest = runtimeconversation.CommitConversationRoutingRequest

type AdvanceConversationCursorRequest = runtimeconversation.AdvanceConversationCursorRequest

type SetConversationPresenceRequest = runtimeconversation.SetConversationPresenceRequest

type ConversationService = runtimeconversation.ConversationService

// NewConversationService forwards to conversation.NewConversationService.
func NewConversationService(store models.ConversationStore, opts ...models.ServiceOption) *runtimeconversation.ConversationService {
	return runtimeconversation.NewConversationService(store, opts...)
}

const ConversationChannelRules = runtimeconversation.ConversationChannelRules

const ConversationAttachmentRules = runtimeconversation.ConversationAttachmentRules

const ConversationExternalSourceRules = runtimeconversation.ConversationExternalSourceRules

// ConversationRunAgentID forwards to conversation.ConversationRunAgentID.
func ConversationRunAgentID(conversation *models.Conversation, run *models.AgentRun) string {
	return runtimeconversation.ConversationRunAgentID(conversation, run)
}

// ResolveTeamHandoffRequest forwards to conversation.ResolveTeamHandoffRequest.
func ResolveTeamHandoffRequest(ctx context.Context, reader models.TeamHandoffMessageReader, run *models.AgentRun, handoff *models.ChannelMessage) (*models.ChannelMessage, error) {
	return runtimeconversation.ResolveTeamHandoffRequest(ctx, reader, run, handoff)
}

var ErrEmbedOriginDenied = runtimeconversation.ErrEmbedOriginDenied

var ErrEmbedAuthentication = runtimeconversation.ErrEmbedAuthentication

type CreateEmbedInstallationRequest = runtimeconversation.CreateEmbedInstallationRequest

type UpdateEmbedInstallationRequest = runtimeconversation.UpdateEmbedInstallationRequest

type OpenEmbedSessionRequest = runtimeconversation.OpenEmbedSessionRequest

type OpenEmbedSessionResult = runtimeconversation.OpenEmbedSessionResult

type EmbedSessionService = runtimeconversation.EmbedSessionService

// NewEmbedSessionService forwards to conversation.NewEmbedSessionService.
func NewEmbedSessionService(store models.EmbedStore, conversations models.ConversationStore, opts ...models.ServiceOption) (*runtimeconversation.EmbedSessionService, error) {
	return runtimeconversation.NewEmbedSessionService(store, conversations, opts...)
}

const RunFailureKindExecution = runtimeconversation.RunFailureKindExecution

const RunFailureKindWorkspace = runtimeconversation.RunFailureKindWorkspace

const RunFailureKindRoutineScript = runtimeconversation.RunFailureKindRoutineScript

const RunFailureKindTaskCompletion = runtimeconversation.RunFailureKindTaskCompletion

const RunFailureKindAgentBehaviorFixed = runtimeconversation.RunFailureKindAgentBehaviorFixed

const RunFailureKindBudget = runtimeconversation.RunFailureKindBudget

const RunFailureKindCredits = runtimeconversation.RunFailureKindCredits

// LoopGovernedToolNames forwards to conversation.LoopGovernedToolNames.
func LoopGovernedToolNames(actions []capability.ModelAction) map[string]string {
	return runtimeconversation.LoopGovernedToolNames(actions)
}

// LoopGovernedAction forwards to conversation.LoopGovernedAction.
func LoopGovernedAction(actions []capability.ModelAction, toolName string) (capability.ModelAction, bool) {
	return runtimeconversation.LoopGovernedAction(actions, toolName)
}

type RecordSurfaceInteractionRequest = runtimeconversation.RecordSurfaceInteractionRequest

// TerminalFailureReply forwards to conversation.TerminalFailureReply.
func TerminalFailureReply(code string) string {
	return runtimeconversation.TerminalFailureReply(code)
}

type TurnRunnerResolver = runtimeconversation.TurnRunnerResolver

type TurnRunnerResolverFunc = runtimeconversation.TurnRunnerResolverFunc
