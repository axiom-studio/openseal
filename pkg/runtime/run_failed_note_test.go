package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeactivity "github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/google/uuid"
)

func TestFailedRunAppendsRunFailedNoteToSession(t *testing.T) {
	ctx := t.Context()
	store := conversationtest.NewStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	scope := models.Scope{Kind: "tenant", ID: "run-failed-" + uuid.NewString()[:8]}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "finn"}
	service := runtimeconversation.NewConversationService(store)
	conversationtest.SeedSpace(t, store, scope)
	conversation, _, err := service.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{Scope: scope, Owner: owner, Title: "Finn", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	posted, err := service.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "kev"}, Intent: models.MessageIntentQuestion,
		Content: "Check my email", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, StartThread: true,
		RequiresResponse: true, IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger := posted.Message
	lane := models.ConversationRunLane(conversation.ID, trigger.ID, owner.ID)
	created, err := agentrun.NewRunCommandService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Kind: models.RunKindConversation, Source: models.RunSourceChat,
		Goal: "Check my email", ConcurrencyKey: lane, SessionID: models.ConversationAgentSessionID(scope, lane),
		Context:        map[string]interface{}{models.ConversationRunConversationIDContextKey: conversation.ID, models.ConversationRunTriggerIDContextKey: trigger.ID, "threadRootMessageId": trigger.ID},
		IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	now = time.Now().UTC().Add(time.Second).Truncate(time.Millisecond)
	run, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: "w", Now: now, LeaseDuration: time.Hour, AgingInterval: time.Minute})
	if err != nil || run == nil || run.ID != created.Run.ID {
		t.Fatalf("claim = %+v %v", run, err)
	}
	opened, err := store.OpenConversationAgentSession(ctx, models.OpenConversationAgentSessionRequest{
		Scope: scope, ConversationID: conversation.ID, ThreadRootID: trigger.ID, AgentID: owner.ID, RunID: run.ID,
		Lease: models.AgentRunLeaseGuard{WorkerID: "w", Now: now}, Now: now,
	})
	if err != nil || opened.Session.ID != run.SessionID {
		t.Fatalf("open session = %+v %v", opened, err)
	}
	activity := runtimeactivity.NewRunActivityService(store, store, models.WithClock(func() time.Time { return now }))
	failed, _, err := activity.TransitionRun(ctx, scope, run.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: models.AgentRunStatusFailed, LeaseOwner: "w", Error: "provider down",
		FailureExplanation: runtimeconversation.BeginRunFailureExplanation(run, runtimeconversation.RunFailureKindExecution, "provider_unavailable", "The model provider was unavailable.", "", ""),
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
	if err != nil || reply == nil || reply.Content != runtimeconversation.TerminalFailureReply("provider_unavailable") {
		t.Fatalf("kernel reply = %+v %v", reply, err)
	}
	notes, err := store.ListAgentSessionEntries(ctx, models.SessionEntryFilter{Scope: scope, SessionID: run.SessionID, NoteKinds: []models.NoteKind{models.NoteRunFailed}})
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
	models.AgentSessionStore
	err error
}

func (s refusingNoteStore) AppendAgentSessionEntries(context.Context, models.AppendAgentSessionEntriesRequest) (*models.AppendAgentSessionEntriesResult, error) {
	return nil, s.err
}

func TestRunFailedNoteRefusalDoesNotRetryAPostedReply(t *testing.T) {
	run := &models.AgentRun{ID: "run", Scope: models.Scope{Kind: "tenant", ID: "t"}, SessionID: "session", Status: models.AgentRunStatusFailed}
	message := &models.ChannelMessage{ID: "reply", Content: "I stopped."}
	for _, refusal := range []error{models.ErrInvalidSessionEntry, models.ErrSessionMoved, models.ErrLeaseLost, models.ErrAgentSessionNotFound} {
		if err := appendRunFailedNote(t.Context(), refusingNoteStore{err: fmt.Errorf("append: %w", refusal)}, run, message); err != nil {
			t.Fatalf("%v: permanent refusal retried: %v", refusal, err)
		}
	}
	transient := errors.New("connection reset")
	if err := appendRunFailedNote(t.Context(), refusingNoteStore{err: transient}, run, message); !errors.Is(err, transient) {
		t.Fatalf("transient store error = %v, want it retried", err)
	}
}
