package runtime

import (
	"github.com/axiom-studio/openseal/pkg/skill"
	"path/filepath"
	"testing"
)

func TestSQLiteAccountOnlyBindingPersistsWithoutExecutionAuthority(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog := skill.NewCatalogWithStore(store)
	def := redditSkillDefinition()
	if err := catalog.Register(t.Context(), def); err != nil {
		t.Fatal(err)
	}
	scope := skill.ScopeReference{Kind: "tenant", ID: "one"}
	saved, err := catalog.UpsertBinding(t.Context(), skill.UpsertBindingRequest{Binding: &skill.Binding{ID: "account", Scope: scope, DeploymentID: "agent", SkillID: def.ID, SkillVersion: def.Version, MaximumRisk: skill.RiskLevelRead, Credentials: map[string]skill.CredentialReference{"reddit": {Kind: "reddit-oauth", ID: "credential://test-only"}}}, Actor: skill.BindingActor{Type: "user", ID: "one"}, Reason: "Connect account"})
	if err != nil {
		t.Fatal(err)
	}
	fresh := skill.NewCatalogWithStore(store)
	restored, err := fresh.GetBinding(t.Context(), scope, "agent", saved.ID)
	if err != nil || restored == nil || restored.EnablePrompt || len(restored.AllowedActions) != 0 || restored.Credentials["reddit"].ID != "credential://test-only" {
		t.Fatalf("restored account: %#v, %v", restored, err)
	}
	actions, err := fresh.ListModelActions(t.Context(), scope, "agent")
	if err != nil || len(actions) != 0 {
		t.Fatalf("account granted actions: %#v, %v", actions, err)
	}
	if _, err := fresh.ResolveExactPrompt(t.Context(), scope, "agent", def.ID, def.Version, skill.BindingReference{ID: saved.ID, Revision: saved.Revision}); err == nil {
		t.Fatal("account granted prompt access")
	}
	foreign, err := store.ListSkillBindings(t.Context(), skill.ScopeReference{Kind: "tenant", ID: "two"}, "agent")
	if err != nil || len(foreign) != 0 {
		t.Fatalf("foreign bindings: %#v, %v", foreign, err)
	}
}
