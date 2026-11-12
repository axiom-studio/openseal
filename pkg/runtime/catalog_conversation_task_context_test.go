package runtime

import (
	"context"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun/conversationruntest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

type countedCatalogTaskContextStore struct {
	*sqlite.Store
	runReads, taskReads, taskLists int
}

func (s *countedCatalogTaskContextStore) GetAgentRun(ctx context.Context, scope models.Scope, id string) (*models.AgentRun, error) {
	s.runReads++
	return s.Store.GetAgentRun(ctx, scope, id)
}

func (s *countedCatalogTaskContextStore) FindConversationTaskByWorkRunID(ctx context.Context, scope models.Scope, id string) (*models.ConversationTask, error) {
	s.taskReads++
	return s.Store.FindConversationTaskByWorkRunID(ctx, scope, id)
}

func (s *countedCatalogTaskContextStore) ListConversationTasks(ctx context.Context, filter models.ConversationTaskFilter) ([]*models.ConversationTaskResult, error) {
	s.taskLists++
	return s.Store.ListConversationTasks(ctx, filter)
}

func TestCatalogConversationTaskContextSkipsOrdinaryRunReads(t *testing.T) {
	store := &countedCatalogTaskContextStore{Store: newTestKernelStore(t)}
	for name, input := range map[string]map[string]interface{}{
		"non-chat":           {},
		"partial context":    {models.ConversationRunConversationIDContextKey: "chat"},
		"non-string trigger": {models.ConversationRunConversationIDContextKey: "chat", models.ConversationRunTriggerIDContextKey: true},
		"empty trigger":      {models.ConversationRunConversationIDContextKey: "chat", models.ConversationRunTriggerIDContextKey: ""},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := conversationrun.ResolveCatalogConversationTaskContext(t.Context(), nil, &models.AgentRun{ID: "ordinary", Kind: models.RunKindAgentWork, Context: input}, store)
			if err != nil || result != nil || store.runReads != 0 || store.taskReads != 0 || store.taskLists != 0 {
				t.Fatalf("ordinary work added task reads: result=%#v err=%v runs=%d tasks=%d lists=%d", result, err, store.runReads, store.taskReads, store.taskLists)
			}
		})
	}
}

func TestCatalogConversationTaskContextSkipsUnmarkedChatChildTaskLookups(t *testing.T) {
	kernel := newTestKernelStore(t)
	source, _, _, _ := conversationruntest.SettledTaskSource(t, kernel)
	child, err := portfolio.NewPortfolioService(kernel).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: source.Scope, Owner: source.Owner, Kind: models.RunKindAgentWork, ParentRunID: source.ID, AssignedAgentID: source.AssignedAgentID,
		Goal: "Ordinary delegated operation", Source: models.RunSourceFork, Context: maps.Clone(source.Context),
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &countedCatalogTaskContextStore{Store: kernel}
	result, err := conversationrun.ResolveCatalogConversationTaskContext(t.Context(), nil, child, store)
	if err != nil || result != nil || store.runReads != 1 || store.taskReads != 0 || store.taskLists != 0 {
		t.Fatalf("ordinary chat child added task lookups: result=%#v err=%v runs=%d tasks=%d lists=%d", result, err, store.runReads, store.taskReads, store.taskLists)
	}
}

func catalogTaskPageFixture(t *testing.T, count int) (*sqlite.Store, *models.Conversation, []*models.ConversationTaskResult) {
	t.Helper()
	store := newTestKernelStore(t)
	base := time.Now().UTC().Add(-time.Hour)
	scope := models.Scope{Kind: "tenant", ID: "task-sql"}
	conversation := &models.Conversation{ID: "conversation", Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "task-agent"},
		Title: "Shared tasks", Status: models.ConversationStatusActive, Revision: 1, CreatedAt: base, UpdatedAt: base}
	seedTestSpace(t, store, scope)
	if _, _, err := store.CreateConversation(t.Context(), conversation, "catalog-task-page"); err != nil {
		t.Fatal(err)
	}
	service := runtimeconversation.NewConversationService(store)
	if _, err := service.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{
		ID: "thread", Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "requester"}, Intent: models.MessageIntentQuestion,
		Content: "Work on these independent requests.", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, StartThread: true, IdempotencyKey: "thread",
	}); err != nil {
		t.Fatal(err)
	}
	results := make([]*models.ConversationTaskResult, 0, count+1)
	for i := 0; i <= count; i++ {
		record := taskContextTaskRecord(t, store, fmt.Sprintf("catalog-%02d", i), base.Add(time.Duration(i+1)*time.Second))
		actor := record.Task.AuthenticatedActor
		if i == count {
			actor.ID = "another-member"
		}
		current, err := store.GetConversation(t.Context(), scope, conversation.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.PostChannelMessage(t.Context(), runtimeconversation.PostChannelMessageRequest{
			ID: record.Task.SourceMessageID, Scope: scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
			Sender: actor, Intent: models.MessageIntentQuestion, Content: record.Task.Goal,
			Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, ReplyToMessageID: "thread", IdempotencyKey: record.Task.SourceMessageID,
		}); err != nil {
			t.Fatal(err)
		}
		turn := &models.AgentTurn{ID: record.Task.SourceTurnID, Scope: scope, RunID: record.Task.SourceRunID, Sequence: 1,
			Status: models.AgentTurnStatusCompleted, Revision: 1, CreatedAt: record.Task.CreatedAt, UpdatedAt: record.Task.CreatedAt,
			StartedAt: record.Task.CreatedAt, CompletedAt: &record.Task.CreatedAt}
		if _, err := store.CreateAgentTurn(t.Context(), turn); err != nil {
			t.Fatal(err)
		}
		tasks := conversationrun.NewConversationTaskService(store, models.WithClock(func() time.Time { return record.Task.CreatedAt }))
		source, err := store.GetAgentRun(t.Context(), scope, record.Task.SourceRunID)
		if err != nil {
			t.Fatal(err)
		}
		req := conversationruntest.ProposedTaskStartRequest(t, store, source, turn, conversationruntest.TaskSourceProposal{Goal: record.Task.Goal, Acknowledgment: record.Task.Acknowledgment, Budget: record.Task.RequestedBudget})
		req.WorkerID = "task-worker"
		accepted, err := tasks.Start(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, accepted)
	}
	return store, conversation, results
}

func TestCatalogConversationTaskContextPrioritizesIndependentRootWithoutWideningPage(t *testing.T) {
	for _, count := range []int{3, 13} {
		t.Run(fmt.Sprintf("active=%d", count), func(t *testing.T) {
			store, conversation, admitted := catalogTaskPageFixture(t, count)
			self := admitted[0]
			context, err := conversationrun.ResolveCatalogConversationTaskContext(t.Context(), nil, self.WorkRun, store)
			wantCount := min(count, 10)
			if err != nil || context == nil || context.CanStart || len(context.Tasks) != wantCount || context.Tasks[0].TaskID != self.Task.ID || context.Tasks[0].WorkRunID != self.WorkRun.ID {
				t.Fatalf("executing task missing from bounded first slot: %#v, %v", context, err)
			}
			seen := make(map[string]bool)
			for _, snapshot := range context.Tasks {
				if seen[snapshot.TaskID] || snapshot.TaskID == admitted[count].Task.ID {
					t.Fatalf("duplicate or another member's task exposed: %#v", context.Tasks)
				}
				seen[snapshot.TaskID] = true
			}
			ordinary, err := store.ListConversationTasks(t.Context(), models.ConversationTaskFilter{
				Scope: self.Task.Scope, Owner: self.Task.Owner, ConversationID: conversation.ID, ThreadRootID: "thread",
				AuthenticatedActor: self.Task.AuthenticatedActor, ActiveOnly: true, Limit: 10,
			})
			if err != nil {
				t.Fatal(err)
			}
			if count > 10 && ordinary[0].Task.ID == self.Task.ID {
				t.Fatal("fixture did not place the executing task outside the ordinary recent page")
			}
			foreground, err := conversationrun.ResolveCatalogConversationTaskContext(t.Context(), nil, self.SourceRun, store)
			if err != nil || foreground == nil || len(foreground.Tasks) != len(ordinary) {
				t.Fatalf("foreground bounded projection changed: %#v, %v", foreground, err)
			}
			for i, row := range ordinary {
				if foreground.Tasks[i].TaskID != row.Task.ID {
					t.Fatalf("foreground task order changed at %d: %#v", i, foreground.Tasks)
				}
			}
			foreign := models.CloneAgentRun(self.WorkRun)
			foreign.Scope.ID = "foreign-tenant"
			if context, err := conversationrun.ResolveCatalogConversationTaskContext(t.Context(), nil, foreign, store); err != nil || context != nil {
				t.Fatalf("foreign tenant acquired executing-task identity: %#v, %v", context, err)
			}
		})
	}
}

func TestCatalogConversationTaskContextDoesNotPrioritizeForkRoot(t *testing.T) {
	store, _, admitted := catalogTaskPageFixture(t, 13)
	root := admitted[0].WorkRun
	child, err := portfolio.NewPortfolioService(store).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
		Scope: root.Scope, Owner: root.Owner, Kind: models.RunKindAgentWork, ParentRunID: root.ID, AssignedAgentID: root.AssignedAgentID,
		Goal: "Complete this fork's partial goal", Source: models.RunSourceFork, Context: maps.Clone(root.Context),
	})
	if err != nil {
		t.Fatal(err)
	}
	context, err := conversationrun.ResolveCatalogConversationTaskContext(t.Context(), nil, child, store)
	if err != nil || context == nil || len(context.Tasks) != 10 {
		t.Fatalf("fork task context = %#v, %v", context, err)
	}
	for _, snapshot := range context.Tasks {
		if snapshot.WorkRunID == root.ID || snapshot.WorkRunID == child.ID {
			t.Fatalf("fork gained forced whole-task identity: %#v", context.Tasks)
		}
	}
}

// taskContextTaskRecord is a copy of the conversation run tests' conversationTaskBenchmarkRecord; change them
// together.
func taskContextTaskRecord(t testing.TB, store conversationTaskSQLTestStore, suffix string, at time.Time) models.ConversationTaskCreateRecord {
	t.Helper()
	now := at
	expires := time.Now().UTC().Add(10 * time.Minute)
	scope := models.Scope{Kind: "tenant", ID: "task-sql"}
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "task-agent"}
	sourceID := "source-" + suffix
	source := &models.AgentRun{ID: sourceID, Scope: scope, Kind: models.RunKindConversation, RootRunID: sourceID, Owner: owner,
		AssignedAgentID: owner.ID, ConcurrencyKey: models.ConversationRunLane("conversation", "thread", owner.ID), Goal: "Accept conversation work", Source: models.RunSourceChat, Status: models.AgentRunStatusRunning,
		Priority: 90, AvailableAt: now, QueueEnteredAt: now, LeaseOwner: "task-worker", LeaseExpiresAt: &expires,
		LastAppliedTurn: 1, Budget: &models.BudgetPolicy{MaxTurns: 20}, Policy: map[string]interface{}{"preserve": "source-policy"},
		Context:  map[string]interface{}{models.ConversationRunConversationIDContextKey: "conversation", models.ConversationRunTriggerIDContextKey: "message-" + suffix, "threadRootMessageId": "thread"},
		Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateAgentRun(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	taskID := models.ConversationTaskID(scope, sourceID, "task-"+suffix)
	workID := "work-" + suffix
	// The task asks for the child minimum of 8 Turns.
	task := &models.ConversationTask{ID: taskID, Scope: scope, Owner: owner, ConversationID: "conversation", SourceMessageID: "message-" + suffix,
		ThreadRootID: "thread", SourceRunID: sourceID, SourceTurnID: "turn-" + suffix, SourceTurnNumber: 1,
		AuthenticatedActor: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: "requester"}, TargetAgentID: owner.ID,
		WorkRunID: workID, TaskKey: "task-" + suffix, Goal: "Prepare the requested result", Acknowledgment: "I have started the work.",
		RequestedBudget: &models.BudgetPolicy{MaxTurns: 8}, Revision: 1, CreatedAt: now}
	var err error
	task.RequestDigest, err = models.ConversationTaskDigest(task)
	if err != nil {
		t.Fatal(err)
	}
	work := &models.AgentRun{ID: workID, Scope: scope, Kind: models.RunKindAgentWork, RootRunID: workID, Owner: owner, AssignedAgentID: owner.ID,
		ConcurrencyKey: "task:" + taskID, Goal: task.Goal, Source: models.RunSourceChat, Status: models.AgentRunStatusQueued, Priority: source.Priority,
		AvailableAt: now, QueueEnteredAt: now, Budget: models.CloneBudgetPolicy(task.RequestedBudget), Revision: 1, CreatedAt: now, UpdatedAt: now,
		Context: map[string]interface{}{models.ConversationTaskContextKey: taskID, models.ConversationRunConversationIDContextKey: task.ConversationID,
			models.ConversationRunTriggerIDContextKey: task.SourceMessageID, "threadRootMessageId": task.ThreadRootID, models.RunReportingRootRunIDContextKey: workID,
			models.RunReportingMilestonesContextKey: []interface{}{"completed", "failed"}}}
	updated := models.CloneAgentRun(source)
	if err := models.AddRunBudgetAllocation(updated, workID, work.Budget); err != nil {
		t.Fatal(err)
	}
	updated.Status = models.AgentRunStatusCompleted
	updated.Context[models.RunReportingRootRunIDContextKey] = source.ID
	updated.Context[models.RunReportingMilestonesContextKey] = []interface{}{"completed", "failed"}
	updated.Output = map[string]interface{}{"summary": task.Acknowledgment, "conversationTaskIds": []string{taskID}, "conversationTaskWorkRunId": workID}
	updated.CompletedAt = &now
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	updated.Revision++
	event := &models.ActivityEvent{ID: "task-event-" + suffix, Scope: scope, RunID: sourceID, EventType: "conversation.task_started",
		Summary: "Started independent work", Actor: models.ActivityActor{Type: "user", ID: "requester"}, CorrelationID: taskID,
		Visibility: models.ActivityVisibilityScope, CreatedAt: now}
	record := models.ConversationTaskCreateRecord{Task: task, WorkRun: work, SourceRun: updated, ExpectedSourceRevision: 1, WorkerID: "task-worker", Event: event}
	if err := models.ValidateConversationTaskCreateRecord(record); err != nil {
		t.Fatalf("invalid SQL fixture: %v", err)
	}
	return record
}
