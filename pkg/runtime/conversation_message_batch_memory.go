package runtime

import "context"

func (s *MemoryStore) GetChannelMessagesByIDs(_ context.Context, scope Scope, conversationID string, ids []string) ([]*ChannelMessage, error) {
	if err := validateChannelMessageBatch(scope, ids); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*ChannelMessage, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if message := s.channelMessageIDs[channelMessageStoreKey(scope, conversationID, id)]; message != nil {
			result = append(result, cloneChannelMessage(message))
		}
	}
	return result, nil
}
