package runtime

import (
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

// Deleting a binding removes its row in every durable store. Nothing resolves
// through it afterwards, and its ID can carry a different Skill identity.
func TestDeleteSkillBindingRemovesAuthorityAndFreesTheID(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
		store, ok := kernel.(skill.CatalogStore)
		if !ok {
			t.Fatal("store does not persist Skill bindings")
		}
		ctx := t.Context()
		base, _ := governedActionCatalog(t)
		definition, err := base.GetDefinition(ctx, "release", "1.0.0")
		if err != nil {
			t.Fatal(err)
		}
		catalog := skill.NewCatalogWithStore(store)
		if err := catalog.Register(ctx, definition); err != nil {
			t.Fatal(err)
		}
		next := *definition
		next.Version = "1.1.0"
		if err := catalog.Register(ctx, &next); err != nil {
			t.Fatal(err)
		}
		scope := skill.ScopeReference{Kind: "tenant", ID: "binding-delete-" + uuid.NewString()}
		actor := skill.BindingActor{Type: "system", ID: "test"}
		binding := &skill.Binding{ID: "release-binding", Scope: scope, DeploymentID: "release-agent", SkillID: "release", SkillVersion: "1.0.0",
			AllowedActions: []string{"deploy"}, MaximumRisk: skill.RiskLevelProduction,
			Credentials: map[string]skill.CredentialReference{"token": {Kind: "api-token", ID: "release-secret"}}}
		created, err := catalog.UpsertBinding(ctx, skill.UpsertBindingRequest{Binding: binding, Actor: actor, Reason: "create"})
		if err != nil {
			t.Fatal(err)
		}
		disabled, err := catalog.DisableBinding(ctx, skill.DisableBindingRequest{Scope: scope, DeploymentID: "release-agent", BindingID: created.ID,
			ExpectedRevision: created.Revision, Actor: actor, Reason: "retire"})
		if err != nil {
			t.Fatal(err)
		}
		request := skill.DeleteBindingRequest{Scope: scope, DeploymentID: "release-agent", BindingID: created.ID, ExpectedRevision: created.Revision, Actor: actor, Reason: "remove"}
		if _, err := catalog.DeleteBinding(ctx, request); !errors.Is(err, skill.ErrBindingRevisionConflict) {
			t.Fatalf("stale delete = %v", err)
		}
		if err := store.DeleteSkillBinding(ctx, scope, "release-agent", created.ID, created.Revision); !errors.Is(err, skill.ErrBindingRevisionConflict) {
			t.Fatalf("stale store delete = %v", err)
		}
		request.ExpectedRevision = disabled.Revision
		receipt, err := catalog.DeleteBinding(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		last := receipt.Lifecycle[len(receipt.Lifecycle)-1]
		if !receipt.Disabled || receipt.Revision != disabled.Revision+1 || last.Action != skill.BindingLifecycleDeleted || last.Revision != receipt.Revision || last.Reason != "remove" {
			t.Fatalf("delete receipt = %#v", receipt)
		}
		if _, err := catalog.DeleteBinding(ctx, request); !errors.Is(err, skill.ErrBindingNotFound) {
			t.Fatalf("repeated delete = %v", err)
		}
		if err := store.DeleteSkillBinding(ctx, scope, "release-agent", created.ID, disabled.Revision); !errors.Is(err, skill.ErrBindingNotFound) {
			t.Fatalf("repeated store delete = %v", err)
		}
		listed, err := skill.NewCatalogWithStore(store).ListBindings(ctx, scope, "release-agent")
		if err != nil || len(listed) != 0 {
			t.Fatalf("bindings after delete = %#v, %v", listed, err)
		}

		floor, err := store.SkillBindingRevisionFloor(ctx, scope, "release-agent", created.ID)
		if err != nil || floor != receipt.Revision {
			t.Fatalf("revision floor = %d, %v; want the deletion revision %d", floor, err, receipt.Revision)
		}

		// The ID is free for the same or another Skill identity, without a Skill
		// reference upgrade. A creation that ignores the floor is refused.
		stale := *binding
		stale.Revision, stale.Lifecycle, stale.CreatedAt, stale.UpdatedAt = 1, nil, time.Time{}, time.Time{}
		if err := store.SaveSkillBinding(ctx, &stale, 0); !errors.Is(err, skill.ErrBindingRevisionConflict) {
			t.Fatalf("creation below the revision floor = %v", err)
		}
		recreated, err := catalog.UpsertBinding(ctx, skill.UpsertBindingRequest{Binding: binding, Actor: actor, Reason: "recreate"})
		if err != nil {
			t.Fatal(err)
		}
		if recreated.Revision != receipt.Revision+1 || recreated.SkillVersion != "1.0.0" || len(recreated.Lifecycle) != 1 ||
			recreated.Lifecycle[0].Action != skill.BindingLifecycleCreated || recreated.Lifecycle[0].Revision != recreated.Revision {
			t.Fatalf("recreated binding = %#v", recreated)
		}
		// Same ID, Skill and version: a reference to any revision of the deleted
		// binding is still rejected.
		for revision := int64(1); revision <= receipt.Revision; revision++ {
			if _, err := catalog.Resolve(ctx, scope, "release-agent", "release", "1.0.0", "deploy", skill.BindingReference{ID: created.ID, Revision: revision}); !errors.Is(err, skill.ErrBindingUnavailable) {
				t.Fatalf("reference to deleted revision %d = %v", revision, err)
			}
		}
		if _, err := catalog.Resolve(ctx, scope, "release-agent", "release", "1.0.0", "deploy", skill.BindingReference{ID: recreated.ID, Revision: recreated.Revision}); err != nil {
			t.Fatalf("recreated binding reference = %v", err)
		}

		// A second deletion raises the floor again; the next binding of another
		// version starts above it.
		second, err := catalog.DeleteBinding(ctx, skill.DeleteBindingRequest{Scope: scope, DeploymentID: "release-agent", BindingID: recreated.ID,
			ExpectedRevision: recreated.Revision, Actor: actor, Reason: "remove again"})
		if err != nil {
			t.Fatal(err)
		}
		binding.SkillVersion = "1.1.0"
		recreated, err = catalog.UpsertBinding(ctx, skill.UpsertBindingRequest{Binding: binding, Actor: actor, Reason: "recreate on 1.1.0"})
		if err != nil || recreated.Revision != second.Revision+1 || recreated.SkillVersion != "1.1.0" {
			t.Fatalf("recreated 1.1.0 binding = %#v, %v", recreated, err)
		}
		if _, err := catalog.Resolve(ctx, scope, "release-agent", "release", "1.0.0", "deploy", skill.BindingReference{ID: created.ID, Revision: created.Revision}); !errors.Is(err, skill.ErrBindingUnavailable) {
			t.Fatalf("deleted binding reference = %v", err)
		}
		if _, err := catalog.Resolve(ctx, scope, "release-agent", "release", "1.1.0", "deploy", skill.BindingReference{ID: recreated.ID, Revision: recreated.Revision}); err != nil {
			t.Fatalf("recreated binding reference = %v", err)
		}

		// Deleting an enabled binding removes its model authority at once.
		if _, err := catalog.DeleteBinding(ctx, skill.DeleteBindingRequest{Scope: scope, DeploymentID: "release-agent", BindingID: recreated.ID,
			ExpectedRevision: recreated.Revision, Actor: actor, Reason: "remove"}); err != nil {
			t.Fatal(err)
		}
		actions, err := catalog.ListModelActions(ctx, scope, "release-agent")
		if err != nil || len(actions) != 0 {
			t.Fatalf("model actions after delete = %#v, %v", actions, err)
		}
		if _, err := catalog.Resolve(ctx, scope, "release-agent", "release", "1.1.0", "deploy"); err == nil {
			t.Fatal("deleted binding still resolves")
		}
	})
}
