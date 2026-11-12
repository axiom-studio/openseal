package conversation

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

// teamRoutingAgent copies conversationtest.AgentParticipant for the
// in-package tests.
func teamRoutingAgent(id string) models.ConversationParticipant {
	return models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: id}
}

func TestTeamRoutingAddressingOrder(t *testing.T) {
	store := kerneltest.NewStore(t)
	service := NewConversationService(store)
	seedTestSpace(t, store, models.Scope{Kind: "tenant", ID: "routing"})
	conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{
		Scope: models.Scope{Kind: "tenant", ID: "routing"}, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "launch"},
		Title: "Launch", IdempotencyKey: "launch",
	})
	if err != nil {
		t.Fatal(err)
	}
	participants := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
		return []ConversationParticipantBinding{
			{Participant: teamRoutingAgent("maya"), SemanticRoles: []string{"pricing"}},
			{Participant: teamRoutingAgent("otto"), SemanticRoles: []string{"press"}},
			{Participant: teamRoutingAgent("rio"), SemanticRoles: []string{"writer"}},
		}, nil
	})
	var routed []ConversationRouteRequest
	routerErr := error(nil)
	router := ConversationRouterFunc(func(_ context.Context, req ConversationRouteRequest) (ConversationRouteDecision, error) {
		routed = append(routed, req)
		return ConversationRouteDecision{AgentID: "rio", Reason: "  owns the\nlaunch copy "}, routerErr
	})
	coordinator, err := NewConversationCoordinator(service, participants, router, DefaultConversationCoordinatorConfig())
	if err != nil {
		t.Fatal(err)
	}
	post := func(key string, req PostChannelMessageRequest) *models.ChannelMessage {
		req.IdempotencyKey = key
		return threadStartPost(t, service, conversation, req)
	}
	route := func(trigger *models.ChannelMessage) []models.ConversationResponder {
		t.Helper()
		result, err := coordinator.Coordinate(t.Context(), ConversationRoutingRequest{
			Scope: conversation.Scope, ConversationID: conversation.ID, TriggerMessageID: trigger.ID, IdempotencyKey: "route:" + trigger.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result.Round.Responders
	}
	want := func(name string, got []models.ConversationResponder, expected ...models.ConversationResponder) {
		t.Helper()
		if len(expected) == 0 {
			expected = nil
		}
		if len(got) == 0 {
			got = nil
		}
		if !reflect.DeepEqual(got, expected) {
			t.Fatalf("%s: responders = %#v, want %#v", name, got, expected)
		}
	}
	mention := func(id string) models.ConversationResponder {
		return models.ConversationResponder{Participant: teamRoutingAgent(id), Addressed: models.ConversationAddressedMention}
	}

	// Mentions win over @everyone and are answered in mention order.
	mentioned := post("mentions", PostChannelMessageRequest{
		Mentions: []models.ConversationParticipant{teamRoutingAgent("otto"), teamRoutingAgent("maya"), teamRoutingAgent("stranger")},
		Audience: models.ConversationAudience{Kind: models.ConversationAudienceEveryone},
	})
	want("mentions", route(mentioned), mention("otto"), mention("maya"))

	everyone := post("everyone", PostChannelMessageRequest{Audience: models.ConversationAudience{Kind: models.ConversationAudienceEveryone}})
	everyoneResponders := route(everyone)
	if len(everyoneResponders) != 3 || everyoneResponders[0].Addressed != models.ConversationAddressedEveryone {
		t.Fatalf("everyone = %#v", everyoneResponders)
	}

	answer := post("otto-answer", PostChannelMessageRequest{Sender: teamRoutingAgent("otto"), Intent: models.MessageIntentAnswer, ReplyToMessageID: everyone.ID})
	reply := post("reply", PostChannelMessageRequest{QuotedMessageID: answer.ID})
	want("quoted agent", route(reply), models.ConversationResponder{Participant: teamRoutingAgent("otto"), Addressed: models.ConversationAddressedReply})

	byRole := post("role", PostChannelMessageRequest{Audience: models.ConversationAudience{Kind: models.ConversationAudienceRoles, Roles: []string{"Pricing"}}})
	want("role", route(byRole), models.ConversationResponder{Participant: teamRoutingAgent("maya"), Addressed: models.ConversationAddressedRole})
	if len(routed) != 0 {
		t.Fatalf("addressed messages called the router %d times", len(routed))
	}

	plain := post("plain", PostChannelMessageRequest{Content: "Can someone draft the launch post?"})
	want("routed", route(plain), models.ConversationResponder{Participant: teamRoutingAgent("rio"), Addressed: models.ConversationAddressedRouted, Reason: "owns the launch copy"})
	if len(routed) != 1 || len(routed[0].Members) != 3 || routed[0].Trigger.ID != plain.ID || routed[0].LastAgentWithUser != "otto" ||
		len(routed[0].Recent) == 0 || routed[0].Recent[len(routed[0].Recent)-1].Sequence >= plain.Sequence {
		t.Fatalf("route request = %#v", routed)
	}
	// Replays never call the router again.
	want("replay", route(plain), models.ConversationResponder{Participant: teamRoutingAgent("rio"), Addressed: models.ConversationAddressedRouted, Reason: "owns the launch copy"})
	if len(routed) != 1 {
		t.Fatal("a replayed routing decision called the router")
	}

	// A router failure falls back to who the user was just talking to.
	routerErr = errors.New("router down")
	failed := post("failed", PostChannelMessageRequest{Content: "And the press list?"})
	want("fallback", route(failed), models.ConversationResponder{Participant: teamRoutingAgent("otto"), Addressed: models.ConversationAddressedRouted, Reason: "was just talking with you"})
	routerErr = nil

	// An Agent's handoff is addressed only by its mentions, never routed or
	// fanned out, and never back to itself.
	handoff := post("handoff", PostChannelMessageRequest{
		Sender: teamRoutingAgent("rio"), Intent: models.MessageIntentAnswer,
		Mentions: []models.ConversationParticipant{teamRoutingAgent("rio"), teamRoutingAgent("maya")},
	})
	want("handoff", route(handoff), mention("maya"))
	silent := post("agent-update", PostChannelMessageRequest{Sender: teamRoutingAgent("rio"), Intent: models.MessageIntentUpdate, Audience: models.ConversationAudience{Kind: models.ConversationAudienceEveryone}})
	want("agent without mentions", route(silent))
	if len(routed) != 2 {
		t.Fatalf("Agent messages called the router: %d", len(routed))
	}
}

func TestTeamRoutingFallbackOrder(t *testing.T) {
	members := []RouteMember{{AgentID: "maya", Busy: true}, {AgentID: "otto"}, {AgentID: "rio"}}
	for _, tc := range []struct {
		name, last, lead, want string
		members                []RouteMember
	}{
		{"last with user", "rio", "maya", "rio", members},
		{"lead", "", "maya", "maya", members},
		{"first free", "", "", "otto", members},
		{"everyone busy", "", "", "maya", []RouteMember{{AgentID: "maya", Busy: true}, {AgentID: "otto", Busy: true}}},
	} {
		got := fallbackConversationResponder(ConversationRouteRequest{LastAgentWithUser: tc.last, Lead: tc.lead, Members: tc.members})
		if got.Participant.ID != tc.want || got.Addressed != models.ConversationAddressedRouted || got.Validate() != nil {
			t.Fatalf("%s: %#v", tc.name, got)
		}
	}
}

func TestFindHandoffMentions(t *testing.T) {
	roster := ConversationTeamRoster{members: map[string]conversationTeamMember{
		"maya": {name: "Maya"}, "otto": {name: "Otto Bell"}, "rio": {name: "Rio"},
	}}
	bindings := []ConversationParticipantBinding{{Participant: teamRoutingAgent("maya")}, {Participant: teamRoutingAgent("otto")}, {Participant: teamRoutingAgent("rio")}}
	got := FindHandoffMentions("Draft is up. @otto bell can you check the list, then @Maya the pricing? @Riot and @everyone ignored.", "rio", roster, bindings)
	if !reflect.DeepEqual(got, []models.ConversationParticipant{teamRoutingAgent("otto"), teamRoutingAgent("maya")}) {
		t.Fatalf("mentions = %#v", got)
	}
	if got := FindHandoffMentions("Thanks @Rio!", "rio", roster, bindings); got != nil {
		t.Fatalf("self mention = %#v", got)
	}
}

// The router's view of a Team comes from its members: the one marked lead,
// and what each does on the Team.
func TestConversationTeamRosterReadsLeadAndPurposeFromMembers(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	if err := store.CreateTeamDefinition(ctx, &kernelteam.Definition{
		ID: "launch", Version: "1", DisplayName: "Launch", Purpose: "Ship", Routing: kernelteam.RoutingPolicy{Notes: "Maya owns pricing"},
		Approvals: kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTeamDeployment(ctx, &kernelteam.Deployment{
		ID: "launch", Scope: scope, DefinitionID: "launch", ActiveVersion: "1", Status: kernelteam.DeploymentActive, Revision: 1,
		Roster: []kernelteam.RosterAssignment{
			{ID: "lead", AgentDeploymentID: "otto", DisplayName: "Otto", Purpose: "Owns press"},
			{ID: "maya-2", AgentDeploymentID: "maya", DisplayName: "Maya", Purpose: "Owns pricing", Lead: true, Owns: []string{"pricing"}},
		},
	}, workforce.DefinitionActivation{ID: "created"}); err != nil {
		t.Fatal(err)
	}
	roster := LoadConversationTeamRoster(ctx, store, &models.Conversation{Scope: models.Scope{Kind: "tenant", ID: "one"}, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "launch"}})
	if roster.lead != "maya" || roster.notes != "Maya owns pricing" ||
		!reflect.DeepEqual(roster.members["maya"], conversationTeamMember{name: "Maya", role: "Owns pricing", owns: []string{"pricing"}}) ||
		roster.members["otto"].role != "Owns press" {
		t.Fatalf("roster = %+v", roster)
	}
}
