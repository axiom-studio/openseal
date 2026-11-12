package runtime

import (
	"context"
	"errors"
	"testing"
)

// A Team chat hands work only to the Team or an Agent on its active roster;
// an Agent's 1:1 chat hands work to no one.
func TestChatDelegationStaysOnTheRoster(t *testing.T) {
	ctx, scope := context.Background(), Scope{Kind: "tenant", ID: "roster"}
	service := &CollaborationService{teams: chatRosterTeams{team: chatRosterTeam(scope, "marlow", "harp")}}
	source := &AgentRun{ID: "marlow-run", Scope: scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}, AssignedAgentID: "marlow"}
	for _, recipient := range []CollaborationParty{{Type: OwnerTypeAgent, ID: "harp"}, {Type: OwnerTypeTeam, ID: "crew"}} {
		if err := service.validateChatDelegation(ctx, source, recipient); err != nil {
			t.Fatalf("hand-off to %v = %v", recipient, err)
		}
	}
	for _, recipient := range []CollaborationParty{{Type: OwnerTypeAgent, ID: "outsider"}, {Type: OwnerTypeTeam, ID: "other-team"}} {
		if err := service.validateChatDelegation(ctx, source, recipient); !errors.Is(err, ErrAgentRequestUnauthorized) {
			t.Fatalf("hand-off to %v = %v", recipient, err)
		}
	}
	if err := (&CollaborationService{}).validateChatDelegation(ctx, source, CollaborationParty{Type: OwnerTypeAgent, ID: "harp"}); !errors.Is(err, ErrAgentRequestUnauthorized) {
		t.Fatalf("hand-off without a Team roster = %v", err)
	}
	own := &AgentRun{ID: "own-run", Scope: scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "own"}, AssignedAgentID: "own"}
	if err := service.validateChatDelegation(ctx, own, CollaborationParty{Type: OwnerTypeAgent, ID: "harp"}); !errors.Is(err, ErrInvalidAgentRequestState) {
		t.Fatalf("1:1 hand-off = %v", err)
	}
	work := &AgentRun{ID: "work", Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "own"}, AssignedAgentID: "own"}
	if err := service.validateChatDelegation(ctx, work, CollaborationParty{Type: OwnerTypeAgent, ID: "outsider"}); err != nil {
		t.Fatalf("non-chat delegation = %v", err)
	}
}
