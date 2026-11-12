package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sync"
	"testing"
)

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
	// A store opened after the others were written to must still start from
	// the migrated template alone.
	third := newTestKernelStore(t)
	if _, err := third.db.ExecContext(ctx, `CREATE TABLE probe (id INTEGER)`); err != nil {
		t.Fatalf("a later store sees an earlier store's table: %v", err)
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

// A store copied from the migrated template must hold exactly what running
// the migrations on an empty database produces: same schema, same rows.
func TestInMemorySQLiteStoreCopyMatchesFreshMigration(t *testing.T) {
	fresh, err := openInMemorySQLiteStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fresh.Close() })
	if err := migrate(fresh.db); err != nil {
		t.Fatal(err)
	}
	copied := newTestKernelStore(t)

	want, got := dumpSQLiteDatabase(t, fresh.db), dumpSQLiteDatabase(t, copied.db)
	if len(want) == 0 {
		t.Fatal("fresh migration produced an empty dump")
	}
	if !slices.Equal(got, want) {
		for _, line := range want {
			if !slices.Contains(got, line) {
				t.Errorf("copy lacks: %s", line)
			}
		}
		for _, line := range got {
			if !slices.Contains(want, line) {
				t.Errorf("copy has extra: %s", line)
			}
		}
	}
}

// dumpSQLiteDatabase lists the header pragmas, every schema object and every
// row of every table, sorted, so two databases compare as string slices.
func dumpSQLiteDatabase(t *testing.T, db *sql.DB) []string {
	t.Helper()
	var lines []string
	for _, pragma := range []string{"user_version", "application_id", "page_size", "encoding", "auto_vacuum"} {
		var value string
		if err := db.QueryRow(`PRAGMA ` + pragma).Scan(&value); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, "pragma "+pragma+"="+value)
	}
	schema := queryStrings(t, db, `SELECT type || ' ' || name || ' ' || tbl_name || ' ' || coalesce(sql, '') FROM sqlite_master`)
	lines = append(lines, schema...)
	for _, table := range queryStrings(t, db, `SELECT name FROM sqlite_master WHERE type = 'table'`) {
		for _, row := range queryRows(t, db, `SELECT * FROM "`+table+`"`) {
			lines = append(lines, "row "+table+" "+row)
		}
	}
	slices.Sort(lines)
	return lines
}

func queryStrings(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return values
}

func queryRows(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var values []string
	for rows.Next() {
		row := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range row {
			pointers[i] = &row[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		values = append(values, fmt.Sprintf("%q", row))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return values
}

func BenchmarkNewInMemorySQLiteStore(b *testing.B) {
	for b.Loop() {
		store, err := NewInMemorySQLiteStore()
		if err != nil {
			b.Fatal(err)
		}
		store.Close()
	}
}
