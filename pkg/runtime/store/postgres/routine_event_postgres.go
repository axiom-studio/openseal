package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
	"github.com/lib/pq"
)

// Queries shared by the store and its EXPLAIN test, so the test checks the
// plans of the real statements.

func (s *Store) routineEventActivationsQuery() string {
	return `SELECT payload::text FROM ` + s.table("runbook_activations") + `
		WHERE scope_kind = $1 AND scope_id = $2 AND event_kind = $3 AND event_source_ref = $4
			AND status = 'active' AND event_kind IS NOT NULL ORDER BY id`
}

func (s *Store) routineEventSourceKeysQuery() string {
	return `SELECT DISTINCT event_kind, event_source_ref FROM ` + s.table("runbook_activations") + `
		WHERE scope_kind = $1 AND scope_id = $2 AND status = 'active' AND event_kind IS NOT NULL ORDER BY 1, 2`
}

func (s *Store) activeRoutineEventRunsQuery() string {
	return `SELECT COUNT(*) FROM ` + s.table("routine_event_batches") + ` b
		JOIN ` + s.table("agent_runs") + ` r ON r.scope_kind = b.scope_kind AND r.scope_id = b.scope_id AND r.id = b.run_id
		WHERE b.scope_kind = $1 AND b.scope_id = $2 AND b.started_at >= $3 AND r.status NOT IN ('completed','failed','canceled')`
}

func (s *Store) pendingRoutineEventsQuery() string {
	return `SELECT COUNT(*) FROM ` + s.table("routine_events") + `
		WHERE scope_kind = $1 AND scope_id = $2 AND activation_id = $3 AND state IN ('pending','batched')`
}

func (s *Store) pendingRoutineEventsInScopeQuery() string {
	return `SELECT COUNT(*) FROM ` + s.table("routine_events") + `
		WHERE scope_kind = $1 AND scope_id = $2 AND state IN ('pending','batched')`
}

func (s *Store) routineEventWorkQuery() string {
	return `SELECT DISTINCT activation_id FROM ` + s.table("routine_events") + `
		WHERE scope_kind = $1 AND scope_id = $2 AND state IN ('pending','batched') AND activation_id > $3 ORDER BY activation_id LIMIT $4`
}

func (s *Store) routineEventWorkScopesQuery() string {
	return `SELECT DISTINCT scope_kind, scope_id FROM ` + s.table("routine_events") + `
		WHERE state IN ('pending','batched') ORDER BY scope_kind, scope_id`
}

func (s *Store) pruneRoutineEventsQuery() string {
	return `DELETE FROM ` + s.table("routine_events") + ` WHERE ctid IN (
		SELECT ctid FROM ` + s.table("routine_events") + `
		WHERE state IN ('started','dropped') AND received_at < $1 ORDER BY received_at LIMIT $2)`
}

func (s *Store) ListRoutineEventActivations(ctx context.Context, scope models.Scope, kind models.RoutineEventKind, sourceRef string) ([]*models.RunbookActivation, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, s.routineEventActivationsQuery(), scope.Kind, scope.ID, string(kind), sourceRef)
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
	query := `SELECT payload::text FROM ` + s.table("runbook_activations") + `
		WHERE status = 'active' AND event_kind IS NOT NULL AND (cardinality($1::text[]) = 0 OR event_kind = ANY($1::text[]))
			AND ($2 = '' OR (scope_kind, scope_id, id) > ($3, $4, $5))
		ORDER BY scope_kind, scope_id, id LIMIT $6`
	rows, err := s.db.QueryContext(ctx, query, pq.Array(kindValues), after, afterKind, afterScope, afterID, limit)
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
	rows, err := s.db.QueryContext(ctx, s.routineEventSourceKeysQuery(), scope.Kind, scope.ID)
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
	err := s.db.QueryRowContext(ctx, s.activeRoutineEventRunsQuery(), scope.Kind, scope.ID, time.Now().Add(-models.RoutineEventActiveRunWindow).UTC()).Scan(&count)
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
	result, err := s.db.ExecContext(ctx, `INSERT INTO `+s.table("routine_events")+`
		(scope_kind, scope_id, activation_id, event_id, state, dropped_reason, test, received_at, occurred_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb)
		ON CONFLICT (scope_kind, scope_id, activation_id, event_id) DO NOTHING`,
		record.Scope.Kind, record.Scope.ID, record.ActivationID, record.Event.ID, string(record.State), droppedReason,
		record.Event.Test, record.ReceivedAt, record.Event.OccurredAt.UTC(), payloadValue)
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
	err := s.db.QueryRowContext(ctx, s.pendingRoutineEventsQuery(), scope.Kind, scope.ID, activationID).Scan(&count)
	return count, err
}

func (s *Store) CountPendingRoutineEventsInScope(ctx context.Context, scope models.Scope) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	var count int
	err := s.db.QueryRowContext(ctx, s.pendingRoutineEventsInScopeQuery(), scope.Kind, scope.ID).Scan(&count)
	return count, err
}

func (s *Store) ListRoutineEventWorkScopes(ctx context.Context) ([]models.Scope, error) {
	rows, err := s.db.QueryContext(ctx, s.routineEventWorkScopesQuery())
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
	rows, err := s.db.QueryContext(ctx, s.routineEventWorkQuery(), scope.Kind, scope.ID, after, models.NormalizeRoutineEventLimit(limit, models.RoutineEventSubscriptionPageLimit))
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

const postgresRoutineBatchColumns = `scope_kind, scope_id, id, activation_id, event_ids::text, dropped, test, COALESCE(run_id, ''), created_at, started_at`

func scanPostgresRoutineEventBatch(row sqlstore.RowScanner) (*models.RoutineEventBatch, error) {
	var batch models.RoutineEventBatch
	var eventIDs string
	var startedAt sql.NullTime
	if err := row.Scan(&batch.Scope.Kind, &batch.Scope.ID, &batch.ID, &batch.ActivationID, &eventIDs, &batch.Dropped, &batch.Test, &batch.RunID, &batch.CreatedAt, &startedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(eventIDs), &batch.EventIDs); err != nil {
		return nil, err
	}
	batch.CreatedAt = batch.CreatedAt.UTC()
	if startedAt.Valid {
		started := startedAt.Time.UTC()
		batch.StartedAt = &started
	}
	return &batch, nil
}

func (s *Store) openRoutineEventBatch(ctx context.Context, query sqlstore.Reader, scope models.Scope, activationID string) (*models.RoutineEventBatch, error) {
	batch, err := scanPostgresRoutineEventBatch(query.QueryRowContext(ctx, `SELECT `+postgresRoutineBatchColumns+` FROM `+s.table("routine_event_batches")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND activation_id = $3 AND started_at IS NULL`, scope.Kind, scope.ID, activationID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return batch, err
}

// lockRoutineActivationEvents serializes claims and drops of one
// activation's events across processes for the rest of the transaction.
func (s *Store) lockRoutineActivationEvents(ctx context.Context, tx *sql.Tx, scope models.Scope, activationID string) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`,
		"openseal:routine-events:"+s.schema+"|"+scope.Kind+"|"+scope.ID+"|"+activationID)
	return err
}

func (s *Store) ClaimRoutineEventBatch(ctx context.Context, scope models.Scope, activationID string, maxEvents int, now time.Time) (*models.RoutineEventBatch, error) {
	if err := models.ValidateRoutineEventScopeActivation(scope, activationID); err != nil {
		return nil, err
	}
	maxEvents = models.NormalizeRoutineEventBatchSize(maxEvents)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := s.lockRoutineActivationEvents(ctx, tx, scope, activationID); err != nil {
		return nil, err
	}
	if open, err := s.openRoutineEventBatch(ctx, tx, scope, activationID); err != nil || open != nil {
		if err != nil {
			return nil, err
		}
		return open, tx.Commit()
	}
	pending := func(test bool) ([]string, error) {
		rows, err := tx.QueryContext(ctx, `SELECT event_id FROM `+s.table("routine_events")+`
			WHERE scope_kind = $1 AND scope_id = $2 AND activation_id = $3 AND state = 'pending' AND test = $4
			ORDER BY received_at, event_id LIMIT $5 FOR UPDATE SKIP LOCKED`, scope.Kind, scope.ID, activationID, test, maxEvents)
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
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	batch := &models.RoutineEventBatch{Scope: scope, ID: models.NewRoutineEventBatchID(), ActivationID: activationID, EventIDs: ids, Test: test, CreatedAt: now.UTC()}
	if !test {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+s.table("routine_events")+`
			WHERE scope_kind = $1 AND scope_id = $2 AND activation_id = $3 AND state = 'dropped' AND dropped_reason = 'overflow' AND NOT test
				AND received_at > COALESCE((SELECT MAX(created_at) FROM `+s.table("routine_event_batches")+`
					WHERE scope_kind = $1 AND scope_id = $2 AND activation_id = $3 AND NOT test), '-infinity'::timestamptz)`,
			scope.Kind, scope.ID, activationID).Scan(&batch.Dropped); err != nil {
			return nil, err
		}
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("routine_event_batches")+`
		(scope_kind, scope_id, id, activation_id, event_ids, dropped, test, created_at) VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8)`,
		scope.Kind, scope.ID, batch.ID, activationID, string(encoded), batch.Dropped, test, batch.CreatedAt); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE `+s.table("routine_events")+` SET state = 'batched', batch_id = $4
		WHERE scope_kind = $1 AND scope_id = $2 AND activation_id = $3 AND event_id = ANY($5::text[])`,
		scope.Kind, scope.ID, activationID, batch.ID, pq.Array(ids)); err != nil {
		return nil, err
	}
	return batch, tx.Commit()
}

func (s *Store) MarkRoutineEventBatchStarted(ctx context.Context, scope models.Scope, batchID, runID string, now time.Time) error {
	if err := models.ValidateRoutineEventScopeActivation(scope, batchID); err != nil {
		return err
	}
	if runID == "" {
		return errors.New("a started routine event batch needs its Run")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	batch, err := scanPostgresRoutineEventBatch(tx.QueryRowContext(ctx, `SELECT `+postgresRoutineBatchColumns+` FROM `+s.table("routine_event_batches")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND id = $3 FOR UPDATE`, scope.Kind, scope.ID, batchID))
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
	if _, err := tx.ExecContext(ctx, `UPDATE `+s.table("routine_event_batches")+` SET run_id = $4, started_at = $5
		WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`, scope.Kind, scope.ID, batchID, runID, now.UTC()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE `+s.table("routine_events")+` SET state = 'started'
		WHERE scope_kind = $1 AND scope_id = $2 AND activation_id = $3 AND event_id = ANY($4::text[]) AND state = 'batched'`,
		scope.Kind, scope.ID, batch.ActivationID, pq.Array(batch.EventIDs)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetRoutineEventBatch(ctx context.Context, scope models.Scope, batchID string) (*models.RoutineEventBatch, []*models.RoutineEventRecord, error) {
	if err := models.ValidateRoutineEventScopeActivation(scope, batchID); err != nil {
		return nil, nil, err
	}
	batch, err := scanPostgresRoutineEventBatch(s.db.QueryRowContext(ctx, `SELECT `+postgresRoutineBatchColumns+` FROM `+s.table("routine_event_batches")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`, scope.Kind, scope.ID, batchID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_id, state, COALESCE(batch_id, ''), COALESCE(dropped_reason, ''), test, received_at, occurred_at, payload::text
		FROM `+s.table("routine_events")+` WHERE scope_kind = $1 AND scope_id = $2 AND activation_id = $3 AND event_id = ANY($4::text[])`,
		scope.Kind, scope.ID, batch.ActivationID, pq.Array(batch.EventIDs))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	byID := make(map[string]*models.RoutineEventRecord, len(batch.EventIDs))
	for rows.Next() {
		record := &models.RoutineEventRecord{Scope: scope, ActivationID: batch.ActivationID}
		var eventID string
		var test bool
		var occurredAt time.Time
		var payload sql.NullString
		if err := rows.Scan(&eventID, &record.State, &record.BatchID, &record.DroppedReason, &test, &record.ReceivedAt, &occurredAt, &payload); err != nil {
			return nil, nil, err
		}
		record.ReceivedAt = record.ReceivedAt.UTC()
		if err := sqlstore.DecodeRoutineEventRecordEvent(record, []byte(payload.String), eventID, test, occurredAt); err != nil {
			return nil, nil, err
		}
		byID[eventID] = record
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	records := make([]*models.RoutineEventRecord, 0, len(batch.EventIDs))
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
	return s.openRoutineEventBatch(ctx, s.db, scope, activationID)
}

func (s *Store) CountRoutineEventBatchesSince(ctx context.Context, scope models.Scope, activationID string, since time.Time, test bool) (int, *time.Time, error) {
	if err := models.ValidateRoutineEventScopeActivation(scope, activationID); err != nil {
		return 0, nil, err
	}
	var count int
	var oldest sql.NullTime
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(started_at) FROM `+s.table("routine_event_batches")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND activation_id = $3 AND started_at >= $4 AND test = $5`,
		scope.Kind, scope.ID, activationID, since.UTC(), test).Scan(&count, &oldest); err != nil {
		return 0, nil, err
	}
	if !oldest.Valid {
		return count, nil, nil
	}
	value := oldest.Time.UTC()
	return count, &value, nil
}

func (s *Store) CountRoutineTestEventsSince(ctx context.Context, scope models.Scope, activationID string, since time.Time) (int, error) {
	if err := models.ValidateRoutineEventScopeActivation(scope, activationID); err != nil {
		return 0, err
	}
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+s.table("routine_events")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND activation_id = $3 AND test AND received_at >= $4`,
		scope.Kind, scope.ID, activationID, since.UTC()).Scan(&count)
	return count, err
}

func (s *Store) DropRoutineEvents(ctx context.Context, scope models.Scope, activationID string, reason string) (int, error) {
	if err := models.ValidateRoutineEventScopeActivation(scope, activationID); err != nil {
		return 0, err
	}
	if !models.IsValidRoutineEventDropReason(reason) {
		return 0, fmt.Errorf("routine event dropped reason %q is invalid", reason)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := s.lockRoutineActivationEvents(ctx, tx, scope, activationID); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("routine_events")+`
		SET state = 'dropped', dropped_reason = $4, batch_id = NULL, payload = NULL
		WHERE scope_kind = $1 AND scope_id = $2 AND activation_id = $3 AND state IN ('pending','batched')`,
		scope.Kind, scope.ID, activationID, reason)
	if err != nil {
		return 0, err
	}
	dropped, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+s.table("routine_event_batches")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND activation_id = $3 AND started_at IS NULL`, scope.Kind, scope.ID, activationID); err != nil {
		return 0, err
	}
	return int(dropped), tx.Commit()
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
	dayAgo, hourAgo := now.Add(-24*time.Hour).UTC(), now.Add(-time.Hour).UTC()
	oldestInHour := make(map[string]*time.Time)
	rows, err := s.db.QueryContext(ctx, `SELECT activation_id, MAX(started_at),
			COUNT(*) FILTER (WHERE started_at >= $4), COUNT(*) FILTER (WHERE started_at >= $5), MIN(started_at) FILTER (WHERE started_at >= $5)
		FROM `+s.table("routine_event_batches")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND activation_id = ANY($3::text[]) AND started_at IS NOT NULL AND NOT test
		GROUP BY activation_id`, scope.Kind, scope.ID, pq.Array(ids), dayAgo, hourAgo)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var last, oldest sql.NullTime
		var day, hour int
		if err := rows.Scan(&id, &last, &day, &hour, &oldest); err != nil {
			rows.Close()
			return nil, err
		}
		activity := result[id]
		if last.Valid {
			value := last.Time.UTC()
			activity.LastFiredAt = &value
		}
		activity.FiredLast24h, activity.FiredLastHour = day, hour
		if oldest.Valid {
			value := oldest.Time.UTC()
			oldestInHour[id] = &value
		}
		result[id] = activity
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT activation_id,
			COUNT(*) FILTER (WHERE state IN ('pending','batched')), COUNT(*) FILTER (WHERE state = 'dropped' AND received_at >= $4)
		FROM `+s.table("routine_events")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND activation_id = ANY($3::text[])
			AND (state IN ('pending','batched') OR (state = 'dropped' AND received_at >= $4))
		GROUP BY activation_id`, scope.Kind, scope.ID, pq.Array(ids), dayAgo)
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
	rows, err = s.db.QueryContext(ctx, `SELECT id, COALESCE((payload->'task'->'event'->>'maxPerHour')::int, 0) FROM `+s.table("runbook_activations")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND id = ANY($3::text[])`, scope.Kind, scope.ID, pq.Array(ids))
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
	result, err := s.db.ExecContext(ctx, s.pruneRoutineEventsQuery(), before.UTC(), limit)
	if err != nil {
		return 0, err
	}
	events, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if int(events) >= limit {
		return int(events), nil
	}
	result, err = s.db.ExecContext(ctx, `DELETE FROM `+s.table("routine_event_batches")+` WHERE (scope_kind, scope_id, id) IN (
		SELECT b.scope_kind, b.scope_id, b.id FROM `+s.table("routine_event_batches")+` b
		LEFT JOIN `+s.table("runbook_activations")+` a ON a.scope_kind = b.scope_kind AND a.scope_id = b.scope_id AND a.id = b.activation_id
		WHERE b.created_at < $1 AND (a.id IS NULL OR (a.status = 'retired' AND a.updated_at < $1)) LIMIT $2)`,
		before.UTC(), limit-int(events))
	if err != nil {
		return int(events), err
	}
	batches, err := result.RowsAffected()
	return int(events + batches), err
}

const postgresHookColumns = `id, scope_kind, scope_id, activation_id, revision, path_token_enabled, created_at, rotated_at`

func scanPostgresHook(row sqlstore.RowScanner) (*models.Hook, error) {
	var hook models.Hook
	if err := row.Scan(&hook.ID, &hook.Scope.Kind, &hook.Scope.ID, &hook.ActivationID, &hook.Revision, &hook.PathTokenEnabled, &hook.CreatedAt, &hook.RotatedAt); err != nil {
		return nil, err
	}
	hook.CreatedAt, hook.RotatedAt = hook.CreatedAt.UTC(), hook.RotatedAt.UTC()
	return &hook, nil
}

func (s *Store) CreateHook(ctx context.Context, hook *models.Hook) (*models.Hook, error) {
	value, err := models.PrepareHook(hook)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO `+s.table("hooks")+` (`+postgresHookColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING`,
		value.ID, value.Scope.Kind, value.Scope.ID, value.ActivationID, value.Revision, value.PathTokenEnabled, value.CreatedAt, value.RotatedAt); err != nil {
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
	hook, err := scanPostgresHook(s.db.QueryRowContext(ctx, `SELECT `+postgresHookColumns+` FROM `+s.table("hooks")+` WHERE id = $1`, hookID))
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
	hook, err := scanPostgresHook(s.db.QueryRowContext(ctx, `UPDATE `+s.table("hooks")+` SET
			revision = revision + CASE WHEN $5 THEN 1 ELSE 0 END,
			rotated_at = CASE WHEN $5 THEN $6 ELSE rotated_at END,
			path_token_enabled = COALESCE($7, path_token_enabled)
		WHERE id = $1 AND scope_kind = $2 AND scope_id = $3 AND revision = $4
		RETURNING `+postgresHookColumns, hookID, scope.Kind, scope.ID, expectedRevision, rotate, now.UTC(), pathToken))
	if errors.Is(err, sql.ErrNoRows) {
		existing, getErr := s.GetHook(ctx, hookID)
		if getErr != nil {
			return nil, getErr
		}
		if existing == nil || existing.Scope != scope {
			return nil, models.ErrHookNotFound
		}
		return nil, models.ErrHookRevision
	}
	return hook, err
}

func (s *Store) PruneHooks(ctx context.Context, createdBefore time.Time) (int, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM `+s.table("hooks")+` h
		WHERE h.created_at < $1 AND NOT EXISTS (
			SELECT 1 FROM `+s.table("runbook_activations")+` a
			WHERE a.scope_kind = h.scope_kind AND a.scope_id = h.scope_id AND a.id = h.activation_id AND a.status <> 'retired')`,
		createdBefore.UTC())
	if err != nil {
		return 0, err
	}
	removed, err := result.RowsAffected()
	return int(removed), err
}

var _ models.RoutineEventStore = (*Store)(nil)
