package skill

import (
	"errors"
	"strings"
	"testing"
)

// A person reference is valid only on an OAuth action slot of a binding with
// no adapters, and only with the requirement's kind.
func TestBindingValidationPlacesPersonCredentials(t *testing.T) {
	person := CredentialReference{Kind: "oauth2", ID: PersonCredentialReferenceID}
	definition := &Definition{
		Actions: map[string]Action{
			"send": {Name: "send", Risk: RiskLevelWrite, Credentials: []CredentialRequirement{
				{Name: "google", Kind: "oauth2", OAuth2: &OAuth2Requirement{Provider: "google"}},
				{Name: "key", Kind: "api_key"},
			}},
		},
		Prompt: &PromptModule{Credentials: []CredentialRequirement{{Name: "notes", Kind: "oauth2", OAuth2: &OAuth2Requirement{Provider: "google"}}}},
		ConversationAdapters: map[string]ConversationAdapter{"chat": {Credentials: []CredentialRequirement{
			{Name: "bot", Kind: "oauth2", OAuth2: &OAuth2Requirement{Provider: "slack"}},
		}}},
	}
	binding := func(credentials map[string]CredentialReference, adapters ...string) *Binding {
		return &Binding{AllowedActions: []string{"send"}, MaximumRisk: RiskLevelWrite, Credentials: credentials, EnabledConversationAdapters: adapters}
	}
	valid := binding(map[string]CredentialReference{"google": person, "key": {Kind: "api_key", ID: "vault://key"}})
	if err := validatePersonCredentials(valid, definition); err != nil {
		t.Fatalf("per_person OAuth action slot refused: %v", err)
	}
	for name, candidate := range map[string]*Binding{
		"non-OAuth slot":  binding(map[string]CredentialReference{"google": {Kind: "oauth2", ID: "vault://credential/1"}, "key": {Kind: "api_key", ID: PersonCredentialReferenceID}}),
		"prompt slot":     binding(map[string]CredentialReference{"google": person, "notes": person}),
		"adapter slot":    binding(map[string]CredentialReference{"google": {Kind: "oauth2", ID: "vault://credential/1"}, "bot": person}, "chat"),
		"adapter binding": binding(map[string]CredentialReference{"google": person, "bot": {Kind: "oauth2", ID: "vault://credential/2"}}, "chat"),
		"kind mismatch":   binding(map[string]CredentialReference{"google": {Kind: "api_key", ID: PersonCredentialReferenceID}}),
	} {
		err := validateBindingAgainstDefinition(candidate, definition)
		if !errors.Is(err, ErrBindingInvalid) || !strings.Contains(err.Error(), "per-person accounts work only for actions") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
