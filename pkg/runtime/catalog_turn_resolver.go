package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// AgentTurnCatalog is the portable, product-neutral catalog required to bind
// a durable Run to its immutable Agent definition and activated Skills.
type AgentTurnCatalog interface {
	GetAgentDeployment(context.Context, skill.ScopeReference, string) (*kernelagent.AgentDeployment, error)
	GetAgentDefinition(context.Context, string, string) (*kernelagent.AgentDefinition, error)
	ActivateSkills(context.Context, skill.ScopeReference, string, skill.HostCapabilityState) (*skill.ActivationSnapshot, error)
	OutreachTurnLifecycle
}

type CatalogTurnResolverConfig struct {
	Host       TurnHost
	SkillHost  skill.HostCapabilityState
	SkillHosts SkillHostCapabilityResolver
}

// SkillHostCapabilityResolver lets an embedding runtime project the exact
// adapters available to one deployment without duplicating Agent, Skill, and
// Turn resolution. The returned state is host-authoritative and never model
// writable.
type SkillHostCapabilityResolver interface {
	ResolveSkillHostCapabilities(context.Context, skill.ScopeReference, string) (*skill.HostCapabilityState, error)
}

type SkillHostCapabilityResolverFunc func(context.Context, skill.ScopeReference, string) (*skill.HostCapabilityState, error)

func (f SkillHostCapabilityResolverFunc) ResolveSkillHostCapabilities(ctx context.Context, scope skill.ScopeReference, deploymentID string) (*skill.HostCapabilityState, error) {
	return f(ctx, scope, deploymentID)
}

// ResolveCatalogTurnRunner resolves deterministic runbooks and typed Skill
// invocations inside the kernel. General prompt work additionally requires a
// bounded TurnHost; callers must not advertise general work creation without
// one.
func ResolveCatalogTurnRunner(ctx context.Context, catalog AgentTurnCatalog, run *AgentRun, config CatalogTurnResolverConfig) (*TurnRunnerBinding, error) {
	if catalog == nil {
		return nil, errors.New("canonical Agent turn catalog is unavailable")
	}
	if run == nil || run.Kind != RunKindAgentWork || strings.TrimSpace(run.AssignedAgentID) == "" {
		return nil, errors.New("Agent work requires an assigned Agent")
	}
	scope := skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}
	deployment, err := catalog.GetAgentDeployment(ctx, scope, run.AssignedAgentID)
	if err != nil || deployment == nil {
		return nil, fmt.Errorf("resolve Agent deployment %s: %w", run.AssignedAgentID, err)
	}
	if deployment.RolloutStatus != kernelagent.RolloutActive || strings.TrimSpace(deployment.ActiveVersion) == "" {
		return nil, errors.New("assigned Agent deployment is not active")
	}
	definition, err := catalog.GetAgentDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil || definition == nil {
		return nil, fmt.Errorf("resolve active Agent definition: %w", err)
	}
	skillHost := config.SkillHost
	if config.SkillHosts != nil {
		resolved, resolveErr := config.SkillHosts.ResolveSkillHostCapabilities(ctx, scope, deployment.ID)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve Skill execution host for Agent %s: %w", deployment.ID, resolveErr)
		}
		if resolved == nil {
			return nil, fmt.Errorf("resolve Skill execution host for Agent %s: empty capability state", deployment.ID)
		}
		skillHost = *resolved
	}
	activation, err := catalog.ActivateSkills(ctx, scope, deployment.ID, skillHost)
	if err != nil {
		return nil, fmt.Errorf("activate bound Skills for Agent %s: %w", deployment.ID, err)
	}
	if activation == nil || strings.TrimSpace(activation.SnapshotID) == "" {
		return nil, errors.New("Skill activation returned no immutable snapshot")
	}
	prompts, actions, prepared, contextRefs := projectActivatedSkills(activation)
	actionDeploymentID := deployment.ID
	if run.Owner.Type == OwnerTypeTeam && strings.TrimSpace(run.Owner.ID) != "" {
		teamActivation, activateErr := catalog.ActivateSkills(ctx, scope, run.Owner.ID, skillHost)
		if activateErr != nil {
			return nil, fmt.Errorf("activate bound Skills for Team %s: %w", run.Owner.ID, activateErr)
		}
		if teamActivation == nil || strings.TrimSpace(teamActivation.SnapshotID) == "" {
			return nil, errors.New("Team Skill activation returned no immutable snapshot")
		}
		teamPrompts, teamActions, teamPrepared, teamContextRefs := projectActivatedSkills(teamActivation)
		mergedActions, shadowedBindings, mergeErr := mergeOwnerModelActions(actions, teamActions)
		if mergeErr != nil {
			return nil, mergeErr
		}
		if len(shadowedBindings) > 0 {
			filtered := prepared[:0]
			for _, runtime := range prepared {
				if _, shadowed := shadowedBindings[runtime.BindingID]; !shadowed {
					filtered = append(filtered, runtime)
				}
			}
			prepared = filtered
		}
		prompts = append(prompts, teamPrompts...)
		actions = mergedActions
		prepared = append(prepared, teamPrepared...)
		contextRefs = append(contextRefs, teamContextRefs...)
		actionDeploymentID = run.Owner.ID
	}
	base := TurnRunnerBinding{
		DeploymentID: deployment.ID, ActionDeploymentID: actionDeploymentID, DefinitionID: definition.ID, DefinitionVersion: definition.Version,
		ModelActions: actions, PreparedRuntimes: prepared, InputContextRefs: contextRefs,
		BudgetReservation: BudgetUsage{Turns: 1},
	}
	if _, requested := run.Context[OutreachInvocationContextKey]; requested {
		runner, resolveErr := NewOutreachTurnRunner(catalog, actions)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve governed outreach for Agent %s: %w", deployment.ID, resolveErr)
		}
		base.Runner = runner
		base.InputContextRefs = append(base.InputContextRefs, "run:"+OutreachInvocationContextKey)
		return &base, nil
	}
	if _, requested := run.Context["capabilityInvocation"]; requested {
		runner, resolveErr := NewCapabilityInvocationTurnRunner(actions)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve typed capability invocation for Agent %s: %w", deployment.ID, resolveErr)
		}
		base.Runner = runner
		base.InputContextRefs = append(base.InputContextRefs, "run:capabilityInvocation")
		return &base, nil
	}
	delegationMode, _ := run.Context[DelegationModeContextKey].(string)
	if definition.Runbook != nil && delegationMode != string(runbook.DelegateReason) {
		entrypoint := catalogRunbookEntrypoint(definition, run)
		runner, resolveErr := NewRunbookTurnRunner(definition.Runbook, entrypoint)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve governed runbook for Agent %s: %w", deployment.ID, resolveErr)
		}
		base.Runner = runner
		base.InputContextRefs = append(base.InputContextRefs, "runbook:"+definition.Runbook.ID+"@"+definition.Runbook.Version)
		return &base, nil
	}
	if config.Host == nil {
		return nil, ErrTurnHostUnavailable
	}
	instructions := []string{definition.SystemPrompt, "Purpose: " + definition.Purpose}
	if definition.Personality != "" {
		instructions = append(instructions, "Personality: "+definition.Personality)
	}
	instructions = append(instructions, definition.OperatingPrinciples...)
	if delegated, ok := run.Context["delegatedSystemPrompt"].(string); ok && strings.TrimSpace(delegated) != "" {
		instructions = append(instructions, "Delegated execution instructions: "+strings.TrimSpace(delegated))
	}
	runner, err := NewHostedTurnRunner(config.Host, HostedTurnRunnerConfig{
		AgentID: deployment.ID, ActionDeploymentID: actionDeploymentID, DefinitionID: definition.ID, DefinitionVersion: definition.Version,
		SystemInstructions: instructions, SkillPrompts: prompts, Actions: actions,
		ModelCredential: deploymentModelCredential(deployment),
	})
	if err != nil {
		return nil, err
	}
	base.Runner = runner
	return &base, nil
}

func mergeOwnerModelActions(existing, owner []capability.ModelAction) ([]capability.ModelAction, map[string]struct{}, error) {
	merged := append([]capability.ModelAction(nil), existing...)
	indexByName := make(map[string]int, len(merged)+len(owner))
	for index, action := range merged {
		if _, duplicate := indexByName[action.Name]; duplicate {
			return nil, nil, fmt.Errorf("Agent Skill activation exposes ambiguous action %q", action.Name)
		}
		indexByName[action.Name] = index
	}
	shadowedBindings := make(map[string]struct{})
	for _, action := range owner {
		if index, duplicate := indexByName[action.Name]; duplicate {
			current := merged[index]
			if current.SkillID != action.SkillID || current.Version != action.Version || current.Action != action.Action {
				return nil, nil, fmt.Errorf("Team and Agent Skill activations expose conflicting action %q", action.Name)
			}
			shadowedBindings[current.BindingID] = struct{}{}
			merged[index] = action
			continue
		}
		indexByName[action.Name] = len(merged)
		merged = append(merged, action)
	}
	return merged, shadowedBindings, nil
}

func deploymentModelCredential(deployment *kernelagent.AgentDeployment) *capability.CredentialReference {
	if deployment == nil {
		return nil
	}
	reference, ok := deployment.Credentials["MODEL_PROVIDER"]
	if !ok {
		return nil
	}
	cloned := reference
	return &cloned
}

func projectActivatedSkills(activation *skill.ActivationSnapshot) ([]HostedSkillPrompt, []capability.ModelAction, []PreparedSkillRuntime, []string) {
	prompts := make([]HostedSkillPrompt, 0, len(activation.Skills))
	actions := make([]capability.ModelAction, 0)
	prepared := make([]PreparedSkillRuntime, 0)
	refs := []string{"skill-snapshot:" + activation.SnapshotID}
	for _, activated := range activation.Skills {
		if activated.Prompt != nil && strings.TrimSpace(activated.Prompt.Instructions) != "" {
			prompts = append(prompts, HostedSkillPrompt{
				SkillID: activated.SkillID, Version: activated.SkillVersion, BindingID: activated.BindingID, BindingRevision: activated.BindingRevision,
				Name: activated.Name, Description: activated.Description, Instructions: activated.Prompt.Instructions,
			})
			refs = append(refs, fmt.Sprintf("skill:%s@%s#binding:%s@%d", activated.SkillID, activated.SkillVersion, activated.BindingID, activated.BindingRevision))
		}
		actions = append(actions, activated.Actions...)
		if activated.PreparedRuntime != nil && len(activated.Actions) > 0 {
			preparedRuntime := *activated.PreparedRuntime
			preparedRuntime.Executables = append([]string(nil), activated.PreparedRuntime.Executables...)
			prepared = append(prepared, PreparedSkillRuntime{
				BindingID: activated.BindingID, BindingRevision: activated.BindingRevision,
				SkillID: activated.SkillID, SkillVersion: activated.SkillVersion, Runtime: preparedRuntime,
			})
		}
	}
	return prompts, actions, prepared, refs
}

func catalogRunbookEntrypoint(definition *kernelagent.AgentDefinition, run *AgentRun) string {
	if entrypoint := strings.TrimSpace(run.Entrypoint); entrypoint != "" {
		return entrypoint
	}
	base := "manual"
	switch run.Source {
	case RunSourceSchedule:
		base = "cron"
	case RunSourceWebhook:
		base = "webhook"
	case RunSourceEvent:
		base = "event"
		if trigger, ok := run.Checkpoint["trigger"].(map[string]interface{}); ok {
			for _, key := range []string{"kind", "type"} {
				if value, ok := trigger[key].(string); ok && (value == "k8s-event" || value == "k8s-watch" || value == "event") {
					base = value
				}
			}
		}
	}
	if definition != nil && definition.Runbook != nil {
		nodeID := ""
		if trigger, ok := run.Checkpoint["trigger"].(map[string]interface{}); ok {
			nodeID, _ = trigger["triggerNodeId"].(string)
		}
		if nodeID == "" {
			nodeID, _ = run.Context["triggerNodeId"].(string)
		}
		candidate := base + ":" + strings.TrimSpace(nodeID)
		if nodeID != "" {
			if _, ok := definition.Runbook.Entrypoints[candidate]; ok {
				return candidate
			}
		}
	}
	return base
}
