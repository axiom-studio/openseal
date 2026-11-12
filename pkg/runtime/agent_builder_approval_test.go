package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeaction "github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder/agentbuildertest"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// agentBuilderApprovalCoordinator wires the builder fixture to a kernel store
// with a claimed Finn Run, so proposals take the real coordinator path.
func agentBuilderApprovalCoordinator(t *testing.T, policy runtimeaction.PolicyEvaluator, now time.Time) (*agentbuildertest.Fixture, *sqlite.Store, *models.AgentRun, *runtimeaction.Coordinator) {
	t.Helper()
	ctx := context.Background()
	fixture := agentbuildertest.NewFixture(t, true)
	store := kerneltest.NewStore(t)
	run, err := portfolio.NewService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: fixture.Scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "finn"},
		AssignedAgentID: "finn", Goal: "Build an inbox helper", Source: models.RunSourceChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{
		Scope: fixture.Scope, WorkerID: "conversation-worker", Now: time.Now().UTC(), LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	coordinator := runtimeaction.NewCoordinator(store, store, fixture.Catalog, policy, []runtimeaction.ProposalValidator{fixture.Validator}, models.WithClock(func() time.Time { return now }))
	return fixture, store, claimed, coordinator
}

func agentBuilderApprovalRequest(fixture *agentbuildertest.Fixture, run *models.AgentRun, action string, arguments map[string]interface{}, toolCallID string) runtimeaction.ProposeActionRequest {
	return actiontest.LoopProposeRequest(runtimeaction.ProposeActionRequest{
		Scope: fixture.Scope, RunID: run.ID, WorkerID: "conversation-worker", DeploymentID: "finn",
		SkillID: models.AgentManagementSkillID, SkillVersion: models.AgentManagementSkillVersion, Action: action,
		Arguments: arguments, Summary: "Review the agent",
	}, toolCallID)
}

// generousBuilderPolicy is a host policy that would waive or loosen the card
// every way it can: skip mode, a routine grant, auto-approve on timeout, a
// one-hour TTL and an external destination.
func generousBuilderPolicy() runtimeaction.PolicyEvaluator {
	base := runtimeaction.NewDefaultActionPolicy()
	base.Approvers = []models.ApprovalPrincipal{{Type: "user", ID: "operator"}}
	return runtimeaction.PolicyEvaluatorFunc(func(ctx context.Context, input runtimeaction.PolicyInput) (runtimeaction.PolicyDecision, error) {
		input.ApprovalMode = models.ConversationApprovalSkip
		input.RoutineGrant = &models.RoutineGrant{Action: input.Bound.Definition.ID + "." + input.Bound.Action.Name, Label: "Create agents"}
		decision, err := base.EvaluateAction(ctx, input)
		if err != nil {
			return decision, err
		}
		decision.ApprovalTTL = time.Hour
		decision.ApprovalTimeout = models.ApprovalTimeoutApprove
		decision.ApprovalDestinations = []models.ApprovalDestination{{EndpointID: "slack-approvals"}}
		return decision, nil
	})
}

func TestAgentBuilderApprovalCardPolicy(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	fixture, _, run, coordinator := agentBuilderApprovalCoordinator(t, generousBuilderPolicy(), now)
	create := agentbuildertest.CreateArguments()
	create["spaceId"] = fixture.SalesID
	cases := []struct {
		action    string
		arguments map[string]interface{}
	}{
		{models.AgentActionCreateAgent, create},
		{models.AgentActionUpdateAgent, map[string]interface{}{"agentId": "researcher", "purpose": "Research competitors weekly.", "rationale": "Sharper focus."}},
	}
	for _, tc := range cases {
		result, err := coordinator.Propose(ctx, agentBuilderApprovalRequest(fixture, run, tc.action, tc.arguments, tc.action))
		if err != nil {
			t.Fatalf("%s: %v", tc.action, err)
		}
		approval := result.Approval
		if approval == nil || result.Call.Status != models.ActionCallStatusWaitingApproval {
			t.Fatalf("%s was not held for review: %#v", tc.action, result)
		}
		if !approval.ExpiresAt.Equal(now.Add(30*24*time.Hour)) || approval.ExpiresAt.Sub(approval.CreatedAt) != 30*24*time.Hour {
			t.Fatalf("%s card expires at %s (created %s)", tc.action, approval.ExpiresAt, approval.CreatedAt)
		}
		if approval.TimeoutDecision != models.ApprovalTimeoutExpire {
			t.Fatalf("%s timeout decision = %q", tc.action, approval.TimeoutDecision)
		}
		if len(approval.Destinations) != 0 {
			t.Fatalf("%s card was sent to destinations %#v", tc.action, approval.Destinations)
		}
		if approval.ProposedAction["resourceType"] != models.AgentProfileResourceType || approval.ProposedAction["operation"] != tc.action ||
			approval.ProposedAction["review"] != string(skill.ActionReviewAlways) {
			t.Fatalf("%s card preview = %#v", tc.action, approval.ProposedAction)
		}
	}
}

func TestAgentBuilderApprovalCannotBeAllowedByPolicy(t *testing.T) {
	allow := runtimeaction.PolicyEvaluatorFunc(func(context.Context, runtimeaction.PolicyInput) (runtimeaction.PolicyDecision, error) {
		return runtimeaction.PolicyDecision{Disposition: runtimeaction.DispositionAllow, Reason: "host allows everything"}, nil
	})
	fixture, store, run, coordinator := agentBuilderApprovalCoordinator(t, allow, time.Now().UTC())
	create := agentbuildertest.CreateArguments()
	create["spaceId"] = fixture.SalesID
	_, err := coordinator.Propose(context.Background(), agentBuilderApprovalRequest(fixture, run, models.AgentActionCreateAgent, create, "allowed"))
	if err == nil || !strings.Contains(err.Error(), "always requires explicit review") {
		t.Fatalf("an allowed create_agent was not refused: %v", err)
	}
	calls, err := store.ListActionCalls(context.Background(), models.ActionFilter{Scope: fixture.Scope, RunID: run.ID})
	if err != nil || len(calls) != 0 {
		t.Fatalf("refused proposal recorded calls %#v, %v", calls, err)
	}
}

func TestAgentBuilderApprovalExpiresAndNeverApproves(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	fixture, store, run, coordinator := agentBuilderApprovalCoordinator(t, generousBuilderPolicy(), now)
	create := agentbuildertest.CreateArguments()
	create["spaceId"] = fixture.SalesID
	result, err := coordinator.Propose(ctx, agentBuilderApprovalRequest(fixture, run, models.AgentActionCreateAgent, create, "expire"))
	if err != nil {
		t.Fatal(err)
	}
	approval := result.Approval

	early := runtimeaction.NewApprovalCoordinator(store, store, runtimeaction.EligibleApprovalAuthorizer{}, models.WithClock(func() time.Time { return approval.ExpiresAt.Add(-time.Minute) }))
	if _, err := early.ResolveTimeout(ctx, fixture.Scope, approval.ID, approval.Revision, approval.ID); err == nil {
		t.Fatal("a card was timed out before its 30 days elapsed")
	}

	late := runtimeaction.NewApprovalCoordinator(store, store, runtimeaction.EligibleApprovalAuthorizer{}, models.WithClock(func() time.Time { return approval.ExpiresAt.Add(time.Second) }))
	resolved, err := late.ResolveTimeout(ctx, fixture.Scope, approval.ID, approval.Revision, approval.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Approval.Status != models.ApprovalStatusExpired || resolved.Call.Status != models.ActionCallStatusDenied {
		t.Fatalf("an elapsed card did not expire: approval %#v call %#v", resolved.Approval, resolved.Call)
	}
}
