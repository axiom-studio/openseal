package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func TestGovernedTeamMemberActionUpdatesTheRosterAndReplays(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "tenant-a"}
	teams, agentDeploymentID := teamMemberActionRegistry(t, ctx, scope)
	owner := models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "accessibility-team"}
	portfolio := portfolio.NewService(store)
	run, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Kind: models.RunKindConversation, Owner: owner, AssignedAgentID: agentDeploymentID,
		Goal: "Let the UX Reviewer speak", Source: models.RunSourceChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{
		Scope: scope, WorkerID: "conversation-worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	catalog := teamMemberActionCatalog(t, ctx, scope, owner.ID)
	validator, err := collaboration.NewTeamMemberActionValidator(teams)
	if err != nil {
		t.Fatal(err)
	}
	policy := action.PolicyEvaluatorFunc(func(context.Context, action.PolicyInput) (action.PolicyDecision, error) {
		return action.PolicyDecision{
			Disposition: action.DispositionRequireApproval, Reason: "Team behavior changes require review",
			EligibleApprovers: []models.ApprovalPrincipal{{Type: "user", ID: "operator"}}, ApprovalTTL: time.Hour,
		}, nil
	})
	coordinator := action.NewCoordinator(store, store, catalog, policy, []action.ProposalValidator{validator})
	arguments := map[string]interface{}{
		"memberId": "ux-reviewer", "expectedDeploymentRevision": 1, "channelParticipation": "active",
	}
	proposal, err := coordinator.Propose(ctx, actiontest.LoopProposeRequest(action.ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "conversation-worker", DeploymentID: owner.ID,
		SkillID: models.TeamManagementSkillID, SkillVersion: models.TeamManagementSkillVersion, Action: models.TeamActionUpdateMember,
		Arguments: arguments, IdempotencyKey: "message-42:enable-ux-reviewer", Summary: "Let the UX Reviewer speak",
	}, "enable-ux-reviewer"))
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Approval == nil || proposal.Call.Status != models.ActionCallStatusWaitingApproval {
		t.Fatalf("proposal lifecycle = %#v", proposal)
	}
	preview := proposal.Approval.ProposedAction
	changes, _ := preview["changes"].(map[string]interface{})
	if preview["resourceType"] != "team" || preview["operation"] != models.TeamActionUpdateMember ||
		preview["memberName"] != "UX Reviewer" || fmt.Sprint(changes["channelParticipation"]) != string(kernelteam.ChannelActive) {
		t.Fatalf("typed Team diff = %#v", preview)
	}
	resolved, err := action.NewApprovalCoordinator(store, store, action.EligibleApprovalAuthorizer{}).Resolve(ctx, action.ResolveApprovalRequest{
		Scope: scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
		DecisionID: "decision-42", Decision: models.ApprovalDecisionApprove, Principal: models.ApprovalPrincipal{Type: "user", ID: "operator"}, Reason: "Reviewed",
	})
	if err != nil || resolved.Call.Status != models.ActionCallStatusReady {
		t.Fatalf("resolve = %#v, %v", resolved, err)
	}
	dispatcher, err := collaboration.NewTeamMemberActionDispatcher(store, teams, nil)
	if err != nil {
		t.Fatal(err)
	}
	worker := action.NewWorker(store, catalog, nil, dispatcher)
	executed, err := worker.ExecuteOwnLoopCall(ctx, scope, proposal.Call.ID, "conversation-worker")
	if err != nil {
		t.Fatal(err)
	}
	if executed.Call.Status != models.ActionCallStatusSucceeded || executed.Call.Output["resourceType"] != "team" || executed.Call.Output["replayed"] != false {
		t.Fatalf("execution = %#v", executed)
	}
	deployment, err := teams.GetDeployment(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, owner.ID)
	if err != nil || deployment.Revision != 2 || deployment.ActiveVersion != "1.0.0" ||
		deployment.Roster[0].ChannelParticipation != kernelteam.ChannelActive || deployment.Roster[0].Purpose != "Review UX" {
		t.Fatalf("deployment = %#v, %v", deployment, err)
	}
	// The change is a roster update the approver made: no definition version, no amendment.
	reference := capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	activations, err := teams.ListActivations(ctx, reference, owner.ID)
	if err != nil || len(activations) != 2 || activations[1].ActorType != "user" || activations[1].ActorID != "operator" || activations[1].DeploymentRevision != 2 {
		t.Fatalf("activations = %#v, %v", activations, err)
	}
	if versions, err := teams.ListDefinitionVersions(ctx, deployment.DefinitionID); err != nil || len(versions) != 1 {
		t.Fatalf("definition versions = %d, %v", len(versions), err)
	}
	if amendments, err := teams.ListAmendments(ctx, reference, owner.ID); err != nil || len(amendments) != 0 {
		t.Fatalf("amendments = %#v, %v", amendments, err)
	}
	bound, err := catalog.Resolve(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, owner.ID, models.TeamManagementSkillID, models.TeamManagementSkillVersion, models.TeamActionUpdateMember)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := dispatcher.DispatchAction(ctx, agentrun.ActionDispatchInput{Call: executed.Call, Bound: bound, Arguments: executed.Call.Arguments})
	if err != nil || replayed["replayed"] != true {
		t.Fatalf("replay = %#v, %v", replayed, err)
	}
	latest, _ := teams.GetDeployment(ctx, reference, owner.ID)
	if latest.Revision != 2 {
		t.Fatalf("replay changed Team revision to %d", latest.Revision)
	}
	// Another call's retry at the same stale revision is not this call's replay.
	other := *executed.Call
	other.ID = "another-call"
	if _, err := dispatcher.DispatchAction(ctx, agentrun.ActionDispatchInput{Call: &other, Bound: bound, Arguments: executed.Call.Arguments}); !errors.Is(err, models.ErrRevisionConflict) {
		t.Fatalf("foreign replay error = %v", err)
	}
}

func TestTeamMemberActionRejectsStaleForeignAndPolicyForbiddenChangesBeforeApproval(t *testing.T) {
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "tenant-a"}
	teams, agentDeploymentID := teamMemberActionRegistry(t, ctx, scope)
	validator, _ := collaboration.NewTeamMemberActionValidator(teams)
	bound := &skill.BoundAction{Definition: collaboration.TeamManagementSkill(), Action: collaboration.TeamManagementSkill().Actions[models.TeamActionUpdateMember]}

	for name, testCase := range map[string]struct {
		run       *models.AgentRun
		arguments map[string]interface{}
	}{
		"Agent owner": {
			run:       &models.AgentRun{Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: agentDeploymentID}},
			arguments: map[string]interface{}{"memberId": "ux-reviewer", "expectedDeploymentRevision": 1, "channelParticipation": "active"},
		},
		"stale revision": {
			run:       &models.AgentRun{Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "accessibility-team"}},
			arguments: map[string]interface{}{"memberId": "ux-reviewer", "expectedDeploymentRevision": 9, "channelParticipation": "active"},
		},
		"missing member": {
			run:       &models.AgentRun{Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "accessibility-team"}},
			arguments: map[string]interface{}{"memberId": "intruder", "expectedDeploymentRevision": 1, "channelParticipation": "active"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validator.ValidateActionProposal(ctx, agentrun.ActionProposalValidationInput{Run: testCase.run, Bound: bound, Arguments: testCase.arguments}); err == nil {
				t.Fatal("invalid Team member action reached approval")
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
	_, err = validator.ValidateActionProposal(ctx, agentrun.ActionProposalValidationInput{
		Run: &models.AgentRun{Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "accessibility-team"}}, Bound: bound,
		Arguments: map[string]interface{}{"memberId": "ux-reviewer", "expectedDeploymentRevision": 2, "channelParticipation": "active"},
	})
	if err == nil || errors.Is(err, models.ErrRevisionConflict) {
		t.Fatalf("policy-forbidden action error = %v", err)
	}
}

func teamMemberActionRegistry(t *testing.T, ctx context.Context, scope models.Scope) (*kernelteam.Registry, string) {
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
		Approvals: kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelWrite},
		Amendments: workforce.AmendmentPolicy{
			AgentMayPropose: true, AllowedFields: []string{models.TeamMemberAmendmentField}, RequiresApproval: true,
			ApproverPrincipals: []string{"user:operator"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := teams.CreateDeployment(ctx, &kernelteam.Deployment{
		ID: "accessibility-team", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID},
		DefinitionID: definition.ID, ActiveVersion: definition.Version, Status: kernelteam.DeploymentActive,
		Roster: []kernelteam.RosterAssignment{{
			ID: "ux-reviewer", AgentDeploymentID: agentDeployment.ID, DisplayName: "UX Reviewer", Purpose: "Review UX",
			ChannelParticipation: kernelteam.ChannelObserveOnly,
		}},
	}, "user", "operator", "initial Team"); err != nil {
		t.Fatal(err)
	}
	return teams, agentDeployment.ID
}

func teamMemberActionCatalog(t *testing.T, ctx context.Context, scope models.Scope, deploymentID string) *skill.Catalog {
	t.Helper()
	catalog := skill.NewCatalog()
	if err := catalog.Register(ctx, collaboration.TeamManagementSkill()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "teams", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: deploymentID,
		SkillID: models.TeamManagementSkillID, SkillVersion: models.TeamManagementSkillVersion,
		AllowedActions: []string{models.TeamActionUpdateMember}, MaximumRisk: skill.RiskLevelWrite,
	}); err != nil {
		t.Fatal(err)
	}
	return catalog
}
