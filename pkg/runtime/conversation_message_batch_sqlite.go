package runtime

import "context"

func (s *SQLiteStore) GetChannelMessagesByIDs(ctx context.Context, scope Scope, conversationID string, ids []string) ([]*ChannelMessage, error) {
	return getChannelMessageBatchSQL(ctx, s.db, "channel_messages", scope, conversationID, ids, false)
}
