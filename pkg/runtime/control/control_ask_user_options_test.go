package control

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
)

func TestAskUserOptionsDecodeLimits(t *testing.T) {
	option := func(label string) string { return `{"label":"` + label + `"}` }
	options := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = option(string(rune('A' + i)))
		}
		return `[` + strings.Join(parts, ",") + `]`
	}
	for name, raw := range map[string]string{
		"one option":                    `{"question":"Which?","options":` + options(1) + `}`,
		"seven options":                 `{"question":"Which?","options":` + options(7) + `}`,
		"empty label":                   `{"question":"Which?","options":[{"label":"  "},{"label":"B"}]}`,
		"long label":                    `{"question":"Which?","options":[{"label":"` + strings.Repeat("a", 81) + `"},{"label":"B"}]}`,
		"long description":              `{"question":"Which?","options":[{"label":"A","description":"` + strings.Repeat("d", 241) + `"},{"label":"B"}]}`,
		"duplicate labels after trim":   `{"question":"Which?","options":[{"label":"A"},{"label":" A "}]}`,
		"unknown option field":          `{"question":"Which?","options":[{"label":"A","id":"x"},{"label":"B"}]}`,
		"allowMultiple without options": `{"question":"Which?","allowMultiple":true}`,
		"allowOther without options":    `{"question":"Which?","allowOther":true}`,
	} {
		if _, err := (askUserHandler{}).Decode(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: decode accepted it", name)
		}
	}
	value, err := (askUserHandler{}).Decode(json.RawMessage(`{"question":"Which?","options":[{"label":" A ","description":" first "},{"label":"` +
		strings.Repeat("b", 80) + `"}],"allowMultiple":true,"allowOther":true}`))
	if err != nil {
		t.Fatal(err)
	}
	args := value.(*models.AskUserArgs)
	want := &models.ChannelQuestion{AllowMultiple: true, AllowOther: true, Options: []models.ChannelQuestionOption{
		{ID: "o1", Label: "A", Description: "first"}, {ID: "o2", Label: strings.Repeat("b", 80)},
	}}
	if got := args.ChannelQuestion(); !reflect.DeepEqual(got, want) {
		t.Fatalf("question = %#v", got)
	}
	if plain, err := (askUserHandler{}).Decode(json.RawMessage(`{"question":"Which?","options":[]}`)); err != nil ||
		plain.(*models.AskUserArgs).ChannelQuestion() != nil {
		t.Fatalf("empty options = %#v %v", plain, err)
	}
	spec := (askUserHandler{}).Spec(agentrun.ControlAuthority{})
	properties := spec.InputSchema["properties"].(map[string]interface{})
	for _, key := range []string{"options", "allowMultiple", "allowOther"} {
		if properties[key] == nil {
			t.Fatalf("schema lacks %s", key)
		}
	}
	if !strings.Contains(spec.Description, "Offer options when the answer is a choice.") {
		t.Fatalf("description = %q", spec.Description)
	}
}

func TestAgentBuilderConversationOriginValidates(t *testing.T) {
	if err := (models.ConversationReference{Kind: models.ConversationReferenceAgentBuilder, ID: "k"}).Validate(); err != nil {
		t.Fatal(err)
	}
	store := conversationtest.NewStore(t)
	conversationtest.SeedSpace(t, store, models.Scope{Kind: "organization", ID: "builder"})
	service := conversation.NewConversationService(store)
	origin := &models.ConversationReference{Kind: models.ConversationReferenceAgentBuilder, ID: "k"}
	conversation, _, err := service.CreateConversation(t.Context(), conversation.CreateConversationRequest{
		Scope: models.Scope{Kind: "organization", ID: "builder"}, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "builder-agent"},
		Title: "New agent", Origin: origin, IdempotencyKey: "agent-builder:k",
	})
	if err != nil || conversation.Origin == nil || *conversation.Origin != *origin {
		t.Fatalf("conversation = %#v %v", conversation, err)
	}
}
