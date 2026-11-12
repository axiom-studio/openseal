package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// agentBuilderApprovalCoordinator wires the builder fixture to a kernel store
// with a claimed Finn Run, so proposals take the real coordinator path.
func agentBuilderApprovalCoordinator(t *testing.T, policy ActionPolicyEvaluator, now time.Time) (*agentBuilderFixture, *SQLiteStore, *AgentRun, *ActionCoordinator) {
	t.Helper()
	ctx := context.Background()
	fixture := newAgentBuilderFixture(t, true)
	store := newTestKernelStore(t)
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: fixture.scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "finn"},
		AssignedAgentID: "finn", Goal: "Build an inbox helper", Source: RunSourceChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{
		Scope: fixture.scope, WorkerID: "conversation-worker", Now: time.Now().UTC(), LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	coordinator := NewActionCoordinator(store, store, fixture.catalog, policy, fixture.validator)
	coordinator.now = func() time.Time { return now }
	return fixture, store, claimed, coordinator
}

func agentBuilderApprovalRequest(fixture *agentBuilderFixture, run *AgentRun, action string, arguments map[string]interface{}, toolCallID string) ProposeActionRequest {
	return loopProposeRequest(ProposeActionRequest{
		Scope: fixture.scope, RunID: run.ID, WorkerID: "conversation-worker", DeploymentID: "finn",
		SkillID: AgentManagementSkillID, SkillVersion: AgentManagementSkillVersion, Action: action,
		Arguments: arguments, Summary: "Review the agent",
	}, toolCallID)
}

// generousBuilderPolicy is a host policy that would waive or loosen the card
// every way it can: skip mode, a routine grant, auto-approve on timeout, a
// one-hour TTL and an external destination.
func generousBuilderPolicy() ActionPolicyEvaluator {
	base := NewDefaultActionPolicy()
	base.Approvers = []ApprovalPrincipal{{Type: "user", ID: "operator"}}
	return ActionPolicyEvaluatorFunc(func(ctx context.Context, input ActionPolicyInput) (ActionPolicyDecision, error) {
		input.ApprovalMode = ConversationApprovalSkip
		input.RoutineGrant = &RoutineGrant{Action: input.Bound.Definition.ID + "." + input.Bound.Action.Name, Label: "Create agents"}
		decision, err := base.EvaluateAction(ctx, input)
		if err != nil {
			return decision, err
		}
		decision.ApprovalTTL = time.Hour
		decision.ApprovalTimeout = ApprovalTimeoutApprove
		decision.ApprovalDestinations = []ApprovalDestination{{EndpointID: "slack-approvals"}}
		return decision, nil
	})
}

func TestAgentBuilderApprovalCardPolicy(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	fixture, _, run, coordinator := agentBuilderApprovalCoordinator(t, generousBuilderPolicy(), now)
	create := agentBuilderCreateArguments()
	create["spaceId"] = fixture.salesID
	cases := []struct {
		action    string
		arguments map[string]interface{}
	}{
		{AgentActionCreateAgent, create},
		{AgentActionUpdateAgent, map[string]interface{}{"agentId": "researcher", "purpose": "Research competitors weekly.", "rationale": "Sharper focus."}},
	}
	for _, tc := range cases {
		result, err := coordinator.Propose(ctx, agentBuilderApprovalRequest(fixture, run, tc.action, tc.arguments, tc.action))
		if err != nil {
			t.Fatalf("%s: %v", tc.action, err)
		}
		approval := result.Approval
		if approval == nil || result.Call.Status != ActionCallStatusWaitingApproval {
			t.Fatalf("%s was not held for review: %#v", tc.action, result)
		}
		if !approval.ExpiresAt.Equal(now.Add(30*24*time.Hour)) || approval.ExpiresAt.Sub(approval.CreatedAt) != agentBuilderApprovalTTL {
			t.Fatalf("%s card expires at %s (created %s)", tc.action, approval.ExpiresAt, approval.CreatedAt)
		}
		if approval.TimeoutDecision != ApprovalTimeoutExpire {
			t.Fatalf("%s timeout decision = %q", tc.action, approval.TimeoutDecision)
		}
		if len(approval.Destinations) != 0 {
			t.Fatalf("%s card was sent to destinations %#v", tc.action, approval.Destinations)
		}
		if approval.ProposedAction["resourceType"] != agentProfileResourceType || approval.ProposedAction["operation"] != tc.action ||
			approval.ProposedAction["review"] != string(skill.ActionReviewAlways) {
			t.Fatalf("%s card preview = %#v", tc.action, approval.ProposedAction)
		}
	}
}

func TestAgentBuilderApprovalCannotBeAllowedByPolicy(t *testing.T) {
	allow := ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionAllow, Reason: "host allows everything"}, nil
	})
	fixture, store, run, coordinator := agentBuilderApprovalCoordinator(t, allow, time.Now().UTC())
	create := agentBuilderCreateArguments()
	create["spaceId"] = fixture.salesID
	_, err := coordinator.Propose(context.Background(), agentBuilderApprovalRequest(fixture, run, AgentActionCreateAgent, create, "allowed"))
	if err == nil || !strings.Contains(err.Error(), "always requires explicit review") {
		t.Fatalf("an allowed create_agent was not refused: %v", err)
	}
	calls, err := store.ListActionCalls(context.Background(), ActionFilter{Scope: fixture.scope, RunID: run.ID})
	if err != nil || len(calls) != 0 {
		t.Fatalf("refused proposal recorded calls %#v, %v", calls, err)
	}
}

func TestAgentBuilderApprovalExpiresAndNeverApproves(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	fixture, store, run, coordinator := agentBuilderApprovalCoordinator(t, generousBuilderPolicy(), now)
	create := agentBuilderCreateArguments()
	create["spaceId"] = fixture.salesID
	result, err := coordinator.Propose(ctx, agentBuilderApprovalRequest(fixture, run, AgentActionCreateAgent, create, "expire"))
	if err != nil {
		t.Fatal(err)
	}
	approval := result.Approval

	early := NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{})
	early.now = func() time.Time { return approval.ExpiresAt.Add(-time.Minute) }
	if _, err := early.ResolveTimeout(ctx, fixture.scope, approval.ID, approval.Revision, approval.ID); err == nil {
		t.Fatal("a card was timed out before its 30 days elapsed")
	}

	late := NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{})
	late.now = func() time.Time { return approval.ExpiresAt.Add(time.Second) }
	resolved, err := late.ResolveTimeout(ctx, fixture.scope, approval.ID, approval.Revision, approval.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Approval.Status != ApprovalStatusExpired || resolved.Call.Status != ActionCallStatusDenied {
		t.Fatalf("an elapsed card did not expire: approval %#v call %#v", resolved.Approval, resolved.Call)
	}
}

func TestAgentBuilderApprovalActionsMatchCreateAndUpdateAndTeam(t *testing.T) {
	bound := func(skillID, action string) *skill.BoundAction {
		return &skill.BoundAction{Definition: &skill.Definition{ID: skillID}, Action: skill.Action{Name: action}}
	}
	for _, tc := range []struct {
		bound *skill.BoundAction
		want  bool
	}{
		{bound(AgentManagementSkillID, AgentActionCreateAgent), true},
		{bound(AgentManagementSkillID, AgentActionUpdateAgent), true},
		{bound(TeamManagementSkillID, "create_team"), true},
		{bound(AgentManagementSkillID, AgentActionTryAgent), false},
		{bound(AgentManagementSkillID, AgentActionListAgents), false},
		{bound(AgentManagementSkillID, AgentActionAmendBehavior), false},
		{bound(TeamManagementSkillID, TeamActionUpdateMember), false},
		{bound("acme.notes", AgentActionCreateAgent), false},
		{nil, false},
	} {
		if got := isAgentBuilderApprovalAction(tc.bound); got != tc.want {
			t.Fatalf("isAgentBuilderApprovalAction(%#v) = %v", tc.bound, got)
		}
	}

	// A create_team card gets the same policy as an agent card.
	coordinator := NewActionCoordinator(newTestKernelStore(t), newTestKernelStore(t), skill.NewCatalog(), NewDefaultActionPolicy())
	coordinator.newID = func() string { return "approval-team" }
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	team := bound(TeamManagementSkillID, "create_team")
	team.Action.Risk, team.Action.Review = skill.RiskLevelWrite, skill.ActionReviewAlways
	call := &ActionCall{ID: "call-team"}
	approval := coordinator.newApprovalCheckpoint(context.Background(),
		ProposeActionRequest{Scope: Scope{Kind: "tenant", ID: "tenant-a"}}, &AgentRun{ID: "run-team"}, team, map[string]interface{}{},
		map[string]interface{}{"resourceType": "team", "operation": "create_team"}, call,
		ActionPolicyDecision{
			Disposition: ActionDispositionRequireApproval, EligibleApprovers: []ApprovalPrincipal{{Type: "user", ID: "operator"}},
			ApprovalTTL: time.Hour, ApprovalTimeout: ApprovalTimeoutApprove, ApprovalDestinations: []ApprovalDestination{{EndpointID: "slack"}},
		}, "Create the team", now)
	if !approval.ExpiresAt.Equal(now.Add(agentBuilderApprovalTTL)) || approval.TimeoutDecision != ApprovalTimeoutExpire || len(approval.Destinations) != 0 {
		t.Fatalf("create_team card = %#v", approval)
	}
}
