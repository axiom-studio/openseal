package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

func (s *PostgresStore) migrateSourceMonitors(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("source_observations")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			initiative_id TEXT NOT NULL, monitor_id TEXT NOT NULL, run_id TEXT NOT NULL,
			dedupe_key TEXT NOT NULL, ingested_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY(scope_kind,scope_id,id),
			UNIQUE(scope_kind,scope_id,initiative_id,monitor_id,dedupe_key)
		);
		CREATE INDEX IF NOT EXISTS source_observations_monitor_idx ON `+s.table("source_observations")+`(scope_kind,scope_id,initiative_id,monitor_id,ingested_at DESC);
		CREATE INDEX IF NOT EXISTS source_observations_run_idx ON `+s.table("source_observations")+`(scope_kind,scope_id,run_id,ingested_at DESC);
		CREATE TABLE IF NOT EXISTS `+s.table("source_monitor_checkpoints")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, initiative_id TEXT NOT NULL, monitor_id TEXT NOT NULL,
			revision BIGINT NOT NULL CHECK(revision>0), updated_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY(scope_kind,scope_id,initiative_id,monitor_id)
		)`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name)VALUES(16,'source monitor observations and checkpoints')ON CONFLICT(version)DO NOTHING`)
	return err
}

func (s *PostgresStore) IngestSourceObservation(ctx context.Context, observation *SourceObservation, checkpoint *SourceMonitorCheckpoint, expected int64, event *ActivityEvent) (*SourceObservation, *SourceMonitorCheckpoint, *ActivityEvent, bool, error) {
	if err := observation.Validate(); err != nil {
		return nil, nil, nil, false, err
	}
	if event == nil || event.Validate() != nil {
		return nil, nil, nil, false, ErrInvalidSourceObservation
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, nil, false, err
	}
	defer tx.Rollback()
	lockKey := "openseal:source-monitor:" + observation.Scope.Kind + ":" + observation.Scope.ID + ":" + observation.InitiativeID + ":" + observation.MonitorID
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lockKey); err != nil {
		return nil, nil, nil, false, err
	}
	current, err := s.getPostgresSourceMonitorCheckpointTx(ctx, tx, observation.Scope, observation.InitiativeID, observation.MonitorID, true)
	if err != nil && !errors.Is(err, ErrSourceObservationNotFound) {
		return nil, nil, nil, false, err
	}
	currentRevision := int64(0)
	if current != nil {
		currentRevision = current.Revision
	}
	existing, err := s.getPostgresSourceObservationByDedupeTx(ctx, tx, observation)
	if err != nil && !errors.Is(err, ErrSourceObservationNotFound) {
		return nil, nil, nil, false, err
	}
	if currentRevision != expected {
		if existing != nil && current != nil && current.LastObservationID == existing.ID && current.Cursor == checkpoint.Cursor {
			if err = tx.Commit(); err != nil {
				return nil, nil, nil, false, err
			}
			return existing, current, nil, true, nil
		}
		return nil, nil, nil, false, ErrSourceMonitorCheckpoint
	}
	next := cloneSourceMonitorCheckpoint(checkpoint)
	next.Revision = expected + 1
	if existing != nil {
		next.ObservationCount = currentObservationCount(current)
		if err = s.upsertPostgresSourceMonitorCheckpointTx(ctx, tx, next, expected); err != nil {
			return nil, nil, nil, false, err
		}
		if err = tx.Commit(); err != nil {
			return nil, nil, nil, false, err
		}
		return existing, next, nil, true, nil
	}
	next.ObservationCount = currentObservationCount(current) + 1
	payload, err := json.Marshal(observation)
	if err != nil {
		return nil, nil, nil, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("source_observations")+`(scope_kind,scope_id,id,initiative_id,monitor_id,run_id,dedupe_key,ingested_at,payload)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb)`, observation.Scope.Kind, observation.Scope.ID, observation.ID, observation.InitiativeID, observation.MonitorID, observation.RunID, observation.DedupeKey, observation.IngestedAt, string(payload)); err != nil {
		return nil, nil, nil, false, err
	}
	if err = s.upsertPostgresSourceMonitorCheckpointTx(ctx, tx, next, expected); err != nil {
		return nil, nil, nil, false, err
	}
	persistedEvent, err := s.insertPostgresActivityTx(ctx, tx, event)
	if err != nil {
		return nil, nil, nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, nil, nil, false, err
	}
	return cloneSourceObservation(observation), cloneSourceMonitorCheckpoint(next), persistedEvent, false, nil
}

func (s *PostgresStore) upsertPostgresSourceMonitorCheckpointTx(ctx context.Context, tx *sql.Tx, checkpoint *SourceMonitorCheckpoint, expected int64) error {
	payload, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	if expected == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("source_monitor_checkpoints")+`(scope_kind,scope_id,initiative_id,monitor_id,revision,updated_at,payload)VALUES($1,$2,$3,$4,$5,$6,$7::jsonb)`, checkpoint.Scope.Kind, checkpoint.Scope.ID, checkpoint.InitiativeID, checkpoint.MonitorID, checkpoint.Revision, checkpoint.UpdatedAt, string(payload))
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("source_monitor_checkpoints")+` SET revision=$1,updated_at=$2,payload=$3::jsonb WHERE scope_kind=$4 AND scope_id=$5 AND initiative_id=$6 AND monitor_id=$7 AND revision=$8`, checkpoint.Revision, checkpoint.UpdatedAt, string(payload), checkpoint.Scope.Kind, checkpoint.Scope.ID, checkpoint.InitiativeID, checkpoint.MonitorID, expected)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrSourceMonitorCheckpoint
	}
	return nil
}

func (s *PostgresStore) getPostgresSourceMonitorCheckpointTx(ctx context.Context, tx *sql.Tx, scope Scope, initiativeID, monitorID string, lock bool) (*SourceMonitorCheckpoint, error) {
	query := `SELECT payload FROM ` + s.table("source_monitor_checkpoints") + ` WHERE scope_kind=$1 AND scope_id=$2 AND initiative_id=$3 AND monitor_id=$4`
	if lock {
		query += ` FOR UPDATE`
	}
	var payload []byte
	err := tx.QueryRowContext(ctx, query, scope.Kind, scope.ID, initiativeID, monitorID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSourceObservationNotFound
	}
	if err != nil {
		return nil, err
	}
	var value SourceMonitorCheckpoint
	if err = json.Unmarshal(payload, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *PostgresStore) getPostgresSourceObservationByDedupeTx(ctx context.Context, tx *sql.Tx, observation *SourceObservation) (*SourceObservation, error) {
	var payload []byte
	err := tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("source_observations")+` WHERE scope_kind=$1 AND scope_id=$2 AND initiative_id=$3 AND monitor_id=$4 AND dedupe_key=$5`, observation.Scope.Kind, observation.Scope.ID, observation.InitiativeID, observation.MonitorID, observation.DedupeKey).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSourceObservationNotFound
	}
	if err != nil {
		return nil, err
	}
	var value SourceObservation
	if err = json.Unmarshal(payload, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *PostgresStore) GetSourceObservation(ctx context.Context, scope Scope, id string) (*SourceObservation, error) {
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("source_observations")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3`, scope.Kind, scope.ID, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSourceObservationNotFound
	}
	if err != nil {
		return nil, err
	}
	var value SourceObservation
	if err = json.Unmarshal(payload, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *PostgresStore) ListSourceObservations(ctx context.Context, filter SourceObservationFilter) ([]*SourceObservation, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 || filter.Offset < 0 {
		return nil, ErrInvalidSourceObservation
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("source_observations")+` WHERE scope_kind=$1 AND scope_id=$2 AND ($3='' OR initiative_id=$3) AND ($4='' OR monitor_id=$4) AND ($5='' OR run_id=$5) AND ($6='' OR payload->>'actionCallId'=$6) ORDER BY ingested_at DESC,id DESC LIMIT $7 OFFSET $8`, filter.Scope.Kind, filter.Scope.ID, filter.InitiativeID, filter.MonitorID, filter.RunID, filter.ActionCallID, limit, filter.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]*SourceObservation, 0)
	for rows.Next() {
		var payload []byte
		if err = rows.Scan(&payload); err != nil {
			return nil, err
		}
		var value SourceObservation
		if err = json.Unmarshal(payload, &value); err != nil {
			return nil, err
		}
		values = append(values, &value)
	}
	return values, rows.Err()
}

func (s *PostgresStore) GetSourceMonitorCheckpoint(ctx context.Context, scope Scope, initiativeID, monitorID string) (*SourceMonitorCheckpoint, error) {
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("source_monitor_checkpoints")+` WHERE scope_kind=$1 AND scope_id=$2 AND initiative_id=$3 AND monitor_id=$4`, scope.Kind, scope.ID, strings.TrimSpace(initiativeID), strings.TrimSpace(monitorID)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSourceObservationNotFound
	}
	if err != nil {
		return nil, err
	}
	var value SourceMonitorCheckpoint
	if err = json.Unmarshal(payload, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

var _ SourceMonitorStore = (*PostgresStore)(nil)
