package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

var _ RunEventWaitStore = (*PostgresStore)(nil)

func (s *PostgresStore) migrateRunEventWaits(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("run_event_inbox")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, source TEXT NOT NULL, event_id TEXT NOT NULL,
			event_type TEXT NOT NULL, subject TEXT NOT NULL, occurred_at BIGINT NOT NULL,
			received_at BIGINT NOT NULL, digest TEXT NOT NULL, attributes JSONB NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, source, event_id)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("run_event_waits")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, run_id TEXT NOT NULL, wait_key TEXT NOT NULL,
			status TEXT NOT NULL, source TEXT NOT NULL, event_type TEXT NOT NULL, subject TEXT NOT NULL,
			after_at BIGINT NOT NULL, deadline BIGINT NOT NULL, attributes JSONB NOT NULL,
			available_at TIMESTAMPTZ, lease_owner TEXT NOT NULL DEFAULT '', lease_expires_at TIMESTAMPTZ,
			payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, run_id, wait_key),
			FOREIGN KEY (scope_kind, scope_id, run_id) REFERENCES `+s.table("agent_runs")+` (scope_kind, scope_id, id) ON DELETE CASCADE
		);
		CREATE TABLE IF NOT EXISTS `+s.table("run_event_consumptions")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, run_id TEXT NOT NULL, source TEXT NOT NULL,
			event_id TEXT NOT NULL, wait_key TEXT NOT NULL, consumed_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, run_id, source, event_id),
			FOREIGN KEY (scope_kind, scope_id, run_id) REFERENCES `+s.table("agent_runs")+` (scope_kind, scope_id, id) ON DELETE CASCADE
		);
		CREATE TABLE IF NOT EXISTS `+s.table("run_event_wait_scopes")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, available_at TIMESTAMPTZ,
			PRIMARY KEY (scope_kind, scope_id)
		);
		CREATE INDEX IF NOT EXISTS run_event_wait_scopes_work_idx ON `+s.table("run_event_wait_scopes")+`
			(available_at, scope_kind, scope_id) WHERE available_at IS NOT NULL;
		CREATE INDEX IF NOT EXISTS run_event_inbox_match_idx ON `+s.table("run_event_inbox")+`
			(scope_kind, scope_id, source, event_type, subject, occurred_at, received_at, event_id);
		CREATE INDEX IF NOT EXISTS run_event_inbox_retention_idx ON `+s.table("run_event_inbox")+`
			(scope_kind, scope_id, received_at, source, event_id);
		CREATE INDEX IF NOT EXISTS run_event_inbox_attributes_idx ON `+s.table("run_event_inbox")+` USING GIN (attributes jsonb_path_ops);
		CREATE INDEX IF NOT EXISTS run_event_waits_claim_idx ON `+s.table("run_event_waits")+`
			(scope_kind, scope_id, available_at, run_id, wait_key) WHERE status = 'pending';
		CREATE INDEX IF NOT EXISTS run_event_waits_ready_idx ON `+s.table("run_event_waits")+`
			(scope_kind, scope_id, available_at, run_id, wait_key) WHERE status = 'pending' AND lease_expires_at IS NULL;
		CREATE INDEX IF NOT EXISTS run_event_waits_lease_idx ON `+s.table("run_event_waits")+`
			(scope_kind, scope_id, lease_expires_at, run_id, wait_key) WHERE status = 'pending' AND lease_expires_at IS NOT NULL;
		CREATE INDEX IF NOT EXISTS run_event_waits_match_idx ON `+s.table("run_event_waits")+`
			(scope_kind, scope_id, source, event_type, subject, after_at, deadline) WHERE status IN ('pending', 'paused');
		CREATE INDEX IF NOT EXISTS run_event_waits_notification_idx ON `+s.table("run_event_waits")+`
			(scope_kind, scope_id, source, event_type, subject, run_id, wait_key) WHERE status = 'pending';
		CREATE UNIQUE INDEX IF NOT EXISTS run_event_waits_active_run_idx ON `+s.table("run_event_waits")+`
			(scope_kind, scope_id, run_id) WHERE status IN ('pending', 'paused');
		INSERT INTO `+s.table("schema_migrations")+` (version, name) VALUES (47, 'durable correlated run event waits') ON CONFLICT (version) DO NOTHING
	`); err != nil {
		return err
	}
	return nil
}

// syncPostgresRunEventWaitTx runs under the authoritative Run row lock. Every
// Run transition and its wait projection commit together; a paused execution
// retains its immutable expectation and replays buffered observations on resume.
func (s *PostgresStore) syncPostgresRunEventWaitTx(ctx context.Context, tx *sql.Tx, run *AgentRun) error {
	wait, err := runEventWaitForRun(run)
	if err != nil {
		return err
	}
	key := ""
	if wait != nil {
		key = wait.Spec.Key
	}
	canceled, err := tx.ExecContext(ctx, `UPDATE `+s.table("run_event_waits")+`
		SET status='canceled', available_at=NULL, lease_owner='', lease_expires_at=NULL,
			payload=payload || '{"status":"canceled"}'::jsonb
		WHERE scope_kind=$1 AND scope_id=$2 AND run_id=$3 AND wait_key<>$4 AND status IN ('pending','paused')`,
		run.Scope.Kind, run.Scope.ID, run.ID, key)
	if err != nil {
		return err
	}
	if wait == nil {
		count, err := canceled.RowsAffected()
		if err != nil || count == 0 {
			return err
		}
		return s.recomputePostgresRunEventWaitScopeTx(ctx, tx, run.Scope, run.UpdatedAt)
	}
	existing, err := s.getPostgresRunEventWait(ctx, tx, run.Scope, run.ID, key, true)
	if err != nil && !errors.Is(err, ErrRunEventWaitNotFound) {
		return err
	}
	if existing != nil {
		if !sameRunEventWaitSpec(&existing.Spec, &wait.Spec) || existing.AssignedAgentID != wait.AssignedAgentID || (existing.Status != RunEventWaitPending && existing.Status != RunEventWaitPaused) {
			return ErrRunEventConflict
		}
		// An unrelated revision while already pending must not discard a lease
		// or a publication notification. Only pause/resume changes readiness.
		if existing.Status == wait.Status {
			return nil
		}
		if err := s.savePostgresRunEventWaitTx(ctx, tx, wait); err != nil {
			return err
		}
		if wait.Status == RunEventWaitPending {
			return s.enqueuePostgresRunEventWaitScopeTx(ctx, tx, run.Scope, *wait.AvailableAt)
		}
		return s.recomputePostgresRunEventWaitScopeTx(ctx, tx, run.Scope, run.UpdatedAt)
	}
	attributes, err := json.Marshal(nonNilRunEventAttributes(wait.Spec.Attributes))
	if err != nil {
		return err
	}
	payload, err := json.Marshal(wait)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("run_event_waits")+`
		(scope_kind,scope_id,run_id,wait_key,status,source,event_type,subject,after_at,deadline,attributes,available_at,payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12,$13::jsonb)`,
		wait.Scope.Kind, wait.Scope.ID, wait.RunID, wait.Spec.Key, wait.Status, wait.Spec.Source, wait.Spec.Type,
		wait.Spec.Subject, wait.Spec.After.UnixNano(), wait.Spec.Deadline.UnixNano(), string(attributes), wait.AvailableAt, string(payload))
	if err != nil {
		return err
	}
	if wait.Status == RunEventWaitPending {
		return s.enqueuePostgresRunEventWaitScopeTx(ctx, tx, run.Scope, *wait.AvailableAt)
	}
	return s.recomputePostgresRunEventWaitScopeTx(ctx, tx, run.Scope, run.UpdatedAt)
}

func nonNilRunEventAttributes(attributes map[string]interface{}) map[string]interface{} {
	if attributes == nil {
		return map[string]interface{}{}
	}
	return attributes
}

func (s *PostgresStore) PublishRunEvent(ctx context.Context, receipt *RunEventReceipt) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	created, err := s.publishPostgresRunEventTx(ctx, tx, receipt)
	if err != nil {
		return false, err
	}
	return created, tx.Commit()
}

func (s *PostgresStore) publishPostgresRunEventTx(ctx context.Context, tx *sql.Tx, receipt *RunEventReceipt) (bool, error) {
	if err := validateRunEventReceipt(receipt); err != nil {
		return false, err
	}
	payload, err := json.Marshal(receipt)
	if err != nil {
		return false, err
	}
	attributes, err := json.Marshal(nonNilRunEventAttributes(receipt.Event.Attributes))
	if err != nil {
		return false, err
	}
	event := receipt.Event
	result, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("run_event_inbox")+`
		(scope_kind,scope_id,source,event_id,event_type,subject,occurred_at,received_at,digest,attributes,payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11::jsonb)
		ON CONFLICT (scope_kind,scope_id,source,event_id) DO NOTHING`, event.Scope.Kind, event.Scope.ID,
		event.Source, event.ID, event.Type, event.Subject, event.OccurredAt.UnixNano(), receipt.ReceivedAt.UnixNano(),
		receipt.Digest, string(attributes), string(payload))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if count == 0 {
		var digest string
		if err := tx.QueryRowContext(ctx, `SELECT digest FROM `+s.table("run_event_inbox")+`
			WHERE scope_kind=$1 AND scope_id=$2 AND source=$3 AND event_id=$4`,
			event.Scope.Kind, event.Scope.ID, event.Source, event.ID).Scan(&digest); err != nil {
			return false, err
		}
		if digest != receipt.Digest {
			return false, ErrRunEventConflict
		}
		return false, nil
	}
	// Acquisition performs a constant number of writes. Fan-out is a durable,
	// bounded worker task instead of one transaction locking every recipient.
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("run_event_notifications")+`
		(scope_kind,scope_id,source,event_id,available_at) VALUES ($1,$2,$3,$4,$5)`,
		event.Scope.Kind, event.Scope.ID, event.Source, event.ID, receipt.ReceivedAt); err != nil {
		return false, err
	}
	if err := s.enqueuePostgresRunEventWaitScopeTx(ctx, tx, event.Scope, receipt.ReceivedAt); err != nil {
		return false, err
	}
	return true, nil
}

func (s *PostgresStore) ClaimRunEventWaits(ctx context.Context, request ClaimRunEventWaitsRequest) ([]*RunEventWait, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := s.processPostgresRunEventNotificationTx(ctx, tx, request.Scope, request.Now); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `WITH ready AS MATERIALIZED (
			SELECT scope_kind,scope_id,run_id,wait_key,available_at FROM `+s.table("run_event_waits")+`
			 WHERE scope_kind=$1 AND scope_id=$2 AND status='pending' AND lease_expires_at IS NULL AND available_at <= $3
			 ORDER BY available_at,run_id,wait_key LIMIT $4 FOR UPDATE SKIP LOCKED
	), expired AS MATERIALIZED (
			SELECT scope_kind,scope_id,run_id,wait_key,available_at FROM `+s.table("run_event_waits")+`
			 WHERE scope_kind=$1 AND scope_id=$2 AND status='pending' AND lease_expires_at <= $3
			 ORDER BY lease_expires_at,run_id,wait_key LIMIT $4 FOR UPDATE SKIP LOCKED
	), candidates AS (
		SELECT scope_kind,scope_id,run_id,wait_key FROM (SELECT * FROM ready UNION ALL SELECT * FROM expired) AS eligible
		ORDER BY available_at,run_id,wait_key LIMIT $4
	) UPDATE `+s.table("run_event_waits")+` AS waits SET lease_owner=$5,lease_expires_at=$6,available_at=$6
	FROM candidates WHERE waits.scope_kind=candidates.scope_kind AND waits.scope_id=candidates.scope_id
	AND waits.run_id=candidates.run_id AND waits.wait_key=candidates.wait_key
	RETURNING waits.payload,waits.status,waits.available_at,waits.lease_owner,waits.lease_expires_at`,
		request.Scope.Kind, request.Scope.ID, request.Now, request.Limit, request.WorkerID, request.Now.Add(request.LeaseDuration))
	if err != nil {
		return nil, err
	}
	claimed := make([]*RunEventWait, 0, request.Limit)
	for rows.Next() {
		wait, err := scanPostgresRunEventWait(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		claimed = append(claimed, wait)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.recomputePostgresRunEventWaitScopeTx(ctx, tx, request.Scope, request.Now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claimed, nil
}

func (s *PostgresStore) ProcessRunEventWait(ctx context.Context, request ProcessRunEventWaitRequest) (*RunEventWaitResult, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// All transitions use Run -> wait lock order, so cancellation and event
	// resolution cannot race into contradictory authoritative Run states.
	run, err := s.getPostgresAgentRunTx(ctx, tx, request.Scope, request.RunID, true)
	if err != nil {
		return nil, err
	}
	wait, err := s.getPostgresRunEventWait(ctx, tx, request.Scope, request.RunID, request.Key, true)
	if err != nil {
		return nil, err
	}
	if wait.Status != RunEventWaitPending || wait.LeaseOwner != request.WorkerID || wait.LeaseExpiresAt == nil || !wait.LeaseExpiresAt.Equal(request.LeaseExpiresAt) || !wait.LeaseExpiresAt.After(request.Now) {
		return nil, ErrLeaseLost
	}
	var receipt *RunEventReceipt
	if run.Status == AgentRunStatusWaitingForEvent && run.WakeCondition != nil && sameRunEventWaitSpec(run.WakeCondition.EventWait, &wait.Spec) {
		attributes, err := json.Marshal(nonNilRunEventAttributes(wait.Spec.Attributes))
		if err != nil {
			return nil, err
		}
		var payload string
		err = tx.QueryRowContext(ctx, `SELECT inbox.payload FROM `+s.table("run_event_inbox")+` AS inbox
			WHERE inbox.scope_kind=$1 AND inbox.scope_id=$2 AND inbox.source=$3 AND inbox.event_type=$4 AND inbox.subject=$5
			AND inbox.occurred_at >= $6 AND inbox.received_at <= $7 AND inbox.attributes @> $8::jsonb
			AND NOT EXISTS (SELECT 1 FROM jsonb_each($8::jsonb) AS expected
				WHERE jsonb_typeof(inbox.attributes->expected.key) IS DISTINCT FROM jsonb_typeof(expected.value))
			AND (inbox.source NOT LIKE 'binding:%' OR inbox.attributes->>'deploymentId'=$10)
			AND NOT EXISTS (SELECT 1 FROM `+s.table("run_event_consumptions")+` AS consumed
				WHERE consumed.scope_kind=inbox.scope_kind AND consumed.scope_id=inbox.scope_id AND consumed.run_id=$9
				AND consumed.source=inbox.source AND consumed.event_id=inbox.event_id)
			ORDER BY inbox.occurred_at,inbox.received_at,inbox.event_id LIMIT 1`,
			request.Scope.Kind, request.Scope.ID, wait.Spec.Source, wait.Spec.Type, wait.Spec.Subject,
			wait.Spec.After.UnixNano(), wait.Spec.Deadline.UnixNano(), string(attributes), request.RunID, wait.AssignedAgentID).Scan(&payload)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			receipt = &RunEventReceipt{}
			decoder := json.NewDecoder(bytes.NewBufferString(payload))
			decoder.UseNumber()
			if err := decoder.Decode(receipt); err != nil {
				return nil, err
			}
		}
	}
	updated, resolved, activity, err := prepareRunEventWaitResolution(run, wait, receipt, request.Now)
	if err != nil {
		return nil, err
	}
	if updated != nil {
		if receipt != nil {
			if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("run_event_consumptions")+`
				(scope_kind,scope_id,run_id,source,event_id,wait_key,consumed_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
				request.Scope.Kind, request.Scope.ID, request.RunID, receipt.Event.Source, receipt.Event.ID, request.Key, request.Now); err != nil {
				return nil, err
			}
		}
		if err := s.updatePostgresAgentRunTx(ctx, tx, updated, run.Revision); err != nil {
			return nil, err
		}
		activity, err = s.insertPostgresActivityTx(ctx, tx, activity)
		if err != nil {
			return nil, err
		}
	}
	if err := s.savePostgresRunEventWaitTx(ctx, tx, resolved); err != nil {
		return nil, err
	}
	if err := s.recomputePostgresRunEventWaitScopeTx(ctx, tx, request.Scope, request.Now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	result := &RunEventWaitResult{Wait: resolved, Run: updated, Activity: activity}
	if receipt != nil {
		event := receipt.Event
		result.Event = &event
	}
	return result, nil
}

// ListRunEventWaitWorkScopes discovers tenants with due work without scanning
// the tenant inventory or their idle executions. A short discovery reservation
// gives another tenant a turn when a burst exceeds one worker's bounded batch.
func (s *PostgresStore) ListRunEventWaitWorkScopes(ctx context.Context, now time.Time, limit int) ([]Scope, error) {
	if now.IsZero() || limit < 1 || limit > 100 {
		return nil, ErrInvalidRunEventWait
	}
	rows, err := s.db.QueryContext(ctx, `WITH due AS (
		SELECT scope_kind,scope_id FROM `+s.table("run_event_wait_scopes")+`
		WHERE available_at <= $1 ORDER BY available_at,scope_kind,scope_id LIMIT $2 FOR UPDATE SKIP LOCKED
	) UPDATE `+s.table("run_event_wait_scopes")+` AS scopes SET available_at=$3 FROM due
	WHERE scopes.scope_kind=due.scope_kind AND scopes.scope_id=due.scope_id RETURNING scopes.scope_kind,scopes.scope_id`,
		now, limit, now.Add(time.Second))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	scopes := make([]Scope, 0, limit)
	for rows.Next() {
		var scope Scope
		if err := rows.Scan(&scope.Kind, &scope.ID); err != nil {
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	return scopes, rows.Err()
}

func (s *PostgresStore) recomputePostgresRunEventWaitScopeTx(ctx context.Context, tx *sql.Tx, scope Scope, _ time.Time) error {
	// Scope is always locked last, after Run and wait. Taking this lock before
	// reading the minimum prevents a concurrent publication's queue notification
	// from being overwritten by an older processor snapshot.
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("run_event_wait_scopes")+`
		(scope_kind,scope_id) VALUES ($1,$2) ON CONFLICT (scope_kind,scope_id) DO NOTHING`, scope.Kind, scope.ID); err != nil {
		return err
	}
	var unused string
	if err := tx.QueryRowContext(ctx, `SELECT scope_id FROM `+s.table("run_event_wait_scopes")+`
		WHERE scope_kind=$1 AND scope_id=$2 FOR UPDATE`, scope.Kind, scope.ID).Scan(&unused); err != nil {
		return err
	}
	var next sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT MIN(candidate) FROM (
		(SELECT available_at AS candidate FROM `+s.table("run_event_waits")+`
		 WHERE scope_kind=$1 AND scope_id=$2 AND status='pending' AND available_at IS NOT NULL
		 AND lease_expires_at IS NULL ORDER BY available_at LIMIT 1)
		UNION ALL
		(SELECT lease_expires_at AS candidate FROM `+s.table("run_event_waits")+`
		 WHERE scope_kind=$1 AND scope_id=$2 AND status='pending' AND available_at IS NOT NULL
		 AND lease_expires_at IS NOT NULL ORDER BY lease_expires_at LIMIT 1)
		UNION ALL
		(SELECT available_at AS candidate FROM `+s.table("run_event_notifications")+`
		 WHERE scope_kind=$1 AND scope_id=$2 ORDER BY available_at LIMIT 1)
	) AS candidates`, scope.Kind, scope.ID).Scan(&next); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE `+s.table("run_event_wait_scopes")+` SET available_at=$1
		WHERE scope_kind=$2 AND scope_id=$3`, next, scope.Kind, scope.ID)
	return err
}

func (s *PostgresStore) enqueuePostgresRunEventWaitScopeTx(ctx context.Context, tx *sql.Tx, scope Scope, at time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("run_event_wait_scopes")+` AS scopes
		(scope_kind,scope_id,available_at) VALUES ($1,$2,$3)
		ON CONFLICT (scope_kind,scope_id) DO UPDATE
		SET available_at=LEAST(COALESCE(scopes.available_at,$3),$3)`, scope.Kind, scope.ID, at)
	return err
}

func (s *PostgresStore) GetRunEventWait(ctx context.Context, scope Scope, runID, key string) (*RunEventWait, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.getPostgresRunEventWait(ctx, s.db, scope, runID, key, false)
}

func (s *PostgresStore) getPostgresRunEventWait(ctx context.Context, queryer postgresDependencyQueryer, scope Scope, runID, key string, lock bool) (*RunEventWait, error) {
	query := `SELECT payload,status,available_at,lease_owner,lease_expires_at FROM ` + s.table("run_event_waits") + `
		WHERE scope_kind=$1 AND scope_id=$2 AND run_id=$3 AND wait_key=$4`
	if lock {
		query += ` FOR UPDATE`
	}
	wait, err := scanPostgresRunEventWait(queryer.QueryRowContext(ctx, query, scope.Kind, scope.ID, runID, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRunEventWaitNotFound
	}
	return wait, err
}

func scanPostgresRunEventWait(row interface{ Scan(...interface{}) error }) (*RunEventWait, error) {
	var payload, status, owner string
	var available, expires sql.NullTime
	if err := row.Scan(&payload, &status, &available, &owner, &expires); err != nil {
		return nil, err
	}
	var wait RunEventWait
	if err := json.Unmarshal([]byte(payload), &wait); err != nil {
		return nil, err
	}
	wait.Status = RunEventWaitStatus(status)
	wait.LeaseOwner = owner
	wait.AvailableAt, wait.LeaseExpiresAt = nil, nil
	if available.Valid {
		at := available.Time
		wait.AvailableAt = &at
	}
	if expires.Valid {
		at := expires.Time
		wait.LeaseExpiresAt = &at
	}
	return &wait, nil
}

func (s *PostgresStore) savePostgresRunEventWaitTx(ctx context.Context, tx *sql.Tx, wait *RunEventWait) error {
	payload, err := json.Marshal(wait)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("run_event_waits")+`
		SET status=$1,available_at=$2,lease_owner=$3,lease_expires_at=$4,payload=$5::jsonb
		WHERE scope_kind=$6 AND scope_id=$7 AND run_id=$8 AND wait_key=$9`, wait.Status, wait.AvailableAt,
		wait.LeaseOwner, wait.LeaseExpiresAt, string(payload), wait.Scope.Kind, wait.Scope.ID, wait.RunID, wait.Spec.Key)
	return expectPostgresDependencyRow(result, err)
}

func (s *PostgresStore) PruneRunEvents(ctx context.Context, scope Scope, now time.Time, limit int) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	if now.IsZero() || limit < 1 || limit > 1000 {
		return 0, ErrInvalidRunEventWait
	}
	result, err := s.db.ExecContext(ctx, `WITH candidates AS MATERIALIZED (
		SELECT inbox.scope_kind,inbox.scope_id,inbox.source,inbox.event_id FROM `+s.table("run_event_inbox")+` AS inbox
		WHERE inbox.scope_kind=$1 AND inbox.scope_id=$2 AND inbox.received_at < $3
		ORDER BY inbox.received_at,inbox.source,inbox.event_id LIMIT $4
	), expired AS MATERIALIZED (
		SELECT inbox.scope_kind,inbox.scope_id,inbox.source,inbox.event_id FROM `+s.table("run_event_inbox")+` AS inbox
		JOIN candidates ON candidates.scope_kind=inbox.scope_kind AND candidates.scope_id=inbox.scope_id
			AND candidates.source=inbox.source AND candidates.event_id=inbox.event_id
		AND NOT EXISTS (SELECT 1 FROM `+s.table("run_event_waits")+` AS waits
			WHERE waits.scope_kind=inbox.scope_kind AND waits.scope_id=inbox.scope_id AND waits.source=inbox.source
			AND waits.event_type=inbox.event_type AND waits.subject=inbox.subject AND waits.status IN ('pending','paused')
			AND waits.after_at <= inbox.occurred_at AND waits.deadline >= inbox.received_at AND inbox.attributes @> waits.attributes
			AND (inbox.source NOT LIKE 'binding:%' OR inbox.attributes->>'deploymentId'=waits.payload->>'assignedAgentId')
			AND NOT EXISTS (SELECT 1 FROM `+s.table("run_event_consumptions")+` AS consumed
				WHERE consumed.scope_kind=inbox.scope_kind AND consumed.scope_id=inbox.scope_id AND consumed.run_id=waits.run_id
				AND consumed.source=inbox.source AND consumed.event_id=inbox.event_id)
			AND NOT EXISTS (SELECT 1 FROM jsonb_each(waits.attributes) AS expected
				WHERE jsonb_typeof(inbox.attributes->expected.key) IS DISTINCT FROM jsonb_typeof(expected.value)))
		ORDER BY inbox.received_at,inbox.source,inbox.event_id FOR UPDATE OF inbox SKIP LOCKED
	) DELETE FROM `+s.table("run_event_inbox")+` AS inbox USING expired
	WHERE inbox.scope_kind=expired.scope_kind AND inbox.scope_id=expired.scope_id AND inbox.source=expired.source AND inbox.event_id=expired.event_id`,
		scope.Kind, scope.ID, now.Add(-RunEventRetention).UnixNano(), limit)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	return int(count), err
}
