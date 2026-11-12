package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type acknowledgementRecoveryHost struct {
	provider     string
	acknowledged map[string]string
	deliveries   int
	lookups      int
	address      string
}

type ephemeralRetryHost struct {
	deliveries int
	lookups    int
}

func (h *ephemeralRetryHost) LookupExternalConversationDelivery(
	context.Context,
	externalconversation.ExternalConversationDeliveryHostRequest,
) (*externalconversation.ExternalConversationDeliveryAcknowledgement, error) {
	h.lookups++
	return &externalconversation.ExternalConversationDeliveryAcknowledgement{Status: externalconversation.ExternalConversationAcknowledgementUnknown}, nil
}

func (h *ephemeralRetryHost) DeliverExternalConversation(
	_ context.Context,
	req externalconversation.ExternalConversationDeliveryHostRequest,
) (*externalconversation.ExternalConversationDeliveryHostResult, error) {
	h.deliveries++
	if req.Delivery.Operation != capability.ConversationDeliveryTypingIndicator {
		return nil, errors.New("expected typing indicator")
	}
	if h.deliveries == 1 {
		return nil, errors.New("temporary provider failure")
	}
	return &externalconversation.ExternalConversationDeliveryHostResult{
		Outcome: externalconversation.ExternalConversationDeliveryOutcomeDelivered, ProviderMessageID: req.Delivery.ExternalThreadID,
	}, nil
}

func (h *acknowledgementRecoveryHost) LookupExternalConversationDelivery(
	_ context.Context,
	req externalconversation.ExternalConversationDeliveryHostRequest,
) (*externalconversation.ExternalConversationDeliveryAcknowledgement, error) {
	h.lookups++
	if req.Adapter.Adapter.Provider != h.provider || req.Endpoint.Provider != h.provider {
		return nil, errors.New("wrong provider adapter")
	}
	providerMessageID := h.acknowledged[req.Delivery.IdempotencyKey]
	if providerMessageID == "" {
		return &externalconversation.ExternalConversationDeliveryAcknowledgement{Status: externalconversation.ExternalConversationAcknowledgementNotFound}, nil
	}
	return &externalconversation.ExternalConversationDeliveryAcknowledgement{
		Status: externalconversation.ExternalConversationAcknowledgementFound, ProviderMessageID: providerMessageID,
	}, nil
}

func (h *acknowledgementRecoveryHost) DeliverExternalConversation(
	_ context.Context,
	req externalconversation.ExternalConversationDeliveryHostRequest,
) (*externalconversation.ExternalConversationDeliveryHostResult, error) {
	h.deliveries++
	h.address = req.Endpoint.Address
	if req.Adapter.Adapter.Provider != h.provider || req.Message.Content != "Canonical answer" ||
		req.Adapter.Binding == nil || req.Adapter.Binding.Revision != req.Delivery.Adapter.BindingRevision {
		return nil, errors.New("host received drifted delivery state")
	}
	providerMessageID := h.provider + "/message/1"
	h.acknowledged[req.Delivery.IdempotencyKey] = providerMessageID
	// Model a provider acknowledgement followed by a lost response. The next
	// lease must recover through lookup instead of sending a second message.
	return nil, errors.New("connection closed after provider acknowledgement")
}

func TestExternalConversationDeliveryWorkerUsesOriginForInstallationWideEndpoint(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	previousRevision := endpoint.Revision
	endpoint.Address = "C-approvals"
	endpoint.InstallationWide = true
	endpoint.Revision++
	endpoint.UpdatedAt = endpoint.UpdatedAt.Add(time.Second)
	if err := store.UpdateExternalConversationEndpoint(ctx, endpoint, previousRevision); err != nil {
		t.Fatal(err)
	}
	conversations := runtimeconversation.NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
		Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "Slack mention",
		IdempotencyKey: "installation-wide-origin",
	})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := conversations.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: endpoint.Owner.ID},
		Intent: MessageIntentAnswer, Content: "Canonical answer",
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "answer",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = externalconversation.NewExternalConversationTransportService(store, catalog).Enqueue(ctx, externalconversation.EnqueueExternalConversationDeliveryRequest{
		Scope: endpoint.Scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryMessageSend,
		ConversationID: conversation.ID, ChannelMessageID: reply.Message.ID,
		ExternalConversationID: "C-origin", MaximumAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	host := &acknowledgementRecoveryHost{provider: "slack", acknowledged: map[string]string{}}
	worker, err := externalconversation.NewExternalConversationDeliveryWorker(store, catalog, host, externalconversation.ExternalConversationDeliveryWorkerConfig{WorkerID: "origin-worker"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = worker.ProcessOne(ctx, endpoint.Scope)
	if host.address != "C-origin" {
		t.Fatalf("delivery address = %q, want originating conversation", host.address)
	}
	storedEndpoint, err := store.GetExternalConversationEndpoint(ctx, endpoint.Scope, endpoint.ID)
	if err != nil || storedEndpoint.Address != "C-approvals" {
		t.Fatalf("durable endpoint was mutated: %#v, %v", storedEndpoint, err)
	}
}

func TestExternalConversationDeliveryWorkerRetriesTypingWithoutMessageLookup(t *testing.T) {
	ctx := context.Background()
	store, catalog, endpoint := externalConversationDeliveryFixtureWithOperations(t, ctx, "slack", []capability.ConversationDeliveryOperation{
		capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryTypingIndicator,
	})
	conversations := runtimeconversation.NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
		Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "Slack progress", IdempotencyKey: "typing-retry",
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := conversations.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: endpoint.Owner.ID},
		Intent: MessageIntentAcknowledgment, Content: "Checking now.",
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "typing-status",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = externalconversation.NewExternalConversationTransportService(store, catalog).Enqueue(ctx, externalconversation.EnqueueExternalConversationDeliveryRequest{
		Scope: endpoint.Scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryTypingIndicator,
		ConversationID: conversation.ID, ChannelMessageID: message.Message.ID, ExternalThreadID: "thread-1",
		Parameters: map[string]interface{}{"status": "Checking now."}, MaximumAttempts: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A previous progress acknowledgement already owns this thread timestamp.
	if err := store.SaveExternalMessageMapping(ctx, &ExternalMessageMapping{
		Scope: endpoint.Scope, EndpointID: endpoint.ID, Direction: ExternalMessageOutbound,
		ExternalMessageID: "thread-1", ConversationID: conversation.ID,
		ChannelMessageID: "earlier-progress", Revision: 1,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}, 0); err != nil {
		t.Fatal(err)
	}
	host := &ephemeralRetryHost{}
	workerClock := time.Now
	worker, err := externalconversation.NewExternalConversationDeliveryWorker(store, catalog, host, externalconversation.ExternalConversationDeliveryWorkerConfig{
		WorkerID: "typing-worker", BaseRetry: time.Nanosecond, MaximumRetry: time.Second,
	}, models.WithClock(func() time.Time { return workerClock() }))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	workerClock = func() time.Time { return now }
	if retry, processErr := worker.ProcessOne(ctx, endpoint.Scope); processErr == nil || retry == nil || retry.Status != ExternalConversationDeliveryRetry {
		t.Fatalf("first typing attempt = %#v, %v", retry, processErr)
	}
	workerClock = func() time.Time { return now.Add(time.Second) }
	delivered, err := worker.ProcessOne(ctx, endpoint.Scope)
	if err != nil || delivered == nil || delivered.Status != ExternalConversationDeliveryDelivered {
		t.Fatalf("retried typing delivery = %#v, %v", delivered, err)
	}
	mapping, err := store.GetExternalMessageMapping(ctx, endpoint.Scope, endpoint.ID, ExternalMessageOutbound, "thread-1")
	if err != nil || mapping == nil || mapping.ChannelMessageID != "earlier-progress" {
		t.Fatalf("typing changed the existing message mapping: %#v, %v", mapping, err)
	}
	if host.deliveries != 2 || host.lookups != 0 {
		t.Fatalf("host calls: deliveries=%d lookups=%d", host.deliveries, host.lookups)
	}
}

func TestExternalConversationDeliveryWorkerUsesOneHostContractAndRecoversAcknowledgement(t *testing.T) {
	for _, provider := range []string{"slack", "webchat"} {
		t.Run(provider, func(t *testing.T) {
			ctx := context.Background()
			store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, provider)
			conversations := runtimeconversation.NewConversationService(store)
			conversation, _, err := conversations.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
				Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "External support",
				Origin: &ConversationReference{
					Kind: ConversationReferenceExternalSource, ID: endpoint.ID, Version: endpoint.Revision,
				},
				IdempotencyKey: "external-support",
			})
			if err != nil {
				t.Fatal(err)
			}
			reply, err := conversations.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
				Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
				Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: endpoint.Owner.ID},
				Intent: MessageIntentAnswer, Content: "Canonical answer",
				Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "answer",
			})
			if err != nil {
				t.Fatal(err)
			}
			enqueued, err := externalconversation.NewExternalConversationTransportService(store, catalog).Enqueue(
				ctx,
				externalconversation.EnqueueExternalConversationDeliveryRequest{
					Scope: endpoint.Scope, EndpointID: endpoint.ID,
					Operation:      capability.ConversationDeliveryMessageSend,
					ConversationID: conversation.ID, ChannelMessageID: reply.Message.ID,
					ExternalThreadID: "thread/1", MaximumAttempts: 3,
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			host := &acknowledgementRecoveryHost{provider: provider, acknowledged: map[string]string{}}
			workerClock := time.Now
			worker, err := externalconversation.NewExternalConversationDeliveryWorker(
				store,
				catalog,
				host,
				externalconversation.ExternalConversationDeliveryWorkerConfig{
					WorkerID: "delivery-worker", LeaseDuration: time.Minute,
					BaseRetry: time.Second, MaximumRetry: time.Minute,
				},
				models.WithClock(func() time.Time { return workerClock() }),
			)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Add(time.Second)
			workerClock = func() time.Time { return now }
			retry, err := worker.ProcessOne(ctx, endpoint.Scope)
			if err == nil || retry == nil || retry.Status != ExternalConversationDeliveryRetry || retry.Attempt != 1 {
				t.Fatalf("lost acknowledgement = %#v, %v", retry, err)
			}

			workerClock = func() time.Time { return now.Add(time.Second) }
			delivered, err := worker.ProcessOne(ctx, endpoint.Scope)
			if err != nil || delivered == nil || delivered.Status != ExternalConversationDeliveryDelivered ||
				delivered.ProviderMessageID != provider+"/message/1" || delivered.Attempt != 2 {
				t.Fatalf("recovered delivery = %#v, %v", delivered, err)
			}
			if host.deliveries != 1 || host.lookups != 1 {
				t.Fatalf("host calls: deliveries=%d lookups=%d", host.deliveries, host.lookups)
			}
			mapping, err := store.GetExternalMessageMapping(
				ctx, endpoint.Scope, endpoint.ID, ExternalMessageOutbound, delivered.ProviderMessageID,
			)
			if err != nil || mapping == nil || mapping.ConversationID != conversation.ID ||
				mapping.ChannelMessageID != reply.Message.ID {
				t.Fatalf("outbound mapping = %#v, %v", mapping, err)
			}
			stored, err := store.GetExternalConversationDelivery(ctx, endpoint.Scope, enqueued.Delivery.ID)
			if err != nil || stored.Status != ExternalConversationDeliveryDelivered {
				t.Fatalf("stored delivery = %#v, %v", stored, err)
			}
		})
	}
}

func externalConversationDeliveryFixture(
	t *testing.T,
	ctx context.Context,
	provider string,
) (*sqlite.Store, *skill.Catalog, *ExternalConversationEndpoint) {
	return externalConversationDeliveryFixtureWithOperations(t, ctx, provider, []skill.ConversationDeliveryOperation{skill.ConversationDeliveryMessageSend})
}

func externalConversationDeliveryFixtureWithOperations(
	t *testing.T,
	ctx context.Context,
	provider string,
	operations []skill.ConversationDeliveryOperation,
) (*sqlite.Store, *skill.Catalog, *ExternalConversationEndpoint) {
	t.Helper()
	store := newTestKernelStore(t)
	catalog, endpoint := externalConversationEndpointFixtureOn(t, ctx, store, provider, operations)
	return store, catalog, endpoint
}

// externalConversationEndpointFixtureOn creates the fixture endpoint in any
// endpoint store (SQLite or PostgreSQL).
func externalConversationEndpointFixtureOn(
	t *testing.T,
	ctx context.Context,
	store ExternalConversationEndpointStore,
	provider string,
	operations []skill.ConversationDeliveryOperation,
) (*skill.Catalog, *ExternalConversationEndpoint) {
	t.Helper()
	return externalConversationEndpointFixtureOwnedBy(t, ctx, store, provider, operations, ObjectiveOwner{Type: OwnerTypeAgent, ID: provider + "-agent"})
}

// externalConversationEndpointFixtureOwnedBy creates the fixture endpoint for
// an Agent or a Team owner.
func externalConversationEndpointFixtureOwnedBy(
	t *testing.T,
	ctx context.Context,
	store ExternalConversationEndpointStore,
	provider string,
	operations []skill.ConversationDeliveryOperation,
	owner ObjectiveOwner,
) (*skill.Catalog, *ExternalConversationEndpoint) {
	t.Helper()
	catalog := skill.NewCatalog()
	definition := skillbindingtest.SlackConversationSkillDefinition()
	definition.ID, definition.Name = provider, provider
	adapter := definition.ConversationAdapters["conversations"]
	adapter.Delivery.Operations = append([]skill.ConversationDeliveryOperation(nil), operations...)
	adapter.Provider = provider
	adapter.Name = provider + " conversations"
	adapter.Transport.IngressEndpoint = provider + ".conversation.ingress"
	adapter.Transport.DeliveryEndpoint = provider + ".conversation.deliver"
	if provider == "webchat" {
		adapter.Credentials = nil
		adapter.Transport.DeliveryCredentials = nil
	}
	definition.ConversationAdapters["conversations"] = adapter
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: provider}
	seedTestSpace(t, store.(SpaceStore), scope)
	deploymentID := owner.ID
	handler := ExternalConversationHandler{Kind: ExternalConversationHandlerAgent, ID: deploymentID}
	if owner.Type == OwnerTypeTeam {
		handler.Kind = ExternalConversationHandlerTeam
	}
	binding := &skill.Binding{
		ID: provider, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: deploymentID,
		SkillID: definition.ID, SkillVersion: definition.Version,
		EnabledConversationAdapters: []string{"conversations"}, MaximumRisk: skill.RiskLevelRead, Revision: 1,
	}
	if provider == "slack" {
		binding.Credentials = map[string]skill.CredentialReference{
			"SLACK_CONNECTION": {Kind: "slack-oauth", ID: "connection://tenant/slack"},
		}
	}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	endpoint, err := externalconversation.NewExternalConversationEndpointService(store, catalog).Create(
		ctx,
		externalconversation.CreateExternalConversationEndpointRequest{
			ID: provider + "-endpoint", Scope: scope,
			Owner: owner, DeploymentID: deploymentID,
			Name: provider + " endpoint",
			Adapter: ExternalConversationAdapterReference{
				SkillID: definition.ID, SkillVersion: definition.Version,
				BindingID: binding.ID, BindingRevision: binding.Revision, AdapterID: "conversations",
			},
			Mode: capability.ConversationEndpointChannel, Address: provider + "-address",
			Handler: handler,
			Policy: ExternalConversationPolicy{
				MessageSelection: ExternalConversationSelectAllMessages,
				ReplyMode:        ExternalConversationReplyThread, IgnoreBots: true,
			},
			Status: ExternalConversationEndpointActive,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return catalog, endpoint
}
