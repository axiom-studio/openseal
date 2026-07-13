//go:build integration

package runtime

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill/openclaw"
	"github.com/axiom-studio/openseal/pkg/skill/sourceartifact"
	"github.com/google/uuid"
)

func TestPostgresSkillSourceArtifactsAreReplicaSafeScopedDurableAndCollectable(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	schema := "openseal_skill_source_" + uuid.NewString()[:8]
	primary, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_ = primary.Close()
	})
	replica, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	if version, err := primary.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	bundle := postgresSourceArtifactBundle()
	compilation, err := openclaw.Compile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	services := make([]*sourceartifact.Service, 2)
	services[0], _ = sourceartifact.NewService(primary)
	services[1], _ = sourceartifact.NewService(replica)
	var created atomic.Int32
	var wait sync.WaitGroup
	for index := 0; index < 8; index++ {
		wait.Add(1)
		go func(service *sourceartifact.Service) {
			defer wait.Done()
			_, wasCreated, importErr := service.ImportOpenClaw(ctx, sourceartifact.ImportOpenClawRequest{
				Scope: scope, Compilation: compilation, ReferenceID: "install:acme/research", ReferenceKind: "installation",
			})
			if importErr != nil {
				t.Errorf("replica import: %v", importErr)
			}
			if wasCreated {
				created.Add(1)
			}
		}(services[index%len(services)])
	}
	wait.Wait()
	if created.Load() != 1 {
		t.Fatalf("replica created count=%d", created.Load())
	}
	if err := replica.Close(); err != nil {
		t.Fatal(err)
	}
	restartedStore, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer restartedStore.Close()
	restarted, _ := sourceartifact.NewService(restartedStore)
	exported, err := restarted.ExportOpenClaw(ctx, scope, compilation.SourceDigest)
	if err != nil || !reflect.DeepEqual(exported, bundle) {
		t.Fatalf("restart export equal=%t err=%v", reflect.DeepEqual(exported, bundle), err)
	}
	if _, err := restarted.ExportOpenClaw(ctx, capability.ScopeReference{Kind: "tenant", ID: "two"}, compilation.SourceDigest); !errors.Is(err, sourceartifact.ErrNotFound) {
		t.Fatalf("cross-tenant export error=%v", err)
	}
	updatedBundle := postgresSourceArtifactBundle()
	updatedBundle.Source.Version = "2.0.0"
	updatedBundle.Files[0].Content = []byte("cite sources and counter-evidence\n")
	updated, err := openclaw.Compile(updatedBundle)
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := restarted.ImportOpenClaw(ctx, sourceartifact.ImportOpenClawRequest{
		Scope: scope, Compilation: updated, ReferenceID: "install:acme/research", ReferenceKind: "installation",
	}); err != nil || !created {
		t.Fatalf("updated source created=%t err=%v", created, err)
	}
	oldReport, err := restarted.GarbageCollect(ctx, time.Now().UTC().Add(time.Hour), 0, 10)
	if err != nil || oldReport.DeletedArtifacts != 1 {
		t.Fatalf("old source collection=%#v err=%v", oldReport, err)
	}
	if err := restarted.Release(ctx, scope, updated.SourceDigest, "install:acme/research"); err != nil {
		t.Fatal(err)
	}
	report, err := restarted.GarbageCollect(ctx, time.Now().UTC().Add(time.Hour), 0, 10)
	if err != nil || report.DeletedArtifacts != 1 {
		t.Fatalf("garbage collection=%#v err=%v", report, err)
	}
	if err := primary.RollbackPostgresMigrations(ctx, 16); err != nil {
		t.Fatal(err)
	}
	var tables int
	if err := primary.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=$1 AND table_name IN ('skill_source_artifacts','skill_source_artifact_references')`, primary.schema).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("source artifact tables after rollback=%d err=%v", tables, err)
	}
	if err := primary.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if version, err := primary.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("reapplied schema version=%d err=%v", version, err)
	}
}

func postgresSourceArtifactBundle() openclaw.Bundle {
	return openclaw.Bundle{
		SkillMD: []byte("---\nname: research\ndescription: Research with evidence.\n---\nRead references/guide.md."),
		Files:   []openclaw.File{{Path: "references/guide.md", Content: []byte("cite sources\n")}},
		Source:  openclaw.Source{Registry: "https://registry.example", Publisher: "acme", Reference: "@acme/research", ExpectedName: "research", Version: "1.0.0", Trust: map[string]interface{}{"verified": true}},
	}
}
