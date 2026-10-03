package runtime

import "context"

// ConversationActiveRunsReadStore is an optional indexed read boundary for
// active child work whose canonical root belongs to one exact conversation.
// Embedding stores without this optimization retain the existing fallback.
type ConversationActiveRunsReadStore interface {
	ListConversationActiveRuns(context.Context, Scope, ObjectiveOwner, string, int) ([]*AgentRun, error)
}

func validateConversationActiveRunsQuery(scope Scope, owner ObjectiveOwner, conversationID string, limit int) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if err := owner.Validate(); err != nil {
		return err
	}
	if !validOpaqueIdentifier(conversationID, 128) || limit < 1 || limit > 100 {
		return ErrInvalidConversationTask
	}
	return nil
}

func (s *MemoryStore) refreshMemoryConversationActiveRunLocked(run *AgentRun) {
	runKey := portfolioKey(run.Scope, run.ID)
	if rootKey := s.conversationActiveRunRootKeys[runKey]; rootKey != "" {
		delete(s.conversationActiveRunRoots[rootKey], runKey)
		if len(s.conversationActiveRunRoots[rootKey]) == 0 {
			delete(s.conversationActiveRunRoots, rootKey)
		}
		delete(s.conversationActiveRunRootKeys, runKey)
	}
	if previous, ok := s.conversationActiveRunMembership[runKey]; ok {
		s.conversationActiveRunOrder[previous.index] = memoryOrderedIndexDelete(s.conversationActiveRunOrder[previous.index], previous.page, memoryConversationTaskPageLess)
		delete(s.conversationActiveRunMembership, runKey)
	}
	if run.Kind == RunKindConversation || isTerminalAgentRunStatus(run.Status) || run.RootRunID == run.ID {
		return
	}
	rootKey := portfolioKey(run.Scope, run.RootRunID)
	if s.conversationActiveRunRoots == nil {
		s.conversationActiveRunRoots = make(map[string]map[string]struct{})
		s.conversationActiveRunRootKeys = make(map[string]string)
	}
	if s.conversationActiveRunRoots[rootKey] == nil {
		s.conversationActiveRunRoots[rootKey] = make(map[string]struct{})
	}
	s.conversationActiveRunRoots[rootKey][runKey] = struct{}{}
	s.conversationActiveRunRootKeys[runKey] = rootKey
	root := s.agentRuns[portfolioKey(run.Scope, run.RootRunID)]
	if root == nil || root.Kind != RunKindConversation || root.Owner != run.Owner {
		return
	}
	conversationID, _ := root.Context[conversationRunContextConversationID].(string)
	if conversationID == "" {
		return
	}
	if s.conversationActiveRunOrder == nil {
		s.conversationActiveRunOrder = make(map[memoryConversationActiveRunIndexKey]*memoryOrderedIndexNode[memoryConversationTaskPageKey])
		s.conversationActiveRunMembership = make(map[string]memoryConversationActiveRunMembership)
	}
	index := memoryConversationActiveRunIndexKey{Scope: run.Scope, Owner: run.Owner, ConversationID: conversationID}
	page := memoryConversationTaskPageKey{CreatedAt: run.CreatedAt, ID: run.ID}
	s.conversationActiveRunOrder[index] = memoryOrderedIndexInsert(s.conversationActiveRunOrder[index], page, memoryConversationTaskPageLess)
	s.conversationActiveRunMembership[runKey] = memoryConversationActiveRunMembership{index: index, page: page}
}

// Canonical provenance edits refresh only that root's current active children;
// ordinary heartbeat/checkpoint/status writes leave their indexes untouched.
func (s *MemoryStore) refreshMemoryConversationActiveRootLocked(key string, previous, run *AgentRun) {
	if previous != nil {
		before, beforeString := previous.Context[conversationRunContextConversationID].(string)
		after, afterString := run.Context[conversationRunContextConversationID].(string)
		if previous.Kind == run.Kind && previous.Owner == run.Owner && beforeString == afterString && before == after {
			return
		}
	}
	children := make([]string, 0, len(s.conversationActiveRunRoots[key]))
	for childKey := range s.conversationActiveRunRoots[key] {
		children = append(children, childKey)
	}
	for _, childKey := range children {
		if child := s.agentRuns[childKey]; child != nil {
			s.refreshMemoryConversationActiveRunLocked(child)
		}
	}
}

type memoryConversationActiveRunIndexKey struct {
	Scope          Scope
	Owner          ObjectiveOwner
	ConversationID string
}

type memoryConversationActiveRunMembership struct {
	index memoryConversationActiveRunIndexKey
	page  memoryConversationTaskPageKey
}

func (s *MemoryStore) ListConversationActiveRuns(ctx context.Context, scope Scope, owner ObjectiveOwner, conversationID string, limit int) ([]*AgentRun, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateConversationActiveRunsQuery(scope, owner, conversationID, limit); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	index := memoryConversationActiveRunIndexKey{Scope: scope, Owner: owner, ConversationID: conversationID}
	pages := memoryOrderedIndexAfter(s.conversationActiveRunOrder[index], nil, nil, limit, memoryConversationTaskPageLess)
	runs := make([]*AgentRun, 0, len(pages))
	for _, page := range pages {
		run := s.agentRuns[portfolioKey(scope, page.ID)]
		root := s.agentRuns[portfolioKey(scope, run.RootRunID)]
		if root != nil && root.Kind == RunKindConversation && root.Owner == owner && root.Context[conversationRunContextConversationID] == conversationID {
			runs = append(runs, cloneAgentRun(run))
		}
	}
	return runs, nil
}
