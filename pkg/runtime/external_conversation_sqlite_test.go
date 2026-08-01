package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSQLiteExternalConversationTransportRecoversAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "external-conversation.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "one"}
	definition := slackConversationSkillDefinition()
	catalog := skill.NewCatalogWithStore(store)
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
	endpoint, err := NewExternalConversationEndpointService(store, catalog).Create(ctx, CreateExternalConversationEndpointRequest{
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
	if _, replayed, err := store.ReceiveExternalConversationEvent(ctx, inbox); err != nil || replayed {
		t.Fatalf("receive replayed=%v, %v", replayed, err)
	}
	delivery := &ExternalConversationDelivery{
		ID: "delivery-1", Scope: scope, EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Operation: capability.ConversationDeliveryMessageSend, ConversationID: "conversation-1", ChannelMessageID: "message-out",
		ExternalThreadID: "171.001", OrderingKey: "order-1", IdempotencyKey: "reply:message-out",
		Correlation: &ExternalConversationDeliveryCorrelation{Kind: "approval", ID: "approval-1", Phase: "request"},
		Status:      ExternalConversationDeliveryPending, MaximumAttempts: 5, AvailableAt: now,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if _, replayed, err := store.EnqueueExternalConversationDelivery(ctx, delivery); err != nil || replayed {
		t.Fatalf("enqueue replayed=%v, %v", replayed, err)
	}
	if correlated, err := store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{Scope: scope, CorrelationKind: "approval", CorrelationID: "approval-1", Limit: 10}); err != nil || len(correlated) != 1 {
		t.Fatalf("correlated deliveries = %#v, %v", correlated, err)
	}
	firstInboxLease, err := store.ClaimExternalConversationInbox(ctx, scope, "worker-before-restart", now, time.Minute)
	if err != nil || firstInboxLease == nil {
		t.Fatalf("inbox lease = %#v, %v", firstInboxLease, err)
	}
	firstDeliveryLease, err := store.ClaimExternalConversationDelivery(ctx, scope, "worker-before-restart", now, time.Minute)
	if err != nil || firstDeliveryLease == nil {
		t.Fatalf("delivery lease = %#v, %v", firstDeliveryLease, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restartedCatalog := skill.NewCatalogWithStore(restarted)
	if resolved, err := NewExternalConversationEndpointService(restarted, restartedCatalog).Get(ctx, scope, endpoint.ID); err != nil || resolved.Provider != "slack" {
		t.Fatalf("restarted endpoint = %#v, %v", resolved, err)
	}
	replay := cloneExternalConversationInboxItem(inbox)
	replay.ID = "another-local-id"
	if existing, replayed, err := restarted.ReceiveExternalConversationEvent(ctx, replay); err != nil || !replayed || existing.ID != inbox.ID {
		t.Fatalf("restarted inbox replay = %#v, replayed=%v, %v", existing, replayed, err)
	}
	reclaimedInbox, err := restarted.ClaimExternalConversationInbox(ctx, scope, "worker-after-restart", now.Add(time.Minute), time.Minute)
	if err != nil || reclaimedInbox == nil || reclaimedInbox.Attempt != 2 {
		t.Fatalf("reclaimed inbox = %#v, %v", reclaimedInbox, err)
	}
	reclaimedDelivery, err := restarted.ClaimExternalConversationDelivery(ctx, scope, "worker-after-restart", now.Add(time.Minute), time.Minute)
	if err != nil || reclaimedDelivery == nil || reclaimedDelivery.Attempt != 2 {
		t.Fatalf("reclaimed delivery = %#v, %v", reclaimedDelivery, err)
	}

	applied := cloneExternalConversationInboxItem(reclaimedInbox)
	applied.Status, applied.LeaseOwner, applied.LeaseExpiresAt = ExternalConversationInboxApplied, "", time.Time{}
	applied.ConversationID, applied.ChannelMessageID, applied.RunID = "conversation-1", "message-in", "run-1"
	applied.AppliedAt, applied.UpdatedAt = now.Add(61*time.Second), now.Add(61*time.Second)
	applied.Revision++
	if err := restarted.SaveExternalConversationInbox(ctx, applied, reclaimedInbox.Revision, "worker-after-restart"); err != nil {
		t.Fatal(err)
	}
	delivered := cloneExternalConversationDelivery(reclaimedDelivery)
	delivered.Status, delivered.LeaseOwner, delivered.LeaseExpiresAt = ExternalConversationDeliveryDelivered, "", time.Time{}
	delivered.ProviderMessageID, delivered.DeliveredAt, delivered.UpdatedAt = "provider/message/1", now.Add(61*time.Second), now.Add(61*time.Second)
	delivered.Revision++
	if err := restarted.SaveExternalConversationDelivery(ctx, delivered, reclaimedDelivery.Revision, "worker-after-restart"); err != nil {
		t.Fatal(err)
	}
	for _, save := range []func() error{
		func() error {
			return restarted.SaveExternalConversationMapping(ctx, &ExternalConversationMapping{
				Scope: scope, EndpointID: endpoint.ID, ExternalConversationID: "C/one", ExternalThreadID: "171.001",
				ConversationID: "conversation-1", ThreadRootMessageID: "message-in",
				Revision: 1, CreatedAt: now, UpdatedAt: now,
			}, 0)
		},
		func() error {
			return restarted.SaveExternalParticipantMapping(ctx, &ExternalParticipantMapping{
				Scope: scope, EndpointID: endpoint.ID, ExternalParticipantID: "U/one",
				Participant: ConversationParticipant{Type: ConversationParticipantUser, ID: "external-user-1"},
				Revision:    1, CreatedAt: now, UpdatedAt: now,
			}, 0)
		},
		func() error {
			return restarted.SaveExternalMessageMapping(ctx, &ExternalMessageMapping{
				Scope: scope, EndpointID: endpoint.ID, Direction: ExternalMessageInbound, ExternalMessageID: "M/one",
				ConversationID: "conversation-1", ChannelMessageID: "message-in",
				Revision: 1, CreatedAt: now, UpdatedAt: now,
			}, 0)
		},
	} {
		if err := save(); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := restarted.GetExternalConversationInboxItem(ctx, scope, inbox.ID); got == nil || got.Status != ExternalConversationInboxApplied {
		t.Fatalf("durable inbox = %#v", got)
	}
	if got, _ := restarted.GetExternalConversationDelivery(ctx, scope, delivery.ID); got == nil || got.Status != ExternalConversationDeliveryDelivered {
		t.Fatalf("durable delivery = %#v", got)
	}
	if got, _ := restarted.GetExternalMessageMapping(ctx, scope, endpoint.ID, ExternalMessageInbound, "M/one"); got == nil || got.ChannelMessageID != "message-in" {
		t.Fatalf("durable message mapping = %#v", got)
	}
}

func TestSQLiteExternalConversationDeliveryRebindsAfterEndpointUpgrade(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "delivery-rebind.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Millisecond)
	scope := Scope{Kind: "tenant", ID: "one"}
	adapter := ExternalConversationAdapterReference{
		SkillID: "skill-slack", SkillVersion: "1.0.0", BindingID: "slack-binding", BindingRevision: 1, AdapterID: "conversations",
	}
	endpoint := &ExternalConversationEndpoint{
		ID: "slack-endpoint", IngressRoute: "opaque-route", Scope: scope,
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "slack-agent"}, DeploymentID: "slack-agent",
		Name: "Slack approvals", Adapter: adapter, Provider: "slack", Mode: capability.ConversationEndpointChannel,
		Address: "C012345", Handler: ExternalConversationHandler{Kind: ExternalConversationHandlerAgent, ID: "slack-agent"},
		Policy: ExternalConversationPolicy{MessageSelection: ExternalConversationSelectAllMessages, ReplyMode: ExternalConversationReplyChannel, IgnoreBots: true},
		Status: ExternalConversationEndpointActive, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateExternalConversationEndpoint(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	delivery := &ExternalConversationDelivery{
		ID: "delivery-rebind", Scope: scope, EndpointID: endpoint.ID, EndpointRevision: 1, Adapter: adapter,
		Operation: capability.ConversationDeliveryMessageSend, ConversationID: "conversation-1", ChannelMessageID: "message-1",
		OrderingKey: "order-1", Parameters: map[string]interface{}{"approval": map[string]interface{}{"revision": int64(1)}},
		IdempotencyKey: "approval:one", Status: ExternalConversationDeliveryRetry,
		Attempt: 1, MaximumAttempts: 8, AvailableAt: now.Add(time.Minute), ErrorCode: "endpoint_or_adapter_conflict",
		Summary: "The exact adapter changed.", Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if _, replayed, err := store.EnqueueExternalConversationDelivery(ctx, delivery); err != nil || replayed {
		t.Fatalf("enqueue = replayed %v, %v", replayed, err)
	}
	upgraded := cloneExternalConversationEndpoint(endpoint)
	upgraded.Adapter.SkillVersion, upgraded.Adapter.BindingRevision = "1.0.1", 2
	upgraded.Revision, upgraded.UpdatedAt = 2, now.Add(time.Second)
	if err := store.UpdateExternalConversationEndpoint(ctx, upgraded, 1); err != nil {
		t.Fatal(err)
	}
	replay := cloneExternalConversationDelivery(delivery)
	replay.EndpointRevision, replay.Adapter = 2, upgraded.Adapter
	replay.Status, replay.AvailableAt, replay.UpdatedAt = ExternalConversationDeliveryPending, now.Add(2*time.Second), now.Add(2*time.Second)
	replay.ErrorCode, replay.Summary = "", ""
	rebound, replayed, err := store.EnqueueExternalConversationDelivery(ctx, replay)
	if err != nil || !replayed || rebound.ID != delivery.ID || rebound.EndpointRevision != 2 ||
		rebound.Adapter != upgraded.Adapter || rebound.Status != ExternalConversationDeliveryPending || rebound.Attempt != 1 || rebound.Revision != 2 {
		t.Fatalf("rebind = %#v, replayed %v, %v", rebound, replayed, err)
	}
}

func TestSQLiteExternalConversationClaimsPreserveOrderingPartitions(t *testing.T) {
	ctx := context.Background()
	_, endpoint := activeExternalConversationTestEndpoint(t, ctx)
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "ordering.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.CreateExternalConversationEndpoint(ctx, endpoint); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	for index, order := range []string{"same", "same", "independent"} {
		item := &ExternalConversationInboxItem{
			ID: "inbox-order-" + order + string(rune('a'+index)), Scope: endpoint.Scope,
			EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
			Event: NormalizedExternalConversationEvent{
				ID: "event-" + order + string(rune('a'+index)), Type: capability.ConversationEventMessageReceived,
				ExternalConversationID: "channel", ExternalMessageID: "message-" + order + string(rune('a'+index)),
				ExternalParticipantID: "user", Text: "hello", OrderingKey: order, OccurredAt: now,
			},
			Status: ExternalConversationInboxPending, MaximumAttempts: 3, AvailableAt: now,
			Revision: 1, CreatedAt: now.Add(time.Duration(index) * time.Millisecond),
			UpdatedAt: now.Add(time.Duration(index) * time.Millisecond),
		}
		if _, _, err := store.ReceiveExternalConversationEvent(ctx, item); err != nil {
			t.Fatal(err)
		}
		delivery := &ExternalConversationDelivery{
			ID: "delivery-" + order + string(rune('a'+index)), Scope: endpoint.Scope,
			EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
			Operation: capability.ConversationDeliveryMessageSend, ConversationID: "conversation",
			ChannelMessageID: "out-" + order + string(rune('a'+index)), OrderingKey: order,
			IdempotencyKey: "key-" + order + string(rune('a'+index)),
			Status:         ExternalConversationDeliveryPending, MaximumAttempts: 3, AvailableAt: now,
			Revision: 1, CreatedAt: now.Add(time.Duration(index) * time.Millisecond),
			UpdatedAt: now.Add(time.Duration(index) * time.Millisecond),
		}
		if _, _, err := store.EnqueueExternalConversationDelivery(ctx, delivery); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.ClaimExternalConversationInbox(ctx, endpoint.Scope, "inbox-one", now.Add(time.Second), time.Minute)
	second, secondErr := store.ClaimExternalConversationInbox(ctx, endpoint.Scope, "inbox-two", now.Add(time.Second), time.Minute)
	blocked, blockedErr := store.ClaimExternalConversationInbox(ctx, endpoint.Scope, "inbox-three", now.Add(time.Second), time.Minute)
	if err != nil || secondErr != nil || blockedErr != nil || first == nil || first.Event.OrderingKey != "same" ||
		second == nil || second.Event.OrderingKey != "independent" || blocked != nil {
		t.Fatalf("SQLite inbox ordering: first=%#v second=%#v blocked=%#v errors=%v/%v/%v",
			first, second, blocked, err, secondErr, blockedErr)
	}
	firstDelivery, err := store.ClaimExternalConversationDelivery(ctx, endpoint.Scope, "delivery-one", now.Add(time.Second), time.Minute)
	secondDelivery, secondErr := store.ClaimExternalConversationDelivery(ctx, endpoint.Scope, "delivery-two", now.Add(time.Second), time.Minute)
	blockedDelivery, blockedErr := store.ClaimExternalConversationDelivery(ctx, endpoint.Scope, "delivery-three", now.Add(time.Second), time.Minute)
	if err != nil || secondErr != nil || blockedErr != nil || firstDelivery == nil || firstDelivery.OrderingKey != "same" ||
		secondDelivery == nil || secondDelivery.OrderingKey != "independent" || blockedDelivery != nil {
		t.Fatalf("SQLite delivery ordering: first=%#v second=%#v blocked=%#v errors=%v/%v/%v",
			firstDelivery, secondDelivery, blockedDelivery, err, secondErr, blockedErr)
	}
}
