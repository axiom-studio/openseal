package runtime

import "context"

func (s *PostgresStore) GetChannelMessagesByIDs(ctx context.Context, scope Scope, conversationID string, ids []string) ([]*ChannelMessage, error) {
	return getChannelMessageBatchSQL(ctx, s.db, s.table("channel_messages"), scope, conversationID, ids, true)
}
