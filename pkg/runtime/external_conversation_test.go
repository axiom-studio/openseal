package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type switchableConversationAdapterResolver struct {
	externalconversation.ExternalConversationAdapterResolver
	unavailable bool
}

func (r *switchableConversationAdapterResolver) ResolveConversationAdapter(
	ctx context.Context,
	scope skill.ScopeReference,
	deploymentID, skillID, skillVersion, adapterID string,
	binding ...skill.BindingReference,
) (*skill.BoundConversationAdapter, error) {
	if r.unavailable {
		return nil, errors.New("adapter unavailable")
	}
	return r.ExternalConversationAdapterResolver.ResolveConversationAdapter(ctx, scope, deploymentID, skillID, skillVersion, adapterID, binding...)
}

func (r *switchableConversationAdapterResolver) ResolveConversationAdapterBinding(
	ctx context.Context,
	scope skill.ScopeReference,
	deploymentID, bindingID, adapterID string,
) (*skill.BoundConversationAdapter, error) {
	if r.unavailable {
		return nil, errors.New("adapter unavailable")
	}
	return r.ExternalConversationAdapterResolver.ResolveConversationAdapterBinding(ctx, scope, deploymentID, bindingID, adapterID)
}

func TestExternalConversationEndpointFollowsItsSingularSkillBinding(t *testing.T) {
	ctx := context.Background()
	store, catalog, scope, adapter := externalConversationTestCatalog(t, ctx)
	service := externalconversation.NewExternalConversationEndpointService(store, catalog)
	endpoint, err := service.Create(ctx, externalconversation.CreateExternalConversationEndpointRequest{
		ID: "slack-channel", Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "slack-agent"},
		DeploymentID: "slack-agent", Name: "Customer channel", Adapter: adapter,
		Mode: capability.ConversationEndpointChannel, InstallationID: "workspace:T0123", ApplicationID: "app:A0123", Address: "C012345",
		Handler: models.ExternalConversationHandler{Kind: models.ExternalConversationHandlerAgent, ID: "slack-agent"},
		Policy: models.ExternalConversationPolicy{
			MessageSelection: models.ExternalConversationSelectDirectOrMention,
			ReplyMode:        models.ExternalConversationReplyThread,
			IgnoreBots:       true,
		},
		Configuration: map[string]interface{}{"locale": "en-US"},
		Status:        models.ExternalConversationEndpointActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.Provider != "slack" || endpoint.Adapter != adapter || endpoint.InstallationID != "workspace:T0123" ||
		endpoint.ApplicationID != "app:A0123" || endpoint.Revision != 1 {
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
	nextDefinition := *skillbindingtest.SlackConversationSkillDefinition()
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
	upgraded, err := service.Update(ctx, scope, endpoint.ID, externalconversation.UpdateExternalConversationEndpointRequest{
		ExpectedRevision: stored.Revision, Adapter: &nextAdapter,
	})
	if err != nil || upgraded.Adapter != nextAdapter || upgraded.Revision != stored.Revision+1 {
		t.Fatalf("upgraded endpoint = %#v, %v", upgraded, err)
	}

	following, err := service.Create(ctx, externalconversation.CreateExternalConversationEndpointRequest{
		ID: "stale", Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "slack-agent"},
		DeploymentID: "slack-agent", Name: "Stale", Adapter: models.ExternalConversationAdapterReference{
			SkillID: "slack", SkillVersion: "1.0.0", BindingID: "slack", BindingRevision: 2, AdapterID: "conversations",
		},
		Mode:    capability.ConversationEndpointChannel,
		Handler: models.ExternalConversationHandler{Kind: models.ExternalConversationHandlerAgent, ID: "slack-agent"},
		Policy: models.ExternalConversationPolicy{
			MessageSelection: models.ExternalConversationSelectAllMessages,
			ReplyMode:        models.ExternalConversationReplyThread,
			IgnoreBots:       true,
		},
	})
	if err != nil || following.Adapter.BindingID != nextBinding.ID {
		t.Fatalf("binding-following endpoint = %#v, %v", following, err)
	}

	_, err = service.Create(ctx, externalconversation.CreateExternalConversationEndpointRequest{
		ID: "secret", Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "slack-agent"},
		DeploymentID: "slack-agent", Name: "Secret", Adapter: adapter,
		Mode:    capability.ConversationEndpointChannel,
		Handler: models.ExternalConversationHandler{Kind: models.ExternalConversationHandlerAgent, ID: "slack-agent"},
		Policy: models.ExternalConversationPolicy{
			MessageSelection: models.ExternalConversationSelectAllMessages,
			ReplyMode:        models.ExternalConversationReplyThread,
			IgnoreBots:       true,
		},
		Configuration: map[string]interface{}{"access_token": "forbidden"},
	})
	if !errors.Is(err, models.ErrInvalidExternalConversation) {
		t.Fatalf("secret configuration error = %v", err)
	}
}

func TestExternalConversationEndpointCanRetireWhenAdapterIsUnavailable(t *testing.T) {
	ctx := context.Background()
	store, catalog, scope, adapter := externalConversationTestCatalog(t, ctx)
	resolver := &switchableConversationAdapterResolver{ExternalConversationAdapterResolver: catalog}
	service := externalconversation.NewExternalConversationEndpointService(store, resolver)
	endpoint, err := service.Create(ctx, externalconversation.CreateExternalConversationEndpointRequest{
		ID: "slack-retire", Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "slack-agent"},
		DeploymentID: "slack-agent", Name: "Retirable endpoint", Adapter: adapter,
		Mode: capability.ConversationEndpointChannel, Address: "C012345",
		Handler: models.ExternalConversationHandler{Kind: models.ExternalConversationHandlerAgent, ID: "slack-agent"},
		Policy: models.ExternalConversationPolicy{
			MessageSelection: models.ExternalConversationSelectDirectOrMention,
			ReplyMode:        models.ExternalConversationReplyThread, IgnoreBots: true,
		},
		Status: models.ExternalConversationEndpointActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver.unavailable = true
	retired := models.ExternalConversationEndpointRetired
	endpoint, err = service.Update(ctx, scope, endpoint.ID, externalconversation.UpdateExternalConversationEndpointRequest{
		ExpectedRevision: endpoint.Revision, Status: &retired,
	})
	if err != nil || endpoint.Status != models.ExternalConversationEndpointRetired || endpoint.RetiredAt == nil {
		t.Fatalf("retired endpoint = %#v, %v", endpoint, err)
	}
}

func TestExternalConversationInboxIsIdempotentAndLeaseRecoverable(t *testing.T) {
	ctx := context.Background()
	store, endpoint := activeExternalConversationTestEndpoint(t, ctx)
	now := time.Now().UTC().Truncate(time.Millisecond)
	item := &models.ExternalConversationInboxItem{
		ID: "inbox-1", Scope: endpoint.Scope, EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Event: models.NormalizedExternalConversationEvent{
			ID: "Ev/+=1", Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "workspace/channel", ExternalThreadID: "171.001",
			ExternalMessageID: "wamid.HBg/+=1", ExternalParticipantID: "user@example.com",
			ParticipantDisplayName: "External User", Text: "Can you help?", MentionsEndpoint: true,
			OrderingKey: "workspace/channel:171.001", OccurredAt: now,
		},
		Status: models.ExternalConversationInboxPending, MaximumAttempts: 8, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	first, replayed, err := store.ReceiveExternalConversationEvent(ctx, item)
	if err != nil || replayed {
		t.Fatalf("receive = %#v, replayed=%v, %v", first, replayed, err)
	}
	replay := models.CloneExternalConversationInboxItem(item)
	replay.ID = "different-local-id"
	existing, replayed, err := store.ReceiveExternalConversationEvent(ctx, replay)
	if err != nil || !replayed || existing.ID != item.ID {
		t.Fatalf("replay = %#v, replayed=%v, %v", existing, replayed, err)
	}

	leased, err := store.ClaimExternalConversationInbox(ctx, endpoint.Scope, "worker-one", now, time.Minute)
	if err != nil || leased == nil || leased.Status != models.ExternalConversationInboxLeased || leased.Attempt != 1 {
		t.Fatalf("first lease = %#v, %v", leased, err)
	}
	if next, err := store.ClaimExternalConversationInbox(ctx, endpoint.Scope, "worker-two", now.Add(30*time.Second), time.Minute); err != nil || next != nil {
		t.Fatalf("active lease was stolen: %#v, %v", next, err)
	}
	reclaimed, err := store.ClaimExternalConversationInbox(ctx, endpoint.Scope, "worker-two", now.Add(time.Minute), time.Minute)
	if err != nil || reclaimed == nil || reclaimed.Attempt != 2 || reclaimed.LeaseOwner != "worker-two" {
		t.Fatalf("expired lease recovery = %#v, %v", reclaimed, err)
	}

	stale := models.CloneExternalConversationInboxItem(leased)
	stale.Status, stale.LeaseOwner, stale.LeaseExpiresAt = models.ExternalConversationInboxApplied, "", time.Time{}
	stale.ConversationID, stale.ChannelMessageID, stale.Revision = "conversation-1", "message-1", leased.Revision+1
	stale.AppliedAt, stale.UpdatedAt = now.Add(10*time.Second), now.Add(10*time.Second)
	if err := store.SaveExternalConversationInbox(ctx, stale, leased.Revision, "worker-one"); !errors.Is(err, models.ErrExternalConversationLeaseLost) {
		t.Fatalf("stale worker save error = %v", err)
	}

	applied := models.CloneExternalConversationInboxItem(reclaimed)
	applied.Status, applied.LeaseOwner, applied.LeaseExpiresAt = models.ExternalConversationInboxApplied, "", time.Time{}
	applied.ConversationID, applied.ChannelMessageID, applied.RunID = "conversation-1", "message-1", "run-1"
	applied.AppliedAt, applied.UpdatedAt = now.Add(61*time.Second), now.Add(61*time.Second)
	applied.Revision++
	if err := store.SaveExternalConversationInbox(ctx, applied, reclaimed.Revision, "worker-two"); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetExternalConversationInboxItem(ctx, endpoint.Scope, item.ID)
	if err != nil || stored.Status != models.ExternalConversationInboxApplied || stored.Attempt != 2 {
		t.Fatalf("applied inbox = %#v, %v", stored, err)
	}
}

func TestExternalConversationDeliveryRebindsOnlyUnconfirmedWorkAfterEndpointUpgrade(t *testing.T) {
	ctx := context.Background()
	store, endpoint := activeExternalConversationTestEndpoint(t, ctx)
	now := time.Now().UTC().Truncate(time.Millisecond)
	delivery := &models.ExternalConversationDelivery{
		ID: "delivery-rebind", Scope: endpoint.Scope, EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Operation: capability.ConversationDeliveryMessageSend, ConversationID: "conversation-1", ChannelMessageID: "message-1",
		OrderingKey: "order-1", Parameters: map[string]interface{}{"text": "approve this"}, IdempotencyKey: "approval:one",
		Status: models.ExternalConversationDeliveryFailed, Attempt: 8, MaximumAttempts: 8, AvailableAt: now.Add(time.Minute),
		ErrorCode: "adapter_delivery_unconfirmed", Summary: "The adapter could not confirm delivery.",
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if _, replayed, err := store.EnqueueExternalConversationDelivery(ctx, delivery); err != nil || replayed {
		t.Fatalf("enqueue = replayed %v, %v", replayed, err)
	}
	upgraded := models.CloneExternalConversationEndpoint(endpoint)
	upgraded.Adapter.SkillVersion = "1.0.1"
	upgraded.Adapter.BindingRevision = 2
	upgraded.Revision++
	upgraded.UpdatedAt = now.Add(time.Second)
	if err := store.UpdateExternalConversationEndpoint(ctx, upgraded, endpoint.Revision); err != nil {
		t.Fatal(err)
	}
	replay := models.CloneExternalConversationDelivery(delivery)
	replay.ID = "ignored-replay-id"
	replay.EndpointRevision = upgraded.Revision
	replay.Adapter = upgraded.Adapter
	replay.Status = models.ExternalConversationDeliveryPending
	replay.Attempt = 0
	replay.AvailableAt = now.Add(2 * time.Second)
	replay.ErrorCode, replay.Summary = "", ""
	replay.UpdatedAt = now.Add(2 * time.Second)
	rebound, replayed, err := store.EnqueueExternalConversationDelivery(ctx, replay)
	if err != nil || !replayed {
		t.Fatalf("rebind = %#v, replayed %v, %v", rebound, replayed, err)
	}
	if rebound.ID != delivery.ID || rebound.EndpointRevision != upgraded.Revision || rebound.Adapter != upgraded.Adapter ||
		rebound.Status != models.ExternalConversationDeliveryPending || rebound.Attempt != delivery.Attempt || rebound.Revision != 2 ||
		rebound.ErrorCode != "" || rebound.Summary != "" {
		t.Fatalf("rebound delivery = %#v", rebound)
	}

	leasedForDelivery, err := store.ClaimExternalConversationDelivery(ctx, endpoint.Scope, "delivery-worker", now.Add(3*time.Second), time.Minute)
	if err != nil || leasedForDelivery == nil || leasedForDelivery.ID != delivery.ID {
		t.Fatalf("claim rebound delivery = %#v, %v", leasedForDelivery, err)
	}
	delivered := models.CloneExternalConversationDelivery(leasedForDelivery)
	delivered.Status = models.ExternalConversationDeliveryDelivered
	delivered.ProviderMessageID = "provider-message-1"
	delivered.DeliveredAt, delivered.UpdatedAt = now.Add(4*time.Second), now.Add(4*time.Second)
	delivered.LeaseOwner, delivered.LeaseExpiresAt = "", time.Time{}
	delivered.Revision++
	if err := store.SaveExternalConversationDelivery(ctx, delivered, leasedForDelivery.Revision, "delivery-worker"); err != nil {
		t.Fatal(err)
	}
	if _, ok := models.RebindExternalConversationDelivery(delivered, replay); ok {
		t.Fatal("provider-confirmed delivery was rebound")
	}
	upgradedAgain := models.CloneExternalConversationEndpoint(upgraded)
	upgradedAgain.Adapter.SkillVersion = "1.0.2"
	upgradedAgain.Adapter.BindingRevision = 3
	upgradedAgain.Revision++
	upgradedAgain.UpdatedAt = now.Add(5 * time.Second)
	if err := store.UpdateExternalConversationEndpoint(ctx, upgradedAgain, upgraded.Revision); err != nil {
		t.Fatal(err)
	}
	deliveredReplay := models.CloneExternalConversationDelivery(replay)
	deliveredReplay.EndpointRevision = upgradedAgain.Revision
	deliveredReplay.Adapter = upgradedAgain.Adapter
	deliveredReplay.AvailableAt, deliveredReplay.UpdatedAt = now.Add(6*time.Second), now.Add(6*time.Second)
	receipt, replayed, err := store.EnqueueExternalConversationDelivery(ctx, deliveredReplay)
	if err != nil || !replayed || receipt.Status != models.ExternalConversationDeliveryDelivered ||
		receipt.ProviderMessageID != delivered.ProviderMessageID || receipt.EndpointRevision != delivered.EndpointRevision ||
		receipt.Revision != delivered.Revision {
		t.Fatalf("delivered replay after endpoint upgrade = %#v, replayed %v, %v", receipt, replayed, err)
	}
	leased := models.CloneExternalConversationDelivery(rebound)
	leased.Status = models.ExternalConversationDeliveryLeased
	leased.LeaseOwner = "worker-one"
	leased.LeaseExpiresAt = now.Add(time.Minute)
	if _, ok := models.RebindExternalConversationDelivery(leased, replay); ok {
		t.Fatal("leased delivery was rebound")
	}
}

func TestExternalConversationMappingsAndOutboxSurviveRetry(t *testing.T) {
	ctx := context.Background()
	store, endpoint := activeExternalConversationTestEndpoint(t, ctx)
	now := time.Now().UTC().Truncate(time.Millisecond)
	conversationMapping := &models.ExternalConversationMapping{
		Scope: endpoint.Scope, EndpointID: endpoint.ID, ExternalConversationID: "C/one", ExternalThreadID: "171.001",
		ConversationID: "conversation-1", ThreadRootMessageID: "message-root",
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	participantMapping := &models.ExternalParticipantMapping{
		Scope: endpoint.Scope, EndpointID: endpoint.ID, ExternalParticipantID: "U/one",
		Participant: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "external-user-1"}, DisplayName: "External User",
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	messageMapping := &models.ExternalMessageMapping{
		Scope: endpoint.Scope, EndpointID: endpoint.ID, Direction: models.ExternalMessageInbound, ExternalMessageID: "M/one",
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

	delivery := &models.ExternalConversationDelivery{
		ID: "delivery-1", Scope: endpoint.Scope, EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Operation: capability.ConversationDeliveryMessageSend, ConversationID: "conversation-1", ChannelMessageID: "message-out",
		ExternalThreadID: "171.001", OrderingKey: "order-1", IdempotencyKey: "reply:message-out",
		Status: models.ExternalConversationDeliveryPending, MaximumAttempts: 5, AvailableAt: now,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if _, replayed, err := store.EnqueueExternalConversationDelivery(ctx, delivery); err != nil || replayed {
		t.Fatalf("enqueue replayed=%v, %v", replayed, err)
	}
	replay := models.CloneExternalConversationDelivery(delivery)
	replay.ID = "another-delivery-id"
	if existing, replayed, err := store.EnqueueExternalConversationDelivery(ctx, replay); err != nil || !replayed || existing.ID != delivery.ID {
		t.Fatalf("delivery replay = %#v, replayed=%v, %v", existing, replayed, err)
	}
	leased, err := store.ClaimExternalConversationDelivery(ctx, endpoint.Scope, "delivery-worker", now, time.Minute)
	if err != nil || leased == nil || leased.Attempt != 1 {
		t.Fatalf("delivery lease = %#v, %v", leased, err)
	}
	retry := models.CloneExternalConversationDelivery(leased)
	retry.Status, retry.LeaseOwner, retry.LeaseExpiresAt = models.ExternalConversationDeliveryRetry, "", time.Time{}
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
	delivered := models.CloneExternalConversationDelivery(retried)
	delivered.Status, delivered.LeaseOwner, delivered.LeaseExpiresAt = models.ExternalConversationDeliveryDelivered, "", time.Time{}
	delivered.ProviderMessageID, delivered.DeliveredAt, delivered.UpdatedAt = "provider/message/1", now.Add(121*time.Second), now.Add(121*time.Second)
	delivered.ErrorCode, delivered.Summary = "", ""
	delivered.Revision++
	if err := store.SaveExternalConversationDelivery(ctx, delivered, retried.Revision, "delivery-worker"); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetExternalConversationDelivery(ctx, endpoint.Scope, delivery.ID)
	if err != nil || stored.Status != models.ExternalConversationDeliveryDelivered || stored.Attempt != 2 {
		t.Fatalf("delivered outbox = %#v, %v", stored, err)
	}
}

func TestExternalConversationClaimsSerializeOrderingKeysAndParallelizeIndependentWork(t *testing.T) {
	ctx := context.Background()
	store, endpoint := activeExternalConversationTestEndpoint(t, ctx)
	now := time.Now().UTC().Truncate(time.Millisecond)
	for index, orderingKey := range []string{"thread-one", "thread-one", "thread-two"} {
		eventID := fmt.Sprintf("event-%d", index+1)
		item := &models.ExternalConversationInboxItem{
			ID: eventID, Scope: endpoint.Scope, EndpointID: endpoint.ID,
			EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
			Event: models.NormalizedExternalConversationEvent{
				ID: eventID, Type: capability.ConversationEventMessageReceived,
				ExternalConversationID: "channel", ExternalMessageID: "message-" + eventID,
				ExternalParticipantID: "user", Text: "hello", OrderingKey: orderingKey, OccurredAt: now,
			},
			Status: models.ExternalConversationInboxPending, MaximumAttempts: 3, AvailableAt: now,
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
		delivery := &models.ExternalConversationDelivery{
			ID: id, Scope: endpoint.Scope, EndpointID: endpoint.ID,
			EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
			Operation:      capability.ConversationDeliveryMessageSend,
			ConversationID: "conversation", ChannelMessageID: "message-" + id,
			OrderingKey: orderingKey, IdempotencyKey: "idempotency-" + id,
			Status: models.ExternalConversationDeliveryPending, MaximumAttempts: 3, AvailableAt: now,
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
	endpoint, err := externalconversation.NewExternalConversationEndpointService(store, catalog).Create(ctx, externalconversation.CreateExternalConversationEndpointRequest{
		ID: "slack-channel", Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "slack-agent"},
		DeploymentID: "slack-agent", Name: "Slack channel", Adapter: adapter,
		Mode: capability.ConversationEndpointChannel, Address: "C012345",
		Handler: models.ExternalConversationHandler{Kind: models.ExternalConversationHandlerAgent, ID: "slack-agent"},
		Policy: models.ExternalConversationPolicy{
			MessageSelection: models.ExternalConversationSelectMentions,
			ReplyMode:        models.ExternalConversationReplyThread,
			IgnoreBots:       true,
		},
		Status: models.ExternalConversationEndpointActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A compatible binding amendment must not require rewriting every live
	// endpoint. The endpoint retains its reviewed binding identity while each
	// new operation captures the current exact binding revision.
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "slack", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "slack-agent",
		SkillID: "slack", SkillVersion: "1.0.0",
		EnabledConversationAdapters: []string{"conversations"}, MaximumRisk: skill.RiskLevelRead,
		Credentials: map[string]skill.CredentialReference{
			"SLACK_CONNECTION": {Kind: "slack-oauth", ID: "connection://tenant/one/slack"},
		},
		Revision: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if endpoint.Adapter.BindingRevision != 1 {
		t.Fatalf("endpoint binding revision changed without an endpoint amendment: %#v", endpoint.Adapter)
	}
	if endpoint.Status != models.ExternalConversationEndpointActive {
		t.Fatalf("endpoint status = %q", endpoint.Status)
	}
	resolvedAfterAmendment, resolveErr := catalog.ResolveConversationAdapterBinding(
		ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "slack-agent", "slack", "conversations",
	)
	if resolveErr != nil || resolvedAfterAmendment == nil || resolvedAfterAmendment.Binding.Revision != 2 {
		t.Fatalf("current conversation binding = %#v, %v", resolvedAfterAmendment, resolveErr)
	}
	if resolvedAfterAmendment.Adapter.Provider != endpoint.Provider ||
		!slices.Contains(resolvedAfterAmendment.Adapter.EndpointModes, endpoint.Mode) {
		t.Fatalf("current adapter is incompatible: provider %q/%q modes %#v/%q", resolvedAfterAmendment.Adapter.Provider, endpoint.Provider, resolvedAfterAmendment.Adapter.EndpointModes, endpoint.Mode)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	transport := externalconversation.NewExternalConversationTransportService(store, catalog, models.WithClock(func() time.Time { return now }))
	event := models.NormalizedExternalConversationEvent{
		ID: "event/non-mention", Type: capability.ConversationEventMessageReceived,
		ExternalConversationID: "C/one", ExternalThreadID: "171.001",
		ExternalMessageID: "M/one", ExternalParticipantID: "U/one", Text: "hello",
		OrderingKey: "C/one:171.001", OccurredAt: now,
	}
	ignored, err := transport.Receive(ctx, externalconversation.ReceiveExternalConversationEventRequest{
		Scope: scope, EndpointID: endpoint.ID, Event: event,
	})
	if err != nil || ignored.Accepted || ignored.Item.Status != models.ExternalConversationInboxIgnored || ignored.Item.AppliedAt.IsZero() {
		t.Fatalf("ignored ingress = %#v, %v", ignored, err)
	}
	replayed, err := transport.Receive(ctx, externalconversation.ReceiveExternalConversationEventRequest{
		Scope: scope, EndpointID: endpoint.ID, Event: event,
	})
	if err != nil || !replayed.Replayed || replayed.Accepted {
		t.Fatalf("ignored replay = %#v, %v", replayed, err)
	}
	event.ID, event.ExternalMessageID, event.MentionsEndpoint = "event/mention", "M/two", true
	accepted, err := transport.Receive(ctx, externalconversation.ReceiveExternalConversationEventRequest{
		Scope: scope, EndpointID: endpoint.ID, Event: event,
	})
	if err != nil || !accepted.Accepted || accepted.Item.Status != models.ExternalConversationInboxPending {
		t.Fatalf("accepted ingress = %#v, %v", accepted, err)
	}
	if accepted.Item.Adapter.BindingRevision != 2 {
		t.Fatalf("inbox captured binding revision %d, want 2", accepted.Item.Adapter.BindingRevision)
	}
	reaction := event
	reaction.ID, reaction.Type, reaction.Text = "event/reaction", capability.ConversationEventReactionAdded, ""
	if _, err := transport.Receive(ctx, externalconversation.ReceiveExternalConversationEventRequest{
		Scope: scope, EndpointID: endpoint.ID, Event: reaction,
	}); !errors.Is(err, models.ErrInvalidExternalConversation) {
		t.Fatalf("undeclared event error = %v", err)
	}

	conversations := runtimeconversation.NewConversationService(store, models.WithClock(func() time.Time { return now }))
	seedTestSpace(t, store, scope)
	conversation, _, err := conversations.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "slack-agent"},
		Title: "Slack C012345", IdempotencyKey: "external:" + endpoint.ID + ":C/one",
	})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := conversations.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "slack-agent"},
		Intent: models.MessageIntentAnswer, Content: "How can I help?",
		Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: "reply-one",
	})
	if err != nil {
		t.Fatal(err)
	}
	enqueued, err := transport.Enqueue(ctx, externalconversation.EnqueueExternalConversationDeliveryRequest{
		Scope: scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryMessageSend,
		ConversationID: conversation.ID, ChannelMessageID: reply.Message.ID, ExternalThreadID: "171.001",
		Correlation: &models.ExternalConversationDeliveryCorrelation{Kind: "run", ID: "run-one", Phase: "reply"},
	})
	if err != nil || enqueued.Replayed || enqueued.Delivery.Status != models.ExternalConversationDeliveryPending {
		t.Fatalf("enqueued reply = %#v, %v", enqueued, err)
	}
	if enqueued.Delivery.Adapter.BindingRevision != 2 {
		t.Fatalf("delivery captured binding revision %d, want 2", enqueued.Delivery.Adapter.BindingRevision)
	}
	replayedDelivery, err := transport.Enqueue(ctx, externalconversation.EnqueueExternalConversationDeliveryRequest{
		Scope: scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryMessageSend,
		ConversationID: conversation.ID, ChannelMessageID: reply.Message.ID, ExternalThreadID: "171.001",
		Correlation: &models.ExternalConversationDeliveryCorrelation{Kind: "run", ID: "run-one", Phase: "reply"},
	})
	if err != nil || !replayedDelivery.Replayed || replayedDelivery.Delivery.ID != enqueued.Delivery.ID {
		t.Fatalf("replayed delivery = %#v, %v", replayedDelivery, err)
	}
	correlated, err := store.ListExternalConversationDeliveries(ctx, models.ExternalConversationDeliveryFilter{
		Scope: scope, CorrelationKind: "run", CorrelationID: "run-one", Limit: 10,
	})
	if err != nil || len(correlated) != 1 || correlated[0].Correlation == nil || correlated[0].Correlation.Phase != "reply" {
		t.Fatalf("correlated deliveries = %#v, %v", correlated, err)
	}
	if _, err := transport.Enqueue(ctx, externalconversation.EnqueueExternalConversationDeliveryRequest{
		Scope: scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryMessageUpdate,
		ConversationID: conversation.ID, ChannelMessageID: reply.Message.ID, ExternalThreadID: "171.001",
		Parameters: map[string]interface{}{"text": "updated"},
	}); !errors.Is(err, models.ErrInvalidExternalConversation) {
		t.Fatalf("undeclared delivery operation error = %v", err)
	}
}

func externalConversationTestCatalog(t *testing.T, ctx context.Context) (*sqlite.Store, *skill.Catalog, models.Scope, models.ExternalConversationAdapterReference) {
	t.Helper()
	store := kerneltest.NewStore(t)
	catalog := skill.NewCatalog()
	definition := skillbindingtest.SlackConversationSkillDefinition()
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	scope := models.Scope{Kind: "tenant", ID: "one"}
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
	return store, catalog, scope, models.ExternalConversationAdapterReference{
		SkillID: definition.ID, SkillVersion: definition.Version,
		BindingID: binding.ID, BindingRevision: binding.Revision, AdapterID: "conversations",
	}
}

func activeExternalConversationTestEndpoint(t *testing.T, ctx context.Context) (*sqlite.Store, *models.ExternalConversationEndpoint) {
	t.Helper()
	store, catalog, scope, adapter := externalConversationTestCatalog(t, ctx)
	endpoint, err := externalconversation.NewExternalConversationEndpointService(store, catalog).Create(ctx, externalconversation.CreateExternalConversationEndpointRequest{
		ID: "slack-channel", Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "slack-agent"},
		DeploymentID: "slack-agent", Name: "Slack channel", Adapter: adapter,
		Mode: capability.ConversationEndpointChannel, Address: "C012345",
		Handler: models.ExternalConversationHandler{Kind: models.ExternalConversationHandlerAgent, ID: "slack-agent"},
		Policy: models.ExternalConversationPolicy{
			MessageSelection: models.ExternalConversationSelectAllMessages,
			ReplyMode:        models.ExternalConversationReplyThread,
			IgnoreBots:       true,
		},
		Status: models.ExternalConversationEndpointActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, endpoint
}
