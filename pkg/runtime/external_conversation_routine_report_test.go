package runtime

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type routineReportFixture struct {
	store        *sqlite.Store
	endpoint     *models.ExternalConversationEndpoint
	conversation *models.Conversation
	source       *models.ChannelMessage
	worker       *RunTerminalReportingWorker
	catalog      *skill.Catalog
}

// newRoutineReportFixture mirrors a Slack DM whose user message created a
// routine: the Conversation, its provider mappings and the creating message.
// Without mapSource the creating message has no inbound provider mapping.
func newRoutineReportFixture(t *testing.T, threadID string, mapSource bool) *routineReportFixture {
	t.Helper()
	ctx := t.Context()
	store, catalog, endpoint := endpointFixtureWithOperations(t, ctx, "slack", []capability.ConversationDeliveryOperation{
		capability.ConversationDeliveryMessageSend,
	})
	conversations := runtimeconversation.NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{
		Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "Slack DM",
		Origin:         &models.ConversationReference{Kind: models.ConversationReferenceExternalSource, ID: endpoint.ID, Version: endpoint.Revision},
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
		if err := store.SaveExternalConversationMapping(ctx, &models.ExternalConversationMapping{Scope: endpoint.Scope, EndpointID: endpoint.ID,
			ExternalConversationID: "D123", ExternalThreadID: thread, ConversationID: conversation.ID, Revision: 1, CreatedAt: now, UpdatedAt: now}, 0); err != nil {
			t.Fatal(err)
		}
	}
	source, err := conversations.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: endpoint.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender:         models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "U123"},
		ExternalSource: &models.ExternalMessageSource{Provider: "slack", ChannelID: "D123", ChannelType: "im", ThreadID: threadID, MessageID: "171.001", ParticipantID: "U123"},
		Intent:         models.MessageIntentQuestion, Content: "Send me a news digest every morning at 7.",
		Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: "routine-report-source",
	})
	if err != nil {
		t.Fatal(err)
	}
	if mapSource {
		if err := store.SaveExternalMessageMapping(ctx, &models.ExternalMessageMapping{Scope: endpoint.Scope, EndpointID: endpoint.ID, Direction: models.ExternalMessageInbound,
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
	return &routineReportFixture{store: store, endpoint: endpoint, conversation: conversation, source: source.Message, worker: worker, catalog: catalog}
}

// occurrence is one finished routine occurrence Run reporting to the
// fixture's Conversation, as prepareActivationReporting configures it.
func (f *routineReportFixture) occurrence(id string, routine bool) *models.AgentRun {
	values := map[string]interface{}{
		models.ConversationRunConversationIDContextKey: f.conversation.ID, models.ConversationRunTriggerIDContextKey: f.source.ID, models.RunReportingRootRunIDContextKey: id,
		models.RunReportingMilestonesContextKey: []interface{}{string(runbook.ReportingCompleted), string(runbook.ReportingFailed)},
	}
	if routine {
		values[models.ScheduledTaskContextKey] = true
	}
	now := time.Now().UTC()
	return &models.AgentRun{ID: id, Kind: models.RunKindAgentWork, Scope: f.endpoint.Scope, RootRunID: id, Owner: f.endpoint.Owner, AssignedAgentID: f.endpoint.DeploymentID,
		Goal: "Send the morning news digest", Source: models.RunSourceSchedule, Status: models.AgentRunStatusCompleted, Context: values,
		Output: map[string]interface{}{"summary": "Today's digest: three stories."}, Revision: 3, CreatedAt: now, UpdatedAt: now, CompletedAt: &now}
}

func (f *routineReportFixture) deliveries(t *testing.T) []*models.ExternalConversationDelivery {
	t.Helper()
	deliveries, err := f.store.ListExternalConversationDeliveries(t.Context(), models.ExternalConversationDeliveryFilter{Scope: f.endpoint.Scope, EndpointID: f.endpoint.ID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return deliveries
}

// A routine created from a Slack conversation posts each occurrence's report
// there as a new top-level message, even when it was created in a thread, and
// exactly once.
func TestRoutineReportReachesTheExternalConversationItWasCreatedFrom(t *testing.T) {
	for _, test := range []struct{ name, thread string }{
		{"routine from a top-level message", ""},
		{"routine from a thread", "170.500"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRoutineReportFixture(t, test.thread, true)
			run := f.occurrence("routine-occurrence", true)
			for range 2 {
				if err := f.worker.project(t.Context(), run); err != nil {
					t.Fatal(err)
				}
			}
			report, err := f.store.FindChannelMessageByIdempotencyKey(t.Context(), run.Scope, f.conversation.ID, "run-reporting-terminal:"+run.ID+":"+string(run.Status))
			if err != nil || report == nil || report.Content != "Today's digest: three stories." {
				t.Fatalf("routine report was not posted: %#v %v", report, err)
			}
			if report.ReplyToMessageID != "" || report.ThreadRootID != "" {
				t.Fatalf("report placement in the Conversation = reply %q thread %q", report.ReplyToMessageID, report.ThreadRootID)
			}
			deliveries := f.deliveries(t)
			if len(deliveries) != 1 {
				t.Fatalf("deliveries = %d, want exactly one", len(deliveries))
			}
			delivery := deliveries[0]
			if delivery.Operation != capability.ConversationDeliveryMessageSend || delivery.ChannelMessageID != report.ID || delivery.ConversationID != f.conversation.ID ||
				delivery.ExternalConversationID != "D123" || delivery.ExternalThreadID != "" || delivery.IdempotencyKey != routineReportDeliveryKey(report.ID) {
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
			paused := models.CloneExternalConversationEndpoint(f.endpoint)
			paused.Status, paused.Revision = models.ExternalConversationEndpointPaused, f.endpoint.Revision+1
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

// recordingMessageHost records the messages the delivery worker sends.
type recordingMessageHost struct{ sent []string }

func (h *recordingMessageHost) LookupExternalConversationDelivery(context.Context, ExternalConversationDeliveryHostRequest) (*ExternalConversationDeliveryAcknowledgement, error) {
	return &ExternalConversationDeliveryAcknowledgement{Status: ExternalConversationAcknowledgementNotFound}, nil
}

func (h *recordingMessageHost) DeliverExternalConversation(_ context.Context, req ExternalConversationDeliveryHostRequest) (*ExternalConversationDeliveryHostResult, error) {
	h.sent = append(h.sent, req.Message.Content)
	return &ExternalConversationDeliveryHostResult{Outcome: ExternalConversationDeliveryOutcomeDelivered, ProviderMessageID: "172.001"}, nil
}

// An event occurrence's mirrored report starts with its header line, because
// the external chat has no trail of what started it. The HelloSeal message
// keeps its plain text.
func TestRoutineReportMirrorHeader(t *testing.T) {
	for _, test := range []struct{ name, header, want string }{
		{"event occurrence", "New email from Ana Ruiz: Invoice #4411 (+3 more)", "New email from Ana Ruiz: Invoice #4411 (+3 more)\nToday's digest: three stories."},
		{"schedule occurrence", "", "Today's digest: three stories."},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRoutineReportFixture(t, "", true)
			run := f.occurrence("routine-occurrence", true)
			if test.header != "" {
				run.Context[models.RoutineEventBatchContextKey] = "batch-1"
				run.Context[models.RoutineEventHeaderContextKey] = test.header
			}
			if err := f.worker.project(t.Context(), run); err != nil {
				t.Fatal(err)
			}
			report, err := f.store.FindChannelMessageByIdempotencyKey(t.Context(), run.Scope, f.conversation.ID, "run-reporting-terminal:"+run.ID+":"+string(run.Status))
			if err != nil || report == nil || report.Content != "Today's digest: three stories." {
				t.Fatalf("HelloSeal report = %#v, %v", report, err)
			}
			host := &recordingMessageHost{}
			worker, err := NewExternalConversationDeliveryWorker(f.store, f.catalog, host, ExternalConversationDeliveryWorkerConfig{WorkerID: "mirror"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := worker.ProcessOne(t.Context(), f.endpoint.Scope); err != nil {
				t.Fatal(err)
			}
			if len(host.sent) != 1 || host.sent[0] != test.want {
				t.Fatalf("mirrored = %q, want %q", host.sent, test.want)
			}
		})
	}
}

// Outside header text is cut to fit at a rune boundary and cannot ping
// people in the mirrored chat.
func TestRoutineReportHeaderFitsAndNeutralizesMentions(t *testing.T) {
	header := func(text string) string {
		return routineReportHeader(&models.AgentRun{Context: map[string]interface{}{models.RoutineEventHeaderContextKey: text}})
	}
	if got := header("New email: <!channel> ping <@U123> and @here"); got != "New email: ‹!channel› ping ‹@\u200bU123› and @\u200bhere" {
		t.Fatalf("header = %q", got)
	}
	long := header("New email: " + strings.Repeat("é", 200))
	if len(long) > maxRoutineEventHeaderBytes || !utf8.ValidString(long) || !strings.HasPrefix(long, "New email: éé") || !strings.HasSuffix(long, "…") {
		t.Fatalf("long header = %q (%d bytes)", long, len(long))
	}
}
