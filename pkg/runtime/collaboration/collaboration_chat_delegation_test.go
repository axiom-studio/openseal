package collaboration

import (
	"context"
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
)

// A Team chat hands work only to the Team or an Agent on its active roster;
// an Agent's 1:1 chat hands work to no one.
func TestChatDelegationStaysOnTheRoster(t *testing.T) {
	ctx, scope := context.Background(), models.Scope{Kind: "tenant", ID: "roster"}
	service := &Service{teams: collaborationRosterTeams{team: collaborationRosterTeam(scope, "marlow", "harp")}}
	source := &models.AgentRun{ID: "marlow-run", Scope: scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "crew"}, AssignedAgentID: "marlow"}
	for _, recipient := range []models.CollaborationParty{{Type: models.OwnerTypeAgent, ID: "harp"}, {Type: models.OwnerTypeTeam, ID: "crew"}} {
		if err := service.validateChatDelegation(ctx, source, recipient); err != nil {
			t.Fatalf("hand-off to %v = %v", recipient, err)
		}
	}
	for _, recipient := range []models.CollaborationParty{{Type: models.OwnerTypeAgent, ID: "outsider"}, {Type: models.OwnerTypeTeam, ID: "other-team"}} {
		if err := service.validateChatDelegation(ctx, source, recipient); !errors.Is(err, ErrAgentRequestUnauthorized) {
			t.Fatalf("hand-off to %v = %v", recipient, err)
		}
	}
	if err := (&Service{}).validateChatDelegation(ctx, source, models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "harp"}); !errors.Is(err, ErrAgentRequestUnauthorized) {
		t.Fatalf("hand-off without a Team roster = %v", err)
	}
	own := &models.AgentRun{ID: "own-run", Scope: scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "own"}, AssignedAgentID: "own"}
	if err := service.validateChatDelegation(ctx, own, models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "harp"}); !errors.Is(err, models.ErrInvalidAgentRequestState) {
		t.Fatalf("1:1 hand-off = %v", err)
	}
	work := &models.AgentRun{ID: "work", Scope: scope, Kind: models.RunKindAgentWork, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "own"}, AssignedAgentID: "own"}
	if err := service.validateChatDelegation(ctx, work, models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "outsider"}); err != nil {
		t.Fatalf("non-chat delegation = %v", err)
	}
}
