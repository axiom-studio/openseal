// Runtime's own copies of the external conversation test helpers its tests
// share with the external conversation tests. They become
// externalconversationtest once that code has its own package.

package runtime

import (
	"bytes"
	"context"
	"io"
	"sort"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	runtimeactivity "github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

// StreamChatHost is a provider chat: sends post messages, updates edit them
// and deletes remove them.
type StreamChatHost struct {
	Messages  map[string]string
	Requests  []ExternalConversationDeliveryHostRequest
	rejectAll capability.ConversationDeliveryOperation
	next      int
}

func (h *StreamChatHost) LookupExternalConversationDelivery(context.Context, ExternalConversationDeliveryHostRequest) (*ExternalConversationDeliveryAcknowledgement, error) {
	return &ExternalConversationDeliveryAcknowledgement{Status: ExternalConversationAcknowledgementUnknown}, nil
}

func (h *StreamChatHost) DeliverExternalConversation(_ context.Context, request ExternalConversationDeliveryHostRequest) (*ExternalConversationDeliveryHostResult, error) {
	h.Requests = append(h.Requests, request)
	if request.Delivery.Operation == h.rejectAll {
		return &ExternalConversationDeliveryHostResult{Outcome: ExternalConversationDeliveryOutcomeFailed, ErrorCode: "message_not_found"}, nil
	}
	switch request.Delivery.Operation {
	case capability.ConversationDeliveryMessageSend:
		h.next++
		id := "chat-" + string(rune('0'+h.next))
		h.Messages[id] = request.Message.Content
		return &ExternalConversationDeliveryHostResult{Outcome: ExternalConversationDeliveryOutcomeDelivered, ProviderMessageID: id}, nil
	case capability.ConversationDeliveryMessageUpdate:
		id, _ := request.Delivery.Parameters["providerMessageId"].(string)
		h.Messages[id] = request.Message.Content
		return &ExternalConversationDeliveryHostResult{Outcome: ExternalConversationDeliveryOutcomeDelivered, ProviderMessageID: id}, nil
	case capability.ConversationDeliveryMessageDelete:
		id, _ := request.Delivery.Parameters["providerMessageId"].(string)
		delete(h.Messages, id)
		return &ExternalConversationDeliveryHostResult{Outcome: ExternalConversationDeliveryOutcomeDelivered, ProviderMessageID: id}, nil
	}
	return &ExternalConversationDeliveryHostResult{Outcome: ExternalConversationDeliveryOutcomeDelivered, ProviderMessageID: "status"}, nil
}

// FileContentStore keeps the one artifact content written to it in memory.
type FileContentStore struct {
	data  []byte
	scope models.Scope
	owner *models.ObjectiveOwner
	opens int
}

func (s *FileContentStore) Put(_ context.Context, req models.ArtifactContentWrite) (models.ArtifactStoredContent, error) {
	s.data, _ = io.ReadAll(req.Reader)
	s.scope = req.Scope
	s.owner = req.Owner
	return models.ArtifactStoredContent{ContentRef: "opaque-file", SizeBytes: int64(len(s.data)), Digest: conversationtest.Digest(string(s.data))}, nil
}

func (s *FileContentStore) Open(_ context.Context, scope models.Scope, _ string) (io.ReadCloser, error) {
	s.scope = scope
	s.opens++
	return io.NopCloser(bytes.NewReader(s.data)), nil
}

// FileConversationFixture is the Slack endpoint fixture with a catalog
// whose conversation adapter also supports attachments.
func FileConversationFixture(t *testing.T) (*sqlite.Store, *skill.Catalog, *models.ExternalConversationEndpoint) {
	t.Helper()
	store, original, endpoint := externalConversationDeliveryFixture(t, t.Context(), "slack")
	bound, err := original.ResolveConversationAdapterBinding(t.Context(), skill.ScopeReference{Kind: endpoint.Scope.Kind, ID: endpoint.Scope.ID}, endpoint.DeploymentID, endpoint.Adapter.BindingID, endpoint.Adapter.AdapterID)
	if err != nil {
		t.Fatal(err)
	}
	definition := skillbindingtest.SlackConversationSkillDefinition()
	definition.ID = "slack"
	definition.Name = "slack"
	adapter := definition.ConversationAdapters["conversations"]
	adapter.Features = append(adapter.Features, capability.ConversationFeatureAttachments)
	sort.Slice(adapter.Features, func(i, j int) bool { return adapter.Features[i] < adapter.Features[j] })
	definition.ConversationAdapters["conversations"] = adapter
	catalog := skill.NewCatalog()
	if err := catalog.Register(t.Context(), definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(t.Context(), bound.Binding); err != nil {
		t.Fatal(err)
	}
	return store, catalog, endpoint
}

// ContextHistoryResolver resolves adapter bindings through the embedded
// resolver and adds the context history feature to every adapter.
type ContextHistoryResolver struct {
	ExternalConversationAdapterResolver
}

func (r ContextHistoryResolver) ResolveConversationAdapterBinding(ctx context.Context, scope skill.ScopeReference, deploymentID, bindingID, adapterID string) (*skill.BoundConversationAdapter, error) {
	resolved, err := r.ExternalConversationAdapterResolver.ResolveConversationAdapterBinding(ctx, scope, deploymentID, bindingID, adapterID)
	if err != nil {
		return nil, err
	}
	copy := *resolved
	copy.Adapter.Features = append(append([]capability.ConversationAdapterFeature(nil), resolved.Adapter.Features...), capability.ConversationFeatureContextHistory)
	return &copy, nil
}

// RecordingContextHost records context history reads and answers each with
// Result and Err, calling AfterRead first when it is set.
type RecordingContextHost struct {
	requests  []ExternalConversationContextHostRequest
	Result    *ExternalConversationContextResult
	err       error
	AfterRead func()
}

func (h *RecordingContextHost) ReadExternalConversationContext(_ context.Context, req ExternalConversationContextHostRequest) (*ExternalConversationContextResult, error) {
	h.requests = append(h.requests, req)
	if h.AfterRead != nil {
		h.AfterRead()
	}
	return h.Result, h.err
}

// ReceiveContextEvent stores a pending inbox item for a message in thread
// threadID of the endpoint's channel that mentions the endpoint.
func ReceiveContextEvent(t *testing.T, store *sqlite.Store, endpoint *models.ExternalConversationEndpoint, id, messageID, threadID string, now time.Time) *models.ExternalConversationInboxItem {
	t.Helper()
	item := &models.ExternalConversationInboxItem{
		ID: id, Scope: endpoint.Scope, EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Event: models.NormalizedExternalConversationEvent{
			ID: id, Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "workspace/channel", ExternalThreadID: threadID, ExternalMessageID: messageID,
			ExternalParticipantID: "current-user", ParticipantDisplayName: "Kev", Text: "Which ones can you pick up?",
			MentionsEndpoint: true, OrderingKey: "workspace/channel:" + threadID, OccurredAt: now,
		},
		Status: models.ExternalConversationInboxPending, MaximumAttempts: 3, AvailableAt: now,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if _, _, err := store.ReceiveExternalConversationEvent(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	return item
}

// NewContextWorker is an inbox worker that reads context history from host
// and dispatches through a conversation run scheduler; with crash the first
// dispatch fails after its Run is durable.
func NewContextWorker(t *testing.T, store *sqlite.Store, resolver ExternalConversationAdapterResolver, host ExternalConversationContextHost, crash bool, opts ...models.ServiceOption) (*ExternalConversationInboxWorker, *crashAfterExternalConversationDispatch) {
	t.Helper()
	scheduler, err := conversationrun.NewConversationRunScheduler(store, store, conversationrun.ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &crashAfterExternalConversationDispatch{next: NewCanonicalExternalConversationDispatcher(scheduler), crashOnce: crash}
	worker, err := NewExternalConversationInboxWorker(store, dispatcher, ExternalConversationInboxWorkerConfig{
		WorkerID: "context-worker", BaseRetry: time.Second, MaximumRetry: time.Minute,
		ContextHost: host, ContextCatalog: resolver,
	}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return worker, dispatcher
}

// IngressHostStub records the ingress request and answers it with Result.
type IngressHostStub struct {
	Request ExternalConversationIngressHostRequest
	Result  *ExternalConversationIngressHostResult
}

// GatewayHostStub records the gateway request and answers it with Result.
type GatewayHostStub struct {
	Request ExternalConversationGatewayHostRequest
	Result  *ExternalConversationGatewayHostResult
}

// CreateActiveGateway creates the gateway paused and then activates it, as
// an operator does after reviewing the provider route.
func CreateActiveGateway(t *testing.T, ctx context.Context, service *ExternalConversationGatewayService, request CreateExternalConversationGatewayRequest) *models.ExternalConversationGatewayRegistration {
	t.Helper()
	request.Status = models.ExternalConversationGatewayPaused
	created, err := service.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	active := models.ExternalConversationGatewayActive
	activated, err := service.Update(ctx, request.Gateway.Scope, created.ID, UpdateExternalConversationGatewayRequest{
		ExpectedRevision: created.Revision, Status: &active,
		Actor: models.ActivityActor{Type: "test", ID: "operator"}, Reason: "activate reviewed provider route",
	})
	if err != nil {
		t.Fatal(err)
	}
	return activated
}

func (h *GatewayHostStub) NormalizeExternalConversationGateway(
	_ context.Context,
	request ExternalConversationGatewayHostRequest,
) (*ExternalConversationGatewayHostResult, error) {
	h.Request = request
	return h.Result, nil
}

func (h *IngressHostStub) NormalizeExternalConversation(
	_ context.Context,
	request ExternalConversationIngressHostRequest,
) (*ExternalConversationIngressHostResult, error) {
	h.Request = request
	return h.Result, nil
}

// TerminalReportingStore is a store with runs, their terminal report queue
// and conversations.
type TerminalReportingStore interface {
	models.RunCommandStore
	models.RunTerminalReportingStore
	models.ConversationStore
}

// TerminalReportingStoreOf is store as a TerminalReportingStore; the test
// fails when store has no terminal reporting queue.
func TerminalReportingStoreOf(t *testing.T, store interface{}) TerminalReportingStore {
	t.Helper()
	reporting, ok := store.(TerminalReportingStore)
	if !ok {
		t.Fatal("canonical persistent store lacks terminal reporting queue")
	}
	return reporting
}

// TerminalReportingRun creates a conversation with a person's request and a
// scheduled Run, created at at, that reports its completion or failure
// there.
func TerminalReportingRun(t *testing.T, store TerminalReportingStore, scope models.Scope, at time.Time) (*models.AgentRun, *models.Conversation, *models.ChannelMessage) {
	t.Helper()
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "report-agent"}
	conversations := runtimeconversation.NewConversationService(store)
	conversationtest.SeedSpace(t, store.(models.SpaceStore), scope)
	conversation, _, err := conversations.CreateConversation(t.Context(), runtimeconversation.CreateConversationRequest{Scope: scope, Owner: owner, Title: "Original request", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	trigger, err := conversations.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "requesting-user"}, Intent: models.MessageIntentQuestion,
		Content: "Send me the result here when the work finishes", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	key := uuid.NewString()
	commands := agentrun.NewRunCommandService(store, models.WithClock(func() time.Time { return at }))
	created, err := commands.CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Goal: "Report the completed background work", Source: models.RunSourceSchedule, IdempotencyKey: key,
		Context: map[string]interface{}{
			models.ConversationRunConversationIDContextKey: conversation.ID, models.ConversationRunTriggerIDContextKey: trigger.Message.ID,
			models.RunReportingRootRunIDContextKey: agentrun.RunIDForIdempotencyKey(scope, key), models.RunReportingMilestonesContextKey: []string{"completed", "failed"}, models.ScheduledTaskContextKey: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return created.Run, trigger.Conversation, trigger.Message
}

// CompleteTerminalReportingRun moves run to running and then completes it
// with a summary, at at.
func CompleteTerminalReportingRun(t *testing.T, store TerminalReportingStore, run *models.AgentRun, at time.Time) *models.AgentRun {
	t.Helper()
	activity := runtimeactivity.NewRunActivityService(store, store, models.WithClock(func() time.Time { return at }))
	running, _, err := activity.TransitionRun(t.Context(), run.Scope, run.ID, runtimeactivity.RunTransitionRequest{ExpectedRevision: run.Revision, Status: models.AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	terminal, _, err := activity.TransitionRun(t.Context(), run.Scope, run.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: models.AgentRunStatusCompleted, Output: map[string]interface{}{"summary": "The requested result is ready"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return terminal
}

// TerminalReportingWorker is a terminal reporting worker on store whose
// clock reads at.
func TerminalReportingWorker(t *testing.T, store TerminalReportingStore, at time.Time) *RunTerminalReportingWorker {
	t.Helper()
	return TerminalReportingWorkerWithClock(t, store, func() time.Time { return at })
}

// TerminalReportingWorkerWithClock is TerminalReportingWorker reading time
// from clock.
func TerminalReportingWorkerWithClock(t *testing.T, store TerminalReportingStore, clock models.Clock) *RunTerminalReportingWorker {
	t.Helper()
	worker, err := NewRunTerminalReportingWorker(store, RunTerminalReportingWorkerOptions{WorkerID: "report-worker", LeaseDuration: time.Minute, BatchSize: 10, Concurrency: 2}, models.WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

// AssertTerminalReport checks that the conversation holds the trigger and
// exactly one report of run, posted to the channel and referencing run.
func AssertTerminalReport(t *testing.T, store models.ConversationStore, run *models.AgentRun, conversation *models.Conversation, trigger *models.ChannelMessage) {
	t.Helper()
	messages, err := store.ListChannelMessages(t.Context(), models.ChannelMessageFilter{Scope: run.Scope, ConversationID: conversation.ID, Limit: 100})
	if err != nil || len(messages) != 2 {
		t.Fatalf("terminal result lost or duplicated: messages=%#v error=%v", messages, err)
	}
	var report *models.ChannelMessage
	for _, message := range messages {
		if message.ID != trigger.ID {
			report = message
		}
	}
	if report == nil || report.Content != "The requested result is ready" || report.ReplyToMessageID != "" || report.ThreadRootID != "" || len(report.References) != 1 || report.References[0].ID != run.ID {
		t.Fatalf("result missed the original chat/context: %#v", report)
	}
}
