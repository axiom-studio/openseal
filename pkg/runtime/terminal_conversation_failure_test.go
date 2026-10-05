package runtime

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func terminalFailureConversationFixture(t *testing.T, store terminalReportingContractStore, at time.Time, private bool) (*AgentRun, *Conversation, *ChannelMessage) {
	t.Helper()
	scope := Scope{Kind: "tenant", ID: "failure-" + uuid.NewString()}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "finn"}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: owner, Title: "Finn", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	audience := ConversationAudience{Kind: ConversationAudienceChannel}
	if private {
		audience = ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{{Type: ConversationParticipantAgent, ID: owner.ID}}}
	}
	posted, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "kev"}, Intent: MessageIntentQuestion,
		Content: "Check my recent emails", Audience: audience, StartThread: true, ResponseMode: "spoken",
		RequiresResponse: true, IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	commands := NewRunCommandService(store)
	commands.now = func() time.Time { return at }
	created, err := commands.CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Kind: RunKindConversation, Source: RunSourceChat,
		Goal: "Check the recent email request", ConcurrencyKey: conversation.ID + ":thread:" + posted.Message.ID,
		Context:        map[string]interface{}{conversationRunContextConversationID: conversation.ID, conversationRunContextTriggerID: posted.Message.ID, "threadRootMessageId": posted.Message.ID},
		IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return created.Run, posted.Conversation, posted.Message
}

func failTerminalConversation(t *testing.T, store terminalReportingContractStore, run *AgentRun, at time.Time, code string) *AgentRun {
	t.Helper()
	activity := NewRunActivityService(store, store)
	activity.now = func() time.Time { return at }
	running, _, err := activity.TransitionRun(t.Context(), run.Scope, run.ID, RunTransitionRequest{ExpectedRevision: run.Revision, Status: AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	failed, _, err := activity.TransitionRun(t.Context(), run.Scope, run.ID, RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: AgentRunStatusFailed,
		Error: "private-provider-diagnostic token=NEVER-PUBLISH", Checkpoint: checkpointTerminalFailure(run.Checkpoint, code),
	})
	if err != nil {
		t.Fatal(err)
	}
	return failed
}

func assertTerminalConversationFailure(t *testing.T, store terminalReportingContractStore, run *AgentRun, trigger *ChannelMessage, code string) *ChannelMessage {
	t.Helper()
	message, err := store.FindChannelMessageByIdempotencyKey(t.Context(), run.Scope, trigger.ConversationID, terminalConversationFailureReplyKey(run))
	if err != nil || message == nil || message.Content != TerminalFailureReply(code) || message.Sender != (ConversationParticipant{Type: ConversationParticipantAgent, ID: run.AssignedAgentID}) || message.Intent != MessageIntentAnswer || message.ReplyToMessageID != trigger.ID || message.ResolvesMessageID != trigger.ID || message.ThreadRootID != trigger.ID || message.ResponseMode != trigger.ResponseMode {
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
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := terminalReportingContractFixture(t, fixture)
		now := eventWaitContractEpoch
		run, _, trigger := terminalFailureConversationFixture(t, store, now, true)
		run = failTerminalConversation(t, store, run, now, "cap_lookup_not_available")
		intent, err := store.GetRunTerminalReport(t.Context(), run.Scope, run.ID, run.Status)
		if err != nil || intent.Run == nil || intent.TerminalRevision != run.Revision || intent.Run.Revision != run.Revision {
			t.Fatalf("failed commit did not capture an immutable reply intent: %#v %v", intent, err)
		}
		if fixture.reopen != nil {
			fixture.store = fixture.reopen()
			store = terminalReportingContractFixture(t, fixture)
		}
		if count, err := terminalReportingContractWorker(t, store, now).ProcessBatch(t.Context()); err != nil || count != 1 {
			t.Fatalf("crash recovery did not publish failure: %d %v", count, err)
		}
		message := assertTerminalConversationFailure(t, store, run, trigger, "cap_lookup_not_available")
		for _, viewer := range []struct {
			id      string
			visible bool
		}{{"kev", true}, {"other-user", false}} {
			messages, err := NewConversationService(store).ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: run.Scope, ConversationID: trigger.ConversationID, ThreadRootID: trigger.ID, Limit: 20, Viewer: &ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantUser, ID: viewer.id}}})
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
		if count, err := terminalReportingContractWorker(t, store, now).ProcessBatch(t.Context()); err != nil || count != 0 {
			t.Fatalf("failure delivery duplicated after acknowledgment: %d %v", count, err)
		}
	})
}

func TestTerminalConversationFailureManualRetryDoesNotOverwriteLeasedAttempt(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := terminalReportingContractFixture(t, fixture)
		now := eventWaitContractEpoch
		first, _, trigger := terminalFailureConversationFixture(t, store, now, false)
		first = failTerminalConversation(t, store, first, now, "provider_rate_limited")
		claims, err := store.ClaimRunTerminalReports(t.Context(), RunTerminalReportingClaim{WorkerID: "old-delivery", Now: now, LeaseDuration: time.Minute, Limit: 1})
		if err != nil || len(claims) != 1 {
			t.Fatalf("first attempt claim: %#v %v", claims, err)
		}
		commands := NewRunCommandService(store)
		commands.now = func() time.Time { return now.Add(time.Second) }
		retry, err := commands.RetryConversationRun(t.Context(), AgentRunCommandRequest{Scope: first.Scope, RunID: first.ID, ExpectedRevision: first.Revision, Actor: ActivityActor{Type: "user", ID: "kev"}})
		if err != nil || retry.Run.ID != first.ID {
			t.Fatalf("explicit safe retry stopped working: %#v %v", retry, err)
		}
		second := failTerminalConversation(t, store, retry.Run, now.Add(2*time.Second), "tool_call_cardinality")
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
		if err := projectTerminalRunReporting(t.Context(), store, claims[0].Run); err != nil {
			t.Fatal(err)
		}
		if err := store.CompleteRunTerminalReport(t.Context(), completion); err != nil {
			t.Fatal(err)
		}
		if count, err := terminalReportingContractWorker(t, store, now.Add(3*time.Second)).ProcessBatch(t.Context()); err != nil || count != 1 {
			t.Fatalf("new failure receipt was lost: %d %v", count, err)
		}
		message := assertTerminalConversationFailure(t, store, second, trigger, "tool_call_cardinality")
		old, err := store.FindChannelMessageByIdempotencyKey(t.Context(), first.Scope, trigger.ConversationID, terminalConversationFailureReplyKey(first))
		if err != nil || old == nil || old.ID == message.ID || old.Content != TerminalFailureReply("provider_rate_limited") {
			t.Fatalf("manual attempts collapsed: %#v %#v %v", old, message, err)
		}
	})
}

func TestTerminalConversationFailureRejectsForgedOriginAndPreservesSavedAnswer(t *testing.T) {
	store := NewMemoryStore()
	run, conversation, trigger := terminalFailureConversationFixture(t, store, eventWaitContractEpoch, false)
	run = failTerminalConversation(t, store, run, eventWaitContractEpoch, "provider_invalid_response")
	for _, mutate := range []func(*AgentRun){
		func(r *AgentRun) { r.Owner.ID = "other-agent" },
		func(r *AgentRun) { r.AssignedAgentID = "other-agent" },
		func(r *AgentRun) { r.Context["threadRootMessageId"] = "another-thread" },
		func(r *AgentRun) { r.Context[conversationRunContextTriggerID] = "other-message" },
	} {
		forged := cloneAgentRun(run)
		mutate(forged)
		if err := projectFailedConversationReply(t.Context(), store, forged); err == nil {
			t.Fatal("forged origin published an Agent answer")
		}
	}
	service := NewConversationService(store)
	posted, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{Scope: run.Scope, ConversationID: conversation.ID, ExpectedRevision: trigger.Sequence + 1,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: run.AssignedAgentID}, Intent: MessageIntentAnswer, Content: "The saved explanation", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: trigger.ID, ResolvesMessageID: trigger.ID,
		References: []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}}, IdempotencyKey: conversationTaskFinalResponseKey(run)})
	if err != nil {
		t.Fatal(err)
	}
	if err := projectFailedConversationReply(t.Context(), store, run); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetConversation(t.Context(), run.Scope, conversation.ID)
	if err != nil || current.LastSequence != posted.Message.Sequence {
		t.Fatal("already-saved explanation was duplicated")
	}
}

func TestTerminalFailureReplyUsesOnlyFiniteClassifiedFacts(t *testing.T) {
	for _, code := range []string{"cap_lookup_not_available", "cap_lookup_duplicate", "cap_lookup_already_loaded", "tool_call_cardinality", "tool_call_identity_missing", "tool_call_not_authorized", "provider_authentication_failed", "provider_quota_exhausted", "workspace_operation_failed", "action_admission_failed", "unknown token=PRIVATE"} {
		text := TerminalFailureReply(code)
		if len(text) > 512 || !strings.Contains(text, "I couldn't finish") || !strings.Contains(text, "Ask me to try again") || strings.Contains(text, "PRIVATE") {
			t.Fatalf("unbounded/unsafe failure reply: %q", text)
		}
	}
	if TerminalFailureReply("cap_lookup_not_available") == TerminalFailureReply("cap_lookup_already_loaded") {
		t.Fatal("distinct causes were falsely collapsed")
	}
}

func TestTerminalFailureReplyDistinguishesSafeValidationAndBudgetCauses(t *testing.T) {
	for _, tc := range []struct{ input, code, reason string }{
		{"provider_output_truncated", "provider_output_truncated", "stopped before completing"},
		{"capability_input_budget_exhausted", "budget_exhausted", "execution budget"},
		{"tool_arguments_invalid", "tool_arguments_invalid", "arguments that did not match"},
		{"action_review_invalid", "action_review_invalid", "operation review details"},
		{"action_external_identity_invalid", "action_external_identity_invalid", "identification for an external operation"},
		{"final_skill_identity_invalid", "final_skill_identity_invalid", "Skill reference"},
		{"conversation_task_review_invalid", "conversation_task_review_invalid", "invalid review response"},
		{"conversation_task_review_failed", "conversation_task_review_failed", "did not meet the review requirements"},
		{"provider_invalid_turn_outcome", "provider_invalid_response", "could not be safely executed"},
		{"tool_arguments_invalid: token=PRIVATE", "execution_failed", "execution failure"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			checkpoint := checkpointTerminalFailure(nil, tc.input)
			if terminalFailureCodeFromCheckpoint(checkpoint) != tc.code {
				t.Fatal("checkpoint lost the finite terminal cause")
			}
			text := TerminalFailureReply(tc.input)
			if !strings.Contains(text, tc.reason) || !strings.Contains(text, "Ask me to try again") || len(text) > 512 || strings.Contains(text, "PRIVATE") {
				t.Fatal("failure reply lost safe specificity or leaked arbitrary code text")
			}
		})
	}
	if TerminalFailureReply("provider_output_truncated") == TerminalFailureReply("provider_invalid_response") {
		t.Fatal("truncated output was collapsed into malformed output")
	}
}

func TestTerminalConversationFailureReportsCanonicalScheduleWithoutResponseFlag(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := terminalReportingContractFixture(t, fixture)
		for _, startsThread := range []bool{false, true} {
			scope := Scope{Kind: "tenant", ID: "canonical-scheduled-failure-" + uuid.NewString()}
			owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "finn"}
			service := NewConversationService(store)
			conversation, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: owner, Title: "Scheduled user question", IdempotencyKey: uuid.NewString()})
			if err != nil {
				t.Fatal(err)
			}
			posted, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{
				Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
				Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "kev"}, Intent: MessageIntentQuestion,
				Content: "Check my calendar", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, StartThread: startsThread,
				IdempotencyKey: uuid.NewString(),
			})
			if err != nil || posted.Message.RequiresResponse {
				t.Fatalf("canonical source fixture unexpectedly opted into response flag: %#v %v", posted, err)
			}
			scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
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
			failed := failTerminalConversation(t, store, scheduled.Run, now, "provider_invalid_response")
			if count, err := terminalReportingContractWorker(t, store, now).ProcessBatch(t.Context()); err != nil || count != 1 {
				t.Fatalf("failure reporter disagreed with canonical scheduler (thread=%t): %d %v", startsThread, count, err)
			}
			assertTerminalConversationFailure(t, store, failed, posted.Message, "provider_invalid_response")
		}
	})
}

func TestTerminalConversationFailureResponseFlagCannotAuthorizeControlSource(t *testing.T) {
	store := NewMemoryStore()
	run, conversation, _ := terminalFailureConversationFixture(t, store, eventWaitContractEpoch, false)
	run = failTerminalConversation(t, store, run, eventWaitContractEpoch, "provider_invalid_response")
	current, err := store.GetConversation(t.Context(), run.Scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	control, err := NewConversationService(store).PostChannelMessage(t.Context(), PostChannelMessageRequest{
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
	if err := projectFailedConversationReply(t.Context(), store, forged); !errors.Is(err, ErrInvalidAgentRun) {
		t.Fatalf("response flag authorized a source the kernel cannot schedule: %v", err)
	}
	current, err = store.GetConversation(t.Context(), run.Scope, conversation.ID)
	if err != nil || current.LastSequence != control.Message.Sequence {
		t.Fatalf("unscheduled control source produced a failure reply: %#v %v", current, err)
	}
}
