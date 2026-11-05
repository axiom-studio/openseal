package runtime

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestTeamRoutingSQLiteMigrationRemovesCoordinationPolicy(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE team_definitions (payload TEXT NOT NULL)`,
		`CREATE TABLE team_definition_amendments (payload TEXT NOT NULL)`,
		`CREATE TABLE workforce_change_sets (payload TEXT NOT NULL)`,
		`INSERT INTO team_definitions(payload) VALUES
		 ('{"id":"a","coordination":{"maximumSpeakersPerRound":2,"quietByDefault":true}}'),
		 ('{"id":"b","routing":{"notes":"Maya owns pricing"}}')`,
		`INSERT INTO team_definition_amendments(payload) VALUES ('{"candidate":{"id":"a","coordination":{"requireRoleRelevance":true}}}')`,
		`INSERT INTO workforce_change_sets(payload) VALUES ('{"result":{"candidate":{"team":{"coordination":{"suppressDuplicateContent":true}}}}}')`,
	} {
		if _, err = db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err = migrateTeamRoutingSQLite(db); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range teamCoordinationPaths {
		var remaining int
		if err = db.QueryRow(`SELECT COUNT(*) FROM `+path.table+` WHERE json_type(payload, ?) IS NOT NULL`, path.sqlite).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining != 0 {
			t.Fatalf("%s still has %d coordination policies", path.table, remaining)
		}
	}
	var notes string
	if err = db.QueryRow(`SELECT json_extract(payload, '$.routing.notes') FROM team_definitions WHERE json_extract(payload, '$.id') = 'b'`).Scan(&notes); err != nil || notes != "Maya owns pricing" {
		t.Fatalf("routing notes = %q, %v", notes, err)
	}
}
