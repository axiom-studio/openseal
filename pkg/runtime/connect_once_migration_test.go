package runtime

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func TestSQLiteConnectOnceMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connect-once.db")
	store, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := openTestSQLiteDB(t, path)
	h := storetest.ConnectOnceMigrationHarness{
		SQLite: true,
		DB:     func() *sql.DB { return db },
		Table:  func(name string) string { return name },
		Store:  func() storetest.ConnectOnceMigrationStore { return store },
		Rerun:  func(*testing.T) {},
	}
	storetest.RunConnectOnceMigrationCase(t, h, func(t *testing.T) {
		t.Helper()
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if store, err = sqlite.NewSQLiteStore(path); err != nil {
			t.Fatal(err)
		}
	})
}
