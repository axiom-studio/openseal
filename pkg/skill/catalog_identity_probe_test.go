package skill

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type identityProbeCatalogStore struct {
	*countingCatalogStore
	identityReads atomic.Int64
	projection    func() ([]capability.SkillIdentity, error)
}

func (s *identityProbeCatalogStore) ListSkillDefinitionIdentities(ctx context.Context, id, version string) ([]capability.SkillIdentity, error) {
	s.identityReads.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.projection != nil {
		return s.projection()
	}
	s.bindingManagementStore.mu.Lock()
	defer s.bindingManagementStore.mu.Unlock()
	definitions := s.bindingManagementStore.definitions[id+"\x00"+version]
	identities := make([]capability.SkillIdentity, 0, len(definitions))
	for _, definition := range definitions {
		identities = append(identities, capability.SkillIdentity{ID: definition.ID, Version: definition.Version, SourceIdentity: DefinitionSourceIdentity(definition)})
	}
	return identities, nil
}

func prepareIdentityProbeCatalog(tb testing.TB, builtin bool, actions int) (*Catalog, *Catalog, *identityProbeCatalogStore, *Definition, *Binding) {
	tb.Helper()
	store := &identityProbeCatalogStore{countingCatalogStore: newCountingCatalogStore()}
	writer := NewCatalogWithStore(store)
	definition := cacheTestDefinition(actions)
	if builtin {
		definition.Source = nil
	}
	binding := cacheTestBinding(definition)
	if err := writer.Register(context.Background(), definition); err != nil {
		tb.Fatal(err)
	}
	if err := writer.Bind(context.Background(), binding); err != nil {
		tb.Fatal(err)
	}
	store.identityReads.Store(0)
	store.definitionReads.Store(0)
	store.bindingReads.Store(0)
	return NewCatalogWithStore(store), writer, store, definition, binding
}

func TestCatalogLiveIdentityProbeReusesWarmDefinitionsAndSchemas(t *testing.T) {
	for _, builtin := range []bool{false, true} {
		name := "publisher"
		if builtin {
			name = "source-less-builtin"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			catalog, _, store, definition, _ := prepareIdentityProbeCatalog(t, builtin, 24)
			identity := DefinitionSourceIdentity(definition)
			if _, err := catalog.GetDefinition(ctx, definition.ID, definition.Version); err != nil {
				t.Fatal(err)
			}
			catalog.mu.RLock()
			schemas := catalog.schemas[actionKey(definition.ID, definition.Version, identity, "deploy")]
			catalog.mu.RUnlock()
			for range 3 {
				loaded, err := catalog.GetDefinition(ctx, definition.ID, definition.Version)
				if err != nil || loaded == nil || DefinitionSourceIdentity(loaded) != identity {
					t.Fatalf("warm lookup = %#v, %v", loaded, err)
				}
				// Mutating a returned nested payload must not alter the cache or
				// compiled schema subsequently used by another caller.
				loaded.Prompt.Instructions = "mutated"
				loaded.Actions["deploy"].InputSchema["type"] = "string"
				if err := catalog.ValidateDefinitionInput(ctx, definition.ID, definition.Version, "", "deploy", map[string]interface{}{"environment": "staging", "revision": "abc"}); err != nil {
					t.Fatal(err)
				}
			}
			if got := store.identityReads.Load(); got != 7 {
				t.Fatalf("live identity checks = %d, want every one of 7 lookups", got)
			}
			if got := store.definitionReads.Load(); got != 1 {
				t.Fatalf("immutable payload hydrations = %d, want 1", got)
			}
			catalog.mu.RLock()
			after := catalog.schemas[actionKey(definition.ID, definition.Version, identity, "deploy")]
			catalog.mu.RUnlock()
			if schemas == nil || schemas != after {
				t.Fatal("live probe replaced the warm compiled schema")
			}
			loaded, err := catalog.GetDefinition(ctx, definition.ID, definition.Version)
			if err != nil || loaded.Prompt.Instructions != definition.Prompt.Instructions || loaded.Actions["deploy"].InputSchema["type"] != "object" {
				t.Fatalf("caller mutated cached payload: %#v, %v", loaded, err)
			}
			if err := catalog.ValidateDefinitionInput(ctx, definition.ID, definition.Version, "", "deploy", map[string]interface{}{"environment": "staging"}); err == nil {
				t.Fatal("warm schema accepted a missing required revision")
			}
		})
	}
}

func TestCatalogLiveIdentityProbeSeesLaterPublisherImmediately(t *testing.T) {
	for _, builtin := range []bool{false, true} {
		t.Run(map[bool]string{false: "publisher", true: "builtin"}[builtin], func(t *testing.T) {
			ctx := context.Background()
			catalog, writer, store, definition, _ := prepareIdentityProbeCatalog(t, builtin, 1)
			if _, err := catalog.GetDefinition(ctx, definition.ID, definition.Version); err != nil {
				t.Fatal(err)
			}
			variant := cloneDefinition(definition)
			variant.Source = &SourceProvenance{Identity: "clawhub::@another/release", Format: "openclaw.skill.v1"}
			if err := writer.Register(ctx, variant); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err := catalog.GetDefinition(ctx, definition.ID, definition.Version); !errors.Is(err, ErrDefinitionAmbiguous) {
					t.Fatalf("live publisher collision was hidden: %v", err)
				}
			}
			if got := store.identityReads.Load(); got != 3 {
				t.Fatalf("live probes = %d, want 3", got)
			}
			if got := store.definitionReads.Load(); got != 1 {
				t.Fatalf("ambiguous metadata unnecessarily hydrated payloads: %d", got)
			}
			if !builtin {
				if loaded, err := catalog.GetDefinitionVariant(ctx, definition.ID, definition.Version, DefinitionSourceIdentity(definition)); err != nil || loaded == nil {
					t.Fatalf("qualified positive cache changed: %#v, %v", loaded, err)
				}
				if got := store.identityReads.Load(); got != 3 {
					t.Fatalf("qualified cache unexpectedly probed metadata: %d", got)
				}
			}
		})
	}
}

func TestCatalogLiveIdentityProbeFailuresCannotUseWarmFallback(t *testing.T) {
	ctx := context.Background()
	catalog, _, store, definition, _ := prepareIdentityProbeCatalog(t, false, 1)
	if _, err := catalog.GetDefinition(ctx, definition.ID, definition.Version); err != nil {
		t.Fatal(err)
	}
	identity := capability.SkillIdentity{ID: definition.ID, Version: definition.Version, SourceIdentity: DefinitionSourceIdentity(definition)}
	storeError := errors.New("identity storage unavailable")
	cases := []struct {
		name       string
		identities []capability.SkillIdentity
		err        error
		missing    bool
	}{
		{name: "storage-error", err: storeError},
		{name: "no-current-identity", missing: true},
		{name: "duplicate-exact-key", identities: []capability.SkillIdentity{identity, identity}},
		{name: "wrong-id", identities: []capability.SkillIdentity{{ID: "other", Version: identity.Version, SourceIdentity: identity.SourceIdentity}}},
		{name: "wrong-version", identities: []capability.SkillIdentity{{ID: identity.ID, Version: "different", SourceIdentity: identity.SourceIdentity}}},
		{name: "noncanonical-source", identities: []capability.SkillIdentity{{ID: identity.ID, Version: identity.Version, SourceIdentity: " " + identity.SourceIdentity}}},
		{name: "invalid-source", identities: []capability.SkillIdentity{{ID: identity.ID, Version: identity.Version, SourceIdentity: "native::bad\x00identity"}}},
		{name: "oversized-source", identities: []capability.SkillIdentity{{ID: identity.ID, Version: identity.Version, SourceIdentity: strings.Repeat("a", 2049)}}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store.projection = func() ([]capability.SkillIdentity, error) { return test.identities, test.err }
			before := store.identityReads.Load()
			for range 2 {
				loaded, err := catalog.GetDefinition(ctx, definition.ID, definition.Version)
				if loaded != nil || (err == nil) != test.missing {
					t.Fatalf("invalid live metadata used warmed fallback: %#v, %v", loaded, err)
				}
				if test.err != nil && !errors.Is(err, test.err) {
					t.Fatalf("metadata error changed: %v", err)
				}
			}
			if got := store.identityReads.Load() - before; got != 2 {
				t.Fatalf("metadata miss/error was cached: %d probes", got)
			}
			if got := store.definitionReads.Load(); got != 1 {
				t.Fatalf("metadata failure fell back to full hydration: %d", got)
			}
		})
	}
	store.projection = nil
	if loaded, err := catalog.GetDefinition(ctx, definition.ID, definition.Version); err != nil || loaded == nil {
		t.Fatalf("recovered metadata remained unavailable: %#v, %v", loaded, err)
	}
}

func TestCatalogLiveIdentityProbeColdPayloadMustMatchBeforeCaching(t *testing.T) {
	for _, name := range []string{"missing", "nil", "wrong-source", "wrong-id", "wrong-version", "invalid-schema", "publisher-added-after-probe"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			catalog, _, store, definition, _ := prepareIdentityProbeCatalog(t, false, 1)
			identity := capability.SkillIdentity{ID: definition.ID, Version: definition.Version, SourceIdentity: DefinitionSourceIdentity(definition)}
			store.projection = func() ([]capability.SkillIdentity, error) { return []capability.SkillIdentity{identity}, nil }
			payload := cloneDefinition(definition)
			payloads := []*Definition{payload}
			switch name {
			case "missing":
				payloads = nil
			case "nil":
				payloads = []*Definition{nil}
			case "wrong-source":
				payload.Source = nil
			case "wrong-id":
				payload.ID = "other"
			case "wrong-version":
				payload.Version = "2.0.0"
			case "invalid-schema":
				payload.Actions["deploy"].InputSchema["$ref"] = "https://example.invalid/schema.json"
			case "publisher-added-after-probe":
				second := cloneDefinition(payload)
				second.Source.Identity = "native::late-publisher"
				payloads = append(payloads, second)
			}
			store.bindingManagementStore.definitions[definition.ID+"\x00"+definition.Version] = payloads
			for range 2 {
				if loaded, err := catalog.GetDefinition(ctx, definition.ID, definition.Version); err == nil || loaded != nil {
					t.Fatalf("unproven cold payload was accepted: %#v, %v", loaded, err)
				}
			}
			catalog.mu.RLock()
			definitions, schemas := len(catalog.skills), len(catalog.schemas)
			catalog.mu.RUnlock()
			if definitions != 0 || schemas != 0 {
				t.Fatalf("unproven cold payload populated cache: definitions=%d schemas=%d", definitions, schemas)
			}
			if store.definitionReads.Load() != 2 || store.identityReads.Load() != 2 {
				t.Fatal("failed cold hydration was cached")
			}
		})
	}
}

func TestCatalogLiveIdentityProbeDoesNotCacheBuiltinBindingAuthority(t *testing.T) {
	ctx := context.Background()
	catalog, writer, store, definition, binding := prepareIdentityProbeCatalog(t, true, 1)
	if _, err := catalog.ListModelActions(ctx, binding.Scope, binding.DeploymentID); err != nil {
		t.Fatal(err)
	}
	binding.Revision++
	binding.Credentials["git"] = CredentialReference{Kind: "git-token", ID: "credential://replacement"}
	if err := writer.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	store.bindingReads.Store(0)
	bound, err := catalog.Resolve(ctx, binding.Scope, binding.DeploymentID, definition.ID, definition.Version, "deploy")
	if err != nil || bound.Binding.Credentials["git"].ID != "credential://replacement" || bound.Binding.Revision != 2 {
		t.Fatalf("warm builtin hid current credentials: %#v, %v", bound, err)
	}
	if store.bindingReads.Load() != 1 {
		t.Fatal("live identity shortcut skipped mutable binding read")
	}
	binding.Revision++
	binding.Disabled = true
	if err := writer.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if actions, err := catalog.ListModelActions(ctx, binding.Scope, binding.DeploymentID); err != nil || len(actions) != 0 {
		t.Fatalf("live identity shortcut retained revoked builtin authority: %#v, %v", actions, err)
	}
	if store.definitionReads.Load() != 1 {
		t.Fatal("live binding checks rehydrated warm immutable builtin")
	}
}

func TestCatalogLiveIdentityProbeConcurrentMutationAndPublisherInstall(t *testing.T) {
	ctx := context.Background()
	catalog, writer, store, definition, _ := prepareIdentityProbeCatalog(t, true, 1)
	if _, err := catalog.GetDefinition(ctx, definition.ID, definition.Version); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	failed := make(chan error, 4)
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			<-start
			for range 20 {
				loaded, err := catalog.GetDefinition(ctx, definition.ID, definition.Version)
				if errors.Is(err, ErrDefinitionAmbiguous) {
					continue
				}
				if err != nil {
					failed <- err
					return
				}
				if loaded == nil || loaded.Prompt.Instructions != definition.Prompt.Instructions || loaded.Actions["deploy"].InputSchema["type"] != "object" {
					failed <- errors.New("concurrent caller changed immutable cached definition")
					return
				}
				loaded.Prompt.Instructions = "mutated by concurrent reader"
				loaded.Actions["deploy"].InputSchema["type"] = "string"
			}
		}()
	}
	close(start)
	variant := cloneDefinition(definition)
	variant.Source = &SourceProvenance{Identity: "native::concurrent-publisher", Format: "openclaw.skill.v1"}
	if err := writer.Register(ctx, variant); err != nil {
		t.Fatal(err)
	}
	readers.Wait()
	close(failed)
	for err := range failed {
		t.Fatal(err)
	}
	if store.identityReads.Load() != 81 || store.definitionReads.Load() != 1 {
		t.Fatalf("concurrent probes bypassed freshness or hydrated payloads: probes=%d hydrations=%d", store.identityReads.Load(), store.definitionReads.Load())
	}
	if loaded, err := catalog.GetDefinition(ctx, definition.ID, definition.Version); !errors.Is(err, ErrDefinitionAmbiguous) || loaded != nil {
		t.Fatalf("publisher installed concurrently stayed hidden: %#v, %v", loaded, err)
	}
}

func BenchmarkCatalogLiveIdentityProbe(b *testing.B) {
	for _, probe := range []bool{false, true} {
		name := "full-payload-variants"
		if probe {
			name = "live-identity-projection"
		}
		b.Run(name, func(b *testing.B) {
			_, _, store, definition, _ := prepareIdentityProbeCatalog(b, false, 24)
			var selected CatalogStore = store.countingCatalogStore
			if probe {
				selected = store
			}
			catalog := NewCatalogWithStore(selected)
			ctx := context.Background()
			if _, err := catalog.GetDefinition(ctx, definition.ID, definition.Version); err != nil {
				b.Fatal(err)
			}
			store.identityReads.Store(0)
			store.definitionReads.Store(0)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := catalog.GetDefinition(ctx, definition.ID, definition.Version); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(store.identityReads.Load())/float64(b.N), "identity/op")
			b.ReportMetric(float64(store.definitionReads.Load())/float64(b.N), "hydrate/op")
		})
	}
}
