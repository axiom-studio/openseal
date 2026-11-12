package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

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
	if err := filter.Validate(); err != nil {
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
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY sequence ASC LIMIT ?`, append(args, filter.EffectiveLimit())...)
	if err != nil {
		return nil, err
	}
	return scanSurfaceInteractions(rows)
}

func (s *SQLiteStore) ListLatestSurfaceInteractions(ctx context.Context, filter LatestSurfaceInteractionFilter) ([]*SurfaceInteraction, error) {
	if err := filter.Validate(); err != nil {
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
