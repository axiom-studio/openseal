package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

// sqliteRoutineTimeLayout is fixed width, so stored times compare as text.
const sqliteRoutineTimeLayout = "2006-01-02T15:04:05.000000000Z"

func sqliteRoutineTime(value time.Time) string { return value.UTC().Format(sqliteRoutineTimeLayout) }

func parseSQLiteRoutineTime(value string) (time.Time, error) {
	return time.Parse(sqliteRoutineTimeLayout, value)
}

func sqliteRoutineNullTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid || value.String == "" {
		return nil, nil
	}
	parsed, err := parseSQLiteRoutineTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func sqliteStringArgs(values []string) []interface{} {
	args := make([]interface{}, len(values))
	for i, value := range values {
		args[i] = value
	}
	return args
}

// withSQLiteWrite runs fn in a BEGIN IMMEDIATE transaction, so a read that
// decides a write cannot race another writer.
func (s *Store) withSQLiteWrite(ctx context.Context, fn func(conn *sql.Conn) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if err := fn(conn); err != nil {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		return err
	}
	return nil
}

func (s *Store) ListRoutineEventActivations(ctx context.Context, scope models.Scope, kind models.RoutineEventKind, sourceRef string) ([]*models.RunbookActivation, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM runbook_activations
		WHERE scope_kind = ? AND scope_id = ? AND event_kind = ? AND event_source_ref = ? AND status = 'active' AND event_kind IS NOT NULL ORDER BY id`,
		scope.Kind, scope.ID, string(kind), sourceRef)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]*models.RunbookActivation, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		activation, err := sqlstore.DecodeRunbookActivation(payload)
		if err != nil {
			return nil, err
		}
		values = append(values, activation)
	}
	return values, rows.Err()
}

func (s *Store) ListRoutineEventSubscriptions(ctx context.Context, kinds []models.RoutineEventKind, after string, limit int) ([]models.RoutineEventSubscription, string, error) {
	kindValues, err := models.RoutineEventKindStrings(kinds)
	if err != nil {
		return nil, "", err
	}
	afterKind, afterScope, afterID, err := models.DecodeRoutineEventSubscriptionCursor(after)
	if err != nil {
		return nil, "", err
	}
	limit = models.NormalizeRoutineEventLimit(limit, models.RoutineEventSubscriptionPageLimit)
	query := `SELECT payload FROM runbook_activations WHERE status = 'active' AND event_kind IS NOT NULL`
	args := make([]interface{}, 0, len(kindValues)+4)
	if len(kindValues) > 0 {
		query += ` AND event_kind IN (` + sqlitePlaceholders(len(kindValues)) + `)`
		args = append(args, sqliteStringArgs(kindValues)...)
	}
	if after != "" {
		query += ` AND (scope_kind, scope_id, id) > (?, ?, ?)`
		args = append(args, afterKind, afterScope, afterID)
	}
	query += ` ORDER BY scope_kind, scope_id, id LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	values := make([]models.RoutineEventSubscription, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, "", err
		}
		activation, err := sqlstore.DecodeRunbookActivation(payload)
		if err != nil {
			return nil, "", err
		}
		if subscription, ok := models.RoutineEventSubscriptionOf(activation); ok {
			values = append(values, subscription)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(values) == limit {
		next = models.EncodeRoutineEventSubscriptionCursor(values[len(values)-1])
	}
	return values, next, nil
}

func (s *Store) ListRoutineEventSourceKeysInScope(ctx context.Context, scope models.Scope) ([]models.RoutineEventSourceKey, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT event_kind, event_source_ref FROM runbook_activations
		WHERE scope_kind = ? AND scope_id = ? AND status = 'active' AND event_kind IS NOT NULL ORDER BY 1, 2`, scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := make([]models.RoutineEventSourceKey, 0)
	for rows.Next() {
		key := models.RoutineEventSourceKey{Scope: scope}
		if err := rows.Scan(&key.Kind, &key.SourceRef); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (s *Store) CountActiveRoutineEventRuns(ctx context.Context, scope models.Scope) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM routine_event_batches b
		JOIN agent_runs r ON r.scope_kind = b.scope_kind AND r.scope_id = b.scope_id AND r.id = b.run_id
		WHERE b.scope_kind = ? AND b.scope_id = ? AND b.started_at >= ? AND r.status NOT IN ('completed','failed','canceled')`,
		scope.Kind, scope.ID, sqliteRoutineTime(time.Now().Add(-models.RoutineEventActiveRunWindow))).Scan(&count)
	return count, err
}

func (s *Store) InsertRoutineEvent(ctx context.Context, record *models.RoutineEventRecord) (bool, error) {
	if err := models.PrepareRoutineEventRecord(record); err != nil {
		return false, err
	}
	payload, err := sqlstore.RoutineEventRecordPayload(record)
	if err != nil {
		return false, err
	}
	var payloadValue, droppedReason interface{}
	if payload != nil {
		payloadValue = string(payload)
	}
	if record.DroppedReason != "" {
		droppedReason = record.DroppedReason
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO routine_events
		(scope_kind, scope_id, activation_id, event_id, state, dropped_reason, test, received_at, occurred_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (scope_kind, scope_id, activation_id, event_id) DO NOTHING`,
		record.Scope.Kind, record.Scope.ID, record.ActivationID, record.Event.ID, string(record.State), droppedReason,
		record.Event.Test, sqliteRoutineTime(record.ReceivedAt), sqliteRoutineTime(record.Event.OccurredAt), payloadValue)
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	return inserted == 1, err
}

func (s *Store) CountPendingRoutineEvents(ctx context.Context, scope models.Scope, activationID string) (int, error) {
	if err := models.ValidateRoutineEventScopeActivation(scope, activationID); err != nil {
		return 0, err
	}
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM routine_events
		WHERE scope_kind = ? AND scope_id = ? AND activation_id = ? AND state IN ('pending','batched')`, scope.Kind, scope.ID, activationID).Scan(&count)
	return count, err
}

func (s *Store) CountPendingRoutineEventsInScope(ctx context.Context, scope models.Scope) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM routine_events
		WHERE scope_kind = ? AND scope_id = ? AND state IN ('pending','batched')`, scope.Kind, scope.ID).Scan(&count)
	return count, err
}

func (s *Store) ListRoutineEventWorkScopes(ctx context.Context) ([]models.Scope, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT scope_kind, scope_id FROM routine_events
		WHERE state IN ('pending','batched') ORDER BY scope_kind, scope_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	scopes := make([]models.Scope, 0)
	for rows.Next() {
		var scope models.Scope
		if err := rows.Scan(&scope.Kind, &scope.ID); err != nil {
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	return scopes, rows.Err()
}

func (s *Store) ListRoutineEventWork(ctx context.Context, scope models.Scope, after string, limit int) ([]string, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT activation_id FROM routine_events
		WHERE scope_kind = ? AND scope_id = ? AND state IN ('pending','batched') AND activation_id > ? ORDER BY activation_id LIMIT ?`,
		scope.Kind, scope.ID, after, models.NormalizeRoutineEventLimit(limit, models.RoutineEventSubscriptionPageLimit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

const sqliteRoutineBatchColumns = `scope_kind, scope_id, id, activation_id, event_ids, dropped, test, COALESCE(run_id, ''), created_at, started_at`

func scanSQLiteRoutineEventBatch(row sqlstore.RowScanner) (*models.RoutineEventBatch, error) {
	var batch models.RoutineEventBatch
	var eventIDs, createdAt string
	var startedAt sql.NullString
	if err := row.Scan(&batch.Scope.Kind, &batch.Scope.ID, &batch.ID, &batch.ActivationID, &eventIDs, &batch.Dropped, &batch.Test, &batch.RunID, &createdAt, &startedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(eventIDs), &batch.EventIDs); err != nil {
		return nil, err
	}
	created, err := parseSQLiteRoutineTime(createdAt)
	if err != nil {
		return nil, err
	}
	batch.CreatedAt = created
	if batch.StartedAt, err = sqliteRoutineNullTime(startedAt); err != nil {
		return nil, err
	}
	return &batch, nil
}

func sqliteOpenRoutineEventBatch(ctx context.Context, query sqlstore.ReadWriter, scope models.Scope, activationID string) (*models.RoutineEventBatch, error) {
	batch, err := scanSQLiteRoutineEventBatch(query.QueryRowContext(ctx, `SELECT `+sqliteRoutineBatchColumns+` FROM routine_event_batches
		WHERE scope_kind = ? AND scope_id = ? AND activation_id = ? AND started_at IS NULL`, scope.Kind, scope.ID, activationID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return batch, err
}

func (s *Store) ClaimRoutineEventBatch(ctx context.Context, scope models.Scope, activationID string, maxEvents int, now time.Time) (*models.RoutineEventBatch, error) {
	if err := models.ValidateRoutineEventScopeActivation(scope, activationID); err != nil {
		return nil, err
	}
	maxEvents = models.NormalizeRoutineEventBatchSize(maxEvents)
	var claimed *models.RoutineEventBatch
	err := s.withSQLiteWrite(ctx, func(conn *sql.Conn) error {
		open, err := sqliteOpenRoutineEventBatch(ctx, conn, scope, activationID)
		if err != nil || open != nil {
			claimed = open
			return err
		}
		pending := func(test bool) ([]string, error) {
			rows, err := conn.QueryContext(ctx, `SELECT event_id FROM routine_events
				WHERE scope_kind = ? AND scope_id = ? AND activation_id = ? AND state = 'pending' AND test = ?
				ORDER BY received_at, event_id LIMIT ?`, scope.Kind, scope.ID, activationID, test, maxEvents)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			ids := make([]string, 0)
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return nil, err
				}
				ids = append(ids, id)
			}
			return ids, rows.Err()
		}
		test := true
		ids, err := pending(true)
		if err == nil && len(ids) == 0 {
			test = false
			ids, err = pending(false)
		}
		if err != nil || len(ids) == 0 {
			return err
		}
		batch := &models.RoutineEventBatch{Scope: scope, ID: models.NewRoutineEventBatchID(), ActivationID: activationID, EventIDs: ids, Test: test, CreatedAt: now.UTC()}
		if !test {
			if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM routine_events
				WHERE scope_kind = ? AND scope_id = ? AND activation_id = ? AND state = 'dropped' AND dropped_reason = 'overflow' AND test = 0
					AND received_at > COALESCE((SELECT MAX(created_at) FROM routine_event_batches
						WHERE scope_kind = ? AND scope_id = ? AND activation_id = ? AND test = 0), '')`,
				scope.Kind, scope.ID, activationID, scope.Kind, scope.ID, activationID).Scan(&batch.Dropped); err != nil {
				return err
			}
		}
		encoded, err := json.Marshal(ids)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO routine_event_batches
			(scope_kind, scope_id, id, activation_id, event_ids, dropped, test, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			scope.Kind, scope.ID, batch.ID, activationID, string(encoded), batch.Dropped, test, sqliteRoutineTime(batch.CreatedAt)); err != nil {
			return err
		}
		args := append([]interface{}{batch.ID, scope.Kind, scope.ID, activationID}, sqliteStringArgs(ids)...)
		if _, err := conn.ExecContext(ctx, `UPDATE routine_events SET state = 'batched', batch_id = ?
			WHERE scope_kind = ? AND scope_id = ? AND activation_id = ? AND event_id IN (`+sqlitePlaceholders(len(ids))+`)`, args...); err != nil {
			return err
		}
		claimed = batch
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

func (s *Store) MarkRoutineEventBatchStarted(ctx context.Context, scope models.Scope, batchID, runID string, now time.Time) error {
	if err := models.ValidateRoutineEventScopeActivation(scope, batchID); err != nil {
		return err
	}
	if runID == "" {
		return errors.New("a started routine event batch needs its Run")
	}
	return s.withSQLiteWrite(ctx, func(conn *sql.Conn) error {
		batch, err := scanSQLiteRoutineEventBatch(conn.QueryRowContext(ctx, `SELECT `+sqliteRoutineBatchColumns+` FROM routine_event_batches
			WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, batchID))
		if errors.Is(err, sql.ErrNoRows) {
			return models.ErrRoutineEventBatchNotFound
		}
		if err != nil {
			return err
		}
		if batch.StartedAt != nil {
			if batch.RunID == runID {
				return nil
			}
			return models.ErrRoutineEventBatchStarted
		}
		if _, err := conn.ExecContext(ctx, `UPDATE routine_event_batches SET run_id = ?, started_at = ?
			WHERE scope_kind = ? AND scope_id = ? AND id = ?`, runID, sqliteRoutineTime(now), scope.Kind, scope.ID, batchID); err != nil {
			return err
		}
		if len(batch.EventIDs) == 0 {
			return nil
		}
		args := append([]interface{}{scope.Kind, scope.ID, batch.ActivationID}, sqliteStringArgs(batch.EventIDs)...)
		_, err = conn.ExecContext(ctx, `UPDATE routine_events SET state = 'started'
			WHERE scope_kind = ? AND scope_id = ? AND activation_id = ? AND state = 'batched' AND event_id IN (`+sqlitePlaceholders(len(batch.EventIDs))+`)`, args...)
		return err
	})
}

func (s *Store) GetRoutineEventBatch(ctx context.Context, scope models.Scope, batchID string) (*models.RoutineEventBatch, []*models.RoutineEventRecord, error) {
	if err := models.ValidateRoutineEventScopeActivation(scope, batchID); err != nil {
		return nil, nil, err
	}
	batch, err := scanSQLiteRoutineEventBatch(s.db.QueryRowContext(ctx, `SELECT `+sqliteRoutineBatchColumns+` FROM routine_event_batches
		WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, batchID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	records := make([]*models.RoutineEventRecord, 0, len(batch.EventIDs))
	if len(batch.EventIDs) == 0 {
		return batch, records, nil
	}
	args := append([]interface{}{scope.Kind, scope.ID, batch.ActivationID}, sqliteStringArgs(batch.EventIDs)...)
	rows, err := s.db.QueryContext(ctx, `SELECT event_id, state, COALESCE(batch_id, ''), COALESCE(dropped_reason, ''), test, received_at, occurred_at, payload
		FROM routine_events WHERE scope_kind = ? AND scope_id = ? AND activation_id = ? AND event_id IN (`+sqlitePlaceholders(len(batch.EventIDs))+`)`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	byID := make(map[string]*models.RoutineEventRecord, len(batch.EventIDs))
	for rows.Next() {
		record := &models.RoutineEventRecord{Scope: scope, ActivationID: batch.ActivationID}
		var eventID, receivedAt, occurredAt string
		var test bool
		var payload sql.NullString
		if err := rows.Scan(&eventID, &record.State, &record.BatchID, &record.DroppedReason, &test, &receivedAt, &occurredAt, &payload); err != nil {
			return nil, nil, err
		}
		if record.ReceivedAt, err = parseSQLiteRoutineTime(receivedAt); err != nil {
			return nil, nil, err
		}
		occurred, err := parseSQLiteRoutineTime(occurredAt)
		if err != nil {
			return nil, nil, err
		}
		if err := sqlstore.DecodeRoutineEventRecordEvent(record, []byte(payload.String), eventID, test, occurred); err != nil {
			return nil, nil, err
		}
		byID[eventID] = record
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for _, id := range batch.EventIDs {
		if record := byID[id]; record != nil {
			records = append(records, record)
		}
	}
	return batch, records, nil
}

func (s *Store) GetOpenRoutineEventBatch(ctx context.Context, scope models.Scope, activationID string) (*models.RoutineEventBatch, error) {
	if err := models.ValidateRoutineEventScopeActivation(scope, activationID); err != nil {
		return nil, err
	}
	return sqliteOpenRoutineEventBatch(ctx, s.db, scope, activationID)
}

func (s *Store) CountRoutineEventBatchesSince(ctx context.Context, scope models.Scope, activationID string, since time.Time, test bool) (int, *time.Time, error) {
	if err := models.ValidateRoutineEventScopeActivation(scope, activationID); err != nil {
		return 0, nil, err
	}
	var count int
	var oldest sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(started_at) FROM routine_event_batches
		WHERE scope_kind = ? AND scope_id = ? AND activation_id = ? AND started_at >= ? AND test = ?`,
		scope.Kind, scope.ID, activationID, sqliteRoutineTime(since), test).Scan(&count, &oldest); err != nil {
		return 0, nil, err
	}
	first, err := sqliteRoutineNullTime(oldest)
	return count, first, err
}

func (s *Store) CountRoutineTestEventsSince(ctx context.Context, scope models.Scope, activationID string, since time.Time) (int, error) {
	if err := models.ValidateRoutineEventScopeActivation(scope, activationID); err != nil {
		return 0, err
	}
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM routine_events
		WHERE scope_kind = ? AND scope_id = ? AND activation_id = ? AND test = 1 AND received_at >= ?`,
		scope.Kind, scope.ID, activationID, sqliteRoutineTime(since)).Scan(&count)
	return count, err
}

func (s *Store) DropRoutineEvents(ctx context.Context, scope models.Scope, activationID string, reason string) (int, error) {
	if err := models.ValidateRoutineEventScopeActivation(scope, activationID); err != nil {
		return 0, err
	}
	if !models.IsValidRoutineEventDropReason(reason) {
		return 0, fmt.Errorf("routine event dropped reason %q is invalid", reason)
	}
	var dropped int64
	err := s.withSQLiteWrite(ctx, func(conn *sql.Conn) error {
		result, err := conn.ExecContext(ctx, `UPDATE routine_events SET state = 'dropped', dropped_reason = ?, batch_id = NULL, payload = NULL
			WHERE scope_kind = ? AND scope_id = ? AND activation_id = ? AND state IN ('pending','batched')`, reason, scope.Kind, scope.ID, activationID)
		if err != nil {
			return err
		}
		if dropped, err = result.RowsAffected(); err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `DELETE FROM routine_event_batches
			WHERE scope_kind = ? AND scope_id = ? AND activation_id = ? AND started_at IS NULL`, scope.Kind, scope.ID, activationID)
		return err
	})
	return int(dropped), err
}

func (s *Store) RoutineEventActivity(ctx context.Context, scope models.Scope, activationIDs []string, now time.Time) (map[string]models.RoutineEventActivity, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	ids := models.UniqueRoutineActivationIDs(activationIDs)
	result := make(map[string]models.RoutineEventActivity, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	for _, id := range ids {
		result[id] = models.RoutineEventActivity{ActivationID: id}
	}
	dayAgo, hourAgo := sqliteRoutineTime(now.Add(-24*time.Hour)), sqliteRoutineTime(now.Add(-time.Hour))
	in := sqlitePlaceholders(len(ids))
	idArgs := sqliteStringArgs(ids)
	oldestInHour := make(map[string]*time.Time)
	args := append([]interface{}{dayAgo, hourAgo, hourAgo, scope.Kind, scope.ID}, idArgs...)
	rows, err := s.db.QueryContext(ctx, `SELECT activation_id, MAX(started_at),
			SUM(CASE WHEN started_at >= ? THEN 1 ELSE 0 END), SUM(CASE WHEN started_at >= ? THEN 1 ELSE 0 END),
			MIN(CASE WHEN started_at >= ? THEN started_at END)
		FROM routine_event_batches
		WHERE scope_kind = ? AND scope_id = ? AND activation_id IN (`+in+`) AND started_at IS NOT NULL AND test = 0
		GROUP BY activation_id`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var last, oldest sql.NullString
		var day, hour int
		if err := rows.Scan(&id, &last, &day, &hour, &oldest); err != nil {
			rows.Close()
			return nil, err
		}
		activity := result[id]
		if activity.LastFiredAt, err = sqliteRoutineNullTime(last); err != nil {
			rows.Close()
			return nil, err
		}
		activity.FiredLast24h, activity.FiredLastHour = day, hour
		if oldestInHour[id], err = sqliteRoutineNullTime(oldest); err != nil {
			rows.Close()
			return nil, err
		}
		result[id] = activity
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	args = append([]interface{}{dayAgo, scope.Kind, scope.ID}, idArgs...)
	args = append(args, dayAgo)
	rows, err = s.db.QueryContext(ctx, `SELECT activation_id,
			SUM(CASE WHEN state IN ('pending','batched') THEN 1 ELSE 0 END), SUM(CASE WHEN state = 'dropped' AND received_at >= ? THEN 1 ELSE 0 END)
		FROM routine_events
		WHERE scope_kind = ? AND scope_id = ? AND activation_id IN (`+in+`)
			AND (state IN ('pending','batched') OR (state = 'dropped' AND received_at >= ?))
		GROUP BY activation_id`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var pending, dropped int
		if err := rows.Scan(&id, &pending, &dropped); err != nil {
			rows.Close()
			return nil, err
		}
		activity := result[id]
		activity.Pending, activity.DroppedLast24h = pending, dropped
		result[id] = activity
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	args = append([]interface{}{scope.Kind, scope.ID}, idArgs...)
	rows, err = s.db.QueryContext(ctx, `SELECT id, COALESCE(json_extract(payload, '$.task.event.maxPerHour'), 0) FROM runbook_activations
		WHERE scope_kind = ? AND scope_id = ? AND id IN (`+in+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var maxPerHour int
		if err := rows.Scan(&id, &maxPerHour); err != nil {
			return nil, err
		}
		activity := result[id]
		models.FinishRoutineEventActivity(&activity, maxPerHour, oldestInHour[id])
		result[id] = activity
	}
	return result, rows.Err()
}

func (s *Store) PruneRoutineEvents(ctx context.Context, before time.Time, limit int) (int, error) {
	limit = models.NormalizeRoutineEventLimit(limit, 10_000)
	cutoff := sqliteRoutineTime(before)
	result, err := s.db.ExecContext(ctx, `DELETE FROM routine_events WHERE rowid IN (
		SELECT rowid FROM routine_events WHERE state IN ('started','dropped') AND received_at < ? ORDER BY received_at LIMIT ?)`, cutoff, limit)
	if err != nil {
		return 0, err
	}
	events, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	removed := int(events)
	if removed >= limit {
		return removed, nil
	}
	// Candidate batches of missing or retired activations; the retirement
	// time is checked here from the activation payload.
	rows, err := s.db.QueryContext(ctx, `SELECT b.scope_kind, b.scope_id, b.id, a.id IS NULL, COALESCE(json_extract(a.payload, '$.updatedAt'), '')
		FROM routine_event_batches b
		LEFT JOIN runbook_activations a ON a.scope_kind = b.scope_kind AND a.scope_id = b.scope_id AND a.id = b.activation_id
		WHERE b.created_at < ? AND (a.id IS NULL OR a.status = 'retired')`, cutoff)
	if err != nil {
		return removed, err
	}
	type batchKey struct{ kind, scopeID, id string }
	gone := make([]batchKey, 0)
	for rows.Next() {
		var key batchKey
		var missing bool
		var updatedAt string
		if err := rows.Scan(&key.kind, &key.scopeID, &key.id, &missing, &updatedAt); err != nil {
			rows.Close()
			return removed, err
		}
		if !missing {
			retiredAt, err := time.Parse(time.RFC3339Nano, updatedAt)
			if err != nil || !retiredAt.Before(before) {
				continue
			}
		}
		gone = append(gone, key)
		if removed+len(gone) >= limit {
			break
		}
	}
	if err := rows.Close(); err != nil {
		return removed, err
	}
	for _, key := range gone {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM routine_event_batches WHERE scope_kind = ? AND scope_id = ? AND id = ?`, key.kind, key.scopeID, key.id); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

const sqliteHookColumns = `id, scope_kind, scope_id, activation_id, revision, path_token_enabled, created_at, rotated_at`

func scanSQLiteHook(row sqlstore.RowScanner) (*models.Hook, error) {
	var hook models.Hook
	var createdAt, rotatedAt string
	if err := row.Scan(&hook.ID, &hook.Scope.Kind, &hook.Scope.ID, &hook.ActivationID, &hook.Revision, &hook.PathTokenEnabled, &createdAt, &rotatedAt); err != nil {
		return nil, err
	}
	var err error
	if hook.CreatedAt, err = parseSQLiteRoutineTime(createdAt); err != nil {
		return nil, err
	}
	if hook.RotatedAt, err = parseSQLiteRoutineTime(rotatedAt); err != nil {
		return nil, err
	}
	return &hook, nil
}

func (s *Store) CreateHook(ctx context.Context, hook *models.Hook) (*models.Hook, error) {
	value, err := models.PrepareHook(hook)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO hooks (`+sqliteHookColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		value.ID, value.Scope.Kind, value.Scope.ID, value.ActivationID, value.Revision, value.PathTokenEnabled,
		sqliteRoutineTime(value.CreatedAt), sqliteRoutineTime(value.RotatedAt)); err != nil {
		return nil, err
	}
	stored, err := s.GetHook(ctx, value.ID)
	if err != nil {
		return nil, err
	}
	if stored == nil || !models.SameHookOwner(stored, value) {
		return nil, models.ErrHookConflict
	}
	return stored, nil
}

func (s *Store) GetHook(ctx context.Context, hookID string) (*models.Hook, error) {
	hook, err := scanSQLiteHook(s.db.QueryRowContext(ctx, `SELECT `+sqliteHookColumns+` FROM hooks WHERE id = ?`, hookID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return hook, err
}

func (s *Store) UpdateHook(ctx context.Context, scope models.Scope, hookID string, expectedRevision int64, rotate bool, pathTokenEnabled *bool, now time.Time) (*models.Hook, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	var pathToken sql.NullBool
	if pathTokenEnabled != nil {
		pathToken = sql.NullBool{Bool: *pathTokenEnabled, Valid: true}
	}
	var updated *models.Hook
	err := s.withSQLiteWrite(ctx, func(conn *sql.Conn) error {
		current, err := scanSQLiteHook(conn.QueryRowContext(ctx, `SELECT `+sqliteHookColumns+` FROM hooks WHERE id = ?`, hookID))
		if errors.Is(err, sql.ErrNoRows) || err == nil && current.Scope != scope {
			return models.ErrHookNotFound
		}
		if err != nil {
			return err
		}
		if current.Revision != expectedRevision {
			return models.ErrHookRevision
		}
		if rotate {
			current.Revision++
			current.RotatedAt = now.UTC()
		}
		if pathToken.Valid {
			current.PathTokenEnabled = pathToken.Bool
		}
		if _, err := conn.ExecContext(ctx, `UPDATE hooks SET revision = ?, rotated_at = ?, path_token_enabled = ? WHERE id = ? AND revision = ?`,
			current.Revision, sqliteRoutineTime(current.RotatedAt), current.PathTokenEnabled, hookID, expectedRevision); err != nil {
			return err
		}
		updated = current
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *Store) PruneHooks(ctx context.Context, createdBefore time.Time) (int, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM hooks
		WHERE created_at < ? AND NOT EXISTS (
			SELECT 1 FROM runbook_activations a
			WHERE a.scope_kind = hooks.scope_kind AND a.scope_id = hooks.scope_id AND a.id = hooks.activation_id AND a.status <> 'retired')`,
		sqliteRoutineTime(createdBefore))
	if err != nil {
		return 0, err
	}
	removed, err := result.RowsAffected()
	return int(removed), err
}

var _ models.RoutineEventStore = (*Store)(nil)
