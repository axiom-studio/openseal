package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

func (s *PostgresStore) migrateConversations(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("conversations")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			owner_type TEXT NOT NULL, owner_id TEXT NOT NULL, status TEXT NOT NULL,
			idempotency_key TEXT NOT NULL, last_sequence BIGINT NOT NULL, revision BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id), UNIQUE (scope_kind, scope_id, idempotency_key),
			CHECK (last_sequence >= 0), CHECK (revision > 0)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("channel_messages")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, conversation_id TEXT NOT NULL, id TEXT NOT NULL,
			sequence BIGINT NOT NULL, sender_type TEXT NOT NULL, sender_id TEXT NOT NULL, intent TEXT NOT NULL,
			thread_root_id TEXT NOT NULL DEFAULT '', participation_round_id TEXT NOT NULL DEFAULT '',
			idempotency_key TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, conversation_id, id),
			UNIQUE (scope_kind, scope_id, conversation_id, sequence),
			UNIQUE (scope_kind, scope_id, conversation_id, idempotency_key), CHECK (sequence > 0)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("participation_rounds")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, conversation_id TEXT NOT NULL, id TEXT NOT NULL,
			status TEXT NOT NULL, idempotency_key TEXT NOT NULL, revision BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL, committed_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, conversation_id, id),
			UNIQUE (scope_kind, scope_id, conversation_id, idempotency_key), CHECK (revision > 0)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("conversation_cursors")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, conversation_id TEXT NOT NULL,
			participant_type TEXT NOT NULL, participant_id TEXT NOT NULL,
			delivered_sequence BIGINT NOT NULL, read_sequence BIGINT NOT NULL, revision BIGINT NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, conversation_id, participant_type, participant_id),
			CHECK (delivered_sequence >= 0), CHECK (read_sequence >= 0), CHECK (read_sequence <= delivered_sequence), CHECK (revision > 0)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("conversation_presence")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, conversation_id TEXT NOT NULL,
			participant_type TEXT NOT NULL, participant_id TEXT NOT NULL, state TEXT NOT NULL,
			lease_id TEXT NOT NULL, revision BIGINT NOT NULL, updated_at TIMESTAMPTZ NOT NULL, expires_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, conversation_id, participant_type, participant_id),
			CHECK (revision > 0), CHECK (expires_at > updated_at)
		)`); err != nil {
		return err
	}
	statements := []string{
		`CREATE INDEX IF NOT EXISTS conversations_owner_idx ON ` + s.table("conversations") + ` (scope_kind, scope_id, owner_type, owner_id, status, updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS channel_messages_sequence_idx ON ` + s.table("channel_messages") + ` (scope_kind, scope_id, conversation_id, sequence)`,
		`CREATE INDEX IF NOT EXISTS channel_messages_thread_idx ON ` + s.table("channel_messages") + ` (scope_kind, scope_id, conversation_id, thread_root_id, sequence)`,
		`CREATE INDEX IF NOT EXISTS channel_messages_round_idx ON ` + s.table("channel_messages") + ` (scope_kind, scope_id, conversation_id, participation_round_id, sequence)`,
		`CREATE INDEX IF NOT EXISTS conversation_presence_active_idx ON ` + s.table("conversation_presence") + ` (scope_kind, scope_id, conversation_id, expires_at)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version, name)
		VALUES (10, 'natural team channels') ON CONFLICT (version) DO NOTHING`)
	return err
}

func (s *PostgresStore) CreateConversation(ctx context.Context, conversation *Conversation, idempotencyKey string) (*Conversation, bool, error) {
	if err := conversation.Validate(); err != nil {
		return nil, false, err
	}
	key := strings.TrimSpace(idempotencyKey)
	if key == "" {
		return nil, false, ErrInvalidConversation
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if err := postgresConversationAdvisoryLock(ctx, tx, conversation.Scope, "conversation:"+key); err != nil {
		return nil, false, err
	}
	existing, err := s.getPostgresConversationByKey(ctx, tx, conversation.Scope, key, true)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		if existing.Owner != conversation.Owner || existing.Title != conversation.Title {
			return nil, false, ErrMessageConflict
		}
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return existing, true, nil
	}
	payload, err := json.Marshal(conversation)
	if err != nil {
		return nil, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("conversations")+`
		(scope_kind, scope_id, id, owner_type, owner_id, status, idempotency_key, last_sequence, revision, created_at, updated_at, payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::jsonb)`, conversation.Scope.Kind, conversation.Scope.ID,
		conversation.ID, conversation.Owner.Type, conversation.Owner.ID, conversation.Status, key, conversation.LastSequence,
		conversation.Revision, conversation.CreatedAt, conversation.UpdatedAt, string(payload))
	if err != nil {
		if postgresUniqueViolation(err) {
			return nil, false, ErrMessageConflict
		}
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return cloneConversation(conversation), false, nil
}

func (s *PostgresStore) GetConversation(ctx context.Context, scope Scope, conversationID string) (*Conversation, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.getPostgresConversation(ctx, s.db, scope, strings.TrimSpace(conversationID), false)
}

func (s *PostgresStore) FindConversationByIdempotencyKey(ctx context.Context, scope Scope, key string) (*Conversation, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, nil
	}
	return s.getPostgresConversationByKey(ctx, s.db, scope, strings.TrimSpace(key), false)
}

func (s *PostgresStore) ListConversations(ctx context.Context, filter ConversationFilter) ([]*Conversation, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT payload FROM ` + s.table("conversations") + ` WHERE scope_kind = $1 AND scope_id = $2`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	placeholder := 3
	if filter.Owner != nil {
		query += fmt.Sprintf(` AND owner_type = $%d AND owner_id = $%d`, placeholder, placeholder+1)
		args = append(args, filter.Owner.Type, filter.Owner.ID)
		placeholder += 2
	}
	if len(filter.Statuses) > 0 {
		statuses := make([]string, len(filter.Statuses))
		for index, status := range filter.Statuses {
			statuses[index] = string(status)
		}
		query += fmt.Sprintf(` AND status = ANY($%d)`, placeholder)
		args = append(args, pq.Array(statuses))
		placeholder++
	}
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	query += fmt.Sprintf(` ORDER BY updated_at DESC, id ASC LIMIT $%d OFFSET $%d`, placeholder, placeholder+1)
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPostgresConversations(rows)
}

func (s *PostgresStore) CommitChannelMessage(ctx context.Context, record ChannelMessageCommitRecord) (*ChannelMessageCommitResult, error) {
	if err := validateChannelMessageCommitRecord(record); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := postgresConversationAdvisoryLock(ctx, tx, record.Message.Scope, "message:"+record.Message.ConversationID+":"+record.Message.IdempotencyKey); err != nil {
		return nil, err
	}
	existing, err := s.getPostgresChannelMessageByKey(ctx, tx, record.Message.Scope, record.Message.ConversationID, record.Message.IdempotencyKey, true)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if !sameChannelMessage(existing, record.Message) {
			return nil, ErrMessageConflict
		}
		conversation, err := s.getPostgresConversation(ctx, tx, record.Message.Scope, record.Message.ConversationID, true)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return &ChannelMessageCommitResult{Conversation: conversation, Message: existing, Replayed: true}, nil
	}
	current, err := s.getPostgresConversation(ctx, tx, record.Conversation.Scope, record.Conversation.ID, true)
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
	if err := s.updatePostgresConversationTx(ctx, tx, record.Conversation, record.ExpectedRevision); err != nil {
		return nil, err
	}
	if err := s.insertPostgresChannelMessageTx(ctx, tx, record.Message); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ChannelMessageCommitResult{Conversation: cloneConversation(record.Conversation), Message: cloneChannelMessage(record.Message)}, nil
}

func (s *PostgresStore) GetChannelMessage(ctx context.Context, scope Scope, conversationID, messageID string) (*ChannelMessage, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.getPostgresChannelMessage(ctx, s.db, scope, conversationID, messageID, false)
}

func (s *PostgresStore) FindChannelMessageByIdempotencyKey(ctx context.Context, scope Scope, conversationID, key string) (*ChannelMessage, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, nil
	}
	return s.getPostgresChannelMessageByKey(ctx, s.db, scope, conversationID, strings.TrimSpace(key), false)
}

func (s *PostgresStore) ListChannelMessages(ctx context.Context, filter ChannelMessageFilter) ([]*ChannelMessage, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	conversation, err := s.getPostgresConversation(ctx, s.db, filter.Scope, filter.ConversationID, false)
	if err != nil {
		return nil, err
	}
	if conversation == nil {
		return nil, ErrConversationNotFound
	}
	query := `SELECT payload FROM ` + s.table("channel_messages") + ` WHERE scope_kind = $1 AND scope_id = $2 AND conversation_id = $3 AND sequence > $4`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID, filter.ConversationID, filter.AfterSequence}
	placeholder := 5
	if filter.ThreadRootID != "" {
		query += fmt.Sprintf(` AND (thread_root_id = $%d OR id = $%d)`, placeholder, placeholder)
		args = append(args, filter.ThreadRootID)
		placeholder++
	}
	if len(filter.Intents) > 0 {
		intents := make([]string, len(filter.Intents))
		for index, intent := range filter.Intents {
			intents[index] = string(intent)
		}
		query += fmt.Sprintf(` AND intent = ANY($%d)`, placeholder)
		args = append(args, pq.Array(intents))
		placeholder++
	}
	if filter.Descending {
		query += ` ORDER BY sequence DESC`
	} else {
		query += ` ORDER BY sequence ASC`
	}
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query += fmt.Sprintf(` LIMIT $%d`, placeholder)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPostgresChannelMessages(rows)
}

func (s *PostgresStore) CommitParticipationRound(ctx context.Context, record ParticipationRoundCommitRecord) (*ParticipationRoundResult, error) {
	if err := validateParticipationRoundCommitRecord(record); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := postgresConversationAdvisoryLock(ctx, tx, record.Round.Scope, "round:"+record.Round.ConversationID+":"+record.Round.IdempotencyKey); err != nil {
		return nil, err
	}
	existing, err := s.getPostgresParticipationRoundByKey(ctx, tx, record.Round.Scope, record.Round.ConversationID, record.Round.IdempotencyKey, true)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if !sameParticipationRound(existing, record.Round) {
			return nil, ErrMessageConflict
		}
		result, err := s.loadPostgresParticipationRoundResult(ctx, tx, existing, true)
		if err != nil {
			return nil, err
		}
		result.Replayed = true
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return result, nil
	}
	current, err := s.getPostgresConversation(ctx, tx, record.Conversation.Scope, record.Conversation.ID, true)
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
	if err := s.updatePostgresConversationTx(ctx, tx, record.Conversation, record.ExpectedRevision); err != nil {
		return nil, err
	}
	for _, message := range record.Messages {
		if err := s.insertPostgresChannelMessageTx(ctx, tx, message); err != nil {
			return nil, err
		}
	}
	if err := s.insertPostgresParticipationRoundTx(ctx, tx, record.Round); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ParticipationRoundResult{Conversation: cloneConversation(record.Conversation), Round: cloneParticipationRound(record.Round), Messages: cloneChannelMessages(record.Messages)}, nil
}

func (s *PostgresStore) FindParticipationRoundByIdempotencyKey(ctx context.Context, scope Scope, conversationID, key string) (*ParticipationRoundResult, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, nil
	}
	round, err := s.getPostgresParticipationRoundByKey(ctx, s.db, scope, conversationID, strings.TrimSpace(key), false)
	if err != nil || round == nil {
		return nil, err
	}
	result, err := s.loadPostgresParticipationRoundResult(ctx, s.db, round, false)
	if result != nil {
		result.Replayed = true
	}
	return result, err
}

func (s *PostgresStore) GetConversationCursor(ctx context.Context, scope Scope, conversationID string, participant ConversationParticipant) (*ConversationCursor, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := participant.Validate(); err != nil {
		return nil, err
	}
	return s.getPostgresConversationCursor(ctx, s.db, scope, conversationID, participant, false)
}

func (s *PostgresStore) PutConversationCursor(ctx context.Context, record ConversationCursorRecord) (*ConversationCursor, bool, error) {
	if record.Cursor == nil {
		return nil, false, ErrConversationCursorConflict
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
		return nil, false, ErrConversationNotFound
	}
	if record.Cursor.DeliveredSequence > conversation.LastSequence {
		return nil, false, ErrConversationCursorConflict
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
			return nil, false, ErrConversationCursorConflict
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("conversation_cursors")+`
			(scope_kind, scope_id, conversation_id, participant_type, participant_id, delivered_sequence, read_sequence, revision, updated_at, payload)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb)`, record.Cursor.Scope.Kind, record.Cursor.Scope.ID,
			record.Cursor.ConversationID, record.Cursor.Participant.Type, record.Cursor.Participant.ID, record.Cursor.DeliveredSequence,
			record.Cursor.ReadSequence, record.Cursor.Revision, record.Cursor.UpdatedAt, string(payload))
	} else {
		if current.Revision != record.ExpectedRevision || record.Cursor.Revision != current.Revision+1 {
			return nil, false, ErrConversationCursorConflict
		}
		var result sql.Result
		result, err = tx.ExecContext(ctx, `UPDATE `+s.table("conversation_cursors")+` SET delivered_sequence=$1, read_sequence=$2,
			revision=$3, updated_at=$4, payload=$5::jsonb WHERE scope_kind=$6 AND scope_id=$7 AND conversation_id=$8
			AND participant_type=$9 AND participant_id=$10 AND revision=$11`, record.Cursor.DeliveredSequence, record.Cursor.ReadSequence,
			record.Cursor.Revision, record.Cursor.UpdatedAt, string(payload), record.Cursor.Scope.Kind, record.Cursor.Scope.ID,
			record.Cursor.ConversationID, record.Cursor.Participant.Type, record.Cursor.Participant.ID, record.ExpectedRevision)
		if err == nil {
			err = expectPostgresDependencyRow(result, nil)
		}
	}
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return cloneConversationCursor(record.Cursor), false, nil
}

func (s *PostgresStore) GetConversationPresence(ctx context.Context, scope Scope, conversationID string, participant ConversationParticipant) (*ConversationPresence, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := participant.Validate(); err != nil {
		return nil, err
	}
	return s.getPostgresConversationPresence(ctx, s.db, scope, conversationID, participant, false)
}

func (s *PostgresStore) PutConversationPresence(ctx context.Context, record ConversationPresenceRecord) (*ConversationPresence, error) {
	if record.Presence == nil {
		return nil, ErrConversationPresenceConflict
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
		return nil, ErrConversationNotFound
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
			return nil, ErrConversationPresenceConflict
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
		if err := expectPostgresDependencyRow(result, nil); err != nil {
			return nil, err
		}
	} else {
		if record.ExpectedRevision != 0 || record.Presence.Revision != 1 {
			return nil, ErrConversationPresenceConflict
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
	return cloneConversationPresence(record.Presence), nil
}

func (s *PostgresStore) ReleaseConversationPresence(ctx context.Context, request ReleaseConversationPresenceRequest) error {
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
			return ErrConversationPresenceConflict
		}
	}
	return nil
}

func (s *PostgresStore) ListConversationPresence(ctx context.Context, scope Scope, conversationID string, activeAt time.Time) ([]*ConversationPresence, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("conversation_presence")+` WHERE scope_kind=$1 AND scope_id=$2
		AND conversation_id=$3 AND expires_at > $4 ORDER BY participant_type, participant_id`, scope.Kind, scope.ID, conversationID, activeAt)
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

type postgresConversationQueryer interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
}

func postgresConversationAdvisoryLock(ctx context.Context, tx *sql.Tx, scope Scope, key string) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "openseal:conversation:"+scope.key()+":"+key)
	return err
}

func postgresUniqueViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}

func (s *PostgresStore) getPostgresConversation(ctx context.Context, queryer postgresConversationQueryer, scope Scope, id string, lock bool) (*Conversation, error) {
	query := `SELECT payload FROM ` + s.table("conversations") + ` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3`
	if lock {
		query += ` FOR UPDATE`
	}
	var payload string
	err := queryer.QueryRowContext(ctx, query, scope.Kind, scope.ID, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeConversation(payload)
}

func (s *PostgresStore) getPostgresConversationByKey(ctx context.Context, queryer postgresConversationQueryer, scope Scope, key string, lock bool) (*Conversation, error) {
	query := `SELECT payload FROM ` + s.table("conversations") + ` WHERE scope_kind=$1 AND scope_id=$2 AND idempotency_key=$3`
	if lock {
		query += ` FOR UPDATE`
	}
	var payload string
	err := queryer.QueryRowContext(ctx, query, scope.Kind, scope.ID, key).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeConversation(payload)
}

func (s *PostgresStore) updatePostgresConversationTx(ctx context.Context, tx *sql.Tx, conversation *Conversation, expectedRevision int64) error {
	payload, err := json.Marshal(conversation)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("conversations")+` SET status=$1, last_sequence=$2, revision=$3,
		updated_at=$4, payload=$5::jsonb WHERE scope_kind=$6 AND scope_id=$7 AND id=$8 AND revision=$9`, conversation.Status,
		conversation.LastSequence, conversation.Revision, conversation.UpdatedAt, string(payload), conversation.Scope.Kind,
		conversation.Scope.ID, conversation.ID, expectedRevision)
	return expectPostgresDependencyRow(result, err)
}

func (s *PostgresStore) insertPostgresChannelMessageTx(ctx context.Context, tx *sql.Tx, message *ChannelMessage) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("channel_messages")+`
		(scope_kind, scope_id, conversation_id, id, sequence, sender_type, sender_id, intent, thread_root_id, participation_round_id, idempotency_key, created_at, payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13::jsonb)`, message.Scope.Kind, message.Scope.ID,
		message.ConversationID, message.ID, message.Sequence, message.Sender.Type, message.Sender.ID, message.Intent,
		message.ThreadRootID, message.ParticipationRoundID, message.IdempotencyKey, message.CreatedAt, string(payload))
	if postgresUniqueViolation(err) {
		return ErrMessageConflict
	}
	return err
}

func (s *PostgresStore) getPostgresChannelMessage(ctx context.Context, queryer postgresConversationQueryer, scope Scope, conversationID, id string, lock bool) (*ChannelMessage, error) {
	query := `SELECT payload FROM ` + s.table("channel_messages") + ` WHERE scope_kind=$1 AND scope_id=$2 AND conversation_id=$3 AND id=$4`
	if lock {
		query += ` FOR UPDATE`
	}
	var payload string
	err := queryer.QueryRowContext(ctx, query, scope.Kind, scope.ID, conversationID, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeChannelMessage(payload)
}

func (s *PostgresStore) getPostgresChannelMessageByKey(ctx context.Context, queryer postgresConversationQueryer, scope Scope, conversationID, key string, lock bool) (*ChannelMessage, error) {
	query := `SELECT payload FROM ` + s.table("channel_messages") + ` WHERE scope_kind=$1 AND scope_id=$2 AND conversation_id=$3 AND idempotency_key=$4`
	if lock {
		query += ` FOR UPDATE`
	}
	var payload string
	err := queryer.QueryRowContext(ctx, query, scope.Kind, scope.ID, conversationID, key).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeChannelMessage(payload)
}

func (s *PostgresStore) insertPostgresParticipationRoundTx(ctx context.Context, tx *sql.Tx, round *ParticipationRound) error {
	payload, err := json.Marshal(round)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("participation_rounds")+`
		(scope_kind, scope_id, conversation_id, id, status, idempotency_key, revision, created_at, committed_at, payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb)`, round.Scope.Kind, round.Scope.ID, round.ConversationID,
		round.ID, round.Status, round.IdempotencyKey, round.Revision, round.CreatedAt, round.CommittedAt, string(payload))
	if postgresUniqueViolation(err) {
		return ErrMessageConflict
	}
	return err
}

func (s *PostgresStore) getPostgresParticipationRoundByKey(ctx context.Context, queryer postgresConversationQueryer, scope Scope, conversationID, key string, lock bool) (*ParticipationRound, error) {
	query := `SELECT payload FROM ` + s.table("participation_rounds") + ` WHERE scope_kind=$1 AND scope_id=$2 AND conversation_id=$3 AND idempotency_key=$4`
	if lock {
		query += ` FOR UPDATE`
	}
	var payload string
	err := queryer.QueryRowContext(ctx, query, scope.Kind, scope.ID, conversationID, key).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeParticipationRound(payload)
}

func (s *PostgresStore) loadPostgresParticipationRoundResult(ctx context.Context, queryer postgresConversationQueryer, round *ParticipationRound, lock bool) (*ParticipationRoundResult, error) {
	conversation, err := s.getPostgresConversation(ctx, queryer, round.Scope, round.ConversationID, lock)
	if err != nil {
		return nil, err
	}
	query := `SELECT payload FROM ` + s.table("channel_messages") + ` WHERE scope_kind=$1 AND scope_id=$2 AND conversation_id=$3 AND participation_round_id=$4 ORDER BY sequence`
	if lock {
		query += ` FOR UPDATE`
	}
	rows, err := queryer.QueryContext(ctx, query, round.Scope.Kind, round.Scope.ID, round.ConversationID, round.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages, err := scanPostgresChannelMessages(rows)
	return &ParticipationRoundResult{Conversation: conversation, Round: round, Messages: messages}, err
}

func (s *PostgresStore) getPostgresConversationCursor(ctx context.Context, queryer postgresConversationQueryer, scope Scope, conversationID string, participant ConversationParticipant, lock bool) (*ConversationCursor, error) {
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
	return decodeConversationCursor(payload)
}

func (s *PostgresStore) getPostgresConversationPresence(ctx context.Context, queryer postgresConversationQueryer, scope Scope, conversationID string, participant ConversationParticipant, lock bool) (*ConversationPresence, error) {
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
	return decodeConversationPresence(payload)
}

func scanPostgresConversations(rows *sql.Rows) ([]*Conversation, error) {
	result := make([]*Conversation, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		value, err := decodeConversation(payload)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func scanPostgresChannelMessages(rows *sql.Rows) ([]*ChannelMessage, error) {
	result := make([]*ChannelMessage, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		value, err := decodeChannelMessage(payload)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

var _ ConversationStore = (*PostgresStore)(nil)
