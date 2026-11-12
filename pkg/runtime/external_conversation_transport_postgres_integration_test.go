//go:build integration

package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/google/uuid"
)

func TestPostgresExternalConversationTransportIsConcurrentAndRestartSafe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	primary, pgSchema := openTestPostgresStore(t, "external_conversation_")
	dsn, schema := pgSchema.DSN, pgSchema.Name
	replica, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	var migration string
	if err := pgSchema.DB.QueryRowContext(ctx, `SELECT name FROM `+pgSchema.Table("schema_migrations")+`
		WHERE version=$1`, externalConversationTransportMigrationVersion).Scan(&migration); err != nil ||
		migration != "external conversation endpoints and transport" {
		t.Fatalf("migration = %q, %v", migration, err)
	}

	scope := models.Scope{Kind: "tenant", ID: "one"}
	// The endpoint the endpoint service creates for a bound Slack adapter.
	created := time.Now().UTC()
	endpoint := &models.ExternalConversationEndpoint{
		ID: "slack-channel", IngressRoute: uuid.NewString(), Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "slack-agent"},
		DeploymentID: "slack-agent", Name: "Slack channel", Provider: "slack",
		Adapter: models.ExternalConversationAdapterReference{
			SkillID: "slack", SkillVersion: "1.0.0", BindingID: "slack", BindingRevision: 1, AdapterID: "conversations",
		},
		Mode: capability.ConversationEndpointChannel, Address: "C012345",
		Handler: models.ExternalConversationHandler{Kind: models.ExternalConversationHandlerAgent, ID: "slack-agent"},
		Policy: models.ExternalConversationPolicy{
			MessageSelection: models.ExternalConversationSelectAllMessages,
			ReplyMode:        models.ExternalConversationReplyThread,
			IgnoreBots:       true,
		},
		Status: models.ExternalConversationEndpointActive, Revision: 1, CreatedAt: created, UpdatedAt: created,
	}
	if err := primary.CreateExternalConversationEndpoint(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	inbox := &models.ExternalConversationInboxItem{
		ID: "inbox-1", Scope: scope, EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Event: models.NormalizedExternalConversationEvent{
			ID: "provider/event/1", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "C/one", ExternalThreadID: "171.001",
			ExternalMessageID: "M/one", ExternalParticipantID: "U/one", Text: "hello",
			OrderingKey: "C/one:171.001", OccurredAt: now,
		},
		Status: models.ExternalConversationInboxPending, MaximumAttempts: 8, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if _, _, err := primary.ReceiveExternalConversationEvent(ctx, inbox); err != nil {
		t.Fatal(err)
	}
	delivery := &models.ExternalConversationDelivery{
		ID: "delivery-1", Scope: scope, EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Operation: capability.ConversationDeliveryMessageSend, ConversationID: "conversation-1", ChannelMessageID: "message-out",
		ExternalThreadID: "171.001", OrderingKey: "order-1", IdempotencyKey: "reply:message-out",
		Correlation: &models.ExternalConversationDeliveryCorrelation{Kind: "approval", ID: "approval-1", Phase: "request"},
		Status:      models.ExternalConversationDeliveryPending, MaximumAttempts: 5, AvailableAt: now,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if _, _, err := primary.EnqueueExternalConversationDelivery(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	if correlated, err := replica.ListExternalConversationDeliveries(ctx, models.ExternalConversationDeliveryFilter{Scope: scope, CorrelationKind: "approval", CorrelationID: "approval-1", Limit: 10}); err != nil || len(correlated) != 1 {
		t.Fatalf("correlated deliveries = %#v, %v", correlated, err)
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
	applied := models.CloneExternalConversationInboxItem(reclaimedInbox)
	applied.Status, applied.LeaseOwner, applied.LeaseExpiresAt = models.ExternalConversationInboxApplied, "", time.Time{}
	applied.ConversationID, applied.ChannelMessageID = "conversation-1", "message-in"
	applied.AppliedAt, applied.UpdatedAt = now.Add(61*time.Second), now.Add(61*time.Second)
	applied.Revision++
	if err := replica.SaveExternalConversationInbox(ctx, applied, reclaimedInbox.Revision, "restart-worker"); err != nil {
		t.Fatal(err)
	}
	delivered := models.CloneExternalConversationDelivery(reclaimedDelivery)
	delivered.Status, delivered.LeaseOwner, delivered.LeaseExpiresAt = models.ExternalConversationDeliveryDelivered, "", time.Time{}
	delivered.ProviderMessageID, delivered.DeliveredAt, delivered.UpdatedAt = "provider/message/1", now.Add(61*time.Second), now.Add(61*time.Second)
	delivered.Revision++
	if err := replica.SaveExternalConversationDelivery(ctx, delivered, reclaimedDelivery.Revision, "restart-worker"); err != nil {
		t.Fatal(err)
	}
}
