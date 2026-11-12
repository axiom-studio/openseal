package sqlite

import (
	"context"
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

const sqliteConversationReactionSchema = `
		CREATE TABLE IF NOT EXISTS conversation_message_reactions (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, conversation_id TEXT NOT NULL, message_id TEXT NOT NULL,
			actor_type TEXT NOT NULL, actor_id TEXT NOT NULL, emoji TEXT NOT NULL, created_at DATETIME NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, conversation_id, message_id, actor_type, actor_id, emoji)
		);
		CREATE TABLE IF NOT EXISTS conversation_reaction_events (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, conversation_id TEXT NOT NULL, sequence INTEGER NOT NULL,
			type TEXT NOT NULL, message_id TEXT NOT NULL, actor_type TEXT NOT NULL, actor_id TEXT NOT NULL,
			emoji TEXT NOT NULL, created_at DATETIME NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, conversation_id, sequence)
		);
`

func migrateSQLiteConversationReactions(db *sql.DB) error {
	_, err := db.Exec(sqliteConversationReactionSchema)
	return err
}

func (s *Store) AddMessageReaction(ctx context.Context, record models.MessageReactionRecord) (*models.ConversationReactionEvent, error) {
	return s.changeSQLiteReaction(ctx, record, true)
}

func (s *Store) RemoveMessageReaction(ctx context.Context, record models.MessageReactionRecord) (*models.ConversationReactionEvent, error) {
	return s.changeSQLiteReaction(ctx, record, false)
}

func (s *Store) changeSQLiteReaction(ctx context.Context, record models.MessageReactionRecord, add bool) (*models.ConversationReactionEvent, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	conversation, err := getSQLiteConversation(ctx, conn, record.Scope, record.ConversationID)
	if err != nil {
		return nil, err
	}
	if conversation == nil {
		return nil, models.ErrConversationNotFound
	}
	key := []interface{}{record.Scope.Kind, record.Scope.ID, record.ConversationID, record.MessageID, record.Actor.Type, record.Actor.ID, record.Emoji}
	eventType := models.ConversationReactionAdded
	var result sql.Result
	if add {
		result, err = conn.ExecContext(ctx, `INSERT INTO conversation_message_reactions
			(scope_kind, scope_id, conversation_id, message_id, actor_type, actor_id, emoji, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, append(key, record.CreatedAt)...)
	} else {
		eventType = models.ConversationReactionRemoved
		result, err = conn.ExecContext(ctx, `DELETE FROM conversation_message_reactions
			WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND message_id = ? AND actor_type = ? AND actor_id = ? AND emoji = ?`, key...)
	}
	if err != nil {
		return nil, err
	}
	if changed, err := result.RowsAffected(); err != nil || changed == 0 {
		if err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, err
		}
		committed = true
		return nil, nil
	}
	var sequence int64
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM conversation_reaction_events
		WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ?`, record.Scope.Kind, record.Scope.ID, record.ConversationID).Scan(&sequence); err != nil {
		return nil, err
	}
	event := models.NewConversationReactionEvent(eventType, record, sequence)
	if _, err := conn.ExecContext(ctx, `INSERT INTO conversation_reaction_events
		(scope_kind, scope_id, conversation_id, sequence, type, message_id, actor_type, actor_id, emoji, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, record.Scope.Kind, record.Scope.ID, record.ConversationID, sequence, eventType,
		record.MessageID, record.Actor.Type, record.Actor.ID, record.Emoji, record.CreatedAt); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return event, nil
}

func (s *Store) ListMessageReactions(ctx context.Context, scope models.Scope, conversationID string, messageIDs []string) ([]*models.MessageReactionRecord, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if len(messageIDs) == 0 {
		return []*models.MessageReactionRecord{}, nil
	}
	args := []interface{}{scope.Kind, scope.ID, conversationID}
	for _, id := range messageIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT message_id, actor_type, actor_id, emoji, created_at FROM conversation_message_reactions
		WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND message_id IN (`+sqlstore.ReactionMessagePlaceholders(4, len(messageIDs), false)+`)
		ORDER BY created_at ASC, actor_type ASC, actor_id ASC, emoji ASC`, args...)
	if err != nil {
		return nil, err
	}
	return sqlstore.ScanMessageReactionRecords(rows, scope, conversationID)
}

func (s *Store) ListConversationReactionEvents(ctx context.Context, scope models.Scope, conversationID string, afterSequence int64, limit int) ([]*models.ConversationReactionEvent, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sequence, type, message_id, actor_type, actor_id, emoji, created_at FROM conversation_reaction_events
		WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND sequence > ? ORDER BY sequence ASC LIMIT ?`,
		scope.Kind, scope.ID, conversationID, afterSequence, sqlstore.ReactionEventLimit(limit))
	if err != nil {
		return nil, err
	}
	return sqlstore.ScanConversationReactionEvents(rows, scope, conversationID)
}
