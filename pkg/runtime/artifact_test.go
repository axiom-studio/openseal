package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestArtifactCatalogIsImmutableVersionedScopedAndQueryable(t *testing.T) {
	factories := []struct {
		name string
		open func(*testing.T) (ArtifactStore, func())
	}{
		{name: "memory", open: func(t *testing.T) (ArtifactStore, func()) {
			return NewMemoryStore(100), func() {}
		}},
		{name: "sqlite", open: func(t *testing.T) (ArtifactStore, func()) {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "artifacts.db"))
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
			catalog := NewArtifactCatalog(store)
			catalog.now = func() time.Time { return time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC) }
			ctx := context.Background()
			artifact := validCatalogArtifact("research-report", 1)

			created, err := catalog.Register(ctx, RegisterArtifactRequest{Artifact: artifact, ExpectedLatestVersion: 0})
			if err != nil {
				t.Fatal(err)
			}
			if created.Replayed || created.Artifact.Classification != ArtifactClassificationInternal || created.Artifact.Fingerprint == "" {
				t.Fatalf("created = %#v", created)
			}
			replayed, err := catalog.Register(ctx, RegisterArtifactRequest{Artifact: artifact, ExpectedLatestVersion: 0})
			if err != nil {
				t.Fatal(err)
			}
			if !replayed.Replayed || replayed.Artifact.Fingerprint != created.Artifact.Fingerprint {
				t.Fatalf("replayed = %#v", replayed)
			}

			changed := validCatalogArtifact("research-report", 1)
			changed.Name = "silently replaced.pdf"
			if _, err := catalog.Register(ctx, RegisterArtifactRequest{Artifact: changed, ExpectedLatestVersion: 0}); !errors.Is(err, ErrArtifactImmutable) {
				t.Fatalf("immutable replacement error = %v", err)
			}

			second := validCatalogArtifact("research-report", 2)
			second.ContentRef = "object-store:research-report-v2"
			second.Digest = digestFor("version two")
			second.Evidence = append(second.Evidence, EvidenceLink{
				Relation: EvidenceRelationDerivedFrom, TargetKind: EvidenceTargetArtifact,
				TargetRef: "research-report:1", Summary: "Editorial revision",
			})
			if _, err := catalog.Register(ctx, RegisterArtifactRequest{Artifact: second, ExpectedLatestVersion: 1}); err != nil {
				t.Fatal(err)
			}
			latest, err := catalog.Get(ctx, artifact.Scope, artifact.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if latest.Version != 2 || latest.Digest != second.Digest {
				t.Fatalf("latest = %#v", latest)
			}

			listed, err := catalog.List(ctx, ArtifactFilter{
				Scope: artifact.Scope, Owner: artifact.Provenance.Owner, LatestOnly: true, ProducerRunID: "run-research",
				EvidenceTarget: "research-report:1",
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(listed) != 1 || listed[0].Version != 2 {
				t.Fatalf("listed = %#v", listed)
			}
			otherOwner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "another-agent"}
			listed, err = catalog.List(ctx, ArtifactFilter{Scope: artifact.Scope, Owner: &otherOwner, LatestOnly: true})
			if err != nil || len(listed) != 0 {
				t.Fatalf("other owner artifacts = %#v, err = %v", listed, err)
			}

			otherScope := Scope{Kind: "tenant", ID: "other"}
			if _, err := catalog.Get(ctx, otherScope, artifact.ID, 0); !errors.Is(err, ErrArtifactNotFound) {
				t.Fatalf("cross-scope lookup error = %v", err)
			}
			gap := validCatalogArtifact("research-report", 4)
			if _, err := catalog.Register(ctx, RegisterArtifactRequest{Artifact: gap, ExpectedLatestVersion: 2}); !errors.Is(err, ErrArtifactVersionConflict) {
				t.Fatalf("version gap error = %v", err)
			}
		})
	}
}

func TestArtifactCatalogConcurrentReplayCreatesOneVersion(t *testing.T) {
	store := NewMemoryStore(100)
	catalog := NewArtifactCatalog(store)
	catalog.now = func() time.Time { return time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC) }
	artifact := validCatalogArtifact("game-log", 1)
	var created atomic.Int32
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 24)
	for range 24 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := catalog.Register(context.Background(), RegisterArtifactRequest{Artifact: artifact, ExpectedLatestVersion: 0})
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
	catalog := NewArtifactCatalog(NewMemoryStore(100))
	tests := []struct {
		name   string
		mutate func(*Artifact)
	}{
		{name: "secret metadata", mutate: func(a *Artifact) { a.Metadata["apiKey"] = "resolved-secret" }},
		{name: "signed content URL", mutate: func(a *Artifact) { a.ContentRef = "https://objects.test/report?signature=secret" }},
		{name: "signed evidence URL", mutate: func(a *Artifact) {
			a.Evidence[0].TargetRef = "https://forum.test/thread?X-Amz-Signature=secret"
		}},
		{name: "invalid confidence", mutate: func(a *Artifact) {
			confidence := 1.2
			a.Evidence[0].Confidence = &confidence
		}},
		{name: "invalid content availability", mutate: func(a *Artifact) { a.ContentAvailability = "maybe" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			artifact := validCatalogArtifact("unsafe-"+test.name, 1)
			test.mutate(artifact)
			if _, err := catalog.Register(context.Background(), RegisterArtifactRequest{Artifact: artifact}); !errors.Is(err, ErrInvalidArtifactRecord) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSQLiteArtifactCatalogSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	catalog := NewArtifactCatalog(store)
	created, err := catalog.Register(context.Background(), RegisterArtifactRequest{Artifact: validCatalogArtifact("restart-report", 1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := NewArtifactCatalog(reopened).Get(context.Background(), created.Artifact.Scope, created.Artifact.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Digest != created.Artifact.Digest || loaded.Provenance.RequestID != "request-research" {
		t.Fatalf("loaded = %#v", loaded)
	}
}

func validCatalogArtifact(id string, version int64) *Artifact {
	confidence := 0.91
	return &Artifact{
		ID: id, Version: version, Scope: Scope{Kind: "tenant", ID: "one"}, Name: "market-research.pdf",
		Type: "report", MediaType: "application/pdf", ContentRef: "object-store:" + id,
		Digest: digestFor(id), SizeBytes: 4_096, Classification: ArtifactClassificationInternal,
		Retention: ArtifactRetention{ExpiresAt: artifactTimePointer(time.Date(2027, 7, 10, 0, 0, 0, 0, time.UTC))},
		MetadataSchema: map[string]interface{}{
			"type": "object", "required": []interface{}{"pages"},
			"properties":           map[string]interface{}{"pages": map[string]interface{}{"type": "integer", "minimum": float64(1)}},
			"additionalProperties": false,
		},
		Metadata: map[string]interface{}{"pages": 12},
		Provenance: ArtifactProvenance{
			Producer: ActivityActor{Type: "agent", ID: "analyst"},
			Owner:    &ObjectiveOwner{Type: OwnerTypeTeam, ID: "market-research"}, RunID: "run-research",
			ObjectiveID: "objective-research", RequestID: "request-research",
		},
		Evidence: []EvidenceLink{{
			Relation: EvidenceRelationCites, TargetKind: EvidenceTargetExternalSource,
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
