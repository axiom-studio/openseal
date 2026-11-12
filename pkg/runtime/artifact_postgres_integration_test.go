//go:build integration

package runtime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
)

func TestPostgresArtifactCatalogIsConcurrentRecoverableAndScoped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	primary, pgSchema := openTestPostgresStore(t, "openseal_artifacts_")
	schema := pgSchema.Name
	dsn := pgSchema.DSN
	replica, err := postgres.NewPostgresStore(ctx, dsn, postgres.WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	stores := []*postgres.Store{primary, replica}
	artifact := validCatalogArtifact("postgres-report", 1)
	var created atomic.Int32
	errorsSeen := make(chan error, 20)
	var wait sync.WaitGroup
	for index := 0; index < 20; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			result, registerErr := artifactcatalog.NewArtifactCatalog(stores[index%2]).Register(ctx, artifactcatalog.RegisterArtifactRequest{Artifact: artifact})
			if registerErr != nil {
				errorsSeen <- registerErr
				return
			}
			if !result.Replayed {
				created.Add(1)
			}
		}(index)
	}
	wait.Wait()
	close(errorsSeen)
	for registerErr := range errorsSeen {
		t.Fatal(registerErr)
	}
	if created.Load() != 1 {
		t.Fatalf("created count = %d", created.Load())
	}
	second := validCatalogArtifact(artifact.ID, 2)
	second.ContentRef = "object-store:postgres-report-v2"
	second.Digest = digestFor("postgres-version-two")
	second.Evidence = append(second.Evidence, EvidenceLink{
		Relation: EvidenceRelationDerivedFrom, TargetKind: EvidenceTargetArtifact, TargetRef: "postgres-report:1",
	})
	if _, err := artifactcatalog.NewArtifactCatalog(replica).Register(ctx, artifactcatalog.RegisterArtifactRequest{Artifact: second, ExpectedLatestVersion: 1}); err != nil {
		t.Fatal(err)
	}
	latest, err := artifactcatalog.NewArtifactCatalog(primary).Get(ctx, artifact.Scope, artifact.ID, 0)
	if err != nil || latest.Version != 2 {
		t.Fatalf("latest = %#v, %v", latest, err)
	}
	listed, err := artifactcatalog.NewArtifactCatalog(primary).List(ctx, ArtifactFilter{
		Scope: artifact.Scope, LatestOnly: true, EvidenceTarget: "postgres-report:1",
	})
	if err != nil || len(listed) != 1 || listed[0].Version != 2 {
		t.Fatalf("listed = %#v, %v", listed, err)
	}
	if crossScope, err := replica.GetArtifact(ctx, Scope{Kind: "tenant", ID: "other"}, artifact.ID, 0); err != nil || crossScope != nil {
		t.Fatalf("cross-scope artifact = %#v, %v", crossScope, err)
	}
	if version, err := primary.PostgresSchemaVersion(ctx); err != nil || version != postgresSchemaHead(t) {
		t.Fatalf("schema version = %d, %v", version, err)
	}
}
