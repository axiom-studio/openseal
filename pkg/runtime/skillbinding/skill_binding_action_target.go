package skillbinding

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// skillActionMember is the acting member: the Agent whose own management
// binding the action runs through.
func skillActionMember(run *models.AgentRun) (string, error) {
	if run == nil {
		return "", errors.New("Agent Run is required")
	}
	if deploymentID := strings.TrimSpace(run.AssignedAgentID); deploymentID != "" {
		return deploymentID, nil
	}
	return "", errors.New("skill management requires a Run assigned to an Agent deployment")
}

// skillTargetAgent is the Agent a builder acts for (targetAgentId).
type skillTargetAgent struct {
	ID          string
	DisplayName string
}

func (t *skillTargetAgent) owner() models.ObjectiveOwner {
	return models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: t.ID}
}

func (t *skillTargetAgent) preview() map[string]interface{} {
	return map[string]interface{}{"id": t.ID, "displayName": t.DisplayName}
}

// IsTargetedSkillBindingAction reports whether bound is an openseal.skills
// upsert_binding or disable_binding whose arguments name another Agent with
// targetAgentId. It changes that Agent, so like create_agent and update_agent
// it is always reviewed and approved only by a person in the app, in every
// approval mode.
func IsTargetedSkillBindingAction(bound *skill.BoundAction, arguments map[string]interface{}) bool {
	if bound == nil || bound.Definition == nil || bound.Definition.ID != models.SkillManagementSkillID {
		return false
	}
	if bound.Action.Name != SkillActionUpsertBinding && bound.Action.Name != SkillActionDisableBinding {
		return false
	}
	_, present := arguments[SkillActionTargetAgentArgument]
	return present
}

// skillActionTargetable reports the actions that accept targetAgentId.
func skillActionTargetable(action string) bool {
	switch action {
	case SkillActionDiscoverBinding, SkillActionUpsertBinding, SkillActionDisableBinding, agentrun.SkillActionRequestSetup, agentrun.SkillActionRequestConnection:
		return true
	}
	return false
}

// skillActionTarget returns the Agent named by targetAgentId, or nil when
// the call names none. Only a builder (an Agent holding an openseal.agents
// binding that allows create_agent or update_agent) may name one, and only
// in a reply in its own chat. The target exists in scope, is not retired,
// is not the builder and is not fixed-policy, the same rule as update_agent.
func skillActionTarget(ctx context.Context, catalog *skill.Catalog, agents *kernelagent.Registry, run *models.AgentRun, member string, arguments map[string]interface{}) (*skillTargetAgent, error) {
	raw, present := arguments[SkillActionTargetAgentArgument]
	if !present {
		return nil, nil
	}
	value, _ := raw.(string)
	targetID := strings.TrimSpace(value)
	if targetID == "" {
		// A blank target names nobody: the model filled the optional
		// argument with nothing, so the call acts for this chat's Agent.
		return nil, nil
	}
	if len(targetID) > 128 {
		return nil, errors.New("targetAgentId must be an Agent deployment id; use list_agents for the exact id")
	}
	if targetID == member {
		return nil, errors.New("targetAgentId names the Agent you are; omit it for this chat's own Skills")
	}
	if catalog == nil || run == nil {
		return nil, errors.New("acting for another Agent is unavailable")
	}
	scope := skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}
	bindings, err := catalog.ListBindings(ctx, scope, member)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(bindings, agentBuilderBinding) {
		return nil, errors.New("only an Agent that builds Agents can act for another Agent; omit targetAgentId")
	}
	_, taskHint := run.Context[models.ConversationTaskContextKey]
	if run.Owner != (models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: member}) || run.Kind != models.RunKindConversation || taskHint || strings.HasPrefix(run.ConcurrencyKey, "task:") {
		return nil, errors.New("targetAgentId is only available in a reply in your own chat")
	}
	if agents == nil {
		return nil, errors.New("acting for another Agent is unavailable on this host")
	}
	deployment, err := agents.GetDeployment(ctx, capability.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, targetID)
	if err != nil {
		return nil, AgentBuilderTargetError(targetID, err)
	}
	if deployment.RolloutStatus == kernelagent.RolloutRetired {
		return nil, fmt.Errorf("agent %q is retired and cannot be changed", targetID)
	}
	definition, err := agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return nil, err
	}
	if AgentDefinitionPolicyFixed(definition) {
		return nil, fmt.Errorf("agent %q cannot be changed in chat: %w", targetID, ErrAgentBehaviorFixed)
	}
	return &skillTargetAgent{ID: deployment.ID, DisplayName: definition.DisplayName}, nil
}

// WithoutUnusableTargetAgent hides targetAgentId from the model-visible
// actions of an Agent that cannot use it. Only a builder may name another
// Agent (skillActionTarget), so every other Agent is offered the actions
// without the argument and without the sentence that describes it; a model
// that sees an optional "act for another Agent" field will fill it.
func WithoutUnusableTargetAgent(activation *skill.ActivationSnapshot, actions []capability.ModelAction) []capability.ModelAction {
	touched := false
	for _, action := range actions {
		if hasTargetAgentProperty(action) {
			touched = true
			break
		}
	}
	if !touched || activationHasAgentBuilder(activation) {
		return actions
	}
	out := make([]capability.ModelAction, len(actions))
	for i, action := range actions {
		out[i] = action
		if !hasTargetAgentProperty(action) {
			continue
		}
		out[i].InputSchema = maps.Clone(action.InputSchema)
		properties := make(map[string]interface{}, len(action.InputSchema["properties"].(map[string]interface{})))
		for name, property := range action.InputSchema["properties"].(map[string]interface{}) {
			if name != SkillActionTargetAgentArgument {
				properties[name] = property
			}
		}
		out[i].InputSchema["properties"] = properties
		out[i].Description = strings.Replace(action.Description, targetAgentConnectionSentence, "", 1)
	}
	return out
}

// activationHasAgentBuilder mirrors agentBuilderBinding on an activation: an
// openseal.agents binding offering create_agent or update_agent.
func activationHasAgentBuilder(activation *skill.ActivationSnapshot) bool {
	if activation == nil {
		return false
	}
	for _, activated := range activation.Skills {
		if activated.SkillID != models.AgentManagementSkillID || activated.SkillVersion != models.AgentManagementSkillVersion || activated.SourceIdentity != "" {
			continue
		}
		for _, action := range activated.Actions {
			if action.Action == models.AgentActionCreateAgent || action.Action == models.AgentActionUpdateAgent {
				return true
			}
		}
	}
	return false
}

// targetAgentConnectionSentence is the request_connection description
// sentence that only a builder can act on.
const targetAgentConnectionSentence = " With targetAgentId, the card still appears in this chat and the app is connected for that Agent."

func hasTargetAgentProperty(action capability.ModelAction) bool {
	if action.SkillID != models.SkillManagementSkillID || !skillActionTargetable(action.Action) {
		return false
	}
	properties, _ := action.InputSchema["properties"].(map[string]interface{})
	_, ok := properties[SkillActionTargetAgentArgument]
	return ok
}

// agentBuilderBinding reports an enabled openseal.agents binding that lets
// its Agent create or change other Agents.
func agentBuilderBinding(binding *skill.Binding) bool {
	return binding != nil && !binding.Disabled && binding.SkillID == models.AgentManagementSkillID && binding.SkillVersion == models.AgentManagementSkillVersion &&
		binding.SourceIdentity == "" && (slices.Contains(binding.AllowedActions, models.AgentActionCreateAgent) || slices.Contains(binding.AllowedActions, models.AgentActionUpdateAgent))
}
