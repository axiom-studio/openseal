package models

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/validation"
)

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

func TestAgentBuilderCutoverStartRequestFitsParameters(t *testing.T) {
	draft := AgentBuilderCutoverDraft{
		Scope: builderCutoverScope, ID: "cs-1", ActorID: " user-1 ", Prompt: "Plain request without marker",
		Agent: &kernelagent.AgentDefinition{
			DisplayName: strings.Repeat("é", 60), Purpose: "Uses password: hunter2-secret daily",
			SystemPrompt: "  Behave.  ", OperatingPrinciples: []string{"Use api key: sk-1234567890abcdefghijklmnop", "Be kind"},
		},
	}
	request := draft.StartRequest("model\nwith newline", "")
	if request.Prompt != "Plain request without marker" || request.Requester.ID != "user-1" {
		t.Fatalf("request = %#v", request)
	}
	parameters := request.Parameters
	if len(parameters.Name) > maximumAgentBuilderNameBytes || !strings.HasPrefix(parameters.Name, "éé") || parameters.Model != "model with newline" {
		t.Fatalf("name %q (%d bytes) model %q", parameters.Name, len(parameters.Name), parameters.Model)
	}
	if parameters.Draft == nil || parameters.Draft.Purpose != "" || parameters.Draft.SystemPrompt != "Behave." ||
		!reflect.DeepEqual(parameters.Draft.OperatingPrinciples, []string{"Be kind"}) {
		t.Fatalf("draft = %#v", parameters.Draft)
	}
	if err := parameters.Validate(); err != nil {
		t.Fatalf("fitted parameters refused: %v", err)
	}
	if bare := (AgentBuilderCutoverDraft{ID: "cs-2", Prompt: "x"}).StartRequest("", ""); bare.Parameters.Draft != nil || bare.Parameters.Name != "" {
		t.Fatalf("draft without a profile = %#v", bare.Parameters)
	}
}

var builderCutoverScope = Scope{Kind: "tenant", ID: "cutover"}
