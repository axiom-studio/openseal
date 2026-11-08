package runtime

import (
	"context"
	"testing"
	"time"
)

// A Team message routed to another member still replaces the person's pending
// approvals in the chat; those member Runs stop without a reply.
func TestTeamMessageReplacesOtherMembersApprovals(t *testing.T) {
	catalog, scope := cancellationActionCatalog(t)
	f := newConversationActorFixture(t, scope, "unused")
	ctx := t.Context()
	team, _, err := f.service.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "launch-team"}, Title: "Launch", IdempotencyKey: "team-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.conversation = team
	person := ConversationParticipant{Type: ConversationParticipantUser, ID: "member-a"}
	coordinator := NewActionCoordinator(f.store, f.store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionRequireApproval, Reason: "review required", ApprovalTTL: time.Hour, EligibleApprovers: []ApprovalPrincipal{{Type: "role", ID: "reviewer"}}}, nil
	}))
	waiting := func(agentID string, trigger *ChannelMessage) *ActionProposalResult {
		created, err := f.scheduler.runs.CreateAgentRun(ctx, CreateAgentRunRequest{
			Scope: scope, Kind: RunKindConversation, Owner: team.Owner, AssignedAgentID: agentID,
			ConcurrencyKey: conversationRunLane(team.ID, "", agentID), Goal: teamMemberRunGoal, Source: RunSourceChat,
			Context:        map[string]interface{}{conversationRunContextConversationID: team.ID, conversationRunContextTriggerID: trigger.ID, conversationRunContextTriggerSeq: trigger.Sequence},
			IdempotencyKey: "member-" + agentID + "-" + trigger.ID, Actor: ActivityActor{Type: "service", ID: conversationRunSchedulerParticipant},
		})
		if err != nil {
			t.Fatal(err)
		}
		member := newLoopProposalFixture(t, f.store, created.Run, "worker-"+agentID, time.Now().UTC())
		proposal, err := coordinator.Propose(ctx, member.request(ProposeActionRequest{
			DeploymentID: "research-agent", SkillID: "forum", SkillVersion: "1.0.0",
			Action: "reply", Arguments: map[string]interface{}{"body": "Launch moved to Thursday"}, Summary: "Post in #launch",
		}, "post-"+agentID))
		if err != nil || proposal.Approval == nil || proposal.Call.Status != ActionCallStatusWaitingApproval {
			t.Fatalf("pending approval = %#v, %v", proposal, err)
		}
		member.assertUntouched()
		member.parkOnApproval(proposal.Approval.ID)
		return proposal
	}
	ask := f.post(t, PostChannelMessageRequest{Sender: person, IdempotencyKey: "ask"})
	nova := waiting("nova", ask)
	other := f.post(t, PostChannelMessageRequest{Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "member-b"}, IdempotencyKey: "ask-b"})
	someoneElses := waiting("rex", other)
	next := f.post(t, PostChannelMessageRequest{Sender: person, IdempotencyKey: "next", Content: "connect my Google account first"})

	// Routed to iris; nova's approval is the person's, rex's is someone else's.
	if err := f.scheduler.replaceOtherMemberApprovals(ctx, team, next, "", map[string]bool{"iris": true}); err != nil {
		t.Fatal(err)
	}
	if approval, err := f.store.GetApproval(ctx, scope, nova.Approval.ID); err != nil || approval.Status != ApprovalStatusSuperseded || approval.SupersededByMessageID != next.ID {
		t.Fatalf("nova's approval = %#v, %v", approval, err)
	}
	if approval, err := f.store.GetApproval(ctx, scope, someoneElses.Approval.ID); err != nil || approval.Status != ApprovalStatusPending {
		t.Fatalf("another person's approval changed: %#v, %v", approval, err)
	}
	// nova's parked Run is queued to see its superseded call; it is not
	// handed the message as a follow-up.
	run, err := f.store.GetAgentRun(ctx, scope, nova.Call.RunID)
	if err != nil || run.Status != AgentRunStatusQueued || conversationRunHasFollowUp(run, next.ID) {
		t.Fatalf("nova's Run = %#v, %v", run, err)
	}
	if call, err := f.store.GetActionCall(ctx, scope, nova.Call.ID); err != nil || call.Status != ActionCallStatusDenied || call.CompletedAt == nil {
		t.Fatalf("nova's superseded call = %#v, %v", call, err)
	}

	// A routed member is left to the normal follow-up path.
	again := f.post(t, PostChannelMessageRequest{Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "member-b"}, IdempotencyKey: "again"})
	if err := f.scheduler.replaceOtherMemberApprovals(ctx, team, again, "", map[string]bool{"rex": true}); err != nil {
		t.Fatal(err)
	}
	if approval, _ := f.store.GetApproval(ctx, scope, someoneElses.Approval.ID); approval.Status != ApprovalStatusPending {
		t.Fatalf("routed member's approval changed: %#v", approval)
	}
}
