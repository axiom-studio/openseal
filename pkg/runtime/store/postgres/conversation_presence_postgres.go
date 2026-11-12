package postgres

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
	return s.getPostgresConversationCursor(ctx, s.db, scope, conversationID, participant, false)
}

func (s *Store) ListConversationCursors(ctx context.Context, scope models.Scope, conversationID string) ([]*models.ConversationCursor, error) {
	if _, err := s.GetConversation(ctx, scope, conversationID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("conversation_cursors")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND conversation_id=$3
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	lockKey := "cursor:" + record.Cursor.ConversationID + ":" + string(record.Cursor.Participant.Type) + ":" + record.Cursor.Participant.ID
	if err := postgresConversationAdvisoryLock(ctx, tx, record.Cursor.Scope, lockKey); err != nil {
		return nil, false, err
	}
	conversation, err := s.getPostgresConversation(ctx, tx, record.Cursor.Scope, record.Cursor.ConversationID, false)
	if err != nil {
		return nil, false, err
	}
	if conversation == nil {
		return nil, false, models.ErrConversationNotFound
	}
	if record.Cursor.DeliveredSequence > conversation.LastSequence {
		return nil, false, models.ErrConversationCursorConflict
	}
	current, err := s.getPostgresConversationCursor(ctx, tx, record.Cursor.Scope, record.Cursor.ConversationID, record.Cursor.Participant, true)
	if err != nil {
		return nil, false, err
	}
	if current != nil && current.DeliveredSequence == record.Cursor.DeliveredSequence && current.ReadSequence == record.Cursor.ReadSequence {
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return current, true, nil
	}
	payload, err := json.Marshal(record.Cursor)
	if err != nil {
		return nil, false, err
	}
	if current == nil {
		if record.ExpectedRevision != 0 {
			return nil, false, models.ErrConversationCursorConflict
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("conversation_cursors")+`
			(scope_kind, scope_id, conversation_id, participant_type, participant_id, delivered_sequence, read_sequence, revision, updated_at, payload)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb)`, record.Cursor.Scope.Kind, record.Cursor.Scope.ID,
			record.Cursor.ConversationID, record.Cursor.Participant.Type, record.Cursor.Participant.ID, record.Cursor.DeliveredSequence,
			record.Cursor.ReadSequence, record.Cursor.Revision, record.Cursor.UpdatedAt, string(payload))
	} else {
		if current.Revision != record.ExpectedRevision || record.Cursor.Revision != current.Revision+1 {
			return nil, false, models.ErrConversationCursorConflict
		}
		var result sql.Result
		result, err = tx.ExecContext(ctx, `UPDATE `+s.table("conversation_cursors")+` SET delivered_sequence=$1, read_sequence=$2,
			revision=$3, updated_at=$4, payload=$5::jsonb WHERE scope_kind=$6 AND scope_id=$7 AND conversation_id=$8
			AND participant_type=$9 AND participant_id=$10 AND revision=$11`, record.Cursor.DeliveredSequence, record.Cursor.ReadSequence,
			record.Cursor.Revision, record.Cursor.UpdatedAt, string(payload), record.Cursor.Scope.Kind, record.Cursor.Scope.ID,
			record.Cursor.ConversationID, record.Cursor.Participant.Type, record.Cursor.Participant.ID, record.ExpectedRevision)
		if err == nil {
			err = sqlstore.ExpectRevisionRow(result, nil)
		}
	}
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return models.CloneConversationCursor(record.Cursor), false, nil
}

func (s *Store) GetConversationPresence(ctx context.Context, scope models.Scope, conversationID string, participant models.ConversationParticipant) (*models.ConversationPresence, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := participant.Validate(); err != nil {
		return nil, err
	}
	return s.getPostgresConversationPresence(ctx, s.db, scope, conversationID, participant, false)
}

func (s *Store) PutConversationPresence(ctx context.Context, record models.ConversationPresenceRecord) (*models.ConversationPresence, error) {
	if record.Presence == nil {
		return nil, models.ErrConversationPresenceConflict
	}
	if err := record.Presence.Validate(); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	lockKey := "presence:" + record.Presence.ConversationID + ":" + string(record.Presence.Participant.Type) + ":" + record.Presence.Participant.ID
	if err := postgresConversationAdvisoryLock(ctx, tx, record.Presence.Scope, lockKey); err != nil {
		return nil, err
	}
	conversation, err := s.getPostgresConversation(ctx, tx, record.Presence.Scope, record.Presence.ConversationID, false)
	if err != nil {
		return nil, err
	}
	if conversation == nil {
		return nil, models.ErrConversationNotFound
	}
	current, err := s.getPostgresConversationPresence(ctx, tx, record.Presence.Scope, record.Presence.ConversationID, record.Presence.Participant, true)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(record.Presence)
	if err != nil {
		return nil, err
	}
	if current != nil && current.ExpiresAt.After(record.Presence.UpdatedAt) {
		if current.Revision != record.ExpectedRevision || current.LeaseID != record.Presence.LeaseID || record.Presence.Revision != current.Revision+1 {
			return nil, models.ErrConversationPresenceConflict
		}
		result, err := tx.ExecContext(ctx, `UPDATE `+s.table("conversation_presence")+` SET state=$1, lease_id=$2, revision=$3,
			updated_at=$4, expires_at=$5, payload=$6::jsonb WHERE scope_kind=$7 AND scope_id=$8 AND conversation_id=$9
			AND participant_type=$10 AND participant_id=$11 AND revision=$12`, record.Presence.State, record.Presence.LeaseID,
			record.Presence.Revision, record.Presence.UpdatedAt, record.Presence.ExpiresAt, string(payload), record.Presence.Scope.Kind,
			record.Presence.Scope.ID, record.Presence.ConversationID, record.Presence.Participant.Type, record.Presence.Participant.ID,
			record.ExpectedRevision)
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
		_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("conversation_presence")+`
			(scope_kind, scope_id, conversation_id, participant_type, participant_id, state, lease_id, revision, updated_at, expires_at, payload)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb)
			ON CONFLICT (scope_kind, scope_id, conversation_id, participant_type, participant_id) DO UPDATE SET
			state=EXCLUDED.state, lease_id=EXCLUDED.lease_id, revision=EXCLUDED.revision, updated_at=EXCLUDED.updated_at,
			expires_at=EXCLUDED.expires_at, payload=EXCLUDED.payload`, record.Presence.Scope.Kind, record.Presence.Scope.ID,
			record.Presence.ConversationID, record.Presence.Participant.Type, record.Presence.Participant.ID, record.Presence.State,
			record.Presence.LeaseID, record.Presence.Revision, record.Presence.UpdatedAt, record.Presence.ExpiresAt, string(payload))
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return models.CloneConversationPresence(record.Presence), nil
}

func (s *Store) ReleaseConversationPresence(ctx context.Context, request models.ReleaseConversationPresenceRequest) error {
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	if err := request.Participant.Validate(); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM `+s.table("conversation_presence")+` WHERE scope_kind=$1 AND scope_id=$2
		AND conversation_id=$3 AND participant_type=$4 AND participant_id=$5 AND lease_id=$6`, request.Scope.Kind, request.Scope.ID,
		request.ConversationID, request.Participant.Type, request.Participant.ID, strings.TrimSpace(request.LeaseID))
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
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("conversation_presence")+` WHERE scope_kind=$1 AND scope_id=$2
		AND conversation_id=$3 AND expires_at > $4 ORDER BY participant_type, participant_id`, scope.Kind, scope.ID, conversationID, activeAt)
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

func (s *Store) getPostgresConversationCursor(ctx context.Context, queryer sqlstore.Reader, scope models.Scope, conversationID string, participant models.ConversationParticipant, lock bool) (*models.ConversationCursor, error) {
	query := `SELECT payload FROM ` + s.table("conversation_cursors") + ` WHERE scope_kind=$1 AND scope_id=$2 AND conversation_id=$3 AND participant_type=$4 AND participant_id=$5`
	if lock {
		query += ` FOR UPDATE`
	}
	var payload string
	err := queryer.QueryRowContext(ctx, query, scope.Kind, scope.ID, conversationID, participant.Type, participant.ID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sqlstore.DecodeConversationCursor(payload)
}

func (s *Store) getPostgresConversationPresence(ctx context.Context, queryer sqlstore.Reader, scope models.Scope, conversationID string, participant models.ConversationParticipant, lock bool) (*models.ConversationPresence, error) {
	query := `SELECT payload FROM ` + s.table("conversation_presence") + ` WHERE scope_kind=$1 AND scope_id=$2 AND conversation_id=$3 AND participant_type=$4 AND participant_id=$5`
	if lock {
		query += ` FOR UPDATE`
	}
	var payload string
	err := queryer.QueryRowContext(ctx, query, scope.Kind, scope.ID, conversationID, participant.Type, participant.ID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sqlstore.DecodeConversationPresence(payload)
}
