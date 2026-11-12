package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
)

type conversationContinuationTestStore interface {
	models.ConversationTaskKernelStore
	models.ConversationTaskPromotionStore
	models.ConversationTaskDueStore
}

func forConversationContinuationStores(t *testing.T, test func(*testing.T, conversationContinuationTestStore, func(*testing.T) conversationContinuationTestStore)) {
	t.Helper()
	forConversationTaskSQLStores(t, func(t *testing.T, fixture conversationTaskSQLTestFixture) {
		test(t, fixture.store.(conversationContinuationTestStore), func(t *testing.T) conversationContinuationTestStore {
			return fixture.reopen(t).(conversationContinuationTestStore)
		})
	})
}

func conversationContinuationFixture(t *testing.T, store conversationContinuationTestStore, status models.AgentRunStatus, spoken bool) (*models.AgentRun, *models.Conversation, *models.ChannelMessage) {
	t.Helper()
	scope := models.Scope{Kind: "tenant", ID: "continuation"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "continuation-agent"}
	service := runtimeconversation.NewService(store)
	seedTestSpace(t, store.(models.SpaceStore), scope)
	conversation, _, err := service.CreateConversation(t.Context(), runtimeconversation.CreateConversationRequest{Scope: scope, Owner: owner, Title: "Continuation", IdempotencyKey: "continuation"})
	if err != nil {
		t.Fatal(err)
	}
	mode := ""
	if spoken {
		mode = "spoken"
	}
	posted, err := service.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "requester"}, Intent: models.MessageIntentQuestion, Content: "Research this request and save the requested result.",
		Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, RequiresResponse: true, ResponseMode: mode, IdempotencyKey: "continuation-source"})
	if err != nil {
		t.Fatal(err)
	}
	portfolio := portfolio.NewService(store, models.WithClock(func() time.Time { return time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond) }))
	run, err := portfolio.CreateAgentRun(t.Context(), models.CreateAgentRunRequest{Scope: scope, Kind: models.RunKindConversation, Owner: owner, AssignedAgentID: owner.ID,
		ConcurrencyKey: models.ConversationRunLane(conversation.ID, "", owner.ID), Goal: "Respond to an Agent channel message", Source: models.RunSourceChat, Budget: &models.BudgetPolicy{MaxTurns: 20, MaxAttempts: 30, MaxDurationMS: 600000},
		Context: map[string]interface{}{models.ConversationRunConversationIDContextKey: conversation.ID, models.ConversationRunTriggerIDContextKey: posted.Message.ID},
		Plan:    map[string]interface{}{"accepted": "plan"}, Checkpoint: map[string]interface{}{"saved": map[string]interface{}{"actionCallId": "existing-action", "status": "running"}}})
	if err != nil {
		t.Fatal(err)
	}
	previous := run.Revision
	run.Status = status
	if status == models.AgentRunStatusRunning {
		expires := time.Now().Add(time.Minute)
		run.LeaseOwner = "same-worker"
		run.LeaseExpiresAt = &expires
	}
	if status == models.AgentRunStatusWaitingForApproval {
		run.WakeCondition = &models.WakeCondition{Type: "approval", Reference: "pending-approval"}
	}
	run.BudgetUsage = models.BudgetUsage{Attempts: 1, Turns: 1, DurationMS: 321}
	run.BudgetReservations = map[string]models.BudgetReservation{"existing-turn": {ID: "existing-turn", Usage: models.BudgetUsage{Turns: 1, DurationMS: 1000}, CreatedAt: run.CreatedAt}}
	run.PendingInterventions = []models.AgentRunIntervention{{ID: "steering", Actor: models.ActivityActor{Type: "user", ID: "requester"}, Instruction: "Preserve the saved guidance", CreatedAt: run.CreatedAt}}
	run.Revision++
	updateConversationTaskSQLRun(t, store, run, previous)
	canonical, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return canonical, posted.Conversation, posted.Message
}

func TestConversationTaskContinuationStorePreservesExecutionAndAcknowledgesAtomically(t *testing.T) {
	for _, status := range []models.AgentRunStatus{models.AgentRunStatusQueued, models.AgentRunStatusRunning, models.AgentRunStatusWaitingForApproval} {
		t.Run(string(status), func(t *testing.T) {
			forConversationContinuationStores(t, func(t *testing.T, store conversationContinuationTestStore, reopen func(*testing.T) conversationContinuationTestStore) {
				source, conversation, trigger := conversationContinuationFixture(t, store, status, true)
				deadline := source.CreatedAt.Add(models.ConversationTaskForegroundTimeout)
				before, err := store.PromoteConversationTask(t.Context(), models.ConversationTaskPromotionRequest{Scope: source.Scope, RunID: source.ID, Now: deadline.Add(-time.Nanosecond)})
				if err != nil || before != nil {
					t.Fatalf("promoted before deadline: %#v %v", before, err)
				}
				accepted, err := store.PromoteConversationTask(t.Context(), models.ConversationTaskPromotionRequest{Scope: source.Scope, RunID: source.ID, Now: deadline})
				if err != nil {
					t.Fatal(err)
				}
				if accepted == nil || accepted.Task.Mode != models.ConversationTaskModeContinuation || accepted.Task.SourceRunID != source.ID || accepted.Task.WorkRunID != source.ID ||
					accepted.Task.SourceTurnID != "" || accepted.Task.SourceTurnNumber != 0 || !models.ConversationTaskMatchesWorkRun(accepted.Task, accepted.WorkRun) {
					t.Fatalf("lost exact continuation identity: %#v", accepted)
				}
				expected := models.CloneAgentRun(source)
				expected.Context[models.ConversationTaskContextKey] = accepted.Task.ID
				expected.ConcurrencyKey = "task:" + accepted.Task.ID
				expected.Revision++
				expected.UpdatedAt = deadline.UTC()
				want, _ := json.Marshal(expected)
				got, _ := json.Marshal(accepted.WorkRun)
				if string(want) != string(got) {
					t.Fatalf("promotion changed execution state\nwant %s\ngot %s", want, got)
				}
				ack, err := store.GetChannelMessage(t.Context(), source.Scope, conversation.ID, accepted.AcknowledgmentMessageID)
				if err != nil || ack == nil || ack.Content != accepted.Task.Acknowledgment || ack.Intent != models.MessageIntentUpdate || ack.ResponseMode != "spoken" ||
					ack.ReplyToMessageID != trigger.ID || ack.ResolvesMessageID != "" || len(ack.References) != 2 {
					t.Fatalf("acknowledgment was not committed with work: %#v %v", ack, err)
				}
				restarted := reopen(t)
				again, err := restarted.PromoteConversationTask(t.Context(), models.ConversationTaskPromotionRequest{Scope: source.Scope, RunID: source.ID, Now: deadline.Add(time.Second)})
				if err != nil || again == nil || !again.Replayed || again.Task.ID != accepted.Task.ID || again.AcknowledgmentMessageID != ack.ID {
					t.Fatalf("restart duplicated handoff: %#v %v", again, err)
				}
				runs, err := restarted.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: source.Scope, Limit: 100})
				if err != nil || len(runs) != 1 {
					t.Fatalf("handoff created another Run: %d %v", len(runs), err)
				}
				due, err := restarted.ListDueConversationTaskRuns(t.Context(), models.ConversationTaskDueFilter{Scope: source.Scope, BeforeCreatedAt: deadline, Limit: 10})
				if err != nil || len(due) != 0 {
					t.Fatalf("promoted Run remained foreground due work: %#v %v", due, err)
				}
			})
		})
	}
}

func TestConversationTaskContinuationStoreFinalAnswerWinsHandoff(t *testing.T) {
	forConversationContinuationStores(t, func(t *testing.T, store conversationContinuationTestStore, _ func(*testing.T) conversationContinuationTestStore) {
		source, conversation, trigger := conversationContinuationFixture(t, store, models.AgentRunStatusRunning, false)
		_, err := runtimeconversation.NewService(store).PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{Scope: source.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
			Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: source.AssignedAgentID}, Intent: models.MessageIntentAnswer, Content: "The result is complete.",
			Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, ReplyToMessageID: trigger.ID, ResolvesMessageID: trigger.ID,
			References: []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: source.ID}}, IdempotencyKey: models.ConversationTaskFinalResponseKey(source)})
		if err != nil {
			t.Fatal(err)
		}
		result, err := store.PromoteConversationTask(t.Context(), models.ConversationTaskPromotionRequest{Scope: source.Scope, RunID: source.ID, Now: time.Now()})
		if err != nil || result != nil {
			t.Fatalf("already answered work was handed off: %#v %v", result, err)
		}
		current, err := store.GetAgentRun(t.Context(), source.Scope, source.ID)
		if err != nil || !reflect.DeepEqual(current, source) {
			t.Fatalf("rejected promotion changed Run: %#v %v", current, err)
		}
	})
}

func TestConversationTaskContinuationStoreConcurrentReplay(t *testing.T) {
	forConversationContinuationStores(t, func(t *testing.T, store conversationContinuationTestStore, _ func(*testing.T) conversationContinuationTestStore) {
		source, conversation, _ := conversationContinuationFixture(t, store, models.AgentRunStatusRunning, false)
		var wg sync.WaitGroup
		results := make(chan *models.ConversationTaskResult, 8)
		errs := make(chan error, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				result, err := store.PromoteConversationTask(context.Background(), models.ConversationTaskPromotionRequest{Scope: source.Scope, RunID: source.ID, Now: time.Now()})
				results <- result
				errs <- err
			}()
		}
		wg.Wait()
		close(results)
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		id := ""
		for result := range results {
			if result == nil {
				t.Fatal("missing accepted continuation")
			}
			if id == "" {
				id = result.Task.ID
			}
			if result.Task.ID != id || result.WorkRun.ID != source.ID {
				t.Fatal("replay changed exact work")
			}
		}
		messages, err := store.ListChannelMessages(t.Context(), models.ChannelMessageFilter{Scope: source.Scope, ConversationID: conversation.ID, Limit: 100})
		if err != nil || len(messages) != 2 {
			t.Fatalf("replay duplicated acknowledgment: %d %v", len(messages), err)
		}
	})
}

func TestConversationTaskContinuationStoreRejectsWeakenedDeadline(t *testing.T) {
	store := kerneltest.NewStore(t)
	source, _, _ := conversationContinuationFixture(t, store, models.AgentRunStatusQueued, false)
	_, err := store.PromoteConversationTask(t.Context(), models.ConversationTaskPromotionRequest{Scope: source.Scope, RunID: source.ID, Now: time.Now(), MinimumAge: time.Second})
	if !errors.Is(err, models.ErrInvalidConversationTask) {
		t.Fatalf("weakened policy accepted: %v", err)
	}
}

func TestConversationTaskContinuationStoreDueNullMarkerAndKeyset(t *testing.T) {
	forConversationContinuationStores(t, func(t *testing.T, store conversationContinuationTestStore, _ func(*testing.T) conversationContinuationTestStore) {
		source, _, _ := conversationContinuationFixture(t, store, models.AgentRunStatusQueued, false)
		previous := source.Revision
		source.Context[models.ConversationTaskContextKey] = nil
		source.Revision++
		updateConversationTaskSQLRun(t, store, source, previous)
		other := models.CloneAgentRun(source)
		other.ID = "null-marker-other"
		other.RootRunID = other.ID
		other.Revision = 1
		if err := store.CreateAgentRun(t.Context(), other); err != nil {
			t.Fatal(err)
		}
		filter := models.ConversationTaskDueFilter{Scope: source.Scope, BeforeCreatedAt: time.Now(), Limit: 100}
		page, err := store.ListDueConversationTaskRuns(t.Context(), filter)
		if err != nil || len(page) != 2 {
			t.Fatalf("nullable marker hid overdue roots: %#v %v", page, err)
		}
		if page[0].ID >= page[1].ID || !page[0].CreatedAt.Equal(page[1].CreatedAt) {
			t.Fatalf("same-time roots lost stable order: %#v", page)
		}
		filter.AfterCreatedAt = &page[0].CreatedAt
		filter.AfterID = page[0].ID
		after, err := store.ListDueConversationTaskRuns(t.Context(), filter)
		if err != nil || len(after) != 1 || after[0].ID != page[1].ID {
			t.Fatalf("due cursor repeated or skipped work: %#v %v", after, err)
		}
		accepted, err := store.PromoteConversationTask(t.Context(), models.ConversationTaskPromotionRequest{Scope: source.Scope, RunID: source.ID, Now: time.Now()})
		if err != nil || accepted == nil {
			t.Fatalf("nullable marker prevented handoff: %#v %v", accepted, err)
		}
		filter.AfterCreatedAt = nil
		filter.AfterID = ""
		due, err := store.ListDueConversationTaskRuns(t.Context(), filter)
		if err != nil || len(due) != 1 || due[0].ID != other.ID {
			t.Fatalf("index retained promoted work: %#v %v", due, err)
		}
	})
}

func TestConversationTaskContinuationStoreFinishedTurnWinsHandoff(t *testing.T) {
	forConversationContinuationStores(t, func(t *testing.T, store conversationContinuationTestStore, _ func(*testing.T) conversationContinuationTestStore) {
		source, conversation, _ := conversationContinuationFixture(t, store, models.AgentRunStatusRunning, false)
		now := time.Now().UTC()
		turn := &models.AgentTurn{ID: "finished-original-turn", Scope: source.Scope, RunID: source.ID, Sequence: source.LastAppliedTurn + 1,
			Status: models.AgentTurnStatusCompleted, Final: true,
			Revision: 1, CreatedAt: source.CreatedAt, UpdatedAt: now, StartedAt: source.CreatedAt, CompletedAt: &now}
		if _, err := store.CreateAgentTurn(t.Context(), turn); err != nil {
			t.Fatal(err)
		}
		accepted, err := store.PromoteConversationTask(t.Context(), models.ConversationTaskPromotionRequest{Scope: source.Scope, RunID: source.ID, Now: now})
		if err != nil || accepted != nil {
			t.Fatalf("finished original turn was handed off: %#v %v", accepted, err)
		}
		current, err := store.GetAgentRun(t.Context(), source.Scope, source.ID)
		if err != nil || !reflect.DeepEqual(current, source) {
			t.Fatalf("finished-turn rejection changed accepted execution: %#v %v", current, err)
		}
		messages, err := store.ListChannelMessages(t.Context(), models.ChannelMessageFilter{Scope: source.Scope, ConversationID: conversation.ID, Limit: 100})
		if err != nil || len(messages) != 1 {
			t.Fatalf("finished-turn rejection emitted acknowledgment: %#v %v", messages, err)
		}
	})
}
