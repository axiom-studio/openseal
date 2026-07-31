package runtime

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func TestGovernedAgentBehaviorActionActivatesImmutableDefinitionAndReplays(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "tenant-a"}
	agents := agentBehaviorActionRegistry(t, ctx, scope, nil)
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"},
		AssignedAgentID: "researcher", Goal: "Become more evidence focused", Source: RunSourceChat,
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
	catalog := agentBehaviorActionCatalog(t, ctx, scope, "researcher")
	validator, err := NewAgentBehaviorActionValidator(agents)
	if err != nil {
		t.Fatal(err)
	}
	policy := ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{
			Disposition: ActionDispositionRequireApproval, Reason: "Agent behavior changes require review",
			EligibleApprovers: []ApprovalPrincipal{{Type: "user", ID: "operator"}}, ApprovalTTL: time.Hour,
		}, nil
	})
	coordinator := NewActionCoordinator(store, store, catalog, policy, validator)
	proposal, err := coordinator.Propose(ctx, ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "conversation-worker", DeploymentID: "researcher",
		SkillID: AgentManagementSkillID, SkillVersion: AgentManagementSkillVersion, Action: AgentActionAmendBehavior,
		Arguments: map[string]interface{}{
			"displayName":         "Evidence Researcher",
			"personality":         "Calm, curious, and evidence focused",
			"operatingPrinciples": []interface{}{"Separate facts from inference.", "Cite durable evidence."},
			"rationale":           "Improve research quality",
		},
		IdempotencyKey: "message-42:improve-behavior", Summary: "Improve the researcher's evidence discipline",
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Approval == nil || proposal.Call.Status != ActionCallStatusWaitingApproval {
		t.Fatalf("proposal lifecycle = %#v", proposal)
	}
	if _, modelSupplied := proposal.Call.Arguments["expectedDeploymentRevision"]; !modelSupplied ||
		fmt.Sprint(proposal.Call.Arguments["expectedDeploymentRevision"]) != "1" {
		t.Fatalf("kernel revision was not resolved: %#v", proposal.Call.Arguments)
	}
	preview := proposal.Approval.ProposedAction
	changes, _ := preview["changes"].(map[string]interface{})
	if preview["resourceType"] != agentBehaviorResourceType || preview["operation"] != AgentActionAmendBehavior ||
		preview["deploymentId"] != "researcher" || changes["personality"] != "Calm, curious, and evidence focused" {
		t.Fatalf("typed Agent diff = %#v", preview)
	}
	resolved, err := NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{}).Resolve(ctx, ResolveApprovalRequest{
		Scope: scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
		DecisionID: "decision-42", Approve: true, Principal: ApprovalPrincipal{Type: "user", ID: "operator"}, Reason: "Reviewed",
	})
	if err != nil || resolved.Call.Status != ActionCallStatusReady {
		t.Fatalf("resolve = %#v, %v", resolved, err)
	}
	dispatcher, err := NewAgentBehaviorActionDispatcher(store, agents, nil)
	if err != nil {
		t.Fatal(err)
	}
	executed, err := NewActionWorker(store, catalog, nil, dispatcher).RunOnce(ctx, scope, "action-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if executed.Call.Status != ActionCallStatusSucceeded ||
		executed.Call.Output["resourceType"] != agentBehaviorResourceType ||
		executed.Call.Output["replayed"] != false {
		t.Fatalf("execution = %#v", executed)
	}
	deployment, err := agents.GetDeployment(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "researcher")
	if err != nil || deployment.Revision != 2 || deployment.ActiveVersion == "1.0.0" {
		t.Fatalf("deployment = %#v, %v", deployment, err)
	}
	definition, err := agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil || definition.DisplayName != "Evidence Researcher" || definition.Personality != "Calm, curious, and evidence focused" ||
		len(definition.OperatingPrinciples) != 2 || definition.Provenance.DerivedFrom == "" {
		t.Fatalf("definition = %#v, %v", definition, err)
	}
	amendments, err := agents.ListAmendments(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deployment.ID)
	if err != nil || len(amendments) != 1 || amendments[0].Status != kernelagent.AmendmentActivated ||
		amendments[0].Decision == nil || amendments[0].Decision.ActorID != "operator" {
		t.Fatalf("amendments = %#v, %v", amendments, err)
	}
	bound, err := catalog.Resolve(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deployment.ID, AgentManagementSkillID, AgentManagementSkillVersion, AgentActionAmendBehavior)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := dispatcher.DispatchAction(ctx, ActionDispatchInput{Call: executed.Call, Bound: bound, Arguments: executed.Call.Arguments})
	if err != nil || replayed["replayed"] != true {
		t.Fatalf("replay = %#v, %v", replayed, err)
	}
	latest, _ := agents.GetDeployment(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deployment.ID)
	if latest.Revision != 2 {
		t.Fatalf("replay changed Agent revision to %d", latest.Revision)
	}
}

func TestGovernedAgentBehaviorActionUsesCheckpointApprovalAuthority(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "tenant-a"}
	agents := agentBehaviorActionRegistry(t, ctx, scope, &workforce.AmendmentPolicy{
		AgentMayPropose: true, AllowedFields: []string{"systemPrompt"}, RequiresApproval: true,
		ApproverPrincipals: []string{"user:definition-owner"},
	})
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"},
		AssignedAgentID: "researcher", Goal: "Rename the Agent", Source: RunSourceChat,
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
	catalog := agentBehaviorActionCatalog(t, ctx, scope, "researcher")
	validator, err := NewAgentBehaviorActionValidator(agents)
	if err != nil {
		t.Fatal(err)
	}
	policy := ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{
			Disposition: ActionDispositionRequireApproval, Reason: "Agent behavior changes require review",
			EligibleApprovers: []ApprovalPrincipal{{Type: "role", ID: "operator"}}, ApprovalTTL: time.Hour,
		}, nil
	})
	proposal, err := NewActionCoordinator(store, store, catalog, policy, validator).Propose(ctx, ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "conversation-worker", DeploymentID: "researcher",
		SkillID: AgentManagementSkillID, SkillVersion: AgentManagementSkillVersion, Action: AgentActionAmendBehavior,
		Arguments:      map[string]interface{}{"displayName": "Agent 008", "rationale": "The user approved the rename"},
		IdempotencyKey: "rename-agent-008", Summary: "Rename the Agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{}).Resolve(ctx, ResolveApprovalRequest{
		Scope: scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
		DecisionID: "decision-rename", Approve: true, Principal: ApprovalPrincipal{Type: "role", ID: "operator"}, Reason: "Looks good",
	})
	if err != nil || resolved.Call.Status != ActionCallStatusReady {
		t.Fatalf("resolve = %#v, %v", resolved, err)
	}
	dispatcher, err := NewAgentBehaviorActionDispatcher(store, agents, nil)
	if err != nil {
		t.Fatal(err)
	}
	executed, err := NewActionWorker(store, catalog, nil, dispatcher).RunOnce(ctx, scope, "action-worker", time.Minute)
	if err != nil || executed == nil || executed.Call.Status != ActionCallStatusSucceeded {
		t.Fatalf("execution = %#v, %v", executed, err)
	}
	deployment, err := agents.GetDeployment(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "researcher")
	if err != nil {
		t.Fatal(err)
	}
	definition, err := agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil || definition.DisplayName != "Agent 008" {
		t.Fatalf("definition = %#v, %v", definition, err)
	}
	amendments, err := agents.ListAmendments(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deployment.ID)
	if err != nil || len(amendments) != 1 || amendments[0].Decision == nil || amendments[0].Decision.ActorType != "role" || amendments[0].Decision.ActorID != "operator" {
		t.Fatalf("amendments = %#v, %v", amendments, err)
	}
}

func TestAgentBehaviorActionRejectsForeignStaleAndInvalidChanges(t *testing.T) {
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "tenant-a"}
	baseRun := &AgentRun{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"}, AssignedAgentID: "researcher",
	}

	t.Run("foreign binding", func(t *testing.T) {
		agents := agentBehaviorActionRegistry(t, ctx, scope, nil)
		validator, _ := NewAgentBehaviorActionValidator(agents)
		bound := agentBehaviorBoundAction("another-agent")
		_, err := validator.ValidateActionProposal(ctx, ActionProposalValidationInput{
			Run: baseRun, Bound: bound,
			Arguments: map[string]interface{}{"expectedDeploymentRevision": 1, "personality": "Curious", "rationale": "Improve"},
		})
		if err == nil || !strings.Contains(err.Error(), "not bound") {
			t.Fatalf("foreign binding error = %v", err)
		}
	})

	for name, testCase := range map[string]struct {
		mutateRun func(*AgentRun)
		arguments map[string]interface{}
		policy    *workforce.AmendmentPolicy
	}{
		"Team owner": {
			mutateRun: func(run *AgentRun) { run.Owner = ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"} },
			arguments: map[string]interface{}{"expectedDeploymentRevision": 1, "personality": "Curious", "rationale": "Improve"},
		},
		"another assigned Agent": {
			mutateRun: func(run *AgentRun) { run.AssignedAgentID = "another-agent" },
			arguments: map[string]interface{}{"expectedDeploymentRevision": 1, "personality": "Curious", "rationale": "Improve"},
		},
		"stale revision": {
			arguments: map[string]interface{}{"expectedDeploymentRevision": 9, "personality": "Curious", "rationale": "Improve"},
		},
		"unknown credential field": {
			arguments: map[string]interface{}{"expectedDeploymentRevision": 1, "personality": "Curious", "rationale": "Improve", "apiKey": "secret"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			agents := agentBehaviorActionRegistry(t, ctx, scope, testCase.policy)
			validator, _ := NewAgentBehaviorActionValidator(agents)
			run := *baseRun
			if testCase.mutateRun != nil {
				testCase.mutateRun(&run)
			}
			if _, err := validator.ValidateActionProposal(ctx, ActionProposalValidationInput{
				Run: &run, Bound: agentBehaviorBoundAction("researcher"), Arguments: testCase.arguments,
			}); err == nil {
				t.Fatal("invalid Agent behavior action reached approval")
			}
		})
	}

	t.Run("definition outcome evaluations do not block a reviewed amendment proposal", func(t *testing.T) {
		agents := agentBehaviorActionRegistry(t, ctx, scope, nil)
		current, err := agents.GetDefinition(ctx, "researcher-definition", "1.0.0")
		if err != nil {
			t.Fatal(err)
		}
		next := *current
		next.Version = "evaluated"
		next.Digest = ""
		next.Evaluations = []workforce.EvaluationCriterion{{ID: "quality", Description: "Behavior quality", Required: true}}
		if _, err := agents.RegisterDefinition(ctx, &next); err != nil {
			t.Fatal(err)
		}
		if _, _, err := agents.ActivateDefinition(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "researcher", next.Version, 1, "user", "operator", "require evaluation"); err != nil {
			t.Fatal(err)
		}
		validator, _ := NewAgentBehaviorActionValidator(agents)
		preview, err := validator.ValidateActionProposal(ctx, ActionProposalValidationInput{
			Run: baseRun, Bound: agentBehaviorBoundAction("researcher"),
			Arguments: map[string]interface{}{"expectedDeploymentRevision": 2, "personality": "Analytical", "rationale": "Improve"},
		})
		if err != nil || preview["operation"] != AgentActionAmendBehavior {
			t.Fatalf("reviewable evaluated-definition amendment = %#v, %v", preview, err)
		}
	})
}

func TestAgentManagementSkillHidesKernelRevisionFromModel(t *testing.T) {
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "tenant-a"}
	catalog := agentBehaviorActionCatalog(t, ctx, scope, "researcher")
	actions, err := catalog.ListModelActions(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "researcher")
	if err != nil || len(actions) != 1 {
		t.Fatalf("model actions = %#v, %v", actions, err)
	}
	properties, _ := actions[0].InputSchema["properties"].(map[string]interface{})
	if _, visible := properties["expectedDeploymentRevision"]; visible {
		t.Fatalf("kernel revision leaked into model schema: %#v", actions[0].InputSchema)
	}
	bound, err := catalog.Resolve(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "researcher", AgentManagementSkillID, AgentManagementSkillVersion, AgentActionAmendBehavior)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.ValidateInput(ctx, bound, map[string]interface{}{"personality": "Curious", "rationale": "Improve"}); err == nil {
		t.Fatal("execution schema accepted unresolved deployment revision")
	}
}

func agentBehaviorActionRegistry(
	t *testing.T,
	ctx context.Context,
	scope Scope,
	policy *workforce.AmendmentPolicy,
) *kernelagent.Registry {
	t.Helper()
	amendments := workforce.AmendmentPolicy{
		AgentMayPropose:  true,
		AllowedFields:    []string{"systemPrompt"},
		RequiresApproval: true, ApproverPrincipals: []string{"user:operator"},
	}
	if policy != nil {
		amendments = *policy
	}
	agents := kernelagent.NewRegistry()
	definition, err := agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "researcher-definition", Version: "1.0.0", DisplayName: "Researcher", Purpose: "Research product pain points",
		SystemPrompt: "Research carefully.", Personality: "Curious",
		OperatingPrinciples: []string{"Separate facts from inference."},
		Evaluations:         []workforce.EvaluationCriterion{{ID: "quality", Description: "Work remains high quality", Required: true}},
		Authority:           kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelWrite, MaxConcurrentRuns: 1},
		Amendments:          amendments,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "researcher", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID},
		DefinitionID: definition.ID, ActiveVersion: definition.Version, RolloutStatus: kernelagent.RolloutActive,
		Environment: "local", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "operator", "initial Agent"); err != nil {
		t.Fatal(err)
	}
	return agents
}

func agentBehaviorActionCatalog(t *testing.T, ctx context.Context, scope Scope, deploymentID string) *skill.Catalog {
	t.Helper()
	catalog := skill.NewCatalog()
	if err := catalog.Register(ctx, AgentManagementSkill()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "agents", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: deploymentID,
		SkillID: AgentManagementSkillID, SkillVersion: AgentManagementSkillVersion,
		AllowedActions: []string{AgentActionAmendBehavior}, MaximumRisk: skill.RiskLevelWrite,
	}); err != nil {
		t.Fatal(err)
	}
	return catalog
}

func agentBehaviorBoundAction(deploymentID string) *skill.BoundAction {
	definition := AgentManagementSkill()
	return &skill.BoundAction{
		Definition: definition, Action: definition.Actions[AgentActionAmendBehavior],
		Binding: &skill.Binding{
			ID: "agents", Revision: 1, Scope: skill.ScopeReference{Kind: "tenant", ID: "tenant-a"},
			DeploymentID: deploymentID, SkillID: AgentManagementSkillID, SkillVersion: AgentManagementSkillVersion,
			AllowedActions: []string{AgentActionAmendBehavior}, MaximumRisk: skill.RiskLevelWrite,
		},
	}
}
