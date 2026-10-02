package runtime

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// runEventRetentionPosition is operator maintenance state, not an authority or
// a tenant resource. Its ordered key is identical to the global received index.
// A sweep advances past protected rows so a paused Run cannot starve cleanup.
type runEventRetentionPosition struct {
	ReceivedAt time.Time `json:"receivedAt"`
	ScopeKind  string    `json:"scopeKind"`
	ScopeID    string    `json:"scopeId"`
	Source     string    `json:"source"`
	EventID    string    `json:"eventId"`
}

// Internal maintenance never shortens the retention contract, including when a
// host clock or a caller supplies an unsafe cutoff. The batch limit bounds rows
// inspected, not merely rows deleted. A zero result can mean protected rows
// were inspected; callers must keep scheduling maintenance rather than treating
// zero as completion of the sweep.
func validateRunEventRetention(before time.Time, limit int) error {
	if before.IsZero() || !runEventNanoTimeValid(before) || before.After(time.Now().UTC().Add(-RunEventRetention)) || limit < 1 || limit > 1000 {
		return ErrInvalidRunEventWait
	}
	return nil
}

func (s *MemoryStore) PruneExpiredRunEvents(ctx context.Context, before time.Time, limit int) (int, error) {
	if err := validateRunEventRetention(before, limit); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	candidates := s.memoryRunEventReceiptsAfterLocked(s.runEventRetentionCursor, before.UTC(), limit, nil)
	deletable := make([]memoryRunEventReceiptKey, 0, len(candidates))
	for _, candidate := range candidates {
		receipt := s.runEventReceipts[candidate.Identity]
		if !s.memoryRunEventReceiptProtectedLocked(receipt) {
			deletable = append(deletable, candidate.Identity)
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	for _, identity := range deletable {
		s.deleteMemoryRunEventReceiptLocked(identity)
	}
	s.runEventRetentionCursor = nil
	if len(candidates) == limit {
		last := candidates[len(candidates)-1]
		s.runEventRetentionCursor = &last
	}
	return len(deletable), nil
}

func migrateRunEventRetentionSQLite(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS run_event_retention_cursor (
			id INTEGER PRIMARY KEY CHECK(id=1), received_at DATETIME,
			scope_kind TEXT NOT NULL DEFAULT '', scope_id TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT '', event_id TEXT NOT NULL DEFAULT ''
		);
		INSERT OR IGNORE INTO run_event_retention_cursor(id) VALUES(1);
		CREATE INDEX IF NOT EXISTS idx_run_event_inbox_received
			ON run_event_inbox(received_at,scope_kind,scope_id,source,event_id);
	`)
	return err
}

func (s *SQLiteStore) PruneExpiredRunEvents(ctx context.Context, before time.Time, limit int) (int, error) {
	if err := validateRunEventRetention(before, limit); err != nil {
		return 0, err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return 0, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	var at sql.NullTime
	var cursor runEventRetentionPosition
	if err = conn.QueryRowContext(ctx, `SELECT received_at,scope_kind,scope_id,source,event_id
		FROM run_event_retention_cursor WHERE id=1`).Scan(&at, &cursor.ScopeKind, &cursor.ScopeID, &cursor.Source, &cursor.EventID); err != nil {
		return 0, err
	}
	var position *runEventRetentionPosition
	if at.Valid {
		cursor.ReceivedAt = at.Time.UTC()
		position = &cursor
	}
	count, next, err := pruneSQLiteRunEventBatch(ctx, conn, nil, before.UTC(), position, limit)
	if err != nil {
		return 0, err
	}
	var lastReceived interface{}
	last := runEventRetentionPosition{}
	if next != nil {
		last = *next
		lastReceived = last.ReceivedAt.UTC()
	}
	if _, err = conn.ExecContext(ctx, `UPDATE run_event_retention_cursor SET received_at=?,scope_kind=?,scope_id=?,source=?,event_id=? WHERE id=1`,
		lastReceived, last.ScopeKind, last.ScopeID, last.Source, last.EventID); err != nil {
		return 0, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, err
	}
	committed = true
	return int(count), nil
}

func (s *PostgresStore) migrateRunEventRetentionPostgres(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("run_event_retention_cursor")+` (
			id SMALLINT PRIMARY KEY CHECK(id=1), received_at BIGINT,
			scope_kind TEXT NOT NULL DEFAULT '', scope_id TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT '', event_id TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO `+s.table("run_event_retention_cursor")+`(id) VALUES(1) ON CONFLICT(id) DO NOTHING;
		CREATE INDEX IF NOT EXISTS run_event_inbox_received_idx ON `+s.table("run_event_inbox")+`
			(received_at,scope_kind,scope_id,source,event_id);
		INSERT INTO `+s.table("schema_migrations")+` (version,name)
			VALUES(49,'bounded run event retention cursor') ON CONFLICT(version) DO NOTHING;
	`)
	return err
}

func (s *PostgresStore) PruneExpiredRunEvents(ctx context.Context, before time.Time, limit int) (int, error) {
	if err := validateRunEventRetention(before, limit); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var at sql.NullInt64
	var cursor runEventRetentionPosition
	err = tx.QueryRowContext(ctx, `SELECT received_at,scope_kind,scope_id,source,event_id FROM `+s.table("run_event_retention_cursor")+`
		WHERE id=1 FOR UPDATE SKIP LOCKED`).Scan(&at, &cursor.ScopeKind, &cursor.ScopeID, &cursor.Source, &cursor.EventID)
	if errors.Is(err, sql.ErrNoRows) {
		// Another replica owns this maintenance batch; no work is stolen.
		return 0, tx.Commit()
	}
	if err != nil {
		return 0, err
	}
	query := `WITH page AS MATERIALIZED (
		SELECT inbox.scope_kind,inbox.scope_id,inbox.source,inbox.event_id,inbox.received_at
		FROM ` + s.table("run_event_inbox") + ` AS inbox WHERE inbox.received_at < $1`
	args := []interface{}{before.UTC().UnixNano()}
	if at.Valid {
		query += ` AND (inbox.received_at,inbox.scope_kind,inbox.scope_id,inbox.source,inbox.event_id)>($2,$3,$4,$5,$6)`
		args = append(args, at.Int64, cursor.ScopeKind, cursor.ScopeID, cursor.Source, cursor.EventID)
		query += ` ORDER BY inbox.received_at,inbox.scope_kind,inbox.scope_id,inbox.source,inbox.event_id LIMIT $7`
	} else {
		query += ` ORDER BY inbox.received_at,inbox.scope_kind,inbox.scope_id,inbox.source,inbox.event_id LIMIT $2`
	}
	args = append(args, limit)
	query += `
	), candidates AS MATERIALIZED (
		SELECT inbox.scope_kind,inbox.scope_id,inbox.source,inbox.event_id,inbox.received_at
		FROM ` + s.table("run_event_inbox") + ` AS inbox JOIN page
		ON inbox.scope_kind=page.scope_kind AND inbox.scope_id=page.scope_id
		AND inbox.source=page.source AND inbox.event_id=page.event_id
		FOR UPDATE OF inbox SKIP LOCKED
	), deleted AS (
		DELETE FROM ` + s.table("run_event_inbox") + ` AS inbox USING candidates
		WHERE inbox.scope_kind=candidates.scope_kind AND inbox.scope_id=candidates.scope_id
		AND inbox.source=candidates.source AND inbox.event_id=candidates.event_id
		AND NOT EXISTS (SELECT 1 FROM ` + s.table("run_event_waits") + ` AS waits
			WHERE waits.scope_kind=inbox.scope_kind AND waits.scope_id=inbox.scope_id AND waits.source=inbox.source
			AND waits.event_type=inbox.event_type AND waits.subject=inbox.subject AND waits.status IN ('pending','paused')
			AND waits.after_at<=inbox.occurred_at AND waits.deadline>=inbox.received_at AND inbox.attributes @> waits.attributes
			AND NOT EXISTS (SELECT 1 FROM jsonb_each(waits.attributes) AS expected
				WHERE jsonb_typeof(inbox.attributes->expected.key) IS DISTINCT FROM jsonb_typeof(expected.value))
			AND (left(inbox.source,8)<>'binding:' OR inbox.attributes->>'deploymentId'=waits.payload->>'assignedAgentId')
			AND NOT EXISTS (SELECT 1 FROM ` + s.table("run_event_consumptions") + ` AS consumed
				WHERE consumed.scope_kind=waits.scope_kind AND consumed.scope_id=waits.scope_id
				AND consumed.run_id=waits.run_id AND consumed.source=inbox.source AND consumed.event_id=inbox.event_id))
		RETURNING 1
	), last_candidate AS (
		SELECT * FROM page ORDER BY received_at DESC,scope_kind DESC,scope_id DESC,source DESC,event_id DESC LIMIT 1
	) SELECT (SELECT COUNT(*) FROM page),(SELECT COUNT(*) FROM deleted),
		last_candidate.received_at,COALESCE(last_candidate.scope_kind,''),COALESCE(last_candidate.scope_id,''),
		COALESCE(last_candidate.source,''),COALESCE(last_candidate.event_id,'')
		FROM (SELECT 1) AS singleton LEFT JOIN last_candidate ON TRUE`
	var inspected, deleted int
	var lastReceived sql.NullInt64
	var last runEventRetentionPosition
	if err = tx.QueryRowContext(ctx, query, args...).Scan(&inspected, &deleted, &lastReceived, &last.ScopeKind, &last.ScopeID, &last.Source, &last.EventID); err != nil {
		return 0, err
	}
	var nextReceived interface{}
	if inspected == limit {
		nextReceived = lastReceived.Int64
	} else {
		last = runEventRetentionPosition{}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE `+s.table("run_event_retention_cursor")+`
		SET received_at=$1,scope_kind=$2,scope_id=$3,source=$4,event_id=$5 WHERE id=1`,
		nextReceived, last.ScopeKind, last.ScopeID, last.Source, last.EventID); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}
