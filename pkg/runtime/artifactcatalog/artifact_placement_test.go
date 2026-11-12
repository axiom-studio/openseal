package artifactcatalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

type placementRuns map[string]*models.AgentRun

func (r placementRuns) GetAgentRun(_ context.Context, _ models.Scope, runID string) (*models.AgentRun, error) {
	if run := r[runID]; run != nil {
		return run, nil
	}
	return nil, models.ErrRunNotFound
}

type placementConversations struct {
	byID map[string]*models.Conversation
	err  error
}

func (c placementConversations) GetConversation(_ context.Context, _ models.Scope, conversationID string) (*models.Conversation, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.byID[conversationID], nil
}

func TestArtifactCatalogRecordsTheChatAndSpaceAnArtifactWasMadeIn(t *testing.T) {
	ctx := context.Background()
	runs := placementRuns{
		"run-chat":     {ID: "run-chat", RootRunID: "run-chat", Context: map[string]interface{}{"conversationId": "chat-sales"}},
		"run-delegate": {ID: "run-delegate", RootRunID: "run-chat"},
		"run-schedule": {ID: "run-schedule", RootRunID: "run-schedule"},
	}
	conversations := placementConversations{byID: map[string]*models.Conversation{
		"chat-sales":  {ID: "chat-sales", SpaceID: "space-sales"},
		"chat-design": {ID: "chat-design", SpaceID: "space-design"},
	}}
	catalog := artifactcatalog.NewCatalog(kerneltest.NewStore(t)).WithPlacement(artifactcatalog.ArtifactPlacement{Runs: runs, Conversations: conversations})
	register := func(id string, mutate func(*models.Artifact)) models.ArtifactProvenance {
		t.Helper()
		artifact := validCatalogArtifact(id, 1)
		mutate(artifact)
		result, err := catalog.Register(ctx, artifactcatalog.RegisterArtifactRequest{Artifact: artifact})
		if err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
		return result.Artifact.Provenance
	}

	tests := []struct {
		name             string
		mutate           func(*models.Artifact)
		wantConversation string
		wantSpace        string
	}{
		{name: "chat run", mutate: func(a *models.Artifact) { a.Provenance.RunID = "run-chat" }, wantConversation: "chat-sales", wantSpace: "space-sales"},
		{name: "delegated run uses its root chat", mutate: func(a *models.Artifact) { a.Provenance.RunID = "run-delegate" }, wantConversation: "chat-sales", wantSpace: "space-sales"},
		{name: "app keeps its chat in metadata", mutate: func(a *models.Artifact) {
			a.Provenance.RunID = ""
			a.MetadataSchema = nil
			a.Metadata = map[string]interface{}{"conversationId": "chat-design"}
		}, wantConversation: "chat-design", wantSpace: "space-design"},
		{name: "explicit chat is kept", mutate: func(a *models.Artifact) {
			a.Provenance.RunID = "run-chat"
			a.Provenance.ConversationID = " chat-design "
		}, wantConversation: "chat-design", wantSpace: "space-design"},
		{name: "run outside a chat", mutate: func(a *models.Artifact) { a.Provenance.RunID = "run-schedule" }},
		{name: "run that no longer exists", mutate: func(a *models.Artifact) { a.Provenance.RunID = "run-gone" }},
		{name: "chat that no longer exists", mutate: func(a *models.Artifact) {
			a.Provenance.RunID = ""
			a.Provenance.ConversationID = "chat-gone"
		}, wantConversation: "chat-gone"},
	}
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provenance := register(fmt.Sprintf("placed-%d", i), test.mutate)
			if provenance.ConversationID != test.wantConversation || provenance.SpaceID != test.wantSpace {
				t.Fatalf("placement = (%q, %q), want (%q, %q)", provenance.ConversationID, provenance.SpaceID, test.wantConversation, test.wantSpace)
			}
		})
	}

	stored, err := catalog.Get(ctx, models.Scope{Kind: "tenant", ID: "one"}, "placed-0", 1)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"conversationId":"chat-sales"`) || !strings.Contains(string(encoded), `"spaceId":"space-sales"`) {
		t.Fatalf("artifact JSON does not expose its placement: %s", encoded)
	}
	// A replay resolves the same placement, so it matches the stored fingerprint.
	replay := validCatalogArtifact("placed-0", 1)
	replay.Provenance.RunID = "run-chat"
	if result, err := catalog.Register(ctx, artifactcatalog.RegisterArtifactRequest{Artifact: replay}); err != nil || !result.Replayed {
		t.Fatalf("replay = %#v, %v", result, err)
	}
}

func TestArtifactPlacementFailsOnConversationLookupErrors(t *testing.T) {
	lookupErr := errors.New("conversation store unavailable")
	catalog := artifactcatalog.NewCatalog(kerneltest.NewStore(t)).WithPlacement(artifactcatalog.ArtifactPlacement{
		Runs:          placementRuns{"run-chat": {ID: "run-chat", Context: map[string]interface{}{"conversationId": "chat-sales"}}},
		Conversations: placementConversations{err: lookupErr},
	})
	artifact := validCatalogArtifact("placement-error", 1)
	artifact.Provenance.RunID = "run-chat"
	if _, err := catalog.Register(context.Background(), artifactcatalog.RegisterArtifactRequest{Artifact: artifact}); !errors.Is(err, lookupErr) {
		t.Fatalf("error = %v", err)
	}
}

func TestArtifactCatalogFiltersBySpaceAndChatWithStablePages(t *testing.T) {
	ctx := context.Background()
	sqlite, err := sqlite.New(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlite.Close()
	stamp := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	clock := models.WithClock(func() time.Time { return stamp })
	catalogs := []*artifactcatalog.Catalog{artifactcatalog.NewCatalog(kerneltest.NewStore(t), clock), artifactcatalog.NewCatalog(sqlite, clock)}
	for _, catalog := range catalogs {
		for i := 0; i < 90; i++ {
			artifact := validCatalogArtifact(fmt.Sprintf("asset-%03d", i), 1)
			// Every third artifact predates placement and has neither field.
			switch i % 3 {
			case 1:
				artifact.Provenance.ConversationID, artifact.Provenance.SpaceID = fmt.Sprintf("chat-sales-%d", i%2), "space-sales"
			case 2:
				artifact.Provenance.ConversationID, artifact.Provenance.SpaceID = "chat-design", "space-design"
			}
			if _, err := catalog.Register(ctx, artifactcatalog.RegisterArtifactRequest{Artifact: artifact}); err != nil {
				t.Fatal(err)
			}
		}
		// The latest version of asset-001 moved out of Sales, so a latest-only
		// space page must not return its older Sales version.
		moved := validCatalogArtifact("asset-001", 2)
		moved.ContentRef, moved.Digest = "object-store:asset-001-v2", digestFor("asset-001-v2")
		moved.Provenance.ConversationID, moved.Provenance.SpaceID = "chat-design", "space-design"
		if _, err := catalog.Register(ctx, artifactcatalog.RegisterArtifactRequest{Artifact: moved, ExpectedLatestVersion: 1}); err != nil {
			t.Fatal(err)
		}
	}
	scope := models.Scope{Kind: "tenant", ID: "one"}
	listAll := func(catalog *artifactcatalog.Catalog, filter models.ArtifactFilter) []string {
		t.Helper()
		var ids []string
		for filter.Offset = 0; ; filter.Offset += filter.Limit {
			page, err := catalog.List(ctx, filter)
			if err != nil {
				t.Fatal(err)
			}
			for _, artifact := range page {
				ids = append(ids, fmt.Sprintf("%s:%d", artifact.ID, artifact.Version))
			}
			if len(page) < filter.Limit {
				return ids
			}
		}
	}
	tests := []struct {
		filter models.ArtifactFilter
		want   int
	}{
		{filter: models.ArtifactFilter{Scope: scope, LatestOnly: true, Limit: 7}, want: 90},
		{filter: models.ArtifactFilter{Scope: scope, SpaceID: " space-sales ", LatestOnly: true, Limit: 7}, want: 29},
		{filter: models.ArtifactFilter{Scope: scope, SpaceID: "space-design", LatestOnly: true, Limit: 7}, want: 31},
		{filter: models.ArtifactFilter{Scope: scope, SpaceID: "space-sales", Limit: 7}, want: 30},
		{filter: models.ArtifactFilter{Scope: scope, ConversationID: "chat-sales-0", SpaceID: "space-sales", LatestOnly: true, Limit: 4}, want: 15},
		{filter: models.ArtifactFilter{Scope: scope, SpaceID: "space-unknown", LatestOnly: true, Limit: 7}, want: 0},
	}
	for _, test := range tests {
		pages := make([][]string, len(catalogs))
		for i, catalog := range catalogs {
			pages[i] = listAll(catalog, test.filter)
		}
		if len(pages[1]) != test.want {
			t.Fatalf("filter %+v: got %d artifacts, want %d", test.filter, len(pages[1]), test.want)
		}
		if !reflect.DeepEqual(pages[0], pages[1]) {
			t.Fatalf("filter %+v: memory %v, sqlite %v", test.filter, pages[0], pages[1])
		}
	}
}

func TestArtifactRejectsInvalidPlacementIdentifiers(t *testing.T) {
	catalog := artifactcatalog.NewCatalog(kerneltest.NewStore(t))
	for name, mutate := range map[string]func(*models.Artifact){
		"conversation": func(a *models.Artifact) { a.Provenance.ConversationID = "chats/sales" },
		"space":        func(a *models.Artifact) { a.Provenance.SpaceID = strings.Repeat("s", 129) },
	} {
		artifact := validCatalogArtifact("bad-"+name, 1)
		mutate(artifact)
		if _, err := catalog.Register(context.Background(), artifactcatalog.RegisterArtifactRequest{Artifact: artifact}); !errors.Is(err, models.ErrInvalidArtifactRecord) {
			t.Fatalf("%s: error = %v", name, err)
		}
	}
}

func TestKernelStoresProvideArtifactPlacementLookups(t *testing.T) {
	sqlite, err := sqlite.New(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlite.Close()
	for name, store := range map[string]any{"sqlite": sqlite, "postgres": &postgres.Store{}} {
		if placement := artifactcatalog.NewStoreArtifactPlacement(store); placement.Runs == nil || placement.Conversations == nil {
			t.Fatalf("%s store placement = %#v", name, placement)
		}
	}
}
