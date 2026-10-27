package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const surfaceInteractionMigrationVersion int64 = 65

func surfaceInteractionStoreKey(scope Scope, conversationID string) string {
	return conversationStoreKey(scope, conversationID)
}

func (s *MemoryStore) CreateSurfaceInteraction(_ context.Context, interaction *SurfaceInteraction) (*SurfaceInteraction, bool, error) {
	if err := interaction.Validate(); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conversations[conversationStoreKey(interaction.Scope, interaction.ConversationID)] == nil {
		return nil, false, ErrConversationNotFound
	}
	key := surfaceInteractionStoreKey(interaction.Scope, interaction.ConversationID)
	stored := s.surfaceInteractions[key]
	for _, existing := range stored {
		if existing.IdempotencyKey == interaction.IdempotencyKey {
			if !sameSurfaceInteractionTarget(existing, interaction) {
				return nil, false, &SurfaceInteractionConflictError{Existing: cloneSurfaceInteraction(existing)}
			}
			return cloneSurfaceInteraction(existing), true, nil
		}
	}
	if !interaction.Repeatable {
		for _, existing := range stored {
			if !existing.Repeatable && existing.SurfaceID == interaction.SurfaceID && existing.SurfaceVersion == interaction.SurfaceVersion &&
				existing.ComponentID == interaction.ComponentID && existing.ActorType == interaction.ActorType && existing.ActorID == interaction.ActorID {
				return nil, false, &SurfaceInteractionConflictError{Existing: cloneSurfaceInteraction(existing)}
			}
		}
	}
	for _, existing := range stored {
		if existing.ID == interaction.ID {
			return nil, false, ErrSurfaceInteractionConflict
		}
	}
	created := cloneSurfaceInteraction(interaction)
	created.Sequence = int64(len(stored)) + 1
	s.surfaceInteractions[key] = append(stored, created)
	return cloneSurfaceInteraction(created), false, nil
}

func (s *MemoryStore) ListSurfaceInteractions(_ context.Context, filter SurfaceInteractionFilter) ([]*SurfaceInteraction, error) {
	if err := filter.validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.conversations[conversationStoreKey(filter.Scope, filter.ConversationID)] == nil {
		return nil, ErrConversationNotFound
	}
	result := make([]*SurfaceInteraction, 0)
	for _, interaction := range s.surfaceInteractions[surfaceInteractionStoreKey(filter.Scope, filter.ConversationID)] {
		if len(result) == filter.limit() {
			break
		}
		if interaction.Sequence > filter.AfterSequence && (filter.SurfaceID == "" || interaction.SurfaceID == filter.SurfaceID) {
			result = append(result, cloneSurfaceInteraction(interaction))
		}
	}
	return result, nil
}

func (s *MemoryStore) ListLatestSurfaceInteractions(_ context.Context, filter LatestSurfaceInteractionFilter) ([]*SurfaceInteraction, error) {
	if err := filter.validate(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.conversations[conversationStoreKey(filter.Scope, filter.ConversationID)] == nil {
		return nil, ErrConversationNotFound
	}
	surface := make([]*SurfaceInteraction, 0)
	for _, interaction := range s.surfaceInteractions[surfaceInteractionStoreKey(filter.Scope, filter.ConversationID)] {
		if interaction.SurfaceID == filter.SurfaceID {
			surface = append(surface, interaction)
		}
	}
	return cloneSurfaceInteractions(latestSurfaceInteractions(surface, filter.AcrossActors)), nil
}

// latestSurfaceInteractionQuery keeps the last write per item; table is the
// (qualified) surface_interactions table and placeholders are the four
// scope/conversation/surface parameters in order.
func latestSurfaceInteractionQuery(table string, acrossActors bool, placeholders [4]string) string {
	partition := "component_id, item_key"
	if !acrossActors {
		partition += ", actor_type, actor_id"
	}
	return `SELECT payload FROM (
		SELECT payload, sequence, ROW_NUMBER() OVER (PARTITION BY ` + partition + ` ORDER BY sequence DESC) AS position
		FROM ` + table + ` WHERE scope_kind = ` + placeholders[0] + ` AND scope_id = ` + placeholders[1] + `
			AND conversation_id = ` + placeholders[2] + ` AND surface_id = ` + placeholders[3] + `
	) latest WHERE position = 1 ORDER BY sequence ASC`
}

func scanSurfaceInteractions(rows *sql.Rows) ([]*SurfaceInteraction, error) {
	defer rows.Close()
	result := make([]*SurfaceInteraction, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		interaction, err := decodeSurfaceInteraction(payload)
		if err != nil {
			return nil, err
		}
		result = append(result, interaction)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) CreateSurfaceInteraction(ctx context.Context, interaction *SurfaceInteraction) (*SurfaceInteraction, bool, error) {
	if err := interaction.Validate(); err != nil {
		return nil, false, err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, false, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	commit := func() error {
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}
		committed = true
		return nil
	}
	conversation, err := getSQLiteConversation(ctx, conn, interaction.Scope, interaction.ConversationID)
	if err != nil {
		return nil, false, err
	}
	if conversation == nil {
		return nil, false, ErrConversationNotFound
	}
	scope := []interface{}{interaction.Scope.Kind, interaction.Scope.ID, interaction.ConversationID}
	existing, err := getSQLiteSurfaceInteraction(ctx, conn, `idempotency_key = ?`, append(scope, interaction.IdempotencyKey)...)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		if !sameSurfaceInteractionTarget(existing, interaction) {
			return nil, false, &SurfaceInteractionConflictError{Existing: existing}
		}
		return existing, true, commit()
	}
	if !interaction.Repeatable {
		existing, err := getSQLiteSurfaceInteraction(ctx, conn, `repeatable = 0 AND surface_id = ? AND surface_version = ? AND component_id = ? AND actor_type = ? AND actor_id = ?`,
			append(scope, interaction.SurfaceID, interaction.SurfaceVersion, interaction.ComponentID, interaction.ActorType, interaction.ActorID)...)
		if err != nil {
			return nil, false, err
		}
		if existing != nil {
			return nil, false, &SurfaceInteractionConflictError{Existing: existing}
		}
	}
	created := cloneSurfaceInteraction(interaction)
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM surface_interactions
		WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ?`, scope...).Scan(&created.Sequence); err != nil {
		return nil, false, err
	}
	payload, err := json.Marshal(created)
	if err != nil {
		return nil, false, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO surface_interactions
		(scope_kind, scope_id, conversation_id, id, sequence, surface_artifact_id, surface_id, surface_version, message_id, component_id,
		 item_key, actor_type, actor_id, repeatable, idempotency_key, created_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, created.Scope.Kind, created.Scope.ID, created.ConversationID,
		created.ID, created.Sequence, created.SurfaceArtifactID, created.SurfaceID, created.SurfaceVersion, created.MessageID, created.ComponentID,
		created.ItemKey, created.ActorType, created.ActorID, created.Repeatable, created.IdempotencyKey, created.CreatedAt, string(payload)); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, false, ErrSurfaceInteractionConflict
		}
		return nil, false, err
	}
	if err := commit(); err != nil {
		return nil, false, err
	}
	return cloneSurfaceInteraction(created), false, nil
}

func (s *SQLiteStore) ListSurfaceInteractions(ctx context.Context, filter SurfaceInteractionFilter) ([]*SurfaceInteraction, error) {
	if err := filter.validate(); err != nil {
		return nil, err
	}
	if conversation, err := getSQLiteConversation(ctx, s.db, filter.Scope, filter.ConversationID); err != nil {
		return nil, err
	} else if conversation == nil {
		return nil, ErrConversationNotFound
	}
	query := `SELECT payload FROM surface_interactions WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND sequence > ?`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID, filter.ConversationID, filter.AfterSequence}
	if filter.SurfaceID != "" {
		query += ` AND surface_id = ?`
		args = append(args, filter.SurfaceID)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY sequence ASC LIMIT ?`, append(args, filter.limit())...)
	if err != nil {
		return nil, err
	}
	return scanSurfaceInteractions(rows)
}

func (s *SQLiteStore) ListLatestSurfaceInteractions(ctx context.Context, filter LatestSurfaceInteractionFilter) ([]*SurfaceInteraction, error) {
	if err := filter.validate(); err != nil {
		return nil, err
	}
	if conversation, err := getSQLiteConversation(ctx, s.db, filter.Scope, filter.ConversationID); err != nil {
		return nil, err
	} else if conversation == nil {
		return nil, ErrConversationNotFound
	}
	rows, err := s.db.QueryContext(ctx, latestSurfaceInteractionQuery("surface_interactions", filter.AcrossActors, [4]string{"?", "?", "?", "?"}),
		filter.Scope.Kind, filter.Scope.ID, filter.ConversationID, filter.SurfaceID)
	if err != nil {
		return nil, err
	}
	return scanSurfaceInteractions(rows)
}

// getSQLiteSurfaceInteraction reads one interaction of a conversation; where
// filters after the leading scope and conversation parameters.
func getSQLiteSurfaceInteraction(ctx context.Context, queryer sqliteConversationQueryer, where string, args ...interface{}) (*SurfaceInteraction, error) {
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM surface_interactions
		WHERE scope_kind = ? AND scope_id = ? AND conversation_id = ? AND `+where+` ORDER BY sequence LIMIT 1`, args...).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeSurfaceInteraction(payload)
}

func (s *PostgresStore) migrateSurfaceInteractions(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, surfaceInteractionMigrationVersion)
	if err != nil || applied {
		return err
	}
	table := s.table("surface_interactions")
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS ` + table + ` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, conversation_id TEXT NOT NULL, id TEXT NOT NULL,
			sequence BIGINT NOT NULL, surface_artifact_id TEXT NOT NULL, surface_id TEXT NOT NULL, surface_version BIGINT NOT NULL,
			message_id TEXT NOT NULL, component_id TEXT NOT NULL, item_key TEXT NOT NULL DEFAULT '', actor_type TEXT NOT NULL, actor_id TEXT NOT NULL,
			repeatable BOOLEAN NOT NULL, idempotency_key TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, conversation_id, id),
			UNIQUE (scope_kind, scope_id, conversation_id, sequence),
			UNIQUE (scope_kind, scope_id, conversation_id, idempotency_key),
			CHECK (sequence > 0), CHECK (surface_version > 0)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS surface_interactions_single_use_idx ON ` + table +
			` (scope_kind, scope_id, conversation_id, surface_id, surface_version, component_id, actor_type, actor_id) WHERE NOT repeatable`,
		`CREATE INDEX IF NOT EXISTS surface_interactions_surface_idx ON ` + table + ` (scope_kind, scope_id, conversation_id, surface_id, sequence)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES($1,'durable surface interactions') ON CONFLICT(version) DO NOTHING`, surfaceInteractionMigrationVersion)
	return err
}

func (s *PostgresStore) CreateSurfaceInteraction(ctx context.Context, interaction *SurfaceInteraction) (*SurfaceInteraction, bool, error) {
	if err := interaction.Validate(); err != nil {
		return nil, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	// One lock per conversation serializes idempotency, single-use checks and
	// Sequence assignment, so a reader never sees a later Sequence commit
	// before an earlier one.
	if err := postgresConversationAdvisoryLock(ctx, tx, interaction.Scope, "surface-interactions:"+interaction.ConversationID); err != nil {
		return nil, false, err
	}
	conversation, err := s.getPostgresConversation(ctx, tx, interaction.Scope, interaction.ConversationID, false)
	if err != nil {
		return nil, false, err
	}
	if conversation == nil {
		return nil, false, ErrConversationNotFound
	}
	scope := []interface{}{interaction.Scope.Kind, interaction.Scope.ID, interaction.ConversationID}
	existing, err := s.getPostgresSurfaceInteraction(ctx, tx, `idempotency_key = $4`, append(scope, interaction.IdempotencyKey)...)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		if !sameSurfaceInteractionTarget(existing, interaction) {
			return nil, false, &SurfaceInteractionConflictError{Existing: existing}
		}
		return existing, true, tx.Commit()
	}
	if !interaction.Repeatable {
		existing, err := s.getPostgresSurfaceInteraction(ctx, tx, `NOT repeatable AND surface_id = $4 AND surface_version = $5 AND component_id = $6 AND actor_type = $7 AND actor_id = $8`,
			append(scope, interaction.SurfaceID, interaction.SurfaceVersion, interaction.ComponentID, interaction.ActorType, interaction.ActorID)...)
		if err != nil {
			return nil, false, err
		}
		if existing != nil {
			return nil, false, &SurfaceInteractionConflictError{Existing: existing}
		}
	}
	table := s.table("surface_interactions")
	created := cloneSurfaceInteraction(interaction)
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM `+table+`
		WHERE scope_kind = $1 AND scope_id = $2 AND conversation_id = $3`, scope...).Scan(&created.Sequence); err != nil {
		return nil, false, err
	}
	payload, err := json.Marshal(created)
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+table+`
		(scope_kind, scope_id, conversation_id, id, sequence, surface_artifact_id, surface_id, surface_version, message_id, component_id,
		 item_key, actor_type, actor_id, repeatable, idempotency_key, created_at, payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17::jsonb)`, created.Scope.Kind, created.Scope.ID, created.ConversationID,
		created.ID, created.Sequence, created.SurfaceArtifactID, created.SurfaceID, created.SurfaceVersion, created.MessageID, created.ComponentID,
		created.ItemKey, created.ActorType, created.ActorID, created.Repeatable, created.IdempotencyKey, created.CreatedAt, string(payload)); err != nil {
		if postgresUniqueViolation(err) {
			return nil, false, ErrSurfaceInteractionConflict
		}
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return cloneSurfaceInteraction(created), false, nil
}

func (s *PostgresStore) ListSurfaceInteractions(ctx context.Context, filter SurfaceInteractionFilter) ([]*SurfaceInteraction, error) {
	if err := filter.validate(); err != nil {
		return nil, err
	}
	if conversation, err := s.getPostgresConversation(ctx, s.db, filter.Scope, filter.ConversationID, false); err != nil {
		return nil, err
	} else if conversation == nil {
		return nil, ErrConversationNotFound
	}
	query := `SELECT payload FROM ` + s.table("surface_interactions") + ` WHERE scope_kind = $1 AND scope_id = $2 AND conversation_id = $3 AND sequence > $4`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID, filter.ConversationID, filter.AfterSequence}
	if filter.SurfaceID != "" {
		query += ` AND surface_id = $5`
		args = append(args, filter.SurfaceID)
	}
	query += fmt.Sprintf(` ORDER BY sequence ASC LIMIT $%d`, len(args)+1)
	rows, err := s.db.QueryContext(ctx, query, append(args, filter.limit())...)
	if err != nil {
		return nil, err
	}
	return scanSurfaceInteractions(rows)
}

func (s *PostgresStore) ListLatestSurfaceInteractions(ctx context.Context, filter LatestSurfaceInteractionFilter) ([]*SurfaceInteraction, error) {
	if err := filter.validate(); err != nil {
		return nil, err
	}
	if conversation, err := s.getPostgresConversation(ctx, s.db, filter.Scope, filter.ConversationID, false); err != nil {
		return nil, err
	} else if conversation == nil {
		return nil, ErrConversationNotFound
	}
	rows, err := s.db.QueryContext(ctx, latestSurfaceInteractionQuery(s.table("surface_interactions"), filter.AcrossActors, [4]string{"$1", "$2", "$3", "$4"}),
		filter.Scope.Kind, filter.Scope.ID, filter.ConversationID, filter.SurfaceID)
	if err != nil {
		return nil, err
	}
	return scanSurfaceInteractions(rows)
}

// getPostgresSurfaceInteraction reads one interaction of a conversation; where
// filters after the leading $1..$3 scope and conversation parameters.
func (s *PostgresStore) getPostgresSurfaceInteraction(ctx context.Context, queryer postgresConversationQueryer, where string, args ...interface{}) (*SurfaceInteraction, error) {
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM `+s.table("surface_interactions")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND conversation_id = $3 AND `+where+` ORDER BY sequence LIMIT 1`, args...).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeSurfaceInteraction(payload)
}
