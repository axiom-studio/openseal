package runtime

import "context"

var _ AgentBuilderCutoverStore = (*MemoryStore)(nil)

// The memory store never held change sets, so it has no cutover drafts.

func (s *MemoryStore) ListAgentBuilderCutoverDrafts(context.Context) ([]AgentBuilderCutoverDraft, error) {
	return nil, nil
}

func (s *MemoryStore) DeleteAgentBuilderCutoverDraft(context.Context, Scope, string) error {
	return nil
}

func (s *MemoryStore) DropAgentBuilderCutoverTable(context.Context) (bool, error) { return false, nil }
