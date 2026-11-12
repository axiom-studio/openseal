package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// The store's tests open stores on schemas they own. kerneltest has the same
// two openers for other packages' tests; it imports this package, so these
// tests cannot use it.

// openPostgresStoreOn opens another store on schema s, closed when the
// test ends.
func openPostgresStoreOn(t testing.TB, s storetest.PostgresSchema, options ...Option) *Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, err := New(ctx, s.DSN, append([]Option{WithSchema(s.Name)}, options...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// openTestPostgresStore opens a store on a fresh schema (see
// storetest.NewPostgresSchema) and returns it with the schema.
func openTestPostgresStore(t testing.TB, prefix string, options ...Option) (*Store, storetest.PostgresSchema) {
	t.Helper()
	schema := storetest.NewPostgresSchema(t, prefix)
	return openPostgresStoreOn(t, schema, options...), schema
}
