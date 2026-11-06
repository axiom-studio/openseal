package openseal

import (
	"context"
	"errors"
	"fmt"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

// ResolveUmbrella returns the Agent or Team that owns deployment id. Agent
// and Team IDs share one namespace, so the answer is never ambiguous.
func (e *Engine) ResolveUmbrella(ctx context.Context, scope skill.ScopeReference, id string) (ObjectiveOwner, error) {
	if e == nil || e.agents == nil || e.agents.DeploymentIDs() == nil {
		return ObjectiveOwner{}, errors.New("deployment ID namespace is not configured")
	}
	kind, err := e.agents.DeploymentIDs().DeploymentIDKind(ctx, scope, id)
	if err != nil {
		return ObjectiveOwner{}, err
	}
	switch kind {
	case workforce.DeploymentKindAgent:
		return ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: id}, nil
	case workforce.DeploymentKindTeam:
		return ObjectiveOwner{Type: runtime.OwnerTypeTeam, ID: id}, nil
	}
	return ObjectiveOwner{}, fmt.Errorf("deployment ID %s has unknown kind %q", id, kind)
}

// checkUmbrella verifies that owner is the Agent or Team holding deployment
// ID owner.ID in scope, and that it is neither retired nor archived.
func (e *Engine) checkUmbrella(ctx context.Context, scope skill.ScopeReference, owner ObjectiveOwner) error {
	if err := owner.Validate(); err != nil {
		return err
	}
	resolved, err := e.ResolveUmbrella(ctx, scope, owner.ID)
	if err != nil {
		return err
	}
	if resolved != owner {
		return fmt.Errorf("deployment %s is not a %s", owner.ID, owner.Type)
	}
	switch owner.Type {
	case runtime.OwnerTypeAgent:
		deployment, err := e.agents.GetDeployment(ctx, scope, owner.ID)
		if err != nil {
			return err
		}
		if deployment.RolloutStatus == kernelagent.RolloutRetired {
			return fmt.Errorf("Agent %s is retired", owner.ID)
		}
	case runtime.OwnerTypeTeam:
		deployment, err := e.teams.GetDeployment(ctx, scope, owner.ID)
		if err != nil {
			return err
		}
		if deployment.Status == kernelteam.DeploymentArchived {
			return fmt.Errorf("Team %s is archived", owner.ID)
		}
	}
	return nil
}

// ListUmbrellaBindings lists the stored bindings of an Agent or Team.
func (e *Engine) ListUmbrellaBindings(ctx context.Context, scope skill.ScopeReference, owner ObjectiveOwner) ([]*skill.Binding, error) {
	if err := e.checkUmbrella(ctx, scope, owner); err != nil {
		return nil, err
	}
	return e.skills.ListBindings(ctx, scope, owner.ID)
}

// UpsertOwnerSkillBinding creates or updates a binding of an Agent or Team.
// Saving it completes any pending setup request it satisfies, wherever it
// was saved (Settings, an OAuth callback, the chat form).
func (e *Engine) UpsertOwnerSkillBinding(ctx context.Context, owner ObjectiveOwner, request skill.UpsertBindingRequest) (*skill.Binding, error) {
	if request.Binding == nil || request.Binding.DeploymentID != owner.ID {
		return nil, errors.New("Skill binding must belong to its Agent or Team")
	}
	if err := e.checkUmbrella(ctx, request.Binding.Scope, owner); err != nil {
		return nil, err
	}
	binding, err := e.skills.UpsertBinding(ctx, request)
	if err != nil {
		return nil, err
	}
	e.reconcileSkillSetupRequestsForBinding(ctx, binding)
	e.reconcileConnectAccountRequestsForUmbrella(ctx, request.Binding.Scope, owner)
	return binding, nil
}

// DisableOwnerSkillBinding disables one binding of an Agent or Team.
func (e *Engine) DisableOwnerSkillBinding(ctx context.Context, owner ObjectiveOwner, request skill.DisableBindingRequest) (*skill.Binding, error) {
	if request.DeploymentID != owner.ID {
		return nil, errors.New("Skill binding must belong to its Agent or Team")
	}
	if err := e.checkUmbrella(ctx, request.Scope, owner); err != nil {
		return nil, err
	}
	binding, err := e.skills.DisableBinding(ctx, request)
	if err != nil {
		return nil, err
	}
	e.reconcileSkillSetupRequestsForBinding(ctx, binding)
	e.reconcileConnectAccountRequestsForUmbrella(ctx, request.Scope, owner)
	return binding, nil
}

// DeleteOwnerSkillBinding permanently removes one binding of an Agent or
// Team at its expected revision. No row remains: every action, prompt,
// adapter and credential lease that referenced it stops resolving, and the
// ID can be reused. The result is the receipt with a final "deleted" entry.
func (e *Engine) DeleteOwnerSkillBinding(ctx context.Context, owner ObjectiveOwner, request skill.DeleteBindingRequest) (*skill.Binding, error) {
	if request.DeploymentID != owner.ID {
		return nil, errors.New("Skill binding must belong to its Agent or Team")
	}
	if err := e.checkUmbrella(ctx, request.Scope, owner); err != nil {
		return nil, err
	}
	binding, err := e.skills.DeleteBinding(ctx, request)
	if err != nil {
		return nil, err
	}
	e.reconcileSkillSetupRequestsForBinding(ctx, binding)
	e.reconcileConnectAccountRequestsForUmbrella(ctx, request.Scope, owner)
	return binding, nil
}

// conversationOwnedBy returns conversation conversationID when owner owns it.
func (e *Engine) conversationOwnedBy(ctx context.Context, scope runtime.Scope, owner ObjectiveOwner, conversationID string) error {
	conversation, err := e.GetConversation(ctx, scope, conversationID)
	if err != nil {
		return err
	}
	if conversation == nil || conversation.Owner != owner {
		return runtime.ErrConversationNotFound
	}
	return nil
}
