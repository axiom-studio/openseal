package runtime

import (
	"context"
	"errors"
	"path/filepath"
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
	updated.Config = map[string]interface{}{"region": "us-east"}
	if err := restarted.Bind(context.Background(), &updated); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Bind(context.Background(), &updated); !errors.Is(err, skill.ErrBindingRevisionConflict) {
		t.Fatalf("stale binding update = %v", err)
	}
	freshCatalog := skill.NewCatalogWithStore(reopened)
	if err := freshCatalog.Register(context.Background(), definition); !errors.Is(err, skill.ErrDefinitionImmutable) {
		t.Fatalf("immutable definition overwrite = %v", err)
	}
}
