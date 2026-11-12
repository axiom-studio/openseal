package conversationrun_test

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

// A Team message routed to another member still replaces the person's pending
// approvals in the chat; those member Runs stop without a reply.
func TestTeamMessageReplacesOtherMembersApprovals(t *testing.T) {
	catalog, scope := cancellationActionCatalog(t)
	f := newConversationActorFixture(t, scope, "unused")
	ctx := t.Context()
	team, _, err := f.service.CreateConversation(ctx, conversation.CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "launch-team"}, Title: "Launch", IdempotencyKey: "team-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.conversation = team
	person := models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "member-a"}
	coordinator := action.NewCoordinator(f.store, f.store, catalog, action.PolicyEvaluatorFunc(func(context.Context, action.PolicyInput) (action.PolicyDecision, error) {
		return action.PolicyDecision{Disposition: action.DispositionRequireApproval, Reason: "review required", ApprovalTTL: time.Hour, EligibleApprovers: []models.ApprovalPrincipal{{Type: "role", ID: "reviewer"}}}, nil
	}), nil)
	waiting := func(agentID string, trigger *models.ChannelMessage) *models.ActionProposalResult {
		created, err := f.scheduler.Runs().CreateAgentRun(ctx, models.CreateAgentRunRequest{
			Scope: scope, Kind: models.RunKindConversation, Owner: team.Owner, AssignedAgentID: agentID,
			ConcurrencyKey: models.ConversationRunLane(team.ID, "", agentID), Goal: "Respond to a Team channel message", Source: models.RunSourceChat,
			Context:        map[string]interface{}{models.ConversationRunConversationIDContextKey: team.ID, models.ConversationRunTriggerIDContextKey: trigger.ID, conversation.RunContextTriggerSeq: trigger.Sequence},
			IdempotencyKey: "member-" + agentID + "-" + trigger.ID, Actor: models.ActivityActor{Type: "service", ID: models.ConversationRunSchedulerParticipant},
		})
		if err != nil {
			t.Fatal(err)
		}
		member := actiontest.NewLoopProposalFixture(t, f.store, created.Run, "worker-"+agentID, time.Now().UTC())
		proposal, err := coordinator.Propose(ctx, member.Request(action.ProposeActionRequest{
			DeploymentID: "research-agent", SkillID: "forum", SkillVersion: "1.0.0",
			Action: "reply", Arguments: map[string]interface{}{"body": "Launch moved to Thursday"}, Summary: "Post in #launch",
		}, "post-"+agentID))
		if err != nil || proposal.Approval == nil || proposal.Call.Status != models.ActionCallStatusWaitingApproval {
			t.Fatalf("pending approval = %#v, %v", proposal, err)
		}
		member.AssertUntouched()
		member.ParkOnApproval(proposal.Approval.ID)
		return proposal
	}
	ask := f.post(t, conversation.PostChannelMessageRequest{Sender: person, IdempotencyKey: "ask"})
	nova := waiting("nova", ask)
	other := f.post(t, conversation.PostChannelMessageRequest{Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "member-b"}, IdempotencyKey: "ask-b"})
	someoneElses := waiting("rex", other)
	next := f.post(t, conversation.PostChannelMessageRequest{Sender: person, IdempotencyKey: "next", Content: "connect my Google account first"})

	// Routed to iris; nova's approval is the person's, rex's is someone else's.
	if err := f.scheduler.ReplaceOtherMemberApprovals(ctx, team, next, "", map[string]bool{"iris": true}); err != nil {
		t.Fatal(err)
	}
	if approval, err := f.store.GetApproval(ctx, scope, nova.Approval.ID); err != nil || approval.Status != models.ApprovalStatusSuperseded || approval.SupersededByMessageID != next.ID {
		t.Fatalf("nova's approval = %#v, %v", approval, err)
	}
	if approval, err := f.store.GetApproval(ctx, scope, someoneElses.Approval.ID); err != nil || approval.Status != models.ApprovalStatusPending {
		t.Fatalf("another person's approval changed: %#v, %v", approval, err)
	}
	// nova's parked Run is queued to see its superseded call; it is not
	// handed the message as a follow-up.
	run, err := f.store.GetAgentRun(ctx, scope, nova.Call.RunID)
	if err != nil || run.Status != models.AgentRunStatusQueued || runHasFollowUp(run, next.ID) {
		t.Fatalf("nova's Run = %#v, %v", run, err)
	}
	if call, err := f.store.GetActionCall(ctx, scope, nova.Call.ID); err != nil || call.Status != models.ActionCallStatusDenied || call.CompletedAt == nil {
		t.Fatalf("nova's superseded call = %#v, %v", call, err)
	}

	// A routed member is left to the normal follow-up path.
	again := f.post(t, conversation.PostChannelMessageRequest{Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "member-b"}, IdempotencyKey: "again"})
	if err := f.scheduler.ReplaceOtherMemberApprovals(ctx, team, again, "", map[string]bool{"rex": true}); err != nil {
		t.Fatal(err)
	}
	if approval, _ := f.store.GetApproval(ctx, scope, someoneElses.Approval.ID); approval.Status != models.ApprovalStatusPending {
		t.Fatalf("routed member's approval changed: %#v", approval)
	}
}
