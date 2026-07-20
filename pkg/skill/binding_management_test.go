package skill

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestCanonicalBindingManagementIsScopedCASAuditedAndCredentialExplicit(t *testing.T) {
	ctx := context.Background()
	catalog := NewCatalog()
	definition := testSkillDefinition()
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	scope := ScopeReference{Kind: "tenant", ID: "one"}
	proposed := &Binding{
		ID: "production-git", Scope: scope, DeploymentID: "release-agent", SkillID: definition.ID, SkillVersion: definition.Version,
		AllowedActions: []string{"deploy"}, MaximumRisk: RiskLevelProduction,
		Credentials: map[string]CredentialReference{"git": {Kind: "git-token", ID: "vault://tenant/one/git/production"}},
		Revision:    99, Lifecycle: []BindingLifecycleEntry{{Revision: 99, Action: BindingLifecycleDisabled, Actor: BindingActor{Type: "user", ID: "spoofed"}, Reason: "spoofed"}},
	}
	created, err := catalog.UpsertBinding(ctx, UpsertBindingRequest{
		Binding: proposed, ExpectedRevision: 0, Actor: BindingActor{Type: " user ", ID: " admin "}, Reason: " connect production Git credential ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Revision != 1 || created.CreatedAt.IsZero() || !created.UpdatedAt.Equal(created.CreatedAt) || len(created.Lifecycle) != 1 ||
		created.Lifecycle[0].Action != BindingLifecycleCreated || created.Lifecycle[0].Actor.Type != "user" || created.Lifecycle[0].Actor.ID != "admin" ||
		created.Lifecycle[0].Reason != "connect production Git credential" {
		t.Fatalf("created binding = %#v", created)
	}
	if reference := created.Credentials["git"]; reference.Kind != "git-token" || reference.ID != "vault://tenant/one/git/production" {
		t.Fatalf("opaque credential reference = %#v", reference)
	}
	proposed.Revision = 1
	proposed.Lifecycle = nil
	updated, err := catalog.UpsertBinding(ctx, UpsertBindingRequest{
		Binding: proposed, ExpectedRevision: 1, Actor: BindingActor{Type: "user", ID: "admin"}, Reason: "confirm deployment policy",
	})
	if err != nil || updated.Revision != 2 || len(updated.Lifecycle) != 2 || updated.Lifecycle[1].Action != BindingLifecycleUpdated {
		t.Fatalf("updated binding = %#v, err = %v", updated, err)
	}
	if _, err := catalog.DisableBinding(ctx, DisableBindingRequest{
		Scope: scope, DeploymentID: "release-agent", BindingID: created.ID, ExpectedRevision: 1,
		Actor: BindingActor{Type: "user", ID: "admin"}, Reason: "rotate account",
	}); !errors.Is(err, ErrBindingRevisionConflict) {
		t.Fatalf("stale disable = %v", err)
	}
	disabled, err := catalog.DisableBinding(ctx, DisableBindingRequest{
		Scope: scope, DeploymentID: "release-agent", BindingID: created.ID, ExpectedRevision: 2,
		Actor: BindingActor{Type: "user", ID: "admin"}, Reason: "rotate account",
	})
	if err != nil || !disabled.Disabled || disabled.Revision != 3 || len(disabled.Lifecycle) != 3 || disabled.Lifecycle[2].Action != BindingLifecycleDisabled {
		t.Fatalf("disabled binding = %#v, err = %v", disabled, err)
	}
	listed, err := catalog.ListBindings(ctx, scope, "release-agent")
	if err != nil || len(listed) != 1 || !listed[0].Disabled {
		t.Fatalf("listed bindings = %#v, err = %v", listed, err)
	}
	other, err := catalog.GetBinding(ctx, ScopeReference{Kind: "tenant", ID: "two"}, "release-agent", created.ID)
	if err != nil || other != nil {
		t.Fatalf("cross-scope binding = %#v, err = %v", other, err)
	}
	actions, err := catalog.ListModelActions(ctx, scope, "release-agent")
	if err != nil || len(actions) != 0 {
		t.Fatalf("disabled binding actions = %#v, err = %v", actions, err)
	}
}

func TestCanonicalBindingManagementPersistsLifecycleAcrossCatalogRestart(t *testing.T) {
	ctx := context.Background()
	store := newBindingManagementStore()
	first := NewCatalogWithStore(store)
	definition := testSkillDefinition()
	if err := first.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	scope := ScopeReference{Kind: "tenant", ID: "one"}
	created, err := first.UpsertBinding(ctx, UpsertBindingRequest{
		Binding: &Binding{ID: "git", Scope: scope, DeploymentID: "agent", SkillID: definition.ID, SkillVersion: definition.Version,
			AllowedActions: []string{"deploy"}, MaximumRisk: RiskLevelProduction,
			Credentials: map[string]CredentialReference{"git": {Kind: "git-token", ID: "vault://git"}}},
		Actor: BindingActor{Type: "user", ID: "admin"}, Reason: "initial assignment",
	})
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewCatalogWithStore(store)
	loaded, err := restarted.GetBinding(ctx, scope, "agent", "git")
	if err != nil || loaded == nil || loaded.Revision != created.Revision || len(loaded.Lifecycle) != 1 || loaded.Lifecycle[0].Reason != "initial assignment" {
		t.Fatalf("restarted binding = %#v, err = %v", loaded, err)
	}
	if _, err := restarted.UpsertBinding(ctx, UpsertBindingRequest{
		Binding: loaded, ExpectedRevision: loaded.Revision, Actor: BindingActor{Type: "workload", ID: "reconciler"}, Reason: "reconcile assignment",
	}); err != nil {
		t.Fatal(err)
	}
	third := NewCatalogWithStore(store)
	values, err := third.ListBindings(ctx, scope, "agent")
	if err != nil || len(values) != 1 || values[0].Revision != 2 || len(values[0].Lifecycle) != 2 || values[0].Lifecycle[1].Actor.ID != "reconciler" {
		t.Fatalf("persisted binding lifecycle = %#v, err = %v", values, err)
	}
}

func TestCanonicalBindingManagementSerializesConcurrentCAS(t *testing.T) {
	ctx := context.Background()
	catalog := NewCatalog()
	definition := testSkillDefinition()
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	binding, err := catalog.UpsertBinding(ctx, UpsertBindingRequest{
		Binding: &Binding{ID: "git", Scope: ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "agent", SkillID: definition.ID, SkillVersion: definition.Version,
			AllowedActions: []string{"deploy"}, MaximumRisk: RiskLevelProduction, Credentials: map[string]CredentialReference{"git": {Kind: "git-token", ID: "vault://git"}}},
		Actor: BindingActor{Type: "user", ID: "admin"}, Reason: "create",
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, actor := range []string{"one", "two"} {
		wg.Add(1)
		go func(actor string) {
			defer wg.Done()
			candidate := cloneBinding(binding)
			_, updateErr := catalog.UpsertBinding(ctx, UpsertBindingRequest{
				Binding: candidate, ExpectedRevision: 1, Actor: BindingActor{Type: "user", ID: actor}, Reason: "concurrent update",
			})
			results <- updateErr
		}(actor)
	}
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for result := range results {
		switch {
		case result == nil:
			successes++
		case errors.Is(result, ErrBindingRevisionConflict):
			conflicts++
		default:
			t.Fatalf("concurrent update error = %v", result)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent results: successes=%d conflicts=%d", successes, conflicts)
	}
}

type bindingManagementStore struct {
	mu          sync.Mutex
	definitions map[string][]*Definition
	bindings    map[string]*Binding
}

func newBindingManagementStore() *bindingManagementStore {
	return &bindingManagementStore{definitions: make(map[string][]*Definition), bindings: make(map[string]*Binding)}
}

func (s *bindingManagementStore) CreateSkillDefinition(_ context.Context, definition *Definition) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := definition.ID + "\x00" + definition.Version
	s.definitions[key] = append(s.definitions[key], cloneDefinition(definition))
	return nil
}

func (s *bindingManagementStore) ListSkillDefinitionVariants(_ context.Context, id, version string) ([]*Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	values := s.definitions[id+"\x00"+version]
	result := make([]*Definition, 0, len(values))
	for _, value := range values {
		result = append(result, cloneDefinition(value))
	}
	return result, nil
}

func (s *bindingManagementStore) SaveSkillBinding(_ context.Context, binding *Binding, expectedRevision int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := bindingKey(binding.Scope, binding.DeploymentID, binding.ID)
	current := s.bindings[key]
	if current == nil && expectedRevision != 0 || current != nil && current.Revision != expectedRevision || binding.Revision != expectedRevision+1 {
		return ErrBindingRevisionConflict
	}
	s.bindings[key] = cloneBinding(binding)
	return nil
}

func (s *bindingManagementStore) ListSkillBindings(_ context.Context, scope ScopeReference, deploymentID string) ([]*Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]*Binding, 0)
	for _, binding := range s.bindings {
		if binding.Scope == scope && binding.DeploymentID == deploymentID {
			result = append(result, cloneBinding(binding))
		}
	}
	return result, nil
}

var _ CatalogStore = (*bindingManagementStore)(nil)
