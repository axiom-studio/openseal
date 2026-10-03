package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type skillIdentityProbeTestStore interface {
	skill.CatalogStore
	ListSkillDefinitionIdentities(context.Context, string, string) ([]capability.SkillIdentity, error)
}

func skillIdentityProbeDefinition(id, version, source string) *skill.Definition {
	definition := &skill.Definition{
		ID: id, Version: version, Name: "Research",
		Prompt: &skill.PromptModule{Instructions: "Use the exact installed source."},
	}
	if source != "" {
		definition.Source = &skill.SourceProvenance{Identity: source, Format: "openclaw.skill.v1"}
	}
	return definition
}

func TestSkillDefinitionIdentityProbeIsLiveExactAndIsolated(t *testing.T) {
	for _, fixture := range []struct {
		name string
		open func(*testing.T) skillIdentityProbeTestStore
	}{
		{name: "memory", open: func(*testing.T) skillIdentityProbeTestStore { return NewMemoryStore() }},
		{name: "sqlite", open: func(t *testing.T) skillIdentityProbeTestStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "identities.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			testSkillDefinitionIdentityProbeContract(t, fixture.open(t), fixture.open(t))
		})
	}
}

func testSkillDefinitionIdentityProbeContract(t *testing.T, store, unrelated skillIdentityProbeTestStore) {
	t.Helper()
	ctx := context.Background()
	writer := skill.NewCatalogWithStore(store)
	probe := func(id, version string) []capability.SkillIdentity {
		t.Helper()
		identities, err := store.ListSkillDefinitionIdentities(ctx, id, version)
		if err != nil {
			t.Fatal(err)
		}
		return identities
	}
	if got := probe("research", "1.0.0"); len(got) != 0 {
		t.Fatalf("empty store identities = %#v", got)
	}
	for _, definition := range []*skill.Definition{
		skillIdentityProbeDefinition("research", "1.0.0", "clawhub::@zoe/research"),
		skillIdentityProbeDefinition("research", "1.0.0", ""),
		skillIdentityProbeDefinition("researcher", "1.0.0", "clawhub::@other/research"),
		skillIdentityProbeDefinition("research", "1.0.00", "clawhub::@other/research"),
	} {
		if err := writer.Register(ctx, definition); err != nil {
			t.Fatal(err)
		}
	}
	want := []capability.SkillIdentity{
		{ID: "research", Version: "1.0.0", SourceIdentity: ""},
		{ID: "research", Version: "1.0.0", SourceIdentity: "clawhub::@zoe/research"},
	}
	if got := probe("research", "1.0.0"); !reflect.DeepEqual(got, want) {
		t.Fatalf("exact identity probe = %#v, want %#v", got, want)
	}
	if got := probe("missing", "1.0.0"); len(got) != 0 {
		t.Fatalf("foreign ID identities = %#v", got)
	}
	if got := probe("research", "1.0.1"); len(got) != 0 {
		t.Fatalf("foreign version identities = %#v", got)
	}
	// A different catalog can install a publisher immediately after a probe.
	// Selection metadata must never cache the former singleton variant set.
	otherWriter := skill.NewCatalogWithStore(store)
	if err := otherWriter.Register(ctx, skillIdentityProbeDefinition("research", "1.0.0", "clawhub::@alice/research")); err != nil {
		t.Fatal(err)
	}
	want = append(want[:1], capability.SkillIdentity{ID: "research", Version: "1.0.0", SourceIdentity: "clawhub::@alice/research"}, want[1])
	got := probe("research", "1.0.0")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("new publisher identities = %#v, want %#v", got, want)
	}
	got[0] = capability.SkillIdentity{ID: "mutated", Version: "mutated", SourceIdentity: "mutated"}
	got = append(got, capability.SkillIdentity{ID: "injected", Version: "injected"})
	if again := probe("research", "1.0.0"); !reflect.DeepEqual(again, want) {
		t.Fatalf("returned identity mutation changed store = %#v", again)
	}
	// Metadata is global to one definition store, not tenant scoped or shared
	// across independent stores using the same declared ID and version.
	if identities, err := unrelated.ListSkillDefinitionIdentities(ctx, "research", "1.0.0"); err != nil || len(identities) != 0 {
		t.Fatalf("independent store identities = %#v, %v", identities, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if identities, err := store.ListSkillDefinitionIdentities(canceled, "research", "1.0.0"); !errors.Is(err, context.Canceled) || len(identities) != 0 {
		t.Fatalf("canceled identity probe = %#v, %v", identities, err)
	}
	expired, expire := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer expire()
	if identities, err := store.ListSkillDefinitionIdentities(expired, "research", "1.0.0"); !errors.Is(err, context.DeadlineExceeded) || len(identities) != 0 {
		t.Fatalf("expired identity probe = %#v, %v", identities, err)
	}
}

func TestSQLiteSkillDefinitionIdentityProbeDoesNotDecodePayload(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "corrupt-definition.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity := capability.SkillIdentity{ID: "broken", Version: "1", SourceIdentity: "clawhub::@publisher/broken"}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO skill_definitions(id, version, source_identity, payload) VALUES(?, ?, ?, ?)`, identity.ID, identity.Version, identity.SourceIdentity, "{malformed immutable payload"); err != nil {
		t.Fatal(err)
	}
	got, err := store.ListSkillDefinitionIdentities(ctx, identity.ID, identity.Version)
	if err != nil || !reflect.DeepEqual(got, []capability.SkillIdentity{identity}) {
		t.Fatalf("metadata must use indexed columns, not payload = %#v, %v", got, err)
	}
	if definitions, err := store.ListSkillDefinitionVariants(ctx, identity.ID, identity.Version); err == nil || len(definitions) != 0 {
		t.Fatalf("payload hydration accepted malformed definition = %#v, %v", definitions, err)
	}
	if definition, err := skill.NewCatalogWithStore(store).GetDefinitionVariant(ctx, identity.ID, identity.Version, identity.SourceIdentity); err == nil || definition != nil {
		t.Fatalf("cold catalog accepted malformed definition = %#v, %v", definition, err)
	}
	rows, err := store.db.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT id, version, source_identity FROM skill_definitions WHERE id = ? AND version = ? ORDER BY source_identity`, identity.ID, identity.Version)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var node, parent, unused int
		var detail string
		if err := rows.Scan(&node, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if text := strings.Join(plan, "\n"); !strings.Contains(text, "COVERING INDEX sqlite_autoindex_skill_definitions_1") || strings.Contains(text, "USE TEMP B-TREE") {
		t.Fatalf("metadata query did not use existing covering primary key: %s", text)
	}
}

func TestMemorySkillDefinitionIdentityProbeDoesNotInspectPayload(t *testing.T) {
	store := NewMemoryStore()
	identity := capability.SkillIdentity{ID: "broken", Version: "1", SourceIdentity: "clawhub::@publisher/broken"}
	// This immutable payload cannot be encoded by the deep-clone hydration
	// path. The metadata probe must derive its identity from the map key.
	store.skillDefinitions[memorySkillDefinitionKey(identity.ID, identity.Version, identity.SourceIdentity)] = &skill.Definition{
		ID: "foreign", Version: "foreign",
		Actions: map[string]skill.Action{"read": {InputSchema: map[string]interface{}{"unencodable": make(chan int)}}},
	}
	got, err := store.ListSkillDefinitionIdentities(context.Background(), identity.ID, identity.Version)
	if err != nil || !reflect.DeepEqual(got, []capability.SkillIdentity{identity}) {
		t.Fatalf("metadata inspected mutable payload fields = %#v, %v", got, err)
	}
	if definition, err := skill.NewCatalogWithStore(store).GetDefinitionVariant(context.Background(), identity.ID, identity.Version, identity.SourceIdentity); err == nil || definition != nil {
		t.Fatalf("cold catalog accepted invalid payload = %#v, %v", definition, err)
	}
}

func TestMemorySkillDefinitionIdentityProbePreservesAcceptedEmbeddedSeparators(t *testing.T) {
	for _, test := range []struct{ name, id, version string }{
		{name: "id", id: "research\x00child", version: "1.0.0"},
		{name: "version", id: "research", version: "1.0.0\x00preview"},
		{name: "id-and-version", id: "research\x00child", version: "1.0.0\x00preview"},
	} {
		for _, source := range []string{"", "clawhub::@publisher/research"} {
			name := test.name + "/publisher"
			if source == "" {
				name = test.name + "/builtin"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				store := NewMemoryStore()
				definition := skillIdentityProbeDefinition(test.id, test.version, source)
				writer := skill.NewCatalogWithStore(store)
				if err := writer.Register(ctx, definition); err != nil {
					t.Fatalf("existing accepted identity could not register: %v", err)
				}
				want := []capability.SkillIdentity{{ID: test.id, Version: test.version, SourceIdentity: source}}
				if got, err := store.ListSkillDefinitionIdentities(ctx, test.id, test.version); err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("embedded separator changed exact identity: %#v, %v", got, err)
				}
				reader := skill.NewCatalogWithStore(store)
				for range 2 {
					if got, err := reader.GetDefinition(ctx, test.id, test.version); err != nil || !reflect.DeepEqual(got, definition) {
						t.Fatalf("accepted identity failed cold or warm unqualified lookup: %#v, %v", got, err)
					}
				}
				if err := skill.NewCatalogWithStore(store).Register(ctx, definition); !errors.Is(err, skill.ErrDefinitionImmutable) {
					t.Fatalf("exact identity overwrite was allowed: %v", err)
				}
				later := skillIdentityProbeDefinition(test.id, test.version, "clawhub::@later/research")
				if err := skill.NewCatalogWithStore(store).Register(ctx, later); err != nil {
					t.Fatal(err)
				}
				if got, err := reader.GetDefinition(ctx, test.id, test.version); !errors.Is(err, skill.ErrDefinitionAmbiguous) || got != nil {
					t.Fatalf("embedded separator hid later publisher collision: %#v, %v", got, err)
				}
			})
		}
	}
}

func TestMemorySkillDefinitionIdentityProbeKeepsDelimiterCollisionImmutable(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	definition := skillIdentityProbeDefinition("research\x00child", "1.0.0", "clawhub::@publisher/research")
	if err := skill.NewCatalogWithStore(store).Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	// The existing store key encoding gives these different accepted ID/version
	// pairs the same key. This probe does not change that storage contract or
	// permit a second definition to overwrite the first immutable record.
	collision := skillIdentityProbeDefinition("research", "child\x001.0.0", skill.DefinitionSourceIdentity(definition))
	if err := skill.NewCatalogWithStore(store).Register(ctx, collision); !errors.Is(err, skill.ErrDefinitionImmutable) {
		t.Fatalf("existing delimiter collision no longer rejects overwrite: %v", err)
	}
	if got, err := skill.NewCatalogWithStore(store).GetDefinition(ctx, definition.ID, definition.Version); err != nil || !reflect.DeepEqual(got, definition) {
		t.Fatalf("delimiter collision altered registered immutable definition: %#v, %v", got, err)
	}
}

func TestSQLiteSkillDefinitionIdentityProbeReturnsClosedStoreError(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if identities, err := store.ListSkillDefinitionIdentities(context.Background(), "research", "1.0.0"); err == nil || len(identities) != 0 {
		t.Fatalf("closed store identity probe = %#v, %v", identities, err)
	}
}

func TestSQLiteSkillDefinitionIdentityProbeSeesReplicaPublisherAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "replica-identities.db")
	primary, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	replica, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	if err := skill.NewCatalogWithStore(primary).Register(ctx, skillIdentityProbeDefinition("research", "1.0.0", "")); err != nil {
		t.Fatal(err)
	}
	if got, err := replica.ListSkillDefinitionIdentities(ctx, "research", "1.0.0"); err != nil || len(got) != 1 || got[0].SourceIdentity != "" {
		t.Fatalf("replica builtin identity = %#v, %v", got, err)
	}
	if err := skill.NewCatalogWithStore(replica).Register(ctx, skillIdentityProbeDefinition("research", "1.0.0", "clawhub::@replica/research")); err != nil {
		t.Fatal(err)
	}
	want := []capability.SkillIdentity{
		{ID: "research", Version: "1.0.0"},
		{ID: "research", Version: "1.0.0", SourceIdentity: "clawhub::@replica/research"},
	}
	if got, err := primary.ListSkillDefinitionIdentities(ctx, "research", "1.0.0"); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("publisher installed by replica was not immediately visible = %#v, %v", got, err)
	}
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, err := reopened.ListSkillDefinitionIdentities(ctx, "research", "1.0.0"); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("restarted identity probe = %#v, %v", got, err)
	}
}
