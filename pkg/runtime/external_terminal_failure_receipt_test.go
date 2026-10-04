package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/google/uuid"
)

func TestExternalTerminalFailureUsesCapturedRevisionAfterMetadataUpdate(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := terminalReportingContractFixture(t, fixture)
		now := eventWaitContractEpoch
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
			changed := cloneAgentRun(current)
			changed.Revision++
			changed.UpdatedAt = now.Add(time.Duration(changed.Revision) * time.Second)
			changed.Plan = map[string]interface{}{"metadata": "updated"}
			if _, err := store.UpdateAgentRunWithEvent(t.Context(), changed, current.Revision, &ActivityEvent{ID: uuid.NewString(), Scope: changed.Scope, RunID: changed.ID, EventType: "run.metadata_updated", Summary: "Updated metadata", Severity: ActivitySeverityInfo, Visibility: ActivityVisibilityPrivate, CreatedAt: changed.UpdatedAt}, nil); err != nil {
				t.Fatal(err)
			}
			captured, err := externalConversationFailureReportRun(t.Context(), store, changed)
			if err != nil || captured.Revision != first.Revision {
				t.Fatalf("metadata revision became a new failure attempt: %#v %v", captured, err)
			}
			if err := projectFailedConversationReply(t.Context(), store, captured); err != nil {
				t.Fatal(err)
			}
			messages, err := store.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: first.Scope, ConversationID: trigger.ConversationID, Limit: 20})
			if err != nil || len(messages) != 2 {
				t.Fatalf("metadata update duplicated the failure reply: %#v %v", messages, err)
			}
		}
	})
}

func TestExternalTerminalFailurePrefersSavedNormalAnswerOverOlderFailure(t *testing.T) {
	store := NewMemoryStore()
	now := eventWaitContractEpoch
	run, conversation, trigger := terminalFailureConversationFixture(t, store, now, false)
	run = failTerminalConversation(t, store, run, now, "provider_rate_limited")
	if err := projectFailedConversationReply(t.Context(), store, run); err != nil {
		t.Fatal(err)
	}
	conversation, err := store.GetConversation(t.Context(), run.Scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	normal, err := NewConversationService(store).PostChannelMessage(t.Context(), PostChannelMessageRequest{
		Scope: run.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: run.AssignedAgentID}, Intent: MessageIntentAnswer,
		Content: "The saved normal answer", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: trigger.ID,
		References: []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}}, IdempotencyKey: conversationTaskFinalResponseKey(run),
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := &ExternalConversationReplyWorker{store: store}
	message, err := worker.findCanonicalReply(t.Context(), &ExternalConversationInboxItem{Scope: run.Scope, ConversationID: conversation.ID, ChannelMessageID: trigger.ID}, run)
	if err != nil || message == nil || message.ID != normal.Message.ID {
		t.Fatalf("history scan selected an older failed attempt instead of the exact normal answer: %#v %v", message, err)
	}
}

type externalFailureReceiptFixture struct{ report *RunTerminalReport }

func (f externalFailureReceiptFixture) GetRunTerminalReport(context.Context, Scope, string, AgentRunStatus) (*RunTerminalReport, error) {
	return f.report, nil
}

func TestExternalTerminalFailureRejectsReceiptIdentityDrift(t *testing.T) {
	run := &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "tenant"}, Kind: RunKindConversation, Source: RunSourceChat, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "finn"}, AssignedAgentID: "finn", Status: AgentRunStatusFailed, Revision: 4,
		Context: map[string]interface{}{conversationRunContextConversationID: "conversation", conversationRunContextTriggerID: "trigger", "threadRootMessageId": "trigger"}}
	for _, field := range []string{"scope", "revision", "agent", "source", "trigger", "malformed_thread"} {
		t.Run(field, func(t *testing.T) {
			snapshot := cloneAgentRun(run)
			report := &RunTerminalReport{Scope: run.Scope, RunID: run.ID, Status: run.Status, TerminalRevision: run.Revision, Run: snapshot}
			switch field {
			case "scope":
				report.Scope.ID = "other"
			case "revision":
				report.TerminalRevision++
			case "agent":
				snapshot.AssignedAgentID = "other"
			case "source":
				snapshot.Source = RunSourceEvent
			case "trigger":
				snapshot.Context[conversationRunContextTriggerID] = "other"
			case "malformed_thread":
				snapshot.Context["threadRootMessageId"] = map[string]interface{}{"invalid": true}
			}
			if _, err := externalConversationFailureReportRun(t.Context(), externalFailureReceiptFixture{report: report}, run); !errors.Is(err, ErrInvalidRunTerminalReport) {
				t.Fatalf("drifted terminal authority was accepted: %v", err)
			}
		})
	}
}

func TestExternalTerminalFailureAcknowledgedReceiptCannotRedirectInboxOrigin(t *testing.T) {
	store, catalog, endpoint := externalConversationDeliveryFixture(t, t.Context(), "slack")
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "Original inbox", Origin: &ConversationReference{Kind: ConversationReferenceExternalSource, ID: endpoint.ID, Version: endpoint.Revision}, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	post := func(conversation *Conversation) *ChannelMessage {
		t.Helper()
		message, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{Scope: conversation.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision, Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "kev"}, Intent: MessageIntentQuestion, Content: "Check this request", RequiresResponse: true, StartThread: true, Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: uuid.NewString()})
		if err != nil {
			t.Fatal(err)
		}
		return message.Message
	}
	trigger := post(conversation)
	created, err := NewRunCommandService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: endpoint.Scope, Owner: endpoint.Owner, AssignedAgentID: endpoint.DeploymentID, Kind: RunKindConversation, Source: RunSourceEvent, Goal: "Respond", Context: map[string]interface{}{conversationRunContextConversationID: conversation.ID, conversationRunContextTriggerID: trigger.ID, "threadRootMessageId": trigger.ID}, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run := failTerminalConversation(t, store, created.Run, now, "provider_unavailable")
	if count, err := terminalReportingContractWorker(t, store, now).ProcessBatch(t.Context()); err != nil || count != 1 {
		t.Fatalf("failure delivery before acknowledgment: %d %v", count, err)
	}
	foreign, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: endpoint.Scope, Owner: endpoint.Owner, Title: "Different same-owner conversation", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	foreignTrigger := post(foreign)
	changed := cloneAgentRun(run)
	changed.Context[conversationRunContextConversationID] = foreign.ID
	changed.Context[conversationRunContextTriggerID] = foreignTrigger.ID
	changed.Context["threadRootMessageId"] = foreignTrigger.ID
	changed.Revision++
	changed.UpdatedAt = now.Add(time.Second)
	if _, err := store.UpdateAgentRunWithEvent(t.Context(), changed, run.Revision, &ActivityEvent{ID: uuid.NewString(), Scope: run.Scope, RunID: run.ID, EventType: "run.metadata_updated", Summary: "Changed metadata", CreatedAt: changed.UpdatedAt}, nil); err != nil {
		t.Fatal(err)
	}
	worker, err := NewExternalConversationReplyWorker(store, catalog)
	if err != nil {
		t.Fatal(err)
	}
	item := &ExternalConversationInboxItem{ID: "original-inbox", Scope: endpoint.Scope, EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision, Adapter: endpoint.Adapter, Status: ExternalConversationInboxApplied, ConversationID: conversation.ID, ChannelMessageID: trigger.ID, RunID: run.ID,
		Event: NormalizedExternalConversationEvent{ID: "original-event", Type: capability.ConversationEventMessageReceived, ExternalConversationID: "C123", ExternalMessageID: "171.001", ExternalParticipantID: "kev", Text: "Check this request", OrderingKey: "C123:171.001", OccurredAt: now}}
	if _, err := worker.project(t.Context(), item); !errors.Is(err, ErrExternalConversationConflict) {
		t.Fatalf("mutable metadata redirected an acknowledged inbox reply: %v", err)
	}
	messages, err := store.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: endpoint.Scope, ConversationID: foreign.ID, Limit: 10})
	if err != nil || len(messages) != 1 || messages[0].ID != foreignTrigger.ID {
		t.Fatalf("failure projection wrote in the foreign conversation: %#v %v", messages, err)
	}
	deliveries, err := store.ListExternalConversationDeliveries(t.Context(), ExternalConversationDeliveryFilter{Scope: endpoint.Scope, ConversationID: conversation.ID, Limit: 10})
	if err != nil || len(deliveries) != 0 {
		t.Fatalf("rejected foreign origin still enqueued external delivery: %#v %v", deliveries, err)
	}
}
