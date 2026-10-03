package runtime

import (
	"context"
	"testing"
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
