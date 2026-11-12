//go:build integration

package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
)

func TestPostgresApprovalModeMigrationAndGovernedUpdate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store, pgSchema := openTestPostgresStore(t, "openseal_approval_mode_")
	schema := pgSchema.Name
	dsn := pgSchema.DSN
	scope := Scope{Kind: "tenant", ID: "one"}
	seedTestSpace(t, store, scope)
	conversation, _, err := runtimeconversation.NewConversationService(store).CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "assistant"}, Title: "Chat", IdempotencyKey: "chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a conversation stored before the approval mode migration (61)
	// and re-run it. The ledger names the migration.
	var approvalModeMigration int64
	if err := pgSchema.DB.QueryRowContext(ctx, `SELECT version FROM `+pgSchema.Table("schema_migrations")+` WHERE name = 'conversation approval mode'`).Scan(&approvalModeMigration); err != nil {
		t.Fatal(err)
	}
	if err := store.RollbackPostgresMigrations(ctx, approvalModeMigration-1); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := pgSchema.DB.QueryRowContext(ctx, `SELECT COALESCE(payload->>'approvalMode', '') FROM `+pgSchema.Table("conversations")).Scan(&raw); err != nil || raw != "" {
		t.Fatalf("rolled back approval mode = %q, %v", raw, err)
	}
	reopened, err := postgres.NewPostgresStore(ctx, dsn, postgres.WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if version, err := reopened.PostgresSchemaVersion(ctx); err != nil || version != postgresSchemaHead(t) {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	if err := pgSchema.DB.QueryRowContext(ctx, `SELECT payload->>'approvalMode' FROM `+pgSchema.Table("conversations")).Scan(&raw); err != nil || raw != "auto" {
		t.Fatalf("migrated approval mode = %q, %v", raw, err)
	}
	service := runtimeconversation.NewConversationApprovalModeService(reopened)
	result, err := service.SetApprovalMode(ctx, runtimeconversation.SetConversationApprovalModeRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Mode: ConversationApprovalManual, Actor: ApprovalModeActor,
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := reopened.GetConversation(ctx, scope, conversation.ID)
	if err != nil || stored.ApprovalMode != ConversationApprovalManual || stored.Revision != conversation.Revision+1 {
		t.Fatalf("stored conversation = %#v, %v", stored, err)
	}
	events, err := reopened.ListActivity(ctx, ActivityFilter{Scope: scope, AgentID: "assistant", EventTypes: []string{runtimeconversation.ConversationApprovalModeChangedEvent}, Descending: true})
	if err != nil || len(events) != 1 || events[0].ID != result.Event.ID || events[0].Payload["from"] != "auto" {
		t.Fatalf("approval mode events = %#v, %v", events, err)
	}
	changes, err := runtimeconversation.NewConversationChangeService(reopened, reopened)
	if err != nil {
		t.Fatal(err)
	}
	set, err := changes.ListChanges(ctx, runtimeconversation.ConversationChangeRequest{Scope: scope, ConversationID: conversation.ID})
	if err != nil || len(set.Activity) != 1 || set.Activity[0].Payload["to"] != "manual" || set.Conversation.ApprovalMode != ConversationApprovalManual {
		t.Fatalf("change set = %#v, %v", set, err)
	}
	if _, err := service.SetApprovalMode(ctx, runtimeconversation.SetConversationApprovalModeRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Mode: ConversationApprovalSkip, Actor: ApprovalModeActor,
	}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale revision error = %v", err)
	}
}
