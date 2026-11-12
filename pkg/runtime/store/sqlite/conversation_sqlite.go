package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
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
		CREATE TABLE IF NOT EXISTS surface_interactions (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, conversation_id TEXT NOT NULL, id TEXT NOT NULL,
			sequence INTEGER NOT NULL, surface_artifact_id TEXT NOT NULL, surface_id TEXT NOT NULL, surface_version INTEGER NOT NULL,
			message_id TEXT NOT NULL, component_id TEXT NOT NULL, item_key TEXT NOT NULL DEFAULT '', actor_type TEXT NOT NULL, actor_id TEXT NOT NULL,
			repeatable INTEGER NOT NULL, idempotency_key TEXT NOT NULL, created_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, conversation_id, id),
			UNIQUE (scope_kind, scope_id, conversation_id, sequence),
			UNIQUE (scope_kind, scope_id, conversation_id, idempotency_key)
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_surface_interactions_single_use
			ON surface_interactions(scope_kind, scope_id, conversation_id, surface_id, surface_version, component_id, actor_type, actor_id)
			WHERE repeatable = 0;
		CREATE INDEX IF NOT EXISTS idx_surface_interactions_surface
			ON surface_interactions(scope_kind, scope_id, conversation_id, surface_id, sequence);
	`)
	return err
}

func (s *Store) CreateConversation(ctx context.Context, conversation *models.Conversation, idempotencyKey string) (*models.Conversation, bool, error) {
	if err := conversation.Validate(); err != nil {
		return nil, false, err
	}
	models.ApplyConversationDefaults(conversation)
	key := strings.TrimSpace(idempotencyKey)
	if key == "" {
		return nil, false, models.ErrInvalidConversation
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
		if existing.Owner != conversation.Owner || existing.Title != conversation.Title ||
			!reflect.DeepEqual(existing.Origin, conversation.Origin) || !models.SameRequestedSpace(existing, conversation.SpaceID) {
			return nil, false, models.ErrMessageConflict
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, false, err
		}
		committed = true
		return existing, true, nil
	}
	conversation = models.CloneConversation(conversation)
	if err := resolveSQLiteConversationSpace(ctx, conn, conversation); err != nil {
		return nil, false, err
	}
	payload, err := json.Marshal(conversation)
	if err != nil {
		return nil, false, err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO conversations
		(scope_kind, scope_id, id, owner_type, owner_id, status, idempotency_key, last_sequence, revision, created_at, updated_at, payload, space_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, conversation.Scope.Kind, conversation.Scope.ID, conversation.ID,
		conversation.Owner.Type, conversation.Owner.ID, conversation.Status, key, conversation.LastSequence, conversation.Revision,
		conversation.CreatedAt, conversation.UpdatedAt, string(payload), conversation.SpaceID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, false, models.ErrMessageConflict
		}
		return nil, false, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, false, err
	}
	committed = true
	return models.CloneConversation(conversation), false, nil
}

func (s *Store) UpdateConversation(ctx context.Context, conversation *models.Conversation, expectedRevision int64) (*models.Conversation, error) {
	if err := conversation.Validate(); err != nil {
		return nil, err
	}
	models.ApplyConversationDefaults(conversation)
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	if err := updateSQLiteConversation(ctx, conn, conversation, expectedRevision); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return models.CloneConversation(conversation), nil
}

func (s *Store) UpdateConversationWithEvent(ctx context.Context, conversation *models.Conversation, expectedRevision int64, event *models.ActivityEvent) (*models.Conversation, *models.ActivityEvent, error) {
	if err := models.ValidateConversationEvent(conversation, event); err != nil {
		return nil, nil, err
	}
	models.ApplyConversationDefaults(conversation)
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	if err := updateSQLiteConversation(ctx, conn, conversation, expectedRevision); err != nil {
		return nil, nil, err
	}
	persisted, err := insertSQLiteActivityConn(ctx, conn, event)
	if err != nil {
		return nil, nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, nil, err
	}
	committed = true
	return models.CloneConversation(conversation), models.CloneActivityEvent(persisted), nil
}

func (s *Store) GetConversation(ctx context.Context, scope models.Scope, conversationID string) (*models.Conversation, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return getSQLiteConversation(ctx, s.db, scope, strings.TrimSpace(conversationID))
}

func (s *Store) FindConversationByIdempotencyKey(ctx context.Context, scope models.Scope, key string) (*models.Conversation, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, nil
	}
	return getSQLiteConversationByKey(ctx, s.db, scope, strings.TrimSpace(key))
}

func (s *Store) ListConversations(ctx context.Context, filter models.ConversationFilter) ([]*models.Conversation, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT payload FROM conversations WHERE scope_kind = ? AND scope_id = ?`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	if filter.Owner != nil {
		query += ` AND owner_type = ? AND owner_id = ?`
		args = append(args, filter.Owner.Type, filter.Owner.ID)
	}
	if filter.SpaceID != "" {
		query += ` AND space_id = ?`
		args = append(args, filter.SpaceID)
	}
	if filter.SpaceIDs != nil && len(filter.SpaceIDs) == 0 {
		query += ` AND 0`
	}
	query, args = appendSQLiteActivityStrings(query, args, "space_id", filter.SpaceIDs)
	statuses := make([]string, 0, len(filter.Statuses))
	for _, status := range filter.Statuses {
		statuses = append(statuses, string(status))
	}
	query, args = appendSQLiteActivityStrings(query, args, "status", statuses)
	if filter.OriginKind != "" {
		query += ` AND json_extract(payload, '$.origin.kind') = ?`
		args = append(args, string(filter.OriginKind))
	}
	if filter.AgentBuilderDraftOf != "" {
		query += ` AND builder_draft_requester = ?`
		args = append(args, filter.AgentBuilderDraftOf)
	}
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
	result := make([]*models.Conversation, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		conversation, err := sqlstore.DecodeConversation(payload)
		if err != nil {
			return nil, err
		}
		result = append(result, conversation)
	}
	return result, rows.Err()
}

func (s *Store) LastConversationActivity(ctx context.Context, scope models.Scope, ownerType models.OwnerType, ownerIDs []string) (map[string]time.Time, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	result := make(map[string]time.Time)
	if len(ownerIDs) == 0 {
		return result, nil
	}
	// With MAX, SQLite takes payload from the row that holds the maximum.
	query, args := appendSQLiteActivityStrings(`SELECT owner_id, MAX(updated_at), payload FROM conversations
		WHERE scope_kind = ? AND scope_id = ? AND owner_type = ? AND status = ? AND last_sequence > 0`,
		[]interface{}{scope.Kind, scope.ID, ownerType, models.ConversationStatusActive}, "owner_id", ownerIDs)
	rows, err := s.db.QueryContext(ctx, query+` GROUP BY owner_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ownerID, payload string
		var newest interface{}
		if err := rows.Scan(&ownerID, &newest, &payload); err != nil {
			return nil, err
		}
		conversation, err := sqlstore.DecodeConversation(payload)
		if err != nil {
			return nil, err
		}
		result[ownerID] = conversation.UpdatedAt
	}
	return result, rows.Err()
}

func (s *Store) CommitChannelMessage(ctx context.Context, record models.ChannelMessageCommitRecord) (*models.ChannelMessageCommitResult, error) {
	if err := models.ValidateChannelMessageCommitRecord(record); err != nil {
		return nil, err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	// BEGIN IMMEDIATE holds the write lock, so the Run read here stays
	// current until COMMIT (Run before conversation, SPEC §4.1).
	var guardedRun *models.AgentRun
	if record.RunGuard != nil {
		if guardedRun, err = getSQLiteAgentRun(ctx, conn, record.RunGuard.Scope, record.RunGuard.RunID); err != nil {
			return nil, err
		}
	}
	existing, err := getSQLiteChannelMessageByKey(ctx, conn, record.Message.Scope, record.Message.ConversationID, record.Message.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if !models.SameChannelMessage(existing, record.Message) {
			return nil, models.ErrMessageConflict
		}
		conversation, err := getSQLiteConversation(ctx, conn, record.Message.Scope, record.Message.ConversationID)
		if err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, err
		}
		committed = true
		return &models.ChannelMessageCommitResult{Conversation: conversation, Message: existing, Replayed: true}, nil
	}
	if guardedRun != nil {
		if err := models.CheckReplyRunGuard(guardedRun, record.RunGuard, time.Now().UTC()); err != nil {
			return nil, err
		}
	}
	current, err := getSQLiteConversation(ctx, conn, record.Conversation.Scope, record.Conversation.ID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, models.ErrConversationNotFound
	}
	if current.Revision != record.ExpectedRevision || record.Conversation.Revision != current.Revision+1 ||
		record.Message.Sequence != current.LastSequence+1 || record.Conversation.LastSequence != record.Message.Sequence {
		return nil, models.ErrRevisionConflict
	}
	if err := updateSQLiteConversation(ctx, conn, record.Conversation, record.ExpectedRevision); err != nil {
		return nil, err
	}
	if err := insertSQLiteChannelMessage(ctx, conn, record.Message); err != nil {
		return nil, err
	}
	if requester := models.AgentBuilderDraftStarterID(current, record.Message); requester != "" {
		if _, err := conn.ExecContext(ctx, `UPDATE conversations SET builder_draft_requester = ? WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
			requester, current.Scope.Kind, current.Scope.ID, current.ID); err != nil {
			return nil, err
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return &models.ChannelMessageCommitResult{Conversation: models.CloneConversation(record.Conversation), Message: models.CloneChannelMessage(record.Message)}, nil
}

func (s *Store) GetChannelMessage(ctx context.Context, scope models.Scope, conversationID, messageID string) (*models.ChannelMessage, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return getSQLiteChannelMessage(ctx, s.db, scope, conversationID, messageID)
}

func (s *Store) FindChannelMessageByIdempotencyKey(ctx context.Context, scope models.Scope, conversationID, key string) (*models.ChannelMessage, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, nil
	}
	return getSQLiteChannelMessageByKey(ctx, s.db, scope, conversationID, strings.TrimSpace(key))
}

func (s *Store) ListChannelMessages(ctx context.Context, filter models.ChannelMessageFilter) ([]*models.ChannelMessage, error) {
	if err := models.ValidateChannelMessageViewFilter(filter); err != nil {
		return nil, err
	}
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	conversation, err := getSQLiteConversation(ctx, s.db, filter.Scope, filter.ConversationID)
	if err != nil {
		return nil, err
	}
	if conversation == nil {
		return nil, models.ErrConversationNotFound
	}
	query := `SELECT payload FROM channel_messages WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND sequence > ?`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID, filter.ConversationID, filter.AfterSequence}
	if filter.BeforeSequence > 0 {
		query += ` AND sequence < ?`
		args = append(args, filter.BeforeSequence)
	}
	if filter.ThreadRootID != "" {
		query += ` AND (thread_root_id = ? OR id = ?)`
		args = append(args, filter.ThreadRootID, filter.ThreadRootID)
	}
	if filter.ChannelTimeline {
		query += ` AND (thread_root_id = '' OR json_extract(payload, '$.broadcastToChannel') = 1)`
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
	result := make([]*models.ChannelMessage, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		message, err := sqlstore.DecodeChannelMessage(payload)
		if err != nil {
			return nil, err
		}
		result = append(result, message)
	}
	return result, rows.Err()
}

func (s *Store) CommitParticipationRound(ctx context.Context, record models.ParticipationRoundCommitRecord) (*models.ParticipationRoundResult, error) {
	if err := models.ValidateParticipationRoundCommitRecord(record); err != nil {
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
		if !models.SameParticipationRound(existing, record.Round) {
			return nil, models.ErrMessageConflict
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
		return nil, models.ErrConversationNotFound
	}
	if current.Revision != record.ExpectedRevision || record.Conversation.Revision != current.Revision+1 {
		return nil, models.ErrRevisionConflict
	}
	expectedSequence := current.LastSequence
	for _, message := range record.Messages {
		expectedSequence++
		if message.Sequence != expectedSequence || message.ParticipationRoundID != record.Round.ID {
			return nil, models.ErrInvalidConversation
		}
	}
	if record.Conversation.LastSequence != expectedSequence {
		return nil, models.ErrInvalidConversation
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
	return &models.ParticipationRoundResult{Conversation: models.CloneConversation(record.Conversation), Round: models.CloneParticipationRound(record.Round), Messages: models.CloneChannelMessages(record.Messages)}, nil
}

func (s *Store) FindParticipationRoundByIdempotencyKey(ctx context.Context, scope models.Scope, conversationID, key string) (*models.ParticipationRoundResult, error) {
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

func (s *Store) GetParticipationRound(ctx context.Context, scope models.Scope, conversationID, roundID string) (*models.ParticipationRoundResult, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	round, err := getSQLiteParticipationRound(ctx, s.db, scope, conversationID, roundID)
	if err != nil || round == nil {
		return nil, err
	}
	return loadSQLiteParticipationRoundResult(ctx, s.db, round)
}

func (s *Store) ListParticipationRounds(ctx context.Context, filter models.ParticipationRoundFilter) ([]*models.ParticipationRoundResult, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM participation_rounds WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ?
		ORDER BY committed_at DESC, id ASC LIMIT ? OFFSET ?`, filter.Scope.Kind, filter.Scope.ID, filter.ConversationID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rounds := make([]*models.ParticipationRound, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		round, err := sqlstore.DecodeParticipationRound(payload)
		if err != nil {
			return nil, err
		}
		rounds = append(rounds, round)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]*models.ParticipationRoundResult, 0, len(rounds))
	for _, round := range rounds {
		loaded, err := loadSQLiteParticipationRoundResult(ctx, s.db, round)
		if err != nil {
			return nil, err
		}
		result = append(result, loaded)
	}
	return result, nil
}

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

func getSQLiteConversation(ctx context.Context, queryer sqlstore.Reader, scope models.Scope, id string) (*models.Conversation, error) {
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM conversations WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sqlstore.DecodeConversation(payload)
}

func getSQLiteConversationByKey(ctx context.Context, queryer sqlstore.Reader, scope models.Scope, key string) (*models.Conversation, error) {
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM conversations WHERE scope_kind = ? AND scope_id = ? AND idempotency_key = ?`, scope.Kind, scope.ID, key).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sqlstore.DecodeConversation(payload)
}

func updateSQLiteConversation(ctx context.Context, conn *sql.Conn, conversation *models.Conversation, expectedRevision int64) error {
	payload, err := json.Marshal(conversation)
	if err != nil {
		return err
	}
	// space_id is authoritative; see updatePostgresConversationTx.
	result, err := conn.ExecContext(ctx, `UPDATE conversations SET status = ?, last_sequence = ?, revision = ?, updated_at = ?,
		payload = json_set(?, '$.spaceId', space_id)
		WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ?`, conversation.Status, conversation.LastSequence,
		conversation.Revision, conversation.UpdatedAt, string(payload), conversation.Scope.Kind, conversation.Scope.ID, conversation.ID, expectedRevision)
	return sqlstore.ExpectRevisionRow(result, err)
}

func insertSQLiteChannelMessage(ctx context.Context, conn *sql.Conn, message *models.ChannelMessage) error {
	payload, err := json.Marshal(models.PersistedChannelMessage(message))
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO channel_messages
		(scope_kind, scope_id, conversation_id, id, sequence, sender_type, sender_id, intent, thread_root_id, participation_round_id, idempotency_key, created_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, message.Scope.Kind, message.Scope.ID, message.ConversationID, message.ID,
		message.Sequence, message.Sender.Type, message.Sender.ID, message.Intent, message.ThreadRootID, message.ParticipationRoundID,
		message.IdempotencyKey, message.CreatedAt, string(payload))
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
		return models.ErrMessageConflict
	}
	return err
}

func getSQLiteChannelMessage(ctx context.Context, queryer sqlstore.Reader, scope models.Scope, conversationID, id string) (*models.ChannelMessage, error) {
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM channel_messages WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND id = ?`,
		scope.Kind, scope.ID, conversationID, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sqlstore.DecodeChannelMessage(payload)
}

func getSQLiteChannelMessageByKey(ctx context.Context, queryer sqlstore.Reader, scope models.Scope, conversationID, key string) (*models.ChannelMessage, error) {
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM channel_messages WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND idempotency_key = ?`,
		scope.Kind, scope.ID, conversationID, key).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sqlstore.DecodeChannelMessage(payload)
}

func insertSQLiteParticipationRound(ctx context.Context, conn *sql.Conn, round *models.ParticipationRound) error {
	payload, err := json.Marshal(round)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO participation_rounds
		(scope_kind, scope_id, conversation_id, id, status, idempotency_key, revision, created_at, committed_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, round.Scope.Kind, round.Scope.ID, round.ConversationID, round.ID, round.Status,
		round.IdempotencyKey, round.Revision, round.CreatedAt, round.CommittedAt, string(payload))
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unique") {
		return models.ErrMessageConflict
	}
	return err
}

func getSQLiteParticipationRoundByKey(ctx context.Context, queryer sqlstore.Reader, scope models.Scope, conversationID, key string) (*models.ParticipationRound, error) {
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM participation_rounds WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND idempotency_key = ?`,
		scope.Kind, scope.ID, conversationID, key).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sqlstore.DecodeParticipationRound(payload)
}

func getSQLiteParticipationRound(ctx context.Context, queryer sqlstore.Reader, scope models.Scope, conversationID, id string) (*models.ParticipationRound, error) {
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM participation_rounds WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND id = ?`,
		scope.Kind, scope.ID, conversationID, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sqlstore.DecodeParticipationRound(payload)
}

func loadSQLiteParticipationRoundResult(ctx context.Context, queryer sqlstore.Reader, round *models.ParticipationRound) (*models.ParticipationRoundResult, error) {
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
	messages := make([]*models.ChannelMessage, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		message, err := sqlstore.DecodeChannelMessage(payload)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return &models.ParticipationRoundResult{Conversation: conversation, Round: round, Messages: messages}, rows.Err()
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

var _ models.ConversationStore = (*Store)(nil)
