package runtime

import (
	"context"
	"sync"
	"testing"
)

// newTestKernelStore returns an empty in-memory kernel store that is closed
// when the test ends. runtimetest.NewKernelStore is its copy for other
// packages (importing runtimetest here would be a cycle); change both.
func newTestKernelStore(t testing.TB) *SQLiteStore {
	t.Helper()
	store, err := NewInMemorySQLiteStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestInMemorySQLiteStoreSharesOneDatabaseAcrossConnections(t *testing.T) {
	store := newTestKernelStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `CREATE TABLE probe (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			tx, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				errs <- err
				return
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO probe DEFAULT VALUES`); err != nil {
				tx.Rollback()
				errs <- err
				return
			}
			errs <- tx.Commit()
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM probe`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 16 {
		t.Fatalf("rows = %d, want 16", count)
	}
	if stats := store.db.Stats(); stats.OpenConnections < 2 {
		t.Fatalf("open connections = %d, want a pool of several", stats.OpenConnections)
	}
}

func TestInMemorySQLiteStoresAreIsolated(t *testing.T) {
	first := newTestKernelStore(t)
	second := newTestKernelStore(t)
	ctx := context.Background()
	if _, err := first.db.ExecContext(ctx, `CREATE TABLE probe (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := second.db.ExecContext(ctx, `CREATE TABLE probe (id INTEGER)`); err != nil {
		t.Fatalf("second store sees the first store's table: %v", err)
	}
}

func TestInMemorySQLiteStoreSurvivesIdleConnectionsClosing(t *testing.T) {
	store := newTestKernelStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `CREATE TABLE probe (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	// Dropping every idle pooled connection must not drop the database.
	store.db.SetMaxIdleConns(0)
	store.db.SetMaxIdleConns(2)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO probe VALUES (1)`); err != nil {
		t.Fatalf("database lost after idle connections closed: %v", err)
	}
}
