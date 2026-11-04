package runtime

import (
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

type routineReportFixture struct {
	store        *MemoryStore
	endpoint     *ExternalConversationEndpoint
	conversation *Conversation
	source       *ChannelMessage
	worker       *RunTerminalReportingWorker
}

// newRoutineReportFixture mirrors a Slack DM whose user message created a
// routine: the Conversation, its provider mappings and the creating message.
// Without mapSource the creating message has no inbound provider mapping.
func newRoutineReportFixture(t *testing.T, threadID string, mapSource bool) *routineReportFixture {
	t.Helper()
	ctx := t.Context()
	store, catalog, endpoint := externalConversationDeliveryFixtureWithOperations(t, ctx, "slack", []capability.ConversationDeliveryOperation{
		capability.ConversationDeliveryMessageSend,
	})
	conversations := NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(ctx, CreateConversationRequest{
		Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "Slack DM",
		Origin:         &ConversationReference{Kind: ConversationReferenceExternalSource, ID: endpoint.ID, Version: endpoint.Revision},
		IdempotencyKey: "routine-report-conversation",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	threads := []string{""}
	if threadID != "" {
		threads = append(threads, threadID)
	}
	for _, thread := range threads {
		if err := store.SaveExternalConversationMapping(ctx, &ExternalConversationMapping{Scope: endpoint.Scope, EndpointID: endpoint.ID,
			ExternalConversationID: "D123", ExternalThreadID: thread, ConversationID: conversation.ID, Revision: 1, CreatedAt: now, UpdatedAt: now}, 0); err != nil {
			t.Fatal(err)
		}
	}
	source, err := conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender:         ConversationParticipant{Type: ConversationParticipantUser, ID: "U123"},
		ExternalSource: &ExternalMessageSource{Provider: "slack", ChannelID: "D123", ChannelType: "im", ThreadID: threadID, MessageID: "171.001", ParticipantID: "U123"},
		Intent:         MessageIntentQuestion, Content: "Send me a news digest every morning at 7.",
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: "routine-report-source",
	})
	if err != nil {
		t.Fatal(err)
	}
	if mapSource {
		if err := store.SaveExternalMessageMapping(ctx, &ExternalMessageMapping{Scope: endpoint.Scope, EndpointID: endpoint.ID, Direction: ExternalMessageInbound,
			ExternalMessageID: "171.001", ConversationID: conversation.ID, ChannelMessageID: source.Message.ID, Revision: 1, CreatedAt: now, UpdatedAt: now}, 0); err != nil {
			t.Fatal(err)
		}
	}
	worker, err := NewRunTerminalReportingWorker(store, RunTerminalReportingWorkerOptions{WorkerID: "routine-report", LeaseDuration: time.Minute, BatchSize: 1, Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.SetExternalConversationDelivery(catalog); err != nil {
		t.Fatal(err)
	}
	return &routineReportFixture{store: store, endpoint: endpoint, conversation: conversation, source: source.Message, worker: worker}
}

// occurrence is one finished routine occurrence Run reporting to the
// fixture's Conversation, as prepareActivationReporting configures it.
func (f *routineReportFixture) occurrence(id string, routine bool) *AgentRun {
	values := map[string]interface{}{
		conversationRunContextConversationID: f.conversation.ID, conversationRunContextTriggerID: f.source.ID, runReportingContextRootRunID: id,
		runReportingContextMilestones: []interface{}{string(runbook.ReportingCompleted), string(runbook.ReportingFailed)},
	}
	if routine {
		values[scheduledTaskContextKey] = true
	}
	now := time.Now().UTC()
	return &AgentRun{ID: id, Kind: RunKindAgentWork, Scope: f.endpoint.Scope, RootRunID: id, Owner: f.endpoint.Owner, AssignedAgentID: f.endpoint.DeploymentID,
		Goal: "Send the morning news digest", Source: RunSourceSchedule, Status: AgentRunStatusCompleted, Context: values,
		Output: map[string]interface{}{"summary": "Today's digest: three stories."}, Revision: 3, CreatedAt: now, UpdatedAt: now, CompletedAt: &now}
}

func (f *routineReportFixture) deliveries(t *testing.T) []*ExternalConversationDelivery {
	t.Helper()
	deliveries, err := f.store.ListExternalConversationDeliveries(t.Context(), ExternalConversationDeliveryFilter{Scope: f.endpoint.Scope, EndpointID: f.endpoint.ID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return deliveries
}

// A routine created from a Slack conversation posts each occurrence's report
// there as a new top-level message, even when it was created in a thread, and
// exactly once. A deferred workflow keeps answering where its request got a
// reply: that thread, or the reply-mode thread of a top-level request.
func TestRoutineReportReachesTheExternalConversationItWasCreatedFrom(t *testing.T) {
	for _, test := range []struct {
		name, thread, want string
		workflow           bool
	}{
		{"routine from a top-level message", "", "", false},
		{"routine from a thread", "170.500", "", false},
		{"workflow from a top-level message", "", "171.001", true},
		{"workflow from a thread", "170.500", "170.500", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRoutineReportFixture(t, test.thread, true)
			run := f.occurrence("routine-occurrence", true)
			if test.workflow {
				run.Context[deferredWorkflowContextKey] = true
			}
			for range 2 {
				if err := f.worker.project(t.Context(), run); err != nil {
					t.Fatal(err)
				}
			}
			report, err := f.store.FindChannelMessageByIdempotencyKey(t.Context(), run.Scope, f.conversation.ID, "run-reporting-terminal:"+run.ID+":"+string(run.Status))
			if err != nil || report == nil || report.Content != "Today's digest: three stories." {
				t.Fatalf("routine report was not posted: %#v %v", report, err)
			}
			if topLevel := report.ReplyToMessageID == "" && report.ThreadRootID == ""; topLevel == test.workflow {
				t.Fatalf("report placement in the Conversation = reply %q thread %q", report.ReplyToMessageID, report.ThreadRootID)
			}
			deliveries := f.deliveries(t)
			if len(deliveries) != 1 {
				t.Fatalf("deliveries = %d, want exactly one", len(deliveries))
			}
			delivery := deliveries[0]
			if delivery.Operation != capability.ConversationDeliveryMessageSend || delivery.ChannelMessageID != report.ID || delivery.ConversationID != f.conversation.ID ||
				delivery.ExternalConversationID != "D123" || delivery.ExternalThreadID != test.want || delivery.IdempotencyKey != routineReportDeliveryKey(report.ID) {
				t.Fatalf("routine report delivery = %#v", delivery)
			}
		})
	}
}

// Only routine reports are projected, and only to an active endpoint whose
// records prove the destination.
func TestRoutineReportDeliveryRequiresARoutineAndAnActiveProvenEndpoint(t *testing.T) {
	for _, test := range []struct {
		name    string
		routine bool
		change  func(*testing.T, *routineReportFixture)
	}{
		{"not a routine", false, nil},
		{"paused endpoint", true, func(t *testing.T, f *routineReportFixture) {
			paused := cloneExternalConversationEndpoint(f.endpoint)
			paused.Status, paused.Revision = ExternalConversationEndpointPaused, f.endpoint.Revision+1
			if err := f.store.UpdateExternalConversationEndpoint(t.Context(), paused, f.endpoint.Revision); err != nil {
				t.Fatal(err)
			}
		}},
		{"unmapped creating message", true, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRoutineReportFixture(t, "", test.name != "unmapped creating message")
			if test.change != nil {
				test.change(t, f)
			}
			if err := f.worker.project(t.Context(), f.occurrence("occurrence", test.routine)); err != nil {
				t.Fatal(err)
			}
			if deliveries := f.deliveries(t); len(deliveries) != 0 {
				t.Fatalf("report was delivered: %#v", deliveries)
			}
		})
	}
}
