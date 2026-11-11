package runtime

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The table must exist on a database that was already at the previous schema
// head when this kernel first opened it, not only on a fresh one: a current
// ledger skips every earlier migration.
func TestPostgresWorkingFilesTableIsCreatedOnAnExistingDatabase(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	schema := "openseal_working_files_" + uuid.NewString()[:8]
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
		_ = store.Close()
	})
	// Put the database back to how the previous kernel left it.
	if _, err := store.db.ExecContext(ctx, `DROP TABLE `+store.table("artifact_working_files")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM `+store.table("schema_migrations")+` WHERE version = $1`, artifactWorkingFilesMigrationVersion); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if version, err := reopened.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("schema head = %d, %v; want %d", version, err, currentPostgresSchemaVersion)
	}

	scope := Scope{Kind: "tenant", ID: "one"}
	catalog := NewArtifactCatalog(reopened)
	for _, id := range []string{"report.pdf", "plot.py"} {
		if _, err := catalog.Register(ctx, RegisterArtifactRequest{Artifact: validCatalogArtifact(id, 1)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := markRunFiles(ctx, reopened, scope, []savedArtifact{{Ref: artifactRef("report.pdf")}, {Ref: artifactRef("plot.py")}}, []ConversationReference{artifactRef("report.pdf")}); err != nil {
		t.Fatal(err)
	}
	for files, want := range map[ArtifactFiles]string{ArtifactFilesResults: "report.pdf", ArtifactFilesWorking: "plot.py"} {
		listed, err := catalog.List(ctx, ArtifactFilter{Scope: scope, Files: files, LatestOnly: true, Limit: 10})
		if err != nil || len(listed) != 1 || listed[0].ID != want {
			t.Fatalf("%s = %v, %v; want only %s", files, listed, err, want)
		}
	}
}
