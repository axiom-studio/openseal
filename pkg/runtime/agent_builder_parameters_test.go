package runtime

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/validation"
)

type agentBuilderParametersFixture struct {
	service *ConversationService
	scope   Scope
	user    ConversationParticipant
}

func newAgentBuilderParametersFixture() agentBuilderParametersFixture {
	return agentBuilderParametersFixture{
		service: NewConversationService(NewMemoryStore()),
		scope:   Scope{Kind: "organization", ID: "builder"},
		user:    ConversationParticipant{Type: ConversationParticipantUser, ID: "u_7"},
	}
}

func (f agentBuilderParametersFixture) conversation(t *testing.T, key string, origin *ConversationReference) *Conversation {
	t.Helper()
	conversation, _, err := f.service.CreateConversation(t.Context(), CreateConversationRequest{
		Scope: f.scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "openseal-assistant"},
		Title: "New agent", Origin: origin, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return conversation
}

func (f agentBuilderParametersFixture) builderConversation(t *testing.T) *Conversation {
	return f.conversation(t, "agent-builder:k", &ConversationReference{Kind: ConversationReferenceAgentBuilder, ID: "k"})
}

func (f agentBuilderParametersFixture) post(t *testing.T, conversation *Conversation, sender ConversationParticipant, content string, parameters *AgentBuilderParameters, key string) (*ChannelMessageCommitResult, error) {
	t.Helper()
	current, err := f.service.GetConversation(t.Context(), f.scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f.service.PostChannelMessage(t.Context(), PostChannelMessageRequest{
		Scope: f.scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
		Sender: sender, Intent: MessageIntentUpdate, Content: content,
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, BuilderParameters: parameters, IdempotencyKey: key,
	})
}

func TestAgentBuilderParametersAcceptedOnUserMessageInBuilderConversation(t *testing.T) {
	f := newAgentBuilderParametersFixture()
	conversation := f.builderConversation(t)
	parameters := &AgentBuilderParameters{Kind: " agent ", Name: "  Press Desk ", Model: "openai/gpt-5"}
	result, err := f.post(t, conversation, f.user, "Build me a press desk agent", parameters, "agent-builder-prompt:k")
	if err != nil {
		t.Fatal(err)
	}
	message := result.Message
	want := AgentBuilderParameters{Kind: AgentBuilderKindAgent, Name: "Press Desk", Model: "openai/gpt-5"}
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
	if _, err := f.post(t, conversation, f.user, "Build me a press desk agent", &AgentBuilderParameters{Kind: AgentBuilderKindAgent, Name: "Other"}, "agent-builder-prompt:k"); !errors.Is(err, ErrMessageConflict) {
		t.Fatalf("changed parameters under the same key = %v, want message conflict", err)
	}

	stored, err := f.service.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: f.scope, ConversationID: conversation.ID})
	if err != nil || len(stored) != 1 || stored[0].BuilderParameters == nil || *stored[0].BuilderParameters != want {
		t.Fatalf("stored = %#v %v", stored, err)
	}
	encoded, err := json.Marshal(stored[0])
	if err != nil || !strings.Contains(string(encoded), `"builderParameters":{"kind":"agent","name":"Press Desk","model":"openai/gpt-5"}`) {
		t.Fatalf("json = %s %v", encoded, err)
	}
}

func TestAgentBuilderParametersEmptyContentOnlyWithParameters(t *testing.T) {
	f := newAgentBuilderParametersFixture()
	conversation := f.builderConversation(t)
	result, err := f.post(t, conversation, f.user, "  ", &AgentBuilderParameters{Kind: AgentBuilderKindTeam, Name: "Launch crew"}, "rename")
	if err != nil {
		t.Fatal(err)
	}
	if result.Message.Content != "" || result.Message.BuilderParameters.Name != "Launch crew" {
		t.Fatalf("rename message = %#v", result.Message)
	}
	if _, err := f.post(t, conversation, f.user, "", &AgentBuilderParameters{Kind: AgentBuilderKindAgent, Model: "anthropic/claude"}, "model"); err != nil {
		t.Fatalf("model-only reply refused: %v", err)
	}
	if _, err := f.post(t, conversation, f.user, "", nil, "empty"); !errors.Is(err, ErrInvalidConversation) {
		t.Fatalf("empty content without parameters = %v", err)
	}
	if _, err := f.post(t, conversation, f.user, "", &AgentBuilderParameters{Kind: AgentBuilderKindAgent}, "kind-only"); !errors.Is(err, ErrInvalidConversation) {
		t.Fatalf("empty content with parameters that set nothing = %v", err)
	}
}

func TestAgentBuilderParametersRefusedOutsideBuilderConversation(t *testing.T) {
	f := newAgentBuilderParametersFixture()
	parameters := &AgentBuilderParameters{Kind: AgentBuilderKindAgent, Name: "Press Desk"}
	for name, origin := range map[string]*ConversationReference{
		"no origin":    nil,
		"other origin": {Kind: ConversationReferenceEmbedSession, ID: "embed"},
	} {
		t.Run(name, func(t *testing.T) {
			conversation := f.conversation(t, "conversation:"+name, origin)
			if _, err := f.post(t, conversation, f.user, "Build me an agent", parameters, "first"); !errors.Is(err, ErrInvalidConversation) {
				t.Fatalf("parameters accepted outside a builder conversation: %v", err)
			}
			messages, err := f.service.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: f.scope, ConversationID: conversation.ID})
			if err != nil || len(messages) != 0 {
				t.Fatalf("refused message persisted: %#v %v", messages, err)
			}
		})
	}
}

func TestAgentBuilderParametersRefusedFromAgentSender(t *testing.T) {
	f := newAgentBuilderParametersFixture()
	conversation := f.builderConversation(t)
	for _, sender := range []ConversationParticipant{
		{Type: ConversationParticipantAgent, ID: "openseal-assistant"},
		{Type: ConversationParticipantService, ID: "openseal"},
	} {
		if _, err := f.post(t, conversation, sender, "Here is a draft", &AgentBuilderParameters{Kind: AgentBuilderKindAgent, Name: "X"}, "from-"+string(sender.Type)); !errors.Is(err, ErrInvalidConversation) {
			t.Fatalf("%s sender with parameters = %v", sender.Type, err)
		}
	}
}

func TestAgentBuilderParametersValidate(t *testing.T) {
	at := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	base := ChannelMessage{
		ID: "m_1", Scope: Scope{Kind: "organization", ID: "builder"}, ConversationID: "c_1", Sequence: 1,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "u_7"}, Intent: MessageIntentUpdate,
		Content: "Build me an agent", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, CreatedAt: at,
	}
	valid := &AgentBuilderParameters{Kind: AgentBuilderKindAgent, Name: "Press Desk", Model: "openai/gpt-5", Draft: &AgentBuilderDraftProfile{
		Purpose: "Answers press questions", SystemPrompt: "Answer from the press kit.\nKeep it short.", Personality: "Warm",
		OperatingPrinciples: []string{"Cite the kit", "Never promise dates"},
	}}
	message := base
	message.BuilderParameters = valid
	if err := message.Validate(); err != nil {
		t.Fatalf("valid parameters refused: %v", err)
	}
	principles := make([]string, maximumAgentBuilderOperatingPrinciples+1)
	for index := range principles {
		principles[index] = "principle " + string(rune('a'+index))
	}
	for _, test := range []struct {
		name       string
		parameters AgentBuilderParameters
		sensitive  bool
	}{
		{name: "missing kind", parameters: AgentBuilderParameters{Name: "X"}},
		{name: "unknown kind", parameters: AgentBuilderParameters{Kind: "workflow", Name: "X"}},
		{name: "long name", parameters: AgentBuilderParameters{Kind: AgentBuilderKindAgent, Name: strings.Repeat("n", maximumAgentBuilderNameBytes+1)}},
		{name: "multi-line name", parameters: AgentBuilderParameters{Kind: AgentBuilderKindAgent, Name: "Press\nDesk"}},
		{name: "long model", parameters: AgentBuilderParameters{Kind: AgentBuilderKindAgent, Model: strings.Repeat("m", maximumAgentBuilderModelBytes+1)}},
		{name: "long purpose", parameters: AgentBuilderParameters{Kind: AgentBuilderKindAgent, Draft: &AgentBuilderDraftProfile{Purpose: strings.Repeat("p", maximumAgentBuilderPurposeBytes+1)}}},
		{name: "long behaviour", parameters: AgentBuilderParameters{Kind: AgentBuilderKindAgent, Draft: &AgentBuilderDraftProfile{SystemPrompt: strings.Repeat("s", maximumAgentBuilderSystemPromptBytes+1)}}},
		{name: "too many principles", parameters: AgentBuilderParameters{Kind: AgentBuilderKindAgent, Draft: &AgentBuilderDraftProfile{OperatingPrinciples: principles}}},
		{name: "duplicate principle", parameters: AgentBuilderParameters{Kind: AgentBuilderKindAgent, Draft: &AgentBuilderDraftProfile{OperatingPrinciples: []string{"a", "a"}}}},
		{name: "empty principle", parameters: AgentBuilderParameters{Kind: AgentBuilderKindAgent, Draft: &AgentBuilderDraftProfile{OperatingPrinciples: []string{" "}}}},
		{name: "sensitive name", parameters: AgentBuilderParameters{Kind: AgentBuilderKindAgent, Name: "Password: hunter2-secret"}, sensitive: true},
		{name: "sensitive model", parameters: AgentBuilderParameters{Kind: AgentBuilderKindAgent, Model: "sk-1234567890abcdefghijklmnop"}, sensitive: true},
		{name: "sensitive behaviour", parameters: AgentBuilderParameters{Kind: AgentBuilderKindAgent, Draft: &AgentBuilderDraftProfile{SystemPrompt: "Use api key: sk-1234567890abcdefghijklmnop"}}, sensitive: true},
		{name: "sensitive principle", parameters: AgentBuilderParameters{Kind: AgentBuilderKindAgent, Draft: &AgentBuilderDraftProfile{OperatingPrinciples: []string{"Use github_pat_1234567890abcdefghijklmnop"}}}, sensitive: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := base
			message.BuilderParameters = &test.parameters
			err := message.Validate()
			if !errors.Is(err, ErrInvalidConversation) {
				t.Fatalf("Validate = %v, want invalid conversation", err)
			}
			var sensitive *validation.SensitiveInputError
			if test.sensitive != errors.As(err, &sensitive) || test.sensitive != errors.Is(err, validation.ErrSensitiveInput) {
				t.Fatalf("Validate = %v, sensitive = %t", err, test.sensitive)
			}
			if test.sensitive && strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("error leaks the sensitive value: %v", err)
			}
		})
	}
}

func TestAgentBuilderParametersConversationMessageEntryLine(t *testing.T) {
	at := time.Date(2026, 11, 7, 10, 42, 13, 0, time.UTC)
	user := ConversationParticipant{Type: ConversationParticipantUser, ID: "u_7"}
	header := "[Maya (user u_7) · m_1 · 2026-11-07 10:42 UTC]\n"
	for _, test := range []struct {
		name    string
		content string
		params  *AgentBuilderParameters
		want    string
	}{
		{"first message", "Build me a press desk agent", &AgentBuilderParameters{Kind: AgentBuilderKindAgent, Name: "Press Desk", Model: "openai/gpt-5"},
			header + "Build me a press desk agent\nBuilder parameters (set in the form): kind agent; name “Press Desk”; model openai/gpt-5"},
		{"rename reply", "", &AgentBuilderParameters{Kind: AgentBuilderKindTeam, Name: "Launch crew"},
			header + "Builder parameters (set in the form): kind team; name “Launch crew”"},
		{"migrated draft", "Help with press", &AgentBuilderParameters{Kind: AgentBuilderKindAgent, Name: "Press Desk", Draft: &AgentBuilderDraftProfile{
			Purpose: "Answers press questions", SystemPrompt: "Answer from the kit", Personality: "Warm", OperatingPrinciples: []string{"Cite the kit", "Be brief"}}},
			header + "Help with press\nBuilder parameters (set in the form): kind agent; name “Press Desk”\n" +
				"Started draft: purpose Answers press questions; behaviour Answer from the kit; personality Warm; principles: Cite the kit; Be brief"},
		{"no parameters", "Hello", nil, header + "Hello"},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := &ChannelMessage{ID: "m_1", Sender: user, SenderDisplayName: "Maya", CreatedAt: at, Content: test.content, BuilderParameters: test.params}
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
