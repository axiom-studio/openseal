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

func TestPinnedRunbookPlanPreventsDefinitionAndTriggerDrift(t *testing.T) {
	definition := &runbook.Definition{
		ID: "chat", Version: "2",
		Triggers: map[string]runbook.Trigger{"on-message": {
			Kind: runbook.TriggerEvent, EventType: externalConversationEventType, Entrypoint: "respond",
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
				{ID: "lead", AgentDeploymentID: current.ID},
				{ID: "reviewer", AgentDeploymentID: reviewer.ID},
			},
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
		ProposedActions: []TurnAction{{
			Type: "skill_action", Capability: teamAction.Name, BindingID: teamAction.BindingID,
			BindingRevision: teamAction.BindingRevision, Summary: "Enable the reviewer", InputRef: "/roleChange",
		}},
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
