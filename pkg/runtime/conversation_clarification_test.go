package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type clarificationStore interface {
	KernelStore
	ConversationStore
}
type clarificationFixture struct {
	store        clarificationStore
	service      *ConversationService
	scheduler    *ConversationRunScheduler
	conversation *Conversation
	root, child  *AgentRun
	trigger      *ChannelMessage
}

func newClarificationFixture(t *testing.T, store clarificationStore, scopeID string) *clarificationFixture {
	t.Helper()
	ctx := t.Context()
	scope := Scope{Kind: "tenant", ID: scopeID}
	service := NewConversationService(store)
	conversation, _, err := service.CreateConversation(ctx, CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "writer"}, Title: "Writing", IdempotencyKey: "writing"})
	if err != nil {
		t.Fatal(err)
	}
	trigger := postConversationRunTestMessage(t, service, conversation, ConversationParticipantUser, MessageIntentQuestion, "Write a 1000 letter blog about a black hole", "request")
	scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{MessagePageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	created, _, err := scheduler.ScheduleMessage(ctx, scope, conversation.ID, trigger.ID)
	if err != nil {
		t.Fatal(err)
	}
	activity := NewRunActivityService(store, store)
	root, _, err := activity.TransitionRun(ctx, scope, created.Run.ID, RunTransitionRequest{ExpectedRevision: created.Run.Revision, Status: AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	root, _, err = activity.TransitionRun(ctx, scope, root.ID, RunTransitionRequest{ExpectedRevision: root.Revision, Status: AgentRunStatusWaitingForDependency, WakeCondition: &WakeCondition{Type: "run_dependencies", Reference: "writing"}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Kind: RunKindAgentWork, Owner: root.Owner, AssignedAgentID: "writer", ParentRunID: root.ID, Goal: "Draft the post", Source: RunSourceRequest})
	if err != nil {
		t.Fatal(err)
	}
	child, _, err = activity.TransitionRun(ctx, scope, child.ID, RunTransitionRequest{ExpectedRevision: child.Revision, Status: AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	child, _, err = activity.TransitionRun(ctx, scope, child.ID, RunTransitionRequest{ExpectedRevision: child.Revision, Status: AgentRunStatusWaitingForEvent, WakeCondition: &WakeCondition{Type: "user_message", Reference: "user:next_message"}, Output: map[string]interface{}{"summary": "Which audience should I use?"}, AppliedTurn: 1})
	if err != nil {
		t.Fatal(err)
	}
	return &clarificationFixture{store, service, scheduler, conversation, root, child, trigger}
}
func (f *clarificationFixture) question(t *testing.T) *ChannelMessage {
	t.Helper()
	if _, err := f.scheduler.ReconcileScope(t.Context(), f.root.Scope); err != nil {
		t.Fatal(err)
	}
	q, err := f.store.FindChannelMessageByIdempotencyKey(t.Context(), f.root.Scope, f.conversation.ID, clarificationQuestionKey(f.child))
	if err != nil || q == nil {
		t.Fatalf("missing projected question: %v", err)
	}
	return q
}
func (f *clarificationFixture) answer(t *testing.T, target string) *ChannelMessage {
	t.Helper()
	current, err := f.service.GetConversation(t.Context(), f.root.Scope, f.conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	posted, err := f.service.PostChannelMessage(t.Context(), PostChannelMessageRequest{Scope: f.root.Scope, ConversationID: current.ID, ExpectedRevision: current.Revision, Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "user"}, Intent: MessageIntentAnswer, Content: "General readers, conversational tone. Keep it to 1000 characters.", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ResolvesMessageID: target, IdempotencyKey: "answer"})
	if err != nil {
		t.Fatal(err)
	}
	return posted.Message
}
func TestConversationClarificationRoundTrip(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store clarificationStore = NewMemoryStore()
			if backend == "sqlite" {
				db, err := NewSQLiteStore(filepath.Join(t.TempDir(), "state.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				store = db
			}
			f := newClarificationFixture(t, store, "round-trip")
			q := f.question(t)
			if q.Content != "Which audience should I use?" || !q.RequiresResponse || q.ReplyToMessageID != f.trigger.ID {
				t.Fatalf("question = %#v", q)
			}
			// Recreate the scheduler to exercise durable, rather than in-memory, deduplication.
			f.scheduler, _ = NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
			f.question(t)
			msgs, _ := f.service.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: f.root.Scope, ConversationID: f.conversation.ID})
			if len(msgs) != 2 {
				t.Fatalf("question duplicated: %d", len(msgs))
			}
			answer := f.answer(t, "")
			resumed, replayed, err := f.scheduler.ScheduleMessage(t.Context(), f.root.Scope, f.conversation.ID, answer.ID)
			if err != nil || replayed || resumed == nil || resumed.Run.ID != f.child.ID || resumed.Run.Status != AgentRunStatusQueued || resumed.Run.WakeCondition != nil {
				t.Fatalf("resume=%#v replay=%v err=%v", resumed, replayed, err)
			}
			if len(resumed.Run.PendingInterventions) != 1 || !strings.Contains(resumed.Run.PendingInterventions[0].Instruction, answer.Content) {
				t.Fatal("answer did not reach durable model input")
			}
			// Completion before scheduling retry must not create another conversation Run.
			activity := NewRunActivityService(store, store)
			running, _, err := activity.TransitionRun(t.Context(), f.root.Scope, f.child.ID, RunTransitionRequest{ExpectedRevision: resumed.Run.Revision, Status: AgentRunStatusRunning})
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = activity.TransitionRun(t.Context(), f.root.Scope, f.child.ID, RunTransitionRequest{ExpectedRevision: running.Revision, Status: AgentRunStatusCompleted})
			if err != nil {
				t.Fatal(err)
			}
			f.scheduler, _ = NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
			again, replayed, err := f.scheduler.ScheduleMessage(t.Context(), f.root.Scope, f.conversation.ID, answer.ID)
			if err != nil || !replayed || again == nil || again.Run.ID != f.child.ID {
				t.Fatalf("replay=%#v %v %v", again, replayed, err)
			}
			if _, err = f.scheduler.ReconcileScope(t.Context(), f.root.Scope); err != nil {
				t.Fatal(err)
			}
			roots, _ := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: f.root.Scope, Kind: RunKindConversation})
			if len(roots) != 1 {
				t.Fatalf("answer started %d conversation runs", len(roots))
			}
		})
	}
}

func TestConversationClarificationTargetsOnlyTheRepliedTask(t *testing.T) {
	f := newClarificationFixture(t, NewMemoryStore(), "targets")
	q := f.question(t)
	other := cloneAgentRun(f.child)
	other.ID = "other-child"
	other.Revision = 1
	if err := f.store.CreateAgentRun(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	if _, err := f.scheduler.ReconcileScope(t.Context(), f.root.Scope); err != nil {
		t.Fatal(err)
	}
	answer := f.answer(t, "")
	// More than one waiting task: do not guess what an unthreaded message answers.
	if _, handled, err := f.scheduler.resumeConversationAnswer(t.Context(), f.conversation, answer); err != nil || handled {
		t.Fatalf("ambiguous reply consumed: %v %v", handled, err)
	}
	answer.ResolvesMessageID = q.ID
	result, handled, err := f.scheduler.resumeConversationAnswer(t.Context(), f.conversation, answer)
	if err != nil || !handled || result.Run.ID != f.child.ID {
		t.Fatalf("explicit reply = %#v %v %v", result, handled, err)
	}
	unchanged, _ := f.store.GetAgentRun(t.Context(), f.root.Scope, other.ID)
	if unchanged.Status != AgentRunStatusWaitingForEvent {
		t.Fatal("unrelated task woke")
	}
}

type failingAnswerProjectionStore struct {
	ConversationStore
	fail bool
}

func (s *failingAnswerProjectionStore) CommitChannelMessage(ctx context.Context, record ChannelMessageCommitRecord) (*ChannelMessageCommitResult, error) {
	if s.fail && strings.HasPrefix(record.Message.IdempotencyKey, "conversation-answer-received:") {
		return nil, errors.New("simulated process failure after wake")
	}
	return s.ConversationStore.CommitChannelMessage(ctx, record)
}
func TestConversationClarificationRecoversAfterWakeBeforeAcknowledgment(t *testing.T) {
	f := newClarificationFixture(t, NewMemoryStore(), "crash")
	q := f.question(t)
	answer := f.answer(t, q.ID)
	failing := &failingAnswerProjectionStore{ConversationStore: f.store, fail: true}
	f.scheduler, _ = NewConversationRunScheduler(failing, f.store, ConversationRunSchedulerConfig{})
	if _, _, err := f.scheduler.ScheduleMessage(t.Context(), f.root.Scope, f.conversation.ID, answer.ID); err == nil {
		t.Fatal("expected simulated failure")
	}
	f.scheduler, _ = NewConversationRunScheduler(f.store, f.store, ConversationRunSchedulerConfig{})
	result, replayed, err := f.scheduler.ScheduleMessage(t.Context(), f.root.Scope, f.conversation.ID, answer.ID)
	if err != nil || !replayed || result == nil || result.Run.ID != f.child.ID || len(result.Run.PendingInterventions) != 1 {
		t.Fatalf("recovered=%#v %v %v", result, replayed, err)
	}
}
func TestConversationWorkInputPreservesOriginalRequestAndOwnerBoundary(t *testing.T) {
	f := newClarificationFixture(t, NewMemoryStore(), "context")
	runner := conversationWorkTurnRunner{runs: f.store, conversations: f.store, inner: TurnRunnerFunc(func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
		source, _ := input.Run.Context["conversationRequest"].(map[string]interface{})
		if source["content"] != f.trigger.Content {
			t.Fatalf("original length constraint lost: %#v", source)
		}
		return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted}, nil
	})}
	if _, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: f.child}); err != nil {
		t.Fatal(err)
	}
	if f.child.Context["conversationRequest"] != nil {
		t.Fatal("request-time input mutated stored child")
	}
	other := cloneAgentRun(f.child)
	other.Owner.ID = "another-agent"
	_, conversation, _, err := conversationWorkOrigin(t.Context(), f.store, f.store, other)
	if err != nil || conversation != nil {
		t.Fatal("other Agent received private conversation")
	}
	other = cloneAgentRun(f.child)
	other.Scope.ID = "other-tenant"
	_, conversation, _, err = conversationWorkOrigin(t.Context(), f.store, f.store, other)
	if err != nil || conversation != nil {
		t.Fatal("cross-tenant origin resolved")
	}
	// Ordinary events must never become clarification questions.
	changed, _, err := NewRunActivityService(f.store, f.store).TransitionRun(t.Context(), f.root.Scope, f.child.ID, RunTransitionRequest{ExpectedRevision: f.child.Revision, Status: AgentRunStatusQueued})
	if err != nil {
		t.Fatal(err)
	}
	changed, _, err = NewRunActivityService(f.store, f.store).TransitionRun(t.Context(), f.root.Scope, f.child.ID, RunTransitionRequest{ExpectedRevision: changed.Revision, Status: AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	wakeAt := time.Now().Add(time.Hour)
	_, _, err = NewRunActivityService(f.store, f.store).TransitionRun(t.Context(), f.root.Scope, f.child.ID, RunTransitionRequest{ExpectedRevision: changed.Revision, Status: AgentRunStatusWaitingForEvent, WakeCondition: &WakeCondition{Type: "webhook", WakeAt: &wakeAt}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.scheduler.ReconcileScope(t.Context(), f.root.Scope); err != nil {
		t.Fatal(err)
	}
	msgs, _ := f.service.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: f.root.Scope, ConversationID: f.conversation.ID})
	if len(msgs) != 1 {
		t.Fatal("external event projected as user question")
	}
}
