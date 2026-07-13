package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

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
