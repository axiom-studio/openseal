// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/runtime/skillbinding. Remove once in-flight branches have landed.
//
// Exported declarations of pkg/runtime/skillbinding, kept so that importers
// of pkg/runtime continue to compile.

package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/skill"
)

var ErrAgentBehaviorFixed = skillbinding.ErrAgentBehaviorFixed

type BindingActionArgumentResolver = skillbinding.BindingActionArgumentResolver

type EmbedSessionActionAuthorityValidator = skillbinding.EmbedSessionActionAuthorityValidator

const CredentialErrorPersonUnavailable = skillbinding.CredentialErrorPersonUnavailable

// ConnectAccountProvider forwards to skillbinding.ConnectAccountProvider.
func ConnectAccountProvider(r *models.SkillSetupRequest) string {
	return skillbinding.ConnectAccountProvider(r)
}

// IsExactConnectAccountWait forwards to skillbinding.IsExactConnectAccountWait.
func IsExactConnectAccountWait(run *models.AgentRun, requestID string) bool {
	return skillbinding.IsExactConnectAccountWait(run, requestID)
}

// ConnectAccountResolutionInstruction forwards to skillbinding.ConnectAccountResolutionInstruction.
func ConnectAccountResolutionInstruction(r *models.SkillSetupRequest) string {
	return skillbinding.ConnectAccountResolutionInstruction(r)
}

const SkillActionFindApps = skillbinding.SkillActionFindApps

const ConnectAppWakeType = skillbinding.ConnectAppWakeType

const FindAppsTypeApp = skillbinding.FindAppsTypeApp

const FindAppsTypeSkill = skillbinding.FindAppsTypeSkill

const FindAppsSetupApp = skillbinding.FindAppsSetupApp

const FindAppsSetupSkill = skillbinding.FindAppsSetupSkill

type AppCatalogEntry = skillbinding.AppCatalogEntry

type AppCatalog = skillbinding.AppCatalog

type AppCatalogRef = skillbinding.AppCatalogRef

// ConnectAppKey forwards to skillbinding.ConnectAppKey.
func ConnectAppKey(target models.ConnectAppTarget) string {
	return skillbinding.ConnectAppKey(target)
}

// ConnectedAppBinding forwards to skillbinding.ConnectedAppBinding.
func ConnectedAppBinding(set *skill.HostBindingSet, target models.ConnectAppTarget) *skill.Binding {
	return skillbinding.ConnectedAppBinding(set, target)
}

// ConnectedAppToolNames forwards to skillbinding.ConnectedAppToolNames.
func ConnectedAppToolNames(ctx context.Context, catalog *skill.Catalog, binding *skill.Binding) ([]string, error) {
	return skillbinding.ConnectedAppToolNames(ctx, catalog, binding)
}

// IsExactConnectAppWait forwards to skillbinding.IsExactConnectAppWait.
func IsExactConnectAppWait(run *models.AgentRun, requestID string) bool {
	return skillbinding.IsExactConnectAppWait(run, requestID)
}

// ConnectAppResolutionInstruction forwards to skillbinding.ConnectAppResolutionInstruction.
func ConnectAppResolutionInstruction(r *models.SkillSetupRequest, tools []string) string {
	return skillbinding.ConnectAppResolutionInstruction(r, tools)
}

const CredentialRequestWakeType = skillbinding.CredentialRequestWakeType

// IsExactCredentialRequestWait forwards to skillbinding.IsExactCredentialRequestWait.
func IsExactCredentialRequestWait(run *models.AgentRun, requestID string) bool {
	return skillbinding.IsExactCredentialRequestWait(run, requestID)
}

// CredentialRequestResolutionInstruction forwards to skillbinding.CredentialRequestResolutionInstruction.
func CredentialRequestResolutionInstruction(r *models.CredentialRequest) string {
	return skillbinding.CredentialRequestResolutionInstruction(r)
}

const SkillManagementSkillVersion = skillbinding.SkillManagementSkillVersion

const SkillActionDiscoverBinding = skillbinding.SkillActionDiscoverBinding

const SkillActionUpsertBinding = skillbinding.SkillActionUpsertBinding

const SkillActionDisableBinding = skillbinding.SkillActionDisableBinding

const SkillManagementEndpoint = skillbinding.SkillManagementEndpoint

const SkillActionTargetAgentArgument = skillbinding.SkillActionTargetAgentArgument

// SkillManagementSkill forwards to skillbinding.SkillManagementSkill.
func SkillManagementSkill() *skill.Definition {
	return skillbinding.SkillManagementSkill()
}

type SkillBindingActionValidator = skillbinding.SkillBindingActionValidator

// NewSkillBindingActionValidator forwards to skillbinding.NewSkillBindingActionValidator.
func NewSkillBindingActionValidator(catalog *skill.Catalog, roster models.TeamRosterReader) (*skillbinding.SkillBindingActionValidator, error) {
	return skillbinding.NewSkillBindingActionValidator(catalog, roster)
}

type SkillBindingActionDispatcher = skillbinding.SkillBindingActionDispatcher

// NewSkillBindingActionDispatcher forwards to skillbinding.NewSkillBindingActionDispatcher.
func NewSkillBindingActionDispatcher(store models.KernelStore, catalog *skill.Catalog, fallback agentrun.ActionDispatcher, discovery ...skill.DiscoveryProvider) (*skillbinding.SkillBindingActionDispatcher, error) {
	return skillbinding.NewSkillBindingActionDispatcher(store, catalog, fallback, discovery...)
}

const SkillReferenceUpgradeAPIVersion = skillbinding.SkillReferenceUpgradeAPIVersion

var ErrSkillReferenceUpgradeUnavailable = skillbinding.ErrSkillReferenceUpgradeUnavailable

var ErrSkillReferenceUpgradeApproval = skillbinding.ErrSkillReferenceUpgradeApproval

var ErrSkillReferenceUpgradeCurrent = skillbinding.ErrSkillReferenceUpgradeCurrent

type PlanSkillReferenceUpgradeRequest = skillbinding.PlanSkillReferenceUpgradeRequest

type ApplySkillReferenceUpgradeRequest = skillbinding.ApplySkillReferenceUpgradeRequest

type SkillReferenceUpgradeService = skillbinding.SkillReferenceUpgradeService

// NewSkillReferenceUpgradeService forwards to skillbinding.NewSkillReferenceUpgradeService.
func NewSkillReferenceUpgradeService(store models.SkillReferenceUpgradeStore, catalog *skill.Catalog, opts ...models.ServiceOption) *skillbinding.SkillReferenceUpgradeService {
	return skillbinding.NewSkillReferenceUpgradeService(store, catalog, opts...)
}

const SkillActionListSetupRequests = skillbinding.SkillActionListSetupRequests

// RebaseSkillSetupRequestAfterBindingUpgrade forwards to skillbinding.RebaseSkillSetupRequestAfterBindingUpgrade.
func RebaseSkillSetupRequestAfterBindingUpgrade(request *models.SkillSetupRequest, expected int64, binding *skill.Binding) (*models.SkillSetupRequest, error) {
	return skillbinding.RebaseSkillSetupRequestAfterBindingUpgrade(request, expected, binding)
}

type TeamSkillAuthorityCatalog = skillbinding.TeamSkillAuthorityCatalog

// AuthorizeTeamSkillActivation forwards to skillbinding.AuthorizeTeamSkillActivation.
func AuthorizeTeamSkillActivation(ctx context.Context, catalog skillbinding.TeamSkillAuthorityCatalog, run *models.AgentRun, snapshot *skill.ActivationSnapshot) (*skill.ActivationSnapshot, error) {
	return skillbinding.AuthorizeTeamSkillActivation(ctx, catalog, run, snapshot)
}

type TeamSkillActionValidator = skillbinding.TeamSkillActionValidator

// NewTeamSkillActionValidator forwards to skillbinding.NewTeamSkillActionValidator.
func NewTeamSkillActionValidator(catalog skillbinding.TeamSkillAuthorityCatalog) (*skillbinding.TeamSkillActionValidator, error) {
	return skillbinding.NewTeamSkillActionValidator(catalog)
}

type TeamSkillActionDispatcher = skillbinding.TeamSkillActionDispatcher

// NewTeamSkillActionDispatcher forwards to skillbinding.NewTeamSkillActionDispatcher.
func NewTeamSkillActionDispatcher(catalog skillbinding.TeamSkillAuthorityCatalog, next agentrun.ActionDispatcher) (*skillbinding.TeamSkillActionDispatcher, error) {
	return skillbinding.NewTeamSkillActionDispatcher(catalog, next)
}
