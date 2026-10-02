package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type contextHistoryResolver struct {
	ExternalConversationAdapterResolver
}

func (r contextHistoryResolver) ResolveConversationAdapterBinding(ctx context.Context, scope skill.ScopeReference, deploymentID, bindingID, adapterID string) (*skill.BoundConversationAdapter, error) {
	resolved, err := r.ExternalConversationAdapterResolver.ResolveConversationAdapterBinding(ctx, scope, deploymentID, bindingID, adapterID)
	if err != nil {
		return nil, err
	}
	copy := *resolved
	copy.Adapter.Features = append(append([]capability.ConversationAdapterFeature(nil), resolved.Adapter.Features...), capability.ConversationFeatureContextHistory)
	return &copy, nil
}

type recordingContextHost struct {
	requests  []ExternalConversationContextHostRequest
	result    *ExternalConversationContextResult
	err       error
	afterRead func()
}

func (h *recordingContextHost) ReadExternalConversationContext(_ context.Context, req ExternalConversationContextHostRequest) (*ExternalConversationContextResult, error) {
	h.requests = append(h.requests, req)
	if h.afterRead != nil {
		h.afterRead()
	}
	return h.result, h.err
}

func receiveContextTestEvent(t *testing.T, store *MemoryStore, endpoint *ExternalConversationEndpoint, id, messageID, threadID string, now time.Time) *ExternalConversationInboxItem {
	t.Helper()
	item := &ExternalConversationInboxItem{
		ID: id, Scope: endpoint.Scope, EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter,
		Event: NormalizedExternalConversationEvent{
			ID: id, Type: capability.ConversationEventMessageReceived,
			ExternalConversationID: "workspace/channel", ExternalThreadID: threadID, ExternalMessageID: messageID,
			ExternalParticipantID: "current-user", ParticipantDisplayName: "Kev", Text: "Which ones can you pick up?",
			MentionsEndpoint: true, OrderingKey: "workspace/channel:" + threadID, OccurredAt: now,
		},
		Status: ExternalConversationInboxPending, MaximumAttempts: 3, AvailableAt: now,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if _, _, err := store.ReceiveExternalConversationEvent(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	return item
}

func contextTestHistory(event NormalizedExternalConversationEvent) *ExternalConversationContextResult {
	return &ExternalConversationContextResult{Status: ExternalConversationContextComplete, Messages: []ExternalConversationContextMessage{
		{ExternalConversationID: event.ExternalConversationID, ExternalThreadID: event.ExternalThreadID,
			ExternalMessageID: "earlier-reply", ExternalParticipantID: "human-two", ParticipantDisplayName: "Vishnu",
			Text: "The photo book is my favorite.", OccurredAt: event.OccurredAt.Add(-time.Minute)},
		{ExternalConversationID: event.ExternalConversationID, ExternalMessageID: event.ExternalThreadID,
			ExternalParticipantID: "human-one", ParticipantDisplayName: "Mahendra",
			Text: "Make a photo book, suggest recipes, or plan an event.", OccurredAt: event.OccurredAt.Add(-time.Hour)},
	}}
}

func newContextTestWorker(t *testing.T, store *MemoryStore, resolver ExternalConversationAdapterResolver, host ExternalConversationContextHost, crash bool) (*ExternalConversationInboxWorker, *crashAfterExternalConversationDispatch) {
	t.Helper()
	scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &crashAfterExternalConversationDispatch{next: NewCanonicalExternalConversationDispatcher(scheduler, nil), crashOnce: crash}
	worker, err := NewExternalConversationInboxWorker(store, dispatcher, ExternalConversationInboxWorkerConfig{
		WorkerID: "context-worker", BaseRetry: time.Second, MaximumRetry: time.Minute,
		ContextHost: host, ContextCatalog: resolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	return worker, dispatcher
}

func TestExternalConversationContextImportsParentAndRepliesBeforeFirstMention(t *testing.T) {
	ctx := t.Context()
	store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	now := time.Now().UTC().Truncate(time.Millisecond)
	item := receiveContextTestEvent(t, store, endpoint, "first-mention", "current-mention", "original-parent", now)
	host := &recordingContextHost{result: contextTestHistory(item.Event)}
	// Saving setup increments the binding revision without rewriting historical
	// event snapshots. Context must resolve the current authorized credentials.
	resolved, err := catalog.ResolveConversationAdapterBinding(ctx, skill.ScopeReference{Kind: endpoint.Scope.Kind, ID: endpoint.Scope.ID}, endpoint.DeploymentID, endpoint.Adapter.BindingID, endpoint.Adapter.AdapterID)
	if err != nil {
		t.Fatal(err)
	}
	nextBinding := *resolved.Binding
	nextBinding.Revision++
	if err := catalog.Bind(ctx, &nextBinding); err != nil {
		t.Fatal(err)
	}
	worker, dispatcher := newContextTestWorker(t, store, contextHistoryResolver{catalog}, host, true)
	worker.now = func() time.Time { return now }
	if _, err := worker.ProcessOne(ctx, endpoint.Scope); err == nil {
		t.Fatal("expected synthetic post-dispatch crash")
	}
	if len(host.requests) != 1 || host.requests[0].Endpoint.Address != item.Event.ExternalConversationID ||
		host.requests[0].Adapter.Binding.Revision != nextBinding.Revision || host.requests[0].Endpoint.Adapter.BindingRevision != nextBinding.Revision {
		t.Fatalf("provider context was not routed using the current binding: %#v", host.requests)
	}
	worker.now = func() time.Time { return now.Add(time.Second) }
	applied, err := worker.ProcessOne(ctx, endpoint.Scope)
	if err != nil || applied.Status != ExternalConversationInboxApplied || applied.RunID == "" {
		t.Fatalf("application = %#v, %v", applied, err)
	}
	messages, err := store.ListChannelMessages(ctx, ChannelMessageFilter{Scope: endpoint.Scope, ConversationID: applied.ConversationID, Limit: 20})
	if err != nil || len(messages) != 3 {
		t.Fatalf("history = %#v, %v", messages, err)
	}
	parent, reply, trigger := messages[0], messages[1], messages[2]
	if parent.Content != host.result.Messages[1].Text || !parent.Historical || parent.RequiresResponse || parent.ThreadRootID != "" ||
		parent.Sender.Type != ConversationParticipantUser || parent.SenderDisplayName != "Mahendra" ||
		!reply.Historical || reply.ThreadRootID != parent.ID || reply.ReplyToMessageID != parent.ID ||
		trigger.Historical || !trigger.RequiresResponse || trigger.ThreadRootID != parent.ID || trigger.ReplyToMessageID != parent.ID {
		t.Fatalf("parent/reply/trigger boundaries = %#v / %#v / %#v", parent, reply, trigger)
	}
	state := ExternalConversationContextAvailability(trigger, endpoint.ID)
	if state == nil || state.Status != ExternalConversationContextComplete || state.ImportedMessages != 2 || state.ExternalConversationID != item.Event.ExternalConversationID || state.ExternalThreadID != item.Event.ExternalThreadID {
		t.Fatalf("availability = %#v", state)
	}
	runs, _ := store.ListAgentRuns(ctx, AgentRunFilter{Scope: endpoint.Scope, Kind: RunKindConversation, Limit: 20})
	if len(runs) != 1 || len(host.requests) != 1 || dispatcher.dispatches != 2 {
		t.Fatalf("recovery duplicated context or work: runs=%d reads=%d dispatches=%d", len(runs), len(host.requests), dispatcher.dispatches)
	}
	// A delayed event for an imported historical message must remain context.
	receiveContextTestEvent(t, store, endpoint, "delayed-earlier-reply", "earlier-reply", item.Event.ExternalThreadID, now.Add(2*time.Second))
	worker.now = func() time.Time { return now.Add(2 * time.Second) }
	late, err := worker.ProcessOne(ctx, endpoint.Scope)
	if err != nil || late.RunID != "" || dispatcher.dispatches != 2 {
		t.Fatalf("delayed history woke work: %#v, %v", late, err)
	}
}

func TestExternalConversationContextRefreshesMissingRepliesAndKeepsThreadsSeparate(t *testing.T) {
	ctx := t.Context()
	store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	now := time.Now().UTC().Truncate(time.Millisecond)
	first := receiveContextTestEvent(t, store, endpoint, "first-thread-first-mention", "mention-one", "parent-one", now)
	host := &recordingContextHost{result: contextTestHistory(first.Event)}
	worker, _ := newContextTestWorker(t, store, contextHistoryResolver{catalog}, host, false)
	worker.now = func() time.Time { return now }
	applied, err := worker.ProcessOne(ctx, endpoint.Scope)
	if err != nil {
		t.Fatal(err)
	}
	firstMessages, _ := store.ListChannelMessages(ctx, ChannelMessageFilter{Scope: endpoint.Scope, ConversationID: applied.ConversationID, Limit: 20})
	firstRoot := firstMessages[0].ID
	second := receiveContextTestEvent(t, store, endpoint, "first-thread-next-mention", "mention-two", "parent-one", now.Add(time.Minute))
	host.result = contextTestHistory(first.Event)
	host.result.Messages = append(host.result.Messages, ExternalConversationContextMessage{
		ExternalConversationID: first.Event.ExternalConversationID, ExternalThreadID: first.Event.ExternalThreadID,
		ExternalMessageID: "missed-reply", ExternalParticipantID: "human-three", Text: "I can supply photos.", OccurredAt: now.Add(time.Second),
	})
	worker.now = func() time.Time { return second.Event.OccurredAt }
	if _, err := worker.ProcessOne(ctx, endpoint.Scope); err != nil {
		t.Fatal(err)
	}
	secondMessages, _ := store.ListChannelMessages(ctx, ChannelMessageFilter{Scope: endpoint.Scope, ConversationID: applied.ConversationID, ThreadRootID: firstRoot, Limit: 20})
	if len(secondMessages) != 5 || !secondMessages[3].Historical || secondMessages[3].Content != "I can supply photos." {
		t.Fatalf("missed context refresh = %#v", secondMessages)
	}
	other := receiveContextTestEvent(t, store, endpoint, "second-thread-mention", "mention-three", "parent-two", now.Add(2*time.Minute))
	host.result = contextTestHistory(other.Event)
	host.result.Messages[0].ExternalMessageID = "other-thread-reply"
	worker.now = func() time.Time { return other.Event.OccurredAt }
	otherApplied, err := worker.ProcessOne(ctx, endpoint.Scope)
	if err != nil {
		t.Fatal(err)
	}
	otherTrigger, err := store.GetChannelMessage(ctx, endpoint.Scope, otherApplied.ConversationID, otherApplied.ChannelMessageID)
	if err != nil {
		t.Fatal(err)
	}
	otherMessages, _ := store.ListChannelMessages(ctx, ChannelMessageFilter{Scope: endpoint.Scope, ConversationID: otherApplied.ConversationID, ThreadRootID: otherTrigger.ThreadRootID, Limit: 20})
	if applied.ConversationID != otherApplied.ConversationID || otherTrigger.ThreadRootID == firstRoot || len(otherMessages) != 3 {
		t.Fatalf("threads were not isolated within their channel: %#v", otherMessages)
	}
	for _, message := range otherMessages {
		if message.ThreadRootID == firstRoot || message.ID == firstRoot || message.Content == "I can supply photos." {
			t.Fatal("another thread's context leaked")
		}
	}
}

func TestExternalConversationContextUnthreadedMentionDoesNotReadWholeChannel(t *testing.T) {
	store, catalog, endpoint := externalConversationDeliveryFixture(t, t.Context(), "slack")
	now := time.Now().UTC().Truncate(time.Millisecond)
	receiveContextTestEvent(t, store, endpoint, "unthreaded-mention", "mention", "", now)
	host := &recordingContextHost{err: errors.New("must not read the entire channel")}
	worker, _ := newContextTestWorker(t, store, contextHistoryResolver{catalog}, host, false)
	worker.now = func() time.Time { return now }
	applied, err := worker.ProcessOne(t.Context(), endpoint.Scope)
	if err != nil || applied.RunID == "" || len(host.requests) != 0 {
		t.Fatalf("unthreaded read = %#v, reads=%d, %v", applied, len(host.requests), err)
	}
	message, _ := store.GetChannelMessage(t.Context(), endpoint.Scope, applied.ConversationID, applied.ChannelMessageID)
	state := ExternalConversationContextAvailability(message, endpoint.ID)
	if state == nil || state.Status != ExternalConversationContextUnavailable || state.ExternalConversationID != "workspace/channel" || state.ExternalThreadID != "" {
		t.Fatalf("unthreaded origin = %#v", state)
	}
}

func TestExternalConversationContextReadFailureDoesNotHideTrigger(t *testing.T) {
	store, catalog, endpoint := externalConversationDeliveryFixture(t, t.Context(), "webchat")
	now := time.Now().UTC().Truncate(time.Millisecond)
	receiveContextTestEvent(t, store, endpoint, "unavailable-context", "mention", "parent", now)
	host := &recordingContextHost{err: errors.New("read permission unavailable")}
	worker, _ := newContextTestWorker(t, store, contextHistoryResolver{catalog}, host, false)
	worker.now = func() time.Time { return now }
	applied, err := worker.ProcessOne(t.Context(), endpoint.Scope)
	if err != nil || applied.RunID == "" {
		t.Fatalf("trigger did not proceed: %#v, %v", applied, err)
	}
	messages, _ := store.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: endpoint.Scope, ConversationID: applied.ConversationID, Limit: 10})
	if len(messages) != 1 {
		t.Fatalf("unexpected history count: %d", len(messages))
	}
	state := ExternalConversationContextAvailability(messages[0], endpoint.ID)
	if state == nil || state.Status != ExternalConversationContextUnavailable || state.ImportedMessages != 0 {
		t.Fatalf("permission gap was reported as empty history: %#v", state)
	}
}

func TestExternalConversationContextPermissionCodeIsSafeAndPreserved(t *testing.T) {
	for _, code := range []string{"missing_scope", "not_in_channel", "raw-secret-or-provider-response"} {
		t.Run(code, func(t *testing.T) {
			store, catalog, endpoint := externalConversationDeliveryFixture(t, t.Context(), "slack")
			now := time.Now().UTC().Truncate(time.Millisecond)
			receiveContextTestEvent(t, store, endpoint, "permission-context", "mention", "parent", now)
			worker, _ := newContextTestWorker(t, store, contextHistoryResolver{catalog}, &recordingContextHost{
				result: &ExternalConversationContextResult{Status: ExternalConversationContextUnavailable, ErrorCode: code},
			}, false)
			worker.now = func() time.Time { return now }
			applied, err := worker.ProcessOne(t.Context(), endpoint.Scope)
			if err != nil {
				t.Fatal(err)
			}
			message, _ := store.GetChannelMessage(t.Context(), endpoint.Scope, applied.ConversationID, applied.ChannelMessageID)
			state := ExternalConversationContextAvailability(message, endpoint.ID)
			if state == nil || state.ErrorCode != normalizeExternalConversationContextErrorCode(code) {
				t.Fatalf("permission availability = %#v", state)
			}
			for _, reference := range message.References {
				if strings.Contains(reference.ID, "raw-secret") {
					t.Fatal("raw provider error leaked into canonical metadata")
				}
			}
		})
	}
}

func TestExternalConversationContextBindingChangeDuringReadDoesNotImportStaleData(t *testing.T) {
	ctx := t.Context()
	store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	now := time.Now().UTC().Truncate(time.Millisecond)
	item := receiveContextTestEvent(t, store, endpoint, "binding-changed-during-read", "mention", "parent", now)
	host := &recordingContextHost{result: contextTestHistory(item.Event), afterRead: func() {
		resolved, err := catalog.ResolveConversationAdapterBinding(ctx, skill.ScopeReference{Kind: endpoint.Scope.Kind, ID: endpoint.Scope.ID}, endpoint.DeploymentID, endpoint.Adapter.BindingID, endpoint.Adapter.AdapterID)
		if err != nil {
			t.Fatal(err)
		}
		binding := *resolved.Binding
		binding.Revision++
		if err := catalog.Bind(ctx, &binding); err != nil {
			t.Fatal(err)
		}
	}}
	worker, _ := newContextTestWorker(t, store, contextHistoryResolver{catalog}, host, false)
	worker.now = func() time.Time { return now }
	applied, err := worker.ProcessOne(ctx, endpoint.Scope)
	if err != nil || applied.RunID == "" {
		t.Fatalf("trigger = %#v, %v", applied, err)
	}
	messages, _ := store.ListChannelMessages(ctx, ChannelMessageFilter{Scope: endpoint.Scope, ConversationID: applied.ConversationID, Limit: 20})
	if len(messages) != 1 {
		t.Fatalf("stale binding context was imported: %d messages", len(messages))
	}
	state := ExternalConversationContextAvailability(messages[0], endpoint.ID)
	if state == nil || state.Status != ExternalConversationContextUnavailable {
		t.Fatalf("stale read availability = %#v", state)
	}
}

func TestExternalConversationContextAvailabilityKeepsLongCanonicalOrigin(t *testing.T) {
	state := &ExternalConversationContextState{Status: ExternalConversationContextComplete, ImportedMessages: 1}
	message := &ChannelMessage{ExternalSource: &ExternalMessageSource{ChannelID: strings.Repeat("c", 1024), ThreadID: strings.Repeat("t", 1024)},
		References: []ConversationReference{externalConversationContextReference("endpoint", state)}}
	actual := ExternalConversationContextAvailability(message, "endpoint")
	if actual == nil || actual.ExternalConversationID != message.ExternalSource.ChannelID || actual.ExternalThreadID != message.ExternalSource.ThreadID {
		t.Fatalf("long origin was truncated: %#v", actual)
	}
}

func TestExternalConversationContextAvailabilityPrefersCanonicalSource(t *testing.T) {
	state := &ExternalConversationContextState{Status: ExternalConversationContextComplete,
		ExternalConversationID: "old-channel", ExternalThreadID: "old-thread"}
	message := &ChannelMessage{ExternalSource: &ExternalMessageSource{ChannelID: "actual-channel", ThreadID: "actual-thread"},
		References: []ConversationReference{externalConversationContextReference("endpoint", state), *externalConversationContextOriginReference("endpoint", state)}}
	actual := ExternalConversationContextAvailability(message, "endpoint")
	if actual == nil || actual.ExternalConversationID != "actual-channel" || actual.ExternalThreadID != "actual-thread" {
		t.Fatalf("marker overrode canonical provenance: %#v", actual)
	}
}

func TestExternalConversationContextRejectsCrossDestinationAndOversizedResults(t *testing.T) {
	for _, name := range []string{"other-channel", "other-thread", "oversized-message", "oversized-batch", "oversized-total", "conflicting-duplicate"} {
		t.Run(name, func(t *testing.T) {
			store, catalog, endpoint := externalConversationDeliveryFixture(t, t.Context(), "slack")
			now := time.Now().UTC().Truncate(time.Millisecond)
			item := receiveContextTestEvent(t, store, endpoint, "isolated-context", "mention", "parent", now)
			result := contextTestHistory(item.Event)
			switch name {
			case "other-channel":
				result.Messages[0].ExternalConversationID = "private-dm"
			case "other-thread":
				result.Messages[0].ExternalThreadID = "unrelated-thread"
			case "oversized-message":
				result.Messages[0].Text = strings.Repeat("x", 64*1024+1)
			case "oversized-batch":
				result.Messages = make([]ExternalConversationContextMessage, MaximumExternalConversationContextMessages+1)
			case "oversized-total":
				for i := 0; i < 5; i++ {
					message := result.Messages[0]
					message.ExternalMessageID = strings.Repeat("x", i+1)
					message.Text = strings.Repeat("x", 64*1024)
					result.Messages = append(result.Messages, message)
				}
			case "conflicting-duplicate":
				message := result.Messages[0]
				message.Text = "conflicting source content"
				result.Messages = append(result.Messages, message)
			}
			worker, _ := newContextTestWorker(t, store, contextHistoryResolver{catalog}, &recordingContextHost{result: result}, false)
			worker.now = func() time.Time { return now }
			applied, err := worker.ProcessOne(t.Context(), endpoint.Scope)
			if err != nil || applied.RunID == "" {
				t.Fatalf("unsafe history blocked trigger: %#v, %v", applied, err)
			}
			messages, _ := store.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: endpoint.Scope, ConversationID: applied.ConversationID, Limit: 20})
			if len(messages) != 1 {
				t.Fatalf("unsafe context was partially imported: %d messages", len(messages))
			}
			state := ExternalConversationContextAvailability(messages[0], endpoint.ID)
			if state == nil || state.Status != ExternalConversationContextUnavailable {
				t.Fatalf("unsafe context availability = %#v", state)
			}
		})
	}
}

func TestExternalConversationContextFiltersFutureAndDeduplicatesHistory(t *testing.T) {
	event := NormalizedExternalConversationEvent{ExternalConversationID: "channel", ExternalThreadID: "parent", ExternalMessageID: "trigger", OccurredAt: time.Now().UTC()}
	result := contextTestHistory(event)
	result.Messages = append(result.Messages, result.Messages[0])
	future := result.Messages[0]
	future.ExternalMessageID = "future"
	future.OccurredAt = event.OccurredAt.Add(time.Second)
	trigger := future
	trigger.ExternalMessageID = event.ExternalMessageID
	trigger.OccurredAt = event.OccurredAt
	result.Messages = append(result.Messages, future, trigger)
	messages, status, valid := normalizeExternalConversationContext(result, event)
	if !valid || status != ExternalConversationContextPartial || len(messages) != 2 || messages[0].ExternalMessageID != "parent" {
		t.Fatalf("normalized history = %#v, %s, %v", messages, status, valid)
	}
}

func TestExternalConversationContextRequiresFeatureDeclaration(t *testing.T) {
	store, catalog, endpoint := externalConversationDeliveryFixture(t, t.Context(), "slack")
	now := time.Now().UTC().Truncate(time.Millisecond)
	receiveContextTestEvent(t, store, endpoint, "no-history-feature", "mention", "parent", now)
	host := &recordingContextHost{err: errors.New("must not be called")}
	worker, _ := newContextTestWorker(t, store, catalog, host, false)
	worker.now = func() time.Time { return now }
	applied, err := worker.ProcessOne(t.Context(), endpoint.Scope)
	if err != nil || applied.RunID == "" || len(host.requests) != 0 {
		t.Fatalf("undeclared provider history used: %#v, reads=%d, %v", applied, len(host.requests), err)
	}
}
