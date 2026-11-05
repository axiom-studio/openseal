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

func TestTeamMemberTurnGetsTheSameAttachmentsAsOneToOne(t *testing.T) {
	ctx := t.Context()
	store := NewMemoryStore()
	artifact := conversationTestArtifact()
	content := "company,total\nAcme,42"
	artifact.MediaType, artifact.SizeBytes, artifact.Digest = "text/csv", int64(len(content)), conversationTestDigest(content)
	if err := store.CreateArtifactVersion(ctx, artifact, 0); err != nil {
		t.Fatal(err)
	}
	service := NewConversationService(store)
	var inputs []TurnExecutionContext
	agentTurns := TurnRunnerResolverFunc(func(_ context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
		if run.Kind != RunKindAgentWork || run.AssignedAgentID != "agent-1" {
			t.Fatalf("Agent turn resolved for %#v", run)
		}
		return &TurnRunnerBinding{DeploymentID: "agent-1", Runner: TurnRunnerFunc(func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
			inputs = append(inputs, input)
			return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "Acme totals 42."}}, nil
		})}, nil
	})
	runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{
		AgentTurns: agentTurns, AttachmentContent: &conversationTestContent{text: content},
	})
	if err != nil {
		t.Fatal(err)
	}
	ask := func(owner ObjectiveOwner) (*Conversation, *AgentRun) {
		conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{
			Scope: artifact.Scope, Owner: owner, Title: "Totals", IdempotencyKey: "chat-" + string(owner.Type),
		})
		if err != nil {
			t.Fatal(err)
		}
		trigger := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{
			IdempotencyKey: "question", Content: "What does the attached file total?", RequiresResponse: true,
			References: []ConversationReference{{Kind: ConversationReferenceArtifact, ID: artifact.ID, Version: 1}},
		})
		scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(ctx, conversation.Scope, conversation.ID, trigger.ID)
		if err != nil || scheduled == nil {
			t.Fatalf("schedule: %#v %v", scheduled, err)
		}
		return conversation, scheduled.Run
	}

	// 1:1 chat with agent-1.
	_, direct := ask(ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-1"})
	binding, err := runner.ResolveTurnRunner(ctx, direct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := binding.Runner.RunTurn(ctx, TurnExecutionContext{Run: direct}); err != nil {
		t.Fatal(err)
	}

	// The same message in a Team channel, routed to agent-1.
	team, routing := ask(ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"})
	routed, err := runner.RunTurn(ctx, TurnExecutionContext{Run: routing})
	if err != nil {
		t.Fatal(err)
	}
	childID := routed.RunOutput["responderRunIds"].([]interface{})[0].(string)
	child, err := store.GetAgentRun(ctx, team.Scope, childID)
	// The member's answer is the same root conversation Run a 1:1 chat gets,
	// in the member's own lane.
	if err != nil || !isTeamMemberRun(child) || child.ParentRunID != "" || child.RootRunID != child.ID ||
		child.ConcurrencyKey != team.ID+":agent:agent-1" || child.Context[teamRoutingRunContextKey] != routing.ID || validateConversationRun(child) != nil {
		t.Fatalf("member Run = %#v, %v", child, err)
	}
	binding, err = runner.ResolveTurnRunner(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := binding.Runner.RunTurn(ctx, TurnExecutionContext{Run: child}); err != nil {
		t.Fatal(err)
	}

	if len(inputs) != 2 {
		t.Fatalf("Agent turns = %d", len(inputs))
	}
	oneToOne, noTeam := teamTurnAttachments(t, inputs[0])
	member, teamContext := teamTurnAttachments(t, inputs[1])
	if noTeam != nil || teamContext == nil || teamContext.RoutingReason != "owns the canary" || teamContext.Rules != teamChannelRules ||
		len(teamContext.Roster) != 1 || !teamContext.Roster[0].You {
		t.Fatalf("team context: 1:1 = %#v, member = %#v", noTeam, teamContext)
	}
	if len(oneToOne) != 1 || oneToOne[0].Status != "supplied" || oneToOne[0].Text != content {
		t.Fatalf("1:1 attachments = %#v", oneToOne)
	}
	if !reflect.DeepEqual(member, oneToOne) {
		t.Fatalf("Team member attachments = %#v, 1:1 = %#v", member, oneToOne)
	}
	messages, err := service.ListChannelMessages(ctx, ChannelMessageFilter{Scope: team.Scope, ConversationID: team.ID})
	if err != nil || len(messages) != 2 || messages[1].Sender != teamRoutingAgent("agent-1") || messages[1].RoutingReason != "owns the canary" {
		t.Fatalf("member answer = %#v, %v", messages, err)
	}
}
