package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

func migrateSourceMonitors(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS source_observations (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			initiative_id TEXT NOT NULL, monitor_id TEXT NOT NULL, run_id TEXT NOT NULL,
			dedupe_key TEXT NOT NULL, ingested_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY(scope_kind, scope_id, id),
			UNIQUE(scope_kind, scope_id, initiative_id, monitor_id, dedupe_key)
		);
		CREATE INDEX IF NOT EXISTS idx_source_observations_monitor ON source_observations(scope_kind,scope_id,initiative_id,monitor_id,ingested_at DESC);
		CREATE INDEX IF NOT EXISTS idx_source_observations_run ON source_observations(scope_kind,scope_id,run_id,ingested_at DESC);
		CREATE TABLE IF NOT EXISTS source_monitor_checkpoints (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, initiative_id TEXT NOT NULL, monitor_id TEXT NOT NULL,
			revision INTEGER NOT NULL, updated_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY(scope_kind, scope_id, initiative_id, monitor_id)
		);
	`)
	return err
}

func (s *SQLiteStore) IngestSourceObservation(ctx context.Context, observation *SourceObservation, checkpoint *SourceMonitorCheckpoint, expected int64, event *ActivityEvent) (*SourceObservation, *SourceMonitorCheckpoint, *ActivityEvent, bool, error) {
	if err := observation.Validate(); err != nil {
		return nil, nil, nil, false, err
	}
	if event == nil || event.Validate() != nil {
		return nil, nil, nil, false, ErrInvalidSourceObservation
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, nil, nil, false, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, nil, nil, false, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	current, err := getSQLiteSourceMonitorCheckpoint(ctx, conn, observation.Scope, observation.InitiativeID, observation.MonitorID)
	if err != nil && !errors.Is(err, ErrSourceObservationNotFound) {
		return nil, nil, nil, false, err
	}
	currentRevision := int64(0)
	if current != nil {
		currentRevision = current.Revision
	}
	existing, err := getSQLiteSourceObservationByDedupe(ctx, conn, observation)
	if err != nil && !errors.Is(err, ErrSourceObservationNotFound) {
		return nil, nil, nil, false, err
	}
	if currentRevision != expected {
		if existing != nil && current != nil && current.LastObservationID == existing.ID && current.Cursor == checkpoint.Cursor {
			if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
				return nil, nil, nil, false, err
			}
			committed = true
			return existing, current, nil, true, nil
		}
		return nil, nil, nil, false, ErrSourceMonitorCheckpoint
	}
	next := cloneSourceMonitorCheckpoint(checkpoint)
	next.Revision = expected + 1
	if existing != nil {
		next.ObservationCount = currentObservationCount(current)
		if err = upsertSQLiteSourceMonitorCheckpoint(ctx, conn, next, expected); err != nil {
			return nil, nil, nil, false, err
		}
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, nil, nil, false, err
		}
		committed = true
		return existing, next, nil, true, nil
	}
	next.ObservationCount = currentObservationCount(current) + 1
	payload, err := json.Marshal(observation)
	if err != nil {
		return nil, nil, nil, false, err
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO source_observations(scope_kind,scope_id,id,initiative_id,monitor_id,run_id,dedupe_key,ingested_at,payload)VALUES(?,?,?,?,?,?,?,?,?)`, observation.Scope.Kind, observation.Scope.ID, observation.ID, observation.InitiativeID, observation.MonitorID, observation.RunID, observation.DedupeKey, observation.IngestedAt, string(payload)); err != nil {
		return nil, nil, nil, false, err
	}
	if err = upsertSQLiteSourceMonitorCheckpoint(ctx, conn, next, expected); err != nil {
		return nil, nil, nil, false, err
	}
	persistedEvent, err := insertSQLiteActivityConn(ctx, conn, event)
	if err != nil {
		return nil, nil, nil, false, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, nil, nil, false, err
	}
	committed = true
	return cloneSourceObservation(observation), cloneSourceMonitorCheckpoint(next), persistedEvent, false, nil
}

func (s *SQLiteStore) AdvanceSourceMonitorCheckpoint(ctx context.Context, checkpoint *SourceMonitorCheckpoint, expected int64, event *ActivityEvent) (*SourceMonitorCheckpoint, *ActivityEvent, bool, error) {
	if checkpoint == nil || event == nil || event.Validate() != nil {
		return nil, nil, false, ErrInvalidSourceObservation
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, nil, false, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	current, err := getSQLiteSourceMonitorCheckpoint(ctx, conn, checkpoint.Scope, checkpoint.InitiativeID, checkpoint.MonitorID)
	if err != nil && !errors.Is(err, ErrSourceObservationNotFound) {
		return nil, nil, false, err
	}
	if current != nil && current.LastActionCallID == checkpoint.LastActionCallID {
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, nil, false, err
		}
		committed = true
		return current, nil, true, nil
	}
	if currentObservationRevision(current) != expected {
		return nil, nil, false, ErrSourceMonitorCheckpoint
	}
	next := checkpointWithoutObservationChange(checkpoint, current, expected)
	if err = upsertSQLiteSourceMonitorCheckpoint(ctx, conn, next, expected); err != nil {
		return nil, nil, false, err
	}
	persisted, err := insertSQLiteActivityConn(ctx, conn, event)
	if err != nil {
		return nil, nil, false, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, nil, false, err
	}
	committed = true
	return next, persisted, false, nil
}

func upsertSQLiteSourceMonitorCheckpoint(ctx context.Context, conn *sql.Conn, checkpoint *SourceMonitorCheckpoint, expected int64) error {
	payload, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	if expected == 0 {
		_, err = conn.ExecContext(ctx, `INSERT INTO source_monitor_checkpoints(scope_kind,scope_id,initiative_id,monitor_id,revision,updated_at,payload)VALUES(?,?,?,?,?,?,?)`, checkpoint.Scope.Kind, checkpoint.Scope.ID, checkpoint.InitiativeID, checkpoint.MonitorID, checkpoint.Revision, checkpoint.UpdatedAt, string(payload))
		return err
	}
	result, err := conn.ExecContext(ctx, `UPDATE source_monitor_checkpoints SET revision=?,updated_at=?,payload=? WHERE scope_kind=? AND scope_id=? AND initiative_id=? AND monitor_id=? AND revision=?`, checkpoint.Revision, checkpoint.UpdatedAt, string(payload), checkpoint.Scope.Kind, checkpoint.Scope.ID, checkpoint.InitiativeID, checkpoint.MonitorID, expected)
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

func getSQLiteSourceMonitorCheckpoint(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}, scope Scope, initiativeID, monitorID string) (*SourceMonitorCheckpoint, error) {
	var payload string
	err := query.QueryRowContext(ctx, `SELECT payload FROM source_monitor_checkpoints WHERE scope_kind=? AND scope_id=? AND initiative_id=? AND monitor_id=?`, scope.Kind, scope.ID, initiativeID, monitorID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSourceObservationNotFound
	}
	if err != nil {
		return nil, err
	}
	var value SourceMonitorCheckpoint
	if err = json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func getSQLiteSourceObservationByDedupe(ctx context.Context, conn *sql.Conn, observation *SourceObservation) (*SourceObservation, error) {
	var payload string
	err := conn.QueryRowContext(ctx, `SELECT payload FROM source_observations WHERE scope_kind=? AND scope_id=? AND initiative_id=? AND monitor_id=? AND dedupe_key=?`, observation.Scope.Kind, observation.Scope.ID, observation.InitiativeID, observation.MonitorID, observation.DedupeKey).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSourceObservationNotFound
	}
	if err != nil {
		return nil, err
	}
	var value SourceObservation
	if err = json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *SQLiteStore) GetSourceObservation(ctx context.Context, scope Scope, id string) (*SourceObservation, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM source_observations WHERE scope_kind=? AND scope_id=? AND id=?`, scope.Kind, scope.ID, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSourceObservationNotFound
	}
	if err != nil {
		return nil, err
	}
	var value SourceObservation
	if err = json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *SQLiteStore) ListSourceObservations(ctx context.Context, filter SourceObservationFilter) ([]*SourceObservation, error) {
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
	query := `SELECT payload FROM source_observations WHERE scope_kind=? AND scope_id=?`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	for _, selector := range []struct{ column, value string }{{"initiative_id", filter.InitiativeID}, {"monitor_id", filter.MonitorID}, {"run_id", filter.RunID}} {
		if selector.value != "" {
			query += " AND " + selector.column + "=?"
			args = append(args, selector.value)
		}
	}
	if filter.ActionCallID != "" {
		query += " AND json_extract(payload,'$.actionCallId')=?"
		args = append(args, filter.ActionCallID)
	}
	query += ` ORDER BY ingested_at DESC,id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, filter.Offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]*SourceObservation, 0)
	for rows.Next() {
		var payload string
		if err = rows.Scan(&payload); err != nil {
			return nil, err
		}
		var value SourceObservation
		if err = json.Unmarshal([]byte(payload), &value); err != nil {
			return nil, err
		}
		values = append(values, &value)
	}
	return values, rows.Err()
}

func (s *SQLiteStore) GetSourceMonitorCheckpoint(ctx context.Context, scope Scope, initiativeID, monitorID string) (*SourceMonitorCheckpoint, error) {
	return getSQLiteSourceMonitorCheckpoint(ctx, s.db, scope, strings.TrimSpace(initiativeID), strings.TrimSpace(monitorID))
}

var _ SourceMonitorStore = (*SQLiteStore)(nil)
