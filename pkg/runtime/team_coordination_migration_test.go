package runtime

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestNaturalTeamCoordinationSQLiteMigrationRemovesOnlyDecorativeMode(t *testing.T) {
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
		 ('{"coordination":{"mode":"peer"}}'),
		 ('{"coordination":{"mode":"dynamic","quietByDefault":false,"requireRoleRelevance":false,"suppressDuplicateContent":false,"maximumSpeakersPerRound":7}}')`,
		`INSERT INTO team_definition_amendments(payload) VALUES
		 ('{"candidate":{"coordination":{"mode":"leader_facilitated","requireRoleRelevance":true}}}')`,
		`INSERT INTO workforce_change_sets(payload) VALUES
		 ('{"result":{"candidate":{"team":{"coordination":{"mode":"dynamic","suppressDuplicateContent":true}}}}}')`,
	} {
		if _, err = db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err = migrateNaturalTeamCoordinationSQLite(db); err != nil {
		t.Fatal(err)
	}
	if err = migrateTeamCoordinationDefaultsSQLite(db); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		table string
		path  string
		flag  string
	}{
		{table: "team_definitions", path: "$.coordination.mode", flag: "$.coordination.quietByDefault"},
		{table: "team_definition_amendments", path: "$.candidate.coordination.mode", flag: "$.candidate.coordination.requireRoleRelevance"},
		{table: "workforce_change_sets", path: "$.result.candidate.team.coordination.mode", flag: "$.result.candidate.team.coordination.suppressDuplicateContent"},
	} {
		var mode interface{}
		var flag int
		if err = db.QueryRow(`SELECT json_extract(payload, ?), json_extract(payload, ?) FROM `+check.table+` LIMIT 1`, check.path, check.flag).Scan(&mode, &flag); err != nil {
			t.Fatal(err)
		}
		if mode != nil || flag != 1 {
			t.Fatalf("%s mode=%v flag=%d", check.table, mode, flag)
		}
	}
	// The data migration is safe to replay on every SQLite open.
	if err = migrateNaturalTeamCoordinationSQLite(db); err != nil {
		t.Fatal(err)
	}
	if err = migrateTeamCoordinationDefaultsSQLite(db); err != nil {
		t.Fatal(err)
	}

	var quiet, relevant, duplicates, speakers int
	if err = db.QueryRow(`SELECT
		json_extract(payload, '$.coordination.quietByDefault'),
		json_extract(payload, '$.coordination.requireRoleRelevance'),
		json_extract(payload, '$.coordination.suppressDuplicateContent'),
		json_extract(payload, '$.coordination.maximumSpeakersPerRound')
		FROM team_definitions ORDER BY rowid LIMIT 1`).Scan(&quiet, &relevant, &duplicates, &speakers); err != nil {
		t.Fatal(err)
	}
	if quiet != 1 || relevant != 1 || duplicates != 1 || speakers != 2 {
		t.Fatalf("legacy defaults = quiet:%d relevant:%d duplicates:%d speakers:%d", quiet, relevant, duplicates, speakers)
	}
	if err = db.QueryRow(`SELECT
		json_extract(payload, '$.coordination.quietByDefault'),
		json_extract(payload, '$.coordination.requireRoleRelevance'),
		json_extract(payload, '$.coordination.suppressDuplicateContent'),
		json_extract(payload, '$.coordination.maximumSpeakersPerRound')
		FROM team_definitions ORDER BY rowid DESC LIMIT 1`).Scan(&quiet, &relevant, &duplicates, &speakers); err != nil {
		t.Fatal(err)
	}
	if quiet != 0 || relevant != 0 || duplicates != 0 || speakers != 7 {
		t.Fatalf("authored policy = quiet:%d relevant:%d duplicates:%d speakers:%d", quiet, relevant, duplicates, speakers)
	}
}
