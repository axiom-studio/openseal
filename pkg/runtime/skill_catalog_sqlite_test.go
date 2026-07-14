package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSQLiteSkillCatalogRestoresSourceQualifiedVariants(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "skill-variants.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	catalog := skill.NewCatalogWithStore(store)
	identities := []string{"clawhub::@alice/research", "clawhub::@bob/research"}
	for _, identity := range identities {
		definition := &skill.Definition{
			ID: "research", Version: "1.0.0", Name: "Research",
			Source: &skill.SourceProvenance{Identity: identity, Format: "openclaw.skill.v1"},
			Prompt: &skill.PromptModule{Instructions: "Research using the exact installed source."},
		}
		if err := catalog.Register(ctx, definition); err != nil {
			t.Fatal(err)
		}
	}
	scope := skill.ScopeReference{Kind: "tenant", ID: "one"}
	for index, identity := range identities {
		if err := catalog.Bind(ctx, &skill.Binding{
			ID: []string{"alice", "bob"}[index], Scope: scope, DeploymentID: "analyst",
			SkillID: "research", SkillVersion: "1.0.0", SourceIdentity: identity,
			EnablePrompt: true, MaximumRisk: skill.RiskLevelRead, Revision: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := skill.NewCatalogWithStore(reopened)
	if _, err := restarted.GetDefinition(ctx, "research", "1.0.0"); !errors.Is(err, skill.ErrDefinitionAmbiguous) {
		t.Fatalf("restarted unqualified lookup = %v", err)
	}
	prompts, err := restarted.ListModelPrompts(ctx, scope, "analyst")
	if err != nil || len(prompts) != 2 || prompts[0].BindingID == prompts[1].BindingID {
		t.Fatalf("restarted prompt projections = %#v, %v", prompts, err)
	}
	for index, bindingID := range []string{"alice", "bob"} {
		definition, err := restarted.GetDefinitionVariant(ctx, "research", "1.0.0", identities[index])
		if err != nil || skill.DefinitionSourceIdentity(definition) != identities[index] {
			t.Fatalf("variant %s = %#v, %v", identities[index], definition, err)
		}
		prompt, err := restarted.ResolveExactPrompt(ctx, scope, "analyst", "research", "1.0.0", skill.BindingReference{ID: bindingID, Revision: 1})
		if err != nil || prompt == nil {
			t.Fatalf("exact restarted prompt %s = %#v, %v", bindingID, prompt, err)
		}
	}
}

func TestSQLiteSkillCatalogMigratesLegacyIdentitySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-skills.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	definition := &skill.Definition{ID: "legacy", Version: "1", Name: "Legacy", Prompt: &skill.PromptModule{Instructions: "Preserve me."}}
	binding := &skill.Binding{
		ID: "legacy", Scope: skill.ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "agent",
		SkillID: "legacy", SkillVersion: "1", EnablePrompt: true, MaximumRisk: skill.RiskLevelRead, Revision: 1,
	}
	definitionPayload, _ := json.Marshal(definition)
	bindingPayload, _ := json.Marshal(binding)
	if _, err = db.Exec(`
		CREATE TABLE skill_definitions (id TEXT NOT NULL, version TEXT NOT NULL, payload TEXT NOT NULL, PRIMARY KEY (id, version));
		CREATE TABLE skill_bindings (scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, deployment_id TEXT NOT NULL, id TEXT NOT NULL, skill_id TEXT NOT NULL, skill_version TEXT NOT NULL, revision INTEGER NOT NULL, payload TEXT NOT NULL, PRIMARY KEY (scope_kind, scope_id, deployment_id, id));
		INSERT INTO skill_definitions(id, version, payload) VALUES (?, ?, ?);
		INSERT INTO skill_bindings(scope_kind, scope_id, deployment_id, id, skill_id, skill_version, revision, payload) VALUES (?, ?, ?, ?, ?, ?, ?, ?);
	`, definition.ID, definition.Version, string(definitionPayload), binding.Scope.Kind, binding.Scope.ID, binding.DeploymentID, binding.ID, binding.SkillID, binding.SkillVersion, binding.Revision, string(bindingPayload)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog := skill.NewCatalogWithStore(store)
	loaded, err := catalog.GetDefinition(context.Background(), "legacy", "1")
	if err != nil || loaded == nil || skill.DefinitionSourceIdentity(loaded) != "" {
		t.Fatalf("migrated definition = %#v, %v", loaded, err)
	}
	prompts, err := catalog.ListModelPrompts(context.Background(), binding.Scope, binding.DeploymentID)
	if err != nil || len(prompts) != 1 || prompts[0].BindingID != binding.ID {
		t.Fatalf("migrated binding = %#v, %v", prompts, err)
	}
}

func TestSQLiteSkillCatalogRestoresBindingsAndEnforcesRevisions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "skills.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	catalog := skill.NewCatalogWithStore(store)
	definition := &skill.Definition{
		ID: "research", Version: "1.0.0", Name: "Research",
		Prompt: &skill.PromptModule{Instructions: "Research with cited evidence."},
	}
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	scope := skill.ScopeReference{Kind: "tenant", ID: "one"}
	binding := &skill.Binding{ID: "research", Scope: scope, DeploymentID: "analyst", SkillID: definition.ID, SkillVersion: definition.Version, EnablePrompt: true, MaximumRisk: skill.RiskLevelRead, Revision: 1}
	if err := catalog.Bind(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := skill.NewCatalogWithStore(reopened)
	loaded, err := restarted.GetDefinition(context.Background(), definition.ID, definition.Version)
	if err != nil || loaded == nil || loaded.Prompt == nil || loaded.Prompt.Instructions != definition.Prompt.Instructions {
		t.Fatalf("restored definition = %#v, %v", loaded, err)
	}
	prompts, err := restarted.ListModelPrompts(context.Background(), scope, "analyst")
	if err != nil || len(prompts) != 1 || prompts[0].SkillID != definition.ID {
		t.Fatalf("restored prompts = %#v, %v", prompts, err)
	}
	resolved, err := restarted.ResolvePrompt(context.Background(), scope, "analyst", definition.ID, definition.Version)
	if err != nil || resolved.Instructions != definition.Prompt.Instructions {
		t.Fatalf("restored prompt = %#v, %v", resolved, err)
	}

	updated := *binding
	updated.Revision = 2
	updated.Disabled = true
	updated.Config = map[string]interface{}{"region": "us-east"}
	if err := restarted.Bind(context.Background(), &updated); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Bind(context.Background(), &updated); !errors.Is(err, skill.ErrBindingRevisionConflict) {
		t.Fatalf("stale binding update = %v", err)
	}
	freshCatalog := skill.NewCatalogWithStore(reopened)
	if prompts, err := freshCatalog.ListModelPrompts(context.Background(), scope, "analyst"); err != nil || len(prompts) != 0 {
		t.Fatalf("disabled binding survived restart as active: %#v, %v", prompts, err)
	}
	if err := freshCatalog.Register(context.Background(), definition); !errors.Is(err, skill.ErrDefinitionImmutable) {
		t.Fatalf("immutable definition overwrite = %v", err)
	}
}

func TestSQLiteSkillCatalogRestoresPromptCredentialContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt-credentials.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	definition := &skill.Definition{
		ID: "publisher", Version: "1.0.0", Name: "Publisher", Actions: map[string]skill.Action{},
		Prompt: &skill.PromptModule{
			Instructions: "Prepare authenticated publishing guidance.", UserInvocable: true,
			Credentials: []skill.CredentialRequirement{{Name: "PUBLISH_TOKEN", Kind: "environment-secret"}},
		},
	}
	catalog := skill.NewCatalogWithStore(store)
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	scope := skill.ScopeReference{Kind: "tenant", ID: "one"}
	if err := catalog.Bind(context.Background(), &skill.Binding{
		ID: "publisher", Scope: scope, DeploymentID: "marketing", SkillID: definition.ID, SkillVersion: definition.Version,
		EnablePrompt: true, MaximumRisk: skill.RiskLevelRead,
		Credentials: map[string]skill.CredentialReference{"PUBLISH_TOKEN": {Kind: "environment-secret", ID: "opaque-binding-a"}}, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := skill.NewCatalogWithStore(reopened)
	loaded, err := restarted.GetDefinition(context.Background(), definition.ID, definition.Version)
	if err != nil || loaded == nil || loaded.Prompt == nil || len(loaded.Prompt.Credentials) != 1 || loaded.Prompt.Credentials[0].Name != "PUBLISH_TOKEN" {
		t.Fatalf("restored prompt credential contract = %#v, %v", loaded, err)
	}
	snapshot, err := restarted.Activate(context.Background(), scope, "marketing", skill.HostCapabilityState{})
	if err != nil || len(snapshot.Skills) != 1 || len(snapshot.Unavailable) != 0 || snapshot.Skills[0].Prompt == nil || len(snapshot.Skills[0].Prompt.Credentials) != 0 {
		t.Fatalf("restored prompt activation = %#v, %v", snapshot, err)
	}
	resolved, err := restarted.ResolvePrompt(context.Background(), scope, "marketing", definition.ID, definition.Version)
	if err != nil || resolved == nil || len(resolved.Credentials) != 0 {
		t.Fatalf("resolved model prompt exposed credential contract = %#v, %v", resolved, err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"opaque-binding-a", "PUBLISH_TOKEN"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("restored activation exposed %q: %s", forbidden, encoded)
		}
	}
}
