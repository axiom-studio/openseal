package artifactcatalog_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestArtifactCatalogIsImmutableVersionedScopedAndQueryable(t *testing.T) {
	factories := []struct {
		name string
		open func(*testing.T) (models.ArtifactStore, func())
	}{
		{name: "sqlite", open: func(t *testing.T) (models.ArtifactStore, func()) {
			store, err := sqlite.New(filepath.Join(t.TempDir(), "artifacts.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	}
	for _, factory := range factories {
		t.Run(factory.name, func(t *testing.T) {
			store, closeStore := factory.open(t)
			defer closeStore()
			catalog := artifactcatalog.NewCatalog(store, models.WithClock(func() time.Time { return time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC) }))
			ctx := context.Background()
			artifact := validCatalogArtifact("research-report", 1)

			created, err := catalog.Register(ctx, artifactcatalog.RegisterArtifactRequest{Artifact: artifact, ExpectedLatestVersion: 0})
			if err != nil {
				t.Fatal(err)
			}
			if created.Replayed || created.Artifact.Classification != models.ArtifactClassificationInternal || created.Artifact.Fingerprint == "" {
				t.Fatalf("created = %#v", created)
			}
			replayed, err := catalog.Register(ctx, artifactcatalog.RegisterArtifactRequest{Artifact: artifact, ExpectedLatestVersion: 0})
			if err != nil {
				t.Fatal(err)
			}
			if !replayed.Replayed || replayed.Artifact.Fingerprint != created.Artifact.Fingerprint {
				t.Fatalf("replayed = %#v", replayed)
			}

			changed := validCatalogArtifact("research-report", 1)
			changed.Name = "silently replaced.pdf"
			if _, err := catalog.Register(ctx, artifactcatalog.RegisterArtifactRequest{Artifact: changed, ExpectedLatestVersion: 0}); !errors.Is(err, models.ErrArtifactImmutable) {
				t.Fatalf("immutable replacement error = %v", err)
			}

			second := validCatalogArtifact("research-report", 2)
			second.ContentRef = "object-store:research-report-v2"
			second.Digest = digestFor("version two")
			second.Evidence = append(second.Evidence, models.EvidenceLink{
				Relation: models.EvidenceRelationDerivedFrom, TargetKind: models.EvidenceTargetArtifact,
				TargetRef: "research-report:1", Summary: "Editorial revision",
			})
			if _, err := catalog.Register(ctx, artifactcatalog.RegisterArtifactRequest{Artifact: second, ExpectedLatestVersion: 1}); err != nil {
				t.Fatal(err)
			}
			latest, err := catalog.Get(ctx, artifact.Scope, artifact.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if latest.Version != 2 || latest.Digest != second.Digest {
				t.Fatalf("latest = %#v", latest)
			}

			listed, err := catalog.List(ctx, models.ArtifactFilter{
				Scope: artifact.Scope, Owner: artifact.Provenance.Owner, LatestOnly: true, ProducerRunID: "run-research",
				EvidenceTarget: "research-report:1",
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(listed) != 1 || listed[0].Version != 2 {
				t.Fatalf("listed = %#v", listed)
			}
			otherOwner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "another-agent"}
			listed, err = catalog.List(ctx, models.ArtifactFilter{Scope: artifact.Scope, Owner: &otherOwner, LatestOnly: true})
			if err != nil || len(listed) != 0 {
				t.Fatalf("other owner artifacts = %#v, err = %v", listed, err)
			}

			otherScope := models.Scope{Kind: "tenant", ID: "other"}
			if _, err := catalog.Get(ctx, otherScope, artifact.ID, 0); !errors.Is(err, artifactcatalog.ErrArtifactNotFound) {
				t.Fatalf("cross-scope lookup error = %v", err)
			}
			gap := validCatalogArtifact("research-report", 4)
			if _, err := catalog.Register(ctx, artifactcatalog.RegisterArtifactRequest{Artifact: gap, ExpectedLatestVersion: 2}); !errors.Is(err, models.ErrArtifactVersionConflict) {
				t.Fatalf("version gap error = %v", err)
			}
		})
	}
}

func TestArtifactCatalogConcurrentReplayCreatesOneVersion(t *testing.T) {
	store := kerneltest.NewStore(t)
	catalog := artifactcatalog.NewCatalog(store, models.WithClock(func() time.Time { return time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC) }))
	artifact := validCatalogArtifact("game-log", 1)
	var created atomic.Int32
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 24)
	for range 24 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := catalog.Register(context.Background(), artifactcatalog.RegisterArtifactRequest{Artifact: artifact, ExpectedLatestVersion: 0})
			if err != nil {
				errorsSeen <- err
				return
			}
			if !result.Replayed {
				created.Add(1)
			}
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatal(err)
	}
	if created.Load() != 1 {
		t.Fatalf("created count = %d", created.Load())
	}
}

func TestArtifactCatalogRejectsSecretsSignedURLsAndInvalidEvidence(t *testing.T) {
	catalog := artifactcatalog.NewCatalog(kerneltest.NewStore(t))
	tests := []struct {
		name   string
		mutate func(*models.Artifact)
	}{
		{name: "secret metadata", mutate: func(a *models.Artifact) { a.Metadata["apiKey"] = "resolved-secret" }},
		{name: "signed content URL", mutate: func(a *models.Artifact) { a.ContentRef = "https://objects.test/report?signature=secret" }},
		{name: "signed evidence URL", mutate: func(a *models.Artifact) {
			a.Evidence[0].TargetRef = "https://forum.test/thread?X-Amz-Signature=secret"
		}},
		{name: "invalid confidence", mutate: func(a *models.Artifact) {
			confidence := 1.2
			a.Evidence[0].Confidence = &confidence
		}},
		{name: "invalid content availability", mutate: func(a *models.Artifact) { a.ContentAvailability = "maybe" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			artifact := validCatalogArtifact("unsafe-"+test.name, 1)
			test.mutate(artifact)
			if _, err := catalog.Register(context.Background(), artifactcatalog.RegisterArtifactRequest{Artifact: artifact}); !errors.Is(err, models.ErrInvalidArtifactRecord) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSQLiteArtifactCatalogSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	store, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	catalog := artifactcatalog.NewCatalog(store)
	created, err := catalog.Register(context.Background(), artifactcatalog.RegisterArtifactRequest{Artifact: validCatalogArtifact("restart-report", 1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := artifactcatalog.NewCatalog(reopened).Get(context.Background(), created.Artifact.Scope, created.Artifact.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Digest != created.Artifact.Digest || loaded.Provenance.RequestID != "request-research" {
		t.Fatalf("loaded = %#v", loaded)
	}
}

// Offset and limit apply after every predicate and latest-version selection,
// never before. The expected pages are worked out from the seeded fixtures,
// which all share one creation time, so the catalog orders them by ID and
// then by version, newest first.
func TestSQLiteArtifactCatalogPagesMatchUnpagedFilters(t *testing.T) {
	ctx := context.Background()
	sqlite, err := sqlite.New(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlite.Close()
	stamp := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	catalog := artifactcatalog.NewCatalog(sqlite, models.WithClock(func() time.Time { return stamp }))
	for i := 0; i < 125; i++ {
		artifact := validCatalogArtifact(fmt.Sprintf("artifact-%03d", i), 1)
		if i%2 == 0 {
			artifact.Provenance.Owner = &models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "alice"}
		}
		if i%3 == 0 {
			artifact.Type = "slides"
			artifact.MediaType = "text/html"
		}
		if _, err := catalog.Register(ctx, artifactcatalog.RegisterArtifactRequest{Artifact: artifact}); err != nil {
			t.Fatal(err)
		}
	}
	// The latest version changes owners, so filtering must select latest
	// globally before applying the owner predicate.
	latest := validCatalogArtifact("artifact-000", 2)
	latest.ContentRef = "object-store:artifact-000-v2"
	latest.Digest = digestFor("artifact-000-v2")
	if _, err := catalog.Register(ctx, artifactcatalog.RegisterArtifactRequest{Artifact: latest, ExpectedLatestVersion: 1}); err != nil {
		t.Fatal(err)
	}
	owner := &models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "alice"}
	// ids lists artifact-<from>..artifact-<to> (stepping by step) at version 1.
	ids := func(from, to, step int) []string {
		listed := []string{}
		for i := from; i <= to; i += step {
			listed = append(listed, fmt.Sprintf("artifact-%03d:1", i))
		}
		return listed
	}
	cases := []struct {
		name   string
		filter models.ArtifactFilter
		want   []string
	}{{
		// Alice owns the even artifacts except artifact-000, whose latest
		// version moved to the Team: 002..124, so the second page of 25
		// starts at 052.
		name:   "latest owner",
		filter: models.ArtifactFilter{Scope: models.Scope{Kind: "tenant", ID: "one"}, Owner: owner, LatestOnly: true, Limit: 25, Offset: 25},
		want:   ids(52, 100, 2),
	}, {
		// Slides are every third artifact except artifact-000, whose latest
		// version is a report: 003..123, so offset 20 starts at 063.
		name:   "latest type and media type",
		filter: models.ArtifactFilter{Scope: models.Scope{Kind: "tenant", ID: "one"}, Types: []string{" slides "}, MediaTypes: []string{"text/html"}, LatestOnly: true, Limit: 20, Offset: 20},
		want:   ids(63, 120, 3),
	}, {
		// Every version matches: artifact-000 v2 and v1 come first, so
		// offset 100 starts at artifact-099 and runs to the end.
		name:   "every version by producer",
		filter: models.ArtifactFilter{Scope: models.Scope{Kind: "tenant", ID: "one"}, ProducerRunID: "run-research", ProducerRequestID: "request-research", Classifications: []models.ArtifactClassification{models.ArtifactClassificationInternal}, Limit: 100, Offset: 100},
		want:   ids(99, 124, 1),
	}, {
		// Without latest-only, artifact-000 v1 is still Alice's: 000..124,
		// so offset 40 starts at 080.
		name:   "every version by owner and evidence",
		filter: models.ArtifactFilter{Scope: models.Scope{Kind: "tenant", ID: "one"}, Owner: owner, EvidenceTarget: "https://forum.example/research/thread-7", Limit: 20, Offset: 40},
		want:   ids(80, 118, 2),
	}}
	for _, tc := range cases {
		artifacts, err := catalog.List(ctx, tc.filter)
		if err != nil {
			t.Fatal(err)
		}
		page := []string{}
		for _, artifact := range artifacts {
			page = append(page, fmt.Sprintf("%s:%d", artifact.ID, artifact.Version))
		}
		if !reflect.DeepEqual(page, tc.want) {
			t.Fatalf("%s: page %v, want %v", tc.name, page, tc.want)
		}
	}
}

func validCatalogArtifact(id string, version int64) *models.Artifact {
	confidence := 0.91
	return &models.Artifact{
		ID: id, Version: version, Scope: models.Scope{Kind: "tenant", ID: "one"}, Name: "market-research.pdf",
		Type: "report", MediaType: "application/pdf", ContentRef: "object-store:" + id,
		Digest: digestFor(id), SizeBytes: 4_096, Classification: models.ArtifactClassificationInternal,
		Retention: models.ArtifactRetention{ExpiresAt: artifactTimePointer(time.Date(2027, 7, 10, 0, 0, 0, 0, time.UTC))},
		MetadataSchema: map[string]interface{}{
			"type": "object", "required": []interface{}{"pages"},
			"properties":           map[string]interface{}{"pages": map[string]interface{}{"type": "integer", "minimum": float64(1)}},
			"additionalProperties": false,
		},
		Metadata: map[string]interface{}{"pages": 12},
		Provenance: models.ArtifactProvenance{
			Producer: models.ActivityActor{Type: "agent", ID: "analyst"},
			Owner:    &models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "market-research"}, RunID: "run-research",
			ObjectiveID: "objective-research", RequestID: "request-research",
		},
		Evidence: []models.EvidenceLink{{
			Relation: models.EvidenceRelationCites, TargetKind: models.EvidenceTargetExternalSource,
			TargetRef: "https://forum.example/research/thread-7", Summary: "Public product feedback",
			Confidence: &confidence, ObservedAt: artifactTimePointer(time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)),
		}},
	}
}

func digestFor(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func artifactTimePointer(value time.Time) *time.Time { return &value }
