package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

// WithRunLoopCutoverLock holds the schema migration advisory lock for the
// whole pass, so concurrently starting replicas serialize (with each other
// and with a migration) and the later one finds nothing to do. It is a
// session-level lock on a dedicated connection that is not in a transaction,
// so idle_in_transaction_session_timeout cannot end it; the cutover's writes
// use other connections. The pass fails when the lock is no longer held at
// its end.
func (s *PostgresStore) WithRunLoopCutoverLock(ctx context.Context, fn func(context.Context) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	key := "openseal:migrate:" + s.schema
	if err := s.acquireRunLoopCutoverLock(ctx, conn, key); err != nil {
		return err
	}
	runErr := fn(ctx)
	// Unlock on a context that outlives a canceled pass so the session does
	// not keep the lock; false means the lock was lost during the pass.
	unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	var released bool
	unlockErr := conn.QueryRowContext(unlockCtx, `SELECT pg_advisory_unlock(hashtext($1))`, key).Scan(&released)
	if unlockErr == nil && !released {
		unlockErr = errors.New("cutover lock was lost during the pass")
	}
	if unlockErr != nil {
		// Drop the connection rather than return it to the pool holding a
		// lock or in an unknown state.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		return errors.Join(runErr, fmt.Errorf("release cutover lock: %w", unlockErr))
	}
	return runErr
}

func (s *PostgresStore) acquireRunLoopCutoverLock(ctx context.Context, conn *sql.Conn, key string) error {
	startedAt := time.Now()
	lockContext, cancel := context.WithTimeout(ctx, s.migrationLockTimeout)
	defer cancel()
	for {
		var acquired bool
		err := conn.QueryRowContext(lockContext, `SELECT pg_try_advisory_lock(hashtext($1))`, key).Scan(&acquired)
		if err == nil && acquired {
			return nil
		}
		if err == nil {
			timer := time.NewTimer(s.migrationLockPollInterval)
			select {
			case <-lockContext.Done():
				timer.Stop()
				err = lockContext.Err()
			case <-timer.C:
				continue
			}
		}
		if errors.Is(lockContext.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("acquire cutover lock: %w after %s", ErrPostgresMigrationLockTimeout, time.Since(startedAt).Round(time.Millisecond))
		}
		return fmt.Errorf("acquire cutover lock: %w", err)
	}
}

func (s *PostgresStore) ListRunLoopCutoverCandidates(ctx context.Context) ([]models.RunLoopCutoverRef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT scope_kind, scope_id, id FROM `+s.table("agent_runs")+`
		WHERE status NOT IN ('completed', 'failed', 'canceled') AND COALESCE(payload->>'engine', '') = ''
		ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	return sqlstore.ScanRunLoopCutoverRefs(rows)
}
