package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

type resolverCatalog struct {
	deployment            *kernelagent.AgentDeployment
	deployments           []*kernelagent.AgentDeployment
	definition            *kernelagent.AgentDefinition
	definitions           map[string]*kernelagent.AgentDefinition
	teamDeployment        *kernelteam.Deployment
	teamDefinition        *kernelteam.Definition
	activation            *skill.ActivationSnapshot
	activations           map[string]*skill.ActivationSnapshot
	activationDeployments []string
	gotHost               skill.HostCapabilityState
}

func TestRunbookSkillAuthorityExcludesConversationDeliveryTools(t *testing.T) {
	activation := &skill.ActivationSnapshot{SnapshotID: "snapshot", Skills: []skill.ActivatedSkill{
		{SkillID: "skill-browser", SkillVersion: "2", Actions: []capability.ModelAction{{SkillID: "skill-browser", Action: "commit"}}},
		{SkillID: "skill-slack", SkillVersion: "2", Actions: []capability.ModelAction{{SkillID: "skill-slack", Action: "slack-read-messages"}}},
	}}
	filtered, err := restrictActivatedSkillsToRun(activation, map[string]interface{}{
		authorizedSkillCatalogIDsContextKey: []interface{}{"skill-browser"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Skills) != 1 || filtered.Skills[0].SkillID != "skill-browser" || len(activation.Skills) != 2 {
		t.Fatalf("filtered activation = %#v; original = %#v", filtered.Skills, activation.Skills)
	}
	empty, err := restrictActivatedSkillsToRun(activation, map[string]interface{}{
		authorizedSkillCatalogIDsContextKey: []interface{}{},
	})
	if err != nil || len(empty.Skills) != 0 {
		t.Fatalf("empty authority = %#v, %v", empty, err)
	}
	if _, err := restrictActivatedSkillsToRun(activation, map[string]interface{}{
		authorizedSkillCatalogIDsContextKey: "skill-browser",
	}); err == nil {
		t.Fatal("scalar Skill authority accepted")
	}
}

func (c *resolverCatalog) GetTeamDeployment(context.Context, skill.ScopeReference, string) (*kernelteam.Deployment, error) {
	return c.teamDeployment, nil
}

func (c *resolverCatalog) GetTeamDefinition(context.Context, string, string) (*kernelteam.Definition, error) {
	return c.teamDefinition, nil
}

func (c *resolverCatalog) GetAgentDeployment(context.Context, skill.ScopeReference, string) (*kernelagent.AgentDeployment, error) {
	return c.deployment, nil
}

func (c *resolverCatalog) ListAgentDeployments(context.Context, skill.ScopeReference) ([]*kernelagent.AgentDeployment, error) {
	if c.deployments != nil {
		return c.deployments, nil
	}
	if c.deployment == nil {
		return nil, nil
	}
	return []*kernelagent.AgentDeployment{c.deployment}, nil
}

func (c *resolverCatalog) GetAgentDefinition(_ context.Context, id, version string) (*kernelagent.AgentDefinition, error) {
	if c.definitions != nil {
		return c.definitions[id+"@"+version], nil
	}
	return c.definition, nil
}

func (c *resolverCatalog) ActivateSkills(_ context.Context, _ skill.ScopeReference, deploymentID string, host skill.HostCapabilityState) (*skill.ActivationSnapshot, error) {
	c.gotHost = host
	c.activationDeployments = append(c.activationDeployments, deploymentID)
	if c.activations != nil {
		return c.activations[deploymentID], nil
	}
	return c.activation, nil
}

func (c *resolverCatalog) GetOutreachThread(context.Context, Scope, string) (*OutreachThread, error) {
	return nil, ErrOutreachThreadNotFound
}

func (c *resolverCatalog) ReconcileOutreachAction(context.Context, Scope, string, ReconcileOutreachActionRequest) (*ReconcileOutreachActionResult, error) {
	return nil, errors.New("not used")
}

func TestCatalogTurnResolverSelectsDeterministicKernelRunnersAndFailsClosedWithoutHost(t *testing.T) {
	scope := Scope{Kind: "local", ID: "research"}
	action := capability.ModelAction{
		Name: "forum.reply", BindingID: "forum-account", BindingRevision: 3,
		SkillID: "forum", Version: "1", Action: "reply", SideEffect: capability.SideEffectExternal,
	}
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{ID: "researcher", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "researcher", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive},
		definition: &kernelagent.AgentDefinition{ID: "researcher", Version: "1", Purpose: "Research communities", SystemPrompt: "Be truthful."},
		activation: &skill.ActivationSnapshot{SnapshotID: "snapshot-1", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "researcher", Skills: []skill.ActivatedSkill{{
			BindingID: "forum-account", BindingRevision: 3, SkillID: "forum", SkillVersion: "1", Name: "Forum", Actions: []capability.ModelAction{action},
		}}},
	}
	base := &AgentRun{ID: "run", Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"}, AssignedAgentID: "researcher", Context: map[string]interface{}{}}

	outreach := cloneAgentRun(base)
	outreach.Context = map[string]interface{}{}
	outreach.Context[OutreachInvocationContextKey] = map[string]interface{}{"threadId": "thread-1", "messageId": "message-1"}
	binding, err := ResolveCatalogTurnRunner(t.Context(), catalog, outreach, CatalogTurnResolverConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := binding.Runner.(*OutreachTurnRunner); !ok || len(binding.ModelActions) != 1 || binding.ModelActions[0].BindingID != "forum-account" ||
		len(binding.InputContextRefs) != 2 || binding.InputContextRefs[0] != "skill-snapshot:snapshot-1" || binding.InputContextRefs[1] != "run:"+OutreachInvocationContextKey {
		t.Fatalf("outreach binding = %#v", binding)
	}

	invocation := cloneAgentRun(base)
	invocation.Context = map[string]interface{}{}
	invocation.Context["capabilityInvocation"] = map[string]interface{}{"skillId": "forum", "skillVersion": "1", "action": "reply"}
	binding, err = ResolveCatalogTurnRunner(t.Context(), catalog, invocation, CatalogTurnResolverConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := binding.Runner.(*CapabilityInvocationTurnRunner); !ok || len(binding.ModelActions) != 1 {
		t.Fatalf("capability binding = %#v", binding)
	}

	_, err = ResolveCatalogTurnRunner(t.Context(), catalog, base, CatalogTurnResolverConfig{})
	if !errors.Is(err, ErrTurnHostUnavailable) {
		t.Fatalf("unhosted prompt work error = %v", err)
	}
}

func TestCatalogTurnResolverProjectsNativeDefaultWorkspace(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "11"}
	spec := workspace.DefaultSpec()
	spec.Policy.CredentialBindings = []string{"GITHUB_TOKEN"}
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn", ModelProvider: "test", Model: "model",
		NextRunStatus: AgentRunStatusCompleted, OutputSummary: "done", ContinuationCheckpoint: map[string]interface{}{},
	}}
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{
			ID: "coding-agent", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "coding-agent",
			ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive, DefaultWorkspaceID: spec.ID, Workspaces: []workspace.Spec{spec},
			Credentials: map[string]capability.CredentialReference{"GITHUB_TOKEN": {Kind: "vault", ID: "vault-credential"}},
		},
		definition: &kernelagent.AgentDefinition{ID: "coding-agent", Version: "1", Purpose: "Work in repositories"},
		activation: &skill.ActivationSnapshot{SnapshotID: "snapshot", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "coding-agent"},
	}
	run := &AgentRun{
		ID: "run", Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "coding-agent"},
		AssignedAgentID: "coding-agent", Goal: "Inspect the repository", Context: map[string]interface{}{},
	}
	binding, err := ResolveCatalogTurnRunner(t.Context(), catalog, run, CatalogTurnResolverConfig{Host: host})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := binding.Runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn"}}); err != nil {
		t.Fatal(err)
	}
	if host.request.Workspace == nil || host.request.Workspace.Workspace.ID != workspace.DefaultID || host.request.Workspace.Workspace.Policy.Filesystem != workspace.AccessReadWrite {
		t.Fatalf("workspace authority = %#v", host.request.Workspace)
	}
	if host.request.WorkspaceCredentials["GITHUB_TOKEN"].ID != "vault-credential" {
		t.Fatalf("Workspace credentials = %#v", host.request.WorkspaceCredentials)
	}
	encoded, err := MarshalHostedTurnModelInput(host.request)
	if err != nil || !strings.Contains(string(encoded), `"credentialBindings":["GITHUB_TOKEN"]`) || strings.Contains(string(encoded), `"storage"`) || strings.Contains(string(encoded), "vault-credential") || strings.Contains(string(encoded), "workspaceCredentials") {
		t.Fatalf("model input = %s, error=%v", encoded, err)
	}
}

func TestProjectActivatedSkillsPrefersCanonicalKernelManagementBinding(t *testing.T) {
	action := func(bindingID string, revision int64) capability.ModelAction {
		return capability.ModelAction{
			Name:      AgentManagementSkillID + "." + AgentActionAmendBehavior,
			BindingID: bindingID, BindingRevision: revision, DeploymentID: "agent-1",
			SkillID: AgentManagementSkillID, Version: AgentManagementSkillVersion, Action: AgentActionAmendBehavior,
		}
	}
	activation := &skill.ActivationSnapshot{
		SnapshotID: "snapshot-management", DeploymentID: "agent-1",
		Skills: []skill.ActivatedSkill{
			{
				BindingID: "workforce:agent-1:" + AgentManagementSkillID, BindingRevision: 1,
				SkillID: AgentManagementSkillID, SkillVersion: AgentManagementSkillVersion,
				Actions: []capability.ModelAction{action("workforce:agent-1:"+AgentManagementSkillID, 1)},
			},
			{
				BindingID: "bundled:agents", BindingRevision: 2,
				SkillID: AgentManagementSkillID, SkillVersion: AgentManagementSkillVersion,
				Actions: []capability.ModelAction{action("bundled:agents", 2)},
			},
		},
	}

	_, actions, prepared, refs := projectActivatedSkills(activation)
	if len(actions) != 1 || actions[0].BindingID != "bundled:agents" || actions[0].BindingRevision != 2 {
		t.Fatalf("management actions = %#v", actions)
	}
	if len(prepared) != 0 || len(refs) != 1 {
		t.Fatalf("prepared = %#v, refs = %#v", prepared, refs)
	}
}

func TestPinnedRunbookPlanPreventsDefinitionAndTriggerDrift(t *testing.T) {
	definition := &runbook.Definition{
		ID: "chat", Version: "2",
		Triggers: map[string]runbook.Trigger{"on-message": {
			Kind: runbook.TriggerEvent, EventType: externalConversationEventType, Entrypoint: "respond", ObjectiveID: "agent:agent:respond",
		}},
	}
	run := &AgentRun{
		Entrypoint: "respond",
		Plan: map[string]interface{}{"runbook": map[string]interface{}{
			"id": "chat", "version": "2", "trigger": "on-message",
		}},
	}
	if err := validatePinnedRunbookPlan(definition, run); err != nil {
		t.Fatal(err)
	}
	run.Plan["runbook"].(map[string]interface{})["version"] = "3"
	if err := validatePinnedRunbookPlan(definition, run); err == nil {
		t.Fatal("Runbook version drift was accepted")
	}
	run.Plan["runbook"].(map[string]interface{})["version"] = "2"
	run.Entrypoint = "other"
	if err := validatePinnedRunbookPlan(definition, run); err == nil {
		t.Fatal("Runbook trigger drift was accepted")
	}
}

func TestCatalogTurnResolverUsesDeploymentSpecificSkillHost(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "42"}
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{ID: "sre", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "sre", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive},
		definition: &kernelagent.AgentDefinition{ID: "sre", Version: "1", Purpose: "Operate safely", SystemPrompt: "Use bounded tools."},
		activation: &skill.ActivationSnapshot{SnapshotID: "snapshot-host", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "sre"},
	}
	wanted := skill.HostCapabilityState{OperatingSystem: "linux", Architecture: "arm64", Revision: "enterprise-host/v1"}
	var resolvedScope skill.ScopeReference
	var resolvedDeployment string
	run := &AgentRun{ID: "run", Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "sre"}, AssignedAgentID: "sre", Context: map[string]interface{}{"capabilityInvocation": map[string]interface{}{}}}
	_, err := ResolveCatalogTurnRunner(t.Context(), catalog, run, CatalogTurnResolverConfig{SkillHosts: SkillHostCapabilityResolverFunc(func(_ context.Context, gotScope skill.ScopeReference, deploymentID string) (*skill.HostCapabilityState, error) {
		resolvedScope, resolvedDeployment = gotScope, deploymentID
		return &wanted, nil
	})})
	if err == nil || !strings.Contains(err.Error(), "requires authorized actions") {
		t.Fatalf("expected deterministic runner validation after host resolution, got %v", err)
	}
	if resolvedScope != (skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}) || resolvedDeployment != "sre" || !reflect.DeepEqual(catalog.gotHost, wanted) {
		t.Fatalf("scope=%#v deployment=%q host=%#v", resolvedScope, resolvedDeployment, catalog.gotHost)
	}
}

func TestCatalogTurnResolverCarriesOpaqueDeploymentModelCredentialOnlyToHost(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "42"}
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusCompleted,
		ModelProvider: "openai-compatible", Model: "bound-model", OutputSummary: "done",
	}}
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{
			ID: "analyst", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "analyst", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive,
			Credentials: map[string]capability.CredentialReference{"MODEL_PROVIDER": {Kind: "model-provider", ID: "17"}},
		},
		definition: &kernelagent.AgentDefinition{ID: "analyst", Version: "1", Purpose: "Analyze", SystemPrompt: "Work carefully."},
		activation: &skill.ActivationSnapshot{SnapshotID: "snapshot", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "analyst"},
	}
	run := &AgentRun{ID: "run", Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "analyst"}, AssignedAgentID: "analyst", Goal: "Analyze", Context: map[string]interface{}{}}
	binding, err := ResolveCatalogTurnRunner(t.Context(), catalog, run, CatalogTurnResolverConfig{Host: host})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := binding.Runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn"}}); err != nil {
		t.Fatal(err)
	}
	if host.request.ModelCredential == nil || host.request.ModelCredential.Kind != "model-provider" || host.request.ModelCredential.ID != "17" {
		t.Fatalf("model credential = %#v", host.request.ModelCredential)
	}
	modelInput, _ := MarshalHostedTurnModelInput(host.request)
	if strings.Contains(string(modelInput), "17") || strings.Contains(strings.ToLower(string(modelInput)), "credential") {
		t.Fatalf("model input leaked credential reference: %s", modelInput)
	}
}

func TestCatalogTurnResolverHidesOriginatingRunbookFromDelegatedAgent(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "42"}
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusCompleted,
		ModelProvider: "test", Model: "test-model", OutputSummary: "done",
	}}
	definition := &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "daily-browser-task", Version: "1.2.3", Name: "Daily browser task",
		Interfaces: map[string]runbook.Interface{"dailyOccurrence": {
			Description:  "Run the daily browser task",
			InputSchema:  map[string]interface{}{"type": "object", "additionalProperties": false},
			OutputSchema: map[string]interface{}{"type": "object", "additionalProperties": true},
		}},
	}
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{
			ID: "browser", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "browser", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive,
			Credentials: map[string]capability.CredentialReference{"MODEL_PROVIDER": {Kind: "model-provider", ID: "model-binding"}},
		},
		definition: &kernelagent.AgentDefinition{ID: "browser", Version: "1", Purpose: "Browse", SystemPrompt: "Use bounded tools.", Runbook: definition},
		activation: &skill.ActivationSnapshot{SnapshotID: "snapshot", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "browser"},
	}
	run := &AgentRun{
		ID: "child", Scope: scope, Kind: RunKindAgentWork, Source: RunSourceRequest,
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "browser"}, AssignedAgentID: "browser", Goal: "Perform delegated work",
		Context: map[string]interface{}{"triggerInput": map[string]interface{}{
			"runbookDefinitionId": "daily-browser-task", "runbookDefinitionVersion": "1.2.3",
		}},
	}
	binding, err := ResolveCatalogTurnRunner(t.Context(), catalog, run, CatalogTurnResolverConfig{Host: host})
	if err != nil {
		t.Fatal(err)
	}
	if len(binding.RunbookOperations) != 0 {
		t.Fatalf("originating Runbook remained callable: %#v", binding.RunbookOperations)
	}
	if _, err := binding.Runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn"}}); err != nil {
		t.Fatal(err)
	}
	if len(host.request.RunbookOperations) != 0 {
		t.Fatalf("host received originating Runbook: %#v", host.request.RunbookOperations)
	}

	run.Context["triggerInput"].(map[string]interface{})["runbookDefinitionVersion"] = "different"
	binding, err = ResolveCatalogTurnRunner(t.Context(), catalog, run, CatalogTurnResolverConfig{Host: host})
	if err != nil || len(binding.RunbookOperations) != 1 {
		t.Fatalf("unrelated Runbook identity was filtered: binding=%#v error=%v", binding, err)
	}
}

func TestCatalogTurnResolverPrefersDefinitionOperationWithoutCallableActivation(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "42"}
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusCompleted,
		ModelProvider: "test", Model: "test-model", OutputSummary: "done",
	}}
	definition := &runbook.Definition{
		APIVersion: runbook.APIVersion, ID: "community-engagement", Version: "1", Name: "Community engagement",
		Interfaces: map[string]runbook.Interface{"engage-now": {
			Description:  "Run the reviewed engagement operation",
			InputSchema:  map[string]interface{}{"type": "object", "additionalProperties": false},
			OutputSchema: map[string]interface{}{"type": "object"},
		}},
	}
	start := capability.ModelAction{
		Name: "openseal.runbooks.start", SkillID: RunbookManagementSkillID, Version: RunbookManagementSkillVersion,
		Action: RunbookActionStart, BindingID: "bundled:runbooks", BindingRevision: 1,
	}
	replace := capability.ModelAction{
		Name: "openseal.runbooks.replace_schedule", SkillID: RunbookManagementSkillID, Version: RunbookManagementSkillVersion,
		Action: RunbookActionReplaceSchedule, BindingID: "bundled:runbooks", BindingRevision: 1,
	}
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{
			ID: "community-agent", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID},
			DefinitionID: "community-agent", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive,
		},
		definition: &kernelagent.AgentDefinition{
			ID: "community-agent", Version: "1", Purpose: "Engage", SystemPrompt: "Act carefully.", Runbook: definition,
		},
		activation: &skill.ActivationSnapshot{
			SnapshotID: "snapshot", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "community-agent",
			Skills: []skill.ActivatedSkill{{
				BindingID: "bundled:runbooks", BindingRevision: 1, SkillID: RunbookManagementSkillID,
				SkillVersion: RunbookManagementSkillVersion, Name: "Runbooks", Actions: []capability.ModelAction{start, replace},
			}},
		},
	}
	run := &AgentRun{
		ID: "conversation-turn", Scope: scope, Kind: RunKindAgentWork,
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "community-agent"}, AssignedAgentID: "community-agent",
		Goal: "Run it now", Context: map[string]interface{}{},
	}
	binding, err := ResolveCatalogTurnRunner(t.Context(), catalog, run, CatalogTurnResolverConfig{Host: host})
	if err != nil {
		t.Fatal(err)
	}
	if len(binding.RunbookOperations) != 1 || binding.RunbookOperations[0].Entrypoint != "engage-now" ||
		len(binding.ModelActions) != 1 || binding.ModelActions[0].Action != RunbookActionReplaceSchedule {
		t.Fatalf("conversation capabilities = %#v", binding)
	}
	if _, err := binding.Runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn"}}); err != nil {
		t.Fatal(err)
	}
	if len(host.request.Actions) != 1 || host.request.Actions[0].Action != RunbookActionReplaceSchedule ||
		len(host.request.RunbookOperations) != 1 {
		t.Fatalf("hosted capabilities = %#v", host.request)
	}
}

func TestCatalogTurnResolverProjectsActiveSameScopeDelegationCatalog(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "42"}
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusCompleted,
		ModelProvider: "test", Model: "test-model", OutputSummary: "done",
	}}
	current := &kernelagent.AgentDeployment{
		ID: "lead", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID},
		DefinitionID: "lead-definition", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive,
	}
	reviewer := &kernelagent.AgentDeployment{
		ID: "reviewer-7", Scope: current.Scope,
		DefinitionID: "reviewer-definition", ActiveVersion: "2", RolloutStatus: kernelagent.RolloutActive,
	}
	paused := &kernelagent.AgentDeployment{
		ID: "paused-agent", Scope: current.Scope,
		DefinitionID: "paused-definition", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutPaused,
	}
	catalog := &resolverCatalog{
		deployment:  current,
		deployments: []*kernelagent.AgentDeployment{paused, reviewer, current},
		definition:  &kernelagent.AgentDefinition{ID: "lead-definition", Version: "1", DisplayName: "Lead", Purpose: "Coordinate", SystemPrompt: "Coordinate safely."},
		definitions: map[string]*kernelagent.AgentDefinition{
			"lead-definition@1": {
				ID: "lead-definition", Version: "1", DisplayName: "Lead", Purpose: "Coordinate", SystemPrompt: "Coordinate safely.",
			},
			"reviewer-definition@2": {
				ID: "reviewer-definition", Version: "2", DisplayName: "Release Reviewer", Purpose: "Review releases", SystemPrompt: "Review safely.",
			},
		},
		activation: &skill.ActivationSnapshot{
			SnapshotID: "snapshot", Scope: current.Scope, DeploymentID: current.ID,
		},
		teamDeployment: &kernelteam.Deployment{
			ID: "release-team", Scope: current.Scope, DefinitionID: "release-team", ActiveVersion: "1",
			Status: kernelteam.DeploymentActive, Roster: []kernelteam.RosterAssignment{
				{ID: "lead", RoleID: "lead", AgentDeploymentID: current.ID},
				{ID: "reviewer", RoleID: "reviewer", AgentDeploymentID: reviewer.ID},
			},
		},
		teamDefinition: &kernelteam.Definition{
			ID: "release-team", Version: "1", Roles: []kernelteam.RoleSlot{
				{ID: "lead", DisplayName: "Lead", Purpose: "Coordinate releases"},
				{ID: "reviewer", DisplayName: "Reviewer", Purpose: "Review releases"},
			},
		},
		activations: map[string]*skill.ActivationSnapshot{
			current.ID:     {SnapshotID: "snapshot", Scope: current.Scope, DeploymentID: current.ID},
			"release-team": {SnapshotID: "team-snapshot", Scope: current.Scope, DeploymentID: "release-team"},
		},
	}
	run := &AgentRun{
		ID: "run", Scope: scope, Kind: RunKindAgentWork,
		Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "release-team"}, AssignedAgentID: current.ID,
		Goal: "Delegate a release review", Context: map[string]interface{}{},
	}
	binding, err := ResolveCatalogTurnRunner(t.Context(), catalog, run, CatalogTurnResolverConfig{Host: host})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := binding.Runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn"}}); err != nil {
		t.Fatal(err)
	}
	if len(host.request.EligibleAgents) != 1 ||
		host.request.EligibleAgents[0] != (HostedAgentTarget{ID: "reviewer-7", DisplayName: "Release Reviewer", Purpose: "Review releases"}) {
		t.Fatalf("eligible Agents = %#v", host.request.EligibleAgents)
	}
}

func TestCatalogTurnResolverDoesNotProjectTenantDirectoryIntoStandaloneAgent(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "42"}
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusCompleted,
		ModelProvider: "test", Model: "test-model", OutputSummary: "done",
	}}
	current := &kernelagent.AgentDeployment{ID: "standalone", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "standalone", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive}
	deployments := []*kernelagent.AgentDeployment{current}
	definitions := map[string]*kernelagent.AgentDefinition{
		"standalone@1": {ID: "standalone", Version: "1", DisplayName: "Standalone", Purpose: "Work independently", SystemPrompt: "Work safely."},
	}
	for index := 0; index < 200; index++ {
		id := fmt.Sprintf("tenant-agent-%03d", index)
		definitionID := id + "-definition"
		deployments = append(deployments, &kernelagent.AgentDeployment{ID: id, Scope: current.Scope, DefinitionID: definitionID, ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive})
		definitions[definitionID+"@1"] = &kernelagent.AgentDefinition{ID: definitionID, Version: "1", DisplayName: id, Purpose: strings.Repeat("unrelated tenant purpose ", 8)}
	}
	catalog := &resolverCatalog{
		deployment: current, deployments: deployments, definitions: definitions,
		activation: &skill.ActivationSnapshot{SnapshotID: "snapshot", Scope: current.Scope, DeploymentID: current.ID},
	}
	run := &AgentRun{ID: "run", Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: current.ID}, AssignedAgentID: current.ID, Goal: "Answer concisely", Context: map[string]interface{}{}}
	binding, err := ResolveCatalogTurnRunner(t.Context(), catalog, run, CatalogTurnResolverConfig{Host: host})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = binding.Runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn"}}); err != nil {
		t.Fatal(err)
	}
	if len(host.request.EligibleAgents) != 0 {
		t.Fatalf("standalone Agent received tenant directory: %#v", host.request.EligibleAgents)
	}
	estimate, err := EstimateHostedTurnInputTokens(host.request)
	if err != nil || estimate >= HostedTurnMinimumChildInputTokens {
		t.Fatalf("minimal standalone estimate = %d, %v", estimate, err)
	}
}

func TestCatalogTurnResolverIsolatesAgentRequestDecisionFromSkillsAndRunbooks(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "42"}
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusCompleted,
		ModelProvider: "openai-compatible", Model: "decision-model", OutputSummary: "Request accepted",
		RunOutput: map[string]interface{}{AgentRequestDecisionOutputKey: map[string]interface{}{
			"decision": string(AgentRequestDecisionAccept), "message": "The request is clear and within my role.",
		}},
	}}
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{
			ID: "reviewer", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID},
			DefinitionID: "reviewer", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive,
			Credentials: map[string]capability.CredentialReference{"MODEL_PROVIDER": {Kind: "model-provider", ID: "model-binding"}},
		},
		definition: &kernelagent.AgentDefinition{
			ID: "reviewer", Version: "1", Purpose: "Review launches", SystemPrompt: "Be precise.",
			Runbook: &runbook.Definition{APIVersion: runbook.APIVersion, ID: "normal-work", Version: "1"},
		},
	}
	run := &AgentRun{
		ID: "decision", Scope: scope, Kind: RunKindAgentWork,
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "reviewer"}, AssignedAgentID: "reviewer",
		Goal: "Evaluate incoming work", Source: RunSourceRequestDecision,
		Context: map[string]interface{}{AgentRequestInboxContextKey: map[string]interface{}{
			"requestId": "request", "requestRevision": int64(1),
		}},
	}
	binding, err := ResolveCatalogTurnRunner(t.Context(), catalog, run, CatalogTurnResolverConfig{Host: host})
	if err != nil {
		t.Fatal(err)
	}
	decisionRunner, ok := binding.Runner.(*agentRequestDecisionTurnRunner)
	if !ok {
		t.Fatalf("decision runner = %#v", binding.Runner)
	}
	hosted, hostedOK := decisionRunner.inner.(*HostedTurnRunner)
	if !hostedOK || len(binding.ModelActions) != 0 || len(hosted.config.Actions) != 0 || len(hosted.config.SkillPrompts) != 0 ||
		len(catalog.activationDeployments) != 0 || !containsString(binding.InputContextRefs, "run:"+AgentRequestInboxContextKey) ||
		!containsString(hosted.config.SystemInstructions, agentRequestDecisionSystemInstruction) {
		t.Fatalf("decision binding=%#v hosted=%#v activations=%v", binding, hosted, catalog.activationDeployments)
	}
	outcome, err := binding.Runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn"}})
	if err != nil || outcome.NextRunStatus != AgentRunStatusCompleted ||
		host.request.ModelCredential == nil || host.request.ModelCredential.ID != "model-binding" ||
		!containsString(host.request.SystemInstructions, agentRequestDecisionSystemInstruction) {
		t.Fatalf("decision outcome=%#v request=%#v error=%v", outcome, host.request, err)
	}
}

func TestCatalogTurnResolverSeparatesAcceptedRequestExecutionFromIntake(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "42"}
	action := capability.ModelAction{
		Name: "browser.snapshot", BindingID: "browser", BindingRevision: 1,
		SkillID: "skill-browser", Version: "1", Action: "snapshot", SideEffect: capability.SideEffectRead,
	}
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusRunning,
		ModelProvider: "test", Model: "test-model", OutputSummary: "Inspect the page",
		ProposedAction: &TurnAction{
			Type: "skill_action", Capability: action.Name, BindingID: action.BindingID,
			BindingRevision: action.BindingRevision, Summary: "Inspect the page",
		},
		ContinuationCheckpoint: map[string]interface{}{},
	}}
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{
			ID: "browser-agent", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID},
			DefinitionID: "browser-agent", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive,
		},
		definition: &kernelagent.AgentDefinition{ID: "browser-agent", Version: "1", Purpose: "Browse safely", SystemPrompt: "Use evidence."},
		activation: &skill.ActivationSnapshot{
			SnapshotID: "snapshot", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "browser-agent",
			Skills: []skill.ActivatedSkill{{BindingID: "browser", BindingRevision: 1, SkillID: "skill-browser", SkillVersion: "1", Actions: []capability.ModelAction{action}}},
		},
	}
	run := &AgentRun{
		ID: "execution", Scope: scope, Kind: RunKindAgentWork, Source: RunSourceRequest,
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "browser-agent"}, AssignedAgentID: "browser-agent",
		Goal: "Perform the accepted request", Context: map[string]interface{}{"collaboration": map[string]interface{}{"requestId": "request"}},
	}
	binding, err := ResolveCatalogTurnRunner(t.Context(), catalog, run, CatalogTurnResolverConfig{Host: host})
	if err != nil {
		t.Fatal(err)
	}
	executionRunner, ok := binding.Runner.(*acceptedAgentRequestExecutionTurnRunner)
	if !ok || !containsString(executionRunner.inner.(*HostedTurnRunner).config.SystemInstructions, acceptedAgentRequestExecutionSystemInstruction) {
		t.Fatalf("accepted execution binding = %#v", binding)
	}
	outcome, err := binding.Runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn"}})
	if err != nil || outcome == nil || len(outcome.ProposedActions) != 1 || outcome.ProposedActions[0].Capability != action.Name {
		t.Fatalf("accepted execution outcome=%#v request=%#v error=%v", outcome, host.request, err)
	}
	if containsString(host.request.SystemInstructions, agentRequestDecisionSystemInstruction) {
		t.Fatalf("intake instruction leaked into accepted execution: %#v", host.request.SystemInstructions)
	}

	executionRunner.inner = TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
		return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{
			AgentRequestDecisionOutputKey: map[string]interface{}{"decision": "accept"},
		}}, nil
	})
	recovered, err := executionRunner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn-2"}})
	if err != nil || recovered == nil || recovered.NextRunStatus != AgentRunStatusRunning ||
		recovered.RunOutput != nil || acceptedAgentRequestExecutionRecoveryAttempt(recovered.ContinuationCheckpoint) != 1 {
		t.Fatalf("accepted execution recovery=%#v error=%v", recovered, err)
	}
}

func TestCatalogTurnResolverProjectsTeamOwnedActionsWithoutLeakingThemToAgentRuns(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "42"}
	teamAction := capability.ModelAction{
		Name: "openseal.teams.update_role", BindingID: "teams", BindingRevision: 2,
		SkillID: "openseal.teams", Version: "1.0.0", Action: "update_role", Risk: capability.RiskLevelWrite,
	}
	agentAction := teamAction
	agentAction.BindingID = "agent-teams"
	agentAction.BindingRevision = 1
	host := &recordingTurnHost{response: &HostedTurnResponse{
		APIVersion: HostedTurnAPIVersion, InvocationID: "turn", NextRunStatus: AgentRunStatusRunning,
		ModelProvider: "test", Model: "test-model", OutputSummary: "Propose the role change",
		ProposedAction: &TurnAction{
			Type: "skill_action", Capability: teamAction.Name, BindingID: teamAction.BindingID,
			BindingRevision: teamAction.BindingRevision, Summary: "Enable the reviewer", InputRef: "/roleChange",
		},
		ContinuationCheckpoint: map[string]interface{}{"roleChange": map[string]interface{}{
			"roleId": "reviewer", "expectedDeploymentRevision": 1, "channelParticipation": "active",
		}},
	}}
	catalog := &resolverCatalog{
		deployment: &kernelagent.AgentDeployment{ID: "reviewer-agent", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "reviewer", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive},
		definition: &kernelagent.AgentDefinition{ID: "reviewer", Version: "1", Purpose: "Review", SystemPrompt: "Review carefully.", Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelWrite, AllowedSkillIDs: []string{"openseal.teams"}}},
		teamDeployment: &kernelteam.Deployment{
			ID: "review-team", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: "review-team", ActiveVersion: "1",
			Status: kernelteam.DeploymentActive, Revision: 1, Roster: []kernelteam.RosterAssignment{{ID: "reviewer", RoleID: "reviewer", AgentDeploymentID: "reviewer-agent"}},
		},
		teamDefinition: &kernelteam.Definition{
			ID: "review-team", Version: "1", Approvals: kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelWrite},
			Roles: []kernelteam.RoleSlot{{ID: "reviewer", SkillGrants: []kernelteam.RoleSkillGrant{{SkillID: "openseal.teams", SkillVersion: "1.0.0", AllowedActions: []string{"update_role"}, MaximumRisk: capability.RiskLevelWrite}}}},
		},
		activations: map[string]*skill.ActivationSnapshot{
			"reviewer-agent": {SnapshotID: "agent-snapshot", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "reviewer-agent", Skills: []skill.ActivatedSkill{{
				BindingID: "agent-teams", BindingRevision: 1, SkillID: "openseal.teams", SkillVersion: "1.0.0", Name: "Teams", Actions: []capability.ModelAction{agentAction},
			}}},
			"review-team": {SnapshotID: "team-snapshot", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "review-team", Skills: []skill.ActivatedSkill{{
				BindingID: "teams", BindingRevision: 2, SkillID: "openseal.teams", SkillVersion: "1.0.0", Name: "Teams", Actions: []capability.ModelAction{teamAction},
			}}},
		},
	}
	teamRun := &AgentRun{ID: "run", Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "review-team"}, AssignedAgentID: "reviewer-agent", Goal: "Enable the reviewer", Context: map[string]interface{}{}, Checkpoint: map[string]interface{}{}}
	binding, err := ResolveCatalogTurnRunner(t.Context(), catalog, teamRun, CatalogTurnResolverConfig{Host: host})
	if err != nil {
		t.Fatal(err)
	}
	if binding.DeploymentID != "reviewer-agent" || binding.ActionDeploymentID != "reviewer-agent" || len(binding.ModelActions) != 1 || binding.ModelActions[0].BindingID != "teams" || binding.ModelActions[0].DeploymentID != "review-team" || !reflect.DeepEqual(catalog.activationDeployments, []string{"reviewer-agent", "review-team"}) {
		t.Fatalf("Team binding = %#v activations=%v", binding, catalog.activationDeployments)
	}
	hosted, ok := binding.Runner.(*HostedTurnRunner)
	if !ok || hosted.config.AgentID != "reviewer-agent" || hosted.config.ActionDeploymentID != "reviewer-agent" {
		t.Fatalf("hosted Team identity = %#v", binding.Runner)
	}
	outcome, err := binding.Runner.RunTurn(t.Context(), TurnExecutionContext{Run: teamRun, Turn: &AgentTurn{ID: "turn"}})
	if err != nil || len(outcome.ProposedActions) != 1 || host.request.AgentID != "reviewer-agent" {
		t.Fatalf("Team hosted turn = %#v request=%#v err=%v", outcome, host.request, err)
	}

	catalog.activationDeployments = nil
	agentRun := cloneAgentRun(teamRun)
	agentRun.Owner = ObjectiveOwner{Type: OwnerTypeAgent, ID: "reviewer-agent"}
	agentBinding, err := ResolveCatalogTurnRunner(t.Context(), catalog, agentRun, CatalogTurnResolverConfig{Host: host})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(catalog.activationDeployments, []string{"reviewer-agent"}) || agentBinding.ActionDeploymentID != "reviewer-agent" || agentBinding.ModelActions[0].BindingID != "agent-teams" {
		t.Fatalf("Agent run binding=%#v activations=%v", agentBinding, catalog.activationDeployments)
	}
}
