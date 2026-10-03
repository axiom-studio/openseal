package skill

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

type countingCatalogStore struct {
	*bindingManagementStore
	definitionReads atomic.Int64
	bindingReads    atomic.Int64
}

func newCountingCatalogStore() *countingCatalogStore {
	return &countingCatalogStore{bindingManagementStore: newBindingManagementStore()}
}

func (s *countingCatalogStore) ListSkillDefinitionVariants(ctx context.Context, id, version string) ([]*Definition, error) {
	s.definitionReads.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.bindingManagementStore.ListSkillDefinitionVariants(ctx, id, version)
}

func (s *countingCatalogStore) ListSkillBindings(ctx context.Context, scope ScopeReference, deploymentID string) ([]*Binding, error) {
	s.bindingReads.Add(1)
	return s.bindingManagementStore.ListSkillBindings(ctx, scope, deploymentID)
}

func cacheTestDefinition(actionCount int) *Definition {
	definition := testSkillDefinition()
	definition.Source = &SourceProvenance{Identity: "clawhub::@publisher/release", Format: "openclaw.skill.v1"}
	definition.Prompt = &PromptModule{Instructions: "Use the authorized release operations.", UserInvocable: true}
	for i := 1; i < actionCount; i++ {
		action := definition.Actions["deploy"]
		action.Name = fmt.Sprintf("operation-%02d", i)
		definition.Actions[action.Name] = action
	}
	return definition
}

func cacheTestBinding(definition *Definition) *Binding {
	return &Binding{
		ID: "release", Scope: ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "operator",
		SkillID: definition.ID, SkillVersion: definition.Version, SourceIdentity: DefinitionSourceIdentity(definition),
		AllowedActions: []string{"deploy"}, EnablePrompt: true, MaximumRisk: RiskLevelProduction,
		Credentials: map[string]CredentialReference{"git": {Kind: "git-token", ID: "credential://git"}}, Revision: 1,
	}
}

func prepareCacheCatalog(tb testing.TB, actionCount int) (*Catalog, *Catalog, *countingCatalogStore, *Definition, *Binding) {
	tb.Helper()
	ctx := context.Background()
	store := newCountingCatalogStore()
	writer := NewCatalogWithStore(store)
	definition := cacheTestDefinition(actionCount)
	binding := cacheTestBinding(definition)
	if err := writer.Register(ctx, definition); err != nil {
		tb.Fatal(err)
	}
	if err := writer.Bind(ctx, binding); err != nil {
		tb.Fatal(err)
	}
	store.definitionReads.Store(0)
	store.bindingReads.Store(0)
	return NewCatalogWithStore(store), writer, store, definition, binding
}

func TestCatalogWarmQualifiedDefinitionReusesImmutableSchemas(t *testing.T) {
	ctx := context.Background()
	catalog, _, store, definition, binding := prepareCacheCatalog(t, 24)
	identity := DefinitionSourceIdentity(definition)
	if _, err := catalog.GetDefinitionVariant(ctx, definition.ID, definition.Version, identity); err != nil {
		t.Fatal(err)
	}
	catalog.mu.RLock()
	schemas := catalog.schemas[actionKey(definition.ID, definition.Version, identity, "deploy")]
	catalog.mu.RUnlock()
	for range 3 {
		if _, err := catalog.GetDefinitionVariant(ctx, definition.ID, definition.Version, identity); err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.ListModelActions(ctx, binding.Scope, binding.DeploymentID); err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.ListModelPrompts(ctx, binding.Scope, binding.DeploymentID); err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.Activate(ctx, binding.Scope, binding.DeploymentID, HostCapabilityState{
			Adapters: map[string]AdapterCapability{AdapterLocal: {State: AdapterStateAvailable}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if reads := store.definitionReads.Load(); reads != 1 {
		t.Fatalf("warm source-qualified definition was loaded %d times, want 1", reads)
	}
	if reads := store.bindingReads.Load(); reads != 9 {
		t.Fatalf("mutable binding was read %d times, want 9", reads)
	}
	catalog.mu.RLock()
	after := catalog.schemas[actionKey(definition.ID, definition.Version, identity, "deploy")]
	catalog.mu.RUnlock()
	if schemas == nil || schemas != after {
		t.Fatal("warm definition did not retain its validated compiled schema")
	}
}

func TestCatalogUnqualifiedLookupSeesPublisherAddedByAnotherCatalog(t *testing.T) {
	ctx := context.Background()
	catalog, writer, store, definition, _ := prepareCacheCatalog(t, 1)
	if _, err := catalog.GetDefinition(ctx, definition.ID, definition.Version); err != nil {
		t.Fatal(err)
	}
	catalog.mu.RLock()
	schemas := catalog.schemas[actionKey(definition.ID, definition.Version, DefinitionSourceIdentity(definition), "deploy")]
	catalog.mu.RUnlock()
	if _, err := catalog.GetDefinition(ctx, definition.ID, definition.Version); err != nil {
		t.Fatal(err)
	}
	variant := cloneDefinition(definition)
	variant.Source.Identity = "clawhub::@another/release"
	if err := writer.Register(ctx, variant); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.GetDefinition(ctx, definition.ID, definition.Version); !errors.Is(err, ErrDefinitionAmbiguous) {
		t.Fatalf("new publisher was hidden by a cached unqualified lookup: %v", err)
	}
	if reads := store.definitionReads.Load(); reads != 3 {
		t.Fatalf("unqualified lookup read variants %d times, want 3", reads)
	}
	catalog.mu.RLock()
	after := catalog.schemas[actionKey(definition.ID, definition.Version, DefinitionSourceIdentity(definition), "deploy")]
	catalog.mu.RUnlock()
	if schemas != after {
		t.Fatal("fresh variant selection replaced an immutable compiled schema")
	}
	if exact, err := catalog.GetDefinitionVariant(ctx, definition.ID, definition.Version, DefinitionSourceIdentity(definition)); err != nil || DefinitionSourceIdentity(exact) != DefinitionSourceIdentity(definition) {
		t.Fatalf("exact variant changed after publisher collision: %#v, %v", exact, err)
	}
}

func TestCatalogImmutableCacheDoesNotCacheBindingAuthorityOrConfiguration(t *testing.T) {
	ctx := context.Background()
	store := newCountingCatalogStore()
	writer := NewCatalogWithStore(store)
	definition := cacheTestDefinition(1)
	definition.BindingConfigSchema = map[string]interface{}{
		"type": "object", "additionalProperties": false, "required": []interface{}{"target"},
		"properties": map[string]interface{}{"target": map[string]interface{}{"type": "string", "minLength": 1}},
	}
	if err := writer.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	binding := cacheTestBinding(definition)
	binding.Config = map[string]interface{}{"target": "first"}
	if err := writer.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	catalog := NewCatalogWithStore(store)
	resolve := func() (*BoundAction, error) {
		return catalog.Resolve(ctx, binding.Scope, binding.DeploymentID, definition.ID, definition.Version, "deploy")
	}
	if bound, err := resolve(); err != nil || bound.Binding.Config["target"] != "first" {
		t.Fatalf("initial configuration: %#v, %v", bound, err)
	}
	binding.Revision++
	binding.Config["target"] = "second"
	binding.Credentials["git"] = CredentialReference{Kind: "git-token", ID: "credential://replacement"}
	if err := writer.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if bound, err := resolve(); err != nil || bound.Binding.Config["target"] != "second" || bound.Binding.Credentials["git"].ID != "credential://replacement" || bound.Binding.Revision != 2 {
		t.Fatalf("fresh configuration and credentials: %#v, %v", bound, err)
	}
	binding.Revision++
	binding.Config["target"] = ""
	// Model a legacy or externally corrupted durable binding. Cached immutable
	// definitions must not turn off validation of freshly read mutable input.
	if err := store.SaveSkillBinding(ctx, binding, binding.Revision-1); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(); err == nil {
		t.Fatal("invalid freshly stored configuration was accepted")
	}
	binding.Revision++
	binding.Config["target"] = "second"
	binding.Disabled = true
	if err := writer.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(); err == nil {
		t.Fatal("revoked binding still resolved through immutable cache")
	}
	if actions, err := catalog.ListModelActions(ctx, binding.Scope, binding.DeploymentID); err != nil || len(actions) != 0 {
		t.Fatalf("revoked binding remained model visible: %#v, %v", actions, err)
	}
}

func TestCatalogCachedDefinitionsAndProjectionsAreMutationSafe(t *testing.T) {
	ctx := context.Background()
	catalog, _, _, definition, binding := prepareCacheCatalog(t, 1)
	identity := DefinitionSourceIdentity(definition)
	loaded, err := catalog.GetDefinitionVariant(ctx, definition.ID, definition.Version, identity)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Source.Identity = "clawhub::@mutated/release"
	loaded.Prompt.Instructions = "mutated"
	loaded.Actions["deploy"].InputSchema["type"] = "string"
	actions, err := catalog.ListModelActions(ctx, binding.Scope, binding.DeploymentID)
	if err != nil || len(actions) != 1 {
		t.Fatalf("actions = %#v, %v", actions, err)
	}
	actions[0].InputSchema["type"] = "string"
	actions[0].SemanticArguments["target"] = "mutated"
	bound, err := catalog.Resolve(ctx, binding.Scope, binding.DeploymentID, definition.ID, definition.Version, "deploy")
	if err != nil {
		t.Fatal(err)
	}
	bound.Definition.Actions["deploy"].InputSchema["type"] = "string"
	bound.Binding.Credentials["git"] = CredentialReference{Kind: "git-token", ID: "mutated"}
	prompt, err := catalog.ResolvePrompt(ctx, binding.Scope, binding.DeploymentID, definition.ID, definition.Version)
	if err != nil {
		t.Fatal(err)
	}
	prompt.Instructions = "mutated"
	loaded, err = catalog.GetDefinitionVariant(ctx, definition.ID, definition.Version, identity)
	if err != nil || DefinitionSourceIdentity(loaded) != identity || loaded.Prompt.Instructions != definition.Prompt.Instructions || loaded.Actions["deploy"].InputSchema["type"] != "object" {
		t.Fatalf("caller mutated cached definition: %#v, %v", loaded, err)
	}
	bound, err = catalog.Resolve(ctx, binding.Scope, binding.DeploymentID, definition.ID, definition.Version, "deploy")
	if err != nil || bound.Binding.Credentials["git"].ID != "credential://git" || bound.Action.SemanticArguments["target"] != "environment" {
		t.Fatalf("caller mutated future bound projection: %#v, %v", bound, err)
	}
	if err := catalog.ValidateInput(ctx, bound, map[string]interface{}{"environment": "staging", "revision": "abc"}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.ValidateInput(ctx, bound, map[string]interface{}{"environment": "staging"}); err == nil {
		t.Fatal("cached schema stopped rejecting invalid input")
	}
}

func TestCatalogDefinitionMissesAndErrorsAreNotCached(t *testing.T) {
	ctx := context.Background()
	store := newCountingCatalogStore()
	catalog := NewCatalogWithStore(store)
	definition := cacheTestDefinition(1)
	identity := DefinitionSourceIdentity(definition)
	for range 2 {
		if got, err := catalog.GetDefinitionVariant(ctx, definition.ID, definition.Version, identity); err != nil || got != nil {
			t.Fatalf("missing definition = %#v, %v", got, err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for range 2 {
		if _, err := catalog.GetDefinitionVariant(canceled, definition.ID, definition.Version, identity); !errors.Is(err, context.Canceled) {
			t.Fatalf("store error = %v", err)
		}
	}
	writer := NewCatalogWithStore(store)
	if err := writer.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if got, err := catalog.GetDefinitionVariant(ctx, definition.ID, definition.Version, identity); err != nil || got == nil {
		t.Fatalf("newly registered definition remained missing: %#v, %v", got, err)
	}
	if reads := store.definitionReads.Load(); reads != 5 {
		t.Fatalf("misses or errors were cached: %d reads, want 5", reads)
	}
}

func TestCatalogFreshPublisherVariantMustStillValidateItsSchemas(t *testing.T) {
	ctx := context.Background()
	catalog, _, store, definition, _ := prepareCacheCatalog(t, 1)
	if _, err := catalog.GetDefinition(ctx, definition.ID, definition.Version); err != nil {
		t.Fatal(err)
	}
	invalid := cloneDefinition(definition)
	invalid.Source.Identity = "clawhub::@unvalidated/release"
	invalid.Actions["deploy"].InputSchema["$ref"] = "https://example.invalid/schema.json"
	// A newly encountered persisted variant must pass the same validation as a
	// cold lookup, even when another publisher's immutable variant is cached.
	if err := store.CreateSkillDefinition(ctx, invalid); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.GetDefinition(ctx, definition.ID, definition.Version); err == nil {
		t.Fatal("invalid schema on a newly encountered variant was accepted")
	}
	if _, err := catalog.GetDefinitionVariant(ctx, definition.ID, definition.Version, DefinitionSourceIdentity(invalid)); err == nil {
		t.Fatal("invalid variant was saved as a positive cache entry")
	}
	catalog.mu.RLock()
	bad := catalog.skills[definitionKey(invalid.ID, invalid.Version, DefinitionSourceIdentity(invalid))]
	catalog.mu.RUnlock()
	if bad != nil {
		t.Fatal("failed schema validation populated the immutable cache")
	}
}

func TestCatalogWarmImmutableCacheAllowsConcurrentFreshBindings(t *testing.T) {
	ctx := context.Background()
	catalog, writer, store, definition, binding := prepareCacheCatalog(t, 1)
	identity := DefinitionSourceIdentity(definition)
	if _, err := catalog.GetDefinitionVariant(ctx, definition.ID, definition.Version, identity); err != nil {
		t.Fatal(err)
	}
	scope, deploymentID := binding.Scope, binding.DeploymentID
	errorsSeen := make(chan error, 4)
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 20 {
				loaded, err := catalog.GetDefinitionVariant(ctx, definition.ID, definition.Version, identity)
				if err != nil {
					errorsSeen <- err
					return
				}
				// Each caller owns its result, including nested schemas.
				loaded.Actions["deploy"].InputSchema["type"] = "string"
				if _, err := catalog.ListModelActions(ctx, scope, deploymentID); err != nil {
					errorsSeen <- err
					return
				}
			}
		}()
	}
	for i := 0; i < 10; i++ {
		binding.Revision++
		binding.Credentials["git"] = CredentialReference{Kind: "git-token", ID: fmt.Sprintf("credential://git/%d", i)}
		if err := writer.Bind(ctx, binding); err != nil {
			t.Fatal(err)
		}
	}
	readers.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatal(err)
	}
	if reads := store.definitionReads.Load(); reads != 1 {
		t.Fatalf("concurrent warm reads reloaded immutable definition %d times", reads)
	}
	resolved, err := catalog.Resolve(ctx, scope, deploymentID, definition.ID, definition.Version, "deploy")
	if err != nil || resolved.Binding.Revision != binding.Revision || resolved.Binding.Credentials["git"].ID != "credential://git/9" {
		t.Fatalf("concurrent reads hid the final binding update: %#v, %v", resolved, err)
	}
}

func BenchmarkCatalogWarmDefinition(b *testing.B) {
	ctx := context.Background()
	catalog, _, _, definition, _ := prepareCacheCatalog(b, 24)
	identity := DefinitionSourceIdentity(definition)
	for _, qualified := range []bool{true, false} {
		name := "unqualified"
		if qualified {
			name = "source-qualified"
		}
		b.Run(name, func(b *testing.B) {
			lookup := func() (*Definition, error) {
				if qualified {
					return catalog.GetDefinitionVariant(ctx, definition.ID, definition.Version, identity)
				}
				return catalog.GetDefinition(ctx, definition.ID, definition.Version)
			}
			if _, err := lookup(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := lookup(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkCatalogWarmModelActions(b *testing.B) {
	ctx := context.Background()
	catalog, _, _, _, binding := prepareCacheCatalog(b, 24)
	if _, err := catalog.ListModelActions(ctx, binding.Scope, binding.DeploymentID); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := catalog.ListModelActions(ctx, binding.Scope, binding.DeploymentID); err != nil {
			b.Fatal(err)
		}
	}
}
