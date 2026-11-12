package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// SQLite store test: it moves to store/sqlite in U2.9. The contract it runs
// is in skill_definition_identity_probe_contract_test.go (storetest).

func TestSkillDefinitionIdentityProbeIsLiveExactAndIsolated(t *testing.T) {
	for _, fixture := range []struct {
		name string
		open func(*testing.T) storetest.SkillIdentityProbeStore
	}{
		{name: "sqlite", open: func(t *testing.T) storetest.SkillIdentityProbeStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "identities.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			storetest.RunSkillDefinitionIdentityProbeContract(t, fixture.open(t), fixture.open(t))
		})
	}
}

func TestSQLiteSkillDefinitionIdentityProbeDoesNotDecodePayload(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "corrupt-definition.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db := openTestSQLiteDB(t, path)
	identity := capability.SkillIdentity{ID: "broken", Version: "1", SourceIdentity: "catalog::@publisher/broken"}
	if _, err := db.ExecContext(ctx, `INSERT INTO skill_definitions(id, version, source_identity, payload) VALUES(?, ?, ?, ?)`, identity.ID, identity.Version, identity.SourceIdentity, "{malformed immutable payload"); err != nil {
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
	rows, err := db.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT id, version, source_identity FROM skill_definitions WHERE id = ? AND version = ? ORDER BY source_identity`, identity.ID, identity.Version)
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

func TestSkillDefinitionIdentityProbePreservesAcceptedEmbeddedSeparators(t *testing.T) {
	for _, test := range []struct{ name, id, version string }{
		{name: "id", id: "research\x00child", version: "1.0.0"},
		{name: "version", id: "research", version: "1.0.0\x00preview"},
		{name: "id-and-version", id: "research\x00child", version: "1.0.0\x00preview"},
	} {
		for _, source := range []string{"", "catalog::@publisher/research"} {
			name := test.name + "/publisher"
			if source == "" {
				name = test.name + "/builtin"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				store := newTestKernelStore(t)
				definition := storetest.SkillIdentityProbeDefinition(test.id, test.version, source)
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
				later := storetest.SkillIdentityProbeDefinition(test.id, test.version, "catalog::@later/research")
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
	if err := skill.NewCatalogWithStore(primary).Register(ctx, storetest.SkillIdentityProbeDefinition("research", "1.0.0", "")); err != nil {
		t.Fatal(err)
	}
	if got, err := replica.ListSkillDefinitionIdentities(ctx, "research", "1.0.0"); err != nil || len(got) != 1 || got[0].SourceIdentity != "" {
		t.Fatalf("replica builtin identity = %#v, %v", got, err)
	}
	if err := skill.NewCatalogWithStore(replica).Register(ctx, storetest.SkillIdentityProbeDefinition("research", "1.0.0", "catalog::@replica/research")); err != nil {
		t.Fatal(err)
	}
	want := []capability.SkillIdentity{
		{ID: "research", Version: "1.0.0"},
		{ID: "research", Version: "1.0.0", SourceIdentity: "catalog::@replica/research"},
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
