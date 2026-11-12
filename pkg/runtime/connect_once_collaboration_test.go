package runtime

import (
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

func TestCollaborationChildrenActForTheSourcePersonAndKeepTheirOwner(t *testing.T) {
	agent := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "a"}
	team := models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "crew"}
	recipient := models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "b"}
	for name, test := range map[string]struct {
		source *models.AgentRun
		kind   models.AgentRequestKind
		want   models.ObjectiveOwner
	}{
		"1:1 request":          {&models.AgentRun{Kind: models.RunKindConversation, Owner: agent, AssignedAgentID: "a"}, models.AgentRequestKindRequest, agent},
		"non-chat hand-off":    {&models.AgentRun{Kind: models.RunKindAgentWork, Owner: agent, AssignedAgentID: "a"}, models.AgentRequestKindHandoff, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "b"}},
		"1:1 chat hand-off":    {&models.AgentRun{Kind: models.RunKindConversation, Owner: agent, AssignedAgentID: "a"}, models.AgentRequestKindHandoff, models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "b"}},
		"Team-chat hand-off":   {&models.AgentRun{Kind: models.RunKindConversation, Owner: team, AssignedAgentID: "a"}, models.AgentRequestKindHandoff, team},
		"Team-chat request":    {&models.AgentRun{Kind: models.RunKindConversation, Owner: team, AssignedAgentID: "a"}, models.AgentRequestKindRequest, team},
		"escalation from work": {&models.AgentRun{Kind: models.RunKindAgentWork, Owner: agent, AssignedAgentID: "a"}, models.AgentRequestKindEscalation, agent},
	} {
		test.source.ID, test.source.ActingUser = "source", "7"
		request := &models.AgentRequest{ID: "request", Kind: test.kind, Recipient: recipient, AssignedAgentID: "b", Goal: "Help"}
		if owner := models.CollaborationChildOwner(test.source, request); owner != test.want {
			t.Errorf("%s: owner = %#v, want %#v", name, owner, test.want)
		}
		child := buildCollaborationChildRun(test.source, request, time.Now(), "child")
		if child.Owner != test.want || child.ActingUser != "7" {
			t.Errorf("%s: child owner %#v acting %q", name, child.Owner, child.ActingUser)
		}
	}
}
