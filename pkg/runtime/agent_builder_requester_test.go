package runtime

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// TestAgentBuilderCardIsDecidedByTheRequester: an agent or team card names
// the person who asked the builder as its only approver, whatever role the
// host policy named (D9). Without an acting user the policy's approvers stay.
func TestAgentBuilderCardIsDecidedByTheRequester(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		actingUser string
		want       []ApprovalPrincipal
	}{
		{"local-operator", []ApprovalPrincipal{{Type: "user", ID: "local-operator"}}},
		{"", []ApprovalPrincipal{{Type: "user", ID: "operator"}}},
	} {
		fixture := newAgentBuilderFixture(t, true)
		store := newTestKernelStore(t)
		if _, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
			Scope: fixture.scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "finn"},
			AssignedAgentID: "finn", ActingUser: tc.actingUser, Goal: "Build an inbox helper", Source: RunSourceChat,
		}); err != nil {
			t.Fatal(err)
		}
		run, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{
			Scope: fixture.scope, WorkerID: "conversation-worker", Now: time.Now().UTC(), LeaseDuration: time.Hour, AgingInterval: time.Minute,
		})
		if err != nil || run == nil {
			t.Fatalf("claim = %#v, %v", run, err)
		}
		policy := NewDefaultActionPolicy()
		policy.Approvers = []ApprovalPrincipal{{Type: "user", ID: "operator"}}
		coordinator := NewActionCoordinator(store, store, fixture.catalog, policy, []ActionProposalValidator{fixture.validator})
		create := agentBuilderCreateArguments()
		create["spaceId"] = fixture.salesID
		result, err := coordinator.Propose(ctx, agentBuilderApprovalRequest(fixture, run, AgentActionCreateAgent, create, "requester-"+tc.actingUser))
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval == nil || !reflect.DeepEqual(result.Approval.EligibleApprovers, tc.want) {
			t.Fatalf("acting user %q: approvers = %#v, want %#v", tc.actingUser, result.Approval, tc.want)
		}
	}
}

// TestAgentBuilderConversationNeedsNoParticipationOptIn: a host that leaves
// unconfigured chats inert (the OSS daemon) still answers a builder chat,
// and only a builder chat.
func TestAgentBuilderConversationNeedsNoParticipationOptIn(t *testing.T) {
	builder := ObjectiveOwner{Type: OwnerTypeAgent, ID: AgentBuilderDeploymentID}
	origin := &ConversationReference{Kind: ConversationReferenceAgentBuilder, ID: "request-1"}
	message := &ChannelMessage{Sequence: 1}
	for _, tc := range []struct {
		name         string
		conversation *Conversation
		want         bool
	}{
		{"builder chat", &Conversation{Owner: builder, Origin: origin, Status: ConversationStatusActive}, true},
		{"archived builder chat", &Conversation{Owner: builder, Origin: origin, Status: ConversationStatusArchived}, false},
		{"builder's other chat", &Conversation{Owner: builder, Status: ConversationStatusActive}, false},
		{"another agent's builder-origin chat", &Conversation{Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "helper"}, Origin: origin, Status: ConversationStatusActive}, false},
		{"opted-in chat", &Conversation{Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}, Status: ConversationStatusActive, Participation: &ConversationParticipation{Enabled: true}}, true},
	} {
		if got := conversationParticipationAllows(tc.conversation, message); got != tc.want {
			t.Errorf("%s: allows = %v, want %v", tc.name, got, tc.want)
		}
	}
}
