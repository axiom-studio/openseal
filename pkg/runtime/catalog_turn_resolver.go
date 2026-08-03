package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// AgentTurnCatalog is the portable, product-neutral catalog required to bind
// a durable Run to its immutable Agent definition and activated Skills.
type AgentTurnCatalog interface {
	GetAgentDeployment(context.Context, skill.ScopeReference, string) (*kernelagent.AgentDeployment, error)
	ListAgentDeployments(context.Context, skill.ScopeReference) ([]*kernelagent.AgentDeployment, error)
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
	if _, requested := run.Context[AgentRequestInboxContextKey]; requested {
		if config.Host == nil {
			return nil, ErrTurnHostUnavailable
		}
		instructions := hostedAgentInstructions(definition)
		instructions = append(instructions, agentRequestDecisionSystemInstruction)
		runner, runnerErr := NewHostedTurnRunner(config.Host, HostedTurnRunnerConfig{
			AgentID: deployment.ID, ActionDeploymentID: deployment.ID,
			DefinitionID: definition.ID, DefinitionVersion: definition.Version,
			SystemInstructions: instructions, ModelCredential: deploymentModelCredential(deployment),
		})
		if runnerErr != nil {
			return nil, runnerErr
		}
		return &TurnRunnerBinding{
			Runner: &agentRequestDecisionTurnRunner{inner: runner}, DeploymentID: deployment.ID, ActionDeploymentID: deployment.ID,
			DefinitionID: definition.ID, DefinitionVersion: definition.Version,
			InputContextRefs:  []string{"run:" + AgentRequestInboxContextKey},
			BudgetReservation: BudgetUsage{Turns: 1},
		}, nil
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
	activation, err = restrictActivatedSkillsToRun(activation, run.Context)
	if err != nil {
		return nil, fmt.Errorf("apply Runbook Skill authority: %w", err)
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
		teamActivation, activateErr = restrictActivatedSkillsToRun(teamActivation, run.Context)
		if activateErr != nil {
			return nil, fmt.Errorf("apply Team Runbook Skill authority: %w", activateErr)
		}
		authorityCatalog, ok := catalog.(TeamSkillAuthorityCatalog)
		if !ok {
			return nil, errors.New("Team Skill authority resolution is unavailable")
		}
		teamActivation, activateErr = AuthorizeTeamSkillActivation(ctx, authorityCatalog, run, teamActivation)
		if activateErr != nil {
			return nil, fmt.Errorf("authorize bound Skills for Team %s: %w", run.Owner.ID, activateErr)
		}
		teamPrompts, teamActions, teamPrepared, teamContextRefs := projectActivatedSkills(teamActivation)
		mergedActions, shadowedBindings, mergeErr := mergeOwnerModelActions(actions, teamActions)
		if mergeErr != nil {
			return nil, mergeErr
		}
		if len(shadowedBindings) > 0 {
			filtered := prepared[:0]
			for _, runtime := range prepared {
				if _, shadowed := shadowedBindings[ownerBindingKey(runtime.DeploymentID, runtime.BindingID)]; !shadowed {
					filtered = append(filtered, runtime)
				}
			}
			prepared = filtered
		}
		prompts = append(prompts, teamPrompts...)
		actions = mergedActions
		prepared = append(prepared, teamPrepared...)
		contextRefs = append(contextRefs, teamContextRefs...)
	}
	runbookOperations := projectCallableRunbookOperations(definition.Runbook)
	if len(runbookOperations) > 0 {
		actions = withoutRunbookActivationStart(actions)
	}
	base := TurnRunnerBinding{
		DeploymentID: deployment.ID, ActionDeploymentID: actionDeploymentID, DefinitionID: definition.ID, DefinitionVersion: definition.Version,
		ModelActions: actions, PreparedRuntimes: prepared, InputContextRefs: contextRefs,
		RunbookOperations: runbookOperations,
		BudgetReservation: BudgetUsage{Turns: 1},
	}
	if delegatedFromCurrentRunbook(run, definition.Runbook) {
		base.RunbookOperations = nil
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
	if definition.Runbook != nil && strings.TrimSpace(run.Entrypoint) != "" && delegationMode != string(runbook.DelegateReason) {
		if err := validatePinnedRunbookPlan(definition.Runbook, run); err != nil {
			return nil, fmt.Errorf("resolve governed runbook for Agent %s: %w", deployment.ID, err)
		}
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
	eligibleAgents, err := resolveHostedAgentTargets(ctx, catalog, scope, run, deployment.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve eligible Agent delegation targets: %w", err)
	}
	instructions := hostedAgentInstructions(definition)
	acceptedRequestExecution := run.Source == RunSourceRequest || run.Source == RunSourceHandoff
	if acceptedRequestExecution {
		if _, ok := run.Context["collaboration"]; ok {
			instructions = append(instructions, acceptedAgentRequestExecutionSystemInstruction)
		} else {
			acceptedRequestExecution = false
		}
	}
	if delegated, ok := run.Context["delegatedSystemPrompt"].(string); ok && strings.TrimSpace(delegated) != "" {
		instructions = append(instructions, "Delegated execution instructions: "+strings.TrimSpace(delegated))
	}
	runner, err := NewHostedTurnRunner(config.Host, HostedTurnRunnerConfig{
		AgentID: deployment.ID, ActionDeploymentID: actionDeploymentID, DefinitionID: definition.ID, DefinitionVersion: definition.Version,
		SystemInstructions: instructions, EligibleAgents: eligibleAgents, SkillPrompts: prompts, Actions: actions,
		RunbookOperations: base.RunbookOperations,
		ModelCredential:   deploymentModelCredential(deployment),
	})
	if err != nil {
		return nil, err
	}
	base.Runner = runner
	if acceptedRequestExecution {
		base.Runner = &acceptedAgentRequestExecutionTurnRunner{inner: runner}
	}
	return &base, nil
}

const authorizedSkillCatalogIDsContextKey = "authorizedSkillCatalogIds"

// restrictActivatedSkillsToRun enforces the immutable capability envelope
// compiled onto a delegated Runbook step. Conversation providers used for
// approval delivery remain active for the runtime transport worker without
// becoming model-callable message or polling tools.
func restrictActivatedSkillsToRun(activation *skill.ActivationSnapshot, context map[string]interface{}) (*skill.ActivationSnapshot, error) {
	if activation == nil {
		return nil, errors.New("Skill activation is required")
	}
	raw, present := context[authorizedSkillCatalogIDsContextKey]
	if !present {
		return activation, nil
	}
	allowed := map[string]bool{}
	switch values := raw.(type) {
	case []string:
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				return nil, errors.New("authorized Skill catalog IDs must be non-empty")
			}
			allowed[value] = true
		}
	case []interface{}:
		for _, item := range values {
			value, ok := item.(string)
			value = strings.TrimSpace(value)
			if !ok || value == "" {
				return nil, errors.New("authorized Skill catalog IDs must be strings")
			}
			allowed[value] = true
		}
	default:
		return nil, errors.New("authorized Skill catalog IDs must be an array")
	}
	filtered := make([]skill.ActivatedSkill, 0, len(activation.Skills))
	for _, activated := range activation.Skills {
		if allowed[activated.SkillID] || (strings.TrimSpace(activated.SourceIdentity) != "" && allowed[activated.SourceIdentity]) {
			filtered = append(filtered, activated)
		}
	}
	copy := *activation
	copy.Skills = filtered
	return &copy, nil
}

func withoutRunbookActivationStart(actions []capability.ModelAction) []capability.ModelAction {
	filtered := make([]capability.ModelAction, 0, len(actions))
	for _, action := range actions {
		if action.SkillID == RunbookManagementSkillID && action.Action == RunbookActionStart {
			continue
		}
		filtered = append(filtered, action)
	}
	return filtered
}

func validatePinnedRunbookPlan(definition *runbook.Definition, run *AgentRun) error {
	if definition == nil || run == nil || len(run.Plan) == 0 {
		return nil
	}
	raw, present := run.Plan["runbook"]
	if !present {
		return nil
	}
	pin, ok := raw.(map[string]interface{})
	if !ok {
		return errors.New("pinned Runbook plan is malformed")
	}
	id, idOK := pin["id"].(string)
	version, versionOK := pin["version"].(string)
	triggerID, triggerOK := pin["trigger"].(string)
	if !idOK || !versionOK || !triggerOK || strings.TrimSpace(triggerID) == "" ||
		id != definition.ID || version != definition.Version {
		return errors.New("pinned Runbook identity is unavailable or stale")
	}
	trigger, ok := definition.Triggers[triggerID]
	if !ok || trigger.Entrypoint != strings.TrimSpace(run.Entrypoint) {
		return errors.New("pinned Runbook trigger is unavailable or stale")
	}
	return nil
}

func projectCallableRunbookOperations(definition *runbook.Definition) []HostedRunbookOperation {
	if definition == nil || len(definition.Interfaces) == 0 {
		return nil
	}
	entrypoints := make([]string, 0, len(definition.Interfaces))
	for entrypoint := range definition.Interfaces {
		entrypoints = append(entrypoints, entrypoint)
	}
	sort.Strings(entrypoints)
	operations := make([]HostedRunbookOperation, 0, len(entrypoints))
	for _, entrypoint := range entrypoints {
		contract := definition.Interfaces[entrypoint]
		operations = append(operations, HostedRunbookOperation{
			DefinitionID: definition.ID, DefinitionVersion: definition.Version,
			Entrypoint: entrypoint, Name: definition.Name + " · " + entrypoint,
			Description: contract.Description, InputSchema: cloneMap(contract.InputSchema), OutputSchema: cloneMap(contract.OutputSchema),
		})
	}
	return operations
}

func delegatedFromCurrentRunbook(run *AgentRun, definition *runbook.Definition) bool {
	if run == nil || definition == nil || run.Source != RunSourceRequest {
		return false
	}
	triggerInput, ok := run.Context["triggerInput"].(map[string]interface{})
	if !ok {
		return false
	}
	definitionID, _ := triggerInput["runbookDefinitionId"].(string)
	definitionVersion, _ := triggerInput["runbookDefinitionVersion"].(string)
	return strings.TrimSpace(definitionID) == definition.ID && strings.TrimSpace(definitionVersion) == definition.Version
}

func resolveHostedAgentTargets(
	ctx context.Context,
	catalog AgentTurnCatalog,
	scope skill.ScopeReference,
	run *AgentRun,
	currentDeploymentID string,
) ([]HostedAgentTarget, error) {
	if run == nil || run.Owner.Type != OwnerTypeTeam || strings.TrimSpace(run.Owner.ID) == "" {
		return nil, nil
	}
	teamCatalog, ok := catalog.(TeamSkillAuthorityCatalog)
	if !ok {
		return nil, errors.New("Team delegation authority resolution is unavailable")
	}
	teamDeployment, err := teamCatalog.GetTeamDeployment(ctx, scope, run.Owner.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve Team delegation roster: %w", err)
	}
	if teamDeployment == nil || teamDeployment.Status != kernelteam.DeploymentActive {
		return nil, errors.New("Team delegation roster is not active")
	}
	allowed := make(map[string]struct{}, len(teamDeployment.Roster))
	for _, assignment := range teamDeployment.Roster {
		if id := strings.TrimSpace(assignment.AgentDeploymentID); id != "" && id != currentDeploymentID {
			allowed[id] = struct{}{}
		}
	}
	if len(allowed) == 0 {
		return nil, nil
	}
	deployments, err := catalog.ListAgentDeployments(ctx, scope)
	if err != nil {
		return nil, err
	}
	targets := make([]HostedAgentTarget, 0, len(deployments))
	for _, candidate := range deployments {
		if candidate == nil || candidate.ID == currentDeploymentID ||
			candidate.RolloutStatus != kernelagent.RolloutActive || strings.TrimSpace(candidate.ActiveVersion) == "" {
			continue
		}
		if _, authorized := allowed[candidate.ID]; !authorized {
			continue
		}
		definition, err := catalog.GetAgentDefinition(ctx, candidate.DefinitionID, candidate.ActiveVersion)
		if err != nil {
			return nil, fmt.Errorf("resolve Agent %s definition: %w", candidate.ID, err)
		}
		if definition == nil {
			return nil, fmt.Errorf("resolve Agent %s definition: definition is unavailable", candidate.ID)
		}
		targets = append(targets, HostedAgentTarget{
			ID: strings.TrimSpace(candidate.ID), DisplayName: strings.TrimSpace(definition.DisplayName),
			Purpose: strings.TrimSpace(definition.Purpose),
		})
	}
	if len(targets) != len(allowed) {
		return nil, errors.New("Team delegation roster contains an unavailable Agent")
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	return targets, nil
}

func hostedAgentInstructions(definition *kernelagent.AgentDefinition) []string {
	if definition == nil {
		return nil
	}
	instructions := []string{definition.SystemPrompt, "Purpose: " + definition.Purpose}
	if definition.Personality != "" {
		instructions = append(instructions, "Personality: "+definition.Personality)
	}
	return append(instructions, definition.OperatingPrinciples...)
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
			shadowedBindings[ownerBindingKey(current.DeploymentID, current.BindingID)] = struct{}{}
			merged[index] = action
			continue
		}
		indexByName[action.Name] = len(merged)
		merged = append(merged, action)
	}
	return merged, shadowedBindings, nil
}

func ownerBindingKey(deploymentID, bindingID string) string { return deploymentID + "\x00" + bindingID }

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
	// Kernel management Skills have one well-known binding identity. Historical
	// authoring versions could leave an equivalent alias beside it; never expose
	// both aliases as indistinguishable tools while asynchronous reconciliation
	// catches up. Non-kernel Skills retain exact-binding ambiguity because their
	// bindings may select different credentials, configuration, or authority.
	canonicalManagement := make(map[string]string)
	for _, activated := range activation.Skills {
		canonicalID := workforceSkillBindingID(activation.DeploymentID, activated.SkillID, activated.SkillVersion)
		if !strings.HasPrefix(canonicalID, "bundled:") || activated.BindingID != canonicalID {
			continue
		}
		canonicalManagement[activated.SkillID+"@"+activated.SkillVersion] = canonicalID
	}
	for _, activated := range activation.Skills {
		if canonicalID := canonicalManagement[activated.SkillID+"@"+activated.SkillVersion]; canonicalID != "" && activated.BindingID != canonicalID {
			continue
		}
		if activated.Prompt != nil && strings.TrimSpace(activated.Prompt.Instructions) != "" {
			prompts = append(prompts, HostedSkillPrompt{
				SkillID: activated.SkillID, Version: activated.SkillVersion, BindingID: activated.BindingID, BindingRevision: activated.BindingRevision,
				Name: activated.Name, Description: activated.Description, Instructions: activated.Prompt.Instructions,
			})
			refs = append(refs, fmt.Sprintf("skill:%s@%s#binding:%s@%d", activated.SkillID, activated.SkillVersion, activated.BindingID, activated.BindingRevision))
		}
		for _, action := range activated.Actions {
			if strings.TrimSpace(action.DeploymentID) == "" {
				action.DeploymentID = activation.DeploymentID
			}
			actions = append(actions, action)
		}
		if activated.PreparedRuntime != nil && len(activated.Actions) > 0 {
			preparedRuntime := *activated.PreparedRuntime
			preparedRuntime.Executables = append([]string(nil), activated.PreparedRuntime.Executables...)
			prepared = append(prepared, PreparedSkillRuntime{
				DeploymentID: activation.DeploymentID, BindingID: activated.BindingID, BindingRevision: activated.BindingRevision,
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
