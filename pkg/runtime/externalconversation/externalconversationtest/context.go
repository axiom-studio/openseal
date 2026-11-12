package externalconversationtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// ContextHistoryResolver resolves adapter bindings through the embedded
// resolver and adds the context history feature to every adapter.
type ContextHistoryResolver struct {
	externalconversation.ExternalConversationAdapterResolver
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
	requests  []externalconversation.ExternalConversationContextHostRequest
	Result    *externalconversation.ExternalConversationContextResult
	err       error
	AfterRead func()
}

func (h *RecordingContextHost) ReadExternalConversationContext(_ context.Context, req externalconversation.ExternalConversationContextHostRequest) (*externalconversation.ExternalConversationContextResult, error) {
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
func NewContextWorker(t *testing.T, store *sqlite.Store, resolver externalconversation.ExternalConversationAdapterResolver, host externalconversation.ExternalConversationContextHost, crash bool, opts ...models.ServiceOption) (*externalconversation.ExternalConversationInboxWorker, *CrashAfterDispatch) {
	t.Helper()
	scheduler, err := conversationrun.NewConversationRunScheduler(store, store, conversationrun.ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &CrashAfterDispatch{Next: externalconversation.NewCanonicalExternalConversationDispatcher(scheduler), CrashOnce: crash}
	worker, err := externalconversation.NewExternalConversationInboxWorker(store, dispatcher, externalconversation.ExternalConversationInboxWorkerConfig{
		WorkerID: "context-worker", BaseRetry: time.Second, MaximumRetry: time.Minute,
		ContextHost: host, ContextCatalog: resolver,
	}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return worker, dispatcher
}

// CrashAfterDispatch dispatches through Next and, with CrashOnce, fails the
// first dispatch after its Run is durable, as a crash would. It counts the
// dispatches and keeps the last Run dispatched.
type CrashAfterDispatch struct {
	Next       externalconversation.ExternalConversationDispatcher
	CrashOnce  bool
	Dispatches int
	RunID      string
}

func (d *CrashAfterDispatch) DispatchExternalConversation(
	ctx context.Context,
	req externalconversation.ExternalConversationDispatchRequest,
) (*externalconversation.ExternalConversationDispatchResult, error) {
	d.Dispatches++
	result, err := d.Next.DispatchExternalConversation(ctx, req)
	if err != nil {
		return nil, err
	}
	d.RunID = result.RunID
	if d.CrashOnce {
		d.CrashOnce = false
		return nil, errors.New("synthetic crash after durable Run dispatch")
	}
	return result, nil
}
