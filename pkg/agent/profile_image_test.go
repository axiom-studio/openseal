package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestProfileImageUpdatesAreVersionedAndAudited(t *testing.T) {
	ctx := context.Background()
	registry := NewRegistry()
	definition, err := registry.RegisterDefinition(ctx, testDefinition("1", capability.RiskLevelRead, 1))
	if err != nil {
		t.Fatal(err)
	}
	deployment, _, err := registry.CreateDeployment(ctx, &AgentDeployment{
		ID: "writer", Scope: capability.ScopeReference{Kind: "tenant", ID: "one"},
		DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: RolloutActive, Environment: "production",
		Capacity: DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "owner", "create writer")
	if err != nil {
		t.Fatal(err)
	}
	proposed := cloneDeployment(deployment)
	proposed.ProfileImage = &ProfileImage{ArtifactID: "portrait-1", Version: 1}
	updated, audit, err := registry.UpdateDeployment(ctx, proposed, deployment.Revision, "user", "owner", "change profile picture")
	if err != nil {
		t.Fatal(err)
	}
	if updated.ProfileImage == nil || updated.ProfileImage.ArtifactID != "portrait-1" || audit == nil || audit.Reason != "change profile picture" {
		t.Fatalf("missing saved profile or audit: %#v, %#v", updated, audit)
	}
	proposed.ProfileImage.ArtifactID = "mutated"
	if updated.ProfileImage.ArtifactID != "portrait-1" {
		t.Fatal("profile reference aliases caller memory")
	}
	if _, _, err = registry.UpdateDeployment(ctx, proposed, deployment.Revision, "user", "owner", "stale replacement"); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("expected revision conflict, got %v", err)
	}
	for _, invalid := range []*ProfileImage{{ArtifactID: "portrait-1"}, {Version: 1}, {ArtifactID: "https://provider/image", Version: 1}, {ArtifactID: "portrait-1", Version: -1}} {
		candidate := cloneDeployment(updated)
		candidate.ProfileImage = invalid
		if err := candidate.Validate(); err == nil {
			t.Fatalf("accepted invalid image reference: %#v", invalid)
		}
	}
	removed := cloneDeployment(updated)
	removed.ProfileImage = nil
	cleared, _, err := registry.UpdateDeployment(ctx, removed, updated.Revision, "user", "owner", "remove profile picture")
	if err != nil || cleared.ProfileImage != nil {
		t.Fatalf("clear profile: %#v, %v", cleared, err)
	}
}
