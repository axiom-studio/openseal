package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

// AgentInstaller is the one Agent creation pipeline: the create_agent
// builder action and a shared-agent install both build through it, so the
// space and name checks, the ceiling, the definition, model, deployment,
// placement, bindings, control conversation, retire-on-failure and
// AfterAgentCreated cannot drift apart. An Engine builds one and shares it.
type AgentInstaller struct {
	store     KernelStore
	agents    *kernelagent.Registry
	teams     *kernelteam.Registry
	catalog   *skill.Catalog
	provision AgentProvisioner
	host      AgentBuilderHost
}

// NewAgentInstaller builds the installer. host may be nil: model choices are
// then refused at proposal time and nothing runs after creation.
func NewAgentInstaller(
	store KernelStore,
	agents *kernelagent.Registry,
	teams *kernelteam.Registry,
	catalog *skill.Catalog,
	provision AgentProvisioner,
	host AgentBuilderHost,
) (*AgentInstaller, error) {
	if store == nil || agents == nil || teams == nil || catalog == nil || provision == nil {
		return nil, errors.New("agent installer requires the kernel store, the Agent and Team registries, the Skill catalog and a provisioner")
	}
	return &AgentInstaller{store: store, agents: agents, teams: teams, catalog: catalog, provision: provision, host: host}, nil
}

// agentBuildInput is one reviewed Agent to create. Grants are resolved
// (risk recomputed from the catalog, account skills with their credential
// references) and MaximumRisk is the ceiling they need.
type agentBuildInput struct {
	Scope        Scope
	DefinitionID string
	DeploymentID string
	Profile      agentProfileArguments // normalized and validated; Model "" = host default
	SpaceID      string
	Grants       []resolvedAgentSkillGrant
	MaximumRisk  capability.RiskLevel
	Provenance   workforce.DefinitionProvenance
	Approver     string // the person whose review approved it; billing owner
	CallRef      string // AgentModelRequest.ActionCallID and AgentCreated.ActionCallID
	RunID        string // "" for a run-less install
	InstallKey   string // AgentCreated.InstallKey of a run-less install
	Reason       string // audit reason of the deployment and bindings
}

type agentBuildResult struct {
	definition   *kernelagent.AgentDefinition
	deployment   *kernelagent.AgentDeployment
	activation   *kernelagent.DefinitionActivation
	replayed     bool
	space        *Space
	bindings     []interface{}
	conversation *Conversation
}

// agentBuildRetryable marks a build error a fresh attempt (new ids) can get
// past: the reviewed space is gone, or this attempt's Agent was retired.
type agentBuildRetryable struct{ err error }

func (e agentBuildRetryable) Error() string { return e.err.Error() }
func (e agentBuildRetryable) Unwrap() error { return e.err }

// build creates the reviewed Agent. Every step is idempotent on the ids, so
// a replay converges on the same Agent. A failure after the deployment
// exists retires it, so the reviewed name is free again.
func (i *AgentInstaller) build(ctx context.Context, input agentBuildInput) (*agentBuildResult, error) {
	scope := input.Scope
	reference := agentBuilderScope(scope)
	approver := strings.TrimSpace(input.Approver)
	if approver == "" {
		return nil, errors.New("an Agent is created only after a person approves it")
	}
	// The name may have been taken while the review waited. This build's own
	// deployment is excluded so a replay passes.
	if err := ensureAgentNameAvailable(ctx, i.agents, i.teams, scope, input.Profile.DisplayName, input.DeploymentID); err != nil {
		return nil, err
	}
	space, err := i.createSpace(ctx, scope, input.SpaceID)
	if err != nil {
		return nil, err
	}
	definition, err := i.registerAgentBuilderDefinition(ctx, input.DefinitionID, input.Profile, input.MaximumRisk, input.Provenance)
	if err != nil {
		return nil, err
	}
	// The model is the one external step that can fail, so it is prepared
	// before the deployment exists: a failure leaves no Agent holding the
	// reviewed name.
	if err := i.prepareAgentModel(ctx, AgentModelRequest{
		Scope: reference, DeploymentID: input.DeploymentID, Model: input.Profile.Model, OwnerUserID: approver, ActionCallID: input.CallRef,
	}); err != nil {
		return nil, err
	}
	deployment, activation, replayed, err := i.createAgentDeployment(ctx, reference, input.DeploymentID, definition, approver, input.Reason)
	if err != nil {
		return nil, err
	}
	actor := skill.BindingActor{Type: "user", ID: approver}
	bindings, conversation, err := i.finishCreatedAgent(ctx, scope, space, deployment, input.Grants, actor, input.Reason)
	if err != nil {
		if retireErr := i.retireFailedAgent(ctx, reference, input.DeploymentID, approver); retireErr != nil {
			return nil, errors.Join(err, fmt.Errorf("retire the unfinished Agent: %w", retireErr))
		}
		return nil, agentBuildRetryable{fmt.Errorf("%w. The unfinished Agent was removed", err)}
	}
	if i.host != nil {
		i.host.AfterAgentCreated(ctx, AgentCreated{
			Scope: scope, DeploymentID: deployment.ID, DisplayName: definition.DisplayName, Purpose: definition.Purpose,
			ActionCallID: input.CallRef, RunID: input.RunID, BillingUserID: approver, InstallKey: input.InstallKey,
		})
	}
	return &agentBuildResult{
		definition: definition, deployment: deployment, activation: activation, replayed: replayed,
		space: space, bindings: bindings, conversation: conversation,
	}, nil
}

func sameDefinitionProvenance(left, right workforce.DefinitionProvenance) bool {
	return left.Source == right.Source && left.Reference == right.Reference
}

// finishCreatedAgent places the committed Agent in the reviewed space, binds
// the grants and returns its control conversation.
func (i *AgentInstaller) finishCreatedAgent(ctx context.Context, scope Scope, space *Space, deployment *kernelagent.AgentDeployment, grants []resolvedAgentSkillGrant, actor skill.BindingActor, reason string) ([]interface{}, *Conversation, error) {
	if err := i.placeAgentInSpace(ctx, scope, space, deployment.ID, actor); err != nil {
		return nil, nil, err
	}
	bindings := make([]interface{}, 0, len(grants))
	for _, grant := range grants {
		binding, err := i.grantAgentSkill(ctx, scope, deployment.ID, grant.grant, grant.credentials, actor, reason)
		if err != nil {
			return nil, nil, err
		}
		bindings = append(bindings, secretSafeBindingPreview(binding))
	}
	conversation, err := i.provision.EnsureAgentControlConversation(ctx, deployment)
	if err != nil {
		return nil, nil, err
	}
	return bindings, conversation, nil
}

// retireFailedAgent retires the deployment a failed build committed.
func (i *AgentInstaller) retireFailedAgent(ctx context.Context, reference capability.ScopeReference, deploymentID, approver string) error {
	current, err := i.agents.GetDeployment(ctx, reference, deploymentID)
	if err != nil {
		return err
	}
	if current.RolloutStatus == kernelagent.RolloutRetired {
		return nil
	}
	retired := *current
	retired.RolloutStatus = kernelagent.RolloutRetired
	_, _, err = i.agents.UpdateDeployment(ctx, &retired, current.Revision, "user", approver, agentBuilderCreateFailedReason)
	return err
}

// createSpace resolves the reviewed space at execution. A space deleted
// while the review waited fails the build.
func (i *AgentInstaller) createSpace(ctx context.Context, scope Scope, spaceID string) (*Space, error) {
	if spaceID == "" {
		return nil, errors.New("an Agent needs a space")
	}
	if spaceID == DefaultSpaceID(scope) {
		return &Space{ID: spaceID, Scope: scope, Name: DefaultSpaceName, IsDefault: true}, nil
	}
	spaces, err := i.provision.ListSpaces(ctx, scope)
	if err != nil {
		return nil, err
	}
	for _, space := range spaces {
		if space != nil && space.ID == spaceID {
			return space, nil
		}
	}
	return nil, agentBuildRetryable{fmt.Errorf("space %q no longer exists", spaceID)}
}

// registerAgentBuilderDefinition registers the reviewed profile as version
// 1.0.0 of definitionID, with maximumRisk the ceiling its reviewed grants
// need. A definition with the same provenance Source and Reference (an
// earlier attempt of the same build) counts as success.
func (i *AgentInstaller) registerAgentBuilderDefinition(ctx context.Context, definitionID string, profile agentProfileArguments, maximumRisk capability.RiskLevel, provenance workforce.DefinitionProvenance) (*kernelagent.AgentDefinition, error) {
	existing, err := i.agents.GetDefinition(ctx, definitionID, agentBuilderDefinitionVersion)
	if err == nil && existing != nil {
		if !sameDefinitionProvenance(existing.Provenance, provenance) {
			return nil, fmt.Errorf("agent definition %s already exists for another change", definitionID)
		}
		return existing, nil
	}
	if err != nil && !errors.Is(err, kernelagent.ErrDefinitionNotFound) {
		return nil, err
	}
	definition := &kernelagent.AgentDefinition{
		ID: definitionID, Version: agentBuilderDefinitionVersion,
		DisplayName: profile.DisplayName, Purpose: profile.Purpose, SystemPrompt: profile.SystemPrompt,
		Personality: profile.Personality, OperatingPrinciples: append([]string(nil), profile.OperatingPrinciples...),
		Authority:  kernelagent.AuthorityPolicy{MaximumRisk: maximumRisk, MaxConcurrentRuns: 1},
		Amendments: workforce.AmendmentPolicy{AgentMayPropose: true},
		Provenance: provenance,
	}
	registered, err := i.agents.RegisterDefinition(ctx, definition)
	if err != nil {
		// A concurrent attempt of the same call may have registered it.
		if existing, getErr := i.agents.GetDefinition(ctx, definitionID, agentBuilderDefinitionVersion); getErr == nil && existing != nil &&
			sameDefinitionProvenance(existing.Provenance, provenance) {
			return existing, nil
		}
		return nil, err
	}
	return registered, nil
}

// createAgentDeployment creates the deployment, or returns the one an
// earlier attempt of the call created (replayed true).
func (i *AgentInstaller) createAgentDeployment(ctx context.Context, reference capability.ScopeReference, deploymentID string, definition *kernelagent.AgentDefinition, approver, reason string) (*kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, bool, error) {
	existing, err := i.agents.GetDeployment(ctx, reference, deploymentID)
	if err == nil && existing != nil {
		if existing.RolloutStatus == kernelagent.RolloutRetired {
			return nil, nil, false, agentBuildRetryable{errors.New("this attempt failed earlier and its Agent was removed")}
		}
		if existing.DefinitionID != definition.ID {
			return nil, nil, false, fmt.Errorf("agent deployment %s already exists for another definition", deploymentID)
		}
		activation, err := i.firstAgentActivation(ctx, reference, deploymentID)
		return existing, activation, true, err
	}
	if err != nil && !errors.Is(err, kernelagent.ErrDeploymentNotFound) {
		return nil, nil, false, err
	}
	deployment, activation, err := i.provision.CreateAgentDeployment(ctx, &kernelagent.AgentDeployment{
		ID: deploymentID, Scope: reference, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "default",
		Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1}, Revision: 1,
	}, "user", approver, reason)
	if err != nil {
		if deployment == nil {
			return nil, nil, false, err
		}
		// The deployment committed; its control conversation is ensured
		// again below.
	}
	return deployment, activation, false, nil
}

func (i *AgentInstaller) firstAgentActivation(ctx context.Context, reference capability.ScopeReference, deploymentID string) (*kernelagent.DefinitionActivation, error) {
	activations, err := i.agents.ListActivations(ctx, reference, deploymentID)
	if err != nil {
		return nil, err
	}
	if len(activations) == 0 {
		return nil, nil
	}
	first := activations[0]
	for _, activation := range activations[1:] {
		if activation.CreatedAt.Before(first.CreatedAt) {
			first = activation
		}
	}
	return &first, nil
}

// placeAgentInSpace adds the Agent to the reviewed space. Outside Default it
// then leaves Default, where the deployment create placed it.
func (i *AgentInstaller) placeAgentInSpace(ctx context.Context, scope Scope, space *Space, deploymentID string, actor skill.BindingActor) error {
	if _, _, err := i.provision.PutSpaceMember(ctx, PutSpaceMemberRequest{
		Scope: scope, SpaceID: space.ID, Type: SpaceMemberAgent, MemberID: deploymentID,
	}); err != nil {
		return err
	}
	defaultSpace := DefaultSpaceID(scope)
	if space.ID == defaultSpace {
		return nil
	}
	if err := i.provision.DeleteSpaceMember(ctx, DeleteSpaceMemberRequest{
		Scope: scope, SpaceID: defaultSpace, Type: SpaceMemberAgent, MemberID: deploymentID, ActorType: actor.Type, ActorID: actor.ID,
	}); err != nil && !errors.Is(err, ErrSpaceNotFound) {
		return err
	}
	return nil
}

// grantAgentSkill binds one grant under its builder binding id. A binding
// that already matches the grant (an earlier attempt) is returned as is.
// credentials, when not nil, are the binding's credential references (an
// account skill's person refs); nil keeps the current binding's.
func (i *AgentInstaller) grantAgentSkill(ctx context.Context, scope Scope, deploymentID string, grant AgentSkillGrant, credentials map[string]skill.CredentialReference, actor skill.BindingActor, reason string) (*skill.Binding, error) {
	reference := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	bindingID := agentBuilderBindingID(deploymentID, grant.SkillID)
	args := skillBindingUpsertArguments{
		BindingID: bindingID, SkillID: grant.SkillID, SkillVersion: grant.SkillVersion, SourceIdentity: grant.SourceIdentity,
		AllowedActions: grant.AllowedActions, EnablePrompt: grant.EnablePrompt, MaximumRisk: grant.MaximumRisk, Config: grant.Config,
	}
	current, err := i.catalog.GetBinding(ctx, reference, deploymentID, bindingID)
	if err != nil && !errors.Is(err, skill.ErrBindingNotFound) {
		return nil, err
	}
	candidate := skillBindingCandidate(reference, deploymentID, args, current)
	if credentials != nil {
		candidate.Credentials = cloneCredentialReferences(credentials)
	}
	if current != nil && !current.Disabled && sameManagedSkillBinding(current, candidate) {
		return current, nil
	}
	var expected int64
	if current != nil {
		expected = current.Revision
	}
	binding, err := i.catalog.UpsertBinding(ctx, skill.UpsertBindingRequest{
		Binding: candidate, ExpectedRevision: expected, Actor: actor, Reason: reason,
	})
	if errors.Is(err, skill.ErrBindingRevisionConflict) {
		latest, getErr := i.catalog.GetBinding(ctx, reference, deploymentID, bindingID)
		if getErr == nil && latest != nil && !latest.Disabled && sameManagedSkillBinding(latest, candidate) {
			return latest, nil
		}
	}
	return binding, err
}

func (i *AgentInstaller) prepareAgentModel(ctx context.Context, request AgentModelRequest) error {
	if i.host == nil {
		return nil
	}
	if err := i.host.PrepareAgentModel(ctx, request); err != nil {
		return fmt.Errorf("prepare the Agent's model: %w", err)
	}
	return nil
}

// activateApprovedAgentVersion registers candidate (unless an earlier
// attempt did) and activates it as the approving person.
func (i *AgentInstaller) activateApprovedAgentVersion(ctx context.Context, reference capability.ScopeReference, deployment *kernelagent.AgentDeployment, candidate *kernelagent.AgentDefinition, approver string) (*kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, error) {
	if _, err := i.agents.GetDefinition(ctx, candidate.ID, candidate.Version); errors.Is(err, kernelagent.ErrDefinitionNotFound) {
		if _, err := i.agents.RegisterDefinition(ctx, candidate); err != nil {
			return nil, nil, err
		}
	} else if err != nil {
		return nil, nil, err
	}
	return i.agents.ActivateDefinition(ctx, reference, deployment.ID, candidate.Version, deployment.Revision, "user", approver, agentBuilderUpdateReason)
}

func (i *AgentInstaller) agentActivationTo(ctx context.Context, reference capability.ScopeReference, deploymentID, version string) (*kernelagent.DefinitionActivation, error) {
	activations, err := i.agents.ListActivations(ctx, reference, deploymentID)
	if err != nil {
		return nil, err
	}
	for index := len(activations) - 1; index >= 0; index-- {
		if activations[index].ToVersion == version {
			activation := activations[index]
			return &activation, nil
		}
	}
	return nil, nil
}

// disableAgentSkill disables one binding; an already disabled binding is done.
func (i *AgentInstaller) disableAgentSkill(ctx context.Context, scope skill.ScopeReference, deploymentID, bindingID string, actor skill.BindingActor) error {
	if strings.HasPrefix(bindingID, "bundled:") {
		return fmt.Errorf("binding %s is built in and cannot be removed", bindingID)
	}
	current, err := i.catalog.GetBinding(ctx, scope, deploymentID, bindingID)
	if err != nil && !errors.Is(err, skill.ErrBindingNotFound) {
		return err
	}
	if current == nil || current.Disabled {
		return nil
	}
	_, err = i.catalog.DisableBinding(ctx, skill.DisableBindingRequest{
		Scope: scope, DeploymentID: deploymentID, BindingID: bindingID, ExpectedRevision: current.Revision,
		Actor: actor, Reason: agentBuilderUpdateReason,
	})
	if errors.Is(err, skill.ErrBindingAlreadyDisabled) {
		return nil
	}
	return err
}
