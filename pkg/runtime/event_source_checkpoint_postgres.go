package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

func (s *PostgresStore) migrateEventSourceCheckpoints(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+s.table("event_source_checkpoints")+` (
		scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, source TEXT NOT NULL, subscription_id TEXT NOT NULL,
		revision BIGINT NOT NULL CHECK(revision>0), updated_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
		PRIMARY KEY(scope_kind, scope_id, source, subscription_id)
	)`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES(20,'durable event source checkpoints') ON CONFLICT(version) DO NOTHING`)
	return err
}

func (s *PostgresStore) GetEventSourceCheckpoint(ctx context.Context, scope Scope, source, subscriptionID string) (*EventSourceCheckpoint, error) {
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("event_source_checkpoints")+` WHERE scope_kind=$1 AND scope_id=$2 AND source=$3 AND subscription_id=$4`, scope.Kind, scope.ID, source, subscriptionID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var checkpoint EventSourceCheckpoint
	if err := json.Unmarshal(payload, &checkpoint); err != nil {
		return nil, err
	}
	return &checkpoint, nil
}

func (s *PostgresStore) SaveEventSourceCheckpoint(ctx context.Context, checkpoint *EventSourceCheckpoint, expectedRevision int64) error {
	if checkpoint == nil || checkpoint.Validate() != nil || checkpoint.Revision != expectedRevision+1 {
		return ErrInvalidEventSourceCheckpoint
	}
	payload, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	var result sql.Result
	if expectedRevision == 0 {
		result, err = s.db.ExecContext(ctx, `INSERT INTO `+s.table("event_source_checkpoints")+`(scope_kind,scope_id,source,subscription_id,revision,updated_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb) ON CONFLICT DO NOTHING`, checkpoint.Scope.Kind, checkpoint.Scope.ID, checkpoint.Source, checkpoint.SubscriptionID, checkpoint.Revision, checkpoint.UpdatedAt, string(payload))
	} else {
		result, err = s.db.ExecContext(ctx, `UPDATE `+s.table("event_source_checkpoints")+` SET revision=$1,updated_at=$2,payload=$3::jsonb WHERE scope_kind=$4 AND scope_id=$5 AND source=$6 AND subscription_id=$7 AND revision=$8`, checkpoint.Revision, checkpoint.UpdatedAt, string(payload), checkpoint.Scope.Kind, checkpoint.Scope.ID, checkpoint.Source, checkpoint.SubscriptionID, expectedRevision)
	}
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrEventSourceCheckpointConflict
	}
	return nil
}

var _ EventSourceCheckpointStore = (*PostgresStore)(nil)
