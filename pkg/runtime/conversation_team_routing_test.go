package runtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func teamRoutingAgent(id string) ConversationParticipant {
	return ConversationParticipant{Type: ConversationParticipantAgent, ID: id}
}

func TestTeamRoutingAddressingOrder(t *testing.T) {
	store := newTestKernelStore(t)
	service := NewConversationService(store)
	seedTestSpace(t, store, Scope{Kind: "tenant", ID: "routing"})
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

// A Team member's chat step gets the message's attachments exactly as a 1:1
// chat step does: the same content parts on the trigger's user entry and the
// same "Attached:" line. Only the member also gets the Team context.
func TestTeamMemberStepGetsTheSameAttachmentsAsOneToOne(t *testing.T) {
	forConversationThreadStartStores(t, func(t *testing.T, base conversationThreadStartTestStore, _ func(*testing.T) conversationThreadStartTestStore) {
		content := "company,total\nAcme,42"
		artifacts, ok := base.(interface {
			CreateArtifactVersion(context.Context, *Artifact, int64) error
		})
		if !ok {
			t.Fatalf("%T stores no artifacts", base)
		}
		ask := func(team bool) (*SessionEntry, *SessionEntry, *TurnRunnerBinding, *chatLoopFixture) {
			t.Helper()
			f := newChatLoopFixture(t, base, chatLoopOptions{team: team})
			artifact := conversationTestArtifact()
			artifact.Scope = f.conversation.Scope
			artifact.MediaType, artifact.SizeBytes, artifact.Digest = "text/csv", int64(len(content)), conversationTestDigest(content)
			if err := artifacts.CreateArtifactVersion(f.ctx, artifact, 0); err != nil {
				t.Fatal(err)
			}
			runner, err := NewConversationRunTurnRunner(f.store, conversationRunTestCoordinator(t, f.service), ConversationRunTurnRunnerConfig{
				AttachmentContent: &conversationTestContent{text: content},
			})
			if err != nil {
				t.Fatal(err)
			}
			f.runner = runner
			posted, err := f.service.PostChannelMessage(f.ctx, PostChannelMessageRequest{
				Scope: f.conversation.Scope, ConversationID: f.conversation.ID, ExpectedRevision: f.conversation.Revision,
				Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "user-1"}, Intent: MessageIntentQuestion,
				Content: "What does the attached file total?", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
				References:     []ConversationReference{{Kind: ConversationReferenceArtifact, ID: artifact.ID, Version: 1}},
				IdempotencyKey: "question", RequiresResponse: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			f.conversation = posted.Conversation
			run := f.startRun(posted.Message)
			producer := &chatLoopProducer{answers: []string{"Acme totals 42."}}
			binding := f.bind(run, producer)
			if exit := f.drive(run, binding); exit.Run.Status != AgentRunStatusCompleted || len(producer.paths) != 1 {
				t.Fatalf("team=%v: exit %s, run %s, steps %d", team, exit.Reason, exit.Run.Status, len(producer.paths))
			}
			var trigger, note *SessionEntry
			for _, entry := range producer.paths[0] {
				switch {
				case entry.User != nil && entry.User.MessageID == posted.Message.ID:
					trigger = entry
				case entry.Note != nil && entry.Note.Kind == NoteRunContext:
					note = entry
				}
			}
			if trigger == nil || note == nil {
				t.Fatalf("team=%v: step path = %v", team, chatLoopShape(producer.paths[0]))
			}
			return trigger, note, binding, f
		}
		direct, directNote, directBinding, _ := ask(false)
		member, memberNote, memberBinding, team := ask(true)

		want := ContentPart{Type: ContentPartText, Text: "<attachment id=\"file\" version=\"1\" name=\"file.csv\" status=\"supplied\">\n" + content + "\n</attachment>"}
		if len(direct.User.Content) != 2 || !reflect.DeepEqual(direct.User.Content[1], want) {
			t.Fatalf("1:1 attachment parts = %#v", direct.User.Content)
		}
		if !reflect.DeepEqual(member.User.Content[1:], direct.User.Content[1:]) {
			t.Fatalf("Team member attachment parts = %#v, 1:1 = %#v", member.User.Content[1:], direct.User.Content[1:])
		}
		for name, entry := range map[string]*SessionEntry{"1:1": direct, "member": member} {
			if !strings.HasSuffix(chatLoopEntryText(entry), "\nAttached: file.csv (artifact file v1)") {
				t.Fatalf("%s trigger = %q", name, chatLoopEntryText(entry))
			}
		}
		if strings.Contains(chatStepText(directBinding, []*SessionEntry{directNote}), teamChannelRules) ||
			!strings.Contains(chatStepText(memberBinding, []*SessionEntry{memberNote}), teamChannelRules) {
			t.Fatal("Team rules reached the 1:1 step or missed the member step")
		}
		answers := team.agentMessages()
		if len(answers) != 1 || answers[0].Sender != teamRoutingAgent("agent-1") || answers[0].Content != "Acme totals 42." {
			t.Fatalf("member answer = %#v", answers)
		}
	})
}

// The router's view of a Team comes from its members: the one marked lead,
// and what each does on the Team.
func TestConversationTeamRosterReadsLeadAndPurposeFromMembers(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
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
	roster := loadConversationTeamRoster(ctx, store, &Conversation{Scope: Scope{Kind: "tenant", ID: "one"}, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "launch"}})
	if roster.lead != "maya" || roster.notes != "Maya owns pricing" ||
		!reflect.DeepEqual(roster.members["maya"], conversationTeamMember{name: "Maya", role: "Owns pricing", owns: []string{"pricing"}}) ||
		roster.members["otto"].role != "Owns press" {
		t.Fatalf("roster = %+v", roster)
	}
}
