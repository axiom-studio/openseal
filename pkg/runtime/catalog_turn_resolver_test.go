package runtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

type resolverCatalog struct {
	deployment            *kernelagent.AgentDeployment
	definition            *kernelagent.AgentDefinition
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

func (c *resolverCatalog) GetAgentDefinition(context.Context, string, string) (*kernelagent.AgentDefinition, error) {
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
			Credentials: map[string]capability.CredentialReference{"MODEL_PROVIDER": {Kind: "vault", ID: "17"}},
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
	if host.request.ModelCredential == nil || host.request.ModelCredential.Kind != "vault" || host.request.ModelCredential.ID != "17" {
		t.Fatalf("model credential = %#v", host.request.ModelCredential)
	}
	modelInput, _ := MarshalHostedTurnModelInput(host.request)
	if strings.Contains(string(modelInput), "17") || strings.Contains(strings.ToLower(string(modelInput)), "credential") {
		t.Fatalf("model input leaked credential reference: %s", modelInput)
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
