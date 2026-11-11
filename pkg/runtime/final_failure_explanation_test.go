package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestBeginRunFailureExplanationClassifiesAndBounds(t *testing.T) {
	for code, want := range map[string]string{
		"gateway_credits_exhausted":       "workspace_credits_exhausted",
		"provider_model_unavailable":      "provider_unavailable",
		RunFailureCodeErrorStreak:         RunFailureCodeErrorStreak,
		TurnHostContextExceeded:           TurnHostContextExceeded,
		"task_completion_rejected":        "task_completion_rejected",
		"":                                "execution_failed",
		"provider said: TOKEN=SECRET":     "execution_failed",
		strings.Repeat("x", 100):          "execution_failed",
		"unverified_approval_claim":       "unverified_approval_claim",
		"  final_answer_rejected  ":       "final_answer_rejected",
		"agent_behavior_fixed":            "agent_behavior_fixed",
		"workspace_turn_budget_exhausted": "budget_exhausted",
	} {
		f := beginRunFailureExplanation(&AgentRun{}, "", code, "m", "", "")
		if f.Code != want || f.Kind != RunFailureKindExecution || f.Explained {
			t.Fatalf("code %q = %+v, want %s", code, f, want)
		}
	}
	long := beginRunFailureExplanation(nil, RunFailureKindWorkspace, "workspace_operation_failed", strings.Repeat("é", 2000), strings.Repeat("e", 4000), "bad code!")
	if len(long.Message) > maximumTurnFailureTextBytes+len("…") || len(long.StepError) > maximumTurnFailureTextBytes+len("…") || long.StepErrorCode != "" || long.Kind != RunFailureKindWorkspace {
		t.Fatalf("failure not bounded: %d %d %q", len(long.Message), len(long.StepError), long.StepErrorCode)
	}
	if empty := beginRunFailureExplanation(nil, "", "", "  ", "", ""); empty.Message == "" {
		t.Fatal("a failure needs a message")
	}
	first := &RunFailureExplanation{Kind: RunFailureKindExecution, Code: "execution_failed", Message: "first"}
	if again := beginRunFailureExplanation(&AgentRun{FailureExplanation: first}, "", "provider_unavailable", "second", "", ""); again == first || *again != *first {
		t.Fatalf("a second failure replaced the first cause: %+v", again)
	}
}

func TestExplainableFailureRunsToolLessStepThenFailsExplained(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		ctx := t.Context()
		run := claimedFailureRun(t, store, "explained")
		cause := errors.New("the model returned malformed tool arguments")
		if !modelExplainsFailure(run) || !turnFailureCanBeExplained(cause, "tool_arguments_invalid") {
			t.Fatal("an ordinary step failure must be explainable")
		}
		activity := NewRunActivityService(store, store)
		explaining, _, err := activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusRunning, LeaseOwner: run.LeaseOwner,
			FailureExplanation: beginRunFailureExplanation(run, RunFailureKindExecution, "tool_arguments_invalid", "The model supplied invalid arguments.", cause.Error(), "producer_failed"),
			EventType:          "run.failure_recorded", Summary: "The attempt stopped; the model will explain it",
		})
		if err != nil {
			t.Fatal(err)
		}
		if !runExplainingFailure(explaining) || explaining.CreditError != nil || explaining.FailureExplanation.StepErrorCode != "producer_failed" {
			t.Fatalf("Run is not in explain mode: %+v", explaining.FailureExplanation)
		}
		note := failureExplanationNote(explaining.SessionID, explaining.ID, "step-2", explaining.FailureExplanation)
		if err := note.Validate(); err != nil {
			t.Fatal(err)
		}
		var data map[string]string
		if note.Type != SessionEntryNote || note.Note.Kind != NoteFailureExplanation || note.SourceKey != FailureNoteSourceKey(explaining.ID, "step-2") ||
			note.ID != SessionEntryID(explaining.SessionID, note.SourceKey) || json.Unmarshal(note.Note.Data, &data) != nil || data["code"] != "tool_arguments_invalid" {
			t.Fatalf("note = %+v", note)
		}
		for _, want := range []string{"This attempt has stopped: The model supplied invalid arguments. Give one brief", "Do not call tools"} {
			if !strings.Contains(note.Note.Text, want) {
				t.Fatalf("note text %q lacks %q", note.Note.Text, want)
			}
		}
		if strings.Contains(note.Note.Text, "continuationCheckpoint") || strings.Contains(note.Note.Text, "runOutput") {
			t.Fatalf("note text names the old form: %q", note.Note.Text)
		}

		// The tool-free step posted its reply.
		failed, _, err := activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: explaining.Revision, Status: AgentRunStatusFailed, LeaseOwner: run.LeaseOwner,
			Error: explaining.FailureExplanation.Message, FailureExplanation: explainedRunFailure(explaining.FailureExplanation),
			Output:    map[string]interface{}{"summary": "I couldn't read that file, so I stopped.", "messageId": "m-1"},
			EventType: "run.failed", Summary: "Run failed",
		})
		if err != nil {
			t.Fatal(err)
		}
		stored, err := store.GetAgentRun(ctx, run.Scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if runExplainingFailure(stored) || stored.FailureExplanation == nil || !stored.FailureExplanation.Explained || stored.FailureExplanation.Code != "tool_arguments_invalid" || stored.Error != "The model supplied invalid arguments." {
			t.Fatalf("explained failure not persisted: %+v %q", stored.FailureExplanation, stored.Error)
		}
		if reply, ok := externalConversationRunReply(ctx, nil, nil, failed); !ok || reply != "I couldn't read that file, so I stopped." {
			t.Fatalf("explained reply = %q %v", reply, ok)
		}
		if reason, err := NewRunCommandService(store).conversationRetryBlocker(ctx, stored); err != nil || !strings.Contains(reason, "already has a reply") {
			t.Fatalf("explained attempt was retryable: %q %v", reason, err)
		}
	})
}

func TestNonExplainableFailureFailsWithKernelReply(t *testing.T) {
	for _, code := range []string{"gateway_credits_exhausted", "provider_unavailable", "budget_exhausted", TurnHostContextExceeded, RunFailureCodeCompactionFailed, "agent_behavior_fixed", "model_image_input_unsupported"} {
		if turnFailureCanBeExplained(errors.New("failed"), code) {
			t.Fatalf("%s must not get an explanation step", code)
		}
	}
	for _, err := range []error{ErrTurnHostUnavailable, ErrBudgetExhausted, context.Canceled, ErrLeaseLost} {
		if turnFailureCanBeExplained(err, "execution_failed") {
			t.Fatalf("%v must not get an explanation step", err)
		}
	}
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		ctx := t.Context()
		run := claimedFailureRun(t, store, "kernel-reply")
		failed, _, err := NewRunActivityService(store, store).TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusFailed, LeaseOwner: run.LeaseOwner, Error: "provider down",
			FailureExplanation: beginRunFailureExplanation(run, RunFailureKindExecution, "provider_unavailable", "The model provider was unavailable.", "upstream 503 SECRET", "host_unavailable"),
			Output:             map[string]interface{}{"summary": "stale model text"},
			EventType:          "run.failed", Summary: "Run failed",
		})
		if err != nil {
			t.Fatal(err)
		}
		if failed.FailureExplanation.Explained || runExplainingFailure(failed) {
			t.Fatalf("unexplained failure marked explained: %+v", failed.FailureExplanation)
		}
		reply, ok := externalConversationRunReply(ctx, store, store, failed)
		if !ok || reply != TerminalFailureReply("provider_unavailable") || strings.Contains(reply, "SECRET") {
			t.Fatalf("kernel reply = %q %v", reply, ok)
		}
		if reason, err := NewRunCommandService(store).conversationRetryBlocker(ctx, failed); err != nil || reason != "" {
			t.Fatalf("unexplained failure must be retryable: %q %v", reason, err)
		}
	})
}

func TestCreditErrorFromTypedFailure(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		ctx := t.Context()
		run := claimedFailureRun(t, store, "credits")
		failed, _, err := NewRunActivityService(store, store).TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
			ExpectedRevision: run.Revision, Status: AgentRunStatusFailed, LeaseOwner: run.LeaseOwner, Error: "no credits",
			FailureExplanation: beginRunFailureExplanation(run, RunFailureKindCredits, "gateway_credits_exhausted", "This workspace has no model credits available.", "", ""),
			EventType:          "run.failed", Summary: "Run failed",
		})
		if err != nil {
			t.Fatal(err)
		}
		stored, err := store.GetAgentRun(ctx, run.Scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.CreditError == nil || stored.CreditError.Code != RunCreditErrorWorkspaceCreditsExhausted || failed.CreditError == nil {
			t.Fatalf("typed credit stop lost its credit error: %+v %+v", stored.CreditError, stored.FailureExplanation)
		}
		if terminalCreditError(AgentRunStatusRunning, stored.FailureExplanation) != nil || terminalCreditError(AgentRunStatusFailed, nil) != nil ||
			terminalCreditError(AgentRunStatusFailed, &RunFailureExplanation{Code: "provider_unavailable"}) != nil {
			t.Fatal("credit error from an unrelated failure")
		}
	})
}

func TestFailedRunAppendsRunFailedNoteToSession(t *testing.T) {
	ctx := t.Context()
	store := NewMemoryStore()
	now := time.Now().UTC().Truncate(time.Millisecond)
	scope := Scope{Kind: "tenant", ID: "run-failed-" + uuid.NewString()[:8]}
	owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: "finn"}
	service := NewConversationService(store)
	seedTestSpace(t, store, scope)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{Scope: scope, Owner: owner, Title: "Finn", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := service.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "kev"}, Intent: MessageIntentQuestion,
		Content: "Check my email", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, StartThread: true,
		RequiresResponse: true, IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger := posted.Message
	lane := conversationRunLane(conversation.ID, trigger.ID, owner.ID)
	created, err := NewRunCommandService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Kind: RunKindConversation, Source: RunSourceChat,
		Goal: "Check my email", ConcurrencyKey: lane, SessionID: ConversationAgentSessionID(scope, lane),
		Context:        map[string]interface{}{conversationRunContextConversationID: conversation.ID, conversationRunContextTriggerID: trigger.ID, "threadRootMessageId": trigger.ID},
		IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	now = time.Now().UTC().Add(time.Second).Truncate(time.Millisecond)
	run, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "w", Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || run == nil || run.ID != created.Run.ID {
		t.Fatalf("claim = %+v %v", run, err)
	}
	opened, err := store.OpenConversationAgentSession(ctx, OpenConversationAgentSessionRequest{
		Scope: scope, ConversationID: conversation.ID, ThreadRootID: trigger.ID, AgentID: owner.ID, RunID: run.ID,
		Lease: AgentRunLeaseGuard{WorkerID: "w", Now: now}, Now: now,
	})
	if err != nil || opened.Session.ID != run.SessionID {
		t.Fatalf("open session = %+v %v", opened, err)
	}
	activity := NewRunActivityService(store, store)
	activity.now = func() time.Time { return now }
	failed, _, err := activity.TransitionRun(ctx, scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusFailed, LeaseOwner: "w", Error: "provider down",
		FailureExplanation: beginRunFailureExplanation(run, RunFailureKindExecution, "provider_unavailable", "The model provider was unavailable.", "", ""),
		EventType:          "run.failed", Summary: "Run failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := projectFailedConversationReply(ctx, store, failed); err != nil {
			t.Fatalf("report %d: %v", attempt, err)
		}
	}
	reply, err := store.FindChannelMessageByIdempotencyKey(ctx, scope, conversation.ID, terminalConversationFailureReplyKey(failed))
	if err != nil || reply == nil || reply.Content != TerminalFailureReply("provider_unavailable") {
		t.Fatalf("kernel reply = %+v %v", reply, err)
	}
	notes, err := store.ListAgentSessionEntries(ctx, SessionEntryFilter{Scope: scope, SessionID: run.SessionID, NoteKinds: []NoteKind{NoteRunFailed}})
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("run_failed notes = %d, want 1", len(notes))
	}
	note := notes[0]
	var data map[string]string
	if note.RunID != run.ID || note.SourceKey != runFailedNoteSourceKey(failed) || json.Unmarshal(note.Note.Data, &data) != nil || data["messageId"] != reply.ID ||
		note.Note.Text != "Your previous attempt (run "+run.ID+") stopped and the user was told: "+`"`+reply.Content+`"` {
		t.Fatalf("note = %+v data=%v", note.Note, data)
	}
}

// claimedFailureRun is a leased Agent conversation Run in a fresh scope.
func claimedFailureRun(t *testing.T, store KernelStore, name string) *AgentRun {
	t.Helper()
	ctx := t.Context()
	scope := Scope{Kind: "tenant", ID: name + "-" + uuid.NewString()[:8]}
	created, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "Reply", Source: RunSourceChat, SessionID: "session-" + name,
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "worker", Now: time.Now().UTC(), LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || run == nil || run.ID != created.ID {
		t.Fatalf("claim = %+v %v", run, err)
	}
	return run
}

func TestExplicitActionFailureEnvelopePreservesResourceData(t *testing.T) {
	for _, output := range []map[string]interface{}{
		{"success": false, "error": "the connection failed"}, {"isError": true}, {"error": "The operation failed."}, {"success": false},
	} {
		if explicitActionResultFailure(output) == nil {
			t.Fatalf("explicit failure not recognized: %#v", output)
		}
	}
	for _, output := range []map[string]interface{}{
		{"status": "failed"}, {"ok": false}, {"success": true, "error": "historic diagnostic"}, {"result": map[string]interface{}{"success": false, "error": "document content"}},
	} {
		if err := explicitActionResultFailure(output); err != nil {
			t.Fatalf("fetched data was mistaken for operation failure: %#v %v", output, err)
		}
	}
}

// refusingNoteStore refuses or fails every session append with err.
type refusingNoteStore struct {
	AgentSessionStore
	err error
}

func (s refusingNoteStore) AppendAgentSessionEntries(context.Context, AppendAgentSessionEntriesRequest) (*AppendAgentSessionEntriesResult, error) {
	return nil, s.err
}

func TestRunFailedNoteRefusalDoesNotRetryAPostedReply(t *testing.T) {
	run := &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "t"}, SessionID: "session", Status: AgentRunStatusFailed}
	message := &ChannelMessage{ID: "reply", Content: "I stopped."}
	for _, refusal := range []error{ErrInvalidSessionEntry, ErrSessionMoved, ErrLeaseLost, ErrAgentSessionNotFound} {
		if err := appendRunFailedNote(t.Context(), refusingNoteStore{err: fmt.Errorf("append: %w", refusal)}, run, message); err != nil {
			t.Fatalf("%v: permanent refusal retried: %v", refusal, err)
		}
	}
	transient := errors.New("connection reset")
	if err := appendRunFailedNote(t.Context(), refusingNoteStore{err: transient}, run, message); !errors.Is(err, transient) {
		t.Fatalf("transient store error = %v, want it retried", err)
	}
}
