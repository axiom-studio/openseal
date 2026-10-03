package runtime

import (
	"context"
	"time"
)

type memoryConversationTaskIndexKey struct {
	Scope          Scope
	Owner          ObjectiveOwner
	ConversationID string
	ThreadRootID   string
	Actor          ConversationParticipant
}

type memoryConversationTaskPageKey struct {
	CreatedAt time.Time
	ID        string
}

func memoryConversationTaskPageLess(a, b memoryConversationTaskPageKey) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.ID > b.ID
}

func memoryConversationTaskIndexKeys(task *ConversationTask) []memoryConversationTaskIndexKey {
	keys := make([]memoryConversationTaskIndexKey, 0, 4)
	for _, thread := range []string{"", task.ThreadRootID} {
		for _, actor := range []ConversationParticipant{{}, task.AuthenticatedActor} {
			keys = append(keys, memoryConversationTaskIndexKey{Scope: task.Scope, Owner: task.Owner, ConversationID: task.ConversationID, ThreadRootID: thread, Actor: actor})
		}
	}
	return keys
}

func (s *MemoryStore) saveMemoryConversationTaskLocked(task *ConversationTask, run *AgentRun) {
	if s.conversationTasks == nil {
		s.conversationTasks = make(map[string]*ConversationTask)
		s.conversationTaskWorkRuns = make(map[string]string)
		s.conversationTaskOrder = make(map[memoryConversationTaskIndexKey]*memoryOrderedIndexNode[memoryConversationTaskPageKey])
		s.conversationTaskActiveOrder = make(map[memoryConversationTaskIndexKey]*memoryOrderedIndexNode[memoryConversationTaskPageKey])
	}
	key := portfolioKey(task.Scope, task.ID)
	s.conversationTasks[key] = cloneConversationTask(task)
	s.conversationTaskWorkRuns[portfolioKey(task.Scope, task.WorkRunID)] = key
	page := memoryConversationTaskPageKey{CreatedAt: task.CreatedAt, ID: task.ID}
	for _, index := range memoryConversationTaskIndexKeys(task) {
		s.conversationTaskOrder[index] = memoryOrderedIndexInsert(s.conversationTaskOrder[index], page, memoryConversationTaskPageLess)
		if !isTerminalAgentRunStatus(run.Status) {
			s.conversationTaskActiveOrder[index] = memoryOrderedIndexInsert(s.conversationTaskActiveOrder[index], page, memoryConversationTaskPageLess)
		}
	}
}

func (s *MemoryStore) refreshMemoryConversationTaskRunLocked(run *AgentRun) {
	task := s.conversationTasks[s.conversationTaskWorkRuns[portfolioKey(run.Scope, run.ID)]]
	if task == nil {
		return
	}
	page := memoryConversationTaskPageKey{CreatedAt: task.CreatedAt, ID: task.ID}
	for _, index := range memoryConversationTaskIndexKeys(task) {
		if isTerminalAgentRunStatus(run.Status) {
			s.conversationTaskActiveOrder[index] = memoryOrderedIndexDelete(s.conversationTaskActiveOrder[index], page, memoryConversationTaskPageLess)
		} else {
			s.conversationTaskActiveOrder[index] = memoryOrderedIndexInsert(s.conversationTaskActiveOrder[index], page, memoryConversationTaskPageLess)
		}
	}
}

func (s *MemoryStore) CreateConversationTask(ctx context.Context, record ConversationTaskCreateRecord) (*ConversationTaskResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateConversationTaskCreateRecord(record); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := portfolioKey(record.Task.Scope, record.Task.ID)
	if task := s.conversationTasks[key]; task != nil {
		if !sameConversationTask(task, record.Task) {
			return nil, ErrConversationTaskConflict
		}
		return &ConversationTaskResult{Task: cloneConversationTask(task), WorkRun: cloneAgentRun(s.agentRuns[portfolioKey(task.Scope, task.WorkRunID)]),
			SourceRun: cloneAgentRun(s.agentRuns[portfolioKey(task.Scope, task.SourceRunID)]), Replayed: true}, nil
	}
	sourceKey := portfolioKey(record.Task.Scope, record.Task.SourceRunID)
	if err := validateConversationTaskSourceUpdate(s.agentRuns[sourceKey], record); err != nil {
		return nil, err
	}
	workKey := portfolioKey(record.WorkRun.Scope, record.WorkRun.ID)
	if s.agentRuns[workKey] != nil || s.conversationTaskWorkRuns[workKey] != "" {
		return nil, ErrConversationTaskConflict
	}
	if err := s.validateMemoryRunSkillDependenciesLocked(record.WorkRun); err != nil {
		return nil, err
	}
	if err := s.validateMemoryRunSkillDependenciesLocked(record.SourceRun); err != nil {
		return nil, err
	}
	s.saveMemoryConversationTaskLocked(record.Task, record.WorkRun)
	s.saveMemoryAgentRunLocked(workKey, record.WorkRun)
	s.saveMemoryAgentRunLocked(sourceKey, record.SourceRun)
	appendMemoryActivityLocked(s, record.Event)
	return &ConversationTaskResult{Task: cloneConversationTask(record.Task), WorkRun: cloneAgentRun(record.WorkRun), SourceRun: cloneAgentRun(record.SourceRun)}, nil
}

func (s *MemoryStore) GetConversationTask(ctx context.Context, scope Scope, id string) (*ConversationTask, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneConversationTask(s.conversationTasks[portfolioKey(scope, id)]), nil
}

func (s *MemoryStore) FindConversationTaskByWorkRunID(ctx context.Context, scope Scope, id string) (*ConversationTask, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneConversationTask(s.conversationTasks[s.conversationTaskWorkRuns[portfolioKey(scope, id)]]), nil
}

func (s *MemoryStore) ListConversationTasks(ctx context.Context, filter ConversationTaskFilter) ([]*ConversationTaskResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	index := memoryConversationTaskIndexKey{Scope: filter.Scope, Owner: filter.Owner, ConversationID: filter.ConversationID, ThreadRootID: filter.ThreadRootID, Actor: filter.AuthenticatedActor}
	order := s.conversationTaskOrder[index]
	if filter.ActiveOnly {
		order = s.conversationTaskActiveOrder[index]
	}
	var cursor *memoryConversationTaskPageKey
	if filter.BeforeCreatedAt != nil {
		cursor = &memoryConversationTaskPageKey{CreatedAt: *filter.BeforeCreatedAt, ID: filter.BeforeID}
	}
	pages := memoryOrderedIndexAfter(order, cursor, nil, filter.Limit, memoryConversationTaskPageLess)
	results := make([]*ConversationTaskResult, 0, len(pages))
	for _, page := range pages {
		task := s.conversationTasks[portfolioKey(filter.Scope, page.ID)]
		results = append(results, &ConversationTaskResult{Task: cloneConversationTask(task), WorkRun: cloneAgentRun(s.agentRuns[portfolioKey(filter.Scope, task.WorkRunID)])})
	}
	return results, nil
}
