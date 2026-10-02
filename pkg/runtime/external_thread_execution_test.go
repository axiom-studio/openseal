package runtime

import (
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestExternalThreadsKeepOneChatAndIndependentQueuedRuns(t *testing.T) {
	ctx := t.Context()
	store, _, endpoint := externalConversationDeliveryFixtureWithOperations(t, ctx, "slack", []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend})
	scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewExternalConversationInboxWorker(store, NewCanonicalExternalConversationDispatcher(scheduler, nil), ExternalConversationInboxWorkerConfig{WorkerID: "thread-inbox"})
	if err != nil {
		t.Fatal(err)
	}
	var applied []*ExternalConversationInboxItem
	for i, thread := range []string{"one", "two", "one"} {
		now := time.Now().UTC()
		id := []string{"root-one", "root-two", "followup-one"}[i]
		item := &ExternalConversationInboxItem{
			ID: id, Scope: endpoint.Scope, EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
			Event: NormalizedExternalConversationEvent{ID: id, Type: capability.ConversationEventMessageReceived, ExternalConversationID: "same-channel", ExternalThreadID: thread,
				ExternalMessageID: id, ExternalParticipantID: "human", Text: id, MentionsEndpoint: true, OrderingKey: "same-channel:" + thread, OccurredAt: now},
			Status: ExternalConversationInboxPending, MaximumAttempts: 3, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
		if _, _, err := store.ReceiveExternalConversationEvent(ctx, item); err != nil {
			t.Fatal(err)
		}
		result, err := worker.ProcessOne(ctx, endpoint.Scope)
		if err != nil || result == nil || result.Status != ExternalConversationInboxApplied {
			t.Fatalf("import=%#v err=%v", result, err)
		}
		applied = append(applied, result)
	}
	if applied[0].ConversationID != applied[1].ConversationID || applied[0].ConversationID != applied[2].ConversationID {
		t.Fatal("split channel into separate chats")
	}
	first, _ := store.GetAgentRun(ctx, endpoint.Scope, applied[0].RunID)
	other, _ := store.GetAgentRun(ctx, endpoint.Scope, applied[1].RunID)
	followup, _ := store.GetAgentRun(ctx, endpoint.Scope, applied[2].RunID)
	if first.Status != AgentRunStatusQueued || other.Status != AgentRunStatusQueued || followup.Status != AgentRunStatusQueued {
		t.Fatal("incoming message canceled previous work")
	}
	if first.ConcurrencyKey == other.ConcurrencyKey || first.ConcurrencyKey != followup.ConcurrencyKey {
		t.Fatal("thread execution keys do not match canonical roots")
	}
	for _, run := range []*AgentRun{first, other, followup} {
		if err := validateConversationRun(run); err != nil {
			t.Fatal(err)
		}
	}
	claim := AgentRunClaim{Scope: endpoint.Scope, Kind: RunKindConversation, WorkerID: "thread-worker", Now: time.Now().UTC().Add(time.Second), LeaseDuration: time.Minute, AgingInterval: time.Minute, MaxActiveForConcurrencyKey: 1}
	a, err := store.ClaimNextAgentRun(ctx, claim)
	if err != nil || a == nil {
		t.Fatalf("first claim=%#v %v", a, err)
	}
	b, err := store.ClaimNextAgentRun(ctx, claim)
	if err != nil || b == nil || b.ConcurrencyKey == a.ConcurrencyKey {
		t.Fatalf("independent thread blocked=%#v %v", b, err)
	}
	if blocked, err := store.ClaimNextAgentRun(ctx, claim); err != nil || blocked != nil {
		t.Fatalf("same thread ran concurrently: %#v %v", blocked, err)
	}
	viewer := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: endpoint.Owner.ID}}
	root, _ := first.Context["threadRootMessageId"].(string)
	page, err := ReadConversationHistory(ctx, worker.conversations, endpoint.Scope, endpoint.Owner, applied[0].ConversationID, viewer, ConversationHistoryReadRequest{}, root)
	if err != nil || len(page.Messages) != 2 {
		t.Fatalf("thread history=%#v %v", page, err)
	}
	after := int64(0)
	forward, err := ReadConversationHistory(ctx, worker.conversations, endpoint.Scope, endpoint.Owner, applied[0].ConversationID, viewer, ConversationHistoryReadRequest{AfterSequence: &after, Limit: 1}, root)
	if err != nil || len(forward.Messages) != 1 || forward.Messages[0].ID != root || forward.NextAfterSequence == 0 {
		t.Fatalf("forward thread history=%#v %v", forward, err)
	}
	if _, err := ReadConversationHistory(ctx, worker.conversations, endpoint.Scope, endpoint.Owner, applied[0].ConversationID, viewer, ConversationHistoryReadRequest{MessageID: applied[1].ChannelMessageID}, root); err == nil {
		t.Fatal("explicit message lookup escaped its thread")
	}
	// Explicit cancellation remains available even though new messages no longer cancel.
	if _, err := scheduler.runs.CommandAgentRun(ctx, AgentRunCommandRequest{Scope: endpoint.Scope, RunID: a.ID, ExpectedRevision: a.Revision, Kind: AgentRunCommandCancel, Actor: ActivityActor{Type: "user", ID: "human"}}); err != nil {
		t.Fatal(err)
	}
	if next, err := store.ClaimNextAgentRun(ctx, claim); err != nil || next == nil || next.ConcurrencyKey != a.ConcurrencyKey {
		t.Fatalf("queued followup not released: %#v %v", next, err)
	}
}
