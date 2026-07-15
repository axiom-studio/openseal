package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestCatalogCanDisableLegacyBindingAfterSourceBecomesAmbiguous(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "legacy-binding.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	catalog := skill.NewCatalogWithStore(store)
	for _, sourceIdentity := range []string{"clawhub::@alice/image", "clawhub::@bob/image"} {
		definition := &skill.Definition{
			ID: "image", Version: "1.0.0", Name: "Image", Description: "Render an image.",
			Source: &skill.SourceProvenance{Identity: sourceIdentity, Format: "openclaw.skill.v1"},
			Prompt: &skill.PromptModule{Instructions: "Render an image."},
		}
		if err := catalog.Register(ctx, definition); err != nil {
			t.Fatal(err)
		}
	}
	scope := skill.ScopeReference{Kind: "tenant", ID: "one"}
	legacy := &skill.Binding{
		ID: "legacy", Scope: scope, DeploymentID: "artist", SkillID: "image", SkillVersion: "1.0.0",
		EnablePrompt: true, MaximumRisk: skill.RiskLevelRead, Revision: 1,
	}
	if err := store.SaveSkillBinding(ctx, legacy, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Activate(ctx, scope, "artist", skill.HostCapabilityState{OperatingSystem: "linux"}); !errors.Is(err, skill.ErrDefinitionAmbiguous) {
		t.Fatalf("active legacy binding error = %v", err)
	}
	disabled := *legacy
	disabled.Disabled = true
	disabled.Revision = 2
	if err := catalog.Bind(ctx, &disabled); err != nil {
		t.Fatal(err)
	}

	restarted := skill.NewCatalogWithStore(store)
	snapshot, err := restarted.Activate(ctx, scope, "artist", skill.HostCapabilityState{OperatingSystem: "linux"})
	if err != nil || snapshot == nil || len(snapshot.Skills) != 0 || len(snapshot.Unavailable) != 0 {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	bindings, err := store.ListSkillBindings(ctx, scope, "artist")
	if err != nil || len(bindings) != 1 || !bindings[0].Disabled || bindings[0].Revision != 2 || bindings[0].SourceIdentity != "" {
		t.Fatalf("bindings=%#v err=%v", bindings, err)
	}
}

func TestCatalogCannotRetargetWhileDisabling(t *testing.T) {
	catalog := skill.NewCatalog()
	definition := &skill.Definition{ID: "one", Version: "1", Name: "One", Description: "One.", Prompt: &skill.PromptModule{Instructions: "One."}}
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	scope := skill.ScopeReference{Kind: "tenant", ID: "one"}
	binding := &skill.Binding{ID: "binding", Scope: scope, DeploymentID: "agent", SkillID: "one", SkillVersion: "1", EnablePrompt: true, MaximumRisk: skill.RiskLevelRead, Revision: 1}
	if err := catalog.Bind(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	disabled := *binding
	disabled.Disabled, disabled.Revision, disabled.SkillID = true, 2, "other"
	if err := catalog.Bind(context.Background(), &disabled); err == nil {
		t.Fatal("disabled binding changed its skill identity")
	}
}
