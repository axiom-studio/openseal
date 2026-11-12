// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/runtime/collaboration. Remove once in-flight branches have landed.
//
// Exported declarations of pkg/runtime/collaboration, kept so that importers
// of pkg/runtime continue to compile.

package runtime

import (
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

const AgentRequestDecisionOutputKey = collaboration.AgentRequestDecisionOutputKey

const AgentRequestCompletionReviewOutputKey = collaboration.AgentRequestCompletionReviewOutputKey

type AgentRequestInboxReconcileResult = collaboration.AgentRequestInboxReconcileResult

type AgentRequestInboxReconciler = collaboration.AgentRequestInboxReconciler

// NewAgentRequestInboxReconciler forwards to collaboration.NewAgentRequestInboxReconciler.
func NewAgentRequestInboxReconciler(store models.AgentRequestInboxStore, opts ...models.ServiceOption) (*collaboration.AgentRequestInboxReconciler, error) {
	return collaboration.NewAgentRequestInboxReconciler(store, opts...)
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

type AgentRequestConversationProjector = collaboration.AgentRequestConversationProjector

// NewAgentRequestConversationProjector forwards to collaboration.NewAgentRequestConversationProjector.
func NewAgentRequestConversationProjector(store interface {
	models.CollaborationKernelStore
	models.ConversationStore
}, opts ...models.ServiceOption) (*collaboration.AgentRequestConversationProjector, error) {
	return collaboration.NewAgentRequestConversationProjector(store, opts...)
}

type RoutineTeamCatalog = collaboration.RoutineTeamCatalog

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
