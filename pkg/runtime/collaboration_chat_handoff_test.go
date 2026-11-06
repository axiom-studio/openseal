package runtime

import (
	"errors"
	"testing"
	"time"

	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// Work a Team member hands to a teammate in the chat is answered in that same
// chat: an ordinary chat Run in the teammate's own lane with the chat's ids.
func TestCollaborationChildAnswersInTheSameChat(t *testing.T) {
	now := time.Now().UTC()
	scope := Scope{Kind: "tenant", ID: "handoff"}
	source := &AgentRun{ID: "marlow-run", RootRunID: "marlow-run", Kind: RunKindConversation, Scope: scope,
		Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "gtm"}, AssignedAgentID: "marlow",
		ConcurrencyKey: conversationRunLane("chat", "root", "marlow"), Source: RunSourceChat,
		Context: map[string]interface{}{conversationRunContextConversationID: "chat", conversationRunContextTriggerID: "ask", "threadRootMessageId": "root"}}
	for _, kind := range []AgentRequestKind{AgentRequestKindRequest, AgentRequestKindHandoff} {
		request := &AgentRequest{ID: "request", Kind: kind, Goal: "Suggest one launch step",
			Requester: CollaborationParty{Type: OwnerTypeTeam, ID: "gtm"}, Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "harp"}, AssignedAgentID: "harp"}
		child := buildCollaborationChildRun(source, request, now, "harp-run")
		// A hand-off gives the work to its recipient member; the chat keeps it.
		if child.Owner != source.Owner || child.AssignedAgentID != "harp" {
			t.Fatalf("%s child owner = %v, Agent %q", kind, child.Owner, child.AssignedAgentID)
		}
		if child.Kind != RunKindConversation || child.ConcurrencyKey != conversationRunLane("chat", "root", "harp") {
			t.Fatalf("%s child = %s %q", kind, child.Kind, child.ConcurrencyKey)
		}
		if err := validateConversationRun(child); err != nil {
			t.Fatalf("%s child is not a valid chat Run: %v", kind, err)
		}
		if child.Context[conversationRunContextTriggerID] != "ask" || child.Context["threadRootMessageId"] != "root" {
			t.Fatalf("%s child context = %#v", kind, child.Context)
		}
		if assignment := conversationRunAssignment(child); assignment == nil || assignment.From != "marlow" || assignment.Work != "Suggest one launch step" {
			t.Fatalf("%s assignment = %#v", kind, assignment)
		}
	}
	if conversationRunAssignment(source) != nil {
		t.Fatal("a chat message is not handed-over work")
	}
}

// An Agent's 1:1 chat belongs to that Agent alone, so it cannot hand work to
// another Agent.
func TestOneToOneChatCannotHandWorkToAnotherAgent(t *testing.T) {
	source := &AgentRun{ID: "own-run", Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "own"}, AssignedAgentID: "own"}
	if err := validateChatDelegationSource(source); !errors.Is(err, ErrInvalidAgentRequestState) {
		t.Fatalf("1:1 chat hand-off = %v", err)
	}
	team := &AgentRun{ID: "team-run", Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "gtm"}, AssignedAgentID: "marlow"}
	if err := validateChatDelegationSource(team); err != nil {
		t.Fatalf("Team chat hand-off = %v", err)
	}
	if err := validateChatDelegationSource(&AgentRun{ID: "work", Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "own"}}); err != nil {
		t.Fatalf("Agent work hand-off = %v", err)
	}
}

// A Team chat hand-off is checked against the roster again when it is
// accepted: the child answers in the chat, so a removed member cannot take it.
func TestTeamChatHandOffAcceptanceRechecksTheRoster(t *testing.T) {
	ctx, store, scope := t.Context(), NewMemoryStore(), Scope{Kind: "tenant", ID: "handoff-roster"}
	owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "gtm"}
	team := activeCollaborationTeam(scope, owner.ID,
		kernelteam.RosterAssignment{ID: "marlow", AgentDeploymentID: "marlow"}, kernelteam.RosterAssignment{ID: "harp", AgentDeploymentID: "harp"})
	team.deployment.DefinitionID, team.deployment.ActiveVersion = owner.ID, "1"
	team.definition = &kernelteam.Definition{ID: owner.ID, Version: "1", Delegation: kernelteam.DelegationPolicy{
		MaximumDepth: 3, MaximumConcurrent: 3, AllowPeerDelegation: true, RequireAcceptance: true,
	}}
	service := NewCollaborationService(store)
	service.teams = team
	source, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Kind: RunKindConversation, Owner: owner, AssignedAgentID: "marlow", Goal: "Answer", Source: RunSourceChat,
		ConcurrencyKey: conversationRunLane("chat", "", "marlow"),
		Context:        map[string]interface{}{conversationRunContextConversationID: "chat", conversationRunContextTriggerID: "ask"},
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindHandoff, SourceRunID: source.ID,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "marlow"}, Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "harp"},
		Goal: "Draft the launch post", IdempotencyKey: "handoff",
	})
	if err != nil {
		t.Fatal(err)
	}
	accept := RespondAgentRequestRequest{Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: created.Request.Recipient}
	roster := team.deployment.Roster
	team.deployment.Roster = roster[:1]
	if _, err := service.RespondAgentRequest(ctx, accept); !errors.Is(err, ErrAgentRequestUnauthorized) {
		t.Fatalf("removed member accepted the hand-off: %v", err)
	}
	team.deployment.Roster = roster
	accepted, err := service.RespondAgentRequest(ctx, accept)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Child.Owner != owner || accepted.Child.AssignedAgentID != "harp" {
		t.Fatalf("hand-off child = %v / %q", accepted.Child.Owner, accepted.Child.AssignedAgentID)
	}
}
