package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

func migrateScopedRunEventRetentionSQLite(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS run_event_retention_scopes (
			scope_kind TEXT NOT NULL,scope_id TEXT NOT NULL,received_at DATETIME,
			source TEXT NOT NULL DEFAULT '',event_id TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(scope_kind,scope_id)
		);
		CREATE INDEX IF NOT EXISTS idx_run_event_inbox_scope_received
			ON run_event_inbox(scope_kind,scope_id,received_at,source,event_id);
	`)
	return err
}

// The scoped API retains its explicit-clock contract for replay and tests.
// Ordinary operator maintenance uses the stricter real-clock global method.
func pruneSQLiteRunEventScope(ctx context.Context, db *sql.DB, scope Scope, now time.Time, limit int) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	if now.IsZero() || !runEventNanoTimeValid(now) || limit < 1 || limit > 1000 {
		return 0, ErrInvalidRunEventWait
	}
	conn, err := beginImmediateSQLite(ctx, db)
	if err != nil {
		return 0, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	if _, err = conn.ExecContext(ctx, `INSERT OR IGNORE INTO run_event_retention_scopes(scope_kind,scope_id) VALUES(?,?)`, scope.Kind, scope.ID); err != nil {
		return 0, err
	}
	var at sql.NullTime
	cursor := runEventRetentionPosition{ScopeKind: scope.Kind, ScopeID: scope.ID}
	if err = conn.QueryRowContext(ctx, `SELECT received_at,source,event_id FROM run_event_retention_scopes WHERE scope_kind=? AND scope_id=?`,
		scope.Kind, scope.ID).Scan(&at, &cursor.Source, &cursor.EventID); err != nil {
		return 0, err
	}
	var position *runEventRetentionPosition
	if at.Valid {
		cursor.ReceivedAt = at.Time.UTC()
		position = &cursor
	}
	count, next, err := pruneSQLiteRunEventBatch(ctx, conn, &scope, now.UTC().Add(-RunEventRetention), position, limit)
	if err != nil {
		return 0, err
	}
	var nextAt interface{}
	cursor.Source, cursor.EventID = "", ""
	if next != nil {
		nextAt, cursor.Source, cursor.EventID = next.ReceivedAt.UTC(), next.Source, next.EventID
	}
	if _, err = conn.ExecContext(ctx, `UPDATE run_event_retention_scopes SET received_at=?,source=?,event_id=? WHERE scope_kind=? AND scope_id=?`,
		nextAt, cursor.Source, cursor.EventID, scope.Kind, scope.ID); err != nil {
		return 0, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, err
	}
	committed = true
	return count, nil
}

// Caller owns the immediate transaction and persists its cursor in that same
// transaction. Optional scope uses a separate scoped index and cursor. Both
// paths inspect at most limit inbox keys before checking exact wait protection.
func pruneSQLiteRunEventBatch(ctx context.Context, conn *sql.Conn, scope *Scope, before time.Time, cursor *runEventRetentionPosition, limit int) (int, *runEventRetentionPosition, error) {
	query := `SELECT received_at,scope_kind,scope_id,source,event_id FROM run_event_inbox WHERE `
	args := make([]interface{}, 0, 8)
	if scope != nil {
		query += `scope_kind=? AND scope_id=? AND `
		args = append(args, scope.Kind, scope.ID)
	}
	query += `received_at<?`
	args = append(args, before.UTC())
	order := `received_at,scope_kind,scope_id,source,event_id`
	if scope != nil {
		order = `received_at,source,event_id`
	}
	if cursor != nil {
		if scope != nil {
			query += ` AND (received_at,source,event_id)>(?,?,?)`
			args = append(args, cursor.ReceivedAt.UTC(), cursor.Source, cursor.EventID)
		} else {
			query += ` AND (received_at,scope_kind,scope_id,source,event_id)>(?,?,?,?,?)`
			args = append(args, cursor.ReceivedAt.UTC(), cursor.ScopeKind, cursor.ScopeID, cursor.Source, cursor.EventID)
		}
	}
	query += ` ORDER BY ` + order + ` LIMIT ?`
	args = append(args, limit)
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, nil, err
	}
	candidates := make([]runEventRetentionPosition, 0, limit)
	for rows.Next() {
		var candidate runEventRetentionPosition
		if err = rows.Scan(&candidate.ReceivedAt, &candidate.ScopeKind, &candidate.ScopeID, &candidate.Source, &candidate.EventID); err != nil {
			_ = rows.Close()
			return 0, nil, err
		}
		candidates = append(candidates, candidate)
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return 0, nil, err
	}
	count := int64(0)
	if len(candidates) > 0 {
		// A bounded JSON batch avoids SQLite's variable limit. Every lookup
		// still uses the entire scope-qualified event primary key.
		encoded, err := json.Marshal(candidates)
		if err != nil {
			return 0, nil, err
		}
		result, err := conn.ExecContext(ctx, `DELETE FROM run_event_inbox WHERE rowid IN (
			SELECT e.rowid FROM json_each(?) candidate JOIN run_event_inbox e
			ON e.scope_kind=json_extract(candidate.value,'$.scopeKind')
			AND e.scope_id=json_extract(candidate.value,'$.scopeId')
			AND e.source=json_extract(candidate.value,'$.source') AND e.event_id=json_extract(candidate.value,'$.eventId')
			WHERE NOT EXISTS (SELECT 1 FROM run_event_waits w
				WHERE w.scope_kind=e.scope_kind AND w.scope_id=e.scope_id AND w.source=e.source
				AND w.event_type=e.type AND w.subject=e.subject AND w.status IN ('pending','paused')
				AND e.occurred_at>=w.after_at AND e.received_at<=w.deadline AND `+sqliteRunEventAttributesMatch+`
				AND NOT EXISTS (SELECT 1 FROM run_event_consumptions c
					WHERE c.scope_kind=w.scope_kind AND c.scope_id=w.scope_id AND c.run_id=w.run_id
					AND c.source=e.source AND c.event_id=e.event_id))
		)`, string(encoded))
		if err != nil {
			return 0, nil, err
		}
		count, err = result.RowsAffected()
		if err != nil {
			return 0, nil, err
		}
		// SQLite deployments may not enable foreign-key enforcement globally.
		// Remove only notifications for this bounded, scoped candidate batch
		// whose inbox payload was actually deleted in the same transaction.
		if _, err = conn.ExecContext(ctx, `DELETE FROM run_event_notifications WHERE rowid IN (
			SELECT n.rowid FROM json_each(?) candidate JOIN run_event_notifications n
			ON n.scope_kind=json_extract(candidate.value,'$.scopeKind')
			AND n.scope_id=json_extract(candidate.value,'$.scopeId')
			AND n.source=json_extract(candidate.value,'$.source') AND n.event_id=json_extract(candidate.value,'$.eventId')
			WHERE NOT EXISTS (SELECT 1 FROM run_event_inbox e
				WHERE e.scope_kind=n.scope_kind AND e.scope_id=n.scope_id AND e.source=n.source AND e.event_id=n.event_id)
		)`, string(encoded)); err != nil {
			return 0, nil, err
		}
	}
	var next *runEventRetentionPosition
	if len(candidates) == limit {
		last := candidates[len(candidates)-1]
		next = &last
	}
	return int(count), next, nil
}
