package runtime

import (
	"testing"
	"time"
)

func TestCollaborationChildrenActForTheSourcePersonAndKeepTheirOwner(t *testing.T) {
	agent := ObjectiveOwner{Type: OwnerTypeAgent, ID: "a"}
	team := ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}
	recipient := CollaborationParty{Type: OwnerTypeAgent, ID: "b"}
	for name, test := range map[string]struct {
		source *AgentRun
		kind   AgentRequestKind
		want   ObjectiveOwner
	}{
		"1:1 request":          {&AgentRun{Kind: RunKindConversation, Owner: agent, AssignedAgentID: "a"}, AgentRequestKindRequest, agent},
		"non-chat hand-off":    {&AgentRun{Kind: RunKindAgentWork, Owner: agent, AssignedAgentID: "a"}, AgentRequestKindHandoff, ObjectiveOwner{Type: OwnerTypeAgent, ID: "b"}},
		"1:1 chat hand-off":    {&AgentRun{Kind: RunKindConversation, Owner: agent, AssignedAgentID: "a"}, AgentRequestKindHandoff, ObjectiveOwner{Type: OwnerTypeAgent, ID: "b"}},
		"Team-chat hand-off":   {&AgentRun{Kind: RunKindConversation, Owner: team, AssignedAgentID: "a"}, AgentRequestKindHandoff, team},
		"Team-chat request":    {&AgentRun{Kind: RunKindConversation, Owner: team, AssignedAgentID: "a"}, AgentRequestKindRequest, team},
		"escalation from work": {&AgentRun{Kind: RunKindAgentWork, Owner: agent, AssignedAgentID: "a"}, AgentRequestKindEscalation, agent},
	} {
		test.source.ID, test.source.ActingUser = "source", "7"
		request := &AgentRequest{ID: "request", Kind: test.kind, Recipient: recipient, AssignedAgentID: "b", Goal: "Help"}
		if owner := collaborationChildOwner(test.source, request); owner != test.want {
			t.Errorf("%s: owner = %#v, want %#v", name, owner, test.want)
		}
		child := buildCollaborationChildRun(test.source, request, time.Now(), "child")
		if child.Owner != test.want || child.ActingUser != "7" {
			t.Errorf("%s: child owner %#v acting %q", name, child.Owner, child.ActingUser)
		}
	}
}
