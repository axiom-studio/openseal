package collaboration

import (
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// An Agent's 1:1 chat belongs to that Agent alone, so it cannot hand work to
// another Agent.
func TestOneToOneChatCannotHandWorkToAnotherAgent(t *testing.T) {
	source := &models.AgentRun{ID: "own-run", Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "own"}, AssignedAgentID: "own"}
	if err := validateChatDelegationSource(source); !errors.Is(err, models.ErrInvalidAgentRequestState) {
		t.Fatalf("1:1 chat hand-off = %v", err)
	}
	team := &models.AgentRun{ID: "team-run", Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "gtm"}, AssignedAgentID: "marlow"}
	if err := validateChatDelegationSource(team); err != nil {
		t.Fatalf("Team chat hand-off = %v", err)
	}
	if err := validateChatDelegationSource(&models.AgentRun{ID: "work", Kind: models.RunKindAgentWork, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "own"}}); err != nil {
		t.Fatalf("Agent work hand-off = %v", err)
	}
}

// A Team chat hand-off is checked against the roster again when it is
// accepted: the child answers in the chat, so a removed member cannot take it.
func TestTeamChatHandOffAcceptanceRechecksTheRoster(t *testing.T) {
	ctx, store, scope := t.Context(), kerneltest.NewStore(t), models.Scope{Kind: "tenant", ID: "handoff-roster"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "gtm"}
	team := activeCollaborationTeam(scope, owner.ID,
		kernelteam.RosterAssignment{ID: "marlow", AgentDeploymentID: "marlow"}, kernelteam.RosterAssignment{ID: "harp", AgentDeploymentID: "harp"})
	team.deployment.DefinitionID, team.deployment.ActiveVersion = owner.ID, "1"
	team.definition = &kernelteam.Definition{ID: owner.ID, Version: "1", Delegation: kernelteam.DelegationPolicy{
		MaximumDepth: 3, MaximumConcurrent: 3, AllowPeerDelegation: true, RequireAcceptance: true,
	}}
	service := NewCollaborationService(store)
	service.teams = team
	source, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Kind: models.RunKindConversation, Owner: owner, AssignedAgentID: "marlow", Goal: "Answer", Source: models.RunSourceChat,
		ConcurrencyKey: models.ConversationRunLane("chat", "", "marlow"),
		Context:        map[string]interface{}{models.ConversationRunConversationIDContextKey: "chat", models.ConversationRunTriggerIDContextKey: "ask"},
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindHandoff, SourceRunID: source.ID,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "marlow"}, Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "harp"},
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
