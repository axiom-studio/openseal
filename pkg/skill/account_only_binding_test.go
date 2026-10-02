package skill

import (
	"testing"
)

func TestAccountOnlyBindingSavesReferenceWithoutExecutableAuthority(t *testing.T) {
	catalog := NewCatalog()
	def := testSkillDefinition()
	def.Prompt = &PromptModule{Instructions: "guidance"}
	if err := catalog.Register(t.Context(), def); err != nil {
		t.Fatal(err)
	}
	scope := ScopeReference{Kind: "tenant", ID: "one"}
	b := &Binding{ID: "account", Scope: scope, DeploymentID: "agent", SkillID: def.ID, SkillVersion: def.Version, MaximumRisk: RiskLevelRead, Credentials: map[string]CredentialReference{"git": {Kind: "git-token", ID: "credential://test-only"}}}
	saved, err := catalog.UpsertBinding(t.Context(), UpsertBindingRequest{Binding: b, Actor: BindingActor{Type: "user", ID: "one"}, Reason: "Connect saved account"})
	if err != nil {
		t.Fatal(err)
	}
	if saved.EnablePrompt || len(saved.AllowedActions) != 0 || saved.Credentials["git"].ID != "credential://test-only" {
		t.Fatalf("unexpected authority: %#v", saved)
	}
	actions, err := catalog.ListModelActions(t.Context(), scope, "agent")
	if err != nil || len(actions) != 0 {
		t.Fatalf("account exposed actions: %#v %v", actions, err)
	}
	if _, err := catalog.Resolve(t.Context(), scope, "agent", def.ID, def.Version, "deploy", BindingReference{ID: saved.ID, Revision: saved.Revision}); err == nil {
		t.Fatal("account-only action executed")
	}
	if _, err := catalog.ResolveExactPrompt(t.Context(), scope, "agent", def.ID, def.Version, BindingReference{ID: saved.ID, Revision: saved.Revision}); err == nil {
		t.Fatal("account-only prompt enabled")
	}
	// An explicit later permission update can enable the requested operation.
	saved.AllowedActions = []string{"deploy"}
	saved.MaximumRisk = RiskLevelProduction
	if _, err := catalog.UpsertBinding(t.Context(), UpsertBindingRequest{Binding: saved, ExpectedRevision: saved.Revision, Actor: BindingActor{Type: "user", ID: "one"}, Reason: "Allow the requested deploy operation"}); err != nil {
		t.Fatal(err)
	}
	actions, err = catalog.ListModelActions(t.Context(), scope, "agent")
	if err != nil || len(actions) != 1 {
		t.Fatalf("explicit action update failed: %#v, %v", actions, err)
	}
	if other, err := catalog.ListBindings(t.Context(), ScopeReference{Kind: "tenant", ID: "two"}, "agent"); err != nil || len(other) != 0 {
		t.Fatalf("cross-tenant bindings: %#v, %v", other, err)
	}
}
func TestAccountOnlyBindingRejectsEmptyUnknownAndMalformedCredentials(t *testing.T) {
	for _, tc := range []struct {
		name string
		refs map[string]CredentialReference
		risk RiskLevel
	}{
		{"empty", nil, RiskLevelRead}, {"unknown", map[string]CredentialReference{"other": {Kind: "git-token", ID: "opaque"}}, RiskLevelRead},
		{"wrong kind", map[string]CredentialReference{"git": {Kind: "other", ID: "opaque"}}, RiskLevelRead},
		{"missing reference", map[string]CredentialReference{"git": {Kind: "git-token"}}, RiskLevelRead},
		{"risk", map[string]CredentialReference{"git": {Kind: "git-token", ID: "opaque"}}, RiskLevelProduction},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := NewCatalog()
			def := testSkillDefinition()
			if err := catalog.Register(t.Context(), def); err != nil {
				t.Fatal(err)
			}
			_, err := catalog.UpsertBinding(t.Context(), UpsertBindingRequest{Binding: &Binding{ID: "account", Scope: ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "agent", SkillID: def.ID, SkillVersion: def.Version, MaximumRisk: tc.risk, Credentials: tc.refs}, Actor: BindingActor{Type: "user", ID: "one"}, Reason: "Connect saved account"})
			if err == nil {
				t.Fatal("invalid account-only binding accepted")
			}
		})
	}
}
