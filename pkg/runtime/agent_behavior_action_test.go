package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func TestAgentBehaviorAmendmentSchemaRequiresAnActualChange(t *testing.T) {
	action := models.AgentManagementSkill().Actions[models.AgentActionAmendBehavior]
	controlOnly := map[string]interface{}{
		"expectedDeploymentRevision": float64(1),
		"rationale":                  "Update the persona heritage",
	}
	if err := runbook.ValidateInterfaceInput(action.InputSchema, controlOnly); err == nil {
		t.Fatal("amend_behavior accepted rationale without a changed behavior field")
	}
	withChange := maps.Clone(controlOnly)
	withChange["personality"] = "Utah-native engineer with Black heritage."
	if err := runbook.ValidateInterfaceInput(action.InputSchema, withChange); err != nil {
		t.Fatalf("amend_behavior rejected an actual personality change: %v", err)
	}
}

func TestAgentManagementExecutorAcceptsActivePreviousContractVersion(t *testing.T) {
	for _, version := range []string{"1.2.0", "1.2.1", "1.2.2", "1.3.0", "1.3.1", "1.3.2", "1.4.0", "1.5.0", models.AgentManagementSkillVersion} {
		t.Run(version, func(t *testing.T) {
			definition := models.AgentManagementSkill()
			definition.Version = version
			action := definition.Actions[models.AgentActionAmendBehavior]
			if !models.IsAgentManagementAction(&skill.BoundAction{Definition: definition, Action: action}) {
				t.Fatalf("active %s Agent management binding is no longer executable", version)
			}
		})
	}
}

func TestGovernedAgentChannelActionComposesIngressAndApprovalDelivery(t *testing.T) {
	ctx := context.Background()
	store, catalog, scope, adapter := slackConversationCatalog(t, ctx)
	endpoints := externalconversation.NewExternalConversationEndpointService(store, catalog)
	endpoint, err := endpoints.Create(ctx, externalconversation.CreateExternalConversationEndpointRequest{
		ID: "slack-channel", Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "slack-agent"},
		DeploymentID: "slack-agent", Name: "Agent approvals", Adapter: adapter,
		Mode: capability.ConversationEndpointChannel, Address: "C012345",
		Handler: models.ExternalConversationHandler{Kind: models.ExternalConversationHandlerAgent, ID: "slack-agent"},
		Policy:  models.ExternalConversationPolicy{MessageSelection: models.ExternalConversationSelectMentions, ReplyMode: models.ExternalConversationReplyThread, IgnoreBots: true},
		Status:  models.ExternalConversationEndpointActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Channel activation writes the Agent definition and the endpoint in one
	// transaction, so the registry shares the runtime store.
	agents := kernelagent.NewRegistryWithStore(store)
	definition, err := agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "slack-agent-definition", Version: "1.0.0", DisplayName: "Slack Agent", Purpose: "Handle Slack work", SystemPrompt: "Work carefully.",
		Authority:  kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelWrite, MaxConcurrentRuns: 1},
		Amendments: workforce.AmendmentPolicy{AgentMayPropose: true, RequiresApproval: true, ApproverPrincipals: []string{"user:operator"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "slack-agent", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: definition.ID,
		ActiveVersion: definition.Version, RolloutStatus: kernelagent.RolloutActive, Environment: "test", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "operator", "test"); err != nil {
		t.Fatal(err)
	}
	if err = catalog.Register(ctx, models.AgentManagementSkill()); err != nil {
		t.Fatal(err)
	}
	if err = catalog.Bind(ctx, &skill.Binding{
		ID: "agents", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "slack-agent",
		SkillID: models.AgentManagementSkillID, SkillVersion: models.AgentManagementSkillVersion,
		AllowedActions: []string{models.AgentActionListChannels, models.AgentActionConfigureChannel}, MaximumRisk: skill.RiskLevelWrite,
	}); err != nil {
		t.Fatal(err)
	}
	run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "slack-agent"}, AssignedAgentID: "slack-agent", Goal: "Configure Slack", Source: models.RunSourceChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: "conversation-worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute}); err != nil {
		t.Fatal(err)
	}
	validator, _ := NewAgentBehaviorActionValidator(agents, endpoints)
	policy := action.ActionPolicyEvaluatorFunc(func(context.Context, action.ActionPolicyInput) (action.ActionPolicyDecision, error) {
		return action.ActionPolicyDecision{Disposition: action.ActionDispositionRequireApproval, Reason: "Review routing", EligibleApprovers: []models.ApprovalPrincipal{{Type: "user", ID: "operator"}}, ApprovalTTL: time.Hour}, nil
	})
	proposal, err := action.NewActionCoordinator(store, store, catalog, policy, []action.ActionProposalValidator{validator}).Propose(ctx, actiontest.LoopProposeRequest(action.ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "conversation-worker", DeploymentID: "slack-agent",
		SkillID: models.AgentManagementSkillID, SkillVersion: models.AgentManagementSkillVersion, Action: models.AgentActionConfigureChannel,
		Arguments:      map[string]interface{}{"endpointId": endpoint.ID, "messageSelection": "all_messages", "replyMode": "thread", "purposes": []interface{}{"conversation", "approvals"}, "rationale": "Use Slack for work and approvals"},
		IdempotencyKey: "configure-slack", Summary: "Configure Slack workflow channel",
	}, "configure-slack"))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := action.NewApprovalCoordinator(store, store, action.EligibleApprovalAuthorizer{}).Resolve(ctx, action.ResolveApprovalRequest{
		Scope: scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision, DecisionID: "approve-routing", Decision: models.ApprovalDecisionApprove,
		Principal: models.ApprovalPrincipal{Type: "user", ID: "operator"}, Reason: "Approved",
	})
	if err != nil || resolved.Call.Status != models.ActionCallStatusReady {
		t.Fatalf("resolve=%#v err=%v", resolved, err)
	}
	dispatcher, _ := NewAgentBehaviorActionDispatcher(store, agents, nil, endpoints)
	executed, err := action.NewActionWorker(store, catalog, nil, dispatcher).ExecuteOwnLoopCall(ctx, scope, proposal.Call.ID, "conversation-worker")
	if err != nil || executed.Call.Status != models.ActionCallStatusSucceeded {
		t.Fatalf("execute status=%s callError=%q output=%#v err=%v", executed.Call.Status, executed.Call.Error, executed.Call.Output, err)
	}
	deployment, _ := agents.GetDeployment(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "slack-agent")
	active, _ := agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	updated, _ := endpoints.Get(ctx, scope, endpoint.ID)
	if len(active.Channels) != 1 || len(active.Authority.ApprovalDestinations) != 1 ||
		updated.Handler.Kind != models.ExternalConversationHandlerAgent || updated.Handler.ID != "slack-agent" || updated.Policy.MessageSelection != models.ExternalConversationSelectAllMessages {
		t.Fatalf("definition=%#v endpoint=%#v", active, updated)
	}
	listed, err := dispatcher.listChannels(ctx, scope, "slack-agent")
	if err != nil || len(listed["channels"].([]map[string]interface{})) != 1 {
		t.Fatalf("listed=%#v err=%v", listed, err)
	}
}

func TestGovernedAgentBehaviorActionActivatesImmutableDefinitionAndReplays(t *testing.T) {
	ctx := context.Background()
	store := conversationtest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "tenant-a"}
	agents := agentBehaviorActionRegistry(t, ctx, scope, nil)
	run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "researcher"},
		AssignedAgentID: "researcher", Goal: "Become more evidence focused", Source: models.RunSourceChat,
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
	catalog := agentBehaviorActionCatalog(t, ctx, scope, "researcher")
	validator, err := NewAgentBehaviorActionValidator(agents)
	if err != nil {
		t.Fatal(err)
	}
	policy := action.ActionPolicyEvaluatorFunc(func(context.Context, action.ActionPolicyInput) (action.ActionPolicyDecision, error) {
		return action.ActionPolicyDecision{
			Disposition: action.ActionDispositionRequireApproval, Reason: "Agent behavior changes require review",
			EligibleApprovers: []models.ApprovalPrincipal{{Type: "user", ID: "operator"}}, ApprovalTTL: time.Hour,
		}, nil
	})
	coordinator := action.NewActionCoordinator(store, store, catalog, policy, []action.ActionProposalValidator{validator})
	proposal, err := coordinator.Propose(ctx, actiontest.LoopProposeRequest(action.ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "conversation-worker", DeploymentID: "researcher",
		SkillID: models.AgentManagementSkillID, SkillVersion: models.AgentManagementSkillVersion, Action: models.AgentActionAmendBehavior,
		Arguments: map[string]interface{}{
			"displayName":         "Evidence Researcher",
			"personality":         "Calm, curious, and evidence focused",
			"operatingPrinciples": []interface{}{"Separate facts from inference.", "Cite durable evidence."},
			"approvalTimeout":     map[string]interface{}{"afterSeconds": float64(900), "decision": "approve"},
			"rationale":           "Improve research quality",
		},
		IdempotencyKey: "message-42:improve-behavior", Summary: "Improve the researcher's evidence discipline",
	}, "improve-behavior"))
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Approval == nil || proposal.Call.Status != models.ActionCallStatusWaitingApproval {
		t.Fatalf("proposal lifecycle = %#v", proposal)
	}
	if _, modelSupplied := proposal.Call.Arguments["expectedDeploymentRevision"]; !modelSupplied ||
		fmt.Sprint(proposal.Call.Arguments["expectedDeploymentRevision"]) != "1" {
		t.Fatalf("kernel revision was not resolved: %#v", proposal.Call.Arguments)
	}
	preview := proposal.Approval.ProposedAction
	changes, _ := preview["changes"].(map[string]interface{})
	if preview["resourceType"] != models.AgentBehaviorResourceType || preview["operation"] != models.AgentActionAmendBehavior ||
		preview["deploymentId"] != "researcher" || changes["personality"] != "Calm, curious, and evidence focused" ||
		changes["authority.approvalTimeout"] == nil {
		t.Fatalf("typed Agent diff = %#v", preview)
	}
	resolved, err := action.NewApprovalCoordinator(store, store, action.EligibleApprovalAuthorizer{}).Resolve(ctx, action.ResolveApprovalRequest{
		Scope: scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
		DecisionID: "decision-42", Decision: models.ApprovalDecisionApprove, Principal: models.ApprovalPrincipal{Type: "user", ID: "operator"}, Reason: "Reviewed",
	})
	if err != nil || resolved.Call.Status != models.ActionCallStatusReady {
		t.Fatalf("resolve = %#v, %v", resolved, err)
	}
	dispatcher, err := NewAgentBehaviorActionDispatcher(store, agents, nil)
	if err != nil {
		t.Fatal(err)
	}
	executed, err := action.NewActionWorker(store, catalog, nil, dispatcher).ExecuteOwnLoopCall(ctx, scope, proposal.Call.ID, "conversation-worker")
	if err != nil {
		t.Fatal(err)
	}
	if executed.Call.Status != models.ActionCallStatusSucceeded ||
		executed.Call.Output["resourceType"] != models.AgentBehaviorResourceType ||
		executed.Call.Output["replayed"] != false {
		t.Fatalf("execution = %#v", executed)
	}
	deployment, err := agents.GetDeployment(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "researcher")
	if err != nil || deployment.Revision != 2 || deployment.ActiveVersion == "1.0.0" {
		t.Fatalf("deployment = %#v, %v", deployment, err)
	}
	definition, err := agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil || definition.DisplayName != "Evidence Researcher" || definition.Personality != "Calm, curious, and evidence focused" ||
		len(definition.OperatingPrinciples) != 2 || definition.Provenance.DerivedFrom == "" ||
		definition.Authority.ApprovalTimeout == nil || definition.Authority.ApprovalTimeout.AfterSeconds != 900 ||
		definition.Authority.ApprovalTimeout.Decision != "approve" {
		t.Fatalf("definition = %#v, %v", definition, err)
	}
	amendments, err := agents.ListAmendments(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deployment.ID)
	if err != nil || len(amendments) != 1 || amendments[0].Status != kernelagent.AmendmentActivated ||
		amendments[0].Decision == nil || amendments[0].Decision.ActorID != "operator" {
		t.Fatalf("amendments = %#v, %v", amendments, err)
	}
	bound, err := catalog.Resolve(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deployment.ID, models.AgentManagementSkillID, models.AgentManagementSkillVersion, models.AgentActionAmendBehavior)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := dispatcher.DispatchAction(ctx, agentrun.ActionDispatchInput{Call: executed.Call, Bound: bound, Arguments: executed.Call.Arguments})
	if err != nil || replayed["replayed"] != true {
		t.Fatalf("replay = %#v, %v", replayed, err)
	}
	latest, _ := agents.GetDeployment(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deployment.ID)
	if latest.Revision != 2 {
		t.Fatalf("replay changed Agent revision to %d", latest.Revision)
	}
}

// A Team member's Run amends that member exactly as its own chat would.
func TestTeamMemberRunAmendsItsOwnBehavior(t *testing.T) {
	ctx := context.Background()
	store := conversationtest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "tenant-a"}
	agents := agentBehaviorActionRegistry(t, ctx, scope, nil)
	run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "research-team"},
		AssignedAgentID: "researcher", Goal: "Rename yourself", Source: models.RunSourceChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !models.IsTeamMemberRun(run) {
		t.Fatalf("run is not a Team member Run: %#v", run)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{
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
	policy := action.ActionPolicyEvaluatorFunc(func(context.Context, action.ActionPolicyInput) (action.ActionPolicyDecision, error) {
		return action.ActionPolicyDecision{
			Disposition: action.ActionDispositionRequireApproval, Reason: "Agent behavior changes require review",
			EligibleApprovers: []models.ApprovalPrincipal{{Type: "user", ID: "operator"}}, ApprovalTTL: time.Hour,
		}, nil
	})
	propose := func(key string) (*models.ActionProposalResult, error) {
		return action.NewActionCoordinator(store, store, catalog, policy, []action.ActionProposalValidator{validator}).Propose(ctx, actiontest.LoopProposeRequest(action.ProposeActionRequest{
			Scope: scope, RunID: run.ID, WorkerID: "conversation-worker", DeploymentID: "researcher",
			SkillID: models.AgentManagementSkillID, SkillVersion: models.AgentManagementSkillVersion, Action: models.AgentActionAmendBehavior,
			Arguments:      map[string]interface{}{"displayName": "Team Researcher", "rationale": "The user asked for the rename"},
			IdempotencyKey: key, Summary: "Rename the member",
		}, key))
	}
	// Without the Team roster, or off it, the member cannot amend itself.
	if _, err := propose("team-rename-unproven"); err == nil || !strings.Contains(err.Error(), "requires the current Team roster") {
		t.Fatalf("proposal without the Team roster = %v, want the roster rejection", err)
	}
	team := &kernelteam.Deployment{ID: "research-team", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, Status: kernelteam.DeploymentActive,
		Roster: []kernelteam.RosterAssignment{{ID: "other", AgentDeploymentID: "writer"}}}
	validator.SetTeamRoster(rosterPortfolio{Store: store, team: team})
	if _, err := propose("team-rename-removed"); err == nil || !strings.Contains(err.Error(), "is not on the active Team roster") {
		t.Fatalf("proposal from an Agent off the Team roster = %v, want the roster rejection", err)
	}
	team.Roster = append(team.Roster, kernelteam.RosterAssignment{ID: "member", AgentDeploymentID: "researcher"})
	proposal, err := propose("team-rename")
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Approval == nil || proposal.Approval.ProposedAction["deploymentId"] != "researcher" {
		t.Fatalf("proposal = %#v", proposal)
	}
	resolved, err := action.NewApprovalCoordinator(store, store, action.EligibleApprovalAuthorizer{}).Resolve(ctx, action.ResolveApprovalRequest{
		Scope: scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
		DecisionID: "decision-team-rename", Decision: models.ApprovalDecisionApprove, Principal: models.ApprovalPrincipal{Type: "user", ID: "operator"}, Reason: "Fine",
	})
	if err != nil || resolved.Call.Status != models.ActionCallStatusReady {
		t.Fatalf("resolve = %#v, %v", resolved, err)
	}
	dispatcher, err := NewAgentBehaviorActionDispatcher(rosterPortfolio{Store: store, team: team}, agents, nil)
	if err != nil {
		t.Fatal(err)
	}
	executed, err := action.NewActionWorker(store, catalog, nil, dispatcher).ExecuteOwnLoopCall(ctx, scope, proposal.Call.ID, "conversation-worker")
	if err != nil || executed == nil || executed.Call.Status != models.ActionCallStatusSucceeded {
		t.Fatalf("execution = %#v, %v", executed, err)
	}
	deployment, err := agents.GetDeployment(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "researcher")
	if err != nil {
		t.Fatal(err)
	}
	definition, err := agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil || definition.DisplayName != "Team Researcher" {
		t.Fatalf("definition = %#v, %v", definition, err)
	}
}

func TestGovernedAgentBehaviorActionUsesCheckpointApprovalAuthority(t *testing.T) {
	ctx := context.Background()
	store := conversationtest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "tenant-a"}
	agents := agentBehaviorActionRegistry(t, ctx, scope, &workforce.AmendmentPolicy{
		AgentMayPropose: true, AllowedFields: []string{"systemPrompt"}, RequiresApproval: true,
		ApproverPrincipals: []string{"user:definition-owner"},
	})
	run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "researcher"},
		AssignedAgentID: "researcher", Goal: "Rename the Agent", Source: models.RunSourceChat,
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
	catalog := agentBehaviorActionCatalog(t, ctx, scope, "researcher")
	validator, err := NewAgentBehaviorActionValidator(agents)
	if err != nil {
		t.Fatal(err)
	}
	policy := action.ActionPolicyEvaluatorFunc(func(context.Context, action.ActionPolicyInput) (action.ActionPolicyDecision, error) {
		return action.ActionPolicyDecision{
			Disposition: action.ActionDispositionRequireApproval, Reason: "Agent behavior changes require review",
			EligibleApprovers: []models.ApprovalPrincipal{{Type: "role", ID: "operator"}}, ApprovalTTL: time.Hour,
		}, nil
	})
	proposal, err := action.NewActionCoordinator(store, store, catalog, policy, []action.ActionProposalValidator{validator}).Propose(ctx, actiontest.LoopProposeRequest(action.ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "conversation-worker", DeploymentID: "researcher",
		SkillID: models.AgentManagementSkillID, SkillVersion: models.AgentManagementSkillVersion, Action: models.AgentActionAmendBehavior,
		Arguments:      map[string]interface{}{"displayName": "Agent 008", "rationale": "The user approved the rename"},
		IdempotencyKey: "rename-agent-008", Summary: "Rename the Agent",
	}, "rename-agent-008"))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := action.NewApprovalCoordinator(store, store, action.EligibleApprovalAuthorizer{}).Resolve(ctx, action.ResolveApprovalRequest{
		Scope: scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
		DecisionID: "decision-rename", Decision: models.ApprovalDecisionApprove, Principal: models.ApprovalPrincipal{Type: "role", ID: "operator"}, Reason: "Looks good",
	})
	if err != nil || resolved.Call.Status != models.ActionCallStatusReady {
		t.Fatalf("resolve = %#v, %v", resolved, err)
	}
	dispatcher, err := NewAgentBehaviorActionDispatcher(store, agents, nil)
	if err != nil {
		t.Fatal(err)
	}
	executed, err := action.NewActionWorker(store, catalog, nil, dispatcher).ExecuteOwnLoopCall(ctx, scope, proposal.Call.ID, "conversation-worker")
	if err != nil || executed == nil || executed.Call.Status != models.ActionCallStatusSucceeded {
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
	scope := models.Scope{Kind: "tenant", ID: "tenant-a"}
	baseRun := &models.AgentRun{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "researcher"}, AssignedAgentID: "researcher",
	}

	t.Run("foreign binding", func(t *testing.T) {
		agents := agentBehaviorActionRegistry(t, ctx, scope, nil)
		validator, _ := NewAgentBehaviorActionValidator(agents)
		bound := agentBehaviorBoundAction("another-agent")
		_, err := validator.ValidateActionProposal(ctx, agentrun.ActionProposalValidationInput{
			Run: baseRun, Bound: bound,
			Arguments: map[string]interface{}{"expectedDeploymentRevision": 1, "personality": "Curious", "rationale": "Improve"},
		})
		if err == nil || !strings.Contains(err.Error(), "not bound") {
			t.Fatalf("foreign binding error = %v", err)
		}
	})

	for name, testCase := range map[string]struct {
		mutateRun func(*models.AgentRun)
		arguments map[string]interface{}
		policy    *workforce.AmendmentPolicy
	}{
		"unassigned Team routing Run": {
			mutateRun: func(run *models.AgentRun) {
				run.Kind, run.Owner, run.AssignedAgentID = models.RunKindConversation, models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team"}, ""
			},
			arguments: map[string]interface{}{"expectedDeploymentRevision": 1, "personality": "Curious", "rationale": "Improve"},
		},
		"another assigned Agent": {
			mutateRun: func(run *models.AgentRun) { run.AssignedAgentID = "another-agent" },
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
			if _, err := validator.ValidateActionProposal(ctx, agentrun.ActionProposalValidationInput{
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
		preview, err := validator.ValidateActionProposal(ctx, agentrun.ActionProposalValidationInput{
			Run: baseRun, Bound: agentBehaviorBoundAction("researcher"),
			Arguments: map[string]interface{}{"expectedDeploymentRevision": 2, "personality": "Analytical", "rationale": "Improve"},
		})
		if err != nil || preview["operation"] != models.AgentActionAmendBehavior {
			t.Fatalf("reviewable evaluated-definition amendment = %#v, %v", preview, err)
		}
	})
}

func TestAgentManagementSkillHidesKernelRevisionFromModel(t *testing.T) {
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "tenant-a"}
	catalog := agentBehaviorActionCatalog(t, ctx, scope, "researcher")
	actions, err := catalog.ListModelActions(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "researcher")
	if err != nil || len(actions) != 1 {
		t.Fatalf("model actions = %#v, %v", actions, err)
	}
	properties, _ := actions[0].InputSchema["properties"].(map[string]interface{})
	if _, visible := properties["expectedDeploymentRevision"]; visible {
		t.Fatalf("kernel revision leaked into model schema: %#v", actions[0].InputSchema)
	}
	bound, err := catalog.Resolve(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "researcher", models.AgentManagementSkillID, models.AgentManagementSkillVersion, models.AgentActionAmendBehavior)
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
	scope models.Scope,
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

func agentBehaviorActionCatalog(t *testing.T, ctx context.Context, scope models.Scope, deploymentID string) *skill.Catalog {
	t.Helper()
	catalog := skill.NewCatalog()
	if err := catalog.Register(ctx, models.AgentManagementSkill()); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "agents", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: deploymentID,
		SkillID: models.AgentManagementSkillID, SkillVersion: models.AgentManagementSkillVersion,
		AllowedActions: []string{models.AgentActionAmendBehavior}, MaximumRisk: skill.RiskLevelWrite,
	}); err != nil {
		t.Fatal(err)
	}
	return catalog
}

func agentBehaviorBoundAction(deploymentID string) *skill.BoundAction {
	definition := models.AgentManagementSkill()
	return &skill.BoundAction{
		Definition: definition, Action: definition.Actions[models.AgentActionAmendBehavior],
		Binding: &skill.Binding{
			ID: "agents", Revision: 1, Scope: skill.ScopeReference{Kind: "tenant", ID: "tenant-a"},
			DeploymentID: deploymentID, SkillID: models.AgentManagementSkillID, SkillVersion: models.AgentManagementSkillVersion,
			AllowedActions: []string{models.AgentActionAmendBehavior}, MaximumRisk: skill.RiskLevelWrite,
		},
	}
}

func TestOlderAgentManagementBindingsAlwaysRequireReviewInSkipMode(t *testing.T) {
	for _, version := range []string{"1.2.0", "1.2.1", "1.2.2", "1.3.0", "1.3.1"} {
		t.Run(version, func(t *testing.T) {
			ctx := t.Context()
			store := conversationtest.NewStore(t)
			conversation := conversationtest.ApprovalModeConversation(t, store)
			conversationtest.SetApprovalMode(t, runtimeconversation.NewConversationApprovalModeService(store), conversation, models.ConversationApprovalSkip)
			// Published versions predate review: always, so their stored
			// definitions carry no review floor.
			legacy := models.AgentManagementSkill()
			legacy.Version = version
			for name, action := range legacy.Actions {
				action.Review = ""
				legacy.Actions[name] = action
			}
			catalog := skill.NewCatalog()
			if err := catalog.Register(ctx, legacy); err != nil {
				t.Fatal(err)
			}
			if err := catalog.Bind(ctx, &skill.Binding{
				ID: "legacy-agents", Revision: 1, Scope: skill.ScopeReference{Kind: conversation.Scope.Kind, ID: conversation.Scope.ID}, DeploymentID: "assistant",
				SkillID: models.AgentManagementSkillID, SkillVersion: version, AllowedActions: []string{models.AgentActionAmendBehavior, models.AgentActionConfigureChannel}, MaximumRisk: skill.RiskLevelWrite,
			}); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{models.AgentActionAmendBehavior, models.AgentActionConfigureChannel} {
				bound, err := catalog.Resolve(ctx, skill.ScopeReference{Kind: conversation.Scope.Kind, ID: conversation.Scope.ID}, "assistant", models.AgentManagementSkillID, version, name)
				if err != nil || bound.Action.AlwaysReview() {
					t.Fatalf("legacy %s binding = %#v, %v", name, bound, err)
				}
				var seen action.ActionPolicyInput
				policy := action.ActionPolicyEvaluatorFunc(func(ctx context.Context, input action.ActionPolicyInput) (action.ActionPolicyDecision, error) {
					seen = input
					return action.NewDefaultActionPolicy().EvaluateAction(ctx, input)
				})
				run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
					Scope: conversation.Scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "assistant"}, AssignedAgentID: "assistant",
					Goal: "Rename the agent", Source: models.RunSourceObjective, Context: map[string]interface{}{"conversationId": conversation.ID},
				})
				if err != nil {
					t.Fatal(err)
				}
				if claimed, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: conversation.Scope, WorkerID: "worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute}); err != nil || claimed == nil || claimed.ID != run.ID {
					t.Fatalf("claim: %#v %v", claimed, err)
				}
				result, err := action.NewActionCoordinator(store, store, catalog, policy, nil).Propose(ctx, actiontest.LoopProposeRequest(action.ProposeActionRequest{
					Scope: conversation.Scope, RunID: run.ID, WorkerID: "worker", DeploymentID: "assistant",
					SkillID: models.AgentManagementSkillID, SkillVersion: version, Action: name,
					Arguments: legacyAgentManagementArguments[name], IdempotencyKey: "legacy-" + name, Summary: "Apply legacy " + name,
				}, name))
				if err != nil {
					t.Fatalf("propose legacy %s: %v", name, err)
				}
				if seen.ApprovalMode != models.ConversationApprovalSkip || !seen.Bound.Action.AlwaysReview() {
					t.Fatalf("%s policy input mode=%q review=%q err=%v", name, seen.ApprovalMode, seen.Bound.Action.Review, err)
				}
				if result.Approval == nil || result.Call.Review != skill.ActionReviewAlways {
					t.Fatalf("skip mode waived legacy %s review: %#v", name, result)
				}
			}
		})
	}
}

var legacyAgentManagementArguments = map[string]map[string]interface{}{
	models.AgentActionAmendBehavior: {"expectedDeploymentRevision": 1, "displayName": "Renamed", "rationale": "The user asked for a new name"},
	models.AgentActionConfigureChannel: {"expectedDeploymentRevision": 1, "expectedEndpointRevision": 1, "endpointId": "slack", "status": "paused",
		"rationale": "The user asked to pause the channel"},
}

// The roster can change while a proposal waits for approval, so dispatch
// rechecks it; an Agent's own Run is its roster of one.
func TestAgentBehaviorRunOnRosterChecksTheCurrentTeamRoster(t *testing.T) {
	ctx, store, scope := context.Background(), conversationtest.NewStore(t), models.Scope{Kind: "tenant", ID: "tenant-a"}
	team := &kernelteam.Deployment{ID: "research-team", Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, Status: kernelteam.DeploymentActive,
		Roster: []kernelteam.RosterAssignment{{ID: "member", AgentDeploymentID: "researcher"}}}
	teams := rosterPortfolio{Store: store, team: team}
	member := &models.AgentRun{Scope: scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "research-team"}, AssignedAgentID: "researcher"}
	if err := agentBehaviorRunOnRoster(ctx, teams, member); err != nil {
		t.Fatalf("roster member rejected: %v", err)
	}
	if err := agentBehaviorRunOnRoster(ctx, nil, member); err == nil {
		t.Fatal("Team member accepted without a Team roster")
	}
	team.Roster = nil
	if err := agentBehaviorRunOnRoster(ctx, teams, member); err == nil {
		t.Fatal("removed member accepted")
	}
	team.Roster, team.Status = []kernelteam.RosterAssignment{{ID: "member", AgentDeploymentID: "researcher"}}, kernelteam.DeploymentPaused
	if err := agentBehaviorRunOnRoster(ctx, teams, member); err == nil {
		t.Fatal("member of an inactive Team accepted")
	}
	own := &models.AgentRun{Scope: scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "researcher"}, AssignedAgentID: "researcher"}
	if err := agentBehaviorRunOnRoster(ctx, nil, own); err != nil {
		t.Fatalf("Agent's own Run rejected: %v", err)
	}
	if id, err := agentBehaviorDeploymentID(own); err != nil || id != "researcher" {
		t.Fatalf("own Run Agent = %q, %v", id, err)
	}
	if _, err := agentBehaviorDeploymentID(&models.AgentRun{Kind: models.RunKindConversation, Owner: own.Owner}); err == nil {
		t.Fatal("Run without an acting Agent accepted")
	}
	if _, err := agentBehaviorDeploymentID(&models.AgentRun{Kind: models.RunKindConversation, Owner: own.Owner, AssignedAgentID: "writer"}); err == nil {
		t.Fatal("Agent's chat Run assigned to another Agent accepted")
	}
	// A member's background task amends its own behavior like its 1:1 task.
	team.Status = kernelteam.DeploymentActive
	task := &models.AgentRun{Scope: scope, Kind: models.RunKindAgentWork, Owner: member.Owner, AssignedAgentID: "researcher"}
	if id, err := agentBehaviorDeploymentID(task); err != nil || id != "researcher" {
		t.Fatalf("Team member task Agent = %q, %v", id, err)
	}
	if err := agentBehaviorRunOnRoster(ctx, teams, task); err != nil {
		t.Fatalf("Team member task rejected: %v", err)
	}
	team.Roster = nil
	if err := agentBehaviorRunOnRoster(ctx, teams, task); err == nil {
		t.Fatal("removed member's task accepted")
	}
	if _, err := agentBehaviorDeploymentID(&models.AgentRun{Kind: models.RunKindAgentWork, Owner: member.Owner}); err == nil {
		t.Fatal("Team Run without an acting Agent accepted")
	}
}

func TestAgentChannelActionRefusesRemovedTrigger(t *testing.T) {
	base := map[string]interface{}{"endpointId": "slack-channel", "replyMode": "thread", "rationale": "Reply in threads"}
	for name, trigger := range map[string]interface{}{"named": "on-message", "padded": "  on-message ", "not a string": 7.0} {
		t.Run(name, func(t *testing.T) {
			arguments := maps.Clone(base)
			arguments["trigger"] = trigger
			if _, err := withoutRemovedChannelTrigger(arguments); !errors.Is(err, ErrChannelTriggerRemoved) || err.Error() != "Channel triggers were removed" {
				t.Fatalf("trigger %#v: err = %v", trigger, err)
			}
			if _, _, _, _, _, _, err := resolveAgentChannelAction(t.Context(), kernelagent.NewRegistry(), nil, &models.AgentRun{}, arguments); !errors.Is(err, ErrChannelTriggerRemoved) {
				t.Fatalf("resolve with trigger %#v: err = %v", trigger, err)
			}
		})
	}
	arguments := maps.Clone(base)
	arguments["trigger"] = " "
	kept, err := withoutRemovedChannelTrigger(arguments)
	if err != nil || len(kept) != len(base) || kept["trigger"] != nil || arguments["trigger"] != " " {
		t.Fatalf("empty trigger: kept=%#v err=%v caller=%#v", kept, err, arguments)
	}
	var args agentChannelActionArguments
	if err := decodeAgentBehaviorArguments(kept, &args); err != nil || args.EndpointID != "slack-channel" {
		t.Fatalf("decode without trigger: %#v, %v", args, err)
	}
	if unchanged, err := withoutRemovedChannelTrigger(base); err != nil || len(unchanged) != len(base) {
		t.Fatalf("no trigger: %#v, %v", unchanged, err)
	}
}

func TestAgentManagementSkillHasNoChannelTrigger(t *testing.T) {
	definition := models.AgentManagementSkill()
	if definition.Version != "1.6.0" {
		t.Fatalf("version = %s", definition.Version)
	}
	properties := definition.Actions[models.AgentActionConfigureChannel].InputSchema["properties"].(map[string]interface{})
	if _, ok := properties["trigger"]; ok {
		t.Fatal("configure_channel still takes a trigger")
	}
	for _, name := range []string{models.AgentActionListChannels, models.AgentActionConfigureChannel} {
		if strings.Contains(definition.Actions[name].Description, "Runbook trigger") {
			t.Fatalf("%s still describes Runbook triggers", name)
		}
	}
}
