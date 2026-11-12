package actiontest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

// StoreBackend is one kernel store backend a test runs on.
type StoreBackend struct {
	Name string
	Open func(*testing.T) models.KernelStore
}

// Stores is SQLite, and Postgres when OPENSEAL_TEST_POSTGRES_DSN is set.
func Stores(t *testing.T) []StoreBackend {
	stores := []StoreBackend{
		{Name: "sqlite", Open: func(t *testing.T) models.KernelStore {
			store, err := sqlite.New(filepath.Join(t.TempDir(), "nested.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	}
	if dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN"); dsn != "" {
		stores = append(stores, StoreBackend{Name: "postgres", Open: func(t *testing.T) models.KernelStore {
			store, _ := kerneltest.OpenPostgres(t, "openseal_nested_")
			return store
		}})
	}
	return stores
}
