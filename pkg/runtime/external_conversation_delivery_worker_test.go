package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type acknowledgementRecoveryHost struct {
	provider     string
	acknowledged map[string]string
	deliveries   int
	lookups      int
}

func (h *acknowledgementRecoveryHost) LookupExternalConversationDelivery(
	_ context.Context,
	req ExternalConversationDeliveryHostRequest,
) (*ExternalConversationDeliveryAcknowledgement, error) {
	h.lookups++
	if req.Adapter.Adapter.Provider != h.provider || req.Endpoint.Provider != h.provider {
		return nil, errors.New("wrong provider adapter")
	}
	providerMessageID := h.acknowledged[req.Delivery.IdempotencyKey]
	if providerMessageID == "" {
		return &ExternalConversationDeliveryAcknowledgement{Status: ExternalConversationAcknowledgementNotFound}, nil
	}
	return &ExternalConversationDeliveryAcknowledgement{
		Status: ExternalConversationAcknowledgementFound, ProviderMessageID: providerMessageID,
	}, nil
}

func (h *acknowledgementRecoveryHost) DeliverExternalConversation(
	_ context.Context,
	req ExternalConversationDeliveryHostRequest,
) (*ExternalConversationDeliveryHostResult, error) {
	h.deliveries++
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

func TestExternalConversationDeliveryWorkerUsesOneHostContractAndRecoversAcknowledgement(t *testing.T) {
	for _, provider := range []string{"slack", "webchat"} {
		t.Run(provider, func(t *testing.T) {
			ctx := context.Background()
			store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, provider)
			conversations := NewConversationService(store)
			conversation, _, err := conversations.CreateConversation(ctx, CreateConversationRequest{
				Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "External support",
				Origin: &ConversationReference{
					Kind: ConversationReferenceExternalSource, ID: endpoint.ID, Version: endpoint.Revision,
				},
				IdempotencyKey: "external-support",
			})
			if err != nil {
				t.Fatal(err)
			}
			reply, err := conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
				Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
				Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: endpoint.Owner.ID},
				Intent: MessageIntentAnswer, Content: "Canonical answer",
				Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "answer",
			})
			if err != nil {
				t.Fatal(err)
			}
			enqueued, err := NewExternalConversationTransportService(store, catalog).Enqueue(
				ctx,
				EnqueueExternalConversationDeliveryRequest{
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
			worker, err := NewExternalConversationDeliveryWorker(
				store,
				catalog,
				host,
				ExternalConversationDeliveryWorkerConfig{
					WorkerID: "delivery-worker", LeaseDuration: time.Minute,
					BaseRetry: time.Second, MaximumRetry: time.Minute,
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Add(time.Second)
			worker.now = func() time.Time { return now }
			retry, err := worker.ProcessOne(ctx, endpoint.Scope)
			if err == nil || retry == nil || retry.Status != ExternalConversationDeliveryRetry || retry.Attempt != 1 {
				t.Fatalf("lost acknowledgement = %#v, %v", retry, err)
			}

			worker.now = func() time.Time { return now.Add(time.Second) }
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
) (*MemoryStore, *skill.Catalog, *ExternalConversationEndpoint) {
	t.Helper()
	store := NewMemoryStore()
	catalog := skill.NewCatalog()
	definition := slackConversationSkillDefinition()
	definition.ID, definition.Name = provider, provider
	adapter := definition.ConversationAdapters["conversations"]
	adapter.Provider = provider
	adapter.Name = provider + " conversations"
	adapter.Transport.IngressEndpoint = provider + ".conversation.ingress"
	adapter.Transport.DeliveryEndpoint = provider + ".conversation.deliver"
	if provider == "webchat" {
		adapter.Credentials = []skill.CredentialRequirement{}
	}
	definition.ConversationAdapters["conversations"] = adapter
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: provider}
	deploymentID := provider + "-agent"
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
	endpoint, err := NewExternalConversationEndpointService(store, catalog).Create(
		ctx,
		CreateExternalConversationEndpointRequest{
			ID: provider + "-endpoint", Scope: scope,
			Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: deploymentID}, DeploymentID: deploymentID,
			Name: provider + " endpoint",
			Adapter: ExternalConversationAdapterReference{
				SkillID: definition.ID, SkillVersion: definition.Version,
				BindingID: binding.ID, BindingRevision: binding.Revision, AdapterID: "conversations",
			},
			Mode: capability.ConversationEndpointChannel, Address: provider + "-address",
			Handler: ExternalConversationHandler{Kind: ExternalConversationHandlerAgent, ID: deploymentID},
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
	return store, catalog, endpoint
}
