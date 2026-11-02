package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestArtifactCatalogPlacesArtifactsMadeOutsideAChatInTheOwnersSpace(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "one"}
	spaces := NewSpaceService(store)
	work, err := spaces.CreateSpace(ctx, CreateSpaceRequest{Scope: scope, Name: "Work", Look: SpaceLookHarbor})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := spaces.PutSpaceMember(ctx, PutSpaceMemberRequest{Scope: scope, SpaceID: work.ID, Type: SpaceMemberTeam, MemberID: "market-research"}); err != nil {
		t.Fatal(err)
	}
	catalog := NewArtifactCatalog(store).WithPlacement(NewStoreArtifactPlacement(store))
	personal := PersonalSpaceID(scope)
	for name, test := range map[string]struct {
		mutate           func(*Artifact)
		wantConversation string
		wantSpace        string
	}{
		"owner-in-space":    {mutate: func(a *Artifact) { a.Provenance.RunID = "" }, wantSpace: work.ID},
		"owner-in-no-space": {mutate: func(a *Artifact) { a.Provenance.Owner = &ObjectiveOwner{Type: OwnerTypeAgent, ID: "loner"} }, wantSpace: personal},
		"no-owner":          {mutate: func(a *Artifact) { a.Provenance.Owner = nil }, wantSpace: personal},
		"chat-gone":         {mutate: func(a *Artifact) { a.Provenance.ConversationID = "chat-gone" }, wantConversation: "chat-gone", wantSpace: work.ID},
		"explicit-space":    {mutate: func(a *Artifact) { a.Provenance.SpaceID = "space-kept" }, wantSpace: "space-kept"},
	} {
		t.Run(name, func(t *testing.T) {
			artifact := validCatalogArtifact("outside-"+name, 1)
			test.mutate(artifact)
			result, err := catalog.Register(ctx, RegisterArtifactRequest{Artifact: artifact})
			if err != nil {
				t.Fatal(err)
			}
			if got := result.Artifact.Provenance; got.ConversationID != test.wantConversation || got.SpaceID != test.wantSpace {
				t.Fatalf("placement = (%q, %q), want (%q, %q)", got.ConversationID, got.SpaceID, test.wantConversation, test.wantSpace)
			}
		})
	}
	if _, err := spaces.GetSpace(ctx, scope, personal); err != nil {
		t.Fatalf("Personal space was not created: %v", err)
	}
}

func TestSQLiteBackfillPlacesArtifactsMadeBeforePlacement(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
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
	insertRun := func(id, root string, runContext map[string]interface{}) {
		t.Helper()
		payload, err := json.Marshal(map[string]interface{}{"id": id, "scope": scope, "rootRunId": root, "status": "completed", "context": runContext})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`INSERT INTO agent_runs (id, scope_kind, scope_id, root_run_id, status, created_at, payload)
			VALUES (?, ?, ?, ?, 'completed', ?, ?)`, id, scope.Kind, scope.ID, root, time.Now().UTC(), string(payload)); err != nil {
			t.Fatal(err)
		}
	}
	insertRun("run-chat", "run-chat", map[string]interface{}{"conversationId": chat.ID})
	insertRun("run-delegate", "run-chat", nil)
	insertRun("run-schedule", "run-schedule", nil)

	// Artifacts registered before placement existed carry no chat or space.
	legacy := NewArtifactCatalog(store)
	fixtures := map[string]func(*Artifact){
		"from-run": func(a *Artifact) { a.Provenance.RunID = "run-delegate" },
		"from-metadata": func(a *Artifact) {
			a.Provenance.RunID, a.MetadataSchema, a.Metadata = "", nil, map[string]interface{}{"conversationId": chat.ID}
		},
		"owner-space": func(a *Artifact) { a.Provenance.RunID = "run-schedule" },
		"upload":      func(a *Artifact) { a.Provenance.RunID, a.Provenance.Owner = "", nil },
		"chat-gone": func(a *Artifact) {
			a.Provenance.RunID, a.Provenance.Owner, a.Provenance.ConversationID = "", nil, "chat-gone"
		},
		"already-placed": func(a *Artifact) {
			a.Provenance.ConversationID, a.Provenance.SpaceID = "chat-kept", "space-kept"
		},
	}
	for id, mutate := range fixtures {
		artifact := validCatalogArtifact(id, 1)
		mutate(artifact)
		if _, err := legacy.Register(ctx, RegisterArtifactRequest{Artifact: artifact}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	// A later version of a legacy artifact is placed on its own.
	second := validCatalogArtifact("from-run", 2)
	second.ContentRef, second.Digest = "object-store:from-run-v2", digestFor("from-run-v2")
	second.Provenance.RunID = "run-schedule"
	if _, err := legacy.Register(ctx, RegisterArtifactRequest{Artifact: second, ExpectedLatestVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopen := func() *SQLiteStore {
		t.Helper()
		reopened, err := NewSQLiteStore(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reopened.Close() })
		return reopened
	}
	store = reopen()
	spaces = NewSpaceService(store)
	catalog := NewArtifactCatalog(store).WithPlacement(NewStoreArtifactPlacement(store))
	personal := PersonalSpaceID(scope)
	for _, want := range []struct {
		id               string
		version          int64
		wantConversation string
		wantSpace        string
	}{
		{id: "from-run", version: 1, wantConversation: chat.ID, wantSpace: sales.ID},
		{id: "from-run", version: 2, wantSpace: work.ID},
		{id: "from-metadata", version: 1, wantConversation: chat.ID, wantSpace: sales.ID},
		{id: "owner-space", version: 1, wantSpace: work.ID},
		{id: "upload", version: 1, wantSpace: personal},
		{id: "chat-gone", version: 1, wantConversation: "chat-gone", wantSpace: personal},
		{id: "already-placed", version: 1, wantConversation: "chat-kept", wantSpace: "space-kept"},
	} {
		artifact, err := catalog.Get(ctx, scope, want.id, want.version)
		if err != nil {
			t.Fatalf("get %s@%d: %v", want.id, want.version, err)
		}
		if got := artifact.Provenance; got.ConversationID != want.wantConversation || got.SpaceID != want.wantSpace {
			t.Fatalf("%s@%d placement = (%q, %q), want (%q, %q)", want.id, want.version, got.ConversationID, got.SpaceID, want.wantConversation, want.wantSpace)
		}
	}
	if _, err := spaces.GetSpace(ctx, scope, personal); err != nil {
		t.Fatalf("Personal space was not created: %v", err)
	}

	// A space filter now finds the old artifacts.
	page, err := catalog.List(ctx, ArtifactFilter{Scope: scope, SpaceID: personal, LatestOnly: true})
	if err != nil || len(page) != 2 {
		t.Fatalf("Personal artifacts = %d, err = %v", len(page), err)
	}
	page, err = catalog.List(ctx, ArtifactFilter{Scope: scope, SpaceID: work.ID, LatestOnly: true})
	if err != nil || len(page) != 2 {
		t.Fatalf("Work artifacts = %d, err = %v", len(page), err)
	}

	// Replaying an old registration places it the same way, so it still
	// matches the backfilled fingerprint.
	for id, mutate := range fixtures {
		replay := validCatalogArtifact(id, 1)
		mutate(replay)
		if result, err := catalog.Register(ctx, RegisterArtifactRequest{Artifact: replay}); err != nil || !result.Replayed {
			t.Fatalf("replay %s = %#v, %v", id, result, err)
		}
	}

	// Opening again changes nothing.
	before := artifactPayloads(t, store.db)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = reopen()
	if after := artifactPayloads(t, store.db); len(after) != len(before) {
		t.Fatalf("payload count changed: %d -> %d", len(before), len(after))
	} else {
		for key, payload := range before {
			if after[key] != payload {
				t.Fatalf("%s changed on reopen:\n%s\n%s", key, payload, after[key])
			}
		}
	}
}

func artifactPayloads(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT id, version, payload FROM artifacts`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := map[string]string{}
	for rows.Next() {
		var id, payload string
		var version int64
		if err := rows.Scan(&id, &version, &payload); err != nil {
			t.Fatal(err)
		}
		result[fmt.Sprintf("%s@%d", id, version)] = payload
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
