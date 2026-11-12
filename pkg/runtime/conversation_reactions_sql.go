package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const conversationReactionMigrationVersion int64 = 68

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

func reactionMessagePlaceholders(start, count int, positional bool) string {
	parts := make([]string, count)
	for index := range parts {
		if positional {
			parts[index] = fmt.Sprintf("$%d", start+index)
		} else {
			parts[index] = "?"
		}
	}
	return strings.Join(parts, ",")
}

func scanMessageReactionRecords(rows *sql.Rows, scope Scope, conversationID string) ([]*MessageReactionRecord, error) {
	defer rows.Close()
	result := make([]*MessageReactionRecord, 0)
	for rows.Next() {
		record := &MessageReactionRecord{Scope: scope, ConversationID: conversationID}
		if err := rows.Scan(&record.MessageID, &record.Actor.Type, &record.Actor.ID, &record.Emoji, &record.CreatedAt); err != nil {
			return nil, err
		}
		record.CreatedAt = record.CreatedAt.UTC()
		result = append(result, record)
	}
	return result, rows.Err()
}

func scanConversationReactionEvents(rows *sql.Rows, scope Scope, conversationID string) ([]*ConversationReactionEvent, error) {
	defer rows.Close()
	result := make([]*ConversationReactionEvent, 0)
	for rows.Next() {
		event := &ConversationReactionEvent{Scope: scope, ConversationID: conversationID}
		if err := rows.Scan(&event.Sequence, &event.Type, &event.MessageID, &event.Actor.Type, &event.Actor.ID, &event.Emoji, &event.CreatedAt); err != nil {
			return nil, err
		}
		event.CreatedAt = event.CreatedAt.UTC()
		result = append(result, event)
	}
	return result, rows.Err()
}

func reactionEventLimit(limit int) int {
	if limit <= 0 || limit > maximumConversationChangeLimit+1 {
		return defaultConversationChangeLimit
	}
	return limit
}

// SQLite

func (s *SQLiteStore) AddMessageReaction(ctx context.Context, record MessageReactionRecord) (*ConversationReactionEvent, error) {
	return s.changeSQLiteReaction(ctx, record, true)
}

func (s *SQLiteStore) RemoveMessageReaction(ctx context.Context, record MessageReactionRecord) (*ConversationReactionEvent, error) {
	return s.changeSQLiteReaction(ctx, record, false)
}

func (s *SQLiteStore) changeSQLiteReaction(ctx context.Context, record MessageReactionRecord, add bool) (*ConversationReactionEvent, error) {
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
		return nil, ErrConversationNotFound
	}
	key := []interface{}{record.Scope.Kind, record.Scope.ID, record.ConversationID, record.MessageID, record.Actor.Type, record.Actor.ID, record.Emoji}
	eventType := ConversationReactionAdded
	var result sql.Result
	if add {
		result, err = conn.ExecContext(ctx, `INSERT INTO conversation_message_reactions
			(scope_kind, scope_id, conversation_id, message_id, actor_type, actor_id, emoji, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, append(key, record.CreatedAt)...)
	} else {
		eventType = ConversationReactionRemoved
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
	event := newConversationReactionEvent(eventType, record, sequence)
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

func (s *SQLiteStore) ListMessageReactions(ctx context.Context, scope Scope, conversationID string, messageIDs []string) ([]*MessageReactionRecord, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if len(messageIDs) == 0 {
		return []*MessageReactionRecord{}, nil
	}
	args := []interface{}{scope.Kind, scope.ID, conversationID}
	for _, id := range messageIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT message_id, actor_type, actor_id, emoji, created_at FROM conversation_message_reactions
		WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND message_id IN (`+reactionMessagePlaceholders(4, len(messageIDs), false)+`)
		ORDER BY created_at ASC, actor_type ASC, actor_id ASC, emoji ASC`, args...)
	if err != nil {
		return nil, err
	}
	return scanMessageReactionRecords(rows, scope, conversationID)
}

func (s *SQLiteStore) ListConversationReactionEvents(ctx context.Context, scope Scope, conversationID string, afterSequence int64, limit int) ([]*ConversationReactionEvent, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sequence, type, message_id, actor_type, actor_id, emoji, created_at FROM conversation_reaction_events
		WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND sequence > ? ORDER BY sequence ASC LIMIT ?`,
		scope.Kind, scope.ID, conversationID, afterSequence, reactionEventLimit(limit))
	if err != nil {
		return nil, err
	}
	return scanConversationReactionEvents(rows, scope, conversationID)
}

// PostgreSQL

func (s *PostgresStore) migrateConversationReactions(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, conversationReactionMigrationVersion)
	if err != nil || applied {
		return err
	}
	reactions, events := s.table("conversation_message_reactions"), s.table("conversation_reaction_events")
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS ` + reactions + ` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, conversation_id TEXT NOT NULL, message_id TEXT NOT NULL,
			actor_type TEXT NOT NULL, actor_id TEXT NOT NULL, emoji TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, conversation_id, message_id, actor_type, actor_id, emoji),
			CHECK (octet_length(emoji) BETWEEN 1 AND 16)
		)`,
		`CREATE TABLE IF NOT EXISTS ` + events + ` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, conversation_id TEXT NOT NULL, sequence BIGINT NOT NULL,
			type TEXT NOT NULL, message_id TEXT NOT NULL, actor_type TEXT NOT NULL, actor_id TEXT NOT NULL,
			emoji TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, conversation_id, sequence),
			CHECK (sequence > 0)
		)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES($1,'conversation message reactions') ON CONFLICT(version) DO NOTHING`, conversationReactionMigrationVersion)
	return err
}

func (s *PostgresStore) AddMessageReaction(ctx context.Context, record MessageReactionRecord) (*ConversationReactionEvent, error) {
	return s.changePostgresReaction(ctx, record, true)
}

func (s *PostgresStore) RemoveMessageReaction(ctx context.Context, record MessageReactionRecord) (*ConversationReactionEvent, error) {
	return s.changePostgresReaction(ctx, record, false)
}

func (s *PostgresStore) changePostgresReaction(ctx context.Context, record MessageReactionRecord, add bool) (*ConversationReactionEvent, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// One lock per conversation serializes event Sequence assignment, so a
	// reader never sees a later Sequence commit before an earlier one.
	if err := postgresConversationAdvisoryLock(ctx, tx, record.Scope, "reactions:"+record.ConversationID); err != nil {
		return nil, err
	}
	conversation, err := s.getPostgresConversation(ctx, tx, record.Scope, record.ConversationID, false)
	if err != nil {
		return nil, err
	}
	if conversation == nil {
		return nil, ErrConversationNotFound
	}
	key := []interface{}{record.Scope.Kind, record.Scope.ID, record.ConversationID, record.MessageID, record.Actor.Type, record.Actor.ID, record.Emoji}
	eventType := ConversationReactionAdded
	var result sql.Result
	if add {
		result, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("conversation_message_reactions")+`
			(scope_kind, scope_id, conversation_id, message_id, actor_type, actor_id, emoji, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, append(key, record.CreatedAt)...)
	} else {
		eventType = ConversationReactionRemoved
		result, err = tx.ExecContext(ctx, `DELETE FROM `+s.table("conversation_message_reactions")+`
			WHERE scope_kind=$1 AND scope_id=$2 AND conversation_id=$3 AND message_id=$4 AND actor_type=$5 AND actor_id=$6 AND emoji=$7`, key...)
	}
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed == 0 {
		return nil, tx.Commit()
	}
	events := s.table("conversation_reaction_events")
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM `+events+`
		WHERE scope_kind=$1 AND scope_id=$2 AND conversation_id=$3`, record.Scope.Kind, record.Scope.ID, record.ConversationID).Scan(&sequence); err != nil {
		return nil, err
	}
	event := newConversationReactionEvent(eventType, record, sequence)
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+events+`
		(scope_kind, scope_id, conversation_id, sequence, type, message_id, actor_type, actor_id, emoji, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, record.Scope.Kind, record.Scope.ID, record.ConversationID, sequence, eventType,
		record.MessageID, record.Actor.Type, record.Actor.ID, record.Emoji, record.CreatedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return event, nil
}

func (s *PostgresStore) ListMessageReactions(ctx context.Context, scope Scope, conversationID string, messageIDs []string) ([]*MessageReactionRecord, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if len(messageIDs) == 0 {
		return []*MessageReactionRecord{}, nil
	}
	args := []interface{}{scope.Kind, scope.ID, conversationID}
	for _, id := range messageIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT message_id, actor_type, actor_id, emoji, created_at FROM `+s.table("conversation_message_reactions")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND conversation_id=$3 AND message_id IN (`+reactionMessagePlaceholders(4, len(messageIDs), true)+`)
		ORDER BY created_at ASC, actor_type ASC, actor_id ASC, emoji ASC`, args...)
	if err != nil {
		return nil, err
	}
	return scanMessageReactionRecords(rows, scope, conversationID)
}

func (s *PostgresStore) ListConversationReactionEvents(ctx context.Context, scope Scope, conversationID string, afterSequence int64, limit int) ([]*ConversationReactionEvent, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sequence, type, message_id, actor_type, actor_id, emoji, created_at FROM `+s.table("conversation_reaction_events")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND conversation_id=$3 AND sequence > $4 ORDER BY sequence ASC LIMIT $5`,
		scope.Kind, scope.ID, conversationID, afterSequence, reactionEventLimit(limit))
	if err != nil {
		return nil, err
	}
	return scanConversationReactionEvents(rows, scope, conversationID)
}
