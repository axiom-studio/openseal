package runtime

import "context"

func memoryConversationTaskDueLess(a, b memoryConversationTaskPageKey) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}

func (s *MemoryStore) refreshMemoryConversationTaskDueRunLocked(run *AgentRun) {
	key := portfolioKey(run.Scope, run.ID)
	if previous, ok := s.conversationTaskDueMembership[key]; ok {
		s.conversationTaskDueOrder[run.Scope] = memoryOrderedIndexDelete(s.conversationTaskDueOrder[run.Scope], previous, memoryConversationTaskDueLess)
		delete(s.conversationTaskDueMembership, key)
	}
	if !conversationTaskContinuationCandidate(run) {
		return
	}
	if s.conversationTaskDueOrder == nil {
		s.conversationTaskDueOrder = make(map[Scope]*memoryOrderedIndexNode[memoryConversationTaskPageKey])
		s.conversationTaskDueMembership = make(map[string]memoryConversationTaskPageKey)
	}
	page := memoryConversationTaskPageKey{CreatedAt: run.CreatedAt, ID: run.ID}
	s.conversationTaskDueOrder[run.Scope] = memoryOrderedIndexInsert(s.conversationTaskDueOrder[run.Scope], page, memoryConversationTaskDueLess)
	s.conversationTaskDueMembership[key] = page
}

func (s *MemoryStore) ListDueConversationTaskRuns(ctx context.Context, filter ConversationTaskDueFilter) ([]*AgentRun, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var cursor *memoryConversationTaskPageKey
	if filter.AfterCreatedAt != nil {
		cursor = &memoryConversationTaskPageKey{CreatedAt: *filter.AfterCreatedAt, ID: filter.AfterID}
	}
	pages := memoryOrderedIndexAfter(s.conversationTaskDueOrder[filter.Scope], cursor,
		func(page memoryConversationTaskPageKey) bool { return !page.CreatedAt.After(filter.BeforeCreatedAt) }, filter.Limit, memoryConversationTaskDueLess)
	runs := make([]*AgentRun, 0, len(pages))
	for _, page := range pages {
		runs = append(runs, cloneAgentRun(s.agentRuns[portfolioKey(filter.Scope, page.ID)]))
	}
	return runs, nil
}

func (s *MemoryStore) PromoteConversationTask(ctx context.Context, req ConversationTaskPromotionRequest) (*ConversationTaskResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := portfolioKey(req.Scope, req.RunID)
	run := s.agentRuns[key]
	if run == nil {
		return nil, ErrRunNotFound
	}
	if existing := s.conversationTasks[s.conversationTaskWorkRuns[key]]; existing != nil {
		return replayConversationTaskContinuation(existing, run)
	}
	if !conversationTaskContinuationCandidate(run) {
		return nil, nil
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	messageID, _ := run.Context[conversationRunContextTriggerID].(string)
	conversation := s.conversations[conversationStoreKey(req.Scope, conversationID)]
	message := s.channelMessageIDs[channelMessageStoreKey(req.Scope, conversationID, messageID)]
	for _, responseKey := range conversationTaskFinalResponseKeys(run) {
		if s.channelMessageKeys[channelMessageIdempotencyKey(req.Scope, conversationID, responseKey)] != "" {
			return nil, nil
		}
	}
	if conversationTaskFinishedBeforePromotion(latestConversationTaskStep(s.turns[key])) {
		return nil, nil
	}
	var chat *AgentSession
	if run.SessionID != "" {
		chat = cloneAgentSession(s.agentSessions[portfolioKey(req.Scope, run.SessionID)])
	}
	task, updated, event, fork, err := buildConversationTaskContinuation(req, run, conversation, message, chat)
	if err != nil || task == nil {
		return nil, err
	}
	if task.ThreadRootID != task.SourceMessageID {
		root := s.channelMessageIDs[channelMessageStoreKey(req.Scope, conversationID, task.ThreadRootID)]
		if root == nil || root.ConversationID != conversationID {
			return nil, ErrInvalidConversationTask
		}
	}
	if s.conversationTasks[portfolioKey(task.Scope, task.ID)] != nil {
		return nil, ErrConversationTaskConflict
	}
	ack, err := buildConversationTaskAcknowledgment(task, conversation, message)
	if err != nil {
		return nil, err
	}
	ackKey := channelMessageStoreKey(task.Scope, task.ConversationID, ack.Message.ID)
	ackIdempotencyKey := channelMessageIdempotencyKey(task.Scope, task.ConversationID, ack.Message.IdempotencyKey)
	if s.channelMessageIDs[ackKey] != nil || s.channelMessageKeys[ackIdempotencyKey] != "" {
		return nil, ErrMessageConflict
	}
	// The session writes are the only fallible ones, so they go first. The
	// fork stores run.SessionID = child (Revision+1, as updated already has);
	// updated then replaces that row.
	if fork != nil {
		if s.agentSessions[portfolioKey(req.Scope, updated.SessionID)] != nil {
			return nil, ErrAgentSessionConflict
		}
		if _, err := s.createRunAgentSessionLocked(fork.Child); err != nil {
			return nil, err
		}
		if fork.Note != nil {
			if _, err := s.appendAgentSessionEntriesLocked(*fork.Note); err != nil {
				return nil, err
			}
		}
	}
	s.saveMemoryConversationTaskLocked(task, updated)
	s.saveMemoryAgentRunLocked(key, updated)
	appendMemoryActivityLocked(s, event)
	conversationKey := conversationStoreKey(task.Scope, task.ConversationID)
	s.conversations[conversationKey] = cloneConversation(ack.Conversation)
	s.channelMessages[conversationKey] = append(s.channelMessages[conversationKey], cloneChannelMessage(ack.Message))
	s.channelMessageIDs[ackKey] = cloneChannelMessage(ack.Message)
	s.channelMessageKeys[ackIdempotencyKey] = ack.Message.ID
	return &ConversationTaskResult{Task: cloneConversationTask(task), WorkRun: cloneAgentRun(updated), SourceRun: cloneAgentRun(updated), AcknowledgmentMessageID: ack.Message.ID}, nil
}
