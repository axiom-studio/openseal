package postgres

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

func (s *PostgresStore) GetChannelMessagesByIDs(ctx context.Context, scope models.Scope, conversationID string, ids []string) ([]*models.ChannelMessage, error) {
	return sqlstore.GetChannelMessageBatch(ctx, s.db, s.table("channel_messages"), scope, conversationID, ids, true)
}

var _ models.ChannelMessageBatchStore = (*PostgresStore)(nil)
