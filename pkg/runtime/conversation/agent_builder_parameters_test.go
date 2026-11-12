package conversation

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

type agentBuilderParametersFixture struct {
	service *ConversationService
	scope   models.Scope
	user    models.ConversationParticipant
}

func newAgentBuilderParametersFixture(t *testing.T) agentBuilderParametersFixture {
	t.Helper()
	store, scope := newTestKernelStore(t), models.Scope{Kind: "organization", ID: "builder"}
	seedTestSpace(t, store, scope)
	return agentBuilderParametersFixture{
		service: NewConversationService(store),
		scope:   scope,
		user:    models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "u_7"},
	}
}

func (f agentBuilderParametersFixture) conversation(t *testing.T, key string, origin *models.ConversationReference) *models.Conversation {
	t.Helper()
	conversation, _, err := f.service.CreateConversation(t.Context(), CreateConversationRequest{
		Scope: f.scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "openseal-assistant"},
		Title: "New agent", Origin: origin, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return conversation
}

func (f agentBuilderParametersFixture) builderConversation(t *testing.T) *models.Conversation {
	return f.conversation(t, "agent-builder:k", &models.ConversationReference{Kind: models.ConversationReferenceAgentBuilder, ID: "k"})
}

func (f agentBuilderParametersFixture) post(t *testing.T, conversation *models.Conversation, sender models.ConversationParticipant, content string, parameters *models.AgentBuilderParameters, key string) (*models.ChannelMessageCommitResult, error) {
	t.Helper()
	current, err := f.service.GetConversation(t.Context(), f.scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f.service.PostChannelMessage(t.Context(), PostChannelMessageRequest{
		Scope: f.scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
		Sender: sender, Intent: models.MessageIntentUpdate, Content: content,
		Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, BuilderParameters: parameters, IdempotencyKey: key,
	})
}

func TestAgentBuilderParametersAcceptedOnUserMessageInBuilderConversation(t *testing.T) {
	f := newAgentBuilderParametersFixture(t)
	conversation := f.builderConversation(t)
	parameters := &models.AgentBuilderParameters{Kind: " agent ", Name: "  Press Desk ", Model: "openai/gpt-5"}
	result, err := f.post(t, conversation, f.user, "Build me a press desk agent", parameters, "agent-builder-prompt:k")
	if err != nil {
		t.Fatal(err)
	}
	message := result.Message
	want := models.AgentBuilderParameters{Kind: models.AgentBuilderKindAgent, Name: "Press Desk", Model: "openai/gpt-5"}
	if message.BuilderParameters == nil || *message.BuilderParameters != want {
		t.Fatalf("parameters = %#v", message.BuilderParameters)
	}
	if message.Content != "Build me a press desk agent" {
		t.Fatalf("content = %q; parameters must never be written into content", message.Content)
	}
	if message.BuilderParameters == parameters {
		t.Fatal("stored parameters alias the request")
	}

	replay, err := f.post(t, conversation, f.user, "Build me a press desk agent", parameters, "agent-builder-prompt:k")
	if err != nil || !replay.Replayed || replay.Message.ID != message.ID {
		t.Fatalf("replay = %#v %v", replay, err)
	}
	if _, err := f.post(t, conversation, f.user, "Build me a press desk agent", &models.AgentBuilderParameters{Kind: models.AgentBuilderKindAgent, Name: "Other"}, "agent-builder-prompt:k"); !errors.Is(err, models.ErrMessageConflict) {
		t.Fatalf("changed parameters under the same key = %v, want message conflict", err)
	}

	stored, err := f.service.ListChannelMessages(t.Context(), models.ChannelMessageFilter{Scope: f.scope, ConversationID: conversation.ID})
	if err != nil || len(stored) != 1 || stored[0].BuilderParameters == nil || *stored[0].BuilderParameters != want {
		t.Fatalf("stored = %#v %v", stored, err)
	}
	encoded, err := json.Marshal(stored[0])
	if err != nil || !strings.Contains(string(encoded), `"builderParameters":{"kind":"agent","name":"Press Desk","model":"openai/gpt-5"}`) {
		t.Fatalf("json = %s %v", encoded, err)
	}
}

func TestAgentBuilderParametersEmptyContentOnlyWithParameters(t *testing.T) {
	f := newAgentBuilderParametersFixture(t)
	conversation := f.builderConversation(t)
	result, err := f.post(t, conversation, f.user, "  ", &models.AgentBuilderParameters{Kind: models.AgentBuilderKindTeam, Name: "Launch crew"}, "rename")
	if err != nil {
		t.Fatal(err)
	}
	if result.Message.Content != "" || result.Message.BuilderParameters.Name != "Launch crew" {
		t.Fatalf("rename message = %#v", result.Message)
	}
	if _, err := f.post(t, conversation, f.user, "", &models.AgentBuilderParameters{Kind: models.AgentBuilderKindAgent, Model: "anthropic/claude"}, "model"); err != nil {
		t.Fatalf("model-only reply refused: %v", err)
	}
	if _, err := f.post(t, conversation, f.user, "", nil, "empty"); !errors.Is(err, models.ErrInvalidConversation) {
		t.Fatalf("empty content without parameters = %v", err)
	}
	if _, err := f.post(t, conversation, f.user, "", &models.AgentBuilderParameters{Kind: models.AgentBuilderKindAgent}, "kind-only"); !errors.Is(err, models.ErrInvalidConversation) {
		t.Fatalf("empty content with parameters that set nothing = %v", err)
	}
}

func TestAgentBuilderParametersRefusedOutsideBuilderConversation(t *testing.T) {
	f := newAgentBuilderParametersFixture(t)
	parameters := &models.AgentBuilderParameters{Kind: models.AgentBuilderKindAgent, Name: "Press Desk"}
	for name, origin := range map[string]*models.ConversationReference{
		"no origin":    nil,
		"other origin": {Kind: models.ConversationReferenceEmbedSession, ID: "embed"},
	} {
		t.Run(name, func(t *testing.T) {
			conversation := f.conversation(t, "conversation:"+name, origin)
			if _, err := f.post(t, conversation, f.user, "Build me an agent", parameters, "first"); !errors.Is(err, models.ErrInvalidConversation) {
				t.Fatalf("parameters accepted outside a builder conversation: %v", err)
			}
			messages, err := f.service.ListChannelMessages(t.Context(), models.ChannelMessageFilter{Scope: f.scope, ConversationID: conversation.ID})
			if err != nil || len(messages) != 0 {
				t.Fatalf("refused message persisted: %#v %v", messages, err)
			}
		})
	}
}

func TestAgentBuilderParametersRefusedFromAgentSender(t *testing.T) {
	f := newAgentBuilderParametersFixture(t)
	conversation := f.builderConversation(t)
	for _, sender := range []models.ConversationParticipant{
		{Type: models.ConversationParticipantAgent, ID: "openseal-assistant"},
		{Type: models.ConversationParticipantService, ID: "openseal"},
	} {
		if _, err := f.post(t, conversation, sender, "Here is a draft", &models.AgentBuilderParameters{Kind: models.AgentBuilderKindAgent, Name: "X"}, "from-"+string(sender.Type)); !errors.Is(err, models.ErrInvalidConversation) {
			t.Fatalf("%s sender with parameters = %v", sender.Type, err)
		}
	}
}

func TestAgentBuilderParametersConversationMessageEntryLine(t *testing.T) {
	at := time.Date(2026, 11, 7, 10, 42, 13, 0, time.UTC)
	user := models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "u_7"}
	header := "[Maya (user u_7) · m_1 · 2026-11-07 10:42 UTC]\n"
	for _, test := range []struct {
		name    string
		content string
		params  *models.AgentBuilderParameters
		want    string
	}{
		{"first message", "Build me a press desk agent", &models.AgentBuilderParameters{Kind: models.AgentBuilderKindAgent, Name: "Press Desk", Model: "openai/gpt-5"},
			header + "Build me a press desk agent\nBuilder parameters (set in the form): kind agent; name “Press Desk”; model openai/gpt-5"},
		{"rename reply", "", &models.AgentBuilderParameters{Kind: models.AgentBuilderKindTeam, Name: "Launch crew"},
			header + "Builder parameters (set in the form): kind team; name “Launch crew”"},
		{"migrated draft", "Help with press", &models.AgentBuilderParameters{Kind: models.AgentBuilderKindAgent, Name: "Press Desk", Draft: &models.AgentBuilderDraftProfile{
			Purpose: "Answers press questions", SystemPrompt: "Answer from the kit", Personality: "Warm", OperatingPrinciples: []string{"Cite the kit", "Be brief"}}},
			header + "Help with press\nBuilder parameters (set in the form): kind agent; name “Press Desk”\n" +
				"Started draft: purpose Answers press questions; behaviour Answer from the kit; personality Warm; principles: Cite the kit; Be brief"},
		{"no parameters", "Hello", nil, header + "Hello"},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := &models.ChannelMessage{ID: "m_1", Sender: user, SenderDisplayName: "Maya", CreatedAt: at, Content: test.content, BuilderParameters: test.params}
			if got := renderConversationMessageBody(message, conversationAttributionUser, nil); got != test.want {
				t.Fatalf("body\n got %q\nwant %q", got, test.want)
			}
			entry := conversationMessageEntry("session", "run", message, conversationAttributionUser, conversationMessageAttachments{})
			if entry.User == nil || len(entry.User.Content) != 1 || entry.User.Content[0].Text != test.want {
				t.Fatalf("entry = %#v", entry.User)
			}
			if message.Content != test.content {
				t.Fatalf("content changed to %q", message.Content)
			}
		})
	}
}
