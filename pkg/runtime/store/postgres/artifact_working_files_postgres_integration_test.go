package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

// The table must exist on a database that was already at the previous schema
// head when this kernel first opened it, not only on a fresh one: a current
// ledger skips every earlier migration.
func TestPostgresWorkingFilesTableIsCreatedOnAnExistingDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	_, pgSchema := openTestPostgresStore(t, "openseal_working_files_")
	schema := pgSchema.Name
	dsn := pgSchema.DSN
	// Put the database back to how the previous kernel left it.
	if _, err := pgSchema.DB.ExecContext(ctx, `DROP TABLE `+pgSchema.Table("artifact_working_files")); err != nil {
		t.Fatal(err)
	}
	if _, err := pgSchema.DB.ExecContext(ctx, `DELETE FROM `+pgSchema.Table("schema_migrations")+` WHERE version = $1`, artifactWorkingFilesMigrationVersion); err != nil {
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

	scope := models.Scope{Kind: "tenant", ID: "one"}
	for _, id := range []string{"report.pdf", "plot.py"} {
		if err := reopened.CreateArtifactVersion(ctx, &models.Artifact{
			ID: id, Version: 1, Scope: scope, Name: id, Type: "report", MediaType: "application/octet-stream",
			ContentRef: "object-store:" + id, Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 10,
			Classification: models.ArtifactClassificationInternal, CreatedAt: time.Now().UTC(),
			Provenance: models.ArtifactProvenance{Producer: models.ActivityActor{Type: "agent", ID: "analyst"}, RunID: "run-research"},
		}, 0); err != nil {
			t.Fatal(err)
		}
	}
	// The reply handed over report.pdf; plot.py stayed a working file.
	if err := reopened.MarkArtifactFiles(ctx, scope, []string{"plot.py"}, []string{"report.pdf"}); err != nil {
		t.Fatal(err)
	}
	for files, want := range map[models.ArtifactFiles]string{models.ArtifactFilesResults: "report.pdf", models.ArtifactFilesWorking: "plot.py"} {
		listed, err := reopened.ListArtifacts(ctx, models.ArtifactFilter{Scope: scope, Files: files, LatestOnly: true, Limit: 10})
		if err != nil || len(listed) != 1 || listed[0].ID != want {
			t.Fatalf("%s = %v, %v; want only %s", files, listed, err, want)
		}
	}
}
