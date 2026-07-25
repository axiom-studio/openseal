//go:build integration

package runtime

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

func TestPostgresExternalConversationTransportIsConcurrentAndRestartSafe(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	schema := "external_conversation_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	primary, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_ = primary.Close()
	})
	replica, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	var migration string
	if err := primary.db.QueryRowContext(ctx, `SELECT name FROM `+primary.table("schema_migrations")+`
		WHERE version=$1`, externalConversationTransportMigrationVersion).Scan(&migration); err != nil ||
		migration != "external conversation endpoints and transport" {
		t.Fatalf("migration = %q, %v", migration, err)
	}

	scope := Scope{Kind: "tenant", ID: "one"}
	definition := slackConversationSkillDefinition()
	catalog := skill.NewCatalogWithStore(primary)
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	binding := &skill.Binding{
		ID: "slack", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "slack-agent",
		SkillID: definition.ID, SkillVersion: definition.Version,
		EnabledConversationAdapters: []string{"conversations"}, MaximumRisk: skill.RiskLevelRead,
		Credentials: map[string]skill.CredentialReference{
			"SLACK_CONNECTION": {Kind: "slack-oauth", ID: "connection://tenant/one/slack"},
		},
		Revision: 1,
	}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	adapter := ExternalConversationAdapterReference{
		SkillID: definition.ID, SkillVersion: definition.Version,
		BindingID: binding.ID, BindingRevision: binding.Revision, AdapterID: "conversations",
	}
	endpoint, err := NewExternalConversationEndpointService(primary, catalog).Create(ctx, CreateExternalConversationEndpointRequest{
		ID: "slack-channel", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "slack-agent"},
		DeploymentID: "slack-agent", Name: "Slack channel", Adapter: adapter,
		Mode: capability.ConversationEndpointChannel, Address: "C012345",
		Handler: ExternalConversationHandler{Kind: ExternalConversationHandlerAgent, ID: "slack-agent"},
		Policy: ExternalConversationPolicy{
			MessageSelection: ExternalConversationSelectAllMessages,
			ReplyMode:        ExternalConversationReplyThread,
			IgnoreBots:       true,
		},
		Status: ExternalConversationEndpointActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	inbox := &ExternalConversationInboxItem{
		ID: "inbox-1", Scope: scope, EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Event: NormalizedExternalConversationEvent{
			ID: "provider/event/1", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "C/one", ExternalThreadID: "171.001",
			ExternalMessageID: "M/one", ExternalParticipantID: "U/one", Text: "hello",
			OrderingKey: "C/one:171.001", OccurredAt: now,
		},
		Status: ExternalConversationInboxPending, MaximumAttempts: 8, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if _, _, err := primary.ReceiveExternalConversationEvent(ctx, inbox); err != nil {
		t.Fatal(err)
	}
	delivery := &ExternalConversationDelivery{
		ID: "delivery-1", Scope: scope, EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Operation: capability.ConversationDeliveryMessageSend, ConversationID: "conversation-1", ChannelMessageID: "message-out",
		ExternalThreadID: "171.001", OrderingKey: "order-1", IdempotencyKey: "reply:message-out",
		Status: ExternalConversationDeliveryPending, MaximumAttempts: 5, AvailableAt: now,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if _, _, err := primary.EnqueueExternalConversationDelivery(ctx, delivery); err != nil {
		t.Fatal(err)
	}

	var inboxClaims, deliveryClaims int
	var claimMu sync.Mutex
	var wait sync.WaitGroup
	for index, store := range []*PostgresStore{primary, replica} {
		wait.Add(1)
		go func(index int, store *PostgresStore) {
			defer wait.Done()
			worker := "worker-" + string(rune('a'+index))
			claimedInbox, claimErr := store.ClaimExternalConversationInbox(ctx, scope, worker, now, time.Minute)
			if claimErr != nil {
				t.Errorf("claim inbox: %v", claimErr)
			}
			claimedDelivery, claimErr := store.ClaimExternalConversationDelivery(ctx, scope, worker, now, time.Minute)
			if claimErr != nil {
				t.Errorf("claim delivery: %v", claimErr)
			}
			claimMu.Lock()
			if claimedInbox != nil {
				inboxClaims++
			}
			if claimedDelivery != nil {
				deliveryClaims++
			}
			claimMu.Unlock()
		}(index, store)
	}
	wait.Wait()
	if inboxClaims != 1 || deliveryClaims != 1 {
		t.Fatalf("concurrent claims inbox=%d delivery=%d", inboxClaims, deliveryClaims)
	}

	if err := replica.Close(); err != nil {
		t.Fatal(err)
	}
	replica, err = NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	reclaimedInbox, err := replica.ClaimExternalConversationInbox(ctx, scope, "restart-worker", now.Add(time.Minute), time.Minute)
	if err != nil || reclaimedInbox == nil || reclaimedInbox.Attempt != 2 {
		t.Fatalf("reclaimed inbox = %#v, %v", reclaimedInbox, err)
	}
	reclaimedDelivery, err := replica.ClaimExternalConversationDelivery(ctx, scope, "restart-worker", now.Add(time.Minute), time.Minute)
	if err != nil || reclaimedDelivery == nil || reclaimedDelivery.Attempt != 2 {
		t.Fatalf("reclaimed delivery = %#v, %v", reclaimedDelivery, err)
	}
	applied := cloneExternalConversationInboxItem(reclaimedInbox)
	applied.Status, applied.LeaseOwner, applied.LeaseExpiresAt = ExternalConversationInboxApplied, "", time.Time{}
	applied.ConversationID, applied.ChannelMessageID = "conversation-1", "message-in"
	applied.AppliedAt, applied.UpdatedAt = now.Add(61*time.Second), now.Add(61*time.Second)
	applied.Revision++
	if err := replica.SaveExternalConversationInbox(ctx, applied, reclaimedInbox.Revision, "restart-worker"); err != nil {
		t.Fatal(err)
	}
	delivered := cloneExternalConversationDelivery(reclaimedDelivery)
	delivered.Status, delivered.LeaseOwner, delivered.LeaseExpiresAt = ExternalConversationDeliveryDelivered, "", time.Time{}
	delivered.ProviderMessageID, delivered.DeliveredAt, delivered.UpdatedAt = "provider/message/1", now.Add(61*time.Second), now.Add(61*time.Second)
	delivered.Revision++
	if err := replica.SaveExternalConversationDelivery(ctx, delivered, reclaimedDelivery.Revision, "restart-worker"); err != nil {
		t.Fatal(err)
	}
}
