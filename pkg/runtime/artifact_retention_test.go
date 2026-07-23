package runtime

import (
	"context"
	"testing"
	"time"
)

type retentionContentStore struct{ available map[string]bool }

func (s *retentionContentStore) Available(_ context.Context, _ Scope, ref string) (bool, error) {
	return s.available[ref], nil
}

func (s *retentionContentStore) Delete(_ context.Context, _ Scope, ref string) (bool, error) {
	deleted := s.available[ref]
	delete(s.available, ref)
	return deleted, nil
}

func TestArtifactRetentionSweepDeletesDueBytesAndPreservesLegalHold(t *testing.T) {
	ctx := t.Context()
	store := NewMemoryStore()
	catalog := NewArtifactCatalog(store)
	now := time.Date(2026, 7, 13, 1, 0, 0, 0, time.UTC)
	for _, fixture := range []struct {
		id      string
		expires time.Time
		hold    bool
	}{
		{id: "due", expires: now.Add(-time.Hour)},
		{id: "held", expires: now.Add(-time.Hour), hold: true},
		{id: "future", expires: now.Add(time.Hour)},
	} {
		artifact := validCatalogArtifact(fixture.id, 1)
		artifact.ContentRef = "content:" + fixture.id
		artifact.Digest = digestFor(fixture.id)
		artifact.Retention = ArtifactRetention{ExpiresAt: &fixture.expires, LegalHold: fixture.hold}
		if _, err := catalog.Register(ctx, RegisterArtifactRequest{Artifact: artifact}); err != nil {
			t.Fatal(err)
		}
	}
	content := &retentionContentStore{available: map[string]bool{"content:due": true, "content:held": true, "content:future": true}}
	service, err := NewArtifactRetentionService(store, content, content)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	result, err := service.Sweep(ctx, validCatalogArtifact("scope", 1).Scope, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if result.Processed != 2 || len(result.Deleted) != 1 || result.Deleted[0].ArtifactID != "due" || result.Held != 1 || content.available["content:due"] || !content.available["content:held"] || !content.available["content:future"] {
		t.Fatalf("retention result=%#v available=%#v", result, content.available)
	}
	replayed, err := service.Sweep(ctx, validCatalogArtifact("scope", 1).Scope, 0, 50)
	if err != nil || len(replayed.Deleted) != 0 || replayed.Missing != 1 || replayed.Held != 1 {
		t.Fatalf("replayed retention=%#v err=%v", replayed, err)
	}
}
