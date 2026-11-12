// Re-exports of declarations moved to github.com/axiom-studio/openseal/pkg/runtime/conversation,
// kept so that importers of github.com/axiom-studio/openseal/pkg/runtime continue to compile.

package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

const ConversationApprovalModeChangedEvent = conversation.ConversationApprovalModeChangedEvent

type SetConversationApprovalModeRequest = conversation.SetConversationApprovalModeRequest

type SetConversationApprovalModeResult = conversation.SetConversationApprovalModeResult

type ConversationApprovalModeService = conversation.ConversationApprovalModeService

// NewConversationApprovalModeService forwards to conversation.NewConversationApprovalModeService.
func NewConversationApprovalModeService(store models.ConversationApprovalModeKernelStore, opts ...models.ServiceOption) *conversation.ConversationApprovalModeService {
	return conversation.NewConversationApprovalModeService(store, opts...)
}

type ConversationArchiveSource = conversation.ConversationArchiveSource

type ConversationArchiveMessage = conversation.ConversationArchiveMessage

type ImportConversationArchiveRequest = conversation.ImportConversationArchiveRequest

type ConversationArchiveImportResult = conversation.ConversationArchiveImportResult

type ConversationChangeRequest = conversation.ConversationChangeRequest

type ConversationChangeSet = conversation.ConversationChangeSet

type ConversationChangeService = conversation.ConversationChangeService

// NewConversationChangeService forwards to conversation.NewConversationChangeService.
func NewConversationChangeService(conversationStore models.ConversationStore, portfolio models.PortfolioStore, opts ...models.ServiceOption) (*conversation.ConversationChangeService, error) {
	return conversation.NewConversationChangeService(conversationStore, portfolio, opts...)
}

type ConversationCredentialRequest = conversation.ConversationCredentialRequest

type SkillSetupRequestReader = conversation.SkillSetupRequestReader

type ConversationSkillSetupRequest = conversation.ConversationSkillSetupRequest

type ConversationRunbookStatus = conversation.ConversationRunbookStatus

const ConversationRunbookActive = conversation.ConversationRunbookActive

const ConversationRunbookPaused = conversation.ConversationRunbookPaused

const ConversationRunbookStopped = conversation.ConversationRunbookStopped

type ConversationRunbook = conversation.ConversationRunbook

type ConversationRunbookTrigger = conversation.ConversationRunbookTrigger

type ConversationRunbookScript = conversation.ConversationRunbookScript

type ConversationRunbookRun = conversation.ConversationRunbookRun

var ErrConversationCoordinationUnavailable = conversation.ErrConversationCoordinationUnavailable

var ErrNoConversationParticipants = conversation.ErrNoConversationParticipants

type ConversationParticipantBinding = conversation.ConversationParticipantBinding

type ConversationParticipantQuery = conversation.ConversationParticipantQuery

type ConversationParticipantSource = conversation.ConversationParticipantSource

type ConversationParticipantSourceFunc = conversation.ConversationParticipantSourceFunc

type ConversationRoutingRequest = conversation.ConversationRoutingRequest

type ConversationCoordinatorConfig = conversation.ConversationCoordinatorConfig

// DefaultConversationCoordinatorConfig forwards to conversation.DefaultConversationCoordinatorConfig.
func DefaultConversationCoordinatorConfig() conversation.ConversationCoordinatorConfig {
	return conversation.DefaultConversationCoordinatorConfig()
}

type ConversationCoordinator = conversation.ConversationCoordinator

// NewConversationCoordinator forwards to conversation.NewConversationCoordinator.
func NewConversationCoordinator(conversations *conversation.ConversationService, participants conversation.ConversationParticipantSource, router conversation.ConversationRouter, config conversation.ConversationCoordinatorConfig) (*conversation.ConversationCoordinator, error) {
	return conversation.NewConversationCoordinator(conversations, participants, router, config)
}

const MaximumExternalConversationContextMessages = conversation.MaximumExternalConversationContextMessages

const ExternalConversationContextComplete = conversation.ExternalConversationContextComplete

const ExternalConversationContextPartial = conversation.ExternalConversationContextPartial

const ExternalConversationContextUnavailable = conversation.ExternalConversationContextUnavailable

type ExternalConversationContextState = conversation.ExternalConversationContextState

// ExternalConversationContextAvailability forwards to conversation.ExternalConversationContextAvailability.
func ExternalConversationContextAvailability(message *models.ChannelMessage, endpointID string) *conversation.ExternalConversationContextState {
	return conversation.ExternalConversationContextAvailability(message, endpointID)
}

type ConversationHistoryReadStore = conversation.ConversationHistoryReadStore

type ConversationHistoryReadRequest = conversation.ConversationHistoryReadRequest

type ConversationHistoryExcerpt = conversation.ConversationHistoryExcerpt

type ConversationHistoryReadResult = conversation.ConversationHistoryReadResult

// ReadConversationHistory forwards to conversation.ReadConversationHistory.
func ReadConversationHistory(ctx context.Context, store conversation.ConversationHistoryReadStore, scope models.Scope, owner models.ObjectiveOwner, conversationID string, viewer models.ConversationViewer, request conversation.ConversationHistoryReadRequest, trustedThreadRoot ...string) (*conversation.ConversationHistoryReadResult, error) {
	return conversation.ReadConversationHistory(ctx, store, scope, owner, conversationID, viewer, request, trustedThreadRoot...)
}

type MessageReactionProjector = conversation.MessageReactionProjector

type MessageReactionRequest = conversation.MessageReactionRequest

type MessageReactionResult = conversation.MessageReactionResult

type TurnReaction = conversation.TurnReaction

const MaximumTurnReactions = conversation.MaximumTurnReactions

const ConversationReactInstruction = conversation.ConversationReactInstruction

type ConversationRouter = conversation.ConversationRouter

type ConversationRouterFunc = conversation.ConversationRouterFunc

type ConversationRouteRequest = conversation.ConversationRouteRequest

type RouteMember = conversation.RouteMember

type ConversationRouteDecision = conversation.ConversationRouteDecision

type ConversationPlace = conversation.ConversationPlace

const MaximumConversationPlaces = conversation.MaximumConversationPlaces

type CreateConversationRequest = conversation.CreateConversationRequest

type UpdateConversationRequest = conversation.UpdateConversationRequest

type PostChannelMessageRequest = conversation.PostChannelMessageRequest

type CommitConversationRoutingRequest = conversation.CommitConversationRoutingRequest

type AdvanceConversationCursorRequest = conversation.AdvanceConversationCursorRequest

type SetConversationPresenceRequest = conversation.SetConversationPresenceRequest

type ConversationService = conversation.ConversationService

// NewConversationService forwards to conversation.NewConversationService.
func NewConversationService(store models.ConversationStore, opts ...models.ServiceOption) *conversation.ConversationService {
	return conversation.NewConversationService(store, opts...)
}

const ConversationChannelRules = conversation.ConversationChannelRules

const ConversationAttachmentRules = conversation.ConversationAttachmentRules

const ConversationExternalSourceRules = conversation.ConversationExternalSourceRules

// ConversationRunAgentID forwards to conversation.ConversationRunAgentID.
func ConversationRunAgentID(arg0 *models.Conversation, run *models.AgentRun) string {
	return conversation.ConversationRunAgentID(arg0, run)
}

// ResolveTeamHandoffRequest forwards to conversation.ResolveTeamHandoffRequest.
func ResolveTeamHandoffRequest(ctx context.Context, reader models.TeamHandoffMessageReader, run *models.AgentRun, handoff *models.ChannelMessage) (*models.ChannelMessage, error) {
	return conversation.ResolveTeamHandoffRequest(ctx, reader, run, handoff)
}

var ErrEmbedOriginDenied = conversation.ErrEmbedOriginDenied

var ErrEmbedAuthentication = conversation.ErrEmbedAuthentication

type CreateEmbedInstallationRequest = conversation.CreateEmbedInstallationRequest

type UpdateEmbedInstallationRequest = conversation.UpdateEmbedInstallationRequest

type OpenEmbedSessionRequest = conversation.OpenEmbedSessionRequest

type OpenEmbedSessionResult = conversation.OpenEmbedSessionResult

type EmbedSessionService = conversation.EmbedSessionService

// NewEmbedSessionService forwards to conversation.NewEmbedSessionService.
func NewEmbedSessionService(store models.EmbedStore, conversations models.ConversationStore, opts ...models.ServiceOption) (*conversation.EmbedSessionService, error) {
	return conversation.NewEmbedSessionService(store, conversations, opts...)
}

const RunFailureKindExecution = conversation.RunFailureKindExecution

const RunFailureKindWorkspace = conversation.RunFailureKindWorkspace

const RunFailureKindRoutineScript = conversation.RunFailureKindRoutineScript

const RunFailureKindTaskCompletion = conversation.RunFailureKindTaskCompletion

const RunFailureKindAgentBehaviorFixed = conversation.RunFailureKindAgentBehaviorFixed

const RunFailureKindBudget = conversation.RunFailureKindBudget

const RunFailureKindCredits = conversation.RunFailureKindCredits

// LoopGovernedToolNames forwards to conversation.LoopGovernedToolNames.
func LoopGovernedToolNames(actions []capability.ModelAction) map[string]string {
	return conversation.LoopGovernedToolNames(actions)
}

// LoopGovernedAction forwards to conversation.LoopGovernedAction.
func LoopGovernedAction(actions []capability.ModelAction, toolName string) (capability.ModelAction, bool) {
	return conversation.LoopGovernedAction(actions, toolName)
}

type RecordSurfaceInteractionRequest = conversation.RecordSurfaceInteractionRequest

// TerminalFailureReply forwards to conversation.TerminalFailureReply.
func TerminalFailureReply(code string) string {
	return conversation.TerminalFailureReply(code)
}

type TurnRunnerResolver = conversation.TurnRunnerResolver
