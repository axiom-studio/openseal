package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestExternalConversationEndpointPinsExactSkillOwnedAdapter(t *testing.T) {
	ctx := context.Background()
	store, catalog, scope, adapter := externalConversationTestCatalog(t, ctx)
	service := NewExternalConversationEndpointService(store, catalog)
	endpoint, err := service.Create(ctx, CreateExternalConversationEndpointRequest{
		ID: "slack-channel", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "slack-agent"},
		DeploymentID: "slack-agent", Name: "Customer channel", Adapter: adapter,
		Mode: capability.ConversationEndpointChannel, Address: "C012345",
		Handler: ExternalConversationHandler{Kind: ExternalConversationHandlerAgent, ID: "slack-agent"},
		Policy: ExternalConversationPolicy{
			MessageSelection: ExternalConversationSelectDirectOrMention,
			ReplyMode:        ExternalConversationReplyThread,
			IgnoreBots:       true,
		},
		Configuration: map[string]interface{}{"locale": "en-US"},
		Status:        ExternalConversationEndpointActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.Provider != "slack" || endpoint.Adapter != adapter || endpoint.Revision != 1 {
		t.Fatalf("endpoint = %#v", endpoint)
	}
	encoded, _ := json.Marshal(endpoint)
	if string(encoded) == "" || strings.Contains(string(encoded), "connection://") ||
		strings.Contains(string(encoded), "access_token") || strings.Contains(string(encoded), "SLACK_CONNECTION") {
		t.Fatalf("endpoint leaked binding credentials: %s", encoded)
	}
	endpoint.Configuration["locale"] = "mutated"
	stored, err := service.Get(ctx, scope, endpoint.ID)
	if err != nil || stored.Configuration["locale"] != "en-US" {
		t.Fatalf("stored endpoint was not isolated: %#v, %v", stored, err)
	}
	nextDefinition := *slackConversationSkillDefinition()
	nextDefinition.Version = "1.0.1"
	if err := catalog.Register(ctx, &nextDefinition); err != nil {
		t.Fatal(err)
	}
	nextBinding := &skill.Binding{
		ID: "slack", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "slack-agent",
		SkillID: nextDefinition.ID, SkillVersion: nextDefinition.Version,
		EnabledConversationAdapters: []string{"conversations"}, MaximumRisk: skill.RiskLevelRead,
		Credentials: map[string]skill.CredentialReference{
			"SLACK_CONNECTION": {Kind: "slack-oauth", ID: "connection://tenant/one/slack"},
		},
		Revision: 2,
	}
	if err := catalog.Bind(ctx, nextBinding); err != nil {
		t.Fatal(err)
	}
	nextAdapter := adapter
	nextAdapter.SkillVersion = nextDefinition.Version
	nextAdapter.BindingRevision = nextBinding.Revision
	upgraded, err := service.Update(ctx, scope, endpoint.ID, UpdateExternalConversationEndpointRequest{
		ExpectedRevision: stored.Revision, Adapter: &nextAdapter,
	})
	if err != nil || upgraded.Adapter != nextAdapter || upgraded.Revision != stored.Revision+1 {
		t.Fatalf("upgraded endpoint = %#v, %v", upgraded, err)
	}

	_, err = service.Create(ctx, CreateExternalConversationEndpointRequest{
		ID: "stale", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "slack-agent"},
		DeploymentID: "slack-agent", Name: "Stale", Adapter: ExternalConversationAdapterReference{
			SkillID: "slack", SkillVersion: "1.0.0", BindingID: "slack", BindingRevision: 2, AdapterID: "conversations",
		},
		Mode:    capability.ConversationEndpointChannel,
		Handler: ExternalConversationHandler{Kind: ExternalConversationHandlerAgent, ID: "slack-agent"},
		Policy: ExternalConversationPolicy{
			MessageSelection: ExternalConversationSelectAllMessages,
			ReplyMode:        ExternalConversationReplyThread,
			IgnoreBots:       true,
		},
	})
	if !errors.Is(err, ErrInvalidExternalConversation) {
		t.Fatalf("stale binding error = %v", err)
	}

	_, err = service.Create(ctx, CreateExternalConversationEndpointRequest{
		ID: "secret", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "slack-agent"},
		DeploymentID: "slack-agent", Name: "Secret", Adapter: adapter,
		Mode:    capability.ConversationEndpointChannel,
		Handler: ExternalConversationHandler{Kind: ExternalConversationHandlerAgent, ID: "slack-agent"},
		Policy: ExternalConversationPolicy{
			MessageSelection: ExternalConversationSelectAllMessages,
			ReplyMode:        ExternalConversationReplyThread,
			IgnoreBots:       true,
		},
		Configuration: map[string]interface{}{"access_token": "forbidden"},
	})
	if !errors.Is(err, ErrInvalidExternalConversation) {
		t.Fatalf("secret configuration error = %v", err)
	}
}

func TestExternalConversationMemoryInboxIsIdempotentAndLeaseRecoverable(t *testing.T) {
	ctx := context.Background()
	store, endpoint := activeExternalConversationTestEndpoint(t, ctx)
	now := time.Now().UTC().Truncate(time.Millisecond)
	item := &ExternalConversationInboxItem{
		ID: "inbox-1", Scope: endpoint.Scope, EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Event: NormalizedExternalConversationEvent{
			ID: "Ev/+=1", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "workspace/channel", ExternalThreadID: "171.001",
			ExternalMessageID: "wamid.HBg/+=1", ExternalParticipantID: "user@example.com",
			ParticipantDisplayName: "External User", Text: "Can you help?", MentionsEndpoint: true,
			OrderingKey: "workspace/channel:171.001", OccurredAt: now,
		},
		Status: ExternalConversationInboxPending, MaximumAttempts: 8, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	first, replayed, err := store.ReceiveExternalConversationEvent(ctx, item)
	if err != nil || replayed {
		t.Fatalf("receive = %#v, replayed=%v, %v", first, replayed, err)
	}
	replay := cloneExternalConversationInboxItem(item)
	replay.ID = "different-local-id"
	existing, replayed, err := store.ReceiveExternalConversationEvent(ctx, replay)
	if err != nil || !replayed || existing.ID != item.ID {
		t.Fatalf("replay = %#v, replayed=%v, %v", existing, replayed, err)
	}

	leased, err := store.ClaimExternalConversationInbox(ctx, endpoint.Scope, "worker-one", now, time.Minute)
	if err != nil || leased == nil || leased.Status != ExternalConversationInboxLeased || leased.Attempt != 1 {
		t.Fatalf("first lease = %#v, %v", leased, err)
	}
	if next, err := store.ClaimExternalConversationInbox(ctx, endpoint.Scope, "worker-two", now.Add(30*time.Second), time.Minute); err != nil || next != nil {
		t.Fatalf("active lease was stolen: %#v, %v", next, err)
	}
	reclaimed, err := store.ClaimExternalConversationInbox(ctx, endpoint.Scope, "worker-two", now.Add(time.Minute), time.Minute)
	if err != nil || reclaimed == nil || reclaimed.Attempt != 2 || reclaimed.LeaseOwner != "worker-two" {
		t.Fatalf("expired lease recovery = %#v, %v", reclaimed, err)
	}

	stale := cloneExternalConversationInboxItem(leased)
	stale.Status, stale.LeaseOwner, stale.LeaseExpiresAt = ExternalConversationInboxApplied, "", time.Time{}
	stale.ConversationID, stale.ChannelMessageID, stale.Revision = "conversation-1", "message-1", leased.Revision+1
	stale.AppliedAt, stale.UpdatedAt = now.Add(10*time.Second), now.Add(10*time.Second)
	if err := store.SaveExternalConversationInbox(ctx, stale, leased.Revision, "worker-one"); !errors.Is(err, ErrExternalConversationLeaseLost) {
		t.Fatalf("stale worker save error = %v", err)
	}

	applied := cloneExternalConversationInboxItem(reclaimed)
	applied.Status, applied.LeaseOwner, applied.LeaseExpiresAt = ExternalConversationInboxApplied, "", time.Time{}
	applied.ConversationID, applied.ChannelMessageID, applied.RunID = "conversation-1", "message-1", "run-1"
	applied.AppliedAt, applied.UpdatedAt = now.Add(61*time.Second), now.Add(61*time.Second)
	applied.Revision++
	if err := store.SaveExternalConversationInbox(ctx, applied, reclaimed.Revision, "worker-two"); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetExternalConversationInboxItem(ctx, endpoint.Scope, item.ID)
	if err != nil || stored.Status != ExternalConversationInboxApplied || stored.Attempt != 2 {
		t.Fatalf("applied inbox = %#v, %v", stored, err)
	}
}

func TestExternalConversationDeliveryRebindsOnlyUnconfirmedWorkAfterEndpointUpgrade(t *testing.T) {
	ctx := context.Background()
	store, endpoint := activeExternalConversationTestEndpoint(t, ctx)
	now := time.Now().UTC().Truncate(time.Millisecond)
	delivery := &ExternalConversationDelivery{
		ID: "delivery-rebind", Scope: endpoint.Scope, EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Operation: capability.ConversationDeliveryMessageSend, ConversationID: "conversation-1", ChannelMessageID: "message-1",
		OrderingKey: "order-1", Parameters: map[string]interface{}{"text": "approve this"}, IdempotencyKey: "approval:one",
		Status: ExternalConversationDeliveryFailed, Attempt: 8, MaximumAttempts: 8, AvailableAt: now.Add(time.Minute),
		ErrorCode: "adapter_delivery_unconfirmed", Summary: "The adapter could not confirm delivery.",
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if _, replayed, err := store.EnqueueExternalConversationDelivery(ctx, delivery); err != nil || replayed {
		t.Fatalf("enqueue = replayed %v, %v", replayed, err)
	}
	upgraded := cloneExternalConversationEndpoint(endpoint)
	upgraded.Adapter.SkillVersion = "1.0.1"
	upgraded.Adapter.BindingRevision = 2
	upgraded.Revision++
	upgraded.UpdatedAt = now.Add(time.Second)
	if err := store.UpdateExternalConversationEndpoint(ctx, upgraded, endpoint.Revision); err != nil {
		t.Fatal(err)
	}
	replay := cloneExternalConversationDelivery(delivery)
	replay.ID = "ignored-replay-id"
	replay.EndpointRevision = upgraded.Revision
	replay.Adapter = upgraded.Adapter
	replay.Status = ExternalConversationDeliveryPending
	replay.Attempt = 0
	replay.AvailableAt = now.Add(2 * time.Second)
	replay.ErrorCode, replay.Summary = "", ""
	replay.UpdatedAt = now.Add(2 * time.Second)
	rebound, replayed, err := store.EnqueueExternalConversationDelivery(ctx, replay)
	if err != nil || !replayed {
		t.Fatalf("rebind = %#v, replayed %v, %v", rebound, replayed, err)
	}
	if rebound.ID != delivery.ID || rebound.EndpointRevision != upgraded.Revision || rebound.Adapter != upgraded.Adapter ||
		rebound.Status != ExternalConversationDeliveryPending || rebound.Attempt != delivery.Attempt || rebound.Revision != 2 ||
		rebound.ErrorCode != "" || rebound.Summary != "" {
		t.Fatalf("rebound delivery = %#v", rebound)
	}

	leasedForDelivery, err := store.ClaimExternalConversationDelivery(ctx, endpoint.Scope, "delivery-worker", now.Add(3*time.Second), time.Minute)
	if err != nil || leasedForDelivery == nil || leasedForDelivery.ID != delivery.ID {
		t.Fatalf("claim rebound delivery = %#v, %v", leasedForDelivery, err)
	}
	delivered := cloneExternalConversationDelivery(leasedForDelivery)
	delivered.Status = ExternalConversationDeliveryDelivered
	delivered.ProviderMessageID = "provider-message-1"
	delivered.DeliveredAt, delivered.UpdatedAt = now.Add(4*time.Second), now.Add(4*time.Second)
	delivered.LeaseOwner, delivered.LeaseExpiresAt = "", time.Time{}
	delivered.Revision++
	if err := store.SaveExternalConversationDelivery(ctx, delivered, leasedForDelivery.Revision, "delivery-worker"); err != nil {
		t.Fatal(err)
	}
	if _, ok := rebindExternalConversationDelivery(delivered, replay); ok {
		t.Fatal("provider-confirmed delivery was rebound")
	}
	upgradedAgain := cloneExternalConversationEndpoint(upgraded)
	upgradedAgain.Adapter.SkillVersion = "1.0.2"
	upgradedAgain.Adapter.BindingRevision = 3
	upgradedAgain.Revision++
	upgradedAgain.UpdatedAt = now.Add(5 * time.Second)
	if err := store.UpdateExternalConversationEndpoint(ctx, upgradedAgain, upgraded.Revision); err != nil {
		t.Fatal(err)
	}
	deliveredReplay := cloneExternalConversationDelivery(replay)
	deliveredReplay.EndpointRevision = upgradedAgain.Revision
	deliveredReplay.Adapter = upgradedAgain.Adapter
	deliveredReplay.AvailableAt, deliveredReplay.UpdatedAt = now.Add(6*time.Second), now.Add(6*time.Second)
	receipt, replayed, err := store.EnqueueExternalConversationDelivery(ctx, deliveredReplay)
	if err != nil || !replayed || receipt.Status != ExternalConversationDeliveryDelivered ||
		receipt.ProviderMessageID != delivered.ProviderMessageID || receipt.EndpointRevision != delivered.EndpointRevision ||
		receipt.Revision != delivered.Revision {
		t.Fatalf("delivered replay after endpoint upgrade = %#v, replayed %v, %v", receipt, replayed, err)
	}
	leased := cloneExternalConversationDelivery(rebound)
	leased.Status = ExternalConversationDeliveryLeased
	leased.LeaseOwner = "worker-one"
	leased.LeaseExpiresAt = now.Add(time.Minute)
	if _, ok := rebindExternalConversationDelivery(leased, replay); ok {
		t.Fatal("leased delivery was rebound")
	}
}

func TestExternalConversationMemoryMappingsAndOutboxSurviveRetry(t *testing.T) {
	ctx := context.Background()
	store, endpoint := activeExternalConversationTestEndpoint(t, ctx)
	now := time.Now().UTC().Truncate(time.Millisecond)
	conversationMapping := &ExternalConversationMapping{
		Scope: endpoint.Scope, EndpointID: endpoint.ID, ExternalConversationID: "C/one", ExternalThreadID: "171.001",
		ConversationID: "conversation-1", ThreadRootMessageID: "message-root",
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	participantMapping := &ExternalParticipantMapping{
		Scope: endpoint.Scope, EndpointID: endpoint.ID, ExternalParticipantID: "U/one",
		Participant: ConversationParticipant{Type: ConversationParticipantUser, ID: "external-user-1"}, DisplayName: "External User",
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	messageMapping := &ExternalMessageMapping{
		Scope: endpoint.Scope, EndpointID: endpoint.ID, Direction: ExternalMessageInbound, ExternalMessageID: "M/one",
		ConversationID: "conversation-1", ChannelMessageID: "message-in",
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.SaveExternalConversationMapping(ctx, conversationMapping, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveExternalParticipantMapping(ctx, participantMapping, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveExternalMessageMapping(ctx, messageMapping, 0); err != nil {
		t.Fatal(err)
	}
	if mapping, _ := store.GetExternalConversationMapping(ctx, endpoint.Scope, endpoint.ID, "C/one", "171.001"); mapping == nil || mapping.ConversationID != "conversation-1" {
		t.Fatalf("conversation mapping = %#v", mapping)
	}

	delivery := &ExternalConversationDelivery{
		ID: "delivery-1", Scope: endpoint.Scope, EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Operation: capability.ConversationDeliveryMessageSend, ConversationID: "conversation-1", ChannelMessageID: "message-out",
		ExternalThreadID: "171.001", OrderingKey: "order-1", IdempotencyKey: "reply:message-out",
		Status: ExternalConversationDeliveryPending, MaximumAttempts: 5, AvailableAt: now,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if _, replayed, err := store.EnqueueExternalConversationDelivery(ctx, delivery); err != nil || replayed {
		t.Fatalf("enqueue replayed=%v, %v", replayed, err)
	}
	replay := cloneExternalConversationDelivery(delivery)
	replay.ID = "another-delivery-id"
	if existing, replayed, err := store.EnqueueExternalConversationDelivery(ctx, replay); err != nil || !replayed || existing.ID != delivery.ID {
		t.Fatalf("delivery replay = %#v, replayed=%v, %v", existing, replayed, err)
	}
	leased, err := store.ClaimExternalConversationDelivery(ctx, endpoint.Scope, "delivery-worker", now, time.Minute)
	if err != nil || leased == nil || leased.Attempt != 1 {
		t.Fatalf("delivery lease = %#v, %v", leased, err)
	}
	retry := cloneExternalConversationDelivery(leased)
	retry.Status, retry.LeaseOwner, retry.LeaseExpiresAt = ExternalConversationDeliveryRetry, "", time.Time{}
	retry.AvailableAt, retry.UpdatedAt, retry.ErrorCode, retry.Summary = now.Add(2*time.Minute), now.Add(10*time.Second), "rate_limited", "Provider requested retry."
	retry.Revision++
	if err := store.SaveExternalConversationDelivery(ctx, retry, leased.Revision, "delivery-worker"); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimExternalConversationDelivery(ctx, endpoint.Scope, "delivery-worker", now.Add(time.Minute), time.Minute); err != nil || claimed != nil {
		t.Fatalf("delivery ignored retry-after: %#v, %v", claimed, err)
	}
	retried, err := store.ClaimExternalConversationDelivery(ctx, endpoint.Scope, "delivery-worker", now.Add(2*time.Minute), time.Minute)
	if err != nil || retried == nil || retried.Attempt != 2 {
		t.Fatalf("delivery retry = %#v, %v", retried, err)
	}
	delivered := cloneExternalConversationDelivery(retried)
	delivered.Status, delivered.LeaseOwner, delivered.LeaseExpiresAt = ExternalConversationDeliveryDelivered, "", time.Time{}
	delivered.ProviderMessageID, delivered.DeliveredAt, delivered.UpdatedAt = "provider/message/1", now.Add(121*time.Second), now.Add(121*time.Second)
	delivered.ErrorCode, delivered.Summary = "", ""
	delivered.Revision++
	if err := store.SaveExternalConversationDelivery(ctx, delivered, retried.Revision, "delivery-worker"); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetExternalConversationDelivery(ctx, endpoint.Scope, delivery.ID)
	if err != nil || stored.Status != ExternalConversationDeliveryDelivered || stored.Attempt != 2 {
		t.Fatalf("delivered outbox = %#v, %v", stored, err)
	}
}

func TestExternalConversationClaimsSerializeOrderingKeysAndParallelizeIndependentWork(t *testing.T) {
	ctx := context.Background()
	store, endpoint := activeExternalConversationTestEndpoint(t, ctx)
	now := time.Now().UTC().Truncate(time.Millisecond)
	for index, orderingKey := range []string{"thread-one", "thread-one", "thread-two"} {
		eventID := fmt.Sprintf("event-%d", index+1)
		item := &ExternalConversationInboxItem{
			ID: eventID, Scope: endpoint.Scope, EndpointID: endpoint.ID,
			EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
			Event: NormalizedExternalConversationEvent{
				ID: eventID, Type: capability.ConversationEventMessageReceived,
				ExternalConversationID: "channel", ExternalMessageID: "message-" + eventID,
				ExternalParticipantID: "user", Text: "hello", OrderingKey: orderingKey, OccurredAt: now,
			},
			Status: ExternalConversationInboxPending, MaximumAttempts: 3, AvailableAt: now,
			Revision: 1, CreatedAt: now.Add(time.Duration(index) * time.Millisecond),
			UpdatedAt: now.Add(time.Duration(index) * time.Millisecond),
		}
		if _, _, err := store.ReceiveExternalConversationEvent(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.ClaimExternalConversationInbox(ctx, endpoint.Scope, "worker-one", now.Add(time.Second), time.Minute)
	if err != nil || first == nil || first.Event.OrderingKey != "thread-one" {
		t.Fatalf("first inbox claim = %#v, %v", first, err)
	}
	second, err := store.ClaimExternalConversationInbox(ctx, endpoint.Scope, "worker-two", now.Add(time.Second), time.Minute)
	if err != nil || second == nil || second.Event.OrderingKey != "thread-two" {
		t.Fatalf("independent inbox claim = %#v, %v", second, err)
	}
	if blocked, err := store.ClaimExternalConversationInbox(ctx, endpoint.Scope, "worker-three", now.Add(time.Second), time.Minute); err != nil || blocked != nil {
		t.Fatalf("same-order inbox item escaped serialization: %#v, %v", blocked, err)
	}

	for index, orderingKey := range []string{"delivery-thread-one", "delivery-thread-one", "delivery-thread-two"} {
		id := fmt.Sprintf("delivery-order-%d", index+1)
		delivery := &ExternalConversationDelivery{
			ID: id, Scope: endpoint.Scope, EndpointID: endpoint.ID,
			EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
			Operation:      capability.ConversationDeliveryMessageSend,
			ConversationID: "conversation", ChannelMessageID: "message-" + id,
			OrderingKey: orderingKey, IdempotencyKey: "idempotency-" + id,
			Status: ExternalConversationDeliveryPending, MaximumAttempts: 3, AvailableAt: now,
			Revision: 1, CreatedAt: now.Add(time.Duration(index) * time.Millisecond),
			UpdatedAt: now.Add(time.Duration(index) * time.Millisecond),
		}
		if _, _, err := store.EnqueueExternalConversationDelivery(ctx, delivery); err != nil {
			t.Fatal(err)
		}
	}
	firstDelivery, err := store.ClaimExternalConversationDelivery(ctx, endpoint.Scope, "delivery-one", now.Add(time.Second), time.Minute)
	if err != nil || firstDelivery == nil || firstDelivery.OrderingKey != "delivery-thread-one" {
		t.Fatalf("first delivery claim = %#v, %v", firstDelivery, err)
	}
	secondDelivery, err := store.ClaimExternalConversationDelivery(ctx, endpoint.Scope, "delivery-two", now.Add(time.Second), time.Minute)
	if err != nil || secondDelivery == nil || secondDelivery.OrderingKey != "delivery-thread-two" {
		t.Fatalf("independent delivery claim = %#v, %v", secondDelivery, err)
	}
	if blocked, err := store.ClaimExternalConversationDelivery(ctx, endpoint.Scope, "delivery-three", now.Add(time.Second), time.Minute); err != nil || blocked != nil {
		t.Fatalf("same-order delivery escaped serialization: %#v, %v", blocked, err)
	}
}

func TestExternalConversationTransportServiceFiltersIngressAndEnqueuesCanonicalReplies(t *testing.T) {
	ctx := context.Background()
	store, catalog, scope, adapter := externalConversationTestCatalog(t, ctx)
	endpoint, err := NewExternalConversationEndpointService(store, catalog).Create(ctx, CreateExternalConversationEndpointRequest{
		ID: "slack-channel", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "slack-agent"},
		DeploymentID: "slack-agent", Name: "Slack channel", Adapter: adapter,
		Mode: capability.ConversationEndpointChannel, Address: "C012345",
		Handler: ExternalConversationHandler{Kind: ExternalConversationHandlerAgent, ID: "slack-agent"},
		Policy: ExternalConversationPolicy{
			MessageSelection: ExternalConversationSelectMentions,
			ReplyMode:        ExternalConversationReplyThread,
			IgnoreBots:       true,
		},
		Status: ExternalConversationEndpointActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	transport := NewExternalConversationTransportService(store, catalog)
	transport.now = func() time.Time { return now }
	event := NormalizedExternalConversationEvent{
		ID: "event/non-mention", Type: capability.ConversationEventMessageReceived,
		ExternalConversationID: "C/one", ExternalThreadID: "171.001",
		ExternalMessageID: "M/one", ExternalParticipantID: "U/one", Text: "hello",
		OrderingKey: "C/one:171.001", OccurredAt: now,
	}
	ignored, err := transport.Receive(ctx, ReceiveExternalConversationEventRequest{
		Scope: scope, EndpointID: endpoint.ID, Event: event,
	})
	if err != nil || ignored.Accepted || ignored.Item.Status != ExternalConversationInboxIgnored || ignored.Item.AppliedAt.IsZero() {
		t.Fatalf("ignored ingress = %#v, %v", ignored, err)
	}
	replayed, err := transport.Receive(ctx, ReceiveExternalConversationEventRequest{
		Scope: scope, EndpointID: endpoint.ID, Event: event,
	})
	if err != nil || !replayed.Replayed || replayed.Accepted {
		t.Fatalf("ignored replay = %#v, %v", replayed, err)
	}
	event.ID, event.ExternalMessageID, event.MentionsEndpoint = "event/mention", "M/two", true
	accepted, err := transport.Receive(ctx, ReceiveExternalConversationEventRequest{
		Scope: scope, EndpointID: endpoint.ID, Event: event,
	})
	if err != nil || !accepted.Accepted || accepted.Item.Status != ExternalConversationInboxPending {
		t.Fatalf("accepted ingress = %#v, %v", accepted, err)
	}
	reaction := event
	reaction.ID, reaction.Type, reaction.Text = "event/reaction", capability.ConversationEventReactionAdded, ""
	if _, err := transport.Receive(ctx, ReceiveExternalConversationEventRequest{
		Scope: scope, EndpointID: endpoint.ID, Event: reaction,
	}); !errors.Is(err, ErrInvalidExternalConversation) {
		t.Fatalf("undeclared event error = %v", err)
	}

	conversations := NewConversationService(store)
	conversations.now = func() time.Time { return now }
	conversation, _, err := conversations.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "slack-agent"},
		Title: "Slack C012345", IdempotencyKey: "external:" + endpoint.ID + ":C/one",
	})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: "slack-agent"},
		Intent: MessageIntentAnswer, Content: "How can I help?",
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "reply-one",
	})
	if err != nil {
		t.Fatal(err)
	}
	enqueued, err := transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
		Scope: scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryMessageSend,
		ConversationID: conversation.ID, ChannelMessageID: reply.Message.ID, ExternalThreadID: "171.001",
		Correlation: &ExternalConversationDeliveryCorrelation{Kind: "run", ID: "run-one", Phase: "reply"},
	})
	if err != nil || enqueued.Replayed || enqueued.Delivery.Status != ExternalConversationDeliveryPending {
		t.Fatalf("enqueued reply = %#v, %v", enqueued, err)
	}
	replayedDelivery, err := transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
		Scope: scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryMessageSend,
		ConversationID: conversation.ID, ChannelMessageID: reply.Message.ID, ExternalThreadID: "171.001",
		Correlation: &ExternalConversationDeliveryCorrelation{Kind: "run", ID: "run-one", Phase: "reply"},
	})
	if err != nil || !replayedDelivery.Replayed || replayedDelivery.Delivery.ID != enqueued.Delivery.ID {
		t.Fatalf("replayed delivery = %#v, %v", replayedDelivery, err)
	}
	correlated, err := store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{
		Scope: scope, CorrelationKind: "run", CorrelationID: "run-one", Limit: 10,
	})
	if err != nil || len(correlated) != 1 || correlated[0].Correlation == nil || correlated[0].Correlation.Phase != "reply" {
		t.Fatalf("correlated deliveries = %#v, %v", correlated, err)
	}
	if _, err := transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
		Scope: scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryMessageUpdate,
		ConversationID: conversation.ID, ChannelMessageID: reply.Message.ID, ExternalThreadID: "171.001",
		Parameters: map[string]interface{}{"text": "updated"},
	}); !errors.Is(err, ErrInvalidExternalConversation) {
		t.Fatalf("undeclared delivery operation error = %v", err)
	}
}

func externalConversationTestCatalog(t *testing.T, ctx context.Context) (*MemoryStore, *skill.Catalog, Scope, ExternalConversationAdapterReference) {
	t.Helper()
	store := NewMemoryStore()
	catalog := skill.NewCatalog()
	definition := slackConversationSkillDefinition()
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "one"}
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
	return store, catalog, scope, ExternalConversationAdapterReference{
		SkillID: definition.ID, SkillVersion: definition.Version,
		BindingID: binding.ID, BindingRevision: binding.Revision, AdapterID: "conversations",
	}
}

func activeExternalConversationTestEndpoint(t *testing.T, ctx context.Context) (*MemoryStore, *ExternalConversationEndpoint) {
	t.Helper()
	store, catalog, scope, adapter := externalConversationTestCatalog(t, ctx)
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
	return store, endpoint
}
