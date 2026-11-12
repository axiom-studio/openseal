// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/runtime/agentbuilder. Remove once in-flight branches have landed.
//
// Exported declarations of pkg/runtime/agentbuilder, kept so that importers
// of pkg/runtime continue to compile.

package runtime

import (
	"context"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

type AgentBehaviorActionValidator = agentbuilder.AgentBehaviorActionValidator

// NewAgentBehaviorActionValidator forwards to agentbuilder.NewAgentBehaviorActionValidator.
func NewAgentBehaviorActionValidator(agents *kernelagent.Registry, endpoints ...*externalconversation.ExternalConversationEndpointService) (*agentbuilder.AgentBehaviorActionValidator, error) {
	return agentbuilder.NewAgentBehaviorActionValidator(agents, endpoints...)
}

type AgentBehaviorActionDispatcher = agentbuilder.AgentBehaviorActionDispatcher

// NewAgentBehaviorActionDispatcher forwards to agentbuilder.NewAgentBehaviorActionDispatcher.
func NewAgentBehaviorActionDispatcher(store models.KernelStore, agents *kernelagent.Registry, fallback agentrun.ActionDispatcher, endpoints ...*externalconversation.ExternalConversationEndpointService) (*agentbuilder.AgentBehaviorActionDispatcher, error) {
	return agentbuilder.NewAgentBehaviorActionDispatcher(store, agents, fallback, endpoints...)
}

var ErrChannelTriggerRemoved = agentbuilder.ErrChannelTriggerRemoved

type StartAgentBuilderResult = agentbuilder.StartAgentBuilderResult

type AgentBuilderConversationWriter = agentbuilder.AgentBuilderConversationWriter

// StartAgentBuilderConversation forwards to agentbuilder.StartAgentBuilderConversation.
func StartAgentBuilderConversation(ctx context.Context, writer agentbuilder.AgentBuilderConversationWriter, req models.StartAgentBuilderRequest) (*agentbuilder.StartAgentBuilderResult, error) {
	return agentbuilder.StartAgentBuilderConversation(ctx, writer, req)
}

type AgentBuilderDraftFilter = agentbuilder.AgentBuilderDraftFilter

type AgentBuilderDraftStatus = agentbuilder.AgentBuilderDraftStatus

const AgentBuilderDraftDrafting = agentbuilder.AgentBuilderDraftDrafting

const AgentBuilderDraftNeedsInput = agentbuilder.AgentBuilderDraftNeedsInput

const AgentBuilderDraftReady = agentbuilder.AgentBuilderDraftReady

const AgentBuilderDraftNeedsApproval = agentbuilder.AgentBuilderDraftNeedsApproval

const AgentBuilderDraftStopped = agentbuilder.AgentBuilderDraftStopped

const AgentBuilderDraftFailed = agentbuilder.AgentBuilderDraftFailed

const AgentBuilderDraftIdle = agentbuilder.AgentBuilderDraftIdle

type AgentBuilderDraft = agentbuilder.AgentBuilderDraft

// ListAgentBuilderDrafts forwards to agentbuilder.ListAgentBuilderDrafts.
func ListAgentBuilderDrafts(ctx context.Context, reader models.AgentBuilderDraftReader, filter agentbuilder.AgentBuilderDraftFilter) ([]agentbuilder.AgentBuilderDraft, error) {
	return agentbuilder.ListAgentBuilderDrafts(ctx, reader, filter)
}

type AgentProvisioner = agentbuilder.AgentProvisioner

type AgentBuilderActionDispatcher = agentbuilder.AgentBuilderActionDispatcher

// NewAgentBuilderActionDispatcher forwards to agentbuilder.NewAgentBuilderActionDispatcher.
func NewAgentBuilderActionDispatcher(installer *agentbuilder.AgentInstaller, fallback agentrun.ActionDispatcher) (*agentbuilder.AgentBuilderActionDispatcher, error) {
	return agentbuilder.NewAgentBuilderActionDispatcher(installer, fallback)
}

var ErrAgentChangeForbidden = agentbuilder.ErrAgentChangeForbidden

type AgentBuilderHost = agentbuilder.AgentBuilderHost

type AgentModelRequest = agentbuilder.AgentModelRequest

type AgentCreated = agentbuilder.AgentCreated

type AgentBuilderCutoverHost = agentbuilder.AgentBuilderCutoverHost

const AgentTrialModelContextKey = agentbuilder.AgentTrialModelContextKey

const AgentTrialContextKey = agentbuilder.AgentTrialContextKey

type AgentTrialContext = agentbuilder.AgentTrialContext

type AgentBuilderActionValidator = agentbuilder.AgentBuilderActionValidator

// NewAgentBuilderActionValidator forwards to agentbuilder.NewAgentBuilderActionValidator.
func NewAgentBuilderActionValidator(agents *kernelagent.Registry, teams *kernelteam.Registry, catalog *skill.Catalog, spaces *portfolio.SpaceService, conversations models.ConversationStore, runs models.PortfolioStore, host agentbuilder.AgentBuilderHost) (*agentbuilder.AgentBuilderActionValidator, error) {
	return agentbuilder.NewAgentBuilderActionValidator(agents, teams, catalog, spaces, conversations, runs, host)
}

type AgentInstaller = agentbuilder.AgentInstaller

// NewAgentInstaller forwards to agentbuilder.NewAgentInstaller.
func NewAgentInstaller(store models.KernelStore, agents *kernelagent.Registry, teams *kernelteam.Registry, catalog *skill.Catalog, provision agentbuilder.AgentProvisioner, host agentbuilder.AgentBuilderHost) (*agentbuilder.AgentInstaller, error) {
	return agentbuilder.NewAgentInstaller(store, agents, teams, catalog, provision, host)
}

const AgentTrialStatusCompleted = agentbuilder.AgentTrialStatusCompleted

const AgentTrialStatusFailed = agentbuilder.AgentTrialStatusFailed

const AgentTrialStatusTimedOut = agentbuilder.AgentTrialStatusTimedOut

type AgentTrialFinalizer = agentbuilder.AgentTrialFinalizer

// NewAgentTrialFinalizer forwards to agentbuilder.NewAgentTrialFinalizer.
func NewAgentTrialFinalizer(store models.RunCommandStore, next agentrun.RunTerminalFinalizer, wake func(models.Scope), opts ...models.ServiceOption) *agentbuilder.AgentTrialFinalizer {
	return agentbuilder.NewAgentTrialFinalizer(store, next, wake, opts...)
}

const SharedAgentFieldName = agentbuilder.SharedAgentFieldName

const SharedAgentFieldPurpose = agentbuilder.SharedAgentFieldPurpose

const SharedAgentFieldInstructions = agentbuilder.SharedAgentFieldInstructions

const SharedAgentFieldPersonality = agentbuilder.SharedAgentFieldPersonality

const SharedAgentFieldPrinciples = agentbuilder.SharedAgentFieldPrinciples

const SharedAgentFieldSkills = agentbuilder.SharedAgentFieldSkills

const SharedAgentProvenanceSource = agentbuilder.SharedAgentProvenanceSource

const SharedAgentRefusalVaultCredential = agentbuilder.SharedAgentRefusalVaultCredential

const SharedAgentRefusalChannelAdapter = agentbuilder.SharedAgentRefusalChannelAdapter

const SharedAgentRefusalNotInCatalog = agentbuilder.SharedAgentRefusalNotInCatalog

const SharedAgentSkillUnavailable = agentbuilder.SharedAgentSkillUnavailable

var ErrAgentNameTaken = agentbuilder.ErrAgentNameTaken

var ErrSharedAgentReviewChanged = agentbuilder.ErrSharedAgentReviewChanged

var ErrSharedAgentNotShareable = agentbuilder.ErrSharedAgentNotShareable

var ErrSharedAgentIsCopy = agentbuilder.ErrSharedAgentIsCopy

var ErrSharedAgentNotCopy = agentbuilder.ErrSharedAgentNotCopy

var ErrSharedAgentSkillUnavailable = agentbuilder.ErrSharedAgentSkillUnavailable

type SharedAgentSkillUnavailableError = agentbuilder.SharedAgentSkillUnavailableError

type SharedAgentProfile = agentbuilder.SharedAgentProfile

type SharedAgentSlot = agentbuilder.SharedAgentSlot

type SharedAgentSkill = agentbuilder.SharedAgentSkill

type SharedAgentExportSkill = agentbuilder.SharedAgentExportSkill

type SharedAgentExcluded = agentbuilder.SharedAgentExcluded

type SharedAgentRefusal = agentbuilder.SharedAgentRefusal

type SharedAgentExport = agentbuilder.SharedAgentExport

type SharedAgentSource = agentbuilder.SharedAgentSource

type SharedAgentInstallRequest = agentbuilder.SharedAgentInstallRequest

type SharedAgentPlannedSkill = agentbuilder.SharedAgentPlannedSkill

type SharedAgentPlannedSlot = agentbuilder.SharedAgentPlannedSlot

type SharedAgentInstallPlan = agentbuilder.SharedAgentInstallPlan

type SharedAgentInstalled = agentbuilder.SharedAgentInstalled

type SharedAgentUpdateRequest = agentbuilder.SharedAgentUpdateRequest

type SharedAgentSpec = agentbuilder.SharedAgentSpec

const SharedAgentChangeProfile = agentbuilder.SharedAgentChangeProfile

const SharedAgentChangeSkillAdded = agentbuilder.SharedAgentChangeSkillAdded

const SharedAgentChangeSkillRemoved = agentbuilder.SharedAgentChangeSkillRemoved

const SharedAgentChangeSkillChanged = agentbuilder.SharedAgentChangeSkillChanged

type SharedAgentChange = agentbuilder.SharedAgentChange

type SharedAgentRiskChange = agentbuilder.SharedAgentRiskChange

type SharedAgentUpdatePreview = agentbuilder.SharedAgentUpdatePreview

type SharedAgentUpdated = agentbuilder.SharedAgentUpdated

type TeamCreateActionValidator = agentbuilder.TeamCreateActionValidator

// NewTeamCreateActionValidator forwards to agentbuilder.NewTeamCreateActionValidator.
func NewTeamCreateActionValidator(agents *kernelagent.Registry, teams *kernelteam.Registry, spaces *portfolio.SpaceService, conversations models.ConversationStore, runs models.PortfolioStore) (*agentbuilder.TeamCreateActionValidator, error) {
	return agentbuilder.NewTeamCreateActionValidator(agents, teams, spaces, conversations, runs)
}
