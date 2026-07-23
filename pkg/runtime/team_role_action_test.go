package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func TestGovernedTeamRoleActionActivatesImmutableDefinitionAndReplays(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "tenant-a"}
	teams, agentDeploymentID := teamRoleActionRegistry(t, ctx, scope)
	owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "accessibility-team"}
	portfolio := NewPortfolioService(store)
	run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Kind: RunKindConversation, Owner: owner, AssignedAgentID: agentDeploymentID,
		Goal: "Let the UX Reviewer speak", Source: RunSourceChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{
		Scope: scope, WorkerID: "conversation-worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	catalog := teamRoleActionCatalog(t, ctx, scope, owner.ID)
	validator, err := NewTeamRoleActionValidator(teams)
	if err != nil {
		t.Fatal(err)
	}
	policy := ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{
			Disposition: ActionDispositionRequireApproval, Reason: "Team behavior changes require review",
			EligibleApprovers: []ApprovalPrincipal{{Type: "user", ID: "operator"}}, ApprovalTTL: time.Hour,
		}, nil
	})
	coordinator := NewActionCoordinator(store, store, catalog, policy, validator)
	arguments := map[string]interface{}{
		"roleId": "ux-reviewer", "expectedDeploymentRevision": 1, "channelParticipation": "active",
	}
	proposal, err := coordinator.Propose(ctx, ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "conversation-worker", DeploymentID: owner.ID,
		SkillID: TeamManagementSkillID, SkillVersion: TeamManagementSkillVersion, Action: TeamActionUpdateRole,
		Arguments: arguments, IdempotencyKey: "message-42:enable-ux-reviewer", Summary: "Let the UX Reviewer speak",
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Approval == nil || proposal.Call.Status != ActionCallStatusWaitingApproval {
		t.Fatalf("proposal lifecycle = %#v", proposal)
	}
	preview := proposal.Approval.ProposedAction
	changes, _ := preview["changes"].(map[string]interface{})
	if preview["resourceType"] != "team" || preview["operation"] != TeamActionUpdateRole ||
		preview["roleName"] != "UX Reviewer" || fmt.Sprint(changes["channelParticipation"]) != string(kernelteam.RoleChannelActive) {
		t.Fatalf("typed Team diff = %#v", preview)
	}
	resolved, err := NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{}).Resolve(ctx, ResolveApprovalRequest{
		Scope: scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
		DecisionID: "decision-42", Approve: true, Principal: ApprovalPrincipal{Type: "user", ID: "operator"}, Reason: "Reviewed",
	})
	if err != nil || resolved.Call.Status != ActionCallStatusReady {
		t.Fatalf("resolve = %#v, %v", resolved, err)
	}
	dispatcher, err := NewTeamRoleActionDispatcher(store, teams, nil)
	if err != nil {
		t.Fatal(err)
	}
	worker := NewActionWorker(store, catalog, nil, dispatcher)
	executed, err := worker.RunOnce(ctx, scope, "action-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if executed.Call.Status != ActionCallStatusSucceeded || executed.Call.Output["resourceType"] != "team" || executed.Call.Output["replayed"] != false {
		t.Fatalf("execution = %#v", executed)
	}
	deployment, err := teams.GetDeployment(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, owner.ID)
	if err != nil || deployment.Revision != 2 || deployment.ActiveVersion == "1.0.0" {
		t.Fatalf("deployment = %#v, %v", deployment, err)
	}
	definition, err := teams.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil || definition.Roles[0].ChannelParticipation != kernelteam.RoleChannelActive || definition.Provenance.DerivedFrom == "" {
		t.Fatalf("definition = %#v, %v", definition, err)
	}
	amendments, err := teams.ListAmendments(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, owner.ID)
	if err != nil || len(amendments) != 1 || amendments[0].Status != kernelteam.AmendmentActivated || amendments[0].Decision == nil || amendments[0].Decision.ActorID != "operator" {
		t.Fatalf("amendments = %#v, %v", amendments, err)
	}
	bound, err := catalog.Resolve(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, owner.ID, TeamManagementSkillID, TeamManagementSkillVersion, TeamActionUpdateRole)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := dispatcher.DispatchAction(ctx, ActionDispatchInput{Call: executed.Call, Bound: bound, Arguments: executed.Call.Arguments})
	if err != nil || replayed["replayed"] != true {
		t.Fatalf("replay = %#v, %v", replayed, err)
	}
	latest, _ := teams.GetDeployment(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, owner.ID)
	if latest.Revision != 2 {
		t.Fatalf("replay changed Team revision to %d", latest.Revision)
	}
}

func TestTeamRoleActionRejectsStaleForeignAndPolicyForbiddenChangesBeforeApproval(t *testing.T) {
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "tenant-a"}
	teams, agentDeploymentID := teamRoleActionRegistry(t, ctx, scope)
	validator, _ := NewTeamRoleActionValidator(teams)
	bound := &skill.BoundAction{Definition: TeamManagementSkill(), Action: TeamManagementSkill().Actions[TeamActionUpdateRole]}

	for name, testCase := range map[string]struct {
		run       *AgentRun
		arguments map[string]interface{}
	}{
		"Agent owner": {
			run:       &AgentRun{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: agentDeploymentID}},
			arguments: map[string]interface{}{"roleId": "ux-reviewer", "expectedDeploymentRevision": 1, "channelParticipation": "active"},
		},
		"stale revision": {
			run:       &AgentRun{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "accessibility-team"}},
			arguments: map[string]interface{}{"roleId": "ux-reviewer", "expectedDeploymentRevision": 9, "channelParticipation": "active"},
		},
		"missing role": {
			run:       &AgentRun{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "accessibility-team"}},
			arguments: map[string]interface{}{"roleId": "intruder", "expectedDeploymentRevision": 1, "channelParticipation": "active"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validator.ValidateActionProposal(ctx, ActionProposalValidationInput{Run: testCase.run, Bound: bound, Arguments: testCase.arguments}); err == nil {
				t.Fatal("invalid Team role action reached approval")
			}
		})
	}

	definition, err := teams.GetDefinition(ctx, "accessibility-team-definition", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := *definition
	forbidden.Version = "policy-locked"
	forbidden.Amendments.AgentMayPropose = false
	if _, err := teams.RegisterDefinition(ctx, &forbidden); err != nil {
		t.Fatal(err)
	}
	if _, _, err := teams.ActivateDefinition(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "accessibility-team", forbidden.Version, 1, "user", "operator", "lock policy"); err != nil {
		t.Fatal(err)
	}
	_, err = validator.ValidateActionProposal(ctx, ActionProposalValidationInput{
		Run: &AgentRun{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "accessibility-team"}}, Bound: bound,
		Arguments: map[string]interface{}{"roleId": "ux-reviewer", "expectedDeploymentRevision": 2, "channelParticipation": "active"},
	})
	if err == nil || errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("policy-forbidden action error = %v", err)
	}
}

func teamRoleActionRegistry(t *testing.T, ctx context.Context, scope Scope) (*kernelteam.Registry, string) {
	t.Helper()
	agents := kernelagent.NewRegistry()
	agentDefinition, err := agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "ux-reviewer-definition", Version: "1.0.0", DisplayName: "UX Reviewer", Purpose: "Review UX", SystemPrompt: "Review UX.",
		Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelWrite, MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	agentDeployment, _, err := agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "ux-reviewer-agent", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID},
		DefinitionID: agentDefinition.ID, ActiveVersion: agentDefinition.Version, RolloutStatus: kernelagent.RolloutActive,
		Environment: "local", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "operator", "Team roster")
	if err != nil {
		t.Fatal(err)
	}
	teams := kernelteam.NewRegistry(agents)
	definition, err := teams.RegisterDefinition(ctx, &kernelteam.Definition{
		ID: "accessibility-team-definition", Version: "1.0.0", DisplayName: "Accessibility Team", Purpose: "Review accessibility",
		Roles: []kernelteam.RoleSlot{{
			ID: "ux-reviewer", DisplayName: "UX Reviewer", Purpose: "Review UX", MinimumMembers: 1, MaximumMembers: 1,
			RequiredDefinitionIDs: []string{agentDefinition.ID}, ChannelParticipation: kernelteam.RoleChannelObserveOnly,
		}},
		Coordination: kernelteam.CoordinationPolicy{Mode: kernelteam.CoordinationPeer},
		Approvals:    kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelWrite},
		Amendments: workforce.AmendmentPolicy{
			AgentMayPropose: true, AllowedFields: []string{"roles"}, RequiresApproval: true,
			ApproverPrincipals: []string{"user:operator"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := teams.CreateDeployment(ctx, &kernelteam.Deployment{
		ID: "accessibility-team", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID},
		DefinitionID: definition.ID, ActiveVersion: definition.Version, Status: kernelteam.DeploymentActive,
		Roster: []kernelteam.RosterAssignment{{ID: "ux-reviewer", RoleID: "ux-reviewer", AgentDeploymentID: agentDeployment.ID}},
	}, "user", "operator", "initial Team"); err != nil {
		t.Fatal(err)
	}
	return teams, agentDeployment.ID
}

func teamRoleActionCatalog(t *testing.T, ctx context.Context, scope Scope, deploymentID string) *skill.Catalog {
	t.Helper()
	catalog := skill.NewCatalog()
	if err := catalog.Register(ctx, TeamManagementSkill()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "teams", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: deploymentID,
		SkillID: TeamManagementSkillID, SkillVersion: TeamManagementSkillVersion,
		AllowedActions: []string{TeamActionUpdateRole}, MaximumRisk: skill.RiskLevelWrite,
	}); err != nil {
		t.Fatal(err)
	}
	return catalog
}
