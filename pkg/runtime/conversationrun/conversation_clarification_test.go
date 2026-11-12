package conversationrun_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/control/controltest"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun/conversationruntest"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

type clarificationStore = controltest.AskUserStore
type clarificationFixture struct {
	store        clarificationStore
	service      *runtimeconversation.Service
	scheduler    *conversationrun.Scheduler
	conversation *models.Conversation
	root, child  *models.AgentRun
	trigger      *models.ChannelMessage
}

func newClarificationFixture(t *testing.T, store clarificationStore, scopeID string) *clarificationFixture {
	t.Helper()
	ctx := t.Context()
	scope := models.Scope{Kind: "tenant", ID: scopeID}
	service := runtimeconversation.NewService(store)
	conversationtest.SeedSpace(t, store.(models.SpaceStore), scope)
	conversation, _, err := service.CreateConversation(ctx, runtimeconversation.CreateConversationRequest{Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "writer"}, Title: "Writing", IdempotencyKey: "writing"})
	if err != nil {
		t.Fatal(err)
	}
	trigger := conversationruntest.PostMessage(t, service, conversation, models.ConversationParticipantUser, models.MessageIntentQuestion, "Write a 1000 letter blog about a black hole", "request")
	scheduler, err := conversationrun.NewScheduler(store, store, conversationrun.SchedulerConfig{MessagePageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	created, _, err := scheduler.ScheduleMessage(ctx, scope, conversation.ID, trigger.ID)
	if err != nil {
		t.Fatal(err)
	}
	activities := activity.NewRunActivityService(store, store)
	root, _, err := activities.TransitionRun(ctx, scope, created.Run.ID, activity.RunTransitionRequest{ExpectedRevision: created.Run.Revision, Status: models.AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	root, _, err = activities.TransitionRun(ctx, scope, root.ID, activity.RunTransitionRequest{ExpectedRevision: root.Revision, Status: models.AgentRunStatusWaitingForDependency, WakeCondition: &models.WakeCondition{Type: "run_dependencies", Reference: "writing"}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := portfolio.NewService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{Scope: scope, Kind: models.RunKindAgentWork, Owner: root.Owner, AssignedAgentID: "writer", ParentRunID: root.ID, Goal: "Draft the post", Source: models.RunSourceRequest})
	if err != nil {
		t.Fatal(err)
	}
	child, _ = controltest.AskUserParked(t, store, controltest.ClaimRun(t, store, child, "child-worker"), "child-worker", "Which audience should I use?")
	return &clarificationFixture{store, service, scheduler, conversation, root, child, trigger}
}
func (f *clarificationFixture) question(t *testing.T) *models.ChannelMessage {
	t.Helper()
	if _, err := f.scheduler.ReconcileScope(t.Context(), f.root.Scope); err != nil {
		t.Fatal(err)
	}
	q, err := f.store.FindChannelMessageByIdempotencyKey(t.Context(), f.root.Scope, f.conversation.ID, clarificationQuestionKeyOf(f.child))
	if err != nil || q == nil {
		t.Fatalf("missing projected question: %v", err)
	}
	return q
}
func (f *clarificationFixture) answer(t *testing.T, target string) *models.ChannelMessage {
	t.Helper()
	current, err := f.service.GetConversation(t.Context(), f.root.Scope, f.conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	posted, err := f.service.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{Scope: f.root.Scope, ConversationID: current.ID, ExpectedRevision: current.Revision, Sender: f.trigger.Sender, Intent: models.MessageIntentAnswer, Content: "General readers, conversational tone. Keep it to 1000 characters.", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, ResolvesMessageID: target, IdempotencyKey: "answer"})
	if err != nil {
		t.Fatal(err)
	}
	return posted.Message
}
func TestConversationClarificationRoundTrip(t *testing.T) {
	for _, backend := range []string{"sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store clarificationStore = kerneltest.NewStore(t)
			if backend == "sqlite" {
				db, err := sqlite.New(filepath.Join(t.TempDir(), "state.db"))
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
			f.scheduler, _ = conversationrun.NewScheduler(store, store, conversationrun.SchedulerConfig{})
			f.question(t)
			msgs, _ := f.service.ListChannelMessages(t.Context(), models.ChannelMessageFilter{Scope: f.root.Scope, ConversationID: f.conversation.ID})
			if len(msgs) != 2 {
				t.Fatalf("question duplicated: %d", len(msgs))
			}
			answer := f.answer(t, "")
			resumed, replayed, err := f.scheduler.ScheduleMessage(t.Context(), f.root.Scope, f.conversation.ID, answer.ID)
			if err != nil || replayed || resumed == nil || resumed.Run.ID != f.child.ID || resumed.Run.Status != models.AgentRunStatusQueued || resumed.Run.WakeCondition != nil {
				t.Fatalf("resume=%#v replay=%v err=%v", resumed, replayed, err)
			}
			if len(resumed.Run.PendingInterventions) != 1 || !strings.Contains(resumed.Run.PendingInterventions[0].Instruction, answer.Content) {
				t.Fatal("answer did not reach durable model input")
			}
			// Completion before scheduling retry must not create another conversation Run.
			activities := activity.NewRunActivityService(store, store)
			running, _, err := activities.TransitionRun(t.Context(), f.root.Scope, f.child.ID, activity.RunTransitionRequest{ExpectedRevision: resumed.Run.Revision, Status: models.AgentRunStatusRunning})
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = activities.TransitionRun(t.Context(), f.root.Scope, f.child.ID, activity.RunTransitionRequest{ExpectedRevision: running.Revision, Status: models.AgentRunStatusCompleted})
			if err != nil {
				t.Fatal(err)
			}
			f.scheduler, _ = conversationrun.NewScheduler(store, store, conversationrun.SchedulerConfig{})
			again, replayed, err := f.scheduler.ScheduleMessage(t.Context(), f.root.Scope, f.conversation.ID, answer.ID)
			if err != nil || !replayed || again == nil || again.Run.ID != f.child.ID {
				t.Fatalf("replay=%#v %v %v", again, replayed, err)
			}
			if _, err = f.scheduler.ReconcileScope(t.Context(), f.root.Scope); err != nil {
				t.Fatal(err)
			}
			roots, _ := store.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: f.root.Scope, Kind: models.RunKindConversation})
			if len(roots) != 1 {
				t.Fatalf("answer started %d conversation runs", len(roots))
			}
		})
	}
}

type failingAnswerProjectionStore struct {
	models.ConversationStore
	fail bool
}

func (s *failingAnswerProjectionStore) CommitChannelMessage(ctx context.Context, record models.ChannelMessageCommitRecord) (*models.ChannelMessageCommitResult, error) {
	if s.fail && strings.HasPrefix(record.Message.IdempotencyKey, "conversation-answer-received:") {
		return nil, errors.New("simulated process failure after wake")
	}
	return s.ConversationStore.CommitChannelMessage(ctx, record)
}
func TestConversationClarificationRecoversAfterWakeBeforeAcknowledgment(t *testing.T) {
	f := newClarificationFixture(t, kerneltest.NewStore(t), "crash")
	q := f.question(t)
	answer := f.answer(t, q.ID)
	failing := &failingAnswerProjectionStore{ConversationStore: f.store, fail: true}
	f.scheduler, _ = conversationrun.NewScheduler(failing, f.store, conversationrun.SchedulerConfig{})
	if _, _, err := f.scheduler.ScheduleMessage(t.Context(), f.root.Scope, f.conversation.ID, answer.ID); err == nil {
		t.Fatal("expected simulated failure")
	}
	f.scheduler, _ = conversationrun.NewScheduler(f.store, f.store, conversationrun.SchedulerConfig{})
	result, replayed, err := f.scheduler.ScheduleMessage(t.Context(), f.root.Scope, f.conversation.ID, answer.ID)
	if err != nil || !replayed || result == nil || result.Run.ID != f.child.ID || len(result.Run.PendingInterventions) != 1 {
		t.Fatalf("recovered=%#v %v %v", result, replayed, err)
	}
}
func TestConversationWorkOriginKeepsOwnerBoundary(t *testing.T) {
	f := newClarificationFixture(t, kerneltest.NewStore(t), "context")
	other := models.CloneAgentRun(f.child)
	other.Owner.ID = "another-agent"
	_, conversation, _, err := runtimeconversation.WorkOrigin(t.Context(), f.store, f.store, other)
	if err != nil || conversation != nil {
		t.Fatal("other Agent received private conversation")
	}
	other = models.CloneAgentRun(f.child)
	other.Scope.ID = "other-tenant"
	_, conversation, _, err = runtimeconversation.WorkOrigin(t.Context(), f.store, f.store, other)
	if err != nil || conversation != nil {
		t.Fatal("cross-tenant origin resolved")
	}
	// Ordinary events must never become clarification questions.
	changed, _, err := activity.NewRunActivityService(f.store, f.store).TransitionRun(t.Context(), f.root.Scope, f.child.ID, activity.RunTransitionRequest{ExpectedRevision: f.child.Revision, Status: models.AgentRunStatusQueued})
	if err != nil {
		t.Fatal(err)
	}
	changed, _, err = activity.NewRunActivityService(f.store, f.store).TransitionRun(t.Context(), f.root.Scope, f.child.ID, activity.RunTransitionRequest{ExpectedRevision: changed.Revision, Status: models.AgentRunStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	wakeAt := time.Now().Add(time.Hour)
	_, _, err = activity.NewRunActivityService(f.store, f.store).TransitionRun(t.Context(), f.root.Scope, f.child.ID, activity.RunTransitionRequest{ExpectedRevision: changed.Revision, Status: models.AgentRunStatusWaitingForEvent, WakeCondition: &models.WakeCondition{Type: "webhook", WakeAt: &wakeAt}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.scheduler.ReconcileScope(t.Context(), f.root.Scope); err != nil {
		t.Fatal(err)
	}
	msgs, _ := f.service.ListChannelMessages(t.Context(), models.ChannelMessageFilter{Scope: f.root.Scope, ConversationID: f.conversation.ID})
	if len(msgs) != 2 { // the trigger and the earlier ask_user question
		t.Fatal("external event projected as user question")
	}
}
