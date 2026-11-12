//go:build integration

package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func TestPostgresWorkspaceSearchFindsHistoryAndIsolatesTenants(t *testing.T) {
	pgSchema := storetest.NewPostgresSchema(t, "search_")
	dsn := pgSchema.DSN
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := postgres.New(ctx, dsn, postgres.WithSchema(pgSchema.Name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	service := runtimeconversation.NewConversationService(store)
	for _, tenant := range []string{"one", "two"} {
		scope := models.Scope{Kind: "tenant", ID: tenant}
		seedTestSpace(t, store, scope)
		conversation, _, err := service.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, Title: "Quorum planning", IdempotencyKey: "plan"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = service.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "operator"}, Intent: models.MessageIntentQuestion, Content: "Decide the nebula rollout", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: "question"})
		if err != nil {
			t.Fatal(err)
		}
	}
	hits, err := store.SearchWorkspacePage(ctx, models.Scope{Kind: "tenant", ID: "one"}, "nebula rollout", 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Kind != "message" || hits[0].Message.Content != "Decide the nebula rollout" || hits[0].Conversation.Scope.ID != "one" {
		t.Fatalf("search hits = %#v", hits)
	}
	other, err := store.SearchWorkspacePage(ctx, models.Scope{Kind: "tenant", ID: "two"}, "Quorum", 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || other[0].Kind != "chat" || other[0].Conversation.Scope.ID != "two" {
		t.Fatalf("other tenant hits = %#v", other)
	}
	if _, err := pgSchema.DB.ExecContext(ctx, `INSERT INTO `+pgSchema.Table("artifacts")+` (scope_kind,scope_id,id,version,type,media_type,classification,digest,content_ref,created_at,payload) VALUES ('tenant','one','artifact-1',1,'report','text/plain','internal','digest','object',now(),'{"id":"artifact-1","name":"Nebula report","provenance":{"owner":{"type":"agent","id":"agent"}}}'::jsonb)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pgSchema.DB.ExecContext(ctx, `INSERT INTO `+pgSchema.Table("agent_runs")+` (id,scope_kind,scope_id,root_run_id,status,revision,available_at,queue_entered_at,created_at,payload) VALUES ('run-1','tenant','one','run-1','completed',1,now(),now(),now(),'{"id":"run-1","goal":"Publish status","owner":{"type":"agent","id":"agent"},"output":{"summary":"Nebula release approved"},"updatedAt":"2026-09-29T00:00:00Z"}'::jsonb)`); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ query, kind string }{{"Nebula report", "artifact"}, {"release approved", "run"}} {
		matches, err := store.SearchWorkspacePage(ctx, models.Scope{Kind: "tenant", ID: "one"}, test.query, 20, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, match := range matches {
			if match.Kind == test.kind {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s search did not find %s: %#v", test.query, test.kind, matches)
		}
	}
	if _, err := pgSchema.DB.ExecContext(ctx, `UPDATE `+pgSchema.Table("conversations")+` SET status='archived' WHERE scope_kind='tenant' AND scope_id='one'`); err != nil {
		t.Fatal(err)
	}
	archived, err := store.SearchWorkspacePage(ctx, models.Scope{Kind: "tenant", ID: "one"}, "nebula", 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range archived {
		if hit.Kind == "chat" || hit.Kind == "message" {
			t.Fatalf("archived chat leaked into search: %#v", hit)
		}
	}
	// Simulate an existing version 45 installation and verify the versioned
	// upgrade builds every search index for already-persisted records.
	for _, name := range []string{"conversations_search_idx", "channel_messages_search_idx", "artifacts_search_idx", "agent_runs_search_idx"} {
		if _, err := pgSchema.DB.ExecContext(ctx, `DROP INDEX `+pgSchema.Table(name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pgSchema.DB.ExecContext(ctx, `DELETE FROM `+pgSchema.Table("schema_migrations")+` WHERE version=46`); err != nil {
		t.Fatal(err)
	}
	// A store opened on the schema runs the upgrade.
	kerneltest.OpenPostgresOn(t, pgSchema)
	var indexCount int
	if err := pgSchema.DB.QueryRowContext(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname=$1 AND indexname IN ('conversations_search_idx','channel_messages_search_idx','artifacts_search_idx','agent_runs_search_idx')`, pgSchema.Name).Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if indexCount != 4 {
		t.Fatalf("recreated workspace indexes = %d, want 4", indexCount)
	}
}
