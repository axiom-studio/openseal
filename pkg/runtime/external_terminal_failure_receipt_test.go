package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/google/uuid"
)

func TestExternalTerminalFailureUsesCapturedRevisionAfterMetadataUpdate(t *testing.T) {
	runStoreContractFixtures(t, func(t *testing.T, fixture runStoreContractFixture) {
		store := terminalReportingContractFixture(t, fixture)
		now := runStoreContractEpoch
		first, _, trigger := terminalFailureConversationFixture(t, store, now, false)
		first = failTerminalConversation(t, store, first, now, "provider_rate_limited")
		for _, acknowledged := range []bool{false, true} {
			if acknowledged {
				if count, err := terminalReportingContractWorker(t, store, now).ProcessBatch(t.Context()); err != nil || count != 1 {
					t.Fatalf("failure acknowledgment: %d %v", count, err)
				}
			}
			current, err := store.GetAgentRun(t.Context(), first.Scope, first.ID)
			if err != nil {
				t.Fatal(err)
			}
			changed := models.CloneAgentRun(current)
			changed.Revision++
			changed.UpdatedAt = now.Add(time.Duration(changed.Revision) * time.Second)
			changed.Plan = map[string]interface{}{"metadata": "updated"}
			if _, err := store.UpdateAgentRunWithEvent(t.Context(), changed, current.Revision, &models.ActivityEvent{ID: uuid.NewString(), Scope: changed.Scope, RunID: changed.ID, EventType: "run.metadata_updated", Summary: "Updated metadata", Severity: models.ActivitySeverityInfo, Visibility: models.ActivityVisibilityPrivate, CreatedAt: changed.UpdatedAt}, nil); err != nil {
				t.Fatal(err)
			}
			captured, err := externalConversationFailureReportRun(t.Context(), store, changed)
			if err != nil || captured.Revision != first.Revision {
				t.Fatalf("metadata revision became a new failure attempt: %#v %v", captured, err)
			}
			if err := conversationrun.ProjectFailedConversationReply(t.Context(), store, captured); err != nil {
				t.Fatal(err)
			}
			messages, err := store.ListChannelMessages(t.Context(), models.ChannelMessageFilter{Scope: first.Scope, ConversationID: trigger.ConversationID, Limit: 20})
			if err != nil || len(messages) != 2 {
				t.Fatalf("metadata update duplicated the failure reply: %#v %v", messages, err)
			}
		}
	})
}

func TestExternalTerminalFailurePrefersSavedNormalAnswerOverOlderFailure(t *testing.T) {
	store := newTestKernelStore(t)
	now := runStoreContractEpoch
	run, conversation, trigger := terminalFailureConversationFixture(t, store, now, false)
	run = failTerminalConversation(t, store, run, now, "provider_rate_limited")
	if err := conversationrun.ProjectFailedConversationReply(t.Context(), store, run); err != nil {
		t.Fatal(err)
	}
	conversation, err := store.GetConversation(t.Context(), run.Scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	normal, err := runtimeconversation.NewConversationService(store).PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{
		Scope: run.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: run.AssignedAgentID}, Intent: models.MessageIntentAnswer,
		Content: "The saved normal answer", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, ReplyToMessageID: trigger.ID,
		References: []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: run.ID}}, IdempotencyKey: models.ConversationTaskFinalResponseKey(run),
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := &ExternalConversationReplyWorker{store: store}
	message, err := worker.findCanonicalReply(t.Context(), &models.ExternalConversationInboxItem{Scope: run.Scope, ConversationID: conversation.ID, ChannelMessageID: trigger.ID}, run)
	if err != nil || message == nil || message.ID != normal.Message.ID {
		t.Fatalf("history scan selected an older failed attempt instead of the exact normal answer: %#v %v", message, err)
	}
}

type externalFailureReceiptFixture struct{ report *models.RunTerminalReport }

func (f externalFailureReceiptFixture) GetRunTerminalReport(context.Context, models.Scope, string, models.AgentRunStatus) (*models.RunTerminalReport, error) {
	return f.report, nil
}

func TestExternalTerminalFailureRejectsReceiptIdentityDrift(t *testing.T) {
	run := &models.AgentRun{ID: "run", Scope: models.Scope{Kind: "tenant", ID: "tenant"}, Kind: models.RunKindConversation, Source: models.RunSourceChat, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "finn"}, AssignedAgentID: "finn", Status: models.AgentRunStatusFailed, Revision: 4,
		Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: "conversation", models.ConversationRunTriggerIDContextKey: "trigger", "threadRootMessageId": "trigger"}}
	for _, field := range []string{"scope", "revision", "agent", "source", "trigger", "malformed_thread"} {
		t.Run(field, func(t *testing.T) {
			snapshot := models.CloneAgentRun(run)
			report := &models.RunTerminalReport{Scope: run.Scope, RunID: run.ID, Status: run.Status, TerminalRevision: run.Revision, Run: snapshot}
			switch field {
			case "scope":
				report.Scope.ID = "other"
			case "revision":
				report.TerminalRevision++
			case "agent":
				snapshot.AssignedAgentID = "other"
			case "source":
				snapshot.Source = models.RunSourceEvent
			case "trigger":
				snapshot.Context[models.ConversationRunTriggerIDContextKey] = "other"
			case "malformed_thread":
				snapshot.Context["threadRootMessageId"] = map[string]interface{}{"invalid": true}
			}
			if _, err := externalConversationFailureReportRun(t.Context(), externalFailureReceiptFixture{report: report}, run); !errors.Is(err, models.ErrInvalidRunTerminalReport) {
				t.Fatalf("drifted terminal authority was accepted: %v", err)
			}
		})
	}
}

func TestExternalTerminalFailureAcknowledgedReceiptCannotRedirectInboxOrigin(t *testing.T) {
	store, catalog, endpoint := externalConversationDeliveryFixture(t, t.Context(), "slack")
	service := runtimeconversation.NewConversationService(store)
	conversation, _, err := service.CreateConversation(t.Context(), runtimeconversation.CreateConversationRequest{Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "Original inbox", Origin: &models.ConversationReference{Kind: models.ConversationReferenceExternalSource, ID: endpoint.ID, Version: endpoint.Revision}, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	post := func(conversation *models.Conversation) *models.ChannelMessage {
		t.Helper()
		message, err := service.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{Scope: conversation.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "kev"}, Intent: models.MessageIntentQuestion, Content: "Check this request", RequiresResponse: true, StartThread: true, Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, IdempotencyKey: uuid.NewString()})
		if err != nil {
			t.Fatal(err)
		}
		return message.Message
	}
	trigger := post(conversation)
	created, err := agentrun.NewRunCommandService(store).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{Scope: endpoint.Scope, Owner: endpoint.Owner, AssignedAgentID: endpoint.DeploymentID, Kind: models.RunKindConversation, Source: models.RunSourceEvent, Goal: "Respond", Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: conversation.ID, models.ConversationRunTriggerIDContextKey: trigger.ID, "threadRootMessageId": trigger.ID}, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run := failTerminalConversation(t, store, created.Run, now, "provider_unavailable")
	if count, err := terminalReportingContractWorker(t, store, now).ProcessBatch(t.Context()); err != nil || count != 1 {
		t.Fatalf("failure delivery before acknowledgment: %d %v", count, err)
	}
	foreign, _, err := service.CreateConversation(t.Context(), runtimeconversation.CreateConversationRequest{Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "Different same-owner conversation", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	foreignTrigger := post(foreign)
	changed := models.CloneAgentRun(run)
	changed.Context[models.ConversationRunConversationIDContextKey] = foreign.ID
	changed.Context[models.ConversationRunTriggerIDContextKey] = foreignTrigger.ID
	changed.Context["threadRootMessageId"] = foreignTrigger.ID
	changed.Revision++
	changed.UpdatedAt = now.Add(time.Second)
	if _, err := store.UpdateAgentRunWithEvent(t.Context(), changed, run.Revision, &models.ActivityEvent{ID: uuid.NewString(), Scope: run.Scope, RunID: run.ID, EventType: "run.metadata_updated", Summary: "Changed metadata", CreatedAt: changed.UpdatedAt}, nil); err != nil {
		t.Fatal(err)
	}
	worker, err := NewExternalConversationReplyWorker(store, catalog)
	if err != nil {
		t.Fatal(err)
	}
	item := &models.ExternalConversationInboxItem{ID: "original-inbox", Scope: endpoint.Scope, EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter, Status: models.ExternalConversationInboxApplied, ConversationID: conversation.ID, ChannelMessageID: trigger.ID, RunID: run.ID,
		Event: models.NormalizedExternalConversationEvent{ID: "original-event", Type: capability.ConversationEventMessageReceived, ExternalConversationID: "C123", ExternalMessageID: "171.001", ExternalParticipantID: "kev", Text: "Check this request", OrderingKey: "C123:171.001", OccurredAt: now}}
	if _, _, err := worker.project(t.Context(), item); !errors.Is(err, models.ErrExternalConversationConflict) {
		t.Fatalf("mutable metadata redirected an acknowledged inbox reply: %v", err)
	}
	messages, err := store.ListChannelMessages(t.Context(), models.ChannelMessageFilter{Scope: endpoint.Scope, ConversationID: foreign.ID, Limit: 10})
	if err != nil || len(messages) != 1 || messages[0].ID != foreignTrigger.ID {
		t.Fatalf("failure projection wrote in the foreign conversation: %#v %v", messages, err)
	}
	deliveries, err := store.ListExternalConversationDeliveries(t.Context(), models.ExternalConversationDeliveryFilter{Scope: endpoint.Scope, ConversationID: conversation.ID, Limit: 10})
	if err != nil || len(deliveries) != 0 {
		t.Fatalf("rejected foreign origin still enqueued external delivery: %#v %v", deliveries, err)
	}
}
