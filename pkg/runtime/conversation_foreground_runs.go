package runtime

import "context"

// ConversationForegroundRunsReadStore retains legacy unthreaded roots and
// canonically scheduled explicit threads without scanning unrelated runs.
type ConversationForegroundRunsReadStore interface {
	ListConversationForegroundRuns(context.Context, Scope, ObjectiveOwner, string, int, int) ([]*AgentRun, error)
}

func validateConversationForegroundRunsQuery(scope Scope, owner ObjectiveOwner, conversationID string, limit, offset int) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if err := owner.Validate(); err != nil {
		return err
	}
	if !validOpaqueIdentifier(conversationID, 128) || limit < 1 || limit > 500 || offset < 0 {
		return ErrInvalidConversationTask
	}
	return nil
}

func foregroundConversationRunID(run *AgentRun) string {
	if run == nil || run.Kind != RunKindConversation {
		return ""
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	threadID, _ := run.Context["threadRootMessageId"].(string)
	if conversationID != "" && threadID != "" && run.ConcurrencyKey == conversationID+":thread:"+threadID {
		return conversationID
	}
	if validOpaqueIdentifier(run.ConcurrencyKey, 128) {
		return run.ConcurrencyKey
	}
	return ""
}

func (s *MemoryStore) refreshMemoryConversationForegroundRunLocked(run *AgentRun) {
	runKey := portfolioKey(run.Scope, run.ID)
	if previous, ok := s.conversationForegroundRunMembership[runKey]; ok {
		s.conversationForegroundRunOrder[previous.index] = memoryOrderedIndexDelete(s.conversationForegroundRunOrder[previous.index], previous.page, memoryConversationTaskPageLess)
		delete(s.conversationForegroundRunMembership, runKey)
	}
	conversationID := foregroundConversationRunID(run)
	if conversationID == "" {
		return
	}
	if s.conversationForegroundRunOrder == nil {
		s.conversationForegroundRunOrder = make(map[memoryConversationActiveRunIndexKey]*memoryOrderedIndexNode[memoryConversationTaskPageKey])
		s.conversationForegroundRunMembership = make(map[string]memoryConversationActiveRunMembership)
	}
	index := memoryConversationActiveRunIndexKey{Scope: run.Scope, Owner: run.Owner, ConversationID: conversationID}
	page := memoryConversationTaskPageKey{CreatedAt: run.CreatedAt, ID: run.ID}
	s.conversationForegroundRunOrder[index] = memoryOrderedIndexInsert(s.conversationForegroundRunOrder[index], page, memoryConversationTaskPageLess)
	s.conversationForegroundRunMembership[runKey] = memoryConversationActiveRunMembership{index: index, page: page}
}

func (s *MemoryStore) ListConversationForegroundRuns(ctx context.Context, scope Scope, owner ObjectiveOwner, conversationID string, limit, offset int) ([]*AgentRun, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateConversationForegroundRunsQuery(scope, owner, conversationID, limit, offset); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	index := memoryConversationActiveRunIndexKey{Scope: scope, Owner: owner, ConversationID: conversationID}
	pages := memoryOrderedIndexAfter(s.conversationForegroundRunOrder[index], nil, nil, limit+offset, memoryConversationTaskPageLess)
	if offset >= len(pages) {
		return []*AgentRun{}, nil
	}
	pages = pages[offset:]
	result := make([]*AgentRun, 0, len(pages))
	for _, page := range pages {
		result = append(result, cloneAgentRun(s.agentRuns[portfolioKey(scope, page.ID)]))
	}
	return result, nil
}
