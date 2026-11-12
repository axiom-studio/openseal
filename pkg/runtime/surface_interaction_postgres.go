package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

const surfaceInteractionMigrationVersion int64 = 65

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
	if err := filter.Validate(); err != nil {
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
	rows, err := s.db.QueryContext(ctx, query, append(args, filter.EffectiveLimit())...)
	if err != nil {
		return nil, err
	}
	return scanSurfaceInteractions(rows)
}

func (s *PostgresStore) ListLatestSurfaceInteractions(ctx context.Context, filter LatestSurfaceInteractionFilter) ([]*SurfaceInteraction, error) {
	if err := filter.Validate(); err != nil {
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
