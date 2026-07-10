package openseal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

func TestEngineExposesArtifactEvidenceCatalog(t *testing.T) {
	engine, err := New(WithStore(runtime.NewMemoryStore(100)))
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "local", ID: "artifacts"}
	digest := sha256.Sum256([]byte("game log"))
	created, err := engine.RegisterArtifact(context.Background(), RegisterArtifactRequest{
		Artifact: &Artifact{
			ID: "snake-game-log", Version: 1, Scope: scope, Name: "snake-game.json",
			Type: "game-log", MediaType: "application/json", ContentRef: "local-store:snake-game-log",
			Digest: "sha256:" + hex.EncodeToString(digest[:]), Classification: ArtifactClassInternal,
			Provenance: ArtifactProvenance{Producer: ActivityActor{Type: "agent", ID: "player-one"}, RunID: "run-game"},
			Evidence: []ArtifactEvidenceLink{{
				Relation: ArtifactEvidenceOutputOf, TargetKind: ArtifactEvidenceTargetRun, TargetRef: "run-game",
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Replayed || created.Artifact.Fingerprint == "" {
		t.Fatalf("created = %#v", created)
	}
	listed, err := engine.ListArtifacts(context.Background(), ArtifactFilter{
		Scope: scope, EvidenceTarget: "run-game", LatestOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != created.Artifact.ID {
		t.Fatalf("listed = %#v", listed)
	}
}
