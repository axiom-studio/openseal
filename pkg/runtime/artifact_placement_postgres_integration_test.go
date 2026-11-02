//go:build integration

package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestPostgresArtifactPlacementBackfill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store, dsn, schema := openSpacesPostgresStore(t, "openseal_artifact_placement_")
	scope := Scope{Kind: "tenant", ID: "one"}
	spaces := NewSpaceService(store)
	sales, err := spaces.CreateSpace(ctx, CreateSpaceRequest{Scope: scope, Name: "Sales", Look: SpaceLookHarbor})
	if err != nil {
		t.Fatal(err)
	}
	work, err := spaces.CreateSpace(ctx, CreateSpaceRequest{Scope: scope, Name: "Work", Look: SpaceLookGraphite})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := spaces.PutSpaceMember(ctx, PutSpaceMemberRequest{Scope: scope, SpaceID: work.ID, Type: SpaceMemberTeam, MemberID: "market-research"}); err != nil {
		t.Fatal(err)
	}
	chat, _, err := NewConversationService(store).CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "helper"}, Title: "Pitch", SpaceID: sales.ID, IdempotencyKey: "pitch",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []struct {
		id, root string
		context  map[string]interface{}
	}{
		{id: "run-chat", root: "run-chat", context: map[string]interface{}{"conversationId": chat.ID}},
		{id: "run-delegate", root: "run-chat"},
		{id: "run-schedule", root: "run-schedule"},
	} {
		payload, _ := json.Marshal(map[string]interface{}{"id": run.id, "scope": scope, "rootRunId": run.root, "status": "completed", "context": run.context})
		now := time.Now().UTC()
		if _, err := store.db.ExecContext(ctx, `INSERT INTO `+store.table("agent_runs")+`
			(id, scope_kind, scope_id, root_run_id, status, available_at, queue_entered_at, created_at, payload)
			VALUES ($1, $2, $3, $4, 'completed', $5, $5, $5, $6)`, run.id, scope.Kind, scope.ID, run.root, now, string(payload)); err != nil {
			t.Fatal(err)
		}
	}
	legacy := NewArtifactCatalog(store)
	fixtures := map[string]func(*Artifact){
		"from-run":    func(a *Artifact) { a.Provenance.RunID = "run-delegate" },
		"owner-space": func(a *Artifact) { a.Provenance.RunID = "run-schedule" },
		"upload":      func(a *Artifact) { a.Provenance.RunID, a.Provenance.Owner = "", nil },
	}
	for id, mutate := range fixtures {
		artifact := validCatalogArtifact(id, 1)
		mutate(artifact)
		if _, err := legacy.Register(ctx, RegisterArtifactRequest{Artifact: artifact}); err != nil {
			t.Fatal(err)
		}
	}
	// A database at migration 67 has neither the backfill nor the space index.
	if err := store.RollbackPostgresMigrations(ctx, artifactPlacementMigrationVersion-1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP INDEX IF EXISTS `+store.table("artifacts_space_page_idx")); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if version, err := reopened.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("schema version = %d, err = %v", version, err)
	}
	var indexed bool
	if err := reopened.db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+".artifacts_space_page_idx").Scan(&indexed); err != nil || !indexed {
		t.Fatalf("space page index = %v, err = %v", indexed, err)
	}
	catalog := NewArtifactCatalog(reopened).WithPlacement(NewStoreArtifactPlacement(reopened))
	for id, want := range map[string][2]string{
		"from-run":    {chat.ID, sales.ID},
		"owner-space": {"", work.ID},
		"upload":      {"", PersonalSpaceID(scope)},
	} {
		artifact, err := catalog.Get(ctx, scope, id, 1)
		if err != nil {
			t.Fatal(err)
		}
		if got := artifact.Provenance; got.ConversationID != want[0] || got.SpaceID != want[1] {
			t.Fatalf("%s placement = (%q, %q), want %v", id, got.ConversationID, got.SpaceID, want)
		}
		replay := validCatalogArtifact(id, 1)
		fixtures[id](replay)
		if result, err := catalog.Register(ctx, RegisterArtifactRequest{Artifact: replay}); err != nil || !result.Replayed {
			t.Fatalf("replay %s = %#v, %v", id, result, err)
		}
	}
}
