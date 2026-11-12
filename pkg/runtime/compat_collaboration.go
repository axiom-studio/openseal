// Re-exports of declarations moved to github.com/axiom-studio/openseal/pkg/runtime/collaboration,
// kept so that importers of github.com/axiom-studio/openseal/pkg/runtime continue to compile.

package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

const AgentRequestDecisionOutputKey = collaboration.AgentRequestDecisionOutputKey

const AgentRequestCompletionReviewOutputKey = collaboration.AgentRequestCompletionReviewOutputKey

const AgentRequestDecisionSystemInstruction = collaboration.AgentRequestDecisionSystemInstruction

const AgentRequestCompletionReviewSystemInstruction = collaboration.AgentRequestCompletionReviewSystemInstruction

const AcceptedAgentRequestExecutionSystemInstruction = collaboration.AcceptedAgentRequestExecutionSystemInstruction

type AgentRequestInboxReconcileResult = collaboration.AgentRequestInboxReconcileResult

type AgentRequestInboxReconciler = collaboration.AgentRequestInboxReconciler

// NewAgentRequestInboxReconciler forwards to collaboration.NewAgentRequestInboxReconciler.
func NewAgentRequestInboxReconciler(store models.AgentRequestInboxStore, opts ...models.ServiceOption) (*collaboration.AgentRequestInboxReconciler, error) {
	return collaboration.NewAgentRequestInboxReconciler(store, opts...)
}

// ParseAgentRequestDecisionOutput forwards to collaboration.ParseAgentRequestDecisionOutput.
func ParseAgentRequestDecisionOutput(output map[string]interface{}) (collaboration.AgentRequestDecision, string, error) {
	return collaboration.ParseAgentRequestDecisionOutput(output)
}

// ParseAgentRequestCompletionReviewOutput forwards to collaboration.ParseAgentRequestCompletionReviewOutput.
func ParseAgentRequestCompletionReviewOutput(output map[string]interface{}) (bool, string, error) {
	return collaboration.ParseAgentRequestCompletionReviewOutput(output)
}

var ErrAgentRequestUnauthorized = collaboration.ErrAgentRequestUnauthorized

type AgentRequestDecision = collaboration.AgentRequestDecision

const AgentRequestDecisionAccept = collaboration.AgentRequestDecisionAccept

const AgentRequestDecisionReject = collaboration.AgentRequestDecisionReject

const AgentRequestDecisionRequestClarification = collaboration.AgentRequestDecisionRequestClarification

const AgentRequestDecisionProvideClarification = collaboration.AgentRequestDecisionProvideClarification

type CreateAgentRequestRequest = collaboration.CreateAgentRequestRequest

type AgentRequestGroupSpec = collaboration.AgentRequestGroupSpec

type CreateAgentRequestGroupRequest = collaboration.CreateAgentRequestGroupRequest

type AgentRequestGroupResult = collaboration.AgentRequestGroupResult

type RespondAgentRequestRequest = collaboration.RespondAgentRequestRequest

type SubmitAgentRequestBidRequest = collaboration.SubmitAgentRequestBidRequest

type CompleteAgentRequestRequest = collaboration.CompleteAgentRequestRequest

type ReviewAgentRequestCompletionRequest = collaboration.ReviewAgentRequestCompletionRequest

type AgentRequestResult = collaboration.AgentRequestResult

type CollaborationService = collaboration.CollaborationService

// NewCollaborationService forwards to collaboration.NewCollaborationService.
func NewCollaborationService(store models.CollaborationKernelStore, opts ...models.ServiceOption) *collaboration.CollaborationService {
	return collaboration.NewCollaborationService(store, opts...)
}

// CanStartAgentRequestFromRun forwards to collaboration.CanStartAgentRequestFromRun.
func CanStartAgentRequestFromRun(run *models.AgentRun) bool {
	return collaboration.CanStartAgentRequestFromRun(run)
}

// ValidateTaskDelegationSource forwards to collaboration.ValidateTaskDelegationSource.
func ValidateTaskDelegationSource(ctx context.Context, store models.PortfolioStore, source *models.AgentRun) error {
	return collaboration.ValidateTaskDelegationSource(ctx, store, source)
}

// StableCollaborationID forwards to collaboration.StableCollaborationID.
func StableCollaborationID(scope models.Scope, key, suffix string) string {
	return collaboration.StableCollaborationID(scope, key, suffix)
}

type AgentRequestConversationProjector = collaboration.AgentRequestConversationProjector

// NewAgentRequestConversationProjector forwards to collaboration.NewAgentRequestConversationProjector.
func NewAgentRequestConversationProjector(store interface {
	models.CollaborationKernelStore
	models.ConversationStore
}, opts ...models.ServiceOption) (*collaboration.AgentRequestConversationProjector, error) {
	return collaboration.NewAgentRequestConversationProjector(store, opts...)
}

type RoutineTeamCatalog = collaboration.RoutineTeamCatalog

// NewRoutineTeamRoster forwards to collaboration.NewRoutineTeamRoster.
func NewRoutineTeamRoster(teams collaboration.RoutineTeamCatalog) models.TeamRosterReader {
	return collaboration.NewRoutineTeamRoster(teams)
}

// IsSupportedTeamManagementSkillVersion forwards to collaboration.IsSupportedTeamManagementSkillVersion.
func IsSupportedTeamManagementSkillVersion(version string) bool {
	return collaboration.IsSupportedTeamManagementSkillVersion(version)
}

// TeamManagementSkill forwards to collaboration.TeamManagementSkill.
func TeamManagementSkill() *skill.Definition {
	return collaboration.TeamManagementSkill()
}

type TeamMemberActionValidator = collaboration.TeamMemberActionValidator

// NewTeamMemberActionValidator forwards to collaboration.NewTeamMemberActionValidator.
func NewTeamMemberActionValidator(teams *kernelteam.Registry) (*collaboration.TeamMemberActionValidator, error) {
	return collaboration.NewTeamMemberActionValidator(teams)
}

type TeamMemberActionDispatcher = collaboration.TeamMemberActionDispatcher

// NewTeamMemberActionDispatcher forwards to collaboration.NewTeamMemberActionDispatcher.
func NewTeamMemberActionDispatcher(store models.KernelStore, teams *kernelteam.Registry, fallback agentrun.ActionDispatcher) (*collaboration.TeamMemberActionDispatcher, error) {
	return collaboration.NewTeamMemberActionDispatcher(store, teams, fallback)
}
