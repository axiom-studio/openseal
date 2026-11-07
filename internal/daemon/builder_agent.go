package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	opensealkernel "github.com/axiom-studio/openseal/pkg/openseal"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// The OSS daemon's builder (D2): the Agent people talk to when they create
// Agents and Teams on desktop and in the TUI. HelloSeal's is Finn, which
// cortex provisions; both use the kernel's builder deployment id.
const (
	BuilderDefinitionID      = "openseal-builder"
	BuilderDefinitionVersion = "1.0.0"
	BuilderDisplayName       = "Builder"

	builderSkillsBindingPrefix = "bundled:skills@"
	builderSeedActorType       = "system"
	builderSeedActorID         = "openseal-daemon"
)

const builderSystemPrompt = "You are Builder, the OpenSeal agent that builds and changes the user's agents and teams. " +
	"When someone asks for an agent (a message that carries builder parameters always is one), or gives you a brief to adopt, don't play it or start the work: build it. " +
	"Use the name and model the builder parameters give, and start from the draft they carry if there is one. " +
	"Draft quickly: a short, distinctive name not already used, a one-line purpose, behaviour, personality and a few operating principles, and propose create_agent in your first step. " +
	"Ask only questions whose answer changes the agent, one at a time, and at most a few. " +
	"Keep per-task details (accounts, destinations, schedules, inputs) out of the profile; they are set up in the new agent's chat after creation. " +
	"Each later message refines the draft: propose a revised create_agent. Offer try_agent when a quick test would help. " +
	"To change an existing agent use update_agent, to make a team from existing agents use create_team, and to give an agent a skill use the skill tools with that agent as the target. " +
	"Never claim to have created or changed anything without a successful tool result."

// builderAgentEngine is the part of the Engine the seed uses.
type builderAgentEngine interface {
	GetAgentDefinition(ctx context.Context, id, version string) (*opensealkernel.AgentDefinition, error)
	RegisterAgentDefinition(ctx context.Context, definition *opensealkernel.AgentDefinition) (*opensealkernel.AgentDefinition, error)
	GetAgentDeployment(ctx context.Context, scope skill.ScopeReference, deploymentID string) (*opensealkernel.AgentDeployment, error)
	CreateAgentDeployment(ctx context.Context, deployment *opensealkernel.AgentDeployment, actorType, actorID, reason string) (*opensealkernel.AgentDeployment, *opensealkernel.AgentDefinitionActivation, error)
	ActivateAgentDefinition(ctx context.Context, scope skill.ScopeReference, deploymentID, version string, expectedRevision int64, actorType, actorID, reason string) (*opensealkernel.AgentDeployment, *opensealkernel.AgentDefinitionActivation, error)
	ListUmbrellaBindings(ctx context.Context, scope skill.ScopeReference, owner opensealkernel.ObjectiveOwner) ([]*skill.Binding, error)
	UpsertOwnerSkillBinding(ctx context.Context, owner opensealkernel.ObjectiveOwner, request skill.UpsertBindingRequest) (*skill.Binding, error)
	DeleteOwnerSkillBinding(ctx context.Context, owner opensealkernel.ObjectiveOwner, request skill.DeleteBindingRequest) (*skill.Binding, error)
}

var _ builderAgentEngine = (*opensealkernel.Engine)(nil)

func builderDefinition() *opensealkernel.AgentDefinition {
	return &opensealkernel.AgentDefinition{
		ID: BuilderDefinitionID, Version: BuilderDefinitionVersion, DisplayName: BuilderDisplayName,
		Purpose:      "Builds and changes agents and teams with you.",
		SystemPrompt: builderSystemPrompt,
		Authority: opensealkernel.AgentAuthorityPolicy{
			MaximumRisk:       opensealkernel.SkillRiskWrite,
			RequireApprovalAt: opensealkernel.SkillRiskWrite, // every write is a review card
			MaxConcurrentRuns: 4,
		},
		// Fixed: the builder never amends itself.
		Amendments: opensealkernel.AgentAmendmentPolicy{},
	}
}

// builderBindings are the builder's whole authority. The skills binding id
// carries its version, like cortex's version-qualified management binding.
func builderBindings(scope skill.ScopeReference) []*skill.Binding {
	return []*skill.Binding{
		{
			ID: "bundled:agents", Scope: scope, DeploymentID: runtime.AgentBuilderDeploymentID,
			SkillID: runtime.AgentManagementSkillID, SkillVersion: runtime.AgentManagementSkillVersion,
			AllowedActions: []string{runtime.AgentActionCreateAgent, runtime.AgentActionUpdateAgent, runtime.AgentActionTryAgent, runtime.AgentActionListAgents},
			MaximumRisk:    skill.RiskLevelWrite,
		},
		{
			ID: "bundled:teams", Scope: scope, DeploymentID: runtime.AgentBuilderDeploymentID,
			SkillID: runtime.TeamManagementSkillID, SkillVersion: runtime.TeamManagementSkillVersion,
			AllowedActions: []string{runtime.TeamActionCreateTeam},
			MaximumRisk:    skill.RiskLevelWrite,
		},
		{
			ID: builderSkillsBindingPrefix + runtime.SkillManagementSkillVersion, Scope: scope, DeploymentID: runtime.AgentBuilderDeploymentID,
			SkillID: runtime.SkillManagementSkillID, SkillVersion: runtime.SkillManagementSkillVersion,
			AllowedActions: []string{runtime.SkillActionDiscoverBinding, runtime.SkillActionUpsertBinding, runtime.SkillActionDisableBinding, runtime.SkillActionRequestSetup, runtime.SkillActionRequestConnection},
			MaximumRisk:    skill.RiskLevelWrite,
		},
	}
}

// SeedBuilderAgent makes sure scope has the builder at the current definition
// version with exactly the current builder bindings. It runs at every daemon
// start: an existing builder is kept (its conversations stay), moved to a new
// definition version in place, and its bindings follow the bundled Skill
// versions.
func SeedBuilderAgent(ctx context.Context, engine builderAgentEngine, scope runtime.Scope) error {
	if engine == nil || strings.TrimSpace(scope.Kind) == "" || strings.TrimSpace(scope.ID) == "" {
		return errors.New("the builder seed requires an engine and a scope")
	}
	reference := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	if err := registerBuilderDefinition(ctx, engine); err != nil {
		return err
	}
	if err := ensureBuilderDeployment(ctx, engine, reference); err != nil {
		return err
	}
	return reconcileBuilderBindings(ctx, engine, reference)
}

func registerBuilderDefinition(ctx context.Context, engine builderAgentEngine) error {
	want := builderDefinition()
	existing, err := engine.GetAgentDefinition(ctx, want.ID, want.Version)
	if err != nil && !errors.Is(err, opensealkernel.ErrAgentDefinitionNotFound) {
		return fmt.Errorf("read the builder definition: %w", err)
	}
	if existing == nil {
		if existing, err = engine.RegisterAgentDefinition(ctx, want); err != nil {
			return fmt.Errorf("register the builder definition: %w", err)
		}
	}
	if changed := opensealkernel.ChangedAgentDefinitionFields(existing, want); len(changed) != 0 {
		return fmt.Errorf("the stored builder definition %s@%s differs from the built-in one (%s)", want.ID, want.Version, strings.Join(changed, ", "))
	}
	return nil
}

func ensureBuilderDeployment(ctx context.Context, engine builderAgentEngine, scope skill.ScopeReference) error {
	deployment, err := engine.GetAgentDeployment(ctx, scope, runtime.AgentBuilderDeploymentID)
	if errors.Is(err, opensealkernel.ErrAgentDeploymentNotFound) {
		_, _, err = engine.CreateAgentDeployment(ctx, &opensealkernel.AgentDeployment{
			ID: runtime.AgentBuilderDeploymentID, DisplayName: BuilderDisplayName, Scope: scope,
			DefinitionID: BuilderDefinitionID, ActiveVersion: BuilderDefinitionVersion, Environment: "default",
			RolloutStatus: opensealkernel.AgentRolloutActive, Revision: 1,
			Capacity: opensealkernel.AgentDeploymentCapacity{MaxConcurrentRuns: 4, MaxQueuedRuns: 32},
		}, builderSeedActorType, builderSeedActorID, "Create the builder that makes agents and teams")
		if err != nil {
			return fmt.Errorf("create the builder: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the builder: %w", err)
	}
	if deployment.DefinitionID != BuilderDefinitionID {
		return fmt.Errorf("deployment %s already exists with definition %s, not the builder", runtime.AgentBuilderDeploymentID, deployment.DefinitionID)
	}
	if deployment.ActiveVersion == BuilderDefinitionVersion {
		return nil
	}
	if _, _, err := engine.ActivateAgentDefinition(ctx, scope, deployment.ID, BuilderDefinitionVersion, deployment.Revision,
		builderSeedActorType, builderSeedActorID, "Move the builder to the current built-in version"); err != nil {
		return fmt.Errorf("upgrade the builder to %s: %w", BuilderDefinitionVersion, err)
	}
	return nil
}

// reconcileBuilderBindings converges the stored bindings on builderBindings.
// A binding whose Skill version changed is replaced: an existing binding's
// Skill identity can't change through an upsert, and a reference upgrade
// keeps the old allowed actions, while the new version adds actions.
func reconcileBuilderBindings(ctx context.Context, engine builderAgentEngine, scope skill.ScopeReference) error {
	owner := opensealkernel.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: runtime.AgentBuilderDeploymentID}
	existing, err := engine.ListUmbrellaBindings(ctx, scope, owner)
	if err != nil {
		return fmt.Errorf("list the builder's bindings: %w", err)
	}
	desired := builderBindings(scope)
	current := make(map[string]*skill.Binding, len(existing))
	for _, binding := range existing {
		if binding != nil {
			current[binding.ID] = binding
		}
	}
	actor := skill.BindingActor{Type: builderSeedActorType, ID: builderSeedActorID}
	remove := func(binding *skill.Binding, reason string) error {
		_, err := engine.DeleteOwnerSkillBinding(ctx, owner, skill.DeleteBindingRequest{
			Scope: scope, DeploymentID: owner.ID, BindingID: binding.ID, ExpectedRevision: binding.Revision, Actor: actor, Reason: reason,
		})
		if err != nil && !errors.Is(err, skill.ErrBindingNotFound) {
			return fmt.Errorf("remove the builder binding %s: %w", binding.ID, err)
		}
		delete(current, binding.ID)
		return nil
	}
	// Earlier versions of the version-qualified skills binding.
	for id, binding := range current {
		if strings.HasPrefix(id, builderSkillsBindingPrefix) && !slices.ContainsFunc(desired, func(b *skill.Binding) bool { return b.ID == id }) {
			if err := remove(binding, "Retire a builder binding for an earlier Skill version"); err != nil {
				return err
			}
		}
	}
	for _, want := range desired {
		have := current[want.ID]
		if have != nil && (have.SkillID != want.SkillID || have.SkillVersion != want.SkillVersion || have.SourceIdentity != want.SourceIdentity) {
			if err := remove(have, "Replace a builder binding for a new Skill version"); err != nil {
				return err
			}
			have = nil
		}
		if have != nil && builderBindingCurrent(have, want) {
			continue
		}
		var revision int64
		if have != nil {
			revision = have.Revision
		}
		if _, err := engine.UpsertOwnerSkillBinding(ctx, owner, skill.UpsertBindingRequest{
			Binding: want, ExpectedRevision: revision, Actor: actor, Reason: "Give the builder its built-in Skills",
		}); err != nil {
			return fmt.Errorf("save the builder binding %s: %w", want.ID, err)
		}
	}
	return nil
}

func builderBindingCurrent(have, want *skill.Binding) bool {
	return !have.Disabled && have.MaximumRisk == want.MaximumRisk && have.EnablePrompt == want.EnablePrompt &&
		slices.Equal(sortedActions(have.AllowedActions), sortedActions(want.AllowedActions))
}

func sortedActions(actions []string) []string {
	sorted := slices.Clone(actions)
	slices.Sort(sorted)
	return sorted
}
