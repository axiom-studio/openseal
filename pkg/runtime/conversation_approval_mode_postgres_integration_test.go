//go:build integration

package runtime

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgresApprovalModeMigrationAndGovernedUpdate(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	schema := "openseal_approval_mode_" + uuid.NewString()[:8]
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
		_ = store.Close()
	})
	scope := Scope{Kind: "tenant", ID: "one"}
	conversation, _, err := NewConversationService(store).CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "assistant"}, Title: "Chat", IdempotencyKey: "chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a conversation stored before migration 61 and re-run it.
	if err := store.RollbackPostgresMigrations(ctx, conversationApprovalModeMigrationVersion-1); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := store.db.QueryRowContext(ctx, `SELECT COALESCE(payload->>'approvalMode', '') FROM `+store.table("conversations")).Scan(&raw); err != nil || raw != "" {
		t.Fatalf("rolled back approval mode = %q, %v", raw, err)
	}
	reopened, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if version, err := reopened.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	if err := reopened.db.QueryRowContext(ctx, `SELECT payload->>'approvalMode' FROM `+reopened.table("conversations")).Scan(&raw); err != nil || raw != "auto" {
		t.Fatalf("migrated approval mode = %q, %v", raw, err)
	}
	service := NewConversationApprovalModeService(reopened)
	result, err := service.SetApprovalMode(ctx, SetConversationApprovalModeRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Mode: ConversationApprovalManual, Actor: approvalModeActor,
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := reopened.GetConversation(ctx, scope, conversation.ID)
	if err != nil || stored.ApprovalMode != ConversationApprovalManual || stored.Revision != conversation.Revision+1 {
		t.Fatalf("stored conversation = %#v, %v", stored, err)
	}
	events, err := reopened.ListActivity(ctx, ActivityFilter{Scope: scope, AgentID: "assistant", EventTypes: []string{ConversationApprovalModeChangedEvent}, Descending: true})
	if err != nil || len(events) != 1 || events[0].ID != result.Event.ID || events[0].Payload["from"] != "auto" {
		t.Fatalf("approval mode events = %#v, %v", events, err)
	}
	if _, err := service.SetApprovalMode(ctx, SetConversationApprovalModeRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Mode: ConversationApprovalSkip, Actor: approvalModeActor,
	}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale revision error = %v", err)
	}
}
