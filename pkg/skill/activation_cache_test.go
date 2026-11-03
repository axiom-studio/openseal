package skill

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

type countingValidationStore struct {
	*bindingManagementStore
	listings atomic.Int64
}

func (s *countingValidationStore) ListSkillBindings(ctx context.Context, scope ScopeReference, deploymentID string) ([]*Binding, error) {
	s.listings.Add(1)
	return s.bindingManagementStore.ListSkillBindings(ctx, scope, deploymentID)
}

type revisionListingStore struct {
	*countingValidationStore
	revisionListings atomic.Int64
}

func (s *revisionListingStore) ListSkillBindingRevisions(ctx context.Context, scope ScopeReference, deploymentID string) ([]BindingRevision, error) {
	s.revisionListings.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	bindings := make([]*Binding, 0)
	for _, binding := range s.bindings {
		if binding.Scope == scope && binding.DeploymentID == deploymentID {
			bindings = append(bindings, binding)
		}
	}
	return bindingRevisions(bindings), nil
}

func TestActivationSnapshotIsCachedUntilABindingRevisionChanges(t *testing.T) {
	t.Run("payloads", func(t *testing.T) {
		testActivationSnapshotCache(t, &countingValidationStore{bindingManagementStore: newBindingManagementStore()}, nil)
	})
	t.Run("revisions", func(t *testing.T) {
		store := &revisionListingStore{countingValidationStore: &countingValidationStore{bindingManagementStore: newBindingManagementStore()}}
		testActivationSnapshotCache(t, store, store)
	})
}

func testActivationSnapshotCache(t *testing.T, store CatalogStore, revisions *revisionListingStore) {
	ctx := context.Background()
	writer := NewCatalogWithStore(store)
	if err := writer.Register(ctx, bindingToolsDefinition()); err != nil {
		t.Fatal(err)
	}
	binding := bindingToolsBinding("github-binding", "github", bindingToolsTool("search", BindingToolEffectRead))
	if err := writer.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	reader := NewCatalogWithStore(store)
	first, err := reader.Activate(ctx, bindingToolsScope, "agent", HostCapabilityState{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := reader.Activate(ctx, bindingToolsScope, "agent", HostCapabilityState{})
	if err != nil {
		t.Fatal(err)
	}
	if first.SnapshotID != second.SnapshotID || &first.Skills[0] == &second.Skills[0] {
		t.Fatal("warm activation must return an equal snapshot with a caller-owned top level")
	}
	if revisions != nil && (revisions.revisionListings.Load() != 1 || revisions.listings.Load() != 2) {
		// The cold activation lists payloads once for this reader (and once
		// for the writer's Bind); the warm one lists revisions only.
		t.Fatalf("revision listings = %d, payload listings = %d", revisions.revisionListings.Load(), revisions.listings.Load())
	}
	if reader.validated.len() != 1 || reader.activations.len() != 1 {
		t.Fatalf("validated = %d, activations = %d", reader.validated.len(), reader.activations.len())
	}
	if len(reader.bindings) != 0 {
		t.Fatal("a store-backed catalog must not keep its own copy of bindings")
	}
	// Different declared host state is a different cache entry.
	other, err := reader.Activate(ctx, bindingToolsScope, "agent", HostCapabilityState{Revision: "host-2"})
	if err != nil || other.SnapshotID == first.SnapshotID {
		t.Fatalf("host revision did not change the snapshot: %v", err)
	}

	// A write through another catalog changes the revision, so the reader's
	// fingerprint misses even though its own cache was never invalidated.
	updated := cloneBinding(binding)
	updated.Revision = 2
	updated.Tools = append(updated.Tools, bindingToolsTool("list", BindingToolEffectRead))
	if err := writer.Bind(ctx, updated); err != nil {
		t.Fatal(err)
	}
	third, err := reader.Activate(ctx, bindingToolsScope, "agent", HostCapabilityState{})
	if err != nil {
		t.Fatal(err)
	}
	if third.SnapshotID == first.SnapshotID || activatedActions(third)["connector.github.list"].Name == "" {
		t.Fatalf("stale snapshot after a binding write: %#v", activatedActions(third))
	}
	// A write through this catalog drops its cached snapshots at once.
	latest := cloneBinding(updated)
	latest.Revision = 3
	if err := reader.Bind(ctx, latest); err != nil {
		t.Fatal(err)
	}
	if _, ok := reader.activations.get(activationTargetKey(bindingToolsScope, "agent")); ok {
		t.Fatal("binding write did not invalidate the deployment's snapshots")
	}
}

func TestActivationThatInvokesAHostAdapterIsNotCached(t *testing.T) {
	ctx := context.Background()
	scope := ScopeReference{Kind: "tenant", ID: "one"}
	definition := &Definition{
		ID: "resource", Version: "1", Name: "resource", Actions: map[string]Action{},
		Prompt:    &PromptModule{Instructions: "Read {baseDir}/guide.md", UserInvocable: true},
		Resources: []Resource{{Path: "guide.md", Kind: "reference", Digest: strings.Repeat("a", 64), Size: 1}},
		Source:    &SourceProvenance{Format: "test", Digest: strings.Repeat("b", 64)},
	}
	catalog := NewCatalog()
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &Binding{ID: "resource", Scope: scope, DeploymentID: "agent", SkillID: "resource", SkillVersion: "1", EnablePrompt: true, MaximumRisk: RiskLevelRead, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	stager := &activationResourceStager{stage: &ResourceStage{Root: "/sandbox/resource", Revision: "one", Adapter: "sandbox/v1", SourceDigest: strings.Repeat("b", 64), ResourceCount: 1}}
	host := HostCapabilityState{OperatingSystem: "linux", ResourceStager: stager, Adapters: map[string]AdapterCapability{AdapterResourceStaging: {State: AdapterStateAvailable}}}
	for range 2 {
		snapshot, err := catalog.Activate(ctx, scope, "agent", host)
		if err != nil || len(snapshot.Skills) != 1 {
			t.Fatalf("snapshot = %#v, %v", snapshot, err)
		}
	}
	if len(stager.seen) != 2 || catalog.activations.len() != 0 {
		t.Fatalf("stager calls = %d, cached = %d", len(stager.seen), catalog.activations.len())
	}
}

func TestLRUCacheEvictsLeastRecentlyUsed(t *testing.T) {
	cache := newLRUCache[int](2)
	cache.put("a", 1)
	cache.put("b", 2)
	cache.get("a")
	cache.put("c", 3)
	if _, ok := cache.get("b"); ok {
		t.Fatal("least recently used entry was kept")
	}
	if value, ok := cache.get("a"); !ok || value != 1 || cache.len() != 2 {
		t.Fatal("recently used entry was evicted")
	}
}

const (
	benchmarkBindings = 2000
	benchmarkTools    = 20
)

func benchmarkActivationStore(b *testing.B) *bindingManagementStore {
	b.Helper()
	store := newBindingManagementStore()
	if err := store.CreateSkillDefinition(context.Background(), bindingToolsDefinition()); err != nil {
		b.Fatal(err)
	}
	for index := range benchmarkBindings {
		tools := make([]BindingTool, benchmarkTools)
		for tool := range tools {
			effect := BindingToolEffectRead
			if tool%2 == 1 {
				effect = BindingToolEffectWrite
			}
			tools[tool] = bindingToolsTool(fmt.Sprintf("tool_%02d", tool), effect)
			// Distinct schemas per binding, as from distinct services, so the
			// cold path compiles every one instead of reusing compiled schemas.
			tools[tool].InputSchema["description"] = fmt.Sprintf("Input of tool %d of service %d", tool, index)
		}
		binding := bindingToolsBinding(fmt.Sprintf("binding-%04d", index), fmt.Sprintf("service-%04d", index), tools...)
		if err := store.SaveSkillBinding(context.Background(), binding, 0); err != nil {
			b.Fatal(err)
		}
	}
	return store
}

// BenchmarkActivation measures 2,000 bindings with 20 binding tools each.
// Cold activation loads and compiles the definition and validates every
// binding. Warm activation reuses the cached snapshot: warm-payloads after a
// full store listing, warm-revisions after a payload-free revision listing,
// and warm-memory for a catalog without a store.
func BenchmarkActivation(b *testing.B) {
	store := benchmarkActivationStore(b)
	ctx := context.Background()
	activate := func(b *testing.B, catalog *Catalog) {
		snapshot, err := catalog.Activate(ctx, bindingToolsScope, "agent", HostCapabilityState{})
		if err != nil {
			b.Fatal(err)
		}
		if len(snapshot.Skills) != benchmarkBindings || len(snapshot.Skills[0].Actions) != benchmarkTools+1 {
			b.Fatalf("activated %d skills", len(snapshot.Skills))
		}
	}
	b.Run("cold", func(b *testing.B) {
		for b.Loop() {
			activate(b, NewCatalogWithStore(store))
		}
	})
	b.Run("warm-payloads", func(b *testing.B) {
		catalog := NewCatalogWithStore(store)
		activate(b, catalog)
		for b.Loop() {
			activate(b, catalog)
		}
	})
	b.Run("warm-revisions", func(b *testing.B) {
		catalog := NewCatalogWithStore(&revisionListingStore{countingValidationStore: &countingValidationStore{bindingManagementStore: store}})
		activate(b, catalog)
		for b.Loop() {
			activate(b, catalog)
		}
	})
	b.Run("warm-memory", func(b *testing.B) {
		catalog := NewCatalog()
		if err := catalog.Register(ctx, bindingToolsDefinition()); err != nil {
			b.Fatal(err)
		}
		bindings, _ := store.ListSkillBindings(ctx, bindingToolsScope, "agent")
		catalog.mu.Lock()
		for _, binding := range bindings {
			catalog.bindings[bindingKey(binding.Scope, binding.DeploymentID, binding.ID)] = binding
		}
		catalog.mu.Unlock()
		activate(b, catalog)
		for b.Loop() {
			activate(b, catalog)
		}
	})
}
