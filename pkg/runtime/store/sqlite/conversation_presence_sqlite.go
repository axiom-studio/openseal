package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

func (s *Store) GetConversationCursor(ctx context.Context, scope models.Scope, conversationID string, participant models.ConversationParticipant) (*models.ConversationCursor, error) {
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
	return sqlstore.DecodeConversationCursor(payload)
}

func (s *Store) ListConversationCursors(ctx context.Context, scope models.Scope, conversationID string) ([]*models.ConversationCursor, error) {
	if _, err := s.GetConversation(ctx, scope, conversationID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM conversation_cursors
		WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ?
		ORDER BY participant_type, participant_id`, scope.Kind, scope.ID, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*models.ConversationCursor, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		cursor, err := sqlstore.DecodeConversationCursor(payload)
		if err != nil {
			return nil, err
		}
		result = append(result, cursor)
	}
	return result, rows.Err()
}

func (s *Store) PutConversationCursor(ctx context.Context, record models.ConversationCursorRecord) (*models.ConversationCursor, bool, error) {
	if record.Cursor == nil {
		return nil, false, models.ErrConversationCursorConflict
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
		return nil, false, models.ErrConversationNotFound
	}
	if record.Cursor.DeliveredSequence > conversation.LastSequence {
		return nil, false, models.ErrConversationCursorConflict
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
			return nil, false, models.ErrConversationCursorConflict
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO conversation_cursors
			(scope_kind, scope_id, conversation_id, participant_type, participant_id, delivered_sequence, read_sequence, revision, updated_at, payload)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, record.Cursor.Scope.Kind, record.Cursor.Scope.ID, record.Cursor.ConversationID,
			record.Cursor.Participant.Type, record.Cursor.Participant.ID, record.Cursor.DeliveredSequence, record.Cursor.ReadSequence,
			record.Cursor.Revision, record.Cursor.UpdatedAt, string(payload))
	} else {
		if current.Revision != record.ExpectedRevision || record.Cursor.Revision != current.Revision+1 {
			return nil, false, models.ErrConversationCursorConflict
		}
		var result sql.Result
		result, err = conn.ExecContext(ctx, `UPDATE conversation_cursors SET delivered_sequence = ?, read_sequence = ?, revision = ?, updated_at = ?, payload = ?
			WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND participant_type = ? AND participant_id = ? AND revision = ?`,
			record.Cursor.DeliveredSequence, record.Cursor.ReadSequence, record.Cursor.Revision, record.Cursor.UpdatedAt, string(payload),
			record.Cursor.Scope.Kind, record.Cursor.Scope.ID, record.Cursor.ConversationID, record.Cursor.Participant.Type, record.Cursor.Participant.ID, record.ExpectedRevision)
		if err == nil {
			err = sqlstore.ExpectRevisionRow(result, nil)
		}
	}
	if err != nil {
		return nil, false, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, false, err
	}
	committed = true
	return models.CloneConversationCursor(record.Cursor), false, nil
}

func (s *Store) GetConversationPresence(ctx context.Context, scope models.Scope, conversationID string, participant models.ConversationParticipant) (*models.ConversationPresence, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := participant.Validate(); err != nil {
		return nil, err
	}
	return getSQLiteConversationPresence(ctx, s.db, scope, conversationID, participant)
}

func (s *Store) PutConversationPresence(ctx context.Context, record models.ConversationPresenceRecord) (*models.ConversationPresence, error) {
	if record.Presence == nil {
		return nil, models.ErrConversationPresenceConflict
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
		return nil, models.ErrConversationNotFound
	}
	current, err := getSQLiteConversationPresence(ctx, conn, record.Presence.Scope, record.Presence.ConversationID, record.Presence.Participant)
	if err != nil {
		return nil, err
	}
	if current != nil && current.ExpiresAt.After(record.Presence.UpdatedAt) {
		if current.Revision != record.ExpectedRevision || current.LeaseID != record.Presence.LeaseID || record.Presence.Revision != current.Revision+1 {
			return nil, models.ErrConversationPresenceConflict
		}
		result, err := conn.ExecContext(ctx, `UPDATE conversation_presence SET state = ?, lease_id = ?, revision = ?, updated_at = ?, expires_at = ?, payload = ?
			WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND participant_type = ? AND participant_id = ? AND revision = ?`,
			record.Presence.State, record.Presence.LeaseID, record.Presence.Revision, record.Presence.UpdatedAt, record.Presence.ExpiresAt,
			string(payload), record.Presence.Scope.Kind, record.Presence.Scope.ID, record.Presence.ConversationID,
			record.Presence.Participant.Type, record.Presence.Participant.ID, record.ExpectedRevision)
		if err != nil {
			return nil, err
		}
		if err := sqlstore.ExpectRevisionRow(result, nil); err != nil {
			return nil, err
		}
	} else {
		if record.ExpectedRevision != 0 || record.Presence.Revision != 1 {
			return nil, models.ErrConversationPresenceConflict
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
	return models.CloneConversationPresence(record.Presence), nil
}

func (s *Store) ReleaseConversationPresence(ctx context.Context, request models.ReleaseConversationPresenceRequest) error {
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
			return models.ErrConversationPresenceConflict
		}
	}
	return nil
}

func (s *Store) ListConversationPresence(ctx context.Context, scope models.Scope, conversationID string, activeAt time.Time) ([]*models.ConversationPresence, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM conversation_presence WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ?
		AND expires_at > ? ORDER BY participant_type, participant_id`, scope.Kind, scope.ID, conversationID, activeAt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*models.ConversationPresence, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		presence, err := sqlstore.DecodeConversationPresence(payload)
		if err != nil {
			return nil, err
		}
		result = append(result, presence)
	}
	return result, rows.Err()
}

func getSQLiteConversationCursor(ctx context.Context, queryer sqlstore.Reader, scope models.Scope, conversationID string, participant models.ConversationParticipant) (*models.ConversationCursor, error) {
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM conversation_cursors WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND participant_type = ? AND participant_id = ?`,
		scope.Kind, scope.ID, conversationID, participant.Type, participant.ID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sqlstore.DecodeConversationCursor(payload)
}

func getSQLiteConversationPresence(ctx context.Context, queryer sqlstore.Reader, scope models.Scope, conversationID string, participant models.ConversationParticipant) (*models.ConversationPresence, error) {
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM conversation_presence WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND participant_type = ? AND participant_id = ?`,
		scope.Kind, scope.ID, conversationID, participant.Type, participant.ID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sqlstore.DecodeConversationPresence(payload)
}
