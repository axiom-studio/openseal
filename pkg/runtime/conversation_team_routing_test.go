package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func teamRoutingAgent(id string) ConversationParticipant {
	return ConversationParticipant{Type: ConversationParticipantAgent, ID: id}
}

func TestTeamRoutingAddressingOrder(t *testing.T) {
	store := NewMemoryStore()
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{
		Scope: Scope{Kind: "tenant", ID: "routing"}, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "launch"},
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
	post := func(key string, req PostChannelMessageRequest) *ChannelMessage {
		req.IdempotencyKey = key
		return conversationThreadStartPost(t, service, conversation, req)
	}
	route := func(trigger *ChannelMessage) []ConversationResponder {
		t.Helper()
		result, err := coordinator.Coordinate(t.Context(), ConversationRoutingRequest{
			Scope: conversation.Scope, ConversationID: conversation.ID, TriggerMessageID: trigger.ID, IdempotencyKey: "route:" + trigger.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result.Round.Responders
	}
	want := func(name string, got []ConversationResponder, expected ...ConversationResponder) {
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
	mention := func(id string) ConversationResponder {
		return ConversationResponder{Participant: teamRoutingAgent(id), Addressed: ConversationAddressedMention}
	}

	// Mentions win over @everyone and are answered in mention order.
	mentioned := post("mentions", PostChannelMessageRequest{
		Mentions: []ConversationParticipant{teamRoutingAgent("otto"), teamRoutingAgent("maya"), teamRoutingAgent("stranger")},
		Audience: ConversationAudience{Kind: ConversationAudienceEveryone},
	})
	want("mentions", route(mentioned), mention("otto"), mention("maya"))

	everyone := post("everyone", PostChannelMessageRequest{Audience: ConversationAudience{Kind: ConversationAudienceEveryone}})
	everyoneResponders := route(everyone)
	if len(everyoneResponders) != 3 || everyoneResponders[0].Addressed != ConversationAddressedEveryone {
		t.Fatalf("everyone = %#v", everyoneResponders)
	}

	answer := post("otto-answer", PostChannelMessageRequest{Sender: teamRoutingAgent("otto"), Intent: MessageIntentAnswer, ReplyToMessageID: everyone.ID})
	reply := post("reply", PostChannelMessageRequest{QuotedMessageID: answer.ID})
	want("quoted agent", route(reply), ConversationResponder{Participant: teamRoutingAgent("otto"), Addressed: ConversationAddressedReply})

	byRole := post("role", PostChannelMessageRequest{Audience: ConversationAudience{Kind: ConversationAudienceRoles, Roles: []string{"Pricing"}}})
	want("role", route(byRole), ConversationResponder{Participant: teamRoutingAgent("maya"), Addressed: ConversationAddressedRole})
	if len(routed) != 0 {
		t.Fatalf("addressed messages called the router %d times", len(routed))
	}

	plain := post("plain", PostChannelMessageRequest{Content: "Can someone draft the launch post?"})
	want("routed", route(plain), ConversationResponder{Participant: teamRoutingAgent("rio"), Addressed: ConversationAddressedRouted, Reason: "owns the launch copy"})
	if len(routed) != 1 || len(routed[0].Members) != 3 || routed[0].Trigger.ID != plain.ID || routed[0].LastAgentWithUser != "otto" ||
		len(routed[0].Recent) == 0 || routed[0].Recent[len(routed[0].Recent)-1].Sequence >= plain.Sequence {
		t.Fatalf("route request = %#v", routed)
	}
	// Replays never call the router again.
	want("replay", route(plain), ConversationResponder{Participant: teamRoutingAgent("rio"), Addressed: ConversationAddressedRouted, Reason: "owns the launch copy"})
	if len(routed) != 1 {
		t.Fatal("a replayed routing decision called the router")
	}

	// A router failure falls back to who the user was just talking to.
	routerErr = errors.New("router down")
	failed := post("failed", PostChannelMessageRequest{Content: "And the press list?"})
	want("fallback", route(failed), ConversationResponder{Participant: teamRoutingAgent("otto"), Addressed: ConversationAddressedRouted, Reason: "was just talking with you"})
	routerErr = nil

	// An Agent's handoff is addressed only by its mentions, never routed or
	// fanned out, and never back to itself.
	handoff := post("handoff", PostChannelMessageRequest{
		Sender: teamRoutingAgent("rio"), Intent: MessageIntentAnswer,
		Mentions: []ConversationParticipant{teamRoutingAgent("rio"), teamRoutingAgent("maya")},
	})
	want("handoff", route(handoff), mention("maya"))
	silent := post("agent-update", PostChannelMessageRequest{Sender: teamRoutingAgent("rio"), Intent: MessageIntentUpdate, Audience: ConversationAudience{Kind: ConversationAudienceEveryone}})
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
		if got.Participant.ID != tc.want || got.Addressed != ConversationAddressedRouted || got.Validate() != nil {
			t.Fatalf("%s: %#v", tc.name, got)
		}
	}
}

func TestTeamMessagesStartRunsOnlyForUsersAndHandoffs(t *testing.T) {
	conversation := &Conversation{ID: "chat", Scope: Scope{Kind: "tenant", ID: "one"}, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}, Status: ConversationStatusActive}
	message := func(sender ConversationParticipant, mentions ...ConversationParticipant) *ChannelMessage {
		return &ChannelMessage{ID: "m", Scope: conversation.Scope, ConversationID: "chat", Sender: sender, Intent: MessageIntentAnswer, Mentions: mentions}
	}
	user := ConversationParticipant{Type: ConversationParticipantUser, ID: "kev"}
	if !conversationMessageStartsRun(conversation, message(user)) {
		t.Fatal("user message did not start a Run")
	}
	if conversationMessageStartsRun(conversation, message(teamRoutingAgent("rio"))) {
		t.Fatal("a member answer started a Run")
	}
	if conversationMessageStartsRun(conversation, message(teamRoutingAgent("rio"), teamRoutingAgent("rio"), user)) {
		t.Fatal("a self or user mention started a Run")
	}
	if !conversationMessageStartsRun(conversation, message(teamRoutingAgent("rio"), teamRoutingAgent("maya"))) {
		t.Fatal("a handoff did not start a Run")
	}
}

func TestFindHandoffMentions(t *testing.T) {
	roster := conversationTeamRoster{members: map[string]conversationTeamMember{
		"maya": {name: "Maya"}, "otto": {name: "Otto Bell"}, "rio": {name: "Rio"},
	}}
	bindings := []ConversationParticipantBinding{{Participant: teamRoutingAgent("maya")}, {Participant: teamRoutingAgent("otto")}, {Participant: teamRoutingAgent("rio")}}
	got := findHandoffMentions("Draft is up. @otto bell can you check the list, then @Maya the pricing? @Riot and @everyone ignored.", "rio", roster, bindings)
	if !reflect.DeepEqual(got, []ConversationParticipant{teamRoutingAgent("otto"), teamRoutingAgent("maya")}) {
		t.Fatalf("mentions = %#v", got)
	}
	if got := findHandoffMentions("Thanks @Rio!", "rio", roster, bindings); got != nil {
		t.Fatalf("self mention = %#v", got)
	}
}

// teamTurnAttachments returns the attachments an Agent turn received in its
// goal, the one place a 1:1 and a Team member turn both carry them.
func teamTurnAttachments(t *testing.T, input TurnExecutionContext) ([]conversationAttachment, *agentConversationTeam) {
	t.Helper()
	var goal struct {
		Attachments []conversationAttachment `json:"attachments"`
		Team        *agentConversationTeam   `json:"team"`
	}
	payload := input.Run.Goal
	if at := strings.Index(payload, "\n\n{"); at >= 0 {
		payload = payload[at+2:]
	}
	if err := json.Unmarshal([]byte(payload), &goal); err != nil {
		t.Fatalf("turn goal is not JSON: %v", err)
	}
	return goal.Attachments, goal.Team
}
