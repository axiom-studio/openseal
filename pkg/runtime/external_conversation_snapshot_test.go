package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type snapshotDeliveryHost struct {
	requests []ExternalConversationDeliveryHostRequest
}

func (h *snapshotDeliveryHost) LookupExternalConversationDelivery(context.Context, ExternalConversationDeliveryHostRequest) (*ExternalConversationDeliveryAcknowledgement, error) {
	return &ExternalConversationDeliveryAcknowledgement{Status: ExternalConversationAcknowledgementNotFound}, nil
}

func (h *snapshotDeliveryHost) DeliverExternalConversation(_ context.Context, req ExternalConversationDeliveryHostRequest) (*ExternalConversationDeliveryHostResult, error) {
	h.requests = append(h.requests, req)
	messageID := "provider-reply"
	if req.Delivery.Operation == capability.ConversationDeliveryTypingIndicator {
		messageID = req.Delivery.ExternalThreadID
	}
	return &ExternalConversationDeliveryHostResult{Outcome: ExternalConversationDeliveryOutcomeDelivered, ProviderMessageID: messageID}, nil
}

func upgradeSnapshotEndpoint(t *testing.T, store ExternalConversationEndpointStore, catalog *skill.Catalog, endpoint *ExternalConversationEndpoint) *ExternalConversationEndpoint {
	t.Helper()
	ctx := t.Context()
	adapter, err := catalog.ResolveConversationAdapterBinding(ctx, skill.ScopeReference{Kind: endpoint.Scope.Kind, ID: endpoint.Scope.ID}, endpoint.DeploymentID, endpoint.Adapter.BindingID, endpoint.Adapter.AdapterID)
	if err != nil {
		t.Fatal(err)
	}
	definition := *adapter.Definition
	definition.Version = "2.3.2"
	if err := catalog.Register(ctx, &definition); err != nil {
		t.Fatal(err)
	}
	binding := *adapter.Binding
	binding.SkillVersion = definition.Version
	binding.Revision++
	if err := catalog.Bind(ctx, &binding); err != nil {
		t.Fatal(err)
	}
	upgraded := cloneExternalConversationEndpoint(endpoint)
	upgraded.Revision++
	upgraded.UpdatedAt = upgraded.UpdatedAt.Add(time.Second)
	upgraded.Adapter.SkillVersion = definition.Version
	upgraded.Adapter.BindingRevision = binding.Revision
	// This fixture models an installation-wide connection. The destination is
	// retained in its durable inbox and outbox, not in a static endpoint address.
	upgraded.Address = ""
	if err := store.UpdateExternalConversationEndpoint(ctx, upgraded, endpoint.Revision); err != nil {
		t.Fatal(err)
	}
	return upgraded
}

func postSnapshotCanonicalReply(t *testing.T, store ConversationStore, endpoint *ExternalConversationEndpoint, item *ExternalConversationInboxItem) {
	t.Helper()
	conversations := NewConversationService(store)
	current, err := conversations.GetConversation(t.Context(), item.Scope, item.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conversations.PostChannelMessage(t.Context(), PostChannelMessageRequest{
		Scope: item.Scope, ConversationID: item.ConversationID, ExpectedRevision: current.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: endpoint.Owner.ID},
		Intent: MessageIntentAnswer, Content: "Canonical answer", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		ReplyToMessageID: item.ChannelMessageID, ResolvesMessageID: item.ChannelMessageID,
		References: []ConversationReference{{Kind: ConversationReferenceRun, ID: item.RunID}}, IdempotencyKey: "snapshot-canonical-reply",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestExternalConversationSnapshotSQLiteQueuedReplySurvivesUpgrade(t *testing.T) {
	ctx := t.Context()
	_, memoryCatalog, original, item := externalConversationFailureReplyFixture(t, "slack", AgentRunStatusCompleted)
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "snapshot-upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	resolved, err := memoryCatalog.ResolveConversationAdapterBinding(ctx, skill.ScopeReference{Kind: original.Scope.Kind, ID: original.Scope.ID}, original.DeploymentID, original.Adapter.BindingID, original.Adapter.AdapterID)
	if err != nil {
		t.Fatal(err)
	}
	catalog := skill.NewCatalogWithStore(store)
	if err := catalog.Register(ctx, resolved.Definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, resolved.Binding); err != nil {
		t.Fatal(err)
	}
	endpoint, err := NewExternalConversationEndpointService(store, catalog).Create(ctx, CreateExternalConversationEndpointRequest{
		ID: original.ID, Scope: original.Scope, Owner: original.Owner, DeploymentID: original.DeploymentID,
		Name: original.Name, Adapter: original.Adapter, Mode: original.Mode, Handler: original.Handler,
		Policy: original.Policy, Status: original.Status,
	})
	if err != nil {
		t.Fatal(err)
	}
	conversations := NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(ctx, CreateConversationRequest{
		ID: item.ConversationID, Scope: item.Scope, Owner: endpoint.Owner, Title: "Saved provider thread",
		Origin:         &ConversationReference{Kind: ConversationReferenceExternalSource, ID: endpoint.ID, Version: endpoint.Revision},
		IdempotencyKey: "sqlite-snapshot-conversation",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
		ID: item.ChannelMessageID, Scope: item.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "sender"}, Intent: MessageIntentQuestion,
		Content: item.Event.Text, Audience: ConversationAudience{Kind: ConversationAudienceChannel}, RequiresResponse: true,
		IdempotencyKey: "sqlite-snapshot-trigger",
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := NewRunCommandService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: item.Scope, Kind: RunKindConversation, Owner: endpoint.Owner,
		AssignedAgentID: endpoint.DeploymentID, Goal: "Respond", Source: RunSourceEvent,
		IdempotencyKey: "sqlite-snapshot-run", Actor: ActivityActor{Type: "service", ID: "test"}, Visibility: ActivityVisibilityPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	item.RunID = created.Run.ID
	activity := NewRunActivityService(store, store)
	running, _, err := activity.TransitionRun(ctx, item.Scope, created.Run.ID, RunTransitionRequest{
		ExpectedRevision: created.Run.Revision, Status: AgentRunStatusRunning, Summary: "Started", Actor: ActivityActor{Type: "worker", ID: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = activity.TransitionRun(ctx, item.Scope, running.ID, RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: AgentRunStatusCompleted, Summary: "Finished", Actor: ActivityActor{Type: "worker", ID: "test"},
		Output: map[string]interface{}{"reply": "Canonical answer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReceiveExternalConversationEvent(ctx, item); err != nil {
		t.Fatal(err)
	}
	projector, _ := NewExternalConversationReplyWorker(store, catalog)
	projected, err := projector.ProcessScope(ctx, item.Scope, 10)
	if err != nil || len(projected) != 1 {
		t.Fatalf("sqlite projection = %#v, %v", projected, err)
	}
	saved := projected[0]
	upgraded := upgradeSnapshotEndpoint(t, store, catalog, endpoint)
	host := &snapshotDeliveryHost{}
	worker, _ := NewExternalConversationDeliveryWorker(store, catalog, host, ExternalConversationDeliveryWorkerConfig{WorkerID: "sqlite-snapshot-worker"})
	for i := 0; i < 3; i++ {
		if _, err := worker.ProcessOne(ctx, item.Scope); err != nil {
			t.Fatal(err)
		}
	}
	if len(host.requests) != 2 || host.requests[0].Endpoint.Revision != upgraded.Revision || host.requests[0].Endpoint.Address != item.Event.ExternalConversationID {
		t.Fatalf("sqlite queued reply was stranded or redirected: %#v", host.requests)
	}
	durable, err := store.GetExternalConversationDelivery(ctx, item.Scope, saved.ID)
	if err != nil || durable.Status != ExternalConversationDeliveryDelivered || durable.EndpointRevision != saved.EndpointRevision || durable.Adapter != saved.Adapter {
		t.Fatalf("sqlite audit snapshot = %#v, %v", durable, err)
	}
	if _, err := projector.ProcessScope(ctx, item.Scope, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessOne(ctx, item.Scope); err != nil || len(host.requests) != 2 {
		t.Fatalf("sqlite upgrade replay duplicated delivery: count=%d, %v", len(host.requests), err)
	}
}

func TestExternalConversationSnapshotProjectsUnsentReplyAfterSkillUpgrade(t *testing.T) {
	ctx := t.Context()
	store, catalog, endpoint, item := externalConversationFailureReplyFixture(t, "slack", AgentRunStatusCompleted)
	postSnapshotCanonicalReply(t, store, endpoint, item)
	upgraded := upgradeSnapshotEndpoint(t, store, catalog, endpoint)
	worker, err := NewExternalConversationReplyWorker(store, catalog)
	if err != nil {
		t.Fatal(err)
	}
	deliveries, err := worker.ProcessScope(ctx, endpoint.Scope, 10)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("compatible upgraded projection = %#v, %v", deliveries, err)
	}
	delivery := deliveries[0]
	if delivery.EndpointRevision != upgraded.Revision || delivery.Adapter.SkillVersion != "2.3.2" ||
		delivery.ExternalConversationID != item.Event.ExternalConversationID || delivery.ExternalThreadID != item.Event.ExternalMessageID {
		t.Fatalf("upgrade changed reply destination or used stale authority: %#v", delivery)
	}
	if _, err := worker.ProcessScope(ctx, endpoint.Scope, 10); err != nil {
		t.Fatal(err)
	}
	all, _ := store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{Scope: endpoint.Scope, ConversationID: item.ConversationID, Limit: 10})
	if len(all) != 2 {
		t.Fatalf("projection replay duplicated outbox effects: %d", len(all))
	}
}

func TestExternalConversationSnapshotQueuedOutboxUsesCurrentAuthorityAndOriginalOrigin(t *testing.T) {
	ctx := t.Context()
	store, catalog, endpoint, item := externalConversationFailureReplyFixture(t, "slack", AgentRunStatusCompleted)
	postSnapshotCanonicalReply(t, store, endpoint, item)
	projector, _ := NewExternalConversationReplyWorker(store, catalog)
	projected, err := projector.ProcessScope(ctx, endpoint.Scope, 10)
	if err != nil || len(projected) != 1 {
		t.Fatalf("initial projection = %#v, %v", projected, err)
	}
	original := projected[0]
	upgraded := upgradeSnapshotEndpoint(t, store, catalog, endpoint)
	replayed, err := projector.ProcessScope(ctx, endpoint.Scope, 10)
	if err != nil || len(replayed) != 1 || replayed[0].ID != original.ID || replayed[0].Adapter != original.Adapter || replayed[0].EndpointRevision != original.EndpointRevision {
		t.Fatalf("upgrade replaced immutable receipt: %#v, %v", replayed, err)
	}
	host := &snapshotDeliveryHost{}
	deliveryWorker, err := NewExternalConversationDeliveryWorker(store, catalog, host, ExternalConversationDeliveryWorkerConfig{WorkerID: "snapshot-delivery-worker"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := deliveryWorker.ProcessOne(ctx, endpoint.Scope); err != nil {
			t.Fatal(err)
		}
	}
	if len(host.requests) != 2 {
		t.Fatalf("queued reply/status not delivered exactly once: %d", len(host.requests))
	}
	for _, request := range host.requests {
		if request.Endpoint.Revision != upgraded.Revision || request.Delivery.EndpointRevision != upgraded.Revision || request.Endpoint.Adapter != request.Delivery.Adapter ||
			request.Adapter.Binding.Revision != upgraded.Adapter.BindingRevision || request.Delivery.Adapter.SkillVersion != "2.3.2" ||
			request.Endpoint.Address != item.Event.ExternalConversationID || request.Delivery.ExternalConversationID != item.Event.ExternalConversationID || request.Delivery.ExternalThreadID != item.Event.ExternalMessageID {
			t.Fatalf("queued delivery did not use current authority and saved origin: %#v", request)
		}
	}
	durable, err := store.GetExternalConversationDelivery(ctx, endpoint.Scope, original.ID)
	if err != nil || durable.Status != ExternalConversationDeliveryDelivered || durable.Adapter != original.Adapter || durable.EndpointRevision != original.EndpointRevision {
		t.Fatalf("audit snapshot mutated: %#v, %v", durable, err)
	}
	if _, err := projector.ProcessScope(ctx, endpoint.Scope, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := deliveryWorker.ProcessOne(ctx, endpoint.Scope); err != nil || len(host.requests) != 2 {
		t.Fatalf("completed upgraded receipt replayed side effects: count=%d, %v", len(host.requests), err)
	}
}

func TestExternalConversationSnapshotRejectsRedirectAndNewAuthority(t *testing.T) {
	for _, queued := range []bool{false, true} {
		for _, scenario := range []string{"address", "binding", "source", "skill", "provider", "future-endpoint", "future-binding", "deployment"} {
			t.Run(scenario+map[bool]string{false: "-projection", true: "-outbox"}[queued], func(t *testing.T) {
				ctx := t.Context()
				store, catalog, endpoint, item := externalConversationFailureReplyFixture(t, "slack", AgentRunStatusCompleted)
				postSnapshotCanonicalReply(t, store, endpoint, item)
				projector, _ := NewExternalConversationReplyWorker(store, catalog)
				var delivery *ExternalConversationDelivery
				if queued {
					projected, err := projector.ProcessScope(ctx, endpoint.Scope, 10)
					if err != nil {
						t.Fatal(err)
					}
					delivery = projected[0]
				}
				upgraded := upgradeSnapshotEndpoint(t, store, catalog, endpoint)
				changed := cloneExternalConversationEndpoint(upgraded)
				switch scenario {
				case "address":
					changed.Address = "different-private-room"
				case "binding":
					changed.Adapter.BindingID = "another-account"
				case "source":
					changed.Adapter.SourceIdentity = "another-source"
				case "skill":
					changed.Adapter.SkillID = "another-skill"
				case "provider":
					changed.Provider = "telegram"
				case "deployment":
					changed.DeploymentID = "different-agent"
				case "future-endpoint":
					item.EndpointRevision = upgraded.Revision + 1
					if queued {
						delivery.EndpointRevision = upgraded.Revision + 1
					}
				case "future-binding":
					item.Adapter.BindingRevision = upgraded.Adapter.BindingRevision + 1
					if queued {
						delivery.Adapter.BindingRevision = upgraded.Adapter.BindingRevision + 1
					}
				}
				if scenario != "future-endpoint" && scenario != "future-binding" {
					changed.Revision++
					changed.UpdatedAt = changed.UpdatedAt.Add(time.Second)
					if err := store.UpdateExternalConversationEndpoint(ctx, changed, upgraded.Revision); err != nil {
						// Provider and deployment are immutable endpoint identities,
						// and the store rejects those swaps before execution.
						if scenario == "provider" || scenario == "deployment" {
							return
						}
						t.Fatal(err)
					}
				}
				if queued {
					worker, _ := NewExternalConversationDeliveryWorker(store, catalog, &snapshotDeliveryHost{}, ExternalConversationDeliveryWorkerConfig{WorkerID: "negative-worker"})
					if _, _, err := worker.resolve(ctx, delivery); !errors.Is(err, ErrExternalConversationConflict) {
						t.Fatalf("changed authority accepted: %v", err)
					}
				} else {
					if _, err := projector.project(ctx, item); !errors.Is(err, ErrExternalConversationConflict) {
						t.Fatalf("changed authority accepted: %v", err)
					}
				}
			})
		}
	}
}
