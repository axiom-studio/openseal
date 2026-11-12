package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/google/uuid"
)

func TestFailedRunAppendsRunFailedNoteToSession(t *testing.T) {
	ctx := t.Context()
	store := newTestKernelStore(t)
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
	lane := ConversationRunLane(conversation.ID, trigger.ID, owner.ID)
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
	activity := NewRunActivityService(store, store, models.WithClock(func() time.Time { return now }))
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
