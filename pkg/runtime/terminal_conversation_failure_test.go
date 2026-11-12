package runtime

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun/conversationruntest"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation/externalconversationtest"
	"github.com/google/uuid"
)

func assertTerminalConversationFailure(t *testing.T, store externalconversationtest.TerminalReportingStore, run *AgentRun, trigger *ChannelMessage, code string) *ChannelMessage {
	t.Helper()
	message, err := store.FindChannelMessageByIdempotencyKey(t.Context(), run.Scope, trigger.ConversationID, conversationrun.TerminalConversationFailureReplyKey(run))
	if err != nil || message == nil || message.Content != runtimeconversation.TerminalFailureReply(code) || message.Sender != (ConversationParticipant{Type: ConversationParticipantAgent, ID: run.AssignedAgentID}) || message.Intent != MessageIntentAnswer || message.ReplyToMessageID != trigger.ID || message.ResolvesMessageID != trigger.ID || message.ThreadRootID != trigger.ID || message.ResponseMode != trigger.ResponseMode {
		t.Fatalf("failure reply lost canonical actor, cause or call thread: %#v %v", message, err)
	}
	if strings.Contains(message.Content, "NEVER-PUBLISH") || !strings.Contains(message.Content, "Ask me to try again") {
		t.Fatal("private diagnostics escaped or manual retry missing")
	}
	canonical, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
	if err != nil || canonical.Status != AgentRunStatusFailed || canonical.Revision != run.Revision {
		t.Fatal("reply delivery mutated or re-executed the failed attempt")
	}
	return message
}

func TestTerminalConversationFailureAtomicCrashRestartAndPrivateCallThread(t *testing.T) {
	runStoreContractFixtures(t, func(t *testing.T, fixture runStoreContractFixture) {
		store := externalconversationtest.TerminalReportingStoreOf(t, fixture.store)
		now := runStoreContractEpoch
		run, _, trigger := conversationruntest.TerminalFailureConversation(t, store, now, true)
		run = conversationruntest.FailTerminalConversation(t, store, run, now, "cap_lookup_not_available")
		intent, err := store.GetRunTerminalReport(t.Context(), run.Scope, run.ID, run.Status)
		if err != nil || intent.Run == nil || intent.TerminalRevision != run.Revision || intent.Run.Revision != run.Revision {
			t.Fatalf("failed commit did not capture an immutable reply intent: %#v %v", intent, err)
		}
		if fixture.reopen != nil {
			fixture.store = fixture.reopen()
			store = externalconversationtest.TerminalReportingStoreOf(t, fixture.store)
		}
		if count, err := externalconversationtest.TerminalReportingWorker(t, store, now).ProcessBatch(t.Context()); err != nil || count != 1 {
			t.Fatalf("crash recovery did not publish failure: %d %v", count, err)
		}
		message := assertTerminalConversationFailure(t, store, run, trigger, "cap_lookup_not_available")
		for _, viewer := range []struct {
			id      string
			visible bool
		}{{"kev", true}, {"other-user", false}} {
			messages, err := runtimeconversation.NewConversationService(store).ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: run.Scope, ConversationID: trigger.ConversationID, ThreadRootID: trigger.ID, Limit: 20, Viewer: &ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantUser, ID: viewer.id}}})
			if err != nil {
				t.Fatal(err)
			}
			visible := false
			for _, candidate := range messages {
				visible = visible || candidate.ID == message.ID
			}
			if visible != viewer.visible {
				t.Fatalf("failure reply widened private ancestry for %s: %#v", viewer.id, messages)
			}
		}
		if count, err := externalconversationtest.TerminalReportingWorker(t, store, now).ProcessBatch(t.Context()); err != nil || count != 0 {
			t.Fatalf("failure delivery duplicated after acknowledgment: %d %v", count, err)
		}
	})
}

func TestTerminalConversationFailureManualRetryDoesNotOverwriteLeasedAttempt(t *testing.T) {
	runStoreContractFixtures(t, func(t *testing.T, fixture runStoreContractFixture) {
		store := externalconversationtest.TerminalReportingStoreOf(t, fixture.store)
		now := runStoreContractEpoch
		first, _, trigger := conversationruntest.TerminalFailureConversation(t, store, now, false)
		first = conversationruntest.FailTerminalConversation(t, store, first, now, "provider_rate_limited")
		claims, err := store.ClaimRunTerminalReports(t.Context(), RunTerminalReportingClaim{WorkerID: "old-delivery", Now: now, LeaseDuration: time.Minute, Limit: 1})
		if err != nil || len(claims) != 1 {
			t.Fatalf("first attempt claim: %#v %v", claims, err)
		}
		commands := agentrun.NewRunCommandService(store, models.WithClock(func() time.Time { return now.Add(time.Second) }))
		retry, err := commands.RetryConversationRun(t.Context(), agentrun.AgentRunCommandRequest{Scope: first.Scope, RunID: first.ID, ExpectedRevision: first.Revision, Actor: ActivityActor{Type: "user", ID: "kev"}})
		if err != nil || retry.Run.ID != first.ID {
			t.Fatalf("explicit safe retry stopped working: %#v %v", retry, err)
		}
		second := conversationruntest.FailTerminalConversation(t, store, retry.Run, now.Add(2*time.Second), "tool_call_cardinality")
		latest, err := store.GetRunTerminalReport(t.Context(), second.Scope, second.ID, second.Status)
		if err != nil || latest.TerminalRevision != second.Revision || latest.LeaseOwner != "" {
			t.Fatalf("new attempt replaced old lease or missing latest receipt: %#v %v", latest, err)
		}
		completion := RunTerminalReportingCompletion{Scope: first.Scope, RunID: first.ID, Status: first.Status, TerminalRevision: first.Revision, WorkerID: claims[0].LeaseOwner, LeaseExpiresAt: *claims[0].LeaseExpiresAt, Now: now.Add(3 * time.Second)}
		wrong := completion
		wrong.TerminalRevision = second.Revision
		if err := store.CompleteRunTerminalReport(t.Context(), wrong); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("old lease could acknowledge new attempt: %v", err)
		}
		if err := conversationrun.ProjectTerminalRunReporting(t.Context(), store, claims[0].Run); err != nil {
			t.Fatal(err)
		}
		if err := store.CompleteRunTerminalReport(t.Context(), completion); err != nil {
			t.Fatal(err)
		}
		if count, err := externalconversationtest.TerminalReportingWorker(t, store, now.Add(3*time.Second)).ProcessBatch(t.Context()); err != nil || count != 1 {
			t.Fatalf("new failure receipt was lost: %d %v", count, err)
		}
		message := assertTerminalConversationFailure(t, store, second, trigger, "tool_call_cardinality")
		old, err := store.FindChannelMessageByIdempotencyKey(t.Context(), first.Scope, trigger.ConversationID, conversationrun.TerminalConversationFailureReplyKey(first))
		if err != nil || old == nil || old.ID == message.ID || old.Content != runtimeconversation.TerminalFailureReply("provider_rate_limited") {
			t.Fatalf("manual attempts collapsed: %#v %#v %v", old, message, err)
		}
	})
}

func TestTerminalConversationFailureRejectsForgedOriginAndPreservesSavedAnswer(t *testing.T) {
	store := newTestKernelStore(t)
	run, conversation, trigger := conversationruntest.TerminalFailureConversation(t, store, runStoreContractEpoch, false)
	run = conversationruntest.FailTerminalConversation(t, store, run, runStoreContractEpoch, "provider_invalid_response")
	for _, mutate := range []func(*AgentRun){
		func(r *AgentRun) { r.Owner.ID = "other-agent" },
		func(r *AgentRun) { r.AssignedAgentID = "other-agent" },
		func(r *AgentRun) { r.Context["threadRootMessageId"] = "another-thread" },
		func(r *AgentRun) { r.Context[conversationRunContextTriggerID] = "other-message" },
	} {
		forged := cloneAgentRun(run)
		mutate(forged)
		if err := conversationrun.ProjectFailedConversationReply(t.Context(), store, forged); err == nil {
			t.Fatal("forged origin published an Agent answer")
		}
	}
	service := runtimeconversation.NewConversationService(store)
	posted, err := service.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{Scope: run.Scope, ConversationID: conversation.ID, ExpectedRevision: trigger.Sequence + 1,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: run.AssignedAgentID}, Intent: MessageIntentAnswer, Content: "The saved explanation", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: trigger.ID, ResolvesMessageID: trigger.ID,
		References: []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}}, IdempotencyKey: conversationTaskFinalResponseKey(run)})
	if err != nil {
		t.Fatal(err)
	}
	if err := conversationrun.ProjectFailedConversationReply(t.Context(), store, run); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetConversation(t.Context(), run.Scope, conversation.ID)
	if err != nil || current.LastSequence != posted.Message.Sequence {
		t.Fatal("already-saved explanation was duplicated")
	}
}

func TestTerminalConversationFailureReportsCanonicalScheduleWithoutResponseFlag(t *testing.T) {
	runStoreContractFixtures(t, func(t *testing.T, fixture runStoreContractFixture) {
		store := externalconversationtest.TerminalReportingStoreOf(t, fixture.store)
		for _, startsThread := range []bool{false, true} {
			scope := Scope{Kind: "tenant", ID: "canonical-scheduled-failure-" + uuid.NewString()}
			owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "finn"}
			service := runtimeconversation.NewConversationService(store)
			seedTestSpace(t, store.(SpaceStore), scope)
			conversation, _, err := service.CreateConversation(t.Context(), runtimeconversation.CreateConversationRequest{Scope: scope, Owner: owner, Title: "Scheduled user question", IdempotencyKey: uuid.NewString()})
			if err != nil {
				t.Fatal(err)
			}
			posted, err := service.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{
				Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
				Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "kev"}, Intent: MessageIntentQuestion,
				Content: "Check my calendar", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, StartThread: startsThread,
				IdempotencyKey: uuid.NewString(),
			})
			if err != nil || posted.Message.RequiresResponse {
				t.Fatalf("canonical source fixture unexpectedly opted into response flag: %#v %v", posted, err)
			}
			scheduler, err := conversationrun.NewConversationRunScheduler(store, store, conversationrun.ConversationRunSchedulerConfig{})
			if err != nil {
				t.Fatal(err)
			}
			scheduled, _, err := scheduler.ScheduleMessage(t.Context(), scope, conversation.ID, posted.Message.ID)
			if err != nil || scheduled == nil || scheduled.Run == nil {
				t.Fatalf("real scheduler did not authorize the user question: %#v %v", scheduled, err)
			}
			// PostgreSQL stores timestamps at microsecond precision. Use the same
			// explicit transition and worker clock across all store contracts.
			now := time.Now().UTC().Truncate(time.Microsecond)
			failed := conversationruntest.FailTerminalConversation(t, store, scheduled.Run, now, "provider_invalid_response")
			if count, err := externalconversationtest.TerminalReportingWorker(t, store, now).ProcessBatch(t.Context()); err != nil || count != 1 {
				t.Fatalf("failure reporter disagreed with canonical scheduler (thread=%t): %d %v", startsThread, count, err)
			}
			assertTerminalConversationFailure(t, store, failed, posted.Message, "provider_invalid_response")
		}
	})
}

func TestTerminalConversationFailureResponseFlagCannotAuthorizeControlSource(t *testing.T) {
	store := newTestKernelStore(t)
	run, conversation, _ := conversationruntest.TerminalFailureConversation(t, store, runStoreContractEpoch, false)
	run = conversationruntest.FailTerminalConversation(t, store, run, runStoreContractEpoch, "provider_invalid_response")
	current, err := store.GetConversation(t.Context(), run.Scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	control, err := runtimeconversation.NewConversationService(store).PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{
		Scope: run.Scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "kev"}, Intent: MessageIntentSystem,
		Content: "Control projection", RequiresResponse: true, StartThread: true,
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	forged := cloneAgentRun(run)
	forged.Context[conversationRunContextTriggerID] = control.Message.ID
	forged.Context["threadRootMessageId"] = control.Message.ID
	if err := conversationrun.ProjectFailedConversationReply(t.Context(), store, forged); !errors.Is(err, ErrInvalidAgentRun) {
		t.Fatalf("response flag authorized a source the kernel cannot schedule: %v", err)
	}
	current, err = store.GetConversation(t.Context(), run.Scope, conversation.ID)
	if err != nil || current.LastSequence != control.Message.Sequence {
		t.Fatalf("unscheduled control source produced a failure reply: %#v %v", current, err)
	}
}

func TestTerminalConversationCreditStopLeavesTheSavedMessageForRetry(t *testing.T) {
	runStoreContractFixtures(t, func(t *testing.T, fixture runStoreContractFixture) {
		store := externalconversationtest.TerminalReportingStoreOf(t, fixture.store)
		now := runStoreContractEpoch
		run, _, trigger := conversationruntest.TerminalFailureConversation(t, store, now, false)
		run = conversationruntest.FailTerminalConversation(t, store, run, now, "workspace_credits_exhausted")
		if run.CreditError == nil || run.CreditError.Code != RunCreditErrorWorkspaceCreditsExhausted {
			t.Fatalf("credit stop is not typed on the Run: %#v", run.CreditError)
		}
		if count, err := externalconversationtest.TerminalReportingWorker(t, store, now).ProcessBatch(t.Context()); err != nil || count != 1 {
			t.Fatalf("credit stop report was not acknowledged: %d %v", count, err)
		}
		if message, err := store.FindChannelMessageByIdempotencyKey(t.Context(), run.Scope, trigger.ConversationID, conversationrun.TerminalConversationFailureReplyKey(run)); message != nil || !errors.Is(err, ErrChannelMessageNotFound) && err != nil {
			t.Fatalf("an application chat received a failure answer for a credit stop: %#v %v", message, err)
		}
		retry, err := agentrun.NewRunCommandService(store).RetryConversationRun(t.Context(), agentrun.AgentRunCommandRequest{Scope: run.Scope, RunID: run.ID, ExpectedRevision: run.Revision, Actor: ActivityActor{Type: "user", ID: "kev"}})
		if err != nil || retry.Run.CreditError != nil || retry.Run.Status != AgentRunStatusQueued {
			t.Fatalf("retry after adding credits kept the credit stop: %#v %v", retry, err)
		}
		other := conversationruntest.FailTerminalConversation(t, store, retry.Run, now.Add(time.Second), "provider_rate_limited")
		if other.CreditError != nil {
			t.Fatalf("a different failure carried a credit error: %#v", other.CreditError)
		}
	})
}

func TestFailedConversationReplySkipsServiceWakeWithoutRetry(t *testing.T) {
	store := newTestKernelStore(t)
	run, conversation, _ := conversationruntest.TerminalFailureConversation(t, store, runStoreContractEpoch, false)
	run = conversationruntest.FailTerminalConversation(t, store, run, runStoreContractEpoch, "provider_invalid_response")
	current, err := store.GetConversation(t.Context(), run.Scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	wake, err := runtimeconversation.NewConversationService(store).PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{
		Scope: run.Scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantService, ID: "skill-setup-coordinator"}, Intent: MessageIntentUpdate,
		Content: "Gmail is connected", StartThread: true,
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	woken := cloneAgentRun(run)
	woken.Context[conversationRunContextTriggerID] = wake.Message.ID
	woken.Context["threadRootMessageId"] = wake.Message.ID
	if err := conversationrun.ProjectFailedConversationReply(t.Context(), store, woken); err != nil {
		t.Fatalf("a service wake has nobody waiting and must not be retried: %v", err)
	}
	after, err := store.GetConversation(t.Context(), run.Scope, conversation.ID)
	if err != nil || after.LastSequence != wake.Message.Sequence {
		t.Fatalf("service wake produced a failure reply: %#v %v", after, err)
	}
}
