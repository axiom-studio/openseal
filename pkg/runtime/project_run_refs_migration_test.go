package runtime

import (
	"database/sql"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestSQLiteProjectMigrationRemovesDerivedRunRefs(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = migrateProjects(db); err != nil {
		t.Fatal(err)
	}
	legacy := `{"id":"project","runRefs":["run-a"],"objectiveRefs":["objective-a"]}`
	if _, err = db.Exec(`INSERT INTO projects(id,scope_kind,scope_id,owner_type,owner_id,status,revision,updated_at,payload) VALUES('project','local','one','agent','agent','active',1,CURRENT_TIMESTAMP,?)`, legacy); err != nil {
		t.Fatal(err)
	}
	if err = migrateProjects(db); err != nil {
		t.Fatal(err)
	}
	var payload string
	if err = db.QueryRow(`SELECT payload FROM projects WHERE id='project'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, "runRefs") || !strings.Contains(payload, "objectiveRefs") {
		t.Fatalf("migrated Project payload = %s", payload)
	}
}
