package runtime

import (
	"context"
	"fmt"
	"testing"
	"time"
)

type countedCatalogTaskContextStore struct {
	*MemoryStore
	runReads, taskReads, taskLists int
}

func (s *countedCatalogTaskContextStore) GetAgentRun(ctx context.Context, scope Scope, id string) (*AgentRun, error) {
	s.runReads++
	return s.MemoryStore.GetAgentRun(ctx, scope, id)
}

func (s *countedCatalogTaskContextStore) FindConversationTaskByWorkRunID(ctx context.Context, scope Scope, id string) (*ConversationTask, error) {
	s.taskReads++
	return s.MemoryStore.FindConversationTaskByWorkRunID(ctx, scope, id)
}

func (s *countedCatalogTaskContextStore) ListConversationTasks(ctx context.Context, filter ConversationTaskFilter) ([]*ConversationTaskResult, error) {
	s.taskLists++
	return s.MemoryStore.ListConversationTasks(ctx, filter)
}

func TestCatalogConversationTaskContextSkipsOrdinaryRunReads(t *testing.T) {
	store := &countedCatalogTaskContextStore{MemoryStore: NewMemoryStore()}
	for name, input := range map[string]map[string]interface{}{
		"non-chat":           {},
		"partial context":    {conversationRunContextConversationID: "chat"},
		"non-string trigger": {conversationRunContextConversationID: "chat", conversationRunContextTriggerID: true},
		"empty trigger":      {conversationRunContextConversationID: "chat", conversationRunContextTriggerID: ""},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := resolveCatalogConversationTaskContext(t.Context(), nil, &AgentRun{ID: "ordinary", Kind: RunKindAgentWork, Context: input}, store)
			if err != nil || result != nil || store.runReads != 0 || store.taskReads != 0 || store.taskLists != 0 {
				t.Fatalf("ordinary work added task reads: result=%#v err=%v runs=%d tasks=%d lists=%d", result, err, store.runReads, store.taskReads, store.taskLists)
			}
		})
	}
}

func TestCatalogConversationTaskContextSkipsUnmarkedChatChildTaskLookups(t *testing.T) {
	memory := NewMemoryStore()
	source, _, _, _ := settledWorkerTaskFixture(t, memory)
	child, err := NewPortfolioService(memory).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: source.Scope, Owner: source.Owner, Kind: RunKindAgentWork, ParentRunID: source.ID, AssignedAgentID: source.AssignedAgentID,
		Goal: "Ordinary delegated operation", Source: RunSourceFork, Context: cloneMap(source.Context),
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &countedCatalogTaskContextStore{MemoryStore: memory}
	result, err := resolveCatalogConversationTaskContext(t.Context(), nil, child, store)
	if err != nil || result != nil || store.runReads != 1 || store.taskReads != 0 || store.taskLists != 0 {
		t.Fatalf("ordinary chat child added task lookups: result=%#v err=%v runs=%d tasks=%d lists=%d", result, err, store.runReads, store.taskReads, store.taskLists)
	}
}

func catalogTaskPageFixture(t *testing.T, count int) (*MemoryStore, *Conversation, []*ConversationTaskResult) {
	t.Helper()
	store := NewMemoryStore()
	base := time.Now().UTC().Add(-time.Hour)
	scope := Scope{Kind: "tenant", ID: "task-sql"}
	conversation := &Conversation{ID: "conversation", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "task-agent"},
		Title: "Shared tasks", Status: ConversationStatusActive, Revision: 1, CreatedAt: base, UpdatedAt: base}
	if _, _, err := store.CreateConversation(t.Context(), conversation, "catalog-task-page"); err != nil {
		t.Fatal(err)
	}
	service := NewConversationService(store)
	if _, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{
		ID: "thread", Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "requester"}, Intent: MessageIntentQuestion,
		Content: "Work on these independent requests.", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, StartThread: true, IdempotencyKey: "thread",
	}); err != nil {
		t.Fatal(err)
	}
	results := make([]*ConversationTaskResult, 0, count+1)
	for i := 0; i <= count; i++ {
		record := conversationTaskBenchmarkRecord(t, store, fmt.Sprintf("catalog-%02d", i), base.Add(time.Duration(i+1)*time.Second))
		actor := record.Task.AuthenticatedActor
		if i == count {
			actor.ID = "another-member"
		}
		current, err := store.GetConversation(t.Context(), scope, conversation.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{
			ID: record.Task.SourceMessageID, Scope: scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
			Sender: actor, Intent: MessageIntentQuestion, Content: record.Task.Goal,
			Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: "thread", IdempotencyKey: record.Task.SourceMessageID,
		}); err != nil {
			t.Fatal(err)
		}
		turn := &AgentTurn{ID: record.Task.SourceTurnID, Scope: scope, RunID: record.Task.SourceRunID, Sequence: 1,
			Status: AgentTurnStatusCompleted, Revision: 1, CreatedAt: record.Task.CreatedAt, UpdatedAt: record.Task.CreatedAt,
			StartedAt: record.Task.CreatedAt, CompletedAt: &record.Task.CreatedAt,
			RequestedTask: &TurnTaskProposal{TaskKey: record.Task.TaskKey, Goal: record.Task.Goal, Acknowledgment: record.Task.Acknowledgment, Budget: cloneBudgetPolicy(record.Task.RequestedBudget)}}
		if _, err := store.CreateAgentTurn(t.Context(), turn); err != nil {
			t.Fatal(err)
		}
		tasks := NewConversationTaskService(store)
		tasks.now = func() time.Time { return record.Task.CreatedAt }
		accepted, err := tasks.Start(t.Context(), StartConversationTaskRequest{
			Scope: scope, SourceRunID: record.Task.SourceRunID, ExpectedSourceRevision: 1, WorkerID: "task-worker", TurnID: turn.ID,
			AssignedAgentID: record.Task.TargetAgentID, TaskKey: record.Task.TaskKey, Goal: record.Task.Goal,
			Acknowledgment: record.Task.Acknowledgment, Budget: cloneBudgetPolicy(record.Task.RequestedBudget),
		})
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
			context, err := resolveCatalogConversationTaskContext(t.Context(), nil, self.WorkRun, store)
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
			ordinary, err := store.ListConversationTasks(t.Context(), ConversationTaskFilter{
				Scope: self.Task.Scope, Owner: self.Task.Owner, ConversationID: conversation.ID, ThreadRootID: "thread",
				AuthenticatedActor: self.Task.AuthenticatedActor, ActiveOnly: true, Limit: 10,
			})
			if err != nil {
				t.Fatal(err)
			}
			if count > 10 && ordinary[0].Task.ID == self.Task.ID {
				t.Fatal("fixture did not place the executing task outside the ordinary recent page")
			}
			foreground, err := resolveCatalogConversationTaskContext(t.Context(), nil, self.SourceRun, store)
			if err != nil || foreground == nil || len(foreground.Tasks) != len(ordinary) {
				t.Fatalf("foreground bounded projection changed: %#v, %v", foreground, err)
			}
			for i, row := range ordinary {
				if foreground.Tasks[i].TaskID != row.Task.ID {
					t.Fatalf("foreground task order changed at %d: %#v", i, foreground.Tasks)
				}
			}
			foreign := cloneAgentRun(self.WorkRun)
			foreign.Scope.ID = "foreign-tenant"
			if context, err := resolveCatalogConversationTaskContext(t.Context(), nil, foreign, store); err != nil || context != nil {
				t.Fatalf("foreign tenant acquired executing-task identity: %#v, %v", context, err)
			}
		})
	}
}

func TestCatalogConversationTaskContextDoesNotPrioritizeForkRoot(t *testing.T) {
	store, _, admitted := catalogTaskPageFixture(t, 13)
	root := admitted[0].WorkRun
	child, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: root.Scope, Owner: root.Owner, Kind: RunKindAgentWork, ParentRunID: root.ID, AssignedAgentID: root.AssignedAgentID,
		Goal: "Complete this fork's partial goal", Source: RunSourceFork, Context: cloneMap(root.Context),
	})
	if err != nil {
		t.Fatal(err)
	}
	context, err := resolveCatalogConversationTaskContext(t.Context(), nil, child, store)
	if err != nil || context == nil || len(context.Tasks) != 10 {
		t.Fatalf("fork task context = %#v, %v", context, err)
	}
	for _, snapshot := range context.Tasks {
		if snapshot.WorkRunID == root.ID || snapshot.WorkRunID == child.ID {
			t.Fatalf("fork gained forced whole-task identity: %#v", context.Tasks)
		}
	}
}
