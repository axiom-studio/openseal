package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func migrateConversations(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS conversations (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			owner_type TEXT NOT NULL, owner_id TEXT NOT NULL, status TEXT NOT NULL,
			idempotency_key TEXT NOT NULL, last_sequence INTEGER NOT NULL, revision INTEGER NOT NULL,
			created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id),
			UNIQUE (scope_kind, scope_id, idempotency_key)
		);
		CREATE INDEX IF NOT EXISTS idx_conversations_owner
			ON conversations(scope_kind, scope_id, owner_type, owner_id, status, updated_at DESC);
		CREATE TABLE IF NOT EXISTS channel_messages (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, conversation_id TEXT NOT NULL, id TEXT NOT NULL,
			sequence INTEGER NOT NULL, sender_type TEXT NOT NULL, sender_id TEXT NOT NULL, intent TEXT NOT NULL,
			thread_root_id TEXT NOT NULL DEFAULT '', participation_round_id TEXT NOT NULL DEFAULT '',
			idempotency_key TEXT NOT NULL, created_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, conversation_id, id),
			UNIQUE (scope_kind, scope_id, conversation_id, sequence),
			UNIQUE (scope_kind, scope_id, conversation_id, idempotency_key)
		);
		CREATE INDEX IF NOT EXISTS idx_channel_messages_sequence
			ON channel_messages(scope_kind, scope_id, conversation_id, sequence);
		CREATE INDEX IF NOT EXISTS idx_channel_messages_thread
			ON channel_messages(scope_kind, scope_id, conversation_id, thread_root_id, sequence);
		CREATE INDEX IF NOT EXISTS idx_channel_messages_round
			ON channel_messages(scope_kind, scope_id, conversation_id, participation_round_id, sequence);
		CREATE TABLE IF NOT EXISTS participation_rounds (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, conversation_id TEXT NOT NULL, id TEXT NOT NULL,
			status TEXT NOT NULL, idempotency_key TEXT NOT NULL, revision INTEGER NOT NULL,
			created_at DATETIME NOT NULL, committed_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, conversation_id, id),
			UNIQUE (scope_kind, scope_id, conversation_id, idempotency_key)
		);
		CREATE TABLE IF NOT EXISTS conversation_cursors (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, conversation_id TEXT NOT NULL,
			participant_type TEXT NOT NULL, participant_id TEXT NOT NULL,
			delivered_sequence INTEGER NOT NULL, read_sequence INTEGER NOT NULL, revision INTEGER NOT NULL,
			updated_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, conversation_id, participant_type, participant_id)
		);
		CREATE TABLE IF NOT EXISTS conversation_presence (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, conversation_id TEXT NOT NULL,
			participant_type TEXT NOT NULL, participant_id TEXT NOT NULL, state TEXT NOT NULL,
			lease_id TEXT NOT NULL, revision INTEGER NOT NULL, updated_at DATETIME NOT NULL, expires_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, conversation_id, participant_type, participant_id)
		);
		CREATE INDEX IF NOT EXISTS idx_conversation_presence_active
			ON conversation_presence(scope_kind, scope_id, conversation_id, expires_at);
	`)
	return err
}

func (s *SQLiteStore) CreateConversation(ctx context.Context, conversation *Conversation, idempotencyKey string) (*Conversation, bool, error) {
	if err := conversation.Validate(); err != nil {
		return nil, false, err
	}
	key := strings.TrimSpace(idempotencyKey)
	if key == "" {
		return nil, false, ErrInvalidConversation
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, false, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	existing, err := getSQLiteConversationByKey(ctx, conn, conversation.Scope, key)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		if existing.Owner != conversation.Owner || existing.Title != conversation.Title {
			return nil, false, ErrMessageConflict
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, false, err
		}
		committed = true
		return existing, true, nil
	}
	payload, err := json.Marshal(conversation)
	if err != nil {
		return nil, false, err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO conversations
		(scope_kind, scope_id, id, owner_type, owner_id, status, idempotency_key, last_sequence, revision, created_at, updated_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, conversation.Scope.Kind, conversation.Scope.ID, conversation.ID,
		conversation.Owner.Type, conversation.Owner.ID, conversation.Status, key, conversation.LastSequence, conversation.Revision,
		conversation.CreatedAt, conversation.UpdatedAt, string(payload))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, false, ErrMessageConflict
		}
		return nil, false, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, false, err
	}
	committed = true
	return cloneConversation(conversation), false, nil
}

func (s *SQLiteStore) GetConversation(ctx context.Context, scope Scope, conversationID string) (*Conversation, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return getSQLiteConversation(ctx, s.db, scope, strings.TrimSpace(conversationID))
}

func (s *SQLiteStore) FindConversationByIdempotencyKey(ctx context.Context, scope Scope, key string) (*Conversation, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, nil
	}
	return getSQLiteConversationByKey(ctx, s.db, scope, strings.TrimSpace(key))
}

func (s *SQLiteStore) ListConversations(ctx context.Context, filter ConversationFilter) ([]*Conversation, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT payload FROM conversations WHERE scope_kind = ? AND scope_id = ?`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	if filter.Owner != nil {
		query += ` AND owner_type = ? AND owner_id = ?`
		args = append(args, filter.Owner.Type, filter.Owner.ID)
	}
	statuses := make([]string, 0, len(filter.Statuses))
	for _, status := range filter.Statuses {
		statuses = append(statuses, string(status))
	}
	query, args = appendSQLiteActivityStrings(query, args, "status", statuses)
	query += ` ORDER BY updated_at DESC, id ASC LIMIT ? OFFSET ?`
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*Conversation, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		conversation, err := decodeConversation(payload)
		if err != nil {
			return nil, err
		}
		result = append(result, conversation)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) CommitChannelMessage(ctx context.Context, record ChannelMessageCommitRecord) (*ChannelMessageCommitResult, error) {
	if err := validateChannelMessageCommitRecord(record); err != nil {
		return nil, err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	existing, err := getSQLiteChannelMessageByKey(ctx, conn, record.Message.Scope, record.Message.ConversationID, record.Message.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if !sameChannelMessage(existing, record.Message) {
			return nil, ErrMessageConflict
		}
		conversation, err := getSQLiteConversation(ctx, conn, record.Message.Scope, record.Message.ConversationID)
		if err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, err
		}
		committed = true
		return &ChannelMessageCommitResult{Conversation: conversation, Message: existing, Replayed: true}, nil
	}
	current, err := getSQLiteConversation(ctx, conn, record.Conversation.Scope, record.Conversation.ID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, ErrConversationNotFound
	}
	if current.Revision != record.ExpectedRevision || record.Conversation.Revision != current.Revision+1 ||
		record.Message.Sequence != current.LastSequence+1 || record.Conversation.LastSequence != record.Message.Sequence {
		return nil, ErrRevisionConflict
	}
	if err := updateSQLiteConversation(ctx, conn, record.Conversation, record.ExpectedRevision); err != nil {
		return nil, err
	}
	if err := insertSQLiteChannelMessage(ctx, conn, record.Message); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return &ChannelMessageCommitResult{Conversation: cloneConversation(record.Conversation), Message: cloneChannelMessage(record.Message)}, nil
}

func (s *SQLiteStore) GetChannelMessage(ctx context.Context, scope Scope, conversationID, messageID string) (*ChannelMessage, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return getSQLiteChannelMessage(ctx, s.db, scope, conversationID, messageID)
}

func (s *SQLiteStore) FindChannelMessageByIdempotencyKey(ctx context.Context, scope Scope, conversationID, key string) (*ChannelMessage, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, nil
	}
	return getSQLiteChannelMessageByKey(ctx, s.db, scope, conversationID, strings.TrimSpace(key))
}

func (s *SQLiteStore) ListChannelMessages(ctx context.Context, filter ChannelMessageFilter) ([]*ChannelMessage, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	conversation, err := getSQLiteConversation(ctx, s.db, filter.Scope, filter.ConversationID)
	if err != nil {
		return nil, err
	}
	if conversation == nil {
		return nil, ErrConversationNotFound
	}
	query := `SELECT payload FROM channel_messages WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND sequence > ?`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID, filter.ConversationID, filter.AfterSequence}
	if filter.ThreadRootID != "" {
		query += ` AND (thread_root_id = ? OR id = ?)`
		args = append(args, filter.ThreadRootID, filter.ThreadRootID)
	}
	intents := make([]string, 0, len(filter.Intents))
	for _, intent := range filter.Intents {
		intents = append(intents, string(intent))
	}
	query, args = appendSQLiteActivityStrings(query, args, "intent", intents)
	if filter.Descending {
		query += ` ORDER BY sequence DESC`
	} else {
		query += ` ORDER BY sequence ASC`
	}
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query += ` LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*ChannelMessage, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		message, err := decodeChannelMessage(payload)
		if err != nil {
			return nil, err
		}
		result = append(result, message)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) CommitParticipationRound(ctx context.Context, record ParticipationRoundCommitRecord) (*ParticipationRoundResult, error) {
	if err := validateParticipationRoundCommitRecord(record); err != nil {
		return nil, err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	existing, err := getSQLiteParticipationRoundByKey(ctx, conn, record.Round.Scope, record.Round.ConversationID, record.Round.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if !sameParticipationRound(existing, record.Round) {
			return nil, ErrMessageConflict
		}
		result, err := loadSQLiteParticipationRoundResult(ctx, conn, existing)
		if err != nil {
			return nil, err
		}
		result.Replayed = true
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, err
		}
		committed = true
		return result, nil
	}
	current, err := getSQLiteConversation(ctx, conn, record.Conversation.Scope, record.Conversation.ID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, ErrConversationNotFound
	}
	if current.Revision != record.ExpectedRevision || record.Conversation.Revision != current.Revision+1 {
		return nil, ErrRevisionConflict
	}
	expectedSequence := current.LastSequence
	for _, message := range record.Messages {
		expectedSequence++
		if message.Sequence != expectedSequence || message.ParticipationRoundID != record.Round.ID {
			return nil, ErrInvalidConversation
		}
	}
	if record.Conversation.LastSequence != expectedSequence {
		return nil, ErrInvalidConversation
	}
	if err := updateSQLiteConversation(ctx, conn, record.Conversation, record.ExpectedRevision); err != nil {
		return nil, err
	}
	for _, message := range record.Messages {
		if err := insertSQLiteChannelMessage(ctx, conn, message); err != nil {
			return nil, err
		}
	}
	if err := insertSQLiteParticipationRound(ctx, conn, record.Round); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return &ParticipationRoundResult{Conversation: cloneConversation(record.Conversation), Round: cloneParticipationRound(record.Round), Messages: cloneChannelMessages(record.Messages)}, nil
}

func (s *SQLiteStore) FindParticipationRoundByIdempotencyKey(ctx context.Context, scope Scope, conversationID, key string) (*ParticipationRoundResult, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, nil
	}
	round, err := getSQLiteParticipationRoundByKey(ctx, s.db, scope, conversationID, strings.TrimSpace(key))
	if err != nil || round == nil {
		return nil, err
	}
	result, err := loadSQLiteParticipationRoundResult(ctx, s.db, round)
	if result != nil {
		result.Replayed = true
	}
	return result, err
}

func (s *SQLiteStore) GetConversationCursor(ctx context.Context, scope Scope, conversationID string, participant ConversationParticipant) (*ConversationCursor, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := participant.Validate(); err != nil {
		return nil, err
	}
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM conversation_cursors WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND participant_type = ? AND participant_id = ?`,
		scope.Kind, scope.ID, conversationID, participant.Type, participant.ID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeConversationCursor(payload)
}

func (s *SQLiteStore) PutConversationCursor(ctx context.Context, record ConversationCursorRecord) (*ConversationCursor, bool, error) {
	if record.Cursor == nil {
		return nil, false, ErrConversationCursorConflict
	}
	if err := record.Cursor.Validate(); err != nil {
		return nil, false, err
	}
	payload, err := json.Marshal(record.Cursor)
	if err != nil {
		return nil, false, err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, false, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	conversation, err := getSQLiteConversation(ctx, conn, record.Cursor.Scope, record.Cursor.ConversationID)
	if err != nil {
		return nil, false, err
	}
	if conversation == nil {
		return nil, false, ErrConversationNotFound
	}
	if record.Cursor.DeliveredSequence > conversation.LastSequence {
		return nil, false, ErrConversationCursorConflict
	}
	current, err := getSQLiteConversationCursor(ctx, conn, record.Cursor.Scope, record.Cursor.ConversationID, record.Cursor.Participant)
	if err != nil {
		return nil, false, err
	}
	if current != nil && current.DeliveredSequence == record.Cursor.DeliveredSequence && current.ReadSequence == record.Cursor.ReadSequence {
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, false, err
		}
		committed = true
		return current, true, nil
	}
	if current == nil {
		if record.ExpectedRevision != 0 {
			return nil, false, ErrConversationCursorConflict
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO conversation_cursors
			(scope_kind, scope_id, conversation_id, participant_type, participant_id, delivered_sequence, read_sequence, revision, updated_at, payload)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, record.Cursor.Scope.Kind, record.Cursor.Scope.ID, record.Cursor.ConversationID,
			record.Cursor.Participant.Type, record.Cursor.Participant.ID, record.Cursor.DeliveredSequence, record.Cursor.ReadSequence,
			record.Cursor.Revision, record.Cursor.UpdatedAt, string(payload))
	} else {
		if current.Revision != record.ExpectedRevision || record.Cursor.Revision != current.Revision+1 {
			return nil, false, ErrConversationCursorConflict
		}
		var result sql.Result
		result, err = conn.ExecContext(ctx, `UPDATE conversation_cursors SET delivered_sequence = ?, read_sequence = ?, revision = ?, updated_at = ?, payload = ?
			WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND participant_type = ? AND participant_id = ? AND revision = ?`,
			record.Cursor.DeliveredSequence, record.Cursor.ReadSequence, record.Cursor.Revision, record.Cursor.UpdatedAt, string(payload),
			record.Cursor.Scope.Kind, record.Cursor.Scope.ID, record.Cursor.ConversationID, record.Cursor.Participant.Type, record.Cursor.Participant.ID, record.ExpectedRevision)
		if err == nil {
			err = expectDependencyRow(result, nil)
		}
	}
	if err != nil {
		return nil, false, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, false, err
	}
	committed = true
	return cloneConversationCursor(record.Cursor), false, nil
}

func (s *SQLiteStore) GetConversationPresence(ctx context.Context, scope Scope, conversationID string, participant ConversationParticipant) (*ConversationPresence, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := participant.Validate(); err != nil {
		return nil, err
	}
	return getSQLiteConversationPresence(ctx, s.db, scope, conversationID, participant)
}

func (s *SQLiteStore) PutConversationPresence(ctx context.Context, record ConversationPresenceRecord) (*ConversationPresence, error) {
	if record.Presence == nil {
		return nil, ErrConversationPresenceConflict
	}
	if err := record.Presence.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(record.Presence)
	if err != nil {
		return nil, err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	conversation, err := getSQLiteConversation(ctx, conn, record.Presence.Scope, record.Presence.ConversationID)
	if err != nil {
		return nil, err
	}
	if conversation == nil {
		return nil, ErrConversationNotFound
	}
	current, err := getSQLiteConversationPresence(ctx, conn, record.Presence.Scope, record.Presence.ConversationID, record.Presence.Participant)
	if err != nil {
		return nil, err
	}
	if current != nil && current.ExpiresAt.After(record.Presence.UpdatedAt) {
		if current.Revision != record.ExpectedRevision || current.LeaseID != record.Presence.LeaseID || record.Presence.Revision != current.Revision+1 {
			return nil, ErrConversationPresenceConflict
		}
		result, err := conn.ExecContext(ctx, `UPDATE conversation_presence SET state = ?, lease_id = ?, revision = ?, updated_at = ?, expires_at = ?, payload = ?
			WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND participant_type = ? AND participant_id = ? AND revision = ?`,
			record.Presence.State, record.Presence.LeaseID, record.Presence.Revision, record.Presence.UpdatedAt, record.Presence.ExpiresAt,
			string(payload), record.Presence.Scope.Kind, record.Presence.Scope.ID, record.Presence.ConversationID,
			record.Presence.Participant.Type, record.Presence.Participant.ID, record.ExpectedRevision)
		if err != nil {
			return nil, err
		}
		if err := expectDependencyRow(result, nil); err != nil {
			return nil, err
		}
	} else {
		if record.ExpectedRevision != 0 || record.Presence.Revision != 1 {
			return nil, ErrConversationPresenceConflict
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO conversation_presence
			(scope_kind, scope_id, conversation_id, participant_type, participant_id, state, lease_id, revision, updated_at, expires_at, payload)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(scope_kind, scope_id, conversation_id, participant_type, participant_id) DO UPDATE SET
			state = excluded.state, lease_id = excluded.lease_id, revision = excluded.revision, updated_at = excluded.updated_at,
			expires_at = excluded.expires_at, payload = excluded.payload`, record.Presence.Scope.Kind, record.Presence.Scope.ID,
			record.Presence.ConversationID, record.Presence.Participant.Type, record.Presence.Participant.ID, record.Presence.State,
			record.Presence.LeaseID, record.Presence.Revision, record.Presence.UpdatedAt, record.Presence.ExpiresAt, string(payload))
		if err != nil {
			return nil, err
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return cloneConversationPresence(record.Presence), nil
}

func (s *SQLiteStore) ReleaseConversationPresence(ctx context.Context, request ReleaseConversationPresenceRequest) error {
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	if err := request.Participant.Validate(); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM conversation_presence WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ?
		AND participant_type = ? AND participant_id = ? AND lease_id = ?`, request.Scope.Kind, request.Scope.ID, request.ConversationID,
		request.Participant.Type, request.Participant.ID, strings.TrimSpace(request.LeaseID))
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		current, err := s.GetConversationPresence(ctx, request.Scope, request.ConversationID, request.Participant)
		if err != nil {
			return err
		}
		if current != nil {
			return ErrConversationPresenceConflict
		}
	}
	return nil
}

func (s *SQLiteStore) ListConversationPresence(ctx context.Context, scope Scope, conversationID string, activeAt time.Time) ([]*ConversationPresence, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM conversation_presence WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ?
		AND expires_at > ? ORDER BY participant_type, participant_id`, scope.Kind, scope.ID, conversationID, activeAt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*ConversationPresence, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		presence, err := decodeConversationPresence(payload)
		if err != nil {
			return nil, err
		}
		result = append(result, presence)
	}
	return result, rows.Err()
}

type sqliteConversationQueryer interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
}

func getSQLiteConversation(ctx context.Context, queryer sqliteConversationQueryer, scope Scope, id string) (*Conversation, error) {
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM conversations WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeConversation(payload)
}

func getSQLiteConversationByKey(ctx context.Context, queryer sqliteConversationQueryer, scope Scope, key string) (*Conversation, error) {
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM conversations WHERE scope_kind = ? AND scope_id = ? AND idempotency_key = ?`, scope.Kind, scope.ID, key).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeConversation(payload)
}

func updateSQLiteConversation(ctx context.Context, conn *sql.Conn, conversation *Conversation, expectedRevision int64) error {
	payload, err := json.Marshal(conversation)
	if err != nil {
		return err
	}
	result, err := conn.ExecContext(ctx, `UPDATE conversations SET status = ?, last_sequence = ?, revision = ?, updated_at = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ?`, conversation.Status, conversation.LastSequence,
		conversation.Revision, conversation.UpdatedAt, string(payload), conversation.Scope.Kind, conversation.Scope.ID, conversation.ID, expectedRevision)
	return expectDependencyRow(result, err)
}

func insertSQLiteChannelMessage(ctx context.Context, conn *sql.Conn, message *ChannelMessage) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO channel_messages
		(scope_kind, scope_id, conversation_id, id, sequence, sender_type, sender_id, intent, thread_root_id, participation_round_id, idempotency_key, created_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, message.Scope.Kind, message.Scope.ID, message.ConversationID, message.ID,
		message.Sequence, message.Sender.Type, message.Sender.ID, message.Intent, message.ThreadRootID, message.ParticipationRoundID,
		message.IdempotencyKey, message.CreatedAt, string(payload))
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
		return ErrMessageConflict
	}
	return err
}

func getSQLiteChannelMessage(ctx context.Context, queryer sqliteConversationQueryer, scope Scope, conversationID, id string) (*ChannelMessage, error) {
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM channel_messages WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND id = ?`,
		scope.Kind, scope.ID, conversationID, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeChannelMessage(payload)
}

func getSQLiteChannelMessageByKey(ctx context.Context, queryer sqliteConversationQueryer, scope Scope, conversationID, key string) (*ChannelMessage, error) {
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM channel_messages WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND idempotency_key = ?`,
		scope.Kind, scope.ID, conversationID, key).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeChannelMessage(payload)
}

func insertSQLiteParticipationRound(ctx context.Context, conn *sql.Conn, round *ParticipationRound) error {
	payload, err := json.Marshal(round)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO participation_rounds
		(scope_kind, scope_id, conversation_id, id, status, idempotency_key, revision, created_at, committed_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, round.Scope.Kind, round.Scope.ID, round.ConversationID, round.ID, round.Status,
		round.IdempotencyKey, round.Revision, round.CreatedAt, round.CommittedAt, string(payload))
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
		return ErrMessageConflict
	}
	return err
}

func getSQLiteParticipationRoundByKey(ctx context.Context, queryer sqliteConversationQueryer, scope Scope, conversationID, key string) (*ParticipationRound, error) {
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM participation_rounds WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND idempotency_key = ?`,
		scope.Kind, scope.ID, conversationID, key).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeParticipationRound(payload)
}

func loadSQLiteParticipationRoundResult(ctx context.Context, queryer sqliteConversationQueryer, round *ParticipationRound) (*ParticipationRoundResult, error) {
	conversation, err := getSQLiteConversation(ctx, queryer, round.Scope, round.ConversationID)
	if err != nil {
		return nil, err
	}
	rows, err := queryer.QueryContext(ctx, `SELECT payload FROM channel_messages WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND participation_round_id = ? ORDER BY sequence`,
		round.Scope.Kind, round.Scope.ID, round.ConversationID, round.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := make([]*ChannelMessage, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		message, err := decodeChannelMessage(payload)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return &ParticipationRoundResult{Conversation: conversation, Round: round, Messages: messages}, rows.Err()
}

func getSQLiteConversationCursor(ctx context.Context, queryer sqliteConversationQueryer, scope Scope, conversationID string, participant ConversationParticipant) (*ConversationCursor, error) {
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM conversation_cursors WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND participant_type = ? AND participant_id = ?`,
		scope.Kind, scope.ID, conversationID, participant.Type, participant.ID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeConversationCursor(payload)
}

func getSQLiteConversationPresence(ctx context.Context, queryer sqliteConversationQueryer, scope Scope, conversationID string, participant ConversationParticipant) (*ConversationPresence, error) {
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM conversation_presence WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND participant_type = ? AND participant_id = ?`,
		scope.Kind, scope.ID, conversationID, participant.Type, participant.ID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeConversationPresence(payload)
}

func decodeConversation(payload string) (*Conversation, error) {
	var value Conversation
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, fmt.Errorf("decode conversation: %w", err)
	}
	return &value, nil
}

func decodeChannelMessage(payload string) (*ChannelMessage, error) {
	var value ChannelMessage
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, fmt.Errorf("decode channel message: %w", err)
	}
	return &value, nil
}

func decodeParticipationRound(payload string) (*ParticipationRound, error) {
	var value ParticipationRound
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, fmt.Errorf("decode participation round: %w", err)
	}
	return &value, nil
}

func decodeConversationCursor(payload string) (*ConversationCursor, error) {
	var value ConversationCursor
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, fmt.Errorf("decode conversation cursor: %w", err)
	}
	return &value, nil
}

func decodeConversationPresence(payload string) (*ConversationPresence, error) {
	var value ConversationPresence
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, fmt.Errorf("decode conversation presence: %w", err)
	}
	return &value, nil
}

var _ ConversationStore = (*SQLiteStore)(nil)
