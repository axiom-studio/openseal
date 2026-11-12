package openseal

import (
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/runtimetest"
)

// A conversation's requests are listed by the umbrella that owns it: the
// Team for a Team chat, whichever member asked, and the Agent for its 1:1
// chat. Nobody else lists them, not even a Team member under its own Agent.
func TestRequestsAreListedByTheConversationUmbrella(t *testing.T) {
	ctx := t.Context()
	store := runtimetest.NewKernelStore(t)
	engine, err := New(WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	scope := models.Scope{Kind: "tenant", ID: "roster"}
	seedSpace(t, engine, scope, "operator")
	catalogScope := capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	definition, err := engine.RegisterAgentDefinition(ctx, &AgentDefinition{
		ID: "helper", Version: "1", DisplayName: "Helper", Purpose: "Help", SystemPrompt: "Help.",
		Authority: AgentAuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"member", "outsider"} {
		if _, _, err := engine.CreateAgentDeployment(ctx, &AgentDeployment{
			ID: id, Scope: catalogScope, DefinitionID: definition.ID, ActiveVersion: definition.Version,
			RolloutStatus: AgentRolloutActive, Environment: "test", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 1},
		}, "user", "operator", "create"); err != nil {
			t.Fatal(err)
		}
	}
	team, err := engine.RegisterTeamDefinition(ctx, &TeamDefinition{
		ID: "crew", Version: "1", DisplayName: "Crew", Purpose: "Work together",
		Approvals: TeamApprovalPolicy{MaximumRisk: capability.RiskLevelRead},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.CreateTeamDeployment(ctx, &TeamDeployment{
		ID: "crew-one", Scope: catalogScope, DefinitionID: team.ID, ActiveVersion: team.Version, Status: TeamDeploymentActive,
		Roster: []TeamRosterAssignment{{ID: "helper", AgentDeploymentID: "member"}},
	}, "user", "operator", "create"); err != nil {
		t.Fatal(err)
	}
	for _, chat := range []struct {
		id        string
		ownerType models.OwnerType
		ownerID   string
	}{{"team-chat", OwnerTypeTeam, "crew-one"}, {"own-chat", OwnerTypeAgent, "member"}} {
		if _, _, err := engine.CreateConversation(ctx, CreateConversationRequest{ID: chat.id, Scope: scope, Owner: ObjectiveOwner{Type: chat.ownerType, ID: chat.ownerID}, Title: chat.id, IdempotencyKey: chat.id}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	crew := ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew-one"}
	for chat, owner := range map[string]ObjectiveOwner{"team-chat": crew, "own-chat": agentOwner("member")} {
		if err := store.SaveSkillSetupRequest(ctx, &SkillSetupRequest{ID: "setup-" + chat, Scope: scope, Owner: owner, DeploymentID: owner.ID, AgentID: "member",
			ConversationID: chat, TriggerMessageID: "ask", RunID: "run", ActionCallID: "call-" + chat, Kind: "configure",
			SkillID: "vercel.reader", SkillVersion: "1.0.0", SkillName: "Vercel", Reason: "Connect Vercel", Status: "pending", Revision: 1, CreatedAt: now, UpdatedAt: now}, 0); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveCredentialRequest(ctx, &models.CredentialRequest{ID: "credential-" + chat, Scope: scope, Owner: owner, DeploymentID: owner.ID, AgentID: "member",
			ConversationID: chat, TriggerMessageID: "ask", RunID: "run", ActionCallID: "secret-" + chat, Kind: models.CredentialRequestKindPaymentCard, Reason: "Pay",
			Status: models.CredentialRequestStatusPending, Revision: 1, CreatedAt: now, UpdatedAt: now}, 0); err != nil {
			t.Fatal(err)
		}
	}
	for chat, owner := range map[string]ObjectiveOwner{"team-chat": crew, "own-chat": agentOwner("member")} {
		setups, err := engine.ListSkillSetupRequests(ctx, scope, owner, chat)
		if err != nil || len(setups) != 1 || setups[0].ID != "setup-"+chat {
			t.Fatalf("%s setups = %#v, %v", chat, setups, err)
		}
		credentials, err := engine.ListCredentialRequests(ctx, scope, owner, chat)
		if err != nil || len(credentials) != 1 || credentials[0].ID != "credential-"+chat {
			t.Fatalf("%s credentials = %#v, %v", chat, credentials, err)
		}
		if _, err := engine.GetSkillSetupRequest(ctx, scope, owner, "setup-"+chat); err != nil {
			t.Fatalf("%s get: %v", chat, err)
		}
	}
	for chat, outsider := range map[string]ObjectiveOwner{"team-chat": agentOwner("member"), "own-chat": crew} {
		if _, err := engine.ListSkillSetupRequests(ctx, scope, outsider, chat); !errors.Is(err, models.ErrConversationNotFound) {
			t.Fatalf("%s outsider setups err = %v", chat, err)
		}
		if _, err := engine.ListCredentialRequests(ctx, scope, outsider, chat); !errors.Is(err, models.ErrConversationNotFound) {
			t.Fatalf("%s outsider credentials err = %v", chat, err)
		}
		if _, err := engine.GetSkillSetupRequest(ctx, scope, outsider, "setup-"+chat); err == nil {
			t.Fatalf("%s request read under another umbrella", chat)
		}
	}
}
