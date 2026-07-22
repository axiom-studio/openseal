package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/source"
)

var _ source.LifecycleStore = (*SQLiteStore)(nil)

func migrateSourcePolicies(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS source_policy_versions (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, policy_id TEXT NOT NULL, version TEXT NOT NULL,
			registered_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY(scope_kind, scope_id, policy_id, version)
		);
		CREATE TABLE IF NOT EXISTS source_policy_lifecycles (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, policy_id TEXT NOT NULL,
			revision INTEGER NOT NULL, state TEXT NOT NULL, updated_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY(scope_kind, scope_id, policy_id)
		);
		CREATE TABLE IF NOT EXISTS source_policy_lifecycle_events (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, policy_id TEXT NOT NULL, id TEXT NOT NULL,
			policy_revision INTEGER NOT NULL, created_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY(scope_kind, scope_id, policy_id, id),
			UNIQUE(scope_kind, scope_id, policy_id, policy_revision)
		);
		CREATE INDEX IF NOT EXISTS source_policy_events_order_idx
			ON source_policy_lifecycle_events(scope_kind, scope_id, policy_id, policy_revision);
	`)
	return err
}

func (s *SQLiteStore) RegisterVersion(ctx context.Context, value *source.PolicyVersion) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO source_policy_versions(scope_kind,scope_id,policy_id,version,registered_at,payload) VALUES(?,?,?,?,?,?)`,
		value.Scope.Kind, value.Scope.ID, value.Policy.ID, value.Policy.Version, value.RegisteredAt, string(payload))
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return source.ErrPolicyVersionExists
	}
	return nil
}

func (s *SQLiteStore) GetVersion(ctx context.Context, scope capability.ScopeReference, id, version string) (*source.PolicyVersion, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM source_policy_versions WHERE scope_kind=? AND scope_id=? AND policy_id=? AND version=?`, scope.Kind, scope.ID, id, version).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, source.ErrPolicyVersionNotFound
	}
	if err != nil {
		return nil, err
	}
	var value source.PolicyVersion
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *SQLiteStore) ListVersions(ctx context.Context, scope capability.ScopeReference, id string) ([]*source.PolicyVersion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM source_policy_versions WHERE scope_kind=? AND scope_id=? AND policy_id=? ORDER BY version`, scope.Kind, scope.ID, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]*source.PolicyVersion, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var value source.PolicyVersion
		if err := json.Unmarshal([]byte(payload), &value); err != nil {
			return nil, err
		}
		items = append(items, &value)
	}
	return items, rows.Err()
}

func (s *SQLiteStore) GetLifecycle(ctx context.Context, scope capability.ScopeReference, id string) (*source.Lifecycle, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM source_policy_lifecycles WHERE scope_kind=? AND scope_id=? AND policy_id=?`, scope.Kind, scope.ID, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, source.ErrPolicyNotFound
	}
	if err != nil {
		return nil, err
	}
	var value source.Lifecycle
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *SQLiteStore) ListLifecycles(ctx context.Context, scope capability.ScopeReference) ([]*source.Lifecycle, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM source_policy_lifecycles WHERE scope_kind=? AND scope_id=? ORDER BY policy_id`, scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]*source.Lifecycle, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var value source.Lifecycle
		if err := json.Unmarshal([]byte(payload), &value); err != nil {
			return nil, err
		}
		items = append(items, &value)
	}
	return items, rows.Err()
}

func (s *SQLiteStore) ApplyLifecycle(ctx context.Context, next *source.Lifecycle, expectedRevision int64, event source.LifecycleEvent) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var current int64
	err = conn.QueryRowContext(ctx, `SELECT revision FROM source_policy_lifecycles WHERE scope_kind=? AND scope_id=? AND policy_id=?`, next.Scope.Kind, next.Scope.ID, next.PolicyID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		current, err = 0, nil
	}
	if err != nil {
		return err
	}
	if current != expectedRevision {
		return source.ErrPolicyRevision
	}
	lifecyclePayload, err := json.Marshal(next)
	if err != nil {
		return err
	}
	eventPayload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if expectedRevision == 0 {
		_, err = conn.ExecContext(ctx, `INSERT INTO source_policy_lifecycles(scope_kind,scope_id,policy_id,revision,state,updated_at,payload) VALUES(?,?,?,?,?,?,?)`, next.Scope.Kind, next.Scope.ID, next.PolicyID, next.Revision, next.State, next.UpdatedAt, string(lifecyclePayload))
	} else {
		var result sql.Result
		result, err = conn.ExecContext(ctx, `UPDATE source_policy_lifecycles SET revision=?,state=?,updated_at=?,payload=? WHERE scope_kind=? AND scope_id=? AND policy_id=? AND revision=?`, next.Revision, next.State, next.UpdatedAt, string(lifecyclePayload), next.Scope.Kind, next.Scope.ID, next.PolicyID, expectedRevision)
		if err == nil {
			var affected int64
			affected, err = result.RowsAffected()
			if err == nil && affected != 1 {
				err = source.ErrPolicyRevision
			}
		}
	}
	if err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO source_policy_lifecycle_events(scope_kind,scope_id,policy_id,id,policy_revision,created_at,payload) VALUES(?,?,?,?,?,?,?)`, event.Scope.Kind, event.Scope.ID, event.PolicyID, event.ID, event.PolicyRevision, event.CreatedAt, string(eventPayload)); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func (s *SQLiteStore) ListLifecycleEvents(ctx context.Context, scope capability.ScopeReference, id string) ([]*source.LifecycleEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM source_policy_lifecycle_events WHERE scope_kind=? AND scope_id=? AND policy_id=? ORDER BY policy_revision`, scope.Kind, scope.ID, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]*source.LifecycleEvent, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var value source.LifecycleEvent
		if err := json.Unmarshal([]byte(payload), &value); err != nil {
			return nil, err
		}
		items = append(items, &value)
	}
	return items, rows.Err()
}
