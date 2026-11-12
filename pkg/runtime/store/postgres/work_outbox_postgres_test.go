package postgres

import "testing"

// Store test: writesRows belongs to the Postgres work outbox connection, so
// this file moves to store/postgres with work_outbox_postgres.go (U2.8).

func TestWritesRows(t *testing.T) {
	for query, writes := range map[string]bool{
		"SELECT 1": false, "\n  select * from x": false, "INSERT INTO x": true, "update x": true,
		"DELETE FROM x": true, "WITH moved AS (DELETE FROM x RETURNING *) SELECT 1": true, "(SELECT 1)": false,
	} {
		if got := writesRows(query); got != writes {
			t.Fatalf("writesRows(%q) = %t", query, got)
		}
	}
}
