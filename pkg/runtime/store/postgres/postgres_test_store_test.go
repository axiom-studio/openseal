package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// The store's tests open stores on schemas they own. Runtime's scenario
// tests have the same two openers in its postgres_test_schema_test.go;
// each package opens its own store type on storetest.PostgresSchema.

// openPostgresStoreOn opens another store on schema s, closed when the
// test ends.
func openPostgresStoreOn(t testing.TB, s storetest.PostgresSchema, options ...PostgresStoreOption) *Store {
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

// openTestPostgresStore opens a store on a fresh schema (see
// storetest.NewPostgresSchema) and returns it with the schema.
func openTestPostgresStore(t testing.TB, prefix string, options ...PostgresStoreOption) (*Store, storetest.PostgresSchema) {
	t.Helper()
	schema := storetest.NewPostgresSchema(t, prefix)
	return openPostgresStoreOn(t, schema, options...), schema
}
