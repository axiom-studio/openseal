package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

func (s *SQLiteStore) GetChannelMessagesByIDs(ctx context.Context, scope models.Scope, conversationID string, ids []string) ([]*models.ChannelMessage, error) {
	return sqlstore.GetChannelMessageBatch(ctx, s.db, "channel_messages", scope, conversationID, ids, false)
}

var _ models.ChannelMessageBatchStore = (*SQLiteStore)(nil)
