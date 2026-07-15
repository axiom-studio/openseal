package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

func migrateEventSourceCheckpoints(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS event_source_checkpoints (
		scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, source TEXT NOT NULL, subscription_id TEXT NOT NULL,
		revision INTEGER NOT NULL, updated_at DATETIME NOT NULL, payload TEXT NOT NULL,
		PRIMARY KEY(scope_kind, scope_id, source, subscription_id)
	)`)
	return err
}

func (s *SQLiteStore) GetEventSourceCheckpoint(ctx context.Context, scope Scope, source, subscriptionID string) (*EventSourceCheckpoint, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM event_source_checkpoints WHERE scope_kind=? AND scope_id=? AND source=? AND subscription_id=?`, scope.Kind, scope.ID, source, subscriptionID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var checkpoint EventSourceCheckpoint
	if err := json.Unmarshal([]byte(payload), &checkpoint); err != nil {
		return nil, err
	}
	return &checkpoint, nil
}

func (s *SQLiteStore) SaveEventSourceCheckpoint(ctx context.Context, checkpoint *EventSourceCheckpoint, expectedRevision int64) error {
	if checkpoint == nil || checkpoint.Validate() != nil || checkpoint.Revision != expectedRevision+1 {
		return ErrInvalidEventSourceCheckpoint
	}
	payload, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	var result sql.Result
	if expectedRevision == 0 {
		result, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO event_source_checkpoints(scope_kind,scope_id,source,subscription_id,revision,updated_at,payload) VALUES(?,?,?,?,?,?,?)`, checkpoint.Scope.Kind, checkpoint.Scope.ID, checkpoint.Source, checkpoint.SubscriptionID, checkpoint.Revision, checkpoint.UpdatedAt, string(payload))
	} else {
		result, err = s.db.ExecContext(ctx, `UPDATE event_source_checkpoints SET revision=?,updated_at=?,payload=? WHERE scope_kind=? AND scope_id=? AND source=? AND subscription_id=? AND revision=?`, checkpoint.Revision, checkpoint.UpdatedAt, string(payload), checkpoint.Scope.Kind, checkpoint.Scope.ID, checkpoint.Source, checkpoint.SubscriptionID, expectedRevision)
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

var _ EventSourceCheckpointStore = (*SQLiteStore)(nil)
