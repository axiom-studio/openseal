package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// openPostgresStoreOn opens another store on schema s, closed when the
// test ends.
func openPostgresStoreOn(t testing.TB, s storetest.PostgresSchema, options ...PostgresStoreOption) *PostgresStore {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, err := NewPostgresStore(ctx, s.DSN, append([]PostgresStoreOption{WithPostgresSchema(s.Name)}, options...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// openTestPostgresStore opens a Postgres store on a fresh schema (see
// storetest.NewPostgresSchema) and returns it with the schema.
func openTestPostgresStore(t testing.TB, prefix string, options ...PostgresStoreOption) (*PostgresStore, storetest.PostgresSchema) {
	t.Helper()
	schema := storetest.NewPostgresSchema(t, prefix)
	return openPostgresStoreOn(t, schema, options...), schema
}

// postgresHead caches the schema version a fresh schema migrates to.
var postgresHead struct {
	sync.Mutex
	version int64
}

// postgresSchemaHead returns the version a store reaches when it migrates an
// empty schema: the head of this binary's migrations. Tests outside the
// store compare an upgraded schema with it; the store's own tests pin the
// number itself.
func postgresSchemaHead(t testing.TB) int64 {
	t.Helper()
	postgresHead.Lock()
	defer postgresHead.Unlock()
	if postgresHead.version == 0 {
		store, _ := openTestPostgresStore(t, "openseal_head_")
		version, err := store.PostgresSchemaVersion(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		postgresHead.version = version
	}
	return postgresHead.version
}
