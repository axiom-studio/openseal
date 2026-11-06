package skill_test

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestPersonCredentialAllowed(t *testing.T) {
	oauth := skill.CredentialRequirement{Name: "google", Kind: "oauth2", OAuth2: &skill.OAuth2Requirement{}}
	key := skill.CredentialRequirement{Name: "key", Kind: "api_key"}
	definition := &skill.Definition{
		Actions: map[string]skill.Action{
			"send": {Credentials: []skill.CredentialRequirement{oauth, key}},
			"read": {Credentials: []skill.CredentialRequirement{oauth}},
		},
		Prompt:               &skill.PromptModule{Credentials: []skill.CredentialRequirement{{Name: "prompt", Kind: "oauth2", OAuth2: &skill.OAuth2Requirement{}}}},
		ConversationAdapters: map[string]skill.ConversationAdapter{"chat": {Credentials: []skill.CredentialRequirement{{Name: "bot", Kind: "oauth2", OAuth2: &skill.OAuth2Requirement{}}}}},
	}
	binding := &skill.Binding{}
	for slot, want := range map[string]bool{"google": true, "key": false, "prompt": false, "bot": false, "missing": false, "": false} {
		if got := skill.PersonCredentialAllowed(definition, binding, slot); got != want {
			t.Errorf("slot %q: allowed = %v, want %v", slot, got, want)
		}
	}
	// An OAuth slot that a prompt also uses is not an action-only slot.
	shared := *definition
	shared.Prompt = &skill.PromptModule{Credentials: []skill.CredentialRequirement{oauth}}
	if skill.PersonCredentialAllowed(&shared, binding, "google") {
		t.Error("slot used by the prompt allowed")
	}
	if skill.PersonCredentialAllowed(definition, &skill.Binding{EnabledConversationAdapters: []string{"chat"}}, "google") {
		t.Error("binding with a conversation adapter allowed")
	}
	if skill.PersonCredentialAllowed(definition, &skill.Binding{EnabledCallbackAdapters: []string{"hook"}}, "google") {
		t.Error("binding with a callback adapter allowed")
	}
	if skill.PersonCredentialAllowed(nil, binding, "google") || skill.PersonCredentialAllowed(definition, nil, "google") {
		t.Error("nil definition or binding allowed")
	}
}

func TestBindingUsesPersonCredentials(t *testing.T) {
	person := skill.CredentialReference{Kind: "oauth2", ID: skill.PersonCredentialReferenceID}
	if !skill.IsPersonCredentialReference(person) || skill.IsPersonCredentialReference(skill.CredentialReference{Kind: "oauth2", ID: "vault://credential/1"}) {
		t.Fatal("IsPersonCredentialReference")
	}
	if skill.BindingUsesPersonCredentials(nil) || skill.BindingUsesPersonCredentials(&skill.Binding{Credentials: map[string]skill.CredentialReference{"a": {Kind: "oauth2", ID: "vault://credential/1"}}}) {
		t.Fatal("binding without a person slot reported as per_person")
	}
	if !skill.BindingUsesPersonCredentials(&skill.Binding{Credentials: map[string]skill.CredentialReference{"a": {Kind: "oauth2", ID: "vault://credential/1"}, "b": person}}) {
		t.Fatal("binding with a person slot not reported")
	}
}

// PersonalApps travel with the host set, key it, and are returned even when
// the host supplies no bindings.
func TestHostBindingsCarryPersonalApps(t *testing.T) {
	ctx := context.Background()
	scope := skill.ScopeReference{Kind: "tenant", ID: "one"}
	catalog := hostCatalog(t)
	apps := map[string]string{"linear": "Linear"}
	catalog.SetHostBindingProvider(func(_ context.Context, _ skill.ScopeReference, _ string, actor skill.RunActor) (*skill.HostBindingSet, error) {
		if actor.ActingUser != "7" {
			t.Errorf("ActingUser = %q", actor.ActingUser)
		}
		copied := make(map[string]string, len(apps))
		for key, name := range apps {
			copied[key] = name
		}
		return &skill.HostBindingSet{Fingerprint: "f", PersonalApps: copied}, nil
	})
	actor := skill.RunActor{RunID: "run", AgentID: "agent", Umbrella: skill.Umbrella{Kind: skill.UmbrellaAgent, ID: "agent"}, ActingUser: " 7 "}
	actor = mustRunActor(t, skill.WithRunActor(ctx, actor))
	first, err := catalog.HostBindings(ctx, scope, "agent", actor)
	if err != nil {
		t.Fatal(err)
	}
	if first.PersonalApps["linear"] != "Linear" || len(first.Bindings) != 0 || first.Fingerprint == "" {
		t.Fatalf("set = %#v", first)
	}
	apps["github"] = "GitHub"
	second, err := catalog.HostBindings(ctx, scope, "agent", actor)
	if err != nil {
		t.Fatal(err)
	}
	if second.Fingerprint == first.Fingerprint || len(second.PersonalApps) != 2 {
		t.Fatalf("personal apps did not key the set: %#v", second)
	}
	apps[""] = "Nameless"
	if _, err := catalog.HostBindings(ctx, scope, "agent", actor); err == nil {
		t.Fatal("empty personal app key accepted")
	}
}

func mustRunActor(t *testing.T, ctx context.Context) skill.RunActor {
	t.Helper()
	actor, ok := skill.RunActorFromContext(ctx)
	if !ok || actor.ActingUser != "7" {
		t.Fatalf("WithRunActor did not trim ActingUser: %#v", actor)
	}
	return actor
}
