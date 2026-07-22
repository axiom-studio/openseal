package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/source"
)

var _ source.LifecycleStore = (*PostgresStore)(nil)

func (s *PostgresStore) migrateSourcePolicies(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("source_policy_versions")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, policy_id TEXT NOT NULL, version TEXT NOT NULL,
			registered_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY(scope_kind, scope_id, policy_id, version)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("source_policy_lifecycles")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, policy_id TEXT NOT NULL,
			revision BIGINT NOT NULL CHECK(revision > 0), state TEXT NOT NULL, updated_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY(scope_kind, scope_id, policy_id)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("source_policy_lifecycle_events")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, policy_id TEXT NOT NULL, id TEXT NOT NULL,
			policy_revision BIGINT NOT NULL CHECK(policy_revision > 0), created_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY(scope_kind, scope_id, policy_id, id),
			UNIQUE(scope_kind, scope_id, policy_id, policy_revision)
		);
		CREATE INDEX IF NOT EXISTS source_policy_events_order_idx
			ON `+s.table("source_policy_lifecycle_events")+`(scope_kind, scope_id, policy_id, policy_revision)
	`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES(22,'governed source policy lifecycle') ON CONFLICT(version) DO NOTHING`)
	return err
}

func (s *PostgresStore) RegisterVersion(ctx context.Context, value *source.PolicyVersion) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO `+s.table("source_policy_versions")+`(scope_kind,scope_id,policy_id,version,registered_at,payload) VALUES($1,$2,$3,$4,$5,$6::jsonb) ON CONFLICT DO NOTHING`, value.Scope.Kind, value.Scope.ID, value.Policy.ID, value.Policy.Version, value.RegisteredAt, string(payload))
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

func (s *PostgresStore) GetVersion(ctx context.Context, scope capability.ScopeReference, id, version string) (*source.PolicyVersion, error) {
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("source_policy_versions")+` WHERE scope_kind=$1 AND scope_id=$2 AND policy_id=$3 AND version=$4`, scope.Kind, scope.ID, id, version).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, source.ErrPolicyVersionNotFound
	}
	if err != nil {
		return nil, err
	}
	var value source.PolicyVersion
	if err := json.Unmarshal(payload, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *PostgresStore) ListVersions(ctx context.Context, scope capability.ScopeReference, id string) ([]*source.PolicyVersion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("source_policy_versions")+` WHERE scope_kind=$1 AND scope_id=$2 AND policy_id=$3 ORDER BY version`, scope.Kind, scope.ID, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]*source.PolicyVersion, 0)
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var value source.PolicyVersion
		if err := json.Unmarshal(payload, &value); err != nil {
			return nil, err
		}
		items = append(items, &value)
	}
	return items, rows.Err()
}

func (s *PostgresStore) GetLifecycle(ctx context.Context, scope capability.ScopeReference, id string) (*source.Lifecycle, error) {
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("source_policy_lifecycles")+` WHERE scope_kind=$1 AND scope_id=$2 AND policy_id=$3`, scope.Kind, scope.ID, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, source.ErrPolicyNotFound
	}
	if err != nil {
		return nil, err
	}
	var value source.Lifecycle
	if err := json.Unmarshal(payload, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *PostgresStore) ListLifecycles(ctx context.Context, scope capability.ScopeReference) ([]*source.Lifecycle, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("source_policy_lifecycles")+` WHERE scope_kind=$1 AND scope_id=$2 ORDER BY policy_id`, scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]*source.Lifecycle, 0)
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var value source.Lifecycle
		if err := json.Unmarshal(payload, &value); err != nil {
			return nil, err
		}
		items = append(items, &value)
	}
	return items, rows.Err()
}

func (s *PostgresStore) ApplyLifecycle(ctx context.Context, next *source.Lifecycle, expectedRevision int64, event source.LifecycleEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	lockKey := "openseal:source-policy:" + next.Scope.Kind + ":" + next.Scope.ID + ":" + next.PolicyID
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lockKey); err != nil {
		return err
	}
	var current int64
	err = tx.QueryRowContext(ctx, `SELECT revision FROM `+s.table("source_policy_lifecycles")+` WHERE scope_kind=$1 AND scope_id=$2 AND policy_id=$3 FOR UPDATE`, next.Scope.Kind, next.Scope.ID, next.PolicyID).Scan(&current)
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
		_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("source_policy_lifecycles")+`(scope_kind,scope_id,policy_id,revision,state,updated_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb)`, next.Scope.Kind, next.Scope.ID, next.PolicyID, next.Revision, next.State, next.UpdatedAt, string(lifecyclePayload))
	} else {
		var result sql.Result
		result, err = tx.ExecContext(ctx, `UPDATE `+s.table("source_policy_lifecycles")+` SET revision=$1,state=$2,updated_at=$3,payload=$4::jsonb WHERE scope_kind=$5 AND scope_id=$6 AND policy_id=$7 AND revision=$8`, next.Revision, next.State, next.UpdatedAt, string(lifecyclePayload), next.Scope.Kind, next.Scope.ID, next.PolicyID, expectedRevision)
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
	if _, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("source_policy_lifecycle_events")+`(scope_kind,scope_id,policy_id,id,policy_revision,created_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb)`, event.Scope.Kind, event.Scope.ID, event.PolicyID, event.ID, event.PolicyRevision, event.CreatedAt, string(eventPayload)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) ListLifecycleEvents(ctx context.Context, scope capability.ScopeReference, id string) ([]*source.LifecycleEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("source_policy_lifecycle_events")+` WHERE scope_kind=$1 AND scope_id=$2 AND policy_id=$3 ORDER BY policy_revision`, scope.Kind, scope.ID, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]*source.LifecycleEvent, 0)
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var value source.LifecycleEvent
		if err := json.Unmarshal(payload, &value); err != nil {
			return nil, err
		}
		items = append(items, &value)
	}
	return items, rows.Err()
}
